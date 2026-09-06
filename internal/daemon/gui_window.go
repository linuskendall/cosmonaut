package daemon

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/history"
	"github.com/linuskendall/cosmonaut/internal/provider"
)

// unifiedWindow is the main Cosmonaut window with sidebar + content.
type unifiedWindow struct {
	daemon  *Daemon
	win     fyne.Window
	content *fyne.Container // stack container for swapping content panels
	banner  *fyne.Container // top banner for transient warnings; empty when nothing to show
	tree    *widget.Tree

	// Data for the tree.
	allRepos []string
	filter   string
	filtered []string // repos matching current filter

	// currentView re-invokes the active right-panel view on theme change.
	currentView func()
	// currentViewID identifies the rendered view so pending async
	// done-callbacks can tell whether the user has navigated away (in
	// which case re-rendering their old view would yank the UI back).
	// Read and written on the Fyne main thread only.
	currentViewID string
}

func (uw *unifiedWindow) loadRepos() {
	repos := provider.UniqueRepos(filterWorkspacesByProvider(uw.daemon.Workspaces(), provider.NameGitHub))
	repos = mergeRepos(repos, configRepos(uw.daemon.Cfg))
	uw.allRepos = history.Load().SortRepos(repos)
	uw.applyFilter()
}

func (uw *unifiedWindow) applyFilter() {
	if uw.filter == "" {
		uw.filtered = uw.allRepos
		return
	}
	lower := strings.ToLower(uw.filter)
	uw.filtered = nil
	for _, repo := range uw.allRepos {
		if strings.Contains(strings.ToLower(repo), lower) {
			uw.filtered = append(uw.filtered, repo)
		}
	}
}

// setContent replaces the right panel content.
func (uw *unifiedWindow) setContent(obj fyne.CanvasObject) {
	uw.content.Objects = []fyne.CanvasObject{obj}
	uw.content.Refresh()
}

// --- Tree node ID scheme ---
// "section:<provider>": branch node for a provider section
// "repo:<owner/name>": branch node for a GitHub repo
// "ws:<provider>:<name>": leaf node for a workspace
// "new:<provider>:<context>": leaf node for "create new"

const (
	sectionPrefix = "section:"
	repoPrefix    = "repo:"
	wsPrefix      = "ws:"
	newPrefix     = "new:"
)

func sectionNodeID(providerName string) widget.TreeNodeID { return sectionPrefix + providerName }
func repoNodeID(repo string) widget.TreeNodeID            { return repoPrefix + repo }
func workspaceNodeID(providerName, name string) widget.TreeNodeID {
	return wsPrefix + providerName + ":" + name
}

func newNodeID(providerName, context string) widget.TreeNodeID {
	return newPrefix + providerName + ":" + context
}
func isSectionNode(id widget.TreeNodeID) bool     { return strings.HasPrefix(id, sectionPrefix) }
func isRepoNode(id widget.TreeNodeID) bool        { return strings.HasPrefix(id, repoPrefix) }
func isWorkspaceNode(id widget.TreeNodeID) bool   { return strings.HasPrefix(id, wsPrefix) }
func isNewNode(id widget.TreeNodeID) bool         { return strings.HasPrefix(id, newPrefix) }
func sectionFromNode(id widget.TreeNodeID) string { return strings.TrimPrefix(id, sectionPrefix) }
func repoFromNode(id widget.TreeNodeID) string    { return strings.TrimPrefix(id, repoPrefix) }

func providerAndNameFromWorkspaceNode(id widget.TreeNodeID) (string, string) {
	s := strings.TrimPrefix(id, wsPrefix)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", s
	}
	return parts[0], parts[1]
}

func providerAndContextFromNewNode(id widget.TreeNodeID) (string, string) {
	s := strings.TrimPrefix(id, newPrefix)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", s
	}
	return parts[0], parts[1]
}

