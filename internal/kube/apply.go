package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/mutate"
	"github.com/lucashalbert/axi-sdk-go/output"
	"github.com/lucashalbert/axi-sdk-go/redact"
)

const (
	// pathsShown is how many changed field paths a plan line lists; --full lists all.
	pathsShown = 5
	// detailMax bounds an error message in a plan line or result.
	detailMax = 300
)

// servedPoll is how often apply asks discovery whether a kind created by the
// same manifest is served yet.
var servedPoll = 250 * time.Millisecond

// applyCmd is "apply", or "plan apply" when preview is set: the same code,
// always in dry-run mode and without a --dry-run flag.
func (g *globals) applyCmd(preview bool) *cobra.Command {
	var files []string
	var force bool
	var dry dryRunFlag
	short := "Create or update objects from manifests with server-side apply"
	if preview {
		short = "Preview applying manifests without changing anything"
	}
	cmd := &cobra.Command{
		Use:   "apply -f FILE|DIR|-",
		Short: short,
		Long: "Create or update the objects in YAML or JSON manifests with server-side apply under the field manager " + fieldManager + ".\n" +
			"-f takes a file, a directory (yaml, yml and json files, not recursive) or - for stdin, and may repeat; documents may be multi-document YAML, JSON or a List.\n" +
			"Every object is resolved through discovery, so built-in kinds and custom resources work, and a CRD may be in the same manifest as its custom resources.\n" +
			"The plan (context, namespace and per object whether it is created, configured or unchanged) is printed before the change.\n" +
			"A field owned by another manager is a conflict; only --force-conflicts takes ownership of such fields.\n" +
			"Secret values are never printed, only keys and sizes. Objects that fail are reported one by one and the exit code is 1.\n" +
			"A namespaced object takes its namespace from the manifest, then -n, then the kubeconfig context; falling back to default is a usage error. The kubeconfig is never changed.",
		Example: "  k8s-axi apply -f deploy.yaml -n prod\n  k8s-axi apply -f manifests/ --dry-run=server\n  cat app.yaml | k8s-axi apply -f - -n prod --force-conflicts",
		Args:    argRange(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			return g.apply(cmd, files, force, mode, f)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "manifest file, directory or - for stdin; repeatable (required)")
	cmd.Flags().BoolVar(&force, "force-conflicts", false, "take ownership of fields another manager owns; without it a conflict fails the object")
	if !preview {
		dry.bind(cmd)
	}
	return cmd
}

type applyState int

const (
	stateCreate applyState = iota
	stateConfigure
	stateUnchanged
	statePending // waits for a namespace or CRD created by the same manifest
	stateFailed
)

// applyItem is one object of a manifest on its way to the cluster.
type applyItem struct {
	obj    *unstructured.Unstructured
	source string
	r      Resource
	ns     string                    // empty for cluster-scoped kinds
	ri     dynamic.ResourceInterface // nil until the kind is served

	state     applyState
	exists    bool     // the live object was found
	paths     []string // changed field paths of a configure
	note      string   // "not validated: ..."
	after     string   // statePending: "namespace/x" or "customresourcedefinition/y"
	err       error
	verb      string // the RBAC verb behind err
	conflicts []string
}

func (it *applyItem) id() string { return strings.ToLower(it.obj.GetKind()) + "/" + it.obj.GetName() }

func (it *applyItem) where() string { return it.id() + nsFlag(it.ns) }

// applyJob is a prepared apply: resolved, assessed and ordered.
type applyJob struct {
	g     *globals
	c     *Clients
	files []string
	force bool
	order []*applyItem
}

func (g *globals) apply(cmd *cobra.Command, files []string, force bool, mode mutate.Mode, f output.Format) error {
	ctx, cancel := g.bounded(cmd.Context())
	defer cancel()
	objs, err := readManifests(files, cmd.InOrStdin())
	if err != nil {
		return err
	}
	c, err := g.clients()
	if err != nil {
		return err
	}
	items, err := g.resolveItems(ctx, cmd, c, objs)
	if err != nil {
		return err
	}
	declared := declaredNamespaces(items)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		it.assess(ctx, force, declared)
	}
	j := &applyJob{g: g, c: c, files: files, force: force, order: applyOrder(items)}
	out := cmd.OutOrStdout()
	plan := j.plan()
	err = mutate.Run(ctx, mutate.Options{
		Plan: plan, Mode: mode, Format: f, Out: out,
		Validate: func(context.Context) error { return j.report(out, f, j.validate(), true) },
		Apply:    func(ctx context.Context) error { return j.report(out, f, j.execute(ctx), false) },
	})
	if err != nil || len(plan.Changes) > 0 {
		return err
	}
	// Nothing to change: Run printed the no-op plan, the result lists why.
	if mode == mutate.DryRun {
		return j.report(out, f, j.validate(), true)
	}
	return j.report(out, f, j.execute(ctx), false)
}

