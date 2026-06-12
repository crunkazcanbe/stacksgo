package lib

// Zero Scale — Bellz's own Sablier replacement, the wake-on-visit engine built
// straight into the stacks program (no more separate Node "stackwake" container).
//
//   stacks zeroscale status      report Traefik detection + per-site sleep/wake state
//   stacks zeroscale run         the engine daemon: wake HTTP server + idle sleeper
//   stacks zeroscale install     install + enable the systemd service
//   stacks zeroscale uninstall
//
// How it plugs in: Traefik's errors-middleware routes a 502/503 (service asleep)
// to this engine with the original Host header. The engine starts the site's
// container(s) via the Docker API, serves a themeable loading screen that streams
// the container's Docker logs live, polls until the service answers, then bounces
// the browser back. An idle loop reads Traefik's Prometheus metrics and stops
// containers that have seen no traffic for their idle window.
//
// Config: <configDir>/zeroscale.yaml (same schema the Settings popup writes — see
// zsConfig/zsSite in menu.go). Master on/off: ZERO_SCALE. Traefik gating:
// AUTO_DETECT_TRAEFIK + ZERO_SCALE_TRAEFIK_API.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// group cache — the env-scan over hundreds of containers is too slow to run on
// every 2.5s poll, so cache each site's group for a short window.
var (
	zsGroupCache   = map[string][]string{}
	zsGroupCacheAt = map[string]time.Time{}
	zsGroupMu      sync.Mutex
)

// ── Traefik auto-detection (Docker API) ───────────────────────────────────────

type traefikInfo struct {
	present   bool
	container string
	apiBase   string // e.g. http://traefik:8080
	metrics   string // e.g. http://traefik:8080/metrics
	useAPI    bool   // ZERO_SCALE_TRAEFIK_API and Traefik present
}

// detectTraefik finds the main Traefik router via the Docker API. It prefers a
// container literally named "traefik" and ignores the Authentik outpost
// (ak-outpost-*) which is a forward-auth proxy, not the router.
func detectTraefik() traefikInfo {
	cfg := configLoad()
	ti := traefikInfo{}
	if cfg["AUTO_DETECT_TRAEFIK"] == "0" {
		return ti // detection explicitly off
	}
	out, err := exec.Command("docker", "ps",
		"--filter", "ancestor=traefik",
		"--format", "{{.Names}}\t{{.Image}}").Output()
	names := map[string]bool{}
	if err == nil {
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ln == "" {
				continue
			}
			name, _, _ := strings.Cut(ln, "\t")
			names[strings.TrimSpace(name)] = true
		}
	}
	// catch the real Traefik image (repo basename == "traefik"), NOT traefik-adjacent
	// helpers like the crowdsec bouncer, an Authentik outpost, or a plugin.
	out2, _ := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Image}}").Output()
	for _, ln := range strings.Split(strings.TrimSpace(string(out2)), "\n") {
		name, img, _ := strings.Cut(ln, "\t")
		name = strings.TrimSpace(name)
		if isTraefikImage(img) && !isTraefikHelper(name) {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return ti
	}
	// pick "traefik" if present, else the first non-outpost match
	pick := ""
	if names["traefik"] {
		pick = "traefik"
	} else {
		for n := range names {
			pick = n
			break
		}
	}
	ti.present = true
	ti.container = pick
	ti.apiBase = "http://" + pick + ":8080"
	ti.metrics = ti.apiBase + "/metrics"
	// honour the metrics URL already configured in zeroscale.yaml if set
	if zc := loadZSConfig(); zc.TraefikMetrics != "" {
		ti.metrics = zc.TraefikMetrics
	}
	ti.useAPI = ti.present && cfg["ZERO_SCALE_TRAEFIK_API"] != "0"
	return ti
}

// isTraefikImage reports whether an image's repo basename is exactly "traefik"
// (so "traefik:v3", "library/traefik", "myreg:5000/traefik" match, but
// "crowdsecurity/traefik-bouncer" does NOT).
func isTraefikImage(img string) bool {
	base := img
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexByte(base, ':'); i >= 0 {
		base = base[:i]
	}
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	return strings.EqualFold(base, "traefik")
}

// isTraefikHelper excludes traefik-adjacent sidecars that aren't the router.
func isTraefikHelper(name string) bool {
	n := strings.ToLower(name)
	for _, bad := range []string{"outpost", "bouncer", "crowdsec", "plugin", "whoami"} {
		if strings.Contains(n, bad) {
			return true
		}
	}
	return false
}

