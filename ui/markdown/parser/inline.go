package parser

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// InlineKind is what an inline is.
type InlineKind uint8

// Kinds of inlines.
const (
	// Text is literal text, in Text.
	Text InlineKind = iota
	// SoftBreak is a line ending within a paragraph, shown as a space.
	SoftBreak
	// LineBreak is a hard line break.
	LineBreak
	// Code is a code span, its text in Text.
	Code
	Emphasis
	Strong
	Strikethrough
	// Link holds the inlines of its text, to Dest, titled Title.
	Link
	// Image holds the inlines of its description, from Dest.
	Image
	// HTML is raw HTML, in Text.
	HTML
	// Math is TeX, in Text, Display or within the line.
	Math
	// FootnoteRef refers to the footnote labelled Text.
	FootnoteRef
	// Mention mentions a person or a team, written in Text as @octocat or
	// @github/docs (ExtMentions); Login returns whom.
	Mention
	// Reference refers to an issue or a pull request, written in Text as
	// #123, GH-123 or octocat/hello#123 (ExtReferences); Ref returns
	// which.
	Reference
)

var inlineNames = [...]string{"text", "soft break", "line break", "code", "emphasis", "strong", "strikethrough", "link", "image", "HTML", "math", "footnote reference", "mention", "reference"}

func (k InlineKind) String() string {
	if int(k) < len(inlineNames) {
		return inlineNames[k]
	}
	return "unknown"
}

// Inline is an inline of a block's content: inlines are in document
// order, an inline's children right after it, up to its Next.
type Inline struct {
	Kind InlineKind
	// Text is the literal of Text, Code, HTML and Math, the label of a
	// FootnoteRef, and a Mention or a Reference as written.
	Text string
	// Dest and Title are a link's or an image's.
	Dest, Title string
	// Start and End are the offsets of the inline's bytes in the source.
	Start, End int
	// Next is the index after the inline and its children.
	Next int
	// Auto tells a link written as its address, as <https://…> or
	// www.example.com.
	Auto bool
	// Display tells math shown on its own lines, between $$ or \[ \].
	Display bool
	// Pending tells, while the document is streaming, a link or an image
	// whose destination is still being written.
	Pending bool
}

// Inlines returns the inlines of block i, a paragraph, a heading or a
// table cell, parsing them the first time.
func (d *Document) Inlines(i int) []Inline {
	b := &d.blocks[i]
	if b.inlOK {
		return b.inl
	}
	switch b.Kind {
	case Paragraph, Heading, TableCell:
	default:
		return nil
	}
	b.inlOK = true
	text, _ := d.contentOf(i)
	if b.segB > b.segA && i != d.tail {
		at := int(d.segs[b.segA].start)
		for _, o := range d.oldInl {
			if o.at == at && o.text == text && !o.healed && slices.Equal(o.segs, d.segsOf(b)) {
				b.inl, b.labels, b.rev = o.inl, o.labels, o.rev
				return b.inl
			}
		}
	}
	b.healed = d.streaming && i == d.tail
	if b.healed {
		d.healedAt = i
	}
	b.inl, b.labels = d.inp.parse(d, i, b.healed)
	d.revs++
	b.rev = d.revs
	return b.inl
}

// inode is an inline while the parser builds the tree.
type inode struct {
	kind        InlineKind
	text        string
	dest, title string
	start, end  int
	parent      int32
	first, last int32
	prev, next  int32
	auto        bool
	display     bool
	plain       bool // a text node others may merge into
}

type delim struct {
	c                 byte
	n, orig           int
	node              int32
	prev, next        int32
	canOpen, canClose bool
	at                int
	// used records a run that has already closed a pair. A closer's
	// leftover can open the next pair (`**a*****b**`), and the same two
	// runs close a nested pair (`***a***`); a used run pairs no other way.
	used bool
	// pair tells the pair a used run closed, so its two leftovers, one
	// star each, are the pair's own opening and closing runs.
	pair int32
	// absorbed notes a closer that left a star of content before its
	// closer, which keeps its leftover from closing an outer pair.
	absorbed bool
	// strongOnly marks a closer Typora's strong regex admits but its
	// emphasis regex rejects: a run of three or more stars after a space.
	// The run closes strong with its last two stars and takes part in no
	// emphasis (`***beta1. ***`, record 2026-11-11, newops/c46-6).
	strongOnly bool
}

type bracket struct {
	node         int32
	prev         int32
	prevDelim    int32
	index        int
	image        bool
	bracketAfter bool
}

const maxBackticks = 1000

type inlineParser struct {
	d     *Document
	ext   Extensions
	s     string
	pos   int
	nodes []inode
	dl    []delim
	last  int32 // the last delimiter
	br    []bracket
	lastB int32 // the last bracket
	// linked is the number of brackets when a link last closed: those
	// before it, but for images, open no link, as links do not nest.
	linked int32

	backticks        [maxBackticks + 1]int
	scannedBackticks bool
	html             htmlMemo
	noMath           int // no closing $$ after this offset
	// noDollar, noParen and noBracket are the offsets after which no $,
	// \) or \] closes math, so that openers repeated stay linear.
	noDollar, noParen, noBracket int
	// openMath is where $$ opens math that nothing closes, or -1, and hide
	// a bracket healing leaves out, as the text of a link being written;
	// openDollar is where a single $ opens math (ExtDollarMath) that
	// nothing closes up to the end of the text, or -1.
	openMath, hide, openDollar int
	labels                     []string
	// refBytes are which bytes that may start a mention or a reference
	// the text holds (referenceBytes).
	refBytes uint8
	// leftover are the openers no closer matched, for healing.
	leftover []delim
	// pairs counts the pairs made, naming each for its used runs.
	pairs int32
	probe bool
	out   []Inline
}

func (ip *inlineParser) reset() {}

// parse parses the inlines of block i; heal completes what the end of
// the block leaves open, as the document streams.
func (ip *inlineParser) parse(d *Document, i int, heal bool) ([]Inline, []string) {
	text, _ := d.contentOf(i)
	// Paragraphs and headings are trimmed of whitespace around their
	// text, but Typora keeps the whitespace at their start as text.
	a, b := 0, len(text)
	if d.ext&ExtTyporaContentSpace == 0 {
		for a < b && (text[a] == ' ' || text[a] == '\t' || text[a] == '\n') {
			a++
		}
	}
	trimmed := b
	for b > a && (text[b-1] == ' ' || text[b-1] == '\t' || text[b-1] == '\n') {
		b--
	}
	if b < trimmed && d.ext&ExtTyporaContentSpace != 0 && strings.Contains(text[b:trimmed], "\n") {
		// Typora keeps a whitespace-only line that runs past a list
		// item's indent: the line stays the paragraph's last line, which
		// the soft break shows with the whitespace past the indent
		// (record 2026-11-10, seed6/ordered-softbreak-in-item).
		b = trimmed
	}
	s := text[a:b]
	ip.d, ip.ext = d, d.ext
	ip.labels = nil
	ip.hide = -1
	healFrom := len(s)
	built := false
	if heal {
		s, healFrom, built = ip.heal(s)
	}
	if !built {
		ip.run(s)
	}
	out := ip.flatten(d, i, a, healFrom)
	labels := ip.labels
	ip.labels = nil
	return out, labels
}

