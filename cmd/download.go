package cmd

import (
	"fmt"
	"regexp"

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
	boostyURLRe = regexp.MustCompile(`boosty\.to/([^/]+)/posts/([^/?#]+)`)
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
	if blogName == "" && postURL == "" {
		return fmt.Errorf("specify --blog or --url")
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	if postURL != "" {
		matches := boostyURLRe.FindStringSubmatch(postURL)
		if matches == nil {
			return fmt.Errorf("invalid boosty URL: %s", postURL)
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
		_, err := syncer.New(c, buildConfig(blog)).SavePost(&post)
		return err
	}

	e := syncer.New(c, buildConfig(blogName))
	if syncMode {
		return e.Sync()
	}
	return e.DownloadAll()
}
