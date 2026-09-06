package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/editor"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/sshconfig"
	"github.com/linuskendall/cosmonaut/internal/terminal"
)

// detailFocus identifies which group of inputs has keyboard focus inside
// the detail view. Tab cycles between them.
type detailFocus int

const (
	focusActions detailFocus = iota
	focusOptions
	focusPorts
)

// detailModel renders the per-workspace detail page. It owns the SSH
// option toggles and the inline delete confirmation; ports are read from
// the shared cache.
type detailModel struct {
	data      *AppletData
	workspace provider.Workspace

	focus         detailFocus
	actionsCursor int // 0=Open, 1=SSH, 2=Delete
	optionsCursor int // index into the per-provider sshOptionRow list
	portsCursor   int // index into displayed ports

	confirmDelete bool
}

// sshOptionKind identifies one of the SSH option toggles. Used so the
// detail view can render a provider-appropriate subset (Coder hides
// ControlMaster because coder.conf is shared across workspaces).
type sshOptionKind int

const (
	sshOptionControlMaster sshOptionKind = iota
	sshOptionMultiplexer
)

type sshOptionRow struct {
	kind  sshOptionKind
	label string
	hint  string
}

// sshOptionRowsFor returns the SSH option rows that should be shown
// for a workspace from the given provider. Coder hides ControlMaster
// because all Coder workspaces share ~/.ssh/cosmonaut/coder.conf, so
// toggling it per workspace is incoherent (last writer wins). The
// multiplexer is a per-invocation wrapper around the SSH command and is
// safe everywhere.
func sshOptionRowsFor(providerName string) []sshOptionRow {
	mux := sshOptionRow{
		kind:  sshOptionMultiplexer,
		label: "Terminal multiplexer",
		hint:  "SSH button and `cosmonaut shell` attach to a persistent tmux/zellij session.",
	}
	if providerName == provider.NameCoder {
		return []sshOptionRow{mux}
	}
	return []sshOptionRow{
		{
			kind:  sshOptionControlMaster,
			label: "Persistent SSH (ControlMaster)",
			hint:  "Multiplex extra sessions over one TCP connection — instant reconnects.",
		},
		mux,
	}
}

// sshOptionsSharedConfNote returns a non-empty explanation when the
// provider's on-disk SSH conf is shared across workspaces and the
// ControlMaster toggle is therefore hidden.
func sshOptionsSharedConfNote(providerName string) string {
	if providerName == provider.NameCoder {
		return "Coder workspaces share ~/.ssh/cosmonaut/coder.conf — ControlMaster is managed globally and can't be toggled per workspace."
	}
	return ""
}

func newDetailModel(d *AppletData, ws provider.Workspace) detailModel {
	return detailModel{data: d, workspace: ws}
}

// Init kicks off the ports fetch for GitHub codespaces as a proper command
// whose completion message repaints the view. Fetching from View (the old
// approach) both stacked subprocesses per repaint and never repainted when
// the fetch landed — "loading..." stuck until the next keypress.
func (m detailModel) Init() tea.Cmd {
	if m.workspace.Provider != provider.NameGitHub || m.data == nil {
		return nil
	}
	name := m.workspace.Name
	d := m.data
	return func() tea.Msg {
		done := make(chan struct{})
		if _, started := d.ensurePortsCache(name, func() { close(done) }); !started {
			// Cached already, or another fetch is in flight and owns the
			// repaint; nothing to wait on.
			return nil
		}
		<-done
		return reloadMsg{}
	}
}

func (m detailModel) update(msg tea.Msg, d *AppletData) (detailModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, nil
	case reloadMsg:
		// Re-fetch the latest workspace snapshot if the data layer has it.
		for _, latest := range d.Workspaces() {
			if latest.Provider == m.workspace.Provider && latest.Name == m.workspace.Name {
				m.workspace = latest
				return m, nil
			}
		}
		return m, nil
	case tea.KeyMsg:
		if m.confirmDelete {
			return m.handleConfirmDelete(msg, d)
		}
		return m.handleKey(msg, d)
	}
	return m, nil
}

