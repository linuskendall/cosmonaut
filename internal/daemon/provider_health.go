package daemon

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"

	"github.com/linuskendall/cosmonaut/internal/doctor"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

// allGUIProviders is the full set of providers the GUI surfaces (tray,
// banner, Health section) know about. Which of them are actually polled
// and rendered is gated per-provider by config — see guiProviders.
var allGUIProviders = []string{provider.NameGitHub, provider.NameCoder}

// guiProviders returns the providers that are enabled in config, in
// display order. A disabled provider is invisible to every GUI surface:
// not polled, no tray section, no sidebar section, no auth checks — so
// a Coder-only user is never nagged to fix GitHub sign-in.
func (d *Daemon) guiProviders() []string {
	enabled := make([]string, 0, len(allGUIProviders))
	for _, name := range allGUIProviders {
		if d.Cfg.ProviderEnabled(name) {
			enabled = append(enabled, name)
		}
	}
	return enabled
}

// providerListErr returns a supplier of the named provider's most recent
// cached list error. Backed by the per-provider ProviderStatus the
// poller records, so each doctor check reads only its own provider's
// error rather than the single effective-provider listErr.
func (d *Daemon) providerListErr(name string) func() error {
	return func() error { return d.StatusFor(name).Err }
}

// guiCatalog builds the doctor catalog the GUI health surfaces render
// (main-window banner and the settings Health section). Unlike
// doctor.Catalog — which is GitHub-only and reads the single effective
// listErr — it includes every provider's auth check wired to that
// provider's own cached error, so a Coder login problem and a GitHub
// scope problem surface side by side. Each check stays silent unless its
// provider actually has the matching error.
func (d *Daemon) guiCatalog() []doctor.Check {
	providers := make([]doctor.ProviderListErr, 0, len(allGUIProviders))
	for _, name := range d.guiProviders() {
		providers = append(providers, doctor.ProviderListErr{
			Name:    name,
			ListErr: d.providerListErr(name),
		})
	}
	return doctor.CatalogForProviders(providers...)
}

// providerHasAuthIssue reports whether a provider's CLI is installed but
// its last poll surfaced an authentication/authorization problem — not
// logged in, or (GitHub only) a token missing the codespace scope.
// Transient list failures (network, timeout) don't count, so a blip
// neither hides the submenu nor raises the fix item. Returns false
// before the first poll records status, and false when the CLI isn't
// installed (nothing to fix by signing in).
func (d *Daemon) providerHasAuthIssue(name string) bool {
	status := d.StatusFor(name)
	if status.CheckedAt.IsZero() || !status.Available || status.Err == nil {
		return false
	}
	switch name {
	case provider.NameGitHub:
		return doctor.IsGitHubAuthIssue(status.Err)
	case provider.NameCoder:
		return doctor.IsCoderUnauthenticated(status.Err)
	}
	return false
}

// authIssueMenuItem returns a single tray item that routes the user to
// the settings window (Health section + auth controls) when a provider's
// CLI is installed but its last poll hit an auth/scope problem. Because
// both submenus hide themselves in that state, this is the one
// unmissable entry point to fix sign-in. Returns nil when no surfaced
// provider has an auth issue.
func (d *Daemon) authIssueMenuItem() *fyne.MenuItem {
	var names []string
	for _, name := range d.guiProviders() {
		if d.providerHasAuthIssue(name) {
			names = append(names, providerDisplayName(name))
		}
	}
	if len(names) == 0 {
		return nil
	}
	label := fmt.Sprintf("⚠ Fix %s sign-in…", strings.Join(names, " & "))
	return fyne.NewMenuItem(label, func() {
		go d.showPreferences()
	})
}

// providerDisplayName maps a provider ID to a human label for menus.
func providerDisplayName(name string) string {
	switch name {
	case provider.NameGitHub:
		return "GitHub"
	case provider.NameCoder:
		return "Coder"
	default:
		return name
	}
}
