package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ===== from main.go =====

// main.go — the single entry point. It ONLY routes the command word to the right
// handler; each command's real code lives in its own file (status.go, up.go, …).
// To add/change a command: edit its file, then add one line to the switch below.

func Run() {
	args := os.Args[1:]
	if len(args) == 0 {
		banner()
		fmt.Println("\nusage: stacks <command>   (status | ls | version | menu | …)")
		os.Exit(1)
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "status":
		cmdStatus(rest) // status.go
	case "ls":
		cmdLs(rest) // ls.go
	case "dedupe":
		cmdDedupe(rest) // dedupe.go
	case "netdedupe":
		cmdNetdedupe(rest) // netdedupe.go
	case "menu":
		cmdMenu(rest) // menu.go
	case "build":
		cmdBuild(rest) // buildcmd.go
	case "regsearch", "search":
		cmdRegsearch(rest) // search.go
	case "fix":
		cmdFix(rest) // fix.go
	case "describe":
		describeMain(rest) // describe.go
	case "purge":
		purgeMain(rest) // purge.go
	case "volclean":
		volcleanMain(rest) // volclean.go
	case "reclaim":
		cmdReclaim(rest) // reclaim.go
	case "update":
		updMain(rest) // updates.go
	case "selfupdate", "upgrade":
		selfupdateMain(rest) // selfupdate.go
	case "images":
		imageHistoryMain(rest) // imagehistory.go
	case "logdump", "logsync":
		cmdLogdump(rest) // logs.go — write one log file per running container

	// ── lifecycle + utility commands (dispatch.go) ────────────────────────────
	case "up":
		// `stacks up --boot` = the controlled, parallel, strategy-driven boot
		// bring-up (boot.go) instead of a normal interactive up.
		isBoot := false
		filtered := rest[:0]
		for _, r := range rest {
			if r == "--boot" || r == "boot" {
				isBoot = true
				continue
			}
			filtered = append(filtered, r)
		}
		if isBoot {
			runBoot()
		} else {
			dispUp(dispParse(cmd, filtered))
		}
	case "boot":
		cmdBoot(rest) // boot.go — install/uninstall/status/run the boot+watchdog services
	case "watch":
		cmdWatch(rest) // boot.go — the 24/7 watchdog loop
	case "install", "setup":
		cmdBoot([]string{"install"}) // boot.go — install Docker (if missing) + boot/watchdog services
	case "zeroscale", "zs", "wake":
		cmdZeroScale(rest) // zeroscale.go — wake-on-visit engine (Sablier replacement)
	case "park":
		cmdPark(rest) // zeroscale.go — sleep all but never_sleep (replaces old stacks-park)
	case "argus", "edge", "sentinel":
		cmdArgus(rest) // argus.go — VPS edge-watchdog: probe public sites + repair the edge
	case "down":
		dispDown(dispParse(cmd, rest))
	case "start", "stop", "restart":
		dispManage(dispParse(cmd, rest))
	case "recreate":
		dispRecreate(dispParse(cmd, rest))
	case "heal", "healall", "fixall", "allfix":
		// one-word do-everything: up + force-recreate (fix/repair/dynamics are
		// applied via the ported fix/repair/dynamics engines as needed)
		a := dispParse("up", rest)
		a.recreate = true
		dispUp(a)
	case "custom":
		dispCustom(dispParse(cmd, rest))
	case "rm":
		dispRm(dispParse(cmd, rest))
	case "get":
		dispGet(dispParse(cmd, rest))
	case "kill":
		dispKill(dispParse(cmd, rest))
	case "logs":
		dispLogs(dispParse(cmd, rest))
	case "inspect":
		dispInspect(dispParse(cmd, rest))
	case "pull":
		dispPull(dispParse(cmd, rest))
	case "reload":
		dispReload(dispParse(cmd, rest))
	case "clean":
		dispClean(dispParse(cmd, rest))
	case "snapshot":
		dispSnapshot(dispParse(cmd, rest))
	case "volume":
		dispVolume(dispParse(cmd, rest))
	case "network":
		dispNetwork(dispParse(cmd, rest))
	case "dynamics":
		dispDynamics(dispParse(cmd, rest), rest)
	case "art":
		dispArt(dispParse(cmd, rest))
	case "inject", "strip":
		dispInjectStrip(cmd, dispParse(cmd, rest))
	case "scale":
		dispScale(dispParse(cmd, rest))
	case "proxy":
		dispProxy(dispParse(cmd, rest))
	case "backup":
		dispBackup(dispParse(cmd, rest))
	case "version", "--version", "-v":
		fmt.Println(stacksVersion()) // loadingbar.go
	case "__families": // internal: mirrors `python3 stacks_families.py` report (families.go)
		familiesReport()
	case "__gensrvs": // internal: mirrors `python3 stacks_gen_srvs.py` (gensrvs.go)
		genServices()
	case "__geninject": // internal: mirrors `python3 stacks_gen_gi.py global_inject.conf STACKS_DIR`
		conf := filepath.Join(configDir(), "global_inject.conf")
		if err := genGlobalInject(conf, stacksDir()); err != nil {
			fmt.Println("✘ gen inject:", err)
		} else {
			fmt.Println("✔ Generated", conf)
		}
	case "__proxyfile": // internal: mirrors stacks_proxy_file.py <path> <svc> <val> [skip]
		sk := ""
		if len(rest) > 3 {
			sk = rest[3]
		}
		proxyFile(rest[0], rest[1], rest[2], sk)
	case "__scalefile": // internal: mirrors stacks_scale_file.py <path> <svc> <val> [skip]
		sk := ""
		if len(rest) > 3 {
			sk = rest[3]
		}
		scaleFile(rest[0], rest[1], rest[2], sk)
	case "__docker": // internal: mirrors `python3 stacks_docker.py` self-test (docker.go)
		mode := "CLI fallback"
		if apiMode() {
			mode = "API"
		}
		fmt.Println("mode:", mode)
		fmt.Println("containers:", len(containerStateMap()))
		fmt.Println("networks:", len(networkTable()))
	case "__config": // internal: feeds bash `eval "$(stacks __config --env)"` (config_load.go)
		mode := "--env"
		if len(rest) > 0 {
			mode = rest[0]
		}
		if mode == "--check" {
			configCheck()
		} else {
			configEnv()
		}
	default:
		fmt.Printf("unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

// ===== from menu.go =====

// menu.go — the interactive TUI menu (the Go home for what stacks_menu.py does).
// The engine commands are being ported first; the full menu lands here later.
// Keeping it in its own file so the menu work never tangles with the engine.

// cmdMenu launches the interactive Bubble Tea TUI (the Go home for the work that
// stacks_menu.py used to do). The model + tabs live in the menu_*.go siblings.
func cmdMenu(args []string) {
	if err := menuRun(); err != nil {
		fmt.Fprintln(os.Stderr, "menu:", err)
		os.Exit(1)
	}
}

// ===== from status.go =====

// status.go — `stacks status`: a quick count of containers via the Docker API.

func cmdStatus(args []string) {
	banner()
	list := containers(true)
	running, unhealthy := 0, 0
	for _, c := range list {
		if c.State == "running" {
			running++
		}
		if strings.Contains(c.Status, "(unhealthy)") {
			unhealthy++
		}
	}
	fmt.Printf("\n📊 containers: %d total · %d running · %d stopped · %d unhealthy\n",
		len(list), running, len(list)-running, unhealthy)
}

// ===== from ls.go =====

// ls.go — `stacks ls`: list the stack .yml files (skips *-ext VPS stacks).

func cmdLs(args []string) {
	dir := stacksDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("cannot read", dir, ":", err)
		return
	}
	var stacks []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".yml") && !strings.HasSuffix(n, "-ext.yml") {
			stacks = append(stacks, strings.TrimSuffix(n, ".yml"))
		}
	}
	sort.Strings(stacks)
	fmt.Printf("📦 %d stacks in %s:\n", len(stacks), dir)
	for _, s := range stacks {
		fmt.Println("  \x1b[36m" + s + "\x1b[0m")
	}
}

