package richtext

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

type EditIntent uint8

const (
	IntentNone EditIntent = iota
	IntentEnter
	IntentShiftEnter
	IntentBackspaceAtStart
	IntentDeleteAtEnd
	IntentIndent
	IntentOutdent
	IntentToggleTask
	IntentExitBlock
	// IntentExitDocument opens a paragraph below the document's last block.
	// The line a move reaches past the document's bytes holds no container,
	// so the paragraph it writes belongs to the document root (record
	// 2026-11-10, exitprobe/*-down-eof).
	IntentExitDocument
	IntentFormatInline
	IntentFormatBlock
	IntentCrossBlockReplace
	IntentPasteMarkdown
	IntentPastePlain
)

type EditRequest struct {
	Intent EditIntent
	Start  int
	End    int
	Text   string
	Level  int
	Shift  bool
}

func (e *Edit) SetIntent(intent EditIntent) {
	e.intent = intent
}

func (d *Document) ApplyIntent(intent EditIntent) error {
	d.lock()
	defer d.mu.Unlock()
	a, z := d.span()
	return d.applyIntent(EditRequest{Intent: intent, Start: a, End: z})
}

func (d *Document) ApplyIntentAt(intent EditIntent, start, end int, text string) error {
	d.lock()
	defer d.mu.Unlock()
	return d.applyIntent(EditRequest{Intent: intent, Start: start, End: end, Text: text})
}

func (d *Document) applyIntent(req EditRequest) error {
	if d.md == nil {
		return ErrStructuralEdit
	}
	n := d.text.Len()
	req.Start = max(0, min(req.Start, n))
	req.End = max(0, min(req.End, n))
	if req.Start > req.End {
		req.Start, req.End = req.End, req.Start
	}
	// The paragraphs a split, join, or deletion rebuild parse from the
	// bytes alone: the breaks the session typed in them stop opening their
	// first lines. The breaks are found before the edit, by the source
	// span of the paragraph the intent acts on, and dropped after it at
	// the offsets the patches moved them to.
	rebuilt := req.Intent == IntentEnter || req.Intent == IntentBackspaceAtStart || req.Intent == IntentDeleteAtEnd
	at := req.Start
	if req.Intent == IntentEnter && req.End > req.Start {
		at = req.End
	}
	var losing []int
	if rebuilt {
		if p := d.text.paraAt(at); p >= 0 && p < d.md.m.Len() {
			start, end, _ := d.md.m.Paragraph(p)
			for _, b := range d.state().liveBreaks {
				if b >= start && b < end {
					losing = append(losing, b)
				}
			}
		}
	}
	// An indent or outdent that spans a block boundary leaves the selection
	// running forward, from the earlier endpoint to the later one, even when
	// the range was selected upward: Typora rebuilds the selection from the
	// paragraphs it rewrote rather than from the runes the selection held
	// (record 2026-11-12, seed11/c10).
	forwardSelection := (req.Intent == IntentIndent || req.Intent == IntentOutdent) &&
		d.indentSelectionSweepsBackward()
	// An outdent that rewrites a whole line, as lifting an item's marker into
	// the blank line above it does, leaves the selection without bytes to map
	// it onto: Typora rebuilds it over the text it held instead of collapsing
	// to the point the rewrite leaves behind (record 2026-11-12, seed19/c6
	// keeps `1.a` selected at 16..19, anchor 16 upstream and caret 19
	// downstream). The range is rebuilt only while the text under it reads the
	// same afterwards; an outdent that strips the indent in front of the range
	// maps it as it stands, direction included.
	var kept outdentSelection
	if req.Intent == IntentOutdent && d.anchor != d.caret {
		kept = d.outdentSelectionLocked(d.anchor, d.caret)
	}
	var err error
	undoBefore := len(d.undo)
	d.edit(false, func(e *Edit) {
		e.SetIntent(req.Intent)
		err = e.markdownIntent(req)
		if err != nil {
			e.fail(err)
		}
	})
	// An intent that makes no edit has no step: the paragraph it acted on
	// was not rebuilt, and its breaks stay.
	if err == nil && len(losing) > 0 && len(d.undo) > undoBefore {
		d.dropLiveLines(losing)
	}
	restored := false
	if err == nil && kept.holds && len(d.undo) > undoBefore &&
		d.anchor == d.caret && d.outdentSelectionHolds(kept) {
		lo, hi := d.outdentSelectionRange(kept)
		d.assignSourceSelection(d.sourceSelectionForDocLocked(lo, hi), true)
		// Redo restores the range this step keeps, not the collapsed point
		// the rewritten markers mapped it to.
		d.undo[len(d.undo)-1].selAfter = d.selection
		restored = true
	}
	if err == nil && forwardSelection && !restored {
		d.forwardSourceSelection()
	}
	return err
}

// outdentSelection is the range an outdent's selection covered before the
// edit, by the paragraph each end sits in and how far into that paragraph it
// is: the offsets themselves move when the edit rewrites the line's prefix.
type outdentSelection struct {
	holds    bool
	pa, pz   int
	lo, hi   int
	paras    int
	text     string
	selected string
}

func (d *Document) outdentSelectionLocked(anchor, caret int) outdentSelection {
	lo, hi := min(anchor, caret), max(anchor, caret)
	sel := outdentSelection{
		holds: true,
		pa:    d.text.paraAt(lo),
		pz:    d.text.paraAt(hi),
		paras: d.text.ParagraphCount(),
	}
	sel.lo = lo - d.text.paraStart(sel.pa)
	sel.hi = hi - d.text.paraStart(sel.pz)
	sel.text = d.paragraphsText(sel.pa, sel.pz)
	sel.selected = d.text.Slice(lo, hi).String()
	return sel
}

// outdentSelectionRange returns the document range the selection holds after
// the edit: its offsets, read again from the paragraph each end belongs to.
func (d *Document) outdentSelectionRange(sel outdentSelection) (int, int) {
	return d.text.paraStart(sel.pa) + sel.lo, d.text.paraStart(sel.pz) + sel.hi
}

// outdentSelectionHolds reports whether the paragraphs the selection covered
// read as they did before the edit, so the text it held is still there to
// select.
func (d *Document) outdentSelectionHolds(sel outdentSelection) bool {
	if d.text.ParagraphCount() != sel.paras {
		return false
	}
	if d.paragraphsText(sel.pa, sel.pz) != sel.text {
		return false
	}
	lo, hi := d.outdentSelectionRange(sel)
	return d.text.Slice(lo, hi).String() == sel.selected
}

// paragraphsText returns the text of the paragraphs pa through pz.
func (d *Document) paragraphsText(pa, pz int) string {
	if pa < 0 || pz < pa || pz >= d.text.ParagraphCount() {
		return ""
	}
	start := d.text.paraStart(pa)
	end := min(d.text.paraStart(pz)+paragraphRunes(d.text.paragraph(pz)), d.text.Len())
	return d.text.Slice(start, end).String()
}

// indentSelectionSweepsBackward reports whether the selection an indent acts
// on was selected upward across a paragraph boundary, the range Typora
// reports forward once the indent rewrites the paragraphs it held.
func (d *Document) indentSelectionSweepsBackward() bool {
	sel := d.selection
	if d.md == nil || sel.Anchor.Offset == sel.Caret.Offset {
		return false
	}
	if sel.Anchor.Offset < sel.Caret.Offset {
		return false
	}
	anchor, caret := d.docSelectionForSourceLocked(sel)
	return d.text.paraAt(min(anchor, caret)) != d.text.paraAt(max(anchor, caret))
}

// forwardSourceSelection orders the selection by source offset: the anchor
// takes the earlier endpoint and the caret the later one, which is the
// direction Typora reports for a block the indent rewrote.
func (d *Document) forwardSourceSelection() {
	sel := d.selection
	if sel.Anchor.Offset <= sel.Caret.Offset {
		return
	}
	d.assignSourceSelection(SourceSelection{
		Anchor: sourcePoint(sel.Caret.Offset, BoundaryUpstream),
		Caret:  sourcePoint(sel.Anchor.Offset, BoundaryDownstream),
	}, true)
}

// dropLiveLines drops the given breaks from the session, at the offsets the
// step's patches moved them to. A break whose bytes a patch replaced is
// gone with them; a step that edited no source rebuilt no paragraph.
func (d *Document) dropLiveLines(breaks []int) {
	if d.md == nil || len(d.undo) == 0 {
		return
	}
	step := &d.undo[len(d.undo)-1]
	if step.start < 0 || len(step.patches) == 0 {
		return
	}
	for _, b := range breaks {
		p := b
		moved := true
		for _, c := range step.patches {
			var ok bool
			p, ok = parser.MapLiveOffset(p, c.at, c.at+len(c.old), len(c.new)-len(c.old))
			if !ok {
				moved = false
				break
			}
		}
		if moved {
			d.setState(d.state().clearLiveLineAt(p))
		}
	}
	// The step read the breaks after the edit and before this drop.
	step.sessionAfter = d.syncFromParser()
	// The paragraphs that lost their breaks show their text on the first
	// line they hold.
	d.reprojectStep(step)
}

