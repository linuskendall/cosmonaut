// Package terminal provides shared helpers for launching commands in the
// user's platform terminal emulator, used by both the GUI and TUI.
//
// The helpers wrap two flows:
//
//   - OpenSSHInTerminal builds and launches an `ssh -t <alias> <cmd>` command
//     in the terminal. The remote working directory is shell-quoted so paths
//     containing spaces or other special characters can't be misinterpreted.
//   - OpenCommandInTerminal launches an arbitrary shell command. On macOS the
//     command is passed through osascript as an argv parameter (never
//     interpolated into the AppleScript source) to avoid AppleScript-level
//     injection. On Linux a list of well-known terminal emulators is tried in
//     order and exec'd with argv [-e sh -c <cmd>].
package terminal

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strings"
)

// linuxTerminals lists terminal emulators tried in order on Linux. The first
// one found on PATH wins.
var linuxTerminals = []string{"ghostty", "alacritty", "kitty", "gnome-terminal", "xterm"}

// OpenSSHInTerminal opens an SSH session to the given alias in the user's
// terminal emulator. workspacePath, if non-empty, is `cd`'d into before the
// remote shell starts; it is POSIX-shell-quoted so paths with spaces, quotes,
// or shell metacharacters are safe. multiplexer selects a persistent remote
// session that survives SSH drops and is re-attached by re-running the same
// command: "tmux" runs `tmux new -A -s cosmonaut`, "zellij" runs
// `zellij attach --create cosmonaut`; any other value gets a plain login
// shell. (Plain strings rather than config constants so this package stays
// decoupled from internal/config.)
func OpenSSHInTerminal(alias, workspacePath, multiplexer string) {
	remoteCmd := "exec $SHELL -l"
	switch multiplexer {
	case "tmux":
		remoteCmd = "tmux new -A -s cosmonaut"
	case "zellij":
		remoteCmd = "zellij attach --create cosmonaut"
	}
	cdPrefix := ""
	if workspacePath != "" {
		cdPrefix = fmt.Sprintf("cd %s && ", ShellQuote(workspacePath))
	}
	sshCmd := fmt.Sprintf("ssh -t %s %s", alias, ShellQuote(cdPrefix+remoteCmd))
	OpenCommandInTerminal(sshCmd)
}

// OpenCommandInTerminal launches the platform's default terminal emulator
// running the given shell command. The command is never interpolated into an
// AppleScript or shell string; it is passed as an argv parameter so the
// terminal/osascript see it as a single literal value.
func OpenCommandInTerminal(shellCmd string) {
	if runtime.GOOS == "darwin" {
		// Pass shellCmd as argv to osascript so AppleScript treats it as a
		// literal string. Interpolating into the script source would allow a
		// path/command containing `"` to break out and run arbitrary
		// AppleScript. `activate` brings Terminal.app to the foreground so
		// the user actually sees the new window.
		script := `on run argv
tell application "Terminal"
activate
do script item 1 of argv
end tell
end run`
		if err := exec.Command("osascript", "-e", script, "--", shellCmd).Run(); err != nil {
			log.Printf("terminal: osascript: %v", err)
		}
		return
	}
	for _, term := range linuxTerminals {
		if _, err := exec.LookPath(term); err == nil {
			// gnome-terminal deprecated (and modern releases removed) -e;
			// `--` is the supported way to pass a command. Other terminals
			// (alacritty, kitty, xterm) still use -e.
			args := []string{"-e", "sh", "-c", shellCmd}
			if term == "gnome-terminal" {
				args = []string{"--", "sh", "-c", shellCmd}
			}
			// Start (not Run): Run blocks until the terminal window is
			// closed, and callers invoke this synchronously from the
			// TUI/GUI — the whole app froze until the user closed the
			// spawned terminal. Release detaches so the child never
			// becomes a zombie waiting on us.
			cmd := exec.Command(term, args...)
			if err := cmd.Start(); err != nil {
				log.Printf("terminal: %s: %v", term, err)
				return
			}
			_ = cmd.Process.Release()
			return
		}
	}
}

// ShellQuote wraps s in POSIX single quotes so it can be safely embedded in
// a shell command. Empty strings return `”`. Embedded single quotes are
// escaped via the `'\”` idiom.
//
// Strings are returned bare only when every byte is on a conservative
// allowlist. The old implementation blocklisted known metacharacters and
// missed newline — so a workspace path like "/tmp/x\nrm -rf ~" passed
// through unquoted and the remote shell executed the second line as its
// own command — as well as `~` (tilde expansion) and `#` (comment start).
// An allowlist can't miss the next dangerous byte.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '@' || c == '%' || c == '+' || c == '=' ||
			c == ':' || c == ',' || c == '.' || c == '/' || c == '-':
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
