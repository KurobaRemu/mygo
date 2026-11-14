package richtext

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// The caret-input invariant: after every step of a sequence, a rune typed
// at the caret must appear where the caret was drawn. The probe types one
// rune, checks the view and the geometry input methods use, then takes the
// probe back with undo, which must restore the document, the selection and
// the transient paragraph exactly.
//
// The probe is invisible to the caller: it gives the step back to the undo
// history and puts back the redo stack, typing attributes and coalescing
// flags it changed, so a sequence continues as if it had never run.

const caretProbeRune = "q"

// caretInputState is what the invariant puts back after its probe.
type caretInputState struct {
	source    string
	selection SourceSelection
	anchor    int
	caret     int

	shown      string
	viewAnchor int
	viewCaret  int

	transient    *transientInvariant
	transientPtr *transientBlock

	undoLen     int
	redo        []undoStep
	mdLast      [3]int
	mdOK        bool
	docCoalesce bool
	docLastEdit time.Time
	typing      *Attributes

	viewCoalesce     bool
	viewCoalesceKind coalesceKind
	viewMode         editMode
	prefix           *livePrefix
	empty            *emptyContainer
	literals         []parser.LiteralLine
}

// transientInvariant is a transient block's state, compared across the
// probe and put back by undo.
type transientInvariant struct {
	anchor          SourcePoint
	write           int
	atEOF, finalEOL bool
	mode            transientMode
	marker          string
	brk             pendingBreak
	preview         string
}

func transientInvariantOf(t *transientBlock) *transientInvariant {
	if t == nil {
		return nil
	}
	return &transientInvariant{
		anchor: t.anchor, write: t.write, atEOF: t.atEOF, finalEOL: t.finalEOL,
		mode: t.mode, marker: t.marker, brk: t.brk, preview: t.preview.Source(),
	}
}

func (a *transientInvariant) equal(b *transientInvariant) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (a *transientInvariant) String() string {
	if a == nil {
		return "none"
	}
	return fmt.Sprintf("anchor=%+v write=%d atEOF=%v finalEOL=%v mode=%d marker=%q brk=%+v preview=%q",
		a.anchor, a.write, a.atEOF, a.finalEOL, a.mode, a.marker, a.brk, a.preview)
}

func captureCaretInputState(d *Document) (*view, caretInputState) {
	d.lock()
	defer d.mu.Unlock()
	v := d.activeView()
	st := caretInputState{
		selection:   d.selection,
		anchor:      d.anchor,
		caret:       d.caret,
		undoLen:     len(d.undo),
		docCoalesce: d.coalesce,
		docLastEdit: d.lastEdit,
	}
	st.redo = append([]undoStep(nil), d.redo...)
	if d.md != nil {
		st.source = d.md.p.String()
		st.mdLast, st.mdOK = d.md.last, d.md.ok
		st.literals = d.md.p.LiteralLines()
	}
	if d.typing != nil {
		cp := *d.typing
		st.typing = &cp
	}
	if v != nil {
		st.shown = v.shown.String()
		st.viewAnchor, st.viewCaret = v.selAnchor, v.caret
		st.viewCoalesce, st.viewCoalesceKind, st.viewMode = v.coalesce, v.coalesceKind, v.mode
		st.transient = transientInvariantOf(v.transient)
		st.transientPtr = v.transient
		if v.pfx.live != nil {
			cp := *v.pfx.live
			st.prefix = &cp
		}
		if v.empty != nil {
			// The retained empty container (`****` after deleting the
			// content of a strong) is view state: the parser does not hold
			// the element, so undoing the probe cannot bring it back from
			// the source.
			cp := *v.empty
			st.empty = &cp
		}
	}
	return v, st
}

// restoreCaretInputState puts back the state the probe changed besides the
// document: the redo stack, the typing attributes and coalescing. The
// document's source, selection and transient are already back from undo.
func restoreCaretInputState(d *Document, st caretInputState) {
	d.lock()
	defer d.mu.Unlock()
	d.redo = append(d.redo[:0], st.redo...)
	if d.md != nil {
		d.md.last, d.md.ok = st.mdLast, st.mdOK
		d.md.p.SetLiteralLines(st.literals)
	}
	d.coalesce, d.lastEdit = st.docCoalesce, st.docLastEdit
	d.typing = nil
	if st.typing != nil {
		cp := *st.typing
		d.typing = &cp
	}
	if v := d.activeView(); v != nil {
		v.coalesce, v.coalesceKind, v.mode = st.viewCoalesce, st.viewCoalesceKind, st.viewMode
		v.transient = st.transientPtr
		v.pfx.live = nil
		if st.prefix != nil {
			cp := *st.prefix
			v.pfx.live = &cp
		}
		v.empty = nil
		if st.empty != nil {
			cp := *st.empty
			v.empty = &cp
		}
	}
}

// caretInputMismatches reports how the state now differs from st. The
// caller holds the document lock.
func caretInputMismatches(d *Document, v *view, st caretInputState) []string {
	var out []string
	source := ""
	if d.md != nil {
		source = d.md.p.String()
	}
	if source != st.source {
		out = append(out, fmt.Sprintf("source %q, want %q", source, st.source))
	}
	if d.selection != st.selection {
		out = append(out, fmt.Sprintf("source selection %+v, want %+v", d.selection, st.selection))
	}
	if len(d.undo) != st.undoLen {
		out = append(out, fmt.Sprintf("undo has %d steps, want %d", len(d.undo), st.undoLen))
	}
	if v == nil {
		out = append(out, "the editor has no view")
		return out
	}
	if got := v.shown.String(); got != st.shown {
		out = append(out, fmt.Sprintf("view text %q, want %q", got, st.shown))
	}
	if v.caret != st.viewCaret || v.selAnchor != st.viewAnchor {
		out = append(out, fmt.Sprintf("view selection %d-%d, want %d-%d", v.selAnchor, v.caret, st.viewAnchor, st.viewCaret))
	}
	if !transientInvariantOf(v.transient).equal(st.transient) {
		out = append(out, fmt.Sprintf("transient %s, want %s", transientInvariantOf(v.transient), st.transient))
	}
	return out
}

