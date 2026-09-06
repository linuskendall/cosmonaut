// Detail panel for a Coder workspace: hero header, actions, info form,
// and the configured-port-forwards section.
package daemon

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/terminal"
)

func (uw *unifiedWindow) showCoderWorkspaceDetail(ws provider.Workspace) {
	uw.currentView = func() { uw.showCoderWorkspaceDetail(ws) }
	uw.currentViewID = "coder:" + ws.Name
	target, resolvedName := guiTargetForCoderWorkspace(uw.daemon.Cfg, ws)

	stateLbl := canvas.NewText(strings.ToUpper(ws.State), stateColor(ws.State))
	stateLbl.TextSize = 10
	stateLbl.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
	statusRow := container.NewHBox(stateDot(ws.State), stateLbl)

	title := ws.DisplayName
	if title == "" {
		title = ws.Name
	}
	heroTitle := canvas.NewText(title, theme.Color(theme.ColorNameForeground))
	heroTitle.TextSize = 16
	heroTitle.TextStyle = fyne.TextStyle{Bold: true}

	subtitle := canvas.NewText("coder", theme.Color(theme.ColorNamePlaceHolder))
	subtitle.TextSize = 11
	subtitle.TextStyle = fyne.TextStyle{Monospace: true}

	selectedEditor := uw.daemon.getEditor().Name()
	editorEntry := widget.NewEntry()
	editorEntry.SetPlaceHolder("zed (default)")
	editorEntry.SetText(selectedEditor)
	editorEntry.OnChanged = func(val string) {
		selectedEditor = val
	}

	openBtn := primaryButton("Open", func() {
		workspace := ws
		uw.daemon.Cfg.WithEditor(selectedEditor, func() {
			uw.daemon.runLaunchFlow(uw.win, target, resolvedName, &workspace)
		})
	})
	sshBtn := widget.NewButton("SSH", func() {
		go func() {
			sshAlias := fmt.Sprintf("%s.coder", ws.Name)
			mux := uw.daemon.Cfg.WorkspaceSSHMultiplexer(provider.NameCoder, ws.Name)
			workspacePath := provider.GuessWorkspacePath(target, &ws)
			terminal.OpenSSHInTerminal(sshAlias, workspacePath, mux)
			if uw.daemon.sessions != nil {
				uw.daemon.sessions.TrackSession(sshAlias)
			}
		}()
	})
	var refreshBtn *widget.Button
	refreshBtn = widget.NewButton("Refresh", func() {
		refreshBtn.Disable()
		uw.daemon.refreshCoderWorkspacesAsync(func() {
			uw.loadRepos()
			uw.applyFilter()
			uw.tree.Refresh()
			if !uw.stillOn("coder:" + ws.Name) {
				return
			}
			for _, latest := range uw.daemon.Workspaces() {
				if latest.Provider == provider.NameCoder && latest.Name == ws.Name {
					uw.showCoderWorkspaceDetail(latest)
					return
				}
			}
			uw.showCoderSummary()
		})
	})

	deleteBtn := destructiveButton("Delete", func() {
		uw.daemon.confirmAndDeleteWorkspace(uw.win, provider.NameCoder, ws.Name, func() {
			uw.tree.Refresh()
			uw.showCoderSummary()
		})
	})
	if reason := uw.daemon.deleteDisabledReason(provider.NameCoder); reason != "" {
		deleteBtn.SetText(fmt.Sprintf("Delete — %s", reason))
		deleteBtn.Disable()
	}

	nameVal := widget.NewLabel(ws.Name)
	nameVal.TextStyle = fyne.TextStyle{Monospace: true}
	stateVal := widget.NewLabel(ws.State)
	templateVal := widget.NewLabel(ws.Template)
	lastUsedVal := widget.NewLabel(formatTimeAgo(ws.LastUsedAt))
	sshHostVal := widget.NewLabel(fmt.Sprintf("%s.coder", ws.Name))
	sshHostVal.TextStyle = fyne.TextStyle{Monospace: true}
	pathVal := widget.NewLabel(provider.GuessWorkspacePath(target, &ws))
	pathVal.TextStyle = fyne.TextStyle{Monospace: true}

	info := widget.NewForm(
		widget.NewFormItem("Workspace", nameVal),
		widget.NewFormItem("State", stateVal),
		widget.NewFormItem("Template", templateVal),
		widget.NewFormItem("Last used", lastUsedVal),
		widget.NewFormItem("SSH host", sshHostVal),
		widget.NewFormItem("Path", pathVal),
	)
	portTargetName := coderPortTargetName(uw.daemon.Cfg, ws, resolvedName)
	portTarget := target
	if configured, ok := uw.daemon.Cfg.Target(portTargetName); ok {
		portTarget = applyWorkspaceDefaults(configured, ws)
	}
	ports := uw.buildCoderPortsSection(ws, portTarget, portTargetName)
	sshSection := uw.buildWorkspaceSSHSection(provider.NameCoder, ws.Name, func() {
		uw.showCoderWorkspaceDetail(ws)
	})

	body := container.NewVBox(
		statusRow,
		heroTitle,
		subtitle,
		widget.NewSeparator(),
		container.NewHBox(openBtn, editorEntry, sshBtn, layout.NewSpacer(), refreshBtn, deleteBtn),
		widget.NewSeparator(),
		info,
		widget.NewSeparator(),
		sshSection,
		widget.NewSeparator(),
		ports,
	)
	uw.setContent(container.NewPadded(body))
}

