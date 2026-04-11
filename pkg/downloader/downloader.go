package downloader

import (
	"os"
	"path/filepath"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// DownloadMedia downloads all non-external media items to the given directory.
func DownloadMedia(c *boosty.Client, media []parser.MediaItem, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	for _, m := range media {
		if m.Type == "external_video" {
			continue
		}
		dest := filepath.Join(dir, m.Filename)
		c.Log.Printf("  downloading %s...", m.Filename)
		if err := c.DownloadFile(m.URL, dest); err != nil {
			c.Log.Printf("  warning: failed to download %s: %v", m.Filename, err)
		}
	}
	return nil
}