// cmdPark stops every running container NOT in the never_sleep list — i.e. only
// your always-on infra stays up, everything else sleeps (and wakes on demand via
// Zero Scale). Replaces the old python-dependent /usr/local/bin/stacks-park.
// Dry-run by default; pass --apply (or "apply") to actually stop them.
func cmdPark(args []string) {
	apply := false
	for _, a := range args {
		if a == "--apply" || a == "apply" {
			apply = true
		}
	}
	cfg := configLoad()
	ns := map[string]bool{}
	for _, n := range strings.Fields(cfg["NEVER_SLEEP"]) {
		ns[n] = true
	}
	out, _ := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	var park []string
	for _, name := range strings.Fields(string(out)) {
		if !ns[name] {
			park = append(park, name)
		}
	}
	fmt.Printf("\x1b[1;36mPark\x1b[0m  keep %d never-sleep up, sleep %d others\n", len(ns), len(park))
	if !apply {
		fmt.Println("  (dry-run — would stop:", strings.Join(park, " "), ")")
		fmt.Println("  run 'stacks park --apply' to actually park them")
		return
	}
	for _, name := range park {
		fmt.Println("  💤", name)
		_ = exec.Command("docker", "stop", name).Run()
	}
	fmt.Printf("\x1b[1;32m✔ parked %d\x1b[0m\n", len(park))
}

// uaIgnored reports whether a User-Agent matches one of the ignore patterns
// (substring, case-insensitive) — bots/monitors/health-checks that must not wake.
func uaIgnored(ua, list string) bool {
	if ua == "" || strings.TrimSpace(list) == "" {
		return false
	}
	lua := strings.ToLower(ua)
	for _, item := range strings.Split(list, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" && strings.Contains(lua, item) {
			return true
		}
	}
	return false
}

// zsAutoStopOnStartup stops every managed (non-always-on) site so they begin
// asleep — mirrors Sablier's --provider.auto-stop-on-startup.
func zsAutoStopOnStartup() {
	c := loadZSConfig()
	n := 0
	for _, s := range c.Sites {
		if s.AlwaysOn {
			continue
		}
		for _, cn := range s.Containers {
			if containerRunning(cn) {
				_ = exec.Command("docker", "stop", cn).Run()
				n++
			}
		}
	}
	fmt.Printf("  auto-stop-on-startup: parked %d managed containers\n", n)
}

// sablierInstalled reports whether a Sablier container exists (running or not).
func sablierInstalled() bool {
	for _, f := range [][]string{
		{"ps", "-a", "--filter", "ancestor=sablierapp/sablier", "--format", "{{.Names}}"},
		{"ps", "-a", "--filter", "name=sablier", "--format", "{{.Names}}"},
	} {
		if out, err := exec.Command("docker", f...).Output(); err == nil &&
			strings.TrimSpace(string(out)) != "" {
			return true
		}
	}
	return false
}

// zeroScaleAvailable decides whether the Zero Scale options should be SHOWN.
// Rules (per the user's design):
//   - master ZERO_SCALE must be on;
//   - if Sablier is installed, hide Zero Scale (they'd both fight over wake-on-
//     visit) UNLESS ZERO_SCALE_FORCE=1 forces it on (the engine then warns);
//   - Zero Scale needs Traefik to route wakes, so when AUTO_DETECT_TRAEFIK is on
//     and Traefik isn't detected, hide it too.
func zeroScaleAvailable() bool {
	if !zeroScaleEnabled() {
		return false
	}
	cfg := configLoad()
	forced := cfg["ZERO_SCALE_FORCE"] == "1"
	// Needs Traefik to route wakes (skip the check when forced). Sablier being
	// installed no longer HIDES the per-container config — you can set Zero Scale
	// up while still on Sablier (transition); the engine + status just warn about
	// the conflict. This is what lets the Containers-tab ⚡ Zero Scale toggle show.
	if !forced && cfg["AUTO_DETECT_TRAEFIK"] != "0" && !detectTraefik().present {
		return false
	}
	return true
}

