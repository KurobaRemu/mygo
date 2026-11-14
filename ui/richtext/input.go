package richtext

import (
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/internal/rope"
	"github.com/egoist/mygo/internal/text"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

const scrollbarWidth = 10

// input takes the editor's input as it comes: keys, text, edit commands,
// the pointer and the wheel.
func (v *view) input(ev ui.InputEvent) bool {
	// The app is asked first, before the editor takes the document's state
	// and gives its own back: what the app does to the document is kept.
	if ev.Kind == ui.InputKeyDown && v.opts.OnKey != nil && v.opts.OnKey(ev.Key, ev.Mods) {
		return true
	}
	v.sync()
	defer func() {
		v.settleBlockPrefix()
		v.publish()
	}()
	switch ev.Kind {
	case ui.InputKeyDown:
		if v.key(ev.Key, ev.Mods) {
			v.blinkStart = ev.Time
			return true
		}
		return false
	case ui.InputText:
		if v.opts.ReadOnly || ev.Text == "" {
			return false
		}
		if v.lang != nil {
			v.fenceLangType(ev.Text)
			v.blinkStart = ev.Time
			return true
		}
		v.compose = ""
		v.wordMove = false
		text := ev.Text
		if v.skipAutoPair(text) {
			source := v.caretSource()
			if source >= 0 {
				v.setSourceCaret(source + 1)
				v.resumeLivePrefix(nil)
			}
		} else if pair, ok := v.autoPair(text); ok {
			v.insert(pair)
			v.typed(ev.Text)
			v.caretInsideAutoPair(pair)
		} else {
			text = v.prepareBlockPrefix(text)
			v.insert(text)
			v.typed(ev.Text)
		}
		v.blinkStart = ev.Time
		return true
	case ui.InputCommand:
		if v.lang != nil {
			// The language input owns the editing surface: the document's
			// commands do not act while it holds the focus. A paste is the
			// input's own edit and replaces its selection (record 2026-11-11,
			// probe92b/cmd-v-escape, /cmd-v-type-tab).
			if ev.Text == "paste" || ev.Text == "pasteAndMatchStyle" {
				v.fenceLangPaste()
			}
			return true
		}
		return v.editCommand(ev.Text)
	case ui.InputPointerDown:
		return v.press(ev)
	case ui.InputPointerMove:
		return v.move(ev)
	case ui.InputPointerUp:
		return v.release(ev)
	case ui.InputSelect:
		if v.lang != nil {
			v.commitFenceLanguage()
		}
		if v.transient != nil {
			v.discardTransient()
		}
		n := v.shown.Len()
		v.wordMove = false
		a, z := max(0, min(ev.Start, n)), max(0, min(ev.End, n))
		v.anchorAffinity, v.caretAffinity = boundaryAffinity(a, z, true), boundaryAffinity(a, z, false)
		v.doc.lock()
		v.setSelectionLocked(a, z, v.anchorAffinity, v.caretAffinity)
		v.doc.mu.Unlock()
		v.affinity = ui.TextDownstream
		v.hasDesired = false
		v.reveal = true
		v.maybeLeaveEmpty()
		return true
	case ui.InputScroll:
		// At an end, the wheel goes on to the containers around.
		return v.scrollTo(v.scroll + float64(ev.DY))
	}
	return false
}

// moveTo puts the caret at i, extending the selection or not.
func (v *view) moveTo(i int, affinity ui.TextAffinity, extend bool) {
	v.moveCaret(i, affinity, extend)
	v.maybeLeaveEmpty()
}

// moveCaret puts the caret at i without dropping a retained empty pair the
// caret lands at the end of. Moving back into the pair from the line below
// enters the element, which Typora keeps (record 2026-11-10, seed4/c18);
// the positional test in maybeLeaveEmpty would read it as a departure.
func (v *view) moveCaret(i int, affinity ui.TextAffinity, extend bool) {
	i = max(0, min(i, v.shown.Len()))
	v.affinity = affinity
	boundary := BoundaryDownstream
	if affinity == ui.TextUpstream {
		boundary = BoundaryUpstream
	}
	v.doc.lock()
	v.setCaretLocked(i, boundary, extend)
	v.doc.mu.Unlock()
	v.coalesce = false
	v.reveal = true
}

func (v *view) moveToEnd(i int, affinity ui.TextAffinity, extend bool) {
	v.moveTo(i, affinity, extend)
	p := v.shown.paraAt(v.caret)
	if v.source.Len() <= p || v.caret != v.shown.paraStart(p)+v.para(p).n {
		return
	}
	source := v.paragraphEndSource(p)
	v.doc.lock()
	anchor := v.doc.selection.Anchor
	if !extend {
		anchor = sourcePoint(source, BoundaryUpstream)
	}
	v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: sourcePoint(source, BoundaryUpstream)})
	v.anchorAffinity, v.caretAffinity = v.doc.selection.Anchor.Affinity, v.doc.selection.Caret.Affinity
	v.doc.mu.Unlock()
}

func (v *view) stepLiteral(i int, affinity ui.TextAffinity, extend bool) {
	if extend && i < v.caret && v.transient != nil && v.transient.preservesOnShiftLeft() {
		return
	}
	from, source := v.caret, v.caretSource()
	v.moveTo(i, affinity, extend)
	if source < 0 || v.shown.paraAt(from) != v.shown.paraAt(i) {
		return
	}
	a, z := min(from, i), max(from, i)
	text := v.shown.Slice(a, z).String()
	// A shown rune can hold more source bytes than it shows, as a hard
	// break's "\<newline>" does: the visible text steps over bytes the
	// caret cannot stand between. The moved rune is literal only when its
	// source span is as long as the text, or the caret the step computes
	// would land inside those bytes; the move's own caret stands after
	// them.
	if span := v.sourceForCaret(z) - v.sourceForCaret(a); span != len(text) {
		return
	}
	switch {
	case i < from && source >= len(text) && v.sourceSlice(source-len(text), source) == text:
		source -= len(text)
	case i > from && v.sourceSlice(source, source+len(text)) == text:
		source += len(text)
	default:
		return
	}
	boundary := BoundaryDownstream
	if affinity == ui.TextUpstream {
		boundary = BoundaryUpstream
	}
	collapse := SourceSelection{Anchor: sourcePoint(source, boundary), Caret: sourcePoint(source, boundary)}
	v.doc.lock()
	// A shown rune can hold source bytes the text does not, as a hard
	// break's "\<newline>": moving by the shown text's length would stop
	// inside them. The moved caret's source point is the one that maps
	// back to it; anything else keeps what the move found.
	if a, z := v.doc.docSelectionForSourceLocked(collapse); a != i || z != i {
		v.doc.mu.Unlock()
		return
	}
	anchor := v.doc.selection.Anchor
	if !extend {
		anchor = collapse.Anchor
	}
	v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: collapse.Caret})
	v.anchorAffinity, v.caretAffinity = v.doc.selection.Anchor.Affinity, v.doc.selection.Caret.Affinity
	v.doc.mu.Unlock()
}

// transientHoldsPlainParagraph reports whether the transient paragraph Enter
// opened at the document's end is a plain paragraph whose bytes are nothing
// but its own line ending, so the line above it ends the document's content.
// A container's empty item or quote line, and a paragraph that already wrote
// a marker for the parser, hold bytes of their own and keep the ordinary
// extension.
func (v *view) transientHoldsPlainParagraph() bool {
	t := v.transient
	if t == nil || t.para < 0 || t.para >= v.shown.ParagraphCount() {
		return false
	}
	st := v.para(t.para).style
	return st.List == NoList && !st.Quote && st.Heading == 0 && !st.CodeBlock
}

// leftExtendStopsAtItemStart reports whether a backward Shift+Left from the
// caret z to the target i leaves the caret's list item at its start and must
// stay put. Typora stops there once the selection already spans item
// boundaries and both the caret's and the anchor's items hold text; an empty
// item on either end lets the extension cross (records 2026-11-10, probe52b).
func (v *view) leftExtendStopsAtItemStart(z, i int) bool {
	a := v.selAnchor
	if a == z {
		return false
	}
	p := v.shown.paraAt(z)
	if q := v.shown.paraAt(i); q >= p || z != v.shown.paraStart(p) {
		return false
	}
	anchorPara := v.shown.paraAt(a)
	if anchorPara == p ||
		v.para(anchorPara).style.List == NoList || v.para(anchorPara).n == 0 {
		return false
	}
	return v.para(p).style.List != NoList && v.para(p).n > 0
}

// stepHiddenMarker moves the moving edge of a non-collapsed selection one
// source byte through the inline delimiter run it stands in. The selection
// draws the markers it spans as nothing (spec §4.4), so its visible position
// does not change, but Typora still walks the hidden bytes: the source range
// is what a later paste or cut replaces (record 2026-11-10, seed4/c30;
// seed9/c5).
func (v *view) stepHiddenMarker(left, extend bool) bool {
	d := v.doc
	if d == nil || v.source.Len() == 0 {
		return false
	}
	d.lock()
	sel := d.selection
	d.mu.Unlock()
	if sel.Anchor.Offset == sel.Caret.Offset {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.source.Len() {
		return false
	}
	source := sel.Caret.Offset
	next := source
	moved := false
	for _, b := range v.boxes(p) {
		// A forward step from an opening run's first byte does not walk the
		// run: the display map folds it into the boundary before the
		// element's content, so the caret stands in front of that content
		// and the next visible glyph is the content's first rune. Typora
		// crosses the run and that rune in one step (record 2026-11-12,
		// seed15/c7).
		if !left && b.openEnd > b.openStart && source == b.openStart {
			continue
		}
		for _, run := range [][2]int{{b.openStart, b.openEnd}, {b.closeStart, b.closeEnd}} {
			from, to := run[0], run[1]
			if to <= from {
				continue
			}
			switch {
			case left && source > from && source <= to:
				next, moved = source-1, true
			case !left && source >= from && source < to:
				next, moved = source+1, true
			}
		}
	}
	if !moved {
		return false
	}
	affinity, boundary := ui.TextDownstream, BoundaryDownstream
	if left {
		affinity, boundary = ui.TextUpstream, BoundaryUpstream
		if next == v.source.start(p) {
			// The paragraph's source start is the downstream side of the
			// folded run that opens the block: that is the point the source
			// projection gives a caret at the block start, and Typora leaves
			// the caret there after the run's last byte (seed10/c14).
			affinity, boundary = ui.TextDownstream, BoundaryDownstream
		}
	}
	point := sourcePoint(next, boundary)
	d.lock()
	anchor := point
	if extend {
		anchor = d.selection.Anchor
	}
	v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: point})
	v.anchorAffinity, v.caretAffinity = d.selection.Anchor.Affinity, d.selection.Caret.Affinity
	d.mu.Unlock()
	v.affinity = affinity
	v.coalesce = false
	v.reveal = true
	v.hasDesired = false
	v.maybeLeaveEmpty()
	return true
}

