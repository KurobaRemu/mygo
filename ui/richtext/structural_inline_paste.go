package richtext

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

func (e *Edit) deleteCrossBlock(start, end int, insert string) error {
	if start == end && insert == "" {
		return nil
	}
	t := e.d.text
	pa, pz := t.paraAt(start), t.paraAt(end)
	src := e.d.md.p.String()
	if start == 0 && end == t.Len() && insert == "" {
		// The document offsets covering every paragraph do not mean the
		// selection covers every byte: an empty list item's marker is hidden
		// syntax, so a selection over its empty content spans the whole
		// document while the marker's bytes stay outside it. Only a selection
		// whose source points span the source takes the whole-document
		// shortcut; anything narrower falls through and keeps the marker
		// (record 2026-11-12, seed17/c1, seed17/c14).
		a, z := e.d.selection.Anchor.Offset, e.d.selection.Caret.Offset
		if a > z {
			a, z = z, a
		}
		narrow := a != z && (a > 0 || z < len(src))
		if !narrow {
			eol := defaultEOL(src)
			if !e.replaceMarkdownSource(0, len(src), eol) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.Select(0, 0)
			}
			return nil
		}
	}
	from := e.d.sourceAtParagraph(pa, start-t.paraStart(pa))
	to := e.d.sourceAtParagraph(pz, end-t.paraStart(pz))
	if pa == pz {
		if from > to {
			from, to = to, from
		}
		if strings.ContainsAny(insert, "\r\n") {
			return e.pasteMarkdown(start, end, insert)
		} else if !e.replaceMarkdownSource(from, to, insert) {
			return ErrStructuralEdit
		} else {
			if e.step != nil {
				e.Select(start+utf8.RuneCountInString(insert), start+utf8.RuneCountInString(insert))
			}
			return nil
		}
	}
	last := e.d.paragraphLocation(pz)
	to = min(to, last.contentEnd)
	from = min(from, to)
	delTo := last.contentEnd
	if insert == "" && from < to && last.contentStart == last.contentEnd &&
		pz+1 < t.ParagraphCount() && src[from:to] == lineAt(src, from).eol {
		// Backspace at the start of an empty paragraph joins it into the
		// block above, and the empty block leaves with the blank line it
		// stood on: its own line ending goes along with the break above it.
		// A paragraph whose bytes already hold a blank line above keeps that
		// line, and it is the separator the join leaves, so only a selection
		// that spans the break alone takes the line ending too (record
		// 2026-11-13, seed17/c2).
		delTo = last.line.stop
	}
	tail := src[to:last.contentEnd]
	middle := src[from:to]
	refs := referenceDefinitionLines(middle)
	body := insert + tail
	if refs != "" {
		eol := defaultEOL(src)
		if body != "" && !strings.HasSuffix(body, "\n") && !strings.HasSuffix(body, "\r") {
			body += eol + eol
		}
		body += refs
	}
	if !e.replaceMarkdownSource(from, delTo, body) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		at := e.d.docAtSource(from + len(insert))
		if insert == "" && body == "" && last.contentStart == last.contentEnd &&
			pz+1 == t.ParagraphCount() && blankBytes(src[from:to]) {
			// The join takes the document's last, empty paragraph and leaves
			// the blank line above it as the paragraph it spells. The caret
			// stands on that paragraph, not at the end of the text above it,
			// so the next Backspace takes the line ending rather than a
			// character (record 2026-11-13, seed18/c4).
			at = e.d.docAtSource(delTo)
		}
		e.Select(at, at)
	}
	return nil
}

func referenceDefinitionLines(s string) string {
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, line := range lines {
		trim := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
		if strings.HasPrefix(trim, "[") {
			if end := strings.Index(trim, "]:"); end > 0 {
				b.WriteString(trim)
				b.WriteByte('\n')
				continue
			}
		}
	}
	return b.String()
}

