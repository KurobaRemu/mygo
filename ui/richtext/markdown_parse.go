package richtext

import (
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

// ParseMarkdown returns the attributed text of Markdown, CommonMark with
// GitHub's extensions and footnotes, as Markdown writes it: headings,
// lists and task lists, quotations, blocks of code, bold, italic,
// strikethrough, code, links, images, and the HTML tags of these marks
// and of underlines, <u>. A list item's paragraphs after its first are
// indented under it; a table's rows are paragraphs of their cells apart
// by tabs, the header's bold. Math, which the editor does not show, stays
// as written: \[ is an escaped bracket, as everywhere but in math.
func ParseMarkdown(s string) Text {
	d := newMarkdownParser()
	d.Set(s)
	m := mdReader{d: d, src: s}
	m.blocks(0, ParagraphStyle{}, -1)
	if len(m.paras) == 0 {
		return Text{}
	}
	return TextOf(m.paras...)
}

// mdReader reads the blocks and inlines of a Markdown document into
// paragraphs.
type mdReader struct {
	d     *parser.Document
	paras []Paragraph
	// html counts the inline HTML tags open, by the attribute they turn
	// on: <u>, <s>, <b> and <i> and their synonyms.
	html    [4]int
	htmlTag htmlTag
	src     string
	track
}

// blocks adds the children of block i in style st; depth is the level of
// the list they are in, -1 outside lists.
func (m *mdReader) blocks(i int, st ParagraphStyle, depth int) {
	for k := range m.d.Children(i) {
		m.block(k, st, depth)
	}
}

func (m *mdReader) block(i int, st ParagraphStyle, depth int) {
	b := m.d.Block(i)
	outer := m.owner
	if b.Kind == parser.CodeBlock || b.Kind == parser.Quote {
		m.owner = uint32(i + 1)
	}
	defer func() { m.owner = outer }()
	switch b.Kind {
	case parser.Paragraph:
		m.inlinePara(st, i)
	case parser.Heading:
		st.Heading = b.Level
		m.inlinePara(st, i)
	case parser.FrontMatter, parser.ThematicBreak, parser.MathBlock:
		m.literalBlock(st, i)
	case parser.CodeBlock:
		st.CodeBlock, st.Heading = true, 0
		n := m.d.LineCount(i)
		for k := range n {
			line, at := m.d.Line(i, k)
			indent := m.d.LineIndent(i, k)
			p := Paragraph{Style: st}
			if indent+line != "" {
				p.Runs = []Run{{Text: indent + line}}
			}
			m.add(p, m.codeLine(line, at, indent))
		}
		if n == 0 {
			m.add(Paragraph{Style: st}, m.codeEmptyBodyLine(i, st))
		}
	case parser.HTMLBlock:
		if m.on {
			m.literalBlock(st, i)
			break
		}
		at := b.Start
		for _, p := range ParseHTML(m.d.Lines(i)).Paragraphs() {
			depth := p.Style.quoteDepth() + st.quoteDepth()
			p.Style.Quote = depth > 0
			if depth > 1 {
				p.Style.QuoteDepth = depth
			}
			p.Style.Indent += st.Indent
			m.add(p, m.opaque(at, b.End, paragraphRunes(p)))
			at = b.End
		}
	case parser.Quote:
		if st.Quote {
			st.QuoteDepth = st.quoteDepth() + 1
		}
		st.Quote = true
		before := len(m.paras)
		if m.on {
			first := -1
			for k := range m.d.Children(i) {
				first = k
				break
			}
			if first >= 0 {
				firstLine := lineAt(m.src, b.Start)
				if start := m.d.Block(first).Start; start > firstLine.stop &&
					quoteOnly(m.src[firstLine.start:firstLine.end]) &&
					quoteGapBlank(m.src, firstLine.stop, start) {
					// A bare ">" line between the quote's empty first line
					// and its first paragraph is the paragraph separator:
					// the empty line is a paragraph of its own, not the
					// following paragraph's first line (record 2026-11-10,
					// seed5/c10).
					at := m.quoteContentAt(b.Start)
					m.reset(at)
					m.add(Paragraph{Style: st}, builtPara{start: at, end: at, m: paraMap{owner: m.owner}})
				}
			}
		}
		for k := range m.d.Children(i) {
			m.block(k, st, depth)
			next, keep := b.End, 0
			if n := m.d.Block(k).Next; n < b.Next {
				next, keep = m.d.Block(n).Start, 1
			}
			m.quoteBlanks(st, m.d.Block(k).End, next, keep)
		}
		if m.on && len(m.paras) == before {
			at := b.Start
			for at < len(m.src) && at < 3+b.Start && m.src[at] == ' ' {
				at++
			}
			if at < len(m.src) && m.src[at] == '>' {
				at++
				if at < len(m.src) && m.src[at] == ' ' {
					at++
				}
			}
			m.reset(at)
			m.add(Paragraph{Style: st}, builtPara{start: at, end: at, m: paraMap{owner: m.owner}})
		}
	case parser.List:
		kind := Bullets
		if b.Ordered {
			kind = Numbers
		}
		prevEnd := -1
		split := false
		for it := range m.d.Children(i) {
			ist := ParagraphStyle{List: kind, Indent: depth + 1, Quote: st.Quote, QuoteDepth: st.QuoteDepth}
			if split {
				// Items after a blank run that split the list start a list of
				// their own at the document root, whatever their nesting
				// before it (record 2026-11-10, newops/cold-gap-nested).
				ist.Indent = 0
			}
			if task := m.d.Block(it).Task; task != parser.NoTask {
				ist.List, ist.Checked = Tasks, task == parser.Checked
			}
			if m.listGapParagraph(prevEnd, m.d.Block(it).Start, st, ist) {
				split, ist.Indent = true, 0
			}
			m.item(it, ist, depth+1)
			prevEnd = m.d.Block(it).End
		}
	case parser.Table:
		if m.on {
			m.literalBlock(st, i)
			break
		}
		for row := range m.d.Children(i) {
			p := Paragraph{Style: st}
			var a Attributes
			if m.d.Block(row).Header {
				a.Weight = 700
			}
			m.reset(m.d.Block(row).Start)
			first := true
			for cell := range m.d.Children(row) {
				m.gapTo(m.d.Block(cell).Start)
				if !first {
					p.Runs = addMDRun(p.Runs, Run{Text: "\t", Attributes: a})
					m.seg(SegmentSynthetic, 1, 0)
				}
				first = false
				for _, r := range m.runs(m.d.Inlines(cell), a) {
					p.Runs = addMDRun(p.Runs, r)
				}
			}
			m.addTracked(p)
		}
	case parser.FootnoteDefinition:
		if m.on {
			m.literalBlock(st, i)
			break
		}
		from, mapFrom := len(m.paras), len(m.maps)
		m.blocks(i, st, depth)
		if from < len(m.paras) {
			p := &m.paras[from]
			prefix := "[" + b.Label() + "] "
			p.Runs = append([]Run{{Text: prefix}}, p.Runs...)
			if m.on {
				q := &m.maps[mapFrom].m
				q.segs = append([]Segment{{Kind: SegmentSynthetic, Shown: utf8.RuneCountInString(prefix)}}, q.segs...)
				shift := utf8.RuneCountInString(prefix)
				for k := range q.boxes {
					q.boxes[k].open++
					q.boxes[k].close++
					q.boxes[k].docStart += shift
					q.boxes[k].docEnd += shift
				}
			}
		}
	}
}

// item adds a list item: its first paragraph in the item's style ist, and
// what follows indented under it.
func (m *mdReader) item(i int, ist ParagraphStyle, depth int) {
	under := ParagraphStyle{Indent: depth + 1, Quote: ist.Quote, QuoteDepth: ist.QuoteDepth}
	first := true
	prev := -1
	col := -1
	if start := m.itemParagraphStart(i); start >= 0 {
		col = start - m.lineStart(start)
	}
	for k := range m.d.Children(i) {
		b := m.d.Block(k)
		if prev >= 0 && b.Kind == parser.List {
			// A blank line and the item's continuation line between two of
			// its blocks are the item's empty paragraph, as the item's own
			// line Enter leaves it before a following child:
			// `- parent\n\n  \n  - b` holds an empty paragraph at the
			// two-space line.
			m.itemBlankParagraph(m.d.Block(prev).End, b.Start, ist, col)
		}
		// A quote is the item's content, not an indented child block:
		// Typora reports the quote's paragraphs as list items with the
		// item's own style and the quote's bar.
		childUnder := under
		if b.Kind == parser.Quote || b.Kind == parser.Paragraph || b.Kind == parser.Heading {
			// A paragraph or heading in the item is the item's content:
			// Typora reports it with the item's list, depth and check box,
			// as it does the item's first paragraph.
			childUnder = ist
		}
		if first {
			first = false
			if b.Kind == parser.Paragraph || b.Kind == parser.Heading ||
				b.Kind == parser.ThematicBreak && b.Start <= m.lineEnd(m.d.Block(i).Start) {
				m.block(k, ist, depth)
				prev = k
				continue
			}
			// An item whose first child starts on the item's own line has
			// no empty line of its own: a quote marker or a nested list's
			// marker takes the line, and the line's source stops where the
			// child starts. A child on a later line keeps the empty line.
			if b.Kind == parser.Quote ||
				b.Kind == parser.List && b.Start <= m.lineEnd(m.d.Block(i).Start) {
				m.block(k, childUnder, depth)
				prev = k
				continue
			}
			if b.Kind == parser.CodeBlock && ist.List == Numbers &&
				b.Start <= m.lineEnd(m.d.Block(i).Start) {
				// A fence that is an ordered item's whole content is the
				// item's block, not a block nested under an empty item
				// paragraph: Typora reports one code block whose source starts
				// at the item's marker, and no item block of its own (record
				// 2026-11-12, seed20/c3).
				m.block(k, ParagraphStyle{Indent: ist.Indent, Quote: ist.Quote}, depth)
				prev = k
				continue
			}
			at := min(m.lineEnd(m.d.Block(i).Start), b.Start)
			m.add(Paragraph{Style: ist}, m.opaque(at, at, 0))
		}
		m.block(k, childUnder, depth)
		prev = k
	}
	if first {
		at := m.lineEnd(m.d.Block(i).Start)
		if n := m.emptyItemBreak(at); n > 0 {
			// An empty item followed by its continuation line holds a soft
			// break: Typora shows the line below the marker, the caret on
			// it after Shift+Enter (record 2026-11-10, seed4/c17).
			m.reset(at)
			m.seg(segmentBreak, 1, n)
			m.addTracked(Paragraph{Style: ist, Runs: []Run{{Text: string(LineSeparator)}}})
			return
		}
		m.add(Paragraph{Style: ist}, m.opaque(at, at, 0))
	}
}

// codeEmptyBodyLine projects a fenced block's empty body line. A fence that
// opens on a list item's own line carries the item marker as its prefix, so
// the empty line Enter leaves stands at the column that marker indents the
// body to: the caret rests there and typed text lands there (record
// 2026-11-12, seed20/c3). Any other empty block keeps the line after the
// closing fence, which is where its caret writes.
func (m *mdReader) codeEmptyBodyLine(i int, st ParagraphStyle) builtPara {
	b := m.d.Block(i)
	if !b.Open && st.List == NoList {
		open := lineAt(m.src, b.Start)
		if closer, ok := m.listMarkerFenceCloser(b, open); ok {
			at := closer.start
			for at < closer.end && (m.src[at] == ' ' || m.src[at] == '\t') {
				at++
			}
			return builtPara{start: at, end: at, m: paraMap{src: 0, codeEmpty: true, owner: m.owner}}
		}
	}
	return m.codeEmpty(i)
}

// listMarkerFenceCloser returns the closing fence line of the fenced block b
// when its opening line holds a list marker in front of the fence and the
// block holds no body line: the block stands where an item's own line put it,
// and the closing fence sits at the column that marker indents to.
func (m *mdReader) listMarkerFenceCloser(b *parser.Block, open markdownLine) (markdownLine, bool) {
	if !fenceAfterListMarker(m.src[open.start:open.end]) || open.eol == "" {
		return markdownLine{}, false
	}
	closer := lineAt(m.src, open.stop)
	if closer.end != b.End {
		return markdownLine{}, false
	}
	text := strings.TrimSpace(m.src[closer.start:closer.end])
	if !strings.HasPrefix(text, "```") && !strings.HasPrefix(text, "~~~") {
		return markdownLine{}, false
	}
	return closer, true
}

// fenceAfterListMarker reports whether line holds nothing but an ordered list
// marker followed by a fence: the opening line of a fence typed on an ordered
// item's own line, which Typora reports as the block's own source (record
// 2026-11-12, seed20/c3). A bullet item's fence keeps the nested shape its
// item's empty paragraph needs (record 2026-11-12, seed11/c11).
func fenceAfterListMarker(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != '.' && line[i] != ')' {
		return false
	}
	i++
	if i >= len(line) || line[i] != ' ' {
		return false
	}
	rest := strings.TrimSpace(line[i:])
	return strings.HasPrefix(rest, "```") || strings.HasPrefix(rest, "~~~")
}

// emptyItemBreak returns the bytes of the break an empty item's continuation
// line holds after line end at: the line ending and the line's indent, which
// the parser drops when the line has no content.
func (m *mdReader) emptyItemBreak(at int) int {
	if at >= len(m.src) {
		return 0
	}
	eol := 0
	switch {
	case m.src[at] == '\n':
		eol = 1
	case m.src[at] == '\r':
		eol = 1
		if at+1 < len(m.src) && m.src[at+1] == '\n' {
			eol = 2
		}
	default:
		return 0
	}
	line := m.src[at+eol:]
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	if line == "" || strings.Trim(line, " \t") != "" {
		return 0
	}
	return eol + len(line)
}

// listGapParagraph adds the empty block Typora shows between two items of one
// list. Blank lines alone keep the list for two lines and split it for three
// or more, leaving one paragraph at the first blank line, however many blank
// lines follow it: `- a\n- b\n\n\n\n- c` holds one paragraph at 9:9 (record
// 2026-11-10, newops/cold-blank-list). A whitespace line is the item's own
// continuation where it directly follows the item's content, and the next one
// is an empty item: `- b\n  \n  \n  ` shows it at the second spaced line with
// the spaces as its prefix (record 2026-11-10, newops/cold-gap-spaced). A
// whitespace line after a blank line has no item line to continue and shows
// there: `- alpha\n\n  \n- beta` holds an empty item at 9:11 (record
// 2026-11-10, exitprobe/bullet-cmdenter-line-end).
func (m *mdReader) listGapParagraph(prevEnd, next int, st, item ParagraphStyle) bool {
	if prevEnd < 0 || prevEnd >= next {
		return false
	}
	// The next item's line starts its own block: its indentation is not gap.
	nextLine := next
	for nextLine > prevEnd && m.src[nextLine-1] != '\n' && m.src[nextLine-1] != '\r' {
		nextLine--
	}
	if nextLine <= prevEnd {
		return false
	}
	gap := m.src[prevEnd:nextLine]
	if strings.Trim(gap, "\r\n") == "" {
		n := 0
		for i := 0; i < len(gap); i++ {
			if gap[i] == '\r' {
				n++
				if i+1 < len(gap) && gap[i+1] == '\n' {
					i++
				}
			} else if gap[i] == '\n' {
				n++
			}
		}
		if n < 4 {
			return false
		}
		at := prevEnd
		if at < nextLine && m.src[at] == '\r' {
			at++
		}
		if at < nextLine && m.src[at] == '\n' {
			at++
		}
		m.reset(at)
		m.add(Paragraph{Style: ParagraphStyle{Quote: st.Quote}}, builtPara{start: at, end: at})
		return true
	}
	// Lines of the gap after the previous item's own line.
	at := prevEnd
	for at < nextLine && m.src[at] != '\n' && m.src[at] != '\r' {
		at++
	}
	if at >= nextLine {
		return false
	}
	if m.src[at] == '\r' && at+1 < nextLine && m.src[at+1] == '\n' {
		at += 2
	} else {
		at++
	}
	quoted, spaced := -1, -1
	first := true
	for at < nextLine {
		end := at
		for end < nextLine && m.src[end] != '\n' && m.src[end] != '\r' {
			end++
		}
		text := m.src[at:end]
		switch {
		case gapQuoteLine(text):
			if quoted < 0 {
				quoted = at
			}
		case text != "" && strings.Trim(text, " \t") == "":
			// The first continuation line after the item's content is the
			// item's own, not an empty block (record 2026-11-10,
			// newops/cold-gap-spaced).
			if !first && spaced < 0 {
				spaced = at
			}
		}
		first = false
		at = end
		if at < nextLine {
			if m.src[at] == '\r' && at+1 < nextLine && m.src[at+1] == '\n' {
				at += 2
			} else {
				at++
			}
		}
	}
	if quoted >= 0 {
		// Quote-only lines are the quote's own blank line and become one
		// empty paragraph of it: `> - b\n> \n> \n> \n> - c` shows one at
		// 19:24 with the `> ` prefix (record 2026-11-10,
		// newops/cold-gap-quote). The paragraph starts after the markers,
		// which its snapshot pulls back in as the prefix.
		at := quoted
		for at < nextLine && (m.src[at] == ' ' || m.src[at] == '>') {
			at++
		}
		m.reset(at)
		m.add(Paragraph{Style: ParagraphStyle{Quote: st.Quote}},
			builtPara{start: at, end: at, m: paraMap{owner: m.owner}})
		return false
	}
	if spaced < 0 {
		return false
	}
	end := spaced
	for end < nextLine && m.src[end] != '\n' && m.src[end] != '\r' {
		end++
	}
	item.Checked = false
	m.reset(spaced)
	m.add(Paragraph{Style: item}, builtPara{
		start: spaced, end: end,
		m: paraMap{segs: []Segment{{Kind: SegmentGap, Source: end - spaced}}, src: end - spaced},
	})
	return false
}

// gapQuoteLine reports whether a line is a quote marker with nothing else: the
// blank line of a quote.
func gapQuoteLine(line string) bool {
	bar := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ', '\t':
		case '>':
			bar = true
		default:
			return false
		}
	}
	return bar
}