// runeAt returns rune i of the text shown.
func (v *view) runeAt(i int) rune {
	r, _ := rope.TextOf(v.shown.text).RuneAt(rope.ByteOfRune(v.shown.text, i))
	return r
}

// nextWord returns where the forward word move stops: the end of the word
// after i, or the start of the next paragraph when i is already at the end
// of one (probe90/para-end-alt-right); prevWord returns the start of the
// word before i. Each ideograph and kana is a word, as text.WordBoundary
// has it.
func (v *view) nextWord(i int) int {
	if _, end := v.shown.ParagraphRange(i); i == end && end < v.shown.Len() {
		return end + 1
	}
	n, word, prev := v.shown.Len(), false, rune(0)
	for ; i < n; i++ {
		r := v.runeAt(i)
		if word && text.WordBoundary(prev, r) {
			break
		}
		word, prev = word || text.IsWordRune(r), r
	}
	return i
}

// nextWordDelete returns where Option+Delete stops: the end of the word
// after i plus the whitespace that follows it, as Typora has it
// (probe90/mid-empty-alt-delete).
func (v *view) nextWordDelete(i int) int {
	end := v.nextWord(i)
	for end < v.shown.Len() && unicode.IsSpace(v.runeAt(end)) {
		end++
	}
	return end
}

// finalEOLStart returns where the source's final line ending starts, or -1
// when the source does not end with one.
func finalEOLStart(source string) int {
	switch {
	case strings.HasSuffix(source, "\r\n"):
		return len(source) - 2
	case strings.HasSuffix(source, "\n"), strings.HasSuffix(source, "\r"):
		return len(source) - 1
	}
	return -1
}

func (v *view) autoPair(s string) (string, bool) {
	r := []rune(s)
	if len(r) != 1 {
		return "", false
	}
	var close rune
	switch r[0] {
	case '(':
		close = ')'
	case '[':
		close = ']'
	case '{':
		close = '}'
	default:
		return "", false
	}
	a, z := v.selection()
	if a != z {
		return "", false
	}
	// Typora's WYSIWYG editor (frame.js handlePair) adds the closing
	// delimiter only when the source character after the caret is empty or
	// one of `[]{}()"'` whitespace, `>`, backtick or `$`; typing before any
	// other character leaves the bracket alone. The check reads the source
	// rather than the shown text because Typora reads the block's raw
	// markdown: inside `*x*` the character after the caret is the closing
	// `*`, so `[` does not pair (recorded in probe/auto-pair-italic).
	if at := v.caretSource(); at >= 0 {
		src := v.doc.Source()
		if at < len(src) {
			next, _ := utf8.DecodeRuneInString(src[at:])
			if !bracketPairFollows(next) {
				return "", false
			}
		}
	}
	return string(r[0]) + string(close), true
}

// bracketPairFollows reports whether Typora pairs an opening bracket typed
// before r: its handlePair only accepts these characters (plus whitespace).
func bracketPairFollows(r rune) bool {
	switch r {
	case '[', ']', '{', '}', '(', ')', '"', '\'', '`', '>', '$':
		return true
	}
	return strings.TrimSpace(string(r)) == ""
}

func (v *view) skipAutoPair(s string) bool {
	r := []rune(s)
	if len(r) != 1 {
		return false
	}
	a, z := v.selection()
	if a != z {
		return false
	}
	switch r[0] {
	case ')', ']', '}':
	default:
		return false
	}
	if source := v.caretSource(); source >= 0 && v.sourceSlice(source, source+1) == s {
		return true
	}
	if a >= v.shown.Len() {
		return false
	}
	return v.runeAt(a) == r[0]
}

func (v *view) setSourceCaret(source int) {
	v.sync()
	v.doc.lock()
	v.setSourceSelectionLocked(sourceCaret(source))
	v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	v.doc.mu.Unlock()
	v.affinity = ui.TextDownstream
	v.hasDesired = false
	v.reveal = true
}

// caretInsideAutoPair puts the source caret between the delimiters just
// inserted. The insertion itself may have materialized a transient block,
// so its source patch can start before the delimiters, and the typing
// attributes may render them escaped inside markers, as *\[\]*: between them
// is after the opening delimiter's escape.
func (v *view) caretInsideAutoPair(pair string) {
	if source := v.caretSource(); source >= len(pair) && v.sourceSlice(source-len(pair), source) == pair {
		v.setSourceCaret(source - len(pair) + firstRuneSize(pair))
		return
	}
	src := v.doc.Source()
	start, _, end, ok := v.doc.LastSourcePatch()
	if !ok || start < 0 || start > end || end > len(src) {
		return
	}
	inserted := src[start:end]
	open := escapeText(pair[:1])
	if i := strings.Index(inserted, open+escapeText(pair[1:])); i >= 0 {
		v.setSourceCaret(start + i + len(open))
		return
	}
	if i := strings.Index(inserted, pair); i >= 0 {
		v.setSourceCaret(start + i + len(pair[:1]))
	}
}

func isASCIIWord(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_'
}

func emptyPairAt(v *view, at int) bool {
	if at <= 0 || at >= v.shown.Len() {
		return false
	}
	a, z := v.runeAt(at-1), v.runeAt(at)
	return a == '(' && z == ')' || a == '[' && z == ']' || a == '{' && z == '}'
}

// prevWord returns the start of the word before i, within the caret's
// paragraph. At the start of a paragraph the move lands at the end of the
// closest preceding paragraph that holds text, as Typora does
// (probe77/mid-empty-alt-shift-left); a soft line break does not stop it
// (probe90/line-start-alt-left, probe90/punct-line-end-alt-left). Each
// ideograph and kana is a word, as text.WordBoundary has it.
func (v *view) prevWord(i int) int {
	start, _ := v.shown.ParagraphRange(i)
	word, next := false, rune(0)
	for j := i; j > start; j-- {
		r := v.runeAt(j - 1)
		if word && text.WordBoundary(r, next) {
			return j
		}
		word, next = word || text.IsWordRune(r), r
	}
	if i > start {
		return start
	}
	// A paragraph start has no word before it: step over whitespace-only
	// paragraphs to the end of the closest one that holds text.
	blank := func(a, z int) bool {
		for ; a < z; a++ {
			if !unicode.IsSpace(v.runeAt(a)) {
				return false
			}
		}
		return true
	}
	for start > 0 {
		ps, pe := v.shown.ParagraphRange(start - 1)
		if !blank(ps, pe) {
			return pe
		}
		start = ps
	}
	return 0
}

// wordAt returns the word, or the run of other runes, around i, within its
// paragraph: an ideograph or a kana is a word alone.
func (v *view) wordAt(i int) (int, int) {
	n := v.shown.Len()
	if n == 0 {
		return 0, 0
	}
	i = max(0, min(i, n-1))
	at := v.runeAt(i)
	a, z := i, i
	for next := at; a > 0; a-- {
		r := v.runeAt(a - 1)
		if r == '\n' || r == LineSeparator || text.WordBoundary(r, next) {
			break
		}
		next = r
	}
	for prev := at; z < n; z++ {
		r := v.runeAt(z)
		if r == '\n' || r == LineSeparator || z > i && text.WordBoundary(prev, r) {
			break
		}
		prev = r
	}
	return a, z
}

// grapheme returns the edge of the grapheme before i (dir < 0) or after
// it: a newline is one.
func (v *view) grapheme(i, dir int) int {
	n := v.shown.Len()
	p := v.shown.paraAt(i)
	ps := v.shown.paraStart(p)
	pe := ps + v.para(p).n
	if dir < 0 && i == ps {
		return max(i-1, 0)
	}
	if dir > 0 && i >= pe {
		return min(i+1, n)
	}
	pv := v.paraLayout(p)
	u := pv.units[min(v.local(p, i), len(pv.units)-1)]
	if dir < 0 {
		u = pv.layout.PreviousBoundary(u)
	} else {
		u = pv.layout.NextBoundary(u)
	}
	return v.global(p, pv.runeOfUnit(u))
}

// visual returns the caret one grapheme left (dir < 0) or right on screen,
// across bidi runs, and to the next paragraph at the edge of one. An
// offset can have two places, the edge of a span's PaddingX (outside the
// span and in it) or a line wrapped within a word (the end of one line and
// the start of the next), and a step between them is a move.
func (v *view) visual(dir int) (int, ui.TextAffinity) {
	p := v.shown.paraAt(v.caret)
	pv := v.paraLayout(p)
	l := min(v.local(p, v.caret), len(pv.units)-1)
	pos := ui.TextCaretPosition{Index: pv.units[l], Affinity: v.affinity}
	next := pv.layout.MoveCaret(pos, dir)
	if next == pos {
		if dir < 0 {
			return max(v.shown.paraStart(p)-1, 0), ui.TextDownstream
		}
		return min(v.shown.paraStart(p)+v.para(p).n+1, v.shown.Len()), ui.TextDownstream
	}
	return v.global(p, pv.runeOfUnit(next.Index)), next.Affinity
}

