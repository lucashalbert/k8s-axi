// Command k8s-axi is an agent-ergonomic CLI for the Kubernetes API.
package main

import (
	"os"

	"github.com/lucashalbert/axi-sdk-go/update"

	"github.com/lucashalbert/k8s-axi/internal/app"
)

// Set at build time by GoReleaser: -X main.version=... -X main.commit=... -X main.date=...
var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	info := update.Info{Version: version, Commit: commit, Date: date}
	os.Exit(app.Run(info, os.Args[1:], os.Stdout, os.Stderr))
}