// paragraphText returns the text of paragraph p of the view.
func paragraphText(v *view, p int) string {
	ps := v.shown.paraStart(p)
	return v.shown.Slice(ps, ps+v.para(p).n).String()
}

// displayRunes returns the runes paragraph p draws, markers included.
func displayRunes(v *view, p int) []rune {
	pv := v.paraLayout(p)
	if pv == nil || pv.display == nil {
		return nil
	}
	return []rune(spansText(pv.display.Spans))
}

// sameCaretLine reports whether the rune at display index i of paragraph p
// is drawn on the line the caret rect was on. Typing can dissolve an empty
// paragraph into a line of the one before it, as a lone "*" under a list
// item stops being an empty item once a rune follows it; the rune still has
// to go where the caret was drawn.
func sameCaretLine(v *view, p, i int, pre ui.Rect, preScroll float64) bool {
	_, oy := v.origin(p)
	y, h := displayCaretLine(v, p, i)
	y += v.padY + float32(oy) - float32(v.scroll)
	preY := pre.Y - float32(v.scroll-preScroll)
	return y < preY+pre.H && preY < y+h
}

// displayCaretLine is where the view lays out display rune i of paragraph
// p within the paragraph: the caret's top and height, without the
// paragraph's origin or the viewport's scroll. Paragraph origins move when
// a transient preview materializes, and the caret's line does not.
func displayCaretLine(v *view, p, i int) (y, h float32) {
	pv := v.paraLayout(p)
	if pv == nil || pv.display == nil || i < 0 || i >= len(pv.units) {
		return 0, 0
	}
	pos := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[i], Affinity: ui.TextDownstream})
	return pos.Y, pos.H
}

// displayCaretRect is where the view draws the caret at display rune i of
// paragraph p, and the rune's advance.
func displayCaretRect(v *view, p, i int) (ui.Rect, float32) {
	pv := v.paraLayout(p)
	if pv == nil || pv.display == nil || i < 0 || i >= len(pv.units) {
		return ui.Rect{}, 0
	}
	pos := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[i], Affinity: ui.TextDownstream})
	ox, oy := v.origin(p)
	r := ui.Rect{X: v.padX + ox + pos.X, Y: v.padY + float32(oy+float64(pos.Y)-v.scroll), W: 1, H: pos.H}
	if i+1 < len(pv.units) {
		next := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[i+1], Affinity: ui.TextDownstream})
		r.W = next.X - pos.X
	} else {
		r.W = pos.H * 0.5
	}
	return r, r.W
}

// assertCaretIsInput checks that the caret is drawn in an existing
// paragraph, that the viewport holds it, and that a rune typed there is
// drawn where the caret was. It gives the document back as it found it.
// step names the step under test in failures.
func assertCaretIsInput(t *testing.T, d *Document, tt *ui.Tester, step string) {
	t.Helper()
	if err := checkCaretIsInput(d, tt); err != nil {
		if step != "" {
			t.Fatalf("%s: caret-input: %v", step, err)
		}
		t.Fatalf("caret-input: %v", err)
	}
}

