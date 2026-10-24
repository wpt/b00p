package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/parser"
)

// TestDownloadExternal_RefusesBatchWrapper: run through cmd.exe, a batch
// yt-dlp would execute the `echo.>INJECTED` tail of this URL in the post dir.
func TestDownloadExternal_RefusesBatchWrapper(t *testing.T) {
	for _, name := range []string{"yt-dlp.cmd", "yt-dlp.bat"} {
		t.Run(name, func(t *testing.T) {
			binDir := t.TempDir()
			ranMarker := filepath.Join(t.TempDir(), "ran")
			// Forwards its args like a `py -m yt_dlp %*` wrapper.
			shim := "@echo off\r\necho ran>\"" + ranMarker + "\"\r\necho %*\r\n"
			if err := os.WriteFile(filepath.Join(binDir, name), []byte(shim), 0o755); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			t.Setenv("PATH", binDir)
			t.Setenv("PATHEXT", ".CMD;.BAT")

			dir := t.TempDir()
			err := DownloadExternal(&recordingLogger{}, []parser.MediaItem{
				{Type: "external_video", URL: "https://www.youtube.com/watch?v=abc&echo.>INJECTED"},
			}, dir)
			if err == nil || !strings.Contains(err.Error(), "batch wrapper") {
				t.Errorf("err = %v, want batch wrapper refusal", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "INJECTED")); err == nil {
				t.Error("cmd.exe executed a command from the post URL")
			}
			if _, err := os.Stat(ranMarker); err == nil {
				t.Errorf("%s was executed", name)
			}
		})
	}
}
