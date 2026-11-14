package richtext

import (
	"slices"
	"strings"
	"sync"
	"time"

	"weak"

	"github.com/egoist/mygo/ui/markdown/parser"
)

// Document is what an Editor edits: a Text, its
// selection with the attributes typing gets there, and the history of
// undo, which the app keeps from frame to frame, as it keeps a TextBuffer.
// Its methods are safe from any goroutine: a toolbar, a menu or a timer
// formats and edits it while the editor shows it, and the editor follows
// what changed at its next frame (schedule changes from other goroutines
// with Window.Update, so that a frame comes).
//
// Commands act on the selection as the editor's shortcuts do: on a range
// they change its runs or paragraphs, as one step of undo; on a caret,
// character formats change only the typing attributes, which the next
// typed text gets and the next move of the caret forgets, as ⌘B does in
// NSTextView.
type Document struct {
	mu   sync.Mutex
	text Text
	// anchor and caret are the projection of selection and are rebuilt from
	// it whenever the source selection changes.
	anchor    int
	caret     int
	selection SourceSelection
	typing    *Attributes // nil: derived from the text at the caret
	version   uint64
	// selVersion counts changes of the selection made by the document's
	// methods, which the editor then takes.
	selVersion uint64
	undo, redo []undoStep
	lastEdit   time.Time
	coalesce   bool // typing may join the last step
	// pendingTransient is the view state the next step restores on undo,
	// set when the view drops a held paragraph that the step acts before. A
	// step that never comes leaves it to the input event's end, which clears
	// it.
	pendingTransient *transientUndo
	// pendingPromotion is the block prefix the step being written promoted on
	// a line it wrote itself: a held paragraph the run materialized, whose
	// markers then read as a block. The step keeps it, so Undo restores the
	// hold and the bytes stay as the text they began as (record 2026-11-12,
	// seed14/c8).
	pendingPromotion *literalPromotion
	// log is what changed since the versions the editor last saw, for it
	// to lay out only the paragraphs that changed.
	log []change
	// newID names the paragraphs edits make. shownIn is the editor built
	// last that shows the document, kept weakly for compatibility; active
	// keeps that editor reachable while a headless caller holds only the
	// document (TestRichTextReplaceCRLF).
	newID           func() string
	shownIn         weak.Pointer[view]
	active          *view
	readNone        bool
	md              *markdownSource
	viewport        ViewportAnchor
	viewportVersion uint64
}

// liveCloser is the source range of a heading closing sequence, such as
// the `# ` at the end of `# title # `, as the session typed it.
type liveCloser struct{ from, to int }

// splitMarker is one item-marker line an Enter split wrote this session:
// lineStart..outerEnd holds the sibling's own marker and outerEnd..innerEnd
// the moved marker the live view shows as the empty item's prefix.
type splitMarker struct {
	lineStart int
	outerEnd  int
	innerEnd  int
	line      string
}

// splitMarkerAt returns the session split whose line still starts at
// lineStart with the bytes it wrote.
func (d *Document) splitMarkerAt(lineStart int) (splitMarker, bool) {
	if d.md == nil {
		return splitMarker{}, false
	}
	d.lock()
	defer d.mu.Unlock()
	return d.splitMarkerAtLocked(lineStart)
}

// splitMarkerAtLocked is splitMarkerAt with the document's lock held.
func (d *Document) splitMarkerAtLocked(lineStart int) (splitMarker, bool) {
	src := d.md.p.String()
	return d.md.session.splitMarkerAt(lineStart, src)
}

// heldSplitMarkerAtParagraphLocked reports the session split whose empty
// sibling is paragraph p, if the line still holds the bytes Enter wrote. The
// reparse gives the line a nested empty item whose content starts after both
// markers; the live sibling's content starts before the moved marker, where
// the bytes and the caret belong (record 2026-11-11,
// probe60/c6-enter-split-marker-typed).
func (d *Document) heldSplitMarkerAtParagraphLocked(p int) (splitMarker, bool) {
	if d.md == nil || len(d.md.session.splitMarkers) == 0 || p < 0 || p >= d.md.m.Len() {
		return splitMarker{}, false
	}
	start := d.md.m.start(p)
	m, ok := d.splitMarkerAtLocked(lineAt(d.md.p.String(), start).start)
	if !ok || m.innerEnd != start {
		return splitMarker{}, false
	}
	return m, true
}

