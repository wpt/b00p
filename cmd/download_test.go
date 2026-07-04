package cmd

import "testing"

// blogNameRe protects filepath.Join from URL-derived blog slugs that contain
// path traversal or separators. These tests pin the contract so future
// refactors do not silently widen the accepted set.
func TestBlogNameRe(t *testing.T) {
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

		// Path traversal — primary attack we are blocking.
		{"dotdot", "..", false},
		{"single_dot", ".", false},
		{"traversal", "../etc", false},

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
			if got := blogNameRe.MatchString(tc.blog); got != tc.want {
				t.Errorf("blogNameRe.MatchString(%q) = %v, want %v", tc.blog, got, tc.want)
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
		{
			name: "unsafe_blog",
			url:  "https://boosty.to/../posts/abc-123",
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
