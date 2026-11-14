package richtext

import (
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// transientMode names the lifecycle state of a deferred view edit. The
// modes are mutually exclusive; atEOF and finalEOL still describe the
// underlying document's tail position.
type transientMode uint8

const (
	transientBlockOpen transientMode = iota
	transientContainerExit
	transientTrailingSplit
	transientReopenedTail
	transientPhantomEOF
	transientDetachedEOF
	transientPendingBreak
	transientEmptiedBreak
)

// pendingBreak is the line break a transient holds at the end of the last
// paragraph: the break's bytes in preview's source, the LineSeparators it
// shows, and the bytes another Shift+Enter adds.
type pendingBreak struct {
	source, shown int
	unit          string
}

// transientBlock is an empty paragraph the view shows after an edit that
// has no source bytes yet, or a line break held at the end of the last
// paragraph. Its canonical cursor is anchor; preview is the virtual
// Markdown document that becomes real on the first content edit.
type transientBlock struct {
	preview *Document
	anchor  SourcePoint
	// write is the source offset the first content goes at. It differs from
	// anchor for an empty paragraph after a block that needs a separator
	// blank line, where the paragraph's caret sits after that line.
	write int
	// para is the transient paragraph in the view's current projection.
	para int
	// atEOF and finalEOL keep the document's final-newline state when the
	// deferred edit is at its end.
	atEOF    bool
	finalEOL bool
	mode     transientMode
	brk      pendingBreak
	// marker spells the transient paragraph in preview's source when blank
	// lines alone cannot: a paragraph between two blocks, or an empty
	// quoted paragraph. The projection shows the paragraph without it.
	marker string
}

func (t *transientBlock) isLineBreak() bool {
	return t.mode == transientPendingBreak || t.mode == transientEmptiedBreak
}

func (t *transientBlock) isEmptyParagraph() bool {
	return !t.isLineBreak()
}

func (t *transientBlock) isPhantomEOF() bool {
	return t.mode == transientPhantomEOF || t.mode == transientDetachedEOF
}

func (t *transientBlock) isViewOnlyLine() bool {
	return t.isPhantomEOF() || t.isTrailingSplit()
}

func (t *transientBlock) isTrailingSplit() bool {
	return t.mode == transientTrailingSplit
}

func (t *transientBlock) writesBytesOnBlur() bool {
	return t.visibleInSnapshot() && !t.isViewOnlyLine()
}

func (t *transientBlock) visibleInSnapshot() bool {
	return t.mode != transientDetachedEOF
}

func (t *transientBlock) preservesOnShiftLeft() bool {
	return t.mode == transientReopenedTail
}

func (t *transientBlock) setContainerExit(exit bool) {
	if exit {
		t.mode = transientContainerExit
	} else if t.mode == transientContainerExit {
		t.mode = transientBlockOpen
	}
}

// transientUndo recreates the preview a materialization replaced. Undo
// puts the source bytes back, then this patch and selection rebuild the
// empty paragraph without keeping a whole Document in the undo stack.
type transientUndo struct {
	anchor          SourcePoint
	write           int
	atEOF, finalEOL bool
	mode            transientMode
	brk             pendingBreak
	marker          string
	session         sessionState
	patch           sourcePatch
	selection       SourceSelection
}

func (t *transientBlock) undo(base string) *transientUndo {
	src := t.preview.Source()
	from, to, newEnd := changedBytes(base, src)
	return &transientUndo{
		anchor:    t.anchor,
		write:     t.write,
		atEOF:     t.atEOF,
		finalEOL:  t.finalEOL,
		mode:      t.mode,
		brk:       t.brk,
		marker:    t.marker,
		session:   t.preview.state(),
		patch:     sourcePatch{at: from, old: base[from:to], new: src[from:newEnd]},
		selection: t.preview.SourceSelection(),
	}
}

func (u *transientUndo) block(base string, current sessionState) *transientBlock {
	p := u.patch
	if p.at < 0 || p.at+len(p.old) > len(base) || base[p.at:p.at+len(p.old)] != p.old {
		return nil
	}
	src := base[:p.at] + p.new + base[p.at+len(p.old):]
	state := u.session.clone()
	state.openedLists = mapOpenedLists(append([]int(nil), current.openedLists...), p.at, p.at+len(p.old), len(p.new))
	state.openedNested = append([]int(nil), current.openedNested...)
	preview := newExtMarkdownDocument(src, state, previewExtensions)
	preview.md.keepFinalEOLCaret = true
	preview.md.preview = true
	preview.SetSourceSelection(u.selection)
	return &transientBlock{
		preview:  preview,
		anchor:   u.anchor,
		write:    u.write,
		atEOF:    u.atEOF,
		finalEOL: u.finalEOL,
		mode:     u.mode,
		brk:      u.brk,
		marker:   u.marker,
	}
}

// seed24UndoMaterialization reports whether Undo has to restore the explicit
// empty list item a held paragraph stands for. The second Enter that exits a
// container, or a Delete that takes the preview's final EOL away, leaves the
// held paragraph behind the last undo step: Typora keeps the item in its live
// bytes, while the ordinary undo path drops the held paragraph instead
// (record-20261114-153934 / 154454).
func seed24UndoMaterialization(base string, t *transientBlock) bool {
	if t == nil || !t.atEOF || !t.finalEOL || t.marker != "" || !t.isEmptyParagraph() ||
		base == "" || !strings.HasSuffix(base, defaultEOL(base)) {
		return false
	}
	line := lineAt(base, finalEOLStart(base))
	last := base[line.start:line.end]
	if len(last) <= 2 || !strings.ContainsRune("-*+", rune(last[0])) || last[1] != ' ' {
		return false
	}
	return t.mode == transientContainerExit ||
		t.mode == transientBlockOpen && !strings.HasSuffix(t.preview.Source(), defaultEOL(base))
}

// openTransient previews intent at source, without changing the document.
// It reports whether the intent made a transient paragraph or a pending
// line break.
func (v *view) openTransient(intent EditIntent, source int, atEOF bool) bool {
	if v.opts.ReadOnly || v.doc.md == nil || v.transient != nil {
		return false
	}
	base := v.doc.Source()
	preview := v.previewDocument(base)
	preview.SetSourceSelection(sourceCaret(source))
	if intent == IntentShiftEnter {
		if unit, full, ok := v.pendingHardBreak(source); ok {
			// The block writes hard breaks with its own marker: the
			// structural edit writes the marker at once, while its newline
			// is the document's final EOL. Hold the break over the bytes the
			// first input writes at the paragraph's end.
			preview = v.previewDocumentInserted(base[:source]+unit+base[source:], source, len(unit))
			markLiveBreak(preview, source, unit)
			preview.reproject()
			preview.SetSourceSelection(sourceCaret(source + len(unit)))
			t := &transientBlock{
				preview:  preview,
				anchor:   sourcePoint(source, BoundaryDownstream),
				write:    source + len(unit),
				atEOF:    atEOF,
				finalEOL: strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r"),
				mode:     transientPendingBreak,
				brk:      pendingBreak{source: len(unit), shown: 1, unit: full},
			}
			v.showTransient(t)
			return true
		}
	}
	if !applyTransientIntent(preview, intent) {
		return false
	}
	t := &transientBlock{
		preview:  preview,
		anchor:   sourcePoint(source, BoundaryDownstream),
		atEOF:    atEOF,
		finalEOL: strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r"),
	}
	if intent == IntentShiftEnter {
		// The preview holds the line break Shift+Enter writes; the
		// canonical bytes still end at the paragraph. The break is the
		// span the intent inserted at source.
		src := preview.Source()
		caret := preview.SourceSelection().Caret.Offset
		if source < 0 || source > len(base) || caret <= source || caret > len(src) ||
			!strings.HasPrefix(src, base[:source]) || src[caret:] != base[source:] {
			return false
		}
		breaks := countLineBreaks(src[source:caret])
		if breaks == 0 {
			return false
		}
		markLiveBreak(preview, source, src[source:caret])
		preview.reproject()
		t.mode = transientPendingBreak
		t.brk = pendingBreak{source: caret - source, shown: breaks, unit: src[source:caret]}
	}
	t.write = preview.SourceSelection().Caret.Offset
	if t.isEmptyParagraph() && atEOF {
		if anchor, write, ok := transientEndTarget(preview, t.write); ok {
			t.anchor = sourcePoint(anchor, BoundaryDownstream)
			t.write = write
			preview.SetSourceSelection(sourceCaret(anchor))
		}
	}
	v.showTransient(t)
	return true
}

// markLiveBreak keeps the break intent inserted at base+at as one the
// session typed. unit holds the inserted bytes; the break the paragraph
// starts on is the first line ending in it, after any hard break marker.
func markLiveBreak(d *Document, at int, unit string) {
	if i := strings.IndexAny(unit, "\r\n"); i >= 0 {
		d.setState(d.state().setLiveLine(at + i))
	}
}

// replaceEmptiedLineBreak turns a deletion that takes away all the content
// of a paragraph's last line into the pending line break the line was typed
// into: the line's bytes leave the source and the break waits in the view,
// as after Shift+Enter. A soft break whose line is empty ends the paragraph,
// so the bytes alone would read the line away and leave the caret nowhere;
// Typora keeps the line in the view, and so does the pending break. It
// reports whether it made the edit.
func (v *view) replaceEmptiedLineBreak(start, end int) bool {
	if v.transient != nil || v.opts.ReadOnly || v.doc.md == nil || v.source.Len() == 0 {
		return false
	}
	p := v.shown.paraAt(start)
	if p < 0 || end <= start || p != v.shown.paraAt(end-1) {
		return false
	}
	ps := v.shown.paraStart(p)
	pe := ps + v.para(p).n
	if start >= end || end != pe {
		return false
	}
	var text []rune
	for _, r := range v.shown.paragraph(p).Runs {
		text = append(text, []rune(r.Text)...)
	}
	sep := -1
	for i := len(text) - 1; i >= 0; i-- {
		if text[i] == LineSeparator {
			sep = i
			break
		}
	}
	if sep < 0 || start != ps+sep+1 {
		return false
	}
	srcStart, srcEnd, segs := v.source.Paragraph(p)
	at, atSrc := 0, srcStart
	breakSrc, lineSrc := -1, -1
	for i, s := range segs {
		if s.Shown > 0 && at <= sep && sep < at+s.Shown {
			if s.Kind != segmentBreak || s.Shown != 1 {
				return false
			}
			breakSrc = atSrc
			lineSrc = atSrc + s.Source
			// The line holds no marker the deletion leaves behind: its
			// text is its bytes.
			for _, tail := range segs[i+1:] {
				if tail.Kind != SegmentVerbatim {
					return false
				}
			}
			break
		}
		at += s.Shown
		atSrc += s.Source
	}
	if breakSrc < 0 || lineSrc > srcEnd {
		return false
	}
	for _, b := range v.source.boxes(p) {
		// A hard break's marker sits before its line break and would be
		// read away with it. The marker this session wrote to hold a blank
		// line is the exception: deleting the content under it takes the
		// marker with the emptied line's bytes and puts the line back into
		// the pending break it came from (record 2026-11-11, probe63/held-
		// break-delete-content).
		if b.kind.hardBreak() && b.docStart == sep && !v.doc.heldBreakAt(b.start) {
			return false
		}
	}
	from, to, insert, _, ok := v.directSourceEdit(start, end, "", false)
	if !ok || insert != "" || from != lineSrc || to != srcEnd {
		return false
	}
	d := v.doc
	d.lock()
	v.lockedPublish()
	undoLen := len(d.undo)
	d.mu.Unlock()
	// The deletion is an action of its own, not a rune of the typing before
	// it: undoing it brings the line back instead of joining the typing
	// step the line's content was typed in.
	v.mode, v.coalesce = newStep, false
	err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(breakSrc, srcEnd, "")
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(breakSrc)
	})
	if err != nil {
		return false
	}
	d.lock()
	stepped := len(d.undo) > undoLen
	d.mu.Unlock()
	if !stepped {
		return false
	}
	v.setSourceSelectionLocked(sourceCaret(breakSrc))
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	atEOF := p == v.shown.ParagraphCount()-1
	if v.openTransient(IntentShiftEnter, breakSrc, atEOF) {
		if t := v.transient; t != nil {
			t.mode = transientEmptiedBreak
			d.lock()
			if n := len(d.undo); n > 0 {
				d.undo[n-1].afterTransient = t.undo(d.md.p.String())
			}
			d.mu.Unlock()
		}
	}
	return true
}

// replaceEmptiedSentinelLine takes the deletion that empties a last line
// holding only the tab Tab left behind its invisible U+200B. Typora writes
// the sentinel with the tab as one pair, so Backspace on the tab removes
// both: the line's bytes and the blank line that opened them leave, and the
// paragraph waits at the document's end in the view, as the empty paragraph
// Enter opens there (record 2026-11-12, seed13/c11). It reports whether it
// made the edit.
func (v *view) replaceEmptiedSentinelLine(start, end int) bool {
	if start >= end {
		return false
	}
	p := v.shown.paraAt(start)
	if p != v.shown.paraAt(end-1) {
		return false
	}
	para, at, ok := v.sentinelTabPair()
	if !ok || para != p {
		return false
	}
	return v.withdrawSentinelTabLine(at)
}

