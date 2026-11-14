package richtext

import (
	"strings"

	"github.com/egoist/mygo/internal/text"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// FormatState is whether a format holds for a selection: all of it, none,
// or some of it, as a toolbar's button shows it.
type FormatState uint8

const (
	FormatOff FormatState = iota
	FormatOn
	FormatMixed
)

// Formatting describes the formats of a document's selection, as
// a toolbar and a Format menu show them: of the runes selected, or of the
// typing attributes at a caret.
type Formatting struct {
	// Attributes are those of the first rune selected, or the typing
	// attributes at a caret.
	Attributes                                   Attributes
	Bold, Italic, Underline, Strikethrough, Code FormatState
	// Link is the link all of the selection is in, "" when none is.
	Link string
	// Paragraph is the style of the first paragraph selected, and
	// ParagraphsMixed whether others differ from it.
	Paragraph       ParagraphStyle
	ParagraphsMixed bool
}

// Perform runs an Edit or Format command on the document's active editor, as
// the command's keyboard shortcut does, and reports whether the editor knows
// the command. A menu accelerator must use this path: calling a Document
// command directly skips what the view decides first, such as discarding a
// transient block on undo or refusing a format inside inline code.
func (d *Document) Perform(command string) bool {
	d.lock()
	v := d.activeView()
	d.mu.Unlock()
	if v == nil {
		return false
	}
	return v.editCommand(command)
}

// Formatting returns the formats of the selection.
func (d *Document) Formatting() Formatting {
	d.lock()
	defer d.mu.Unlock()
	return d.formatting()
}

func (d *Document) formatting() Formatting {
	a, z := d.span()
	f := Formatting{Attributes: d.typingAttributes()}
	if a < z {
		// From the first rune that takes a format, not a newline.
		for _, r := range d.text.Runs(a, z) {
			if r.Text != "\n" {
				f.Attributes = r.Attributes
				f.Attributes.Attachment = nil
				break
			}
		}
	}
	state := func(on bool) FormatState {
		if on {
			return FormatOn
		}
		return FormatOff
	}
	at := f.Attributes
	f.Bold, f.Italic, f.Underline, f.Strikethrough, f.Code = state(at.Bold()), state(at.Italic), state(at.Underline), state(at.Strikethrough), state(at.Code)
	f.Link = at.Link
	if a < z {
		mix := func(s *FormatState, on bool) {
			if (*s == FormatOn) != on {
				*s = FormatMixed
			}
		}
		for _, r := range d.text.Runs(a, z) {
			if r.Text == "\n" {
				continue // a newline selected takes no format
			}
			x := r.Attributes
			mix(&f.Bold, x.Bold())
			mix(&f.Italic, x.Italic)
			mix(&f.Underline, x.Underline)
			mix(&f.Strikethrough, x.Strikethrough)
			mix(&f.Code, x.Code)
			if x.Link != f.Link {
				f.Link = ""
			}
		}
	}
	pa, pz := d.text.paragraphSpan(a, z)
	seqEach(d.text.seq(), pa, func(i int, p *richPara) bool {
		st := p.style
		if i == pa {
			f.Paragraph = st
		} else if st.ID = f.Paragraph.ID; st != f.Paragraph {
			// Paragraphs of other IDs look the same.
			f.ParagraphsMixed = true
		}
		return i < pz
	})
	return f
}

// Format changes the attributes of the selection with fn, as one step of
// undo, or the typing attributes at a caret.
func (d *Document) Format(fn func(a *Attributes)) {
	d.lock()
	defer d.mu.Unlock()
	d.format(fn)
}

func (d *Document) format(fn func(a *Attributes)) {
	a, z := d.span()
	if a == z {
		if d.md == nil {
			t := d.typingAttributes()
			fn(&t)
			t.Attachment = nil
			d.typing = &t
			return
		}
		d.edit(false, func(e *Edit) { e.Format(a, z, fn) })
		return
	}
	d.formatSelection(a, z, d.anchor, d.caret, fn)
}

func (d *Document) formatSelection(start, end, anchor, caret int, fn func(a *Attributes)) {
	d.edit(false, func(e *Edit) {
		e.Format(start, end, fn)
		if e.err == nil {
			e.Select(anchor, caret)
		}
	})
}

// toggle turns a format on for the selection unless all of it has it, then
// off.
func (d *Document) toggle(kind MarkerKind, state func(f Formatting) FormatState, set func(a *Attributes, on bool)) {
	d.lock()
	defer d.mu.Unlock()
	on := state(d.formatting()) != FormatOn
	if d.md != nil {
		a, z := d.span()
		if a == z && d.sourceInsideMark(a, MarkerCode) && kind != MarkerCode {
			return
		}
		if a < z && strings.TrimSpace(d.text.Slice(a, z).String()) == "" {
			// A selection of whitespace alone takes no marker: Typora writes
			// no delimiter pair for it, leaving the bytes and the selection
			// as they were (record 2026-11-12, seed15/c12).
			return
		}
		if a == z {
			if from, to, ok := d.markEndingAt(a, kind); ok {
				anchor, caret := d.anchor, d.caret
				d.assignSelection(from, to, false)
				d.formatSelection(from, to, anchor, caret, func(a *Attributes) { set(a, false) })
				return
			}
		}
		if a == z {
			from, to, inWord := d.text.wordAt(a)
			if inWord && !d.wordFormatBlocked(from) {
				// Typora formats the word at the caret rather than only
				// setting the typing attributes and leaves the caret where
				// it was in the word: inside, between the same two
				// characters (record 2026-11-10, fmtprobe/i/mid-latin); at
				// the start, just after the opening delimiter, with the
				// delimiters shown (record 2026-11-10, seed4/c19). The
				// paragraph's first word is the exception: there the
				// formatted word stays selected between its delimiters
				// (record 2026-11-09, seed4/c29; record 2026-11-10,
				// diff.json 6f050ddc).
				if on && from == a {
					if open, _ := markerDefaults(kind); open != "" {
						start := d.sourceAtDoc(from)
						// The word starts the paragraph when nothing but its
						// leading whitespace stands in front of it. Delimiter
						// bytes the line already holds mean a word after them,
						// whose toggle leaves the caret right after the opening
						// delimiter as any other word's does (record 2026-11-13,
						// seed16/c6: `**\t\t a` keeps the caret at 17, not the
						// selection 17:18).
						first := strings.TrimSpace(d.text.Slice(d.text.paraStart(d.text.paraAt(a)), a).String()) == ""
						if start >= 0 && first {
							end := d.sourceAtDoc(to)
							d.edit(false, func(e *Edit) {
								e.Format(from, to, func(x *Attributes) { set(x, on) })
								if e.err == nil && end >= start {
									e.selectMarkdownSourceRange(start+len(open), end+len(open))
								}
							})
							return
						}
						if start >= 0 {
							d.edit(false, func(e *Edit) {
								e.Format(from, to, func(x *Attributes) { set(x, on) })
								if e.err == nil {
									e.selectMarkdownSource(start + len(open))
								}
							})
							return
						}
					}
				}
				// A caret inside the word keeps its character on either
				// toggle; at the word's start or end it stays at that edge
				// (record 2026-11-10, fmtprobe/i/off-mid-latin,
				// fmtprobe/i/off-start-latin, fmtprobe/i/end-latin).
				d.formatSelection(from, to, a, a, func(a *Attributes) { set(a, on) })
				return
			}
			atLinkEnd := inWord && d.wordFormatBlocked(from) && d.atLinkEnd(a)
			if on && (!inWord || atLinkEnd) {
				if open, close := markerDefaults(kind); open != "" && close != "" {
					// No word sits at the caret: Typora writes the pair of
					// delimiters there and leaves the caret between them (an
					// empty italic is `**`, an empty strong `****`). A caret
					// at a link's visible end is outside its text too, so the
					// word in the link is not the caret's to format: the pair
					// goes after the link's target and the link stays hidden
					// (record 2026-11-09, inline/link/left-outside/cmd-i;
					// record 2026-11-10, diff.json 16543a65; record
					// 2026-11-12, seed19/c2).
					at := d.sourceAtDoc(a)
					if at >= 0 {
						d.edit(false, func(e *Edit) {
							if !e.replaceMarkdownSource(at, at, open+close) {
								e.fail(ErrStructuralEdit)
								return
							}
							if e.step != nil {
								e.selectMarkdownSource(at + len(open))
							}
						})
						return
					}
				}
			}
		}
		if a < z && on {
			from, to := d.sourceAtDoc(a), d.sourceAtDoc(z)
			if from > to {
				from, to = to, from
			}
			open, close := markerDefaults(kind)
			pa, pz := d.text.paragraphSpan(a, z)
			d.format(func(x *Attributes) { set(x, on) })
			// Typora selects a wrapped span from its content start to its
			// content end whichever way the selection pointed before the
			// command; a backward selection flips (diff.json c0f1953f).
			if open != "" && close != "" && from >= 0 && to >= from && d.md.ok {
				if pa != pz {
					// A format that spans blocks selects the visible content
					// of the first and last block whole, delimiters hidden
					// (record 2026-11-10, seed7/c8): the anchor is the first
					// block's content start and the caret the last block's
					// line end, after its closing delimiters.
					first, last := d.paragraphLocation(pa), d.paragraphLocation(pz)
					if first.leaf >= 0 && last.leaf >= 0 && first.contentStart <= last.contentEnd {
						d.assignSourceSelection(SourceSelection{
							Anchor: sourcePoint(first.contentStart, BoundaryUpstream),
							Caret:  sourcePoint(last.contentEnd, BoundaryDownstream),
						}, true)
						return
					}
				}
				start, oldEnd, newEnd := d.md.last[0], d.md.last[1], d.md.last[2]
				if start == from && oldEnd == to &&
					newEnd-start == to-from+len(open)+len(close) {
					d.assignSourceSelection(SourceSelection{
						Anchor: sourcePoint(start+len(open), BoundaryUpstream),
						Caret:  sourcePoint(newEnd-len(close), BoundaryDownstream),
					}, true)
				}
			}
			return
		}
	}
	d.format(func(a *Attributes) { set(a, on) })
}

// opensFenceLocked reports whether the caret's source line opens a fenced
// code block: the line, past any list or quote prefix and its indentation,
// starts with a run of three or more backticks or tildes. Typora reads
// such a line as a fence even while the live view still draws the run
// among its container's text, so an inline command there writes nothing at
// all (record 2026-11-12, probe124b/tick3-i, /tick3-b, /tick4-i,
// /tilde3-i, /para-fence-i; seed14/c5). A run of two backticks opens no
// fence and the command still wraps the word (/tick2-i), as it does when
// the run follows content (/mid-fence-i).
func (v *view) opensFenceLocked() bool {
	if v.doc.md == nil {
		return false
	}
	src := v.doc.md.p.String()
	if len(src) == 0 {
		return false
	}
	at := max(0, min(v.doc.sourceAtDoc(v.doc.caret), len(src)))
	line := lineAt(src, at)
	if line.start < 0 || line.end > len(src) || at < line.start || at > line.end {
		return false
	}
	start := line.start
	for start < line.end && (src[start] == ' ' || src[start] == '	') {
		start++
	}
	for _, prefix := range []string{"- ", "* ", "+ ", "> "} {
		if strings.HasPrefix(src[start:line.end], prefix) {
			start += len(prefix)
			break
		}
	}
	if start >= line.end {
		return false
	}
	return parser.OpenFenceLength(src[start:line.end], 0) > 0
}

// wordFormatBlocked reports whether the word at from belongs to an inline
// element whose text formatting is applied inside it: Typora leaves those
// to the typing attributes instead of wrapping the word again.
func (d *Document) wordFormatBlocked(from int) bool {
	at := d.text.AttributesAt(from)
	return at.Link != "" || at.Code
}

// atLinkEnd reports whether doc stands at the visible end of a link, after
// its closing syntax. The caret is outside the element's text there, so the
// word the projection puts before it is not the caret's to format, and
// Typora writes a fresh pair instead.
func (d *Document) atLinkEnd(doc int) bool {
	if d.md == nil {
		return false
	}
	p := d.text.paraAt(doc)
	if p < 0 || p >= d.md.m.Len() {
		return false
	}
	source := d.sourceAtDoc(doc)
	base := d.text.paraStart(p)
	for _, b := range d.md.m.boxes(p) {
		if b.kind == MarkerLink && b.end == source && base+b.docEnd == doc {
			return true
		}
	}
	return false
}

// wordAt returns the word (letters, digits, or underscores) the caret
// touches: the one it is inside or at the end of, or the one it starts.
// An ideograph or a kana is a word alone, so the caret between Latin
// letters and Han text takes only the Han rune: Typora's DOM word stops at
// the script change (record 2026-11-10, seed4/c19). An empty paragraph has
// no word.
func (t Text) wordAt(i int) (int, int, bool) {
	start, end := t.ParagraphRange(i)
	if i < start || i > end {
		return 0, 0, false
	}
	runes := []rune(t.Slice(start, end).String())
	at := i - start
	if at < len(runes) && text.IsWordRune(runes[at]) {
		a := at
		for a > 0 && text.IsWordRune(runes[a-1]) && !text.FormatWordBoundary(runes[a-1], runes[a]) {
			a--
		}
		z := at + 1
		for z < len(runes) && text.IsWordRune(runes[z]) && !text.FormatWordBoundary(runes[z-1], runes[z]) {
			z++
		}
		return start + a, start + z, true
	}
	if at > 0 && text.IsWordRune(runes[at-1]) {
		a := at - 1
		for a > 0 && text.IsWordRune(runes[a-1]) && !text.FormatWordBoundary(runes[a-1], runes[a]) {
			a--
		}
		return start + a, start + at, true
	}
	return 0, 0, false
}

// ToggleBold makes the selection bold, or not when all of it is, as ⌘B
// does.
func (d *Document) ToggleBold() {
	d.toggle(MarkerStrong, func(f Formatting) FormatState { return f.Bold }, func(a *Attributes, on bool) {
		if on {
			a.Weight = 700
		} else {
			a.Weight = 0
		}
	})
}

// ToggleItalic, ToggleUnderline, ToggleStrikethrough and ToggleCode turn
// those formats on for the selection, or off when all of it has them.
func (d *Document) ToggleItalic() {
	d.toggle(MarkerEmphasis, func(f Formatting) FormatState { return f.Italic }, func(a *Attributes, on bool) { a.Italic = on })
}

func (d *Document) ToggleUnderline() {
	d.toggle(MarkerHTML, func(f Formatting) FormatState { return f.Underline }, func(a *Attributes, on bool) { a.Underline = on })
}

func (d *Document) ToggleStrikethrough() {
	d.toggle(MarkerStrikethrough, func(f Formatting) FormatState { return f.Strikethrough }, func(a *Attributes, on bool) { a.Strikethrough = on })
}

func (d *Document) ToggleCode() {
	d.toggle(MarkerCode, func(f Formatting) FormatState { return f.Code }, func(a *Attributes, on bool) { a.Code = on })
}

func (d *Document) sourceInsideMark(doc int, kind MarkerKind) bool {
	if d.md == nil {
		return false
	}
	p := d.text.paraAt(doc)
	source := d.sourceAtDoc(doc)
	for _, b := range d.md.m.boxes(p) {
		if b.kind == kind && b.contentFrom <= source && source <= b.contentTo {
			return true
		}
	}
	return false
}

func (d *Document) markEndingAt(doc int, kind MarkerKind) (int, int, bool) {
	if d.md == nil {
		return 0, 0, false
	}
	p := d.text.paraAt(doc)
	local := doc - d.text.paraStart(p)
	for _, b := range d.md.m.boxes(p) {
		if b.kind == kind && b.docEnd > b.docStart && local == b.docEnd {
			start := d.text.paraStart(p) + b.docStart
			return start, doc, true
		}
	}
	return 0, 0, false
}

// linkRange returns the runes around i that link to the same URL as the
// rune at i, or i, i.
func (t Text) linkRange(i int) (int, int) {
	url := t.AttributesAt(i).Link
	if url == "" || i >= t.Len() {
		return i, i
	}
	ps, pe := t.ParagraphRange(i)
	a, z := i, i
	for a > ps && t.AttributesAt(a-1).Link == url {
		a--
	}
	for z < pe && t.AttributesAt(z).Link == url {
		z++
	}
	return a, z
}

// SetLink links the selection to url, or takes its link away for "". At a
// caret inside a link, it changes or takes away that link; elsewhere it
// inserts url, linked to itself.
func (d *Document) SetLink(url string) {
	d.lock()
	defer d.mu.Unlock()
	if d.md != nil {
		d.setMarkdownLink(url, false)
		return
	}
	a, z := d.span()
	if a == z {
		la, lz := d.text.linkRange(a)
		if la == lz && a > 0 {
			la, lz = d.text.linkRange(a - 1)
			if lz != a {
				la, lz = a, a
			}
		}
		if la == lz {
			if url != "" {
				attrs := d.typingAttributes()
				attrs.Link = url
				d.edit(false, func(e *Edit) { e.ReplaceString(a, a, url, attrs) })
			}
			return
		}
		a, z = la, lz
	}
	sel := d.selection
	d.edit(false, func(e *Edit) { e.Format(a, z, func(x *Attributes) { x.Link = url }) })
	d.assignSourceSelection(sel, false)
}

func (d *Document) InsertLink() {
	d.lock()
	defer d.mu.Unlock()
	if d.md != nil {
		d.setMarkdownLink("", true)
		return
	}
	a, z := d.span()
	if a == z {
		t := d.typingAttributes()
		t.Link = ""
		d.typing = &t
		return
	}
	text := d.text.Slice(a, z).String()
	d.edit(false, func(e *Edit) {
		e.ReplaceString(a, z, "["+text+"]()", Attributes{})
		e.Select(a+1, a+1)
	})
}

func (d *Document) setMarkdownLink(url string, emptyDest bool) {
	a, z := d.span()
	anchor, caret := d.anchor, d.caret
	if a == z && d.sourceInsideMark(a, MarkerCode) {
		return
	}
	if a == z {
		if a < d.text.Len() {
			if la, lz := d.text.linkRange(a); la != lz {
				d.formatSelection(la, lz, anchor, caret, func(x *Attributes) { x.Link = url })
				return
			}
		}
		if a > 0 {
			la, lz := d.text.linkRange(a - 1)
			if la != lz && lz == a {
				d.formatSelection(la, lz, anchor, caret, func(x *Attributes) { x.Link = url })
				return
			}
		}
	}
	if a < z && url == "" && !emptyDest {
		d.formatSelection(a, z, anchor, caret, func(x *Attributes) { x.Link = "" })
		return
	}
	if a < z && url != "" {
		if d.formatting().Link == url {
			return
		}
		if la, lz := d.text.linkRange(a); la < lz && a >= la && z <= lz {
			if from, to, ok := d.markdownLinkSourceRange(la, lz); ok {
				text := d.text.Slice(la, lz).String()
				raw := "[" + escapeBracketText(text) + "](" + escapeLinkDest(url) + ")"
				d.edit(false, func(e *Edit) {
					if !e.replaceMarkdownSource(from, to, raw) {
						e.fail(ErrStructuralEdit)
						return
					}
					e.selectMarkdownSource(from + len(raw))
				})
				return
			}
		}
	}
	text := d.text.Slice(a, z).String()
	if emptyDest && looksLikeURL(strings.TrimSpace(text)) {
		url = strings.TrimSpace(text)
		emptyDest = false
	}
	if a < z && emptyDest {
		// The brackets wrap the selected source bytes and nothing else:
		// markers around the selection belong to their elements and stay
		// outside the link. markdownSlice, which repeats those markers,
		// applies to the text moved out of the document, not wrapped in
		// place.
		from, to := d.sourceAtDoc(a), d.sourceAtDoc(z)
		if from > to {
			from, to = to, from
		}
		if src := d.md.p.String(); from <= to && to <= len(src) {
			label := src[from:to]
			d.edit(false, func(e *Edit) {
				if !e.replaceMarkdownSource(from, to, "["+label+"]()") {
					e.fail(ErrStructuralEdit)
					return
				}
				e.d.setSourceSelection(SourceSelection{
					Anchor: sourcePoint(from+1, BoundaryUpstream),
					Caret:  sourcePoint(from+1+len(label), BoundaryDownstream),
				})
			})
			return
		}
	}
	content := text
	if a < z {
		content = d.markdownSlice(a, z)
	}
	syntax := "[" + content + "](" + escapeLinkDest(url) + ")"
	caretSource := d.sourceAtDoc(a) + len(syntax)
	if emptyDest {
		caretSource = d.sourceAtDoc(a) + len(content) + 3
		if content == "" {
			caretSource = d.sourceAtDoc(a) + 1
		}
	}
	d.edit(false, func(e *Edit) {
		if err := e.pasteMarkdown(a, z, syntax); err != nil {
			e.fail(err)
			return
		}
		e.selectMarkdownSource(caretSource)
	})
}

func (d *Document) markdownLinkSourceRange(a, z int) (int, int, bool) {
	if d.md == nil {
		return 0, 0, false
	}
	pa, pz := d.text.paragraphSpan(a, z)
	for p := pa; p <= pz && p < d.md.m.Len(); p++ {
		ps := d.text.paraStart(p)
		for _, b := range d.md.m.boxes(p) {
			if b.kind != MarkerLink || b.docEnd <= b.docStart {
				continue
			}
			if a >= ps+b.docStart && z <= ps+b.docEnd {
				return b.start, b.end, true
			}
		}
	}
	return 0, 0, false
}

// FormatParagraphs changes the styles of the paragraphs selected with fn,
// as one step of undo.
func (d *Document) FormatParagraphs(fn func(p *ParagraphStyle)) {
	d.lock()
	defer d.mu.Unlock()
	d.formatParagraphs(fn)
}

func (d *Document) formatParagraphs(fn func(p *ParagraphStyle)) {
	a, z := d.span()
	typing := d.typing
	d.edit(false, func(e *Edit) { e.FormatParagraphs(a, z, fn) })
	d.typing = typing
}

// paragraphsAll reports whether every paragraph selected satisfies fn.
func (d *Document) paragraphsAll(fn func(p ParagraphStyle) bool) bool {
	a, z := d.span()
	pa, pz := d.text.paragraphSpan(a, z)
	all := true
	seqEach(d.text.seq(), pa, func(i int, p *richPara) bool {
		all = fn(p.style)
		return all && i < pz
	})
	return all
}

// SetHeading makes the paragraphs selected headings of level, from 1, or
// body paragraphs for 0.
func (d *Document) SetHeading(level int) {
	d.FormatParagraphs(func(p *ParagraphStyle) {
		p.Heading = max(0, min(level, 6))
		if p.Heading > 0 {
			p.List, p.Checked, p.CodeBlock = NoList, false, false
		}
	})
}

// ToggleList makes the paragraphs selected items of a list of kind, or
// takes them out of it when all of them are.
func (d *Document) ToggleList(kind ListKind) {
	d.lock()
	defer d.mu.Unlock()
	off := d.paragraphsAll(func(p ParagraphStyle) bool { return p.List == kind })
	d.formatParagraphs(func(p *ParagraphStyle) {
		if off {
			p.List, p.Checked = NoList, false
			return
		}
		p.List, p.Heading, p.CodeBlock = kind, 0, false
		if kind != Tasks {
			p.Checked = false
		}
	})
}

// ToggleQuote and ToggleCodeBlock make the paragraphs selected a quotation
// or a block of code, or not when all of them are.
func (d *Document) ToggleQuote() {
	d.lock()
	defer d.mu.Unlock()
	off := d.paragraphsAll(func(p ParagraphStyle) bool { return p.Quote })
	d.formatParagraphs(func(p *ParagraphStyle) {
		if off || !p.Quote {
			p.QuoteDepth = 0
		}
		p.Quote = !off
	})
}

func (d *Document) ToggleCodeBlock() {
	d.lock()
	defer d.mu.Unlock()
	off := d.paragraphsAll(func(p ParagraphStyle) bool { return p.CodeBlock })
	d.formatParagraphs(func(p *ParagraphStyle) {
		p.CodeBlock = !off
		if p.CodeBlock {
			p.Heading, p.List, p.Checked = 0, NoList, false
		}
	})
}

// maxIndent is the deepest a paragraph indents.
const maxIndent = 8

// Indent indents the paragraphs selected by levels, or outdents them for
// a negative count, as Tab and Shift-Tab do in a list.
func (d *Document) Indent(levels int) {
	d.lock()
	defer d.mu.Unlock()
	if d.md != nil {
		a, _ := d.span()
		intent := IntentIndent
		n := levels
		if n < 0 {
			n = -n
		}
		for range n {
			if levels < 0 {
				intent = IntentOutdent
			}
			if err := d.applyIntent(EditRequest{Intent: intent, Start: a, End: a}); err != nil {
				return
			}
		}
		return
	}
	d.formatParagraphs(func(p *ParagraphStyle) { p.Indent = max(0, min(p.Indent+levels, maxIndent)) })
}

// SetAlign aligns the lines of the paragraphs selected.
func (d *Document) SetAlign(a ui.Align) {
	d.FormatParagraphs(func(p *ParagraphStyle) { p.Align = a })
}

// ToggleChecked checks the task holding rune i, or unchecks it.
func (d *Document) ToggleChecked(i int) {
	d.lock()
	defer d.mu.Unlock()
	if d.md != nil {
		_ = d.applyIntent(EditRequest{Intent: IntentToggleTask, Start: i, End: i})
		return
	}
	typing := d.typing
	d.edit(false, func(e *Edit) {
		e.FormatParagraphs(i, i, func(p *ParagraphStyle) {
			if p.List == Tasks {
				p.Checked = !p.Checked
			}
		})
	})
	d.typing = typing
}

// InsertText replaces the selection with s, in the typing attributes, as
// typing does but as a step of undo of its own.
func (d *Document) InsertText(s string) {
	s = normalizeNewlines(s)
	d.lock()
	defer d.mu.Unlock()
	a, z := d.span()
	attrs := d.typingAttributes()
	d.edit(false, func(e *Edit) {
		n := e.Text().Len()
		e.ReplaceString(a, z, s, attrs)
		end := z + e.Text().Len() - n
		e.Select(end, end)
	})
}

// Insert replaces the selection with text, as a paste does.
func (d *Document) Insert(text Text) {
	d.lock()
	defer d.mu.Unlock()
	a, z := d.span()
	d.edit(false, func(e *Edit) {
		e.Replace(a, z, text)
		e.Select(a+text.Len(), a+text.Len())
	})
}

// InsertAttachment replaces the selection with an attachment.
func (d *Document) InsertAttachment(a *Attachment) {
	d.lock()
	defer d.mu.Unlock()
	s, z := d.span()
	attrs := d.typingAttributes().extending()
	attrs.Attachment = a
	d.edit(false, func(e *Edit) {
		e.ReplaceString(s, z, string(ObjectReplacement), attrs)
		e.Select(s+1, s+1)
	})
}

// normalizeNewlines turns the newlines of other systems into "\n", the
// editor's LineSeparator among them: a Text holds a line break within a
// paragraph as a LineSeparator.
func normalizeNewlines(s string) string {
	if !strings.ContainsAny(s, "\r\u2028\u2029") {
		return s
	}
	return strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n").Replace(s)
}
