package syncer

import (
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/fileutil"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// applyActions describes which artefacts the apply phase needs to (re)produce
// for a single syncItem. Computed by decideApplyActions; consumed by applyItem.
// Decoupling the decision from execution makes the rule set itself
// table-testable without faking any client or filesystem.
type applyActions struct {
	Post     bool // re-write post.json from a freshly fetched payload
	Media    bool // invalidate + re-download media (DownloadFile skips existing)
	MD       bool // regenerate post.md from current data
	Comments bool // refetch comments.json
}

// NeedFetch reports whether any action requires a fresh GetJSON for the post.
// Comments fetching goes through its own endpoint and does not require this.
func (a applyActions) NeedFetch() bool {
	return a.Post || a.Media || a.MD
}

// applyOutcome captures per-artefact success after applyItem ran. It is the
// input to buildSyncEntry's "do not advance retry-controlling fields past
// disk reality" contract: each *OK gates the corresponding state field, and
// the *Written flags promote HasComments/HasMd from false to true only when
// this run actually produced the artefact.
//
// "Not needed" counts as OK — we only fail-close fields whose artefact this
// run was responsible for producing. The Written flags differ from the OK
// flags only when an artefact was not requested (OK=true, Written=false),
// in which case the prior value is preserved.
type applyOutcome struct {
	PostJSONOK bool
	MediaOK    bool
	MDOK       bool
	CommentsOK bool

	CommentsWritten bool
	MDWritten       bool
	// CommentsCapped reports that the comments fetch (when it happened)
	// hit Boosty's structural ceiling — the state side uses this to stop
	// re-triggering NewComments on every sync when disk count can never
	// catch up to API count. Only meaningful when CommentsWritten=true.
	CommentsCapped bool
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
//   - Comments: count grew (NewComments), or comments.json went missing, or
//     post was Edited and either the prior entry tracked comments or the
//     current run carries WithComments. The `cfg.WithComments && NewComments`
//     case from earlier code was redundant: NewComments alone already fires.
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
// missing post.md) are all applied in a single pass instead of one of
// them being silently skipped.
//
// Dispatch is split per category. IsNew posts have no prior state entry —
// applyNew handles them via SavePost. JustUnlocked posts have a locked
// state stub plus stale on-disk artefacts from before the lock —
// applyJustUnlocked re-downloads everything (media must be invalidated
// first because DownloadFile skips existing non-empty files) and preserves
// prior HasMd/HasComments so artefacts still on disk aren't silently
// dropped from state when the run lacks --md/--comments. JustLocked just
// flips Locked=true on the existing entry. The actionable-update branch
// (Edited / NewComments / VideoMismatch / Missing.*) requires InState=true
// to safely read from st.Posts. BackfillUpdatedAt items still reach this
// dispatch but no case matches (IsActionable excludes them), so they fall
// to the default no-op — the actual backfill write happens upstream in
// applyBackfill before the per-item loop in Sync.
//
// The InState=true precondition for JustLocked / JustUnlocked / actionable
// is enforced by three producer-side gates: classifyPost (for Edited,
// NewComments, JustLocked, JustUnlocked), runCheckMedia in checks.go (for
// VideoMismatch), and Sync's --check-files block in sync.go (for Missing.*).
func (e *Engine) applyItem(blogDir string, st *state.State, item syncItem) {
	switch {
	case item.IsNew:
		e.applyNew(st, item)
	case item.JustUnlocked:
		e.applyJustUnlocked(blogDir, st, item)
	case item.JustLocked:
		e.applyJustLocked(st, item)
	case item.IsActionable():
		e.applyUpdate(blogDir, st, item)
		// default: pure backfill or no-change. applyBackfill was called in
		// Sync before the apply loop, and its mutations are persisted by the
		// per-item st.Save() calls in the actionable branches above — nothing
		// left to do for this item.
	}
}

// applyNew handles IsNew posts: no prior state entry exists.
func (e *Engine) applyNew(st *state.State, item syncItem) {
	e.c.Log.Printf("  downloading: %s", item.Post.Title)
	e.saveNewPost(st, &item.Post)
}

// saveNewPost runs the full first-download flow for an accessible post with
// no usable prior state: refresh signed video URLs (so post.json and the
// state entry both reflect the URLs actually downloaded against — see
// MaybeRefreshSignedURLs), SavePost, then record the fresh state entry.
// Shared by Sync's applyNew and DownloadAll's worker so the contract guards
// cannot drift between the two paths. Returns true once the post's files are
// on disk — a failed state save is logged and counted as a failure, but does
// not un-download the files (the entry re-syncs as NEW on the next run).
func (e *Engine) saveNewPost(st *state.State, p *boosty.Post) bool {
	c := e.c
	post := e.MaybeRefreshSignedURLs(p)
	dirName, capped, err := e.SavePost(post)
	if err != nil {
		c.Log.Printf("  error: %v", err)
		e.failedPosts.Add(1)
		return false
	}
	// SavePost returns dirName="" only when !post.HasAccess. Both callers
	// queue only accessible posts (classifyPost guarantees IsNew carries
	// HasAccess=true; DownloadAll skips locked posts before queueing), so an
	// empty dirName here is a contract violation — recording an empty
	// DirName in state would poison future syncs (apply would join
	// blogDir+"" and operate on the blog root). Refuse to advance state.
	if dirName == "" {
		c.Log.Printf("  warning: SavePost returned empty dirName for accessible post %q; state not updated", post.ID)
		e.failedPosts.Add(1)
		return false
	}
	entry := e.postStateEntry(post, dirName)
	entry.CommentsCapped = capped
	e.stMu.Lock()
	defer e.stMu.Unlock()
	st.Add(post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
		e.failedPosts.Add(1)
	}
	return true
}

// applyJustUnlocked handles posts that were locked at last sync and are now
// accessible. The prior state entry has Locked=true plus whatever metadata
// was recorded before the lock, and the on-disk directory may still hold
// stale post.json/media/post.md/comments.json from then.
//
// Three failure modes the naive "dispatch to applyNew" path produced are
// fixed here:
//
//  1. Media skip. DownloadFile skips existing non-empty files for crash-
//     safety, so re-running SavePost over the prior directory left the
//     stale image_001.jpg / video_001.mp4 untouched while post.json was
//     refreshed. invalidateMediaForRedownload with edited=true removes
//     every media item DownloadMedia would touch, forcing a real refetch.
//  2. Flag wipe. postStateEntry seeds HasMd/HasComments from the engine's
//     current Config flags, so a sync run without --md or --comments would
//     reset those flags to false even though post.md / comments.json
//     existed on disk from the original (pre-lock) download. Starting from
//     the prior entry via buildSyncEntry preserves the flags for artefacts
//     that are still present, and the OR into actions.MD/.Comments below
//     ensures the prior md/comments are regenerated against the fresh post.
//  3. Folder orphaning. classify.go re-derives item.DirName from the (now
//     possibly changed) post title; if SavePost picked the new name, the
//     original folder with all its files was silently abandoned. Reusing
//     the existing on-disk DirName when it's still there keeps the data
//     under one roof.
//
// Signed video URLs returned by the list endpoint may have expired by the
// time the apply queue reaches this post, so a fresh post fetch is also
// required before re-downloading media — same reason applyUpdate uses
// fetchFullPost for any actions.NeedFetch() path.
func (e *Engine) applyJustUnlocked(blogDir string, st *state.State, item syncItem) {
	c := e.c
	c.Log.Printf("  re-downloading (unlocked): %s", item.Post.Title)

	// failedPosts counts per-POST failures (config.go documents the
	// contract; Sync reports it as "N failed post(s)"). Multiple failure
	// branches below can fire for the same post — e.g. an artefact-channel
	// failure followed by an st.Save failure on the same full disk — so the
	// increments are funneled through one deferred check.
	failed := false
	defer func() {
		if failed {
			e.failedPosts.Add(1)
		}
	}()

	e.stMu.Lock()
	old := st.Posts[item.Post.ID]
	e.stMu.Unlock()

	// Reuse the existing folder if it survived the lock period; classify.go
	// pre-filled item.DirName with the freshly-formatted name (in case the
	// title changed during the lock), but adopting that unconditionally
	// orphans the old directory.
	dirName := item.DirName
	if old.DirName != "" {
		if _, err := os.Stat(filepath.Join(blogDir, old.DirName)); err == nil {
			dirName = old.DirName
		}
	}
	dirName = e.res.reserve(blogDir, item.Post.ID, dirName)
	dir := filepath.Join(blogDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.Log.Printf("  error: %v", err)
		failed = true
		return
	}

	actions := applyActions{
		Post:     true,
		Media:    true,
		MD:       e.cfg.WithMD || old.HasMd,
		Comments: e.cfg.WithComments || old.HasComments,
	}
	fullPost, ok := e.fetchFullPost(item.Post, actions)
	if !ok {
		failed = true
		return
	}

	// edited=true so invalidateMediaForRedownload removes every media item,
	// not just videos — same scope as a real Edited post would need.
	out := e.runApplyActions(dir, &fullPost, true, actions)
	if !(out.PostJSONOK && out.MediaOK && out.MDOK && out.CommentsOK) {
		// Some artefact channel failed (logged inside runApplyActions);
		// keep Locked=true (handled below) AND tell the orchestrator so
		// the top-level call exits non-zero.
		failed = true
	}

	entry := buildSyncEntry(old, &fullPost, dirName, true, out)
	// Only clear Locked once every requested channel actually landed.
	// runApplyActions seeds *OK=true for channels that were not requested,
	// so this is also true for sync runs without --md/--comments where
	// old.HasMd/HasComments was false. Partial success leaves Locked=true
	// so the next sync re-fires JustUnlocked and retries — without this,
	// a tier-toggle lock with no content edit would never re-trigger
	// (UpdatedAt unchanged → not Edited; Locked already false → not
	// JustUnlocked) and the stale artefacts would linger until --check-*.
	if out.PostJSONOK && out.MediaOK && out.MDOK && out.CommentsOK {
		entry.Locked = false
	}
	e.stMu.Lock()
	defer e.stMu.Unlock()
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
		failed = true
	}
}

// applyJustLocked flips Locked=true on the existing entry. Requires
// InState=true (enforced by classifyPost).
func (e *Engine) applyJustLocked(st *state.State, item syncItem) {
	c := e.c
	e.stMu.Lock()
	defer e.stMu.Unlock()
	entry := st.Posts[item.Post.ID]
	entry.Locked = true
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
		e.failedPosts.Add(1)
	}
}

