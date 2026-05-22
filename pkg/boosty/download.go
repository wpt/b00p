package boosty

// Media download path: DownloadFile / downloadOnce and their support
// machinery (idle-read watchdog, Range resume with the .tmp.url sidecar,
// non-retriable status classification, progress writer). The request layer
// (GetJSON, token handling, iterators) lives in client.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RedactURLError strips the URL out of a *url.Error so a surrounding
// fmt.Errorf("download %s: %w", filename, err) (or log.Printf "%v") does
// not re-leak the signed okcdn URL through err.Error() — which renders as
// `Get "<full URL>": ...` for transport-layer failures (DNS, conn-refused,
// header timeout). For any other error type, returns the original.
//
// Exported because the same hazard applies to the syncer's HEAD-check
// path in pkg/syncer/checks.go.
func RedactURLError(err error) error {
	var uerr *neturl.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}

// errIdleTimeout is the cause attached to the download context when the
// idle-read watchdog fires. Surfacing it via context.Cause means the final
// "after 3 retries: ..." error tells the user idle-timeout vs ctrl-C vs
// other cancellation — otherwise it all collapses to "context canceled".
var errIdleTimeout = errors.New("idle timeout: no response body bytes for 60s")

const (
	// downloadIdleTimeout is the maximum time downloadOnce waits between bytes
	// on the response body before cancelling the request. A wedged TCP stream
	// that stops delivering bytes mid-stream would otherwise block io.Copy
	// indefinitely and never give the retry loop a chance to fire.
	downloadIdleTimeout = 60 * time.Second

	// downloadHeaderTimeout caps how long DownloadHTTP waits for response
	// headers after the request body is fully written. Catches the case
	// where the TCP connection is established but the server never replies.
	downloadHeaderTimeout = 60 * time.Second
)

// errNonRetriable marks deterministic failures — a 4xx (other than 429)
// download status, where re-requesting the same URL yields the same verdict.
// DownloadFile fails fast on it instead of burning the backoff schedule.
var errNonRetriable = errors.New("non-retriable")

// newDownloadTransport clones http.DefaultTransport so we inherit modern
// defaults (HTTP/2, connection pooling, dialer timeouts) but layer a
// ResponseHeaderTimeout on top — DefaultTransport has none, so a stuck
// server keeps the request alive forever.
func newDownloadTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = downloadHeaderTimeout
	return t
}

