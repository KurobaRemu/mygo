package richtext

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

type DisplayOffset struct {
	Display     int
	Doc         int
	Source      int
	SourceAfter int
	Marker      bool
}

type DisplayMap struct {
	offsets []DisplayOffset
}

type DisplayParagraph struct {
	Paragraph    int
	Spans        []ui.Span
	Map          DisplayMap
	composeStart int
	composeEnd   int
}

func (m DisplayMap) Len() int {
	return max(0, len(m.offsets)-1)
}

func (m DisplayMap) At(i int) DisplayOffset {
	if len(m.offsets) == 0 {
		return DisplayOffset{}
	}
	i = max(0, min(i, len(m.offsets)-1))
	return m.offsets[i]
}

func (m DisplayMap) DisplayOfDoc(doc int, upper bool) int {
	if len(m.offsets) == 0 {
		return 0
	}
	if upper {
		for i := len(m.offsets) - 1; i >= 0; i-- {
			if m.offsets[i].Doc <= doc {
				return i
			}
		}
		return 0
	}
	for i, o := range m.offsets {
		if o.Doc >= doc {
			return i
		}
	}
	return len(m.offsets) - 1
}

func (m DisplayMap) DisplayOfSource(source int, upper bool) int {
	if len(m.offsets) == 0 {
		return 0
	}
	if upper {
		for i := len(m.offsets) - 1; i >= 0; i-- {
			if m.offsets[i].Source <= source {
				return i
			}
		}
		return 0
	}
	for i, o := range m.offsets {
		// A boundary covers the source bytes up to its SourceAfter: the
		// glyphs after it start further on when the display hides the
		// bytes between, as a link's destination is hidden. A point on
		// those hidden bytes renders at that boundary, before the glyph
		// the display puts after them (record 2026-11-13, seed16/c10).
		if o.SourceAfter >= source {
			return i
		}
	}
	return len(m.offsets) - 1
}

func (m DisplayMap) DisplayAt(doc, source, prefer int) int {
	if len(m.offsets) == 0 {
		return 0
	}
	prefer = max(0, min(prefer, len(m.offsets)-1))
	best, distance := prefer, int(^uint(0)>>1)
	for i, o := range m.offsets {
		if o.Doc != doc || o.Source != source {
			continue
		}
		d := i - prefer
		if d < 0 {
			d = -d
		}
		if d < distance {
			best, distance = i, d
		}
	}
	return best
}

type displayPart struct {
	text   string
	src    string
	attrs  Attributes
	doc    int
	docEnd int
	source int
	srcEnd int
	marker bool
}

func buildDisplayMap(parts []displayPart, docStart, sourceStart int) DisplayMap {
	offsets := []DisplayOffset{{Doc: docStart, Source: sourceStart, SourceAfter: sourceStart}}
	for _, part := range parts {
		runes := []rune(part.text)
		if len(runes) == 0 {
			continue
		}
		last := &offsets[len(offsets)-1]
		if part.source > last.Source {
			last.SourceAfter = part.source
		}
		src := sourceText(part)
		srcRunes := utf8.RuneCountInString(src)
		for i := range runes {
			docNext := part.doc + (part.docEnd-part.doc)*(i+1)/len(runes)
			sourceNext := part.source + sourceAdvance(src, srcRunes, i, len(runes))
			offsets = append(offsets, DisplayOffset{
				Display:     len(offsets),
				Doc:         docNext,
				Source:      sourceNext,
				SourceAfter: sourceNext,
				Marker:      part.marker,
			})
		}
	}
	return DisplayMap{offsets: offsets}
}

func sourceText(part displayPart) string {
	return part.src
}

func sourceAdvance(text string, runes, run, total int) int {
	if total <= 0 || runes <= 0 {
		return len(text)
	}
	if runes == total || total == 1 {
		if run == total-1 {
			return len(text)
		}
		if runes == total {
			return runeBytesBefore(text, run+1)
		}
	}
	at := (runes*(run+1) + total - 1) / total
	return runeBytesBefore(text, at)
}

