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
// Best-effort: every item is attempted; per-file failures are logged and
// joined into the returned error. A non-nil return means at least one item
// failed, so callers must not record the post as fully downloaded in state.
func DownloadMedia(c *boosty.Client, media []parser.MediaItem, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var errs []error
	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		dest := filepath.Join(dir, m.Filename)
		c.Log.Printf("  downloading %s...", m.Filename)
		if err := c.DownloadFile(m.URL, dest); err != nil {
			c.Log.Printf("  warning: failed to download %s: %v", m.Filename, err)
			errs = append(errs, fmt.Errorf("%s: %w", m.Filename, err))
		}
	}
	return errors.Join(errs...)
}
