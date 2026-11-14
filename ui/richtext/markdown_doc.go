package richtext

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui/markdown/parser"
)

var (
	ErrStructuralEdit = errors.New("richtext: structural Markdown edit")
	ErrUnwritable     = errors.New("richtext: edit cannot be written to the Markdown source")
)

type markdownSource struct {
	p    *parser.Document
	m    SourceMap
	last [3]int
	ok   bool
	eol  bool
	// keepFinalEOLCaret lets a transient preview hold its caret after the
	// final line ending: the deferred paragraph has no other source offset.
	keepFinalEOLCaret bool
	// preview 标记暂态块的虚拟文档：它持有的块可能只存在于视图里，提交
	// 其中一个块的编辑写的是 Typora 的运行时字节，不是保存文件的字节。
	preview bool
	session sessionState
}

type sourcePatch struct {
	at       int
	old, new string
}

func NewMarkdownDocument(src string) *Document {
	var state sessionState
	if line, at := initialEmptyHeadingPrefix(src); line >= 0 {
		state.literals = append(state.literals, parser.LiteralLine{Line: line, Content: at})
	}
	// The file says nothing about who typed its line breaks: a document
	// opened from it starts with none of the session's breaks.
	return newMarkdownDocument(src, state)
}

// newMarkdownDocument builds the document with the given lines parsed as
// literal text, as a live block prefix leaves the source it typed.
func newMarkdownDocument(src string, state sessionState) *Document {
	return newExtMarkdownDocument(src, state, parser.Typora)
}

// newExtMarkdownDocument builds the document as the given extensions read
// it. A transient preview passes previewExtensions.
func newExtMarkdownDocument(src string, state sessionState, ext parser.Extensions) *Document {
	p := parser.New(ext)
	state = state.clone()
	state.syncToParser(p)
	p.Set(src)
	t, m := projectSource(p)
	d := NewDocument(t)
	d.md = &markdownSource{
		p: p, m: m, session: state,
		eol: strings.HasSuffix(src, "\n") || strings.HasSuffix(src, "\r"),
	}
	d.md.session = d.md.session.recordOpenedLists(p)
	if n := t.ParagraphCount(); n > 0 {
		at := d.sourceAtParagraph(n-1, 0)
		if t.Len() > 0 {
			at = d.mappingViewLocked().paragraphEndSource(n - 1)
		}
		d.assignSourceSelection(sourceCaret(at), false)
	}
	return d
}

// initialEmptyHeadingPrefix treats the final empty ATX prefix as the
// literal paragraph Typora shows on open. In-session headings that become
// empty stay headings because their block identity is already established.
func initialEmptyHeadingPrefix(src string) (lineStart, contentStart int) {
	at := len(src)
	for at > 0 && (src[at-1] == '\n' || src[at-1] == '\r') {
		at--
	}
	line := liveLineAt(src, at)
	if emptyHeadingPrefix(src[line.start:line.end]) {
		return line.start, line.start
	}
	return -1, -1
}

func (d *Document) Source() string {
	d.lock()
	defer d.mu.Unlock()
	if d.md == nil {
		return d.text.Markdown()
	}
	return d.md.p.String()
}

func (d *Document) SourceMap() SourceMap {
	d.lock()
	defer d.mu.Unlock()
	if d.md == nil {
		return SourceMap{}
	}
	return d.md.m
}

// ReparseSource parses src as the session reads it: the lines a live block
// prefix holds literal stay literal, so the reparse is the view's parse of
// the bytes, not the cold parse the file would get on open. src is the
// document's current source; the literal lines index it.
func (d *Document) ReparseSource(src string) (Text, SourceMap) {
	d.lock()
	var state sessionState
	if d.md != nil {
		state = d.state()
	}
	d.mu.Unlock()
	p := newMarkdownParser()
	state.syncToParser(p)
	p.Set(src)
	return projectSource(p)
}

// reproject refreshes the document's text from its source as the session
// reads it, after a session fact changed how the bytes read. The source and
// the document's place in it stay as they are.
func (d *Document) reproject() {
	d.reprojectStep(nil)
}

