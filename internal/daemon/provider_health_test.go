package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

func statusMap(name string, s ProviderStatus) map[string]ProviderStatus {
	return map[string]ProviderStatus{name: s}
}

func TestProviderHasAuthIssue(t *testing.T) {
	now := time.Now()
	scopeErr := errors.New(`error: this API operation needs the "codespace" scope`)
	ghAuthErr := errors.New("not logged into any GitHub hosts")
	coderAuthErr := errors.New("coder: not authenticated, run coder login")
	netErr := errors.New("dial tcp: connection refused")

	cases := []struct {
		name     string
		provider string
		status   ProviderStatus
		want     bool
	}{
		{"github scope missing", provider.NameGitHub, ProviderStatus{Available: true, Err: scopeErr, CheckedAt: now}, true},
		{"github not logged in", provider.NameGitHub, ProviderStatus{Available: true, Err: ghAuthErr, CheckedAt: now}, true},
		{"github transient error", provider.NameGitHub, ProviderStatus{Available: true, Err: netErr, CheckedAt: now}, false},
		{"github healthy", provider.NameGitHub, ProviderStatus{Available: true, CheckedAt: now}, false},
		{"github cli missing", provider.NameGitHub, ProviderStatus{Available: false, Err: scopeErr, CheckedAt: now}, false},
		{"github pre-poll", provider.NameGitHub, ProviderStatus{}, false},
		{"coder not authenticated", provider.NameCoder, ProviderStatus{Available: true, Err: coderAuthErr, CheckedAt: now}, true},
		{"coder transient error", provider.NameCoder, ProviderStatus{Available: true, Err: netErr, CheckedAt: now}, false},
		{"coder healthy", provider.NameCoder, ProviderStatus{Available: true, CheckedAt: now}, false},
		{"coder cli missing", provider.NameCoder, ProviderStatus{Available: false, Err: coderAuthErr, CheckedAt: now}, false},
		{"coder pre-poll", provider.NameCoder, ProviderStatus{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{providerStatus: statusMap(tc.provider, tc.status)}
			if got := d.providerHasAuthIssue(tc.provider); got != tc.want {
				t.Fatalf("providerHasAuthIssue(%s) = %v, want %v", tc.provider, got, tc.want)
			}
		})
	}
}

func TestAuthIssueMenuItem(t *testing.T) {
	now := time.Now()
	scopeErr := errors.New(`needs the "codespace" scope`)
	coderAuthErr := errors.New("not authenticated")

	t.Run("no issue returns nil", func(t *testing.T) {
		d := &Daemon{providerStatus: map[string]ProviderStatus{
			provider.NameGitHub: {Available: true, CheckedAt: now},
			provider.NameCoder:  {Available: true, CheckedAt: now},
		}}
		if item := d.authIssueMenuItem(); item != nil {
			t.Fatalf("authIssueMenuItem() = %q, want nil when all healthy", item.Label)
		}
	})

	t.Run("single provider", func(t *testing.T) {
		d := &Daemon{providerStatus: map[string]ProviderStatus{
			provider.NameGitHub: {Available: true, Err: scopeErr, CheckedAt: now},
			provider.NameCoder:  {Available: true, CheckedAt: now},
		}}
		item := d.authIssueMenuItem()
		if item == nil {
			t.Fatal("authIssueMenuItem() = nil, want an item for GitHub")
		}
		if want := "⚠ Fix GitHub sign-in…"; item.Label != want {
			t.Fatalf("label = %q, want %q", item.Label, want)
		}
	})

	t.Run("disabled provider is ignored", func(t *testing.T) {
		f := false
		cfg := &config.Config{}
		cfg.Providers.GitHub.Enabled = &f
		d := &Daemon{
			Cfg: cfg,
			providerStatus: map[string]ProviderStatus{
				provider.NameGitHub: {Available: true, Err: scopeErr, CheckedAt: now},
				provider.NameCoder:  {Available: true, CheckedAt: now},
			},
		}
		if item := d.authIssueMenuItem(); item != nil {
			t.Fatalf("authIssueMenuItem() = %q, want nil when the failing provider is disabled", item.Label)
		}
	})

	t.Run("both providers", func(t *testing.T) {
		d := &Daemon{providerStatus: map[string]ProviderStatus{
			provider.NameGitHub: {Available: true, Err: scopeErr, CheckedAt: now},
			provider.NameCoder:  {Available: true, Err: coderAuthErr, CheckedAt: now},
		}}
		item := d.authIssueMenuItem()
		if item == nil {
			t.Fatal("authIssueMenuItem() = nil, want an item for both providers")
		}
		if want := "⚠ Fix GitHub & Coder sign-in…"; item.Label != want {
			t.Fatalf("label = %q, want %q", item.Label, want)
		}
	})
}

// guiCatalog surfaces each provider's own auth check independently: a
// coder login error must produce the coder check even though GitHub is
// the conventional default, and a healthy provider produces no failing
// check.
func TestGUICatalogSurfacesBothProviders(t *testing.T) {
	now := time.Now()
	d := &Daemon{providerStatus: map[string]ProviderStatus{
		provider.NameCoder: {Available: true, Err: errors.New("not authenticated"), CheckedAt: now},
	}}

	var coderFailing, ghFailing bool
	for _, c := range d.guiCatalog() {
		if c.Status() == nil {
			continue
		}
		switch c.ID {
		case "coder-login":
			coderFailing = true
		case "gh-codespace-scope":
			ghFailing = true
		}
	}
	if !coderFailing {
		t.Error("guiCatalog: coder-login check did not fail despite coder auth error")
	}
	if ghFailing {
		t.Error("guiCatalog: gh-codespace-scope check failed despite no GitHub error")
	}
}

// A disabled provider contributes no checks to the GUI catalog at all,
// so its auth problems never surface in the Health section or banners.
func TestGUICatalogSkipsDisabledProvider(t *testing.T) {
	now := time.Now()
	f := false
	cfg := &config.Config{}
	cfg.Providers.GitHub.Enabled = &f
	d := &Daemon{
		Cfg: cfg,
		providerStatus: map[string]ProviderStatus{
			provider.NameGitHub: {Available: true, Err: errors.New(`needs the "codespace" scope`), CheckedAt: now},
		},
	}

	for _, c := range d.guiCatalog() {
		if c.ID == "gh-codespace-scope" || c.ID == "gh-auth" || c.ID == "gh-cli" {
			t.Errorf("guiCatalog: %s present despite the GitHub provider being disabled", c.ID)
		}
	}
}
