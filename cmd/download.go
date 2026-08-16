package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"
	"github.com/wpt/b00p/pkg/parser"
	"github.com/wpt/b00p/pkg/syncer"

	"github.com/spf13/cobra"
)

var (
	blogName         string
	postURL          string
	withMD           bool
	withComments     bool
	downloadExternal bool
	forceDownload    bool
	syncMode         bool
	checkMedia       bool
	checkFilesFlag   bool
	autoApply        bool
	dirFormat        string
	numWorkers       int
)

var downloadCmd = &cobra.Command{
	Use:   "download",
	Short: "Download posts from a boosty blog",
	Args:  cobra.NoArgs,
	RunE:  runDownload,
}

func init() {
	downloadCmd.Flags().StringVar(&blogName, "blog", "", "blog name to download all posts")
	downloadCmd.Flags().StringVar(&postURL, "url", "", "single post URL to download")
	downloadCmd.Flags().BoolVar(&withMD, "md", false, "generate markdown file")
	downloadCmd.Flags().BoolVar(&withComments, "comments", false, "download comments")
	downloadCmd.Flags().BoolVar(&downloadExternal, "download-external", false, "download external videos via yt-dlp")
	downloadCmd.Flags().BoolVar(&forceDownload, "force", false, "re-download all posts ignoring state")
	downloadCmd.Flags().BoolVar(&syncMode, "sync", false, "sync mode: check for updates, show diff, confirm before applying")
	downloadCmd.Flags().BoolVar(&checkMedia, "check-media", false, "with --sync: also validate video file sizes via HEAD requests")
	downloadCmd.Flags().BoolVar(&checkFilesFlag, "check-files", false, "with --sync: verify post.json, comments.json, post.md exist on disk")
	downloadCmd.Flags().BoolVar(&autoApply, "yes", false, "with --sync: skip the interactive confirmation and apply changes")
	downloadCmd.Flags().StringVar(&dirFormat, "format", parser.DefaultFormat, "directory name format: {date}, {date:FORMAT}, {title}, {id}")
	downloadCmd.Flags().IntVar(&numWorkers, "workers", 1, "number of concurrent downloads")
	rootCmd.AddCommand(downloadCmd)
}

// blogSlugChars is the --url capture charset. It deliberately admits shapes
// validateBlogName rejects ("..", "alice.") so the user sees the precise
// reason, not a generic "invalid boosty URL" — every capture MUST be validated.
const blogSlugChars = `[A-Za-z0-9._-]{1,64}`

// maxBlogNameLen caps the slug below FS component limits (ASCII, bytes == runes).
const maxBlogNameLen = 64

var (
	// boostyURLRe is anchored at the start so the host is the real host —
	// unanchored, any string CONTAINING "boosty.to/x/posts/y" matched too
	// (evilboosty.to, boosty.to inside another URL's query/fragment) and the
	// run proceeded to a confusing API 404 instead of a clean rejection.
	boostyURLRe = regexp.MustCompile(`^(?:https?://)?(?:www\.|m\.)?boosty\.to/(` + blogSlugChars + `)/posts/([A-Za-z0-9_-]+)(?:/)?(?:[?#].*)?$`)

	// blogStructRe allows dots only between runs of safe characters: ".",
	// "..", ".hidden", "alice." can never validate — the slug lands in
	// filepath.Join, where those traverse or trip the Windows trailing-dot strip.
	blogStructRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
)

// validateBlogName is the single slug contract for --blog and the --url
// capture. Windows reserved device names (con, nul) are deliberately NOT
// rejected: modern Windows creates them fine and a reject would block a real
// blog on every platform. Errors are bare reasons; callers prefix context.
func validateBlogName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty")
	case len(name) > maxBlogNameLen:
		return fmt.Errorf("longer than %d characters", maxBlogNameLen)
	case !blogStructRe.MatchString(name):
		return fmt.Errorf("allowed: ASCII letters, digits, '_', '-', and '.' between them")
	}
	return nil
}

func newClient() (*boosty.Client, error) {
	if authPath == "" {
		return nil, fmt.Errorf("--auth cannot be empty")
	}
	tokens, err := boosty.LoadTokens(authPath)
	if err != nil {
		return nil, err
	}
	c := boosty.NewClient(tokens, authPath)
	c.Log = &stdLogger{}
	return c, nil
}

