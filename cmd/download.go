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

var (
	// boostyURLRe is anchored at the start so the host is the real host —
	// unanchored, any string CONTAINING "boosty.to/x/posts/y" matched too
	// (evilboosty.to, boosty.to inside another URL's query/fragment) and the
	// run proceeded to a confusing API 404 instead of a clean rejection.
	boostyURLRe = regexp.MustCompile(`^(?:https?://)?(?:www\.|m\.)?boosty\.to/([^/]+)/posts/([^/?#]+)`)
	// blogNameRe limits the URL-derived blog slug to safe path characters.
	// Boosty usernames are alphanumeric + hyphen + underscore in practice;
	// the regex blocks "..", "CON", and any FS-significant character before
	// it reaches filepath.Join. Without this a hostile post URL could
	// redirect output into a parent directory.
	blogNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func newClient() (*boosty.Client, error) {
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
	// URLs pasted from chat/docs into a quoted arg routinely carry edge
	// whitespace. A leading space would fail the anchored regex with a
	// rejection message where the space is invisible; a trailing space
	// would ride into the post-id capture and produce a confusing API 404.
	postURL = strings.TrimSpace(postURL)
	if blogName == "" && postURL == "" {
		return fmt.Errorf("specify --blog or --url")
	}
	if blogName != "" && postURL != "" {
		return fmt.Errorf("--blog and --url are mutually exclusive")
	}
	if blogName != "" && !blogNameRe.MatchString(blogName) {
		return fmt.Errorf("invalid --blog %q: must match %s", blogName, blogNameRe)
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
		if numWorkers > 1 {
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

	c, err := newClient()
	if err != nil {
		return err
	}

	if postURL != "" {
		matches := boostyURLRe.FindStringSubmatch(postURL)
		if matches == nil {
			return fmt.Errorf("invalid boosty URL: %s (expected https://boosty.to/{blog}/posts/{post-id})", postURL)
		}
		blog := matches[1]
		postID := matches[2]
		if !blogNameRe.MatchString(blog) {
			return fmt.Errorf("invalid blog name in URL %q: must match %s", blog, blogNameRe)
		}

		var post boosty.Post
		if err := c.GetJSON(boosty.PostURL(blog, postID), &post); err != nil {
			return fmt.Errorf("fetch post: %w", err)
		}
		// Stub guard: per-post endpoint may return a degraded payload (no
		// access, empty Data) when the post is locked or the subscription
		// lapsed. SavePost would otherwise write an empty post.json + zero-
		// length post.md against a stub. Same guard as fetchFullPost and
		// MaybeRefreshSignedURLs apply on the sync path.
		if !post.HasAccess || len(post.Data) == 0 {
			return fmt.Errorf("post %s not accessible (locked, deleted, or subscription lapsed)", postID)
		}
		_, _, err := syncer.New(c, buildConfig(blog)).SavePost(&post)
		return err
	}

	e := syncer.New(c, buildConfig(blogName))
	if syncMode {
		return e.Sync()
	}
	return e.DownloadAll()
}
