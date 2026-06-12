package main

// buildcmd.go — faithful Go port of stacks_build.py.
//
// Interactive service scaffolder. Clean self-contained UI with a 9-line loading
// bar and inline questions. Ported line-for-line: the UI primitives (_draw,
// init_ui, update, clear_ui), ask, fzf, load_conf, hub_search, detect_db,
// find_existing, next_ip, rand_mac, setup_db, build_svc, probe_healthcheck,
// build_db_block, inject, and main().
//
// Paths use the universal helpers (stacksDir()/configDir()/home()); the Python
// hardcodes /home/bellzserver and /home/loveiznothin.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── UI — exactly 9 lines, never changes ───────────────────────────────────────

const buildUIH = 9 // MUST match lines printed in buildDraw()

var (
	buildStTarget = "build"
	buildStSvc    = "service"
	buildStAction = "Initializing..."
	buildStPct    = 0
	buildDrawn    = false
)

// buildTermCols mirrors the terminal-size probe in _draw(): try stdout, then
// stderr, capped at 120, defaulting to 80.
func buildTermCols() int {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if w, _, err := buildTermSize(f); err == nil && w > 0 {
			if w > 120 {
				return 120
			}
			return w
		}
	}
	return 80
}

// buildTermSize gets the terminal width/height via the COLUMNS env / stty as a
// portable fallback (Python uses os.get_terminal_size on the fd).
func buildTermSize(f *os.File) (int, int, error) {
	// Try `stty size` against the file's tty.
	cmd := exec.Command("stty", "size")
	cmd.Stdin = f
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) == 2 {
			rows, e1 := strconv.Atoi(parts[0])
			cols, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil {
				return cols, rows, nil
			}
		}
	}
	if c := os.Getenv("COLUMNS"); c != "" {
		if v, e := strconv.Atoi(c); e == nil {
			return v, 24, nil
		}
	}
	return 0, 0, fmt.Errorf("no tty size")
}

