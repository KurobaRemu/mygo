package richtext

import (
	"strings"
	"unicode/utf8"
)

// livePair is a strong element the session's own edit put in the view while
// the parser reads its delimiters as text. A formatting command's closing
// `**` stops balancing once the content typed inside it ends in a space, and
// a paste at a strong span's end leaves an empty pair after the one the
// parser still reads. Typora's live model keeps the DOM element it made
// either way, so a caret inside it reveals both delimiters although a cold
// open of the same bytes holds no element at all (VM record
// 2026-11-12-064820, seed11/c2 and seed11/c3).
//
// The hold is the view's, not the document's: it never changes an edit or a
// saved byte. It follows the bytes the session edits, and lapses as soon as
// the caret stands outside the element or the delimiters stop spelling the
// `**` the edit wrote.
type livePair struct {
	para       int
	openStart  int
	openEnd    int
	closeStart int
	closeEnd   int
	// endCaret marks the pair the session closed with the caret left at the
	// closing delimiter's end, where Typora reveals it as a whole.
	endCaret bool
	// tail marks the pair whose paragraph ended at its closing delimiters with
	// the caret drawn on the paragraph below: a split at the content end keeps
	// the element the edit made in the paragraph above, and Typora draws it
	// without its delimiters because the caret is not in it (record
	// 2026-11-13, seed17/c11).
	tail bool
}

// holdTypedStrongComplete records the strong element the typed `**` closes
// when the parser reads the span as literal text. Typora opens the element
// as soon as the session types the opener, so the closing run completes it
// whatever the content before it is: `- item two**` then `- ` and `**`
// leaves a strong around `- `, while a cold open reads the same bytes as
// text (record 2026-11-13, seed16/c1).
func (v *view) holdTypedStrongComplete(typed string) {
	if !strings.HasSuffix(typed, "*") || v.hold != nil || v.empty != nil ||
		v.doc == nil || v.doc.md == nil || v.opts.ReadOnly {
		return
	}
	d := v.doc
	d.lock()
	src := d.md.p.String()
	sel := d.selection
	d.mu.Unlock()
	if sel.Anchor.Offset != sel.Caret.Offset {
		return
	}
	at := sel.Caret.Offset
	// The keystroke typed the run's last byte: `**` arrives as two `*`
	// keystrokes, and the pair is complete when the run ending at the caret
	// holds two stars and no longer run follows it.
	if at < 4 || at > len(src) || src[at-1] != '*' {
		return
	}
	if at < len(src) && src[at] == '*' {
		// A longer run is not the pair the keystroke completed.
		return
	}
	closeStart := at - 1
	if src[closeStart-1] == '*' && closeStart > 1 && src[closeStart-2] != '*' {
		closeStart--
	}
	if at-closeStart != 2 {
		// The closing run is one star: the keystroke half closed an
		// emphasis, not the `**` pair.
		return
	}
	// The view's projection and caret offsets catch up on the next frame, so
	// the paragraph comes from the source the edit wrote.
	open := v.strongOpenerBeforeIn(src, at-2)
	if open < 0 {
		return
	}
	p := v.paragraphHoldingSource(at - 2)
	if p < 0 {
		return
	}
	for _, b := range v.source.boxes(p) {
		if b.start == open && b.end == at {
			// The parser reads the element itself; its own box reveals it.
			return
		}
	}
	v.hold = &livePair{
		para: p, openStart: open, openEnd: open + 2,
		closeStart: at - 2, closeEnd: at, endCaret: true,
	}
}

// holdStrongPairAfterTypedOpener replaces the empty pair a formatting command
// wrote at a paragraph's end with the strong element Typora's live model holds
// when the session typed `**` right before it: the typed run is the opener and
// the command's first two stars are content, so `alpha` + typed `**` + ⌘B
// leaves `alpha******` holding a strong whose delimiters are the first and last
// two stars, each drawn over its own bytes. Reporting the command's own pair
// instead reads the four stars as one close marker (record 2026-11-13,
// seed17/c15 and c17). It reports whether it took the pair.
func (v *view) holdStrongPairAfterTypedOpener(p int, src string, pairStart, pairEnd int) bool {
	if v.hold != nil || pairStart < 2 || pairEnd > len(src) || pairEnd-pairStart != 4 {
		return false
	}
	if src[pairStart-2:pairStart] != "**" || src[pairStart:pairEnd] != "****" {
		return false
	}
	if pairStart-3 >= 0 && src[pairStart-3] == '*' {
		// One longer run is one delimiter run, not an opener beside a pair.
		return false
	}
	if pairEnd < len(src) && src[pairEnd] == '*' {
		return false
	}
	if from := v.source.start(p); pairStart-2 < from {
		return false
	}
	v.hold = &livePair{
		para: p, openStart: pairStart - 2, openEnd: pairStart,
		closeStart: pairEnd - 2, closeEnd: pairEnd,
	}
	return true
}

