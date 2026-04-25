# b00p

[![tag](https://img.shields.io/github/v/tag/wpt/b00p?sort=semver&label=tag)](https://github.com/wpt/b00p/tags)
[![CI](https://github.com/wpt/b00p/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/wpt/b00p/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://wpt.github.io/b00p/coverage.json)](https://github.com/wpt/b00p/actions/workflows/ci.yml)

CLI parser and content downloader for [boosty.to](https://boosty.to). Downloads posts, images, native videos, comments. Also usable as a Go library.

Requires Go **1.26.1+** to build from source.

## Installation

```bash
go install github.com/wpt/b00p@latest
```

Or from source:

```bash
git clone https://github.com/wpt/b00p.git
cd b00p
go build -o b00p .
```

## Quick Start

1. Log in to [boosty.to](https://boosty.to) in your browser.
2. Open DevTools (F12) → Application → Cookies → `https://boosty.to`.
3. Copy the value of the `auth` cookie — it's a JSON object containing `accessToken`, `refreshToken`, and optional `deviceId` / `expiresAt`.
4. Create `auth.json` from the template:

```bash
cp auth.json.example auth.json
```

Paste your tokens (only `accessToken` is required; `refreshToken` enables auto-refresh):

```json
{
  "accessToken": "paste_access_token_here",
  "refreshToken": "paste_refresh_token_here"
}
```

> Tokens auto-refresh on expiry (when `expiresAt` has passed) and on 401. The refreshed file is written via temp-file + rename, so an interrupted refresh cannot leave you with empty credentials. `auth.json` is in `.gitignore`.

5. Run:

```bash
# Blog statistics
b00p stat --blog username

# Download all accessible posts
b00p download --blog username

# Download a single post
b00p download --url "https://boosty.to/username/posts/post-id"
```

## Commands

### stat

Subscription info and blog post counts.

```bash
b00p stat --blog coolblogger
```

```
=== Who Is Me ===
  Blog:   coolblogger
  Tier:   Supporter
  Price:  300 RUB
  Status: Active

=== Blog: coolblogger ===
  Total posts:  84
  Accessible:   71
  Locked:       13
```

### download

Downloads posts with media.

```bash
# All accessible posts
b00p download --blog username

# Single post by URL
b00p download --url "https://boosty.to/username/posts/post-id"

# With markdown and comments
b00p download --blog username --md --comments

# Re-process all posts (state is ignored; existing non-empty media files are still skipped)
b00p download --blog username --force

# Custom directory name format
b00p download --blog username --format "{date:ymd}_{title}"

# Download external videos (YouTube, VK, OK) via yt-dlp
b00p download --blog username --download-external

# Concurrent downloads (3 posts at a time)
b00p download --blog username --workers 3
```

### download --sync

Smart sync: fetches the post list (pagination only — cheap), diffs against state, asks for confirmation, applies only what changed.

```bash
# Interactive
b00p download --blog username --sync

# Headless (skip prompt — for cron / nohup runs)
b00p download --blog username --sync --yes

# Also validate native video file sizes against remote
b00p download --blog username --sync --check-media

# Verify on-disk artefacts match what state says was written
b00p download --blog username --sync --check-files
```

Example output:

```
Syncing username...
  [NEW] Brand new accessible post
  [UNLOCKED] Previously locked post
  [UPDATED] Edited post
  [COMMENTS] Comments thread (comments: 5 → 8)
  [UPDATED,VIDEO_MISMATCH] Reuploaded with new video (video_001.mp4: local 1.2 GB vs remote 1.4 GB)
  [FILES_MISSING] Stale entry (comments.json missing)
  [LOCKED] Downgraded post

Sync summary:
  1 new posts
  1 unlocked posts
  1 updated posts
  1 comments updated
  1 locked (data preserved)
  79 no changes

Apply changes? [y/N]
```

Sync detects:

- **NEW** — accessible post not in state. Downloaded fresh.
- **LOCKED_NEW** — brand-new post you don't have access to. Counted in the summary but not downloaded or written to state.
- **UNLOCKED** — was locked, now accessible (subscription upgraded). Triggers full re-download.
- **UPDATED** — author edited the post (`updatedAt` changed).
- **COMMENTS** — comment-count drift. For posts with `hasComments=true`, the on-disk count (top-level + inlined replies in `comments.json`) is compared to the API count, with disk reality winning over the cached state count. If `comments.json` is missing or unreadable, any non-zero API count triggers a refetch. For posts with `hasComments=false`, the legacy state-vs-API count comparison is used.
- **VIDEO_MISMATCH** — native `ok_video` discrepancy: local file is missing, the HEAD request returns non-200, or `Content-Length` differs from the local file size. Transient HEAD errors are logged and skipped. External videos (YouTube/VK/OK) are not validated. Requires `--check-media`.
- **FILES_MISSING** — expected files absent on disk. `post.json` is always required; `comments.json` and `post.md` are required only when state says they were previously written. Requires `--check-files`.
- **LOCKED** — was accessible, now locked. On-disk data is preserved; only state's `locked` flag is flipped.

Multiple labels can apply to the same post — they appear in one bracket joined by commas, e.g. `[UPDATED,VIDEO_MISMATCH]`.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--auth` | `auth.json` | Path to token file |
| `-o, --output` | `output` | Output directory |
| `--blog` | — | Blog username (required for `stat`, `download` without `--url`) |
| `--url` | — | Full post URL (alternative to `--blog` for single-post download) |
| `--md` | `false` | Generate `post.md` with frontmatter (price/tier included) |
| `--comments` | `false` | Download `comments.json` |
| `--download-external` | `false` | Download external videos via yt-dlp (best-effort; failures are logged, not retried) |
| `--force` | `false` | Ignore state and reprocess; integrity check still skips existing non-empty media |
| `--sync` | `false` | Smart sync with diff and confirmation |
| `--yes` | `false` | With `--sync`: skip the interactive confirmation |
| `--check-media` | `false` | With `--sync`: validate native video sizes via HEAD |
| `--check-files` | `false` | With `--sync`: verify expected files exist on disk |
| `--format` | `{date}_{title}` | Post directory name format |
| `--workers` | `1` | Concurrent post downloads |

## Directory Name Format

Variables for `--format`:

| Variable | Example | Description |
|----------|---------|-------------|
| `{title}` | `Stream #87` | Post title (sanitized) |
| `{date}` | `2026-03-13` | Publish date (ISO) |
| `{date:ymd}` | `20260313` | Date with custom format |
| `{date:d.m.y}` | `13.03.2026` | y=year, m=month, d=day |
| `{id}` | `e24c0343-...` | Post UUID |

`{title}` is sanitized for Windows/POSIX filesystems: strips `\ / : * ? " < > |`, collapses whitespace, caps at 80 runes. The fully-formatted directory name is then trimmed of trailing dots and spaces (a Windows FS quirk). If formatting yields an empty string, the post ID is used. Formatted-name collisions are resolved by appending the first 8 characters of the post ID.

## Output Structure

```
output/username/
  _state.json                              # downloaded posts tracker
  2026-03-13_Post Title/
    post.json                              # post data (always)
    post.md                                # markdown (with --md)
    comments.json                          # comments (with --comments)
    image_001.jpg                          # images
    video_001.mp4                          # native videos (best MP4)
    external_video_001.<ext>               # external videos (with --download-external)
```

`post.json` always contains links to external videos. `post.md` includes them only when generated with `--md`.

## State Tracking

`_state.json` per blog directory tracks downloaded posts. Each entry stores:

- `title`, `dirName`, `downloadedAt`
- `updatedAt` — for sync's edit-detection
- `commentsCount` — what the API claimed at last save
- `hasComments`, `hasMd` — which artefacts were generated
- `price`, `tier`, `locked` — access info

The state file itself records `lastSync` and the post map. Writes are atomic (temp + fsync + rename) — see the **Atomic writes** bullet under [Reliability](#reliability) for the exact guarantees.

Sync prefers disk reality over cached counts: for posts with `hasComments=true`, the next sync recomputes `len(top-level) + Σ len(replies.data)` from `comments.json` and refetches when that disagrees with the API. This auto-heals stale on-disk artefacts (e.g. posts whose replies were dropped before `reply_limit` was set on the comments endpoint) without a one-shot repair flag.

Locked posts are not stored — after upgrading your subscription they are downloaded automatically. Downgraded posts keep their data on disk and are marked `locked: true`.

## Reliability

- **Retry with backoff**: API GETs retry 3× (5s / 15s / 30s) on request-side errors (transport, token-refresh, 5xx, 429) — other 4xx responses and JSON decode failures fail fast. Media downloads retry 3× after any `downloadOnce` error (network failures, any non-200 status including 4xx, create/write/close errors), cleaning partial files between attempts. HEAD checks for `--check-media` and `yt-dlp` invocations are not retried.
- **Atomic writes**: `_state.json`, `post.json`, `post.md`, `comments.json`, and `auth.json` (after refresh) are written via temp file + fsync + rename. This prevents truncated target files during interrupted writes; the parent directory is not fsynced, so power loss is not strongly defended against.
- **Integrity check**: existing non-empty files are skipped; 0-byte partials are removed and re-downloaded.
- **Incremental state saves**: state is written after each post, so interrupted runs resume cleanly.
- **Comments endpoint quirks**: the server silently drops replies unless `reply_limit` is set, and `offset>0` returns `data=[]` with `isLast=true`. b00p sends `reply_limit=100` and uses `limit=100` with offset pagination — but the broken `offset=` short-circuits the iterator after the first page, so posts with >100 top-level comments would silently cap and surface as a disk-vs-API count mismatch on the next sync (a true fix would need cursor pagination, which the API doesn't appear to expose).
- **Spinner**: animated progress with file size during downloads (`⠹ video_001.mp4 45.2 MB / 1.2 GB (3.7%)`).
- **Clear errors**: expired tokens print instructions to update `auth.json`.

## External Videos

Embedded YouTube/VK/OK videos appear as links in `post.json` regardless. With `--download-external`, b00p invokes [yt-dlp](https://github.com/yt-dlp/yt-dlp) to fetch them. Failures are logged and skipped — they don't fail the post.

```bash
pip install yt-dlp
b00p download --blog username --download-external
```

## Library Usage

```go
package main

import (
    "fmt"
    "log"

    "github.com/wpt/b00p/pkg/boosty"
    "github.com/wpt/b00p/pkg/parser"
)

func main() {
    tokens, err := boosty.LoadTokens("auth.json")
    if err != nil {
        log.Fatal(err)
    }
    client := boosty.NewClient(tokens, "auth.json")

    var post boosty.Post
    if err := client.GetJSON(boosty.PostURL("blogname", "post-id"), &post); err != nil {
        log.Fatal(err)
    }

    parsed := parser.ParseBlocks(post.Data)
    for _, text := range parsed.TextParts {
        fmt.Println(text)
    }
    for _, media := range parsed.Media {
        fmt.Println(media.Type, media.URL)
    }

    if post.SubscriptionLevel != nil {
        fmt.Println("Tier:", post.SubscriptionLevel.Name)
    }
    fmt.Println("Price:", post.Price, "RUB")
    if eur, ok := post.CurrencyPrices["EUR"]; ok {
        fmt.Printf("Price: %.2f EUR\n", eur)
    }
}
```

`FetchPosts` and `FetchComments` return `iter.Seq2` iterators (Go 1.23+) for paginated traversal.

## Tests

```bash
go vet ./...
go test ./... -v
```

CI runs both on every push and pull request against `master`.

## License

MIT
