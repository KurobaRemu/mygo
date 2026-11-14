package richtext

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

type ViewportAnchor struct {
	Source       int
	Offset       float64
	CaretVisible bool
	CaretY       float64
}

func (d *Document) RichSelectionFromSource(srcAnchor, srcCaret int) (int, int) {
	d.lock()
	v := d.mappingViewLocked()
	d.mu.Unlock()
	return v.richSelectionFromSource(srcAnchor, srcCaret)
}

func (d *Document) mappingViewLocked() *view {
	if d.activeView() != nil {
		d.activeView().lockedSync()
		return d.activeView()
	}
	v := &view{doc: d, reveal: true, hoverPara: -1, hoverBox: -1, pfx: newPrefixTracker()}
	v.shown = d.text
	if d.md != nil {
		v.source = d.md.m
	}
	v.selAnchor, v.caret = d.anchor, d.caret
	return v
}

func (v *view) sourceForCaret(doc int) int {
	if v.source.Len() == 0 {
		return max(0, min(doc, v.shown.Len()))
	}
	doc = max(0, min(doc, v.shown.Len()))
	p := v.shown.paraAt(doc)
	if p >= v.source.Len() {
		if v.transient != nil && p == v.transient.para {
			return v.transient.anchor.Offset
		}
		return len(v.doc.md.p.String())
	}
	pm := seqAt(v.source.paras, p)
	start := v.source.start(p)
	if pm.src == 0 || len(pm.segs) == 0 {
		local := doc - v.shown.paraStart(p)
		return v.doc.sourceAtParagraph(p, local)
	}
	local := doc - v.shown.paraStart(p)
	if end, ok := v.boxBoundarySource(p, local, BoundaryDownstream); ok {
		return end
	}
	total := 0
	for _, s := range pm.segs {
		total += s.Shown
	}
	if local >= total {
		return v.paragraphEndSource(p)
	}
	if source, ok := v.boxCaretBoundarySource(p, local); ok {
		return source
	}
	shown, cursor := 0, start
	for _, s := range pm.segs {
		if shown == local {
			return cursor
		}
		if s.Shown > 0 && local < shown+s.Shown {
			if s.Kind == SegmentVerbatim {
				body := v.sourceSlice(cursor, cursor+s.Source)
				return cursor + byteOfRune(body, max(0, local-shown))
			}
			return cursor
		}
		shown += s.Shown
		cursor += s.Source
	}
	return cursor
}

func (v *view) sourceForDoc(doc int, upper bool) int {
	if v.source.Len() == 0 {
		return max(0, min(doc, v.shown.Len()))
	}
	doc = max(0, min(doc, v.shown.Len()))
	p := v.shown.paraAt(doc)
	if p >= v.source.Len() {
		if v.transient != nil && p == v.transient.para {
			return v.transient.anchor.Offset
		}
		return len(v.doc.md.p.String())
	}
	pm := seqAt(v.source.paras, p)
	start := v.source.start(p)
	if pm.src == 0 || len(pm.segs) == 0 {
		local := doc - v.shown.paraStart(p)
		return v.doc.sourceAtParagraph(p, local)
	}
	local := doc - v.shown.paraStart(p)
	if upper {
		if end, ok := v.boxSelectionBoundarySource(p, local); ok {
			return end
		}
	} else if start, ok := v.boxBoundarySource(p, local, BoundaryUpstream); ok {
		return start
	}
	total := 0
	for _, s := range pm.segs {
		total += s.Shown
	}
	if local >= total {
		return v.paragraphEndSource(p)
	}
	shown, cursor := 0, start
	for _, s := range pm.segs {
		if local == shown && upper {
			return cursor
		}
		if s.Shown <= 0 {
			cursor += s.Source
			continue
		}
		if local < shown+s.Shown {
			if s.Kind == SegmentVerbatim {
				body := v.sourceSlice(cursor, cursor+s.Source)
				return cursor + byteOfRune(body, max(0, local-shown))
			}
			if local > shown && upper {
				return cursor + s.Source
			}
			return cursor
		}
		shown += s.Shown
		cursor += s.Source
	}
	return cursor
}

