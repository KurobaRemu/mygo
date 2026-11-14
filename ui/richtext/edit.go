package richtext

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// Every edit of the editor goes through its document, as one of its steps
// of undo, so that undoing it brings its attributes back with its text.

// lockedPublish gives the document the editor's selection; the caller
// holds its lock. A selection moved forgets the typing attributes set at
// the caret, as in NSTextView.
func (v *view) lockedPublish() {
	d := v.doc
	if v.transient != nil &&
		v.shown.paraAt(v.selAnchor) == v.transient.para &&
		v.shown.paraAt(v.caret) == v.transient.para {
		// A transient paragraph has no source bytes: its caret is the
		// source anchor, not a projection through the canonical document,
		// where the virtual paragraph does not exist.
		sel := sourceCaret(v.transient.anchor.Offset)
		if d.selection != sel {
			d.assignSourceSelection(sel, false)
		}
		return
	}
	if d.anchor != v.selAnchor || d.caret != v.caret {
		d.assignSourceSelection(SourceSelection{
			Anchor: v.sourcePositionLocked(v.selAnchor, v.anchorAffinity),
			Caret:  v.sourcePositionLocked(v.caret, v.caretAffinity),
		}, false)
	}
}

// publish gives the document the editor's selection.
func (v *view) publish() {
	v.doc.lock()
	defer v.doc.mu.Unlock()
	v.lockedPublish()
}

func (v *view) lockedPublishSource() {
	v.lockedPublish()
}

func (v *view) sourcePositionLocked(doc int, affinity BoundaryAffinity) SourcePoint {
	d := v.doc
	if doc == d.caret {
		return sourcePoint(d.selection.Caret.Offset, affinity)
	}
	if doc == d.anchor {
		return sourcePoint(d.selection.Anchor.Offset, affinity)
	}
	if d.md == nil {
		return sourcePoint(max(0, min(doc, d.text.Len())), affinity)
	}
	return sourcePoint(v.sourceForDoc(doc, affinity == BoundaryDownstream), affinity)
}

func (v *view) setSourceSelectionLocked(sel SourceSelection) {
	v.doc.assignSourceSelection(sel, false)
	v.selAnchor, v.caret = v.doc.anchor, v.doc.caret
}

func (v *view) setSourceSelection(sel SourceSelection) {
	v.doc.lock()
	defer v.doc.mu.Unlock()
	v.setSourceSelectionLocked(sel)
}

func (v *view) setCaretLocked(doc int, affinity BoundaryAffinity, extend bool) {
	d := v.doc
	caret := v.sourcePositionLocked(doc, affinity)
	anchor := caret
	if extend {
		anchor = d.selection.Anchor
	}
	v.anchorAffinity = anchor.Affinity
	v.caretAffinity = affinity
	v.setSourceSelectionLocked(SourceSelection{
		Anchor: anchor,
		Caret:  caret,
	})
}

func (v *view) setSelectionLocked(anchor, caret int, anchorAffinity, caretAffinity BoundaryAffinity) {
	v.anchorAffinity, v.caretAffinity = anchorAffinity, caretAffinity
	v.setSourceSelectionLocked(SourceSelection{
		Anchor: v.sourcePositionLocked(anchor, anchorAffinity),
		Caret:  v.sourcePositionLocked(caret, caretAffinity),
	})
}

// record starts an action of the editor: typing joins the step typing
// made last, anything else starts a step of its own.
func (v *view) record(typing bool) {
	if !typing {
		// A run of typing ends here: a prefix the next input completes is its
		// own step's promotion, not this run's (record 2026-11-12, seed14/c8).
		v.pfx.wroteLine = -1
	}
	if typing && v.coalesce && v.coalesceKind == coalesceTyping {
		v.mode = typingStep
	} else {
		v.mode = newStep
	}
	v.coalesce, v.coalesceKind = typing, coalesceNone
	if typing {
		v.coalesceKind = coalesceTyping
	}
}

// recordDelete starts a deletion. A deletion after a typing run starts its
// own step: undoing Backspace after typing restores the typed runes (record
// 2026-11-10, probe/undo/type-backspace, 4/4 replays). Consecutive
// deletions join one step, and typing after a deletion starts its own
// (record 2026-11-10, probe/undo/backspace-backspace and seed4/c12).
func (v *view) recordDelete() {
	// A deletion ends the run of typing: the bytes it takes back are not a
	// prefix this run wrote (record 2026-11-12, seed14/c8).
	v.pfx.wroteLine = -1
	if v.coalesce && v.coalesceKind == coalesceDelete {
		v.mode = typingStep
	} else {
		v.mode = newStep
	}
	v.coalesce, v.coalesceKind = true, coalesceDelete
}

// replace replaces the runes from start to end with s in the typing
// attributes, as part of the action's step of undo, and puts the caret
// after it.
func (v *view) replace(start, end int, s string) {
	if pending := v.seed24FullSelectInserted(s); pending != nil {
		v.replaceSource(pending.from, pending.to, pending.text, pending.from)
		v.seed24FullSelectFinish()
		return
	}
	if v.transient != nil {
		if v.transient.isTrailingSplit() && v.shown.paraAt(v.caret) != v.transient.para {
			// The held paragraph stands after the caret's own: the content
			// goes into the paragraph the caret is in, and the step's undo
			// puts the held line back.
			v.holdTrailingForUndo()
		} else if v.materializeTransient(func(e *Edit, at int, _ Attributes) error {
			return e.markdownIntent(EditRequest{Intent: IntentPastePlain, Start: at, End: at, Text: s})
		}, v.transient.isEmptyParagraph() && v.transient.atEOF &&
			v.transient.finalEOL && defaultEOL(v.doc.Source()) == "\n" &&
			s == " ") {
			return
		}
	}
	defer v.doc.clearPendingTransient()
	if s == "" && start < end {
		if v.replaceEmptiedLineBreak(start, end) {
			return
		}
		if v.replaceEmptiedSentinelLine(start, end) {
			return
		}
		if v.replaceEmptiedTailParagraph(start, end) {
			return
		}
		if b, ok := v.contentBoxFor(start, end); ok {
			v.replaceBoxContent(b)
			return
		}
	}
	v.doc.lock()
	rawSource := v.doc.typing == nil
	srcCaret := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	if s != "" && start == end && start == v.caret {
		// The line opened with the invisible U+200B Tab writes to keep its tab
		// text; text typed in front of the byte makes the tab a tab among text,
		// which Typora writes without it. The byte's removal and the insertion
		// are one patch, so the caret lands after what was typed rather than at
		// the end of a byte withdrawn in a step of its own (record 2026-11-13,
		// seed22/c3).
		if at, ok := v.sentinelLinePrefix(); ok && v.sourceAtDoc(start) == at {
			v.empty = nil
			v.replaceSource(at, at+len("\u200b"), s, at+len(s))
			return
		}
	}
	if v.doc.md != nil && start == end && s != "" {
		if from, to, insert, caret, ok := v.widenFenceForTypedRun(start, srcCaret, s); ok {
			v.replaceSource(from, to, insert, caret)
			return
		}
	}
	if from, to, insert, caret, ok := v.directSourceEdit(start, end, s, rawSource); ok {
		v.empty = nil
		v.replaceSource(from, to, insert, caret)
		return
	}
	if v.doc.md != nil && start == end {
		var from, caretIn int
		insert, ok := "", false
		emptyBox := !rawSource && v.emptyBoxAt(start)
		v.doc.lock()
		from = v.doc.selection.Caret.Offset
		if emptyBox && from >= 0 {
			// The paragraph has no rune of the box's content, so the
			// document's offsets cannot say the rune goes inside it: only
			// the source caret can. Typora writes it there.
			attrs := v.doc.typingAttributes()
			if v.doc.typing != nil {
				attrs = *v.doc.typing
			}
			insert, caretIn = formattedInsert(s, attrs)
			ok = true
		} else if rawSource {
			// The empty code line of a fenced block takes input through the
			// document: inserting there also writes the line ending that the
			// closing marker stands in for.
			ok = from >= 0 && !v.doc.codeEmptyAt(from)
			insert, caretIn = s, len(s)
			if ok {
				insert = v.doc.gapInsert(from, s)
			}
		}
		v.doc.mu.Unlock()
		if ok && v.replaceSourceErr(from, from, insert, from+caretIn) == nil {
			return
		}
	}
	d := v.doc
	d.lock()
	if c, ok := v.lockedSync(); ok {
		start = mapPosition(start, c.start, c.oldEnd-c.start, c.newEnd-c.start, false)
		end = mapPosition(end, c.start, c.oldEnd-c.start, c.newEnd-c.start, true)
	}
	v.lockedPublish()
	attrs := typingAttributes(d.text, start, end)
	if d.typing != nil {
		attrs = *d.typing
	}
	mode := v.mode
	fromSrc, collapsed := 0, d.anchor == d.caret
	if d.md != nil {
		fromSrc = d.selection.Caret.Offset
	}
	if err := v.lockedEdit(func(e *Edit) { e.ReplaceString(start, end, s, attrs) }); err != nil {
		// A paragraph the source holds only in its containers has no bytes
		// to format in place: the input goes at the source caret with the
		// markers its attributes need. A source patch that cannot carry the
		// paragraph's projection, as one whose rune turns a lazy line's text
		// into a block prefix, takes the same path: the source reparses and
		// the view follows it, as source-mode typing does.
		v.mode = mode
		d.mu.Unlock()
		if !v.replaceAttributedEmpty(start, end, s, attrs) && err == ErrUnwritable {
			v.replaceTypedAtSource(start, end, s, attrs)
		}
		return
	}
	caret := start + utf8.RuneCountInString(s)
	sel := d.selection
	sel.Anchor = sel.Caret
	// The edit mapped the source selection through its patch; its caret
	// point can stand inside the markers the edit wrote, as in an empty
	// link's text, or after them, as for a run it wrote in place. Either
	// way the caret belongs inside the run it typed, at its content end,
	// where the document offset cannot say.
	kept := false
	if collapsed && d.md != nil && s != "" {
		src := d.md.p.String()
		if insert, inner := formattedInsert(s, attrs); fromSrc >= 0 &&
			fromSrc+len(insert) <= len(src) && src[fromSrc:fromSrc+len(insert)] == insert {
			at := sourcePoint(fromSrc+inner, BoundaryDownstream)
			trial := SourceSelection{Anchor: at, Caret: at}
			if a, z := d.docSelectionForSourceLocked(trial); a == caret && z == caret {
				sel, kept = trial, true
			}
		}
	}
	if kept {
		d.selection = sel
		d.anchor, d.caret = caret, caret
	} else {
		if p := v.shown.paraAt(caret); p >= 0 && p < v.source.Len() {
			for _, b := range v.boxes(p) {
				if b.closeStart <= sel.Caret.Offset && sel.Caret.Offset <= b.closeEnd {
					sel.Caret.Offset = min(sel.Caret.Offset, b.closeStart)
				}
			}
			sel.Anchor = sel.Caret
		}
		if a, z := d.docSelectionForSourceLocked(sel); a == caret && z == caret {
			d.selection = sel
			d.anchor, d.caret = a, z
		} else if s == "" && start < end && d.caret != caret && d.caret <= d.text.Len() {
			// A join can leave its caret on a paragraph the deleted range did
			// not reach, as the empty paragraph a blank line still spells: the
			// edit's own caret is where the join puts it, not where the range
			// began (record 2026-11-13, seed18/c4).
			d.anchor = d.caret
		} else {
			d.assignSelection(caret, caret, false)
		}
	}
	v.selAnchor, v.caret = d.anchor, d.caret
	v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	if s == "" && start < end {
		// Typing again where text was deleted gets its attributes, until
		// the caret moves, as in NSTextView.
		keep := attrs
		if d.md != nil {
			keep = typingAttributes(d.text, start, start)
			keep.Attachment = nil
		}
		d.typing = &keep
	}
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	d.mu.Unlock()
}

