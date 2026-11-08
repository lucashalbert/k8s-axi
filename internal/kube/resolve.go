package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// Resource is a kind argument resolved through discovery and the REST mapper.
type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

// String is the resource as "plural", or "plural.version.group" outside the
// core group, the form Resolve accepts back.
func (r Resource) String() string {
	if r.GVR.Group == "" {
		return r.GVR.Resource
	}
	return r.GVR.Resource + "." + r.GVR.Version + "." + r.GVR.Group
}

// Resolve maps a kind argument to a served resource. The argument is the
// plural, singular, kind or short name, optionally qualified as
// resource.version.group or group/version/resource. The scope and version
// come from the cluster's discovery, never from a table.
func (c *Clients) Resolve(arg string) (Resource, error) {
	return c.resolve(context.Background(), arg)
}

func (c *Clients) ResolveContext(ctx context.Context, arg string) (Resource, error) {
	return withContext(ctx, func() (Resource, error) { return c.resolve(ctx, arg) })
}

func (c *Clients) resolve(ctx context.Context, arg string) (Resource, error) {
	mapper := meta.ToRESTMapperWithContext(c.Mapper)
	for _, cand := range candidates(arg) {
		gvk, err := mapper.KindForWithContext(ctx, cand)
		if err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return Resource{}, c.resolveError(arg, err)
		}
		m, err := mapper.RESTMappingWithContext(ctx, gvk.GroupKind(), gvk.Version)
		if err != nil {
			return Resource{}, c.resolveError(arg, err)
		}
		return Resource{GVR: m.Resource, Kind: gvk.Kind, Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}, nil
	}
	return Resource{}, axierr.Usage("resolve kind", fmt.Sprintf("the cluster does not serve a kind named %q", arg),
		"run 'k8s-axi discover --kind "+arg+"' to list matching kinds")
}

// candidates are the readings of arg to try in order, as kubectl does:
// a full resource.version.group first, then resource.group.
func candidates(arg string) []schema.GroupVersionResource {
	if parts := strings.Split(arg, "/"); len(parts) == 3 {
		return []schema.GroupVersionResource{{Group: parts[0], Version: parts[1], Resource: parts[2]}}
	}
	gvr, gr := schema.ParseResourceArg(arg)
	var out []schema.GroupVersionResource
	if gvr != nil {
		out = append(out, *gvr)
	}
	return append(out, gr.WithVersion(""))
}

// resolveError classifies a failed lookup: a cancelled context passes
// through, an ambiguous kind is a usage error, and anything else means
// discovery could not reach the API server.
func (c *Clients) resolveError(arg string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case meta.IsAmbiguousError(err):
		return axierr.Usage("resolve kind", fmt.Sprintf("%q is ambiguous: %v", arg, err),
			"qualify it with an API group, for example "+arg+".<group>")
	}
	return apiError("resolve kind", "context/"+c.Context+" ("+c.Server+")", err, "get", "discovery")
}

// splitTarget reads "KIND NAME", "KIND/NAME" or "KIND" (name empty). KIND may
// itself be group/version/resource, in which case a fourth path element is
// the name.
func splitTarget(args []string) (kind, name string, err error) {
	bad := func() (string, string, error) {
		return "", "", axierr.Usage("target", fmt.Sprintf("cannot read %q as KIND NAME or KIND/NAME", strings.Join(args, " ")),
			"write the target as 'pod web' or 'pod/web'")
	}
	switch len(args) {
	case 1:
		parts := strings.Split(args[0], "/")
		switch len(parts) {
		case 1, 3:
			return args[0], "", nil
		case 2:
			kind, name = parts[0], parts[1]
		case 4:
			kind, name = strings.Join(parts[:3], "/"), parts[3]
		default:
			return bad()
		}
	case 2:
		if n := strings.Count(args[0], "/"); n != 0 && n != 2 {
			return bad()
		}
		kind, name = args[0], args[1]
	default:
		return bad()
	}
	if kind == "" || name == "" {
		return bad()
	}
	return kind, name, nil
}