// run parses s into the node tree, node 0 its root.
func (ip *inlineParser) run(s string) {
	ip.s, ip.pos = s, 0
	ip.nodes = append(ip.nodes[:0], inode{parent: -1, first: -1, last: -1, prev: -1, next: -1, end: len(s)})
	ip.dl = ip.dl[:0]
	ip.br = ip.br[:0]
	ip.last, ip.lastB, ip.linked = -1, -1, 0
	ip.scannedBackticks = false
	ip.html.reset()
	ip.noMath = -1
	ip.noDollar, ip.noParen, ip.noBracket, ip.openMath, ip.openDollar = -1, -1, -1, -1, -1
	ip.leftover = ip.leftover[:0]
	ip.pairs = 0
	for ip.pos < len(s) {
		ip.parseInline()
	}
	ip.processEmphasis(0, true)
	ip.joinTexts(0)
	if ip.ext&ExtAutolinks != 0 && strings.IndexByte(s, '@') >= 0 {
		ip.emailLinks(0, false)
	}
	if ip.refBytes = referenceBytes(s, ip.ext); ip.refBytes != 0 {
		ip.references(0, 0)
	}
	ip.d.steps += ip.html.steps
}

func (ip *inlineParser) newNode(kind InlineKind, text string, start, end int) int32 {
	ip.nodes = append(ip.nodes, inode{kind: kind, text: text, start: start, end: end, parent: -1, first: -1, last: -1, prev: -1, next: -1})
	return int32(len(ip.nodes) - 1)
}

func (ip *inlineParser) appendChild(parent, child int32) {
	n := &ip.nodes[child]
	n.parent = parent
	p := &ip.nodes[parent]
	n.prev, n.next = p.last, -1
	if p.last >= 0 {
		ip.nodes[p.last].next = child
	} else {
		p.first = child
	}
	p.last = child
}

func (ip *inlineParser) unlink(c int32) {
	n := &ip.nodes[c]
	if n.prev >= 0 {
		ip.nodes[n.prev].next = n.next
	} else if n.parent >= 0 {
		ip.nodes[n.parent].first = n.next
	}
	if n.next >= 0 {
		ip.nodes[n.next].prev = n.prev
	} else if n.parent >= 0 {
		ip.nodes[n.parent].last = n.prev
	}
	n.parent, n.prev, n.next = -1, -1, -1
}

func (ip *inlineParser) insertAfter(c, sib int32) {
	ip.unlink(sib)
	n := &ip.nodes[c]
	s := &ip.nodes[sib]
	s.parent, s.prev, s.next = n.parent, c, n.next
	if n.next >= 0 {
		ip.nodes[n.next].prev = sib
	} else if n.parent >= 0 {
		ip.nodes[n.parent].last = sib
	}
	ip.nodes[c].next = sib
}

func (ip *inlineParser) insertBefore(c, sib int32) {
	ip.unlink(sib)
	n := &ip.nodes[c]
	s := &ip.nodes[sib]
	s.parent, s.next, s.prev = n.parent, c, n.prev
	if n.prev >= 0 {
		ip.nodes[n.prev].next = sib
	} else if n.parent >= 0 {
		ip.nodes[n.parent].first = sib
	}
	ip.nodes[c].prev = sib
}

// addText adds the subject's [a, b) as text, merged into the text before
// it when that ends at a.
func (ip *inlineParser) addText(a, b int) {
	if a >= b {
		return
	}
	if l := ip.nodes[0].last; l >= 0 {
		n := &ip.nodes[l]
		if n.kind == Text && n.plain && n.end == a && n.text == ip.s[n.start:n.end] {
			n.end = b
			n.text = ip.s[n.start:b]
			return
		}
	}
	c := ip.newNode(Text, ip.s[a:b], a, b)
	ip.nodes[c].plain = true
	ip.appendChild(0, c)
}

// addLiteral adds text that is not the subject's bytes [a, b), as an
// escaped character's.
func (ip *inlineParser) addLiteral(text string, a, b int) int32 {
	c := ip.newNode(Text, text, a, b)
	ip.appendChild(0, c)
	return c
}

var special = func() (t [256]bool) {
	for _, c := range "\n\\`*_[]!<&~$:w" {
		t[c] = true
	}
	return
}()

func (ip *inlineParser) parseInline() {
	s := ip.s
	c := s[ip.pos]
	switch c {
	case '\n':
		ip.parseNewline()
	case '\\':
		ip.parseBackslash()
	case '`':
		ip.parseBackticks()
	case '*', '_':
		ip.handleDelim(c)
	case '~':
		if ip.ext&ExtStrikethrough != 0 {
			ip.handleDelim(c)
		} else {
			ip.addText(ip.pos, ip.pos+1)
			ip.pos++
		}
	case '[':
		text := "["
		if ip.pos == ip.hide {
			text = ""
		}
		n := ip.addLiteral(text, ip.pos, ip.pos+1)
		ip.addBracket(n, ip.pos+1, false)
		ip.pos++
	case '!':
		if ip.pos+1 < len(s) && s[ip.pos+1] == '[' {
			text := "!["
			if ip.pos == ip.hide {
				text = ""
			}
			n := ip.addLiteral(text, ip.pos, ip.pos+2)
			ip.addBracket(n, ip.pos+2, true)
			ip.pos += 2
		} else {
			ip.addText(ip.pos, ip.pos+1)
			ip.pos++
		}
	case ']':
		ip.parseCloseBracket()
	case '<':
		ip.parseAngle()
	case '&':
		if n, text := scanEntity(s, ip.pos); n > 0 {
			ip.addLiteral(text, ip.pos, ip.pos+n)
			ip.pos += n
		} else {
			ip.addText(ip.pos, ip.pos+1)
			ip.pos++
		}
	case '$':
		if ip.ext&ExtMath == 0 || !ip.parseDollar() {
			ip.addText(ip.pos, ip.pos+1)
			ip.pos++
		}
	case ':':
		if ip.ext&ExtAutolinks == 0 || !ip.urlLink() {
			ip.addText(ip.pos, ip.pos+1)
			ip.pos++
		}
	case 'w':
		if ip.ext&ExtAutolinks == 0 || !ip.wwwLink() {
			ip.parseString()
		}
	default:
		ip.parseString()
	}
}

