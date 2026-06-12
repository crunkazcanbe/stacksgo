package main

// logs.go — central log location + per-container log dumping.
//
// All stacks logs live in ONE folder (default <dataDir>/logs), configurable via
// the `logs_folder` setting (STACKS_LOG_DIR). Besides the engine logs
// (stacks_*.log), `stacks logdump` (and the menu, on open) writes one
// <container>.log per running container here so every service has its own log.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// logDir is the single logs folder. Override with the `logs_folder` setting
// (STACKS_LOG_DIR, env or stacks.conf); defaults to <dataDir>/logs. Created if
// missing so callers can write straight away.
func logDir() string {
	d := os.Getenv("STACKS_LOG_DIR")
	if d == "" {
		d = confValue("STACKS_LOG_DIR")
	}
	if d == "" {
		d = filepath.Join(dispDataDir(), "logs")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

// logPath joins a filename under logDir().
func logPath(name string) string { return filepath.Join(logDir(), name) }

// dumpContainerLogs writes `docker logs` (recent tail) for every RUNNING
// container to <logDir>/<name>.log. Best-effort: a container that errors is
// skipped. Returns how many were written.
func dumpContainerLogs() int {
	dir := logDir()
	n := 0
	for _, c := range containers(false) { // all=false → running only
		name := nameOf(c)
		if name == "" {
			continue
		}
		cmd := exec.Command("docker", "logs", "--tail", "2000", "--timestamps", name)
		cmd.Env = dockerEnv()
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) == 0 {
			continue
		}
		if os.WriteFile(filepath.Join(dir, name+".log"), out, 0o644) == nil {
			n++
		}
	}
	return n
}

// cmdLogdump is `stacks logdump`: refresh the per-container log files.
func cmdLogdump(_ []string) {
	banner()
	fmt.Printf("\nWriting per-container logs → %s\n", logDir())
	n := dumpContainerLogs()
	fmt.Printf("✅ wrote %d container log file(s).\n", n)
}
