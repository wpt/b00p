package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/downloader"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// commentsPageLimit is the per-page limit for the comments listing endpoint.
// Boosty's offset query param is effectively ignored on the comments endpoint
// (offset>0 returns data=[] with isLast=true, so paginated fetching never
// advances past the first page), but the server honors limit values up to
// ~200 in a single call. 100 mirrors defaultReplyLimit and covers every post
// in observed blogs; posts with >100 top-level comments would silently cap
// here and surface on the next sync as a disk-vs-API mismatch via
// diskCommentCount, re-triggering a refetch (which still wouldn't help —
// real cursor pagination would be needed for >100 top-level threads).
const commentsPageLimit = 100

// SavePost downloads a post's full content into the engine's output directory.
// Returns the directory name actually used (which may include a collision
// suffix) so the caller can record it in state.
//
// A non-nil error means at least one required artifact (post.json, media,
// post.md when WithMD, comments.json when WithComments) could not be written
// or downloaded. The caller MUST NOT record the post as downloaded in state
// on error — that is what makes the next sync re-attempt the failed pieces
// instead of silently leaving stale/missing files behind.
//
// External video failures are NOT fatal: DownloadExternal is opt-in and
// depends on third-party sites that fail in routine ways (geo-blocks, age
// gates, dead links). They are logged and ignored for the state contract.
func (e *Engine) SavePost(post *boosty.Post) (string, error) {
	if !post.HasAccess {
		e.c.Log.Printf("  skipping (no access): %s", post.Title)
		return "", nil
	}

	blogDir := filepath.Join(e.cfg.OutputDir, e.cfg.Blog)
	dirName := parser.FormatDirName(e.cfg.DirFormat, post.Title, post.PublishTime, post.ID)
	dirName = e.res.reserve(blogDir, post.ID, dirName)
	dir := filepath.Join(blogDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	if err := writeJSON(filepath.Join(dir, "post.json"), post); err != nil {
		return "", err
	}
	e.c.Log.Printf("  saved post.json: %s", post.Title)

	parsed := parser.ParseBlocks(post.Data)

	// Download media. Errors are joined and returned so the caller can refuse
	// to mark the post as downloaded in state.
	var errs []error
	if err := downloader.DownloadMedia(e.c, parsed.Media, dir); err != nil {
		errs = append(errs, fmt.Errorf("media: %w", err))
	}

	// External videos: opt-in and best-effort, only logged.
	if e.cfg.DownloadExternal {
		if err := downloader.DownloadExternal(e.c.Log, parsed.Media, dir); err != nil {
			e.c.Log.Printf("  warning: external download error: %v", err)
		}
	}

	if e.cfg.WithMD {
		if err := writePostMarkdown(post, parsed, dir); err != nil {
			errs = append(errs, fmt.Errorf("post.md: %w", err))
		} else {
			e.c.Log.Printf("  saved post.md")
		}
	}

	if e.cfg.WithComments {
		if err := e.downloadComments(post.ID, dir); err != nil {
			errs = append(errs, fmt.Errorf("comments: %w", err))
		}
	}

	if len(errs) > 0 {
		return dirName, errors.Join(errs...)
	}
	return dirName, nil
}

func (e *Engine) downloadComments(postID, dir string) error {
	var allComments []boosty.Comment
	for comment, err := range e.c.FetchComments(e.cfg.Blog, postID, commentsPageLimit) {
		if err != nil {
			return err
		}
		allComments = append(allComments, comment)
	}

	if err := writeJSON(filepath.Join(dir, "comments.json"), allComments); err != nil {
		return err
	}
	e.c.Log.Printf("  saved comments.json (%d comments)", len(allComments))
	return nil
}

// postStateEntry builds a state.PostEntry from a post for the initial
// (NEW / JustUnlocked) save path. HasComments / HasMd reflect the engine's
// current Config flags. The Edited / VideoMismatch / Missing apply path does
// NOT use this helper — it patches an existing entry in place so that failed
// writes do not advance UpdatedAt / CommentsCount past disk reality.
func (e *Engine) postStateEntry(post *boosty.Post, dirName string) state.PostEntry {
	tier := ""
	if post.SubscriptionLevel != nil {
		tier = post.SubscriptionLevel.Name
	}
	return state.PostEntry{
		Title:         post.Title,
		DirName:       dirName,
		UpdatedAt:     post.UpdatedAt,
		CommentsCount: post.Count.Comments,
		Price:         post.Price,
		Tier:          tier,
		HasComments:   e.cfg.WithComments,
		HasMd:         e.cfg.WithMD,
	}
}

// writeJSON marshals v with indent and writes it to path (0644) atomically.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	return state.WriteFileAtomic(path, data, 0644)
}

// writePostMarkdown generates markdown for a post and writes it to dir/post.md
// atomically. Returns an error so callers can avoid persisting HasMd=true on
// failure.
func writePostMarkdown(post *boosty.Post, parsed parser.ParsedContent, dir string) error {
	md := parser.GenerateMarkdown(post, parsed)
	return state.WriteFileAtomic(filepath.Join(dir, "post.md"), []byte(md), 0644)
}

// dirReserver tracks which directory names are in flight or already owned by
// a given post ID, so two concurrent workers cannot pick the same target dir
// for posts whose formatted names collide. A pure-filesystem check would race
// between two workers that have not yet written post.json.
//
// A reservation is keyed by absolute blog dir + base name. Once owned by a
// post ID it is never released — even on failure — so a second post that
// would have collided is forced to a suffix instead of clobbering partial
// data on disk.
type dirReserver struct {
	mu    sync.Mutex
	owned map[string]string // key = blogDir + "\x00" + name → postID
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

	if name, ok := r.tryName(blogDir, postID, base); ok {
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
	r.owned[blogDir+"\x00"+candidate] = postID
	return candidate
}

// tryName reports whether `name` can be used by postID. It returns the name
// when the in-process map either has no owner, or already names this post;
// or when the filesystem has no post.json or has one belonging to this post.
// The reservation is recorded on success.
func (r *dirReserver) tryName(blogDir, postID, name string) (string, bool) {
	key := blogDir + "\x00" + name
	if owner, ok := r.owned[key]; ok {
		if owner == postID {
			return name, true
		}
		return "", false
	}
	target := filepath.Join(blogDir, name)
	data, err := os.ReadFile(filepath.Join(target, "post.json"))
	if err != nil {
		r.owned[key] = postID
		return name, true
	}
	var existing struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &existing); err != nil || existing.ID == "" || existing.ID == postID {
		r.owned[key] = postID
		return name, true
	}
	return "", false
}