// sentinelLinePrefix reports the source offset of the invisible U+200B Tab
// writes when it keeps a tab as text at the start of a line that cannot
// indent. The byte is content only for the tab alone on a line: typing in
// front of it leaves a line whose tab is text among other text, and Typora
// writes such a line without the byte, so the insertion that makes it takes
// the byte with it (record 2026-11-13, seed22/c3; the bare pair's deletion
// paths are seed13/c11, seed19/c4 and seed21/c1). It reports ok when the
// caret opens the paragraph whose line starts with the pair and the line
// holds text after it.
func (v *view) sentinelLinePrefix() (at int, ok bool) {
	if v.transient != nil || v.opts.ReadOnly || v.doc.md == nil || v.shown.Len() == 0 {
		return 0, false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.source.Len() || v.shown.paraStart(p) != v.caret {
		return 0, false
	}
	if st := v.para(p).style; st.CodeBlock || st.Quote {
		return 0, false
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	content := d.sourceAtParagraph(p, 0)
	d.mu.Unlock()
	line := lineAt(src, content)
	if content != line.start {
		// A block prefix in front of the pair leaves the line the block's
		// rather than the pair's own, and no record covers typing there.
		return 0, false
	}
	body := src[line.start:line.end]
	if !strings.HasPrefix(body, "\u200b\t") || strings.TrimSpace(body[len("\u200b\t"):]) == "" {
		// The bare pair is the line's only content, so nothing says the tab
		// stays text among text; its own deletion paths own that shape.
		return 0, false
	}
	return content, true
}

// sentinelTabPair reports the document's last paragraph when the whole line
// shows only the pair Tab wrote at the start of a line that cannot indent:
// the invisible U+200B that keeps the tab text and the tab itself. at is
// where the bytes the pair's line contributes begin: the pair itself when the
// line holds the prefix of its block (`> ` of a quote, `- ` of an item), and
// otherwise the line's start above the pair, since the blank line there is
// what spells the paragraph. Any other text on the line is content, which the
// paragraph keeps, and a paragraph above the line is needed for the view to
// keep the caret once the line leaves.
func (v *view) sentinelTabPair() (para, at int, ok bool) {
	if v.transient != nil || v.opts.ReadOnly || v.doc.md == nil || v.shown.Len() == 0 {
		return 0, 0, false
	}
	p := v.shown.paraAt(v.shown.Len() - 1)
	ps := v.shown.paraStart(p)
	if p != v.shown.ParagraphCount()-1 || ps+v.para(p).n != v.shown.Len() ||
		!strings.HasPrefix(v.shown.Slice(ps, v.shown.Len()).String(), "\u200b\t") {
		return 0, 0, false
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	content := d.sourceAtParagraph(p, 0)
	eol := defaultEOL(src)
	prefix := d.paragraphLocation(p).prefix
	d.mu.Unlock()
	if content > len(src) || !strings.HasPrefix(src[content:], "\u200b\t") {
		return 0, 0, false
	}
	if prefix != "" {
		// The pair stands on the paragraph's own line after the bytes its
		// block writes there: those bytes keep the line a block of the
		// document, so only the pair leaves and at stops before the prefix
		// (record 2026-11-13, seed21/c1).
		if content < len(prefix) || src[content-len(prefix):content] != prefix {
			return 0, 0, false
		}
		return p, content, true
	}
	// at is where the pair's line begins: a paragraph above the line spells
	// it with the ending that precedes the pair, and a document whose only
	// line holds the pair begins at the document's start.
	at = content - len(eol)
	if p == 0 {
		at = 0
	}
	if at < 0 || at > content || src[at:content] != "" && src[at:content] != eol {
		return 0, 0, false
	}
	return p, at, true
}

// withdrawSentinelTabLine takes the sentinel pair away with the line that
// spells it: the pair's bytes and the blank line above them leave, and the
// paragraph waits at the document's end in the view, as the empty paragraph
// Enter opens there. A line that keeps the prefix of its block stands in the
// document on those bytes, so the pair leaves alone and no paragraph is held
// (record 2026-11-13, seed21/c1). Backspace on the tab and Shift+Tab on the
// pair both undo the one key that wrote it (record 2026-11-12, seed13/c11 and
// seed19/c4). It reports whether it made the edit.
func (v *view) withdrawSentinelTabLine(at int) bool {
	d := v.doc
	d.lock()
	src := d.md.p.String()
	undoLen := len(d.undo)
	atEOF := strings.HasSuffix(src[:at], "\n") || strings.HasSuffix(src[:at], "\r")
	d.mu.Unlock()
	pair := "\u200b\t"
	eol := defaultEOL(src)
	pairStart := at
	if !strings.HasPrefix(src[pairStart:], pair) && strings.HasPrefix(src[pairStart:], eol) &&
		strings.HasPrefix(src[pairStart+len(eol):], pair) {
		pairStart += len(eol)
	}
	tail := ""
	if line := lineAt(src, pairStart); line.start == pairStart &&
		strings.HasPrefix(src[pairStart:], pair) && src[pairStart:line.end] != pair {
		tail = src[pairStart+len(pair):]
	}
	if tail != "" && seed24SentinelTypedBackspace(v, pairStart, v.caret, tail) {
		return v.seed24WithdrawTypedRune(pairStart, tail, eol)
	}
	from, to, replace := at, len(src), ""
	caret := at
	if tail == "" {
		// The bare pair and the line that spells it leave together.
	} else {
		from, replace, caret = pairStart, strings.TrimSuffix(tail, eol), pairStart
	}
	v.mode, v.coalesce = newStep, false
	if err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(from, to, replace)
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(caret)
	}); err != nil {
		return false
	}
	d.lock()
	stepped := len(d.undo) > undoLen
	d.mu.Unlock()
	if !stepped {
		return false
	}
	v.setSourceSelectionLocked(sourceCaret(caret))
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	held := false
	switch {
	case at == 0:
		// No byte is left above the pair: the document's only paragraph is the
		// one the caret returns to, and the view holds it as the paragraph an
		// emptied block exits to (record 2026-11-12, seed16/c14).
		held = v.openEmptyExitTransient(emptyLift{anchor: at, atEOF: true}, atEOF)
	case atEOF && tail == "":
		// The bytes left end in the line break that opened the pair's line, so
		// the paragraph the caret returns to stands below that break: the view
		// holds it as it holds the one Enter opens at the document's end
		// (record 2026-11-12, seed19/c4).
		held = v.openTransient(IntentEnter, len(v.doc.Source()), atEOF)
	}
	if held {
		if t := v.transient; t != nil {
			d.lock()
			if n := len(d.undo); n > 0 {
				d.undo[n-1].afterTransient = t.undo(d.md.p.String())
			}
			d.mu.Unlock()
		}
	}
	return true
}

// seed24SentinelTypedBackspace distinguishes the Backspace that takes back a
// rune typed after the sentinel pair from Shift+Tab's withdrawal of the pair.
// The key that wrote the pair and that first rune share one step of undo
// (records 2026-11-14-153934 / 154454, seed24/c7).
func seed24SentinelTypedBackspace(v *view, pairStart, caret int, tail string) bool {
	if v.transient != nil || v.doc.md == nil || v.selAnchor != v.caret {
		return false
	}
	if caret <= v.shown.paraStart(v.shown.paraAt(caret)) {
		return false
	}
	d := v.doc
	d.lock()
	defer d.mu.Unlock()
	src := d.md.p.String()
	if len(d.undo) == 0 {
		return false
	}
	step := d.undo[len(d.undo)-1]
	if len(step.patches) < 2 || step.selAfter.Caret.Offset != d.selection.Caret.Offset {
		return false
	}
	eol := defaultEOL(src)
	typed := step.patches[len(step.patches)-1]
	paired := false
	for _, patch := range step.patches[:len(step.patches)-1] {
		if patch.old == "" && strings.HasSuffix(patch.new, "\u200b\t"+eol) {
			paired = true
			break
		}
	}
	return paired && typed.old == "" && typed.new == strings.TrimSuffix(tail, eol) &&
		typed.at == pairStart+len("\u200b\t") && strings.HasSuffix(tail, eol)
}

// seed24WithdrawTypedRune leaves the bare sentinel pair and its final line
// ending in the source, moving the caret back to just after the tab.
func (v *view) seed24WithdrawTypedRune(pairStart int, tail, eol string) bool {
	pair := "\u200b\t"
	from, to := pairStart+len(pair), pairStart+len(pair)+len(strings.TrimSuffix(tail, eol))
	caret := pairStart + len(pair)
	v.mode, v.coalesce = newStep, false
	if err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(from, to, "")
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(caret)
	}); err != nil {
		return false
	}
	v.setSourceSelectionLocked(sourceCaret(caret))
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	return true
}

// replaceEmptiedTailParagraph holds the document's last paragraph in the
// view again when a deletion takes away the content that materialized it.
// Typora's live model keeps the document's last paragraph as a block the
// session holds: the bytes that spelled it leave with its content, and the
// paragraph waits at the document's end until its first content writes its
// line again. Two spellings share the rule. A document of blank lines
// spells an empty paragraph with a line ending of its own, so that ending
// leaves with the content and a third paragraph stands in the view (record
// 2026-11-12, seed12/c2). A paragraph under the document's last block is
// spelled by the blank line that separates the two, so that blank line
// leaves and the paragraph is held after the block (record 2026-11-12,
// seed14/c12). It reports whether it made the edit.
func (v *view) replaceEmptiedTailParagraph(start, end int) bool {
	if v.transient != nil || v.opts.ReadOnly || v.doc.md == nil || start >= end {
		return false
	}
	p := v.shown.paraAt(start)
	// The emptied paragraph is the document's last, and paragraphs stand above
	// it to keep the caret: a document whose only paragraph is emptied has no
	// line for the held paragraph to stand on.
	if p < 1 || p != v.shown.ParagraphCount()-1 || p != v.shown.paraAt(end-1) {
		return false
	}
	if st := v.para(p).style; st.List != NoList || st.Quote || st.Heading > 0 || st.CodeBlock {
		// An item, quote or heading spells its block with markers of its own:
		// emptied, those bytes keep it a block of the document, and the view
		// holds no paragraph in its place.
		return false
	}
	n := v.para(p).n
	if n == 0 || start != v.shown.paraStart(p) || end != start+n {
		return false
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	eol := defaultEOL(src)
	content := d.sourceAtParagraph(p, 0)
	d.mu.Unlock()
	if content <= 0 || content > len(src) {
		return false
	}
	if line := lineAt(src, content); line.stop != len(src) {
		// The content is the document's last line: bytes after it are blank
		// lines the deletion does not own.
		return false
	}
	// from is where the bytes the paragraph added to the document begin: the
	// blank line over its content, which a paragraph under a block always has.
	from := content - len(eol)
	if from < 0 || !blankText(src[from:content]) {
		// Only the one blank line separates the paragraph's content from the
		// source the deletion keeps.
		return false
	}
	blankDoc := strings.TrimSpace(src[:from]) == ""
	var preview string
	if blankDoc {
		// A document of blank lines spells an empty paragraph with a line
		// ending of its own, so the emptied paragraph's ending leaves with its
		// content and the blank lines above stay one per paragraph. One blank
		// line is the document's own paragraph: emptied, it keeps its line.
		if p < 2 {
			return false
		}
		from = p * len(eol)
		preview = strings.Repeat(eol, 2*p)
	} else {
		// Above a block the paragraph is spelled by the single blank line that
		// separates the two. A longer run of blank lines is the document's
		// own: emptied, the paragraph keeps that run (probe90/mid-empty-alt-
		// delete, TestCaretInputBlankOnlyDocument).
		preceding := v.paragraphEndSource(p - 1)
		if preceding < 0 || preceding > from || !blankText(src[preceding:from]) ||
			countLineBreaks(src[preceding:from]) != 1 {
			return false
		}
		from = preceding + len(eol)
		preview = src[:from] + eol
	}
	anchor := max(0, from-len(eol))
	d.lock()
	undoLen := len(d.undo)
	d.mu.Unlock()
	v.mode, v.coalesce = newStep, false
	err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(from, len(src), "")
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(from)
	})
	if err != nil {
		return false
	}
	d.lock()
	stepped := len(d.undo) > undoLen
	d.mu.Unlock()
	if !stepped {
		return false
	}
	v.setSourceSelectionLocked(sourceCaret(anchor))
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	// The held paragraph's first content goes where the preview's paragraph
	// starts, which a blank run the paragraph keeps can put before the
	// preview's end: bytes written past that line open another paragraph, and
	// the caret drawn in the held one lands there (seed14/c12 after a fence).
	previewDoc := v.previewDocument(preview)
	write := len(preview)
	previewDoc.lock()
	if n := previewDoc.md.m.Len(); n > 0 {
		if start := previewDoc.md.m.start(n - 1); start >= 0 && start <= len(preview) {
			write = start
		}
	}
	previewDoc.mu.Unlock()
	t := &transientBlock{
		preview:  previewDoc,
		anchor:   sourcePoint(anchor, BoundaryDownstream),
		write:    write,
		atEOF:    true,
		finalEOL: true,
	}
	t.preview.SetSourceSelection(sourceCaret(t.write))
	t.para = t.preview.Text().ParagraphCount() - 1
	v.showTransient(t)
	d.lock()
	if n := len(d.undo); n > 0 {
		d.undo[n-1].afterTransient = t.undo(d.md.p.String())
	}
	d.mu.Unlock()
	return true
}

// blankText reports whether s holds line endings and whitespace only.
func blankText(s string) bool {
	return strings.Trim(s, " \t\r\n") == ""
}

// deleteInHeldTailParagraph reports whether Delete acts in the empty
// paragraph the view holds at the document's end, where it has nothing to
// delete: the paragraph owns no bytes, and no block follows the line it is
// drawn on. Typora keeps the held paragraph and the bytes as they are
// (record 2026-11-13, seed17/c16).
func (v *view) deleteInHeldTailParagraph() bool {
	t := v.transient
	if t == nil || t.isLineBreak() || !t.atEOF || t.isViewOnlyLine() ||
		t.mode == transientContainerExit || t.marker != "" {
		return false
	}
	if v.selAnchor != v.caret || v.shown.paraAt(v.caret) != t.para {
		return false
	}
	return v.caret == v.shown.paraStart(t.para)
}

// backspaceHeldTailBreak returns the document's last line to the empty
// paragraph the view held before Shift+Enter opened a break in it. The break
// is all that paragraph holds, so Backspace deletes it: the line ending the
// break was opened over leaves the source and the paragraph waits in the view
// where it stood, as it did before the Shift+Enter (record 2026-11-12,
// seed13/c9). A paragraph that holds text keeps that text and the ordinary
// deletion path takes the break alone.
func (v *view) backspaceHeldTailBreak() bool {
	t := v.transient
	if t == nil || v.opts.ReadOnly || v.doc.md == nil || t.isEmptyParagraph() ||
		!t.atEOF || t.marker != "" || t.mode == transientContainerExit || t.isViewOnlyLine() {
		return false
	}
	if v.selAnchor != v.caret {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p != v.shown.ParagraphCount()-1 || v.caret != v.shown.paraStart(p)+v.para(p).n {
		return false
	}
	if strings.Trim(plainOf(v.shown.paragraph(p)), string(LineSeparator)) != "" {
		return false
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	eol := defaultEOL(src)
	at := t.anchor.Offset
	d.mu.Unlock()
	// The break's anchor is the blank line the empty paragraph was written
	// with: the last line ending of the source, and the only byte the
	// paragraph added to it.
	if !strings.HasSuffix(src, eol) || at != len(src)-len(eol) {
		return false
	}
	d.lock()
	undo := t.undo(src)
	d.mu.Unlock()
	// The deletion is an action of its own: undoing it puts the break back and
	// redoing it holds the emptied paragraph again.
	v.mode, v.coalesce = newStep, false
	err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(at, len(src), "")
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(at)
	})
	if err != nil {
		return false
	}
	last := v.shown.ParagraphCount() - 1
	if last < 0 || !v.openTransient(IntentEnter, v.paragraphEndSource(last), true) {
		return true
	}
	next := v.transient
	d.lock()
	if n := len(d.undo); n > 0 && undo != nil && next != nil {
		d.undo[n-1].transient = undo
		d.undo[n-1].afterTransient = next.undo(d.md.p.String())
	}
	d.mu.Unlock()
	return true
}

