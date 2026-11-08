// Package app assembles the k8s-axi command line.
package app

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"

	"github.com/lucashalbert/k8s-axi/internal/kube"
)

const name = "k8s-axi"

// New builds the root command.
func New(info update.Info) *cobra.Command {
	return cli.New(name, info, cli.Repo("lucashalbert/k8s-axi"), cli.EnvPrefix("K8S_AXI_"), kube.Module{})
}

// Run executes the CLI with args and returns the process exit code.
func Run(info update.Info, args []string, stdout, stderr io.Writer) int {
	root := New(info)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return cli.Execute(root)
}
