// Package config loads the cosmonaut JSONC configuration file
// and defines the Target struct that describes a named codespace target
// (repository, branch, machine type, Zed display settings, etc.).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tailscale/hujson"

	"github.com/linuskendall/cosmonaut/internal/fileutil"
)

type Config struct {
	// mu guards every field below. Reads take RLock, writes take Lock.
	// The GUI/TUI mutate Config from goroutines other than the one that
	// reads it during a launch or tray rebuild, so all access has to be
	// serialized.
	//
	// Embedding sync.RWMutex on Config means Config must always be used
	// via a pointer — copying it would copy the mutex (which `go vet`
	// flags as a copylock violation).
	mu sync.RWMutex

	DefaultTarget     string            `json:"defaultTarget,omitempty"`
	WorkspaceProvider string            `json:"workspaceProvider,omitempty"` // "github" (default) or "coder"
	Editor            string            `json:"editor,omitempty"`            // any binary on PATH; "" / "zed" / "zeditor" use the built-in Zed integration
	Providers         ProviderConfigs   `json:"providers,omitempty"`
	Targets           map[string]Target `json:"targets"`
	Daemon            *DaemonConfig     `json:"daemon,omitempty"`

	// SSH holds SSH defaults that apply to every workspace unless a
	// per-workspace WorkspaceSSH entry overrides them. Workspace names are
	// often ephemeral (Coder workspaces come and go), so a declarative
	// config wants one global knob rather than per-name entries.
	SSH *SSHConfig `json:"ssh,omitempty"`

	// WorkspaceSSH holds per-workspace SSH options keyed by "<provider>:<name>"
	// (e.g. "github:cs-abc" or "coder:my-ws"). Unset workspaces fall back to
	// the SSH defaults above, then to the built-ins: ControlMaster on, no
	// multiplexer.
	WorkspaceSSH map[string]WorkspaceSSHSettings `json:"workspaceSsh,omitempty"`
}

// Terminal multiplexer choices for the remote shell. The multiplexer keeps
// the remote session alive across SSH drops; reconnecting re-attaches to it.
const (
	MultiplexerNone   = "none"
	MultiplexerTmux   = "tmux"
	MultiplexerZellij = "zellij"
)

// Multiplexers lists the valid multiplexer values in UI presentation order.
var Multiplexers = []string{MultiplexerNone, MultiplexerTmux, MultiplexerZellij}

// ValidMultiplexer reports whether s names a known multiplexer setting.
func ValidMultiplexer(s string) bool {
	switch s {
	case MultiplexerNone, MultiplexerTmux, MultiplexerZellij:
		return true
	}
	return false
}

// SSHConfig holds global SSH defaults, overridable per workspace via
// WorkspaceSSH.
type SSHConfig struct {
	// Multiplexer wraps `cosmonaut shell` (and the GUI/TUI SSH buttons)
	// in a persistent terminal multiplexer session on the remote:
	// "tmux" (`tmux new -A -s cosmonaut`), "zellij"
	// (`zellij attach --create cosmonaut`), or "none". Default: none.
	Multiplexer string `json:"multiplexer,omitempty"`
}

// WorkspaceSSHSettings stores per-workspace SSH knobs. Each field is a pointer
// so "unset" can be distinguished from an explicit on/off.
type WorkspaceSSHSettings struct {
	// ControlMaster enables OpenSSH connection multiplexing
	// (ControlMaster auto + ControlPersist) in the managed extras block,
	// so additional sessions to the same workspace reuse the existing TCP
	// connection. Default: true.
	ControlMaster *bool `json:"controlMaster,omitempty"`
	// Multiplexer overrides the global SSH multiplexer for this
	// workspace: "none", "tmux", or "zellij". See SSHConfig.Multiplexer.
	Multiplexer *string `json:"multiplexer,omitempty"`
	// Tmux is the legacy boolean form of Multiplexer (true == "tmux").
	// Still parsed so existing configs keep working; Multiplexer wins
	// when both are set, and writes clear this field.
	Tmux *bool `json:"tmux,omitempty"`
}

// WorkspaceSSHKey returns the canonical map key used by Config.WorkspaceSSH
// for a workspace. Stable across renames since both provider and the
// provider-issued name are immutable.
func WorkspaceSSHKey(provider, name string) string {
	return provider + ":" + name
}

