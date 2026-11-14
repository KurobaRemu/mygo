package richtext

import (
	"strings"

	"github.com/egoist/mygo/ui/markdown/parser"
)

type livePrefixKind uint8

const (
	livePrefixLiteral livePrefixKind = iota
	livePrefixHeading
	livePrefixFence
	livePrefixAtomic
)

// livePrefix is the block prefix on the line being typed. Literal
// prefixes stay in a paragraph projection until their trigger; a live
// heading is already a heading but still shows its gray marker.
type livePrefix struct {
	kind          livePrefixKind
	lineStart     int
	lineEnd       int
	prefixStart   int
	prefix        string
	displayPrefix string
	exact         bool
	text          string
}

// quoteSpaceEdit records the space prepareBlockPrefix wrote between a
// quote marker and its content, as the byte offset in the prepared text
// where the caller will insert it. insert writes the space as its own
// edit so one Undo takes the space back and leaves the marker literal.
type quoteSpaceEdit struct {
	ok bool
	at int
}

// quoteMarkerEdit records a quote marker the session typed in front of
// spaces the line already holds. Typora writes that marker as text on the
// line and leaves the line's own spaces where they are, then converts the
// line as an edit of its own: Undo takes the conversion back and not the
// byte, so the line keeps its bytes and reads the marker as text (record
// 2026-11-12, probe94/leading-space-gt-undo). lineStart is the line the
// conversion acts on and markerEnd the offset just after the typed marker.
type quoteMarkerEdit struct {
	ok                   bool
	lineStart, markerEnd int
}

// quoteMarkerLive is the quote marker the session just typed at the end
// of a quote-only continuation line. The marker is unfinished, so
// Typora's live view gives it a paragraph of its own: the paragraph above
// keeps its source, and the typed marker becomes a visible prefix of an
// empty paragraph (record 2026-11-11, probe83/bare-gt-lines). lineStart
// and marker hold the line the marker landed on and its bytes; the
// snapshot checks the bytes still spell the marker before reporting its
// paragraph, so a later edit that moves the caret or the line ends it.
type quoteMarkerLive struct {
	ok        bool
	lineStart int
	marker    string
}

// prefixTracker owns the view's live block prefix and the quote-marker
// edits around it. undoneQuoteSpaceLine and wroteLine are -1 when inactive.
type prefixTracker struct {
	live        *livePrefix
	quoteSpace  quoteSpaceEdit
	quoteMarker quoteMarkerEdit
	quoteLive   quoteMarkerLive
	fullSelect  *seed24FullSelectReplacement
	spaceHeld   bool
	// undoneQuoteSpaceLine is the line whose quote space Undo took back,
	// or -1 when the hold is inactive.
	undoneQuoteSpaceLine int
	// wroteLine is the line whose bytes the current typing run wrote,
	// or -1 outside a run.
	wroteLine int
}

func newPrefixTracker() prefixTracker {
	return prefixTracker{undoneQuoteSpaceLine: -1, wroteLine: -1}
}

// seed24FullSelectReplacement is the one edit Typora makes when a session
// replaces every shown rune of an empty-bullets document with a completed
// empty item prefix: the outer item marker stays, the selection's other
// bytes go, and the typed prefix opens the nested item. The prefix owns no
// source bytes and the caret rests at its content start (record
// 2026-11-14, seed24/c9 and seed3/c05).
type seed24FullSelectReplacement struct {
	from, to int
	docFrom  int
	docTo    int
	source   string
	text     string
}

// prefixTarget is the source line a prefix edit acts on. A transient edit
// reads its preview and write offset; a canonical edit reads the document.
type prefixTarget struct {
	src          string
	caret        int
	line         markdownLine
	inTransient  bool
	hasSelection bool
}

func (v *view) activePrefixTarget() prefixTarget {
	if v.transient != nil {
		src := v.transient.preview.Source()
		sel := v.transient.preview.SourceSelection()
		caret := v.transient.write
		return prefixTarget{
			src: src, caret: caret, line: liveLineAt(src, caret), inTransient: true,
			hasSelection: sel.Anchor.Offset != sel.Caret.Offset,
		}
	}
	v.doc.lock()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	src := v.doc.md.p.String()
	return prefixTarget{
		src: src, caret: sel.Caret.Offset, line: liveLineAt(src, sel.Caret.Offset),
		hasSelection: sel.Anchor.Offset != sel.Caret.Offset,
	}
}

// seed24FullSelectReplacement recognizes the first rune of a completed empty
// item prefix that replaces every shown rune of an empty-bullets document.
// The selection spans source [2,len(src)): its low edge is the outer item's
// content start, and the two trailing spaces are the continuation's own
// bytes. Typora commits the whole retyped prefix as one edit, so the outer
// item marker stays and those spaces go instead of expanding the prefix
// twice (record 2026-11-14, seed24/c9).
func (v *view) seed24FullSelectReplacement(s string) (seed24FullSelectReplacement, bool) {
	if v.transient != nil || v.doc == nil || v.doc.md == nil ||
		len(s) != 1 || !strings.ContainsAny(s, "-*+") ||
		v.shown.ParagraphCount() != 2 {
		return seed24FullSelectReplacement{}, false
	}
	if v.pfx.fullSelect != nil {
		return seed24FullSelectReplacement{}, false
	}
	docFrom, docTo := v.selection()
	if docFrom != 0 || docTo != v.shown.Len() || docTo <= docFrom {
		return seed24FullSelectReplacement{}, false
	}
	v.doc.lock()
	sel := v.doc.selection
	src := v.doc.md.p.String()
	sourceFrom := v.sourcePositionLocked(docFrom, BoundaryUpstream).Offset
	sourceTo := v.sourcePositionLocked(docTo, BoundaryDownstream).Offset
	v.doc.mu.Unlock()
	from, to := min(sel.Anchor.Offset, sel.Caret.Offset), max(sel.Anchor.Offset, sel.Caret.Offset)
	if from != sourceFrom || to != sourceTo || from <= 0 || to != len(src) || from >= to ||
		!emptyListItemPrefix(src[:from]) {
		return seed24FullSelectReplacement{}, false
	}
	selected := strings.ReplaceAll(src[from:to], "\r\n", "\n")
	lines := strings.Split(selected, "\n")
	if len(lines) != 3 || lines[0] == "" || lines[1] != "" || lines[2] != "  " {
		return seed24FullSelectReplacement{}, false
	}
	shown := v.shown.String()
	if !strings.HasPrefix(shown, lines[0]) || !strings.HasSuffix(shown, "\n") {
		return seed24FullSelectReplacement{}, false
	}
	return seed24FullSelectReplacement{
		from: from, to: to, docFrom: docFrom, docTo: docTo, source: src, text: s + " ",
	}, true
}

func (v *view) seed24FullSelectCommit(pending seed24FullSelectReplacement) {
	v.pfx.fullSelect = &pending
}

func (v *view) seed24FullSelectInserted(s string) *seed24FullSelectReplacement {
	pending := v.pfx.fullSelect
	if pending == nil || s != pending.text[:1] ||
		!v.seed24FullSelectStillPending(*pending) {
		return nil
	}
	v.pfx.fullSelect = nil
	return pending
}

func (v *view) seed24FullSelectFromFirstRune(s string) *seed24FullSelectReplacement {
	pending, ok := v.seed24FullSelectReplacement(s)
	if !ok {
		return nil
	}
	return &pending
}

func (v *view) seed24FullSelectStillPending(pending seed24FullSelectReplacement) bool {
	if v.doc == nil || v.doc.md == nil || v.doc.Source() != pending.source ||
		v.shown.Len() != pending.docTo {
		return false
	}
	a, z := v.selection()
	return a == pending.docFrom && z == pending.docTo
}

func (v *view) seed24FullSelectFinish() {
	v.pfx.fullSelect = nil
	v.pfx.spaceHeld = true
}

func (v *view) seed24FullSelectExpire(s string) {
	if s != " " {
		v.pfx.fullSelect = nil
		v.pfx.spaceHeld = false
	}
}

func (v *view) seed24FullSelectConsumeSpace(s string) bool {
	if s != " " || !v.pfx.spaceHeld {
		return false
	}
	v.pfx.spaceHeld = false
	return true
}

// prepareBlockPrefix applies the projected source line before the text
// edit, so the parser and source map commit their normal block projection
// at the same time as the bytes. It returns the text to insert, with the
// space Typora writes between a quote marker and its content (see
// quoteMarkerSpace).
func (v *view) prepareBlockPrefix(text string) string {
	return v.planBlockPrefix(text, true)
}

