package syncer

import "sync"

// runWorkerPool dispatches jobs across a bounded pool of goroutines.
//
// Worker count is clamped to [1, len(jobs)]: cfg.Workers <= 0 collapses to 1
// (a serial run is still valid), and we never spawn more workers than jobs
// because the surplus would idle on an immediately-closed channel.
//
// Each spawned worker pulls jobs from a buffered channel until drained, so
// fan-out is preserved without per-job goroutine churn. The function blocks
// until every job has returned from worker.
//
// The worker closure is responsible for any per-job state mutation. Callers
// that share mutable state across workers must synchronize externally — this
// helper only owns the pool, not the work.
//
// An empty jobs slice is a no-op: no workers, no channel sends, immediate
// return. Callers may still short-circuit empty cases earlier when they want
// to log "0 jobs" or skip side effects.
func runWorkerPool[T any](workers int, jobs []T, worker func(T)) {
	if len(jobs) == 0 {
		return
	}
	n := max(1, min(workers, len(jobs)))

	ch := make(chan T, len(jobs))
	for _, j := range jobs {
		ch <- j
	}
	close(ch)

	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for job := range ch {
				worker(job)
			}
		})
	}
	wg.Wait()
}
