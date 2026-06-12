package main

// menu_containers.go — Containers tab: live list with stack/status/memory/cache
// columns, "/" + A-Z filter, and the per-row action popup (Start/Stop/Restart/
// Recreate/Remove/Rename/Rollback/Inspect/scale/proxy/fix-combos).

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// filteredContainers applies the letter-jump + inline filter.
func (m menuModel) filteredContainers() []tuiContainer {
	var out []tuiContainer
	for _, c := range m.data.Containers {
		if m.fltLetter != "" && !tuiMatchLetter(c.Name, m.fltLetter) {
			continue
		}
		if m.fltInline != "" && !tuiContains([]string{c.Name, c.Image, c.Stack}, m.fltInline) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (m menuModel) renderContainers() string {
	items := m.filteredContainers()
	if m.sel >= len(items) {
		// clamp handled in key handler; guard here too
	}
	var b strings.Builder
	header := fmt.Sprintf("  %-26s %-12s %-10s %-19s %-9s %s", "NAME", "STACK", "STATUS", "MEMORY", "CACHE", "IMAGE")
	b.WriteString(tuiAccentStyle.Render(header))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(items) {
		end = len(items)
	}
	for i := m.scroll; i < end; i++ {
		c := items[i]
		running := strings.EqualFold(c.State, "running")
		ind := "○"
		if running {
			ind = "●"
		}
		mem := truncate(m.data.MemStats[c.Name], 18)
		szCell, cached := m.imageCache(c.Image)
		name := truncate(c.Name, 26)
		stack := truncate(c.Stack, 12)
		status := truncate(c.Status, 10)
		image := truncate(c.Image, maxInt(0, m.width-85))
		line := fmt.Sprintf("%s %-26s %-12s %-10s %-19s %-9s %s", ind, name, stack, status, mem, szCell, image)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate(line, m.width-2)))
		} else {
			indStyle := tuiStoppedStyle
			if running {
				indStyle = tuiRunningStyle
			}
			szStyle := tuiRedStyle
			if cached {
				szStyle = tuiCyanStyle
			}
			b.WriteString(indStyle.Render(ind) + " ")
			b.WriteString(tuiNormalStyle.Render(fmt.Sprintf("%-26s ", name)))
			b.WriteString(tuiGreenStyle.Render(fmt.Sprintf("%-12s ", stack)))
			b.WriteString(tuiDimStyle.Render(fmt.Sprintf("%-10s ", status)))
			b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("%-19s ", mem)))
			b.WriteString(szStyle.Render(fmt.Sprintf("%-9s ", szCell)))
			b.WriteString(tuiDimStyle.Render(image))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// imageCache mirrors the CACHE column: locally-present image -> size, else "↓ pull".
func (m menuModel) imageCache(image string) (string, bool) {
	sz := m.data.ImgSizes[image]
	if sz == "" {
		base := image
		if i := strings.LastIndex(image, ":"); i >= 0 {
			base = image[:i]
		}
		sz = m.data.ImgSizes[base+":latest"]
	}
	if sz != "" {
		return truncate(sz, 8), true
	}
	return "↓ pull", false
}

func (m menuModel) handleContainersKey(k string) (tea.Model, tea.Cmd) {
	items := m.filteredContainers()
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
		c := items[m.sel]
		stackFile := m.stackFileForContainer(c.Name)
		acts := tuiContainerActions
		if zeroScaleEnabled() {
			// insert "⚡ Zero Scale…" just above the trailing Cancel row
			n := len(tuiContainerActions)
			acts = append([]tuiAction{}, tuiContainerActions[:n-1]...)
			acts = append(acts, tuiAction{"⚡  Zero Scale…", "zeroscale"})
			acts = append(acts, tuiContainerActions[n-1])
		}
		m.popup = tuiActionPopup("Container: "+truncate(c.Name, 24), acts,
			func(action string) (menuModel, tea.Cmd) {
				return m.doContainerAction(c.Name, stackFile, action)
			})
	}
	return m, nil
}

// stackFileForContainer locates the .yml that declares container_name: <name>.
func (m menuModel) stackFileForContainer(name string) string {
	for _, s := range m.data.Stacks {
		if s.Name == m.containerStack(name) {
			return s.File
		}
	}
	// fall back to grepping files
	for _, s := range m.data.Stacks {
		data, err := readFile(s.File)
		if err == nil && strings.Contains(data, "container_name: "+name) {
			return s.File
		}
	}
	return ""
}

func (m menuModel) containerStack(name string) string {
	for _, c := range m.data.Containers {
		if c.Name == name {
			return c.Stack
		}
	}
	return ""
}

