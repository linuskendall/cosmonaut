package daemon

import (
	"fmt"
	"image/color"
	"log"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/linuskendall/cosmonaut/internal/codespace"
	"github.com/linuskendall/cosmonaut/internal/config"
	"github.com/linuskendall/cosmonaut/internal/doctor"
	"github.com/linuskendall/cosmonaut/internal/provider"
	"github.com/linuskendall/cosmonaut/internal/terminal"
)

// buildSettingsPanel builds the settings content panel for the unified window.
func (d *Daemon) buildSettingsPanel(win fyne.Window) fyne.CanvasObject {
	var items []fyne.CanvasObject

	heading := widget.NewLabel("Settings")
	heading.TextStyle = fyne.TextStyle{Bold: true}
	items = append(items, heading)

	// Health checks: doctor catalog with per-check status and fix
	// buttons. Mirrors what the main-window banner shows, but stays
	// visible even if the user dismissed banners earlier.
	items = append(items, d.buildHealthSection(win))
	items = append(items, widget.NewSeparator())

	// Provider enable/disable toggles.
	items = append(items, d.buildProvidersSection(win))
	items = append(items, widget.NewSeparator())

	// GitHub auth section — pointless when the provider is disabled.
	if d.Cfg.ProviderEnabled(provider.NameGitHub) {
		items = append(items, d.buildAuthSection(win))
		items = append(items, widget.NewSeparator())
	}

	// Editor selection.
	items = append(items, d.buildEditorSection())
	items = append(items, widget.NewSeparator())

	// Daemon settings.
	if d.Cfg != nil {
		d.Cfg.EnsureDaemon()
		items = append(items, d.buildDaemonSection())
		items = append(items, widget.NewSeparator())
	}

	// Default target settings.
	if defaultTarget := d.Cfg.GetDefaultTarget(); defaultTarget != "" {
		if _, ok := d.Cfg.Target(defaultTarget); ok {
			items = append(items, d.buildTargetSection())
			items = append(items, widget.NewSeparator())
		}
	}

	// Edit config file button.
	configPath := d.ConfigPath
	items = append(items, widget.NewButton("Edit config file...", func() {
		go openFile(configPath)
	}))

	return container.NewVScroll(container.NewPadded(container.NewVBox(items...)))
}

// showPreferences opens settings as a separate window (called from tray menu).
func (d *Daemon) showPreferences() {
	if d.app == nil {
		return
	}
	fyne.Do(func() {
		win := d.app.NewWindow("Cosmonaut Settings")
		win.Resize(fyne.NewSize(420, 400))
		win.SetFixedSize(true)
		win.CenterOnScreen()
		win.SetContent(d.buildSettingsPanel(win))
		unsubscribeTheme := d.addThemeListener(func() {
			win.SetContent(d.buildSettingsPanel(win))
		})
		win.SetOnClosed(unsubscribeTheme)
		win.Show()
	})
}

// buildHealthSection renders the doctor catalog. Failing checks stay
// fully visible with their fix actions; passing checks are folded into
// a single collapsed accordion row so the section stays compact when
// everything is healthy.
func (d *Daemon) buildHealthSection(win fyne.Window) fyne.CanvasObject {
	heading := widget.NewLabel("Health checks")
	heading.TextStyle = fyne.TextStyle{Bold: true}

	rebuild := func() {
		if win != nil {
			win.SetContent(d.buildSettingsPanel(win))
		}
		// Also refresh the main window banner if it's open.
		d.refreshMainWindowBanner()
	}

	var failing, passing []doctor.Check
	for _, c := range d.guiCatalog() {
		if c.Status() == nil {
			passing = append(passing, c)
		} else {
			failing = append(failing, c)
		}
	}

	rows := []fyne.CanvasObject{heading}
	for _, c := range failing {
		rows = append(rows, d.buildHealthRow(c, win, rebuild))
	}

	if len(passing) > 0 {
		var title string
		if len(failing) == 0 {
			title = fmt.Sprintf("✓ All OK (%d checks)", len(passing))
		} else {
			title = fmt.Sprintf("Other checks passing (%d)", len(passing))
		}
		var passingRows []fyne.CanvasObject
		for _, c := range passing {
			passingRows = append(passingRows, d.buildHealthRow(c, win, rebuild))
		}
		detail := container.NewVBox(passingRows...)
		acc := widget.NewAccordion(widget.NewAccordionItem(title, detail))
		rows = append(rows, acc)
	}

	return container.NewVBox(rows...)
}