// WorkspaceSSHControlMaster returns the resolved ControlMaster setting for a
// workspace, with the default (true) applied when no explicit value is set.
func (c *Config) WorkspaceSSHControlMaster(provider, name string) bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.WorkspaceSSH[WorkspaceSSHKey(provider, name)]; ok && s.ControlMaster != nil {
		return *s.ControlMaster
	}
	return true
}

// WorkspaceSSHMultiplexer returns the resolved multiplexer setting for a
// workspace. Resolution order: the workspace's Multiplexer, its legacy Tmux
// boolean, the global SSH default, then "none". Values outside the known
// set are treated as unset.
func (c *Config) WorkspaceSSHMultiplexer(provider, name string) string {
	if c == nil {
		return MultiplexerNone
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.WorkspaceSSH[WorkspaceSSHKey(provider, name)]; ok {
		if s.Multiplexer != nil && ValidMultiplexer(*s.Multiplexer) {
			return *s.Multiplexer
		}
		if s.Tmux != nil {
			if *s.Tmux {
				return MultiplexerTmux
			}
			return MultiplexerNone
		}
	}
	if c.SSH != nil && ValidMultiplexer(c.SSH.Multiplexer) {
		return c.SSH.Multiplexer
	}
	return MultiplexerNone
}

// SetWorkspaceSSHControlMaster persists an explicit ControlMaster setting for
// a workspace. Passing nil clears it (so the default applies).
func (c *Config) SetWorkspaceSSHControlMaster(provider, name string, val *bool) {
	c.setWorkspaceSSH(provider, name, func(s *WorkspaceSSHSettings) { s.ControlMaster = val })
}

// SetWorkspaceSSHMultiplexer persists an explicit multiplexer setting for a
// workspace. Passing nil clears it (so the global default applies). The
// legacy Tmux boolean is cleared either way — it must not shadow the new
// field, and once the user touches the setting the config is migrated.
func (c *Config) SetWorkspaceSSHMultiplexer(provider, name string, val *string) {
	c.setWorkspaceSSH(provider, name, func(s *WorkspaceSSHSettings) {
		s.Multiplexer = val
		s.Tmux = nil
	})
}

func (c *Config) setWorkspaceSSH(provider, name string, mut func(*WorkspaceSSHSettings)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := WorkspaceSSHKey(provider, name)
	if c.WorkspaceSSH == nil {
		c.WorkspaceSSH = map[string]WorkspaceSSHSettings{}
	}
	s := c.WorkspaceSSH[key]
	mut(&s)
	if s.ControlMaster == nil && s.Tmux == nil && s.Multiplexer == nil {
		delete(c.WorkspaceSSH, key)
		if len(c.WorkspaceSSH) == 0 {
			c.WorkspaceSSH = nil
		}
		return
	}
	c.WorkspaceSSH[key] = s
}

// GetEditor returns the configured editor name.
func (c *Config) GetEditor() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Editor
}

// SetEditor persists the editor name. Callers should follow up with
// SaveConfig to flush to disk.
func (c *Config) SetEditor(editor string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Editor = editor
}

// WithEditor swaps in a temporary editor for the duration of fn (typically a
// launch flow whose editor override should not leak to the persistent config)
// and restores the prior value when fn returns. Holds the write lock for the
// whole window so no concurrent reader observes a half-swapped state.
func (c *Config) WithEditor(editor string, fn func()) {
	if c == nil {
		fn()
		return
	}
	c.mu.Lock()
	prev := c.Editor
	c.Editor = editor
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.Editor = prev
		c.mu.Unlock()
	}()
	fn()
}

// EnsureDaemon initialises Config.Daemon to a zero-value DaemonConfig if it's
// nil, and returns a snapshot of the current daemon settings under the read
// lock. The returned struct is a copy: mutating it does not affect the live
// config — use the SetDaemon* helpers for that.
func (c *Config) EnsureDaemon() DaemonConfig {
	if c == nil {
		return DaemonConfig{}
	}
	c.mu.Lock()
	if c.Daemon == nil {
		c.Daemon = &DaemonConfig{}
	}
	snap := *c.Daemon
	c.mu.Unlock()
	return snap
}

// SetDaemonHotkey persists the daemon Hotkey field. Daemon is auto-created
// if nil. An empty string falls back to the platform default at register time.
func (c *Config) SetDaemonHotkey(hotkey string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Daemon == nil {
		c.Daemon = &DaemonConfig{}
	}
	c.Daemon.Hotkey = hotkey
}

