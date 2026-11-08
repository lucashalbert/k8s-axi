package kube

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/mutate"
	"github.com/lucashalbert/axi-sdk-go/output"
)

// scaleCmd is "scale", or "plan scale" when preview is set: the same code,
// always in dry-run mode and without a --dry-run flag.
func (g *globals) scaleCmd(preview bool) *cobra.Command {
	var replicas int
	var dry dryRunFlag
	short := "Set the replica count of a workload"
	if preview {
		short = "Preview scaling a workload without changing anything"
	}
	cmd := &cobra.Command{
		Use:   "scale KIND/NAME --replicas N",
		Short: short,
		Long: "Set the replica count through the scale subresource, found by discovery, so deployments, statefulsets, replicasets and custom resources that declare one all work.\n" +
			"The plan (context, namespace, resource and the replica change) is printed before the change. Scaling to zero is reported as notable.\n" +
			"A namespaced target needs -n or a namespace set on the kubeconfig context. The kubeconfig is never changed.",
		Example: "  k8s-axi scale deploy/web --replicas 5 -n prod\n  k8s-axi scale statefulset/db --replicas 0 -n prod --dry-run=server",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := mutate.DryRun
			if !preview {
				var err error
				if mode, err = dry.mode(cmd); err != nil {
					return err
				}
			}
			if !cmd.Flags().Changed("replicas") {
				return axierr.Usage(cmd.CommandPath(), "--replicas is required", "pass --replicas N, for example --replicas 3")
			}
			if replicas < 0 {
				return axierr.Usage(cmd.CommandPath(), fmt.Sprintf("--replicas must be zero or more, got %d", replicas), "pass --replicas 0 to scale to zero")
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			return g.scale(cmd, args, int64(replicas), mode, preview, f)
		},
	}
	cmd.Flags().IntVar(&replicas, "replicas", 0, "desired number of replicas (required)")
	if !preview {
		dry.bind(cmd)
	}
	return cmd
}

func (g *globals) scale(cmd *cobra.Command, args []string, want int64, mode mutate.Mode, preview bool, f output.Format) error {
	ctx, cancel := g.bounded(cmd.Context())
	defer cancel()
	t, err := g.prepare(ctx, cmd, args, func(Resource) error { return nil })
	if err != nil {
		return err
	}
	ok, err := t.c.hasScale(ctx, t.r)
	if err != nil {
		return apiError("scale", t.apiTarget(), err, "get", "discovery")
	}
	if !ok {
		return axierr.Usage("scale", t.r.Kind+" ("+t.r.String()+") has no scale subresource",
			"scale works on kinds with a scale subresource, such as deployments, statefulsets and replicasets; run 'k8s-axi discover --kind "+t.r.GVR.Resource+"'")
	}
	cur, err := t.ri.Get(ctx, t.name, metav1.GetOptions{}, "scale")
	if err != nil {
		return apiError("scale", t.apiTarget(), err, "get", t.r.String()+"/scale")
	}
	have, _, _ := unstructured.NestedInt64(cur.Object, "spec", "replicas")

	applyLine := t.command(g, "scale", t.hint(), "--replicas", strconv.FormatInt(want, 10))
	return g.runMutation(ctx, cmd, f, mutation{
		plan: mutate.Plan{Operation: "scale", Target: t.id(), Scope: t.scope(), Changes: scaleChanges(have, want)},
		mode: mode, tolerant: preview, verb: "update", t: t,
		exec: func(ctx context.Context, dry bool) (mutationResult, error) {
			o := metav1.UpdateOptions{FieldManager: fieldManager}
			if dry {
				o.DryRun = dryRunAll
			}
			out, err := updateScale(ctx, t, cur, have, want, o)
			if err != nil {
				return mutationResult{}, err
			}
			obs, _, _ := unstructured.NestedInt64(out.Object, "status", "replicas")
			return mutationResult{Replicas: &want, Observed: &obs}, nil
		},
		dryNext:   []string{applyLine},
		applyNext: []string{t.command(g, "inspect", t.hint()), t.command(g, "events", t.hint())},
	})
}

// scaleChanges lists what scaling from have to want changes.
func scaleChanges(have, want int64) []string {
	if have == want {
		return nil
	}
	c := []string{fmt.Sprintf("replicas %d -> %d", have, want)}
	if want == 0 {
		c = append(c, "notable: scales to zero, no pods will run and the workload stops serving")
	}
	return c
}

// hasScale reports whether discovery lists a scale subresource for r.
func (c *Clients) hasScale(ctx context.Context, r Resource) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dc := discovery.ToDiscoveryInterfaceWithContext(c.Discovery)
	l, err := withContext(ctx, func() (*metav1.APIResourceList, error) {
		return dc.ServerResourcesForGroupVersionWithContext(ctx, r.GVR.GroupVersion().String())
	})
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, ar := range l.APIResources {
		if ar.Name == r.GVR.Resource+"/scale" {
			return true, nil
		}
	}
	return false, nil
}

// scaleRetries bounds how often a conflicting update is retried.
const scaleRetries = 3

// updateScale writes the replica count. The Scale carries the resourceVersion
// it was read at, but that is the workload's own, which controllers bump with
// every status write. A conflict is therefore retried against a fresh read as
// long as the replica count is still the one the plan was made from. If
// someone else changed it, the plan is stale and the conflict is returned.
func updateScale(ctx context.Context, t mutationTarget, cur *unstructured.Unstructured, have, want int64, o metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	for attempt := 0; ; attempt++ {
		s := cur.DeepCopy()
		if err := unstructured.SetNestedField(s.Object, want, "spec", "replicas"); err != nil {
			return nil, err
		}
		out, err := t.ri.Update(ctx, s, o, "scale")
		if err == nil || !apierrors.IsConflict(err) || attempt >= scaleRetries {
			return out, err
		}
		fresh, gerr := t.ri.Get(ctx, t.name, metav1.GetOptions{}, "scale")
		if gerr != nil {
			return nil, gerr
		}
		if n, _, _ := unstructured.NestedInt64(fresh.Object, "spec", "replicas"); n != have {
			return nil, err
		}
		cur = fresh
	}
}
