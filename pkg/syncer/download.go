package syncer

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/wpt/b00p/pkg/boosty"
)

type postJob struct {
	num  int
	post boosty.Post
}

// DownloadAll fetches every post in the blog and saves any that are not yet
// in state, plus tracked posts whose entry is Locked but which are accessible
// again. With Config.Force, state is ignored and every accessible post is
// re-processed (see Config.Force for what is still skipped).
//
// State is saved per-completed post under a mutex so a mid-run crash leaves
// a consistent _state.json.
func (e *Engine) DownloadAll() error {
	c := e.c
	c.Log.Printf("Fetching all posts from %s...", e.cfg.Blog)

	blogDir, st, err := e.loadState()
	if err != nil {
		return err
	}

	var jobs []postJob
	total := 0
	skippedState := 0

	for post, err := range c.FetchPosts(e.cfg.Blog, boosty.DefaultPageLimit) {
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

		if existing, ok := st.Get(post.ID); ok && !existing.Locked && !e.cfg.Force {
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

	var downloaded atomic.Int64
	e.failedPosts.Store(0)
	runWorkerPool(e.cfg.Workers, jobs, func(job postJob) {
		c.Log.Printf("  [%d] %s", job.num, job.post.Title)
		if e.saveNewPost(st, &job.post) {
			downloaded.Add(1)
		}
	})

	e.writeBlogIndex(blogDir, st)

	failed := int(e.failedPosts.Load())
	c.Log.Printf("Done. %d total, %d downloaded, %d already synced, %d failed.", total, downloaded.Load(), skippedState, failed)
	if failed > 0 {
		return fmt.Errorf("%d post(s) failed; see log above", failed)
	}
	return nil
}