// itemBlankParagraph adds the empty paragraph an item's continuation line
// holds between two of the item's blocks. The line must be whitespace alone
// and as wide as the item's content column, and the line before it must be
// empty: Typora writes a loose list's separator as an indented whitespace
// line directly after the item's content, and that line is not a paragraph.
func (m *mdReader) itemBlankParagraph(from, to int, st ParagraphStyle, col int) {
	if col <= 0 || st.Quote || from >= to {
		return
	}
	// The gap starts at the previous block's content, inside its last line;
	// skip the rest of that line before reading whole lines.
	for from < to && m.src[from] != '\n' && m.src[from] != '\r' {
		from++
	}
	if from >= to {
		return
	}
	if m.src[from] == '\r' && from+1 < to && m.src[from+1] == '\n' {
		from++
	}
	from++
	empty := false
	for at := from; at < to; {
		i := at
		for i < to && m.src[i] != '\n' && m.src[i] != '\r' {
			i++
		}
		if i >= to {
			return
		}
		blank := i-at == col && strings.Trim(m.src[at:i], " \t") == ""
		if blank && empty {
			m.reset(at)
			m.add(Paragraph{Style: st}, builtPara{
				start: at, end: i,
				m: paraMap{segs: []Segment{{Kind: SegmentGap, Source: i - at}}, src: i - at},
			})
			return
		}
		empty = i == at
		if m.src[i] == '\r' && i+1 < to && m.src[i+1] == '\n' {
			i++
		}
		at = i + 1
	}
}