// applyUpdate handles the actionable-update path: Edited, NewComments,
// VideoMismatch, or any Missing.* artefact. Requires InState=true — the
// per-artefact UpdatedAt contract reads from the existing state entry,
// and reading a zero-valued entry would silently lose all prior metadata
// (Title, HasMd, HasComments, etc.) on save. The invariant is enforced
// upstream by three producers: classifyPost (Edited, NewComments),
// runCheckMedia in checks.go (VideoMismatch), and Sync's --check-files
// block in sync.go (Missing.*).
func (e *Engine) applyUpdate(blogDir string, st *state.State, item syncItem) {
	c := e.c

	// Same per-POST failure accounting as applyJustUnlocked: several failure
	// branches can fire for one post, but failedPosts must count it once.
	failed := false
	defer func() {
		if failed {
			e.failedPosts.Add(1)
		}
	}()

	e.stMu.Lock()
	old := st.Posts[item.Post.ID]
	e.stMu.Unlock()

	actions := decideApplyActions(item, e.cfg)
	c.Log.Printf("  updating: %s — %s", item.Post.Title, item.Detail())
	// Reserve the directory even though it normally already belongs to this
	// post on disk: while its post.json is missing or corrupt (exactly the
	// Missing.PostJSON repair window), dirReserver's disk probe reports the
	// directory as free, so a same-run NEW post with a colliding formatted
	// name could otherwise claim it and the two posts would interleave
	// artefacts in one folder. Reserving first makes the in-memory claim
	// protect the name; a no-op when uncontended (returns item.DirName).
	dirName := e.res.reserve(blogDir, item.Post.ID, item.DirName)
	dir := filepath.Join(blogDir, dirName)
	// Defensive MkdirAll: if the user manually removed/renamed the post
	// directory between syncs, writeJSON / writePostMarkdown / downloadComments
	// all go through fileutil.WriteFileAtomic → os.CreateTemp(dir, ...) which
	// ENOENTs on a missing parent. The media downloaders call their own
	// MkdirAll, but without this line a deleted directory leaves post.json /
	// post.md / comments.json silently failing every sync. Cheap on the happy
	// path (idempotent), corrects on the user-edited path.
	if err := os.MkdirAll(dir, 0755); err != nil {
		c.Log.Printf("  error: %v", err)
		failed = true
		return
	}

	fullPost, ok := e.fetchFullPost(item.Post, actions)
	if !ok {
		failed = true
		return
	}

	out := e.runApplyActions(dir, &fullPost, item.Edited, actions)
	if !(out.PostJSONOK && out.MediaOK && out.MDOK && out.CommentsOK) {
		// Some artefact channel failed (logged inside runApplyActions);
		// surface to the orchestrator so the top-level call exits non-zero.
		failed = true
	}
	entry := buildSyncEntry(old, &fullPost, dirName, item.Edited, out)
	e.stMu.Lock()
	defer e.stMu.Unlock()
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
		failed = true
	}
}