// preparePastedPrefix applies the line projection to text the pasting path
// writes. Typora's block shortcut fires on typed input only: a paste into a
// line the session holds literal leaves the markers as text, so an
// unfinished ATX prefix keeps its marker in the paragraph the session sees
// (record 2026-11-12, seed16/c13: `# ` typed then `e` pasted stays a
// paragraph showing `# e`).
func (v *view) preparePastedPrefix(text string) string {
	return v.planBlockPrefix(text, false)
}

func (v *view) planBlockPrefix(text string, typed bool) string {
	// The hold is for the state Undo restored; the next text edit ends it.
	// The live quote marker outlives its own edit: Undo of later input
	// restores the line the marker stands on (record 2026-11-11,
	// probe83/bare-gt-lines), and the snapshot checks the bytes still
	// spell it before reporting the marker's paragraph.
	v.pfx.undoneQuoteSpaceLine = -1
	v.pfx.quoteSpace = quoteSpaceEdit{}
	v.pfx.quoteMarker = quoteMarkerEdit{}
	v.seed24FullSelectExpire(text)
	if v.opts.NoMarkdown || v.opts.ReadOnly || v.doc.md == nil || strings.ContainsAny(text, "\r\n") {
		v.clearLivePrefix()
		return text
	}
	target := v.activePrefixTarget()
	src, caret, line := target.src, target.caret, target.line
	if target.hasSelection || caret < line.start || caret > line.end {
		v.clearLivePrefix()
		return text
	}
	continuing := continuingLivePrefix(v.pfx.live, line.start)
	if !continuing && !target.inTransient && v.reanchorLivePrefix(line) {
		continuing = true
	}
	prefixStart := 0
	if continuing {
		prefixStart = v.pfx.live.prefixStart
	} else {
		var ok bool
		if prefixStart, ok = v.livePrefixStartsHere(line, caret); !ok {
			if target.inTransient {
				if text == ">" && caret == line.end && quoteOnlyLine(src[line.start:line.end]) {
					// The typed marker grows a line that already holds quote
					// markers only: Typora's live view holds it as an
					// unfinished marker in a paragraph of its own instead of
					// a deeper quote (record 2026-11-11,
					// probe83/bare-gt-lines).
					v.pfx.quoteLive = quoteMarkerLive{ok: true, lineStart: line.start, marker: text}
				}
			} else if v.setextContentLine(line) {
				// The line is the content of a setext heading, which opens no
				// block of its own: Typora reads whatever is typed there as
				// the heading's text. `alpha\n=====` with `alpha` deleted and
				// `- ljkv` typed stays heading level 1 showing `- ljkv`, with
				// no list and no prefix (record 2026-11-13, seed22/c10), so
				// the marker stays text and the underline keeps it a heading.
				v.holdLiteralLine(line.start, line.start)
			}
			v.clearLivePrefix()
			return text
		}
	}
	next := src[prefixStart:caret] + text + src[caret:line.end]
	typedEnd := caret - prefixStart + len(text)
	next, typedEnd, text, spaceAt := quoteMarkerInput(next, caret-prefixStart, typedEnd, text)
	quoteSpace := false
	if !target.inTransient {
		typedEnd, quoteSpace = quoteMarkerSpaceTypedEnd(next, typedEnd, spaceAt)
	}
	p := classifyLivePrefix(next, line.start, prefixStart, line.end+len(text), typedEnd)
	if p != nil && quoteMarkerAfterPrefix(src, line.start, prefixStart, next[:typedEnd]) {
		p = nil
	}
	if p != nil && !target.inTransient && p.kind == livePrefixHeading && hardBreakContinued(src, line.start) {
		// The line continues the paragraph above it across a hard break: its
		// marker is the paragraph's text, not a block opening. Typora keeps the
		// paragraph and shows the marker on the second line (record 2026-11-12,
		// seed19/c8).
		p = &livePrefix{kind: livePrefixLiteral, prefix: next[:typedEnd]}
	}
	if p != nil && !target.inTransient && !typed && p.kind == livePrefixHeading && v.literalATXPrefix() {
		// The ATX shortcut fires on typed input only. A paste that lands on a
		// line the session holds literal leaves the markers as text: `# `
		// typed, then `e` pasted, keeps the paragraph showing `# e` instead of
		// opening a heading level 1 (record 2026-11-12, seed16/c13).
		p = &livePrefix{kind: livePrefixLiteral, prefix: v.pfx.live.prefix}
	}
	if p != nil {
		p.text = src[line.start:caret] + text + src[caret:line.end]
		if target.inTransient {
			v.pfx.live = p
			return text
		}
		v.setLivePrefix(p)
		return text
	}
	if target.inTransient {
		v.pfx.live = nil
		return text
	}
	if spaceAt >= 0 {
		// The space the marker needed is in the bytes: the line is a
		// quote now, not the literal marker the session held. Write the
		// space as its own edit, so Undo takes back only the space.
		v.pfx.quoteSpace = quoteSpaceEdit{ok: true, at: spaceAt}
		v.clearLiteralLineAt(line.start)
	} else if quoteSpace {
		// The marker opens in front of spaces the line already holds and
		// writes none of its own. The bytes spell the quote only once the
		// line is marked literal no more, which Typora does as an edit after
		// the marker's own: Undo takes that conversion back and leaves the
		// marker as text (record 2026-11-12, probe94/leading-space-gt-undo).
		v.pfx.quoteMarker = quoteMarkerEdit{ok: true, lineStart: line.start, markerEnd: caret + len(text)}
		v.holdLiteralLine(line.start, prefixStart)
		v.pfx.live = nil
		return text
	}
	// Typed text that is no live prefix leaves the line's literal lines
	// alone: what the line already spells as text stays text. Completing a
	// trigger is what makes the markers parse as a block.
	v.pfx.live = nil
	if completeImmediatePrefix(next[:typedEnd]) {
		if prefixStart == line.start && v.emptyListItemPrefixOnContinuation(next[:typedEnd], line, caret) {
			// An empty list item cannot interrupt the paragraph above:
			// Markdown reads its line as a setext underline. Typora ends
			// the paragraph and keeps the line as the blank separator
			// before the item (record 2026-11-09, seed3/c16): keep the
			// line literal until splitContinuationPrefix writes it.
			return text
		}
		v.clearLiteralLineAt(line.start)
		if v.pfx.wroteLine == line.start && prefixStart == line.start {
			// The run wrote this line's bytes and they now read as a block; the
			// hold began inside the run, not before it, so the step keeps the
			// promotion as its own Undo and the bytes stay as text (record
			// 2026-11-12, seed14/c8).
			d := v.doc
			d.lock()
			d.pendingPromotion = &literalPromotion{
				line: line.start, prefixStart: prefixStart, marker: next[:typedEnd],
			}
			d.mu.Unlock()
			v.pfx.wroteLine = -1
		}
	}
	return text
}

// quoteMarkerInput inserts the space Typora writes between a quote marker
// and the content directly after it: `>alpha` becomes `> alpha` when the
// marker opens in front of existing text, and content typed right after a
// lone `>` splits the bytes the same way (record 2026-11-11,
// p0/live-syntax/line-start/quote cp1). next is the prospective line from
// the prefix start, caret the typed text's start in it, and typedEnd the
// typed bytes through the text. The space is part of what the user typed:
// Typora's live bytes carry it, and later input sees it as content.
func quoteMarkerInput(next string, caret, typedEnd int, text string) (string, int, string, int) {
	at, ok := quoteMarkerSpace(next)
	if !ok || at < caret || at > caret+len(text) {
		return next, typedEnd, text, -1
	}
	p := at - caret
	text = text[:p] + " " + text[p:]
	next = next[:at] + " " + next[at:]
	if at <= typedEnd {
		typedEnd++
	}
	return next, typedEnd, text, p
}

// quoteMarkerAfterPrefix reports whether the bytes typed at the line's
// content start are a lone `>` behind an existing quote marker. That
// marker opens a deeper quote, so the line is a quote block even though a
// bare `>` with no quote before it stays the literal marker Typora's live
// view holds (record 2026-11-11, probe83/bare-gt-lines).
func quoteMarkerAfterPrefix(src string, lineStart, prefixStart int, typed string) bool {
	if typed != ">" || prefixStart <= lineStart || prefixStart > len(src) {
		return false
	}
	return strings.Contains(src[lineStart:prefixStart], ">")
}

