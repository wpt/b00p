package cmd

import (
	"fmt"
	"os"
	"sync"
)

// stdLogger writes diagnostic lines (Printf) and progress (Progress) to
// stderr so they don't pollute a command's stdout — e.g. `stat` prints its
// report to stdout via fmt.Println, so `b00p stat ... > out.txt` captures
// the report cleanly while `2> log.txt` captures the diagnostics.
//
// Progress (spinner / per-byte updates) is suppressed entirely when stderr
// is not a TTY: a headless run that pipes 2>&1 into a file would otherwise
// fill it with hundreds of thousands of \r-overwritten lines during a
// multi-GB video download.
//
// All methods take the mutex so concurrent workers (--workers > 1) cannot
// interleave Progress writes with Printf lines, and so hasProgress is read
// and mutated under a lock — the previous version mutated it from worker
// goroutines without synchronization.
type stdLogger struct {
	mu          sync.Mutex
	hasProgress bool
	noProgress  bool // true when stderr is not a TTY (set at init)
	initOnce    sync.Once
}

// initTTYOnce sets noProgress=true when stderr is not a character device.
// Best-effort: on Windows ConPTY etc. the stat may misreport, but the
// fallback is just "no spinner", not a functional break.
func (l *stdLogger) initTTYOnce() {
	l.initOnce.Do(func() {
		info, err := os.Stderr.Stat()
		if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
			l.noProgress = true
		}
	})
}

func (l *stdLogger) Printf(format string, args ...any) {
	l.initTTYOnce()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hasProgress {
		// Clear the in-flight progress line before printing a normal log
		// line, so the two do not visually collide.
		fmt.Fprint(os.Stderr, "\r\033[K")
		l.hasProgress = false
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func (l *stdLogger) Progress(format string, args ...any) {
	l.initTTYOnce()
	if l.noProgress {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hasProgress = true
	fmt.Fprintf(os.Stderr, "\r"+format, args...)
}

func (l *stdLogger) ClearProgress() {
	l.initTTYOnce()
	if l.noProgress {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hasProgress {
		fmt.Fprint(os.Stderr, "\r\033[K")
		l.hasProgress = false
	}
}