func (uw *unifiedWindow) buildTree() *widget.Tree {
	t := widget.NewTree(
		// childUIDs
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			if id == "" {
				sections := make([]widget.TreeNodeID, 0, len(allGUIProviders))
				for _, name := range uw.daemon.guiProviders() {
					sections = append(sections, sectionNodeID(name))
				}
				return sections
			}
			if isSectionNode(id) {
				switch sectionFromNode(id) {
				case provider.NameGitHub:
					ids := make([]widget.TreeNodeID, len(uw.filtered))
					for i, repo := range uw.filtered {
						ids[i] = repoNodeID(repo)
					}
					return ids
				case provider.NameCoder:
					workspaces := filterWorkspacesByProvider(uw.daemon.Workspaces(), provider.NameCoder)
					ids := make([]widget.TreeNodeID, 0, len(workspaces)+1)
					for _, ws := range workspaces {
						if uw.filter != "" && !workspaceMatchesFilter(ws, uw.filter) {
							continue
						}
						ids = append(ids, workspaceNodeID(ws.Provider, ws.Name))
					}
					ids = append(ids, newNodeID(provider.NameCoder, ""))
					return ids
				}
			}
			if isRepoNode(id) {
				repo := repoFromNode(id)
				all := filterWorkspacesByProvider(uw.daemon.Workspaces(), provider.NameGitHub)
				repoWS := provider.FilterByRepo(all, repo)
				ids := make([]widget.TreeNodeID, 0, len(repoWS)+1)
				for _, ws := range repoWS {
					ids = append(ids, workspaceNodeID(ws.Provider, ws.Name))
				}
				ids = append(ids, newNodeID(provider.NameGitHub, repo))
				return ids
			}
			return nil
		},
		// isBranch
		func(id widget.TreeNodeID) bool {
			return id == "" || isSectionNode(id) || isRepoNode(id)
		},
		// create
		func(branch bool) fyne.CanvasObject {
			return widget.NewLabel("template")
		},
		// update
		func(id widget.TreeNodeID, branch bool, obj fyne.CanvasObject) {
			label := obj.(*widget.Label)
			switch {
			case isSectionNode(id):
				switch sectionFromNode(id) {
				case provider.NameGitHub:
					label.SetText("GitHub Codespaces")
				case provider.NameCoder:
					label.SetText("Coder Workspaces")
				}
			case isRepoNode(id):
				repo := repoFromNode(id)
				count := len(provider.FilterByRepo(filterWorkspacesByProvider(uw.daemon.Workspaces(), provider.NameGitHub), repo))
				if count > 0 {
					label.SetText(fmt.Sprintf("%s (%d)", repo, count))
				} else {
					label.SetText(repo)
				}
			case isWorkspaceNode(id):
				providerName, name := providerAndNameFromWorkspaceNode(id)
				for _, ws := range uw.daemon.Workspaces() {
					if ws.Provider == providerName && ws.Name == name {
						label.SetText(fmt.Sprintf("  %s %s", stateIcon(ws.State), workspaceLabel(ws)))
						return
					}
				}
				label.SetText("  " + name)
			case isNewNode(id):
				providerName, _ := providerAndContextFromNewNode(id)
				if providerName == provider.NameCoder {
					label.SetText("  + Create new Coder workspace")
				} else {
					label.SetText("  + Create new")
				}
			}
		},
	)

	// No OnSelected here: buildCosmoSidebar owns selection routing and
	// assigns it right after calling buildTree.
	return t
}

// --- Content panel builders ---

// showDetailFor routes to the right detail page for ws. Coder
// workspaces have a richer detail surface that takes the whole
// Workspace; GitHub codespaces look up their cached entry by name.
func (uw *unifiedWindow) showDetailFor(ws provider.Workspace) {
	if ws.Provider == provider.NameCoder {
		uw.showCoderWorkspaceDetail(ws)
		return
	}
	uw.showWorkspaceDetail(ws.Provider, ws.Name)
}

func (uw *unifiedWindow) showWorkspaceDetail(providerName, name string) {
	for _, ws := range uw.daemon.Workspaces() {
		if ws.Provider == providerName && ws.Name == name {
			if providerName == provider.NameGitHub {
				uw.showCosmoCodespaceDetail(name, ws.Repository)
				return
			}
			uw.showCoderWorkspaceDetail(ws)
			return
		}
	}
	uw.showCosmoWelcome()
}