// buildClip mirrors Python's s[:n] slice (rune-safe).
func buildClip(s string, n int) string {
	if n < 0 {
		n = 0
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func buildDraw() {
	cols := buildTermCols()
	bw := 38
	if cols-18 < bw {
		bw = cols - 18
	}
	if bw < 0 {
		bw = 0
	}
	filled := (buildStPct * bw) / 100
	bar := strings.Repeat("=", filled) + strings.Repeat("-", bw-filled)

	if buildDrawn {
		fmt.Fprintf(os.Stdout, "\033[%dA", buildUIH)
	}

	actionClip := cols - 8
	if actionClip < 0 {
		actionClip = 0
	}

	fmt.Fprint(os.Stdout,
		"\r\033[K\033[38;5;81m     _             _        \033[0m\n"+
			"\r\033[K\033[38;5;81m ___| |_ __ _  ___| | _____ \033[0m\n"+
			"\r\033[K\033[38;5;81m/ __| __/ _` |/ __| |/ / __|\033[0m\n"+
			"\r\033[K\033[38;5;81m\\__ \\ || (_| | (__|   <\\__ \\\033[0m\n"+
			"\r\033[K\033[38;5;218m|___/\\__\\__,_|\\___|_|\\_\\___/ \033[0m\n"+
			"\r\033[K\n"+
			fmt.Sprintf("\r\033[K\033[1;33m  📦 %s \033[90m|\033[0m \033[1;36m%s\033[0m\n",
				buildClip(buildStTarget, 30), buildClip(buildStSvc, 35))+
			fmt.Sprintf("\r\033[K\033[1;34m  ▶ %s\033[0m\n", buildClip(buildStAction, actionClip))+
			fmt.Sprintf("\r\033[K\033[1;32m  [%s] %d%%\033[0m\n", bar, buildStPct),
	)
	os.Stdout.Sync()
	buildDrawn = true
}

func buildInitUI(target, svc string) {
	buildStTarget = target
	buildStSvc = svc
	buildDrawn = false
	fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
	os.Stdout.Sync()
	buildDraw()
}

func buildUpdate(action string, pct int) {
	buildStAction = action
	buildStPct = pct
	buildDraw()
}

func buildClearUI() {
	if buildDrawn {
		fmt.Fprintf(os.Stdout, "\033[%dA\033[J", buildUIH)
		os.Stdout.Sync()
	}
}

// ── Ask — prints on line BELOW the bar, then erases ──────────────────────────

func buildAsk(prompt, def string) string {
	buildStAction = "❓ " + prompt
	buildUpdate(buildStAction, buildStPct)
	exec.Command("stty", "echo").Run()
	fmt.Fprintf(os.Stdout, "  \033[1;36m%s\033[0m [\033[1;33m%s\033[0m]: ", prompt, def)
	os.Stdout.Sync()
	reader := bufio.NewReader(os.Stdin)
	// Python: sys.stdin.readline() returns "" at EOF WITHOUT raising, so an EOF
	// (e.g. Ctrl-D) yields val=="" and the function returns the default — it does
	// NOT exit. (Ctrl-C raises KeyboardInterrupt and would terminate the process
	// via signal here anyway.) So treat a plain read error the same as empty input.
	line, _ := reader.ReadString('\n')
	val := strings.TrimSpace(line)
	// erase prompt line
	fmt.Fprint(os.Stdout, "\033[1A\r\033[K")
	os.Stdout.Sync()
	if val != "" {
		return val
	}
	return def
}

// ── fzf ───────────────────────────────────────────────────────────────────────

func buildFzf(items []string, header, prompt string) string {
	if len(items) == 0 {
		return ""
	}
	if prompt == "" {
		prompt = "▶ "
	}
	if _, err := exec.LookPath("fzf"); err != nil {
		for i, it := range items {
			fmt.Printf("  %d. %s\n", i+1, it)
		}
		v := buildAsk("Number", "1")
		// Python: items[int(v)-1] with a bare except -> items[0]. Mirror Python
		// list indexing, including negative wrap (v="0" -> items[-1] = last item).
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
			idx := n - 1
			if idx < 0 {
				idx += len(items)
			}
			if idx >= 0 && idx < len(items) {
				return items[idx]
			}
		}
		return items[0]
	}
	inp := strings.Join(items, "\n")
	tf, err := os.CreateTemp("", "*.txt")
	if err != nil {
		return ""
	}
	tfp := tf.Name()
	tf.WriteString(inp)
	tf.Close()

	cmdStr := fmt.Sprintf("cat %s | fzf --ansi --no-sort --layout=reverse "+
		"--height=~50%% --border=rounded --margin=1,3 "+
		"--header=%s --prompt=%s "+
		"--color=bg:#0a1628,bg+:#1a3a5c,fg:#c8d8e8,fg+:#ffffff,"+
		"hl:#4fc3f7,border:#2a6496,header:#4fc3f7,prompt:#81d4fa",
		buildShellQuote(tfp), buildShellQuote(header), buildShellQuote(prompt))

	tty, terr := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if terr != nil {
		os.Remove(tfp)
		return ""
	}
	defer tty.Close()
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Stderr = tty
	out, runErr := cmd.Output()
	os.Remove(tfp)

	rc := 0
	if runErr != nil {
		rc = 1
		if ee, ok := runErr.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
	}
	if rc != 0 {
		// Restore terminal after fzf exit
		exec.Command("tput", "reset").Run()
		buildDrawn = false
		fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
		os.Stdout.Sync()
		return ""
	}
	outStr := strings.TrimSpace(string(out))
	var result string
	if outStr != "" {
		result = strings.SplitN(outStr, "\n", 2)[0]
	}
	// Restore terminal cleanly after fzf
	exec.Command("tput", "reset").Run()
	buildDrawn = false
	fmt.Fprint(os.Stdout, strings.Repeat("\n", buildUIH))
	os.Stdout.Sync()
	return result
}

// buildShellQuote mirrors shlex.quote for embedding in `sh -c`.
func buildShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if regexp.MustCompile(`^[a-zA-Z0-9_@%+=:,./-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ── Config ────────────────────────────────────────────────────────────────────

// buildConf is the resolved build config (mirrors the dict from load_conf()).
type buildConf struct {
	UseCommonCaps   bool
	ExtraNetworks   []interface{}
	Cpuset          string
	CPUShares       int
	StopGracePeriod string
	StopSignal      string
	Restart         string
	User            string
	Blkio           bool
	Ulimits         bool
	DeployLimits    bool
	Logging         bool
	DNS             []string
	SablierGroup    string
	ExtraEnv        []string
	ExtraLabels     []string
	ExtraVolumes    []string
}

func buildLoadConf() buildConf {
	c := buildConf{
		UseCommonCaps:   true,
		ExtraNetworks:   []interface{}{},
		Cpuset:          "0-15",
		CPUShares:       4096,
		StopGracePeriod: "120s",
		StopSignal:      "SIGTERM",
		Restart:         "no",
		User:            "0:0",
		Blkio:           true,
		Ulimits:         true,
		DeployLimits:    true,
		Logging:         true,
		DNS:             []string{"192.168.1.114", "8.8.8.8"},
		SablierGroup:    "",
		ExtraEnv:        []string{"TZ=America/New_York"},
		ExtraLabels:     []string{},
		ExtraVolumes: []string{
			"/usr/lib/x86_64-linux-gnu/libtcmalloc_minimal.so.4:" +
				"/usr/lib/x86_64-linux-gnu/libtcmalloc_minimal.so.4:ro"},
	}
	// loadDoc('build') reads build.yaml / build.conf via the shared config layer
	// (equivalent to importing stacks_config and falling back to BUILD_CONF JSON).
	doc := loadDoc("build")
	for k, v := range doc {
		if strings.HasPrefix(k, "_") {
			continue
		}
		buildApplyConf(&c, k, v)
	}
	return c
}

func buildApplyConf(c *buildConf, key string, v interface{}) {
	switch key {
	case "use_common_caps":
		c.UseCommonCaps = buildAsBool(v, c.UseCommonCaps)
	case "extra_networks":
		if l, ok := v.([]interface{}); ok {
			c.ExtraNetworks = l
		}
	case "cpuset":
		c.Cpuset = buildAsStr(v, c.Cpuset)
	case "cpu_shares":
		c.CPUShares = buildAsInt(v, c.CPUShares)
	case "stop_grace_period":
		c.StopGracePeriod = buildAsStr(v, c.StopGracePeriod)
	case "stop_signal":
		c.StopSignal = buildAsStr(v, c.StopSignal)
	case "restart":
		c.Restart = buildAsStr(v, c.Restart)
	case "user":
		c.User = buildAsStr(v, c.User)
	case "blkio":
		c.Blkio = buildAsBool(v, c.Blkio)
	case "ulimits":
		c.Ulimits = buildAsBool(v, c.Ulimits)
	case "deploy_limits":
		c.DeployLimits = buildAsBool(v, c.DeployLimits)
	case "logging":
		c.Logging = buildAsBool(v, c.Logging)
	case "dns":
		c.DNS = buildAsStrList(v, c.DNS)
	case "sablier_group":
		c.SablierGroup = buildAsStr(v, c.SablierGroup)
	case "extra_env":
		c.ExtraEnv = buildAsStrList(v, c.ExtraEnv)
	case "extra_labels":
		c.ExtraLabels = buildAsStrList(v, c.ExtraLabels)
	case "extra_volumes":
		c.ExtraVolumes = buildAsStrList(v, c.ExtraVolumes)
	}
}

func buildAsBool(v interface{}, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes"
	}
	return def
}

func buildAsStr(v interface{}, def string) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.Itoa(int(t))
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return def
}

func buildAsInt(v interface{}, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		if n, e := strconv.Atoi(strings.TrimSpace(t)); e == nil {
			return n
		}
	}
	return def
}

func buildAsStrList(v interface{}, def []string) []string {
	if l, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(l))
		for _, it := range l {
			out = append(out, buildAsStr(it, ""))
		}
		return out
	}
	return def
}

// ── Registry search — uses stacks regsearch TUI ───────────────────────────────

func buildHubSearch(term string) string {
	buildUpdate("Searching registries for: "+term, 15)
	// Clear UI so regsearch has full screen
	buildClearUI()
	// Use THIS binary's native regsearch (12 registries: Docker Hub, Hub
	// Official, ghcr.io, Self-Hosted, Quay, GitLab, Verified/AWS, Codeberg,
	// LinuxServer.io, Bitnami, Microsoft MCR, ArtifactHub). --select writes the
	// chosen image to /tmp/stacks_build_selected and exits. No Python.
	os.Remove("/tmp/stacks_build_selected")
	cmd := exec.Command(selfExe(), "regsearch", term, "--select")
	cmd.Env = dockerEnv()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
	// regsearch writes selected image to /tmp/stacks_build_selected
	selPath := "/tmp/stacks_build_selected"
	if data, err := os.ReadFile(selPath); err == nil {
		image := strings.TrimSpace(string(data))
		os.Remove(selPath)
		if image != "" {
			// Reset UI state so it redraws from scratch after TUI
			buildDrawn = false
			return image
		}
	}
	return ""
}

// ── Detect db needs ───────────────────────────────────────────────────────────

func buildDetectDB(image string) map[string]bool {
	buildUpdate("Inspecting image for requirements...", 30)
	reqs := map[string]bool{"postgres": false, "mysql": false, "redis": false, "mongo": false}

	data := buildInspectImageJSON(image)
	if data == nil {
		// docker pull then re-inspect
		buildDockerPull(image)
		data = buildInspectImageJSON(image)
	}
	if len(data) == 0 {
		return reqs
	}
	cfg, _ := data[0]["Config"].(map[string]interface{})
	text := ""
	if cfg != nil {
		if env, ok := cfg["Env"].([]interface{}); ok {
			for _, e := range env {
				if s, ok := e.(string); ok {
					text += " " + s
				}
			}
		}
		if labels, ok := cfg["Labels"].(map[string]interface{}); ok {
			for _, lv := range labels {
				if s, ok := lv.(string); ok {
					text += " " + s
				}
			}
		}
	}
	if regexp.MustCompile(`(?i)POSTGRES|DATABASE_URL.*post|PGHOST`).MatchString(text) {
		reqs["postgres"] = true
	}
	if regexp.MustCompile(`(?i)MYSQL|MARIADB`).MatchString(text) {
		reqs["mysql"] = true
	}
	if regexp.MustCompile(`(?i)REDIS|REDIS_HOST`).MatchString(text) {
		reqs["redis"] = true
	}
	if regexp.MustCompile(`(?i)MONGO`).MatchString(text) {
		reqs["mongo"] = true
	}
	return reqs
}

// buildInspectImageJSON runs `docker inspect <image>` (10s timeout) returning the
// parsed JSON array, or nil on failure (mirrors subprocess + json.loads).
func buildInspectImageJSON(image string) []map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", image)
	cmd.Env = dockerEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil {
		return nil
	}
	return arr
}

func buildDockerPull(image string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Env = dockerEnv()
	cmd.Run()
}

// ── Find existing db containers ───────────────────────────────────────────────

// buildDBRec mirrors the per-db dict used throughout (find_existing/setup_db).
type buildDBRec struct {
	Type     string
	Name     string
	IP       string
	Port     string
	Stack    string
	Image    string
	Password string
	DBName   string
	Net      string
	New      bool
}

func buildFindExisting(dbType string) []buildDBRec {
	var found []buildDBRec
	pats := map[string]string{
		"postgres": `postgres`, "mysql": `mysql|mariadb`,
		"redis": `redis`, "mongo": `mongo`}
	patStr := pats[dbType]
	var pat *regexp.Regexp
	if patStr != "" {
		pat = regexp.MustCompile(`(?i)` + patStr)
	}

	files := buildListYML(stacksDir())
	sort.Strings(files)

	reServices := regexp.MustCompile(`^services:`)
	reTop := regexp.MustCompile(`^[a-zA-Z]`)
	reSvc := regexp.MustCompile(`^  ([a-zA-Z0-9_.\-]+):\s*$`)
	reImg := regexp.MustCompile(`\s+image:\s+(.+)`)
	reIPPort := regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+):(\d+):\d+`)

	matchPat := func(s string) bool {
		// Python: re.search(pat, img, re.I). An empty pattern (unknown db_type)
		// matches everything in Python, so mirror that here.
		if pat == nil {
			return true
		}
		return pat.MatchString(s)
	}

	for _, f := range files {
		content, err := os.ReadFile(filepath.Join(stacksDir(), f))
		if err != nil {
			continue
		}
		inSvc := false
		cur, img, ip, port := "", "", "", ""
		for _, line := range strings.Split(string(content), "\n") {
			if reServices.MatchString(line) {
				inSvc = true
				continue
			}
			if reTop.MatchString(line) && !strings.HasPrefix(line, " ") {
				inSvc = false
			}
			if !inSvc {
				continue
			}
			if m := reSvc.FindStringSubmatch(line); m != nil {
				if cur != "" && matchPat(img) {
					found = append(found, buildDBRec{Name: cur, Image: img, IP: ip, Port: port, Stack: f})
				}
				cur = m[1]
				img, ip, port = "", "", ""
				continue
			}
			if cur != "" {
				if mi := reImg.FindStringSubmatch(line); mi != nil {
					img = strings.TrimSpace(mi[1])
				}
				if mp := reIPPort.FindStringSubmatch(line); mp != nil {
					ip = mp[1]
					port = mp[2]
				}
			}
		}
		if cur != "" && matchPat(img) {
			found = append(found, buildDBRec{Name: cur, Image: img, IP: ip, Port: port, Stack: f})
		}
	}
	return found
}

