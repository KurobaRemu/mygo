package richtext

import (
	"strings"

	"github.com/egoist/mygo/ui/markdown/parser"
)

// BoundaryAffinity says which side of a projected boundary a source position
// belongs to when one visible boundary represents more than one source offset.
type BoundaryAffinity uint8

const (
	BoundaryUpstream BoundaryAffinity = iota
	BoundaryDownstream
)

// SourcePoint is one endpoint of a source selection. Offset is a byte offset
// in the Markdown source, or a rune offset when the document did not come
// from Markdown.
type SourcePoint struct {
	Offset   int
	Affinity BoundaryAffinity
}

// SourceSelection is the document selection in source coordinates. Anchor is
// where the selection started and Caret is where it ends; their order retains
// the selection's direction.
type SourceSelection struct {
	Anchor SourcePoint
	Caret  SourcePoint
}

func sourcePoint(offset int, affinity BoundaryAffinity) SourcePoint {
	return SourcePoint{Offset: offset, Affinity: affinity}
}

func sourceCaret(offset int) SourceSelection {
	at := sourcePoint(offset, BoundaryDownstream)
	return SourceSelection{Anchor: at, Caret: at}
}

func (d *Document) sourceLenLocked() int {
	if d.md == nil {
		return d.text.Len()
	}
	return len(d.md.p.String())
}

func (d *Document) clampSourceSelectionLocked(sel SourceSelection) SourceSelection {
	n := d.sourceLenLocked()
	sel.Anchor.Offset = d.snapSetextUnderlineOffsetLocked(d.snapFenceMarkerOffsetLocked(d.snapBlockPrefixOffsetLocked(d.snapFinalEOLOffsetLocked(max(0, min(sel.Anchor.Offset, n))))))
	sel.Caret.Offset = d.snapSetextUnderlineOffsetLocked(d.snapFenceMarkerOffsetLocked(d.snapBlockPrefixOffsetLocked(d.snapFinalEOLOffsetLocked(max(0, min(sel.Caret.Offset, n))))))
	if sel.Anchor.Affinity > BoundaryDownstream {
		sel.Anchor.Affinity = BoundaryDownstream
	}
	if sel.Caret.Affinity > BoundaryDownstream {
		sel.Caret.Affinity = BoundaryDownstream
	}
	return sel
}

// snapFinalEOLOffsetLocked maps an offset at or inside the document's final
// line ending to the last block's content end. Those bytes are a terminator,
// not a paragraph: the caret there is the one an open document starts with,
// and input writes before them.
func (d *Document) snapFinalEOLOffsetLocked(off int) int {
	if d.md == nil || d.md.keepFinalEOLCaret {
		return off
	}
	src := d.md.p.String()
	if len(src) == 0 || (src[len(src)-1] != '\n' && src[len(src)-1] != '\r') {
		return off
	}
	start := len(src) - 1
	if src[start] == '\n' && start > 0 && src[start-1] == '\r' {
		start--
	}
	if d.md.m.Len() == 0 {
		return off
	}
	if off < start || off == start && d.sourceAtParagraph(d.md.m.Len()-1, 0) <= start {
		// At the final EOL's start the caret ends the last block, unless
		// that block is the empty paragraph after a trailing blank line,
		// written after it: the offset is on the blank line it shows.
		return off
	}
	p := d.md.m.Len() - 1
	return d.md.m.end(p)
}

