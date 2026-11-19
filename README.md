# b00p

[![tag](https://img.shields.io/github/v/tag/wpt/b00p?sort=semver&label=tag)](https://github.com/wpt/b00p/tags)
[![CI](https://github.com/wpt/b00p/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/wpt/b00p/actions/workflows/ci.yml)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/wpt/b00p/gh-pages/coverage.json)](https://github.com/wpt/b00p/actions/workflows/ci.yml)

Command-line downloader for [boosty.to](https://boosty.to). Archives the posts your subscriptions give you access to and keeps the archive up to date.

- Post data, optional Markdown and comments
- Images, audio, file attachments and native videos in the best MP4 quality
- Embedded YouTube/VK videos via [yt-dlp](https://github.com/yt-dlp/yt-dlp) (optional)
- Incremental runs; `--sync` also picks up edits, new comments and access changes, reviewed before applying
- Resumable downloads and parallel workers

## Install

Download the binary for your platform from [Releases](https://github.com/wpt/b00p/releases) (`b00p_linux_amd64`, `b00p_windows_amd64.exe`, `b00p_darwin_arm64`, …), rename it to `b00p` (`b00p.exe` on Windows) and put it on your `PATH`. On Linux and macOS, run `chmod +x b00p`.

Or build from source with Go 1.27.4+:

```bash
go install github.com/wpt/b00p@latest
```

## Authentication

b00p uses your browser session:

1. Log in to boosty.to and open DevTools (F12) → Application → Cookies → `https://boosty.to`.
2. Copy the value of the `auth` cookie.
3. Save it as `auth.json` in the directory you run b00p from, or pass its path with `--auth`. The percent-encoded value (`%7B%22accessToken…`) works as is.

b00p refreshes the tokens itself when they expire (about a month). For that it needs the browser's device id: add `"deviceId": "<value>"` to `auth.json` (b00p rewrites it as plain JSON on the first run), with the value of `_clientId` from the same Cookies list or from Local Storage. Don't use the same `auth.json` on two machines: each refresh invalidates the other copy.

Check that it works:

```bash
b00p stat --blog <blog>
```

## Usage

`<blog>` is the name from the blog address, `boosty.to/<blog>`.

```bash
# Everything you have access to; repeat runs fetch only new posts
b00p download --blog <blog>

# Also save post.md and comments, three posts in parallel
b00p download --blog <blog> --md --comments --workers 3

# A single post
b00p download --url https://boosty.to/<blog>/posts/<id>

# Show new posts, edits, new comments and access changes, then ask to apply
b00p download --blog <blog> --sync

# The same without the prompt, for cron or Task Scheduler
b00p download --blog <blog> --sync --yes
```

| Flag | Description |
|---|---|
| `--blog NAME` | Download a blog |
| `--url URL` | Download a single post |
| `--md` | Write `post.md` (Markdown with frontmatter) |
| `--comments` | Write `comments.json` |
| `--download-external` | Download embedded videos with yt-dlp |
| `--sync` | Show what changed and ask before applying |
| `--yes` | With `--sync`: apply without asking |
| `--check-files` | With `--sync`: restore missing `post.json`, `post.md` and `comments.json` |
| `--check-media` | With `--sync`: re-download media files whose size differs from the server |
| `--force` | Process every post again, not only new ones |
| `--workers N` | Posts processed in parallel (default 1) |
| `--since DATE` | Skip posts published before DATE (`YYYY-MM-DD`) that are not downloaded yet |
| `--format FMT` | Folder name for new posts (default `{date}_{title}`); existing folders keep theirs |
| `-o, --output DIR` | Output directory (default `output`) |
| `--auth FILE` | Token file (default `auth.json`) |

`--format` placeholders: `{date}` (`2026-03-13`), `{date:ymd}` or `{date:d.m.y}` (any order of `y`, `m`, `d`), `{title}`, `{id}`.

`--md` and `--comments` apply to the posts downloaded in that run. To add them to an existing archive, run once with `--force`.

## Output

```
output/<blog>/
  _state.json                what has been downloaded; don't edit
  index.md                   list of all posts
  2026-03-13_Post title/
    post.json                full post data
    post.md                  with --md
    comments.json            with --comments
    image_001.jpg
    video_001.mp4
    audio_001.mp3
    file_001.pdf
    external_Youtube_<id>.*  with --download-external
```

## Notes

- Progress and errors go to stderr: use `2> log.txt` to save them.
- Deleted a post folder or some media? `b00p download --blog <blog> --sync` downloads them again.- Comments are saved for up to about 100 threads per post: Boosty's comments API can't page.

## Library

The packages under `github.com/wpt/b00p/pkg/` can be imported: `boosty` (API client), `parser` (post content) and `syncer` (the download and sync engine). Reference: [pkg.go.dev](https://pkg.go.dev/github.com/wpt/b00p).

```go
tokens, err := boosty.LoadTokens("auth.json")
if err != nil {
    log.Fatal(err)
}
client := boosty.NewClient(tokens, "auth.json")

for post, err := range client.FetchPosts("blogname", 50) {
    if err != nil {
        log.Fatal(err)
    }
    parsed := parser.ParseBlocks(post.Data)
    fmt.Println(post.Title, len(parsed.Media), "media files")
}
```

## Development

```bash
go vet ./...
go test ./...
```

## License

MIT