// pasteSourceRange returns the source bytes a paste replaces when the
// document offsets collapse to one point. A selection over hidden delimiter
// bytes alone has one document offset at both ends; only the source
// selection says which bytes it spans (record 2026-11-10, seed9/c5).
func (d *Document) pasteSourceRange(from, to int) (int, int, bool) {
	if from != to || from != d.anchor || from != d.caret {
		return 0, 0, false
	}
	a, z := d.selection.Anchor.Offset, d.selection.Caret.Offset
	if a == z {
		return 0, 0, false
	}
	if a > z {
		a, z = z, a
	}
	return a, z, true
}

func (e *Edit) pasteMarkdown(from, to int, md string) error {
	if md == "" {
		return e.deleteCrossBlock(from, to, "")
	}
	t := e.d.text
	p := t.paraAt(from)
	loc := e.d.paragraphLocation(p)
	point := e.d.sourceAtDoc(from)
	cont := e.d.continuationPrefix(loc)
	src := e.d.md.p.String()
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")
	if cont != "" {
		quoteBlank := ""
		if strings.Contains(cont, ">") {
			quoteBlank = strings.TrimRight(cont, " \t")
		}
		lines := strings.Split(md, "\n")
		for i := 1; i < len(lines); i++ {
			if lines[i] != "" {
				lines[i] = cont + lines[i]
			} else if quoteBlank != "" && i < len(lines)-1 {
				lines[i] = quoteBlank
			}
		}
		md = strings.Join(lines, "\n")
	}
	if eol := defaultEOL(src); eol != "\n" {
		md = strings.ReplaceAll(md, "\n", eol)
	}
	if to == from {
		if a, z, ok := e.d.pasteSourceRange(from, to); ok {
			if !e.replaceMarkdownSource(a, z, md) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				e.selectMarkdownSource(a + len(md))
			}
			return nil
		}
		if !e.replaceMarkdownSource(point, point, e.d.gapInsert(point, md)) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			caret := point + len(md)
			if !strings.Contains(md, "\n") {
				caret = e.d.pastInlineClose(p, caret)
			}
			e.selectMarkdownSource(caret)
		}
		return nil
	}
	fromSrc := point
	toSrc := e.d.sourceAtDoc(to)
	if fromSrc > toSrc {
		fromSrc, toSrc = toSrc, fromSrc
	}
	if !e.replaceMarkdownSource(fromSrc, toSrc, md) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		// The caret ends where the pasted bytes do, in source coordinates:
		// mapping that point to the display and back stops in front of the
		// inline syntax the paste left at the paragraph's end (record
		// 2026-11-13, seed20/c12).
		e.selectMarkdownSource(fromSrc + len(md))
	}
	return nil
}

func (e *Edit) pastePlain(from, to int, s string) error {
	if s == "" {
		return e.deleteCrossBlock(from, to, "")
	}
	s = normalizeNewlines(s)
	if e.d.text.ParagraphStyleAt(from).CodeBlock && e.d.text.paraAt(from) == e.d.text.paraAt(to) {
		at, end := e.d.sourceAtDoc(from), e.d.sourceAtDoc(to)
		if loc := e.d.paragraphLocation(e.d.text.paraAt(from)); loc.leaf >= 0 {
			src := e.d.md.p.String()
			b := e.d.md.p.Block(loc.leaf)
			s = strings.ReplaceAll(s, "\n", defaultEOL(src)+src[lineAt(src, b.Start).start:b.Start])
		}
		if !e.replaceMarkdownSource(at, end, s) {
			return ErrStructuralEdit
		}
		if e.step != nil {
			e.selectMarkdownSource(at + len(s))
		}
		return nil
	}
	if strings.Contains(s, "\n") {
		return e.pasteMarkdown(from, to, strings.ReplaceAll(s, "\n", "\n\n"))
	}
	at, end := e.d.sourceAtDoc(from), e.d.sourceAtDoc(to)
	if a, z, ok := e.d.pasteSourceRange(from, to); ok {
		at, end = a, z
	}
	if at == end && from != e.d.caret {
		at = e.d.splitOutside(e.d.text.paraAt(from), at)
		end = at
	}
	insert := s
	if at == end {
		insert = e.d.gapInsert(at, s)
	}
	if !e.replaceMarkdownSource(at, end, insert) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		caret := at + len(s)
		if !strings.Contains(s, "\n") {
			caret = e.d.pastInlineClose(e.d.text.paraAt(from), caret)
		}
		e.selectMarkdownSource(caret)
	}
	return nil
}

