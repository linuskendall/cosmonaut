package daemon

import (
	"context"
	"fmt"
	"log"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/systray"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

// pollerBackstopInterval is the periodic refresh used as a fallback when
// no user interaction is happening. Event-driven refreshes (tray opened,
// app foregrounded) cover the responsive case; this just keeps state
// from going indefinitely stale when the user leaves the menu alone.
const pollerBackstopInterval = 15 * time.Minute

// pollListTimeout caps how long a single provider list call may take
// during a poll. If `gh` or `coder` hangs the timeout fires, the list
// call returns an error, and the in-flight poll slot is released so
// later forcePollAsync callers (which `cond.Wait` on the slot) don't
// leak goroutines. Declared as a var (not const) so tests can shorten
// it without waiting the full 30s.
var pollListTimeout = 30 * time.Second

func (d *Daemon) startPoller() {
	ticker := time.NewTicker(pollerBackstopInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.poll()
		case <-d.stopCh:
			return
		}
	}
}

// watchTrayOpened listens for system tray menu opens and triggers a
// debounced refresh. Wakes on macOS (NSMenu menuWillOpen:) and Linux
// (DBusMenu "opened"); does nothing on platforms where the underlying
// systray library doesn't drive the channel.
//
// TODO(systray): fyne.io/systray does not currently expose a tray-closed
// channel. When/if one is added, hook it here to reset d.trayOpenedAt
// on close and immediately flush any pending rebuild. Until then, the
// interaction-window timeout in rebuildTrayMenu's gate is the only
// signal we have that the user has finished navigating the menu.
func (d *Daemon) watchTrayOpened() {
	for {
		select {
		case <-systray.TrayOpenedCh:
			d.onTrayOpened()
		case <-d.stopCh:
			return
		}
	}
}

// onTrayOpened records the open timestamp (used as the start of the
// interaction window during which rebuildTrayMenu defers) and kicks
// off a fresh poll. It does NOT flush any pendingRebuild here: applying
// the menu as the tray opens still dismisses the user's first click on
// some platforms. Instead, rebuildTrayMenu's timer fires after the
// interaction window elapses, by which point the user is no longer
// expected to be navigating the freshly opened menu.
func (d *Daemon) onTrayOpened() {
	d.mu.Lock()
	d.trayOpenedAt = d.now()
	// If a rebuild is pending, make sure the retry timer is armed for
	// at least one full interaction window from now — otherwise an
	// earlier timer could fire mid-interaction.
	if d.pendingRebuild {
		if d.rebuildTimer != nil {
			d.rebuildTimer.Stop()
			d.rebuildTimer = nil
		}
		d.armRebuildTimerLocked(trayInteractionWindow)
	}
	d.mu.Unlock()
	d.maybePollAsync()
}

// maybePollAsync spawns a poll goroutine when a poll hasn't run in the
// last autoPollMinInterval and none is currently in flight. The check
// is advisory: two callers that pass the check concurrently can both
// spawn goroutines, but poll()'s single-flight gate (tryAcquirePoll)
// ensures only one actually runs. This wrapper just avoids the cost of
// spawning goroutines that would immediately no-op.
func (d *Daemon) maybePollAsync() {
	d.mu.Lock()
	if d.pollInFlight || time.Since(d.lastPollAt) < autoPollMinInterval {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	go d.poll()
}

// poll runs a single refresh, skipping if another poll is already in
// flight. Single-flight prevents concurrent triggers (ticker, foreground
// event, initial Run) from racing on the workspace caches.
func (d *Daemon) poll() {
	if !d.tryAcquirePoll() {
		return
	}
	defer d.releasePoll()
	d.runPoll()
}

// forcePollAsync spawns a goroutine that waits for any in-flight poll
// to finish and then runs a fresh one. Used after state-changing
// actions (e.g. delete) where the in-flight poll's data predates the
// action and would clobber the post-action state on completion. Always
// async because the wait can be long if the in-flight call hangs; the
// caller never wants to block on it.
//
// done, if non-nil, is invoked on the Fyne main thread once the poll
// completes — useful for re-enabling a Refresh button or refreshing a
// view that reads d.Workspaces() synchronously.
func (d *Daemon) forcePollAsync(done func()) {
	go func() {
		d.acquirePoll()
		defer d.releasePoll()
		d.runPoll()
		if done != nil {
			fyne.Do(done)
		}
	}()
}

// tryAcquirePoll claims the in-flight slot if free. Returns false when
// another poll already holds it.
func (d *Daemon) tryAcquirePoll() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pollInFlight {
		return false
	}
	d.pollInFlight = true
	return true
}

// acquirePoll blocks until the in-flight slot is free, then claims it.
func (d *Daemon) acquirePoll() {
	d.mu.Lock()
	for d.pollInFlight {
		d.pollCond.Wait()
	}
	d.pollInFlight = true
	d.mu.Unlock()
}

