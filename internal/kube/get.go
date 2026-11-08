package kube

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
)

type getOut struct {
	Resource  string   `json:"resource"`
	Namespace string   `json:"namespace,omitempty"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
	Remaining *int64   `json:"remaining,omitempty"`
	Note      string   `json:"note,omitempty"`
	Items     any      `json:"items"`
	Next      []string `json:"next,omitempty"`
}

func (g *globals) getCmd() *cobra.Command {
	var all bool
	var selector, fieldSelector string
	cmd := &cobra.Command{
		Use:   "get KIND [NAME]",
		Short: "List resources of a kind, or read one, as a compact table",
		Long: "List resources of any kind the cluster serves, built-in or custom, or read one by name.\n" +
			"KIND is a plural, singular, kind or short name (pods, pod, po), optionally qualified as resource.version.group or group/version/resource.\n" +
			"The default view is a few columns per kind. --full prints the complete objects; Secret values are never printed, only their sizes.\n" +
			"A list is cut at --limit rows and says so; --limit 0 lists everything.",
		Example: "  k8s-axi get pods -n kube-system\n  k8s-axi get deploy -A -l app=web\n  k8s-axi get widgets.v1beta1.example.com --full",
		Args:    argRange(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := splitTarget(args)
			if err != nil {
				return err
			}
			if name != "" && (selector != "" || fieldSelector != "") {
				return axierr.Usage("get", "a name cannot be combined with -l or --field-selector", "drop the name to filter a list, or drop the filters to read one object")
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
			if name != "" && all && r.Namespaced {
				return axierr.Usage("get", "a namespaced object name cannot be combined with -A", "drop the name to list across namespaces, or drop -A to read one object")
			}
			ns, err := scopeNamespace(cmd, c, all, r.Namespaced)
			if err != nil {
				return err
			}
			out, err := g.get(cmd, c, r, ns, name, metav1.ListOptions{LabelSelector: selector, FieldSelector: fieldSelector})
			if err != nil {
				return err
			}
			return output.Render(cmd.OutOrStdout(), out, f)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "list across all namespaces")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector, for example app=web,tier!=db")
	cmd.Flags().StringVar(&fieldSelector, "field-selector", "", "field selector, for example status.phase=Running")
	return cmd
}

func (g *globals) get(cmd *cobra.Command, c *Clients, r Resource, ns, name string, o metav1.ListOptions) (getOut, error) {
	ctx := cmd.Context()
	target := "context/" + c.Context + " " + r.String()
	if !r.Namespaced {
		ns = ""
	}
	var objs []unstructured.Unstructured
	out := getOut{Resource: r.String()}
	switch {
	case name != "":
		u, err := c.Get(ctx, r, ns, name)
		if err != nil {
			return out, apiError("get", target+"/"+name+nsFlag(ns), err, "get", r.String())
		}
		objs = []unstructured.Unstructured{*u}
	default:
		o.Limit = int64(g.limit)
		res, err := c.List(ctx, r, ns, o)
		if err != nil {
			return out, apiError("get", target+nsFlag(ns), err, "list", r.String())
		}
		objs = res.Items
		out.Truncated, out.Remaining = res.More, res.Remaining
		if g.limit > 0 && len(objs) > g.limit { // a server that ignores the limit
			objs, out.Truncated, out.Remaining = objs[:g.limit], true, nil
		}
	}

	showNS := r.Namespaced && ns == ""
	if r.Namespaced {
		out.Namespace = ns
		if ns == "" {
			out.Namespace = "*"
		}
	}
	out.Count = len(objs)
	if g.full {
		full := make([]map[string]any, 0, len(objs))
		for i := range objs {
			full = append(full, scrubObject(objs[i].Object))
		}
		out.Items = full
	} else {
		rows := make([]any, 0, len(objs))
		for i := range objs {
			rows = append(rows, rowFor(r, &objs[i], showNS))
		}
		out.Items = rows
	}
	out.Note = g.getNote(r, ns, o, out)
	if len(objs) > 0 {
		first := &objs[0]
		out.Next = []string{g.cmdline("inspect", r.String(), first.GetName()) + nsFlag(first.GetNamespace())}
	}
	return out, nil
}

func (g *globals) getNote(r Resource, ns string, o metav1.ListOptions, out getOut) string {
	switch {
	case out.Truncated:
		shown := fmt.Sprintf("showing %d", out.Count)
		if out.Remaining != nil {
			shown += fmt.Sprintf(" of %d", int64(out.Count)+*out.Remaining)
		} else {
			shown += ", more exist"
		}
		return "truncated: " + shown + "; use --limit 0 to see all, or narrow with -n, -l or --field-selector"
	case out.Count == 0:
		where := "in all namespaces"
		switch {
		case !r.Namespaced:
			where = "in the cluster"
		case ns != "":
			where = "in namespace " + ns
		}
		var filters []string
		if o.LabelSelector != "" {
			filters = append(filters, "selector "+o.LabelSelector)
		}
		if o.FieldSelector != "" {
			filters = append(filters, "field selector "+o.FieldSelector)
		}
		note := fmt.Sprintf("no %s found %s", r.GVR.Resource, where)
		if len(filters) > 0 {
			note += " (" + strings.Join(filters, ", ") + ")"
		}
		return note
	}
	return ""
}
