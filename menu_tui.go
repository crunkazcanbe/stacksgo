package main

// menu_tui.go — the Go TUI (Bubble Tea + Lip Gloss) replacement for
// stacks_menu.py. This file holds the styles, the data layer, the top-level
// model + Update/View dispatch, and the tab bar. Per-tab rendering and the
// action popups live in the menu_*.go siblings.
//
// All identifiers are tui*/menu* prefixed to avoid colliding with the ~38
// existing engine files. Universal paths only (configDir/stacksDir/etc).

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
)

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

	// Popup = a centered modal over the screen (like the Python menu), instead
	// of glued to the bottom under everything.
	if m.popup != nil {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, m.popup.render(m.width))
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

// tuiRenderTabs draws the scrolling tab bar with the active tab highlighted.
func tuiRenderTabs(active, width int) string {
	var parts []string
	for i, t := range tuiTabs {
		label := " " + t + " "
		if i == active {
			parts = append(parts, tuiTabActive.Render(label))
		} else {
			parts = append(parts, tuiTabInactive.Render(label))
		}
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