// pastInlineClose moves a source caret past a run of closing markers that
// the paste merged. A paste at the content end of nested inline elements
// leaves the caret where the inner element's closing run meets the outer
// element's: inside the one run the two close bytes form, a position
// Typora has no caret for, so it leaves the caret after the run (record
// 2026-11-09, seed5/c5). A paste at one element's content end, with the
// caret ahead of that element's closing run alone, keeps the caret at the
// content end (record 2026-11-12, probe102/cold-strong-paste-plain,
// /cold-strong-paste-markdown, /cold-italic-paste-plain,
// /live-italic-paste-markdown).
func (d *Document) pastInlineClose(p, at int) int {
	if d.md == nil || p < 0 || p >= d.md.m.Len() {
		return at
	}
	for {
		boxes := d.md.m.boxes(p)
		if !insideCloseRun(boxes, at) {
			return at
		}
		end := at
		for _, b := range boxes {
			if b.closeStart == at && b.closeEnd > at {
				end = max(end, b.closeEnd)
			}
		}
		at = end
	}
}

// insideCloseRun reports whether at falls strictly inside a run of closing
// markers: a closing run ends exactly there and another starts there. The
// two runs are adjacent in the source bytes, so the caret between them has
// no position to stand at.
func insideCloseRun(boxes []box, at int) bool {
	ends, starts := false, false
	for _, b := range boxes {
		if b.closeEnd <= b.closeStart {
			continue
		}
		ends = ends || b.closeEnd == at
		starts = starts || b.closeStart == at
	}
	return ends && starts
}

func (e *Edit) formatMarkdownParagraphs(start, end int, want Text) error {
	t := e.d.text
	pa, pz := t.paragraphSpan(start, end)
	for p := pz; p >= pa; p-- {
		oldPara := e.d.text.paragraph(p)
		newPara := want.paragraph(p)
		if oldPara.Style == newPara.Style {
			continue
		}
		from, to, repl, err := e.markdownBlockReplacement(p, oldPara, newPara)
		if err != nil {
			return err
		}
		if !e.replaceMarkdownSource(from, to, repl) {
			return ErrStructuralEdit
		}
	}
	return nil
}