func (v *view) boxBoundarySource(p, local int, affinity BoundaryAffinity) (int, bool) {
	if affinity == BoundaryUpstream {
		best, found := 0, false
		for _, b := range v.boxes(p) {
			// A hard break holds no content: the caret at its boundary is
			// beside the break, never between its marker and the newline.
			if b.kind.hardBreak() || local != b.docStart {
				continue
			}
			source := b.contentFrom
			if b.contentFrom == b.contentTo && b.close >= 0 && markerExtendsTyping(b.kind) {
				// An empty pair parsed from the source holds no content
				// point: a caret reaching its visible start from the right
				// stands after the whole delimiter run, as the empty strong
				// of `****a**` does (record 2026-11-10, probe75/rh).
				source = b.end
			}
			if !found || source > best {
				best, found = source, true
			}
		}
		return best, found
	}
	outer, outerSet := 0, false
	inner, innerSet := 0, false
	for _, b := range v.boxes(p) {
		if b.kind.hardBreak() || local != b.docEnd {
			continue
		}
		if markerExtendsTyping(b.kind) {
			source := b.closeStart
			if source < b.contentFrom || source > b.end {
				source = b.end
			}
			if !innerSet || source < inner {
				inner, innerSet = source, true
			}
		} else if !outerSet || b.end > outer {
			outer, outerSet = b.end, true
		}
	}
	if outerSet {
		return outer, true
	}
	return inner, innerSet
}

// boxSelectionBoundarySource maps the end of a selection at the visible end
// of inline elements. The innermost element at that boundary owns the end:
// a selection over a link's bold label ends inside the label, not after the
// link destination.
func (v *view) boxSelectionBoundarySource(p, local int) (int, bool) {
	box, found := box{}, false
	for _, b := range v.boxes(p) {
		// Above: a hard break holds no content.
		if b.kind.hardBreak() || local != b.docEnd {
			continue
		}
		if !found || b.end < box.end {
			box, found = b, true
		}
	}
	if !found {
		return 0, false
	}
	if markerExtendsTyping(box.kind) {
		source := box.closeStart
		if source < box.contentFrom || source > box.end {
			source = box.end
		}
		return source, true
	}
	return box.contentTo, true
}

// boxCaretBoundarySource maps a collapsed caret at the visible start of an
// inline element to the source before its opening marker. A selection
// anchor at that same doc boundary instead starts at the first visible
// character, so sourceForDoc handles that case separately.
func (v *view) boxCaretBoundarySource(p, local int) (int, bool) {
	best, found := 0, false
	for _, b := range v.boxes(p) {
		if local != b.docStart {
			continue
		}
		if !found || b.start < best {
			best, found = b.start, true
		}
	}
	return best, found
}

func markerExtendsTyping(kind MarkerKind) bool {
	switch kind {
	case MarkerStrong, MarkerEmphasis, MarkerStrikethrough, MarkerHTML:
		return true
	default:
		return false
	}
}

func (v *view) richSelectionFromSource(srcAnchor, srcCaret int) (int, int) {
	anchor, caret := v.docForSource(srcAnchor, false), v.docForSource(srcCaret, true)
	if srcAnchor > srcCaret {
		anchor, caret = v.docForSource(srcCaret, false), v.docForSource(srcAnchor, true)
	}
	if anchor > caret {
		anchor, caret = caret, anchor
	}
	if srcAnchor > srcCaret {
		return caret, anchor
	}
	return anchor, caret
}

func (v *view) docForSource(source int, upper bool) int {
	if v.source.Len() == 0 {
		return max(0, min(source, v.shown.Len()))
	}
	source = max(0, source)
	startP := 0
	if source > 0 {
		startP = max(0, v.source.from(source)-1)
	}
	for p := startP; p < v.source.Len() && p < v.shown.ParagraphCount(); p++ {
		pm := seqAt(v.source.paras, p)
		start := v.source.start(p)
		end := start + pm.src
		ps := v.shown.paraStart(p)
		if source < start {
			// The bytes between paragraphs separate them, except a line's
			// trailing bytes the projection drops, such as a table row's
			// closing space: a caret after them is still at that line's end.
			if p > 0 && v.sourceEndsParagraphLine(p-1, source) {
				return v.shown.paraStart(p-1) + v.para(p-1).n
			}
			return ps
		}
		if source <= end {
			if pm.src == 0 || len(pm.segs) == 0 {
				return ps
			}
			return v.docForSourceInParagraph(p, source-start, upper)
		}
	}
	return v.shown.Len()
}

