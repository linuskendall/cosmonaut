package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/slug"
)

// createField enumerates the focusable rows in the create form. The
// provider radio is only present when both providers are configured;
// when one is locked in (the common case), focusProvider is skipped.
type createField int

const (
	focusProvider createField = iota
	focusRepoOrTemplate
	focusLabelOrName
	focusSubmit
	focusCancel
)

// createDoneMsg is dispatched when a create call finishes — either
// successfully (Workspace != nil) or with an error.
type createDoneMsg struct {
	workspace *provider.Workspace
	err       error
}

// createModel renders the "new workspace" form. Provider-aware: for
// GitHub it asks for repo + optional work label; for Coder it asks the
// user to pick a configured template target and pick a workspace name.
type createModel struct {
	providerName string // current selection — "github" or "coder"
	providerLock bool   // true when only one provider is configured

	// GitHub fields
	repoInput  textinput.Model
	labelInput textinput.Model

	// Coder fields
	coderTargets []string // names of config targets that have a Coder block
	coderIdx     int
	nameInput    textinput.Model

	focus      createField
	submitting bool
	spinner    spinner.Model

	// active is true once the form has meaningful state — either seeded
	// at construction or after the user has typed into a field. It gates
	// whether the Create tab participates in the tab rotation, and the
	// applet uses it to decide whether to preserve the model across view
	// switches.
	active bool
}

func newCreateModel(d *AppletData) createModel {
	m := createModel{}

	repo := textinput.New()
	repo.Placeholder = "owner/repo"
	repo.CharLimit = 100
	repo.Width = 40
	m.repoInput = repo

	label := textinput.New()
	label.Placeholder = "(optional) e.g. fix indexer health checks"
	label.CharLimit = 80
	label.Width = 40
	m.labelInput = label

	name := textinput.New()
	name.Placeholder = "workspace name"
	name.CharLimit = 60
	name.Width = 40
	m.nameInput = name

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	m.spinner = sp

	// Inventory Coder targets (those with a Coder block) for the
	// template picker; create is only meaningful against one of these.
	for name, t := range d.Config().TargetsSnapshot() {
		if t.Coder != nil {
			m.coderTargets = append(m.coderTargets, name)
		}
	}

	// Default provider selection. Prefer whichever provider CLI is
	// installed; if both are, default to GitHub but allow toggling. The
	// provider row is hidden when only one is in play. Coder also
	// requires at least one Coder-tagged target since create needs a
	// template selector.
	//
	// Deliberately only a PATH lookup here, not the full auth probe:
	// IsGitHubAvailable/IsCoderAvailable each exec the CLI with a 5s
	// timeout, and this constructor runs inside Update on the UI thread —
	// a hung CLI froze the whole TUI for up to 10 seconds when the user
	// pressed "n". Auth problems still surface at submit with a clear
	// error message.
	gh := provider.HasGitHubCLI() && d.cfg.ProviderEnabled(provider.NameGitHub)
	cd := provider.HasCoderCLI() && len(m.coderTargets) > 0 && d.cfg.ProviderEnabled(provider.NameCoder)
	switch {
	case gh && cd:
		m.providerName = provider.NameGitHub
	case cd:
		m.providerName = provider.NameCoder
		m.providerLock = true
	default:
		m.providerName = provider.NameGitHub
		m.providerLock = true
	}
	m.focusInitial()
	return m
}

// newCreateModelWithSeed is the AppletInitial pathway: builds a Create
// view pre-filled with the provider + repository the root command
// resolved. The first focus lands on the work-label / workspace-name
// field so the user only has to type the optional details.
func newCreateModelWithSeed(d *AppletData, seed AppletCreateSeed) createModel {
	m := newCreateModel(d)
	switch seed.Provider {
	case provider.NameCoder, provider.NameGitHub:
		m.providerName = seed.Provider
	}
	if seed.Repository != "" && m.providerName == provider.NameGitHub {
		m.repoInput.SetValue(seed.Repository)
		m.focus = focusLabelOrName
	}
	m.applyFocus()
	// A seeded form already carries user-intent state, so mark it active
	// from the start — that way the Create tab is visible on first paint
	// and tabbing away/back doesn't wipe the seed.
	m.active = true
	return m
}