// reprojectStep is reproject as part of an undo step, which takes the text
// the new projection writes.
func (d *Document) reprojectStep(step *undoStep) {
	if d.md == nil {
		return
	}
	t, m := projectSource(d.md.p)
	x, removed, inserted := diffText(d.text, t)
	d.md.m = m
	if step != nil {
		// The step's patches already ran; this projection is the map its
		// source reads to after them, as redo restores it.
		step.mapAfter = m
	}
	d.apply(step, x, removed, inserted, t)
}

// reparseLiteralLines parses the source again with lines as the lines held
// literal. Undo and Redo restore a set of literal lines the bytes do not
// spell, so the parser has to read the source under it.
func (d *Document) reparseLiteralLines(state sessionState) {
	if d.md == nil {
		return
	}
	state = state.restoreProjection(d.state())
	p := parser.New(d.md.p.Extensions())
	state.syncToParser(p)
	p.Set(d.md.p.String())
	d.md.p = p
	d.md.session = state.clone()
	d.reproject()
}

func (d *Document) LastSourcePatch() (start, oldEnd, newEnd int, ok bool) {
	d.lock()
	defer d.mu.Unlock()
	if d.md == nil || !d.md.ok {
		return 0, 0, 0, false
	}
	return d.md.last[0], d.md.last[1], d.md.last[2], true
}

// openedListAt reports whether an ordered list began at start in the opened
// document.
func (md *markdownSource) openedListAt(start int) bool {
	return md.session.openedListAt(start)
}

// openedListStartAt reports whether a list began at start in the opened
// document.
func (md *markdownSource) openedListStartAt(start int) bool {
	return md.session.openedListStartAt(start)
}

// edit changes the source and keeps the opened lists' offsets on their lists.
func (md *markdownSource) edit(at, oldEnd int, text string) {
	md.p.Edit(at, oldEnd, text)
	md.session = md.session.pullFromParser(md.p)
	md.session.openedLists = mapOpenedLists(md.session.openedLists, at, oldEnd, len(text))
}

func (d *Document) run(step *undoStep, fn func(e *Edit)) {
	if d.md == nil {
		fn(&Edit{d: d, step: step})
		return
	}
	saved, text, m, version := *step, d.text, d.md.m, d.version
	log := append([]change(nil), d.log...)
	selection, typing := d.selection, d.typing
	held := append([]int(nil), d.md.session.heldBreaks...)
	lives := append([]int(nil), d.md.p.LiveLines()...)
	d.md.ok = false
	e := &Edit{d: d, step: step}
	fn(e)
	if e.err == nil {
		// 本会话为持有空行写的反斜杠只在它下面还有内容时成立：块级
		// 起始、空行或删光内容都会让它变成独立段落里的字面反斜杠。
		// 编辑完成后重解析一遍，失效的就在同一个撤销步里删掉。
		d.sweepHeldBreaks(e)
		step.sessionAfter = d.syncFromParser()
		// A caret inside the range a patch removes maps to the patch's
		// start, which can be a marker the view hides, as the prefix of the
		// line a deleted empty item joins. The view holds no caret there:
		// the mapped selection lands where every source selection lands, at
		// the content start of the paragraph under it.
		d.selection = d.clampSourceSelectionLocked(d.selection)
		return
	}
	d.md.session.heldBreaks = held
	d.md.p.SetLiveLines(lives)
	if d.version == version {
		return
	}
	for k := len(step.patches) - 1; k >= len(saved.patches); k-- {
		c := step.patches[k]
		d.md.edit(c.at, c.at+len(c.new), c.old)
	}
	*step = saved
	d.text, d.md.m, d.md.ok = text, m, false
	d.version = version
	d.log = log
	d.assignSourceSelection(selection, false)
	d.typing = typing
}

func (e *Edit) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

func (e *Edit) markdownReplace(start, end int, text Text) {
	if text.ParagraphCount() > 1 || e.d.text.paraAt(start) != e.d.text.paraAt(end) {
		if err := e.pasteMarkdown(start, end, text.Markdown()); err != nil {
			e.fail(err)
		}
		return
	}
	var s strings.Builder
	a := typingAttributes(e.d.text, start, end)
	first := true
	for _, r := range text.Runs(0, text.Len()) {
		if first {
			a, first = r.Attributes, false
		} else if r.Attributes != a {
			if err := e.pasteMarkdown(start, end, text.Markdown()); err != nil {
				e.fail(err)
			}
			return
		}
		s.WriteString(r.Text)
	}
	e.markdownString(start, end, s.String(), a)
}

