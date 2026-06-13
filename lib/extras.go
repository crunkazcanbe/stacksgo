package lib

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// ===== from reclaim.go =====

// reclaim.go — faithful Go port of stacks_reclaim.py.
//
// Reclaim disk by removing UNUSED tagged images, by size. Lists every local
// image largest-first, classifies each as in-use / unused / dangling. Most
// stacks here are on-demand (Sablier) and sit DOWN most of the time, so "no
// running container" does NOT mean unused — an image referenced by any compose
// file is protected (RECLAIM_PROTECT_STACK_IMAGES=1, default on). Removal uses
// `docker rmi` WITHOUT --force so an image still wired to a container can never
// be pulled out from under it; Docker refuses and we skip it.
//
// CLI:
//   stacks reclaim report  [--json] [--all] [--min-size MB]
//   stacks reclaim clean   [--auto] [--dangling] [--dry-run] [--min-size MB]
//                          [--force]            # allow rmi --force (untag only)

// ───────────────────────── config ─────────────────────────

// reclaimLoadConf mirrors load_conf(): legacy stacks.conf overlaid with the
// YAML-derived config. (configLoad already prefers stacks.yaml, falling back to
// stacks.conf, which subsumes both halves of the Python load_conf.)
func reclaimLoadConf() map[string]string {
	cfg := map[string]string{}
	conf := filepath.Join(configDir(), "stacks.conf")
	if f, err := os.Open(conf); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") && strings.Contains(line, "=") {
				k, v, _ := strings.Cut(line, "=")
				cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		f.Close()
	}
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// reclaimBool mirrors _bool().
func reclaimBool(cfg map[string]string, key, def string) bool {
	v, ok := cfg[key]
	if !ok {
		v = def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "", "false", "no", "off":
		return false
	}
	return true
}

// ───────────────────────── helpers ─────────────────────────

// reclaimHuman mirrors _human(): bytes → human (decimal, matching docker).
// negative sentinel (-1) renders as the em-dash used for None in the Python.
func reclaimHuman(n int64) string {
	if n < 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, u := range units {
		if f < 1000 || u == "TB" {
			if u == "B" || f >= 100 {
				return fmt.Sprintf("%.0f%s", f, u)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1000.0
	}
	return ""
}

var reclaimImageRe = regexp.MustCompile(`(?m)^\s*image:\s*([^\s#\n]+)`)

// stackReferencedImages mirrors stack_referenced_images(): set of image refs
// (repo:tag) named by image: in any compose file.
func stackReferencedImages() map[string]bool {
	refs := map[string]bool{}
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	for _, fpath := range matches {
		data, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		for _, m := range reclaimImageRe.FindAllStringSubmatch(string(data), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), `'"`)
			if img == "" {
				continue
			}
			refs[img] = true
			// bare repo (no tag in the last path segment) → :latest
			seg := img
			if i := strings.LastIndex(img, "/"); i >= 0 {
				seg = img[i+1:]
			}
			if !strings.Contains(seg, ":") {
				refs[img+":latest"] = true
			}
		}
	}
	return refs
}

// containerImageIDs mirrors container_image_ids(): full image IDs every
// container (running OR stopped) is built on, plus the image references those
// containers report (name form).
func containerImageIDs() (ids map[string]bool, names map[string]bool) {
	ids, names = map[string]bool{}, map[string]bool{}
	r := cli("ps", "-a", "--no-trunc", "--format", "{{.ID}}\t{{.Image}}")
	if r.exitCode != 0 {
		return ids, names
	}
	var cids []string
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cid, img, _ := strings.Cut(line, "\t")
		cids = append(cids, strings.TrimSpace(cid))
		if strings.TrimSpace(img) != "" {
			names[strings.TrimSpace(img)] = true
		}
	}
	// resolve each container to its real image ID (handles name drift / retags)
	for _, cid := range cids {
		ri := cli("inspect", "--format", "{{.Image}}", cid)
		if ri.exitCode == 0 && strings.TrimSpace(ri.stdout) != "" {
			ids[strings.TrimSpace(ri.stdout)] = true
		}
	}
	return ids, names
}

// reclaimImage mirrors a list_images() dict entry. JSON tags reproduce the
// exact key names the Python emitted under `--json`.
type reclaimImage struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	Repo     string `json:"repo"`
	Tag      string `json:"tag"`
	Size     int64  `json:"size"`
	SizeH    string `json:"size_h"`
	Dangling bool   `json:"dangling"`
	Status   string `json:"status"`
	Why      string `json:"why"`
}

// listReclaimImages mirrors list_images(): every local image.
func listReclaimImages() []*reclaimImage {
	r := cli("images", "--no-trunc", "--format",
		"{{.ID}}\t{{.Repository}}\t{{.Tag}}\t{{.Size}}")
	var out []*reclaimImage
	if r.exitCode != 0 {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		iid := strings.TrimSpace(parts[0])
		repo := strings.TrimSpace(parts[1])
		tag := strings.TrimSpace(parts[2])
		size := strings.TrimSpace(parts[3])
		dangling := repo == "<none>" || tag == "<none>"
		ref := "<none>"
		if !dangling {
			ref = repo + ":" + tag
		}
		out = append(out, &reclaimImage{
			ID: iid, Ref: ref, Repo: repo, Tag: tag,
			Size: parseReclaimSize(size), SizeH: size, Dangling: dangling,
		})
	}
	return out
}

var reclaimSizeRe = regexp.MustCompile(`(?i)^([\d.]+)\s*([KMGT]?B)$`)

// parseReclaimSize mirrors _parse_size(): '4.59GB' / '276MB' / '0B' → bytes.
func parseReclaimSize(s string) int64 {
	m := reclaimSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}
	mul, ok := mult[strings.ToUpper(m[2])]
	if !ok {
		mul = 1
	}
	return int64(val * mul)
}

// reclaimStatusSummary mirrors a per-status summary entry.
type reclaimStatusSummary struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// reclaimSummary mirrors the summary dict.
type reclaimSummary struct {
	Total            int                  `json:"total"`
	InUse            reclaimStatusSummary `json:"in-use"`
	Unused           reclaimStatusSummary `json:"unused"`
	Dangling         reclaimStatusSummary `json:"dangling"`
	ReclaimableBytes int64                `json:"reclaimable_bytes"`
}

// classifyReclaim mirrors classify(): rows sorted largest-first, each tagged
// with 'status' in {in-use, unused, dangling}, plus the summary.
func classifyReclaim(minSize int64) ([]*reclaimImage, reclaimSummary) {
	cfg := reclaimLoadConf()
	protectStacks := reclaimBool(cfg, "RECLAIM_PROTECT_STACK_IMAGES", "1")
	stackRefs := map[string]bool{}
	if protectStacks {
		stackRefs = stackReferencedImages()
	}
	usedIDs, usedNames := containerImageIDs()

	rows := []*reclaimImage{}
	for _, img := range listReclaimImages() {
		if img.Size < minSize {
			continue
		}
		switch {
		case img.Dangling:
			img.Status, img.Why = "dangling", "untagged leftover"
		case usedIDs[img.ID]:
			img.Status, img.Why = "in-use", "container"
		case usedNames[img.Ref] || usedNames[img.Repo]:
			img.Status, img.Why = "in-use", "container"
		case protectStacks && (stackRefs[img.Ref] || stackRefs[img.Repo]):
			img.Status, img.Why = "in-use", "stack file"
		default:
			img.Status, img.Why = "unused", "no container, no stack"
		}
		rows = append(rows, img)
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Size > rows[j].Size })

	summary := reclaimSummary{Total: len(rows)}
	for _, st := range []string{"in-use", "unused", "dangling"} {
		var cnt int
		var by int64
		for _, r := range rows {
			if r.Status == st {
				cnt++
				by += r.Size
			}
		}
		ss := reclaimStatusSummary{Count: cnt, Bytes: by}
		switch st {
		case "in-use":
			summary.InUse = ss
		case "unused":
			summary.Unused = ss
		case "dangling":
			summary.Dangling = ss
		}
	}
	summary.ReclaimableBytes = summary.Unused.Bytes + summary.Dangling.Bytes
	return rows, summary
}

var reclaimDFRe = regexp.MustCompile(`([\d.]+\s*[KMGT]?B)`)

// dockerDFReclaimable mirrors docker_df_reclaimable(): Docker's authoritative
// image reclaimable bytes (accounts for shared layers). Returns -1 for None.
func dockerDFReclaimable() int64 {
	r := cli("system", "df", "--format", "{{.Type}}\t{{.Reclaimable}}")
	if r.exitCode != 0 {
		return -1
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.HasPrefix(strings.ToLower(line), "images") {
			parts := strings.Split(line, "\t")
			m := reclaimDFRe.FindString(parts[len(parts)-1])
			if m != "" {
				return parseReclaimSize(m)
			}
		}
	}
	return -1
}

// reclaimError mirrors an (ref, msg) error tuple.
type reclaimError struct {
	Ref string
	Msg string
}

// removeReclaim mirrors remove(): rmi each row; returns (removed, freed, errors).
func removeReclaim(rows []*reclaimImage, force, dryRun bool) (int, int64, []reclaimError) {
	removed := 0
	var freed int64
	var errors []reclaimError
	for _, r := range rows {
		target := r.ID
		if !r.Dangling {
			if r.Ref != "<none>" {
				target = r.Ref
			}
		}
		if dryRun {
			removed++
			freed += r.Size
			continue
		}
		args := []string{"rmi"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, target)
		res := cli(args...)
		if res.exitCode == 0 {
			removed++
			freed += r.Size
		} else {
			msg := res.stderr
			if msg == "" {
				msg = res.stdout
			}
			lines := strings.Split(strings.TrimSpace(msg), "\n")
			last := lines[len(lines)-1]
			if len(last) > 120 {
				last = last[:120]
			}
			errors = append(errors, reclaimError{r.Ref, last})
		}
	}
	return removed, freed, errors
}

// ────────────────────────────── CLI ──────────────────────────────

// reclaimOpts mirrors the dict produced by _parse_flags().
type reclaimOpts struct {
	minSize int64
	flags   map[string]bool
}

// parseReclaimFlags mirrors _parse_flags().
func parseReclaimFlags(args []string) reclaimOpts {
	opts := reclaimOpts{minSize: 0, flags: map[string]bool{}}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--min-size" && i+1 < len(args) {
			if v, err := strconv.ParseFloat(args[i+1], 64); err == nil {
				opts.minSize = int64(v * 1e6)
			}
			i += 2
			continue
		}
		if strings.HasPrefix(a, "--") {
			opts.flags[a] = true
		}
		i++
	}
	return opts
}

// reclaimTrunc returns s truncated to at most n runes-as-bytes (Python slices on
// characters; refs/repos here are ASCII so byte slicing matches).
func reclaimTrunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// cmdReclaimReport mirrors cmd_report().
func cmdReclaimReport(args []string) {
	opts := parseReclaimFlags(args)
	rows, summ := classifyReclaim(opts.minSize)
	if opts.flags["--json"] {
		// Preserve Python's dict insertion order: summary first, then images.
		payload := struct {
			Summary reclaimSummary  `json:"summary"`
			Images  []*reclaimImage `json:"images"`
		}{Summary: summ, Images: rows}
		b, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(b))
		return
	}
	showAll := opts.flags["--all"]

	fmt.Printf("\n\033[1;35m🧹 Image disk reclaim\033[0m   (%d images scanned)\n\n", summ.Total)
	fmt.Printf("  \033[1;32min-use\033[0m    %3d   %9s\n", summ.InUse.Count, reclaimHuman(summ.InUse.Bytes))
	fmt.Printf("  \033[1;33munused\033[0m    %3d   %9s\n", summ.Unused.Count, reclaimHuman(summ.Unused.Bytes))
	fmt.Printf("  \033[1;31mdangling\033[0m  %3d   %9s\n", summ.Dangling.Count, reclaimHuman(summ.Dangling.Bytes))
	df := dockerDFReclaimable()
	var stackOnly int64
	for _, r := range rows {
		if r.Status == "in-use" && r.Why == "stack file" {
			stackOnly += r.Size
		}
	}
	fmt.Printf("\n  \033[1mReclaimable now (safe): ~%s\033[0m — unused + dangling, protects stacks\n",
		reclaimHuman(summ.ReclaimableBytes))
	fmt.Printf("  \033[1maggressive: ~%s\033[0m — also drops %s of idle stack images (re-pull on next up)\n",
		reclaimHuman(summ.ReclaimableBytes+stackOnly), reclaimHuman(stackOnly))
	fmt.Printf("  \033[2mdocker reports %s 'reclaimable' overall (counts every stopped-stack image)\033[0m\n\n",
		reclaimHuman(df))

	var cand []*reclaimImage
	for _, r := range rows {
		if r.Status == "unused" || r.Status == "dangling" {
			cand = append(cand, r)
		}
	}
	shown := cand
	if !showAll && len(cand) > 25 {
		shown = cand[:25]
	}
	if len(cand) == 0 {
		fmt.Print("  ✓ Nothing to reclaim — every image is in use.\n")
		return
	}
	fmt.Println("  \033[2mLargest reclaimable images:\033[0m")
	for _, r := range shown {
		col := "\033[1;33m"
		tag := "unused"
		if r.Dangling {
			col = "\033[1;31m"
			tag = "dangling"
		}
		fmt.Printf("    %s%9s\033[0m  %-8s %s\n", col, reclaimHuman(r.Size), tag, reclaimTrunc(r.Ref, 54))
	}
	if !showAll && len(cand) > len(shown) {
		fmt.Printf("    \033[2m… +%d more (use --all)\033[0m\n", len(cand)-len(shown))
	}
	fmt.Println("\n  Reclaim:  stacks reclaim clean            (interactive)")
	fmt.Println("            stacks reclaim clean --auto     (remove all unused+dangling)")
	fmt.Print("            stacks reclaim clean --dangling (only untagged leftovers)\n")
}