// SetDaemonInhibitSleep persists the daemon InhibitSleep field. Daemon is
// auto-created if nil.
func (c *Config) SetDaemonInhibitSleep(mode string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Daemon == nil {
		c.Daemon = &DaemonConfig{}
	}
	c.Daemon.InhibitSleep = mode
}

// Target returns a deep copy of the named target, or the zero value with
// ok=false when no such target is configured. The copy shares no pointers
// with the live config, so callers may freely mutate it (e.g. to stage a
// launch override) without racing concurrent readers or leaking the change
// back into the config.
func (c *Config) Target(name string) (Target, bool) {
	if c == nil {
		return Target{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.Targets[name]
	return t.Clone(), ok
}

// GetDefaultTarget returns the DefaultTarget name under the read lock.
func (c *Config) GetDefaultTarget() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DefaultTarget
}

// ProviderEnabled reports whether the named provider ("github" or
// "coder"; the config package can't import provider's Name constants
// without a cycle) is enabled. Unset means enabled, so existing configs
// keep today's both-providers behavior; unknown names report false.
func (c *Config) ProviderEnabled(name string) bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.providerEnabledLocked(name)
}

// providerEnabledLocked is the lock-free body of ProviderEnabled,
// callable by methods that already hold c.mu.
func (c *Config) providerEnabledLocked(name string) bool {
	var enabled *bool
	switch name {
	case "github":
		enabled = c.Providers.GitHub.Enabled
	case "coder":
		enabled = c.Providers.Coder.Enabled
	default:
		return false
	}
	return enabled == nil || *enabled
}

// SetProviderEnabled persists an explicit enabled/disabled state for a
// provider. Passing nil clears it (so the default — enabled — applies).
// Unknown provider names are ignored.
func (c *Config) SetProviderEnabled(name string, val *bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch name {
	case "github":
		c.Providers.GitHub.Enabled = val
	case "coder":
		c.Providers.Coder.Enabled = val
	}
}

// CoderOrganization returns Providers.Coder.Organization under the read lock.
func (c *Config) CoderOrganization() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Providers.Coder.Organization
}

// TargetsSnapshot returns a deep copy of the Targets map taken under the
// read lock. Safe to iterate or mutate without further locking; mutations
// do not affect the live config.
func (c *Config) TargetsSnapshot() map[string]Target {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Targets == nil {
		return nil
	}
	snap := make(map[string]Target, len(c.Targets))
	for name, t := range c.Targets {
		snap[name] = t.Clone()
	}
	return snap
}

// SetTarget writes a deep copy of t into the Targets map, so later caller
// mutations of t (or its Coder sub-struct) can't reach into the live config.
// The Targets map is auto-created if nil.
func (c *Config) SetTarget(name string, t Target) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Targets == nil {
		c.Targets = map[string]Target{}
	}
	c.Targets[name] = t.Clone()
}

// UpdateTarget performs a read-modify-write on the named target atomically.
// The callback receives a deep copy of the target (so mutating nested
// pointers like Coder never aliases the live config) plus whether the target
// already existed; whatever the callback leaves in *t is written back. The
// Targets map is auto-created if nil.
func (c *Config) UpdateTarget(name string, fn func(t *Target, exists bool)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Targets == nil {
		c.Targets = map[string]Target{}
	}
	cur, ok := c.Targets[name]
	t := cur.Clone()
	fn(&t, ok)
	// Clone again on the way back in: the callback may have replaced *t
	// wholesale with a value that shares pointers with caller-held state.
	c.Targets[name] = t.Clone()
}

type ProviderConfigs struct {
	GitHub GitHubProviderConfig `json:"github,omitempty"`
	Coder  CoderProviderConfig  `json:"coder,omitempty"`
}

type GitHubProviderConfig struct {
	// Enabled turns the GitHub Codespaces provider on or off across the
	// whole app: polling, tray section, GUI sidebar, auth prompts, and
	// health checks. nil means enabled (the historical behavior), so a
	// user who only works with Coder can set it to false and never see a
	// "fix GitHub sign-in" nag for a token that lacks the codespace
	// scope.
	Enabled *bool `json:"enabled,omitempty"`
}