func (e *Edit) markdownString(start, end int, s string, a Attributes) {
	if e.err != nil || start == end && s == "" {
		return
	}
	if start != end && s == "" && e.d.text.paraAt(start) != e.d.text.paraAt(end) {
		if err := e.deleteCrossBlock(start, end, ""); err != nil {
			e.fail(err)
		}
		return
	}
	if start != end && s != "" && e.d.text.paraAt(start) != e.d.text.paraAt(end) {
		if err := e.deleteCrossBlock(start, end, formattedText(s, a)); err != nil {
			e.fail(err)
		}
		return
	}
	if strings.ContainsAny(s, "\n\r") {
		if s == "\n" {
			if start != end {
				if err := e.deleteCrossBlock(start, end, ""); err != nil {
					e.fail(err)
					return
				}
				start = min(start, e.d.text.Len())
				end = start
			}
			if err := e.enterBlock(start, false); err != nil {
				e.fail(err)
			}
			return
		}
		s = normalizeNewlines(s)
		if e.d.text.paraAt(start) != e.d.text.paraAt(end) {
			if err := e.deleteCrossBlock(start, end, s); err != nil {
				e.fail(err)
			}
			return
		}
		lines := strings.Split(s, "\n")
		for i := range lines {
			lines[i] = formattedText(escapeText(lines[i]), a)
		}
		if err := e.pasteMarkdown(start, end, strings.Join(lines, "\n\n")); err != nil {
			e.fail(err)
		}
		return
	}
	if a.Attachment != nil || strings.ContainsRune(s, ObjectReplacement) {
		e.fail(ErrStructuralEdit)
		return
	}
	if err := e.d.writeText(e.step, start, end, s, a); err != nil {
		if err != ErrUnwritable || s == "" || !e.canonicalizeInsertion(start, end, s, a) {
			e.fail(err)
		}
	}
}

// canonicalizeInsertion writes an insertion whose attributes no minimal
// source patch can carry, such as plain text typed inside a strong: the
// block's content is written again with the markers split around it.
func (e *Edit) canonicalizeInsertion(start, end int, s string, a Attributes) bool {
	pa, pz := e.d.text.paragraphSpan(start, end)
	if pa != pz {
		return false
	}
	want := e.d.text.ReplaceString(start, end, s, a)
	return e.canonicalizeInlineParagraphs(start, end, want, pa, pz) == nil
}

func (e *Edit) ReplaceSource(srcFrom, srcTo int, newBytes string) {
	if e.err != nil || e.d.md == nil {
		if e.d.md == nil {
			e.fail(ErrStructuralEdit)
		}
		return
	}
	md := e.d.md
	src := md.p.String()
	if srcFrom < 0 || srcTo < srcFrom || srcTo > len(src) || srcFrom == srcTo && newBytes == "" {
		if srcFrom < 0 || srcTo < srcFrom || srcTo > len(src) {
			e.fail(ErrStructuralEdit)
		}
		return
	}
	old := src[srcFrom:srcTo]
	if old == newBytes {
		return
	}
	// Deleting the session's own line break at the document's start takes
	// away the empty first line a paragraph begins on. The paragraph's
	// leading break is a segment the incremental projection keeps, and its
	// text would lose the first rune the line now holds (Shift+Enter at a
	// paragraph's start, then Backspace: `,\n`-style bytes keep the text
	// and the source apart). A reparse reads the paragraph as the line it
	// is.
	leadingLiveBreak := srcFrom == 0 && newBytes == "" && md.session.pullFromParser(md.p).isLiveLine(srcFrom) &&
		(old == "\n" || old == "\r\n")
	relinks := md.p.Relinks()
	md.edit(srcFrom, srcTo, newBytes)
	t, m, _, _, ok := md.project(e.d.text, len(newBytes)-(srcTo-srcFrom), relinks)
	if ok && (typingOnBlankSourceLine(src, srcFrom, old) || leadingLiveBreak) {
		t, m = projectSource(md.p)
	}
	if !ok {
		md.edit(srcFrom, srcFrom+len(newBytes), old)
		e.fail(ErrStructuralEdit)
		return
	}
	if e.step != nil {
		if len(e.step.patches) == 0 {
			e.step.mapBefore = md.m
		}
		e.step.patches = append(e.step.patches, sourcePatch{at: srcFrom, old: old, new: newBytes})
		e.step.mapAfter = m
	}
	md.m = m
	md.last, md.ok = [3]int{srcFrom, srcTo, srcFrom + len(newBytes)}, true
	e.d.mapSourceSelection(srcFrom, srcTo, srcFrom+len(newBytes))
	start, removed, inserted := diffText(e.d.text, t)
	e.d.apply(e.step, start, removed, inserted, t)
}