func runeBytesBefore(s string, n int) int {
	at := 0
	for ; n > 0 && at < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

type emptyContainer struct {
	para int
	box  box
	// pair marks a delimiter pair a formatting command wrote, or the typing
	// that completed one. Backspace deletes one byte of such a pair, while a
	// pair left by deleting a span's content drops whole (record
	// 2026-11-12-133304, seed12/c6).
	pair bool
}

// sourceEditBoxes returns the source-backed inline boxes of paragraph p,
// including the empty delimiter container the view retains. Editing, deletion
// and source inspection read these boxes: a live held pair is only a visible
// syntax reveal, never source-edit state (see live_hold.go).
func (v *view) sourceEditBoxes(p int) []box {
	if v.source.Len() <= p {
		return nil
	}
	boxes := v.source.boxes(p)
	if v.empty != nil && v.empty.para == p {
		// A retained container is only the fact source while the parser
		// does not hold the element itself, as with `****` after deleting
		// the content of a strong span.
		duplicate := false
		for _, b := range boxes {
			if b.kind == v.empty.box.kind && b.start == v.empty.box.start && b.end == v.empty.box.end {
				duplicate = true
				break
			}
		}
		if !duplicate {
			boxes = append(boxes, v.empty.box)
		}
	}
	return boxes
}

func (v *view) boxes(p int) []box { return v.sourceEditBoxes(p) }

// visibleSyntaxBoxes returns the boxes a collapsed caret can reveal in
// paragraph p. It starts from the source-edit boxes and adds the session-held
// strong pair only for this reveal pass; callers that write or delete source
// bytes must use sourceEditBoxes instead.
func (v *view) visibleSyntaxBoxes(p int) []box {
	source := v.revealCaretSource()
	return v.withLiveHeldBox(p, source, v.sourceEditBoxes(p))
}

func (v *view) caretSource() int {
	if v.selAnchor != v.caret {
		return -1
	}
	v.doc.lock()
	source := v.doc.selection.Caret.Offset
	v.doc.mu.Unlock()
	return source
}

// revealCaretSource is the caret a held line break's inline spans reveal by.
// The document's caret stays on the break's anchor while the view's caret
// stands after the break: the break's bytes are the preview's, and the live
// caret is past the closing marker it follows (record 2026-11-10, seed9/c4).
func (v *view) revealCaretSource() int {
	if t := v.transient; t != nil && t.isLineBreak() {
		return t.preview.SourceSelection().Caret.Offset
	}
	return v.caretSource()
}

func (v *view) sourceAtDoc(doc int) int {
	if v.source.Len() == 0 {
		return 0
	}
	doc = max(0, min(doc, v.shown.Len()))
	if v.empty != nil {
		at := v.shown.paraStart(v.empty.para) + v.empty.box.docStart
		if doc == at {
			return v.empty.box.contentFrom
		}
	}
	p := v.shown.paraAt(doc)
	if p >= v.source.Len() {
		if v.transient != nil && p == v.transient.para {
			return v.transient.anchor.Offset
		}
		return 0
	}
	local := doc - v.shown.paraStart(p)
	source := v.source.start(p)
	shown := 0
	for _, s := range seqAt(v.source.paras, p).segs {
		if shown == local {
			return source
		}
		if s.Shown > 0 && local < shown+s.Shown {
			body := v.sourceSlice(source, source+s.Source)
			return source + sourceAdvance(body, utf8.RuneCountInString(body), local-shown, s.Shown)
		}
		shown += s.Shown
		source += s.Source
	}
	return source
}

func (v *view) paragraphEndSource(p int) int {
	if p >= v.source.Len() {
		if v.transient != nil && p == v.transient.para {
			return v.transient.anchor.Offset
		}
		return 0
	}
	source := v.source.start(p)
	if p == 0 && v.shown.Len() == 0 {
		// A document that shows one empty paragraph ends where its content
		// starts, after any bytes the paragraph follows, as a container
		// prefix or blank lines. A final line ending is the document's
		// terminator, not a byte that holds the caret, so the paragraph
		// cannot end inside it; the view's source map is the document's.
		src := v.sourceText()
		if at := emptyParagraphSource(src, v.source.start(0)); at >= 0 {
			source = at
		}
		if n := len(src); n > 0 && (src[n-1] == '\n' || src[n-1] == '\r') {
			end := n - 1
			if src[n-1] == '\n' && n > 1 && src[n-2] == '\r' {
				end--
			}
			source = min(source, end)
		}
		if start := v.source.start(0); start > source && !strings.ContainsAny(src[source:start], "\r\n") {
			source = start
		}
		return source
	}
	for _, s := range seqAt(v.source.paras, p).segs {
		source += s.Source
	}
	for moved := true; moved; {
		moved = false
		for _, b := range v.boxes(p) {
			if b.closeStart == source && b.closeStart < b.end {
				source, moved = b.end, true
			}
		}
	}
	// A setext heading owns its underline: its content line ends before the
	// line that makes it a heading (record 2026-11-10, newops/c46-5).
	if v.doc != nil {
		return v.doc.setextHeadingSourceEnd(p, source)
	}
	return source
}

func (v *view) sourceSlice(from, to int) string {
	src := v.sourceText()
	if src == "" || from < 0 || from > to {
		return ""
	}
	to = min(to, len(src))
	if from > len(src) {
		return ""
	}
	return src[from:to]
}

func (v *view) sourceText() string {
	if v.transient != nil {
		return v.transient.preview.Source()
	}
	if v.doc.md == nil {
		return ""
	}
	return v.doc.md.p.String()
}

func (v *view) docAtSource(source int) (int, bool) {
	if v.source.Len() == 0 {
		return max(0, min(source, v.shown.Len())), true
	}
	if v.transient != nil && v.transient.isEmptyParagraph() &&
		v.transient.para < v.shown.ParagraphCount() {
		if source == v.transient.anchor.Offset {
			return v.shown.paraStart(v.transient.para) + v.para(v.transient.para).n, true
		}
	}
	if v.empty != nil && source >= v.empty.box.start && source <= v.empty.box.end {
		return v.shown.paraStart(v.empty.para) + v.empty.box.docStart, true
	}
	startP := 0
	if source > 0 {
		startP = max(0, v.source.from(source)-1)
	}
	for p := startP; p < v.source.Len() && p < v.shown.ParagraphCount(); p++ {
		at := v.source.start(p)
		if source < at {
			break
		}
		doc := v.shown.paraStart(p)
		shown := 0
		pm := seqAt(v.source.paras, p)
		end := at + pm.src
		if source < at || source > end {
			continue
		}
		cursor := at
		for _, s := range pm.segs {
			next := cursor + s.Source
			if cursor == next && source == cursor {
				// A zero-length segment, as a paragraph's leading gap,
				// holds the offset at its own rune.
				return doc + shown, true
			}
			if source < next {
				if s.Shown <= 0 {
					return doc + shown, true
				}
				body := v.sourceSlice(cursor, next)
				want := source - cursor
				if s.Kind == SegmentVerbatim {
					// A verbatim segment shows its source rune by rune;
					// other segments, as a line break with its container's
					// continuation prefix, map as a whole.
					return doc + shown + utf8.RuneCountInString(body[:min(want, len(body))]), true
				}
				srcCount := utf8.RuneCountInString(body)
				delta := 0
				for delta < s.Shown && sourceAdvance(body, srcCount, delta, s.Shown) < want {
					delta++
				}
				return doc + shown + delta, true
			}
			cursor = next
			shown += s.Shown
		}
		return doc + shown, true
	}
	return 0, false
}

func (v *view) displayNavigation() bool {
	if v.source.Len() == 0 || v.compose != "" || v.pendingSet {
		return false
	}
	p := v.shown.paraAt(v.caret)
	if len(v.revealedBoxes(p)) > 0 {
		return true
	}
	// A selection that spans a range draws no inline markers, so nothing is
	// revealed, but its moving edge still steps one visible rune at a time:
	// the display map's folded gaps carry the delimiter bytes a step leaves
	// for the next one (record 2026-11-11, seed10/c14).
	return !v.selectionCollapsedInSource() && v.hiddenMarkerBox(p)
}

// hiddenMarkerBox reports whether paragraph p folds an inline delimiter run
// that is not drawn as text of its own.
func (v *view) hiddenMarkerBox(p int) bool {
	for _, b := range v.boxes(p) {
		if b.literal || b.kind.hardBreak() {
			continue
		}
		if b.openEnd > b.openStart || b.closeEnd > b.closeStart {
			return true
		}
	}
	return false
}

// openingRunGap reports whether the source gap [from, to) a display boundary
// folds away is exactly an element's opening delimiter run. A caret at that
// boundary stands before the element's content, on the run's far side.
func (v *view) openingRunGap(p, from, to int) bool {
	if from >= to {
		return false
	}
	for _, b := range v.boxes(p) {
		if b.literal || b.kind.hardBreak() {
			continue
		}
		if b.openEnd > b.openStart && from == b.openStart && to == b.openEnd {
			return true
		}
	}
	return false
}

func (v *view) stepDisplay(left, extend bool) bool {
	p := v.shown.paraAt(v.caret)
	pv := v.paraLayout(p)
	if pv.display == nil {
		return false
	}
	v.doc.lock()
	source := v.doc.selection.Caret
	v.doc.mu.Unlock()
	at := v.displayPositionForCaret(p, pv, source, left)
	next := at
	if left {
		next--
	} else {
		next++
	}
	if next < 0 || next > pv.display.Map.Len() {
		i, aff := v.visual(1)
		if left {
			i, aff = v.visual(-1)
		}
		v.moveTo(i, aff, extend)
		return true
	}
	o := pv.display.Map.At(next)
	// A caret stepping left to a boundary in front of an element's content
	// stands outside the opening delimiter run: the display map folds the run
	// into that boundary, and its bytes belong to later steps (seed10/c14).
	atSource := o.Source
	if left && v.openingRunGap(p, o.Source, o.SourceAfter) {
		atSource = o.SourceAfter
	}
	v.affinity = ui.TextDownstream
	if left {
		v.affinity = ui.TextUpstream
	}
	affinity := BoundaryDownstream
	if left {
		affinity = BoundaryUpstream
	}
	v.doc.lock()
	if extend {
		anchor := v.doc.selection.Anchor
		if v.empty != nil && v.empty.para == p &&
			v.empty.box.contentFrom == v.empty.box.contentTo &&
			anchor.Offset == v.empty.box.contentFrom {
			// A selection that starts inside a retained empty pair keeps
			// its anchor inside the element, before the closing delimiters
			// (record 2026-11-10, seed7/c15).
			anchor.Affinity = BoundaryUpstream
		}
		v.setSourceSelectionLocked(SourceSelection{Anchor: anchor, Caret: sourcePoint(atSource, affinity)})
	} else {
		point := sourcePoint(atSource, affinity)
		v.setSourceSelectionLocked(SourceSelection{Anchor: point, Caret: point})
	}
	v.anchorAffinity = v.doc.selection.Anchor.Affinity
	v.caretAffinity = v.doc.selection.Caret.Affinity
	v.doc.mu.Unlock()
	v.coalesce = false
	v.reveal = true
	v.hasDesired = false
	v.maybeLeaveEmpty()
	return true
}

func (v *view) displayPositionForCaret(p int, pv *paraView, source SourcePoint, left bool) int {
	at := v.caretDisplay(p, pv, source)
	start := v.shown.paraStart(p)
	end := start + v.para(p).n
	for _, b := range v.boxes(p) {
		if (b.kind == MarkerLink || b.kind == MarkerCode) &&
			source.Offset == b.end && v.caret == start+b.docEnd && v.caret < end {
			// A link or code element stays outside its closing syntax at
			// its visible end while the paragraph goes on after it:
			// movement starts at that visible edge, not at the end of the
			// revealed syntax (Left enters the link's destination there).
			// At the paragraph's own end no visible edge follows the
			// caret, so it stays past the revealed syntax and Right leaves
			// the paragraph (record 2026-11-10, seed4/c16; record
			// 2026-11-12, seed11/c4).
			return pv.display.Map.DisplayOfDoc(v.caret, false)
		}
	}
	return at
}

// caretDisplay is where the caret at a source point renders in paragraph
// p's display. The display map answers for every point, including the
// content point of an empty emphasis pair the view retains: Typora records
// the caret between the inserted delimiters there (seed8/c17, seed9/c20),
// as it does when the pair is the paragraph's whole content (21340604).
func (v *view) caretDisplay(p int, pv *paraView, source SourcePoint) int {
	if p < v.source.Len() && source.Offset >= v.source.end(p) {
		// A caret at the paragraph's source end stands after every glyph
		// the display draws, including one with no bytes of its own: the
		// space that completes a live `# ` heading prefix draws as a
		// marker glyph whose source sits at the paragraph's end, and the
		// downstream lookup would stop in front of it (record 2026-11-10,
		// seed6/transient-heading-anchor).
		return pv.display.Map.DisplayOfSource(source.Offset, true)
	}
	if p < v.source.Len() && source.Affinity != BoundaryUpstream && source.Offset <= v.source.start(p) {
		if up := pv.display.Map.DisplayOfSource(source.Offset, true); up > 0 {
			// Glyphs the display draws in front of the paragraph's first
			// byte carry no source of their own, and the caret stands
			// after them: an indented code block shows the indentation
			// the parser took off as the line's first column, and Typora
			// keeps the caret behind it (record 2026-11-12,
			// probe94/leading-tab-gt).
			return up
		}
	}
	return pv.display.Map.DisplayOfSource(source.Offset, source.Affinity == BoundaryUpstream)
}

func (v *view) maybeLeaveEmpty() {
	if v.empty == nil {
		return
	}
	if !v.selectionCollapsedInSource() {
		// A selection that extends over the pair keeps it: its bytes are
		// selected even though they draw nothing of their own (record
		// 2026-11-10, seed7/c15).
		return
	}
	source := v.caretSource()
	// The pair's right edge is still the pair: Typora keeps the empty
	// strong element when the caret moves past its closing delimiters
	// (record 2026-11-10, probe/pair/right-to-end). Past the edge the caret
	// has left it.
	if source <= v.empty.box.start || source > v.empty.box.end {
		v.removeEmpty()
	}
}

func (v *view) displayPositionAt(x float32, y float64) (p, l int, o DisplayOffset, aff ui.TextAffinity) {
	p, l, aff = v.layoutPositionAt(x, y)
	if p < 0 {
		return p, l, DisplayOffset{Doc: v.shown.Len()}, aff
	}
	pv := v.paraLayout(p)
	if pv.display != nil {
		o = pv.display.Map.At(l)
		if o.SourceAfter > o.Source {
			unit := pv.units[min(l, len(pv.units)-1)]
			caret := pv.layout.CaretAt(ui.TextCaretPosition{Index: unit, Affinity: aff})
			ox, _ := v.origin(p)
			if x > ox+caret.X {
				o.Source = o.SourceAfter
			}
		}
	}
	return p, l, o, aff
}

func (v *view) linkBoxAt(p, source int) (int, box, bool) {
	boxes := v.boxes(p)
	best := -1
	var found box
	for i, b := range boxes {
		if b.kind != MarkerLink || source < b.start || source > b.end {
			continue
		}
		if best < 0 || b.start >= found.start && b.end <= found.end {
			best, found = i, b
		}
	}
	return best, found, best >= 0
}

func (v *view) fragmentTarget(link string) (int, bool) {
	fragment, err := url.PathUnescape(strings.TrimPrefix(link, "#"))
	if err != nil {
		fragment = strings.TrimPrefix(link, "#")
	}
	fragment = strings.ToLower(fragment)
	for i := 0; i < v.shown.ParagraphCount(); i++ {
		p := v.shown.paragraph(i)
		if p.Style.Heading <= 0 {
			continue
		}
		var b strings.Builder
		for _, run := range p.Runs {
			b.WriteString(run.Text)
		}
		text := strings.TrimSpace(b.String())
		if strings.ToLower(text) == fragment || headingSlug(text) == fragment {
			return v.shown.paraStart(i), true
		}
	}
	return 0, false
}

func headingSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case unicode.IsSpace(r):
			dash = true
		}
	}
	return b.String()
}