// checkCaretIsInput is assertCaretIsInput for callers that minimize a
// failing sequence: it reports the mismatch instead of failing the test.
func checkCaretIsInput(d *Document, tt *ui.Tester) error {
	d.lock()
	v := d.activeView()
	if v == nil || v.opts.ReadOnly {
		d.mu.Unlock()
		return nil
	}
	composing := v.compose != "" || v.pendingSet
	langInput := v.lang != nil
	caret, anchor := v.caret, v.selAnchor
	shownLen, paraCount := v.shown.Len(), v.shown.ParagraphCount()
	d.mu.Unlock()
	if err := d.CheckSource(); err != nil {
		return fmt.Errorf("the projection does not match a reparse of the source: %w", err)
	}
	if composing {
		// A composition owns the caret and its input; its commit is the
		// step under test.
		return nil
	}
	if langInput {
		// The fence language input owns the caret; the document draws none
		// while it holds the focus (record 2026-11-11, probe92/right-right).
		return nil
	}
	if paraCount == 0 {
		return fmt.Errorf("the view drew no paragraph")
	}
	if caret < 0 || caret > shownLen {
		return fmt.Errorf("caret %d is outside the view text of %d runes", caret, shownLen)
	}
	p0 := v.shown.paraAt(caret)
	if p0 < 0 || p0 >= paraCount {
		return fmt.Errorf("caret %d is in paragraph %d of %d", caret, p0, paraCount)
	}
	if pv := v.paraLayout(p0); pv == nil || pv.display == nil {
		return fmt.Errorf("paragraph %d has no layout", p0)
	}
	pre, visible := tt.TextCaret()
	if !visible {
		return fmt.Errorf("the caret is not drawn in view at paragraph %d (%q)", p0, paragraphText(v, p0))
	}
	if anchor != caret {
		// A selection has no caret to compare; replace-selection semantics
		// are not this invariant.
		return nil
	}
	p0Text := paragraphText(v, p0)
	preSpans := displayRunes(v, p0)
	preStyle := v.para(p0).style
	preSource := SourcePoint{}
	preViewSource := -1
	preScroll := float64(0)
	prePadY := float32(0)
	preOY := float64(0)
	d.lock()
	preSource = d.selection.Caret
	preViewSource = preSource.Offset
	if v.transient != nil && v.transient.isLineBreak() {
		// The document's selection stays on the break's anchor while the
		// view's caret stands after the held line's break: the preview
		// holds where that caret writes.
		preViewSource = v.transient.preview.SourceSelection().Caret.Offset
	}
	preScroll = v.scroll
	prePadY = v.padY
	_, preOY = v.origin(p0)
	d.mu.Unlock()
	preDisplay := -1
	preLo, preHi := -1, -1
	if pv := v.paraLayout(p0); pv != nil && pv.display != nil {
		preDisplay = pv.display.Map.DisplayOfSource(preSource.Offset, preSource.Affinity == BoundaryUpstream)
		if preDisplay >= 0 && preDisplay+1 < len(pv.display.Map.offsets) {
			// The source range the rune after the caret covers. A synthetic
			// segment shares its source among its shown runes, so the rune a
			// step writes there can lie anywhere in the range the caret's
			// next rune holds.
			preLo = pv.display.Map.offsets[preDisplay].SourceAfter
			preHi = pv.display.Map.offsets[preDisplay+1].Source
		}
	}

	_, before := captureCaretInputState(d)
	d.BreakUndoCoalescing()
	d.lock()
	v.record(false)
	d.mu.Unlock()

	tt.Type(caretProbeRune)
	tt.Frame()

	d.lock()
	v = d.activeView()
	afterCaret := -1
	if v != nil {
		afterCaret = v.caret
	}
	probeSource := ""
	if d.md != nil {
		probeSource = d.md.p.String()
	}
	caretSource := 0
	if v != nil {
		caretSource = d.selection.Caret.Offset
	}
	undoMade := len(d.undo) > before.undoLen
	d.mu.Unlock()

	// Find the probe rune where the editor wrote it, in the display text
	// with its markers, which the document's own text does not hold: the
	// rune may sit inside a revealed marker such as a link destination.
	// Typing attributes insert markers around the rune, so the caret it
	// leaves is not right after the rune: locate the rune in the region
	// the step changed, after the bytes it kept.
	qStart := -1
	if from, to, newEnd := changedBytes(before.source, probeSource); from >= 0 && from <= to && to <= len(before.source) && newEnd >= from && newEnd <= len(probeSource) {
		kept := 0
		for kept < to-from && kept < newEnd-from && before.source[from+kept] == probeSource[from+kept] {
			kept++
		}
		if i := strings.Index(probeSource[from+kept:newEnd], caretProbeRune); i >= 0 {
			qStart = from + kept + i
		}
	}
	pq, dq := -1, -1
	var postSpans []rune
	if qStart >= 0 && qStart+len(caretProbeRune) <= len(probeSource) && probeSource[qStart:qStart+len(caretProbeRune)] == caretProbeRune {
		for p := 0; p < v.shown.ParagraphCount() && pq < 0; p++ {
			pv := v.paraLayout(p)
			if pv == nil || pv.display == nil {
				continue
			}
			spans := displayRunes(v, p)
			n := min(len(spans), len(pv.display.Map.offsets)-1)
			for i := 0; i < n; i++ {
				// Display rune i covers the source range
				// [SourceAfter of boundary i, Source of boundary i+1):
				// the gap between the two skips a collapsed marker, so the
				// rune after "**bold**" starts at the gap's far end. The
				// range can hold several source runes where a synthetic
				// segment's shown rune covers a share of its source, as a
				// code span's soft break line does.
				if qStart >= pv.display.Map.offsets[i].SourceAfter &&
					qStart+len(caretProbeRune) <= pv.display.Map.offsets[i+1].Source &&
					string(spans[i]) == caretProbeRune {
					pq, dq, postSpans = p, i, spans
					break
				}
			}
		}
	}
	if os.Getenv("ZZZDEBUG") != "" {
		fmt.Printf("DBG2 p0=%d preDisplay=%d preLo=%d preHi=%d qStart=%d pq=%d pre=%q\n", p0, preDisplay, preLo, preHi, qStart, pq, string(preSpans))
	}
	if pq < 0 && preLo >= 0 && qStart >= preLo && qStart+len(caretProbeRune) <= preHi {
		// The source range the caret's next shown rune covers: a synthetic
		// segment shares its source among its shown runes, so the rune the
		// step wrote there need not start at a display boundary. It is
		// drawn where the caret was; the display cannot place it closer.
		pq, dq, postSpans = p0, preDisplay, displayRunes(v, p0)
	}

	var fail string
	if pq < 0 {
		fail = fmt.Sprintf("typing %q at the caret drew nothing there\n source %q, caret %d, source caret %d, probe source caret %d", caretProbeRune, probeSource, afterCaret, preSource.Offset, caretSource)
	} else {
		atCaretSource := qStart == preViewSource
		// A rune written at the held line's break is at the caret even
		// though it is past the document's selection, and laying the line
		// out as a real block can move it by a line.
		// The caret's x is not compared as pixels: inserting a rune can
		// change how the text before it is shaped, and the same runes then
		// advance differently (a full-width period drawn at half width while
		// it is the only rune, at full width beside Latin text). The rune is
		// at the caret when it starts at the same display index among the
		// same runes. Typing need not change the display text before the
		// rune, and a label can wrap as one run and move to another line
		// while the caret stays at its text position. The line comparison is
		// for when the markers shown before the rune changed, so the display
		// indices no longer line up.
		preLineY := float32(pre.Y) - prePadY - float32(preOY) + float32(preScroll)
		runeLineY, runeLineH := displayCaretLine(v, pq, dq)
		samePrefix := preDisplay >= 0 && preDisplay <= len(preSpans) && dq <= len(postSpans) &&
			string(preSpans[:preDisplay]) == string(postSpans[:dq])
		switch {
		case pq != p0:
			// The rune may reparse the block around it, merging the
			// paragraphs it was drawn among into one or splitting them, as a
			// table's delimiter row does once a rune lands in it. The rune is
			// at the caret when it is drawn on the line the caret was.
			// A paragraph that drew nothing is an empty line held by block
			// markers alone, as an empty nested item's bullet. The rune's
			// own markers can dissolve that line into the paragraph above,
			// and the line goes with it.
			merged := len(preSpans) == 0 && pq == p0-1
			if !merged && !sameCaretLine(v, pq, dq, pre, preScroll) {
				fail = fmt.Sprintf("the caret was drawn in paragraph %d (%q) but the rune went to paragraph %d (%q)\n source %q, caret %d",
					p0, p0Text, pq, paragraphText(v, pq), probeSource, afterCaret)
			}
		case !samePrefix:
			// Typing revealed, collapsed or inserted the markers the
			// paragraph shows before the rune, so its display index no
			// longer lines up, and the rune moves with the bytes the
			// markers added or removed, up or down whole lines. The step
			// is at the caret when its first changed byte is the caret's
			// source position. Otherwise the bytes before the rune grew by
			// at most the markers around it: a rune above the caret's line,
			// or more than one line below it, is not at the caret.
			from, _, _ := changedBytes(before.source, probeSource)
			if !atCaretSource && from != preSource.Offset && (runeLineY < preLineY-pre.H/2 || runeLineY >= preLineY+1.5*pre.H) {
				fail = fmt.Sprintf("the caret was drawn in paragraph %d (%q) at line %.1f of the paragraph but the rune went to line %.1f\n source %q",
					p0, p0Text, preLineY, runeLineY, probeSource)
			}
		case dq != preDisplay && !atCaretSource && (runeLineY >= preLineY+pre.H || runeLineY+runeLineH <= preLineY):
			fail = fmt.Sprintf("the caret was drawn in paragraph %d (%q) at line %.1f of the paragraph but the rune went to line %.1f\n source %q",
				p0, p0Text, preLineY, runeLineY, probeSource)
		case v.para(pq).style != preStyle:
			// The probe's rune changed the paragraph's block style (typing
			// a rune after a bare "#" makes a heading), so the text before
			// the rune is drawn at another size and its index moved.
		case dq != preDisplay:
			fail = fmt.Sprintf("the caret was drawn after %d runes of paragraph %d (%q) but the rune went after %d\n source %q, caret %d",
				preDisplay, p0, p0Text, dq, probeSource, afterCaret)
		}
	}

	mismatches := undoProbe(d, tt, before, undoMade, probeSource)
	restoreCaretInputState(d, before)
	if fail != "" {
		return fmt.Errorf("%s", fail)
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("undo did not restore the state before the probe: %s", strings.Join(mismatches, "; "))
	}
	return nil
}

