package main

import (
	"testing"

	"github.com/linuskendall/cosmonaut/internal/config"
)

func TestBuildRemoteShellCommand(t *testing.T) {
	cases := []struct {
		name          string
		workspacePath string
		multiplexer   string
		want          string
	}{
		{"plain shell", "", config.MultiplexerNone, "exec $SHELL -l"},
		{"tmux", "", config.MultiplexerTmux, "tmux new -A -s cosmonaut"},
		{"zellij", "", config.MultiplexerZellij, "zellij attach --create cosmonaut"},
		{"unknown falls back to shell", "", "screen", "exec $SHELL -l"},
		{"cd prefix", "/workspaces/demo", config.MultiplexerZellij, "cd /workspaces/demo && zellij attach --create cosmonaut"},
		{"cd prefix quoted", "/tmp/a b", config.MultiplexerTmux, "cd '/tmp/a b' && tmux new -A -s cosmonaut"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildRemoteShellCommand(tc.workspacePath, tc.multiplexer); got != tc.want {
				t.Fatalf("buildRemoteShellCommand(%q, %q) = %q, want %q", tc.workspacePath, tc.multiplexer, got, tc.want)
			}
		})
	}
}