// ===== from ui.go =====

// ui.go — faithful Go port of stacks_ui.py.
// Shared UI helpers for the loading bar / log display:
//   stripNoise(line) : clean a log line of ANSI/control chars
//   isNoise(line)    : true if the line should be skipped
//   cleanLogLine(raw): strip + filter, returning "" for noise
//   stacksArt        : the ASCII banner as a string

// stacksArt is STACKS_ART (single backslashes, exactly as Python renders it).
const stacksArt = `
  ____  _____  _    ____ _  _______
 / ___||_   _|/ \  / ___| |/ /  ___|
 \___ \  | | / _ \| |   | ' /|___ \
  ___) | | |/ ___ \ |___| . \ ___) |
 |____/  |_/_/   \_\____|_|\_\____/
`

// _NOISE: lines that are definitely noise — never shown in a log display.
var reNoise = regexp.MustCompile(
	`[\x1b\x00-\x1f\x7f]` + // control/escape chars
		`|[░█]{2,}` + // block chars from loading bars
		`|\[[\s#>\-=]{3,}` + // old loading bar brackets
		`|Press Ctrl` + // cancel hints
		`|=== ` + // sequence markers
		`|SEQUENCE` +
		`|____` + // ASCII art fragments
		`|\\___` +
		`|/ ___` +
		`|\|____`)

// _ART: art-specific lines to skip.
var reArt = regexp.MustCompile(`^[\s_/\\|.=\[\](){}#*\-]+$`)

// reStripEsc / reStripCtl mirror the two substitutions in strip_noise().
var reStripEsc = regexp.MustCompile(`\x1b[^a-zA-Z]*[a-zA-Z]`)
var reStripCtl = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// rePct mirrors the pure percentage/progress check.
var rePct = regexp.MustCompile(`^[\d\s%]+$`)

// stripNoise strips ANSI codes and control characters from a log line.
func stripNoise(line string) string {
	line = reStripEsc.ReplaceAllString(line, "")
	line = reStripCtl.ReplaceAllString(line, "")
	return strings.TrimSpace(line)
}

// isNoise reports whether this line should NOT be shown in a log display.
func isNoise(line string) bool {
	if line == "" || len([]rune(line)) < 3 {
		return true
	}
	if reNoise.MatchString(line) {
		return true
	}
	if reArt.MatchString(line) {
		return true
	}
	if rePct.MatchString(line) { // pure percentage/progress lines
		return true
	}
	return false
}

// cleanLogLine strips and filters a raw log line; returns "" for noise.
func cleanLogLine(raw string) string {
	line := stripNoise(raw)
	if isNoise(line) {
		return ""
	}
	return line
}