// vertical moves the caret up or down lines, keeping its x.
func (v *view) vertical(lines int, extend bool) {
	// A plain move first puts the caret at the selection's end in the
	// direction of travel — the top for up, the bottom for down — as
	// NSTextView does. At the last line that collapsed caret is what the
	// move below sees, and it is what opens the transient paragraph there
	// (record 2026-11-10, newops/c46-3).
	if !extend {
		if a, z := v.selection(); a != z {
			if lines < 0 {
				v.moveTo(a, ui.TextDownstream, false)
			} else {
				v.moveTo(z, ui.TextDownstream, false)
			}
		}
	}
	// An empty paragraph has one caret position: moving up from it reaches
	// the end of the paragraph before, not the column its empty line sits at.
	// A code block's lines are one text, so its blank line keeps the column
	// and lands at the start of the line above (record 2026-11-11,
	// seed10/c10).
	if lines < 0 {
		p := v.shown.paraAt(v.caret)
		if p > 0 && v.para(p).n == 0 {
			first, _, code := v.codeBlockAt(p)
			if !code || first == p {
				q := v.shown.paraAt(v.shown.paraStart(p) - 1)
				v.moveToEnd(v.shown.paraStart(q)+v.para(q).n, ui.TextUpstream, extend)
				v.hasDesired = false
				return
			}
		}
	}
	x, _, _ := v.caretAt(v.caret, 0, v.affinity)
	if !v.hasDesired {
		v.desiredX, v.hasDesired = x, true
	}
	p := v.shown.paraAt(v.caret)
	pv := v.paraLayout(p)
	r := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[min(v.local(p, v.caret), len(pv.units)-1)], Affinity: v.affinity})
	y := r.Y + r.H/2
	for ; lines != 0; lines -= sign(lines) {
		_, lh := pv.layout.Size()
		switch {
		case lines < 0 && y-r.H >= 0:
			y -= r.H
		case lines < 0 && p == 0:
			if st := v.para(p).style; st.CodeBlock && !extend {
				// Up on the code block's first line leaves the block: Typora
				// writes the empty paragraph above it and the blank line the
				// fence needs, then the caret stands there (record
				// 2026-11-10, seed6/up-from-code-first-line).
				v.record(false)
				v.replaceSourceReparse(0, 0, "\n\n", 0)
				v.hasDesired = false
				return
			}
			v.moveTo(0, ui.TextDownstream, extend)
			return
		case lines < 0:
			p--
			pv = v.paraLayout(p)
			_, lh = pv.layout.Size()
			y = lh - 1
		case y+r.H < lh:
			y += r.H
		case p == len(v.paras)-1:
			if lines > 0 && !extend && v.selAnchor == v.caret && !v.para(p).style.CodeBlock {
				st := v.para(p).style
				if v.para(p).n == 0 && st.List == NoList && !st.Quote {
					// The document already ends in the empty paragraph the
					// move is going to: Typora leaves it where it is.
					return
				}
				if v.para(p).n == 0 && st.List != NoList {
					// An empty item's line is the list's last line: there
					// is no line below it to open, so the caret stays in
					// the item (record 2026-11-10, newops/c47-6).
					return
				}
				if st.List != NoList {
					// Down from a continuation line of an item's last
					// paragraph has no line below to take: Typora moves to
					// the paragraph's end without opening a line when the
					// line above it holds text, and leaves the caret when
					// that line is the empty first one
					// (probe10/bullet-soft-start-down,
					// probe9/task-down-checkpoints). A line opened here
					// would put the next Enter on an exit paragraph
					// instead of the split Typora writes.
					if local := v.local(p, v.caret); local < v.para(p).n {
						text := plainOf(v.shown.paragraph(p))
						if at := strings.LastIndex(text, string(LineSeparator)); at >= 0 &&
							utf8.RuneCountInString(text[:at]) < local {
							if at == 0 {
								return
							}
							v.moveToEnd(v.shown.paraStart(p)+v.para(p).n, ui.TextDownstream, extend)
							return
						}
					}
				}
				// The move reaches a line past the document's bytes: Typora's
				// last line has no line below it, because the editor's text
				// never holds the file's final line ending. The line stays in
				// the view and leaves no bytes when the caret goes or the
				// document is saved (record 2026-11-10, newops/c46-3).
				if v.openTransient(IntentExitDocument, v.paragraphEndSource(p), true) {
					v.transient.mode = transientPhantomEOF
					return
				}
			}
			if v.para(p).style.CodeBlock {
				// Down on a fenced block's last code line moves the code
				// editor's caret back to the block's first position:
				// Typora reports the caret at the start of the code text
				// (record 2026-11-09, seed4/c15).
				if first, _, ok := v.codeBlockAt(p); ok {
					v.moveTo(v.shown.paraStart(first), ui.TextDownstream, extend)
					return
				}
			}
			v.moveTo(v.shown.Len(), ui.TextDownstream, extend)
			return
		default:
			p++
			pv = v.paraLayout(p)
			y = 1
		}
		r = pv.layout.CaretAt(pv.layout.PositionAt(ui.Point{Y: y}))
		y = r.Y + r.H/2
	}
	ox, _ := v.origin(p)
	pos := pv.layout.PositionAt(ui.Point{X: v.desiredX - ox, Y: y})
	v.moveTo(v.global(p, pv.runeOfUnit(pos.Index)), pos.Affinity, extend)
	v.hasDesired = true
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	return 1
}

// emacsKeys are the Control keys of macOS text views that act as other
// keys.
var emacsKeys = map[ui.Key]ui.Key{ui.KeyB: ui.KeyLeft, ui.KeyF: ui.KeyRight, ui.KeyP: ui.KeyUp, ui.KeyN: ui.KeyDown, ui.KeyH: ui.KeyBackspace, ui.KeyD: ui.KeyDelete}

