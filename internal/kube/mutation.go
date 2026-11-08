package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/mutate"
	"github.com/lucashalbert/axi-sdk-go/output"
)

// fieldManager names k8s-axi in the API server's managed fields.
const fieldManager = "k8s-axi"

// dryRunFlag is --dry-run. Only the server can validate a change, so "client"
// is refused instead of being treated as a weaker equivalent.
type dryRunFlag struct{ value string }

func (d *dryRunFlag) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&d.value, "dry-run", "none",
		`"none" applies the change; "server" sends it to the API server with dryRun=All and changes nothing`)
}

func (d *dryRunFlag) mode(cmd *cobra.Command) (mutate.Mode, error) {
	switch d.value {
	case "none":
		return mutate.Apply, nil
	case "server":
		return mutate.DryRun, nil
	case "client":
		return 0, axierr.Usage(cmd.CommandPath(), "--dry-run=client is not supported: client-side dry-run does not validate against the API server",
			"use --dry-run=server, or '"+planPath(cmd)+"' to preview the change")
	default:
		return 0, axierr.Usage(cmd.CommandPath(), fmt.Sprintf("invalid --dry-run value %q", d.value), `use "none" or "server"`)
	}
}

// planPath is the plan command that previews cmd, for hints.
func planPath(cmd *cobra.Command) string {
	return strings.Replace(cmd.CommandPath(), cmd.Root().Name(), cmd.Root().Name()+" plan", 1)
}

// bounded limits a whole command to --timeout.
func (g *globals) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, g.timeout)
}

type contextCallResult[T any] struct {
	value T
	err   error
}