func (e *Edit) markdownBlockReplacement(p int, oldPara, newPara Paragraph) (int, int, string, error) {
	d := e.d
	loc := d.paragraphLocation(p)
	if loc.leaf < 0 {
		return 0, 0, "", ErrStructuralEdit
	}
	src := d.md.p.String()
	b := d.md.p.Block(loc.leaf)
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	old := oldPara.Style
	next := newPara.Style
	content := src[loc.contentStart:loc.contentEnd]
	quote := quotePrefix(loc.prefix)
	if old.CodeBlock && !next.CodeBlock {
		body := codeBlockBody(src, b)
		repl := body
		if repl != "" && !strings.HasSuffix(repl, "\n") && !strings.HasSuffix(repl, "\r") {
			repl += eol
		}
		return b.Start, b.End, repl, nil
	}
	if next.CodeBlock && !old.CodeBlock {
		fence := strings.Repeat("`", 3)
		for strings.Contains(content, fence) {
			fence += "`"
		}
		repl := quote + fence + eol
		if content != "" {
			repl += quote + content + eol
		}
		repl += quote + fence + eol
		return loc.line.start, loc.line.stop, repl, nil
	}
	if old.Heading > 0 && next.Heading > 0 && b.Setext && next.Heading <= 2 {
		mark := "="
		if next.Heading == 2 {
			mark = "-"
		}
		line := src[loc.line.start:loc.line.end]
		underline := lineAt(src, loc.line.stop)
		if underline.start == loc.line.stop {
			line = src[loc.line.start:loc.line.end]
		}
		repl := line + eol + quote + strings.Repeat(mark, max(3, utf8.RuneCountInString(content))) + eol
		return loc.line.start, underline.stop, repl, nil
	}
	if next.Heading > 0 {
		prefix := quote + strings.Repeat("#", next.Heading) + " "
		closing := ""
		if old.Heading > 0 {
			closing = headingClosing(loc.suffix)
		}
		repl := prefix + content + closing + eol
		if b.Setext {
			to := loc.line.stop
			if underline := lineAt(src, loc.line.stop); underline.start == loc.line.stop {
				to = underline.stop
			}
			return loc.line.start, to, repl, nil
		}
		return loc.line.start, loc.line.stop, repl, nil
	}
	if next.List != NoList {
		container, indent, marker := listStylePrefix(src, d.md.p, loc, old, next)
		_ = container
		repl := quote + indent + marker + content + eol
		if b.Setext {
			to := loc.line.stop
			if underline := lineAt(src, loc.line.stop); underline.start == loc.line.stop {
				to = underline.stop
			}
			return loc.line.start, to, repl, nil
		}
		return loc.line.start, loc.line.stop, repl, nil
	}
	repl := quote + content + eol
	if b.Setext {
		to := loc.line.stop
		if underline := lineAt(src, loc.line.stop); underline.start == loc.line.stop {
			to = underline.stop
		}
		return loc.line.start, to, repl, nil
	}
	return loc.line.start, loc.line.stop, repl, nil
}

func headingClosing(suffix string) string {
	s := strings.TrimRight(suffix, " \t")
	if s == "" {
		return ""
	}
	t := strings.TrimSpace(s)
	if t == "" || strings.Trim(t, "#") != "" {
		return ""
	}
	return " " + t
}

func codeBlockBody(src string, b *parser.Block) string {
	if !b.Fenced {
		return src[b.Start:b.End]
	}
	start := lineAt(src, b.Start)
	from := start.stop
	if from > b.End {
		from = b.End
	}
	body := src[from:b.End]
	last := lineAt(src, b.End)
	if strings.TrimSpace(strings.Trim(src[last.start:last.end], " \t")) != "" &&
		strings.HasPrefix(strings.TrimSpace(src[last.start:last.end]), strings.Repeat(string(b.FenceChar), max(3, b.FenceLength))) {
		body = src[from:last.start]
	}
	return body
}

func listStylePrefix(src string, d *parser.Document, loc paragraphLocation, old, next ParagraphStyle) (string, string, string) {
	if loc.item >= 0 {
		item := d.Block(loc.item)
		line := lineAt(src, item.Start)
		container := src[line.start:item.Start]
		marker := src[item.Start:loc.contentStart]
		if old.List == next.List && old.Heading == 0 {
			if next == old {
				return container, "", marker
			}
			if next.List == Tasks {
				if i := strings.Index(marker, "["); i >= 0 {
					box := "[ ] "
					if next.Checked {
						box = "[x] "
					}
					return container, "", marker[:i] + box
				}
				return container, "", marker + "[ ] "
			}
			if next.List == Numbers {
				delim := "."
				if ch := d.Block(loc.list).Marker; ch == '.' || ch == ')' {
					delim = string(ch)
				}
				return container, "", strconv.Itoa(max(1, item.Number)) + delim + " "
			}
			if next.List == Bullets {
				ch := d.Block(loc.list).Marker
				if ch != '-' && ch != '*' && ch != '+' {
					ch = '-'
				}
				return container, "", string(ch) + " "
			}
		}
	}
	indent := strings.Repeat(" ", max(0, next.Indent-1)*2)
	switch next.List {
	case Numbers:
		return "", indent, "1. "
	case Tasks:
		if next.Checked {
			return "", indent, "- [x] "
		}
		return "", indent, "- [ ] "
	default:
		return "", indent, "- "
	}
}

