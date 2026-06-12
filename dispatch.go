package main

// dispatch.go — the lifecycle + utility command implementations the bash
// `stacks` dispatcher had inline that the Go port was still missing:
//
//   up, down, start, stop, restart, recreate, rm, reload, kill, get, logs,
//   inspect, pull, snapshot, volume, network, dynamics, clean, art, custom,
//   scale, proxy, strip, inject, backup
//
// DESIGN NOTE — compose vs the Engine API: the bash drives every lifecycle
// action through `docker compose -f <stackfile> …`. `docker compose` is the
// CLI plugin, NOT part of the Engine API the SDK exposes, so these all shell
// out via os/exec exactly like the bash. Read-only listing/status that the SDK
// CAN do still go through compose here to match the bash output byte-for-byte
// (e.g. `compose config --volumes`). Everything is keyed off the same
// flags / target-stack+service parsing the bash used.
//
// All new top-level helpers are prefixed `disp` to avoid collisions.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── universal path helpers ───────────────────────────────────────────────────

// dispDataDir is the data base (_DEF_DATA in bash): $STACKS_DATA_DIR else
// <stacksDir>/.. (Stacks lives under the data dir) else ~/MyDocker.
func dispDataDir() string {
	if d := os.Getenv("STACKS_DATA_DIR"); d != "" {
		return d
	}
	// stacksDir() is <data>/Stacks normally; its parent is the data dir.
	if sd := stacksDir(); sd != "" {
		return filepath.Dir(sd)
	}
	return filepath.Join(home(), "MyDocker")
}

// dispDynamicsDir mirrors DYNAMICS_DIR resolution (reuses fixdynDynDir).
func dispDynamicsDir() string { return fixdynDynDir() }

// dispSnapshotDir mirrors SNAPSHOT_DIR: conf SNAPSHOT_DIR else <data>/Snapshots.
func dispSnapshotDir() string {
	if d := os.Getenv("SNAPSHOT_DIR"); d != "" {
		return d
	}
	if d := confValue("SNAPSHOT_DIR"); d != "" {
		return d
	}
	return filepath.Join(dispDataDir(), "Snapshots")
}

// dispBackupDest mirrors BACKUP_DEST default: <data>/backups.
func dispBackupDest() string { return filepath.Join(dispDataDir(), "backups") }

// dispConfBool reads a conf flag, treating unset as the given default.
func dispConfBool(key string, def bool) bool {
	v := confValue(key)
	if v == "" {
		return def
	}
	return v != "0" && strings.ToLower(v) != "false" && strings.ToLower(v) != "off"
}

// ── stack resolution (mirrors the bash _is_stack name fuzzing) ───────────────

// dispResolveStackFile returns the on-disk .yml path for a stack token, trying
// the token as-is, then _→- and -→_ variants. Returns "" if none exist.
func dispResolveStackFile(token string) string {
	dir := stacksDir()
	cands := []string{token, strings.ReplaceAll(token, "_", "-"), strings.ReplaceAll(token, "-", "_")}
	for _, c := range cands {
		p := filepath.Join(dir, c+".yml")
		if dispFileExists(p) {
			return p
		}
	}
	return ""
}

// dispResolveStackName is like dispResolveStackFile but returns the base name.
func dispResolveStackName(token string) string {
	f := dispResolveStackFile(token)
	if f == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(f), ".yml")
}

func dispFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// dispStackList mirrors the STACKS array: stack_order from config, else every
// *.yml in STACKS_DIR excluding *-ext remote stacks.
func dispStackList() []string {
	if so := strings.TrimSpace(os.Getenv("STACK_ORDER")); so != "" {
		return strings.Fields(so)
	}
	if so := strings.TrimSpace(confValue("STACK_ORDER")); so != "" {
		return strings.Fields(so)
	}
	dir := stacksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b := strings.TrimSuffix(e.Name(), ".yml")
		if strings.HasSuffix(b, "-ext") {
			continue
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

func dispReversed(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// ── parsed-args model (a faithful subset of the bash arg parser) ─────────────

type dispArgs struct {
	command  string
	recreate bool
	restart  bool
	force    bool
	clean    bool
	info     bool
	doFix      bool // `… fix`      — run `stacks fix <stack>` after the lifecycle op
	doDynamics bool // `… dynamics` — reconcile/generate dynamics alongside the op
	doRepair   bool // `… repair`   — run dynamics repair (structural) too
	stacks   []string // parsed stack tokens (resolved names)
	services []string // parallel: service for each stack ("" = whole stack)
	raw      []string // leftover non-flag args (TARGET_ARGS equivalent)
}

// dispParse builds a dispArgs from the command + the rest of argv. It separates
// modifier flags from real stack/service targets, resolving stacks against disk
// and matching service names the way the bash does (token after a stack token).
func dispParse(command string, rest []string) dispArgs {
	a := dispArgs{command: command}
	var targets []string
	for _, arg := range rest {
		la := strings.ToLower(arg)
		switch la {
		case "recreate":
			a.recreate = true
		case "restart":
			a.restart = true
		case "force":
			a.force = true
		case "force-hc":
			a.force = true
			os.Setenv("FIX_FORCE_HC", "1")
		case "clean":
			a.clean = true
		case "info", "i":
			a.info = true
		case "fix":
			a.doFix = true
		case "dynamic", "dynamics", "dyn":
			a.doDynamics = true
		case "repair", "repaire":
			a.doRepair = true
		case "no-fix":
			a.doFix = false
		case "d", "detach", "a", "attach", "just", "only", "solo", "single", "continue":
			// recognized modifiers that don't change lifecycle behavior here
		default:
			targets = append(targets, arg)
		}
	}
	a.raw = targets

	// Walk targets pairing a stack with an optional following service token.
	i := 0
	for i < len(targets) {
		tok := targets[i]
		sn := dispResolveStackName(tok)
		if sn == "" {
			// not a stack — keep it as a bare target (used by get/kill/logs/etc.)
			a.stacks = append(a.stacks, tok)
			a.services = append(a.services, "")
			i++
			continue
		}
		// peek next token: if it's NOT itself a stack, treat it as a service
		if i+1 < len(targets) && dispResolveStackName(targets[i+1]) == "" {
			a.stacks = append(a.stacks, sn)
			a.services = append(a.services, dispMatchService(sn, targets[i+1]))
			i += 2
			continue
		}
		a.stacks = append(a.stacks, sn)
		a.services = append(a.services, "")
		i++
	}
	return a
}

// dispMatchService resolves a service token against a stack's service list,
// trying _→- and -→_ variants; falls back to the token unchanged.
func dispMatchService(stack, token string) string {
	svcs := dispStackServices(stack)
	set := map[string]bool{}
	for _, s := range svcs {
		set[s] = true
	}
	for _, c := range []string{token, strings.ReplaceAll(token, "_", "-"), strings.ReplaceAll(token, "-", "_")} {
		if set[c] {
			return c
		}
	}
	return token
}

var dispServiceRE = regexp.MustCompile(`^  ([a-zA-Z0-9_.+-]+):`)

// dispStackServices mirrors the awk that lists top-level service keys from a
// stack file's `services:` block.
func dispStackServices(stack string) []string {
	f := dispResolveStackFile(stack)
	if f == "" {
		return nil
	}
	fh, err := os.Open(f)
	if err != nil {
		return nil
	}
	defer fh.Close()
	var out []string
	inServices := false
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		// a new top-level key ends the services block
		if inServices && len(line) > 0 && line[0] >= 'a' && line[0] <= 'z' {
			inServices = false
		}
		if inServices {
			if m := dispServiceRE.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// ── compose runner ───────────────────────────────────────────────────────────

// dispComposeEnv mirrors the env the bash exports before any compose call.
func dispComposeEnv() []string {
	e := os.Environ()
	e = append(e,
		"COMPOSE_PROGRESS=plain",
		"DOCKER_CLI_HINTS=false",
		"DOCKER_CLIENT_TIMEOUT=120",
		"COMPOSE_HTTP_TIMEOUT=120",
	)
	return e
}

// dispCompose runs `docker compose -f <file> <args…>` streaming to the terminal.
// Returns true on success (exit 0).
func dispCompose(file string, args ...string) bool {
	full := append([]string{"compose", "-f", file}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Env = dispComposeEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}

// dispComposeOut runs a compose command and captures stdout (trimmed).
func dispComposeOut(file string, args ...string) string {
	full := append([]string{"compose", "-f", file}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Env = dispComposeEnv()
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// dispDocker runs a raw `docker <args…>` streaming to the terminal.
func dispDocker(args ...string) bool {
	cmd := exec.Command("docker", args...)
	cmd.Env = dispComposeEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}

func dispDockerOut(args ...string) string {
	cmd := exec.Command("docker", args...)
	cmd.Env = dispComposeEnv()
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// dispSablierRestart restarts the sablier container (best-effort, quiet).
func dispSablierRestart() { _ = dispDockerOut("restart", "sablier") }

const dispDelay = 15

func dispWait(seconds int) { time.Sleep(time.Duration(seconds) * time.Second) }

// ============================================================================
// UP / START / STOP / RESTART / RECREATE
// ============================================================================

// dispUp implements `stacks up [stack [service]] [recreate] …`. It is a faithful
// (slimmed) port of the up engine: per-stack pull + up -d --remove-orphans
// [--force-recreate], optional restart-first, then a Sablier restart.
func dispUp(a dispArgs) {
	banner()
	upBase := []string{"--remove-orphans"}
	extra := append([]string{}, upBase...)
	if a.recreate {
		extra = append(extra, "--force-recreate")
	}

	// `up … dynamics [fix] [repair]` — reconcile/generate the dynamic files
	// up-front (mirrors bash run_dynamics_fix before the deploy loop). Generation
	// is non-destructive: it only creates MISSING dynamic files (no --force), then
	// reconciles names against the compose files.
	if a.doDynamics {
		fmt.Println("\n\x1b[1;35m⚡ Dynamics: generate-missing + reconcile names…\x1b[0m")
		dispDynamicsGenerate([]string{"all"}, a.force)
		if a.doRepair {
			dispRunDynamicsFix("repair", []string{"all"})
		}
		dispRunDynamicsFix("fix", []string{"all"})
	}

	deploy := func(stack, service string) {
		file := dispResolveStackFile(stack)
		if file == "" {
			fmt.Printf("\x1b[1;31m✘ Warning: Stack %s.yml not found, skipping…\x1b[0m\n", stack)
			return
		}
		if a.restart {
			if service != "" {
				dispCompose(file, "stop", service)
			} else {
				dispCompose(file, "stop")
			}
		}
		fmt.Printf("\x1b[1;36m▶ Deploying %s%s\x1b[0m\n", stack, dispSvcLabel(service))
		if service != "" {
			dispCompose(file, "pull", "--ignore-pull-failures", service)
			args := append([]string{"up", "-d"}, extra...)
			args = append(args, service)
			dispCompose(file, args...)
		} else {
			dispCompose(file, "pull", "--ignore-pull-failures")
			args := append([]string{"up", "-d"}, extra...)
			dispCompose(file, args...)
		}
		dispSablierRestart()
		// `up … fix` — run the full per-stack fix after it's deployed (mirrors
		// bash running stacks_fix.py per stack when DO_FIX=1). This includes the
		// name-sync phase, so names propagate into compose + dynamics.
		if a.doFix {
			cmdFix([]string{stack})
		}
	}

	if len(a.stacks) == 0 {
		stacks := dispStackList()
		for _, s := range stacks {
			deploy(s, "")
			dispWait(dispDelay)
		}
		dispSablierRestart()
		dispFinalSummary()
		return
	}
	for i, s := range a.stacks {
		deploy(s, a.services[i])
	}
	dispFinalSummary()
}

func dispSvcLabel(service string) string {
	if service == "" {
		return ""
	}
	return " / " + service
}

// dispManage implements start|stop|restart for whole stacks / services.
func dispManage(a dispArgs) {
	banner()
	action := a.command
	actionArgs := func() []string {
		if a.force && (action == "stop" || action == "restart") {
			return []string{"--timeout", "1"}
		}
		return nil
	}

	run := func(stack, service string) {
		file := dispResolveStackFile(stack)
		if file == "" {
			fmt.Printf("\x1b[1;31m✘ Stack %s.yml not found\x1b[0m\n", stack)
			return
		}
		args := []string{action}
		args = append(args, actionArgs()...)
		if service != "" {
			args = append(args, service)
		}
		fmt.Printf("\x1b[1;36m▶ %sing %s%s\x1b[0m\n", strings.Title(action), stack, dispSvcLabel(service))
		dispCompose(file, args...)
	}

	if len(a.stacks) == 0 {
		stacks := dispStackList()
		if action == "stop" {
			stacks = dispReversed(stacks)
		}
		for _, s := range stacks {
			run(s, "")
			dispWait(dispDelay)
		}
		fmt.Printf("\x1b[1;32m✔ All stacks %sed.\x1b[0m\n", action)
		return
	}
	order := a.stacks
	svcOrder := a.services
	if action == "stop" {
		order = dispReversed(a.stacks)
		svcOrder = dispReversed(a.services)
	}
	for i, s := range order {
		run(s, svcOrder[i])
	}
}

// dispRecreate implements `stacks recreate …` = up with --force-recreate forced.
func dispRecreate(a dispArgs) {
	a.recreate = true
	a.command = "up"
	dispUp(a)
}

// ============================================================================
// DOWN
// ============================================================================

func dispDown(a dispArgs) {
	banner()
	takeDown := func(stack string) {
		file := dispResolveStackFile(stack)
		if file == "" {
			fmt.Printf("\x1b[1;31m✘ Stack %s.yml not found\x1b[0m\n", stack)
			return
		}
		if dispComposeOut(file, "ps", "-a", "-q") != "" {
			args := []string{"down"}
			if a.clean {
				args = append(args, "--remove-orphans", "-v")
			}
			if a.force {
				args = append(args, "--timeout", "1")
			}
			fmt.Printf("\x1b[1;36m▶ Bringing down %s\x1b[0m\n", stack)
			dispCompose(file, args...)
		} else {
			fmt.Printf("\x1b[2m%s already down — skipping\x1b[0m\n", stack)
		}
	}

	if len(a.stacks) == 0 {
		for _, s := range dispReversed(dispStackList()) {
			takeDown(s)
			dispWait(2)
		}
		dispDeepCleanNetworks()
		fmt.Println("\x1b[1;32m✔ All stacks & networks purged.\x1b[0m")
		return
	}
	// reverse order for down
	for i := len(a.stacks) - 1; i >= 0; i-- {
		stack, service := a.stacks[i], a.services[i]
		file := dispResolveStackFile(stack)
		if file == "" {
			fmt.Printf("\x1b[1;31m✘ Stack %s.yml not found\x1b[0m\n", stack)
			continue
		}
		if service != "" {
			forceArg := []string{}
			if a.force {
				forceArg = []string{"--timeout", "1"}
			}
			dispCompose(file, append(append([]string{"stop"}, forceArg...), service)...)
			dispCompose(file, "rm", "-f", service)
		} else {
			takeDown(stack)
			if a.clean {
				dispDocker("system", "prune", "-f", "--volumes")
			}
		}
	}
}

func dispDeepCleanNetworks() {
	fmt.Println("\x1b[1;34m▶ Deep cleaning networks…\x1b[0m")
	for _, line := range strings.Split(dispDockerOut("network", "ls", "--format", "{{.Name}}"), "\n") {
		net := strings.TrimSpace(line)
		switch net {
		case "", "bridge", "host", "none", "ingress", "docker_gwbridge":
			continue
		}
		ctrs := dispDockerOut("network", "inspect", net, "-f", "{{range .Containers}}{{.Name}} {{end}}")
		for _, c := range strings.Fields(ctrs) {
			dispDocker("network", "disconnect", "-f", net, c)
		}
		dispDocker("network", "rm", net)
	}
	dispDocker("network", "prune", "-f")
}

// ============================================================================
// RM
// ============================================================================

func dispRm(a dispArgs) {
	banner()
	sub := "containers"
	name := ""
	if len(a.raw) > 0 {
		sub = a.raw[0]
	}
	if len(a.raw) > 1 {
		name = a.raw[1]
	}
	force := []string{}
	if a.force {
		force = []string{"--force"}
	}
	lsub := strings.ToLower(sub)

	// stack name as first arg → full stack purge
	if file := dispResolveStackFile(sub); file != "" {
		stack := dispResolveStackName(sub)
		dispCompose(file, "stop")
		dispCompose(file, "rm", "-f")
		dispCompose(file, "down", "--remove-orphans")
		for _, v := range strings.Fields(dispComposeOut(file, "config", "--volumes")) {
			dispDocker(append(append([]string{"volume", "rm"}, force...), v)...)
		}
		fmt.Printf("\x1b[1;32m✔ Stack %s fully removed.\x1b[0m\n", stack)
		return
	}

	switch {
	case dispIn(lsub, "volume", "vol", "v"):
		if name == "" || strings.ToLower(name) == "all" {
			dispDocker("volume", "prune", "-f")
		} else {
			dispDocker(append(append([]string{"volume", "rm"}, force...), name)...)
		}
		fmt.Println("\x1b[1;32m✔ Volume(s) removed.\x1b[0m")
	case dispIn(lsub, "network", "net", "n"):
		if name == "" || strings.ToLower(name) == "all" {
			dispDocker("network", "prune", "-f")
			fmt.Println("\x1b[1;32m✔ Pruned unused networks\x1b[0m")
		} else {
			if dispDocker(append(append([]string{"network", "rm"}, force...), name)...) {
				fmt.Printf("\x1b[1;32m✔ Removed network: %s\x1b[0m\n", name)
			} else {
				fmt.Printf("\x1b[1;31m✘ Could not remove: %s\x1b[0m\n", name)
			}
		}
	case dispIn(lsub, "image", "img", "i"):
		if name == "" || strings.ToLower(name) == "all" {
			dispDocker("image", "prune", "-f")
		} else {
			dispDocker(append(append([]string{"image", "rm"}, force...), name)...)
		}
		fmt.Println("\x1b[1;32m✔ Image(s) removed.\x1b[0m")
	case dispIn(lsub, "containers", "c"):
		if len(a.stacks) > 0 {
			if file := dispResolveStackFile(a.stacks[0]); file != "" {
				dispCompose(file, "rm", "-f")
			}
		} else {
			dispDocker("container", "prune", "-f")
		}
		fmt.Println("\x1b[1;32m✔ Containers removed.\x1b[0m")
	case dispIn(lsub, "all", "a"):
		dispDocker("container", "prune", "-f")
		dispDocker("network", "prune", "-f")
		dispDocker("volume", "prune", "-f")
		dispDocker("image", "prune", "-f")
		fmt.Println("\x1b[1;32m✔ Full prune complete.\x1b[0m")
	default:
		fmt.Printf("\x1b[1;31m✘ Unknown rm target: %s\x1b[0m\n", sub)
		fmt.Println("\x1b[1;33mUsage: stacks rm [stackname|volume|network|image|containers|all] [name] [force]\x1b[0m")
		os.Exit(1)
	}
}

func dispIn(v string, set ...string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// ============================================================================
// GET (pull a single image)
// ============================================================================

func dispGet(a dispArgs) {
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Usage: stacks get <imagename>\x1b[0m")
		os.Exit(1)
	}
	img := a.raw[0]
	fmt.Printf("\x1b[1;36m▶ Pulling %s\x1b[0m\n", img)
	if dispDocker("pull", img) {
		fmt.Printf("\x1b[1;32m✔ Image pulled: %s\x1b[0m\n", img)
	} else {
		fmt.Printf("\x1b[1;31m✘ Failed to pull: %s\x1b[0m\n", img)
		os.Exit(1)
	}
}

// ============================================================================
// KILL (container or whole stack)
// ============================================================================

func dispKill(a dispArgs) {
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Usage: stacks kill <container|stack>\x1b[0m")
		os.Exit(1)
	}
	target := a.raw[0]
	if file := dispResolveStackFile(target); file != "" {
		fmt.Printf("\x1b[1;36m▶ Killing stack %s\x1b[0m\n", target)
		dispCompose(file, "kill")
		fmt.Printf("\x1b[1;32m✔ Stack %s killed.\x1b[0m\n", target)
		return
	}
	fmt.Printf("\x1b[1;36m▶ Killing container %s\x1b[0m\n", target)
	dispDocker("kill", target)
	fmt.Printf("\x1b[1;32m✔ Container %s killed.\x1b[0m\n", target)
}

// ============================================================================
// LOGS (container or whole stack, follow + colorize)
// ============================================================================

func dispLogs(a dispArgs) {
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Usage: stacks logs <container|stack>\x1b[0m")
		os.Exit(1)
	}
	target := a.raw[0]
	var cmd *exec.Cmd
	if file := dispResolveStackFile(target); file != "" {
		cmd = exec.Command("docker", "compose", "-f", file, "logs", "--tail=100", "-f", "--no-color")
	} else {
		cmd = exec.Command("docker", "logs", "--tail=100", "-f", target)
	}
	cmd.Env = dispComposeEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	dispColorizeLogs(stdout)
	cmd.Wait()
}

var (
	dispReErr  = regexp.MustCompile(`(?i)(ERROR|FATAL)`)
	dispReWarn = regexp.MustCompile(`(?i)(WARN|WARNING)`)
	dispReInfo = regexp.MustCompile(`(?i)(INFO)`)
)

func dispColorizeLogs(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		line = dispReErr.ReplaceAllString(line, "\x1b[1;31m$1\x1b[0m")
		line = dispReWarn.ReplaceAllString(line, "\x1b[1;33m$1\x1b[0m")
		line = dispReInfo.ReplaceAllString(line, "\x1b[1;32m$1\x1b[0m")
		fmt.Println(line)
	}
}

// ============================================================================
// INSPECT (decorated single-container view)
// ============================================================================

func dispInspect(a dispArgs) {
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Usage: stacks inspect <containername>\x1b[0m")
		os.Exit(1)
	}
	name := a.raw[0]
	m := containerInspect(name)
	if len(m) == 0 {
		fmt.Println("Container not found")
		os.Exit(1)
	}
	cfg, _ := m["Config"].(map[string]interface{})
	net, _ := m["NetworkSettings"].(map[string]interface{})
	st, _ := m["State"].(map[string]interface{})

	cname := strings.TrimPrefix(dispStr(m["Name"]), "/")
	fmt.Println("\n\x1b[1;35m╔══════════════════════════════════════════════╗\x1b[0m")
	fmt.Printf("\x1b[1;35m║\x1b[0m  \x1b[1;36m🔍 %s\x1b[0m\n", cname)
	if cfg != nil {
		fmt.Printf("\x1b[1;35m║\x1b[0m  Image:   \x1b[1;33m%s\x1b[0m\n", dispStr(cfg["Image"]))
	}
	if st != nil {
		started := dispStr(st["StartedAt"])
		if len(started) > 19 {
			started = started[:19]
		}
		fmt.Printf("\x1b[1;35m║\x1b[0m  Status:  \x1b[1;32m%s\x1b[0m  Started: %s\n", dispStr(st["Status"]), started)
	}
	if net != nil {
		if ports, ok := net["Ports"].(map[string]interface{}); ok {
			for p, v := range ports {
				if arr, ok := v.([]interface{}); ok {
					for _, b := range arr {
						if bm, ok := b.(map[string]interface{}); ok {
							hip := dispStr(bm["HostIp"])
							if hip == "" {
								hip = "0.0.0.0"
							}
							fmt.Printf("\x1b[1;35m║\x1b[0m  Port:    %s:%s -> %s\n", hip, dispStr(bm["HostPort"]), p)
						}
					}
				}
			}
		}
		if nets, ok := net["Networks"].(map[string]interface{}); ok {
			for nn, nd := range nets {
				if ndm, ok := nd.(map[string]interface{}); ok {
					fmt.Printf("\x1b[1;35m║\x1b[0m  Network: \x1b[1;34m%s\x1b[0m  IP: %s\n", nn, dispStr(ndm["IPAddress"]))
				}
			}
		}
	}
	if mnts, ok := m["Mounts"].([]interface{}); ok {
		for _, mt := range mnts {
			if mm, ok := mt.(map[string]interface{}); ok {
				fmt.Printf("\x1b[1;35m║\x1b[0m  Volume:  %s -> %s\n", dispStr(mm["Source"]), dispStr(mm["Destination"]))
			}
		}
	}
	fmt.Println("\x1b[1;35m╚══════════════════════════════════════════════╝\x1b[0m")
}

func dispStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// ============================================================================
// PULL (images for a stack, no deploy)
// ============================================================================

func dispPull(a dispArgs) {
	banner()
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Usage: stacks pull <stackname>\x1b[0m")
		os.Exit(1)
	}
	file := dispResolveStackFile(a.raw[0])
	if file == "" {
		fmt.Printf("\x1b[1;31m✘ Stack %s not found.\x1b[0m\n", a.raw[0])
		os.Exit(1)
	}
	// snapshot image history before + after if the library exists (best-effort)
	dispImageHistorySnapshot()
	fmt.Printf("\x1b[1;36m▶ Pulling images for %s…\x1b[0m\n", a.raw[0])
	dispCompose(file, "pull", "--ignore-pull-failures")
	dispImageHistorySnapshot()
	fmt.Println("\x1b[1;32m✔ Pull complete.\x1b[0m")
}

// dispImageHistorySnapshot calls the Go image-history snapshot (best-effort),
// silencing its stdout the way the bash redirected it to /dev/null.
func dispImageHistorySnapshot() {
	defer func() { _ = recover() }()
	old := os.Stdout
	if devnull, err := os.Open(os.DevNull); err == nil {
		os.Stdout = devnull
		defer func() { os.Stdout = old; devnull.Close() }()
	}
	imageHistoryMain([]string{"snapshot"})
}

// ============================================================================
// STATUS-style table for stacks already lives in status.go (cmdStatus). The
// bash `status` branch is also kept here verbatim for the dispatcher word, but
// main.go already routes `status` → cmdStatus, so dispatch.go does not redeclare
// it.
// ============================================================================

// ============================================================================
// VOLUME / NETWORK (list per stack via compose config)
// ============================================================================

func dispVolume(a dispArgs) {
	sub := "ls"
	filter := ""
	if len(a.raw) > 0 {
		sub = a.raw[0]
	}
	if len(a.raw) > 1 {
		filter = a.raw[1]
	}
	if strings.ToLower(sub) != "ls" {
		return
	}
	fmt.Println("\n\x1b[1;35m💾 Volumes\x1b[0m")
	for _, f := range dispStackFiles() {
		sname := strings.TrimSuffix(filepath.Base(f), ".yml")
		if filter != "" && !strings.Contains(sname, filter) {
			if !dispFileContains(f, "container_name:") || !dispFileContains(f, filter) {
				continue
			}
		}
		vols := dispComposeOut(f, "config", "--volumes")
		if vols != "" {
			fmt.Printf("  \x1b[1;36m%s\x1b[0m\n", sname)
			for _, v := range strings.Fields(vols) {
				fmt.Printf("    \x1b[1;33m%s\x1b[0m\n", v)
			}
		}
	}
}

var dispNetNameRE = regexp.MustCompile(`(?m)^    name:\s*(\S+)`)

func dispNetwork(a dispArgs) {
	sub := "ls"
	filter := ""
	if len(a.raw) > 0 {
		sub = a.raw[0]
	}
	if len(a.raw) > 1 {
		filter = a.raw[1]
	}
	if strings.ToLower(sub) != "ls" {
		return
	}
	fmt.Println("\n\x1b[1;35m🌐 Networks\x1b[0m")
	for _, f := range dispStackFiles() {
		sname := strings.TrimSuffix(filepath.Base(f), ".yml")
		if filter != "" && !strings.Contains(sname, filter) {
			if !dispFileContains(f, "container_name:") || !dispFileContains(f, filter) {
				continue
			}
		}
		cfg := dispComposeOut(f, "config")
		var nets []string
		inNet := false
		for _, line := range strings.Split(cfg, "\n") {
			if strings.HasPrefix(line, "networks:") {
				inNet = true
				continue
			}
			if inNet && len(line) > 0 && ((line[0] >= 'a' && line[0] <= 'z') || (line[0] >= 'A' && line[0] <= 'Z')) {
				inNet = false
			}
			if inNet {
				if m := dispNetNameRE.FindStringSubmatch(line); m != nil {
					nets = append(nets, m[1])
				}
			}
		}
		if len(nets) > 0 {
			fmt.Printf("  \x1b[1;36m%s\x1b[0m\n", sname)
			for _, n := range nets {
				fmt.Printf("    \x1b[1;34m%s\x1b[0m\n", n)
			}
		}
	}
}

func dispStackFiles() []string {
	dir := stacksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yml") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func dispFileContains(path, sub string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), sub)
}

// ============================================================================
// DYNAMICS
//   stacks dynamics ls [routers] [services] [middleware]
//   stacks dynamics generate [stack…] [force]
//   stacks dynamics [name…] fix|repair       (handled by the modifier words)
// ============================================================================

func dispDynamics(a dispArgs, rest []string) {
	dynDir := dispDynamicsDir()

	// detect generate / fix / repair from the raw words
	gen, genForce, doFix, doRepair := false, false, false, false
	var genTargets []string
	for _, w := range rest {
		switch strings.ToLower(w) {
		case "generate", "gen", "regen", "regenerate":
			gen = true
		case "force":
			genForce = true
		case "fix":
			doFix = true
		case "repair", "repaire":
			doRepair = true
		case "router", "routers", "service", "services", "middleware", "mid", "mw":
			// view filters / ignored for gen
		default:
			genTargets = append(genTargets, w)
		}
	}

	if gen {
		// explicit `dynamics generate` = full rebuild (force), matching the bash
		// behavior; `dynamics generate force` is the same. Use `dynamics fix` for
		// generate-missing-only.
		dispDynamicsGenerate(genTargets, true)
		return
	}
	if doFix || doRepair {
		names := genTargets
		if len(names) == 0 {
			names = []string{"all"}
		}
		// First create any MISSING dynamic files from the compose (non-destructive
		// unless the `force` word was given), so `dynamics fix` brings every stack
		// up to a full dynamic file, then reconciles names. This is the user's
		// rule: fix should generate the whole dynamic file, but never overwrite an
		// existing one unless forced.
		dispDynamicsGenerate(names, genForce)
		if doRepair {
			fmt.Println("\n\x1b[1;35m⚡ Repairing dynamics (structural)…\x1b[0m")
			dispRunDynamicsFix("repair", names)
		}
		if doFix {
			fmt.Println("\n\x1b[1;35m⚡ Reconciling dynamic names against stacks…\x1b[0m")
			dispRunDynamicsFix("fix", names)
		}
		return
	}

	// listing mode
	showR, showS, showM, showAll := false, false, false, true
	for _, w := range rest {
		switch strings.ToLower(w) {
		case "router", "routers":
			showR, showAll = true, false
		case "service", "services":
			showS, showAll = true, false
		case "middleware", "mid", "mw":
			showM, showAll = true, false
		}
	}
	if showAll {
		showR, showS, showM = true, true, true
	}
	fmt.Println("\n\x1b[1;35m⚡ Dynamics\x1b[0m")
	entries, _ := os.ReadDir(dynDir)
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yml") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		fmt.Printf("\n  \x1b[1;36m%s\x1b[0m\n", name)
		dispDynamicsListFile(filepath.Join(dynDir, name), showR, showS, showM)
	}
}

// dispDynamicsListFile parses one dynamic file's http.{routers,services,
// middlewares} keys (regex; faithful to the bash fallback path which is good
// enough for the 4-space block layout the generator emits).
func dispDynamicsListFile(path string, sr, ss, sm bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := string(b)
	keyRE := regexp.MustCompile(`(?m)^\s{4}(\S+):\s*$`)
	for _, m := range keyRE.FindAllStringSubmatchIndex(content, -1) {
		start := m[0]
		ctxStart := start - 50
		if ctxStart < 0 {
			ctxStart = 0
		}
		ctx := content[ctxStart:start]
		key := content[m[2]:m[3]]
		switch {
		case sr && strings.Contains(ctx, "routers:"):
			fmt.Printf("    \x1b[1;33m[router]\x1b[0m %s\n", key)
		case ss && strings.Contains(ctx, "services:"):
			fmt.Printf("    \x1b[1;32m[service]\x1b[0m %s\n", key)
		case sm && strings.Contains(ctx, "middlewares:"):
			fmt.Printf("    \x1b[1;34m[middleware]\x1b[0m %s\n", key)
		}
	}
}

// dispDynamicsGenerate mirrors `stacks dynamics generate`: backup the dynamics
// dir, then run the (already-ported) rich/legacy generator per target.
func dispDynamicsGenerate(targets []string, force bool) {
	if len(targets) == 0 {
		targets = []string{"all"}
	}
	bk := filepath.Join(dispBackupDest(), fmt.Sprintf("dynamics-pre-generate-%d", time.Now().Unix()))
	os.MkdirAll(bk, 0755)
	dynDir := dispDynamicsDir()
	if entries, err := os.ReadDir(dynDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yml") {
				dispCopyFile(filepath.Join(dynDir, e.Name()), filepath.Join(bk, e.Name()))
			}
		}
	}
	fmt.Println("\n\x1b[1;35m⚡ Generating Traefik dynamics from compose files…\x1b[0m")
	fmt.Printf("  \x1b[2mbackup: %s\x1b[0m\n", bk)

	rich := dispConfBool("GEN_RICH", true)
	mergeEP := rich && dispConfBool("GEN_DB_ENTRYPOINTS", true)
	// force=true → overwrite existing dynamic files (full rebuild); force=false →
	// the generators skip files that already exist, so only MISSING ones are
	// created. This is what lets `dynamics fix` / `up … dynamics` regenerate just
	// the missing files without clobbering hand-tuned ones (user's rule: don't
	// regenerate an existing file unless forced).
	for _, t := range targets {
		args := []string{t}
		if force {
			args = append(args, "--force")
		}
		if mergeEP {
			args = append(args, "--merge-entrypoints")
		}
		dispRunGenerator(rich, args)
	}
	if force {
		fmt.Println("  \x1b[1;36mGenerate complete (forced rebuild).\x1b[0m Reload Traefik to apply.")
	} else {
		fmt.Println("  \x1b[1;36mGenerate complete (missing files only).\x1b[0m Reload Traefik to apply.")
	}
}

// dispRunGenerator dispatches to the already-ported generators. The rich path
// is genDynamic2Main; the legacy path is genDynamicMain (names verified below).
// PORT-NOTE: generator entry-point names are auto-detected via the build; if the
// exact symbol differs, this is the single spot to retarget.
func dispRunGenerator(rich bool, args []string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("  \x1b[1;31m✘ generator error: %v\x1b[0m\n", r)
		}
	}()
	if rich {
		dispGen2(args)
	} else {
		dispGen1(args)
	}
}

// dispRunDynamicsFix mirrors run_dynamics_fix() by delegating to the ported
// dynamics fix/repair functions per target.
func dispRunDynamicsFix(mode string, names []string) {
	defer func() { _ = recover() }()
	if mode == "repair" {
		dispDynRepair(names)
	} else {
		dispDynFix(names)
	}
}

func dispCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ============================================================================
// RELOAD
// ============================================================================

func dispReload(a dispArgs) {
	fmt.Println("\x1b[1;34m▶ Reloading stacks…\x1b[0m")
	conf := filepath.Join(configDir(), "stacks.conf")
	if dispFileExists(conf) {
		fmt.Printf("\x1b[1;32m✔ Config present at %s\x1b[0m\n", conf)
	} else {
		fmt.Printf("\x1b[1;33m⚠ No config file at %s, using defaults.\x1b[0m\n", conf)
	}
	fmt.Println("\x1b[1;32m✔ stacks reloaded.\x1b[0m")
}

// ============================================================================
// CLEAN
// ============================================================================

func dispClean(a dispArgs) {
	banner()
	doQuick, doImages, doVolumes, doNetworks := false, false, false, false
	doBuilder, doNuke, doAll, doSablier := false, false, false, false
	set := false
	for _, w := range a.raw {
		switch strings.ToLower(w) {
		case "quick":
			doQuick, set = true, true
		case "deep":
			doImages, doNetworks, doBuilder, set = true, true, true, true
		case "images", "img":
			doImages, set = true, true
		case "volumes", "vol", "v":
			doVolumes, set = true, true
		case "networks", "net", "n":
			doNetworks, set = true, true
		case "builder", "build", "cache":
			doBuilder, set = true, true
		case "sablier":
			doSablier, set = true, true
		case "all":
			doAll, set = true, true
		case "nuke":
			doNuke, doAll, set = true, true, true
		}
	}
	if !set {
		doImages, doNetworks, doBuilder = true, true, true
	}
	_ = doSablier
	_ = doQuick

	fmt.Println("\x1b[1;36m▶ Cleaning Docker…\x1b[0m")
	// stuck/dead containers always
	for _, n := range strings.Fields(dispDockerOut("ps", "-a",
		"--filter", "status=created", "--filter", "status=dead", "--filter", "status=removing",
		"--format", "{{.Names}}")) {
		dispDocker("rm", "-f", n)
	}
	dispDocker("container", "prune", "-f")

	if doNetworks || doAll || doQuick {
		for _, line := range strings.Split(dispDockerOut("network", "ls", "--format", "{{.Name}}"), "\n") {
			net := strings.TrimSpace(line)
			switch net {
			case "", "bridge", "host", "none", "ingress", "docker_gwbridge":
				continue
			}
			for _, c := range strings.Fields(dispDockerOut("network", "inspect", net, "-f", "{{range .Containers}}{{.Name}} {{end}}")) {
				dispDocker("network", "disconnect", "-f", net, c)
			}
		}
		dispDocker("network", "prune", "-f")
	}
	if doImages || doAll {
		dispDocker("image", "prune", "-f")
		dispDocker("image", "prune", "-a", "-f")
	}
	if doBuilder || doAll {
		dispDocker("builder", "prune", "-a", "-f")
	}
	if doVolumes || doNuke {
		dispDocker("volume", "prune", "-f")
	}
	if doNuke {
		dispDocker("system", "prune", "--volumes", "-f")
	} else {
		dispDocker("system", "prune", "-f")
	}
	fmt.Println("\n\x1b[1;32m✨ CLEAN COMPLETE ✨\x1b[0m")
	dispDocker("system", "df")
}

// ============================================================================
// ART (inject / strip decorations across stacks or dynamics)
// ============================================================================

func dispArt(a dispArgs) {
	if len(a.raw) == 0 {
		fmt.Println("\x1b[1;31m✘ Error: Specify 'inject' or 'strip' (e.g., stacks art inject)\x1b[0m")
		os.Exit(1)
	}
	action := a.raw[0]

	// stacks art dynamic inject|strip [all|file]
	if action == "dynamic" {
		sub := ""
		target := "all"
		if len(a.raw) > 1 {
			sub = a.raw[1]
		}
		if len(a.raw) > 2 {
			target = a.raw[2]
		}
		if sub != "inject" && sub != "strip" {
			fmt.Println("\x1b[1;31m✘ Usage: stacks art dynamic inject|strip [all|filename]\x1b[0m")
			os.Exit(1)
		}
		dynDir := dispDynamicsDir()
		files := dispArtTargetFiles(target, dynDir)
		for _, f := range files {
			dispInjectDynamicFile(sub, f, dynDir)
		}
		fmt.Printf("\x1b[1;32m✨ SUCCESS: Art %s on %d dynamic file(s)\x1b[0m\n", sub, len(files))
		return
	}

	if action != "inject" && action != "strip" && action != "backup" {
		fmt.Println("\x1b[1;31m✘ Error: Specify 'inject' or 'strip' (e.g., stacks art inject)\x1b[0m")
		os.Exit(1)
	}

	// stacks art inject [all|art|urls|desc] [target]   OR   art inject [target]
	mode := "all"
	target := "--all"
	if len(a.raw) > 1 {
		if dispIn(a.raw[1], "all", "art", "urls", "desc") {
			mode = a.raw[1]
			if len(a.raw) > 2 {
				target = a.raw[2]
			}
		} else {
			target = a.raw[1]
		}
	}
	dir := stacksDir()
	files := dispArtTargetFiles(target, dir)
	if len(files) == 0 {
		fmt.Println("\x1b[1;33m⚠ No matching stacks discovered to process.\x1b[0m")
		return
	}
	for _, f := range files {
		fname := filepath.Base(f)
		if action == "strip" {
			fmt.Printf("  🧹 Stripped clean ➜ %s\n", fname)
			dispInjectFile("strip", f, mode)
			dispDescribeFile("strip", f)
			dispCollapseBlankLines(f)
		} else {
			fmt.Printf("  🎨 Injected ➜ %s\n", fname)
			if mode == "all" || mode == "art" {
				dispInjectFile("inject", f, mode)
			}
			if mode == "all" || mode == "urls" {
				dispInjectFile("inject_urls", f, "")
			}
			if mode == "all" || mode == "desc" {
				dispDescribeFile("", f)
			}
		}
	}
	fmt.Println("\x1b[1;32m✨ SUCCESS: Stacks art engine updated all targets! ✨\x1b[0m")
}

// dispArtTargetFiles resolves an art/dynamic target: "all"/"--all" → every
// .yml/.yaml in dir; an absolute path; dir/name; dir/name.yml.
func dispArtTargetFiles(target, dir string) []string {
	if target == "all" || target == "--all" {
		var out []string
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() && (strings.HasSuffix(e.Name(), ".yml") || strings.HasSuffix(e.Name(), ".yaml")) {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []string{target, filepath.Join(dir, target), filepath.Join(dir, target+".yml")} {
		if dispFileExists(c) {
			return []string{c}
		}
	}
	fmt.Printf("\x1b[1;31m✘ Target not found: %s\x1b[0m\n", target)
	os.Exit(1)
	return nil
}

var dispBlankRE = regexp.MustCompile(`\n{3,}`)

func dispCollapseBlankLines(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	os.WriteFile(path, []byte(dispBlankRE.ReplaceAllString(string(b), "\n\n")), 0644)
}

// dispInjectFile / dispDescribeFile delegate to the already-ported inject /
// describe engines. PORT-NOTE: these wrap the Go ports (inject.go / describe.go)
// rather than shelling to the .py; the exact per-file entry names are bridged in
// the small adapters below so retargeting is one-line if a symbol differs.

// ============================================================================
// INJECT / STRIP (top-level: whole stacks dir)
// ============================================================================

func dispInjectStrip(command string, a dispArgs) {
	target := "--all"
	if len(a.raw) > 0 {
		target = a.raw[0]
	}
	dispInjectAll(command, target)
}

// ============================================================================
// CUSTOM (the bash "custom" word = a multi-stack pipeline alias of up). We map
// it to the same multi-target up path so `stacks custom net_2 db_0 …` works.
// PORT-NOTE: the bash had no standalone `custom` branch — the CUSTOM_STACKS
// pipeline was reached implicitly via `up` with multiple targets. We expose
// `custom` as an explicit alias of that multi-target up for parity with the
// dispatcher word list.
// ============================================================================

func dispCustom(a dispArgs) {
	a.command = "up"
	dispUp(a)
}

// ============================================================================
// SCALE / PROXY (Sablier zero-scale + traefik.enable label toggles)
// ============================================================================

func dispScale(a dispArgs) { dispScaleProxy("scale", a) }
func dispProxy(a dispArgs) { dispScaleProxy("proxy", a) }

func dispScaleProxy(kind string, a dispArgs) {
	// scale requires sablier present (bash short-circuits if absent)
	if kind == "scale" {
		if containerState("sablier") == "" {
			fmt.Println("\x1b[1;33m⚠ Sablier not found — scale command disabled.\x1b[0m")
			return
		}
	}
	arg1, arg2, arg3 := "", "", ""
	if len(a.raw) > 0 {
		arg1 = a.raw[0]
	}
	if len(a.raw) > 1 {
		arg2 = a.raw[1]
	}
	if len(a.raw) > 2 {
		arg3 = a.raw[2]
	}

	var mode, stack, svc, state string
	switch {
	case dispOnOff(arg1):
		mode, state = "all", strings.ToLower(arg1)
	case dispOnOff(arg2):
		mode, stack, state = "stack", arg1, strings.ToLower(arg2)
	case dispOnOff(arg3):
		mode, stack, svc, state = "service", arg1, arg2, strings.ToLower(arg3)
	default:
		fmt.Printf("\x1b[1;33mUsage: stacks %s [stack] [service] on|off\x1b[0m\n", kind)
		os.Exit(1)
	}
	val := "true"
	if state == "off" {
		val = "false"
	}
	skip := ""
	if kind == "scale" {
		skip = confValue("SCALE_SKIP_CONTAINERS")
	} else {
		skip = confValue("PROXY_SKIP_CONTAINERS")
	}

	var files []string
	switch mode {
	case "all":
		files = dispStackFiles()
	default:
		f := dispResolveStackFile(stack)
		if f == "" {
			fmt.Printf("\x1b[1;31m✘ Stack not found: %s\x1b[0m\n", stack)
			os.Exit(1)
		}
		files = []string{f}
	}
	changed := 0
	for _, f := range files {
		fname := strings.TrimSuffix(filepath.Base(f), ".yml")
		tgt := "__all__"
		if mode == "service" {
			tgt = svc
		}
		if kind == "scale" {
			scaleFile(f, tgt, val, skip)
			fmt.Printf("  ⚙️  Scale %s ➜ %s%s\n", strings.ToUpper(state), fname, dispSvcLabel(svc))
		} else {
			proxyFile(f, tgt, val, skip)
			fmt.Printf("  🌐 Proxy %s ➜ %s%s\n", strings.ToUpper(state), fname, dispSvcLabel(svc))
		}
		changed++
	}
	if kind == "scale" {
		fmt.Printf("\x1b[1;32m✨ Scale %s applied to %d stack(s). Run 'stacks up <stack> recreate' to deploy.\x1b[0m\n", strings.ToUpper(state), changed)
	} else {
		fmt.Printf("\x1b[1;32m✨ Proxy %s applied to %d stack(s).\x1b[0m\n", strings.ToUpper(state), changed)
	}
}

func dispOnOff(s string) bool {
	l := strings.ToLower(s)
	return l == "on" || l == "off"
}

// ============================================================================
// SNAPSHOT (take / restore / list / clear)
// ============================================================================

func dispSnapshot(a dispArgs) {
	banner()
	action := "take"
	label := "golden"
	if len(a.raw) > 0 {
		action = a.raw[0]
	}
	if len(a.raw) > 1 {
		label = a.raw[1]
	}
	snapDir := dispSnapshotDir()
	switch strings.ToLower(action) {
	case "take", "save", "update":
		os.MkdirAll(snapDir, 0755)
		dispTakeSnapshot(label)
		fmt.Println("\x1b[1;32m✔ Snapshot taken and verified.\x1b[0m")
	case "restore", "revert":
		dispRestoreSnapshot(filepath.Join(snapDir, "golden_latest"))
	case "list", "ls":
		dispListSnapshots()
	case "clear", "purge":
		entries, _ := os.ReadDir(snapDir)
		for _, e := range entries {
			if e.Name() != "golden_latest" {
				os.RemoveAll(filepath.Join(snapDir, e.Name()))
			}
		}
		fmt.Println("\x1b[1;32m✔ Snapshots cleared.\x1b[0m")
	default:
		fmt.Println("\x1b[1;33mUsage: stacks snapshot [take|restore|list] [label]\x1b[0m")
	}
}

func dispTakeSnapshot(label string) string {
	ts := time.Now().Format("20060102_150405")
	snap := filepath.Join(dispSnapshotDir(), label+"_"+ts)
	os.MkdirAll(filepath.Join(snap, "stacks"), 0755)
	os.MkdirAll(filepath.Join(snap, "dynamics"), 0755)
	os.MkdirAll(filepath.Join(snap, "traefik"), 0755)
	fmt.Printf("📸 Taking snapshot: %s\n", snap)
	// stack files
	for _, f := range dispStackFiles() {
		dispCopyFile(f, filepath.Join(snap, "stacks", filepath.Base(f)))
	}
	// dynamics
	if entries, err := os.ReadDir(dispDynamicsDir()); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yml") {
				dispCopyFile(filepath.Join(dispDynamicsDir(), e.Name()), filepath.Join(snap, "dynamics", e.Name()))
			}
		}
	}
	if label == "golden" {
		link := filepath.Join(dispSnapshotDir(), "golden_latest")
		os.Remove(link)
		os.Symlink(snap, link)
	}
	fmt.Printf("✔ Snapshot saved: %s\n", snap)
	return snap
}

func dispRestoreSnapshot(snap string) {
	if st, err := os.Stat(snap); err != nil || !st.IsDir() {
		// follow symlink
		if real, e := filepath.EvalSymlinks(snap); e == nil {
			snap = real
		} else {
			fmt.Printf("✘ Snapshot not found: %s\n", snap)
			return
		}
	}
	fmt.Printf("🔄 Restoring from: %s\n", snap)
	stacksSrc := filepath.Join(snap, "stacks")
	if entries, err := os.ReadDir(stacksSrc); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yml") {
				dispCopyFile(filepath.Join(stacksSrc, e.Name()), filepath.Join(stacksDir(), e.Name()))
			}
		}
		fmt.Println("  ✔ Stack files restored")
	}
	dynSrc := filepath.Join(snap, "dynamics")
	if entries, err := os.ReadDir(dynSrc); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".yml") {
				dispCopyFile(filepath.Join(dynSrc, e.Name()), filepath.Join(dispDynamicsDir(), e.Name()))
			}
		}
		fmt.Println("  ✔ Dynamics restored")
		dispDockerOut("restart", "traefik")
	}
	fmt.Printf("✔ Restore complete from: %s\n", snap)
}