func (v *view) revealedBoxes(p int) []box {
	if !v.selectionCollapsedInSource() {
		// A selection that spans marker bytes only keeps the document
		// offsets collapsed: the markers have no display of their own, so
		// the two ends draw at one visible point, as Typora draws them,
		// and moving over them does not reveal them (record 2026-11-10,
		// seed4/c30).
		return v.hoveredBoxes(p)
	}
	if v.wordMove {
		return v.hoveredBoxes(p)
	}
	if v.shown.paraAt(v.caret) != p {
		// The caret is drawn in another paragraph. Its source offset can
		// equal the end of this one, as when the next paragraph is a line
		// the session has not written yet, and that does not put the caret
		// inside this paragraph's spans (record 2026-11-09, seed3/c24).
		return v.hoveredBoxes(p)
	}
	// The strong element the session's own edit put in the view is a visible
	// syntax reveal: its delimiters still parse as text (see live_hold.go).
	boxes := v.visibleSyntaxBoxes(p)
	source := v.revealCaretSource()
	// 建一张 closeStart→closeEnd 的表，扩展链按表前进，不再为每个 box
	// 重扫全部 boxes。扫描是 boxes²；盒子少时建表的分配反而更贵，按
	// 实测交叉点（32 个 box）保留原路径。
	next := revealCloseIndex(boxes)
	var out []box
	for _, b := range boxes {
		if b.literal && !b.kind.keepsEmptyMarkers() {
			// Its delimiters are drawn as text, not revealed markers.
			continue
		}
		if b.kind == MarkerHardBreakBackslash && v.doc != nil && v.doc.heldBreakAt(b.start) {
			// 本会话为持有空行写的反斜杠是编辑器的字节：Typora 的
			// 源码里没有它，光标停在这条空行上时也不显示。
			continue
		}
		end := b.end
		if next != nil {
			end = revealEndIndex(next, end)
		} else {
			end = revealEndScan(boxes, end)
		}
		if source >= b.start && source <= end {
			out = append(out, b)
		}
	}
	// A pair that opens where a revealed opening run ends is part of the
	// same revealed syntax: in `***a***` the emphasis opens with the run's
	// first star and the strong with its last two, so a caret within either
	// delimiter shows both (record 2026-11-10, probe75/d, /p, /s).
	for changed := true; changed; {
		changed = false
		for _, b := range out {
			if b.openEnd <= b.openStart {
				continue
			}
			for _, c := range boxes {
				if c.start < b.start || c.start > b.openEnd || containsBox(out, c) {
					continue
				}
				out = append(out, c)
				changed = true
			}
		}
	}
	if v.hoverPara == p {
		for _, b := range v.hoveredBoxes(p) {
			if !containsBox(out, b) {
				out = append(out, b)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end > out[j].end
	})
	return out
}

// revealCloseIndexMin 是建表比扫描更便宜的 boxes 数量。实测（M4 Pro，
// 每个 box 都走一遍扩展）：16 个 box 扫描 168ns、建表 657ns；32 个
// 748ns 对 619ns；64 个 2.65µs 对 1.66µs。32 以下保留扫描。
const revealCloseIndexMin = 32

// revealCloseIndex 把「闭合标记起点」映射到「闭合标记终点」，只收
// 自身结束于闭合标记终点的 box——扩展链只可能踏进这样的 box。同一
// 起点有多个候选时取 boxes 里第一个，与逐次扫描的胜负顺序一致。
func revealCloseIndex(boxes []box) map[int]int {
	if len(boxes) < revealCloseIndexMin {
		return nil
	}
	next := make(map[int]int, len(boxes))
	for _, c := range boxes {
		if c.closeEnd > c.closeStart && c.end == c.closeEnd {
			if _, ok := next[c.closeStart]; !ok {
				next[c.closeStart] = c.closeEnd
			}
		}
	}
	return next
}

// revealEndIndex 沿闭合标记链前进：光标刚越过嵌套闭合标记时，Typora
// 同时揭示内外两层（record 2026-11-09, seed5/c5）。链长是嵌套深度，
// 每步终点严格变大，不会回环。
func revealEndIndex(next map[int]int, end int) int {
	for {
		n, ok := next[end]
		if !ok {
			return end
		}
		end = n
	}
}

// revealEndScan 是盒子少时的原路径：逐个扫描找结束点正好接在本 box
// 末尾的闭合标记。
func revealEndScan(boxes []box, end int) int {
	for {
		grown := false
		for _, c := range boxes {
			if c.closeStart == end && c.closeEnd > end && c.end == c.closeEnd {
				end = c.closeEnd
				grown = true
			}
		}
		if !grown {
			return end
		}
	}
}

// selectionCollapsedInSource reports whether the selection's source points
// coincide, which a selection over syntax markers alone does not share with
// the document offsets.
func (v *view) selectionCollapsedInSource() bool {
	d := v.doc
	if d == nil {
		return v.selAnchor == v.caret
	}
	d.lock()
	collapsed := d.selection.Anchor.Offset == d.selection.Caret.Offset
	d.mu.Unlock()
	return collapsed
}

func (v *view) hoveredBoxes(p int) []box {
	var out []box
	if p < 0 || v.hoverPara != p {
		return out
	}
	boxes := v.boxes(p)
	if v.hoverBox < 0 || v.hoverBox >= len(boxes) {
		return out
	}
	hover := boxes[v.hoverBox]
	for _, b := range boxes {
		if hover.start >= b.start && hover.end <= b.end {
			out = append(out, b)
		}
	}
	return out
}

func containsBox(boxes []box, b box) bool {
	for _, q := range boxes {
		if q == b {
			return true
		}
	}
	return false
}

func markerSource(b box) (string, string) {
	if b.kind == MarkerHardBreakSpace {
		return "↓", strings.Repeat(" ", max(1, b.end-b.start))
	}
	return "\\", "\\"
}

func (v *view) displayParagraph(p int, typing Attributes) DisplayParagraph {
	active := v.revealedBoxes(p)
	docStart := v.shown.paraStart(p)
	sourceStart := 0
	if p < v.source.Len() {
		sourceStart = v.source.start(p)
	}
	parts := v.displayParts(p, active, sourceStart)
	composeStart, composeEnd := -1, -1
	a, z := v.composeRange()
	if (v.pendingSet || v.compose != "") && v.shown.paraAt(a) == p {
		source := [2]int{v.pendingSelection.Anchor.Offset, v.pendingSelection.Caret.Offset}
		sourceSet := v.pendingSourceSet
		if !sourceSet && v.doc.md != nil {
			v.doc.lock()
			caret := v.doc.selection.Caret.Offset
			v.doc.mu.Unlock()
			source, sourceSet = [2]int{caret, caret}, true
		}
		parts, composeStart, composeEnd = replaceDisplayRange(parts, a, z, source, sourceSet, displayPart{
			text: v.compose, attrs: typing,
			doc: a, docEnd: a,
			source: source[0], srcEnd: source[0],
		})
	}
	displayMap := buildDisplayMap(parts, docStart, sourceStart)
	composeSource := [2]int{v.pendingSelection.Anchor.Offset, v.pendingSelection.Caret.Offset}
	composeSourceSet := v.pendingSourceSet
	if !composeSourceSet && v.doc.md != nil {
		v.doc.lock()
		caret := v.doc.selection.Caret.Offset
		v.doc.mu.Unlock()
		composeSource, composeSourceSet = [2]int{caret, caret}, true
	}
	setCompositionMap(&displayMap, composeStart, composeEnd, a, sourceStart, composeSource, composeSourceSet)
	spans := make([]ui.Span, 0, len(parts))
	// The paragraph's style, once: rebuilding the paragraph per span costs
	// the paragraph's runs and line at each of its spans.
	style := v.para(p).style
	for _, part := range parts {
		if part.text == "" {
			continue
		}
		if part.marker {
			a := part.attrs
			a = Attributes{Color: v.look.Muted}
			spans = append(spans, v.look.span(part.text, a, style))
			continue
		}
		spans = append(spans, v.look.span(part.text, part.attrs, style))
	}
	return DisplayParagraph{Paragraph: p, Spans: spans, Map: displayMap, composeStart: composeStart, composeEnd: composeEnd}
}

func replaceDisplayRange(parts []displayPart, docFrom, docTo int, source [2]int, sourceSet bool, insert displayPart) ([]displayPart, int, int) {
	var out []displayPart
	at, end := -1, -1
	inserted := false
	appendInsert := func() {
		if inserted {
			return
		}
		at = 0
		for _, part := range out {
			at += utf8.RuneCountInString(part.text)
		}
		if insert.text != "" {
			out = append(out, insert)
			end = at + utf8.RuneCountInString(insert.text)
		} else {
			end = at
		}
		inserted = true
	}
	for _, part := range parts {
		runes := []rune(part.text)
		count := len(runes)
		if count == 0 {
			continue
		}
		srcRunes := utf8.RuneCountInString(part.src)
		// boundary is the source offset in front of rune i. A part stands
		// for bytes of its own (a verbatim `[]`, a link's destination), and
		// the composition joins it between runes as the source does, not at
		// the whole part's edges only: `[|]` composes inside the brackets
		// (record 2026-11-13, issue 178).
		boundary := func(i int) int {
			return part.source + displayBoundary(part.src, srcRunes, i, count)
		}
		covered := func(i int) bool {
			docA := part.doc + (part.docEnd-part.doc)*i/count
			docB := part.doc + (part.docEnd-part.doc)*(i+1)/count
			hit := docA < docTo && docB > docFrom
			if part.doc == part.docEnd {
				hit = false
			}
			if sourceSet && boundary(i) < source[1] && boundary(i+1) > source[0] {
				hit = true
			}
			return hit
		}
		atOrAfter := func(i int) bool {
			if sourceSet {
				return boundary(i) >= source[0]
			}
			if part.marker {
				return i == 0 && part.source >= source[0]
			}
			return part.doc+(part.docEnd-part.doc)*i/count >= docFrom
		}
		for i := 0; i < count; {
			if !inserted && atOrAfter(i) {
				appendInsert()
			}
			hit := covered(i)
			j := i + 1
			// The run stops at the composition's insertion point even when
			// the text around it is all the same: the preedit joins the
			// part's own bytes between two runes, as `[|]` draws `[zhong]`
			// rather than `[]zhong` (record 2026-11-13, issue 178).
			for j < count && covered(j) == hit && (inserted || !atOrAfter(j)) {
				j++
			}
			if !hit {
				out = append(out, splitDisplayPart(part, i, j))
			} else if !inserted {
				appendInsert()
			}
			i = j
		}
	}
	if !inserted {
		appendInsert()
	}
	return out, at, end
}

func splitDisplayPart(part displayPart, from, to int) displayPart {
	count := utf8.RuneCountInString(part.text)
	runes := []rune(part.text)
	srcRunes := utf8.RuneCountInString(part.src)
	docA := part.doc + (part.docEnd-part.doc)*from/count
	docB := part.doc + (part.docEnd-part.doc)*to/count
	srcA := displayBoundary(part.src, srcRunes, from, count)
	srcB := displayBoundary(part.src, srcRunes, to, count)
	part.text = string(runes[from:to])
	part.doc, part.docEnd = docA, docB
	part.source, part.srcEnd = part.source+srcA, part.source+srcB
	part.src = part.src[srcA:srcB]
	return part
}

func setCompositionMap(m *DisplayMap, from, to, doc int, sourceStart int, source [2]int, sourceSet bool) {
	if from < 0 || len(m.offsets) == 0 {
		return
	}
	from = min(max(from, 0), len(m.offsets)-1)
	to = min(max(to, from), len(m.offsets)-1)
	start, end := sourceStart, sourceStart
	if sourceSet {
		start, end = source[0], source[1]
	}
	for i := from; i <= to; i++ {
		m.offsets[i].Doc = doc
		m.offsets[i].Source = start
		m.offsets[i].SourceAfter = end
	}
	if to < len(m.offsets)-1 {
		m.offsets[to].SourceAfter = end
	}
}

func (v *view) displayParts(p int, active []box, sourceStart int) []displayPart {
	if p >= v.source.Len() {
		return v.plainDisplayParts(p)
	}
	src := v.sourceText()
	var parts []displayPart
	literalPrefix := false
	if from, to, text, ok := v.blockPrefixSource(p); ok {
		literalPrefix = v.para(p).style.Heading == 0 && text == src[from:to]
		if !literalPrefix {
			parts = append(parts, displayPart{
				text: text, src: src[from:to], doc: v.shown.paraStart(p), docEnd: v.shown.paraStart(p),
				source: from, srcEnd: to, marker: true,
			})
		}
	}
	line := []rune(v.lineOf(p))
	pm := seqAt(v.source.paras, p)
	source := sourceStart
	shown := 0
	// The runs holding the segments' attributes, walked along the shown
	// text: a lookup per segment from the paragraph's start is quadratic.
	para := v.para(p)
	run, runAt := 0, 0
	attributes := func(at int) Attributes {
		if at >= para.n {
			return Attributes{}
		}
		for run < len(para.runs)-1 && runAt+para.runs[run].n <= at {
			runAt += para.runs[run].n
			run++
		}
		return para.runs[run].a
	}
	addMarker := func(b box, open bool) {
		from, to := b.openStart, b.openEnd
		if !open {
			from, to = b.closeStart, b.closeEnd
		}
		if from < 0 || to <= from {
			return
		}
		hardBreak := b.kind.hardBreak()
		if hardBreak && !open {
			return
		}
		text, raw := src[from:to], src[from:to]
		if hardBreak {
			text, raw = markerSource(b)
		}
		parts = append(parts, displayPart{
			text: text, src: raw, doc: v.shown.paraStart(p) + shown, docEnd: v.shown.paraStart(p) + shown,
			source: from, srcEnd: to, marker: true,
		})
	}
	for i, s := range pm.segs {
		for _, b := range active {
			if b.open == i {
				addMarker(b, true)
			}
			if b.close == i && b.closeStart < b.closeEnd {
				addMarker(b, false)
			}
		}
		if s.Shown > 0 {
			from := shown
			to := min(from+s.Shown, len(line))
			text := string(line[from:to])
			a := attributes(from)
			raw := src[max(0, source):max(source, source+s.Source)]
			parts = append(parts, v.docDisplayParts(p, active, text, raw, a, v.shown.paraStart(p)+from, source)...)
			shown += s.Shown
		}
		source += s.Source
	}
	for _, b := range active {
		if b.kind.hardBreak() {
			text, raw := markerSource(b)
			parts = append(parts, displayPart{
				text: text, src: raw, doc: v.shown.paraStart(p) + shown, docEnd: v.shown.paraStart(p) + shown,
				source: b.start, srcEnd: b.end, marker: true,
			})
		}
	}
	if v.transient == nil && p < v.source.Len() && v.para(p).style.Heading > 0 && v.doc != nil {
		// Typora keeps a closing sequence the session typed at the end of
		// the heading's DOM, so it stays on screen (record 2026-11-09,
		// diff.json bdbf85b7).
		if closerEnd, ok := v.doc.liveCloserEndAt(source); ok && closerEnd > source {
			text := src[source:closerEnd]
			parts = append(parts, displayPart{
				text: text, src: text,
				doc: v.shown.paraStart(p) + shown, docEnd: v.shown.paraStart(p) + shown,
				source: source, srcEnd: closerEnd,
			})
		}
	}
	for _, b := range v.boxes(p) {
		if !b.literal || strings.Contains(string(line), src[max(0, b.start):max(b.start, b.end)]) {
			continue
		}
		text := src[max(0, b.start):max(b.start, b.end)]
		parts = append(parts, displayPart{
			text: text, src: text, doc: v.shown.paraStart(p) + shown, docEnd: v.shown.paraStart(p) + shown,
			source: b.start, srcEnd: b.end, marker: true,
		})
	}
	if h := v.hiddenLiveHeldPair(p); h != nil {
		// The element the session's own split left behind draws its content
		// without the delimiters the parser reads as text (see live_hold.go).
		parts = hideHeldDelimiters(parts, h)
	}
	if literalPrefix {
		parts = markDisplaySource(parts, sourceStart, sourceStart+len(v.pfx.live.prefix))
	}
	return parts
}

func markDisplaySource(parts []displayPart, from, end int) []displayPart {
	var out []displayPart
	for _, part := range parts {
		start, stop := max(from, part.source), min(end, part.srcEnd)
		if start >= stop {
			out = append(out, part)
			continue
		}
		count := utf8.RuneCountInString(part.text)
		srcRunes := utf8.RuneCountInString(part.src)
		fromRune := min(count, sourceRuneAt(part.src, srcRunes, start-part.source))
		toRune := min(count, sourceRuneAt(part.src, srcRunes, stop-part.source))
		if fromRune > 0 {
			out = append(out, splitDisplayPart(part, 0, fromRune))
		}
		if toRune > fromRune {
			marked := splitDisplayPart(part, fromRune, toRune)
			marked.marker = true
			out = append(out, marked)
		}
		if toRune < count {
			out = append(out, splitDisplayPart(part, toRune, count))
		}
	}
	return out
}

func sourceRuneAt(raw string, runes, at int) int {
	n := 0
	for i := range runes {
		if sourceAdvance(raw, runes, i, runes) >= at {
			return i
		}
		n = i + 1
	}
	return n
}

func (v *view) plainDisplayParts(p int) []displayPart {
	line := []rune(v.lineOf(p))
	at := v.shown.paraStart(p)
	var out []displayPart
	for _, run := range v.shown.paragraph(p).Runs {
		n := utf8.RuneCountInString(run.Text)
		text := string(line[min(at-v.shown.paraStart(p), len(line)):min(at-v.shown.paraStart(p)+n, len(line))])
		out = append(out, displayPart{text: text, src: text, attrs: run.Attributes, doc: at, docEnd: at + n, source: at, srcEnd: at + n})
		at += n
	}
	return out
}

func (v *view) docDisplayParts(p int, active []box, text, raw string, attrs Attributes, doc, source int) []displayPart {
	var literals []box
	for _, b := range active {
		if b.literal {
			literals = append(literals, b)
		}
	}
	// A retained empty pair is marker bytes whichever way the selection
	// lies; while no caret reveals it, it draws nothing of its own, not the
	// parser's literal reading of `****` (record 2026-11-10, seed7/c15).
	empty := v.empty
	emptyHere := empty != nil && empty.para == p && empty.box.literal && empty.box.kind.keepsEmptyMarkers()
	emptyActive := emptyHere && containsBox(active, empty.box)
	if emptyHere && !emptyActive {
		literals = append(literals, empty.box)
	}
	if len(literals) == 0 {
		return []displayPart{{
			text: text, src: raw, attrs: attrs,
			doc: doc, docEnd: doc + utf8.RuneCountInString(text),
			source: source, srcEnd: source + len(raw),
		}}
	}
	runes := []rune(text)
	srcRunes := utf8.RuneCountInString(raw)
	// boundary is the source offset of the display rune boundary i.
	boundary := func(i int) int {
		if i <= 0 {
			return 0
		}
		return sourceAdvance(raw, srcRunes, i-1, len(runes))
	}
	var out []displayPart
	start := 0
	marked := v.sourceInMarkers(source, literals)
	for i := 1; i <= len(runes); i++ {
		next := boundary(i)
		nextMarked := i < len(runes) && v.sourceInMarkers(source+next, literals)
		if i == len(runes) || nextMarked != marked {
			from := boundary(start)
			part := displayPart{
				text: string(runes[start:i]), src: raw[from:next], attrs: attrs,
				doc: doc + start, docEnd: doc + i,
				source: source + from, srcEnd: source + next, marker: marked,
			}
			if marked && emptyHere && !emptyActive &&
				part.source >= empty.box.start && part.srcEnd <= empty.box.end {
				// Not revealed: the pair has no visible glyphs.
			} else {
				out = append(out, part)
			}
			start, marked = i, nextMarked
		}
	}
	return out
}

func (v *view) sourceInMarkers(source int, boxes []box) bool {
	for _, b := range boxes {
		if source >= b.openStart && source < b.openEnd ||
			source >= b.closeStart && source < b.closeEnd {
			return true
		}
	}
	return false
}

func (v *view) attributesAt(p, local int) Attributes {
	return v.para(p).attributesAt(local)
}

func insertDisplayPart(parts []displayPart, at int, insert displayPart) []displayPart {
	n := 0
	for i, part := range parts {
		count := utf8.RuneCountInString(part.text)
		if at <= n+count {
			cut := at - n
			docSplit := part.doc + (part.docEnd-part.doc)*max(0, cut)/max(1, count)
			runes := []rune(part.text)
			var out []displayPart
			out = append(out, parts[:i]...)
			if cut > 0 {
				left := part
				left.text = string(runes[:cut])
				left.src = rawPrefix(left.src, cut, count)
				left.docEnd = docSplit
				left.srcEnd = part.source + len(left.src)
				out = append(out, left)
			}
			out = append(out, insert)
			if cut < count {
				right := part
				right.text = string(runes[cut:])
				right.src = rawSuffix(part.src, cut, count)
				right.doc = docSplit
				right.source = part.source + len(part.src) - len(right.src)
				out = append(out, right)
			}
			out = append(out, parts[i+1:]...)
			return out
		}
		n += count
	}
	return append(parts, insert)
}

func rawPrefix(raw string, run, total int) string {
	if total <= 0 {
		return ""
	}
	return raw[:sourceAdvance(raw, utf8.RuneCountInString(raw), run-1, total)]
}

func rawSuffix(raw string, run, total int) string {
	return raw[len(rawPrefix(raw, run, total)):]
}

// displayBoundary returns the byte offset in src in front of its rune i of
// the total runes it shows. A part stands for the bytes it draws one rune at
// a time (a verbatim `[]`, a link's destination), so a boundary inside one
// is a boundary between its runes, not only its two edges.
func displayBoundary(src string, srcRunes, i, total int) int {
	if i <= 0 || srcRunes <= 0 || total <= 0 {
		return 0
	}
	if i >= total {
		return len(src)
	}
	return sourceAdvance(src, srcRunes, i-1, total)
}