// parserReadsTypedPair reports whether the parser already reads an element over
// the bytes from..to, as it does over a pasted `<b>te</b>`'s span: the `**`
// typed after that element stays literal text, so the view holds no empty pair
// for it (record 2026-11-13, seed17/c8).
func (v *view) parserReadsTypedPair(p, from, to int) bool {
	for _, b := range v.source.boxes(p) {
		if b.end > from && b.start < to {
			return true
		}
	}
	return false
}

// strongOpenerBeforeIn is the source start of the `**` run in src that opens
// the element the closing run at closeStart completes, -1 when the line holds
// none: the nearest run of exactly two stars before it whose content is not
// blank.
func (v *view) strongOpenerBeforeIn(src string, closeStart int) int {
	from := 0
	for k := min(closeStart, len(src)) - 1; k >= 0; k-- {
		if src[k] == '\n' || src[k] == '\r' {
			from = k + 1
			break
		}
	}
	for k := closeStart - 2; k >= from; k-- {
		if src[k] != '*' || src[k+1] != '*' {
			continue
		}
		if k > from && src[k-1] == '*' {
			continue
		}
		if k+2 < len(src) && src[k+2] == '*' {
			continue
		}
		content := src[k+2 : closeStart]
		if content == "" || strings.TrimSpace(content) == "" {
			continue
		}
		if !strings.HasSuffix(content, " ") && !strings.HasSuffix(content, "\t") {
			// Content that ends in no space closes the pair in the
			// parser as well, so the pair is not the one the typed
			// run completed (record 2026-11-12, seed14/c6).
			continue
		}
		return k
	}
	return -1
}

// holdStrongFromPatch records the strong pair a formatting command just
// wrote around the caret: the command's own source patch spells both
// delimiters, and the caret stands between them. An empty pair is the view's
// retained container instead (v.empty).
func (v *view) holdStrongFromPatch() {
	if v.empty != nil || v.doc == nil || v.doc.md == nil || v.opts.ReadOnly {
		return
	}
	start, _, newEnd, ok := v.doc.liveHoldPatch()
	if !ok || newEnd-start <= 4 {
		return
	}
	src := v.doc.liveHoldSource()
	if src == "" || newEnd > len(src) ||
		src[start:start+2] != "**" || src[newEnd-2:newEnd] != "**" ||
		start > 0 && src[start-1] == '*' || newEnd < len(src) && src[newEnd] == '*' ||
		start+2 < len(src) && src[start+2] == '*' || newEnd >= 3 && src[newEnd-3] == '*' {
		// A longer run of stars is not the pair the command wrote: ⌘B inside
		// `*beta*` writes `***beta***`, whose live model nests an emphasis
		// in a strong instead of holding this span.
		return
	}
	caret := v.caretSource()
	if caret < start+2 || caret > newEnd-2 {
		return
	}
	p := v.paragraphHoldingSource(start)
	if p < 0 {
		return
	}
	v.hold = &livePair{
		para:       p,
		openStart:  start,
		openEnd:    start + 2,
		closeStart: newEnd - 2,
		closeEnd:   newEnd,
	}
}

// holdPastedEmptyPair records the empty strong pair an edit wrote at a
// paragraph's content end with the caret between its delimiters, as the paste
// of HTML bold `qq` after a strong span leaves `qq****`. The bytes are the
// `****` a cold open reads as text; Typora's live view reports the pair at
// the caret the way it reports one typed there (VM record 2026-11-12-064820,
// seed11/c2).
func (v *view) holdPastedEmptyPair() {
	if v.empty != nil || v.doc == nil || v.doc.md == nil || v.opts.ReadOnly {
		return
	}
	d := v.doc
	caret := d.selection.Caret.Offset
	src := d.liveHoldSource()
	if caret < 2 || caret+2 > len(src) ||
		src[caret-2:caret] != "**" || src[caret:caret+2] != "**" {
		return
	}
	p := v.paragraphHoldingSource(caret - 2)
	if p < 0 || caret+2 != v.paragraphEndSource(p) {
		return
	}
	v.hold = &livePair{
		para:       p,
		openStart:  caret - 2,
		openEnd:    caret,
		closeStart: caret,
		closeEnd:   caret + 2,
	}
}