func (d *Daemon) buildHealthRow(c doctor.Check, win fyne.Window, rebuild func()) fyne.CanvasObject {
	issue := c.Status()

	var dotColor color.Color
	var statusText string
	switch {
	case issue == nil:
		dotColor = statusOK
		statusText = "OK"
	case issue.Severity == doctor.SeverityError:
		dotColor = statusError
		statusText = "Error"
	default:
		dotColor = statusWarn
		statusText = "Warning"
	}
	dot := canvas.NewCircle(dotColor)
	dot.StrokeWidth = 0
	dot.Resize(fyne.NewSize(8, 8))

	title := widget.NewLabel(c.Title)
	title.TextStyle = fyne.TextStyle{Bold: true}

	status := canvas.NewText(statusText, dotColor)
	status.TextSize = 11
	status.TextStyle = fyne.TextStyle{Monospace: true, Bold: true}

	header := container.NewHBox(container.NewCenter(dot), title, layout.NewSpacer(), status)

	var detail fyne.CanvasObject
	if issue != nil {
		lbl := widget.NewLabel(issue.Summary)
		lbl.Wrapping = fyne.TextWrapWord
		detail = lbl
	} else {
		// When passing, show the description so the user understands
		// what was checked.
		lbl := widget.NewLabel(c.Description)
		lbl.Wrapping = fyne.TextWrapWord
		detail = lbl
	}

	var actions fyne.CanvasObject
	if issue != nil {
		var btn *widget.Button
		switch {
		case c.HasInProcessFix():
			btn = primaryButton("Fix", func() {
				go func() {
					if err := c.Fix(); err != nil {
						fyne.Do(func() {
							dialog.ShowError(fmt.Errorf("fix %s: %w", c.ID, err), win)
						})
						return
					}
					fyne.Do(rebuild)
				}()
			})
		case c.HasTerminalFix():
			btn = primaryButton("Fix in terminal", func() {
				cmd := c.FixCommand() + `; echo; echo "Press enter to close"; read _`
				go terminal.OpenCommandInTerminal(cmd)
			})
		}
		recheckBtn := widget.NewButton("Re-check", func() { rebuild() })
		row := container.NewHBox(layout.NewSpacer())
		if btn != nil {
			row.Add(btn)
		}
		row.Add(recheckBtn)
		// If the user previously dismissed the banner, surface a way
		// to bring it back.
		if d.IsDismissed(c.ID) {
			restoreBtn := widget.NewButton("Show banner again", func() {
				d.UndismissCheck(c.ID)
				rebuild()
			})
			row.Add(restoreBtn)
		}
		actions = row
	}

	if actions == nil {
		return container.NewPadded(container.NewVBox(header, detail))
	}
	return container.NewPadded(container.NewVBox(header, detail, actions))
}

// refreshMainWindowBanner re-renders the top banner of an open main
// window if there is one, so a fix applied (or banner restored) from
// the Settings page is reflected immediately.
func (d *Daemon) refreshMainWindowBanner() {
	if uw := d.activeUnifiedWindow(); uw != nil {
		uw.refreshBanner()
	}
}

// buildProvidersSection renders one enable/disable checkbox per known
// provider. Disabling a provider hides every surface it owns (tray
// section, sidebar section, auth prompts, health checks) and stops
// polling its CLI — the escape hatch for users who only ever use one of
// the two. The last enabled provider's checkbox is disabled so the app
// can't be talked into showing nothing at all.
func (d *Daemon) buildProvidersSection(win fyne.Window) fyne.CanvasObject {
	heading := widget.NewLabel("Providers")
	heading.TextStyle = fyne.TextStyle{Bold: true}

	refresh := func() {
		d.persistConfig()
		d.forcePollAsync(nil)
		if win != nil {
			win.SetContent(d.buildSettingsPanel(win))
		}
	}

	rows := []fyne.CanvasObject{heading}
	enabledCount := len(d.guiProviders())
	for _, name := range allGUIProviders {
		name := name
		enabled := d.Cfg.ProviderEnabled(name)
		check := widget.NewCheck(providerDisplayName(name), func(v bool) {
			d.Cfg.SetProviderEnabled(name, &v)
			refresh()
		})
		check.SetChecked(enabled)
		if enabled && enabledCount == 1 {
			check.Disable()
		}
		rows = append(rows, check)
	}
	return container.NewVBox(rows...)
}

