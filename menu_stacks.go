package main

// menu_stacks.go — Stacks tab: one row per stack file with run/total counts,
// file size, image cached-count, RAM, and a UP/PARTIAL/DOWN status. ENTER/TAB
// opens the per-stack action popup; "*"/"A" opens the ALL-STACKS popup.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m menuModel) filteredStacks() []tuiStack {
	var out []tuiStack
	for _, s := range m.data.Stacks {
		if m.fltLetter != "" && !tuiMatchLetter(s.Name, m.fltLetter) {
			continue
		}
		if m.fltInline != "" && !tuiContains([]string{s.Name}, m.fltInline) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (m menuModel) renderStacks() string {
	items := m.filteredStacks()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  [ * / A ] All-Stacks Actions"))
	b.WriteString("\n")
	header := fmt.Sprintf("  %-20s %-8s %-7s %-11s %-9s %s", "STACK", "RUN/T", "KB", "IMG C/T", "RAM", "STATUS")
	b.WriteString(tuiYellowStyle.Render(header))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	vis := m.visibleRows() - 1
	if vis < 1 {
		vis = 1
	}
	end := m.scroll + vis
	if end > len(items) {
		end = len(items)
	}
	for i := m.scroll; i < end; i++ {
		s := items[i]
		missing := s.Total - s.Running - s.Stopped
		status, stStyle := "● UP", tuiRunningStyle
		if s.Running == 0 {
			status, stStyle = "■ DOWN", tuiStoppedStyle
		} else if missing > 0 {
			status, stStyle = "⚠ PARTIAL", tuiYellowStyle
		}
		sizeStr := fmt.Sprintf("%dK", s.SizeKB)
		if s.SizeKB >= 1000 {
			sizeStr = fmt.Sprintf("%dM", s.SizeKB/1000)
		}
		cached := 0
		for _, img := range s.Images {
			if m.data.ImgSizes[img] != "" {
				cached++
			} else {
				base := img
				if idx := strings.LastIndex(img, ":"); idx >= 0 {
					base = img[:idx]
				}
				if m.data.ImgSizes[base+":latest"] != "" {
					cached++
				}
			}
		}
		imgCell := fmt.Sprintf("%d/%d", cached, len(s.Images))
		ram := m.stackRAM(s)
		name := truncate(s.Name, 20)
		line := fmt.Sprintf("  %-20s %3d/%-4d %-7s %-11s %-9s %s", name, s.Running, s.Total, sizeStr, imgCell, ram, status)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate(line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(fmt.Sprintf("  %-20s %3d/%-4d ", name, s.Running, s.Total)))
			b.WriteString(tuiDimStyle.Render(fmt.Sprintf("%-7s ", sizeStr)))
			imgStyle := tuiRedStyle
			if len(s.Images) > 0 && cached == len(s.Images) {
				imgStyle = tuiGreenStyle
			} else if cached > 0 {
				imgStyle = tuiYellowStyle
			}
			b.WriteString(imgStyle.Render(fmt.Sprintf("%-11s ", imgCell)))
			b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("%-9s ", ram)))
			b.WriteString(stStyle.Render(status))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// stackRAM sums the used-memory of this stack's containers (best-effort parse).
func (m menuModel) stackRAM(s tuiStack) string {
	if s.Running == 0 {
		return ""
	}
	data, err := readFile(s.File)
	if err != nil {
		return ""
	}
	names := map[string]bool{}
	for _, mm := range tuiContainerNameRe.FindAllStringSubmatch(data, -1) {
		names[mm[1]] = true
	}
	var total float64
	for cname, mem := range m.data.MemStats {
		if names[cname] {
			used, _, ok := strings.Cut(mem, "/")
			if !ok {
				continue
			}
			used = strings.TrimSpace(used)
			total += tuiParseMiB(used)
		}
	}
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf("%.0fM", total)
}

func tuiParseMiB(s string) float64 {
	var v float64
	switch {
	case strings.HasSuffix(s, "GiB"):
		fmt.Sscanf(s, "%fGiB", &v)
		return v * 1024
	case strings.HasSuffix(s, "MiB"):
		fmt.Sscanf(s, "%fMiB", &v)
		return v
	case strings.HasSuffix(s, "KiB"):
		fmt.Sscanf(s, "%fKiB", &v)
		return v / 1024
	}
	return 0
}

func (m menuModel) handleStacksKey(k string) (tea.Model, tea.Cmd) {
	items := m.filteredStacks()
	if m.sel >= len(items) {
		m.sel = maxInt(0, len(items)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(items))
	case "enter", "tab":
		if len(items) == 0 {
			return m, nil
		}
		s := items[m.sel]
		m.popup = tuiActionPopup("Stack: "+truncate(s.Name, 24), tuiStackActions,
			func(action string) (menuModel, tea.Cmd) {
				return m.doStackAction(s.Name, action)
			})
	case "*", "A":
		m.popup = tuiActionPopup("★ ALL STACKS — every stack at once", tuiGlobalActions,
			func(action string) (menuModel, tea.Cmd) {
				return m.doGlobalAction(action)
			})
	}
	return m, nil
}

// tuiStackActions mirrors STACK_ACTIONS.
var tuiStackActions = []tuiAction{
	{"▶  Start", "up"},
	{"■  Stop", "down"},
	{"↺  Restart", "restart"},
	{"⟳  Recreate", "recreate"},
	{"✦  Fix", "fix"},
	{"✦  Repair", "repair"},
	{"⟳  Recreate + Up", "recreate_up"},
	{"◈  Repair + Recreate", "recreate_repair"},
	{"★  Fix + Repair + Recreate + Up", "full_repair"},
	{"◉  Repair + Fix + Recreate + Up", "deep_repair"},
	{"↑  Scale ON", "scale_on"},
	{"↓  Scale OFF", "scale_off"},
	{"↑  Proxy ON", "proxy_on"},
	{"↓  Proxy OFF", "proxy_off"},
	{"🎨  Art Inject", "art_inject"},
	{"🧹  Art Strip", "art_strip"},
	{"🛠  Build service into this stack…", "build_into"},
	{"✎  Rename stack", "rename"},
	{"🧹  Reclaim disk (unused images)…", "reclaim_menu"},
	{"🗑  Remove (down -v + archive file)", "remove"},
	{"✕  Cancel", ""},
}

// tuiGlobalActions mirrors GLOBAL_ACTIONS (1:1 with the Python menu).
var tuiGlobalActions = []tuiAction{
	{"▶  Up — ALL stacks", "up_all"},
	{"■  Down — ALL stacks", "down_all"},
	{"↺  Restart — ALL stacks", "restart_all"},
	{"⟳  Recreate — ALL stacks", "recreate_all"},
	{"⟳  Recreate + Up — ALL", "recreate_up_all"},
	{"✦  Fix — ALL stacks", "fix_all"},
	{"✦  Repair — ALL stacks", "repair_all"},
	{"◈  Repair + Recreate — ALL", "repair_recreate_all"},
	{"◈  Repair + Recreate + Up — ALL", "repair_recreate_up_all"},
	{"★  Fix + Repair + Recreate + Up — ALL", "full_repair_all"},
	{"◉  Repair + Fix + Recreate + Up — ALL", "deep_repair_all"},
	{"↑  Scale ON — all", "scale_on_all"},
	{"↓  Scale OFF — all", "scale_off_all"},
	{"↑  Proxy ON — all", "proxy_on_all"},
	{"↓  Proxy OFF — all", "proxy_off_all"},
	{"🧹  Reclaim disk (unused images)", "reclaim_menu"},
	{"✕  Cancel", ""},
}

// tuiReclaimActions mirrors RECLAIM_ACTIONS — the tiered reclaim submenu.
var tuiReclaimActions = []tuiAction{
	{"👁  Report — what's reclaimable", "reclaim_report"},
	{"🧹  Safe clean — unused + dangling", "reclaim_safe"},
	{"🔥  Aggressive — all but container-bound", "reclaim_aggressive"},
	{"💥  EVERYTHING — incl. in-use (force)", "reclaim_everything"},
	{"✕  Cancel", ""},
}

// openReclaimPopup shows the tiered reclaim submenu (shared by the ALL-stacks
// popup and the per-stack menu — mirrors Python's reclaim_menu).
func (m menuModel) openReclaimPopup() (menuModel, tea.Cmd) {
	m.popup = tuiActionPopup("🧹 Reclaim disk", tuiReclaimActions,
		func(a string) (menuModel, tea.Cmd) { return m.doGlobalAction(a) })
	return m, nil
}

func (m menuModel) doStackAction(name, action string) (menuModel, tea.Cmd) {
	switch action {
	case "", "cancel":
		return m, nil
	case "build_into":
		return m.doBuildAction("build_into")
	case "reclaim_menu":
		return m.openReclaimPopup()
	case "up":
		return m, tuiSelfCmd("Up "+name, "up", name)
	case "down":
		return m, tuiSelfCmd("Down "+name, "down", name)
	case "restart":
		return m, tuiSelfCmd("Restart "+name, "restart", name)
	case "recreate":
		return m, tuiSelfCmd("Recreate "+name, "up", name, "recreate")
	case "fix":
		return m, tuiSelfCmd("Fix "+name, "fix", name)
	case "repair":
		return m, tuiSelfCmd("Repair "+name, "fix", name, "repair")
	case "recreate_up":
		return m, tuiSelfCmd("Recreate+Up "+name, "up", name, "recreate")
	case "recreate_repair":
		return m, tuiSelfCmd("Repair+Up "+name, "up", name, "repair")
	case "full_repair":
		return m, tuiSelfCmd("Fix+Repair+Up "+name, "up", name, "repair", "fix")
	case "deep_repair":
		return m, tuiSelfCmd("Repair+Fix+Up "+name, "up", name, "repair", "fix")
	case "scale_on":
		return m, tuiSelfCmd("Scale ON "+name, "scale", name, "on")
	case "scale_off":
		return m, tuiSelfCmd("Scale OFF "+name, "scale", name, "off")
	case "proxy_on":
		return m, tuiSelfCmd("Proxy ON "+name, "proxy", name, "on")
	case "proxy_off":
		return m, tuiSelfCmd("Proxy OFF "+name, "proxy", name, "off")
	case "art_inject":
		return m, tuiSelfCmd("Art inject "+name, "art", "inject", name)
	case "art_strip":
		return m, tuiSelfCmd("Art strip "+name, "art", "strip", name)
	case "rename":
		m.popup = tuiInputPopup("Rename stack "+name, "New stack name:", name,
			func(newName string) (menuModel, tea.Cmd) {
				if newName == "" || newName == name || !tuiValidName(newName) {
					return m, nil
				}
				oldYml := stacksDir() + "/" + name + ".yml"
				newYml := stacksDir() + "/" + newName + ".yml"
				m.popup = tuiConfirmPopup("Rename "+name+" -> "+newName+"?", "✎  YES — down, rename, up",
					func() (menuModel, tea.Cmd) {
						return m, tuiDockerCmd("Rename "+name, func() string {
							down := cliSelf("down", name)
							ok, msg := tuiRenameStackFile(oldYml, newYml, newName)
							if !ok {
								return down + "\nRename failed: " + msg
							}
							up := cliSelf("up", newName)
							return down + "\n" + msg + "\n" + up
						})
					})
				return m, nil
			})
		return m, nil
	case "remove":
		m.popup = tuiConfirmPopup("Remove stack "+name+" + clean its networks?",
			"🗑  YES — down -v, archive .yml, purge orphan nets", func() (menuModel, tea.Cmd) {
				return m, tuiSelfCmd("Remove "+name, "purge", "stack", name, "--apply")
			})
		return m, nil
	}
	return m, nil
}

func (m menuModel) doGlobalAction(action string) (menuModel, tea.Cmd) {
	switch action {
	case "", "cancel":
		return m, nil
	case "up_all":
		return m, tuiSelfCmd("Up ALL", "up")
	case "down_all":
		return m, tuiSelfCmd("Down ALL", "down")
	case "restart_all":
		return m, tuiSelfCmd("Restart ALL", "restart")
	case "recreate_all":
		return m, tuiSelfCmd("Recreate ALL", "up", "recreate")
	case "fix_all":
		return m, tuiSelfCmd("Fix ALL", "fix", "all")
	case "repair_all":
		return m, tuiSelfCmd("Repair ALL", "fix", "all", "repair")
	case "full_repair_all":
		return m, tuiSelfCmd("Fix+Repair+Up ALL", "up", "repair", "fix")
	case "scale_on_all":
		return m, tuiSelfCmd("Scale ON all", "scale", "on")
	case "scale_off_all":
		return m, tuiSelfCmd("Scale OFF all", "scale", "off")
	case "proxy_on_all":
		return m, tuiSelfCmd("Proxy ON all", "proxy", "on")
	case "proxy_off_all":
		return m, tuiSelfCmd("Proxy OFF all", "proxy", "off")
	case "recreate_up_all":
		return m, tuiSelfCmd("Recreate+Up ALL", "up", "recreate")
	case "repair_recreate_all":
		return m, tuiSelfCmd("Repair+Recreate ALL", "up", "repair", "recreate")
	case "repair_recreate_up_all":
		return m, tuiSelfCmd("Repair+Recreate+Up ALL", "up", "repair", "recreate")
	case "deep_repair_all":
		return m, tuiSelfCmd("Repair+Fix+Recreate+Up ALL", "up", "repair", "fix", "recreate")
	case "reclaim_menu":
		return m.openReclaimPopup()
	case "reclaim_report":
		return m, tuiSelfCmd("Reclaim report", "reclaim", "report", "--all")
	case "reclaim_safe":
		return m, tuiSelfCmd("Reclaim safe", "reclaim", "clean", "--auto")
	case "reclaim_aggressive":
		return m, tuiSelfCmd("Reclaim aggressive", "reclaim", "clean", "--aggressive", "--auto")
	case "reclaim_everything":
		return m.tuiConfirmAndRun("Reclaim EVERYTHING — incl. in-use images?",
			"💥  YES — nuke all reclaimable images",
			"Reclaim everything", "reclaim", "clean", "--everything", "--auto")
	}
	return m, nil
}

// tuiConfirmAndRun shows a danger confirm, then runs a self-command on Yes.
func (m menuModel) tuiConfirmAndRun(title, danger, runTitle string, args ...string) (menuModel, tea.Cmd) {
	m.popup = tuiConfirmPopup(title, danger, func() (menuModel, tea.Cmd) {
		return m, tuiSelfCmd(runTitle, args...)
	})
	return m, nil
}
