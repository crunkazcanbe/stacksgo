package lib

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ===== from fix.go =====

// fix.go — faithful Go port of stacks_fix.py.
//
// The core "fix" / "recreate" / "repair" / "up" engine of the stacks tool.
// Auto-defines missing networks/volumes into the smallest creator file, heals
// creator-file typos, injects smart healthchecks, normalizes domains, renames
// containers, injects depends_on / global keys / cpuset, strips profiles, collapses
// gaps, manages bind-mount volumes, and auto-injects networks — driven entirely by
// the loaded config map.
//
// REUSE: this file deliberately reuses helpers already defined elsewhere:
//   - docker.go      : cli, containerInspect, containerState, startContainer,
//                      isUnhealthy, etc.
//   - config_load.go : configLoad()
//   - config.go      : stacksDir(), configDir(), home(), expandUser()
//   - families.go    : getFamilies(""), getFamilyOf("", "")
//   - netguardian.go : ngOn, ngConfGet, ngBackup, ngTopLevelBlockNames,
//                      ngDiscoverCreatorFiles, ngCollectServiceRefs,
//                      ngSmallestFileOverall, ngAllUsedSubnets, ngNextSubnetOctet,
//                      ngAddToCreator, ngFindProvisionerBlock, ngNetDefinition,
//                      ngVolDefinition, ngInsertAfterBlockHeader, ngSet,
//                      ngSortedKeys, ngSortedYmls, ngCreator
//   - repair.go      : repair_file() (Phase 0.5 corruption repair)
//   - proxyscale.go  : splitLines, insertAt, inList
//
// For `docker compose up/down` we shell out via os/exec (compose is NOT in the
// Engine API). Universal paths only — never hardcoded /home/<user>.

// ── ANSI colors (mirror the Python G/Y/R/C/M/X) ──────────────────────────────
const (
	fxG = "\033[1;32m"
	fxY = "\033[1;33m"
	fxR = "\033[1;31m"
	fxC = "\033[1;36m"
	fxM = "\033[1;35m"
	fxX = "\033[0m"
)

func fxpr(msg string) { fmt.Println(msg) }

// fxBackupDir mirrors stacks_fix.BACKUP_DIR with universal paths.
func fxBackupDir() string { return filepath.Join(configDir(), "fix-backups") }