func (d *Document) ReplaceSource(srcFrom, srcTo int, newBytes string) error {
	d.lock()
	defer d.mu.Unlock()
	var err error
	d.edit(false, func(e *Edit) {
		e.ReplaceSource(srcFrom, srcTo, newBytes)
		err = e.err
	})
	return err
}

func (d *Document) writeText(step *undoStep, start, end int, s string, a Attributes) error {
	t := d.text
	pa := t.paraAt(start)
	pz := t.paraAt(end)
	if pz != pa {
		if s != "" {
			return ErrStructuralEdit
		}
		from := d.md.m.start(pa) + start - t.paraStart(pa)
		to := d.md.m.start(pz) + end - t.paraStart(pz)
		for i := pa; i <= pz; i++ {
			pm := seqAt(d.md.m.paras, i)
			if pm.hasContentBox() {
				return ErrStructuralEdit
			}
			for _, s := range pm.segs {
				if s.Kind != SegmentVerbatim {
					return ErrStructuralEdit
				}
			}
			if i < pz {
				between := d.md.p.String()[d.md.m.end(i):d.md.m.start(i+1)]
				if !lineBreakGap(between) {
					return ErrStructuralEdit
				}
			}
		}
		src := d.md.p.String()
		if from < 0 || from > to || to > len(src) {
			return ErrStructuralEdit
		}
		want := t.ReplaceString(start, end, s, a)
		c := sourcePatch{at: from, old: src[from:to]}
		if d.tryPatch(step, c, want, pa, start, end, 0, false) {
			return nil
		}
		return ErrStructuralEdit
	}
	ps := t.paraStart(pa)
	m := d.md.m
	pm := seqAt(m.paras, pa)
	base := m.start(pa)
	if t.Len() == 0 && pm.src == 0 && !pm.codeEmpty && !t.ParagraphStyleAt(0).CodeBlock {
		base = 0
	}
	if start == end && start == ps && pm.src == 0 {
		if sm, ok := d.heldSplitMarkerAtParagraphLocked(pa); ok {
			// A held split's live empty sibling starts before the marker Enter
			// moved into its line: typing goes there, left of that marker,
			// while the reparse reads the line as a nested item whose content
			// starts after it (record 2026-11-11,
			// probe60/c6-enter-split-marker-typed).
			base = sm.outerEnd
		}
	}
	body := d.md.p.String()[base : base+pm.src]
	texts := []string{s}
	if e := escapeText(s); e != s {
		texts = append(texts, e)
	}
	if f := formattedText(s, a); f != s {
		same := false
		for _, text := range texts {
			same = same || text == f
		}
		if !same {
			texts = append(texts, f)
		}
	}
	var cands []sourcePatch
	if start < end {
		from, to, kept, joins, ok := pm.deletion(body, start-ps, end-ps)
		if !ok {
			return ErrStructuralEdit
		}
		if s == "" {
			cands = append(cands, sourcePatch{base + from, body[from:to], kept})
		}
		for _, text := range texts {
			if s == "" {
				break
			}
			if pm.codeEmpty && !pm.codeOpen {
				text += lineEndingBefore(d.md.p.String(), base)
			}
			for _, j := range joins {
				cands = append(cands, sourcePatch{base + from, body[from:to], kept[:j] + text + kept[j:]})
			}
		}
	} else {
		for _, text := range texts {
			if pm.codeEmpty && !pm.codeOpen {
				text += lineEndingBefore(d.md.p.String(), base)
			}
			for _, at := range pm.insertions(body, start-ps) {
				cands = append(cands, sourcePatch{base + at, "", text})
			}
		}
	}
	if len(cands) == 0 {
		return ErrStructuralEdit
	}
	want := t.ReplaceString(start, end, s, a)
	for _, c := range cands {
		if c.new == s && a == (Attributes{}) &&
			(liveMarkdownTyping(s) || typingOnBlankSourceLine(d.md.p.String(), c.at, c.old)) &&
			d.tryDirtyPatch(step, c.at, c.at+len(c.old), c.new) {
			return nil
		}
		if d.tryPatch(step, c, want, pa, start, end, utf8.RuneCountInString(s), s == "") {
			return nil
		}
	}
	if a != (Attributes{}) {
		want = t.ReplaceString(start, end, s, Attributes{})
		for _, c := range cands {
			if d.tryPatch(step, c, want, pa, start, end, utf8.RuneCountInString(s), s == "") {
				return nil
			}
		}
	}
	return ErrUnwritable
}