// emptyBoxAt reports whether the source caret is where an empty box's
// content is, as an empty link's text: the projection holds no rune there,
// and the source caret is the only place that says input goes inside it.
func (v *view) emptyBoxAt(at int) bool {
	p := v.shown.paraAt(at)
	if p < 0 || p >= v.source.Len() {
		return false
	}
	v.doc.lock()
	source := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	for _, b := range v.boxes(p) {
		if b.contentFrom == b.contentTo && source == b.contentFrom {
			return true
		}
	}
	return false
}

// emptyAtCaret reports whether the caret stands inside the empty inline
// pair the view holds, where Enter drops the pair.
func (v *view) emptyAtCaret() bool {
	e := v.empty
	if e == nil || e.para < 0 || e.para >= v.shown.ParagraphCount() {
		return false
	}
	return v.caret == v.shown.paraStart(e.para)+e.box.docStart
}

// closingRunBeforeBreak finds the rune Enter takes when the caret stands
// strictly inside a closing delimiter run and the paragraph break waits in
// the view: the rune's source offset and size, or 0 for the size when the
// break is not that shape. A split that writes its own break takes the same
// rune in enterBlock; this path covers the paragraph end, where no split
// branch runs. The rune is located before the transient moves the caret to
// the paragraph below it (record 2026-11-13, seed20/c2).
func (v *view) closingRunBeforeBreak(intent EditIntent, shift bool, anchor, caret int) (at, size int) {
	if shift || intent != IntentEnter || v.opts.ReadOnly || v.doc == nil || v.doc.md == nil {
		return 0, 0
	}
	p := v.shown.paraAt(caret)
	if p < 0 || p >= v.source.Len() {
		return 0, 0
	}
	at = v.caretSource()
	if at < 0 {
		return 0, 0
	}
	v.doc.lock()
	src := v.doc.md.p.String()
	size = closingRunRuneSize(src, v.doc.md.m.boxes(p), at)
	v.doc.mu.Unlock()
	if size == 0 {
		return 0, 0
	}
	return at, size
}

// takeRuneAt writes the rune [at, at+size) away as the step the break
// started, so one Undo takes the break and the rune back together.
func (v *view) takeRuneAt(at, size int) {
	v.record(false)
	v.replaceSource(at, at+size, "", at)
}

// replaceAttributedEmpty writes typed text into a paragraph the source
// holds only in its containers, as the empty content of a list item or a
// quote: it has no bytes of its own to format, so the text goes where its
// content starts, with the markers its attributes need, as Typora writes
// it. A paragraph with bytes, or one the parser gives no paragraph of its
// own, is left to the document's own path.
func (v *view) replaceAttributedEmpty(start, end int, s string, a Attributes) bool {
	if start != end || s == "" || v.doc.md == nil {
		return false
	}
	p := v.shown.paraAt(start)
	if p < 0 || p >= v.source.Len() {
		return false
	}
	d := v.doc
	d.lock()
	ok := d.md != nil && p < d.md.m.Len()
	var from int
	if ok {
		pm := seqAt(d.md.m.paras, p)
		loc := d.paragraphLocation(p)
		ok = pm.src == 0 && !pm.codeEmpty && loc.leaf < 0
		from = d.selection.Caret.Offset
	}
	d.mu.Unlock()
	if !ok || from < 0 {
		return false
	}
	insert, inner := formattedInsert(s, a)
	return v.replaceSourceErr(from, from, insert, from+inner) == nil
}

// replaceTypedAtSource writes what was typed at the source caret when no
// source patch can carry the paragraph's projection, as when the rune turns
// a lazy line's text into a block prefix: the source reparses and the view
// follows it, as source-mode typing does.
func (v *view) replaceTypedAtSource(start, end int, s string, a Attributes) bool {
	if start != end || s == "" || v.doc.md == nil {
		return false
	}
	v.doc.lock()
	from := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	if from < 0 {
		return false
	}
	insert, inner := formattedInsert(s, a)
	return v.replaceSourceErr(from, from, insert, from+inner) == nil
}

func (v *view) replaceSource(from, to int, s string, caretSource int) {
	_ = v.replaceSourceErr(from, to, s, caretSource)
}

func (v *view) replaceSourceErr(from, to int, s string, caretSource int) error {
	return v.editSource(caretSource, func(e *Edit) {
		e.ReplaceSource(from, to, s)
	})
}

// replaceSourceReparse writes bytes whose structure the incremental
// projection cannot place and reparses the whole document instead. Only the
// block structure at the edit needs it, as when a paragraph opens above a
// code block's fence.
func (v *view) replaceSourceReparse(from, to int, s string, caretSource int) error {
	return v.editSource(caretSource, func(e *Edit) {
		if !e.replaceMarkdownSource(from, to, s) {
			e.fail(ErrStructuralEdit)
		}
	})
}