// focusInitial moves the cursor to the first editable field after
// (re)building the model, skipping the provider row when it's locked.
func (m *createModel) focusInitial() {
	if m.providerLock {
		m.focus = focusRepoOrTemplate
	} else {
		m.focus = focusProvider
	}
	m.applyFocus()
}

// applyFocus updates each textinput's Focused state to match m.focus, so
// only the focused field renders a blinking cursor and accepts keystrokes.
func (m *createModel) applyFocus() {
	m.repoInput.Blur()
	m.labelInput.Blur()
	m.nameInput.Blur()
	switch m.providerName {
	case provider.NameGitHub:
		switch m.focus {
		case focusRepoOrTemplate:
			m.repoInput.Focus()
		case focusLabelOrName:
			m.labelInput.Focus()
		}
	case provider.NameCoder:
		if m.focus == focusLabelOrName {
			m.nameInput.Focus()
		}
	}
}

func (m createModel) Init() tea.Cmd { return textinput.Blink }

func (m createModel) update(msg tea.Msg, d *AppletData) (createModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, nil
	case createDoneMsg:
		m.submitting = false
		if msg.err != nil {
			return m, emitFlash("create: "+msg.err.Error(), true)
		}
		// Successful create: the form is no longer "in progress", so drop
		// the active flag and wipe inputs. The applet will rebuild a fresh
		// model on the next switchToFresh(viewCreate, ...).
		m = m.clear()
		// Switch to detail for the new workspace + force a poll so the
		// list catches up. pollDoneMsg is routed to the list and detail
		// models regardless of focus (a bare reloadMsg went to whatever
		// view was active, so the list never rebuilt its rows).
		ws := msg.workspace
		return m, tea.Batch(
			func() tea.Msg { return pollDoneMsg{result: d.Poll()} },
			emitFlash(fmt.Sprintf("Created %s", ws.Name), false),
			switchTo(viewDetail, ws),
		)
	case spinner.TickMsg:
		if m.submitting {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	case tea.KeyMsg:
		if m.submitting {
			// Drop key input while a create is in flight — anything but
			// ctrl+c would race the goroutine.
			return m, nil
		}
		return m.handleKey(msg, d)
	}
	return m.routeInput(msg)
}

func (m createModel) handleKey(msg tea.KeyMsg, d *AppletData) (createModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Park the form: keep typed input and the active flag so the
		// Create tab stays in the header and tabbing back from the list
		// restores it. Deliberate abandonment is the Cancel button, which
		// clears the form.
		return m, switchTo(viewList, nil)
	case "tab":
		m = m.cycleFocus(1)
		return m, nil
	case "shift+tab":
		m = m.cycleFocus(-1)
		return m, nil
	case "left", "right":
		if m.focus == focusProvider {
			if m.providerName == provider.NameGitHub {
				m.providerName = provider.NameCoder
			} else {
				m.providerName = provider.NameGitHub
			}
			m.applyFocus()
			return m, nil
		}
		if m.providerName == provider.NameCoder && m.focus == focusRepoOrTemplate {
			if len(m.coderTargets) == 0 {
				return m, nil
			}
			if msg.String() == "left" {
				m.coderIdx = (m.coderIdx - 1 + len(m.coderTargets)) % len(m.coderTargets)
			} else {
				m.coderIdx = (m.coderIdx + 1) % len(m.coderTargets)
			}
			return m, nil
		}
	case "enter":
		if m.focus == focusCancel {
			m = m.clear()
			return m, switchTo(viewList, nil)
		}
		if m.focus == focusSubmit || m.focus == focusLabelOrName || (m.providerName == provider.NameCoder && m.focus == focusRepoOrTemplate) {
			return m.submit(d)
		}
		// Enter from an early field advances rather than submits, so the
		// user can fill out everything before committing.
		m = m.cycleFocus(1)
		return m, nil
	}
	return m.routeInput(msg)
}

