package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// applyActions describes which artefacts the apply phase needs to (re)produce
// for a single syncItem. Computed by decideApplyActions; consumed by
// runApplyActions. Decoupling the decision from execution makes the rule set
// itself table-testable without faking any client or filesystem.
type applyActions struct {
	Post     bool // re-write post.json from a freshly fetched payload
	Media    bool // re-download media (mode decides which files are replaced)
	MD       bool // regenerate post.md from current data
	Comments bool // refetch comments.json
}

// NeedFetch reports whether any action requires a fresh GetJSON for the post.
// Comments fetching goes through its own endpoint and does not require this.
func (a applyActions) NeedFetch() bool {
	return a.Post || a.Media || a.MD
}

// applyOutcome captures per-artefact success after runApplyActions ran. It
// is the input to buildSyncEntry's "do not advance retry-controlling fields
// past disk reality" contract: each *OK gates the corresponding state field,
// and the *Written flags promote HasComments/HasMd from false to true only
// when this run actually produced the artefact.
//
// "Not needed" counts as OK — only channels this run was responsible for
// fail-close their fields. The Written flags differ from the OK flags only
// when an artefact was not requested (OK=true, Written=false), in which case
// the prior value is preserved.
type applyOutcome struct {
	PostJSONOK bool
	MediaOK    bool
	MDOK       bool
	CommentsOK bool

	CommentsWritten bool
	MDWritten       bool
	// CommentsCapped reports that the comments fetch (when it happened)
	// hit Boosty's structural ceiling. Only meaningful when CommentsWritten.
	CommentsCapped bool
}

// allOK reports whether every requested channel landed.
func (o applyOutcome) allOK() bool {
	return o.PostJSONOK && o.MediaOK && o.MDOK && o.CommentsOK
}

// decideApplyActions converts a classified syncItem (plus the engine's
// current Config) into the set of artefacts to (re)produce. Pure function:
// no I/O, no global state, suitable for table-driven tests of the trigger
// matrix without faking any HTTP client.
//
// Triggers, by artefact:
//   - Post: post was Edited, or post.json went missing on disk.
//   - Media: post was Edited (block list may have changed), or check-media
//     reported a size mismatch needing fresh signed URLs.
//   - MD: this run carries WithMD (or the prior entry recorded HasMd) AND
//     either Edited or post.md is missing from disk.
//   - Comments: count changed (NewComments — only ever set for posts whose
//     comments are tracked), or comments.json went missing, or the post was
//     Edited and either the prior entry tracked comments or the current run
//     carries WithComments.
func decideApplyActions(item syncItem, cfg Config) applyActions {
	return applyActions{
		Post:  item.Edited || item.Missing.PostJSON,
		Media: item.Edited || item.VideoMismatch != "",
		MD: (cfg.WithMD || item.Existing.HasMd) &&
			(item.Edited || item.Missing.Markdown),
		Comments: item.NewComments ||
			item.Missing.Comments ||
			(item.Edited && (item.Existing.HasComments || cfg.WithComments)),
	}
}

// applyItem runs the apply phase for a single syncItem. Every flag is
// handled independently so combined changes (e.g. edited + comments +
// missing post.md) are all applied in a single pass.
//
// IsNew and JustUnlocked posts go through the first-download flow (the
// latter's on-disk artefacts predate the lock, and saveNewPost merges the
// prior entry). JustLocked only flips Locked=true. The actionable-update
// branch (Edited / NewComments / VideoMismatch / Missing.*) requires
// InState=true, which its three producers guarantee: classifyPost,
// runCheckMedia, and Sync's --check-files block. BackfillUpdatedAt-only items
// fall to the default no-op — applyBackfill already handled them before the
// per-item loop.
func (e *Engine) applyItem(st *state.State, item syncItem) {
	switch {
	case item.IsNew:
		e.c.Log.Printf("  downloading: %s", item.Post.Title)
		e.saveNewPost(st, &item.Post)
	case item.JustUnlocked:
		e.c.Log.Printf("  re-downloading (unlocked): %s", item.Post.Title)
		e.saveNewPost(st, &item.Post)
	case item.JustLocked:
		e.applyJustLocked(st, item)
	case item.IsActionable():
		e.c.Log.Printf("  updating: %s — %s", item.Post.Title, item.Detail())
		e.applyExisting(st, item)
	}
}