// zsHostForContainer best-effort extracts the Traefik Host(`…`) from a
// container's labels, so auto-generated sites get their URL filled in.
func zsHostForContainer(name string) string {
	out, _ := exec.Command("docker", "inspect", "-f",
		"{{range .Config.Labels}}{{println .}}{{end}}", name).Output()
	for _, v := range strings.Split(string(out), "\n") {
		if i := strings.Index(v, "Host(`"); i >= 0 {
			rest := v[i+6:]
			if j := strings.Index(rest, "`"); j >= 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

// zeroScaleGenerate auto-fills zeroscale.yaml with EVERY container, grouped by
// its compose stack (standalone containers get a single-container entry). It is
// idempotent: existing entries keep their enabled flag and per-site overrides —
// only the stack label + newly-seen containers are added. New entries default to
// DISABLED so nothing changes behaviour until you flip it on (menu or config).
func zeroScaleGenerate() {
	c := loadZSConfig()
	out, _ := exec.Command("docker", "ps", "-a", "--format",
		"{{.Names}}\t{{.Label \"com.docker.compose.project\"}}").Output()
	added, kept := 0, 0
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, stack, _ := strings.Cut(ln, "\t")
		name = strings.TrimSpace(name)
		stack = strings.TrimSpace(stack)
		if name == "" {
			continue
		}
		if _, s := c.siteForContainer(name); s != nil {
			s.Stack = stack // refresh the stack label, keep everything else
			kept++
			continue
		}
		off := false
		site := &zsSite{Stack: stack, Containers: []string{name}, Enabled: &off}
		if h := zsHostForContainer(name); h != "" {
			site.Host = []string{h}
		}
		c.Sites[name] = site
		added++
	}
	if err := saveZSConfig(c); err != nil {
		fmt.Println("✘ save:", err)
		return
	}
	fmt.Printf("\x1b[1;32m✔ zeroscale.yaml generated\x1b[0m  +%d new, %d kept  (new entries default OFF)\n", added, kept)
	fmt.Printf("  total containers tracked: %d — enable per-container in the menu or by editing the file\n", len(c.Sites))
}

// ── dispatch ──────────────────────────────────────────────────────────────────

func cmdZeroScale(args []string) {
	action := "status"
	if len(args) > 0 {
		action = strings.TrimPrefix(strings.TrimPrefix(args[0], "--"), "-")
	}
	switch action {
	case "status", "":
		zeroScaleStatus()
	case "run", "engine", "daemon":
		zeroScaleEngine()
	case "install":
		zeroScaleInstall()
	case "uninstall", "remove":
		zeroScaleUninstall()
	case "screens":
		fmt.Println("loading screens:", strings.Join(zsScreens, " "))
	case "generate", "gen", "fill", "sync":
		zeroScaleGenerate()
	default:
		fmt.Println("usage: stacks zeroscale [status|run|generate|install|uninstall|screens]")
	}
}

// containerRunning reports whether a single container is in the running state.
func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func zeroScaleStatus() {
	banner()
	on := zeroScaleEnabled()
	ti := detectTraefik()
	c := loadZSConfig()
	fmt.Printf("\x1b[1;36mZero Scale\x1b[0m  master=%s  idle=%ds  poll=%ds\n",
		boolStr(on), c.IdleSeconds, c.PollSeconds)
	if ti.present {
		fmt.Printf("  \x1b[32m✔ Traefik detected\x1b[0m: %s  (API %s, metrics %s)\n",
			ti.container, boolStr(ti.useAPI), ti.metrics)
	} else {
		fmt.Printf("  \x1b[33m• Traefik not detected\x1b[0m — Zero Scale options stay hidden / generic-middleware mode\n")
	}
	// Sablier conflict-guard
	if sablierInstalled() {
		if configLoad()["ZERO_SCALE_FORCE"] == "1" {
			fmt.Printf("  \x1b[1;31m⚠ Sablier is installed AND zero_scale_force=1\x1b[0m — both wake-on-visit engines are active; they MAY conflict.\n")
		} else {
			fmt.Printf("  \x1b[33m• Sablier also installed\x1b[0m — configure Zero Scale freely, but don't RUN both engines at once (retire Sablier before starting the Zero Scale engine)\n")
		}
	}
	fmt.Printf("  options shown in menu: %s\n", boolStr(zeroScaleAvailable()))
	if _, err := exec.LookPath("docker"); err == nil {
		if exec.Command("docker", "ps", "-a", "--filter", "name=sablier", "-q").Run() == nil {
			// informational only
		}
	}
	fmt.Printf("  sites (%d):\n", len(c.Sites))
	for k, s := range c.Sites {
		state := "\x1b[90masleep\x1b[0m"
		awake := false
		for _, cn := range s.Containers {
			if containerRunning(cn) {
				awake = true
			}
		}
		if awake {
			state = "\x1b[32mawake\x1b[0m"
		}
		enabled := s.Enabled == nil || *s.Enabled
		fmt.Printf("    %-18s %s  screen=%s  enabled=%s  hosts=%s\n",
			k, state, orDash(s.Screen), boolStr(enabled), strings.Join(s.Host, ","))
	}
}

// ── the engine daemon ─────────────────────────────────────────────────────────

func zeroScaleEngine() {
	if !zeroScaleEnabled() {
		fmt.Println("Zero Scale master switch is OFF (set zero_scale=1) — engine exiting")
		return
	}
	listen := os.Getenv("ZS_LISTEN")
	if listen == "" {
		listen = cfgStrKey(configLoad(), "ZERO_SCALE_LISTEN", ":8787")
	}
	ti := detectTraefik()
	fmt.Printf("🟢 Zero Scale engine on %s  (traefik=%v api=%v)\n", listen, ti.present, ti.useAPI)

	if cfgBoolKey(configLoad(), "ZERO_SCALE_AUTO_STOP_ON_STARTUP", false) {
		zsAutoStopOnStartup()
	}

	// idle sleeper in the background
	go zeroScaleIdleLoop(ti)

	mux := http.NewServeMux()
	mux.HandleFunc("/zs/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/zs/wake", zsWakeStatusHandler) // JSON poll: is the site up yet?
	mux.HandleFunc("/zs/logs", zsLogsHandler)       // SSE: live docker logs for the loading screen
	mux.HandleFunc("/status", zsStatusHandler)      // {"ready":…} — what the bellzloader/Sablier themes poll
	mux.HandleFunc("/zs/group", zsGroupHandler)     // per-container readiness for the default screen's checklist
	mux.HandleFunc("/", zsLandingHandler)           // the loading screen (Traefik errors route here)
	srv := &http.Server{Addr: listen, Handler: mux, ReadTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println("engine stopped:", err)
	}
}

// siteForHost returns the site whose host list matches the request Host.
func siteForHost(host string) (string, *zsSite) {
	host = strings.ToLower(strings.Split(host, ":")[0])
	c := loadZSConfig()
	for k, s := range c.Sites {
		if strings.EqualFold(k, host) {
			return k, s
		}
		for _, h := range s.Host {
			if strings.EqualFold(strings.TrimSpace(h), host) {
				return k, s
			}
		}
	}
	return "", nil
}

// zsLandingHandler is what Traefik's errors-middleware hits when a site is asleep.
// It starts the site's containers and serves the themeable loading screen.
func zsLandingHandler(w http.ResponseWriter, r *http.Request) {
	// the theme's live-log WebSocket connects to "/?session=…" — hand it off.
	if strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket") {
		zsWSHandler(w, r)
		return
	}
	cfg := configLoad()
	key, s := siteForHost(r.Host)
	if s == nil {
		// failOpen: don't hard-error an unknown host — soft retry so traffic isn't blocked.
		if cfg["ZERO_SCALE_FAIL_OPEN"] != "0" {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "<html><head><meta http-equiv=refresh content=2></head><body>starting…</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "Zero Scale: no site for host %q", r.Host)
		return
	}
	// Monitors / bots / health-checks must NOT wake a sleeping site.
	if uaIgnored(r.Header.Get("User-Agent"), cfg["ZERO_SCALE_IGNORE_USER_AGENTS"]) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "asleep")
		return
	}
	// WAIT FOR THE DATABASES: wake (and ensure running) the shared DB containers
	// FIRST, so the app doesn't come up before its database is ready. Default
	// covers the consolidated db_0 databases; override with zero_scale_depends.
	if cfgBoolKey(cfg, "ZERO_SCALE_WAKE_DEPENDS", true) {
		deps := strings.Fields(cfgStrKey(cfg, "ZERO_SCALE_DEPENDS", "pgvectordb redisdb"))
		for _, dep := range deps {
			if dep != "" && !containerRunning(dep) {
				_ = exec.Command("docker", "start", dep).Run() // blocking: DB up before the app
			}
		}
	}
	// fire the wake for the WHOLE GROUP (the site's containers + auto-detected
	// dependencies: its DB, redis, backend, etc.) so the app comes up working.
	for _, cn := range siteGroup(s) {
		if !containerRunning(cn) {
			_ = exec.Command("docker", "start", cn).Start()
		}
	}
	c := loadZSConfig()
	screen := s.Screen
	if screen == "" {
		screen = c.DefaultScreen
	}
	display := s.Display
	if display == "" {
		display = key
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, loadingScreenHTML(screen, display, key, r.Host))
}

