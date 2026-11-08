package kube

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
)

const (
	inspectEvents = 10 // related events shown
	maxOwnerDepth = 5
	maxOwnerRows  = 10
)

type ownerRow struct {
	Depth      int    `json:"depth"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Controller bool   `json:"controller"`
	Note       string `json:"note,omitempty"`
}

type relatedEvents struct {
	Count     int    `json:"count"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
	Items     any    `json:"items"`
}

type inspectOut struct {
	Kind        string            `json:"kind"`
	Resource    string            `json:"resource"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Created     string            `json:"created"`
	Age         string            `json:"age"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Details     any               `json:"details"`
	Conditions  []conditionRow    `json:"conditions"`
	Owners      []ownerRow        `json:"owners"`
	Events      relatedEvents     `json:"events"`
	Object      map[string]any    `json:"object,omitempty"`
	Next        []string          `json:"next,omitempty"`
}

func (g *globals) inspectCmd() *cobra.Command {
	var labels, annotations bool
	cmd := &cobra.Command{
		Use:   "inspect KIND NAME",
		Short: "Summarise one object: key facts, conditions, owners and related events",
		Long: "Summarise one object of any kind the cluster serves: identity and age, a per-kind summary, status conditions,\n" +
			"the chain of owners (a Pod's ReplicaSet and Deployment), and the newest events about it.\n" +
			"Labels and annotations are left out unless asked for. --full adds the complete object.\n" +
			"Secret values are never printed, only key names and sizes.",
		Example: "  k8s-axi inspect pod web-6d5f8 -n prod\n  k8s-axi inspect deploy/web --labels\n  k8s-axi inspect widgets.example.com w1 --full",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := splitTarget(args)
			if err != nil {
				return err
			}
			if name == "" {
				return axierr.Usage("inspect", "an object needs a name: KIND NAME or KIND/NAME", "write 'k8s-axi inspect pod web' or 'k8s-axi inspect pod/web'")
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			c, err := g.clients()
			if err != nil {
				return err
			}
			r, err := c.Resolve(kind)
			if err != nil {
				return err
			}
			ns := c.Namespace
			if !r.Namespaced {
				ns = ""
			}
			u, err := c.Get(cmd.Context(), r, ns, name)
			if err != nil {
				return apiError("inspect", "context/"+c.Context+" "+r.String()+"/"+name+nsFlag(ns), err, "get", r.String())
			}
			return output.Render(cmd.OutOrStdout(), g.inspect(cmd.Context(), c, r, u, labels, annotations), f)
		},
	}
	cmd.Flags().BoolVar(&labels, "labels", false, "include the object's labels")
	cmd.Flags().BoolVar(&annotations, "annotations", false, "include the object's annotations")
	return cmd
}

func (g *globals) inspect(ctx context.Context, c *Clients, r Resource, u *unstructured.Unstructured, withLabels, withAnnotations bool) inspectOut {
	safe := scrubObject(u.Object)
	su := &unstructured.Unstructured{Object: safe}
	out := inspectOut{
		Kind: u.GetKind(), Resource: r.String(), Name: u.GetName(), Namespace: u.GetNamespace(),
		Created: u.GetCreationTimestamp().UTC().Format("2006-01-02T15:04:05Z"), Age: createdAge(u),
		Details:    detailsFor(r, safe, u.Object),
		Conditions: conditions(safe, g.full),
		Owners:     c.ownerChain(ctx, u),
		Events:     c.relatedEvents(ctx, r, u, g.full),
	}
	if withLabels {
		out.Labels = su.GetLabels()
	}
	if withAnnotations {
		out.Annotations = su.GetAnnotations()
	}
	if g.full {
		out.Object = safe
	}
	out.Next = []string{g.cmdline("events", r.String(), u.GetName()) + nsFlag(u.GetNamespace())}
	if logsSupported(r) {
		out.Next = append(out.Next, g.cmdline("logs", r.GVR.Resource+"/"+u.GetName())+nsFlag(u.GetNamespace()))
	}
	return out
}

// relatedEvents are the newest events about u. A failure to read events is
// reported on the block instead of failing the whole inspect.
func (c *Clients) relatedEvents(ctx context.Context, r Resource, u *unstructured.Unstructured, full bool) relatedEvents {
	res, err := c.Events(ctx, eventQuery{Namespace: u.GetNamespace(), Kind: u.GetKind(), Name: u.GetName(), UID: string(u.GetUID())})
	if err != nil {
		return relatedEvents{Items: eventRows(nil, false, full), Note: "events unavailable: " + reasonOf(err)}
	}
	evs := res.Events
	out := relatedEvents{Total: len(evs), Truncated: res.Cut}
	if len(evs) > inspectEvents {
		evs, out.Truncated = evs[:inspectEvents], true
	}
	out.Count = len(evs)
	out.Items = eventRows(evs, false, full)
	if out.Truncated {
		out.Note = fmt.Sprintf("showing %d of %d; see them all with %s", out.Count, out.Total,
			commandHint("events", r.String(), u.GetName())+nsFlag(u.GetNamespace()))
	}
	if res.Cut {
		out.Note += "; more than " + fmt.Sprint(maxEventScan) + " events matched, so the newest may be missing"
	}
	return out
}

// ownerChain follows ownerReferences upward from u, nearest owner first.
// An owner that cannot be read is listed with a note rather than failing.
func (c *Clients) ownerChain(ctx context.Context, u *unstructured.Unstructured) []ownerRow {
	rows := []ownerRow{}
	var walk func(o *unstructured.Unstructured, depth int)
	walk = func(o *unstructured.Unstructured, depth int) {
		for _, ref := range o.GetOwnerReferences() {
			if len(rows) >= maxOwnerRows {
				return
			}
			row := ownerRow{Depth: depth, Kind: ref.Kind, Name: ref.Name, Controller: ref.Controller != nil && *ref.Controller}
			if depth >= maxOwnerDepth {
				rows = append(rows, row)
				continue
			}
			owner, err := c.readOwner(ctx, o.GetNamespace(), ref.APIVersion, ref.Kind, ref.Name)
			if err != nil {
				row.Note = reasonOf(err)
				rows = append(rows, row)
				continue
			}
			rows = append(rows, row)
			walk(owner, depth+1)
		}
	}
	walk(u, 1)
	return rows
}

func (c *Clients) readOwner(ctx context.Context, ns, apiVersion, kind, name string) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	m, err := c.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return nil, fmt.Errorf("kind not served: %w", err)
	}
	r := Resource{GVR: m.Resource, Kind: kind, Namespaced: m.Scope.Name() == "namespace"}
	return c.Get(ctx, r, ns, name)
}

// reasonOf is a short, credential-free reason for a failed read.
func reasonOf(err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return "not found"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return truncate(redactedError(err).Error(), 120)
}
