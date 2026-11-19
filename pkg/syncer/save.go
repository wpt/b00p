package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/fileutil"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
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
// actually used (which may include a collision suffix) and whether the saved
// comments can never match post.Count.Comments (Boosty's structural cap, or
// a count the API gets wrong), so a caller can record both in state.
//
// A non-nil error means at least one required artefact (post.json, media,
// post.md when WithMD, comments.json when WithComments) could not be written
// or downloaded. The caller MUST NOT record the post as downloaded in state
// on error — that is what makes the next sync re-attempt the failed pieces
// instead of silently leaving stale/missing files behind.
//
// A post the blog's state tracks is saved the way a sync saves it: in its
// folder, with the post.md and comments.json its entry tracks, replacing
// only the media an edit changed (see the entry's Media record), and its
// entry is updated, so the next sync neither repeats the work nor misses
// the edit. An untracked post stays out of state: its existing non-empty
// media files are skipped unless the post.json already in the directory
// carries a different updatedAt, and then every item is re-downloaded.
//
// SavePost does not refresh signed video URLs on its own — callers that
// receive posts from the list endpoint must hoist the refresh via
// MaybeRefreshSignedURLs before calling SavePost, so the same fresh *Post is
// used for post.json, the download, and the state entry. Callers that
// already fetched the per-post endpoint (cmd/download --url) pass that fresh
// post through directly.
//
// SavePost holds the per-blog lock for its duration, like Sync and
// DownloadAll: it writes into the same post directories and media .tmp files
// they do. If another run is using the blog it fails fast.
//
// Contract: when err is nil, dirName is "" iff the post was inaccessible —
// every accessible post returns a non-empty name on success. On error,
// dirName may be empty (failures before the directory was created) or
// non-empty (artefact failures after); callers must check err first.
func (e *Engine) SavePost(post *boosty.Post) (dirName string, capped bool, err error) {
	defer func() { err = e.withTokenSaveErr(err) }()
	// savePost writes nothing for an inaccessible post, not even the blog
	// directory the lock file lives in.
	if !post.HasAccess {
		dirName, out, err := e.savePost(post, "", false, false, state.PostEntry{})
		return dirName, out.CommentsCapped, err
	}
	_, st, unlock, err := e.loadState()
	if err != nil {
		return "", false, err
	}
	defer unlock()
	old, tracked := st.Get(post.ID)
	dirName, out, err := e.savePost(post, e.pickDirName(old, post),
		e.cfg.WithMD || old.HasMd, e.cfg.WithComments || old.HasComments, old)
	if tracked && dirName != "" && !e.commitEntry(st, post.ID, buildSyncEntry(old, post, dirName, true, out)) {
		err = errors.Join(err, errors.New("state not saved; see log above"))
	}
	return dirName, out.CommentsCapped, err
}

