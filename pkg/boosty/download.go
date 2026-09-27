package boosty

// Media download path: Download / downloadOnce and their support machinery
// (idle-read watchdog, Range resume with the .tmp.url sidecar, non-retriable
// status classification, progress writer). The request layer (GetJSON,
// token handling, iterators) lives in client.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wpt/b00p/pkg/fileutil"
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

	// downloadIdleConnsPerHost raises the per-host idle pool above the
	// net/http default of 2 so --workers N > 2 pulling from one CDN host keep
	// their connections between files instead of re-handshaking TLS.
	downloadIdleConnsPerHost = 16
)

// errNonRetriable marks deterministic failures — a 4xx (other than 429)
// download status, where re-requesting the same URL yields the same verdict.
// Download fails fast on it instead of burning the backoff schedule.
var errNonRetriable = errors.New("non-retriable")

// newDownloadTransport clones http.DefaultTransport so we inherit modern
// defaults (HTTP/2, connection pooling, dialer timeouts) but layer a
// ResponseHeaderTimeout on top — DefaultTransport has none, so a stuck
// server keeps the request alive forever.
func newDownloadTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = downloadHeaderTimeout
	t.MaxIdleConnsPerHost = downloadIdleConnsPerHost
	return t
}

// DownloadRequest describes one media download for Client.Download.
type DownloadRequest struct {
	URL  string
	Path string

	// Key identifies the remote object independently of URL signing. A
	// partial <Path>.tmp left by an earlier attempt is resumed only when the
	// key recorded in its sidecar matches. Signed okcdn URLs change on every
	// refresh (expires/sig/srcIp query params), so keying on the URL itself
	// would restart every cross-run resume from byte 0. Empty means URL.
	Key string

	// Replace downloads even when Path already holds a non-empty file. The
	// old file stays in place until the new bytes are renamed over it, so a
	// failed re-download never leaves the slot empty.
	Replace bool
}

// DownloadFile downloads url to path, skipping the download when path
// already holds a non-empty file (0-byte leftovers are re-downloaded). Media
// that must be refreshed over an existing copy goes through Download with
// Replace set.
func (c *Client) DownloadFile(url, path string) error {
	return c.Download(DownloadRequest{URL: url, Path: path})
}