// sourceEndsParagraphLine reports whether source lies on the source line
// that holds paragraph p's last bytes. The paragraph's own source ends
// before the line does when the projection drops the line's trailing bytes;
// the caret after them is still at the paragraph's drawn end.
func (v *view) sourceEndsParagraphLine(p, source int) bool {
	if p < 0 || p >= v.source.Len() {
		return false
	}
	pm := seqAt(v.source.paras, p)
	end := v.source.start(p) + pm.src
	if pm.src <= 0 || source < end {
		return false
	}
	src := v.sourceText()
	last, line := lineAt(src, end-1), lineAt(src, source)
	return line.start == last.start && line.end == last.end
}

func (v *view) docForSourceInParagraph(p, local int, upper bool) int {
	pm := seqAt(v.source.paras, p)
	ps := v.shown.paraStart(p)
	shown, cursor := 0, 0
	for _, s := range pm.segs {
		next := cursor + s.Source
		if local < next || local == next && s.Kind == SegmentGap {
			if s.Kind == SegmentVerbatim {
				body := v.sourceSlice(v.source.start(p)+cursor, v.source.start(p)+next)
				return ps + shown + runesInBytes(body, local-cursor)
			}
			if s.Shown <= 0 {
				return ps + shown
			}
			// A synthetic segment's shown runes cover its source in the
			// shares the display map gives them, so a source offset inside
			// the segment maps between two of its runes: a caret inside a
			// run sits before it (upstream) or after it (downstream). The
			// source the run starts at belongs to that run.
			body := v.sourceSlice(v.source.start(p)+cursor, v.source.start(p)+next)
			srcRunes := utf8.RuneCountInString(body)
			want := local - cursor
			boundary := func(i int) int {
				if i <= 0 {
					return 0
				}
				return sourceAdvance(body, srcRunes, i-1, s.Shown)
			}
			if upper {
				at := 0
				for at < s.Shown && boundary(at+1) <= want {
					at++
				}
				return ps + shown + at
			}
			for i := 0; i <= s.Shown; i++ {
				if boundary(i) >= want {
					return ps + shown + i
				}
			}
			return ps + shown + s.Shown
		}
		cursor = next
		shown += s.Shown
	}
	return ps + shown
}

func runesInBytes(s string, n int) int {
	n = max(0, min(n, len(s)))
	count := 0
	for n > 0 && len(s) > 0 {
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if n < size {
			break
		}
		n -= size
		count++
	}
	return count
}

func (d *Document) ScrollAnchor() ViewportAnchor {
	d.lock()
	v := d.activeView()
	if v != nil {
		v.lockedSync()
	}
	src := d.viewport
	set := d.viewportVersion > 0
	d.mu.Unlock()
	if v == nil {
		if set {
			return src
		}
		return ViewportAnchor{}
	}
	return v.viewportAnchor()
}

func (d *Document) SetScrollAnchor(a ViewportAnchor) {
	d.lock()
	d.viewport = a
	d.viewportVersion++
	v := d.activeView()
	d.mu.Unlock()
	if v != nil {
		v.sync()
		v.applyViewportAnchor(a)
	}
}

