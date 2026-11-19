package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/wpt/b00p/pkg/state"
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
	// archived maps a post ID to the untracked folder whose post.json names
	// it (see archivedDir); nil until the first lookup.
	archived map[string]string
}

func newDirReserver() *dirReserver {
	return &dirReserver{owned: make(map[string]string)}
}

// seed records every tracked directory as owned by its post before any
// worker runs. Without it a tracked folder whose post.json is missing (the
// FILES_MISSING repair window) reads as free to the disk probe, and a NEW
// post with the same formatted name claims it — adopting the tracked post's
// media as its own. Entries with an empty DirName are skipped.
func (r *dirReserver) seed(blogDir string, posts map[string]state.PostEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range posts {
		if e.DirName == "" {
			continue
		}
		key := reservationKey(blogDir, e.DirName)
		if _, taken := r.owned[key]; !taken {
			r.owned[key] = id
		}
	}
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
	// A dir named after a blog-root file b00p writes breaks it: index.md stops
	// regenerating, a _state.json dir aborts every sync at state.Load. Refuse
	// even before the file exists — post dirs are created before the first save.
	if isBlogServiceName(name) {
		return "", false
	}
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
	// A stray file at blogDir/name would read as "free" (the post.json stat
	// fails with ENOTDIR) and MkdirAll then fails every run — refuse, caller suffixes.
	if info, err := os.Stat(filepath.Join(blogDir, name)); err == nil && !info.IsDir() {
		return false
	}
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

// archivedDir returns the folder under blogDir whose post.json belongs to
// postID, among folders no state entry tracks, or "". After _state.json is
// lost a post's formatted name can differ from its folder (title edited,
// another --format or time zone); without this lookup the post is
// downloaded again into a new folder and the old one is orphaned. The first
// call reads the post.json of every untracked folder, which on a healthy
// archive is none.
func (r *dirReserver) archivedDir(blogDir, postID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.archived == nil {
		r.archived = make(map[string]string)
		// An unreadable blog dir only loses the lookup; every write into it
		// fails on its own.
		entries, err := os.ReadDir(blogDir)
		if err != nil {
			return ""
		}
		for _, d := range entries {
			if !d.IsDir() {
				continue
			}
			if _, tracked := r.owned[reservationKey(blogDir, d.Name())]; tracked {
				continue
			}
			data, err := os.ReadFile(filepath.Join(blogDir, d.Name(), "post.json"))
			if err != nil {
				continue
			}
			var p struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(data, &p) == nil && p.ID != "" {
				if _, dup := r.archived[p.ID]; !dup {
					r.archived[p.ID] = d.Name()
				}
			}
		}
	}
	return r.archived[postID]
}

// isBlogServiceName reports whether name collides with a blog-root file b00p
// writes (index.md, _state.json, .b00p.lock), folded like reservationKey on
// case-insensitive FS.
func isBlogServiceName(name string) bool {
	if caseFoldFS {
		name = strings.ToLower(name)
	}
	return name == blogIndexName || name == state.FileName || name == blogLockName
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
