package richtext

import (
	"slices"
	"sort"
	"strings"

	"github.com/egoist/mygo/ui/markdown/parser"
)

// sessionState owns the byte-offset annotations that only the live editing
// session can know. The parser's literal lines and live breaks are a
// projection of this value; every handoff copies its slices so undo steps,
// transient previews and parser reparses never share mutable state.
type sessionState struct {
	literals     []parser.LiteralLine
	liveBreaks   []int
	heldBreaks   []int
	liveClosers  []liveCloser
	splitMarkers []splitMarker
	openedLists  []int
	openedNested []int
}

func (s sessionState) clone() sessionState {
	return sessionState{
		literals:     append([]parser.LiteralLine(nil), s.literals...),
		liveBreaks:   append([]int(nil), s.liveBreaks...),
		heldBreaks:   append([]int(nil), s.heldBreaks...),
		liveClosers:  append([]liveCloser(nil), s.liveClosers...),
		splitMarkers: append([]splitMarker(nil), s.splitMarkers...),
		openedLists:  append([]int(nil), s.openedLists...),
		openedNested: append([]int(nil), s.openedNested...),
	}
}

// pullFromParser reads the two tables the incremental parser shifted in place.
func (s sessionState) pullFromParser(p *parser.Document) sessionState {
	next := s.clone()
	next.literals = p.LiteralLines()
	next.liveBreaks = p.LiveLines()
	return next
}

// syncToParser writes the two tables the parser projects from this value.
func (s sessionState) syncToParser(p *parser.Document) {
	p.SetLiteralLines(append([]parser.LiteralLine(nil), s.literals...))
	p.SetLiveLines(append([]int(nil), s.liveBreaks...))
}

// syncFromParser makes the parser's incremental shifts authoritative for its
// projection and returns an independent snapshot of the resulting session.
func (d *Document) syncFromParser() sessionState {
	if d.md == nil {
		return sessionState{}
	}
	d.md.session = d.md.session.pullFromParser(d.md.p)
	return d.md.session.clone()
}

// state reads the current session as a value no caller can mutate.
func (d *Document) state() sessionState {
	if d.md == nil {
		return sessionState{}
	}
	return d.md.session.pullFromParser(d.md.p).clone()
}

// setState writes a session and its parser projection.
func (d *Document) setState(s sessionState) {
	if d.md == nil {
		return
	}
	d.md.session = s.clone()
	d.md.session.syncToParser(d.md.p)
}

// mapPatch maps every byte-offset table over a source replacement
// [at, oldEnd) -> newLen. src and nextSrc let a literal line at the replaced
// line start survive when the replacement writes that same line again, as
// Enter rewrites an empty ATX prefix (seed7/c9).
func (s sessionState) mapPatch(at, oldEnd, newLen int, src, nextSrc string) sessionState {
	next := s.clone()
	delta := newLen - (oldEnd - at)
	next.literals = mapLiteralLines(next.literals, at, oldEnd, src, nextSrc)
	next.liveBreaks = parser.ShiftLiveLines(next.liveBreaks, at, oldEnd, delta)
	next.heldBreaks = mapPoints(next.heldBreaks, at, oldEnd, delta)
	next.liveClosers = mapRanges(next.liveClosers, at, oldEnd, delta)
	next.splitMarkers = mapSplitMarkers(next.splitMarkers, at, oldEnd, delta)
	next.openedLists = mapOpenedLists(next.openedLists, at, oldEnd, newLen)
	next.openedNested = append([]int(nil), next.openedNested...)
	return next
}

func (s sessionState) setLiteralLine(lineStart, contentStart int) sessionState {
	next := s.clone()
	for i := range next.literals {
		if next.literals[i].Line == lineStart {
			next.literals[i].Content = contentStart
			return next
		}
	}
	next.literals = append(next.literals, parser.LiteralLine{Line: lineStart, Content: contentStart})
	slices.SortFunc(next.literals, func(a, b parser.LiteralLine) int { return a.Line - b.Line })
	return next
}