// DownloadFile downloads a URL to a local file path.
// Skips if file already exists with size > 0. Removes 0-byte files.
// Uses a separate HTTP client with no timeout for large files.
// Logs progress with file size.
func (c *Client) DownloadFile(url, path string) error {
	// Integrity check: skip existing non-empty files
	if info, err := os.Stat(path); err == nil {
		if info.Size() > 0 {
			c.Log.Printf("  skipping %s (already exists, %s)", path, FormatSize(info.Size()))
			return nil
		}
		// Remove 0-byte files
		os.Remove(path)
	}

	var lastErr error
	for attempt := 0; attempt <= len(RetryDelays); attempt++ {
		if attempt > 0 {
			c.waitRetry("download retry", attempt)
		}

		err := c.downloadOnce(url, path)
		if err == nil {
			return nil
		}
		if errors.Is(err, errNonRetriable) {
			// Deterministic 4xx (expired signed URL, deleted media, IP
			// rebind): the same URL yields the same verdict, so retrying
			// only wastes the whole backoff schedule per file.
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("after %d retries: %w", len(RetryDelays), lastErr)
}

var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

func (c *Client) downloadOnce(url, path string) error {
	tmpPath := path + ".tmp"
	sidecarPath := tmpPath + ".url"
	// Use filename (not url) in error messages — okcdn signed URLs carry
	// IP-bound credentials in their query string and path; surfacing them
	// to stdout on every transient failure leaks the same secret the
	// Reliability section warns about.
	filename := filepath.Base(path)

	// Resume from an existing partial tmp if one is present. A previous
	// retry (or a crashed run) may have left the first N bytes on disk; we
	// can ask the server to ship only the rest via a Range request. Saves
	// bandwidth and wall time on flaky networks where a multi-GB video
	// might otherwise restart from byte 0 on every attempt.
	//
	// Resume is only safe if the bytes already on disk were downloaded from
	// the same URL we're about to request — otherwise the head bytes of the
	// stale URL would be concatenated with the tail of the new URL, which
	// the size check in --check-media cannot distinguish from a clean file.
	// The sidecar <tmp>.url records the URL the tmp was opened against.
	var resumeFrom int64
	if info, err := os.Stat(tmpPath); err == nil && info.Mode().IsRegular() {
		sidecar, _ := os.ReadFile(sidecarPath)
		if string(sidecar) == url {
			resumeFrom = info.Size()
		} else {
			// URL changed (signed URL refreshed, or no sidecar from a pre-
			// sidecar run): drop the stale partial so we restart cleanly.
			os.Remove(tmpPath)
			os.Remove(sidecarPath)
		}
	}

	// Cancellable context drives the idle-read watchdog below. cancel() fires
	// either when the function returns (defer) or when the watchdog timer
	// expires without seeing a byte; both interrupt io.Copy cleanly. Cause
	// distinguishes idle-timeout cancel (errIdleTimeout) from the normal
	// defer cancel so the final error message tells the user which one.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", filename, RedactURLError(err))
	}
	// okcdn signed URLs bind to the User-Agent used when obtaining them (see
	// srcAg=... in the URL). Reuse the client UA or the server returns 400.
	req.Header.Set("User-Agent", UserAgent)
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := c.DownloadHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", filename, RedactURLError(err))
	}
	defer resp.Body.Close()

	// Idle-read watchdog: cancel the context if no body bytes arrive for
	// downloadIdleTimeout. Armed BEFORE the status switch so even the
	// error-path body-snippet read below cannot hang forever on a server
	// that sends headers and then stalls (DownloadHTTP has no Client.Timeout
	// and ResponseHeaderTimeout covers only the wait for headers). The
	// progressWriter's keepAlive resets the timer on every productive Write
	// so a slow-but-live stream keeps going.
	idleTimer := time.AfterFunc(downloadIdleTimeout, func() { cancel(errIdleTimeout) })
	defer idleTimer.Stop()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored our Range header (if any). resumeFrom is reset
		// below; tmp is truncated and write starts at byte 0.
	case http.StatusPartialContent:
		// Server honored Range. Verify the first byte of the returned range
		// matches our resumeFrom — a misbehaving CDN that returns a wider
		// prefix (e.g. `bytes 500-/total` when we asked `bytes 1000-`) would
		// otherwise have its first 500 bytes O_APPEND'd onto the existing
		// tmp, duplicating those bytes. Spec (RFC 7233) requires first ==
		// our requested first; non-conforming responses get the safe path:
		// treat as 200 (truncate and restart).
		if resumeFrom > 0 {
			cr := resp.Header.Get("Content-Range")
			expectedPrefix := fmt.Sprintf("bytes %d-", resumeFrom)
			if !strings.HasPrefix(cr, expectedPrefix) {
				c.Log.Printf("  warning: server returned 206 with unexpected Content-Range %q (wanted %s); restarting from 0", cr, expectedPrefix)
				// Drop the partial — we can't trust its alignment any more.
				resp.Body.Close()
				os.Remove(tmpPath)
				os.Remove(sidecarPath)
				return fmt.Errorf("download %s: Content-Range %q does not start at %d", filename, cr, resumeFrom)
			}
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// Our tmp file is larger than the server-side resource (signed URL
		// pointing at a re-encoded variant, or server-side rotation). Drop
		// the tmp and its sidecar so the next attempt starts fresh.
		os.Remove(tmpPath)
		os.Remove(sidecarPath)
		return fmt.Errorf("download %s: range not satisfiable; tmp reset", filename)
	default:
		// Include a body snippet for diagnosis — bare "status 403" hides the
		// CDN's explanation. okcdn returns plaintext or JSON; cap the read
		// so an HTML challenge page doesn't dominate the log line.
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		bodyStr := strings.TrimSpace(string(bodySnippet))
		// 400 / 403 / 410 on signed CDN URLs is usually expiry or IP-binding —
		// add a hint so the user knows to re-run rather than chase a token bug.
		hint := ""
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusGone:
			hint = " (signed URL likely expired or IP changed; rerun sync)"
		}
		statusErr := fmt.Errorf("download %s: status %d%s: %s", filename, resp.StatusCode, hint, bodyStr)
		// 4xx other than 429 is deterministic — same URL, same verdict — so
		// mark it non-retriable and let DownloadFile fail fast. 429 and 5xx
		// are transient by nature and keep the retry schedule.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%w (%w)", statusErr, errNonRetriable)
		}
		return statusErr
	}

	// Write to <path>.tmp and rename on success. A SIGKILL/power-loss mid-copy
	// leaves a .tmp orphan that the next attempt can resume from via Range:
	// the orphan is no longer truncated on entry (that would defeat resume).
	//
	// Invariants this relies on:
	//   - path and path+".tmp" live on the same filesystem. os.Rename across
	//     filesystems returns EXDEV on Linux/macOS and ERROR_NOT_SAME_DEVICE
	//     on Windows; all current callers keep media under the same blog dir
	//     as the destination, so this holds. A future caller that puts the
	//     tmp staging area on a different mount must implement copy+remove.
	//   - No two goroutines target the same `path` concurrently. The syncer
	//     dirReserver gives each post a unique directory and DownloadMedia
	//     emits unique filenames per media slot, so concurrent workers do
	//     not race on the .tmp suffix. Direct callers outside the syncer
	//     must enforce this themselves.
	var f *os.File
	if resp.StatusCode == http.StatusPartialContent && resumeFrom > 0 {
		f, err = os.OpenFile(tmpPath, os.O_WRONLY|os.O_APPEND, 0644)
	} else {
		// Server ignored Range (200) or we had no tmp to resume from: start
		// over by truncating the tmp and writing from byte 0.
		f, err = os.Create(tmpPath)
		resumeFrom = 0
	}
	if err != nil {
		return fmt.Errorf("create file %s: %w", tmpPath, err)
	}
	// Pin the URL the tmp is now associated with so the next retry can
	// verify the resume target hasn't shifted. Best-effort: a sidecar write
	// failure only weakens future resume (we'd treat the tmp as orphaned
	// and re-download from scratch), not correctness of this attempt.
	if err := os.WriteFile(sidecarPath, []byte(url), 0644); err != nil {
		c.Log.Printf("  warning: failed to write resume sidecar %s: %v", sidecarPath, err)
	}

	// Total bytes once known: 200 returns full size in ContentLength; 206
	// returns the remaining range, so we add resumeFrom to recover the full
	// size for progress display. -1 stays -1 (server didn't advertise).
	totalSize := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent && totalSize > 0 {
		totalSize += resumeFrom
	}

	plog, hasProgress := c.Log.(ProgressLogger)

	pw := &progressWriter{
		writer:    f,
		total:     totalSize,
		written:   resumeFrom,
		filename:  filename,
		log:       plog,
		hasLog:    hasProgress,
		keepAlive: func() { idleTimer.Reset(downloadIdleTimeout) },
	}

	_, copyErr := io.Copy(pw, resp.Body)
	closeErr := f.Close()
	if hasProgress {
		plog.ClearProgress()
	}
	// Promote the watchdog cause through the wrapped error so the retry loop
	// (and the final user-visible message) name the actual reason instead of
	// the generic "context canceled". Run unconditionally on any copyErr —
	// net/http versions differ on whether they expose context.Canceled or
	// the cause directly, so the safest test is "is there a non-standard
	// cause?" rather than gating on errors.Is(copyErr, context.Canceled).
	// Skip wrapping when copyErr already IS the cause (Go 1.26+ surfaces it
	// directly in some paths) — wrapping then produces a duplicate string
	// like "idle timeout: ... (idle timeout: ...)".
	if copyErr != nil {
		if cause := context.Cause(ctx); cause != nil &&
			!errors.Is(cause, context.Canceled) &&
			!errors.Is(cause, context.DeadlineExceeded) &&
			!errors.Is(copyErr, cause) {
			copyErr = fmt.Errorf("%w (%s)", copyErr, cause)
		}
	}
	if copyErr != nil || closeErr != nil {
		// Leave the partial tmp in place when only the copy errored — the
		// retry loop in DownloadFile re-enters downloadOnce, sees the tmp,
		// and resumes via Range. If the failure is structural, the next
		// attempt either receives 416 (handled above, tmp dropped) or 200
		// (server ignored Range, tmp truncated above). If the close itself
		// errored, the file's durability is suspect — drop the tmp so the
		// next attempt starts fresh rather than resuming garbage. The
		// sidecar goes with it — tmp and sidecar are always dropped (or
		// kept) as a pair so the pairing never has to be reasoned about.
		if closeErr != nil {
			os.Remove(tmpPath)
			os.Remove(sidecarPath)
		}
		return fmt.Errorf("write %s: %w", path, errors.Join(copyErr, closeErr))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		os.Remove(sidecarPath)
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	// Sidecar's only purpose is resume safety — once the final file is in
	// place there is nothing to verify on the next run.
	os.Remove(sidecarPath)

	c.Log.Printf("  downloaded %s (%s)", filename, FormatSize(pw.written))
	return nil
}

type progressWriter struct {
	writer   io.Writer
	total    int64
	written  int64
	filename string
	log      ProgressLogger
	hasLog   bool
	lastLog  time.Time
	frame    int
	// keepAlive resets the idle-read watchdog. Called on every Write that
	// landed at least one byte so a slow-but-live stream keeps streaming.
	// nil for callers that don't enforce idle timeouts.
	keepAlive func()
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.writer.Write(p)
	pw.written += int64(n)
	if n > 0 && pw.keepAlive != nil {
		pw.keepAlive()
	}

	if pw.hasLog && time.Since(pw.lastLog) > 100*time.Millisecond {
		pw.lastLog = time.Now()
		spinner := string(spinnerFrames[pw.frame%len(spinnerFrames)])
		pw.frame++

		if pw.total > 0 {
			pct := float64(pw.written) / float64(pw.total) * 100
			pw.log.Progress("  %s %s  %s / %s  (%.1f%%)",
				spinner, pw.filename, FormatSize(pw.written), FormatSize(pw.total), pct)
		} else {
			pw.log.Progress("  %s %s  %s",
				spinner, pw.filename, FormatSize(pw.written))
		}
	}

	return n, err
}

// FormatSize formats a byte count as a human-readable string.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