func (e *Edit) markdownIntent(req EditRequest) error {
	if e.err != nil || e.d.md == nil {
		return e.err
	}
	if req.Start != req.End && req.Intent != IntentCrossBlockReplace &&
		req.Intent != IntentPasteMarkdown && req.Intent != IntentPastePlain {
		if req.Intent == IntentEnter || req.Intent == IntentShiftEnter {
			req.Start = req.End
		} else {
			return e.deleteCrossBlock(req.Start, req.End, "")
		}
	}
	switch req.Intent {
	case IntentEnter:
		return e.enterBlock(req.Start, req.Shift)
	case IntentShiftEnter:
		return e.shiftEnter(req.Start)
	case IntentBackspaceAtStart:
		return e.backspaceBlockStart(req.Start)
	case IntentDeleteAtEnd:
		return e.deleteBlockEnd(req.Start)
	case IntentIndent:
		return e.indentList(req.Start, 1)
	case IntentOutdent:
		return e.indentList(req.Start, -1)
	case IntentToggleTask:
		return e.toggleTask(req.Start)
	case IntentExitBlock:
		return e.exitBlock(req.Start, false)
	case IntentExitDocument:
		return e.exitBlock(req.Start, true)
	case IntentFormatInline:
		return e.formatInline(req.Start, req.End, func(a *Attributes) { a.Weight = req.Level })
	case IntentFormatBlock:
		return ErrStructuralEdit
	case IntentCrossBlockReplace:
		return e.deleteCrossBlock(req.Start, req.End, req.Text)
	case IntentPasteMarkdown:
		return e.pasteMarkdown(req.Start, req.End, req.Text)
	case IntentPastePlain:
		return e.pastePlain(req.Start, req.End, req.Text)
	}
	return nil
}

type markdownLine struct {
	start int
	end   int
	stop  int
	eol   string
}

func lineAt(src string, at int) markdownLine {
	at = max(0, min(at, len(src)))
	start := at
	for start > 0 && src[start-1] != '\n' && src[start-1] != '\r' {
		start--
	}
	i := at
	if i < start {
		i = start
	}
	for i < len(src) && src[i] != '\n' && src[i] != '\r' {
		i++
	}
	end := i
	if i < len(src) && src[i] == '\r' {
		i++
		if i < len(src) && src[i] == '\n' {
			i++
		}
	} else if i < len(src) && src[i] == '\n' {
		i++
	}
	return markdownLine{start: start, end: end, stop: i, eol: src[end:i]}
}

func defaultEOL(src string) string {
	if strings.Contains(src, "\r\n") {
		return "\r\n"
	}
	if strings.Contains(src, "\r") {
		return "\r"
	}
	return "\n"
}

// hardBreakMarker returns the marker the block of loc writes for a hard
// line break: the backslash or the two spaces its lines use, or "" when
// they use none.
func (d *Document) hardBreakMarker(loc paragraphLocation) string {
	if loc.leaf < 0 {
		return ""
	}
	src := d.md.p.String()
	b := d.md.p.Block(loc.leaf)
	block := src[b.Start:b.End]
	if strings.Contains(block, "\\\n") || strings.Contains(block, "\\\r\n") {
		return "\\"
	}
	for _, line := range strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n") {
		if strings.HasSuffix(line, "  ") {
			return "  "
		}
	}
	return ""
}

func (d *Document) sourceAtParagraph(p, local int) int {
	if d.md == nil || p < 0 || p >= d.md.m.Len() {
		return 0
	}
	pm := seqAt(d.md.m.paras, p)
	src := d.md.p.String()
	at := d.md.m.start(p)
	shown := 0
	local = max(0, min(local, paragraphRunes(d.text.paragraph(p))))
	if local == 0 && paragraphRunes(d.text.paragraph(p)) == 0 && !d.text.ParagraphStyleAt(d.text.paraStart(p)).CodeBlock {
		if at := emptyParagraphSource(src, d.md.m.start(p)); at >= 0 {
			return at
		}
		return d.paragraphLocation(p).contentStart
	}
	for _, seg := range pm.segs {
		if shown == local {
			return at
		}
		if seg.Shown > 0 && local < shown+seg.Shown {
			body := src[at : at+seg.Source]
			return at + byteOfRune(body, local-shown)
		}
		shown += seg.Shown
		at += seg.Source
	}
	return at
}

func (d *Document) splitOutside(p, point int) int {
	at, _ := d.splitOutsideCaret(p, point)
	return at
}

func (d *Document) splitOutsideCaret(p, point int) (at, caret int) {
	at, caret = point, point
	for moved := true; moved; {
		moved = false
		for _, b := range d.md.m.boxes(p) {
			if !b.literal {
				switch {
				case b.closeStart < b.closeEnd && at == b.closeStart:
					at = b.closeEnd
					caret = b.closeEnd
					moved = true
				case b.openStart < b.openEnd && at == b.openEnd:
					at = b.openStart
					moved = true
				}
			}
		}
	}
	return at, caret
}

// codeEmptyAt reports whether source position at is the virtual empty code
// line before a fenced block's closing marker. Input there belongs to the
// code block, and writeText adds the line ending the marker stands in for.
func (d *Document) codeEmptyAt(at int) bool {
	if d.md == nil {
		return false
	}
	m := d.md.m
	p := m.from(at)
	if p >= m.Len() || m.start(p) != at {
		return false
	}
	pm := seqAt(m.paras, p)
	return pm.codeEmpty && !pm.codeOpen
}

func (d *Document) gapInsert(at int, text string) string {
	src := d.md.p.String()
	if at == len(src) {
		if blank := previousMarkdownLine(src, at); d.md.eol && blank.start == blank.end && blank.eol != "" && blank.start > 0 {
			return text + blank.eol
		}
		return text
	}
	line := lineAt(src, at)
	if at != line.start || line.start != line.end || line.stop >= len(src) {
		return text
	}
	m := d.md.m
	k := m.from(at)
	if k < m.Len() && m.start(k) == at || k > 0 && m.end(k-1) >= at {
		return text
	}
	next := lineAt(src, line.stop)
	if strings.TrimSpace(src[next.start:next.end]) == "" {
		return text
	}
	return text + line.eol
}

func emptyParagraphSource(src string, at int) int {
	if at >= len(src) {
		line := previousMarkdownLine(src, at)
		body := src[line.start:line.end]
		if quoteOnly(body) {
			i := 0
			for i < len(body) && i < 3 && body[i] == ' ' {
				i++
			}
			i++
			if i < len(body) && body[i] == ' ' {
				i++
			}
			return line.start + i
		}
		if strings.TrimSpace(body) == "" {
			// After a blank line that separates it from content above,
			// input goes after the blank line. A document of blank lines
			// only writes on the blank line itself.
			if above := previousMarkdownLine(src, line.start); line.start > 0 && strings.TrimSpace(src[above.start:above.end]) != "" {
				return at
			}
			return line.start
		}
		return -1
	}
	line := lineAt(src, at)
	if at <= line.end && strings.TrimSpace(src[line.start:at]) != "" {
		return at
	}
	return -1
}

func (d *Document) sourceAtDoc(doc int) int {
	if d.md == nil {
		return 0
	}
	doc = max(0, min(doc, d.text.Len()))
	if doc == d.caret {
		return d.selection.Caret.Offset
	}
	if doc == d.anchor {
		return d.selection.Anchor.Offset
	}
	p := d.text.paraAt(doc)
	return d.sourceAtParagraph(p, doc-d.text.paraStart(p))
}

func (d *Document) docAtSource(source int) int {
	if d.md == nil {
		return max(0, min(source, d.text.Len()))
	}
	source = max(0, min(source, len(d.md.p.String())))
	startP := 0
	if source > 0 {
		startP = max(0, d.md.m.from(source)-1)
	}
	for p := startP; p < d.md.m.Len(); p++ {
		at := d.md.m.start(p)
		end := at + seqAt(d.md.m.paras, p).src
		if source < at {
			return d.text.paraStart(p)
		}
		if source > end {
			continue
		}
		want := source - at
		shown := 0
		for _, s := range seqAt(d.md.m.paras, p).segs {
			if want <= s.Source {
				if s.Shown <= 0 {
					return d.text.paraStart(p) + shown
				}
				body := d.md.p.String()[at : at+s.Source]
				if s.Kind == SegmentVerbatim {
					return d.text.paraStart(p) + shown + min(s.Shown, runesInBytes(body, want))
				}
				bodyRunes := utf8.RuneCountInString(body)
				delta := 0
				for delta < s.Shown && sourceAdvance(body, bodyRunes, delta, s.Shown) < want {
					delta++
				}
				return d.text.paraStart(p) + shown + delta
			}
			want -= s.Source
			at += s.Source
			shown += s.Shown
		}
		return d.text.paraStart(p) + shown
	}
	if d.text.ParagraphCount() == 0 {
		return 0
	}
	last := d.text.ParagraphCount() - 1
	if source < d.md.m.start(0) {
		return 0
	}
	return d.text.paraStart(last) + paragraphRunes(d.text.paragraph(last))
}

// setextHeadingSourceEnd extends a heading paragraph's source range over its
// underline. The projection maps the heading's content only, while Typora's
// block owns the line that makes it a heading: `alpha\n=====` sources 0:11,
// shown `alpha`.
func (d *Document) setextHeadingSourceEnd(p, end int) int {
	if d.md == nil || p < 0 || p >= d.md.m.Len() {
		return end
	}
	loc := d.paragraphLocation(p)
	b, ok := d.setextHeadingFor(loc)
	if !ok {
		return end
	}
	// The parser ends a setext heading at its content line; the underline
	// follows it, and Typora's block owns that line too.
	src := d.md.p.String()
	at := min(b.End, len(src))
	// An empty setext heading ends where its content line starts, though the
	// line may hold bytes the session typed on it: the underline follows that
	// line, not the block's own end.
	for at < len(src) && src[at] != '\n' && src[at] != '\r' {
		at++
	}
	switch {
	case at < len(src) && src[at] == '\r':
		at++
		if at < len(src) && src[at] == '\n' {
			at++
		}
	case at < len(src) && src[at] == '\n':
		at++
	}
	for at < len(src) && src[at] != '\n' && src[at] != '\r' {
		at++
	}
	if at > end {
		return at
	}
	return end
}

