package daemon

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/history"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

const maxSubmenuCodespaces = 5

// trayInteractionWindow is how long after a tray-opened event we treat
// the menu as in-use. Calling SetSystemTrayMenu while the user is
// navigating a submenu drops their focus and dismisses the submenu, so
// rebuildTrayMenu defers within this window. The deferred rebuild is
// flushed by a timer that fires once the window expires, rather than
// at the start of the next tray-open event (which would still race the
// user's first click).
const trayInteractionWindow = 30 * time.Second

// trayApplyCooldown is the minimum gap between two SetSystemTrayMenu
// calls. Even when no interaction window is active, back-to-back applies
// (e.g. a fast poll completing right after a config change) can briefly
// flicker the menu; this cooldown coalesces them.
const trayApplyCooldown = 2 * time.Second

// Tray rebuild gate semantics
// ---------------------------
//
// The system tray menu is a shared mutable resource. Every call to
// SetSystemTrayMenu replaces the menu wholesale, which on macOS and
// Linux has the side effect of closing any open submenu the user is
// currently navigating. rebuildTrayMenu is therefore guarded by two
// gates, both checked atomically under d.mu:
//
//  1. Interaction window: if the tray was opened less than
//     trayInteractionWindow ago, we assume the user may still be
//     browsing the menu. Apply is deferred. (We have no portable
//     "menu closed" signal from fyne.io/systray; see the TODO in
//     watchTrayOpened.)
//
//  2. Cooldown: if applyTrayMenu ran less than trayApplyCooldown ago,
//     defer this apply to coalesce a burst of back-to-back rebuilds.
//
// When either gate trips, pendingRebuild is set and a single shared
// timer (rebuildTimer) is armed to retry once the gate is expected to
// have cleared. Subsequent rebuild requests during the gate window are
// idempotent: they set pendingRebuild (already true) and bail out.
//
// onTrayOpened intentionally does NOT flush a pending rebuild right
// away — doing so would replace the menu as the user clicks it. The
// flush is scheduled via the same timer mechanism, firing after the
// interaction window elapses.

// buildTrayMenu constructs the system tray menu from config, history,
// and cached codespace state.
func (d *Daemon) buildTrayMenu() *fyne.Menu {
	var items []*fyne.MenuItem

	// Auth problems hide the affected provider's submenu (see
	// providerUsable), so surface a single prominent item at the top that
	// routes to the settings Health section to fix sign-in.
	if authItem := d.authIssueMenuItem(); authItem != nil {
		items = append(items, authItem)
	}

	if githubItem := d.githubCodespacesMenu(); githubItem != nil {
		items = append(items, githubItem)
	}
	if coderItem := d.coderWorkspaceMenu(); coderItem != nil {
		items = append(items, coderItem)
	}

	// Launch default target.
	if len(items) > 0 {
		items = append(items, fyne.NewMenuItemSeparator())
	}
	launchItem := fyne.NewMenuItem("Launch", func() {
		go d.launchDefaultTarget()
	})
	if d.Cfg.GetDefaultTarget() == "" {
		launchItem.Disabled = true
	}
	items = append(items, launchItem)
	items = append(items, fyne.NewMenuItem("Open picker...", func() {
		go d.showGUI()
	}))

	// Refresh — manual re-poll of all providers. Auto-refresh fires on
	// tray-open and window-focus, but a visible button is the escape
	// hatch when either signal misses (cold start, CLI not yet ready,
	// flaky provider call) and lets the user prove to themselves that
	// the data is fresh.
	items = append(items, fyne.NewMenuItem("Refresh workspaces", func() {
		d.forcePollAsync(nil)
	}))

	// Preferences.
	items = append(items, fyne.NewMenuItemSeparator())
	items = append(items, d.preferencesMenuItem())

	// Quit.
	items = append(items, fyne.NewMenuItemSeparator())
	items = append(items, fyne.NewMenuItem("Quit", func() {
		d.Stop()
	}))

	return fyne.NewMenu("cosmonaut", items...)
}

