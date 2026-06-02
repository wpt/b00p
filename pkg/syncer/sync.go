package syncer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
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
	//
	// Two error shapes from FetchPosts:
	//   - ErrFetchPage: page-level GET failed (transport, 5xx-after-retries,
	//     refresh rejected). The iterator has already terminated. Abort
	//     the sync — silently exiting 0 when posts could not even be listed
	//     is exactly the cron-failure mode this guard exists to prevent.
	//   - any other error: per-post parse failure inside a successful page;
	//     iterator continues, we log and skip just that item.
	var items []syncItem
	for post, err := range c.FetchPosts(e.cfg.Blog, 20) {
		if err != nil {
			if errors.Is(err, boosty.ErrFetchPage) {
				return fmt.Errorf("fetch posts: %w", err)
			}
			c.Log.Printf("  warning: skipping malformed post: %v", err)
			continue
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
			// Skip cases that already trigger a full re-download in apply
			// (JustUnlocked), aren't in state at all, or have no access.
			// !HasAccess covers newly locked, locked-new AND still-locked
			// posts: none of them can be repaired — the per-post endpoint
			// returns a stub that fetchFullPost rejects — so flagging
			// Missing.* on them would queue a guaranteed-failing apply on
			// every run (and a permanent non-zero exit for cron). Mirrors
			// runCheckMedia's guard in checks.go.
			if !items[i].InState || !items[i].Post.HasAccess ||
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
		// Regenerate the index even on a no-change run so a deleted
		// index.md self-heals without waiting for actual updates.
		e.writeBlogIndex(blogDir, st)
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
	// already-corrected st.Posts entries. Persist now so the backfill
	// survives even if every actionable worker fails before reaching its
	// own st.Save — otherwise legacy UpdatedAt=0 entries would silently
	// be re-backfilled on every run.
	applyBackfill(st, items)
	if err := st.Save(); err != nil {
		c.Log.Printf("  warning: failed to save backfill: %v", err)
	}

	// Phase 4: apply. Runs through the worker pool so --workers > 1 actually
	// parallelizes sync (previously only DownloadAll honored the flag). Each
	// apply helper takes short critical sections around its st.Posts read
	// and st.Add/st.Save write via e.stMu — the lock is never held across
	// HTTP downloads or file writes, so parallelism stays effective.
	c.Log.Printf("Applying...")
	e.failedPosts.Store(0)
	runWorkerPool(e.cfg.Workers, items, func(item syncItem) {
		e.applyItem(blogDir, st, item)
	})

	// Index reflects whatever state the apply phase managed to persist —
	// written even when some posts failed (their entries simply aren't in
	// state yet and show up after the retrying sync).
	e.writeBlogIndex(blogDir, st)

	if failed := e.failedPosts.Load(); failed > 0 {
		c.Log.Printf("Sync finished with %d failed post(s).", failed)
		return fmt.Errorf("%d post(s) failed; see log above", failed)
	}
	c.Log.Printf("Sync complete.")
	return nil
}
