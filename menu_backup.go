package main

// menu_backup.go — Backup tab: run a full backup, take a pre-backup snapshot,
// clean old backups, and view the engine log files. Mirrors BACKUP_ITEMS /
// draw_backup_tab, wired to the Go backup + snapshot engines.

import (
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

// Order mirrors the Python BACKUP_ITEMS exactly; the "Clean old backups" entry
// is a Go-only extra appended at the end so the shared rows line up 1:1.
var tuiBackupItems = []tuiAction{
	{"Run full backup now", "backup_full"},
	{"Run pre-backup snapshot", "backup_pre"},
	{"View backup log", "view_backup_log"},
	{"View stacks up log", "view_up_log"},
	{"View stacks fix log", "view_fix_log"},
	{"View stacks build log", "view_build_log"},
	{"Restore from backup", "backup_restore"},
	{"Clean old backups", "backup_clean"},
}

func (m menuModel) renderBackup() string {
	return tuiRenderActionList("BACKUP & LOGS", tuiBackupItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleBackupKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiBackupItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiBackupItems) {
			return m, nil
		}
		return m.doBackupAction(tuiBackupItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doBackupAction(action string) (menuModel, tea.Cmd) {
	dataDir := dispDataDir()
	// Prefer the logs folder; fall back to the data dir for any legacy log.
	pick := func(name string) string {
		if p := logPath(name); func() bool { _, e := os.Stat(p); return e == nil }() {
			return p
		}
		return filepath.Join(dataDir, name)
	}
	switch action {
	case "backup_full":
		return m, tuiSelfCmd("Full backup", "backup", "all")
	case "backup_pre":
		return m, tuiSelfCmd("Pre-backup snapshot", "snapshot")
	case "backup_restore":
		return m, tuiSelfCmd("Restore from backup", "backup", "restore")
	case "backup_clean":
		return m, tuiSelfCmd("Clean backups", "backup", "clean")
	case "view_backup_log":
		return m, tuiViewLog("backup log", pick("stacks_backup.log"))
	case "view_up_log":
		return m, tuiViewLog("up log", pick("stacks_up.log"))
	case "view_fix_log":
		return m, tuiViewLog("fix log", pick("stacks_fix.log"))
	case "view_build_log":
		return m, tuiViewLog("build log", pick("stacks_build.log"))
	}
	return m, nil
}

// tuiViewLog shows the tail of a log file in an output popup.
func tuiViewLog(title, path string) tea.Cmd {
	return tuiDockerCmd(title, func() string { return tuiTailFile(path, 400) })
}
