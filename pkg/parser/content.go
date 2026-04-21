package parser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

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
// We only need the first element (the actual text).
func ExtractText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
			var text string
			if err := json.Unmarshal(arr[0], &text); err == nil {
				return strings.TrimSpace(text)
			}
		}
	}
	return raw
}

// ParseBlocks extracts text and media from a post's content blocks.
func ParseBlocks(blocks []boosty.ContentBlock) ParsedContent {
	var result ParsedContent
	var imgIdx, vidIdx int

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Modificator == "BLOCK_END" {
				continue
			}
			text := ExtractText(block.Content)
			if text != "" {
				result.TextParts = append(result.TextParts, text)
			}

		case "image":
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
			vidIdx++
			vidURL := BestMP4URL(block.PlayerURLs)
			if vidURL == "" {
				continue
			}
			filename := fmt.Sprintf("video_%03d.mp4", vidIdx)
			result.Media = append(result.Media, MediaItem{
				Type:     "video",
				URL:      vidURL,
				Filename: filename,
			})

		case "video":
			vidIdx++
			if block.URL != "" {
				result.Media = append(result.Media, MediaItem{
					Type:     "external_video",
					URL:      block.URL,
					Filename: fmt.Sprintf("external_video_%03d", vidIdx),
				})
			}

		case "link":
			if block.URL != "" {
				text := ExtractText(block.Content)
				if text == "" {
					text = block.URL
				}
				result.TextParts = append(result.TextParts, fmt.Sprintf("[%s](%s)", text, block.URL))
			}
		}
	}

	return result
}

// imageExt picks an extension for a downloaded image. Boosty image URLs are
// signed, so naive path.Ext("...png?sig=...") returns ".png?sig=..." (too long
// to be a real extension) and the previous code fell back to ".jpg" for every
// signed URL. Strip query/fragment first, then fall back to ".jpg" only on
// genuinely missing or implausible extensions.
func imageExt(s string) string {
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		s = u.Path
	}
	ext := path.Ext(s)
	if ext == "" || len(ext) > 5 {
		return ".jpg"
	}
	return strings.ToLower(ext)
}

// BestMP4URL selects the highest quality direct MP4 URL from player URLs.
// bestRank starts at -1 so the lowest-rank "lowest" URL (rank 0) is selectable
// when nothing better is available.
func BestMP4URL(urls []boosty.PlayerURL) string {
	var best string
	bestRank := -1

	for _, u := range urls {
		if u.URL == "" {
			continue
		}
		rank, ok := mp4QualityRank[u.Type]
		if ok && rank > bestRank {
			best = u.URL
			bestRank = rank
		}
	}
	return best
}