// setActiveViewLocked publishes the editor showing the document. The strong
// reference keeps the view alive for headless callers that hold only a
// *Document; the weak reference remains for compatibility.
func (d *Document) setActiveViewLocked(v *view) {
	d.active = v
	d.shownIn = weak.Make(v)
}

// activeView is the editor built last that shows the document. The strong
// reference keeps the usual editor reachable; callers that publish a test
// view through shownIn alone get that most recent view.
func (d *Document) activeView() *view {
	if weakView := d.shownIn.Value(); weakView != nil && weakView != d.active {
		return weakView
	}
	return d.active
}

// change is a change of a document: the runes from start to oldEnd
// became those from start to newEnd, at version.
type change struct {
	version               uint64
	start, oldEnd, newEnd int
}

// undoStep is a step of undo: the texts before and after it, which share
// all they did not change, the selections, and the region it changed.
type undoStep struct {
	before, after         Text
	selBefore, selAfter   SourceSelection
	start, oldEnd, newEnd int
	parts                 []change
	mapBefore, mapAfter   SourceMap
	patches               []sourcePatch
	// transient is the view-only state the view showed before this step
	// materialized it: an empty paragraph or a pending line break.
	// Undoing the step shows it again; it is view state, not source bytes.
	transient *transientUndo
	// afterTransient is the view-only state the step leaves after it, shown
	// again when it is redone: the pending line break a deletion opens over
	// the bytes without the emptied line.
	afterTransient *transientUndo
	// sessionBefore and sessionAfter are the live-session annotations on
	// either side of the step. Undo and Redo restore the parser projection
	// and held breaks, while replay maps current-byte holdings.
	sessionBefore, sessionAfter sessionState
	// committedPrefix marks the step that completed a block prefix: the
	// line's markers stopped being text and became a block. Typora ends the
	// typing run there, so the next keystroke starts a step of its own and
	// one Undo lands on the block's own bytes, `- ` for a list item, instead
	// of taking the marker back with everything typed after it (record
	// 2026-11-11, seed11/c13).
	committedPrefix bool
	// promotion is the block prefix this step completed on a line whose bytes
	// the typing run wrote. Its Undo takes that promotion back alone: the
	// bytes stay and read as the text they began as, `- p` for a list item
	// (record 2026-11-12, seed14/c8).
	promotion *literalPromotion
}

// literalPromotion is a block prefix a step completed on a line whose bytes
// the typing run wrote: the hold began inside the run, so Undo reads the
// line as text again instead of taking the bytes back.
type literalPromotion struct {
	line, prefixStart int
	marker            string
}

// maxUndoParts is how many replacements a step of undo keeps: more than
// the edits that move text need (a move is two), and well under the 128
// changes the log keeps for the editor, past which it lays the whole text
// out again. A longer step, as typing a while, is one region.
const maxUndoParts = 16

// NewDocument returns a document of text, its caret at the end.
func NewDocument(text Text) *Document {
	n := text.Len()
	sel := sourceCaret(n)
	return &Document{text: text, anchor: n, caret: n, selection: sel, version: 1}
}

func (d *Document) lock() {
	d.mu.Lock()
	if d.version == 0 {
		d.version = 1 // the zero value is an empty document
	}
}

// Text returns the text of the document, which later edits leave as it is.
func (d *Document) Text() Text {
	d.lock()
	defer d.mu.Unlock()
	return d.text
}

// Version counts the changes of the document's text.
func (d *Document) Version() uint64 {
	d.lock()
	defer d.mu.Unlock()
	return d.version
}

