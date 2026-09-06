package daemon

import (
	"strings"
	"testing"

	"github.com/linuskendall/cosmonaut/internal/provider"
)

// TestWorkspaceSSHControlsForCoder asserts that Coder workspaces hide the
// ControlMaster toggle (because ~/.ssh/cosmonaut/coder.conf is shared
// across all Coder workspaces and a per-workspace toggle would be last
// writer wins). The multiplexer selector is unaffected since it only wraps
// the per-invocation SSH command.
func TestWorkspaceSSHControlsForCoder(t *testing.T) {
	got := workspaceSSHControlsFor(provider.NameCoder)
	if got.ShowControlMaster {
		t.Fatalf("Coder should hide ControlMaster toggle, got ShowControlMaster=true")
	}
	if !got.ShowMultiplexer {
		t.Fatalf("Coder should still expose the multiplexer selector, got ShowMultiplexer=false")
	}
	if got.SharedConfNote == "" {
		t.Fatalf("Coder should provide a SharedConfNote explaining why ControlMaster is hidden")
	}
	if !strings.Contains(got.SharedConfNote, "coder.conf") {
		t.Fatalf("SharedConfNote should reference coder.conf, got %q", got.SharedConfNote)
	}
}

// TestWorkspaceSSHControlsForGitHub asserts that GitHub workspaces keep
// both per-workspace controls — each GitHub codespace has its own
// ~/.ssh/cosmonaut/cs.<name>.conf so the toggle is coherent there.
func TestWorkspaceSSHControlsForGitHub(t *testing.T) {
	got := workspaceSSHControlsFor(provider.NameGitHub)
	if !got.ShowControlMaster {
		t.Fatalf("GitHub should expose ControlMaster toggle, got ShowControlMaster=false")
	}
	if !got.ShowMultiplexer {
		t.Fatalf("GitHub should expose multiplexer selector, got ShowMultiplexer=false")
	}
	if got.SharedConfNote != "" {
		t.Fatalf("GitHub should not emit a SharedConfNote, got %q", got.SharedConfNote)
	}
}
