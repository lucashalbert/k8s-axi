package kube

import (
	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
)

type clusterInfoOut struct {
	Context   string `json:"context"`
	Server    string `json:"server"`
	Version   string `json:"version"`
	Namespace string `json:"namespace"`
	Reachable bool   `json:"reachable"`
}

func (g *globals) clusterCmd() *cobra.Command {
	cluster := &cobra.Command{Use: "cluster", Short: "Inspect the target cluster"}
	cluster.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Show the context, API server, server version and namespace",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return g.clusterInfo(cmd) },
	})
	return cluster
}

func (g *globals) clusterInfo(cmd *cobra.Command) error {
	f, err := cli.Format(cmd)
	if err != nil {
		return err
	}
	c, err := g.clients()
	if err != nil {
		return err
	}
	v, err := c.Discovery.ServerVersion()
	if err != nil {
		return axierr.Runtime("cluster info", "context/"+c.Context+" ("+c.Server+")", err,
			"check that the API server is reachable and the credentials are valid, or raise --timeout ("+g.timeout.String()+")")
	}
	return output.Render(cmd.OutOrStdout(), clusterInfoOut{
		Context: c.Context, Server: c.Server, Version: v.GitVersion, Namespace: c.Namespace, Reachable: true,
	}, f)
}
