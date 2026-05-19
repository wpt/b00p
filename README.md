# b00p

[![tag](https://img.shields.io/github/v/tag/wpt/b00p?sort=semver&label=tag)](https://github.com/wpt/b00p/tags)
[![CI](https://github.com/wpt/b00p/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/wpt/b00p/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/wpt/b00p/gh-pages/coverage.json)](https://github.com/wpt/b00p/actions/workflows/ci.yml)

CLI parser and content downloader for [boosty.to](https://boosty.to). Downloads posts, images, native videos, comments. Also usable as a Go library.

## Installation

### Prebuilt binary (recommended)

Grab the latest archive for your OS from [GitHub Releases](https://github.com/wpt/b00p/releases) — Windows and Linux are first-class (amd64 + arm64). Unzip it, drop `b00p` (or `b00p.exe` on Windows) anywhere on your `PATH`, and you're done.

> On Linux/macOS make the binary executable: `chmod +x b00p`. On Windows, run it as `.\b00p.exe` from PowerShell or `b00p` from any directory on `PATH`. Commands below use `b00p` — substitute `.\b00p.exe` if needed.

### Build from source

Requires **Go 1.26.3+**:

```bash
go install github.com/wpt/b00p@latest
```

Or clone and build:

```bash
git clone https://github.com/wpt/b00p.git
cd b00p
go build -o b00p .
```

## Quick Start

1. Log in to [boosty.to](https://boosty.to) in your browser.
2. Open DevTools (F12). In Chrome/Edge/Firefox go to Application (or Storage) → Cookies → `https://boosty.to`; in Safari open Develop → Show Web Inspector → Storage → Cookies.
3. Find the `auth` cookie — its value is a JSON object starting with `{` and containing `accessToken`, `refreshToken`, and optional `deviceId` / `expiresAt`. Copy the whole thing.
4. Create `auth.json` next to the binary. Copy the template (`cp auth.json.example auth.json` on Linux/macOS, `Copy-Item auth.json.example auth.json` in PowerShell, or just duplicate the file in Explorer), then paste your tokens:

```json
{
  "accessToken": "paste_access_token_here",
  "refreshToken": "paste_refresh_token_here"
}
```

Only `accessToken` is required. With `refreshToken`, b00p auto-refreshes on expiry and on 401; without it you'll re-paste tokens whenever they expire.

> Tokens auto-refresh on expiry (when `expiresAt` has passed) and on 401. The refreshed file is written via temp-file + rename, so an interrupted refresh cannot leave you with empty credentials. `auth.json` is in `.gitignore`.

5. Verify auth works:

```bash
b00p stat --blog username
```

This prints your subscription tier and the blog's post counts. If you see `accessToken is empty` or `token refresh failed`, the tokens in step 4 are wrong — see [Troubleshooting](#troubleshooting).

6. Download:

```bash
# Download all accessible posts (creates output/username/ and _state.json)
b00p download --blog username

# Download a single post
b00p download --url "https://boosty.to/username/posts/post-id"
```

Posts land under `output/username/`. A `_state.json` file appears alongside them and tracks what's been downloaded so repeat runs only fetch new posts. See [State Tracking](#state-tracking) for details.

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

Downloads posts with media. Pick a mode based on what you want to do:

| Mode | Command | What it does |
|------|---------|--------------|
| **New posts only** (default) | `b00p download --blog username` | First-time download or incremental update. Skips posts already in `_state.json`. |
| **Force re-download** | `b00p download --blog username --force` | Reprocess every post. Existing non-empty media files are still skipped by the integrity check; state is ignored. |
| **Single post** | `b00p download --url "https://boosty.to/username/posts/id"` | Download one post by URL. Ignores state. |
| **Smart sync** | `b00p download --blog username --sync` | Fetch the post list, diff against `_state.json` and disk, show the diff, ask `Apply changes? [y/N]`. Detects NEW, UNLOCKED, UPDATED, COMMENTS, VIDEO_MISMATCH, FILES_MISSING, LOCKED, LOCKED_NEW. |
| **Sync headless** | `b00p download --blog username --sync --yes` | Same as sync but skip the prompt. **Required** for cron, `nohup`, Windows Task Scheduler, SSH-without-TTY, systemd — without `--yes` the prompt blocks forever on stdin EOF. |

#### Examples

Any flag below combines with any download mode above:

```bash
# Save markdown and comments alongside post.json
b00p download --blog username --md --comments

# Single post with markdown, comments, external videos, custom dir name
b00p download --url "https://boosty.to/username/posts/post-id" --md --comments --download-external --format "{date:ymd}_{title}"

# Custom directory name format
b00p download --blog username --format "{date:ymd}_{title}"

# Download external videos (YouTube, VK, OK) via yt-dlp — see External Videos below
b00p download --blog username --download-external

# Concurrent downloads (3 posts in flight at once)
b00p download --blog username --workers 3

# Sync + validate native video file sizes against remote (one HEAD per video)
b00p download --blog username --sync --check-media

# Sync + verify on-disk artefacts match what state says was written (no network)
b00p download --blog username --sync --check-files
```

> `--check-media` and `--check-files` are sync-only. Passing them without `--sync` is silently ignored — always combine them with `--sync`.

#### Sync output

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

#### Sync detection labels

- **NEW** — accessible post not in state. Downloaded fresh.
- **LOCKED_NEW** — brand-new post you don't have access to. Counted in the summary but not downloaded or written to state.
- **UNLOCKED** — was locked, now accessible (subscription upgraded). Triggers full re-download.
- **UPDATED** — author edited the post (`updatedAt` changed).
- **COMMENTS** — comment-count drift. For posts with `hasComments=true`, the on-disk count (top-level + inlined replies in `comments.json`) is compared to the API count, with disk reality winning over the cached state count. If `comments.json` is missing or unreadable, any non-zero API count triggers a refetch. For posts with `hasComments=false`, only the legacy state-vs-API count comparison applies — on-disk `comments.json` is not reread, and API-side comment additions on those entries don't trigger a refetch on their own. To pull comments for such posts, use `--sync --check-files` so the missing artefact is flagged, or pass `--comments` once on a forced run.
- **VIDEO_MISMATCH** — native `ok_video` discrepancy: local file is missing, the HEAD request returns non-200, or `Content-Length` differs from the local file size. Transient HEAD errors are logged and skipped. External videos (YouTube/VK/OK) are not validated. Requires `--check-media`.
- **FILES_MISSING** — expected files absent on disk. `post.json` is always required; `comments.json` and `post.md` are required only when state says they were previously written. The apply phase re-fetches and rewrites whatever the check flagged; it doesn't just report. Requires `--check-files`.
- **LOCKED** — was accessible, now locked. On-disk data is preserved; only state's `locked` flag is flipped.

Posts with no actionable change are not labelled per-post; they only show up as `N no changes` in the summary count.

Multiple labels can apply to the same post — they appear in one bracket joined by commas, e.g. `[UPDATED,VIDEO_MISMATCH]`.

## Flags

Global flags (apply to every command):

| Flag | Default | Description |
|------|---------|-------------|
| `--auth` | `auth.json` | Path to token file |
| `-o, --output` | `output` | Root output directory (posts land under `<output>/<blog>/`) |

`download` and `stat` flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--blog` | — | Blog username (required for `stat`, `download` without `--url`) |
| `--url` | — | Full post URL (alternative to `--blog` for single-post download) |
| `--md` | `false` | Generate `post.md` with frontmatter (price/tier included) |
| `--comments` | `false` | Download `comments.json` |
| `--download-external` | `false` | Download external videos via yt-dlp (best-effort; failures are logged, not retried) |
| `--force` | `false` | Ignore state and reprocess; integrity check still skips existing non-empty media |
| `--sync` | `false` | Smart sync with diff and confirmation |
| `--yes` | `false` | With `--sync`: skip the interactive `Apply changes? [y/N]` prompt. **Required for headless runs** (cron, `nohup`, Windows Task Scheduler, SSH-without-TTY) — without `--yes` the prompt blocks forever on stdin EOF. |
| `--check-media` | `false` | With `--sync`: validate native video sizes via HEAD. Silently ignored without `--sync`. |
| `--check-files` | `false` | With `--sync`: verify expected files exist on disk. Silently ignored without `--sync`. |
| `--format` | `{date}_{title}` | Post directory name format |
| `--workers` | `1` | Concurrent post downloads (also parallelises `--check-media` HEAD requests) |

## Directory Name Format

Variables for `--format`:

| Variable | Example | Description |
|----------|---------|-------------|
| `{title}` | `Stream #87` | Post title (sanitized) |
| `{date}` | `2026-03-13` | Publish date (ISO) |
| `{date:ymd}` | `20260313` | Date with custom format |
| `{date:d.m.y}` | `13.03.2026` | y=year, m=month, d=day |
| `{id}` | `e24c0343-...` | Post UUID |

`{title}` is sanitized for Windows/POSIX filesystems: strips `\ / : * ? " < > |`, collapses whitespace, caps at 80 runes. The fully-formatted directory name is then trimmed of leading/trailing dots and spaces (so `..` and `.hidden` are normalized), and the post ID is substituted whenever the result is empty or matches a Windows reserved device name (`CON`, `PRN`, `AUX`, `NUL`, `COM1..9`, `LPT1..9`, case-insensitive — including extensions like `CON.txt`). Formatted-name collisions are resolved by appending the first 8 characters of the post ID.

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

- `title`, `dirName` — post metadata
- `downloadedAt` — when b00p fetched this post (set at save time)
- `updatedAt` — when the author last edited the post (from the Boosty API; `0` for legacy entries written before the field was tracked, auto-backfilled on the next sync)
- `commentsCount` — what the API claimed at last save
- `hasComments`, `hasMd` — which artefacts were written on save (sync uses these to decide what files are expected on disk)
- `price`, `tier`, `locked` — access and pricing info

The state file itself records `lastSync` and the post map. Writes are atomic (temp + fsync + rename) — see the **Atomic writes** bullet under [Reliability](#reliability) for the exact guarantees.

Sync prefers disk reality over cached counts: for posts with `hasComments=true`, the next sync recomputes `len(top-level) + Σ len(replies.data)` from `comments.json` and refetches when that disagrees with the API. This auto-heals stale on-disk artefacts (e.g. posts whose replies were dropped before `reply_limit` was set on the comments endpoint) without a one-shot repair flag.

Locked posts are not stored — after upgrading your subscription they are downloaded automatically. Downgraded posts keep their data on disk and are marked `locked: true`.

## Reliability

- **Retry with backoff**: API GETs retry 3× (5s / 15s / 30s) on request-side errors (transport, token-refresh, 5xx, 429) — other 4xx responses and JSON decode failures fail fast. Media downloads retry 3× after any `downloadOnce` error (network failures, any non-200 status including 4xx, create/write/close errors), cleaning partial files between attempts. HEAD checks for `--check-media` and `yt-dlp` invocations are not retried — a transient HEAD failure is logged and the per-post check skipped (no `VIDEO_MISMATCH` is raised on transport errors), and yt-dlp failures fall through without failing the post.
- **Atomic writes**: `_state.json`, `post.json`, `post.md`, `comments.json`, and `auth.json` (after refresh) are written via temp file + fsync + rename. Media downloads use temp file + rename (no fsync, to avoid stalling multi-GB writes) — a SIGKILL or crash mid-download leaves a `.tmp` orphan that the next run overwrites instead of poisoning the final path. The parent directory is not fsynced, so power loss is not strongly defended against.
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

If b00p logs `yt-dlp not found in PATH`, yt-dlp isn't installed or isn't on your shell's `PATH`. Verify with `yt-dlp --version`; on Windows, pip-installed binaries land in `%APPDATA%\Python\Python3xx\Scripts` (add that to `PATH`) or you can invoke it as `py -m yt_dlp`. See [Troubleshooting](#troubleshooting) for more.

## Troubleshooting

### `accessToken is empty` / `token refresh failed` / `token expired, refresh failed`

Your tokens are missing, expired, or the refresh attempt was rejected. Re-extract them:

1. Log in to [boosty.to](https://boosty.to).
2. DevTools (F12) → Application/Storage → Cookies → `https://boosty.to`.
3. Copy the `auth` cookie value (the whole JSON object).
4. Paste `accessToken` and `refreshToken` into `auth.json`, save, re-run.

### `API returned 401`

Same fix as above — the current access token has been revoked or expired and the refresh token couldn't recover it.

### `API returned 403` / post shows up `[LOCKED]`

You don't have the required subscription tier for that post. Not a b00p error. If you upgrade later, the next `--sync` picks it up automatically (it'll show as `[UNLOCKED]`).

### `API returned 404`

The post or blog no longer exists (deleted by the author or renamed). b00p skips it; if it was previously in state, the directory and `_state.json` entry stay on disk until you remove them manually.

### `yt-dlp not found in PATH`

You passed `--download-external` but yt-dlp isn't installed or isn't on `PATH`. Install it (`pip install yt-dlp`) and verify with `yt-dlp --version`. On Windows, ensure the Python `Scripts` directory is on `PATH` or use `py -m yt_dlp`. yt-dlp failures on individual videos are logged and the post still saves — only the missing binary blocks the feature entirely.

### `--sync` hangs after printing the summary

You're running headless (cron, `nohup`, Windows Task Scheduler, SSH-without-TTY, systemd) and stdin is closed. The `Apply changes? [y/N]` prompt blocks on EOF. Add `--yes` to apply automatically:

```bash
b00p download --blog username --sync --yes
```

The log line `Auto-applying (--yes).` makes the choice explicit so headless logs still show the decision.

### Disk and API counts don't match after a long run

Posts with more than ~100 top-level comments hit the Boosty API's `offset=` quirk — the server returns an empty page even when more comments exist, capping retrieval. This shows up as a disk-vs-API mismatch on the next sync. b00p can't fix it server-side (the API doesn't expose cursor pagination); the affected posts will keep mismatching until the API changes. Everything below ~100 top-level comments syncs cleanly.

## Library Usage

`pkg/boosty` and `pkg/parser` are importable. Full reference lives in godoc; the snippet below is enough to fetch and parse posts.

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

    // FetchPosts is an iter.Seq2 iterator (Go 1.23+) — pagination is
    // handled internally. Break out of the loop to stop early.
    for post, err := range client.FetchPosts("blogname", 50) {
        if err != nil {
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
}
```

`FetchComments(blog, postID, pageSize)` works the same way for top-level comments (replies are inlined per item, up to `reply_limit=100` — see the comments quirks under [Reliability](#reliability)).

For arbitrary endpoints not covered by a typed iterator, use `client.GetJSON(url, &out)` directly — `boosty.PostURL`, `boosty.PostsURL`, `boosty.CommentsURL`, and friends build the URLs.

By default `client.Log` is a silent discard. To see what b00p is doing (errors, retries, progress), assign your own `boosty.Logger`:

```go
type stderrLog struct{}
func (stderrLog) Printf(format string, args ...any) { log.Printf(format, args...) }

client.Log = stderrLog{}
```

`boosty.ProgressLogger` extends `Logger` with `Progress(format, args...)` and `ClearProgress()` for the spinner — implement it when you want download progress; implementations MUST be safe for concurrent calls (the CLI's `cmd/log.go` `stdLogger` is a reference).

## Tests

```bash
go vet ./...
go test ./... -race
```

CI runs `go vet`, `go test -race`, [`staticcheck`](https://staticcheck.dev/), and [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) on every push and pull request against `master`.

## License

MIT
