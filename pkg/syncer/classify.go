package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

// syncItem is a per-post classification with independent flags. A single
// post can carry several at once (edited AND comments changed AND video size
// mismatch); a single-action enum would silently drop the combined changes.
// The apply phase dispatches on each flag independently and re-fetches or
// re-downloads only what is actually needed.
type syncItem struct {
	Post     boosty.Post
	DirName  string
	Existing state.PostEntry // zero value if !InState
	InState  bool

	// Classification (set during phase 1; CheckMedia/CheckFiles may add).
	IsNew             bool
	IsLockedNew       bool // brand new, no access
	JustLocked        bool // existed accessible, now locked
	JustUnlocked      bool // existed locked, now accessible
	Edited            bool // updatedAt changed
	NewComments       bool // tracked comments differ from post.Count.Comments
	BackfillUpdatedAt bool // existing.UpdatedAt was 0; needs persisting

	// DiskCommentCount is the count of live top-level comments + their inlined
	// live replies read from comments.json (isDeleted stubs excluded, matching
	// what post.Count.Comments measures). Populated in classifyPost for posts in
	// state with HasComments=true. -1 means the file was missing, unreadable, or
	// corrupt; any non-negative value is directly comparable to
	// post.Count.Comments.
	DiskCommentCount int

	VideoMismatch string       // detail string (empty = no mismatch)
	Missing       missingFiles // file existence check result
}

// IsActionable reports whether the item needs apply-phase work beyond a
// pure UpdatedAt backfill (which is persisted regardless).
func (s syncItem) IsActionable() bool {
	return s.IsNew || s.JustUnlocked || s.JustLocked ||
		s.Edited || s.NewComments ||
		s.VideoMismatch != "" || s.Missing.Any()
}

// Labels returns short status tags for display ordered by severity.
func (s syncItem) Labels() []string {
	var labels []string
	switch {
	case s.IsNew:
		labels = append(labels, "NEW")
	case s.IsLockedNew:
		labels = append(labels, "LOCKED_NEW")
	case s.JustLocked:
		labels = append(labels, "LOCKED")
	case s.JustUnlocked:
		labels = append(labels, "UNLOCKED")
	}
	if s.Edited {
		labels = append(labels, "UPDATED")
	}
	if s.NewComments {
		labels = append(labels, "COMMENTS")
	}
	if s.VideoMismatch != "" {
		labels = append(labels, "VIDEO_MISMATCH")
	}
	if s.Missing.Any() {
		labels = append(labels, "FILES_MISSING")
	}
	return labels
}

// Detail aggregates per-flag detail strings for display.
func (s syncItem) Detail() string {
	var parts []string
	if s.JustLocked {
		parts = append(parts, "was accessible, now locked")
	}
	if s.JustUnlocked {
		parts = append(parts, "was locked, now accessible")
	}
	if s.Edited {
		parts = append(parts, "post edited")
	}
	if s.NewComments {
		switch {
		case s.DiskCommentCount < 0:
			// The trigger fired because comments.json is missing/unreadable,
			// not because of a count delta — a "N → N" line would read as a
			// spurious no-op. Name the real reason instead.
			parts = append(parts, fmt.Sprintf(
				"comments.json missing or unreadable (API: %d); refetching",
				s.Post.Count.Comments))
		case s.Existing.CommentsCapped:
			// Capped posts fire on the API count moving since the last
			// fetch; the disk count is permanently below it.
			parts = append(parts, fmt.Sprintf("comments: %d → %d (capped; %d on disk)",
				s.Existing.CommentsCount, s.Post.Count.Comments, s.DiskCommentCount))
		default:
			parts = append(parts, fmt.Sprintf("comments: %d → %d",
				s.DiskCommentCount, s.Post.Count.Comments))
		}
	}
	if s.VideoMismatch != "" {
		parts = append(parts, s.VideoMismatch)
	}
	if m := s.Missing.String(); m != "" {
		parts = append(parts, "missing "+m)
	}
	return strings.Join(parts, "; ")
}

