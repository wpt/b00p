package parser

import (
	"strings"
	"testing"
	"time"
)

func TestFormatDate(t *testing.T) {
	ts := time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		format string
		want   string
	}{
		{"", "2026-03-13"},
		{"ymd", "20260313"},
		{"dmy", "13032026"},
		{"d.m.y", "13.03.2026"},
		{"y-m-d", "2026-03-13"},
		{"y", "2026"},
		{"m/d", "03/13"},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			got := FormatDate(ts, tt.format)
			if got != tt.want {
				t.Errorf("FormatDate(%q) = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}

func TestSanitizeTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"normal", "Hello World", "Hello World"},
		{"unsafe chars", `Test: "file" <name>`, "Test file name"},
		{"slashes", "path/to\\file", "pathtofile"},
		{"cyrillic", "Тест или не тест вот в чём вопрос", "Тест или не тест вот в чём вопрос"},
		{"collapse spaces", "too   many   spaces", "too many spaces"},
		{"trim", "  trimmed  ", "trimmed"},
		{"pipe", "a|b", "ab"},
		// Non-space control characters are stripped (Windows MkdirAll rejects
		// paths containing them — a title with a BEL/NUL would make the post
		// permanently un-downloadable); space-like controls (\t etc.) survive
		// to strings.Fields and collapse into a single separator.
		{"control_bel", "a\x07b", "ab"},
		{"control_nul", "a\x00b", "ab"},
		{"control_del", "a\x7fb", "ab"},
		{"control_tab_is_separator", "a\tb", "a b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeTitle(tt.title)
			if got != tt.want {
				t.Errorf("SanitizeTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestSanitizeTitle_LongTitle(t *testing.T) {
	// 100 character cyrillic string
	long := strings.Repeat("я", 100)
	got := SanitizeTitle(long)
	if len([]rune(got)) > 80 {
		t.Errorf("SanitizeTitle(100 chars) = %d runes, want <= 80", len([]rune(got)))
	}
}

func TestFormatDirName(t *testing.T) {
	publishTime := time.Date(2026, 3, 13, 10, 0, 0, 0, time.UTC).Unix()
	postID := "abc-123"

	tests := []struct {
		format string
		want   string
	}{
		{"{date}_{title}", "2026-03-13_Test Post"},
		{"{date:ymd}_{title}", "20260313_Test Post"},
		{"{title}", "Test Post"},
		{"{id}", "abc-123"},
		{"{date}_{id}", "2026-03-13_abc-123"},
		{"{unknown}", "{unknown}"},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			got := FormatDirName(tt.format, "Test Post", publishTime, postID)
			if got != tt.want {
				t.Errorf("FormatDirName(%q) = %q, want %q", tt.format, got, tt.want)
			}
		})
	}
}

func TestFormatDirName_EmptyResult(t *testing.T) {
	// Title with only unsafe chars, empty format
	got := FormatDirName("{title}", `<>:"/\|?*`, 0, "fallback-id")
	if got != "fallback-id" {
		t.Errorf("FormatDirName with empty title = %q, want 'fallback-id'", got)
	}
}

func TestFormatDirName_TrailingDots(t *testing.T) {
	got := FormatDirName("{title}", "Post title...", 0, "id")
	if got != "Post title" {
		t.Errorf("FormatDirName = %q, want trailing dots trimmed", got)
	}
}

func TestFormatDirName_SanitizesFullFormat(t *testing.T) {
	got := FormatDirName(`../{date:m/d}_{title}:{id}`, "Title", 1700000000, "post123")
	if strings.ContainsAny(got, `\/:*?"<>|`) {
		t.Fatalf("FormatDirName returned unsafe name %q", got)
	}
	if strings.HasPrefix(got, ".") {
		t.Fatalf("FormatDirName returned leading-dot name %q", got)
	}
}

// Leading dots must be stripped — otherwise a title like ".." sanitize-survives
// (no unsafe chars, not empty) and collides with the parent directory entry.
func TestFormatDirName_LeadingDots(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"double dot", "..", "fallback-id"},
		{"leading dot", ".hidden", "hidden"},
		{"dot dot title", "..title", "title"},
		{"mixed leading", ". . Real Title", "Real Title"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDirName("{title}", tt.title, 0, "fallback-id")
			if got != tt.want {
				t.Errorf("FormatDirName(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

// Windows reserved device names (CON, PRN, COM1..9, LPT1..9, etc.) must fall
// back to the post ID — creating a directory with one of these on Windows
// fails or yields an unopenable handle, even via SMB from a non-Windows host.
func TestFormatDirName_WindowsReservedNames(t *testing.T) {
	tests := []struct {
		name  string
		title string
	}{
		{"con", "CON"},
		{"con lowercase", "con"},
		{"prn with extension", "prn.txt"},
		{"com1", "COM1"},
		{"lpt9", "lpt9"},
		{"nul", "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDirName("{title}", tt.title, 0, "fallback-id")
			if got != "fallback-id" {
				t.Errorf("FormatDirName(%q) = %q, want 'fallback-id'", tt.title, got)
			}
		})
	}
}

func TestFormatDirName_NonReservedSimilarNames(t *testing.T) {
	// Names that look reserved but are not — must NOT fall back to ID.
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"console", "Console", "Console"},
		{"com10", "COM10", "COM10"},
		{"lpt", "LPT", "LPT"},
		{"prefix con", "configuration", "configuration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDirName("{title}", tt.title, 0, "fallback-id")
			if got != tt.want {
				t.Errorf("FormatDirName(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