// resolveItems maps every object to a served resource and a namespace, and
// rejects what cannot be planned: an unserved kind that the manifest does not
// create, an ambiguous namespace, or the same object twice.
func (g *globals) resolveItems(ctx context.Context, cmd *cobra.Command, c *Clients, objs []manifestObject) ([]*applyItem, error) {
	crds := manifestCRDs(objs)
	seen := map[string]string{}
	items := make([]*applyItem, 0, len(objs))
	for _, m := range objs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gvk := m.obj.GroupVersionKind()
		it := &applyItem{obj: m.obj, source: m.source}
		unstructured.RemoveNestedField(it.obj.Object, "metadata", "managedFields")
		r, err := c.resolveGVK(ctx, gvk)
		var unserved *notServedError
		switch {
		case err == nil:
		case errors.As(err, &unserved):
			crd, ok := crds[gvk.GroupKind()]
			if !ok {
				return nil, axierr.Usage(cmd.CommandPath(), fmt.Sprintf("%s: the cluster does not serve %s %s", m.source, gvk.GroupVersion(), gvk.Kind),
					"check apiVersion and kind, or list the served kinds with 'k8s-axi discover'; a CRD may be in the same manifest")
			}
			r, it.state, it.after = crd.resource(gvk), statePending, "customresourcedefinition/"+crd.name
		case errors.Is(err, context.DeadlineExceeded):
			return nil, apiError("resolve kind", "context/"+c.Context+" "+gvk.Kind, err, "get", "discovery")
		default:
			return nil, err
		}
		if it.ns, err = g.objectNamespace(cmd, c, r, m); err != nil {
			return nil, err
		}
		it.r = r
		it.obj.SetNamespace(it.ns)
		key := gvk.Group + "/" + gvk.Kind + "/" + it.ns + "/" + it.obj.GetName()
		if prev, dup := seen[key]; dup {
			return nil, axierr.Usage(cmd.CommandPath(), fmt.Sprintf("%s repeats %s, already given in %s", m.source, it.where(), prev), "name each object once")
		}
		seen[key] = m.source
		if it.state != statePending {
			it.ri = c.dynamicFor(r, it.ns)
		}
		items = append(items, it)
	}
	return items, nil
}

// objectNamespace is the namespace m is applied to: its own, else -n, else the
// context's. Cluster-scoped kinds take none, so a -n for the namespaced
// objects of the same manifest does not matter to them.
func (g *globals) objectNamespace(cmd *cobra.Command, c *Clients, r Resource, m manifestObject) (string, error) {
	if !r.Namespaced {
		return "", nil
	}
	own := m.obj.GetNamespace()
	switch {
	case own != "" && g.namespace != "" && own != g.namespace:
		return "", axierr.Usage(cmd.CommandPath(), fmt.Sprintf("%s has namespace %s but -n %s was given", m.source, own, g.namespace),
			"drop -n, or fix the manifest; a change is never sent to a namespace you did not name")
	case own != "":
		return own, nil
	case g.namespace == "" && !c.NamespaceFromContext:
		return "", axierr.Usage(cmd.CommandPath(), fmt.Sprintf("the namespace of %s is ambiguous: it sets none, -n was not given and the context %s sets none", m.source, c.Context),
			"pass -n NAMESPACE or set metadata.namespace; a change is never sent to an assumed namespace")
	}
	return c.Namespace, nil
}

