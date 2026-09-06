// Detail panel for a GitHub codespace: hero header, actions, info form,
// and the forwarded-ports section.
package daemon

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/terminal"
)

// ── CODESPACE DETAIL ────────────────────────────────────────────────────

func (uw *unifiedWindow) showCosmoCodespaceDetail(csName, repo string) {
	uw.currentView = func() { uw.showCosmoCodespaceDetail(csName, repo) }
	uw.currentViewID = "codespace:" + csName
	var cs *codespace.Codespace
	for _, c := range uw.daemon.Codespaces() {
		if c.Name == csName {
			cs = &c
			break
		}
	}
	if cs == nil {
		uw.showCosmoWelcome()
		return
	}

	target, resolvedName := guiTargetForRepo(uw.daemon.Cfg, repo)

	// ── HEADER: status + title + repo / branch links
	stateLbl := canvas.NewText(strings.ToUpper(cs.State), stateColor(cs.State))
	stateLbl.TextSize = 10
	stateLbl.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}
	statusRow := container.NewHBox(stateDot(cs.State), stateLbl)

	titleText := cs.DisplayName
	if titleText == "" {
		titleText = cs.Name
	}
	heroTitle := canvas.NewText(titleText, theme.Color(theme.ColorNameForeground))
	heroTitle.TextSize = 16
	heroTitle.TextStyle = fyne.TextStyle{Bold: true}

	branchStr := ""
	if cs.GitStatus != nil {
		branchStr = cs.GitStatus.Ref
		if branchStr == "" {
			branchStr = cs.GitStatus.Branch
		}
	}

	repoLink := widget.NewHyperlink("⌂ "+repo, githubURL(repo))
	branchLink := widget.NewHyperlink("⎇ "+branchStr, githubURL(repo, "tree", branchStr))

	repoRow := container.NewHBox(repoLink, branchLink)

	// ── ACTIONS
	selectedEditor := uw.daemon.getEditor().Name()
	editorEntry := widget.NewEntry()
	editorEntry.SetPlaceHolder("zed (default)")
	editorEntry.SetText(selectedEditor)
	editorEntry.OnChanged = func(val string) {
		selectedEditor = val
	}

	openBtn := primaryButton("Open", func() {
		workspace := provider.Workspace{
			Provider:    provider.NameGitHub,
			Name:        cs.Name,
			DisplayName: cs.DisplayName,
			Repository:  repo,
			State:       cs.State,
			MachineName: cs.MachineName,
			CreatedAt:   cs.CreatedAt,
			LastUsedAt:  cs.LastUsedAt,
		}
		if cs.GitStatus != nil {
			workspace.Branch = cs.GitStatus.Ref
			if workspace.Branch == "" {
				workspace.Branch = cs.GitStatus.Branch
			}
		}
		uw.daemon.Cfg.WithEditor(selectedEditor, func() {
			uw.daemon.runLaunchFlow(uw.win, target, resolvedName, &workspace)
		})
	})
	sshBtn := widget.NewButton("SSH", func() {
		go func() {
			sshAlias := fmt.Sprintf("cs.%s.github.dev", cs.Name)
			mux := uw.daemon.Cfg.WorkspaceSSHMultiplexer(provider.NameGitHub, cs.Name)
			terminal.OpenSSHInTerminal(sshAlias, target.WorkspacePath, mux)
			if uw.daemon.sessions != nil {
				uw.daemon.sessions.TrackSession(sshAlias)
			}
		}()
	})

	deleteBtn := destructiveButton("Delete", func() {
		uw.daemon.confirmAndDeleteWorkspace(uw.win, provider.NameGitHub, cs.Name, func() {
			uw.tree.Refresh()
			uw.showCosmoWelcome()
		})
	})
	if reason := uw.daemon.deleteDisabledReason(provider.NameGitHub); reason != "" {
		deleteBtn.SetText(fmt.Sprintf("Delete — %s", reason))
		deleteBtn.Disable()
	}

	actions := container.NewHBox(openBtn, editorEntry, sshBtn, layout.NewSpacer(), deleteBtn)

	sshSection := uw.buildWorkspaceSSHSection(provider.NameGitHub, cs.Name, func() {
		uw.showCosmoCodespaceDetail(csName, repo)
	})

	// ── INFO: codespace details + SSH connection
	csNameVal := widget.NewLabel(cs.Name)
	csNameVal.TextStyle = fyne.TextStyle{Monospace: true}
	csNameVal.Truncation = fyne.TextTruncateEllipsis

	machineVal := widget.NewLabel(cs.MachineName)
	machineVal.Truncation = fyne.TextTruncateEllipsis

	createdVal := widget.NewLabel(formatTimeAgo(cs.CreatedAt))
	lastUsedVal := widget.NewLabel(formatTimeAgo(cs.LastUsedAt))

	sshHostVal := widget.NewLabel(fmt.Sprintf("cs.%s.github.dev", cs.Name))
	sshHostVal.TextStyle = fyne.TextStyle{Monospace: true}
	sshHostVal.Truncation = fyne.TextTruncateEllipsis

	pathVal := widget.NewLabel(target.WorkspacePath)
	pathVal.TextStyle = fyne.TextStyle{Monospace: true}
	pathVal.Truncation = fyne.TextTruncateEllipsis

	info := widget.NewForm(
		widget.NewFormItem("Codespace", csNameVal),
		widget.NewFormItem("Machine", machineVal),
		widget.NewFormItem("Created", createdVal),
		widget.NewFormItem("Last used", lastUsedVal),
		widget.NewFormItem("SSH host", sshHostVal),
		widget.NewFormItem("Path", pathVal),
	)

	ports := uw.buildCodespacePortsSection(cs.Name, repo)

	body := container.NewVBox(
		statusRow,
		heroTitle,
		repoRow,
		widget.NewSeparator(),
		actions,
		widget.NewSeparator(),
		info,
		widget.NewSeparator(),
		sshSection,
		widget.NewSeparator(),
		ports,
	)
	uw.setContent(container.NewPadded(body))
}

