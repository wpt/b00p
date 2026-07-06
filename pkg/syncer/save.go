package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/fileutil"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// commentsPageLimit is the per-page limit for the comments listing endpoint.
// Boosty's offset query param is effectively ignored on the comments endpoint
// (offset>0 returns data=[] with isLast=true, so paginated fetching never
// advances past the first page), but the server honors limit values up to
// ~200 in a single call.
//
// 101 = 100 expected + 1 probe slot: a post with EXACTLY 100 top-level
// threads returns 100 here (uncapped), while a post with >100 top-level
// threads returns 101 (capped). With a flat limit of 100 the two cases are
// indistinguishable and posts that happen to sit at the boundary would be
// permanently flagged CommentsCapped, suppressing all future refetch even
// though the comments fit. The 101st entry is kept in the saved file.
const commentsPageLimit = 101

// commentsCapThreshold is the count above which we consider the fetch
// structurally capped. Threads at-or-below this fit in a single page.
const commentsCapThreshold = 100

// SavePost downloads a post's full content into the engine's output directory.
// Returns the directory name actually used (which may include a collision
// suffix) and whether the comments fetch hit Boosty's structural cap (for
// state-side bookkeeping) so the caller can record both in state.
//
// A non-nil error means at least one required artifact (post.json, media,
// post.md when WithMD, comments.json when WithComments) could not be written
// or downloaded. The caller MUST NOT record the post as downloaded in state
// on error — that is what makes the next sync re-attempt the failed pieces
// instead of silently leaving stale/missing files behind.
//
// External video failures are NOT fatal: DownloadExternal is opt-in and
// depends on third-party sites that fail in routine ways (geo-blocks, age
// gates, dead links). They are logged and ignored for the state contract.
//
// SavePost does not refresh signed video URLs on its own — callers that
// receive posts from the list endpoint (DownloadAll worker, applyNew) must
// hoist the refresh via MaybeRefreshSignedURLs before calling SavePost, so
// the same fresh *Post is used for post.json, the download, and the state
// entry. Callers that already fetched the per-post endpoint (cmd/download
// --url) pass that fresh post through directly.
//
// Contract: when err is nil, dirName is "" iff the post was inaccessible —
// every accessible post returns a non-empty name on success. On error,
// dirName may be empty (failures before the directory was created: MkdirAll,
// post.json write) or non-empty (artefact failures after); callers must
// check err before relying on dirName.
func (e *Engine) SavePost(post *boosty.Post) (dirName string, capped bool, err error) {
	if !post.HasAccess {
		e.c.Log.Printf("  skipping (no access): %s", post.Title)
		return "", false, nil
	}

	blogDir := filepath.Join(e.cfg.OutputDir, e.cfg.Blog)
	dirName = parser.FormatDirName(e.cfg.DirFormat, post.Title, post.PublishTime, post.ID)
	dirName = e.res.reserve(blogDir, post.ID, dirName)
	dir := filepath.Join(blogDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", false, err
	}

	if err := writeJSON(filepath.Join(dir, "post.json"), post); err != nil {
		return "", false, err
	}
	e.c.Log.Printf("  saved post.json: %s", post.Title)

	parsed := e.parsePostContent(post)

	// Download media. Errors are joined and returned so the caller can refuse
	// to mark the post as downloaded in state.
	var errs []error
	if err := downloader.DownloadMedia(e.c, parsed.Media, dir); err != nil {
		errs = append(errs, fmt.Errorf("media: %w", err))
	}

	// External videos: opt-in and best-effort, only logged.
	if e.cfg.DownloadExternal {
		if err := downloader.DownloadExternal(e.c.Log, parsed.Media, dir); err != nil {
			e.c.Log.Printf("  warning: external download error: %v", err)
		}
	}

	if e.cfg.WithMD {
		if err := writePostMarkdown(post, parsed, dir); err != nil {
			errs = append(errs, fmt.Errorf("post.md: %w", err))
		} else {
			e.c.Log.Printf("  saved post.md")
		}
	}

	if e.cfg.WithComments {
		cappedFetch, err := e.downloadComments(post.ID, dir, post.Count.Comments)
		if err != nil {
			errs = append(errs, fmt.Errorf("comments: %w", err))
		} else {
			capped = cappedFetch
		}
	}

	if len(errs) > 0 {
		return dirName, capped, errors.Join(errs...)
	}
	return dirName, capped, nil
}