// buildListYML lists *.yml filenames (not full paths) in a dir.
func buildListYML(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, e.Name())
		}
	}
	return out
}

func buildNextIP() string {
	used := map[int]bool{}
	re := regexp.MustCompile(`192\.168\.1\.(\d+)`)
	for _, f := range buildListYML(stacksDir()) {
		data, err := os.ReadFile(filepath.Join(stacksDir(), f))
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if n, e := strconv.Atoi(m[1]); e == nil {
				used[n] = true
			}
		}
	}
	for i := 150; i < 254; i++ {
		if !used[i] {
			return fmt.Sprintf("192.168.1.%d", i)
		}
	}
	return "192.168.1.200"
}

func buildRandMac() string {
	return fmt.Sprintf("02:42:ac:%02x:%02x:%02x",
		rand.Intn(256), rand.Intn(256), rand.Intn(256))
}

// ── DB setup ──────────────────────────────────────────────────────────────────

func buildSetupDB(dbType, svcName string) *buildDBRec {
	buildUpdate(fmt.Sprintf("Setting up %s...", dbType), 55)
	existing := buildFindExisting(dbType)

	var dbStacks []string
	reDB := regexp.MustCompile(`db_\d+\.yml`)
	for _, f := range buildListYML(stacksDir()) {
		if reDB.MatchString(f) {
			dbStacks = append(dbStacks, f)
		}
	}
	sort.Strings(dbStacks)

	var choices []string
	for _, e := range existing {
		ipStr := "no IP"
		if e.IP != "" {
			ipStr = fmt.Sprintf("%s:%s", e.IP, e.Port)
		}
		choices = append(choices, fmt.Sprintf("USE  %s  (%s)  [%s]", e.Name, ipStr, e.Stack))
	}
	choices = append(choices, fmt.Sprintf("NEW  Create new %s container", dbType))

	choice := buildFzf(choices, fmt.Sprintf("Use existing %s or create new?", dbType), "")
	if choice == "" {
		return nil
	}
	if strings.HasPrefix(choice, "USE") {
		flds := strings.Fields(choice)
		if len(flds) < 2 {
			return nil
		}
		name := flds[1]
		var match *buildDBRec
		for i := range existing {
			if existing[i].Name == name {
				match = &existing[i]
				break
			}
		}
		if match == nil {
			return nil
		}
		return &buildDBRec{Type: dbType, Name: name, IP: match.IP,
			Port: match.Port, Stack: match.Stack,
			New: false, Net: svcName + "_net"}
	}

	sc := buildFzf(dbStacks, "Which db stack?", "")
	if sc == "" {
		sc = "db_0.yml"
	}
	defs := map[string][2]string{
		"postgres": {"postgres:16-alpine", "5432"},
		"mysql":    {"mariadb:10.11", "3306"},
		"redis":    {"redis:7-alpine", "6379"},
		"mongo":    {"mongo:7", "27017"},
	}
	def, ok := defs[dbType]
	if !ok {
		def = [2]string{"postgres:16-alpine", "5432"}
	}
	img, port := def[0], def[1]
	dbName := buildAsk("DB container name", fmt.Sprintf("%s-%s", svcName, dbType))
	dbIP := buildAsk("DB IP address", buildNextIP())
	dbPass := buildAsk("DB password", "bellzpass")
	dbDB := buildAsk("DB name", strings.ReplaceAll(svcName, "-", "_"))
	return &buildDBRec{Type: dbType, Name: dbName, IP: dbIP, Port: port,
		Image: img, Password: dbPass, DBName: dbDB,
		Stack: sc, New: true, Net: svcName + "_net"}
}