// setextHeadingFor finds the setext heading block a paragraph belongs to.
// The paragraph's leaf is the heading when the block covers the paragraph's
// content; an empty heading ends where its content line starts, so the
// paragraph holding the bytes typed on that line matches no block at all:
// the tab Tab typed on `\t\n=====` leaves the heading at 0:0 and its
// paragraph at 0:1 (record 2026-11-13, seed22/c9). Such a heading still owns
// the line, so the heading starting a line the paragraph starts on is its
// block.
func (d *Document) setextHeadingFor(loc paragraphLocation) (*parser.Block, bool) {
	p := d.md.p
	if loc.leaf >= 0 {
		b := p.Block(loc.leaf)
		if b.Kind == parser.Heading && b.Setext {
			return b, true
		}
		return nil, false
	}
	for i := 1; i < p.Block(0).Next; i++ {
		b := p.Block(i)
		if b.Kind == parser.Heading && b.Setext && b.Start == loc.line.start {
			return b, true
		}
	}
	return nil, false
}

func (d *Document) leafBlockAt(from, to int) int {
	if d.md == nil {
		return -1
	}
	best := -1
	bestSpan := int(^uint(0) >> 1)
	for i := 1; i < d.md.p.Block(0).Next; i++ {
		b := d.md.p.Block(i)
		switch b.Kind {
		case parser.Paragraph, parser.Heading, parser.CodeBlock, parser.FrontMatter, parser.ThematicBreak, parser.MathBlock, parser.HTMLBlock, parser.Table, parser.FootnoteDefinition:
		default:
			continue
		}
		if from < b.Start || from > b.End || to < b.Start || to > b.End {
			continue
		}
		span := b.End - b.Start
		if best < 0 || span <= bestSpan {
			best, bestSpan = i, span
		}
	}
	return best
}

func (d *Document) ancestors(i int) []int {
	if d.md == nil {
		return nil
	}
	var out []int
	for i >= 0 {
		out = append(out, i)
		i = d.md.p.Block(i).Parent
	}
	return out
}

func (d *Document) ancestorKind(i int, kind parser.Kind) int {
	for _, k := range d.ancestors(i) {
		if d.md.p.Block(k).Kind == kind {
			return k
		}
	}
	return -1
}

type paragraphLocation struct {
	para         int
	leaf         int
	item         int
	list         int
	line         markdownLine
	contentStart int
	contentEnd   int
	prefix       string
	suffix       string
}

func (d *Document) firstListItem(at int) bool {
	d.lock()
	defer d.mu.Unlock()
	loc := d.paragraphLocation(d.text.paraAt(at))
	return loc.item >= 0 && loc.item == loc.list+1
}

func (d *Document) paragraphLocation(p int) paragraphLocation {
	if d.md == nil || p < 0 || p >= d.md.m.Len() {
		return paragraphLocation{para: p, leaf: -1, item: -1, list: -1}
	}
	pm := seqAt(d.md.m.paras, p)
	start := d.md.m.start(p)
	end := start + pm.src
	if len(pm.segs) > 0 && pm.segs[0].Kind == SegmentBreak {
		// The paragraph's first line is blank: its map starts with that
		// line's break, while its block begins on the next line, with the
		// continuation prefix the break's bytes include.
		start += pm.segs[0].Source
	}
	leaf := d.leafBlockAt(start, end)
	if leaf < 0 && start > 0 {
		leaf = d.leafBlockAt(start-1, start-1)
	}
	line := lineAt(d.md.p.String(), start)
	contentStart := max(start, line.start)
	contentEnd := min(end, line.end)
	if contentStart > line.end {
		contentStart = line.end
	}
	if contentEnd < contentStart {
		contentEnd = contentStart
	}
	if contentStart == contentEnd {
		prev := previousMarkdownLine(d.md.p.String(), contentStart)
		if quoteOnly(prevBody(d.md.p.String(), prev)) {
			line = prev
			contentStart, contentEnd = prev.end, prev.end
		}
	}
	item, list := -1, -1
	if leaf >= 0 {
		item = d.ancestorKind(leaf, parser.Item)
		list = d.ancestorKind(leaf, parser.List)
	}
	if item < 0 {
		for i := 1; i < d.md.p.Block(0).Next; i++ {
			b := d.md.p.Block(i)
			if b.Kind == parser.Item && b.Start >= line.start && b.Start <= line.end {
				if item < 0 || b.Start > d.md.p.Block(item).Start {
					item = i
				}
			}
		}
		if item >= 0 {
			list = d.md.p.Block(item).Parent
		}
	}
	if m, ok := d.heldSplitMarkerAtParagraphLocked(p); ok {
		// The reparse nests the marker Enter moved into the split line as an
		// item of the sibling the line opened. The live view keeps that moved
		// marker as the sibling's own content instead: the empty paragraph
		// sits on the line's first item, before the moved marker (record
		// 2026-11-11, probe60/c6-enter-split-marker-typed).
		if live := d.splitMarkerLiveItemLocked(m); live >= 0 {
			line = lineAt(d.md.p.String(), m.lineStart)
			contentStart = max(line.start, min(m.outerEnd, line.end))
			contentEnd = max(contentStart, min(m.innerEnd, line.end))
			item, list = live, d.md.p.Block(live).Parent
			return paragraphLocation{
				para: p, leaf: leaf, item: item, list: list, line: line,
				contentStart: contentStart, contentEnd: contentEnd,
				prefix: d.md.p.String()[line.start:contentStart],
				suffix: d.md.p.String()[contentEnd:line.end],
			}
		}
	}
	return paragraphLocation{
		para: p, leaf: leaf, item: item, list: list, line: line,
		contentStart: contentStart, contentEnd: contentEnd,
		prefix: d.md.p.String()[line.start:contentStart],
		suffix: d.md.p.String()[contentEnd:line.end],
	}
}

// lineLocation returns loc with the line the source point falls on. A
// paragraph holds soft line breaks, so an edit at the caret works on the
// caret's line, not on the paragraph's first.
func (d *Document) lineLocation(loc paragraphLocation, src string, point int) paragraphLocation {
	if point <= loc.contentEnd {
		return loc
	}
	line := lineAt(src, point)
	loc.line = line
	loc.contentStart, loc.contentEnd = line.start, line.end
	loc.prefix, loc.suffix = "", ""
	return loc
}

func previousMarkdownLine(src string, at int) markdownLine {
	at = max(0, min(at, len(src)))
	i := at
	if i > 0 && src[i-1] == '\n' {
		i--
		if i > 0 && src[i-1] == '\r' {
			i--
		}
	} else if i > 0 && src[i-1] == '\r' {
		i--
	}
	end := i
	for i > 0 && src[i-1] != '\n' && src[i-1] != '\r' {
		i--
	}
	eol := ""
	if end < len(src) {
		stop := end
		if src[stop] == '\r' {
			stop++
			if stop < len(src) && src[stop] == '\n' {
				stop++
			}
		} else if src[stop] == '\n' {
			stop++
		}
		eol = src[end:stop]
	}
	return markdownLine{start: i, end: end, stop: end + len(eol), eol: eol}
}

func prevBody(src string, line markdownLine) string {
	return src[max(0, line.start):max(0, min(line.end, len(src)))]
}

// literalLineAt returns the literal line record covering lineStart.
func (d *Document) literalLineAt(lineStart int) (parser.LiteralLine, bool) {
	if d.md == nil {
		return parser.LiteralLine{}, false
	}
	for _, l := range d.state().literals {
		if l.Line == lineStart {
			return l, true
		}
	}
	return parser.LiteralLine{}, false
}

func (e *Edit) replaceMarkdownSource(at, oldEnd int, text string) bool {
	md := e.d.md
	if md == nil || at < 0 || oldEnd < at || oldEnd > len(md.p.String()) {
		return false
	}
	src := md.p.String()
	old := src[at:oldEnd]
	if old == text {
		return true
	}
	oldText := e.d.text
	nextSrc := src[:at] + text + src[oldEnd:]
	p := parser.New(md.p.Extensions())
	state := md.session.pullFromParser(md.p)
	p.SetLiteralLines(mapLiteralLines(state.literals, at, oldEnd, src, nextSrc))
	p.SetLiveLines(parser.ShiftLiveLines(state.liveBreaks, at, oldEnd, len(text)-(oldEnd-at)))
	state = state.pullFromParser(p)
	state.openedLists = mapOpenedLists(state.openedLists, at, oldEnd, len(text))
	md.session = state
	p.Set(nextSrc)
	t, m := projectSource(p)
	md.p = p
	if e.step != nil {
		if len(e.step.patches) == 0 {
			e.step.mapBefore = md.m
		}
		e.step.patches = append(e.step.patches, sourcePatch{at: at, old: old, new: text})
		e.step.mapAfter = m
	}
	md.m = m
	md.last, md.ok = [3]int{at, oldEnd, at + len(text)}, true
	e.d.mapSourceSelection(at, oldEnd, at+len(text))
	x, removed, inserted := diffText(oldText, t)
	e.d.apply(e.step, x, removed, inserted, t)
	return true
}

// reparseMarkdownSource refreshes the projection when a structural trigger
// changes how existing bytes parse without changing the bytes themselves.
func (e *Edit) reparseMarkdownSource() bool {
	md := e.d.md
	if md == nil {
		return false
	}
	src := md.p.String()
	p := parser.New(md.p.Extensions())
	dState := e.d.state()
	dState.syncToParser(p)
	p.Set(src)
	t, m := projectSource(p)
	oldText := e.d.text
	if e.step != nil {
		if len(e.step.patches) == 0 {
			e.step.mapBefore = md.m
		}
		e.step.mapAfter = m
	}
	md.p, md.m = p, m
	x, removed, inserted := diffText(oldText, t)
	e.d.apply(e.step, x, removed, inserted, t)
	return true
}

