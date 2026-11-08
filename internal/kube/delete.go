package kube

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/mutate"
	"github.com/lucashalbert/axi-sdk-go/output"
)

// deleteCmd is "delete", or "plan delete" when preview is set: the same code,
// always in dry-run mode, without --confirm and --dry-run.
func (g *globals) deleteCmd(preview bool) *cobra.Command {
	var confirm, propagation, selector string
	var all bool
	var dry dryRunFlag
	short := "Delete one named object; needs --confirm KIND/NAME"
	if preview {
		short = "Preview deleting an object without changing anything"
	}
	cmd := &cobra.Command{
		Use:   "delete KIND/NAME --confirm KIND/NAME",
		Short: short,
		Long: "Delete one object by name. This cannot be undone.\n" +
			"--confirm must equal the resolved target exactly, lower-case kind and name, for example deployment/web; otherwise the command stops with a usage error that names the token and nothing is read or deleted.\n" +
			"The plan (context, namespace, resource and the delete) is printed first; deleting a namespace or a CustomResourceDefinition adds a WARNING line.\n" +
			"The delete carries the UID that was read, so an object recreated since is not removed. --propagation sets how dependents are deleted (default background).\n" +
			"There is no delete by selector, by --all or by wildcard. A namespaced target needs -n or a namespace set on the kubeconfig context. The kubeconfig is never changed.",
		Example: "  k8s-axi delete deploy/web -n prod --confirm deployment/web\n  k8s-axi delete deploy/web -n prod --dry-run=server\n  k8s-axi plan delete pvc/data -n prod",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := mutate.DryRun
			if !preview {
				var err error
				if mode, err = dry.mode(cmd); err != nil {
					return err
				}
			}
			if all || selector != "" {
				return axierr.Usage(cmd.CommandPath(), "delete by --all or by selector is not supported", "name the object, for example 'k8s-axi delete deployment/web --confirm deployment/web'")
			}
			policy, err := parsePropagation(cmd, propagation)
			if err != nil {
				return err
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			return g.deleteObject(cmd, args, confirm, policy, mode, f)
		},
	}
	cmd.Flags().StringVar(&propagation, "propagation", "background", `how dependents are deleted: "background", "foreground" or "orphan"`)
	cmd.Flags().BoolVar(&all, "all", false, "not supported: name the object")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "not supported: name the object")
	_ = cmd.Flags().MarkHidden("all")
	_ = cmd.Flags().MarkHidden("selector")
	if !preview {
		cmd.Flags().StringVar(&confirm, "confirm", "", "the exact target to delete, KIND/NAME in lower case as the plan prints it (required)")
		dry.bind(cmd)
	}
	return cmd
}

func parsePropagation(cmd *cobra.Command, v string) (metav1.DeletionPropagation, error) {
	switch v {
	case "background":
		return metav1.DeletePropagationBackground, nil
	case "foreground":
		return metav1.DeletePropagationForeground, nil
	case "orphan":
		return metav1.DeletePropagationOrphan, nil
	}
	return "", axierr.Usage(cmd.CommandPath(), fmt.Sprintf("invalid --propagation %q", v), `use "background" (default), "foreground" or "orphan"`)
}

type deleteResult struct {
	Target      string `json:"target"`
	Status      string `json:"status"` // deleted, terminating, requested or validated
	Propagation string `json:"propagation"`
	Note        string `json:"note,omitempty"`
}

type deleteOut struct {
	Result deleteResult `json:"result"`
	Next   []string     `json:"next,omitempty"`
}

