package downloader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// Mode selects how DownloadMedia treats a media file that already exists on
// disk.
type Mode int

const (
	// KeepExisting skips non-empty existing files: first download, or a
	// crash-resume where the bytes on disk are still the right ones.
	KeepExisting Mode = iota
	// ReplaceVideos re-downloads native videos over their existing copies
	// and keeps other media (a --check-media size mismatch).
	ReplaceVideos
	// ReplaceAll re-downloads every item over its existing copy (an edited
	// post may have swapped media at the same slot).
	ReplaceAll
)

// DownloadMedia downloads all non-external media items to the given directory.
// Best-effort: every item is attempted; per-file failures are logged and
// joined into the returned error. A non-nil return means at least one item
// failed, so callers must not record the post as fully downloaded in state.
//
// Replacement never deletes first: the existing file stays until the new
// bytes are renamed over it, so a failed re-download leaves the old copy in
// place rather than an empty slot.
func DownloadMedia(c *boosty.Client, media []parser.MediaItem, dir string, mode Mode) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var errs []error
	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		replace := mode == ReplaceAll || (mode == ReplaceVideos && m.Type == "video")
		c.Log.Printf("  downloading %s...", m.Filename)
		err := c.Download(boosty.DownloadRequest{
			URL:     m.URL,
			Path:    filepath.Join(dir, m.Filename),
			Key:     m.ID,
			Replace: replace,
		})
		if err != nil {
			c.Log.Printf("  warning: failed to download %s: %v", m.Filename, err)
			errs = append(errs, fmt.Errorf("%s: %w", m.Filename, err))
		}
	}
	return errors.Join(errs...)
}