// tuiContainerActions mirrors CONTAINER_ACTIONS.
var tuiContainerActions = []tuiAction{
	{"▶  Start", "start"},
	{"■  Stop", "stop"},
	{"↺  Restart", "restart"},
	{"⟳  Recreate", "recreate"},
	{"⟳  Recreate + Up", "recreate_up"},
	{"✦  Fix (stack)", "fix"},
	{"✦  Repair (stack)", "repair"},
	{"✦  Fix + Repair (stack)", "fix_repair"},
	{"◐  Fix + Recreate + Up", "fix_recreate"},
	{"◓  Repair + Recreate + Up", "repair_recreate"},
	{"★  Fix + Repair + Recreate + Up", "full_repair"},
	{"◉  Repair + Fix + Recreate + Up", "deep_repair"},
	{"↑  Scale ON", "scale_on"},
	{"↓  Scale OFF", "scale_off"},
	{"↑  Proxy ON", "proxy_on"},
	{"↓  Proxy OFF", "proxy_off"},
	{"🔍  Inspect", "inspect"},
	{"⏪  Rollback image…", "rollback"},
	{"🌐  Edit IP", "edit_ip"},
	{"✎  Rename container", "rename"},
	{"🧹  Reclaim disk (unused images)…", "reclaim_menu"},
	{"🗑  Remove (force rm)", "remove"},
	{"✕  Cancel", ""},
}

// ipRe validates a dotted IPv4 (mirrors the Python edit_ip check).
var ipRe = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

// containerCurrentIP reads the container's ipv4_address from its stack file block.
func containerCurrentIP(stackFile, name string) string {
	data, err := readFile(stackFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(data, "\n")
	inBlock := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "container_name:") {
			inBlock = strings.TrimSpace(strings.TrimPrefix(t, "container_name:")) == name
			continue
		}
		if inBlock && strings.HasPrefix(t, "ipv4_address:") {
			return strings.TrimSpace(strings.TrimPrefix(t, "ipv4_address:"))
		}
	}
	return ""
}

// applyContainerIP rewrites the container's ipv4_address in its stack file block.
func applyContainerIP(stackFile, name, newIP string) bool {
	data, err := readFile(stackFile)
	if err != nil {
		return false
	}
	lines := strings.Split(data, "\n")
	inBlock, changed := false, false
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "container_name:") {
			inBlock = strings.TrimSpace(strings.TrimPrefix(t, "container_name:")) == name
			continue
		}
		if inBlock && strings.HasPrefix(t, "ipv4_address:") {
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, " "))]
			lines[i] = indent + "ipv4_address: " + newIP
			changed = true
			break
		}
	}
	if !changed {
		return false
	}
	return os.WriteFile(stackFile, []byte(strings.Join(lines, "\n")), 0644) == nil
}