// SetText replaces the text, as when the app opens another note: it is not
// a step of undo, and clears the history. The selection goes to the end.
func (d *Document) SetText(text Text) {
	d.lock()
	defer d.mu.Unlock()
	old := d.text.Len()
	d.text = d.identify(text, 0, text.ParagraphCount()-1)
	d.md = nil
	d.undo, d.redo = nil, nil
	d.coalesce = false
	d.changed(0, old, text.Len())
	n := text.Len()
	d.setSelection(n, n)
}

// Selection returns the selection: anchor is where it started, caret where
// it ends and the caret is, equal for a caret.
func (d *Document) Selection() (anchor, caret int) {
	d.lock()
	defer d.mu.Unlock()
	return d.anchor, d.caret
}

// SetSelection selects the runes between anchor and caret, or puts the
// caret at caret when they are equal; the editor scrolls it into view.
func (d *Document) SetSelection(anchor, caret int) {
	d.lock()
	defer d.mu.Unlock()
	d.setSelection(anchor, caret)
}

func (d *Document) setSelection(anchor, caret int) {
	d.assignSelection(anchor, caret, true)
}

func (d *Document) assignSelection(anchor, caret int, event bool) {
	n := d.text.Len()
	anchor, caret = max(0, min(anchor, n)), max(0, min(caret, n))
	d.selection = d.sourceSelectionForDocLocked(anchor, caret)
	d.anchor, d.caret = anchor, caret
	d.typing = nil
	if event {
		d.coalesce = false
		d.selVersion++
	}
}

// SourceSelection returns the selection in source coordinates.
func (d *Document) SourceSelection() SourceSelection {
	d.lock()
	defer d.mu.Unlock()
	return d.selection
}

// SetSourceSelection selects the source bytes between the points, or puts the
// caret at the caret point when they are equal.
func (d *Document) SetSourceSelection(sel SourceSelection) {
	d.lock()
	defer d.mu.Unlock()
	d.setSourceSelection(sel)
}

func (d *Document) setSourceSelection(sel SourceSelection) {
	d.assignSourceSelection(sel, true)
}

func (d *Document) assignSourceSelection(sel SourceSelection, event bool) {
	sel = d.clampSourceSelectionLocked(sel)
	d.selection = sel
	d.anchor, d.caret = d.docSelectionForSourceLocked(sel)
	d.typing = nil
	if event {
		d.coalesce = false
		d.selVersion++
	}
}

func (d *Document) span() (int, int) {
	return min(d.anchor, d.caret), max(d.anchor, d.caret)
}

// TypingAttributes returns the attributes typing gets at the caret: those
// a command set at the caret, else those of the character before it (after
// it at the start of a paragraph), or of the first selected character,
// without a link, code or tag at their edges, as NSTextView derives them.
func (d *Document) TypingAttributes() Attributes {
	d.lock()
	defer d.mu.Unlock()
	return d.typingAttributes()
}

func (d *Document) typingAttributes() Attributes {
	if d.typing != nil {
		return *d.typing
	}
	a, z := d.span()
	return typingAttributes(d.text, a, z)
}

// typingAttributes returns the attributes typing gets in place of the runes
// from start to end of t.
func typingAttributes(t Text, start, end int) Attributes {
	if start < end {
		a := t.AttributesAt(start)
		a.Attachment = nil
		return a
	}
	ps, _ := t.ParagraphRange(start)
	if start == ps {
		return t.AttributesAt(start).extending()
	}
	before, after := t.AttributesAt(start-1), t.AttributesAt(start)
	if _, pe := t.ParagraphRange(start); start == pe {
		// The newline: a link or code ends with the line, as a selection
		// of whole lines formats newlines too.
		after = Attributes{}
	}
	a := before.extending()
	// What does not extend holds inside: between two runes of a link.
	if before.Link == after.Link {
		a.Link = before.Link
	}
	if before.Code && after.Code {
		a.Code = true
	}
	if before.Tag == after.Tag {
		a.Tag = before.Tag
	}
	return a
}