// fetchFullPost returns the post to operate on. When NeedFetch is true a
// fresh GET is performed (Edited/Media/MD changes need the latest payload,
// including refreshed signed video URLs); otherwise the classified post is
// used directly — comments-only updates do not need to round-trip the
// post endpoint. Returns ok=false when the fetch fails so callers can
// fall through without mutating disk or state.
//
// A success-but-degraded response (HasAccess=false or empty Data) is treated
// as failure: subscription lapsed between list-call and per-post call, or the
// post was locked server-side after the classifier saw it. Writing the stub
// over the existing post.json + parsing empty Data into empty media (which
// makes invalidateMediaForRedownload a no-op and trivially nil-errors
// DownloadMedia) would tick every channel OK and let buildSyncEntry advance
// UpdatedAt + clear Locked against a corrupted payload — permanent on-disk
// damage no trigger could detect afterwards. Same guard as MaybeRefreshSignedURLs.
func (e *Engine) fetchFullPost(classified boosty.Post, actions applyActions) (boosty.Post, bool) {
	if !actions.NeedFetch() {
		return classified, true
	}
	var p boosty.Post
	if err := e.c.GetJSON(boosty.PostURL(e.cfg.Blog, classified.ID), &p); err != nil {
		e.c.Log.Printf("  error fetching post: %v", err)
		return boosty.Post{}, false
	}
	if !p.HasAccess || len(p.Data) == 0 {
		e.c.Log.Printf("  warning: per-post fetch for %s returned no-access/empty stub; skipping apply (state preserved, will retry next sync)", classified.ID)
		return boosty.Post{}, false
	}
	return p, true
}