func (m createModel) routeInput(msg tea.Msg) (createModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.providerName {
	case provider.NameGitHub:
		switch m.focus {
		case focusRepoOrTemplate:
			m.repoInput, cmd = m.repoInput.Update(msg)
		case focusLabelOrName:
			m.labelInput, cmd = m.labelInput.Update(msg)
		}
	case provider.NameCoder:
		if m.focus == focusLabelOrName {
			m.nameInput, cmd = m.nameInput.Update(msg)
		}
	}
	// If anything has been typed, the form is now "in progress" and should
	// survive view switches.
	if m.hasInput() {
		m.active = true
	}
	return m, cmd
}

// hasInput reports whether the user has typed anything the model would care
// about preserving — used to flip the active flag once the form is no
// longer pristine.
func (m createModel) hasInput() bool {
	return strings.TrimSpace(m.repoInput.Value()) != "" ||
		strings.TrimSpace(m.labelInput.Value()) != "" ||
		strings.TrimSpace(m.nameInput.Value()) != ""
}

// clear drops the in-progress flag and wipes the text inputs, returning the
// updated model. Used when the user abandons or successfully submits the
// form — both signals that the next entry into Create should start fresh.
func (m createModel) clear() createModel {
	m.active = false
	m.repoInput.SetValue("")
	m.labelInput.SetValue("")
	m.nameInput.SetValue("")
	return m
}

func (m createModel) cycleFocus(delta int) createModel {
	fields := m.focusOrder()
	for i, f := range fields {
		if f == m.focus {
			next := (i + delta + len(fields)) % len(fields)
			m.focus = fields[next]
			break
		}
	}
	m.applyFocus()
	return m
}

func (m createModel) focusOrder() []createField {
	fields := []createField{}
	if !m.providerLock {
		fields = append(fields, focusProvider)
	}
	fields = append(fields, focusRepoOrTemplate, focusLabelOrName, focusSubmit, focusCancel)
	return fields
}

// submit validates the form, kicks the create call off in a goroutine,
// and shows the spinner until createDoneMsg comes back.
func (m createModel) submit(d *AppletData) (createModel, tea.Cmd) {
	target, err := m.buildTarget(d)
	if err != nil {
		return m, emitFlash(err.Error(), true)
	}
	m.submitting = true
	providerName := m.providerName
	return m, tea.Batch(
		m.spinner.Tick,
		func() tea.Msg {
			mgr, err := d.ManagerForProvider(providerName)
			if err != nil {
				return createDoneMsg{err: err}
			}
			ws, err := mgr.CreateWorkspace(target, false)
			return createDoneMsg{workspace: ws, err: err}
		},
	)
}

// buildTarget assembles the config.Target the provider's CreateWorkspace
// will receive. For GitHub it composes a display name from the work label
// the way the standalone picker does; for Coder it inherits the template
// from the selected configured target.
func (m createModel) buildTarget(d *AppletData) (config.Target, error) {
	switch m.providerName {
	case provider.NameGitHub:
		repo := strings.TrimSpace(m.repoInput.Value())
		if repo == "" || !strings.Contains(repo, "/") {
			return config.Target{}, fmt.Errorf("repository must be in owner/repo form")
		}
		target := config.Target{Repository: repo}
		// If the user has a config target for this repo, inherit machine,
		// location, etc. so the new codespace lines up with their defaults.
		for _, t := range d.Config().TargetsSnapshot() {
			if t.Repository == repo {
				target = t
				break
			}
		}
		if label := strings.TrimSpace(m.labelInput.Value()); label != "" {
			target.DisplayName = slug.BuildDisplayName(target.Repository, target.Branch, label, target.DisplayName)
		}
		return target, nil
	case provider.NameCoder:
		if len(m.coderTargets) == 0 {
			return config.Target{}, fmt.Errorf("no config target with a coder block — edit config first")
		}
		name := m.coderTargets[m.coderIdx]
		target, _ := d.Config().Target(name)
		if wsName := strings.TrimSpace(m.nameInput.Value()); wsName != "" {
			if target.Coder == nil {
				target.Coder = &config.CoderTargetConfig{}
			}
			target.Coder.WorkspaceName = wsName
		}
		return target, nil
	}
	return config.Target{}, fmt.Errorf("unknown provider %q", m.providerName)
}