// classifyPost compares post against state and returns a syncItem with all
// applicable flags set. State entries with an empty DirName never reach here
// — loadState drops them.
func classifyPost(post boosty.Post, st *state.State, blogDir string) syncItem {
	existing, inState := st.Get(post.ID)

	item := syncItem{Post: post, DiskCommentCount: -1}

	if !inState {
		// DirName stays empty for out-of-state posts: nothing reads it —
		// display prints titles, the check phases skip !InState items, and
		// the real directory name is decided at apply time (savePost formats
		// it from the possibly-refreshed post and the dirReserver may still
		// suffix it on collision), so a name computed here could only be
		// wrong or unused.
		if post.HasAccess {
			item.IsNew = true
		} else {
			item.IsLockedNew = true
		}
		return item
	}

	item.Existing = existing
	item.InState = true
	item.DirName = existing.DirName

	if !post.HasAccess {
		if !existing.Locked {
			item.JustLocked = true
		}
		return item
	}

	if existing.Locked {
		// Was locked, now accessible — UNLOCKED: full re-download through
		// the first-download flow, which also decides the directory (the
		// title may have changed during the lock).
		item.JustUnlocked = true
		return item
	}

	// State entries written before UpdatedAt was added to the schema have
	// UpdatedAt == 0; treating that as an edit would flag every such post
	// as UPDATED on first sync after upgrade. Require a known previous
	// value before declaring an edit.
	if existing.UpdatedAt != 0 && post.UpdatedAt != existing.UpdatedAt {
		item.Edited = true
	}
	if existing.UpdatedAt == 0 && post.UpdatedAt != 0 {
		item.BackfillUpdatedAt = true
	}

	// Comment-count trigger, only for posts whose comments are tracked
	// (HasComments): content flags are not retroactive, so a post saved
	// without --comments never fetches them from sync — --force backfills.
	//
	// Prefer disk reality over the state-cached count. The cached value is
	// post.Count.Comments at last save, so for posts whose Boosty count
	// includes inlined replies that weren't actually saved (the pre-
	// reply_limit bug), state matches API while disk silently has fewer;
	// reading comments.json catches that gap on the next sync without any
	// flag.
	//
	// A CommentsCapped post can never have disk catch up with the API (the
	// endpoint serves one page), so for those the API count at last fetch
	// (CommentsCount) is the baseline instead: the refetch fires once per
	// API-side change — new threads, deletions — and goes quiet after a
	// successful fetch rewrites CommentsCount.
	if existing.HasComments {
		n, ok := diskCommentCount(filepath.Join(blogDir, existing.DirName))
		switch {
		case !ok:
			// Missing or unreadable comments.json is itself a reason to
			// refetch when the post has any comments.
			if post.Count.Comments > 0 {
				item.NewComments = true
			}
		case existing.CommentsCapped:
			item.DiskCommentCount = n
			if post.Count.Comments != existing.CommentsCount {
				item.NewComments = true
			}
		default:
			item.DiskCommentCount = n
			if n != post.Count.Comments {
				item.NewComments = true
			}
		}
	}

	return item
}

// diskCommentCount returns the on-disk equivalent of post.Count.Comments —
// the liveCommentCount of comments.json. Returns ok=false when the file is
// missing, unreadable, or fails to parse; the caller treats that as a reason
// to refetch when the post has any comments at all.
//
// Files written by builds that predate the IsDeleted field carry unmarked
// stubs, which count as live here. The inflated count triggers a refetch as
// soon as it disagrees with the API count, and the rewrite adds the markers —
// but while the two coincidentally match (K unmarked stubs offsetting K new
// live comments), the gap goes undetected until the counts drift. Closing
// it would mean refetching every legacy post unconditionally.
func diskCommentCount(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "comments.json"))
	if err != nil {
		return 0, false
	}
	var comments []boosty.Comment
	if err := json.Unmarshal(data, &comments); err != nil {
		return 0, false
	}
	n, _ := liveCommentCount(comments)
	return n, true
}

// liveCommentCount counts the live (non-deleted) comments in a fetched or
// stored comment list — live top-level comments plus the live replies
// actually inlined into each thread — and reports whether any thread inlined
// fewer live replies than its ReplyCount claims exist (reply truncation:
// the server holds live replies the page didn't include; a nil replies
// object with ReplyCount > 0 counts as truncation too, fail-closed).
//
// Deleted comments come back as IsDeleted stubs (they hold thread structure —
// a deleted parent keeps its live replies attached) and are excluded from
// both numbers: post.Count.Comments and per-thread ReplyCount are live-only,
// so counting stubs would hold disk permanently above API for any post that
// ever had a comment deleted, refiring COMMENTS on every sync with no path
// to closure. We count the live subset of c.Replies.Data (not c.ReplyCount),
// since disk reflects what was stored, not what the server claims exists.
//
// Shared by diskCommentCount (classify, read side) and downloadComments
// (save, write side) so the counting contract cannot drift between what
// sync writes and what it later reads back.
func liveCommentCount(comments []boosty.Comment) (live int, replyTruncated bool) {
	for _, c := range comments {
		if !c.IsDeleted {
			live++
		}
		liveReplies := 0
		if c.Replies != nil {
			for _, r := range c.Replies.Data {
				if !r.IsDeleted {
					liveReplies++
				}
			}
		}
		live += liveReplies
		if liveReplies < c.ReplyCount {
			replyTruncated = true
		}
	}
	return live, replyTruncated
}
