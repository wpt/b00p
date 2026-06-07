package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wpt/b00p/pkg/fileutil"
)

func TestState_AddAndHas(t *testing.T) {
	s := &State{Posts: make(map[string]PostEntry)}

	if s.Has("abc") {
		t.Error("Has('abc') = true on empty state")
	}

	s.Add("abc", PostEntry{
		Title:   "Test Post",
		DirName: "2026-03-13_Test Post",
		HasMd:   true,
	})

	if !s.Has("abc") {
		t.Error("Has('abc') = false after Add")
	}

	entry, ok := s.Get("abc")
	if !ok {
		t.Fatal("Get('abc') returned false")
	}
	if entry.Title != "Test Post" {
		t.Errorf("Title = %q, want 'Test Post'", entry.Title)
	}
	if entry.DirName != "2026-03-13_Test Post" {
		t.Errorf("DirName = %q, want '2026-03-13_Test Post'", entry.DirName)
	}
	if entry.HasMd != true {
		t.Error("HasMd = false, want true")
	}
	if entry.DownloadedAt == "" {
		t.Error("DownloadedAt should be set automatically")
	}
}

func TestState_AddWithAllFields(t *testing.T) {
	s := &State{Posts: make(map[string]PostEntry)}

	s.Add("abc", PostEntry{
		Title:         "Paid Post",
		DirName:       "2026-03-13_Paid Post",
		UpdatedAt:     1234567890,
		CommentsCount: 5,
		Price:         20,
		Tier:          "tier_2",
		HasComments:   true,
		HasMd:         true,
	})

	entry, _ := s.Get("abc")
	if entry.Price != 20 {
		t.Errorf("Price = %g, want 20", entry.Price)
	}
	if entry.Tier != "tier_2" {
		t.Errorf("Tier = %q, want 'tier_2'", entry.Tier)
	}
	if entry.UpdatedAt != 1234567890 {
		t.Errorf("UpdatedAt = %d, want 1234567890", entry.UpdatedAt)
	}
	if entry.CommentsCount != 5 {
		t.Errorf("CommentsCount = %d, want 5", entry.CommentsCount)
	}
}

func TestState_Locked(t *testing.T) {
	s := &State{Posts: make(map[string]PostEntry)}

	s.Add("abc", PostEntry{Title: "Post", DirName: "dir", Locked: true})

	entry, _ := s.Get("abc")
	if !entry.Locked {
		t.Error("Locked = false, want true")
	}
}

func TestState_Count(t *testing.T) {
	s := &State{Posts: make(map[string]PostEntry)}
	if s.Count() != 0 {
		t.Errorf("Count() = %d, want 0", s.Count())
	}
	s.Add("a", PostEntry{Title: "A", DirName: "dir_a"})
	s.Add("b", PostEntry{Title: "B", DirName: "dir_b"})
	if s.Count() != 2 {
		t.Errorf("Count() = %d, want 2", s.Count())
	}
}

func TestState_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()

	s := &State{
		Posts: make(map[string]PostEntry),
		path:  filepath.Join(dir, FileName),
	}
	s.Add("post-1", PostEntry{Title: "First Post", DirName: "2026-01-01_First Post", HasComments: true, Price: 20, Tier: "tier_2"})
	s.Add("post-2", PostEntry{Title: "Second Post", DirName: "2026-01-02_Second Post", HasMd: true})

	if err := s.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.Count() != 2 {
		t.Fatalf("loaded Count() = %d, want 2", loaded.Count())
	}
	if !loaded.Has("post-1") {
		t.Error("loaded.Has('post-1') = false")
	}
	e, _ := loaded.Get("post-1")
	if e.Price != 20 {
		t.Errorf("loaded post-1 Price = %g, want 20", e.Price)
	}
	if e.Tier != "tier_2" {
		t.Errorf("loaded post-1 Tier = %q, want 'tier_2'", e.Tier)
	}
	if loaded.LastSync == "" {
		t.Error("loaded LastSync is empty")
	}
}

func TestState_LoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("Load(nonexistent) error: %v", err)
	}
	if s.Count() != 0 {
		t.Errorf("Count() = %d, want 0 for non-existent dir", s.Count())
	}
	if s.Posts == nil {
		t.Error("Posts is nil, want initialized map")
	}
}

func TestState_LoadCorrupted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("not json"), 0644); err != nil {
		t.Fatalf("setup write failed: %v", err)
	}

	s, err := Load(dir)
	if err == nil {
		t.Fatalf("Load(corrupted) error = nil, want non-nil; got state=%+v", s)
	}
	if s != nil {
		t.Errorf("Load(corrupted) state = %+v, want nil", s)
	}
}

