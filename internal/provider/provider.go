package provider

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/sshconfig"
)

const (
	NameGitHub = "github"
	NameCoder  = "coder"
)

type Workspace struct {
	Provider    string
	ID          string
	Name        string
	DisplayName string
	Repository  string
	Branch      string
	State       string
	MachineName string
	CreatedAt   string
	LastUsedAt  string
	Template    string
	Metadata    map[string]string
}

type Manager interface {
	Name() string
	EnsurePrereqs() error
	EnsureAuth() error
	ListAllWorkspaces() ([]Workspace, error)
	ListRepositories() ([]string, error)
	ListWorkspacesForTarget(target config.Target) ([]Workspace, error)
	ResolveWorkspace(name string) (*Workspace, error)
	CreateWorkspace(target config.Target, interactive bool) (*Workspace, error)
	StartWorkspace(workspace *Workspace) (*Workspace, error)
	DeleteWorkspace(name string) error
	EnsureReachable(workspace *Workspace) error
	PrepareSSH(paths sshconfig.SSHPaths, workspace *Workspace, opts sshconfig.ManagedExtrasOptions) (string, error)
}

func RequireCommand(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%q not found on PATH", name)
	}
	return nil
}

func NewManager(cfg *config.Config) (Manager, error) {
	if name := cfg.EffectiveWorkspaceProvider(); !cfg.ProviderEnabled(name) {
		return nil, fmt.Errorf("workspace provider %q is disabled in config (providers.%s.enabled)", name, name)
	}
	switch cfg.EffectiveWorkspaceProvider() {
	case "", NameGitHub:
		return NewGitHubManager(codespace.DefaultGHRunner{}), nil
	case NameCoder:
		return NewCoderManager(cfg), nil
	default:
		return nil, fmt.Errorf("unknown workspaceProvider %q (supported: github, coder)", cfg.EffectiveWorkspaceProvider())
	}
}

func MatchesTarget(ws *Workspace, t *config.Target) bool {
	if ws == nil || t == nil {
		return false
	}
	// Case-insensitive: GitHub repository names are.
	if t.Repository != "" && ws.Repository != "" && !strings.EqualFold(ws.Repository, t.Repository) {
		return false
	}
	explicitName := t.ExplicitWorkspaceName(ws.Provider)
	if explicitName != "" && ws.Name != explicitName {
		return false
	}
	if t.DisplayName != "" && ws.DisplayName != t.DisplayName {
		return false
	}
	if t.Branch != "" && ws.Branch != "" && ws.Branch != t.Branch {
		return false
	}
	return true
}

func FindMatching(workspaces []Workspace, t *config.Target) []Workspace {
	var matches []Workspace
	for i := range workspaces {
		if MatchesTarget(&workspaces[i], t) {
			matches = append(matches, workspaces[i])
		}
	}
	return matches
}

func ChooseWorkspace(workspaces []Workspace, t *config.Target) (*Workspace, error) {
	matches := FindMatching(workspaces, t)
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		return nil, fmt.Errorf("ambiguous workspace match: %s", strings.Join(names, ", "))
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	return nil, nil
}

func DescribeWorkspace(ws *Workspace, recommended bool) string {
	state := ws.State
	if state == "" {
		state = "unknown"
	}

	label := ws.Name
	if ws.DisplayName != "" {
		label += fmt.Sprintf(" (%s)", ws.DisplayName)
	}
	label += fmt.Sprintf(", provider=%s, state=%s", ws.Provider, state)
	if ws.Branch != "" {
		label += fmt.Sprintf(", branch=%s", ws.Branch)
	}
	if ws.Template != "" {
		label += fmt.Sprintf(", template=%s", ws.Template)
	}
	if recommended {
		label += " [matches config]"
	}
	return label
}

func UniqueRepos(workspaces []Workspace) []string {
	seen := make(map[string]bool)
	var repos []string
	for _, ws := range workspaces {
		if ws.Repository == "" || seen[ws.Repository] {
			continue
		}
		seen[ws.Repository] = true
		repos = append(repos, ws.Repository)
	}
	return repos
}

func FilterByRepo(workspaces []Workspace, repo string) []Workspace {
	var result []Workspace
	for _, ws := range workspaces {
		if ws.Repository == repo {
			result = append(result, ws)
		}
	}
	return result
}

// GuessWorkspacePath returns the remote folder the editor should open:
// the target's explicit workspacePath when set, otherwise a
// /workspaces/<name> guess derived from the workspace or repository.
// Shared by the CLI and the GUI so the heuristic can't drift between
// them (it used to be duplicated verbatim in both).
func GuessWorkspacePath(target config.Target, ws *Workspace) string {
	if target.WorkspacePath != "" {
		return target.WorkspacePath
	}
	if ws != nil && ws.Provider == NameCoder {
		return "/workspaces/" + ws.Name
	}
	if target.Repository != "" {
		parts := strings.SplitN(target.Repository, "/", 2)
		return "/workspaces/" + parts[len(parts)-1]
	}
	if ws != nil && ws.Name != "" {
		return "/workspaces/" + ws.Name
	}
	return "/workspaces"
}

// IsWorkspaceRunning reports whether ws is in a state where SSH should
// already be reachable without a start.
func IsWorkspaceRunning(ws Workspace) bool {
	state := strings.ToLower(ws.State)
	return state == "available" || state == "ready" || state == "running" || state == "connected"
}