// ── Build YAML blocks ─────────────────────────────────────────────────────────

func buildSvc(name, image, ip, port string, cfg buildConf, svcNum int, db, redis *buildDBRec) string {
	net := name + "_net"
	mac := buildRandMac()
	nets := fmt.Sprintf("    networks:\n      traefik_net:\n        priority: 1000\n"+
		"      %s:\n        priority: 500", net)
	for _, xn := range cfg.ExtraNetworks {
		if m, ok := xn.(map[string]interface{}); ok {
			for nn, np := range m {
				nets += fmt.Sprintf("\n      %s:\n        priority: %v", nn, np)
			}
		}
	}
	var env []string
	for _, e := range cfg.ExtraEnv {
		env = append(env, fmt.Sprintf(`      - "%s"`, e))
	}
	if db != nil {
		dt, dip, dport := db.Type, db.IP, db.Port
		dpw := db.Password
		if dpw == "" {
			dpw = "pass"
		}
		ddb := db.DBName
		if ddb == "" {
			ddb = name
		}
		switch dt {
		case "postgres":
			env = append(env, fmt.Sprintf(`      - "DATABASE_URL=postgresql://postgres:%s@%s:%s/%s"`, dpw, dip, dport, ddb))
		case "mysql":
			env = append(env, fmt.Sprintf(`      - "DATABASE_URL=mysql://root:%s@%s:%s/%s"`, dpw, dip, dport, ddb))
		case "redis":
			env = append(env, fmt.Sprintf(`      - "REDIS_URL=redis://%s:%s/0"`, dip, dport))
		case "mongo":
			env = append(env, fmt.Sprintf(`      - "MONGODB_URI=mongodb://%s:%s/%s"`, dip, dport, ddb))
		}
	}
	if redis != nil {
		rip := redis.IP
		if rip == "" {
			rip = "127.0.0.1"
		}
		rpt := redis.Port
		if rpt == "" {
			rpt = "6379"
		}
		env = append(env, fmt.Sprintf(`      - "REDIS_URL=redis://%s:%s/0"`, rip, rpt))
	}
	envB := ""
	if len(env) > 0 {
		envB = "    environment:\n" + strings.Join(env, "\n") + "\n"
	}

	vols := []string{fmt.Sprintf(`      - "%s/docker/%s:/data"`, home(), name)}
	for _, v := range cfg.ExtraVolumes {
		vols = append(vols, fmt.Sprintf(`      - "%s"`, v))
	}

	sg := cfg.SablierGroup
	if sg == "" {
		sg = strings.ReplaceAll(strings.ReplaceAll(name, "-", ""), "_", "")
	}
	labels := []string{
		`      - "traefik.enable=true"`,
		`      - "sablier.enable=true"`,
		fmt.Sprintf(`      - "sablier.group=%s"`, sg),
	}
	for _, l := range cfg.ExtraLabels {
		labels = append(labels, fmt.Sprintf(`      - "%s"`, l))
	}

	useCaps := cfg.UseCommonCaps
	caps := ""
	if useCaps {
		caps = "    <<: *common-caps\n"
	}
	blkio := ""
	if cfg.Blkio {
		blkio = "    blkio_config: {weight: 500, device_read_bps: [{path: /dev/nvme0n1, rate: 300mb}], device_write_bps: [{path: /dev/nvme0n1, rate: 300mb}]}\n"
	}
	ulim := ""
	if cfg.Ulimits {
		ulim = "    ulimits: {memlock: {soft: -1, hard: -1}, nofile: {soft: 65535, hard: 65535}, nproc: 65535}\n"
	}
	dep := ""
	if cfg.DeployLimits {
		dep = "    deploy: {resources: {limits: {memory: 2G, cpus: '4.0', pids: 1000}, reservations: {memory: 256M, cpus: '0.5'}}}\n"
	}
	log := ""
	if !useCaps && cfg.Logging {
		log = "    logging: {driver: json-file, options: {max-size: 50m, max-file: '5'}}\n"
	}
	// Python: cfg.get("dns", [default]) — the default only applies when the key
	// is missing, NOT when it is an explicit empty list. load_conf always sets
	// the key, so we use cfg.DNS verbatim (empty list -> empty dns block).
	dnsList := cfg.DNS
	var dnsParts []string
	for _, d := range dnsList {
		dnsParts = append(dnsParts, fmt.Sprintf(`      - "%s"`, d))
	}
	dns := strings.Join(dnsParts, "\n")
	num := fmt.Sprintf("%02d", svcNum)
	hc := buildProbeHealthcheck(image, port)

	// The per-line settings block (only when not using common caps).
	settings := ""
	if !useCaps {
		settings = fmt.Sprintf("    cpuset: \"%s\"\n    cpu_shares: %d\n    stop_grace_period: %s\n    stop_signal: %s\n    restart: %s\n    user: \"%s\"\n",
			cfg.Cpuset, cfg.CPUShares, cfg.StopGracePeriod, cfg.StopSignal, cfg.Restart, cfg.User)
	}
	dnsBlock := ""
	if !useCaps {
		dnsBlock = "    dns:\n" + dns + "\n"
	}

	volsJoined := ""
	{
		var b []string
		for _, v := range vols {
			b = append(b, "  "+v)
		}
		volsJoined = strings.Join(b, "\n")
	}
	labelsJoined := ""
	{
		var b []string
		for _, l := range labels {
			b = append(b, "  "+l)
		}
		labelsJoined = strings.Join(b, "\n")
	}

	return fmt.Sprintf(`
  # ---------------------------------------------------------
  # %s. %s 🐳
  # Description: %s service — edit description here ✅
  # ---------------------------------------------------------
  %s:
%s    image: %s
    container_name: %s
    hostname: %s
    domainname: %s.loveiznothin.com
    mac_address: "%s"
%s%s
    ports:
      - "%s:%s:%s"
%s    volumes:
%s
    labels:
%s
%s%s%s%s%s%s`,
		num, strings.ToUpper(name),
		image,
		name,
		caps, image,
		name,
		name,
		name,
		mac,
		settings, nets,
		ip, port, port,
		envB,
		volsJoined,
		labelsJoined,
		dnsBlock, hc, blkio, ulim, dep, log)
}