type CoderProviderConfig struct {
	// Enabled mirrors GitHubProviderConfig.Enabled for the Coder
	// provider. nil means enabled.
	Enabled      *bool  `json:"enabled,omitempty"`
	Organization string `json:"organization,omitempty"`
}

// DaemonConfig holds settings for the background daemon (tray, hotkey, poller).
type DaemonConfig struct {
	Hotkey       string `json:"hotkey,omitempty"`       // e.g. "Cmd+Shift+S" (macOS) or "Ctrl+Shift+S" (Linux)
	Terminal     string `json:"terminal,omitempty"`     // terminal app to launch picker in; "auto" to detect
	InhibitSleep string `json:"inhibitSleep,omitempty"` // "off" (default), "sleep", or "sleep+shutdown"
}

type Target struct {
	Repository          string             `json:"repository,omitempty"`
	Branch              string             `json:"branch,omitempty"`
	DisplayName         string             `json:"displayName,omitempty"`
	CodespaceName       string             `json:"codespaceName,omitempty"`
	WorkspacePath       string             `json:"workspacePath"`
	Machine             string             `json:"machine,omitempty"`
	Location            string             `json:"location,omitempty"`
	DevcontainerPath    string             `json:"devcontainerPath,omitempty"`
	IdleTimeout         string             `json:"idleTimeout,omitempty"`
	RetentionPeriod     string             `json:"retentionPeriod,omitempty"`
	UploadBinaryOverSSH *bool              `json:"uploadBinaryOverSsh,omitempty"`
	ZedNickname         string             `json:"zedNickname,omitempty"`
	AutoStop            string             `json:"autoStop,omitempty"` // auto-stop after idle duration (e.g. "30m")
	PreWarm             string             `json:"preWarm,omitempty"`  // time-of-day to pre-warm codespace (e.g. "08:00")
	Coder               *CoderTargetConfig `json:"coder,omitempty"`
}

// Clone returns a deep copy of the target: the Coder sub-struct (with its
// Parameters map and PortForwards slice) and the UploadBinaryOverSSH pointer
// are duplicated, so mutating the copy never writes through to the original.
func (t Target) Clone() Target {
	if t.UploadBinaryOverSSH != nil {
		v := *t.UploadBinaryOverSSH
		t.UploadBinaryOverSSH = &v
	}
	if t.Coder != nil {
		coder := *t.Coder
		if coder.Parameters != nil {
			params := make(map[string]string, len(coder.Parameters))
			for k, v := range coder.Parameters {
				params[k] = v
			}
			coder.Parameters = params
		}
		if coder.PortForwards != nil {
			coder.PortForwards = append([]PortForward(nil), coder.PortForwards...)
		}
		t.Coder = &coder
	}
	return t
}

type CoderTargetConfig struct {
	Template      string            `json:"template,omitempty"`
	WorkspaceName string            `json:"workspaceName,omitempty"`
	Parameters    map[string]string `json:"parameters,omitempty"`
	StopAfter     string            `json:"stopAfter,omitempty"`
	Organization  string            `json:"organization,omitempty"`
	PortForwards  []PortForward     `json:"portForwards,omitempty"`
}

