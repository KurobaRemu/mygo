package richtext

import (
	"strings"

	"github.com/egoist/mygo/ui/markdown/parser"
)

func quoteOnly(body string) bool {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, ">") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(body, ">")) == ""
}

// blankQuoteLine reports whether a line made of continuation prefixes is
// blank in its innermost quote: only whitespace, or quote markers with
// nothing inside them.
func blankQuoteLine(s string) bool {
	for {
		s = strings.TrimLeft(s, " ")
		if !strings.HasPrefix(s, ">") {
			return strings.TrimSpace(s) == ""
		}
		s = s[1:]
	}
}

// quoteTailEnd moves a paragraph's source end past the blank quote lines
// directly below it. A quote's trailing `>` lines spell no paragraph of
// their own, so the paragraph above them ends at its own bytes; Typora
// keeps them in the quote and puts the paragraph after it past them (record
// 2026-11-13, zz177/A-empty-line-enter).
func (d *Document) quoteTailEnd(end int) int {
	if d.md == nil {
		return end
	}
	src := d.md.p.String()
	line := lineAt(src, end)
	if line.end != end {
		// The paragraph stops inside a line: no quote tail follows it.
		return end
	}
	at := line.stop
	for at < len(src) {
		next := lineAt(src, at)
		if next.start != at || next.end == next.start || !quoteOnly(src[next.start:next.end]) {
			break
		}
		at = next.stop
	}
	return at
}

// blankLineBefore reports whether a blank line already separates the line
// at start from the block before it. Inside a quote the separator is a
// quote-only line.
func blankLineBefore(src string, start int, quote string) bool {
	if start <= 0 {
		return false
	}
	body := prevBody(src, previousMarkdownLine(src, start))
	if quote != "" {
		return quoteOnly(body)
	}
	return strings.TrimSpace(body) == ""
}

// lastBlockIsQuote reports whether the document's last block is a quote.
// The paragraph a quote's tail exits to stands below that quote, and the
// Enter that follows writes its blank line (record 2026-11-13,
// zz177/A-empty-line-enter cp2). A paragraph after any other block stands
// on the bytes its own lift wrote.
func (d *Document) lastBlockIsQuote() bool {
	if d.md == nil {
		return false
	}
	n := d.md.p.Len()
	if n < 2 {
		return false
	}
	return d.md.p.Block(n-1).Kind == parser.Quote
}

func (e *Edit) enterQuote(loc paragraphLocation, point, caretPoint int) error {
	src := e.d.md.p.String()
	// A paragraph holds soft line breaks: Enter works on the caret's line.
	// The line keeps its own quote markers, which end where its content
	// begins; a lazy continuation has none and takes the paragraph's.
	if point > loc.contentEnd {
		line := lineAt(src, point)
		start := line.start + quoteContentStart(src[line.start:line.end], strings.Count(loc.prefix, ">"))
		loc.line = line
		loc.contentStart, loc.contentEnd = start, line.end
		if pfx := src[line.start:start]; strings.Contains(pfx, ">") {
			loc.prefix, loc.suffix = pfx, ""
		}
	}
	content := src[loc.contentStart:loc.contentEnd]
	cut := max(0, min(point-loc.contentStart, len(content)))
	eol := loc.line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	if strings.TrimSpace(content) == "" {
		if strings.Count(loc.prefix, ">") <= 1 {
			if back := loc.line.start - 1; back > 0 {
				if src[back-1] == '\r' {
					back--
				}
				prev := lineAt(src, back)
				if quoteOnly(src[prev.start:prev.end]) {
					stop, newText := loc.line.stop, eol+eol
					if next := lineAt(src, stop); stop < len(src) && quoteOnly(src[next.start:next.end]) {
						stop, newText = next.stop, newText+eol
					}
					if !e.replaceMarkdownSource(prev.start, stop, newText) {
						return ErrStructuralEdit
					}
					if e.step != nil {
						e.selectMarkdownSource(prev.start + len(eol))
					}
					return nil
				}
			}
			newText := loc.line.eol
			if !e.replaceMarkdownSource(loc.line.start, loc.line.stop, newText) {
				return ErrStructuralEdit
			}
			if e.step != nil {
				at := e.d.docAtSource(loc.line.start)
				e.Select(at, at)
			}
			return nil
		}
		// A line inside a nested quote keeps its depth: Enter splits it
		// like a line with content, writing the quote-only separator and
		// the continuation line below (record 2026-11-11,
		// probe83/bare-gt-lines). Leaving the quote would drop the depth
		// the user is typing in.
	}
	prefix := loc.prefix
	if !strings.Contains(prefix, ">") {
		prefix = "> "
	}
	left, right := content[:cut], content[cut:]
	blank := ""
	if strings.Contains(prefix, ">") && (cut > 0 || cut < len(content) || strings.Count(prefix, ">") > 1) {
		// Two quote paragraphs need the blank quote line between them even
		// when the first half is empty: without it the two lines read back
		// as one paragraph. Enter past the last line's content opens the
		// empty line below it instead (record 2026-11-10, seed5/c10).
		blank = strings.TrimRight(prefix, " \t") + eol
	}
	before, after := e.splitDelimiters(loc.para, point)
	newText := prefix + left + before + eol + blank + prefix + after + right
	if right != "" || loc.line.stop < len(src) {
		newText += eol
	}
	if err := e.replaceMarkdownLine(loc, newText); err != nil {
		return err
	}
	if e.step != nil {
		e.selectMarkdownSource(caretPoint + len(before) + len(eol) + len(blank) + len(prefix) + len(after))
	}
	return nil
}