// openEmptyExitTransient shows the paragraph an empty container exits to
// after the lift wrote the exit as its own step of undo. The paragraph has
// no source bytes until its first content. Between two blocks, blank lines
// alone cannot spell it, so a marker in the preview does and the
// projection leaves the marker out.
func (v *view) openEmptyExitTransient(lift emptyLift, finalEOL bool) bool {
	if v.opts.ReadOnly || v.doc.md == nil || v.transient != nil {
		return false
	}
	base := v.doc.Source()
	t := &transientBlock{
		anchor:   sourcePoint(lift.anchor, BoundaryDownstream),
		write:    lift.anchor,
		atEOF:    lift.atEOF,
		finalEOL: finalEOL,
		mode:     transientContainerExit,
	}
	if lift.atEOF && lift.quote == "" {
		// The paragraph the edit exits to is the document's last line. The
		// lift left the blank line that already separated it when there was
		// one; otherwise one ending after what remains opens it, and an
		// empty document's only paragraph needs none.
		if content := trailingLineWhitespace(base); content != "" {
			// The lift kept the whitespace content of the item it emptied
			// (record 2026-11-09, seed3/c12): Typora's live model shows it
			// as the paragraph the exit opened, with the caret before it.
			// A marker after the content keeps the line a paragraph for
			// the preview's parser; the projection hides the marker, and
			// the first input lands on the content's first byte.
			at := len(base) - len(content)
			marker := transientMarker(base)
			preview := v.previewDocument(base + marker)
			preview.SetSourceSelection(sourceCaret(at))
			preview.md.keepFinalEOLCaret = true
			t.anchor = sourcePoint(at, BoundaryDownstream)
			t.write = at
			t.marker = marker
			t.preview = preview
			v.showTransient(t)
			return true
		}
		preview := v.previewDocument(base)
		anchor, write := 0, 0
		if base != "" {
			var ok bool
			var quoteTail bool
			tail := 0
			preview.lock()
			last := preview.text.ParagraphCount() - 1
			container := false
			if last >= 0 {
				st := preview.text.paragraph(last).Style
				container = st.List != NoList || st.Quote
				quoteTail = st.Quote && st.List == NoList
			}
			if container {
				// The preview's last paragraph is still the item or quote
				// line the lift emptied; the paragraph the exit opened
				// follows it, at that block's end (record 2026-11-09,
				// seed3/c22). A quote keeps the blank quote lines below its
				// last paragraph, and the exit paragraph follows those too:
				// its bytes start past the whole quote (record 2026-11-13,
				// zz177/A-empty-line-enter).
				anchor = preview.mappingViewLocked().paragraphEndSource(last)
			}
			if quoteTail {
				tail = preview.quoteTailEnd(anchor)
			}
			preview.mu.Unlock()
			if quoteTail {
				// The quote's own blank lines end the bytes: the paragraph the
				// exit opens stands outside the quote, below that whole tail,
				// and a marker spells it so the preview's parser reads it as a
				// paragraph of its own. The projection hides the marker, and
				// the next Enter materializes the paragraph (record
				// 2026-11-13, zz177/A-empty-line-enter).
				eol := defaultEOL(base)
				marker := transientMarker(base)
				// The paragraph needs a blank line above it: the line the lift
				// left ends the quote tail, and a marker on it would read back
				// as another line of the quote.
				insert := eol + marker
				if tail > 0 && !strings.HasSuffix(base[:tail], eol) {
					insert = eol + insert
				}
				preview = v.previewDocumentInserted(base[:tail]+insert+base[tail:], tail, len(insert))
				if preview.Source() != base[:tail]+insert+base[tail:] {
					return false
				}
				t.anchor = sourcePoint(tail+len(insert)-len(marker), BoundaryDownstream)
				t.write = tail + len(insert) - len(marker)
				t.marker = marker
				t.preview = preview
				preview.SetSourceSelection(sourceCaret(t.write))
				v.showTransient(t)
				return true
			}
			if container {
				// The lift emptied the document's last line, so the exit
				// paragraph has no line of its own in the bytes yet. The
				// preview holds one after the item's line, as it does for
				// the paragraph Enter opens at a text line's end: the next
				// Enter materializes it and holds the paragraph below it
				// (record 2026-11-11, seed10/c1).
				eol := defaultEOL(base)
				preview = v.previewDocument(base + eol)
				write, ok = len(base)+len(eol), true
			} else {
				anchor, write, ok = transientEndTarget(preview, len(base))
			}
			if !ok {
				eol := defaultEOL(base)
				preview = v.previewDocument(base + eol)
				anchor, write, ok = transientEndTarget(preview, len(base)+len(eol))
			}
			if !ok {
				return false
			}
		}
		t.anchor = sourcePoint(anchor, BoundaryDownstream)
		t.write = write
		preview.SetSourceSelection(sourceCaret(anchor))
		preview.md.keepFinalEOLCaret = true
		t.preview = preview
		v.showTransient(t)
		return true
	}

	quote := lift.quote
	blank := ""
	if i := strings.LastIndex(quote, ">"); i >= 0 {
		blank = quote[:i+1]
	}
	eol := defaultEOL(base)
	marker := transientMarker(base)
	var insert strings.Builder
	insert.WriteString(quote)
	insert.WriteString(marker)
	insert.WriteString(eol)
	if !blankFirstMarkdownLine(base[lift.anchor:], blank) {
		insert.WriteString(blank)
		insert.WriteString(eol)
	}
	preview := v.previewDocumentInserted(base[:lift.anchor]+insert.String()+base[lift.anchor:], lift.anchor, insert.Len())
	t.write = lift.anchor + len(quote)
	preview.SetSourceSelection(sourceCaret(t.write))
	t.preview = preview
	t.marker = marker
	v.showTransient(t)
	return true
}

// backspaceHeldQuoteBreak takes Backspace on the quote continuation break
// the view holds at the end of the document. Enter inside a quote opened the
// break's line; the line holds no bytes yet, so the press deletes the break
// instead of writing it, and the bytes end on the content above: `> a\n> b\n`
// with `b` deleted leaves `> a` (record 2026-11-13, zz177/bs-multiline).
func (v *view) backspaceHeldQuoteBreak() bool {
	t := v.transient
	if t == nil || t.isEmptyParagraph() || !t.atEOF || t.marker != "" ||
		t.mode == transientContainerExit || t.isViewOnlyLine() {
		return false
	}
	if !quoteContinuation(t.brk.unit) {
		// A plain line ending is the held tail paragraph's break, which
		// backspaceHeldTailBreak takes.
		return false
	}
	if v.selAnchor != v.caret {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p != v.shown.ParagraphCount()-1 || v.caret != v.shown.paraStart(p)+v.para(p).n {
		return false
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	at := t.anchor.Offset
	d.mu.Unlock()
	if at <= 0 || at >= len(src) {
		return false
	}
	v.mode, v.coalesce = newStep, false
	v.discardTransient()
	err := v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(at, len(src), "")
		if e.Err() != nil {
			return
		}
		e.selectMarkdownSource(at)
	})
	return err == nil
}

// quoteContinuation reports whether a pending break's bytes open a line that
// continues a quote: the text after the break's first line ending is a quote
// marker with nothing after it.
func quoteContinuation(unit string) bool {
	i := strings.IndexAny(unit, "\r\n")
	if i < 0 {
		return false
	}
	prefix := unit[i:]
	prefix = strings.TrimLeft(prefix, "\r\n")
	prefix = strings.TrimSpace(prefix)
	return prefix != "" && strings.Trim(prefix, ">") == ""
}

// trailingLineWhitespace returns the spaces and tabs that end src as a line
// of their own, or "" when the last line holds anything else.
func trailingLineWhitespace(src string) string {
	at := len(src)
	for at > 0 && (src[at-1] == ' ' || src[at-1] == '\t') {
		at--
	}
	if at == len(src) || at == 0 {
		return ""
	}
	if src[at-1] != '\n' && src[at-1] != '\r' {
		return ""
	}
	return src[at:]
}

// blankFirstMarkdownLine reports whether s starts with a blank line or an
// empty quote line: the transient paragraph needs no separator before it.
func blankFirstMarkdownLine(s, blank string) bool {
	if s == "" {
		return true
	}
	line := s
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if blank != "" {
		line = strings.TrimPrefix(line, blank)
	}
	return strings.TrimSpace(line) == ""
}

// transientEndTarget locates an empty paragraph the preview shows at the
// document's end. Its anchor is the previous block's content end. Its first
// content goes at caret, or after a separator blank line when the previous
// line holds the previous paragraph's text: without one, Markdown would join
// the paragraph to that block. A line that holds only the paragraph's own
// item or quote marker is not text before it, and the bytes after the
// paragraph are blank lines, so the content goes where the caret is.
func transientEndTarget(preview *Document, caret int) (anchor, write int, ok bool) {
	preview.lock()
	defer preview.mu.Unlock()
	if preview.md == nil {
		return 0, 0, false
	}
	m := preview.md.m
	if m.Len() == 0 {
		return 0, 0, false
	}
	p := m.Len() - 1
	// The empty paragraph is the preview's last when that paragraph shows
	// nothing: a map holding only blank lines the projection hides as gaps
	// is as empty as one with no source at all.
	if paragraphRunes(preview.text.paragraph(p)) != 0 {
		return 0, 0, false
	}
	start := m.start(p)
	write = caret
	if p > 0 {
		anchor = preview.mappingViewLocked().paragraphEndSource(p - 1)
	}
	// The preview's caret can stand past blank lines the empty paragraph
	// does not own; when it is beyond the paragraph's own bytes, its first
	// content goes where the paragraph's input goes, after its gap bytes.
	// A caret at the paragraph's end is already where the content goes.
	if pm := seqAt(m.paras, p); pm.src != 0 && caret > m.end(p) {
		if w := preview.sourceAtParagraph(p, 0); w >= 0 {
			write = w
		}
	}
	// A fenced code block's last line is its closing fence, which the
	// source map holds before the empty paragraph rather than in the
	// preceding paragraph's segments.
	root := preview.md.p.Block(0)
	for i := 1; i < root.Next; i++ {
		if b := preview.md.p.Block(i); b.Parent == 0 && b.Kind == parser.CodeBlock && b.End > anchor {
			anchor = b.End
		}
	}
	src := preview.md.p.String()
	if start > 0 {
		line := lineAt(src, start-1)
		// The line before the paragraph's start is the paragraph's own
		// marker line when the paragraph is an item's or a quote's empty
		// content; the previous paragraph ends before it, on another line.
		if strings.TrimSpace(src[line.start:line.end]) != "" &&
			anchor >= line.start && anchor <= line.end {
			write = len(src)
		}
	}
	return anchor, write, true
}

// trailingEmptyParagraphs counts the empty plain paragraphs from p to the
// end of the view. A split at the paragraph before them writes its break
// above the blank tail, and Typora's live view keeps those paragraphs after
// the one the split opens; it reports 0 when any of them is not an empty
// plain paragraph, so the split's lines are not the blank tail it holds.
func (v *view) trailingEmptyParagraphs(from int) int {
	count := 0
	for p := from; p < v.shown.ParagraphCount(); p++ {
		st := v.para(p).style
		if st.List != NoList || st.Quote || st.Heading > 0 || st.CodeBlock || v.para(p).n != 0 {
			return 0
		}
		count++
	}
	if count == 0 {
		return 0
	}
	// Only a text paragraph's split above a blank tail is the state
	// Typora keeps: an empty line's Enter opens its own paragraph, and a
	// heading or container before the tail owns its blank lines.
	p := from - 1
	st := v.para(p).style
	if st.List != NoList || st.Quote || st.Heading > 0 || st.CodeBlock || v.para(p).n == 0 {
		return 0
	}
	return count
}

// holdTrailingSplit keeps the empty paragraph a real split left after the
// caret. The view showed `trailing` empty paragraphs after the caret's
// paragraph; Typora's live view keeps them after the paragraph the split
// opened, while the projection of the new bytes folds the first of their
// blank lines into the split paragraph's own blank run. The view holds that
// one line as a paragraph of its own; the bytes already carry it, so the
// hold writes nothing when the caret leaves or the document is saved
// (record 2026-11-10, seed9/c12).
func (v *view) holdTrailingSplit(trailing int) {
	if trailing <= 0 || v.transient != nil {
		return
	}
	last := v.shown.ParagraphCount() - 1
	if last < 0 || last >= v.source.Len() {
		return
	}
	projected := v.projectedTrailingEmptyParagraphs()
	if projected != trailing {
		// The projection already shows every paragraph the split left, or
		// folds more than one of them: only the single folded line is the
		// recorded state.
		return
	}
	d := v.doc
	d.lock()
	base := d.md.p.String()
	caret := d.selection.Caret.Offset
	d.mu.Unlock()
	start := v.source.start(last)
	if start <= 0 || start >= len(base) || (base[start-1] != '\n' && base[start-1] != '\r') {
		return
	}
	// Typora reports the k-th empty paragraph of a blank run on its k-th
	// line, starting at the run's first line; the held paragraph follows
	// the ones the projection shows.
	runStart := start
	for runStart > 0 && (base[runStart-1] == '\n' || base[runStart-1] == '\r') {
		runStart--
	}
	if runStart > 0 {
		if base[runStart] == '\r' && runStart+1 < len(base) && base[runStart+1] == '\n' {
			runStart += 2
		} else {
			runStart++
		}
	}
	anchor := runStart + projected
	if anchor < runStart || anchor >= len(base) {
		return
	}
	preview := v.previewDocument(base)
	preview.SetSourceSelection(sourceCaret(caret))
	v.showTransient(&transientBlock{
		preview:  preview,
		anchor:   sourcePoint(anchor, BoundaryDownstream),
		write:    caret,
		atEOF:    true,
		finalEOL: strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r"),
		mode:     transientTrailingSplit,
	})
}

// projectedTrailingEmptyParagraphs counts the empty plain paragraphs the
// current projection ends with. A blank run at the end of the bytes folds
// into one paragraph however many lines it holds, so this is the number of
// empty paragraphs the cold projection shows there.
func (v *view) projectedTrailingEmptyParagraphs() int {
	last := v.shown.ParagraphCount() - 1
	if last < 0 || last >= v.source.Len() {
		return 0
	}
	projected := 0
	for p := last; p >= 0; p-- {
		st := v.para(p).style
		if st.List != NoList || st.Quote || st.Heading > 0 || st.CodeBlock || v.para(p).n != 0 {
			break
		}
		projected++
	}
	return projected
}

// holdMaterializedTail writes the empty paragraph the caret stands in at the
// document's end and holds the blank line the materialization folds into the
// paragraph above it as the paragraph after the caret's. Up leaves such a
// paragraph by writing it: the bytes then end in a blank run that reads back
// as the paragraph's own blank line, while Typora shows that line as a
// paragraph of its own below the one the caret lands in (record 2026-11-12,
// seed18/c8).
func (v *view) holdMaterializedTail() bool {
	t := v.transient
	if t == nil || t.marker != "" || t.isLineBreak() || !t.atEOF || t.isViewOnlyLine() {
		return false
	}
	before := v.shown.ParagraphCount()
	if !v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
		return false
	}
	folded := before - v.shown.ParagraphCount()
	if folded > 0 && v.projectedTrailingEmptyParagraphs() == folded {
		v.holdTrailingSplit(folded)
	}
	v.mode, v.coalesce = newStep, false
	if v.transient == nil {
		// The bytes folded no line into the paragraph above: the move up
		// behaves as it does after any materialization.
		v.vertical(-1, false)
	}
	return true
}

// emptyParagraphAboveCaret reports whether the paragraph above the transient
// one is empty: Up then lands in that paragraph, whose blank line the
// materialized bytes share with the paragraph below it.
func (v *view) emptyParagraphAboveCaret() bool {
	t := v.transient
	if t == nil || t.para <= 0 || t.para >= v.shown.ParagraphCount() {
		return false
	}
	st := v.para(t.para - 1).style
	return st.List == NoList && !st.Quote && st.Heading == 0 && !st.CodeBlock && v.para(t.para-1).n == 0
}

// pendingHardBreak returns the bytes a Shift+Enter at source holds when the
// block there writes hard breaks: the marker its last line lacks, the line
// ending and the continuation prefix, and the unit a further Shift+Enter
// adds. The block's own marker and the document's final EOL already spell
// the break, so input writes the unit and the content instead.
func (v *view) pendingHardBreak(source int) (unit, full string, ok bool) {
	d := v.doc
	d.lock()
	defer d.mu.Unlock()
	p := d.text.ParagraphCount() - 1
	loc := d.paragraphLocation(p)
	hard := d.hardBreakMarker(loc)
	if hard == "" || d.text.ParagraphStyleAt(d.text.paraStart(p)).Heading > 0 {
		// Headings end with Enter, which writes the lines below them.
		return "", "", false
	}
	src := d.md.p.String()
	if source < 0 || source > len(src) {
		return "", "", false
	}
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	cont := d.continuationPrefix(loc)
	full = hard + eol + cont
	line := lineAt(src, source)
	unit = full
	if strings.HasSuffix(src[line.start:line.end], hard) {
		// The line already ends with the marker, which the break reuses:
		// only the line ending is missing.
		unit = eol + cont
	}
	return unit, full, true
}