func (d *Daemon) githubCodespacesMenu() *fyne.MenuItem {
	if !d.Cfg.ProviderEnabled(provider.NameGitHub) {
		return nil
	}
	all := d.Codespaces()
	// Use the poller's cached ProviderStatus, never a live
	// provider.IsGitHubAvailable() probe: this runs inside fyne.Do on
	// every tray rebuild, and the probe execs `gh auth status` with a 5s
	// timeout — an offline laptop froze the whole UI for seconds per
	// rebuild. providerUsable also hides the submenu on an auth error;
	// authIssueMenuItem then routes the user to the Health section.
	if len(all) == 0 && !d.providerUsable(provider.NameGitHub) {
		return nil
	}

	repos := codespace.UniqueRepos(all)
	hist := history.Load()
	repos = hist.SortRepos(repos)

	items := make([]*fyne.MenuItem, 0, len(repos)+2)
	items = d.appendProviderStatusRow(items, provider.NameGitHub, githubStatusMessage)

	if len(repos) == 0 {
		items = append(items, disabledMenuItem("No codespaces"))
		root := fyne.NewMenuItem("Codespaces", nil)
		root.ChildMenu = fyne.NewMenu("", items...)
		return root
	}

	for _, repo := range repos {
		args := d.targetNameForRepo(repo)
		if args == "" {
			args = repo
		}
		item := fyne.NewMenuItem(repo, func() {
			go d.showGUI(args)
		})
		if sub := d.codespaceSubmenu(repo, args); sub != nil {
			item.ChildMenu = sub
		}
		items = append(items, item)
	}

	root := fyne.NewMenuItem("Codespaces", nil)
	root.ChildMenu = fyne.NewMenu("", items...)
	return root
}

// appendProviderStatusRow adds a disabled status row + separator to
// items when the provider's last ProviderStatus has a problem to
// report. Returns items unchanged when status is empty or the provider
// hasn't been polled yet. msgFn maps a status to provider-specific
// wording.
func (d *Daemon) appendProviderStatusRow(items []*fyne.MenuItem, providerName string, msgFn func(ProviderStatus) string) []*fyne.MenuItem {
	status := d.StatusFor(providerName)
	if status.CheckedAt.IsZero() {
		return items
	}
	msg := msgFn(status)
	if msg == "" {
		return items
	}
	return append(items, disabledMenuItem(msg), fyne.NewMenuItemSeparator())
}

// githubStatusMessage returns a short human-readable summary of the
// GitHub CLI local-setup state. Empty when everything is healthy.
func githubStatusMessage(status ProviderStatus) string {
	if !status.Available {
		return "gh CLI not installed"
	}
	if status.Err == nil {
		return ""
	}
	msg := strings.ToLower(status.Err.Error())
	switch {
	case strings.Contains(msg, `needs the "codespace" scope`):
		return "gh token missing codespace scope"
	case strings.Contains(msg, "not logged"),
		strings.Contains(msg, "authentication"),
		strings.Contains(msg, "auth status"):
		return "Not authenticated (run `gh auth login`)"
	default:
		return "Codespaces unavailable"
	}
}