// key handles a key pressed, and reports whether it took it.
func (v *view) key(key ui.Key, mods ui.Modifiers) bool {
	v.wordMove = false
	// The first Shift+Right after an IME commit leaves the fence input and
	// opens the block's exit paragraph (record 2026-11-14, seed23/c13).
	if v.lang != nil && key == ui.KeyRight && mods == ui.Shift &&
		v.lang.text != v.lang.original && v.lang.anchor == v.lang.focus &&
		v.lang.focus == utf8.RuneCountInString(v.lang.text) && !v.lang.shiftRight &&
		v.leaveFenceLanguageByShiftRight() {
		return true
	}
	if v.lang != nil && v.fenceLangKey(key, mods) {
		return true
	}
	// A held paragraph dropped for this key comes back with the step the key
	// makes; a key that only moves leaves it dropped.
	defer v.doc.clearPendingTransient()
	if v.transient != nil {
		if handled, done := v.handleTransientKey(key, mods); handled {
			return done
		}
	}
	if cmd, ok := shortcut(key, mods); ok {
		taken := v.run(cmd)
		if taken && cmd == "italic" {
			v.releaseEmptyEmphasis()
		}
		return taken
	}
	mac := runtime.GOOS == "darwin"
	shift := mods&ui.Shift != 0
	m := mods &^ ui.Shift
	if mac && m == ui.Ctrl {
		if to, ok := emacsKeys[key]; ok {
			key, m = to, 0
		} else {
			p := v.shown.paraAt(v.caret)
			ps := v.shown.paraStart(p)
			pe := ps + v.para(p).n
			switch key {
			case ui.KeyA:
				v.moveTo(ps, ui.TextDownstream, shift)
				return true
			case ui.KeyE:
				v.moveToEnd(pe, ui.TextDownstream, shift)
				return true
			case ui.KeyK:
				if v.opts.ReadOnly {
					return true
				}
				if pe == v.caret && pe < v.shown.Len() {
					pe++
				}
				v.deleteRange(v.caret, pe, false)
				return true
			}
		}
	}
	word := (!mac && m == ui.Ctrl) || (mac && m == ui.Alt)
	readOnly := v.opts.ReadOnly
	if key == ui.KeyV && shift && (m == ui.Cmd || m == ui.Cmd|ui.Alt) {
		// Paste and Match Style: ⇧⌥⌘V, and ⇧⌘V as Notes has it.
		return v.editCommand("pasteAndMatchStyle")
	}
	a, z := v.selection()
	n := v.shown.Len()
	switch key {
	case ui.KeyLeft, ui.KeyRight:
		if key == ui.KeyRight && mods == 0 && v.enterFenceLanguageInput() {
			return true
		}
		left := key == ui.KeyLeft
		switch {
		case m == ui.Alt && !mac:
			return false // back and forward
		case mac && m == ui.Super:
			s, e := v.lineEdges(v.caret)
			if left {
				v.moveTo(s, ui.TextDownstream, shift)
			} else {
				v.moveToEnd(e, ui.TextUpstream, shift)
			}
		case word && left:
			v.moveTo(v.prevWord(v.caret), ui.TextDownstream, shift)
			v.wordMove = true
		case word:
			v.moveTo(v.nextWord(v.caret), ui.TextDownstream, shift)
			v.wordMove = true
		case a != z && !shift && left:
			v.moveTo(a, ui.TextDownstream, false)
		case a != z && !shift:
			v.moveTo(z, ui.TextDownstream, false)
		case m == 0 && v.stepHiddenMarker(left, shift):
			// A non-collapsed selection draws the delimiters it spans as
			// nothing but still moves over their bytes.
		case m == 0 && v.displayNavigation():
			v.stepDisplay(left, shift)
		case left:
			i, aff := v.visual(-1)
			if v.doc.md == nil {
				v.moveTo(i, aff, shift)
				break
			}
			if shift && v.leftExtendStopsAtItemStart(v.caret, i) {
				// Typora's backward selection stops at the start of the list
				// item it has reached once the selection spans items and both
				// the moving and the fixed item hold text (probe52b/empty-middle,
				// probe52b/c19-plain-left). An empty item on either end keeps the
				// extension moving (probe52b/empty-only, seed4/c13).
				break
			}
			if !shift {
				// A backward move stands upstream of the boundary it reaches:
				// hidden delimiters between the two visible glyphs are crossed
				// whole, landing after them rather than inside the element
				// (record 2026-11-10, probe75/ra2, /rh).
				aff = ui.TextUpstream
			}
			v.stepLiteral(i, aff, shift)
		default:
			i, aff := v.visual(1)
			if v.doc.md == nil {
				v.moveTo(i, aff, shift)
			} else {
				v.stepLiteral(i, aff, shift)
			}
		}
		v.hasDesired = false
		return true
	case ui.KeyUp, ui.KeyDown:
		if mac && m == ui.Super {
			if v.doc.md != nil {
				if first, last, ok := v.codeBlockAt(v.shown.paraAt(v.caret)); ok {
					// Typora edits a markdown code block in its own text
					// editor, where Cmd+Up and Cmd+Down move to the block's
					// first and last code line instead of the document's
					// ends.
					if key == ui.KeyUp {
						v.moveTo(v.shown.paraStart(first), ui.TextDownstream, shift)
					} else {
						v.moveToEnd(v.shown.paraStart(last)+v.para(last).n, ui.TextUpstream, shift)
					}
					v.hasDesired = false
					return true
				}
			}
			if key == ui.KeyUp {
				v.moveTo(0, ui.TextDownstream, shift)
			} else {
				v.moveToEnd(n, ui.TextDownstream, shift)
			}
			return true
		}
		if mac && m == ui.Alt || !mac && m == ui.Ctrl {
			// By paragraphs: up to the start of this one or the one
			// before; down to the end of this one or the next on macOS,
			// as NSTextView, to the start of the next elsewhere, as
			// WordPad and GTK.
			a, z := v.shown.ParagraphRange(v.caret)
			switch {
			case key == ui.KeyUp && v.caret == a && a > 0:
				a, _ = v.shown.ParagraphRange(a - 1)
				v.moveTo(a, ui.TextDownstream, shift)
			case key == ui.KeyUp:
				v.moveTo(a, ui.TextDownstream, shift)
			case mac && v.caret == z && z < n:
				_, z = v.shown.ParagraphRange(z + 1)
				v.moveTo(z, ui.TextUpstream, shift)
			case mac:
				v.moveTo(z, ui.TextUpstream, shift)
			default:
				v.moveTo(min(z+1, n), ui.TextDownstream, shift)
			}
			v.hasDesired = false
			return true
		}
		if m != 0 {
			return false
		}
		d := 1
		if key == ui.KeyUp {
			d = -1
		}
		v.vertical(d, shift)
		return true
	case ui.KeyPageUp, ui.KeyPageDown:
		lines := max(int(v.viewH/(v.look.size*1.3))-1, 1)
		if key == ui.KeyPageUp {
			lines = -lines
		}
		v.vertical(lines, shift)
		return true
	case ui.KeyHome, ui.KeyEnd:
		if m == 0 && v.doc.md != nil {
			p := v.shown.paraAt(v.caret)
			if !v.para(p).style.CodeBlock {
				// Typora's rich-text blocks leave the physical Home/End keys
				// alone; its code block editor handles End.
				return true
			}
		}
		if m == ui.Ctrl {
			if key == ui.KeyHome {
				v.moveTo(0, ui.TextDownstream, shift)
			} else {
				v.moveToEnd(n, ui.TextDownstream, shift)
			}
			return true
		}
		s, e := v.lineEdges(v.caret)
		if key == ui.KeyHome {
			v.moveTo(s, ui.TextDownstream, shift)
		} else {
			v.moveToEnd(e, ui.TextUpstream, shift)
		}
		return true
	case ui.KeyA:
		if m == ui.Cmd {
			v.doc.lock()
			v.setSelectionLocked(0, n, BoundaryUpstream, BoundaryDownstream)
			v.doc.mu.Unlock()
			return true
		}
	case ui.KeyC, ui.KeyX, ui.KeyV, ui.KeyZ, ui.KeyY:
		if m != ui.Cmd {
			break
		}
		switch key {
		case ui.KeyC:
			return v.editCommand("copy")
		case ui.KeyX:
			return v.editCommand("cut")
		case ui.KeyV:
			return v.editCommand("paste")
		case ui.KeyZ:
			if shift {
				return v.editCommand("redo")
			}
			return v.editCommand("undo")
		case ui.KeyY:
			return v.editCommand("redo")
		}
	}
	if readOnly {
		return false
	}
	p := v.shown.paraAt(v.caret)
	st := v.para(p).style
	ps := v.shown.paraStart(p)
	if key == ui.KeyEnter && m != 0 {
		if m == ui.Cmd || !mac && m == ui.Ctrl {
			a, z := v.selection()
			if v.canOpenTransient(IntentExitBlock, a, z) {
				p := v.shown.paraAt(v.caret)
				v.openTransient(IntentExitBlock, v.paragraphEndSource(p), p == len(v.paras)-1)
				return true
			}
			v.command(func(d *Document) {
				_ = d.applyIntent(EditRequest{Intent: IntentExitBlock, Start: v.caret, End: v.caret})
			})
			return true
		}
		return false
	}
	switch key {
	case ui.KeyBackspace:
		switch {
		case a != z:
			v.deleteRange(a, z, false)
		case m == 0 && v.replaceSourceSelection(""):
			// A selection over hidden syntax markers only, as two Lefts
			// make on the closing `**` after a paste, leaves the document
			// selection collapsed while the source selection still covers
			// bytes. Backspace replaces those bytes, as Tab does for the
			// same shape: deleting back from a collapsed display offset
			// would take the character before the run as well (VM record
			// 2026-11-12-160443, seed13/c3).
		case m == 0 && v.caret > 0 && v.caret < n && emptyPairAt(v, v.caret):
			v.deleteRange(v.caret-1, v.caret+1, false)
		case v.markerDelete(true):
		case m == 0 && v.caret == ps && special(st):
			if !v.opts.NoMarkdown && v.doc.md != nil {
				v.command(func(d *Document) {
					_ = d.applyIntent(EditRequest{Intent: IntentBackspaceAtStart, Start: v.caret, End: v.caret})
				})
			} else {
				v.command(func(d *Document) { d.FormatParagraphs(unstyle) })
			}
		case mac && m == ui.Super:
			s, _ := v.lineEdges(v.caret)
			v.deleteRange(s, v.caret, false)
		case word:
			v.deleteRange(v.prevWord(v.caret), v.caret, false)
		case v.caret > 0:
			v.deleteRange(v.grapheme(v.caret, -1), v.caret, true)
		}
		return true
	case ui.KeyDelete:
		switch {
		case a != z:
			v.deleteRange(a, z, false)
		case v.markerDelete(false):
		case m == 0 && v.caret == ps+v.para(p).n && !v.opts.NoMarkdown && v.doc.md != nil:
			v.command(func(d *Document) {
				_ = d.applyIntent(EditRequest{Intent: IntentDeleteAtEnd, Start: v.caret, End: v.caret})
			})
		case word:
			// Typora deletes to the end of the word and the whitespace after
			// it, and takes the file's final line ending when they end the
			// last paragraph (probe90/mid-empty-alt-delete).
			last := v.shown.paraAt(v.caret) == v.shown.ParagraphCount()-1 &&
				v.paragraphEndSource(v.shown.paraAt(v.caret)) == finalEOLStart(v.sourceText())
			end := v.nextWordDelete(v.caret)
			v.deleteRange(v.caret, end, false)
			if last && end > v.caret && end == n {
				if from := finalEOLStart(v.sourceText()); from >= 0 {
					v.replaceSourceErr(from, len(v.sourceText()), "", v.caretSource())
				}
			}
		case v.caret < n:
			v.deleteRange(v.caret, v.grapheme(v.caret, 1), false)
		}
		return true
	case ui.KeyEnter:
		if m != 0 {
			return false
		}
		if !shift && v.finishLivePrefixOnEnter() {
			return true
		}
		if shift && v.writeHeadingItemContinuation() {
			return true
		}
		if !shift && v.materializeHeldBreakOnEmptyItem() {
			return true
		}
		if !v.opts.NoMarkdown && v.doc.md != nil {
			if !shift && v.emptyAtCaret() {
				// Enter in a view-held empty inline pair drops the pair:
				// Typora opens the next paragraph with its bytes gone
				// (record 2026-11-11, probe60/c16-exit-cmd-b).
				v.removeEmptyPair(true)
			}
			if a, z := v.selection(); a != z {
				// Typora replaces the selection with the paragraph break:
				// the selected text leaves first, then Enter splits where
				// it stood (diff.json 64cae57c).
				v.deleteRange(a, z, false)
				// The deletion can empty the document's last paragraph, which the
				// view takes back. The break Enter writes is that paragraph's own
				// bytes: they are written before the split, and the split's empty
				// paragraph waits above them.
				v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true)
			}
			a, z := v.selection()
			intent := IntentEnter
			if shift {
				intent = IntentShiftEnter
				if v.para(v.shown.paraAt(v.caret)).style.Heading > 0 {
					// A heading holds no line break: Shift+Enter splits the
					// block as Enter does, and the empty paragraph it leaves
					// waits in the view.
					intent = IntentEnter
				}
			}
			atEOF := v.shown.paraAt(v.caret) == len(v.paras)-1
			source := v.paragraphEndSource(v.shown.paraAt(v.caret))
			if !shift && intent == IntentEnter {
				// Enter on an empty top-level item or quote line exits its
				// container as a step of its own, then the paragraph it
				// exits to waits in the view. The line is classified by the
				// caret's source offset: a blank quote line maps to the
				// paragraph after it.
				sel := v.doc.SourceSelection()
				if sel.Anchor.Offset == sel.Caret.Offset {
					switch v.doc.emptyBlockAt(sel.Caret.Offset) {
					case emptyBlockOnlyTopItem, emptyBlockOnlyNestedItem, emptyBlockNestedItem:
						// Typora writes the split, the lone nested item's
						// marker removal or the join to the item above it at
						// once; nothing waits in the view.
						v.record(false)
						v.command(func(d *Document) {
							_ = d.applyIntent(EditRequest{Intent: IntentEnter, Start: a, End: z})
						})
						v.mode, v.coalesce = newStep, false
						return true
					case emptyBlockTopItem, emptyBlockQuoteLine:
						base := v.doc.Source()
						finalEOL := strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r")
						var lift emptyLift
						var ok bool
						v.command(func(d *Document) { lift, ok = d.liftEmptyBlock(sel.Caret.Offset) })
						if ok {
							if !lift.materialized {
								v.openEmptyExitTransient(lift, finalEOL)
								// The paragraph the lift exits to is what the step leaves
								// behind: Undo takes the lift back to the item's own bytes
								// and Redo brings the paragraph back, as it does for the
								// item exit a deferred split performs (record 2026-11-13,
								// seed21/c6).
								v.doc.lock()
								if t := v.transient; t != nil {
									if n := len(v.doc.undo); n > 0 {
										v.doc.undo[n-1].afterTransient = t.undo(v.doc.md.p.String())
									}
								}
								v.doc.mu.Unlock()
							}
							return true
						}
					}
				}
			}
			if v.canOpenTransient(intent, a, z) &&
				(intent != IntentEnter || atEOF && !v.blankListLine() ||
					v.para(v.shown.paraAt(v.caret)).style.List == NoList &&
						v.para(v.shown.paraAt(v.caret)).n != 0 &&
						strings.TrimSpace(v.doc.Source()[source:]) != "") {
				// A list item that a following item continues needs the
				// split's marker bytes now: the empty line is not the
				// document's last, and only that line waits in the view
				// (record 2026-11-10, seed4/c27). So does the item's
				// whitespace-only continuation line, which Enter rewrites
				// into the next item's line (record 2026-11-10, seed5/c7).
				// The document's last text paragraph splits for real too:
				// Typora writes the empty paragraph Enter opens above the
				// blank tail instead of holding it (record 2026-11-10,
				// seed9/c12). An empty paragraph mid-document splits for real
				// as well: Enter there makes two empty paragraphs of the
				// document, not one line and a view-held one (record
				// 2026-11-12, seed13/c2). Only a non-empty paragraph keeps
				// the paragraph it opens in the view.
				if at, n := v.closingRunBeforeBreak(intent, shift, a, z); n > 0 {
					// Enter with the caret strictly inside a closing delimiter run
					// takes the rune after it: the paragraph above keeps a run one
					// rune short and the break opens below it (record 2026-11-13,
					// seed20/c2). A split this writes itself takes the same rune in
					// enterBlock; this covers the paragraph end, where the break
					// waits in the view and no split branch runs. The rune goes
					// first, so the break anchors at the shortened run's end.
					v.takeRuneAt(at, n)
					atEOF = v.shown.paraAt(v.caret) == len(v.paras)-1
					source = v.paragraphEndSource(v.shown.paraAt(v.caret))
				}
				if v.openTransient(intent, source, atEOF) {
					return true
				}
			}
			hold := 0
			if intent == IntentEnter {
				p := v.shown.paraAt(v.caret)
				hold = v.trailingEmptyParagraphs(p + 1)
			}
			v.record(false)
			v.command(func(d *Document) {
				_ = d.applyIntent(EditRequest{Intent: intent, Start: a, End: z})
			})
			if hold > 0 {
				v.holdTrailingSplit(hold)
			}
			v.mode, v.coalesce = newStep, false
		} else {
			v.enter()
		}
		return true
	case ui.KeyTab:
		if !shift && m == 0 && v.selAnchor == v.caret && v.doc != nil && v.doc.md != nil {
			v.doc.lock()
			src, at := v.doc.md.p.String(), v.doc.selection.Caret.Offset
			v.doc.mu.Unlock()
			if src == "**" && at == 1 {
				// Typora turns a Tab typed between the two stars of the empty
				// emphasis pair Cmd+I wrote on a blank document into a space: the
				// line becomes a bullet item whose marker is the left star and
				// whose content starts with the space, leaving the right star as
				// its text (`* *`, record 2026-11-13, seed22/c7). The caret stays
				// at that content start, where the space is not drawn. The bytes
				// are the whole state this rule reads: a pair that opens the
				// paragraph is not held as an empty container (retainEmptyPair
				// keeps only a pair after text), so v.empty is nil here.
				v.replaceSource(at, at, " ", at+1)
				return true
			}
		}
		switch {
		case m != 0:
			return false
		case !shift && v.replaceSourceSelection("\t"):
			// A selection over hidden syntax markers only, as the closing
			// `**` after a caret moved past it, leaves the document
			// selection collapsed while the source selection still covers
			// bytes. Typora's Tab replaces those bytes: the emphasis loses
			// its closer and both delimiters show as literal text (VM
			// record-20261112-064820, seed11/c9).
			return true
		case st.List != NoList:
			if shift {
				if v.doc.md != nil {
					// Shift+Tab leaves the list for every item the selection
					// covers, the caret's item and an empty item its far end
					// reaches: Typora lifts the empty last item's line too and
					// holds the paragraph that exit opens at the document's end
					// (record 2026-11-13, seed21/c5).
					liftAt, finalEOL := -1, false
					v.doc.lock()
					sel := v.doc.selection
					src := v.doc.md.p.String()
					v.doc.mu.Unlock()
					if end := max(sel.Anchor.Offset, sel.Caret.Offset); end == len(src) &&
						sel.Anchor.Offset != sel.Caret.Offset &&
						lineAt(src, end).start != lineAt(src, sel.Caret.Offset).start {
						switch v.doc.emptyBlockAt(end) {
						case emptyBlockTopItem, emptyBlockOnlyTopItem:
							liftAt = end
							finalEOL = strings.HasSuffix(src, "\n") || strings.HasSuffix(src, "\r")
						}
					}
					var lift emptyLift
					lifted := false
					v.command(func(d *Document) {
						if liftAt >= 0 {
							lift, lifted = d.liftEmptyBlock(liftAt)
						}
						_ = d.applyIntent(EditRequest{Intent: IntentOutdent, Start: v.caret, End: v.caret})
					})
					if lifted && !lift.materialized {
						v.openEmptyExitTransient(lift, finalEOL)
					}
					return true
				}
				return v.run("outdent")
			}
			if v.doc.md != nil && v.doc.firstListItem(v.caret) {
				if v.caret != ps {
					v.insert("\t")
				} else {
					// Typora keeps a tab at the start of the content, where
					// the list cannot indent further, behind an invisible
					// U+200B so that the tab stays text.
					v.insert("\u200b\t")
				}
				return true
			}
			if v.doc.md != nil {
				v.command(func(d *Document) {
					_ = d.applyIntent(EditRequest{Intent: IntentIndent, Start: v.caret, End: v.caret})
				})
				return true
			}
			return v.run("indent")
		case shift && st.Quote && v.doc.md != nil:
			v.command(func(d *Document) {
				_ = d.applyIntent(EditRequest{Intent: IntentOutdent, Start: v.caret, End: v.caret})
			})
			return true
		case shift && v.revertSentinelTab():
			// Shift+Tab takes back the sentinel pair Tab wrote on a paragraph
			// line that cannot indent: the pair and its line leave, and the
			// paragraph waits at the document's end (record 2026-11-12,
			// seed19/c4).
			return true
		case !shift && st.CodeBlock:
			if first, last, ok := v.codeBlockAt(p); ok && v.indentCodeSelection(first, last) {
				return true
			}
			v.insert("\t")
			return true
		case !shift && v.doc.md != nil && st.Heading > 0:
			v.insert("\t")
			return true
		case !shift && v.doc.md != nil && v.caret == ps:
			if v.lineAfterTypedBreak(ps) {
				// The line the session's own break opened keeps the tab as
				// text without the sentinel: the break's bytes are already
				// in the source, so the tab needs no invisible byte to stay
				// a line of its paragraph (record 2026-11-12, seed15/c3).
				v.insert("\t")
				return true
			}
			// A paragraph or quote line that cannot indent further keeps
			// the tab as text behind Typora's invisible U+200B.
			v.insert("\u200b\t")
			return true
		case !shift && v.doc.md != nil:
			v.insert("\t")
			return true
		}
		return false
	}
	return false
}

