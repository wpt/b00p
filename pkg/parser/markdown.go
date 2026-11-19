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
		b.WriteString(fmt.Sprintf("price: %g\n", post.Price))
	}
	if tier := post.TierName(); tier != "" {
		b.WriteString(fmt.Sprintf("tier: %s\n", yamlString(tier)))
	}
	b.WriteString("---\n\n")

	// Title — collapsed onto a single line so a multi-line API title cannot
	// split the H1 across lines.
	b.WriteString(fmt.Sprintf("# %s\n\n", collapseWhitespace(post.Title)))

	parts := parsed.BodyParts
	if parts == nil {
		// Keep callers that assemble ParsedContent themselves working.
		parts = append(parts, parsed.TextParts...)
		for _, m := range parsed.Media {
			parts = append(parts, mediaMarkdown(m))
		}
	}
	for _, part := range parts {
		b.WriteString(part)
		b.WriteString("\n\n")
	}

	return b.String()
}

func mediaMarkdown(m MediaItem) string {
	switch m.Type {
	case "image":
		return fmt.Sprintf("![%s](%s)", m.Filename, m.Filename)
	case "video":
		return fmt.Sprintf("[Video: %s](%s)", m.Filename, m.Filename)
	case "audio":
		return fmt.Sprintf("[Audio: %s](%s)", attachmentLabel(m), m.Filename)
	case "file":
		return fmt.Sprintf("[File: %s](%s)", attachmentLabel(m), m.Filename)
	case "external_video":
		return formatMarkdownLink("External Video", m.URL)
	}
	return ""
}

// attachmentLabel is the link label for an audio/file attachment: the
// author-supplied original name (escaped — it is free text that may contain
// markdown metacharacters), falling back to the parser-generated filename,
// which is metacharacter-free by construction and needs no escaping.
func attachmentLabel(m MediaItem) string {
	if strings.TrimSpace(m.Title) == "" {
		return m.Filename
	}
	return EscapeMarkdownLabel(m.Title)
}