func typingOnBlankSourceLine(src string, at int, old string) bool {
	if old != "" || at < 0 || at > len(src) {
		return false
	}
	line := liveLineAt(src, at)
	return strings.Trim(src[line.start:line.end], " \t") == ""
}

func liveMarkdownTyping(s string) bool {
	if s == " " {
		return true
	}
	if s == "()" || s == "[]" || s == "{}" {
		return true
	}
	if utf8.RuneCountInString(s) != 1 {
		return false
	}
	return strings.ContainsAny(s, "*_`~[]()#-+>")
}

func (d *Document) tryDirtyPatch(step *undoStep, at, oldEnd int, text string) bool {
	if d.md == nil {
		return false
	}
	md := d.md
	src := md.p.String()
	if at < 0 || oldEnd < at || oldEnd > len(src) {
		return false
	}
	oldText := d.text
	old := src[at:oldEnd]
	nextSrc := src[:at] + text + src[oldEnd:]
	p := parser.New(d.md.p.Extensions())
	state := md.session.pullFromParser(md.p)
	p.SetLiteralLines(mapLiteralLines(state.literals, at, oldEnd, src, nextSrc))
	p.SetLiveLines(parser.ShiftLiveLines(state.liveBreaks, at, oldEnd, len(text)-(oldEnd-at)))
	state = state.pullFromParser(p)
	state.openedLists = mapOpenedLists(state.openedLists, at, oldEnd, len(text))
	md.session = state
	p.Set(nextSrc)
	t, m := projectSource(p)
	md.p = p
	if step != nil {
		if len(step.patches) == 0 {
			step.mapBefore = md.m
		}
		step.patches = append(step.patches, sourcePatch{at: at, old: old, new: text})
		step.mapAfter = m
	}
	md.m = m
	md.last, md.ok = [3]int{at, oldEnd, at + len(text)}, true
	d.mapSourceSelection(at, oldEnd, at+len(text))
	x, removed, inserted := diffText(oldText, t)
	d.apply(step, x, removed, inserted, t)
	return true
}

func lineBreakGap(gap string) bool {
	i := 0
	for i < len(gap) && (gap[i] == ' ' || gap[i] == '\t') {
		i++
	}
	switch {
	case i < len(gap) && gap[i] == '\\':
		i++
		if i >= len(gap) || gap[i] != '\n' && gap[i] != '\r' {
			return false
		}
	case i+3 <= len(gap) && strings.EqualFold(gap[i:i+3], "<br"):
		i += 3
		for i < len(gap) && (gap[i] == ' ' || gap[i] == '\t') {
			i++
		}
		if i < len(gap) && gap[i] == '/' {
			i++
			for i < len(gap) && (gap[i] == ' ' || gap[i] == '\t') {
				i++
			}
		}
		if i >= len(gap) || gap[i] != '>' {
			return false
		}
		i++
	case i < len(gap) && (gap[i] == '\n' || gap[i] == '\r'):
	default:
		return false
	}
	if i < len(gap) && gap[i] == '\r' {
		i++
		if i < len(gap) && gap[i] == '\n' {
			i++
		}
	} else if i < len(gap) && gap[i] == '\n' {
		i++
	}
	for i < len(gap) {
		for i < len(gap) && (gap[i] == ' ' || gap[i] == '\t') {
			i++
		}
		if i >= len(gap) {
			return true
		}
		if gap[i] != '>' {
			return false
		}
		i++
		if i < len(gap) && (gap[i] == ' ' || gap[i] == '\t') {
			i++
		}
	}
	return true
}

