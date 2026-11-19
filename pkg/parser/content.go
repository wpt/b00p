package parser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/wpt/b00p/pkg/boosty"
)

// MediaItem represents a downloadable media file from a post.
type MediaItem struct {
	Type string // "image", "video", "external_video", "audio", "file"
	URL  string
	// Filename is the file's name in the post directory. Empty for
	// external_video: yt-dlp names those after the remote video.
	Filename string
	// ID is the Boosty block id of the media object. Stable across API
	// fetches while the signed URL is not, so downloads key their resume
	// sidecar on it (boosty.DownloadRequest.Key). Empty when the block had
	// no id.
	ID string
	// Title is the author-supplied display name for audio/file attachments
	// (the original filename as uploaded); used as the link label in
	// generated markdown. Empty for other media types.
	Title string
	// TextBefore is the number of ParsedContent.TextParts entries that come
	// before the item in block order; GenerateMarkdown places the item after
	// them. ParseBlocks sets it. A caller building ParsedContent by hand gets
	// the zero value, which puts the item before all text.
	TextBefore int
}

// ParsedContent is the result of parsing a post's content blocks.
type ParsedContent struct {
	// TextParts are the post's text blocks as markdown, in reading order:
	// paragraphs, headings ("## ...") and lists (one entry per list).
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
	// in first-seen order; "list/<type>" for one inside a list item).
	// Boosty keeps shipping new content kinds, and silently dropping one
	// would leave an invisible hole in post.md. Callers should log the list
	// so the user learns support is missing the moment it matters.
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
	text, _ := parseDraft(raw)
	return text
}

// parseDraft splits Draft.js content into its text and its block style, the
// second element ("unstyled", "header-two", ...). Content that is not such an
// array is the text itself, trimmed, with no style.
func parseDraft(raw string) (text, style string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil && len(arr) > 0 {
			if err := json.Unmarshal(arr[0], &text); err == nil {
				if len(arr) > 1 && json.Unmarshal(arr[1], &style) != nil {
					style = ""
				}
				return text, style
			}
		}
	}
	return trimmed, ""
}

// ParseBlocks extracts text and media from a post's content blocks.
//
// Text flow: the API delivers one visual paragraph as several consecutive
// inline runs (text, link and smile blocks) — e.g. text("see "),
// link("this post"), text(" for details") — closed by a text block whose
// modificator is "BLOCK_END". Runs accumulate into one TextParts entry and
// flush at each BLOCK_END; every other block also flushes, so a paragraph can
// never span across an image. Joining is verbatim (the author's spacing lives
// at the run edges), with a single trim of the assembled paragraph. Header and
// list blocks become TextParts entries of their own.
func ParseBlocks(blocks []boosty.ContentBlock) ParsedContent {
	var result ParsedContent
	var imgIdx, vidIdx, audIdx, fileIdx int
	var para []string

	addText := func(s string) {
		if s != "" {
			result.TextParts = append(result.TextParts, s)
		}
	}
	flushPara := func() {
		addText(joinRuns(para))
		para = para[:0]
	}
	// Every media block is preceded by a flush, so the open paragraph is
	// already counted and TextBefore is exact.
	addMedia := func(m MediaItem) {
		m.TextBefore = len(result.TextParts)
		result.Media = append(result.Media, m)
	}
	unknown := func(typ string) {
		if !slices.Contains(result.UnknownTypes, typ) {
			result.UnknownTypes = append(result.UnknownTypes, typ)
		}
	}

	for _, block := range blocks {
		if isBlockEnd(block) {
			if run, _ := inlineRun(block); run != "" {
				para = append(para, run)
			}
			flushPara()
			continue
		}
		if run, ok := inlineRun(block); ok {
			para = append(para, run)
			continue
		}
		flushPara()

		switch block.Type {
		case "header":
			addText(heading(block.Content))

		case "list":
			addText(renderList(block, unknown))

		case "image":
			imgIdx++
			imgURL := block.URL
			if imgURL == "" {
				continue
			}
			ext := imageExt(imgURL)
			filename := fmt.Sprintf("image_%03d%s", imgIdx, ext)
			addMedia(MediaItem{
				Type:     "image",
				URL:      imgURL,
				Filename: filename,
				ID:       block.ID,
			})

		case "ok_video":
			vidIdx++
			vidURL := BestMP4URL(block.PlayerURLs)
			if vidURL == "" {
				result.SkippedVideos++
				continue
			}
			filename := fmt.Sprintf("video_%03d.mp4", vidIdx)
			addMedia(MediaItem{
				Type:     "video",
				URL:      vidURL,
				Filename: filename,
				ID:       block.ID,
			})

		case "video":
			// Still counts: skipping it would renumber the native
			// video_NNN.mp4 files of existing archives.
			vidIdx++
			if block.URL != "" {
				addMedia(MediaItem{
					Type: "external_video",
					URL:  block.URL,
				})
			}

		case "audio_file":
			audIdx++
			if block.URL == "" {
				continue
			}
			addMedia(MediaItem{
				Type:     "audio",
				URL:      block.URL,
				Title:    block.Title,
				Filename: fmt.Sprintf("audio_%03d%s", audIdx, attachmentExt(block.Title, block.URL, ".mp3")),
				ID:       block.ID,
			})

		case "file":
			fileIdx++
			if block.URL == "" {
				continue
			}
			addMedia(MediaItem{
				Type:     "file",
				URL:      block.URL,
				Title:    block.Title,
				Filename: fmt.Sprintf("file_%03d%s", fileIdx, attachmentExt(block.Title, block.URL, "")),
				ID:       block.ID,
			})

		default:
			unknown(block.Type)
		}
	}
	flushPara()

	return result
}

