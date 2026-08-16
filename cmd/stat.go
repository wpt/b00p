package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"

	"github.com/spf13/cobra"
)

var statBlog string

var statCmd = &cobra.Command{
	Use:   "stat",
	Short: "Show blog statistics and current user info",
	Args:  cobra.NoArgs,
	RunE:  runStat,
}

func init() {
	statCmd.Flags().StringVar(&statBlog, "blog", "", "blog name")
	if err := statCmd.MarkFlagRequired("blog"); err != nil {
		// Only fails if the flag does not exist; would be a programming
		// error, so panic during init rather than silently ignoring it.
		panic(fmt.Sprintf("MarkFlagRequired(blog): %v", err))
	}
	rootCmd.AddCommand(statCmd)
}

func runStat(cmd *cobra.Command, args []string) error {
	// Same slug contract as download's --blog: boosty.PostsURL interpolates the
	// name into the API path, so '/' or '?' would silently rewrite the endpoint.
	statBlog = strings.TrimSpace(statBlog)
	if err := validateBlogName(statBlog); err != nil {
		return fmt.Errorf("invalid --blog %q: %v", statBlog, err)
	}

	c, err := newClient()
	if err != nil {
		return err
	}

	// Who is me
	fmt.Println("=== Who Is Me ===")
	var subs boosty.SubscriptionsResponse
	if err := c.GetJSON(boosty.UserSubscriptionsURL(), &subs); err != nil {
		c.Log.Printf("  warning: could not fetch subscriptions: %v", err)
		fmt.Println("  Subscription info unavailable")
	} else {
		found := false
		for _, sub := range subs.Data {
			if strings.EqualFold(sub.Blog.BlogURL, statBlog) {
				fmt.Printf("  Blog:   %s\n", sub.Blog.BlogURL)
				fmt.Printf("  Tier:   %s\n", sub.Name)
				fmt.Printf("  Price:  %g RUB\n", sub.Price)
				if sub.IsPaused {
					fmt.Println("  Status: PAUSED")
				} else {
					fmt.Println("  Status: Active")
				}
				found = true
				break
			}
		}
		if !found {
			fmt.Printf("  No active subscription to %s\n", statBlog)
		}
	}

	// Blog stats
	fmt.Printf("\n=== Blog: %s ===\n", statBlog)

	totalPosts := 0
	accessible := 0
	locked := 0

	for post, err := range c.FetchPosts(statBlog, 20) {
		if err != nil {
			// Same abort-vs-skip contract as DownloadAll/Sync: a page-level
			// failure kills the run, a single malformed post is skipped so
			// one drifted JSON shape doesn't kill stats for the whole blog.
			if errors.Is(err, boosty.ErrFetchPage) {
				return err
			}
			c.Log.Printf("  warning: skipping malformed post: %v", err)
			continue
		}
		totalPosts++
		if post.HasAccess {
			accessible++
		} else {
			locked++
		}
	}

	fmt.Printf("  Total posts:  %d\n", totalPosts)
	fmt.Printf("  Accessible:   %d\n", accessible)
	fmt.Printf("  Locked:       %d\n", locked)

	return nil
}
