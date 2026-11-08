package kube

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// scopeNamespace is the namespace a command reads: empty for all namespaces or
// cluster-scoped resources. -n together with -A is ambiguous for namespaced resources.
func scopeNamespace(cmd *cobra.Command, c *Clients, all, namespaced bool) (string, error) {
	if !namespaced {
		return "", nil
	}
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
	s := commandHint(parts...)
	if g.context != "" {
		s += " --context " + shellArg(g.context)
	}
	if g.kubeconfig != "" {
		s += " --kubeconfig " + shellArg(g.kubeconfig)
	}
	return s
}

func commandHint(parts ...string) string {
	return "k8s-axi " + shellJoin(parts)
}

func shellJoin(parts []string) string {
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, shellArg(p))
	}
	return strings.Join(quoted, " ")
}

func shellArg(s string) string {
	if s != "" && shellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func shellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}

// nsFlag is " -n NS" for a namespaced object, else empty.
func nsFlag(ns string) string {
	if ns == "" {
		return ""
	}
	return " -n " + shellArg(ns)
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