// zsWakeStatusHandler answers the loading screen's poll: {"up": true/false}.
func zsWakeStatusHandler(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("site")
	c := loadZSConfig()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"up": %v}`, siteReady(c.Sites[key])) // whole group ready
}

// containerReady = running AND (no Docker healthcheck OR healthy). When
// ZERO_SCALE_HEALTHCHECK is on we wait for a real "healthy" before calling it up,
// so the loading screen doesn't bounce the user to a half-started app.
func containerReady(name string, requireHealth bool) bool {
	if !containerRunning(name) {
		return false
	}
	if !requireHealth {
		return true
	}
	out, _ := exec.Command("docker", "inspect", "-f",
		"{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}", name).Output()
	st := strings.TrimSpace(string(out))
	return st == "healthy" || st == "none" || st == ""
}

// zsStatusHandler is what the bellzloader/Sablier theme HTML polls
// (GET /status?host=…) — replies {"ready": true} once the site is up so the
// screen reloads the user into the real app.
func zsStatusHandler(w http.ResponseWriter, r *http.Request) {
	_, s := siteForHost(r.URL.Query().Get("host"))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ready": %v}`, siteReady(s)) // whole group must be ready
}

// zsLogsHandler streams the site container's docker logs as SSE for the screen.
func zsLogsHandler(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("site")
	c := loadZSConfig()
	s := c.Sites[key]
	cfg := configLoad()
	if s == nil || len(s.Containers) == 0 || cfg["ZERO_SCALE_SHOW_LOGS"] == "0" {
		http.NotFound(w, r)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	tail := fmt.Sprintf("%d", cfgInt(cfg, "ZERO_SCALE_LOG_LINES", 30))
	cmd := exec.Command("docker", "logs", "-f", "--tail", tail, s.Containers[0])
	pipe, err := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err != nil || cmd.Start() != nil {
		return
	}
	defer func() { _ = cmd.Process.Kill() }()
	buf := make([]byte, 4096)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		n, err := pipe.Read(buf)
		if n > 0 {
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if strings.TrimSpace(line) != "" {
					fmt.Fprintf(w, "data: %s\n\n", strings.TrimSpace(line))
				}
			}
			fl.Flush()
		}
		if err != nil {
			break
		}
	}
}

