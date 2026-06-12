package main

// main.go — the single entry point. It ONLY routes the command word to the right
// handler; each command's real code lives in its own file (status.go, up.go, …).
// To add/change a command: edit its file, then add one line to the switch below.

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
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
		dispUp(dispParse(cmd, rest))
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