// savePost is SavePost with the directory name, artefact flags and the
// caller's record of the post supplied explicitly: saveNewPost takes them
// from a prior state entry, SavePost from Config (and has no record).
//
// old.UpdatedAt is the updatedAt of the last copy whose media fully landed;
// 0 means unknown and the on-disk post.json stands in. State must win over
// disk: runApplyActions writes post.json before media, so after an edit
// whose media replacement failed, disk already carries the new updatedAt
// next to the old bytes, and trusting it would keep them and then advance
// state past them on the retry. An edit replaces only the files whose block
// changed (newMediaPlan), or every file when nothing records what landed.
func (e *Engine) savePost(post *boosty.Post, dirName string, withMD, withComments bool, old state.PostEntry) (string, applyOutcome, error) {
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
	prev, known := old.UpdatedAt, old.UpdatedAt != 0
	if !known {
		prev, known = readPostUpdatedAt(dir)
	}
	edited := known && post.UpdatedAt != 0 && prev != post.UpdatedAt
	if edited {
		e.c.Log.Printf("  post edited since last save; re-downloading changed media: %s", post.Title)
	}

	out, err := e.runApplyActions(dir, post, newMediaPlan(dir, old, post.UpdatedAt, edited, nil), applyActions{
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

// postJSONHasBlocks reports whether dir/post.json holds a post with content.
func postJSONHasBlocks(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "post.json"))
	if err != nil {
		return false
	}
	var p struct {
		Data []json.RawMessage `json:"data"`
	}
	return json.Unmarshal(data, &p) == nil && len(p.Data) > 0
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
		e.c.Log.Printf("  warning: post %s contains unhandled block type(s) %v — kept in post.json, missing from post.md and media (b00p does not support it yet)",
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
	// A listing copy without blocks is refetched too: the listing may have
	// dropped them, and an empty copy would be archived as the post.
	if !hasSignedMedia(post.Data) && len(post.Data) > 0 {
		return post
	}
	var fresh boosty.Post
	if err := e.c.GetJSON(boosty.PostURL(e.cfg.Blog, post.ID), &fresh); err != nil {
		e.c.Log.Printf("  warning: refresh signed video URLs failed for %s: %v", post.ID, err)
		return post
	}
	if fresh.DegradedFrom(post) {
		e.c.Log.Printf("  warning: per-post fetch for %s returned no-access/empty stub; using list-endpoint copy", post.ID)
		return post
	}
	return &fresh
}

// downloadComments fetches and saves comments.json. Returns capped=true when
// the saved live count can never match expectedCount (post.Count.Comments
// at fetch time), so the caller can mark state accordingly: the fetch hit
// Boosty's structural per-post limit (more than commentsCapThreshold
// top-level items in the single page the endpoint serves, or a thread that
// inlined fewer live replies than its ReplyCount), or the full fetch simply
// disagrees with the API count — Boosty's count.comments is sometimes wrong
// (seen: 1 for 3 live comments, and negative counts).
func (e *Engine) downloadComments(postID, dir string, expectedCount int) (capped bool, err error) {
	allComments, err := e.c.FetchComments(e.cfg.Blog, postID, commentsPageLimit)
	if err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(dir, "comments.json"), rawComments(allComments), allComments); err != nil {
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
	truncated := replyTruncated || len(allComments) > commentsCapThreshold

	if truncated && expectedCount > diskCount {
		e.c.Log.Printf("  warning: comments capped for %s: %d of %d (API limit; remaining unreachable via current endpoint)",
			postID, diskCount, expectedCount)
	}
	e.c.Log.Printf("  saved comments.json (%d comments)", diskCount)
	return truncated || diskCount != expectedCount, nil
}

// writeJSON writes raw, the API bytes v was decoded from, indented to path
// (0644) atomically, or v marshalled when raw is nil (built in code). The
// archive must hold raw: the boosty structs model only the fields b00p
// reads, so marshalling them drops the rest for good once the post is gone.
func writeJSON(path string, raw json.RawMessage, v any) error {
	var data []byte
	if raw != nil {
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			return fmt.Errorf("indent %s: %w", filepath.Base(path), err)
		}
		data = buf.Bytes()
	} else {
		var err error
		if data, err = json.MarshalIndent(v, "", "  "); err != nil {
			return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
		}
	}
	return fileutil.WriteFileAtomic(path, data, 0644)
}

// rawComments joins the API bytes of each comment into a JSON array, or
// returns nil when any comment lacks them, so writeJSON falls back to
// marshalling the slice.
func rawComments(comments []boosty.Comment) json.RawMessage {
	raw := []byte{'['}
	for i, c := range comments {
		if c.Raw == nil {
			return nil
		}
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, c.Raw...)
	}
	return append(raw, ']')
}

// writePostMarkdown generates markdown for a post and writes it to dir/post.md
// atomically. Returns an error so callers can avoid persisting HasMd=true on
// failure.
func writePostMarkdown(post *boosty.Post, parsed parser.ParsedContent, dir string) error {
	md := parser.GenerateMarkdown(post, parsed)
	return fileutil.WriteFileAtomic(filepath.Join(dir, "post.md"), []byte(md), 0644)
}
