package kube

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
	"github.com/lucashalbert/axi-sdk-go/redact"
)

// Home view states.
const (
	stateReady            = "ready"
	stateUnreachable      = "unreachable"
	stateNoKubeconfig     = "no-kubeconfig"
	stateNoCurrentContext = "no-current-context"
)

// homeOut is the content-first view printed when k8s-axi runs with no
// arguments. The cluster fields mirror 'cluster info'.
type homeOut struct {
	State           string   `json:"state"`
	Context         string   `json:"context,omitempty"`
	Server          string   `json:"server,omitempty"`
	Version         string   `json:"version,omitempty"`
	Namespace       string   `json:"namespace,omitempty"`
	Reachable       bool     `json:"reachable"`
	Error           string   `json:"error,omitempty"`
	Contexts        []string `json:"contexts,omitempty"`
	ContextsOmitted int      `json:"contexts_omitted,omitempty"`
	Next            []string `json:"next"`
}

// home prints live cluster data for the resolved context, or the kubeconfig
// state when there is no context to resolve. Like diagnose, it exits 0
// whenever it produced a report, so an unreachable cluster or a missing
// kubeconfig is data rather than failure; an explicit --kubeconfig or
// --context that does not resolve, or a kubeconfig that does not parse, is
// still the usual usage or runtime error.
func (g *globals) home(cmd *cobra.Command) error {
	f, err := cli.Format(cmd)
	if err != nil {
		return err
	}
	out, err := g.homeView()
	if err != nil {
		return err
	}
	return output.Render(cmd.OutOrStdout(), out, f)
}

func (g *globals) homeView() (homeOut, error) {
	c, err := g.clients()
	var nc *noContextError
	switch {
	case errors.As(err, &nc):
		return g.configHome(nc), nil
	case err != nil:
		return homeOut{}, err
	}
	out := homeOut{Context: c.Context, Server: c.Server, Namespace: c.Namespace}
	v, err := serverVersion(c)
	if err != nil {
		out.State, out.Error = stateUnreachable, redact.Mask(redactedError(err).Error())
		out.Next = g.hints("k8s-axi cluster info --timeout 60s", "k8s-axi --help")
		return out, nil
	}
	out.State, out.Version, out.Reachable = stateReady, v, true
	out.Next = g.hints("k8s-axi get pods", "k8s-axi events --type Warning", "k8s-axi diagnose deploy/NAME", "k8s-axi --help")
	return out, nil
}

// configHome reports a kubeconfig with no context to act on.
func (g *globals) configHome(nc *noContextError) homeOut {
	out := homeOut{State: stateNoKubeconfig, Next: []string{
		"k8s-axi --kubeconfig PATH",
		"k8s-axi --help",
	}}
	if len(nc.names) == 0 {
		return out
	}
	names := nc.names
	out.State = stateNoCurrentContext
	if g.limit > 0 && len(names) > g.limit {
		out.ContextsOmitted, names = len(names)-g.limit, names[:g.limit]
	}
	out.Contexts = names
	out.Next = []string{"k8s-axi --context " + names[0], "k8s-axi --help"}
	return out
}

// hints returns the commands with the --context flag the view was resolved
// with, so each one targets the cluster the view describes.
func (g *globals) hints(cmds ...string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c
		if g.context != "" && c != "k8s-axi --help" {
			out[i] = c + " --context " + g.context
		}
	}
	return out
}

// noContextError marks a Load failure where the kubeconfig has no context to
// use and none was asked for: the kubeconfig is absent or empty (no names),
// or it has contexts but no current one.
type noContextError struct{ names []string }

func (*noContextError) Error() string { return "no usable kubeconfig context" }
