package main

// config_load.go — faithful Go port of stacks_config.py's loader.
// Single source of truth = <configDir>/stacks.yaml (clean, human-friendly),
// falling back to the legacy stacks.conf if the YAML is missing/unreadable.
// Mirrors: SCALAR_MAP, LIST_MAP, _scalar, _from_conf, load, load_named,
// load_doc, yaml_set_scalar, yaml_get_list, yaml_set_list, and the --env/--check CLI.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	"auto_image_search":           "FIX_AUTO_SEARCH",
	"reclaim_protect_stack_images": "RECLAIM_PROTECT_STACK_IMAGES",
	"deep_inspect": "FIX_DEEP_INSPECT", "backup_before_changes": "FIX_BACKUP",
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
	"ip_whitelist": {"IP_WHITELIST", ","}, "port_whitelist": {"PORT_WHITELIST", ","},
	"proxy_skip": {"PROXY_SKIP_CONTAINERS", " "},
	"scale_skip": {"SCALE_SKIP_CONTAINERS", " "},
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
