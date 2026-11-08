package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/mutate"
	"github.com/lucashalbert/axi-sdk-go/output"
)

// restartedAtKey is the pod template annotation that triggers a rollout,
// the same one kubectl sets.
const restartedAtKey = "kubectl.kubernetes.io/restartedAt"

func restartable(r Resource) bool {
	if r.GVR.Group != "apps" {
		return false
	}
	switch r.GVR.Resource {
	case "deployments", "statefulsets", "daemonsets":
		return true
	}
	return false
}

// rolloutCmd is "rollout restart", or "plan rollout restart" when preview is set.
func (g *globals) rolloutCmd(preview bool) *cobra.Command {
	rollout := &cobra.Command{Use: "rollout", Short: "Change how a workload rolls out"}
	var dry dryRunFlag
	restart := &cobra.Command{
		Use:   "restart KIND/NAME",
		Short: "Restart a deployment, statefulset or daemonset",
		Long: "Restart the pods of a workload by setting the " + restartedAtKey + " annotation on its pod template, which starts a rollout under the workload's update strategy.\n" +
			"The plan (context, namespace, resource and the annotation value) is printed before the change.\n" +
			"A namespaced target needs -n or a namespace set on the kubeconfig context. The kubeconfig is never changed.",
		Example: "  k8s-axi rollout restart deploy/web -n prod\n  k8s-axi rollout restart sts/db -n prod --dry-run=server",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := mutate.DryRun
			if !preview {
				var err error
				if mode, err = dry.mode(cmd); err != nil {
					return err
				}
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			return g.restart(cmd, args, mode, preview, f)
		},
	}
	if preview {
		restart.Short = "Preview a restart without changing anything"
	} else {
		dry.bind(restart)
	}
	rollout.AddCommand(restart)
	return rollout
}

func (g *globals) restart(cmd *cobra.Command, args []string, mode mutate.Mode, preview bool, f output.Format) error {
	t, err := g.prepare(cmd, args, func(r Resource) error {
		if restartable(r) {
			return nil
		}
		return axierr.Usage(cmd.CommandPath(), "rollout restart supports deployments, statefulsets and daemonsets, not "+r.String(),
			"use 'k8s-axi scale' to change replicas, or pick a deployment, statefulset or daemonset")
	})
	if err != nil {
		return err
	}
	ctx, cancel := g.bounded(cmd.Context())
	defer cancel()
	cur, err := t.ri.Get(ctx, t.name, metav1.GetOptions{})
	if err != nil {
		return apiError("rollout restart", t.apiTarget(), err, "get", t.r.String())
	}
	if paused, _, _ := unstructured.NestedBool(cur.Object, "spec", "paused"); paused {
		return axierr.Runtime("rollout restart", t.apiTarget(), errors.New("the deployment is paused, so a new template would not roll out"),
			"resume the deployment first, for example with 'kubectl rollout resume'")
	}

	stamp := now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{restartedAtKey: stamp}}}}})
	if err != nil {
		return err
	}
	applyLine := t.command(g, "rollout", "restart", t.hint())
	return g.runMutation(cmd, f, mutation{
		plan: mutate.Plan{Operation: "rollout restart", Target: t.id(), Scope: t.scope(), Changes: restartChanges(cur, stamp)},
		mode: mode, tolerant: preview, verb: "patch", t: t,
		exec: func(ctx context.Context, dry bool) (mutationResult, error) {
			o := metav1.PatchOptions{FieldManager: fieldManager}
			if dry {
				o.DryRun = dryRunAll
			}
			if _, err := t.ri.Patch(ctx, t.name, types.MergePatchType, body, o); err != nil {
				return mutationResult{}, err
			}
			return mutationResult{RestartedAt: stamp}, nil
		},
		dryNext:   []string{applyLine},
		applyNext: []string{t.command(g, "inspect", t.hint()), t.command(g, "events", t.hint())},
	})
}

// restartChanges lists what restarting cur does, calling out update
// strategies and sizes where nothing, or everything at once, restarts.
func restartChanges(cur *unstructured.Unstructured, stamp string) []string {
	set := fmt.Sprintf("set pod template annotation %s=%s", restartedAtKey, stamp)
	if was, ok, _ := unstructured.NestedString(cur.Object, "spec", "template", "metadata", "annotations", restartedAtKey); ok {
		set += " (was " + was + ")"
	}
	changes := []string{set}

	kind := cur.GetKind()
	var pods int64
	if kind == "DaemonSet" {
		pods, _, _ = unstructured.NestedInt64(cur.Object, "status", "desiredNumberScheduled")
	} else if n, ok, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas"); ok {
		pods = n
	} else {
		pods = 1 // the API default
	}

	strategyPath := []string{"spec", "updateStrategy", "type"}
	if kind == "Deployment" {
		strategyPath = []string{"spec", "strategy", "type"}
	}
	strategy, _, _ := unstructured.NestedString(cur.Object, strategyPath...)

	switch {
	case pods == 0 && kind != "DaemonSet":
		return append(changes, "notable: 0 replicas, no running pod is restarted")
	case strategy == "OnDelete":
		return append(changes, "notable: updateStrategy is OnDelete, no pod is replaced until it is deleted")
	case strategy == "Recreate":
		return append(changes, fmt.Sprintf("replaces %d pod(s)", pods),
			"notable: Recreate strategy, all pods stop before new ones start (downtime)")
	}
	return append(changes, fmt.Sprintf("replaces %d pod(s) with a rolling update", pods))
}
