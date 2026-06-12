package main

// menu_helpers.go — small utilities shared across the TUI files: command runners
// (Docker API closures, shell-outs, self-invocations), string/file helpers,
// inspect/rename/image-resolution mirrors of the Python menu's helper functions.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
