package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
	// state.State is not concurrency-safe (see its doc); stMu covers every
	// Add/Save pair below so workers do not race on st.Posts. The
	// `downloaded` counter is incremented inside the same critical section
	// — it looks like a separate race but isn't, because every worker that
	// touches it already holds stMu.
	var stMu sync.Mutex

	var jobs []postJob
	total := 0
	skippedState := 0

	for post, err := range c.FetchPosts(e.cfg.Blog, 10) {
		if err != nil {
			return err
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
		c.Log.Printf("Done. %d total, 0 new, %d already synced.", total, skippedState)
		return nil
	}

	c.Log.Printf("Found %d posts to download (workers: %d)", len(jobs), e.cfg.Workers)

	var downloaded int
	runWorkerPool(e.cfg.Workers, jobs, func(job postJob) {
		c.Log.Printf("  [%d] %s", job.num, job.post.Title)
		dirName, err := e.SavePost(&job.post)
		if err != nil {
			c.Log.Printf("  error: %v", err)
			return
		}

		stMu.Lock()
		defer stMu.Unlock()
		st.Add(job.post.ID, e.postStateEntry(&job.post, dirName))
		if err := st.Save(); err != nil {
			c.Log.Printf("  warning: failed to save state: %v", err)
		}
		downloaded++
	})

	c.Log.Printf("Done. %d total, %d downloaded, %d already synced.", total, downloaded, skippedState)
	return nil
}
