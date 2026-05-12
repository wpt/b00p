package syncer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
)

// recordingLogger captures log lines for later inspection. Concurrency-safe
// because DownloadAll fans log writes out across N worker goroutines.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// fakeVideoServer serves HEAD responses per the test table.
func fakeVideoServer(t *testing.T, wantUA string, status int, contentLength int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method = %q, want HEAD", r.Method)
		}
		if wantUA != "" && r.Header.Get("User-Agent") != wantUA {
			// Simulate okcdn behavior: wrong UA → 400 with tiny body.
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if contentLength >= 0 {
			w.Header().Set("Content-Length", fmt.Sprint(contentLength))
		}
		w.WriteHeader(status)
	}))
}

// --- fakeAPI: routable httptest harness for end-to-end engine tests ---
//
// The boosty package builds URLs against a hardcoded BaseURL constant
// ("https://api.boosty.to"), so we can't just point the client at a test
// server. Instead, a custom RoundTripper rewrites every request bound for
// api.boosty.to to this server's address. Production URL builders
// (boosty.PostsURL etc.) flow through unmodified.
//
// Media URLs are full URLs the test controls — point them at f.MediaURL(name)
// directly, no rewrite needed since they don't go through api.boosty.to.

type fakeAPI struct {
	t         *testing.T
	srv       *httptest.Server
	targetURL *url.URL

	mu       sync.Mutex
	handlers map[string]http.HandlerFunc

	client *boosty.Client
	log    *recordingLogger
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:        t,
		handlers: make(map[string]http.HandlerFunc),
		log:      &recordingLogger{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.dispatch))
	t.Cleanup(f.srv.Close)

	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	f.targetURL = u

	transport := &apiRewriteTransport{target: f.targetURL, base: http.DefaultTransport}
	f.client = &boosty.Client{
		Tokens: &boosty.Tokens{
			AccessToken: "test-token",
			ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		},
		HTTP:         &http.Client{Transport: transport, Timeout: 10 * time.Second},
		DownloadHTTP: &http.Client{Transport: transport, Timeout: 10 * time.Second},
		Log:          f.log,
	}
	return f
}

func (f *fakeAPI) dispatch(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	h, ok := f.handlers[key]
	f.mu.Unlock()
	if !ok {
		f.t.Logf("fakeAPI: no handler for %s (query=%q)", key, r.URL.RawQuery)
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// HandleFunc registers a handler for METHOD + exact path. Query string is
// ignored at the routing layer — handlers can inspect r.URL.Query() if needed.
func (f *fakeAPI) HandleFunc(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	f.handlers[method+" "+path] = h
	f.mu.Unlock()
}

// apiRewriteTransport reroutes api.boosty.to requests to the test server.
// Other hosts pass through. Uses req.Clone so we don't violate the
// RoundTripper contract of not mutating the input request.
type apiRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (a *apiRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.boosty.to" {
		return a.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = a.target.Scheme
	clone.URL.Host = a.target.Host
	return a.base.RoundTrip(clone)
}

// --- Fixture helpers ---

// PostsList registers a single-page response for a blog's post listing.
// Pagination is set to IsLast=true so FetchPosts terminates after one call.
func (f *fakeAPI) PostsList(blog string, posts ...boosty.Post) {
	raws := make([]json.RawMessage, len(posts))
	for i, p := range posts {
		b, err := json.Marshal(p)
		if err != nil {
			f.t.Fatalf("marshal post %s: %v", p.ID, err)
		}
		raws[i] = b
	}
	body := boosty.PostsResponse{
		Data:  raws,
		Extra: boosty.PaginationExtra{IsLast: true, IsFirst: true},
	}
	f.HandleFunc("GET", "/v1/blog/"+blog+"/post/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	})
}

// SinglePost registers a response for fetching one post by ID. Used by apply
// (refetch on Edited) and check-media (refresh signed video URLs).
func (f *fakeAPI) SinglePost(blog, postID string, post boosty.Post) {
	f.HandleFunc("GET", "/v1/blog/"+blog+"/post/"+postID, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(post)
	})
}

// CommentsList registers a single-page comments response.
func (f *fakeAPI) CommentsList(blog, postID string, comments ...boosty.Comment) {
	body := boosty.CommentsResponse{
		Data:  comments,
		Extra: boosty.PaginationExtra{IsLast: true, IsFirst: true},
	}
	f.HandleFunc("GET", "/v1/blog/"+blog+"/post/"+postID+"/comment/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	})
}

// Media serves bytes for GET and Content-Length for HEAD at MediaURL(name).
// The HEAD-reported size matches the GET body, which is the realistic case;
// MediaHEADSize lets tests intentionally diverge to drive mismatch detection.
func (f *fakeAPI) Media(filename string, content []byte) {
	path := "/_media/" + filename
	f.HandleFunc("GET", path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		_, _ = w.Write(content)
	})
	f.HandleFunc("HEAD", path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.WriteHeader(http.StatusOK)
	})
}

// MediaURL returns the absolute URL a fixture should embed in post content
// blocks so DownloadMedia / HEAD lands on this server's handler.
func (f *fakeAPI) MediaURL(filename string) string {
	return f.srv.URL + "/_media/" + filename
}
