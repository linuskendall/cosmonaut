package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/daemon"
	"github.com/linuskendall/cosmonaut/internal/doctor"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

// ProviderStatus mirrors daemon.ProviderStatus inside the TUI package so the
// list/detail/settings views don't have to import daemon directly.
type ProviderStatus struct {
	Available bool
	Err       error
	CheckedAt time.Time
}

// PortCacheEntry mirrors the daemon's port cache shape: a per-codespace
// snapshot used to render the ports section without re-fetching on every
// repaint.
type PortCacheEntry struct {
	Ports     []codespace.Port
	Err       error
	CheckedAt time.Time
	Loading   bool
}

// AppletData owns the TUI applet's mutable state — workspace lists, provider
// health, port snapshots, and the long-lived port-forward supervisor. It's
// the TUI-side equivalent of daemon.Daemon, minus the Fyne pieces (windows,
// tray, hotkey listener) which a terminal doesn't need.
//
// All exported methods are safe for concurrent use; the TUI's tea.Cmd
// callbacks read state from background goroutines while the foreground
// model is rendering.
type AppletData struct {
	cfg     *config.Config
	cfgPath string

	mu             sync.Mutex
	codespaces     []codespace.Codespace
	workspaces     []provider.Workspace
	providerStatus map[string]ProviderStatus
	listErr        error
	ports          map[string]PortCacheEntry
	lastPollAt     time.Time
	pollInFlight   bool

	forwards *daemon.PortForwardManager
}

// NewAppletData wires the long-lived TUI state. Pass the parsed config and
// the absolute path it was loaded from so per-workspace SSH toggles can be
// persisted.
func NewAppletData(cfg *config.Config, cfgPath string) *AppletData {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &AppletData{
		cfg:      cfg,
		cfgPath:  cfgPath,
		ports:    map[string]PortCacheEntry{},
		forwards: daemon.NewPortForwardManager(),
	}
}

// Config returns the live config pointer. Callers may mutate it (e.g. to
// toggle a per-workspace SSH option) and then call PersistConfig.
func (d *AppletData) Config() *config.Config { return d.cfg }

// ConfigPath returns the config file path used by PersistConfig.
func (d *AppletData) ConfigPath() string { return d.cfgPath }

// PortForwards returns the shared supervisor used to start/stop forwards.
func (d *AppletData) PortForwards() *daemon.PortForwardManager { return d.forwards }

// Codespaces returns a defensive copy of the last-polled codespace list.
func (d *AppletData) Codespaces() []codespace.Codespace {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]codespace.Codespace, len(d.codespaces))
	copy(out, d.codespaces)
	return out
}

// Workspaces returns a defensive copy of the last-polled combined workspace
// list (GitHub codespaces + Coder workspaces, both translated to the
// unified provider.Workspace shape).
func (d *AppletData) Workspaces() []provider.Workspace {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]provider.Workspace, len(d.workspaces))
	copy(out, d.workspaces)
	return out
}

// ListErr returns the most recent provider list error, or nil.
func (d *AppletData) ListErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listErr
}

// StatusFor returns a snapshot of the named provider's local-setup health.
func (d *AppletData) StatusFor(name string) ProviderStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.providerStatus[name]
}

// HealthCatalog builds the doctor catalog the settings Health section
// renders. Like the GUI, it includes every provider's auth check wired
// to that provider's own cached error, so a Coder login problem and a
// GitHub scope problem surface independently rather than only the
// effective provider's.
func (d *AppletData) HealthCatalog() []doctor.Check {
	var providers []doctor.ProviderListErr
	for _, name := range []string{provider.NameGitHub, provider.NameCoder} {
		if !d.cfg.ProviderEnabled(name) {
			continue
		}
		providers = append(providers, doctor.ProviderListErr{
			Name: name, ListErr: d.providerListErr(name),
		})
	}
	return doctor.CatalogForProviders(providers...)
}

// providerListErr returns a supplier of the named provider's most recent
// cached list error, backed by the per-provider status snapshot.
func (d *AppletData) providerListErr(name string) func() error {
	return func() error { return d.StatusFor(name).Err }
}

