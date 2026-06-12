package lib

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
)

// ===== from docker.go =====

// docker.go — faithful Go port of stacks_docker.py: the single shared Docker
// access layer for every command. API-FIRST (Engine API via the compiled-in SDK)
// WITH CLI FALLBACK (shell out to `docker`) when STACKS_FORCE_CLI=1 or the API
// can't be reached — so it behaves like the Python on any install.
//
// Mirrors: env, cli, client, api_mode/available, container_state_map,
// container_info, containers, exists, remove_container, start, stop, state,
// inspect, is_running, is_unhealthy, networks, network_table, remove_network,
// running_names, image_inspect, volumes, images.

var (
	_cli      *client.Client
	_apiOK    *bool
	dockerCtx = context.Background()
)

// dockerEnv mirrors _env(): ensure DOCKER_HOST has a default for the CLI fallback.
func dockerEnv() []string {
	e := os.Environ()
	if os.Getenv("DOCKER_HOST") == "" {
		e = append(e, "DOCKER_HOST=unix:///var/run/docker.sock")
	}
	return e
}

// cliResult is a CLI fallback invocation result (mirrors subprocess.run).
type cliResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// cli mirrors _cli(): run `docker <args>` with a 60s timeout.
func cli(args ...string) cliResult {
	ctx, cancel := context.WithTimeout(dockerCtx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = dockerEnv()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	rc := 0
	if err != nil {
		rc = 1
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
	}
	return cliResult{out.String(), errb.String(), rc}
}

// dockerClient mirrors client(): cached client that negotiates the API version.
func dockerClient() *client.Client {
	if _cli == nil {
		c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			panic(err)
		}
		_cli = c
	}
	return _cli
}

// apiMode mirrors api_mode(): true if the Engine API can/should be used.
func apiMode() bool {
	if _apiOK == nil {
		v := true
		if os.Getenv("STACKS_FORCE_CLI") == "1" {
			v = false
		} else {
			ctx, cancel := context.WithTimeout(dockerCtx, 30*time.Second)
			defer cancel()
			if _, err := dockerClient().Ping(ctx); err != nil {
				v = false
			}
		}
		_apiOK = &v
	}
	return *_apiOK
}

func available() bool { return apiMode() }

// ── Containers: data ─────────────────────────────────────────────────────────

// rawContainers mirrors _raw_containers(): one ContainerList(all=true) call.
func rawContainers() []types.Container {
	list, err := dockerClient().ContainerList(dockerCtx, container.ListOptions{All: true})
	if err != nil {
		return nil
	}
	return list
}

// nameOf mirrors _name_of(): first name, leading slash stripped.
func nameOf(c types.Container) string {
	if len(c.Names) == 0 {
		return "?"
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// healthFromStatus mirrors _health_from_status().
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	default:
		return ""
	}
}

// containerStateMap mirrors container_state_map(): {name: state}.
func containerStateMap() map[string]string {
	out := map[string]string{}
	if apiMode() {
		for _, d := range rawContainers() {
			out[nameOf(d)] = d.State
		}
		return out
	}
	r := cli("ps", "-a", "--format", "{{.Names}}\t{{.State}}")
	for _, line := range strings.Split(r.stdout, "\n") {
		if n, s, ok := strings.Cut(line, "\t"); ok {
			out[n] = s
		}
	}
	return out
}

// ctrInfo mirrors the per-container dict from container_info().
type ctrInfo struct {
	State   string
	Health  string
	Project string
	Service string
	Image   string
}

// containerInfo mirrors container_info(): {name: {state,health,project,service,image}}.
func containerInfo() map[string]ctrInfo {
	out := map[string]ctrInfo{}
	if apiMode() {
		for _, d := range rawContainers() {
			out[nameOf(d)] = ctrInfo{
				State:   d.State,
				Health:  healthFromStatus(d.Status),
				Project: d.Labels["com.docker.compose.project"],
				Service: d.Labels["com.docker.compose.service"],
				Image:   d.Image,
			}
		}
		return out
	}
	format := "{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Label \"com.docker.compose.project\"}}\t" +
		"{{.Label \"com.docker.compose.service\"}}\t{{.Image}}"
	r := cli("ps", "-a", "--format", format)
	for _, line := range strings.Split(r.stdout, "\n") {
		p := strings.Split(line, "\t")
		if len(p) >= 6 {
			out[p[0]] = ctrInfo{State: p[1], Health: healthFromStatus(p[2]),
				Project: p[3], Service: p[4], Image: p[5]}
		}
	}
	return out
}

// containers mirrors containers(): live container objects (API).
func containers(all bool) []types.Container {
	list, err := dockerClient().ContainerList(dockerCtx, container.ListOptions{All: all})
	if err != nil {
		return nil
	}
	return list
}

// containerExists mirrors exists().
func containerExists(name string) bool {
	if apiMode() {
		_, err := dockerClient().ContainerInspect(dockerCtx, name)
		return err == nil
	}
	_, ok := containerStateMap()[name]
	return ok
}

// ── Containers: actions ──────────────────────────────────────────────────────

