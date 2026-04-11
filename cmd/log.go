package cmd

import "fmt"

// stdLogger prints to stdout and supports progress updates with \r.
type stdLogger struct {
	hasProgress bool
}

func (stdLogger) Printf(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func (l *stdLogger) Progress(format string, args ...interface{}) {
	l.hasProgress = true
	fmt.Printf("\r"+format, args...)
}

func (l *stdLogger) ClearProgress() {
	if l.hasProgress {
		fmt.Print("\r\033[K")
		l.hasProgress = false
	}
}