// SetTypingAttributes sets the attributes the next typed text gets, until
// the caret moves.
func (d *Document) SetTypingAttributes(a Attributes) {
	d.lock()
	defer d.mu.Unlock()
	a.Attachment = nil
	d.typing = &a
}

// changed notes that the runes from start to oldEnd became those from start
// to newEnd.
func (d *Document) changed(start, oldEnd, newEnd int) {
	d.version++
	d.log = append(d.log, change{d.version, start, oldEnd, newEnd})
	if len(d.log) > 256 {
		// The editor reloads a text it is too far behind.
		d.log = append(d.log[:0], d.log[len(d.log)-128:]...)
	}
}

// since returns the change from version v to the document's, as one
// region, the changes it is made of in the order they were made, and
// whether the log holds all of it. The changes are the log's, which the
// next change of the document reuses.
func (d *Document) since(v uint64) (change, []change, bool) {
	if v == d.version {
		return change{version: v}, nil, true
	}
	var c change
	from := -1
	for i, l := range d.log {
		if l.version <= v {
			continue
		}
		if from < 0 {
			if l.version != v+1 {
				return change{}, nil, false
			}
			c, from = l, i
			continue
		}
		c = composeChanges(c, l)
	}
	if from < 0 || c.version != d.version {
		return change{}, nil, false
	}
	return c, d.log[from:], true
}

// composeChanges returns the change a then b make together.
func composeChanges(a, b change) change {
	// Map a's new end through b, and b's old end back through a.
	end := a.newEnd
	switch {
	case end > b.start && end < b.oldEnd:
		end = b.newEnd
	case end >= b.oldEnd:
		end += b.newEnd - b.oldEnd
	}
	old := b.oldEnd
	switch {
	case old > a.start && old < a.newEnd:
		old = a.oldEnd
	case old >= a.newEnd:
		old -= a.newEnd - a.oldEnd
	}
	return change{
		version: b.version,
		start:   min(a.start, b.start),
		oldEnd:  max(a.oldEnd, old),
		newEnd:  max(end, b.newEnd),
	}
}

// Edit changes a document as one step of undo, in
// Document.Edit. Offsets are into the text as the edit's changes
// so far left it, and the selection follows them.
type Edit struct {
	d      *Document
	step   *undoStep
	err    error
	intent EditIntent
	// splitMultiline marks an Enter that first joined a multiline list
	// item's continuation line: the item it splits no longer looks
	// multiline, but a new item formed from its later line takes the next
	// number, as Typora does.
	splitMultiline bool
}

func (e *Edit) Err() error { return e.err }

// Text returns the text as the edit's changes so far left it.
func (e *Edit) Text() Text { return e.d.text }

// Selection returns the selection as the edit's changes so far moved it.
func (e *Edit) Selection() (anchor, caret int) { return e.d.anchor, e.d.caret }

// Select selects the runes between anchor and caret.
func (e *Edit) Select(anchor, caret int) { e.d.setSelection(anchor, caret) }

func (e *Edit) selectMarkdownSource(source int) {
	e.d.setSourceSelection(sourceCaret(source))
}

// selectMarkdownSourceRange selects the source bytes between anchor and
// caret, for an edit that leaves a formatted word selected inside its
// delimiters as Typora does.
func (e *Edit) selectMarkdownSourceRange(anchor, caret int) {
	e.d.assignSourceSelection(SourceSelection{
		Anchor: sourcePoint(anchor, BoundaryUpstream),
		Caret:  sourcePoint(caret, BoundaryDownstream),
	}, true)
}

// Replace replaces the runes from start to end with text.
func (e *Edit) Replace(start, end int, text Text) {
	n := e.d.text.Len()
	start, end = max(0, min(start, end, n)), max(0, min(max(start, end), n))
	if e.d.md != nil {
		e.markdownReplace(start, end, text)
		return
	}
	e.d.apply(e.step, start, end-start, text.Len(), e.d.text.Replace(start, end, text))
}

