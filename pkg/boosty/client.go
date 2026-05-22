package boosty

// Request layer: the authenticated Client, retry/backoff engine, token
// refresh handling, and the post/comment iterators. The media-download path
// lives in download.go; API URL builders in urls.go; token storage and
// refresh protocol in auth.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrFetchPage marks an iterator-level failure in FetchPosts / FetchComments:
// the whole page request failed (transport, 5xx after retries, refresh
// rejected, etc.) and the iteration terminates. Distinct from a per-post
// json.Unmarshal failure inside a successful page, which the iterators
// recover from by yielding (Zero, parseErr) and continuing to the next item.
// Consumers use errors.Is(err, ErrFetchPage) to decide whether to abort the
// whole sync (page error) vs. skip the offending item (parse error).
var ErrFetchPage = errors.New("page fetch failed")

const (
	BaseURL   = "https://api.boosty.to"
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"
)

// RetryDelays controls the backoff schedule between retry attempts. The
// slice length defines the retry count: first attempt + one retry per entry.
// Exposed as a package variable so tests can shrink the schedule to
// milliseconds instead of the 5s/15s/30s the CLI uses against real network
// flake. Library callers may also resize it — the retry loops derive their
// attempt count from len(RetryDelays), so any non-empty schedule is valid.
var RetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}

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
//
// SAFETY: Implementations must synchronize all calls to Printf, Progress, and
// ClearProgress with a mutex. See cmd/log.go for the reference implementation.
//
// The client invokes Progress from download workers (--workers > 1) while
// Printf may fire from the orchestrator at the same time; without internal
// synchronization the spinner line and a log line race and produce garbled
// output (or, worse, data races on a `hasProgress`-style flag).
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
			// Per-attempt liveness is enforced by ResponseHeaderTimeout in the
			// transport (caps wait for headers) plus an idle-read watchdog in
			// downloadOnce that cancels the request context if the body stops
			// delivering bytes for downloadIdleTimeout.
			Transport: newDownloadTransport(),
		},
		Log: discardLogger{},
	}
}

// waitRetry logs and sleeps before retry attempt N (1-based, in range
// [1, len(RetryDelays)]). The label prefixes the log line (e.g. "retry" or
// "download retry").
func (c *Client) waitRetry(label string, attempt int) {
	delay := RetryDelays[attempt-1]
	c.Log.Printf("  %s %d/%d in %s...", label, attempt, len(RetryDelays), delay)
	time.Sleep(delay)
}

// GetJSON makes an authenticated GET request and decodes the JSON response.
// Retries with backoff on network errors and transient HTTP responses (5xx,
// 429). Non-transient HTTP statuses (4xx other than 429) fail fast, and so
// does a server-rejected token refresh (ErrRefreshRejected): the refresh
// request body is deterministic, so a 4xx verdict cannot change on retry —
// retrying would only delay the actionable "get new tokens" message.
func (c *Client) GetJSON(url string, out any) error {
	var lastErr error
	for attempt := 0; attempt <= len(RetryDelays); attempt++ {
		if attempt > 0 {
			c.waitRetry("retry", attempt)
		}

		resp, err := c.doRequest("GET", url)
		if err != nil {
			if errors.Is(err, ErrRefreshRejected) {
				return err
			}
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// Best-effort read for error context: if the body itself errors out
			// (truncated stream, mid-read network drop), the status code we
			// already have is the load-bearing diagnostic, so a partial body —
			// or none at all — is acceptable and we intentionally discard the
			// read error.
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
	return fmt.Errorf("after %d retries: %w", len(RetryDelays), lastErr)
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
				yield(Post{}, fmt.Errorf("%w: %w", ErrFetchPage, err))
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

// FetchComments returns an iterator over a single page of top-level comments
// on a post (replies are inlined per item up to defaultReplyLimit=100).
//
// The Boosty comments endpoint ignores `offset>0` (returns data=[] with
// isLast=true even when more pages exist), so pagination beyond the first
// page does not actually work — pass a `limit` value that covers every
// top-level thread in one call. The CLI uses limit=101 with cap detection;
// library callers should size `limit` similarly (>= post.Count.Comments
// expected top-level threads) and treat the result as "what fit in the
// first page". A returned page of exactly `limit` items hints that the
// post may have more threads than the call could retrieve.
func (c *Client) FetchComments(blog, postID string, limit int) iter.Seq2[Comment, error] {
	return func(yield func(Comment, error) bool) {
		// Single GET, no pagination loop: the server ignores offset>0 (see
		// doc comment), so a second request can never yield anything — the
		// first page IS the result.
		var resp CommentsResponse
		if err := c.GetJSON(CommentsURL(blog, postID, limit, 0), &resp); err != nil {
			yield(Comment{}, fmt.Errorf("%w: %w", ErrFetchPage, err))
			return
		}
		for _, comment := range resp.Data {
			if !yield(comment, nil) {
				return
			}
		}
	}
}