func formattedText(s string, a Attributes) string {
	if a == (Attributes{}) || a.Attachment != nil || strings.ContainsAny(s, "\r\n") {
		return s
	}
	out, _ := renderRuns([]Run{{Text: s, Attributes: a}}, nil)
	return out
}

// formattedInsert returns the source bytes for typing s with attributes a,
// and where the caret goes in them: after the text, inside the markers
// closing it.
func formattedInsert(s string, a Attributes) (string, int) {
	out, delims := renderRuns([]Run{{Text: s, Attributes: a}}, nil)
	end := len(out)
	for _, d := range delims {
		end = min(end, d.close)
	}
	if a.Underline {
		if i := strings.LastIndex(out[:end], "</u>"); i >= 0 {
			end = i
		}
	}
	if a.Link != "" {
		if i := strings.LastIndex(out[:end], "]("); i >= 0 {
			end = i
		}
	}
	// A code span's caret goes before the fence closing it.
	for end > 0 && out[end-1] == '`' {
		end--
	}
	return out, end
}

func lineEndingBefore(src string, at int) string {
	if at > 0 && src[at-1] == '\n' {
		if at > 1 && src[at-2] == '\r' {
			return "\r\n"
		}
		return "\n"
	}
	if at > 0 && src[at-1] == '\r' {
		return "\r"
	}
	return "\n"
}

