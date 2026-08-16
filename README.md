# b00p

[![tag](https://img.shields.io/github/v/tag/wpt/b00p?sort=semver&label=tag)](https://github.com/wpt/b00p/tags)
[![CI](https://github.com/wpt/b00p/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/wpt/b00p/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/wpt/b00p/gh-pages/coverage.json)](https://github.com/wpt/b00p/actions/workflows/ci.yml)

CLI content downloader for [boosty.to](https://boosty.to). Archives everything your subscriptions give you access to. Also usable as a Go library.

- **Posts** — full JSON payload, optional markdown rendering with frontmatter
- **Media** — images, native videos (best MP4 quality), audio and file attachments
- **Comments** — with inlined replies
- **External videos** — YouTube/VK/OK embeds archived as links, optionally downloaded via [yt-dlp](https://github.com/yt-dlp/yt-dlp)
- **Smart sync** — incremental updates with a reviewable diff: new/edited posts, new comments, unlocked tiers, on-disk integrity checks
- **Crash-safe** — atomic writes, HTTP range resume, retry with backoff; interrupted runs pick up where they left off

## Installation

### Prebuilt binary (recommended)

Grab the binary for your OS from [GitHub Releases](https://github.com/wpt/b00p/releases). Releases publish **raw binaries** (no archive), one per platform:

- Linux: `b00p_linux_amd64`, `b00p_linux_arm64`
- Windows: `b00p_windows_amd64.exe`, `b00p_windows_arm64.exe`
- macOS (best-effort, not CI-tested): `b00p_darwin_amd64`, `b00p_darwin_arm64`

Rename it to `b00p` (or `b00p.exe` on Windows) and put it anywhere on your `PATH`.

> On Linux/macOS make the binary executable: `chmod +x b00p`. On Windows, run it as `.\b00p.exe` from PowerShell or `b00p` from any directory on `PATH`. Commands below use `b00p` — substitute `.\b00p.exe` if needed.

### Build from source

Requires **Go 1.26.6+**:

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
3. Find the `auth` cookie. Its value is JSON — but the browser shows it **percent-encoded**, so what you actually see starts with `%7B%22accessToken%22...`, not `{`. That's the right cookie; copy the whole value. It holds `accessToken`, `refreshToken`, and optionally `deviceId` / `expiresAt`.
4. Create `auth.json` in the directory you run b00p from (the default `--auth auth.json` is resolved against the current working directory, not the binary's location — or pass `--auth` with a full path). The release binary ships alone (no template file), so just open a new file in any editor and save:

```json
{
  "accessToken": "paste_access_token_here",
  "refreshToken": "paste_refresh_token_here",
  "deviceId": "paste_device_id_here"
}
```

(If you cloned the repo, `auth.json.example` is in the repo root — `cp auth.json.example auth.json` works there. Saving the copied percent-encoded value verbatim works too — b00p decodes it.)

Only `accessToken` is required; drop `deviceId` if the cookie doesn't have one. With `refreshToken`, b00p auto-refreshes on expiry and on 401; without it you'll re-paste tokens whenever they expire.

> `auth.json` must be **writable**: b00p writes refreshed tokens back to it (mode 0600), and a refresh it cannot persist is a hard error, not a warning. Boosty also rotates the refresh token on every refresh, so don't share one `auth.json` between two machines — the second one starts failing as soon as the first refreshes. Give each its own copy.

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

> Progress and diagnostics go to **stderr**; only `stat`'s report goes to stdout. So `b00p download --blog username > log.txt` captures nothing — use `2> log.txt` or `2>&1`. And when stderr isn't a terminal the download spinner is switched off, so a multi-gigabyte video prints nothing between `downloading video_001.mp4...` and `downloaded video_001.mp4`. That's not a hang.

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
| **Sync headless** | `b00p download --blog username --sync --yes` | Same as sync but skip the prompt. **Required** for cron / Task Scheduler / any run without a terminal — see [Troubleshooting](#troubleshooting). |

#### Examples

Content flags (`--md`, `--comments`, `--download-external`, `--format`) combine with any download mode above. `--check-media`, `--check-files`, and `--yes` require `--sync`. `--force` is rejected together with `--sync`. `--url` is single-post and accepts only the four content flags — passing `--sync`, `--check-media`, `--check-files`, `--yes`, `--force`, or `--workers` (even an explicit `--workers 1`) is a hard error. b00p names exactly what's incompatible if you mix them.

> **Content flags are not retroactive.** They apply to posts downloaded *in that run*. Adding `--md` or `--comments` to an archive you already have does nothing: the default mode skips every post already in `_state.json`, and sync only regenerates an artefact when the post was edited or when a file it previously recorded went missing. To backfill an existing archive, run it once with `--force`:
>
> ```bash
> b00p download --blog username --force --md --comments
> ```
>
> Media already on disk is skipped by the integrity check, so this is cheap — it re-fetches metadata, not gigabytes.

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

# Sync + validate native video file sizes against remote
# (one post GET to refresh signed URLs, then one HEAD per native video)
b00p download --blog username --sync --check-media

# Sync + verify on-disk artefacts match what state says was written (no network)
b00p download --blog username --sync --check-files
```

#### Sync output

Both disk checks are on here, so every label can appear — plain `--sync` never emits `VIDEO_MISMATCH` or `FILES_MISSING`:

```
$ b00p download --blog username --sync --check-media --check-files

Syncing username...
Checking media sizes (83 posts, 1 workers)...
Checking files on disk...
  [NEW] Brand new accessible post
  [UNLOCKED] Previously locked post (was locked, now accessible)
  [UPDATED] Edited post (post edited)
  [COMMENTS] Comments thread (comments: 5 → 8)
  [UPDATED,VIDEO_MISMATCH] Reuploaded with new video (post edited; video_001.mp4: local 1.2 GB vs remote 1.4 GB)
  [FILES_MISSING] Stale entry (missing comments.json)
  [LOCKED] Downgraded post (was accessible, now locked)

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
- **COMMENTS** — the comment count changed since last download; `comments.json` is re-fetched. (Posts with more than 100 top-level threads are a special case — see [Troubleshooting](#some-posts-always-show-fewer-comments-than-boosty).)
- **VIDEO_MISMATCH** — a native video's size on disk doesn't match the server. Only native videos are checked. Requires `--check-media`.
- **FILES_MISSING** — expected files are missing on disk and get re-fetched. Requires `--check-files`.
- **LOCKED** — was accessible, now locked (subscription downgraded). On-disk data is kept; the post is just marked locked.

Posts with nothing to do show up only as `N no changes` in the summary. Multiple labels can apply to one post — they appear in one bracket, e.g. `[UPDATED,VIDEO_MISMATCH]`.

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
| `--blog` | — | Blog username, as in `boosty.to/<username>` (mutually exclusive with `--url`). Letters, digits, `_`, `-`, and dots between them (`bbb.sss`); up to 64 characters. |
| `--url` | — | Full post URL for single-post download (mutually exclusive with `--blog`; rejects the sync flags, `--force` and `--workers` — see note above the table) |
| `--md` | `false` | Generate `post.md` with frontmatter (price/tier included). Not retroactive — use `--force` to backfill an existing archive |
| `--comments` | `false` | Download `comments.json`. Not retroactive — use `--force` to backfill an existing archive |
| `--download-external` | `false` | Download external videos via yt-dlp (best-effort; failures are logged, not retried) |
| `--force` | `false` | Ignore state and reprocess. Rejected together with `--sync`. Integrity check still skips existing non-empty media. |
| `--sync` | `false` | Smart sync with diff and confirmation |
| `--yes` | `false` | With `--sync`: skip the `Apply changes? [y/N]` prompt — required for cron/headless runs, see [Troubleshooting](#troubleshooting). Without `--sync`: hard error. |
| `--check-media` | `false` | With `--sync`: validate native video sizes via HEAD. Without `--sync`: hard error. |
| `--check-files` | `false` | With `--sync`: verify expected files exist on disk. Without `--sync`: hard error. |
| `--format` | `{date}_{title}` | Post directory name format |
| `--workers` | `1` | Concurrent post processing — parallelises `download --blog` (default mode), `download --blog --sync` apply phase, and `--check-media` HEAD requests. Values below 1 are rejected (`--workers must be >= 1`). |

## Directory Name Format

Variables for `--format`:

| Variable | Example | Description |
|----------|---------|-------------|
| `{title}` | `Stream #87` | Post title (sanitized) |
| `{date}` | `2026-03-13` | Publish date (ISO) |
| `{date:ymd}` | `20260313` | Date with custom format |
| `{date:d.m.y}` | `13.03.2026` | y=year, m=month, d=day |
| `{id}` | `e24c0343-...` | Post UUID |

`{title}` is sanitized to be safe on Windows and POSIX filesystems: unsafe characters stripped, whitespace collapsed, length capped at 80 characters; names that end up empty or reserved on Windows (`CON`, `NUL`, ...) are replaced by the post ID. Name collisions are resolved by appending the first 8 characters of the post ID.

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
    audio_001.mp3                          # audio attachments
    file_001.pdf                           # file attachments
    external_video_001.<ext>               # external videos (with --download-external)
```

`post.json` always contains links to external videos. `post.md` includes them only when generated with `--md`.

Audio and file attachments get numbered on-disk names (`audio_001.mp3`, `file_001.pdf`) with the extension taken from the author's original filename (falling back to the URL when the name has none); `post.md` links each one under its original name when the author supplied one, so nothing readable is lost.

`index.md` is a clickable list of every tracked post (title → directory, comment counts, locked markers), sorted by directory name — chronological under the default `{date}_{title}` format. It is regenerated from `_state.json` at the end of every `--blog` download/sync run, including no-change runs, so deleting it self-heals (single-post `--url` downloads don't touch it). Don't edit it by hand.

Content block types b00p doesn't support yet (e.g. polls) are skipped with a per-post warning naming the type — if you see one, that content exists on Boosty but is not in your archive.

Native videos are downloaded only when Boosty offers a direct MP4 variant. A video published as HLS/DASH only is skipped with a `had no MP4 URL — only HLS/DASH variants` warning; the post still saves and counts as complete, and `--check-media` won't flag it afterwards because there is no local file to compare. That warning during the run is your only notice, so watch for it — grab such videos manually with yt-dlp if you need them.

## State Tracking

Each blog directory has a `_state.json` that records what's already downloaded, so repeat runs only fetch what's new. Don't hand-edit it; deleting it forces a full re-download (existing files are still skipped by the integrity check, so it's cheap). Sync checks the actual files on disk, not just this cache, so stale or partially-written files heal on the next run without any repair flag.

Locked posts aren't stored — upgrade your subscription and the next run downloads them. Downgrade, and b00p keeps the files you already have.

`_state.json` is version-stamped. An older b00p refuses to load a state file written by a newer one — saving it back would silently drop fields it doesn't understand — and aborts the run before downloading anything (`state file ... has schema version N, newer than this b00p understands`). Upgrade the binary, or point `--output` somewhere else.

## Reliability

- **Interrupted runs resume cleanly.** State is saved after each post and partial downloads pick up where they left off, so a killed or crashed run loses nothing — just re-run it. Existing complete files are skipped; empty partials are re-downloaded.
- **A killed process never corrupts your data.** `post.json`, `post.md`, `comments.json`, `_state.json` and `auth.json` are written to a temp file, fsynced, then renamed into place — Ctrl-C or a crash mid-write leaves the old file intact. Media uses temp + rename without the fsync (it would stall multi-gigabyte writes), so sudden power loss is the one case that can still leave a truncated media file; the next run re-downloads it.
- **Exit codes.** 0 on success, 1 on any hard error — including a run where 199 posts synced fine and one failed. A sync you cancel exits **0**, and so does one that cancels itself because `--yes` was missing, so cron alerting on exit status won't notice a sync that has silently applied nothing for weeks. Check the log line, not just the code.
- **Transient errors retry automatically** (network blips, 5xx, rate limits); permanent ones (expired links, deleted media, dead tokens) fail fast with a hint about the cause instead of hammering the server.
- **Don't run two b00p processes on the same blog at once** (e.g. a manual run overlapping a cron sync) — they can clobber each other's state. Nothing corrupts and it self-heals next run, but use `--workers N` for parallelism within a single run instead.

## External Videos

Embedded YouTube/VK/OK videos appear as links in `post.json` regardless. With `--download-external`, b00p invokes [yt-dlp](https://github.com/yt-dlp/yt-dlp) to fetch them. Failures are logged and skipped — they don't fail the post.

```bash
pip install yt-dlp
b00p download --blog username --download-external
```

If b00p logs `yt-dlp not found in PATH`, see [Troubleshooting](#yt-dlp-not-found-in-path).

## Troubleshooting

### `accessToken is empty` / `token refresh failed` / `token expired, refresh failed` / `401 refresh failed`

Your tokens are missing, expired, or the refresh attempt was rejected. Re-extract them:

1. Log in to [boosty.to](https://boosty.to).
2. DevTools (F12) → Application/Storage → Cookies → `https://boosty.to`.
3. Copy the `auth` cookie value (the whole JSON object).
4. Paste `accessToken` and `refreshToken` into `auth.json`, save, re-run.

### `API ... returned 401`

Same fix as the token errors above — the access token expired and the refresh token couldn't recover it. The log line names the failing URL.

### `API ... returned 403` / post shows up `[LOCKED]`

You don't have the required subscription tier for that post. Not a b00p error. If you upgrade later, the next `--sync` picks it up automatically (shown as `[UNLOCKED]`).

### `API ... returned 404`

The post or blog no longer exists (deleted or renamed). Check your `--blog`. b00p never deletes anything on its own — stale directories and state entries stay until you remove them.

### `yt-dlp not found in PATH`

You passed `--download-external` but yt-dlp isn't installed or on `PATH`. Install it with `pip install yt-dlp` and check `yt-dlp --version`. b00p looks for a `yt-dlp` **executable** on `PATH` and nothing else, so `py -m yt_dlp` is not a substitute — on Windows add the Python `Scripts` directory (usually `%APPDATA%\Python\Python3xx\Scripts`) to `PATH`.

### `--sync` does nothing in cron / systemd

In a headless run (cron, Task Scheduler, SSH without a terminal) there's no one to answer the `Apply changes? [y/N]` prompt, so b00p cancels without applying. Add `--yes` to apply automatically:

```bash
b00p download --blog username --sync --yes
```

### Some posts always show fewer comments than Boosty

The limit is on **threads**, not on the total: a post with more than 100 top-level comment threads, or a single thread with more than 100 replies, can't be fully fetched. That's a hard limit in Boosty's API, not a b00p bug — a post with 300 comments spread over 40 threads is archived in full, so check the thread count before assuming anything is missing.

Once a post does hit the cap, b00p marks it and **stops re-fetching its comments entirely** — it shows up under `N no changes` in every later sync even as the thread keeps growing. That's deliberate: the disk count can never catch up to the API count, so the alternative is firing `[COMMENTS]` forever with no path to closure. To force one fresh fetch, delete that post's `comments.json` and re-run `--sync` (an edit by the author also re-pulls them).

### Download fails with `status 403`/`400`/`410` and "signed URL likely expired"

Boosty's video links expire and are tied to your IP, so long download queues can hit dead ones. Just re-run the sync — it fetches fresh links. If it keeps happening, lower `--workers` so each post finishes before its links expire.

### `yt-dlp failed for ...: timed out after 20m0s`

An external video took longer than the 20-minute limit. The post still saves without it. If one URL keeps timing out, grab it manually with `yt-dlp <url>`.

### `N post(s) failed; see log above`

At least one post couldn't be completed, so the run exits 1 even if everything else synced. A post is written to `_state.json` only once every requested artefact landed, which means a post with a permanently dead media URL (deleted server-side) never gets recorded: it re-appears as `[NEW]` on every run, fails on the same file, and keeps the exit code non-zero forever. The rest of the archive is unaffected and the files that did download are kept. There's no flag to acknowledge such a post — if it bothers a cron job, filter on the log line rather than the exit code.

### `failed to save state: ... Access is denied` on Windows

Windows Defender briefly locked a file mid-write. No data is lost (the post just re-downloads next run). If it happens often, exclude the output directory from Defender's real-time scanning.

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
        // Audio/file attachment URLs are served unsigned — attach the
        // post-level signed query before downloading them.
        parser.ApplySignedQuery(parsed.Media, post.SignedQuery)

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

`FetchComments(blog, postID, limit)` yields top-level comments (replies are inlined per item, up to `reply_limit=100`), but unlike `FetchPosts` it returns a **single page**: the Boosty comments endpoint ignores `offset>0`, so pagination is impossible — size `limit` to cover every top-level thread you expect.

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

CI runs on every push to `master` and every pull request against it — pushes to other branches run nothing. The Linux job gates on `go vet`, [staticcheck](https://staticcheck.dev/), [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) and `go test -race`; the Windows job on `go vet` and `go test -race`. Any of them failing fails the build, so run staticcheck and govulncheck locally before opening a PR if you don't want a surprise.

`-race` needs CGO and therefore a C toolchain — on Windows without gcc on `PATH` it fails with `cgo: C compiler "gcc" not found`. Plain `go test ./...` still works there; CI covers the race detector on both platforms.

## License

MIT