// heldBreakContent is the whitespace the preview holds after the pending
// line breaks, before the marker that spells the blank line for the parser.
// The line is blank, so it writes no bytes until content joins it; Typora
// shows the whitespace on the break's line (record 2026-11-10, newops/c47-3).
func (t *transientBlock) heldBreakContent() string {
	if t.preview == nil || t.brk.source == 0 {
		return ""
	}
	src := t.preview.Source()
	start := t.anchor.Offset + t.brk.source
	end := len(src)
	if t.marker != "" {
		at := strings.Index(src, t.marker)
		if at < 0 || at < start {
			return ""
		}
		end = at
	}
	if start < 0 || end > len(src) || start >= end {
		return ""
	}
	held := src[start:end]
	if strings.Trim(held, " \t") != "" {
		return ""
	}
	return held
}

// extendTransient applies another Enter-like intent to the virtual
// document, or adds another pending line break. It leaves the canonical
// source untouched.
func (v *view) extendTransient(intent EditIntent) bool {
	t := v.transient
	if t == nil || v.opts.ReadOnly {
		return false
	}
	if t.isLineBreak() {
		// Another Shift+Enter extends the pending run of line breaks. The
		// structural edit cannot add one to the empty last line its
		// preview shows, so the break's own bytes are inserted there.
		if intent != IntentShiftEnter {
			return false
		}
		src := t.preview.Source()
		at := t.preview.SourceSelection().Caret.Offset
		previous := t.preview
		unit := t.brk.unit
		// The line the added break leaves empty would read back as a blank
		// line, ending the paragraph, and Typora's view holds the empty
		// line. A soft break's unit begins with the line ending and a
		// space-marked hard break's with spaces alone: neither leaves a
		// marker the line keeps, so the break writes its backslash. A
		// backslash-marked unit already has one.
		soft := false
		if i := strings.IndexAny(unit, "\r\n"); i >= 0 && strings.Trim(unit[:i], " \t") == "" {
			unit = "\\" + unit[i:]
			soft = true
		}
		marked := src[:at] + unit + src[at:]
		state := previous.state().preview().mapPatch(at, at, len(unit), src, marked)
		extra := 0
		if soft && at == len(src) && strings.HasSuffix(src, t.brk.unit) {
			// The old line had no marker: its plain line ending kept it in
			// the paragraph only while it was the document's last line.
			// The added line follows it now, so an empty old line needs
			// the marker that keeps it inside the paragraph.
			old := at - len(t.brk.unit)
			if i := strings.IndexAny(t.brk.unit, "\r\n"); i >= 0 {
				eol := old + i
				if line := lineAt(src, eol); strings.TrimSpace(src[line.start:line.end]) == "" {
					marked = marked[:eol] + "\\" + marked[eol:]
					state = state.mapPatch(eol, eol, 1, src, marked)
					extra = 1
				}
			}
		}
		if i := strings.IndexAny(unit, "\r\n"); i >= 0 {
			state = state.setLiveLine(at + i + extra)
		}
		t.preview = literalPreview(marked, state)
		previous.md.carryOpenedLists(t.preview, at, at, unit)
		t.preview.SetSourceSelection(sourceCaret(at + len(unit) + extra))
		t.write = at + len(unit) + extra
		t.brk.source += len(unit) + extra
		t.brk.shown++
		t.mode = transientPendingBreak
		v.showTransient(t)
		return true
	}
	if t.isEmptyParagraph() && intent == IntentShiftEnter && t.atEOF && t.marker == "" &&
		t.plainBreakLine() {
		// Shift+Enter in the view-held empty line after a block adds a
		// plain line break to that line. No backslash marks it: the line
		// stays empty, so the break does not read back as a paragraph
		// separator. The empty paragraph becomes the line's bytes and the
		// break waits in the view (record 2026-11-10, seed7/c4).
		if v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			base := v.doc.Source()
			eol := defaultEOL(base)
			if len(base) > 0 && strings.HasSuffix(base, eol) {
				anchor := len(base) - len(eol)
				preview := v.previewDocument(base + eol)
				preview.SetSourceSelection(sourceCaret(len(base) + len(eol)))
				v.showTransient(&transientBlock{
					preview:  preview,
					anchor:   sourcePoint(anchor, BoundaryDownstream),
					write:    len(base) + len(eol),
					atEOF:    true,
					finalEOL: false,
					mode:     transientPendingBreak,
					brk:      pendingBreak{source: len(eol), shown: 1, unit: eol},
				})
				return true
			}
		}
	}
	if t.isEmptyParagraph() && intent == IntentShiftEnter && !t.atEOF && t.marker == "" &&
		t.plainBreakLine() {
		// The empty paragraph has no bytes of its own, so the break has no
		// line to sit on: write the paragraph's blank line and hold the
		// break at its end, as Shift+Enter does in any other empty
		// paragraph between blocks. Without the paragraph, the preview's
		// blank lines read the break as the next paragraph's first line,
		// and the caret is drawn below the line the rune lands on.
		if v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			p := v.shown.paraAt(v.caret)
			v.openTransient(IntentShiftEnter, v.paragraphEndSource(p), false)
			return true
		}
	}
	if t.isEmptyParagraph() && intent == IntentEnter && t.quoted() {
		// The lift wrote an empty quote line for the paragraph between two
		// quoted blocks. Enter on it leaves the quote, as it does on any
		// empty quote line, and the paragraph it exits to waits outside.
		if v.liftQuotedLine(t) {
			return true
		}
	}
	if t.isEmptyParagraph() && t.atEOF && t.finalEOL && intent == IntentEnter &&
		strings.TrimSpace(v.doc.Source()) != "" && t.plainAtEOF() {
		// Typora writes the empty paragraph the caret stands in when Enter
		// opens the line below it: the paragraph becomes real, as Enter at
		// the end of a text line writes its break (diff.json b609e7fa).
		if v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			p := v.shown.ParagraphCount() - 1
			if v.openTransient(IntentEnter, v.paragraphEndSource(p), true) {
				return true
			}
		}
	}
	before := t.preview.Source()
	heldCaret := t.write
	heldState := t.preview.state()
	selection := t.preview.SourceSelection()
	if t.isEmptyParagraph() && t.atEOF {
		t.preview.SetSourceSelection(sourceCaret(t.write))
		selection = t.preview.SourceSelection()
	}
	// Enter on the empty item the transient preview holds lifts it out of
	// its container: the paragraph it leaves is the container's exit, and
	// a later Enter materializes it (see materializeExitParagraphOnEnter).
	lifted := false
	join := false
	if intent == IntentEnter && t.isEmptyParagraph() {
		if doc := t.preview.docAtSource(selection.Caret.Offset); doc >= 0 {
			st := t.preview.text.ParagraphStyleAt(doc)
			lifted = st.List != NoList || st.Quote
		}
		// A nested empty item joins the item above it, and the only item of
		// a nested list leaves its marker as the second paragraph of the
		// parent: Typora writes that at the key press, so the bytes may not
		// wait in the view the way a split's empty line does (record
		// 2026-11-10, seed6/enter-after-tab-at-eof, seed6/shift-enter-then-
		// tab-enter).
		if selection.Anchor.Offset == selection.Caret.Offset {
			switch t.preview.emptyBlockAt(selection.Caret.Offset) {
			case emptyBlockNestedItem, emptyBlockOnlyNestedItem:
				join = true
			}
		}
	}
	if !applyTransientIntent(t.preview, intent) {
		t.preview.SetSourceSelection(selection)
		return false
	}
	if t.preview.Source() == before {
		return false
	}
	if lifted {
		sel := t.preview.SourceSelection()
		if doc := t.preview.docAtSource(sel.Caret.Offset); doc >= 0 {
			st := t.preview.text.ParagraphStyleAt(doc)
			t.setContainerExit(st.List == NoList && !st.Quote)
		}
	}
	if t.isEmptyParagraph() && t.atEOF {
		if anchor, write, ok := transientEndTarget(t.preview, t.preview.SourceSelection().Caret.Offset); ok {
			t.anchor = sourcePoint(anchor, BoundaryDownstream)
			t.write = write
			t.preview.SetSourceSelection(sourceCaret(anchor))
		} else {
			t.write = t.preview.SourceSelection().Caret.Offset
		}
	} else if t.isEmptyParagraph() {
		t.write = t.preview.SourceSelection().Caret.Offset
	}
	if join && v.materializeJoin(t, before, heldCaret, heldState) {
		return true
	}
	v.showTransient(t)
	return true
}

// seed24HeldBreakDeleteIntent reports the held hard break at the document's
// end whose trailing EOL Typora keeps through Delete. The break's own bytes
// are the file's final EOL, and no content or marker waits behind it
// (record-20261114-153934 / 154454, seed24/c2).
func seed24HeldBreakDeleteIntent(t *transientBlock) bool {
	if t == nil || !t.isLineBreak() || !t.atEOF || t.finalEOL ||
		t.marker != "" || t.mode != transientPendingBreak ||
		t.brk.source != 1 || t.brk.shown != 1 || t.preview == nil {
		return false
	}
	src := t.preview.Source()
	if t.anchor.Offset == 0 {
		return false
	}
	return strings.HasSuffix(src, "\n") && t.anchor.Offset+t.brk.source == len(src) &&
		t.write == len(src) && t.preview.SourceSelection().Caret.Offset == len(src)
}

// seed24HeldBreakKeepTransient writes the held break's trailing EOL into the
// canonical source and keeps the break's preview for the view. Delete then
// sees no byte after the caret to take, while the shown block keeps its EOL,
// source span and 1:2 caret.
func seed24HeldBreakKeepTransient(v *view) bool {
	d := v.doc
	d.lock()
	t := v.transient
	base := d.md.p.String()
	if len(base) >= len(t.preview.Source()) {
		d.mu.Unlock()
		return false
	}
	undo := t.undo(base)
	v.mode, v.coalesce = newStep, false
	err := v.lockedEdit(func(e *Edit) {
		if !e.replaceMarkdownSource(len(base), len(base), defaultEOL(base)) {
			return
		}
		e.selectMarkdownSource(len(base) + 1)
	})
	if err == nil && len(d.undo) > 0 {
		d.undo[len(d.undo)-1].transient = undo
	}
	d.mu.Unlock()
	if err != nil {
		return false
	}
	v.transient = t
	v.selAnchor, v.caret = d.anchor, d.caret
	v.selVersion = d.selVersion
	v.showTransient(t)
	return true
}

// materializeJoin writes the empty item the join lifts as bytes of its own
// before the join's own step. Typora writes that item's line at the Tab that
// indented it (record 2026-11-10, seed6/enter-after-tab-at-eof cp3), so the
// Enter that joins it lifts real bytes and one Undo lands on the indented
// empty child item instead of returning to the state before the indent
// (record 2026-11-12, seed20/c4). The join itself still comes from the
// preview the key built, which is where the blank line a loose list puts
// under its parent item comes from (record 2026-11-10, seed8/c8).
func (v *view) materializeJoin(t *transientBlock, held string, heldCaret int,
	state sessionState) bool {
	if held != v.doc.Source() {
		if !v.writeHeldLine(held, state, heldCaret) {
			return false
		}
		v.transient = t
		if !v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			return false
		}
		// The item's line is the document's now: taking the join back stands
		// on it rather than on a paragraph the view holds.
		v.doc.lock()
		if n := len(v.doc.undo); n > 0 {
			v.doc.undo[n-1].transient = nil
		}
		v.doc.mu.Unlock()
		return true
	}
	return v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true)
}

// writeHeldLine writes src, the line the view held, into the document as a
// step of undo of its own, with the view's caret at caret.
func (v *view) writeHeldLine(src string, state sessionState, caret int) bool {
	d := v.doc
	if d.md == nil {
		return false
	}
	base := d.md.p.String()
	from, to, newEnd := changedBytes(base, src)
	replacement := src[from:newEnd]
	d.lock()
	v.transient = nil
	v.version = 0
	v.mode = newStep
	v.coalesce = false
	d.mu.Unlock()
	var err error
	if editErr := v.lockedEdit(func(e *Edit) {
		if from != to || replacement != "" {
			if !e.replaceMarkdownSource(from, to, replacement) {
				err = ErrStructuralEdit
				return
			}
		}
		d.setState(state.restoreProjection(d.state()))
		if !e.reparseMarkdownSource() {
			err = ErrStructuralEdit
			return
		}
		e.selectMarkdownSource(min(caret, len(src)))
	}); editErr != nil {
		err = editErr
	}
	if err != nil {
		return false
	}
	v.selAnchor, v.caret = d.anchor, d.caret
	v.selVersion = d.selVersion
	v.mode, v.coalesce, v.coalesceKind = newStep, false, coalesceNone
	v.reveal = true
	return true
}

// plainAtEOF reports whether the transient paragraph is a plain paragraph:
// an item or quote the caret sits in lifts on Enter instead.
func (t *transientBlock) plainAtEOF() bool {
	if t.preview == nil {
		return false
	}
	at := t.preview.SourceSelection().Caret.Offset
	return t.plainParagraphAt(at)
}

// plainBreakLine reports whether the line Shift+Enter adds a break to is a
// plain paragraph. The exit paragraph has no bytes of its own, so the
// preview's own selection stands at the block the lift emptied one line
// above: the paragraph the caret shows in is at write (record 2026-11-12,
// seed15/c13).
func (t *transientBlock) plainBreakLine() bool {
	if t.preview == nil {
		return false
	}
	at := t.preview.SourceSelection().Caret.Offset
	if t.mode == transientContainerExit {
		at = t.write
	}
	return t.plainParagraphAt(at)
}

// plainParagraphAt reports whether the paragraph source offset at names is a
// plain paragraph: an item or quote the caret sits in lifts on Enter instead.
func (t *transientBlock) plainParagraphAt(at int) bool {
	if doc := t.preview.docAtSource(at); doc >= 0 {
		st := t.preview.text.ParagraphStyleAt(doc)
		return st.List == NoList && !st.Quote
	}
	return false
}

// previewLastParagraphPlain reports whether the preview's last paragraph is a
// plain one: the marker the lift wrote after a quote's tail spells that
// paragraph, so it stands outside the quote (record 2026-11-13,
// zz177/A-empty-line-enter).
func (t *transientBlock) previewLastParagraphPlain() bool {
	if t.preview == nil || t.preview.md == nil {
		return false
	}
	t.preview.lock()
	defer t.preview.mu.Unlock()
	last := t.preview.text.ParagraphCount() - 1
	if last < 0 {
		return false
	}
	st := t.preview.text.paragraph(last).Style
	return st.List == NoList && !st.Quote
}

// heldBreakEndsParagraph takes the second Shift+Enter on the break a plain
// paragraph's last line holds at the end of the document. The line the
// added break would open is blank, and Markdown reads a blank line as the
// paragraph's end: Typora ends the paragraph there and shows the line below
// it as an empty paragraph of its own, whose bytes wait for the first input.
// Writing the break instead marks that line with a backslash the user never
// typed (record 2026-11-12, seed17/c12). A break that opens its paragraph,
// or one inside an item or quote, stays pending: the line below it still
// belongs to the block, which the container's prefix keeps.
func (v *view) heldBreakEndsParagraph() bool {
	t := v.transient
	if t == nil || t.isEmptyParagraph() || !t.atEOF || t.marker != "" || t.brk.shown != 1 {
		return false
	}
	if i := strings.IndexAny(t.brk.unit, "\r\n"); i != 0 {
		// Only a plain line ending is the break this takes: a hard break's
		// unit starts with its marker, which keeps the line inside the
		// paragraph.
		return false
	}
	src := t.preview.Source()
	if t.anchor.Offset <= 0 || t.anchor.Offset > len(src) || !t.plainBreakLine() {
		return false
	}
	if line := previousMarkdownLine(src, t.anchor.Offset); strings.TrimSpace(src[line.start:line.end]) == "" {
		return false
	}
	if p := v.shown.paraAt(v.caret); p < 0 || p >= v.shown.ParagraphCount() ||
		strings.TrimSpace(strings.Trim(plainOf(v.shown.paragraph(p)), string(LineSeparator))) == "" {
		// An empty paragraph holds no line to end: the break it opens stays
		// pending, and the first input writes it.
		return false
	}
	v.discardTransient()
	p := v.shown.paraAt(v.caret)
	if p < 0 {
		return false
	}
	return v.openTransient(IntentEnter, v.paragraphEndSource(p), true)
}

