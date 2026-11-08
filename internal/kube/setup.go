package kube

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lucashalbert/axi-sdk-go/axierr"
	"github.com/lucashalbert/axi-sdk-go/cli"
	"github.com/lucashalbert/axi-sdk-go/output"
	"github.com/lucashalbert/axi-sdk-go/setup"

	"github.com/lucashalbert/k8s-axi/skills"
)

// hookCommand runs at session start. The short timeout keeps a dead cluster
// from stalling the session.
const hookCommand = "k8s-axi cluster info --timeout 5s"

type setupOut struct {
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Message string `json:"message"`
}

func (g *globals) setupCmd() *cobra.Command {
	root := &cobra.Command{Use: "setup", Short: "Wire k8s-axi into your coding agent"}

	var settings string
	var printOnly bool
	hooks := &cobra.Command{
		Use:   "hooks",
		Short: "Add a session-start hook that runs 'cluster info'",
		Long: "Add a SessionStart hook to the agent settings file so every session opens with the target cluster.\n" +
			"The file defaults to $CLAUDE_CONFIG_DIR/settings.json, or ~/.claude/settings.json. Unrelated settings are kept, and running it again changes nothing.\n" +
			"With --print, show the settings fragment and write nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printOnly {
				_, err := fmt.Fprint(cmd.OutOrStdout(), setup.HooksSnippet(hookCommand))
				return err
			}
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			path, err := agentPath(settings, "settings.json")
			if err != nil {
				return err
			}
			changed, err := setup.InstallHooks(path, hookCommand)
			if err != nil {
				return axierr.Runtime("setup hooks", path, err, "fix or remove the file, or pass --settings")
			}
			msg := "hook already present"
			if changed {
				msg = "hook added"
			}
			return output.Render(cmd.OutOrStdout(), setupOut{Path: path, Changed: changed, Message: msg}, f)
		},
	}
	hooks.Flags().StringVar(&settings, "settings", "", "agent settings file to edit")
	hooks.Flags().BoolVar(&printOnly, "print", false, "print the settings fragment instead of writing it")

	var dest string
	skill := &cobra.Command{
		Use:   "skill",
		Short: "Install the k8s-axi agent skill",
		Long:  "Copy the embedded skill to <agent config dir>/skills/k8s-axi, overwriting an older copy.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := cli.Format(cmd)
			if err != nil {
				return err
			}
			path, err := agentPath(dest, filepath.Join("skills", "k8s-axi"))
			if err != nil {
				return err
			}
			src, err := fs.Sub(skills.FS, "k8s-axi")
			if err != nil {
				return err
			}
			if err := setup.InstallSkill(src, path); err != nil {
				return axierr.Runtime("setup skill", path, err, "check that the directory is writable, or pass --dest")
			}
			return output.Render(cmd.OutOrStdout(), setupOut{Path: path, Changed: true, Message: "skill installed"}, f)
		},
	}
	skill.Flags().StringVar(&dest, "dest", "", "directory to install the skill into")

	root.AddCommand(hooks, skill)
	return root
}

// agentPath returns override, or rel under the agent config directory.
func agentPath(override, rel string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", axierr.Usage("setup", "cannot find the home directory: "+err.Error(), "set CLAUDE_CONFIG_DIR or pass an explicit path")
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, rel), nil
}