// ── idle sleeper ──────────────────────────────────────────────────────────────

// zeroScaleIdleLoop reads Traefik's Prometheus metrics every poll interval and
// stops containers for sites that have seen no new requests for their idle window.
func zeroScaleIdleLoop(ti traefikInfo) {
	lastReq := map[string]float64{}
	lastSeen := map[string]time.Time{}
	for {
		cfg := configLoad()
		c := loadZSConfig()
		poll := c.PollSeconds
		if poll < 5 {
			poll = 20
		}
		// master switch for the idle-sleeper (wake still works; nothing auto-sleeps)
		if cfg["ZERO_SCALE_AUTOSTOP"] == "0" {
			time.Sleep(time.Duration(poll) * time.Second)
			continue
		}
		idle := time.Duration(c.IdleSeconds) * time.Second
		if idle == 0 {
			idle = 30 * time.Minute
		}
		metrics := fetchTraefikMetrics(ti.metrics)
		now := time.Now()
		for key, s := range c.Sites {
			if s.AlwaysOn || (s.Enabled != nil && !*s.Enabled) {
				continue
			}
			awake := false
			for _, cn := range s.Containers {
				if containerRunning(cn) {
					awake = true
				}
			}
			if !awake {
				delete(lastSeen, key)
				continue
			}
			reqs := metrics[s.Service]
			if prev, ok := lastReq[key]; !ok || reqs != prev {
				lastReq[key] = reqs
				lastSeen[key] = now // activity (or first sight)
				continue
			}
			if seen, ok := lastSeen[key]; ok && now.Sub(seen) >= idle {
				fmt.Printf("💤 Zero Scale: %s idle %s → sleeping\n", key, idle)
				for _, cn := range s.Containers {
					_ = exec.Command("docker", "stop", cn).Run()
				}
				delete(lastSeen, key)
			}
		}
		time.Sleep(time.Duration(poll) * time.Second)
	}
}

