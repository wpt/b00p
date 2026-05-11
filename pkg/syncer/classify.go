package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// syncItem is a per-post classification with independent flags. A single
// post can carry multiple flags simultaneously (e.g. edited AND comments
// changed AND video size mismatch) — the previous implementation used a
// single-action enum and silently dropped combined changes. The apply phase
// dispatches on each flag independently and re-fetches/re-downloads only
// what is actually needed.
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
	NewComments       bool // disk-side count differs from post.Count.Comments
	BackfillUpdatedAt bool // existing.UpdatedAt was 0; needs persisting

	// DiskCommentCount is the count of top-level comments + their inlined replies
	// read from comments.json. Populated in classifyPost for posts in state with
	// HasComments=true. -1 means the file was missing, unreadable, or corrupt;
	// any non-negative value is directly comparable to post.Count.Comments. Used
	// instead of Existing.CommentsCount as the trigger source so legacy state
	// entries that cached an inflated API count cannot mask on-disk gaps.
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
		// Prefer the disk count when available — that's the value the trigger
		// fired on, and is what the user actually has locally. Fall back to the
		// state-cached count for posts that never had comments tracked.
		from := s.Existing.CommentsCount
		if s.Existing.HasComments && s.DiskCommentCount >= 0 {
			from = s.DiskCommentCount
		}
		parts = append(parts, fmt.Sprintf("comments: %d → %d",
			from, s.Post.Count.Comments))
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
// applicable flags set. dirFormat governs the freshly-formatted directory
// name (used for new/unlocked posts where the title may have changed).
func classifyPost(post boosty.Post, st *state.State, blogDir, dirFormat string) syncItem {
	dirName := parser.FormatDirName(dirFormat, post.Title, post.PublishTime, post.ID)
	existing, inState := st.Get(post.ID)

	item := syncItem{Post: post, DirName: dirName, DiskCommentCount: -1}

	if !inState {
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
		// Was locked, now accessible — treat like UNLOCKED: full re-download.
		// Use the freshly-formatted dir name in case the title changed.
		item.JustUnlocked = true
		item.DirName = dirName
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

	// Comment-count trigger: prefer disk reality over the state-cached count.
	// The cached value is post.Count.Comments at last save, so for posts whose
	// Boosty count includes inlined replies that weren't actually saved (the
	// pre-reply_limit bug), state matches API while disk silently has fewer.
	// Reading comments.json catches that gap on the next sync without any flag.
	//
	// For posts the user never asked to track comments (HasComments=false) we
	// have no disk file to consult, so fall back to the legacy state-vs-API
	// comparison — preserves prior behavior for that case.
	if existing.HasComments {
		if n, ok := diskCommentCount(filepath.Join(blogDir, existing.DirName)); ok {
			item.DiskCommentCount = n
			if n != post.Count.Comments {
				item.NewComments = true
			}
		} else {
			// Missing or unreadable comments.json with HasComments=true is
			// itself a reason to refetch when the post has any comments.
			if post.Count.Comments > 0 {
				item.NewComments = true
			}
		}
	} else if post.Count.Comments != existing.CommentsCount {
		item.NewComments = true
	}

	return item
}

// diskCommentCount returns the on-disk equivalent of post.Count.Comments —
// top-level comments plus the replies that the API actually inlined into each
// of them. Returns ok=false when the file is missing, unreadable, or fails to
// parse; the caller treats that as a reason to refetch when the post has any
// comments at all. We only count len(c.Replies.Data) (not c.ReplyCount), since
// disk reflects what was stored, not what the server claims exists.
func diskCommentCount(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "comments.json"))
	if err != nil {
		return 0, false
	}
	var comments []boosty.Comment
	if err := json.Unmarshal(data, &comments); err != nil {
		return 0, false
	}
	n := len(comments)
	for _, c := range comments {
		if c.Replies != nil {
			n += len(c.Replies.Data)
		}
	}
	return n, true
}