func (uw *unifiedWindow) buildCoderPortsSection(ws provider.Workspace, target config.Target, targetName string) fyne.CanvasObject {
	title := caption("CONFIGURED PORT FORWARDS")
	adHocBtn := widget.NewButton("Forward port...", func() {
		uw.showAdHocPortForwardDialog(provider.NameCoder, ws.Name, func() {
			if uw.stillOn("coder:" + ws.Name) {
				uw.showCoderWorkspaceDetail(ws)
			}
		})
	})
	addBtn := primaryButton("Add port forward", func() {
		uw.showCoderPortDialog(ws, target, targetName, -1, nil)
	})

	rows := []fyne.CanvasObject{
		container.NewHBox(title, layout.NewSpacer(), adHocBtn, addBtn),
	}

	if target.Coder == nil || len(target.Coder.PortForwards) == 0 {
		rows = append(rows, widget.NewLabel("No configured Coder port forwards."))
		return container.NewVBox(rows...)
	}
	for i, pf := range target.Coder.PortForwards {
		rows = append(rows, uw.coderPortRow(ws, targetName, i, pf))
	}
	return container.NewVBox(rows...)
}

func (uw *unifiedWindow) coderPortRow(ws provider.Workspace, targetName string, index int, pf config.PortForward) fyne.CanvasObject {
	protocol := normalizePortForwardProtocol(pf.Protocol)
	remotePort := pf.RemotePort
	localPort := pf.LocalPort
	if localPort == 0 {
		localPort = remotePort
	}
	label := pf.Label
	if label == "" {
		label = fmt.Sprintf("%s %d:%d", strings.ToUpper(protocol), localPort, remotePort)
	}
	title := widget.NewLabel(label)
	title.TextStyle = fyne.TextStyle{Bold: true}
	detail := widget.NewLabel(fmt.Sprintf("localhost:%d -> %s:%d", localPort, ws.Name, remotePort))
	detail.TextStyle = fyne.TextStyle{Monospace: true}

	var forwardBtn *widget.Button
	if uw.daemon.forwards != nil && uw.daemon.forwards.IsActiveProtocol(provider.NameCoder, ws.Name, protocol, remotePort, localPort) {
		forwardBtn = widget.NewButton(fmt.Sprintf("Stop localhost %d", localPort), func() {
			uw.daemon.stopWorkspacePortForward(provider.NameCoder, ws.Name, protocol, remotePort, localPort)
			uw.showCoderWorkspaceDetail(ws)
		})
	} else {
		forwardBtn = widget.NewButton(fmt.Sprintf("Forward localhost %d:%d", remotePort, localPort), func() {
			go func() {
				if err := uw.daemon.startWorkspacePortForward(provider.NameCoder, ws.Name, protocol, remotePort, localPort); err != nil {
					uw.daemon.notify(err.Error())
				}
				fyne.Do(func() {
					if uw.stillOn("coder:" + ws.Name) {
						uw.showCoderWorkspaceDetail(ws)
					}
				})
			}()
		})
	}

	editBtn := widget.NewButton("Edit", func() {
		uw.showCoderPortDialog(ws, config.Target{}, targetName, index, &pf)
	})
	removeBtn := widget.NewButton("Remove", func() {
		if err := uw.removeCoderPortForward(targetName, index); err != nil {
			dialog.ShowError(err, uw.win)
			return
		}
		uw.showCoderWorkspaceDetail(ws)
	})
	left := container.NewVBox(title, detail)
	actions := container.NewHBox(forwardBtn, editBtn, removeBtn)
	return surfaceCard(container.NewBorder(nil, nil, nil, actions, left))
}
