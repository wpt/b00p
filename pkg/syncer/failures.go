package syncer

import (
	"fmt"
	"strings"
	"sync"

	"github.com/wpt/b00p/pkg/boosty"
)

// maxNamedFailures caps how many failed posts the run's error lists.
const maxNamedFailures = 10

// failures records the posts the current Sync / DownloadAll failed. Workers
// log their own errors and return; the run converts the record into its
// error, so a cron or --sync --yes run exits non-zero and names what failed:
// with --workers > 1 the per-post log lines interleave.
type failures struct {
	mu    sync.Mutex
	posts []string
}

// add records one failed post by title and ID.
func (f *failures) add(title, id string) {
	f.addf("%q (%s)", title, id)
}

// addf records a failure the run could not tie to a parsed post.
func (f *failures) addf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, fmt.Sprintf(format, args...))
}

func (f *failures) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = nil
}

func (f *failures) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// err returns nil when nothing failed, else an error naming the failed posts.
func (f *failures) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) == 0 {
		return nil
	}
	named := f.posts[:min(len(f.posts), maxNamedFailures)]
	more := ""
	if n := len(f.posts) - len(named); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return fmt.Errorf("%d post(s) failed: %s%s; see log above", len(f.posts), strings.Join(named, ", "), more)
}

// skipMalformed logs a listing entry that did not parse and counts it as a
// failure: an API schema change would otherwise turn every run into
// "Everything up to date" while nothing new is archived.
func (e *Engine) skipMalformed(err error) {
	e.c.Log.Printf("  warning: skipping malformed post: %v", err)
	e.failed.addf("malformed listing entry (%v)", err)
}

// firstSeen reports whether post's ID is new to this listing. A repeat (the
// listing shifting under pagination) would queue the post twice, and two
// workers would write the same directory and media .tmp at once.
func (e *Engine) firstSeen(seen map[string]bool, post boosty.Post) bool {
	if seen[post.ID] {
		e.c.Log.Printf("  warning: post %s listed twice; skipping the repeat", post.ID)
		return false
	}
	seen[post.ID] = true
	return true
}