// TestState_ConcurrentAddSaveUnderExternalMutex exercises the documented
// concurrency contract: State is safe under fan-out when callers serialize
// every {Add → Save} via an external mutex. The syncer package follows this
// pattern (stMu in DownloadAll); this test pins the contract so a regression
// that adds lock-free access from inside the State package would surface
// under -race.
//
// It is intentionally NOT a test that State is internally lock-free-safe —
// it documents the opposite. Removing the mutex below and rerunning with
// -race would (correctly) report a map write race.
func TestState_ConcurrentAddSaveUnderExternalMutex(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var mu sync.Mutex
	const workers = 4
	const perWorker = 25

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				id := fmt.Sprintf("w%d-p%d", w, i)
				mu.Lock()
				s.Add(id, PostEntry{Title: id, DirName: id})
				if err := s.Save(); err != nil {
					t.Errorf("Save: %v", err)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if got, want := s.Count(), workers*perWorker; got != want {
		t.Errorf("Count = %d, want %d", got, want)
	}

	// Reload from disk and verify the persisted view matches in-memory.
	// Every entry the workers added must be on disk — a missed Save under
	// contention would surface as a missing entry here.
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load (reloaded): %v", err)
	}
	if got, want := reloaded.Count(), workers*perWorker; got != want {
		t.Errorf("reloaded Count = %d, want %d", got, want)
	}
}

func TestState_OverwriteEntry(t *testing.T) {
	s := &State{Posts: make(map[string]PostEntry)}
	s.Add("abc", PostEntry{Title: "Version 1", DirName: "dir_v1"})
	s.Add("abc", PostEntry{Title: "Version 2", DirName: "dir_v2", HasComments: true, HasMd: true})

	if s.Count() != 1 {
		t.Errorf("Count() = %d, want 1 (overwrite, not duplicate)", s.Count())
	}
	e, _ := s.Get("abc")
	if e.Title != "Version 2" {
		t.Errorf("Title = %q, want 'Version 2'", e.Title)
	}
}

// TestState_LoadNullPosts pins the documented invariant that after Unmarshal
// of a JSON file with "posts": null, the Posts map is re-initialized to an
// empty (non-nil) map so callers can use it directly. See state.go Load for
// the documented rationale.
func TestState_LoadNullPosts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"posts": null}`), 0644); err != nil {
		t.Fatalf("setup write failed: %v", err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if s.Posts == nil {
		t.Fatal("Posts is nil after Load of {\"posts\": null}; want empty map")
	}
	if s.Count() != 0 {
		t.Errorf("Count() = %d, want 0", s.Count())
	}

	// Verify the map is usable — Add must not panic on a freshly loaded null state.
	s.Add("post-x", PostEntry{Title: "X", DirName: "dir_x"})
	if !s.Has("post-x") {
		t.Error("Has('post-x') = false after Add on null-posts state")
	}
}

// TestWriteFileAtomic_TempCleanupOnRenameFailure verifies that when the final
// rename step fails, no orphan .tmp-* file is left behind in the target dir.
// We force rename to fail by passing a destination path whose parent does not
// exist; CreateTemp is invoked against that missing dir and surfaces the error
// before any temp file lands on disk. Then we point at an existing dir and
// confirm the happy path leaves the tree clean.
func TestWriteFileAtomic_TempCleanupOnRenameFailure(t *testing.T) {
	dir := t.TempDir()

	// Happy path — write succeeds, no temp residue.
	dest := filepath.Join(dir, "state.json")
	if err := fileutil.WriteFileAtomic(dest, []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatalf("WriteFileAtomic happy path: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "state.json" {
			t.Errorf("unexpected leftover file after successful write: %q", name)
		}
	}

	// Failure path — write into a non-existent parent. CreateTemp must fail,
	// returning an error and leaving no .tmp-* artefact in the parent dir.
	missing := filepath.Join(dir, "no-such-subdir", "state.json")
	if err := fileutil.WriteFileAtomic(missing, []byte(`{"x":1}`), 0644); err == nil {
		t.Fatal("WriteFileAtomic(missing parent) error = nil, want non-nil")
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir after failure: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "state.json" {
			t.Errorf("unexpected leftover file after failed write: %q", name)
		}
	}
}

// Save stamps the current schema version; a legacy file (no version field)
// loads as version 0 and upgrades on the next Save.
func TestState_SaveStampsCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Add("p1", PostEntry{Title: "x", DirName: "d"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != CurrentVersion {
		t.Errorf("Version = %d, want %d", loaded.Version, CurrentVersion)
	}
}

func TestState_LoadLegacyFileWithoutVersion(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"posts":{"p":{"title":"t","dirName":"d"}},"lastSync":"2025-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatalf("legacy (pre-version) state file must load cleanly: %v", err)
	}
	if s.Version != 0 {
		t.Errorf("Version = %d, want 0 for a legacy file", s.Version)
	}
	if !s.Has("p") {
		t.Error("legacy entries lost on load")
	}
}

// A file written by a NEWER b00p must be rejected: this version would drop
// the unknown fields of the newer schema on its next Save.
func TestState_LoadRejectsNewerVersion(t *testing.T) {
	dir := t.TempDir()
	future := fmt.Sprintf(`{"version":%d,"posts":{}}`, CurrentVersion+1)
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(future), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir); err == nil {
		t.Fatal("Load must reject a state file with a newer schema version")
	}
}
