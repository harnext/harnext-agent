package main

import (
	"fmt"
	"os"

	"github.com/harnext/harnext-agent/internal/cli"
	"github.com/harnext/harnext-agent/internal/service"
)

// Set at release build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	home, err := service.Home()
	if err != nil {
		fmt.Fprintln(os.Stderr, "harnext-agent: cannot locate the configuration directory")
		os.Exit(1)
	}
	root := cli.New(cli.Env{
		Paths: service.PathsIn(home), Version: version,
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
		ServerDefault: service.DefaultServerURL(),
	})
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "harnext-agent:", err)
		os.Exit(1)
	}
}