// quoted reports whether the marker-spelled paragraph sits inside a quote:
// the marker line's prefix is the quote the lift wrote before it.
func (t *transientBlock) quoted() bool {
	if t.marker == "" {
		return false
	}
	src := t.preview.Source()
	if t.write < 0 || t.write > len(src) {
		return false
	}
	line := lineAt(src, t.write)
	return strings.Contains(src[line.start:t.write], ">")
}

// liftQuotedLine takes Enter on the empty quote line the transient paragraph
// sits on. The line leaves the quote as a step of undo, and the paragraph it
// exits to waits outside the quote.
func (v *view) liftQuotedLine(t *transientBlock) bool {
	base := v.doc.Source()
	line := previousMarkdownLine(base, t.anchor.Offset)
	if !quoteOnly(prevBody(base, line)) {
		return false
	}
	if v.doc.emptyBlockAt(line.start) != emptyBlockQuoteLine {
		return false
	}
	var lift emptyLift
	var ok bool
	v.command(func(d *Document) { lift, ok = d.liftEmptyBlock(line.start) })
	if !ok {
		return true
	}
	finalEOL := strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r")
	v.openEmptyExitTransient(lift, finalEOL)
	// The paragraph the lift exits to is what the step leaves behind: Undo
	// takes the lift back to the quote line, as it does for the lift Enter
	// performs on an empty item (record 2026-11-13, seed21/c6).
	if next := v.transient; next != nil {
		d := v.doc
		d.lock()
		if n := len(d.undo); n > 0 {
			d.undo[n-1].afterTransient = next.undo(d.md.p.String())
		}
		d.mu.Unlock()
	}
	return true
}

func applyTransientIntent(d *Document, intent EditIntent) bool {
	before := d.Source()
	err := d.ApplyIntent(intent)
	if err != nil {
		if intent != IntentExitBlock && intent != IntentExitDocument {
			return false
		}
		err = d.ApplyIntent(IntentEnter)
	}
	return err == nil && d.Source() != before
}

func (v *view) canOpenTransient(intent EditIntent, anchor, caret int) bool {
	if v.opts.ReadOnly || v.doc.md == nil || anchor != caret {
		return false
	}
	p := v.shown.paraAt(caret)
	st := v.para(p).style
	ps := v.shown.paraStart(p)
	pe := ps + v.para(p).n
	if intent == IntentEnter && v.splitsItemLiteralRun(p) {
		// Enter inside a list item whose whole text is an emphasis run the
		// editor does not render as an element splits the run between the
		// item and its new sibling, leaving one marker on each: `3. **`
		// becomes `3. *` and `4. *`. The bytes exist as soon as the split
		// writes them, so no empty item waits in the view (record
		// 2026-11-12, seed20/c6).
		return false
	}
	if caret != pe && v.caretBeforeClosingDelimiters(p) {
		// The caret stands at the paragraph's content end, with only that
		// paragraph's closing delimiters between it and the paragraph's end.
		// Typora splits after them: the pair stays on the paragraph the
		// caret leaves (record 2026-11-13, seed17/c11).
		caret = pe
	}
	if caret != pe {
		return false
	}
	switch intent {
	case IntentEnter:
		if st.CodeBlock {
			return false
		}
		if st.List != NoList && p == v.shown.ParagraphCount()-1 && v.continuationParagraph(p) {
			// Enter at the end of an item's continuation paragraph writes
			// the paragraph it opens at once: Typora materializes the
			// indented empty line, which a transient would hold without
			// bytes (record 2026-11-11, seed10/c8).
			return false
		}
		return true
	case IntentShiftEnter:
		if st.CodeBlock {
			return false
		}
		// The last paragraph's end has no byte that could hold the line
		// break: a final newline the document keeps replaces it in the
		// view until the first content edit.
		if p == v.shown.ParagraphCount()-1 {
			return true
		}
		// A list item's break writes only its continuation prefix, which
		// Markdown reads as a blank line: the bytes exist but no line
		// shows, so the view holds the break too.
		v.doc.lock()
		loc := v.doc.paragraphLocation(p)
		hard := v.doc.hardBreakMarker(loc)
		cont := v.doc.continuationPrefix(loc)
		v.doc.mu.Unlock()
		return blankQuoteLine(hard + cont)
	case IntentExitBlock:
		// An exit from a list item writes its continuation line as a real
		// block at once; Typora's snapshot of it holds no transient (record
		// 2026-11-10, exitprobe/*-cmdenter-eof). An exit that writes no
		// bytes, such as the one at a code block's end, stays in the view.
		return st.List == NoList
	default:
		return false
	}
}

// splitsItemLiteralRun reports whether the caret's paragraph is the whole text
// of a list item and holds nothing but one emphasis run the editor draws as
// text: no element owns those markers, so Enter between them writes one on
// the item the caret leaves and one on the sibling it opens (record
// 2026-11-12, seed20/c6).
func (v *view) splitsItemLiteralRun(p int) bool {
	if v.doc == nil || v.doc.md == nil || p < 0 || p >= v.source.Len() {
		return false
	}
	if v.para(p).style.List != Numbers {
		return false
	}
	start, end := v.source.start(p), v.paragraphEndSource(p)
	text := v.sourceSlice(start, end)
	if len(text) < 2 || strings.Trim(text, "*_") != "" {
		return false
	}
	// A run the editor renders as an element keeps its markers as the
	// element's own, and a split inside it closes and reopens the element on
	// the two sides.
	for _, b := range v.boxes(p) {
		if !b.literal && b.openStart < b.openEnd && b.closeStart < b.closeEnd {
			return false
		}
	}
	return true
}

// caretBeforeClosingDelimiters reports whether the caret stands at a
// paragraph's content end with only that paragraph's own closing delimiters
// between it and the paragraph's end. The run is the parser's box, or the
// pair a formatting command wrote and the view holds because the parser
// reads its bytes as text (see live_hold.go).
func (v *view) caretBeforeClosingDelimiters(p int) bool {
	at := v.caretSource()
	if at < 0 {
		return false
	}
	end := v.paragraphEndSource(p)
	for _, b := range v.boxes(p) {
		if b.closeStart == at && b.closeEnd == end && b.closeEnd > b.closeStart {
			return true
		}
	}
	h := v.liveHoldAt(v.sourceText(), at)
	return h != nil && h.para == p && h.closeStart == at && h.closeEnd == end
}

func (v *view) continuationParagraph(p int) bool {
	if p < 0 || p >= v.source.Len() || v.doc == nil || v.doc.md == nil {
		return false
	}
	src := v.sourceText()
	start := v.source.start(p)
	if start < 0 || start >= len(src) {
		return false
	}
	line := lineAt(src, start)
	if start < line.start || start > line.end {
		return false
	}
	prefix := blockPrefix(src[line.start:line.end], v.para(p).style)
	return prefix != "" && strings.TrimSpace(prefix) == ""
}

// syncLineBreakCaret points the pending line break's preview at the view's
// caret, which may have moved back onto the paragraph's text line. The
// snapshot and the first input read the caret from the preview.
func (v *view) syncLineBreakCaret() {
	t := v.transient
	if t == nil || t.isEmptyParagraph() {
		return
	}
	t.preview.SetSourceSelection(sourceCaret(v.sourceForCaret(v.caret)))
}

// extendHeldBreak extends the selection from the line the pending break holds
// onto the paragraph's text line, keeping the break in the view. The break's
// bytes are the preview's, so its end is a source offset the document's clamp
// would pull back before the break: the selection is assigned in that source
// (record 2026-11-10, seed8/c14).
func (v *view) extendHeldBreak() {
	t := v.transient
	if t == nil || t.isEmptyParagraph() {
		return
	}
	d := v.doc
	collapsed := v.selAnchor == v.caret
	if collapsed {
		// The document's caret stays on the break's anchor while the view's
		// stands after it; the held line's own position is the preview's.
		live := sourcePoint(v.sourceForCaret(v.caret), BoundaryDownstream)
		d.lock()
		d.selection = SourceSelection{Anchor: live, Caret: live}
		d.anchor, d.caret = d.docSelectionForSourceLocked(d.selection)
		v.selAnchor, v.caret = d.anchor, d.caret
		d.mu.Unlock()
	}
	d.lock()
	source := d.selection.Caret
	anchor := d.selection.Anchor
	d.mu.Unlock()
	p := v.shown.paraAt(v.caret)
	pv := v.paraLayout(p)
	at := 0
	if pv != nil && pv.display != nil {
		at = v.displayPositionForCaret(p, pv, source, true)
	}
	if at > 0 {
		o := pv.display.Map.At(at - 1)
		d.lock()
		d.selection = SourceSelection{
			Anchor: anchor,
			Caret:  sourcePoint(o.Source, BoundaryDownstream),
		}
		d.anchor, d.caret = d.docSelectionForSourceLocked(d.selection)
		v.selAnchor, v.caret = d.anchor, d.caret
		v.anchorAffinity, v.caretAffinity = d.selection.Anchor.Affinity, d.selection.Caret.Affinity
		d.mu.Unlock()
	}
	v.affinity = ui.TextUpstream
	v.coalesce = false
	v.reveal = true
	v.hasDesired = false
	v.syncLineBreakCaret()
}

// indentPendingBreak indents or outdents the list item a pending line break
// belongs to. The break survives the structural edit and writes the item's
// new continuation prefix (diff.json 1cf60ae5).
func (v *view) indentPendingBreak(outdent bool) bool {
	t := v.transient
	if t == nil || t.isEmptyParagraph() || v.opts.ReadOnly || v.doc.md == nil {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.shown.ParagraphCount() || v.para(p).style.List == NoList {
		return false
	}
	if !outdent && v.doc.firstListItem(v.caret) {
		// The item cannot indent further: the ordinary path keeps the tab
		// as text and drops the break.
		return false
	}
	breaks, atEOF := t.brk.shown, t.atEOF
	intent := IntentIndent
	if outdent {
		intent = IntentOutdent
	}
	v.command(func(d *Document) {
		_ = d.applyIntent(EditRequest{Intent: intent, Start: v.caret, End: v.caret})
	})
	if v.transient != nil {
		return false
	}
	p = v.shown.paraAt(v.caret)
	if p < 0 || p >= v.shown.ParagraphCount() {
		return false
	}
	if !v.openTransient(IntentShiftEnter, v.paragraphEndSource(p), atEOF) {
		return false
	}
	for i := 1; i < breaks; i++ {
		if !v.extendTransient(IntentShiftEnter) {
			break
		}
	}
	return true
}

// showTransient installs the virtual document as the view projection. Its
// empty paragraphs that Markdown whitespace alone cannot represent are
// added to the view without entering the document or its SourceMap.
func (v *view) showTransient(t *transientBlock) {
	d := v.doc
	d.lock()
	defer d.mu.Unlock()

	shown, source := t.preview.Text(), t.preview.SourceMap()
	sel := t.preview.SourceSelection()
	sourceSelection := sourceCaret(t.anchor.Offset)
	caretSource := t.write
	if t.isLineBreak() {
		caretSource = sel.Caret.Offset
	}
	caret, _ := t.preview.RichSelectionFromSource(caretSource, caretSource)
	if t.marker != "" {
		// The marker spells the transient paragraph for the parser; the
		// view shows the paragraph empty, so the marker leaves it.
		if at := strings.Index(t.preview.Source(), t.marker); at >= 0 {
			mid, _ := t.preview.RichSelectionFromSource(at, at)
			runes := utf8.RuneCountInString(t.marker)
			shown = shown.ReplaceString(mid, mid+runes, "", Attributes{})
			if caret > mid {
				caret -= runes
			}
		}
	}
	if t.isLineBreak() {
		// The pending breaks have no bytes in the canonical document, so
		// its projection leaves them out: the view holds them as trailing
		// LineSeparators of the last paragraph, over the breaks' bytes.
		// The break belongs to the paragraph the anchor sits at, which
		// need not be the last: a list item holds a soft break before the
		// items below it.
		at, _ := d.docSelectionForSourceLocked(sourceCaret(t.anchor.Offset))
		p := d.text.paraAt(at)
		_, end := d.text.ParagraphRange(at)
		shown = d.text.ReplaceString(end, end, strings.Repeat(string(LineSeparator), t.brk.shown), Attributes{})
		caret = end + t.brk.shown
		source = d.md.m.appendBreak(p, t.brk.shown, t.brk.source)
		if held := t.heldBreakContent(); held != "" {
			// Whitespace typed after the break stays in the preview: the
			// line is blank, so it writes no bytes until content joins it.
			// Typora shows it on the break's line (record 2026-11-10,
			// newops/c47-3).
			shown = shown.ReplaceString(caret, caret, held, Attributes{})
			caret += utf8.RuneCountInString(held)
			source = source.appendSegment(p, Segment{
				Kind:   SegmentSynthetic,
				Shown:  utf8.RuneCountInString(held),
				Source: len(held),
			})
		}
		if sel.Caret.Offset < t.write {
			// The caret has moved back onto the paragraph's text line: the
			// break follows it there (diff.json fc3db91f). An empty
			// paragraph's start can map past the anchor, over its blank
			// line, so the move is judged against where the break ends.
			caret, _ = d.docSelectionForSourceLocked(sel)
			sourceSelection = sel
		}
	}
	if t.isEmptyParagraph() && t.atEOF && t.marker == "" {
		// The preview's last paragraph is the one the caret enters. When
		// the source after the block is only a blank line, the projection
		// ends at the block: the empty paragraph the caret sits in needs
		// the line separator the bytes do not show yet. (A blank preview
		// shows paragraphs by the rule below instead.)
		start, end := shown.ParagraphRange(caret)
		empty := caret == shown.Len() && start == end
		// A preview whose paragraph already ends in a soft break shows the
		// caret's line: Shift+Enter leaves the empty item's line holding
		// the break, and Typora shows no further paragraph (record
		// 2026-11-10, seed4/c17).
		held := caret == shown.Len() && !empty && endsWithSoftBreak(shown, caret)
		if !empty && !held && caret == shown.Len() && strings.TrimSpace(t.preview.Source()) != "" {
			// The line after a heading is a paragraph of its own, not another
			// heading line: the projection ends at the heading block, so the
			// empty paragraph the caret enters brings its own style (record
			// 2026-11-10, newops/c46-5).
			heading := shown.ParagraphStyleAt(caret).Heading > 0
			shown = shown.ReplaceString(shown.Len(), shown.Len(), "\n", Attributes{})
			caret = shown.Len()
			if heading {
				shown = shown.FormatParagraphs(caret, caret, func(st *ParagraphStyle) { *st = ParagraphStyle{} })
			}
		} else if t.mode == transientContainerExit && strings.TrimSpace(t.preview.Source()) != "" {
			// The paragraph the exit opened is not the preview paragraph
			// the write lands in: that one is still the item or quote the
			// lift emptied, and the caret must be drawn in the paragraph
			// after it, as Typora draws it (record 2026-11-09, seed3/c22).
			at, _ := t.preview.RichSelectionFromSource(t.write, t.write)
			if st := t.preview.Text().ParagraphStyleAt(at); st.List != NoList || st.Quote {
				shown = shown.ReplaceString(shown.Len(), shown.Len(), "\n", Attributes{})
				caret = shown.Len()
			}
		}
	}
	if t.isEmptyParagraph() && t.isTrailingSplit() {
		// The split wrote the paragraph break above a blank line the
		// document already had; the empty paragraph that line opens stands
		// after the one the caret entered, and the view keeps it without a
		// source range of its own (record 2026-11-10, seed9/c12).
		shown = shown.ReplaceString(shown.Len(), shown.Len(), "\n", Attributes{})
	}
	if t.isEmptyParagraph() && strings.TrimSpace(t.preview.Source()) == "" {
		// Two line endings separate paragraphs of blank lines; a single one
		// only ends a line. The preview writes both per empty paragraph the
		// intent added, so its projection can show a blank line the edit
		// did not make: the view holds the intent's paragraphs, no more.
		breaks := countLineBreaks(t.preview.Source()[:clampByte(t.preview.Source(), t.write)])
		want := max(1, 1+breaks/2)
		switch extra := want - shown.ParagraphCount(); {
		case extra > 0:
			shown = shown.ReplaceString(shown.Len(), shown.Len(), strings.Repeat("\n", extra), Attributes{})
			caret = shown.Len()
		case extra < 0:
			if want > 1 {
				shown = shown.ReplaceString(shown.paraStart(want)-1, shown.Len(), "", Attributes{})
			} else {
				shown = shown.ReplaceString(0, shown.Len(), "", Attributes{})
			}
			caret = shown.Len()
		}
	}

	v.transient = t
	v.shown = shown
	v.source = source
	v.version = d.version
	v.selVersion = d.selVersion
	v.paras = make([]paraView, shown.ParagraphCount())
	v.laid = 0
	v.resetHeights()
	v.hoverPara, v.hoverBox, v.hoverLink = -1, -1, ""
	v.keepEmptyContainer()
	v.selAnchor, v.caret = caret, caret
	t.para = shown.paraAt(caret)
	if t.isTrailingSplit() {
		// The held paragraph is the one after the caret's.
		t.para = shown.ParagraphCount() - 1
	}
	v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true

	if t.isTrailingSplit() {
		// The document's selection follows the caret, in the paragraph the
		// split put it in, not the held paragraph after it.
		if p := shown.paraAt(caret); p < source.Len() {
			sourceSelection = sourceCaret(source.start(p))
		}
	}
	d.selection = sourceSelection
	d.anchor, d.caret = d.docSelectionForSourceLocked(d.selection)
}

func clampByte(s string, at int) int {
	return max(0, min(at, len(s)))
}

// endsWithSoftBreak reports whether the paragraph holding at ends in a line
// separator, so at stands on the line that separator opens.
func endsWithSoftBreak(t Text, at int) bool {
	p := t.paraAt(at)
	if p < 0 || p >= t.ParagraphCount() {
		return false
	}
	runs := t.paragraph(p).Runs
	if len(runs) == 0 {
		return false
	}
	return strings.HasSuffix(runs[len(runs)-1].Text, string(LineSeparator))
}

func countLineBreaks(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\r':
			n++
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		case '\n':
			n++
		}
	}
	return n
}

