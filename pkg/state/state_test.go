package state

import (
	"os"
	"path/filepath"
	"testing"
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
		t.Errorf("Price = %d, want 20", entry.Price)
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
		t.Errorf("loaded post-1 Price = %d, want 20", e.Price)
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
