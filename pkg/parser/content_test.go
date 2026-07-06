package parser

import (
	"fmt"
	"slices"
	"strings"
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
		// Run-boundary whitespace is preserved — one paragraph arrives as
		// several runs and the joining spaces live at the run edges;
		// ParseBlocks trims the assembled paragraph instead.
		{"run whitespace preserved", `["  spaces  ","unstyled",[]]`, "  spaces  "},
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

func TestBestMP4URL_FallsBackToUnknownDirectMP4(t *testing.T) {
	urls := []boosty.PlayerURL{
		{Type: "hls", URL: "https://example.com/video.m3u8"},
		{Type: "source", URL: "https://cdn.example.com/video.mp4?sig=1"},
	}
	got := BestMP4URL(urls)
	if got != "https://cdn.example.com/video.mp4?sig=1" {
		t.Errorf("BestMP4URL = %q, want unknown direct mp4 fallback", got)
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
	if result.TextParts[1] != "[Click here](<https://example.com>)" {
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

func TestParseBlocks_BlockEndTerminatesParagraph(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["Real text","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
		{Type: "text", Modificator: "BLOCK_END"}, // empty paragraph — dropped
	}
	result := ParseBlocks(blocks)
	if len(result.TextParts) != 1 {
		t.Errorf("TextParts len = %d, want 1 (BLOCK_END terminates, empty paragraphs dropped)", len(result.TextParts))
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

// An ok_video block with no usable MP4 (HLS/DASH-only variants, a real API
// condition) is dropped from Media but must be COUNTED — SkippedVideos feeds
// the "videos skipped" warning in the syncer, the only user-visible signal
// that the post's video did not land on disk.
func TestParseBlocks_CountsSkippedVideos(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "hls", URL: "https://example.com/playlist.m3u8"},
			{Type: "dash", URL: "https://example.com/manifest.mpd"},
			{Type: "high", URL: ""}, // empty MP4 variant
		}},
		{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: "https://example.com/v.mp4"},
		}},
	}
	result := ParseBlocks(blocks)
	if result.SkippedVideos != 1 {
		t.Errorf("SkippedVideos = %d, want 1", result.SkippedVideos)
	}
	if len(result.Media) != 1 || result.Media[0].URL != "https://example.com/v.mp4" {
		t.Errorf("Media = %+v, want exactly the one downloadable video", result.Media)
	}
}

func TestParseBlocks_NoSkippedVideosOnHappyPath(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{
			{Type: "high", URL: "https://example.com/v.mp4"},
		}},
	}
	if got := ParseBlocks(blocks).SkippedVideos; got != 0 {
		t.Errorf("SkippedVideos = %d, want 0", got)
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

func TestParseBlocks_ImageExtensionWhitelist(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "image", URL: "https://images.boosty.to/image/abc."},
		{Type: "image", URL: "https://images.boosty.to/image/abc.jp:g"},
	}
	result := ParseBlocks(blocks)
	if len(result.Media) != len(blocks) {
		t.Fatalf("Media len = %d, want %d", len(result.Media), len(blocks))
	}
	for i, m := range result.Media {
		if m.Filename != fmt.Sprintf("image_%03d.jpg", i+1) {
			t.Errorf("Media[%d].Filename = %q, want .jpg fallback", i, m.Filename)
		}
	}
}