// splitDelimiters returns the inline delimiter runs a block split writes
// when it lands inside an element's content: the runs that close the
// elements it cuts, and the runs that open them again after the break. A
// split at a content boundary leaves the element whole in one half and
// writes nothing. Typora writes the halves raw, leaving an opening run on
// one block and its closing run on the next, which breaks the pair; spec
// §5.4 rules that both halves of a split inside a delimiter keep complete
// spans (research/07 §4.4).
func (e *Edit) splitDelimiters(p, point int) (before, after string) {
	if e.d.md == nil {
		return "", ""
	}
	src := e.d.md.p.String()
	var cut []box
	for _, b := range e.d.md.m.boxes(p) {
		switch b.kind {
		case MarkerStrong, MarkerEmphasis, MarkerStrikethrough, MarkerCode:
		default:
			continue
		}
		if b.literal || point <= b.contentFrom || point >= b.contentTo {
			continue
		}
		cut = append(cut, b)
	}
	if len(cut) == 0 {
		return "", ""
	}
	// Nesting closes innermost first and reopens outermost first.
	sort.Slice(cut, func(i, j int) bool { return cut[i].openStart < cut[j].openStart })
	for i := len(cut) - 1; i >= 0; i-- {
		before += src[cut[i].closeStart:cut[i].closeEnd]
	}
	for _, b := range cut {
		after += src[b.openStart:b.openEnd]
	}
	return before, after
}

// closingRunRuneSize is the byte size of the rune at source offset at when
// at lies strictly inside one of boxes' closing delimiter runs, and 0 when it
// lies anywhere else. Enter takes that rune with the break before splitting
// there, so the block above keeps a closing run one rune short and the pair
// stops balancing (record 2026-11-13, seed20/c2: `alpha **ug**` with the
// caret at 38 keeps `alpha **ug*`, while the run's first byte 37 and its end
// 39 keep every byte). A literal box is text rather than a delimiter, so it
// never takes a rune.
func closingRunRuneSize(src string, boxes []box, at int) int {
	if at < 0 || at >= len(src) {
		return 0
	}
	for _, b := range boxes {
		if b.literal || b.closeEnd <= b.closeStart || at <= b.closeStart || at >= b.closeEnd {
			continue
		}
		switch b.kind {
		case MarkerStrong, MarkerEmphasis, MarkerStrikethrough:
		default:
			// A link's destination or a code span's run holds no element
			// content Enter can cut: Typora keeps every byte of both
			// (record 2026-11-13, inline/link/destination-end/cmd-enter).
			continue
		}
		_, n := utf8.DecodeRuneInString(src[at:])
		if at+n > b.closeEnd {
			return 0
		}
		return n
	}
	return 0
}

func (e *Edit) replaceMarkdownLine(loc paragraphLocation, text string) error {
	at, oldEnd := loc.line.start, loc.line.stop
	if at == oldEnd {
		at, oldEnd = loc.contentStart, loc.contentEnd
	}
	if !e.replaceMarkdownSource(at, oldEnd, text) {
		return ErrStructuralEdit
	}
	return nil
}

func hasLineContent(s string) bool {
	return strings.TrimSpace(s) != ""
}

func (d *Document) linePrefixFor(p int) string {
	loc := d.paragraphLocation(p)
	return loc.prefix
}

func (d *Document) continuationPrefix(loc paragraphLocation) string {
	if loc.item >= 0 {
		it := d.md.p.Block(loc.item)
		src := d.md.p.String()
		firstLine := lineAt(src, it.Start)
		contentStart := max(it.Start, min(loc.contentStart, firstLine.end))
		for child := range d.md.p.Children(loc.item) {
			if start := d.md.p.Block(child).Start; start >= it.Start {
				contentStart = min(start, firstLine.end)
			}
			break
		}
		marker := src[it.Start:contentStart]
		if i := strings.IndexByte(marker, '['); i >= 0 {
			marker = marker[:i]
		}
		base := it.Start - firstLine.start
		if list := it.Parent; list > 0 {
			if parent := d.md.p.Block(list).Parent; parent > 0 && d.md.p.Block(parent).Kind == parser.Item &&
				d.md.p.Block(parent).Task != 0 {
				// A nested item's line can start at its parent task's
				// content column, which the check box widens past the
				// parent's continuation prefix. Typora measures the
				// continuation from that prefix: `- [ ] - ` continues with
				// four spaces (record 2026-11-11, newops/c47-4).
				base = d.itemContinuationWidth(parent)
			}
		}
		n := base + len(marker)
		if strings.Contains(loc.prefix, ">") {
			quote := loc.prefix
			if i := strings.LastIndex(quote, ">"); i >= 0 {
				quote = quote[:i+1]
				if i+1 < len(quote) && quote[i+1] == ' ' {
					quote = quote[:i+2]
				}
				return quote + strings.Repeat(" ", max(0, n-len(quote)))
			}
		}
		return strings.Repeat(" ", n)
	}
	if strings.Contains(loc.prefix, ">") {
		i := strings.LastIndex(loc.prefix, ">")
		if i >= 0 {
			prefix := loc.prefix[:i+1]
			if i+1 < len(loc.prefix) && loc.prefix[i+1] == ' ' {
				prefix += " "
			}
			return prefix
		}
	}
	return ""
}

// exitBlankPrefix is the prefix the blank line between an item and the
// paragraph the exit opens must carry to stay in the containers around the
// item: the document root needs none, a nested item's blank line continues
// its parent item, and a quoted one keeps the quote markers without the
// item's content indent (record 2026-11-10, exitprobe/nested-cmdenter-eof
// and exitprobe/quote-cmdenter-eof).
func (d *Document) exitBlankPrefix(loc paragraphLocation) string {
	if loc.list >= 0 && loc.item >= 0 {
		if parent := d.md.p.Block(loc.list).Parent; parent > 0 {
			if b := d.md.p.Block(parent); b.Kind == parser.Item {
				src := d.md.p.String()
				line := lineAt(src, b.Start)
				p := d.continuationPrefix(paragraphLocation{
					item:         parent,
					contentStart: b.Start,
					prefix:       src[line.start:b.Start],
				})
				if strings.Contains(p, ">") {
					p = strings.TrimRight(p, " ")
				}
				return p
			}
		}
	}
	if p := d.continuationPrefix(loc); strings.Contains(p, ">") {
		return strings.TrimRight(p, " ")
	}
	return ""
}

func (e *Edit) enterBlock(at int, shift bool) error {
	if shift {
		return e.shiftEnter(at)
	}
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	st := t.paragraph(p).Style
	point := e.d.sourceAtDoc(at)
	if p >= 0 && at == t.paraStart(p) {
		// The caret at the paragraph's start is drawn there even when the
		// paragraph's first line follows a blank line the projection holds
		// as a gap: Enter splits the paragraph's line, not the gap, so the
		// rune typed at the caret stays in the paragraph it is drawn in.
		point = e.d.sourceAtParagraph(p, 0)
	}
	caretPoint := point
	src := e.d.md.p.String()
	if st.CodeBlock {
		line := lineAt(src, point)
		if loc.leaf >= 0 {
			b := e.d.md.p.Block(loc.leaf)
			if b.Kind == parser.CodeBlock && b.Open && point >= b.Start && point <= line.end {
				if paragraphRunes(t.paragraph(p)) == 0 {
					line = lineAt(src, b.Start)
					point = line.end
				}
				fence := strings.Repeat(stringOrDefault(b.FenceChar, '`'), max(3, b.FenceLength))
				eol := line.eol
				if eol == "" {
					eol = defaultEOL(src)
				}
				if line.eol == "" {
					if !e.replaceMarkdownSource(line.end, line.end, eol+eol+fence) {
						return ErrStructuralEdit
					}
					if e.step != nil {
						caret := point + len(eol)
						e.Select(e.d.docAtSource(caret), e.d.docAtSource(caret))
					}
					return nil
				}
				if !e.replaceMarkdownSource(line.stop, line.stop, eol+fence+eol) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					caret := line.end + len(line.eol)
					e.Select(e.d.docAtSource(caret), e.d.docAtSource(caret))
				}
				return nil
			}
		}
		indent := codeIndentAt(src, line.start, line.end)
		lineText := src[line.start:line.end]
		cut := max(0, min(point-line.start, len(lineText)))
		br := line.eol
		if br == "" {
			br = defaultEOL(src)
		}
		right := strings.TrimLeft(lineText[cut:], " \t")
		newLine := lineText[:cut] + br + indent + right + line.eol
		if !e.replaceMarkdownSource(line.start, line.stop, newLine) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			e.selectMarkdownSource(point + len(br) + len(indent))
		}
		return nil
	}
	if from, to, k, ok := e.d.lineBreakBeside(p, at); ok {
		// Enter beside a line break inside a paragraph turns that break
		// into a block boundary, as Typora does: `> a\n> b` becomes
		// `> a\n>\n> b` and `- a\n  b` becomes `- a\n- b`, with the caret
		// before b. Join the lines, then split where they meet.
		if loc.item >= 0 {
			item := e.d.md.p.Block(loc.item)
			e.splitMultiline = item.End > liveLineAt(src, item.Start).end
		}
		if from == to {
			// A break the session typed over a whitespace-only line has no
			// bytes of the paragraph's own: its line ending belongs to the
			// blank line above, so joining has nothing to remove and the
			// recursion below would meet the same break again. The split
			// turns that ending into the paragraph separator instead, which
			// reads back as the blank line the held line showed and takes
			// the session's break with the replaced byte (issue 98).
			prev := previousMarkdownLine(src, from)
			if prev.eol == "" || prev.stop != from {
				return ErrStructuralEdit
			}
			if !e.replaceMarkdownSource(prev.end, prev.stop, prev.eol+prev.eol) {
				return ErrStructuralEdit
			}
			if e.step != nil && e.d.md.p.Block(loc.leaf).Kind == parser.Table {
				// A table's last line has no line ending of its own in the
				// projection: the row's closing cell ends the line, and the
				// caret stands after the space the row drops. The source point
				// the edit mapped is the row's, which is where the next rune
				// goes.
			} else if e.step != nil {
				e.selectMarkdownSource(prev.stop + len(prev.eol))
			}
			return nil
		}
		if !e.replaceMarkdownSource(from, to, "") {
			return ErrStructuralEdit
		}
		e.Select(k, k)
		return e.enterBlock(k, false)
	}
	point, caretPoint = e.d.splitOutsideCaret(p, point)
	if loc.leaf >= 0 && loc.leaf < len(src) {
		b := e.d.md.p.Block(loc.leaf)
		if b.Kind == parser.Heading {
			return e.enterHeading(loc, point, caretPoint)
		}
	}
	if st.List != NoList {
		return e.enterList(loc, point, caretPoint)
	}
	if st.Quote || strings.Contains(loc.prefix, ">") {
		return e.enterQuote(loc, point, caretPoint)
	}
	loc = e.d.lineLocation(loc, src, point)
	content := src[loc.contentStart:loc.contentEnd]
	cut := max(0, min(point-loc.contentStart, len(content)))
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	// The line's prefix is the indent that keeps a paragraph inside its
	// item: both halves of the split keep it, as a heading's continuation
	// lines do.
	prefix := loc.prefix
	before, after := e.splitDelimiters(loc.para, point)
	// A split inside a closing delimiter run drops the rune the caret sits
	// before, so the block above keeps a run one rune short (see
	// closingRunRuneSize); the text after it opens the block below.
	taken := closingRunRuneSize(src, e.d.md.m.boxes(loc.para), point)
	rest := max(cut, min(cut+taken, len(content)))
	newText := prefix + content[:cut] + before + eol + eol + prefix + after + content[rest:]
	if content[cut:] != "" {
		newText += loc.line.eol
	} else {
		newText += eol
	}
	countBefore := e.d.text.ParagraphCount()
	if err := e.replaceMarkdownLine(loc, newText); err != nil {
		return err
	}
	if e.step != nil {
		caretSource := caretPoint + len(before) + len(eol) + len(eol) + len(prefix) + len(after)
		if content == "" && e.d.text.ParagraphCount() == countBefore {
			// The split wrote the line and the projection shows no paragraph
			// for it: the empty paragraph stands in a blank run that the gap
			// between two loose list items holds, and the run projects as the
			// one empty paragraph it shows however long it is. The offset the
			// split computes runs past that paragraph into the blank lines the
			// next item's lead holds, where the caret is drawn on the item
			// while the rune typed at it stays in the empty paragraph. The
			// caret stays on the paragraph the split worked on (record
			// 2026-11-13, TestFeelFuzz seed 1: a plain list `- a` and
			// [enter, ",", up, enter, shift+tab, enter]).
			caretSource = caretPoint
		}
		e.selectMarkdownSource(caretSource)
	}
	return nil
}