// buildConfig converts the CLI flags into a syncer.Config bound to blog.
// blog is passed in (rather than reading the global) so the URL-mode caller
// can substitute the blog parsed out of the post URL.
func buildConfig(blog string) syncer.Config {
	return syncer.Config{
		Blog:             blog,
		OutputDir:        outputDir,
		DirFormat:        dirFormat,
		WithMD:           withMD,
		WithComments:     withComments,
		DownloadExternal: downloadExternal,
		Force:            forceDownload,
		CheckMedia:       checkMedia,
		CheckFiles:       checkFilesFlag,
		AutoApply:        autoApply,
		Workers:          numWorkers,
	}
}

func runDownload(cmd *cobra.Command, args []string) error {
	// Values pasted from chat/docs into a quoted arg routinely carry edge
	// whitespace. A leading space would fail the anchored regex with a
	// rejection message where the space is invisible; a trailing space
	// would ride into the post-id capture and produce a confusing API 404.
	postURL = strings.TrimSpace(postURL)
	blogName = strings.TrimSpace(blogName)
	if blogName == "" && postURL == "" {
		return fmt.Errorf("specify --blog or --url")
	}
	if blogName != "" && postURL != "" {
		return fmt.Errorf("--blog and --url are mutually exclusive")
	}
	if blogName != "" {
		if err := validateBlogName(blogName); err != nil {
			return fmt.Errorf("invalid --blog %q: %v", blogName, err)
		}
	}
	if outputDir == "" {
		return fmt.Errorf("--output cannot be empty")
	}
	if numWorkers < 1 {
		return fmt.Errorf("--workers must be >= 1")
	}

	// Flag-combination guards: silently no-op flags train the user to add
	// them defensively without understanding what they do. Fail loud.
	if postURL != "" {
		var bad []string
		if syncMode {
			bad = append(bad, "--sync")
		}
		if checkMedia {
			bad = append(bad, "--check-media")
		}
		if checkFilesFlag {
			bad = append(bad, "--check-files")
		}
		if autoApply {
			bad = append(bad, "--yes")
		}
		if forceDownload {
			bad = append(bad, "--force")
		}
		if cmd.Flags().Changed("workers") {
			bad = append(bad, "--workers")
		}
		if len(bad) > 0 {
			return fmt.Errorf("--url is single-post and cannot be combined with: %v", bad)
		}
	}
	if !syncMode {
		var bad []string
		if checkMedia {
			bad = append(bad, "--check-media")
		}
		if checkFilesFlag {
			bad = append(bad, "--check-files")
		}
		if autoApply {
			bad = append(bad, "--yes")
		}
		if len(bad) > 0 {
			return fmt.Errorf("requires --sync: %v", bad)
		}
	}
	if syncMode && forceDownload {
		return fmt.Errorf("--force is only honored without --sync; sync already detects what needs updating")
	}

	// Parse and validate the URL before touching auth so a bad URL fails fast.
	var urlBlog, urlPostID string
	if postURL != "" {
		matches := boostyURLRe.FindStringSubmatch(postURL)
		if matches == nil {
			return fmt.Errorf("invalid boosty URL: %s (expected https://boosty.to/{blog}/posts/{post-id})", postURL)
		}
		urlBlog, urlPostID = matches[1], matches[2]
		// Sole guard between the loose capture ("..") and filepath.Join.
		if err := validateBlogName(urlBlog); err != nil {
			return fmt.Errorf("invalid blog %q in --url: %v", urlBlog, err)
		}
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	if postURL != "" {
		var post boosty.Post
		if err := c.GetJSON(boosty.PostURL(urlBlog, urlPostID), &post); err != nil {
			return fmt.Errorf("fetch post: %w", err)
		}
		// Stub guard: per-post endpoint may return a degraded payload (no
		// access, empty Data) when the post is locked or the subscription
		// lapsed. SavePost would otherwise write an empty post.json + zero-
		// length post.md against a stub. Same guard as fetchFullPost and
		// MaybeRefreshSignedURLs apply on the sync path.
		if !post.HasAccess || len(post.Data) == 0 {
			return fmt.Errorf("post %s not accessible (locked, deleted, or subscription lapsed)", urlPostID)
		}
		_, _, err := syncer.New(c, buildConfig(urlBlog)).SavePost(&post)
		return err
	}

	e := syncer.New(c, buildConfig(blogName))
	if syncMode {
		return e.Sync()
	}
	return e.DownloadAll()
}
