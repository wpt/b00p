package downloader

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// DownloadExternal downloads external videos (YouTube, VK, etc.) using yt-dlp.
// Failures for individual videos are logged but do not abort the loop; the
// combined error (or nil) is returned so the caller can surface them.
func DownloadExternal(log boosty.Logger, media []parser.MediaItem, dir string) error {
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
		out := filepath.Join(dir, m.Filename+".%(ext)s")
		cmd := exec.Command(ytdlp, "-o", out, m.URL)
		cmd.Dir = dir
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("  warning: yt-dlp failed for %s: %v\n%s", m.URL, err, string(output))
			errs = append(errs, fmt.Errorf("%s: %w", m.URL, err))
		}
	}
	return errors.Join(errs...)
}
