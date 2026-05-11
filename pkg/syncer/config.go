// Package syncer orchestrates per-blog mirror operations: single-post save,
// full download, and incremental sync against on-disk state. The package is
// the use-case layer between cmd/ (flag parsing, CLI wiring) and the lower
// pkg/{boosty,downloader,parser,state} layers.
//
// All inputs are passed via Config — no package globals — so a caller can
// run multiple Engines concurrently without cross-talk and tests can drive
// behavior without a Cobra context.
package syncer

import (
	"io"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
)

// Config carries every input the engine needs to act on a blog. Fields map
// 1:1 to CLI flags; the cmd/ layer is responsible for translation.
type Config struct {
	// Blog is the boosty blog name (URL slug). Required.
	Blog string

	// OutputDir is the directory under which per-blog folders are created.
	OutputDir string

	// DirFormat is the parser-format string used to name post directories.
	// See parser.FormatDirName for the supported tokens.
	DirFormat string

	// WithMD generates post.md alongside post.json on save.
	WithMD bool

	// WithComments fetches and writes comments.json on save.
	WithComments bool

	// DownloadExternal invokes yt-dlp for external video links. Best-effort:
	// failures are logged and not propagated.
	DownloadExternal bool

	// Force ignores state during DownloadAll and re-processes every post.
	// Existing non-empty media files are still skipped at the file layer.
	Force bool

	// CheckMedia runs HEAD-based video size validation as part of Sync.
	CheckMedia bool

	// CheckFiles verifies post.json/comments.json/post.md exist on disk
	// (per state flags) as part of Sync. Pure os.Stat — no network.
	CheckFiles bool

	// AutoApply skips the interactive Apply changes? prompt in Sync.
	// Required for headless runs (cron, nohup, scripts without a TTY).
	AutoApply bool

	// Workers is the parallelism for DownloadAll and CheckMedia.
	// Values <1 are clamped to 1; values above the per-call job count are
	// clamped down.
	Workers int

	// In is the source of confirmation answers when AutoApply is false.
	// Nil means os.Stdin; tests inject a fake reader here to exercise the
	// prompt path without touching the real stdin.
	In io.Reader
}

// Engine binds a Client + Config and exposes the three top-level operations:
// SavePost, DownloadAll, Sync. Construct via New; do not zero-initialize.
type Engine struct {
	c   *boosty.Client
	cfg Config
	res *dirReserver
}

// New returns an Engine ready to operate on cfg.Blog using c. The returned
// engine carries its own dir reserver, so two engines for the same blog do
// not share in-flight name reservations (each is a fresh run).
func New(c *boosty.Client, cfg Config) *Engine {
	if cfg.DirFormat == "" {
		cfg.DirFormat = parser.DefaultFormat
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	return &Engine{c: c, cfg: cfg, res: newDirReserver()}
}
