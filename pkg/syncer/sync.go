package syncer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/state"
)

// Sync compares the blog's API state with on-disk state, displays a diff,
// optionally confirms with the user, then applies updates per-post. It is
// the smart-default entrypoint: cheap pagination + state diff, with optional
// expensive checks via Config.CheckMedia / Config.CheckFiles.
func (e *Engine) Sync() error {
	c := e.c
	c.Log.Printf("Syncing %s...", e.cfg.Blog)

	blogDir := filepath.Join(e.cfg.OutputDir, e.cfg.Blog)
	if err := os.MkdirAll(blogDir, 0755); err != nil {
		return err
	}

	st, err := state.Load(blogDir)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Phase 1: Fetch and classify.
	var items []syncItem
	for post, err := range c.FetchPosts(e.cfg.Blog, 20) {
		if err != nil {
			return err
		}
		items = append(items, classifyPost(post, st, blogDir, e.cfg.DirFormat))
	}

	// Phase 2a: video size check (optional).
	if e.cfg.CheckMedia {
		e.runCheckMedia(blogDir, items)
	}

	// Phase 2b: files-on-disk check (optional).
	if e.cfg.CheckFiles {
		c.Log.Printf("Checking files on disk...")
		for i := range items {
			// Skip cases that already trigger a full re-download in apply.
			if !items[i].InState ||
				items[i].JustLocked || items[i].IsLockedNew ||
				items[i].JustUnlocked {
				continue
			}
			items[i].Missing = detectMissingFiles(items[i].Existing,
				filepath.Join(blogDir, items[i].DirName))
		}
	}

	// Phase 3: display + summary.
	displaySync(c.Log, items)

	// Decide whether the apply phase has any work.
	hasActionable := false
	hasBackfill := false
	for _, item := range items {
		if item.IsActionable() {
			hasActionable = true
		}
		if item.BackfillUpdatedAt {
			hasBackfill = true
		}
	}

	if !hasActionable {
		// Persist any UpdatedAt backfills even when nothing else changed —
		// otherwise legacy entries stay at UpdatedAt=0 forever and edits
		// would be silently re-backfilled instead of detected.
		if hasBackfill {
			applyBackfill(st, items)
			if err := st.Save(); err != nil {
				c.Log.Printf("  warning: failed to save state: %v", err)
			}
		}
		c.Log.Printf("Everything up to date.")
		return nil
	}

	// Confirm. With AutoApply, skip the prompt entirely so headless runs
	// (cron, scripts, nohup pipelines) can apply without a TTY. cfg.In is
	// nil in normal CLI use and falls back to os.Stdin; tests inject a
	// fake reader to exercise this path without touching the real stdin.
	var in io.Reader = os.Stdin
	if e.cfg.In != nil {
		in = e.cfg.In
	}
	if !confirmApply(c.Log, bufio.NewReader(in), e.cfg.AutoApply) {
		c.Log.Printf("Cancelled.")
		return nil
	}

	// Apply backfills before per-item changes; per-item updates use the
	// already-corrected st.Posts entries.
	applyBackfill(st, items)

	// Phase 4: apply.
	c.Log.Printf("Applying...")
	for _, item := range items {
		e.applyItem(blogDir, st, item)
	}

	c.Log.Printf("Sync complete.")
	return nil
}
