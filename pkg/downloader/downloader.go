package downloader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// DownloadMedia downloads all non-external media items to the given directory.
// replace reports whether an item is re-downloaded over a non-empty file
// already at its path; nil keeps every such file (first download, or a
// crash-resume where the bytes on disk are still the right ones).
// Best-effort: every item is attempted; per-file failures are logged and
// joined into the returned error. A non-nil error means at least one item
// failed, so callers must not record the post as fully downloaded in state.
//
// landed lists the items whose file is now in place: downloaded, or kept
// because it already existed and replace said no.
//
// Replacement never deletes first: the existing file stays until the new
// bytes are renamed over it, so a failed re-download leaves the old copy in
// place rather than an empty slot.
func DownloadMedia(c *boosty.Client, media []parser.MediaItem, dir string, replace func(parser.MediaItem) bool) (landed []parser.MediaItem, err error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var errs []error
	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		c.Log.Printf("  downloading %s...", m.Filename)
		err := c.Download(boosty.DownloadRequest{
			URL:     m.URL,
			Path:    filepath.Join(dir, m.Filename),
			Key:     m.ID,
			Replace: replace != nil && replace(m),
		})
		if err != nil {
			c.Log.Printf("  warning: failed to download %s: %v", m.Filename, err)
			errs = append(errs, fmt.Errorf("%s: %w", m.Filename, err))
			continue
		}
		landed = append(landed, m)
	}
	return landed, errors.Join(errs...)
}