// ReplaceString replaces the runes from start to end with s, which takes
// attributes a.
func (e *Edit) ReplaceString(start, end int, s string, a Attributes) {
	n := e.d.text.Len()
	start, end = max(0, min(start, end, n)), max(0, min(max(start, end), n))
	if e.d.md != nil {
		e.markdownString(start, end, s, a)
		return
	}
	t := e.d.text.ReplaceString(start, end, s, a)
	e.d.apply(e.step, start, end-start, t.Len()-n+end-start, t)
}

// Format changes the attributes of the runes from start to end.
func (e *Edit) Format(start, end int, fn func(a *Attributes)) {
	n := e.d.text.Len()
	start, end = max(0, min(start, end, n)), max(0, min(max(start, end), n))
	t := e.d.text.Format(start, end, fn)
	if e.d.md != nil {
		if start == end || !t.Same(e.d.text) {
			if err := e.formatInline(start, end, fn); err != nil {
				e.fail(err)
			}
		}
		return
	}
	if !t.Same(e.d.text) {
		e.d.apply(e.step, start, end-start, end-start, t)
	}
}

// FormatParagraphs changes the styles of the paragraphs from the one
// holding start to the one holding end (see Text.FormatParagraphs).
func (e *Edit) FormatParagraphs(start, end int, fn func(p *ParagraphStyle)) {
	t := e.d.text
	pa, pz := t.paragraphSpan(start, end)
	a := t.paraStart(pa)
	z := t.paraStart(pz) + seqAt(t.seq(), pz).n
	if f := t.FormatParagraphs(start, end, fn); !f.Same(t) {
		if e.d.md != nil {
			if err := e.formatMarkdownParagraphs(start, end, f); err != nil {
				e.fail(err)
			}
			return
		}
		e.d.apply(e.step, a, z-a, z-a, f)
	}
}

// apply makes t, in which the runes from start to start+removed of the text
// are now start to start+inserted, the document's text, as part of step.
func (d *Document) apply(step *undoStep, start, removed, inserted int, t Text) {
	t = d.identify(t, t.paraAt(start), t.paraAt(start+inserted))
	formatted := t.text == d.text.text // the text is the same: only formats changed
	d.text = t
	if step != nil {
		part := change{start: start, oldEnd: start + removed, newEnd: start + inserted}
		if step.start < 0 {
			step.start, step.oldEnd, step.newEnd = part.start, part.oldEnd, part.newEnd
			step.parts = []change{part}
		} else {
			c := composeChanges(change{start: step.start, oldEnd: step.oldEnd, newEnd: step.newEnd}, part)
			step.start, step.oldEnd, step.newEnd = c.start, c.oldEnd, c.newEnd
			if step.parts != nil && len(step.parts) < maxUndoParts {
				step.parts = append(step.parts, part)
			} else {
				step.parts = nil
			}
		}
		step.after = t
	}
	d.changed(start, start+removed, start+inserted)
	if formatted {
		return
	}
	a, z := d.span()
	if a == z {
		a = mapPosition(a, start, removed, inserted, false)
		z = a
	} else {
		a, z = mapPosition(a, start, removed, inserted, false), mapPosition(z, start, removed, inserted, true)
	}
	if d.caret >= d.anchor {
		d.anchor, d.caret = a, z
	} else {
		d.anchor, d.caret = z, a
	}
	if d.md == nil {
		d.selection = SourceSelection{
			Anchor: sourcePoint(d.anchor, d.selection.Anchor.Affinity),
			Caret:  sourcePoint(d.caret, d.selection.Caret.Affinity),
		}
	}
	d.typing = nil
}

// mapPosition returns where p goes as the runes from start to
// start+removed become inserted others, as AttributedString tracks indices:
// a caret or the start of a range where text is inserted goes after it,
// the end of a range does not grow, and what was replaced collapses to
// its edges.
func mapPosition(p, start, removed, inserted int, upper bool) int {
	end := start + removed
	switch {
	case p < start:
		return p
	case removed == 0 && p == start:
		if upper {
			return p
		}
		return p + inserted
	case p < end:
		if upper {
			return start + inserted
		}
		return start
	}
	return p + inserted - removed
}

