//go:build unix

package syncer

import "testing"

// shortenLockWait is a no-op on Unix: flock reports a held lock at once.
func shortenLockWait(t *testing.T) { t.Helper() }
