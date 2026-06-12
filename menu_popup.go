package main

// menu_popup.go — the popup layer: action menus, confirm dialogs, single-line
// text input, scrollable output/detail boxes, and the rollback picker. Mirrors
// run_popup_action / _confirm_popup / _prompt_text / run_log_popup / show_message_box.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type tuiPopupKind int

const (
	tuiPopupActions tuiPopupKind = iota // selectable list -> onSelect(value)
	tuiPopupConfirm                     // No/Yes -> onConfirm()
	tuiPopupInput                       // text input -> onSubmit(text)
	tuiPopupOutput                      // read-only output, any key closes
	tuiPopupDetail                      // scrollable read-only detail
)

// tuiAction is one selectable row: a label and an opaque value.
type tuiAction struct {
	Label string
	Value string
}

type tuiPopup struct {
	kind  tuiPopupKind
	title string

	// list / confirm
	actions []tuiAction
	sel     int
	scroll  int

	// input
	prompt string
	buf    string

	// output / detail
	lines     []string
	lineScrol int

	// callbacks (executed by the model; may return a tea.Cmd)
	onSelect  func(value string) (menuModel, tea.Cmd)
	onConfirm func() (menuModel, tea.Cmd)
	onSubmit  func(text string) (menuModel, tea.Cmd)
}

// ── Rendering ────────────────────────────────────────────────────────────────