// Edit changes the document with fn, as one step of undo, which undoing
// takes back whole with the selection before it:
//
//	doc.Edit(func(e *richtext.Edit) {
//		e.ReplaceString(0, 0, "Dear Ann,\n", richtext.Attributes{})
//		e.Format(0, 8, func(a *richtext.Attributes) { a.Italic = true })
//	})
func (d *Document) Edit(fn func(e *Edit)) {
	d.lock()
	defer d.mu.Unlock()
	d.edit(false, fn)
}

// edit runs fn as a step of undo, joined to the last one when join is set
// and the last edit was typing a moment ago. A step that completed a block
// prefix is never joined: the run of typing ends where a line became a
// block.
func (d *Document) edit(join bool, fn func(e *Edit)) {
	now := time.Now()
	var beforeSource string
	if d.md != nil {
		beforeSource = d.md.p.String()
	}
	var step *undoStep
	if join && d.coalesce && now.Sub(d.lastEdit) < time.Second && len(d.undo) > 0 &&
		!d.undo[len(d.undo)-1].committedPrefix {
		step = &d.undo[len(d.undo)-1]
	} else {
		d.undo = append(d.undo, undoStep{
			before: d.text, after: d.text, selBefore: d.selection, start: -1,
			sessionBefore: d.state(),
		})
		step = &d.undo[len(d.undo)-1]
	}
	d.run(step, fn)
	if step.start < 0 {
		// Nothing changed.
		if d.md != nil {
			d.md.ok = false
		}
		d.undo = d.undo[:len(d.undo)-1]
		return
	}
	if d.pendingTransient != nil {
		step.transient = d.pendingTransient
		d.pendingTransient = nil
	}
	d.attachPendingPromotion(step)
	if d.md != nil {
		src := d.md.p.String()
		start, oldEnd, newEnd := changedBytes(beforeSource, src)
		d.md.last, d.md.ok = [3]int{start, oldEnd, newEnd}, true
		step.sessionAfter = d.syncFromParser()
		// Only typing completes a prefix: a command that converts the line
		// after the marker's own keystroke is a step of its own already, and
		// the typing after it joins the marker's line (record 2026-11-12,
		// probe94/leading-space-gt-q-undo).
		step.committedPrefix = join && step.oldEnd == step.start &&
			literalLineCommitted(step.sessionBefore.literals, step.sessionAfter.literals, lineAt(src, step.start).start)
	}
	if len(d.undo) > 200 && d.md == nil {
		d.undo[0] = undoStep{}
		d.undo = d.undo[1:]
	}
	step.selAfter = d.selection
	clear(d.redo)
	d.redo = d.redo[:0]
	d.coalesce = join
	d.lastEdit = now
}

// clearPendingTransient drops a held view state no step of the input event
// picked up.
func (d *Document) clearPendingTransient() {
	d.lock()
	defer d.mu.Unlock()
	d.pendingTransient = nil
	d.pendingPromotion = nil
}

// attachPendingPromotion gives the step the block prefix the input wrote on
// a line of its own, leaving the step's bytes as text.
func (d *Document) attachPendingPromotion(step *undoStep) {
	if d.pendingPromotion == nil {
		return
	}
	step.promotion = d.pendingPromotion
	d.pendingPromotion = nil
}

// IdentifyParagraphs has newID give an ID to each paragraph without one.
// Call it before the document is edited; nil stops naming paragraphs.
func (d *Document) IdentifyParagraphs(newID func() string) {
	d.lock()
	defer d.mu.Unlock()
	d.newID = newID
	if t := d.identify(d.text, 0, d.text.ParagraphCount()-1); !t.Same(d.text) {
		d.text = t
		n := t.Len()
		d.changed(0, n, n)
	}
}

// identify returns t with the paragraphs from pa to pz that have no ID
// given one, when the document names paragraphs.
func (d *Document) identify(t Text, pa, pz int) Text {
	if d.newID == nil {
		return t
	}
	return t.formatParas(pa, pz, func(p *ParagraphStyle) {
		if p.ID == "" {
			p.ID = d.newID()
		}
	})
}

