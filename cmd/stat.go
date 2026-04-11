package cmd

import (
	"fmt"
	"strings"

	"github.com/wpt/b00p/pkg/boosty"

	"github.com/spf13/cobra"
)

var statBlog string

var statCmd = &cobra.Command{
	Use:   "stat",
	Short: "Show blog statistics and current user info",
	RunE:  runStat,
}

func init() {
	statCmd.Flags().StringVar(&statBlog, "blog", "", "blog name")
	statCmd.MarkFlagRequired("blog")
	rootCmd.AddCommand(statCmd)
}

func runStat(cmd *cobra.Command, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}

	// Who is me
	fmt.Println("=== Who Is Me ===")
	var subs boosty.SubscriptionsResponse
	if err := c.GetJSON(boosty.UserSubscriptionsURL(), &subs); err != nil {
		fmt.Printf("  could not fetch subscriptions: %v\n", err)
	} else {
		found := false
		for _, sub := range subs.Data {
			if strings.EqualFold(sub.Blog.BlogURL, statBlog) {
				fmt.Printf("  Blog:   %s\n", sub.Blog.BlogURL)
				fmt.Printf("  Tier:   %s\n", sub.Name)
				fmt.Printf("  Price:  %d RUB\n", sub.Price)
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
			return err
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
