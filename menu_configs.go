package main

// menu_configs.go — Configs tab: lists the config files in configDir() plus the
// descriptions/ folder, with sizes; ENTER shows a file's content in a scrollable
// output popup. Mirrors get_config_items / draw_configs_tab. (Inline editing is
// left to `EDITOR` outside the TUI; the altscreen TUI shows the contents.)

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type tuiConfigRow struct {
	Label  string
	Path   string
	IsDir  bool
	SizeKB int64
}

// tuiConfigFiles mirrors CONFIG_FILES (the well-known config filenames).
var tuiConfigFiles = []string{
	"stacks.conf", "build.conf", "all_services.txt", "global_inject.conf",
	"menu.conf", "stack_urls.conf", "backup.conf", "art.conf",
}

func tuiConfigRows() []tuiConfigRow {
	dir := configDir()
	var rows []tuiConfigRow
	for _, fn := range tuiConfigFiles {
		p := filepath.Join(dir, fn)
		var kb int64 = -1
		if st, err := os.Stat(p); err == nil {
			kb = st.Size() / 1024
			if kb < 1 {
				kb = 1
			}
		}
		rows = append(rows, tuiConfigRow{Label: fn, Path: p, SizeKB: kb})
	}
	// descriptions/ folder
	descDir := filepath.Join(dir, "descriptions")
	if entries, err := os.ReadDir(descDir); err == nil {
		var descFiles []string
		var total int64
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
				descFiles = append(descFiles, e.Name())
				if st, err := os.Stat(filepath.Join(descDir, e.Name())); err == nil {
					total += st.Size()
				}
			}
		}
		sort.Strings(descFiles)
		rows = append(rows, tuiConfigRow{
			Label:  fmt.Sprintf("📁 descriptions/  (%d files, %dK)", len(descFiles), maxInt64(1, total/1024)),
			Path:   descDir, IsDir: true, SizeKB: total / 1024,
		})
		for _, f := range descFiles {
			p := filepath.Join(descDir, f)
			var kb int64 = 1
			if st, err := os.Stat(p); err == nil {
				kb = maxInt64(1, st.Size()/1024)
			}
			rows = append(rows, tuiConfigRow{Label: "   " + f, Path: p, SizeKB: kb})
		}
	}
	return rows
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (m menuModel) renderConfigs() string {
	rows := tuiConfigRows()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  CONFIGS — ENTER shows the file content"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		sz := "—"
		if r.SizeKB >= 0 {
			sz = fmt.Sprintf("%dK", r.SizeKB)
		}
		line := fmt.Sprintf("%-40s %5s", truncate(r.Label, 40), sz)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶ "+line, m.width-2)))
		} else {
			st := tuiNormalStyle
			if r.IsDir {
				st = tuiAccentStyle
			}
			b.WriteString(st.Render(truncate("    "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m menuModel) handleConfigsKey(k string) (tea.Model, tea.Cmd) {
	rows := tuiConfigRows()
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		if r.IsDir {
			return m, nil
		}
		return m, tuiDockerCmd(r.Label, func() string {
			if r.SizeKB < 0 {
				return "(file does not exist yet: " + r.Path + ")"
			}
			return tuiTailFile(r.Path, 600)
		})
	case "e", "E":
		// open the selected config file in $EDITOR (mirrors the Python menu)
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		if r.IsDir {
			return m, nil
		}
		return m, tuiEditFile(r.Path)
	}
	return m, nil
}