// CanUndo and CanRedo report whether there is a step to undo or redo.
func (d *Document) CanUndo() bool {
	d.lock()
	defer d.mu.Unlock()
	return len(d.undo) > 0
}

func (d *Document) CanRedo() bool {
	d.lock()
	defer d.mu.Unlock()
	return len(d.redo) > 0
}

// Undo takes back the last step of undo, and Redo the last step undone,
// restoring the text, its attributes and the selection.
func (d *Document) Undo() {
	d.lock()
	defer d.mu.Unlock()
	d.takeBack(false)
}

func (d *Document) Redo() {
	d.lock()
	defer d.mu.Unlock()
	d.takeBack(true)
}

// takeBackTransient takes back the last step and returns the transient
// states it opens in each direction, the step it took back, and whether
// there was one.
//
// A step that only wrote the bytes of the held paragraph it was opened over
// comes back to the state before the hold began, without the paragraph: the
// step took no content of the user's, so Typora's Undo takes the whole step
// back instead of leaving the line it materialized (record 2026-11-13,
// seed17/c5).
func (d *Document) takeBackTransient(redo bool) (before, after *transientUndo, step undoStep, ok bool) {
	d.lock()
	defer d.mu.Unlock()
	before, after, step, ok = d.takeBack(redo)
	if !redo && materializedHeldParagraphOnly(step) {
		before = nil
	}
	return before, after, step, ok
}

// materializedHeldParagraphOnly reports whether the step wrote nothing but
// the line the view held for a paragraph that stands on its own: an item the
// Enter before it opened, whose marker the press wrote with it. Undo takes
// that item back whole, as Typora does, instead of leaving the line the hold
// showed (record 2026-11-13, seed17/c5). A held paragraph that writes only
// the blank line between two blocks keeps its hold instead: the line is the
// paragraph's own bytes, and Undo restores the hold it replaced.
func materializedHeldParagraphOnly(s undoStep) bool {
	held := s.transient
	if held == nil || s.promotion != nil || held.mode == transientPendingBreak ||
		held.mode == transientEmptiedBreak || len(s.patches) != 1 {
		return false
	}
	c := s.patches[0]
	if c.at != held.patch.at || c.old != held.patch.old {
		return false
	}
	// The line's ending follows the document's, so the two patches compare
	// without it.
	wrote := strings.TrimRight(c.new, "\r\n")
	return wrote != "" && wrote == strings.TrimRight(held.patch.new, "\r\n") &&
		strings.TrimSpace(wrote) != ""
}