// snapBlockPrefixOffsetLocked maps an offset inside a block prefix, such as
// `## ` or `  - `, to the content start on the same line. The rich text view
// hides the prefix, so it holds no caret position; a caret placed there in
// source mode lands at the content start, as in Typora.
func (d *Document) snapBlockPrefixOffsetLocked(off int) int {
	if d.md == nil {
		return off
	}
	src := d.md.p.String()
	if len(d.md.session.splitMarkers) > 0 && off >= 0 && off <= len(src) {
		line := lineAt(src, off)
		if m, ok := d.splitMarkerAtLocked(line.start); ok && off >= m.lineStart && off <= m.innerEnd {
			// The moved marker of a held split is the live empty sibling's
			// visible prefix, not the nested item's own marker: a caret on the
			// line belongs before it, where the sibling's content starts
			// (record 2026-11-11, probe60/c6-enter-split-marker-typed).
			return m.outerEnd
		}
	}
	m := d.md.m
	p := m.from(off)
	if p >= m.Len() || m.start(p) == off {
		return off
	}
	if off > m.start(p) {
		// The offset already stands past the paragraph's start: the text it
		// holds is on the prefix's own line, as a blank document's one
		// whitespace line is a content paragraph's own bytes rather than a
		// prefix. Slicing back from it would read backwards (record 2026-11-13,
		// NewMarkdownDocument(" \n\n")).
		return m.start(p)
	}
	if strings.ContainsAny(src[off:m.start(p)], "\r\n") {
		return off
	}
	return m.start(p)
}

// snapSetextUnderlineOffsetLocked maps an offset on a setext heading's
// underline to the end of the heading's content line. The rich text view
// hides the underline; Typora holds no caret on it, and a caret placed there
// in source mode types at the content's end, `alpha\n=====` at 6 being the
// caret after `alpha`.
func (d *Document) snapSetextUnderlineOffsetLocked(off int) int {
	if d.md == nil || off <= 0 || d.md.keepFinalEOLCaret {
		// A transient preview holds its own caret offsets: its bytes can
		// parse differently from the document the edit commits.
		return off
	}
	p := d.md.p
	src := p.String()
	if off >= len(src) {
		// The document's end is snapFinalEOLOffsetLocked's: a caret there
		// is the one an open document starts at, and input writes before
		// the terminator rather than inside a heading's underline.
		return off
	}
	line := lineAt(src, off)
	if off < line.start || off > line.end {
		return off
	}
	for i := 1; i < p.Block(0).Next; i++ {
		b := p.Block(i)
		if b.Kind != parser.Heading || !b.Setext || b.Parent != 0 || b.End <= b.Start || b.End > len(src) {
			// Only a top-level heading is recorded: inside a quote Typora's
			// live edit keeps a `-` line a list item, so its parse and the
			// caret on that line stay unverified.
			continue
		}
		// The underline follows the block's content line; the parser ends
		// the heading at the content, so step over the line ending to find
		// the line that makes it one.
		underline := b.End
		switch {
		case underline < len(src) && src[underline] == '\r':
			underline++
			if underline < len(src) && src[underline] == '\n' {
				underline++
			}
		case underline < len(src) && src[underline] == '\n':
			underline++
		}
		if underline != line.start {
			continue
		}
		return b.End
	}
	return off
}

func (d *Document) sourceSelectionForDocLocked(anchor, caret int) SourceSelection {
	if d.md == nil {
		return SourceSelection{
			Anchor: sourcePoint(anchor, boundaryAffinity(anchor, caret, true)),
			Caret:  sourcePoint(caret, boundaryAffinity(anchor, caret, false)),
		}
	}
	v := d.mappingViewLocked()
	if anchor == caret {
		source := v.sourceForCaret(anchor)
		// A held split's empty sibling starts before the marker Enter moved
		// into its line: the projection finds the nested item's content start
		// instead, which is not where the live caret or typing belongs
		// (record 2026-11-11, probe60/c6-enter-split-marker-typed).
		if p := d.text.paraAt(anchor); d.text.paraStart(p) == anchor && p < d.md.m.Len() {
			if m, ok := d.heldSplitMarkerAtParagraphLocked(p); ok {
				source = m.outerEnd
			}
		}
		return SourceSelection{
			Anchor: sourcePoint(source, BoundaryDownstream),
			Caret:  sourcePoint(source, BoundaryDownstream),
		}
	}
	lo, hi := min(anchor, caret), max(anchor, caret)
	sourceLo, sourceHi := v.sourceForDoc(lo, false), v.sourceForDoc(hi, true)
	if anchor < caret {
		return SourceSelection{
			Anchor: sourcePoint(sourceLo, BoundaryUpstream),
			Caret:  sourcePoint(sourceHi, BoundaryDownstream),
		}
	}
	return SourceSelection{
		Anchor: sourcePoint(sourceHi, BoundaryDownstream),
		Caret:  sourcePoint(sourceLo, BoundaryUpstream),
	}
}

