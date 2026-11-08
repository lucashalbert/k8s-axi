// Package kube is the Kubernetes module of k8s-axi: client configuration,
// discovery and the commands built on them.
package kube

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
)

const (
	envPrefix      = "K8S_AXI_"
	defaultLimit   = 50
	defaultTimeout = 30 * time.Second
)

// Module registers the global flags and the Kubernetes commands.
// The zero value loads clients from the kubeconfig.
type Module struct {
	load func(Options) (*Clients, error) // test seam
}

// globals holds the values of the persistent flags shared by all commands.
type globals struct {
	kubeconfig string
	context    string
	namespace  string
	full       bool
	limit      int
	timeout    time.Duration

	load func(Options) (*Clients, error)
}

// clients resolves the kubeconfig context the global flags select.
func (g *globals) clients() (*Clients, error) {
	return g.load(Options{Kubeconfig: g.kubeconfig, Context: g.context, Namespace: g.namespace, Timeout: g.timeout})
}

// Register implements cli.Module.
func (m Module) Register(root *cobra.Command) {
	g := &globals{load: m.load}
	if g.load == nil {
		g.load = Load
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	pf.StringVar(&g.context, "context", "", "kubeconfig context to use; the current context is never changed")
	pf.StringVarP(&g.namespace, "namespace", "n", "", "namespace to use (default: the context's namespace)")
	pf.BoolVar(&g.full, "full", false, "show full detail instead of the compact view")
	pf.IntVar(&g.limit, "limit", defaultLimit, "maximum number of rows to print; 0 means no limit")
	pf.DurationVar(&g.timeout, "timeout", defaultTimeout, "timeout for requests to the API server")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return bindEnv(cmd.Root().PersistentFlags())
	}

	root.AddCommand(g.clusterCmd(), g.discoverCmd(), g.getCmd(), g.inspectCmd(), g.eventsCmd(), g.setupCmd())
}

// bindEnv applies K8S_AXI_<FLAG> to every global flag the command line left
// unset, so a flag always wins over the environment.
func bindEnv(fs *pflag.FlagSet) error {
	var err error
	fs.VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed {
			return
		}
		key := envPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			return
		}
		if serr := fs.Set(f.Name, v); serr != nil {
			err = axierr.Usage("flags", fmt.Sprintf("invalid %s=%q: %v", key, v, serr),
				"unset "+key+" or fix its value")
			return
		}
	})
	return err
}

var _ cli.Module = Module{}
