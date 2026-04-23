package boosty

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	BaseURL   = "https://api.boosty.to"
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"

	maxRetries = 3
)

var retryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

// Client is an authenticated HTTP client for the Boosty API.
//
// HTTP carries a 60s timeout suitable for API calls; DownloadHTTP has no
// timeout because media downloads can legitimately take many minutes for
// gigabyte-scale videos. Both clients are reused across requests to share
// the connection pool — earlier code allocated a fresh *http.Client per
// download, defeating keep-alive.
//
// tokensMu guards reads/writes of *Tokens fields against concurrent refresh
// from worker goroutines (--workers > 1). All access goes through
// currentToken() / refreshAndSave().
type Client struct {
	Tokens       *Tokens
	AuthPath     string
	HTTP         *http.Client // API requests; 60s timeout.
	DownloadHTTP *http.Client // Media downloads; no timeout.
	Log          Logger

	tokensMu sync.Mutex
}

// Logger is the interface for logging messages.
// Implement this to capture b00p output in your application.
type Logger interface {
	Printf(format string, args ...any)
}

// ProgressLogger extends Logger with support for in-place progress updates.
type ProgressLogger interface {
	Logger
	// Progress writes a line that will be overwritten by the next Progress call.
	// Used for spinner/progress bar during downloads.
	Progress(format string, args ...any)
	// ClearProgress clears the current progress line.
	ClearProgress()
}

// discardLogger silently drops all log output.
type discardLogger struct{}

func (discardLogger) Printf(string, ...any)   {}
func (discardLogger) Progress(string, ...any) {}
func (discardLogger) ClearProgress()          {}

// NewClient creates a new Boosty API client.
func NewClient(tokens *Tokens, authPath string) *Client {
	return &Client{
		Tokens:   tokens,
		AuthPath: authPath,
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
		DownloadHTTP: &http.Client{
			// No Timeout: media downloads can legitimately take many minutes.
		},
		Log: discardLogger{},
	}
}

// waitRetry logs and sleeps before retry attempt N (1-based, in range [1, maxRetries]).
// The label prefixes the log line (e.g. "retry" or "download retry").
func (c *Client) waitRetry(label string, attempt int) {
	delay := retryDelays[attempt-1]
	c.Log.Printf("  %s %d/%d in %s...", label, attempt, maxRetries, delay)
	time.Sleep(delay)
}

// GetJSON makes an authenticated GET request and decodes the JSON response.
// Retries on network errors and transient HTTP responses (5xx, 429) with
// backoff; non-transient HTTP errors (4xx other than 429, 401-after-refresh)
// fail fast.
func (c *Client) GetJSON(url string, out any) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			c.waitRetry("retry", attempt)
		}

		resp, err := c.doRequest("GET", url)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			httpErr := fmt.Errorf("API %s returned %d: %s", url, resp.StatusCode, string(body))
			// Boosty sits behind Cloudflare/QRATOR which can return 5xx or
			// 429 under load. Treat those as transient and retry; everything
			// else is a real error.
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
				lastErr = httpErr
				continue
			}
			return httpErr
		}

		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		return err
	}
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
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
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			c.waitRetry("download retry", attempt)
			os.Remove(path) // clean up partial file
		}

		err := c.downloadOnce(url, path)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	os.Remove(path) // clean up on final failure
	return fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

func (c *Client) downloadOnce(url, path string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	// okcdn signed URLs bind to the User-Agent used when obtaining them (see
	// srcAg=... in the URL). Reuse the client UA or the server returns 400.
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.DownloadHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file %s: %w", path, err)
	}

	totalSize := resp.ContentLength
	filename := filepath.Base(path)

	plog, hasProgress := c.Log.(ProgressLogger)
	pw := &progressWriter{
		writer:    f,
		total:     totalSize,
		filename:  filename,
		log:       plog,
		hasLog:    hasProgress,
		startTime: time.Now(),
	}

	_, copyErr := io.Copy(pw, resp.Body)
	closeErr := f.Close()
	if hasProgress {
		plog.ClearProgress()
	}
	if copyErr != nil {
		return fmt.Errorf("write %s: %w", path, copyErr)
	}
	// Close errors surface delayed write/flush failures. Without this check a
	// truncated download could be reported as a successful download.
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	c.Log.Printf("  downloaded %s (%s)", filename, FormatSize(pw.written))
	return nil
}