// removeContainer mirrors remove_container().
func removeContainer(name string, force, volumes bool) bool {
	if apiMode() {
		err := dockerClient().ContainerRemove(dockerCtx, name,
			container.RemoveOptions{Force: force, RemoveVolumes: volumes})
		return err == nil
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	if volumes {
		args = append(args, "-v")
	}
	args = append(args, name)
	return cli(args...).exitCode == 0
}

// startContainer mirrors start().
func startContainer(name string) bool {
	if apiMode() {
		return dockerClient().ContainerStart(dockerCtx, name, container.StartOptions{}) == nil
	}
	return cli("start", name).exitCode == 0
}

// stopContainer mirrors stop().
func stopContainer(name string, timeout int) bool {
	if apiMode() {
		return dockerClient().ContainerStop(dockerCtx, name, container.StopOptions{Timeout: &timeout}) == nil
	}
	return cli("stop", "-t", strconv.Itoa(timeout), name).exitCode == 0
}

// containerState mirrors state(): single container's status string or "".
func containerState(name string) string {
	if apiMode() {
		j, err := dockerClient().ContainerInspect(dockerCtx, name)
		if err != nil || j.State == nil {
			return ""
		}
		return j.State.Status
	}
	r := cli("inspect", "-f", "{{.State.Status}}", name)
	if r.exitCode == 0 {
		return strings.TrimSpace(r.stdout)
	}
	return ""
}

// containerInspect mirrors inspect(): full inspect as a generic map (or {} if absent).
func containerInspect(name string) map[string]interface{} {
	empty := map[string]interface{}{}
	if apiMode() {
		_, raw, err := dockerClient().ContainerInspectWithRaw(dockerCtx, name, false)
		if err != nil {
			return empty
		}
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			return empty
		}
		return m
	}
	r := cli("inspect", name)
	if r.exitCode != 0 || strings.TrimSpace(r.stdout) == "" {
		return empty
	}
	var arr []map[string]interface{}
	if json.Unmarshal([]byte(r.stdout), &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return empty
}

// isRunning mirrors is_running().
func isRunning(name string) bool { return containerStateMap()[name] == "running" }

// isUnhealthy mirrors is_unhealthy().
func isUnhealthy(name string) bool { return containerInfo()[name].Health == "unhealthy" }

// ── Networks ─────────────────────────────────────────────────────────────────

// netRow mirrors a row of network_table(): (id, name, container_count).
type netRow struct {
	ID    string
	Name  string
	Count int
}

// networks mirrors networks(): all network summaries (API).
func networks() []types.NetworkResource {
	list, err := dockerClient().NetworkList(dockerCtx, types.NetworkListOptions{})
	if err != nil {
		return nil
	}
	return list
}

// networkTable mirrors network_table(): [(id,name,count)], count -1 on inspect failure.
func networkTable() []netRow {
	var rows []netRow
	if apiMode() {
		for _, nd := range networks() {
			cnt := -1
			if ins, err := dockerClient().NetworkInspect(dockerCtx, nd.ID, types.NetworkInspectOptions{}); err == nil {
				cnt = len(ins.Containers)
			}
			rows = append(rows, netRow{nd.ID, nd.Name, cnt})
		}
		return rows
	}
	r := cli("network", "ls", "--format", "{{.ID}}\t{{.Name}}")
	for _, line := range strings.Split(r.stdout, "\n") {
		nid, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		cnt := -1
		ins := cli("network", "inspect", nid, "--format", "{{len .Containers}}")
		if v, err := strconv.Atoi(strings.TrimSpace(ins.stdout)); err == nil {
			cnt = v
		}
		rows = append(rows, netRow{nid, name, cnt})
	}
	return rows
}

// removeNetwork mirrors remove_network().
func removeNetwork(netID string) bool {
	if apiMode() {
		return dockerClient().NetworkRemove(dockerCtx, netID) == nil
	}
	return cli("network", "rm", netID).exitCode == 0
}

// runningNames mirrors running_names(): set of running container names.
func runningNames() map[string]bool {
	out := map[string]bool{}
	if apiMode() {
		for _, d := range containers(false) {
			out[nameOf(d)] = true
		}
		return out
	}
	r := cli("ps", "--format", "{{.Names}}")
	for _, ln := range strings.Split(r.stdout, "\n") {
		if ln != "" {
			out[ln] = true
		}
	}
	return out
}

// imageInspect mirrors image_inspect(): raw image inspect as a generic map (or {}).
func imageInspect(img string) map[string]interface{} {
	empty := map[string]interface{}{}
	if apiMode() {
		_, raw, err := dockerClient().ImageInspectWithRaw(dockerCtx, img)
		if err != nil {
			return empty
		}
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			return empty
		}
		return m
	}
	r := cli("inspect", "--type", "image", "--format", "{{json .}}", img)
	if r.exitCode != 0 || strings.TrimSpace(r.stdout) == "" {
		return empty
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(r.stdout), &m) != nil {
		return empty
	}
	return m
}

// dockerVolumes mirrors volumes(): all volumes (API).
func dockerVolumes() []*volume.Volume {
	resp, err := dockerClient().VolumeList(dockerCtx, volume.ListOptions{})
	if err != nil {
		return nil
	}
	return resp.Volumes
}

// dockerImages mirrors images(): all image summaries (API).
func dockerImages() []image.Summary {
	list, err := dockerClient().ImageList(dockerCtx, image.ListOptions{})
	if err != nil {
		return nil
	}
	return list
}

var _ = network.ListOptions{} // network pkg reserved for future inspect/create helpers

// ===== from sync.go =====

// syncDescDir is the per-user descriptions directory (CONF_DIR/descriptions).
func syncDescDir() string { return filepath.Join(configDir(), "descriptions") }

// syncSvcFile is the all_services.txt path (CONF_DIR/all_services.txt).
func syncSvcFile() string { return filepath.Join(configDir(), "all_services.txt") }

// syncSvc is one (service, image) pair parsed from a compose file.
type syncSvc struct {
	svc string
	img string
}

// getDefaultDesc mirrors get_default_desc(): prefer stacks_config.load()
// (YAML master, falling back to stacks.conf), then a direct stacks.conf read,
// then a hardcoded default.
func getDefaultDesc() string {
	// First try stacks_config.load().get("BUILD_DEFAULT_DESC").
	if v := configLoad()["BUILD_DEFAULT_DESC"]; v != "" {
		return v
	}
	// Then a direct stacks.conf read (Python's fallback loop).
	if v := confValue("BUILD_DEFAULT_DESC"); v != "" {
		return v
	}
	return "A powerful service running on BellzServer. Edit this description."
}

var (
	syncReSection = regexp.MustCompile(`^(networks|volumes|configs|secrets):`)
	syncReSvcKey  = regexp.MustCompile(`^  [a-zA-Z0-9_-]+:\s*$`)
)

// parseStack mirrors parse_stack(): get all services and images from a compose file.
func parseStack(fpath string) []syncSvc {
	var services []syncSvc
	data, err := os.ReadFile(fpath)
	if err != nil {
		return services
	}
	content := string(data)
	inServices := false
	currentSvc := ""
	currentImg := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "services:" {
			inServices = true
			continue
		}
		if inServices && syncReSection.MatchString(line) {
			inServices = false
			continue
		}
		if inServices && syncReSvcKey.MatchString(line) {
			if currentSvc != "" {
				services = append(services, syncSvc{currentSvc, currentImg})
			}
			currentSvc = strings.TrimSuffix(strings.TrimSpace(line), ":")
			currentImg = ""
		}
		if inServices && currentSvc != "" && strings.Contains(line, "image:") {
			parts := strings.SplitN(line, "image:", 2)
			currentImg = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
		}
		if inServices && currentSvc != "" && strings.Contains(line, "container_name:") {
			parts := strings.SplitN(line, "container_name:", 2)
			currentSvc = strings.TrimSpace(parts[1])
		}
	}
	if currentSvc != "" {
		services = append(services, syncSvc{currentSvc, currentImg})
	}
	return services
}