func escapeText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if escapable(s[i]) {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (d *Document) tryPatch(step *undoStep, c sourcePatch, want Text, pa, start, end, inserted int, trim bool) bool {
	md := d.md
	relinks := md.p.Relinks()
	md.edit(c.at, c.at+len(c.old), c.new)
	t, m, a, z, ok := md.project(d.text, len(c.new)-len(c.old), relinks)
	ok = ok && t.ParagraphCount() == want.ParagraphCount() && a <= pa && pa < z
	trimmed := false
	for i := a; ok && i < z; i++ {
		if sameParagraph(t.paragraph(i), want.paragraph(i), !seqAt(m.paras, i).auto) {
			continue
		}
		if trim && i == pa && sameParagraph(trimmedParagraph(t.paragraph(i)), trimmedParagraph(want.paragraph(i)), !seqAt(m.paras, i).auto) {
			trimmed = true
			continue
		}
		ok = false
	}
	if !ok {
		md.edit(c.at, c.at+len(c.new), c.old)
		return false
	}
	if step != nil {
		if len(step.patches) == 0 {
			step.mapBefore = md.m
		}
		step.patches = append(step.patches, c)
		step.mapAfter = m
	}
	md.m = m
	md.last, md.ok = [3]int{c.at, c.at + len(c.old), c.at + len(c.new)}, true
	d.mapSourceSelection(c.at, c.at+len(c.old), c.at+len(c.new))
	if trimmed {
		at, removed, added := diffText(d.text, t)
		d.apply(step, at, removed, added, t)
	} else {
		d.apply(step, start, end-start, inserted, t)
	}
	return true
}

func trimmedParagraph(p Paragraph) Paragraph {
	runs := append([]Run(nil), p.Runs...)
	for len(runs) > 0 {
		s := strings.TrimLeftFunc(runs[0].Text, unicode.IsSpace)
		runs[0].Text = s
		if s != "" {
			break
		}
		runs = runs[1:]
	}
	for len(runs) > 0 {
		s := strings.TrimRightFunc(runs[len(runs)-1].Text, unicode.IsSpace)
		runs[len(runs)-1].Text = s
		if s != "" {
			break
		}
		runs = runs[:len(runs)-1]
	}
	p.Runs = runs
	return p
}

func changedText(a, b string) (at, removed, inserted int) {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	for p > 0 && (p < len(a) && !utf8.RuneStart(a[p]) || p < len(b) && !utf8.RuneStart(b[p])) {
		p--
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	for s > 0 && (!utf8.RuneStart(a[len(a)-s]) || !utf8.RuneStart(b[len(b)-s])) {
		s--
	}
	at = utf8.RuneCountInString(a[:p])
	removed = utf8.RuneCountInString(a[p : len(a)-s])
	inserted = utf8.RuneCountInString(b[p : len(b)-s])
	return at, removed, inserted
}

func changedBytes(a, b string) (start, oldEnd, newEnd int) {
	start = 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	tail := 0
	for tail < len(a)-start && tail < len(b)-start && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	return start, len(a) - tail, len(b) - tail
}

func (md *markdownSource) project(t Text, delta int, relinks uint32) (Text, SourceMap, int, int, bool) {
	p := md.p
	if p.Relinks() != relinks {
		nt, nm := projectSource(p)
		return nt, nm, 0, nt.ParagraphCount(), true
	}
	if t.Len() == 0 {
		nt, nm := projectSource(p)
		return nt, nm, 0, nt.ParagraphCount(), true
	}
	at, from, to := p.Reparsed()
	m := md.m
	pa := m.from(from)
	if pa > 0 && from < m.end(pa-1) {
		// The reparse starts inside the paragraph before pa: that paragraph
		// begins with a leading break or continuation prefix whose source
		// starts before the block it belongs to, so the rebuilt block is its
		// content and the paragraph is the one to replace.
		pa--
	}
	pz := m.Len()
	if to < len(p.String()) {
		pz = m.from(to - delta)
	}
	paras, built := projectBlocks(p, at, to, pa == 0)
	if len(paras) == 0 && pa == 0 && pz == m.Len() {
		nt, nm := blankProjection(p)
		return nt, nm, 0, nt.ParagraphCount(), true
	}
	if pz < pa || pz > m.Len() || len(paras) == 0 {
		return t, m, 0, 0, false
	}
	return t.replaceParagraphs(pa, pz, TextOf(paras...)), m.splice(pa, pz, built, delta), pa, pa + len(paras), true
}

func (d *Document) replay(s undoStep, redo bool) {
	md := d.md
	if redo {
		for _, c := range s.patches {
			md.edit(c.at, c.at+len(c.old), c.new)
		}
		md.m = s.mapAfter
	} else {
		for k := len(s.patches) - 1; k >= 0; k-- {
			c := s.patches[k]
			md.edit(c.at, c.at+len(c.new), c.old)
		}
		md.m = s.mapBefore
	}
	md.ok = len(s.patches) > 0
	if !md.ok {
		return
	}
	var c change
	for k, p := range s.patches {
		x := change{start: p.at, oldEnd: p.at + len(p.old), newEnd: p.at + len(p.new)}
		if k == 0 {
			c = x
		} else {
			c = composeChanges(c, x)
		}
	}
	if redo {
		md.last = [3]int{c.start, c.oldEnd, c.newEnd}
	} else {
		md.last = [3]int{c.start, c.newEnd, c.oldEnd}
	}
}

func (d *Document) CheckSource() error {
	d.lock()
	defer d.mu.Unlock()
	return d.checkSource()
}

func (d *Document) CheckSourceParsed(t Text, m SourceMap) error {
	d.lock()
	defer d.mu.Unlock()
	return d.checkSourceParsed(t, m)
}

func (d *Document) checkSource() error {
	if d.md == nil {
		return nil
	}
	p := parser.New(d.md.p.Extensions())
	d.state().syncToParser(p)
	p.Set(d.md.p.String())
	t, m := projectSource(p)
	return d.checkSourceParsed(t, m)
}

func (d *Document) checkSourceParsed(t Text, m SourceMap) error {
	if d.md == nil {
		return nil
	}
	if t.ParagraphCount() != d.text.ParagraphCount() {
		return fmt.Errorf("projection has %d paragraphs, the source %d", d.text.ParagraphCount(), t.ParagraphCount())
	}
	for i := range t.ParagraphCount() {
		if !sameParagraph(d.text.paragraph(i), t.paragraph(i), true) {
			return fmt.Errorf("projection paragraph %d differs from the source", i)
		}
	}
	if m.Len() != d.md.m.Len() {
		return fmt.Errorf("source map has %d paragraphs, the source %d", d.md.m.Len(), m.Len())
	}
	for i := range m.Len() {
		if !sameParaMap(seqAt(m.paras, i), seqAt(d.md.m.paras, i)) {
			return fmt.Errorf("source map paragraph %d differs", i)
		}
	}
	return nil
}

func (d *Document) SaveMarkdown(path string) error {
	d.materializeActiveLangInput()
	d.materializeActiveTransient()
	src := d.Source()
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(src)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