// probeMadeQuoteSpace reports whether the probe rune opened a quote marker:
// the synthesized space after `>` is an edit of its own in Typora's undo
// history (record 2026-11-11, probe83/undo-after-gt), so this probe makes
// two steps and takes two undos to put back.
func probeMadeQuoteSpace(beforeSource, probeSource string) bool {
	start, _, newEnd := changedBytes(beforeSource, probeSource)
	if start <= 0 || start > len(beforeSource) || beforeSource[start-1] != '>' {
		return false
	}
	return probeSource[start:newEnd] == " "+caretProbeRune
}

// undoProbe takes the probe back with undo and returns how the state it
// found differs from before.
func undoProbe(d *Document, tt *ui.Tester, before caretInputState, undoMade bool, probeSource string) []string {
	d.lock()
	now := len(d.undo)
	d.mu.Unlock()
	quoteSpace := probeMadeQuoteSpace(before.source, probeSource)
	if !undoMade || now != before.undoLen+1 && !(quoteSpace && now == before.undoLen+2) {
		// Nothing of the probe reached the undo history: undoing now would
		// take back the caller's step instead.
		return []string{fmt.Sprintf("the probe made no step of its own (undo now %d, before %d)", now, before.undoLen)}
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if quoteSpace {
		tt.Key(ui.Cmd, ui.KeyZ)
		tt.Frame()
	}
	d.lock()
	if d.selection.Anchor.Offset == before.selection.Anchor.Offset && d.selection.Caret.Offset == before.selection.Caret.Offset {
		// Undo returns the offset but not the boundary affinity of the
		// step's selection, and the drawn caret is the same either way.
		d.selection = before.selection
	}
	if v := d.activeView(); v != nil && before.transient == nil && v.transient != nil &&
		d.md != nil && d.md.p.String() == "" && v.transient.preview.Source() == "" {
		// Undoing input that emptied the document leaves the empty
		// document's implicit paragraph as a transient. The user sees the
		// same paragraph either way; the probe puts the state back as it
		// found it.
		v.transient = nil
	}
	mismatches := caretInputMismatches(d, d.activeView(), before)
	d.mu.Unlock()
	return mismatches
}

// TestCaretInputUndoRestoresEmptyItem pins the case the user-style fuzz
// found: the caret at the start of an empty list item sits where the item's
// content begins, and the line's bytes after it, the empty emphasis's "*",
// are not a live prefix the undo brought back. Undoing a rune typed there
// restores the line, and the line must project the paragraphs it did.
func TestCaretInputUndoRestoresEmptyItem(t *testing.T) {
	d := NewMarkdownDocument("- a\n- *\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	// The caret at the empty item's content start, with the close of an
	// emphasis after it: typing at the caret must not touch it.
	d.lock()
	want := d.activeView().shown.String()
	wantParas := d.text.ParagraphCount()
	d.mu.Unlock()
	d.SetSourceSelection(sourceCaret(6))
	tt.Frame()
	tt.Type("q")
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	d.lock()
	source, got := d.md.p.String(), d.activeView().shown.String()
	gotParas := d.text.ParagraphCount()
	d.mu.Unlock()
	if source != "- a\n- *\n" || got != want || gotParas != wantParas {
		t.Fatalf("undoing the rune left source %q shown %q with %d paragraphs, want %q shown %q with %d",
			source, got, gotParas, "- a\n- *\n", want, wantParas)
	}
}

// TestBackspaceEmptyItemKeepsSourceCaretCanonical pins the case the
// user-style fuzz found: deleting an empty list item's line joins the
// following item's content in its place, and the view's caret is at that
// content's start. The source selection, mapped through the patch, must land
// there too, not before the joined line's marker: input at the marker would
// reparse the line, and undo, which snaps the caret to the content start,
// would not return the state the step started with.
func TestBackspaceEmptyItemKeepsSourceCaretCanonical(t *testing.T) {
	d := NewMarkdownDocument("- \n- x\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	d.SetSourceSelection(sourceCaret(2))
	tt.Frame()
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	d.lock()
	source, sel := d.md.p.String(), d.selection
	d.mu.Unlock()
	if source != "- x\n" {
		t.Fatalf("source %q, want %q", source, "- x\n")
	}
	if sel.Caret.Offset != 2 {
		t.Fatalf("source caret %d, want 2, the start of x", sel.Caret.Offset)
	}
}

// TestCaretInputAfterEmptyListExit is the case issue 42 reported: after
// typing a list item, Enter twice, the caret is drawn in the paragraph the
// empty item exits to, and the next rune must go there.
func TestCaretInputAfterEmptyListExit(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Type("- a")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after typing - a")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the first Enter")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the second Enter")
}

// TestCaretInputAfterLeavingHardBreakLine pins the case the user-style fuzz
// found: a source line ending in an escaped break, "\<newline>", shows as a
// plain newline, so its source bytes run past its shown ones. Stepping left
// from the line after it has to land before the backslash, at the end of the
// line drawn above, which is where the next rune goes.
func TestCaretInputAfterLeavingHardBreakLine(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Type("[")
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after stepping left over the break")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	want := "[\n\nq\\\n]"
	if source != want || caret != 4 {
		t.Fatalf("typing at the line end before the break gave source %q caret %d, want %q caret 4", source, caret, want)
	}
	assertCaretIsInput(t, d, tt, "after typing before the break")
}

// TestFenceClosingMarkerCaretSnapsToContent pins the recorded Typora
// behavior for a source caret at the start of a fenced code block's
// closing marker: Typora snaps it to the end of the last code line, and
// typing there appends to that line.
func TestFenceClosingMarkerCaretSnapsToContent(t *testing.T) {
	d := NewMarkdownDocument("```go\nalpha\n```\n")
	tt, _ := richTester(t, d)
	d.selectFromSource(12, 12)
	tt.Frame()
	d.lock()
	sel := d.selection
	d.mu.Unlock()
	if sel.Caret.Offset != 11 {
		t.Fatalf("caret 12 at the closing marker snapped to %d, want the code line end 11", sel.Caret.Offset)
	}
	assertCaretIsInput(t, d, tt, "at the closing marker")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "```go\nalphaq\n```\n" || caret != 12 {
		t.Fatalf("typing at the closing marker gave source %q caret %d, want %q caret 12",
			source, caret, "```go\nalphaq\n```\n")
	}
}

// TestCaretInputAfterEmptyLineIMECommit pins the case the user-style fuzz
// found: Shift+Enter on an empty document holds a transient line break;
// committing an IME composition materializes it as the first line of the
// source, and the next rune must land after the committed text. The first
// line is blank and belongs to the paragraph the caret is drawn in, so the
// projection has to replace that paragraph, not append another.
func TestCaretInputAfterEmptyLineIMECommit(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Compose("zhong", 5)
	tt.Frame()
	tt.Type("中")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the IME commit")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "\n中q" || caret != 5 {
		t.Fatalf("typing after the commit gave source %q caret %d, want %q caret 5", source, caret, "\n中q")
	}
	assertCaretIsInput(t, d, tt, "after typing after the commit")
}

// TestCaretInputAttributedInEmptyItem pins the case the user-style fuzz
// found: a source line that is only a container, as "- " or "- *", whose
// paragraph holds no bytes of its own. With formatting on for typing, the
// first rune has to be written at the caret with the markers its attributes
// need; the paragraph has no bytes to format in place, and dropping the rune
// leaves the caret meaning nothing.
func TestCaretInputAttributedInEmptyItem(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
		at, caret       int
	}{
		{"bullet", "- \n", "- *q*\n", 2, 4},
		{"quote", "> \n", "> *q*\n", 2, 4},
		{"ordered", "1. \n", "1. *q*\n", 3, 5},
		{"task", "- [ ] \n", "- [ ] *q*\n", 6, 8},
		{"quote list", "> - \n", "> - *q*\n", 4, 6},
		{"nested item", "- *\n", "- **q*\n", 3, 5},
		{"blank line", "\n", "*q*\n", 0, 2},
		{"blank lines", "\n\n", "*q*\n\n", 0, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument(c.src)
			tt, _ := richTester(t, d)
			d.selectFromSource(c.at, c.at)
			tt.Frame()
			tt.Key(ui.Cmd, ui.KeyI)
			tt.Frame()
			assertCaretIsInput(t, d, tt, "with italic on at the empty paragraph")
			tt.Type("q")
			tt.Frame()
			d.lock()
			source, caret := d.md.p.String(), d.selection.Caret.Offset
			d.mu.Unlock()
			if source != c.want || caret != c.caret {
				t.Fatalf("typing italic gave source %q caret %d, want %q caret %d", source, caret, c.want, c.caret)
			}
			assertCaretIsInput(t, d, tt, "after typing italic")
		})
	}
}

