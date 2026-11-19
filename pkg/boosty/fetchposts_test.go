package boosty

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// maxPageRequests caps every listing fixture below: a FetchPosts that ignores
// isLast or loses the offset would otherwise re-request forever and hang the
// test instead of failing it.
const maxPageRequests = 5

// pageOffset is opaque server data with characters that must be query-escaped.
const pageOffset = "1700000000:p2+x=/&"

// listingServer serves the blog "b" listing through page(offset); the
// returned func lists each request's offset parameter.
func listingServer(t *testing.T, page func(offset string) string) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var offsets []string
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/blog/b/post/" {
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) > maxPageRequests {
			t.Errorf("more than %d listing requests; pagination does not terminate", maxPageRequests)
			fmt.Fprint(w, `{"data":[],"extra":{"isLast":true}}`)
			return
		}
		offset := r.URL.Query().Get("offset")
		mu.Lock()
		offsets = append(offsets, offset)
		mu.Unlock()
		fmt.Fprint(w, page(offset))
	}))
	t.Cleanup(srv.Close)
	return &Client{
		Tokens: &Tokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
		HTTP:   rewriteClient(t, srv),
		Log:    discardLogger{},
	}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(offsets)
	}
}

// twoPages answers page 1 with p1, p2 and the offset, page 2 with p3, a
// malformed item and p4. Page 2 still names an offset, so only isLast ends
// the listing.
func twoPages(offset string) string {
	switch offset {
	case "":
		return fmt.Sprintf(`{"data":[{"id":"p1"},{"id":"p2"}],"extra":{"isLast":false,"offset":%q}}`, pageOffset)
	case pageOffset:
		return `{"data":[{"id":"p3"},{"id":5},{"id":"p4"}],"extra":{"isLast":true,"offset":"past-the-end"}}`
	}
	return `{"data":[],"extra":{"isLast":true}}`
}

func TestFetchPosts_FollowsOffsetAndSkipsMalformedItem(t *testing.T) {
	c, offsets := listingServer(t, twoPages)

	var ids []string
	var parseErrs int
	for post, err := range c.FetchPosts("b", 2) {
		if err != nil {
			parseErrs++
			continue
		}
		ids = append(ids, post.ID)
	}

	if fmt.Sprint(ids) != "[p1 p2 p3 p4]" {
		t.Errorf("ids = %v, want [p1 p2 p3 p4]", ids)
	}
	if parseErrs != 1 {
		t.Errorf("errors = %d, want 1 (the malformed item)", parseErrs)
	}
	if got := offsets(); !slices.Equal(got, []string{"", pageOffset}) {
		t.Errorf("request offsets = %q, want [\"\" %q]", got, pageOffset)
	}
}

func TestFetchPosts_BreakSendsNoFurtherRequest(t *testing.T) {
	c, offsets := listingServer(t, twoPages)

	for post, err := range c.FetchPosts("b", 2) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if post.ID == "p2" {
			break
		}
	}
	if n := len(offsets()); n != 1 {
		t.Errorf("listing requests = %d, want 1 (break on page 1)", n)
	}
}

// A page that is not last but carries no offset (or no data) cannot be
// continued; re-requesting page 1 would loop.
func TestFetchPosts_StopsWithoutOffsetOrData(t *testing.T) {
	for name, body := range map[string]string{
		"no offset": `{"data":[{"id":"p1"}],"extra":{"isLast":false,"offset":""}}`,
		"no data":   `{"data":[],"extra":{"isLast":false,"offset":"x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, offsets := listingServer(t, func(string) string { return body })
			for _, err := range c.FetchPosts("b", 2) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if n := len(offsets()); n != 1 {
				t.Errorf("listing requests = %d, want 1", n)
			}
		})
	}
}
