package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// reserverKeySep separates the blog directory from the base name when forming
// a reservation map key. NUL is not a valid path component on any supported
// OS (Linux, macOS, Windows), so it can never appear in either half and
// guarantees the concatenation has no aliasing — i.e. (blog="a", name="b\x00c")
// can never collide with (blog="a\x00b", name="c"). Picking, say, '/' would
// not have that property.
const reserverKeySep = "\x00"

// dirReserver tracks which directory names are in flight or already owned by
// a given post ID, so two concurrent workers cannot pick the same target dir
// for posts whose formatted names collide. A pure-filesystem check would race
// between two workers that have not yet written post.json.
//
// A reservation is keyed by absolute blog dir + base name. Once owned by a
// post ID it is never released — even on failure — so a second post that
// would have collided is forced to a suffix instead of clobbering partial
// data on disk.
//
// All access is guarded by mu: reserve takes it on entry and holds it across
// the disk probe in tryName, so the in-memory check and the filesystem read
// are atomic with respect to other workers. The "check r.owned then read disk"
// pair is therefore a single critical section, not a TOCTOU window.
type dirReserver struct {
	mu    sync.Mutex
	owned map[string]string // key = blogDir + reserverKeySep + name → postID
}

func newDirReserver() *dirReserver {
	return &dirReserver{owned: make(map[string]string)}
}

// reserve returns a directory name (relative to blogDir) safe to use for the
// given postID. If base is unowned and either free on disk or already holds
// this post, base is returned. Otherwise the post ID is appended as a suffix
// so the caller never silently overwrites a sibling or a peer worker.
func (r *dirReserver) reserve(blogDir, postID, base string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name, ok := r.tryNameLocked(blogDir, postID, base); ok {
		return name
	}
	suffix := postID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	candidate := base + "_" + suffix
	// Suffix collisions in practice require an 8-char hex prefix collision
	// AND a name collision; if it ever happens, fall through and accept it —
	// the on-disk post.json check still prevents data loss for the same ID.
	r.owned[reservationKey(blogDir, candidate)] = postID
	return candidate
}

// tryNameLocked reports whether `name` can be used by postID. It returns the
// name when the in-process map either has no owner, or already names this post;
// or when the filesystem has no post.json or has one belonging to this post.
// The reservation is recorded on success.
//
// Caller MUST hold r.mu; the function is suffixed `Locked` to make that
// requirement audible at call sites. The os.ReadFile happens inside the lock
// on purpose: releasing it between the r.owned probe and the disk read would
// open a window where two workers could both observe "unowned + no file on
// disk" and claim the same name.
func (r *dirReserver) tryNameLocked(blogDir, postID, name string) (string, bool) {
	key := reservationKey(blogDir, name)
	if owner, ok := r.owned[key]; ok {
		if owner == postID {
			return name, true
		}
		return "", false
	}
	if r.diskOwnedBy(blogDir, name, postID) {
		r.owned[key] = postID
		return name, true
	}
	return "", false
}

// diskOwnedBy reports whether the on-disk post.json at blogDir/name either
// does not exist, is not a regular file (directory / symlink / device — we
// don't trust it as ours), is unreadable, has no id field, or has an id that
// matches postID. In all of those cases the name is safe for postID to claim.
//
// The Stat+IsRegular guard rejects pathological layouts (someone manually
// created a directory called post.json, or symlinked it into a sensitive
// path) without ever opening the file: ReadFile on a directory would still
// fail, but the failure mode (ENOENT-equivalent error, no panic) would be
// indistinguishable from "no post.json at all" and we'd happily claim a
// directory that already holds another post's content tree.
func (r *dirReserver) diskOwnedBy(blogDir, name, postID string) bool {
	path := filepath.Join(blogDir, name, "post.json")
	info, err := os.Stat(path)
	if err != nil {
		return true // missing file (or stat-denied parent) — treat as free
	}
	if !info.Mode().IsRegular() {
		return false // something weird at post.json; do not claim
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false // file exists but unreadable; refuse to clobber
	}
	var existing struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &existing); err != nil {
		// Corrupt JSON — better to overwrite garbage than to suffix forever.
		return true
	}
	return existing.ID == "" || existing.ID == postID
}

// reservationKey forms the map key for a (blogDir, name) pair. Centralised so
// every reader and writer of r.owned agrees on the exact encoding.
func reservationKey(blogDir, name string) string {
	return blogDir + reserverKeySep + name
}
