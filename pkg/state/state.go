package state

import (
	"encoding/json"
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
	LastSync string              `json:"lastSync"`
	path     string
}

// Load reads the state file from the given directory.
// Returns an empty state if the file doesn't exist or is invalid.
func Load(dir string) *State {
	s := &State{
		Posts: make(map[string]PostEntry),
		path:  filepath.Join(dir, FileName),
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		s.Posts = make(map[string]PostEntry)
		return s
	}
	if s.Posts == nil {
		s.Posts = make(map[string]PostEntry)
	}
	return s
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

// Save writes the state to disk.
func (s *State) Save() error {
	s.LastSync = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Count returns the number of tracked posts.
func (s *State) Count() int {
	return len(s.Posts)
}
