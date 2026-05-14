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

// Two workers simultaneously reserving the same base name for different
// posts must produce two distinct names — anything else is a silent
// directory-stomping bug.
func TestDirReserver_ConcurrentDifferentIDsProduceUniqueNames(t *testing.T) {
	dir := t.TempDir()
	r := newDirReserver()

	var wg sync.WaitGroup
	names := make([]string, 2)
	ids := []string{"aaaaaaaa11", "bbbbbbbb22"}
	for i, id := range ids {
		wg.Go(func() {
			names[i] = r.reserve(dir, id, "shared")
		})
	}
	wg.Wait()

	if names[0] == names[1] {
		t.Fatalf("concurrent reserve produced same name twice: %q", names[0])
	}
	bareCount := 0
	for _, n := range names {
		if n == "shared" {
			bareCount++
		}
	}
	if bareCount != 1 {
		t.Errorf("expected exactly one bare 'shared' winner, got %d (names=%v)", bareCount, names)
	}
}