// parseString adds the text up to the next byte that may start syntax.
func (ip *inlineParser) parseString() {
	s := ip.s
	j := ip.pos + 1
	for j < len(s) {
		c := s[j]
		if special[c] {
			if c == 'w' && (!strings.HasPrefix(s[j:], "www.") || ip.ext&ExtAutolinks == 0) {
				j++
				continue
			}
			if c == ':' && (!strings.HasPrefix(s[j:], "://") || ip.ext&ExtAutolinks == 0) {
				j++
				continue
			}
			if c == '$' && ip.ext&ExtMath == 0 {
				j++
				continue
			}
			if c == '~' && ip.ext&ExtStrikethrough == 0 {
				j++
				continue
			}
			break
		}
		j++
	}
	ip.d.steps += j - ip.pos
	ip.addText(ip.pos, j)
	ip.pos = j
}

func (ip *inlineParser) parseNewline() {
	ip.pos++
	hard := false
	if l := ip.nodes[0].last; l >= 0 {
		n := &ip.nodes[l]
		if n.kind == Text && strings.HasSuffix(n.text, " ") {
			t := strings.TrimRight(n.text, " ")
			hard = len(n.text)-len(t) >= 2
			n.text = t
			if n.plain {
				n.end = n.start + len(t)
			}
		}
	}
	kind := SoftBreak
	if hard {
		kind = LineBreak
	}
	c := ip.newNode(kind, "", ip.pos-1, ip.pos)
	ip.appendChild(0, c)
	if ip.ext&ExtTyporaContentSpace == 0 {
		// Typora 把续行行首超出容器缩进的空白当作内容：跳过它会让投影
		// 无法区分结构缩进和内容空白（记录 2026-11-11，seed9/c9）。
		for ip.pos < len(ip.s) && (ip.s[ip.pos] == ' ' || ip.s[ip.pos] == '\t') {
			ip.pos++
		}
	}
}

func (ip *inlineParser) parseBackslash() {
	s := ip.s
	ip.pos++
	if ip.pos < len(s) {
		switch c := s[ip.pos]; {
		case c == '\n':
			ip.pos++
			n := ip.newNode(LineBreak, "", ip.pos-2, ip.pos)
			ip.appendChild(0, n)
			if ip.ext&ExtTyporaContentSpace == 0 {
				for ip.pos < len(s) && (s[ip.pos] == ' ' || s[ip.pos] == '\t') {
					ip.pos++
				}
			}
			return
		case (c == '(' || c == '[') && ip.ext&ExtMath != 0:
			end, none := `\)`, &ip.noParen
			if c == '[' {
				end, none = `\]`, &ip.noBracket
			}
			k := -1
			if *none < 0 || ip.pos < *none {
				if k = strings.Index(s[ip.pos+1:], end); k < 0 {
					*none = ip.pos
					ip.d.steps += len(s) - ip.pos - 1
				} else {
					ip.d.steps += k
				}
			}
			if k > 0 {
				tex := s[ip.pos+1 : ip.pos+1+k]
				n := ip.newNode(Math, tex, ip.pos-1, ip.pos+1+k+2)
				ip.nodes[n].display = c == '['
				ip.appendChild(0, n)
				ip.pos += 1 + k + 2
				return
			}
			ip.addLiteral(s[ip.pos:ip.pos+1], ip.pos-1, ip.pos+1)
			ip.pos++
			return
		case isEscapable(c):
			ip.addLiteral(s[ip.pos:ip.pos+1], ip.pos-1, ip.pos+1)
			ip.pos++
			return
		}
	}
	ip.addText(ip.pos-1, ip.pos)
}

func (ip *inlineParser) parseBackticks() {
	s := ip.s
	start := ip.pos
	for ip.pos < len(s) && s[ip.pos] == '`' {
		ip.pos++
	}
	n := ip.pos - start
	after := ip.pos
	if cs, m := ip.scanCloseBackticks(n); m > 0 {
		// Both fences are as wide as the shorter of the two runs, and the rest
		// of a longer opening run is the span's text, as Typora reads a run of
		// three backticks followed by a run of one (ExtTyporaCodeSpan).
		d := n
		if m < n {
			d = m
		}
		content := s[start+d : cs]
		if strings.IndexByte(content, '\n') >= 0 {
			content = strings.ReplaceAll(content, "\n", " ")
		}
		if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' && strings.Trim(content, " ") != "" {
			content = content[1 : len(content)-1]
		}
		c := ip.newNode(Code, content, start, cs+d)
		ip.appendChild(0, c)
		ip.pos = cs + d
		return
	}
	ip.pos = after
	ip.addText(start, after)
}

// CodeFence returns the width of the two runs of backticks that fence the code
// span n in src. They are equal but for ExtTyporaCodeSpan, where a span's
// opener is as wide as the shorter run: Typora fences a run of three backticks
// and a run of one with one backtick on either side, and reads the opening
// run's other two backticks as the span's text (record 2026-11-12, seed18/c1,
// /c7).
func CodeFence(src string, n *Inline) int {
	open, closing := 0, 0
	for n.Start+open < n.End && src[n.Start+open] == '`' {
		open++
	}
	for n.End-closing > n.Start && src[n.End-closing-1] == '`' {
		closing++
	}
	if closing < open {
		return closing
	}
	return open
}

// scanCloseBackticks returns where the run that closes a span of n backticks
// from pos starts and how long that run is, or a length of 0, remembering the
// last run of each length so that unmatched openers do not take quadratic
// time, as cmark does. A run of its own length closes the span, as CommonMark
// reads it. Typora's dialect (ExtTyporaCodeSpan) closes the span with the next
// run of any length when no run of its own length follows: a run of three
// backticks and a later run of one are a one backtick span whose text is the
// opening run's other two backticks (record 2026-11-12, seed18/c1, /c7).
func (ip *inlineParser) scanCloseBackticks(n int) (int, int) {
	if n > maxBackticks {
		return 0, 0
	}
	if ip.scannedBackticks && ip.backticks[n] <= ip.pos {
		if ip.ext&ExtTyporaCodeSpan == 0 {
			return 0, 0
		}
		// An earlier scan reached this text's end without a run of n
		// backticks; the dialect still takes the next run it finds.
		if i := strings.IndexByte(ip.s[ip.pos:], '`'); i >= 0 {
			at := ip.pos + i
			end := at
			for end < len(ip.s) && ip.s[end] == '`' {
				end++
			}
			return at, end - at
		}
		return 0, 0
	}
	s := ip.s
	i := ip.pos
	first, firstLen := -1, 0
	for {
		k := strings.IndexByte(s[i:], '`')
		if k < 0 {
			break
		}
		i += k
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		m := j - i
		if m <= maxBackticks {
			ip.backticks[m] = i
		}
		if m == n {
			ip.d.steps += j - ip.pos
			return i, m
		}
		if first < 0 {
			first, firstLen = i, m
		}
		i = j
	}
	ip.d.steps += len(s) - ip.pos
	ip.scannedBackticks = true
	if ip.ext&ExtTyporaCodeSpan != 0 {
		return first, firstLen
	}
	return 0, 0
}

