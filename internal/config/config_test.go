package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseJSONCAcceptsCommentsAndTrailingCommas(t *testing.T) {
	source := `
	{
	  // comment
	  "name": "demo",
	  "nested": {
	    "enabled": true,
	  },
	}`

	clean, err := ParseJSONC(source)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(clean, &got); err != nil {
		t.Fatal(err)
	}

	if got["name"] != "demo" {
		t.Errorf("name = %v, want demo", got["name"])
	}
	nested := got["nested"].(map[string]any)
	if nested["enabled"] != true {
		t.Errorf("nested.enabled = %v, want true", nested["enabled"])
	}
}

func TestLoadConfig(t *testing.T) {
	content := `{
		"defaultTarget": "demo",
		"targets": {
			"demo": {
				"repository": "acme/demo",
				"workspacePath": "/workspaces"
			}
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultTarget != "demo" {
		t.Errorf("defaultTarget = %q, want demo", cfg.DefaultTarget)
	}
	if cfg.WorkspaceProvider != "github" {
		t.Errorf("workspaceProvider = %q, want github default", cfg.WorkspaceProvider)
	}
	if _, ok := cfg.Targets["demo"]; !ok {
		t.Error("missing target 'demo'")
	}
}

func TestWorkspaceSSHDefaultsAndOverrides(t *testing.T) {
	var cfg Config

	// Defaults: ControlMaster on, no multiplexer, no map allocated.
	if !cfg.WorkspaceSSHControlMaster("github", "cs-a") {
		t.Error("default ControlMaster should be true")
	}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-a"); got != MultiplexerNone {
		t.Errorf("default multiplexer = %q, want none", got)
	}

	// Explicit override on one workspace doesn't leak to others.
	off := false
	cfg.SetWorkspaceSSHControlMaster("github", "cs-a", &off)
	if cfg.WorkspaceSSHControlMaster("github", "cs-a") {
		t.Error("explicit false should win over default")
	}
	if !cfg.WorkspaceSSHControlMaster("github", "cs-b") {
		t.Error("cs-b should still see the default")
	}

	zellij := MultiplexerZellij
	cfg.SetWorkspaceSSHMultiplexer("coder", "ws-1", &zellij)
	if got := cfg.WorkspaceSSHMultiplexer("coder", "ws-1"); got != MultiplexerZellij {
		t.Errorf("explicit multiplexer = %q, want zellij", got)
	}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-a"); got != MultiplexerNone {
		t.Errorf("multiplexer for coder:ws-1 must not affect github:cs-a (got %q)", got)
	}

	// Clearing the last setting on a workspace drops the entry, so the
	// settings map doesn't accumulate dead keys over time.
	cfg.SetWorkspaceSSHControlMaster("github", "cs-a", nil)
	if _, ok := cfg.WorkspaceSSH["github:cs-a"]; ok {
		t.Error("entry should be removed once all fields are nil")
	}
}

func TestWorkspaceSSHMultiplexerResolution(t *testing.T) {
	on, off := true, false
	tmux := MultiplexerTmux

	// Legacy tmux boolean maps to tmux/none.
	cfg := &Config{WorkspaceSSH: map[string]WorkspaceSSHSettings{
		"github:cs-a": {Tmux: &on},
		"github:cs-b": {Tmux: &off},
	}}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-a"); got != MultiplexerTmux {
		t.Errorf("legacy tmux=true resolves to %q, want tmux", got)
	}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-b"); got != MultiplexerNone {
		t.Errorf("legacy tmux=false resolves to %q, want none", got)
	}

	// Global default applies where no per-workspace entry exists, and an
	// explicit per-workspace value (including legacy tmux=false) wins over it.
	cfg.SSH = &SSHConfig{Multiplexer: MultiplexerZellij}
	if got := cfg.WorkspaceSSHMultiplexer("coder", "ws-1"); got != MultiplexerZellij {
		t.Errorf("global default resolves to %q, want zellij", got)
	}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-b"); got != MultiplexerNone {
		t.Errorf("per-workspace tmux=false should beat the global default (got %q)", got)
	}

	// New field wins over the legacy boolean.
	cfg.WorkspaceSSH["github:cs-b"] = WorkspaceSSHSettings{Tmux: &off, Multiplexer: &tmux}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-b"); got != MultiplexerTmux {
		t.Errorf("multiplexer field should beat legacy tmux (got %q)", got)
	}

	// Unknown values are ignored at every level.
	bogus := "screen"
	cfg.WorkspaceSSH["github:cs-c"] = WorkspaceSSHSettings{Multiplexer: &bogus}
	if got := cfg.WorkspaceSSHMultiplexer("github", "cs-c"); got != MultiplexerZellij {
		t.Errorf("invalid per-workspace value should fall through to global (got %q)", got)
	}
	cfg.SSH.Multiplexer = "screen"
	if got := cfg.WorkspaceSSHMultiplexer("coder", "ws-1"); got != MultiplexerNone {
		t.Errorf("invalid global value should fall through to none (got %q)", got)
	}

	// Writing the new field clears the legacy boolean.
	cfg.SetWorkspaceSSHMultiplexer("github", "cs-a", &tmux)
	if s := cfg.WorkspaceSSH["github:cs-a"]; s.Tmux != nil {
		t.Error("SetWorkspaceSSHMultiplexer should clear the legacy tmux field")
	}
}

func TestLoadCoderConfig(t *testing.T) {
	content := `{
		"workspaceProvider": "coder",
		"providers": {
			"coder": {
				"organization": "coder"
			}
		},
		"targets": {
			"work": {
				"workspacePath": "/workspaces/demo",
				"coder": {
					"template": "nomad-devcontainer",
					"workspaceName": "demo",
					"parameters": {
						"repo": "acme/demo"
					},
					"stopAfter": "8h",
					"portForwards": [
						{"label": "app", "localPort": 8080, "remotePort": 3000, "protocol": "tcp"}
					]
				}
			}
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EffectiveWorkspaceProvider(); got != "coder" {
		t.Fatalf("workspace provider = %q, want coder", got)
	}
	target := cfg.Targets["work"]
	if target.Coder == nil || target.Coder.Template != "nomad-devcontainer" {
		t.Fatalf("coder target not parsed: %+v", target.Coder)
	}
	if target.Coder.Parameters["repo"] != "acme/demo" {
		t.Fatalf("coder parameters = %+v", target.Coder.Parameters)
	}
	if len(target.Coder.PortForwards) != 1 || target.Coder.PortForwards[0].RemotePort != 3000 {
		t.Fatalf("coder port forwards = %+v", target.Coder.PortForwards)
	}
}

func TestProviderEnabled(t *testing.T) {
	f := false
	tr := true

	var nilCfg *Config
	if !nilCfg.ProviderEnabled("github") {
		t.Fatal("nil config should report providers enabled")
	}

	cfg := &Config{}
	if !cfg.ProviderEnabled("github") || !cfg.ProviderEnabled("coder") {
		t.Fatal("unset enabled should default to true")
	}
	if cfg.ProviderEnabled("unknown") {
		t.Fatal("unknown provider should report disabled")
	}

	cfg.Providers.GitHub.Enabled = &f
	cfg.Providers.Coder.Enabled = &tr
	if cfg.ProviderEnabled("github") {
		t.Fatal("github should be disabled")
	}
	if !cfg.ProviderEnabled("coder") {
		t.Fatal("coder should be enabled")
	}

	cfg.SetProviderEnabled("github", &tr)
	if !cfg.ProviderEnabled("github") {
		t.Fatal("SetProviderEnabled(true) should enable github")
	}
	cfg.SetProviderEnabled("github", nil)
	if cfg.Providers.GitHub.Enabled != nil {
		t.Fatal("SetProviderEnabled(nil) should clear the field")
	}
}

func TestEffectiveWorkspaceProviderFallsBackToCoder(t *testing.T) {
	f := false

	cfg := &Config{}
	if got := cfg.EffectiveWorkspaceProvider(); got != "github" {
		t.Fatalf("default provider = %q, want github", got)
	}

	cfg.Providers.GitHub.Enabled = &f
	if got := cfg.EffectiveWorkspaceProvider(); got != "coder" {
		t.Fatalf("provider with github disabled = %q, want coder", got)
	}

	// An explicit workspaceProvider always wins over the fallback.
	cfg.WorkspaceProvider = "github"
	if got := cfg.EffectiveWorkspaceProvider(); got != "github" {
		t.Fatalf("explicit provider = %q, want github", got)
	}

	// Both disabled: no sensible fallback, keep the github default.
	cfg = &Config{}
	cfg.Providers.GitHub.Enabled = &f
	cfg.Providers.Coder.Enabled = &f
	if got := cfg.EffectiveWorkspaceProvider(); got != "github" {
		t.Fatalf("provider with both disabled = %q, want github", got)
	}
}

func TestLoadConfigParsesProviderEnabled(t *testing.T) {
	content := `{
		"providers": {
			"github": {"enabled": false},
			"coder": {"enabled": true}
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderEnabled("github") {
		t.Fatal("github should be disabled from config")
	}
	if !cfg.ProviderEnabled("coder") {
		t.Fatal("coder should be enabled from config")
	}
	if got := cfg.EffectiveWorkspaceProvider(); got != "coder" {
		t.Fatalf("effective provider = %q, want coder", got)
	}
}

func TestTargetCloneIsDeep(t *testing.T) {
	up := true
	orig := Target{
		Repository:          "acme/demo",
		UploadBinaryOverSSH: &up,
		Coder: &CoderTargetConfig{
			WorkspaceName: "ws",
			Parameters:    map[string]string{"repo": "acme/demo"},
			PortForwards:  []PortForward{{RemotePort: 3000, LocalPort: 3000}},
		},
	}
	cp := orig.Clone()
	cp.Coder.WorkspaceName = "changed"
	cp.Coder.Parameters["repo"] = "changed"
	cp.Coder.PortForwards[0].RemotePort = 9999
	*cp.UploadBinaryOverSSH = false

	if orig.Coder.WorkspaceName != "ws" {
		t.Fatal("Clone shares Coder pointer")
	}
	if orig.Coder.Parameters["repo"] != "acme/demo" {
		t.Fatal("Clone shares Parameters map")
	}
	if orig.Coder.PortForwards[0].RemotePort != 3000 {
		t.Fatal("Clone shares PortForwards slice")
	}
	if !*orig.UploadBinaryOverSSH {
		t.Fatal("Clone shares UploadBinaryOverSSH pointer")
	}
}

func TestUpdateTargetReportsExistence(t *testing.T) {
	cfg := &Config{}
	cfg.UpdateTarget("new", func(tg *Target, exists bool) {
		if exists {
			t.Error("target should not exist yet")
		}
		tg.Repository = "acme/demo"
	})
	cfg.UpdateTarget("new", func(tg *Target, exists bool) {
		if !exists {
			t.Error("target should exist now")
		}
		if tg.Repository != "acme/demo" {
			t.Errorf("repository = %q", tg.Repository)
		}
	})
}

// TestConfigConcurrentAccess exercises the accessor surface from many
// goroutines at once; it exists to fail under `go test -race` if any
// accessor touches shared state without the mutex.
func TestConfigConcurrentAccess(t *testing.T) {
	cfg := &Config{Targets: map[string]Target{
		"work": {Repository: "acme/demo", Coder: &CoderTargetConfig{WorkspaceName: "ws"}},
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			cfg.UpdateTarget("work", func(tg *Target, _ bool) {
				tg.Coder.PortForwards = append(tg.Coder.PortForwards, PortForward{RemotePort: i})
			})
			cfg.SetDaemonInhibitSleep("sleep")
			cfg.SetEditor("zed")
		}
	}()
	for i := 0; i < 500; i++ {
		for range cfg.TargetsSnapshot() {
		}
		_, _ = cfg.Target("work")
		_ = cfg.GetDefaultTarget()
		_ = cfg.GetEditor()
		_ = cfg.CoderOrganization()
		_ = cfg.EnsureDaemon()
		_ = cfg.EffectiveWorkspaceProvider()
	}
	<-done
}