// inlinePara adds a paragraph of inlines in style st: one Paragraph however
// many lines it has, line breaks within it apart.
func (m *mdReader) inlinePara(st ParagraphStyle, i int) {
	in := m.d.Inlines(i)
	start := m.contentStart(i, in)
	m.reset(start)
	m.prefix = m.contentPrefix(i)
	var lead []Run
	if n := m.leadingBreak(i, start, m.prefix); n > 0 {
		breakStart := start - n
		ownLine := m.prefix > 0 || m.lineStart(breakStart) == breakStart
		live := m.d.IsLiveLine(breakStart)
		switch {
		case ownLine || !live:
			m.start -= n
			m.pos -= n
			if live {
				m.seg(segmentBreak, 1, n)
				lead = addMDRun(lead, Run{Text: string(LineSeparator)})
			} else {
				// The same bytes reopened from a file show the paragraph on the
				// line its text starts: the empty first line and the prefix hold
				// no rune, but the paragraph's source still spans them.
				m.seg(SegmentGap, 0, n)
			}
		default:
			// The break ends a line that holds another block's bytes, as a
			// list marker line directly above the paragraph does: the line
			// ending is that block's separator, not this paragraph's empty
			// first line. The session-typed break still shows, with no bytes
			// of its own in this paragraph's source span.
			m.seg(segmentBreak, 1, 0)
			lead = addMDRun(lead, Run{Text: string(LineSeparator)})
		}
	}
	runs := append(lead, m.runs(in, Attributes{})...)
	m.prefix = 0
	if m.on {
		if e := m.lineEnd(m.pos); e > m.pos && strings.Trim(m.src[m.pos:e], " \t") == "" {
			runs = addMDRun(runs, Run{Text: m.src[m.pos:e]})
			m.text(m.pos, e, m.src[m.pos:e])
		}
	}
	m.addTracked(Paragraph{Style: st, Runs: runs})
}