func (uw *unifiedWindow) buildCodespacePortsSection(csName, repo string) fyne.CanvasObject {
	title := caption("PORTS")
	viewID := "codespace:" + csName
	reshow := func() {
		if uw.stillOn(viewID) {
			uw.showCosmoCodespaceDetail(csName, repo)
		}
	}
	refreshBtn := widget.NewButton("Refresh", func() {
		uw.daemon.refreshPortsAsync(csName, reshow)
	})
	forwardBtn := widget.NewButton("Forward port...", func() {
		uw.showAdHocPortForwardDialog(provider.NameGitHub, csName, reshow)
	})
	header := container.NewHBox(title, layout.NewSpacer(), forwardBtn, refreshBtn)

	entry := uw.daemon.ensurePortsWithCallback(csName, reshow)

	var rows []fyne.CanvasObject
	rows = append(rows, header)
	switch {
	case entry.Loading:
		rows = append(rows, widget.NewLabel("Loading forwarded ports..."))
	case entry.Err != nil:
		rows = append(rows, widget.NewLabel("Ports unavailable. Refresh to try again."))
	case len(entry.Ports) == 0:
		rows = append(rows, widget.NewLabel("No forwarded ports."))
	default:
		for _, port := range entry.Ports {
			rows = append(rows, uw.portRow(csName, repo, port))
		}
	}
	return container.NewVBox(rows...)
}

func (uw *unifiedWindow) portRow(csName, repo string, port codespace.Port) fyne.CanvasObject {
	title := widget.NewLabel(codespace.PortLabel(port))
	title.TextStyle = fyne.TextStyle{Bold: true}
	urlLabel := widget.NewLabel(port.BrowseURL)
	if port.BrowseURL == "" {
		urlLabel.SetText("No browse URL")
	}
	urlLabel.Truncation = fyne.TextTruncateEllipsis
	urlLabel.TextStyle = fyne.TextStyle{Monospace: true}

	openBtn := widget.NewButton("Open", func() {
		uw.daemon.openURL(port.BrowseURL)
	})
	copyBtn := widget.NewButton("Copy", func() {
		uw.daemon.copyText(port.BrowseURL)
	})
	if port.BrowseURL == "" {
		openBtn.Disable()
		copyBtn.Disable()
	}

	remotePort := port.SourcePort
	localPort := port.SourcePort
	var forwardBtn *widget.Button
	if uw.daemon.forwards != nil && uw.daemon.forwards.IsActive(provider.NameGitHub, csName, remotePort, localPort) {
		forwardBtn = widget.NewButton(fmt.Sprintf("Stop localhost %d", localPort), func() {
			uw.daemon.stopLocalPortForward(csName, remotePort, localPort)
			uw.showCosmoCodespaceDetail(csName, repo)
		})
	} else {
		forwardBtn = widget.NewButton(fmt.Sprintf("Forward localhost %d", localPort), func() {
			go func() {
				if err := uw.daemon.startLocalPortForward(csName, remotePort, localPort); err != nil {
					uw.daemon.notify(err.Error())
				}
				fyne.Do(func() {
					if uw.stillOn("codespace:" + csName) {
						uw.showCosmoCodespaceDetail(csName, repo)
					}
				})
			}()
		})
	}

	left := container.NewVBox(title, urlLabel)
	actions := container.NewHBox(openBtn, copyBtn, forwardBtn)
	return surfaceCard(container.NewBorder(nil, nil, nil, actions, left))
}
