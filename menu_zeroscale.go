package main

// menu_zeroscale.go — the "Zero Scale" per-container options, shown inside the
// Containers-tab Tab/Enter popup (only when `zero_scale: on` in the config).
//
// Zero Scale is Bellz's own Sablier replacement (the `stackwake` engine). This
// screen lets her flip wake-on-visit per container and set every option she used
// to hand-write in the Traefik sablier middleware — loading screen, idle time,
// the container group, display name, stop timeout, etc. It reads/writes the same
// config the engine watches: <configDir>/zeroscale.yaml.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// the editable loading screens shipped with the engine
var zsScreens = []string{"minecraft", "terminal", "ghost", "synthwave", "pride"}

type zsSite struct {
	Host       []string `yaml:"host,omitempty"`
	Containers []string `yaml:"containers,omitempty"`
	Service    string   `yaml:"service,omitempty"`
	Display    string   `yaml:"display,omitempty"`
	Screen     string   `yaml:"screen,omitempty"`
	Idle       string   `yaml:"session_duration,omitempty"` // per-site override, e.g. "30m"
	StopTimeout string  `yaml:"stop_timeout,omitempty"`
	AlwaysOn   bool     `yaml:"always_on,omitempty"`
	Enabled    *bool    `yaml:"enabled,omitempty"`
}

type zsConfig struct {
	IdleSeconds    int                `yaml:"idle_seconds"`
	PollSeconds    int                `yaml:"poll_seconds"`
	TraefikMetrics string             `yaml:"traefik_metrics"`
	WakeBase       string             `yaml:"wake_base"`
	DefaultScreen  string             `yaml:"default_screen"`
	Sites          map[string]*zsSite `yaml:"sites"`
}

func zeroScalePath() string { return filepath.Join(configDir(), "zeroscale.yaml") }

func zeroScaleEnabled() bool { return configLoad()["ZERO_SCALE"] == "1" }

func loadZSConfig() *zsConfig {
	c := &zsConfig{Sites: map[string]*zsSite{}}
	data, err := os.ReadFile(zeroScalePath())
	if err == nil {
		_ = yaml.Unmarshal(data, c)
	}
	if c.Sites == nil {
		c.Sites = map[string]*zsSite{}
	}
	if c.DefaultScreen == "" {
		c.DefaultScreen = "minecraft"
	}
	return c
}

func saveZSConfig(c *zsConfig) error {
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(zeroScalePath(), out, 0644)
}

// siteForContainer returns the existing site whose container list includes name,
// plus its key. Returns ("", nil) if this container is not Zero-Scaled yet.
func (c *zsConfig) siteForContainer(name string) (string, *zsSite) {
	for k, s := range c.Sites {
		for _, cn := range s.Containers {
			if cn == name {
				return k, s
			}
		}
		if k == name {
			return k, s
		}
	}
	return "", nil
}

func boolStr(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// openZeroScalePopup builds the scrollable per-container options menu.
func (m menuModel) openZeroScalePopup(name string) (menuModel, tea.Cmd) {
	c := loadZSConfig()
	key, s := c.siteForContainer(name)
	on := s != nil && (s.Enabled == nil || *s.Enabled)

	scr := c.DefaultScreen
	if s != nil && s.Screen != "" {
		scr = s.Screen
	}
	idle := "(global)"
	if s != nil && s.Idle != "" {
		idle = s.Idle
	}
	disp, group, stopT, always := name, name, "30s", false
	if s != nil {
		if s.Display != "" {
			disp = s.Display
		}
		if len(s.Containers) > 0 {
			group = strings.Join(s.Containers, ",")
		}
		if s.StopTimeout != "" {
			stopT = s.StopTimeout
		}
		always = s.AlwaysOn
	}

	acts := []tuiAction{
		{fmt.Sprintf("⚡ Wake-on-visit (Zero Scale)   [ %s ]", boolStr(on)), "toggle"},
		{fmt.Sprintf("🎨 Loading screen              [ %s ]", scr), "screen"},
		{fmt.Sprintf("💤 Idle before sleep           [ %s ]", idle), "idle"},
		{fmt.Sprintf("🏷  Display name                [ %s ]", orDash(disp)), "display"},
		{fmt.Sprintf("📦 Wake group (containers)     [ %s ]", orDash(group)), "group"},
		{fmt.Sprintf("⏱  Stop timeout                [ %s ]", orDash(stopT)), "stop"},
		{fmt.Sprintf("📌 Keep always-on (never sleep)[ %s ]", boolStr(always)), "always"},
		{"✕  Done", "cancel"},
	}
	_ = key
	m.popup = tuiActionPopup("⚡ Zero Scale — "+truncate(name, 22), acts,
		func(action string) (menuModel, tea.Cmd) {
			return m.doZeroScaleAction(name, action)
		})
	return m, nil
}

// doZeroScaleAction applies one option change, persists, and re-opens the menu so
// it stays put and shows the new value.
func (m menuModel) doZeroScaleAction(name, action string) (menuModel, tea.Cmd) {
	c := loadZSConfig()
	key, s := c.siteForContainer(name)
	ensure := func() *zsSite {
		if s == nil {
			key = name
			s = &zsSite{
				Host:       []string{name + ".loveiznothin.com"},
				Containers: []string{name},
				Service:    name + "-svc@file",
				Display:    name,
			}
			c.Sites[key] = s
		}
		return s
	}

	switch action {
	case "", "cancel":
		return m, nil
	case "toggle":
		s = ensure()
		v := !(s.Enabled == nil || *s.Enabled)
		s.Enabled = &v
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "always":
		s = ensure()
		s.AlwaysOn = !s.AlwaysOn
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "screen":
		// cycle through the 5 screens
		s = ensure()
		cur := s.Screen
		if cur == "" {
			cur = c.DefaultScreen
		}
		next := zsScreens[0]
		for i, sc := range zsScreens {
			if sc == cur {
				next = zsScreens[(i+1)%len(zsScreens)]
				break
			}
		}
		s.Screen = next
		_ = saveZSConfig(c)
		return m.openZeroScalePopup(name)
	case "idle":
		def := ""
		if s != nil {
			def = s.Idle
		}
		m.popup = tuiInputPopup("Idle before sleep — "+truncate(name, 18),
			"e.g. 30m, 2h, 600s  (blank = use global):", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.Idle = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "display":
		def := name
		if s != nil && s.Display != "" {
			def = s.Display
		}
		m.popup = tuiInputPopup("Display name — "+truncate(name, 18),
			"Shown on the loading screen:", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.Display = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "group":
		def := name
		if s != nil && len(s.Containers) > 0 {
			def = strings.Join(s.Containers, ",")
		}
		m.popup = tuiInputPopup("Wake group — "+truncate(name, 18),
			"Comma-separated containers to wake together:", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				parts := []string{}
				for _, p := range strings.Split(txt, ",") {
					if p = strings.TrimSpace(p); p != "" {
						parts = append(parts, p)
					}
				}
				if len(parts) > 0 {
					s.Containers = parts
				}
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	case "stop":
		def := "30s"
		if s != nil && s.StopTimeout != "" {
			def = s.StopTimeout
		}
		m.popup = tuiInputPopup("Stop timeout — "+truncate(name, 18),
			"How long to wait when stopping (e.g. 30s):", def,
			func(txt string) (menuModel, tea.Cmd) {
				s = ensure()
				s.StopTimeout = strings.TrimSpace(txt)
				_ = saveZSConfig(c)
				return m.openZeroScalePopup(name)
			})
		return m, nil
	}
	_ = key
	return m.openZeroScalePopup(name)
}