func (s sessionState) clearLiteralLineAt(lineStart int) sessionState {
	next := s.clone()
	for i := range next.literals {
		if next.literals[i].Line == lineStart {
			next.literals = slices.Delete(next.literals, i, i+1)
			return next
		}
	}
	return next
}

func (s sessionState) setLiveLine(breakAt int) sessionState {
	next := s.clone()
	i, ok := slices.BinarySearch(next.liveBreaks, breakAt)
	if ok {
		return next
	}
	next.liveBreaks = slices.Insert(next.liveBreaks, i, breakAt)
	return next
}

func (s sessionState) clearLiveLineAt(breakAt int) sessionState {
	next := s.clone()
	if i, ok := slices.BinarySearch(next.liveBreaks, breakAt); ok {
		next.liveBreaks = slices.Delete(next.liveBreaks, i, i+1)
	}
	return next
}

func (s sessionState) isLiveLine(breakAt int) bool {
	_, ok := slices.BinarySearch(s.liveBreaks, breakAt)
	return ok
}

func (s sessionState) heldBreakAt(at int) bool {
	return slices.Contains(s.heldBreaks, at)
}

func (s sessionState) addLiveCloser(from, to int) sessionState {
	if from >= to {
		return s
	}
	next := s.clone()
	for i, closer := range next.liveClosers {
		if closer.from == from {
			next.liveClosers[i] = liveCloser{from, to}
			return next
		}
	}
	next.liveClosers = append(next.liveClosers, liveCloser{from, to})
	return next
}

func (s sessionState) liveCloserEndAt(from int) (int, bool) {
	for _, closer := range s.liveClosers {
		if closer.from == from {
			return closer.to, true
		}
	}
	return 0, false
}

func (s sessionState) addSplitMarker(marker splitMarker) sessionState {
	next := s.clone()
	next.splitMarkers = append(next.splitMarkers, marker)
	return next
}

func (s sessionState) splitMarkerAt(lineStart int, src string) (splitMarker, bool) {
	for i := len(s.splitMarkers) - 1; i >= 0; i-- {
		marker := s.splitMarkers[i]
		if marker.lineStart != lineStart || marker.outerEnd <= marker.lineStart || marker.innerEnd > len(src) {
			continue
		}
		if src[marker.lineStart:marker.innerEnd] != marker.line {
			continue
		}
		return marker, true
	}
	return splitMarker{}, false
}

func (s sessionState) openedListAt(start int) bool {
	i := sort.SearchInts(s.openedLists, start)
	return i < len(s.openedLists) && s.openedLists[i] == start
}

func (s sessionState) openedListStartAt(start int) bool {
	i := sort.SearchInts(s.openedNested, start)
	return i < len(s.openedNested) && s.openedNested[i] == start
}

func (s sessionState) dropAfter(markerAt int) sessionState {
	next := s.clone()
	keepLiterals := next.literals[:0]
	for _, literal := range next.literals {
		if literal.Line < markerAt && literal.Content < markerAt {
			keepLiterals = append(keepLiterals, literal)
		}
	}
	next.literals = keepLiterals
	keepBreaks := next.liveBreaks[:0]
	for _, at := range next.liveBreaks {
		if at < markerAt {
			keepBreaks = append(keepBreaks, at)
		}
	}
	next.liveBreaks = keepBreaks
	return next
}

// preview keeps the parser projection and opened-document list offsets.
// A hypothetical edit does not inherit view holdings of the current bytes.
func (s sessionState) preview() sessionState {
	return sessionState{
		literals:     append([]parser.LiteralLine(nil), s.literals...),
		liveBreaks:   append([]int(nil), s.liveBreaks...),
		openedLists:  append([]int(nil), s.openedLists...),
		openedNested: append([]int(nil), s.openedNested...),
	}
}

