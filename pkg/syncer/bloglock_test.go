package syncer

import (
	"bufio"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/state"
)

// assertBlogUnlocked fails the test if e's blog lock cannot be taken now.
func assertBlogUnlocked(t *testing.T, e *Engine, after string) {
	t.Helper()
	unlock, err := e.lockBlog(e.blogDir())
	if err != nil {
		t.Fatalf("blog lock still held after %s: %v", after, err)
	}
	unlock()
}

// A second run on a blog whose media download is in flight would resume the
// first run's live .tmp and record a corrupted file as complete. Every entry
// point of a second Engine must fail fast instead, and the first run must
// finish untouched.
func TestEngine_SecondRunOnSameBlogFailsFast(t *testing.T) {
	shortenLockWait(t)
	saved := boosty.RetryDelays
	boosty.RetryDelays = []time.Duration{time.Millisecond}
	defer func() { boosty.RetryDelays = saved }()

	f := newFakeAPI(t)
	started := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock) // before the server's Close, which waits for this handler
	var requests atomic.Int32
	f.HandleFunc("GET", "/_media/slow.jpg", func(w http.ResponseWriter, r *http.Request) {
		// Only the first run's request stalls; a second run that got past
		// the lock downloads at once and its nil error fails the test.
		if requests.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte("IMAGE"))
	})
	post := boosty.Post{
		ID: "p1", Title: "Hello", HasAccess: true, PublishTime: 1700000000, UpdatedAt: 100,
		Data: []boosty.ContentBlock{{Type: "image", URL: f.MediaURL("slow.jpg")}},
	}
	f.PostsList("myblog", post)
	cfg := Config{Blog: "myblog", OutputDir: t.TempDir(), AutoApply: true}
	blogDir := filepath.Join(cfg.OutputDir, cfg.Blog)

	first := make(chan error, 1)
	go func() { first <- New(f.client, cfg).DownloadAll() }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("first run never reached the media download")
	}

	second := New(f.client, cfg)
	runs := map[string]func() error{
		"Sync":        second.Sync,
		"DownloadAll": second.DownloadAll,
		"SavePost": func() error {
			_, _, err := second.SavePost(&post)
			return err
		},
	}
	for name, run := range runs {
		err := run()
		if want := "another b00p run is using " + blogDir; err == nil || err.Error() != want {
			t.Errorf("second %s: err = %v, want %q", name, err, want)
		}
	}

	unblock()
	if err := <-first; err != nil {
		t.Fatalf("first run: %v", err)
	}
	st, err := state.Load(blogDir)
	if err != nil {
		t.Fatal(err)
	}
	dirName := st.Posts["p1"].DirName
	if data, _ := os.ReadFile(filepath.Join(blogDir, dirName, "image_001.jpg")); string(data) != "IMAGE" {
		t.Errorf("image = %q, want the first run's download intact", data)
	}
	assertBlogUnlocked(t, second, "the first run returned")
}

// Every entry point releases the lock when it returns, including a run that
// failed after taking it.
func TestEngine_BlogLockReleasedAfterRun(t *testing.T) {
	f := newFakeAPI(t)
	f.PostsList("myblog")
	cfg := Config{Blog: "myblog", OutputDir: t.TempDir(), AutoApply: true}
	blogDir := filepath.Join(cfg.OutputDir, cfg.Blog)
	e := New(f.client, cfg)

	mustMkdir(t, blogDir)
	writeFile(t, filepath.Join(blogDir, "_state.json"), "{not json")
	if err := e.Sync(); err == nil || !strings.Contains(err.Error(), "load state") {
		t.Fatalf("Sync over corrupt state: err = %v, want a load state error", err)
	}
	assertBlogUnlocked(t, e, "a failed Sync")
	if err := os.Remove(filepath.Join(blogDir, "_state.json")); err != nil {
		t.Fatal(err)
	}

	if err := e.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	assertBlogUnlocked(t, e, "Sync")
	if err := e.DownloadAll(); err != nil {
		t.Fatalf("DownloadAll: %v", err)
	}
	assertBlogUnlocked(t, e, "DownloadAll")
	if _, _, err := e.SavePost(&boosty.Post{ID: "p1", Title: "Hello", HasAccess: true}); err != nil {
		t.Fatalf("SavePost: %v", err)
	}
	assertBlogUnlocked(t, e, "SavePost")
}

// The lock must hold against another process and must not outlive it: a
// killed run (crash, OOM, Ctrl-C) leaves no stale lock behind.
func TestBlogLock_ExcludesOtherProcessAndDiesWithIt(t *testing.T) {
	if path := os.Getenv("B00P_TEST_LOCK_HOLDER"); path != "" {
		f, err := lockFile(path)
		if err != nil {
			os.Stdout.WriteString("lock failed: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("locked\n")
		time.Sleep(time.Minute) // until killed
		runtime.KeepAlive(f)
		os.Exit(0)
	}

	shortenLockWait(t)
	path := filepath.Join(t.TempDir(), blogLockName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestBlogLock_ExcludesOtherProcessAndDiesWithIt$")
	cmd.Env = append(os.Environ(), "B00P_TEST_LOCK_HOLDER="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	kill := func() {
		if !killed {
			killed = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	defer kill()

	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "locked\n" {
		t.Fatalf("holder process: %q, want locked", line)
	}
	if f, err := lockFile(path); !errors.Is(err, errLockHeld) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("lockFile while another process holds it: err = %v, want errLockHeld", err)
	}

	kill()
	f, err := lockFile(path)
	if err != nil {
		t.Fatalf("lock not released after its holder was killed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