func buildProbeHealthcheck(image, port string) string {
	// Inspect the IMAGE and pick a healthcheck that fits what's inside.
	probe := "command -v nc   >/dev/null 2>&1 && echo HAS_nc; " +
		"command -v wget >/dev/null 2>&1 && echo HAS_wget; " +
		"command -v curl >/dev/null 2>&1 && echo HAS_curl; echo SHELLOK"
	out := ""
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"--entrypoint", "sh", image, "-c", probe)
	env := os.Environ()
	if os.Getenv("DOCKER_HOST") == "" {
		env = append(env, "DOCKER_HOST=unix:///var/run/docker.sock")
	}
	cmd.Env = env
	// Python concatenates r.stdout + r.stderr; CombinedOutput captures both.
	combined, _ := cmd.CombinedOutput()
	out = string(combined)

	if !strings.Contains(out, "SHELLOK") {
		return "" // distroless / no shell
	}
	var test string
	switch {
	case strings.Contains(out, "HAS_nc"):
		test = fmt.Sprintf("nc -z 127.0.0.1 %s || exit 1", port)
	case strings.Contains(out, "HAS_wget"):
		test = fmt.Sprintf("wget -qO- http://127.0.0.1:%s/ || exit 1", port)
	case strings.Contains(out, "HAS_curl"):
		test = fmt.Sprintf("curl -sf http://127.0.0.1:%s/ || exit 1", port)
	default:
		return "" // shell but no probe tool
	}
	return "    healthcheck:\n" +
		fmt.Sprintf("      test: [\"CMD-SHELL\", \"%s\"]\n", test) +
		"      interval: 10s\n" +
		"      timeout: 5s\n" +
		"      retries: 10\n" +
		"      start_period: 30s\n"
}

