package main

// gendynamic2.go — faithful Go port of stacks_gen_dynamic2.py.
//
// Rich, config-driven Traefik dynamics generator. Single source of truth for the
// dynamic files: scans the compose stacks and emits the FULL rich set
// (serversTransports + all middlewares + HTTP routers + per-service Sablier +
// TCP DB routers on DEDICATED entrypoints). Everything is driven by
// <configDir>/dynamics.yaml — nothing is hardcoded, so it works for anyone's
// stacks.
//
// Universal paths: STACKS_DIR / DYNAMICS_DIR come from env (set by `stacks`) →
// master config (stacks.yaml: stacks_folder / dynamics_folder) → generic default
// derived from home(). No user identity is ever hardcoded.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ── Paths: generic for any user ───────────────────────────────────────────────
//
//	env var (exported by `stacks`) → master config (stacks.yaml) → sane default.
func gd2Path(env, mckey, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	// master config: stacks.yaml maps friendly keys -> internal scalar keys.
	mc := configLoad()
	if v := mc[mckey]; v != "" {
		return v
	}
	return def
}

func gd2DefBase() string { return filepath.Join(home(), "MyDocker") }
func gd2StacksDir() string {
	return gd2Path("STACKS_DIR", "STACKS_DIR", filepath.Join(gd2DefBase(), "Stacks"))
}
func gd2DynDir() string {
	return gd2Path("DYNAMICS_DIR", "DYNAMICS_DIR", filepath.Join(gd2DefBase(), "Configs", "Dynamics"))
}

