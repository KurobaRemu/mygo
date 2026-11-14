package richtext

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/egoist/mygo/internal/rope"
	"github.com/egoist/mygo/ui"
)

// The editor is the client of the system's input methods: they read the
// text around the caret and compose there, in UTF-16 units of the text
// with the composition, which is a virtual insertion at the caret until it
// is committed, as one step of undo with its start.

var _ ui.TextInputClient = (*view)(nil)

// units returns the UTF-16 offset of rune i of the text shown.
func (v *view) units(i int) int {
	return rope.Prefix(v.shown.text, rope.ByteOfRune(v.shown.text, max(0, min(i, v.shown.Len())))).Units
}

// runeOfUnits returns the rune holding UTF-16 unit u, or after it.
func (v *view) runeOfUnits(u int) int {
	return rope.RuneOfUnit(v.shown.text, max(0, u))
}

func countUnits(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// virtualRune returns the rune of the text with the composition at UTF-16
// unit u of it.
func (v *view) virtualRune(u int) int {
	a, z := v.composeRange()
	start, n := v.units(a), countUnits(v.compose)
	removed := v.units(z) - start
	if u <= start {
		return v.runeOfUnits(u)
	}
	if u <= start+n {
		return a + utf8.RuneCountInString(v.compose[:ui.UTF16ByteOffset(v.compose, u-start)])
	}
	return v.runeOfUnits(u - n + removed)
}

// virtualUnits returns the UTF-16 offset of rune i of the text with the
// composition.
func (v *view) virtualUnits(i int) int {
	a, z := v.composeRange()
	n := utf8.RuneCountInString(v.compose)
	if i <= a {
		return v.units(i)
	}
	if i <= a+n {
		return v.units(a) + countUnits(string([]rune(v.compose)[:i-a]))
	}
	return v.units(i+z-a) + countUnits(v.compose) - (v.units(z) - v.units(a))
}

func (v *view) composeRange() (int, int) {
	if v.pendingSet {
		return v.pendingRange[0], v.pendingRange[1]
	}
	return v.caret, v.caret
}

func (v *view) clearComposition() {
	v.compose, v.composing = "", false
	v.composeCaret, v.composeSelected = 0, ui.TextInputRange{}
	v.pendingSet, v.pendingSourceSet = false, false
}

func (v *view) TextForRange(r ui.TextInputRange) (string, ui.TextInputRange) {
	v.sync()
	if r.Start > r.End {
		r.Start, r.End = r.End, r.Start
	}
	a, z := v.virtualRune(r.Start), v.virtualRune(r.End)
	actual := ui.TextInputRange{Start: v.virtualUnits(a), End: v.virtualUnits(z)}
	pendingA, pendingZ := v.composeRange()
	n := utf8.RuneCountInString(v.compose)
	if n == 0 && !v.pendingSet {
		return v.shown.Slice(a, z).String(), actual
	}
	var out strings.Builder
	if a < pendingA {
		out.WriteString(v.shown.Slice(a, min(z, pendingA)).String())
	}
	if a < pendingA+n && z > pendingA {
		cr := []rune(v.compose)
		out.WriteString(string(cr[max(0, a-pendingA):min(n, z-pendingA)]))
	}
	if z > pendingA+n {
		out.WriteString(v.shown.Slice(max(a, pendingZ), z).String())
	}
	return out.String(), actual
}

func (v *view) Selection() ui.TextInputSelection {
	v.sync()
	if v.pendingSet || v.compose != "" {
		a, _ := v.composeRange()
		start := v.units(a)
		return ui.TextInputSelection{Range: ui.TextInputRange{Start: start + v.composeSelected.Start, End: start + v.composeSelected.End}}
	}
	a, z := v.selection()
	return ui.TextInputSelection{Range: ui.TextInputRange{Start: v.units(a), End: v.units(z)}, Reversed: v.caret == a && a != z}
}

func (v *view) MarkedRange() (ui.TextInputRange, bool) {
	if !v.pendingSet && v.compose == "" {
		return ui.TextInputRange{}, false
	}
	a, _ := v.composeRange()
	start := v.units(a)
	return ui.TextInputRange{Start: start, End: start + countUnits(v.compose)}, true
}

func (v *view) ReplaceText(r *ui.TextInputRange, s string) {
	v.sync()
	if v.opts.ReadOnly {
		return
	}
	// As typed text goes in.
	s = normalizeNewlines(s)
	if v.lang != nil {
		v.fenceLangType(s)
		v.clearComposition()
		v.reveal = true
		v.publish()
		return
	}
	active := v.pendingSet || v.compose != ""
	a, z := v.composeRange()
	if !v.pendingSet {
		a, z = v.selection()
	}
	if r != nil && !v.pendingSet {
		a, z = v.virtualRune(r.Start), v.virtualRune(r.End)
		if a > z {
			a, z = z, a
		}
	}
	if active && s == "" {
		v.clearComposition()
		v.reveal = true
		v.publish()
		return
	}
	if active {
		v.commitText(a, z, s)
	} else if r != nil {
		v.record(false)
		v.replace(a, z, s)
	} else {
		if v.skipAutoPair(s) {
			source := v.caretSource()
			if source >= 0 {
				v.setSourceCaret(source + 1)
				v.resumeLivePrefix(nil)
			}
		} else if pair, ok := v.autoPair(s); ok {
			v.insert(pair)
			v.typed(s)
			v.caretInsideAutoPair(pair)
		} else {
			s = v.prepareBlockPrefix(s)
			v.insert(s)
			v.typed(s)
		}
	}
	v.clearComposition()
	v.reveal = true
	v.publish()
}

func (v *view) SetMarkedText(r *ui.TextInputRange, s string, selected ui.TextInputRange) {
	v.sync()
	if v.opts.ReadOnly {
		return
	}
	if s == "" {
		v.clearComposition()
		v.reveal = true
		v.publish()
		return
	}
	if !v.pendingSet {
		a, z := v.selection()
		if r != nil {
			a, z = v.virtualRune(r.Start), v.virtualRune(r.End)
			if a > z {
				a, z = z, a
			}
		}
		v.pendingRange = [2]int{a, z}
		v.pendingSet = true
		v.doc.lock()
		v.pendingSelection = v.doc.selection
		v.pendingSourceSet = v.doc.md != nil && v.transient == nil
		v.doc.mu.Unlock()
		v.composing = true
		v.coalesce = false
	}
	if selected.Start > selected.End {
		selected.Start, selected.End = selected.End, selected.Start
	}
	n := countUnits(s)
	selected.Start, selected.End = min(selected.Start, n), min(selected.End, n)
	selected.Start = countUnits(s[:ui.UTF16ByteOffset(s, selected.Start)])
	selected.End = countUnits(s[:ui.UTF16ByteOffset(s, selected.End)])
	v.compose, v.composeSelected = s, selected
	v.composeCaret = utf8.RuneCountInString(s[:ui.UTF16ByteOffset(s, selected.End)])
	v.reveal = true
}

func (v *view) UnmarkText() {
	v.commitCompose()
	v.publish()
}

func (v *view) commitText(a, z int, s string) {
	if s == "" {
		v.clearComposition()
		return
	}
	pendingSource := v.pendingSelection
	pendingSourceSet := v.pendingSourceSet
	direct := v.doc.md != nil && pendingSourceSet
	insert := s
	if direct {
		v.doc.lock()
		// The empty code line of a fenced block takes the commit through
		// the document, which writes the line ending the closing marker
		// stands in for.
		collapsed := pendingSource.Anchor.Offset == pendingSource.Caret.Offset
		direct = !collapsed || !v.doc.codeEmptyAt(pendingSource.Caret.Offset)
		if direct && collapsed {
			// A commit on a blank line writes the line ending the line
			// stands for, as typing there does.
			insert = v.doc.gapInsert(pendingSource.Caret.Offset, s)
		}
		v.doc.mu.Unlock()
	}
	prepared := s
	if v.doc.md != nil && pendingSourceSet {
		prepared = v.prepareBlockPrefix(s)
		// The commit writes the prepared bytes as one edit, space included;
		// the held space is for the typing path, which splits it out.
		v.pfx.quoteSpace = quoteSpaceEdit{}
		if insert == s {
			insert = prepared
		}
	}
	v.clearComposition()
	if direct {
		v.record(false)
		if err := v.replaceSourceErr(pendingSource.Anchor.Offset, pendingSource.Caret.Offset, insert, pendingSource.Anchor.Offset+len(prepared)); err == nil {
			return
		}
	}
	v.record(false)
	before := v.doc.Version()
	v.replace(a, z, prepared)
	if v.doc.Version() != before {
		return
	}
}

func (v *view) BoundsForRange(r ui.TextInputRange) (ui.Rect, ui.TextInputRange, bool) {
	v.sync()
	if len(v.paras) == 0 || v.width <= 0 {
		return ui.Rect{}, ui.TextInputRange{}, false
	}
	start := v.virtualRune(min(r.Start, r.End))
	end := v.virtualRune(max(r.Start, r.End))
	base, extra := v.virtualCaret(start)
	x, y, h := v.caretAt(base, extra, v.affinity)
	b := ui.Rect{X: v.padX + x, Y: v.padY + float32(y-v.scroll), W: 1, H: h}
	actual := ui.TextInputRange{Start: v.virtualUnits(start), End: v.virtualUnits(end)}
	endBase, endExtra := v.virtualCaret(end)
	x2, _, _ := v.caretAt(endBase, endExtra, v.affinity)
	if x2 > x {
		b.W = x2 - x
	}
	visible := b.Y+b.H > 0 && b.Y < v.rect.H
	return b, actual, visible
}

func (v *view) virtualCaret(i int) (int, int) {
	a, z := v.composeRange()
	n := utf8.RuneCountInString(v.compose)
	switch {
	case i <= a:
		return i, 0
	case i <= a+n:
		return a, i - a
	default:
		return i + z - a, 0
	}
}

func (v *view) IndexForPoint(p ui.Point) (int, bool) {
	v.sync()
	if v.width <= 0 {
		return 0, false
	}
	x, y := v.contentPoint(p.X, p.Y)
	para, l, _ := v.layoutPositionAt(x, y)
	i := v.shown.Len()
	if para >= 0 {
		pv := v.paraLayout(para)
		a, _ := v.composeRange()
		compStart, compEnd := -1, -1
		if v.pendingSet && pv.display != nil {
			compStart, compEnd = pv.display.composeStart, pv.display.composeEnd
		}
		if compStart >= 0 && l >= compStart && l <= compEnd {
			i = a + min(max(0, l-compStart), utf8.RuneCountInString(v.compose))
		} else {
			i = v.global(para, l)
		}
	}
	return v.virtualUnits(i), true
}
