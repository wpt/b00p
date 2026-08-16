package cmd

import (
	"strings"
	"testing"
)

// validateBlogName protects filepath.Join from URL-derived blog slugs that
// contain path traversal or separators. These tests pin the contract so future
// refactors do not silently widen the accepted set.
func TestValidateBlogName(t *testing.T) {
	cases := []struct {
		name string
		blog string
		want bool
	}{
		// Valid Boosty-style slugs.
		{"plain", "alice", true},
		{"mixed_case", "AliceBob", true},
		{"digits", "user42", true},
		{"underscore", "alice_bob", true},
		{"hyphen", "alice-bob", true},
		{"single_char", "a", true},
		{"length_64", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},

		// Dots: Boosty allows them mid-name (issue #1).
		{"dotted", "bbb.sss", true},
		{"multi_dot", "a.b.c", true},

		// Path traversal — primary attack we are blocking.
		{"dotdot", "..", false},
		{"single_dot", ".", false},
		{"traversal", "../etc", false},

		// Dot placements that break Windows dirs or hide the dir on Unix.
		{"leading_dot", ".hidden", false},
		{"trailing_dot", "alice.", false},
		{"double_dot_inside", "a..b", false},

		// Reserved device names deliberately allowed (see validateBlogName);
		// pinned so a future "safety" tightening does not re-block real blogs.
		{"reserved_con_ok", "con", true},
		{"reserved_con_dotted_ok", "con.duende", true},

		// FS separators.
		{"slash", "alice/bob", false},
		{"backslash", `alice\bob`, false},

		// Windows-reserved drive/device chars.
		{"colon", "C:", false},
		{"pipe", "a|b", false},
		{"question", "a?b", false},
		{"star", "a*b", false},
		{"angle", "a<b", false},

		// Boundaries.
		{"empty", "", false},
		{"length_65", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},

		// Other dangerous shapes.
		{"null_byte", "alice\x00bob", false},
		{"newline", "alice\nbob", false},
		{"space", "alice bob", false},
		{"unicode", "алиса", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBlogName(tc.blog)
			if got := err == nil; got != tc.want {
				t.Errorf("validateBlogName(%q) = %v, want valid=%v", tc.blog, err, tc.want)
			}
		})
	}
}

// boostyURLRe extracts (blog, postID). Coverage focuses on inputs that have
// historically broken: trailing slash, query string, fragment.
func TestBoostyURLRe(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		wantBlog string
		wantID   string
		wantOK   bool
	}{
		{
			name:     "plain",
			url:      "https://boosty.to/alice/posts/abc-123",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "dotted_blog",
			url:      "https://boosty.to/bbb.sss/posts/abc-123",
			wantBlog: "bbb.sss", wantID: "abc-123", wantOK: true,
		},
		// Bad dot placement is captured on purpose and rejected later by
		// validateBlogName, so the user gets the precise reason.
		{
			name:     "trailing_dot_blog_captured",
			url:      "https://boosty.to/alice./posts/abc-123",
			wantBlog: "alice.", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "query_strips",
			url:      "https://boosty.to/alice/posts/abc-123?utm=x",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "fragment_strips",
			url:      "https://boosty.to/alice/posts/abc-123#top",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "trailing_slash_strips",
			url:      "https://boosty.to/alice/posts/abc-123/",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "no_scheme",
			url:      "boosty.to/alice/posts/abc-123",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name: "wrong_host",
			url:  "https://example.com/alice/posts/abc-123",
		},
		{
			name: "missing_post_segment",
			url:  "https://boosty.to/alice",
		},
		{
			name: "trailing_path_segment",
			url:  "https://boosty.to/alice/posts/abc-123/extra",
		},
		// ".." fits the charset; validateBlogName is the guard (pinned below).
		{
			name:     "traversal_blog_captured",
			url:      "https://boosty.to/../posts/abc-123",
			wantBlog: "..", wantID: "abc-123", wantOK: true,
		},
		{
			name: "unsafe_post_id",
			url:  "https://boosty.to/alice/posts/abc%2F123",
		},
		// Anchoring regressions: the regex must match the HOST, not any
		// substring. Unanchored, all three below were accepted and produced
		// a confusing API 404 instead of a clean "invalid boosty URL".
		{
			name: "embedded_in_other_host",
			url:  "https://evilboosty.to/alice/posts/abc-123",
		},
		{
			name: "smuggled_in_query",
			url:  "https://evil.com/?next=boosty.to/alice/posts/abc-123",
		},
		{
			name: "smuggled_in_fragment",
			url:  "https://evil.com/#boosty.to/alice/posts/abc-123",
		},
		// Legitimate host spellings that must keep working after anchoring.
		{
			name:     "www_prefix",
			url:      "https://www.boosty.to/alice/posts/abc-123",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "mobile_prefix_no_scheme",
			url:      "m.boosty.to/alice/posts/abc-123",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
		{
			name:     "plain_http",
			url:      "http://boosty.to/alice/posts/abc-123",
			wantBlog: "alice", wantID: "abc-123", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := boostyURLRe.FindStringSubmatch(tc.url)
			if !tc.wantOK {
				if m != nil {
					t.Errorf("FindStringSubmatch(%q) = %v, want nil", tc.url, m)
				}
				return
			}
			if m == nil {
				t.Fatalf("FindStringSubmatch(%q) = nil, want match", tc.url)
			}
			if m[1] != tc.wantBlog || m[2] != tc.wantID {
				t.Errorf("FindStringSubmatch(%q) = (%q, %q), want (%q, %q)",
					tc.url, m[1], m[2], tc.wantBlog, tc.wantID)
			}
		})
	}
}

// The --url capture admits ".." and runDownload's validateBlogName call is
// the only guard before filepath.Join — deleting or reordering it must fail here.
func TestRunDownload_ValidatesURLBlog(t *testing.T) {
	oldURL, oldBlog := postURL, blogName
	t.Cleanup(func() { postURL, blogName = oldURL, oldBlog })
	blogName = ""
	postURL = "https://boosty.to/../posts/abc-123"

	err := runDownload(downloadCmd, nil)
	if err == nil || !strings.Contains(err.Error(), `invalid blog ".."`) {
		t.Fatalf("runDownload(--url with traversal blog) = %v, want invalid-blog rejection", err)
	}
}