// saveNewPost runs the first-download flow for an accessible post: refresh
// signed media URLs (so post.json and the state entry both reflect the URLs
// actually downloaded against — see MaybeRefreshSignedURLs), write every
// requested artefact, then record the state entry. Shared by Sync's IsNew
// branch and DownloadAll's worker so the contract guards cannot drift.
//
// A prior entry may exist (Force mode, or a Locked=true entry whose post is
// accessible again — Sync's UNLOCKED and DownloadAll both land here): the
// entry is merged rather than rebuilt so HasMd / HasComments for artefacts
// still on disk survive a run without --md / --comments, those artefacts
// are regenerated against the fresh post, the surviving directory is reused
// instead of being orphaned by a title change, and media is replaced only
// when the entry's UpdatedAt says the post was edited since it fully landed.
//
// Returns true once the post's files are on disk — a failed state save is
// logged and counted as a failure, but does not un-download the files (the
// entry re-syncs as NEW on the next run).
func (e *Engine) saveNewPost(st *state.State, p *boosty.Post) bool {
	c := e.c
	e.stMu.Lock()
	old := st.Posts[p.ID]
	e.stMu.Unlock()

	post := e.MaybeRefreshSignedURLs(p)
	dirName := e.pickDirName(old, post)
	withMD := e.cfg.WithMD || old.HasMd
	withComments := e.cfg.WithComments || old.HasComments
	dirName, out, err := e.savePost(post, dirName, withMD, withComments, old.UpdatedAt)
	if err != nil {
		c.Log.Printf("  error: %v", err)
		e.failedPosts.Add(1)
		return false
	}
	// savePost returns dirName="" only for !post.HasAccess. Both callers queue
	// only accessible posts, so an empty name here is a contract violation —
	// recording it would aim every later read and write at the blog root.
	if dirName == "" {
		c.Log.Printf("  warning: savePost returned empty dirName for accessible post %q; state not updated", post.ID)
		e.failedPosts.Add(1)
		return false
	}
	entry := buildSyncEntry(old, post, dirName, true, out)
	if !e.commitEntry(st, post.ID, entry) {
		e.failedPosts.Add(1)
	}
	return true
}

// pickDirName returns the directory a post should be written to: the entry's
// existing directory when it is still on disk, otherwise a freshly formatted
// name. Adopting the fresh name unconditionally would orphan the old folder
// whenever the title changed. The result still goes through dirReserver.
func (e *Engine) pickDirName(old state.PostEntry, post *boosty.Post) string {
	if old.DirName != "" {
		if _, err := os.Stat(filepath.Join(e.blogDir(), old.DirName)); err == nil {
			return old.DirName
		}
	}
	return parser.FormatDirName(e.cfg.DirFormat, post.Title, post.PublishTime, post.ID)
}