// quoteMarkerSpace reports the offset of the non-whitespace content that
// directly follows a line's last quote marker. Nested markers each need
// their own space (`> >alpha` splits after the second `>`); a marker
// already followed by whitespace needs none.
func quoteMarkerSpace(line string) (int, bool) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	for i < len(line) && line[i] == '>' {
		i++
		if i < len(line) && line[i] != ' ' && line[i] != '\t' {
			return i, true
		}
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
	}
	return 0, false
}

// setextContentLine reports whether the line is the content line of a setext
// heading whose underline follows it. Typora keeps such a line the heading's
// text whatever block markers it holds: `alpha\n=====` with `alpha` deleted
// and `- ljkv` typed shows a level-1 heading whose text is `- ljkv` (record
// 2026-11-13, seed22/c10).
func (v *view) setextContentLine(line markdownLine) bool {
	if v.doc == nil || v.doc.md == nil {
		return false
	}
	v.doc.lock()
	src := v.doc.md.p.String()
	setext := false
	p := v.doc.md.p
	for j := 1; j < p.Block(0).Next; j++ {
		b := p.Block(j)
		if b.Kind == parser.Heading && b.Setext && b.Start == line.start {
			setext = true
			break
		}
	}
	v.doc.mu.Unlock()
	if !setext {
		return false
	}
	next := lineAt(src, line.stop)
	if next.start != line.stop || next.start >= len(src) {
		return false
	}
	return setextUnderlineLine(src[next.start:next.end])
}

// setextUnderlineLine reports a line that can be the underline of a setext
// heading: a run of `=` or of `-`, indented no more than three spaces.
func setextUnderlineLine(line string) bool {
	mark := strings.TrimSpace(line)
	if mark == "" {
		return false
	}
	if len(line)-len(strings.TrimLeft(line, " \t")) > 3 {
		return false
	}
	return strings.Trim(mark, "=") == "" || strings.Trim(mark, "-") == ""
}

// emptyListItemPrefixOnContinuation reports whether typed, the completed
// block prefix at the end of line, asks for an empty list item on a line
// that continues the paragraph above. Such an item cannot interrupt the
// paragraph: a "-" line is its setext underline. Only a plain paragraph
// has the problem; a quoted paragraph's line is split with its own quote
// markers, and a heading or a container line does not continue a
// paragraph.
func (v *view) emptyListItemPrefixOnContinuation(typed string, line markdownLine, caret int) bool {
	if caret != line.end || !emptyListItemPrefix(typed) || v.doc == nil || v.doc.md == nil {
		return false
	}
	p := v.source.from(line.start)
	if p < 0 || p >= v.source.Len() {
		p = v.shown.paraAt(line.start)
	}
	if p < 0 || p >= v.shown.ParagraphCount() {
		return false
	}
	st := v.para(p).style
	return st.Heading == 0 && st.List == NoList && !st.Quote && !st.CodeBlock &&
		v.source.start(p) < line.start
}

// emptyListItemPrefix reports a block prefix that opens a list item with
// no content of its own.
func emptyListItemPrefix(s string) bool {
	switch s {
	case "- ", "* ", "+ ":
		return true
	}
	if i := strings.IndexAny(s, ".)"); i > 0 && i+1 == len(s)-1 && s[i+1] == ' ' {
		return orderedDigits(s[:i])
	}
	return false
}

// itemContentStart returns where a list item's content starts on its line,
// when the line opens one with a marker.
func itemContentStart(line string) (int, bool) {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return 2, true
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
		return i + 2, true
	}
	return 0, false
}

func (v *view) livePrefixStart(line markdownLine, caret int) (int, bool) {
	if caret < line.start || caret > line.end {
		return 0, false
	}
	src := v.sourceText()
	if caret == line.start && strings.TrimSpace(src[line.start:line.end]) == "" {
		return line.start, true
	}
	p := v.source.from(caret)
	if p < 0 || p >= v.source.Len() {
		p = v.shown.paraAt(caret)
		if p < 0 || p >= v.source.Len() {
			return 0, false
		}
	}
	start := v.source.start(p)
	if start < line.start && v.typedEmptyFirstLine(line, start) {
		// The paragraph's first line is empty and the session typed the
		// break above the caret's line: the prefix starts on the caret's
		// line, which is the paragraph's first line of text (issue 95).
		start = line.start
	}
	if start < line.start || start > line.end {
		return 0, false
	}
	return start, true
}

// typedEmptyFirstLine reports whether the paragraph starts on the single
// break above the caret's line, a break the session typed (Shift+Enter at
// the paragraph's start). Such a paragraph has no text or container marker
// above the caret, so a block prefix typed there opens the line the way it
// does with no empty line above it. A paragraph reopened from a file holds
// no session break and keeps its first line out of the prefix (record
// 2026-11-12, probe91/group3-cold-open-leading-softbreak).
func (v *view) typedEmptyFirstLine(line markdownLine, start int) bool {
	src := v.sourceText()
	at := line.start - 1
	if src[at] == '\n' && at > 0 && src[at-1] == '\r' {
		at--
	}
	if at != start {
		return false
	}
	live := v.doc.md.p
	if v.transient != nil {
		live = v.transient.preview.md.p
	}
	return live.IsLiveLine(at)
}

// splitQuoteItemPrefix writes the blank line Typora keeps before a list
// item whose content opens a quote. Enter opens the item's line directly
// under the previous one; the quote inside the item is a block of its own,
// and Typora separates it from the line above (record 2026-11-10,
// seed3/c11).
func (v *view) splitQuoteItemPrefix() {
	if v.doc == nil || v.doc.md == nil || v.opts.NoMarkdown || v.opts.ReadOnly || v.transient != nil {
		return
	}
	v.doc.lock()
	src := v.doc.md.p.String()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	caret := sel.Caret.Offset
	line := liveLineAt(src, caret)
	if caret != line.end || line.start == 0 {
		return
	}
	body := strings.TrimLeft(src[line.start:line.end], " \t")
	at, ok := itemContentStart(body)
	if !ok && body == "> " && len(body) < line.end-line.start {
		prevAt := line.start - 1
		if prevAt > 0 && src[prevAt-1] == '\r' {
			prevAt--
		}
		prev := liveLineAt(src, prevAt)
		prevBody := strings.TrimLeft(src[prev.start:prev.end], " \t")
		if markerEnd, itemOK := itemContentStart(prevBody); itemOK &&
			!strings.HasPrefix(strings.TrimLeft(prevBody[markerEnd:], " \t"), ">") {
			ok = true
		}
	}
	if !ok || at >= len(body) || body[at] != '>' {
		return
	}
	if line.eol == "" && line.stop >= len(src) {
		// The document does not end with a line break: the item is its
		// final line, and Typora keeps the quote marker on it instead of
		// separating it (record 2026-11-09, seed4/c20).
		return
	}
	p := v.source.from(caret)
	if p < 0 || p >= v.source.Len() {
		p = v.shown.paraAt(caret)
	}
	if p < 0 || p >= v.shown.ParagraphCount() {
		return
	}
	if st := v.para(p).style; st.List == NoList || !st.Quote || st.CodeBlock {
		return
	}
	if rest := body[at+1:]; rest != "" && rest != " " && v.para(p).n != 0 {
		// Bytes after the quote marker belong to the quote's paragraph, as in
		// `- >text`: the quote already holds them. A split sibling's moved
		// marker is not part of the paragraph the view shows, so the quote
		// opens with no content and still separates from the item above
		// (record 2026-11-11, probe70/type-gt).
		return
	}
	if blankLineBefore(src, line.start, "") {
		return
	}
	eol := defaultEOL(src)
	v.doc.lock()
	v.lockedEdit(func(e *Edit) {
		if !e.replaceMarkdownSource(line.start, line.start, eol) {
			e.fail(ErrStructuralEdit)
		}
	})
	v.doc.mu.Unlock()
	v.setSourceSelection(sourceCaret(caret + len(eol)))
}