// lineBreakBeside returns the source bytes of the in-paragraph line break
// just after or just before doc offset at in paragraph p, with the doc
// offset where the lines meet once it is removed.
func (d *Document) lineBreakBeside(p, at int) (from, to, join int, ok bool) {
	ps := d.text.paraStart(p)
	local := at - ps
	pm := seqAt(d.md.m.paras, p)
	base := d.md.m.start(p)
	shown, cursor := 0, 0
	for _, s := range pm.segs {
		if s.Kind == SegmentBreak && (shown == local || shown+s.Shown == local) {
			return base + cursor, base + cursor + s.Source, ps + shown, true
		}
		shown += s.Shown
		cursor += s.Source
	}
	return 0, 0, 0, false
}

func codeIndentAt(src string, start, end int) string {
	line := src[start:end]
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i == len(line) {
		// A line of whitespace alone has no token to indent from: Typora's
		// code editor leaves the line Enter opens empty (record
		// 2026-11-10, seed9/c1).
		return ""
	}
	return line[:i]
}

// indentCodeLines adds the code editor's two spaces to the start of each
// line from through to. Typora's code editor indents the lines a selection
// covers rather than replacing the bytes it holds, and leaves the rewritten
// lines selected (record 2026-11-12, seed15/c8).
func (d *Document) indentCodeLines(from, to int) {
	if d.md == nil {
		return
	}
	src := d.md.p.String()
	last := lineAt(src, to)
	var starts []int
	for l := lineAt(src, from); ; l = lineAt(src, l.stop) {
		starts = append(starts, l.start)
		if l.start >= last.start || l.stop >= len(src) {
			break
		}
	}
	indented := last.end
	d.edit(false, func(e *Edit) {
		for i := len(starts) - 1; i >= 0; i-- {
			if !e.replaceMarkdownSource(starts[i], starts[i], "  ") {
				e.fail(ErrStructuralEdit)
				return
			}
		}
	})
	indented += 2 * len(starts)
	d.assignSourceSelection(SourceSelection{
		Anchor: sourcePoint(starts[0], BoundaryDownstream),
		Caret:  sourcePoint(indented, BoundaryDownstream),
	}, true)
}

func stringOrDefault(c byte, fallback byte) string {
	if c == 0 {
		c = fallback
	}
	return string(c)
}

// enterHeadingItemEnd writes the line Shift+Enter opens below a heading that
// sits in a list item. The heading keeps the item's marker line, so the line
// it leaves is the item's continuation paragraph: the marker column is its
// prefix. Typora serializes that line without the marker, which reads back as
// a pure-whitespace line and reopens as the previous paragraph's tail, so
// ruling R2-C writes the empty item explicitly under the marker column
// instead (spec §5.5, seed16/c5).
func (e *Edit) enterHeadingItemEnd(loc paragraphLocation) bool {
	_, ok := e.enterHeadingItemContinuation(loc)
	return ok
}

// enterHeadingItemContinuation writes that line and returns the source offset
// the caret goes to: the start of the empty item it opened.
func (e *Edit) enterHeadingItemContinuation(loc paragraphLocation) (int, bool) {
	src := e.d.md.p.String()
	item := e.d.md.p.Block(loc.item)
	first := lineAt(src, item.Start)
	content := first.end
	for child := range e.d.md.p.Children(loc.item) {
		if start := e.d.md.p.Block(child).Start; start >= item.Start {
			content = min(start, first.end)
		}
		break
	}
	marker := src[item.Start:content]
	sibling, ok := e.d.siblingPrefix(loc, marker)
	if !ok || marker == "" {
		return 0, false
	}
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	newText := eol + sibling
	if !e.replaceMarkdownSource(loc.line.stop, loc.line.stop, newText) {
		return 0, false
	}
	caret := loc.line.stop + len(newText)
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return caret, true
}

func (e *Edit) enterHeading(loc paragraphLocation, point, caretPoint int) error {
	src := e.d.md.p.String()
	b := e.d.md.p.Block(loc.leaf)
	content := src[loc.contentStart:loc.contentEnd]
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	cut := max(0, min(point-loc.contentStart, len(content)))
	if b.Setext {
		underline := ""
		next := markdownLine{}
		if loc.line.stop <= len(src) {
			next = lineAt(src, loc.line.stop)
			if next.start == loc.line.stop {
				underline = src[next.start:next.stop]
			}
		}
		if strings.TrimSpace(strings.TrimRight(underline, "\r\n")) != "" {
			if cut == 0 && content != "" {
				newText := eol + eol
				if !e.replaceMarkdownSource(loc.line.start, loc.line.start, newText) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(loc.contentStart + len(newText))
				}
				return nil
			}
			if cut == len(content) {
				if !e.replaceMarkdownSource(next.stop, next.stop, eol) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(next.stop + len(eol))
				}
				return nil
			}
			mark := strings.TrimSpace(strings.TrimRight(underline, "\r\n"))
			prefix := strings.TrimSpace(mark)
			left := content[:cut]
			right := content[len(left):]
			if strings.Trim(right, " \t") == "" {
				newText := loc.prefix + content + eol + eol
				if err := e.replaceMarkdownLine(loc, newText); err != nil {
					return err
				}
				if e.step != nil {
					e.selectMarkdownSource(loc.line.start + len(loc.prefix+content+eol+eol))
				}
				return nil
			}
			newText := loc.prefix + left + eol + prefix + eol + eol + loc.prefix + right + eol + prefix
			if next.eol != "" {
				newText += next.eol
			}
			at0 := loc.line.start
			oldEnd := next.stop
			if !e.replaceMarkdownSource(at0, oldEnd, newText) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				caret := at0 + len(left+eol+prefix+eol+eol)
				e.selectMarkdownSource(caret)
			}
			return nil
		}
	}
	if cut == 0 && content != "" {
		newText := eol + eol
		if !e.replaceMarkdownSource(loc.line.start, loc.line.start, newText) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			e.selectMarkdownSource(loc.contentStart + len(newText))
		}
		return nil
	}
	if cut == len(content) {
		line := src[loc.line.start:loc.line.end]
		if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, line+eol+eol+eol) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			at := loc.line.start + len(line+eol+eol)
			if strings.TrimSpace(line) == "" {
				// The blank line is itself the empty paragraph Enter splits:
				// it stays, and the paragraph Enter opens follows the
				// separator, one line further down.
				at += len(eol)
			}
			e.selectMarkdownSource(at)
		}
		return nil
	}
	prefix := loc.prefix
	blank := ""
	if strings.Contains(prefix, ">") {
		blank = strings.TrimRight(prefix, " \t")
	}
	left, right := content[:cut], content[cut:]
	before, after := e.splitDelimiters(loc.para, point)
	newText := prefix + left + before + eol + blank + eol + prefix + after + right + eol
	if err := e.replaceMarkdownLine(loc, newText); err != nil {
		return err
	}
	if e.step != nil {
		e.selectMarkdownSource(caretPoint + len(before) + len(eol) + len(blank) + len(eol) + len(prefix) + len(after))
	}
	return nil
}

