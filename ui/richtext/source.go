package richtext

import (
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

type SegmentKind uint8

const (
	SegmentVerbatim SegmentKind = iota
	SegmentAtomic
	SegmentSynthetic
	SegmentGap
	SegmentBreak
	segmentBreak = SegmentBreak
)

type Segment struct {
	Kind      SegmentKind
	Removable bool
	Shown     int
	Source    int
}
type SourceMap struct {
	paras *seq[paraMap]
}

type MarkerKind uint8

const (
	MarkerStrong MarkerKind = iota + 1
	MarkerEmphasis
	MarkerCode
	MarkerStrikethrough
	MarkerLink
	MarkerHTML
	MarkerHardBreakSpace
	MarkerHardBreakBackslash
)

// hardBreak reports whether k marks a hard line break: the spaces before
// its newline, or its backslash.
func (k MarkerKind) hardBreak() bool {
	return k == MarkerHardBreakSpace || k == MarkerHardBreakBackslash
}

// keepsEmptyMarkers reports whether Typora leaves a span of kind an element
// when its content is deleted and its delimiters stand empty: `****` parses
// as an empty strong. Emphasis, code and strikethrough delimiters stay text;
// links and HTML tags parse empty on their own, so the parser holds those.
func (k MarkerKind) keepsEmptyMarkers() bool {
	return k == MarkerStrong
}

type paraMap struct {
	segs  []Segment
	boxes []box
	lead  int
	src   int
	// codeIndent is how many runes of an indented code block's line the
	// parser consumed as indentation. Typora keeps them in the block's
	// mark but out of its CodeMirror text, whose offsets the recorded
	// visible position uses (record 2026-11-12, probe94/leading-tab-gt).
	codeIndent int
	auto       bool
	codeEmpty  bool
	codeOpen   bool
	owner      uint32
}

type box struct {
	open, close int
	kind        MarkerKind
	start, end  int
	openStart   int
	openEnd     int
	closeStart  int
	closeEnd    int
	contentFrom int
	contentTo   int
	docStart    int
	docEnd      int
	literal     bool
}

func (p *paraMap) weight() int { return p.lead + p.src }

func (p *paraMap) hasContentBox() bool {
	for _, b := range p.boxes {
		if !b.kind.hardBreak() && b.docEnd > b.docStart {
			return true
		}
	}
	return false
}

func (m SourceMap) Len() int { return seqLen(m.paras) }

func (m SourceMap) boxes(i int) []box {
	if i < 0 || i >= m.Len() {
		return nil
	}
	pm := seqAt(m.paras, i)
	if len(pm.boxes) == 0 {
		return nil
	}
	base := m.start(i)
	out := append([]box(nil), pm.boxes...)
	for k := range out {
		out[k].start += base
		out[k].end += base
		out[k].openStart += base
		out[k].openEnd += base
		out[k].closeStart += base
		out[k].closeEnd += base
		out[k].contentFrom += base
		out[k].contentTo += base
	}
	return out
}

func (m SourceMap) Paragraph(i int) (start, end int, segs []Segment) {
	p := seqAt(m.paras, i)
	start = m.start(i)
	return start, start + p.src, p.segs
}

// codeIndentRunes reports how many runes of paragraph i's shown text are
// the indentation an indented code block consumed, which Typora's
// CodeMirror document leaves out.
func (m SourceMap) codeIndentRunes(i int) int {
	if i < 0 || i >= m.Len() {
		return 0
	}
	return seqAt(m.paras, i).codeIndent
}

// appendBreak adds a line break after paragraph i's segments, over the
// source bytes the editor's pending line break holds without the paragraph
// map covering them.
func (m SourceMap) appendBreak(i, shown, source int) SourceMap {
	return m.appendSegment(i, Segment{Kind: segmentBreak, Shown: shown, Source: source})
}

// appendSegment adds a segment after paragraph i's own, over the source
// bytes a view-held edit keeps outside the paragraph map.
func (m SourceMap) appendSegment(i int, seg Segment) SourceMap {
	p := *seqAt(m.paras, i)
	p.segs = append(append([]Segment(nil), p.segs...), seg)
	p.src += seg.Source
	return SourceMap{paras: seqSet(m.paras, i, p)}
}

func (m SourceMap) start(i int) int { return seqWeightBefore(m.paras, i) + seqAt(m.paras, i).lead }

func (m SourceMap) end(i int) int {
	if i < 0 {
		return 0
	}
	return seqWeightBefore(m.paras, i+1)
}

func (m SourceMap) from(x int) int {
	k := seqSearch(m.paras, x)
	if k < m.Len() && m.start(k) < x {
		k++
	}
	return k
}

func (m SourceMap) splice(pa, pz int, built []builtPara, delta int) SourceMap {
	prev := m.end(pa - 1)
	next := -1
	if pz < m.Len() {
		next = m.start(pz) + delta
	}
	items := leads(built, prev)
	paras := seqReplace(m.paras, pa, pz, items)
	if next >= 0 {
		k := pa + len(items)
		last := prev
		if len(built) > 0 {
			last = built[len(built)-1].end
		}
		q := *seqAt(paras, k)
		q.lead = next - last
		paras = seqSet(paras, k, q)
	}
	return SourceMap{paras: paras}
}

func newMarkdownParser() *parser.Document { return parser.New(parser.Typora) }

// previewExtensions is what a transient preview reads: Typora's syntax
// without the empty strong element. A preview holds the marker that keeps
// its blank paragraph alive right after the input, so a run of four stars
// typed before it would read as that element and lose its stars; the live
// editor keeps the pair as an empty element of the view instead (v.empty).
const previewExtensions = parser.Typora &^ parser.ExtTyporaEmptyStrong

func ParseMarkdownSource(src string) (Text, SourceMap) {
	d := newMarkdownParser()
	d.Set(src)
	return projectSource(d)
}

func projectSource(d *parser.Document) (Text, SourceMap) {
	paras, built := projectBlocks(d, 1, -1, true)
	if len(paras) == 0 {
		return blankProjection(d)
	}
	return TextOf(paras...), SourceMap{paras: seqBuild(leads(built, 0))}
}

// blankProjection projects a source with no block of its own: the blank
// lines before the final one are the empty paragraphs they show as, and the
// final empty paragraph sits at the end of the source, where its input
// goes. Reading them as one paragraph would lose the line a caret at that
// end was drawn on once a rune follows.
func blankProjection(d *parser.Document) (Text, SourceMap) {
	m := mdReader{d: d, src: d.String(), track: track{on: true}}
	m.blankParagraphs(-1, len(d.String()), 0, -1)
	at := len(d.String())
	m.reset(at)
	if text := m.blankLineText(0); text != "" && len(m.paras) == 0 {
		// A document that is one line of whitespace holds it as its
		// paragraph: Typora keeps the bytes the session typed and reports
		// them as the paragraph's text, with the caret behind them (record
		// 2026-11-13, seed16/c7).
		n := utf8.RuneCountInString(text)
		m.add(Paragraph{Runs: []Run{{Text: text}}}, builtPara{
			start: 0, end: len(text),
			m: paraMap{segs: []Segment{{Kind: SegmentVerbatim, Shown: n, Source: len(text)}}, src: len(text)},
		})
		return TextOf(m.paras...), SourceMap{paras: seqBuild(leads(m.maps, 0))}
	}
	m.add(Paragraph{}, builtPara{start: at, end: at})
	return TextOf(m.paras...), SourceMap{paras: seqBuild(leads(m.maps, 0))}
}

// projectBlocks projects the blocks from at up to to, -1 for the end of the
// document. leading tells whether the projection starts at the document:
// only then do the blank lines before the first block stand for a paragraph
// of their own. A rebuild of the first block in place keeps the paragraph
// the projection already has for them.
func projectBlocks(d *parser.Document, at, to int, leading bool) ([]Paragraph, []builtPara) {
	m := mdReader{d: d, src: d.String(), track: track{on: true}}
	end := d.Block(0).Next
	for j := at; j < end; j = d.Block(j).Next {
		if to >= 0 && d.Block(j).Start >= to {
			break
		}
		if j == 1 && leading {
			m.blankParagraphs(-1, d.Block(j).Start, 0, -1)
		}
		st, depth := m.quoteBlankOwnerStyle(j)
		m.block(j, st, depth)
		next, keep := len(m.src), 0
		if n := d.Block(j).Next; n < end {
			next, keep = d.Block(n).Start, 1
		}
		m.blankParagraphs(projectBlockEnd(d, j, m.src), next, keep, j)
	}
	return m.paras, m.maps
}

// projectBlockEnd extends a block's end over the underline line of a setext
// heading. The parser ends the heading at its content line; the underline is
// the block's own line, so the blank lines after it project by the same rule
// as those after an ATX heading: `alpha\n=====\n\n` shows the heading and an
// empty paragraph, as `# alpha\n\n` does.
func projectBlockEnd(d *parser.Document, j int, src string) int {
	b := d.Block(j)
	if b.Kind != parser.Heading || !b.Setext {
		return b.End
	}
	end := b.End
	if b.End == b.Start {
		// An empty setext heading ends where its content line starts, though
		// that line may still hold bytes: the tab Tab indented it with. Typora
		// reads them as the heading's content line and reports the heading from
		// that line's start through its underline, `\t\n=====` being one
		// heading at 0:7 showing `\t` (record 2026-11-13, seed22/c9). Ending
		// the heading before its own content line leaves the block ending where
		// its paragraph's map already runs past it, so the view finds no heading
		// for that paragraph and reports the content line alone, 0:1 — neither
		// Typora's span nor the one a cold parse of the bytes reports.
		//
		// A setext heading's end is its content line's end wherever the line
		// holds text; the empty case is the one the parser leaves at the line's
		// start.
		if next := lineAt(src, end).end; next > end {
			end = next
		}
	}
	at := min(end, len(src))
	if at < len(src) && src[at] == '\r' {
		at++
		if at < len(src) && src[at] == '\n' {
			at++
		}
	} else if at < len(src) && src[at] == '\n' {
		at++
	}
	for at < len(src) && src[at] != '\n' && src[at] != '\r' {
		at++
	}
	return max(at, end)
}

func (m *mdReader) blankParagraphs(from, to, keep, prev int) {
	lead := from < 0
	from = max(from, 0)
	if from >= to || strings.Trim(m.src[from:to], " \t\r\n") != "" {
		return
	}
	var lines []int
	if lead {
		lines = append(lines, 0)
	}
	for at := from; at < to; at++ {
		if m.src[at] == '\n' {
			lines = append(lines, at+1)
		}
	}
	if !lead && keep == 0 && len(m.paras) > 0 {
		if st := m.paras[len(m.paras)-1].Style; st.List != NoList {
			// A list followed by trailing whitespace lines keeps them as a
			// paragraph of the item their indentation reaches: Typora
			// shows none of the spaces and keeps them as the paragraph's
			// prefix. When every whitespace line has the same indentation
			// the paragraph sits on the last one, the line its input is
			// written on. Lines indented deeper than the first one write
			// the paragraph inside the last item, which starts on the
			// first line instead: `- a\n  - b- \n  \n    ` sources the
			// empty paragraph at the two-space line as beta's, while
			// `- a\n  - b\n  \n  ` sources it at the last line as
			// alpha's, after the nested list kept the blank line.
			first, last, same := -1, -1, true
			for _, at := range lines {
				text := m.blankLineText(at)
				if text == "" {
					continue
				}
				if first < 0 {
					first = at
				} else if text != m.blankLineText(first) {
					same = false
				}
				last = at
			}
			text := ""
			if last >= 0 {
				text = m.blankLineText(last)
			}
			_, ownerPara, ok := m.blankOwner(prev, len(text))
			if ok {
				at := first
				if same {
					at = last
				}
				text = m.blankLineText(at)
				m.reset(at)
				m.add(Paragraph{Style: m.blankOwnerStyle(ownerPara, st)}, builtPara{
					start: at, end: at + len(text),
					m: paraMap{segs: []Segment{{Kind: SegmentGap, Source: len(text)}}, src: len(text)},
				})
				return
			}
		}
	}
	if !lead && keep == 0 && len(lines) == 2 {
		// One blank line after the final EOL is a real empty paragraph, as
		// Typora shows it on cold open. Like an empty paragraph between
		// blocks it is sourced where its input is written, after the blank
		// line: p1.\n\n has a paragraph p1. and an empty one at 5:5. A line
		// of spaces after that blank line is the paragraph's text: Typora
		// keeps the trailing space a split left, shows it and sources it on
		// the line (record 2026-11-11, seed5/c11).
		at := lines[1]
		m.reset(at)
		if text := m.blankLineText(at); text != "" {
			n := utf8.RuneCountInString(text)
			m.add(Paragraph{Runs: []Run{{Text: text}}}, builtPara{
				start: at, end: at + len(text),
				m: paraMap{segs: []Segment{{Kind: SegmentVerbatim, Shown: n, Source: len(text)}}, src: len(text)},
			})
			return
		}
		m.add(Paragraph{}, builtPara{start: at, end: at})
	}
	for j := 1; j+1 < len(lines)-keep; j += 2 {
		at := lines[j]
		if lead {
			at = lines[j-1]
		}
		m.reset(at)
		// The line the empty paragraph sits on holds its text: input typed
		// there stays in the paragraph, though spaces alone read back as a
		// blank line. Typora keeps them in the paragraph as it is typed,
		// and so does the projection; the source bytes are the same.
		if text := m.blankLineText(at); text != "" {
			n := utf8.RuneCountInString(text)
			m.add(Paragraph{Runs: []Run{{Text: text}}}, builtPara{
				start: at, end: at + len(text),
				m: paraMap{segs: []Segment{{Kind: SegmentVerbatim, Shown: n, Source: len(text)}}, src: len(text)},
			})
			continue
		}
		m.add(Paragraph{}, builtPara{start: at, end: at})
	}
}

// blankOwner finds the list item that the trailing whitespace lines belong
// to: the deepest item whose content starts in the given indentation column.
// It returns the source start of the item's first paragraph, whose
// projection carries the item's style.
func (m *mdReader) blankOwner(list, col int) (int, int, bool) {
	if col <= 0 || list < 0 || m.d.Block(list).Kind != parser.List {
		return 0, 0, false
	}
	deepest := -1
	end := m.d.Block(list).Next
	for k := list + 1; k < end; k++ {
		if b := m.d.Block(k); b.Kind == parser.Item && (deepest < 0 || b.Start > m.d.Block(deepest).Start) {
			deepest = k
		}
	}
	if deepest < 0 {
		return 0, 0, false
	}
	chain := []int{deepest}
	for {
		parent := m.d.Block(chain[len(chain)-1]).Parent
		if parent < 0 {
			break
		}
		up := m.d.Block(parent).Parent
		if up < 0 || m.d.Block(up).Kind != parser.Item {
			break
		}
		chain = append(chain, up)
	}
	for _, item := range chain {
		start := m.itemParagraphStart(item)
		if start < 0 {
			// An item with no paragraph child spells its content on its own
			// marker line, which ends where its content column starts. A
			// whitespace line at that column is the item's empty second
			// paragraph only when a blank line separates the two: the line
			// right after the marker line continues the item's content
			// instead and stays out of the trailing-whitespace rule (record
			// 2026-11-12, seed15/c11).
			if m.itemHasNoParagraphOwner(item, col) {
				return item, lineAt(m.src, m.d.Block(item).Start).end, true
			}
			continue
		}
		if start-m.lineStart(start) == col {
			return item, start, true
		}
		if m.d.Block(item).Task != parser.NoTask && m.itemContinuationColumn(item) == col {
			// A task item's continuation lines start under its bullet, not
			// under its content: `- [ ] a` continues at two spaces, where
			// Typora shows the item's empty second paragraph (record
			// 2026-11-12, seed12/c9).
			return item, start, true
		}
	}
	return 0, 0, false
}

// quoteBlankOwnerStyle returns the style of the empty item whose soft-break
// continuation line now opens a quote. CommonMark closes `- ` at the blank
// separator, so the indented `> ` is a top-level quote; Typora keeps it as
// the item's own quote block (record 2026-11-14, seed23/c8).
func (m *mdReader) quoteBlankOwnerStyle(quote int) (ParagraphStyle, int) {
	none := ParagraphStyle{}
	b := m.d.Block(quote)
	if b.Kind != parser.Quote {
		return none, -1
	}
	lineStart := m.lineStart(b.Start)
	col := b.Start - lineStart
	if col <= 0 || strings.Trim(m.src[lineStart:b.Start], " ") != "" {
		return none, -1
	}
	end := m.d.Block(0).Next
	prev := -1
	for k := 1; k < end; k = m.d.Block(k).Next {
		if k == quote {
			break
		}
		prev = k
	}
	if prev < 0 || m.d.Block(prev).Kind != parser.List {
		return none, -1
	}
	item, ownerPara, ok := m.blankOwner(prev, col)
	if !ok || m.itemParagraphStart(item) >= 0 {
		return none, -1
	}
	first := lineAt(m.src, m.d.Block(item).Start)
	blank := lineAt(m.src, first.stop)
	if blank.stop != lineStart {
		return none, -1
	}
	st := m.blankOwnerStyle(ownerPara, m.itemStyle(item))
	return st, st.Indent
}

// itemStyle restores an item's style from the AST when its projection has no
// paragraph to copy, as an incremental rebuild starting at a quote has.
func (m *mdReader) itemStyle(item int) ParagraphStyle {
	b := m.d.Block(item)
	list := m.d.Block(b.Parent)
	st := ParagraphStyle{List: Bullets}
	if list.Ordered {
		st.List = Numbers
	}
	if b.Task != parser.NoTask {
		st.List, st.Checked = Tasks, b.Task == parser.Checked
	}
	for parent := b.Parent; parent >= 0; parent = m.d.Block(parent).Parent {
		switch m.d.Block(parent).Kind {
		case parser.Item:
			st.Indent++
		case parser.Quote:
			st.Quote = true
		}
	}
	return st
}

// itemHasNoParagraphOwner reports whether an item with no paragraph child
// owns the trailing whitespace line at column col: the item's own line ends
// where that column starts, and a blank line stands between the two.
func (m *mdReader) itemHasNoParagraphOwner(item, col int) bool {
	first := lineAt(m.src, m.d.Block(item).Start)
	// A task item's continuation line stands under its bullet, not under its
	// content: `- [ ] ` owns the two-space line below it just as `- ` does, so
	// Enter at the item's end keeps that line as the item's empty second
	// paragraph (record 2026-11-13, seed22/c2).
	column := first.end - first.start
	if m.d.Block(item).Task != parser.NoTask {
		column = m.itemContinuationColumn(item)
	}
	if column != col {
		return false
	}
	// A blank line has to separate the item's own line from the whitespace
	// line: without it the whitespace line continues the item's content
	// (`- \n  \n`, a held soft break) and is no second paragraph.
	blank := lineAt(m.src, first.stop)
	return blank.start == first.stop && blank.end == blank.start
}

// itemContinuationColumn is the column an item's continuation lines start
// at: its marker up to a task check box, on its parent item's continuation
// column when it is nested. Typora measures a task item's continuation from
// the bullet, not from the content after its check box.
func (m *mdReader) itemContinuationColumn(item int) int {
	b := m.d.Block(item)
	first := lineAt(m.src, b.Start)
	content := first.end
	for child := range m.d.Children(item) {
		if start := m.d.Block(child).Start; start >= b.Start {
			content = min(start, first.end)
		}
		break
	}
	marker := m.src[b.Start:content]
	if i := strings.IndexByte(marker, '['); i >= 0 {
		marker = marker[:i]
	}
	base := b.Start - first.start
	if list := b.Parent; list > 0 {
		if parent := m.d.Block(list).Parent; parent > 0 && m.d.Block(parent).Kind == parser.Item {
			base = m.itemContinuationColumn(parent)
		}
	}
	return base + len(marker)
}

// itemParagraphStart returns the source start of an item's first paragraph
// child, or -1 when the item holds none.
func (m *mdReader) itemParagraphStart(item int) int {
	next := m.d.Block(item).Next
	for k := item + 1; k < next; k++ {
		if b := m.d.Block(k); b.Parent == item && (b.Kind == parser.Paragraph || b.Kind == parser.Heading) {
			return b.Start
		}
	}
	return -1
}

// blankOwnerStyle returns the style the paragraph projected for the owner
// item's first paragraph has, so that the empty paragraph the blank lines
// write reports the item's list, depth and check box.
func (m *mdReader) blankOwnerStyle(start int, fallback ParagraphStyle) ParagraphStyle {
	for k := range m.maps {
		if m.maps[k].start == start {
			return m.paras[k].Style
		}
	}
	return fallback
}

// blankLineText returns the spaces and tabs of the line at lineStart, whose
// text an empty paragraph of a blank run holds, or "" when the line is empty
// or holds anything else.
func (m *mdReader) blankLineText(lineStart int) string {
	at := lineStart
	for at < len(m.src) && m.src[at] != '\n' && m.src[at] != '\r' {
		at++
	}
	if strings.Trim(m.src[lineStart:at], " \t") != "" {
		return ""
	}
	return m.src[lineStart:at]
}

func (m *mdReader) quoteBlanks(st ParagraphStyle, from, to, keep int) {
	if keep == 0 {
		for {
			stop := lineAt(m.src, to).stop
			line := lineAt(m.src, stop)
			if stop >= len(m.src) || !quoteOnly(m.src[line.start:line.end]) {
				break
			}
			to = line.end
		}
	}
	if from >= to || strings.Trim(m.src[from:to], " \t\r\n>") != "" {
		return
	}
	var lines []int
	for at := from; at < to; at++ {
		if m.src[at] == '\n' {
			lines = append(lines, at+1)
		}
	}
	if keep == 0 {
		lines = append(lines, to+1)
	}
	for j := 1; j+1 < len(lines)-keep; j += 2 {
		at := lines[j]
		for at < len(m.src) && (m.src[at] == ' ' || m.src[at] == '>') {
			at++
		}
		style := st
		if n := len(m.maps); n > 0 {
			// A quote line indented to a list item's content column is that
			// item's empty second paragraph, not a paragraph of the quote:
			// `> - parent\n>\n>   ` holds it at the bar and three spaces.
			if prev := m.paras[n-1].Style; prev.List != NoList && prev.Quote {
				start := m.maps[n-1].start
				if start-m.lineStart(start) == at-lines[j] {
					style = prev
				}
			}
		}
		m.reset(at)
		m.add(Paragraph{Style: style}, builtPara{start: at, end: at, m: paraMap{owner: m.owner}})
	}
}

func leads(items []builtPara, end int) []paraMap {
	maps := make([]paraMap, len(items))
	for i := range items {
		base := items[i].start
		maps[i] = items[i].m
		if base != 0 && len(maps[i].boxes) > 0 {
			boxes := append([]box(nil), maps[i].boxes...)
			for j := range boxes {
				boxes[j].start -= base
				boxes[j].end -= base
				boxes[j].openStart -= base
				boxes[j].openEnd -= base
				boxes[j].closeStart -= base
				boxes[j].closeEnd -= base
				boxes[j].contentFrom -= base
				boxes[j].contentTo -= base
			}
			maps[i].boxes = boxes
		}
		maps[i].lead = items[i].start - end
		end = items[i].end
	}
	return maps
}

type builtPara struct {
	start, end int
	m          paraMap
}
type track struct {
	on         bool
	pos, start int
	// prefix is the bytes of container prefix before the content of the
	// block being read, which its continuation lines repeat.
	prefix    int
	segs      []Segment
	boxes     []box
	htmlOpen  [4][]int
	auto, bad bool
	owner     uint32
	maps      []builtPara
}

func (m *mdReader) reset(at int) {
	if m.on {
		m.pos, m.start = at, at
		m.segs, m.boxes = nil, nil
		m.htmlOpen = [4][]int{}
		m.auto, m.bad = false, false
	}
}

func (m *mdReader) seg(k SegmentKind, shown, source int) {
	if m.on {
		m.segs = append(m.segs, Segment{Kind: k, Shown: shown, Source: source})
		m.pos += source
	}
}

func (m *mdReader) object(shown, source int) {
	if m.on {
		m.segs = append(m.segs, Segment{Kind: SegmentSynthetic, Removable: true, Shown: shown, Source: source})
		m.pos += source
	}
}

func (m *mdReader) verbatim(shown, source int) {
	if k := len(m.segs) - 1; k >= 0 && m.segs[k].Kind == SegmentVerbatim {
		m.segs[k].Shown += shown
		m.segs[k].Source += source
	} else {
		m.segs = append(m.segs, Segment{Kind: SegmentVerbatim, Shown: shown, Source: source})
	}
	m.pos += source
}

func (m *mdReader) gapTo(x int) {
	switch {
	case !m.on || x == m.pos:
	case x < m.pos:
		m.bad = true
	default:
		m.seg(SegmentGap, 0, x-m.pos)
	}
}

func (m *mdReader) contentStart(i int, in []parser.Inline) int {
	if !m.on {
		return 0
	}
	if len(in) > 0 {
		return in[0].Start
	}
	if m.d.LineCount(i) > 0 {
		_, at := m.d.Line(i, 0)
		return at
	}
	return m.lineEnd(m.d.Block(i).Start)
}

func (m *mdReader) lineEnd(at int) int {
	for at < len(m.src) && m.src[at] != '\n' && m.src[at] != '\r' {
		at++
	}
	return at
}
func (m *mdReader) text(start, end int, text string) {
	if !m.on {
		return
	}
	src := m.src[start:end]
	if src == text {
		m.verbatim(utf8.RuneCountInString(src), len(src))
		return
	}
	i, j := 0, 0
	for j < len(text) && text[j] == ' ' && (i >= len(src) || src[i] != ' ') {
		// Whitespace the text shows before its source's own first byte: a tab
		// the parser consumed in part shows its rest as columns the line's
		// bytes do not hold, as `- a\n\n\tq` shows "  q" over the source's
		// "q". Mapping the columns onto that byte would let input at the
		// caret land on the tab's columns (record 2026-11-12, TestFeelFuzz
		// seed 1, the last step's probe).
		m.seg(SegmentSynthetic, 1, 0)
		j++
	}
	for i < len(src) && j < len(text) {
		c := src[i]
		if c == '\\' && i+1 < len(src) && escapable(src[i+1]) && text[j] == src[i+1] {
			m.seg(SegmentAtomic, 1, 2)
			i, j = i+2, j+1
			continue
		}
		if c == '&' {
			if n, t := parser.Entity(src[i:]); n > 0 && strings.HasPrefix(text[j:], t) {
				m.seg(SegmentAtomic, utf8.RuneCountInString(t), n)
				i, j = i+n, j+len(t)
				continue
			}
		}
		if c != text[j] {
			break
		}
		shown := 0
		if utf8.RuneStart(c) {
			shown = 1
		}
		m.verbatim(shown, 1)
		i, j = i+1, j+1
	}
	if i < len(src) || j < len(text) {
		m.seg(SegmentSynthetic, utf8.RuneCountInString(text[j:]), len(src)-i)
	}
}
func (m *mdReader) code(n *parser.Inline) {
	if !m.on {
		return
	}
	src := m.src[n.Start:n.End]
	k := parser.CodeFence(m.src, n)
	if 2*k > len(src) {
		m.seg(SegmentSynthetic, utf8.RuneCountInString(n.Text), len(src))
		return
	}
	in := src[k : len(src)-k]
	pad := 0
	if len(in) >= 2 && in[0] == ' ' && in[len(in)-1] == ' ' && strings.Trim(in, " ") != "" {
		pad = 1
	}
	body := in[pad : len(in)-pad]
	if body != n.Text || strings.ContainsAny(in, "\r\n") {
		// The span shows other text than its body, as one holding a line
		// ending does: the ending shows as one space, and the prefix the
		// next line repeats is part of the break. Map the body's runs to
		// the text so a caret inside the span stays beside the rune it
		// was drawn at; a body that does not line up stays one synthetic
		// run.
		at, segs := m.pos, len(m.segs)
		open := len(m.segs)
		m.seg(SegmentGap, 0, k+pad)
		if m.codeBody(body, n.Text) {
			close := len(m.segs)
			m.seg(SegmentGap, 0, k+pad)
			if hiddenCodeSpan(src, n.Text) {
				openEnd := openEndOfCode(n.Start, k, pad, in)
				m.boxes = append(m.boxes, box{
					open: open, close: close, kind: MarkerCode,
					start: n.Start, end: n.End,
					openStart: n.Start, openEnd: openEnd,
					closeStart: n.End - k, closeEnd: n.End,
					contentFrom: openEnd, contentTo: n.End - k,
				})
			}
			return
		}
		m.pos, m.segs = at, m.segs[:segs]
		m.seg(SegmentSynthetic, utf8.RuneCountInString(n.Text), len(src))
		return
	}
	open := len(m.segs)
	m.seg(SegmentGap, 0, k+pad)
	if body != "" {
		m.verbatim(utf8.RuneCountInString(body), len(body))
	}
	close := len(m.segs)
	m.seg(SegmentGap, 0, k+pad)
	m.boxes = append(m.boxes, box{
		open: open, close: close, kind: MarkerCode,
		start: n.Start, end: n.End,
		openStart: n.Start, openEnd: n.Start + k,
		closeStart: n.End - k, closeEnd: n.End,
		contentFrom: n.Start + k, contentTo: n.End - k,
	})
}

// hiddenCodeSpan reports whether a code span renders as nothing: its body
// holds a line ending and the text it would show is all whitespace. Typora
// draws no content for such a span until the caret reveals its delimiters
// (record 2026-11-09, diff.json ac6ec2fb).
func hiddenCodeSpan(span, text string) bool {
	return text != "" && strings.TrimSpace(text) == "" && strings.ContainsAny(span, "\r\n")
}

// openEndOfCode returns the source offset a code span's opening marker
// reaches when the span shows the line ending it holds: the delimiters,
// their padding and the ending, which the revealed marker draws.
func openEndOfCode(start, k, pad int, in string) int {
	brk := strings.IndexAny(in[pad:], "\r\n")
	if brk < 0 {
		return start + k + pad
	}
	n := 1
	if in[pad+brk] == '\r' && pad+brk+1 < len(in) && in[pad+brk+1] == '\n' {
		n = 2
	}
	return start + k + pad + brk + n
}

// codeBody maps a code span's source body to the text it shows: a line
// ending, with the prefix the next line repeats, shows as one space, and
// every other rune shows itself. It reports whether the whole body lined
// up; a partial mapping is the caller's to drop.
func (m *mdReader) codeBody(body, text string) bool {
	i, j := 0, 0
	for i < len(body) && j < len(text) {
		if c := body[i]; c == '\n' || c == '\r' {
			brk := 1
			if c == '\r' && i+1 < len(body) && body[i+1] == '\n' {
				brk = 2
			}
			prefix := 0
			for prefix < m.prefix && i+brk+prefix < len(body) &&
				(body[i+brk+prefix] == ' ' || body[i+brk+prefix] == '\t') {
				prefix++
			}
			if text[j] != ' ' {
				return false
			}
			m.seg(SegmentSynthetic, 1, brk+prefix)
			i += brk + prefix
			j++
			continue
		}
		_, size := utf8.DecodeRuneInString(body[i:])
		if i+size > len(body) || j+size > len(text) || body[i:i+size] != text[j:j+size] {
			return false
		}
		m.verbatim(1, size)
		i += size
		j += size
	}
	return i == len(body) && j == len(text)
}

// codeLine maps line k of a code block, whose content starts at source
// offset at, with the indentation the parser took off it: bytes the source
// holds before at, which an indented block shows as the line's first
// column and which stay outside its editable bytes, as a container's
// prefix does (record 2026-11-12, probe94/leading-tab-gt).
func (m *mdReader) codeLine(line string, at int, indent string) builtPara {
	if !m.on {
		return builtPara{}
	}
	pad := 0
	for pad < len(line) && line[pad] == ' ' && !strings.HasPrefix(m.src[at:], line[pad:]) {
		pad++
	}
	body := line[pad:]
	if !strings.HasPrefix(m.src[at:], body) {
		return m.opaque(at, at, utf8.RuneCountInString(indent+line))
	}
	var segs []Segment
	if indent != "" {
		segs = append(segs, Segment{Kind: SegmentSynthetic, Shown: utf8.RuneCountInString(indent)})
	}
	if pad > 0 {
		segs = append(segs, Segment{Kind: SegmentSynthetic, Shown: pad})
	}
	if body != "" {
		segs = append(segs, Segment{Kind: SegmentVerbatim, Shown: utf8.RuneCountInString(body), Source: len(body)})
	}
	return builtPara{start: at, end: at + len(body), m: paraMap{
		segs:       segs,
		src:        len(body),
		codeIndent: utf8.RuneCountInString(indent),
		owner:      m.owner,
	}}
}
func (m *mdReader) codeEmpty(i int) builtPara {
	b := m.d.Block(i)
	at := b.End
	if b.Open {
		at = len(m.src)
	} else {
		at = m.lineEnd(b.Start)
		if at < len(m.src) && m.src[at] == '\r' {
			at++
		}
		if at < len(m.src) && m.src[at] == '\n' {
			at++
		}
	}
	return builtPara{start: at, end: at, m: paraMap{src: 0, codeEmpty: true, codeOpen: b.Open, owner: m.owner}}
}
func (m *mdReader) literalBlock(st ParagraphStyle, i int) {
	b := m.d.Block(i)
	at, end := b.Start, b.End
	for at <= end {
		lineEnd := m.lineEnd(at)
		if lineEnd > end {
			lineEnd = end
		}
		p := Paragraph{Style: st}
		if lineEnd > at {
			p.Runs = []Run{{Text: m.src[at:lineEnd]}}
		}
		m.reset(at)
		if lineEnd > at {
			m.verbatim(utf8.RuneCountInString(m.src[at:lineEnd]), lineEnd-at)
		}
		m.addTracked(p)
		if lineEnd >= end {
			break
		}
		at = lineEnd
		if at < end && m.src[at] == '\r' {
			at++
		}
		if at < end && m.src[at] == '\n' {
			at++
		}
	}
}
func (m *mdReader) opaque(start, end, shown int) builtPara {
	if !m.on {
		return builtPara{}
	}
	b := builtPara{start: start, end: end, m: paraMap{src: end - start, owner: m.owner}}
	if shown > 0 || end > start {
		b.m.segs = []Segment{{Kind: SegmentSynthetic, Shown: shown, Source: end - start}}
	}
	return b
}

func (m *mdReader) add(p Paragraph, b builtPara) {
	if b.m.owner == 0 {
		b.m.owner = m.owner
	}
	m.paras = append(m.paras, p)
	if m.on {
		m.maps = append(m.maps, b)
	}
}
func (m *mdReader) addTracked(p Paragraph) {
	m.paras = append(m.paras, p)
	if !m.on {
		return
	}
	w := paragraphRunes(p)
	shown := 0
	for _, s := range m.segs {
		shown += s.Shown
	}
	if m.bad || shown != w {
		m.maps = append(m.maps, m.opaque(m.start, m.pos, w))
		return
	}
	var boxes []box
	prefix := make([]int, len(m.segs)+1)
	for k, s := range m.segs {
		prefix[k+1] = prefix[k] + s.Shown
	}
	for _, b := range m.boxes {
		if b.kind.hardBreak() {
			if b.start >= m.start && b.start <= m.pos {
				q := b
				q.docStart = m.docAt(b.start)
				q.docEnd = q.docStart
				boxes = append(boxes, q)
			}
			continue
		}
		q := b
		if q.open >= 0 {
			q.docStart = prefix[q.open]
		} else {
			q.docStart = 0
		}
		if q.close >= 0 {
			q.docEnd = prefix[q.close]
		} else {
			q.docEnd = prefix[len(m.segs)]
		}
		boxes = append(boxes, q)
	}
	m.maps = append(m.maps, builtPara{start: m.start, end: m.pos, m: paraMap{segs: m.segs, boxes: boxes, src: m.pos - m.start, auto: m.auto, owner: m.owner}})
}

// docAt returns the rune of the paragraph at the start of the segment that
// holds source offset at. Callers pass the starts of markers, which begin
// segments, so an offset within a segment is not mapped rune by rune.
func (m *mdReader) docAt(at int) int {
	n, x := 0, m.start
	for _, s := range m.segs {
		if at <= x || at < x+s.Source {
			return n
		}
		n += s.Shown
		x += s.Source
	}
	return n
}

func paragraphRunes(p Paragraph) int {
	n := 0
	for _, r := range p.Runs {
		n += utf8.RuneCountInString(r.Text)
	}
	return n
}

func escapable(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func (p *paraMap) insertions(src string, o int) []int {
	var at []int
	sh, so := 0, 0
	for _, s := range p.segs {
		if sh == o && (len(at) == 0 || at[len(at)-1] != so) {
			at = append(at, so)
		}
		if sh < o && o < sh+s.Shown {
			if s.Kind != SegmentVerbatim {
				return nil
			}
			return []int{so + byteOfRune(src[so:so+s.Source], o-sh)}
		}
		sh += s.Shown
		so += s.Source
	}
	if sh == o && (len(at) == 0 || at[len(at)-1] != so) {
		at = append(at, so)
	}
	if o == 0 {
		for i, j := 0, len(at)-1; i < j; i, j = i+1, j-1 {
			at[i], at[j] = at[j], at[i]
		}
	}
	return at
}
func (p *paraMap) deletion(src string, a, b int) (from, to int, kept string, joins []int, ok bool) {
	type span struct{ from, to int }
	cut := make([]span, len(p.segs))
	whole := make([]bool, len(p.segs))
	starts := make([]int, len(p.segs)+1)
	shows := make([]int, len(p.segs)+1)
	sh := 0
	for k, s := range p.segs {
		so := starts[k]
		starts[k+1] = so + s.Source
		shows[k] = sh
		cut[k] = span{-1, -1}
		x, y := max(a, sh), min(b, sh+s.Shown)
		if x < y {
			covered := x == sh && y == sh+s.Shown
			switch {
			case s.Kind == SegmentVerbatim:
				body := src[so : so+s.Source]
				cut[k] = span{so + byteOfRune(body, x-sh), so + byteOfRune(body, y-sh)}
				whole[k] = covered
			case covered && (s.Kind == SegmentAtomic || s.Kind == SegmentBreak || s.Removable):
				cut[k] = span{so, so + s.Source}
				whole[k] = true
			default:
				return 0, 0, "", nil, false
			}
		}
		sh += s.Shown
	}
	// A hard break is a marker before its break: deleting the break deletes
	// the marker with it.
	for _, bx := range p.boxes {
		if !bx.kind.hardBreak() {
			continue
		}
		for k, s := range p.segs {
			if whole[k] && s.Kind == SegmentBreak && shows[k] == bx.docStart {
				cut[k].from = min(cut[k].from, bx.openStart)
				cut[k].to = max(cut[k].to, bx.openEnd)
			}
		}
	}
	for _, bx := range p.boxes {
		if bx.open < 0 || bx.close < 0 || bx.open >= len(p.segs) || bx.close >= len(p.segs) || bx.open > bx.close {
			continue
		}
		empty, shown := true, false
		for k := bx.open + 1; k < bx.close; k++ {
			if p.segs[k].Shown > 0 {
				shown = true
				empty = empty && whole[k]
			}
		}
		if empty && shown {
			for _, k := range []int{bx.open, bx.close} {
				cut[k] = span{starts[k], starts[k+1]}
			}
		}
	}
	from, to = -1, -1
	var b2 strings.Builder
	for _, c := range cut {
		if c.from < 0 || c.from == c.to {
			continue
		}
		if from < 0 {
			from = c.from
			joins = append(joins, 0)
		} else if c.from > to {
			b2.WriteString(src[to:c.from])
			joins = append(joins, b2.Len())
		}
		to = c.to
	}
	if from < 0 {
		return 0, 0, "", nil, false
	}
	return from, to, b2.String(), joins, true
}

func byteOfRune(s string, n int) int {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}
func sameParagraph(p, q Paragraph, links bool) bool {
	if p.Style != q.Style {
		return false
	}
	a, b := shownRuns(p.Runs, links), shownRuns(q.Runs, links)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i].Attributes, b[i].Attributes
		if a[i].Text != b[i].Text || !sameAttachment(x.Attachment, y.Attachment) {
			return false
		}
		x.Attachment, y.Attachment = nil, nil
		if x != y {
			return false
		}
	}
	return true
}

func shownRuns(runs []Run, links bool) []Run {
	var out []Run
	for _, r := range runs {
		if !links {
			r.Attributes.Link = ""
		}
		if k := len(out) - 1; k >= 0 && r.Attributes.Attachment == nil && out[k].Attributes.Attachment == nil && out[k].Attributes == r.Attributes {
			out[k].Text += r.Text
			continue
		}
		out = append(out, r)
	}
	return out
}

func sameAttachment(a, b *Attachment) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.URL == b.URL && a.Width == b.Width && a.Height == b.Height
}

func sameParaMap(p, q *paraMap) bool {
	if p.src != q.src || p.lead != q.lead || p.auto != q.auto || p.codeEmpty != q.codeEmpty || p.codeOpen != q.codeOpen || len(p.segs) != len(q.segs) || len(p.boxes) != len(q.boxes) {
		return false
	}
	for i := range p.segs {
		if p.segs[i] != q.segs[i] {
			return false
		}
	}
	for i := range p.boxes {
		if p.boxes[i] != q.boxes[i] {
			return false
		}
	}
	return true
}
