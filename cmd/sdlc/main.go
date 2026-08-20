package main

import (
	"os"

	"github.com/vipinm/sdlc-orchestrator/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