func (m detailModel) handleKey(msg tea.KeyMsg, d *AppletData) (detailModel, tea.Cmd) {
	key := msg.String()
	switch key {
	case "esc":
		return m, switchTo(viewList, nil)
	case "tab":
		// Let the top-level handler swap views, but cycle focus first if
		// there's more than one group on this view.
		m.focus = (m.focus + 1) % 3
		return m, nil
	case "up", "k":
		m.moveCursor(-1, d)
	case "down", "j":
		m.moveCursor(1, d)
	case "enter", " ":
		return m.activate(d)
	case "r":
		// Refresh the ports cache for GitHub codespaces (no-op for Coder).
		if m.workspace.Provider == provider.NameGitHub {
			name := m.workspace.Name
			return m, func() tea.Msg {
				d.RefreshPorts(name)
				return reloadMsg{}
			}
		}
	}
	return m, nil
}

func (m detailModel) handleConfirmDelete(msg tea.KeyMsg, d *AppletData) (detailModel, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		ws := m.workspace
		m.confirmDelete = false
		return m, tea.Batch(
			deleteWorkspaceCmd(d, ws),
			switchTo(viewList, nil),
		)
	case "n", "N", "esc":
		m.confirmDelete = false
	}
	return m, nil
}

// moveCursor moves the cursor within the active focus group, wrapping.
func (m *detailModel) moveCursor(delta int, d *AppletData) {
	switch m.focus {
	case focusActions:
		m.actionsCursor = wrapDetail(m.actionsCursor+delta, 3)
	case focusOptions:
		n := len(sshOptionRowsFor(m.workspace.Provider))
		if n == 0 {
			return
		}
		m.optionsCursor = wrapDetail(m.optionsCursor+delta, n)
	case focusPorts:
		n := m.visiblePortCount(d)
		if n == 0 {
			return
		}
		m.portsCursor = wrapDetail(m.portsCursor+delta, n)
	}
}

func wrapDetail(i, n int) int {
	if n <= 0 {
		return 0
	}
	return ((i % n) + n) % n
}