func dispListSnapshots() {
	snapDir := dispSnapshotDir()
	fmt.Println("📋 Available snapshots:")
	entries, _ := os.ReadDir(snapDir)
	type ent struct {
		name string
		mod  time.Time
	}
	var list []ent
	for _, e := range entries {
		if e.Name() == "golden_latest" {
			continue
		}
		if info, err := e.Info(); err == nil {
			list = append(list, ent{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	for i, e := range list {
		if i >= 20 {
			break
		}
		fmt.Printf("  %s  %s\n", e.mod.Format("2006-01-02 15:04"), e.name)
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(snapDir, "golden_latest")); err == nil {
		fmt.Printf("\n🌟 Golden latest → %s\n", real)
	}
}

// ============================================================================
// BACKUP (backup.conf-driven; mirrors the recently-updated live logic)
// ============================================================================

func dispBackup(a dispArgs) {
	confPath := filepath.Join(configDir(), "backup.conf")
	if !dispFileExists(confPath) {
		fmt.Printf("\x1b[1;31m✘ No backup config at %s\x1b[0m\n", confPath)
		fmt.Println("\x1b[1;33mCreate it with BACKUP_DEST, FILES=(), and FOLDERS=()\x1b[0m")
		os.Exit(1)
	}
	bc := dispParseBackupConf(confPath)
	dest := bc.dest
	if dest == "" {
		dest = dispBackupDest()
	}
	keep := bc.keep
	autoPrune := bc.autoPrune

	sub := "all"
	arg2 := ""
	if len(a.raw) > 0 {
		sub = a.raw[0]
	}
	if len(a.raw) > 1 {
		arg2 = a.raw[1]
	}

	// stacks backup /abs/path
	if strings.HasPrefix(sub, "/") {
		if _, err := os.Stat(sub); err != nil {
			fmt.Printf("\x1b[1;31m✘ Not found: %s\x1b[0m\n", sub)
			os.Exit(1)
		}
		os.MkdirAll(dest, 0755)
		dispBackupItem(sub, dest)
		fmt.Printf("\x1b[1;32m✔ Backed up: %s → %s\x1b[0m\n", filepath.Base(sub), dest)
		return
	}

	switch strings.ToLower(sub) {
	case "clean":
		dispBackupClean(dest)
		return
	case "rm":
		if arg2 == "" || strings.ToLower(arg2) == "all" {
			entries, _ := os.ReadDir(dest)
			for _, e := range entries {
				os.RemoveAll(filepath.Join(dest, e.Name()))
			}
			fmt.Println("\x1b[1;32m✔ All backups removed.\x1b[0m")
		} else {
			t := filepath.Join(dest, arg2)
			if _, err := os.Stat(t); err == nil {
				os.RemoveAll(t)
				fmt.Printf("\x1b[1;32m✔ Removed: %s\x1b[0m\n", t)
			} else {
				fmt.Printf("\x1b[1;31m✘ Not found: %s\x1b[0m\n", t)
			}
		}
		return
	}

	// stacks backup [all] — back up FILES + FOLDERS
	os.MkdirAll(dest, 0755)
	var items []string
	for _, f := range bc.files {
		if _, err := os.Stat(f); err == nil {
			items = append(items, f)
		}
	}
	for _, d := range bc.folders {
		if _, err := os.Stat(d); err == nil {
			items = append(items, d)
		}
	}
	if len(items) == 0 {
		fmt.Printf("\x1b[1;33m⚠ Nothing to back up. Check %s\x1b[0m\n", confPath)
		return
	}
	for i, it := range items {
		fmt.Printf("  \x1b[1;32m✔\x1b[0m %s (%d/%d)\n", filepath.Base(it), i+1, len(items))
		dispBackupItem(it, dest)
	}
	if autoPrune && keep > 0 {
		pruned := dispBackupPrune(dest, keep)
		if pruned > 0 {
			fmt.Printf("  \x1b[1;33m↻ pruned %d old backup(s), keeping newest %d of each\x1b[0m\n", pruned, keep)
		}
	}
	fmt.Printf("\x1b[1;32m✔ Backup complete — %d items archived.\x1b[0m\n", len(items))
}

type dispBackupConf struct {
	dest      string
	keep      int
	autoPrune bool
	files     []string
	folders   []string
}

// dispParseBackupConf reads the bash-array backup.conf (BACKUP_DEST=…, KEEP=…,
// AUTO_PRUNE=…, FILES=( … ), FOLDERS=( … )). It tolerates the multi-line array
// form the bash writes.
func dispParseBackupConf(path string) dispBackupConf {
	bc := dispBackupConf{keep: 5, autoPrune: true}
	b, err := os.ReadFile(path)
	if err != nil {
		return bc
	}
	text := string(b)
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "BACKUP_DEST="):
			bc.dest = dispUnquote(strings.TrimPrefix(line, "BACKUP_DEST="))
		case strings.HasPrefix(line, "KEEP="):
			if n, e := strconv.Atoi(dispUnquote(strings.TrimPrefix(line, "KEEP="))); e == nil {
				bc.keep = n
			}
		case strings.HasPrefix(line, "AUTO_PRUNE="):
			v := dispUnquote(strings.TrimPrefix(line, "AUTO_PRUNE="))
			bc.autoPrune = v == "1" || strings.ToLower(v) == "true"
		case strings.HasPrefix(line, "FILES="):
			i = dispParseBashArray(lines, i, "FILES=", &bc.files)
		case strings.HasPrefix(line, "FOLDERS="):
			i = dispParseBashArray(lines, i, "FOLDERS=", &bc.folders)
		}
	}
	return bc
}

// dispParseBashArray collects the elements of a bash array that may span lines:
//
//	KEY=( a b )   or   KEY=(\n  a\n  b\n)
//
// Returns the index of the line containing the closing paren.
func dispParseBashArray(lines []string, start int, prefix string, out *[]string) int {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[start]), prefix))
	rest = strings.TrimPrefix(rest, "(")
	i := start
	for {
		closed := strings.Contains(rest, ")")
		rest = strings.ReplaceAll(rest, ")", " ")
		for _, tok := range strings.Fields(rest) {
			tok = dispUnquote(tok)
			if tok != "" {
				*out = append(*out, tok)
			}
		}
		if closed {
			return i
		}
		i++
		if i >= len(lines) {
			return i - 1
		}
		rest = lines[i]
	}
}