// splitQuoteAfterSoftBreak 在普通段落的软换行所开出的那一行上打出 `> `
// 时，写出段落与引用之间的空行。引用打断了段落，Typora 用 W1 的一个空行
// 分隔块：`alpha beta`、Shift+Enter、`> ` 得到 `alpha beta\n\n> `
// （录制 2026-11-10, seed9/c6）。补全的前缀本身已解析为引用，这一行没有
// live prefix 标记。
func (v *view) splitQuoteAfterSoftBreak(src string, line markdownLine, caret int) bool {
	body := src[line.start:line.end]
	if caret != line.end || line.start == 0 || strings.TrimSpace(body) != ">" || !strings.HasSuffix(body, " ") {
		return false
	}
	prevAt := line.start - 1
	if prevAt > 0 && src[prevAt-1] == '\r' {
		prevAt--
	}
	prev := liveLineAt(src, prevAt)
	if strings.TrimSpace(src[prev.start:prev.end]) == "" {
		return false
	}
	p := v.source.from(prev.start)
	if p < 0 || p >= v.source.Len() {
		p = v.shown.paraAt(prev.start)
	}
	if p < 0 || p >= v.shown.ParagraphCount() {
		return false
	}
	// 上一行必须属于普通段落。引用或列表里的段落带着容器的 marker，标题与
	// 围栏根本不是段落：只有软换行会把普通段落的行留在新行正上方。
	if st := v.para(p).style; st.Heading != 0 || st.List != NoList || st.Quote || st.CodeBlock {
		return false
	}
	eol := defaultEOL(src)
	v.doc.lock()
	v.lockedEdit(func(e *Edit) {
		if !e.replaceMarkdownSource(line.start, line.start, eol) {
			e.fail(ErrStructuralEdit)
		}
	})
	v.doc.mu.Unlock()
	v.setSourceSelection(sourceCaret(caret + len(eol)))
	return true
}

// splitQuoteContinuationPrefix gives a block prefix typed on a quoted
// paragraph's continuation line a block of its own, as Typora does: a bare
// quote marker line separates it from the paragraph above (typing "- " after
// Shift+Enter in "> alpha" writes "> alpha\n>\n> - ").
func (v *view) splitQuoteContinuationPrefix() {
	if v.doc == nil || v.doc.md == nil || v.opts.NoMarkdown || v.opts.ReadOnly || v.transient != nil {
		return
	}
	v.doc.lock()
	src := v.doc.md.p.String()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	caret := sel.Caret.Offset
	line := liveLineAt(src, caret)
	if caret != line.end {
		return
	}
	if v.splitQuoteAfterSoftBreak(src, line, caret) {
		return
	}
	// The line's content starts after the quote markers it repeats.
	content := line.start
	for content < line.end && (src[content] == ' ' || src[content] == '\t') {
		content++
	}
	for content < line.end && src[content] == '>' {
		content++
		if content < line.end && src[content] == ' ' {
			content++
		}
		for content < line.end && (src[content] == ' ' || src[content] == '\t') {
			content++
		}
	}
	if content >= line.end || !completeImmediatePrefix(src[content:line.end]) {
		return
	}
	prefix := src[line.start:content]
	if !strings.Contains(prefix, ">") {
		if prefix != "" || !emptyListItemPrefix(src[content:line.end]) {
			return
		}
		// A plain paragraph's continuation line: the empty item cannot
		// interrupt the paragraph above, where Markdown reads "- " as a
		// setext underline. Typora ends the paragraph: the line stays as
		// the blank separator and the item opens on the next one (record
		// 2026-11-09, seed3/c16).
		p := v.source.from(caret)
		if p < 0 || p >= v.source.Len() {
			p = v.shown.paraAt(caret)
		}
		if p < 0 || p >= v.shown.ParagraphCount() {
			return
		}
		st := v.para(p).style
		if st.Heading != 0 || st.List != NoList || st.CodeBlock || st.Quote ||
			v.source.start(p) >= line.start {
			return
		}
		eol := defaultEOL(src)
		v.doc.lock()
		v.lockedEdit(func(e *Edit) {
			// The line is a real item now; it parses as one.
			e.d.setState(e.d.state().clearLiteralLineAt(line.start))
			if !e.replaceMarkdownSource(line.start, line.start, eol) {
				e.fail(ErrStructuralEdit)
			}
		})
		v.doc.mu.Unlock()
		v.setSourceSelection(sourceCaret(caret + len(eol)))
		return
	}
	for i := 0; i < len(prefix); i++ {
		if prefix[i] != '>' && prefix[i] != ' ' && prefix[i] != '\t' {
			return
		}
	}
	p := v.source.from(caret)
	if p < 0 || p >= v.source.Len() {
		p = v.shown.paraAt(caret)
	}
	if p < 0 || p >= v.shown.ParagraphCount() {
		return
	}
	st := v.para(p).style
	if !st.Quote || st.CodeBlock || v.source.start(p) >= line.start {
		return
	}
	prevAt := line.start - 1
	for prevAt > 0 && (src[prevAt] == '\n' || src[prevAt] == '\r') {
		prevAt--
	}
	prev := liveLineAt(src, prevAt)
	if !strings.HasPrefix(src[prev.start:prev.end], prefix) {
		// The line above carries the same quote markers: the prefix
		// continues a quoted block whose first line is earlier.
		return
	}
	replacement := strings.TrimRight(prefix, " \t") + defaultEOL(src) + prefix
	v.doc.lock()
	v.lockedEdit(func(e *Edit) {
		if !e.replaceMarkdownSource(line.start, content, replacement) {
			e.fail(ErrStructuralEdit)
		}
	})
	v.doc.mu.Unlock()
	v.setSourceSelection(sourceCaret(caret + len(replacement) - len(prefix)))
}

func (v *view) livePrefixStartsHere(line markdownLine, caret int) (int, bool) {
	start, ok := v.livePrefixStart(line, caret)
	if !ok || caret != start {
		return 0, false
	}
	p := v.source.from(caret)
	shown := p
	if p < 0 || p >= v.source.Len() {
		shown = v.shown.paraAt(caret)
	}
	if shown < 0 || shown >= v.shown.ParagraphCount() {
		return 0, false
	}
	st := v.para(shown).style
	if st.Heading != 0 || st.CodeBlock {
		return 0, false
	}
	return start, true
}

func continuingLivePrefix(p *livePrefix, lineStart int) bool {
	return p != nil && p.lineStart == lineStart
}

// hardBreakContinued reports whether the line at lineStart continues the
// paragraph above it across a hard break, spelled by a trailing backslash or
// two trailing spaces. Such a line's markers are the paragraph's text: Typora
// neither opens a block there nor drops the blank line the break came from
// (record 2026-11-12, seed19/c8).
func hardBreakContinued(src string, lineStart int) bool {
	if lineStart <= 1 {
		return false
	}
	end := lineStart - 1
	if src[end-1] == '\r' {
		end--
	}
	start := 0
	for i := end - 1; i >= 0; i-- {
		if src[i] == '\n' || src[i] == '\r' {
			start = i + 1
			break
		}
	}
	line := src[start:end]
	if strings.TrimSpace(line) == "" {
		return false
	}
	return strings.HasSuffix(line, "\\") || strings.HasSuffix(line, "  ")
}

// reanchorLivePrefix moves the live prefix onto the line its bytes now stand
// on, and moves the line's literal hold with it. Materializing a held blank
// line writes the backslash the preview lacks, which pushes the typed line one
// byte later: the prefix keeps the preview's coordinates, and the hold, made
// from them, lands on the blank line instead of the line the session types on.
// A marker typed on a hard-continued line would then open a block rather than
// stay the paragraph's second line (record 2026-11-12, seed19/c8).
func (v *view) reanchorLivePrefix(line markdownLine) bool {
	p := v.pfx.live
	if p == nil || p.prefix == "" || line.start <= p.lineStart {
		return false
	}
	src := v.doc.md.p.String()
	if line.start+len(p.prefix) > len(src) || src[line.start:line.start+len(p.prefix)] != p.prefix {
		return false
	}
	delta := line.start - p.lineStart
	p.lineStart = line.start
	p.lineEnd += delta
	p.prefixStart += delta
	v.setLivePrefix(p)
	return true
}

