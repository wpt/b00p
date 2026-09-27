package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/fileutil"
	"github.com/wpt/b00p/pkg/parser"
)

// commentsPageLimit is the per-page limit for the comments listing endpoint.
// Boosty's offset query param is ignored on that endpoint (offset>0 returns
// data=[] with isLast=true), so the first page is the whole result; the
// server honors limit values up to ~200 in a single call.
//
// 101 = 100 expected + 1 probe slot: a post with EXACTLY 100 top-level
// threads returns 100 here (uncapped), while a post with >100 top-level
// threads returns 101 (capped). With a flat limit of 100 the two cases are
// indistinguishable and posts that happen to sit at the boundary would be
// permanently flagged CommentsCapped. The 101st entry is kept in the saved file.
const commentsPageLimit = 101

// commentsCapThreshold is the count above which we consider the fetch
// structurally capped. Threads at-or-below this fit in a single page.
const commentsCapThreshold = 100

// SavePost downloads a post's full content into the engine's output directory
// using the engine's Config for --md / --comments. Returns the directory name
// actually used (which may include a collision suffix) and whether the
// comments fetch hit Boosty's structural cap, so a caller can record both in
// state.
//
// A non-nil error means at least one required artefact (post.json, media,
// post.md when WithMD, comments.json when WithComments) could not be written
// or downloaded. The caller MUST NOT record the post as downloaded in state
// on error — that is what makes the next sync re-attempt the failed pieces
// instead of silently leaving stale/missing files behind.
//
// Existing non-empty media files are skipped unless the post.json already in
// the directory carries a different updatedAt — the post was edited since
// that copy was saved, so media at the same slot may have been swapped and
// every item is re-downloaded over its old copy.
//
// SavePost does not refresh signed video URLs on its own — callers that
// receive posts from the list endpoint must hoist the refresh via
// MaybeRefreshSignedURLs before calling SavePost, so the same fresh *Post is
// used for post.json, the download, and the state entry. Callers that
// already fetched the per-post endpoint (cmd/download --url) pass that fresh
// post through directly.
//
// Contract: when err is nil, dirName is "" iff the post was inaccessible —
// every accessible post returns a non-empty name on success. On error,
// dirName may be empty (failures before the directory was created) or
// non-empty (artefact failures after); callers must check err first.
func (e *Engine) SavePost(post *boosty.Post) (dirName string, capped bool, err error) {
	name := parser.FormatDirName(e.cfg.DirFormat, post.Title, post.PublishTime, post.ID)
	dirName, out, err := e.savePost(post, name, e.cfg.WithMD, e.cfg.WithComments, 0)
	return dirName, out.CommentsCapped, err
}

// savePost is SavePost with the directory name, artefact flags and the
// caller's record of the post supplied explicitly: saveNewPost takes them
// from a prior state entry, SavePost from Config (and has no record).
//
// savedUpdatedAt is the updatedAt of the last copy whose media fully landed
// (state.PostEntry.UpdatedAt); 0 means unknown and the on-disk post.json
// stands in. State must win over disk: runApplyActions writes post.json
// before media, so after an edit whose media replacement failed, disk
// already carries the new updatedAt next to the old bytes, and trusting it
// would keep them and then advance state past them on the retry.
func (e *Engine) savePost(post *boosty.Post, dirName string, withMD, withComments bool, savedUpdatedAt int64) (string, applyOutcome, error) {
	if !post.HasAccess {
		e.c.Log.Printf("  skipping (no access): %s", post.Title)
		return "", applyOutcome{}, nil
	}

	blogDir := e.blogDir()
	dirName = e.res.reserve(blogDir, post.ID, dirName)
	dir := filepath.Join(blogDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", applyOutcome{}, err
	}

	// A copy saved against a different updatedAt may hold media that the
	// edit replaced at the same filename, which the skip-existing check
	// would keep.
	prev, known := savedUpdatedAt, savedUpdatedAt != 0
	if !known {
		prev, known = readPostUpdatedAt(dir)
	}
	mode := downloader.KeepExisting
	if known && prev != post.UpdatedAt {
		e.c.Log.Printf("  post edited since last save; re-downloading media: %s", post.Title)
		mode = downloader.ReplaceAll
	}

	out, err := e.runApplyActions(dir, post, mode, applyActions{
		Post:     true,
		Media:    true,
		MD:       withMD,
		Comments: withComments,
	})
	return dirName, out, err
}