// contentPrefix returns the bytes before the content of block i on its first
// line, which its continuation lines repeat: the markers of quotes and the
// indents of lists.
func (m *mdReader) contentPrefix(i int) int {
	start := m.d.Block(i).Start
	return start - m.lineStart(start)
}

// lineStart returns the offset of the line start at or before at.
func (m *mdReader) lineStart(at int) int {
	for at > 0 && m.src[at-1] != '\n' && m.src[at-1] != '\r' {
		at--
	}
	return at
}

// leadingBreak returns the bytes of the line break that starts a paragraph
// whose first line is empty, and 0 when it starts on the first line it holds.
// A paragraph in an item or quote that starts on a continuation line, and the
// document's first paragraph after a blank first line, begin with the newline
// ending that first line and the prefix the continuation line repeats; the
// break is the paragraph's first inline, as Typora shows it. A line break the
// session typed opens a paragraph's first line wherever that paragraph sits,
// so it counts as a leading break outside those two positions too. Only the
// editor projection shows it: ParseMarkdown, which converts content, leaves
// the empty first line out.
func (m *mdReader) leadingBreak(i, start, prefix int) int {
	if !m.on || start <= 0 {
		return 0
	}
	at := start - prefix
	if at <= 0 {
		return 0
	}
	eol := 0
	switch {
	case m.src[at-1] == '\n':
		eol = 1
		if at >= 2 && m.src[at-2] == '\r' {
			eol = 2
		}
	case m.src[at-1] == '\r':
		eol = 1
	}
	if eol == 0 {
		return 0
	}
	breakStart := at - eol
	switch parent := m.d.Block(i).Parent; {
	case parent > 0 && (m.d.Block(parent).Kind == parser.Item || m.d.Block(parent).Kind == parser.Quote):
		if breakStart != m.lineEnd(m.d.Block(parent).Start) {
			return 0
		}
	case parent <= 0:
		first := -1
		for c := range m.d.Children(0) {
			first = c
			break
		}
		if !(first == i && breakStart == m.lineEnd(0)) && !m.d.IsLiveLine(breakStart) {
			return 0
		}
	default:
		return 0
	}
	return eol + prefix
}