// fxBackup — faithful port of stacks_fix._backup(): honour FIX_BACKUP, copy file
// into backup dir as <name>.bak-<unix-ts>. Errors swallowed.
func fxBackup(p string) {
	defer func() { recover() }()
	cfg := fxLoadConf()
	if !ngOn(ngConfGet(cfg, "FIX_BACKUP", "1")) {
		return
	}
	bdir := fxBackupDir()
	if os.MkdirAll(bdir, 0o755) != nil {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	dst := filepath.Join(bdir, filepath.Base(p)+fmt.Sprintf(".bak-%d", time.Now().Unix()))
	_ = os.WriteFile(dst, data, 0o644)
}

// fxOn is stacks_fix.on(): "0"/""/false/no -> false, else true.
func fxOn(v string) bool { return ngOn(v) }

// ── Config loader ────────────────────────────────────────────────────────────
// fxLoadConf — faithful port of stacks_fix.load_conf(). Starts from the same
// defaults table, overlays the YAML/conf via configLoad() (config_load.go already
// implements the stacks.yaml->internal-key mapping and stacks.conf fallback), then
// applies the FIX_DEPENDS master switch.
func fxLoadConf() map[string]string {
	cfg := map[string]string{
		"FIX_HEALTHCHECKS":           "0",
		"FIX_DEFINE_NETVOL":          "0",
		"FIX_HEAL_TYPOS":             "0",
		"FIX_DEEP_INSPECT":           "1",
		"FIX_SUBNET_BASE":            "10.50",
		"FIX_FORCE_NEW_CREATOR":      "0",
		"FIX_BACKUP":                 "1",
		"FIX_VOLUME_BASE":            filepath.Join(home(), "docker"),
		"FIX_VOLUME_CONTAINER_PATH":  "/config",
		"FIX_AUTO_BIND_MOUNTS":       "0",
		"FIX_AUTO_NAMED_VOLUMES":     "0",
		"FIX_CONVERT_NAMED_TO_BIND":  "0",
		"FIX_CREATE_VOLUME_DIRS":     "0",
		"FIX_AUTO_NETWORKS":          "",
		"FIX_AUTO_LINK_NETWORKS":     "0",
		"FIX_AUTHORITATIVE_NETWORKS": "1",
		"FIX_NORMALIZE_DOMAINS":      "0",
		"FIX_DOMAIN_BLACKLIST":       "",
		"FIX_FORCE_VOLUME_BASE":      "0",
		"FIX_AUTO_NAME_CONTAINERS":   "0",
		"FIX_SYNC_DYNAMICS_NAMES":    "0",
		"FIX_SYNC_ALL_NAMES":         "0",
		"FIX_REMOVE_GAPS":            "0",
		"FIX_HC_IGNORE_STACKS":       "",
		"FIX_REPLACE_BROKEN_HC":      "0",
		"FIX_FORCE_HC":               "0",
		"FIX_FORCE_HC_CONTAINERS":    "",
		"FIX_FORCE_NETWORKS":         "0",
		"FIX_FORCE_VOLUMES":          "0",
		"FIX_EXTERNAL_NETWORKS":      "1",
		"FIX_EXTERNAL_VOLUMES":       "1",
		"FIX_LOCAL_NETWORKS":         "0",
		"FIX_LOCAL_VOLUMES":          "0",
		"FIX_INLINE_NETWORKS":        "0",
		"FIX_INLINE_VOLUMES":         "0",
		"FIX_DEPENDS":                "off",
		"FIX_DEPENDS_INCLUDES":       "1",
		"FIX_AUTO_DEPENDS":           "0",
		"FIX_FORCE_DEPENDS":          "0",
		"FIX_STRIP_PROFILES":         "0",
		"FIX_SKIP_FILES":             "net_0-ext.yml",
		"FIX_HC_SKIP":                "",
		"STACKS_DIR":                 stacksDir(),
	}
	// Overlay YAML/conf-derived values (config_load.go handles the YAMLkey->FIX_*
	// mapping and the stacks.conf fallback — equivalent to load_conf's body).
	for k, v := range configLoad() {
		cfg[k] = v
	}
	if _, ok := cfg["STACKS_DIR"]; !ok || cfg["STACKS_DIR"] == "" {
		cfg["STACKS_DIR"] = stacksDir()
	}
	// FIX_DEPENDS master switch -> internal flags.
	fd := strings.ToLower(strings.TrimSpace(fxGet(cfg, "FIX_DEPENDS", "off")))
	switch fd {
	case "on", "1", "true", "yes":
		cfg["FIX_AUTO_DEPENDS"] = "1"
		cfg["FIX_REMOVE_DEPENDS"] = "0"
	case "off", "0", "false", "no":
		cfg["FIX_AUTO_DEPENDS"] = "0"
		cfg["FIX_REMOVE_DEPENDS"] = "1"
	}
	return cfg
}

func fxGet(cfg map[string]string, k, def string) string { return ngConfGet(cfg, k, def) }

// ── Healthcheck templates (image-name based) ──────────────────────────────────
type fxHC struct {
	pattern  *regexp.Regexp
	cmd      []string
	interval string
	timeout  string
	retries  int
	start    string
}

var fxHealthchecks = []fxHC{
	{regexp.MustCompile(`(?i)postgres|pgvecto|timescale`), []string{"CMD-SHELL", "pg_isready -U postgres || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)mariadb|mysql`), []string{"CMD-SHELL", "healthcheck.sh --connect --innodb_initialized || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)redis(?:.*insight)?`), []string{"CMD", "redis-cli", "ping"}, "10s", "3s", 10, "10s"},
	{regexp.MustCompile(`(?i)mongo`), []string{"CMD", "mongosh", "--quiet", "--eval", "db.adminCommand('ping').ok"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)elasticsearch|opensearch`), []string{"CMD-SHELL", "curl -sf http://localhost:9200/_cluster/health || exit 1"}, "30s", "10s", 5, "60s"},
	{regexp.MustCompile(`(?i)qdrant`), []string{"CMD-SHELL", "curl -sf http://localhost:6333/healthz || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)neo4j`), []string{"CMD-SHELL", "curl -sf http://localhost:7474 || exit 1"}, "15s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)influxdb`), []string{"CMD-SHELL", "curl -sf http://localhost:8086/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)couchdb`), []string{"CMD-SHELL", "curl -sf http://localhost:5984/_up || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)rabbitmq`), []string{"CMD", "rabbitmq-diagnostics", "ping"}, "15s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)minio`), []string{"CMD-SHELL", "curl -sf http://localhost:9000/minio/health/live || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)surrealdb|surreal`), []string{"CMD-SHELL", "curl -sf http://localhost:8000/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)traefik`), []string{"CMD", "traefik", "healthcheck"}, "10s", "5s", 5, "10s"},
	{regexp.MustCompile(`(?i)nginx-proxy-manager|jc21/nginx`), []string{"CMD-SHELL", "curl -sf http://localhost:81/api || exit 1"}, "15s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)nginx(?:.*proxy.*manager)?|openresty`), []string{"CMD-SHELL", "curl -sf http://localhost/ || exit 1"}, "10s", "5s", 5, "10s"},
	{regexp.MustCompile(`(?i)caddy`), []string{"CMD-SHELL", "caddy validate --config /etc/caddy/Caddyfile || exit 1"}, "10s", "5s", 5, "10s"},
	{regexp.MustCompile(`(?i)authelia`), []string{"CMD-SHELL", "wget -qO- http://localhost:9091/api/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)goauthentik.*server|authentik.*server`), []string{"CMD-SHELL", "ak healthcheck || exit 1"}, "10s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)vaultwarden|bitwarden`), []string{"CMD-SHELL", "curl -sf http://localhost:80/alive || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)crowdsec(?:.*bouncer)?`), []string{"CMD-SHELL", "cscli version || exit 1"}, "15s", "5s", 5, "30s"},
	{regexp.MustCompile(`(?i)grafana`), []string{"CMD-SHELL", "curl -sf http://localhost:3000/api/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)prometheus`), []string{"CMD-SHELL", "wget -qO- http://localhost:9090/-/healthy || exit 1"}, "10s", "5s", 5, "30s"},
	{regexp.MustCompile(`(?i)netdata`), []string{"CMD-SHELL", "curl -sf http://localhost:19999/api/v1/info || exit 1"}, "15s", "5s", 5, "30s"},
	{regexp.MustCompile(`(?i)uptime.kuma`), []string{"CMD-SHELL", "curl -sf http://localhost:3001 || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)wazuh.*dashboard`), []string{"CMD-SHELL", "curl -skf https://localhost:5601/api/status || exit 1"}, "30s", "10s", 10, "120s"},
	{regexp.MustCompile(`(?i)wazuh.*manager`), []string{"CMD-SHELL", "/var/ossec/bin/wazuh-control status | grep -q running || exit 1"}, "15s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)jellyfin`), []string{"CMD-SHELL", "curl -sf http://localhost:8096/health || exit 1"}, "15s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)immich.*server|immich.*microservices`), []string{"CMD-SHELL", "curl -sf http://localhost:3001/api/server-info/ping || exit 1"}, "10s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)nextcloud`), []string{"CMD-SHELL", "curl -sf http://localhost/status.php | grep -q ok || exit 1"}, "30s", "10s", 10, "120s"},
	{regexp.MustCompile(`(?i)gitea`), []string{"CMD-SHELL", "curl -sf http://localhost:3000/api/v1/version || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)portainer`), []string{"CMD-SHELL", "curl -sf https://localhost:9443/api/system/status || curl -sf http://localhost:9000/api/system/status || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)ollama`), []string{"CMD-SHELL", "curl -sf http://localhost:11434/api/version || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)open.webui|openwebui`), []string{"CMD-SHELL", "curl -sf http://localhost:8080/health || exit 1"}, "10s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)searxng`), []string{"CMD-SHELL", "curl -sf http://localhost:8080/ || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)litellm`), []string{"CMD-SHELL", "curl -sf http://localhost:4000/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)n8n`), []string{"CMD-SHELL", "curl -sf http://localhost:5678/healthz || exit 1"}, "10s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)netbird.*server`), []string{"CMD-SHELL", "curl -sf http://localhost:80/api/v1/setup-keys || exit 1"}, "15s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)adguard`), []string{"CMD-SHELL", "curl -sf http://localhost:3000 || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)pihole`), []string{"CMD-SHELL", "curl -sf http://localhost/admin/api.php || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)technitium`), []string{"CMD-SHELL", "curl -sf http://localhost:5380 || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)letta`), []string{"CMD-SHELL", "wget -qO- http://localhost:8283/v1/health || exit 1"}, "10s", "5s", 10, "60s"},
	{regexp.MustCompile(`(?i)speaches`), []string{"CMD-SHELL", "wget -qO- http://localhost:8000/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)whisper|faster.whisper`), []string{"CMD-SHELL", "wget -qO- http://localhost:8000/health || exit 1"}, "10s", "5s", 10, "30s"},
	{regexp.MustCompile(`(?i)playwright`), []string{"CMD-SHELL", "wget -qO- http://localhost:3000 || exit 1"}, "10s", "5s", 10, "30s"},
}

// PORT-NOTE: Python's mongo pattern is `mongo(?!.*express|.*compass)` and redis is
// `redis(?!.*insight)`. Go's RE2 has no lookahead. The mongo/redis/nginx/crowdsec
// patterns above approximate these by matching the base word; the negative-lookahead
// exclusions (mongo-express, redisinsight, nginx-proxy-manager handled by its own
// earlier entry, crowdsec-bouncer) are best-effort. nginx-proxy-manager has a
// dedicated earlier entry which wins, preserving correct behavior for that case.

var fxPortMap = map[string]string{
	"80": "http://localhost:80/", "81": "http://localhost:81/",
	"3000": "http://localhost:3000/", "3001": "http://localhost:3001/",
	"4000": "http://localhost:4000/", "5000": "http://localhost:5000/",
	"7860": "http://localhost:7860/", "8000": "http://localhost:8000/",
	"8080": "http://localhost:8080/", "8096": "http://localhost:8096/",
	"9000": "http://localhost:9000/", "9090": "http://localhost:9090/",
}

// hcResult mirrors the (cmd, interval, timeout, retries, start, source) tuple.
type hcResult struct {
	cmd      []string
	interval string
	timeout  string
	retries  int
	start    string
	source   string
}

var (
	fxRePortColon = regexp.MustCompile(`:(\d+):\d+`)
	fxRePortStart = regexp.MustCompile(`^(\d+):\d+`)
)

// fxHCFromPattern — faithful port of hc_from_pattern().
func fxHCFromPattern(image string, ports []string) hcResult {
	img := strings.Split(strings.ToLower(image), ":")[0]
	for _, h := range fxHealthchecks {
		if h.pattern.MatchString(img) {
			return hcResult{h.cmd, h.interval, h.timeout, h.retries, h.start, "pattern:" + h.pattern.String()}
		}
	}
	for _, p := range ports {
		var grp string
		if m := fxRePortColon.FindStringSubmatch(p); m != nil {
			grp = m[1]
		} else if m := fxRePortStart.FindStringSubmatch(p); m != nil {
			grp = m[1]
		}
		if grp != "" {
			if url, ok := fxPortMap[grp]; ok {
				return hcResult{[]string{"CMD-SHELL", "wget -qO- " + url + " || exit 1"}, "30s", "10s", 5, "60s", "port:" + grp}
			}
		}
	}
	return hcResult{[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/ || exit 1"}, "30s", "10s", 5, "60s", "generic"}
}

// ── Image healthcheck knowledge base ──────────────────────────────────────────
type fxImgHC struct {
	cmd      []string
	interval string
	timeout  string
	retries  int
	start    string
}

// fxImageHCKeys preserves insertion order for partial-match iteration.
var fxImageHCKeys = []string{
	"cloudflare/cloudflared", "thespad/traefik-crowdsec-bouncer", "crowdsecurity/cs-traefik-bouncer",
	"crowdsecurity/crowdsec", "tailscale/tailscale", "pihole/pihole", "adguard/adguardhome",
	"nginxproxy/nginx-proxy-manager", "jc21/nginx-proxy-manager", "technitium/dns-server",
	"fosrl/pangolin", "fosrl/gerbil", "netbirdio/management", "netbirdio/dashboard",
	"authelia/authelia", "acouvreur/sablier", "wazuh/wazuh-indexer", "wazuh/wazuh-manager",
	"wazuh/wazuh-dashboard", "portainer/portainer", "traefik", "dperson/openvpn-client",
	"qmcgaw/gluetun", "dperson/torproxy", "ghcr.io/goauthentik/server", "v2fly/v2fly-core",
	"nginx", "caddy", "headscale/headscale", "lscr.io/linuxserver/speedtest-tracker",
	"jellyfin/jellyfin", "jlesage/jdownloader-2", "juanfont/headscale", "lscr.io/linuxserver/jellyfin",
	"lscr.io/linuxserver/bazarr", "lscr.io/linuxserver/readarr", "lscr.io/linuxserver/lidarr",
	"lscr.io/linuxserver/radarr", "lscr.io/linuxserver/sonarr", "lscr.io/linuxserver/prowlarr",
	"lscr.io/linuxserver/jackett", "lscr.io/linuxserver/qbittorrent", "lscr.io/linuxserver/sabnzbd",
	"lscr.io/linuxserver/jdownloader-2", "cauliflower/speedtest-tracker", "alexjustesen/speedtest-tracker",
	"henrywhitaker3/speedtest-tracker", "containrrr/watchtower", "amir20/dozzle",
}

var fxImageHCDB = map[string]fxImgHC{
	"cloudflare/cloudflared":                {[]string{"CMD", "cloudflared", "version"}, "30s", "5s", 3, "10s"},
	"thespad/traefik-crowdsec-bouncer":      {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/api/v1/forwardAuth || exit 1"}, "15s", "5s", 5, "30s"},
	"crowdsecurity/cs-traefik-bouncer":      {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/api/v1/forwardAuth || exit 1"}, "15s", "5s", 5, "30s"},
	"crowdsecurity/crowdsec":                {[]string{"CMD-SHELL", "cscli version || exit 1"}, "30s", "5s", 3, "30s"},
	"tailscale/tailscale":                   {[]string{"CMD-SHELL", "tailscale status || exit 1"}, "30s", "5s", 3, "30s"},
	"pihole/pihole":                         {[]string{"CMD-SHELL", "dig +short +norecurse +retry=0 @127.0.0.1 pi.hole || exit 1"}, "30s", "10s", 3, "30s"},
	"adguard/adguardhome":                   {[]string{"CMD-SHELL", "wget -qO- http://localhost:3000 || exit 1"}, "30s", "5s", 3, "20s"},
	"nginxproxy/nginx-proxy-manager":        {[]string{"CMD-SHELL", "wget -qO- http://localhost:81/api || exit 1"}, "30s", "5s", 3, "30s"},
	"jc21/nginx-proxy-manager":              {[]string{"CMD-SHELL", "wget -qO- http://localhost:81/api || exit 1"}, "30s", "5s", 3, "30s"},
	"technitium/dns-server":                 {[]string{"CMD-SHELL", "curl -sf http://localhost:5380/ || exit 1"}, "30s", "5s", 3, "30s"},
	"fosrl/pangolin":                        {[]string{"CMD-SHELL", "wget -qO- http://localhost:3001/ || exit 1"}, "30s", "5s", 3, "30s"},
	"fosrl/gerbil":                          {[]string{"CMD-SHELL", "wget -qO- http://localhost:3003/ || exit 1"}, "30s", "5s", 3, "30s"},
	"netbirdio/management":                  {[]string{"CMD-SHELL", "wget -qO- http://localhost:80/ || exit 1"}, "30s", "5s", 3, "30s"},
	"netbirdio/dashboard":                   {[]string{"CMD-SHELL", "wget -qO- http://localhost:80/ || exit 1"}, "30s", "5s", 3, "20s"},
	"authelia/authelia":                     {[]string{"CMD-SHELL", "wget -qO- http://localhost:9091/api/health || exit 1"}, "30s", "5s", 3, "30s"},
	"acouvreur/sablier":                     {[]string{"CMD-SHELL", "wget -qO- http://localhost:10000/health || exit 1"}, "15s", "5s", 3, "10s"},
	"wazuh/wazuh-indexer":                   {[]string{"CMD-SHELL", "curl -sf http://localhost:9200/_cluster/health || exit 1"}, "30s", "10s", 5, "60s"},
	"wazuh/wazuh-manager":                   {[]string{"CMD-SHELL", "/var/ossec/bin/wazuh-control status || exit 1"}, "30s", "10s", 5, "60s"},
	"wazuh/wazuh-dashboard":                 {[]string{"CMD-SHELL", "curl -sf http://localhost:5601/api/status || exit 1"}, "30s", "10s", 5, "60s"},
	"portainer/portainer":                   {[]string{"CMD-SHELL", "wget -qO- http://localhost:9000/api/system/status || exit 1"}, "30s", "5s", 3, "20s"},
	"traefik":                               {[]string{"CMD-SHELL", "traefik healthcheck || exit 1"}, "10s", "5s", 3, "10s"},
	"dperson/openvpn-client":                {[]string{"CMD-SHELL", "ip addr show tun0 || exit 1"}, "30s", "5s", 3, "30s"},
	"qmcgaw/gluetun":                        {[]string{"CMD-SHELL", "wget -qO- http://localhost:8000/v1/vpn/status || exit 1"}, "30s", "5s", 3, "30s"},
	"dperson/torproxy":                      {[]string{"CMD-SHELL", "nc -z localhost 8118 || exit 1"}, "30s", "5s", 3, "30s"},
	"ghcr.io/goauthentik/server":            {[]string{"CMD-SHELL", "ak healthcheck || exit 1"}, "30s", "5s", 5, "30s"},
	"v2fly/v2fly-core":                      {[]string{"CMD-SHELL", "v2ray version || exit 1"}, "30s", "5s", 3, "10s"},
	"nginx":                                 {[]string{"CMD-SHELL", "nginx -t || exit 1"}, "30s", "5s", 3, "10s"},
	"caddy":                                 {[]string{"CMD-SHELL", "caddy validate --config /etc/caddy/Caddyfile || exit 1"}, "30s", "5s", 3, "10s"},
	"headscale/headscale":                   {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/health || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/speedtest-tracker": {[]string{"CMD-SHELL", "curl -sf http://localhost:80/ || exit 1"}, "60s", "10s", 3, "60s"},
	"jellyfin/jellyfin":                     {[]string{"CMD-SHELL", "curl -sf http://localhost:8096/health || exit 1"}, "30s", "10s", 3, "60s"},
	"jlesage/jdownloader-2":                 {[]string{"CMD-SHELL", "curl -sf http://localhost:5800/ || exit 1"}, "30s", "5s", 3, "60s"},
	"juanfont/headscale":                    {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/health || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/jellyfin":          {[]string{"CMD-SHELL", "curl -sf http://localhost:8096/health || exit 1"}, "30s", "10s", 3, "60s"},
	"lscr.io/linuxserver/bazarr":            {[]string{"CMD-SHELL", "curl -sf http://localhost:6767/api/ || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/readarr":           {[]string{"CMD-SHELL", "curl -sf http://localhost:8787/api/v1/system/status || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/lidarr":            {[]string{"CMD-SHELL", "curl -sf http://localhost:8686/api/v1/system/status || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/radarr":            {[]string{"CMD-SHELL", "curl -sf http://localhost:7878/api/v3/system/status || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/sonarr":            {[]string{"CMD-SHELL", "curl -sf http://localhost:8989/api/v3/system/status || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/prowlarr":          {[]string{"CMD-SHELL", "curl -sf http://localhost:9696/api/v1/system/status || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/jackett":           {[]string{"CMD-SHELL", "curl -sf http://localhost:9117/UI/Login || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/qbittorrent":       {[]string{"CMD-SHELL", "curl -sf http://localhost:8080/ || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/sabnzbd":           {[]string{"CMD-SHELL", "curl -sf http://localhost:8080/api?mode=version || exit 1"}, "30s", "5s", 3, "30s"},
	"lscr.io/linuxserver/jdownloader-2":     {[]string{"CMD-SHELL", "curl -sf http://localhost:5800/ || exit 1"}, "30s", "5s", 3, "60s"},
	"cauliflower/speedtest-tracker":         {[]string{"CMD-SHELL", "curl -sf http://localhost:80/ || exit 1"}, "60s", "10s", 3, "60s"},
	"alexjustesen/speedtest-tracker":        {[]string{"CMD-SHELL", "curl -sf http://localhost:80/ || exit 1"}, "60s", "10s", 3, "60s"},
	"henrywhitaker3/speedtest-tracker":      {[]string{"CMD-SHELL", "curl -sf http://localhost:80/ || exit 1"}, "60s", "10s", 3, "60s"},
	"containrrr/watchtower":                 {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/ || exit 1"}, "30s", "5s", 3, "30s"},
	"amir20/dozzle":                         {[]string{"CMD-SHELL", "wget -qO- http://localhost:8080/healthcheck || exit 1"}, "30s", "5s", 3, "20s"},
}

var fxReSSPort = regexp.MustCompile(`:(\d+)\s`)

// fxProbeContainer — faithful port of probe_container(): exec into a running
// container to find available tools + listening ports + main binary.
func fxProbeContainer(name string) (tools map[string]string, ports []int, mainBin string) {
	tools = map[string]string{}
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/busybox/sh", "/usr/bin/sh"} {
		r := cli("exec", name, shell, "-c", "echo ok")
		if r.exitCode == 0 {
			tools["shell"] = shell
			break
		}
	}
	if sh, ok := tools["shell"]; ok {
		for _, tool := range []string{"curl", "wget", "nc", "netcat", "ping", "ss", "netstat"} {
			r := cli("exec", name, sh, "-c", "which "+tool+" 2>/dev/null")
			if r.exitCode == 0 && strings.TrimSpace(r.stdout) != "" {
				tools[tool] = strings.TrimSpace(r.stdout)
			}
		}
	}
	if sh, ok := tools["shell"]; ok {
		for _, c := range []string{"ss -tlnp 2>/dev/null", "netstat -tlnp 2>/dev/null"} {
			r := cli("exec", name, sh, "-c", c)
			if r.exitCode == 0 {
				for _, line := range strings.Split(r.stdout, "\n") {
					if m := fxReSSPort.FindStringSubmatch(line); m != nil {
						if p, e := strconv.Atoi(m[1]); e == nil && p > 0 && p < 65536 && !fxIntIn(ports, p) {
							ports = append(ports, p)
						}
					}
				}
				if len(ports) > 0 {
					break
				}
			}
		}
	}
	// main binary from inspect's Config.Cmd[0]
	if ins := containerInspect(name); ins != nil {
		if cfgm, ok := ins["Config"].(map[string]interface{}); ok {
			if cmd, ok := cfgm["Cmd"].([]interface{}); ok && len(cmd) > 0 {
				if s, ok := cmd[0].(string); ok {
					mainBin = s
				}
			}
		}
	}
	sort.Ints(ports)
	return tools, ports, mainBin
}

func fxIntIn(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// fxHCFromInspect — faithful port of hc_from_inspect(). Returns (result, ok).
func fxHCFromInspect(container, image string) (hcResult, bool) {
	imgLower := strings.Split(strings.ToLower(image), ":")[0]
	imgBase := imgLower
	if strings.Contains(imgLower, "/") {
		parts := strings.Split(imgLower, "/")
		imgBase = parts[len(parts)-1]
	}
	for _, pattern := range fxImageHCKeys {
		patLower := strings.ToLower(pattern)
		if strings.Contains(imgLower, patLower) || patLower == imgBase {
			h := fxImageHCDB[pattern]
			return hcResult{h.cmd, h.interval, h.timeout, h.retries, h.start, "db:" + pattern}, true
		}
	}

	if containerState(container) != "running" {
		return hcResult{}, false
	}

	tools, ports, mainBin := fxProbeContainer(container)

	if _, hasShell := tools["shell"]; !hasShell {
		if mainBin != "" {
			return hcResult{[]string{"CMD", mainBin, "--version"}, "30s", "5s", 3, "10s", "probe:distroless"}, true
		}
		return hcResult{}, false
	}

	webSet := map[int]bool{80: true, 81: true, 443: true, 3000: true, 3001: true, 8000: true, 8080: true, 8443: true, 9000: true, 9090: true, 9091: true}
	var webPorts []int
	for _, p := range ports {
		if webSet[p] {
			webPorts = append(webPorts, p)
		}
	}
	if len(webPorts) > 0 {
		port := webPorts[0]
		if _, ok := tools["curl"]; ok {
			return hcResult{[]string{"CMD-SHELL", fmt.Sprintf("curl -sf http://localhost:%d/ || exit 1", port)}, "30s", "10s", 3, "30s", fmt.Sprintf("probe:curl-%d", port)}, true
		}
		if _, ok := tools["wget"]; ok {
			return hcResult{[]string{"CMD-SHELL", fmt.Sprintf("wget -qO- http://localhost:%d/ || exit 1", port)}, "30s", "10s", 3, "30s", fmt.Sprintf("probe:wget-%d", port)}, true
		}
	}
	if len(ports) > 0 {
		if _, ok := tools["nc"]; ok {
			port := ports[0]
			return hcResult{[]string{"CMD-SHELL", fmt.Sprintf("nc -z localhost %d || exit 1", port)}, "30s", "5s", 3, "20s", fmt.Sprintf("probe:nc-%d", port)}, true
		}
	}
	if _, ok := tools["curl"]; ok {
		return hcResult{[]string{"CMD-SHELL", "curl -sf http://localhost/ || exit 1"}, "30s", "5s", 3, "30s", "probe:curl-generic"}, true
	}
	if _, ok := tools["wget"]; ok {
		return hcResult{[]string{"CMD-SHELL", "wget -qO- http://localhost/ || exit 1"}, "30s", "5s", 3, "30s", "probe:wget-generic"}, true
	}
	return hcResult{}, false
}

// fxChooseHealthcheck — faithful port of choose_healthcheck().
func fxChooseHealthcheck(svc *fxService, deepInspect bool) hcResult {
	if deepInspect && svc.name != "" {
		if res, ok := fxHCFromInspect(svc.name, svc.image); ok {
			return res
		}
	}
	return fxHCFromPattern(svc.image, svc.ports)
}

// fxFormatHealthcheck — faithful port of format_healthcheck().
func fxFormatHealthcheck(cmd []string, interval, timeout string, retries int, start string) string {
	lines := []string{"    healthcheck:", "      test:"}
	for _, item := range cmd {
		lines = append(lines, fmt.Sprintf("        - %q", item))
	}
	lines = append(lines,
		fmt.Sprintf("      interval: %s", interval),
		fmt.Sprintf("      timeout: %s", timeout),
		fmt.Sprintf("      retries: %d", retries),
		fmt.Sprintf("      start_period: %s", start),
	)
	return strings.Join(lines, "\n") + "\n"
}

// ── Robust service parser ─────────────────────────────────────────────────────
type fxService struct {
	name           string
	image          string
	ports          []string
	hasHealthcheck bool
	blockStart     int
	blockEnd       int
}

var (
	fxReServicesHdr   = regexp.MustCompile(`^services:\s*$`)
	fxReTopLevelAlpha = regexp.MustCompile(`^[a-zA-Z]`)
	fxReSvcName       = regexp.MustCompile(`^  ([a-zA-Z0-9][a-zA-Z0-9_.\-]*):\s*$`)
	fxReImage         = regexp.MustCompile(`^\s+image:\s+(.+)`)
	fxRePortLine      = regexp.MustCompile(`^\s+-\s+"?(\S+:\d+:\d+)`)
	fxReHCKey         = regexp.MustCompile(`^#?\s*healthcheck\s*:`)
	fxReHCAnchor      = regexp.MustCompile(`\*[\w\-]*health`)
)

var fxAnchorKeys = map[string]bool{
	"cap_add": true, "sysctls": true, "tmpfs": true, "security_opt": true, "dns": true,
	"volumes": true, "networks": true, "ports": true, "environment": true, "labels": true,
	"devices": true, "ulimits": true, "logging": true, "deploy": true, "secrets": true,
	"configs": true, "build": true, "command": true, "entrypoint": true, "depends_on": true,
	"healthcheck": true, "restart": true, "image": true, "container_name": true,
}

// fxParseServicesWithPositions — faithful port of parse_services_with_positions().
// Returns the services and the file lines (each WITHOUT a trailing newline split via
// readlines-equivalent: here we keep newline-terminated lines like Python readlines()).
func fxParseServicesWithPositions(path string) ([]*fxService, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	lines := fxReadlines(string(data))
	var services []*fxService
	inServices := false
	var current *fxService

	for i, line := range lines {
		stripped := strings.TrimRight(line, " \t\r\n")

		if fxReServicesHdr.MatchString(stripped) {
			inServices = true
			continue
		}
		if inServices && fxReTopLevelAlpha.MatchString(stripped) && !strings.HasPrefix(stripped, " ") {
			if current != nil {
				current.blockEnd = i - 1
				services = append(services, current)
				current = nil
			}
			inServices = false
			continue
		}
		if !inServices {
			continue
		}

		m := fxReSvcName.FindStringSubmatch(stripped)
		if m != nil && !strings.HasPrefix(m[1], "x-") && !fxAnchorKeys[m[1]] {
			if current != nil {
				current.blockEnd = i - 1
				services = append(services, current)
			}
			current = &fxService{name: m[1], blockStart: i, blockEnd: len(lines) - 1}
			continue
		}

		if current != nil {
			if im := fxReImage.FindStringSubmatch(stripped); im != nil {
				current.image = strings.TrimSpace(im[1])
			}
			if pm := fxRePortLine.FindStringSubmatch(stripped); pm != nil {
				current.ports = append(current.ports, pm[1])
			}
			low := strings.TrimSpace(stripped)
			if fxReHCKey.MatchString(low) {
				current.hasHealthcheck = true
			}
			if strings.Contains(low, "healthcheck") && fxReHCAnchor.MatchString(low) {
				current.hasHealthcheck = true
			}
		}
	}
	if current != nil {
		services = append(services, current)
	}
	return services, lines, nil
}

// fxReadlines mirrors Python's open().readlines(): each element keeps its trailing
// "\n"; the final element omits "\n" only if the file didn't end with one.
func fxReadlines(s string) []string {
	if s == "" {
		return []string{}
	}
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

var (
	fxReHCStart    = regexp.MustCompile(`^\s*healthcheck:`)
	fxReSvcChildHi = regexp.MustCompile(`^  [a-zA-Z0-9]`)
	fxReLineAlpha  = regexp.MustCompile(`^[a-zA-Z0-9]`)
)

// fxReplaceHCInService — faithful port of replace_hc_in_service().
func fxReplaceHCInService(lines []string, svc *fxService, hc hcResult) ([]string, bool) {
	newHCText := fxFormatHealthcheck(hc.cmd, hc.interval, hc.timeout, hc.retries, hc.start)
	result := append([]string{}, lines...)
	inService := false
	hcStart, hcEnd := -1, -1
	for i, line := range lines {
		if i == svc.blockStart {
			inService = true
		}
		if inService && i > svc.blockStart {
			stripped := strings.TrimSpace(line)
			if fxReLineAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
				break
			}
			if fxReSvcChildHi.MatchString(line) && !strings.HasPrefix(line, "    ") {
				if hcStart != -1 {
					hcEnd = i
					break
				}
			}
			if strings.HasPrefix(stripped, "healthcheck:") {
				hcStart = i
			} else if hcStart != -1 && stripped != "" && !strings.HasPrefix(stripped, "healthcheck") {
				indent := len(line) - len(strings.TrimLeft(line, " "))
				if indent <= 4 {
					hcEnd = i
					break
				}
			}
		}
	}
	if hcStart == -1 {
		return lines, false
	}
	if hcEnd == -1 {
		hcEnd = svc.blockEnd + 1
	}
	repl := fxReadlines(newHCText + "\n")
	out := make([]string, 0, len(result))
	out = append(out, result[:hcStart]...)
	out = append(out, repl...)
	out = append(out, result[hcEnd:]...)
	return out, true
}

var fxReInjectAnchor = regexp.MustCompile(`^\s+(blkio_config|ulimits|deploy|storage_opt|logging):`)

// fxInjectHCIntoService — faithful port of inject_hc_into_service().
func fxInjectHCIntoService(lines []string, svc *fxService, deepInspect bool) ([]string, string) {
	hc := fxChooseHealthcheck(svc, deepInspect)
	hcText := fxFormatHealthcheck(hc.cmd, hc.interval, hc.timeout, hc.retries, hc.start)

	insertAfter := -1
	for i := svc.blockStart; i <= svc.blockEnd && i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " \t\r\n")
		if fxReInjectAnchor.MatchString(l) {
			insertAfter = i
			break
		}
	}
	if insertAfter == -1 {
		for i := svc.blockStart; i <= svc.blockEnd && i < len(lines); i++ {
			if regexp.MustCompile(`^\s+image:`).MatchString(lines[i]) {
				insertAfter = i + 1
				break
			}
		}
	}
	if insertAfter == -1 {
		insertAfter = svc.blockStart + 1
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAfter]...)
	out = append(out, hcText)
	out = append(out, lines[insertAfter:]...)
	return out, hc.source
}

// ── Creator/network/volume definition templates ──────────────────────────────
// fxNetDefinition / fxVolDefinition reuse netguardian's ng* helpers.

// ── Typo healing in creator files ─────────────────────────────────────────────
var fxReExternalTypo = regexp.MustCompile(`external:\s*([A-Za-z]+)`)

// fxLevenshtein — tiny Levenshtein (mirrors the nested near()).
func fxLevenshtein(a, b string) int {
	if abs(len(a)-len(b)) > 2 {
		return 99
	}
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := []int{i}
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur = append(cur, min3(prev[j]+1, cur[len(cur)-1]+1, prev[j-1]+cost))
		}
		prev = cur
	}
	return prev[len(prev)-1]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// fxHealCreatorTypos — faithful port of heal_creator_typos().
func fxHealCreatorTypos(path string, dryRun bool) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	content := string(data)
	original := content
	var fixes []string

	content = fxReExternalTypo.ReplaceAllStringFunc(content, func(s string) string {
		m := fxReExternalTypo.FindStringSubmatch(s)
		val := m[1]
		if val == "true" || val == "false" {
			return s
		}
		cand := "true"
		if fxLevenshtein(val, "false") < fxLevenshtein(val, "true") {
			cand = "false"
		}
		if fxLevenshtein(val, cand) <= 2 {
			fixes = append(fixes, fmt.Sprintf("external: %s -> %s", val, cand))
			return strings.Replace(s, val, cand, 1)
		}
		return s
	})

	if content != original && len(fixes) > 0 {
		if dryRun {
			fxpr(fmt.Sprintf("  %s[dry-run] would heal %s: %s%s", fxY, filepath.Base(path), strings.Join(fixes, "; "), fxX))
		} else {
			fxBackup(path)
			_ = os.WriteFile(path, []byte(content), 0o644)
			fxpr(fmt.Sprintf("  %s✔ healed %s: %s%s", fxG, filepath.Base(path), strings.Join(fixes, "; "), fxX))
		}
		return len(fixes)
	}
	return 0
}

// ── Healthcheck pass on one file ──────────────────────────────────────────────

// fxHCEnsureUp — faithful port of _hc_ensure_up(): bring the container up before
// probing so the scan sees real tools/ports.
func fxHCEnsureUp(name string, wait int) bool {
	st := containerState(name)
	if st == "running" {
		return true
	}
	if st == "" {
		return false
	}
	startContainer(name)
	for i := 0; i < wait; i++ {
		time.Sleep(1 * time.Second)
		st = containerState(name)
		if st == "running" {
			time.Sleep(3 * time.Second)
			return true
		}
	}
	return false
}

// fxHCRecreate — faithful port of _hc_recreate(): recreate ONE service via compose.
func fxHCRecreate(stackFile, svc, stacksDirPath string) {
	stack := strings.TrimSuffix(filepath.Base(stackFile), filepath.Ext(stackFile))
	cmd := exec.Command("docker", "compose", "-p", stack, "--project-directory", stacksDirPath,
		"-f", stackFile, "up", "-d", "--force-recreate", "--no-deps", svc)
	cmd.Env = dockerEnv()
	_ = cmd.Run()
}

// fxStateHealth pulls State.Health (Status, FailingStreak) out of an inspect map.
func fxStateHealth(name string) (status string, failing int) {
	ins := containerInspect(name)
	state, ok := ins["State"].(map[string]interface{})
	if !ok {
		return "", 0
	}
	h, ok := state["Health"].(map[string]interface{})
	if !ok {
		return "", 0
	}
	if s, ok := h["Status"].(string); ok {
		status = s
	}
	if f, ok := h["FailingStreak"].(float64); ok {
		failing = int(f)
	}
	return status, failing
}

var fxReAlpine = regexp.MustCompile(`^alpine(:|$)`)

// fxFixHealthchecks — faithful port of fix_healthchecks().
func fxFixHealthchecks(path string, cfg map[string]string, targetSvc string, dryRun, replaceBroken, forceHC bool) int {
	deep := fxOn(cfg["FIX_DEEP_INSPECT"])
	skip := ngSet(strings.Fields(cfg["FIX_HC_SKIP"]))
	services, lines, err := fxParseServicesWithPositions(path)
	if err != nil {
		return 0
	}
	if targetSvc != "" {
		var filtered []*fxService
		for _, s := range services {
			if s.name == targetSvc {
				filtered = append(filtered, s)
			}
		}
		services = filtered
	}

	changes := 0
	var recreate []string
	// reverse iteration keeps line numbers valid
	for i := len(services) - 1; i >= 0; i-- {
		svc := services[i]
		if svc.image == "" {
			continue
		}
		if strings.HasPrefix(svc.name, "provisioner") || fxReAlpine.MatchString(strings.TrimSpace(svc.image)) {
			fxpr(fmt.Sprintf("  %s  %s: idle holder, skipping%s", fxC, svc.name, fxX))
			continue
		}
		if skip[svc.name] {
			fxpr(fmt.Sprintf("  %s  %s: in skip-list, leaving alone%s", fxC, svc.name, fxX))
			continue
		}
		if forceHC && !dryRun && svc.name != "" {
			fxHCEnsureUp(svc.name, 15)
		}
		if svc.hasHealthcheck {
			replaced := false
			if (replaceBroken || forceHC) && svc.name != "" {
				hcStatus, failing := fxStateHealth(svc.name)
				if forceHC || hcStatus == "unhealthy" || failing > 0 {
					newHC, ok := fxHCFromInspect(svc.name, svc.image)
					if !ok {
						newHC = fxHCFromPattern(svc.image, svc.ports)
						ok = true
					}
					if ok {
						if dryRun {
							fxpr(fmt.Sprintf("  %s  [dry-run] re-stamp HC: %s (failing:%d) → %s%s", fxY, svc.name, failing, newHC.source, fxX))
							changes++
							replaced = true
						} else {
							lines2, changed := fxReplaceHCInService(lines, svc, newHC)
							if changed {
								lines = lines2
								fxpr(fmt.Sprintf("  %s  ✔ %s: HC re-stamped → %s%s", fxG, svc.name, newHC.source, fxX))
								changes++
								replaced = true
								recreate = append(recreate, svc.name)
							}
						}
					}
				}
			}
			if !replaced {
				fxpr(fmt.Sprintf("  %s  %s: already has healthcheck — NOT touched%s", fxC, svc.name, fxX))
			}
			continue
		}
		if dryRun {
			fxpr(fmt.Sprintf("  %s[dry-run] would add healthcheck to %s%s", fxY, svc.name, fxX))
			changes++
			continue
		}
		var source string
		lines, source = fxInjectHCIntoService(lines, svc, deep)
		changes++
		fxpr(fmt.Sprintf("  %s💉 %s: healthcheck added (%s)%s", fxG, svc.name, source, fxX))
		if forceHC {
			recreate = append(recreate, svc.name)
		}
	}

	if changes > 0 && !dryRun {
		fxBackup(path)
		_ = os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)
	}
	if forceHC && !dryRun && len(recreate) > 0 {
		sd := fxGet(cfg, "STACKS_DIR", filepath.Dir(path))
		if sd == "" {
			sd = filepath.Dir(path)
		}
		for _, s := range recreate {
			fxpr(fmt.Sprintf("  %s  ↻ recreating %s to apply its new healthcheck...%s", fxC, s, fxX))
			fxHCRecreate(path, s, sd)
		}
	}
	return changes
}

// ── Profiles / blank-line / gaps cleanup ──────────────────────────────────────

// fxStripProfilesFromFile — faithful port of strip_profiles_from_file().
func fxStripProfilesFromFile(filepath_ string, dryRun bool) bool {
	data, err := os.ReadFile(filepath_)
	if err != nil {
		return false
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	var result []string
	skipUntilDedent := -1
	for _, line := range lines {
		stripped := strings.TrimLeft(line, " ")
		indent := len(line) - len(stripped)
		if strings.HasPrefix(stripped, "profiles:") {
			skipUntilDedent = indent
			continue
		}
		if skipUntilDedent != -1 {
			if stripped == "" || indent > skipUntilDedent || strings.HasPrefix(stripped, "-") {
				continue
			}
			skipUntilDedent = -1
		}
		result = append(result, line)
	}
	newContent := strings.Join(result, "\n")
	if newContent != content {
		if !dryRun {
			fxBackup(filepath_)
			_ = os.WriteFile(filepath_, []byte(newContent), 0o644)
		}
		return true
	}
	return false
}

var (
	fxRe3Blank       = regexp.MustCompile(`\n{3,}`)
	fxReCollapsePrev = regexp.MustCompile(`^    (blkio_config|storage_opt|ulimits|deploy):`)
	fxReCollapseNext = regexp.MustCompile(`^  [a-zA-Z#]`)
)

// fxCollapseBlankLines — faithful port of collapse_blank_lines().
func fxCollapseBlankLines(filepath_ string, dryRun bool) bool {
	data, err := os.ReadFile(filepath_)
	if err != nil {
		return false
	}
	content := string(data)
	original := content
	content = strings.TrimLeft(content, "\n")
	content = fxRe3Blank.ReplaceAllString(content, "\n\n")
	lines := strings.Split(content, "\n")
	blankCount := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blankCount++
		}
	}
	total := len(lines)
	if total > 10 && float64(blankCount)/float64(total) > 0.05 {
		var result []string
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				prev := ""
				for j := i - 1; j >= 0; j-- {
					if strings.TrimSpace(lines[j]) != "" {
						prev = lines[j]
						break
					}
				}
				nxt := ""
				for j := i + 1; j < len(lines); j++ {
					if strings.TrimSpace(lines[j]) != "" {
						nxt = lines[j]
						break
					}
				}
				prevI := len(prev) - len(strings.TrimLeft(prev, " "))
				nxtI := len(nxt) - len(strings.TrimLeft(nxt, " "))
				if fxReCollapsePrev.MatchString(prev) && fxReCollapseNext.MatchString(nxt) {
					result = append(result, line)
				} else if prevI == 0 && nxtI == 0 && !strings.HasPrefix(prev, "#") && !strings.HasPrefix(nxt, "#") {
					result = append(result, line)
				}
			} else {
				result = append(result, line)
			}
		}
		content = strings.Join(result, "\n")
	}
	if content != original {
		if !dryRun {
			_ = os.WriteFile(filepath_, []byte(content), 0o644)
		}
		return true
	}
	return false
}

var (
	fxReBannerGap = regexp.MustCompile(`(?m)(^#.*$)\n\n(^#)`)
)

// fxRemoveGapsFromFile — faithful port of remove_gaps_from_file().
func fxRemoveGapsFromFile(filepath_ string, dryRun bool) bool {
	data, err := os.ReadFile(filepath_)
	if err != nil {
		return false
	}
	content := string(data)
	lines := strings.Split(content, "\n")
	var result []string
	inServiceBlock := false
	inServicesSection := false
	changed := false

	for i, line := range lines {
		stripped := strings.TrimSpace(line)
		if fxReServicesHdr.MatchString(line) {
			inServicesSection = true
			result = append(result, line)
			continue
		}
		if inServicesSection && fxReTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inServicesSection = false
		}
		if inServicesSection && fxReSvcChildHi.MatchString(line) {
			inServiceBlock = true
		}
		if inServiceBlock && stripped == "" {
			nextContent := ""
			for j := i + 1; j < i+5 && j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) != "" {
					nextContent = lines[j]
					break
				}
			}
			if nextContent != "" && fxReSvcChildHi.MatchString(nextContent) {
				result = append(result, line)
			} else if nextContent != "" && regexp.MustCompile(`^[a-zA-Z#]`).MatchString(nextContent) {
				result = append(result, line)
			} else {
				changed = true
				continue
			}
		} else {
			result = append(result, line)
		}
	}

	newContent := strings.Join(result, "\n")
	newContent2 := fxReBannerGap.ReplaceAllString(newContent, "$1\n$2")
	if newContent2 != newContent {
		newContent = newContent2
		changed = true
	}
	if changed {
		if !dryRun {
			fxBackup(filepath_)
			_ = os.WriteFile(filepath_, []byte(newContent), 0o644)
		}
		return true
	}
	return false
}

// ── Bind-mount volume directories ─────────────────────────────────────────────
var fxReVolumesHdr = regexp.MustCompile(`^volumes:\s*$`)

// fxGetBindMounts — faithful port of get_bind_mounts().
func fxGetBindMounts(svcBlockLines []string) []string {
	var mounts []string
	inVolumes := false
	for _, line := range svcBlockLines {
		stripped := strings.TrimSpace(line)
		if fxReVolumesHdr.MatchString(stripped) {
			inVolumes = true
			continue
		}
		if inVolumes {
			if strings.HasPrefix(stripped, "-") {
				val := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(stripped, "- "), "")), `"'`)
				if strings.HasPrefix(val, "/") {
					hostPath := strings.Split(val, ":")[0]
					mounts = append(mounts, hostPath)
				}
			} else if stripped != "" && !strings.HasPrefix(stripped, "#") {
				indent := len(line) - len(strings.TrimLeft(line, " "))
				if indent <= 4 {
					inVolumes = false
				}
			}
		}
	}
	return mounts
}

// fxCreateVolumeDirs — faithful port of create_volume_dirs().
func fxCreateVolumeDirs(paths []string, dryRun bool) []string {
	var created []string
	guard := []string{"/tmp", "/proc", "/sys", "/dev", "/run"}
	for _, path := range paths {
		skip := false
		for _, g := range guard {
			if strings.HasPrefix(path, g) {
				skip = true
				break
			}
		}
		if skip || !strings.HasPrefix(path, "/") {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if dryRun {
			created = append(created, "[dry-run] would create: "+path)
			continue
		}
		if err := os.MkdirAll(path, 0o755); err == nil {
			created = append(created, "created: "+path)
		} else if os.IsPermission(err) {
			r := exec.Command("sudo", "mkdir", "-p", path).Run()
			if r == nil {
				created = append(created, "created (sudo): "+path)
			} else {
				created = append(created, "failed (permission): "+path)
			}
		} else {
			created = append(created, fmt.Sprintf("failed: %s (%v)", path, err))
		}
	}
	return created
}

// ── Domain normalization ──────────────────────────────────────────────────────
var (
	fxReSvcStart      = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:\s*$`)
	fxReContainerName = regexp.MustCompile(`^\s*container_name:\s*(\S+)`)
	fxReDomainname    = regexp.MustCompile(`^\s*domainname:\s*(\S+)`)
	fxReNetworkMode   = regexp.MustCompile(`^\s*network_mode:\s*\S`)
	fxReHostnameLine  = regexp.MustCompile(`^(\s*)hostname:\s*(.*)$`)
	fxReDomainLine    = regexp.MustCompile(`^(\s*)domainname:\s*(.*)$`)
	fxReHostnameAny   = regexp.MustCompile(`^\s*hostname:\s*`)
	fxReDomainAny     = regexp.MustCompile(`^\s*domainname:\s*`)
)

// fxNormalizeHostDomain — faithful port of normalize_host_domain().
// Returns (newContent, count).
func fxNormalizeHostDomain(content, domain string, blacklist []string) (string, int) {
	lines := strings.Split(content, "\n")
	out := make([]*string, len(lines))
	for i := range lines {
		s := lines[i]
		out[i] = &s
	}
	n := 0
	N := len(lines)
	var svcStarts []int
	for idx, l := range lines {
		if fxReSvcStart.MatchString(l) {
			svcStarts = append(svcStarts, idx)
		}
	}
	svcStarts = append(svcStarts, N)
	for si := 0; si < len(svcStarts)-1; si++ {
		bStart, bEnd := svcStarts[si], svcStarts[si+1]
		block := lines[bStart:bEnd]
		cname := ""
		for _, l := range block {
			if m := fxReContainerName.FindStringSubmatch(l); m != nil {
				cname = strings.Trim(strings.TrimSpace(m[1]), `"'`)
				break
			}
		}
		if cname == "" {
			continue
		}
		curDomain := ""
		for _, l := range block {
			if m := fxReDomainname.FindStringSubmatch(l); m != nil {
				curDomain = strings.Trim(strings.TrimSpace(m[1]), `"'`)
				break
			}
		}
		if curDomain != "" {
			blacklisted := false
			for _, bd := range blacklist {
				if strings.HasSuffix(curDomain, bd) {
					blacklisted = true
					break
				}
			}
			if blacklisted {
				continue
			}
		}
		hasNetMode := false
		for _, l := range block {
			if fxReNetworkMode.MatchString(l) {
				hasNetMode = true
				break
			}
		}
		if hasNetMode {
			for j := bStart; j < bEnd; j++ {
				if fxReHostnameAny.MatchString(*out[j]) || fxReDomainAny.MatchString(*out[j]) {
					out[j] = nil
					n++
				}
			}
			continue
		}
		wantHost := cname
		wantDom := cname + "." + domain
		for j := bStart; j < bEnd; j++ {
			if out[j] == nil {
				continue
			}
			if hm := fxReHostnameLine.FindStringSubmatch(*out[j]); hm != nil {
				newl := hm[1] + "hostname: " + wantHost
				if newl != *out[j] {
					*out[j] = newl
					n++
				}
			} else if dm := fxReDomainLine.FindStringSubmatch(*out[j]); dm != nil {
				newl := dm[1] + "domainname: " + wantDom
				if newl != *out[j] {
					*out[j] = newl
					n++
				}
			}
		}
	}
	var kept []string
	for _, p := range out {
		if p != nil {
			kept = append(kept, *p)
		}
	}
	return strings.Join(kept, "\n"), n
}

// ── Container renaming ────────────────────────────────────────────────────────
var (
	fxReCnameVal     = regexp.MustCompile(`container_name:\s*(\S+)`)
	fxReVolMount     = regexp.MustCompile(`^\s*-\s+[A-Za-z0-9._-]+:/`)
	fxReDeclBrace    = regexp.MustCompile(`^  [A-Za-z0-9._-]+:\s*\{`)
	fxReRenameSuffix = regexp.MustCompile(`[_-](one|two|three|four|1|2|3|4)$`)
)

// fxApplyRenames — faithful port of apply_renames(). Returns per-file change counts.
func fxApplyRenames(stacksDirPath string, rmap map[string]string, dryRun bool) map[string]int {
	// sort by length desc so 'coolify-db' replaced before 'coolify'
	type pair struct{ old, new string }
	var pairs []pair
	for o, n := range rmap {
		pairs = append(pairs, pair{o, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].old) > len(pairs[j].old) })

	report := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(stacksDirPath, "*.yml"))
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		txt := string(data)
		orig := txt
		n := 0
		var outLines []string
		for _, line := range strings.Split(txt, "\n") {
			l := line
			st := strings.TrimSpace(l)
			isVolmount := fxReVolMount.MatchString(st)
			isDecl := fxReDeclBrace.MatchString(l) && (strings.Contains(l, "_data") || strings.Contains(l, "_net") || strings.Contains(l, "external"))
			if isVolmount || isDecl {
				outLines = append(outLines, l)
				continue
			}
			for _, p := range pairs {
				old, nw := p.old, p.new
				ctx := regexp.MustCompile(`^\s*container_name:\s*`).MatchString(l) ||
					regexp.MustCompile(`^\s*hostname:\s*`).MatchString(l) ||
					regexp.MustCompile(`^  `+regexp.QuoteMeta(old)+`:\s*$`).MatchString(l) ||
					strings.HasPrefix(st, "- "+old) || strings.HasPrefix(st, `- "`+old+`"`) || strings.HasPrefix(st, "- '"+old+"'") ||
					strings.Contains(l, "sablier") || strings.Contains(l, "names:") || strings.Contains(l, "names=") ||
					strings.Contains(l, "depends_on") ||
					strings.Contains(l, "@"+old) || strings.Contains(l, "//"+old) || strings.Contains(l, "('"+old+"'") || strings.Contains(l, "://"+old)
				if ctx {
					// PORT-NOTE: Python uses re.subn with a zero-width lookaround word
					// boundary. RE2 has no lookaround, so we capture the flanking boundary
					// chars and scan forward from a cursor (always terminates even if nw
					// contains old as a substring).
					pat := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(old) + `($|[^A-Za-z0-9_.-])`)
					var sb strings.Builder
					cursor := 0
					for cursor <= len(l) {
						loc := pat.FindStringSubmatchIndex(l[cursor:])
						if loc == nil {
							sb.WriteString(l[cursor:])
							break
						}
						g1 := l[cursor+loc[2] : cursor+loc[3]]
						g2 := l[cursor+loc[4] : cursor+loc[5]]
						sb.WriteString(l[cursor : cursor+loc[2]])
						sb.WriteString(g1)
						sb.WriteString(nw)
						sb.WriteString(g2)
						n++
						cursor += loc[5]
					}
					l = sb.String()
				}
			}
			outLines = append(outLines, l)
		}
		txt = strings.Join(outLines, "\n")
		if n > 0 && txt != orig {
			report[filepath.Base(f)] = n
			if !dryRun {
				fxCopy(f, f+fmt.Sprintf(".bak-rename-%d", time.Now().Unix()))
				_ = os.WriteFile(f, []byte(txt), 0o644)
			}
		}
	}
	return report
}

func fxCopy(src, dst string) {
	if data, err := os.ReadFile(src); err == nil {
		_ = os.WriteFile(dst, data, 0o644)
	}
}

var fxGUIWords = map[string]bool{"dashboard": true, "gui": true, "web": true, "frontend": true,
	"ui": true, "console": true, "studio": true, "portal": true, "panel": true, "admin": true}

// fxBuildRenameMap — faithful port of build_rename_map().
func fxBuildRenameMap(stacksDirPath string) map[string]string {
	var names []string
	files, _ := filepath.Glob(filepath.Join(stacksDirPath, "*.yml"))
	sort.Strings(files)
	for _, f := range files {
		data, _ := os.ReadFile(f)
		for _, m := range fxReCnameVal.FindAllStringSubmatch(string(data), -1) {
			names = append(names, strings.Trim(strings.TrimSpace(m[1]), `"'`))
		}
	}
	exclude := map[string]bool{"gerbil": true, "pangolin-client": true, "ak-outpost-traefik": true}
	fams := getFamilies("")
	lookup := map[string]map[string]bool{}
	lookupHead := map[string]string{}
	for h, mem := range fams {
		for m := range mem {
			lookup[m] = mem
			lookupHead[m] = h
		}
	}
	rmap := map[string]string{}
	seen := map[string]bool{}
	for _, cn := range names {
		if seen[cn] {
			continue
		}
		seen[cn] = true
		if exclude[cn] {
			continue
		}
		members := lookup[cn]
		head := lookupHead[cn]
		var nw string
		if head != "" && members != nil && len(members) >= 2 {
			root := strings.Split(strings.ReplaceAll(strings.ReplaceAll(head, ".", "-"), "_", "-"), "-")[0]
			clean := fxReRenameSuffix.ReplaceAllString(cn, "")
			parts := strings.Split(strings.ReplaceAll(strings.ReplaceAll(clean, ".", "-"), "_", "-"), "-")
			var role []string
			for _, x := range parts {
				if x != root {
					role = append(role, x)
				}
			}
			rootTaken := false
			for m := range members {
				if m == root || strings.ReplaceAll(strings.ReplaceAll(m, ".", "-"), "_", "-") == root {
					rootTaken = true
					break
				}
			}
			hasGUI := false
			for _, r := range role {
				if fxGUIWords[r] {
					hasGUI = true
					break
				}
			}
			if cn == root || len(role) == 0 {
				nw = root
			} else if hasGUI && !rootTaken {
				nw = root
			} else {
				nw = root + "_" + strings.Join(role, "_")
			}
		} else {
			nw = strings.NewReplacer("-", "", ".", "", "_", "").Replace(cn)
		}
		if nw != cn {
			rmap[cn] = nw
		}
	}
	return rmap
}

// fxRenameReport — faithful port of rename_report(): returns (rmap, collisions).
func fxRenameReport(stacksDirPath string) (map[string]string, map[string][]string) {
	rmap := fxBuildRenameMap(stacksDirPath)
	allFinal := map[string][]string{}
	seen := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(stacksDirPath, "*.yml"))
	sort.Strings(files)
	for _, f := range files {
		data, _ := os.ReadFile(f)
		for _, m := range fxReCnameVal.FindAllStringSubmatch(string(data), -1) {
			cn := strings.Trim(strings.TrimSpace(m[1]), `"'`)
			if seen[cn] {
				continue
			}
			seen[cn] = true
			fin := cn
			if v, ok := rmap[cn]; ok {
				fin = v
			}
			allFinal[fin] = append(allFinal[fin], cn)
		}
	}
	collisions := map[string][]string{}
	for k, v := range allFinal {
		if len(v) > 1 {
			collisions[k] = v
		}
	}
	return rmap, collisions
}

// ── depends_on injection ──────────────────────────────────────────────────────
var (
	fxReDepHdr     = regexp.MustCompile(`    depends_on:`)
	fxReDepEntry   = regexp.MustCompile(`      [-{]`)
	fxReImageLine4 = regexp.MustCompile(`    image:\s*`)
	fxReNameLine   = regexp.MustCompile(`name:\s*`)
	fxReIncludeHdr = regexp.MustCompile(`include:\s*$`)
)

// fxInjectDependsOn — faithful port of inject_depends_on().
func fxInjectDependsOn(fpath string, cfg map[string]string) []string {
	auto := fxGet(cfg, "FIX_AUTO_DEPENDS", "0") == "1"
	force := fxGet(cfg, "FIX_FORCE_DEPENDS", "0") == "1"
	removeAll := fxGet(cfg, "FIX_REMOVE_DEPENDS", "0") == "1"
	if !auto && !force && !removeAll {
		return nil
	}
	// Remove-only mode.
	if removeAll {
		data, err := os.ReadFile(fpath)
		if err != nil {
			return nil
		}
		lines := fxReadlines(string(data))
		var newLines []string
		inDep := false
		for _, l := range lines {
			if fxReDepHdr.MatchString(l) {
				inDep = true
				continue
			}
			if inDep {
				if fxReDepEntry.MatchString(l) {
					continue
				}
				inDep = false
			}
			newLines = append(newLines, l)
		}
		if len(newLines) != len(lines) {
			_ = os.WriteFile(fpath, []byte(strings.Join(newLines, "")), 0o644)
			return []string{"removed depends_on from " + filepath.Base(fpath)}
		}
		return nil
	}

	var notes []string
	dataB, err := os.ReadFile(fpath)
	if err != nil {
		return []string{fmt.Sprintf("depends_on error: %v", err)}
	}
	data := string(dataB)
	var cnames []string
	for _, m := range fxReCnameVal.FindAllStringSubmatch(data, -1) {
		cnames = append(cnames, strings.Trim(strings.TrimSpace(m[1]), `"'`))
	}
	if len(cnames) == 0 {
		return nil
	}
	lines := fxReadlines(data)

	// Pre-pass: strip depends_on from non-head family members (cycle-proof).
	strip := map[string]bool{}
	for _, cn := range cnames {
		h, _ := getFamilyOf(cn, "")
		if h != "" && h != cn {
			strip[cn] = true
		}
	}
	for cn := range strip {
		idx := strings.Index(data, "container_name: "+cn)
		if idx < 0 {
			continue
		}
		ln := strings.Count(data[:idx], "\n")
		var out []string
		ind := false
		for j, l := range lines {
			if !ind && ln <= j && j < ln+60 && fxReDepHdr.MatchString(l) {
				ind = true
				notes = append(notes, "stripped depends_on from "+cn)
				continue
			}
			if ind {
				if fxReDepEntry.MatchString(l) {
					continue
				}
				ind = false
			}
			out = append(out, l)
		}
		lines = out
		data = strings.Join(lines, "")
	}

	for _, cname := range cnames {
		head, members := getFamilyOf(cname, "")
		if head == "" || head != cname {
			continue
		}
		var deps []string
		for m := range members {
			if m != cname {
				deps = append(deps, m)
			}
		}
		sort.Strings(deps)
		if len(deps) == 0 {
			continue
		}
		idx := strings.Index(data, "container_name: "+cname)
		if idx < 0 {
			continue
		}
		lineNum := strings.Count(data[:idx], "\n")
		insertAfter := lineNum
		for j := lineNum; j < lineNum+15 && j < len(lines); j++ {
			if fxReImageLine4.MatchString(lines[j]) {
				insertAfter = j
				break
			}
		}
		block := strings.Join(fxSliceClamp(lines, lineNum, lineNum+60), "")
		hasDeps := strings.Contains(block, "depends_on:")
		if hasDeps && !force {
			continue
		}
		if hasDeps && force {
			var newLines []string
			inDep := false
			for j, l := range lines {
				if j < lineNum {
					newLines = append(newLines, l)
					continue
				}
				if strings.Contains(l, "depends_on:") && j > lineNum && j < lineNum+60 {
					inDep = true
					continue
				}
				if inDep {
					if fxReDepEntry.MatchString(l) {
						continue
					}
					inDep = false
				}
				newLines = append(newLines, l)
			}
			lines = newLines
			data = strings.Join(lines, "")
			lineNum = strings.Count(data[:strings.Index(data, "container_name: "+cname)], "\n")
			insertAfter = lineNum
			for j := lineNum; j < lineNum+15 && j < len(lines); j++ {
				if fxReImageLine4.MatchString(lines[j]) {
					insertAfter = j
					break
				}
			}
		}
		depLines := []string{"    depends_on:\n"}
		for _, d := range deps {
			depLines = append(depLines, "      - "+d+"\n")
		}
		out := make([]string, 0, len(lines)+len(depLines))
		out = append(out, lines[:insertAfter+1]...)
		out = append(out, depLines...)
		out = append(out, lines[insertAfter+1:]...)
		lines = out
		data = strings.Join(lines, "")
		notes = append(notes, fmt.Sprintf("depends_on: %s -> %v", cname, deps))
	}

	// Include injection for cross-stack family members.
	if fxOn(fxGet(cfg, "FIX_DEPENDS_INCLUDES", "1")) {
		data = strings.Join(lines, "")
		thisFile := filepath.Base(fpath)
		localCn := map[string]bool{}
		for _, m := range fxReCnameVal.FindAllStringSubmatch(data, -1) {
			localCn[strings.Trim(strings.TrimSpace(m[1]), `"'`)] = true
		}
		sd2 := filepath.Dir(fpath)
		neededFiles := map[string]bool{}
		for _, cname := range cnames {
			head, members := getFamilyOf(cname, "")
			if head == "" || head != cname {
				continue
			}
			for m := range members {
				if m == cname || localCn[m] {
					continue
				}
				for _, fn := range ngSortedYmls(sd2) {
					if fn == thisFile {
						continue
					}
					fdata, e := os.ReadFile(filepath.Join(sd2, fn))
					if e != nil {
						continue
					}
					if regexp.MustCompile(`container_name:\s*["']?` + regexp.QuoteMeta(m) + `["']?\s`).Match(fdata) {
						neededFiles[fn] = true
						break
					}
				}
			}
		}
		var safeInc []string
		for _, fn := range fxSortedSet(neededFiles) {
			tdata, e := os.ReadFile(filepath.Join(sd2, fn))
			if e != nil {
				continue
			}
			if regexp.MustCompile(`(?s)include:.*` + regexp.QuoteMeta(thisFile)).Match(tdata) {
				notes = append(notes, "include SKIPPED (would cycle): "+fn)
				continue
			}
			safeInc = append(safeInc, fn)
		}
		var toAdd []string
		for _, fn := range safeInc {
			if !strings.Contains(data, fn) {
				toAdd = append(toAdd, fn)
			}
		}
		if len(toAdd) > 0 {
			lines2 := fxReadlines(strings.Join(lines, ""))
			ins := 0
			for i, l := range lines2 {
				if fxReNameLine.MatchString(l) {
					ins = i + 1
					break
				}
			}
			if strings.Contains(data, "include:") {
				for i, l := range lines2 {
					if fxReIncludeHdr.MatchString(strings.TrimRight(l, "\n")) {
						var blk []string
						for _, fn := range toAdd {
							blk = append(blk, "  - "+sd2+"/"+fn+"\n")
						}
						out := make([]string, 0, len(lines2)+len(blk))
						out = append(out, lines2[:i+1]...)
						out = append(out, blk...)
						out = append(out, lines2[i+1:]...)
						lines2 = out
						break
					}
				}
			} else {
				blk := []string{"include:\n"}
				for _, fn := range toAdd {
					blk = append(blk, "  - "+sd2+"/"+fn+"\n")
				}
				out := make([]string, 0, len(lines2)+len(blk))
				out = append(out, lines2[:ins]...)
				out = append(out, blk...)
				out = append(out, lines2[ins:]...)
				lines2 = out
			}
			lines = lines2
			for _, fn := range toAdd {
				notes = append(notes, "include added: "+fn)
			}
		}
	}

	if len(notes) > 0 {
		_ = os.WriteFile(fpath, []byte(strings.Join(lines, "")), 0o644)
	}
	return notes
}

func fxSliceClamp(s []string, lo, hi int) []string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(s) {
		hi = len(s)
	}
	if lo > hi {
		return nil
	}
	return s[lo:hi]
}

func fxSortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── Authoritative network assignment ──────────────────────────────────────────
var (
	fxReSvcNetworks = regexp.MustCompile(`^    networks:\s*$`)
	fxReSixIndent   = regexp.MustCompile(`^      `)
	fxReSvcInsertK  = regexp.MustCompile(`^    (labels|healthcheck|deploy|volumes):`)
)

// fxSetNetworksAuthoritative — faithful port of set_networks_authoritative().
func fxSetNetworksAuthoritative(lines []string, svc *fxService, masterNet, famNet, stackNet string) ([]string, bool) {
	bs, be := svc.blockStart, svc.blockEnd
	blk := fxSliceClamp(lines, bs, be+1)
	for _, l := range blk {
		if strings.Contains(l, "network_mode:") {
			return lines, false
		}
	}
	want := []string{"    networks:", "      " + masterNet + ":", "        priority: 1000"}
	if famNet != "" && famNet != masterNet {
		want = append(want, "      "+famNet+":", "        priority: 500")
	}
	if stackNet != "" && stackNet != masterNet && stackNet != famNet {
		want = append(want, "      "+stackNet+":", "        priority: 200")
	}
	netLo, netHi := -1, -1
	for j := bs; j <= be && j < len(lines); j++ {
		if fxReSvcNetworks.MatchString(strings.TrimRight(lines[j], "\n")) {
			netLo = j
			k := j + 1
			for k <= be && k < len(lines) && fxReSixIndent.MatchString(strings.TrimRight(lines[k], "\n")) {
				k++
			}
			netHi = k - 1
			break
		}
	}
	newLines := append([]string{}, lines...)
	if netLo != -1 {
		existing := fxSliceClamp(lines, netLo, netHi+1)
		// strip trailing newlines for comparison since want has none
		var existingTrim []string
		for _, e := range existing {
			existingTrim = append(existingTrim, strings.TrimRight(e, "\n"))
		}
		if fxStrSliceEq(existingTrim, want) {
			return lines, false
		}
		out := make([]string, 0, len(newLines))
		out = append(out, newLines[:netLo]...)
		out = append(out, want...)
		out = append(out, newLines[netHi+1:]...)
		return out, true
	}
	ins := be + 1
	for k := bs; k < be+1 && k < len(lines); k++ {
		if fxReSvcInsertK.MatchString(strings.TrimRight(lines[k], "\n")) {
			ins = k
			break
		}
	}
	out := make([]string, 0, len(newLines)+len(want))
	out = append(out, newLines[:ins]...)
	out = append(out, want...)
	out = append(out, newLines[ins:]...)
	return out, true
}

func fxStrSliceEq(a, b []string) bool {
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

// fxEnsureNetworkDeclared — faithful port of ensure_network_declared().
func fxEnsureNetworkDeclared(lines []string, netName string) ([]string, bool) {
	declRe := regexp.MustCompile(`^  ` + regexp.QuoteMeta(netName) + `[\s:{]`)
	inNetworks := false
	for _, line := range lines {
		if regexp.MustCompile(`^networks:\s*$`).MatchString(line) {
			inNetworks = true
			continue
		}
		if inNetworks {
			if fxReTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
				inNetworks = false
				continue
			}
			if declRe.MatchString(strings.TrimRight(line, "\n")) {
				return lines, false
			}
		}
	}
	newEntry := fmt.Sprintf("  %s: {driver: bridge, external: false}", netName)
	newLines := append([]string{}, lines...)
	netSection := -1
	for i, line := range lines {
		if regexp.MustCompile(`^networks:\s*$`).MatchString(line) {
			netSection = i
			break
		}
	}
	if netSection != -1 {
		return insertAt(newLines, netSection+1, newEntry), true
	}
	for i, line := range newLines {
		if fxReServicesHdr.MatchString(line) {
			return insertAt(newLines, i, "networks:", newEntry), true
		}
	}
	newLines = append(newLines, "networks:", newEntry)
	return newLines, true
}

// ── Global key injection (load_global_inject_conf + injectors) ────────────────

// fxLoadGlobalInjectConf — faithful port of load_global_inject_conf().
// PORT-NOTE: Python reads ~loveiznothin/.config/stacks/global_inject.conf (a
// hardcoded user). This Go port uses the universal configDir() for the conf file
// and overlays loadNamed("global_inject") (YAML master), matching the intent.
func fxLoadGlobalInjectConf() map[string]string {
	cfg := map[string]string{}
	confP := filepath.Join(configDir(), "global_inject.conf")
	if data, err := os.ReadFile(confP); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for k, v := range loadNamed("global_inject") {
		cfg[k] = v
	}
	// `… force` (STACKS_FORCE_ALL=1) overrides the conf: force every inject key.
	if os.Getenv("STACKS_FORCE_ALL") == "1" {
		cfg["FORCE_ALL"] = "force"
		cfg["INJECT_FILL_ALL"] = "force"
	}
	if fxLower(cfg["INJECT_FILL_ALL"]) == "1" || fxLower(cfg["INJECT_FILL_ALL"]) == "true" || fxLower(cfg["INJECT_FILL_ALL"]) == "force" {
		fillVal := "1"
		if os.Getenv("STACKS_FORCE_ALL") == "1" {
			fillVal = "force" // force overwrites existing blocks, not just fills gaps
		}
		for _, k := range []string{"INJECT_DEPLOY", "INJECT_BLKIO", "INJECT_ULIMITS", "INJECT_COMMON_CAPS",
			"INJECT_HOSTNAME", "INJECT_STORAGE_OPT", "INJECT_MAC", "INJECT_LABELS",
			"INJECT_STOP_GRACE", "INJECT_LOGGING", "INJECT_CPUSET"} {
			if _, ok := cfg[k]; !ok || fillVal == "force" {
				cfg[k] = fillVal
			}
		}
	}
	return cfg
}

func fxLower(s string) string { return strings.ToLower(s) }

func fxGIEnabled(v string) bool {
	s := strings.ToLower(v)
	return s == "1" || s == "force" || s == "true"
}
func fxGIForce(v string) bool { return strings.ToLower(v) == "force" }
func fxGIIsForced(gi map[string]string, key string) bool {
	if fxOn(fxGet(gi, "FORCE_ALL", "0")) {
		return true
	}
	return fxOn(fxGet(gi, key+"_FORCE", "0"))
}

var (
	fxReAnchorHdr   = regexp.MustCompile(`^x-common-caps:`)
	fxReAnchorIns   = regexp.MustCompile(`^  (dns|restart|stop_grace):`)
	fxReStopGrace2  = regexp.MustCompile(`^  stop_grace_period:.*`)
	fxReStopSignal2 = regexp.MustCompile(`^  stop_signal:.*`)
	fxReLogging2    = regexp.MustCompile(`^  logging:`)
	fxReRestart2    = regexp.MustCompile(`^  restart:.*`)
)

// fxInjectIntoAnchor — faithful port of inject_into_anchor().
func fxInjectIntoAnchor(lines []string, gi map[string]string, dryRun bool) ([]string, int) {
	anchorStart, anchorEnd := -1, -1
	for i, line := range lines {
		if fxReAnchorHdr.MatchString(line) {
			anchorStart = i
			continue
		}
		if anchorStart != -1 && anchorEnd == -1 {
			if line != "" && !isSpace(line[0]) && !strings.HasPrefix(line, "#") {
				anchorEnd = i
				break
			}
		}
	}
	if anchorStart == -1 {
		return lines, 0
	}
	if anchorEnd == -1 {
		anchorEnd = len(lines)
	}
	block := lines[anchorStart:anchorEnd]
	blockText := strings.Join(block, "\n")
	newLines := append([]string{}, lines...)
	changes := 0
	var inserts []string
	insertAtPos := anchorEnd
	for i := anchorStart; i < anchorEnd; i++ {
		if fxReAnchorIns.MatchString(lines[i]) {
			insertAtPos = i
			break
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_STOP_GRACE", "0")) {
		force := fxGIIsForced(gi, "INJECT_STOP_GRACE")
		period := fxGet(gi, "STOP_GRACE_PERIOD", "120s")
		signal := fxGet(gi, "STOP_SIGNAL", "SIGTERM")
		if force && strings.Contains(blockText, "stop_grace_period:") {
			newLines = fxReplaceMatching(newLines, fxReStopGrace2, "  stop_grace_period: "+period)
			changes++
		} else if !strings.Contains(blockText, "stop_grace_period:") {
			inserts = append(inserts, "  stop_grace_period: "+period)
			changes++
		}
		if force && strings.Contains(blockText, "stop_signal:") {
			newLines = fxReplaceMatching(newLines, fxReStopSignal2, "  stop_signal: "+signal)
			changes++
		} else if !strings.Contains(blockText, "stop_signal:") {
			inserts = append(inserts, "  stop_signal: "+signal)
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_LOGGING", "0")) {
		force := fxGIIsForced(gi, "INJECT_LOGGING")
		driver := fxGet(gi, "LOGGING_DRIVER", "json-file")
		maxsize := fxGet(gi, "LOGGING_MAX_SIZE", "50m")
		maxfile := fxGet(gi, "LOGGING_MAX_FILE", "5")
		logLine := fmt.Sprintf("  logging: {driver: %s, options: {max-size: %s, max-file: '%s'}}", driver, maxsize, maxfile)
		if force && strings.Contains(blockText, "logging:") {
			newLines = fxReplaceWhole(newLines, fxReLogging2, logLine)
			changes++
		} else if !strings.Contains(blockText, "logging:") {
			inserts = append(inserts, logLine)
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_RESTART", "0")) {
		force := fxGIIsForced(gi, "INJECT_RESTART")
		policy := fxGet(gi, "RESTART_POLICY", "unless-stopped")
		if force && strings.Contains(blockText, "restart:") {
			newLines = fxReplaceMatching(newLines, fxReRestart2, "  restart: "+policy)
			changes++
		} else if !strings.Contains(blockText, "restart:") {
			inserts = append(inserts, "  restart: "+policy)
			changes++
		}
	}

	if len(inserts) > 0 && !dryRun {
		for i := len(inserts) - 1; i >= 0; i-- {
			newLines = insertAt(newLines, insertAtPos, inserts[i])
		}
	}
	return newLines, changes
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// fxReplaceMatching applies re.sub(pat, repl, line) per line (anchored prefix).
func fxReplaceMatching(lines []string, pat *regexp.Regexp, repl string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = pat.ReplaceAllString(l, repl)
	}
	return out
}

// fxReplaceWhole replaces the entire line with repl when it matches pat (prefix).
func fxReplaceWhole(lines []string, pat *regexp.Regexp, repl string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if pat.MatchString(l) {
			out[i] = repl
		} else {
			out[i] = l
		}
	}
	return out
}

var (
	// PORT-NOTE: Python used `^x-common-caps:.*?(?=^\S)` (lookahead). RE2 has no
	// lookahead; match the header + following indented/blank lines (same span).
	fxReXCommonCaps  = regexp.MustCompile(`(?m)^x-common-caps:.*(?:\n[ \t].*|\n)*`)
	fxReAnchorKey    = regexp.MustCompile(`(?m)^  ([a-z_]+):`)
	fxReLabelsHdr    = regexp.MustCompile(`^    labels:\s*$`)
	fxReSvcInjBefore = regexp.MustCompile(`^    (blkio_config|deploy|labels|healthcheck):`)
)

// fxInjectGlobalKeys — faithful port of inject_global_keys().
func fxInjectGlobalKeys(lines []string, svc *fxService, gi map[string]string, dryRun bool, stackPrefix string) ([]string, int) {
	block := fxSliceClamp(lines, svc.blockStart, svc.blockEnd+1)
	blockText := strings.Join(block, "\n")
	whole := strings.Join(lines, "\n")
	if strings.Contains(blockText, "<<: *common-caps") || fxGIEnabled(fxGet(gi, "INJECT_COMMON_CAPS", "0")) {
		if am := fxReXCommonCaps.FindString(whole); am != "" {
			for _, m := range fxReAnchorKey.FindAllStringSubmatch(am, -1) {
				k := m[1]
				if !strings.Contains(blockText, k+":") {
					blockText += "\n    " + k + ":"
				}
			}
		}
	}
	newLines := append([]string{}, lines...)
	changes := 0
	insertBefore := svc.blockEnd
	for i := svc.blockStart; i < svc.blockEnd+1 && i < len(lines); i++ {
		if fxReSvcInjBefore.MatchString(lines[i]) {
			insertBefore = i
			break
		}
	}
	var inserts []string

	if fxGIEnabled(fxGet(gi, "INJECT_COMMON_CAPS", "0")) {
		if !strings.Contains(blockText, "<<:") && strings.Contains(whole, "&common-caps") {
			inserts = append(inserts, "    <<: *common-caps")
			changes++
		}
	}
	if fxGIEnabled(fxGet(gi, "INJECT_HOSTNAME", "0")) && !strings.Contains(blockText, "hostname:") {
		inserts = append(inserts, "    hostname: "+svc.name)
		changes++
	}
	if fxGIEnabled(fxGet(gi, "INJECT_STORAGE_OPT", "0")) && !strings.Contains(blockText, "storage_opt:") {
		inserts = append(inserts, "    storage_opt: {size: "+fxGet(gi, "STORAGE_OPT_SIZE", "10G")+"}")
		changes++
	}

	if fxGIEnabled(fxGet(gi, "INJECT_STOP_GRACE", "0")) {
		force := fxGIIsForced(gi, "INJECT_STOP_GRACE")
		if force || !strings.Contains(blockText, "stop_grace_period:") {
			if force {
				newLines = fxReplaceMatching(newLines, regexp.MustCompile(`^    stop_grace_period:.*`), "    stop_grace_period: "+fxGet(gi, "STOP_GRACE_PERIOD", "120s"))
			} else {
				inserts = append(inserts, "    stop_grace_period: "+fxGet(gi, "STOP_GRACE_PERIOD", "120s"))
			}
			changes++
		}
		if force || !strings.Contains(blockText, "stop_signal:") {
			if force {
				newLines = fxReplaceMatching(newLines, regexp.MustCompile(`^    stop_signal:.*`), "    stop_signal: "+fxGet(gi, "STOP_SIGNAL", "SIGTERM"))
			} else {
				inserts = append(inserts, "    stop_signal: "+fxGet(gi, "STOP_SIGNAL", "SIGTERM"))
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_LOGGING", "0")) {
		force := fxGIIsForced(gi, "INJECT_LOGGING")
		driver := fxGet(gi, "LOGGING_DRIVER", "json-file")
		maxsize := fxGet(gi, "LOGGING_MAX_SIZE", "50m")
		maxfile := fxGet(gi, "LOGGING_MAX_FILE", "5")
		logLine := fmt.Sprintf("    logging: {driver: %s, options: {max-size: %s, max-file: '%s'}}", driver, maxsize, maxfile)
		if force || !strings.Contains(blockText, "logging:") {
			if force && strings.Contains(blockText, "logging:") {
				newLines = fxReplaceWhole(newLines, regexp.MustCompile(`^    logging:`), logLine)
			} else {
				inserts = append(inserts, logLine)
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_DEPLOY", "0")) {
		force := fxGIForce(fxGet(gi, "INJECT_DEPLOY", "0"))
		mem := fxGet(gi, "DEPLOY_MEMORY_LIMIT", "2G")
		cpu := fxGet(gi, "DEPLOY_CPU_LIMIT", "0.20")
		res := fxGet(gi, "DEPLOY_MEMORY_RESERVATION", "256M")
		plc := strings.TrimSpace(fxGet(gi, "DEPLOY_PLACEMENT_CONSTRAINT", ""))
		plcStr := ""
		if plc != "" {
			plcStr = fmt.Sprintf(", placement: {constraints: [%s]}", plc)
		}
		depLine := fmt.Sprintf("    deploy: {resources: {limits: {memory: %s, cpus: '%s'}, reservations: {memory: %s}}%s}", mem, cpu, res, plcStr)
		if force || !strings.Contains(blockText, "deploy:") {
			if force && strings.Contains(blockText, "deploy:") {
				newLines = fxReplaceWhole(newLines, regexp.MustCompile(`^    deploy:`), depLine)
			} else {
				inserts = append(inserts, depLine)
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_BLKIO", "0")) {
		force := fxGIForce(fxGet(gi, "INJECT_BLKIO", "0"))
		w := fxGet(gi, "BLKIO_WEIGHT", "500")
		r := fxGet(gi, "BLKIO_READ_BPS", "750mb")
		wr := fxGet(gi, "BLKIO_WRITE_BPS", "750mb")
		blkLine := fmt.Sprintf("    blkio_config: {weight: %s, device_read_bps: [{path: /dev/nvme0n1, rate: %s}], device_write_bps: [{path: /dev/nvme0n1, rate: %s}]}", w, r, wr)
		if force || !strings.Contains(blockText, "blkio_config:") {
			if force && strings.Contains(blockText, "blkio_config:") {
				newLines = fxReplaceWhole(newLines, regexp.MustCompile(`^    blkio_config:`), blkLine)
			} else {
				inserts = append(inserts, blkLine)
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_ULIMITS", "0")) {
		force := fxGIForce(fxGet(gi, "INJECT_ULIMITS", "0"))
		ns := fxGet(gi, "ULIMIT_NOFILE_SOFT", "65535")
		nh := fxGet(gi, "ULIMIT_NOFILE_HARD", "65535")
		np := fxGet(gi, "ULIMIT_NPROC", "65535")
		ulLine := fmt.Sprintf("    ulimits: {memlock: {soft: -1, hard: -1}, nofile: {soft: %s, hard: %s}, nproc: %s}", ns, nh, np)
		if force || !strings.Contains(blockText, "ulimits:") {
			if force && strings.Contains(blockText, "ulimits:") {
				newLines = fxReplaceWhole(newLines, regexp.MustCompile(`^    ulimits:`), ulLine)
			} else {
				inserts = append(inserts, ulLine)
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_RESTART", "0")) {
		force := fxGIIsForced(gi, "INJECT_RESTART")
		policy := fxGet(gi, "RESTART_POLICY", "unless-stopped")
		if force || !strings.Contains(blockText, "restart:") {
			if force && strings.Contains(blockText, "restart:") {
				newLines = fxReplaceMatching(newLines, regexp.MustCompile(`^    restart:.*`), "    restart: "+policy)
			} else {
				inserts = append(inserts, "    restart: "+policy)
			}
			changes++
		}
	}

	if fxGIEnabled(fxGet(gi, "INJECT_MAC", "0")) && !strings.Contains(blockText, "mac_address:") {
		sum := md5.Sum([]byte(svc.name))
		h := hex.EncodeToString(sum[:])
		inserts = append(inserts, fmt.Sprintf("    mac_address: 02:42:ac:11:%s:%s", h[0:2], h[2:4]))
		changes++
	}

	if fxGIEnabled(fxGet(gi, "INJECT_LABELS", "0")) {
		grp := stackPrefix
		if grp == "" {
			grp = "default"
		}
		want := [][2]string{{"traefik.enable", "true"}, {"sablier.enable", "true"}, {"sablier.group", grp}}
		var missing [][2]string
		for _, kv := range want {
			if !strings.Contains(blockText, kv[0]+"=") && !strings.Contains(blockText, kv[0]+":") {
				missing = append(missing, kv)
			}
		}
		if len(missing) > 0 {
			lblIdx := -1
			for i := svc.blockStart; i < svc.blockEnd+1 && i < len(newLines); i++ {
				if fxReLabelsHdr.MatchString(newLines[i]) {
					lblIdx = i
					break
				}
			}
			if lblIdx != -1 {
				for j := len(missing) - 1; j >= 0; j-- {
					newLines = insertAt(newLines, lblIdx+1, fmt.Sprintf(`      - "%s=%s"`, missing[j][0], missing[j][1]))
					changes++
				}
			} else {
				blk := []string{"    labels:"}
				for _, kv := range missing {
					blk = append(blk, fmt.Sprintf(`      - "%s=%s"`, kv[0], kv[1]))
				}
				inserts = append(inserts, blk...)
				changes++
			}
		}
	}

	if len(inserts) > 0 && !dryRun {
		for i := len(inserts) - 1; i >= 0; i-- {
			newLines = insertAt(newLines, insertBefore, inserts[i])
		}
	}
	return newLines, changes
}

// fxInjectCpuset — faithful port of inject_cpuset().
func fxInjectCpuset(lines []string, svc *fxService, gi map[string]string, stackPrefix string, dryRun bool) ([]string, int) {
	block := fxSliceClamp(lines, svc.blockStart, svc.blockEnd+1)
	blockText := strings.Join(block, "\n")
	newLines := append([]string{}, lines...)
	changes := 0
	insertBefore := svc.blockEnd
	insRe := regexp.MustCompile(`^    (labels|healthcheck|deploy|blkio):`)
	for i := svc.blockStart; i < svc.blockEnd+1 && i < len(lines); i++ {
		if insRe.MatchString(lines[i]) {
			insertBefore = i
			break
		}
	}
	forceAll := fxOn(fxGet(gi, "FORCE_ALL", "0"))
	force := forceAll || fxOn(fxGet(gi, "INJECT_CPUSET_FORCE", "0"))

	var cpuset, shares string
	heavy := strings.Fields(fxGet(gi, "CPUSET_heavy_containers", ""))
	if inList(heavy, svc.name) {
		allCores := fxGet(gi, "CPUSET_all_cores", "")
		if allCores == "" {
			allCores = strconv.Itoa(fxCPUCount() - 1)
		}
		cpuset = "0-" + allCores
		shares = fxGet(gi, "CPU_SHARES_heavy", "4096")
	} else {
		cpuset = fxGet(gi, "CPUSET_"+stackPrefix, fxGet(gi, "CPUSET_default", "0"))
		shares = fxGet(gi, "CPU_SHARES_default", "256")
	}

	var inserts []string
	if force || !strings.Contains(blockText, "cpuset:") {
		if force && strings.Contains(blockText, "cpuset:") {
			newLines = fxReplaceMatching(newLines, regexp.MustCompile(`^    cpuset:.*`), fmt.Sprintf(`    cpuset: "%s"`, cpuset))
		} else {
			inserts = append(inserts, fmt.Sprintf(`    cpuset: "%s"`, cpuset))
		}
		changes++
	}
	if force || !strings.Contains(blockText, "cpu_shares:") {
		if force && strings.Contains(blockText, "cpu_shares:") {
			newLines = fxReplaceMatching(newLines, regexp.MustCompile(`^    cpu_shares:.*`), "    cpu_shares: "+shares)
		} else {
			inserts = append(inserts, "    cpu_shares: "+shares)
		}
		changes++
	}

	if len(inserts) > 0 && !dryRun {
		for i := len(inserts) - 1; i >= 0; i-- {
			newLines = insertAt(newLines, insertBefore, inserts[i])
		}
	}
	return newLines, changes
}

func fxCPUCount() int {
	n := 1
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		n = strings.Count(string(data), "processor\t")
		if n == 0 {
			n = strings.Count(string(data), "processor")
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// fxGetAllGroupsGlobal — faithful port of get_all_groups_global() (used for the
// family-net link path; the authoritative network pass uses getFamilyOf directly).
type fxGroup struct {
	netName       string
	membersByFile map[string][]string
	allMembers    []string
}

func fxGetAllGroupsGlobal(allFiles []string) map[string]fxGroup {
	families := getFamilies("")
	cnameToFile := map[string]string{}
	for _, f := range allFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range fxReCnameVal.FindAllStringSubmatch(string(data), -1) {
			cnameToFile[strings.Trim(strings.TrimSpace(m[1]), `"'`)] = f
		}
	}
	result := map[string]fxGroup{}
	for head, members := range families {
		root := strings.Split(strings.ReplaceAll(strings.ReplaceAll(head, ".", "-"), "_", "-"), "-")[0]
		netName := root + "_net"
		membersByFile := map[string][]string{}
		var all []string
		for m := range members {
			all = append(all, m)
			if f, ok := cnameToFile[m]; ok {
				membersByFile[f] = append(membersByFile[f], m)
			}
		}
		if len(membersByFile) > 0 {
			result[head] = fxGroup{netName: netName, membersByFile: membersByFile, allMembers: all}
		}
	}
	return result
}

// ── Legacy corruption repair regexes (Phase 0.5b) ────────────────────────────
var (
	fxReNetworksHdr2  = regexp.MustCompile(`^networks:\s*$`)
	fxReLabelInNet    = regexp.MustCompile(`\s+- "(traefik\.|sablier\.)`)
	fxReDeviceReadBps = regexp.MustCompile(`device_read_bps:\s*\[.*?\]`)
	fxReNameLineTop   = regexp.MustCompile(`^name:\s*`)
)

// ── _dedup_service_keys — faithful port of _dedup_service_keys() ──────────────
var (
	fxReSvcHdrBare  = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:\s*$`)
	fxReSvcKey4     = regexp.MustCompile(`^    ([A-Za-z_][A-Za-z0-9_]*):`)
	fxReSixIndentNL = regexp.MustCompile(`^      `)
)

var fxDedupSet = map[string]bool{
	"storage_opt": true, "stop_grace_period": true, "stop_signal": true, "restart": true,
	"user": true, "privileged": true, "logging": true, "dns": true, "tmpfs": true,
	"sysctls": true, "security_opt": true, "cap_add": true, "mem_limit": true, "shm_size": true,
}

func fxDedupServiceKeys(content string) string {
	lines := strings.Split(content, "\n")
	var out []string
	seen := map[string]bool{}
	i := 0
	for i < len(lines) {
		l := lines[i]
		if fxReSvcHdrBare.MatchString(l) {
			seen = map[string]bool{}
			out = append(out, l)
			i++
			continue
		}
		if m := fxReSvcKey4.FindStringSubmatch(l); m != nil && fxDedupSet[m[1]] {
			key := m[1]
			if seen[key] {
				i++
				for i < len(lines) && fxReSixIndentNL.MatchString(lines[i]) {
					i++
				}
				continue
			}
			seen[key] = true
		}
		out = append(out, l)
		i++
	}
	return strings.Join(out, "\n")
}

// ── Main entry: cmdFix ────────────────────────────────────────────────────────
// cmdFix is the faithful port of stacks_fix.main(): the 'fix'/'recreate'/'repair'/
// 'up' command core. Args after the command: [target] [service]; flags --dry-run,
// --replace-broken, --force-hc, --all.
func cmdFix(args []string) {
	var positional []string
	dryRun := false
	replaceBrokenFlag := false
	forceHCFlag := false
	forceAll := false
	for _, a := range args {
		switch {
		case a == "--dry-run":
			dryRun = true
		case a == "--replace-broken":
			replaceBrokenFlag = true
		case a == "--force-hc":
			forceHCFlag = true
		case a == "force" || a == "--force" || a == "force-all" || a == "forceall":
			// `stacks fix <stack> force` — force EVERYTHING this pass: recreate
			// healthchecks, force depends + all global-injects, replace broken HCs.
			forceAll = true
		case strings.HasPrefix(a, "--"):
			// other flags (e.g. --all) handled below via positional fallthrough
			if a == "--all" {
				positional = append(positional, a)
			}
		default:
			positional = append(positional, a)
		}
	}

	cfg := fxLoadConf()
	if forceAll {
		// One switch that makes the whole fix pass aggressive. STACKS_FORCE_ALL is
		// picked up by fxLoadGlobalInjectConf() to force every INJECT_* key too.
		os.Setenv("STACKS_FORCE_ALL", "1")
		forceHCFlag = true
		replaceBrokenFlag = true
		cfg["FIX_FORCE_HC"] = "1"
		cfg["FIX_FORCE_DEPENDS"] = "1"
		cfg["FIX_FORCE_NETWORKS"] = "1"
		cfg["FIX_FORCE_VOLUMES"] = "1"
		cfg["FIX_REPLACE_BROKEN_HC"] = "1"
		fxpr(fmt.Sprintf("%s   ⚡ FORCE: recreating healthchecks + forcing all injects%s", fxY, fxX))
	}
	replaceBroken := replaceBrokenFlag || fxOn(fxGet(cfg, "FIX_REPLACE_BROKEN_HC", "0"))
	forceHC := forceHCFlag || fxOn(fxGet(cfg, "FIX_FORCE_HC", "0"))
	sd := cfg["STACKS_DIR"]

	target := "all"
	if len(positional) > 0 {
		target = positional[0]
	}
	svc := ""
	if len(positional) > 1 {
		svc = positional[1]
	}

	// Build files list early.
	var files []string
	if target == "all" || target == "--all" {
		for _, f := range ngSortedYmls(sd) {
			files = append(files, filepath.Join(sd, f))
		}
	} else {
		fname := target
		if !strings.HasSuffix(fname, ".yml") && !strings.HasSuffix(fname, ".yaml") {
			fname = target + ".yml"
		}
		fp := filepath.Join(sd, fname)
		if fi, err := os.Stat(fp); err == nil && !fi.IsDir() {
			files = []string{fp}
		}
	}

	fxpr(fmt.Sprintf("\n%s╔══════════════════════════════════════╗%s", fxM, fxX))
	fxpr(fmt.Sprintf("%s║   🔧 STACKS FIXER                    ║%s", fxM, fxX))
	fxpr(fmt.Sprintf("%s╚══════════════════════════════════════╝%s", fxM, fxX))
	if dryRun {
		fxpr(fmt.Sprintf("%s   DRY RUN — no files will be written%s", fxY, fxX))
	}

	if fi, err := os.Stat(sd); err != nil || !fi.IsDir() {
		fxpr(fmt.Sprintf("%s✘ Stacks dir not found: %s%s", fxR, sd, fxX))
		os.Exit(1)
	}

	// Safety snapshot + compose-config validity.
	safetyOrig := map[string]string{}
	safetyValid := map[string]bool{}
	if !dryRun {
		for _, sf := range files {
			if data, err := os.ReadFile(sf); err == nil {
				safetyOrig[sf] = string(data)
			}
			c := exec.Command("docker", "compose", "-f", sf, "config")
			c.Env = dockerEnv()
			safetyValid[sf] = c.Run() == nil
		}
	}

	total := 0

	// ── Phase 0: strip profiles ──
	if fxOn(cfg["FIX_STRIP_PROFILES"]) {
		fxpr(fmt.Sprintf("\n%s🧹 Stripping profiles: blocks%s", fxC, fxX))
		profFixed := 0
		for _, f := range ngSortedYmls(sd) {
			fp := filepath.Join(sd, f)
			if dryRun {
				if data, err := os.ReadFile(fp); err == nil && strings.Contains(string(data), "profiles:") {
					fxpr(fmt.Sprintf("  %s[dry-run] would strip profiles from %s%s", fxY, f, fxX))
					profFixed++
				}
			} else if fxStripProfilesFromFile(fp, false) {
				fxpr(fmt.Sprintf("  %s✔ %s: profiles: blocks stripped%s", fxG, f, fxX))
				profFixed++
			}
		}
		if profFixed == 0 {
			fxpr(fmt.Sprintf("  %s✔ No profiles: blocks found%s", fxG, fxX))
		}
		total += profFixed
	}

	// ── Phase 0.1: Auto-name compose files ──
	if fxOn(fxGet(cfg, "FIX_AUTO_NAME", "1")) {
		for _, f := range files {
			stackName := fxStripYmlExt(filepath.Base(f))
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			lines := strings.Split(string(data), "\n")
			hasName := false
			nameCorrect := false
			for i := 0; i < 5 && i < len(lines); i++ {
				if fxReNameLineTop.MatchString(lines[i]) {
					hasName = true
					if strings.TrimSpace(lines[i]) == "name: "+stackName {
						nameCorrect = true
					}
					break
				}
			}
			_ = hasName
			if !nameCorrect {
				var kept []string
				for _, l := range lines {
					if !fxReNameLineTop.MatchString(l) {
						kept = append(kept, l)
					}
				}
				insertPos := 0
				for i, l := range kept {
					if strings.HasPrefix(l, "#") {
						insertPos = i + 1
					} else {
						break
					}
				}
				kept = insertAt(kept, insertPos, "name: "+stackName)
				_ = os.WriteFile(f, []byte(strings.Join(kept, "\n")), 0o644)
				fxpr(fmt.Sprintf("  %s✔ Named %s%s", fxG, stackName, fxX))
				total++
			}
		}
	}

	// ── Phase 0.4: Container auto-naming ──
	if fxOn(fxGet(cfg, "FIX_AUTO_NAME_CONTAINERS", "0")) {
		fxpr(fmt.Sprintf("\n%s🏷  Container auto-naming%s", fxC, fxX))
		rmap, coll := fxRenameReport(sd)
		if len(coll) > 0 {
			fxpr(fmt.Sprintf("  %s✘ rename SKIPPED — name collisions detected:%s", fxR, fxX))
			c := 0
			for nw, olds := range coll {
				if c >= 5 {
					break
				}
				fxpr(fmt.Sprintf("    %s%v -> %s%s", fxR, olds, nw, fxX))
				c++
			}
		} else if len(rmap) > 0 {
			rep1 := fxApplyRenames(sd, rmap, dryRun)
			rep2 := map[string]int{}
			dyn := fxGet(cfg, "DYNAMICS_DIR", "")
			if fxOn(fxGet(cfg, "FIX_SYNC_DYNAMICS_NAMES", "0")) && dyn != "" {
				if fi, err := os.Stat(dyn); err == nil && fi.IsDir() {
					rep2 = fxApplyRenames(dyn, rmap, dryRun)
				}
			}
			if dryRun {
				fxpr(fmt.Sprintf("  %s[dry-run] would rename %d containers across %d stack(s) + %d dynamic(s)%s", fxY, len(rmap), len(rep1), len(rep2), fxX))
			} else {
				fxpr(fmt.Sprintf("  %s✔ renamed %d containers across %d stack(s) + %d dynamic(s)%s", fxG, len(rmap), len(rep1), len(rep2), fxX))
				total += len(rep1) + len(rep2)
			}
		} else {
			fxpr(fmt.Sprintf("  %s✔ all container names already clean%s", fxG, fxX))
		}
	}

	// ── Domain normalization ──
	if fxOn(fxGet(cfg, "FIX_NORMALIZE_DOMAINS", "0")) {
		fxpr(fmt.Sprintf("\n%s🌐 Normalizing hostnames/domainnames%s", fxC, fxX))
		dom := fxGet(cfg, "DOMAIN", "loveiznothin.com")
		var bl []string
		for _, x := range strings.Fields(strings.ReplaceAll(fxGet(cfg, "FIX_DOMAIN_BLACKLIST", ""), ",", " ")) {
			if x != "" {
				bl = append(bl, x)
			}
		}
		ndTotal := 0
		ndFiles := 0
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ domain-normalize error on %s: %v%s", fxR, filepath.Base(f), err, fxX))
				continue
			}
			c := string(data)
			nc, cn := fxNormalizeHostDomain(c, dom, bl)
			if cn > 0 && nc != c {
				ndTotal += cn
				ndFiles++
				if !dryRun {
					fxCopy(f, f+fmt.Sprintf(".bak-domnorm-%d", time.Now().Unix()))
					_ = os.WriteFile(f, []byte(nc), 0o644)
				}
			}
		}
		if dryRun {
			fxpr(fmt.Sprintf("  %s[dry-run] would normalize %d host/domain line(s) in %d file(s)%s", fxY, ndTotal, ndFiles, fxX))
		} else {
			fxpr(fmt.Sprintf("  %s✔ normalized %d host/domain line(s) in %d file(s)%s", fxG, ndTotal, ndFiles, fxX))
			total += ndTotal
		}
	}

	// ── Phase 0.5: Corruption repair (reuse repair_file from repair.go) ──
	for _, rf := range files {
		rfixes := repair_file(rf, dryRun)
		if len(rfixes) > 0 {
			for _, fix := range rfixes {
				fxpr(fmt.Sprintf("  %s✔ %s: %s%s", fxG, filepath.Base(rf), fix, fxX))
			}
			total += len(rfixes)
		}
	}

	// ── Phase 0.5b: Legacy corruption repair ──
	repairChanges := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := string(data)
		original := content
		lines := strings.Split(content, "\n")
		var result []string
		inNetworks := false
		for _, line := range lines {
			if fxReNetworksHdr2.MatchString(line) {
				inNetworks = true
			} else if fxReTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
				inNetworks = false
			}
			if inNetworks && fxReLabelInNet.MatchString(line) {
				repairChanges++
				continue
			}
			if strings.Contains(line, "blkio_config") && (strings.Contains(line, "NONE") || strings.Contains(line, "CMD") || strings.Contains(line, "CMD-SHELL")) {
				line = fxReDeviceReadBps.ReplaceAllString(line, "device_read_bps: [{path: /dev/nvme0n1, rate: 500mb}]")
				repairChanges++
			}
			result = append(result, line)
		}
		content = strings.Join(result, "\n")
		if content != original {
			_ = os.WriteFile(f, []byte(content), 0o644)
			fxpr(fmt.Sprintf("  %s✔ Repaired corruption in %s%s", fxG, filepath.Base(f), fxX))
		}
	}
	total += repairChanges

	// ── Phase 1: auto-define missing networks/volumes ──
	if fxOn(cfg["FIX_DEFINE_NETVOL"]) {
		fxpr(fmt.Sprintf("\n%s🌐 Network/Volume auto-define%s", fxC, fxX))
		skip := ngSet(strings.Fields(fxGet(cfg, "FIX_SKIP_FILES", "")))
		creators := ngDiscoverCreatorFiles(sd, skip)
		neededNets, neededVols := ngCollectServiceRefs(sd, creators, skip)
		definedNets := fxRealDefinedNets(sd, skip)
		definedVols := fxRealDefinedVols(sd, skip)
		missingNets := ngDiff(neededNets, definedNets)
		missingVols := ngDiff(neededVols, definedVols)
		if len(missingNets) > 0 || len(missingVols) > 0 {
			var targetPath string
			if len(creators) > 0 {
				paths := make([]string, 0, len(creators))
				for p := range creators {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				var bestSize int64 = -1
				for _, p := range paths {
					if bestSize < 0 || creators[p].size < bestSize {
						bestSize = creators[p].size
						targetPath = p
					}
				}
			} else {
				targetPath = ngSmallestFileOverall(sd)
				fxpr(fmt.Sprintf("  %sNo creator files found — bootstrapping into %s%s", fxY, filepath.Base(targetPath), fxX))
			}
			used := ngAllUsedSubnets(creators, cfg["FIX_SUBNET_BASE"])
			total += ngAddToCreator(targetPath, missingNets, missingVols, cfg["FIX_SUBNET_BASE"], used, dryRun)
		} else {
			fxpr(fmt.Sprintf("  %s✔ All referenced networks/volumes already defined%s", fxG, fxX))
		}
	}

	// ── Phase 2: heal typos in creator files ──
	if fxOn(cfg["FIX_HEAL_TYPOS"]) {
		fxpr(fmt.Sprintf("\n%s🩹 Creator-file typo healing%s", fxC, fxX))
		for path := range ngDiscoverCreatorFiles(sd, nil) {
			total += fxHealCreatorTypos(path, dryRun)
		}
	}

	// ── Phase 3: healthchecks ──
	if fxOn(cfg["FIX_HEALTHCHECKS"]) || forceHC {
		mode := "add-only; existing ones never touched"
		if forceHC {
			mode = "FORCE: re-stamping ALL via probe"
		}
		fxpr(fmt.Sprintf("\n%s❤️  Healthchecks (%s)%s", fxC, mode, fxX))
		var hcFiles []string
		if target == "all" || target == "--all" {
			for _, f := range ngSortedYmls(sd) {
				hcFiles = append(hcFiles, filepath.Join(sd, f))
			}
		} else {
			fname := target
			if !strings.HasSuffix(fname, ".yml") && !strings.HasSuffix(fname, ".yaml") {
				fname = target + ".yml"
			}
			fp := filepath.Join(sd, fname)
			if fi, err := os.Stat(fp); err != nil || fi.IsDir() {
				fxpr(fmt.Sprintf("%s✘ Stack not found: %s%s", fxR, target, fxX))
				os.Exit(1)
			}
			hcFiles = []string{fp}
		}
		for _, f := range hcFiles {
			stackName := fxStripYmlExt(filepath.Base(f))
			fxpr(fmt.Sprintf("\n%s🔧 %s%s", fxC, stackName, fxX))
			total += fxFixHealthchecks(f, cfg, svc, dryRun, replaceBroken, forceHC)
		}
	}

	// ── Phase 3.4: Container name normalization ──
	if fxOn(fxGet(cfg, "FIX_NORMALIZE_NAMES", "0")) {
		fxpr(fmt.Sprintf("\n%s🏷  Normalizing container names (._  ->  -)%s", fxC, fxX))
		normFixed := 0
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ %s: %v%s", fxR, filepath.Base(f), err, fxX))
				continue
			}
			dataStr := string(data)
			newData := dataStr
			for _, m := range fxReCnameVal.FindAllStringSubmatch(dataStr, -1) {
				oldName := strings.Trim(m[1], `"' `)
				newName := strings.NewReplacer(".", "-", "_", "-").Replace(oldName)
				if newName == oldName {
					continue
				}
				if dryRun {
					fxpr(fmt.Sprintf("  %s[dry] %s -> %s%s", fxY, oldName, newName, fxX))
				} else {
					wb := regexp.MustCompile(`\b` + regexp.QuoteMeta(oldName) + `\b`)
					newData = wb.ReplaceAllString(newData, newName)
				}
				normFixed++
			}
			if !dryRun && newData != dataStr {
				_ = os.WriteFile(f, []byte(newData), 0o644)
			}
		}
		if normFixed == 0 {
			fxpr(fmt.Sprintf("  %s✔ All names already normalized%s", fxG, fxX))
		}
		total += normFixed
	}

	// ── Phase 3.5: depends_on injection ──
	if fxOn(fxGet(cfg, "FIX_AUTO_DEPENDS", "0")) || fxOn(fxGet(cfg, "FIX_REMOVE_DEPENDS", "0")) || fxOn(fxGet(cfg, "FIX_FORCE_DEPENDS", "0")) {
		if fxOn(fxGet(cfg, "FIX_REMOVE_DEPENDS", "0")) && !fxOn(fxGet(cfg, "FIX_AUTO_DEPENDS", "0")) {
			fxpr(fmt.Sprintf("\n%s🔗 Removing depends_on (retire mode)%s", fxC, fxX))
		} else {
			fxpr(fmt.Sprintf("\n%s🔗 Injecting depends_on for related containers%s", fxC, fxX))
		}
		depsFixed := 0
		var depFiles []string
		if target == "all" || target == "--all" {
			for _, f := range ngSortedYmls(sd) {
				depFiles = append(depFiles, filepath.Join(sd, f))
			}
		} else {
			fname := target
			if !strings.HasSuffix(fname, ".yml") && !strings.HasSuffix(fname, ".yaml") {
				fname = target + ".yml"
			}
			depFiles = []string{filepath.Join(sd, fname)}
		}
		for _, f := range depFiles {
			if svc != "" {
				if data, err := os.ReadFile(f); err != nil || !strings.Contains(string(data), svc) {
					continue
				}
			}
			notes := fxInjectDependsOn(f, cfg)
			for _, note := range notes {
				if strings.Contains(strings.ToLower(note), "error") {
					fxpr(fmt.Sprintf("  %s✘ %s: %s%s", fxR, filepath.Base(f), note, fxX))
				} else {
					fxpr(fmt.Sprintf("  %s✔ %s: %s%s", fxG, filepath.Base(f), note, fxX))
					depsFixed++
				}
			}
		}
		if depsFixed == 0 {
			fxpr(fmt.Sprintf("  %s✔ No missing depends_on found%s", fxG, fxX))
		}
		total += depsFixed
	}

	filesAlias := files

	// ── Phase 3.6: Group IP alignment ──
	// PORT-NOTE: the Python uses stacks_collision.scan_all_ports + is_locked_container,
	// which are not yet ported to Go. The alignment here mirrors the family-head IP
	// propagation; the port-conflict guard is approximated by checking the locked-
	// containers config list (IP_PORT_LOCKED_CONTAINERS / LOCKED_IPS) instead of a
	// live port map. Behaviour matches in the common case (no conflicting ports).
	if fxOn(fxGet(cfg, "FIX_GROUP_SAME_IP", "0")) {
		fxpr(fmt.Sprintf("\n%s🔗 Aligning family IPs (all members -> same IP as head)%s", fxC, fxX))
		grpFixed := 0
		grpSkip := 0
		locked := ngSet(strings.Fields(strings.ReplaceAll(fxGet(cfg, "IP_PORT_LOCKED_CONTAINERS", ""), ",", " ")))
		allFams := getFamilies("")
		rePortIPlocal := regexp.MustCompile(`(192\.168\.1\.\d+):(\d+):\d+`)
		reIPv4 := regexp.MustCompile(`ipv4_address:\s*(192\.168\.1\.\d+)`)
		reNextSvc := regexp.MustCompile(`\n  [a-zA-Z]`)
		gip := func(cname string) (string, string) {
			ymls, _ := filepath.Glob(filepath.Join(sd, "*.yml"))
			for _, fp := range ymls {
				data, _ := os.ReadFile(fp)
				d := string(data)
				if !strings.Contains(d, "container_name: "+cname) {
					continue
				}
				idx := strings.Index(d, "container_name: "+cname)
				b := d[idx:]
				if len(b) > 3000 {
					b = b[:3000]
				}
				if len(b) > 10 {
					if nx := reNextSvc.FindStringIndex(b[10:]); nx != nil {
						b = b[:nx[0]+10]
					}
				}
				if pp := rePortIPlocal.FindStringSubmatch(b); pp != nil {
					return pp[1], fp
				}
				if m2 := reIPv4.FindStringSubmatch(b); m2 != nil {
					return m2[1], fp
				}
			}
			return "", ""
		}
		heads := make([]string, 0, len(allFams))
		for h := range allFams {
			heads = append(heads, h)
		}
		sort.Strings(heads)
		for _, head := range heads {
			members := allFams[head]
			hip, _ := gip(head)
			if hip == "" {
				continue
			}
			memList := fxSortedSet(members)
			for _, dep := range memList {
				if dep == head || locked[dep] {
					continue
				}
				dip, dfp := gip(dep)
				if dip == "" || dfp == "" || dip == hip {
					continue
				}
				if dryRun {
					fxpr(fmt.Sprintf("  %s[dry] %s: %s -> %s [%s]%s", fxG, dep, dip, hip, head, fxX))
				} else {
					data, _ := os.ReadFile(dfp)
					dc := string(data)
					di := strings.Index(dc, "container_name: "+dep)
					if di < 0 {
						continue
					}
					pre := dc[:di]
					rest := dc[di:]
					blEnd := 3000
					if len(rest) > 10 {
						if nx := reNextSvc.FindStringIndex(rest[10:]); nx != nil {
							blEnd = nx[0] + 10
						}
					}
					if blEnd > len(rest) {
						blEnd = len(rest)
					}
					blk := strings.ReplaceAll(rest[:blEnd], dip+":", hip+":")
					_ = os.WriteFile(dfp, []byte(pre+blk+rest[blEnd:]), 0o644)
					fxpr(fmt.Sprintf("  %s✔ %s: %s -> %s [%s]%s", fxG, dep, dip, hip, head, fxX))
				}
				grpFixed++
			}
		}
		if grpFixed == 0 && grpSkip == 0 {
			fxpr(fmt.Sprintf("  %s✔ All family IPs already aligned%s", fxG, fxX))
		} else {
			fxpr(fmt.Sprintf("  %s✔ %d aligned, %d skipped%s", fxG, grpFixed, grpSkip, fxX))
		}
		total += grpFixed
	}

	// ── Phase 4a: collapse double-spaced files ──
	for _, f := range filesAlias {
		if fxCollapseBlankLines(f, dryRun) && !dryRun {
			fxpr(fmt.Sprintf("  %s✔ %s: double-spacing collapsed%s", fxG, filepath.Base(f), fxX))
		}
	}

	// ── Phase 4: remove gaps ──
	if fxOn(fxGet(cfg, "FIX_REMOVE_GAPS", "1")) {
		fxpr(fmt.Sprintf("\n%s🧹 Removing gaps in service blocks%s", fxC, fxX))
		gapsFixed := 0
		for _, f := range filesAlias {
			if dryRun {
				if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), "\n\n") {
					fxpr(fmt.Sprintf("  %s[dry-run] would remove gaps from %s%s", fxY, filepath.Base(f), fxX))
					gapsFixed++
				}
			} else if fxRemoveGapsFromFile(f, false) {
				fxpr(fmt.Sprintf("  %s✔ %s: gaps removed%s", fxG, filepath.Base(f), fxX))
				gapsFixed++
			}
		}
		if gapsFixed == 0 {
			fxpr(fmt.Sprintf("  %s✔ No gaps found%s", fxG, fxX))
		}
		total += gapsFixed
	}

	// ── Phase 5: Volume management ──
	volBase := fxGet(cfg, "FIX_VOLUME_BASE", filepath.Join(home(), "docker"))
	if fxOn(fxGet(cfg, "FIX_CREATE_VOLUME_DIRS", "1")) {
		fxpr(fmt.Sprintf("\n%s📁 Volume directories%s", fxC, fxX))
		dirsCreated := 0
		dirsChecked := 0
		for _, f := range filesAlias {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var fl []string
			for _, l := range fxReadlines(string(data)) {
				fl = append(fl, strings.TrimRight(l, "\n"))
			}
			mounts := fxGetBindMounts(fl)
			dirsChecked += len(mounts)
			for _, r := range fxCreateVolumeDirs(mounts, dryRun) {
				fxpr(fmt.Sprintf("  %s✔ %s%s", fxG, r, fxX))
				dirsCreated++
			}
		}
		if dirsCreated > 0 {
			fxpr(fmt.Sprintf("  %s✔ %d director(ies) created%s", fxG, dirsCreated, fxX))
		} else {
			fxpr(fmt.Sprintf("  %s✔ All %d bind mount dirs already exist%s", fxG, dirsChecked, fxX))
		}
	}

	if fxOn(fxGet(cfg, "FIX_CONVERT_NAMED_TO_BIND", "0")) {
		fxpr(fmt.Sprintf("\n%s🔄 Converting named volumes to bind mounts%s", fxC, fxX))
		convTotal := 0
		for _, f := range filesAlias {
			data, err := os.ReadFile(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ %s: %v%s", fxR, filepath.Base(f), err, fxX))
				continue
			}
			var fl []string
			for _, l := range fxReadlines(string(data)) {
				fl = append(fl, strings.TrimRight(l, "\n"))
			}
			newLines, changes := fxConvertNamedToBind(fl, volBase, dryRun)
			if changes > 0 {
				if dryRun {
					fxpr(fmt.Sprintf("  %s[dry-run] %s: %d named→bind%s", fxY, filepath.Base(f), changes, fxX))
				} else {
					fxBackup(f)
					_ = os.WriteFile(f, []byte(strings.Join(newLines, "\n")), 0o644)
					fxpr(fmt.Sprintf("  %s✔ %s: %d named→bind%s", fxG, filepath.Base(f), changes, fxX))
				}
				convTotal += changes
			}
		}
		if convTotal == 0 {
			fxpr(fmt.Sprintf("  %s✔ No named volumes found%s", fxG, fxX))
		}
		total += convTotal
	}

	// ── Phase 6: Network auto-injection ──
	autoNets := strings.Fields(fxGet(cfg, "FIX_AUTO_NETWORKS", ""))
	doLink := fxOn(fxGet(cfg, "FIX_AUTO_LINK_NETWORKS", "0"))
	doComposeNet := fxOn(fxGet(cfg, "FIX_AUTO_COMPOSE_NETWORK", "0"))

	if len(autoNets) > 0 || doLink || doComposeNet {
		fxpr(fmt.Sprintf("\n%s🌐 Network auto-injection%s", fxC, fxX))
		netChanges := 0
		_ = fxGetAllGroupsGlobal // global map reserved; authoritative pass uses getFamilyOf
		for _, f := range filesAlias {
			stackName := fxStripYmlExt(filepath.Base(f))
			services, raw, err := fxParseServicesWithPositions(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ %s: %v%s", fxR, stackName, err, fxX))
				continue
			}
			var fileLines []string
			for _, l := range raw {
				fileLines = append(fileLines, strings.TrimRight(l, "\n"))
			}
			var realSvcs []*fxService
			for _, s := range services {
				if !strings.HasPrefix(s.name, "provisioner") && s.image != "" && !fxReAlpine.MatchString(s.image) {
					realSvcs = append(realSvcs, s)
				}
			}
			if len(realSvcs) == 0 {
				continue
			}
			changed := false
			lines := append([]string{}, fileLines...)
			master := "traefik_net"
			if len(autoNets) > 0 {
				master = autoNets[0]
			}
			stk := ""
			if doComposeNet {
				stk = strings.ReplaceAll(stackName+"_net", "-", "_")
			}
			usedNets := map[string]bool{master: true}
			for i := len(realSvcs) - 1; i >= 0; i-- {
				svcObj := realSvcs[i]
				blk := fxSliceClamp(lines, svcObj.blockStart, svcObj.blockEnd+1)
				cn := svcObj.name
				for _, l := range blk {
					if m := regexp.MustCompile(`\s*container_name:\s*(\S+)`).FindStringSubmatch(l); m != nil {
						cn = strings.Trim(strings.TrimSpace(m[1]), `"'`)
						break
					}
				}
				fnet := ""
				if doLink {
					if h, _ := getFamilyOf(cn, ""); h != "" {
						root := strings.Split(strings.ReplaceAll(strings.ReplaceAll(h, ".", "-"), "_", "-"), "-")[0]
						fnet = root + "_net"
					}
				}
				var didChange bool
				lines, didChange = fxSetNetworksAuthoritative(lines, svcObj, master, fnet, stk)
				if fnet != "" {
					usedNets[fnet] = true
				}
				if stk != "" {
					usedNets[stk] = true
				}
				if didChange {
					netChanges++
					changed = true
				}
			}
			for n := range usedNets {
				lines, _ = fxEnsureNetworkDeclared(lines, n)
			}
			if changed && !dryRun {
				content := strings.Join(lines, "\n")
				netCount := len(regexp.MustCompile(`(?m)^networks:\s*$`).FindAllString(content, -1))
				if netCount > 1 {
					fxpr(fmt.Sprintf("  %s✘ %s: duplicate networks: detected — skipping%s", fxR, stackName, fxX))
					continue
				}
				fxBackup(f)
				_ = os.WriteFile(f, []byte(content), 0o644)
				fxpr(fmt.Sprintf("  %s✔ %s: networks injected%s", fxG, stackName, fxX))
			}
		}
		if netChanges == 0 {
			fxpr(fmt.Sprintf("  %s✔ All networks already present%s", fxG, fxX))
		} else {
			fxpr(fmt.Sprintf("  %s✔ %d network injection(s)%s", fxG, netChanges, fxX))
		}
		total += netChanges
	}

	// ── Phase 7: Global key injection ──
	gi := fxLoadGlobalInjectConf()
	anchorKeys := []string{"INJECT_STOP_GRACE", "INJECT_LOGGING", "INJECT_RESTART"}
	svcKeys := []string{"INJECT_DEPLOY", "INJECT_BLKIO", "INJECT_ULIMITS", "INJECT_COMMON_CAPS",
		"INJECT_HOSTNAME", "INJECT_STORAGE_OPT", "INJECT_MAC", "INJECT_LABELS", "INJECT_RESTART"}
	allKeys := append(append([]string{}, anchorKeys...), svcKeys...)
	anyEnabled := func(keys []string) bool {
		for _, k := range keys {
			if fxGIEnabled(fxGet(gi, k, "0")) {
				return true
			}
		}
		return false
	}
	if anyEnabled(allKeys) {
		fxpr(fmt.Sprintf("\n%s⚙️  Global key injection%s", fxC, fxX))
		giChanges := 0
		for _, f := range filesAlias {
			stackName := fxStripYmlExt(filepath.Base(f))
			data, err := os.ReadFile(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ %s: %v%s", fxR, stackName, err, fxX))
				continue
			}
			var lines []string
			for _, l := range fxReadlines(string(data)) {
				lines = append(lines, strings.TrimRight(l, "\n"))
			}
			changed := false

			if anyEnabled(anchorKeys) {
				var anchChanges int
				lines, anchChanges = fxInjectIntoAnchor(lines, gi, dryRun)
				if anchChanges > 0 {
					giChanges += anchChanges
					changed = true
					if dryRun {
						fxpr(fmt.Sprintf("  %s[dry-run] %s anchor: %d key(s) would be updated%s", fxY, stackName, anchChanges, fxX))
					}
				}
			}

			if anyEnabled(svcKeys) {
				// re-parse positions from current in-memory lines via a temp file.
				tmp, tmpErr := os.CreateTemp("", "stacksfix-*.yml")
				if tmpErr == nil {
					tmpName := tmp.Name()
					_, _ = tmp.WriteString(strings.Join(lines, "\n"))
					tmp.Close()
					services, _, _ := fxParseServicesWithPositions(tmpName)
					os.Remove(tmpName)
					var realSvcs []*fxService
					for _, s := range services {
						if !strings.HasPrefix(s.name, "provisioner") && s.image != "" && !fxReAlpine.MatchString(s.image) {
							realSvcs = append(realSvcs, s)
						}
					}
					sp := stackName
					if m := regexp.MustCompile(`^([a-zA-Z]+)`).FindStringSubmatch(stackName); m != nil {
						sp = m[1]
					}
					for i := len(realSvcs) - 1; i >= 0; i-- {
						var svcChanges int
						lines, svcChanges = fxInjectGlobalKeys(lines, realSvcs[i], gi, dryRun, sp)
						if svcChanges > 0 {
							giChanges += svcChanges
							changed = true
							if dryRun {
								fxpr(fmt.Sprintf("  %s[dry-run] %s: %d key(s) would be injected%s", fxY, realSvcs[i].name, svcChanges, fxX))
							}
						}
					}
				}
			}

			if changed && !dryRun {
				joined := fxDedupServiceKeys(strings.Join(lines, "\n"))
				lines = strings.Split(joined, "\n")
				fxBackup(f)
				_ = os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)
				fxpr(fmt.Sprintf("  %s✔ %s: keys injected%s", fxG, stackName, fxX))
			}
		}
		if giChanges == 0 {
			fxpr(fmt.Sprintf("  %s✔ All global keys already present%s", fxG, fxX))
		}
		total += giChanges
	}

	// ── Phase 8: CPU core pinning ──
	gi = fxLoadGlobalInjectConf()
	if fxGIEnabled(fxGet(gi, "INJECT_CPUSET", "0")) {
		fxpr(fmt.Sprintf("\n%s🖥  CPU core pinning%s", fxC, fxX))
		cpuChanges := 0
		for _, f := range filesAlias {
			stackName := fxStripYmlExt(filepath.Base(f))
			stackPrefix := stackName
			if m := regexp.MustCompile(`^([a-zA-Z]+)`).FindStringSubmatch(stackName); m != nil {
				stackPrefix = m[1]
			}
			services, raw, err := fxParseServicesWithPositions(f)
			if err != nil {
				fxpr(fmt.Sprintf("  %s✘ %s: %v%s", fxR, stackName, err, fxX))
				continue
			}
			var fileLines []string
			for _, l := range raw {
				fileLines = append(fileLines, strings.TrimRight(l, "\n"))
			}
			var realSvcs []*fxService
			for _, s := range services {
				if !strings.HasPrefix(s.name, "provisioner") && s.image != "" && !fxReAlpine.MatchString(s.image) {
					realSvcs = append(realSvcs, s)
				}
			}
			if len(realSvcs) == 0 {
				continue
			}
			changed := false
			lines := append([]string{}, fileLines...)
			for i := len(realSvcs) - 1; i >= 0; i-- {
				var changes int
				lines, changes = fxInjectCpuset(lines, realSvcs[i], gi, stackPrefix, dryRun)
				if changes > 0 {
					cpuChanges += changes
					changed = true
					if dryRun {
						fxpr(fmt.Sprintf("  %s[dry-run] %s: cpuset would be set%s", fxY, realSvcs[i].name, fxX))
					}
				}
			}
			if changed && !dryRun {
				fxBackup(f)
				_ = os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)
				fxpr(fmt.Sprintf("  %s✔ %s: CPU pinning applied%s", fxG, stackName, fxX))
			}
		}
		if cpuChanges == 0 {
			fxpr(fmt.Sprintf("  %s✔ All CPU assignments already present%s", fxG, fxX))
		}
		total += cpuChanges
	}

	// ── Safety: re-validate; auto-dedup or revert on breakage ──
	if !dryRun {
		for _, sf := range files {
			if !safetyValid[sf] {
				continue
			}
			c := exec.Command("docker", "compose", "-f", sf, "config")
			c.Env = dockerEnv()
			var errb strings.Builder
			c.Stderr = &errb
			if c.Run() != nil {
				// stash a copy of the broken file
				if data, e := os.ReadFile(sf); e == nil {
					_ = os.WriteFile(filepath.Join("/tmp", "broken-"+filepath.Base(sf)), data, 0o644)
				}
				cur, _ := os.ReadFile(sf)
				dd := fxDedupServiceKeys(string(cur))
				if dd != string(cur) {
					_ = os.WriteFile(sf, []byte(dd), 0o644)
					c2 := exec.Command("docker", "compose", "-f", sf, "config")
					c2.Env = dockerEnv()
					if c2.Run() == nil {
						fxpr(fmt.Sprintf("%s✔ SAFETY: %s duplicate keys auto-deduped, now valid%s", fxG, filepath.Base(sf), fxX))
						continue
					}
				}
				_ = os.WriteFile(sf, []byte(safetyOrig[sf]), 0o644)
				errLines := strings.Split(strings.TrimSpace(errb.String()), "\n")
				reason := "unknown"
				if len(errLines) > 0 && errLines[len(errLines)-1] != "" {
					reason = errLines[len(errLines)-1]
				}
				fxpr(fmt.Sprintf("%s✘ SAFETY: %s broke validation after fix — REVERTED.%s", fxR, filepath.Base(sf), fxX))
				fxpr(fmt.Sprintf("%s   reason: %s%s", fxR, reason, fxX))
			}
		}
	}
	suffix := ""
	if dryRun {
		suffix = "(dry-run, none written)"
	}
	fxpr(fmt.Sprintf("\n%s✨ Done — %d change(s)%s%s\n", fxG, total, suffix, fxX))
}

// ── helpers reused / local ───────────────────────────────────────────────────

// fxStripYmlExt mirrors os.path.basename(f).replace('.yml',”).replace('.yaml',”).
func fxStripYmlExt(name string) string {
	name = strings.ReplaceAll(name, ".yml", "")
	name = strings.ReplaceAll(name, ".yaml", "")
	return name
}

// fxRealDefinedNets — faithful port of real_defined_nets(): networks defined WITH
// a subnet/ipam (true creators) across ALL files.
func fxRealDefinedNets(stacksDirPath string, skipFiles map[string]bool) map[string]bool {
	defined := map[string]bool{}
	if skipFiles == nil {
		skipFiles = map[string]bool{}
	}
	subRe := regexp.MustCompile(`^  ([a-zA-Z0-9][a-zA-Z0-9_.\-]*):(.*)$`)
	for _, f := range ngSortedYmls(stacksDirPath) {
		if skipFiles[f] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stacksDirPath, f))
		if err != nil {
			continue
		}
		inBlock := false
		for _, line := range strings.Split(string(data), "\n") {
			if regexp.MustCompile(`^networks:\s*$`).MatchString(line) {
				inBlock = true
				continue
			}
			if inBlock && fxReTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
				inBlock = false
			}
			if !inBlock {
				continue
			}
			if m := subRe.FindStringSubmatch(line); m != nil && (strings.Contains(m[2], "subnet") || strings.Contains(m[2], "ipam")) {
				defined[m[1]] = true
			}
		}
	}
	return defined
}

// fxRealDefinedVols — faithful port of real_defined_vols(): volumes defined anywhere
// as a top-level entry.
func fxRealDefinedVols(stacksDirPath string, skipFiles map[string]bool) map[string]bool {
	defined := map[string]bool{}
	if skipFiles == nil {
		skipFiles = map[string]bool{}
	}
	nameRe := regexp.MustCompile(`^  ([a-zA-Z0-9][a-zA-Z0-9_.\-]*):`)
	for _, f := range ngSortedYmls(stacksDirPath) {
		if skipFiles[f] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stacksDirPath, f))
		if err != nil {
			continue
		}
		inBlock := false
		for _, line := range strings.Split(string(data), "\n") {
			if regexp.MustCompile(`^volumes:\s*$`).MatchString(line) {
				inBlock = true
				continue
			}
			if inBlock && fxReTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
				inBlock = false
			}
			if !inBlock {
				continue
			}
			if m := nameRe.FindStringSubmatch(line); m != nil {
				defined[m[1]] = true
			}
		}
	}
	return defined
}

// fxConvertNamedToBind — faithful port of convert_named_to_bind().
func fxConvertNamedToBind(lines []string, volBase string, dryRun bool) ([]string, int) {
	// Write to temp file to reuse the service parser (as the Python does).
	tmp, err := os.CreateTemp("", "stacksfix-conv-*.yml")
	if err != nil {
		return lines, 0
	}
	tmpName := tmp.Name()
	_, _ = tmp.WriteString(strings.Join(lines, "\n"))
	tmp.Close()
	services, _, perr := fxParseServicesWithPositions(tmpName)
	os.Remove(tmpName)
	if perr != nil {
		return lines, 0
	}

	// external: true volumes at top level
	externalVols := map[string]bool{}
	inTopVols := false
	currentVol := ""
	nameRe := regexp.MustCompile(`^  ([a-zA-Z0-9_-]+):`)
	for _, line := range lines {
		if regexp.MustCompile(`^volumes:\s*$`).MatchString(line) {
			inTopVols = true
			currentVol = ""
			continue
		}
		if inTopVols {
			if line != "" && !isSpace(line[0]) {
				inTopVols = false
				currentVol = ""
				continue
			}
			if m := nameRe.FindStringSubmatch(line); m != nil {
				currentVol = m[1]
			}
			if currentVol != "" && strings.Contains(line, "external: true") {
				externalVols[currentVol] = true
			}
		}
	}

	// named_vol -> (svc_name, container_path)
	type nv struct{ svc, cpath string }
	namedVols := map[string]nv{}
	nameOnlyRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	startNumRe := regexp.MustCompile(`^[0-9]`)
	for _, svc := range services {
		inVol := false
		for i := svc.blockStart; i < svc.blockEnd+1 && i < len(lines); i++ {
			line := lines[i]
			stripped := strings.TrimSpace(line)
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if regexp.MustCompile(`^\s+volumes:\s*$`).MatchString(line) {
				inVol = true
				continue
			}
			if inVol && stripped != "" && !strings.HasPrefix(stripped, "-") && !strings.HasPrefix(stripped, "#") {
				if indent <= 4 {
					inVol = false
				}
			}
			if inVol && strings.HasPrefix(stripped, "-") {
				val := strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "-")), `"'`)
				if strings.Contains(val, ":") && !strings.HasPrefix(val, "/") && !strings.HasPrefix(val, ".") {
					parts := strings.Split(val, ":")
					volName := strings.TrimSpace(parts[0])
					cpath := strings.TrimSpace(strings.Join(parts[1:], ":"))
					if volName != "" && !startNumRe.MatchString(volName) && !strings.Contains(volName, ".") &&
						!strings.Contains(volName, " ") && nameOnlyRe.MatchString(volName) && !externalVols[volName] {
						namedVols[volName] = nv{svc.name, cpath}
					}
				}
			}
		}
	}

	// Replace named refs with bind mounts.
	var result []string
	changes := 0
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "-") {
			val := strings.Trim(strings.TrimSpace(strings.TrimPrefix(stripped, "-")), `"'`)
			if strings.Contains(val, ":") && !strings.HasPrefix(val, "/") && !strings.HasPrefix(val, ".") {
				parts := strings.Split(val, ":")
				volName := strings.TrimSpace(parts[0])
				cpath := strings.TrimSpace(strings.Join(parts[1:], ":"))
				if nvv, ok := namedVols[volName]; ok && !startNumRe.MatchString(volName) && nameOnlyRe.MatchString(volName) {
					svcName := nvv.svc
					newHost := filepath.Join(volBase, svcName)
					indent := len(line) - len(strings.TrimLeft(line, " "))
					newLine := strings.Repeat(" ", indent) + fmt.Sprintf(`- "%s:%s"`, newHost, cpath)
					if dryRun {
						result = append(result, line)
					} else {
						result = append(result, newLine)
					}
					changes++
					continue
				}
			}
		}
		result = append(result, line)
	}

	// Strip orphaned top-level local named volume declarations.
	stillUsed := map[string]bool{}
	for _, ln := range result {
		st := strings.TrimSpace(ln)
		if strings.HasPrefix(st, "-") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(st, "-")), `"'`)
			if strings.Contains(v, ":") && !strings.HasPrefix(v, "/") && !strings.HasPrefix(v, ".") {
				vn := strings.TrimSpace(strings.Split(v, ":")[0])
				if nameOnlyRe.MatchString(vn) {
					stillUsed[vn] = true
				}
			}
		}
	}
	var out2 []string
	inTV := false
	removedV := 0
	ridx := 0
	for ridx < len(result) {
		ln := result[ridx]
		if regexp.MustCompile(`^volumes:\s*$`).MatchString(ln) {
			inTV = true
			out2 = append(out2, ln)
			ridx++
			continue
		}
		if inTV {
			if ln != "" && !isSpace(ln[0]) {
				inTV = false
				out2 = append(out2, ln)
				ridx++
				continue
			}
			if m := nameRe.FindStringSubmatch(ln); m != nil {
				vn := m[1]
				blk := []string{ln}
				k := ridx + 1
				for k < len(result) && regexp.MustCompile(`^    `).MatchString(result[k]) {
					blk = append(blk, result[k])
					k++
				}
				blkText := strings.Join(blk, "\n")
				isExternal := strings.Contains(blkText, "external: true")
				if !stillUsed[vn] && !isExternal {
					removedV++
					ridx = k
					continue
				}
				out2 = append(out2, blk...)
				ridx = k
				continue
			}
			out2 = append(out2, ln)
			ridx++
			continue
		}
		out2 = append(out2, ln)
		ridx++
	}
	if removedV > 0 && !dryRun {
		var final []string
		i := 0
		for i < len(out2) {
			ln := out2[i]
			if regexp.MustCompile(`^volumes:\s*$`).MatchString(ln) {
				j := i + 1
				hasEntry := false
				for j < len(out2) {
					if out2[j] != "" && !isSpace(out2[j][0]) {
						break
					}
					if regexp.MustCompile(`^  [a-zA-Z0-9_-]+:`).MatchString(out2[j]) {
						hasEntry = true
						break
					}
					j++
				}
				if !hasEntry {
					i++
					continue
				}
			}
			final = append(final, ln)
			i++
		}
		result = final
		changes += removedV
	}

	return result, changes
}

// ===== from repair.go =====

// ── Templates learned from dev_1.yml (perfect reference file) ────────────────
var repairTemplates = map[string]string{
	"blkio_config": "    blkio_config: {weight: 500, device_read_bps: [{path: /dev/nvme0n1, rate: 500mb}], device_write_bps: [{path: /dev/nvme0n1, rate: 500mb}]}",
	"ulimits":      "    ulimits: {memlock: {soft: -1, hard: -1}, nofile: {soft: 65535, hard: 65535}, nproc: 65535}",
	"storage_opt":  "    storage_opt: {size: 10G}",
	"deploy":       "    deploy: {placement: {constraints: [node.labels.priority == high]}, resources: {limits: {memory: 1G, cpus: '0.2', pids: 1000}, reservations: {memory: 100M, cpus: '0.05'}}}",
}

const (
	repairLabelIndent   = "      " // 6 spaces
	repairServiceIndent = "  "     // 2 spaces
)

var repairNetworkPriorities = map[string]int{"traefik_net": 1000}

const repairDefaultNetPriority = 500

// _states_health → repairStatesHealth.
// {name: (status, health)} for every container in ONE Docker API call.
type repairStateHealth struct {
	status string
	health string
}

func repairStatesHealth() map[string]repairStateHealth {
	out := map[string]repairStateHealth{}
	for n, i := range containerInfo() {
		out[n] = repairStateHealth{status: i.State, health: i.Health}
	}
	return out
}

// snapConf mirrors _snap_conf().
type repairSnapConf struct {
	dir       string
	keep      int
	require   string
	onSuccess bool
	use       bool
}

// repairConfPath returns the global_inject.conf path (universal).
func repairConfPath() string {
	return filepath.Join(configDir(), "global_inject.conf")
}

// repairReadConf reads a simple KEY=VALUE conf file (skipping blanks/comments).
func repairReadConf(path string) map[string]string {
	cfg := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		cfg[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return cfg
}

func repairBoolish(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "1" || s == "true"
}

func _snap_conf() repairSnapConf {
	cfg := repairReadConf(repairConfPath())
	// YAML master overlay
	for k, v := range loadNamed("global_inject") {
		cfg[k] = v
	}
	get := func(k, def string) string {
		if v, ok := cfg[k]; ok {
			return v
		}
		return def
	}
	keep := 5
	if v := strings.TrimSpace(get("SNAPSHOT_KEEP", "5")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			keep = n
		}
	}
	dir := get("SNAPSHOT_DIR", filepath.Join(configDir(), "snapshots"))
	dir = repairExpandUser(dir)
	return repairSnapConf{
		dir:       dir,
		keep:      keep,
		require:   get("SNAPSHOT_REQUIRE", "none-failed"),
		onSuccess: repairBoolish(get("SNAPSHOT_ON_SUCCESS", "1")),
		use:       repairBoolish(get("REPAIR_USE_SNAPSHOT", "1")),
	}
}

// repairExpandUser approximates os.path.expanduser for leading ~ / ~user.
func repairExpandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	// ~loveiznothin/... or ~/...  -> map any leading ~user onto the universal home.
	rest := p[1:]
	if rest == "" {
		return home()
	}
	if strings.HasPrefix(rest, "/") {
		return filepath.Join(home(), rest[1:])
	}
	// ~user/path — strip the username component, splice onto home().
	if idx := strings.Index(rest, "/"); idx >= 0 {
		return filepath.Join(home(), rest[idx+1:])
	}
	return home()
}

// _validate — True if `docker compose config` succeeds.
func _validate(path string) bool {
	return cli("compose", "-f", path, "config").exitCode == 0
}

// _stack_services
func _stack_services(path string) []string {
	r := cli("compose", "-f", path, "config", "--services")
	if r.exitCode != 0 {
		return nil
	}
	var out []string
	for _, x := range strings.Fields(r.stdout) {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// _stack_state_ok
func _stack_state_ok(path, require string) bool {
	svcs := _stack_services(path)
	if len(svcs) == 0 {
		return false
	}
	bad := map[string]bool{"restarting": true, "dead": true, "removing": true}
	info := repairStatesHealth()
	for _, svc := range svcs {
		sh, ok := info[svc]
		if !ok {
			if require == "all-healthy" {
				return false
			}
			continue // not created = sleeping/Sablier, OK for none-failed
		}
		status, health := sh.status, sh.health
		if bad[status] {
			return false
		}
		if status != "running" {
			if require == "all-healthy" {
				return false
			}
			continue
		}
		// running:
		if require == "all-healthy" && health != "" && health != "healthy" {
			return false
		}
	}
	return true
}

// snapshot_if_proven
func snapshot_if_proven(path string) string {
	c := _snap_conf()
	if !c.onSuccess {
		return ""
	}
	if !_validate(path) {
		return ""
	}
	if !_stack_state_ok(path, c.require) {
		return ""
	}
	os.MkdirAll(c.dir, 0o755)
	stack := repairStripExt(filepath.Base(path))
	snap := filepath.Join(c.dir, fmt.Sprintf("%s.good.%d", stack, time.Now().Unix()))
	repairCopy2(path, snap)
	repairPruneSnapshots(c.dir, stack, c.keep)
	return snap
}

// _snapshots_for — this stack's .good snapshots, newest first.
func _snapshots_for(stack string) []string {
	c := _snap_conf()
	g, _ := filepath.Glob(filepath.Join(c.dir, fmt.Sprintf("%s.good.*", stack)))
	sort.Sort(sort.Reverse(sort.StringSlice(g)))
	return g
}

// _deploy_health_ok
func _deploy_health_ok(path, require string, settle int) bool {
	svcs := _stack_services(path)
	if len(svcs) == 0 {
		return false
	}
	settleN := settle
	if settleN < 1 {
		settleN = 1
	}
	deadline := time.Now().Add(time.Duration(settleN) * time.Second)
	bad := map[string]bool{"restarting": true, "dead": true, "removing": true}
	for {
		pending := false
		ok := true
		info := repairStatesHealth()
		for _, svc := range svcs {
			sh, exists := info[svc]
			if !exists {
				continue // not created — not part of what came up
			}
			status, health := sh.status, sh.health
			if bad[status] {
				ok = false
				break
			}
			if status != "running" {
				continue
			}
			if health == "starting" {
				pending = true
			} else if health == "unhealthy" {
				if require == "all-healthy" {
					ok = false
					break
				}
			}
		}
		if !ok {
			return false
		}
		if !pending || !time.Now().Before(deadline) {
			return ok
		}
		time.Sleep(2 * time.Second)
	}
}

// _save_snapshot
func _save_snapshot(path string, c repairSnapConf) string {
	os.MkdirAll(c.dir, 0o755)
	stack := repairStripExt(filepath.Base(path))
	snap := filepath.Join(c.dir, fmt.Sprintf("%s.good.%d", stack, time.Now().Unix()))
	repairCopy2(path, snap)
	repairPruneSnapshots(c.dir, stack, c.keep)
	return snap
}

// snapshot_after_up
func snapshot_after_up(path string) string {
	c := _snap_conf()
	if !c.onSuccess || !_validate(path) {
		return ""
	}
	settle := _snap_conf_int("SNAPSHOT_SETTLE_SECS", 15)
	if _deploy_health_ok(path, c.require, settle) {
		return _save_snapshot(path, c)
	}
	return ""
}

// _snap_conf_int
func _snap_conf_int(key string, def int) int {
	// YAML master first.
	if v, ok := loadNamed("global_inject")[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	// then the .conf scan
	data, err := os.ReadFile(repairConfPath())
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+"=") {
				v := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
	}
	return def
}

// repairStripExt removes .yml/.yaml.
func repairStripExt(name string) string {
	name = strings.ReplaceAll(name, ".yml", "")
	name = strings.ReplaceAll(name, ".yaml", "")
	return name
}

// repairCopy2 copies a file preserving mode (best-effort, like shutil.copy2).
func repairCopy2(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(src); err == nil {
		mode = fi.Mode()
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	if fi, err := os.Stat(src); err == nil {
		os.Chtimes(dst, time.Now(), fi.ModTime())
	}
	return nil
}

// repairPruneSnapshots keeps the newest N "<stack>.good.*" files.
func repairPruneSnapshots(dir, stack string, keep int) {
	existing, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%s.good.*", stack)))
	sort.Strings(existing)
	if len(existing) <= keep {
		return
	}
	for _, old := range existing[:len(existing)-keep] {
		os.Remove(old)
	}
}

// repair_file — run all repair passes on a single compose file. Returns fixes.
func repair_file(path string, dryRun bool) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)
	original := content
	var fixes []string

	var f []string

	content, f = fix_corrupt_blkio(content)
	fixes = append(fixes, f...)

	content, f = fix_labels_in_networks(content)
	fixes = append(fixes, f...)

	content, f = fix_duplicate_labels(content)
	fixes = append(fixes, f...)

	content, f = fix_missing_closing_quotes(content)
	fixes = append(fixes, f...)

	content, f = fix_n_labels(content)
	fixes = append(fixes, f...)

	content, f = fix_name_field(content, path)
	fixes = append(fixes, f...)

	// ── Structural passes (dedup + phantom depends_on) ──
	content, f = fix_duplicate_service_keys(content)
	fixes = append(fixes, f...)

	content, f = fix_undefined_depends(content)
	fixes = append(fixes, f...)

	content, f = fix_dependency_cycles(content)
	fixes = append(fixes, f...)

	content, f = fix_network_form(content)
	fixes = append(fixes, f...)

	content, f = fix_undefined_networks(content)
	fixes = append(fixes, f...)

	// orphan network removal — gated on FIX_REMOVE_ORPHANS (default off)
	removeOrphans := false
	answered := false
	if ro, ok := configLoad()["FIX_REMOVE_ORPHANS"]; ok {
		v := strings.Trim(strings.TrimSpace(ro), "\"")
		removeOrphans = v == "1" || v == "on" || v == "true" || v == "True"
		answered = true // YAML answered; skip the .conf scan below
	}
	if !answered {
		confPath := filepath.Join(configDir(), "stacks.conf")
		if cdata, err := os.ReadFile(confPath); err == nil {
			for _, line := range strings.Split(string(cdata), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "FIX_REMOVE_ORPHANS=") {
					v := strings.Trim(strings.TrimSpace(strings.SplitN(strings.TrimSpace(line), "=", 2)[1]), "\"")
					removeOrphans = v == "1" || v == "on" || v == "true" || v == "True"
				}
			}
		}
	}
	if removeOrphans {
		content, f = fix_orphan_networks(content)
		fixes = append(fixes, f...)
	}

	if !dryRun && content != original {
		// back up the broken file before writing the repaired version
		bdir := filepath.Join(configDir(), "snapshots", "repair-backups")
		if os.MkdirAll(bdir, 0o755) == nil {
			stack := filepath.Base(path)
			repairCopy2(path, filepath.Join(bdir, fmt.Sprintf("%s.broken.%d", stack, time.Now().Unix())))
		}
		os.WriteFile(path, []byte(content), 0o644)
	}

	return fixes
}

// ── individual fixers ────────────────────────────────────────────────────────

var reBlkio = regexp.MustCompile(`device_read_bps:\s*\[[^\]]*(?:CMD|NONE|SHELL)[^\]]*\]`)

func fix_corrupt_blkio(content string) (string, []string) {
	var fixes []string
	if reBlkio.MatchString(content) {
		content = reBlkio.ReplaceAllString(content, "device_read_bps: [{path: /dev/nvme0n1, rate: 500mb}]")
		fixes = append(fixes, "corrupt_blkio: HC test leaked into blkio_config")
	}
	return content, fixes
}

var (
	reNetworksTop = regexp.MustCompile(`^networks:\s*$`)
	reTopLevel    = regexp.MustCompile(`^[a-zA-Z\[]`)
	reLabelInNet  = regexp.MustCompile(`^\s+- "(traefik\.|sablier\.)`)
)

func fix_labels_in_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	inNetworks := false

	for _, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNetworks = true
		} else if reTopLevel.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNetworks = false
		}

		if inNetworks && reLabelInNet.MatchString(line) {
			fixes = append(fixes, fmt.Sprintf(`labels_in_networks: removed "%s"`, strings.TrimSpace(line)))
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var (
	reSvcKey2     = regexp.MustCompile(`^  [a-zA-Z0-9_-]+:\s*$`)
	reLabelsBlock = regexp.MustCompile(`^\s+labels:\s*$`)
)

func fix_duplicate_labels(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	seenLabels := map[string]bool{}
	inLabels := false

	for _, line := range lines {
		// Reset on new service
		if reSvcKey2.MatchString(line) {
			inLabels = false
			seenLabels = map[string]bool{}
		}

		if reLabelsBlock.MatchString(line) {
			inLabels = true
			seenLabels = map[string]bool{}
			result = append(result, line)
			continue
		}

		if inLabels {
			if !strings.HasPrefix(strings.TrimSpace(line), "-") {
				inLabels = false
			} else {
				stripped := strings.TrimSpace(line)
				if strings.Contains(stripped, "traefik.enable=") ||
					strings.Contains(stripped, "sablier.enable=") ||
					strings.Contains(stripped, "sablier.group=") {
					if seenLabels[stripped] {
						fixes = append(fixes, "duplicate_label: removed duplicate "+stripped)
						continue
					}
					seenLabels[stripped] = true
				}
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reSablierGroup = regexp.MustCompile(`sablier\.group=([a-zA-Z0-9_-]+)`)

func fix_missing_closing_quotes(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "sablier.group=") {
			m := reSablierGroup.FindStringSubmatch(line)
			if m != nil {
				val := m[1]
				expected := fmt.Sprintf(`sablier.group=%s"`, val)
				if !strings.Contains(line, expected) {
					re := regexp.MustCompile(`sablier\.group=` + regexp.QuoteMeta(val) + `([^a-zA-Z0-9_"-]|$)`)
					line = re.ReplaceAllString(line, fmt.Sprintf(`sablier.group=%s"$1`, val))
					fixes = append(fixes, "missing_quote: fixed sablier.group="+val)
				}
			}
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reSingleLetterLabel = regexp.MustCompile(`^\s+- "[a-z]"\s*$`)

func fix_n_labels(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var result []string
	for _, line := range lines {
		if reSingleLetterLabel.MatchString(line) {
			fixes = append(fixes, "corrupt_label: removed "+strings.TrimSpace(line))
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n"), fixes
}

var reNameField = regexp.MustCompile(`^name:\s*`)

func fix_name_field(content, path string) (string, []string) {
	var fixes []string
	stackName := repairStripExt(filepath.Base(path))
	lines := strings.Split(content, "\n")

	// Remove any existing name: lines (check first 5 for the correct one)
	hasCorrect := false
	limit := len(lines)
	if limit > 5 {
		limit = 5
	}
	for _, l := range lines[:limit] {
		if l == "name: "+stackName {
			hasCorrect = true
			break
		}
	}
	if hasCorrect {
		return content, fixes
	}

	var kept []string
	for _, l := range lines {
		if reNameField.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	lines = kept

	// Insert after leading comments
	insertPos := 0
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			insertPos = i + 1
		} else {
			break
		}
	}

	lines = insertAt(lines, insertPos, "name: "+stackName)
	fixes = append(fixes, "name_field: set to "+stackName)
	return strings.Join(lines, "\n"), fixes
}

var (
	reSvcNetworksBlock = regexp.MustCompile(`^    networks:\s*$`)
	reNetListItem      = regexp.MustCompile(`^      -\s+"?([a-zA-Z0-9_.-]+)"?\s*$`)
	reNetMapKey        = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*$`)
	reNetChild8        = regexp.MustCompile(`^        \S`)
	reSixIndent        = regexp.MustCompile(`^      `)
)

func fix_network_form(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	var out []string
	i := 0
	n := len(lines)
	for i < n {
		l := lines[i]
		if reSvcNetworksBlock.MatchString(l) {
			out = append(out, l)
			i++
			var nets []string // preserve order, dedupe
			seen := map[string]bool{}
			mixed := false
			sawList := false
			sawMap := false
			for i < n {
				lm := reNetListItem.FindStringSubmatch(lines[i])
				mm := reNetMapKey.FindStringSubmatch(lines[i])
				if lm != nil {
					sawList = true
					net := lm[1]
					if !seen[net] {
						seen[net] = true
						nets = append(nets, net)
					}
					i++
				} else if mm != nil {
					sawMap = true
					net := mm[1]
					if !seen[net] {
						seen[net] = true
						nets = append(nets, net)
					}
					i++
					// skip its child lines (priority etc, 8-space)
					for i < n && reNetChild8.MatchString(lines[i]) {
						i++
					}
				} else if reSixIndent.MatchString(lines[i]) {
					i++ // stray indented line, skip
				} else {
					break
				}
			}
			if sawList && sawMap {
				mixed = true
			}
			// rebuild in mapping form
			for _, net := range nets {
				pri := repairDefaultNetPriority
				if net == "traefik_net" {
					pri = 1000
				}
				out = append(out, fmt.Sprintf("      %s:", net))
				out = append(out, fmt.Sprintf("        priority: %d", pri))
			}
			if mixed {
				fixes = append(fixes, "network_form: normalized mixed list/mapping networks block to mapping form")
			} else if sawList {
				fixes = append(fixes, "network_form: converted list-form networks to mapping form")
			}
			continue
		}
		out = append(out, l)
		i++
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reTopLevelAlpha = regexp.MustCompile(`^[a-zA-Z]`)
	reTopNetKey     = regexp.MustCompile(`^  ([a-zA-Z0-9_.-]+):`)
	reUsedMapKey    = regexp.MustCompile(`(?m)^      ([a-zA-Z0-9_.-]+):`)
	reUsedListItem  = regexp.MustCompile(`(?m)^\s+-\s+"?([a-zA-Z0-9_.-]+)"?\s*$`)
)

func fix_orphan_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// collect top-level declared nets (under 'networks:')
	declared := map[string]int{}
	inNet := false
	for idx, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNet = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNet = false
		}
		if inNet {
			m := reTopNetKey.FindStringSubmatch(line)
			if m != nil {
				declared[m[1]] = idx
			}
		}
	}
	// collect nets referenced anywhere in a service networks block (6-space) or list form
	used := map[string]bool{}
	for _, m := range reUsedMapKey.FindAllStringSubmatch(content, -1) {
		used[m[1]] = true
	}
	for _, m := range reUsedListItem.FindAllStringSubmatch(content, -1) {
		used[m[1]] = true
	}
	// never remove traefik_net (universal) even if it looks unused
	protected := map[string]bool{"traefik_net": true}
	orphans := map[string]bool{}
	for net := range declared {
		if !used[net] && !protected[net] && strings.HasSuffix(net, "_net") {
			orphans[net] = true
		}
	}
	if len(orphans) == 0 {
		return content, fixes
	}
	// remove each orphan's declaration line (handles one-line {..} form)
	var out []string
	for _, line := range lines {
		m := reTopNetKey.FindStringSubmatch(line)
		if m != nil && orphans[m[1]] {
			fixes = append(fixes, fmt.Sprintf("orphan_network: removed unused '%s'", m[1]))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reNetKey6     = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*$`)
	reNetKey6Inl  = regexp.MustCompile(`^      ([a-zA-Z0-9_.-]+):\s*\{`)
	reFourNonWS   = regexp.MustCompile(`^    \S`)
	reTwoNonWS    = regexp.MustCompile(`^  \S`)
	reZeroNonWS   = regexp.MustCompile(`^\S`)
	reSvcNetEntry = regexp.MustCompile(`^(    )networks:\s*$`)
)

func fix_undefined_networks(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// defined top-level networks
	defined := map[string]bool{}
	inNet := false
	for _, line := range lines {
		if reNetworksTop.MatchString(line) {
			inNet = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inNet = false
		}
		if inNet {
			m := reTopNetKey.FindStringSubmatch(line)
			if m != nil {
				defined[m[1]] = true
			}
		}
	}
	if !defined["traefik_net"] {
		return content, fixes // safety
	}

	var out []string
	i := 0
	n := len(lines)
	for i < n {
		line := lines[i]
		if !reSvcNetEntry.MatchString(line) {
			out = append(out, line)
			i++
			continue
		}
		// entering a service-level networks: block (4-space)
		out = append(out, line)
		i++
		keptAny := false
		for i < n {
			nl := reNetKey6.FindStringSubmatch(lines[i])
			nlInline := reNetKey6Inl.FindStringSubmatch(lines[i])
			if nl != nil || nlInline != nil {
				var net string
				if nl != nil {
					net = nl[1]
				} else {
					net = nlInline[1]
				}
				if defined[net] {
					out = append(out, lines[i])
					i++
					keptAny = true
					// keep its child lines (8-space) if block form
					if nl != nil {
						for i < n && reNetChild8.MatchString(lines[i]) {
							out = append(out, lines[i])
							i++
						}
					}
				} else {
					fixes = append(fixes, fmt.Sprintf("undefined_network: removed '%s' from service", net))
					i++
					// skip its child lines (8-space)
					for i < n && reNetChild8.MatchString(lines[i]) {
						i++
					}
				}
			} else if reFourNonWS.MatchString(lines[i]) || reTwoNonWS.MatchString(lines[i]) || reZeroNonWS.MatchString(lines[i]) {
				break // left the networks block
			} else {
				out = append(out, lines[i])
				i++
			}
		}
		if !keptAny {
			out = append(out, "      traefik_net:")
			out = append(out, "        priority: 1000")
			fixes = append(fixes, "undefined_network: service left networkless -> added traefik_net")
		}
	}
	return strings.Join(out, "\n"), fixes
}

var (
	reSvcKeyFull = regexp.MustCompile(`^  ([a-zA-Z0-9_.+-]+):\s*$`)
	reDependsOn4 = regexp.MustCompile(`^    depends_on:\s*$`)
	reDepEntry   = regexp.MustCompile(`^      -\s+(["']?)([a-zA-Z0-9_.+-]+)["']?\s*$`)
)

func fix_dependency_cycles(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// map each service -> set of deps, and remember line index of each dep entry
	graph := map[string]map[string]bool{}
	depLines := map[[2]string]int{} // (svc, dep) -> line index
	// preserve insertion order to match Python's `list(graph)` (dict insertion order)
	var svcOrder []string
	depOrder := map[string][]string{} // svc -> deps in insertion order
	cur := ""
	inDep := false
	for i, line := range lines {
		m := reSvcKeyFull.FindStringSubmatch(line)
		if m != nil {
			cur = m[1]
			if graph[cur] == nil {
				graph[cur] = map[string]bool{}
				svcOrder = append(svcOrder, cur)
			}
			inDep = false
			continue
		}
		if cur == "" {
			continue
		}
		if reDependsOn4.MatchString(line) {
			inDep = true
			continue
		}
		if inDep {
			dm := reDepEntry.FindStringSubmatch(line)
			if dm != nil {
				if !graph[cur][dm[2]] {
					depOrder[cur] = append(depOrder[cur], dm[2])
				}
				graph[cur][dm[2]] = true
				depLines[[2]string{cur, dm[2]}] = i
			} else {
				inDep = false
			}
		}
	}
	// find 2-node cycles (iterate services in file order, like Python's list(graph))
	remove := map[int]bool{} // line indices to drop
	for _, a := range svcOrder {
		for _, b := range depOrder[a] {
			if !graph[a][b] {
				continue // edge already removed
			}
			if graph[b] != nil && graph[b][a] {
				// cycle a<->b. Drop the edge from the service with MORE deps.
				var victim, other string
				if len(graph[a]) >= len(graph[b]) {
					victim, other = a, b
				} else {
					victim, other = b, a
				}
				key := [2]string{victim, other}
				if idx, ok := depLines[key]; ok {
					remove[idx] = true
					delete(graph[victim], other)
					fixes = append(fixes, fmt.Sprintf("dependency_cycle: removed '%s' from %s.depends_on", other, victim))
				}
			}
		}
	}
	if len(remove) == 0 {
		return content, fixes
	}
	var newLines []string
	for i, l := range lines {
		if !remove[i] {
			newLines = append(newLines, l)
		}
	}
	return strings.Join(newLines, "\n"), fixes
}

var (
	reServicesTop   = regexp.MustCompile(`^services:\s*$`)
	reDependsAny    = regexp.MustCompile(`^(\s+)depends_on:\s*$`)
	reDepEntryAny   = regexp.MustCompile(`^\s+-\s+(["']?)([a-zA-Z0-9_.+-]+)["']?\s*$`)
	reDependsInline = regexp.MustCompile(`^(\s+)depends_on:\s*\[(.*)\]\s*$`)
)

func fix_undefined_depends(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// collect all defined service names (2-space indent, under services:)
	defined := map[string]bool{}
	inServices := false
	for _, line := range lines {
		if reServicesTop.MatchString(line) {
			inServices = true
			continue
		}
		if reTopLevelAlpha.MatchString(line) && !strings.HasPrefix(line, " ") {
			inServices = false
		}
		if inServices {
			m := reSvcKeyFull.FindStringSubmatch(line)
			if m != nil {
				defined[m[1]] = true
			}
		}
	}
	if len(defined) == 0 {
		return content, fixes
	}
	var out []string
	i := 0
	n := len(lines)
	for i < n {
		line := lines[i]
		// detect a depends_on: block (list form) at indent
		m := reDependsAny.FindStringSubmatch(line)
		if m != nil {
			indent := m[1]
			j := i + 1
			var kept []string
			var removed []string
			for j < n {
				dm := reDepEntryAny.FindStringSubmatch(lines[j])
				if dm != nil && (len(lines[j])-len(strings.TrimLeft(lines[j], " \t"))) > len(indent) {
					dep := dm[2]
					if defined[dep] {
						kept = append(kept, lines[j])
					} else {
						removed = append(removed, dep)
					}
					j++
				} else {
					break
				}
			}
			if len(removed) > 0 {
				for _, d := range removed {
					fixes = append(fixes, fmt.Sprintf("undefined_depends: removed '%s'", d))
				}
				if len(kept) > 0 {
					out = append(out, line)
					out = append(out, kept...)
				}
				// if nothing kept, drop the depends_on: line entirely
				i = j
				continue
			}
			out = append(out, line)
			i++
			continue
		}
		// also handle inline form: depends_on: [a, b]
		mi := reDependsInline.FindStringSubmatch(line)
		if mi != nil {
			indent, body := mi[1], mi[2]
			var deps []string
			for _, d := range strings.Split(body, ",") {
				d = strings.TrimSpace(d)
				if d == "" {
					continue
				}
				d = strings.Trim(d, "\"'")
				deps = append(deps, d)
			}
			var keep, drop []string
			for _, d := range deps {
				if defined[d] {
					keep = append(keep, d)
				} else {
					drop = append(drop, d)
				}
			}
			if len(drop) > 0 {
				for _, d := range drop {
					fixes = append(fixes, fmt.Sprintf("undefined_depends: removed '%s'", d))
				}
				if len(keep) > 0 {
					out = append(out, fmt.Sprintf("%sdepends_on: [%s]", indent, strings.Join(keep, ", ")))
				}
				i++
				continue
			}
		}
		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n"), fixes
}

type repairBlock struct {
	name  string
	start int
	end   int
}

func fix_duplicate_service_keys(content string) (string, []string) {
	var fixes []string
	lines := strings.Split(content, "\n")
	// find service block boundaries: lines matching ^  <name>:  (2-space indent)
	var blocks []repairBlock
	var cur *repairBlock
	for i, line := range lines {
		m := reSvcKeyFull.FindStringSubmatch(line)
		if m != nil {
			if cur != nil {
				blocks = append(blocks, repairBlock{cur.name, cur.start, i})
			}
			cur = &repairBlock{name: m[1], start: i}
		} else if reTopLevelAlpha.MatchString(line) && cur != nil {
			// left the services section
			blocks = append(blocks, repairBlock{cur.name, cur.start, i})
			cur = nil
		}
	}
	if cur != nil {
		blocks = append(blocks, repairBlock{cur.name, cur.start, len(lines)})
	}
	// group by name (preserve first-seen order)
	byName := map[string][]repairBlock{}
	var nameOrder []string
	for _, b := range blocks {
		if _, ok := byName[b.name]; !ok {
			nameOrder = append(nameOrder, b.name)
		}
		byName[b.name] = append(byName[b.name], b)
	}
	var dropRanges []repairBlock
	score := func(b repairBlock) int {
		c := 0
		for _, l := range lines[b.start:b.end] {
			if strings.TrimSpace(l) != "" {
				c++
			}
		}
		return c
	}
	for _, name := range nameOrder {
		bl := byName[name]
		if len(bl) < 2 {
			continue
		}
		// score each by number of non-blank lines (completeness); keep the max.
		// Python's sorted is stable; emulate with a stable sort by descending score.
		scored := make([]repairBlock, len(bl))
		copy(scored, bl)
		sort.SliceStable(scored, func(a, b int) bool {
			return score(scored[a]) > score(scored[b])
		})
		for _, b := range scored[1:] {
			dropRanges = append(dropRanges, b)
			fixes = append(fixes, fmt.Sprintf("duplicate_service: removed second '%s' block (lines %d-%d)", name, b.start+1, b.end))
		}
	}
	if len(dropRanges) == 0 {
		return content, fixes
	}
	drop := map[int]bool{}
	for _, b := range dropRanges {
		for k := b.start; k < b.end; k++ {
			drop[k] = true
		}
	}
	var newLines []string
	for i, l := range lines {
		if !drop[i] {
			newLines = append(newLines, l)
		}
	}
	return strings.Join(newLines, "\n"), fixes
}

var reLineNum = regexp.MustCompile(`line (\d+)`)

// _compose_error — run compose config, return (ok, line_no, message). lineNo 0 if none.
func _compose_error(path string) (bool, int, string) {
	r := cli("compose", "-f", path, "config")
	if r.exitCode == 0 {
		return true, 0, ""
	}
	err := strings.TrimSpace(r.stderr)
	// filter the harmless unset-variable warnings
	var lines []string
	for _, l := range strings.Split(err, "\n") {
		if strings.Contains(l, "variable is not set") || strings.Contains(l, "AK_OUTPOST") {
			continue
		}
		lines = append(lines, l)
	}
	msg := err
	if len(lines) > 0 {
		msg = lines[len(lines)-1]
	}
	// try to extract a line number
	lno := 0
	matches := reLineNum.FindAllStringSubmatch(msg, -1)
	if len(matches) > 0 {
		if n, e := strconv.Atoi(matches[len(matches)-1][1]); e == nil {
			lno = n // the deepest/last line number compose reports
		}
	}
	return false, lno, msg
}

// repair_loop — error-driven surgical repair.
func repair_loop(path string, maxPasses int, logf string) []string {
	var actions []string
	logFn := func(m string) {
		actions = append(actions, m)
		if logf != "" {
			f, err := os.OpenFile(logf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err == nil {
				f.WriteString(m + "\n")
				f.Close()
			}
		}
	}

	lastErr := ""
	haveLast := false
	for pass := 0; pass < maxPasses; pass++ {
		ok, lno, msg := _compose_error(path)
		if ok {
			logFn(fmt.Sprintf("repair_loop: VALID after %d pass(es)", pass))
			return actions
		}
		if haveLast && msg == lastErr {
			// no progress on the same error -> stop to avoid infinite loop
			logFn("repair_loop: STUCK on: " + msg)
			break
		}
		lastErr = msg
		haveLast = true
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		content := string(data)
		before := content
		// classify + dispatch to the right in-place fixer
		fixedBy := ""
		ml := strings.ToLower(msg)
		var f []string
		if strings.Contains(ml, "did not find expected") || strings.Contains(ml, "mapping values") ||
			strings.Contains(ml, "block collection") || strings.Contains(ml, "found character") {
			content, f = fix_network_form(content) // mixed list/mapping nets
			if len(f) > 0 {
				fixedBy = "network_form"
			}
			if len(f) == 0 {
				content2, f2 := _fix_indent_at(content, lno) // generic indent repair
				if f2 {
					content = content2
					fixedBy = "indent"
				}
			}
		} else if strings.Contains(ml, "already defined") || strings.Contains(ml, "are equal") || strings.Contains(ml, "duplicate") {
			content, f = fix_duplicate_service_keys(content)
			if len(f) > 0 {
				fixedBy = "dup_service"
			}
			if len(f) == 0 {
				content, f = fix_network_form(content) // dedupes net lists too
				if len(f) > 0 {
					fixedBy = "dup_network"
				}
			}
		} else if strings.Contains(ml, "depends on undefined service") {
			content, f = fix_undefined_depends(content)
			if len(f) > 0 {
				fixedBy = "undefined_depends"
			}
		} else if strings.Contains(ml, "undefined network") {
			content, f = fix_undefined_networks(content)
			if len(f) > 0 {
				fixedBy = "undefined_network"
			}
		} else if strings.Contains(ml, "cycle") {
			content, f = fix_dependency_cycles(content)
			if len(f) > 0 {
				fixedBy = "dependency_cycle"
			}
		}
		if fixedBy != "" && content != before {
			repairCopy2(path, path+".prerepair")
			os.WriteFile(path, []byte(content), 0o644)
			logFn(fmt.Sprintf("repair_loop pass %d: %s -> fixed (%s)", pass, repairTrunc(msg, 80), fixedBy))
		} else {
			logFn(fmt.Sprintf("repair_loop pass %d: NO FIXER for: %s", pass, repairTrunc(msg, 120)))
			break
		}
	}
	return actions
}

// repairTrunc emulates Python slicing msg[:N].
func repairTrunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// _fix_indent_at — generic indentation repair near a reported line.
func _fix_indent_at(content string, lno int) (string, bool) {
	if lno == 0 {
		return content, false
	}
	lines := strings.Split(content, "\n")
	i := lno - 1
	if i < 0 || i >= len(lines) {
		return content, false
	}
	fixed := false
	start := i - 2
	if start < 0 {
		start = 0
	}
	end := i + 2
	if end > len(lines) {
		end = len(lines)
	}
	for j := start; j < end; j++ {
		l := lines[j]
		st := strings.TrimLeft(l, " ")
		ind := len(l) - len(st)
		// service-level keys should be 4 spaces; list items 6; net children 8
		if st != "" && !strings.HasPrefix(st, "-") && strings.HasSuffix(st, ":") && (ind == 3 || ind == 5) {
			newInd := ind
			if ind == 3 || ind == 5 {
				newInd = ind + 1
			}
			lines[j] = strings.Repeat(" ", newInd) + st
			fixed = true
		}
	}
	if fixed {
		return strings.Join(lines, "\n"), true
	}
	return content, false
}

// scan_all — scan all yml files and repair them.
func scan_all(stacksDirPath string, dryRun bool) {
	totalFixes := 0
	entries, err := os.ReadDir(stacksDirPath)
	if err != nil {
		fmt.Printf("\nTotal fixes: %d\n", totalFixes)
		return
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, fname := range names {
		if !strings.HasSuffix(fname, ".yml") {
			continue
		}
		path := filepath.Join(stacksDirPath, fname)
		fixes := repair_file(path, dryRun)
		if len(fixes) > 0 {
			prefix := ""
			if dryRun {
				prefix = "[dry-run] "
			}
			fmt.Printf("%sFixed %s:\n", prefix, fname)
			for _, f := range fixes {
				fmt.Printf("  - %s\n", f)
			}
			totalFixes += len(fixes)
		}
	}
	fmt.Printf("\nTotal fixes: %d\n", totalFixes)
}
