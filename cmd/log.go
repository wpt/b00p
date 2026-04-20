package cmd

import (
	"fmt"
	"sync"
)

// stdLogger prints to stdout and supports progress updates with \r.
//
// All methods take the mutex so concurrent workers (--workers > 1) cannot
// interleave Progress writes with Printf lines, and so hasProgress is read
// and mutated under a lock — the previous version mutated it from worker
// goroutines without synchronization.
type stdLogger struct {
	mu          sync.Mutex
	hasProgress bool
}

func (l *stdLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hasProgress {
		// Clear the in-flight progress line before printing a normal log
		// line, so the two do not visually collide.
		fmt.Print("\r\033[K")
		l.hasProgress = false
	}
	fmt.Printf(format+"\n", args...)
}

func (l *stdLogger) Progress(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hasProgress = true
	fmt.Printf("\r"+format, args...)
}

func (l *stdLogger) ClearProgress() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hasProgress {
		fmt.Print("\r\033[K")
		l.hasProgress = false
	}
}
