package main

import "github.com/wpt/b00p/cmd"

// version is set at release build time by goreleaser
// (-X main.version={{.Version}}, see .goreleaser.yml).
var version = "dev"

func main() {
	cmd.Execute(version)
}