func TestParseBlocks_LinkMarkdownSafety(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "link", URL: "https://example.com/a path/(x)", Content: `["[Click] \\ here","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
		{Type: "link", URL: "javascript:alert(1)", Content: `["bad","unstyled",[]]`},
	}
	result := ParseBlocks(blocks)
	// Label brackets/backslashes are escaped so they can't break the link; the
	// destination is wrapped in <...>. Spaces/parens are legal inside angle
	// brackets and kept verbatim.
	if got, want := result.TextParts[0], `[\[Click\] \\ here](<https://example.com/a path/(x)>)`; got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
	// No scheme filtering — a non-http(s) URL keeps its link (local archive,
	// no XSS surface); it is not dropped to a bare label.
	if got, want := result.TextParts[1], `[bad](<javascript:alert(1)>)`; got != want {
		t.Errorf("non-http link = %q, want %q", got, want)
	}
}

func TestParseBlocks_LinkLabelNewlineCollapsed(t *testing.T) {
	// A newline inside the link label would split the markdown link across two
	// lines; EscapeMarkdownLabel collapses it to a single space so the link
	// stays intact on one line.
	blocks := []boosty.ContentBlock{
		{Type: "link", URL: "https://example.com", Content: `["Click\nhere","unstyled",[]]`},
	}
	result := ParseBlocks(blocks)
	if got, want := result.TextParts[0], `[Click here](<https://example.com>)`; got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
}

func TestParseBlocks_LinkURLControlCharPreserved(t *testing.T) {
	// A control byte in the URL must not drop the destination — it is
	// percent-escaped over its UTF-8 bytes so the link (and the archived URL)
	// survives instead of degrading to a bare label.
	blocks := []boosty.ContentBlock{
		{Type: "link", URL: "https://example.com/a\tb", Content: `["x","unstyled",[]]`},
	}
	result := ParseBlocks(blocks)
	if got, want := result.TextParts[0], `[x](<https://example.com/a%09b>)`; got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
}

func TestParseBlocks_LinkURLBackslashEscaped(t *testing.T) {
	// A trailing backslash in the destination would otherwise escape the
	// closing '>' inside <...> (CommonMark) and leave the link unterminated; it
	// is percent-escaped to %5C so the link stays closed.
	blocks := []boosty.ContentBlock{
		{Type: "link", URL: `https://example.com/path\`, Content: `["x","unstyled",[]]`},
	}
	result := ParseBlocks(blocks)
	if got, want := result.TextParts[0], `[x](<https://example.com/path%5C>)`; got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
}

// Unknown block types are collected (unique, first-seen order) instead of
// being silently dropped — the syncer logs them so the user learns the
// moment a blog uses a content kind b00p does not support yet (whatever
// Boosty ships next).
func TestParseBlocks_CollectsUnknownTypes(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["hi","unstyled",[]]`},
		{Type: "poll", ID: "p1"},
		{Type: "poll", ID: "p2"},
		{Type: "some_future_kind", URL: "https://example.com/x"},
	}
	got := ParseBlocks(blocks)
	want := []string{"poll", "some_future_kind"}
	if !slices.Equal(got.UnknownTypes, want) {
		t.Errorf("UnknownTypes = %v, want %v (unique, first-seen order)", got.UnknownTypes, want)
	}
}

func TestParseBlocks_AudioAndFileBlocks(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "audio_file", URL: "https://example.com/media/1", Title: "Episode 5.MP3"},
		{Type: "audio_file", URL: ""},                            // no URL — skipped, still counted in numbering
		{Type: "audio_file", URL: "https://example.com/media/2"}, // no title, no URL ext → .mp3 fallback
		{Type: "file", URL: "https://example.com/media/3", Title: "report.pdf"},
		{Type: "file", URL: "https://example.com/media/4.zip", Title: "notes.v2 final"}, // hostile title ext → URL ext
		{Type: "file", URL: "https://example.com/media/5", Title: "no extension"},       // nothing plausible → no ext
	}
	result := ParseBlocks(blocks)

	want := []MediaItem{
		{Type: "audio", URL: "https://example.com/media/1", Title: "Episode 5.MP3", Filename: "audio_001.mp3"},
		{Type: "audio", URL: "https://example.com/media/2", Filename: "audio_003.mp3"},
		{Type: "file", URL: "https://example.com/media/3", Title: "report.pdf", Filename: "file_001.pdf"},
		{Type: "file", URL: "https://example.com/media/4.zip", Title: "notes.v2 final", Filename: "file_002.zip"},
		{Type: "file", URL: "https://example.com/media/5", Title: "no extension", Filename: "file_003"},
	}
	if !slices.Equal(result.Media, want) {
		t.Errorf("Media = %+v\nwant %+v", result.Media, want)
	}
	if len(result.UnknownTypes) != 0 {
		t.Errorf("UnknownTypes = %v, want empty (audio_file and file are supported)", result.UnknownTypes)
	}
}

func TestApplySignedQuery(t *testing.T) {
	media := []MediaItem{
		{Type: "audio", URL: "https://example.com/media/1", Filename: "audio_001.mp3"},
		{Type: "file", URL: "https://example.com/media/2", Filename: "file_001.pdf"},
		{Type: "file", URL: "https://example.com/media/3?sig=already", Filename: "file_002.pdf"},
		{Type: "image", URL: "https://example.com/i.jpg", Filename: "image_001.jpg"},
		{Type: "video", URL: "https://example.com/v.mp4", Filename: "video_001.mp4"},
		{Type: "external_video", URL: "https://example.com/watch", Filename: "external_video_001"},
	}

	ApplySignedQuery(media, "?sign=abc")

	// Only unsigned audio/file URLs gain the query.
	if got, want := media[0].URL, "https://example.com/media/1?sign=abc"; got != want {
		t.Errorf("audio URL = %q, want %q", got, want)
	}
	if got, want := media[1].URL, "https://example.com/media/2?sign=abc"; got != want {
		t.Errorf("file URL = %q, want %q", got, want)
	}
	// Already-signed and non-attachment URLs stay untouched.
	for _, i := range []int{2, 3, 4, 5} {
		if strings.Contains(media[i].URL, "sign=abc") {
			t.Errorf("media[%d] (%s) URL gained signed query: %q", i, media[i].Type, media[i].URL)
		}
	}
}

func TestApplySignedQuery_BareQueryAndEmpty(t *testing.T) {
	media := []MediaItem{{Type: "file", URL: "https://example.com/media/1"}}
	ApplySignedQuery(media, "sign=abc") // no leading "?" — must be added
	if got, want := media[0].URL, "https://example.com/media/1?sign=abc"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	media = []MediaItem{{Type: "file", URL: "https://example.com/media/2"}}
	ApplySignedQuery(media, "")
	if got, want := media[0].URL, "https://example.com/media/2"; got != want {
		t.Errorf("URL = %q, want %q (empty signedQuery must be a no-op)", got, want)
	}
}

// One visual paragraph arrives as several runs closed by a BLOCK_END marker.
// Fixture mirrors a real payload: text("Утиный бусти: ") + link + BLOCK_END is
// ONE line the author wrote — it must land in one TextParts entry with the
// separating space preserved, not split into two paragraphs. (Verified
// against a live post.json; the pre-fix parser emitted three paragraphs for
// a mid-sentence link and ate the run-boundary spacing.)
func TestParseBlocks_AssemblesParagraphs(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["see ","unstyled",[]]`},
		{Type: "link", URL: "https://example.com", Content: `["this post","unstyled",[]]`},
		{Type: "text", Content: `[" for details","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
		{Type: "text", Content: `["next paragraph","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
	}
	result := ParseBlocks(blocks)
	want := []string{
		`see [this post](<https://example.com>) for details`,
		`next paragraph`,
	}
	if !slices.Equal(result.TextParts, want) {
		t.Errorf("TextParts = %q, want %q", result.TextParts, want)
	}
}

// A trailing paragraph without a closing BLOCK_END must still flush, and a
// media block acts as a paragraph boundary so text can never merge across it.
func TestParseBlocks_FlushesOnMediaAndAtEnd(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["before","unstyled",[]]`},
		{Type: "image", URL: "https://images.boosty.to/image/x.jpg"},
		{Type: "text", Content: `["after","unstyled",[]]`},
	}
	result := ParseBlocks(blocks)
	want := []string{"before", "after"}
	if !slices.Equal(result.TextParts, want) {
		t.Errorf("TextParts = %q, want %q", result.TextParts, want)
	}
}

// A link block with an empty URL (editor artifact, dead link) must keep the
// author's label text in the archive instead of silently dropping the block.
func TestParseBlocks_EmptyURLLinkKeepsLabel(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "link", URL: "", Content: `["important text","unstyled",[]]`},
		{Type: "text", Modificator: "BLOCK_END"},
		{Type: "link", URL: "", Content: `["","unstyled",[]]`}, // nothing to archive
	}
	result := ParseBlocks(blocks)
	want := []string{"important text"}
	if !slices.Equal(result.TextParts, want) {
		t.Errorf("TextParts = %q, want %q", result.TextParts, want)
	}
}

func TestParseBlocks_NoUnknownTypesOnKnownBlocks(t *testing.T) {
	blocks := []boosty.ContentBlock{
		{Type: "text", Content: `["hi","unstyled",[]]`},
		{Type: "image", URL: "https://example.com/i.jpg"},
		{Type: "ok_video", PlayerURLs: []boosty.PlayerURL{{Type: "high", URL: "https://example.com/v.mp4"}}},
		{Type: "video", URL: "https://youtube.com/watch?v=1"},
		{Type: "link", URL: "https://example.com"},
	}
	if got := ParseBlocks(blocks).UnknownTypes; len(got) != 0 {
		t.Errorf("UnknownTypes = %v, want empty for fully supported blocks", got)
	}
}