// parseDollar reads math between $$ (or $ with DollarMath) at pos.
func (ip *inlineParser) parseDollar() bool {
	s := ip.s
	start := ip.pos
	if strings.HasPrefix(s[start:], "$$") {
		if ip.noMath >= 0 && start >= ip.noMath {
			return false
		}
		k := strings.Index(s[start+2:], "$$")
		if k < 0 {
			ip.d.steps += len(s) - start - 2
			ip.noMath = start
			if ip.openMath < 0 {
				ip.openMath = start
			}
			return false
		}
		ip.d.steps += k
		if strings.Trim(s[start+2:start+2+k], " \t\n") == "" {
			return false
		}
		n := ip.newNode(Math, s[start+2:start+2+k], start, start+2+k+2)
		ip.nodes[n].display = true
		ip.appendChild(0, n)
		ip.pos = start + 2 + k + 2
		return true
	}
	if ip.ext&ExtDollarMath == 0 {
		return false
	}
	// A dollar followed by a space or a digit is money, not math.
	if start+1 >= len(s) || s[start+1] == ' ' || s[start+1] == '\t' || s[start+1] == '\n' || isASCIIDigit(s[start+1]) {
		return false
	}
	if ip.noDollar >= 0 && start >= ip.noDollar {
		return false
	}
	j := start + 1
	defer func() {
		ip.d.steps += j - start
		if ip.pos == start {
			ip.noDollar = start
		}
	}()
	for ; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '\n':
			if j+1 < len(s) && s[j+1] == '\n' {
				return false
			}
		case '$':
			if s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || j+1 < len(s) && isASCIIDigit(s[j+1]) {
				continue
			}
			n := ip.newNode(Math, s[start+1:j], start, j+1)
			ip.appendChild(0, n)
			ip.pos = j + 1
			return true
		}
	}
	if ip.openDollar < 0 {
		ip.openDollar = start
	}
	return false
}

// parseAngle reads an autolink or raw HTML at <.
func (ip *inlineParser) parseAngle() {
	s := ip.s
	if n, dest, email := scanAutolink(s, ip.pos); n > 0 {
		inner := s[ip.pos+1 : ip.pos+n-1]
		l := ip.newNode(Link, "", ip.pos, ip.pos+n)
		ip.nodes[l].dest = dest
		ip.nodes[l].auto = true
		_ = email
		ip.appendChild(0, l)
		t := ip.newNode(Text, inner, ip.pos+1, ip.pos+n-1)
		ip.appendChild(l, t)
		ip.pos += n
		return
	}
	if n := scanHTMLTag(s, ip.pos, &ip.html); n > 0 {
		h := ip.newNode(HTML, s[ip.pos:ip.pos+n], ip.pos, ip.pos+n)
		ip.appendChild(0, h)
		ip.pos += n
		return
	}
	ip.addText(ip.pos, ip.pos+1)
	ip.pos++
}

// scanAutolink reads <scheme:…> or <email> at i: its length, and its
// destination.
func scanAutolink(s string, i int) (int, string, bool) {
	j := i + 1
	// A URI: a scheme of 2 to 32 characters, a colon, no spaces or <>.
	k := j
	for k < len(s) && k-j < 33 && (isASCIILetter(s[k]) || k > j && (isASCIIDigit(s[k]) || s[k] == '+' || s[k] == '.' || s[k] == '-')) {
		k++
	}
	if n := k - j; n >= 2 && n <= 32 && k < len(s) && s[k] == ':' {
		e := k + 1
		for e < len(s) && s[e] > ' ' && s[e] != '<' && s[e] != '>' {
			e++
		}
		if e < len(s) && s[e] == '>' {
			return e + 1 - i, s[j:e], false
		}
	}
	// An email address.
	k = j
	for k < len(s) && (isASCIILetter(s[k]) || isASCIIDigit(s[k]) || strings.IndexByte(".!#$%&'*+/=?^_`{|}~-", s[k]) >= 0) {
		k++
	}
	if k == j || k >= len(s) || s[k] != '@' {
		return 0, "", false
	}
	e := k + 1
	for {
		// A label: alphanumerics and inner hyphens, at most 63.
		l := e
		for e < len(s) && (isASCIILetter(s[e]) || isASCIIDigit(s[e]) || s[e] == '-') && e-l < 64 {
			e++
		}
		if e == l || e-l > 63 || s[l] == '-' || s[e-1] == '-' {
			return 0, "", false
		}
		if e < len(s) && s[e] == '.' {
			e++
			continue
		}
		break
	}
	if e < len(s) && s[e] == '>' {
		return e + 1 - i, "mailto:" + s[j:e], true
	}
	return 0, "", false
}

// scanDelims reads a run of delimiter c at pos: its length, and whether
// it can open and close emphasis.
func (ip *inlineParser) scanDelims(c byte) (n int, canOpen, canClose, strongOnly bool) {
	s := ip.s
	i := ip.pos
	for i < len(s) && s[i] == c {
		i++
	}
	n = i - ip.pos
	before, after := '\n', '\n'
	if ip.pos > 0 {
		before, _ = utf8.DecodeLastRuneInString(s[:ip.pos])
	}
	if i < len(s) {
		after, _ = utf8.DecodeRuneInString(s[i:])
	}
	afterSpace, afterPunct := isUnicodeSpace(after), isPunct(after)
	beforeSpace, beforePunct := isUnicodeSpace(before), isPunct(before)
	left := !afterSpace && (!afterPunct || beforeSpace || beforePunct)
	right := !beforeSpace && (!beforePunct || afterSpace || afterPunct)
	// Next to CJK text, punctuation such as 。 or “ doesn't stop * or ~~
	// from opening or closing: Chinese writes **结论。**后文 with no
	// space, which strict CommonMark would leave as literal asterisks.
	// A single tilde keeps the rule: it is more often a range, as
	// 第1章~“附录”, than strikethrough.
	if ip.ext != 0 && ip.ext&ExtTyporaEmphasis == 0 && (c == '*' || c == '~' && n > 1) && (isCJK(before) || isCJK(after)) {
		left, right = !afterSpace, !beforeSpace
	}
	switch c {
	case '_':
		canOpen = left && (!right || beforePunct)
		canClose = right && (!left || afterPunct)
	case '~':
		if ip.ext&ExtDoubleTildeOnly != 0 && n == 1 {
			canOpen, canClose = false, false
			break
		}
		if ip.ext&ExtDoubleTildeOnly != 0 && n == 2 {
			// Typora's del regex needs only a non-space after the opening
			// `~~` and before the closing one, not CommonMark's flanking
			// against punctuation.
			canOpen, canClose = !afterSpace, !beforeSpace
			break
		}
		canOpen, canClose = left, right
		// A tilde between letters or digits is not strikethrough, as
		// 20~25, which language models write for ranges.
		if n == 1 && isAlnum(before) && isAlnum(after) {
			canOpen, canClose = false, false
		}
	default:
		if c == '*' && ip.ext&ExtTyporaEmphasis != 0 {
			canOpen, canClose = !afterSpace, !beforeSpace
			if !canClose && n >= 2 {
				// Typora 的强闭 run 前是空白也仍能闭合：`***beta1. ***`
				// 的尾 run 闭在末两颗上（记录 2026-11-11，newops/c46-6），
				// `alpha**- **rs**` 的前两颗与中间 run 配对、末 run 留字面，
				// `_> **- **` 的 `- ` 成为强内容（记录 2026-11-13，
				// seed21/c3、seed21/c10）。Typora 的 em 正则要求闭 run 前
				// 是非空白，这种 run 只参与 strong。
				canClose, strongOnly = true, true
			}
		} else {
			canOpen, canClose = left, right
		}
	}
	return
}