// Download runs req with the retry schedule: first attempt + one retry per
// RetryDelays entry on transport errors, 5xx and 429; deterministic 4xx fail
// fast. Progress is reported through the client's ProgressLogger when it
// implements one.
func (c *Client) Download(req DownloadRequest) error {
	if info, err := os.Stat(req.Path); err == nil && !req.Replace {
		if info.Size() > 0 {
			c.Log.Printf("  skipping %s (already exists, %s)", req.Path, FormatSize(info.Size()))
			return nil
		}
		// Remove 0-byte files. Best-effort: downloadOnce writes to <path>.tmp
		// and os.Rename replaces the destination on success (clobbering a stale
		// 0-byte file on both Linux and Windows), so this upfront unlink is a
		// cleanliness step, not a correctness prerequisite. A transient failure
		// here (e.g. a Windows AV/indexer briefly holding the file open) must
		// not abort a download the rename would otherwise complete.
		if err := fileutil.RemoveIfExists(req.Path); err != nil {
			c.Log.Printf("  warning: failed to remove zero-byte file %s: %v", req.Path, err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= len(RetryDelays); attempt++ {
		if attempt > 0 {
			c.waitRetry("download retry", attempt, lastErr)
		}

		err := c.downloadOnce(req)
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

// resumeSidecar is the content of <tmp>.url: the key the partial was
// downloaded against and the total object size the server advertised (0 when
// unknown). Both must match on resume — the key guards against a different
// object at the same slot, the size against the same object re-encoded at a
// different quality between runs.
type resumeSidecar struct {
	Key   string
	Total int64
}

func (s resumeSidecar) encode() []byte {
	return []byte(s.Key + "\n" + strconv.FormatInt(s.Total, 10) + "\n")
}

func parseResumeSidecar(data []byte) resumeSidecar {
	key, rest, _ := strings.Cut(string(data), "\n")
	total, _ := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	return resumeSidecar{Key: key, Total: total}
}

// contentRangeTotal extracts the complete-length from a Content-Range
// header ("bytes 4-7/8" → 8). Returns 0 when the header is malformed or the
// length is "*" (unknown).
func contentRangeTotal(cr string) int64 {
	_, total, ok := strings.Cut(cr, "/")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// dropPartial removes the tmp/sidecar pair after a resume verdict that made
// the partial untrustworthy. A failed removal is marked non-retriable: the
// next attempt would resume the same bytes and re-hit the same verdict, so
// burning the backoff schedule cannot help.
func dropPartial(tmpPath, sidecarPath string) error {
	if err := removePair(tmpPath, sidecarPath); err != nil {
		return fmt.Errorf("tmp reset failed: %w", errors.Join(err, errNonRetriable))
	}
	return nil
}

func (c *Client) downloadOnce(req DownloadRequest) error {
	url, path := req.URL, req.Path
	if req.Key == "" {
		req.Key = url
	}
	tmpPath := path + ".tmp"
	sidecarPath := tmpPath + ".url"
	// Use filename (not url) in error messages — okcdn signed URLs carry
	// IP-bound credentials in their query string; surfacing them on every
	// transient failure leaks the same secret the Reliability section warns
	// about.
	filename := filepath.Base(path)

	// Resume from an existing partial tmp if one is present: a previous retry
	// or a crashed run may have left the first N bytes on disk, and the server
	// can ship only the rest via a Range request. The sidecar must name the
	// same object (see DownloadRequest.Key) — otherwise the head bytes of one
	// object would be concatenated with the tail of another, which the size
	// check in --check-media cannot distinguish from a clean file.
	var resumeFrom, expectedTotal int64
	if info, err := os.Stat(tmpPath); err == nil && info.Mode().IsRegular() {
		data, readErr := os.ReadFile(sidecarPath)
		if sc := parseResumeSidecar(data); readErr == nil && sc.Key == req.Key {
			resumeFrom = info.Size()
			expectedTotal = sc.Total
		} else {
			if readErr != nil && !os.IsNotExist(readErr) {
				c.Log.Printf("  warning: failed to read resume sidecar %s: %v", sidecarPath, readErr)
			}
			// Different object (or no sidecar): drop the stale partial so we
			// restart cleanly. Best-effort: the truncating os.Create + sidecar
			// rewrite below restart from byte 0 regardless, so a failed unlink
			// must not abort an otherwise-fine download.
			if err := removePair(tmpPath, sidecarPath); err != nil {
				c.Log.Printf("  warning: failed to reset stale tmp for %s: %v", filename, err)
			}
		}
	}

	// Cancellable context drives the idle-read watchdog below. cancel() fires
	// either when the function returns (defer) or when the watchdog timer
	// expires without seeing a byte; both interrupt io.Copy cleanly. Cause
	// distinguishes idle-timeout cancel (errIdleTimeout) from the normal
	// defer cancel so the final error message tells the user which one.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", filename, RedactURLError(err))
	}
	// okcdn signed URLs bind to the User-Agent used when obtaining them (see
	// srcAg=... in the URL). Reuse the client UA or the server returns 400.
	httpReq.Header.Set("User-Agent", UserAgent)
	if resumeFrom > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}

	resp, err := c.DownloadHTTP.Do(httpReq)
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
		// Server honored Range. The returned range must start exactly at
		// resumeFrom (RFC 7233) — a CDN answering a wider prefix would have
		// its first bytes O_APPEND'd onto the tmp, duplicating them — and the
		// advertised total must match the one the partial was opened
		// against, or the object was re-encoded between runs. Either way the
		// partial is untrustworthy: drop it and let the retry restart at 0.
		if resumeFrom > 0 {
			cr := resp.Header.Get("Content-Range")
			expectedPrefix := fmt.Sprintf("bytes %d-", resumeFrom)
			total := contentRangeTotal(cr)
			if !strings.HasPrefix(cr, expectedPrefix) || (expectedTotal > 0 && total > 0 && total != expectedTotal) {
				c.Log.Printf("  warning: server returned 206 with Content-Range %q (wanted %s, total %d); restarting from 0", cr, expectedPrefix, expectedTotal)
				resp.Body.Close()
				if err := dropPartial(tmpPath, sidecarPath); err != nil {
					return fmt.Errorf("download %s: Content-Range %q rejected; %w", filename, cr, err)
				}
				return fmt.Errorf("download %s: Content-Range %q rejected; tmp reset", filename, cr)
			}
			c.Log.Printf("  resuming %s from %s", filename, FormatSize(resumeFrom))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// Our tmp file is larger than the server-side resource (signed URL
		// pointing at a re-encoded variant, or server-side rotation).
		if err := dropPartial(tmpPath, sidecarPath); err != nil {
			return fmt.Errorf("download %s: range not satisfiable; %w", filename, err)
		}
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
		// mark it non-retriable and let Download fail fast. 429 and 5xx are
		// transient by nature and keep the retry schedule.
		if deterministic4xx(resp.StatusCode) {
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

	// Total bytes once known: 200 returns full size in ContentLength; 206
	// returns the remaining range, so we add resumeFrom to recover the full
	// size for progress display. -1 stays -1 (server didn't advertise).
	totalSize := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent && totalSize > 0 {
		totalSize += resumeFrom
	}

	// Pin the object the tmp is now associated with so the next attempt can
	// verify the resume target hasn't shifted. Best-effort: a sidecar write
	// failure only weakens future resume (we'd treat the tmp as orphaned
	// and re-download from scratch), not correctness of this attempt.
	sc := resumeSidecar{Key: req.Key, Total: max(totalSize, 0)}
	if err := os.WriteFile(sidecarPath, sc.encode(), 0644); err != nil {
		c.Log.Printf("  warning: failed to write resume sidecar %s: %v", sidecarPath, err)
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
		// retry loop in Download re-enters downloadOnce, sees the tmp, and
		// resumes via Range. If the failure is structural, the next attempt
		// either receives 416 (handled above, tmp dropped) or 200 (server
		// ignored Range, tmp truncated above). If the close itself errored,
		// the file's durability is suspect — drop the tmp so the next attempt
		// starts fresh rather than resuming unflushed bytes. The sidecar goes
		// with it: tmp and sidecar are always dropped (or kept) as a pair.
		if closeErr != nil {
			if err := dropPartial(tmpPath, sidecarPath); err != nil {
				return fmt.Errorf("write %s: %w", path, errors.Join(copyErr, closeErr, err))
			}
		}
		return fmt.Errorf("write %s: %w", path, errors.Join(copyErr, closeErr))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if cleanupErr := removePair(tmpPath, sidecarPath); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup tmp: %w", cleanupErr))
		}
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	// Sidecar's only purpose is resume safety — once the final file is in
	// place there is nothing to verify on the next run.
	if err := fileutil.RemoveIfExists(sidecarPath); err != nil {
		c.Log.Printf("  warning: failed to remove resume sidecar %s: %v", sidecarPath, err)
	}

	c.Log.Printf("  downloaded %s (%s)", filename, FormatSize(pw.written))
	return nil
}

// removePair drops a tmp file and its .url sidecar together — they are always
// created and removed as a pair so the resume logic never has to reason about a
// tmp with no sidecar (or vice versa). errors.Join attempts both regardless of
// which fails.
func removePair(a, b string) error {
	return errors.Join(fileutil.RemoveIfExists(a), fileutil.RemoveIfExists(b))
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
