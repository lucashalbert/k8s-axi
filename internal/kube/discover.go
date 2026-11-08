package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
)

const coreGroup = "core"

type kindRow struct {
	Kind     string `json:"kind"`
	Group    string `json:"group"`
	Version  string `json:"version"`
	Resource string `json:"resource"`
	Scope    string `json:"scope"`
	Verbs    string `json:"verbs"`
	CRD      bool   `json:"crd"`
}

type kindRowFull struct {
	Kind       string `json:"kind"`
	Group      string `json:"group"`
	Version    string `json:"version"`
	Resource   string `json:"resource"`
	Scope      string `json:"scope"`
	Verbs      string `json:"verbs"`
	CRD        bool   `json:"crd"`
	ShortNames string `json:"short_names"`
	Categories string `json:"categories"`
}

type discoverOut struct {
	Matched   int      `json:"matched"`
	Shown     int      `json:"shown"`
	Note      string   `json:"note,omitempty"`
	CRDSource string   `json:"crd_source"`
	Warnings  []string `json:"warnings,omitempty"`
	Kinds     any      `json:"kinds"`
}

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

func (g *globals) discoverCmd() *cobra.Command {
	var group, kind string
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List the resource kinds the cluster serves, built-in and custom",
		Long: "List the resource kinds the API server serves, found through API discovery at run time.\n" +
			"--group matches the API group exactly (use \"core\" for the core group).\n" +
			"--kind matches a kind, resource name, singular name or short name, ignoring case.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			c, err := g.clients()
			if err != nil {
				return err
			}
			out, err := discoverKinds(cmd.Context(), c, group, kind, g.limit, g.full)
			if err != nil {
				return axierr.Runtime("discover", "context/"+c.Context+" ("+c.Server+")", redactedError(err),
					"check that the API server is reachable and the credentials are valid, or raise --timeout ("+g.timeout.String()+")")
			}
			return output.Render(cmd.OutOrStdout(), out, f)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "only kinds in this API group")
	cmd.Flags().StringVar(&kind, "kind", "", "only kinds matching this kind, resource or short name")
	return cmd
}

type kindInfo struct {
	res        metav1.APIResource
	group, ver string
	crd        bool
}

func discoverKinds(ctx context.Context, c *Clients, group, kind string, limit int, full bool) (discoverOut, error) {
	groups, lists, err := c.Discovery.ServerGroupsAndResources()
	var warnings []string
	if err != nil {
		if len(lists) == 0 {
			return discoverOut{}, err
		}
		warnings = discoveryWarnings(err)
	}

	preferred := map[string]string{}
	for _, g := range groups {
		preferred[g.Name] = g.PreferredVersion.GroupVersion
	}
	crds, source := customResources(ctx, c.Dynamic)

	selected := map[string]kindInfo{}
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue // subresource
			}
			k := gv.Group + "/" + r.Name
			info := kindInfo{res: r, group: gv.Group, ver: gv.Version, crd: isCRD(source, crds, gv.Group, r.Name)}
			if _, ok := selected[k]; !ok || preferred[gv.Group] == l.GroupVersion {
				selected[k] = info
			}
		}
	}
	all := make([]kindInfo, 0, len(selected))
	for _, k := range selected {
		all = append(all, k)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].group != all[j].group {
			return all[i].group < all[j].group
		}
		return all[i].res.Kind < all[j].res.Kind
	})

	var matched []kindInfo
	for _, k := range all {
		if matchGroup(group, k.group) && matchKind(kind, k.res) {
			matched = append(matched, k)
		}
	}
	shown := matched
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}

	out := discoverOut{Matched: len(matched), Shown: len(shown), CRDSource: source, Warnings: warnings}
	switch {
	case len(matched) == 0:
		out.Note = "no kinds match" + describeFilter(group, kind) + "; run discover without filters to list every kind"
	case len(shown) < len(matched):
		out.Note = fmt.Sprintf("truncated: showing %d of %d; use --limit 0 to see all, or narrow with --group or --kind", len(shown), len(matched))
	}
	if full {
		rows := make([]kindRowFull, 0, len(shown))
		for _, k := range shown {
			rows = append(rows, kindRowFull{
				Kind: k.res.Kind, Group: groupName(k.group), Version: k.ver, Resource: k.res.Name, Scope: scope(k.res),
				Verbs: strings.Join(k.res.Verbs, ","), CRD: k.crd,
				ShortNames: strings.Join(k.res.ShortNames, ","), Categories: strings.Join(k.res.Categories, ","),
			})
		}
		out.Kinds = rows
	} else {
		rows := make([]kindRow, 0, len(shown))
		for _, k := range shown {
			rows = append(rows, kindRow{
				Kind: k.res.Kind, Group: groupName(k.group), Version: k.ver, Resource: k.res.Name, Scope: scope(k.res),
				Verbs: strings.Join(k.res.Verbs, ","), CRD: k.crd,
			})
		}
		out.Kinds = rows
	}
	return out, nil
}

// customResources lists the cluster's CRDs as "group/plural" keys. It reports
// "apiextensions" when the cluster answered and "heuristic" when listing CRDs
// is not allowed, in which case isCRD guesses from the group name.
func customResources(ctx context.Context, dyn dynamic.Interface) (map[string]bool, string) {
	set := map[string]bool{}
	o := metav1.ListOptions{Limit: crdChunk}
	for {
		list, err := dyn.Resource(crdGVR).List(ctx, o)
		if err != nil {
			return nil, "heuristic"
		}
		for i := range list.Items {
			grp, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "group")
			plural, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "names", "plural")
			set[grp+"/"+plural] = true
		}
		if list.GetContinue() == "" {
			return set, "apiextensions"
		}
		o.Continue = list.GetContinue()
	}
}

func isCRD(source string, crds map[string]bool, group, resource string) bool {
	if source == "apiextensions" {
		return crds[group+"/"+resource]
	}
	// Built-in groups are the core group, single-word groups such as apps,
	// and the *.k8s.io groups. Anything else is almost certainly a CRD.
	return strings.Contains(group, ".") && !strings.HasSuffix(group, ".k8s.io")
}

func matchGroup(want, group string) bool {
	if want == "" {
		return true
	}
	if want == coreGroup {
		want = ""
	}
	return strings.EqualFold(want, group)
}

func matchKind(want string, r metav1.APIResource) bool {
	if want == "" {
		return true
	}
	names := append([]string{r.Kind, r.Name, r.SingularName}, r.ShortNames...)
	for _, n := range names {
		if n != "" && strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

func describeFilter(group, kind string) string {
	var parts []string
	if group != "" {
		parts = append(parts, "--group "+group)
	}
	if kind != "" {
		parts = append(parts, "--kind "+kind)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func groupName(g string) string {
	if g == "" {
		return coreGroup
	}
	return g
}

func scope(r metav1.APIResource) string {
	if r.Namespaced {
		return "namespaced"
	}
	return "cluster"
}

// discoveryWarnings turns a partial discovery failure into one line per
// unavailable group version.
func discoveryWarnings(err error) []string {
	var gdf *discovery.ErrGroupDiscoveryFailed
	if errors.As(err, &gdf) {
		w := make([]string, 0, len(gdf.Groups))
		for gv, e := range gdf.Groups {
			w = append(w, fmt.Sprintf("%s unavailable: %v", gv, redactedError(e)))
		}
		sort.Strings(w)
		return w
	}
	return []string{"discovery incomplete: " + redactURLUserinfo(err.Error())}
}