func (e *Edit) shiftEnter(at int) error {
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	src := e.d.md.p.String()
	point, caretPoint := e.d.splitOutsideCaret(p, e.d.sourceAtDoc(at))
	if loc.leaf >= 0 && e.d.md.p.Block(loc.leaf).Kind == parser.Heading {
		if loc.item >= 0 && point == loc.contentEnd && e.enterHeadingItemEnd(loc) {
			return nil
		}
		return e.enterHeading(loc, point, caretPoint)
	}
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	hard := e.d.hardBreakMarker(loc)
	cont := e.d.continuationPrefix(loc)
	line := lineAt(src, point)
	// A code block holds empty lines, so only a paragraph needs its empty
	// line spelled with a marker. A paragraph past the last block has no
	// leaf of its own.
	paragraph := loc.leaf < 0 || e.d.md.p.Block(loc.leaf).Kind == parser.Paragraph
	if point == loc.contentEnd && line.end <= point {
		from := line.end
		lineText := src[line.start:line.end]
		if hard == "\\" && strings.HasSuffix(lineText, "\\") {
			from--
		} else if hard == "  " {
			from -= len(lineText) - len(strings.TrimRight(lineText, " "))
		}
		if hard != "" {
			if !e.replaceMarkdownSource(from, from, hard) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				caret := e.d.docAtSource(from + len(hard))
				e.Select(caret, caret)
			}
			return nil
		}
		if paragraph && cont == "" && line.start > 0 && strings.TrimSpace(lineText) == "" {
			// The break would leave an empty line, which a later parse
			// reads as the blank line separating paragraphs. Inside a list
			// or quote the line's own indent keeps it in the container, as
			// Typora writes it; a plain paragraph needs the bytes to spell
			// a hard break so the line survives the parse.
			if lineText != "" && (t.paragraph(p).Style.List != NoList || t.paragraph(p).Style.Quote) {
				cont = lineText
			} else {
				hard = "\\"
			}
		}
		if !e.replaceMarkdownSource(from, from, hard+eol+cont) {
			return ErrStructuralEdit
		}
		// The break these bytes hold is one the session typed, while the
		// paragraph it opens starts on it.
		e.setLiveLine(from + len(hard))
		if e.step != nil {
			e.selectMarkdownSource(from + len(hard) + len(eol) + len(cont))
		}
		return nil
	}
	lineText := src[line.start:line.end]
	cut := max(0, min(point-line.start, len(lineText)))
	marker := hard
	if marker == "" && paragraph && line.start > 0 && strings.TrimSpace(lineText[:cut]) == "" {
		// As above: an empty line left inside the paragraph would read as
		// a paragraph separator. Write the break's marker so the paragraph
		// keeps the empty line.
		marker = "\\"
	}
	// A soft break stays inside the element: an emphasis or code span
	// parses across a line break, so the pair holds and no delimiter run
	// is written (record 2026-11-04, inline/bold/content-start/shift-enter;
	// caret_input_test.go TestCaretInputShiftEnterInCodeSpan).
	newText := lineText[:cut] + marker + eol + cont + lineText[cut:]
	if lineText[cut:] != "" || cont != "" {
		// The replacement covers the line's ending. The text after the
		// break, or the continuation prefix the break left, is a line of
		// its own and keeps that ending.
		newText += line.eol
	}
	literal, literalOK := e.d.literalLineAt(line.start)
	if !e.replaceMarkdownSource(line.start, line.stop, newText) {
		return ErrStructuralEdit
	}
	if literalOK && literal.Content <= point {
		// The split rewrote the line from its start, which drops the record
		// that its markers are text the session typed. The break lands after
		// them, so they are still on the line, at the offset they were: the
		// line above the break keeps reading them as text instead of opening
		// the block they spell (record 2026-11-13, issue177: `>[]()` typed on
		// a paragraph line, then Shift+Enter, stays one literal paragraph
		// and the next rune goes to the line the caret is drawn on).
		e.d.setState(e.d.state().setLiteralLine(line.start, literal.Content))
	}
	// The break these bytes hold is one the session typed, while the
	// paragraph it opens starts on it.
	e.setLiveLine(line.start + cut + len(marker))
	if e.step != nil {
		e.selectMarkdownSource(caretPoint + len(marker) + len(eol) + len(cont))
	}
	return nil
}

// setLiveLine keeps a break the session typed and refreshes the projection,
// which shows the paragraph's empty first line only while the break is the
// session's.
func (e *Edit) setLiveLine(at int) {
	if e.d.md == nil {
		return
	}
	e.d.setState(e.d.state().setLiveLine(at))
	e.reparseMarkdownSource()
}

func (e *Edit) backspaceBlockStart(at int) error {
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	st := t.paragraph(p).Style
	src := e.d.md.p.String()
	if st.List != NoList {
		if loc.item >= 0 && strings.TrimSpace(src[loc.contentStart:loc.line.end]) == "" {
			item := e.d.md.p.Block(loc.item)
			if loc.line.start != lineAt(src, item.Start).start {
				// The empty line continues the item's content instead of
				// holding its marker: Shift+Enter's break, or a blank line
				// after the content. It is no empty item to join backward;
				// Backspace takes the line away and leaves the line above.
				if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, "") {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(previousMarkdownLine(src, loc.line.start).end)
				}
				return nil
			}
			prev, next := e.listSiblings(loc.item)
			if prev >= 0 {
				if st.List == Bullets && st.Indent == 0 && !st.Quote && !st.CodeBlock &&
					next < 0 && !e.d.md.p.Block(loc.list).Ordered &&
					prev == loc.list+1 && loc.item == prev+1 && loc.line.stop == len(src) &&
					loc.line.eol == "" && e.d.md.eol {
					prevItem := e.d.md.p.Block(prev)
					prevLine := previousMarkdownLine(src, loc.line.start)
					if lineAt(src, prevItem.Start).start != 0 ||
						src[prevItem.Start:prevItem.End] != src[loc.line.start:loc.line.end] ||
						prevLine.stop != loc.line.start || prevLine.start != 0 ||
						src[prevLine.start:prevLine.end] != src[loc.line.start:loc.line.end] {
						return e.joinEmptyListItemBackward(loc, next)
					}
					// Enter on the document's only empty bullet opens a second
					// explicit item with no final EOL. Typora's Backspace takes
					// that whole item back and keeps the file's ending on the
					// item it came from; joining it as a continuation would
					// show a second, indented empty block (VM records
					// 2026-11-14-033056 / -033730, seed23/c11).
					if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, "") {
						return ErrStructuralEdit
					}
					if e.step != nil {
						e.selectMarkdownSource(prevItem.End)
					}
					return nil
				}
				return e.joinEmptyListItemBackward(loc, next)
			}
			if next < 0 && e.emptyNestedItemHasOwnLine(loc) {
				return e.liftEmptyNestedItemIntoParent(loc)
			}
			from := loc.line.start
			if item := e.d.md.p.Block(loc.item); item.Start > from {
				// An empty nested item can share the line with its parent:
				// only the item's own marker leaves, and the parent's line
				// stays (record 2026-11-10, seed3/c05).
				from = item.Start
			}
			if !e.replaceMarkdownSource(from, loc.line.stop, "") {
				return ErrStructuralEdit
			}
			if e.step != nil {
				// The marker line's bytes leave and the line below keeps its
				// own prefix, which reads as text once the item is gone: the
				// caret stands at the start of what is left, as the other
				// joins put it at the join (record 2026-11-12, feel/128).
				e.selectMarkdownSource(from)
			}
			return nil
		}
		if loc.item < 0 {
			if items, ok := e.trailingListContinuation(loc); ok {
				return e.backspaceTrailingListContinuation(items)
			}
			return ErrStructuralEdit
		}
		item := e.d.md.p.Block(loc.item)
		list := e.d.md.p.Block(item.Parent)
		prev, next := e.listSiblings(loc.item)
		emptyAbove := prev >= 0
		if emptyAbove {
			for range e.d.md.p.Children(prev) {
				emptyAbove = false
				break
			}
		}
		if emptyAbove && lineAt(src, item.Start).start == loc.line.start &&
			lineAt(src, e.d.md.p.Block(prev).Start).stop == loc.line.start {
			// The item above holds only its marker, so the join takes the
			// break between the two marker lines away: this item's content
			// moves onto the empty item's line and the list keeps the number
			// that line already spells. Handing the content to the parent
			// item instead would put it outside the list (record 2026-11-13,
			// seed21/c12).
			from := e.d.md.p.Block(prev).End
			if !e.replaceMarkdownSource(from, loc.contentStart, "") {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(from)
			}
			return nil
		}
		if list.Kind == parser.List && e.d.md.p.Block(list.Parent).Kind == parser.Item {
			// Only the item's first paragraph starts on the marker line and
			// can absorb the marker into the parent item; a later paragraph
			// joins its previous sibling instead.
			if loc.line.start > item.Start {
				return e.joinItemParagraphBackward(loc)
			}
			return e.mergeIntoParentItem(at, loc)
		}
		if prev >= 0 {
			return e.joinListItemBackward(at, loc, next >= 0)
		}
		if lineIndent(src, loc.line.start) > 0 {
			return e.indentList(at, -1)
		}
		return e.liftListItem(at, loc)
	}
	if st.Heading > 0 {
		if loc.leaf >= 0 {
			b := e.d.md.p.Block(loc.leaf)
			if b.Setext {
				next := lineAt(src, loc.line.stop)
				newText := src[loc.line.start:loc.line.end] + loc.line.eol
				if !e.replaceMarkdownSource(loc.line.start, next.stop, newText) {
					return ErrStructuralEdit
				}
				return nil
			}
		}
		from := loc.line.start
		if loc.contentStart >= len(src) {
			return ErrStructuralEdit
		}
		if !e.replaceMarkdownSource(from, loc.contentStart, "") {
			return ErrStructuralEdit
		}
		return nil
	}
	if st.CodeBlock && loc.leaf >= 0 {
		b := e.d.md.p.Block(loc.leaf)
		if b.Kind == parser.CodeBlock && b.Fenced {
			open := lineAt(src, b.Start)
			close := lineAt(src, b.End)
			point := e.d.sourceAtParagraph(p, at-t.paraStart(p))
			if point == open.stop && close.start >= open.stop {
				body := src[open.stop:close.start]
				if !e.replaceMarkdownSource(b.Start, close.stop, body) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(b.Start)
				}
				return nil
			}
			if point > open.stop && point <= close.start && src[point-1] == '\n' {
				// Backspace at the start of a body line joins it with the
				// line above: the line ending between them leaves and the
				// caret stands at the join. An empty line leaves the same
				// way (record 2026-11-10, seed7/c19).
				if !e.replaceMarkdownSource(point-1, point, "") {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.selectMarkdownSource(point - 1)
				}
				return nil
			}
		}
	}
	if st.Quote && loc.leaf >= 0 {
		q := e.d.ancestorKind(loc.leaf, parser.Quote)
		if q >= 0 {
			b := e.d.md.p.Block(q)
			// The rewrite strips a prefix from every line of the quote, so
			// the caret's offset moves with the bytes before it: an offset
			// inside a line's prefix lands at the content start the prefix
			// precedes. Mapping the doc caret keeps the paragraph the caret
			// was at the start of, though the reparse renumbers paragraphs.
			point := e.d.sourceAtParagraph(p, at-t.paraStart(p))
			rewritten, newPoint := stripQuotePrefixesAt(src[b.Start:b.End], point-b.Start)
			if !e.replaceMarkdownSource(b.Start, b.End, rewritten) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(b.Start + max(0, newPoint))
			}
			return nil
		}
	}
	if st.Quote {
		// The caret's line may spell no paragraph of its own: `>`, `> ` or
		// `> > ` read as an empty quote line, which the parser holds as a
		// container without a leaf. Backspace there takes that line's own
		// quote marker away, one level per press, as Typora does (record
		// 2026-11-13, zz177/bs-*).
		return e.backspaceEmptyQuoteLine(p, at)
	}
	return ErrStructuralEdit
}