func (v *view) viewportAnchor() ViewportAnchor {
	a := ViewportAnchor{Source: v.sourceAtDoc(v.anchor), Offset: v.scroll - v.hs.top(v.anchor)}
	if v.viewH > 0 {
		if p, l, _ := v.layoutPositionAt(0, v.scroll); p >= 0 {
			at := v.global(p, l)
			a.Source = v.sourceAtDoc(at)
			if pv := v.paraLayout(p); pv.display != nil {
				o := pv.display.Map.At(l)
				a.Source = o.Source
			}
			_, y, _ := v.caretAt(at, 0, v.affinity)
			a.Offset = v.scroll - y
		}
		if _, y, h := v.caretAt(v.caret, v.composeCaret, v.affinity); y+float64(v.padY)-v.scroll >= 0 && y+float64(h)+float64(v.padY)-v.scroll <= float64(v.viewH) {
			a.Source = v.sourceAtDoc(v.caret)
			if v.pendingSet && v.caret == v.pendingRange[0] {
				a.Source = v.pendingSelection.Anchor.Offset
			} else {
				v.doc.lock()
				if v.caret == v.doc.caret {
					a.Source = v.doc.selection.Caret.Offset
				}
				v.doc.mu.Unlock()
			}
			a.CaretVisible = true
			a.CaretY = y + float64(v.padY) - v.scroll
		}
	}
	if a.Source < 0 {
		a.Source = 0
	}
	return a
}

func (v *view) applyViewportAnchor(a ViewportAnchor) {
	if len(v.paras) == 0 {
		v.anchor, v.anchorOff = 0, 0
		return
	}
	at, ok := v.docAtSource(max(0, a.Source))
	if !ok {
		v.setScroll(max(0, a.Offset))
		return
	}
	_, y, _ := v.caretAt(at, 0, v.affinity)
	if a.CaretVisible {
		v.setScroll(max(0, y+float64(v.padY)-a.CaretY))
		return
	}
	v.setScroll(max(0, y+a.Offset))
}

type eolLine struct {
	sourceStart      int
	sourceContentEnd int
	sourceEnd        int
	bufferStart      int
	bufferEnd        int
	eol              string
}

type eolTable struct {
	source     string
	normalized string
	lines      []eolLine
	dominant   string
}

func newEOLTable(source string) eolTable {
	t := eolTable{source: source}
	var out strings.Builder
	crlf, lf := 0, 0
	sourceStart, bufferStart := 0, 0
	for i := 0; i < len(source); i++ {
		if source[i] != '\n' {
			continue
		}
		contentEnd := i
		eol := "\n"
		if i > sourceStart && source[i-1] == '\r' {
			contentEnd = i - 1
			eol = "\r\n"
			crlf++
		} else {
			lf++
		}
		content := source[sourceStart:contentEnd]
		out.WriteString(content)
		out.WriteByte('\n')
		bufferEnd := bufferStart + len(content)
		t.lines = append(t.lines, eolLine{
			sourceStart: sourceStart, sourceContentEnd: contentEnd, sourceEnd: i + 1,
			bufferStart: bufferStart, bufferEnd: bufferEnd, eol: eol,
		})
		sourceStart, bufferStart = i+1, bufferEnd+1
	}
	content := source[sourceStart:]
	out.WriteString(content)
	t.lines = append(t.lines, eolLine{
		sourceStart: sourceStart, sourceContentEnd: len(source), sourceEnd: len(source),
		bufferStart: bufferStart, bufferEnd: bufferStart + len(content),
	})
	t.normalized = out.String()
	t.dominant = "\n"
	if crlf > lf {
		t.dominant = "\r\n"
	}
	return t
}

func (t eolTable) lineAtBuffer(offset int) int {
	if len(t.lines) == 0 {
		return 0
	}
	offset = max(0, min(offset, len(t.normalized)))
	i := sort.Search(len(t.lines), func(i int) bool { return t.lines[i].bufferEnd >= offset })
	if i >= len(t.lines) {
		return len(t.lines) - 1
	}
	return i
}

func (t eolTable) toSource(offset int) int {
	if len(t.lines) == 0 {
		return max(0, min(offset, len(t.source)))
	}
	line := t.lines[t.lineAtBuffer(offset)]
	if offset <= line.bufferEnd {
		return line.sourceStart + max(0, min(offset-line.bufferStart, line.sourceContentEnd-line.sourceStart))
	}
	return line.sourceEnd
}

func (t eolTable) fromSource(source int) int {
	if len(t.lines) == 0 {
		return max(0, min(source, len(t.normalized)))
	}
	source = max(0, min(source, len(t.source)))
	i := sort.Search(len(t.lines), func(i int) bool { return t.lines[i].sourceEnd >= source })
	if i >= len(t.lines) {
		return len(t.normalized)
	}
	line := t.lines[i]
	if source == line.sourceEnd {
		if i+1 < len(t.lines) {
			return t.lines[i+1].bufferStart
		}
		return line.bufferEnd
	}
	if source <= line.sourceContentEnd {
		return line.bufferStart + max(0, source-line.sourceStart)
	}
	return line.bufferEnd
}