// cmdReclaimClean mirrors cmd_clean().
func cmdReclaimClean(args []string) {
	opts := parseReclaimFlags(args)
	rows, _ := classifyReclaim(opts.minSize)
	flags := opts.flags
	danglingOnly := flags["--dangling"]
	aggressive := flags["--aggressive"] || flags["--stacks-too"]
	everything := flags["--everything"] || flags["--nuke"] || flags["--all-images"]
	auto := flags["--auto"]
	dry := flags["--dry-run"]
	force := flags["--force"] || everything

	// ── pick the tier ──────────────────────────────────────
	var cand []*reclaimImage
	var mode string
	switch {
	case everything:
		// NUKE: every image, including ones a container uses (force rmi). Images
		// held by a RUNNING container can't actually be removed and are skipped.
		cand = append(cand, rows...)
		mode = "EVERYTHING (incl. in-use — force)"
	case aggressive:
		// Max space: delete everything NOT tied to a real container, including
		// idle stack images (they re-pull on next 'up').
		for _, r := range rows {
			if r.Why != "container" {
				cand = append(cand, r)
			}
		}
		mode = "aggressive (all but container-bound)"
	case danglingOnly:
		for _, r := range rows {
			if r.Status == "dangling" {
				cand = append(cand, r)
			}
		}
		mode = "dangling only"
	default:
		for _, r := range rows {
			if r.Status == "unused" || r.Status == "dangling" {
				cand = append(cand, r)
			}
		}
		mode = "safe (unused + dangling)"
	}
	if len(cand) == 0 {
		fmt.Println("✓ Nothing to reclaim.")
		return
	}

	var nominal int64
	for _, r := range cand {
		nominal += r.Size
	}
	fmt.Printf("\n\033[1mMode: %s\033[0m\n", mode)
	fmt.Printf("%d image(s) to remove — ~%s nominal.\n", len(cand), reclaimHuman(nominal))
	if everything {
		fmt.Println("\033[1;31m⚠ This removes images your running stacks use — they will re-pull on next start.\033[0m")
	} else if aggressive {
		fmt.Println("\033[1;33m⚠ Idle stack images will be deleted and re-pulled the next time those stacks start.\033[0m")
	}
	if dry {
		for _, r := range cand {
			lbl := r.Status
			if r.Status == "in-use" {
				lbl = "in-use:" + r.Why
			}
			fmt.Printf("  would remove  %9s  %-14s %s\n", reclaimHuman(r.Size), lbl, reclaimTrunc(r.Ref, 50))
		}
		fmt.Printf("\n(dry-run) would remove %d images.\n\n", len(cand))
		return
	}

	if !auto {
		limit := cand
		if len(cand) > 30 {
			limit = cand[:30]
		}
		for _, r := range limit {
			lbl := r.Status
			if r.Status == "in-use" {
				lbl = "in-use:" + r.Why
			}
			fmt.Printf("  %9s  %-14s %s\n", reclaimHuman(r.Size), lbl, reclaimTrunc(r.Ref, 50))
		}
		if len(cand) > 30 {
			fmt.Printf("  … +%d more\n", len(cand)-30)
		}
		prompt := fmt.Sprintf("\nRemove these %d images? [y/N]: ", len(cand))
		if everything {
			prompt = "Type DELETE to confirm: "
		}
		fmt.Print(prompt)
		reader := bufio.NewReader(os.Stdin)
		raw, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(raw)
		if (everything && ans != "DELETE") || (!everything && strings.ToLower(ans) != "y") {
			fmt.Println("Aborted.")
			return
		}
	}

	removed, freed, errors := removeReclaim(cand, force, false)
	fmt.Printf("\n✓ Removed %d/%d images (~%s nominal).\n", removed, len(cand), reclaimHuman(freed))
	if len(errors) > 0 {
		fmt.Printf("  %d could not be removed (still referenced — skipped):\n", len(errors))
		shown := errors
		if len(errors) > 8 {
			shown = errors[:8]
		}
		for _, e := range shown {
			fmt.Printf("    • %s: %s\n", e.Ref, e.Msg)
		}
		if len(errors) > 8 {
			fmt.Printf("    … +%d more\n", len(errors)-8)
		}
	}
	df := dockerDFReclaimable()
	if df >= 0 {
		fmt.Printf("  Docker still reports %s reclaimable.\n\n", reclaimHuman(df))
	}
}

// cmdReclaim mirrors main(): route the sub-command word.
func cmdReclaim(args []string) {
	cmd := "report"
	if len(args) > 0 {
		cmd = args[0]
	}
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	switch cmd {
	case "report":
		cmdReclaimReport(rest)
	case "clean":
		cmdReclaimClean(rest)
	default:
		fmt.Print(reclaimDoc)
	}
}

// reclaimDoc mirrors the module docstring printed for an unknown sub-command.
// Python's __doc__ begins with a newline (after the opening triple-quote) and
// ends with a newline (before the closing triple-quote); reproduce both so the
// output of `print(__doc__)` matches byte-for-byte.
const reclaimDoc = `
stacks reclaim — reclaim disk by removing UNUSED tagged images, by size.

Lists every local image largest-first, classifies each as:
  • in-use     — a container (running OR stopped) is built on it, OR it is
                 referenced by image: in a stack compose file
  • unused     — tagged, but no container uses it and no stack references it
  • dangling   — untagged <none>:<none> leftovers (always safe to remove)

CRITICAL SAFETY: most stacks here are on-demand (Sablier) and sit DOWN most of
the time, so "no running container" does NOT mean unused. An image referenced by
any compose file is protected (config RECLAIM_PROTECT_STACK_IMAGES=1, default on).
Removal uses ` + "`docker rmi`" + ` WITHOUT --force, so an image still wired to any
container can never be pulled out from under it — Docker refuses and we skip it.

CLI:
    stacks reclaim report  [--json] [--all] [--min-size MB]
    stacks reclaim clean   [--auto] [--dangling] [--dry-run] [--min-size MB]
                           [--force]            # allow rmi --force (untag only)
`

// ===== from updates.go =====

// updates.go — faithful Go port of stacks_updates.py.
//
// Image update tracker: checks if running container images have newer versions
// available, records a digest-change history, and can pull updates.

// ── module constants (mirror module-level globals) ──────────────────────────

const (
	updHistoryMax = 500
	updUA         = "Mozilla/5.0 (stacks-updater/1.0)"
	updTimeout    = 10 * time.Second
)

func updCacheFile() string   { return filepath.Join(configDir(), "update_cache.json") }
func updHistoryFile() string { return filepath.Join(configDir(), "update_history.json") }

// updEntry mirrors the per-image cache/result dict. Optional keys are tracked
// with separate presence flags where the Python relied on key presence.
type updEntry struct {
	Image        string   `json:"image"`
	Tag          string   `json:"tag"`
	Stacks       []string `json:"stacks"`
	LocalDigest  string   `json:"local_digest"`
	RemoteDigest string   `json:"remote_digest"`
	HasUpdate    bool     `json:"has_update"`
	Checked      int64    `json:"checked"`
	Error        string   `json:"error"`

	// hasRemote tracks whether "remote_digest" was a present key in the cached
	// JSON (the Python uses `"remote_digest" in cached` for the freshness check).
	hasRemote bool `json:"-"`
}

// updHistRecord mirrors a single history record.
type updHistRecord struct {
	TS       int64    `json:"ts"`
	Event    string   `json:"event"`
	Image    string   `json:"image"`
	Tag      string   `json:"tag"`
	Stacks   []string `json:"stacks"`
	Old      string   `json:"old"`
	New      string   `json:"new"`
	OldShort string   `json:"old_short"`
	NewShort string   `json:"new_short"`
}

// ── conf loading ────────────────────────────────────────────────────────────