// paragraphHoldingSource is the paragraph whose source holds at, -1 when
// none does.
func (v *view) paragraphHoldingSource(at int) int {
	for p := 0; p < v.source.Len(); p++ {
		from, to, _ := v.source.Paragraph(p)
		if at >= from && at < to {
			return p
		}
	}
	return -1
}

// mapLiveHold moves a held pair through the document's last source patch. An
// edit that wrote over a delimiter takes the pair it spelled with it.
func (v *view) mapLiveHold(start, oldEnd, newEnd int, ok bool) {
	h := v.hold
	if h == nil || !ok {
		return
	}
	at := start
	if oldEnd > at &&
		(at < h.openEnd && oldEnd > h.openStart || at < h.closeEnd && oldEnd > h.closeStart) {
		v.hold = nil
		return
	}
	// A delimiter the edit starts at or after keeps its span: text typed at
	// the content end goes inside the element and the closing delimiters
	// only shift when the edit began before them.
	delta := newEnd - oldEnd
	if h.closeStart >= oldEnd {
		h.closeStart += delta
		h.closeEnd += delta
	}
	if h.openStart >= oldEnd {
		h.openStart += delta
		h.openEnd += delta
	}
}

// liveHoldAt is the held pair as the bytes src and the caret stand: nil once
// the pair is no longer the paragraph's, the caret's, or spellable at all.
func (v *view) liveHoldAt(src string, caret int) *livePair {
	h := v.hold
	if h == nil {
		return nil
	}
	if h.closeStart < h.openEnd || h.closeEnd > len(src) {
		return nil
	}
	if src[h.openStart:h.openEnd] != "**" || src[h.closeStart:h.closeEnd] != "**" {
		return nil
	}
	p := v.paragraphHoldingSource(h.openStart)
	if p < 0 {
		return nil
	}
	tail := false
	switch {
	case h.endCaret:
		// The session closed the pair with the caret at its closing
		// delimiter, where Typora draws both delimiters of the element it
		// just completed, and keeps drawing them while the caret stays at or
		// after them (record 2026-11-13, seed16/c1).
		if caret < h.closeEnd {
			return nil
		}
	case caret >= h.openEnd && caret <= h.closeStart:
		// The caret stands in the element and reveals both delimiters.
	case caret >= h.closeEnd && h.closeEnd == v.paragraphEndSource(p):
		// The pair ends its paragraph and the caret stands past it. With the
		// caret drawn below, a split at the content end kept the element in
		// the paragraph above without its delimiters; with the caret back on
		// that paragraph, the pair reveals them like any other.
		tail = v.shown.paraAt(v.caret) != p
	default:
		return nil
	}
	h.tail = tail
	if _, to, _ := v.source.Paragraph(p); h.closeEnd > to {
		return nil
	}
	for _, b := range v.source.boxes(p) {
		if b.openStart == h.openStart && b.closeEnd < h.closeEnd {
			// An edit closed the element early, as the paste of a strong
			// span does: the parser now reads a shorter pair from the same
			// opener, and the live view holds whatever follows it instead.
			return nil
		}
	}
	h.para = p
	return h
}

// withLiveHeldBox adds the held pair to paragraph p's source-edit boxes when
// the caret stands inside it, for visible syntax reveal only. The returned
// slice is never a source-edit fact: the held pair's bytes still parse as text.
func (v *view) withLiveHeldBox(p, source int, boxes []box) []box {
	held, ok := v.liveHoldBox(p, source, boxes)
	if !ok {
		return boxes
	}
	return append(boxes[:len(boxes):len(boxes)], held)
}

