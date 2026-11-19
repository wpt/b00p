package syncer

import (
	"bufio"
	"strings"
	"testing"
)

// --- confirmApply: --yes auto-confirm vs interactive prompt ---
//
// Regression for the "headless --sync" gap: a nohup'd `b00p download --sync`
// over ssh hangs forever on the "Apply changes? [y/N]" prompt because
// background jobs without a TTY can't read stdin. With --yes, the prompt
// is skipped entirely.

func TestConfirmApply_AutoYesSkipsPrompt(t *testing.T) {
	log := &recordingLogger{}
	// Empty stdin — would block forever on ReadString in interactive mode.
	in := bufio.NewReader(strings.NewReader(""))

	if !confirmed(log, in, true) {
		t.Error("confirmApply(auto=true) = false, want true")
	}
	if !strings.Contains(log.joined(), "Auto-applying") {
		t.Errorf("expected 'Auto-applying' log line, got %q", log.joined())
	}
}

func TestConfirmApply_TypedY(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("y\n"))

	if !confirmed(log, in, false) {
		t.Error("confirmApply(input='y') = false, want true")
	}
}

func TestConfirmApply_TypedYes(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("YES\n"))

	if !confirmed(log, in, false) {
		t.Error("confirmApply(input='YES') = false, want true (case-insensitive)")
	}
}

func TestConfirmApply_TypedN(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("n\n"))

	if confirmed(log, in, false) {
		t.Error("confirmApply(input='n') = true, want false")
	}
}

func TestConfirmApply_EmptyInputDefaultsToNo(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("\n"))

	if confirmed(log, in, false) {
		t.Error("confirmApply(input='') = true, want false (default no)")
	}
}

// `printf y | b00p ... --sync` delivers the answer together with io.EOF and
// no trailing newline; the bytes read decide, not the EOF.
func TestConfirmApply_NoTrailingNewline(t *testing.T) {
	log := &recordingLogger{}
	if !confirmed(log, bufio.NewReader(strings.NewReader("y")), false) {
		t.Errorf("confirmApply(input='y' without newline) = false, want true; log=%q", log.joined())
	}
	if confirmed(log, bufio.NewReader(strings.NewReader("n")), false) {
		t.Error("confirmApply(input='n' without newline) = true, want false")
	}
}

func TestConfirmApply_EOFReturnsFalse(t *testing.T) {
	log := &recordingLogger{}
	// Empty reader → EOF immediately. Caller treats as cancellation.
	in := bufio.NewReader(strings.NewReader(""))

	if ok, err := confirmApply(log, in, false); ok || err == nil {
		t.Errorf("confirmApply(EOF) = %v, %v; want false and an error (a headless run must not exit 0 as if cancelled)", ok, err)
	}
	if !strings.Contains(log.joined(), "failed to read confirmation") {
		t.Errorf("expected EOF warning in log, got %q", log.joined())
	}
}

// confirmed is confirmApply's answer for tests that only feed it input.
func confirmed(log *recordingLogger, in *bufio.Reader, auto bool) bool {
	ok, _ := confirmApply(log, in, auto)
	return ok
}