func (uw *unifiedWindow) showCoderSummary() {
	uw.currentView = uw.showCoderSummary
	uw.currentViewID = "coder-summary"
	all := filterWorkspacesByProvider(uw.daemon.Workspaces(), provider.NameCoder)
	title := widget.NewLabel("Coder Workspaces")
	title.TextStyle = fyne.TextStyle{Bold: true}
	info := widget.NewLabel(fmt.Sprintf("%d workspace(s)", len(all)))
	var refreshBtn *widget.Button
	refreshBtn = widget.NewButton("Refresh workspaces", func() {
		refreshBtn.Disable()
		uw.daemon.refreshCoderWorkspacesAsync(func() {
			uw.loadRepos()
			uw.applyFilter()
			uw.tree.Refresh()
			uw.showCoderSummary()
		})
	})
	createBtn := widget.NewButton("Create new Coder workspace", func() {
		uw.showCreateNewForProvider(provider.NameCoder, "")
	})
	uw.setContent(container.NewVBox(
		layout.NewSpacer(),
		container.NewCenter(container.NewVBox(title, info, refreshBtn, createBtn)),
		layout.NewSpacer(),
	))
}

func (uw *unifiedWindow) showCreateNewForProvider(providerName, context string) {
	if providerName == provider.NameCoder {
		uw.showCosmoCreateNewCoder()
		return
	}
	uw.showCosmoCreateNew(context)
}

// --- Helper functions ---

func mergeRepos(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	for _, r := range base {
		seen[r] = true
	}
	result := make([]string, len(base))
	copy(result, base)
	for _, r := range extra {
		if !seen[r] {
			seen[r] = true
			result = append(result, r)
		}
	}
	return result
}

func configRepos(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]bool)
	var repos []string
	for _, t := range cfg.TargetsSnapshot() {
		if t.Repository != "" && !seen[t.Repository] {
			seen[t.Repository] = true
			repos = append(repos, t.Repository)
		}
	}
	return repos
}

func guiTargetForRepo(cfg *config.Config, repo string) (config.Target, string) {
	if cfg != nil {
		for name, t := range cfg.TargetsSnapshot() {
			if t.Repository == repo {
				return t, name
			}
		}
	}
	parts := strings.SplitN(repo, "/", 2)
	repoName := parts[len(parts)-1]
	return config.Target{
		Repository:    repo,
		WorkspacePath: "/workspaces/" + repoName,
	}, repo
}

func guiTargetForCoderWorkspace(cfg *config.Config, ws provider.Workspace) (config.Target, string) {
	if cfg != nil {
		// TargetsSnapshot deep-copies, so the WorkspaceName default applied
		// below stays local instead of writing through the shared Coder
		// pointer into the live config.
		snap := cfg.TargetsSnapshot()
		for name, t := range snap {
			if t.Coder != nil && t.Coder.WorkspaceName == ws.Name {
				return applyWorkspaceDefaults(t, ws), name
			}
		}
		for name, t := range snap {
			if t.Coder != nil {
				t = applyWorkspaceDefaults(t, ws)
				if t.Coder.WorkspaceName == "" {
					t.Coder.WorkspaceName = ws.Name
				}
				return t, name
			}
		}
	}
	return config.Target{
		WorkspacePath: "/workspaces/" + ws.Name,
		Coder: &config.CoderTargetConfig{
			WorkspaceName: ws.Name,
		},
	}, ws.Name
}

func applyWorkspaceDefaults(target config.Target, ws provider.Workspace) config.Target {
	if target.Repository == "" && ws.Repository != "" {
		target.Repository = ws.Repository
	}
	if target.WorkspacePath == "" {
		target.WorkspacePath = provider.GuessWorkspacePath(target, &ws)
	}
	return target
}

func filterWorkspacesByProvider(workspaces []provider.Workspace, providerName string) []provider.Workspace {
	var result []provider.Workspace
	for _, ws := range workspaces {
		if ws.Provider == providerName {
			result = append(result, ws)
		}
	}
	return result
}

func workspaceMatchesFilter(ws provider.Workspace, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	fields := []string{ws.Name, ws.DisplayName, ws.Repository, ws.Branch, ws.Template}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), filter) {
			return true
		}
	}
	return false
}

func workspaceLabel(ws provider.Workspace) string {
	name := ws.DisplayName
	if name == "" {
		name = ws.Name
	}
	if ws.Provider == provider.NameCoder && ws.Template != "" {
		return fmt.Sprintf("%s (%s)", name, ws.Template)
	}
	if ws.Branch != "" {
		return fmt.Sprintf("%s (%s)", name, ws.Branch)
	}
	return name
}

func configuredCoderTargets(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var names []string
	for name, target := range cfg.TargetsSnapshot() {
		if target.Coder != nil && target.Coder.Template != "" {
			names = append(names, name)
		}
	}
	return names
}

func coderWorkspaceNameFromInput(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ' || r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}