func withContext[T any](ctx context.Context, call func() (T, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	done := make(chan contextCallResult[T], 1)
	go func() {
		value, err := call()
		done <- contextCallResult[T]{value: value, err: err}
	}()
	select {
	case result := <-done:
		return result.value, result.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// mutationTarget is the object a mutation acts on, resolved and scoped.
type mutationTarget struct {
	c    *Clients
	r    Resource
	ns   string // empty for cluster-scoped kinds
	name string
	ri   dynamic.ResourceInterface
}

// id is the plan's Target, for example "deployment/web".
func (t mutationTarget) id() string { return strings.ToLower(t.r.Kind) + "/" + t.name }

// hint is RESOURCE/NAME in the fully resolved form commands accept back.
func (t mutationTarget) hint() string {
	return t.r.String() + "/" + t.name
}

// apiTarget names the object in an error.
func (t mutationTarget) apiTarget() string {
	return "context/" + t.c.Context + " " + t.r.String() + "/" + t.name + nsFlag(t.ns)
}

// scope bounds the change in the printed plan.
func (t mutationTarget) scope() map[string]string {
	s := map[string]string{"context": t.c.Context, "server": t.c.Server, "resource": t.r.String()}
	if t.ns != "" {
		s["namespace"] = t.ns
	}
	return s
}

// command is a k8s-axi command line aimed at this target, for next hints.
func (t mutationTarget) command(g *globals, parts ...string) string {
	if t.ns != "" {
		parts = append(parts, "-n", t.ns)
	}
	return g.cmdline(parts...)
}

// prepare reads the target from args, resolves it and checks the namespace.
// accept may reject a kind the command does not handle.
func (g *globals) prepare(ctx context.Context, cmd *cobra.Command, args []string, accept func(Resource) error) (mutationTarget, error) {
	kind, name, err := splitTarget(args)
	if err != nil {
		return mutationTarget{}, err
	}
	if name == "" {
		return mutationTarget{}, axierr.Usage(cmd.CommandPath(), "a name is required", "write the target as 'deployment/web' or 'deployment web'")
	}
	if err := ctx.Err(); err != nil {
		return mutationTarget{}, err
	}
	c, err := g.clients()
	if err != nil {
		return mutationTarget{}, err
	}
	if err := ctx.Err(); err != nil {
		return mutationTarget{}, err
	}
	r, err := c.ResolveContext(ctx, kind)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return mutationTarget{}, apiError("resolve kind", "context/"+c.Context+" "+kind, err, "get", "discovery")
		}
		return mutationTarget{}, err
	}
	if err := ctx.Err(); err != nil {
		return mutationTarget{}, err
	}
	if err := accept(r); err != nil {
		return mutationTarget{}, err
	}
	t := mutationTarget{c: c, r: r, name: name}
	switch {
	case !r.Namespaced && g.namespace != "":
		return t, axierr.Usage(cmd.CommandPath(), r.Kind+" is cluster-scoped and cannot take -n", "drop -n")
	case r.Namespaced && g.namespace == "" && !c.NamespaceFromContext:
		return t, axierr.Usage(cmd.CommandPath(), "the namespace is ambiguous: -n was not given and the context "+c.Context+" sets none",
			"pass -n NAMESPACE; a change is never sent to an assumed namespace")
	case r.Namespaced:
		t.ns = c.Namespace
	}
	t.ri = c.dynamicFor(r, t.ns)
	return t, nil
}

// mutationResult is what a mutation reports after it ran.
type mutationResult struct {
	Target      string `json:"target"`
	Status      string `json:"status"` // applied, validated or not-validated
	Replicas    *int64 `json:"replicas,omitempty"`
	Observed    *int64 `json:"observed_replicas,omitempty"`
	RestartedAt string `json:"restarted_at,omitempty"`
	Note        string `json:"note,omitempty"`
}

type mutationOut struct {
	Result mutationResult `json:"result"`
	Next   []string       `json:"next,omitempty"`
}

// mutation is one change, ready to run.
type mutation struct {
	plan     mutate.Plan
	mode     mutate.Mode
	tolerant bool // an API server without dry-run support is a note, not an error
	verb     string
	t        mutationTarget
	// exec sends the change; with dry set it asks the server to validate only.
	exec func(ctx context.Context, dry bool) (mutationResult, error)
	// applyNext and dryNext are the hints printed after an apply and after a dry run.
	applyNext, dryNext []string
}

// runMutation authorizes, prints the plan (target first) and carries it out
// through mutate.Run, then prints the result.
func (g *globals) runMutation(ctx context.Context, cmd *cobra.Command, f output.Format, m mutation) error {
	out := cmd.OutOrStdout()
	finish := func(dry bool, status, note string) error {
		res, err := m.exec(ctx, dry)
		if err != nil {
			if dry && m.tolerant && dryRunUnsupported(err) {
				return output.Render(out, mutationOut{Result: mutationResult{Target: m.plan.Target, Status: "not-validated",
					Note: "the API server does not support dry-run for this resource; the plan reflects the live state only"}, Next: m.dryNext}, f)
			}
			return apiError(m.plan.Operation, m.t.apiTarget(), err, m.verb, m.t.r.String())
		}
		res.Target, res.Status, res.Note = m.plan.Target, status, note
		next := m.applyNext
		if dry {
			next = m.dryNext
		}
		return output.Render(out, mutationOut{Result: res, Next: next}, f)
	}
	return mutate.Run(ctx, mutate.Options{
		Plan: m.plan, Mode: m.mode, Format: f, Out: out,
		Validate: func(context.Context) error { return finish(true, "validated", "") },
		Apply:    func(context.Context) error { return finish(false, "applied", "") },
	})
}

// dryRunUnsupported reports whether the server rejected a request because the
// resource or an admission webhook does not allow dry-run.
func dryRunUnsupported(err error) bool {
	if !apierrors.IsBadRequest(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dry run") || strings.Contains(msg, "dryrun")
}

// mutationCmds are the commands that change the cluster.
func (g *globals) mutationCmds() []*cobra.Command {
	return []*cobra.Command{g.scaleCmd(false), g.rolloutCmd(false), g.applyCmd(false), g.planCmd()}
}

// dryRunAll are the options of a server-side dry run.
var dryRunAll = []string{metav1.DryRunAll}