// releaseEmptyEmphasis drops the empty emphasis pair Cmd+I wrote at the end of
// a paragraph, leaving its two stars as the text they are. Typora opens an
// element for an empty `****` strong pair but not for an empty `**` emphasis
// one: the stars stay literal, so a caret that leaves the paragraph leaves
// them in the bytes (record 2026-11-13, seed18/c11 against seed4/c18's strong
// pair; seed10/c4 shows the same stars with no delimiter revealed while the
// caret stands between them).
func (v *view) releaseEmptyEmphasis() {
	e := v.empty
	if e == nil || !e.pair || e.box.kind != MarkerEmphasis {
		return
	}
	v.empty = nil
}

// quotedAt reports whether the pending break's paragraph lies in a quote. A
// quote's paragraph keeps the tab the break opened as its own line rather
// than lifting out of the container (record 2026-11-13, seed18/c9).
func (t *transientBlock) quotedAt() bool {
	if t.preview == nil {
		return false
	}
	at := t.preview.SourceSelection().Caret.Offset
	doc := t.preview.docAtSource(at)
	if doc < 0 {
		return false
	}
	st := t.preview.text.ParagraphStyleAt(doc)
	return st.Quote && st.List == NoList
}

// materializePendingBreakTab writes the tab the caret typed on the line a
// pending break opened: the break's own bytes stay, and the tab follows them
// as the paragraph's content. A paragraph has no indent to indent, so Typora
// keeps the tab as text; the break the view held gives the line its bytes, so
// no sentinel is needed to keep it (record 2026-11-12, seed15/c3). It reports
// whether it wrote the tab.
func (v *view) materializePendingBreakTab() bool {
	t := v.transient
	if t == nil || v.opts.ReadOnly || v.doc.md == nil {
		return false
	}
	if st := v.para(t.para).style; st.List != NoList || st.Heading > 0 || st.CodeBlock {
		return false
	}
	if !t.plainAtEOF() && !t.quotedAt() {
		return false
	}
	// A quoted paragraph holds the tab the same way: the held break writes the
	// quote's continuation prefix and the tab follows it on that line, so the
	// view shows `alphanxgb\n\t` and the source `> alphanxgb\n> \t`
	// (record 2026-11-13, seed18/c9).
	return v.materializeTransient(func(e *Edit, at int, attrs Attributes) error {
		// at is a document offset, and the quote prefix before the tab is a gap in
		// the document text rather than bytes the tab can stand after. The held
		// line write position is the tab byte in the source, so a quoted
		// paragraph writes `> alphanxgb\n> \t` and a plain one its own line
		// (record 2026-11-13, seed18/c9 against seed15/c3).
		e.ReplaceSource(t.write, t.write, "\t")
		return e.Err()
	}, true)
}

// lineAfterTypedBreak reports whether the source line holding paragraph p
// follows a line break this session typed: the paragraph the break opened
// already has bytes of its own, so a tab at its start stays text without a
// sentinel (record 2026-11-12, seed15/c3).
func (v *view) lineAfterTypedBreak(p int) bool {
	if v.doc == nil || v.doc.md == nil || p < 0 || p >= v.source.Len() {
		return false
	}
	v.doc.lock()
	defer v.doc.mu.Unlock()
	line := lineAt(v.doc.md.p.String(), v.source.start(p))
	return line.start > 0 && v.doc.state().isLiveLine(line.start-1)
}

