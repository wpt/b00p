package parser

import (
	"fmt"
	"strings"
	"time"

	"github.com/wpt/b00p/pkg/boosty"
)

// GenerateMarkdown creates a markdown representation of a post.
func GenerateMarkdown(post *boosty.Post, parsed ParsedContent) string {
	var b strings.Builder

	// Frontmatter
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("title: %q\n", post.Title))
	b.WriteString(fmt.Sprintf("date: %s\n", time.Unix(post.PublishTime, 0).Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("author: %s\n", post.User.Name))
	b.WriteString(fmt.Sprintf("url: https://boosty.to/%s/posts/%s\n", post.User.BlogURL, post.ID))
	if len(post.Tags) > 0 {
		tags := make([]string, len(post.Tags))
		for i, t := range post.Tags {
			tags[i] = t.Title
		}
		b.WriteString(fmt.Sprintf("tags: [%s]\n", strings.Join(tags, ", ")))
	}
	if post.Price > 0 {
		b.WriteString(fmt.Sprintf("price: %d\n", post.Price))
	}
	if post.SubscriptionLevel != nil && post.SubscriptionLevel.Name != "" {
		b.WriteString(fmt.Sprintf("tier: %s\n", post.SubscriptionLevel.Name))
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
