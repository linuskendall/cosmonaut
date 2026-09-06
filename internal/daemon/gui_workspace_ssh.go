package daemon

import (
	"log"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/sshconfig"
)

// workspaceSSHControls captures which SSH option toggles a workspace's
// detail view should render. The ControlMaster toggle is per-workspace
// only when the on-disk conf is per-workspace — Coder workspaces all
// share ~/.ssh/cosmonaut/coder.conf, so toggling ControlMaster there
// would be last-writer-wins across sibling workspaces.
type workspaceSSHControls struct {
	ShowControlMaster bool
	ShowMultiplexer   bool
	// SharedConfNote is a non-empty user-facing explanation when the
	// ControlMaster toggle is hidden because the provider's SSH conf is
	// shared across workspaces.
	SharedConfNote string
}

// workspaceSSHControlsFor returns the set of SSH option controls that
// should be rendered for a workspace from the given provider. Coder
// hides ControlMaster (shared coder.conf) but keeps the multiplexer
// selector (per-invocation only); GitHub gets both.
func workspaceSSHControlsFor(providerName string) workspaceSSHControls {
	switch providerName {
	case provider.NameCoder:
		return workspaceSSHControls{
			ShowControlMaster: false,
			ShowMultiplexer:   true,
			SharedConfNote:    "Coder workspaces share `~/.ssh/cosmonaut/coder.conf` — ControlMaster is managed globally and can't be toggled per workspace.",
		}
	default:
		return workspaceSSHControls{
			ShowControlMaster: true,
			ShowMultiplexer:   true,
		}
	}
}

// buildWorkspaceSSHSection renders the per-workspace SSH option controls
// (ControlMaster persistent connection, terminal multiplexer wrapping) for
// the detail view of a single workspace.
//
// Both controls write to Config.WorkspaceSSH keyed by provider:name, so each
// workspace owns its own settings — changing the multiplexer on cs-A does
// not affect cs-B. Defaults match the package-wide defaults: ControlMaster
// on, multiplexer from the global SSH config (or none).
//
// For providers whose on-disk SSH conf is shared across workspaces (Coder),
// the ControlMaster toggle is omitted in favour of an explanatory note —
// see workspaceSSHControlsFor.
//
// refresh is invoked after a toggle changes so the caller can re-render the
// detail panel (e.g. so ControlMaster info reflects in any sub-sections).
// rebuildTrayMenu is also called so the tray reflects the new state on the
// next menu open.
func (uw *unifiedWindow) buildWorkspaceSSHSection(providerName, workspaceName string, refresh func()) fyne.CanvasObject {
	title := caption("SSH OPTIONS")

	cfg := uw.daemon.Cfg
	controls := workspaceSSHControlsFor(providerName)

	items := []fyne.CanvasObject{title}

	if controls.ShowControlMaster {
		cmCheck := widget.NewCheck("Persistent SSH (ControlMaster)", func(on bool) {
			v := on
			cfg.SetWorkspaceSSHControlMaster(providerName, workspaceName, &v)
			uw.daemon.persistConfig()
			// Rewrite this workspace's conf so the new managed-extras block
			// takes effect immediately — otherwise the next SSH wouldn't pick
			// up the change until the workspace is re-prepared.
			uw.daemon.applyWorkspaceSSHOptions(providerName, workspaceName)
			if refresh != nil {
				refresh()
			}
		})
		cmCheck.SetChecked(cfg.WorkspaceSSHControlMaster(providerName, workspaceName))
		cmHint := mutedHint("Multiplex extra sessions over one TCP connection — instant reconnects.")
		items = append(items, cmCheck, container.NewPadded(cmHint))
	} else if controls.SharedConfNote != "" {
		items = append(items, container.NewPadded(mutedHint(controls.SharedConfNote)))
	}

	if controls.ShowMultiplexer {
		// Set the current value before wiring OnChanged: SetSelected fires
		// the callback, and an unset workspace would otherwise get an
		// explicit setting written just by opening the detail view.
		muxSelect := widget.NewSelect(config.Multiplexers, nil)
		muxSelect.SetSelected(cfg.WorkspaceSSHMultiplexer(providerName, workspaceName))
		muxSelect.OnChanged = func(sel string) {
			v := sel
			cfg.SetWorkspaceSSHMultiplexer(providerName, workspaceName, &v)
			uw.daemon.persistConfig()
			if refresh != nil {
				refresh()
			}
		}
		muxHint := mutedHint("The SSH button (and `cosmonaut shell`) attach to a persistent tmux or zellij session that survives disconnects.")
		items = append(items, widget.NewLabel("Terminal multiplexer"), muxSelect, container.NewPadded(muxHint))
	}

	return container.NewVBox(items...)
}

// mutedHint returns a wrapped label styled as secondary help text.
func mutedHint(s string) fyne.CanvasObject {
	lbl := widget.NewLabel(s)
	lbl.Wrapping = fyne.TextWrapWord
	lbl.Importance = widget.LowImportance
	return lbl
}

// applyWorkspaceSSHOptions rewrites the on-disk SSH conf for a workspace so
// the latest ControlMaster setting takes effect without waiting for the next
// PrepareSSH call. A no-op for workspaces whose conf doesn't exist yet
// (a launch will create one with the right options).
func (d *Daemon) applyWorkspaceSSHOptions(providerName, workspaceName string) {
	paths := sshconfig.ResolvePaths()
	confPath := paths.WorkspaceConfigPath(providerName, workspaceName)
	opts := sshconfig.ManagedExtrasOptions{
		ControlMaster: d.Cfg.WorkspaceSSHControlMaster(providerName, workspaceName),
	}
	if _, err := sshconfig.RefreshManagedExtras(confPath, opts); err != nil {
		log.Printf("ssh options: refresh %s: %v", confPath, err)
	}
}