// PortCache returns a snapshot of the ports section for the named
// codespace. Loading is true while a refresh is in flight.
func (d *AppletData) PortCache(name string) PortCacheEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ports[name]
}

// PersistConfig writes the in-memory config back to disk. Returns nil if no
// path was configured (which can happen in tests).
func (d *AppletData) PersistConfig() error {
	if d.cfgPath == "" {
		return nil
	}
	return config.SaveConfig(d.cfgPath, d.cfg)
}

// PollResult is sent back by Poll as a tea.Msg so the foreground model can
// re-render when fresh data lands. Workspaces is always set; ListErr is
// non-nil when the provider call failed end-to-end.
type PollResult struct {
	Workspaces []provider.Workspace
	ListErr    error
}

// Poll fetches the workspace list from every configured provider, refreshes
// per-provider status, and returns a single combined slice. Safe to call
// from a goroutine; concurrent calls collapse to one in-flight at a time.
func (d *AppletData) Poll() PollResult {
	d.mu.Lock()
	if d.pollInFlight {
		// Another caller is already polling. Return the cached snapshot so
		// the UI can keep rendering rather than blocking on the slot.
		// listErr must be read while still holding the lock — the in-flight
		// poll writes it concurrently.
		listErr := d.listErr
		workspaces := append([]provider.Workspace(nil), d.workspaces...)
		d.mu.Unlock()
		return PollResult{
			Workspaces: workspaces,
			ListErr:    listErr,
		}
	}
	d.pollInFlight = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.pollInFlight = false
		d.lastPollAt = time.Now()
		d.mu.Unlock()
	}()

	var combined []provider.Workspace
	var firstErr error

	if d.cfg.ProviderEnabled(provider.NameGitHub) && provider.IsGitHubAvailable() {
		ws, cs, err := d.pollGitHub()
		d.setProviderStatus(provider.NameGitHub, "gh", err)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		d.mu.Lock()
		d.codespaces = cs
		d.mu.Unlock()
		combined = append(combined, ws...)
	}
	if d.cfg.ProviderEnabled(provider.NameCoder) && provider.IsCoderAvailable() {
		ws, err := d.pollCoder()
		d.setProviderStatus(provider.NameCoder, "coder", err)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		combined = append(combined, ws...)
	}

	d.mu.Lock()
	d.workspaces = combined
	d.listErr = firstErr
	d.mu.Unlock()
	return PollResult{Workspaces: combined, ListErr: firstErr}
}

func (d *AppletData) pollGitHub() ([]provider.Workspace, []codespace.Codespace, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, nil, fmt.Errorf("gh CLI not installed")
	}
	mgr := provider.NewGitHubManager(codespace.DefaultGHRunner{})
	cs, err := codespace.ListAllCodespaces(mgr.Runner)
	if err != nil {
		return nil, nil, err
	}
	ws := make([]provider.Workspace, 0, len(cs))
	for _, c := range cs {
		ws = append(ws, codespaceToWorkspace(c))
	}
	return ws, cs, nil
}

func (d *AppletData) pollCoder() ([]provider.Workspace, error) {
	if _, err := exec.LookPath("coder"); err != nil {
		return nil, fmt.Errorf("coder CLI not installed")
	}
	mgr := provider.NewCoderManager(d.cfg)
	return mgr.ListAllWorkspaces()
}

func (d *AppletData) setProviderStatus(name, cli string, err error) {
	available := true
	if _, lookErr := exec.LookPath(cli); lookErr != nil {
		available = false
		err = errors.Join(err, fmt.Errorf("%s not on PATH", cli))
	}
	d.mu.Lock()
	if d.providerStatus == nil {
		d.providerStatus = map[string]ProviderStatus{}
	}
	d.providerStatus[name] = ProviderStatus{
		Available: available,
		Err:       err,
		CheckedAt: time.Now(),
	}
	d.mu.Unlock()
}