// continuationPrefix returns the bytes a continuation line of block i
// repeats: the prefix of its first line with the list marker replaced by
// spaces, as "- > alpha" continues as "  > alpha".
func (m *mdReader) continuationPrefix(i int) string {
	b := m.d.Block(i)
	prefix := []byte(m.src[m.lineStart(b.Start):b.Start])
	for j, c := range prefix {
		if c != '>' && c != ' ' && c != '\t' {
			prefix[j] = ' '
		}
	}
	return string(prefix)
}

// runs returns the runs of inlines, in attributes a; line breaks within
// the paragraph are LineSeparators.
func (m *mdReader) runs(in []parser.Inline, a Attributes) []Run {
	var out []Run
	m.html = [4]int{}
	m.inlines(&out, in, 0, len(in), a)
	return out
}

func (m *mdReader) inlines(out *[]Run, in []parser.Inline, from, to int, a Attributes) {
	add := func(s string, a Attributes) {
		// A newline in a run, as an entity or an inline HTML tag holds it,
		// is a line break within the paragraph, which shows as a
		// LineSeparator, not as a paragraph break.
		s = strings.ReplaceAll(s, "\n", string(LineSeparator))
		if m.html[0] > 0 {
			a.Underline = true
		}
		if m.html[1] > 0 {
			a.Strikethrough = true
		}
		if m.html[2] > 0 {
			a.Weight = 700
		}
		if m.html[3] > 0 {
			a.Italic = true
		}
		*out = addMDRun(*out, Run{Text: s, Attributes: a})
	}
	for k := from; k < to; k = in[k].Next {
		n := &in[k]
		original := n
		x := a
		if (n.Kind == parser.SoftBreak || n.Kind == parser.LineBreak) && m.on && n.Start > m.pos && strings.Trim(m.src[m.pos:n.Start], " \t") == "" {
			if n.Kind == parser.SoftBreak {
				add(m.src[m.pos:n.Start], a)
				m.text(m.pos, n.Start, m.src[m.pos:n.Start])
			} else {
				n = &parser.Inline{Kind: n.Kind, Start: m.pos, End: n.End}
			}
		}
		m.gapTo(n.Start)
		switch n.Kind {
		case parser.Text:
			add(n.Text, a)
			m.text(n.Start, n.End, n.Text)
		case parser.SoftBreak:
			add(string(LineSeparator)+m.lineBreakSource(n), a)
		case parser.LineBreak:
			m.hardBreak(original)
			add(string(LineSeparator)+m.lineBreakSource(n), a)
		case parser.Code:
			x.Code = true
			if m.on && hiddenCodeSpan(m.src[n.Start:n.End], n.Text) {
				// The span draws nothing of its own, so its content rune
				// is invisible; the caret still needs a position inside
				// it (record 2026-11-09, diff.json ac6ec2fb).
				add(strings.Repeat("\u200b", utf8.RuneCountInString(n.Text)), x)
			} else {
				add(n.Text, x)
			}
			m.code(n)
		case parser.Emphasis:
			x.Italic = true
			m.container(out, in, k, x)
		case parser.Strong:
			x.Weight = 700
			m.container(out, in, k, x)
		case parser.Strikethrough:
			x.Strikethrough = true
			m.container(out, in, k, x)
		case parser.Link:
			x.Link = safeLink(n.Dest)
			m.container(out, in, k, x)
			m.auto = m.auto || n.Auto && m.on && m.src[n.Start] != '<'
		case parser.Image:
			if m.on {
				add(m.src[n.Start:n.End], a)
				m.verbatim(utf8.RuneCountInString(m.src[n.Start:n.End]), n.End-n.Start)
				break
			}
			x = a.extending()
			x.Attachment = &Attachment{Name: parser.PlainText(in[k+1 : n.Next]), URL: safeLink(n.Dest)}
			*out = append(*out, Run{Text: string(ObjectReplacement), Attributes: x})
			m.object(1, n.End-n.Start)
		case parser.HTML:
			tag := m.tag(n.Text, out, a)
			if tag.breakTag {
				m.seg(segmentBreak, 1, n.End-n.Start)
			} else if m.on && !tag.valid {
				add(m.src[n.Start:n.End], a)
				m.verbatim(utf8.RuneCountInString(m.src[n.Start:n.End]), n.End-n.Start)
			}
		case parser.FootnoteRef:
			if m.on {
				add(m.src[n.Start:n.End], a)
				m.verbatim(utf8.RuneCountInString(m.src[n.Start:n.End]), n.End-n.Start)
				break
			}
			s := "[" + n.Text + "]"
			add(s, a)
			m.object(utf8.RuneCountInString(s), n.End-n.Start)
		case parser.Math:
			add(m.src[n.Start:n.End], a)
			m.verbatim(utf8.RuneCountInString(m.src[n.Start:n.End]), n.End-n.Start)
		}
		m.gapTo(n.End)
		if n.Kind == parser.HTML && m.on && m.htmlTag.valid {
			m.trackHTML(n, m.htmlTag)
		}
	}
}