// applyExisting re-produces the artefacts an actionable update needs
// (decideApplyActions). An Edited item replaces every media file and lets
// buildSyncEntry advance UpdatedAt when every channel lands; a
// VideoMismatch-only item replaces videos only.
//
// Requires InState=true: the entry is read from st.Posts and patched in
// place so failed writes never advance UpdatedAt / CommentsCount past disk
// reality and HasMd / HasComments survive a run without the matching flag.
func (e *Engine) applyExisting(st *state.State, item syncItem) {
	c := e.c

	// failedPosts counts per-POST failures. Several branches below can fail
	// for the same post (an artefact channel and then st.Save on the same
	// full disk), so the increment is funneled through one deferred check.
	failed := false
	defer func() {
		if failed {
			e.failedPosts.Add(1)
		}
	}()

	e.stMu.Lock()
	old := st.Posts[item.Post.ID]
	e.stMu.Unlock()

	blogDir := e.blogDir()
	dirName := e.res.reserve(blogDir, item.Post.ID, item.DirName)
	dir := filepath.Join(blogDir, dirName)
	// A user who removed the post directory between syncs would otherwise
	// have every atomic write ENOENT on the missing parent, every run.
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.Log.Printf("  error: %v", err)
		failed = true
		return
	}

	actions := decideApplyActions(item, e.cfg)
	fullPost, ok := e.fetchFullPost(item.Post, actions)
	if !ok {
		failed = true
		return
	}

	mode := downloader.ReplaceVideos
	if item.Edited {
		mode = downloader.ReplaceAll
	}
	out, err := e.runApplyActions(dir, &fullPost, mode, actions)
	if err != nil {
		// Logged here, not inside runApplyActions, so a post fails once in
		// the log; state below still records whatever did land.
		c.Log.Printf("  error: %v", err)
		failed = true
	}
	if !e.commitEntry(st, item.Post.ID, buildSyncEntry(old, &fullPost, dirName, item.Edited, out)) {
		failed = true
	}
}

// applyJustLocked flips Locked=true on the existing entry. Requires
// InState=true (enforced by classifyPost).
func (e *Engine) applyJustLocked(st *state.State, item syncItem) {
	e.stMu.Lock()
	entry := st.Posts[item.Post.ID]
	e.stMu.Unlock()
	entry.Locked = true
	if !e.commitEntry(st, item.Post.ID, entry) {
		e.failedPosts.Add(1)
	}
}

// commitEntry records entry under stMu and persists state. A failed save is
// logged and reported as false; the caller decides how to count it.
func (e *Engine) commitEntry(st *state.State, postID string, entry state.PostEntry) bool {
	e.stMu.Lock()
	defer e.stMu.Unlock()
	st.Add(postID, entry)
	if err := st.Save(); err != nil {
		e.c.Log.Printf("  warning: failed to save state: %v", err)
		return false
	}
	return true
}

// fetchFullPost returns the post to operate on. When NeedFetch is true a
// fresh GET is performed (Edited/Media/MD changes need the latest payload,
// including refreshed signed video URLs); otherwise the classified post is
// used directly — comments-only updates do not need to round-trip the post
// endpoint. Returns ok=false when the fetch fails or returns a stub (see
// Post.IsStub) so callers skip the apply with state preserved; the trigger
// re-fires on the next sync.
func (e *Engine) fetchFullPost(classified boosty.Post, actions applyActions) (boosty.Post, bool) {
	if !actions.NeedFetch() {
		return classified, true
	}
	var p boosty.Post
	if err := e.c.GetJSON(boosty.PostURL(e.cfg.Blog, classified.ID), &p); err != nil {
		e.c.Log.Printf("  error fetching post: %v", err)
		return boosty.Post{}, false
	}
	if p.IsStub() {
		e.c.Log.Printf("  warning: per-post fetch for %s returned no-access/empty stub; skipping apply (state preserved, will retry next sync)", classified.ID)
		return boosty.Post{}, false
	}
	return p, true
}

