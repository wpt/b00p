package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// externalTemplate is yt-dlp's output name for an embed: its remote identity,
// not its slot in the post. yt-dlp skips a file that already exists, so a
// slot name kept the old video after an edit swapped the embed, and fetched
// the same video again under a new number after an edit shifted the slots.
const externalTemplate = "external_%(extractor_key)s_%(id)s.%(ext)s"

// LookYTDLP returns the yt-dlp executable DownloadExternal runs, or why it
// cannot run one. Callers check it before a run: external videos are not
// tracked in state, so a run that finds no yt-dlp loses them for good.
func LookYTDLP() (string, error) {
	ytdlp, err := exec.LookPath("yt-dlp")
	if err != nil {
		return "", fmt.Errorf("yt-dlp not found (%w). Install it: pip install yt-dlp", err)
	}
	// Windows runs .bat/.cmd through cmd.exe, and Go's argument quoting leaves
	// `&`, `|` and `>` bare, so a post URL like `...?v=x&calc` would run calc.
	if runtime.GOOS == "windows" {
		if ext := filepath.Ext(ytdlp); strings.EqualFold(ext, ".bat") || strings.EqualFold(ext, ".cmd") {
			return "", fmt.Errorf("yt-dlp resolves to a batch wrapper (%s), which would let cmd.exe run commands hidden in post URLs. Install yt-dlp.exe (pip install yt-dlp ships one) and put it first in PATH", ytdlp)
		}
	}
	return ytdlp, nil
}

// DownloadExternal downloads external videos (YouTube, VK, etc.) using yt-dlp.
// Failures for individual videos are logged but do not abort the loop; the
// combined error (or nil) is returned so the caller can surface them.
//
// Returns nil if there are no external videos in media — yt-dlp is only
// required when there is something to download.
func DownloadExternal(log boosty.Logger, media []parser.MediaItem, dir string) error {
	if !slices.ContainsFunc(media, func(m parser.MediaItem) bool { return m.Type == "external_video" }) {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	ytdlp, err := LookYTDLP()
	if err != nil {
		return err
	}

	var errs []error
	for _, m := range media {
		if m.Type != "external_video" {
			continue
		}
		if u, err := url.Parse(m.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			log.Printf("  warning: skipping external video with non-http(s) URL %q", m.URL)
			errs = append(errs, fmt.Errorf("%q: not an http(s) URL", m.URL))
			continue
		}
		log.Printf("  downloading external video: %s", m.URL)
		// cmd.Dir is set to `dir`, so the -o template is relative to that
		// directory. Including `dir` in the template too would nest the path
		// (output/blog/post/output/blog/post/file.mp4) when dir is relative.
		// `--` stops yt-dlp from interpreting a URL that begins with `-` as a
		// flag (e.g. `--exec=...`), which would otherwise let a hostile post
		// smuggle arbitrary args through the embed. The user's yt-dlp config
		// still applies (cookies, formats), but -P and --download-archive there
		// would land the video outside the post dir or skip it as already
		// archived; command-line options override the config.
		ctx, cancel := context.WithTimeout(context.Background(), ytdlpTimeout)
		cmd := exec.CommandContext(ctx, ytdlp, "--no-playlist", "-P", ".", "--no-download-archive",
			"-o", externalTemplate, "--", m.URL)
		cmd.Dir = dir
		// WaitDelay closes the inherited stdout/stderr pipes after the
		// context fires so a grandchild that outlives yt-dlp (most often
		// ffmpeg invoked for muxing) cannot keep cmd.Wait blocked forever.
		// Without this the 20m timeout would not fire on the very scenario
		// it was added for — ffmpeg muxing hangs on a remuxed YouTube DL.
		//
		// Accepted cost: only yt-dlp itself is killed on timeout — the
		// grandchild is not (that needs process groups on Linux and Job
		// objects on Windows; not worth the platform plumbing for a
		// best-effort feature). On Linux the orphan usually dies on its next
		// pipe write (SIGPIPE); on Windows it can run on and leave .part
		// files in the post directory until it finishes or errors out.
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