// activate runs the action under the cursor of the focused group.
func (m detailModel) activate(d *AppletData) (detailModel, tea.Cmd) {
	switch m.focus {
	case focusActions:
		switch m.actionsCursor {
		case 0:
			return m, m.openInEditor(d)
		case 1:
			return m, m.openSSHShell(d)
		case 2:
			if d.canDeleteReason(m.workspace.Provider) != "" {
				return m, emitFlash("delete unavailable: "+d.canDeleteReason(m.workspace.Provider), true)
			}
			m.confirmDelete = true
		}
	case focusOptions:
		cfg := d.Config()
		rows := sshOptionRowsFor(m.workspace.Provider)
		if m.optionsCursor < 0 || m.optionsCursor >= len(rows) {
			return m, nil
		}
		switch rows[m.optionsCursor].kind {
		case sshOptionControlMaster:
			cur := cfg.WorkspaceSSHControlMaster(m.workspace.Provider, m.workspace.Name)
			next := !cur
			cfg.SetWorkspaceSSHControlMaster(m.workspace.Provider, m.workspace.Name, &next)
			if err := d.PersistConfig(); err != nil {
				return m, emitFlash("save: "+err.Error(), true)
			}
			// Rewrite the on-disk conf so the new setting takes effect
			// without waiting for the next PrepareSSH call.
			m.applySSHOptionsAsync(d)
			return m, emitFlash(fmt.Sprintf("ControlMaster %s", onOff(next)), false)
		case sshOptionMultiplexer:
			cur := cfg.WorkspaceSSHMultiplexer(m.workspace.Provider, m.workspace.Name)
			next := nextMultiplexer(cur)
			cfg.SetWorkspaceSSHMultiplexer(m.workspace.Provider, m.workspace.Name, &next)
			if err := d.PersistConfig(); err != nil {
				return m, emitFlash("save: "+err.Error(), true)
			}
			return m, emitFlash(fmt.Sprintf("multiplexer: %s", next), false)
		}
	case focusPorts:
		return m.activatePort(d)
	}
	return m, nil
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// nextMultiplexer cycles through the multiplexer choices in order, so a
// single keypress steps none → tmux → zellij → none.
func nextMultiplexer(cur string) string {
	for i, m := range config.Multiplexers {
		if m == cur {
			return config.Multiplexers[(i+1)%len(config.Multiplexers)]
		}
	}
	return config.MultiplexerNone
}

func (m detailModel) applySSHOptionsAsync(d *AppletData) {
	go func() {
		paths := sshconfig.ResolvePaths()
		confPath := paths.WorkspaceConfigPath(m.workspace.Provider, m.workspace.Name)
		opts := sshconfig.ManagedExtrasOptions{
			ControlMaster: d.Config().WorkspaceSSHControlMaster(m.workspace.Provider, m.workspace.Name),
		}
		_, _ = sshconfig.RefreshManagedExtras(confPath, opts)
	}()
}

func (m detailModel) openInEditor(d *AppletData) tea.Cmd {
	return func() tea.Msg {
		paths := sshconfig.ResolvePaths()
		alias, ok := sshconfig.ReadExistingWorkspaceAlias(paths, m.workspace.Provider, m.workspace.Name)
		if !ok {
			// Need to prepare SSH first.
			mgr, err := d.ManagerForProvider(m.workspace.Provider)
			if err != nil {
				return flashMsg{text: err.Error(), err: true}
			}
			ws := m.workspace
			if _, err := mgr.StartWorkspace(&ws); err != nil {
				return flashMsg{text: err.Error(), err: true}
			}
			if err := mgr.EnsureReachable(&ws); err != nil {
				return flashMsg{text: err.Error(), err: true}
			}
			opts := sshconfig.ManagedExtrasOptions{
				ControlMaster: d.Config().WorkspaceSSHControlMaster(ws.Provider, ws.Name),
			}
			alias, err = mgr.PrepareSSH(paths, &ws, opts)
			if err != nil {
				return flashMsg{text: err.Error(), err: true}
			}
		}
		edName := d.Config().GetEditor()
		ed, err := editor.ForName(edName)
		if err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		workspacePath := m.guessWorkspacePath()
		if err := ed.LaunchRemote(alias, workspacePath); err != nil {
			return flashMsg{text: err.Error(), err: true}
		}
		return flashMsg{text: fmt.Sprintf("Launched %s", ed.Name())}
	}
}

func (m detailModel) openSSHShell(d *AppletData) tea.Cmd {
	return func() tea.Msg {
		paths := sshconfig.ResolvePaths()
		alias, ok := sshconfig.ReadExistingWorkspaceAlias(paths, m.workspace.Provider, m.workspace.Name)
		if !ok {
			return flashMsg{text: "no SSH config yet — open in editor first", err: true}
		}
		mux := d.Config().WorkspaceSSHMultiplexer(m.workspace.Provider, m.workspace.Name)
		go terminal.OpenSSHInTerminal(alias, m.guessWorkspacePath(), mux)
		return flashMsg{text: fmt.Sprintf("Opening shell to %s", alias)}
	}
}

func (m detailModel) guessWorkspacePath() string {
	if m.workspace.Provider == provider.NameCoder {
		return "/workspaces/" + m.workspace.Name
	}
	if m.workspace.Repository != "" {
		parts := strings.SplitN(m.workspace.Repository, "/", 2)
		return "/workspaces/" + parts[len(parts)-1]
	}
	return "/workspaces/" + m.workspace.Name
}

func (m detailModel) activatePort(d *AppletData) (detailModel, tea.Cmd) {
	if m.workspace.Provider != provider.NameGitHub {
		return m, nil
	}
	entry := d.PortCache(m.workspace.Name)
	if m.portsCursor >= len(entry.Ports) {
		return m, nil
	}
	p := entry.Ports[m.portsCursor]
	csName := m.workspace.Name
	if d.PortForwards().IsActive(provider.NameGitHub, csName, p.SourcePort, p.SourcePort) {
		d.PortForwards().Stop(provider.NameGitHub, csName, p.SourcePort, p.SourcePort)
		return m, emitFlash(fmt.Sprintf("Stopped localhost:%d", p.SourcePort), false)
	}
	return m, func() tea.Msg {
		if err := d.PortForwards().Start(provider.NameGitHub, csName, p.SourcePort, p.SourcePort); err != nil {
			return flashMsg{text: "forward: " + err.Error(), err: true}
		}
		return flashMsg{text: fmt.Sprintf("Forwarding localhost:%d", p.SourcePort)}
	}
}

func (m detailModel) visiblePortCount(d *AppletData) int {
	if m.workspace.Provider != provider.NameGitHub {
		return 0
	}
	entry := d.PortCache(m.workspace.Name)
	return len(entry.Ports)
}

// ── Rendering ────────────────────────────────────────────────────────

func (m detailModel) view(d *AppletData, width, height int) string {
	var b strings.Builder

	ws := m.workspace
	// Header
	stateText := stateColor(ws.State).Render(strings.ToUpper(ws.State))
	title := ws.DisplayName
	if title == "" {
		title = ws.Name
	}
	fmt.Fprintf(&b, "%s %s\n", stateText, titleStyle.Render(title))
	subtitle := dimStyle.Render(ws.Provider)
	if ws.Repository != "" {
		subtitle += dimStyle.Render("  ⌂ " + ws.Repository)
	}
	if ws.Branch != "" {
		subtitle += dimStyle.Render("  ⎇ " + ws.Branch)
	}
	b.WriteString(subtitle + "\n\n")

	// Actions
	b.WriteString(m.renderActionGroup(d) + "\n")

	// Info table
	b.WriteString(captionStyle.Render("INFO") + "\n")
	infoRows := m.infoRows(d)
	for _, r := range infoRows {
		fmt.Fprintf(&b, "  %s  %s\n", dimStyle.Render(padRight(r[0], 14)), r[1])
	}
	b.WriteString("\n")

	// SSH options
	b.WriteString(m.renderOptionsGroup(d) + "\n")

	// Ports section
	b.WriteString(m.renderPortsGroup(d) + "\n")

	if m.confirmDelete {
		b.WriteString("\n" + errorStyle.Render(fmt.Sprintf("Delete %s? (y/N) ", ws.Name)) + "\n")
	}

	b.WriteString("\n")
	hint := "tab cycle group  ↑/↓ move  enter toggle/run  r refresh ports  esc back"
	b.WriteString(dimStyle.Render(hint))

	return clampHeight(b.String(), height, width)
}

func (m detailModel) renderActionGroup(d *AppletData) string {
	header := captionStyle.Render("ACTIONS")
	if m.focus == focusActions {
		header = selectedStyle.Render("ACTIONS")
	}
	actions := []string{"Open in editor", "SSH shell", "Delete"}
	if reason := d.canDeleteReason(m.workspace.Provider); reason != "" {
		actions[2] = "Delete — " + reason
	}
	var lines []string
	lines = append(lines, header)
	for i, label := range actions {
		cursor := "  "
		if m.focus == focusActions && i == m.actionsCursor {
			cursor = cursorStyle.Render("> ")
		}
		if m.focus == focusActions && i == m.actionsCursor {
			label = selectedStyle.Render(label)
		}
		if i == 2 && d.canDeleteReason(m.workspace.Provider) != "" {
			label = dimStyle.Render(label)
		}
		lines = append(lines, cursor+label)
	}
	return strings.Join(lines, "\n")
}

func (m detailModel) renderOptionsGroup(d *AppletData) string {
	header := captionStyle.Render("SSH OPTIONS")
	if m.focus == focusOptions {
		header = selectedStyle.Render("SSH OPTIONS")
	}
	cfg := d.Config()
	rows := sshOptionRowsFor(m.workspace.Provider)
	var lines []string
	lines = append(lines, header)
	if note := sshOptionsSharedConfNote(m.workspace.Provider); note != "" {
		lines = append(lines, "  "+dimStyle.Render(note))
	}
	for i, r := range rows {
		cursor := "  "
		if m.focus == focusOptions && i == m.optionsCursor {
			cursor = cursorStyle.Render("> ")
		}
		var box string
		switch r.kind {
		case sshOptionControlMaster:
			box = "[ ]"
			if cfg.WorkspaceSSHControlMaster(m.workspace.Provider, m.workspace.Name) {
				box = stateOK.Render("[x]")
			}
		case sshOptionMultiplexer:
			mux := cfg.WorkspaceSSHMultiplexer(m.workspace.Provider, m.workspace.Name)
			box = "[" + mux + "]"
			if mux != config.MultiplexerNone {
				box = stateOK.Render(box)
			}
		}
		line := fmt.Sprintf("%s%s %s", cursor, box, r.label)
		if m.focus == focusOptions && i == m.optionsCursor {
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
		lines = append(lines, "      "+dimStyle.Render(r.hint))
	}
	return strings.Join(lines, "\n")
}

func (m detailModel) renderPortsGroup(d *AppletData) string {
	header := captionStyle.Render("PORTS")
	if m.focus == focusPorts {
		header = selectedStyle.Render("PORTS")
	}
	var lines []string
	lines = append(lines, header)
	if m.workspace.Provider != provider.NameGitHub {
		lines = append(lines, "  "+dimStyle.Render("(port forwarding shown for GitHub codespaces)"))
		return strings.Join(lines, "\n")
	}
	entry := d.EnsurePortsCache(m.workspace.Name, nil)
	switch {
	case entry.Loading:
		lines = append(lines, "  "+dimStyle.Render("loading..."))
	case entry.Err != nil:
		lines = append(lines, "  "+errorStyle.Render("ports unavailable: "+entry.Err.Error()))
	case len(entry.Ports) == 0:
		lines = append(lines, "  "+dimStyle.Render("no forwarded ports"))
	default:
		for i, p := range entry.Ports {
			cursor := "  "
			if m.focus == focusPorts && i == m.portsCursor {
				cursor = cursorStyle.Render("> ")
			}
			active := d.PortForwards().IsActive(provider.NameGitHub, m.workspace.Name, p.SourcePort, p.SourcePort)
			marker := "○"
			if active {
				marker = stateOK.Render("●")
			}
			label := codespace.PortLabel(p)
			line := fmt.Sprintf("%s%s  %s", cursor, marker, label)
			if m.focus == focusPorts && i == m.portsCursor {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m detailModel) infoRows(d *AppletData) [][2]string {
	ws := m.workspace
	rows := [][2]string{}
	rows = append(rows, [2]string{"Name", ws.Name})
	if ws.MachineName != "" {
		rows = append(rows, [2]string{"Machine", ws.MachineName})
	}
	if ws.CreatedAt != "" {
		rows = append(rows, [2]string{"Created", ws.CreatedAt})
	}
	if ws.LastUsedAt != "" {
		rows = append(rows, [2]string{"Last used", ws.LastUsedAt})
	}
	var alias string
	switch ws.Provider {
	case provider.NameGitHub:
		alias = fmt.Sprintf("cs.%s.github.dev", ws.Name)
	case provider.NameCoder:
		alias = fmt.Sprintf("%s.coder", ws.Name)
	}
	rows = append(rows, [2]string{"SSH host", alias})
	rows = append(rows, [2]string{"Path", m.guessWorkspacePath()})
	_ = d
	return rows
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
