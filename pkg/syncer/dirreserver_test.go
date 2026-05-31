package syncer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// dirReserver is the only thing standing between two concurrent workers and
// a silent overwrite of each other's post directory. The collision rules
// (same postID idempotent; different postID gets a suffix; on-disk post.json
// disambiguates by id) are subtle enough that the integration tests can't be
// trusted to exercise every cell — these unit tests pin them down.

func TestDirReserver_FreeBaseReturnsBase(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	if got := r.reserve(dir, "post1", "my-post"); got != "my-post" {
		t.Errorf("reserve = %q, want 'my-post'", got)
	}
}

func TestDirReserver_SamePostIDIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	a := r.reserve(dir, "post1", "my-post")
	b := r.reserve(dir, "post1", "my-post")
	if a != b || a != "my-post" {
		t.Errorf("two reserve() for same post returned %q, %q (want both 'my-post')", a, b)
	}
}

func TestDirReserver_InFlightCollisionGetsSuffix(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	first := r.reserve(dir, "abcdef1234", "shared")
	second := r.reserve(dir, "ffffeeee99", "shared")

	if first != "shared" {
		t.Errorf("first reserve = %q, want 'shared'", first)
	}
	want := "shared_ffffeeee" // first 8 chars of second post's ID
	if second != want {
		t.Errorf("second reserve = %q, want %q", second, want)
	}
}

func TestDirReserver_ShortPostIDNotTruncated(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	r.reserve(dir, "owner", "shared")
	if got := r.reserve(dir, "p2", "shared"); got != "shared_p2" {
		t.Errorf("short ID suffix = %q, want 'shared_p2'", got)
	}
}

// On-disk post.json is the source of truth for "who already owns this dir":
// if the id in there matches us, we keep the base name (so a re-run of the
// same blog reattaches to its own state instead of suffixing every dir).
func TestDirReserver_DiskOwnedByUsReturnsBase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "post.json"), []byte(`{"id":"post1"}`), 0644); err != nil {
		t.Fatal(err)
	}

	r := newDirReserver()
	if got := r.reserve(dir, "post1", "shared"); got != "shared" {
		t.Errorf("reserve = %q, want 'shared' (we own the on-disk dir)", got)
	}
}

func TestDirReserver_DiskOwnedByOtherGetsSuffix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "post.json"), []byte(`{"id":"otherID"}`), 0644); err != nil {
		t.Fatal(err)
	}

	r := newDirReserver()
	if got := r.reserve(dir, "abcdef1234", "shared"); got != "shared_abcdef12" {
		t.Errorf("reserve = %q, want 'shared_abcdef12' (disk owned by other)", got)
	}
}

func TestDirReserver_DiskCorruptPostJSONClaimedByUs(t *testing.T) {
	// A corrupt or id-less post.json is treated as ours; better to overwrite
	// garbage than to suffix-and-leave-it.
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "post.json"), []byte(`{not json`), 0644); err != nil {
		t.Fatal(err)
	}

	r := newDirReserver()
	if got := r.reserve(dir, "post1", "shared"); got != "shared" {
		t.Errorf("reserve = %q, want 'shared' (corrupt post.json treated as ours)", got)
	}
}

func TestDirReserver_DiskNoIDFieldClaimedByUs(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "post.json"), []byte(`{"title":"x"}`), 0644); err != nil {
		t.Fatal(err)
	}

	r := newDirReserver()
	if got := r.reserve(dir, "post1", "shared"); got != "shared" {
		t.Errorf("reserve = %q, want 'shared' (post.json without id treated as ours)", got)
	}
}

// A post.json that is somehow a directory (or other non-regular type) must
// NOT be treated as "we already own this" — we have no way to verify the id,
// and overwriting could destroy unrelated state. The reserver must hand back
// a suffix instead. Regression guard against the type-confusion failure mode
// (ReadFile would also fail on a directory, but Stat+IsRegular makes the
// reasoning explicit at the source).
func TestDirReserver_DiskPostJSONIsDirectoryGetsSuffix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared")
	if err := os.MkdirAll(filepath.Join(target, "post.json"), 0755); err != nil {
		t.Fatal(err)
	}

	r := newDirReserver()
	got := r.reserve(dir, "abcdef1234", "shared")
	if got == "shared" {
		t.Errorf("reserve = %q, want a suffixed name (post.json is a directory, not a file)", got)
	}
}