// revertSentinelTab takes back the tab Tab wrote on a paragraph line that
// cannot indent, where the tab stands as text behind the invisible U+200B
// that keeps it one. Shift+Tab is that key's reverse, so the pair leaves
// with the line that spells it and the paragraph returns to the view-held
// empty paragraph at the document's end, as Typora's does (record
// 2026-11-12, seed19/c4). A quote, item, heading or code line keeps the
// sentinel and is outdented by the branches above. It reports whether it
// took the pair away.
func (v *view) revertSentinelTab() bool {
	p := v.shown.paraAt(v.caret)
	start := v.shown.paraStart(p)
	pairEnd := start + utf8.RuneCountInString("\u200b\t")
	end := start + v.para(p).n
	v.doc.lock()
	wrotePair := false
	if n := len(v.doc.undo); n > 0 {
		for _, patch := range v.doc.undo[n-1].patches {
			if patch.old == "" && strings.HasSuffix(patch.new, "\u200b\t") {
				wrotePair = true
			}
		}
	}
	v.doc.mu.Unlock()
	if v.selAnchor != v.caret || v.caret != end &&
		!(v.caret == pairEnd && wrotePair) {
		// Shift+Tab takes the pair back from the start of the line Tab
		// opened, from the caret Tab left after the pair when text follows
		// it, or from the end of the bare pair (record 2026-11-14,
		// seed23/c7; record 2026-11-12, seed19/c4). A selection or any
		// other caret is not that pair's key.
		return false
	}
	if st := v.para(p).style; st.List != NoList || st.Quote || st.Heading > 0 || st.CodeBlock {
		return false
	}
	para, at, ok := v.sentinelTabPair()
	if !ok || para != p {
		return false
	}
	content := v.shown.Slice(start, start+v.para(p).n).String()
	if !strings.HasPrefix(content, "\u200b\t") ||
		(content != "\u200b\t" || v.caret != end) && !wrotePair {
		return false
	}
	return v.withdrawSentinelTabLine(at)
}

// indentCodeSelection indents the lines a code block's selection touches by
// the code editor's two spaces. Typora's code editor indents the lines the
// selection covers instead of replacing the bytes it holds, and reports the
// lines it rewrote as a selection running forward (record 2026-11-12,
// seed15/c8). It reports whether it wrote the selection's lines.
func (v *view) indentCodeSelection(first, last int) bool {
	a, z := v.selection()
	if a == z || v.opts.ReadOnly || v.doc == nil || v.doc.md == nil {
		return false
	}
	if v.shown.paraAt(a) < first {
		a = v.shown.paraStart(first)
	}
	if end := v.shown.paraStart(last) + v.para(last).n; z > end {
		z = end
	}
	d := v.doc
	d.lock()
	from, to := d.sourceAtDoc(a), d.sourceAtDoc(z)
	src := d.md.p.String()
	d.mu.Unlock()
	if from > to {
		from, to = to, from
	}
	if from >= to {
		return false
	}
	start, stop := lineAt(src, from), lineAt(src, to-1)
	if start.start > stop.start {
		return false
	}
	v.command(func(d *Document) { d.indentCodeLines(start.start, stop.start) })
	return true
}

// materializeDeferredItemExit takes Enter on the empty list item a split at
// the document's end holds in the view. Typora writes that item's line when
// the split happens, so the exit a second Enter performs lifts bytes: the
// marker leaves, the line ending stays, and the paragraph the exit opens
// waits in the view. The lift is a step of undo of its own, so Undo returns
// to the split's two-item list (record 2026-11-12, seed12/c7).
func (v *view) materializeDeferredItemExit() bool {
	t := v.transient
	if t == nil || t.isLineBreak() || t.mode == transientContainerExit || t.marker != "" ||
		!t.atEOF || t.finalEOL || t.preview == nil || v.doc.md == nil {
		// A document that already ends in a line ending keeps the exit's
		// bytes to the line the split wrote; only a document whose last
		// line holds no ending has to write that ending now.
		return false
	}
	src := v.doc.Source()
	preview := t.preview.Source()
	if t.write <= len(src) || len(preview) <= len(src) || !strings.HasPrefix(preview, src) {
		// The item's own line is in the document already: Enter lifts it
		// there, through the empty-block branch of the key handler.
		return false
	}
	switch t.preview.emptyBlockAt(t.write) {
	case emptyBlockOnlyTopItem, emptyBlockTopItem:
	default:
		// A nested empty item joins the item above it and a quote line
		// keeps the paragraph inside its quote: neither exits to a
		// top-level paragraph the split could have written.
		return false
	}
	// The split's item line becomes the document's, as a step of its own.
	v.record(false)
	if !v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
		return false
	}
	base := v.doc.Source()
	finalEOL := strings.HasSuffix(base, "\n") || strings.HasSuffix(base, "\r")
	var lift emptyLift
	var ok bool
	v.command(func(d *Document) { lift, ok = d.liftEmptyBlock(len(base)) })
	if !ok {
		return true
	}
	if lift.materialized {
		// The lift wrote the paragraph it exits to into the document: the
		// view holds no transient for it.
		return true
	}
	v.openEmptyExitTransient(lift, finalEOL)
	next := v.transient
	if next == nil {
		return true
	}
	v.doc.lock()
	if n := len(v.doc.undo); n > 0 {
		// The exit paragraph is what the lift leaves behind: Undo takes the
		// lift back to the split's bytes and Redo brings the paragraph back.
		v.doc.undo[n-1].afterTransient = next.undo(v.doc.md.p.String())
	}
	v.doc.mu.Unlock()
	return true
}

// blankListLine reports whether the caret's paragraph is the whitespace-only
// continuation line the projection gives to the list item above: Enter there
// writes the next item's line instead of waiting in the view.
func (v *view) blankListLine() bool {
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= len(v.paras) || v.doc == nil || v.doc.md == nil {
		return false
	}
	if v.para(p).style.List == NoList {
		return false
	}
	return v.doc.blankLineItem(v.source.start(p)) >= 0
}

// materializeHeldBreakOnEmptyItem writes the next item's line for Enter on
// the break Shift+Enter held on an empty item's own line. The held line is
// the item's continuation paragraph, so Enter ends the item and opens its
// sibling at the item's marker column, as it does on a continuation line the
// bytes already hold (record 2026-11-10, seed5/c7); dropping the break alone
// would exit the list from an item that still shows a second line (record
// 2026-11-13, seed16/c12).
func (v *view) materializeHeldBreakOnEmptyItem() bool {
	if v.opts.ReadOnly || v.doc == nil || v.doc.md == nil {
		return false
	}
	d := v.doc
	d.lock()
	base := d.md.p.String()
	live := d.state().liveBreaks
	sel := d.selection
	d.mu.Unlock()
	t := v.transient
	if t == nil {
		if sel.Anchor.Offset != sel.Caret.Offset || len(live) != 1 {
			return false
		}
		first := lineAt(base, live[0])
		if first.start != 0 || first.stop >= len(base) ||
			!d.md.openedListStartAt(first.start) ||
			sel.Caret.Offset != lineAt(base, first.stop).end ||
			strings.TrimSpace(base[lineAt(base, first.stop).start:sel.Caret.Offset]) == "" {
			return false
		}
		// Enter on content typed into the live continuation line folds it
		// back into the empty item and writes its sibling: the bytes, not a
		// view transient, own the new item (record-20261114-033056 /
		// 033730, seed23/c10 cp1-cp5).
		v.record(false)
		v.command(func(d *Document) {
			_ = d.applyIntent(EditRequest{Intent: IntentEnter, Start: sel.Caret.Offset, End: sel.Caret.Offset})
		})
		v.mode, v.coalesce = newStep, false
		return true
	}
	if t.isEmptyParagraph() || t.brk.shown != 1 {
		return false
	}
	d.lock()
	var at int
	var marker, src, eol string
	src = d.md.p.String()
	held := previousMarkdownLine(src, t.anchor.Offset)
	for i := 1; i < d.md.p.Block(0).Next; i++ {
		b := d.md.p.Block(i)
		if b.Kind != parser.Item || b.Start < held.start || b.Start > held.end {
			continue
		}
		first := lineAt(src, b.Start)
		if first.start != held.start || held.end != first.end {
			// The break sits on a continuation line of the item, not on the
			// item's own line.
			continue
		}
		content := first.end
		for child := range d.md.p.Children(i) {
			if start := d.md.p.Block(child).Start; start >= b.Start {
				content = min(start, first.end)
			}
			break
		}
		if content != first.end || strings.TrimSpace(src[content:first.end]) != "" {
			// The item holds content of its own: Enter there acts on that
			// content, not on a held line below it.
			continue
		}
		at, marker = first.end, src[b.Start:lineAt(src, b.Start).end]
		eol = defaultEOL(src)
	}
	d.mu.Unlock()
	if marker == "" || at < 0 || at > len(src) {
		return false
	}
	caret := at + len(eol) + len(marker)
	v.mode, v.coalesce = newStep, false
	if err := v.editSource(caret, func(e *Edit) {
		e.ReplaceSource(at, at, eol+marker)
	}); err != nil {
		return false
	}
	v.discardTransient()
	v.setSourceSelection(sourceCaret(caret))
	v.reveal = true
	return v.transient == nil
}