// syncDescriptions mirrors sync_descriptions(): add missing services, remove
// deleted ones from the descriptions file. Returns (added, removed).
func syncDescriptions(stackName string, services []syncSvc, defaultDesc string) (int, int) {
	os.MkdirAll(syncDescDir(), 0o755)
	descFile := filepath.Join(syncDescDir(), stackName+".conf")
	existing := ""
	if b, err := os.ReadFile(descFile); err == nil {
		existing = string(b)
	} else {
		existing = fmt.Sprintf("# %s — Service Descriptions\n# Edit the description under each service name.\n#\n", stackName)
	}

	// Build set of valid service names (normalize dash/underscore).
	valid := map[string]bool{}
	for _, s := range services {
		valid[s.svc] = true
		valid[strings.ReplaceAll(s.svc, "-", "_")] = true
		valid[strings.ReplaceAll(s.svc, "_", "-")] = true
	}

	// Parse existing file into blocks.
	// Header = lines before first service entry.
	// Each block = service name line + following lines.
	var headerLines []string
	blocks := map[string][]string{} // {svc_name: [lines]}
	var blockOrder []string         // preserve insertion order
	currentSvc := ""
	currentSet := false
	inHeader := true

	for _, line := range strings.Split(existing, "\n") {
		stripped := strings.TrimSpace(line)
		// Bare service name: non-empty, no "#", no ":", not "-".
		if stripped != "" && !strings.HasPrefix(stripped, "#") && !strings.Contains(stripped, ":") && !strings.HasPrefix(stripped, "-") {
			inHeader = false
			currentSvc = stripped
			currentSet = true
			if _, ok := blocks[currentSvc]; !ok {
				blockOrder = append(blockOrder, currentSvc)
			}
			blocks[currentSvc] = []string{}
		} else if inHeader {
			headerLines = append(headerLines, line)
		} else if currentSet {
			blocks[currentSvc] = append(blocks[currentSvc], line)
		}
	}

	// Rebuild: keep header, keep valid services, add missing ones.
	added, removed := 0, 0
	result := strings.TrimRight(strings.Join(headerLines, "\n"), "\n")

	for _, svcName := range blockOrder {
		svcLines := blocks[svcName]
		if valid[svcName] {
			result += "\n\n" + svcName + "\n" + strings.Trim(strings.Join(svcLines, "\n"), "\n")
		} else {
			removed++
		}
	}

	// Add missing services.
	for _, s := range services {
		svc := s.svc
		svcNorm := strings.ReplaceAll(svc, "-", "_")
		svcDash := strings.ReplaceAll(svc, "_", "-")
		_, ok1 := blocks[svc]
		_, ok2 := blocks[svcNorm]
		_, ok3 := blocks[svcDash]
		if !ok1 && !ok2 && !ok3 {
			result += "\n\n" + svc + "\n# " + defaultDesc
			added++
		}
	}

	result = strings.Trim(result, "\n") + "\n"

	if added != 0 || removed != 0 {
		os.WriteFile(descFile, []byte(result), 0o644)
	}

	return added, removed
}

// syncAllServices mirrors sync_all_services(): update all_services.txt - add new,
// remove deleted. Returns added.
func syncAllServices(stackName string, services []syncSvc) int {
	existing := ""
	if b, err := os.ReadFile(syncSvcFile()); err == nil {
		existing = string(b)
	} else {
		existing = "# ALL SERVICES — BellzServer\n# Format: stack | service | image\n# =========================================\n"
	}

	validNames := map[string]bool{}
	for _, s := range services {
		validNames[s.svc] = true
	}
	section := "# ── " + strings.ToUpper(stackName)
	lines := strings.Split(existing, "\n")
	var newLines []string
	added, removed := 0, 0

	for _, line := range lines {
		// Check if this is a service line for this stack.
		if strings.HasPrefix(line, stackName) && strings.Contains(line, "|") {
			rawParts := strings.Split(line, "|")
			parts := make([]string, len(rawParts))
			for i, p := range rawParts {
				parts[i] = strings.TrimSpace(p)
			}
			if len(parts) >= 2 {
				svc := strings.TrimSpace(parts[1])
				if validNames[svc] {
					newLines = append(newLines, line)
				} else {
					removed++
					continue
				}
			} else {
				newLines = append(newLines, line)
			}
		} else {
			newLines = append(newLines, line)
		}
	}
	_ = removed // matches Python: counted but not returned

	existing = strings.Join(newLines, "\n")

	// Add missing.
	for _, s := range services {
		svc, img := s.svc, s.img
		if !strings.Contains(existing, "| "+svc+" ") &&
			!strings.Contains(existing, "| "+svc+"\n") &&
			!strings.Contains(existing, "| "+svc) {
			entry := fmt.Sprintf("%-12s | %-35s | %s", stackName, svc, img)
			if strings.Contains(existing, section) {
				lines2 := strings.Split(existing, "\n")
				for i, l := range lines2 {
					if strings.HasPrefix(l, section) {
						lines2 = insertAt(lines2, i+1, entry)
						break
					}
				}
				existing = strings.Join(lines2, "\n")
			} else {
				existing += fmt.Sprintf("\n%s ──────────────────────────────────────\n%s\n", section, entry)
			}
			added++
		}
	}

	os.WriteFile(syncSvcFile(), []byte(existing), 0o644)
	return added
}

// syncMain mirrors main(): walk all *.yml in STACKS_DIR and sync.
func syncMain() {
	defaultDesc := getDefaultDesc()
	totalDesc := 0
	totalSvc := 0

	matches, _ := filepath.Glob(filepath.Join(stacksDir(), "*.yml"))
	sort.Strings(matches)
	for _, fpath := range matches {
		stackName := strings.TrimSuffix(filepath.Base(fpath), ".yml")
		services := parseStack(fpath)
		if len(services) == 0 {
			continue
		}
		addedD, removedD := syncDescriptions(stackName, services, defaultDesc)
		addedS := syncAllServices(stackName, services)
		totalDesc += addedD + removedD
		totalSvc += addedS
	}

	if totalDesc != 0 || totalSvc != 0 {
		fmt.Println("Sync complete: descriptions updated, all_services updated")
	}
}

// ===== from families.go =====

// families.go — faithful Go port of stacks_families.py (the Container Family
// Detector). 3 detection methods: common name root, direct prefix, shared
// private network + name match. Universal: uses stacksDir()/configDir() instead
// of the Python's hardcoded paths, but the detection logic is line-for-line.