func (d *Document) docSelectionForSourceLocked(sel SourceSelection) (anchor, caret int) {
	if d.md == nil {
		return sel.Anchor.Offset, sel.Caret.Offset
	}
	v := d.mappingViewLocked()
	return v.docForSource(sel.Anchor.Offset, sel.Anchor.Affinity == BoundaryDownstream),
		v.docForSource(sel.Caret.Offset, sel.Caret.Affinity == BoundaryDownstream)
}

func boundaryAffinity(anchor, caret int, endpoint bool) BoundaryAffinity {
	if anchor == caret {
		return BoundaryDownstream
	}
	atStart := endpoint == (anchor < caret)
	if atStart {
		return BoundaryUpstream
	}
	return BoundaryDownstream
}

func (d *Document) mapSourceSelection(at, oldEnd, newEnd int) {
	d.selection.Anchor = mapSourcePoint(d.selection.Anchor, at, oldEnd, newEnd)
	d.selection.Caret = mapSourcePoint(d.selection.Caret, at, oldEnd, newEnd)
	if d.md != nil {
		session := d.md.session
		session.liveClosers = mapRanges(append([]liveCloser(nil), session.liveClosers...), at, oldEnd, newEnd-oldEnd)
		session.heldBreaks = mapPoints(append([]int(nil), session.heldBreaks...), at, oldEnd, newEnd-oldEnd)
		d.md.session = session
	}
}

func (d *Document) mapReplayLiveClosers(at, oldEnd, newEnd int) {
	if d.md == nil {
		return
	}
	session := d.md.session
	session.liveClosers = mapRanges(append([]liveCloser(nil), session.liveClosers...), at, oldEnd, newEnd-oldEnd)
	d.md.session = session
}

// heldBreakAt 报告 at 处的字节是否为本会话为持有空行写的反斜杠
// 硬换行。Typora 没有这个字节，视图不显示它，光标停在持有行上时
// 也不显示。
func (d *Document) heldBreakAt(at int) bool {
	if d == nil {
		return false
	}
	d.lock()
	defer d.mu.Unlock()
	return d.md != nil && d.md.session.heldBreakAt(at)
}

// addLiveCloser records that the closing sequence at from now reaches to,
// as the session typed it; the longest sequence at a start supersedes the
// shorter it grew from.
func (d *Document) addLiveCloser(from, to int) {
	d.setState(d.state().addLiveCloser(from, to))
}

// liveCloserEndAt returns the end of the closing sequence this session
// typed that starts at from and still ends its line, as Typora keeps it on
// screen (record 2026-11-09, diff.json bdbf85b7).
func (d *Document) liveCloserEndAt(from int) (int, bool) {
	if d.md == nil {
		return 0, false
	}
	d.lock()
	defer d.mu.Unlock()
	to, ok := d.state().liveCloserEndAt(from)
	if !ok {
		return 0, false
	}
	src := d.md.p.String()
	if to < from || to > len(src) {
		return 0, false
	}
	if to < len(src) && src[to] != '\n' && src[to] != '\r' {
		return 0, false
	}
	return to, true
}

func mapSourcePoint(p SourcePoint, at, oldEnd, newEnd int) SourcePoint {
	inserted := newEnd - at
	removed := oldEnd - at
	upper := p.Affinity == BoundaryUpstream
	p.Offset = mapPosition(p.Offset, at, removed, inserted, upper)
	return p
}
