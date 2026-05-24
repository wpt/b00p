package downloader

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// ytdlpTimeout caps how long a SINGLE yt-dlp invocation may run. Stuck
// extractors (network hangs against third-party CDNs, infinite redirect
// loops, slow IP-blocked challenges) would otherwise wedge a worker
// forever in headless --sync --yes runs where no TTY exists for ctrl-C.
//
// A post with N external_video blocks gets up to N independent timeouts —
// they run serially, so the worst-case wall time per post is N*ytdlpTimeout.
// In practice posts have 1-3 embeds; if a future Boosty change inflates
// that, switch to a shared deadline across the loop.
const ytdlpTimeout = 20 * time.Minute

// DownloadExternal downloads external videos (YouTube, VK, etc.) using yt-dlp.
// Failures for individual videos are logged but do not abort the loop; the
// combined error (or nil) is returned so the caller can surface them.
//
// Returns nil if there are no external videos in media — yt-dlp is only
// required when there is something to download.
func DownloadExternal(log boosty.Logger, media []parser.MediaItem, dir string) error {
	hasExternal := false
	for _, m := range media {
		if m.Type == "external_video" {
			hasExternal = true
			break
		}
	}
	if !hasExternal {
		return nil
	}

	ytdlp, err := exec.LookPath("yt-dlp")
	if err != nil {
		return fmt.Errorf("yt-dlp not found in PATH. Install it: pip install yt-dlp")
	}

	var errs []error
	for _, m := range media {
		if m.Type != "external_video" {
			continue
		}
		log.Printf("  downloading external video: %s", m.URL)
		// cmd.Dir is set to `dir`, so the -o template is relative to that
		// directory. Including `dir` in the template too would nest the path
		// (output/blog/post/output/blog/post/file.mp4) when dir is relative.
		// `--` stops yt-dlp from interpreting a URL that begins with `-` as a
		// flag (e.g. `--exec=...`), which would otherwise let a hostile post
		// smuggle arbitrary args through the embed.
		ctx, cancel := context.WithTimeout(context.Background(), ytdlpTimeout)
		cmd := exec.CommandContext(ctx, ytdlp, "-o", m.Filename+".%(ext)s", "--", m.URL)
		cmd.Dir = dir
		// WaitDelay closes the inherited stdout/stderr pipes after the
		// context fires so a grandchild that outlives yt-dlp (most often
		// ffmpeg invoked for muxing) cannot keep cmd.Wait blocked forever.
		// Without this the 20m timeout would not fire on the very scenario
		// it was added for — ffmpeg muxing hangs on a remuxed YouTube DL.
		cmd.WaitDelay = 5 * time.Second
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("timed out after %s", ytdlpTimeout)
			}
			log.Printf("  warning: yt-dlp failed for %s: %v\n%s", m.URL, err, string(output))
			errs = append(errs, fmt.Errorf("%s: %w", m.URL, err))
		}
	}
	return errors.Join(errs...)
}