// declaredNamespaces are the Namespaces the manifest itself creates.
func declaredNamespaces(items []*applyItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		if gvk := it.obj.GroupVersionKind(); gvk.Group == "" && gvk.Kind == "Namespace" {
			out[it.obj.GetName()] = true
		}
	}
	return out
}

// assess reads the live object and asks the server what an apply would
// store, which tells create from configure from unchanged exactly.
func (it *applyItem) assess(ctx context.Context, force bool, declared map[string]bool) {
	if it.state == statePending {
		return
	}
	live, err := it.ri.Get(ctx, it.obj.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		it.fail(err, "get")
		return
	}
	it.exists = err == nil
	opts := metav1.ApplyOptions{FieldManager: fieldManager, Force: force, DryRun: dryRunAll}
	proposed, err := it.ri.Apply(ctx, it.obj.GetName(), it.obj, opts)
	switch {
	case err == nil:
	case dryRunUnsupported(err):
		it.note = "not validated: the API server does not support dry-run for this resource"
		it.state = stateCreate
		if it.exists {
			it.state = stateConfigure
		}
		return
	case it.ns != "" && declared[it.ns] && strings.Contains(err.Error(), fmt.Sprintf("namespaces %q not found", it.ns)):
		it.state, it.after = statePending, "namespace/"+it.ns
		return
	default:
		it.fail(err, it.writeVerb())
		return
	}
	switch {
	case !it.exists:
		it.state = stateCreate
	default:
		if it.paths = changedPaths(live.Object, proposed.Object); len(it.paths) == 0 {
			it.state = stateUnchanged
		} else {
			it.state = stateConfigure
		}
	}
}

func (it *applyItem) writeVerb() string {
	if it.exists {
		return "patch"
	}
	return "create"
}

func (it *applyItem) fail(err error, verb string) {
	it.state, it.err, it.verb = stateFailed, err, verb
	it.conflicts = conflictsOf(err)
}

// message is the error as it may be printed: no credentials in URLs and, for
// a Secret, none of its values.
func (it *applyItem) message() string {
	return truncate(redact.Mask(scrubSecretValues(redactedError(it.err).Error(), it.obj)), detailMax)
}

func (it *applyItem) detail(full bool) string {
	if len(it.conflicts) > 0 {
		return "field conflict: " + pathList(it.conflicts, shown(full))
	}
	return it.message()
}

func shown(full bool) int {
	if full {
		return 1 << 30
	}
	return pathsShown
}

// hint says what to do about a failure.
func (it *applyItem) hint(c *Clients) string {
	if len(it.conflicts) > 0 {
		return "another manager owns those fields; re-run with --force-conflicts to take ownership, or remove them from the manifest"
	}
	var ae *axierr.Error
	if errors.As(apiError("apply", it.where(), it.err, it.verb, it.r.String()), &ae) {
		return ae.Hint
	}
	return ""
}

