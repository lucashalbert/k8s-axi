package kube

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"github.com/lucashalbert/axi-sdk-go/axierr"
)

// Options select the cluster a command talks to.
type Options struct {
	Kubeconfig string // explicit path; empty uses $KUBECONFIG or ~/.kube/config
	Context    string // kubeconfig context; empty uses the current context
	Namespace  string // empty uses the context's namespace, then "default"
	Timeout    time.Duration
}

// Clients are the Kubernetes clients for one resolved context.
type Clients struct {
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	Mapper    meta.RESTMapper
	REST      *rest.Config

	Context   string
	Namespace string
	Server    string // host[:port] of the API server, without credentials or scheme
}

// Load resolves the kubeconfig, picks the context and builds the clients.
// It reads the kubeconfig and never writes it, and it makes no network
// calls: the clients connect lazily. Exec credential plugins stay enabled
// because client-go starts them from the kubeconfig.
//
// A missing explicit kubeconfig, an unknown context, or no context to pick
// is a usage error that lists the available contexts.
func Load(opts Options) (*Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = opts.Kubeconfig
	raw, err := rules.Load()
	if err != nil {
		if opts.Kubeconfig != "" && errors.Is(err, fs.ErrNotExist) {
			return nil, axierr.Usage("kubeconfig", "kubeconfig file not found: "+opts.Kubeconfig,
				"check --kubeconfig or $KUBECONFIG")
		}
		return nil, axierr.Runtime("load kubeconfig", "", err, "check that the kubeconfig is valid YAML")
	}

	name := opts.Context
	if name == "" {
		name = raw.CurrentContext
	}
	if err := checkContext(raw, name, opts.Context != ""); err != nil {
		return nil, err
	}

	cc := clientcmd.NewNonInteractiveClientConfig(*raw, name, &clientcmd.ConfigOverrides{}, rules)
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, axierr.Runtime("load kubeconfig", "context/"+name, err, "check the context's cluster and user entries")
	}
	rc.Timeout = opts.Timeout

	ns := opts.Namespace
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil || ns == "" {
			ns = "default"
		}
	}

	c := &Clients{REST: rc, Context: name, Namespace: ns, Server: hostOnly(rc.Host)}
	if c.Typed, err = kubernetes.NewForConfig(rc); err != nil {
		return nil, axierr.Runtime("create client", "context/"+name, err, "")
	}
	if c.Dynamic, err = dynamic.NewForConfig(rc); err != nil {
		return nil, axierr.Runtime("create client", "context/"+name, err, "")
	}
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, axierr.Runtime("create client", "context/"+name, err, "")
	}
	c.Discovery = dc
	cached := memory.NewMemCacheClient(dc)
	c.Mapper = restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(cached), cached, func(string) {})
	return c, nil
}

// checkContext returns a usage error that lists the available contexts when
// name is unset or unknown.
func checkContext(raw *api.Config, name string, explicit bool) error {
	if _, ok := raw.Contexts[name]; ok && name != "" {
		return nil
	}
	names := make([]string, 0, len(raw.Contexts))
	for n := range raw.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	avail := "available contexts: " + strings.Join(names, ", ")
	switch {
	case len(names) == 0:
		return axierr.Usage("kubeconfig", "no contexts found in the kubeconfig",
			"point --kubeconfig or $KUBECONFIG at a kubeconfig with at least one context")
	case explicit:
		return axierr.Usage("kubeconfig", fmt.Sprintf("context %q not found; %s", name, avail),
			"pass one of the available contexts with --context")
	default:
		return axierr.Usage("kubeconfig", "no current context is set and --context was not given; "+avail,
			"pass --context <name>; k8s-axi never changes the current context")
	}
}

// hostOnly drops the scheme, any userinfo and the path from an API server URL.
func hostOnly(server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