// lineBreakSource adds the source of line break n: the newline and the
// prefix its next line repeats are the break, which shows as one
// LineSeparator; whitespace past the prefix shows as it is, as Typora shows
// it. It returns that whitespace.
func (m *mdReader) lineBreakSource(n *parser.Inline) string {
	span := m.src[n.Start:n.End]
	nl := strings.IndexByte(span, '\n')
	rest := ""
	if nl >= 0 {
		if tail := span[nl+1:]; len(tail) > m.prefix && strings.Trim(tail[m.prefix:], " \t") == "" {
			rest = tail[m.prefix:]
		}
	}
	m.seg(segmentBreak, 1, len(span)-len(rest))
	if rest != "" {
		m.verbatim(len(rest), len(rest))
	}
	return rest
}

func (m *mdReader) container(out *[]Run, in []parser.Inline, k int, x Attributes) {
	n := &in[k]
	if !m.on {
		m.inlines(out, in, k+1, n.Next, x)
		return
	}
	open := -1
	if k+1 < n.Next {
		if in[k+1].Start > m.pos {
			open = len(m.segs)
		}
		m.gapTo(in[k+1].Start)
	}
	m.inlines(out, in, k+1, n.Next, x)
	close := -1
	if n.End > m.pos {
		close = len(m.segs)
	}
	m.gapTo(n.End)
	if open >= 0 || close >= 0 {
		kind := MarkerLink
		switch n.Kind {
		case parser.Emphasis:
			kind = MarkerEmphasis
		case parser.Strong:
			kind = MarkerStrong
		case parser.Strikethrough:
			kind = MarkerStrikethrough
		}
		if n.Kind == parser.Link && n.Auto && n.Start < n.End && m.src[n.Start] != '<' {
			return
		}
		box := box{
			open: open, close: close, kind: kind,
			start: n.Start, end: n.End,
			openStart: n.Start, closeEnd: n.End,
		}
		if k+1 < n.Next {
			box.openEnd = in[k+1].Start
			box.contentFrom = in[k+1].Start
			// The content ends with the last direct child, not with the
			// last of its descendants: nested markers close at the child
			// boundary, not inside the outer element's content.
			end := in[k+1].End
			for j := in[k+1].Next; j < n.Next; j = in[j].Next {
				end = max(end, in[j].End)
			}
			box.contentTo = end
		} else {
			box.openEnd = n.Start
			box.contentFrom = n.Start
			box.contentTo = n.Start
			if n.Kind == parser.Link && n.End > n.Start+1 {
				// An empty label: `[` opens the element, and the rest is
				// its destination and closing bracket. Both are markers,
				// drawn at the element's one gap segment.
				box.openEnd = n.Start + 1
				box.contentFrom = n.Start + 1
				box.contentTo = n.Start + 1
				box.open = close
			}
		}
		box.closeStart = box.contentTo
		m.boxes = append(m.boxes, box)
	}
}