func buildDBBlock(db *buildDBRec, svcName string) (string, string) {
	dt, name, ip, port := db.Type, db.Name, db.IP, db.Port
	img := db.Image
	if img == "" {
		img = "postgres:16-alpine"
	}
	pw := db.Password
	if pw == "" {
		pw = "bellzpass"
	}
	dbn := db.DBName
	if dbn == "" {
		dbn = strings.ReplaceAll(svcName, "-", "_")
	}
	net := svcName + "_net"
	mac := buildRandMac()
	envMap := map[string]string{
		"postgres": fmt.Sprintf("      - \"POSTGRES_PASSWORD=%s\"\n      - \"POSTGRES_DB=%s\"", pw, dbn),
		"mysql":    fmt.Sprintf("      - \"MYSQL_ROOT_PASSWORD=%s\"\n      - \"MARIADB_DATABASE=%s\"", pw, dbn),
		"redis":    `      - "REDIS_REPLICATION_MODE=master"`,
		"mongo":    fmt.Sprintf("      - \"MONGO_INITDB_DATABASE=%s\"", dbn),
	}
	vol := name + "-data"
	block := fmt.Sprintf(`
  # ---------------------------------------------------------
  # %s — %s for %s 🐳
  # ---------------------------------------------------------
  %s:
    image: %s
    container_name: %s
    hostname: %s
    mac_address: "%s"
    restart: "no"
    networks:
      %s:
        priority: 1000
    ports:
      - "%s:%s:%s"
    environment:
%s
    volumes:
      - "%s:/var/lib/postgresql/data"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
`,
		strings.ToUpper(name), strings.ToUpper(dt), svcName,
		name,
		img,
		name,
		name,
		mac,
		net,
		ip, port, port,
		envMap[dt],
		vol)
	return block, vol
}

// ── Inject into stack file ────────────────────────────────────────────────────

func buildInject(stack, block, network, volume string) bool {
	fpath := filepath.Join(stacksDir(), stack)
	if !strings.HasSuffix(fpath, ".yml") {
		fpath += ".yml"
	}
	st, err := os.Stat(fpath)
	if err != nil || st.IsDir() {
		return false
	}
	data, err := os.ReadFile(fpath)
	if err != nil {
		return false
	}
	content := string(data)

	// Duplicate check
	reSvc := regexp.MustCompile(`(?m)^  ([a-zA-Z0-9_.\-]+):`)
	m := reSvc.FindStringSubmatch(block)
	if m != nil {
		dup := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(m[1]) + `:`)
		if dup.MatchString(content) {
			fmt.Printf("\n  \033[1;31m✘ %s already exists in %s\033[0m\n", m[1], stack)
			return false
		}
	}

	if network != "" && !strings.Contains(content, network) {
		re := regexp.MustCompile(`(?m)^(networks:\n)`)
		content = buildReplaceFirst(re, content,
			fmt.Sprintf("${1}  %s: {name: %s, external: true}\n", network, network))
	}
	if volume != "" && !strings.Contains(content, volume) {
		re := regexp.MustCompile(`(?m)^(volumes:\n)`)
		content = buildReplaceFirst(re, content,
			fmt.Sprintf("${1}  %s: {name: %s, external: true}\n", volume, volume))
	}

	if strings.Contains(content, "##BELLZART_START_FOOTER") {
		content = strings.Replace(content, "##BELLZART_START_FOOTER",
			strings.TrimRight(block, "\n")+"\n\n##BELLZART_START_FOOTER", 1)
	} else {
		// Find last top-level # line (footer art) and insert before it
		lines := buildSplitKeepEnds(content)
		insert := len(lines)
		for i := len(lines) - 1; i >= 0; i-- {
			if !strings.HasPrefix(lines[i], "#") && strings.TrimSpace(lines[i]) != "" {
				insert = i + 1
				break
			}
		}
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:insert]...)
		newLines = append(newLines, strings.TrimRight(block, "\n")+"\n\n")
		newLines = append(newLines, lines[insert:]...)
		content = strings.Join(newLines, "")
	}
	return os.WriteFile(fpath, []byte(content), 0644) == nil
}

// buildReplaceFirst mirrors re.sub(..., count=1): replace only the first match,
// expanding ${1}-style group references in the replacement (count=1, MULTILINE).
func buildReplaceFirst(re *regexp.Regexp, s, repl string) string {
	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return s
	}
	expanded := re.ExpandString(nil, repl, s, loc)
	return s[:loc[0]] + string(expanded) + s[loc[1]:]
}

