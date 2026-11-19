package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
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
// Every line goes through termSafe: post titles, filenames and server error
// bodies come from the blog author or the network.
//
// All methods take the mutex so concurrent workers (--workers > 1) cannot
// interleave Progress writes with Printf lines, and so the progress state is
// never read or mutated from two goroutines at once.
type stdLogger struct {
	mu sync.Mutex
	// progressWidth is the width of the progress line on screen; 0 when
	// there is none.
	progressWidth int
	noProgress    bool // true when stderr is not a TTY (set at init)
	initOnce      sync.Once
	out           io.Writer // nil means os.Stderr
}

// initTTYOnce sets noProgress=true when stderr is not a character device.
// Best-effort: on Windows ConPTY etc. the stat may misreport, but the
// fallback is just "no spinner", not a functional break.
func (l *stdLogger) initTTYOnce() {
	l.initOnce.Do(func() {
		if l.out != nil {
			return
		}
		l.out = os.Stderr
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
	l.clearLocked()
	fmt.Fprintln(l.out, termSafe(fmt.Sprintf(format, args...)))
}

// Progress redraws the progress line. A shorter line is padded with spaces
// over the previous one's tail: cursor-control escapes like \033[K print as
// garbage on a Windows console without VT processing.
func (l *stdLogger) Progress(format string, args ...any) {
	l.initTTYOnce()
	if l.noProgress {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := strings.ReplaceAll(termSafe(fmt.Sprintf(format, args...)), "\n", " ")
	width := utf8.RuneCountInString(line)
	fmt.Fprint(l.out, "\r"+line+strings.Repeat(" ", max(l.progressWidth-width, 0)))
	l.progressWidth = max(width, 1)
}

func (l *stdLogger) ClearProgress() {
	l.initTTYOnce()
	if l.noProgress {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearLocked()
}

// clearLocked blanks the progress line, if any. Caller holds mu.
func (l *stdLogger) clearLocked() {
	if l.progressWidth > 0 {
		fmt.Fprint(l.out, "\r"+strings.Repeat(" ", l.progressWidth)+"\r")
		l.progressWidth = 0
	}
}

// termSafe drops control characters except tab and newline. A post title or
// a server error body with ESC, BEL or C1 bytes could otherwise drive the
// terminal: retitle the window, rewrite earlier lines, plant OSC 8 links.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' && r != '\n' {
			return -1
		}
		return r
	}, s)
}