// runApplyActions writes each requested artefact into dir and records the
// per-channel outcome. Not-requested channels are seeded OK so they do not
// fail-close buildSyncEntry. A failed post.json write aborts the remaining
// channels — media written next to a stale post.json would be recorded as
// belonging to it. The returned error joins every channel failure; nil means
// every requested channel landed.
//
// Block parsing only happens when media or md consume it; a comments-only
// update skips the parse cost. External videos ride along with the media
// channel as best-effort (yt-dlp failures are logged, never fatal).
func (e *Engine) runApplyActions(dir string, post *boosty.Post, mode downloader.Mode, actions applyActions) (applyOutcome, error) {
	c := e.c
	out := applyOutcome{
		PostJSONOK: !actions.Post,
		MediaOK:    !actions.Media,
		MDOK:       !actions.MD,
		CommentsOK: !actions.Comments,
	}
	var errs []error

	if actions.Post {
		if err := writeJSON(filepath.Join(dir, "post.json"), post); err != nil {
			return out, fmt.Errorf("post.json: %w", err)
		}
		out.PostJSONOK = true
		c.Log.Printf("  saved post.json: %s", post.Title)
	}

	var parsed parser.ParsedContent
	if actions.Media || actions.MD {
		parsed = e.parsePostContent(post)
	}

	if actions.Media {
		if err := downloader.DownloadMedia(c, parsed.Media, dir, mode); err != nil {
			errs = append(errs, fmt.Errorf("media: %w", err))
		} else {
			out.MediaOK = true
		}
		if e.cfg.DownloadExternal {
			if err := downloader.DownloadExternal(c.Log, parsed.Media, dir); err != nil {
				c.Log.Printf("  warning: external download error: %v", err)
			}
		}
	}

	if actions.MD {
		if err := writePostMarkdown(post, parsed, dir); err != nil {
			errs = append(errs, fmt.Errorf("post.md: %w", err))
		} else {
			out.MDOK = true
			out.MDWritten = true
			c.Log.Printf("  saved post.md")
		}
	}

	if actions.Comments {
		capped, err := e.downloadComments(post.ID, dir, post.Count.Comments)
		if err != nil {
			errs = append(errs, fmt.Errorf("comments: %w", err))
		} else {
			out.CommentsOK = true
			out.CommentsWritten = true
			out.CommentsCapped = capped
		}
	}

	return out, errors.Join(errs...)
}

// applyBackfill mutates st.Posts to record UpdatedAt for legacy entries.
// Idempotent and safe to call before or after applyItem; per-item updates
// still operate on the corrected entries. classifyPost only sets
// BackfillUpdatedAt on InState=true posts.
func applyBackfill(st *state.State, items []syncItem) {
	for _, item := range items {
		if !item.BackfillUpdatedAt {
			continue
		}
		entry := st.Posts[item.Post.ID]
		entry.UpdatedAt = item.Post.UpdatedAt
		st.Posts[item.Post.ID] = entry
	}
}

// buildSyncEntry composes the post-apply state entry from the existing
// (post-backfill) entry, the post that was written, and the apply outcome.
// It is the single point where the contract "do not advance retry-controlling
// fields past what was verifiably persisted" is enforced:
//
//   - Title/DirName/Price/Tier are display metadata, refreshed unconditionally.
//   - UpdatedAt only advances when this run actually caught up with an
//     Edited post — meaning ALL four artefact channels (post.json, media,
//     post.md, comments) either were not required this run or completed
//     successfully. Other triggers (NewComments / VideoMismatch / Missing.*)
//     do not change the remote UpdatedAt, so the cached copy must not
//     advance off the back of them either.
//   - Locked clears only on the same all-channels-landed condition, so a
//     partial failure keeps the JustUnlocked trigger armed. Without this a
//     tier-toggle lock with no content edit would never re-fire (UpdatedAt
//     unchanged → not Edited; Locked already false → not JustUnlocked).
//   - CommentsCount / HasComments / CommentsCapped only advance when
//     comments were freshly written this run.
//   - HasMd only advances to true when post.md was freshly written; an
//     existing true value is preserved by virtue of starting from `old`.
//
// Pure function — no I/O — so the contract is table-testable.
func buildSyncEntry(old state.PostEntry, post *boosty.Post, dirName string,
	edited bool, out applyOutcome,
) state.PostEntry {
	entry := old
	entry.Title = post.Title
	entry.DirName = dirName
	entry.Price = post.Price
	entry.Tier = post.TierName()
	if out.allOK() {
		entry.Locked = false
		if edited {
			entry.UpdatedAt = post.UpdatedAt
		}
	}
	if out.CommentsWritten {
		entry.CommentsCount = post.Count.Comments
		entry.HasComments = true
		entry.CommentsCapped = out.CommentsCapped
	}
	if out.MDWritten {
		entry.HasMd = true
	}
	return entry
}