// writeHeadingItemContinuation writes the continuation line Shift+Enter opens
// below a heading typed inside a list item, as the empty item ruling R2-C
// spells it. Typora serializes the line as bare whitespace, which §5.5 bans
// and a reopen reads as the paragraph's tail (seed16/c5, seed9/c15).
func (v *view) writeHeadingItemContinuation() bool {
	if v.doc == nil || v.doc.md == nil || v.opts.ReadOnly || v.transient != nil {
		return false
	}
	a, z := v.selection()
	if a != z {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if p < 0 || p >= v.shown.ParagraphCount() || v.para(p).style.Heading == 0 {
		return false
	}
	ps := v.shown.paraStart(p)
	if v.caret != ps+v.para(p).n {
		return false
	}
	d := v.doc
	d.lock()
	loc := d.paragraphLocation(p)
	inItem := loc.item >= 0
	d.mu.Unlock()
	if !inItem {
		return false
	}
	v.record(false)
	v.mode, v.coalesce = newStep, false
	caret, wrote := 0, false
	if err := v.editSource(v.caret, func(e *Edit) {
		caret, wrote = e.enterHeadingItemContinuation(loc)
	}); err != nil || !wrote {
		return false
	}
	v.setSourceSelection(sourceCaret(caret))
	v.reveal = true
	return true
}

// typesText reports whether key may produce text before an InputText or
// ReplaceText event. Those events need the transient paragraph to still be
// present so their edit can materialize it.
func typesText(key ui.Key, mods ui.Modifiers) bool {
	if mods&^(ui.Shift|ui.Alt) != 0 {
		return false
	}
	switch key {
	case ui.KeyUnknown, ui.KeySpace, ui.KeyMinus, ui.KeyEqual, ui.KeyComma, ui.KeyPeriod,
		ui.KeySlash, ui.KeySemicolon, ui.KeyQuote, ui.KeyBracketLeft, ui.KeyBracketRight,
		ui.KeyBackslash, ui.KeyBackquote:
		return true
	}
	return key >= ui.Key0 && key <= ui.Key9 || key >= ui.KeyA && key <= ui.KeyZ
}

// shortcut returns the command a key with mods runs: ⌘B, ⌘I, ⌘U, ⇧⌘X, ⌘E,
// ⌘K, ⇧⌘7, ⇧⌘8, ⇧⌘L, ⌘], ⌘[, ⌘{, ⌘|, ⌘} and ⌥⌘0–6 on macOS, as Pages,
// Notes and Google Docs have them, with Control in place of Command
// elsewhere, where Ctrl+L, Ctrl+E, Ctrl+R align, as in Word.
func shortcut(key ui.Key, mods ui.Modifiers) (string, bool) {
	mac := runtime.GOOS == "darwin"
	switch mods {
	case ui.Cmd:
		if key >= ui.Key0 && key <= ui.Key6 {
			return "heading" + string(rune('0'+int(key-ui.Key0))), true
		}
		switch key {
		case ui.KeyB:
			return "bold", true
		case ui.KeyI:
			return "italic", true
		case ui.KeyU:
			return "underline", true
		case ui.KeyK:
			return "link", true
		case ui.KeyBracketRight:
			return "indent", true
		case ui.KeyBracketLeft:
			return "outdent", true
		case ui.KeyE:
			if mac {
				return "code", true
			}
			return "alignCenter", true
		case ui.KeyL:
			if !mac {
				return "alignLeft", true
			}
		case ui.KeyR:
			if !mac {
				return "alignRight", true
			}
		}
	case ui.Cmd | ui.Shift:
		switch key {
		case ui.KeyC:
			return "copyMarkdown", true
		case ui.KeyX:
			return "strikethrough", true
		case ui.Key7:
			return "numbers", true
		case ui.Key8:
			return "bullets", true
		case ui.KeyL:
			return "tasks", true
		case ui.KeyBracketLeft:
			if mac {
				return "alignLeft", true
			}
		case ui.KeyBracketRight:
			if mac {
				return "alignRight", true
			}
		case ui.KeyBackslash:
			if mac {
				return "alignCenter", true
			}
		case ui.KeyE:
			if !mac {
				return "code", true
			}
		}
	case ui.Cmd | ui.Alt:
		if key >= ui.Key0 && key <= ui.Key6 {
			return "heading" + string(rune('0'+int(key-ui.Key0))), true
		}
	}
	return "", false
}

// editCommand performs an edit command of a menu or a shortcut.
func (v *view) editCommand(name string) bool {
	a, z := v.selection()
	switch name {
	case "copy":
		if a != z {
			v.copySelection()
		}
		return true
	case "copyMarkdown":
		if a != z {
			v.copySelection(true)
		}
		return true
	case "cut":
		if a != z && !v.opts.ReadOnly {
			v.cutSelection()
		}
		return true
	case "paste", "pasteAndMatchStyle":
		if !v.opts.ReadOnly {
			v.paste(name == "pasteAndMatchStyle")
		}
		return true
	case "selectAll":
		v.doc.lock()
		v.setSelectionLocked(0, v.shown.Len(), BoundaryUpstream, BoundaryDownstream)
		v.doc.mu.Unlock()
		return true
	case "delete":
		if a != z && !v.opts.ReadOnly {
			v.deleteRange(a, z, false)
		}
		return true
	case "undo", "redo":
		if !v.opts.ReadOnly {
			v.commitCompose()
			if v.transient != nil {
				// Typora's undo at the start of the history of an empty
				// document has nothing to take back and keeps the empty
				// paragraph the undo before it revealed.
				if name == "undo" && v.transient.isEmptyParagraph() &&
					v.doc.Source() == "" && !v.doc.CanUndo() {
					return true
				}
				// A break that stands for the bytes the last step removed
				// leaves with that step, which restores them; undo does not
				// drop the break alone.
				if name == "undo" && v.transient.mode == transientEmptiedBreak {
					v.takeBack(false)
					return true
				}
				if name == "undo" {
					base := v.doc.Source()
					if v.seed24Undo(base, v.transient) {
						return true
					}
				}
				if name == "redo" && !v.doc.CanRedo() {
					// Nothing to redo: the empty block the last step left
					// stays in the view, as in Typora.
					return true
				}
				if name == "undo" {
					// The block in the view is what the last step's
					// materialization left: undo takes that step back and
					// restores the block it replaced, as Typora does.
					v.doc.lock()
					n := len(v.doc.undo)
					restore := n > 0 && v.doc.undo[n-1].afterTransient != nil
					v.doc.mu.Unlock()
					if restore {
						v.takeBack(false)
						return true
					}
				}
				v.discardTransient()
				if name == "undo" {
					return true
				}
			}
			v.takeBack(name == "redo")
			if name == "redo" {
				// Redo puts a fenced block's language input back in focus:
				// the step it restores left the document's caret at the
				// block's code content start, and the input takes it back
				// there (record 2026-11-12, seed18/c5).
				v.resumeFenceLanguageFocus()
			}
		}
		return true
	}
	return v.run(name)
}

// press handles a press of the pointer.
func (v *view) press(ev ui.InputEvent) bool {
	if ev.Button == 1 && !v.overScrollbar(ev.X) {
		// A right-click outside the selection puts the caret where it
		// is, as in a TextArea, so that the context menu acts there; the
		// press goes on to open the menu.
		x, y := v.contentPoint(ev.X, ev.Y)
		i, aff := v.positionAt(x, y)
		if a, b := min(v.selAnchor, v.caret), max(v.selAnchor, v.caret); a == b || i < a || i > b {
			v.commitCompose()
			v.hasDesired = false
			v.moveTo(i, aff, false)
		}
		return false
	}
	if ev.Button != 0 {
		return false
	}
	v.wordMove = false
	v.pressMods = ev.Mods
	v.pressLink = ""
	v.pressPoint = ui.Point{X: ev.X, Y: ev.Y}
	if v.overScrollbar(ev.X) {
		v.thumb = true
		top, size := v.thumbBox()
		if ev.Y >= top && ev.Y < top+size {
			v.thumbGrab = ev.Y - top
		} else {
			v.thumbGrab = size / 2
			v.dragThumb(ev.Y)
		}
		return true
	}
	if v.lang != nil {
		// A press in the document leaves the language input and writes what
		// it shows, then acts where it pressed.
		v.commitFenceLanguage()
	}
	v.commitCompose()
	x, y := v.contentPoint(ev.X, ev.Y)
	if v.transient != nil {
		p, _, o, _ := v.displayPositionAt(x, y)
		// The empty paragraph is a hit target of its transient wherever it
		// is pressed. The pending line break is one only on the empty line
		// after it: a press on the text line moves the caret off the break.
		keep := p == v.transient.para
		if v.transient.isLineBreak() {
			keep = keep && o.Doc == v.shown.paraStart(p)+v.para(p).n && ev.Clicks == 1 && ev.Mods == 0
			if keep {
				// The caret is already on the empty line, which holds no
				// bytes: it stays there, in the view.
				return true
			}
		}
		if !keep {
			if v.transient.isEmptyParagraph() &&
				v.materializeTransient(func(*Edit, int, Attributes) error { return nil }, true) {
				// Typora keeps the empty paragraph and writes its line when
				// the press leaves it; the click then moves the caret into
				// the document (record 2026-11-10, seed4/c14, seed5/c2).
				// The materialization gives the paragraphs new source
				// offsets and clears the heights its view was laid out
				// from, so lay the view out again before reading where the
				// press landed (record 2026-11-10, seed6/c9).
				if v.width > 0 {
					v.layout(v.width, v.viewH)
					x, y = v.contentPoint(ev.X, ev.Y)
				}
			} else {
				v.discardTransient()
			}
		}
	}
	if v.empty != nil {
		probePara, _, probe, _ := v.displayPositionAt(x, y)
		if probePara < 0 || probe.Source <= v.empty.box.start || probe.Source >= v.empty.box.end {
			v.removeEmpty()
		}
	}
	if p, ok := v.taskBox(x, y); ok && ev.Clicks == 1 && !v.opts.ReadOnly {
		start := v.shown.paraStart(p)
		v.command(func(d *Document) { d.ToggleChecked(start) })
		return true
	}
	p, _, o, aff := v.displayPositionAt(x, y)
	if v.pressFinalBlankLine(p) {
		// Typora draws the empty paragraph the final blank line shows but
		// hit-tests a press on it as the document's end, at the block
		// above (record 2026-11-10, seed7/c11). Like a press past the last
		// block, it takes no source offset of its own.
		q := p - 1
		o.Doc = v.shown.paraStart(q) + v.para(q).n
		aff = ui.TextDownstream
		p = -1
	}
	if ev.Clicks == 1 && ev.Mods == 0 && !v.opts.ReadOnly {
		codeStart := v.pressCodeStart(p, o, x)
		if v.belowFinalCode(y) || codeStart {
			last := len(v.paras) - 1
			if v.openTransient(IntentExitBlock, v.paragraphEndSource(last), true) {
				if codeStart {
					// The empty paragraph is the file's final line, which the
					// document already holds: saving writes no bytes for it
					// (record 2026-11-10, seed9/c10).
					v.transient.mode = transientPhantomEOF
				}
				return true
			}
		}
	}
	i := o.Doc
	if ev.Clicks == 1 && ev.Mods&ui.Shift == 0 {
		if v.doc.md != nil {
			if i < v.shown.Len() {
				v.pressLink = v.shown.AttributesAt(i).Link
			}
			if p >= 0 {
				if _, b, ok := v.linkBoxAt(p, o.Source); ok {
					v.pressLink = v.attributesAt(p, b.docStart).Link
				}
			}
		} else {
			v.pressLink = v.linkAt(x, y)
		}
	}
	v.dragging = true
	v.hasDesired = false
	v.dragUnit = min(max(ev.Clicks, 1), 3)
	switch v.dragUnit {
	case 1:
		v.moveTo(i, aff, ev.Mods&ui.Shift != 0)
		if p >= 0 && v.doc.md != nil {
			sourceAffinity := BoundaryDownstream
			if o.SourceAfter > o.Source {
				sourceAffinity = BoundaryUpstream
			}
			v.doc.lock()
			anchor := v.doc.selection.Anchor
			if ev.Mods&ui.Shift == 0 {
				anchor = sourcePoint(o.Source, sourceAffinity)
			}
			v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: sourcePoint(o.Source, sourceAffinity)})
			v.anchorAffinity, v.caretAffinity = v.doc.selection.Anchor.Affinity, v.doc.selection.Caret.Affinity
			v.doc.mu.Unlock()
		}
	case 2:
		a, z := v.wordAt(i)
		v.doc.lock()
		v.setSelectionLocked(a, z, BoundaryUpstream, BoundaryDownstream)
		v.doc.mu.Unlock()
	default:
		a, z := v.shown.ParagraphRange(i)
		v.doc.lock()
		v.setSelectionLocked(a, min(z+1, v.shown.Len()), BoundaryUpstream, BoundaryDownstream)
		v.doc.mu.Unlock()
	}
	v.dragStart = [2]int{v.selAnchor, v.caret}
	v.coalesce = false
	return true
}

