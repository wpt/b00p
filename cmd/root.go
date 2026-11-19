package cmd

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
)

var (
	authPath  string
	outputDir string
)

var rootCmd = &cobra.Command{
	Use:   "b00p",
	Short: "Boosty.to content parser and downloader",
	// Silence Cobra's auto-printing — Execute() below renders the error
	// once. Without these, a runtime failure prints `Error: msg`, the
	// whole usage block, and then our own copy of the error.
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Execute runs the root command and exits with status 1 on error, printing
// the error once to stderr.
// Execute runs the root command; version is what --version prints (see
// resolveVersion).
func Execute(version string) {
	rootCmd.Version = resolveVersion(version)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&authPath, "auth", "auth.json", "path to auth.json with tokens")
	rootCmd.PersistentFlags().StringVarP(&outputDir, "output", "o", "output", "output directory")
}

// resolveVersion returns the release version stamped by goreleaser, or, for
// a `go install` build that has none, the module version Go recorded.
func resolveVersion(stamped string) string {
	if stamped != "dev" {
		return stamped
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return stamped
}
