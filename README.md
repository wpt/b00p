# b00p

CLI parser and content downloader for [boosty.to](https://boosty.to). Downloads posts, images, videos, comments. Can also be used as a Go library.

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

1. Log in to [boosty.to](https://boosty.to) in your browser
2. Open DevTools (F12) → Application → Cookies → `https://boosty.to`
3. Copy the values of `auth` cookie — it contains a JSON object with `accessToken` and `refreshToken`
4. Create `auth.json`:

```bash
cp auth.json.example auth.json
```

Paste your tokens:

```json
{
  "accessToken": "paste_access_token_here",
  "refreshToken": "paste_refresh_token_here"
}
```

> Tokens auto-refresh on 401. `auth.json` is in `.gitignore`.

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

Shows subscription info and blog statistics.

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

# Re-download everything (ignore state)
b00p download --blog username --force

# Custom directory name format
b00p download --blog username --format "{date:ymd}_{title}"

# Download external videos (YouTube, VK) via yt-dlp
b00p download --blog username --download-external

# Concurrent downloads (3 posts at a time)
b00p download --blog username --workers 3
```

### sync

Smart sync: checks for updates, shows a diff, asks for confirmation before applying.

```bash
# Check for new posts, edits, new comments
b00p download --blog username --sync

# Also validate video file sizes
b00p download --blog username --sync --check-media

# Verify all expected files exist on disk
b00p download --blog username --sync --check-files
```

Example output:

```
Syncing username...
  [NEW] New post title
  [UNLOCKED] Previously locked post (was locked, now accessible)
  [UPDATED] Edited post (post edited)
  [COMMENTS] Post with discussion (comments: 5 → 8)
  [LOCKED] Downgraded post (was accessible, now locked)

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
- **NEW** — post not previously downloaded
- **UNLOCKED** — post was locked, now accessible (subscription upgraded)
- **UPDATED** — post was edited by the author (`updatedAt` changed)
- **COMMENTS** — new comments added
- **VIDEO_MISMATCH** — local video file size differs from remote (with `--check-media`)
- **FILES_MISSING** — expected files missing on disk (with `--check-files`)
- **LOCKED** — post was accessible, now locked (subscription downgraded). Data on disk is preserved.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--auth` | `auth.json` | Path to token file |
| `-o, --output` | `output` | Output directory |
| `--md` | `false` | Generate markdown file with frontmatter (includes tier/price) |
| `--comments` | `false` | Download comments |
| `--download-external` | `false` | Download external videos via yt-dlp |
| `--force` | `false` | Ignore state, re-download everything |
| `--sync` | `false` | Smart sync with diff and confirmation |
| `--check-media` | `false` | With `--sync`: validate video file sizes |
| `--check-files` | `false` | With `--sync`: verify post.json, comments.json, post.md exist on disk |
| `--format` | `{date}_{title}` | Post directory name format |
| `--workers` | `1` | Number of concurrent downloads |

## Directory Name Format

Variables for `--format`:

| Variable | Example | Description |
|----------|---------|-------------|
| `{title}` | `Stream #87` | Post title |
| `{date}` | `2026-03-13` | Publish date (ISO) |
| `{date:ymd}` | `20260313` | Date with custom format |
| `{date:d.m.y}` | `13.03.2026` | y=year, m=month, d=day |
| `{id}` | `e24c0343-...` | Post UUID |

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
```

## State Tracking

b00p tracks downloaded posts in `_state.json`. Each entry includes:
- Post title and directory name
- Download timestamp
- `updatedAt` and comment count (for sync diffing)
- Price and subscription tier required
- Locked status

On subsequent runs, only new posts are downloaded. Locked posts are not saved to state — after upgrading your subscription, they are automatically downloaded on the next run. Downgraded posts keep their data on disk and are marked as locked.

## Reliability

- **Retry with backoff**: all network operations retry 3 times with increasing delays (5s / 15s / 30s)
- **Integrity check**: existing non-empty files are skipped, 0-byte partial files are cleaned up
- **Incremental state saves**: state is written after each post, so interrupted downloads resume where they left off
- **Spinner**: animated progress indicator with file size during downloads (`⠹ video_001.mp4 45.2 MB / 1.2 GB (3.7%)`)
- **Clear error messages**: expired tokens show instructions to update `auth.json`

## External Videos

Posts may contain embedded videos from YouTube, VK, OK. By default b00p only saves the link in markdown. With `--download-external` it downloads them via [yt-dlp](https://github.com/yt-dlp/yt-dlp):

```bash
pip install yt-dlp
b00p download --blog username --download-external
```

## Library Usage

```go
import (
    "github.com/wpt/b00p/pkg/boosty"
    "github.com/wpt/b00p/pkg/parser"
)

// Create client
tokens, _ := boosty.LoadTokens("auth.json")
client := boosty.NewClient(tokens, "auth.json")

// Fetch a post
var post boosty.Post
client.GetJSON(boosty.PostURL("blogname", "post-id"), &post)

// Parse content
parsed := parser.ParseBlocks(post.Data)
for _, text := range parsed.TextParts {
    fmt.Println(text)
}
for _, media := range parsed.Media {
    fmt.Println(media.Type, media.URL)
}

// Access tier info
if post.SubscriptionLevel != nil {
    fmt.Println("Tier:", post.SubscriptionLevel.Name)
}
fmt.Println("Price:", post.Price)
```

## Tests

```bash
go test ./... -v
```

## License

MIT