// releasePoll frees the in-flight slot and wakes any forcePoll waiters.
func (d *Daemon) releasePoll() {
	d.mu.Lock()
	d.pollInFlight = false
	d.lastPollAt = time.Now()
	d.pollCond.Broadcast()
	d.mu.Unlock()
}

// runPoll is the actual refresh body. Caller must hold the in-flight
// slot via tryAcquirePoll or acquirePoll.
func (d *Daemon) runPoll() {
	// A disabled provider is skipped outright: no CLI exec, no status
	// row, and an empty workspace slice so its stale entries drop out
	// of the tray/sidebar on the poll after it's turned off. ok stays
	// true — "disabled" is not a transient failure to paper over.
	var codespaces []codespace.Codespace
	ghWorkspaces, ghOK := []provider.Workspace(nil), true
	if d.Cfg.ProviderEnabled(provider.NameGitHub) {
		ghWorkspaces, ghOK = d.pollProvider(provider.NameGitHub, "gh", func(ctx context.Context) ([]provider.Workspace, error) {
			cs, err := codespace.ListAllCodespacesCtx(ctx, d.Runner)
			if err != nil {
				return nil, err
			}
			codespaces = cs
			return codespacesToWorkspaces(cs), nil
		})
	}

	coderWorkspaces, coderOK := []provider.Workspace(nil), true
	if d.Cfg.ProviderEnabled(provider.NameCoder) {
		coderWorkspaces, coderOK = d.pollProvider(provider.NameCoder, "coder", func(ctx context.Context) ([]provider.Workspace, error) {
			return provider.NewCoderManager(d.Cfg).ListAllWorkspacesCtx(ctx)
		})
	}

	oldCodespaces := d.Codespaces()
	oldWorkspaces := d.Workspaces()

	// On a transient provider failure, keep the previous slice for that
	// provider: one flaky list call used to empty the tray ("No
	// codespaces"), churn the workspace listeners, and then have
	// everything "reappear" on the next poll. The ProviderStatus row
	// already tells the user the provider is failing.
	if !ghOK {
		ghWorkspaces = filterWorkspacesByProvider(oldWorkspaces, provider.NameGitHub)
		codespaces = oldCodespaces
	}
	if !coderOK {
		coderWorkspaces = filterWorkspacesByProvider(oldWorkspaces, provider.NameCoder)
	}

	workspaces := append(ghWorkspaces, coderWorkspaces...)

	log.Printf("poll: fetched %d github codespaces and %d total workspaces", len(codespaces), len(workspaces))

	d.SetCodespaces(codespaces)
	d.SetWorkspaces(workspaces)

	if len(oldCodespaces) > 0 {
		d.detectStateChanges(oldCodespaces, codespaces)
	}
	d.checkAutoStop(codespaces)
	d.updateTrayIcon(workspaces)

	// Data-change rebuild: the interaction-window gate exists so an
	// open submenu isn't dismissed by a routine rebuild, but when the
	// poll has actually changed the menu's contents (new workspace,
	// state transition, etc.) the user is better served by the fresh
	// menu than by 30 more seconds of stale data. When the data is
	// identical, fall through to the gated path — same end state, but
	// the gate stays in place for any unrelated config-driven rebuild.
	if workspacesDiffer(oldWorkspaces, workspaces) {
		d.rebuildTrayMenuNow()
		d.notifyWorkspaceListeners()
	} else {
		d.rebuildTrayMenu()
	}
}

// workspacesDiffer reports whether two workspace lists differ in any
// field the tray menu or GUI sidebar renders. Order-insensitive: only
// the multiset of (provider, name, state, displayName, repo, branch)
// tuples matters. Used by runPoll to skip the expensive rebuild path
// when a poll returned the same data we already had.
func workspacesDiffer(a, b []provider.Workspace) bool {
	if len(a) != len(b) {
		return true
	}
	type key struct {
		provider, name, state, displayName, repo, branch string
	}
	tally := make(map[key]int, len(a))
	for _, ws := range a {
		tally[key{ws.Provider, ws.Name, ws.State, ws.DisplayName, ws.Repository, ws.Branch}]++
	}
	for _, ws := range b {
		k := key{ws.Provider, ws.Name, ws.State, ws.DisplayName, ws.Repository, ws.Branch}
		if tally[k] == 0 {
			return true
		}
		tally[k]--
	}
	return false
}