func (d *Daemon) coderWorkspaceMenu() *fyne.MenuItem {
	if !d.Cfg.ProviderEnabled(provider.NameCoder) {
		return nil
	}
	workspaces := filterWorkspacesByProvider(d.Workspaces(), provider.NameCoder)
	// Cached status only — see githubCodespacesMenu for why no live probe.
	if len(workspaces) == 0 && !d.providerUsable(provider.NameCoder) {
		return nil
	}

	sort.Slice(workspaces, func(i, j int) bool {
		oi, oj := stateOrder(workspaces[i].State), stateOrder(workspaces[j].State)
		if oi != oj {
			return oi < oj
		}
		return workspaceLabel(workspaces[i]) < workspaceLabel(workspaces[j])
	})

	items := make([]*fyne.MenuItem, 0, len(workspaces)+3)
	items = d.appendProviderStatusRow(items, provider.NameCoder, coderStatusMessage)

	if len(workspaces) == 0 {
		items = append(items, disabledMenuItem("No Coder workspaces"))
		items = append(items, fyne.NewMenuItem("Create new...", func() {
			go d.showGUI()
		}))
		item := fyne.NewMenuItem("Coder", nil)
		item.ChildMenu = fyne.NewMenu("", items...)
		return item
	}
	for _, ws := range workspaces {
		label := fmt.Sprintf("%s %s", stateIcon(ws.State), ws.Name)
		item := fyne.NewMenuItem(label, func() {
			_, resolvedName := guiTargetForCoderWorkspace(d.Cfg, ws)
			go d.showGUI("--workspace", ws.Name, "--provider", provider.NameCoder, resolvedName)
		})
		item.ChildMenu = d.coderWorkspaceActionsMenu(ws)
		items = append(items, item)
	}
	item := fyne.NewMenuItem("Coder", nil)
	item.ChildMenu = fyne.NewMenu("", items...)
	return item
}

// coderStatusMessage returns a short human-readable summary of the
// Coder local-setup state. Empty when everything is healthy.
func coderStatusMessage(status ProviderStatus) string {
	if !status.Available {
		return "Coder CLI not installed"
	}
	if status.Err == nil {
		return ""
	}
	msg := status.Err.Error()
	switch {
	case strings.Contains(strings.ToLower(msg), "not authenticated"),
		strings.Contains(msg, "coder login"):
		return "Not authenticated (run `coder login`)"
	default:
		return "Coder unavailable"
	}
}

func (d *Daemon) coderWorkspaceActionsMenu(ws provider.Workspace) *fyne.Menu {
	target, resolvedName := guiTargetForCoderWorkspace(d.Cfg, ws)
	deleteItem := fyne.NewMenuItem("Delete workspace...", func() {
		go d.showGUI("--delete", "--workspace", ws.Name, "--provider", provider.NameCoder, resolvedName)
	})
	if !d.canDeleteWorkspace(provider.NameCoder) {
		deleteItem.Disabled = true
	}
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("Open in editor", func() {
			go d.showGUI("--workspace", ws.Name, "--provider", provider.NameCoder, resolvedName)
		}),
		fyne.NewMenuItem("Workspace settings...", func() {
			go d.showGUI("--detail", "--workspace", ws.Name, "--provider", provider.NameCoder, resolvedName)
		}),
		deleteItem,
		fyne.NewMenuItemSeparator(),
	}
	if target.Coder == nil || len(target.Coder.PortForwards) == 0 {
		items = append(items, disabledMenuItem("No configured ports"))
	} else {
		for _, pf := range target.Coder.PortForwards {
			item := fyne.NewMenuItem("Port "+coderPortForwardLabel(pf), nil)
			item.ChildMenu = d.coderPortActionsMenu(ws.Name, pf)
			items = append(items, item)
		}
	}
	items = append(
		items,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Forward port...", func() {
			go d.showGUI("--port-forward", "--workspace", ws.Name, "--provider", provider.NameCoder, resolvedName)
		}),
	)
	return fyne.NewMenu("", items...)
}

func (d *Daemon) coderPortActionsMenu(workspaceName string, pf config.PortForward) *fyne.Menu {
	remotePort := pf.RemotePort
	localPort := pf.LocalPort
	if localPort == 0 {
		localPort = remotePort
	}
	protocol := normalizePortForwardProtocol(pf.Protocol)

	var items []*fyne.MenuItem
	if d.forwards != nil && d.forwards.IsActiveProtocol(provider.NameCoder, workspaceName, protocol, remotePort, localPort) {
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Stop localhost %d", localPort), func() {
			d.stopWorkspacePortForward(provider.NameCoder, workspaceName, protocol, remotePort, localPort)
		}))
	} else {
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Forward localhost %d", localPort), func() {
			go func() {
				if err := d.startWorkspacePortForward(provider.NameCoder, workspaceName, protocol, remotePort, localPort); err != nil {
					d.notify(err.Error())
				}
			}()
		}))
	}
	return fyne.NewMenu("", items...)
}