// showEmptyDocument shows the paragraph of a document with no source bytes.
// Undo that takes away the last of them leaves the empty paragraph Typora
// shows there, which Markdown has no bytes for.
func (v *view) showEmptyDocument() {
	d := v.doc
	d.lock()
	empty := d.md != nil && d.md.p.String() == ""
	d.mu.Unlock()
	if !empty || v.transient != nil {
		return
	}
	preview := NewMarkdownDocument("")
	preview.md.keepFinalEOLCaret = true
	preview.md.preview = true
	preview.SetSourceSelection(sourceCaret(0))
	v.showTransient(&transientBlock{
		preview: preview,
		anchor:  sourcePoint(0, BoundaryDownstream),
		atEOF:   true,
	})
}

// holdEmptiedDocument puts a document a cut emptied back to the empty
// paragraph a document with no source bytes shows. A Cut that takes the
// last of the content away leaves no bytes for the view to hold: a
// pending line break has lost the line it stood on and goes with it, and
// the empty paragraph waits where Typora's undo of the last content leaves
// it (record 2026-11-12, seed13/c8).
func (v *view) holdEmptiedDocument() {
	d := v.doc
	d.lock()
	empty := d.md != nil && d.md.p.String() == ""
	d.mu.Unlock()
	if !empty || v.transient == nil {
		return
	}
	v.discardTransient()
	v.showEmptyDocument()
}

// holdTrailingForUndo drops the held paragraph an action acts before, and
// records it on the step of undo the action makes: the held line stands
// after the caret, so the content goes into the paragraph the caret is in,
// and undoing that content puts the line and its bytes back as they were
// (record 2026-11-10, seed9/c12).
func (v *view) holdTrailingForUndo() bool {
	t := v.transient
	if t == nil || !t.isTrailingSplit() {
		if t == nil || v.doc == nil || len(v.doc.undo) == 0 ||
			t.isLineBreak() || !t.atEOF || t.isViewOnlyLine() ||
			t.mode == transientContainerExit || t.marker != "" {
			return false
		}
		v.doc.lock()
		step := v.doc.undo[len(v.doc.undo)-1]
		base := ""
		if v.doc.md != nil {
			base = v.doc.md.p.String()
		}
		v.doc.mu.Unlock()
		if len(step.patches) != 1 || len(step.patches[0].old) != 1 ||
			!strings.HasSuffix(base, "\n") || t.preview.Source() != base+"\n\n" {
			return false
		}
		t.mode = transientReopenedTail
		return true
	}
	d := v.doc
	d.lock()
	base := ""
	if d.md != nil {
		base = d.md.p.String()
	}
	undo := t.undo(base)
	d.mu.Unlock()
	v.discardTransient()
	d.lock()
	d.pendingTransient = undo
	d.mu.Unlock()
	return true
}

// transientRecoveryClosedTail takes back the empty paragraph Enter opened at
// the document's end when the paragraph above it ends on a closing delimiter
// the view hides, as the `**` of `alpha **beta**`. Typora's Backspace joins
// the two paragraphs by taking their break, and that break is the only byte
// between the last rune the paragraph shows and its closing delimiter: the
// join would start at the visible end and read `alpha **beta`, where Markdown
// finds no emphasis at all. Typora keeps the pair and the bytes, so the view
// only drops the empty paragraph (record 2026-11-13, seed22/c1). It reports
// whether it made the edit.
func (v *view) transientRecoveryClosedTail() bool {
	t := v.transient
	if t == nil || v.opts.ReadOnly || v.doc.md == nil || t.isLineBreak() ||
		!t.atEOF || t.mode == transientContainerExit || t.isViewOnlyLine() || t.marker != "" {
		return false
	}
	if v.selAnchor != v.caret {
		return false
	}
	if p := v.shown.paraAt(v.caret); p >= 0 && p < v.source.Len() && p != v.source.Len()-1 {
		// The caret stands in a paragraph of the source below the last one:
		// the join is that paragraph's own, not the tail's.
		return false
	}
	d := v.doc
	d.lock()
	p := d.md.m.Len() - 1
	closed := false
	if p >= 0 {
		st := d.text.ParagraphStyleAt(d.text.paraStart(p))
		if st.List != NoList || st.Quote || st.Heading != 0 || st.CodeBlock {
			// A container's last line carries the block's own bytes past the
			// closing delimiter, and taking its empty paragraph back writes
			// the continuation Typora shows for it (record 2026-11-13,
			// seed12/c9): only a plain paragraph ends on the delimiter.
			d.mu.Unlock()
			return false
		}
		_, end, _ := d.md.m.Paragraph(p)
		visible := d.sourceAtParagraph(p, paragraphRunes(d.text.paragraph(p)))
		for _, b := range d.md.m.boxes(p) {
			// The box's closing bytes stand after the last rune the paragraph
			// shows, and the paragraph ends on them: the join's range would
			// carry them away with the break.
			if !b.literal && b.closeEnd > b.closeStart && b.closeEnd == end && b.closeStart >= visible {
				closed = true
			}
		}
	}
	d.mu.Unlock()
	if !closed {
		return false
	}
	v.discardTransient()
	return true
}

// discardTransient drops a transient that received no content.
func (v *view) discardTransient() {
	d := v.doc
	d.lock()
	defer d.mu.Unlock()

	t := v.transient
	if t != nil && t.preservesOnShiftLeft() && t.atEOF && t.marker == "" {
		return
	}
	if t == nil || !v.dropTransientLocked() {
		return
	}
	v.version = d.version
	v.selVersion = d.selVersion
	// The anchor may lie in the preview's deferred bytes, past the
	// document's source: dropping the paragraph leaves the caret at the
	// nearest position the source has.
	if t.isTrailingSplit() {
		// The held paragraph stands after the caret, which is already in a
		// paragraph of the source: dropping the line leaves the selection
		// where it is.
		d.selection = d.clampSourceSelectionLocked(d.selection)
	} else {
		d.selection = d.clampSourceSelectionLocked(sourceCaret(t.anchor.Offset))
	}
	d.anchor, d.caret = d.docSelectionForSourceLocked(d.selection)
	v.selAnchor, v.caret = d.anchor, d.caret
	v.anchorAffinity = d.selection.Anchor.Affinity
	v.caretAffinity = d.selection.Caret.Affinity
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
}

// dropTransientLocked removes the preview projection without moving the
// document's selection. The caller holds the document lock.
func (v *view) dropTransientLocked() bool {
	if v.transient == nil {
		return false
	}
	d := v.doc
	v.transient = nil
	v.shown = d.text
	v.source = SourceMap{}
	if d.md != nil {
		v.source = d.md.m
	}
	v.paras = make([]paraView, v.shown.ParagraphCount())
	v.laid = 0
	v.resetHeights()
	v.hoverPara, v.hoverBox, v.hoverLink = -1, -1, ""
	v.keepEmptyContainer()
	v.compose, v.composing = "", false
	v.composeCaret, v.composeSelected = 0, ui.TextInputRange{}
	v.pendingSet, v.pendingSourceSet = false, false
	return true
}

// materializeActiveTransient writes an untouched transient's bytes, as
// Typora serializes the empty block its DOM shows when the document is
// saved. The line a move past the document's last line reached is no
// block, so it leaves no bytes. A transient that cannot be materialized
// is dropped instead.
func (d *Document) materializeActiveTransient() {
	d.lock()
	v := d.activeView()
	d.mu.Unlock()
	if v == nil || v.transient == nil {
		return
	}
	if !v.transient.writesBytesOnBlur() {
		// The line a move past the document's last block reaches is no block
		// in Typora's document: saving writes no bytes for it and leaves it
		// under the caret (record 2026-11-10, seed8/c15).
		return
	}
	if !v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
		v.discardTransient()
	}
}

// transientLineBlankAt reports whether the line the materialized content
// ends on holds only whitespace: a transient paragraph or break written so
// far has no line of its own for Markdown to keep.
func transientLineBlankAt(src string, caret int) bool {
	line := lineAt(src, max(0, caret-1))
	return strings.TrimSpace(src[line.start:line.end]) == ""
}

// discardActiveTransient clears the preview of d's active editor, if any.
func (d *Document) discardActiveTransient() {
	d.lock()
	v := d.activeView()
	d.mu.Unlock()
	if v != nil {
		v.discardTransient()
	}
}

// materializeTransient applies the deferred structural edit and the first
// content edit as one undo step. It first performs the content edit in a
// virtual document, because a source made only of blank lines has no
// writable text range until that content exists.
func (v *view) materializeTransient(fn func(e *Edit, at int, attrs Attributes) error, force bool) bool {
	t := v.transient
	if t == nil || v.opts.ReadOnly {
		return false
	}
	d := v.doc
	d.lock()
	attrs := d.typingAttributes()
	base := d.md.p.String()
	breakAt := -1
	if t.isLineBreak() && t.marker == "" {
		// 持有换行在段落自己的末行上：内容落到它下面一行后，这一
		// 段的源码末尾就是补硬换行的位置。是否真的成立由写入后的
		// 重解析判定，行几何只用来定位。
		at, _ := d.docSelectionForSourceLocked(sourceCaret(t.anchor.Offset))
		if p := d.text.paraAt(at); p >= 0 && p < d.md.m.Len() {
			_, breakAt, _ = d.md.m.Paragraph(p)
		}
	}
	d.mu.Unlock()

	marker := t.marker
	at := t.write
	previewSrc := t.preview.Source()
	marked := previewSrc
	shift := 0
	inserted := ""
	if marker == "" {
		if t.mode == transientContainerExit && t.atEOF && at == len(previewSrc) && at > 0 &&
			(previewSrc[at-1] == '\n' || previewSrc[at-1] == '\r') &&
			!transientLineBlankAt(previewSrc, at) {
			// The exit paragraph has no preview bytes: the preview ends
			// with the container's own line. Markdown needs a blank line
			// between a container and the paragraph after it, so the
			// marker that gives the paragraph its bytes follows one, as
			// Typora writes the empty paragraph the lift opened (record
			// 2026-11-09, seed3/c22).
			inserted = defaultEOL(previewSrc)
		}
		marker = transientMarker(marked)
		marked = marked[:at] + inserted + marker + marked[at:]
		shift = len(inserted) + len(marker)
	}
	state := t.preview.state().preview()
	if shift != 0 {
		state = state.mapPatch(at, at, shift, previewSrc, marked)
	}
	work := newExtMarkdownDocument(marked, state, previewExtensions)
	if t.marker != "" {
		t.preview.md.carryOpenedLists(work, 0, 0, "")
	} else {
		t.preview.md.carryOpenedLists(work, at, at, inserted+marker)
	}
	if p := v.pfx.live; p != nil {
		if line, content := livePrefixLiteralSite(p); line >= 0 {
			if shift != 0 {
				line, _ = parser.MapLiteralOffset(line, at, at, shift)
				content, _ = parser.MapLiteralOffset(content, at, at, shift)
			}
			work.setState(work.state().setLiteralLine(line, content))
		} else if line, ok := parser.MapLiteralOffset(p.lineStart, at, at, shift); ok {
			work.setState(work.state().clearLiteralLineAt(line))
		}
	}
	first := at + len(inserted)
	if t.isLineBreak() {
		if sel := t.preview.SourceSelection().Caret.Offset; sel <= t.anchor.Offset {
			// The caret stands on the paragraph's text line, before the
			// break: the input goes there and the break follows it
			// (diff.json fc3db91f).
			first = sel
		}
	}
	work.SetSourceSelection(sourceCaret(first))
	workAt := work.docAtSource(first)
	if t.isLineBreak() {
		// The marker makes the held line break's bytes real: the caret
		// at the break's end maps past it, where the first content goes.
		workAt = work.caret
	}
	var err error
	caret := at
	work.Edit(func(e *Edit) {
		err = fn(e, workAt, attrs)
		if err != nil {
			e.fail(err)
			return
		}
		p := e.d.text.paraAt(e.d.caret)
		caret = e.d.sourceAtParagraph(p, e.d.caret-e.d.text.paraStart(p))
		// A caret at the end of the text sits after a closing marker, as it
		// does when the text arrives one rune at a time. Pasted plain text
		// lands whole, so the marker must move it (record-20261109-232354).
		caret = e.d.splitOutside(p, caret)
	})
	if err != nil {
		return false
	}
	workCaret := caret
	state = work.state().preview()
	virtual := work.Source()
	virtual, markerAt, ok := removeTransientMarker(virtual, marker)
	if !ok {
		return false
	}
	if t.atEOF {
		state = state.dropAfter(markerAt)
	} else {
		sourceWithMarker := virtual[:markerAt] + marker + virtual[markerAt:]
		state = state.mapPatch(markerAt, markerAt+len(marker), 0, sourceWithMarker, virtual)
	}
	if markerAt < caret {
		caret -= len(marker)
	}
	virtual = normalizeTransientEnd(virtual, markerAt, t)
	caret = max(0, min(caret, len(virtual)))
	if p := v.pfx.live; p != nil && caret >= p.lineEnd && lineAt(virtual, caret).start == p.lineStart {
		// The live prefix holds the caret on the line it was typed on.
		// A transient opened on a following line (Enter at the end of a
		// literal line) leaves the prefix behind: its line end has no
		// say over the new line's caret (record 2026-11-10, seed4/c13).
		caret = p.lineEnd
	}
	if !force && transientLineBlankAt(virtual, caret) {
		// Whitespace alone gives the transient paragraph or break no line
		// of its own in Markdown: the reparse would take the caret out of
		// the block. The block stays transient and holds the input in its
		// preview; the first non-blank content materializes both.
		work.md.keepFinalEOLCaret = true
		work.md.preview = true
		t.preview = work
		t.marker = marker
		t.write = workCaret
		v.showTransient(t)
		return true
	}
	heldAt := -1
	if breakAt >= 0 {
		if hard := heldBreakEnd(previewSrc, virtual, at, breakAt, work.text.ParagraphCount(), state); hard >= 0 {
			// 内容落到了持有空行的下一行，且重解析确认空行与内容仍在
			// 同一段：在空行行尾补一个反斜杠硬换行，保存重开得到与
			// live 相同的可见结构（工单 56 探针：seed8/typed-after-
			// empty-list-continuation-break、seed9/c17）。重解析不成立
			// 就不写：块级起始会把反斜杠留成独立的字面段落。
			virtualBeforeHard := virtual
			virtual = virtual[:hard] + "\\" + virtual[hard:]
			if caret > hard {
				caret++
			}
			state = state.mapPatch(hard, hard, 1, virtualBeforeHard, virtual)
			heldAt = hard
		}
	}
	from, to, newEnd := changedBytes(base, virtual)
	replacement := virtual[from:newEnd]
	d.lock()
	v.transient = nil
	v.version = 0
	v.mode = newStep
	v.coalesce = false
	undo := t.undo(base)
	beforeUndo := len(d.undo)
	// The saved source can add an empty paragraph after a final block, which
	// the incremental projection cannot represent but a reparse can.
	var editErr error
	if editErr = v.lockedEdit(func(e *Edit) {
		if from != to || replacement != "" {
			if !e.replaceMarkdownSource(from, to, replacement) {
				err = ErrStructuralEdit
				return
			}
		}
		// The transient was typed in a preview holding literal lines the
		// canonical source does not parse as blocks: parse the materialized
		// source under them, as the preview did.
		d.setState(state.restoreProjection(d.state()))
		if !e.reparseMarkdownSource() {
			err = ErrStructuralEdit
			return
		}
		e.selectMarkdownSource(caret)
	}); editErr != nil {
		err = editErr
	}
	if err == nil && len(d.undo) == beforeUndo+1 {
		d.undo[len(d.undo)-1].transient = undo
	}
	if err == nil && heldAt >= 0 {
		// 这一段反斜杠是本会话为持有空行写的，记下归属：块级语法、
		// 空行或删光内容一旦让它成为独立段落，编辑结束时按重解析
		// 结果删掉（sweepHeldBreaks）。
		state := d.state()
		state.heldBreaks = append(state.heldBreaks, heldAt)
		d.setState(state)
		if n := len(d.undo); n > 0 {
			d.undo[n-1].sessionAfter = d.syncFromParser()
		}
	}
	// What is typed after the materialized paragraph joins its step, as one
	// contiguous run of input.
	if err == nil {
		d.coalesce = true
	}
	// The materialized content closes the attributes the transient
	// captured. The next keystroke takes them from its caret, which can
	// stand outside the run that content wrote, as after a paste ending in
	// a closing marker: there it writes past the marker (record
	// 2026-11-10, seed4/c24).
	d.mu.Unlock()
	if err != nil {
		v.transient = t
		v.showTransient(t)
		return false
	}
	v.selAnchor, v.caret = d.anchor, d.caret
	v.selVersion = d.selVersion
	v.mode, v.coalesce, v.coalesceKind = newStep, true, coalesceTyping
	// The run wrote this line's bytes: a prefix it completes here began its
	// hold inside the run, so the step's Undo reads the bytes as text again
	// (record 2026-11-12, seed14/c8).
	line := liveLineAt(d.md.p.String(), d.selection.Caret.Offset)
	v.pfx.wroteLine = line.start
	v.reveal = true
	return true
}