func (p *tuiPopup) render(width int) string {
	w := width - 4
	if w < 20 {
		w = 20
	}
	if w > 90 {
		w = 90
	}
	var b strings.Builder
	top := "╔" + strings.Repeat("═", w-2) + "╗"
	bot := "╚" + strings.Repeat("═", w-2) + "╝"
	title := " " + p.title + " "
	if len(title) > w-2 {
		title = title[:w-2]
	}
	// title centered on the top border
	tl := (w - len(title)) / 2
	if tl < 1 {
		tl = 1
	}
	topLine := "╔" + strings.Repeat("═", tl-1) + title + strings.Repeat("═", w-tl-len(title)-1) + "╗"
	b.WriteString(tuiPopupBorder.Render(topLine))
	b.WriteString("\n")
	_ = top

	body := func(s string) {
		if len(s) > w-4 {
			s = s[:w-4]
		}
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString(" " + s)
		pad := w - 4 - len(s)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString(" ")
		b.WriteString(tuiPopupBorder.Render("║"))
		b.WriteString("\n")
	}

	switch p.kind {
	case tuiPopupActions:
		vis := 14
		end := p.scroll + vis
		if end > len(p.actions) {
			end = len(p.actions)
		}
		for i := p.scroll; i < end; i++ {
			label := p.actions[i].Label
			if len(label) > w-6 {
				label = label[:w-6]
			}
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		if p.scroll > 0 {
			body("▲ more above")
		}
		if end < len(p.actions) {
			body("▼ more below")
		}

	case tuiPopupConfirm:
		body("")
		for i, a := range p.actions {
			label := a.Label
			if i == p.sel {
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString(" " + tuiPopupSel.Render(padRight("  "+label, w-4)) + " ")
				b.WriteString(tuiPopupBorder.Render("║"))
				b.WriteString("\n")
			} else {
				body("  " + label)
			}
		}
		body("")
		body("↑↓ select  ENTER confirm  ESC cancel")

	case tuiPopupInput:
		body(p.prompt)
		body("> " + p.buf + "_")
		body("")
		body("ENTER = save   ESC = cancel")

	case tuiPopupOutput, tuiPopupDetail:
		vis := 16
		end := p.lineScrol + vis
		if end > len(p.lines) {
			end = len(p.lines)
		}
		for i := p.lineScrol; i < end; i++ {
			body(p.lines[i])
		}
		if len(p.lines) > vis {
			body(fmt.Sprintf("↑↓ scroll  (%d/%d)  any key / ESC close", end, len(p.lines)))
		} else {
			body("any key / ESC to close")
		}
	}

	b.WriteString(tuiPopupBorder.Render(bot))
	return b.String()
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

// ── Key handling ─────────────────────────────────────────────────────────────

func (m menuModel) handlePopupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.popup
	k := msg.String()

	switch p.kind {
	case tuiPopupActions:
		switch k {
		case "up", "k":
			if p.sel > 0 {
				p.sel--
			}
			if p.sel < p.scroll {
				p.scroll = p.sel
			}
		case "down", "j":
			if p.sel < len(p.actions)-1 {
				p.sel++
			}
			if p.sel >= p.scroll+14 {
				p.scroll = p.sel - 13
			}
		case "pgup":
			p.sel -= 14
			if p.sel < 0 {
				p.sel = 0
			}
			p.scroll = p.sel
		case "pgdown":
			p.sel += 14
			if p.sel > len(p.actions)-1 {
				p.sel = len(p.actions) - 1
			}
		case "enter":
			val := p.actions[p.sel].Value
			// "Cancel" just closes the popup — its callback is a no-op and the
			// closure could re-show a stale popup, which left the user stuck.
			if val == "cancel" || strings.EqualFold(p.actions[p.sel].Label, "Cancel") {
				m.popup = nil
				return m, nil
			}
			cb := p.onSelect
			m.popup = nil
			if cb != nil {
				return cb(val)
			}
			return m, nil
		case "esc", "q", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupConfirm:
		switch k {
		case "up", "k", "down", "j", "left", "right", "tab":
			p.sel = 1 - p.sel
		case "y", "Y":
			p.sel = 1
			fallthrough
		case "enter":
			yes := p.actions[p.sel].Value == "yes"
			cb := p.onConfirm
			m.popup = nil
			if yes && cb != nil {
				return cb()
			}
			return m, nil
		case "n", "N", "esc", "ctrl+c":
			m.popup = nil
		}
		return m, nil

	case tuiPopupInput:
		switch k {
		case "enter":
			text := strings.TrimSpace(p.buf)
			cb := p.onSubmit
			m.popup = nil
			if cb != nil {
				return cb(text)
			}
			return m, nil
		case "esc", "ctrl+c":
			m.popup = nil
		case "backspace":
			if len(p.buf) > 0 {
				p.buf = p.buf[:len(p.buf)-1]
			}
		default:
			if len(msg.Runes) == 1 && msg.Runes[0] >= 32 && msg.Runes[0] < 127 {
				p.buf += string(msg.Runes)
			}
		}
		return m, nil

	case tuiPopupOutput, tuiPopupDetail:
		switch k {
		case "up", "k":
			if p.lineScrol > 0 {
				p.lineScrol--
			}
		case "down", "j":
			if p.lineScrol < len(p.lines)-1 {
				p.lineScrol++
			}
		case "pgup":
			p.lineScrol -= 14
			if p.lineScrol < 0 {
				p.lineScrol = 0
			}
		case "pgdown":
			p.lineScrol += 14
			if p.lineScrol > len(p.lines)-1 {
				p.lineScrol = len(p.lines) - 1
			}
		default:
			m.popup = nil
		}
		return m, nil
	}
	return m, nil
}

// ── Constructors ─────────────────────────────────────────────────────────────

func tuiActionPopup(title string, actions []tuiAction, onSelect func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupActions, title: title, actions: actions, onSelect: onSelect}
}

func tuiConfirmPopup(title, dangerLabel string, onConfirm func() (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{
		kind:  tuiPopupConfirm,
		title: title,
		actions: []tuiAction{
			{Label: "✕  No — cancel", Value: "no"},
			{Label: dangerLabel, Value: "yes"},
		},
		onConfirm: onConfirm,
	}
}

func tuiInputPopup(title, prompt, def string, onSubmit func(string) (menuModel, tea.Cmd)) *tuiPopup {
	return &tuiPopup{kind: tuiPopupInput, title: title, prompt: prompt, buf: def, onSubmit: onSubmit}
}

func tuiOutputPopup(title string, lines []string) *tuiPopup {
	return &tuiPopup{kind: tuiPopupOutput, title: title, lines: lines}
}
