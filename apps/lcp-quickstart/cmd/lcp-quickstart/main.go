package main

import (
	"os"

	"github.com/bruwbird/lcp/apps/lcp-quickstart/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}