func transientMarker(src string) string {
	for r := rune(0xE000); r <= 0xF8FF; r++ {
		if marker := string(r); !strings.Contains(src, marker) {
			return marker
		}
	}
	return "\uE000"
}

func removeTransientMarker(src, marker string) (string, int, bool) {
	at := strings.Index(src, marker)
	if at < 0 || strings.Index(src[at+len(marker):], marker) >= 0 {
		return src, 0, false
	}
	return src[:at] + src[at+len(marker):], at, true
}

func normalizeTransientEnd(src string, markerAt int, t *transientBlock) string {
	if !t.atEOF {
		return src
	}
	head := src[:markerAt]
	if t.finalEOL {
		if !strings.HasSuffix(head, "\n") && !strings.HasSuffix(head, "\r") && !(t.isEmptyParagraph() && quoteTailLine(head)) {
			head += defaultEOL(src)
		}
	}
	return head
}

// quoteTailLine reports whether src's last line is a quote's own continuation
// line: quote markers with nothing after them. Such a line cannot end the
// document's bytes: the line ending after it would open an empty quote line
// the session never typed, so the bytes end on the line itself (record
// 2026-11-13, zz177/A-empty-line-enter: Enter's continuation reads back as
// `> a\n>\n> `).
func quoteTailLine(src string) bool {
	line := markdownLine{start: strings.LastIndexAny(src, "\r\n") + 1, end: len(src)}
	body := src[line.start:line.end]
	return strings.Contains(body, ">") && quoteOnly(body)
}

// heldBreakEnd 返回补持有空行硬换行反斜杠的位置，重解析判定不成立
// 时返回 -1。源码位置只说明持有行和内容在哪；反斜杠是否仍是两者
// 之间的硬换行只能由解析决定，块级语法（`#`、`- `、`> `）与空行
// 都会让它不成立。
func heldBreakEnd(previewSrc, virtual string, write, breakEnd, paragraphs int, state sessionState) int {
	if breakEnd <= 0 || breakEnd > len(previewSrc) || breakEnd >= len(virtual) {
		return -1
	}
	if virtual[:breakEnd] != previewSrc[:breakEnd] {
		return -1
	}
	line := lineAt(virtual, breakEnd)
	if line.end != breakEnd || strings.TrimSpace(virtual[line.start:line.end]) != "" {
		// 持有行不是空的：它已经有内容，硬换行没有要补的。
		return -1
	}
	next := lineAt(virtual, line.stop)
	if next.start != line.stop || write < next.start || write > next.end {
		// 首个内容没有落到持有行下面紧邻的一行。
		return -1
	}
	if strings.TrimSpace(virtual[next.start:next.end]) == "" {
		return -1
	}
	candidate := virtual[:breakEnd] + "\\" + virtual[breakEnd:]
	parsed := newMarkdownDocument(candidate, state.mapPatch(breakEnd, breakEnd, 1, virtual, candidate))
	if parsed.text.ParagraphCount() != paragraphs {
		// 反斜杠把内容切出了新块（标题、列表、引用）：live 里空行
		// 被块级语法吸收，补上的反斜杠会成为字面段落。
		return -1
	}
	if !parsed.heldBreakCarriesContent(breakEnd) {
		return -1
	}
	return breakEnd
}

// heldBreakCarriesContent 报告 at 处的反斜杠是否仍在一行行尾，且
// 下一行是同一段落里的非空内容。只有这时反斜杠才代表持有空行；
// 段落若在反斜杠行结束，它就是自带字面反斜杠的独立段落。
func (d *Document) heldBreakCarriesContent(at int) bool {
	if d.md == nil {
		return false
	}
	src := d.md.p.String()
	if at < 0 || at >= len(src) || src[at] != '\\' {
		return false
	}
	line := lineAt(src, at)
	if line.end != at+1 || line.eol == "" {
		return false
	}
	next := lineAt(src, line.stop)
	if next.start >= next.end || strings.TrimSpace(src[next.start:next.end]) == "" {
		return false
	}
	m := d.md.m
	k := m.from(at)
	for p := max(0, k-1); p < m.Len() && p <= k; p++ {
		start, end, _ := m.Paragraph(p)
		if start <= at && at < end {
			if end < next.end {
				return false
			}
			st := d.text.ParagraphStyleAt(d.text.paraStart(p))
			return !st.CodeBlock && st.Heading == 0
		}
	}
	return false
}

// sweepHeldBreaks 删掉本会话写的、下面已经没有内容的硬换行反斜杠。
// 它在每次编辑后、同一个撤销步内运行：让块级语法或删光内容一起
// 带走 live 投影和保存字节里的反斜杠，撤销时两者一起回来。
func (d *Document) sweepHeldBreaks(e *Edit) {
	state := d.state()
	for i := len(state.heldBreaks) - 1; i >= 0; i-- {
		at := state.heldBreaks[i]
		if d.heldBreakCarriesContent(at) {
			continue
		}
		src := ""
		if d.md != nil {
			src = d.md.p.String()
		}
		if at < 0 || at >= len(src) || src[at] != '\\' {
			// 这个偏移已经没有本会话写的反斜杠：去掉登记。
			state.heldBreaks = append(state.heldBreaks[:i], state.heldBreaks[i+1:]...)
			continue
		}
		e.replaceMarkdownSource(at, at+1, "")
	}
	d.setState(state)
}

// materializeExitParagraphOnEnter writes the paragraph an empty container
// exited to when Enter is pressed on it, and keeps the empty paragraph the
// new line opens in the view. Typora's empty paragraph after a container is
// a block in the document, so Enter materializes it instead of only
// extending the view; Undo restores the transient paragraph.
func (v *view) materializeExitParagraphOnEnter() bool {
	t := v.transient
	if t == nil || t.mode != transientContainerExit || t.isLineBreak() || !t.atEOF {
		// A quoted exit keeps its quote line until the next Enter lifts
		// it; only the plain paragraph after a container materializes
		// here.
		return false
	}
	if t.marker != "" && !t.previewLastParagraphPlain() {
		// The marker spells an empty quote line, which Enter lifts instead
		// of writing: only a marker standing for a plain paragraph outside
		// its container materializes here (record 2026-11-13,
		// zz177/A-empty-line-enter).
		return false
	}
	if t.preview == nil {
		return false
	}
	if t.preview.Source() == v.doc.Source() {
		// Nothing of the paragraph is missing from the bytes. It stands on the
		// document's final line ending when the tail of a quote exited to it,
		// and Enter writes that ending as the paragraph's own blank line before
		// it opens the next one (record 2026-11-13, zz177/A-empty-line-enter
		// cp2); everywhere else only the next empty paragraph is opened.
		base := v.doc.Source()
		eol := defaultEOL(base)
		if t.marker != "" || !t.previewLastParagraphPlain() || !strings.HasSuffix(base, eol) || !v.doc.lastBlockIsQuote() {
			return false
		}
		v.mode, v.coalesce = newStep, false
		if err := v.editSource(len(base), func(e *Edit) {
			e.ReplaceSource(len(base), len(base), eol)
		}); err != nil {
			return false
		}
		if !v.holdParagraphBelowQuoteTail(len(base) + len(eol)) {
			return false
		}
		return true
	}
	plainParagraph := t.marker != "" && t.previewLastParagraphPlain()
	if !v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
		return false
	}
	if plainParagraph {
		// The marker spelled a plain paragraph below a quote's tail, and the
		// bytes now end on its line. Opening the next paragraph through the
		// intent would read the quote above and spell a quote line again; the
		// paragraph below the quote stands outside it (record 2026-11-13,
		// zz177/A-empty-line-enter cp2-cp3).
		v.holdParagraphBelowQuoteTail(len(v.doc.Source()))
		v.rememberHeldParagraph()
		return true
	}
	p := v.shown.ParagraphCount() - 1
	if !v.openTransient(IntentEnter, v.paragraphEndSource(p), true) {
		return false
	}
	v.rememberHeldParagraph()
	return true
}

// rememberHeldParagraph records the paragraph a step leaves in the view as
// what the step produced, so Redo brings it back with the bytes.
func (v *view) rememberHeldParagraph() {
	next := v.transient
	v.doc.lock()
	if n := len(v.doc.undo); n > 0 && next != nil {
		v.doc.undo[n-1].afterTransient = next.undo(v.doc.md.p.String())
	}
	v.doc.mu.Unlock()
}

// holdParagraphBelowQuoteTail opens the empty paragraph Enter leaves below a
// quote's tail. The bytes end on the blank line the paragraph stands on, and
// the paragraph the next input writes is a plain one: opening it through the
// intent would read the quote above and spell a quote line instead (record
// 2026-11-13, zz177/A-empty-line-enter cp2-cp3).
func (v *view) holdParagraphBelowQuoteTail(at int) bool {
	src := v.doc.Source()
	if at < 0 || at > len(src)+2 || strings.TrimSpace(src) == "" {
		return false
	}
	marker := transientMarker(src)
	// The empty paragraph the bytes end on keeps its own line and needs the
	// blank line below it before the next paragraph's content: the first
	// input writes that line and the content under it (record 2026-11-13,
	// zz177/A-empty-line-enter cp2-cp3).
	eol := defaultEOL(src)
	blank := eol + eol
	preview := v.previewDocumentInserted(src+blank+marker, len(src), len(blank)+len(marker))
	at = len(src) + len(blank)
	preview.SetSourceSelection(sourceCaret(at))
	preview.md.keepFinalEOLCaret = true
	t := &transientBlock{
		preview:  preview,
		anchor:   sourcePoint(at, BoundaryDownstream),
		write:    at,
		atEOF:    true,
		finalEOL: true,
		mode:     transientContainerExit,
		marker:   marker,
	}
	v.showTransient(t)
	return true
}

// keepEmptyContainer keeps the retained empty inline pair when the
// projection now shown still holds its bytes, and drops it otherwise. A
// transient swaps the view's text and source map without changing the
// document, so the pair the parser cannot read (`****`) survives both
// showing and dropping it (record 2026-11-10, seed4/c18).
func (v *view) keepEmptyContainer() {
	e := v.empty
	if e == nil {
		return
	}
	if e.para < 0 || e.para >= v.source.Len() {
		v.empty = nil
		return
	}
	start, end, _ := v.source.Paragraph(e.para)
	src := v.sourceText()
	if e.box.start < start || e.box.end > end || e.box.end > len(src) ||
		e.box.start > e.box.end {
		v.empty = nil
		return
	}
	at := v.shown.paraStart(e.para) + e.box.docStart
	open := e.box.openEnd - e.box.openStart
	closing := e.box.closeEnd - e.box.closeStart
	if open < 0 || closing < 0 || at-open < 0 || at+closing > v.shown.Len() ||
		v.shown.Slice(at-open, at+closing).String() != src[e.box.start:e.box.end] {
		v.empty = nil
	}
}