func (e *Edit) deleteBlockEnd(at int) error {
	t := e.d.text
	p := t.paraAt(at)
	loc := e.d.paragraphLocation(p)
	if t.paragraph(p).Style.CodeBlock {
		return nil
	}
	// Delete acts on the caret's line. A paragraph holds soft line breaks,
	// so a caret at its end stands on its last line, while the paragraph's
	// location names its first: the line ending and the prefix between them
	// stay (record 2026-11-12, seed14/c7).
	loc = e.d.lineLocation(loc, e.d.md.p.String(), e.d.sourceAtParagraph(p, at-t.paraStart(p)))
	if m, ok := e.d.heldSplitMarkerAtParagraphLocked(p); ok && m.outerEnd < m.innerEnd {
		// The split sibling's content is the marker Enter moved into its
		// line. Typora's Delete removes its first rune, leaving the item's
		// line without that byte (record 2026-11-11, probe70/delete).
		src := e.d.md.p.String()
		_, size := utf8.DecodeRuneInString(src[m.outerEnd:m.innerEnd])
		if size <= 0 || !e.replaceMarkdownSource(m.outerEnd, m.outerEnd+size, "") {
			return ErrStructuralEdit
		}
		if e.step != nil {
			e.selectMarkdownSource(m.outerEnd)
		}
		return nil
	}
	if loc.leaf < 0 {
		return ErrStructuralEdit
	}
	next := p + 1
	if next >= t.ParagraphCount() {
		if loc.leaf >= 0 {
			if b := e.d.md.p.Block(loc.leaf); b.Kind == parser.Heading && b.Setext {
				// A setext heading owns its underline line: the caret at the
				// end of the text sits before the heading's own last line,
				// not at the end of the last block. Typora keeps the line
				// break and the underline, so Delete does nothing (record
				// 2026-11-10, seed9/c8).
				return nil
			}
		}
		// Delete at the end of the last block takes the file's final line
		// ending with it: Typora saved the document without one
		// (record 2026-11-09, seed4/c20).
		if loc.line.eol != "" {
			if !e.replaceMarkdownSource(loc.line.end, loc.line.stop, "") {
				return ErrStructuralEdit
			}
			// The caret keeps the source point it had: the line's content end.
			// Re-deriving it from the view's end would put it in front of the
			// closing delimiters a revealed element ends the line with
			// (record 2026-11-12, seed15/c9).
			if e.step != nil && loc.leaf >= 0 && e.d.ancestorKind(loc.leaf, parser.Quote) >= 0 {
				// The source point is set here rather than mapped through the
				// edit: a quote line's ending can be a `>` line the projection
				// read as a marker, and the rune that line holds then takes the
				// break's place in the paragraph. The view draws the caret one
				// rune on while the mapped source point stays put, so Undo,
				// which re-derives the caret from the source, puts it back a
				// rune (record 2026-11-13, TestFeelFuzz seed 1: `> a\n> b\n`
				// and [enter, backspace, delete]).
				e.selectMarkdownSource(loc.line.end)
			}
		}
		return nil
	}
	to := e.d.sourceAtParagraph(next, 0)
	if loc.contentEnd > to {
		return ErrStructuralEdit
	}
	nextLoc := e.d.paragraphLocation(next)
	if loc.leaf >= 0 && nextLoc.leaf >= 0 {
		cur := e.d.md.p.Block(loc.leaf)
		n := e.d.md.p.Block(nextLoc.leaf)
		if cur.Kind == parser.Heading && cur.Setext && n.Kind == parser.Paragraph {
			src := e.d.md.p.String()
			nextText := src[nextLoc.contentStart:nextLoc.contentEnd]
			underline := lineAt(src, loc.line.stop)
			text := src[loc.line.start:loc.contentEnd] + nextText + loc.line.eol + src[underline.start:underline.stop]
			if !e.replaceMarkdownSource(loc.line.start, nextLoc.line.stop, text) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(loc.contentEnd)
			}
			return nil
		}
	}
	if leaf := e.d.paragraphLocation(next).leaf; leaf >= 0 {
		b := e.d.md.p.Block(leaf)
		if b.Kind == parser.CodeBlock && b.Fenced {
			src := e.d.md.p.String()
			open := lineAt(src, b.Start)
			close := lineAt(src, b.End)
			fence := strings.Repeat(stringOrDefault(b.FenceChar, '`'), max(3, b.FenceLength))
			closeText := stripLinePrefix(src[close.start:close.end], nextLoc.prefix)
			if strings.HasPrefix(strings.TrimSpace(closeText), fence) {
				body := stripLinePrefix(src[open.stop:close.start], nextLoc.prefix)
				if !e.replaceMarkdownSource(loc.contentEnd, close.stop, body) {
					return ErrStructuralEdit
				}
				if e.step != nil {
					e.Select(min(at, e.d.text.Len()), min(at, e.d.text.Len()))
				}
				return nil
			}
		}
	}
	// Joining the last block with the blank tail after it takes away the
	// blank lines between them, not the line ending the file ends with:
	// W3 keeps the document's final newline (record 2026-11-12, seed12/c3).
	if src := e.d.md.p.String(); nextLoc.leaf < 0 && blankBytes(src[loc.contentEnd:]) {
		if eol := finalEOLStart(src); eol >= loc.contentEnd {
			to = eol
		}
	}
	if !e.replaceMarkdownSource(loc.contentEnd, to, "") {
		return ErrStructuralEdit
	}
	if e.step != nil {
		e.Select(min(at, e.d.text.Len()), min(at, e.d.text.Len()))
	}
	return nil
}

// blankBytes reports whether s holds line endings and whitespace only.
func blankBytes(s string) bool {
	return strings.Trim(s, " \t\r\n") == ""
}

func stripLinePrefix(s, prefix string) string {
	if prefix == "" {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		eol := line[len(body):]
		b.WriteString(strings.TrimPrefix(body, prefix))
		b.WriteString(eol)
	}
	return b.String()
}

func (e *Edit) exitBlock(at int, document bool) error {
	t := e.d.text
	p := t.paraAt(at)
	st := t.paragraph(p).Style
	loc := e.d.paragraphLocation(p)
	if loc.leaf < 0 {
		return ErrStructuralEdit
	}
	b := e.d.md.p.Block(loc.leaf)
	src := e.d.md.p.String()
	eol := defaultEOL(src)
	switch b.Kind {
	case parser.CodeBlock, parser.FrontMatter, parser.MathBlock, parser.HTMLBlock:
	default:
		if st.List == NoList {
			return e.enterBlock(at, false)
		}
		point := e.d.sourceAtDoc(at)
		cont := e.d.continuationPrefix(loc)
		blank := e.d.exitBlankPrefix(loc)
		if document {
			// The paragraph stands below the document's last block, outside
			// every container the block sat in (record 2026-11-10,
			// exitprobe/*-down-eof).
			cont, blank = "", ""
		}
		insert := eol + blank + eol + cont
		if !e.replaceMarkdownSource(point, point, insert) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			e.selectMarkdownSource(point + len(insert))
		}
		return nil
	}
	if !st.CodeBlock && b.Kind == parser.CodeBlock {
		return ErrStructuralEdit
	}
	if b.Kind == parser.CodeBlock && at != t.paraStart(p)+paragraphRunes(t.paragraph(p)) {
		return nil
	}
	at0 := b.End
	closeLine := lineAt(src, b.End)
	if closeLine.start <= b.End && b.End <= closeLine.end && strings.TrimSpace(src[closeLine.start:closeLine.end]) != "" {
		at0 = closeLine.stop
	}
	insert, caret := eol+eol, at0+len(eol)
	if at0 > 0 && src[at0-1] != '\n' && src[at0-1] != '\r' {
		insert, caret = eol+eol+eol, at0+2*len(eol)
	}
	if !e.replaceMarkdownSource(at0, at0, insert) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		e.selectMarkdownSource(caret)
	}
	return nil
}