// isCJK reports whether r is a Chinese, Japanese or Korean character,
// or the full-width punctuation written with them.
func isCJK(r rune) bool {
	if r < 0x2E80 {
		return r >= 0x2018 && r <= 0x201F // curly quotes, used in CJK text
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		r >= 0x3000 && r <= 0x303F || // CJK symbols and punctuation
		r >= 0xFF00 && r <= 0xFFEF // full-width forms
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func isUnicodeSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || unicode.Is(unicode.Zs, r)
}

// isPunct reports whether r is punctuation or a symbol, as CommonMark
// counts them for flanking.
func isPunct(r rune) bool {
	if r < utf8.RuneSelf {
		return isEscapable(byte(r))
	}
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func (ip *inlineParser) handleDelim(c byte) {
	n, canOpen, canClose, strongOnly := ip.scanDelims(c)
	start := ip.pos
	if c == '*' && ip.ext&ExtTyporaEmptyStrong != 0 && start == 0 && n == 4 && canOpen {
		// A paragraph's leading `****` followed by text is an empty strong
		// element: Typora reads the first two stars as its opening run and
		// the last two as its closing one, which the text after them does
		// not disturb (`****x**`, record 2026-11-10, probe75/h).
		ip.appendChild(0, ip.newNode(Strong, "", start, start+n))
		ip.pos += n
		return
	}
	ip.pos += n
	node := ip.addLiteral(ip.s[start:ip.pos], start, ip.pos)
	if c == '~' && n > 2 {
		return
	}
	if canOpen || canClose {
		ip.dl = append(ip.dl, delim{c: c, n: n, orig: n, node: node, prev: ip.last, next: -1, canOpen: canOpen, canClose: canClose, at: start, strongOnly: strongOnly})
		k := int32(len(ip.dl) - 1)
		if ip.last >= 0 {
			ip.dl[ip.last].next = k
		}
		ip.last = k
	}
}

func (ip *inlineParser) removeDelim(k int32) {
	d := &ip.dl[k]
	if d.prev >= 0 {
		ip.dl[d.prev].next = d.next
	}
	if d.next >= 0 {
		ip.dl[d.next].prev = d.prev
	} else {
		ip.last = d.prev
	}
	d.prev, d.next = -1, -1
	d.n = -1 // removed
}

// processEmphasis matches the delimiters at or after offset bottom, as
// CommonMark's algorithm does, openersBottom keeping it linear; final
// notes the openers left unmatched, for healing.
func (ip *inlineParser) processEmphasis(bottom int, final bool) {
	var openersBottom [3][6]int
	for i := range openersBottom {
		for j := range openersBottom[i] {
			openersBottom[i][j] = bottom
		}
	}
	closer := int32(-1)
	for c := ip.last; c >= 0 && ip.dl[c].at >= bottom; c = ip.dl[c].prev {
		closer = c
		ip.d.steps++
	}
	for closer >= 0 {
		ip.d.steps++
		cd := &ip.dl[closer]
		if !cd.canClose {
			closer = cd.next
			continue
		}
		ci := delimClass(cd.c, cd.canOpen)
		opener := cd.prev
		found := false
		for opener >= 0 && ip.dl[opener].at >= openersBottom[cd.orig%3][ci] {
			ip.d.steps++
			od := &ip.dl[opener]
			// Typora rejects a match whose run lengths sum to a
			// multiple of three unless both are, as CommonMark does:
			// `****x**` stays literal and `*jf**hkrd***` nests
			// (record 2026-11-09, seed5/c3, seed5/c5). A `**` run that
			// found no closing `**` still opens a one-star emphasis:
			// Typora's regex takes the run's first star as the opener
			// and the second as content, so `**bold*x` keeps the
			// emphasis (record 2026-11-09, p0/inline/bold/backspace-close).
			typora := ip.ext&ExtTyporaEmphasis != 0
			fallbackEm := typora && od.orig == 2 && cd.orig == 1
			use := 1
			if cd.n >= 2 && od.n >= 2 {
				use = 2
			}
			odd := false
			if !fallbackEm && cd.orig%3 != 0 && (od.orig+cd.orig)%3 == 0 {
				if use == 2 && typora {
					// Typora's strong reader splits a longer closer run and
					// pairs the opener with its middle two stars, one star
					// joining the content and the rest staying literal:
					// `x**a****b**` reads as strong `a*` (record 2026-11-12,
					// probe116/ascii-ab). A long opener that precedes a bare
					// `**` closer still keeps both runs literal, as
					// CommonMark's rule of three rejects the match
					// (`alpha bej****x**`, seed5/c3).
					odd = cd.n < 3
				} else {
					odd = cd.canOpen || od.canClose
				}
			}
			if od.c == cd.c && od.canOpen && !odd &&
				// 空白之后的 `***` 尾 run 只闭 strong，余下的单星
				// 不再闭外层 em（`***beta1. ***`）。
				!(use == 1 && cd.strongOnly) &&
				(!typora || !ip.usedDelimsBlock(od, cd, use)) &&
				(!typora || !ip.adjacentStrongGroup(opener, closer, use)) &&
				!(fallbackEm && ip.laterStrongCloser(od, cd)) {
				found = true
				break
			}
			opener = od.prev
		}
		if !found {
			old := closer
			closer = cd.next
			openersBottom[ip.dl[old].orig%3][ci] = ip.dl[old].at
			if !ip.dl[old].canOpen {
				ip.removeDelim(old)
			}
			continue
		}
		if cd.c == '~' {
			closer = ip.insertStrike(opener, closer)
			continue
		}
		od, cd := &ip.dl[opener], &ip.dl[closer]
		use := 1
		if cd.n >= 2 && od.n >= 2 {
			use = 2
		}
		if use == 1 && ip.ext&ExtTyporaEmphasis != 0 && cd.n >= 3 {
			// Typora 的单星闭合取 run 的末颗星，前面的星留在强调内容
			// 里：`*beta> 试***` 的强调内容是 `beta> 试**`，闭合星
			// 记在 run 末颗（记录 2026-11-13，seed21/c15；
			// probe75/o 的 `*a***b*` 同形）。整条 run 交给
			// insertEmph 会从 run 头收星，把内容读成 `beta> 试`，
			// 光标停在末颗时也就落到了 pair 之外。先把前面的星
			// 切成字面节点，pair 只拿走末颗。
			head, node := cd.n-1, cd.node
			text, at := ip.nodes[node].text, ip.nodes[node].start
			if head > 0 && head < len(text) {
				ip.insertBefore(node, ip.newNode(Text, text[:head], at, at+head))
				tail := &ip.nodes[node]
				tail.text, tail.start = text[head:], at+head
				cd.at += head
				cd.n = 1
			}
		}
		closer = ip.insertEmph(opener, closer)
	}
	// What remains matched nothing.
	for ip.last >= 0 && ip.dl[ip.last].at >= bottom {
		ip.d.steps++
		if d := ip.dl[ip.last]; final && d.canOpen && d.n > 0 {
			ip.leftover = append(ip.leftover, d)
		}
		ip.removeDelim(ip.last)
	}
}

// usedDelimsBlock tells whether a used delimiter keeps the two from
// pairing. A run that closed a pair leaves at most the pair's own one-star
// opening and closing runs, which close the pair's outer emphasis
// (`***a***`), and leftovers of two or more stars that open the next strong
// (`**a*****b**`). A used run pairs in no other way: `**a***b**` leaves a
// literal star, not a second pair (record 2026-11-10, probe75/a, /d, /l).
func (ip *inlineParser) usedDelimsBlock(od, cd *delim, use int) bool {
	if !od.used && !cd.used {
		return false
	}
	if use == 1 {
		if od.used {
			return !(cd.used && od.pair == cd.pair && od.n == 1 && cd.n == 1 &&
				!od.absorbed && !cd.absorbed)
		}
		// A used one-star closer still closes an outer emphasis the
		// strong inside it left open (`*jf**hkrd***`, seed5/c5).
		return cd.n != 1
	}
	return !(od.used && od.n >= 2)
}

// laterStrongCloser tells whether a `**` opener and a one-star closer have a
// later `**` run to close instead. Typora prefers the strong pair:
// `**bold*x` keeps the fallback emphasis, but `**a*b**` closes one strong
// across the literal star (record 2026-11-09, p0/inline/bold/backspace-close;
// record 2026-11-10, probe75/q).
// adjacentStrongGroup reports a fresh `****` run that would pair across the
// boundary Typora left after a `****` run closed strong. Typora's live editor
// keeps each self-pasted `****` group's stars literal instead of pairing the
// next group's opening and closing runs (record 2026-11-14, seed23/c6).
func (ip *inlineParser) adjacentStrongGroup(opener, closer int32, use int) bool {
	if ip.ext&ExtTyporaEmphasis == 0 || use != 2 {
		return false
	}
	od, cd := &ip.dl[opener], &ip.dl[closer]
	if od.c != '*' || cd.c != '*' || od.used || cd.used ||
		od.orig != 4 || od.n != 4 || cd.orig != 4 || cd.n != 4 ||
		cd.prev != opener || od.prev < 0 {
		return false
	}
	boundary := &ip.dl[od.prev]
	return boundary.c == '*' && boundary.used && boundary.absorbed &&
		boundary.orig == 4 && boundary.n == 1
}

func (ip *inlineParser) laterStrongCloser(od, cd *delim) bool {
	for k := cd.next; k >= 0; k = ip.dl[k].next {
		ip.d.steps++
		d := &ip.dl[k]
		if d.c != od.c || d.n < 2 || !d.canClose {
			continue
		}
		if (d.canOpen || od.canClose) && d.orig%3 != 0 && (od.orig+d.orig)%3 == 0 {
			continue
		}
		return true
	}
	return false
}

// outerStarOpener tells whether a fresh opener before opener closes with
// the one star the closer would leave after the strong. Typora gives that
// star to the outer emphasis rather than making it content, so the strong
// ends on the closer's first two stars: `*jf**hkrd***` keeps the emphasis
// around the strong (record 2026-11-09, seed5/c5).
func (ip *inlineParser) outerStarOpener(opener int32, cd *delim) bool {
	for k := ip.dl[opener].prev; k >= 0; k = ip.dl[k].prev {
		ip.d.steps++
		d := &ip.dl[k]
		if d.c == cd.c && d.n >= 1 && d.canOpen && !d.used {
			return true
		}
	}
	return false
}

func delimClass(c byte, canOpen bool) int {
	k := 0
	switch c {
	case '_':
		k = 2
	case '~':
		k = 4
	}
	if canOpen {
		k++
	}
	return k
}

// insertEmph wraps what is between opener and closer in emphasis, one or
// two delimiters of each, and returns the next closer to look at.
func (ip *inlineParser) insertEmph(opener, closer int32) int32 {
	od, cd := &ip.dl[opener], &ip.dl[closer]
	use := 1
	if cd.n >= 2 && od.n >= 2 {
		use = 2
	}
	skip := 0
	if use == 2 && ip.ext&ExtTyporaEmphasis != 0 && cd.n >= 3 &&
		!(od.n-use == 1 && cd.n-use == 1) &&
		!(cd.n-use == 1 && ip.outerStarOpener(opener, cd)) {
		// Typora closes a strong run one star in: the star before the
		// closer is content, and what follows the closer opens the next
		// pair, as in `**a***b**` (record 2026-11-10, probe75/a). The two
		// runs of a nested pair close whole: `***a***` gives its strong
		// the run's first two stars and its emphasis the last (probe75/d).
		skip = 1
	}
	on, cn := od.node, cd.node
	if skip > 0 {
		text, at := ip.nodes[cn].text, ip.nodes[cn].start
		ip.insertBefore(cn, ip.newNode(Text, text[:skip], at, at+skip))
	}
	od.n -= use
	cd.n -= use + skip
	o, c := &ip.nodes[on], &ip.nodes[cn]
	o.text = o.text[:len(o.text)-use]
	o.end -= use
	c.text = c.text[skip+use:]
	c.start += skip + use
	kind := Emphasis
	if use == 2 {
		kind = Strong
	}
	pair := ip.pairs
	ip.pairs++
	od.used, od.pair = true, pair
	cd.used, cd.pair, cd.absorbed = true, pair, skip > 0
	emph := ip.newNode(kind, "", ip.nodes[on].end, ip.nodes[cn].start)
	for t := ip.nodes[on].next; t >= 0 && t != cn; {
		ip.d.steps++
		next := ip.nodes[t].next
		ip.unlink(t)
		ip.appendChild(emph, t)
		t = next
	}
	ip.insertAfter(on, emph)
	// The delimiters between are gone.
	for k := ip.dl[closer].prev; k >= 0 && k != opener; {
		ip.d.steps++
		prev := ip.dl[k].prev
		ip.removeDelim(k)
		k = prev
	}
	if ip.dl[opener].n == 0 {
		ip.unlink(on)
		ip.removeDelim(opener)
	}
	if ip.dl[closer].n == 0 {
		ip.unlink(cn)
		next := ip.dl[closer].next
		ip.removeDelim(closer)
		return next
	}
	return closer
}

// insertStrike makes strikethrough of what is between two runs of
// tildes of the same length.
func (ip *inlineParser) insertStrike(opener, closer int32) int32 {
	next := ip.dl[closer].next
	od, cd := ip.dl[opener], ip.dl[closer]
	if od.n == cd.n {
		on, cn := od.node, cd.node
		st := ip.newNode(Strikethrough, "", ip.nodes[on].start, ip.nodes[cn].end)
		for t := ip.nodes[on].next; t >= 0 && t != cn; {
			ip.d.steps++
			n := ip.nodes[t].next
			ip.unlink(t)
			ip.appendChild(st, t)
			t = n
		}
		ip.insertAfter(on, st)
		ip.unlink(on)
		ip.unlink(cn)
	}
	for k := closer; k >= 0 && k != opener; {
		ip.d.steps++
		prev := ip.dl[k].prev
		ip.removeDelim(k)
		k = prev
	}
	ip.removeDelim(opener)
	return next
}

func (ip *inlineParser) addBracket(node int32, index int, image bool) {
	if ip.lastB >= 0 {
		ip.br[ip.lastB].bracketAfter = true
	}
	ip.br = append(ip.br, bracket{node: node, prev: ip.lastB, prevDelim: ip.last, index: index, image: image})
	ip.lastB = int32(len(ip.br) - 1)
}

func (ip *inlineParser) popBracket() {
	ip.lastB = ip.br[ip.lastB].prev
}

func (ip *inlineParser) parseCloseBracket() {
	s := ip.s
	ip.pos++
	start := ip.pos // after ]
	if ip.lastB < 0 {
		ip.addText(start-1, start)
		return
	}
	op := ip.br[ip.lastB]
	if !op.image && ip.lastB < ip.linked {
		ip.addText(start-1, start)
		ip.popBracket()
		return
	}
	var dest, title string
	matched := false
	// An inline link.
	if ip.pos < len(s) && s[ip.pos] == '(' {
		save := ip.pos
		ip.pos++
		ip.spnl()
		if d, ok := ip.parseLinkDestination(); ok {
			dest = d
			before := ip.pos
			ip.spnl()
			ok := true
			if ip.pos > before {
				if t, tok := ip.parseLinkTitle(); tok {
					title = t
				} else {
					ip.pos = before
					ip.spnl()
				}
			}
			ip.spnl()
			if ok && ip.pos < len(s) && s[ip.pos] == ')' {
				ip.pos++
				matched = true
			}
		}
		if !matched {
			ip.pos = save
		}
	}
	if !matched {
		// A reference: [text][label], [text][] or [text].
		before := ip.pos
		n := ip.parseLinkLabel()
		var label string
		has := false
		if n > 2 {
			label, has = s[before:before+n], true
		} else if !op.bracketAfter {
			label, has = s[op.index-1:start], true
		}
		if n == 0 {
			ip.pos = start
		}
		if has {
			ip.d.steps += len(label)
			key := normalizeLabel(label[1 : len(label)-1])
			if key != "" {
				ip.noteLabel(key)
				if r, ok := ip.d.refs[key]; ok {
					dest, title, matched = r.dest, r.title, true
				}
			}
		}
	}
	if matched {
		kind := Link
		if op.image {
			kind = Image
		}
		openStart := ip.nodes[op.node].start
		l := ip.newNode(kind, "", openStart, ip.pos)
		ip.nodes[l].dest, ip.nodes[l].title = dest, title
		for t := ip.nodes[op.node].next; t >= 0; {
			ip.d.steps++
			next := ip.nodes[t].next
			ip.unlink(t)
			ip.appendChild(l, t)
			t = next
		}
		ip.appendChild(0, l)
		ip.processEmphasis(op.index, false)
		ip.popBracket()
		ip.unlink(op.node)
		if !op.image {
			// The brackets open before it are below len(ip.br), as
			// brackets are added at the end.
			ip.linked = int32(len(ip.br))
		}
		return
	}
	// A footnote reference: [^label], when the footnote is defined.
	if ip.ext&ExtFootnotes != 0 && !op.image && !op.bracketAfter && op.index < start-1 && s[op.index] == '^' && start-1 > op.index+1 && start-1-op.index <= 1000 {
		ip.d.steps += start - 1 - op.index
		key := normalizeLabel(s[op.index+1 : start-1])
		ip.noteLabel("^" + key)
		if _, ok := ip.d.notes[key]; ok {
			ip.pos = start
			f := ip.newNode(FootnoteRef, s[op.index+1:start-1], ip.nodes[op.node].start, start)
			ip.processEmphasis(op.index, false)
			for t := ip.nodes[op.node].next; t >= 0; {
				ip.d.steps++
				next := ip.nodes[t].next
				ip.unlink(t)
				t = next
			}
			ip.insertBefore(op.node, f)
			ip.unlink(op.node)
			ip.popBracket()
			return
		}
	}
	ip.popBracket()
	ip.pos = start
	ip.addText(start-1, start)
}

func (ip *inlineParser) noteLabel(key string) {
	if !containsString(ip.labels, key) {
		ip.labels = append(ip.labels, key)
	}
}

// spnl skips spaces and tabs, and one line ending with those around it.
func (ip *inlineParser) spnl() {
	s := ip.s
	for ip.pos < len(s) && (s[ip.pos] == ' ' || s[ip.pos] == '\t') {
		ip.pos++
	}
	if ip.pos < len(s) && s[ip.pos] == '\n' {
		ip.pos++
		for ip.pos < len(s) && (s[ip.pos] == ' ' || s[ip.pos] == '\t') {
			ip.pos++
		}
	}
}

// parseLinkLabel returns the length of a link label at pos, [ and ]
// included, or 0.
func (ip *inlineParser) parseLinkLabel() int {
	s := ip.s
	i := ip.pos
	if i >= len(s) || s[i] != '[' {
		return 0
	}
	j := i + 1
	for j < len(s) && j-i <= 1001 {
		switch s[j] {
		case '\\':
			j++
		case '[':
			ip.d.steps += j - i
			return 0
		case ']':
			ip.d.steps += j - i
			if j-i > 1000 {
				return 0
			}
			ip.pos = j + 1
			return j + 1 - i
		}
		j++
	}
	ip.d.steps += j - i
	return 0
}

// parseLinkDestination reads a link destination at pos, <…> or bare
// with balanced parentheses.
func (ip *inlineParser) parseLinkDestination() (string, bool) {
	s := ip.s
	i := ip.pos
	if i < len(s) && s[i] == '<' {
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				if j+1 < len(s) && isEscapable(s[j+1]) {
					j++
				}
			case '\n', '<':
				ip.d.steps += j - i
				return "", false
			case '>':
				ip.d.steps += j - i
				ip.pos = j + 1
				return unescapeString(s[i+1 : j]), true
			}
		}
		ip.d.steps += len(s) - i
		return "", false
	}
	parens := 0
	j := i
loop:
	for j < len(s) {
		c := s[j]
		switch {
		case c == '\\' && j+1 < len(s) && isEscapable(s[j+1]):
			j += 2
		case c == '(':
			parens++
			if parens > 32 {
				ip.d.steps += j - i
				return "", false
			}
			j++
		case c == ')':
			if parens < 1 {
				break loop
			}
			parens--
			j++
		case c <= ' ' || c == 0x7f:
			break loop
		default:
			j++
		}
	}
	ip.d.steps += j - i
	if j == i && (j >= len(s) || s[j] != ')') {
		return "", false
	}
	if parens != 0 {
		return "", false
	}
	ip.pos = j
	return unescapeString(s[i:j]), true
}