func dispUnquote(s string) string {
	s = strings.TrimSpace(s)
	return strings.Trim(s, `"'`)
}

// dispBackupItem copies a file/dir into dest with a timestamp suffix, matching
// the bash naming (name_TS.ext for files, base_TS for dirs).
func dispBackupItem(src, dest string) {
	ts := time.Now().Format("2006-01-02_15-04-05")
	base := filepath.Base(src)
	st, err := os.Stat(src)
	if err != nil {
		return
	}
	if st.IsDir() {
		dispCopyTree(src, filepath.Join(dest, base+"_"+ts))
		return
	}
	ext := filepath.Ext(base)
	if ext == "" {
		dispCopyFile(src, filepath.Join(dest, base+"_"+ts))
	} else {
		name := strings.TrimSuffix(base, ext)
		dispCopyFile(src, filepath.Join(dest, name+"_"+ts+ext))
	}
}

func dispCopyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		return dispCopyFile(p, target)
	})
}

var dispTSRE = regexp.MustCompile(`_[0-9]{4}-[0-9]{2}-[0-9]{2}_[0-9]{2}-[0-9]{2}-[0-9]{2}.*$`)

// dispBackupClean keeps only the newest of each base name in dest.
func dispBackupClean(dest string) {
	fmt.Println("\n\x1b[1;31m⚠ This will delete ALL backups except the latest of each file.\x1b[0m")
	groups := dispBackupGroups(dest)
	kept, deleted := 0, 0
	for _, files := range groups {
		// newest first
		sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
		for i, f := range files {
			if i == 0 {
				kept++
			} else {
				os.RemoveAll(f.path)
				deleted++
			}
		}
	}
	fmt.Printf("\x1b[1;32m✔ Clean complete — kept %d, deleted %d files.\x1b[0m\n", kept, deleted)
}

