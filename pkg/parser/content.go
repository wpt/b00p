package parser

import (
	"encoding/json"
	"fmt"
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

// MP4QualityRank defines preference order for direct MP4 formats (higher = better).
var MP4QualityRank = map[string]int{
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
			url := block.URL
			if url == "" {
				continue
			}
			ext := path.Ext(url)
			if ext == "" || len(ext) > 5 {
				ext = ".jpg"
			}
			filename := fmt.Sprintf("image_%03d%s", imgIdx, ext)
			result.Media = append(result.Media, MediaItem{
				Type:     "image",
				URL:      url,
				Filename: filename,
			})

		case "ok_video":
			vidIdx++
			url := BestMP4URL(block.PlayerURLs)
			if url == "" {
				continue
			}
			filename := fmt.Sprintf("video_%03d.mp4", vidIdx)
			result.Media = append(result.Media, MediaItem{
				Type:     "video",
				URL:      url,
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

// BestMP4URL selects the highest quality direct MP4 URL from player URLs.
func BestMP4URL(urls []boosty.PlayerURL) string {
	var best string
	var bestRank int

	for _, u := range urls {
		if u.URL == "" {
			continue
		}
		rank, ok := MP4QualityRank[u.Type]
		if ok && rank > bestRank {
			best = u.URL
			bestRank = rank
		}
	}
	return best
}