// parseLinkTitle reads a link title at pos: "…", '…' or (…).
func (ip *inlineParser) parseLinkTitle() (string, bool) {
	s := ip.s
	i := ip.pos
	if i >= len(s) {
		return "", false
	}
	end := s[i]
	switch end {
	case '"', '\'':
	case '(':
		end = ')'
	default:
		return "", false
	}
	for j := i + 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\\' && j+1 < len(s):
			j++
		case c == end:
			ip.d.steps += j - i
			ip.pos = j + 1
			return unescapeString(s[i+1 : j]), true
		case c == '(' && end == ')':
			ip.d.steps += j - i
			return "", false
		}
	}
	ip.d.steps += len(s) - i
	return "", false
}

// NormalizeLabel normalizes a label as CommonMark matches link
// references and footnotes: whitespace collapsed and case folded.
func NormalizeLabel(s string) string { return normalizeLabel(s) }

// normalizeLabel normalizes a link label for matching: whitespace
// collapsed and case folded.
func normalizeLabel(s string) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteByte(c)
	}
	s = b.String()
	s = strings.ToLower(s)
	if strings.Contains(s, "ß") {
		s = strings.ReplaceAll(s, "ß", "ss")
	}
	return strings.ToUpper(s)
}

// parseReference reads a link reference definition at the start of s,
// adding it to the document unless its label is defined, and returns its
// length, or 0. at is the offset of s in the source.
func (ip *inlineParser) parseReference(d *Document, s string, at int) int {
	saveS, savePos := ip.s, ip.pos
	defer func() { ip.s, ip.pos = saveS, savePos }()
	ip.s, ip.pos = s, 0
	n := ip.parseLinkLabel()
	if n == 0 {
		return 0
	}
	label := s[1 : n-1]
	if ip.pos >= len(s) || s[ip.pos] != ':' {
		return 0
	}
	ip.pos++
	ip.spnl()
	dest, ok := ip.parseLinkDestination()
	if !ok {
		return 0
	}
	beforeTitle := ip.pos
	ip.spnl()
	title, hasTitle := "", false
	if ip.pos != beforeTitle {
		title, hasTitle = ip.parseLinkTitle()
	}
	if !hasTitle {
		ip.pos = beforeTitle
	}
	atEnd := func() bool {
		j := ip.pos
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j == len(s) || s[j] == '\n' {
			if j < len(s) {
				j++
			}
			ip.pos = j
			return true
		}
		return false
	}
	if !atEnd() {
		if !hasTitle {
			return 0
		}
		title, hasTitle = "", false
		ip.pos = beforeTitle
		if !atEnd() {
			return 0
		}
	}
	key := normalizeLabel(label)
	if key == "" {
		return 0
	}
	d.define(definition{key, linkRef{dest: dest, title: title, at: at}})
	return ip.pos
}

