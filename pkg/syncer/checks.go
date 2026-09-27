package syncer

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/state"
)

// missingFiles records which post artefacts are absent on disk. The flags
// drive the FILES_MISSING apply branch so we re-download only what is
// missing and preserve flags for files that still exist.
type missingFiles struct {
	PostJSON bool
	Comments bool
	Markdown bool
}

func (m missingFiles) Any() bool {
	return m.PostJSON || m.Comments || m.Markdown
}

// String returns a comma-separated list of missing filenames; empty if all
// present. Used both for sync display and as a stable test fixture.
func (m missingFiles) String() string {
	var parts []string
	if m.PostJSON {
		parts = append(parts, "post.json")
	}
	if m.Comments {
		parts = append(parts, "comments.json")
	}
	if m.Markdown {
		parts = append(parts, "post.md")
	}
	return strings.Join(parts, ", ")
}

// detectMissingFiles returns a struct describing which expected files are
// absent for a post. Comments and post.md are only checked when the prior
// state recorded them as previously downloaded.
func detectMissingFiles(entry state.PostEntry, dir string) missingFiles {
	var m missingFiles
	if _, err := os.Stat(filepath.Join(dir, "post.json")); err != nil {
		m.PostJSON = true
	}
	if entry.HasComments {
		if _, err := os.Stat(filepath.Join(dir, "comments.json")); err != nil {
			m.Comments = true
		}
	}
	if entry.HasMd {
		if _, err := os.Stat(filepath.Join(dir, "post.md")); err != nil {
			m.Markdown = true
		}
	}
	return m
}

// runCheckMedia performs HEAD-based video size validation in parallel and
// records mismatches on the corresponding items. Items that already have
// IsNew/JustUnlocked/etc. set are skipped — they will get fresh media
// regardless via the apply phase.
func (e *Engine) runCheckMedia(blogDir string, items []syncItem) {
	c := e.c
	var jobs []int
	for i, item := range items {
		if !item.InState || !item.Post.HasAccess {
			continue
		}
		// Skip items that will be fully re-downloaded anyway.
		if item.JustLocked || item.JustUnlocked {
			continue
		}
		jobs = append(jobs, i)
	}

	c.Log.Printf("Checking media sizes (%d posts, %d workers)...",
		len(jobs), max(1, min(e.cfg.Workers, len(jobs))))

	runWorkerPool(e.cfg.Workers, jobs, func(idx int) {
		post := items[idx].Post
		dir := filepath.Join(blogDir, items[idx].DirName)
		mismatch := e.checkVideoSizes(&post, dir)
		if mismatch != "" {
			// Distinct indices per worker → safe write.
			items[idx].VideoMismatch = mismatch
		}
	})
}

// checkVideoSizes validates local video files against remote for a post.
// Skips posts with no native video (ok_video) — nothing to verify. Otherwise
// fetches fresh video URLs and does authenticated HEAD requests, collecting
// all mismatches rather than bailing on the first one.
func (e *Engine) checkVideoSizes(post *boosty.Post, dir string) string {
	if !hasOkVideo(post.Data) {
		return ""
	}

	c := e.c
	var fullPost boosty.Post
	if err := c.GetJSON(boosty.PostURL(e.cfg.Blog, post.ID), &fullPost); err != nil {
		c.Log.Printf("  check-media %s: fetch failed: %v", post.ID, err)
		return ""
	}

	parsed := parser.ParseBlocks(fullPost.Data)
	var issues []string
	for _, m := range parsed.Media {
		if m.Type != "video" {
			continue
		}
		if issue := checkRemoteVideoSize(c.HTTP, boosty.UserAgent, c.Log,
			m.URL, filepath.Join(dir, m.Filename), m.Filename); issue != "" {
			issues = append(issues, issue)
		}
	}
	return strings.Join(issues, "; ")
}

// hasOkVideo reports whether any block is a native (ok_video) video.
func hasOkVideo(blocks []boosty.ContentBlock) bool {
	return slices.ContainsFunc(blocks, func(b boosty.ContentBlock) bool { return b.Type == "ok_video" })
}

// hasSignedMedia reports whether the post carries content whose download
// URLs are signed and can go stale between the list call and the apply:
// native ok_video (per-block signed okcdn URLs) and audio/file attachments
// (post-level signedQuery). Gates MaybeRefreshSignedURLs; checkVideoSizes
// keeps using hasOkVideo because it validates videos only.
func hasSignedMedia(blocks []boosty.ContentBlock) bool {
	return slices.ContainsFunc(blocks, func(b boosty.ContentBlock) bool {
		return b.Type == "ok_video" || b.Type == "audio_file" || b.Type == "file"
	})
}

// checkRemoteVideoSize compares local file size with the server's Content-Length
// obtained via HEAD. The okcdn signed URLs bind to the UA used to fetch them, so
// we must reuse the client's User-Agent.
//
// No Bearer auth is set: okcdn relies on URL signing (srcAg=... and sig=...
// in the query), not Authorization headers — same model as downloadOnce in
// pkg/boosty/download.go.
//
// httpc is c.HTTP (60s timeout). HEAD returns only headers — no body transfer
// — so 60s is generous even for gigabyte videos on slow links; c.DownloadHTTP
// (no timeout) is reserved for actual body streaming where a stuck connection
// must not block forever on a request that legitimately takes minutes.
//
// Returns a descriptive issue string only on verdicts about the local file
// (missing, size differs from a 200 with Content-Length). Everything else —
// network error, non-200 status, missing Content-Length — is logged and
// returns empty: a 403/405/5xx says nothing about the bytes on disk, and a
// mismatch verdict makes the apply phase re-download the video.
func checkRemoteVideoSize(httpc *http.Client, ua string, log boosty.Logger,
	url, localPath, filename string) string {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		return fmt.Sprintf("%s missing", filename)
	}

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		log.Printf("  check-media %s: build request: %v", filename, err)
		return ""
	}
	req.Header.Set("User-Agent", ua)

	resp, err := httpc.Do(req)
	if err != nil {
		log.Printf("  check-media %s: HEAD error: %v", filename, boosty.RedactURLError(err))
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("  check-media %s: HEAD %d; skipping size check", filename, resp.StatusCode)
		return ""
	}
	if resp.ContentLength <= 0 {
		log.Printf("  check-media %s: no Content-Length", filename)
		return ""
	}
	if localInfo.Size() != resp.ContentLength {
		return fmt.Sprintf("%s: local %s vs remote %s",
			filename,
			boosty.FormatSize(localInfo.Size()),
			boosty.FormatSize(resp.ContentLength),
		)
	}
	return ""
}