// readPostUpdatedAt returns the updatedAt recorded in dir/post.json. ok=false
// when the file is missing, unreadable, or not a post payload.
func readPostUpdatedAt(dir string) (int64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "post.json"))
	if err != nil {
		return 0, false
	}
	var p struct {
		UpdatedAt int64 `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return 0, false
	}
	return p.UpdatedAt, true
}

// parsePostContent parses a post's content blocks, attaches the post-level
// signedQuery to attachment (audio/file) URLs — the API serves those unsigned,
// unlike image/video URLs — and logs the content warnings that must never be
// silent (videos with no MP4 variant, unknown block types).
func (e *Engine) parsePostContent(post *boosty.Post) parser.ParsedContent {
	parsed := parser.ParseBlocks(post.Data)
	parser.ApplySignedQuery(parsed.Media, post.SignedQuery)
	if parsed.SkippedVideos > 0 {
		e.c.Log.Printf("  warning: %d ok_video block(s) in %s had no MP4 URL — only HLS/DASH variants; videos skipped",
			parsed.SkippedVideos, post.ID)
	}
	if len(parsed.UnknownTypes) > 0 {
		e.c.Log.Printf("  warning: post %s contains unhandled block type(s) %v — that content is not saved (b00p does not support it yet)",
			post.ID, parsed.UnknownTypes)
	}
	return parsed
}

// MaybeRefreshSignedURLs returns a freshly-fetched post when the input has
// signed media (native video, or audio/file attachments) — the signed okcdn
// URLs and the post-level signedQuery in the list-endpoint payload may have
// already expired by the time the apply queue reaches this post, so
// downloading against them burns through the retry schedule (or 4xxes fast)
// before failing.
//
// Falls back to the input *Post on any failure:
//   - GET error: log warn and keep the input (the download retry path will
//     surface a real error if URLs are dead);
//   - per-post endpoint returns a stub (Post.IsStub) — subscription lapsed
//     between list-call and per-post call. Keeping the input avoids writing
//     a degraded post.json + zero-length post.md that a follow-up sync would
//     re-classify as JustLocked.
//
// Caller passes the result back into SavePost / state.Add, so post.json,
// the downloaded bytes, and the state entry all reflect the same payload.
func (e *Engine) MaybeRefreshSignedURLs(post *boosty.Post) *boosty.Post {
	if !hasSignedMedia(post.Data) {
		return post
	}
	var fresh boosty.Post
	if err := e.c.GetJSON(boosty.PostURL(e.cfg.Blog, post.ID), &fresh); err != nil {
		e.c.Log.Printf("  warning: refresh signed video URLs failed for %s: %v", post.ID, err)
		return post
	}
	if fresh.IsStub() {
		e.c.Log.Printf("  warning: per-post fetch for %s returned no-access/empty stub; using list-endpoint copy", post.ID)
		return post
	}
	return &fresh
}

// downloadComments fetches and saves comments.json. Returns capped=true when
// the fetch hit Boosty's structural per-post limit (more than
// commentsCapThreshold top-level items in the single page the endpoint
// serves, or a thread that inlined fewer live replies than its ReplyCount)
// so the caller can mark state accordingly. expectedCount is
// post.Count.Comments at fetch time — used only for the warning, not for
// disk accounting.
func (e *Engine) downloadComments(postID, dir string, expectedCount int) (capped bool, err error) {
	allComments, err := e.c.FetchComments(e.cfg.Blog, postID, commentsPageLimit)
	if err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(dir, "comments.json"), allComments); err != nil {
		return false, err
	}

	// Disk count is the same liveCommentCount that classifyPost later reads
	// back via diskCommentCount, so the write-side and read-side counting
	// contracts cannot drift.
	//
	// Two cap signals: top-level (>100 items in a single page = server
	// truncated the list; raw length, since deleted stubs occupy page slots
	// too) or per-thread (replyTruncated: a thread reports more live replies
	// than the server actually inlined — compared live-to-live, so deleted
	// stubs padding Replies.Data cannot mask truncation of live replies).
	diskCount, replyTruncated := liveCommentCount(allComments)
	capped = replyTruncated || len(allComments) > commentsCapThreshold

	if capped && expectedCount > diskCount {
		e.c.Log.Printf("  warning: comments capped for %s: %d of %d (API limit; remaining unreachable via current endpoint)",
			postID, diskCount, expectedCount)
	}
	e.c.Log.Printf("  saved comments.json (%d comments)", diskCount)
	return capped, nil
}

// writeJSON marshals v with indent and writes it to path (0644) atomically.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	return fileutil.WriteFileAtomic(path, data, 0644)
}

// writePostMarkdown generates markdown for a post and writes it to dir/post.md
// atomically. Returns an error so callers can avoid persisting HasMd=true on
// failure.
func writePostMarkdown(post *boosty.Post, parsed parser.ParsedContent, dir string) error {
	md := parser.GenerateMarkdown(post, parsed)
	return fileutil.WriteFileAtomic(filepath.Join(dir, "post.md"), []byte(md), 0644)
}
