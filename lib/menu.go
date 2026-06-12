package lib

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"gopkg.in/yaml.v3"
)

// ===== from menu_tui.go =====

// menu_tui.go — the Go TUI (Bubble Tea + Lip Gloss) replacement for
// stacks_menu.py. This file holds the styles, the data layer, the top-level
// model + Update/View dispatch, and the tab bar. Per-tab rendering and the
// action popups live in the menu_*.go siblings.
//
// All identifiers are tui*/menu* prefixed to avoid colliding with the ~38
// existing engine files. Universal paths only (configDir/stacksDir/etc).

// ── Styles (mirror the curses color pairs) ───────────────────────────────────
var (
	tuiHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Background(lipgloss.Color("17")).Bold(true)
	tuiNormalStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	tuiSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("75"))
	tuiAccentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	tuiDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tuiGreenStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	tuiRedStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	tuiYellowStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	tuiCyanStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	tuiRunningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	tuiStoppedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	tuiTabActive     = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("75"))
	tuiTabInactive   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tuiPopupBorder   = lipgloss.NewStyle().Foreground(lipgloss.Color("135"))
	tuiPopupSel      = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("135"))
)

// ── Tab identifiers ──────────────────────────────────────────────────────────
const (
	tabContainers = iota
	tabStacks
	tabLogs
	tabDynamics
	tabArt
	tabBackup
	tabBuild
	tabConfigs
	tabNetwork
	tabUpdates
	tabSettings
	tabUpgrade
)

var tuiTabs = []string{
	"Containers", "Stacks", "Logs", "Dynamics", "Art", "Backup",
	"Build", "Configs", "Network", "Updates", "Settings", "Upgrade",
}

var tuiFilterAlpha = "abcdefghijklmnopqrstuvwxyz#"

// ── Data records ─────────────────────────────────────────────────────────────

// tuiContainer mirrors a Containers-tab row.
type tuiContainer struct {
	Name   string
	State  string
	Status string
	Image  string
	Stack  string
}

// tuiStack mirrors a Stacks-tab row.
type tuiStack struct {
	Name    string
	Running int
	Stopped int
	Total   int
	File    string
	SizeKB  int64
	Images  []string
}

// tuiData is the shared live snapshot, refreshed in the background.
type tuiData struct {
	Containers []tuiContainer
	Stacks     []tuiStack
	MemStats   map[string]string // container -> "used / limit"
	ImgSizes   map[string]string // image -> size string
	Net        *tuiNetData       // IP/port collision scan (heavy — done in bg collect, not in render)
	LastUpdate time.Time
}

// tuiDataMsg carries a fresh snapshot to the model.
type tuiDataMsg struct{ data tuiData }

// tuiTickMsg drives the periodic refresh + clock.
type tuiTickMsg time.Time

// tuiActionDoneMsg signals a shelled-out action finished (output captured).
type tuiActionDoneMsg struct {
	title  string
	output string
}

// ── Background data collection ───────────────────────────────────────────────

var tuiServiceKeyRe = regexp.MustCompile(`(?m)^\s{2}[a-zA-Z0-9_-]+:\s*$`)
var tuiImageRe = regexp.MustCompile(`(?m)^\s+image:\s*(\S+)`)
var tuiContainerNameRe = regexp.MustCompile(`container_name:\s*(\S+)`)

// tuiCollect builds a full data snapshot from the Docker API + stack files.
func tuiCollect() tuiData {
	d := tuiData{
		MemStats:   map[string]string{},
		ImgSizes:   map[string]string{},
		LastUpdate: time.Now(),
	}

	// Image sizes (image:tag -> human size).
	for _, im := range dockerImages() {
		sz := tuiHumanBytes(im.Size)
		for _, rt := range im.RepoTags {
			if rt != "<none>:<none>" {
				d.ImgSizes[rt] = sz
			}
		}
	}

	// Containers via the shared container layer.
	info := containerInfo()
	memRaw := tuiMemStats()
	d.MemStats = memRaw
	var running, others []tuiContainer
	for name, ci := range info {
		c := tuiContainer{
			Name:   name,
			State:  ci.State,
			Status: ci.State,
			Image:  ci.Image,
			Stack:  ci.Project,
		}
		if strings.EqualFold(ci.State, "running") {
			running = append(running, c)
		} else {
			others = append(others, c)
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].Name < running[j].Name })
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	d.Containers = append(running, others...)

	// Stacks from the .yml files in stacksDir.
	d.Stacks = tuiScanStacks(info)

	// Network collision scan (heavy: 172 networks) — done HERE in the background
	// collect, never in the render path, so the Network tab can't freeze the UI.
	d.Net = tuiCollectNetData()
	return d
}

// tuiCollectNetData runs the IP/port collision scan once (was inline in the
// render path via ensureNetData, which re-ran it every frame → froze the menu).
func tuiCollectNetData() *tuiNetData {
	ip, port := getCollisions()
	return &tuiNetData{
		ipCol:   ip,
		portCol: port,
		ipMap:   scanAllIPs(),
		portMap: scanAllPorts(),
		nextIP:  getNextAvailableIP(),
		conf:    collisionLoadConf(),
	}
}

// tuiMemStats fetches per-container memory usage. The Engine API has no batch
// "stats" endpoint comparable to `docker stats`, so we shell out once (cheap,
// background) the same way the Python menu did.
func tuiMemStats() map[string]string {
	out := map[string]string{}
	cmd := exec.Command("docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}")
	cmd.Env = dockerEnv()
	b, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if n, m, ok := strings.Cut(line, "\t"); ok {
			out[strings.TrimSpace(n)] = strings.TrimSpace(m)
		}
	}
	return out
}

// tuiScanStacks mirrors get_stacks(): one row per <name>.yml in stacksDir.
func tuiScanStacks(info map[string]ctrInfo) []tuiStack {
	dir := stacksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var stacks []tuiStack
	for _, fname := range names {
		name := strings.TrimSuffix(fname, ".yml")
		path := filepath.Join(dir, fname)
		content, _ := os.ReadFile(path)
		text := string(content)

		total := len(tuiServiceKeyRe.FindAllString(text, -1))
		var images []string
		for _, m := range tuiImageRe.FindAllStringSubmatch(text, -1) {
			images = append(images, m[1])
		}

		// Count running/stopped by matching this project name on live containers.
		running, stopped := 0, 0
		for _, ci := range info {
			if ci.Project == name {
				if strings.EqualFold(ci.State, "running") {
					running++
				} else {
					stopped++
				}
			}
		}

		var sizeKB int64
		if st, e := os.Stat(path); e == nil {
			sizeKB = st.Size() / 1024
		}
		stacks = append(stacks, tuiStack{
			Name: name, Running: running, Stopped: stopped, Total: total,
			File: path, SizeKB: sizeKB, Images: images,
		})
	}
	return stacks
}

// tuiHumanBytes renders a byte count like docker's image size column.
func tuiHumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fkB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ── Commands ─────────────────────────────────────────────────────────────────

// tuiRefreshCmd runs ONE data collect and returns it. It must never be fired
// while a previous refresh is still in flight — overlapping collects contend on
// Docker (docker stats + API) and deadlock. The model chains the next refresh
// only after tuiDataMsg arrives (see Update), guaranteeing single-in-flight.
func tuiRefreshCmd() tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if recover() != nil {
				// Never let a collect panic wedge the menu — recover and keep
				// the previous snapshot (empty here; next tick retries).
				msg = tuiDataMsg{data: tuiData{}}
			}
		}()
		return tuiDataMsg{data: tuiCollect()}
	}
}

// tuiRefreshTickMsg fires after a delay to kick off the NEXT (non-overlapping)
// refresh. tuiRefreshAfter schedules it.
type tuiRefreshTickMsg time.Time

func tuiRefreshAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tuiRefreshTickMsg(t) })
}

// Loading-splash animation: a fast tick that runs ONLY until the first data
// snapshot lands, driving the trans-flag shimmer on the loading screen so the
// menu never shows an empty frame during the ~10s first collect.
type tuiLoadingTickMsg struct{}

func tuiLoadingTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return tuiLoadingTickMsg{} })
}

func tuiTickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tuiTickMsg(t) })
}

// ── Model ────────────────────────────────────────────────────────────────────

type menuModel struct {
	data   tuiData
	width  int
	height int
	tab    int
	now    time.Time

	// per-tab cursor/scroll
	sel    int
	scroll int

	// per-tab letter/inline filter (Containers, Updates)
	fltLetter string // "" or "a".."z" or "#"
	fltInline string
	fltMode   bool

	// settings list cache + scroll
	settings    []tuiSetting
	netCache    *tuiNetData
	netCacheTS  time.Time
	updateRows  []tuiUpdateRow
	updateSum   tuiUpdateSummary
	updateDirty bool

	// popup state (action menu / confirm / text input / output)
	popup *tuiPopup

	refreshing bool // true while a data collect is in flight (prevents overlap)
	loadFrame  int  // animation frame for the loading splash (trans-flag shimmer)
	quit       bool
}

func tuiFilterableTab(tab int) bool {
	return tab == tabContainers || tab == tabStacks || tab == tabUpdates
}

func (m menuModel) Init() tea.Cmd {
	return tea.Batch(tuiRefreshCmd(), tuiTickCmd(), tuiLoadingTick(), tuiLogDumpCmd())
}