// updLoadConf mirrors load_conf(): defaults, overlaid by stacks.conf, then by the
// YAML master (stacks.yaml wins).
func updLoadConf() map[string]string {
	cfg := map[string]string{
		"UPDATE_CHECK_ENABLED":      "1",
		"UPDATE_CHECK_INTERVAL":     "24",
		"UPDATE_CHECK_RUNNING_ONLY": "1",
		"UPDATE_AUTO_PULL":          "0",
		"UPDATE_SKIP_IMAGES":        "",
	}
	// raw stacks.conf overlay
	if f, err := os.Open(filepath.Join(configDir(), "stacks.conf")); err == nil {
		defer f.Close()
		data, _ := os.ReadFile(filepath.Join(configDir(), "stacks.conf"))
		for _, line := range strings.Split(string(data), "\n") {
			l := strings.TrimSpace(line)
			if strings.Contains(l, "=") && !strings.HasPrefix(l, "#") {
				k, v, _ := strings.Cut(l, "=")
				cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	// YAML master overlay (stacks.yaml wins)
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// ── cache / history persistence ─────────────────────────────────────────────

// updLoadCache mirrors load_cache(): returns {} on any failure.
func updLoadCache() map[string]updEntry {
	out := map[string]updEntry{}
	data, err := os.ReadFile(updCacheFile())
	if err != nil {
		return out
	}
	// First pass into generic maps so we can detect key presence (remote_digest).
	var raw map[string]map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		return map[string]updEntry{}
	}
	for k, m := range raw {
		var e updEntry
		// re-marshal/unmarshal for typed fields
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &e)
		}
		_, e.hasRemote = m["remote_digest"]
		out[k] = e
	}
	return out
}

// updSaveCache mirrors save_cache(): best-effort, indented JSON.
func updSaveCache(cache map[string]updEntry) {
	b, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(updCacheFile(), b, 0o644)
}

// updLoadHistory mirrors load_history(): returns [] on any failure.
func updLoadHistory() []updHistRecord {
	data, err := os.ReadFile(updHistoryFile())
	if err != nil {
		return []updHistRecord{}
	}
	var hist []updHistRecord
	if json.Unmarshal(data, &hist) != nil {
		return []updHistRecord{}
	}
	return hist
}

// updSaveHistory mirrors save_history(): keeps the last HISTORY_MAX records.
func updSaveHistory(hist []updHistRecord) {
	if len(hist) > updHistoryMax {
		hist = hist[len(hist)-updHistoryMax:]
	}
	b, err := json.MarshalIndent(hist, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(updHistoryFile(), b, 0o644)
}

// ── digest helpers ──────────────────────────────────────────────────────────

// updShort mirrors _short(): short form of a sha256:... digest for display.
func updShort(d string) string {
	if d == "" {
		return "—"
	}
	if strings.Contains(d, ":") {
		_, after, _ := strings.Cut(d, ":")
		d = after
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// updRecordHistory mirrors record_history(): append (newest to end).
func updRecordHistory(hist *[]updHistRecord, event, image, tag string, stacks []string, old, new string) {
	*hist = append(*hist, updHistRecord{
		TS:       time.Now().Unix(),
		Event:    event,
		Image:    image,
		Tag:      tag,
		Stacks:   stacks,
		Old:      old,
		New:      new,
		OldShort: updShort(old),
		NewShort: updShort(new),
	})
}

// updGetHistory mirrors get_history(): newest-first, optionally limited.
// limit <= 0 means no limit (Python's None).
func updGetHistory(limit int) []updHistRecord {
	hist := updLoadHistory()
	sort.SliceStable(hist, func(i, j int) bool {
		return hist[i].TS > hist[j].TS
	})
	if limit > 0 && limit < len(hist) {
		return hist[:limit]
	}
	return hist
}

// ── image discovery ─────────────────────────────────────────────────────────

var updImageRe = regexp.MustCompile(`image:\s*([^\s\n]+)`)

// updGetAllImages mirrors get_all_images(): {image: [stack,...]} from compose files.
func updGetAllImages() map[string][]string {
	images := map[string][]string{}
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	for _, fpath := range matches {
		stack := strings.TrimSuffix(filepath.Base(fpath), ".yml")
		content, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		for _, m := range updImageRe.FindAllStringSubmatch(string(content), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), "'\"")
			if img != "" && !strings.HasPrefix(img, "#") {
				images[img] = append(images[img], stack)
			}
		}
	}
	return images
}

// updParseImage mirrors parse_image(): returns registry, repo, tag.
func updParseImage(image string) (string, string, string) {
	tag := "latest"
	// ":" in the last path segment → split tag
	parts := strings.Split(image, "/")
	last := parts[len(parts)-1]
	if strings.Contains(last, ":") {
		i := strings.LastIndex(image, ":")
		tag = image[i+1:]
		image = image[:i]
	}

	if !strings.Contains(image, "/") {
		return "docker.io", "library/" + image, tag
	}
	segs := strings.Split(image, "/")
	if strings.Contains(segs[0], ".") || strings.Contains(segs[0], ":") {
		registry := segs[0]
		repo := strings.Join(segs[1:], "/")
		return registry, repo, tag
	}
	return "docker.io", image, tag
}

// ── registry checks ─────────────────────────────────────────────────────────

// updCheckResult mirrors the dict returned by check_dockerhub/check_ghcr.
type updCheckResult struct {
	digest  string
	checked int64
	err     string
}

func updHTTPClient() *http.Client { return &http.Client{Timeout: updTimeout} }

// updTruncErr mirrors str(e)[:50].
func updTruncErr(e error) string {
	s := e.Error()
	if len(s) > 50 {
		return s[:50]
	}
	return s
}

// updCheckDockerHub mirrors check_dockerhub(): get token, then manifest digest.
func updCheckDockerHub(repo, currentTag string) updCheckResult {
	client := updHTTPClient()

	// Get token
	authURL := fmt.Sprintf(
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repo)
	req, err := http.NewRequest("GET", authURL, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req.Header.Set("User-Agent", updUA)
	resp, err := client.Do(req)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	var tokBody struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(resp.Body)
	derr := dec.Decode(&tokBody)
	resp.Body.Close()
	if derr != nil {
		return updCheckResult{err: updTruncErr(derr), checked: time.Now().Unix()}
	}

	// Get manifest digest for current tag
	manURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s/manifests/%s", repo, currentTag)
	req2, err := http.NewRequest("GET", manURL, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req2.Header.Set("User-Agent", updUA)
	req2.Header.Set("Authorization", "Bearer "+tokBody.Token)
	req2.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp2, err := client.Do(req2)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	remoteDigest := resp2.Header.Get("Docker-Content-Digest")
	resp2.Body.Close()

	return updCheckResult{digest: remoteDigest, checked: time.Now().Unix()}
}

// updCheckGHCR mirrors check_ghcr(): GitHub Container Registry manifest digest.
func updCheckGHCR(repo, tag string) updCheckResult {
	client := updHTTPClient()
	url := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/%s", repo, tag)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	req.Header.Set("User-Agent", updUA)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := client.Do(req)
	if err != nil {
		return updCheckResult{err: updTruncErr(err), checked: time.Now().Unix()}
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	resp.Body.Close()
	return updCheckResult{digest: digest, checked: time.Now().Unix()}
}

// updGetLocalDigest mirrors get_local_digest(): local image digest via inspect.
// The Python shells out to `docker inspect`; we keep that behavior verbatim
// (with a 5s timeout) so the RepoDigests[0]@<digest> parsing is identical.
func updGetLocalDigest(image string) string {
	cmd := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", image)
	cmd.Env = dockerEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	digest := strings.TrimSpace(string(out))
	if strings.Contains(digest, "@") {
		_, after, _ := strings.Cut(digest, "@")
		return after
	}
	return ""
}

// ── update check ────────────────────────────────────────────────────────────

// updCheckUpdates mirrors check_updates(): check all images, returns result list.
func updCheckUpdates(force bool) []updEntry {
	cfg := updLoadConf()
	if cfg["UPDATE_CHECK_ENABLED"] != "1" {
		return []updEntry{}
	}

	cache := updLoadCache()
	hist := updLoadHistory()
	histDirty := false

	intervalHours := 24
	if v, err := strconv.Atoi(strings.TrimSpace(cfg["UPDATE_CHECK_INTERVAL"])); err == nil {
		intervalHours = v
	}
	interval := int64(intervalHours) * 3600

	skip := map[string]bool{}
	var skipList []string
	for _, s := range strings.Split(cfg["UPDATE_SKIP_IMAGES"], ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			skip[s] = true
			skipList = append(skipList, s)
		}
	}

	images := updGetAllImages()
	results := []updEntry{}

	// Deterministic iteration (Python dict preserves insertion order; we sort
	// for stable output — does not affect cache/history correctness).
	imgKeys := make([]string, 0, len(images))
	for k := range images {
		imgKeys = append(imgKeys, k)
	}
	sort.Strings(imgKeys)

	for _, image := range imgKeys {
		stacks := images[image]
		if skip[image] {
			continue
		}
		skipMatch := false
		for _, s := range skipList {
			if strings.Contains(image, s) {
				skipMatch = true
				break
			}
		}
		if skipMatch {
			continue
		}

		// Check cache age
		cached, hasCached := cache[image]
		age := time.Now().Unix() - cached.Checked
		if !force && age < interval && cached.hasRemote {
			out := cached
			out.Image = image
			out.Stacks = stacks
			results = append(results, out)
			continue
		}

		registry, repo, tag := updParseImage(image)
		localDigest := updGetLocalDigest(image)

		// Check remote
		var remote updCheckResult
		if registry == "docker.io" {
			remote = updCheckDockerHub(repo, tag)
		} else if strings.Contains(registry, "ghcr.io") {
			remote = updCheckGHCR(repo, tag)
		} else {
			remote = updCheckResult{err: "unsupported registry"}
		}

		remoteDigest := remote.digest
		hasUpdate := localDigest != "" && remoteDigest != "" && localDigest != remoteDigest

		entry := updEntry{
			Image:        image,
			Tag:          tag,
			Stacks:       stacks,
			LocalDigest:  localDigest,
			RemoteDigest: remoteDigest,
			HasUpdate:    hasUpdate,
			Checked:      time.Now().Unix(),
			Error:        remote.err,
			hasRemote:    true,
		}

		// ── record history on any digest change vs the last cached entry ──
		var prevRemote, prevLocal string
		if hasCached {
			prevRemote = cached.RemoteDigest
			prevLocal = cached.LocalDigest
		}
		if remoteDigest != "" && prevRemote != "" && remoteDigest != prevRemote {
			updRecordHistory(&hist, "published", image, tag, stacks, prevRemote, remoteDigest)
			histDirty = true
		}
		if localDigest != "" && prevLocal != "" && localDigest != prevLocal {
			updRecordHistory(&hist, "pulled", image, tag, stacks, prevLocal, localDigest)
			histDirty = true
		}

		cache[image] = entry
		results = append(results, entry)
	}

	updSaveCache(cache)
	if histDirty {
		updSaveHistory(hist)
	}
	return results
}

// ── pulling ─────────────────────────────────────────────────────────────────

// updPullUpdates mirrors pull_updates(): pull every image with an update available.
// NOTE: the Python integrates with stacks_image_history (record_from_docker_images)
// for rollback snapshots before/after pulling; that module is not ported here, so
// those snapshot calls are omitted (approximation). All other behavior is faithful.
func updPullUpdates() {
	cache := updLoadCache()
	var targets []updEntry
	// Iterate deterministically over cache.
	keys := make([]string, 0, len(cache))
	for k := range cache {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cache[k]
		if v.HasUpdate {
			targets = append(targets, v)
		}
	}
	if len(targets) == 0 {
		fmt.Println("No updates to pull.")
		return
	}
	fmt.Printf("Pulling %d image(s)...\n\n", len(targets))
	// (stacks_image_history snapshot of outgoing versions omitted — not ported)
	for _, r := range targets {
		img := r.Image
		fmt.Printf("⬇ docker pull %s\n", img)
		cmd := exec.Command("docker", "pull", img)
		cmd.Env = dockerEnv()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("  pull failed: %v\n", err)
		}
	}
	// (stacks_image_history snapshot of freshly-pulled versions omitted — not ported)
	fmt.Println("\nRe-checking digests to update history...")
	updCheckUpdates(true)
}

// ── history display ─────────────────────────────────────────────────────────

// updShowHistory mirrors show_history().
func updShowHistory(limit int) {
	hist := updGetHistory(limit)
	if len(hist) == 0 {
		fmt.Println("No update history yet.")
		return
	}
	fmt.Printf("\nUpdate history (newest %d):\n\n", len(hist))
	for _, r := range hist {
		when := time.Unix(r.TS, 0).Format("2006-01-02 15:04")
		ev := r.Event
		arrow := "⬇"
		if ev == "published" {
			arrow = "⬆"
		}
		oldS := r.OldShort
		if oldS == "" {
			oldS = "—"
		}
		newS := r.NewShort
		if newS == "" {
			newS = "—"
		}
		fmt.Printf("  %s  %s %-9s %-44s %s → %s\n", when, arrow, ev, r.Image, oldS, newS)
	}
}

// ── CLI entrypoint ──────────────────────────────────────────────────────────

// updMain mirrors the `if __name__ == "__main__"` block.
func updMain(argv []string) {
	if inList(argv, "--history") {
		updShowHistory(40)
		return
	}
	if inList(argv, "--pull") {
		updPullUpdates()
		return
	}
	force := inList(argv, "--force")
	fmt.Println("Checking for image updates...")
	results := updCheckUpdates(force)

	var updates, errors, ok []updEntry
	for _, r := range results {
		if r.HasUpdate {
			updates = append(updates, r)
		}
		if r.Error != "" {
			errors = append(errors, r)
		}
		if !r.HasUpdate && r.Error == "" {
			ok = append(ok, r)
		}
	}
	fmt.Printf("\n✔ Up to date:  %d\n", len(ok))
	fmt.Printf("⬆ Updates:     %d\n", len(updates))
	fmt.Printf("✘ Errors:      %d\n", len(errors))
	if len(updates) > 0 {
		fmt.Println("\nUpdates available:")
		for _, r := range updates {
			fmt.Printf("  %-50s stacks: %s\n", r.Image, strings.Join(r.Stacks, ", "))
		}
	}
	hist := updGetHistory(8)
	if len(hist) > 0 {
		fmt.Println("\nRecent changes:")
		for _, r := range hist {
			when := time.Unix(r.TS, 0).Format("01-02 15:04")
			arrow := "⬇"
			if r.Event == "published" {
				arrow = "⬆"
			}
			oldS := r.OldShort
			if oldS == "" {
				oldS = "—"
			}
			newS := r.NewShort
			if newS == "" {
				newS = "—"
			}
			fmt.Printf("  %s %s %-44s %s → %s\n", when, arrow, r.Image, oldS, newS)
		}
	}
}

// ===== from selfupdate.go =====

const (
	selfupdateInstallBin = "/usr/local/bin/stacks"
	selfupdateInstallLib = "/usr/local/lib"
)

var selfupdateCandidateRepos = []string{
	"~/stacks", "~/git/stacks", "~/src/stacks",
	"~/.local/share/stacks", "~/projects/stacks",
}

// selfupdateConfDir mirrors the module-level CONF_DIR computation. The Python
// hardcodes ~/.config/stacks via STACKS_CONFIG_DIR; we reuse configDir().
func selfupdateConfDir() string {
	return configDir()
}

func selfupdateBackupDir() string {
	return filepath.Join(selfupdateConfDir(), "selfupdate-backups")
}

// selfupdateLoadConf mirrors load_conf(): parse stacks.conf KEY=VALUE lines, then
// overlay the structured config (stacks_config.load()).
func selfupdateLoadConf() map[string]string {
	cfg := map[string]string{}
	conf := filepath.Join(selfupdateConfDir(), "stacks.conf")
	if data, err := os.ReadFile(conf); err == nil {
		for _, line := range splitLines(string(data)) {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			k, v, _ := strings.Cut(line, "=")
			cfg[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	// overlay structured config (equivalent of `import stacks_config; cfg.update(_sc.load())`)
	for k, v := range configLoad() {
		cfg[k] = v
	}
	return cfg
}

// selfupdateGitResult mirrors the (returncode, stdout, stderr) of subprocess.run.
type selfupdateGitResult struct {
	returncode int
	stdout     string
	stderr     string
}

// selfupdateGit mirrors _git(repo, *args, timeout=60).
func selfupdateGit(repo string, timeout time.Duration, args ...string) selfupdateGitResult {
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return selfupdateGitResult{returncode: 1, stdout: "", stderr: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		rc := 0
		if err != nil {
			rc = 1
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			}
		}
		return selfupdateGitResult{returncode: rc, stdout: outBuf.String(), stderr: errBuf.String()}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return selfupdateGitResult{returncode: 1, stdout: "", stderr: "timeout"}
	}
}

// selfupdateIsStacksRepo mirrors _is_stacks_repo(path).
func selfupdateIsStacksRepo(path string) bool {
	if st, err := os.Stat(filepath.Join(path, ".git")); err != nil || !st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(path, "install.sh")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(path, "lib", "stacks_menu.py")); err != nil || st.IsDir() {
		return false
	}
	return true
}

// selfupdateRepoDir mirrors repo_dir(): locate the stacks git clone.
// (Named uniquely to avoid colliding with the project's repoDir() in config.go.)
func selfupdateRepoDir() string {
	cfg := selfupdateLoadConf()
	if p := strings.TrimSpace(cfg["STACKS_REPO_DIR"]); p != "" {
		p = expandUser(p)
		if selfupdateIsStacksRepo(p) {
			return p
		}
	}
	for _, cand := range selfupdateCandidateRepos {
		cand = expandUser(cand)
		if selfupdateIsStacksRepo(cand) {
			return cand
		}
	}
	return ""
}

// selfupdateBranchOf mirrors branch_of(repo).
func selfupdateBranchOf(repo string) string {
	cfg := selfupdateLoadConf()
	if b := strings.TrimSpace(cfg["STACKS_UPDATE_BRANCH"]); b != "" {
		return b
	}
	r := selfupdateGit(repo, 60*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if r.returncode == 0 && strings.TrimSpace(r.stdout) != "" {
		return strings.TrimSpace(r.stdout)
	}
	return "master"
}

// selfupdateSame mirrors _same(a, b).
func selfupdateSame(a, b string) bool {
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return string(da) == string(db)
}

// selfupdateInstalledDirty mirrors _installed_dirty(repo). Returns (dirty, files).
func selfupdateInstalledDirty(repo string) (bool, []string) {
	var diffs []string
	rb := filepath.Join(repo, "bin", "stacks")
	if selfupdateIsFile(rb) && selfupdateIsFile(selfupdateInstallBin) {
		if !selfupdateSame(rb, selfupdateInstallBin) {
			diffs = append(diffs, "bin/stacks")
		}
	}
	matches, _ := filepath.Glob(filepath.Join(repo, "lib", "*.py"))
	for _, f := range matches {
		inst := filepath.Join(selfupdateInstallLib, filepath.Base(f))
		if selfupdateIsFile(inst) && !selfupdateSame(f, inst) {
			diffs = append(diffs, "lib/"+filepath.Base(f))
		}
	}
	return len(diffs) > 0, diffs
}

func selfupdateIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// selfupdateStatus mirrors status(): fetch origin and report.
func selfupdateStatus() map[string]interface{} {
	repo := selfupdateRepoDir()
	if repo == "" {
		return map[string]interface{}{
			"error": "No stacks git clone found. Set STACKS_REPO_DIR in stacks.conf " +
				"to the folder you installed from (the one with install.sh).",
		}
	}
	branch := selfupdateBranchOf(repo)
	f := selfupdateGit(repo, 45*time.Second, "fetch", "origin", branch)
	fetchErr := ""
	if f.returncode != 0 {
		s := f.stderr
		if s == "" {
			s = f.stdout
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		fetchErr = s
	}
	cur := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-parse", "--short", "HEAD").stdout)
	latest := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-parse", "--short", "origin/"+branch).stdout)
	behind := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-list", "--count", "HEAD..origin/"+branch).stdout)
	if behind == "" {
		behind = "0"
	}
	ahead := strings.TrimSpace(selfupdateGit(repo, 60*time.Second, "rev-list", "--count", "origin/"+branch+"..HEAD").stdout)
	if ahead == "" {
		ahead = "0"
	}
	log := selfupdateGit(repo, 60*time.Second, "log", "--oneline", "--no-decorate", "HEAD..origin/"+branch)
	changelog := []string{}
	if log.returncode == 0 {
		for _, l := range strings.Split(strings.TrimSpace(log.stdout), "\n") {
			if strings.TrimSpace(l) != "" {
				changelog = append(changelog, l)
			}
		}
	}
	dirty, dirtyFiles := selfupdateInstalledDirty(repo)

	behindN, err := strconv.Atoi(behind)
	if err != nil {
		behindN = 0
	}
	aheadN := 0
	if selfupdateIsDigit(ahead) {
		aheadN, _ = strconv.Atoi(ahead)
	}
	if dirtyFiles == nil {
		dirtyFiles = []string{}
	}
	return map[string]interface{}{
		"repo": repo, "branch": branch,
		"current": cur, "latest": latest,
		"behind": behindN, "ahead": aheadN,
		"changelog":       changelog,
		"installed_dirty": dirty, "dirty_files": dirtyFiles,
		"fetch_error": fetchErr,
		"up_to_date":  behindN == 0 && fetchErr == "",
	}
}

// selfupdateIsDigit mirrors Python str.isdigit() for the ahead-count check.
func selfupdateIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// selfupdateBackupInstalled mirrors _backup_installed().
func selfupdateBackupInstalled() string {
	dst := filepath.Join(selfupdateBackupDir(), time.Now().Format("20060102-150405"))
	_ = os.MkdirAll(filepath.Join(dst, "lib"), 0o755)
	func() {
		defer func() { recover() }()
		if selfupdateIsFile(selfupdateInstallBin) {
			_ = selfupdateCopy2(selfupdateInstallBin, filepath.Join(dst, "stacks"))
		}
		matches, _ := filepath.Glob(filepath.Join(selfupdateInstallLib, "stacks_*.py"))
		for _, f := range matches {
			_ = selfupdateCopy2(f, filepath.Join(dst, "lib", filepath.Base(f)))
		}
	}()
	return dst
}

// selfupdateCopy2 mirrors shutil.copy2 (copy contents + mode).
func selfupdateCopy2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(src); err == nil {
		mode = st.Mode()
	}
	return os.WriteFile(dst, data, mode)
}

// selfupdateApply mirrors apply(allow_overwrite_local=False).
// Returns (ok, msg, log).
func selfupdateApply(allowOverwriteLocal bool) (bool, string, []string) {
	st := selfupdateStatus()
	if e, ok := st["error"].(string); ok && e != "" {
		return false, e, []string{}
	}
	repo := st["repo"].(string)
	branch := st["branch"].(string)
	var log []string
	behind := st["behind"].(int)
	fetchErr := st["fetch_error"].(string)
	installedDirty := st["installed_dirty"].(bool)
	dirtyFiles := st["dirty_files"].([]string)

	if behind == 0 && fetchErr == "" {
		return true, "Already up to date.", []string{}
	}
	if installedDirty && !allowOverwriteLocal {
		head := dirtyFiles
		if len(head) > 4 {
			head = head[:4]
		}
		return false, fmt.Sprintf(
			"Installed copy has %d local change(s) not in git "+
				"(e.g. %s). These would be overwritten. "+
				"Re-run with apply --force to proceed anyway (a backup is always made).",
			len(dirtyFiles), strings.Join(head, ", ")), dirtyFiles
	}
	bak := selfupdateBackupInstalled()
	log = append(log, fmt.Sprintf("backed up installed files → %s", bak))
	pull := selfupdateGit(repo, 120*time.Second, "pull", "--ff-only", "origin", branch)
	log = append(log, strings.TrimSpace(pull.stdout))
	if pull.returncode != 0 {
		s := pull.stderr
		if s == "" {
			s = pull.stdout
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		return false, "git pull failed: " + s, log
	}
	inst := exec.Command("sudo", "bash", filepath.Join(repo, "install.sh"))
	var instOut, instErr strings.Builder
	inst.Stdout = &instOut
	inst.Stderr = &instErr
	instRC := 0
	if err := selfupdateRunWithTimeout(inst, 180*time.Second); err != nil {
		instRC = 1
		if ee, ok := err.(*exec.ExitError); ok {
			instRC = ee.ExitCode()
		}
	}
	log = append(log, strings.TrimSpace(instOut.String()))
	if instRC != 0 {
		s := instErr.String()
		if s == "" {
			s = instOut.String()
		}
		s = strings.TrimSpace(s)
		if len(s) > 160 {
			s = s[:160]
		}
		return false, "install.sh failed: " + s, log
	}
	return true, fmt.Sprintf("Updated to %s (%d commit(s)). Restart the menu to load it.",
		st["latest"].(string), behind), log
}

// selfupdateRunWithTimeout runs an already-configured exec.Cmd with a timeout.
func selfupdateRunWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timeout")
	}
}

// ────────────────────────────── CLI ──────────────────────────────

const selfupdateDoc = `
stacks_selfupdate.py — check GitHub for updates to the stacks program (which
includes the TUI menu, stacks_menu.py) and apply them on request.

Model: the program is deployed from a git clone via its install.sh
(` + "`cp bin/stacks /usr/local/bin`" + `, ` + "`cp lib/*.py /usr/local/lib`" + `). "Update" =
git fetch the clone, and if origin is ahead, ` + "`git pull --ff-only`" + ` then re-run
install.sh. The installed files are ALWAYS backed up first (reversible), and if
the installed copy has local edits not in the clone we warn before overwriting.

CLI:
    stacks_selfupdate.py check   [--json]   # fetch + report (default)
    stacks_selfupdate.py apply              # pull + backup + install
    stacks_selfupdate.py where              # print the detected repo dir
`

// selfupdateMain mirrors main(): the CLI dispatcher.
func selfupdateMain(args []string) {
	cmd := "check"
	if len(args) > 0 {
		cmd = args[0]
	}

	if cmd == "where" {
		r := selfupdateRepoDir()
		if r == "" {
			fmt.Println("(no stacks git clone found)")
		} else {
			fmt.Println(r)
		}
		return
	}

	if cmd == "check" || cmd == "status" {
		st := selfupdateStatus()
		if e, ok := st["error"].(string); ok && e != "" {
			fmt.Println("⚠ " + e)
			os.Exit(2)
		}
		if inList(args, "--json") {
			b, _ := json.MarshalIndent(selfupdateOrderedStatus(st), "", "  ")
			fmt.Println(string(b))
			return
		}
		fmt.Printf("\n\033[1;35m⬆ stacks self-update\033[0m   repo: %s  (%s)\n",
			st["repo"].(string), st["branch"].(string))
		fmt.Printf("  installed commit: %s   latest on GitHub: %s\n",
			st["current"].(string), st["latest"].(string))
		if fe := st["fetch_error"].(string); fe != "" {
			fmt.Printf("  \033[1;33m⚠ couldn't reach GitHub: %s\033[0m\n", fe)
		}
		if st["up_to_date"].(bool) {
			fmt.Print("  \033[1;32m✓ Up to date.\033[0m\n")
		} else {
			behind := st["behind"].(int)
			changelog := st["changelog"].([]string)
			fmt.Printf("  \033[1;33m⬆ %d update(s) available:\033[0m\n", behind)
			limit := len(changelog)
			if limit > 15 {
				limit = 15
			}
			for _, line := range changelog[:limit] {
				fmt.Printf("      %s\n", line)
			}
			if len(changelog) > 15 {
				fmt.Printf("      … +%d more\n", len(changelog)-15)
			}
			if st["installed_dirty"].(bool) {
				fmt.Printf("  \033[1;31m⚠ installed copy has local changes (%d files) "+
					"that update would overwrite — use 'apply --force'.\033[0m\n",
					len(st["dirty_files"].([]string)))
			}
			fmt.Print("\n  Update:  stacks update apply\n")
		}
		return
	}

	if cmd == "apply" {
		force := inList(args, "--force") || inList(args, "-f")
		fmt.Println("Updating stacks from GitHub…")
		ok, msg, log := selfupdateApply(force)
		for _, l := range log {
			if l != "" {
				fmt.Println("  " + l)
			}
		}
		prefix := "✗ "
		if ok {
			prefix = "✓ "
		}
		fmt.Println(prefix + msg)
		if ok {
			os.Exit(0)
		}
		os.Exit(1)
	}

	fmt.Print(selfupdateDoc)
}

// selfupdateOrderedStatus mirrors json.dumps(st, indent=2) which preserves the
// dict insertion order. Go maps are unordered, so we project into an ordered
// representation matching the Python key order.
func selfupdateOrderedStatus(st map[string]interface{}) json.Marshaler {
	return selfupdateOrderedMap{st: st, keys: []string{
		"repo", "branch", "current", "latest", "behind", "ahead",
		"changelog", "installed_dirty", "dirty_files", "fetch_error", "up_to_date",
	}}
}

type selfupdateOrderedMap struct {
	st   map[string]interface{}
	keys []string
}

func (m selfupdateOrderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	// include only keys present, preserving order; any extra keys (e.g. error) appended.
	used := map[string]bool{}
	emit := func(k string) {
		v, ok := m.st[k]
		if !ok {
			return
		}
		used[k] = true
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(v)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	for _, k := range m.keys {
		emit(k)
	}
	// append any remaining keys deterministically
	var extra []string
	for k := range m.st {
		if !used[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		emit(k)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// ===== from imagehistory.go =====

// ── doc string (printed for no-args / unknown subcommands) ────────────────────

const imageHistoryDoc = `
stacks images — per-image version history + rollback.

Keeps a SQLite history of every distinct image digest we've seen for each
` + "`repo:tag`" + ` referenced by the stacks, so you can roll a container back to an
older version. Config (stacks.conf):
    IMAGE_HISTORY_ENABLED=1     # record snapshots
    IMAGE_HISTORY_KEEP=10       # versions kept per image (oldest pruned)

CLI:
    stacks images snapshot               # record current digest of every image
    stacks images list <image>           # show recorded versions, newest first
    stacks images rollback <image> <digest>   # pin+retag+(caller recreates)
    stacks images prune                  # enforce keep-count on all images
`

// imageHistoryDBPath mirrors DB_PATH: configDir()/image_history.db.
func imageHistoryDBPath() string {
	return filepath.Join(configDir(), "image_history.db")
}

// ── config helpers ────────────────────────────────────────────────────────────

// imageHistoryConf mirrors load_conf(): a few keys from stacks.conf.
// (The project's confValue already resolves the per-user stacks.conf, which is
// the established Go equivalent for the conf+overlay used elsewhere.)
func imageHistoryConf(key, def string) string {
	v := confValue(key)
	if v == "" {
		return def
	}
	return v
}

// keepCount mirrors keep_count().
func imageHistoryKeepCount() int {
	n, err := strconv.Atoi(strings.TrimSpace(imageHistoryConf("IMAGE_HISTORY_KEEP", "10")))
	if err != nil {
		return 10
	}
	if n < 1 {
		return 1
	}
	return n
}

// enabled mirrors enabled().
func imageHistoryEnabled() bool {
	v := strings.TrimSpace(imageHistoryConf("IMAGE_HISTORY_ENABLED", "1"))
	switch v {
	case "0", "", "false", "no":
		return false
	}
	return true
}

// ── persistent store (faithful stand-in for the SQLite `versions` table) ──────

// imageHistoryRow mirrors a row of the `versions` table.
//
//	PRIMARY KEY (image, digest)
type imageHistoryRow struct {
	Image     string `json:"image"`
	Digest    string `json:"digest"`
	ImageID   string `json:"image_id"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
}

// imageHistoryDB is the in-memory view of the store, loaded/saved as a whole.
type imageHistoryDB struct {
	rows []imageHistoryRow
}

// imageHistoryOpenDB mirrors _db(): ensure dir exists, load (or create) the store.
func imageHistoryOpenDB() *imageHistoryDB {
	_ = os.MkdirAll(configDir(), 0o755)
	db := &imageHistoryDB{}
	data, err := os.ReadFile(imageHistoryDBPath())
	if err == nil && len(data) > 0 {
		var rows []imageHistoryRow
		if json.Unmarshal(data, &rows) == nil {
			db.rows = rows
		}
	}
	return db
}

// commit persists the store back to disk (mirrors con.commit()).
func (db *imageHistoryDB) commit() {
	data, err := json.Marshal(db.rows)
	if err != nil {
		return
	}
	_ = os.WriteFile(imageHistoryDBPath(), data, 0o644)
}

// find returns the index of (image,digest) or -1.
func (db *imageHistoryDB) find(image, digest string) int {
	for i, r := range db.rows {
		if r.Image == image && r.Digest == digest {
			return i
		}
	}
	return -1
}

// ── digest helpers ────────────────────────────────────────────────────────────

// short mirrors short(): short form of a sha256:... digest for display.
func imageHistoryShort(d string) string {
	if d == "" {
		return "—"
	}
	parts := strings.Split(d, ":")
	d = parts[len(parts)-1]
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// _inspect mirrors _inspect(): return (digest, image_id) for a locally-present
// image, or ("", "").
func imageHistoryInspect(image string) (string, string) {
	r := cli("inspect", "--format", "{{index .RepoDigests 0}}|{{.Id}}", image)
	if r.exitCode == 0 && strings.TrimSpace(r.stdout) != "" {
		rd, iid, _ := strings.Cut(strings.TrimSpace(r.stdout), "|")
		digest := ""
		if i := strings.Index(rd, "@"); i >= 0 {
			digest = rd[i+1:]
		}
		return strings.TrimSpace(digest), strings.TrimSpace(iid)
	}
	return "", ""
}

// ── record / prune ────────────────────────────────────────────────────────────

// record mirrors record(): upsert the current (or supplied) version for `image`,
// then prune to keep-count. Returns the digest recorded, or "" if nothing.
func imageHistoryRecord(image, digest, imageID string) string {
	if digest == "" {
		digest, imageID = imageHistoryInspect(image)
	}
	if digest == "" {
		return ""
	}
	now := time.Now().Unix()
	db := imageHistoryOpenDB()
	if idx := db.find(image, digest); idx >= 0 {
		db.rows[idx].LastSeen = now
		if imageID != "" { // COALESCE(?, image_id)
			db.rows[idx].ImageID = imageID
		}
	} else {
		db.rows = append(db.rows, imageHistoryRow{
			Image: image, Digest: digest, ImageID: imageID,
			FirstSeen: now, LastSeen: now,
		})
	}
	db.commit()
	imageHistoryPrune(db, image, imageHistoryKeepCount())
	return digest
}

// _prune mirrors _prune(): keep the `keep` newest (by last_seen DESC) per image.
func imageHistoryPrune(db *imageHistoryDB, image string, keep int) {
	// Gather indices for this image, ordered by last_seen DESC.
	var idxs []int
	for i, r := range db.rows {
		if r.Image == image {
			idxs = append(idxs, i)
		}
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		return db.rows[idxs[a]].LastSeen > db.rows[idxs[b]].LastSeen
	})
	if len(idxs) <= keep {
		return
	}
	// Digests of the extras (rows[keep:]).
	extra := map[string]bool{}
	for _, i := range idxs[keep:] {
		extra[db.rows[i].Digest] = true
	}
	kept := db.rows[:0]
	for _, r := range db.rows {
		if r.Image == image && extra[r.Digest] {
			continue
		}
		kept = append(kept, r)
	}
	db.rows = kept
	db.commit()
}

// ── history (list) ────────────────────────────────────────────────────────────

// imageHistoryEntry mirrors a dict returned by history().
type imageHistoryEntry struct {
	Image     string
	Digest    string
	ImageID   string
	FirstSeen int64
	LastSeen  int64
	Short     string
	Current   bool
}

// history mirrors history(): recorded versions for an image, newest-first.
func imageHistoryList(image string) []imageHistoryEntry {
	db := imageHistoryOpenDB()
	var rows []imageHistoryRow
	for _, r := range db.rows {
		if r.Image == image {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].LastSeen > rows[b].LastSeen })

	curDigest, _ := imageHistoryInspect(image)
	out := make([]imageHistoryEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, imageHistoryEntry{
			Image: image, Digest: r.Digest, ImageID: r.ImageID,
			FirstSeen: r.FirstSeen, LastSeen: r.LastSeen,
			Short: imageHistoryShort(r.Digest), Current: r.Digest == curDigest,
		})
	}
	return out
}

// _repo_no_tag mirrors _repo_no_tag(): strip a trailing :tag from the last path
// segment so we can pin @digest.
func imageHistoryRepoNoTag(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return image
	}
	last := image[i+1:]
	if !strings.Contains(last, "/") {
		return image[:i]
	}
	return image
}

// ── image discovery (faithful port of stacks_updates.get_all_images) ──────────

var imageHistoryImageRe = regexp.MustCompile(`image:\s*([^\s\n]+)`)

// imageHistoryAllImages mirrors stacks_updates.get_all_images(): scan the
// stacks' *.yml files for `image:` references → {image: [stacks...]}.
func imageHistoryAllImages() map[string][]string {
	images := map[string][]string{}
	entries, err := os.ReadDir(stacksDir())
	if err != nil {
		return images
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yml") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		stack := strings.TrimSuffix(name, ".yml")
		content, err := os.ReadFile(filepath.Join(stacksDir(), name))
		if err != nil {
			continue
		}
		for _, m := range imageHistoryImageRe.FindAllStringSubmatch(string(content), -1) {
			img := strings.Trim(strings.TrimSpace(m[1]), "'\"")
			if img != "" && !strings.HasPrefix(img, "#") {
				images[img] = append(images[img], stack)
			}
		}
	}
	return images
}

// ── snapshot (record_all / record_from_docker_images) ─────────────────────────

// record_all mirrors record_all(): snapshot every referenced image present
// locally (per-image inspect, thorough). Returns (recorded, total).
func imageHistoryRecordAll() (int, int) {
	images := imageHistoryAllImages()
	rec := 0
	for image := range images {
		if imageHistoryRecord(image, "", "") != "" {
			rec++
		}
	}
	return rec, len(images)
}

// record_from_docker_images mirrors record_from_docker_images(): fast snapshot
// via one `docker images --digests` call. Returns (recorded, scanned).
func imageHistoryRecordFromDockerImages(onlyStackImages bool) (int, int) {
	r := cli("images", "--digests", "--format",
		"{{.Repository}}:{{.Tag}}\t{{.Digest}}\t{{.ID}}")
	if r.exitCode != 0 {
		return 0, 0
	}
	var wanted map[string]bool
	if onlyStackImages {
		wanted = map[string]bool{}
		for im := range imageHistoryAllImages() {
			wanted[im] = true
		}
	}
	now := time.Now().Unix()
	rec, scanned := 0, 0
	touched := map[string]bool{}
	db := imageHistoryOpenDB()
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if strings.TrimSpace(line) == "" || !strings.Contains(line, "\t") {
			continue
		}
		parts := strings.Split(line, "\t")
		image := strings.TrimSpace(parts[0])
		digest := ""
		if len(parts) > 1 {
			digest = strings.TrimSpace(parts[1])
		}
		iid := ""
		if len(parts) > 2 {
			iid = strings.TrimSpace(parts[2])
		}
		if strings.HasSuffix(image, ":<none>") || digest == "" || digest == "<none>" {
			continue
		}
		if i := strings.Index(digest, "@"); i >= 0 {
			digest = digest[i+1:]
		}
		if wanted != nil && !wanted[image] {
			continue
		}
		scanned++
		if idx := db.find(image, digest); idx >= 0 {
			db.rows[idx].LastSeen = now
			if iid != "" { // COALESCE
				db.rows[idx].ImageID = iid
			}
		} else {
			db.rows = append(db.rows, imageHistoryRow{
				Image: image, Digest: digest, ImageID: iid,
				FirstSeen: now, LastSeen: now,
			})
		}
		touched[image] = true
		rec++
	}
	db.commit()
	keep := imageHistoryKeepCount()
	for im := range touched {
		imageHistoryPrune(db, im, keep)
	}
	return rec, scanned
}

// ── rollback ──────────────────────────────────────────────────────────────────

// rollback mirrors rollback(): pull `image` by `digest` and retag repo:tag to it.
// Caller recreates the container afterward. Returns (ok, message).
func imageHistoryRollback(image, digest string) (bool, string) {
	repo := imageHistoryRepoNoTag(image)
	ref := fmt.Sprintf("%s@%s", repo, digest)
	// 1) make sure the old version is present (fast if layers are cached)
	p := cli("pull", ref)
	if p.exitCode != 0 {
		return false, fmt.Sprintf("pull %s failed: %s", ref, imageHistoryTrim140(p.stderr, p.stdout))
	}
	// 2) point repo:tag at the pinned digest
	t := cli("tag", ref, image)
	if t.exitCode != 0 {
		return false, fmt.Sprintf("tag failed: %s", imageHistoryTrim140(t.stderr, t.stdout))
	}
	imageHistoryRecord(image, digest, "")
	return true, fmt.Sprintf("%s -> %s (recreate to apply)", image, imageHistoryShort(digest))
}

// imageHistoryTrim140 mirrors `(stderr or stdout).strip()[:140]`.
func imageHistoryTrim140(stderr, stdout string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = strings.TrimSpace(stdout)
	}
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}

// ── CLI entry point (mirrors main()) ──────────────────────────────────────────

// imageHistoryMain mirrors main(): args are the subcommand + params (no argv[0]).
func imageHistoryMain(args []string) {
	if len(args) == 0 {
		fmt.Print(imageHistoryDoc)
		return
	}
	cmd := args[0]
	switch {
	case cmd == "snapshot":
		if !imageHistoryEnabled() {
			fmt.Println("image history disabled (IMAGE_HISTORY_ENABLED=0)")
			return
		}
		thorough := inList(args, "--thorough")
		var rec, tot int
		if thorough {
			rec, tot = imageHistoryRecordAll()
		} else {
			rec, tot = imageHistoryRecordFromDockerImages(true)
		}
		fmt.Printf("recorded %d/%d images into %s (keep %d each)\n",
			rec, tot, imageHistoryDBPath(), imageHistoryKeepCount())

	case cmd == "list" && len(args) >= 2:
		for _, v := range imageHistoryList(args[1]) {
			mark := ""
			if v.Current {
				mark = " (current)"
			}
			when := time.Unix(v.LastSeen, 0).Format("2006-01-02 15:04")
			fmt.Printf("  %s  last_seen %s%s\n", v.Short, when, mark)
		}

	case cmd == "rollback" && len(args) >= 3:
		ok, msg := imageHistoryRollback(args[1], args[2])
		prefix := "FAIL: "
		if ok {
			prefix = "OK: "
		}
		fmt.Println(prefix + msg)
		if ok {
			os.Exit(0)
		}
		os.Exit(1)

	case cmd == "prune":
		db := imageHistoryOpenDB()
		seen := map[string]bool{}
		var imgs []string
		for _, r := range db.rows {
			if !seen[r.Image] {
				seen[r.Image] = true
				imgs = append(imgs, r.Image)
			}
		}
		keep := imageHistoryKeepCount()
		for _, im := range imgs {
			imageHistoryPrune(db, im, keep)
		}
		fmt.Printf("pruned to keep %d per image (%d images)\n", keep, len(imgs))

	default:
		fmt.Print(imageHistoryDoc)
	}
}

// ===== from volclean.go =====

// volclean.go — faithful Go port of stacks_volclean.py.
//
// Strip UNUSED top-level named-volume declarations.
//
// When a service is moved to a bind mount, its old top-level `volumes:` entry is
// left orphaned — declared but referenced by nothing — yet Compose still tries to
// create it. This finds declarations no service references and removes them,
// backing up every file first.
//
// SAFETY: a volume is only an orphan if BOTH (a) YAML analysis shows no service
// mounts it, AND (b) its name does not appear as a mount source anywhere in the
// file's text. A volume that is used is never removed. Removal is textual (only the
// orphan's block is deleted; the rest of the file is untouched), and if the whole
// top-level `volumes:` section becomes empty its header is removed too.
//
// CLI:
//     stacks volclean report [--json]
//     stacks volclean clean [--auto] [stack ...]
//     stacks volclean ensure [stack ...]

// volcleanBackupDir mirrors the Python BACKUP_DIR (~/.config/stacks/volclean-backups).
func volcleanBackupDir() string {
	return filepath.Join(configDir(), "volclean-backups")
}

const volcleanDoc = `
stacks_volclean.py — strip UNUSED top-level named-volume declarations.

When a service is moved to a bind mount, its old top-level ` + "`volumes:`" + ` entry is
left orphaned — declared but referenced by nothing — yet Compose still tries to
create it. This finds declarations no service references and removes them, backing
up every file first.

SAFETY: a volume is only an orphan if BOTH (a) YAML analysis shows no service
mounts it, AND (b) its name does not appear as a mount source anywhere in the
file's text. A volume that is used is never removed. Removal is textual (only the
orphan's block is deleted; the rest of the file is untouched), and if the whole
top-level ` + "`volumes:`" + ` section becomes empty its header is removed too.

CLI:
    stacks_volclean.py report [--json]
    stacks_volclean.py clean [--auto] [stack ...]
`

// volcleanIsNamed: a volume mount source is a NAMED volume (not a bind mount/path).
func volcleanIsNamed(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	for _, p := range []string{"/", ".", "~", "$"} {
		if strings.HasPrefix(src, p) {
			return false
		}
	}
	return true
}

// volcleanReferencedAsMount: textual safety net — does `name` appear as a mount
// SOURCE in a service?
func volcleanReferencedAsMount(text, name string) bool {
	n := regexp.QuoteMeta(name)
	pats := []string{
		`-\s*["']?` + n + `:`,                 // short form:  - name:/path
		`source:\s*["']?` + n + `["']?(\s|$)`, // long form:  source: name
	}
	for _, p := range pats {
		re := regexp.MustCompile("(?m)" + p)
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// volcleanReadFile reads a file replacing invalid bytes loosely (Python used
// errors="replace"). Go strings tolerate arbitrary bytes, so a plain read is fine.
func volcleanReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// volcleanAnalyze returns (declared, used, orphans-sorted) for one compose file.
func volcleanAnalyze(path string) (declared map[string]bool, used map[string]bool, orphans []string) {
	declared = map[string]bool{}
	used = map[string]bool{}
	orphans = []string{}

	raw, err := volcleanReadFile(path)
	if err != nil {
		return declared, used, orphans
	}
	var data map[string]interface{}
	if err := yaml.Unmarshal([]byte(raw), &data); err != nil {
		return declared, used, orphans
	}
	if data == nil {
		return declared, used, orphans
	}

	topvols, ok := data["volumes"].(map[string]interface{})
	if !ok {
		// Python: data.get("volumes") or {}; if not a dict → empty.
		return declared, used, orphans
	}
	for k := range topvols {
		declared[k] = true
	}

	services, _ := data["services"].(map[string]interface{})
	for _, sv := range services {
		body, ok := sv.(map[string]interface{})
		if !ok {
			continue
		}
		vols, ok := body["volumes"].([]interface{})
		if !ok {
			continue
		}
		for _, v := range vols {
			switch vv := v.(type) {
			case string:
				src := strings.TrimSpace(strings.SplitN(vv, ":", 2)[0])
				if volcleanIsNamed(src) {
					used[src] = true
				}
			case map[string]interface{}:
				if s, ok := vv["source"]; ok && s != nil {
					srcStr := fmt.Sprintf("%v", s)
					if volcleanIsNamed(srcStr) {
						used[srcStr] = true
					}
				}
			}
		}
	}

	for n := range declared {
		if !used[n] && !volcleanReferencedAsMount(raw, n) {
			orphans = append(orphans, n)
		}
	}
	sort.Strings(orphans)
	return declared, used, orphans
}

// volcleanBackup copies path into the backup dir with a timestamp suffix.
func volcleanBackup(path string) (string, error) {
	dir := volcleanBackupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s.%s.bak",
		filepath.Base(path), time.Now().Format("20060102-150405")))
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if info, e := os.Stat(path); e == nil {
		_ = os.Chtimes(dst, time.Now(), info.ModTime())
		_ = os.Chmod(dst, info.Mode())
	}
	return dst, nil
}

var (
	volcleanReVolumesHdr   = regexp.MustCompile(`^volumes:\s*$`)
	volcleanReTopKey       = regexp.MustCompile(`^\S`)
	volcleanReEntry        = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):`)
	volcleanReContinuation = regexp.MustCompile(`^    `)
	volcleanReHasEntry     = regexp.MustCompile(`^  \S`)
)

// volcleanStripOrphans removes orphan entries from the top-level volumes: section.
// Returns (removedCount, backupPath).
func volcleanStripOrphans(path string, orphans []string) (int, string) {
	if len(orphans) == 0 {
		return 0, ""
	}
	orphSet := map[string]bool{}
	for _, o := range orphans {
		orphSet[o] = true
	}
	raw, err := volcleanReadFile(path)
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(raw, "\n")

	vstart := -1
	for i, l := range lines {
		if volcleanReVolumesHdr.MatchString(l) {
			vstart = i
			break
		}
	}
	if vstart == -1 {
		return 0, ""
	}

	vend := len(lines)
	for j := vstart + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if volcleanReTopKey.MatchString(lines[j]) { // next top-level key
			vend = j
			break
		}
	}

	section := lines[vstart+1 : vend]
	newSection := []string{}
	removed := 0
	i := 0
	for i < len(section) {
		m := volcleanReEntry.FindStringSubmatch(section[i])
		if m != nil && orphSet[m[1]] {
			i++
			for i < len(section) && volcleanReContinuation.MatchString(section[i]) { // 4+ space continuation
				i++
			}
			removed++
		} else {
			newSection = append(newSection, section[i])
			i++
		}
	}
	if removed == 0 {
		return 0, ""
	}

	bak, err := volcleanBackup(path)
	if err != nil {
		return 0, ""
	}

	hasEntry := false
	for _, l := range newSection {
		if volcleanReHasEntry.MatchString(l) {
			hasEntry = true
			break
		}
	}

	var rebuilt []string
	if hasEntry {
		rebuilt = append(rebuilt, lines[:vstart]...)
		rebuilt = append(rebuilt, lines[vstart])
		rebuilt = append(rebuilt, newSection...)
		rebuilt = append(rebuilt, lines[vend:]...)
	} else { // volumes section now empty → drop header
		rebuilt = append(rebuilt, lines[:vstart]...)
		rebuilt = append(rebuilt, lines[vend:]...)
	}
	out := strings.TrimRight(strings.Join(rebuilt, "\n"), "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 0, ""
	}
	return removed, bak
}

// volcleanRow holds one stack's scan result with orphans.
type volcleanRow struct {
	stack    string
	path     string
	declared map[string]bool
	used     map[string]bool
	orphans  []string
}

// volcleanGlobYaml lists sorted *.yml files in the stacks dir.
func volcleanGlobYaml() []string {
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	return matches
}

// volcleanScanAll returns rows for every stack with orphans.
func volcleanScanAll() []volcleanRow {
	rows := []volcleanRow{}
	for _, path := range volcleanGlobYaml() {
		declared, used, orphans := volcleanAnalyze(path)
		if len(orphans) > 0 {
			base := filepath.Base(path)
			stack := strings.TrimSuffix(base, ".yml")
			rows = append(rows, volcleanRow{stack, path, declared, used, orphans})
		}
	}
	return rows
}

// volcleanEnsureNamedDecls is the inverse of strip: add a top-level declaration
// for every NAMED volume a service references but that isn't declared.
// Returns (addedCount, backupPath).
func volcleanEnsureNamedDecls(path string) (int, string) {
	declared, used, _ := volcleanAnalyze(path)
	missing := []string{}
	for u := range used {
		if !declared[u] {
			missing = append(missing, u)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return 0, ""
	}

	bak, err := volcleanBackup(path)
	if err != nil {
		return 0, ""
	}
	raw, err := volcleanReadFile(path)
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(raw, "\n")
	add := make([]string, 0, len(missing))
	for _, n := range missing {
		add = append(add, "  "+n+":")
	}

	vstart := -1
	for i, l := range lines {
		if volcleanReVolumesHdr.MatchString(l) {
			vstart = i
			break
		}
	}
	if vstart == -1 { // no volumes: section → append one
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "volumes:")
		lines = append(lines, add...)
	} else { // insert at end of existing section
		vend := len(lines)
		for j := vstart + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if volcleanReTopKey.MatchString(lines[j]) {
				vend = j
				break
			}
		}
		newLines := []string{}
		newLines = append(newLines, lines[:vend]...)
		newLines = append(newLines, add...)
		newLines = append(newLines, lines[vend:]...)
		lines = newLines
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 0, ""
	}
	return len(missing), bak
}

// volcleanMissingRow holds a stack referencing undeclared named volumes.
type volcleanMissingRow struct {
	stack   string
	path    string
	missing []string
}

// volcleanScanMissing returns stacks referencing undeclared named volumes.
func volcleanScanMissing() []volcleanMissingRow {
	rows := []volcleanMissingRow{}
	for _, path := range volcleanGlobYaml() {
		declared, used, _ := volcleanAnalyze(path)
		missing := []string{}
		for u := range used {
			if !declared[u] {
				missing = append(missing, u)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			base := filepath.Base(path)
			stack := strings.TrimSuffix(base, ".yml")
			rows = append(rows, volcleanMissingRow{stack, path, missing})
		}
	}
	return rows
}

// volcleanMain is the CLI entry point (faithful port of main()).
func volcleanMain(args []string) {
	cmd := "report"
	if len(args) > 0 {
		cmd = args[0]
	}
	rows := volcleanScanAll()

	switch cmd {
	case "report":
		if inList(args, "--json") {
			obj := map[string][]string{}
			for _, r := range rows {
				obj[r.stack] = r.orphans
			}
			b, _ := json.MarshalIndent(obj, "", "  ")
			fmt.Println(string(b))
			return
		}
		if len(rows) == 0 {
			fmt.Println("✓ No unused top-level named volumes.")
			return
		}
		total := 0
		for _, r := range rows {
			total += len(r.orphans)
		}
		fmt.Printf("⚠ %d unused top-level named-volume declaration(s) in %d stack(s):\n\n", total, len(rows))
		for _, r := range rows {
			preview := r.orphans
			ellipsis := ""
			if len(preview) > 6 {
				preview = preview[:6]
				ellipsis = " …"
			}
			fmt.Printf("  %-14s %3d declared → %d unused: %s%s\n",
				r.stack, len(r.declared), len(r.orphans), strings.Join(preview, ", "), ellipsis)
		}
		fmt.Println("\nClean up:  stacks volclean clean          (interactive, per-stack)")
		fmt.Println("           stacks volclean clean --auto   (strip them all, with backups)")

	case "clean":
		if len(rows) == 0 {
			fmt.Println("✓ Nothing to clean.")
			return
		}
		auto := inList(args, "--auto")
		only := map[string]bool{}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				only[a] = true
			}
		}
		reader := bufio.NewReader(os.Stdin)
		total := 0
		for _, r := range rows {
			if len(only) > 0 && !only[r.stack] {
				continue
			}
			if !auto {
				fmt.Printf("\n%s: %d unused → %s\n", r.stack, len(r.orphans), strings.Join(r.orphans, ", "))
				fmt.Print("  strip these? [y/N/q]: ")
				line, _ := reader.ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans == "q" {
					break
				}
				if ans != "y" {
					fmt.Println("  skipped.")
					continue
				}
			}
			n, bak := volcleanStripOrphans(r.path, r.orphans)
			total += n
			fmt.Printf("  ✓ %s: stripped %d  (backup: %s)\n", r.stack, n, bak)
		}
		fmt.Printf("\nDone — removed %d unused volume declaration(s). Backups in %s\n", total, volcleanBackupDir())

	case "ensure":
		miss := volcleanScanMissing()
		if len(miss) == 0 {
			fmt.Println("✓ Every referenced named volume is already declared.")
			return
		}
		only := map[string]bool{}
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				only[a] = true
			}
		}
		total := 0
		for _, r := range miss {
			if len(only) > 0 && !only[r.stack] {
				continue
			}
			n, bak := volcleanEnsureNamedDecls(r.path)
			total += n
			fmt.Printf("  ✓ %s: added %d declaration(s)  (backup: %s)\n", r.stack, n, bak)
		}
		fmt.Printf("\nDone — added %d top-level volume declaration(s).\n", total)

	default:
		fmt.Print(volcleanDoc)
	}
}

// ===== from purge.go =====

const purgeDoc = `
stacks_purge.py — delete a SERVICE or a whole STACK and clean up after it.

Beyond just removing the container/stack, this also strips the networks (and
named volumes) it used out of the PROVISIONER (core_*) stacks + every other
stack's top-level declaration — but ONLY networks/volumes that no remaining real
service still uses. Provisioner containers (name starts 'provisioner') attach to
networks purely to create them, so they DON'T count as real users.

Safe: dry-run by default (report what it WOULD do). ` + "`--apply`" + ` makes changes, and
every edited file is backed up to ~/.config/stacks/purge-backups first.

Usage:
  stacks_purge.py service <stack> <container> [--apply]
  stacks_purge.py stack   <stack>             [--apply]
`

func purgeBackupDir() string  { return filepath.Join(configDir(), "purge-backups") }
func purgeArchiveDir() string { return filepath.Join(configDir(), "removed-stacks") }

// top-level single-line net/vol decl:  "  name: { ... }"
var purgeTopRE = regexp.MustCompile(`^(  )([A-Za-z0-9_.-]+):\s*\{(.*)\}\s*$`)

var (
	purgeServicesRE  = regexp.MustCompile(`^services:`)
	purgeLeftRE      = regexp.MustCompile(`^\S`)
	purgeSvcKeyRE    = regexp.MustCompile(`^  ([A-Za-z0-9_.-]+):\s*$`)
	purgeCNameRE     = regexp.MustCompile(`^\s+container_name:\s*"?([A-Za-z0-9_.-]+)`)
	purgeNetsHdrRE   = regexp.MustCompile(`^    networks:`)
	purgeNetEntryRE1 = regexp.MustCompile(`^      ([A-Za-z0-9_.-]+):`)
	purgeNetEntryRE2 = regexp.MustCompile(`^      -\s*([A-Za-z0-9_.-]+)`)
	purgeSvcLvlRE    = regexp.MustCompile(`^    [A-Za-z]`)
	purgeNextSvcRE   = regexp.MustCompile(`^  \S`)
	purgeTopHdrRE    = regexp.MustCompile(`^(networks|volumes):`)
	purge6spaceRE    = regexp.MustCompile(`^      ([A-Za-z0-9_.-]+):`)
	purge8spaceRE    = regexp.MustCompile(`^        \S`)
	// volume parsing: service-level `volumes:` header (4-space) + short-form
	// entries. A NAMED volume source is a bare identifier (no path chars).
	purgeVolsHdrRE     = regexp.MustCompile(`^    volumes:`)
	purgeVolEntryRE    = regexp.MustCompile(`^      -\s*["']?([^"':]+):`)
	purgeVolLongSrcRE  = regexp.MustCompile(`^\s*source:\s*["']?([^"'\s]+)`)
	purgeVolNamedRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	purgeTopVolsHdrRE  = regexp.MustCompile(`^volumes:\s*$`)
	purgeTopVolEntryRE = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):`)
)

// purgeServiceBlock mirrors a single (key, container_name, start, end) tuple.
type purgeServiceBlock struct {
	key   string
	cname string // "" == None
	start int
	end   int
}

func purgeStackFiles(includeExt bool) []string {
	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	if includeExt {
		return matches
	}
	var out []string
	for _, f := range matches {
		if !strings.HasSuffix(f, "-ext.yml") {
			out = append(out, f)
		}
	}
	return out
}

func purgeBackup(f string) (string, error) {
	if err := os.MkdirAll(purgeBackupDir(), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(purgeBackupDir(), fmt.Sprintf("%s.%s.bak", filepath.Base(f), time.Now().Format("20060102-150405")))
	data, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func purgeReadLines(f string) []string {
	data, err := os.ReadFile(f)
	if err != nil {
		return []string{}
	}
	return strings.Split(string(data), "\n")
}

func purgeWriteJoined(f string, lines []string) error {
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(f, []byte(body), 0o644)
}

// ── parsing ──────────────────────────────────────────────────────────────────

// purgeServiceBlocks yields (key, container_name, start, end) for each service
// in file f. start/end are line indices of the 2-space `  key:` block (end excl).
func purgeServiceBlocks(f string) ([]string, []purgeServiceBlock) {
	lines := purgeReadLines(f)
	inservices := false
	var blocks []purgeServiceBlock
	var cur *purgeServiceBlock
	for i, ln := range lines {
		if purgeServicesRE.MatchString(ln) {
			inservices = true
			continue
		}
		if purgeLeftRE.MatchString(ln) { // left margin → left services:
			if inservices && cur != nil {
				cur.end = i
				blocks = append(blocks, *cur)
				cur = nil
			}
			inservices = false
		}
		if !inservices {
			continue
		}
		if m := purgeSvcKeyRE.FindStringSubmatch(ln); m != nil {
			if cur != nil {
				cur.end = i
				blocks = append(blocks, *cur)
			}
			cur = &purgeServiceBlock{key: m[1], cname: "", start: i, end: 0}
		} else if cur != nil && cur.cname == "" {
			if cm := purgeCNameRE.FindStringSubmatch(ln); cm != nil {
				cur.cname = cm[1]
			}
		}
	}
	if cur != nil {
		cur.end = len(lines)
		blocks = append(blocks, *cur)
	}
	return lines, blocks
}

// purgeNetworksOfBlock returns networks listed in a service block's `networks:`
// section. Service keys are 4-space; network entries 6-space; children 8-space.
func purgeNetworksOfBlock(lines []string, start, end int) map[string]bool {
	nets := map[string]bool{}
	innet := false
	for _, ln := range lines[start:end] {
		if purgeNetsHdrRE.MatchString(ln) {
			innet = true
			continue
		}
		if innet {
			var name string
			if m := purgeNetEntryRE1.FindStringSubmatch(ln); m != nil {
				name = m[1]
			} else if m := purgeNetEntryRE2.FindStringSubmatch(ln); m != nil {
				name = m[1]
			}
			if name != "" {
				nets[name] = true
			} else if purgeSvcLvlRE.MatchString(ln) || purgeNextSvcRE.MatchString(ln) {
				innet = false // next service-level key or next service
			}
		}
	}
	return nets
}

// purgeNetworkUsers mirrors network_users(): {net: set('stack:service')} across
// all stacks, EXCLUDING provisioner* services. skip = set of "stack\x00cname"
// to ignore (the ones being deleted).
func purgeNetworkUsers(skip map[string]bool) map[string]map[string]bool {
	if skip == nil {
		skip = map[string]bool{}
	}
	users := map[string]map[string]bool{}
	for _, f := range purgeStackFiles(false) {
		stack := strings.TrimSuffix(filepath.Base(f), ".yml")
		lines, blocks := purgeServiceBlocks(f)
		for _, b := range blocks {
			if b.cname != "" && strings.HasPrefix(b.cname, "provisioner") {
				continue
			}
			if skip[purgeSkipKey(stack, b.cname)] {
				continue
			}
			for n := range purgeNetworksOfBlock(lines, b.start, b.end) {
				if users[n] == nil {
					users[n] = map[string]bool{}
				}
				who := b.cname
				if who == "" {
					who = b.key
				}
				users[n][stack+":"+who] = true
			}
		}
	}
	return users
}

func purgeSkipKey(stack, cname string) string { return stack + "\x00" + cname }

// purgeTopLevelDecls mirrors _toplevel_decls(): [(file, line_index)] where net
// is declared at top level (networks/volumes).
func purgeTopLevelDecls(net string) [][2]interface{} {
	var hits [][2]interface{}
	for _, f := range purgeStackFiles(false) {
		lines := purgeReadLines(f)
		intop := false
		for i, ln := range lines {
			if purgeTopHdrRE.MatchString(ln) {
				intop = true
				continue
			}
			if purgeLeftRE.MatchString(ln) {
				intop = false
			}
			if intop {
				if m := purgeTopRE.FindStringSubmatch(ln); m != nil && m[2] == net {
					hits = append(hits, [2]interface{}{f, i})
				}
			}
		}
	}
	return hits
}

// ── mutation ──────────────────────────────────────────────────────────────────

func purgeRemoveBlock(f string, start, end int) {
	lines := purgeReadLines(f)
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	lines = append(lines[:start], lines[end:]...)
	purgeWriteJoined(f, lines)
}

// purgeRemoveTopLevel removes every top-level declaration of net. Returns the
// list of basenames touched.
func purgeRemoveTopLevel(net string) []string {
	var touched []string
	for _, hit := range purgeTopLevelDecls(net) {
		f := hit[0].(string)
		lines := purgeReadLines(f)
		var newLines []string
		for _, ln := range lines {
			m := purgeTopRE.FindStringSubmatch(ln)
			if m != nil && m[2] == net {
				continue
			}
			newLines = append(newLines, ln)
		}
		if len(newLines) != len(lines) {
			purgeBackup(f)
			purgeWriteJoined(f, newLines)
			touched = append(touched, filepath.Base(f))
		}
	}
	return touched
}

// purgeStripFromProvisioners removes net from every provisioner service's
// networks: block. Returns basenames touched.
func purgeStripFromProvisioners(net string) []string {
	var touched []string
	for _, f := range purgeStackFiles(false) {
		lines := purgeReadLines(f)
		_, blocks := purgeServiceBlocks(f)
		type span struct{ s, e int }
		var prov []span
		for _, b := range blocks {
			if b.cname != "" && strings.HasPrefix(b.cname, "provisioner") {
				prov = append(prov, span{b.start, b.end})
			}
		}
		if len(prov) == 0 {
			continue
		}
		// first pass: mark dropped net-header lines with a sentinel
		const sentinel = "\x00__DROP_NET__\x00"
		var out []string
		for i, ln := range lines {
			inProv := false
			for _, p := range prov {
				if p.s <= i && i < p.e {
					inProv = true
					break
				}
			}
			if inProv {
				if m := purge6spaceRE.FindStringSubmatch(ln); m != nil && m[1] == net {
					out = append(out, sentinel)
					continue
				}
			}
			out = append(out, ln)
		}
		// second pass: drop 8-space children following a dropped net header
		var final []string
		dropping := false
		for _, ln := range out {
			if ln == sentinel {
				dropping = true
				continue
			}
			if dropping {
				if purge8spaceRE.MatchString(ln) { // 8-space child of the net
					continue
				}
				dropping = false
			}
			final = append(final, ln)
		}
		if !purgeSliceEqual(final, lines) {
			purgeBackup(f)
			purgeWriteJoined(f, final)
			touched = append(touched, filepath.Base(f))
		}
	}
	return touched
}

// purgeMove mirrors shutil.move: rename if possible, else copy+delete so it
// works across filesystems (archive dir may live on a different device).
func purgeMove(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

func purgeSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func purgeNetworks(orphans []string, apply bool, log *[]string) {
	sorted := append([]string(nil), orphans...)
	sort.Strings(sorted)
	for _, net := range sorted {
		if !apply {
			*log = append(*log, fmt.Sprintf("  would remove network '%s': top-level decls + provisioner refs + docker network", net))
			continue
		}
		t1 := purgeRemoveTopLevel(net)
		t2 := purgeStripFromProvisioners(net)
		// docker network rm only if it exists and is empty
		removedDocker := false
		for _, row := range networkTable() {
			if row.Name == net && row.Count == 0 {
				removedDocker = removeNetwork(row.ID)
				break
			}
		}
		decls := purgeUniqSorted(append(append([]string(nil), t1...), t2...))
		declStr := "none"
		if len(decls) > 0 {
			declStr = strings.Join(decls, ",")
		}
		dockerStr := ""
		if removedDocker {
			dockerStr = "; docker net rm"
		}
		*log = append(*log, fmt.Sprintf("  removed network '%s' (decls: %s%s)", net, declStr, dockerStr))
	}
}

// ── volume cascade (mirrors the network cascade) ───────────────────────────────

// purgeVolumesOfBlock returns the NAMED top-level volumes a service block
// references in its `volumes:` section. Service keys are 4-space; volume
// entries are 6-space short-form (`- name:/path`) or long-form (`source: name`).
// Bind mounts (source starting with / . ~ $) and anonymous volumes are excluded;
// only sources matching ^[A-Za-z0-9][A-Za-z0-9_-]*$ count as named.
func purgeVolumesOfBlock(lines []string, start, end int) map[string]bool {
	vols := map[string]bool{}
	invol := false
	for _, ln := range lines[start:end] {
		if purgeVolsHdrRE.MatchString(ln) {
			invol = true
			continue
		}
		if invol {
			// short form:  - name:/container/path
			if m := purgeVolEntryRE.FindStringSubmatch(ln); m != nil {
				src := strings.TrimSpace(m[1])
				if purgeVolNamedRE.MatchString(src) {
					vols[src] = true
				}
				continue
			}
			// long form:  source: name  (8-space child under a `- ` list item)
			if m := purgeVolLongSrcRE.FindStringSubmatch(ln); m != nil && strings.HasPrefix(ln, "        ") {
				src := strings.TrimSpace(m[1])
				if purgeVolNamedRE.MatchString(src) {
					vols[src] = true
				}
				continue
			}
			// stop at the next service-level key or the next service block
			if purgeSvcLvlRE.MatchString(ln) || purgeNextSvcRE.MatchString(ln) {
				invol = false
			}
		}
	}
	return vols
}

// purgeVolumeUsers mirrors purgeNetworkUsers: {volume: set('stack:service')}
// across all stacks, EXCLUDING provisioner* services. skip = set of
// "stack\x00cname" to ignore (the ones being deleted).
func purgeVolumeUsers(skip map[string]bool) map[string]map[string]bool {
	if skip == nil {
		skip = map[string]bool{}
	}
	users := map[string]map[string]bool{}
	for _, f := range purgeStackFiles(false) {
		stack := strings.TrimSuffix(filepath.Base(f), ".yml")
		lines, blocks := purgeServiceBlocks(f)
		for _, b := range blocks {
			if b.cname != "" && strings.HasPrefix(b.cname, "provisioner") {
				continue
			}
			if skip[purgeSkipKey(stack, b.cname)] {
				continue
			}
			for v := range purgeVolumesOfBlock(lines, b.start, b.end) {
				if users[v] == nil {
					users[v] = map[string]bool{}
				}
				who := b.cname
				if who == "" {
					who = b.key
				}
				users[v][stack+":"+who] = true
			}
		}
	}
	return users
}

// purgeRemoveVolTopLevel removes every top-level `volumes:` declaration of vol
// (its key + any nested children) from whatever stack files declare it. If a
// file's top-level volumes section becomes empty, its header is removed too.
// Only writes when there is something to remove; backs up first.
// Returns the list of basenames touched.
func purgeRemoveVolTopLevel(vol string) []string {
	var touched []string
	for _, f := range purgeStackFiles(false) {
		lines := purgeReadLines(f)

		vstart := -1
		for i, ln := range lines {
			if purgeTopVolsHdrRE.MatchString(ln) {
				vstart = i
				break
			}
		}
		if vstart == -1 {
			continue
		}
		// section runs until the next top-level (column-0) key
		vend := len(lines)
		for j := vstart + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if purgeLeftRE.MatchString(lines[j]) {
				vend = j
				break
			}
		}
		section := lines[vstart+1 : vend]
		var newSection []string
		removed := 0
		i := 0
		for i < len(section) {
			m := purgeTopVolEntryRE.FindStringSubmatch(section[i])
			if m != nil && m[1] == vol {
				i++
				// drop nested 4+-space children of this volume key
				for i < len(section) && volcleanReContinuation.MatchString(section[i]) {
					i++
				}
				removed++
			} else {
				newSection = append(newSection, section[i])
				i++
			}
		}
		if removed == 0 {
			continue
		}
		purgeBackup(f)
		hasEntry := false
		for _, ln := range newSection {
			if purgeTopVolEntryRE.MatchString(ln) {
				hasEntry = true
				break
			}
		}
		var rebuilt []string
		if hasEntry {
			rebuilt = append(rebuilt, lines[:vstart]...)
			rebuilt = append(rebuilt, lines[vstart])
			rebuilt = append(rebuilt, newSection...)
			rebuilt = append(rebuilt, lines[vend:]...)
		} else { // volumes section now empty → drop header
			rebuilt = append(rebuilt, lines[:vstart]...)
			rebuilt = append(rebuilt, lines[vend:]...)
		}
		purgeWriteJoined(f, rebuilt)
		touched = append(touched, filepath.Base(f))
	}
	return touched
}

// purgeVolumes mirrors purgeNetworks: for each orphan named volume strip it from
// the declaring stack's top-level volumes block, and when apply==true also
// `docker volume rm <volume>`. Logs clearly.
func purgeVolumes(orphans []string, apply bool, log *[]string) {
	sorted := append([]string(nil), orphans...)
	sort.Strings(sorted)
	for _, vol := range sorted {
		if !apply {
			*log = append(*log, fmt.Sprintf("  would remove volume '%s': top-level decls + docker volume", vol))
			continue
		}
		decls := purgeRemoveVolTopLevel(vol)
		// docker volume rm (best-effort; safe — only the orphan)
		removedDocker := false
		if r := cli("volume", "rm", vol); r.exitCode == 0 {
			removedDocker = true
		}
		declStr := "none"
		if len(decls) > 0 {
			declStr = strings.Join(purgeUniqSorted(decls), ",")
		}
		dockerStr := ""
		if removedDocker {
			dockerStr = "; docker vol rm"
		}
		*log = append(*log, fmt.Sprintf("  removed volume '%s' (decls: %s%s)", vol, declStr, dockerStr))
	}
}

func purgeUniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func purgeSortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// purgeFmtNetList mirrors `sorted(nets) or '[]'`.
func purgeFmtNetList(m map[string]bool) string {
	s := purgeSortedSet(m)
	if len(s) == 0 {
		return "[]"
	}
	return fmt.Sprintf("[%s]", purgePyList(s))
}

// purgePyList renders a slice the way Python prints a list of strings.
func purgePyList(s []string) string {
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = "'" + v + "'"
	}
	return strings.Join(parts, ", ")
}

// ── public ops ────────────────────────────────────────────────────────────────

func purgeService(stack, container string, apply bool) []string {
	var log []string
	f := filepath.Join(stacksDir(), stack+".yml")
	if st, err := os.Stat(f); err != nil || st.IsDir() {
		return []string{"no such stack: " + stack}
	}
	lines, blocks := purgeServiceBlocks(f)
	var target *purgeServiceBlock
	for i := range blocks {
		if blocks[i].cname == container {
			target = &blocks[i]
			break
		}
	}
	if target == nil {
		return []string{fmt.Sprintf("service '%s' not found in %s", container, stack)}
	}
	svcNets := purgeNetworksOfBlock(lines, target.start, target.end)
	svcVols := purgeVolumesOfBlock(lines, target.start, target.end)
	log = append(log, fmt.Sprintf("service %s (key %s) in %s: networks %s", container, target.key, stack, purgeFmtNetList(svcNets)))
	log = append(log, fmt.Sprintf("  named volumes %s", purgeFmtNetList(svcVols)))
	skip := map[string]bool{purgeSkipKey(stack, container): true}
	// who still uses those nets once this service is gone?
	users := purgeNetworkUsers(skip)
	var orphans, keep []string
	for n := range svcNets {
		if users[n] == nil {
			orphans = append(orphans, n)
		} else {
			keep = append(keep, n)
		}
	}
	if len(keep) > 0 {
		sort.Strings(keep)
		log = append(log, "  kept networks (still used): "+strings.Join(keep, ", "))
	}
	// who still uses those volumes once this service is gone?
	volUsers := purgeVolumeUsers(skip)
	var volOrphans, volKeep []string
	for v := range svcVols {
		if volUsers[v] == nil {
			volOrphans = append(volOrphans, v)
		} else {
			volKeep = append(volKeep, v)
		}
	}
	if len(volKeep) > 0 {
		sort.Strings(volKeep)
		log = append(log, "  kept volumes (still used): "+strings.Join(volKeep, ", "))
	}
	if apply {
		purgeBackup(f)
		purgeRemoveBlock(f, target.start, target.end)
		log = append(log, fmt.Sprintf("  removed service block from %s.yml", stack))
		removeContainer(container, true, false)
		log = append(log, "  docker rm "+container)
	} else {
		log = append(log, fmt.Sprintf("  would remove service block from %s.yml + docker rm %s", stack, container))
	}
	purgeNetworks(orphans, apply, &log)
	purgeVolumes(volOrphans, apply, &log)
	return log
}

func purgeStackOp(stack string, apply bool) []string {
	var log []string
	f := filepath.Join(stacksDir(), stack+".yml")
	if st, err := os.Stat(f); err != nil || st.IsDir() {
		return []string{"no such stack: " + stack}
	}
	lines, blocks := purgeServiceBlocks(f)
	var real []purgeServiceBlock
	for _, b := range blocks {
		if b.cname != "" && !strings.HasPrefix(b.cname, "provisioner") {
			real = append(real, b)
		}
	}
	allNets := map[string]bool{}
	for _, b := range real {
		for n := range purgeNetworksOfBlock(lines, b.start, b.end) {
			allNets[n] = true
		}
	}
	skip := map[string]bool{}
	for _, b := range real {
		skip[purgeSkipKey(stack, b.cname)] = true
	}
	users := purgeNetworkUsers(skip)
	var orphans, keep []string
	for n := range allNets {
		if users[n] == nil {
			orphans = append(orphans, n)
		} else {
			keep = append(keep, n)
		}
	}
	log = append(log, fmt.Sprintf("stack %s: %d service(s); networks %s", stack, len(real), purgeFmtNetList(allNets)))
	if len(keep) > 0 {
		sort.Strings(keep)
		log = append(log, "  kept (used elsewhere): "+strings.Join(keep, ", "))
	}
	if apply {
		// down -v + archive the file
		cli("compose", "-p", stack, "--project-directory", stacksDir(), "-f", f, "down", "-v")
		os.MkdirAll(purgeArchiveDir(), 0o755)
		dst := filepath.Join(purgeArchiveDir(), fmt.Sprintf("%s.yml.%s", stack, time.Now().Format("20060102-150405")))
		purgeMove(f, dst)
		log = append(log, fmt.Sprintf("  down -v + archived %s.yml", stack))
	} else {
		log = append(log, fmt.Sprintf("  would down -v + archive %s.yml", stack))
	}
	purgeNetworks(orphans, apply, &log)
	return log
}

// purgeMain mirrors the __main__ entrypoint: argv-style dispatch, prints output.
func purgeMain(argv []string) {
	apply := inList(argv, "--apply")
	var a []string
	for _, x := range argv {
		if x != "--apply" {
			a = append(a, x)
		}
	}
	var out []string
	if len(a) >= 3 && a[0] == "service" {
		out = purgeService(a[1], a[2], apply)
	} else if len(a) >= 2 && a[0] == "stack" {
		out = purgeStackOp(a[1], apply)
	} else {
		out = []string{purgeDoc}
	}
	fmt.Println(strings.Join(out, "\n"))
}

// ===== from describe.go =====

// describe.go — faithful Go port of stacks_describe.py.
//
// stacks_describe — inject service descriptions from conf files.
// Conf dir: ~/.config/stacks/descriptions/
// Usage: describeMain("all") | describeMain(stackname)

// describeConfDir is the descriptions conf dir (universal: configDir()/descriptions).
func describeConfDir() string {
	return filepath.Join(configDir(), "descriptions")
}

// describeLookup mirrors the Python LOOKUP dict (insertion order preserved via
// describeLookupOrder so the "contains" matching scans in the same sequence).
var describeLookup = map[string]string{
	"ollama":         "Local LLM inference server with AMD ROCm GPU acceleration",
	"comfyui":        "Node-based Stable Diffusion image generation GUI",
	"openwebui":      "Elegant chat frontend for local LLMs and RAG",
	"playwright":     "Headless browser for AI web scraping and automation",
	"searxng":        "Privacy-respecting metasearch engine for AI web search",
	"n8n":            "Visual drag-and-drop workflow automation platform",
	"letta":          "Autonomous agents with persistent long-term memory",
	"litellm":        "Multi-provider AI gateway standardizing LLM APIs",
	"langflow":       "Visual LLM pipeline and agent framework builder",
	"langfuse":       "LLM observability and prompt management platform",
	"traefik":        "Cloud-native reverse proxy and load balancer",
	"sablier":        "Container wake-on-demand autoscaling service",
	"authelia":       "Single sign-on authentication and authorization server",
	"crowdsec":       "Collaborative intrusion detection and prevention system",
	"portainer":      "Web UI for managing Docker containers and stacks",
	"rancher":        "Enterprise Kubernetes and container management platform",
	"gitea":          "Lightweight self-hosted Git repository service",
	"nextcloud":      "Self-hosted cloud storage and collaboration suite",
	"immich":         "High-performance self-hosted photo and video backup",
	"grafana":        "Analytics and monitoring dashboard platform",
	"prometheus":     "Time-series metrics collection and alerting system",
	"loki":           "Log aggregation system designed for Grafana",
	"pihole":         "Network-wide DNS ad blocking server",
	"adguard":        "DNS-based ad and tracker blocking server",
	"technitium":     "Advanced self-hosted DNS server with web UI",
	"wazuh":          "Open source SIEM security monitoring platform",
	"vaultwarden":    "Lightweight self-hosted Bitwarden password manager",
	"jellyfin":       "Free self-hosted media streaming server",
	"postgres":       "Robust open source relational database server",
	"mariadb":        "High-performance MySQL-compatible database server",
	"redis":          "In-memory data structure store and cache",
	"mongodb":        "NoSQL document-oriented database server",
	"neo4j":          "Native graph database for connected data",
	"qdrant":         "High-performance vector similarity search engine",
	"surrealdb":      "Multi-model cloud-native database engine",
	"minio":          "S3-compatible high-performance object storage server",
	"netbird":        "WireGuard-based zero-config mesh VPN platform",
	"tailscale":      "Zero-config WireGuard mesh VPN client",
	"cloudflared":    "Cloudflare Tunnel daemon for secure external access",
	"pangolin":       "Secure tunnel relay for private network access",
	"wazuhindexer":   "Wazuh SIEM data indexing and storage engine",
	"wazuhmanager":   "Wazuh security event collection and analysis hub",
	"wazuhdashboard": "Wazuh SIEM web dashboard and visualization UI",
	"generator":      "Wazuh configuration and certificate generator",
	"pangolinclient": "Secure Pangolin tunnel client for remote access",
	"gerbil":         "Pangolin tunnel relay service component",
	"errorpages":     "Custom styled HTTP error pages for Traefik",
	"authentik":      "Open source identity provider and SSO platform",
	"jellyseerr":     "Media request management for Jellyfin",
	"zoraxy":         "Simple self-hosted reverse proxy manager",
	"openresty":      "Nginx-based web platform with Lua scripting",
	"defectdojo":     "DevSecOps vulnerability management platform",
	"voidauth":       "Lightweight authentication proxy service",
	"dockhand":       "Docker webhook and automation handler",
	"speaches":       "OpenAI-compatible speech-to-text API server",
	"whisper":        "Fast Whisper speech recognition backend",
	"terminalagent":  "Open Interpreter AI code execution agent",
	"opennotebook":   "AI-powered Jupyter-style notebook interface",
	"browserless":    "Headless Chrome browser as a service",
	"gooseagent":     "AI coding agent with tool use capabilities",
	"tabby":          "Self-hosted AI coding assistant server",
	"hermes":         "Custom AI agent hub and workspace platform",
	"zep":            "Long-term memory store for AI assistants",
	"memos":          "Lightweight self-hosted memo and note service",
	"supabase":       "Open source Firebase alternative platform",
	"librechat":      "Enhanced multi-provider AI chat interface",
	"exo":            "Distributed AI inference cluster framework",
	"dockmate":       "Docker container management and monitoring UI",
	"glance":         "Self-hosted dashboard for server overview",
	"coolify":        "Self-hosted Heroku and Netlify alternative PaaS",
	"dokploy":        "Free self-hosted app deployment platform",
	"pterodactyl":    "Open source game server management panel",
	"penpot":         "Open source design and prototyping platform",
	"appsmith":       "Low-code platform for building internal tools",
	"tooljet":        "Open source low-code application builder",
	"syncthing":      "Continuous peer-to-peer file synchronization",
	"invidious":      "Privacy-respecting YouTube frontend",
	"mealie":         "Self-hosted recipe manager and meal planner",
	"tandoor":        "Recipe management platform with meal planning",
	"homeassistant":  "Open source home automation platform",
	"nodered":        "Flow-based visual IoT programming tool",
	"mosquitto":      "Lightweight MQTT message broker",
	"dify":           "Open source LLM app development platform",
	"windmill":       "Open source developer platform for scripts",
	"netdata":        "Real-time infrastructure monitoring and alerting",
	"komodo":         "Container and server management platform",
	"beszel":         "Lightweight server resource monitoring hub",
	"dozzle":         "Real-time Docker container log viewer",
	"ntopng":         "High-speed network traffic analysis tool",
	"headscale":      "Self-hosted Tailscale control server",
	"headplane":      "Web UI management panel for Headscale",
	"clamav":         "Open source antivirus engine and scanner",
	"odoo":           "Open source ERP and business application suite",
	"dolibarr":       "Open source ERP and CRM platform",
	"gamevault":      "Self-hosted game library and launcher",
	"romm":           "Self-hosted retro game ROM manager",
	"webtop":         "Full Linux desktop environment in the browser",
	"scrutiny":       "Hard drive SMART monitoring dashboard",
	"duplicati":      "Encrypted cloud backup solution",
	"borgmatic":      "Automated BorgBackup wrapper utility",
	"provisioner":    "NetBird management server provisioner",
	"trivy":          "Container and filesystem vulnerability scanner",
	"redroid":        "Android container for x86 hosts via KVM",
	"dokku":          "Docker-powered mini-Heroku PaaS platform",
}

// describeLookupOrder preserves the Python dict's insertion order so that the
// "contains" fallback scans keys in the identical sequence.
var describeLookupOrder = []string{
	"ollama", "comfyui", "openwebui", "playwright", "searxng", "n8n", "letta",
	"litellm", "langflow", "langfuse", "traefik", "sablier", "authelia",
	"crowdsec", "portainer", "rancher", "gitea", "nextcloud", "immich",
	"grafana", "prometheus", "loki", "pihole", "adguard", "technitium",
	"wazuh", "vaultwarden", "jellyfin", "postgres", "mariadb", "redis",
	"mongodb", "neo4j", "qdrant", "surrealdb", "minio", "netbird", "tailscale",
	"cloudflared", "pangolin", "wazuhindexer", "wazuhmanager", "wazuhdashboard",
	"generator", "pangolinclient", "gerbil", "errorpages", "authentik",
	"jellyseerr", "zoraxy", "openresty", "defectdojo", "voidauth", "dockhand",
	"speaches", "whisper", "terminalagent", "opennotebook", "browserless",
	"gooseagent", "tabby", "hermes", "zep", "memos", "supabase", "librechat",
	"exo", "dockmate", "glance", "coolify", "dokploy", "pterodactyl", "penpot",
	"appsmith", "tooljet", "syncthing", "invidious", "mealie", "tandoor",
	"homeassistant", "nodered", "mosquitto", "dify", "windmill", "netdata",
	"komodo", "beszel", "dozzle", "ntopng", "headscale", "headplane", "clamav",
	"odoo", "dolibarr", "gamevault", "romm", "webtop", "scrutiny", "duplicati",
	"borgmatic", "provisioner", "trivy", "redroid", "dokku",
}

// describeStripChars removes the chars the Python strips for name matching.
func describeStripChars(s string, chars ...string) string {
	for _, c := range chars {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// getFallback is a faithful port of get_fallback(name, image="").
func describeGetFallback(name, image string) string {
	n := describeStripChars(strings.ToLower(name), "-", "_", ".")
	// image.lower().split("/")[-1].split(":")[0].replace("-","").replace("_","")
	imgLower := strings.ToLower(image)
	slashParts := strings.Split(imgLower, "/")
	img := slashParts[len(slashParts)-1]
	img = strings.Split(img, ":")[0]
	img = describeStripChars(img, "-", "_")

	for _, key := range describeLookupOrder {
		k := describeStripChars(key, "-", "_")
		if k == n || k == img {
			return describeLookup[key]
		}
	}
	for _, key := range describeLookupOrder {
		k := describeStripChars(key, "-", "_")
		if strings.Contains(n, k) || strings.Contains(img, k) ||
			strings.Contains(k, n) || strings.Contains(k, img) {
			return describeLookup[key]
		}
	}
	return fmt.Sprintf("Self-hosted %s service container", name)
}

// describeLoadConf loads description conf file, returns map of {service: [lines]}
// plus the keys in Python dict insertion order (so the "find by service in any
// format" fallback in inject scans keys in the identical sequence as Python's
// `for k in conf_descs`). Faithful port of load_conf(stack_name).
func describeLoadConf(stackName string) (map[string][]string, []string) {
	confPath := filepath.Join(describeConfDir(), stackName+".conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return map[string][]string{}, nil
	}
	descs := map[string][]string{}
	var order []string
	// setDesc mimics Python dict assignment: a new key is appended to the order;
	// reassigning an existing key keeps its original position.
	setDesc := func(k string, v []string) {
		if _, exists := descs[k]; !exists {
			order = append(order, k)
		}
		descs[k] = v
	}
	currentSvc := ""
	hasSvc := false
	var currentLines []string

	for _, line := range describeReadLines(string(data)) {
		s := strings.TrimRight(line, " \t\r\n\v\f")
		// Skip blank lines between services
		if s == "" {
			if hasSvc && len(currentLines) > 0 {
				setDesc(currentSvc, currentLines)
				currentSvc = ""
				hasSvc = false
				currentLines = nil
			}
			continue
		}
		// Comment lines — collect as description content
		if strings.HasPrefix(s, "#") {
			if hasSvc {
				currentLines = append(currentLines, s)
			}
			continue
		}
		// Non-comment, non-blank — this is a service name
		if hasSvc && len(currentLines) > 0 {
			setDesc(currentSvc, currentLines)
		}
		currentSvc = strings.TrimSpace(s)
		hasSvc = true
		currentLines = nil
	}
	if hasSvc && len(currentLines) > 0 {
		setDesc(currentSvc, currentLines)
	}
	return descs, order
}

// describeService mirrors the Python service dict {'name','image','container_name'}.
type describeService struct {
	name          string
	image         string
	containerName string
}

var describeReSvcLine = regexp.MustCompile(`^  ([a-zA-Z0-9_.\-]+):\s*$`)
var describeReServices = regexp.MustCompile(`^services:`)
var describeReTopKey = regexp.MustCompile(`^[a-zA-Z]`)
var describeReImage = regexp.MustCompile(`^\s+image:\s+(.+)`)
var describeReAnchor = regexp.MustCompile(`^\s+(<<:|image:|container_name:)`)
var describeReDashes = regexp.MustCompile(`^\s+#\s*-{3,}`)

// describeParseServices is a faithful port of parse_services(path).
func describeParseServices(path string) []describeService {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var services []describeService
	inServices := false
	hasCurrent := false
	var current describeService

	for _, line := range describeReadLines(string(data)) {
		s := strings.TrimRight(line, " \t\r\n\v\f")
		if describeReServices.MatchString(s) {
			inServices = true
			continue
		}
		if describeReTopKey.MatchString(s) && !strings.HasPrefix(s, " ") && inServices {
			if hasCurrent {
				services = append(services, current)
			}
			inServices = false
			continue
		}
		if !inServices {
			continue
		}
		if m := describeReSvcLine.FindStringSubmatch(s); m != nil {
			if hasCurrent {
				services = append(services, current)
			}
			current = describeService{name: m[1], image: "", containerName: m[1]}
			hasCurrent = true
			continue
		}
		if hasCurrent {
			if im := describeReImage.FindStringSubmatch(s); im != nil {
				current.image = strings.TrimSpace(im[1])
			}
		}
	}
	if hasCurrent {
		services = append(services, current)
	}
	return services
}

// describeReadLines splits content like Python's iteration over file lines /
// readlines(), preserving lines. Python's open() iteration yields each line
// including its trailing newline; here we only need the text content, and since
// callers strip the right side, splitting on "\n" is sufficient for parsing.
// For the readlines() use in inject (which preserves the line text), we keep the
// original newline characters via describeReadLinesKeep.
func describeReadLines(content string) []string {
	if content == "" {
		return nil
	}
	// Split keeping behavior equivalent to iterating file lines.
	lines := strings.Split(content, "\n")
	// If the content ends with a newline, the final empty element is spurious
	// for line-iteration semantics (Python would not yield a trailing empty
	// "line" after a final newline).
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// describeReadLinesKeep returns the file's lines WITH their trailing newline
// characters preserved, faithful to Python's readlines().
func describeReadLinesKeep(content string) []string {
	if content == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			out = append(out, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		out = append(out, content[start:])
	}
	return out
}

// describeInjectDescriptions is a faithful port of inject_descriptions(path).
func describeInjectDescriptions(path string) {
	stackName := strings.ReplaceAll(filepath.Base(path), ".yml", "")
	services := describeParseServices(path)
	if len(services) == 0 {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}

	confDescs, confOrder := describeLoadConf(stackName)

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}
	lines := describeReadLinesKeep(string(data))
	var out []string
	svcNum := 0

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		s := strings.TrimRight(line, " \t\r\n\v\f")
		if m := describeReSvcLine.FindStringSubmatch(s); m != nil {
			svcName := m[1]
			// Check it's a real service
			isService := false
			end := i + 6
			if end > len(lines) {
				end = len(lines)
			}
			for j := i + 1; j < end; j++ {
				if describeReAnchor.MatchString(lines[j]) {
					isService = true
					break
				}
			}

			if isService {
				// Remove any existing desc block from end of out.
				// Python: while out and out[-1].strip().startswith('#  #') or
				//         (out and re.match(r'\s+#\s*-{3,}', out[-1])):
				// Operator precedence: (A and B) or (C and D).
				for {
					condA := len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#  #")
					condCD := len(out) > 0 && describeReDashes.MatchString(out[len(out)-1])
					if condA || condCD {
						out = out[:len(out)-1]
						continue
					}
					break
				}
				// Also remove the last block of # lines
				for len(out) > 0 && strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") {
					last := strings.TrimSpace(out[len(out)-1])
					if strings.Contains(last, "---") || strings.Contains(last, "Description:") ||
						strings.Contains(last, "🐳") || strings.Contains(last, "✅") {
						out = out[:len(out)-1]
					} else {
						break
					}
				}

				svcNum++
				// Get description
				var descLines []string
				if dl, ok := confDescs[svcName]; ok {
					descLines = dl
				} else {
					// Find by service in any format
					found := ""
					foundOk := false
					for _, k := range confOrder {
						if describeStripChars(strings.ToLower(k), "-", "_") ==
							describeStripChars(strings.ToLower(svcName), "-", "_") {
							found = k
							foundOk = true
							break
						}
					}
					if foundOk {
						descLines = confDescs[found]
					} else {
						// Fallback to lookup
						img := ""
						for _, sv := range services {
							if sv.name == svcName {
								img = sv.image
								break
							}
						}
						descLines = []string{"# " + describeGetFallback(svcName, img)}
					}
				}

				// Build description block
				display := strings.ReplaceAll(strings.ReplaceAll(strings.ToUpper(svcName), "-", " "), "_", " ")
				block := "  # ---------------------------------------------------------\n"
				block += fmt.Sprintf("  # %02d. %s 🐳\n", svcNum, display)
				for _, dl := range descLines {
					dl = strings.TrimSpace(dl)
					if !strings.HasPrefix(dl, "#") {
						dl = "# " + dl
					}
					block += "  " + dl + "\n"
				}
				block += "  # ---------------------------------------------------------\n"
				out = append(out, block)
			}
		}

		out = append(out, line)
	}

	if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0644); err != nil {
		fmt.Printf("  No services in %s\n", stackName)
		return
	}
	fmt.Printf("  ✔ %s — %d services described\n", stackName, svcNum)
}

// describeMain is a faithful port of the __main__ block.
func describeMain(args []string) {
	target := "all"
	if len(args) > 0 {
		target = args[0]
	}

	var files []string
	if target == "all" || target == "--all" {
		entries, err := os.ReadDir(stacksDir())
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".yml") {
					files = append(files, filepath.Join(stacksDir(), e.Name()))
				}
			}
			sort.Strings(files)
		}
	} else if describeIsFile(target) {
		files = []string{target}
	} else if describeIsFile(filepath.Join(stacksDir(), target+".yml")) {
		files = []string{filepath.Join(stacksDir(), target+".yml")}
	} else {
		files = []string{filepath.Join(stacksDir(), target)}
	}

	fmt.Printf("\n\033[1;35m📝 Injecting service descriptions...\033[0m\n")
	for _, f := range files {
		if describeIsFile(f) {
			describeInjectDescriptions(f)
		}
	}
	fmt.Printf("\n\033[1;32m✔ Done\033[0m\n")
}

// describeIsFile mirrors os.path.isfile (true only for regular files).
func describeIsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// ===== from logs.go =====

// logs.go — central log location + per-container log dumping.
//
// All stacks logs live in ONE folder (default <dataDir>/logs), configurable via
// the `logs_folder` setting (STACKS_LOG_DIR). Besides the engine logs
// (stacks_*.log), `stacks logdump` (and the menu, on open) writes one
// <container>.log per running container here so every service has its own log.

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