func (t eolTable) sourceText(text string, offset int) string {
	eol := t.dominant
	if len(t.lines) > 0 {
		eol = t.lines[t.lineAtBuffer(offset)].eol
		if eol == "" {
			eol = t.dominant
		}
	}
	return strings.ReplaceAll(text, "\n", eol)
}

type SourceModeSession struct {
	Document *Document
	Buffer   *editor.Buffer
	State    editor.State
	Err      error

	active          bool
	rendered        bool
	bufferVersion   uint64
	documentVersion uint64
	eol             eolTable
	enteredAnchor   ViewportAnchor
}

func NewSourceModeSession(doc *Document) *SourceModeSession {
	return &SourceModeSession{Document: doc}
}

func (s *SourceModeSession) Active() bool { return s != nil && s.active }

func (s *SourceModeSession) Enter() error {
	if s == nil || s.Document == nil {
		return fmt.Errorf("richtext: source mode needs a document")
	}
	if s.active {
		return nil
	}
	if v := s.Document.activeView(); v != nil {
		v.commitCompose()
		v.maybeLeaveEmpty()
		v.discardTransient()
		s.Document.syncViewSelection()
	}
	anchor := s.Document.ScrollAnchor()
	s.enteredAnchor = anchor
	s.rendered = false
	s.Document.BreakUndoCoalescing()
	s.eol = newEOLTable(s.Document.Source())
	s.Buffer = editor.NewBuffer(s.eol.normalized)
	s.Buffer.SetLanguage(editor.LanguageNamed("markdown"))
	s.State = editor.State{
		Buffer:      s.Buffer,
		SoftWrap:    true,
		LineNumbers: editor.LineNumbersOn,
		Whitespace:  editor.WhitespaceAll,
		TabSize:     2,
	}
	s.installInput()
	s.bufferVersion = s.Buffer.Version()
	s.documentVersion = s.Document.Version()
	sel := s.Document.SourceSelection()
	s.State.SetSelectionsNoReveal(editor.Selection{Anchor: s.eol.fromSource(sel.Anchor.Offset), Head: s.eol.fromSource(sel.Caret.Offset)})
	s.restoreSourceScroll(anchor)
	s.active = true
	return nil
}

func (s *SourceModeSession) Leave() error {
	if !s.Active() {
		return nil
	}
	if err := s.Sync(); err != nil {
		return err
	}
	anchor := s.enteredAnchor
	if s.rendered {
		anchor = s.sourceViewportAnchor()
	}
	s.Document.BreakUndoCoalescing()
	sel := s.State.Selection()
	a, z := s.eol.toSource(sel.Anchor), s.eol.toSource(sel.Head)
	s.Document.selectFromSource(a, z)
	s.Document.SetScrollAnchor(anchor)
	s.active = false
	s.rendered = false
	s.Buffer = nil
	s.State = editor.State{}
	s.State.Selections()
	s.bufferVersion, s.documentVersion = 0, 0
	return nil
}

func (s *SourceModeSession) Close() error { return s.Leave() }

func (s *SourceModeSession) View(c *ui.Context) ui.Element {
	if !s.Active() {
		if err := s.Enter(); err != nil {
			s.Err = err
			return ui.Box(c)
		}
	}
	e := editor.View(c, &s.State)
	s.rendered = true
	if err := s.Sync(); err != nil {
		s.Err = err
	}
	return e
}

func (s *SourceModeSession) Sync() error {
	if !s.Active() {
		return nil
	}
	if s.Err != nil {
		return s.Err
	}
	changes, version, ok := s.Buffer.ChangesSince(s.bufferVersion)
	if !ok {
		return fmt.Errorf("richtext: source mode fell behind the edit log")
	}
	if len(changes) == 0 && s.Document.Version() != s.documentVersion {
		return s.syncBufferFromDocument()
	}
	for i := 0; i < len(changes); {
		j := i + 1
		for j < len(changes) && changes[j].Version == changes[i].Version {
			j++
		}
		if err := s.applyChanges(changes[i:j]); err != nil {
			s.Err = err
			return err
		}
		s.eol = newEOLTable(s.Document.Source())
		i = j
	}
	s.bufferVersion = version
	s.documentVersion = s.Document.Version()
	return nil
}