func (m menuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Force a full repaint on resize — when the on-screen keyboard slides
		// in/out the terminal resizes, and without clearing, stale characters
		// from the old size get left behind (scrambled). ClearScreen fixes it.
		m.width, m.height = msg.Width, msg.Height
		return m, tea.ClearScreen

	case tuiTickMsg:
		// Clock only — does NOT trigger a data refresh (refreshes are chained
		// off tuiDataMsg so only one ever runs at a time).
		m.now = time.Time(msg)
		if m.tab == tabUpdates {
			m.updateRows, m.updateSum = tuiBuildUpdateRows()
		}
		return m, tuiTickCmd()

	case tuiDataMsg:
		m.data = msg.data
		m.refreshing = false
		// Schedule the next refresh AFTER this one landed → never overlaps.
		return m, tuiRefreshAfter(8 * time.Second)

	case tuiRefreshTickMsg:
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		return m, tuiRefreshCmd()

	case tuiLoadingTickMsg:
		if !m.data.LastUpdate.IsZero() {
			return m, nil // first data landed — stop the splash animation
		}
		m.loadFrame++
		return m, tuiLoadingTick()

	case tuiActionDoneMsg:
		m.popup = &tuiPopup{
			kind:  tuiPopupOutput,
			title: msg.title,
			lines: strings.Split(strings.TrimRight(msg.output, "\n"), "\n"),
		}
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		return m, tuiRefreshCmd()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// loadingView is the splash shown during the first data collect: the STACKS
// word-logo in trans-flag colors that shimmer (colors wave down the lines each
// frame), a Docker whale + moving loading bar, and the version bottom-left —
// the loading screen the Python menu had, so the menu never looks empty/broken.
func (m menuModel) loadingView() string {
	// The trans-pride Docker whale — identical art to the Python menu, with the
	// same flowing per-column color wave (blue · pink · white · pink · blue).
	art := []string{
		"                  ##        .",
		"            ## ## ##       ==",
		"         ## ## ## ## ##   ===",
		"     /=====================\\___/ ===",
		" ~~ {~~  ~~~~  ~~~  ~~~~  ~~ ~ /   ===-  ~~~",
		"     \\______ o            __/",
		"      \\      \\         __/",
		"       \\      \\______ __/",
		"        \\_______________/",
	}
	pal := []int{117, 218, 231, 218, 117} // trans flag: blue · pink · white · pink · blue
	w, h := m.width, m.height
	artW := 0
	for _, l := range art {
		if len(l) > artW {
			artW = len(l)
		}
	}
	pad := func(vis int) string {
		if w <= vis {
			return ""
		}
		return strings.Repeat(" ", (w-vis)/2)
	}
	var b strings.Builder
	top := (h - 14) / 2
	for i := 0; i < top; i++ {
		b.WriteString("\n")
	}
	for _, line := range art {
		b.WriteString(pad(artW))
		for col, ch := range line {
			if ch == ' ' {
				b.WriteString(" ")
				continue
			}
			band := ((col*len(pal))/artW - m.loadFrame/2) % len(pal)
			if band < 0 {
				band += len(pal)
			}
			b.WriteString(fmt.Sprintf("\x1b[38;5;%dm%c\x1b[0m", pal[band], ch))
		}
		b.WriteString("\n")
	}
	// "stacks" label centered under the whale, in pink — like the Python splash.
	label := "stacks"
	b.WriteString(pad(len(label)))
	b.WriteString(fmt.Sprintf("\x1b[38;5;218m%s\x1b[0m\n\n", label))
	// Docker whale + a moving block on the loading bar.
	const bw = 26
	pos := m.loadFrame % bw
	var bar strings.Builder
	for i := 0; i < bw; i++ {
		if i == pos || i == (pos+1)%bw {
			bar.WriteString("█")
		} else {
			bar.WriteString("─")
		}
	}
	barLine := "🐳  " + bar.String()
	b.WriteString(pad(bw + 4))
	b.WriteString(fmt.Sprintf("\x1b[38;5;218m%s\x1b[0m\n\n", barLine))
	msg := "loading your docker stacks…"
	b.WriteString(pad(len(msg)))
	b.WriteString(fmt.Sprintf("\x1b[38;5;245m%s\x1b[0m\n", msg))
	// Version bottom-left, like the splash banner.
	fill := h - top - 12
	for i := 0; i < fill; i++ {
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("\x1b[38;5;245m %s\x1b[0m", stacksVersion()))
	return b.String()
}

func (m menuModel) View() string {
	if m.quit {
		return ""
	}
	if m.width == 0 {
		return "loading…"
	}
	if m.data.LastUpdate.IsZero() {
		return m.loadingView() // splash until the first collect lands
	}
	var b strings.Builder

	// Header
	nr := 0
	for _, c := range m.data.Containers {
		if strings.EqualFold(c.State, "running") {
			nr++
		}
	}
	now := m.now
	if now.IsZero() {
		now = time.Now()
	}
	title := fmt.Sprintf("  ✦ STACKS  ·  %d/%d running  ·  %s  ", nr, len(m.data.Containers), now.Format("15:04:05"))
	b.WriteString(tuiHeaderStyle.Width(m.width).Render(tuiCenter(title, m.width)))
	b.WriteString("\n")

	// Tab bar + divider
	b.WriteString(tuiRenderTabs(m.tab, m.width))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render(strings.Repeat("─", m.width)))
	b.WriteString("\n")

	// Filter bar (filterable tabs)
	if tuiFilterableTab(m.tab) {
		b.WriteString(m.renderFilterBar())
		b.WriteString("\n")
	} else {
		b.WriteString("\n")
	}

	// Body
	body := ""
	switch m.tab {
	case tabContainers:
		body = m.renderContainers()
	case tabStacks:
		body = m.renderStacks()
	case tabLogs:
		body = m.renderLogs()
	case tabDynamics:
		body = m.renderDynamics()
	case tabArt:
		body = m.renderArt()
	case tabBackup:
		body = m.renderBackup()
	case tabBuild:
		body = m.renderBuild()
	case tabConfigs:
		body = m.renderConfigs()
	case tabNetwork:
		body = m.renderNetwork()
	case tabUpdates:
		body = m.renderUpdates()
	case tabSettings:
		body = m.renderSettings()
	case tabUpgrade:
		body = m.renderUpgrade()
	}
	b.WriteString(body)

	// Footer
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Join(m.footerHints(), "  ")))

	view := b.String()

	// Popup = a snug box composited ON TOP of the live screen, so the container
	// list stays visible behind it (not a blank modal).
	if m.popup != nil {
		return overlayCenter(view, m.popup.render(m.width), m.width, m.height)
	}
	return view
}

func (m menuModel) footerHints() []string {
	switch m.tab {
	case tabContainers:
		return []string{"↑↓ Nav", "←→ Tabs", "a-z Jump", "/ Search", "ENTER Menu", "q Quit"}
	case tabStacks:
		return []string{"↑↓ Nav", "←→ Tabs", "a-z Jump", "/ Search", "ENTER 1-Stack", "* ALL-Stacks", "q Quit"}
	case tabLogs:
		return []string{"↑↓ Nav", "ENTER View", "←→ Tabs", "q Quit"}
	case tabDynamics:
		return []string{"↑↓ Nav", "ENTER View", "a Actions", "g Gen ALL", "←→ Tabs", "q Quit"}
	case tabArt:
		return []string{"↑↓ Nav", "ENTER Run", "←→ Tabs", "q Quit"}
	case tabBackup:
		return []string{"↑↓ Nav", "ENTER Run", "←→ Tabs", "q Quit"}
	case tabBuild:
		return []string{"↑↓ Nav", "ENTER Run", "←→ Tabs", "q Quit"}
	case tabConfigs:
		return []string{"↑↓ Nav", "ENTER View", "e Edit", "←→ Tabs", "q Quit"}
	case tabNetwork:
		return []string{"a Edit", "s Scan", "d Dedupe", "e YAML", "←→ Tabs", "q Quit"}
	case tabUpdates:
		return []string{"↑↓ Nav", "a-z Jump", "/ Search", "ENTER Detail", "C Check", "P Pull", "q Quit"}
	case tabSettings:
		return []string{"↑↓ Nav", "ENTER Toggle/Edit", "←→ Tabs", "q Quit"}
	case tabUpgrade:
		return []string{"ENTER Update", "r Re-check", "←→ Tabs", "q Quit"}
	}
	return []string{"q Quit"}
}

// tuiCenter centers a string within width (truncating if needed).
func tuiCenter(s string, width int) string {
	if len(s) >= width {
		if width <= 0 {
			return ""
		}
		return s[:width]
	}
	pad := (width - len(s)) / 2
	return strings.Repeat(" ", pad) + s + strings.Repeat(" ", width-len(s)-pad)
}

// tuiRenderTabs draws the tab bar WINDOWED to the terminal width so the active
// tab is always on-screen (the full 12-tab row overflowed narrow handheld
// screens and the selected tab scrolled off). Shows ‹ / › when tabs are hidden.
func tuiRenderTabs(active, width int) string {
	n := len(tuiTabs)
	if width <= 0 {
		width = 80
	}
	labels := make([]string, n)
	lw := make([]int, n)
	for i, t := range tuiTabs {
		labels[i] = " " + t + " "
		lw[i] = len(labels[i])
	}
	// Reserve room for the leading space + both ‹ › arrows + separators (~7).
	budget := width - 7
	if budget < lw[active] {
		budget = lw[active]
	}
	lo, hi := active, active
	used := lw[active]
	for {
		grew := false
		if hi+1 < n && used+1+lw[hi+1] <= budget {
			used += 1 + lw[hi+1]
			hi++
			grew = true
		}
		if lo-1 >= 0 && used+1+lw[lo-1] <= budget {
			used += 1 + lw[lo-1]
			lo--
			grew = true
		}
		if !grew {
			break
		}
	}
	var parts []string
	if lo > 0 {
		parts = append(parts, tuiDimStyle.Render("‹"))
	}
	for i := lo; i <= hi; i++ {
		if i == active {
			parts = append(parts, tuiTabActive.Render(labels[i]))
		} else {
			parts = append(parts, tuiTabInactive.Render(labels[i]))
		}
	}
	if hi < n-1 {
		parts = append(parts, tuiDimStyle.Render("›"))
	}
	return " " + strings.Join(parts, " ")
}

// renderFilterBar mirrors draw_filter_bar: the A-Z band or the live "/" box.
func (m menuModel) renderFilterBar() string {
	shown, total := m.filterCounts()
	if m.fltMode {
		return tuiYellowStyle.Render(fmt.Sprintf("  / %s_   [%d/%d]", m.fltInline, shown, total))
	}
	var b strings.Builder
	b.WriteString("  ")
	for _, ch := range tuiFilterAlpha {
		c := string(ch)
		if m.fltLetter == c {
			b.WriteString(tuiSelectedStyle.Render(c))
		} else {
			b.WriteString(tuiAccentStyle.Render(c))
		}
		b.WriteString(" ")
	}
	tail := fmt.Sprintf(" [%d/%d]", shown, total)
	if m.fltInline != "" {
		tail = fmt.Sprintf(" /%s [%d/%d]", m.fltInline, shown, total)
	}
	b.WriteString(tuiDimStyle.Render(tail))
	return b.String()
}

func (m menuModel) filterCounts() (int, int) {
	switch m.tab {
	case tabContainers:
		return len(m.filteredContainers()), len(m.data.Containers)
	case tabStacks:
		return len(m.filteredStacks()), len(m.data.Stacks)
	case tabUpdates:
		return len(m.filteredUpdateRows()), len(m.updateRows)
	}
	return 0, 0
}

// tuiMatchLetter mirrors the leading-letter jump (with the leading-symbol strip).
func tuiMatchLetter(s, letter string) bool {
	s = strings.ToLower(strings.TrimLeft(s, "●○■⚠ "))
	if letter == "#" {
		if s == "" {
			return false
		}
		c := s[0]
		return !(c >= 'a' && c <= 'z')
	}
	return strings.HasPrefix(s, letter)
}

func tuiContains(fields []string, sub string) bool {
	sub = strings.ToLower(sub)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), sub) {
			return true
		}
	}
	return false
}