// Two workers simultaneously reserving the same base name for different
// posts must produce two distinct names — anything else is a silent
// directory-stomping bug.
//
// The earlier version of this test relied on goroutine scheduling for
// concurrency, which could pass even if reserve() were not actually
// thread-safe. We now force lockstep entry via a start barrier: both
// goroutines block on `start.Wait()` (with one Done from each goroutine
// itself, after the main loop has Add'd both), then race into reserve()
// at the same instant. The harness then verifies (a) both got back a
// name, (b) the names are distinct, and (c) exactly one of them is the
// bare base — i.e. one worker won the race and the other got suffixed.
func TestDirReserver_ConcurrentDifferentIDsProduceUniqueNames(t *testing.T) {
	dir := t.TempDir()

	const iterations = 50 // amplify the race window so a regression is hard to miss
	for iter := range iterations {
		r := newDirReserver()
		var start sync.WaitGroup
		start.Add(1)

		var wg sync.WaitGroup
		names := make([]string, 2)
		ids := []string{"aaaaaaaa11", "bbbbbbbb22"}
		for idx, id := range ids {
			wg.Go(func() {
				start.Wait() // block until released
				names[idx] = r.reserve(dir, id, "shared")
			})
		}
		// All goroutines are now parked at start.Wait. Release them.
		start.Done()
		wg.Wait()

		if names[0] == "" || names[1] == "" {
			t.Fatalf("iteration %d: a goroutine never set its name: %v", iter, names)
		}
		if names[0] == names[1] {
			t.Fatalf("iteration %d: concurrent reserve produced same name twice: %q", iter, names[0])
		}
		bareCount := 0
		for _, n := range names {
			if n == "shared" {
				bareCount++
			}
		}
		if bareCount != 1 {
			t.Errorf("iteration %d: expected exactly one bare 'shared' winner, got %d (names=%v)", iter, bareCount, names)
		}
	}
}

// Sanity check on the key separator choice: the chosen separator must never
// appear in a valid blog dir / name pair. NUL is the right answer on every
// supported OS — encoding any concrete value here would also catch an
// accidental "let's make this a normal character" refactor.
func TestDirReserver_KeySeparatorIsNUL(t *testing.T) {
	if reserverKeySep != "\x00" {
		t.Errorf("reserverKeySep = %q, want NUL (\\x00); NUL is the only byte forbidden in path components on all supported OSes", reserverKeySep)
	}
}

// On case-insensitive filesystems (NTFS, APFS) the reservation key is folded:
// two posts whose sanitized titles differ only in letter case would otherwise
// both pass the in-memory check independently while MkdirAll quietly reopens
// the same on-disk folder — the second post's writes clobber the first.
// caseFoldFS is a package var (not a build tag) precisely so this data-loss
// guard is pinned on every OS; the package has no t.Parallel, so swapping it
// with a Cleanup restore is safe.
func TestDirReserver_CaseFoldCollisionGetsSuffix(t *testing.T) {
	saved := caseFoldFS
	caseFoldFS = true
	t.Cleanup(func() { caseFoldFS = saved })

	dir := t.TempDir()
	r := newDirReserver()

	first := r.reserve(dir, "aaaaaaaa11", "Стрим")
	second := r.reserve(dir, "bbbbbbbb22", "стрим")

	if first != "Стрим" {
		t.Errorf("first reserve = %q, want 'Стрим'", first)
	}
	if second != "стрим_bbbbbbbb" {
		t.Errorf("second reserve = %q, want 'стрим_bbbbbbbb' (case-only collision must suffix on a folding FS)", second)
	}
}

func TestDirReserver_CaseSensitiveFSKeepsBothNames(t *testing.T) {
	saved := caseFoldFS
	caseFoldFS = false
	t.Cleanup(func() { caseFoldFS = saved })

	dir := t.TempDir()
	r := newDirReserver()

	r.reserve(dir, "aaaaaaaa11", "Стрим")
	if got := r.reserve(dir, "bbbbbbbb22", "стрим"); got != "стрим" {
		t.Errorf("reserve = %q, want 'стрим' (case-sensitive FS: distinct names, no suffix)", got)
	}
}

// The suffixed fallback candidate goes through the same memory+disk
// validation as the base name. When both the base AND base_idprefix8 are
// owned by other posts (8-char ID-prefix collision between same-base posts),
// the reserver escalates to the full post ID instead of silently overwriting
// the sibling's reservation.
func TestDirReserver_SuffixCollisionEscalatesToFullID(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	r.reserve(dir, "aaaaaaaa11", "shared")          // owns the base name
	r.reserve(dir, "bbbbbbbb22", "shared_cccccccc") // squats the next post's suffix candidate

	if got := r.reserve(dir, "cccccccc33", "shared"); got != "shared_cccccccc33" {
		t.Errorf("reserve = %q, want 'shared_cccccccc33' (suffixed candidate taken → full-ID suffix)", got)
	}
	// The squatter's reservation must survive untouched.
	if again := r.reserve(dir, "bbbbbbbb22", "shared_cccccccc"); again != "shared_cccccccc" {
		t.Errorf("squatter lost its reservation: got %q, want 'shared_cccccccc'", again)
	}
}
