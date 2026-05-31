package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// reserverKeySep separates the blog directory from the base name when forming
// a reservation map key. NUL is not a valid path component on any supported
// OS (Linux, macOS, Windows), so it can never appear in either half and
// guarantees the concatenation has no aliasing — i.e. (blog="a", name="b\x00c")
// can never collide with (blog="a\x00b", name="c"). Picking, say, '/' would
// not have that property.
const reserverKeySep = "\x00"

// caseFoldFS is true on filesystems whose default presents directory names
// case-insensitively to userspace: NTFS and APFS. Without folding, two posts
// whose sanitized titles differ only in case ("Стрим" vs "стрим", "Test" vs
// "test") would each pass the in-memory r.owned check independently while
// the disk-side os.MkdirAll quietly reopens the existing folder of the first
// — the second post's writes then clobber the first.
//
// strings.ToLower handles Unicode (Cyrillic, accented Latin, etc.) via
// full-Unicode case mapping. macOS APFS no longer auto-normalizes NFC/NFD
// (since 10.13), so a plain ToLower covers the realistic collision space
// without dragging in golang.org/x/text.
var caseFoldFS = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

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
// the disk probe in tryNameLocked, so the in-memory check and the filesystem
// read are atomic with respect to other workers. The "check r.owned then
// read disk" pair is therefore a single critical section, not a TOCTOU
// window.
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
	// The suffixed candidate goes through the same memory + disk validation
	// as the base name — claiming it blindly would silently overwrite a
	// sibling reservation (or a foreign on-disk post.json) on the rare
	// 8-char ID-prefix collision between same-base posts.
	if name, ok := r.tryNameLocked(blogDir, postID, base+"_"+suffix); ok {
		return name
	}
	// Escalate to the full post ID — unique per post by construction, so the
	// only way tryNameLocked can still refuse is a foreign or unreadable
	// post.json squatting at exactly base_fullID. Claim unconditionally at
	// that point rather than loop forever; the in-memory map cannot refuse
	// (no other post shares this ID) and the pathology requires a manually
	// constructed directory.
	candidate := base + "_" + postID
	if name, ok := r.tryNameLocked(blogDir, postID, candidate); ok {
		return name
	}
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

// diskOwnedBy reports whether the on-disk post.json at blogDir/name is
// either absent or owned by postID — i.e. safe for postID to claim.
//
// Returns true (safe to claim) when:
//   - the file does not exist (stat error: ENOENT, permission, etc.);
//   - the file is a regular file with corrupt/missing id (better to overwrite
//     garbage than to suffix the post forever).
//
// Returns false (refuse to claim) when:
//   - the path is not a regular file (directory / symlink / device — we have
//     no safe way to verify ownership);
//   - the file is unreadable (exists but we can't open it — refuse to clobber);
//   - the file has a non-matching id (it belongs to a different post).
//
// The Stat+IsRegular guard rejects pathological layouts (someone manually
// created a directory called post.json, or symlinked it into a sensitive
// path) without ever opening the file.
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
// every reader and writer of r.owned agrees on the exact encoding. On
// case-insensitive filesystems the name half is lowercased so two posts whose
// only difference is letter case cannot both claim the same on-disk folder.
func reservationKey(blogDir, name string) string {
	if caseFoldFS {
		name = strings.ToLower(name)
	}
	return blogDir + reserverKeySep + name
}