func coderPortForwardLabel(pf config.PortForward) string {
	if pf.Label != "" {
		return fmt.Sprintf("%s (%d)", pf.Label, pf.RemotePort)
	}
	protocol := normalizePortForwardProtocol(pf.Protocol)
	if protocol != "tcp" {
		return fmt.Sprintf("%d (%s)", pf.RemotePort, protocol)
	}
	return fmt.Sprintf("%d", pf.RemotePort)
}

// codespaceSubmenu builds a submenu showing codespaces for a repo.
// Returns nil if the repo has no codespaces.
func (d *Daemon) codespaceSubmenu(repo, launchArgs string) *fyne.Menu {
	all := d.Codespaces()
	repoCS := codespace.FilterByRepo(all, repo)
	if len(repoCS) == 0 {
		return nil
	}

	// Sort: Available/Starting first, then others, alphabetically within groups.
	sort.Slice(repoCS, func(i, j int) bool {
		oi, oj := stateOrder(repoCS[i].State), stateOrder(repoCS[j].State)
		if oi != oj {
			return oi < oj
		}
		return csLabel(repoCS[i]) < csLabel(repoCS[j])
	})

	var items []*fyne.MenuItem
	limit := min(maxSubmenuCodespaces, len(repoCS))
	for _, cs := range repoCS[:limit] {
		label := fmt.Sprintf("%s %s", stateIcon(cs.State), csLabel(cs))
		item := fyne.NewMenuItem(label, func() {
			go d.showGUI("--workspace", cs.Name, "--provider", "github", launchArgs)
		})
		item.ChildMenu = d.codespaceActionsMenu(cs, launchArgs)
		items = append(items, item)
	}

	if len(repoCS) > maxSubmenuCodespaces {
		items = append(items, fyne.NewMenuItemSeparator())
		items = append(items, fyne.NewMenuItem("Show all...", func() {
			go d.showGUI(launchArgs)
		}))
	}

	return fyne.NewMenu("", items...)
}

func (d *Daemon) codespaceActionsMenu(cs codespace.Codespace, launchArgs string) *fyne.Menu {
	deleteItem := fyne.NewMenuItem("Delete codespace...", func() {
		go d.showGUI("--delete", "--workspace", cs.Name, "--provider", provider.NameGitHub, launchArgs)
	})
	if !d.canDeleteWorkspace(provider.NameGitHub) {
		deleteItem.Disabled = true
	}
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("Open in editor", func() {
			go d.showGUI("--workspace", cs.Name, "--provider", "github", launchArgs)
		}),
		fyne.NewMenuItem("Workspace settings...", func() {
			go d.showGUI("--detail", "--workspace", cs.Name, "--provider", "github", launchArgs)
		}),
		deleteItem,
		fyne.NewMenuItemSeparator(),
	}

	entry := d.ensurePorts(cs.Name)
	switch {
	case entry.Loading:
		items = append(items, disabledMenuItem("Loading ports..."))
	case entry.Err != nil:
		items = append(items, disabledMenuItem("Ports unavailable"))
	case len(entry.Ports) == 0:
		items = append(items, disabledMenuItem("No forwarded ports"))
	default:
		for _, port := range entry.Ports {
			item := fyne.NewMenuItem("Port "+codespace.PortLabel(port), nil)
			item.ChildMenu = d.portActionsMenu(cs.Name, port)
			items = append(items, item)
		}
	}

	items = append(
		items,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Refresh ports", func() {
			d.refreshPortsAsync(cs.Name, nil)
		}),
		fyne.NewMenuItem("Forward port...", func() {
			go d.showGUI("--port-forward", "--workspace", cs.Name, "--provider", provider.NameGitHub, launchArgs)
		}),
	)

	return fyne.NewMenu("", items...)
}