// gd2FindTraefikStack auto-detects which compose file defines the traefik service
// (where the entrypoints live). Generic — not assumed to be net_2.yml. Falls back
// to the first file that even mentions an entrypoint, then net_2.yml.
func gd2FindTraefikStack(stacksDirP string) string {
	candWithEP := ""
	entries, err := os.ReadDir(stacksDirP)
	if err != nil {
		return filepath.Join(stacksDirP, "net_2.yml")
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, fn := range names {
		if !strings.HasSuffix(fn, ".yml") {
			continue
		}
		fp := filepath.Join(stacksDirP, fn)
		raw, rerr := os.ReadFile(fp)
		if rerr != nil {
			continue
		}
		var data map[string]interface{}
		if yaml.Unmarshal(raw, &data) != nil {
			// not valid YAML to load fully — cheap text check for entrypoints
			if strings.Contains(string(raw), "entrypoints.") && candWithEP == "" {
				candWithEP = fp
			}
			continue
		}
		svcs := gd2AsMap(gd2Get(data, "services"))
		for sn, svRaw := range svcs {
			sv, ok := svRaw.(map[string]interface{})
			if !ok {
				continue
			}
			img := gd2Str(gd2Get(sv, "image"))
			if strings.Contains(strings.ToLower(sn), "traefik") || strings.Contains(strings.ToLower(img), "traefik") {
				cmds := gd2AsList(gd2Get(sv, "command"))
				hasEP := false
				for _, a := range cmds {
					if strings.Contains(gd2Str(a), "entrypoints.") {
						hasEP = true
						break
					}
				}
				if hasEP {
					return fp // the real traefik stack
				}
				if candWithEP == "" {
					candWithEP = fp
				}
			}
		}
	}
	if candWithEP != "" {
		return candWithEP
	}
	return filepath.Join(stacksDirP, "net_2.yml")
}

func gd2TraefikStack(stacksDirP string) string {
	if v := os.Getenv("TRAEFIK_STACK"); v != "" {
		return v
	}
	return gd2FindTraefikStack(stacksDirP)
}

// ── defaults (used if dynamics.yaml is missing keys) ──────────────────────────
// Faithful translation of the Python DEF dict. Built programmatically since Go
// has no literal nested-map sugar as compact as Python's.
func gd2DEF() map[string]interface{} {
	return map[string]interface{}{
		"domain":         map[string]interface{}{"primary": "example.com", "secondary": "", "use": "primary"},
		"host_overrides": map[string]interface{}{},
		"generate":       map[string]interface{}{"routers": true, "services": true, "middlewares": true, "transports": true, "tcp": true},
		"chain": []interface{}{"redirect-www", "sablier", "cloudflare-realip", "cloudflare-ipallow", "ip-allow-internal",
			"ip-allow-cloudflare", "ddns-allow", "geoblock", "fail2ban", "fail2ban-strict", "https-header",
			"crowdsec_bouncer", "authentik-auth", "rate-limit-auth", "global-retry", "compress", "inflight",
			"buffering", "rate-limit", "circuit-breaker", "cache"},
		"features": map[string]interface{}{"authentik": true, "crowdsec": true, "sablier": true, "https_header": true,
			"https_redirect": true, "global_retry": true, "compress": true, "inflight": true,
			"buffering": true, "rate_limit": true, "error_pages": true, "cloudflare_ipallow": false,
			"cloudflare_realip": false, "geoblock": false, "souin_cache": false,
			"circuit_breaker": false, "rate_limit_auth": false, "ip_allow_internal": false,
			"ip_allow_cloudflare": false, "redirect_www": false, "fail2ban": false,
			"fail2ban_strict": false, "ddns_allowlist": false,
			"tls_options": true, "named_chains": true,
			"cors": false, "content_type": false},
		"urls": map[string]interface{}{"authentik": "http://authentik_server:9000", "crowdsec": "http://crowdsec_bouncer:8080",
			"sablier": "http://sablier:10000", "error_pages": "http://error-pages:8080"},
		"headers": map[string]interface{}{"x_frame_options": "SAMEORIGIN", "content_type_nosniff": true, "xss_protection": "1; mode=block",
			"referrer_policy": "strict-origin-when-cross-origin", "hsts": "max-age=31536000; includeSubDomains; preload",
			"hide_server": true, "robots": "noindex, nofollow", "permissions_policy_enabled": true,
			"permissions_policy": "camera=(), microphone=(), geolocation=(), payment=()",
			"csp_enabled":        true, "csp": "default-src 'self' 'unsafe-inline' 'unsafe-eval' data: blob: wss:; frame-ancestors 'self'"},
		"authentik_response_headers": []interface{}{"X-authentik-username", "X-authentik-groups", "X-authentik-email",
			"X-authentik-name", "X-authentik-uid", "X-authentik-jwt", "X-authentik-meta-jwks",
			"X-authentik-meta-outpost", "X-authentik-meta-provider", "X-authentik-meta-app", "X-authentik-meta-version"},
		"tunables": map[string]interface{}{
			"global_retry": map[string]interface{}{"attempts": 3, "initial_interval": "100ms"},
			"compress":     map[string]interface{}{"min_bytes": 1024, "encodings": []interface{}{"zstd", "br", "gzip"}, "excluded_content_types": []interface{}{"text/event-stream"}},
			"inflight":     map[string]interface{}{"amount": 100, "ip_depth": 1},
			"buffering":    map[string]interface{}{"max_request": 10485760, "mem_request": 2097152, "max_response": 10485760, "mem_response": 2097152, "retry_expression": "IsNetworkError() && Attempts() < 3"},
			"rate_limit":   map[string]interface{}{"average": 100, "burst": 200, "period": "1s", "ip_depth": 1}},
		"plugins": map[string]interface{}{
			"geoblock": map[string]interface{}{"allow_local": true, "allow_unknown": false, "blacklist_mode": false,
				"countries": []interface{}{"US"}, "api": "https://get.geojs.io/v1/ip/country/{ip}",
				"api_timeout_ms": 750, "cache_size": 25, "denied_status": 403,
				"add_country_header": true, "log_level": "info"},
			"souin":             map[string]interface{}{"ttl": "120s", "stale": "120s", "log_level": "info"},
			"cloudflare_realip": map[string]interface{}{"disable_default": false},
			"fail2ban":          map[string]interface{}{"bantime": "10m", "findtime": "5m", "maxretry": 6, "statuscode": "401,403,429"},
			"fail2ban_strict":   map[string]interface{}{"bantime": "1h", "findtime": "10m", "maxretry": 3, "statuscode": "401,403"},
			"ddns":              map[string]interface{}{"hostname": "home.example.com", "refresh": "5m"}},
		"security": map[string]interface{}{
			"circuit_breaker": map[string]interface{}{"expression": "NetworkErrorRatio() > 0.30 || ResponseCodeRatio(500, 600, 0, 600) > 0.25 || LatencyAtQuantileMS(99.0) > 5000",
				"check": "1s", "fallback": "10s", "recovery": "10s", "status": 503},
			"rate_limit_auth": map[string]interface{}{"average": 5, "burst": 10, "period": "1m", "ip_depth": 1},
			"redirect_www":    map[string]interface{}{}},
		"tls_opts": map[string]interface{}{"min_version": "VersionTLS12", "sni_strict": true,
			"cipher_suites": []interface{}{"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305", "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305"},
			"curves": []interface{}{"X25519", "CurveP384", "CurveP256"}},
		"cors": map[string]interface{}{"allow_origins": []interface{}{"https://example.com"}, "allow_methods": []interface{}{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			"allow_headers": []interface{}{"Content-Type", "Authorization", "X-Requested-With"}, "max_age": 86400, "allow_credentials": false},
		"transports": map[string]interface{}{"insecure_skip_verify": true, "h2c": true, "custom_timeout": true},
		"sablier":    map[string]interface{}{"default_theme": "ghost", "default_duration": "1h", "default_timeout": "10m", "overrides": map[string]interface{}{}},
		"tcp": map[string]interface{}{"enabled": true, "generate_entrypoints": true, "merge_into_traefik": true,
			"port_types": map[int]interface{}{5432: "postgres", 3306: "mysql", 6379: "redis", 27017: "mongodb", 7687: "neo4j",
				5672: "amqp", 5984: "couchdb", 8086: "influxdb", 9000: "clickhouse", 9200: "opensearch"},
			"image_types": map[string]interface{}{"pgvector": []interface{}{5432, "postgres"}, "timescale": []interface{}{5432, "postgres"},
				"supabase/postgres": []interface{}{5432, "postgres"}, "postgres": []interface{}{5432, "postgres"},
				"mariadb": []interface{}{3306, "mysql"}, "percona": []interface{}{3306, "mysql"}, "mysql": []interface{}{3306, "mysql"},
				"mongo": []interface{}{27017, "mongodb"}, "rabbitmq": []interface{}{5672, "amqp"},
				"valkey": []interface{}{6379, "redis"}, "keydb": []interface{}{6379, "redis"}, "dragonfly": []interface{}{6379, "redis"}, "redis": []interface{}{6379, "redis"},
				"neo4j": []interface{}{7687, "neo4j"}, "couchdb": []interface{}{5984, "couchdb"}, "influxdb": []interface{}{8086, "influxdb"},
				"clickhouse": []interface{}{9000, "clickhouse"}, "opensearch": []interface{}{9200, "opensearch"}, "elasticsearch": []interface{}{9200, "opensearch"}},
			"image_exclude": []interface{}{"postgrest", "postgres-meta", "-exporter", "_exporter",
				"zabbix-server", "zabbix-web", "zabbix-proxy", "zabbix-agent",
				"adminer", "pgadmin", "phpmyadmin", "mongo-express", "redis-commander",
				"redisinsight", "cloudbeaver", "pgbackweb", "supabase/studio"},
			"base_ports": map[string]interface{}{"postgres": 5432, "mysql": 3306, "redis": 6379, "mongodb": 27017, "neo4j": 7687,
				"amqp": 5672, "couchdb": 5984, "influxdb": 8086, "clickhouse": 9000, "opensearch": 9200},
			"fallback_suffix": "general"},
		"exclude_suffixes": []interface{}{"-ext"},
	}
}

var gd2CFIPs = []string{"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
	"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
	"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32"}

var gd2LANIPs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.1/32"}

var gd2TypeTokens = map[string]bool{"db": true, "redis": true, "mongo": true, "mongodb": true, "postgres": true, "postgresql": true, "mysql": true, "mariadb": true, "cache": true,
	"rabbitmq": true, "opensearch": true, "bolt": true, "grpc": true, "tcp": true}

// ── generic nested-map/value helpers (yaml decodes to map[string]interface{}) ──

// gd2Get fetches a key from a generic map (nil if absent or m not a map).
func gd2Get(m interface{}, key string) interface{} {
	if mm, ok := m.(map[string]interface{}); ok {
		return mm[key]
	}
	return nil
}

// gd2AsMap returns v as a map[string]interface{} (empty if not).
func gd2AsMap(v interface{}) map[string]interface{} {
	if mm, ok := v.(map[string]interface{}); ok {
		return mm
	}
	return map[string]interface{}{}
}

// gd2AsList returns v as a []interface{} (nil if not a list).
func gd2AsList(v interface{}) []interface{} {
	if l, ok := v.([]interface{}); ok {
		return l
	}
	return nil
}

// gd2Str mirrors Python str() for scalars used here.
func gd2Str(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// integers stored as float — render without trailing .0 where whole
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// gd2Bool returns a python-truthiness-ish bool for a config value, with default.
func gd2Bool(v interface{}, def bool) bool {
	switch t := v.(type) {
	case nil:
		return def
	case bool:
		return t
	case string:
		return t != ""
	default:
		return true
	}
}

// gd2Int returns an int from a config value, with default.
func gd2Int(v interface{}, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
	}
	return def
}

// gd2GetOr returns m[key] if present else def (mirrors dict.get(k, def)).
func gd2GetOr(m interface{}, key string, def interface{}) interface{} {
	if mm, ok := m.(map[string]interface{}); ok {
		if v, ok := mm[key]; ok {
			return v
		}
	}
	return def
}

// gd2lower mirrors Python's .lower() on a bool-style string for True/False output.
func gd2lower(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// gd2BoolLower renders a config value as lowercase "true"/"false" (mirrors
// str(g.get(...)).lower() where the value is a Python bool).
func gd2BoolLower(v interface{}, def bool) string {
	return gd2lower(gd2Bool(v, def))
}

// gd2Merge mirrors _merge(): deep-merge over into a copy of base.
func gd2Merge(base, over map[string]interface{}) map[string]interface{} {
	out := gd2DeepCopy(base)
	for k, v := range over {
		if vm, ok := v.(map[string]interface{}); ok {
			if om, ok := out[k].(map[string]interface{}); ok {
				out[k] = gd2Merge(om, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// gd2DeepCopy mirrors copy.deepcopy for our nested map/list/scalar structures.
func gd2DeepCopy(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = gd2DeepCopyVal(v)
	}
	return out
}

func gd2DeepCopyVal(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return gd2DeepCopy(t)
	case []interface{}:
		nl := make([]interface{}, len(t))
		for i, x := range t {
			nl[i] = gd2DeepCopyVal(x)
		}
		return nl
	default:
		return v
	}
}

// gd2LoadCfg mirrors load_cfg().
func gd2LoadCfg() map[string]interface{} {
	cfg := gd2DeepCopy(gd2DEF())
	conf := filepath.Join(configDir(), "dynamics.yaml")
	if _, err := os.Stat(conf); err == nil {
		raw, rerr := os.ReadFile(conf)
		if rerr == nil {
			var over map[string]interface{}
			if uerr := yaml.Unmarshal(raw, &over); uerr == nil {
				if over != nil {
					cfg = gd2Merge(cfg, over)
				}
			} else {
				fmt.Printf("  WARN: bad dynamics.yaml (%v); using defaults\n", uerr)
			}
		}
	}
	return cfg
}

// ── compose scanning helpers ──────────────────────────────────────────────────

// gd2Labels mirrors _labels(): normalize labels to a []string of "k=v".
func gd2Labels(svc map[string]interface{}) []string {
	l := gd2Get(svc, "labels")
	if lst, ok := l.([]interface{}); ok {
		out := make([]string, 0, len(lst))
		for _, x := range lst {
			out = append(out, gd2Str(x))
		}
		return out
	}
	if mm, ok := l.(map[string]interface{}); ok {
		out := make([]string, 0, len(mm))
		for k, v := range mm {
			out = append(out, fmt.Sprintf("%s=%s", k, gd2Str(v)))
		}
		return out
	}
	return []string{}
}

// gd2HasTraefik mirrors has_traefik().
func gd2HasTraefik(svc map[string]interface{}) bool {
	for _, l := range gd2Labels(svc) {
		if strings.Contains(strings.ToLower(l), "traefik.enable=true") {
			return true
		}
	}
	return false
}

// gd2InfraSigns mirrors INFRA_SIGNS.
var gd2InfraSigns = map[string][]string{
	"authentik":   {"authentik", "goauthentik"},
	"crowdsec":    {"crowdsec"},
	"sablier":     {"sablier"},
	"error_pages": {"error-pages", "error_pages", "tarampampam/error-pages"},
}

// gd2DetectInfra mirrors detect_infra().
func gd2DetectInfra(stackDir string, files []string) map[string]bool {
	present := map[string]bool{}
	for _, fn := range files {
		sp := filepath.Join(stackDir, fn)
		raw, err := os.ReadFile(sp)
		if err != nil {
			continue
		}
		var data map[string]interface{}
		if yaml.Unmarshal(raw, &data) != nil {
			continue
		}
		svcs := gd2AsMap(gd2Get(data, "services"))
		for sn, svRaw := range svcs {
			sv, ok := svRaw.(map[string]interface{})
			if !ok {
				continue
			}
			blob := strings.ToLower(sn + " " + gd2Str(gd2Get(sv, "image")) + " " + gd2Str(gd2Get(sv, "container_name")))
			for infra, signs := range gd2InfraSigns {
				for _, s := range signs {
					if strings.Contains(blob, s) {
						present[infra] = true
						break
					}
				}
			}
		}
	}
	return present
}

// gd2ResolveAuto mirrors resolve_auto(): turn 'auto' feature flags into bool.
func gd2ResolveAuto(cfg map[string]interface{}, present map[string]bool) map[string]interface{} {
	f := gd2AsMap(cfg["features"])
	pairs := [][2]string{{"authentik", "authentik"}, {"crowdsec", "crowdsec"}, {"sablier", "sablier"}, {"error_pages", "error_pages"}}
	for _, kp := range pairs {
		k, infra := kp[0], kp[1]
		if s, ok := f[k].(string); ok && strings.ToLower(s) == "auto" {
			f[k] = present[infra]
		}
	}
	return cfg
}

// gd2SablierOn mirrors sablier_on().
func gd2SablierOn(svc map[string]interface{}) bool {
	for _, l := range gd2Labels(svc) {
		if strings.Contains(strings.ToLower(l), "sablier.enable=false") {
			return false
		}
	}
	return true
}

// gd2WebPortRank mirrors WEB_PORT_RANK (preference order, dupes preserved).
var gd2WebPortRank = []int{80, 8080, 3000, 8000, 5000, 8888, 9000, 8443, 443, 4000, 5173,
	3001, 8081, 9090, 2368, 8443, 1880, 8123, 8096, 5601, 8006}

// gd2NonWebPorts mirrors _NON_WEB_PORTS.
var gd2NonWebPorts = map[int]bool{5432: true, 3306: true, 6379: true, 27017: true, 7687: true, 5672: true, 5984: true, 8086: true, 9200: true, 9300: true, 11211: true, 1433: true,
	9042: true, 2379: true, 2380: true, 7000: true, 7001: true, 9001: true, 50000: true, 5044: true, 4222: true, 6222: true, 8222: true}

// gd2ImgPortHints mirrors IMG_PORT_HINTS.
var gd2ImgPortHints = map[string]int{"nginx": 80, "httpd": 80, "apache": 80, "caddy": 80, "traefik": 8080,
	"grafana": 3000, "gitea": 3000, "forgejo": 3000, "ghost": 2368, "node": 3000, "nextjs": 3000,
	"vaultwarden": 80, "bitwarden": 80, "portainer": 9000, "minio": 9001, "jellyfin": 8096,
	"sonarr": 8989, "radarr": 7878, "prowlarr": 9696, "jellyseerr": 5055, "overseerr": 5055,
	"uptime-kuma": 3001, "vikunja": 3456, "nodered": 1880, "home-assistant": 8123, "homeassistant": 8123,
	"code-server": 8080, "vscode": 8080, "wordpress": 80, "ghost-blog": 2368, "kibana": 5601,
	"prometheus": 9090, "pihole": 80, "adguard": 3000, "syncthing": 8384, "filebrowser": 80,
	"dozzle": 8080, "glances": 61208, "netdata": 19999, "speedtest": 80, "searxng": 8080}

// gd2CPorts caches the result of _container_ports().
var gd2CPorts map[string][]int

// gd2ContainerPorts mirrors _container_ports(): batch-inspect ALL containers once
// → {container_name: [exposed ports]}. Empty on any docker error (graceful).
func gd2ContainerPorts() map[string][]int {
	if gd2CPorts != nil {
		return gd2CPorts
	}
	gd2CPorts = map[string][]int{}
	// Use the docker API/CLI layer (containerInspect handles both).
	for _, c := range containers(true) {
		nm := strings.TrimPrefix("/"+nameOf(c), "/")
		ins := containerInspect(nm)
		if len(ins) == 0 {
			continue
		}
		ps := map[int]bool{}
		// Config.ExposedPorts
		cfg := gd2AsMap(ins["Config"])
		for p := range gd2AsMap(cfg["ExposedPorts"]) {
			if n, err := strconv.Atoi(strings.SplitN(p, "/", 2)[0]); err == nil {
				ps[n] = true
			}
		}
		// NetworkSettings.Ports
		netset := gd2AsMap(ins["NetworkSettings"])
		for p := range gd2AsMap(netset["Ports"]) {
			if n, err := strconv.Atoi(strings.SplitN(p, "/", 2)[0]); err == nil {
				ps[n] = true
			}
		}
		if nm != "" {
			sorted := make([]int, 0, len(ps))
			for p := range ps {
				sorted = append(sorted, p)
			}
			sort.Ints(sorted)
			gd2CPorts[nm] = sorted
		}
	}
	return gd2CPorts
}

// gd2PickWeb mirrors _pick_web().
func gd2PickWeb(ports []int) (int, bool) {
	var cand []int
	for _, p := range ports {
		if !gd2NonWebPorts[p] {
			cand = append(cand, p)
		}
	}
	if len(cand) == 0 {
		return 0, false
	}
	for _, w := range gd2WebPortRank {
		for _, c := range cand {
			if c == w {
				return w, true
			}
		}
	}
	min := cand[0]
	for _, c := range cand {
		if c < min {
			min = c
		}
	}
	return min, true
}

var gd2ReLBPort = regexp.MustCompile(`loadbalancer\.server\.port=(\d+)`)
var gd2ReHostRule = regexp.MustCompile("rule=Host\\(`([^.]+)\\.")

// gd2GetPort mirrors get_port().
func gd2GetPort(svc map[string]interface{}, name string, cfg map[string]interface{}) int {
	for _, l := range gd2Labels(svc) {
		if m := gd2ReLBPort.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	if web, ok := gd2PickWeb(gd2ExposedPorts(svc)); ok {
		return web
	}
	container := name
	if cn := gd2Str(gd2Get(svc, "container_name")); cn != "" {
		container = cn
	}
	if container != "" {
		if web, ok := gd2PickWeb(gd2ContainerPorts()[container]); ok {
			return web
		}
	}
	img := strings.ToLower(gd2Str(gd2Get(svc, "image")))
	bestLen := -1
	bestPort := 0
	for k, p := range gd2ImgPortHints {
		if strings.Contains(img, k) && len(k) > bestLen {
			bestLen = len(k)
			bestPort = p
		}
	}
	if bestLen >= 0 {
		return bestPort
	}
	return 80
}

// gd2GetHost mirrors get_host().
func gd2GetHost(svc map[string]interface{}, name string, cfg map[string]interface{}, hmap map[string]map[string]string) string {
	for _, l := range gd2Labels(svc) {
		if m := gd2ReHostRule.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	container := name
	if cn := gd2Str(gd2Get(svc, "container_name")); cn != "" {
		container = cn
	}
	ov := gd2AsMap(gd2GetOr(cfg, "host_overrides", map[string]interface{}{}))
	if v, ok := ov[name]; ok {
		return gd2Str(v)
	}
	if v, ok := ov[container]; ok {
		return gd2Str(v)
	}
	if hmap != nil {
		if v, ok := hmap["by_service"][name]; ok {
			return v
		}
		if v, ok := hmap["by_container"][container]; ok {
			return v
		}
	}
	return strings.ToLower(strings.ReplaceAll(container, "_", "-"))
}

var gd2ReHarvestRouter = regexp.MustCompile("(?m)^    ([a-z0-9_-]+)-router:\\s*\\n\\s*rule:\\s*\"Host\\(`([^.`]+)\\.")
var gd2ReHarvestSvc = regexp.MustCompile("(?ms)^    ([a-z0-9_-]+)-svc:\\s*\\n.*?url:\\s*\"https?://([a-z0-9_.-]+):")

// gd2HarvestHosts mirrors harvest_hosts().
func gd2HarvestHosts(dynDir string) map[string]map[string]string {
	bySvc := map[string]string{}
	byCont := map[string]string{}
	res := map[string]map[string]string{"by_service": bySvc, "by_container": byCont}
	st, err := os.Stat(dynDir)
	if err != nil || !st.IsDir() {
		return res
	}
	entries, err := os.ReadDir(dynDir)
	if err != nil {
		return res
	}
	for _, e := range entries {
		fn := e.Name()
		fp := filepath.Join(dynDir, fn)
		fi, ferr := os.Stat(fp)
		if !(strings.HasSuffix(fn, ".yml") && ferr == nil && !fi.IsDir()) {
			continue
		}
		raw, rerr := os.ReadFile(fp)
		if rerr != nil {
			continue
		}
		txt := string(raw)
		for _, m := range gd2ReHarvestRouter.FindAllStringSubmatch(txt, -1) {
			if _, ok := bySvc[m[1]]; !ok {
				bySvc[m[1]] = m[2]
			}
		}
		for _, m := range gd2ReHarvestSvc.FindAllStringSubmatch(txt, -1) {
			svc, cont := m[1], m[2]
			if sub, ok := bySvc[svc]; ok {
				if _, ok := byCont[cont]; !ok {
					byCont[cont] = sub
				}
			}
		}
	}
	return res
}

// gd2ExposedPorts mirrors _exposed_ports().
func gd2ExposedPorts(svc map[string]interface{}) []int {
	out := map[int]bool{}
	for _, p := range gd2AsList(gd2Get(svc, "ports")) {
		s := strings.SplitN(gd2Str(p), "/", 2)[0]
		parts := strings.Split(s, ":")
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			out[n] = true
		}
	}
	for _, e := range gd2AsList(gd2Get(svc, "expose")) {
		if n, err := strconv.Atoi(strings.SplitN(gd2Str(e), "/", 2)[0]); err == nil {
			out[n] = true
		}
	}
	res := make([]int, 0, len(out))
	for p := range out {
		res = append(res, p)
	}
	sort.Ints(res)
	return res
}

// gd2PortTypes returns the port_types map normalized to map[int]string.
func gd2PortTypes(cfg map[string]interface{}) map[int]string {
	pm := gd2AsMap(gd2Get(gd2Get(cfg, "tcp"), "port_types"))
	out := map[int]string{}
	for k, v := range pm {
		// yaml may key these as ints (int) or strings. Original DEF uses ints,
		// but a merged YAML override could give string keys.
		if n, err := strconv.Atoi(k); err == nil {
			out[n] = gd2Str(v)
		}
	}
	return out
}

// gd2PortTypesDEFOrder is the canonical insertion order of port_types as it
// appears in the Python DEF dict. db_port() relies on dict-insertion order when
// scanning port-type words (step 2) and exposed db ports (step 3); Go map
// iteration is random, so we iterate this order to stay faithful. Any keys not
// in this list (added via dynamics.yaml override) are appended in ascending
// numeric order so behaviour stays deterministic.
var gd2PortTypesDEFOrder = []int{5432, 3306, 6379, 27017, 7687, 5672, 5984, 8086, 9000, 9200}

// gd2OrderedPorts returns the ports of pm in DEF-insertion order, with any
// extra (override-added) ports appended in ascending order.
func gd2OrderedPorts(pm map[int]string) []int {
	out := make([]int, 0, len(pm))
	seen := map[int]bool{}
	for _, p := range gd2PortTypesDEFOrder {
		if _, ok := pm[p]; ok {
			out = append(out, p)
			seen[p] = true
		}
	}
	var extra []int
	for p := range pm {
		if !seen[p] {
			extra = append(extra, p)
		}
	}
	sort.Ints(extra)
	return append(out, extra...)
}

// gd2ImageTypesDEFOrder is the canonical insertion order of image_types in the
// Python DEF dict. The longest-substring-match in db_port() step 1 is mostly
// order-independent, but on an equal-length tie Python keeps the first one in
// dict order; Go map iteration is random, so we iterate this order.
var gd2ImageTypesDEFOrder = []string{"pgvector", "timescale", "supabase/postgres", "postgres",
	"mariadb", "percona", "mysql", "mongo", "rabbitmq",
	"valkey", "keydb", "dragonfly", "redis",
	"neo4j", "couchdb", "influxdb", "clickhouse", "opensearch", "elasticsearch"}

// gd2OrderedImageSubs returns the keys of imgt in DEF order, with any
// override-added keys appended in sorted order.
func gd2OrderedImageSubs(imgt map[string]interface{}) []string {
	out := make([]string, 0, len(imgt))
	seen := map[string]bool{}
	for _, k := range gd2ImageTypesDEFOrder {
		if _, ok := imgt[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	var extra []string
	for k := range imgt {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// gd2DbPort mirrors db_port().
func gd2DbPort(name string, svc map[string]interface{}, cfg map[string]interface{}) (int, bool) {
	tcp := gd2AsMap(gd2Get(cfg, "tcp"))
	pm := gd2PortTypes(cfg)
	imgt := gd2AsMap(gd2GetOr(tcp, "image_types", map[string]interface{}{}))
	img := strings.ToLower(gd2Str(gd2Get(svc, "image")))
	nm := strings.ToLower(name)
	// 0) veto known non-DB images
	for _, x := range gd2AsList(gd2GetOr(tcp, "image_exclude", []interface{}{})) {
		if strings.Contains(img, gd2Str(x)) {
			return 0, false
		}
	}
	// 1) image alias — longest substring match wins (iterate DEF-insertion order
	//    so an equal-length tie resolves the same as Python's dict order)
	bestSub := ""
	bestPort := 0
	found := false
	for _, sub := range gd2OrderedImageSubs(imgt) {
		pt := imgt[sub]
		if strings.Contains(img, sub) && (!found || len(sub) > len(bestSub)) {
			lst := gd2AsList(pt)
			if len(lst) > 0 {
				bestSub = sub
				bestPort = gd2Int(lst[0], 0)
				found = true
			}
		}
	}
	if found {
		return bestPort, true
	}
	// 2) port-type word literally in image or service name (DEF order)
	for _, p := range gd2OrderedPorts(pm) {
		t := pm[p]
		if strings.Contains(img, t) || strings.Contains(nm, t) {
			return p, true
		}
	}
	// 3) a known db port actually exposed (DEF order)
	exposed := gd2ExposedPorts(svc)
	expSet := map[int]bool{}
	for _, p := range exposed {
		expSet[p] = true
	}
	for _, p := range gd2OrderedPorts(pm) {
		if expSet[p] {
			return p, true
		}
	}
	return 0, false
}

// ── renderers ─────────────────────────────────────────────────────────────────

// gd2RenderTransports mirrors render_transports().
func gd2RenderTransports(c map[string]interface{}) string {
	t := gd2AsMap(c["transports"])
	out := "  serversTransports:\n"
	if gd2Bool(gd2GetOr(t, "insecure_skip_verify", true), true) {
		out += "    insecureTransport:\n      insecureSkipVerify: true\n"
	}
	if gd2Bool(t["h2c"], false) {
		out += "    h2cTransport:\n      insecureSkipVerify: true\n"
	}
	if gd2Bool(t["custom_timeout"], false) {
		out += "    custom-timeout:\n      maxIdleConnsPerHost: 10\n" +
			"      forwardingTimeouts:\n        readIdleTimeout: \"0s\"\n        pingTimeout: \"15s\"\n"
	}
	return out
}

// gd2RenderSharedMW mirrors render_shared_mw().
func gd2RenderSharedMW(c map[string]interface{}) string {
	f := gd2AsMap(c["features"])
	h := gd2AsMap(c["headers"])
	t := gd2AsMap(c["tunables"])
	u := gd2AsMap(c["urls"])
	o := "  middlewares:\n"
	if gd2Bool(f["https_header"], false) {
		o += "\n    https-header:\n      headers:\n        customRequestHeaders:\n          X-Forwarded-Proto: \"https\"\n        customResponseHeaders:\n"
		o += fmt.Sprintf("          X-Frame-Options: \"%s\"\n", gd2Str(h["x_frame_options"]))
		if gd2Bool(h["content_type_nosniff"], false) {
			o += "          X-Content-Type-Options: \"nosniff\"\n"
		}
		o += fmt.Sprintf("          X-XSS-Protection: \"%s\"\n", gd2Str(h["xss_protection"]))
		o += fmt.Sprintf("          Referrer-Policy: \"%s\"\n", gd2Str(h["referrer_policy"]))
		if gd2Bool(h["permissions_policy_enabled"], false) {
			o += fmt.Sprintf("          Permissions-Policy: \"%s\"\n", gd2Str(h["permissions_policy"]))
		}
		if gd2Bool(h["csp_enabled"], false) {
			o += fmt.Sprintf("          Content-Security-Policy: \"%s\"\n", gd2Str(h["csp"]))
		}
		o += fmt.Sprintf("          Strict-Transport-Security: \"%s\"\n", gd2Str(h["hsts"]))
		if gd2Bool(h["hide_server"], false) {
			o += "          Server: \"\"\n"
		}
		o += fmt.Sprintf("          X-Robots-Tag: \"%s\"\n", gd2Str(h["robots"]))
	}
	if gd2Bool(f["https_redirect"], false) {
		o += "\n    https-redirect:\n      redirectScheme:\n        scheme: https\n        permanent: true\n        port: \"443\"\n"
	}
	if gd2Bool(f["cloudflare_ipallow"], false) {
		ranges := append(append([]string{}, gd2CFIPs...), gd2LANIPs...)
		o += "\n    cloudflare-ipallow:\n      ipAllowList:\n        sourceRange:\n"
		for _, r := range ranges {
			o += fmt.Sprintf("          - \"%s\"\n", r)
		}
		o += "        ipStrategy:\n          depth: 1\n"
	}
	if gd2Bool(f["global_retry"], false) {
		g := gd2AsMap(t["global_retry"])
		o += fmt.Sprintf("\n    global-retry:\n      retry:\n        attempts: %s\n        initialInterval: %s\n", gd2Str(g["attempts"]), gd2Str(g["initial_interval"]))
	}
	if gd2Bool(f["compress"], false) {
		cm := gd2AsMap(t["compress"])
		o += fmt.Sprintf("\n    compress:\n      compress:\n        minResponseBodyBytes: %d\n        encodings:\n", gd2Int(cm["min_bytes"], 1024))
		for _, e := range gd2AsList(cm["encodings"]) {
			o += fmt.Sprintf("          - %s\n", gd2Str(e))
		}
		if ect := gd2AsList(cm["excluded_content_types"]); len(ect) > 0 {
			o += "        excludedContentTypes:\n"
			for _, x := range ect {
				o += fmt.Sprintf("          - %s\n", gd2Str(x))
			}
		}
	}
	if gd2Bool(f["inflight"], false) {
		i := gd2AsMap(t["inflight"])
		o += fmt.Sprintf("\n    inflight:\n      inFlightReq:\n        amount: %s\n        sourceCriterion:\n          ipStrategy:\n            depth: %s\n", gd2Str(i["amount"]), gd2Str(i["ip_depth"]))
	}
	if gd2Bool(f["buffering"], false) {
		b := gd2AsMap(t["buffering"])
		o += "\n    buffering:\n      buffering:\n" +
			fmt.Sprintf("        maxRequestBodyBytes: %s\n        memRequestBodyBytes: %s\n", gd2Str(b["max_request"]), gd2Str(b["mem_request"])) +
			fmt.Sprintf("        maxResponseBodyBytes: %s\n        memResponseBodyBytes: %s\n", gd2Str(b["max_response"]), gd2Str(b["mem_response"])) +
			fmt.Sprintf("        retryExpression: \"%s\"\n", gd2Str(b["retry_expression"]))
	}
	if gd2Bool(f["rate_limit"], false) {
		r := gd2AsMap(t["rate_limit"])
		o += fmt.Sprintf("\n    rate-limit:\n      rateLimit:\n        average: %s\n        burst: %s\n", gd2Str(r["average"]), gd2Str(r["burst"])) +
			fmt.Sprintf("        period: %s\n        sourceCriterion:\n          ipStrategy:\n            depth: %s\n", gd2Str(r["period"]), gd2Str(r["ip_depth"]))
	}
	if gd2Bool(f["authentik"], false) {
		o += fmt.Sprintf("\n    authentik-auth:\n      forwardAuth:\n        address: \"%s/outpost.goauthentik.io/auth/traefik\"\n        trustForwardHeader: true\n        authResponseHeaders:\n", gd2Str(u["authentik"]))
		for _, hh := range gd2AsList(c["authentik_response_headers"]) {
			o += fmt.Sprintf("          - %s\n", gd2Str(hh))
		}
	}
	if gd2Bool(f["crowdsec"], false) {
		o += fmt.Sprintf("\n    crowdsec_bouncer:\n      forwardAuth:\n        address: \"%s/api/v1/forwardAuth\"\n        trustForwardHeader: true\n", gd2Str(u["crowdsec"]))
	}
	if gd2Bool(f["error_pages"], false) {
		o += "\n    error-pages-middleware:\n      errors:\n        status: \"400-599\"\n        service: error-pages-svc\n        query: \"/{status}.html\"\n"
	}
	p := gd2AsMap(gd2GetOr(c, "plugins", map[string]interface{}{}))
	if gd2Bool(f["cloudflare_realip"], false) {
		cr := gd2AsMap(gd2GetOr(p, "cloudflare_realip", map[string]interface{}{}))
		o += "\n    cloudflare-realip:\n      plugin:\n        cloudflarewarp:\n" +
			fmt.Sprintf("          disableDefault: %s\n", gd2BoolLower(gd2GetOr(cr, "disable_default", false), false))
	}
	if gd2Bool(f["geoblock"], false) {
		g := gd2AsMap(gd2GetOr(p, "geoblock", map[string]interface{}{}))
		o += "\n    geoblock:\n      plugin:\n        geoblock:\n" +
			fmt.Sprintf("          allowLocalRequests: %s\n", gd2BoolLower(gd2GetOr(g, "allow_local", true), true)) +
			fmt.Sprintf("          allowUnknownCountries: %s\n", gd2BoolLower(gd2GetOr(g, "allow_unknown", false), false)) +
			fmt.Sprintf("          blackListMode: %s\n", gd2BoolLower(gd2GetOr(g, "blacklist_mode", false), false)) +
			"          logAllowedRequests: false\n          logApiRequests: false\n" +
			fmt.Sprintf("          api: \"%s\"\n", gd2Str(gd2GetOr(g, "api", "https://get.geojs.io/v1/ip/country/{ip}"))) +
			fmt.Sprintf("          apiTimeoutMs: %d\n", gd2Int(gd2GetOr(g, "api_timeout_ms", 750), 750)) +
			fmt.Sprintf("          cacheSize: %d\n", gd2Int(gd2GetOr(g, "cache_size", 25), 25)) +
			"          forceMonthlyUpdate: true\n" +
			fmt.Sprintf("          httpStatusCodeDeniedRequest: %d\n", gd2Int(gd2GetOr(g, "denied_status", 403), 403)) +
			fmt.Sprintf("          addCountryHeader: %s\n", gd2BoolLower(gd2GetOr(g, "add_country_header", true), true)) +
			fmt.Sprintf("          logLevel: %s\n          countries:\n", gd2Str(gd2GetOr(g, "log_level", "info")))
		countries := gd2AsList(gd2GetOr(g, "countries", nil))
		if len(countries) == 0 {
			countries = []interface{}{"US"}
		}
		for _, cc := range countries {
			o += fmt.Sprintf("            - %s\n", gd2Str(cc))
		}
	}
	if gd2Bool(f["souin_cache"], false) {
		s := gd2AsMap(gd2GetOr(p, "souin", map[string]interface{}{}))
		o += "\n    cache:\n      plugin:\n        souin:\n" +
			"          api:\n            prometheus: {}\n" +
			fmt.Sprintf("          default_cache:\n            ttl: %s\n            stale: %s\n", gd2Str(gd2GetOr(s, "ttl", "120s")), gd2Str(gd2GetOr(s, "stale", "120s"))) +
			fmt.Sprintf("          log_level: %s\n", gd2Str(gd2GetOr(s, "log_level", "info")))
	}
	sec := gd2AsMap(gd2GetOr(c, "security", map[string]interface{}{}))
	if gd2Bool(f["circuit_breaker"], false) {
		cb := gd2AsMap(gd2GetOr(sec, "circuit_breaker", map[string]interface{}{}))
		o += "\n    circuit-breaker:\n      circuitBreaker:\n" +
			fmt.Sprintf("        expression: \"%s\"\n", gd2Str(gd2GetOr(cb, "expression", "NetworkErrorRatio() > 0.30"))) +
			fmt.Sprintf("        checkPeriod: \"%s\"\n        fallbackDuration: \"%s\"\n", gd2Str(gd2GetOr(cb, "check", "1s")), gd2Str(gd2GetOr(cb, "fallback", "10s"))) +
			fmt.Sprintf("        recoveryDuration: \"%s\"\n        responseCode: %d\n", gd2Str(gd2GetOr(cb, "recovery", "10s")), gd2Int(gd2GetOr(cb, "status", 503), 503))
	}
	if gd2Bool(f["rate_limit_auth"], false) {
		ra := gd2AsMap(gd2GetOr(sec, "rate_limit_auth", map[string]interface{}{}))
		o += fmt.Sprintf("\n    rate-limit-auth:\n      rateLimit:\n        average: %d\n        burst: %d\n", gd2Int(gd2GetOr(ra, "average", 5), 5), gd2Int(gd2GetOr(ra, "burst", 10), 10)) +
			fmt.Sprintf("        period: %s\n        sourceCriterion:\n          ipStrategy:\n            depth: %d\n", gd2Str(gd2GetOr(ra, "period", "1m")), gd2Int(gd2GetOr(ra, "ip_depth", 1), 1))
	}
	if gd2Bool(f["redirect_www"], false) {
		dom := gd2Str(gd2GetOr(gd2AsMap(gd2GetOr(c, "domain", map[string]interface{}{})), "primary", "example.com"))
		esc := strings.ReplaceAll(dom, ".", "\\\\.")
		o += fmt.Sprintf("\n    redirect-www:\n      redirectRegex:\n        regex: \"^https?://www\\\\.%s/(.*)\"\n", esc) +
			fmt.Sprintf("        replacement: \"https://%s/${1}\"\n        permanent: true\n", dom)
	}
	if gd2Bool(f["ip_allow_internal"], false) {
		o += "\n    ip-allow-internal:\n      ipAllowList:\n        sourceRange:\n"
		ranges := append(append([]string{"127.0.0.1/32", "::1/128"}, gd2LANIPs...), "100.64.0.0/10")
		for _, r := range ranges {
			o += fmt.Sprintf("          - \"%s\"\n", r)
		}
		o += "        ipStrategy:\n          depth: 1\n"
	}
	if gd2Bool(f["ip_allow_cloudflare"], false) {
		o += "\n    ip-allow-cloudflare:\n      ipAllowList:\n        sourceRange:\n"
		for _, r := range gd2CFIPs {
			o += fmt.Sprintf("          - \"%s\"\n", r)
		}
		o += "        ipStrategy:\n          depth: 1\n"
	}
	f2bAllow := func() string {
		s := "          allowlist:\n            ip:\n"
		for _, r := range append([]string{"127.0.0.1", "::1"}, gd2LANIPs...) {
			s += fmt.Sprintf("              - \"%s\"\n", r)
		}
		return s
	}
	if gd2Bool(f["fail2ban"], false) {
		fb := gd2AsMap(gd2GetOr(p, "fail2ban", map[string]interface{}{}))
		o += "\n    fail2ban:\n      plugin:\n        fail2ban:\n          logLevel: INFO\n" + f2bAllow() +
			"          rules:\n            enabled: true\n" +
			fmt.Sprintf("            bantime: \"%s\"\n            findtime: \"%s\"\n", gd2Str(gd2GetOr(fb, "bantime", "10m")), gd2Str(gd2GetOr(fb, "findtime", "5m"))) +
			fmt.Sprintf("            maxretry: %d\n            statuscode: \"%s\"\n", gd2Int(gd2GetOr(fb, "maxretry", 6), 6), gd2Str(gd2GetOr(fb, "statuscode", "401,403,429")))
	}
	if gd2Bool(f["fail2ban_strict"], false) {
		fs := gd2AsMap(gd2GetOr(p, "fail2ban_strict", map[string]interface{}{}))
		o += "\n    fail2ban-strict:\n      plugin:\n        fail2ban:\n          logLevel: WARN\n" + f2bAllow() +
			"          rules:\n            enabled: true\n" +
			fmt.Sprintf("            bantime: \"%s\"\n            findtime: \"%s\"\n", gd2Str(gd2GetOr(fs, "bantime", "1h")), gd2Str(gd2GetOr(fs, "findtime", "10m"))) +
			fmt.Sprintf("            maxretry: %d\n            statuscode: \"%s\"\n", gd2Int(gd2GetOr(fs, "maxretry", 3), 3), gd2Str(gd2GetOr(fs, "statuscode", "401,403")))
	}
	if gd2Bool(f["ddns_allowlist"], false) {
		dd := gd2AsMap(gd2GetOr(p, "ddns", map[string]interface{}{}))
		o += "\n    ddns-allow:\n      plugin:\n        ddns-allowlist:\n" +
			fmt.Sprintf("          hostname: \"%s\"\n          refreshInterval: \"%s\"\n", gd2Str(gd2GetOr(dd, "hostname", "home.example.com")), gd2Str(gd2GetOr(dd, "refresh", "5m"))) +
			"          rejectStatusCode: 403\n          staticIPs:\n"
		for _, r := range gd2LANIPs {
			o += fmt.Sprintf("            - \"%s\"\n", r)
		}
	}
	if gd2Bool(f["cors"], false) {
		cr := gd2AsMap(gd2GetOr(c, "cors", map[string]interface{}{}))
		o += "\n    api-headers:\n      headers:\n" +
			fmt.Sprintf("        accessControlAllowCredentials: %s\n", gd2BoolLower(gd2GetOr(cr, "allow_credentials", false), false)) +
			"        accessControlAllowOriginList:\n"
		for _, x := range gd2AsList(gd2GetOr(cr, "allow_origins", []interface{}{})) {
			o += fmt.Sprintf("          - \"%s\"\n", gd2Str(x))
		}
		o += "        accessControlAllowMethods:\n"
		for _, x := range gd2AsList(gd2GetOr(cr, "allow_methods", []interface{}{})) {
			o += fmt.Sprintf("          - %s\n", gd2Str(x))
		}
		o += "        accessControlAllowHeaders:\n"
		for _, x := range gd2AsList(gd2GetOr(cr, "allow_headers", []interface{}{})) {
			o += fmt.Sprintf("          - %s\n", gd2Str(x))
		}
		o += fmt.Sprintf("        accessControlMaxAge: %d\n        addVaryHeader: true\n", gd2Int(gd2GetOr(cr, "max_age", 86400), 86400))
	}
	if gd2Bool(f["content_type"], false) {
		o += "\n    content-type:\n      contentType: {}\n"
	}
	return o
}

// gd2RenderTLS mirrors render_tls().
func gd2RenderTLS(c map[string]interface{}) string {
	t := gd2AsMap(gd2GetOr(c, "tls_opts", map[string]interface{}{}))
	o := "tls:\n  options:\n"
	o += fmt.Sprintf("    default:\n      minVersion: %s\n", gd2Str(gd2GetOr(t, "min_version", "VersionTLS12"))) +
		fmt.Sprintf("      sniStrict: %s\n      preferServerCipherSuites: true\n", gd2BoolLower(gd2GetOr(t, "sni_strict", true), true))
	o += "      cipherSuites:\n"
	for _, x := range gd2AsList(gd2GetOr(t, "cipher_suites", []interface{}{})) {
		o += fmt.Sprintf("        - %s\n", gd2Str(x))
	}
	o += "      curvePreferences:\n"
	curves := gd2AsList(gd2GetOr(t, "curves", nil))
	if curves == nil {
		curves = []interface{}{"X25519", "CurveP256"}
	}
	for _, x := range curves {
		o += fmt.Sprintf("        - %s\n", gd2Str(x))
	}
	o += "      alpnProtocols:\n        - h2\n        - http/1.1\n"
	o += "    hardened:\n      minVersion: VersionTLS13\n      sniStrict: true\n" +
		"      disableSessionTickets: true\n      alpnProtocols:\n        - h2\n        - http/1.1\n"
	return o
}

// gd2RenderChains mirrors render_chains().
func gd2RenderChains(c map[string]interface{}) string {
	f := gd2AsMap(c["features"])
	g := map[string]bool{
		"redirect-www": gd2Bool(f["redirect_www"], false), "cloudflare-realip": gd2Bool(f["cloudflare_realip"], false),
		"cloudflare-ipallow": gd2Bool(f["cloudflare_ipallow"], false), "ip-allow-internal": gd2Bool(f["ip_allow_internal"], false),
		"geoblock": gd2Bool(f["geoblock"], false), "fail2ban": gd2Bool(f["fail2ban"], false), "fail2ban-strict": gd2Bool(f["fail2ban_strict"], false),
		"https-header": gd2Bool(f["https_header"], false), "crowdsec_bouncer": gd2Bool(f["crowdsec"], false), "authentik-auth": gd2Bool(f["authentik"], false),
		"rate-limit": gd2Bool(f["rate_limit"], false), "rate-limit-auth": gd2Bool(f["rate_limit_auth"], false), "global-retry": gd2Bool(f["global_retry"], false),
		"compress": gd2Bool(f["compress"], false), "inflight": gd2Bool(f["inflight"], false), "buffering": gd2Bool(f["buffering"], false),
		"circuit-breaker": gd2Bool(f["circuit_breaker"], false), "cache": gd2Bool(f["souin_cache"], false), "api-headers": gd2Bool(f["cors"], false),
	}
	keep := func(ms []string) []string {
		var out []string
		for _, m := range ms {
			if v, ok := g[m]; !ok || v { // g.get(m, True)
				out = append(out, m)
			}
		}
		return out
	}
	// Ordered profiles (insertion order preserved like the Python dict).
	type prof struct {
		name    string
		members []string
	}
	profiles := []prof{
		{"chain-public", keep([]string{"redirect-www", "cloudflare-realip", "cloudflare-ipallow", "geoblock", "fail2ban", "https-header", "crowdsec_bouncer", "global-retry", "compress", "inflight", "buffering", "rate-limit", "circuit-breaker", "cache"})},
		{"chain-auth", keep([]string{"redirect-www", "cloudflare-realip", "cloudflare-ipallow", "geoblock", "fail2ban", "https-header", "crowdsec_bouncer", "authentik-auth", "global-retry", "compress", "inflight", "buffering", "rate-limit", "circuit-breaker", "cache"})},
		{"chain-lan", keep([]string{"ip-allow-internal", "https-header", "global-retry", "compress", "inflight", "buffering", "rate-limit"})},
		{"chain-api", keep([]string{"cloudflare-realip", "api-headers", "crowdsec_bouncer", "rate-limit", "rate-limit-auth", "circuit-breaker", "global-retry", "inflight", "buffering", "compress"})},
	}
	o := "  middlewares:\n"
	for _, pr := range profiles {
		if len(pr.members) == 0 {
			continue
		}
		o += fmt.Sprintf("    %s:\n      chain:\n        middlewares:\n", pr.name)
		for _, m := range pr.members {
			o += fmt.Sprintf("          - %s\n", m)
		}
	}
	return o
}

// gd2RenderSablier mirrors render_sablier().
func gd2RenderSablier(svcName, container string, c map[string]interface{}) string {
	sab := gd2AsMap(c["sablier"])
	ov := gd2AsMap(gd2GetOr(gd2AsMap(gd2GetOr(sab, "overrides", map[string]interface{}{})), svcName, map[string]interface{}{}))
	names := gd2Str(gd2GetOr(ov, "names", container))
	display := gd2Str(gd2GetOr(ov, "display", container))
	theme := gd2Str(gd2GetOr(ov, "theme", gd2Str(sab["default_theme"])))
	dur := gd2Str(gd2GetOr(ov, "duration", gd2Str(sab["default_duration"])))
	tmo := gd2Str(gd2GetOr(ov, "timeout", gd2Str(sab["default_timeout"])))
	urls := gd2AsMap(c["urls"])
	return fmt.Sprintf("\n    sablier-%s:\n      plugin:\n        sablier:\n", svcName) +
		fmt.Sprintf("          sablierUrl: \"%s\"\n          sessionDuration: \"%s\"\n", gd2Str(urls["sablier"]), dur) +
		fmt.Sprintf("          names: \"%s\"\n          dynamic:\n            displayName: \"%s\"\n", names, display) +
		"            provider: \"docker\"\n            stopTimeout: \"30s\"\n            refreshFrequency: \"5s\"\n" +
		fmt.Sprintf("            theme: \"%s\"\n            timeout: \"%s\"\n            warmupPeriod: \"10s\"\n", theme, tmo) +
		"            healthCheckPath: \"/\"\n            healthCheckInterval: \"2s\"\n" +
		"            scaling:\n              replicas: 1\n              minReplicas: 0\n              maxReplicas: 1\n"
}

// gd2BuildChain mirrors build_chain().
func gd2BuildChain(c map[string]interface{}, sablierName string) []string {
	f := gd2AsMap(c["features"])
	gate := map[string]bool{
		"sablier": sablierName != "", "cloudflare-ipallow": gd2Bool(f["cloudflare_ipallow"], false),
		"cloudflare-realip": gd2Bool(f["cloudflare_realip"], false), "geoblock": gd2Bool(f["geoblock"], false),
		"cache": gd2Bool(f["souin_cache"], false), "redirect-www": gd2Bool(f["redirect_www"], false),
		"ip-allow-internal": gd2Bool(f["ip_allow_internal"], false), "ip-allow-cloudflare": gd2Bool(f["ip_allow_cloudflare"], false),
		"ddns-allow": gd2Bool(f["ddns_allowlist"], false), "fail2ban": gd2Bool(f["fail2ban"], false),
		"fail2ban-strict": gd2Bool(f["fail2ban_strict"], false), "rate-limit-auth": gd2Bool(f["rate_limit_auth"], false),
		"circuit-breaker": gd2Bool(f["circuit_breaker"], false),
		"https-header":    gd2Bool(f["https_header"], false), "crowdsec_bouncer": gd2Bool(f["crowdsec"], false),
		"authentik-auth": gd2Bool(f["authentik"], false), "global-retry": gd2Bool(f["global_retry"], false),
		"compress": gd2Bool(f["compress"], false), "inflight": gd2Bool(f["inflight"], false),
		"buffering": gd2Bool(f["buffering"], false), "rate-limit": gd2Bool(f["rate_limit"], false),
	}
	var out []string
	for _, mv := range gd2AsList(c["chain"]) {
		m := gd2Str(mv)
		if m == "sablier" {
			if sablierName != "" {
				out = append(out, sablierName)
			}
		} else if v, ok := gate[m]; !ok || v { // gate.get(m, True)
			out = append(out, m)
		}
	}
	return out
}

// gd2RouterDomains is the full list of domains each router should match
// (primary first, then any domain.extra entries). Set per-generate. Primary
// stays first so the Host(`sub.primary`) harvest/get_host regexes keep working.
var gd2RouterDomains []string

// gd2RenderRouter mirrors render_router(). Emits Host(`sub.d1`) || Host(`sub.d2`) ...
// across every configured domain so the same router answers on .com, .dev, etc.
func gd2RenderRouter(name, host, domain string, chain []string) string {
	doms := gd2RouterDomains
	if len(doms) == 0 {
		doms = []string{domain}
	}
	clauses := make([]string, 0, len(doms))
	for _, d := range doms {
		clauses = append(clauses, fmt.Sprintf("Host(`%s.%s`)", host, d))
	}
	rule := strings.Join(clauses, " || ")
	return fmt.Sprintf("    %s-router:\n      rule: \"%s\"\n      service: %s-svc\n", name, rule, name) +
		fmt.Sprintf("      entryPoints: [web]\n      middlewares: [%s]\n", strings.Join(chain, ", "))
}

// gd2RenderService mirrors render_service().
func gd2RenderService(name, container string, port int) string {
	return fmt.Sprintf("    %s-svc:\n      loadBalancer:\n        servers: [{ url: \"http://%s:%d\" }]\n", name, container, port)
}

// ── TCP entrypoint derivation (generic) ───────────────────────────────────────

var gd2ReSplitTok = regexp.MustCompile(`[-_]`)

// gd2DeriveEntrypoint mirrors derive_entrypoint().
func gd2DeriveEntrypoint(rname string, port int, cfg map[string]interface{}, existing map[string]bool) (string, bool) {
	pm := gd2PortTypes(cfg)
	typ, ok := pm[port]
	if !ok {
		return "", false
	}
	raw := rname
	if strings.HasSuffix(rname, "-tcp") {
		raw = rname[:len(rname)-4]
	}
	// type tokens sorted by length desc for the glued-suffix strip.
	sortedTokens := make([]string, 0, len(gd2TypeTokens))
	for t := range gd2TypeTokens {
		sortedTokens = append(sortedTokens, t)
	}
	sort.Slice(sortedTokens, func(i, j int) bool {
		if len(sortedTokens[i]) != len(sortedTokens[j]) {
			return len(sortedTokens[i]) > len(sortedTokens[j])
		}
		return sortedTokens[i] < sortedTokens[j]
	})
	var toks []string
	for _, p := range gd2ReSplitTok.Split(raw, -1) {
		if p == "" || gd2TypeTokens[p] {
			continue
		}
		for _, tt := range sortedTokens {
			if p != tt && strings.HasSuffix(p, tt) && len(p) > len(tt)+1 {
				p = p[:len(p)-len(tt)]
				break
			}
		}
		toks = append(toks, p)
	}
	inst := strings.Join(toks, "-")
	var cands []string
	if inst != "" {
		cands = append(cands, fmt.Sprintf("%s-%s", typ, inst))
	}
	if port == 3306 && inst != "" {
		cands = append(cands, fmt.Sprintf("mysql-%s", inst), fmt.Sprintf("mariadb-%s", inst))
	}
	if port == 27017 && inst != "" {
		cands = append(cands, fmt.Sprintf("mongodb-%s", inst))
	}
	if port == 7687 {
		cands = append(cands, "neo4j-bolt")
	}
	if port == 5672 && inst != "" {
		cands = append(cands, fmt.Sprintf("amqp-%s", inst), "amqp")
	}
	if port == 5984 && inst != "" {
		cands = append(cands, fmt.Sprintf("couchdb-%s", inst), "couchdb")
	}
	fallback := gd2Str(gd2GetOr(gd2AsMap(gd2Get(cfg, "tcp")), "fallback_suffix", "general"))
	cands = append(cands, fmt.Sprintf("%s-%s", typ, fallback), typ)
	for _, cc := range cands {
		if existing[cc] {
			return cc, true
		}
	}
	// nothing exists yet → propose the primary candidate (will be created)
	return cands[0], true
}

// gd2KeysOfDoc mirrors the inner keys(doc) of _custom_keys.
func gd2KeysOfDoc(doc map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	for _, top := range []string{"http", "tcp", "udp"} {
		sec := gd2AsMap(gd2Get(doc, top))
		for _, sect := range []string{"routers", "services", "middlewares"} {
			for k := range gd2AsMap(sec[sect]) {
				out[k] = true
			}
		}
	}
	return out
}

// gd2CustomKeys mirrors _custom_keys().
func gd2CustomKeys(existingPath, generatedStr string) []string {
	raw, err := os.ReadFile(existingPath)
	if err != nil {
		return []string{} // unparseable → no-custom
	}
	var oldDoc map[string]interface{}
	if yaml.Unmarshal(raw, &oldDoc) != nil {
		return []string{}
	}
	old := gd2KeysOfDoc(oldDoc)
	var newDoc map[string]interface{}
	newKeys := map[string]bool{}
	if yaml.Unmarshal([]byte(generatedStr), &newDoc) == nil {
		newKeys = gd2KeysOfDoc(newDoc)
	}
	var extra []string
	for k := range old {
		if !newKeys[k] && !strings.HasPrefix(k, "sablier-") {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return extra
}

var gd2ReMWHeader = regexp.MustCompile(`^  middlewares:\s*$`)
var gd2ReSectionEnd = regexp.MustCompile(`^(tcp:|http:|  [A-Za-z])`)
var gd2ReMWName = regexp.MustCompile(`^    ([A-Za-z0-9_.-]+):\s*$`)

// gd2mwBlock is one ordered (name, block_text) pair.
type gd2mwBlock struct {
	name  string
	block string
}

// gd2SplitMWBlocks mirrors _split_mw_blocks(). Splits on \n keeping the newlines.
func gd2SplitMWBlocks(text string) []gd2mwBlock {
	var blocks []gd2mwBlock
	curName := ""
	var cur []string
	started := false
	for _, line := range gd2KeepLines(text) {
		if !started {
			if gd2ReMWHeader.MatchString(line) {
				started = true
			}
			continue
		}
		// end of the middlewares section
		if gd2ReSectionEnd.MatchString(line) && !strings.HasPrefix(line, "    ") {
			break
		}
		if m := gd2ReMWName.FindStringSubmatch(line); m != nil {
			if curName != "" {
				blocks = append(blocks, gd2mwBlock{curName, strings.Join(cur, "")})
			}
			curName = m[1]
			cur = []string{line}
		} else if curName != "" {
			cur = append(cur, line)
		}
	}
	if curName != "" {
		blocks = append(blocks, gd2mwBlock{curName, strings.Join(cur, "")})
	}
	return blocks
}

// gd2KeepLines mirrors str.splitlines(keepends=True) for \n-delimited text.
func gd2KeepLines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// gd2PatchMiddlewares mirrors _patch_middlewares(). Returns (newText, addedNames, ok).
func gd2PatchMiddlewares(existingText, generatedStr string) (string, []string, bool) {
	var genBlocks []gd2mwBlock
	for _, b := range gd2SplitMWBlocks(generatedStr) {
		if !strings.HasPrefix(b.name, "sablier-") {
			genBlocks = append(genBlocks, b)
		}
	}
	if len(genBlocks) == 0 {
		return "", nil, false
	}
	have := map[string]bool{}
	for _, b := range gd2SplitMWBlocks(existingText) {
		have[b.name] = true
	}
	var missing []gd2mwBlock
	for _, b := range genBlocks {
		if !have[b.name] {
			missing = append(missing, b)
		}
	}
	if len(missing) == 0 {
		return "", nil, false
	}
	lines := gd2KeepLines(existingText)
	hdr := -1
	for i, l := range lines {
		if gd2ReMWHeader.MatchString(l) {
			hdr = i
			break
		}
	}
	if hdr < 0 {
		return "", nil, false
	}
	var injb strings.Builder
	injb.WriteString("\n")
	var added []string
	for _, b := range missing {
		injb.WriteString(strings.TrimRight(b.block, "\n") + "\n")
		added = append(added, b.name)
	}
	var newLines []string
	newLines = append(newLines, lines[:hdr+1]...)
	newLines = append(newLines, injb.String())
	newLines = append(newLines, lines[hdr+1:]...)
	return strings.Join(newLines, ""), added, true
}

var gd2ReEPAddrPort = regexp.MustCompile(`entrypoints\.([a-z0-9-]+)\.address=:(\d+)`)
var gd2ReEPAddr = regexp.MustCompile(`entrypoints\.([a-z0-9-]+)\.address`)

// gendynamic2Main mirrors main(). args are the positional+flag args after the
// command word.
func gendynamic2Main(args []string) {
	stacksDirP := gd2StacksDir()
	dynDirP := gd2DynDir()
	traefikStack := gd2TraefikStack(stacksDirP)

	force := inList(args, "--force")
	overwriteCustom := inList(args, "--overwrite-custom")
	sandbox := ""
	for i, a := range args {
		if a == "--sandbox" && i+1 < len(args) {
			sandbox = args[i+1]
			break
		}
	}
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			continue
		}
		if a == sandbox && sandbox != "" {
			continue
		}
		pos = append(pos, a)
	}
	target := "all"
	if len(pos) > 0 {
		target = pos[0]
	}
	cfg := gd2LoadCfg()
	outDir := dynDirP
	if sandbox != "" {
		outDir = sandbox
	}
	os.MkdirAll(outDir, 0o755)
	dom := gd2AsMap(cfg["domain"])
	domain := gd2Str(dom["primary"])
	if gd2Str(dom["use"]) == "secondary" {
		domain = gd2Str(dom["secondary"])
	}
	// Routers match the primary domain first (keeps harvest/get_host regexes valid),
	// then every domain.extra entry (e.g. the Pangolin-tunnelled .dev/.run/.site/.online).
	gd2RouterDomains = []string{domain}
	for _, e := range gd2AsList(gd2GetOr(dom, "extra", []interface{}{})) {
		if s := gd2Str(e); s != "" && s != domain {
			gd2RouterDomains = append(gd2RouterDomains, s)
		}
	}
	var exclSuffixes []string
	for _, s := range gd2AsList(gd2GetOr(cfg, "exclude_suffixes", []interface{}{})) {
		exclSuffixes = append(exclSuffixes, gd2Str(s)+".yml")
	}
	endsExcl := func(fn string) bool {
		for _, e := range exclSuffixes {
			if strings.HasSuffix(fn, e) {
				return true
			}
		}
		return false
	}
	// harvest real subdomains from the CURRENT dynamics
	hmap := gd2HarvestHosts(dynDirP)

	var files []string
	if target == "all" {
		entries, _ := os.ReadDir(stacksDirP)
		for _, e := range entries {
			fn := e.Name()
			if strings.HasSuffix(fn, ".yml") && !endsExcl(fn) {
				files = append(files, fn)
			}
		}
		sort.Strings(files)
	} else {
		if strings.HasSuffix(target, ".yml") {
			files = []string{target}
		} else {
			files = []string{target + ".yml"}
		}
	}

	// detect installed infra across ALL stacks
	var scan []string
	{
		entries, _ := os.ReadDir(stacksDirP)
		for _, e := range entries {
			fn := e.Name()
			if strings.HasSuffix(fn, ".yml") && !endsExcl(fn) {
				scan = append(scan, fn)
			}
		}
		sort.Strings(scan)
	}
	present := gd2DetectInfra(stacksDirP, scan)
	cfg = gd2ResolveAuto(cfg, present)
	// autos: keys whose ORIGINAL (re-loaded) feature value was 'auto'
	freshFeatures := gd2AsMap(gd2LoadCfg()["features"])
	var autos []string
	for _, k := range []string{"authentik", "crowdsec", "sablier", "error_pages"} {
		if strings.ToLower(gd2Str(freshFeatures[k])) == "auto" {
			autos = append(autos, k)
		}
	}
	if len(autos) > 0 {
		var pres []string
		for p := range present {
			pres = append(pres, p)
		}
		sort.Strings(pres)
		presStr := strings.Join(pres, ", ")
		if presStr == "" {
			presStr = "none"
		}
		fmt.Printf("Infra detected: %s\n", presStr)
		feat := gd2AsMap(cfg["features"])
		for _, k := range autos {
			state := "off"
			if gd2Bool(feat[k], false) {
				state = "ON"
			}
			fmt.Printf("  auto:%s -> %s\n", k, state)
		}
	}

	// existing traefik entrypoints (for TCP matching) + their ports
	existingEPs := map[string]bool{}
	existingEPPorts := map[string]int{}
	if raw, err := os.ReadFile(traefikStack); err == nil {
		var td map[string]interface{}
		if yaml.Unmarshal(raw, &td) == nil {
			tcmd := gd2AsList(gd2Get(gd2Get(gd2AsMap(gd2Get(td, "services")), "traefik"), "command"))
			for _, av := range tcmd {
				a := gd2Str(av)
				if m := gd2ReEPAddrPort.FindStringSubmatch(a); m != nil {
					existingEPs[m[1]] = true
					p, _ := strconv.Atoi(m[2])
					existingEPPorts[m[1]] = p
				} else if m := gd2ReEPAddr.FindStringSubmatch(a); m != nil {
					existingEPs[m[1]] = true
				}
			}
		}
	}

	gen := gd2AsMap(cfg["generate"])
	tcpCfg := gd2AsMap(cfg["tcp"])
	feat := gd2AsMap(cfg["features"])

	neededEPs := map[string]int{} // name -> port
	count := 0
	for _, fn := range files {
		sp := filepath.Join(stacksDirP, fn)
		if _, err := os.Stat(sp); err != nil {
			continue
		}
		op := filepath.Join(outDir, fn)
		if _, err := os.Stat(op); err == nil && !force && sandbox == "" {
			fmt.Printf("  skip (exists): %s\n", fn)
			continue
		}
		raw, rerr := os.ReadFile(sp)
		if rerr != nil {
			continue
		}
		var data map[string]interface{}
		if uerr := yaml.Unmarshal(raw, &data); uerr != nil {
			fmt.Printf("  parse error %s: %v\n", fn, uerr)
			continue
		}
		svcs := gd2AsMap(gd2Get(data, "services"))
		R, S, M, TR, TS := "", "", "", "", ""
		// preserve compose order — yaml.v3 into map[string]interface{} loses order,
		// so re-derive ordered service names from the raw YAML node.
		for _, sn := range gd2OrderedServiceNames(raw, svcs) {
			svRaw := svcs[sn]
			sv, ok := svRaw.(map[string]interface{})
			if !ok {
				continue
			}
			container := sn
			if cn := gd2Str(gd2Get(sv, "container_name")); cn != "" {
				container = cn
			}
			dp, isDB := gd2DbPort(sn, sv, cfg)
			if isDB && gd2Bool(gen["tcp"], false) && gd2Bool(tcpCfg["enabled"], false) {
				rn := fmt.Sprintf("%s-tcp", sn)
				ep, eok := gd2DeriveEntrypoint(rn, dp, cfg, existingEPs)
				if eok {
					if _, ok := neededEPs[ep]; !ok {
						neededEPs[ep] = dp
					}
					TR += fmt.Sprintf("    %s:\n      rule: \"HostSNI(`*`)\"\n      entryPoints: [%s]\n      service: %s-svc\n", rn, ep, rn)
					TS += fmt.Sprintf("    %s-svc:\n      loadBalancer:\n        servers:\n          - address: \"%s:%d\"\n", rn, container, dp)
				}
				continue
			}
			if !gd2HasTraefik(sv) {
				continue
			}
			host := gd2GetHost(sv, sn, cfg, hmap)
			port := gd2GetPort(sv, sn, cfg)
			sab := ""
			if gd2Bool(feat["sablier"], false) && gd2SablierOn(sv) {
				sab = fmt.Sprintf("sablier-%s", sn)
			}
			if gd2Bool(gen["routers"], false) {
				R += gd2RenderRouter(sn, host, domain, gd2BuildChain(cfg, sab))
			}
			if gd2Bool(gen["services"], false) {
				S += gd2RenderService(sn, container, port)
			}
			if sab != "" {
				M += gd2RenderSablier(sn, container, cfg)
			}
		}
		if R == "" && S == "" && TR == "" {
			fmt.Printf("  skip (nothing): %s\n", fn)
			continue
		}
		out := "http:\n"
		if gd2Bool(gen["transports"], false) {
			out += gd2RenderTransports(cfg)
		}
		if R != "" {
			out += "\n  routers:\n\n" + R
		}
		if S != "" {
			out += "\n  services:\n\n" + S
		}
		if gd2Bool(gen["middlewares"], false) {
			out += "\n" + gd2RenderSharedMW(cfg)
		}
		if M != "" {
			out += M
		}
		if TR != "" {
			out += "\ntcp:\n  routers:\n" + TR + "\n  services:\n" + TS
		}
		// AUTO-PROTECT hand-tuned files (non-destructive patch).
		if _, err := os.Stat(op); err == nil && sandbox == "" && !overwriteCustom {
			existingRaw, _ := os.ReadFile(op)
			existing := string(existingRaw)
			cust := gd2CustomKeys(op, out)
			if len(cust) > 0 {
				patched, added, pok := gd2PatchMiddlewares(existing, out)
				if pok {
					os.WriteFile(op, []byte(patched), 0o644)
					fmt.Printf("  PATCHED (kept custom, added %d mw): %s  [+%s]\n", len(added), fn, strings.Join(added, ", +"))
					count++
				} else {
					custShow := cust
					ellipsis := ""
					if len(cust) > 3 {
						custShow = cust[:3]
						ellipsis = "…"
					}
					fmt.Printf("  protected (no new mw to add): %s  [custom: %s%s]\n", fn, strings.Join(custShow, ", "), ellipsis)
				}
				continue
			}
		}
		os.WriteFile(op, []byte(out), 0o644)
		fmt.Printf("  generated: %s\n", fn)
		count++
	}

	// ── global file: tls.options + named chains, defined ONCE ──
	go_ := ""
	if gd2Bool(gd2GetOr(feat, "named_chains", false), false) {
		go_ += "http:\n" + gd2RenderChains(cfg)
	}
	if gd2Bool(gd2GetOr(feat, "tls_options", false), false) {
		go_ += gd2RenderTLS(cfg)
	}
	if strings.TrimSpace(go_) != "" {
		os.WriteFile(filepath.Join(outDir, "00-global.yml"), []byte(go_), 0o644)
		fmt.Println("  generated: 00-global.yml (tls.options + named chains)")
		count++
	}

	// ── entrypoints: reuse existing, assign unique free ports to genuinely-new ──
	newEPs := map[string]int{}
	for k, v := range neededEPs {
		if !existingEPs[k] {
			newEPs[k] = v
		}
	}
	fmt.Printf("\nGenerated %d dynamic config(s) into %s\n", count, outDir)
	if gd2Bool(gd2GetOr(tcpCfg, "generate_entrypoints", false), false) {
		fmt.Printf("DB entrypoints referenced: %d (%d matched existing, %d new)\n", len(neededEPs), len(neededEPs)-len(newEPs), len(newEPs))
		if len(newEPs) > 0 {
			assigned, _ := gd2AssignPorts(newEPs, existingEPPorts, cfg)
			fmt.Println("New entrypoints to add to Traefik (append-only):")
			// sort by port asc
			type kp struct {
				k string
				p int
			}
			var arr []kp
			for k, p := range assigned {
				arr = append(arr, kp{k, p})
			}
			sort.Slice(arr, func(i, j int) bool { return arr[i].p < arr[j].p })
			for _, e := range arr {
				fmt.Printf("  + --entrypoints.%s.address=:%d\n", e.k, e.p)
			}
			var ports []int
			for _, p := range assigned {
				ports = append(ports, p)
			}
			sort.Ints(ports)
			var portStrs []string
			for _, p := range ports {
				portStrs = append(portStrs, strconv.Itoa(p))
			}
			fmt.Printf("Ports to reserve in PORT_BLACKLIST so the packer skips them: %s\n", strings.Join(portStrs, ", "))
			if inList(args, "--merge-entrypoints") && sandbox == "" {
				gd2MergeIntoNet2(assigned, traefikStack)
			} else {
				fmt.Println("(dry-run — pass --merge-entrypoints to write net_2 + blacklist)")
			}
		}
	}
}

// gd2OrderedServiceNames re-derives service insertion order from the raw compose
// YAML (yaml.v3 decoded into a map loses order; the Python relied on dict order).
func gd2OrderedServiceNames(raw []byte, svcs map[string]interface{}) []string {
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) == nil && len(root.Content) > 0 {
		doc := root.Content[0]
		if doc.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(doc.Content); i += 2 {
				if doc.Content[i].Value == "services" {
					svcNode := doc.Content[i+1]
					if svcNode.Kind == yaml.MappingNode {
						var names []string
						for j := 0; j+1 < len(svcNode.Content); j += 2 {
							n := svcNode.Content[j].Value
							if _, ok := svcs[n]; ok {
								names = append(names, n)
							}
						}
						if len(names) > 0 {
							return names
						}
					}
				}
			}
		}
	}
	// fallback: sorted keys (deterministic) if ordering can't be recovered
	names := make([]string, 0, len(svcs))
	for k := range svcs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// gd2AssignPorts mirrors assign_ports().
func gd2AssignPorts(newEPs map[string]int, existingEPPorts map[string]int, cfg map[string]interface{}) (map[string]int, map[int]bool) {
	// blacklist: from stacks.yaml port_blacklist (preferred) else conf PORT_BLACKLIST,
	// else the hardcoded fallback set.
	blacklist := map[int]bool{}
	loaded := false
	pb := configLoad()["PORT_BLACKLIST"]
	if pb != "" {
		for _, x := range strings.Split(pb, ",") {
			x = strings.TrimSpace(x)
			if n, err := strconv.Atoi(x); err == nil {
				blacklist[n] = true
				loaded = true
			}
		}
	}
	if !loaded {
		for _, p := range []int{22, 80, 443, 3306, 5432, 6379, 27017, 2375, 2376} {
			blacklist[p] = true
		}
	}
	base := gd2AsMap(gd2Get(gd2Get(cfg, "tcp"), "base_ports"))
	taken := map[int]bool{}
	for _, p := range existingEPPorts {
		taken[p] = true
	}
	for p := range blacklist {
		taken[p] = true
	}
	assigned := map[string]int{}
	assignedVals := map[int]bool{}
	// sorted names for deterministic assignment (Python: for name in sorted(new_eps))
	var names []string
	for n := range newEPs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		typ := strings.SplitN(name, "-", 2)[0]
		p := gd2Int(gd2GetOr(base, typ, newEPs[name]), newEPs[name])
		for taken[p] || assignedVals[p] {
			p++
		}
		assigned[name] = p
		assignedVals[p] = true
	}
	return assigned, blacklist
}

// gd2MergeIntoNet2 mirrors merge_into_net2().
func gd2MergeIntoNet2(assigned map[string]int, traefikStack string) {
	bak := fmt.Sprintf("%s.bak-%d", traefikStack, gd2NowUnix())
	if src, err := os.ReadFile(traefikStack); err == nil {
		os.WriteFile(bak, src, 0o644)
	}
	raw, err := os.ReadFile(traefikStack)
	if err != nil {
		fmt.Println("  ! could not locate entrypoint block in net_2 — no changes made")
		return
	}
	lines := gd2KeepLines(string(raw))
	have := map[string]bool{}
	for _, l := range lines {
		if m := gd2ReEPAddr.FindStringSubmatch(l); m != nil {
			have[m[1]] = true
		}
	}
	lastIdx := -1
	indent := ""
	reEPLine := regexp.MustCompile(`^(\s*)-\s*"--entrypoints\.[a-z0-9-]+\.address`)
	for i, l := range lines {
		if m := reEPLine.FindStringSubmatch(l); m != nil {
			lastIdx = i
			indent = m[1]
		}
	}
	if lastIdx < 0 {
		fmt.Println("  ! could not locate entrypoint block in net_2 — no changes made")
		return
	}
	// sort by port asc
	type kp struct {
		k string
		p int
	}
	var arr []kp
	for k, p := range assigned {
		arr = append(arr, kp{k, p})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].p < arr[j].p })
	var add []string
	for _, e := range arr {
		if !have[e.k] {
			add = append(add, fmt.Sprintf("%s- \"--entrypoints.%s.address=:%d\"\n", indent, e.k, e.p))
		}
	}
	if len(add) == 0 {
		fmt.Println("  all entrypoints already present — nothing to merge")
		return
	}
	newLines := insertAt(lines, lastIdx+1, add...)
	os.WriteFile(traefikStack, []byte(strings.Join(newLines, "")), 0o644)
	var ports []int
	for _, p := range assigned {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	ok, where := gd2ReservePorts(ports)
	note := fmt.Sprintf("reserved their ports in %s", where)
	if !ok {
		note = fmt.Sprintf("WARN: could not reserve ports (%s)", where)
	}
	fmt.Printf("  merged %d entrypoint(s) into net_2 (backup: %s); %s\n", len(add), bak, note)
}

// gd2ReservePorts mirrors reserve_ports(). Reserves ports in stacks.yaml
// port_blacklist (preferred) or falls back to a failure if no YAML present.
func gd2ReservePorts(ports []int) (bool, string) {
	portStrs := make([]string, 0, len(ports))
	for _, p := range ports {
		portStrs = append(portStrs, strconv.Itoa(p))
	}
	if _, err := os.Stat(yamlPath()); err == nil {
		cur := yamlGetList("port_blacklist")
		var add []string
		for _, p := range portStrs {
			if !inList(cur, p) {
				add = append(add, p)
			}
		}
		if len(add) > 0 {
			yamlSetList("port_blacklist", append(cur, add...))
		}
		return true, "stacks.yaml"
	}
	// Legacy stacks.conf PORT_BLACKLIST path: the Python deferred to
	// stacks_collision.add_port_blacklist; no Go equivalent exists, so report
	// the missing-yaml condition as a soft failure (mirrors the except path).
	return false, "stacks.yaml not present"
}

// gd2NowUnix returns the current unix time (seconds) for the .bak suffix.
func gd2NowUnix() int64 {
	return time.Now().Unix()
}
