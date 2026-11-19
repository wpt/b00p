package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// Author-controlled text must not reach the terminal as control sequences.
func TestTermSafe(t *testing.T) {
	in := "a\x1b]0;pwned\x07b\x1b[2Jc\u009b31md\te\nж"
	if got, want := termSafe(in), "a]0;pwnedb[2Jc31md\te\nж"; got != want {
		t.Errorf("termSafe = %q, want %q", got, want)
	}
}

// A shorter progress redraw must blank the previous line's tail, and a log
// line must blank the progress line before it prints.
func TestStdLogger_ProgressPadsAndClears(t *testing.T) {
	var buf bytes.Buffer
	l := &stdLogger{out: &buf}
	l.Progress("downloading video_001.mp4 10.5 MB")
	l.Progress("downloading x 1 KB")
	if got := buf.String(); !strings.HasSuffix(got, "\rdownloading x 1 KB"+strings.Repeat(" ", 15)) {
		t.Errorf("second redraw = %q, want the shorter line padded over the old tail", got)
	}
	buf.Reset()
	l.Printf("done \x1b[31m%s", "ok")
	if got, want := buf.String(), "\r"+strings.Repeat(" ", 18)+"\rdone [31mok\n"; got != want {
		t.Errorf("Printf after progress = %q, want %q", got, want)
	}
}
