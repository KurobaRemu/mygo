package richtext

import (
	"strings"

	"github.com/egoist/mygo/ui"
)

// tabSize is how many spaces wide a tab is: CSS's default tab-size, which
// Typora's editor computes. Each of its tabs is the tab alone in a .md-tab
// inline block (white-space: pre), so it takes tabSize spaces of its own
// style from where it starts, rather than reaching a stop of the line above
// it (record 2026-11-10, tab probes: body 8 x 3.888 = 31.10 DIPs, bold
// 34.56, a 30px heading 58.33).
const tabSize = 8

// tabbedSpans returns spans with each tab in a span of its own, tabSize
// spaces of its style wide, as Typora's .md-tab span lays out.
func (v *view) tabbedSpans(spans []ui.Span, base ui.Font) []ui.Span {
	first := -1
	for i, s := range spans {
		if strings.ContainsRune(s.Text, '\t') {
			first = i
			break
		}
	}
	if first < 0 {
		return spans
	}
	out := make([]ui.Span, 0, len(spans)+1)
	out = append(out, spans[:first]...)
	for _, s := range spans[first:] {
		if s.Text == "" {
			out = append(out, s)
			continue
		}
		start := 0
		for i := 0; i < len(s.Text); i++ {
			if s.Text[i] != '\t' {
				continue
			}
			if i > start {
				part := s
				part.Text = s.Text[start:i]
				out = append(out, part)
			}
			tab := s
			// The object replacement character, which the text system lays
			// out as an object of ObjectWidth; a tab would take its
			// system-set stop instead.
			tab.Text = "\uFFFC"
			tab.ObjectWidth = tabSize * v.spaceAdvance(s, base)
			tab.ObjectHeight, tab.ObjectDescent = 0, 0
			out = append(out, tab)
			start = i + 1
		}
		if start < len(s.Text) {
			part := s
			part.Text = s.Text[start:]
			out = append(out, part)
		}
	}
	return out
}

// spaceAdvance returns how wide a space of s's style is, over base.
func (v *view) spaceAdvance(s ui.Span, base ui.Font) float32 {
	sp := s
	sp.Text = " "
	sp.ObjectWidth, sp.ObjectHeight, sp.ObjectDescent = 0, 0, 0
	w, _ := ui.ShapeRichTextWith([]ui.Span{sp}, base, ui.TextShape{}).Size()
	return w
}