func (m *mdReader) trackHTML(n *parser.Inline, tag htmlTag) {
	if tag.breakTag || tag.valid == false || tag.which < 0 {
		return
	}
	if !tag.closing {
		m.htmlOpen[tag.which] = append(m.htmlOpen[tag.which], len(m.segs)-1)
		return
	}
	stack := m.htmlOpen[tag.which]
	if len(stack) == 0 {
		return
	}
	open := stack[len(stack)-1]
	m.htmlOpen[tag.which] = stack[:len(stack)-1]
	start := m.segSource(open)
	m.boxes = append(m.boxes, box{
		open: open, close: len(m.segs) - 1, kind: MarkerHTML,
		start: start, end: n.End,
		openStart: start, openEnd: start + m.segs[open].Source,
		contentFrom: start + m.segs[open].Source, contentTo: n.Start,
		closeStart: n.Start, closeEnd: n.End,
	})
}

func (m *mdReader) segSource(i int) int {
	at := m.start
	for k := 0; k < i; k++ {
		at += m.segs[k].Source
	}
	return at
}

func (m *mdReader) hardBreak(n *parser.Inline) {
	if m.src[n.Start] == '\\' {
		m.boxes = append(m.boxes, box{
			open: -1, close: -1, kind: MarkerHardBreakBackslash,
			start: n.Start, end: n.Start + 1,
			openStart: n.Start, openEnd: n.Start + 1,
			contentFrom: n.Start + 1, contentTo: n.Start + 1,
			closeStart: n.Start + 1, closeEnd: n.Start + 1,
		})
		return
	}
	at := n.Start
	for at > 0 && m.src[at-1] == ' ' {
		at--
	}
	if n.Start-at < 2 {
		return
	}
	m.boxes = append(m.boxes, box{
		open: -1, close: -1, kind: MarkerHardBreakSpace,
		start: at, end: n.Start,
		openStart: at, openEnd: n.Start,
		contentFrom: n.Start, contentTo: n.Start,
		closeStart: n.Start, closeEnd: n.Start,
	})
}

