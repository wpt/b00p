package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileName is the name of the state file in each blog directory.
const FileName = "_state.json"

// PostEntry records metadata about a downloaded post.
type PostEntry struct {
	Title         string `json:"title"`
	DirName       string `json:"dirName"`
	DownloadedAt  string `json:"downloadedAt"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
	CommentsCount int    `json:"commentsCount"`
	Price         int    `json:"price"`
	Tier          string `json:"tier,omitempty"`
	Locked        bool   `json:"locked,omitempty"`
	HasComments   bool   `json:"hasComments"`
	HasMd         bool   `json:"hasMd"`
}

// State tracks which posts have been downloaded for a blog.
type State struct {
	Posts    map[string]PostEntry `json:"posts"`
	LastSync string               `json:"lastSync"`
	path     string
}

// Load reads the state file from the given directory. A missing state file
// is reported as a fresh, empty state (nil error). Read or parse errors are
// returned so callers can refuse to overwrite a partially-recoverable
// `_state.json` with a freshly-initialised one — which would discard every
// previously tracked post.
//
// The Posts map is always non-nil on a successful return so callers can use
// it directly. The JSON nil-check after Unmarshal is intentional and
// documented in AGENTS.md.
func Load(dir string) (*State, error) {
	s := &State{
		Posts: make(map[string]PostEntry),
		path:  filepath.Join(dir, FileName),
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read state file %s: %w", s.path, err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", s.path, err)
	}
	if s.Posts == nil {
		s.Posts = make(map[string]PostEntry)
	}
	return s, nil
}

// Has reports whether a post ID exists in the state.
func (s *State) Has(postID string) bool {
	_, ok := s.Posts[postID]
	return ok
}

// Get returns the post entry for the given ID.
func (s *State) Get(postID string) (PostEntry, bool) {
	e, ok := s.Posts[postID]
	return e, ok
}

// Add records a downloaded post in the state.
func (s *State) Add(postID string, entry PostEntry) {
	if entry.DownloadedAt == "" {
		entry.DownloadedAt = time.Now().Format(time.RFC3339)
	}
	s.Posts[postID] = entry
}

// Save writes the state to disk atomically (write to temp file then rename),
// so an interrupted write cannot truncate the existing state file.
func (s *State) Save() error {
	s.LastSync = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, data, 0644)
}

// WriteFileAtomic writes data to path via a temp file in the same directory,
// fsyncs it, then renames over the destination. A crash mid-write leaves the
// original file untouched.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// Count returns the number of tracked posts.
func (s *State) Count() int {
	return len(s.Posts)
}