func (d *Daemon) buildAuthSection(win fyne.Window) fyne.CanvasObject {
	// EnsureGHAuth execs `gh auth status` (network round-trip) and this
	// builder runs on the Fyne main thread on every settings rebuild —
	// probe asynchronously and fill the section in via fyne.Do.
	statusLabel := widget.NewLabel("GitHub: checking…")

	// After an auth-state change, the section's button label and the tray
	// menu both need to reflect the new state. Rebuilding the whole settings
	// panel is the simplest correct refresh: the panel is small, all sections
	// re-read their state on construction, and there's no other live state
	// in the window worth preserving across an auth flip.
	refresh := func() {
		d.rebuildTrayMenu()
		if win != nil {
			win.SetContent(d.buildSettingsPanel(win))
		}
	}

	actionBtn := widget.NewButton("Log in...", nil)
	actionBtn.Disable()

	logout := func() {
		actionBtn.Disable()
		go func() {
			_, err := d.Runner.Run([]string{"auth", "logout", "--hostname", "github.com"})
			fyne.Do(func() {
				if err != nil {
					log.Printf("auth logout: %v", err)
					dialog.ShowError(fmt.Errorf("gh auth logout failed: %w", err), win)
					actionBtn.Enable()
					return
				}
				refresh()
			})
		}()
	}
	login := func() {
		actionBtn.Disable()
		go func() {
			_, err := d.Runner.Run([]string{"auth", "login", "--web", "--hostname", "github.com"})
			fyne.Do(func() {
				if err != nil {
					log.Printf("auth login: %v", err)
					dialog.ShowError(fmt.Errorf("gh auth login failed: %w", err), win)
					actionBtn.Enable()
					return
				}
				refresh()
			})
		}()
	}

	go func() {
		authed := codespace.EnsureGHAuth(d.Runner) == nil
		fyne.Do(func() {
			if authed {
				statusLabel.SetText("GitHub: authenticated")
				actionBtn.SetText("Remove auth")
				actionBtn.OnTapped = logout
			} else {
				statusLabel.SetText("GitHub: not authenticated")
				actionBtn.SetText("Log in...")
				actionBtn.OnTapped = login
			}
			actionBtn.Enable()
		})
	}()

	return container.NewHBox(statusLabel, layout.NewSpacer(), actionBtn)
}

func (d *Daemon) buildEditorSection() fyne.CanvasObject {
	editorEntry := widget.NewEntry()
	editorEntry.SetPlaceHolder("zed (default)")
	editorEntry.SetText(d.Cfg.GetEditor())
	editorEntry.OnSubmitted = func(val string) {
		d.Cfg.SetEditor(val)
		d.persistConfig()
	}
	return widget.NewForm(widget.NewFormItem("Editor", editorEntry))
}

func (d *Daemon) buildDaemonSection() fyne.CanvasObject {
	daemon := d.Cfg.EnsureDaemon()

	hotkeyEntry := widget.NewEntry()
	hotkeyEntry.SetPlaceHolder(DefaultHotkey())
	hotkeyEntry.SetText(daemon.Hotkey)
	hotkeyEntry.OnSubmitted = func(val string) {
		d.Cfg.SetDaemonHotkey(strings.TrimSpace(val))
		d.persistConfig()
	}

	currentInhibit := daemon.InhibitSleep
	if currentInhibit == "" {
		currentInhibit = "off"
	}
	inhibitSelect := widget.NewSelect([]string{"off", "sleep", "sleep+shutdown"}, func(val string) {
		d.Cfg.SetDaemonInhibitSleep(val)
		if d.sessions != nil {
			d.sessions.SetMode(val)
		}
		d.persistConfig()
	})
	inhibitSelect.Selected = currentInhibit

	return widget.NewForm(
		widget.NewFormItem("Hotkey", hotkeyEntry),
		widget.NewFormItem("Inhibit sleep", inhibitSelect),
	)
}

func (d *Daemon) buildTargetSection() fyne.CanvasObject {
	targetName := d.Cfg.GetDefaultTarget()
	t, _ := d.Cfg.Target(targetName)

	heading := widget.NewLabel(fmt.Sprintf("Target: %s", targetName))
	heading.TextStyle = fyne.TextStyle{Bold: true}

	currentAutoStop := t.AutoStop
	if currentAutoStop == "" {
		currentAutoStop = "off"
	}
	autoStopSelect := widget.NewSelect([]string{"off", "15m", "30m", "1h"}, func(val string) {
		d.Cfg.UpdateTarget(targetName, func(t *config.Target, _ bool) {
			if val == "off" {
				t.AutoStop = ""
			} else {
				t.AutoStop = val
			}
		})
		d.persistConfig()
	})
	autoStopSelect.Selected = currentAutoStop

	currentPreWarm := t.PreWarm
	if currentPreWarm == "" {
		currentPreWarm = "off"
	}
	preWarmSelect := widget.NewSelect([]string{"off", "08:00", "09:00", "10:00"}, func(val string) {
		d.Cfg.UpdateTarget(targetName, func(t *config.Target, _ bool) {
			if val == "off" {
				t.PreWarm = ""
			} else {
				t.PreWarm = val
			}
		})
		d.persistConfig()
	})
	preWarmSelect.Selected = currentPreWarm

	form := widget.NewForm(
		widget.NewFormItem("Auto-stop", autoStopSelect),
		widget.NewFormItem("Pre-warm", preWarmSelect),
	)

	return container.NewVBox(heading, form)
}

// persistConfig saves config and rebuilds the tray menu. A nil Cfg is a
// no-op: SaveConfig would refuse it anyway (writing `null` over the user's
// config file is never right), so don't even log it as an error here.
func (d *Daemon) persistConfig() {
	if d.Cfg == nil {
		return
	}
	if err := config.SaveConfig(d.ConfigPath, d.Cfg); err != nil {
		log.Printf("error saving config: %v", err)
	}
	d.rebuildTrayMenu()
}