// TestCaretBetweenFormattedAutoPair pins how a bracket typed inside a
// formatted span lands: Cmd+I formats the word at the caret (recorded
// below), which leaves the caret before the closing marker, and Typora's
// auto-pair only adds the closing bracket when the source character after
// the caret is whitespace or one of the closing/opening delimiters. Here
// the next byte is the emphasis marker, so `[` stands alone
// (recorded in probe/auto-pair-italic: `alpha`, Cmd+I, `[` saves
// `*alpha[*`).
func TestCaretBetweenFormattedAutoPair(t *testing.T) {
	d := NewMarkdownDocument("alpha\n")
	tt, _ := richTester(t, d)
	d.SetSelection(5, 5)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyI)
	tt.Frame()
	tt.Type("[")
	tt.Frame()
	d.lock()
	source, sel := d.md.p.String(), d.selection
	d.mu.Unlock()
	want := "*alpha[*\n"
	if source != want || sel.Caret.Offset != 7 {
		t.Fatalf("typing italic [ gave source %q caret %d, want %q caret 7", source, sel.Caret.Offset, want)
	}
}

// TestCaretInputWhitespaceInTransient pins the case the user-style fuzz
// found: a space as the first content of a transient paragraph on a blank
// document cannot be written, as Markdown has no line for it; the reparse
// would drop the paragraph and take the caret out of it. The block stays
// transient, the space lives in its view, and the next rune materializes
// both as the paragraph's content.
func TestCaretInputWhitespaceInTransient(t *testing.T) {
	t.Run("paragraph", func(t *testing.T) {
		d := NewMarkdownDocument("")
		tt, _ := richTester(t, d)
		tt.Frame()
		tt.Key(0, ui.KeyEnter)
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the Enter")
		tt.Type(" ")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the space")
		d.lock()
		source := d.md.p.String()
		d.mu.Unlock()
		if source != "" {
			t.Fatalf("the space alone wrote source %q, want the block to stay transient", source)
		}
		tt.Type("q")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the rune")
		d.lock()
		source, caret := d.md.p.String(), d.selection.Caret.Offset
		d.mu.Unlock()
		if source != "\n\n q" || caret != 4 {
			t.Fatalf("typing after the space gave source %q caret %d, want %q caret 4", source, caret, "\n\n q")
		}
	})
	t.Run("line break", func(t *testing.T) {
		d := NewMarkdownDocument("")
		tt, _ := richTester(t, d)
		tt.Frame()
		tt.Key(ui.Shift, ui.KeyEnter)
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the break")
		tt.Type(" ")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the space")
		d.lock()
		source := d.md.p.String()
		d.mu.Unlock()
		if source != "" {
			t.Fatalf("the space alone wrote source %q, want the break to stay transient", source)
		}
		tt.Type("q")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the rune")
		d.lock()
		source, caret := d.md.p.String(), d.selection.Caret.Offset
		d.mu.Unlock()
		if source != "\n q" || caret != 3 {
			t.Fatalf("typing after the space gave source %q caret %d, want %q caret 3", source, caret, "\n q")
		}
	})
	t.Run("after content", func(t *testing.T) {
		d := NewMarkdownDocument("b")
		tt, _ := richTester(t, d)
		tt.Frame()
		tt.Key(0, ui.KeyEnter)
		tt.Key(0, ui.KeyEnter)
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the Enters")
		tt.Type(" ")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the space")
		d.lock()
		source := d.md.p.String()
		d.mu.Unlock()
		if source != "b" {
			t.Fatalf("the space alone wrote source %q, want the empty paragraph to stay transient", source)
		}
		tt.Type("q")
		tt.Frame()
		assertCaretIsInput(t, d, tt, "after the rune")
		d.lock()
		source, caret := d.md.p.String(), d.selection.Caret.Offset
		d.mu.Unlock()
		if source != "b\n\n\n\n q" || caret != 7 {
			t.Fatalf("typing after the space gave source %q caret %d, want %q caret 7", source, caret, "b\n\n\n\n q")
		}
	})
}