func normalizeIntentText(s string) string {
	return strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
}

func (e *Edit) formatInline(start, end int, fn func(a *Attributes)) error {
	if start == end {
		before := e.d.typingAttributes()
		after := before
		fn(&after)
		after.Attachment = nil
		e.d.typing = &after
		return nil
	}
	old := e.d.text
	want := old.Format(start, end, fn)
	pa, pz := old.paragraphSpan(start, end)
	if pa != pz {
		return e.canonicalizeInlineParagraphs(start, end, want, pa, pz)
	}
	if e.tryInlineMinimal(start, end, want, pa) {
		return nil
	}
	return e.canonicalizeInlineParagraphs(start, end, want, pa, pz)
}

func (e *Edit) tryInlineMinimal(start, end int, want Text, p int) bool {
	t := e.d.text
	loc := e.d.paragraphLocation(p)
	if loc.leaf < 0 {
		return false
	}
	before := t.AttributesAt(start)
	after := want.AttributesAt(start)
	kind := changedMarkerKind(before, after)
	if kind == 0 {
		return false
	}
	src := e.d.md.p.String()
	base := e.d.md.m.start(p)
	var hits []box
	for _, b := range e.d.md.m.boxes(p) {
		if b.kind == kind {
			if base+b.docEnd > start && base+b.docStart < end {
				hits = append(hits, b)
			}
		}
	}
	on := markerOn(after, kind)
	if kind == MarkerLink {
		if on {
			text := t.Slice(start, end).String()
			raw := "[" + escapeBracketText(text) + "](" + escapeLinkDest(after.Link) + ")"
			from, to := e.d.sourceAtDoc(start), e.d.sourceAtDoc(end)
			if from < 0 || to < from || to > len(src) {
				return false
			}
			return e.replaceMarkdownSource(from, to, raw)
		}
	}
	if on && len(hits) == 0 {
		open, close := markerDefaults(kind)
		from := e.d.sourceAtDoc(start)
		to := e.d.sourceAtDoc(end)
		if from < 0 || to < from || to > len(src) {
			return false
		}
		return e.replaceMarkdownSource(from, to, open+src[from:to]+close)
	}
	if len(hits) > 1 {
		return false
	}
	if !on && len(hits) == 1 {
		// Turning a format off inside an element of that kind removes the
		// whole element, as Typora does, however much of its text the
		// selection covers: its own delimiters go, its content bytes stay,
		// including the markers of nested elements (`**a *b* c**` with b
		// selected is `a *b* c`; `***y***` turned italic off is `**y**`).
		b := hits[0]
		return e.replaceMarkdownSource(b.openStart, b.closeEnd, src[b.contentFrom:b.contentTo])
	}
	return false
}

func (e *Edit) canonicalizeInlineParagraphs(start, end int, want Text, pa, pz int) error {
	for p := pz; p >= pa; p-- {
		if p < 0 || p >= e.d.text.ParagraphCount() || p >= want.ParagraphCount() {
			return ErrStructuralEdit
		}
		wantPara := want.paragraph(p)
		if sameParagraph(e.d.text.paragraph(p), wantPara, true) {
			continue
		}
		loc := e.d.paragraphLocation(p)
		if loc.leaf < 0 {
			return ErrStructuralEdit
		}
		switch e.d.md.p.Block(loc.leaf).Kind {
		case parser.Paragraph, parser.Heading:
		default:
			return ErrStructuralEdit
		}
		pm := seqAt(e.d.md.m.paras, p)
		from := e.d.md.m.start(p)
		to := from + pm.src
		if pm.src == 0 {
			from, to = loc.contentStart, loc.contentEnd
		}
		content := paragraphMarkdownContent(wantPara, e.d.text.paragraph(p).Style)
		if strings.ContainsAny(e.d.md.p.String()[from:to], "\r\n") {
			if !canonicalizeSoftBreaks(e.d, p, loc, pm) {
				return ErrStructuralEdit
			}
			// A container's continuation lines keep their prefix: the
			// runs hold no byte for it, and Markdown needs it to read
			// the line as the same block.
			content = strings.ReplaceAll(content, "\n", defaultEOL(e.d.md.p.String())+e.d.continuationPrefix(loc))
		} else if eol := defaultEOL(e.d.md.p.String()); eol != "\n" {
			content = strings.ReplaceAll(content, "\n", eol)
		}
		if !e.replaceMarkdownSource(from, to, content) {
			return ErrStructuralEdit
		}
	}
	return nil
}