// RefreshPorts fetches the ports section for a codespace and stores the
// result in the cache. Marks Loading=true for the duration so the detail
// view can render a spinner placeholder.
func (d *AppletData) RefreshPorts(csName string) PortCacheEntry {
	d.mu.Lock()
	entry := d.ports[csName]
	entry.Loading = true
	d.ports[csName] = entry
	d.mu.Unlock()

	runner := codespace.DefaultGHRunner{}
	ports, err := codespace.ListPorts(runner, csName)
	entry = PortCacheEntry{
		Ports:     ports,
		Err:       err,
		CheckedAt: time.Now(),
		Loading:   false,
	}
	d.mu.Lock()
	d.ports[csName] = entry
	d.mu.Unlock()
	return entry
}

// EnsurePortsCache returns the cached ports entry for a codespace, kicking
// off ONE async refresh when there is no cached entry yet. An entry that is
// already Loading is returned as-is — every repaint used to stack another
// `gh codespace ports` subprocess onto the in-flight one. The Loading
// placeholder is written under the lock before the goroutine starts so
// concurrent callers deduplicate too.
func (d *AppletData) EnsurePortsCache(csName string, onReady func()) PortCacheEntry {
	entry, _ := d.ensurePortsCache(csName, onReady)
	return entry
}

// ensurePortsCache additionally reports whether THIS call started the
// fetch — only then will onReady eventually fire, so callers that wait on
// it must check the flag.
func (d *AppletData) ensurePortsCache(csName string, onReady func()) (PortCacheEntry, bool) {
	d.mu.Lock()
	if entry, ok := d.ports[csName]; ok {
		d.mu.Unlock()
		return entry, false
	}
	loading := PortCacheEntry{Loading: true}
	d.ports[csName] = loading
	d.mu.Unlock()
	go func() {
		d.RefreshPorts(csName)
		if onReady != nil {
			onReady()
		}
	}()
	return loading, true
}

// codespaceToWorkspace translates the GitHub-specific codespace shape into
// the unified provider.Workspace used by the list/detail views.
func codespaceToWorkspace(cs codespace.Codespace) provider.Workspace {
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
	return ws
}

// DeleteWorkspace deletes a workspace through the right provider manager.
func (d *AppletData) DeleteWorkspace(providerName, name string) error {
	mgr, err := d.managerForProvider(providerName)
	if err != nil {
		return err
	}
	return mgr.DeleteWorkspace(name)
}

// ResolveWorkspace finds a workspace by name through the right provider.
func (d *AppletData) ResolveWorkspace(providerName, name string) (*provider.Workspace, error) {
	mgr, err := d.managerForProvider(providerName)
	if err != nil {
		return nil, err
	}
	return mgr.ResolveWorkspace(name)
}

// ManagerForProvider returns a provider.Manager for the named provider.
func (d *AppletData) ManagerForProvider(providerName string) (provider.Manager, error) {
	return d.managerForProvider(providerName)
}

func (d *AppletData) managerForProvider(providerName string) (provider.Manager, error) {
	switch providerName {
	case provider.NameGitHub, "":
		return provider.NewGitHubManager(codespace.DefaultGHRunner{}), nil
	case provider.NameCoder:
		return provider.NewCoderManager(d.cfg), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}
}

// canDeleteReason returns a short string describing why Delete is currently
// disabled, or "" if it's available. Mirrors the GUI's
// deleteDisabledReason so the two surfaces show the same message.
func (d *AppletData) canDeleteReason(providerName string) string {
	st := d.StatusFor(providerName)
	if !st.Available {
		switch providerName {
		case provider.NameGitHub:
			return "gh CLI not installed"
		case provider.NameCoder:
			return "Coder CLI not installed"
		}
		return "CLI not installed"
	}
	if st.Err != nil {
		msg := strings.ToLower(st.Err.Error())
		switch {
		case strings.Contains(msg, "not authenticated"),
			strings.Contains(msg, "auth login"),
			strings.Contains(msg, "coder login"),
			strings.Contains(msg, "unauthorized"):
			return "not authenticated"
		case strings.Contains(msg, "scope"):
			return "missing token scope"
		}
	}
	return ""
}