// TestCaretInputPlainRuneInListSoftBreak pins the case the user-style fuzz
// found: a plain run typed into the held break after Shift+Enter in a list
// item still has a source: the break materializes with the item's
// continuation prefix and the run lands on the new line, outside the
// emphasis the first line closed.
func TestCaretInputPlainRuneInListSoftBreak(t *testing.T) {
	d := NewMarkdownDocument("- x\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyI)
	tt.Frame()
	tt.Type("[")
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	want := "- *x[*\n  q\n"
	if source != want || caret != 10 {
		t.Fatalf("typing in the break gave source %q caret %d, want %q caret 10", source, caret, want)
	}
	assertCaretIsInput(t, d, tt, "after typing in the break")
}

// TestCaretInputBreakInEmptiedTaskItem pins the case the user-style fuzz
// found: after the last content of a task item is deleted, the paragraph is
// empty but still follows the item's prefix, so its end is after the prefix,
// not at offset zero. The held break starts there and draws its caret on the
// line the next rune is written on.
func TestCaretInputBreakInEmptiedTaskItem(t *testing.T) {
	d := NewMarkdownDocument("- [ ] a\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the backspace")
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the break")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "- [ ] \n" || caret != 6 {
		t.Fatalf("the break wrote source %q caret %d, want %q caret 6", source, caret, "- [ ] \n")
	}
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the rune")
	d.lock()
	source, caret = d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "- [ ] \n  q\n" || caret != 10 {
		t.Fatalf("typing after the break gave source %q caret %d, want %q caret 10", source, caret, "- [ ] \n  q\n")
	}
}

