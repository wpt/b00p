# b00p

[![tag](https://img.shields.io/github/v/tag/wpt/b00p?sort=semver&label=tag)](https://github.com/wpt/b00p/tags)
[![CI](https://github.com/wpt/b00p/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/wpt/b00p/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/wpt/b00p/gh-pages/coverage.json)](https://github.com/wpt/b00p/actions/workflows/ci.yml)

CLI parser and content downloader for [boosty.to](https://boosty.to). Downloads posts, images, native videos, comments. Also usable as a Go library.

## Installation

### Prebuilt binary (recommended)

Grab the binary for your OS from [GitHub Releases](https://github.com/wpt/b00p/releases). Releases publish **raw binaries** (no archive), one per platform:

- Linux: `b00p_linux_amd64`, `b00p_linux_arm64`
- Windows: `b00p_windows_amd64.exe`, `b00p_windows_arm64.exe`
- macOS (best-effort, not CI-tested): `b00p_darwin_amd64`, `b00p_darwin_arm64`

Rename it to `b00p` (or `b00p.exe` on Windows) and put it anywhere on your `PATH`.

> On Linux/macOS make the binary executable: `chmod +x b00p`. On Windows, run it as `.\b00p.exe` from PowerShell or `b00p` from any directory on `PATH`. Commands below use `b00p` — substitute `.\b00p.exe` if needed. Linux and Windows are first-class targets; macOS is best-effort. CI test-executes the suite on Linux and Windows; macOS builds are produced by goreleaser but not test-executed.

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
4. Create `auth.json` in the directory you run b00p from (the default `--auth auth.json` is resolved against the current working directory, not the binary's location — or pass `--auth` with a full path). The release binary ships alone (no template file), so just open a new file in any editor and save:

```json
{
  "accessToken": "paste_access_token_here",
  "refreshToken": "paste_refresh_token_here"
}
```

(If you cloned the repo, `auth.json.example` is in the repo root — `cp auth.json.example auth.json` works there.)

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
| **Sync headless** | `b00p download --blog username --sync --yes` | Same as sync but skip the prompt. **Required** for cron, `nohup`, Windows Task Scheduler, SSH-without-TTY, systemd — without `--yes` the prompt reads stdin which immediately returns EOF in headless contexts, b00p logs `warning: failed to read confirmation: EOF` followed by `Cancelled.`, and nothing is applied. |

#### Examples

Content flags (`--md`, `--comments`, `--download-external`, `--format`) combine with any download mode above; the sync-only and mode-exclusive flags have restrictions — see the blockquote after the examples:

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

> `--check-media`, `--check-files`, and `--yes` are sync-only. Passing them without `--sync` is a hard error (`requires --sync: [...]`).
>
> `--force` is rejected together with `--sync` (sync already detects what needs updating). Use `--force` only without `--sync`.
>
> `--url` is single-post: combine it only with `--md`, `--comments`, `--download-external`, `--format`. Passing `--sync`, `--check-media`, `--check-files`, `--yes`, `--force`, or `--workers > 1` is a hard error (`--url is single-post and cannot be combined with: [...]`).

#### Sync output

```
Syncing username...
  [NEW] Brand new accessible post
  [UNLOCKED] Previously locked post
  [UPDATED] Edited post
  [COMMENTS] Comments thread (comments: 5 → 8)
  [UPDATED,VIDEO_MISMATCH] Reuploaded with new video (video_001.mp4: local 1.2 GB vs remote 1.4 GB)
  [FILES_MISSING] Stale entry (missing comments.json)
  [LOCKED] Downgraded post

Sync summary:
  1 new posts
  1 unlocked posts
  2 updated posts
  1 comments updated
  1 video size mismatches
  1 files missing on disk
  1 locked (data preserved)
  76 no changes

Apply changes? [y/N]
```

#### Sync detection labels

- **NEW** — accessible post not in state. Downloaded fresh.
- **LOCKED_NEW** — brand-new post you don't have access to. Counted in the summary but not downloaded or written to state.
- **UNLOCKED** — was locked, now accessible (subscription upgraded). Triggers full re-download.
- **UPDATED** — author edited the post (`updatedAt` changed).
- **COMMENTS** — comment-count drift. For posts with `hasComments=true`, the on-disk count (top-level + inlined replies in `comments.json`) is compared to the API count, with disk reality winning over the cached state count. If `comments.json` is missing or unreadable, any non-zero API count triggers a refetch. For posts with `hasComments=false`, the legacy state-vs-API count comparison applies — when the API count differs from what state recorded at last save, sync fires `COMMENTS` and writes `comments.json` (after which `hasComments` flips to `true` and subsequent runs use the disk-count comparison). Posts flagged `commentsCapped` (see [State Tracking](#state-tracking)) suppress the disk<API mismatch trigger; the disk>API direction (deletions) still fires and clears the flag.
- **VIDEO_MISMATCH** — native `ok_video` discrepancy: local file is missing, the HEAD request returns non-200, or `Content-Length` differs from the local file size. Transient HEAD errors are logged and skipped. External videos (YouTube/VK/OK) are not validated. Requires `--check-media`.
- **FILES_MISSING** — expected files absent on disk. `post.json` is always required; `comments.json` and `post.md` are required only when state says they were previously written. The apply phase re-fetches and rewrites whatever the check flagged; it doesn't just report. Inaccessible (locked) posts are skipped — they can't be re-fetched while locked, so flagging them would only produce a guaranteed-failing apply. Requires `--check-files`.
- **LOCKED** — was accessible, now locked. On-disk data is preserved; only state's `locked` flag is flipped.

Posts with no actionable change are not labelled per-post; they only show up as `N no changes` in the summary count (inaccessible LOCKED_NEW posts appear under `N locked (no access)` instead, never in both lines). Note the per-flag lines (updated / comments / mismatch / missing) can each count the same multi-label post, so those lines may sum to more than the post total.

Multiple labels can apply to the same post — they appear in one bracket joined by commas, e.g. `[UPDATED,VIDEO_MISMATCH]`.

## Flags

Global flags (apply to every command):

| Flag | Default | Description |
|------|---------|-------------|
| `--auth` | `auth.json` | Path to token file |
| `-o, --output` | `output` | Root output directory (posts land under `<output>/<blog>/`) |

`stat` accepts only `--blog` (plus the global `--auth`; the global `--output` is a no-op for `stat`).

`download` flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--blog` | — | Blog username (mutually exclusive with `--url`) |
| `--url` | — | Full post URL for single-post download (mutually exclusive with `--blog`; rejects sync-mode flags — see note above the table) |
| `--md` | `false` | Generate `post.md` with frontmatter (price/tier included) |
| `--comments` | `false` | Download `comments.json` |
| `--download-external` | `false` | Download external videos via yt-dlp (best-effort; failures are logged, not retried) |
| `--force` | `false` | Ignore state and reprocess. Rejected together with `--sync`. Integrity check still skips existing non-empty media. |
| `--sync` | `false` | Smart sync with diff and confirmation |
| `--yes` | `false` | With `--sync`: skip the interactive `Apply changes? [y/N]` prompt. **Required for headless runs** (cron, `nohup`, Windows Task Scheduler, SSH-without-TTY) — without `--yes` the prompt sees stdin EOF, logs a warning, and silently cancels the sync. Without `--sync`: hard error. |
| `--check-media` | `false` | With `--sync`: validate native video sizes via HEAD. Without `--sync`: hard error. |
| `--check-files` | `false` | With `--sync`: verify expected files exist on disk. Without `--sync`: hard error. |
| `--format` | `{date}_{title}` | Post directory name format |
| `--workers` | `1` | Concurrent post processing — parallelises `download --blog` (default mode), `download --blog --sync` apply phase, and `--check-media` HEAD requests. Values below 1 are clamped to 1. |

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
  index.md                                 # navigation index over all posts (auto-generated)
  2026-03-13_Post Title/
    post.json                              # post data (always)
    post.md                                # markdown (with --md)
    comments.json                          # comments (with --comments)
    image_001.jpg                          # images
    video_001.mp4                          # native videos (best MP4)
    external_video_001.<ext>               # external videos (with --download-external)
```

`post.json` always contains links to external videos. `post.md` includes them only when generated with `--md`.

`index.md` is a clickable list of every tracked post (title → directory, comment counts, locked markers), sorted by directory name — chronological under the default `{date}_{title}` format. It is regenerated from `_state.json` at the end of every `--blog` download/sync run, including no-change runs, so deleting it self-heals (single-post `--url` downloads don't touch it). Don't edit it by hand.

Content block types b00p doesn't support yet (e.g. audio attachments or file blocks) are skipped with a per-post warning naming the type — if you see one, that content exists on Boosty but is not in your archive.

## State Tracking

`_state.json` per blog directory tracks downloaded posts. Each entry stores:

- `title`, `dirName` — post metadata
- `downloadedAt` — when b00p fetched this post (set at save time)
- `updatedAt` — when the author last edited the post (from the Boosty API; `0` for legacy entries written before the field was tracked, auto-backfilled on the next sync)
- `commentsCount` — what the API claimed at last save
- `hasComments`, `hasMd` — which artefacts were written on save (sync uses these to decide what files are expected on disk)
- `commentsCapped` — the last comment fetch hit Boosty's structural ceiling (>100 top-level threads, or any thread with more inline replies than `reply_limit=100` allows). Sync suppresses re-trigger on the disk-vs-API mismatch for capped posts so they don't fire `COMMENTS` on every run. A subsequent successful uncapped refetch clears the flag.
- `price`, `tier`, `locked` — access and pricing info

The state file itself records `version` (schema version; files written before the field existed load fine and are stamped on the next save — a file from a *newer* b00p is rejected instead of silently dropping its fields), `lastSync`, and the post map. Writes are atomic (temp + fsync + rename) — see the **Atomic writes** bullet under [Reliability](#reliability) for the exact guarantees.

Sync prefers disk reality over cached counts: for posts with `hasComments=true`, the next sync recomputes `len(top-level) + Σ len(replies.data)` from `comments.json` and refetches when that disagrees with the API. This auto-heals stale on-disk artefacts (e.g. posts whose replies were dropped before `reply_limit` was set on the comments endpoint) without a one-shot repair flag.

Locked posts are not stored — after upgrading your subscription they are downloaded automatically. Downgraded posts keep their data on disk and are marked `locked: true`.

## Reliability

- **Retry with backoff**: API GETs retry 3× (5s / 15s / 30s) on transient request-side errors (transport, 5xx, 429 — including those during a token refresh) — other 4xx responses, JSON decode failures, and a token refresh the server *rejected* with 4xx (dead refresh token: retrying a deterministic verdict only delays the actionable error) all fail fast. Media downloads retry 3× on transient errors (network failures, 5xx, 429, 416-after-resume, create/write/close errors); deterministic 4xx download statuses (400/403/404/410 — expired signed URL, deleted media) fail fast, and for 400/403/410 the error includes a hint that the signed CDN URL likely expired or IP-bound — rerun sync to refresh. HEAD checks for `--check-media` are not retried (transient HEAD failure logs a warning, the per-post check is skipped, no `VIDEO_MISMATCH` is raised on transport errors). yt-dlp is bounded by a 20-minute timeout per external video; a timeout reports `timed out after 20m0s` and the post still saves.
- **Idle-read watchdog**: long media downloads carry an idle-byte watchdog. If the server stops sending bytes for 60 seconds (wedged TCP connection that never RST/FINs), the request is cancelled with cause `idle timeout: no response body bytes for 60s` and the normal retry loop applies. Slow-but-live streams keep going forever.
- **HTTP Range resume**: partial `.tmp` files from prior retries are reused via `Range: bytes=N-`. A `.tmp.url` sidecar pins the URL the partial was downloaded against; on signed-URL refresh between attempts (or between runs), the sidecar mismatch drops the stale partial and starts fresh.
- **Atomic writes**: `_state.json`, `post.json`, `post.md`, `comments.json`, and `auth.json` (after refresh) are written via temp file + fsync + rename. Media downloads use temp file + rename (no fsync, to avoid stalling multi-GB writes) — a SIGKILL or crash mid-download leaves a `.tmp` plus `.tmp.url` orphan that the next attempt resumes from (if the URL still matches) or discards (if it doesn't). The parent directory is not fsynced, so power loss is not strongly defended against.
- **Integrity check**: existing non-empty files are skipped; 0-byte partials are removed and re-downloaded.
- **Incremental state saves**: state is written after each post, so interrupted runs resume cleanly.
- **Comments endpoint quirks**: the server silently drops replies unless `reply_limit` is set, and `offset>0` returns `data=[]` with `isLast=true`. b00p sends `reply_limit=100` and fetches comments with `limit=101` — one probe slot beyond the 100 we expect to keep, so posts with exactly 100 top-level threads can be distinguished from posts that hit the cap. Posts that genuinely exceed 100 top-level threads, or any thread with >100 replies, are flagged `commentsCapped` in state and sync suppresses the catch-up re-trigger that would otherwise fire forever — see [State Tracking](#state-tracking). New top-level threads on capped posts are not auto-detected; an edit (`UPDATED` label) re-fetches comments, or you can force a refetch for a specific post by deleting its `comments.json` and re-running a plain `--sync` — the missing file triggers `[COMMENTS]` directly, no extra flag needed (with `--check-files` it is additionally flagged `[FILES_MISSING]`).
- **Sync workers**: `--workers N` parallelises the sync apply phase too (not just `--check-media`). State writes are serialised under a mutex so concurrent updates do not race.
- **One instance per blog directory**: that mutex is in-process — two b00p processes on the same blog directory at once (e.g. a manual run overlapping a cron sync) can overwrite each other's `_state.json` entries (last writer wins). Nothing gets corrupted (writes stay atomic) and a lost entry self-heals on the next sync — the post re-classifies as NEW and existing files are skipped — but don't overlap runs deliberately; use `--workers` for parallelism inside one run.
- **Spinner**: animated progress with file size during downloads (`  ⠹ video_001.mp4  45.2 MB / 1.2 GB  (3.7%)`).
- **Clear errors**: expired tokens print instructions to update `auth.json`; download failures include the response body snippet and a hint when the status code looks like signed-URL expiry (400/403/410).

## External Videos

Embedded YouTube/VK/OK videos appear as links in `post.json` regardless. With `--download-external`, b00p invokes [yt-dlp](https://github.com/yt-dlp/yt-dlp) to fetch them. Failures are logged and skipped — they don't fail the post.

```bash
pip install yt-dlp
b00p download --blog username --download-external
```

If b00p logs `yt-dlp not found in PATH`, yt-dlp isn't installed or isn't on your shell's `PATH`. Verify with `yt-dlp --version`; on Windows, pip-installed binaries land in `%APPDATA%\Python\Python3xx\Scripts` (add that to `PATH`) or you can invoke it as `py -m yt_dlp`. See [Troubleshooting](#troubleshooting) for more.

## Troubleshooting

### `accessToken is empty` / `token refresh failed` / `token expired, refresh failed` / `401 refresh failed`

Your tokens are missing, expired, or the refresh attempt was rejected. Re-extract them:

1. Log in to [boosty.to](https://boosty.to).
2. DevTools (F12) → Application/Storage → Cookies → `https://boosty.to`.
3. Copy the `auth` cookie value (the whole JSON object).
4. Paste `accessToken` and `refreshToken` into `auth.json`, save, re-run.

### `API ... returned 401`

Same fix as the token errors above — the current access token has been revoked or expired and the refresh token couldn't recover it. The log line includes the failing URL between `API` and `returned`, so `API https://api.boosty.to/v1/blog/.../post/? returned 401: ...`.

### `API ... returned 403` / post shows up `[LOCKED]`

You don't have the required subscription tier for that post. Not a b00p error. If you upgrade later, the next `--sync` picks it up automatically (it'll show as `[UNLOCKED]`).

### `API ... returned 404`

The post or blog no longer exists (deleted, or the blog was renamed). A 404 on the **blog post list** (renamed/deleted blog) aborts the whole run with `fetch posts: ... returned 404` — fix your `--blog`. A 404 on a **single post** during sync counts it as a failed post, so the run exits non-zero. Either way b00p does not delete anything: stale directories and `_state.json` entries stay on disk until you remove them manually.

### `yt-dlp not found in PATH`

You passed `--download-external` but yt-dlp isn't installed or isn't on `PATH`. Install it (`pip install yt-dlp`) and verify with `yt-dlp --version`. On Windows, ensure the Python `Scripts` directory is on `PATH` or use `py -m yt_dlp`. yt-dlp failures on individual videos are logged and the post still saves — only the missing binary blocks the feature entirely.

### `--sync` cancels immediately in cron / systemd (or blocks on an idle interactive TTY)

In headless runs (cron, `nohup`, Windows Task Scheduler, SSH-without-TTY, systemd) stdin is closed, so `Apply changes? [y/N]` reads EOF immediately, b00p logs `warning: failed to read confirmation: EOF` followed by `Cancelled.`, and nothing is applied. On a live interactive TTY where no one types anything, the prompt blocks until you do. Either way add `--yes` to apply automatically:

```bash
b00p download --blog username --sync --yes
```

The log line `Auto-applying (--yes).` makes the choice explicit so headless logs still show the decision.

### Disk and API counts don't match after a long run

Posts with more than 100 top-level comments (or any single thread with more than 100 replies) hit the Boosty API's `offset=` quirk — the server returns an empty page even when more comments exist, capping retrieval. b00p detects this on save and sets `commentsCapped: true` in `_state.json` so the catch-up sync no longer re-fires `[COMMENTS]` on every run for that post (it would never close). New top-level threads on capped posts are not auto-detected; if the author edits such a post the `[UPDATED]` path re-fetches comments anyway, or you can force a refetch for a specific post by deleting its `comments.json` — the next plain `--sync` detects the missing file and re-fires `[COMMENTS]`, no extra flag needed. Deletions are still detected (disk > API triggers refetch). Everything below the 100-thread / 100-replies-per-thread limit syncs cleanly.

### Download fails with `status 403`/`400`/`410` and "signed URL likely expired"

okcdn (the CDN serving Boosty's native videos) issues signed URLs that bind to your IP and expire after a short window. On a long worker queue, late-queued downloads can hit dead URLs. b00p surfaces the response body snippet and adds the hint when the status looks like signed-URL expiry. Re-running the sync re-fetches the per-post payload, which contains fresh URLs. If you see this repeatedly, lower `--workers` so each post finishes before its URLs expire.

### `yt-dlp timed out after 20m0s`

A single external video download exceeded the 20-minute per-invocation budget. Usually a wedged ffmpeg muxer or a slow third-party CDN. The post still saves; the external video is logged as a failure and the next `--sync` doesn't retry external videos automatically (they're best-effort). If a specific URL chronically times out, fetch it manually with `yt-dlp <url>` outside b00p.

### `failed to save state: ... Access is denied` on Windows

Windows Defender or the Search Indexer can briefly hold a handle on freshly-written files during anti-malware scan, causing the atomic rename to fail with a sharing violation. The error is logged but data isn't lost — because state didn't advance, the next sync treats the affected post as `[NEW]` (it's missing from `_state.json`) and re-writes everything. If this happens often, exclude the output directory from Defender real-time scanning.

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

`FetchComments(blog, postID, limit)` yields top-level comments (replies are inlined per item, up to `reply_limit=100`), but unlike `FetchPosts` it returns a **single page**: the Boosty comments endpoint ignores `offset>0`, so pagination is impossible — size `limit` to cover every top-level thread you expect (the CLI uses 101 with cap detection; see the comments quirks under [Reliability](#reliability)).

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

CI runs `go vet`, `go test -race`, [`staticcheck`](https://staticcheck.dev/), and [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) on Linux, plus `go vet` and `go test -race` on Windows (NTFS case folding, reserved names, and rename semantics are load-bearing there), on every push and pull request against `master`.

## License

MIT