type PortForward struct {
	Label      string `json:"label,omitempty"`
	LocalPort  int    `json:"localPort,omitempty"`
	RemotePort int    `json:"remotePort,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
}

// ParseJSONC strips comments and trailing commas, then returns clean JSON
// bytes. The conversion uses a real JWCC parser (tailscale/hujson), not
// regexes: comment markers or ",}" sequences inside string values — shell
// snippets in coder.parameters, glob patterns — pass through untouched
// instead of silently corrupting the config, and malformed input is
// reported as an error instead of producing garbage.
func ParseJSONC(source string) ([]byte, error) {
	return hujson.Standardize([]byte(source))
}

// LoadConfig reads a JSONC config file and returns the parsed Config.
//
// LoadConfig returns a freshly-allocated *Config that no other goroutine has
// a reference to yet, so it doesn't need to hold the mutex while populating
// it.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	clean, err := ParseJSONC(string(data))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if cfg.WorkspaceProvider == "" {
		cfg.WorkspaceProvider = cfg.EffectiveWorkspaceProvider()
	}

	return &cfg, nil
}

// SaveConfig writes the config to the given path as formatted JSON
// with 4-space indentation for easy hand-editing.
//
// Takes the write lock for the duration of the marshal so a concurrent
// writer can't mutate the struct mid-serialization. A nil config is an
// error: marshaling nil would write the literal `null` over the user's
// config file.
func SaveConfig(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("refusing to save nil config to %s", path)
	}
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	data = append(data, '\n')
	return fileutil.WriteFileAtomic(path, data, 0o644)
}

func (c *Config) EffectiveWorkspaceProvider() string {
	if c == nil {
		return "github"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.effectiveWorkspaceProviderLocked()
}

// effectiveWorkspaceProviderLocked is the lock-free body of
// EffectiveWorkspaceProvider, callable by other methods that already hold
// c.mu (read or write). When no provider is chosen explicitly the
// default is github — unless github is disabled and coder isn't, in
// which case a coder-only config works without also having to set
// workspaceProvider.
func (c *Config) effectiveWorkspaceProviderLocked() string {
	if c == nil || c.WorkspaceProvider == "" {
		if c != nil && !c.providerEnabledLocked("github") && c.providerEnabledLocked("coder") {
			return "coder"
		}
		return "github"
	}
	return c.WorkspaceProvider
}

func (t Target) ExplicitWorkspaceName(provider string) string {
	// NOTE: cannot import provider package due to import cycle
	// (provider imports config). Keep this literal in sync with
	// provider.NameCoder.
	if provider == "coder" {
		if t.Coder != nil && t.Coder.WorkspaceName != "" {
			return t.Coder.WorkspaceName
		}
		return ""
	}
	if t.CodespaceName != "" {
		return t.CodespaceName
	}
	return ""
}

// FieldDoc describes a single config target field for generated documentation.
type FieldDoc struct {
	JSON     string // JSON key name
	Type     string // human-readable type
	Required bool
	Desc     string
}

// TargetFieldDocs is the authoritative documentation for every Target field.
var TargetFieldDocs = []FieldDoc{
	{"repository", "string", false, "GitHub repository in owner/repo form; optional for Coder targets"},
	{"branch", "string", false, "Preferred branch when creating or matching a codespace"},
	{"displayName", "string", false, "Exact display name to disambiguate codespace matches"},
	{"codespaceName", "string", false, "Exact codespace name for strict reuse"},
	{"workspacePath", "string", true, "Remote folder Zed should open (e.g. /workspaces/repo)"},
	{"machine", "string", false, "Machine type forwarded to gh codespace create"},
	{"location", "string", false, "Location forwarded to gh codespace create"},
	{"devcontainerPath", "string", false, "Dev container config path forwarded to gh codespace create"},
	{"idleTimeout", "string", false, "Idle timeout forwarded to gh codespace create (e.g. 30m)"},
	{"retentionPeriod", "string", false, "Retention period forwarded to gh codespace create (e.g. 720h)"},
	{"uploadBinaryOverSsh", "bool", false, "Set Zed's upload_binary_over_ssh for this host"},
	{"zedNickname", "string", false, "Friendly name shown in Zed's remote project list"},
	{"autoStop", "string", false, "Auto-stop codespace after idle duration (e.g. 30m)"},
	{"preWarm", "string", false, "Time-of-day to pre-warm codespace (e.g. 08:00)"},
	{"coder", "object", false, "Coder-specific target settings: template, workspaceName, parameters, stopAfter, organization, portForwards"},
}

// DaemonFieldDocs is the authoritative documentation for DaemonConfig fields.
var DaemonFieldDocs = []FieldDoc{
	{"hotkey", "string", false, "Global hotkey (e.g. Cmd+Shift+S)"},
	{"terminal", "string", false, "Terminal app for picker; auto to detect"},
	{"inhibitSleep", "string", false, "Hold sleep/shutdown inhibitor while a codespace session is active: off (default), sleep, or sleep+shutdown"},
}

// TargetFieldsHelp returns a formatted help string for all target fields.
func TargetFieldsHelp() string {
	var b strings.Builder
	for _, f := range TargetFieldDocs {
		req := ""
		if f.Required {
			req = " (required)"
		}
		fmt.Fprintf(&b, "  %-22s %s%s\n", f.JSON, f.Desc, req)
	}
	return b.String()
}

// DaemonFieldsHelp returns a formatted help string for daemon config fields.
func DaemonFieldsHelp() string {
	var b strings.Builder
	for _, f := range DaemonFieldDocs {
		fmt.Fprintf(&b, "  %-22s %s\n", f.JSON, f.Desc)
	}
	return b.String()
}