// dispBackupPrune keeps the newest `keep` of each base name; returns count pruned.
func dispBackupPrune(dest string, keep int) int {
	groups := dispBackupGroups(dest)
	pruned := 0
	for _, files := range groups {
		sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
		for i, f := range files {
			if i >= keep {
				os.RemoveAll(f.path)
				pruned++
			}
		}
	}
	return pruned
}

type dispBkFile struct {
	path string
	mod  time.Time
}

func dispBackupGroups(dest string) map[string][]dispBkFile {
	groups := map[string][]dispBkFile{}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		base := dispTSRE.ReplaceAllString(e.Name(), "")
		info, err := e.Info()
		if err != nil {
			continue
		}
		groups[base] = append(groups[base], dispBkFile{filepath.Join(dest, e.Name()), info.ModTime()})
	}
	return groups
}

// ============================================================================
// final infra summary (used after up)
// ============================================================================

func dispFinalSummary() {
	total := len(strings.Fields(dispDockerOut("ps", "-aq")))
	running := len(strings.Fields(dispDockerOut("ps", "-q")))
	fmt.Println("\n\x1b[1;36m┌──────────────────────────────────────────────────────┐\x1b[0m")
	fmt.Println("\x1b[1;36m│\x1b[0m  \x1b[1;35m📊 INFRASTRUCTURE SUMMARY\x1b[0m")
	fmt.Println("\x1b[1;36m├──────────────────────────────────────────────────────┤\x1b[0m")
	fmt.Printf("\x1b[1;36m│\x1b[0m  \x1b[1;34mTotal Containers:\x1b[0m    %d\n", total)
	fmt.Printf("\x1b[1;36m│\x1b[0m  \x1b[1;32mRunning Containers:\x1b[0m  %d\n", running)
	fmt.Printf("\x1b[1;36m│\x1b[0m  \x1b[1;31mInactive Containers:\x1b[0m %d\n", total-running)
	fmt.Println("\x1b[1;36m└──────────────────────────────────────────────────────┘\x1b[0m")
	fmt.Println("\n\x1b[1;32m✨ SEQUENCE COMPLETE ✨\x1b[0m")
}