// canonicalizeSoftBreaks reports whether a paragraph region that holds line
// endings can be written again from its runs. A soft line break's canonical
// Markdown is a line ending followed by the container's continuation prefix,
// which the writer adds from the paragraph's location; a leading blank line
// or a hard break marker would be lost, so those stay unwritable.
func canonicalizeSoftBreaks(d *Document, p int, loc paragraphLocation, pm *paraMap) bool {
	if loc.leaf < 0 {
		return false
	}
	if d.md.p.Block(loc.leaf).Kind != parser.Paragraph {
		return false
	}
	if st := d.text.paragraph(p).Style; st.CodeBlock {
		return false
	}
	if len(pm.segs) > 0 && pm.segs[0].Kind == SegmentBreak {
		return false
	}
	for _, bx := range pm.boxes {
		if bx.kind.hardBreak() {
			return false
		}
	}
	return true
}

func paragraphMarkdownContent(p Paragraph, st ParagraphStyle) string {
	q := Paragraph{Style: ParagraphStyle{}, Runs: p.Runs}
	s := TextOf(q).Markdown()
	s = strings.TrimSuffix(s, "\n")
	if st.CodeBlock {
		return s
	}
	return s
}

func changedMarkerKind(before, after Attributes) MarkerKind {
	switch {
	case before.Link != after.Link:
		return MarkerLink
	case before.Bold() != after.Bold():
		return MarkerStrong
	case before.Italic != after.Italic:
		return MarkerEmphasis
	case before.Code != after.Code:
		return MarkerCode
	case before.Strikethrough != after.Strikethrough:
		return MarkerStrikethrough
	case before.Underline != after.Underline:
		return MarkerHTML
	}
	return 0
}

func markerOn(a Attributes, kind MarkerKind) bool {
	switch kind {
	case MarkerStrong:
		return a.Bold()
	case MarkerEmphasis:
		return a.Italic
	case MarkerCode:
		return a.Code
	case MarkerStrikethrough:
		return a.Strikethrough
	case MarkerHTML:
		return a.Underline
	case MarkerLink:
		return a.Link != ""
	}
	return false
}

func markerDefaults(kind MarkerKind) (string, string) {
	switch kind {
	case MarkerStrong:
		return "**", "**"
	case MarkerEmphasis:
		return "*", "*"
	case MarkerCode:
		return "`", "`"
	case MarkerStrikethrough:
		return "~~", "~~"
	case MarkerHTML:
		return "<u>", "</u>"
	case MarkerLink:
		return "[", "]()"
	}
	return "", ""
}

func escapeBracketText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "]", "\\]")
}

func escapeLinkDest(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), ")", "\\)")
}

func (d *Document) PasteMarkdown(from, to int, md string) error {
	d.lock()
	defer d.mu.Unlock()
	return d.applyIntent(EditRequest{Intent: IntentPasteMarkdown, Start: from, End: to, Text: md})
}

func (d *Document) PastePlain(from, to int, s string) error {
	d.lock()
	defer d.mu.Unlock()
	return d.applyIntent(EditRequest{Intent: IntentPastePlain, Start: from, End: to, Text: s})
}
