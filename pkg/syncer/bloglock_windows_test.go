package syncer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortenLockWait makes a held lock fail fast in tests that expect it held.
func shortenLockWait(t *testing.T) {
	t.Helper()
	saved := lockOpenWait
	lockOpenWait = 0
	t.Cleanup(func() { lockOpenWait = saved })
}

// A third-party handle on .b00p.lock (indexer, sync client, AV scanner) is
// not another b00p run: lockFile waits it out instead of failing.
func TestLockFile_WaitsOutForeignHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), blogLockName)
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		closed <- reader.Close()
	}()

	f, err := lockFile(path)
	if cerr := <-closed; cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("lockFile with a foreign handle open for 200ms: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