// emptyBlockKind classifies the empty container line Enter acts on.
type emptyBlockKind uint8

const (
	emptyBlockNone emptyBlockKind = iota
	// An empty top-level list item with siblings leaves the list and
	// becomes a paragraph; the list's only empty item splits instead, and
	// an empty nested item drops only its marker.
	emptyBlockTopItem
	emptyBlockOnlyTopItem
	emptyBlockNestedItem
	emptyBlockOnlyNestedItem
	// An empty quote line leaves the quote.
	emptyBlockQuoteLine
)

// emptyBlockAt classifies the empty container whose line holds source
// offset at. Enter on a nested empty item stays in the preview, since the
// item it outdents still spells itself; the other two leave their
// container and Enter writes that lift first.
func (d *Document) emptyBlockAt(at int) emptyBlockKind {
	d.lock()
	defer d.mu.Unlock()
	return d.emptyBlockAtLocked(at)
}

func (d *Document) emptyBlockAtLocked(at int) emptyBlockKind {
	if d.md == nil {
		return emptyBlockNone
	}
	src := d.md.p.String()
	if at < 0 || at > len(src) {
		return emptyBlockNone
	}
	p := d.text.paraAt(d.docAtSource(at))
	loc := d.paragraphLocation(p)
	st := d.text.paragraph(p).Style
	if loc.item >= 0 && loc.list >= 0 && strings.TrimSpace(src[loc.contentStart:loc.contentEnd]) == "" {
		// The item's own line must be the caret's line: a blank line after
		// the item maps into the item's paragraph but owns no item.
		if item := d.md.p.Block(loc.item); lineAt(src, item.Start).start == lineAt(src, at).start {
			only := d.onlyListItem(loc.item)
			if d.md.p.Block(d.md.p.Block(loc.list).Parent).Kind == parser.Item {
				if only {
					return emptyBlockOnlyNestedItem
				}
				return emptyBlockNestedItem
			}
			if only {
				return emptyBlockOnlyTopItem
			}
			return emptyBlockTopItem
		}
	}
	// A quote line that spells no paragraph of its own, the blank `>` line
	// between two quoted paragraphs, is found by its line: the caret maps
	// to the quoted paragraph after it.
	line := lineAt(src, at)
	body := src[line.start:line.end]
	if at >= line.start && at <= line.end && quoteOnly(body) && strings.Contains(body, ">") &&
		(st.Quote || d.ancestorKind(loc.leaf, parser.Quote) >= 0) {
		return emptyBlockQuoteLine
	}
	if st.Quote && loc.item < 0 && strings.Contains(loc.prefix, ">") &&
		strings.TrimSpace(src[loc.contentStart:loc.contentEnd]) == "" {
		return emptyBlockQuoteLine
	}
	return emptyBlockNone
}

// emptyLift describes the source a lift leaves: the offset the transient
// paragraph it exits to anchors at, the prefix that paragraph writes
// inside a quote, and whether the exit is at the document's end.
type emptyLift struct {
	anchor int
	quote  string
	atEOF  bool
	// materialized marks a lift that wrote the paragraph it exits to into
	// the document: the view holds no transient for it.
	materialized bool
}

// liftEmptyBlock writes the lift Enter performs on an empty top-level list
// item or quote line, as one step of undo. The paragraph it exits to has no
// bytes until its first content; the view holds it as a transient.
func (d *Document) liftEmptyBlock(at int) (emptyLift, bool) {
	d.lock()
	defer d.mu.Unlock()
	kind := d.emptyBlockAtLocked(at)
	if kind != emptyBlockTopItem && kind != emptyBlockQuoteLine {
		return emptyLift{}, false
	}
	var lift emptyLift
	var ok bool
	d.edit(false, func(e *Edit) {
		lift, ok = e.liftEmptyBlock(at, kind)
		if !ok {
			e.fail(ErrStructuralEdit)
		}
	})
	return lift, ok
}

func (e *Edit) liftEmptyBlock(at int, kind emptyBlockKind) (emptyLift, bool) {
	src := e.d.md.p.String()
	p := e.d.text.paraAt(e.d.docAtSource(at))
	loc := e.d.paragraphLocation(p)
	var lift emptyLift
	var start, stop int
	var eol string
	var content string
	var nextMarker orderedMarker
	hasNextMarker := false
	if kind == emptyBlockQuoteLine {
		line := lineAt(src, at)
		body := src[line.start:line.end]
		onLine := at >= line.start && at <= line.end && quoteOnly(body) && strings.Contains(body, ">")
		if !onLine {
			line = loc.line
			body = src[line.start:line.end]
			if !quoteOnly(body) || !strings.Contains(body, ">") {
				return emptyLift{}, false
			}
		}
		start, stop, eol = line.start, line.stop, line.eol
		if onLine && line.end == len(src) {
			// Enter on the last line of a quote that ends the document leaves
			// the whole quote tail, not just that line: the blank quote lines
			// separating it from the paragraph above go with it, so the quote
			// ends on its last content line (record 2026-11-13,
			// zz177/B-up-down-enter and zz177/D-enter-twice). A document whose
			// bytes end in a line ending keeps the tail: the caret stands below
			// it there, on the empty line the ending opens, and the lift takes
			// only the line it holds (zz177/A-empty-line-enter).
			for start > 0 {
				prev := previousMarkdownLine(src, start)
				if prev.start >= prev.end || !quoteOnly(src[prev.start:prev.end]) {
					break
				}
				start = prev.start
			}
		}
	} else {
		if loc.item < 0 || loc.list < 0 || strings.TrimSpace(src[loc.contentStart:loc.contentEnd]) != "" {
			return emptyLift{}, false
		}
		item := e.d.md.p.Block(loc.item)
		start, stop, eol = loc.line.start, loc.line.stop, loc.line.eol
		content = src[loc.contentStart:loc.contentEnd]
		if base := src[start:item.Start]; strings.Contains(base, ">") {
			lift.quote = base
		}
		if e.d.md.p.Block(item.Parent).Ordered {
			if _, next := e.listSiblings(loc.item); next >= 0 {
				n := e.d.md.p.Block(next)
				line := lineAt(src, n.Start)
				if s, end, number, ok := markerNumber(src[n.Start:line.end]); ok && number != 1 {
					nextMarker = orderedMarker{start: n.Start + s, end: n.Start + end, number: number}
					hasNextMarker = true
				}
			}
		}
	}
	if eol == "" {
		eol = defaultEOL(src)
	}
	lift.atEOF = stop == len(src)
	newText := ""
	switch {
	case blankLineBefore(src, start, lift.quote):
		// A blank line already separates the paragraph from the block
		// before it: the line the item or quote owned goes, and the
		// transient paragraph must not add a second separator.
	case lift.atEOF && lift.quote == "":
		// The document ends at the line the paragraph exits, so the line
		// itself goes: its final ending cannot end an empty line. An item
		// whose content is whitespace keeps it: the paragraph the exit
		// opens holds those bytes after the block (record 2026-11-09,
		// seed3/c12).
		if content != "" {
			newText = eol + content
		}
	default:
		newText = eol
		if lift.quote != "" {
			newText = lift.quote + eol
		} else if kind == emptyBlockTopItem {
			// Typora writes the paragraph a top-level item exits to into the
			// live document at once: its line plus the blank lines that
			// separate it from the block below. Blank lines alone cannot
			// spell an empty paragraph in the saved bytes, so the view used
			// to hold the line as a transient and write this newline on the
			// first input; the live model then differed from Typora's at the
			// exit (record 2026-11-11, probe77/between-*). A blank line
			// already after the item line supplies one of the two separators;
			// without it the exit writes both (record 2026-11-11,
			// probe77/mid-item-between-items). A quote line keeps the
			// transient: its paragraph has to keep the quote open.
			newText = eol + eol
			// An item that opens the document has no block above it to
			// separate from: the paragraph's own line and one blank line are
			// the whole exit, so the third ending mid-item exits write does
			// not belong (record 2026-11-13, seed22/c5 against
			// probe77/mid-item-between-items).
			if start > 0 {
				if next := lineAt(src, stop); strings.TrimSpace(src[next.start:next.end]) != "" {
					newText += eol
				}
			}
			lift.materialized = true
		}
	}
	if !e.replaceMarkdownSource(start, stop, newText) {
		return emptyLift{}, false
	}
	if hasNextMarker {
		delta := len(newText) - (stop - start)
		if !e.replaceMarkdownSource(nextMarker.start+delta, nextMarker.end+delta, "1") {
			return emptyLift{}, false
		}
	}
	lift.anchor = start + len(newText)
	if lift.materialized {
		// The paragraph is the line the lift kept, not the separators below
		// it; its caret stands at that line's start (record 2026-11-11,
		// probe77/mid-item-*).
		lift.anchor = start
	}
	if e.step != nil {
		e.selectMarkdownSource(lift.anchor)
	}
	return lift, true
}