func (d *Daemon) portActionsMenu(codespaceName string, port codespace.Port) *fyne.Menu {
	var items []*fyne.MenuItem
	if port.BrowseURL == "" {
		items = append(items, disabledMenuItem("No browse URL"))
	} else {
		items = append(items, fyne.NewMenuItem("Open URL", func() {
			d.openURL(port.BrowseURL)
		}))
		items = append(items, fyne.NewMenuItem("Copy URL", func() {
			d.copyText(port.BrowseURL)
		}))
	}

	items = append(items, fyne.NewMenuItemSeparator())
	remotePort := port.SourcePort
	localPort := port.SourcePort
	if d.forwards != nil && d.forwards.IsActive(provider.NameGitHub, codespaceName, remotePort, localPort) {
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Stop localhost %d", localPort), func() {
			d.stopLocalPortForward(codespaceName, remotePort, localPort)
		}))
	} else {
		items = append(items, fyne.NewMenuItem(fmt.Sprintf("Forward localhost %d:%d", remotePort, localPort), func() {
			go func() {
				if err := d.startLocalPortForward(codespaceName, remotePort, localPort); err != nil {
					d.notify(err.Error())
				}
			}()
		}))
	}

	return fyne.NewMenu("", items...)
}

func disabledMenuItem(label string) *fyne.MenuItem {
	item := fyne.NewMenuItem(label, nil)
	item.Disabled = true
	return item
}

// stateOrder returns a sort key for codespace states (lower = first).
func stateOrder(state string) int {
	switch state {
	case "Available", "Started", "ready", "running", "connected":
		return 0
	case "Starting", "starting", "pending":
		return 1
	case "Stopped", "stopped":
		return 2
	default:
		return 3
	}
}

// stateIcon returns a Unicode indicator for a codespace state.
func stateIcon(state string) string {
	switch state {
	case "Available", "Started", "ready", "running", "connected":
		return "●"
	case "Starting", "starting", "pending":
		return "◐"
	default:
		return "○"
	}
}

// csLabel returns a short display label for a codespace.
func csLabel(cs codespace.Codespace) string {
	name := cs.DisplayName
	if name == "" {
		name = cs.Name
	}
	if cs.GitStatus != nil {
		ref := cs.GitStatus.Ref
		if ref == "" {
			ref = cs.GitStatus.Branch
		}
		if ref != "" {
			return fmt.Sprintf("%s (%s)", name, ref)
		}
	}
	return name
}

// targetNameForRepo returns the config target name for a repo, or empty string.
func (d *Daemon) targetNameForRepo(repo string) string {
	for name, t := range d.Cfg.TargetsSnapshot() {
		if t.Repository == repo {
			return name
		}
	}
	return ""
}

// preferencesMenuItem opens the preferences window.
func (d *Daemon) preferencesMenuItem() *fyne.MenuItem {
	return fyne.NewMenuItem("Preferences...", func() {
		go d.showPreferences()
	})
}

// rebuildTrayMenu rebuilds and replaces the system tray menu, unless
// the user looks to be mid-interaction with the menu or a recent apply
// is still cooling down. In either case the rebuild is recorded as
// pending and a timer is armed to retry once the relevant gate expires.
// Safe to call from any goroutine.
func (d *Daemon) rebuildTrayMenu() {
	if d.app == nil && d.applyTrayMenuFunc == nil {
		return
	}
	d.mu.Lock()
	wait, ok := d.gateWaitLocked()
	if !ok {
		d.pendingRebuild = true
		d.armRebuildTimerLocked(wait)
		d.mu.Unlock()
		return
	}
	d.pendingRebuild = false
	d.mu.Unlock()
	d.applyTrayMenu()
}