func (g *globals) deleteObject(cmd *cobra.Command, args []string, confirm string, policy metav1.DeletionPropagation, mode mutate.Mode, f output.Format) error {
	ctx, cancel := g.bounded(cmd.Context())
	defer cancel()
	if _, name, err := splitTarget(args); err == nil && strings.ContainsAny(name, "*?") {
		return axierr.Usage(cmd.CommandPath(), fmt.Sprintf("%q is a pattern; delete takes one exact name", name), "name the object; wildcards are not supported")
	}
	t, err := g.prepare(ctx, cmd, args, func(Resource) error { return nil })
	if err != nil {
		return err
	}
	// The token is checked before the object is read, so a refused delete
	// touches nothing.
	// (Authorize only checks plans that change something, hence the placeholder.)
	early := mutate.Plan{Operation: "delete", Target: t.id(), Destructive: true, Changes: []string{"delete " + t.id()}}
	if err := (mutate.Authorizer{Confirm: confirm}).Authorize(early, mode); err != nil {
		return err
	}
	live, err := t.ri.Get(ctx, t.name, metav1.GetOptions{})
	if err != nil {
		return apiError("delete", t.apiTarget(), err, "get", t.r.String())
	}
	plan := mutate.Plan{Operation: "delete", Target: t.id(), Scope: t.scope(), Destructive: true,
		Changes: deleteChanges(t, live, policy)}
	out := cmd.OutOrStdout()
	send := func(dry bool) error {
		uid := live.GetUID()
		opts := metav1.DeleteOptions{PropagationPolicy: &policy, Preconditions: &metav1.Preconditions{UID: &uid}}
		if dry {
			opts.DryRun = dryRunAll
		}
		if err := t.ri.Delete(ctx, t.name, opts); err != nil {
			return apiError("delete", t.apiTarget(), err, "delete", t.r.String())
		}
		res := deleteResult{Target: plan.Target, Status: "validated", Propagation: strings.ToLower(string(policy))}
		next := []string{g.deleteLine(t, policy)}
		if !dry {
			res.Status, res.Note = afterDelete(ctx, t, live)
			next = []string{t.command(g, "get", t.r.String())}
			if res.Status == "terminating" {
				next = []string{t.command(g, "inspect", t.hint())}
			}
		}
		return output.Render(out, deleteOut{Result: res, Next: next}, f)
	}
	return mutate.Run(ctx, mutate.Options{
		Plan: plan, Mode: mode, Format: f, Out: out,
		Auth:     mutate.Authorizer{Confirm: confirm},
		Validate: func(context.Context) error { return send(true) },
		Apply:    func(context.Context) error { return send(false) },
	})
}

// deleteLine is the command that carries out the delete, for next hints.
func (g *globals) deleteLine(t mutationTarget, policy metav1.DeletionPropagation) string {
	parts := []string{"delete", t.hint(), "--confirm", t.id()}
	if policy != metav1.DeletePropagationBackground {
		parts = append(parts, "--propagation", strings.ToLower(string(policy)))
	}
	return t.command(g, parts...)
}

// deleteChanges lists the delete and what makes it matter: warnings for kinds
// that take others with them, finalizers that delay removal and propagation
// choices that leave or hold things behind.
func deleteChanges(t mutationTarget, live *unstructured.Unstructured, policy metav1.DeletionPropagation) []string {
	changes := []string{fmt.Sprintf("delete %s%s (propagation: %s)", t.id(), nsFlag(t.ns), strings.ToLower(string(policy)))}
	switch {
	case t.r.GVR.Group == "" && t.r.GVR.Resource == "namespaces":
		changes = append(changes, "WARNING: deleting a namespace deletes every object in it")
	case t.r.GVR.Group == "apiextensions.k8s.io" && t.r.GVR.Resource == "customresourcedefinitions":
		kind, _, _ := unstructured.NestedString(live.Object, "spec", "names", "kind")
		group, _, _ := unstructured.NestedString(live.Object, "spec", "group")
		changes = append(changes, fmt.Sprintf("WARNING: deleting a CustomResourceDefinition deletes every %s.%s object in the cluster, in all namespaces", kind, group))
	}
	if fin := live.GetFinalizers(); len(fin) > 0 {
		changes = append(changes, "notable: finalizers ["+strings.Join(fin, ", ")+"] must finish before the object is removed")
	}
	switch policy {
	case metav1.DeletePropagationForeground:
		changes = append(changes, "notable: foreground propagation keeps the object until its dependents are deleted")
	case metav1.DeletePropagationOrphan:
		changes = append(changes, "notable: orphan propagation leaves the dependents behind, without an owner")
	}
	return changes
}

// afterDelete reads the object once more to say whether it is gone.
func afterDelete(ctx context.Context, t mutationTarget, was *unstructured.Unstructured) (status, note string) {
	cur, err := t.ri.Get(ctx, t.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return "deleted", ""
	case err != nil:
		return "requested", "the delete was accepted; the object could not be read back: " + redactedError(err).Error()
	case cur.GetUID() != was.GetUID():
		return "deleted", "an object with this name exists again (new uid), recreated by a controller or another writer"
	case len(cur.GetFinalizers()) > 0:
		return "terminating", "removal waits for finalizers: " + strings.Join(cur.GetFinalizers(), ", ")
	}
	return "terminating", "the object is still being removed"
}