func (v *view) editSource(caretSource int, edit func(e *Edit)) error {
	if v.opts.ReadOnly {
		return nil
	}
	d := v.doc
	d.lock()
	defer d.mu.Unlock()
	v.lockedPublish()
	err := v.lockedEdit(func(e *Edit) {
		edit(e)
		if e.err == nil {
			at := v.docForSource(caretSource, true)
			e.Select(at, at)
			e.d.assignSourceSelection(sourceCaret(caretSource), false)
		}
	})
	if err != nil {
		return err
	}
	point := sourcePoint(caretSource, BoundaryDownstream)
	v.setSourceSelectionLocked(SourceSelection{Anchor: point, Caret: point})
	v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
	d.typing = nil
	return nil
}

func (v *view) replaceBoxContent(b box) {
	from, to := b.contentFrom, b.contentTo
	if from >= to {
		return
	}
	d := v.doc
	d.lock()
	p := v.shown.paraAt(v.caret)
	v.lockedPublish()
	var err error
	at := v.shown.paraStart(p) + b.docStart
	v.lockedEdit(func(e *Edit) {
		e.ReplaceSource(from, to, "")
		err = e.Err()
		if err == nil {
			e.Select(at, at)
		}
	})
	if err == nil {
		q := b
		delta := to - from
		q.closeStart -= delta
		q.closeEnd -= delta
		q.end -= delta
		q.contentFrom, q.contentTo = b.contentFrom, b.contentFrom
		q.open, q.close = -1, -1
		q.literal = true
		v.empty = &emptyContainer{para: p, box: q}
		at, ok := v.docAtSource(q.contentFrom)
		if !ok {
			at = v.shown.paraStart(p) + max(0, q.docStart)
		}
		q.docStart, q.docEnd = at-v.shown.paraStart(p), at-v.shown.paraStart(p)
		v.empty.box = q
		point := sourcePoint(q.contentFrom, BoundaryDownstream)
		v.setSourceSelectionLocked(SourceSelection{Anchor: point, Caret: point})
		v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
		v.changed = true
	}
	d.typing = nil
	d.mu.Unlock()
	v.coalesce = false
}

func (v *view) contentBoxFor(start, end int) (box, bool) {
	p := v.shown.paraAt(start)
	if p != v.shown.paraAt(end) || p >= v.source.Len() {
		return box{}, false
	}
	ps := v.shown.paraStart(p)
	var found box
	ok := false
	for _, b := range v.boxes(p) {
		if b.literal || b.open < 0 || b.close < 0 || b.docEnd <= b.docStart {
			continue
		}
		if start == ps+b.docStart && end == ps+b.docEnd {
			if !ok || b.docEnd-b.docStart < found.docEnd-found.docStart {
				found, ok = b, true
			}
		}
	}
	return found, ok
}

func (v *view) directSourceEdit(start, end int, s string, rawSource bool) (from, to int, insert string, caret int, ok bool) {
	if v.source.Len() == 0 {
		return 0, 0, "", 0, false
	}
	p := v.shown.paraAt(start)
	if p != v.shown.paraAt(end) {
		return 0, 0, "", 0, false
	}
	sourceFrom := v.sourceAtDoc(start)
	sourceTo := v.sourceAtDoc(end)
	text := v.shown.Slice(start, end).String()
	v.doc.lock()
	sel := v.doc.selection
	anchorDoc, caretDoc := v.doc.anchor, v.doc.caret
	v.doc.mu.Unlock()
	if start == anchorDoc {
		sourceFrom = sel.Anchor.Offset
		if v.sourceSlice(sourceFrom, sourceFrom+len(text)) == text {
			sourceTo = sourceFrom + len(text)
		}
	}
	if end == caretDoc {
		sourceTo = sel.Caret.Offset
		if sourceTo >= len(text) && v.sourceSlice(sourceTo-len(text), sourceTo) == text {
			sourceFrom = sourceTo - len(text)
		}
	}
	// A backward selection's doc start is its caret and its doc end its
	// anchor; the selection's own source points are the same authority in
	// that direction. Without them a doc offset inside a run maps one source
	// byte late, and Tab's replacement stops after the rune it should have
	// replaced (seed9/c2).
	if start == caretDoc {
		sourceFrom = sel.Caret.Offset
		if v.sourceSlice(sourceFrom, sourceFrom+len(text)) == text {
			sourceTo = sourceFrom + len(text)
		}
	}
	if end == anchorDoc {
		sourceTo = sel.Anchor.Offset
		if sourceTo >= len(text) && v.sourceSlice(sourceTo-len(text), sourceTo) == text {
			sourceFrom = sourceTo - len(text)
		}
	}
	if start < anchorDoc && anchorDoc < end && anchorDoc == caretDoc {
		// A collapsed selection inside the range, as between the pair of
		// delimiters a backspace deletes, fixes the range's source edges by
		// its own source point: the document offsets alone cannot say which
		// side of the delimiters they fall on.
		before, after := v.shown.Slice(start, anchorDoc).String(), v.shown.Slice(anchorDoc, end).String()
		at := sel.Caret.Offset
		if v.sourceSlice(at-len(before), at) == before && v.sourceSlice(at, at+len(after)) == after {
			sourceFrom, sourceTo = at-len(before), at+len(after)
		}
	}
	literal := start < end && v.sourceSlice(sourceFrom, sourceTo) == text
	for _, b := range v.boxes(p) {
		if v.empty != nil && b == v.empty.box && sourceTo >= b.contentFrom && sourceTo <= b.contentTo {
			return sourceTo, sourceTo, s, sourceTo + len(s), true
		}
		if start != end {
			if sourceFrom < b.openEnd && sourceTo > b.openStart ||
				sourceFrom < b.closeEnd && sourceTo > b.closeStart {
				return sourceFrom, sourceTo, s, sourceFrom + len(s), true
			}
			continue
		}
		at := sourceTo
		if at == b.start && s != "" && rawSource {
			return at, at, s, at + len(s), true
		}
		if at > b.openStart && at < b.openEnd ||
			at > b.closeStart && at < b.closeEnd {
			return at, at, s, at + len(s), true
		}
		if at == b.end && s != "" && rawSource {
			return at, at, s, at + len(s), true
		}
	}
	if literal {
		if from, to, insert, caret, ok := v.widenFenceForTypedRun(start, sourceTo, s); ok {
			return from, to, insert, caret, true
		}
		return sourceFrom, sourceTo, s, sourceFrom + len(s), true
	}
	return 0, 0, "", 0, false
}

// widenFenceForTypedRun keeps a fence run typed inside a fenced code block
// as its content. Typora widens the block's own fences to one more character
// than the typed run instead of letting the line close the block
// (record 2026-11-09, seed4/c23).
func (v *view) widenFenceForTypedRun(docAt, at int, s string) (from, to int, insert string, caret int, ok bool) {
	if s == "" || v.doc == nil || v.doc.md == nil {
		return 0, 0, "", 0, false
	}
	v.doc.lock()
	defer v.doc.mu.Unlock()
	src := v.doc.md.p.String()
	if v.doc.selection.Anchor.Offset != v.doc.selection.Caret.Offset {
		return 0, 0, "", 0, false
	}
	p := v.shown.paraAt(docAt)
	if p < 0 || p >= v.source.Len() || !v.para(p).style.CodeBlock {
		return 0, 0, "", 0, false
	}
	loc := v.doc.paragraphLocation(p)
	if loc.leaf < 0 || loc.leaf >= len(src) {
		return 0, 0, "", 0, false
	}
	b := v.doc.md.p.Block(loc.leaf)
	if b.Kind != parser.CodeBlock || !b.Fenced {
		return 0, 0, "", 0, false
	}
	fenceChar := byte('`')
	if b.FenceChar != 0 {
		fenceChar = b.FenceChar
	}
	fenceLen := max(3, b.FenceLength)
	line := lineAt(src, at)
	if at < line.start || at > line.end {
		return 0, 0, "", 0, false
	}
	next := strings.TrimSpace(src[line.start:at] + s + src[at:line.end])
	if len(next) < fenceLen {
		return v.pairFenceCharForTypedRun(fenceChar, src, line, at, s)
	}
	for i := 0; i < len(next); i++ {
		if next[i] != fenceChar {
			return v.pairFenceCharForTypedRun(fenceChar, src, line, at, s)
		}
	}
	open := lineAt(src, b.Start)
	closeAt := b.End
	if closeAt > 0 && closeAt <= len(src) {
		closeAt--
	}
	closeLine := lineAt(src, closeAt)
	if closeLine.start <= open.start || at >= closeLine.start {
		return 0, 0, "", 0, false
	}
	openRun, openRunLen, okOpen := fenceRunAt(src, open, true)
	closeRun, closeRunLen, okClose := fenceRunAt(src, closeLine, false)
	if !okOpen || !okClose {
		return 0, 0, "", 0, false
	}
	newFence := strings.Repeat(string(fenceChar), len(next)+1)
	rebuilt := src[open.start:openRun] + newFence + src[openRun+openRunLen:open.stop] +
		src[open.stop:at] + s + src[at:closeLine.start] + src[closeLine.start:closeRun] + newFence +
		src[closeRun+closeRunLen:closeLine.stop]
	caret = at + len(s) + len(newFence) - openRunLen
	return open.start, closeLine.stop, rebuilt, caret, true
}