// set helpers (Python sets -> map[string]bool)
func strset(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

var (
	famGlobalNets = strset("traefik_net", "apartment_net", "bridge", "host", "none",
		"ingress", "docker_gwbridge")
	famSkipContainers = strset("provisioner", "adminer", "surrealist", "cloudbeaver")
	famInfraSkip      = strset("traefik", "sablier", "crowdsec-bouncer", "error-pages")
	famDBWords        = strset("db", "redis", "cache", "postgres", "mysql", "mongo", "mariadb",
		"worker", "celery", "cron", "realtime", "beat", "scheduler",
		"daemon", "rabbitmq", "memcached", "valkey", "indexer")
	famNonFamilyRoots = strset("open", "agent", "cloudflared", "minecraft", "pritunl",
		"tailscale", "provisioner")
	famDBSubstrings = strset("postgres", "mysql", "mongo", "redis", "rabbitmq", "memcached")
)

// isSupport mirrors is_support(): a support/sidecar container (db, redis, worker…).
func isSupport(name string) bool {
	parts := strings.Split(strings.ReplaceAll(name, "_", "-"), "-")
	if famDBWords[parts[len(parts)-1]] {
		return true
	}
	for w := range famDBSubstrings {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// famRoot mirrors root(): first meaningful segment (authentik-server -> authentik).
func famRoot(name string) string {
	s := strings.ReplaceAll(name, "_", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return strings.Split(s, "-")[0]
}

// related mirrors related(): true if a and b are likely the same family.
func related(a, b string) bool {
	ra, rb := famRoot(a), famRoot(b)
	if famNonFamilyRoots[ra] || famNonFamilyRoots[rb] {
		return false
	}
	if ra == rb && len(ra) >= 3 {
		return true
	}
	s, lg := a, b
	if len(b) < len(a) {
		s, lg = b, a
	}
	return strings.HasPrefix(lg, s+"-") || strings.HasPrefix(lg, s+"_")
}

// famInfo mirrors the per-container dict from load_all().
type famInfo struct {
	file string
	nets map[string]bool
	ip   string
}

var (
	reContainerName = regexp.MustCompile(`container_name:\s*(\S+)`)
	reNextService   = regexp.MustCompile(`\n  [a-zA-Z][a-zA-Z0-9]`)
	reNet           = regexp.MustCompile(`(\w+_net)\s*:`)
	rePortIP        = regexp.MustCompile(`(192\.168\.1\.\d+):(\d+):\d+`)
)

// famStacksDir is overridable via get_families(stacks_dir); default = stacksDir().
var famStacksDir = ""

func familiesStacksDir() string {
	if famStacksDir != "" {
		return famStacksDir
	}
	return stacksDir()
}

// loadAll mirrors load_all(): scan every *.yml; returns ordered names + info map.
func loadAll() ([]string, map[string]famInfo) {
	order := []string{}
	containers := map[string]famInfo{}
	files, _ := filepath.Glob(filepath.Join(familiesStacksDir(), "*.yml"))
	sort.Strings(files)
	for _, fpath := range files {
		raw, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		data := string(raw)
		fname := filepath.Base(fpath)
		for _, m := range reContainerName.FindAllStringSubmatch(data, -1) {
			cname := strings.Trim(strings.TrimSpace(m[1]), `"'`)
			if cname == "" {
				continue
			}
			idx := strings.Index(data, "container_name: "+cname)
			if idx < 0 {
				continue
			}
			end := idx + 3000
			if end > len(data) {
				end = len(data)
			}
			block := data[idx:end]
			if len(block) > 10 {
				if nx := reNextService.FindStringIndex(block[10:]); nx != nil {
					block = block[:nx[0]+10]
				}
			}
			nets := map[string]bool{}
			for _, nm := range reNet.FindAllStringSubmatch(block, -1) {
				if !famGlobalNets[nm[1]] {
					nets[nm[1]] = true
				}
			}
			ip := ""
			if pm := rePortIP.FindStringSubmatch(block); pm != nil {
				ip = pm[1]
			}
			if _, seen := containers[cname]; !seen {
				order = append(order, cname)
			}
			containers[cname] = famInfo{file: fname, nets: nets, ip: ip}
		}
	}
	return order, containers
}

// buildFamilies mirrors build_families(): union-find over the 3 methods.
func buildFamilies(order []string, containers map[string]famInfo) map[string]map[string]bool {
	parent := map[string]string{}
	for _, c := range order {
		parent[c] = c
	}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		pa, pb := find(a), find(b)
		if pa == pb {
			return
		}
		if len(pa) <= len(pb) {
			parent[pb] = pa
		} else {
			parent[pa] = pb
		}
	}

	// Method 1: name-root / prefix match (primary)
	for i, c1 := range order {
		for _, c2 := range order[i+1:] {
			if related(c1, c2) {
				union(c1, c2)
			}
		}
	}

	// Method 2: shared private network + name confirmation
	netOrder := []string{}
	netMembers := map[string][]string{}
	for _, cname := range order {
		// nets iterated in sorted order for determinism (Python set order is arbitrary
		// but membership is what matters; union is commutative under name confirmation)
		nets := make([]string, 0, len(containers[cname].nets))
		for n := range containers[cname].nets {
			nets = append(nets, n)
		}
		sort.Strings(nets)
		for _, net := range nets {
			if _, ok := netMembers[net]; !ok {
				netOrder = append(netOrder, net)
			}
			netMembers[net] = append(netMembers[net], cname)
		}
	}
	for _, net := range netOrder {
		members := netMembers[net]
		if len(members) < 2 {
			continue
		}
		for i, c1 := range members {
			for _, c2 := range members[i+1:] {
				if related(c1, c2) {
					union(c1, c2)
				}
			}
		}
	}

	// Build groups
	groups := map[string]map[string]bool{}
	for _, c := range order {
		h := find(c)
		if groups[h] == nil {
			groups[h] = map[string]bool{}
		}
		groups[h][c] = true
	}

	// Filter + elect proper head
	result := map[string]map[string]bool{}
	for head, members := range groups {
		if len(members) < 2 {
			continue
		}
		skip := false
		for s := range famSkipContainers {
			if strings.Contains(head, s) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		var apps, supports []string
		for m := range members {
			if isSupport(m) {
				supports = append(supports, m)
			} else {
				apps = append(apps, m)
			}
		}
		if len(apps) == 0 {
			continue
		}
		if len(supports) == 0 {
			roots := map[string]bool{}
			for m := range members {
				roots[famRoot(m)] = true
			}
			if len(roots) > 1 {
				continue
			}
		}
		newHead := electHead(apps)
		if famInfraSkip[newHead] {
			continue
		}
		result[newHead] = members
	}
	return result
}

// electHead mirrors min(apps, key=head_score): smallest (len+penalty, name).
func electHead(apps []string) string {
	penalty := map[string]int{"indexer": 3, "dashboard": 2, "generator": 4,
		"certs": 4, "cert": 4, "worker": 3, "web": 1}
	score := func(n string) (int, string) {
		s := strings.ReplaceAll(n, ".", "-")
		parts := strings.Split(s, "-")
		last := parts[len(parts)-1]
		return len(n) + penalty[last], n
	}
	best := apps[0]
	bl, bn := score(best)
	for _, a := range apps[1:] {
		l, n := score(a)
		if l < bl || (l == bl && n < bn) {
			best, bl, bn = a, l, n
		}
	}
	return best
}

// loadFamilyWhitelist mirrors _load_family_whitelist(): families.yaml/.conf -> {member: head}.
func loadFamilyWhitelist() map[string]string {
	wl := map[string]string{}
	yp := filepath.Join(configDir(), "families.yaml")
	if y := loadNamed("families"); len(y) > 0 {
		_ = yp
		for m, h := range y {
			if strings.HasSuffix(h, "_net") {
				h = h[:len(h)-4]
			}
			if m != "" && h != "" {
				wl[m] = h
			}
		}
		if len(wl) > 0 {
			return wl
		}
	}
	cp := filepath.Join(configDir(), "families.conf")
	raw, err := os.ReadFile(cp)
	if err != nil {
		return wl
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		m, h, _ := strings.Cut(line, "=")
		m, h = strings.TrimSpace(m), strings.TrimSpace(h)
		if strings.HasSuffix(h, "_net") {
			h = h[:len(h)-4]
		}
		if m != "" && h != "" {
			wl[m] = h
		}
	}
	return wl
}

// getFamilies mirrors get_families(): {head: members}, with whitelist gap-fill.
func getFamilies(stacksDirArg string) map[string]map[string]bool {
	if stacksDirArg != "" {
		famStacksDir = stacksDirArg
	}
	order, containers := loadAll()
	fams := buildFamilies(order, containers)
	wl := loadFamilyWhitelist()
	if len(wl) > 0 {
		inReal := map[string]bool{}
		for _, mem := range fams {
			if len(mem) >= 2 {
				for m := range mem {
					inReal[m] = true
				}
			}
		}
		for member, head := range wl {
			if inReal[member] {
				continue
			}
			for h := range fams {
				if fams[h][member] && len(fams[h]) == 1 {
					delete(fams, h)
				}
			}
			if fams[head] == nil {
				fams[head] = map[string]bool{}
			}
			fams[head][head] = true
			fams[head][member] = true
		}
	}
	return fams
}

// getFamilyOf mirrors get_family_of().
func getFamilyOf(cname, stacksDirArg string) (string, map[string]bool) {
	for head, members := range getFamilies(stacksDirArg) {
		if members[cname] {
			return head, members
		}
	}
	return "", nil
}

// getFamilyHead mirrors get_family_head().
func getFamilyHead(cname, stacksDirArg string) string {
	h, _ := getFamilyOf(cname, stacksDirArg)
	return h
}

// familiesReport mirrors stacks_families.main(): the CONTAINER FAMILY REPORT.
func familiesReport() {
	order, containers := loadAll()
	families := buildFamilies(order, containers)
	allIn := map[string]bool{}
	for _, m := range families {
		for c := range m {
			allIn[c] = true
		}
	}
	type fam struct {
		head    string
		members map[string]bool
	}
	list := make([]fam, 0, len(families))
	for h, m := range families {
		list = append(list, fam{h, m})
	}
	// sort by (-len, head)
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].members) != len(list[j].members) {
			return len(list[i].members) > len(list[j].members)
		}
		return list[i].head < list[j].head
	})
	line := strings.Repeat("=", 65)
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  CONTAINER FAMILY REPORT")
	fmt.Println(line)
	fmt.Printf("  Total containers:        %d\n", len(containers))
	fmt.Printf("  Total families:          %d\n", len(list))
	fmt.Printf("  Containers in families:  %d\n", len(allIn))
	fmt.Printf("  Standalone containers:   %d\n", len(containers)-len(allIn))
	fmt.Println(line)
	for _, f := range list {
		other := make([]string, 0, len(f.members))
		for m := range f.members {
			if m != f.head {
				other = append(other, m)
			}
		}
		sort.Strings(other)
		fmt.Printf("\n  %s (%d containers)\n", f.head, len(f.members))
		for _, m := range other {
			fmt.Printf("    └─ %s\n", m)
		}
	}
}

