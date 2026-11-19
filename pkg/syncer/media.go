package syncer

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// mediaPlan decides which media files a run downloads over their existing
// copies, and carries the record of what those copies are
// (state.PostEntry.Media).
type mediaPlan struct {
	// landed maps a filename to the block ID of the bytes b00p last landed
	// there; nil when nothing is known.
	landed map[string]string
	// edited replaces every file that landed does not show holding the
	// item's block: a swapped block, an item without an ID, no record.
	edited bool
	// replace names files to re-download regardless (a --check-media
	// verdict on their size).
	replace []string
	// trustKept: a file kept on disk holds the item's block, so record may
	// name it. Only state vouches for that: the post is at the UpdatedAt
	// whose media fully landed, or the plan is an edit (which keeps only
	// files landed already shows holding the block).
	trustKept bool
	// kept names the files the run skips because they exist (see noteKept).
	kept map[string]bool
}

// newMediaPlan builds the plan for a post at updatedAt whose state entry is
// old (zero when untracked). An entry written before b00p kept a media
// record falls back to the post.json on disk, but only while its updatedAt
// equals old.UpdatedAt: that value advances only when every channel landed,
// so that copy's media is on disk. A post.json with any other updatedAt may
// have been written ahead of a media replacement that then failed.
func newMediaPlan(dir string, old state.PostEntry, updatedAt int64, edited bool, replace []string) mediaPlan {
	p := mediaPlan{
		landed:    old.Media,
		edited:    edited,
		replace:   replace,
		trustKept: edited || (old.UpdatedAt != 0 && updatedAt == old.UpdatedAt),
	}
	if edited && p.landed == nil && old.UpdatedAt != 0 {
		p.landed = landedFromPostJSON(dir, old.UpdatedAt)
	}
	return p
}

// noteKept records which items the run will keep: a non-empty file at the
// item's path that replaces says no to, the rule boosty.Download skips by.
// Without trustKept those bytes are of unknown block (a lost _state.json
// after a failed edit, a post edited between the listing and its fetch), and
// naming them after the current block would stop the next edit from ever
// replacing them.
func (p *mediaPlan) noteKept(dir string, media []parser.MediaItem) {
	p.kept = make(map[string]bool)
	for _, m := range media {
		if info, err := os.Stat(filepath.Join(dir, m.Filename)); err == nil && info.Size() > 0 && !p.replaces(m) {
			p.kept[m.Filename] = true
		}
	}
}

// replaces reports whether m is downloaded over its existing file.
func (p mediaPlan) replaces(m parser.MediaItem) bool {
	if slices.Contains(p.replace, m.Filename) {
		return true
	}
	return p.edited && (m.ID == "" || p.landed[m.Filename] != m.ID)
}

// record returns the media record after a run in which the given items
// landed. A file whose download failed keeps its entry: replacement never
// deletes first, so the bytes it names are still there. So does a kept file
// the plan cannot vouch for (trustKept). A file that landed for an item
// without an ID loses its entry, since nothing names its bytes.
func (p mediaPlan) record(landed []parser.MediaItem) map[string]string {
	rec := maps.Clone(p.landed)
	if rec == nil {
		rec = make(map[string]string, len(landed))
	}
	for _, m := range landed {
		if p.kept[m.Filename] && !p.trustKept {
			continue
		}
		if m.ID == "" {
			delete(rec, m.Filename)
		} else {
			rec[m.Filename] = m.ID
		}
	}
	return rec
}

// landedFromPostJSON maps each media filename of dir/post.json to its block
// ID, or returns nil unless that file parses and carries updatedAt.
func landedFromPostJSON(dir string, updatedAt int64) map[string]string {
	data, err := os.ReadFile(filepath.Join(dir, "post.json"))
	if err != nil {
		return nil
	}
	var p boosty.Post
	if err := json.Unmarshal(data, &p); err != nil || p.UpdatedAt != updatedAt {
		return nil
	}
	rec := make(map[string]string)
	for _, m := range parser.ParseBlocks(p.Data).Media {
		if m.ID != "" {
			rec[m.Filename] = m.ID
		}
	}
	return rec
}