func classifyLivePrefix(line string, lineStart, prefixStart, lineEnd, typedEnd int) *livePrefix {
	typedEnd = max(0, min(typedEnd, len(line)))
	typed := line[:typedEnd]
	if fence, ok := fencePrefix(typed); ok {
		return placeLivePrefix(&livePrefix{
			kind:   livePrefixFence,
			prefix: fence.prefix,
			exact:  true,
		}, lineStart, prefixStart, lineEnd)
	}
	if partialFencePrefix(typed) {
		return placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	if p := headingPrefix(line, prefixStart, typedEnd); p != nil {
		if p.kind == livePrefixLiteral && len(typed) < len(line) {
			p.prefix = ""
		}
		return placeLivePrefix(p, lineStart, prefixStart, lineEnd)
	}
	if partialHeadingPrefix(typed) {
		// The opening run is incomplete while trailing text follows it: the
		// space typed next can still open the heading, so keep the live state.
		return placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	if completeImmediatePrefix(typed) {
		return nil
	}
	if partialImmediatePrefix(typed) {
		return placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	if strings.HasPrefix(typed, "|") {
		prefix := ""
		if line == "|a|b|" {
			prefix = "|a|b|"
		}
		return placeLivePrefix(&livePrefix{
			kind:          livePrefixAtomic,
			prefix:        prefix,
			displayPrefix: "|",
			exact:         line == "|a|b|",
		}, lineStart, prefixStart, lineEnd)
	}
	for _, candidate := range []string{"$$", "---", "***", "[toc]"} {
		if strings.HasPrefix(typed, candidate) {
			prefix := ""
			if line == candidate {
				prefix = candidate
			}
			return placeLivePrefix(&livePrefix{
				kind:          livePrefixAtomic,
				prefix:        prefix,
				displayPrefix: candidate,
				exact:         line == candidate,
			}, lineStart, prefixStart, lineEnd)
		}
	}
	if partialAtomicPrefix(typed) {
		return placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	return nil
}

func placeLivePrefix(p *livePrefix, lineStart, prefixStart, lineEnd int) *livePrefix {
	p.lineStart = lineStart
	p.lineEnd = lineEnd
	p.prefixStart = prefixStart
	return p
}

func completeImmediatePrefix(s string) bool {
	switch s {
	case "- ", "* ", "+ ", "> ":
		return true
	}
	if i := strings.IndexAny(s, ".)"); i > 0 && i+1 == len(s)-1 && s[i+1] == ' ' {
		return orderedDigits(s[:i])
	}
	for _, prefix := range []string{"- [ ] ", "- [x] ", "- [X] "} {
		if s == prefix {
			return true
		}
	}
	return false
}

// partialHeadingPrefix reports an ATX opening run at the line's start whose
// trailing text keeps it from opening a heading until the next space.
func partialHeadingPrefix(s string) bool {
	return len(s) >= 1 && len(s) <= 6 && strings.Trim(s, "#") == ""
}

// partialImmediatePrefix reports bytes that open no block until the trigger
// that follows them: a list marker needs the space separating it from its
// content, and an ordered marker needs the space after its dot or paren. A
// lone `>` is the quote's partial prefix too: Typora's live view keeps it
// literal until a space or content follows (record 2026-11-11,
// p0/live-syntax/empty/quote cp1). A cold parse of `>` reads an empty
// quote; the live projection is the session's parse, which holds the
// marker literal until it completes (spec §12.3 gate 1, issue 83).
func partialImmediatePrefix(s string) bool {
	switch s {
	case "-", "*", "+", ">":
		return true
	}
	if orderedDigits(s) || orderedMarkerOnly(s) {
		return true
	}
	for _, prefix := range []string{"- [ ] ", "- [x] ", "- [X] "} {
		if len(s) < len(prefix) && strings.HasPrefix(prefix, s) {
			return true
		}
	}
	return false
}

func headingPrefix(line string, lineStart, typedEnd int) *livePrefix {
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	if n == 0 {
		return nil
	}
	lineEnd := lineStart + len(line)
	if n == len(line) {
		return &livePrefix{
			kind:      livePrefixLiteral,
			lineStart: lineStart,
			lineEnd:   lineEnd,
			prefix:    line,
		}
	}
	if n == 6 && line[n] == '#' {
		return &livePrefix{kind: livePrefixLiteral, lineStart: lineStart, lineEnd: lineEnd}
	}
	if line[n] != ' ' {
		// Strict mode keeps `#abc` a paragraph on every keystroke and on
		// Enter; only `#` followed by a space opens a heading.
		return nil
	}
	prefix := line[:n+1]
	if len(line) <= n+1 {
		return &livePrefix{
			kind:      livePrefixLiteral,
			lineStart: lineStart,
			lineEnd:   lineEnd,
			prefix:    prefix,
		}
	}
	return &livePrefix{
		kind:      livePrefixHeading,
		lineStart: lineStart,
		lineEnd:   lineEnd,
		prefix:    prefix,
	}
}

// resumePromotedPrefix restores the live state of a prefix Undo took back on
// the line the typing run wrote. The bytes stay and the view reads their
// marker as the visible prefix of a paragraph again, the shape Typora leaves
// after that Undo (record 2026-11-12, seed14/c8).
func (v *view) resumePromotedPrefix(src string, line markdownLine, undone *undoStep) bool {
	if undone == nil || undone.promotion == nil || undone.promotion.line != line.start {
		return false
	}
	marker := undone.promotion.marker
	prefixStart := undone.promotion.prefixStart
	if marker == "" || prefixStart < line.start || prefixStart+len(marker) > len(src) ||
		prefixStart+len(marker) > line.end || src[prefixStart:prefixStart+len(marker)] != marker {
		return false
	}
	p := &livePrefix{
		kind:        livePrefixLiteral,
		lineStart:   line.start,
		lineEnd:     line.end,
		prefixStart: prefixStart,
		prefix:      marker,
		text:        src[line.start:line.end],
	}
	v.setLivePrefix(p)
	v.reprojectLiteralPrefix(p)
	return true
}

// resumeUndoneQuoteSpace holds literal the line Undo left with a quote
// marker directly before its content. Typora writes the marker and the
// space after it as separate edits: Undo takes back the space and the live
// view reads the uncompleted marker as text, not as a cold-parsed quote
// (record 2026-11-11, probe83/undo-after-gt).
//
// The step says what the bytes cannot: a line whose bytes read the same way
// can also come from deleting the rune before its marker, and that Undo
// restores the quote the line was (issue 99).
func (v *view) resumeUndoneQuoteSpace(src string, line markdownLine, caret int, undone *undoStep) bool {
	if caret <= line.start || caret >= line.end || src[caret-1] != '>' {
		return false
	}
	if src[caret] == ' ' || src[caret] == '\t' {
		return false
	}
	if strings.Trim(src[line.start:caret], "> \t") != "" {
		return false
	}
	if undone != nil && !undoneQuoteSpace(undone, caret) {
		// The step did not write the space back, so the bytes describe an
		// uncompleted marker for another reason, as a rune deleted from
		// before the marker (issue 99). A hold the session already had still
		// describes these bytes and stays; a step that never wrote a space
		// does not start one.
		return v.pfx.undoneQuoteSpaceLine == line.start
	}
	p := &livePrefix{
		kind:        livePrefixLiteral,
		lineStart:   line.start,
		lineEnd:     line.end,
		prefixStart: line.start,
		text:        src[line.start:line.end],
	}
	v.setLivePrefix(p)
	v.reprojectLiteralPrefix(p)
	v.pfx.undoneQuoteSpaceLine = line.start
	return true
}

// undoneQuoteSpace reports whether the step Undo took back wrote the space
// Typora puts after a quote marker: one patch, inserting that space alone at
// the caret the undo left. insertQuoteSpace makes exactly that edit.
func undoneQuoteSpace(undone *undoStep, caret int) bool {
	if undone == nil {
		return false
	}
	if len(undone.patches) != 1 {
		return false
	}
	c := undone.patches[0]
	return c.old == "" && c.new == " " && c.at == caret
}

// undoneLiteralLine reports whether the step Undo took back found the line
// at lineStart literal, as the prefix the session typed held it. A step that
// removed a rune from an ordinary paragraph found no literal line there, so
// the undo restores the paragraph and not a prefix (issue 99). A step that
// only converted the line, as the quote marker typed in front of a line's
// own spaces does, holds it in both of its literal sets.
func undoneLiteralLine(undone *undoStep, lineStart int) bool {
	if undone == nil {
		return true
	}
	for _, set := range [][]parser.LiteralLine{undone.sessionBefore.literals, undone.sessionAfter.literals} {
		for _, l := range set {
			if l.Line == lineStart {
				return true
			}
		}
	}
	return false
}

// clearUndoneQuoteSpace drops the literal hold when Undo or Redo restores
// bytes the hold no longer describes: the line parses again as its bytes
// spell.
func (v *view) clearUndoneQuoteSpace() {
	lineStart := v.pfx.undoneQuoteSpaceLine
	v.pfx.undoneQuoteSpaceLine = -1
	if v.doc == nil || v.doc.md == nil || lineStart < 0 {
		return
	}
	v.doc.lock()
	held := false
	for _, l := range v.doc.state().literals {
		if l.Line == lineStart {
			held = true
			break
		}
	}
	v.doc.mu.Unlock()
	if !held {
		return
	}
	v.clearLiteralLineAt(lineStart)
	v.reprojectLiteralPrefix(nil)
}

// resumeLivePrefix restores the visible state of the prefix that Undo or
// Redo brought back under the caret. A heading with content is deliberately
// not resumed: a restored heading hides its marker.
func (v *view) resumeLivePrefix(undone *undoStep) {
	if v.doc == nil || v.doc.md == nil {
		v.clearLivePrefix()
		return
	}
	v.doc.lock()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		v.clearLivePrefix()
		return
	}
	src := v.sourceText()
	line := liveLineAt(src, sel.Caret.Offset)
	if sel.Caret.Offset < line.start || sel.Caret.Offset > line.end {
		v.clearLivePrefix()
		return
	}
	if v.resumeUndoneQuoteSpace(src, line, sel.Caret.Offset, undone) {
		return
	}
	if v.resumePromotedPrefix(src, line, undone) {
		return
	}
	if v.pfx.undoneQuoteSpaceLine >= 0 {
		v.clearUndoneQuoteSpace()
	}
	prefixStart, ok := v.livePrefixStart(line, sel.Caret.Offset)
	if !ok || sel.Caret.Offset != line.end {
		// A resting prefix is the line's whole text after its paragraph
		// start: the caret sits at the end of what was typed, and bytes
		// past the caret are the line's content, as an empty item's
		// emphasis close. Reading them as markers would resume a prefix
		// the undo did not bring back.
		v.clearLivePrefix()
		return
	}
	p := restingLivePrefix(src[prefixStart:line.end], line.start, prefixStart, line.end)
	if p == nil {
		// Restoring no prefix changes nothing about how the line parses:
		// its literal line stands, as it does while the caret moves away.
		v.clearLivePrefix()
		return
	}
	if !undoneLiteralLine(undone, line.start) {
		// The step Undo took back restored no literal line here: the input
		// it removed never held a prefix on this line. Resuming one would
		// add a literal line the session did not have, and the line would
		// parse apart from the state before the undone step (issue 99).
		v.clearLivePrefix()
		return
	}
	p.text = src[line.start:line.end]
	v.setLivePrefix(p)
	if lineStart, _ := livePrefixLiteralSite(p); lineStart >= 0 {
		v.reprojectLiteralPrefix(p)
	}
}

// resumeEmptyHeadingPrefix keeps a persisted empty ATX prefix visible as
// the literal paragraph Typora shows after opening the file.
func (v *view) resumeEmptyHeadingPrefix() {
	if v.doc == nil || v.doc.md == nil {
		return
	}
	v.doc.lock()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	src := v.sourceText()
	line := liveLineAt(src, sel.Caret.Offset)
	if sel.Caret.Offset < line.start || sel.Caret.Offset > line.end ||
		!emptyHeadingPrefix(src[line.start:line.end]) {
		return
	}
	v.setLivePrefix(&livePrefix{
		kind:        livePrefixLiteral,
		lineStart:   line.start,
		lineEnd:     line.end,
		prefixStart: line.start,
		prefix:      src[line.start:line.end],
		text:        src[line.start:line.end],
	})
}

// reprojectLiteralPrefix refreshes the projection after Undo or Redo
// restores bytes whose block syntax is held literal by a live prefix.
func (v *view) reprojectLiteralPrefix(p *livePrefix) {
	v.doc.lock()
	defer v.doc.mu.Unlock()
	d := v.doc
	if d.md == nil {
		return
	}
	src := d.md.p.String()
	np := parser.New(d.md.p.Extensions())
	d.state().syncToParser(np)
	np.Set(src)
	t, m := projectSource(np)
	d.md.p, d.md.m = np, m
	x, removed, inserted := diffText(d.text, t)
	d.apply(nil, x, removed, inserted, t)
	v.lockedSync()
}

func restingLivePrefix(line string, lineStart, prefixStart, lineEnd int) *livePrefix {
	var prefix *livePrefix
	if fence, ok := fencePrefix(line); ok {
		indent := strings.Index(line, fence.prefix)
		if indent >= 0 && line[indent:] == fence.prefix {
			prefix = placeLivePrefix(&livePrefix{
				kind:   livePrefixFence,
				prefix: fence.prefix,
				exact:  true,
			}, lineStart, prefixStart, lineEnd)
		}
	}
	if prefix != nil {
		return prefix
	}
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	if n > 0 && n == len(line) {
		prefix = placeLivePrefix(&livePrefix{
			kind:   livePrefixLiteral,
			prefix: line,
		}, lineStart, prefixStart, lineEnd)
	}
	if prefix != nil {
		return prefix
	}
	switch line {
	case "-", "*", "+", ">":
		// Restored bytes that are a lone quote marker show the literal
		// marker Typora's live view holds (record 2026-11-11,
		// p0/live-syntax/empty/quote cp1).
		prefix = placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	if prefix == nil && orderedMarkerOnly(line) {
		prefix = placeLivePrefix(&livePrefix{kind: livePrefixLiteral}, lineStart, prefixStart, lineEnd)
	}
	if prefix != nil {
		return prefix
	}
	if p := headingPrefix(line, prefixStart, len(line)); p != nil && p.kind == livePrefixLiteral {
		return placeLivePrefix(p, lineStart, prefixStart, lineEnd)
	}
	for _, prefix := range []string{"$$", "---", "***", "[toc]", "|a|b|"} {
		if line == prefix {
			return placeLivePrefix(&livePrefix{
				kind:          livePrefixAtomic,
				prefix:        prefix,
				displayPrefix: prefix,
				exact:         true,
			}, lineStart, prefixStart, lineEnd)
		}
	}
	return nil
}

type fencePrefixInfo struct {
	prefix string
	char   byte
	length int
}

func fencePrefix(line string) (fencePrefixInfo, bool) {
	indent := 0
	for indent < len(line) && indent < 4 && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent >= len(line) {
		return fencePrefixInfo{}, false
	}
	c := line[indent]
	if c != '`' && c != '~' {
		return fencePrefixInfo{}, false
	}
	n := 0
	for indent+n < len(line) && line[indent+n] == c {
		n++
	}
	if n < 3 {
		return fencePrefixInfo{}, false
	}
	return fencePrefixInfo{prefix: line[indent : indent+n], char: c, length: n}, true
}

func partialFencePrefix(line string) bool {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || line[i] != '`' && line[i] != '~' {
		return false
	}
	c := line[i]
	n := 0
	for i+n < len(line) && line[i+n] == c {
		n++
	}
	return n < 3 && i+n == len(line)
}

func orderedMarkerOnly(line string) bool {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i+1 == len(line) && (line[i] == '.' || line[i] == ')')
}

func orderedDigits(line string) bool {
	if line == "" {
		return false
	}
	for i := range len(line) {
		if line[i] < '0' || line[i] > '9' {
			return false
		}
	}
	return true
}

func partialAtomicPrefix(line string) bool {
	for _, prefix := range []string{"$$", "---", "***", "[toc]", "|a|b|"} {
		if len(line) < len(prefix) && strings.HasPrefix(prefix, line) {
			return true
		}
	}
	return false
}

func liveLineAt(src string, at int) markdownLine {
	at = max(0, min(at, len(src)))
	start := at
	for start > 0 && src[start-1] != '\n' && src[start-1] != '\r' {
		start--
	}
	end := at
	for end < len(src) && src[end] != '\n' && src[end] != '\r' {
		end++
	}
	stop := end
	if stop < len(src) {
		if src[stop] == '\r' {
			stop++
			if stop < len(src) && src[stop] == '\n' {
				stop++
			}
		} else {
			stop++
		}
	}
	return markdownLine{start: start, end: end, stop: stop, eol: src[end:stop]}
}

func (v *view) setLivePrefix(p *livePrefix) {
	v.pfx.live = p
	v.applyLiteralSite(p)
}

// literalATXPrefix reports the unfinished ATX marker the session typed and
// still holds as text: the line is a paragraph whose shown text carries the
// marker, and only typing completes it.
func (v *view) literalATXPrefix() bool {
	return v.pfx.live != nil && v.pfx.live.kind == livePrefixLiteral &&
		v.pfx.live.prefix != "" && emptyATXPrefix(v.pfx.live.prefix)
}

// applyLiteralSite records how the document parses p's line: a literal
// prefix keeps the line literal, and any other prefix means its bytes now
// spell a block.
func (v *view) applyLiteralSite(p *livePrefix) {
	d := v.doc
	if d == nil || d.md == nil {
		return
	}
	d.lock()
	defer d.mu.Unlock()
	if line, content := livePrefixLiteralSite(p); line >= 0 {
		d.setState(d.state().setLiteralLine(line, content))
	} else {
		d.setState(d.state().clearLiteralLineAt(p.lineStart))
	}
}

// previewDocument builds a transient's virtual copy of src as the
// document parses it, its literal lines included. Without them, a bare
// list marker line parses in the preview as an empty list item, and the
// Enter or Down applied to the preview exits the item, dropping the typed
// marker when the transient materializes.
func (v *view) previewDocument(src string) *Document {
	var state sessionState
	if v.doc != nil && v.doc.md != nil {
		v.doc.lock()
		state = v.doc.state().preview()
		v.doc.mu.Unlock()
	}
	d := literalPreview(src, state)
	return d
}

// previewDocumentInserted builds a preview of src where n bytes were
// inserted at at, moving the literal lines and session's breaks over the
// insertion.
func (v *view) previewDocumentInserted(src string, at, n int) *Document {
	var state sessionState
	if v.doc != nil && v.doc.md != nil {
		v.doc.lock()
		base := v.doc.md.p.String()
		state = v.doc.state().preview().mapPatch(at, at, n, base, src)
		v.doc.mu.Unlock()
	}
	d := literalPreview(src, state)
	return d
}

func literalPreview(src string, state sessionState) *Document {
	d := newExtMarkdownDocument(src, state, previewExtensions)
	d.md.session.openedLists = append([]int(nil), state.openedLists...)
	d.md.session.openedNested = append([]int(nil), state.openedNested...)
	d.md.keepFinalEOLCaret = true
	d.md.preview = true
	return d
}

func livePrefixLiteralSite(p *livePrefix) (int, int) {
	if p == nil {
		return -1, -1
	}
	switch p.kind {
	case livePrefixLiteral, livePrefixFence, livePrefixAtomic:
		return p.lineStart, p.prefixStart
	}
	return -1, -1
}

func (v *view) clearLiteralLineAt(lineStart int) {
	d := v.doc
	if d == nil || d.md == nil || lineStart < 0 {
		return
	}
	d.lock()
	d.setState(d.state().clearLiteralLineAt(lineStart))
	d.mu.Unlock()
}

// holdLiteralLine keeps the markers at contentStart of the line at lineStart
// literal: they are text the session typed, not a block that opens.
func (v *view) holdLiteralLine(lineStart, contentStart int) {
	d := v.doc
	if d == nil || d.md == nil || lineStart < 0 {
		return
	}
	d.lock()
	d.setState(d.state().setLiteralLine(lineStart, contentStart))
	d.mu.Unlock()
}

func (v *view) clearLivePrefix() {
	v.pfx.live = nil
}

// settleBlockPrefix ends a live prefix once the caret leaves its line.
// An unclosed fence gets its missing closing line before its live state is
// forgotten. The line stays literal: its markers were typed as text, and
// the bytes keep parsing as what the user typed for the session.
func (v *view) settleBlockPrefix() {
	p := v.pfx.live
	if p == nil || v.doc.md == nil {
		return
	}
	src := v.doc.md.p.String()
	line := liveLineAt(src, p.lineStart)
	v.doc.lock()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	unchanged := src[line.start:line.end] == p.text
	inside := unchanged && line.start == p.lineStart && sel.Caret.Offset >= line.start && sel.Caret.Offset <= line.end
	if inside && sel.Anchor.Offset >= line.start && sel.Anchor.Offset <= line.end {
		return
	}
	if p.kind == livePrefixLiteral && line.start == p.lineStart && emptyATXPrefix(src[line.start:line.end]) {
		// An empty ATX prefix has no content to commit: Typora keeps its
		// marker shown on the line the session typed it, after the caret
		// leaves and after an edit of the prefix itself (record 2026-11-10,
		// seed7/c9 and seed8/c16).
		p.lineEnd = line.end
		p.prefixStart = line.start
		p.prefix = src[line.start:line.end]
		p.text = p.prefix
		return
	}
	v.clearLivePrefix()
	if p.kind == livePrefixFence {
		// The bytes close a real block: the opener is not literal.
		v.clearLiteralLineAt(p.lineStart)
		v.closeLiveFence(p, line)
	}
}

func (v *view) closeLiveFence(p *livePrefix, line markdownLine) {
	src := v.sourceText()
	if line.start != p.lineStart || line.start > line.end || line.end > len(src) {
		return
	}
	fence, ok := fencePrefix(src[line.start:line.end])
	if !ok {
		return
	}
	eol := line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	at := line.end
	closing := eol + fence.prefix
	if line.eol == "" {
		at = line.stop
		closing += eol
	}
	v.record(false)
	v.replaceSource(at, at, closing, at)
}

// headingPrefixVisible reports the gray marker that follows a heading
// made by the live shortcut. The marker belongs to the view and does not
// change source bytes.
func (v *view) headingPrefixVisible(p int) bool {
	if v.pfx.live == nil || v.pfx.live.kind != livePrefixHeading || p < 0 || p >= v.source.Len() {
		return false
	}
	if v.para(p).style.Heading == 0 {
		return false
	}
	src := v.sourceText()
	line := liveLineAt(src, v.pfx.live.lineStart)
	if line.start != v.pfx.live.lineStart {
		return false
	}
	if line.start > v.source.end(p) || line.end < v.source.start(p) {
		return false
	}
	v.doc.lock()
	caret := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	prefixStart := v.pfx.live.prefixStart
	if prefixStart < line.start || prefixStart > line.end {
		return false
	}
	if v.transient != nil && v.shown.paraAt(v.caret) != p {
		// The caret stands on the held line past the heading: the marker is
		// no longer being typed, so it is hidden again. The document's caret
		// still sits at the line's end, which alone reads as inside it
		// (record 2026-11-12, seed16/c5: `# ` typed inside an item, `Right`s
		// away and back, shows no marker).
		return false
	}
	return caret >= line.start && caret <= line.end &&
		v.pfx.live.prefix != "" && strings.HasPrefix(src[prefixStart:line.end], v.pfx.live.prefix)
}

func (v *view) blockPrefixVisible(p int) bool {
	if v.headingPrefixVisible(p) {
		return true
	}
	if v.pfx.live == nil || v.pfx.live.kind != livePrefixLiteral ||
		v.para(p).style.Heading != 0 || v.pfx.live.prefix == "" {
		return false
	}
	src := v.sourceText()
	line := liveLineAt(src, v.pfx.live.lineStart)
	if line.start != v.pfx.live.lineStart || line.start > v.source.end(p) || line.end < v.source.start(p) {
		return false
	}
	v.doc.lock()
	caret := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	return caret >= line.start && caret <= line.end &&
		src[line.start:line.end] == v.pfx.live.prefix &&
		emptyHeadingPrefix(v.pfx.live.prefix)
}

func (v *view) blockPrefixSource(p int) (int, int, string, bool) {
	if !v.blockPrefixVisible(p) {
		return 0, 0, "", false
	}
	from := v.pfx.live.prefixStart
	to := from + len(v.pfx.live.prefix)
	return from, to, v.pfx.live.prefix, to <= len(v.sourceText())
}

func emptyHeadingPrefix(line string) bool {
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	return n > 0 && n+1 == len(line) && line[n] == ' '
}

// emptyATXPrefix reports whether line is an ATX prefix with no content: one
// to six hashes, alone or followed by the space that opens the heading.
func emptyATXPrefix(line string) bool {
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	return n > 0 && (n == len(line) || n+1 == len(line) && line[n] == ' ')
}

// finishLivePrefixOnEnter commits an exact atomic prefix. Fenced bodies
// also finish here: Enter closes the opener and leaves the caret on its
// closing fence, as Typora does.
func (v *view) finishLivePrefixOnEnter() bool {
	p := v.pfx.live
	if p == nil || v.doc.md == nil {
		return false
	}
	src := v.sourceText()
	line := liveLineAt(src, p.lineStart)
	v.doc.lock()
	sel := v.doc.selection
	v.doc.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset ||
		line.start != p.lineStart || line.end != p.lineEnd ||
		src[line.start:line.end] != p.text {
		return false
	}
	if p.kind == livePrefixHeading {
		if sel.Caret.Offset >= line.end {
			v.clearLivePrefix()
			return false
		}
		v.splitLiveHeading(p, line, sel.Caret.Offset)
		return true
	}
	eol := line.eol
	if eol == "" {
		eol = defaultEOL(src)
	}
	replace := func(from, to int, text string, caret int) {
		v.pfx.live = nil
		v.clearLiteralLineAt(p.lineStart)
		v.record(false)
		v.replaceLiveBlock(from, to, text, caret)
	}
	switch p.kind {
	case livePrefixFence:
		body := src[line.start:line.end]
		suffix := eol + p.prefix + eol + eol
		caret := line.end + len(eol)
		v.doc.lock()
		loc := v.doc.paragraphLocation(v.doc.text.paraAt(v.doc.docAtSource(line.end)))
		closeFence, nextItem, inItem := v.doc.siblingFenceClose(loc, p.prefix)
		v.doc.mu.Unlock()
		if inItem {
			// A fence typed on an item's line closes inside the item and
			// opens the next item below it, as Typora does (record
			// 2026-11-12, seed11/c11).
			suffix = eol + closeFence + eol + nextItem
			caret += len(closeFence) - len(p.prefix)
		}
		if line.stop < len(src) {
			// Keep the blank line that separated the typed block from the
			// following one, and add the new blank paragraph before it.
			suffix += eol
		}
		replace(line.start, line.stop, body+suffix, caret)
		return true
	case livePrefixAtomic:
		if !p.exact {
			return false
		}
		if p.prefix == "|a|b|" {
			table, caret := liveTable(src[line.start:line.end], line.start)
			replace(line.start, line.stop, table, caret)
			return true
		}
		suffix := eol
		if line.eol == "" {
			suffix += eol
		}
		replace(line.start, line.stop, src[line.start:line.end]+suffix, line.end+len(suffix))
		return true
	}
	return false
}

func (v *view) splitLiveHeading(p *livePrefix, line markdownLine, caret int) {
	if caret < line.start+len(p.prefix) || caret > line.end {
		v.clearLivePrefix()
		return
	}
	eol := line.eol
	if eol == "" {
		eol = defaultEOL(v.sourceText())
	}
	src := v.sourceText()
	left := src[line.start:caret]
	right := src[caret:line.end]
	v.pfx.live = nil
	v.clearLiteralLineAt(p.lineStart)
	v.record(false)
	v.replaceLiveBlock(line.start, line.end, left+eol+eol+right, line.start+len(left)+len(eol)*2)
}

func (v *view) replaceLiveBlock(from, to int, text string, caret int) {
	v.lockedEdit(func(e *Edit) {
		if v.sourceText()[from:to] == text {
			if !e.reparseMarkdownSource() {
				e.fail(ErrStructuralEdit)
			}
			return
		}
		if !e.replaceMarkdownSource(from, to, text) {
			e.fail(ErrStructuralEdit)
		}
	})
	v.setSourceSelection(sourceCaret(min(caret, len(v.sourceText()))))
}

func liveTable(line string, lineStart int) (string, int) {
	cells := strings.Split(strings.Trim(line, "|"), "|")
	width := 4
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
		width = max(width, len(cells[i]))
	}
	var b strings.Builder
	writeRow := func(values []string, empty bool) {
		b.WriteByte('|')
		for _, value := range values {
			b.WriteByte(' ')
			if empty {
				b.WriteString(strings.Repeat(" ", width+1))
			} else {
				b.WriteString(value)
				b.WriteString(strings.Repeat(" ", width-len(value)+1))
			}
			b.WriteByte('|')
		}
		b.WriteByte('\n')
	}
	writeRow(cells, false)
	b.WriteByte('|')
	for range cells {
		b.WriteString(" ")
		b.WriteString(strings.Repeat("-", width))
		b.WriteString(" |")
	}
	b.WriteByte('\n')
	body := b.Len()
	writeRow(cells, true)
	b.WriteByte('\n')
	return b.String(), lineStart + body + 2
}

// quoteMarkerSpaceTypedEnd reports a typed quote marker that opens in front
// of spaces the line already holds and writes no space of its own, and returns
// the typed end that makes the marker complete. Typora writes the marker in
// front of the line's own spaces and leaves them where they were: the line
// reads as a quote, and the caret rests after the spaces (record 2026-11-12,
// probe94/leading-space-gt, leading-two-space-gt). A tab indent is a code
// block's, where the marker is plain text and complete nothing
// (probe94/leading-tab-gt).
func quoteMarkerSpaceTypedEnd(next string, typedEnd, spaceAt int) (int, bool) {
	if spaceAt >= 0 || typedEnd <= 0 || typedEnd >= len(next) || strings.Trim(next[:typedEnd], ">") != "" {
		return typedEnd, false
	}
	i := typedEnd
	for i < len(next) && next[i] == ' ' {
		i++
	}
	if i == typedEnd || i == len(next) {
		return typedEnd, false
	}
	return i, true
}

// quoteOnlyLine reports a line holding quote markers and whitespace with no
// content: `>`, `> `, `> >`.
func quoteOnlyLine(line string) bool {
	marker := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '>':
			marker = true
		case ' ', '\t':
		default:
			return false
		}
	}
	return marker
}

func blockPrefix(blockSource string, style ParagraphStyle) string {
	line := blockSource
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	var b strings.Builder
	rest := line
	if style.Quote {
		for range style.quoteDepth() {
			if strings.HasPrefix(rest, "> ") {
				b.WriteString("> ")
				rest = rest[2:]
				continue
			}
			if strings.HasPrefix(rest, ">") {
				b.WriteByte('>')
				rest = rest[1:]
				continue
			}
			break
		}
	}
	if style.List != NoList {
		for strings.HasPrefix(rest, " ") {
			b.WriteByte(' ')
			rest = rest[1:]
		}
		if n := listPrefix(rest, style); n > 0 {
			b.WriteString(rest[:n])
		}
		return b.String()
	}
	if style.Heading > 0 {
		n := 0
		for n < len(rest) && n < style.Heading && rest[n] == '#' {
			n++
		}
		if n < len(rest) && rest[n] == ' ' {
			n++
		}
		b.WriteString(rest[:n])
		return b.String()
	}
	if style.CodeBlock {
		// Typora reports the opening fence's whole run, so a widened
		// fence shows up in the prefix too (record 2026-11-09, seed4/c23).
		rest = strings.TrimLeft(rest, " \t")
		if len(rest) >= 3 && (rest[0] == '`' || rest[0] == '~') {
			n := 0
			for n < len(rest) && rest[n] == rest[0] {
				n++
			}
			if n >= 3 {
				b.WriteString(rest[:n])
				return b.String()
			}
		}
	}
	if style == (ParagraphStyle{}) {
		for _, candidate := range []string{"$$", "---", "***", "[toc]"} {
			if !strings.HasPrefix(rest, candidate) {
				continue
			}
			if candidate == "***" && strings.TrimSpace(rest[len(candidate):]) != "" {
				// A line of its own makes `***` the prefix; with text after
				// it, the stars open emphasis and report no prefix:
				// `***a***` (record 2026-11-10, probe75/d).
				continue
			}
			b.WriteString(candidate)
			return b.String()
		}
		if prefix := atxParagraphPrefix(rest); prefix != "" {
			// A line of hashes alone, or of hashes and a space, is a
			// paragraph in Typora, and the hashes are its visible prefix
			// (record 2026-11-11, seed10/c18).
			b.WriteString(prefix)
			return b.String()
		}
		if strings.HasPrefix(rest, "|") {
			b.WriteByte('|')
		}
	}
	return b.String()
}

// atxParagraphPrefix returns the ATX-shaped prefix of a paragraph's line: a
// run of hashes ending the line or followed by one space, or "". Typora
// reports those bytes as the block's prefix even though the line is a
// paragraph, as its snapshot's prefix extraction does.
func atxParagraphPrefix(line string) string {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 {
		return ""
	}
	if n == len(line) {
		return line
	}
	if line[n] == ' ' {
		return line[:n+1]
	}
	return ""
}

func listPrefix(s string, style ParagraphStyle) int {
	if style.List == Tasks {
		for _, marker := range []string{"- [ ]", "- [x]", "- [X]"} {
			if strings.HasPrefix(s, marker) {
				n := len(marker)
				if n < len(s) && s[n] == ' ' {
					n++
				}
				return n
			}
		}
		return 0
	}
	if style.List == Bullets {
		if s == "" || !strings.ContainsRune("-*+", rune(s[0])) {
			return 0
		}
		n := 1
		for n < len(s) && s[n] == ' ' {
			n++
		}
		return n
	}
	if style.List == Numbers {
		if n := orderedListMarker(s); n > 0 {
			return n
		}
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
			return i + 1
		}
	}
	return 0
}

func orderedListMarker(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(s) || (s[i] != '.' && s[i] != ')') || s[i+1] != ' ' {
		return 0
	}
	return i + 2
}