// runApplyActions executes each requested artefact write and records the
// per-channel outcome. The outcome seeds "not requested = OK" so unrequested
// artefacts do not fail-close the UpdatedAt advance in buildSyncEntry.
//
// Block parsing is only done when a downstream artefact (media or md)
// actually consumes it; a comments-only update skips the parse cost.
func (e *Engine) runApplyActions(dir string, fullPost *boosty.Post, edited bool, actions applyActions) applyOutcome {
	c := e.c
	out := applyOutcome{
		PostJSONOK: !actions.Post,
		MediaOK:    !actions.Media,
		MDOK:       !actions.MD,
		CommentsOK: !actions.Comments,
	}

	if actions.Post {
		if err := writeJSON(filepath.Join(dir, "post.json"), *fullPost); err != nil {
			c.Log.Printf("  error writing post.json: %v", err)
		} else {
			out.PostJSONOK = true
		}
	}

	var parsed parser.ParsedContent
	if actions.Media || actions.MD {
		parsed = e.parsePostContent(fullPost)
	}

	if actions.Media {
		if invalidateMediaForRedownload(parsed.Media, dir, edited, c.Log) {
			if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
				c.Log.Printf("  error re-downloading media: %v", err)
			} else {
				out.MediaOK = true
			}
		}
		// External videos (YouTube/VK embeds) are opt-in best-effort. We
		// invoke yt-dlp here for the same reason as SavePost: a JustUnlocked
		// or Edited post that gained / changed an external embed would
		// otherwise be silently skipped because runApplyActions did not
		// know about external media at all. Failure is logged, not fatal.
		if e.cfg.DownloadExternal {
			if err := downloader.DownloadExternal(c.Log, parsed.Media, dir); err != nil {
				c.Log.Printf("  warning: external download error: %v", err)
			}
		}
	}

	if actions.MD {
		if err := writePostMarkdown(fullPost, parsed, dir); err != nil {
			c.Log.Printf("  error writing post.md: %v", err)
		} else {
			out.MDOK = true
			out.MDWritten = true
		}
	}

	if actions.Comments {
		capped, err := e.downloadComments(fullPost.ID, dir, fullPost.Count.Comments)
		if err != nil {
			c.Log.Printf("  error: %v", err)
		} else {
			out.CommentsOK = true
			out.CommentsWritten = true
			out.CommentsCapped = capped
		}
	}

	return out
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
// (post-backfill) entry, the freshly fetched post, and the apply outcome.
// It is the single point where the contract "do not advance retry-controlling
// fields past what was verifiably persisted" is enforced:
//
//   - Title/DirName/Price/Tier are display metadata, refreshed unconditionally.
//   - UpdatedAt only advances when this run actually caught up with an
//     Edited post — meaning ALL four artefact channels (post.json, media,
//     post.md, comments) either were not required this run or completed
//     successfully. Other triggers (NewComments / VideoMismatch / Missing.*)
//     do not change the remote UpdatedAt, so we must not advance our cached
//     copy off the back of them.
//   - CommentsCount / HasComments only advance when comments were freshly
//     written this run.
//   - HasMd only advances to true when post.md was freshly written; an
//     existing true value is preserved by virtue of starting from `old`.
//
// Pure function — no I/O, suitable for table-driven testing of the bug
// where a failed artefact write used to silently bump UpdatedAt.
func buildSyncEntry(old state.PostEntry, fullPost *boosty.Post, dirName string,
	edited bool, out applyOutcome,
) state.PostEntry {
	entry := old
	entry.Title = fullPost.Title
	entry.DirName = dirName
	entry.Price = fullPost.Price
	if fullPost.SubscriptionLevel != nil {
		entry.Tier = fullPost.SubscriptionLevel.Name
	} else {
		entry.Tier = ""
	}
	if edited && out.PostJSONOK && out.MediaOK && out.MDOK && out.CommentsOK {
		entry.UpdatedAt = fullPost.UpdatedAt
	}
	if out.CommentsWritten {
		entry.CommentsCount = fullPost.Count.Comments
		entry.HasComments = true
		entry.CommentsCapped = out.CommentsCapped
	}
	if out.MDWritten {
		entry.HasMd = true
	}
	return entry
}

