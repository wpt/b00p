package parser

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
)

// yamlString returns a YAML-safe double-quoted scalar for the given value.
// strconv.Quote produces a Go-syntax string with C-style escapes for
// control chars, embedded quotes, and backslashes — all of which YAML's
// double-quoted style accepts. Used for any frontmatter value that could
// legitimately contain `:`, `,`, `[`, `]`, newlines, or quotes.
func yamlString(s string) string {
	return strconv.Quote(s)
}

// GenerateMarkdown creates a markdown representation of a post.
func GenerateMarkdown(post *boosty.Post, parsed ParsedContent) string {
	var b strings.Builder

	// Frontmatter — every user-supplied value is double-quoted so
	// titles/tiers/tags containing `:`, `,`, `[`, `]`, or newlines do not
	// produce invalid YAML.
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("title: %s\n", yamlString(post.Title)))
	b.WriteString(fmt.Sprintf("date: %s\n", time.Unix(post.PublishTime, 0).Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("author: %s\n", yamlString(post.User.Name)))
	b.WriteString(fmt.Sprintf("url: %s\n",
		yamlString(fmt.Sprintf("https://boosty.to/%s/posts/%s", post.User.BlogURL, post.ID))))
	if len(post.Tags) > 0 {
		quoted := make([]string, len(post.Tags))
		for i, t := range post.Tags {
			quoted[i] = yamlString(t.Title)
		}
		b.WriteString(fmt.Sprintf("tags: [%s]\n", strings.Join(quoted, ", ")))
	}
	if post.Price > 0 {
		b.WriteString(fmt.Sprintf("price: %d\n", post.Price))
	}
	if post.SubscriptionLevel != nil && post.SubscriptionLevel.Name != "" {
		b.WriteString(fmt.Sprintf("tier: %s\n", yamlString(post.SubscriptionLevel.Name)))
	}
	b.WriteString("---\n\n")

	// Title
	b.WriteString(fmt.Sprintf("# %s\n\n", post.Title))

	// Text content
	for _, text := range parsed.TextParts {
		b.WriteString(text)
		b.WriteString("\n\n")
	}

	// Media references
	for _, m := range parsed.Media {
		switch m.Type {
		case "image":
			b.WriteString(fmt.Sprintf("![%s](%s)\n\n", m.Filename, m.Filename))
		case "video":
			b.WriteString(fmt.Sprintf("[Video: %s](%s)\n\n", m.Filename, m.Filename))
		case "external_video":
			b.WriteString(fmt.Sprintf("[External Video](%s)\n\n", m.URL))
		}
	}

	return b.String()
}