// TestCaretInputSpaceThenShiftEnter pins the case the user-style fuzz
// found: a space typed into an empty document leaves one visible empty
// paragraph, and its end is after the space, not at offset zero. The held
// line break then starts from that end, so the caret it draws is the line
// the next rune is written on.
func TestCaretInputSpaceThenShiftEnter(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Type(" ")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the space")
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the break")
	d.lock()
	source := d.md.p.String()
	d.mu.Unlock()
	if source != " " {
		t.Fatalf("the break alone wrote source %q, want the space and a pending break", source)
	}
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the rune")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != " \\\nq" || caret != 4 {
		t.Fatalf("typing after the break gave source %q caret %d, want %q caret 4", source, caret, " \\\nq")
	}
}

// TestCaretInputShiftEnterInCodeSpan pins the case the user-style fuzz
// found: a soft line break inside an inline code span shows as one
// collapsed space, and the caret the break leaves must stay before the
// code's next rune, though the span's source is one synthetic segment.
func TestCaretInputShiftEnterInCodeSpan(t *testing.T) {
	d := NewMarkdownDocument("- [ ] a\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Type("0")
	tt.Type("`")
	tt.Frame()
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	tt.Type("`")
	tt.Frame()
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the break")
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the rune")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "- [ ] `a\n  q0`\n" || caret != 12 {
		t.Fatalf("typing after the break gave source %q caret %d, want %q caret 12", source, caret, "- [ ] `a\n  q0`\n")
	}
}

// TestCaretInputShiftEnterInCodeSpanMultibyte pins the multi-byte side of
// the same case: the collapsed space holds the break's source bytes, and a
// rune after it keeps its own boundaries, so the caret before it stays put.
func TestCaretInputShiftEnterInCodeSpanMultibyte(t *testing.T) {
	d := NewMarkdownDocument("- [ ] a\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Type("你")
	tt.Type("0")
	tt.Type("0")
	tt.Type("`")
	tt.Frame()
	tt.Key(0, ui.KeyUp)
	tt.Frame()
	tt.Type("`")
	tt.Frame()
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the break")
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the rune")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "- [ ] `a\n  q你00`\n" || caret != 12 {
		t.Fatalf("typing after the break gave source %q caret %d, want %q caret 12", source, caret, "- [ ] `a\n  q你00`\n")
	}
}

// TestCaretInputInsideBoldWithTypingOff pins the case the user-style fuzz
// found: inside a strong, ⌘B turns bold off for typing, and the next rune
// is still written at the caret: the strong's markers split around it.
//
// The pair starts in the bytes rather than being typed into place: ⌘B at a
// link's visible end writes the pair itself now (record 2026-11-12,
// seed19/c2), so the setup the fuzz reached through ⌘B then `(` is the state
// that command leaves behind.
func TestCaretInputInsideBoldWithTypingOff(t *testing.T) {
	d := NewMarkdownDocument("[link](https://example.com)**()**\n")
	tt, _ := richTester(t, d)
	d.selectFromSource(30, 30)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyB)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "with bold off at the caret")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	want := "[link](https://example.com)**(**q**)**\n"
	if source != want || caret != 33 {
		t.Fatalf("typing with bold off gave source %q caret %d, want %q caret 33", source, caret, want)
	}
	assertCaretIsInput(t, d, tt, "after typing with bold off")
}

// TestCaretInputInLinkAcrossSoftBreak pins the case the user-style fuzz
// found: a soft line break inside a link's text, bold turned on for
// typing, and the next rune typed at the start of the second line. The
// rune is written into the link with strong markers around it.
//
// The caret is put inside the label directly. Left from the paragraph's end
// no longer reaches it: Typora leaves that caret at the destination's last
// byte, inside the closing syntax (record 2026-11-12,
// record-20261112-115419/probe102/link-left-type).
func TestCaretInputInLinkAcrossSoftBreak(t *testing.T) {
	d := NewMarkdownDocument("[link](https://example.com)\n")
	tt, _ := richTester(t, d)
	d.selectFromSource(4, 4)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyB)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "with bold on inside the link")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	want := "[lin\n**q**k](https://example.com)\n"
	if source != want {
		t.Fatalf("typing inside the link gave source %q, want %q", source, want)
	}
	if caret != 8 {
		t.Fatalf("typing inside the link gave caret %d, want 8", caret)
	}
	assertCaretIsInput(t, d, tt, "after typing inside the link")
}

