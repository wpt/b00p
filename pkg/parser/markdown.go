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

	// Body in block order: each media reference goes after the TextBefore
	// text entries that precede it. Media left over (TextBefore past the
	// end, possible only in a hand-built ParsedContent) goes last.
	mi := 0
	for i, text := range parsed.TextParts {
		for ; mi < len(parsed.Media) && parsed.Media[mi].TextBefore <= i; mi++ {
			writeMediaRef(&b, parsed.Media[mi])
		}
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	for _, m := range parsed.Media[mi:] {
		writeMediaRef(&b, m)
	}

	return b.String()
}

// writeMediaRef writes the markdown reference to one media item.
func writeMediaRef(b *strings.Builder, m MediaItem) {
	switch m.Type {
	case "image":
		// m.Filename is parser-generated ("image_001.jpg") — no markdown
		// metacharacters possible, so no escaping is needed.
		fmt.Fprintf(b, "![%s](%s)\n\n", m.Filename, m.Filename)
	case "video":
		fmt.Fprintf(b, "[Video: %s](%s)\n\n", m.Filename, m.Filename)
	case "audio":
		fmt.Fprintf(b, "[Audio: %s](%s)\n\n", attachmentLabel(m), m.Filename)
	case "file":
		fmt.Fprintf(b, "[File: %s](%s)\n\n", attachmentLabel(m), m.Filename)
	case "external_video":
		b.WriteString(formatMarkdownLink("External Video", m.URL))
		b.WriteString("\n\n")
	}
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
