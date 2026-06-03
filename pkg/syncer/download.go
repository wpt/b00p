package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

type postJob struct {
	num  int
	post boosty.Post
}

// DownloadAll fetches every post in the blog and saves any that are not yet
// in state. With Config.Force, state is ignored and every accessible post is
// re-processed (the file-layer integrity check still skips existing non-empty
// media files).
//
// State is saved per-completed post under a mutex so a mid-run crash leaves
// a consistent _state.json.
func (e *Engine) DownloadAll() error {
	c := e.c
	c.Log.Printf("Fetching all posts from %s...", e.cfg.Blog)

	blogDir := filepath.Join(e.cfg.OutputDir, e.cfg.Blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		return err
	}

	st, err := state.Load(blogDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	// state.State is not concurrency-safe (see its doc); e.stMu covers every
	// Add/Save pair below so workers do not race on st.Posts. The
	// `downloaded` counter is incremented inside the same critical section
	// — it looks like a separate race but isn't, because every worker that
	// touches it already holds e.stMu.

	var jobs []postJob
	total := 0
	skippedState := 0

	for post, err := range c.FetchPosts(e.cfg.Blog, 20) {
		if err != nil {
			if errors.Is(err, boosty.ErrFetchPage) {
				// Whole page failed — abort rather than print
				// "Done. 0 total, 0 downloaded, 0 already synced."
				// with exit 0 on a sync that did not actually list
				// anything (auth broken, blog renamed, transport).
				return fmt.Errorf("fetch posts: %w", err)
			}
			c.Log.Printf("  warning: skipping malformed post: %v", err)
			continue
		}
		total++

		if !post.HasAccess {
			c.Log.Printf("  [%d] skipping (locked): %s", total, post.Title)
			continue
		}

		if !e.cfg.Force && st.Has(post.ID) {
			skippedState++
			continue
		}

		jobs = append(jobs, postJob{num: total, post: post})
	}

	if len(jobs) == 0 {
		// Nothing new, but regenerate the index so a deleted index.md
		// self-heals on the next plain run.
		e.writeBlogIndex(blogDir, st)
		c.Log.Printf("Done. %d total, 0 new, %d already synced.", total, skippedState)
		return nil
	}

	c.Log.Printf("Found %d posts to download (workers: %d)", len(jobs), e.cfg.Workers)

	var downloaded int
	e.failedPosts.Store(0)
	runWorkerPool(e.cfg.Workers, jobs, func(job postJob) {
		c.Log.Printf("  [%d] %s", job.num, job.post.Title)
		// Refresh signed video URLs before saving so post.json and the
		// state entry both reflect the URLs we actually downloaded against.
		post := e.MaybeRefreshSignedURLs(&job.post)
		dirName, capped, err := e.SavePost(post)
		if err != nil {
			c.Log.Printf("  error: %v", err)
			e.failedPosts.Add(1)
			return
		}
		entry := e.postStateEntry(post, dirName)
		entry.CommentsCapped = capped

		e.stMu.Lock()
		defer e.stMu.Unlock()
		st.Add(post.ID, entry)
		if err := st.Save(); err != nil {
			c.Log.Printf("  warning: failed to save state: %v", err)
			e.failedPosts.Add(1)
		}
		downloaded++
	})

	e.writeBlogIndex(blogDir, st)

	failed := int(e.failedPosts.Load())
	c.Log.Printf("Done. %d total, %d downloaded, %d already synced, %d failed.", total, downloaded, skippedState, failed)
	if failed > 0 {
		return fmt.Errorf("%d post(s) failed; see log above", failed)
	}
	return nil
}
