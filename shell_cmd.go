package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/sshconfig"
	"github.com/linuskendall/cosmonaut/internal/terminal"
)

// shellCmd implements `cosmonaut shell [target]`. It opens an interactive SSH
// session to the resolved workspace in the current terminal, optionally
// wrapping it in a persistent terminal multiplexer session (tmux or zellij)
// so the shell survives SSH drops.
//
// Unlike the root `cosmonaut` command, shell doesn't touch editor config or
// create workspaces — the workspace must already exist and be reachable.
// This keeps the command snappy and safe to use as a fallback when an editor
// session drops mid-flight.
func shellCmd(configPath *string) *cobra.Command {
	var (
		codespaceName string
		multiplexer   string
		tmux          bool
		controlMaster bool
	)
	cmd := &cobra.Command{
		Use:   "shell [target]",
		Short: "Open an SSH shell to a workspace",
		Long: `Open an SSH shell to a workspace in the current terminal.

With --multiplexer tmux or --multiplexer zellij (or the equivalent config
setting), the remote shell runs inside a long-lived multiplexer session, so
re-running this command reattaches to the same session after an SSH drop.
ControlMaster multiplexing makes reconnects feel instant.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var targetName string
			if len(args) > 0 {
				targetName = args[0]
			}
			var muxOverride *string
			var cmOverride *bool
			switch {
			case cmd.Flags().Changed("multiplexer"):
				if !config.ValidMultiplexer(multiplexer) {
					return fmt.Errorf("invalid --multiplexer %q (valid: %s)", multiplexer, strings.Join(config.Multiplexers, ", "))
				}
				v := multiplexer
				muxOverride = &v
			case cmd.Flags().Changed("tmux"):
				v := config.MultiplexerNone
				if tmux {
					v = config.MultiplexerTmux
				}
				muxOverride = &v
			}
			if cmd.Flags().Changed("control-master") {
				v := controlMaster
				cmOverride = &v
			}
			return runShell(*configPath, targetName, codespaceName, muxOverride, cmOverride)
		},
	}
	cmd.Flags().StringVar(&codespaceName, "codespace", "", "specific workspace name, skipping selection")
	cmd.Flags().StringVar(&multiplexer, "multiplexer", "none", "wrap the remote shell in a persistent multiplexer session (none, tmux, zellij)")
	cmd.Flags().BoolVar(&tmux, "tmux", false, "wrap the remote shell in a persistent tmux session")
	_ = cmd.Flags().MarkDeprecated("tmux", "use --multiplexer tmux")
	cmd.Flags().BoolVar(&controlMaster, "control-master", true, "use SSH ControlMaster multiplexing for instant reconnects")
	return cmd
}

func runShell(configPath, targetName, codespaceName string, muxOverride *string, controlMasterOverride *bool) error {
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(absConfigPath)
	if err != nil {
		// Missing config is fine — `cosmonaut shell <owner>/<repo>` works
		// without one. Parse errors, on the other hand, are user-visible
		// bugs and we'd rather fail loudly than silently use defaults.
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		cfg = &config.Config{}
	}

	manager, err := provider.NewManager(cfg)
	if err != nil {
		return err
	}
	if err := manager.EnsurePrereqs(); err != nil {
		return err
	}
	if err := manager.EnsureAuth(); err != nil {
		return err
	}

	target, err := resolveShellTarget(cfg, targetName, codespaceName, manager.Name())
	if err != nil {
		return err
	}

	var selected *provider.Workspace
	switch {
	case codespaceName != "":
		selected, err = manager.ResolveWorkspace(codespaceName)
	case target.ExplicitWorkspaceName(manager.Name()) != "":
		selected, err = manager.ResolveWorkspace(target.ExplicitWorkspaceName(manager.Name()))
	default:
		matches, listErr := manager.ListWorkspacesForTarget(target)
		if listErr != nil {
			return listErr
		}
		if len(matches) == 0 {
			return fmt.Errorf("no workspace found for target %q; create one with `cosmonaut` first", targetName)
		}
		if len(matches) > 1 {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.Name
			}
			return fmt.Errorf("multiple workspaces match target %q (%s); pass --codespace <name>", targetName, strings.Join(names, ", "))
		}
		selected = &matches[0]
	}
	if err != nil {
		return err
	}
	if selected == nil {
		return fmt.Errorf("could not resolve a workspace")
	}

	if _, err := manager.StartWorkspace(selected); err != nil {
		return err
	}
	if err := manager.EnsureReachable(selected); err != nil {
		return err
	}

	paths := sshconfig.ResolvePaths()
	sshOpts := sshconfig.ManagedExtrasOptions{
		ControlMaster: resolveControlMaster(cfg, selected.Provider, selected.Name, controlMasterOverride),
	}
	alias, err := manager.PrepareSSH(paths, selected, sshOpts)
	if err != nil {
		return err
	}

	workspacePath := provider.GuessWorkspacePath(target, selected)
	mux := resolveMultiplexer(cfg, selected.Provider, selected.Name, muxOverride)
	return execSSHShell(alias, workspacePath, mux)
}

// resolveShellTarget mirrors the root command's target resolution but skips
// the interactive picker — `cosmonaut shell` is expected to be scripted.
func resolveShellTarget(cfg *config.Config, targetName, codespaceName, providerName string) (config.Target, error) {
	if codespaceName != "" && targetName == "" {
		return config.Target{}, nil
	}
	if targetName == "" {
		if cfg != nil && cfg.DefaultTarget != "" {
			t, ok := cfg.Targets[cfg.DefaultTarget]
			if !ok {
				return config.Target{}, fmt.Errorf("default target %q not found", cfg.DefaultTarget)
			}
			return t, nil
		}
		return config.Target{}, fmt.Errorf("no target was provided and config.defaultTarget is not set")
	}
	if strings.Contains(targetName, "/") {
		t, _ := targetForRepo(cfg, targetName, providerName)
		return t, nil
	}
	if cfg == nil {
		return config.Target{}, fmt.Errorf("target %q specified but no config file found", targetName)
	}
	t, ok := cfg.Targets[targetName]
	if !ok {
		return config.Target{}, fmt.Errorf("unknown target %q", targetName)
	}
	return t, nil
}

// execSSHShell replaces the current process with `ssh -t <alias> '<cmd>'`.
// On success it never returns.
func execSSHShell(alias, workspacePath, multiplexer string) error {
	remoteCmd := buildRemoteShellCommand(workspacePath, multiplexer)
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found on PATH: %w", err)
	}
	args := []string{"ssh", "-t", alias, remoteCmd}
	return syscallExec(sshBin, args, os.Environ())
}

// buildRemoteShellCommand returns the command to run on the remote.
// With a multiplexer, attach to a long-lived session named "cosmonaut"
// (creating it on first connect): tmux's `-A` flag makes `tmux new` attach
// to an existing session instead of erroring, and zellij's
// `attach --create` does the same.
func buildRemoteShellCommand(workspacePath, multiplexer string) string {
	cd := ""
	if workspacePath != "" {
		cd = fmt.Sprintf("cd %s && ", terminal.ShellQuote(workspacePath))
	}
	switch multiplexer {
	case config.MultiplexerTmux:
		return cd + "tmux new -A -s cosmonaut"
	case config.MultiplexerZellij:
		return cd + "zellij attach --create cosmonaut"
	}
	return cd + "exec $SHELL -l"
}