func (m menuModel) doContainerAction(name, stackFile, action string) (menuModel, tea.Cmd) {
	stackName := ""
	if stackFile != "" {
		stackName = strings.TrimSuffix(baseName(stackFile), ".yml")
	}
	switch action {
	case "", "cancel":
		return m, nil
	case "zeroscale":
		return m.openZeroScalePopup(name)
	case "reclaim_menu":
		return m, tuiSelfCmd("Reclaim — "+name, "reclaim", "report", "--all")
	case "edit_ip":
		cur := containerCurrentIP(stackFile, name)
		def := cur
		if def == "" {
			def = "192.168.1."
		}
		m.popup = tuiInputPopup("Edit IP — "+truncate(name, 18), "New IP:", def,
			func(newIP string) (menuModel, tea.Cmd) {
				newIP = strings.TrimSpace(newIP)
				if newIP == "" || newIP == cur {
					return m, nil
				}
				if !ipRe.MatchString(newIP) {
					m.popup = tuiOutputPopup("Edit IP", []string{"Invalid IP: " + newIP})
					return m, nil
				}
				if stackFile != "" {
					applyContainerIP(stackFile, name, newIP)
				}
				if stackName != "" {
					return m, tuiSelfCmd("Edit IP "+name, "up", stackName, name, "recreate")
				}
				return m, tuiShellCmd("Restart "+name, "docker", "restart", name)
			})
		return m, nil
	case "start":
		return m, tuiDockerCmd("Start "+name, func() string {
			if startContainer(name) {
				return "Started " + name
			}
			return "Failed to start " + name
		})
	case "stop":
		return m, tuiDockerCmd("Stop "+name, func() string {
			if stopContainer(name, 30) {
				return "Stopped " + name
			}
			return "Failed to stop " + name
		})
	case "restart":
		return m, tuiShellCmd("Restart "+name, "docker", "restart", name)
	case "remove":
		m.popup = tuiConfirmPopup("Remove "+truncate(name, 24)+"?",
			"🗑  YES — rm + delete from stack + purge orphan nets", func() (menuModel, tea.Cmd) {
				if stackName != "" {
					return m, tuiSelfCmd("Remove "+name, "purge", "service", stackName, name, "--apply")
				}
				return m, tuiDockerCmd("Remove "+name, func() string {
					removeContainer(name, true, false)
					return "Removed " + name
				})
			})
		return m, nil
	case "inspect":
		m.popup = &tuiPopup{kind: tuiPopupDetail, title: "Inspect: " + truncate(name, 24), lines: tuiInspectLines(name)}
		return m, nil
	case "rename":
		if stackName == "" {
			m.popup = tuiOutputPopup("Rename", []string{name + " has no known stack — cannot rename in compose."})
			return m, nil
		}
		m.popup = tuiInputPopup("Rename "+truncate(name, 20), "New container name:", name,
			func(newName string) (menuModel, tea.Cmd) {
				if newName == "" || newName == name {
					return m, nil
				}
				if !tuiValidName(newName) {
					m.popup = tuiOutputPopup("Rename", []string{"Invalid name: " + newName})
					return m, nil
				}
				return m, tuiDockerCmd("Rename -> "+newName, func() string {
					tuiRenameContainerInFile(stackFile, name, newName)
					r := cli("rename", name, newName)
					if r.exitCode != 0 {
						return "compose updated; live rename skipped (" + strings.TrimSpace(r.stderr) + ")"
					}
					return "Renamed " + name + " -> " + newName
				})
			})
		return m, nil
	case "rollback":
		return m.doRollback(name, stackFile, stackName)
	// compose-backed combos -> shell to self
	case "recreate":
		if stackName != "" {
			return m, tuiSelfCmd("Recreate "+name, "up", stackName, name, "recreate")
		}
		return m, tuiShellCmd("Restart "+name, "docker", "restart", name)
	case "recreate_up":
		if stackName != "" {
			return m, tuiSelfCmd("Recreate+Up "+name, "up", stackName, name, "recreate")
		}
	case "fix":
		if stackName != "" {
			return m, tuiSelfCmd("Fix "+stackName, "fix", stackName)
		}
	case "repair":
		if stackName != "" {
			return m, tuiSelfCmd("Repair "+stackName, "fix", stackName, "repair")
		}
	case "fix_repair":
		if stackName != "" {
			return m, tuiSelfCmd("Fix+Repair "+stackName, "fix", stackName, "repair")
		}
	case "fix_recreate", "repair_recreate", "full_repair", "deep_repair":
		if stackName != "" {
			return m, tuiSelfCmd("Heal "+stackName, "up", stackName, name, "repair", "fix")
		}
	case "scale_on":
		if stackName != "" {
			return m, tuiSelfCmd("Scale ON "+name, "scale", stackName, name, "on")
		}
	case "scale_off":
		if stackName != "" {
			return m, tuiSelfCmd("Scale OFF "+name, "scale", stackName, name, "off")
		}
	case "proxy_on":
		if stackName != "" {
			return m, tuiSelfCmd("Proxy ON "+name, "proxy", stackName, name, "on")
		}
	case "proxy_off":
		if stackName != "" {
			return m, tuiSelfCmd("Proxy OFF "+name, "proxy", stackName, name, "off")
		}
	}
	return m, nil
}

// doRollback mirrors the rollback popup: list image versions, confirm, then pin.
func (m menuModel) doRollback(name, stackFile, stackName string) (menuModel, tea.Cmd) {
	image := tuiImageForContainer(stackFile, name)
	if image == "" {
		m.popup = tuiOutputPopup("Rollback", []string{"Could not determine the image for " + name + "."})
		return m, nil
	}
	vers := imageHistoryList(image)
	if len(vers) == 0 {
		m.popup = tuiOutputPopup("Rollback", []string{
			"No version history yet for " + image + ".",
			"History builds over time (snapshots on open + each pull/update).",
		})
		return m, nil
	}
	var acts []tuiAction
	for _, v := range vers {
		when := tuiFmtTime(v.LastSeen)
		mark := "  "
		cur := ""
		if v.Current {
			mark = "● "
			cur = "  (current)"
		}
		acts = append(acts, tuiAction{fmt.Sprintf("%s%s   %s%s", mark, v.Short, when, cur), v.Digest})
	}
	acts = append(acts, tuiAction{"✕  Cancel", ""})
	m.popup = tuiActionPopup("Rollback "+truncate(image, 26), acts, func(digest string) (menuModel, tea.Cmd) {
		if digest == "" {
			return m, nil
		}
		for _, v := range vers {
			if v.Digest == digest && v.Current {
				m.popup = tuiOutputPopup("Rollback", []string{"That version is already running."})
				return m, nil
			}
		}
		m.popup = tuiConfirmPopup("Roll "+truncate(image, 18)+" back to "+imageHistoryShort(digest)+"?",
			"⏪  YES — pin + recreate", func() (menuModel, tea.Cmd) {
				return m, tuiDockerCmd("Rollback "+imageHistoryShort(digest), func() string {
					ok, msg := imageHistoryRollback(image, digest)
					out := msg
					if ok && stackName != "" {
						r := cliSelf("up", stackName, name, "recreate")
						out += "\n" + r
					}
					return out
				})
			})
		return m, nil
	})
	return m, nil
}