// invalidateMediaForRedownload removes local copies of media items so the
// subsequent DownloadMedia call actually fetches fresh bytes — DownloadFile
// skips existing non-empty files, so without removal an edited post with
// replaced media at the same filename (image_001.jpg etc.) would keep the
// stale local copy and we'd record success against new state.
//
// Both the final file AND the resume sidecar (.tmp + .tmp.url) are removed.
// Without dropping .tmp, a previous mid-stream failure for THIS filename
// against a now-stale URL would resume via Range against the freshly-signed
// URL and concatenate old bytes [0..N-1] with new bytes [N..end] — silent
// corruption that --check-media cannot detect when total length matches.
//
// Removal scope:
//   - Always skip external_video (DownloadMedia also skips it).
//   - Pure VideoMismatch (edited=false) only invalidates videos.
//   - Edited invalidates every media item DownloadMedia would touch.
//
// Returns false if a removal failed for a reason other than ENOENT — in
// that case the caller MUST NOT proceed with download to avoid leaving
// the directory in a half-cleaned state. ENOENT is normal (the file
// might already be gone, or never existed) and treated as success.
func invalidateMediaForRedownload(media []parser.MediaItem, dir string,
	edited bool, log boosty.Logger,
) bool {
	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		if !edited && m.Type != "video" {
			continue
		}
		p := filepath.Join(dir, m.Filename)
		for _, target := range []string{p, p + ".tmp", p + ".tmp.url"} {
			if err := fileutil.RemoveIfExists(target); err != nil {
				log.Printf("  error: cannot remove %s: %v (skipping redownload)", target, err)
				return false
			}
		}
	}
	return true
}