func (d *Document) takeBack(redo bool) (before, after *transientUndo, step undoStep, ok bool) {
	from, to := &d.undo, &d.redo
	if redo {
		from, to = to, from
	}
	if d.md != nil {
		d.md.ok = false
	}
	n := len(*from)
	if n == 0 {
		return nil, nil, undoStep{}, false
	}
	s := (*from)[n-1]
	(*from)[n-1] = undoStep{}
	*from = (*from)[:n-1]
	if s.promotion != nil {
		// The run wrote the line's bytes and then read their markers as a
		// block. Typora's Undo takes that promotion back alone: the bytes
		// stay and read as the text they began as, `- p` for a list item,
		// with the caret where the run left it, and Redo reads them as the
		// block again. Neither direction moves a byte, so neither replays
		// the run's patches (record 2026-11-12, seed14/c8).
		lit := s.promotedSession()
		if redo {
			lit = s.sessionAfter.clone()
		}
		if d.md != nil {
			d.reparseLiteralLines(lit)
		}
		d.setSourceSelection(s.selAfter)
		*to = append(*to, s)
		// The line is the run's own bytes: Undo takes no hold back with it, so
		// no transient paragraph comes back either way.
		return nil, nil, s, true
	}
	if d.md != nil {
		d.replay(s, redo)
		// The undo step's own offsets are into the projected text; the
		// source patches take the closing sequences through in source
		// coordinates, in the order the replay applies them.
		if redo {
			for _, c := range s.patches {
				d.mapReplayLiveClosers(c.at, c.at+len(c.old), c.at+len(c.new))
			}
		} else {
			for k := len(s.patches) - 1; k >= 0; k-- {
				c := s.patches[k]
				d.mapReplayLiveClosers(c.at, c.at+len(c.new), c.at+len(c.old))
			}
		}
	}
	session := s.sessionBefore.clone()
	if redo {
		d.text = s.after
		if d.md == nil {
			for _, p := range s.parts {
				d.changed(p.start, p.oldEnd, p.newEnd)
			}
			if s.parts == nil {
				d.changed(s.start, s.oldEnd, s.newEnd)
			}
		} else {
			d.changed(s.start, s.oldEnd, s.newEnd)
		}
		session = s.sessionAfter.clone()
	} else {
		d.text = s.before
		if d.md == nil {
			for i := len(s.parts) - 1; i >= 0; i-- {
				p := s.parts[i]
				d.changed(p.start, p.newEnd, p.oldEnd)
			}
			if s.parts == nil {
				d.changed(s.start, s.newEnd, s.oldEnd)
			}
		} else {
			d.changed(s.start, s.newEnd, s.oldEnd)
		}
	}
	if d.md != nil {
		projection := d.state()
		d.setState(session)
		if !sameLiteralLines(s.sessionBefore.literals, s.sessionAfter.literals) ||
			!sameLiteralLines(projection.literals, session.literals) {
			// The step changed which lines' markers are text: the bytes are
			// the same but their blocks are not, so the source is parsed
			// again under the set this direction restored (record 2026-11-12,
			// probe94/leading-space-gt-undo). The replay drops a line's
			// record when its own start is the range a patch replaced, though
			// the bytes the step put back hold the markers as text again: the
			// line the session typed reads its marker as a block otherwise
			// (record 2026-11-13, issue177: a dash on the second line of
			// `编辑` reads back as a setext underline, and the caret moves
			// off the line's end).
			d.reparseLiteralLines(session)
		}
	}
	if redo {
		d.setSourceSelection(s.selAfter)
	} else {
		d.setSourceSelection(s.selBefore)
	}
	*to = append(*to, s)
	return s.transient, s.afterTransient, s, true
}

// BreakUndoCoalescing ends the step typing adds to, as at a save, so that
// what is typed next undoes on its own.
func (d *Document) BreakUndoCoalescing() {
	d.lock()
	defer d.mu.Unlock()
	d.coalesce = false
}

// promotedSession returns the session the source parses under once Undo takes
// the step's promotion back: the set the step left, with the promoted line
// held as text from where its markers end.
func (s undoStep) promotedSession() sessionState {
	state := s.sessionAfter.clone()
	lines := state.literals
	for _, l := range lines {
		if l.Line == s.promotion.line {
			return state
		}
	}
	lines = append(lines, parser.LiteralLine{Line: s.promotion.line, Content: s.promotion.prefixStart})
	slices.SortFunc(lines, func(a, b parser.LiteralLine) int { return a.Line - b.Line })
	state.literals = lines
	return state
}

// sameLiteralLines reports whether two literal-line sets hold the same lines.
func sameLiteralLines(a, b []parser.LiteralLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// literalLineCommitted reports whether a step that inserted bytes ended the
// hold on the line at lineStart: the keystroke completed the block prefix
// the line spelled as text, so the line now parses as the block its markers
// name. Typora ends the typing run there, and one Undo of the typing after
// it lands on the block's own bytes (record 2026-11-11, seed11/c13).
func literalLineCommitted(before, after []parser.LiteralLine, lineStart int) bool {
	held := false
	for _, l := range before {
		if l.Line == lineStart {
			held = true
			break
		}
	}
	if !held {
		return false
	}
	for _, l := range after {
		if l.Line == lineStart {
			return false
		}
	}
	return true
}
