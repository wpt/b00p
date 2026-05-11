package syncer

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"
)

// confirmApply gates the apply phase of Sync behind a Y/N prompt.
// With auto=true (Config.AutoApply), the prompt is skipped entirely so
// headless callers (cron, nohup, scripts) can run without a TTY. The
// prompt itself is written to stdout (so the user sees it on a real
// terminal); only the structural log lines go through `log` so tests
// can observe behavior via a fake logger without capturing stdout.
func confirmApply(log boosty.Logger, in *bufio.Reader, auto bool) bool {
	log.Printf("")
	if auto {
		log.Printf("Auto-applying (--yes).")
		return true
	}
	fmt.Print("Apply changes? [y/N] ")
	answer, err := in.ReadString('\n')
	if err != nil {
		log.Printf("  warning: failed to read confirmation: %v", err)
		return false
	}
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

// displaySync prints the actionable list and a per-flag summary.
func displaySync(log boosty.Logger, items []syncItem) {
	type counts struct {
		new, unlocked, locked, lockedNew               int
		updated, comments, videoMismatch, filesMissing int
		noChange                                       int
	}
	var k counts

	for _, item := range items {
		switch {
		case item.IsNew:
			k.new++
		case item.IsLockedNew:
			k.lockedNew++
		case item.JustLocked:
			k.locked++
		case item.JustUnlocked:
			k.unlocked++
		}
		if item.Edited {
			k.updated++
		}
		if item.NewComments {
			k.comments++
		}
		if item.VideoMismatch != "" {
			k.videoMismatch++
		}
		if item.Missing.Any() {
			k.filesMissing++
		}
		if !item.IsActionable() {
			k.noChange++
		}
	}

	for _, item := range items {
		if !item.IsActionable() {
			continue
		}
		labels := strings.Join(item.Labels(), ",")
		detail := ""
		if d := item.Detail(); d != "" {
			detail = " (" + d + ")"
		}
		log.Printf("  [%s] %s%s", labels, item.Post.Title, detail)
	}

	log.Printf("")
	log.Printf("Sync summary:")
	if k.new > 0 {
		log.Printf("  %d new posts", k.new)
	}
	if k.unlocked > 0 {
		log.Printf("  %d unlocked posts", k.unlocked)
	}
	if k.updated > 0 {
		log.Printf("  %d updated posts", k.updated)
	}
	if k.comments > 0 {
		log.Printf("  %d comments updated", k.comments)
	}
	if k.videoMismatch > 0 {
		log.Printf("  %d video size mismatches", k.videoMismatch)
	}
	if k.filesMissing > 0 {
		log.Printf("  %d files missing on disk", k.filesMissing)
	}
	if k.locked > 0 {
		log.Printf("  %d locked (data preserved)", k.locked)
	}
	if k.lockedNew > 0 {
		log.Printf("  %d locked (no access)", k.lockedNew)
	}
	log.Printf("  %d no changes", k.noChange)
}
