package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	authPath  string
	outputDir string
)

var rootCmd = &cobra.Command{
	Use:   "b00p",
	Short: "Boosty.to content parser and downloader",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&authPath, "auth", "auth.json", "path to auth.json with tokens")
	rootCmd.PersistentFlags().StringVarP(&outputDir, "output", "o", "output", "output directory")
}