// pairFenceCharForTypedRun writes the pair Typora types for a single fence
// character typed at the end of a content line that already holds text, and
// holds the caret between the two characters. The characters stay content:
// the widening above only takes a line that is a fence run on its own, and a
// lone character on a line with text neither widens nor closes the block
// (record 2026-11-13, seed21/c9: `alpha1.1. `, `\“ typed, gives
// `alpha1.1. “ ` with the caret on 17, between the two). A line with nothing
// but fence characters is the widening rule's, so `\`\`\“ typed on an empty
// content line still widens the fences (seed4/c23).
func (v *view) pairFenceCharForTypedRun(fenceChar byte, src string, line markdownLine, at int, s string) (from, to int, insert string, caret int, ok bool) {
	if s != string(fenceChar) || at != line.end || at <= line.start {
		return 0, 0, "", 0, false
	}
	if src[at-1] == fenceChar {
		// The character joins a run the line already ends with: that run is
		// the widening rule's to read.
		return 0, 0, "", 0, false
	}
	content := strings.TrimSpace(src[line.start:at])
	for i := 0; i < len(content); i++ {
		if content[i] != fenceChar {
			return at, at, s + s, at + len(s), true
		}
	}
	return 0, 0, "", 0, false
}

// fenceRunAt finds the first fence run on a line: indentation, a run of the
// fence character, then either an info string (opening) or whitespace only.
func fenceRunAt(src string, line markdownLine, allowInfo bool) (start, length int, ok bool) {
	text := src[line.start:line.end]
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	start = i
	if i >= len(text) || (text[i] != '`' && text[i] != '~') {
		return 0, 0, false
	}
	for i < len(text) && text[i] == text[start] {
		i++
	}
	if !allowInfo {
		for ; i < len(text); i++ {
			if text[i] != ' ' && text[i] != '\t' {
				return 0, 0, false
			}
		}
	}
	return line.start + start, i - start, true
}