// move handles the pointer moving: a drag extends the selection.
func (v *view) move(ev ui.InputEvent) bool {
	if v.thumb {
		v.dragThumb(ev.Y)
		return true
	}
	x, y := v.contentPoint(ev.X, ev.Y)
	if !v.dragging {
		cursor := ui.CursorText
		hoverChanged := false
		p, local, o, _ := v.displayPositionAt(x, y)
		linkIndex, _, _ := v.linkBoxAt(p, o.Source)
		link := ""
		if p >= 0 && o.Doc < v.shown.Len() {
			link = v.shown.AttributesAt(o.Doc).Link
		}
		if (linkIndex >= 0 || link != "") && v.pointerPastLine(p, local, x, y) {
			link, linkIndex = "", -1
		}
		if linkIndex >= 0 || link != "" {
			if v.hoverPara != p || v.hoverBox != linkIndex || v.hoverLink != link {
				v.hoverPara, v.hoverBox, v.hoverLink = p, linkIndex, link
				hoverChanged = true
			}
			cursor = ui.CursorPointer
		} else if v.hoverPara >= 0 || v.hoverBox >= 0 || v.hoverLink != "" {
			v.hoverPara, v.hoverBox, v.hoverLink = -1, -1, ""
			hoverChanged = true
		}
		if _, box := v.taskBox(x, y); box || v.overScrollbar(ev.X) {
			cursor = ui.CursorDefault
		} else if v.linkAt(x, y) != "" {
			cursor = ui.CursorPointer
		}
		if cursor != v.cursor {
			v.cursor = cursor
			return true
		}
		return hoverChanged
	}
	// Near the edges, a drag scrolls.
	if ev.Y < v.padY {
		v.setScroll(max(0, v.scroll-float64(v.padY-ev.Y)))
	} else if ev.Y > v.padY+v.viewH {
		v.setScroll(v.scroll + float64(ev.Y-v.padY-v.viewH))
	}
	p, _, o, aff := v.displayPositionAt(x, y)
	i := o.Doc
	switch v.dragUnit {
	case 2:
		s, e := v.wordAt(i)
		if i < v.dragStart[0] {
			v.doc.lock()
			v.setSelectionLocked(v.dragStart[1], s, BoundaryDownstream, BoundaryUpstream)
			v.doc.mu.Unlock()
		} else {
			v.doc.lock()
			v.setSelectionLocked(v.dragStart[0], max(e, v.dragStart[1]), BoundaryUpstream, BoundaryDownstream)
			v.doc.mu.Unlock()
		}
	case 3:
		a, z := v.shown.ParagraphRange(i)
		if i < v.dragStart[0] {
			v.doc.lock()
			v.setSelectionLocked(v.dragStart[1], a, BoundaryDownstream, BoundaryUpstream)
			v.doc.mu.Unlock()
		} else {
			v.doc.lock()
			v.setSelectionLocked(v.dragStart[0], min(z+1, v.shown.Len()), BoundaryUpstream, BoundaryDownstream)
			v.doc.mu.Unlock()
		}
	default:
		v.affinity = aff
		if v.doc.md == nil {
			v.doc.lock()
			anchor := v.doc.selection.Anchor
			v.setSelectionLocked(v.dragStart[0], i, anchor.Affinity, BoundaryDownstream)
			v.doc.mu.Unlock()
		}
		if p >= 0 && v.doc.md != nil {
			sourceAffinity := BoundaryDownstream
			if o.SourceAfter > o.Source {
				sourceAffinity = BoundaryUpstream
			}
			v.doc.lock()
			anchor := v.doc.selection.Anchor
			v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: sourcePoint(o.Source, sourceAffinity)})
			v.anchorAffinity, v.caretAffinity = v.doc.selection.Anchor.Affinity, sourceAffinity
			v.doc.mu.Unlock()
		}
	}
	dx, dy := ev.X-v.pressPoint.X, ev.Y-v.pressPoint.Y
	if i != v.dragStart[0] || v.dragUnit != 1 || dx*dx+dy*dy >= 16 {
		v.pressLink = ""
	}
	v.reveal = true
	return true
}

// pointerPastLine reports whether (x, y), in the content, is to the right of
// the last glyph on the caret's visual line in paragraph p, whose display
// offset under the pointer is local. Hit testing names the nearest caret, so
// the empty space past a line's end lands on that line's last caret; hovering
// there is hovering the paragraph, not its last glyph (record 2026-11-13,
// seed17/c1: the tester's focus press left such a hover, drew the link's
// destination and put the caret 25.9px right of the window's).
func (v *view) pointerPastLine(p, local int, x float32, y float64) bool {
	if p < 0 {
		return false
	}
	pv := v.paraLayout(p)
	if pv == nil || pv.layout == nil || len(pv.units) == 0 {
		return false
	}
	local = max(0, min(local, len(pv.units)-1))
	line := pv.layout.VisualLine(ui.TextCaretPosition{Index: pv.units[local]})
	last := local
	for q := local + 1; q < len(pv.units); q++ {
		if pv.layout.VisualLine(ui.TextCaretPosition{Index: pv.units[q]}) != line {
			break
		}
		last = q
	}
	if local != last {
		// The nearest caret is inside the line, so the pointer is not past it.
		return false
	}
	ol, oy := v.origin(p)
	end := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[last]}).X
	// The line's own height bounds the paragraph's empty space below it too.
	return x-ol > end || y-oy >= float64(pv.h)
}

// release ends a drag, and opens the link a click began on.
func (v *view) release(ev ui.InputEvent) bool {
	if v.thumb {
		v.thumb = false
		return true
	}
	if !v.dragging {
		return false
	}
	v.dragging = false
	link := v.pressLink
	v.pressLink = ""
	mods := v.pressMods
	open := mods&ui.Cmd != 0 || runtime.GOOS != "darwin" && mods&ui.Ctrl != 0
	x, y := v.contentPoint(ev.X, ev.Y)
	dx, dy := ev.X-v.pressPoint.X, ev.Y-v.pressPoint.Y
	plain := v.doc.md == nil
	clicked := v.caret == v.selAnchor && !ev.Canceled && dx*dx+dy*dy < 16 && v.linkAt(x, y) == link
	if (open || plain) && link != "" && clicked {
		if strings.HasPrefix(link, "#") {
			if at, ok := v.fragmentTarget(link); ok {
				v.moveTo(at, ui.TextDownstream, false)
			}
			return true
		}
		if v.opts.OnLink != nil {
			v.opts.OnLink(link)
		} else if ui.SafeToOpen(link) {
			v.services.OpenURL(link)
		}
	}
	return true
}

// Attachments occupy one rune, but either half of their drawn box opens
// the link. Keep nearest-caret hit testing for selection alone.
func (v *view) linkAt(x float32, y float64) string {
	p, l, _ := v.layoutPositionAt(x, y)
	if p < 0 {
		return ""
	}
	pv := v.paraLayout(p)
	ox, oy := v.origin(p)
	for _, object := range pv.layout.Objects() {
		if object.Rect.Contains(x-ox, float32(y-oy)) {
			at := pv.runeOfUnit(object.Index)
			start := v.caret - v.shown.paraStart(p)
			if v.compose != "" && v.shown.paraAt(v.caret) == p && at >= start && at < start+len([]rune(v.compose)) {
				return ""
			}
			return v.shown.AttributesAt(v.global(p, at)).Link
		}
	}
	i := v.global(p, l)
	if i < v.shown.Len() {
		a := v.shown.AttributesAt(i)
		if a.Attachment == nil {
			return a.Link
		}
	}
	return ""
}

// taskBox reports whether (x, y), in the content, is on the check box of
// a task, and which paragraph's.
func (v *view) taskBox(x float32, y float64) (int, bool) {
	if len(v.paras) == 0 {
		return 0, false
	}
	p := v.hs.at(y)
	st := v.para(p).style
	if st.List != Tasks {
		return 0, false
	}
	left, _ := v.insets(st)
	if x < left-v.look.indent || x >= left-2 {
		return p, false
	}
	// Beside the first line, where the box is.
	_, top, h := v.caretAt(v.shown.paraStart(p), 0, ui.TextDownstream)
	return p, y >= top && y < top+float64(h)
}

// overScrollbar reports whether x of the element is on the scroll bar.
func (v *view) overScrollbar(x float32) bool {
	if v.opts.NoScrollbar {
		return false
	}
	width := v.barStyle.TrackWidth
	if width <= 0 {
		width = scrollbarWidth
	}
	return v.hs.top(len(v.paras)) > float64(v.viewH) && x >= v.rect.W-width
}

// bar returns the scroll bar, in the element.
func (v *view) bar() ui.Scrollbar {
	return ui.Scrollbar{Box: ui.Rect{W: v.rect.W, H: v.rect.H}, View: v.viewH, Content: float32(v.hs.top(len(v.paras))), Offset: float32(v.scroll), Style: v.barStyle}
}

// thumbBox returns the top and the height of the scroll bar's thumb, in
// the element.
func (v *view) thumbBox() (top, size float32) { return v.bar().Thumb() }

// dragThumb scrolls as the thumb is dragged to y.
func (v *view) dragThumb(y float32) {
	v.setScroll(float64(v.bar().OffsetAt(y - v.thumbGrab)))
}

// menu builds the editor's context menu.
func (v *view) menu(m *ui.Menu) {
	m.EditItems()
}

// pressFinalBlankLine reports whether p is the empty paragraph the document's
// final blank line shows: Typora hit-tests it as the block above.
func (v *view) pressFinalBlankLine(p int) bool {
	if v.doc == nil || v.transient != nil || p <= 0 || p != v.shown.ParagraphCount()-1 || v.para(p).n != 0 {
		return false
	}
	src := v.doc.Source()
	src = strings.TrimSuffix(src, "\n")
	src = strings.TrimSuffix(src, "\r")
	return strings.HasSuffix(src, "\n") || strings.HasSuffix(src, "\r")
}
