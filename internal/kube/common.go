package kube

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// scopeNamespace is the namespace a command reads: empty for all namespaces.
// -n together with -A is ambiguous and rejected.
func scopeNamespace(cmd *cobra.Command, c *Clients, all bool) (string, error) {
	if !all {
		return c.Namespace, nil
	}
	if cmd.Flags().Changed("namespace") {
		return "", axierr.Usage(cmd.CommandPath(), "-n and -A cannot be used together", "use -n NAMESPACE or -A, not both")
	}
	return "", nil
}

// cmdline renders a k8s-axi command for a next-step hint, carrying the
// --context and --kubeconfig the current command was given so the hint
// targets the same cluster.
func (g *globals) cmdline(parts ...string) string {
	s := "k8s-axi " + strings.Join(parts, " ")
	if g.context != "" {
		s += " --context " + g.context
	}
	if g.kubeconfig != "" {
		s += " --kubeconfig " + g.kubeconfig
	}
	return s
}

// nsFlag is " -n NS" for a namespaced object, else empty.
func nsFlag(ns string) string {
	if ns == "" {
		return ""
	}
	return " -n " + ns
}

// truncate cuts s to at most n runes, ending in an ellipsis when it was cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// argRange validates the positional argument count as a usage error (exit 2).
func argRange(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) >= min && len(args) <= max {
			return nil
		}
		want := fmt.Sprintf("%d to %d arguments", min, max)
		if min == max {
			want = fmt.Sprintf("%d argument(s)", min)
		}
		return axierr.Usage(cmd.CommandPath(), fmt.Sprintf("expected %s, got %d", want, len(args)),
			"run '"+cmd.CommandPath()+" --help' for usage")
	}
}