type progressWriter struct {
	writer    io.Writer
	total     int64
	written   int64
	filename  string
	log       ProgressLogger
	hasLog    bool
	startTime time.Time
	lastLog   time.Time
	frame     int
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.writer.Write(p)
	pw.written += int64(n)

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

// currentToken returns the access token under lock together with its
// expiry status. Callers pass the returned token through rawRequest so the
// HTTP request is built without re-reading c.Tokens.
func (c *Client) currentToken() (token string, expired bool) {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	return c.Tokens.AccessToken, c.Tokens.IsExpired()
}

func (c *Client) doRequest(method, url string) (*http.Response, error) {
	token, expired := c.currentToken()
	if expired {
		if err := c.refreshAndSave(token); err != nil {
			return nil, fmt.Errorf("token expired, refresh failed: %w", err)
		}
		token, _ = c.currentToken()
	}

	resp, err := c.rawRequest(method, url, token)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if err := c.refreshAndSave(token); err != nil {
			return nil, fmt.Errorf("401 refresh failed: %w", err)
		}
		token, _ = c.currentToken()
		return c.rawRequest(method, url, token)
	}

	return resp, nil
}

func (c *Client) rawRequest(method, url, token string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", UserAgent)
	return c.HTTP.Do(req)
}

// refreshAndSave acquires the token mutex, refreshes if no other goroutine
// already did, and persists the new tokens. staleToken is the token the
// caller saw before calling — if it no longer matches, another goroutine
// refreshed first and we return without doing extra work.
func (c *Client) refreshAndSave(staleToken string) error {
	c.tokensMu.Lock()
	defer c.tokensMu.Unlock()
	if c.Tokens.AccessToken != staleToken {
		return nil
	}
	if err := c.Tokens.Refresh(c.HTTP); err != nil {
		return err
	}
	if c.AuthPath != "" {
		return c.Tokens.SaveTokens(c.AuthPath)
	}
	return nil
}

// FetchPosts returns an iterator over all posts in a blog.
// Handles pagination internally, yields one Post at a time.
func (c *Client) FetchPosts(blog string, limit int) iter.Seq2[Post, error] {
	return func(yield func(Post, error) bool) {
		offset := ""
		for {
			url := PostsURL(blog, limit, offset)
			var resp PostsResponse
			if err := c.GetJSON(url, &resp); err != nil {
				yield(Post{}, fmt.Errorf("fetch posts: %w", err))
				return
			}

			for _, raw := range resp.Data {
				var post Post
				if err := json.Unmarshal(raw, &post); err != nil {
					if !yield(Post{}, fmt.Errorf("parse post: %w", err)) {
						return
					}
					continue
				}
				if !yield(post, nil) {
					return
				}
			}

			if resp.Extra.IsLast || len(resp.Data) == 0 {
				return
			}
			offset = strings.TrimSpace(resp.Extra.Offset)
			if offset == "" {
				return
			}
		}
	}
}

// FetchComments returns an iterator over all comments on a post.
// Handles pagination internally, yields one Comment at a time.
func (c *Client) FetchComments(blog, postID string, limit int) iter.Seq2[Comment, error] {
	return func(yield func(Comment, error) bool) {
		offset := 0
		for {
			url := CommentsURL(blog, postID, limit, offset)
			var resp CommentsResponse
			if err := c.GetJSON(url, &resp); err != nil {
				yield(Comment{}, err)
				return
			}

			for _, comment := range resp.Data {
				if !yield(comment, nil) {
					return
				}
			}

			if resp.Extra.IsLast || len(resp.Data) == 0 {
				return
			}
			offset += len(resp.Data)
		}
	}
}

// URL builders

// PostsURL returns the URL for listing blog posts.
// The offset is opaque server-supplied data which can contain `+`, `=`, `&`,
// or `%` and so must be query-escaped before being concatenated into the URL.
func PostsURL(blogName string, limit int, offset string) string {
	u := fmt.Sprintf("%s/v1/blog/%s/post/?limit=%d", BaseURL, blogName, limit)
	if offset != "" {
		u += "&offset=" + neturl.QueryEscape(offset)
	}
	return u
}

// PostURL returns the URL for a single post.
func PostURL(blogName, postID string) string {
	return fmt.Sprintf("%s/v1/blog/%s/post/%s", BaseURL, blogName, postID)
}

// defaultReplyLimit caps how many replies Boosty inlines per top-level comment.
// Without this query param the server inlines 0 replies even when replyCount > 0
// (the parent endpoint returns replies.data=[] with isLast=true regardless of
// the actual replyCount), so we silently lose every reply body. 100 covers the
// vast majority of threads; deeper threads will surface as a count mismatch
// against API Count.Comments and re-trigger a sync.
const defaultReplyLimit = 100

// CommentsURL returns the URL for post comments. reply_limit is set to
// defaultReplyLimit to force the server to inline replies; see the constant
// for why.
func CommentsURL(blogName, postID string, limit, offset int) string {
	return fmt.Sprintf("%s/v1/blog/%s/post/%s/comment/?limit=%d&offset=%d&reply_limit=%d",
		BaseURL, blogName, postID, limit, offset, defaultReplyLimit)
}

// UserSubscriptionsURL returns the URL for the current user's subscriptions.
func UserSubscriptionsURL() string {
	return BaseURL + "/v1/user/subscriptions"
}
