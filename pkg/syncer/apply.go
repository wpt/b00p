package syncer

import (
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
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
// Dispatch is split per action category so each helper has a narrow,
// auditable contract — IsNew/JustUnlocked posts are *not* in state yet
// (no existing entry to read from), while JustLocked and the actionable-
// update branch require InState=true to safely read from st.Posts.
// classifyPost enforces these invariants; the helpers re-verify them
// defensively so a future refactor or a test breaking the contract
// surfaces as a logged skip instead of a silently-created ghost entry.
func (e *Engine) applyItem(blogDir string, st *state.State, item syncItem) {
	switch {
	case item.IsNew, item.JustUnlocked:
		e.applyNew(st, item)
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

// applyNew handles IsNew / JustUnlocked: posts without a usable prior state
// entry. SavePost performs the full download flow; on success a fresh
// state entry is written. No existing entry is read.
func (e *Engine) applyNew(st *state.State, item syncItem) {
	c := e.c
	c.Log.Printf("  downloading: %s", item.Post.Title)
	dirName, err := e.SavePost(&item.Post)
	if err != nil {
		c.Log.Printf("  error: %v", err)
		return
	}
	// SavePost returns dirName="" only when !post.HasAccess. classifyPost
	// guarantees IsNew/JustUnlocked carry HasAccess=true, so an empty
	// dirName here is a contract violation — recording an empty DirName
	// in state would poison future syncs (apply would join blogDir+""
	// and operate on the blog root). Refuse to advance state.
	if dirName == "" {
		c.Log.Printf("  warning: SavePost returned empty dirName for accessible post %q; state not updated", item.Post.ID)
		return
	}
	st.Add(item.Post.ID, e.postStateEntry(&item.Post, dirName))
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
	}
}

// applyJustLocked flips Locked=true on the existing entry. Requires
// InState=true (enforced by classifyPost; defensively re-checked here
// because a JustLocked with no entry would otherwise silently create one
// with Locked=true and zero metadata).
func (e *Engine) applyJustLocked(st *state.State, item syncItem) {
	c := e.c
	entry, ok := st.Get(item.Post.ID)
	if !ok {
		// Invariant violation: JustLocked implies InState=true. Log and skip
		// rather than create a ghost entry with empty metadata.
		c.Log.Printf("  warning: JustLocked on %s but no state entry; skipping", item.Post.ID)
		return
	}
	entry.Locked = true
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
	}
}

// applyUpdate handles the actionable-update path: Edited, NewComments,
// VideoMismatch, or any Missing.* artefact. Requires InState=true — the
// per-artefact UpdatedAt contract reads from the existing state entry,
// and reading a zero-valued entry would silently lose all prior metadata
// (Title, HasMd, HasComments, etc.) on save.
func (e *Engine) applyUpdate(blogDir string, st *state.State, item syncItem) {
	c := e.c
	old, ok := st.Get(item.Post.ID)
	if !ok {
		// Invariant violation: actionable non-new/non-unlocked items must be
		// InState. Log and skip rather than fabricate a state entry from a
		// freshly-fetched post — the fail-closed UpdatedAt contract depends
		// on reading the prior entry, not a zero value.
		c.Log.Printf("  warning: actionable update for %s but no state entry; skipping", item.Post.ID)
		return
	}

	actions := decideApplyActions(item, e.cfg)
	c.Log.Printf("  updating: %s — %s", item.Post.Title, item.Detail())
	dir := filepath.Join(blogDir, item.DirName)

	fullPost, ok := e.fetchFullPost(item.Post, actions)
	if !ok {
		return
	}

	out := e.runApplyActions(dir, &fullPost, item.Edited, actions)
	entry := buildSyncEntry(old, &fullPost, item.DirName, item.Edited, out)
	st.Add(item.Post.ID, entry)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save state: %v", err)
	}
}

// fetchFullPost returns the post to operate on. When NeedFetch is true a
// fresh GET is performed (Edited/Media/MD changes need the latest payload,
// including refreshed signed video URLs); otherwise the classified post is
// used directly — comments-only updates do not need to round-trip the
// post endpoint. Returns ok=false when the fetch fails so callers can
// fall through without mutating disk or state.
func (e *Engine) fetchFullPost(classified boosty.Post, actions applyActions) (boosty.Post, bool) {
	if !actions.NeedFetch() {
		return classified, true
	}
	var p boosty.Post
	if err := e.c.GetJSON(boosty.PostURL(e.cfg.Blog, classified.ID), &p); err != nil {
		e.c.Log.Printf("  error fetching post: %v", err)
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
		parsed = parser.ParseBlocks(fullPost.Data)
	}

	if actions.Media {
		if invalidateMediaForRedownload(parsed.Media, dir, edited, c.Log) {
			if err := downloader.DownloadMedia(c, parsed.Media, dir); err != nil {
				c.Log.Printf("  error re-downloading media: %v", err)
			} else {
				out.MediaOK = true
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
		if err := e.downloadComments(fullPost.ID, dir); err != nil {
			c.Log.Printf("  error: %v", err)
		} else {
			out.CommentsOK = true
			out.CommentsWritten = true
		}
	}

	return out
}

// applyBackfill mutates st.Posts to record UpdatedAt for legacy entries.
// Idempotent and safe to call before or after applyItem; per-item updates
// still operate on the corrected entries.
//
// Only mutates entries that genuinely exist in state — uses st.Get instead
// of a raw map access so a future bug that sets BackfillUpdatedAt on a
// post not in state (which classifyPost guarantees won't happen) cannot
// silently create a ghost entry with empty title/dirname.
func applyBackfill(st *state.State, items []syncItem) {
	for _, item := range items {
		if !item.BackfillUpdatedAt {
			continue
		}
		entry, ok := st.Get(item.Post.ID)
		if !ok {
			continue
		}
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
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("  error: cannot remove %s: %v (skipping redownload)", p, err)
			return false
		}
	}
	return true
}
