package kube

import "github.com/spf13/cobra"

// planCmd previews the mutating commands. Its subcommands are the same
// commands built in preview mode, so a preview and the real run can never
// disagree about what a change is.
func (g *globals) planCmd() *cobra.Command {
	plan := &cobra.Command{
		Use:   "plan",
		Short: "Preview a scale, rollout restart or apply without changing anything",
		Long: "Print what a scale, rollout restart or apply would change: cluster context, namespace, resource and the change itself.\n" +
			"The change is also sent to the API server with dryRun=All where it supports that, and the result says whether it was validated.\n" +
			"Nothing is ever changed, and the result ends with the exact command that applies the plan.",
		Example: "  k8s-axi plan scale deploy/web --replicas 0 -n prod\n  k8s-axi plan rollout restart sts/db -n prod\n  k8s-axi plan apply -f deploy.yaml -n prod",
	}
	plan.AddCommand(g.scaleCmd(true), g.rolloutCmd(true), g.applyCmd(true))
	return plan
}