// backspaceEmptyQuoteLine removes one quote marker from the empty quote
// line the caret sits on. A line that still spells another level keeps its
// bytes: `> > ` becomes `> ` and the next press takes the outer marker. Once
// the line holds no marker, it is a blank line; the document's last line
// goes with its ending, since an empty line cannot end the bytes, while a
// line that still has a block below it stays blank (record 2026-11-13,
// zz177/bs-empty-quote, zz177/bs-mid-empty, zz177/bs-nested-empty).
func (e *Edit) backspaceEmptyQuoteLine(p, at int) error {
	src := e.d.md.p.String()
	line := lineAt(src, e.d.sourceAtParagraph(p, at-e.d.text.paraStart(p)))
	body := src[line.start:line.end]
	if !strings.Contains(body, ">") || !blankQuoteLine(body) {
		return ErrStructuralEdit
	}
	strip := quoteMarkerEnd(body)
	if strip <= 0 {
		return ErrStructuralEdit
	}
	from, to := line.start, line.start+strip
	rest := body[strip:]
	if strings.TrimSpace(rest) == "" && line.stop == len(src) {
		// The line is the document's last and holds nothing but its
		// markers: they leave with its line ending, which has no empty
		// line to hold below it.
		to = line.stop
	}
	if !e.replaceMarkdownSource(from, to, "") {
		return ErrStructuralEdit
	}
	if e.step != nil {
		// The caret stands after the markers the line keeps, which read as
		// the deeper quote's prefix (record 2026-11-13, zz177/bs-nested-empty).
		e.selectMarkdownSource(from + len(rest))
	}
	return nil
}

func stripQuotePrefixes(s string) string {
	rewritten, _ := stripQuotePrefixesAt(s, -1)
	return rewritten
}

// quoteMarkerEnd returns the length of the quote marker a line starts with:
// up to three spaces of indentation, the `>`, and the single space that may
// follow it. It returns 0 when the line holds no marker.
func quoteMarkerEnd(body string) int {
	i := 0
	for i < len(body) && i < 3 && body[i] == ' ' {
		i++
	}
	if i >= len(body) || body[i] != '>' {
		return 0
	}
	i++
	if i < len(body) && body[i] == ' ' {
		i++
	}
	return i
}

// stripQuotePrefixesAt removes a quote marker from every line of s and maps
// at, an offset in s, into the rewritten text. Bytes before the offset keep
// their place once their line prefixes leave; an offset inside a prefix
// lands at the content start the prefix precedes. A negative at maps
// nothing and yields -1.
func stripQuotePrefixesAt(s string, at int) (string, int) {
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	pos, mapped := 0, -1
	for _, line := range lines {
		body := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		eol := line[len(body):]
		strip := quoteMarkerEnd(body)
		if at >= pos && at <= pos+len(line) {
			rem := at - pos
			if rem > strip {
				rem -= strip
			} else {
				rem = 0
			}
			mapped = b.Len() + rem
		}
		if strip > 0 {
			b.WriteString(body[strip:])
			b.WriteString(eol)
		} else {
			b.WriteString(line)
		}
		pos += len(line)
	}
	return b.String(), mapped
}

func (e *Edit) liftQuote(loc paragraphLocation) error {
	q := e.d.ancestorKind(loc.leaf, parser.Quote)
	if q < 0 {
		return ErrStructuralEdit
	}
	b := e.d.md.p.Block(q)
	src := e.d.md.p.String()
	_, caret := e.Selection()
	p := e.d.text.paraAt(caret)
	point := e.d.sourceAtParagraph(p, caret-e.d.text.paraStart(p))
	rewritten, newPoint := stripQuotePrefixesAt(src[b.Start:b.End], point-b.Start)
	if !e.replaceMarkdownSource(b.Start, b.End, rewritten) {
		return ErrStructuralEdit
	}
	if e.step != nil {
		// The caret stays on the content it was in; only the line prefixes
		// before it leave (record 2026-11-10, difffuzz seed47).
		e.selectMarkdownSource(b.Start + max(0, newPoint))
	}
	return nil
}

// quoteContentStart returns the bytes a line's quote markers take for a
// paragraph depth levels into quotes: each marker may follow up to three
// spaces and take one space after it. A line without a marker at some
// level is a lazy continuation, and its content starts where the markers
// it has end.
func quoteContentStart(line string, depth int) int {
	i := 0
	for range depth {
		j := i
		for j < len(line) && j-i < 3 && line[j] == ' ' {
			j++
		}
		if j >= len(line) || line[j] != '>' {
			return i
		}
		j++
		if j < len(line) && line[j] == ' ' {
			j++
		}
		i = j
	}
	return i
}

func quotePrefix(prefix string) string {
	i := 0
	for i < len(prefix) && i < 3 && prefix[i] == ' ' {
		i++
	}
	if i >= len(prefix) || prefix[i] != '>' {
		return ""
	}
	i++
	if i < len(prefix) && prefix[i] == ' ' {
		i++
	}
	return prefix[:i]
}