func isBlockEnd(block boosty.ContentBlock) bool {
	return block.Type == "text" && block.Modificator == "BLOCK_END"
}

// inlineRun renders a block that sits inside a paragraph as markdown;
// ok=false for any other block type. A smile renders as :name:, so a
// sentence with an emoticon in it stays one paragraph.
func inlineRun(block boosty.ContentBlock) (run string, ok bool) {
	switch block.Type {
	case "text":
		return ExtractText(block.Content), true
	case "link":
		return linkRun(block), true
	case "smile":
		if block.Name == "" {
			return "", true
		}
		return ":" + block.Name + ":", true
	}
	return "", false
}

// linkRun renders a link block. formatMarkdownLink degrades to the bare
// escaped label when the URL is empty (editor artifact, dead link) — the
// author's text must not vanish from the archive with it. The label's edge
// whitespace is the spacing to the neighbouring runs, which
// EscapeMarkdownLabel would trim, so it stays outside the link.
func linkRun(block boosty.ContentBlock) string {
	text := ExtractText(block.Content)
	label := strings.TrimSpace(text)
	lead := text[:len(text)-len(strings.TrimLeftFunc(text, unicode.IsSpace))]
	trail := text[len(lead)+len(label):]
	if label == "" {
		label = block.URL
	}
	if label == "" {
		// Neither label nor URL — nothing to archive but the spacing.
		return lead
	}
	return lead + formatMarkdownLink(label, block.URL) + trail
}

var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// joinRuns assembles a paragraph from its inline runs. Boosty sends a line
// break typed inside a paragraph as "\n" in a run, which CommonMark renders
// as a space, so a lone "\n" between two lines of text becomes a hard break.
// Two trailing spaces, not a backslash: before a line that opens a block
// ("- ", "1. ", "#") a backslash shows up literally.
func joinRuns(runs []string) string {
	s := strings.TrimSpace(lineEndings.Replace(strings.Join(runs, "")))
	lines := strings.Split(s, "\n")
	for i := range len(lines) - 1 {
		if !isBlankLine(lines[i]) && !isBlankLine(lines[i+1]) {
			lines[i] += "  "
		}
	}
	return strings.Join(lines, "\n")
}

// isBlankLine uses CommonMark's definition: only spaces and tabs, so a line
// holding an NBSP is text.
func isBlankLine(s string) bool {
	return strings.Trim(s, " \t") == ""
}

// draftHeadingLevels maps the Draft.js block style of a header block's
// content (["Title", "header-two", [...]]) to its heading level.
var draftHeadingLevels = map[string]int{
	"header-one":   1,
	"header-two":   2,
	"header-three": 3,
	"header-four":  4,
	"header-five":  5,
	"header-six":   6,
}

