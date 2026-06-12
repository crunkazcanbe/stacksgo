package main

// menu_art.go — Art tab: a static list of art/dynamics maintenance actions
// (inject/strip art across all stacks or dynamics, edit art.conf / stack_urls.conf,
// regenerate dynamics, repair dynamics). Mirrors ART_ITEMS / draw_art_tab.

import (
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var tuiArtItems = []tuiAction{
	{"Inject art into ALL stacks", "art_inject_all"},
	{"Strip art from ALL stacks", "art_strip_all"},
	{"Inject art into ALL dynamics", "art_inject_dyn"},
	{"Strip art from ALL dynamics", "art_strip_dyn"},
	{"View art.conf", "view_art"},
	{"View stack_urls.conf", "view_urls"},
	{"Edit art.conf (in $EDITOR)", "edit_art"},
	{"Edit stack_urls.conf (in $EDITOR)", "edit_urls"},
	{"Generate dynamics from ALL stacks", "gen_dyn_all"},
	{"Force regenerate ALL dynamics", "gen_dyn_force"},
	{"Repair ALL dynamic configs", "repair_dyn"},
}

func (m menuModel) renderArt() string {
	return tuiRenderActionList("ART & DYNAMICS", tuiArtItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleArtKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiArtItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiArtItems) {
			return m, nil
		}
		return m.doArtAction(tuiArtItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doArtAction(action string) (menuModel, tea.Cmd) {
	switch action {
	case "art_inject_all":
		return m, tuiSelfCmd("Art inject ALL", "art", "inject", "all")
	case "art_strip_all":
		return m, tuiSelfCmd("Art strip ALL", "art", "strip", "all")
	case "art_inject_dyn":
		return m, tuiSelfCmd("Art inject dynamics", "art", "dynamic", "inject", "all")
	case "art_strip_dyn":
		return m, tuiSelfCmd("Art strip dynamics", "art", "dynamic", "strip", "all")
	case "view_art":
		p := filepath.Join(configDir(), "art.conf")
		return m, tuiDockerCmd("art.conf", func() string { return tuiTailFile(p, 400) })
	case "view_urls":
		p := filepath.Join(configDir(), "stack_urls.conf")
		return m, tuiDockerCmd("stack_urls.conf", func() string { return tuiTailFile(p, 400) })
	case "edit_art":
		return m, tuiEditFile(filepath.Join(configDir(), "art.conf"))
	case "edit_urls":
		return m, tuiEditFile(filepath.Join(configDir(), "stack_urls.conf"))
	case "gen_dyn_all":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "gen_dyn_force":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	case "repair_dyn":
		return m, tuiSelfCmd("Repair ALL dynamics", "dynamics", "repair", "all")
	}
	return m, nil
}

// tuiRenderActionList draws a simple ▶-cursored static action list (shared by the
// Art / Backup / Build tabs). Mirrors the curses draw_*_tab loops.
func tuiRenderActionList(title string, items []tuiAction, sel, scroll, vis, width int) string {
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  " + title))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, width-4))))
	b.WriteString("\n")
	end := scroll + vis
	if end > len(items) {
		end = len(items)
	}
	for i := scroll; i < end; i++ {
		label := items[i].Label
		if i == sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶  "+label, width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(truncate("     "+label, width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}
