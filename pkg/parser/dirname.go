package parser

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// DefaultFormat is the default directory name format for downloaded posts.
const DefaultFormat = "{date}_{title}"

var (
	placeholderRe = regexp.MustCompile(`\{(\w+)(?::([^}]+))?\}`)
	unsafeCharsRe = regexp.MustCompile(`[\\/:*?"<>|]`)

	// Windows reserved device names (case-insensitive, base name only —
	// "CON.txt" is also reserved). Creating a file or directory with one of
	// these on Windows fails or produces an unopenable handle, even on
	// filesystems mounted via SMB from a non-Windows host.
	// Ref: https://learn.microsoft.com/windows/win32/fileio/naming-a-file
	winReservedNames = map[string]struct{}{
		"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
		"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
		"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
		"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
		"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	}
)

// FormatDirName builds a directory name from the format string and post data.
// Supported placeholders: {date}, {date:FORMAT}, {title}, {id}
func FormatDirName(format string, title string, publishTime int64, postID string) string {
	t := time.Unix(publishTime, 0)

	result := placeholderRe.ReplaceAllStringFunc(format, func(match string) string {
		parts := placeholderRe.FindStringSubmatch(match)
		name := parts[1]
		arg := parts[2]

		switch name {
		case "date":
			return FormatDate(t, arg)
		case "title":
			return SanitizeTitle(title)
		case "id":
			return postID
		default:
			return match
		}
	})

	// The format string itself is user-controlled, and {date:FORMAT} copies
	// unknown runes literally. Sanitize the whole formatted result so
	// separators or device characters introduced outside {title} cannot
	// create nested paths or Windows-invalid names. Only the char cleanup is
	// applied here — the length cap stays scoped to {title} (SanitizeTitle) so
	// a {date} prefix does not eat into the title's budget.
	result = sanitizeNameChars(result)

	// Trim trailing dots and spaces (Windows FS limitation) and leading dots
	// or spaces (otherwise "." / ".." / ".hidden" sanitize-survives and can
	// collide with the parent dir entry or be hidden on Unix-style listings).
	// Both cutsets include both characters so interleaved leaders like ". . "
	// collapse in one pass.
	result = strings.TrimRight(result, ". ")
	result = strings.TrimLeft(result, ". ")
	if result == "" || isReservedName(result) {
		result = postID
	}
	return result
}

// isReservedName reports whether name matches a Windows reserved device name,
// matching the base name (everything before the first dot) case-insensitively.
// "CON", "con", "COM1.txt" all return true.
func isReservedName(name string) bool {
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	_, ok := winReservedNames[strings.ToUpper(base)]
	return ok
}

// FormatDate formats time using a simple preset system.
// Letters y, m, d map to year, month, day. Everything else is a literal separator.
// Examples: "" → 2026-03-13, "ymd" → 20260313, "d.m.y" → 13.03.2026
func FormatDate(t time.Time, format string) string {
	if format == "" {
		return t.Format("2006-01-02")
	}

	var b strings.Builder
	for _, ch := range format {
		switch ch {
		case 'y':
			b.WriteString(fmt.Sprintf("%04d", t.Year()))
		case 'm':
			b.WriteString(fmt.Sprintf("%02d", t.Month()))
		case 'd':
			b.WriteString(fmt.Sprintf("%02d", t.Day()))
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// SanitizeTitle cleans a post title for use as a directory name and caps it at
// 80 runes so a single placeholder cannot dominate the path.
func SanitizeTitle(title string) string {
	s := sanitizeNameChars(title)
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80])
		s = strings.TrimRightFunc(s, func(r rune) bool {
			return unicode.IsSpace(r) || r == '-' || r == '_'
		})
	}
	return s
}

// sanitizeNameChars strips FS-unsafe and control characters and collapses
// whitespace, with NO length cap. SanitizeTitle layers the 80-rune cap on top
// for the {title} placeholder; FormatDirName applies this to the whole
// formatted result so separators from a {date:FORMAT} literal cannot create
// nested paths, while leaving the length budget to {title}.
func sanitizeNameChars(s string) string {
	s = unsafeCharsRe.ReplaceAllString(s, "")
	// Strip non-space control bytes (C0 0x00-0x1F, DEL 0x7F, C1 0x80-0x9F).
	// Windows os.MkdirAll rejects any path containing them, so a stray
	// control char in a Boosty title (paste artifact, BEL, NUL, etc.) would
	// otherwise make that post permanently undownloadable and re-error on
	// every --sync. Space-like controls (\t\n\v\f\r) are left for
	// collapseWhitespace to fold into a single separating space; it also
	// trims the ends.
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	return collapseWhitespace(s)
}