func (s *SourceModeSession) applyChanges(changes []editor.TextChange) error {
	type patch struct {
		from, to int
		text     string
	}
	patches := make([]patch, 0, len(changes))
	typing := false
	for _, c := range changes {
		patches = append(patches, patch{
			from: s.eol.toSource(c.Start),
			to:   s.eol.toSource(c.End),
			text: s.eol.sourceText(c.Text, c.Start),
		})
		typing = typing || c.Typing
	}
	sort.SliceStable(patches, func(i, j int) bool { return patches[i].from > patches[j].from })
	d := s.Document
	var err error
	d.lock()
	d.edit(typing, func(e *Edit) {
		for _, p := range patches {
			e.ReplaceSource(p.from, p.to, p.text)
			if e.Err() != nil {
				err = e.Err()
				return
			}
		}
	})
	d.mu.Unlock()
	if err != nil {
		return err
	}
	s.eol = newEOLTable(d.Source())
	s.bufferVersion = changes[len(changes)-1].Version
	s.documentVersion = d.Version()
	s.publishSourceSelection(typing)
	return nil
}

func (s *SourceModeSession) publishSourceSelection(typing bool) {
	sel := s.State.Selection()
	a, z := s.eol.toSource(sel.Anchor), s.eol.toSource(sel.Head)
	d := s.Document
	d.lock()
	d.assignSourceSelection(SourceSelection{
		Anchor: sourcePoint(a, boundaryAffinity(a, z, true)),
		Caret:  sourcePoint(z, boundaryAffinity(a, z, false)),
	}, true)
	if n := len(d.undo); n > 0 {
		d.undo[n-1].selAfter = d.selection
	}
	d.coalesce = typing
	d.mu.Unlock()
}

func (s *SourceModeSession) syncBufferFromDocument() error {
	source := s.Document.Source()
	next := newEOLTable(source)
	oldText := s.Buffer.String()
	newText := next.normalized
	if oldText != newText {
		start, end, insert := textDifference(oldText, newText)
		scroll := s.State.ScrollY()
		s.Buffer.Edit(editor.Edit{Start: start, End: end, Text: insert})
		s.State.SetScrollY(scroll)
	}
	s.eol = next
	s.bufferVersion = s.Buffer.Version()
	s.documentVersion = s.Document.Version()
	sel := s.Document.SourceSelection()
	anchor, head := s.eol.fromSource(sel.Anchor.Offset), s.eol.fromSource(sel.Caret.Offset)
	s.State.SetSelections(editor.Selection{Anchor: anchor, Head: head})
	return nil
}

func textDifference(oldText, newText string) (start, end int, insert string) {
	for start < len(oldText) && start < len(newText) && oldText[start] == newText[start] {
		start++
	}
	tail := 0
	for tail < len(oldText)-start && tail < len(newText)-start && oldText[len(oldText)-1-tail] == newText[len(newText)-1-tail] {
		tail++
	}
	return start, len(oldText) - tail, newText[start : len(newText)-tail]
}

func (s *SourceModeSession) installInput() {
	s.State.OnInput = func(ev ui.InputEvent) bool {
		return s.handleUndo(ev)
	}
	s.State.Bindings = []editor.Binding{
		{Mods: ui.Cmd, Key: ui.KeySlash, Command: editor.NoCommand},
		{Mods: ui.Cmd, Key: ui.KeyZ, Command: editor.NoCommand},
		{Mods: ui.Cmd | ui.Shift, Key: ui.KeyZ, Command: editor.NoCommand},
		{Mods: ui.Cmd, Key: ui.KeyY, Command: editor.NoCommand},
	}
	if ui.Cmd != ui.Ctrl {
		s.State.Bindings = append(s.State.Bindings,
			editor.Binding{Mods: ui.Ctrl, Key: ui.KeySlash, Command: editor.NoCommand},
			editor.Binding{Mods: ui.Ctrl, Key: ui.KeyZ, Command: editor.NoCommand},
			editor.Binding{Mods: ui.Ctrl | ui.Shift, Key: ui.KeyZ, Command: editor.NoCommand},
			editor.Binding{Mods: ui.Ctrl, Key: ui.KeyY, Command: editor.NoCommand},
		)
	}
}