// applyOrder puts Namespaces first and CRDs second, the rest in manifest order,
// so what other objects need exists before they are applied.
func applyOrder(items []*applyItem) []*applyItem {
	rank := func(it *applyItem) int {
		switch gvk := it.obj.GroupVersionKind(); {
		case gvk.Group == "" && gvk.Kind == "Namespace":
			return 0
		case gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition":
			return 1
		}
		return 2
	}
	out := append([]*applyItem(nil), items...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

func (j *applyJob) target() string {
	if len(j.order) == 1 {
		return j.order[0].id()
	}
	return fmt.Sprintf("%d objects", len(j.order))
}

// plan lists what will change; unchanged objects are not changes.
func (j *applyJob) plan() mutate.Plan {
	scope := map[string]string{"context": j.c.Context, "server": j.c.Server}
	if j.g.namespace != "" || j.c.NamespaceFromContext {
		scope["namespace"] = j.c.Namespace
	}
	if j.force {
		scope["force_conflicts"] = "true"
	}
	p := mutate.Plan{Operation: "apply", Target: j.target(), Scope: scope}
	for _, it := range j.order {
		if line := it.planLine(j.g.full); line != "" {
			p.Changes = append(p.Changes, line)
		}
	}
	return p
}

func (it *applyItem) planLine(full bool) string {
	var line string
	switch it.state {
	case stateUnchanged:
		return ""
	case stateCreate:
		line = "create " + it.where()
	case stateConfigure:
		line = "configure " + it.where()
		if len(it.paths) > 0 {
			line += ": " + pathList(it.paths, shown(full))
		}
	case statePending:
		line = "create " + it.where()
	case stateFailed:
		if len(it.conflicts) > 0 {
			return "conflict " + it.where() + ": " + pathList(it.conflicts, shown(full))
		}
		return "error " + it.where() + ": " + it.message()
	}
	var extra []string
	if it.state == statePending {
		extra = append(extra, "after "+it.after)
	}
	if it.obj.GetKind() == "Secret" && it.obj.GroupVersionKind().Group == "" {
		extra = append(extra, secretKeys(it.obj))
	}
	if it.note != "" {
		extra = append(extra, it.note)
	}
	if len(extra) > 0 {
		line += " (" + strings.Join(extra, "; ") + ")"
	}
	return line
}

type applyObject struct {
	Object    string `json:"object"`
	Namespace string `json:"namespace,omitempty"`
	Status    string `json:"status"` // created, configured, unchanged, failed; would-create, would-configure in a dry run
	Detail    string `json:"detail,omitempty"`
}

type applyResult struct {
	Status     string        `json:"status"` // applied or validated
	Created    int           `json:"created"`
	Configured int           `json:"configured"`
	Unchanged  int           `json:"unchanged"`
	Failed     int           `json:"failed"`
	Objects    []applyObject `json:"objects"`
}

type applyOut struct {
	Result applyResult `json:"result"`
	Next   []string    `json:"next,omitempty"`
}

func (r *applyResult) add(o applyObject) {
	switch o.Status {
	case "created", "would-create":
		r.Created++
	case "configured", "would-configure":
		r.Configured++
	case "unchanged":
		r.Unchanged++
	default:
		r.Failed++
	}
	r.Objects = append(r.Objects, o)
}

// validate is the result of a dry run: what the server said in the plan phase.
func (j *applyJob) validate() applyResult {
	res := applyResult{Status: "validated"}
	for _, it := range j.order {
		o := applyObject{Object: it.id(), Namespace: it.ns, Detail: it.note}
		switch it.state {
		case stateUnchanged:
			o.Status = "unchanged"
		case stateCreate:
			o.Status = "would-create"
		case stateConfigure:
			o.Status = "would-configure"
		case statePending:
			o.Status, o.Detail = "would-create", "not validated: waits for "+it.after+", which this manifest creates"
		case stateFailed:
			o.Status, o.Detail = "failed", it.detail(j.g.full)
		}
		res.add(o)
	}
	return res
}

// execute applies every changed object, one at a time. A failure does not stop
// the others, except for objects that wait for what failed.
func (j *applyJob) execute(ctx context.Context) applyResult {
	res := applyResult{Status: "applied"}
	failed := map[string]bool{}
	for _, it := range j.order {
		o := applyObject{Object: it.id(), Namespace: it.ns}
		switch {
		case it.state == stateUnchanged:
			o.Status = "unchanged"
		case it.after != "" && failed[it.after]:
			o.Status, o.Detail = "failed", "skipped: "+it.after+" was not applied"
			failed[it.id()] = true
		case it.applyNow(ctx, j.c, j.force) != nil:
			o.Status, o.Detail = "failed", it.detail(j.g.full)
			failed[it.id()] = true
		case it.exists:
			o.Status = "configured"
		default:
			o.Status = "created"
		}
		res.add(o)
	}
	return res
}

// applyNow sends the real apply request. The error, if any, is kept on the
// item for the report.
func (it *applyItem) applyNow(ctx context.Context, c *Clients, force bool) error {
	it.err, it.conflicts = nil, nil
	if it.ri == nil {
		r, err := c.waitServed(ctx, it.obj.GroupVersionKind())
		if err != nil {
			it.fail(err, "get")
			return err
		}
		it.r, it.ri = r, c.dynamicFor(r, it.ns)
	}
	_, err := it.ri.Apply(ctx, it.obj.GetName(), it.obj, metav1.ApplyOptions{FieldManager: fieldManager, Force: force})
	if err != nil {
		it.fail(err, it.writeVerb())
	}
	return err
}

// report prints the result document and turns failures into the exit code.
func (j *applyJob) report(out io.Writer, f output.Format, res applyResult, dry bool) error {
	if err := output.Render(out, applyOut{Result: res, Next: j.next(res, dry)}, f); err != nil {
		return err
	}
	if res.Failed == 0 {
		return nil
	}
	var hint string
	for _, it := range j.order {
		if it.err != nil {
			hint = it.hint(j.c)
			break
		}
	}
	return axierr.Runtime("apply", j.target(), fmt.Errorf("%d of %d object(s) failed", res.Failed, len(j.order)), hint)
}

func (j *applyJob) next(res applyResult, dry bool) []string {
	if res.Created+res.Configured == 0 || res.Failed > 0 && dry {
		return nil
	}
	if dry {
		parts := []string{"apply"}
		for _, f := range j.files {
			parts = append(parts, "-f", f)
		}
		if j.force {
			parts = append(parts, "--force-conflicts")
		}
		if j.g.namespace != "" {
			parts = append(parts, "-n", j.g.namespace)
		}
		return []string{j.g.cmdline(parts...)}
	}
	for i, it := range j.order {
		if s := res.Objects[i].Status; s == "created" || s == "configured" {
			t := mutationTarget{c: j.c, r: it.r, ns: it.ns, name: it.obj.GetName()}
			return []string{t.command(j.g, "inspect", t.hint())}
		}
	}
	return nil
}

// manifestCRD is a CustomResourceDefinition the manifest itself creates.
type manifestCRD struct{ name, plural, scope string }

func manifestCRDs(objs []manifestObject) map[schema.GroupKind]manifestCRD {
	out := map[schema.GroupKind]manifestCRD{}
	for _, m := range objs {
		if gk := m.obj.GroupVersionKind().GroupKind(); gk.Group != "apiextensions.k8s.io" || gk.Kind != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(m.obj.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(m.obj.Object, "spec", "names", "kind")
		plural, _, _ := unstructured.NestedString(m.obj.Object, "spec", "names", "plural")
		scope, _, _ := unstructured.NestedString(m.obj.Object, "spec", "scope")
		out[schema.GroupKind{Group: group, Kind: kind}] = manifestCRD{name: m.obj.GetName(), plural: plural, scope: scope}
	}
	return out
}

func (d manifestCRD) resource(gvk schema.GroupVersionKind) Resource {
	return Resource{GVR: gvk.GroupVersion().WithResource(d.plural), Kind: gvk.Kind, Namespaced: d.scope == "Namespaced"}
}

// notServedError means discovery has no such group, version and kind.
type notServedError struct{ gvk schema.GroupVersionKind }

func (e *notServedError) Error() string {
	return fmt.Sprintf("the cluster does not serve %s %s", e.gvk.GroupVersion(), e.gvk.Kind)
}

// resolveGVK maps the apiVersion and kind of an object to a served resource
// through the REST mapper.
func (c *Clients) resolveGVK(ctx context.Context, gvk schema.GroupVersionKind) (Resource, error) {
	mapper := meta.ToRESTMapperWithContext(c.Mapper)
	m, err := withContext(ctx, func() (*meta.RESTMapping, error) {
		return mapper.RESTMappingWithContext(ctx, gvk.GroupKind(), gvk.Version)
	})
	if meta.IsNoMatchError(err) {
		return Resource{}, &notServedError{gvk}
	}
	if err != nil {
		return Resource{}, err
	}
	return Resource{GVR: m.Resource, Kind: gvk.Kind, Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}, nil
}

// waitServed resets the mapper and polls discovery until gvk is served, which
// follows shortly after its CRD is created. It gives up when ctx ends.
func (c *Clients) waitServed(ctx context.Context, gvk schema.GroupVersionKind) (Resource, error) {
	for {
		meta.MaybeResetRESTMapper(c.Mapper)
		r, err := c.resolveGVK(ctx, gvk)
		var unserved *notServedError
		if !errors.As(err, &unserved) {
			return r, err
		}
		select {
		case <-ctx.Done():
			return Resource{}, fmt.Errorf("%s is still not served: %w", gvk.Kind, ctx.Err())
		case <-time.After(servedPoll):
		}
	}
}
