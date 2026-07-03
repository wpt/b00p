package parser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/wpt/b00p/pkg/boosty"
)

// MediaItem represents a downloadable media file from a post.
type MediaItem struct {
	Type     string // "image", "video", "external_video"
	URL      string
	Filename string
}

// ParsedContent is the result of parsing a post's content blocks.
type ParsedContent struct {
	TextParts []string
	Media     []MediaItem
	// SkippedVideos counts ok_video blocks that had no usable MP4 URL
	// (only HLS/DASH variants, or every MP4 URL was empty). The block was
	// silently dropped — without an explicit count, the user gets fewer
	// videos than the source had and no warning that anything is missing.
	// Callers should log when SkippedVideos > 0 so the user can decide to
	// grab the missing video via yt-dlp on the HLS source or report it.
	SkippedVideos int
	// UnknownTypes lists block types ParseBlocks does not handle (unique,
	// in first-seen order). Boosty keeps shipping new content kinds, and
	// silently dropping one would leave an invisible hole in the archive.
	// Callers should log the list so the user learns support is missing
	// the moment it matters.
	UnknownTypes []string
}

// mp4QualityRank defines preference order for direct MP4 formats
// (higher = better). Unexported so library callers cannot mutate parser
// behavior globally; if external customisation is ever needed it should be
// a function option, not a public mutable map.
var mp4QualityRank = map[string]int{
	"lowest":   0,
	"tiny":     1,
	"low":      2,
	"medium":   3,
	"high":     4,
	"full_hd":  5,
	"quad_hd":  6,
	"ultra_hd": 7,
}

// ExtractText pulls the human-readable string out of Boosty's Draft.js content format.
// Content comes as a JSON array: ["text", "unstyled", [...styles]]
// We only need the first element (the actual text). Run-boundary whitespace
// inside it is preserved: one visual paragraph arrives as several runs
// (text("see "), link(...), text(" for details")) and the spacing that joins
// them lives at the run edges — trimming here would glue "see" to the link.
// ParseBlocks trims the assembled paragraph instead.
func ExtractText(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil && len(arr) > 0 {
			var text string
			if err := json.Unmarshal(arr[0], &text); err == nil {
				return text
			}
		}
	}
	return trimmed
}

// ParseBlocks extracts text and media from a post's content blocks.
//
// Text flow: the API delivers one visual paragraph as several consecutive
// runs — e.g. text("see "), link("this post"), text(" for details") — closed
// by a text block whose modificator is "BLOCK_END". Runs accumulate into one
// TextParts entry and flush at each BLOCK_END; media and unknown blocks also
// flush, so a paragraph can never span across an image. Joining is verbatim
// (the author's spacing lives at the run edges), with a single trim of the
// assembled paragraph.
func ParseBlocks(blocks []boosty.ContentBlock) ParsedContent {
	var result ParsedContent
	var imgIdx, vidIdx int
	var para []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		joined := strings.TrimSpace(strings.Join(para, ""))
		para = para[:0]
		if joined != "" {
			result.TextParts = append(result.TextParts, joined)
		}
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Modificator == "BLOCK_END" {
				flushPara()
				continue
			}
			if text := ExtractText(block.Content); text != "" {
				para = append(para, text)
			}

		case "image":
			flushPara()
			imgIdx++
			imgURL := block.URL
			if imgURL == "" {
				continue
			}
			ext := imageExt(imgURL)
			filename := fmt.Sprintf("image_%03d%s", imgIdx, ext)
			result.Media = append(result.Media, MediaItem{
				Type:     "image",
				URL:      imgURL,
				Filename: filename,
			})

		case "ok_video":
			flushPara()
			vidIdx++
			vidURL := BestMP4URL(block.PlayerURLs)
			if vidURL == "" {
				result.SkippedVideos++
				continue
			}
			filename := fmt.Sprintf("video_%03d.mp4", vidIdx)
			result.Media = append(result.Media, MediaItem{
				Type:     "video",
				URL:      vidURL,
				Filename: filename,
			})

		case "video":
			flushPara()
			vidIdx++
			if block.URL != "" {
				result.Media = append(result.Media, MediaItem{
					Type:     "external_video",
					URL:      block.URL,
					Filename: fmt.Sprintf("external_video_%03d", vidIdx),
				})
			}

		case "link":
			text := ExtractText(block.Content)
			if strings.TrimSpace(text) == "" {
				text = block.URL
			}
			if text == "" {
				// Neither label nor URL — nothing to archive.
				continue
			}
			// formatMarkdownLink degrades to the bare escaped label when the
			// URL is empty (editor artifact, dead link) — the author's text
			// must not vanish from the archive with it.
			para = append(para, formatMarkdownLink(text, block.URL))

		default:
			flushPara()
			if !slices.Contains(result.UnknownTypes, block.Type) {
				result.UnknownTypes = append(result.UnknownTypes, block.Type)
			}
		}
	}
	flushPara()

	return result
}

