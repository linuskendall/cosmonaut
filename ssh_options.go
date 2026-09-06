package main

import (
	"syscall"

	"github.com/linuskendall/cosmonaut/internal/config"
)

// resolveControlMaster picks the ControlMaster setting for a workspace:
// the per-invocation override wins; otherwise the per-workspace config
// (defaulting to on) applies.
func resolveControlMaster(cfg *config.Config, providerName, workspaceName string, override *bool) bool {
	if override != nil {
		return *override
	}
	return cfg.WorkspaceSSHControlMaster(providerName, workspaceName)
}

// resolveMultiplexer picks the multiplexer setting for a workspace: the
// per-invocation override wins; otherwise the per-workspace config (falling
// back to the global default, then "none") applies.
func resolveMultiplexer(cfg *config.Config, providerName, workspaceName string, override *string) string {
	if override != nil {
		return *override
	}
	return cfg.WorkspaceSSHMultiplexer(providerName, workspaceName)
}

// syscallExec is split out so platforms without an `exec`-style replacement
// (Windows) can stub a runtime fallback later. Today, cosmonaut only ships
// for darwin and linux, where syscall.Exec is fine.
func syscallExec(bin string, args, env []string) error {
	return syscall.Exec(bin, args, env)
}