// liveHoldBox is the visible syntax box the reveal pass draws for a held pair:
// both delimiters as markers while the caret is inside the element. It reports
// false when the pair is another paragraph's, when the caret left it, or when
// the parser already reads the element itself. It never changes source-edit
// boxes, which remain v.sourceEditBoxes(p).
func (v *view) liveHoldBox(p, source int, boxes []box) (box, bool) {
	h := v.hold
	if h == nil || h.para != p || p >= v.source.Len() || source < 0 {
		return box{}, false
	}
	if v.liveHoldAt(v.sourceText(), source) == nil {
		v.hold = nil
		return box{}, false
	}
	if h.tail {
		// The caret is on the paragraph below: the element draws its content
		// alone, without revealing anything (see hiddenLiveHeldPair).
		return box{}, false
	}
	if v.shown.paraAt(v.caret) != p {
		v.hold = nil
		return box{}, false
	}
	for _, b := range boxes {
		if b.start == h.openStart && b.end == h.closeEnd {
			// The parser reads the element itself; its own box reveals it.
			return box{}, false
		}
	}
	return box{
		open:        -1,
		close:       -1,
		kind:        MarkerStrong,
		start:       h.openStart,
		end:         h.closeEnd,
		openStart:   h.openStart,
		openEnd:     h.openEnd,
		closeStart:  h.closeStart,
		closeEnd:    h.closeEnd,
		contentFrom: h.openEnd,
		contentTo:   h.closeStart,
		literal:     true,
	}, true
}

// hiddenLiveHeldPair is the held pair paragraph p draws with its delimiters
// hidden: the caret left it through a split at its content end, so the two
// `**` runs are hidden bytes of the element Typora's live DOM kept rather
// than text the paragraph shows (record 2026-11-13, seed17/c11).
func (v *view) hiddenLiveHeldPair(p int) *livePair {
	h := v.hold
	if h == nil || h.para != p {
		return nil
	}
	if v.liveHoldAt(v.sourceText(), v.revealCaretSource()) == nil || !h.tail {
		return nil
	}
	return h
}

// hideHeldDelimiters drops the delimiters of the held pair h from parts, the
// way the parser's own boxes hide theirs, and keeps the element's content
// strong: the paragraph shows what Typora's live DOM renders, not the bytes
// the parser read as text.
func hideHeldDelimiters(parts []displayPart, h *livePair) []displayPart {
	parts = dropDisplaySource(parts, h.openStart, h.openEnd)
	parts = dropDisplaySource(parts, h.closeStart, h.closeEnd)
	for i := range parts {
		if parts[i].source >= h.openEnd && parts[i].srcEnd <= h.closeStart {
			parts[i].attrs.Weight = 700
		}
	}
	return parts
}

// dropDisplaySource removes the source bytes from..end from parts. The bytes
// come from a run the parser read as literal text, whose display spells its
// own source rune for rune: the rest of the run keeps the doc and source
// offsets of the bytes the drop leaves. A part that shows something other
// than its bytes, as a marker does, is left as it is.
func dropDisplaySource(parts []displayPart, from, end int) []displayPart {
	var out []displayPart
	for _, part := range parts {
		start, stop := max(from, part.source), min(end, part.srcEnd)
		if start >= stop || part.marker {
			out = append(out, part)
			continue
		}
		count := utf8.RuneCountInString(part.text)
		if utf8.RuneCountInString(part.src) != count || part.docEnd-part.doc != count {
			out = append(out, part)
			continue
		}
		runes := []rune(part.text)
		fromRune := runesBeforeByte(part.src, start-part.source)
		toRune := runesBeforeByte(part.src, stop-part.source)
		if fromRune > 0 {
			left := part
			left.text = string(runes[:fromRune])
			left.src = part.src[:start-part.source]
			left.docEnd = part.doc + fromRune
			left.srcEnd = start
			out = append(out, left)
		}
		if toRune < count {
			right := part
			right.text = string(runes[toRune:])
			right.src = part.src[stop-part.source:]
			right.doc = part.doc + toRune
			right.source = stop
			out = append(out, right)
		}
	}
	return out
}

// runesBeforeByte counts the runes of raw that end at or before byte at.
func runesBeforeByte(raw string, at int) int {
	n := 0
	for k := 0; k < at && k < len(raw); {
		_, size := utf8.DecodeRuneInString(raw[k:])
		k += size
		if k <= at {
			n++
		}
	}
	return n
}

// liveHoldPatch is the source patch of the document's last step, with the
// document's lock held.
func (d *Document) liveHoldPatch() (start, oldEnd, newEnd int, ok bool) {
	if d.md == nil || !d.md.ok {
		return 0, 0, 0, false
	}
	return d.md.last[0], d.md.last[1], d.md.last[2], true
}

// liveHoldSource is the document's source, with the document's lock held.
func (d *Document) liveHoldSource() string {
	if d.md == nil {
		return ""
	}
	return d.md.p.String()
}