// parsePostContent parses a post's content blocks, attaches the post-level
// signedQuery to attachment (audio/file) URLs — the API serves those unsigned,
// unlike image/video URLs — and logs the content warnings that must never be
// silent (videos with no MP4 variant, unknown block types). Shared by
// SavePost and runApplyActions so the signed-query step and the warnings
// cannot drift between the fresh-download and the update paths.
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
//   - per-post endpoint returns a stub (HasAccess=false or empty Data) —
//     subscription lapsed between list-call and per-post call. Keeping the
//     input avoids writing a degraded post.json + zero-length post.md that
//     a follow-up sync would re-classify as JustLocked.
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
	if !fresh.HasAccess || len(fresh.Data) == 0 {
		e.c.Log.Printf("  warning: per-post fetch for %s returned no-access/empty stub; using list-endpoint copy", post.ID)
		return post
	}
	return &fresh
}

// downloadComments fetches and saves comments.json. Returns capped=true when
// the fetch hit Boosty's structural per-post limit (top-level count >=
// commentsPageLimit, or any single thread inlined replyCount > defaultReplyLimit
// replies) so the caller can mark state to stop re-triggering NewComments
// against an API count the server will never let us reach. expectedCount is
// post.Count.Comments at fetch time — used only for the warning, not for
// disk accounting.
func (e *Engine) downloadComments(postID, dir string, expectedCount int) (capped bool, err error) {
	var allComments []boosty.Comment
	for comment, err := range e.c.FetchComments(e.cfg.Blog, postID, commentsPageLimit) {
		if err != nil {
			return false, err
		}
		allComments = append(allComments, comment)
	}

	if err := writeJSON(filepath.Join(dir, "comments.json"), allComments); err != nil {
		return false, err
	}

	// Disk count includes inlined replies — matches the value classifyPost
	// computes via diskCommentCount. Cap detection: either we hit the top-
	// level page limit (>100 top-level threads exist), or any single thread
	// reported more replies than we could inline.
	diskCount := len(allComments)
	for _, c := range allComments {
		if c.Replies != nil {
			diskCount += len(c.Replies.Data)
		}
	}
	// Two cap signals: top-level (>100 threads in a single page = server
	// truncated the list) or per-thread (a thread reports more replies than
	// the server actually inlined, i.e. defaultReplyLimit covered fewer than
	// existed).
	if len(allComments) > commentsCapThreshold {
		capped = true
	}
	for _, c := range allComments {
		if c.Replies != nil && len(c.Replies.Data) < c.ReplyCount {
			capped = true
			break
		}
	}

	if capped && expectedCount > diskCount {
		e.c.Log.Printf("  warning: comments capped for %s: %d of %d (API limit; remaining unreachable via current endpoint)",
			postID, diskCount, expectedCount)
	}
	e.c.Log.Printf("  saved comments.json (%d comments)", len(allComments))
	return capped, nil
}

// postStateEntry builds a state.PostEntry from a post for the initial
// NEW save path. HasComments / HasMd reflect the engine's current Config
// flags. JustUnlocked and the Edited / VideoMismatch / Missing apply path
// do NOT use this helper — they patch an existing entry in place via
// buildSyncEntry so that failed writes do not advance UpdatedAt /
// CommentsCount past disk reality, and HasMd / HasComments survive a
// sync without the corresponding flag.
func (e *Engine) postStateEntry(post *boosty.Post, dirName string) state.PostEntry {
	tier := ""
	if post.SubscriptionLevel != nil {
		tier = post.SubscriptionLevel.Name
	}
	return state.PostEntry{
		Title:         post.Title,
		DirName:       dirName,
		UpdatedAt:     post.UpdatedAt,
		CommentsCount: post.Count.Comments,
		Price:         post.Price,
		Tier:          tier,
		HasComments:   e.cfg.WithComments,
		HasMd:         e.cfg.WithMD,
	}
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