// ── Rendering ────────────────────────────────────────────────────

func (m createModel) view(d *AppletData, width, height int) string {
	var b strings.Builder
	b.WriteString(captionStyle.Render("CREATE NEW WORKSPACE") + "\n\n")

	if !m.providerLock {
		b.WriteString(m.renderProviderRow() + "\n\n")
	}

	switch m.providerName {
	case provider.NameGitHub:
		b.WriteString(m.renderGitHubForm() + "\n")
	case provider.NameCoder:
		b.WriteString(m.renderCoderForm(d) + "\n")
	}

	b.WriteString("\n" + m.renderButtons() + "\n")

	if m.submitting {
		b.WriteString("\n" + m.spinner.View() + " Creating workspace...\n")
	}

	b.WriteString("\n" + dimStyle.Render("tab/shift+tab cycle  enter submit  esc cancel"))
	return clampHeight(b.String(), height, width)
}

func (m createModel) renderProviderRow() string {
	label := "Provider:"
	if m.focus == focusProvider {
		label = selectedStyle.Render(label)
	}
	gh := provider.NameGitHub
	cd := provider.NameCoder
	if m.providerName == gh {
		gh = selectedStyle.Render("[" + gh + "]")
		cd = dimStyle.Render(" " + cd + " ")
	} else {
		gh = dimStyle.Render(" " + gh + " ")
		cd = selectedStyle.Render("[" + cd + "]")
	}
	hint := dimStyle.Render("(←/→ to switch)")
	return fmt.Sprintf("%s  %s %s  %s", label, gh, cd, hint)
}

func (m createModel) renderGitHubForm() string {
	rows := []struct {
		label string
		field createField
		view  string
	}{
		{"Repository", focusRepoOrTemplate, m.repoInput.View()},
		{"Work label", focusLabelOrName, m.labelInput.View()},
	}
	var lines []string
	for _, r := range rows {
		cursor := "  "
		label := r.label
		if m.focus == r.field {
			cursor = cursorStyle.Render("> ")
			label = selectedStyle.Render(label)
		}
		lines = append(lines, fmt.Sprintf("%s%s\n      %s", cursor, padRight(label, 14), r.view))
	}
	return strings.Join(lines, "\n")
}

func (m createModel) renderCoderForm(d *AppletData) string {
	if len(m.coderTargets) == 0 {
		return errorStyle.Render("No config target with a coder block — edit config first.")
	}
	target := m.coderTargets[m.coderIdx]
	t, _ := d.Config().Target(target)
	tplName := ""
	if t.Coder != nil {
		tplName = t.Coder.Template
	}

	// Template row
	cursor := "  "
	tplLabel := "Template"
	if m.focus == focusRepoOrTemplate {
		cursor = cursorStyle.Render("> ")
		tplLabel = selectedStyle.Render(tplLabel)
	}
	hint := dimStyle.Render("(←/→ to switch)")
	tplRow := fmt.Sprintf("%s%s\n      %s %s", cursor, padRight(tplLabel, 14),
		selectedStyle.Render("‹ "+target+" ›"), dimStyle.Render("("+tplName+")  "+hint))

	// Workspace name row — defaults to the target's WorkspaceName if set.
	if m.nameInput.Value() == "" && t.Coder != nil && t.Coder.WorkspaceName != "" {
		m.nameInput.SetValue(t.Coder.WorkspaceName)
	}
	nameCursor := "  "
	nameLabel := "Workspace name"
	if m.focus == focusLabelOrName {
		nameCursor = cursorStyle.Render("> ")
		nameLabel = selectedStyle.Render(nameLabel)
	}
	nameRow := fmt.Sprintf("%s%s\n      %s", nameCursor, padRight(nameLabel, 14), m.nameInput.View())

	return tplRow + "\n" + nameRow
}

func (m createModel) renderButtons() string {
	submit := "[ Create ]"
	cancel := "[ Cancel ]"
	if m.focus == focusSubmit {
		submit = selectedStyle.Render(submit)
	}
	if m.focus == focusCancel {
		cancel = selectedStyle.Render(cancel)
	}
	return "  " + submit + "    " + cancel
}