// imageExt picks an extension for a downloaded image. Boosty image URLs are
// signed, so naive path.Ext("...png?sig=...") returns ".png?sig=..." and the
// previous code fell back to ".jpg" for every signed URL. Strip query/fragment
// first, then accept only the formats Boosty's CDN actually serves; anything
// else (missing, query-polluted, or an FS-unsafe extension) falls back to ".jpg".
func imageExt(s string) string {
	switch ext := urlPathExt(s); ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif":
		return ext
	default:
		return ".jpg"
	}
}

// urlPathExt returns the lowercased file extension of a URL's path, stripping
// any query/fragment first. Boosty signs its media URLs, so a naive
// path.Ext("...mp4?sig=...") would return ".mp4?sig=..."; parsing to the path
// avoids that. Returns "" when the path has no extension.
func urlPathExt(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	return strings.ToLower(path.Ext(raw))
}

// BestMP4URL selects the highest quality direct MP4 URL from player URLs.
// bestRank starts at -1 so the lowest-rank "lowest" URL (rank 0) is selectable
// when nothing better is available.
func BestMP4URL(urls []boosty.PlayerURL) string {
	var best string
	var fallback string
	bestRank := -1

	for _, u := range urls {
		if u.URL == "" {
			continue
		}
		rank, ok := mp4QualityRank[u.Type]
		if ok && rank > bestRank {
			best = u.URL
			bestRank = rank
			continue
		}
		if !ok && fallback == "" && isDirectMP4URL(u.URL) {
			fallback = u.URL
		}
	}
	if best == "" {
		return fallback
	}
	return best
}

func isDirectMP4URL(raw string) bool {
	return urlPathExt(raw) == ".mp4"
}

// formatMarkdownLink renders an author-supplied link (post link blocks and the
// external-video reference) as markdown. The label is escaped and its
// line-breaking whitespace collapsed (EscapeMarkdownLabel) so a stray ']' / '\'
// or an embedded newline cannot terminate the link early; the destination is
// wrapped in <...> with angle brackets and control bytes percent-escaped so
// spaces/parens (legal inside <...>) stay verbatim while a '<'/'>' or a
// paste-artifact control char cannot break the markdown. No scheme filtering:
// this is the user's own local archive (no XSS surface), so a mailto:/tg:/ftp:
// link keeps its URL instead of being silently dropped.
func formatMarkdownLink(text, rawURL string) string {
	dest := strings.TrimSpace(rawURL)
	if dest == "" {
		// No destination at all — emit the escaped label alone rather than a
		// broken "[label](<>)".
		return EscapeMarkdownLabel(text)
	}
	return fmt.Sprintf("[%s](<%s>)", EscapeMarkdownLabel(text), escapeMarkdownDestination(dest))
}

// escapeMarkdownDestination neutralises the characters that can break a <...>
// markdown destination: the angle brackets, a backslash (CommonMark treats it
// as an escape inside <...>, so a trailing '\' would escape the closing '>' and
// leave the link unterminated), and any control byte (a stray \n/\r/\t from a
// paste artifact would otherwise split the link or end the line). Each is
// percent-escaped over its UTF-8 bytes so the URL is preserved rather than
// dropped.
func escapeMarkdownDestination(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '<':
			b.WriteString("%3C")
		case r == '>':
			b.WriteString("%3E")
		case r == '\\':
			b.WriteString("%5C")
		case unicode.IsControl(r):
			for _, by := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

var markdownLabelReplacer = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`)

// EscapeMarkdownLabel makes author-supplied text safe as the label in a
// [label](dest) markdown link: line-breaking whitespace is collapsed to single
// spaces (a raw newline would otherwise split the link across lines) and the
// metacharacters that can terminate the label early (`\`, `[`, `]`) are
// backslash-escaped. Shared by the parser's link / external-video rendering
// and the syncer's blog index so the rule lives in one place.
func EscapeMarkdownLabel(s string) string {
	return markdownLabelReplacer.Replace(collapseWhitespace(s))
}

// collapseWhitespace flattens every whitespace run — including \r\n — to a
// single space and trims the ends (strings.Fields drops leading/trailing
// whitespace; Join re-inserts one space only between fields). One home for
// the rule shared by markdown labels, the H1 title line, and directory-name
// sanitizing so the collapse semantics cannot drift between them.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