// pollProvider does the CLI presence check + list call for one
// provider, records the resulting ProviderStatus, and propagates
// listErr when the provider is the effective default. Returns the
// listed workspaces, or nil on failure.
//
// The list call is capped at pollListTimeout so that a hung `gh` or
// `coder` process can't pin the in-flight poll slot indefinitely; on
// timeout the context error surfaces as a regular list error and the
// ProviderStatus banner explains.
// The boolean result distinguishes "listed successfully" from "provider
// failed" so runPoll can keep the previous cached slice on a transient
// failure instead of flashing the tray empty.
func (d *Daemon) pollProvider(name, cli string, list func(ctx context.Context) ([]provider.Workspace, error)) ([]provider.Workspace, bool) {
	if err := provider.RequireCommand(cli); err != nil {
		log.Printf("poll(%s): %v", name, err)
		d.setProviderStatus(name, ProviderStatus{Available: false, Err: err})
		d.updateEffectiveListErr(name, err)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), pollListTimeout)
	defer cancel()
	workspaces, err := list(ctx)
	if err != nil {
		log.Printf("poll(%s): %v", name, err)
	}
	d.setProviderStatus(name, ProviderStatus{Available: true, Err: err})
	d.updateEffectiveListErr(name, err)
	if err != nil {
		return nil, false
	}
	return workspaces, true
}

// updateEffectiveListErr writes listErr only when the named provider
// is the user's effective default — otherwise the shared listErr would
// reflect a non-default provider's errors and confuse banner UI.
func (d *Daemon) updateEffectiveListErr(providerName string, err error) {
	effective := provider.NameGitHub
	if d.Cfg != nil {
		effective = d.Cfg.EffectiveWorkspaceProvider()
	}
	if effective != providerName {
		return
	}
	d.SetListErr(err)
}

func codespacesToWorkspaces(items []codespace.Codespace) []provider.Workspace {
	out := make([]provider.Workspace, 0, len(items))
	for _, cs := range items {
		ws := provider.Workspace{
			Provider:    provider.NameGitHub,
			Name:        cs.Name,
			DisplayName: cs.DisplayName,
			Repository:  string(cs.Repository),
			State:       cs.State,
			MachineName: cs.MachineName,
			CreatedAt:   cs.CreatedAt,
			LastUsedAt:  cs.LastUsedAt,
		}
		if cs.GitStatus != nil {
			ws.Branch = cs.GitStatus.Ref
			if ws.Branch == "" {
				ws.Branch = cs.GitStatus.Branch
			}
		}
		out = append(out, ws)
	}
	return out
}

func (d *Daemon) refreshCoderWorkspacesAsync(done func()) {
	go func() {
		// Hold the poll slot: this read-modify-write of the workspace list
		// otherwise races a concurrent runPoll — a stale snapshot taken
		// here could clobber the poll's fresh GitHub data (or resurrect a
		// just-deleted workspace) until the next 15-minute backstop poll.
		d.acquirePoll()
		defer d.releasePoll()

		manager := provider.NewCoderManager(d.Cfg)
		workspaces, err := manager.ListAllWorkspaces()
		oldWorkspaces := d.Workspaces()
		if err != nil {
			log.Printf("refresh(coder): %v", err)
			if d.Cfg != nil && d.Cfg.EffectiveWorkspaceProvider() == provider.NameCoder {
				d.SetListErr(err)
			}
			d.notify(fmt.Sprintf("Refreshing Coder workspaces failed: %v", err))
		} else {
			d.SetWorkspaces(replaceWorkspacesByProvider(oldWorkspaces, provider.NameCoder, workspaces))
			if d.Cfg != nil && d.Cfg.EffectiveWorkspaceProvider() == provider.NameCoder {
				d.SetListErr(nil)
			}
			d.notify(fmt.Sprintf("Refreshed %d Coder workspace(s)", len(workspaces)))
		}
		d.updateTrayIcon(d.Workspaces())
		if workspacesDiffer(oldWorkspaces, d.Workspaces()) {
			d.rebuildTrayMenuNow()
			d.notifyWorkspaceListeners()
		} else {
			d.rebuildTrayMenu()
		}
		if done != nil {
			fyne.Do(done)
		}
	}()
}

func replaceWorkspacesByProvider(current []provider.Workspace, providerName string, replacement []provider.Workspace) []provider.Workspace {
	result := make([]provider.Workspace, 0, len(current)+len(replacement))
	for _, ws := range current {
		if ws.Provider != providerName {
			result = append(result, ws)
		}
	}
	result = append(result, replacement...)
	return result
}

// updateTrayIcon switches tray icon based on aggregate workspace state.
func (d *Daemon) updateTrayIcon(workspaces []provider.Workspace) {
	hasAvailable := false
	hasStarting := false
	for _, ws := range workspaces {
		switch ws.State {
		case "Available", "ready", "running", "connected":
			hasAvailable = true
		case "Starting", "starting", "pending":
			hasStarting = true
		}
	}

	fyne.Do(func() {
		desk, ok := d.app.(desktop.App)
		if !ok {
			return
		}
		switch {
		case hasStarting:
			desk.SetSystemTrayIcon(trayIconStarting())
		case hasAvailable:
			desk.SetSystemTrayIcon(trayIconActive())
		default:
			desk.SetSystemTrayIcon(trayIconIdle())
		}
	})
}