// handleTransientKey applies the key to the transient's lifecycle mode.
// It reports whether the transient handled the key and whether the key's
// command path is already done.
func (v *view) handleTransientKey(key ui.Key, mods ui.Modifiers) (handled, done bool) {
	if key == ui.KeyDelete && mods == 0 && seed24HeldBreakDeleteIntent(v.transient) &&
		seed24HeldBreakKeepTransient(v) {
		// Delete at the held hard break's tail leaves Typora's final EOL and
		// the visible empty line in place; materializing the transient here
		// would let the last-block Delete strip that EOL
		// (record-20261114-153934 / 154454, seed24/c2).
		return true, true
	}
	if key == ui.KeyEscape && mods == 0 && v.transient.isEmptyParagraph() &&
		v.transient.isViewOnlyLine() && v.transient.atEOF {
		// Escape leaves the empty paragraph an unchanged block exit
		// opened after the last block where it is (record 2026-11-11,
		// probe92b/down-escape cp3).
		return true, true
	}
	if v.transient.isTrailingSplit() && !typesText(key, mods) &&
		v.shown.paraAt(v.caret) != v.transient.para {
		// The held paragraph stands after the caret: a key acts where
		// the caret is, and the reparse that follows decides the lines.
		// A printable key leaves the hold to the text event after it.
		v.holdTrailingForUndo()
		return false, true
	}
	m := mods &^ ui.Shift
	enter := key == ui.KeyEnter && (m == 0 || m == ui.Cmd || runtime.GOOS != "darwin" && m == ui.Ctrl)
	switch {
	case enter && v.transient.isViewOnlyLine() && mods&ui.Shift == 0:
		// Enter on the line past the document's last block writes the
		// empty paragraph the line showed and opens the next one.
		if v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			p := v.shown.ParagraphCount() - 1
			v.openTransient(IntentEnter, v.paragraphEndSource(p), true)
			return true, true
		}
		v.discardTransient()
		return false, true
	case enter && v.transient.isLineBreak():
		if m == 0 && mods&ui.Shift != 0 {
			if v.heldBreakEndsParagraph() {
				return true, true
			}
			v.extendTransient(IntentShiftEnter)
			return true, true
		}
		// The break Shift+Enter opened on an empty item's own line is the
		// item's continuation line: Enter writes it and then ends the item,
		// so the next empty item stands under it. Dropping the break alone
		// would exit the list from the empty item it left (record
		// 2026-11-13, seed16/c12).
		if m == 0 && v.materializeHeldBreakOnEmptyItem() {
			return true, true
		}
		// Any other Enter leaves the pending break behind and acts on
		// the paragraph as it lies in the bytes.
	case enter:
		intent := IntentEnter
		if m == ui.Cmd || runtime.GOOS != "darwin" && m == ui.Ctrl {
			intent = IntentExitBlock
		} else if mods&ui.Shift != 0 {
			// Shift+Enter opens a line break inside the empty paragraph
			// or item the transient holds; it is not Enter's lift out of
			// the container (record 2026-11-10, seed4/c17).
			intent = IntentShiftEnter
		}
		if intent == IntentEnter && v.materializeExitParagraphOnEnter() {
			return true, true
		}
		if intent == IntentEnter && v.materializeDeferredItemExit() {
			return true, true
		}
		if v.extendTransient(intent) {
			return true, true
		}
	}
	if key == ui.KeyTab && m == 0 && v.transient.isEmptyParagraph() {
		outdent := mods&ui.Shift != 0
		intent := IntentIndent
		if outdent {
			intent = IntentOutdent
		}
		if v.extendTransient(intent) {
			return true, true
		}
		if outdent && v.transient.isEmptyParagraph() {
			if st := v.para(v.transient.para).style; st.List == NoList && !st.Quote &&
				st.Heading == 0 && !st.CodeBlock {
				// A paragraph has no indent to leave: Typora keeps the
				// transient paragraph and writes nothing (record
				// 2026-11-10, seed4/c25).
				return true, true
			}
		}
		// A paragraph has no indent the intent could write: Typora
		// materializes the empty block and keeps the tab as its text.
		// The tab is the materialization's first content, so it lands on
		// the line the empty block opens rather than at the caret the
		// reparse can only place at the blank run's start.
		if !outdent && v.transient.isEmptyParagraph() {
			if st := v.para(v.transient.para).style; st.List == NoList && !st.Quote &&
				st.Heading == 0 && !st.CodeBlock {
				text := "\u200b\t"
				if v.caret != v.shown.paraStart(v.transient.para) {
					text = "\t"
				}
				if v.materializeTransient(func(e *Edit, at int, attrs Attributes) error {
					e.ReplaceString(at, at, text, attrs)
					return e.Err()
				}, false) {
					return true, true
				}
			}
		}
	}
	if key == ui.KeyTab && m == 0 && v.transient.isLineBreak() {
		if v.indentPendingBreak(mods&ui.Shift != 0) {
			return true, true
		}
		if mods&ui.Shift == 0 && v.materializePendingBreakTab() {
			return true, true
		}
	}
	if v.transient.isEmptyParagraph() && v.transient.atEOF &&
		(key == ui.KeyDown || key == ui.KeyRight) && (mods == 0 || mods == ui.Shift) {
		// The document's trailing empty block is the last line: a move
		// forward cannot leave it, and Typora keeps it view-held even
		// while the move extends the selection (seed9/c16).
		return true, true
	}
	if v.transient.isEmptyParagraph() && v.transient.atEOF && v.transient.isViewOnlyLine() &&
		key == ui.KeyUp && mods == 0 && v.selAnchor == v.caret {
		// Up on the line past the document's last block steps back to the
		// line that opened it: the line holds no bytes, so the move lands
		// at the paragraph's end and is spent there, instead of climbing
		// another line from the paragraph the caret just returned to
		// (record 2026-11-12, seed15/c2).
		v.discardTransient()
		return true, true
	}
	if key == ui.KeyUp && mods == 0 && v.selAnchor == v.caret &&
		v.transient.isEmptyParagraph() && v.transient.atEOF && !v.transient.isViewOnlyLine() &&
		v.emptyParagraphAboveCaret() {
		// Up from the empty paragraph at the document's end writes it and
		// leaves the caret in the empty paragraph above, as Typora does: the
		// block below stays in the view without bytes of its own (record
		// 2026-11-12, seed18/c8).
		if v.holdMaterializedTail() {
			v.hasDesired = false
			return true, true
		}
	}
	materialized := false
	if key == ui.KeyBackspace || key == ui.KeyDelete {
		// Typora's empty paragraph is a block in the document, so a
		// deletion at its edges edits those bytes instead of dropping
		// the empty block. The line past the document's end holds none.
		if v.transient.isViewOnlyLine() {
			v.discardTransient()
			return true, true
		}
		if key == ui.KeyBackspace && m == 0 && v.transient.isEmptyParagraph() &&
			v.transient.atEOF && v.doc.Source() == "" {
			// The paragraph break Backspace joins across has no bytes:
			// the empty paragraph is the document's own, and Typora
			// keeps it in the view (record 2026-11-10, seed4/c28).
			v.discardTransient()
			return true, true
		}
		if key == ui.KeyBackspace && m == 0 && v.transient.isEmptyParagraph() &&
			v.transient.mode == transientContainerExit && v.transient.atEOF && v.selAnchor == v.caret &&
			v.caret == v.shown.paraStart(v.transient.para) {
			// Backspace at the start of the empty paragraph an empty item
			// or quote line exited to joins it back onto the block above.
			// The break was never written, so the join keeps the block's
			// own type: a list item keeps its marker and the caret stands
			// at its content end (record 2026-11-10, seed9/c16). The exit
			// mark tells this paragraph from the empty item Enter opened
			// at the end of a text line, which Backspace writes and joins
			// with its bytes (diff.json 0a398bfb).
			v.discardTransient()
			v.moveCaret(v.shown.Len(), ui.TextDownstream, false)
			return true, true
		}
		if key == ui.KeyBackspace && m == 0 && v.backspaceHeldTailBreak() {
			// Backspace on the break Shift+Enter opened in the view-held
			// tail paragraph deletes the break and leaves the paragraph
			// where it stood (record 2026-11-12, seed13/c9).
			return true, true
		}
		if key == ui.KeyBackspace && m == 0 && v.backspaceHeldQuoteBreak() {
			// Backspace on the break Enter opened inside a quote deletes the
			// break's line with its bytes: `> a\n> b\n` leaves `> a` (record
			// 2026-11-13, zz177/bs-multiline).
			return true, true
		}
		if key == ui.KeyBackspace && m == 0 && v.transientRecoveryClosedTail() {
			// The paragraph above ends on a closing delimiter the view
			// hides: taking the empty paragraph back would read the pair as
			// text, so the bytes stay and the paragraph leaves the view
			// (record 2026-11-13, seed22/c1).
			return true, true
		}
		if key == ui.KeyDelete && m == 0 && v.deleteInHeldTailParagraph() {
			// Delete in the view-held empty paragraph at the document's end
			// writes nothing: the paragraph holds no bytes for it to take,
			// and Typora keeps the block where it stands (record
			// 2026-11-13, seed17/c16).
			return true, true
		}
		materialized = v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true)
		if !materialized {
			v.discardTransient()
			return true, true
		}
	}
	if !materialized && v.transient.isLineBreak() && m == 0 &&
		key == ui.KeyLeft && mods&ui.Shift != 0 {
		// Extending back from the held line keeps the break in the view:
		// the selection starts on the line the break holds and the caret
		// steps onto the text line before it (record 2026-11-10,
		// seed8/c14).
		v.extendHeldBreak()
		return true, true
	}
	forward := key == ui.KeyDown || key == ui.KeyRight
	if !materialized && v.transient.isLineBreak() &&
		(mods == 0 || mods == ui.Shift && forward) {
		switch key {
		case ui.KeyDown, ui.KeyRight:
			// The empty line after the break is the paragraph's last
			// line: a move forward cannot leave it, and Shift only
			// extends a selection that already ends there (record
			// 2026-11-12, seed15/c15).
			return true, true
		case ui.KeyLeft:
			// The break is the paragraph's own line: a move back onto
			// the text line only puts the caret before it, and the
			// break stays (diff.json fc3db91f).
			if v.displayNavigation() {
				v.stepDisplay(true, false)
			} else {
				i, aff := v.visual(-1)
				v.stepLiteral(i, aff, false)
			}
			v.hasDesired = false
			v.syncLineBreakCaret()
			return true, true
		case ui.KeyUp:
			// A move back reaches the text line's end and keeps the
			// break in the paragraph: Typora shows the block through the
			// line ending after Shift+Enter and Up, and Down returns to
			// the line the break holds (VM record 2026-11-12-151635,
			// probe110b/shift-enter-end-then-up and seed12/c3).
			if v.displayNavigation() {
				v.stepDisplay(true, false)
			} else {
				i, aff := v.visual(-1)
				v.stepLiteral(i, aff, false)
			}
			v.hasDesired = false
			v.syncLineBreakCaret()
			return true, true
		}
	}
	if !materialized && key == ui.KeyLeft && mods&ui.Shift != 0 &&
		(m == 0 || runtime.GOOS == "darwin" && m == ui.Alt) &&
		v.transient.isEmptyParagraph() && v.transient.isViewOnlyLine() && v.transient.atEOF {
		// The line past the document's bytes is no block of the document:
		// Shift+Left does not extend the selection into the paragraph
		// above. Typora drops the line from its model and leaves the
		// selection at the document's end, outside every block (record
		// 2026-11-11, seed10/c15). Option+Shift+Left reaches the same
		// state: the word move has no line to act on (record 2026-11-11,
		// probe77/final-eol-alt-shift-left). The view keeps the line under
		// the caret for the input that follows.
		v.transient.mode = transientDetachedEOF
		return true, true
	}
	if !materialized && key == ui.KeyLeft && mods&ui.Shift != 0 &&
		(m == 0 || runtime.GOOS == "darwin" && m == ui.Alt) &&
		v.transient.isEmptyParagraph() && v.transient.atEOF &&
		v.transient.mode != transientContainerExit && !v.transient.isTrailingSplit() && v.transient.marker == "" &&
		v.transientHoldsPlainParagraph() && v.holdTrailingForUndo() {
		// Backspace took the last rune of the final EOL before Enter
		// opened this paragraph: Typora keeps it and folds the
		// backward selection at 2:0 (record 2026-11-14, seed23/c14).
		return true, true
	}
	if !materialized && key == ui.KeyLeft && mods&ui.Shift != 0 &&
		(m == 0 || runtime.GOOS == "darwin" && m == ui.Alt) &&
		v.transient.isEmptyParagraph() && v.transient.atEOF &&
		v.transient.mode != transientContainerExit && !v.transient.isTrailingSplit() && v.transient.marker == "" &&
		v.transientHoldsPlainParagraph() {
		// The empty paragraph Enter opened at the document's end holds no
		// bytes, and the line above it ends the content: Shift+Left does
		// not extend the selection into that paragraph. Typora selects the
		// paragraph break alone, which shows as nothing, and typing then
		// replaces it: the run lands after the paragraph's last byte, not
		// inside the inline element that ends it (record 2026-11-13,
		// seed19/c10).
		v.discardTransient()
		v.moveCaret(v.shown.Len(), ui.TextDownstream, false)
		v.hasDesired = false
		return true, true
	}
	if !materialized && mods == 0 && key == ui.KeyLeft &&
		v.transient.isEmptyParagraph() && v.transient.atEOF &&
		v.transient.isViewOnlyLine() {
		// The line past the document's last block has no bytes to
		// write, with or without the file's final line ending: moving
		// back out of it drops the line and leaves the caret at the end
		// of the content above, without stepping over a glyph of it
		// (record 2026-11-10, seed4/c18; record 2026-11-12,
		// seed18/c2: the nested item's caret stays after its `beta`).
		// A list item's marker is a real block: its line stays, and the
		// materialization below writes it (diff.json a6950ae9).
		v.discardTransient()
		v.moveCaret(v.shown.Len(), ui.TextDownstream, false)
		v.hasDesired = false
		return true, true
	}
	if !materialized && runtime.GOOS == "darwin" && m == ui.Super && key == ui.KeyLeft &&
		v.transient.isEmptyParagraph() && v.transient.isViewOnlyLine() && v.transient.atEOF {
		// Cmd+Left moves to the start of the caret's line. The line past
		// the document's bytes starts where the caret already stands and
		// is no block of the document, so Typora keeps the line and the
		// selection where they are (record 2026-11-11,
		// probe77/final-eol-cmd-shift-left).
		return true, true
	}
	if !materialized && runtime.GOOS == "darwin" && mods == ui.Alt && key == ui.KeyLeft &&
		v.transient.isEmptyParagraph() && v.transient.isViewOnlyLine() && v.transient.atEOF {
		// Option does not word-move on the line past the document's
		// bytes: the move drops the line and stands at the end of the
		// paragraph above (record 2026-11-11, probe77/final-eol-alt-left).
		v.discardTransient()
		v.moveCaret(v.shown.Len(), ui.TextDownstream, false)
		v.hasDesired = false
		return true, true
	}
	// Paste materializes the transient paragraph together with its
	// first content. A printable key leaves it for the text event that
	// follows on macOS and Windows. Undo and redo decide in editCommand;
	// other commands and navigation write the empty block's bytes, as
	// Typora does when the caret leaves it, instead of dropping the
	// block the user still sees.
	history := (key == ui.KeyZ || key == ui.KeyY) && mods&ui.Cmd != 0
	// Cmd+B writes the empty strong pair into the transient paragraph
	// itself, as Typora does, rather than materializing the empty block
	// and formatting the position before its blank lines (diff.json
	// b609e7fa).
	bold := false
	if cmd, ok := shortcut(key, mods); ok && cmd == "bold" &&
		v.transient != nil && !v.transient.isViewOnlyLine() {
		// Cmd+B writes the pair into the block or the held line the
		// caret stands on, so neither is dropped before the command
		// (record 2026-11-12-133304, seed12/c4).
		bold = v.transient.isEmptyParagraph() ||
			v.transient.isLineBreak()
	}
	if !materialized && !history && !bold && (key != ui.KeyV || mods&ui.Cmd == 0) && !typesText(key, mods) {
		// Only an empty block is a block in Typora's document. A
		// pending line break is a view state the next action leaves
		// behind without writing its bytes.
		if v.transient.isLineBreak() || !v.transient.writesBytesOnBlur() || key == ui.KeyEnter ||
			!v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
			v.discardTransient()
		}
	}
	return false, false
}