// TestCaretInputAfterDeletingSoftBreakLineContent pins the case the
// user-style fuzz found: the line Shift+Enter opens holds its break in the
// view until content comes. Deleting the line's only content must leave the
// empty line at the caret, as Typora does, not a blank line the projection
// reads away.
func TestCaretInputAfterDeletingSoftBreakLineContent(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
		caret           int
	}{
		{"paragraph", "alpha beta\n", "alpha beta\nq\n", 12},
		{"bullet", "- a\n", "- a\n  q\n", 7},
		{"quote", "> a\n", "> a\n> q\n", 7},
		{"quote list", "> - a\n", "> - a\n>   q\n", 11},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument(c.src)
			rt, _ := richTester(t, d)
			rt.Frame()
			rt.Key(ui.Shift, ui.KeyEnter)
			rt.Frame()
			assertCaretIsInput(t, d, rt, "after Shift+Enter")
			rt.Type("?")
			rt.Frame()
			assertCaretIsInput(t, d, rt, "after typing on the new line")
			d.lock()
			materialized := d.md.p.String()
			d.mu.Unlock()
			rt.Key(0, ui.KeyBackspace)
			rt.Frame()
			assertCaretIsInput(t, d, rt, "after deleting the line's content")
			// Undo brings the typed line back; redo puts the empty line,
			// with the break waiting in the view, where the delete left it.
			rt.Key(ui.Cmd, ui.KeyZ)
			rt.Frame()
			assertCaretIsInput(t, d, rt, "after undoing the delete")
			d.lock()
			undone := d.md.p.String()
			d.mu.Unlock()
			if undone != materialized {
				t.Fatalf("undoing the delete gave source %q, want %q", undone, materialized)
			}
			rt.Key(ui.Cmd|ui.Shift, ui.KeyZ)
			rt.Frame()
			assertCaretIsInput(t, d, rt, "after redoing the delete")
			rt.Type("q")
			rt.Frame()
			d.lock()
			source, caret := d.md.p.String(), d.selection.Caret.Offset
			d.mu.Unlock()
			if source != c.want || caret != c.caret {
				t.Fatalf("typing after the delete gave source %q caret %d, want %q caret %d", source, caret, c.want, c.caret)
			}
			assertCaretIsInput(t, d, rt, "after typing after the delete")
		})
	}
}

// TestCaretInputEnterOnTrailingLineOfListItem pins the case the user-style
// fuzz found: Down from the start of a soft-broken item's last line has no
// line below to reach, and Typora leaves the caret there. Enter then splits
// the item at that line: the item above keeps its empty first line, the line
// the caret sits on becomes the next item, and the rune typed lands at its
// start (probe9/task-down-checkpoints).
func TestCaretInputEnterOnTrailingLineOfListItem(t *testing.T) {
	d := NewMarkdownDocument("- [ ] a\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Key(0, ui.KeyDown)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after moving down to the item's empty line")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after Enter on the item's empty line")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	// The document keeps its final line ending after the typed line, as the
	// structural edits do; Typora's snapshot has the same source without it,
	// and its saved file adds it back (probe9/task-down-checkpoints).
	want := "- [ ] \n- [ ] qa\n"
	if source != want || caret != len(want)-2 {
		t.Fatalf("typing after Enter on the split line gave source %q caret %d, want %q caret %d",
			source, caret, want, len(want)-2)
	}
	assertCaretIsInput(t, d, tt, "after typing after the split")
}

// TestCaretInputBackspaceOnEmptyQuoteLine pins the case the user-style fuzz
// found: the caret at the start of the quoted paragraph after an empty quote
// line deletes the quote, and the paragraph it stays at must be where the
// view draws the caret. The quote markers leave every line, so the source
// selection is the caret's line content start, not the start of a paragraph
// the reparse renumbered.
func TestCaretInputBackspaceOnEmptyQuoteLine(t *testing.T) {
	d := NewMarkdownDocument(",\n>d\n>\n>alpha **bold** omega\n")
	tt, _ := richTester(t, d)
	d.SetSourceSelection(sourceCaret(8))
	tt.Frame()
	assertCaretIsInput(t, d, tt, "at the quoted paragraph after the empty quote line")
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after backspace on the empty quote line")
	tt.Type("q")
	tt.Frame()
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	want := ",\nd\n\nqalpha **bold** omega\n"
	if source != want || caret != 6 {
		t.Fatalf("typing after the backspace gave source %q caret %d, want %q caret 6", source, caret, want)
	}
	assertCaretIsInput(t, d, tt, "after typing after the backspace")
}

// TestCaretInputBoldTypingAfterLazyHashLine pins the case the user-style
// fuzz found: a "#" typed on a lazy continuation line stays literal while
// the line is only that, and a bold rune after it turns the line into a
// heading. The source cannot keep the old projection, so the rune is
// written at the source caret and the view follows the reparse.
func TestCaretInputBoldTypingAfterLazyHashLine(t *testing.T) {
	d := NewMarkdownDocument("alpha **bold** omega\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Type("#")
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyB)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "with bold on after the hash")
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after the bold rune")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "alpha **bold** omega\n#**q**\n" || caret != 25 {
		t.Fatalf("typing after the hash gave source %q caret %d, want %q caret 25", source, caret, "alpha **bold** omega\n#**q**\n")
	}
}

// TestCaretInputBlankOnlyDocument pins the case the user-style fuzz found:
// deleting the only rune of a document made of its leading blank lines
// leaves those lines as empty paragraphs, and the caret stays on the last
// one, where the next rune goes. One projected paragraph for the whole run
// draws the caret on a line the rune then leaves.
func TestCaretInputBlankOnlyDocument(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Type("?")
	tt.Frame()
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after deleting the only rune")
	tt.Type("q")
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after typing on the blank line")
	d.lock()
	source, caret := d.md.p.String(), d.selection.Caret.Offset
	d.mu.Unlock()
	if source != "\n\nq" || caret != 3 {
		t.Fatalf("typing on the blank line gave source %q caret %d, want %q caret 3", source, caret, "\n\nq")
	}
}

// TestCaretInputUndoAfterLeavingHeldBreak covers the user-style fuzz case:
// Enter at the document's end opens an empty paragraph, Shift+Enter holds a
// break on it, and Left steps back onto the paragraph's line. Typing a rune
// and undoing it must restore both the source and the drawn caret.
func TestCaretInputUndoAfterLeavingHeldBreak(t *testing.T) {
	d := NewMarkdownDocument("alpha\n")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	assertCaretIsInput(t, d, tt, "after stepping left over the held break")
}