// rebuildTrayMenuNow forces a tray menu rebuild for data-change paths
// (poll completion, refresh, post-delete). Unlike rebuildTrayMenu it
// bypasses the interaction-window gate — once a poll detects new data,
// keeping the stale menu in front of the user just so we don't dismiss
// an open submenu is the worse trade. The cooldown still applies so
// two rapid polls (e.g. forcePollAsync queued right behind the
// scheduled tick) don't double-apply.
//
// Safe to call from any goroutine.
func (d *Daemon) rebuildTrayMenuNow() {
	if d.app == nil && d.applyTrayMenuFunc == nil {
		return
	}
	d.mu.Lock()
	if !d.lastApplyAt.IsZero() {
		if remain := trayApplyCooldown - d.now().Sub(d.lastApplyAt); remain > 0 {
			d.pendingRebuild = true
			d.armRebuildTimerLocked(remain)
			d.mu.Unlock()
			return
		}
	}
	d.pendingRebuild = false
	d.mu.Unlock()
	d.applyTrayMenu()
}

// gateWaitLocked returns (wait, ok) where ok is true when both rebuild
// gates are clear and an apply may proceed immediately. When ok is
// false, wait is the duration after which the next gate is expected to
// clear, so the caller can arm a follow-up timer. d.mu must be held.
func (d *Daemon) gateWaitLocked() (time.Duration, bool) {
	now := d.now()
	var wait time.Duration
	if !d.trayOpenedAt.IsZero() {
		if remain := trayInteractionWindow - now.Sub(d.trayOpenedAt); remain > 0 {
			if remain > wait {
				wait = remain
			}
		}
	}
	if !d.lastApplyAt.IsZero() {
		if remain := trayApplyCooldown - now.Sub(d.lastApplyAt); remain > 0 {
			if remain > wait {
				wait = remain
			}
		}
	}
	return wait, wait == 0
}

// armRebuildTimerLocked (re)arms the shared rebuild timer so it fires
// after wait elapses. If an earlier timer is still pending we leave it:
// the gate is re-checked on fire, so the worst case is one extra wakeup.
// d.mu must be held.
func (d *Daemon) armRebuildTimerLocked(wait time.Duration) {
	if wait <= 0 {
		wait = trayApplyCooldown
	}
	if d.rebuildTimer != nil {
		return
	}
	d.rebuildTimer = time.AfterFunc(wait, d.flushPendingRebuild)
}

// flushPendingRebuild is the timer callback for a deferred rebuild. It
// drops the timer reference, re-evaluates the gate, and either applies
// or re-arms the timer for a future retry. Spurious wakeups (no pending
// rebuild) are safe no-ops.
func (d *Daemon) flushPendingRebuild() {
	d.mu.Lock()
	d.rebuildTimer = nil
	if !d.pendingRebuild {
		d.mu.Unlock()
		return
	}
	wait, ok := d.gateWaitLocked()
	if !ok {
		d.armRebuildTimerLocked(wait)
		d.mu.Unlock()
		return
	}
	d.pendingRebuild = false
	d.mu.Unlock()
	d.applyTrayMenu()
}

// applyTrayMenu unconditionally replaces the tray menu on the Fyne
// main thread and records the apply time so the cooldown gate can
// observe it. Callers should normally go through rebuildTrayMenu so
// both gates are honored.
func (d *Daemon) applyTrayMenu() {
	d.mu.Lock()
	d.lastApplyAt = d.now()
	d.mu.Unlock()

	if d.applyTrayMenuFunc != nil {
		d.applyTrayMenuFunc()
		return
	}
	fyne.Do(func() {
		if desk, ok := d.app.(desktop.App); ok {
			desk.SetSystemTrayMenu(d.buildTrayMenu())
		}
	})
}

// openFile opens a file with the OS default handler.
// openFile opens path with the platform's default handler. Errors are
// surfaced in the log — a silent failure on Linux (where `open` doesn't
// exist) made the "Edit config file" button appear dead.
func openFile(path string) {
	opener := "open"
	if runtime.GOOS == "linux" {
		opener = "xdg-open"
	}
	if err := exec.Command(opener, path).Run(); err != nil {
		log.Printf("open %s: %v", path, err)
	}
}