// ===== from config.go =====

// config.go — universal paths/settings. NOTHING is hardcoded to one machine:
// every location comes from an env var, the per-user stacks.conf, or a generic
// XDG/home default. Works on any user's computer.

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "."
}

// configDir resolves the config dir generically for any user (faithful port of
// stacks_config._resolve_conf_dir). Priority: $STACKS_CONFIG_DIR → the invoking
// user's home under sudo ($SUDO_USER) → $XDG_CONFIG_HOME/stacks → ~/.config/stacks.
func configDir() string {
	if d := os.Getenv("STACKS_CONFIG_DIR"); d != "" {
		return expandUser(d)
	}
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return filepath.Join(u.HomeDir, ".config", "stacks")
		}
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "stacks")
	}
	return filepath.Join(home(), ".config", "stacks")
}

// expandUser is the Go equivalent of os.path.expanduser for a leading ~.
func expandUser(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

// confValue reads a KEY=VALUE from the per-user stacks.conf ("" if absent).
func confValue(key string) string {
	f, err := os.Open(filepath.Join(configDir(), "stacks.conf"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// stacksDir: $STACKS_DIR → conf STACKS_DIR → $STACKS_DATA_DIR/Stacks → ~/MyDocker/Stacks.
func stacksDir() string {
	if d := os.Getenv("STACKS_DIR"); d != "" {
		return d
	}
	if d := confValue("STACKS_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("STACKS_DATA_DIR"); d != "" {
		return filepath.Join(d, "Stacks")
	}
	return filepath.Join(home(), "MyDocker", "Stacks")
}

func isGitRepo(d string) bool {
	if d == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(d, ".git"))
	return err == nil && st.IsDir()
}

// repoDir: where the git clone lives, for the version string. $STACKS_REPO_DIR →
// conf → the running binary's own dir (if a git repo) → "" (caller shows "dev").
func repoDir() string {
	if d := os.Getenv("STACKS_REPO_DIR"); d != "" {
		return d
	}
	if d := confValue("STACKS_REPO_DIR"); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Dir(exe); isGitRepo(d) {
			return d
		}
	}
	return ""
}

// ===== from config_load.go =====

// config_load.go — faithful Go port of stacks_config.py's loader.
// Single source of truth = <configDir>/stacks.yaml (clean, human-friendly),
// falling back to the legacy stacks.conf if the YAML is missing/unreadable.
// Mirrors: SCALAR_MAP, LIST_MAP, _scalar, _from_conf, load, load_named,
// load_doc, yaml_set_scalar, yaml_get_list, yaml_set_list, and the --env/--check CLI.

func yamlPath() string { return filepath.Join(configDir(), "stacks.yaml") }
func confPath() string { return filepath.Join(configDir(), "stacks.conf") }

// listJoin pairs an internal key with the character used to join its items.
type listJoin struct {
	key  string
	join string
}

// SCALAR_MAP: friendly YAML key -> internal scalar key.
var scalarMap = map[string]string{
	"stacks_folder": "STACKS_DIR", "dynamics_folder": "DYNAMICS_DIR", "snapshots_folder": "SNAPSHOT_DIR",
	"info_log": "INFO_LOG", "volume_folder": "FIX_VOLUME_BASE", "delay_between_stacks": "DELAY",
	"master_network": "FIX_AUTO_NETWORKS", "authoritative_networks": "FIX_AUTHORITATIVE_NETWORKS",
	"link_family_networks": "FIX_AUTO_LINK_NETWORKS", "stack_wide_network": "FIX_AUTO_COMPOSE_NETWORK",
	"network_subnet_base": "FIX_SUBNET_BASE", "build_network": "PRIMARY_NETWORK", "build_subnet": "PRIMARY_SUBNET",
	"auto_name_containers": "FIX_AUTO_NAME_CONTAINERS", "sync_all_names": "FIX_SYNC_ALL_NAMES",
	"sync_dynamic_names": "FIX_SYNC_DYNAMICS_NAMES", "auto_name_stack_files": "FIX_AUTO_NAME",
	"convert_to_bind_mounts": "FIX_CONVERT_NAMED_TO_BIND", "force_volume_folder": "FIX_FORCE_VOLUME_BASE",
	"external_volumes": "FIX_EXTERNAL_VOLUMES", "remove_all_depends": "FIX_REMOVE_DEPENDS",
	"replace_broken_healthchecks": "FIX_REPLACE_BROKEN_HC", "remove_orphan_networks": "FIX_REMOVE_ORPHANS",
	"auto_image_search":            "FIX_AUTO_SEARCH",
	"reclaim_protect_stack_images": "RECLAIM_PROTECT_STACK_IMAGES",
	"deep_inspect":                 "FIX_DEEP_INSPECT", "backup_before_changes": "FIX_BACKUP",
	"auto_create_creator": "FIX_AUTO_CREATE_CREATOR", "creator_name": "FIX_CREATOR_NAME",
	"creator_max_networks": "FIX_CREATOR_MAX_NETWORKS", "creator_max_volumes": "FIX_CREATOR_MAX_VOLUMES",
	"force_new_creator": "FIX_FORCE_NEW_CREATOR", "ip_range_start": "IP_RANGE_START", "ip_range_end": "IP_RANGE_END",
	"warn_on_ip_collision": "IP_COLLISION_WARN", "autofix_ip_collisions": "IP_COLLISION_AUTOFIX",
	"skip_host_network_mode": "NETWORK_MODE_SKIP", "port_range_start": "PORT_RANGE_START", "port_range_end": "PORT_RANGE_END",
	"warn_on_port_collision": "PORT_COLLISION_WARN", "force_all_healthchecks": "FIX_FORCE_HC",
	"sablier_scaling": "SABLIER_SCALE_ENABLED", "auto_group_naming": "SCALE_AUTO_GROUP",
	"check_for_updates": "UPDATE_CHECK_ENABLED", "update_check_hours": "UPDATE_CHECK_INTERVAL",
	"update_running_only": "UPDATE_CHECK_RUNNING_ONLY", "auto_pull_updates": "UPDATE_AUTO_PULL",
	"notify_on_updates": "UPDATE_NOTIFY", "domain": "DOMAIN", "descriptions_file": "BUILD_DESC_FILE",
	"default_description": "BUILD_DEFAULT_DESC", "run_fix_after_build": "BUILD_RUN_FIX",
	"normalize_domains": "FIX_NORMALIZE_DOMAINS",
	// network priorities
	"master_network_priority":     "FIX_AUTO_NETWORK_PRIORITY",
	"family_network_priority":     "FIX_AUTO_LINK_PRIORITY",
	"stack_wide_network_priority": "FIX_AUTO_COMPOSE_NETWORK_PRIORITY",
	// fix behaviour
	"auto_depends_on": "FIX_AUTO_DEPENDS_ON", "create_volume_dirs": "FIX_CREATE_VOLUME_DIRS",
	"manage_dynamics": "FIX_DYNAMICS", "remove_blank_gaps": "FIX_REMOVE_GAPS", "strip_profiles": "FIX_STRIP_PROFILES",
	// anchor/service injection master toggles
	"inject_stop_grace": "INJECT_STOP_GRACE", "inject_logging": "INJECT_LOGGING",
	"inject_restart_policy": "INJECT_RESTART", "inject_resource_limits": "INJECT_DEPLOY",
	"inject_cpu_pinning": "INJECT_CPUSET", "inject_block_io": "INJECT_BLKIO", "inject_ulimits": "INJECT_ULIMITS",
	// dynamics generator (rich, config-driven) + DB entrypoint generation
	"rich_dynamics_generator": "GEN_RICH", "generate_db_entrypoints": "GEN_DB_ENTRYPOINTS",
	// base/location overrides (everything else derives from data_folder if unset)
	"data_folder": "STACKS_DATA_DIR", "logs_folder": "STACKS_LOG_DIR", "backup_folder": "BACKUP_DEST",
	// Zero Scale (our Sablier replacement) — master on/off; when off, the Zero Scale
	// options disappear from the Containers Tab popup.
	"zero_scale": "ZERO_SCALE",
	// per-tab visibility toggles (turn any tab off in the config)
	"tab_containers": "TAB_CONTAINERS", "tab_stacks": "TAB_STACKS", "tab_logs": "TAB_LOGS",
	"tab_dynamics": "TAB_DYNAMICS", "tab_art": "TAB_ART", "tab_backup": "TAB_BACKUP",
	"tab_network": "TAB_NETWORK", "tab_updates": "TAB_UPDATES", "tab_settings": "TAB_SETTINGS",
	// ── controlled boot bring-up (stacks up --boot) ───────────────────────────
	"boot_delay":            "BOOT_DELAY",            // seconds to wait after boot before starting
	"up_parallel":           "UP_PARALLEL",           // how many stacks to start at once (default 3)
	"start_strategy":        "START_STRATEGY",        // first pass: up | repair | recreate | fix
	"boot_escalate":         "BOOT_ESCALATE",         // climb the escalation chain if unhealthy
	"boot_force":            "BOOT_FORCE",            // run every step always (forced) vs only-if-needed
	"boot_download_missing": "BOOT_DOWNLOAD_MISSING", // pre-pull images for non-boot stacks, keep stopped
	"boot_override_docker":  "BOOT_OVERRIDE_DOCKER",  // disable Docker's own auto-start; we control startup
	// ── watchdog (stacks watch) ───────────────────────────────────────────────
	"watch_enabled":  "WATCH_ENABLED",  // run the 24/7 watchdog
	"watch_interval": "WATCH_INTERVAL", // seconds between health sweeps (default 30)
	"watch_strategy": "WATCH_STRATEGY", // what to do to a down service: up | repair | recreate | fix
	"watch_escalate": "WATCH_ESCALATE", // climb the escalation chain if it stays unhealthy
	"watch_force":    "WATCH_FORCE",    // always apply the chain vs only-if-needed
	// ── auto-discovery (no reliance on STACKS_DIR) ────────────────────────────
	"auto_detect_containers": "AUTO_DETECT_CONTAINERS", // show ALL running containers (Docker API), on by default
	"auto_detect_stacks":     "AUTO_DETECT_STACKS",     // auto-find compose stacks (Docker API labels)
	// ── Zero Scale ↔ Traefik integration ──────────────────────────────────────
	"auto_detect_traefik":    "AUTO_DETECT_TRAEFIK",    // detect whether Traefik is running (Docker API)
	"zero_scale_traefik_api": "ZERO_SCALE_TRAEFIK_API", // when Traefik is present, drive Zero Scale via its API
	"zero_scale_force":       "ZERO_SCALE_FORCE",       // show/use Zero Scale even if Sablier is installed (may conflict)
	// Zero Scale global engine settings (per-site overrides still live in zeroscale.yaml)
	"zero_scale_idle":          "ZERO_SCALE_IDLE",          // seconds idle before a site sleeps
	"zero_scale_poll":          "ZERO_SCALE_POLL",          // seconds between idle/metrics sweeps
	"zero_scale_default_screen": "ZERO_SCALE_DEFAULT_SCREEN", // default loading screen theme
	"zero_scale_listen":        "ZERO_SCALE_LISTEN",        // engine HTTP listen addr (e.g. :8787)
	"zero_scale_wake_base":     "ZERO_SCALE_WAKE_BASE",     // public wake URL base
	"zero_scale_metrics":       "ZERO_SCALE_METRICS",       // Traefik Prometheus metrics URL
	"zero_scale_stop_timeout":  "ZERO_SCALE_STOP_TIMEOUT",  // seconds to wait when stopping a sleeping container
	"zero_scale_show_logs":     "ZERO_SCALE_SHOW_LOGS",     // stream docker logs on the loading screen
	"zero_scale_log_lines":     "ZERO_SCALE_LOG_LINES",     // how many log lines to tail on the screen
	"zero_scale_autostop":      "ZERO_SCALE_AUTOSTOP",      // master switch for the idle-sleeper loop
}

// LIST_MAP: friendly YAML list key -> (internal key, join char).
var listMap = map[string]listJoin{
	"ip_blacklist": {"IP_BLACKLIST", ","}, "port_blacklist": {"PORT_BLACKLIST", ","},
	"locked_ips": {"LOCKED_IPS", ","}, "ip_port_locked": {"IP_PORT_LOCKED_CONTAINERS", ","},
	"skip_healthcheck": {"FIX_HC_SKIP", " "}, "update_skip_images": {"UPDATE_SKIP_IMAGES", " "},
	"domain_blacklist": {"DOMAIN_BLACKLIST", " "},
	// not currently read by code, but carried through for completeness:
	"never_sleep": {"NEVER_SLEEP", " "}, "never_rename": {"FIX_NEVER_RENAME", " "},
	"update_registries": {"UPDATE_REGISTRIES", " "}, "stack_order": {"STACK_ORDER", " "},
	"health_check_domains": {"HEALTH_CHECK_DOMAINS", " "},
	"ip_whitelist":         {"IP_WHITELIST", ","}, "port_whitelist": {"PORT_WHITELIST", ","},
	"proxy_skip": {"PROXY_SKIP_CONTAINERS", " "},
	"scale_skip": {"SCALE_SKIP_CONTAINERS", " "},
	// boot bring-up + watchdog selectable lists
	"boot_stacks":      {"BOOT_STACKS", " "},      // what starts at boot (stack or stack/service)
	"boot_escalation":  {"BOOT_ESCALATION", " "},  // ordered harder steps, e.g. recreate fix
	"watch_stacks":     {"WATCH_STACKS", " "},     // what the watchdog keeps alive (blank = boot_stacks)
	"watch_escalation": {"WATCH_ESCALATION", " "}, // ordered harder steps for the watchdog
}

// scalarStr mirrors _scalar(): bools become "1"/"0", everything else its string form.
func scalarStr(v interface{}) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "1"
		}
		return "0"
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// fromConf mirrors _from_conf(): parse the legacy KEY=VALUE stacks.conf.
func fromConf() map[string]string {
	cfg := map[string]string{}
	data, err := os.ReadFile(confPath())
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, val, _ := strings.Cut(line, "=")
		cfg[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return cfg
}

// configLoad mirrors load(): internal-key config from stacks.yaml (fallback conf).
func configLoad() map[string]string {
	if _, err := os.Stat(yamlPath()); err != nil {
		return fromConf()
	}
	data, err := os.ReadFile(yamlPath())
	if err != nil {
		return fromConf()
	}
	var y map[string]interface{}
	if err := yaml.Unmarshal(data, &y); err != nil {
		return fromConf()
	}
	cfg := map[string]string{}
	for fk, val := range y {
		if ik, ok := scalarMap[fk]; ok {
			cfg[ik] = scalarStr(val)
		} else if lj, ok := listMap[fk]; ok {
			var items []string
			if lst, ok := val.([]interface{}); ok {
				for _, x := range lst {
					items = append(items, scalarStr(x))
				}
			} else {
				items = append(items, scalarStr(val))
			}
			cfg[lj.key] = strings.Join(items, lj.join)
		}
	}
	return cfg
}

// loadNamed mirrors load_named(): any sibling <name>.yaml -> {KEY: 'string'}.
func loadNamed(name string) map[string]string {
	p := filepath.Join(configDir(), name+".yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		return map[string]string{}
	}
	var y map[string]interface{}
	if err := yaml.Unmarshal(data, &y); err != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	for k, v := range y {
		if lst, ok := v.([]interface{}); ok {
			parts := make([]string, 0, len(lst))
			for _, x := range lst {
				parts = append(parts, scalarStr(x))
			}
			out[k] = strings.Join(parts, " ")
		} else {
			out[k] = scalarStr(v)
		}
	}
	return out
}

// loadDoc mirrors load_doc(): prefer <base>.yaml, fall back to legacy <base>.conf (JSON).
func loadDoc(base string) map[string]interface{} {
	y := filepath.Join(configDir(), base+".yaml")
	if data, err := os.ReadFile(y); err == nil {
		var out map[string]interface{}
		if yaml.Unmarshal(data, &out) == nil {
			return out
		}
	}
	c := filepath.Join(configDir(), base+".conf")
	if data, err := os.ReadFile(c); err == nil {
		var out map[string]interface{}
		// JSON is a subset of YAML, so the YAML decoder reads the legacy .conf too.
		if yaml.Unmarshal(data, &out) == nil {
			return out
		}
	}
	return map[string]interface{}{}
}

// ── stacks.yaml editing (comment-preserving, top-level keys) ──────────────────

// yamlSetScalar mirrors yaml_set_scalar(): set/replace `key: value`, keep comments.
func yamlSetScalar(key, value string) bool {
	data, err := os.ReadFile(yamlPath())
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	pat := regexp.MustCompile("^" + regexp.QuoteMeta(key) + `:\s`)
	for i, l := range lines {
		if pat.MatchString(l) {
			lines[i] = fmt.Sprintf("%s: %s", key, value)
			return os.WriteFile(yamlPath(), []byte(strings.Join(lines, "\n")), 0644) == nil
		}
	}
	lines = append(lines, fmt.Sprintf("%s: %s", key, value))
	return os.WriteFile(yamlPath(), []byte(strings.Join(lines, "\n")), 0644) == nil
}

// yamlGetList mirrors yaml_get_list(): items under a top-level `key:` block.
func yamlGetList(key string) []string {
	data, err := os.ReadFile(yamlPath())
	if err != nil {
		return nil
	}
	head := regexp.MustCompile("^" + regexp.QuoteMeta(key) + `:\s*(\[\s*\])?\s*$`)
	item := regexp.MustCompile(`^\s+-\s+(.*\S)\s*$`)
	comment := regexp.MustCompile(`^\s*#`)
	var out []string
	inblk := false
	for _, l := range strings.Split(string(data), "\n") {
		if head.MatchString(l) {
			inblk = true
			continue
		}
		if inblk {
			if m := item.FindStringSubmatch(l); m != nil {
				out = append(out, strings.Trim(strings.TrimSpace(m[1]), `"'`))
			} else if comment.MatchString(l) {
				continue
			} else {
				break
			}
		}
	}
	return out
}

// yamlSetList mirrors yaml_set_list(): replace the list under `key:`, keep comments.
func yamlSetList(key string, items []string) bool {
	data, err := os.ReadFile(yamlPath())
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	head := regexp.MustCompile("^" + regexp.QuoteMeta(key) + `:\s*(\[\s*\])?\s*$`)
	dash := regexp.MustCompile(`^\s+-\s`)
	var out []string
	i, n, done := 0, len(lines), false
	for i < n {
		l := lines[i]
		if !done && head.MatchString(l) {
			if len(items) > 0 {
				out = append(out, key+":")
				for _, it := range items {
					out = append(out, "  - "+it)
				}
			} else {
				out = append(out, key+": []")
			}
			i++
			for i < n && dash.MatchString(lines[i]) {
				i++
			}
			done = true
			continue
		}
		out = append(out, l)
		i++
	}
	if !done {
		if len(items) > 0 {
			out = append(out, key+":")
			for _, it := range items {
				out = append(out, "  - "+it)
			}
		} else {
			out = append(out, key+": []")
		}
	}
	return os.WriteFile(yamlPath(), []byte(strings.Join(out, "\n")), 0644) == nil
}

// configEnv mirrors main()'s --env: print `export KEY='VALUE'` for bash.
func configEnv() {
	cfg := configLoad()
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("export %s=%s\n", k, shellQuote(cfg[k]))
	}
}

// configCheck mirrors main()'s --check: diff YAML-derived values vs stacks.conf.
func configCheck() {
	cfg := configLoad()
	conf := fromConf()
	var shared []string
	for k := range cfg {
		if _, ok := conf[k]; ok {
			shared = append(shared, k)
		}
	}
	sort.Strings(shared)
	type mm struct{ k, a, b string }
	var bad []mm
	for _, k := range shared {
		if cfg[k] != conf[k] {
			bad = append(bad, mm{k, cfg[k], conf[k]})
		}
	}
	fmt.Printf("keys from YAML: %d | shared with stacks.conf: %d | mismatches: %d\n", len(cfg), len(shared), len(bad))
	for _, b := range bad {
		fmt.Printf("  MISMATCH %s: yaml='%s'  conf='%s'\n", b.k, b.a, b.b)
	}
	var onlyConf []string
	for k := range conf {
		if _, ok := cfg[k]; !ok {
			onlyConf = append(onlyConf, k)
		}
	}
	if len(onlyConf) > 0 {
		sort.Strings(onlyConf)
		fmt.Printf("\nkeys ONLY in stacks.conf (not produced by YAML): %d\n", len(onlyConf))
		fmt.Println("  " + strings.Join(onlyConf, ", "))
	}
}

// shellQuote is the Go equivalent of shlex.quote for the --env output.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	if safe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