// buildSplitKeepEnds mirrors str.splitlines(keepends=True).
func buildSplitKeepEnds(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ── Main ──────────────────────────────────────────────────────────────────────

func cmdBuild(argv []string) {
	// args = [a for a in sys.argv[1:] if a not in ('--progress',) and not startswith('/tmp/')]
	var args []string
	for _, a := range argv {
		if a == "--progress" || strings.HasPrefix(a, "/tmp/") {
			continue
		}
		args = append(args, a)
	}
	var log []string
	cfg := buildLoadConf()

	image, svcName, targetStack := "", "", ""
	if len(args) >= 3 {
		image = args[0]
		svcName = args[1]
		targetStack = args[2]
	} else if len(args) == 2 {
		svcName = args[0]
		targetStack = args[1]
	} else if len(args) == 1 {
		svcName = args[0]
	}

	uiTarget := targetStack
	if uiTarget == "" {
		uiTarget = "build"
	}
	uiSvc := svcName
	if uiSvc == "" {
		uiSvc = "service"
	}
	buildInitUI(uiTarget, uiSvc)

	// ── 1. Stack selection ─────────────────────────────────────────────────
	if targetStack == "" {
		buildUpdate("Select target stack...", 5)
		var stacks []string
		for _, f := range buildListYML(stacksDir()) {
			if !strings.HasPrefix(f, "db_") {
				stacks = append(stacks, strings.TrimSuffix(f, ".yml"))
			}
		}
		sort.Strings(stacks)
		targetStack = buildFzf(stacks, "Which stack to add service to?", "")
		if targetStack == "" {
			buildClearUI()
			fmt.Println("  \033[1;31m✘ Cancelled.\033[0m")
			os.Exit(1)
		}
		buildStTarget = targetStack
	}

	// ── 2. Image search ────────────────────────────────────────────────────
	if image == "" {
		buildUpdate(fmt.Sprintf("Searching Docker Hub for %s...", svcName), 10)
		image = buildHubSearch(svcName)
		if image == "" {
			buildClearUI()
			fmt.Println("  \033[1;31m✘ Cancelled.\033[0m")
			os.Exit(1)
		}
		log = append(log, "Image: "+image)
	}

	buildUpdate("Image: "+image, 20)

	// ── 3. Service details ─────────────────────────────────────────────────
	buildUpdate("Getting service details...", 25)
	svcIP := buildAsk("Service IP (192.168.1.x)", buildNextIP())
	svcPort := buildAsk("Service port", "8080")
	svcNameIn := buildAsk("Container name", svcName)
	if svcNameIn != "" {
		svcName = svcNameIn
	}
	log = append(log, fmt.Sprintf("Name: %s  IP: %s", svcName, svcIP))

	// ── 4. Detect & configure database ────────────────────────────────────
	buildUpdate("Inspecting image...", 30)
	reqs := buildDetectDB(image)
	var detected []string
	// preserve insertion order (postgres,mysql,redis,mongo)
	for _, k := range []string{"postgres", "mysql", "redis", "mongo"} {
		if reqs[k] {
			detected = append(detected, k)
		}
	}
	if len(detected) > 0 {
		buildUpdate("Detected: "+strings.Join(detected, ", "), 35)
	}

	var dbInfo *buildDBRec
	var redisInfo *buildDBRec
	for _, dt := range []string{"postgres", "mysql", "redis", "mongo"} {
		if !reqs[dt] {
			continue
		}
		// Check if a db for this service already exists
		existing := buildFindExisting(dt)
		svcKey := strings.ReplaceAll(strings.ReplaceAll(svcName, "-", ""), "_", "")
		var already []buildDBRec
		for _, e := range existing {
			eKey := strings.ReplaceAll(strings.ReplaceAll(e.Name, "-", ""), "_", "")
			if strings.Contains(eKey, svcKey) {
				already = append(already, e)
			}
		}
		if len(already) > 0 {
			e := already[0]
			fmt.Printf("  \033[1;32m✔ Found existing %s: %s (%s:%s)\033[0m\n", dt, e.Name, e.IP, e.Port)
			info := &buildDBRec{Type: dt, Name: e.Name, IP: e.IP,
				Port: e.Port, Stack: e.Stack, New: false,
				Net: svcName + "_net"}
			if dt == "redis" {
				redisInfo = info
			} else {
				dbInfo = info
			}
		} else {
			fmt.Printf("  \033[1;33m⚠ No existing %s found for %s\033[0m\n", dt, svcName)
			yn := buildAsk(fmt.Sprintf("Add a new %s database? (y/n)", dt), "y")
			if strings.ToLower(yn) == "y" {
				var dbStacks []string
				reDB := regexp.MustCompile(`db_\d+\.yml`)
				for _, f := range buildListYML(stacksDir()) {
					if reDB.MatchString(f) {
						dbStacks = append(dbStacks, strings.TrimSuffix(f, ".yml"))
					}
				}
				sort.Strings(dbStacks)
				buildUpdate(fmt.Sprintf("Select db stack for %s...", dt), 50)
				dbTarget := buildFzf(dbStacks, fmt.Sprintf("Which db stack to add %s to?", dt), "")
				if dbTarget != "" {
					info := buildSetupDB(dt, svcName)
					if info != nil {
						info.Stack = dbTarget + ".yml"
					}
					if dt == "redis" {
						redisInfo = info
					} else {
						dbInfo = info
					}
				}
			}
		}
	}

	// ── 5. Manual db prompt ────────────────────────────────────────────────
	if dbInfo == nil {
		yn := buildAsk("Does this service need a database? (y/n)", "n")
		if strings.ToLower(yn) == "y" {
			dt := buildFzf([]string{"postgres", "mysql", "redis", "mongo", "none"}, "Database type?", "")
			if dt != "" && dt != "none" {
				dbInfo = buildSetupDB(dt, svcName)
			}
		}
	}

	// ── 6. Redis ───────────────────────────────────────────────────────────
	if redisInfo == nil {
		yn := buildAsk("Does this service need Redis? (y/n)", "n")
		if strings.ToLower(yn) == "y" {
			redisInfo = buildSetupDB("redis", svcName)
		}
	}

	// ── 6b. Companion container ───────────────────────────────────────────
	var companionInfo *struct {
		Name, Image, Stack, Desc string
	}
	yn := buildAsk("Does this service need a companion container? (y/n)", "n")
	if strings.ToLower(yn) == "y" {
		buildUpdate("Search for companion image...", 50)
		compImg := buildHubSearch(svcName)
		if compImg != "" {
			compName := buildAsk("Companion container name", svcName+"-worker")
			var compStacks []string
			for _, f := range buildListYML(stacksDir()) {
				if !strings.HasPrefix(f, "db_") {
					compStacks = append(compStacks, strings.TrimSuffix(f, ".yml"))
				}
			}
			sort.Strings(compStacks)
			buildUpdate("Select stack for companion...", 55)
			compStack := buildFzf(compStacks, fmt.Sprintf("Which stack for %s?", compName), "")
			if compStack != "" {
				companionInfo = &struct {
					Name, Image, Stack, Desc string
				}{Name: compName, Image: compImg, Stack: compStack, Desc: "companion service"}
			}
		}
	}

	// ── 6c. Network / volume (netvol step — mirrors the Python wizard) ──────
	buildUpdate("Network & volume...", 65)
	autoNetwork, autoVolume, externalNet := true, true, true
	nv := buildAsk("Auto-create network & volume for this container? (y/n)", "y")
	if strings.ToLower(nv) == "y" {
		nt := buildFzf([]string{
			"External (stored in creator/core file)",
			"Internal (stored in this compose file)",
		}, "Network/Volume type?", "")
		if nt != "" {
			externalNet = strings.Contains(nt, "External")
		}
		log = append(log, fmt.Sprintf("Net/Vol: auto (%s)",
			map[bool]string{true: "external", false: "internal"}[externalNet]))
	} else {
		autoNetwork, autoVolume = false, false
		log = append(log, "Net/Vol: skipped (user)")
	}
	_ = externalNet

	// ── 7. Build scaffold ──────────────────────────────────────────────────
	buildUpdate("Building compose scaffold...", 70)
	var fpath string
	if strings.HasSuffix(targetStack, ".yml") {
		fpath = filepath.Join(stacksDir(), targetStack)
	} else {
		fpath = filepath.Join(stacksDir(), targetStack+".yml")
	}
	svcCount := 0
	if data, err := os.ReadFile(fpath); err == nil {
		inServices := false
		reServices := regexp.MustCompile(`^services:`)
		reTop := regexp.MustCompile(`^[a-zA-Z]`)
		reSvcLine := regexp.MustCompile(`^  [a-zA-Z0-9][a-zA-Z0-9_.\-]+:\s*$`)
		for _, line := range strings.Split(string(data), "\n") {
			if reServices.MatchString(line) {
				inServices = true
				continue
			}
			if reTop.MatchString(line) && !strings.HasPrefix(line, " ") {
				inServices = false
				continue
			}
			if !inServices {
				continue
			}
			if reSvcLine.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "x-") {
				svcCount++
			}
		}
	}
	svcNum := svcCount + 1
	svcNet := svcName + "_net"
	svcBlock := buildSvc(svcName, image, svcIP, svcPort, cfg, svcNum, dbInfo, redisInfo)

	// ── 8. Inject ──────────────────────────────────────────────────────────
	buildUpdate(fmt.Sprintf("Injecting into %s...", targetStack), 80)
	if buildInject(targetStack, svcBlock, svcNet, "") {
		log = append(log, fmt.Sprintf("✔ Added #%d %s to %s", svcNum, svcName, targetStack))
	}

	if dbInfo != nil && dbInfo.New {
		buildUpdate(fmt.Sprintf("Adding DB to %s...", dbInfo.Stack), 85)
		dblk, dvol := buildDBBlock(dbInfo, svcName)
		if buildInject(dbInfo.Stack, dblk, svcNet, dvol) {
			log = append(log, fmt.Sprintf("✔ DB %s → %s", dbInfo.Name, dbInfo.Stack))
		}
		if autoVolume {
			exec.Command("docker", "volume", "create", dvol).Run()
		}
	}

	if redisInfo != nil && redisInfo.New {
		buildUpdate(fmt.Sprintf("Adding Redis to %s...", redisInfo.Stack), 87)
		rblk, rvol := buildDBBlock(redisInfo, svcName)
		if buildInject(redisInfo.Stack, rblk, svcNet, rvol) {
			log = append(log, fmt.Sprintf("✔ Redis → %s", redisInfo.Stack))
		}
	}

	if companionInfo != nil {
		buildUpdate(fmt.Sprintf("Adding companion %s...", companionInfo.Name), 89)
		compBlock := buildSvc(companionInfo.Name, companionInfo.Image,
			buildNextIP(), svcPort, cfg, svcNum+1, nil, nil)
		if buildInject(companionInfo.Stack, compBlock, svcNet, "") {
			log = append(log, fmt.Sprintf("✔ Companion %s → %s", companionInfo.Name, companionInfo.Stack))
		}
	}

	// ── 9. Network ─────────────────────────────────────────────────────────
	if autoNetwork {
		buildUpdate(fmt.Sprintf("Creating network %s...", svcNet), 92)
		if exec.Command("docker", "network", "inspect", svcNet).Run() != nil {
			exec.Command("docker", "network", "create", svcNet).Run()
			log = append(log, "Network: "+svcNet)
		}
	}

	buildUpdate("Build complete! ✨", 100)
	time.Sleep(300 * time.Millisecond)

	// Write log into the central logs folder (logDir = <data>/logs by default).
	lpath := logPath(fmt.Sprintf("stacks_build_%s.log", svcName))
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== Build: %s ===\n", svcName))
	for _, l := range log {
		sb.WriteString(l + "\n")
	}
	os.WriteFile(lpath, []byte(sb.String()), 0644)

	// ── 10. Ask to start ───────────────────────────────────────────────────
	// Final questions — reset terminal state first
	fmt.Fprint(os.Stdout, "\033[0m\n")
	os.Stdout.Sync()
	buildUpdate(fmt.Sprintf("✨ %s built successfully!", svcName), 100)
	yn2start := buildAsk(fmt.Sprintf("Start %s now? (y/n)", svcName), "n")
	if strings.ToLower(yn2start) == "y" {
		mode := buildAsk("(s)ervice only or (w)hole stack?", "s")
		if strings.HasPrefix(strings.ToLower(mode), "w") {
			fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s\n", targetStack)
		} else {
			fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s %s\n", targetStack, svcName)
		}
	} else {
		yn3 := buildAsk(fmt.Sprintf("Start whole stack %s? (y/n)", targetStack), "n")
		fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", svcName)
		if strings.ToLower(yn3) == "y" {
			fmt.Printf("BUILD_OK:%s\n", svcName)
			fmt.Printf("BUILD_START:%s\n", targetStack)
		} else {
			fmt.Printf("BUILD_OK:%s\n", svcName)
		}
	}
}
