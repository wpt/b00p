package parser

import (
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
)

// TestGenerateMarkdown_FrontmatterEscaping covers titles, authors, tiers,
// and tags containing characters that would break unquoted YAML scalars
// (`:`, `,`, `[`, `]`, `"`, newlines). Before the yamlString helper was
// added these would produce invalid YAML frontmatter.
func TestGenerateMarkdown_FrontmatterEscaping(t *testing.T) {
	post := &boosty.Post{
		ID:          "abc123",
		Title:       `Title with: colon, "quote", brackets [1], and newline` + "\nsecond line",
		PublishTime: 1700000000,
		User: boosty.PostUser{
			Name:    `Author: Name, with comma`,
			BlogURL: "blog",
		},
		Tags: []boosty.Tag{
			{Title: "tag, with comma"},
			{Title: "tag: with colon"},
			{Title: "tag [bracketed]"},
		},
		SubscriptionLevel: &boosty.PostSubLevel{
			Name: `Tier: "Premium"`,
		},
	}

	md := GenerateMarkdown(post, ParsedContent{})

	// All special-char fields must be quoted (Go-syntax double-quoted is
	// also valid YAML double-quoted).
	wantSubstrings := []string{
		`title: "Title with: colon, \"quote\", brackets [1], and newline\nsecond line"`,
		`author: "Author: Name, with comma"`,
		`tier: "Tier: \"Premium\""`,
		`"tag, with comma"`,
		`"tag: with colon"`,
		`"tag [bracketed]"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in frontmatter\n--- output ---\n%s", want, md)
		}
	}

	// Frontmatter delimiters and the title heading must still be there.
	if !strings.HasPrefix(md, "---\n") {
		t.Error("missing leading frontmatter delimiter")
	}
	if !strings.Contains(md, "\n---\n\n") {
		t.Error("missing trailing frontmatter delimiter")
	}
	// The user-visible H1 still uses the raw title — markdown rendering
	// handles in-body content so escaping it is not required.
	if !strings.Contains(md, "# Title with: colon") {
		t.Error("missing H1 heading line")
	}
}

func TestGenerateMarkdown_NoTags(t *testing.T) {
	post := &boosty.Post{
		ID:          "abc",
		Title:       "Plain",
		PublishTime: 1700000000,
		User:        boosty.PostUser{Name: "Author", BlogURL: "blog"},
	}

	md := GenerateMarkdown(post, ParsedContent{})
	if strings.Contains(md, "tags:") {
		t.Errorf("did not expect tags: line, got\n%s", md)
	}
}

func TestGenerateMarkdown_PriceAndTier(t *testing.T) {
	post := &boosty.Post{
		ID:          "abc",
		Title:       "Paid",
		PublishTime: 1700000000,
		Price:       100,
		User:        boosty.PostUser{Name: "Author", BlogURL: "blog"},
		SubscriptionLevel: &boosty.PostSubLevel{
			Name: "tier_2",
		},
	}

	md := GenerateMarkdown(post, ParsedContent{})
	if !strings.Contains(md, "price: 100\n") {
		t.Errorf("missing price: 100\n%s", md)
	}
	if !strings.Contains(md, `tier: "tier_2"`) {
		t.Errorf("missing tier: tier_2\n%s", md)
	}
}