// flatten turns the tree into the block's inlines, in source offsets:
// content offsets from base, those past healFrom (text healing added)
// at healFrom.
func (ip *inlineParser) flatten(d *Document, block, base, healFrom int) []Inline {
	if ip.nodes[0].first < 0 {
		return nil
	}
	out := make([]Inline, 0, len(ip.nodes)-1)
	m := d.sourceMap(block)
	srcOf := func(off int) int {
		return m.source(base + min(off, healFrom))
	}
	var walk func(n int32)
	walk = func(n int32) {
		for c := ip.nodes[n].first; c >= 0; c = ip.nodes[c].next {
			nd := &ip.nodes[c]
			if nd.kind == Text && nd.text == "" {
				continue
			}
			d.steps++
			i := len(out)
			out = append(out, Inline{Kind: nd.kind, Text: nd.text, Dest: nd.dest, Title: nd.title, Start: srcOf(nd.start), End: srcOf(nd.end), Auto: nd.auto, Display: nd.display})
			if nd.end > healFrom && (nd.kind == Link || nd.kind == Image) {
				out[i].Pending = true
			}
			walk(c)
			out[i].Next = len(out)
		}
	}
	walk(0)
	return out
}

// joinTexts joins the runs of text among the children of node n, and
// below it, into one node each: a slice of the subject when their texts
// are its bytes one after another, else the texts joined once, so that a
// run of many, as delimiters left unmatched, takes linear time.
func (ip *inlineParser) joinTexts(n int32) {
	for c := ip.nodes[n].first; c >= 0; c = ip.nodes[c].next {
		ip.d.steps++
		if ip.nodes[c].kind != Text {
			ip.joinTexts(c)
			continue
		}
		last := c
		for nx := ip.nodes[c].next; nx >= 0 && ip.nodes[nx].kind == Text; nx = ip.nodes[nx].next {
			ip.d.steps++
			last = nx
		}
		if last == c {
			continue
		}
		first := &ip.nodes[c]
		exact := true
		var b strings.Builder
		for k := c; ; k = ip.nodes[k].next {
			nd := &ip.nodes[k]
			if nd.text != ip.s[nd.start:min(nd.end, len(ip.s))] || k != c && nd.start != ip.nodes[nd.prev].end {
				exact = false
			}
			if k == last {
				break
			}
		}
		end := ip.nodes[last].end
		if exact {
			first.text = ip.s[first.start:end]
		} else {
			for k := c; ; k = ip.nodes[k].next {
				b.WriteString(ip.nodes[k].text)
				if k == last {
					break
				}
			}
			first.text = b.String()
		}
		first.end, first.plain = end, false
		for k := ip.nodes[c].next; k >= 0; {
			nx := ip.nodes[k].next
			ip.unlink(k)
			if k == last {
				break
			}
			k = nx
		}
	}
}