// heading renders a header block as a one-line ATX heading. A header whose
// style names no level stays a plain paragraph.
func heading(content string) string {
	text, style := parseDraft(content)
	text = collapseWhitespace(text)
	if level := draftHeadingLevels[style]; level > 0 && text != "" {
		return strings.Repeat("#", level) + " " + text
	}
	return text
}

// renderList renders a list block as markdown: "N. " items for the "ordered"
// style, "- " for anything else, nested items indented to their parent's
// text. An item's Data holds inline runs, and a BLOCK_END inside it breaks
// the line. Any other block in an item is reported to unknown as
// "list/<type>": its content is not rendered.
func renderList(block boosty.ContentBlock, unknown func(string)) string {
	var lines []string
	var walk func(items []boosty.ListItem, indent string)
	walk = func(items []boosty.ListItem, indent string) {
		for i, item := range items {
			marker := "- "
			if block.Style == "ordered" {
				marker = strconv.Itoa(i+1) + ". "
			}
			cont := indent + strings.Repeat(" ", len(marker))
			for j, line := range strings.Split(itemText(item.Data, unknown), "\n") {
				switch {
				case j == 0 && line == "":
					lines = append(lines, indent+strings.TrimSuffix(marker, " "))
				case j == 0:
					lines = append(lines, indent+marker+line)
				case strings.Trim(line, " \t") == "":
					lines = append(lines, "")
				default:
					lines = append(lines, cont+line)
				}
			}
			walk(item.Items, cont)
		}
	}
	walk(block.Items, "")
	return strings.Join(lines, "\n")
}

// itemText assembles a list item's inline runs into its text.
func itemText(data []boosty.ContentBlock, unknown func(string)) string {
	var runs []string
	for _, block := range data {
		if isBlockEnd(block) {
			if run, _ := inlineRun(block); run != "" {
				runs = append(runs, run)
			}
			runs = append(runs, "\n")
			continue
		}
		run, ok := inlineRun(block)
		if !ok {
			unknown("list/" + block.Type)
			continue
		}
		runs = append(runs, run)
	}
	return joinRuns(runs)
}

// imageExt picks an extension for a downloaded image. Boosty image URLs are
// signed, so a naive path.Ext("...png?sig=...") returns ".png?sig=..." and
// every signed image would land as .jpg. Strip query/fragment first, then
// accept only the formats Boosty's CDN actually serves; anything else
// (missing, query-polluted, or an FS-unsafe extension) falls back to ".jpg".
func imageExt(s string) string {
	switch ext := urlPathExt(s); ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif":
		return ext
	default:
		return ".jpg"
	}
}

// plausibleExtRe bounds what attachmentExt will accept as a filename
// extension: a dot followed by 1-10 lowercased alphanumerics. Anything else
// (spaces, FS-unsafe characters, ".v2 final"-style non-extensions) is
// author-controlled noise that must not end up in an on-disk filename.
var plausibleExtRe = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

// attachmentExt picks the filename extension for an audio/file attachment:
// the author-supplied title's extension when plausible, else the URL path's,
// else fallback. Titles are the original upload names ("report.pdf"), so the
// title extension is usually the truthful one; the URL path often has none.
func attachmentExt(title, url, fallback string) string {
	if ext := strings.ToLower(path.Ext(title)); plausibleExtRe.MatchString(ext) {
		return ext
	}
	if ext := urlPathExt(url); plausibleExtRe.MatchString(ext) {
		return ext
	}
	return fallback
}

// ApplySignedQuery attaches the post-level signedQuery to media items whose
// URLs the API serves unsigned — audio and file attachments. Image and video
// URLs come pre-signed per block, and external videos are third-party links,
// so those are left untouched, as is any URL that already carries a query
// string. The API sends signedQuery with a leading "?"; a bare query string
// is accepted too. Call it after ParseBlocks, passing Post.SignedQuery, or
// attachment downloads will 4xx.
func ApplySignedQuery(media []MediaItem, signedQuery string) {
	if signedQuery == "" {
		return
	}
	if !strings.HasPrefix(signedQuery, "?") {
		signedQuery = "?" + signedQuery
	}
	for i := range media {
		if media[i].Type != "audio" && media[i].Type != "file" {
			continue
		}
		if media[i].URL == "" || strings.Contains(media[i].URL, "?") {
			continue
		}
		media[i].URL += signedQuery
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