func (s *SourceModeSession) handleUndo(ev ui.InputEvent) bool {
	redo := false
	switch ev.Kind {
	case ui.InputCommand:
		switch ev.Text {
		case "undo":
		case "redo":
			redo = true
		default:
			return false
		}
	case ui.InputKeyDown:
		mod := ui.Cmd
		if runtime.GOOS != "darwin" {
			mod = ui.Ctrl
		}
		switch {
		case ev.Key == ui.KeyZ && ev.Mods == mod:
		case ev.Key == ui.KeyZ && ev.Mods == mod|ui.Shift:
			redo = true
		case ev.Key == ui.KeyY && ev.Mods&mod != 0:
			redo = true
		default:
			return false
		}
	default:
		return false
	}
	if err := s.Sync(); err != nil {
		s.Err = err
		return true
	}
	s.Document.BreakUndoCoalescing()
	if redo {
		s.Document.Redo()
	} else {
		s.Document.Undo()
	}
	if err := s.syncBufferFromDocument(); err != nil {
		s.Err = err
	}
	return true
}

func (s *SourceModeSession) restoreSourceScroll(a ViewportAnchor) {
	if a.CaretVisible {
		_, y, _ := s.sourceCaretRect()
		s.State.SetScrollY(max(0, y-a.CaretY))
		return
	}
	offset := s.eol.fromSource(a.Source)
	line := s.eol.lineAtBuffer(offset)
	s.State.SetScrollY(max(0, s.State.LineTop(line)+a.Offset))
}

func (s *SourceModeSession) sourceCaretRect() (line int, y, h float64) {
	sel := s.State.Selection()
	pos := s.Buffer.Pos(sel.Head)
	line = pos.Line
	if r, ok := s.State.CaretRect(); ok {
		return line, float64(r.Y), float64(r.H)
	}
	top := s.State.LineTop(line)
	next := s.State.LineTop(line + 1)
	return line, top, max(1, next-top)
}

func (s *SourceModeSession) sourceViewportAnchor() ViewportAnchor {
	if s.Buffer == nil {
		return ViewportAnchor{}
	}
	sel := s.State.Selection()
	line := s.Buffer.Pos(sel.Head).Line
	first, last := s.State.VisibleLines()
	if r, ok := s.State.CaretRect(); ok && line >= first && line <= last {
		top, bottom := s.State.LineTop(first), s.State.LineTop(last+1)
		if float64(r.Y) >= top && float64(r.Y+r.H) <= bottom {
			return ViewportAnchor{Source: s.eol.toSource(sel.Head), CaretVisible: true, CaretY: float64(r.Y) - s.State.ScrollY()}
		}
	}
	top := first
	scroll := s.State.ScrollY()
	for top+1 < s.Buffer.LineCount() && s.State.LineTop(top+1) <= scroll {
		top++
	}
	if top >= len(s.eol.lines) {
		top = len(s.eol.lines) - 1
	}
	source := s.eol.toSource(s.eol.lines[top].bufferStart)
	return ViewportAnchor{Source: source, Offset: max(0, scroll-s.State.LineTop(top))}
}

func (d *Document) syncViewSelection() {
	d.lock()
	if d.activeView() != nil {
		v := d.activeView()
		v.selAnchor, v.caret = d.anchor, d.caret
	}
	d.mu.Unlock()
}

func (d *Document) selectFromSource(srcAnchor, srcCaret int) {
	d.SetSourceSelection(SourceSelection{
		Anchor: sourcePoint(srcAnchor, boundaryAffinity(srcAnchor, srcCaret, true)),
		Caret:  sourcePoint(srcCaret, boundaryAffinity(srcAnchor, srcCaret, false)),
	})
}