// tag applies an inline HTML tag: the opening and closing tags of
// underlines, strikethrough, bold and italic, and line breaks. Other
// tags are left out, as a browser shows none.
func (m *mdReader) tag(s string, out *[]Run, a Attributes) htmlTag {
	tag := m.tagKind(s, out, a)
	m.htmlTag = tag
	return tag
}

type htmlTag struct {
	valid    bool
	closing  bool
	which    int
	breakTag bool
}

func (m *mdReader) tagKind(s string, out *[]Run, a Attributes) htmlTag {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") {
		return htmlTag{}
	}
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "<"), ">"))
	closing := strings.HasPrefix(s, "/")
	s = strings.TrimPrefix(s, "/")
	s = strings.TrimSuffix(s, "/")
	name, _, _ := strings.Cut(strings.TrimSpace(s), " ")
	which := -1
	switch name {
	case "br":
		*out = append(*out, Run{Text: string(LineSeparator), Attributes: a})
		return htmlTag{valid: true, breakTag: true}
	case "u", "ins":
		which = 0
	case "s", "strike", "del":
		which = 1
	case "b", "strong":
		which = 2
	case "i", "em":
		which = 3
	}
	if which >= 0 {
		if closing {
			m.html[which] = max(m.html[which]-1, 0)
		} else {
			m.html[which]++
		}
	}
	return htmlTag{valid: which >= 0, closing: closing, which: which}
}

// addMDRun appends r to runs, joined to the last when their attributes
// are the same.
func addMDRun(runs []Run, r Run) []Run {
	if n := len(runs); n > 0 && runs[n-1].Attributes == r.Attributes && r.Attributes.Attachment == nil {
		runs[n-1].Text += r.Text
		return runs
	}
	return append(runs, r)
}

// quoteContentAt returns the offset of the innermost quote content on the
// line starting at start: the bytes after its quote markers and spaces.
func (m *mdReader) quoteContentAt(start int) int {
	line := lineAt(m.src, start)
	at := start
	for at < line.end && (m.src[at] == ' ' || m.src[at] == '>') {
		at++
	}
	return at
}

// quoteGapBlank reports whether the lines between from and to hold a bare
// quote marker line, which separates two paragraphs of the quote.
func quoteGapBlank(src string, from, to int) bool {
	for at := from; at < to; {
		line := lineAt(src, at)
		if strings.TrimSpace(src[line.start:line.end]) == ">" {
			return true
		}
		if line.stop <= at {
			return false
		}
		at = line.stop
	}
	return false
}