// menuRun is the entry point invoked by cmdMenu.
func menuRun() error {
	p := tea.NewProgram(menuModel{now: time.Now()}, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ===== from menu_containers.go =====

// menu_containers.go — Containers tab: live list with stack/status/memory/cache
// columns, "/" + A-Z filter, and the per-row action popup (Start/Stop/Restart/
// Recreate/Remove/Rename/Rollback/Inspect/scale/proxy/fix-combos).

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
		if zeroScaleAvailable() {
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
	{"⚡  Fix (FORCE)", "fix_force"},
	{"⚡  Repair (FORCE)", "repair_force"},
	{"⚡  Fix + Repair + Recreate + Up (FORCE)", "full_repair_force"},
	{"⚡  Repair + Fix + Recreate + Up (FORCE)", "deep_repair_force"},
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
	case "fix_force":
		if stackName != "" {
			return m, tuiSelfCmd("Fix (force) "+stackName, "fix", stackName, "force")
		}
	case "repair_force":
		if stackName != "" {
			return m, tuiSelfCmd("Repair (force) "+stackName, "fix", stackName, "repair", "force")
		}
	case "full_repair_force", "deep_repair_force":
		if stackName != "" {
			return m, tuiSelfCmd("Heal (force) "+stackName, "up", stackName, name, "recreate", "repair", "fix", "force")
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

// ===== from menu_stacks.go =====

// menu_stacks.go — Stacks tab: one row per stack file with run/total counts,
// file size, image cached-count, RAM, and a UP/PARTIAL/DOWN status. ENTER/TAB
// opens the per-stack action popup; "*"/"A" opens the ALL-STACKS popup.

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
	{"⚡  Fix (FORCE)", "fix_force"},
	{"⚡  Repair (FORCE)", "repair_force"},
	{"⚡  Fix + Repair + Recreate + Up (FORCE)", "full_repair_force"},
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
	{"⚡  Fix + Repair + Recreate + Up — ALL (FORCE)", "full_repair_force_all"},
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
	case "fix_force":
		return m, tuiSelfCmd("Fix (force) "+name, "fix", name, "force")
	case "repair_force":
		return m, tuiSelfCmd("Repair (force) "+name, "fix", name, "repair", "force")
	case "full_repair_force":
		return m, tuiSelfCmd("Heal (force) "+name, "up", name, "recreate", "repair", "fix", "force")
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
	case "full_repair_force_all":
		return m, tuiSelfCmd("Fix+Repair+Recreate+Up ALL (force)", "up", "repair", "fix", "recreate", "force")
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

// ===== from menu_popup.go =====

// menu_popup.go — the popup layer: action menus, confirm dialogs, single-line
// text input, scrollable output/detail boxes, and the rollback picker. Mirrors
// run_popup_action / _confirm_popup / _prompt_text / run_log_popup / show_message_box.

type tuiPopupKind int

const (
	tuiPopupActions tuiPopupKind = iota // selectable list -> onSelect(value)
	tuiPopupConfirm                     // No/Yes -> onConfirm()
	tuiPopupInput                       // text input -> onSubmit(text)
	tuiPopupOutput                      // read-only output, any key closes
	tuiPopupDetail                      // scrollable read-only detail
)

// tuiAction is one selectable row: a label and an opaque value.
type tuiAction struct {
	Label string
	Value string
}

type tuiPopup struct {
	kind  tuiPopupKind
	title string

	// list / confirm
	actions []tuiAction
	sel     int
	scroll  int

	// input
	prompt string
	buf    string

	// output / detail
	lines     []string
	lineScrol int

	// callbacks (executed by the model; may return a tea.Cmd)
	onSelect  func(value string) (menuModel, tea.Cmd)
	onConfirm func() (menuModel, tea.Cmd)
	onSubmit  func(text string) (menuModel, tea.Cmd)
}

// ── Rendering ────────────────────────────────────────────────────────────────

func (p *tuiPopup) render(width int) string {
	// Size the box to the CONTENT (longest line) so it's a snug little popup
	// floating over the screen, not a near-fullscreen panel. All widths are
	// VISIBLE columns (emoji/wide-char aware) so the right border lines up.
	need := vw(" " + p.title + " ") // title needs w-2 ≥ this
	bump := func(textCols int) {    // a body line needs w-4 ≥ textCols
		if textCols+4 > need {
			need = textCols + 4
		}
		if textCols+2 > need {
			need = textCols + 2
		}
	}
	for _, a := range p.actions {
		bump(vw("  " + a.Label))
	}
	for _, l := range p.lines {
		bump(vw(l))
	}
	if p.kind == tuiPopupInput {
		bump(vw(p.prompt))
		bump(vw("> " + p.buf + "_"))
		bump(vw("ENTER = save   ESC = cancel"))
	}
	bump(vw("↑↓ scroll  (000/000)  any key / ESC close"))
	w := need + 2
	if w < 28 {
		w = 28
	}
	maxW := width - 4
	if maxW > 66 { // keep popups compact even on wide screens
		maxW = 66
	}
	if w > maxW {
		w = maxW
	}
	if w < 20 {
		w = 20
	}
	var b strings.Builder
	bot := "╚" + strings.Repeat("═", w-2) + "╝"
	title := vtrunc(" "+p.title+" ", w-2)
	tw := vw(title)
	tl := (w - 2 - tw) / 2
	if tl < 0 {
		tl = 0
	}
	tr := w - 2 - tw - tl
	topLine := "╔" + strings.Repeat("═", tl) + title + strings.Repeat("═", tr) + "╗"
	b.WriteString(tuiPopupBorder.Render(topLine))
	b.WriteString("\n")

	body := func(s string) {
		s = vtrunc(s, w-4)
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString(" " + s)
		pad := w - 4 - vw(s)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(" ")
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString("\n")
	}

	switch p.kind {
	case tuiPopupActions:
		vis := 14
		end := p.scroll + vis
		if end > len(p.actions) {
			end = len(p.actions)
		}
		for i := p.scroll; i < end; i++ {
			label := vtrunc(p.actions[i].Label, w-6)
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		if p.scroll > 0 {
			body("▲ more above")
		}
		if end < len(p.actions) {
			body("▼ more below")
		}

	case tuiPopupConfirm:
		body("")
		for i, a := range p.actions {
			label := a.Label
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		body("")
		body("↑↓ select  ENTER confirm  ESC cancel")

	case tuiPopupInput:
		body(p.prompt)
		body("> " + p.buf + "_")
		body("")
		body("ENTER = save   ESC = cancel")

	case tuiPopupOutput, tuiPopupDetail:
		vis := 16
		end := p.lineScrol + vis
		if end > len(p.lines) {
			end = len(p.lines)
		}
		for i := p.lineScrol; i < end; i++ {
			body(p.lines[i])
		}
		if len(p.lines) > vis {
			body(fmt.Sprintf("↑↓ scroll  (%d/%d)  any key / ESC close", end, len(p.lines)))
		} else {
			body("any key / ESC to close")
		}
	}

	b.WriteString(tuiPopupBorder.Render(bot))
	return b.String()
}

// padRight pads (or truncates) s to n VISIBLE columns — emoji/wide-char aware,
// so highlighted rows have the same width as the box and borders line up.
func padRight(s string, n int) string {
	w := vw(s)
	if w >= n {
		return vtrunc(s, n)
	}
	return s + strings.Repeat(" ", n-w)
}

// vw is the visible (display) column width of s — counts emoji + CJK as 2.
func vw(s string) int { return runewidth.StringWidth(s) }

// vtrunc truncates s to at most n visible columns (no tail).
func vtrunc(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if vw(s) <= n {
		return s
	}
	return runewidth.Truncate(s, n, "")
}

// overlayCenter composites the fg box centered ON TOP of the bg view, so the
// menu pops up over the still-visible container list instead of a blank screen.
// ANSI- and wide-char-aware: keeps the background showing around the box.
func overlayCenter(bg, fg string, totalW, totalH int) string {
	bgLines := strings.Split(bg, "\n")
	for len(bgLines) < totalH {
		bgLines = append(bgLines, "")
	}
	fgLines := strings.Split(strings.TrimRight(fg, "\n"), "\n")
	fgW := 0
	for _, l := range fgLines {
		if x := ansi.StringWidth(l); x > fgW {
			fgW = x
		}
	}
	startRow := (totalH - len(fgLines)) / 2
	if startRow < 0 {
		startRow = 0
	}
	startCol := (totalW - fgW) / 2
	if startCol < 0 {
		startCol = 0
	}
	for i, fl := range fgLines {
		row := startRow + i
		if row < 0 || row >= len(bgLines) {
			continue
		}
		bl := bgLines[row]
		if x := ansi.StringWidth(bl); x < totalW {
			bl += strings.Repeat(" ", totalW-x)
		}
		left := ansi.Truncate(bl, startCol, "")
		right := ansi.TruncateLeft(bl, startCol+ansi.StringWidth(fl), "")
		bgLines[row] = left + fl + right
	}
	return strings.Join(bgLines, "\n")
}

// ── Key handling ─────────────────────────────────────────────────────────────

func (m menuModel) handlePopupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.popup
	k := msg.String()

	switch p.kind {
	case tuiPopupActions:
		switch k {
		case "up", "k":
			if p.sel > 0 {
				p.sel--
			}
			if p.sel < p.scroll {
				p.scroll = p.sel
			}
		case "down", "j":
			if p.sel < len(p.actions)-1 {
				p.sel++
			}
			if p.sel >= p.scroll+14 {
				p.scroll = p.sel - 13
			}
		case "pgup":
			p.sel -= 14
			if p.sel < 0 {
				p.sel = 0
			}
			p.scroll = p.sel
		case "pgdown":
			p.sel += 14
			if p.sel > len(p.actions)-1 {
				p.sel = len(p.actions) - 1
			}
		case "enter":
			val := p.actions[p.sel].Value
			// "Cancel" just closes the popup — its callback is a no-op and the
			// closure could re-show a stale popup, which left the user stuck.
			if val == "cancel" || strings.EqualFold(p.actions[p.sel].Label, "Cancel") {
				m.popup = nil
				return m, nil
			}
			cb := p.onSelect
			m.popup = nil
			if cb != nil {
				return cb(val)
			}
			return m, nil
		case "esc", "q", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupConfirm:
		switch k {
		case "up", "k", "down", "j", "left", "right", "tab":
			p.sel = 1 - p.sel
		case "y", "Y":
			p.sel = 1
			fallthrough
		case "enter":
			yes := p.actions[p.sel].Value == "yes"
			cb := p.onConfirm
			m.popup = nil
			if yes && cb != nil {
				return cb()
			}
			return m, nil
		case "n", "N", "esc", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupInput:
		switch k {
		case "enter":
			text := strings.TrimSpace(p.buf)
			cb := p.onSubmit
			m.popup = nil
			if cb != nil {
				return cb(text)
			}
			return m, nil
		case "esc", "ctrl+c":
			m.popup = nil
		case "backspace":
			if len(p.buf) > 0 {
				p.buf = p.buf[:len(p.buf)-1]
			}
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= 32 && msg.Runes[0] < 127 {
				p.buf += string(msg.Runes)
			}
		}
		return m, nil

	case tuiPopupOutput, tuiPopupDetail:
		switch k {
		case "up", "k":
			if p.lineScrol > 0 {
				p.lineScrol--
			}
		case "down", "j":
			if p.lineScrol < len(p.lines)-1 {
				p.lineScrol++
			}
		case "pgup":
			p.lineScrol -= 14
			if p.lineScrol < 0 {
				p.lineScrol = 0
			}
		case "pgdown":
			p.lineScrol += 14
			if p.lineScrol > len(p.lines)-1 {
				p.lineScrol = len(p.lines) - 1
			}
		default:
			m.popup = nil
		}
		return m, nil
	}
	return m, nil
}

// ── Constructors ─────────────────────────────────────────────────────────────

func tuiActionPopup(title string, actions []tuiAction, onSelect func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupActions, title: title, actions: actions, onSelect: onSelect}
}

func tuiConfirmPopup(title, dangerLabel string, onConfirm func() (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{
		kind:  tuiPopupConfirm,
		title: title,
		actions: []tuiAction{
			{Label: "✕  No — cancel", Value: "no"},
			{Label: dangerLabel, Value: "yes"},
		},
		onConfirm: onConfirm,
	}
}

func tuiInputPopup(title, prompt, def string, onSubmit func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupInput, title: title, prompt: prompt, buf: def, onSubmit: onSubmit}
}

func tuiOutputPopup(title string, lines []string) *tuiPopup {
	return &tuiPopup{kind: tuiPopupOutput, title: title, lines: lines}
}

// ===== from menu_network.go =====

// menu_network.go — Network tab: IP & port collision detection + the editable
// IP/port range and black/whitelist config (saved into stacks.yaml via the yaml
// helpers). Mirrors draw_network_tab + do_network_action.

// tuiNetData caches a collision scan (rescanned every 30s like the Python tab).
type tuiNetData struct {
	ipCol   []collisionIPRec
	portCol []collisionPortRec
	ipMap   map[string][]collisionOwner
	portMap map[string][]collisionOwner
	nextIP  string
	conf    map[string]string
}

func (m *menuModel) ensureNetData() *tuiNetData {
	if m.netCache == nil || time.Since(m.netCacheTS) > 30*time.Second {
		ip, port := getCollisions()
		m.netCache = &tuiNetData{
			ipCol:   ip,
			portCol: port,
			ipMap:   scanAllIPs(),
			portMap: scanAllPorts(),
			nextIP:  getNextAvailableIP(),
			conf:    collisionLoadConf(),
		}
		m.netCacheTS = time.Now()
	}
	return m.netCache
}

func (m menuModel) renderNetwork() string {
	// Read the scan done in the background collect — NEVER scan in the render
	// path (that re-ran the full 172-network scan every frame and froze the UI).
	nd := m.data.Net
	if nd == nil {
		return "\n  scanning networks… (first load)\n"
	}
	var b strings.Builder
	p := func(style interface{ Render(...string) string }, s string) {
		b.WriteString(style.Render(truncate("  "+s, m.width-1)))
		b.WriteString("\n")
	}
	p(tuiAccentStyle, "NETWORK — IP & PORT COLLISION DETECTION")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	var act, lat, bl []collisionPortRec
	for _, c := range nd.portCol {
		switch {
		case c.Active:
			act = append(act, c)
		case c.Type == "blacklisted":
			bl = append(bl, c)
		case c.Type == "duplicate":
			lat = append(lat, c)
		}
	}

	sumStyle := tuiYellowStyle
	if len(act) > 0 {
		sumStyle = tuiRedStyle
	}
	p(sumStyle, fmt.Sprintf("IPs in use: %d    Ports in use: %d    IP issues: %d    Ports: %d LIVE / %d latent / %d on-blacklist",
		len(nd.ipMap), len(nd.portMap), len(nd.ipCol), len(act), len(lat), len(bl)))
	nextStyle := tuiGreenStyle
	if nd.nextIP == "" {
		nextStyle = tuiRedStyle
	}
	p(nextStyle, "Next free IP: "+orNone(nd.nextIP, "NONE"))

	b.WriteString("\n")
	p(tuiAccentStyle, "── CONFIG ──────────────────────")
	p(tuiCyanStyle, fmt.Sprintf("IP range     : %s  →  %s", nd.conf["IP_RANGE_START"], nd.conf["IP_RANGE_END"]))
	p(tuiCyanStyle, fmt.Sprintf("Port range   : %s  →  %s", nd.conf["PORT_RANGE_START"], nd.conf["PORT_RANGE_END"]))
	p(tuiDimStyle, fmt.Sprintf("IP blacklist : %s", orNone(nd.conf["IP_BLACKLIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("IP whitelist : %s", orNone(nd.conf["IP_WHITELIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("Port blacklist: %s", orNone(nd.conf["PORT_BLACKLIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("Port whitelist: %s", orNone(nd.conf["PORT_WHITELIST"], "(none)")))

	b.WriteString("\n")
	if len(nd.ipCol) > 0 {
		p(tuiRedStyle, "⚠ IP ISSUES:")
		for i, c := range nd.ipCol {
			if i >= 4 {
				break
			}
			p(tuiRedStyle, fmt.Sprintf("  %-12s %-18s %s", c.Type, c.IP, ownersStr(c.Owners, 3)))
		}
	}
	if len(act) > 0 {
		p(tuiRedStyle, "⚠ LIVE PORT COLLISIONS:")
		for i, c := range act {
			if i >= 6 {
				break
			}
			p(tuiRedStyle, fmt.Sprintf("  %s:%-6s %s", c.IP, c.Port, ownersStr(c.Owners, 3)))
		}
	} else {
		p(tuiGreenStyle, "✔ No LIVE port collisions")
	}
	if len(lat) > 0 {
		p(tuiYellowStyle, fmt.Sprintf("• %d latent (declared, not all running):", len(lat)))
		for i, c := range lat {
			if i >= 5 {
				break
			}
			p(tuiDimStyle, fmt.Sprintf("  %s:%-6s %s", c.IP, c.Port, ownersStr(c.Owners, 3)))
		}
	}
	return b.String()
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func ownersStr(owners []collisionOwner, n int) string {
	var parts []string
	for i, o := range owners {
		if i >= n {
			break
		}
		parts = append(parts, o.Stack+"/"+o.Container)
	}
	return strings.Join(parts, ", ")
}

func (m menuModel) handleNetworkKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "a", "A":
		m.popup = tuiActionPopup("Network — Edit", tuiNetEditActions,
			func(action string) (menuModel, tea.Cmd) { return m.doNetworkAction(action) })
	case "s", "S", "enter":
		m.netCache = nil
		return m, tuiDockerCmd("Scan collisions", func() string {
			ip, port := getCollisions()
			var live int
			for _, c := range port {
				if c.Active {
					live++
				}
			}
			lines := []string{
				fmt.Sprintf("IP issues:        %d", len(ip)),
				fmt.Sprintf("Port collisions:  %d (%d live)", len(port), live),
				fmt.Sprintf("IPs in use:       %d", len(scanAllIPs())),
				fmt.Sprintf("Ports in use:     %d", len(scanAllPorts())),
				fmt.Sprintf("Next free IP:     %s", orNone(getNextAvailableIP(), "NONE")),
			}
			return strings.Join(lines, "\n")
		})
	case "r", "R":
		m.netCache = nil
	case "e", "E":
		// open stacks.yaml in $EDITOR (mirrors the Python Network 'e' key)
		return m, tuiEditFile(filepath.Join(configDir(), "stacks.yaml"))
	case "d", "D":
		// Dedupe: report duplicate container_names across stacks + the
		// recommended keeper (mirrors the Python Network 'd' key). Resolving a
		// dup is done from the container menu's Remove/purge actions (FIX_DEDUPE
		// also auto-resolves on `fix`).
		return m, tuiSelfCmd("Dedupe — duplicate container_names", "dedupe")
	}
	return m, nil
}

var tuiNetEditActions = []tuiAction{
	{"✎  IP range — start", "ip_start"},
	{"✎  IP range — end", "ip_end"},
	{"✎  Port range — start", "port_start"},
	{"✎  Port range — end", "port_end"},
	{"➕ Add IP to blacklist", "ip_bl_add"},
	{"➖ Remove IP from blacklist", "ip_bl_rm"},
	{"➕ Add IP to whitelist", "ip_wl_add"},
	{"➖ Remove IP from whitelist", "ip_wl_rm"},
	{"➕ Add port to blacklist", "port_bl_add"},
	{"➖ Remove port from blacklist", "port_bl_rm"},
	{"➕ Add port to whitelist", "port_wl_add"},
	{"➖ Remove port from whitelist", "port_wl_rm"},
	{"↻  Rescan now", "rescan"},
	{"✕  Cancel", ""},
}

// netScalarMap: action -> friendly yaml key.
var tuiNetScalars = map[string][2]string{
	"ip_start":   {"ip_range_start", "Start IP:"},
	"ip_end":     {"ip_range_end", "End IP:"},
	"port_start": {"port_range_start", "Start port:"},
	"port_end":   {"port_range_end", "End port:"},
}
var tuiNetAdds = map[string]string{
	"ip_bl_add": "ip_blacklist", "ip_wl_add": "ip_whitelist",
	"port_bl_add": "port_blacklist", "port_wl_add": "port_whitelist",
}
var tuiNetRms = map[string]string{
	"ip_bl_rm": "ip_blacklist", "ip_wl_rm": "ip_whitelist",
	"port_bl_rm": "port_blacklist", "port_wl_rm": "port_whitelist",
}

func (m menuModel) doNetworkAction(action string) (menuModel, tea.Cmd) {
	if action == "" || action == "cancel" {
		return m, nil
	}
	if action == "rescan" {
		m.netCache = nil
		return m, nil
	}
	if sc, ok := tuiNetScalars[action]; ok {
		fk, prompt := sc[0], sc[1]
		m.popup = tuiInputPopup("Edit "+fk, prompt, "", func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				yamlSetScalar(fk, v)
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	if key, ok := tuiNetAdds[action]; ok {
		m.popup = tuiInputPopup("Add to "+key, "Value:", "", func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				items := yamlGetList(key)
				if !contains(items, v) {
					items = append(items, v)
					yamlSetList(key, items)
				}
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	if key, ok := tuiNetRms[action]; ok {
		items := yamlGetList(key)
		if len(items) == 0 {
			m.popup = tuiOutputPopup("Remove from "+key, []string{"(list is empty)"})
			return m, nil
		}
		var acts []tuiAction
		for _, it := range items {
			acts = append(acts, tuiAction{it, it})
		}
		acts = append(acts, tuiAction{"✕ Cancel", ""})
		m.popup = tuiActionPopup("Remove from "+key, acts, func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				var kept []string
				for _, it := range items {
					if it != v {
						kept = append(kept, it)
					}
				}
				yamlSetList(key, kept)
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	return m, nil
}

func contains(items []string, v string) bool {
	for _, it := range items {
		if it == v {
			return true
		}
	}
	return false
}

// ===== from menu_zeroscale.go =====

// menu_zeroscale.go — the "Zero Scale" per-container options, shown inside the
// Containers-tab Tab/Enter popup (only when `zero_scale: on` in the config).
//
// Zero Scale is Bellz's own Sablier replacement (the `stackwake` engine). This
// screen lets her flip wake-on-visit per container and set every option she used
// to hand-write in the Traefik sablier middleware — loading screen, idle time,
// the container group, display name, stop timeout, etc. It reads/writes the same
// config the engine watches: <configDir>/zeroscale.yaml.

// the editable loading screens shipped with the engine
var zsScreens = []string{"minecraft", "terminal", "ghost", "synthwave", "pride"}

type zsSite struct {
	Host        []string `yaml:"host,omitempty"`
	Containers  []string `yaml:"containers,omitempty"`
	Service     string   `yaml:"service,omitempty"`
	Display     string   `yaml:"display,omitempty"`
	Screen      string   `yaml:"screen,omitempty"`
	Idle        string   `yaml:"session_duration,omitempty"` // per-site override, e.g. "30m"
	StopTimeout string   `yaml:"stop_timeout,omitempty"`
	AlwaysOn    bool     `yaml:"always_on,omitempty"`
	Enabled     *bool    `yaml:"enabled,omitempty"`
}

type zsConfig struct {
	IdleSeconds    int                `yaml:"idle_seconds"`
	PollSeconds    int                `yaml:"poll_seconds"`
	TraefikMetrics string             `yaml:"traefik_metrics"`
	WakeBase       string             `yaml:"wake_base"`
	DefaultScreen  string             `yaml:"default_screen"`
	Sites          map[string]*zsSite `yaml:"sites"`
}

func zeroScalePath() string { return filepath.Join(configDir(), "zeroscale.yaml") }

func zeroScaleEnabled() bool { return configLoad()["ZERO_SCALE"] == "1" }

func loadZSConfig() *zsConfig {
	c := &zsConfig{Sites: map[string]*zsSite{}}
	data, err := os.ReadFile(zeroScalePath())
	if err == nil {
		_ = yaml.Unmarshal(data, c)
	}
	if c.Sites == nil {
		c.Sites = map[string]*zsSite{}
	}
	// Layer in the GLOBAL Zero Scale settings from stacks.conf/yaml (the Settings
	// tab edits those), so the engine honours them. Per-site values in
	// zeroscale.yaml still win when set.
	cfg := configLoad()
	if c.IdleSeconds == 0 {
		c.IdleSeconds = cfgInt(cfg, "ZERO_SCALE_IDLE", 1800)
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = cfgInt(cfg, "ZERO_SCALE_POLL", 20)
	}
	if c.DefaultScreen == "" {
		c.DefaultScreen = cfgStrKey(cfg, "ZERO_SCALE_DEFAULT_SCREEN", "minecraft")
	}
	if c.TraefikMetrics == "" {
		c.TraefikMetrics = cfgStrKey(cfg, "ZERO_SCALE_METRICS", "")
	}
	if c.WakeBase == "" {
		c.WakeBase = cfgStrKey(cfg, "ZERO_SCALE_WAKE_BASE", "")
	}
	return c
}

func saveZSConfig(c *zsConfig) error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(zeroScalePath(), out, 0644)
}

// siteForContainer returns the existing site whose container list includes name,
// plus its key. Returns ("", nil) if this container is not Zero-Scaled yet.
func (c *zsConfig) siteForContainer(name string) (string, *zsSite) {
	for k, s := range c.Sites {
		for _, cn := range s.Containers {
			if cn == name {
				return k, s
			}
		}
		if k == name {
			return k, s
		}
	}
	return "", nil
}

func boolStr(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// openZeroScalePopup builds the scrollable per-container options menu.
func (m menuModel) openZeroScalePopup(name string) (menuModel, tea.Cmd) {
	c := loadZSConfig()
	key, s := c.siteForContainer(name)
	on := s != nil && (s.Enabled == nil || *s.Enabled)

	scr := c.DefaultScreen
	if s != nil && s.Screen != "" {
		scr = s.Screen
	}
	idle := "(global)"
	if s != nil && s.Idle != "" {
		idle = s.Idle
	}
	disp, group, stopT, always := name, name, "30s", false
	if s != nil {
		if s.Display != "" {
			disp = s.Display
		}
		if len(s.Containers) > 0 {
			group = strings.Join(s.Containers, ",")
		}
		if s.StopTimeout != "" {
			stopT = s.StopTimeout
		}
		always = s.AlwaysOn
	}

	acts := []tuiAction{
		{fmt.Sprintf("⚡ Wake-on-visit (Zero Scale)   [ %s ]", boolStr(on)), "toggle"},
		{fmt.Sprintf("🎨 Loading screen              [ %s ]", scr), "screen"},
		{fmt.Sprintf("💤 Idle before sleep           [ %s ]", idle), "idle"},
		{fmt.Sprintf("🏷  Display name                [ %s ]", orDash(disp)), "display"},
		{fmt.Sprintf("📦 Wake group (containers)     [ %s ]", orDash(group)), "group"},
		{fmt.Sprintf("⏱  Stop timeout                [ %s ]", orDash(stopT)), "stop"},
		{fmt.Sprintf("📌 Keep always-on (never sleep)[ %s ]", boolStr(always)), "always"},
		{"✕  Done", "cancel"},
	}
	_ = key
	m.popup = tuiActionPopup("⚡ Zero Scale — "+truncate(name, 22), acts,
		func(action string) (menuModel, tea.Cmd) {
			return m.doZeroScaleAction(name, action)
		})
	return m, nil
}

// doZeroScaleAction applies one option change, persists, and re-opens the menu so
// it stays put and shows the new value.
func (m menuModel) doZeroScaleAction(name, action string) (menuModel, tea.Cmd) {
	c := loadZSConfig()
	key, s := c.siteForContainer(name)
	ensure := func() *zsSite {
		if s == nil {
			key = name
			s = &zsSite{
				Host:       []string{name + ".loveiznothin.com"},
				Containers: []string{name},
				Service:    name + "-svc@file",
				Display:    name,
			}
			c.Sites[key] = s
		}
		return s
	}

	switch action {
	case "", "cancel":
		return m, nil
	case "toggle":
		s = ensure()
		v := !(s.Enabled == nil || *s.Enabled)
		s.Enabled = &v
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "always":
		s = ensure()
		s.AlwaysOn = !s.AlwaysOn
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "screen":
		// cycle through the 5 screens
		s = ensure()
		cur := s.Screen
		if cur == "" {
			cur = c.DefaultScreen
		}
		next := zsScreens[0]
		for i, sc := range zsScreens {
			if sc == cur {
				next = zsScreens[(i+1)%len(zsScreens)]
				break
			}
		}
		s.Screen = next
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "idle":
		def := ""
		if s != nil {
			def = s.Idle
		}
		m.popup = tuiInputPopup("Idle before sleep — "+truncate(name, 18),
			"e.g. 30m, 2h, 600s  (blank = use global):", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.Idle = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "display":
		def := name
		if s != nil && s.Display != "" {
			def = s.Display
		}
		m.popup = tuiInputPopup("Display name — "+truncate(name, 18),
			"Shown on the loading screen:", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.Display = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "group":
		def := name
		if s != nil && len(s.Containers) > 0 {
			def = strings.Join(s.Containers, ",")
		}
		m.popup = tuiInputPopup("Wake group — "+truncate(name, 18),
			"Comma-separated containers to wake together:", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				parts := []string{}
				for _, p := range strings.Split(txt, ",") {
					if p = strings.TrimSpace(p); p != "" {
						parts = append(parts, p)
					}
				}
				if len(parts) > 0 {
					s.Containers = parts
				}
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "stop":
		def := "30s"
		if s != nil && s.StopTimeout != "" {
			def = s.StopTimeout
		}
		m.popup = tuiInputPopup("Stop timeout — "+truncate(name, 18),
			"How long to wait when stopping (e.g. 30s):", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.StopTimeout = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	}
	_ = key
	return m.openZeroScalePopup(name)
}

// ===== from menu_helpers.go =====

// menu_helpers.go — small utilities shared across the TUI files: command runners
// (Docker API closures, shell-outs, self-invocations), string/file helpers,
// inspect/rename/image-resolution mirrors of the Python menu's helper functions.

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func baseName(p string) string { return filepath.Base(p) }

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

var tuiNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func tuiValidName(s string) bool { return tuiNameRe.MatchString(s) }

func tuiFmtTime(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Format("01-02 15:04")
}

// ── Command runners (return tea.Cmd that finishes with tuiActionDoneMsg) ───────

// tuiDockerCmd runs a Go closure (Docker API actions) off the UI goroutine.
func tuiDockerCmd(title string, fn func() string) tea.Cmd {
	return func() tea.Msg {
		return tuiActionDoneMsg{title: title, output: fn()}
	}
}

// tuiShellCmd runs an arbitrary command, capturing combined output.
func tuiShellCmd(title, name string, args ...string) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.Command(name, args...)
		cmd.Env = dockerEnv()
		out, err := cmd.CombinedOutput()
		s := string(out)
		if err != nil && s == "" {
			s = err.Error()
		}
		return tuiActionDoneMsg{title: title, output: s}
	}
}

// selfExe resolves this binary so menu actions reuse the real engine commands.
func selfExe() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "stacks"
}

// tuiSelfCmd runs `stacks <args…>` (the same binary) and captures output.
func tuiSelfCmd(title string, args ...string) tea.Cmd {
	return tuiShellCmd(title, selfExe(), args...)
}

// tuiEditFile suspends the TUI, opens path in $EDITOR (nano fallback), then
// resumes — mirrors the Python menu's "open in $EDITOR" on Configs/Art/Network.
func tuiEditFile(path string) tea.Cmd {
	ed := os.Getenv("EDITOR")
	if ed == "" {
		ed = os.Getenv("VISUAL")
	}
	if ed == "" {
		ed = "nano"
	}
	return tea.ExecProcess(exec.Command(ed, path), func(error) tea.Msg { return nil })
}

// tuiExecSelf suspends the TUI and runs `stacks <args…>` INTERACTIVELY (shares
// the real terminal stdin/stdout/stderr), then resumes — used for the build
// wizard and anything else that needs live prompts (fzf, buildAsk).
func tuiExecSelf(args ...string) tea.Cmd {
	c := exec.Command(selfExe(), args...)
	c.Env = dockerEnv()
	return tea.ExecProcess(c, func(error) tea.Msg { return nil })
}

// cliSelf is the synchronous variant for use inside another command closure.
func cliSelf(args ...string) string {
	cmd := exec.Command(selfExe(), args...)
	cmd.Env = dockerEnv()
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil && s == "" {
		s = err.Error()
	}
	return s
}

// ── Inspect / rename / image resolution (mirror the Python helpers) ───────────

// tuiInspectLines mirrors _show_container_inspect's formatted summary.
func tuiInspectLines(name string) []string {
	r := cli("inspect", "--format",
		"ID: {{.Id}}\nImage: {{.Config.Image}}\nStatus: {{.State.Status}}\n"+
			"Started: {{.State.StartedAt}}\n"+
			"IP: {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}\n"+
			"Ports: {{range $p,$b := .NetworkSettings.Ports}}{{$p}} {{end}}\n"+
			"CPU: {{.HostConfig.CpusetCpus}}\nMemory: {{.HostConfig.Memory}}\n"+
			"Restart: {{.HostConfig.RestartPolicy.Name}}\nMounts: {{len .Mounts}} volumes",
		name)
	info := strings.TrimSpace(r.stdout)
	if r.exitCode != 0 {
		info = strings.TrimSpace(r.stderr)
	}
	if info == "" {
		info = "(no inspect output)"
	}
	return strings.Split(info, "\n")
}

var tuiServiceKeyLineRe = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:\s*$`)

// tuiImageForContainer mirrors _image_for_container: find the compose image: for
// the service whose container_name is cname; fall back to docker inspect.
func tuiImageForContainer(stackFile, cname string) string {
	if stackFile != "" {
		if data, err := readFile(stackFile); err == nil {
			lines := strings.Split(data, "\n")
			re := regexp.MustCompile(`(?m)^\s*container_name:\s*"?` + regexp.QuoteMeta(cname) + `"?\s*$`)
			ci := -1
			for i, l := range lines {
				if re.MatchString(l) {
					ci = i
					break
				}
			}
			if ci >= 0 {
				start := ci
				for start > 0 && !tuiServiceKeyLineRe.MatchString(lines[start]) {
					start--
				}
				end := start + 1
				for end < len(lines) && !tuiServiceKeyLineRe.MatchString(lines[end]) {
					end++
				}
				imgRe := regexp.MustCompile(`^\s*image:\s*([^\s#]+)`)
				for j := start; j < end; j++ {
					if mm := imgRe.FindStringSubmatch(lines[j]); mm != nil {
						return strings.Trim(strings.TrimSpace(mm[1]), `"'`)
					}
				}
			}
		}
	}
	r := cli("inspect", "--format", "{{.Config.Image}}", cname)
	if r.exitCode == 0 {
		return strings.TrimSpace(r.stdout)
	}
	return ""
}

// tuiRenameContainerInFile mirrors _rename_container_in_file (compose only;
// the live rename is done separately by the caller).
func tuiRenameContainerInFile(stackFile, old, newName string) {
	if stackFile == "" {
		return
	}
	data, err := readFile(stackFile)
	if err != nil {
		return
	}
	re := regexp.MustCompile(`(?m)^(\s*container_name:\s*)"?` + regexp.QuoteMeta(old) + `"?\s*$`)
	out := re.ReplaceAllString(data, "${1}"+newName)
	_ = os.WriteFile(stackFile, []byte(out), 0644)
}

// tuiRenameStackFile mirrors _rename_stack_file: rewrite top-level name:, move file.
func tuiRenameStackFile(oldYml, newYml, newName string) (bool, string) {
	data, err := readFile(oldYml)
	if err != nil {
		return false, err.Error()
	}
	nameRe := regexp.MustCompile(`(?m)^name:\s*.*$`)
	if nameRe.MatchString(data) {
		data = nameRe.ReplaceAllString(data, "name: "+newName)
	} else {
		data = "name: " + newName + "\n" + data
	}
	if err := os.WriteFile(oldYml, []byte(data), 0644); err != nil {
		return false, err.Error()
	}
	if err := os.Rename(oldYml, newYml); err != nil {
		return false, err.Error()
	}
	return true, fmt.Sprintf("Renamed -> %s", filepath.Base(newYml))
}

// ===== from menu_settings.go =====

// menu_settings.go — Settings tab: parse stacks.conf into KEY / VALUE / comment
// rows; ENTER toggles a 0/1 switch or edits any value, then saves into stacks.conf
// in place AND mirrors the change into stacks.yaml (via yamlSetScalar / yamlSetList
// for mapped keys). Mirrors get_settings_items / _settings_save / draw_settings_tab.

type tuiSetting struct {
	Key  string
	Val  string
	Desc string
}

func tuiSettingsConf() string { return filepath.Join(configDir(), "stacks.conf") }

// tuiLoadSettings mirrors get_settings_items().
func tuiLoadSettings() []tuiSetting {
	var items []tuiSetting
	data, err := os.ReadFile(tuiSettingsConf())
	if err != nil {
		return items
	}
	desc := ""
	for _, raw := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "#") {
			desc = strings.TrimSpace(strings.TrimLeft(s, "#"))
			continue
		}
		if k, v, ok := strings.Cut(s, "="); ok {
			items = append(items, tuiSetting{
				Key:  strings.TrimSpace(k),
				Val:  strings.Trim(strings.TrimSpace(v), `"'`),
				Desc: desc,
			})
			desc = ""
		}
	}
	return items
}

// tuiSettingsReverseMap mirrors _settings_reverse_map: internal KEY -> (friendly, kind).
func tuiSettingsReverseMap() map[string][2]string {
	rev := map[string][2]string{}
	for fk, ik := range scalarMap {
		rev[ik] = [2]string{fk, "scalar"}
	}
	for fk, lj := range listMap {
		rev[lj.key] = [2]string{fk, "list"}
	}
	return rev
}

var tuiListSplitRe = regexp.MustCompile(`[,\s]+`)
var tuiSpaceSplitRe = regexp.MustCompile(`\s+`)

// tuiSettingsSave mirrors _settings_save: write into stacks.conf, mirror to yaml.
func tuiSettingsSave(key, value string) {
	// 1) stacks.conf in place.
	if data, err := os.ReadFile(tuiSettingsConf()); err == nil {
		lines := strings.Split(string(data), "\n")
		found := false
		for i, l := range lines {
			st := strings.TrimSpace(l)
			if st != "" && !strings.HasPrefix(st, "#") && strings.Contains(st, "=") {
				if k, _, _ := strings.Cut(st, "="); strings.TrimSpace(k) == key {
					qv := value
					if value != "" && strings.ContainsAny(value, " \t#") &&
						!(strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)) {
						qv = `"` + value + `"`
					}
					lines[i] = key + "=" + qv
					found = true
				}
			}
		}
		if !found {
			lines = append(lines, key+"="+value)
		}
		_ = os.WriteFile(tuiSettingsConf(), []byte(strings.Join(lines, "\n")), 0644)
	}
	// 2) yaml overlay for mapped keys.
	rev := tuiSettingsReverseMap()
	if pair, ok := rev[key]; ok {
		fk, kind := pair[0], pair[1]
		if kind == "scalar" {
			yamlSetScalar(fk, value)
		} else {
			join := " "
			if lj, ok := listMap[fk]; ok {
				join = lj.join
			}
			splitter := tuiSpaceSplitRe
			if join == "," {
				splitter = tuiListSplitRe
			}
			var parts []string
			for _, p := range splitter.Split(value, -1) {
				if p != "" {
					parts = append(parts, p)
				}
			}
			yamlSetList(fk, parts)
		}
	}
}

func (m menuModel) renderSettings() string {
	items := tuiLoadSettings()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  SETTINGS — ENTER toggles a switch or edits a value (auto-saves)"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(items) == 0 {
		b.WriteString(tuiDimStyle.Render("  No settings found in stacks.conf."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(items) {
		end = len(items)
	}
	for i := m.scroll; i < end; i++ {
		it := items[i]
		isBool := it.Val == "0" || it.Val == "1"
		shown := orNone(truncate(it.Val, 24), "—")
		if isBool {
			if it.Val == "1" {
				shown = "● ON "
			} else {
				shown = "○ OFF"
			}
		}
		line := fmt.Sprintf("%-30s %-26s %s", it.Key, shown, it.Desc)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("▶ "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(fmt.Sprintf("  %-30s ", it.Key)))
			vc := tuiCyanStyle
			if isBool {
				if it.Val == "1" {
					vc = tuiGreenStyle
				} else {
					vc = tuiDimStyle
				}
			}
			b.WriteString(vc.Render(fmt.Sprintf("%-26s ", shown)))
			b.WriteString(tuiDimStyle.Render(truncate(it.Desc, maxInt(0, m.width-63))))
		}
		b.WriteString("\n")
	}
	b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  %d/%d", m.sel+1, len(items))))
	return b.String()
}

func (m menuModel) handleSettingsKey(k string) (tea.Model, tea.Cmd) {
	items := tuiLoadSettings()
	if m.sel >= len(items) {
		m.sel = maxInt(0, len(items)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(items))
	case "enter":
		if len(items) == 0 {
			return m, nil
		}
		it := items[m.sel]
		if it.Val == "0" || it.Val == "1" {
			nv := "1"
			if it.Val == "1" {
				nv = "0"
			}
			tuiSettingsSave(it.Key, nv)
		} else {
			prompt := it.Desc
			if prompt == "" {
				prompt = "Value:"
			}
			m.popup = tuiInputPopup("Edit "+it.Key, truncate(prompt, 46), it.Val,
				func(nv string) (menuModel, tea.Cmd) {
					if nv != it.Val {
						tuiSettingsSave(it.Key, nv)
					}
					return m, nil
				})
		}
	}
	return m, nil
}

// ===== from menu_keys.go =====

// menu_keys.go — keyboard handling for the TUI model. Mirrors the curses main()
// loop: tab switching, per-tab cursor movement, the "/" + A-Z filter, and the
// ENTER/TAB action popups.

func (m menuModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Popup intercepts all keys while open.
	if m.popup != nil {
		return m.handlePopupKey(msg)
	}

	k := msg.String()

	// ── Inline-search typing mode (filterable tabs) ──────────────────────────
	if tuiFilterableTab(m.tab) && m.fltMode {
		switch k {
		case "enter":
			m.fltMode = false
		case "esc":
			m.fltMode = false
			m.fltInline = ""
			m.sel, m.scroll = 0, 0
		case "backspace":
			if len(m.fltInline) > 0 {
				m.fltInline = m.fltInline[:len(m.fltInline)-1]
			}
			m.sel, m.scroll = 0, 0
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= 32 && msg.Runes[0] < 127 {
				m.fltInline += string(msg.Runes)
				m.sel, m.scroll = 0, 0
			}
		}
		return m, nil
	}

	// ── Filter activation keys (filterable tabs) ─────────────────────────────
	if tuiFilterableTab(m.tab) {
		switch k {
		case "/":
			m.fltMode = true
			m.fltInline = ""
			return m, nil
		case "#":
			if m.fltLetter == "#" {
				m.fltLetter = ""
			} else {
				m.fltLetter = "#"
			}
			m.sel, m.scroll = 0, 0
			return m, nil
		}
		// lowercase a-z = letter jump (uppercase stays a command on Updates/Network)
		if len(msg.Runes) == 1 {
			r := msg.Runes[0]
			if r >= 'a' && r <= 'z' {
				ch := string(r)
				if m.fltLetter == ch {
					m.fltLetter = ""
				} else {
					m.fltLetter = ch
				}
				m.sel, m.scroll = 0, 0
				return m, nil
			}
		}
		if k == "esc" && (m.fltLetter != "" || m.fltInline != "") {
			m.fltLetter, m.fltInline = "", ""
			m.sel, m.scroll = 0, 0
			return m, nil
		}
	}

	// ── Global keys ──────────────────────────────────────────────────────────
	switch k {
	case "q", "Q", "esc", "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "right", "l":
		m.tab = (m.tab + 1) % len(tuiTabs)
		m.resetTab()
		return m, nil
	case "left", "h":
		m.tab = (m.tab - 1 + len(tuiTabs)) % len(tuiTabs)
		m.resetTab()
		return m, nil
	}

	// ── Per-tab keys ─────────────────────────────────────────────────────────
	switch m.tab {
	case tabContainers:
		return m.handleContainersKey(k)
	case tabStacks:
		return m.handleStacksKey(k)
	case tabLogs:
		return m.handleLogsKey(k)
	case tabDynamics:
		return m.handleDynamicsKey(k)
	case tabArt:
		return m.handleArtKey(k)
	case tabBackup:
		return m.handleBackupKey(k)
	case tabBuild:
		return m.handleBuildKey(k)
	case tabConfigs:
		return m.handleConfigsKey(k)
	case tabNetwork:
		return m.handleNetworkKey(k)
	case tabUpdates:
		return m.handleUpdatesKey(k)
	case tabSettings:
		return m.handleSettingsKey(k)
	case tabUpgrade:
		return m.handleUpgradeKey(k)
	}
	return m, nil
}

func (m *menuModel) resetTab() {
	m.sel, m.scroll = 0, 0
	m.fltLetter, m.fltInline, m.fltMode = "", "", false
	if m.tab == tabUpdates {
		m.updateRows, m.updateSum = tuiBuildUpdateRows()
	}
	if m.tab == tabNetwork {
		m.netCache = nil
	}
}

// tuiVisibleRows is how many list rows fit below the header/tabs/headers.
func (m menuModel) visibleRows() int {
	v := m.height - 9
	if v < 1 {
		v = 1
	}
	return v
}

func (m *menuModel) moveCursor(k string, n int) {
	vis := m.visibleRows()
	switch k {
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
		if m.sel < m.scroll {
			m.scroll = m.sel
		}
	case "down", "j":
		if m.sel < n-1 {
			m.sel++
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	case "pgup":
		m.sel -= vis
		if m.sel < 0 {
			m.sel = 0
		}
		if m.sel < m.scroll {
			m.scroll = m.sel
		}
	case "pgdown":
		m.sel += vis
		if m.sel > n-1 {
			m.sel = n - 1
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	case "home":
		m.sel, m.scroll = 0, 0
	case "end":
		m.sel = n - 1
		if m.sel < 0 {
			m.sel = 0
		}
		if m.sel >= m.scroll+vis {
			m.scroll = m.sel - vis + 1
		}
	}
}

// ===== from menu_updates.go =====

// menu_updates.go — Updates tab: available updates (from the update cache) plus
// the update history (newest-first), searchable with "/" + A-Z. ENTER opens a
// detail box; C/F/P trigger check / force-check / pull. Mirrors get_update_rows
// + draw_updates_tab + UPDATES_ACTIONS.

type tuiUpdateRow struct {
	Kind     string // "update" | "hist"
	Image    string
	Tag      string
	Stacks   []string
	Event    string
	TS       int64
	Old      string
	New      string
	OldShort string
	NewShort string
}

type tuiUpdateSummary struct {
	Updates int
	OK      int
	Errors  int
	Hist    int
}

// tuiBuildUpdateRows mirrors get_update_rows().
func tuiBuildUpdateRows() ([]tuiUpdateRow, tuiUpdateSummary) {
	var rows []tuiUpdateRow
	var sum tuiUpdateSummary
	for _, v := range updLoadCache() {
		switch {
		case v.HasUpdate:
			sum.Updates++
			rows = append(rows, tuiUpdateRow{
				Kind: "update", Image: v.Image, Tag: v.Tag, Stacks: v.Stacks,
				Old: v.LocalDigest, New: v.RemoteDigest, TS: v.Checked,
			})
		case v.Error != "":
			sum.Errors++
		default:
			sum.OK++
		}
	}
	for _, r := range updGetHistory(0) {
		sum.Hist++
		rows = append(rows, tuiUpdateRow{
			Kind: "hist", Image: r.Image, Tag: r.Tag, Stacks: r.Stacks,
			Event: r.Event, TS: r.TS, Old: r.Old, New: r.New,
			OldShort: r.OldShort, NewShort: r.NewShort,
		})
	}
	return rows, sum
}

func (m menuModel) filteredUpdateRows() []tuiUpdateRow {
	var out []tuiUpdateRow
	for _, r := range m.updateRows {
		if m.fltLetter != "" && !tuiMatchLetter(r.Image, m.fltLetter) {
			continue
		}
		if m.fltInline != "" && !tuiContains([]string{r.Image, r.Event}, m.fltInline) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (m menuModel) renderUpdates() string {
	rows := m.filteredUpdateRows()
	s := m.updateSum
	var b strings.Builder
	b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("  ⬆ Updates: %d   ✔ OK: %d   ✘ Err: %d   ⟳ History: %d",
		s.Updates, s.OK, s.Errors, s.Hist)))
	b.WriteString("\n")
	b.WriteString(tuiAccentStyle.Render(fmt.Sprintf("  %-13s %-10s %-40s %s", "WHEN", "EVENT", "IMAGE", "OLD → NEW")))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No updates or history yet. Press C to check for updates."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		img := truncate(r.Image, 40)
		var when, ev, ver, mark string
		if r.Kind == "update" {
			when, ev, ver, mark = "now", "AVAILABLE", "update ready", "⬆"
		} else {
			when = tuiFmtTime(r.TS)
			ev = r.Event
			ver = fmt.Sprintf("%s → %s", orNone(r.OldShort, "—"), orNone(r.NewShort, "—"))
			mark = "⬆"
			if ev == "pulled" {
				mark = "⬇"
			}
		}
		line := fmt.Sprintf("%s %-13s %-10s %-40s %s", mark, when, ev, img, ver)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(truncate("  "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m menuModel) handleUpdatesKey(k string) (tea.Model, tea.Cmd) {
	rows := m.filteredUpdateRows()
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter":
		if len(rows) > 0 {
			m.popup = &tuiPopup{kind: tuiPopupDetail, title: "Update detail", lines: tuiUpdateDetail(rows[m.sel])}
		}
	case "C":
		return m, tuiSelfCmd("Check updates", "update")
	case "F":
		return m, tuiSelfCmd("Force check", "update", "--force")
	case "P":
		return m, tuiSelfCmd("Pull updates", "update", "--pull")
	}
	return m, nil
}

func tuiUpdateDetail(r tuiUpdateRow) []string {
	lines := []string{"Image:   " + r.Image}
	if r.Tag != "" {
		lines = append(lines, "Tag:     "+r.Tag)
	}
	if len(r.Stacks) > 0 {
		lines = append(lines, "Stacks:  "+strings.Join(r.Stacks, ", "))
	}
	if r.Kind == "update" {
		lines = append(lines, "Status:  UPDATE AVAILABLE")
	} else {
		lines = append(lines, "Event:   "+r.Event, "When:    "+tuiFmtTime(r.TS))
	}
	lines = append(lines, "", "OLD (was):", "  "+orNone(r.Old, "—"), "NEW (now):", "  "+orNone(r.New, "—"))
	return lines
}

// ===== from menu_configs.go =====

// menu_configs.go — Configs tab: lists the config files in configDir() plus the
// descriptions/ folder, with sizes; ENTER shows a file's content in a scrollable
// output popup. Mirrors get_config_items / draw_configs_tab. (Inline editing is
// left to `EDITOR` outside the TUI; the altscreen TUI shows the contents.)

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
			Label: fmt.Sprintf("📁 descriptions/  (%d files, %dK)", len(descFiles), maxInt64(1, total/1024)),
			Path:  descDir, IsDir: true, SizeKB: total / 1024,
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

// ===== from menu_logs.go =====

// menu_logs.go — Logs tab: lists the engine log files (stacks_*.log in the data
// dir) plus every live container; ENTER shows the recent tail in a scrollable
// output popup (reusing `docker logs` / file reads). Mirrors draw_logs_tab.

// tuiLogRow is one selectable Logs-tab entry.
type tuiLogRow struct {
	Label     string
	Path      string // non-empty for a log file
	Container string // non-empty for a live container
	SizeKB    int64
}

// tuiLogRows mirrors draw_logs_tab: the stacks_*.log files in the data dir,
// followed by the live containers (so per-container `docker logs` is reachable).
func tuiLogRows(d tuiData) []tuiLogRow {
	var rows []tuiLogRow
	seen := map[string]bool{}
	addFile := func(f string) {
		base := filepath.Base(f)
		if seen[base] {
			return
		}
		seen[base] = true
		var kb int64
		if st, err := os.Stat(f); err == nil {
			kb = st.Size() / 1024
		}
		rows = append(rows, tuiLogRow{Label: base, Path: f, SizeKB: kb})
	}
	// All logs live in logDir(): engine logs (stacks_*.log) + per-container
	// dumps (<name>.log). Also sweep the data dir for any legacy stray logs.
	ld := logDir()
	for _, pat := range []string{"stacks_*.log", "*.log"} {
		matches, _ := filepath.Glob(filepath.Join(ld, pat))
		sort.Strings(matches)
		for _, f := range matches {
			addFile(f)
		}
	}
	legacy, _ := filepath.Glob(filepath.Join(dispDataDir(), "stacks_*.log"))
	sort.Strings(legacy)
	for _, f := range legacy {
		addFile(f)
	}
	// Live containers (on-demand `docker logs`).
	for _, c := range d.Containers {
		rows = append(rows, tuiLogRow{Label: "▶ " + c.Name + " (docker logs)", Container: c.Name})
	}
	return rows
}

func (m menuModel) renderLogs() string {
	rows := tuiLogRows(m.data)
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  DOCKER LOGS — ENTER shows the recent tail · d = save every container log to the logs folder"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No log files or containers found."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		sz := ""
		if r.Path != "" {
			sz = fmt.Sprintf("%dK", r.SizeKB)
		}
		line := fmt.Sprintf("%-44s %6s", truncate(r.Label, 44), sz)
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶ "+line, m.width-2)))
		} else {
			// One consistent style for every row (no green/white mix); the
			// "▶ … (docker logs)" prefix already marks live containers.
			b.WriteString(tuiNormalStyle.Render(truncate("    "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m menuModel) handleLogsKey(k string) (tea.Model, tea.Cmd) {
	rows := tuiLogRows(m.data)
	if m.sel >= len(rows) {
		m.sel = maxInt(0, len(rows)-1)
	}
	switch k {
	case "d":
		// Save every running container's log to the logs folder.
		return m, tuiDockerCmd("Saving container logs", func() string {
			n := dumpContainerLogs()
			return fmt.Sprintf("Wrote %d container log file(s) to:\n%s", n, logDir())
		})
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(rows))
	case "enter", "tab":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		if r.Container != "" {
			return m, tuiShellCmd("Logs: "+r.Container, "docker", "logs", "--tail", "200", r.Container)
		}
		// log file: show the last ~400 lines
		return m, tuiDockerCmd("Log: "+r.Label, func() string {
			return tuiTailFile(r.Path, 400)
		})
	}
	return m, nil
}

// tuiLogDumpCmd refreshes the per-container log files in the background (fired
// once when the menu opens so the logs folder is populated). Fire-and-forget.
func tuiLogDumpCmd() tea.Cmd {
	return func() tea.Msg { dumpContainerLogs(); return nil }
}

// tuiTailFile returns the last n lines of a file (best-effort, full read).
func tuiTailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "Could not read " + path + ": " + err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return "(empty log)"
	}
	return strings.Join(lines, "\n")
}

// ===== from menu_dynamics.go =====

// menu_dynamics.go — Dynamics tab: lists the Traefik dynamic config files, ENTER
// shows the file content, "a" opens a per-file action popup (Art inject/strip,
// Repair, Regenerate, Force regen), "g" regenerates ALL. Mirrors draw_dynamics_tab.

type tuiDynRow struct {
	Name   string // basename
	Stack  string // name without extension
	Path   string
	SizeKB int64
}

func tuiDynRows() []tuiDynRow {
	dir := dispDynamicsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var rows []tuiDynRow
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, ".yaml") {
			continue
		}
		p := filepath.Join(dir, n)
		var kb int64
		if st, err := os.Stat(p); err == nil {
			kb = st.Size() / 1024
		}
		stack := strings.TrimSuffix(strings.TrimSuffix(n, ".yml"), ".yaml")
		rows = append(rows, tuiDynRow{Name: n, Stack: stack, Path: p, SizeKB: kb})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func (m menuModel) renderDynamics() string {
	rows := tuiDynRows()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  DYNAMIC CONFIGS — ENTER view · a Actions · g Gen ALL"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")
	if len(rows) == 0 {
		b.WriteString(tuiDimStyle.Render("  No dynamic configs found. Press g to generate from all stacks."))
		return b.String()
	}
	vis := m.visibleRows()
	end := m.scroll + vis
	if end > len(rows) {
		end = len(rows)
	}
	for i := m.scroll; i < end; i++ {
		r := rows[i]
		line := fmt.Sprintf("%-44s %6s", truncate(r.Name, 44), fmt.Sprintf("%dK", r.SizeKB))
		if i == m.sel {
			b.WriteString(tuiSelectedStyle.Render(truncate("  ▶ "+line, m.width-2)))
		} else {
			b.WriteString(tuiNormalStyle.Render(truncate("    "+line, m.width-2)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

var tuiDynActions = []tuiAction{
	{"🎨  Art Inject", "art_inject"},
	{"🧹  Art Strip", "art_strip"},
	{"🔧  Repair", "repair"},
	{"⚙  Regenerate", "gen"},
	{"⚙  Force Regen", "gen_force"},
	{"✕  Cancel", ""},
}

func (m menuModel) handleDynamicsKey(k string) (tea.Model, tea.Cmd) {
	rows := tuiDynRows()
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
		return m, tuiDockerCmd("Dynamic: "+r.Name, func() string { return tuiTailFile(r.Path, 400) })
	case "a", "A":
		if len(rows) == 0 {
			return m, nil
		}
		r := rows[m.sel]
		m.popup = tuiActionPopup("Dynamic: "+truncate(r.Stack, 22), tuiDynActions,
			func(action string) (menuModel, tea.Cmd) { return m.doDynamicsAction(r, action) })
	case "g", "G":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "f", "F":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	}
	return m, nil
}

func (m menuModel) doDynamicsAction(r tuiDynRow, action string) (menuModel, tea.Cmd) {
	switch action {
	case "", "cancel":
		return m, nil
	case "art_inject":
		return m, tuiSelfCmd("Art inject "+r.Stack, "art", "dynamic", "inject", r.Path)
	case "art_strip":
		return m, tuiSelfCmd("Art strip "+r.Stack, "art", "dynamic", "strip", r.Path)
	case "repair":
		return m, tuiSelfCmd("Repair "+r.Stack, "dynamics", "repair", r.Stack)
	case "gen":
		return m, tuiSelfCmd("Gen "+r.Stack, "dynamics", "generate", r.Stack)
	case "gen_force":
		return m, tuiSelfCmd("Force gen "+r.Stack, "dynamics", "generate", r.Stack, "force")
	}
	return m, nil
}

// ===== from menu_upgrade.go =====

// menu_upgrade.go — Upgrade tab: checks GitHub for a newer stacks program (via
// selfupdateStatus), shows the installed/latest commits + changelog, and ENTER
// applies the update (`stacks selfupdate apply [--force]`). Mirrors draw_upgrade_tab
// / do_upgrade_action. The status is cached so it isn't re-fetched every redraw.

// tuiUpgradeStatus caches the last self-update status snapshot.
var tuiUpgradeStatus map[string]interface{}
var tuiUpgradeFetched bool

func tuiEnsureUpgradeStatus() map[string]interface{} {
	if !tuiUpgradeFetched {
		tuiUpgradeStatus = selfupdateStatus()
		tuiUpgradeFetched = true
	}
	return tuiUpgradeStatus
}

func tuiUpgradeStr(st map[string]interface{}, key string) string {
	if v, ok := st[key].(string); ok {
		return v
	}
	return "?"
}

func (m menuModel) renderUpgrade() string {
	st := tuiEnsureUpgradeStatus()
	var b strings.Builder
	b.WriteString(tuiAccentStyle.Render("  UPGRADE — CHECK GITHUB FOR PROGRAM UPDATES"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	if e, ok := st["error"].(string); ok && e != "" {
		b.WriteString(tuiYellowStyle.Render("  ⚠ " + e))
		b.WriteString("\n\n")
		b.WriteString(tuiDimStyle.Render("  Set the clone path:  stacks config STACKS_REPO_DIR /path/to/clone"))
		return b.String()
	}

	b.WriteString(tuiCyanStyle.Render("  Program     : stacks (includes this menu)"))
	b.WriteString("\n")
	b.WriteString(tuiDimStyle.Render(fmt.Sprintf("  Repo        : %s  (%s)",
		tuiUpgradeStr(st, "repo"), tuiUpgradeStr(st, "branch"))))
	b.WriteString("\n")
	b.WriteString(tuiNormalStyle.Render("  Installed   : " + tuiUpgradeStr(st, "current")))
	b.WriteString("\n")
	b.WriteString(tuiNormalStyle.Render("  On GitHub   : " + tuiUpgradeStr(st, "latest")))
	b.WriteString("\n")
	if fe, ok := st["fetch_error"].(string); ok && fe != "" {
		b.WriteString(tuiYellowStyle.Render("  ⚠ couldn't reach GitHub: " + truncate(fe, maxInt(0, m.width-30))))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if up, _ := st["up_to_date"].(bool); up {
		b.WriteString(tuiGreenStyle.Render("  ✓ Up to date — nothing to install."))
		return b.String()
	}

	behind, _ := st["behind"].(int)
	b.WriteString(tuiYellowStyle.Render(fmt.Sprintf("  ⬆ %d update(s) available:", behind)))
	b.WriteString("\n")
	if cl, ok := st["changelog"].([]string); ok {
		limit := len(cl)
		max := maxInt(1, m.height-18)
		if limit > max {
			limit = max
		}
		for _, line := range cl[:limit] {
			b.WriteString(tuiNormalStyle.Render(truncate("    • "+line, m.width-2)))
			b.WriteString("\n")
		}
	}
	if dirty, _ := st["installed_dirty"].(bool); dirty {
		df, _ := st["dirty_files"].([]string)
		b.WriteString(tuiYellowStyle.Render(fmt.Sprintf(
			"  ⚠ installed copy has %d local change(s) that update would overwrite (a backup is made first).", len(df))))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(tuiAccentStyle.Render("  Press ENTER to update now."))
	return b.String()
}

func (m menuModel) handleUpgradeKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "r", "R":
		tuiUpgradeFetched = false
		return m, nil
	case "enter":
		st := tuiEnsureUpgradeStatus()
		if e, _ := st["error"].(string); e != "" {
			return m, nil
		}
		if up, _ := st["up_to_date"].(bool); up {
			return m, nil
		}
		force, _ := st["installed_dirty"].(bool)
		behind, _ := st["behind"].(int)
		label := "⬇  Yes — update now"
		if force {
			label += " (overwrite local)"
		}
		m.popup = tuiConfirmPopup(fmt.Sprintf("Install %d update(s) from GitHub?", behind), label,
			func() (menuModel, tea.Cmd) {
				tuiUpgradeFetched = false // force a re-check after applying
				if force {
					return m, tuiSelfCmd("Updating stacks", "selfupdate", "apply", "--force")
				}
				return m, tuiSelfCmd("Updating stacks", "selfupdate", "apply")
			})
		return m, nil
	}
	return m, nil
}

// ===== from menu_build.go =====

// menu_build.go — Build tab: scaffold a new service into a stack (image/service/
// stack prompts → `stacks build …`), regenerate dynamics, generate the sablier
// groups file, and run fix/repair across all stacks. Mirrors BUILD_ITEMS.
//
// The original curses Build wizard was a multi-step interactive popup; the TUI
// runs the same non-interactive `stacks build <image> <service> <stack>` engine
// behind three input prompts (and exposes the generator/fix helpers it shared).

var tuiBuildItems = []tuiAction{
	{"Build new service (wizard)", "build_into"},
	{"Create new stack + add service", "build_new_stack"},
	{"Generate dynamics from ALL stacks", "gen_dyn_all"},
	{"Generate dynamics from one stack", "gen_dyn_one"},
	{"Force regen ALL dynamics", "gen_dyn_force"},
	{"Generate global inject config", "gen_inject"},
	{"Generate sablier groups config", "gen_groups"},
	{"Run stacks fix on ALL", "fix_all"},
	{"Run stacks repair on ALL", "repair_all"},
}

func (m menuModel) renderBuild() string {
	return tuiRenderActionList("BUILD", tuiBuildItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleBuildKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiBuildItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiBuildItems) {
			return m, nil
		}
		return m.doBuildAction(tuiBuildItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doBuildAction(action string) (menuModel, tea.Cmd) {
	switch action {
	case "build_into":
		// Launch the REAL interactive build wizard (same 9-step flow as the
		// Python menu): stack pick → Docker Hub image search → IP → port →
		// name → DB detect/setup → redis → companion → scaffold. The Go engine
		// (cmdBuild) drives all the prompts itself, so we just suspend the TUI
		// and hand it the terminal. An optional service-name hint seeds the
		// image search; blank = pick everything in the wizard.
		m.popup = tuiInputPopup("Build wizard", "Service name to search for (blank = full wizard):", "",
			func(hint string) (menuModel, tea.Cmd) {
				if hint != "" && !tuiValidName(hint) {
					return m, nil
				}
				if hint == "" {
					return m, tuiExecSelf("build")
				}
				return m, tuiExecSelf("build", hint)
			})
		return m, nil
	case "build_new_stack":
		// Wizard that creates a brand-new stack file. Prompt for the new stack
		// name + a service hint, then run the interactive build engine — it
		// scaffolds a new <stack>.yml when the target doesn't exist yet.
		m.popup = tuiInputPopup("New stack", "New stack name (e.g. media_5):", "",
			func(stack string) (menuModel, tea.Cmd) {
				if stack == "" || !tuiValidName(stack) {
					return m, nil
				}
				m.popup = tuiInputPopup("New stack — service", "Service name to search for:", "",
					func(hint string) (menuModel, tea.Cmd) {
						if hint == "" || !tuiValidName(hint) {
							return m, nil
						}
						// `stacks build <hint> <stack>` → svc=hint, target=stack,
						// image empty → Hub search → full wizard into a new file.
						return m, tuiExecSelf("build", hint, stack)
					})
				return m, nil
			})
		return m, nil
	case "gen_dyn_all":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "gen_dyn_one":
		// Pick a single stack, regenerate just its dynamic config.
		m.popup = tuiInputPopup("Gen dynamics (one stack)", "Stack name:", "",
			func(stack string) (menuModel, tea.Cmd) {
				if stack == "" {
					return m, nil
				}
				return m, tuiSelfCmd("Gen dynamics "+stack, "dynamics", "generate", stack)
			})
		return m, nil
	case "gen_dyn_force":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	case "gen_inject":
		return m, tuiSelfCmd("Gen global inject", "__geninject")
	case "gen_groups":
		return m, tuiSelfCmd("Gen sablier groups", "__gensrvs")
	case "fix_all":
		return m, tuiSelfCmd("Fix ALL", "fix", "all")
	case "repair_all":
		return m, tuiSelfCmd("Repair ALL", "fix", "all", "repair")
	}
	return m, nil
}

// ===== from menu_art.go =====

// menu_art.go — Art tab: a static list of art/dynamics maintenance actions
// (inject/strip art across all stacks or dynamics, edit art.conf / stack_urls.conf,
// regenerate dynamics, repair dynamics). Mirrors ART_ITEMS / draw_art_tab.

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

// ===== from menu_backup.go =====

// menu_backup.go — Backup tab: run a full backup, take a pre-backup snapshot,
// clean old backups, and view the engine log files. Mirrors BACKUP_ITEMS /
// draw_backup_tab, wired to the Go backup + snapshot engines.

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

// ===== from loadingbar.go =====

// loadingbar.go — the TERMINAL loading UI for stacks commands (NOT the menu):
//   * banner()      — the STACKS ascii word-logo splash + version (top of every run)
//   * progressBar() — the little [████░░░] loading bar shown during up/fix/etc.
//   * stacksVersion() — commit-count·short-sha from the git clone (shown bottom-left)
// This is the one place to tweak how the loading art/bar looks.

// stacksRelease is the major version. Jumped to 3.0 for the milestone rewrite:
// v2.x = the Docker Engine API migration; v3.0 = the full Go rewrite (compiled,
// API-native). Bump this by hand for future milestones.
const stacksRelease = "3.1"
const stacksCodename = "Go" // v3.0 = the rewrite; v3.1 = loading bar everywhere + force + art both

// stacksVersion = "v3.0 (Go) · <commitcount>·<shortsha>". The release is explicit;
// the commit-count·sha (from the git clone, universal repoDir()) is the build tag.
func stacksVersion() string {
	rel := "v" + stacksRelease + " (" + stacksCodename + ")"
	repo := repoDir()
	if repo == "" {
		return rel + " · dev"
	}
	sha := gitOut(repo, "rev-parse", "--short", "HEAD")
	if sha == "" {
		return rel + " · dev"
	}
	cnt := gitOut(repo, "rev-list", "--count", "HEAD")
	if cnt == "" {
		cnt = "0"
	}
	return rel + " · " + cnt + "·" + sha
}

func gitOut(repo string, args ...string) string {
	full := append([]string{"-C", repo}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// banner prints the STACKS word-logo (trans-flag colors) + version, bottom-left.
func banner() {
	art := []string{
		"  ____  _____  _    ____ _  _______ ",
		" / ___||_   _|/ \\  / ___| |/ /  ___|",
		" \\___ \\  | | / _ \\| |   | ' /|___ \\",
		"  ___) | | |/ ___ \\ |___| . \\ ___) |",
		" |____/  |_/_/   \\_\\____|_|\\_\\____/ ",
	}
	cols := []int{117, 218, 231, 218, 117} // blue pink white pink blue
	for i, line := range art {
		fmt.Printf("\x1b[38;5;%dm%s\x1b[0m\n", cols[i], line)
	}
	fmt.Printf("\x1b[38;5;245m %s\x1b[0m\n", stacksVersion())
}

// progressBar draws the in-place loading bar: [██████░░░░] 60% <label>.
func progressBar(label string, pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	const w = 30
	filled := w * pct / 100
	bar := strings.Repeat("█", filled) + strings.Repeat("░", w-filled)
	fmt.Printf("\r\x1b[36m[%s]\x1b[0m %3d%%  %s\x1b[K", bar, pct, label)
}