// restoreProjection writes the tables baseline undo restores, preserving the
// current-byte holdings of the live document.
func (s sessionState) restoreProjection(target sessionState) sessionState {
	next := target.clone()
	next.literals = append([]parser.LiteralLine(nil), s.literals...)
	next.liveBreaks = append([]int(nil), s.liveBreaks...)
	next.heldBreaks = append([]int(nil), s.heldBreaks...)
	return next
}

// carryOpenedLists gives a preview the opened lists of its source document,
// shifted for the preview's own patch. A preview must number lists as the
// real edit would, not treat lists written this session as opened ones.
func (md *markdownSource) carryOpenedLists(dst *Document, at, oldEnd int, text string) {
	state := dst.state()
	state.openedLists = mapOpenedLists(append([]int(nil), md.session.openedLists...), at, oldEnd, len(text))
	state.openedNested = append([]int(nil), md.session.openedNested...)
	dst.setState(state)
}

func (s sessionState) recordOpenedLists(p *parser.Document) sessionState {
	next := s.clone()
	next.openedLists = nil
	next.openedNested = nil
	for i := 1; i < p.Block(0).Next; i++ {
		block := p.Block(i)
		if block.Kind != parser.List {
			continue
		}
		if block.Ordered {
			next.openedLists = append(next.openedLists, block.Start)
		}
		next.openedNested = append(next.openedNested, block.Start)
	}
	return next
}

func mapPoints(points []int, at, oldEnd, delta int) []int {
	kept := points[:0]
	for _, point := range points {
		switch {
		case point < at:
			kept = append(kept, point)
		case point >= oldEnd:
			kept = append(kept, point+delta)
		}
	}
	return kept
}

func mapLiteralLines(lines []parser.LiteralLine, at, oldEnd int, src, nextSrc string) []parser.LiteralLine {
	delta := len(nextSrc) - len(src)
	out := make([]parser.LiteralLine, 0, len(lines))
	for _, literal := range lines {
		line, lineOK := parser.MapLiteralOffset(literal.Line, at, oldEnd, delta)
		content, contentOK := parser.MapLiteralOffset(literal.Content, at, oldEnd, delta)
		if lineOK && contentOK && content >= line {
			out = append(out, parser.LiteralLine{Line: line, Content: content})
			continue
		}
		if literal.Line != at {
			continue
		}
		text := src[literal.Line:lineAt(src, literal.Line).end]
		if !strings.HasPrefix(nextSrc[at:], text) {
			continue
		}
		if stop := at + len(text); stop < len(nextSrc) && nextSrc[stop] != '\n' && nextSrc[stop] != '\r' {
			continue
		}
		out = append(out, parser.LiteralLine{Line: at, Content: at + literal.Content - literal.Line})
	}
	return out
}

func mapRanges(ranges []liveCloser, at, oldEnd, delta int) []liveCloser {
	kept := ranges[:0]
	for _, closer := range ranges {
		switch {
		case closer.to <= at:
			kept = append(kept, closer)
		case closer.from >= oldEnd:
			kept = append(kept, liveCloser{closer.from + delta, closer.to + delta})
		}
	}
	return kept
}

func mapSplitMarkers(markers []splitMarker, at, oldEnd, delta int) []splitMarker {
	kept := markers[:0]
	for _, marker := range markers {
		switch {
		case marker.innerEnd <= at:
		case marker.lineStart < oldEnd:
			continue
		default:
			marker.lineStart += delta
			marker.outerEnd += delta
			marker.innerEnd += delta
		}
		kept = append(kept, marker)
	}
	return kept
}

func mapOpenedLists(lists []int, at, oldEnd, newLen int) []int {
	if len(lists) == 0 || at == oldEnd && newLen == 0 {
		return lists
	}
	delta := newLen - (oldEnd - at)
	kept := lists[:0]
	for _, start := range lists {
		switch {
		case start < at:
			kept = append(kept, start)
		case oldEnd == at:
			if start > at {
				kept = append(kept, start+delta)
			}
		case start >= oldEnd:
			kept = append(kept, start+delta)
		}
	}
	return kept
}
