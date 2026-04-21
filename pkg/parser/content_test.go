package parser

import (
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
)

func TestExtractText_DraftJSFormat(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"simple text", `["Hello world","unstyled",[]]`, "Hello world"},
		{"text with styles", `["Bold text","unstyled",[[0,0,17]]]`, "Bold text"},
		{"empty array", `[]`, "[]"},
		{"empty string", "", ""},
		{"plain text fallback", "just plain text", "just plain text"},
		{"whitespace trimmed", `["  spaces  ","unstyled",[]]`, "spaces"},
		{"cyrillic", `["Тест или не тест вот в чём вопрос","unstyled",[]]`, "Тест или не тест вот в чём вопрос"},
		{"escaped quotes", `["Тир \"Тестовый для теста\"","unstyled",[]]`, `Тир "Тестовый для теста"`},
		{"newlines preserved", `["line1\nline2","unstyled",[]]`, "line1\nline2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractText(tt.raw)
			if got != tt.want {
				t.Errorf("ExtractText(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestBestMP4URL_SkipsEmptyURLs(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "ultra_hd", URL: ""},
		{Type: "full_hd", URL: "https://example.com/full_hd.mp4"},
		{Type: "high", URL: "https://example.com/high.mp4"},
	}
	got := BestMP4URL(urls)
	if got != "https://example.com/full_hd.mp4" {
		t.Errorf("BestMP4URL = %q, want full_hd URL", got)
	}
}

func TestBestMP4URL_SelectsHighestQuality(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "low", URL: "https://example.com/low.mp4"},
		{Type: "ultra_hd", URL: "https://example.com/ultra_hd.mp4"},
		{Type: "medium", URL: "https://example.com/medium.mp4"},
	}
	got := BestMP4URL(urls)
	if got != "https://example.com/ultra_hd.mp4" {
		t.Errorf("BestMP4URL = %q, want ultra_hd URL", got)
	}
}

func TestBestMP4URL_EmptyList(t *testing.T) {
	got := BestMP4URL(nil)
	if got != "" {
		t.Errorf("BestMP4URL(nil) = %q, want empty", got)
	}
}

func TestBestMP4URL_AllEmpty(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "ultra_hd", URL: ""},
		{Type: "full_hd", URL: ""},
	}
	got := BestMP4URL(urls)
	if got != "" {
		t.Errorf("BestMP4URL(all empty) = %q, want empty", got)
	}
}

// Regression: previously `bestRank` was initialised to 0 and the selection
// guard was `rank > bestRank`, so a "lowest"-only post (rank 0) was dropped
// and BestMP4URL returned "" instead of the only valid URL.
func TestBestMP4URL_OnlyLowest(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "lowest", URL: "https://example.com/lowest.mp4"},
	}
	got := BestMP4URL(urls)
	if got != "https://example.com/lowest.mp4" {
		t.Errorf("BestMP4URL(only lowest) = %q, want lowest URL", got)
	}
}

// Regression: with bestRank starting at -1, a "lowest" + "tiny" pair must
// still pick "tiny" — the highest-ranked entry wins, no matter where the
// floor was.
func TestBestMP4URL_LowestVsTiny(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "lowest", URL: "https://example.com/lowest.mp4"},
		{Type: "tiny", URL: "https://example.com/tiny.mp4"},
	}
	got := BestMP4URL(urls)
	if got != "https://example.com/tiny.mp4" {
		t.Errorf("BestMP4URL = %q, want tiny URL", got)
	}
}

func TestBestMP4URL_UnknownTypes(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "hls", URL: "https://example.com/video.m3u8"},
		{Type: "dash", URL: "https://example.com/video.mpd"},
		{Type: "low", URL: "https://example.com/low.mp4"},
	}
	got := BestMP4URL(urls)
	if got != "https://example.com/low.mp4" {
		t.Errorf("BestMP4URL = %q, want low URL (hls/dash should be ignored)", got)
	}
}

func TestParseBlocks_TextAndMedia(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["Hello","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
		{Type: "image", URL: "https://images.boosty.to/image/abc.jpg"},
		{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: "https://example.com/high.mp4"},
		}},
		{Type: "video", URL: "https://youtube.com/watch?v=123"},
		{Type: "link", URL: "https://example.com", Content: `["Click here","unstyled",[]]`},
	}

	result := ParseBlocks(blocks)

	if len(result.TextParts) != 2 {
		t.Fatalf("TextParts len = %d, want 2", len(result.TextParts))
	}
	if result.TextParts[0] != "Hello" {
		t.Errorf("TextParts[0] = %q, want 'Hello'", result.TextParts[0])
	}
	if result.TextParts[1] != "[Click here](https://example.com)" {
		t.Errorf("TextParts[1] = %q, want link markdown", result.TextParts[1])
	}

	if len(result.Media) != 3 {
		t.Fatalf("Media len = %d, want 3", len(result.Media))
	}
	if result.Media[0].Type != "image" || result.Media[0].Filename != "image_001.jpg" {
		t.Errorf("Media[0] = %+v, want image_001.jpg", result.Media[0])
	}
	if result.Media[1].Type != "video" || result.Media[1].Filename != "video_001.mp4" {
		t.Errorf("Media[1] = %+v, want video_001.mp4", result.Media[1])
	}
	if result.Media[2].Type != "external_video" {
		t.Errorf("Media[2].Type = %q, want external_video", result.Media[2].Type)
	}
}

func TestParseBlocks_SkipsBlockEnd(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["Real text","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
	}
	result := ParseBlocks(blocks)
	if len(result.TextParts) != 1 {
		t.Errorf("TextParts len = %d, want 1 (BLOCK_END should be skipped)", len(result.TextParts))
	}
}

func TestParseBlocks_SkipsEmptyImageURL(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "image", URL: ""},
	}
	result := ParseBlocks(blocks)
	if len(result.Media) != 0 {
		t.Errorf("Media len = %d, want 0 (empty URL should be skipped)", len(result.Media))
	}
}

func TestParseBlocks_ImageExtension(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "image", URL: "https://images.boosty.to/image/abc.png"},
		{Type: "image", URL: "https://images.boosty.to/image/no-ext"},
	}
	result := ParseBlocks(blocks)
	if result.Media[0].Filename != "image_001.png" {
		t.Errorf("Media[0].Filename = %q, want image_001.png", result.Media[0].Filename)
	}
	if result.Media[1].Filename != "image_002.jpg" {
		t.Errorf("Media[1].Filename = %q, want image_002.jpg (default ext)", result.Media[1].Filename)
	}
}

// Regression: Boosty image URLs are signed, so path.Ext on the full URL
// returned ".png?sig=..." (>5 chars) and the default ".jpg" was used for
// every signed PNG. After fix, the query string is stripped before Ext.
func TestParseBlocks_ImageExtensionWithQueryString(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "image", URL: "https://images.boosty.to/image/abc.png?sig=deadbeef&t=1234"},
		{Type: "image", URL: "https://images.boosty.to/image/abc.JPEG?x=1"},
	}
	result := ParseBlocks(blocks)
	if result.Media[0].Filename != "image_001.png" {
		t.Errorf("Media[0].Filename = %q, want image_001.png (query stripped)", result.Media[0].Filename)
	}
	if result.Media[1].Filename != "image_002.jpeg" {
		t.Errorf("Media[1].Filename = %q, want image_002.jpeg (lowercase, query stripped)", result.Media[1].Filename)
	}
}