// fetchTraefikMetrics pulls the Prometheus text endpoint and returns a map of
// service -> total request count (traefik_service_requests_total{service="…"}).
func fetchTraefikMetrics(url string) map[string]float64 {
	res := map[string]float64{}
	if url == "" {
		return res
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return res
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	for _, ln := range strings.Split(string(buf[:n]), "\n") {
		if !strings.HasPrefix(ln, "traefik_service_requests_total") {
			continue
		}
		// traefik_service_requests_total{service="x@file",...} 42
		svc := ""
		if i := strings.Index(ln, `service="`); i >= 0 {
			rest := ln[i+len(`service="`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				svc = rest[:j]
			}
		}
		fields := strings.Fields(ln)
		if svc != "" && len(fields) >= 2 {
			var v float64
			fmt.Sscanf(fields[len(fields)-1], "%g", &v)
			res[svc] += v
		}
	}
	return res
}

// ── service install ───────────────────────────────────────────────────────────

const zsUnit = "stacks-zeroscale.service"

func zeroScaleInstall() {
	self := selfBin()
	confDir := configDir()
	body := fmt.Sprintf(`[Unit]
Description=Stacks Zero Scale engine (wake-on-visit, Sablier replacement)
After=docker.service
Wants=docker.service

[Service]
Type=simple
Environment=STACKS_CONFIG_DIR=%s
ExecStart=%s zeroscale run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, confDir, self)
	if err := writeUnit(zsUnit, body); err != nil {
		fmt.Println("✘ write unit:", err, "(need root — try: sudo", self, "zeroscale install)")
		return
	}
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("daemon-reload")
	run("enable", zsUnit)
	fmt.Println("\x1b[1;32m✔ installed + enabled\x1b[0m", zsUnit)
	fmt.Println("  start it now with: sudo systemctl start", zsUnit)
}

func zeroScaleUninstall() {
	run := func(a ...string) { c := exec.Command("systemctl", a...); c.Stdout, c.Stderr = os.Stdout, os.Stderr; _ = c.Run() }
	run("disable", "--now", zsUnit)
	_ = os.Remove("/etc/systemd/system/" + zsUnit)
	run("daemon-reload")
	fmt.Println("\x1b[1;32m✔ removed\x1b[0m", zsUnit)
}

// ── themeable loading screens ─────────────────────────────────────────────────

// loadingScreenHTML renders a self-contained, themed wake screen. It polls
// /zs/wake until the site is up (then reloads so Traefik routes to the now-awake
// service) and live-streams the container's Docker logs via /zs/logs (SSE).
// ── container groups (auto-wake the whole dependency group) ───────────────────

func dockerAllNames() []string {
	out, _ := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}").Output()
	return strings.Fields(string(out))
}
func dockerEnvValues(name string) []string {
	out, _ := exec.Command("docker", "inspect", "-f",
		"{{range .Config.Env}}{{println .}}{{end}}", name).Output()
	return strings.Split(string(out), "\n")
}

// zsAutoGroup expands a site's seed containers into its full GROUP, in real time
// via the Docker API: the seed containers + the shared ZERO_SCALE_DEPENDS + any
// OTHER container they reference in their env (its database, redis, backend, etc.
// — e.g. DATABASE_URL=…@pgvectordb pulls in pgvectordb). Waking one wakes them all.
func zsAutoGroup(seed []string) []string {
	cfg := configLoad()
	want := map[string]bool{}
	for _, x := range seed {
		if x != "" {
			want[x] = true
		}
	}
	if cfgBoolKey(cfg, "ZERO_SCALE_WAKE_DEPENDS", true) {
		for _, d := range strings.Fields(cfg["ZERO_SCALE_DEPENDS"]) {
			if d != "" {
				want[d] = true
			}
		}
		all := dockerAllNames()
		for _, x := range seed {
			for _, v := range dockerEnvValues(x) {
				lv := strings.ToLower(v)
				for _, other := range all {
					if other != x && len(other) >= 4 && strings.Contains(lv, strings.ToLower(other)) {
						want[other] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(want))
	for n := range want {
		out = append(out, n)
	}
	return out
}

func siteGroup(s *zsSite) []string {
	if s == nil {
		return nil
	}
	key := strings.Join(s.Containers, ",")
	zsGroupMu.Lock()
	defer zsGroupMu.Unlock()
	if g, ok := zsGroupCache[key]; ok && time.Since(zsGroupCacheAt[key]) < 60*time.Second {
		return g
	}
	g := zsAutoGroup(s.Containers)
	zsGroupCache[key] = g
	zsGroupCacheAt[key] = time.Now()
	return g
}

// zsGroupHandler returns each group container + whether it's ready, so the
// loading screen can show a live checklist of what's coming up.
func zsGroupHandler(w http.ResponseWriter, r *http.Request) {
	c := loadZSConfig()
	s := c.Sites[r.URL.Query().Get("site")]
	requireHealth := configLoad()["ZERO_SCALE_HEALTHCHECK"] != "0"
	w.Header().Set("Content-Type", "application/json")
	parts := []string{}
	for _, cn := range siteGroup(s) {
		parts = append(parts, fmt.Sprintf(`{"name":%q,"ready":%v}`, cn, containerReady(cn, requireHealth)))
	}
	fmt.Fprintf(w, `{"containers":[%s]}`, strings.Join(parts, ","))
}

func siteReady(s *zsSite) bool {
	requireHealth := configLoad()["ZERO_SCALE_HEALTHCHECK"] != "0"
	g := siteGroup(s)
	if len(g) == 0 {
		return false
	}
	for _, cn := range g {
		if !containerReady(cn, requireHealth) {
			return false
		}
	}
	return true
}

// ── minimal WebSocket (no external deps) for live group logs ──────────────────

func wsAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsWriteText writes one unmasked server text frame.
func wsWriteText(conn net.Conn, msg string) error {
	p := []byte(msg)
	n := len(p)
	hdr := []byte{0x81}
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n < 65536:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if _, err := conn.Write(hdr); err != nil {
		return err
	}
	_, err := conn.Write(p)
	return err
}

func wsClassify(ln string) string {
	l := strings.ToLower(ln)
	switch {
	case strings.Contains(l, "error"), strings.Contains(l, "fatal"), strings.Contains(l, "panic"):
		return "error"
	case strings.Contains(l, "warn"):
		return "warn"
	case strings.Contains(l, "listening"), strings.Contains(l, "started"), strings.Contains(l, "ready"), strings.Contains(l, "running on"):
		return "ok"
	}
	return ""
}

func streamContainerLogs(name string, out chan<- string, stop <-chan struct{}) {
	cmd := exec.Command("docker", "logs", "-f", "--tail", "15", name)
	pipe, err := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err != nil || cmd.Start() != nil {
		return
	}
	defer func() { _ = cmd.Process.Kill() }()
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		select {
		case out <- name + " | " + sc.Text():
		case <-stop:
			return
		}
	}
}

// zsWSHandler upgrades to WebSocket and streams the live Docker logs of every
// container in the site's group (what the bellzloader theme's log box shows),
// emitting a {type:"ready"} once the whole group is up so the page drops in.
func zsWSHandler(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if key == "" || !ok {
		http.Error(w, "websocket only", http.StatusBadRequest)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"))

	c := loadZSConfig()
	s := c.Sites[r.URL.Query().Get("session")]
	if s == nil {
		return
	}
	group := siteGroup(s)
	wsWriteText(conn, fmt.Sprintf(`{"text":"⛏ waking group: %s","type":"status"}`,
		strings.Join(group, ", ")))
	lines := make(chan string, 200)
	stop := make(chan struct{})
	defer close(stop)
	for _, cn := range group {
		go streamContainerLogs(cn, lines, stop)
	}
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case ln := <-lines:
			if wsWriteText(conn, fmt.Sprintf(`{"text":%q,"type":%q}`, ln, wsClassify(ln))) != nil {
				return
			}
		case <-tick.C:
			if siteReady(s) {
				wsWriteText(conn, `{"text":"READY — entering…","type":"ready"}`)
				return
			}
		}
	}
}

func loadingScreenHTML(screen, display, siteKey, host string) string {
	cfg := configLoad()
	// First choice: serve the user's REAL theme .html files (identical to the old
	// Node/bellzloader loader) — a custom file, or <themes_dir>/<screen>.html.
	if cfg["ZERO_SCALE_USE_THEMES"] != "0" {
		path := strings.TrimSpace(cfg["ZERO_SCALE_CUSTOM_HTML"])
		if path == "" {
			dir := cfgStrKey(cfg, "ZERO_SCALE_THEMES_DIR",
				"/home/bellzserver/MyDocker/Configs/stackwake/screens")
			path = dir + "/" + screen + ".html"
		}
		if data, err := os.ReadFile(path); err == nil {
			html := string(data)
			repl := map[string]string{
				"{{DISPLAY}}":   display,
				"{{HOST}}":      host,
				"{{SESSION}}":   siteKey,
				"{{WAKE_BASE}}": cfgStrKey(cfg, "ZERO_SCALE_WAKE_BASE", ""),
				"{{TITLE}}":     cfgStrKey(cfg, "ZERO_SCALE_TITLE", "Waking"),
				"{{TIP}}":       cfg["ZERO_SCALE_TIP"],
			}
			for k, v := range repl {
				html = strings.ReplaceAll(html, k, v)
			}
			return html
		}
	}
	// Built-in DEFAULT screen — self-contained, ships with the program, used for
	// everyone when no custom theme file is applied. Shows the whole group as a
	// live checklist (each container flips to ✓ as it comes up), an animated bar,
	// and the live Docker logs. Uses only the engine's own endpoints.
	accent := "#7c9cff"
	switch screen {
	case "minecraft":
		accent = "#5fb83c"
	case "terminal":
		accent = "#3ad14b"
	case "synthwave":
		accent = "#ff4dd2"
	}
	return fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Waking %[1]s…</title>
<style>
 :root{--a:%[3]s}
 *{box-sizing:border-box}
 html,body{height:100%%;margin:0;background:radial-gradient(1200px 600px at 50%% -10%%,#1b2030,#0c0e16);
   color:#e7ebf5;font-family:ui-monospace,'JetBrains Mono',Menlo,monospace}
 .wrap{min-height:100%%;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:18px;padding:24px}
 h1{font-size:1.9rem;margin:0;font-weight:700}
 h1 .a{color:var(--a)}
 .sub{opacity:.7;font-size:.85rem;margin:-6px 0 2px}
 .bar{width:min(560px,86vw);height:14px;border-radius:8px;background:#ffffff14;overflow:hidden;position:relative}
 .bar i{position:absolute;top:0;bottom:0;width:40%%;border-radius:8px;background:var(--a);
   filter:drop-shadow(0 0 8px var(--a));animation:sl 1.3s cubic-bezier(.4,0,.2,1) infinite}
 @keyframes sl{0%%{left:-40%%}100%%{left:100%%}}
 .grid{width:min(560px,86vw);display:flex;flex-wrap:wrap;gap:8px;justify-content:center}
 .chip{display:flex;align-items:center;gap:7px;background:#ffffff0d;border:1px solid #ffffff14;
   border-radius:999px;padding:5px 12px;font-size:.8rem}
 .chip .d{width:9px;height:9px;border-radius:50%%;background:#ffd166;box-shadow:0 0 6px #ffd16688;animation:pz 1s infinite}
 .chip.ok .d{background:var(--a);box-shadow:0 0 8px var(--a);animation:none}
 .chip.ok{color:var(--a);border-color:var(--a)55}
 @keyframes pz{50%%{opacity:.35}}
 pre{width:min(640px,90vw);height:30vh;overflow:auto;margin:0;text-align:left;background:#00000055;
   border:1px solid #ffffff14;border-radius:10px;padding:12px;font-size:.76rem;line-height:1.5;color:#aeb6c6}
 pre .ok{color:var(--a)}pre .error{color:#ff7a7a}pre .warn{color:#ffd166}pre .status{color:#7fd3ff}
 .tip{opacity:.55;font-size:.75rem}
</style></head><body><div class="wrap">
 <h1>Waking <span class="a">%[1]s</span>…</h1>
 <div class="sub">starting the container group — this only takes a moment</div>
 <div class="bar"><i></i></div>
 <div class="grid" id="grid"></div>
 <pre id="log">connecting to logs…
</pre>
 <div class="tip">You'll be dropped in automatically once everything is ready.</div>
</div>
<script>
 const site=%[2]q, grid=document.getElementById('grid'), log=document.getElementById('log');
 try{const es=new EventSource('/zs/logs?site='+encodeURIComponent(site));
   es.onmessage=e=>{const d=document.createElement('span');
     const t=e.data.toLowerCase();
     d.className=/error|fatal/.test(t)?'error':/warn/.test(t)?'warn':/listening|started|ready|running on/.test(t)?'ok':'';
     d.textContent=e.data+'\n';log.appendChild(d);log.scrollTop=log.scrollHeight;
     while(log.children.length>400)log.removeChild(log.firstChild);};}catch(e){}
 async function group(){
   try{const r=await fetch('/zs/group?site='+encodeURIComponent(site),{cache:'no-store'});
     const j=await r.json();grid.innerHTML='';
     (j.containers||[]).forEach(c=>{const el=document.createElement('div');
       el.className='chip'+(c.ready?' ok':'');
       el.innerHTML='<span class="d"></span>'+c.name+(c.ready?' ✓':'');grid.appendChild(el);});
   }catch(e){}
 }
 async function poll(){
   try{const r=await fetch('/zs/wake?site='+encodeURIComponent(site),{cache:'no-store'});
     if((await r.json()).up){location.reload();return;}}catch(e){}
   setTimeout(poll,2000);
 }
 group();setInterval(group,2000);setTimeout(poll,2500);
</script></body></html>`, display, siteKey, accent)
}
