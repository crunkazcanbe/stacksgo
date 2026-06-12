package main

// menu_network.go — Network tab: IP & port collision detection + the editable
// IP/port range and black/whitelist config (saved into stacks.yaml via the yaml
// helpers). Mirrors draw_network_tab + do_network_action.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// tuiNetData caches a collision scan (rescanned every 30s like the Python tab).
type tuiNetData struct {
	ipCol   []collisionIPRec
	portCol []collisionPortRec
	ipMap   map[string][]collisionOwner
	portMap map[string][]collisionOwner
	nextIP  string
	conf    map[string]string
}

func (m *menuModel) ensureNetData() *tuiNetData {
	if m.netCache == nil || time.Since(m.netCacheTS) > 30*time.Second {
		ip, port := getCollisions()
		m.netCache = &tuiNetData{
			ipCol:   ip,
			portCol: port,
			ipMap:   scanAllIPs(),
			portMap: scanAllPorts(),
			nextIP:  getNextAvailableIP(),
			conf:    collisionLoadConf(),
		}
		m.netCacheTS = time.Now()
	}
	return m.netCache
}

func (m menuModel) renderNetwork() string {
	// Read the scan done in the background collect — NEVER scan in the render
	// path (that re-ran the full 172-network scan every frame and froze the UI).
	nd := m.data.Net
	if nd == nil {
		return "\n  scanning networks… (first load)\n"
	}
	var b strings.Builder
	p := func(style interface{ Render(...string) string }, s string) {
		b.WriteString(style.Render(truncate("  "+s, m.width-1)))
		b.WriteString("\n")
	}
	p(tuiAccentStyle, "NETWORK — IP & PORT COLLISION DETECTION")
	b.WriteString(tuiDimStyle.Render("  " + strings.Repeat("─", maxInt(0, m.width-4))))
	b.WriteString("\n")

	var act, lat, bl []collisionPortRec
	for _, c := range nd.portCol {
		switch {
		case c.Active:
			act = append(act, c)
		case c.Type == "blacklisted":
			bl = append(bl, c)
		case c.Type == "duplicate":
			lat = append(lat, c)
		}
	}

	sumStyle := tuiYellowStyle
	if len(act) > 0 {
		sumStyle = tuiRedStyle
	}
	p(sumStyle, fmt.Sprintf("IPs in use: %d    Ports in use: %d    IP issues: %d    Ports: %d LIVE / %d latent / %d on-blacklist",
		len(nd.ipMap), len(nd.portMap), len(nd.ipCol), len(act), len(lat), len(bl)))
	nextStyle := tuiGreenStyle
	if nd.nextIP == "" {
		nextStyle = tuiRedStyle
	}
	p(nextStyle, "Next free IP: "+orNone(nd.nextIP, "NONE"))

	b.WriteString("\n")
	p(tuiAccentStyle, "── CONFIG ──────────────────────")
	p(tuiCyanStyle, fmt.Sprintf("IP range     : %s  →  %s", nd.conf["IP_RANGE_START"], nd.conf["IP_RANGE_END"]))
	p(tuiCyanStyle, fmt.Sprintf("Port range   : %s  →  %s", nd.conf["PORT_RANGE_START"], nd.conf["PORT_RANGE_END"]))
	p(tuiDimStyle, fmt.Sprintf("IP blacklist : %s", orNone(nd.conf["IP_BLACKLIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("IP whitelist : %s", orNone(nd.conf["IP_WHITELIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("Port blacklist: %s", orNone(nd.conf["PORT_BLACKLIST"], "(none)")))
	p(tuiDimStyle, fmt.Sprintf("Port whitelist: %s", orNone(nd.conf["PORT_WHITELIST"], "(none)")))

	b.WriteString("\n")
	if len(nd.ipCol) > 0 {
		p(tuiRedStyle, "⚠ IP ISSUES:")
		for i, c := range nd.ipCol {
			if i >= 4 {
				break
			}
			p(tuiRedStyle, fmt.Sprintf("  %-12s %-18s %s", c.Type, c.IP, ownersStr(c.Owners, 3)))
		}
	}
	if len(act) > 0 {
		p(tuiRedStyle, "⚠ LIVE PORT COLLISIONS:")
		for i, c := range act {
			if i >= 6 {
				break
			}
			p(tuiRedStyle, fmt.Sprintf("  %s:%-6s %s", c.IP, c.Port, ownersStr(c.Owners, 3)))
		}
	} else {
		p(tuiGreenStyle, "✔ No LIVE port collisions")
	}
	if len(lat) > 0 {
		p(tuiYellowStyle, fmt.Sprintf("• %d latent (declared, not all running):", len(lat)))
		for i, c := range lat {
			if i >= 5 {
				break
			}
			p(tuiDimStyle, fmt.Sprintf("  %s:%-6s %s", c.IP, c.Port, ownersStr(c.Owners, 3)))
		}
	}
	return b.String()
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

func ownersStr(owners []collisionOwner, n int) string {
	var parts []string
	for i, o := range owners {
		if i >= n {
			break
		}
		parts = append(parts, o.Stack+"/"+o.Container)
	}
	return strings.Join(parts, ", ")
}

func (m menuModel) handleNetworkKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "a", "A":
		m.popup = tuiActionPopup("Network — Edit", tuiNetEditActions,
			func(action string) (menuModel, tea.Cmd) { return m.doNetworkAction(action) })
	case "s", "S", "enter":
		m.netCache = nil
		return m, tuiDockerCmd("Scan collisions", func() string {
			ip, port := getCollisions()
			var live int
			for _, c := range port {
				if c.Active {
					live++
				}
			}
			lines := []string{
				fmt.Sprintf("IP issues:        %d", len(ip)),
				fmt.Sprintf("Port collisions:  %d (%d live)", len(port), live),
				fmt.Sprintf("IPs in use:       %d", len(scanAllIPs())),
				fmt.Sprintf("Ports in use:     %d", len(scanAllPorts())),
				fmt.Sprintf("Next free IP:     %s", orNone(getNextAvailableIP(), "NONE")),
			}
			return strings.Join(lines, "\n")
		})
	case "r", "R":
		m.netCache = nil
	case "e", "E":
		// open stacks.yaml in $EDITOR (mirrors the Python Network 'e' key)
		return m, tuiEditFile(filepath.Join(configDir(), "stacks.yaml"))
	case "d", "D":
		// Dedupe: report duplicate container_names across stacks + the
		// recommended keeper (mirrors the Python Network 'd' key). Resolving a
		// dup is done from the container menu's Remove/purge actions (FIX_DEDUPE
		// also auto-resolves on `fix`).
		return m, tuiSelfCmd("Dedupe — duplicate container_names", "dedupe")
	}
	return m, nil
}

var tuiNetEditActions = []tuiAction{
	{"✎  IP range — start", "ip_start"},
	{"✎  IP range — end", "ip_end"},
	{"✎  Port range — start", "port_start"},
	{"✎  Port range — end", "port_end"},
	{"➕ Add IP to blacklist", "ip_bl_add"},
	{"➖ Remove IP from blacklist", "ip_bl_rm"},
	{"➕ Add IP to whitelist", "ip_wl_add"},
	{"➖ Remove IP from whitelist", "ip_wl_rm"},
	{"➕ Add port to blacklist", "port_bl_add"},
	{"➖ Remove port from blacklist", "port_bl_rm"},
	{"➕ Add port to whitelist", "port_wl_add"},
	{"➖ Remove port from whitelist", "port_wl_rm"},
	{"↻  Rescan now", "rescan"},
	{"✕  Cancel", ""},
}

// netScalarMap: action -> friendly yaml key.
var tuiNetScalars = map[string][2]string{
	"ip_start":   {"ip_range_start", "Start IP:"},
	"ip_end":     {"ip_range_end", "End IP:"},
	"port_start": {"port_range_start", "Start port:"},
	"port_end":   {"port_range_end", "End port:"},
}
var tuiNetAdds = map[string]string{
	"ip_bl_add": "ip_blacklist", "ip_wl_add": "ip_whitelist",
	"port_bl_add": "port_blacklist", "port_wl_add": "port_whitelist",
}
var tuiNetRms = map[string]string{
	"ip_bl_rm": "ip_blacklist", "ip_wl_rm": "ip_whitelist",
	"port_bl_rm": "port_blacklist", "port_wl_rm": "port_whitelist",
}

func (m menuModel) doNetworkAction(action string) (menuModel, tea.Cmd) {
	if action == "" || action == "cancel" {
		return m, nil
	}
	if action == "rescan" {
		m.netCache = nil
		return m, nil
	}
	if sc, ok := tuiNetScalars[action]; ok {
		fk, prompt := sc[0], sc[1]
		m.popup = tuiInputPopup("Edit "+fk, prompt, "", func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				yamlSetScalar(fk, v)
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	if key, ok := tuiNetAdds[action]; ok {
		m.popup = tuiInputPopup("Add to "+key, "Value:", "", func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				items := yamlGetList(key)
				if !contains(items, v) {
					items = append(items, v)
					yamlSetList(key, items)
				}
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	if key, ok := tuiNetRms[action]; ok {
		items := yamlGetList(key)
		if len(items) == 0 {
			m.popup = tuiOutputPopup("Remove from "+key, []string{"(list is empty)"})
			return m, nil
		}
		var acts []tuiAction
		for _, it := range items {
			acts = append(acts, tuiAction{it, it})
		}
		acts = append(acts, tuiAction{"✕ Cancel", ""})
		m.popup = tuiActionPopup("Remove from "+key, acts, func(v string) (menuModel, tea.Cmd) {
			if v != "" {
				var kept []string
				for _, it := range items {
					if it != v {
						kept = append(kept, it)
					}
				}
				yamlSetList(key, kept)
				m.netCache = nil
			}
			return m, nil
		})
		return m, nil
	}
	return m, nil
}

func contains(items []string, v string) bool {
	for _, it := range items {
		if it == v {
			return true
		}
	}
	return false
}
