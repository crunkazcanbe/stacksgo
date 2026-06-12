package main

// menu_build.go — Build tab: scaffold a new service into a stack (image/service/
// stack prompts → `stacks build …`), regenerate dynamics, generate the sablier
// groups file, and run fix/repair across all stacks. Mirrors BUILD_ITEMS.
//
// The original curses Build wizard was a multi-step interactive popup; the TUI
// runs the same non-interactive `stacks build <image> <service> <stack>` engine
// behind three input prompts (and exposes the generator/fix helpers it shared).

import (
	tea "github.com/charmbracelet/bubbletea"
)

var tuiBuildItems = []tuiAction{
	{"Build new service (wizard)", "build_into"},
	{"Create new stack + add service", "build_new_stack"},
	{"Generate dynamics from ALL stacks", "gen_dyn_all"},
	{"Generate dynamics from one stack", "gen_dyn_one"},
	{"Force regen ALL dynamics", "gen_dyn_force"},
	{"Generate global inject config", "gen_inject"},
	{"Generate sablier groups config", "gen_groups"},
	{"Run stacks fix on ALL", "fix_all"},
	{"Run stacks repair on ALL", "repair_all"},
}

func (m menuModel) renderBuild() string {
	return tuiRenderActionList("BUILD", tuiBuildItems, m.sel, m.scroll, m.visibleRows(), m.width)
}

func (m menuModel) handleBuildKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		m.moveCursor(k, len(tuiBuildItems))
	case "enter":
		if m.sel < 0 || m.sel >= len(tuiBuildItems) {
			return m, nil
		}
		return m.doBuildAction(tuiBuildItems[m.sel].Value)
	}
	return m, nil
}

func (m menuModel) doBuildAction(action string) (menuModel, tea.Cmd) {
	switch action {
	case "build_into":
		// Launch the REAL interactive build wizard (same 9-step flow as the
		// Python menu): stack pick → Docker Hub image search → IP → port →
		// name → DB detect/setup → redis → companion → scaffold. The Go engine
		// (cmdBuild) drives all the prompts itself, so we just suspend the TUI
		// and hand it the terminal. An optional service-name hint seeds the
		// image search; blank = pick everything in the wizard.
		m.popup = tuiInputPopup("Build wizard", "Service name to search for (blank = full wizard):", "",
			func(hint string) (menuModel, tea.Cmd) {
				if hint != "" && !tuiValidName(hint) {
					return m, nil
				}
				if hint == "" {
					return m, tuiExecSelf("build")
				}
				return m, tuiExecSelf("build", hint)
			})
		return m, nil
	case "build_new_stack":
		// Wizard that creates a brand-new stack file. Prompt for the new stack
		// name + a service hint, then run the interactive build engine — it
		// scaffolds a new <stack>.yml when the target doesn't exist yet.
		m.popup = tuiInputPopup("New stack", "New stack name (e.g. media_5):", "",
			func(stack string) (menuModel, tea.Cmd) {
				if stack == "" || !tuiValidName(stack) {
					return m, nil
				}
				m.popup = tuiInputPopup("New stack — service", "Service name to search for:", "",
					func(hint string) (menuModel, tea.Cmd) {
						if hint == "" || !tuiValidName(hint) {
							return m, nil
						}
						// `stacks build <hint> <stack>` → svc=hint, target=stack,
						// image empty → Hub search → full wizard into a new file.
						return m, tuiExecSelf("build", hint, stack)
					})
				return m, nil
			})
		return m, nil
	case "gen_dyn_all":
		return m, tuiSelfCmd("Gen ALL dynamics", "dynamics", "generate", "all")
	case "gen_dyn_one":
		// Pick a single stack, regenerate just its dynamic config.
		m.popup = tuiInputPopup("Gen dynamics (one stack)", "Stack name:", "",
			func(stack string) (menuModel, tea.Cmd) {
				if stack == "" {
					return m, nil
				}
				return m, tuiSelfCmd("Gen dynamics "+stack, "dynamics", "generate", stack)
			})
		return m, nil
	case "gen_dyn_force":
		return m, tuiSelfCmd("Force regen ALL", "dynamics", "generate", "all", "force")
	case "gen_inject":
		return m, tuiSelfCmd("Gen global inject", "__geninject")
	case "gen_groups":
		return m, tuiSelfCmd("Gen sablier groups", "__gensrvs")
	case "fix_all":
		return m, tuiSelfCmd("Fix ALL", "fix", "all")
	case "repair_all":
		return m, tuiSelfCmd("Repair ALL", "fix", "all", "repair")
	}
	return m, nil
}
