package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/wpt/b00p/pkg/fileutil"
)

// FileName is the name of the state file in each blog directory.
const FileName = "_state.json"

// CurrentVersion is the schema version Save stamps into the state file.
// Bump it when an entry field changes meaning (not when one is merely
// added — additions unmarshal cleanly from older files). Past migrations
// (UpdatedAt backfill, Price int→float64) had to be inferred from data
// shape; an explicit version makes the next one a comparison.
const CurrentVersion = 1

// PostEntry records metadata about a downloaded post.
type PostEntry struct {
	Title         string  `json:"title"`
	DirName       string  `json:"dirName"`
	DownloadedAt  string  `json:"downloadedAt"`
	UpdatedAt     int64   `json:"updatedAt,omitempty"`
	CommentsCount int     `json:"commentsCount"`
	Price         float64 `json:"price"`
	Tier          string  `json:"tier,omitempty"`
	Locked        bool    `json:"locked,omitempty"`
	HasComments   bool    `json:"hasComments"`
	HasMd         bool    `json:"hasMd"`
	// CommentsCapped records that this post hit the comments-fetch ceiling
	// the last time it was saved. The ceiling is our choice forced by a
	// broken offset query param on Boosty's comments endpoint (offset>0
	// returns data=[] with isLast=true), so we cannot paginate past the
	// first page and we cap at commentsPageLimit-1 top-level threads or
	// defaultReplyLimit inlined replies per thread — see
	// pkg/syncer/save.go commentsPageLimit and pkg/boosty/client.go
	// defaultReplyLimit. Without this flag, classifyPost would re-fire
	// NewComments on every sync forever because disk count can never catch
	// up to API count. With it, classify skips the disk<API trigger
	// (suppression is one-directional — see pkg/syncer/classify.go).
	CommentsCapped bool `json:"commentsCapped,omitempty"`
}

// State tracks which posts have been downloaded for a blog.
//
// State is NOT safe for concurrent use; callers that mutate it from multiple
// goroutines (e.g. the --workers > 1 download pool) must serialise access
// through their own mutex. The in-memory operations (Has, Get, Add, Count)
// are plain map access and never lock on their own.
type State struct {
	// Version is the schema version of the file on disk. 0 means a legacy
	// file written before the field existed — loaded fine, upgraded to
	// CurrentVersion on the next Save.
	Version  int                  `json:"version"`
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
// it directly. The JSON nil-check after Unmarshal is intentional — a payload
// of `{"posts": null}` would otherwise leave the map nil and panic on the
// first Add.
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
	// Fail closed on files written by a NEWER b00p: a future schema may
	// carry fields this version doesn't know, and the next Save here would
	// silently drop them. Older versions (including 0 = pre-version legacy)
	// load fine.
	if s.Version > CurrentVersion {
		return nil, fmt.Errorf("state file %s has schema version %d, newer than this b00p understands (%d) — upgrade b00p",
			s.path, s.Version, CurrentVersion)
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
	s.Version = CurrentVersion
	s.LastSync = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(s.path, data, 0644)
}

// Count returns the number of tracked posts.
func (s *State) Count() int {
	return len(s.Posts)
}