func (v *view) markerDelete(backspace bool) bool {
	if v.empty != nil && !v.empty.pair {
		v.removeEmpty()
		return true
	}
	if v.selAnchor != v.caret {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p >= v.source.Len() {
		return false
	}
	source := v.caretSource()
	for _, b := range v.boxes(p) {
		from, to := source, source
		if backspace {
			switch {
			case source > b.openStart && source <= b.openEnd:
				from = source - lastRuneSize(v.sourceSlice(b.openStart, source))
			case source > b.closeStart && source <= b.closeEnd:
				from = source - lastRuneSize(v.sourceSlice(b.closeStart, source))
			default:
				continue
			}
		} else {
			switch {
			case source >= b.openStart && source < b.openEnd:
				to = source + firstRuneSize(v.sourceSlice(source, b.openEnd))
			case source >= b.closeStart && source < b.closeEnd:
				to = source + firstRuneSize(v.sourceSlice(source, b.closeEnd))
			default:
				continue
			}
		}
		v.recordDelete()
		v.replaceSource(from, to, "", from)
		return true
	}
	return false
}

func (v *view) removeEmpty() {
	v.removeEmptyPair(false)
}

// removeEmptyPair deletes the empty inline pair the view holds. Enter also
// drops the line ending of a pair that alone holds the document's last
// line after a blank line: the paragraph Enter opens takes the blank
// line's place, as Typora's live bytes show (record 2026-11-11,
// probe60/c16-exit-cmd-b).
func (v *view) removeEmptyPair(enter bool) {
	if v.empty == nil {
		return
	}
	e := v.empty
	v.empty = nil
	start, end := e.box.start, e.box.end
	if enter && v.doc.md != nil {
		v.doc.lock()
		src := v.doc.md.p.String()
		v.doc.mu.Unlock()
		line := lineAt(src, start)
		if line.start == start && line.end == end && line.stop == len(src) &&
			line.eol != "" && line.start > 0 {
			prev := lineAt(src, line.start-1)
			if strings.TrimSpace(src[prev.start:prev.end]) == "" {
				end = line.stop
			}
		}
	}
	v.record(false)
	v.replaceSource(start, end, "", start)
	v.doc.lock()
	v.doc.typing = nil
	v.doc.mu.Unlock()
}

func lastRuneSize(s string) int {
	if s == "" {
		return 1
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return size
}

func firstRuneSize(s string) int {
	if s == "" {
		return 1
	}
	_, size := utf8.DecodeRuneInString(s)
	return size
}

// lockedEdit runs fn as an edit of the document in the action's step, and
// takes what it changed; the caller holds the lock.
func (v *view) lockedEdit(fn func(e *Edit)) error {
	d := v.doc
	before := d.version
	var err error
	run := fn
	fn = func(e *Edit) {
		run(e)
		err = e.err
	}
	switch v.mode {
	case sameStep:
		if len(d.undo) > 0 {
			s := &d.undo[len(d.undo)-1]
			d.run(s, fn)
			s.selAfter = d.selection
			clear(d.redo)
			d.redo = d.redo[:0]
			break
		}
		d.edit(false, fn)
	default:
		d.edit(v.mode == typingStep, fn)
		// Typing that starts a step lets the typing after it join it.
		d.coalesce = v.coalesce
	}
	v.mode = sameStep
	if d.version != before {
		v.changed = true
		v.lockedSync()
		v.holdPastedEmptyPair()
	}
	return err
}

// edit runs fn as a step of undo of its own, as a shortcut's conversion
// does.
func (v *view) edit(fn func(e *Edit)) {
	if v.transient != nil {
		v.discardTransient()
	}
	d := v.doc
	d.lock()
	defer d.mu.Unlock()
	v.lockedSync()
	v.lockedPublish()
	v.mode = newStep
	v.lockedEdit(fn)
	v.mode = newStep
	v.coalesce = false
	v.selAnchor, v.caret = d.anchor, d.caret
	v.selVersion = d.selVersion
	v.reveal = true
}

// command runs a command of the document on the editor's selection.
func (v *view) command(fn func(d *Document)) {
	if v.transient != nil {
		v.discardTransient()
	}
	v.doc.lock()
	v.lockedPublishSource()
	v.doc.mu.Unlock()
	before := v.doc.Version()
	fn(v.doc)
	if v.doc.Version() != before {
		v.changed = true
	}
	v.sync()
	v.mode, v.coalesce = newStep, false
}

// takeBack undoes the document's last step, or redoes the last undone. It
// reports whether a step was taken in the undo direction.
func (v *view) takeBack(redo bool) bool {
	var transient, after *transientUndo
	var undone undoStep
	var taken bool
	// Undo or Redo ends the typing run the promotion belonged to.
	v.pfx.wroteLine = -1
	v.command(func(d *Document) {
		transient, after, undone, taken = d.takeBackTransient(redo)
	})
	if redo || !taken {
		// A redo restores a step instead of taking one back, and taking back
		// nothing restores no line: neither says what the view should hold.
		v.resumeLivePrefix(nil)
	} else {
		v.resumeLivePrefix(&undone)
	}
	if transient != nil && !redo {
		source := v.doc.Source()
		v.doc.lock()
		var state sessionState
		if v.doc.md != nil {
			state = v.doc.state()
		}
		v.doc.mu.Unlock()
		if t := transient.block(source, state); t != nil {
			v.showTransient(t)
		}
	}
	if after != nil && redo {
		source := v.doc.Source()
		v.doc.lock()
		var state sessionState
		if v.doc.md != nil {
			state = v.doc.state()
		}
		v.doc.mu.Unlock()
		if t := after.block(source, state); t != nil {
			v.showTransient(t)
		}
	}
	if v.doc.Source() == "" {
		v.showEmptyDocument()
	}
	v.reveal = true
	return taken
}

// seed24Undo keeps the explicit empty list item Typora leaves behind when
// Undo takes back the edit before a held paragraph. Undo of the last content
// step puts that step back through Redo; a container exit has no content step
// to take back. An ordinary Enter with no step stands for a held paragraph
// that Undo drops instead (seed4/c17).
func (v *view) seed24Undo(base string, t *transientBlock) bool {
	if t == nil || !seed24UndoMaterialization(base, t) {
		return false
	}
	containerExit := t.mode == transientContainerExit
	if containerExit && v.doc.CanUndo() {
		// A lift that wrote the empty item away is itself the next Undo
		// step: that step restores the item, so do not turn the exit into
		// a new item (TestEmptyTopLevelListItemEnterWritesLift).
		return false
	}
	if v.takeBack(false) {
		v.takeBack(true)
	} else if !containerExit {
		return false
	}
	v.discardTransient()
	line := lineAt(base, finalEOLStart(base))
	marker := base[line.start : line.start+2]
	caret := len(base) + len(marker)
	if err := v.doc.ReplaceSource(len(base), len(base), marker+defaultEOL(base)); err != nil {
		return true
	}
	v.doc.SetSourceSelection(sourceCaret(caret))
	v.sync()
	v.mode, v.coalesce = newStep, false
	v.reveal = true
	return true
}

// insert replaces the selection with s, as typing does.
func (v *view) insert(s string) {
	if v.opts.ReadOnly {
		return
	}
	if pending := v.seed24FullSelectFromFirstRune(s); pending != nil {
		v.seed24FullSelectCommit(*pending)
		v.replace(pending.docFrom, pending.docTo, s)
		v.clearLivePrefix()
		v.seed24FullSelectFinish()
		return
	}
	if v.seed24FullSelectConsumeSpace(s) {
		return
	}
	space := v.pfx.quoteSpace
	v.pfx.quoteSpace = quoteSpaceEdit{}
	marker := v.pfx.quoteMarker
	v.pfx.quoteMarker = quoteMarkerEdit{}
	s = normalizeNewlines(s)
	a, z := v.selection()
	// A tab closes the typing group: Typora's undo after a paragraph's Tab
	// removes the tab alone, not the run typed before it (record 2026-11-10,
	// seed10/c2). Typing after the tab starts a step of its own.
	keystroke := utf8.RuneCountInString(s) == 1 && s != "\n" && s != "\t"
	typing := a == z && keystroke
	switch {
	case typing && v.closesInline(s):
		// Typora makes the keystroke that completes an inline span a step of
		// undo of its own: undoing `**bold**` leaves `**bold*`, not an empty
		// paragraph. Typing after the keystroke joins its step.
		v.record(false)
		v.coalesce, v.coalesceKind = true, coalesceTyping
	case a != z && keystroke:
		// A keystroke over a selection replaces it in a step of its own, and
		// the typing after it joins that step: one Undo takes the replacement
		// and what followed it together (record 2026-11-13, seed18/c6).
		v.record(false)
		v.coalesce, v.coalesceKind = true, coalesceTyping
	default:
		v.record(typing)
	}
	if space.ok && v.transient == nil && space.at >= 0 && space.at < len(s) && s[space.at] == ' ' {
		// The marker and the space after it are separate edits: Undo takes
		// back the space alone and leaves the marker literal (record
		// 2026-11-11, probe83/undo-after-gt).
		v.replace(a, z, s[:space.at]+s[space.at+1:])
		v.insertQuoteSpace(s, space.at)
	} else {
		v.replace(a, z, s)
		if marker.ok && v.transient == nil {
			v.convertQuoteMarker(marker)
		}
	}
	v.splitQuoteContinuationPrefix()
	v.splitQuoteItemPrefix()
	v.noteLiveHeadingCloser()
	v.retainTypedEmptyStrong(s)
	v.holdTypedStrongComplete(s)
}

// replaceSourceSelection replaces the hidden syntax markers the source
// selection covers when the document selection has collapsed: a selection
// over markers alone takes no rune of the projection, so both document
// offsets coincide while Typora still replaces their bytes (VM record
// 2026-11-12-064820, seed11/c9). It reports whether it replaced them.
func (v *view) replaceSourceSelection(s string) bool {
	if v.doc == nil || v.doc.md == nil || v.opts.ReadOnly {
		return false
	}
	d := v.doc
	d.lock()
	from, to := d.selection.Anchor.Offset, d.selection.Caret.Offset
	d.mu.Unlock()
	if from == to {
		return false
	}
	if from > to {
		from, to = to, from
	}
	if !v.sourceSelectionInMarkers(from, to) {
		return false
	}
	v.record(false)
	v.replaceSource(from, to, s, from+len(s))
	return true
}

// sourceSelectionInMarkers reports whether the source bytes from..to lie
// inside one marker of the caret's paragraph.
func (v *view) sourceSelectionInMarkers(from, to int) bool {
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.source.Len() {
		return false
	}
	for _, b := range v.boxes(p) {
		if b.literal {
			continue
		}
		if from >= b.openStart && to <= b.openEnd || from >= b.closeStart && to <= b.closeEnd {
			return true
		}
	}
	return false
}

// insertQuoteSpace writes the space prepareBlockPrefix put between a quote
// marker and its content as an edit of its own. s is the prepared text,
// with at the space's byte offset; the rest of s is already in the source.
func (v *view) insertQuoteSpace(s string, at int) {
	if v.doc == nil {
		return
	}
	v.doc.lock()
	after := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	spaceAt := after - (len(s) - at - 1)
	current := v.doc.Source()
	if spaceAt < 0 || spaceAt > len(current) || spaceAt > 0 && current[spaceAt-1] != '>' {
		return
	}
	v.record(false)
	v.replaceSource(spaceAt, spaceAt, " ", after+1)
}

// convertQuoteMarker makes the line a quote marker opened on, as an edit of
// its own: the marker's bytes are in the line and stay there, so Undo takes
// the conversion back and the line reads its marker as text (record
// 2026-11-12, probe94/leading-space-gt-undo). The caret rests after the
// marker and the first of the line's own spaces, which is the separator the
// marker opens (probe94/leading-space-gt); a second space is content and the
// caret stays before it (probe94/leading-two-space-gt).
func (v *view) convertQuoteMarker(m quoteMarkerEdit) {
	if v.doc == nil || v.doc.md == nil || m.lineStart < 0 {
		return
	}
	src := v.doc.Source()
	content := m.markerEnd
	if content < len(src) && src[content] == ' ' {
		content++
	}
	v.record(false)
	v.lockedEdit(func(e *Edit) {
		e.d.setState(e.d.state().clearLiteralLineAt(m.lineStart))
		if !e.reparseMarkdownSource() {
			e.fail(ErrStructuralEdit)
			return
		}
		e.selectMarkdownSource(content)
	})
}

// noteLiveHeadingCloser records a heading closing sequence the session
// typed at the line's end: Typora keeps the bytes in the block's DOM until
// it re-reads the document, so the sequence stays on screen while the
// editor lives (record 2026-11-09, diff.json bdbf85b7).
func (v *view) noteLiveHeadingCloser() {
	d := v.doc
	if d == nil || d.md == nil {
		return
	}
	d.lock()
	src := d.md.p.String()
	sel := d.selection
	d.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	at := sel.Caret.Offset
	if at <= 0 || at > len(src) {
		return
	}
	line := liveLineAt(src, at)
	if at != line.end {
		return
	}
	if from, ok := headingClosingStart(src, line.start, line.end); ok {
		d.lock()
		d.addLiveCloser(from, line.end)
		d.mu.Unlock()
	}
}

// headingClosingStart returns where the closing sequence of the ATX
// heading on the line begins, as the parser strips it: a run of hashes
// preceded by space or tab, after non-empty content, and the whitespace
// that leads to it.
func headingClosingStart(src string, start, end int) (int, bool) {
	i := start
	for n := 0; n < 3 && i < end && src[i] == ' '; n++ {
		i++
	}
	h := i
	for i < end && src[i] == '#' {
		i++
	}
	if i == h || i-h > 6 || i == end || (src[i] != ' ' && src[i] != '\t') {
		return 0, false
	}
	content := i + 1
	e := end
	for e > content && spaceOrTab(src[e-1]) {
		e--
	}
	k := e
	for k > content && src[k-1] == '#' {
		k--
	}
	if k == content || k == e || !spaceOrTab(src[k-1]) {
		return 0, false
	}
	from := k
	for from > content && spaceOrTab(src[from-1]) {
		from--
	}
	return from, true
}

func spaceOrTab(b byte) bool { return b == ' ' || b == '\t' }

// closesInline reports whether typing s at the caret closes an inline span
// that starts where its delimiter run starts. Typora keeps a keystroke that
// only closes a span inside a longer run, as the first closing star of
// `**bold**` does, in the typing step before it.
func (v *view) closesInline(s string) bool {
	if v.doc == nil || v.doc.md == nil || utf8.RuneCountInString(s) != 1 || !strings.ContainsAny(s, "*_~`)]}") {
		return false
	}
	v.doc.lock()
	defer v.doc.mu.Unlock()
	src := v.doc.md.p.String()
	at := v.doc.selection.Caret.Offset
	if at < 0 || at > len(src) || v.doc.selection.Anchor.Offset != at {
		return false
	}
	line := liveLineAt(src, at)
	local := at - line.start
	next := src[line.start:at] + s + src[at:line.end]
	p := parser.New(v.doc.md.p.Extensions())
	p.Set(next)
	for i := 1; i < p.Block(0).Next; i++ {
		for _, in := range p.Inlines(i) {
			if in.End != local+len(s) || in.Start < 0 || in.Start >= in.End || in.End > len(next) {
				continue
			}
			switch in.Kind {
			case parser.Code, parser.Emphasis, parser.Strong, parser.Strikethrough, parser.Link, parser.Image:
			default:
				continue
			}
			if next[in.End-1] != s[0] {
				continue
			}
			if in.Start > 0 && next[in.Start-1] == next[in.Start] {
				continue
			}
			return true
		}
	}
	return false
}

// deleteRange deletes the runes from a to z.
func (v *view) deleteRange(a, z int, typing bool) {
	if a == z || v.opts.ReadOnly {
		return
	}
	if typing {
		v.recordDelete()
	} else {
		v.record(false)
	}
	v.replace(min(a, z), max(a, z), "")
}

// joinStep makes the next edit join the step of the edit before it, unless
// the action started a step no edit made yet: a composition at a caret
// starts one that its commit makes.
func (v *view) joinStep() {
	if v.mode != newStep {
		v.mode = sameStep
	}
}

// commitCompose puts the composition into the text, in the typing
// attributes, as part of the step its start made.
func (v *view) commitCompose() {
	if v.pendingSet || v.compose != "" {
		a, z := v.composeRange()
		v.commitText(a, z, normalizeNewlines(v.compose))
		return
	}
	v.composeCaret, v.composeSelected = 0, ui.TextInputRange{}
	v.composing = false
}

// special reports whether a paragraph of style st is more than a body
// paragraph, which Backspace at its start makes it before joining.
func special(st ParagraphStyle) bool {
	return st.List != NoList || st.Heading > 0 || st.Quote || st.CodeBlock || st.Indent > 0
}

// unstyle takes away what Backspace at the start of a paragraph of style
// st takes first: a level of indentation of a list's item, the list, a
// heading, a quotation, a block of code.
func unstyle(s *ParagraphStyle) {
	switch {
	case s.List != NoList && s.Indent > 0:
		s.Indent--
	case s.List != NoList:
		s.List, s.Checked = NoList, false
	case s.Heading > 0:
		s.Heading = 0
	case s.Quote:
		s.Quote, s.QuoteDepth = false, 0
	case s.CodeBlock:
		s.CodeBlock = false
	case s.Indent > 0:
		s.Indent--
	}
}

// enter breaks the paragraph at the selection: a list's item continues the
// list and an empty one leaves it, a heading ends at its end, and "```"
// starts a block of code.
func (v *view) enter() {
	a, z := v.selection()
	p := v.shown.paraAt(v.caret)
	st := v.para(p).style
	ps := v.shown.paraStart(p)
	pe := ps + v.para(p).n
	if a == z && ps == pe {
		exit := func(s *ParagraphStyle) bool {
			switch {
			case s.List != NoList && s.Indent > 0:
				s.Indent--
			case s.List != NoList:
				s.List, s.Checked = NoList, false
			case s.Quote:
				s.Quote, s.QuoteDepth = false, 0
			case s.CodeBlock && p > 0 && v.para(p-1).style.CodeBlock:
				s.CodeBlock = false
			default:
				return false
			}
			return true
		}
		probe := st
		if exit(&probe) {
			v.command(func(d *Document) { d.FormatParagraphs(func(s *ParagraphStyle) { exit(s) }) })
			return
		}
	}
	if !v.opts.NoMarkdown && a == z && v.caret == pe && !st.CodeBlock {
		if line := v.lineOf(p); strings.HasPrefix(line, "```") && !strings.Contains(line[3:], "`") {
			v.edit(func(e *Edit) {
				e.ReplaceString(ps, pe, "", Attributes{})
				e.FormatParagraphs(ps, ps, func(s *ParagraphStyle) { *s = ParagraphStyle{CodeBlock: true, Indent: s.Indent, ID: s.ID} })
				e.Select(ps, ps)
			})
			return
		}
	}
	atEnd := v.caret == pe && a == z
	atStart := a == z && a == ps && ps < pe
	v.doc.lock()
	v.lockedPublish()
	attrs := v.doc.typingAttributes()
	v.doc.mu.Unlock()
	v.record(false)
	v.replace(a, z, "\n")
	d := v.doc
	d.lock()
	v.lockedEdit(func(e *Edit) {
		// The new paragraph: after the caret's, or before it at its start,
		// as Notes keeps the item it moves down checked.
		at := v.caret
		if atStart {
			at = v.caret - 1
		}
		e.FormatParagraphs(at, at, func(s *ParagraphStyle) {
			if atEnd && s.Heading > 0 {
				s.Heading = 0
			}
			s.Checked = false
		})
	})
	d.assignSelection(v.caret, v.caret, false)
	keep := attrs
	d.typing = &keep
	d.mu.Unlock()
	v.coalesce = false
}

// typed runs the Markdown shortcuts after s was typed at the caret.
func (v *view) typed(s string) {
	if v.opts.NoMarkdown || v.opts.ReadOnly || utf8.RuneCountInString(s) != 1 {
		return
	}
	if v.doc.md != nil {
		v.doc.lock()
		v.doc.typing = nil
		v.doc.mu.Unlock()
		v.reveal = true
		return
	}
	p := v.shown.paraAt(v.caret)
	ps := v.shown.paraStart(p)
	if v.para(p).style.CodeBlock {
		return
	}
	before := []rune(v.shown.Slice(ps, v.caret).String())
	if len(before) == 0 {
		return
	}
	if s == " " {
		v.blockShortcut(ps, string(before[:len(before)-1]))
		return
	}
	switch s {
	case "*", "_", "`", "~":
		v.inlineShortcut(ps, before)
	}
}

// blockShortcut turns what was typed before a space at the start of a
// paragraph into its style: "# " a heading, "- " a list, "> " a quotation.
func (v *view) blockShortcut(ps int, prefix string) {
	var set func(s *ParagraphStyle)
	switch {
	case prefix != "" && strings.Trim(prefix, "#") == "" && len(prefix) <= 6:
		level := len(prefix)
		set = func(s *ParagraphStyle) { s.Heading, s.List, s.Checked = level, NoList, false }
	case prefix == "-" || prefix == "*" || prefix == "+":
		set = func(s *ParagraphStyle) { s.List, s.Heading = Bullets, 0 }
	case prefix == "[]" || prefix == "[ ]" || prefix == "- [ ]" || prefix == "-[ ]":
		set = func(s *ParagraphStyle) { s.List, s.Heading, s.Checked = Tasks, 0, false }
	case prefix == "[x]" || prefix == "- [x]" || prefix == "[X]":
		set = func(s *ParagraphStyle) { s.List, s.Heading, s.Checked = Tasks, 0, true }
	case prefix == ">":
		set = func(s *ParagraphStyle) { s.Quote = true }
	case len(prefix) >= 2 && (prefix[len(prefix)-1] == '.' || prefix[len(prefix)-1] == ')') && strings.Trim(prefix[:len(prefix)-1], "0123456789") == "":
		set = func(s *ParagraphStyle) { s.List, s.Heading = Numbers, 0 }
	}
	if set == nil {
		return
	}
	n := utf8.RuneCountInString(prefix) + 1
	v.edit(func(e *Edit) {
		e.ReplaceString(ps, ps+n, "", Attributes{})
		e.FormatParagraphs(ps, ps, set)
		e.Select(ps, ps)
	})
}

// inlineShortcut formats what a pair of marks typed around words holds:
// **bold**, *italic*, _italic_, `code` and ~~strikethrough~~.
func (v *view) inlineShortcut(ps int, before []rune) {
	n := len(before)
	last := before[n-1]
	mark := string(last)
	if (last == '*' || last == '~' || last == '_') && n >= 2 && before[n-2] == last {
		mark += mark
	}
	if last == '~' && len(mark) == 1 {
		return
	}
	m := len([]rune(mark))
	closeAt := n - m
	if closeAt <= 0 || unicode.IsSpace(before[closeAt-1]) {
		return
	}
	open := -1
	for i := closeAt - 1; i >= 0; i-- {
		if string(before[i:min(i+m, n)]) == mark && i+m < closeAt {
			// Not the inside of a longer mark, and words right after it.
			if (i > 0 && before[i-1] == last) || before[i+m] == last || unicode.IsSpace(before[i+m]) {
				continue
			}
			// An underscore inside a word, as snake_case, is no mark.
			if last == '_' && i > 0 && (unicode.IsLetter(before[i-1]) || unicode.IsDigit(before[i-1])) {
				continue
			}
			open = i
			break
		}
	}
	if open < 0 {
		return
	}
	set := func(a *Attributes) {
		switch mark {
		case "**", "__":
			a.Weight = 700
		case "*", "_":
			a.Italic = true
		case "`":
			a.Code = true
		case "~~":
			a.Strikethrough = true
		}
	}
	start, end := ps+open, ps+closeAt
	v.edit(func(e *Edit) {
		e.ReplaceString(end, end+m, "", Attributes{})
		e.ReplaceString(start, start+m, "", Attributes{})
		e.Format(start, end-m, set)
		e.Select(end-m, end-m)
	})
	// What is typed next is outside the format, as after a closing mark.
	v.doc.lock()
	plain := Attributes{}
	if start > ps {
		plain = v.doc.text.AttributesAt(start - 1).extending()
	}
	v.doc.typing = &plain
	v.doc.mu.Unlock()
}

// looksLikeURL reports whether s is a URL, as ⌘K links one to itself.
func looksLikeURL(s string) bool {
	if strings.ContainsAny(s, " \n\t") {
		return false
	}
	for _, prefix := range []string{"http://", "https://", "mailto:", "www."} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// run runs a command of the Format menu or of a shortcut, and reports
// whether there is one of that name.
func (v *view) run(cmd string) bool {
	if v.doc.md != nil {
		switch cmd {
		case "bold", "italic", "underline", "strikethrough", "code", "link":
			f := v.doc.Formatting()
			insideCode := false
			v.doc.lock()
			if f.Code == FormatOn {
				insideCode = v.doc.sourceInsideMark(v.doc.caret, MarkerCode)
			}
			opensFence := v.opensFenceLocked()
			v.doc.mu.Unlock()
			if f.Paragraph.CodeBlock || insideCode || opensFence {
				return true
			}
		}
	}
	var fn func(d *Document)
	switch cmd {
	case "bold":
		fn = (*Document).ToggleBold
	case "italic":
		fn = (*Document).ToggleItalic
	case "underline":
		fn = (*Document).ToggleUnderline
	case "strikethrough":
		fn = (*Document).ToggleStrikethrough
	case "code":
		fn = (*Document).ToggleCode
	case "bullets":
		fn = func(d *Document) { d.ToggleList(Bullets) }
	case "numbers":
		fn = func(d *Document) { d.ToggleList(Numbers) }
	case "tasks":
		fn = func(d *Document) { d.ToggleList(Tasks) }
	case "indent":
		fn = func(d *Document) { d.Indent(1) }
	case "outdent":
		fn = func(d *Document) { d.Indent(-1) }
	case "alignLeft":
		fn = func(d *Document) { d.SetAlign(ui.Start) }
	case "alignCenter":
		fn = func(d *Document) { d.SetAlign(ui.Center) }
	case "alignRight":
		fn = func(d *Document) { d.SetAlign(ui.End) }
	case "quote":
		fn = (*Document).ToggleQuote
	case "codeBlock":
		fn = (*Document).ToggleCodeBlock
	case "link":
		if v.opts.ReadOnly {
			return false
		}
		if v.opts.OnLinkCommand != nil {
			v.publish()
			v.opts.OnLinkCommand()
			return true
		}
		a, z := v.selection()
		url := strings.TrimSpace(v.shown.Slice(a, z).String())
		if a == z || !looksLikeURL(url) {
			v.command((*Document).InsertLink)
			return true
		} else {
			fn = func(d *Document) { d.SetLink(url) }
		}
	default:
		if level, ok := strings.CutPrefix(cmd, "heading"); ok && len(level) == 1 && level[0] >= '0' && level[0] <= '6' {
			fn = func(d *Document) { d.SetHeading(int(level[0] - '0')) }
		}
	}
	if fn == nil {
		return false
	}
	if v.opts.ReadOnly {
		return true
	}
	if cmd == "bold" && v.boldPairInTransient() {
		return true
	}
	if v.formatPairInHeldBreak(cmd) {
		return true
	}
	if v.formatWordRun(cmd) {
		return true
	}
	v.command(fn)
	switch cmd {
	case "bold":
		v.retainEmptyPair(MarkerStrong)
		v.holdStrongFromPatch()
	case "italic":
		v.retainEmptyPair(MarkerEmphasis)
	}
	return true
}

// formatWordRun formats the word the caret sits in when Typora's word
// reaches past the run of word runes wordAt takes. Typora's splitter
// (appsrc/main.js, WESTERN_WORD_SPLITTER) keeps backticks, `$`, `^`, `=`,
// `|` and `~` inside a word, so the literal backticks typed after `beta`
// do not end its word: the two typed backticks stay inside it and
// ⌘B wraps the whole run, opening the delimiters before `beta`
// (VM record 2026-11-12-064820, seed11/c8). It reports
// whether it formatted such a word, and the command must then stop.
func (v *view) formatWordRun(cmd string) bool {
	kind, set, ok := formatCommand(cmd)
	if !ok || v.opts.ReadOnly || v.doc == nil || v.doc.md == nil {
		return false
	}
	if f := v.doc.Formatting(); kind == MarkerStrong && f.Bold == FormatOn ||
		kind == MarkerEmphasis && f.Italic == FormatOn {
		// Turning the format off keeps the command's own path.
		return false
	}
	from, to, wide := v.formattedWordRun()
	if !wide {
		return false
	}
	v.command(func(d *Document) {
		d.lock()
		defer d.mu.Unlock()
		d.formatSelection(from, to, to, to, func(x *Attributes) { set(x, true) })
	})
	return true
}

// formattedWordRun returns the word around the caret as Typora's splitter
// takes it, and whether it reaches wider than the run of word runes wordAt
// takes: only the wider word needs a write point of its own.
func (v *view) formattedWordRun() (int, int, bool) {
	d := v.doc
	d.lock()
	a, z := d.span()
	text := d.text
	d.mu.Unlock()
	if a != z {
		return 0, 0, false
	}
	runFrom, runTo, inRun := text.wordAt(a)
	if !inRun || text.AttributesAt(runFrom).Link != "" || text.AttributesAt(runFrom).Code {
		return 0, 0, false
	}
	start, end := text.ParagraphRange(a)
	runes := []rune(text.Slice(start, end).String())
	lo, hi, ok := wordSpanAt(runes, a-start)
	if !ok {
		return 0, 0, false
	}
	from, to := start+lo, start+hi
	if from == runFrom && to == runTo {
		return 0, 0, false
	}
	return from, to, true
}

// wordSpanAt returns the word around the caret in runes, whose offset at
// may stand inside it or just after it, as Typora's formatting splitter
// takes it. The bool is false when no word is there.
func wordSpanAt(runes []rune, at int) (int, int, bool) {
	if len(runes) == 0 {
		return 0, 0, false
	}
	if at < 0 {
		at = 0
	}
	if at > len(runes) {
		at = len(runes)
	}
	if at < len(runes) && wordClass(runes[at]) != 0 {
		lo, hi := at, at+1
		for lo > 0 && sameWordRune(runes[lo-1], runes[lo]) {
			lo--
		}
		for hi < len(runes) && sameWordRune(runes[hi-1], runes[hi]) {
			hi++
		}
		return lo, hi, true
	}
	if at > 0 && wordClass(runes[at-1]) != 0 {
		lo := at - 1
		for lo > 0 && sameWordRune(runes[lo-1], runes[lo]) {
			lo--
		}
		return lo, at, true
	}
	return 0, 0, false
}

// sameWordRune reports whether a and b, next to each other, stay in one
// word for Typora's formatting splitter (appsrc/main.js,
// WESTERN_WORD_SPLITTER): the ASCII punctuation Markdown uses as inline
// syntax is kept, the splitter's own punctuation breaks the word, and a
// script change between Latin and Han or kana is a boundary.
func sameWordRune(a, b rune) bool {
	ca, cb := wordClass(a), wordClass(b)
	return ca != 0 && ca == cb
}

// wordClass groups runes as Typora's splitter does for words: 0 breaks a
// word, 1 is Latin-style and 2 is Han or kana.
func wordClass(r rune) int {
	if r == '\n' || r == LineSeparator {
		return 0
	}
	if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
		return 2
	}
	if r < 0x80 {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("$_^=|~`", r) {
			return 1
		}
		return 0
	}
	if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
		return 1
	}
	return 0
}

// formatCommand maps a Format command to the marker it toggles and the
// attribute it sets.
func formatCommand(cmd string) (MarkerKind, func(a *Attributes, on bool), bool) {
	switch cmd {
	case "bold":
		return MarkerStrong, func(a *Attributes, on bool) {
			if on {
				a.Weight = 700
				return
			}
			a.Weight = 0
		}, true
	case "italic":
		return MarkerEmphasis, func(a *Attributes, on bool) { a.Italic = on }, true
	}
	return 0, nil, false
}

// boldPairInTransient writes the empty strong pair into the transient
// paragraph the caret stands in, as Typora's Cmd+B does there: the
// paragraph materializes with the pair, the caret sits between the markers,
// and the view keeps the empty strong element (diff.json b609e7fa).
func (v *view) boldPairInTransient() bool {
	t := v.transient
	if t == nil || t.isLineBreak() || t.isViewOnlyLine() {
		return false
	}
	if a, z := v.selection(); a != z {
		return false
	}
	open, close := markerDefaults(MarkerStrong)
	if !v.materializeTransient(func(e *Edit, at int, attrs Attributes) error {
		e.ReplaceString(at, at, open+close, attrs)
		e.Select(at+len(open), at+len(open))
		return e.Err()
	}, false) {
		return false
	}
	v.retainEmptyPair(MarkerStrong)
	return true
}

// formatPairInHeldBreak writes the empty delimiter pair a formatting
// command puts at the caret standing on a pending line break, as Typora's
// Cmd+B does there: the pair joins the held line, so the break's bytes and
// the pair materialize together instead of the word above it being
// formatted (record 2026-11-12-133304, seed12/c4).
func (v *view) formatPairInHeldBreak(cmd string) bool {
	kind, _, ok := formatCommand(cmd)
	if !ok || v.opts.ReadOnly || v.doc == nil || v.doc.md == nil {
		return false
	}
	t := v.transient
	if t == nil || t.isEmptyParagraph() || t.marker != "" ||
		t.write != t.anchor.Offset+t.brk.source {
		return false
	}
	if a, z := v.selection(); a != z {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.shown.ParagraphCount() ||
		v.caret != v.shown.paraStart(p)+v.para(p).n || t.heldBreakContent() != "" {
		// The caret stands on the text line, or the held line already
		// holds whitespace: the pair is not the line's first content.
		return false
	}
	open, close := markerDefaults(kind)
	unit := t.brk.unit
	if open == "" || close == "" || !strings.ContainsAny(unit, "\r\n") {
		return false
	}
	contentStart := t.anchor.Offset + len(unit)
	d := v.doc
	d.lock()
	base := d.md.p.String()
	d.mu.Unlock()
	if t.anchor.Offset < 0 || t.anchor.Offset > len(base) {
		return false
	}
	// The line the break opens starts after its line ending, which the
	// item's continuation indent follows; the pair is the line's content.
	next := base[:t.anchor.Offset] + unit + open + close + base[t.anchor.Offset:]
	lineStart := lineAt(next, contentStart).start
	v.record(false)
	d.lock()
	transient := t
	v.transient = nil
	v.version = 0
	v.mode = newStep
	beforeUndo := len(d.undo)
	err := v.lockedEdit(func(e *Edit) {
		if !e.replaceMarkdownSource(transient.anchor.Offset, transient.anchor.Offset, unit+open+close) {
			e.fail(ErrStructuralEdit)
			return
		}
		// The line is the item's continuation text, not the thematic break
		// its bytes spell to a cold parse (record 2026-11-12-133304).
		e.d.setState(e.d.state().setLiteralLine(lineStart, contentStart))
		if !e.reparseMarkdownSource() {
			e.fail(ErrStructuralEdit)
			return
		}
		e.selectMarkdownSource(contentStart + len(open))
	})
	d.mu.Unlock()
	if err != nil {
		v.transient = transient
		v.version = 0
		v.showTransient(transient)
		return false
	}
	if len(d.undo) == beforeUndo+1 {
		d.undo[len(d.undo)-1].transient = transient.undo(base)
	}
	v.mode, v.coalesce = newStep, false
	v.reveal = true
	v.retainEmptyPair(kind)
	return true
}

// retainTypedEmptyStrong keeps the empty strong pair that typing `****` at
// a paragraph's end completes. The parser reads the run as literal text, but
// Typora's live model holds an empty strong element and, with the caret at
// the pair's end, reveals both delimiters as markers (record 2026-11-10,
// seed8/c1).
func (v *view) retainTypedEmptyStrong(typed string) {
	if typed == "" || strings.Trim(typed, "*") != "" || v.empty != nil || v.doc == nil || v.doc.md == nil {
		return
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	sel := d.selection
	d.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	at := sel.Caret.Offset
	if at < 4 || at > len(src) || src[at-4:at] != "****" {
		return
	}
	if at > 4 && src[at-5] == '*' {
		// A longer run is not the pair the keystroke completed.
		return
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.source.Len() || at != v.paragraphEndSource(p) {
		return
	}
	if v.parserReadsTypedPair(p, at-4, at) {
		return
	}
	doc, ok := v.docAtSource(at - 2)
	if !ok {
		return
	}
	ps := v.shown.paraStart(p)
	v.empty = &emptyContainer{para: p, pair: true, box: box{
		open:        -1,
		close:       -1,
		kind:        MarkerStrong,
		start:       at - 4,
		end:         at,
		openStart:   at - 4,
		openEnd:     at - 2,
		closeStart:  at - 2,
		closeEnd:    at,
		contentFrom: at - 2,
		contentTo:   at - 2,
		docStart:    doc - ps,
		docEnd:      doc - ps,
		literal:     true,
	}}
}

// retainEmptyPair keeps the empty delimiter pair a formatting command just
// wrote as the inline element Typora's live editor shows. The Markdown
// parser reads `****` and a trailing `**` as text, so the view holds the
// box the parser does not: its content point is where the next rune goes.
//
// A strong pair's markers are revealed at the caret, as Typora shows them
// (4fd2655f, 60b9880a). An emphasis pair renders as literal text with the
// caret after it instead (16543a65), so the view keeps that pair only where
// Typora recorded it: at the end of a paragraph that has text before it.
func (v *view) retainEmptyPair(kind MarkerKind) {
	d := v.doc
	pair := "****"
	if kind == MarkerEmphasis {
		pair = "**"
	}
	start, oldEnd, newEnd, ok := d.LastSourcePatch()
	if !ok || oldEnd != start || newEnd-start < len(pair) {
		return
	}
	src := d.Source()
	marker := len(pair) / 2
	mid := d.SourceSelection().Caret.Offset
	pairStart := mid - marker
	pairEnd := mid + marker
	if pairStart < start || pairEnd > newEnd || pairEnd > len(src) ||
		src[pairStart:pairEnd] != pair {
		return
	}
	start, newEnd = pairStart, pairEnd
	boxKind := kind
	if kind == MarkerEmphasis {
		// The pair lands right after a literal `**`: Typora's live model
		// reads the four stars as one empty strong element rather than
		// two emphasis runs (record 2026-11-10, seed4/c10).
		if q := v.source.from(pairStart) - 1; q >= 0 && q < v.source.Len() &&
			pairStart-2 >= v.source.start(q) && src[pairStart-2:pairStart] == "**" {
			boxKind, start, mid = MarkerStrong, pairStart-2, pairStart
		}
	}
	doc, ok := v.docAtSource(mid)
	if !ok {
		return
	}
	p := v.shown.paraAt(doc)
	if p < 0 || p >= v.source.Len() {
		return
	}
	if boxKind == MarkerEmphasis &&
		(v.source.start(p) >= start || v.paragraphEndSource(p) != newEnd) {
		return
	}
	if boxKind == MarkerStrong && v.holdStrongPairAfterTypedOpener(p, src, start, newEnd) {
		return
	}
	q := box{
		open:        -1,
		close:       -1,
		kind:        boxKind,
		start:       start,
		end:         newEnd,
		openStart:   start,
		openEnd:     mid,
		closeStart:  mid,
		closeEnd:    newEnd,
		contentFrom: mid,
		contentTo:   mid,
		docStart:    doc - v.shown.paraStart(p),
		docEnd:      doc - v.shown.paraStart(p),
		literal:     true,
	}
	v.empty = &emptyContainer{para: p, box: q, pair: true}
}
