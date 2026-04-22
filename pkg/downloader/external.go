package downloader

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

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
		cmd := exec.Command(ytdlp, "-o", m.Filename+".%(ext)s", m.URL)
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("  warning: yt-dlp failed for %s: %v\n%s", m.URL, err, string(output))
			errs = append(errs, fmt.Errorf("%s: %w", m.URL, err))
		}
	}
	return errors.Join(errs...)
}
