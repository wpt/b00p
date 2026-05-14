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

	if !confirmApply(log, in, true) {
		t.Error("confirmApply(auto=true) = false, want true")
	}
	if !strings.Contains(log.joined(), "Auto-applying") {
		t.Errorf("expected 'Auto-applying' log line, got %q", log.joined())
	}
}

func TestConfirmApply_TypedY(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("y\n"))

	if !confirmApply(log, in, false) {
		t.Error("confirmApply(input='y') = false, want true")
	}
}

func TestConfirmApply_TypedYes(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("YES\n"))

	if !confirmApply(log, in, false) {
		t.Error("confirmApply(input='YES') = false, want true (case-insensitive)")
	}
}

func TestConfirmApply_TypedN(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("n\n"))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(input='n') = true, want false")
	}
}

func TestConfirmApply_EmptyInputDefaultsToNo(t *testing.T) {
	log := &recordingLogger{}
	in := bufio.NewReader(strings.NewReader("\n"))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(input='') = true, want false (default no)")
	}
}

func TestConfirmApply_EOFReturnsFalse(t *testing.T) {
	log := &recordingLogger{}
	// Empty reader → EOF immediately. Caller treats as cancellation.
	in := bufio.NewReader(strings.NewReader(""))

	if confirmApply(log, in, false) {
		t.Error("confirmApply(EOF) = true, want false")
	}
	if !strings.Contains(log.joined(), "failed to read confirmation") {
		t.Errorf("expected EOF warning in log, got %q", log.joined())
	}
}
