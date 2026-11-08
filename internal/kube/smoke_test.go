package kube

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/update"
)

// smokePaths is every runnable command. The kind matrix runs one smoke test
// for each of them on every supported Kubernetes minor (see
// TestSmokeEveryCommandAgainstKind), so a new command must be added here and
// to that test, or this test fails.
var smokePaths = []string{
	"apply", "cluster info", "delete", "diagnose", "discover", "events", "get", "inspect", "logs",
	"plan apply", "plan delete", "plan rollout restart", "plan scale",
	"rollout restart", "scale", "setup hooks", "setup skill", "update", "version",
}

func commandPaths(c *cobra.Command, prefix string, out *[]string) {
	for _, sub := range c.Commands() {
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		p := strings.TrimSpace(prefix + " " + sub.Name())
		if sub.Runnable() {
			*out = append(*out, p)
		}
		commandPaths(sub, p, out)
	}
}

func TestSmokeListCoversEveryCommand(t *testing.T) {
	root := cli.New("k8s-axi", update.Info{Version: "0.0.0"}, cli.EnvPrefix("K8S_AXI_"), Module{})
	var got []string
	commandPaths(root, "", &got)
	sort.Strings(got)
	want := slices.Clone(smokePaths)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("smokePaths and the command tree differ:\n tree:  %v\n smoke: %v", got, want)
	}
}
