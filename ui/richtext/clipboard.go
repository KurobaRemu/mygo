package richtext

import (
	"crypto/rand"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// Format is the clipboard's format of attributed text copied from an
// Editor: its paragraphs, styles and runs as JSON, which other MyGo apps
// paste whole, as Lexical and Slate copy a format of their own first.
const Format transfer.Format = "application/x-mygo-rich-text"

// RTF is the format of Rich Text Format, which macOS apps (TextEdit,
// Notes, Pages) copy.
const RTF transfer.Format = "text/rtf"

// clipText is the JSON of the format, for paragraphs of a Text.
type clipText struct {
	// Copy names the copy within the process that made it, whose text,
	// attachments included, pastes as it was.
	Copy       string          `json:"copy,omitempty"`
	Markdown   string          `json:"markdown,omitempty"`
	Paragraphs []clipParagraph `json:"paragraphs"`
}

type clipParagraph struct {
	Style ParagraphStyle `json:"style"`
	Runs  []clipRun      `json:"runs,omitempty"`
}

type clipRun struct {
	Text       string          `json:"text"`
	Attributes clipAttributes  `json:"attributes"`
	Attachment *clipAttachment `json:"attachment,omitempty"`
}

// clipAttributes are the Attributes of a run but its attachment, which
// clipAttachment describes.
type clipAttributes struct {
	Font          string  `json:"font,omitempty"`
	Size          float32 `json:"size,omitempty"`
	Weight        int     `json:"weight,omitempty"`
	Italic        bool    `json:"italic,omitempty"`
	Underline     bool    `json:"underline,omitempty"`
	Strikethrough bool    `json:"strikethrough,omitempty"`
	Color         string  `json:"color,omitempty"`
	Background    string  `json:"background,omitempty"`
	Code          bool    `json:"code,omitempty"`
	Link          string  `json:"link,omitempty"`
	Tag           string  `json:"tag,omitempty"`
}

type clipAttachment struct {
	Name   string  `json:"name,omitempty"`
	URL    string  `json:"url,omitempty"`
	Width  float32 `json:"width,omitempty"`
	Height float32 `json:"height,omitempty"`
}

// copies keeps the last text copied in this process, by the name its
// format carries, so that pasting it keeps its attachments. Names start
// with one random to the process, so that no other app's copy has one.
var (
	copies   sync.Map // string → Text
	copyName atomic.Uint64
	lastCopy atomic.Value // string
	process  = rand.Text()
)

// encodeClip returns t in the clipboard's format.
func encodeClip(t Text, markdown ...string) []byte {
	name := process + "-" + strconv.FormatUint(copyName.Add(1), 36)
	if old, ok := lastCopy.Load().(string); ok {
		copies.Delete(old)
	}
	copies.Store(name, t)
	lastCopy.Store(name)
	c := clipOf(t)
	c.Copy = name
	if len(markdown) > 0 {
		c.Markdown = markdown[0]
	}
	for i := range c.Paragraphs {
		// IDs are the app's, of paragraphs where they are: a paste
		// makes new ones (Text.Replace).
		c.Paragraphs[i].Style.ID = ""
	}
	b, _ := json.Marshal(c)
	return b
}

// MarshalJSON writes t as JSON, as the clipboard's Format holds it, to
// save it: paragraphs of a style each, of runs of text and attributes.
// Attachments keep their names, URLs and sizes; images and drawings are
// the app's to save.
func (t Text) MarshalJSON() ([]byte, error) { return json.Marshal(clipOf(t)) }

// UnmarshalJSON reads what MarshalJSON wrote.
func (t *Text) UnmarshalJSON(b []byte) error {
	var c clipText
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	*t = c.text()
	return nil
}

// clipOf returns the JSON form of t.
func clipOf(t Text) clipText {
	var c clipText
	for _, p := range t.Paragraphs() {
		cp := clipParagraph{Style: p.Style}
		for _, r := range p.Runs {
			a := r.Attributes
			cr := clipRun{Text: lineBreaksAsNewlines(r.Text), Attributes: clipAttributes{
				Font: a.Font, Size: a.Size, Weight: a.Weight, Italic: a.Italic, Underline: a.Underline,
				Strikethrough: a.Strikethrough, Code: a.Code, Link: a.Link, Tag: a.Tag,
			}}
			if a.Color.A > 0 {
				cr.Attributes.Color = cssColor(a.Color)
			}
			if a.Background.A > 0 {
				cr.Attributes.Background = cssColor(a.Background)
			}
			if at := a.Attachment; at != nil {
				w, h := at.size()
				cr.Attachment = &clipAttachment{Name: at.Name, URL: at.URL, Width: w, Height: h}
			}
			cp.Runs = append(cp.Runs, cr)
		}
		c.Paragraphs = append(c.Paragraphs, cp)
	}
	return c
}

// decodeClip returns the text of the clipboard's format.
func decodeClip(b []byte) (Text, bool) {
	t, _, ok := decodeClipSource(b)
	return t, ok
}

func decodeClipSource(b []byte) (Text, string, bool) {
	var c clipText
	if json.Unmarshal(b, &c) != nil {
		return Text{}, "", false
	}
	if c.Copy != "" {
		if t, ok := copies.Load(c.Copy); ok {
			return t.(Text), c.Markdown, true
		}
	}
	if len(c.Paragraphs) == 0 {
		return Text{}, "", false
	}
	return c.text(), c.Markdown, true
}

// text returns the text of the JSON form.
func (c clipText) text() Text {
	var paras []Paragraph
	for _, cp := range c.Paragraphs {
		p := Paragraph{Style: cp.Style}
		for _, cr := range cp.Runs {
			a := Attributes{
				Font: cr.Attributes.Font, Size: cr.Attributes.Size, Weight: cr.Attributes.Weight, Italic: cr.Attributes.Italic,
				Underline: cr.Attributes.Underline, Strikethrough: cr.Attributes.Strikethrough, Code: cr.Attributes.Code,
				Link: safeLink(cr.Attributes.Link), Tag: cr.Attributes.Tag,
			}
			a.Color, _ = parseCSSColor(cr.Attributes.Color)
			a.Background, _ = parseCSSColor(cr.Attributes.Background)
			if at := cr.Attachment; at != nil {
				a.Attachment = &Attachment{Name: at.Name, URL: at.URL, Width: side(at.Width), Height: side(at.Height)}
			}
			p.Runs = append(p.Runs, Run{Text: clipLineBreaks(cr.Text), Attributes: a})
		}
		paras = append(paras, p)
	}
	if len(paras) == 0 {
		return Text{}
	}
	return TextOf(paras...)
}

// clipLineBreaks turns the newlines in the text of a clipRun into the line
// breaks within a paragraph, which its paragraph holds.
func clipLineBreaks(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.ReplaceAll(s, "\n", string(LineSeparator))
}

// PlainText returns t as plain text, its attachments left out.
func (t Text) PlainText() string {
	return strings.ReplaceAll(t.String(), string(ObjectReplacement), "")
}

// clipboardData returns t as the clipboard takes it: in the format of
// the editor, as HTML and as plain text.
func clipboardData(t Text, markdown ...string) transfer.Data {
	plain := t.PlainText()
	if len(markdown) > 0 && markdown[0] != "" {
		plain = markdown[0]
	}
	return transfer.New(transfer.NewItem(
		transfer.Bytes(Format, encodeClip(t, markdown...)),
		transfer.Bytes(transfer.HTML, []byte(t.HTML())),
		transfer.Bytes(transfer.Text, []byte(plain)),
	))
}

// copySelection puts the selection on the clipboard.
func (v *view) copySelection(markdownOnly ...bool) {
	a, z := v.selection()
	if a == z {
		return
	}
	source := v.markdownSlice(a, z)
	if len(markdownOnly) > 0 && markdownOnly[0] {
		v.services.WriteClipboardData(transfer.New(transfer.NewItem(
			transfer.Bytes(transfer.Text, []byte(source)),
		)))
		return
	}
	v.services.WriteClipboardData(clipboardData(v.shown.Slice(a, z), source))
}

func (v *view) markdownSlice(a, z int) string {
	v.doc.lock()
	defer v.doc.mu.Unlock()
	return v.doc.markdownSlice(a, z)
}

func (d *Document) markdownSlice(start, end int) string {
	if start > end {
		start, end = end, start
	}
	start = max(0, min(start, d.text.Len()))
	end = max(0, min(end, d.text.Len()))
	if start == end {
		return ""
	}
	if d.md == nil {
		return d.text.Slice(start, end).Markdown()
	}
	from := d.sourceAtDoc(start)
	to := d.sourceAtDoc(end)
	src := d.md.p.String()
	pa := d.text.paraAt(start)
	pz := d.text.paraAt(end)
	for pass := 0; pass < 8; pass++ {
		changed := false
		for p := pa; p <= pz && p < d.md.m.Len(); p++ {
			var containing []box
			for _, b := range d.md.m.boxes(p) {
				if to <= b.start || from >= b.end {
					continue
				}
				if from >= b.contentFrom && to <= b.contentTo {
					containing = append(containing, b)
					continue
				}
				if from > b.start {
					from, changed = b.start, true
				}
				if to < b.end {
					to, changed = b.end, true
				}
			}
			if len(containing) > 0 {
				sort.SliceStable(containing, func(i, j int) bool {
					if containing[i].start != containing[j].start {
						return containing[i].start < containing[j].start
					}
					return containing[i].end > containing[j].end
				})
				body := src[from:to]
				var prefix, suffix strings.Builder
				for _, b := range containing {
					prefix.WriteString(src[b.openStart:b.openEnd])
				}
				// containing holds the nesting stack outermost first;
				// closing markers run innermost first, so the slice parses
				// as the same nesting (~~**b**~~, not ~~**b~~**).
				for i := len(containing) - 1; i >= 0; i-- {
					b := containing[i]
					suffix.WriteString(src[b.closeStart:b.closeEnd])
				}
				return prefix.String() + body + suffix.String()
			}
		}
		if !changed {
			break
		}
	}
	for leaf := -1; ; {
		next := d.nextLeaf(leaf)
		if next < 0 {
			break
		}
		leaf = next
		b := d.md.p.Block(leaf)
		var fullStart, fullEnd, sourceStart, sourceEnd int
		found := false
		for p := 0; p < d.text.ParagraphCount(); p++ {
			loc := d.paragraphLocation(p)
			if loc.leaf != leaf {
				continue
			}
			ps := d.text.paraStart(p)
			pe := ps + paragraphRunes(d.text.paragraph(p))
			if !found {
				fullStart, fullEnd = ps, pe
				found = true
			} else {
				fullStart, fullEnd = min(fullStart, ps), max(fullEnd, pe)
			}
			if sourceStart == 0 || loc.line.start < sourceStart {
				sourceStart = loc.line.start
			}
			sourceEnd = max(sourceEnd, loc.contentEnd)
		}
		if !found || start > fullStart || end < fullEnd {
			continue
		}
		switch b.Kind {
		case parser.CodeBlock:
			sourceStart, sourceEnd = b.Start, b.End
		case parser.FrontMatter, parser.MathBlock, parser.HTMLBlock:
			sourceStart, sourceEnd = b.Start, b.End
		}
		if sourceStart < from {
			from = sourceStart
		}
		if sourceEnd > to {
			to = sourceEnd
		}
	}
	from = max(0, min(from, len(src)))
	to = max(from, min(to, len(src)))
	return src[from:to]
}

func (d *Document) nextLeaf(after int) int {
	if d.md == nil {
		return -1
	}
	for i := after + 1; i < d.md.p.Block(0).Next; i++ {
		switch d.md.p.Block(i).Kind {
		case parser.Paragraph, parser.Heading, parser.CodeBlock, parser.FrontMatter, parser.ThematicBreak, parser.MathBlock, parser.HTMLBlock:
			if i > after {
				return i
			}
		}
	}
	return -1
}

// paste replaces the selection with what the clipboard holds: the richest
// format the editor reads, or its plain text in the typing attributes when
// matching the style.
func (v *view) paste(matchStyle bool) {
	if matchStyle || v.doc.md != nil && v.para(v.shown.paraAt(v.caret)).style.CodeBlock {
		v.pastePlain()
		return
	}
	var t Text
	var markdown string
	ok := false
	d := v.services.ReadClipboardData(Format, transfer.Markdown, transfer.HTML, RTF, transfer.Text)
	if b, err := d.Read(Format); err == nil {
		t, markdown, ok = decodeClipSource(b)
		if ok && markdown != "" && v.doc.md != nil {
			v.insertMarkdown(markdown)
			return
		}
	}
	if !ok {
		if b, err := d.Read(transfer.Markdown); err == nil && len(b) > 0 {
			if v.doc.md != nil {
				v.insertMarkdown(string(b))
				return
			}
			t, ok = ParseMarkdown(string(b)), true
		}
	}
	if !ok {
		if b, err := d.Read(transfer.HTML); err == nil && len(b) > 0 {
			t, ok = ParseHTML(string(b)), true
		}
	}
	if !ok {
		if b, err := d.Read(RTF); err == nil && len(b) > 0 {
			t, ok = ParseRTF(b), true
		}
	}
	if !ok {
		if b, err := d.Read(transfer.Text); err == nil && len(b) > 0 {
			if v.doc.md != nil {
				v.insertPlain(string(b))
				return
			}
			v.insert(strings.TrimSuffix(string(b), "\n"))
			return
		}
	}
	if !ok {
		s := v.services.ReadClipboard()
		if s != "" {
			if v.doc.md != nil {
				v.insertPlain(s)
			} else {
				v.insert(strings.TrimSuffix(s, "\n"))
			}
		}
		return
	}
	if v.doc.md != nil {
		v.insertMarkdown(t.Markdown())
		return
	}
	v.record(false)
	doc := v.doc
	doc.lock()
	v.lockedSync()
	a, z := v.selection() // as the sync moved it
	v.lockedPublish()
	if v.lockedEdit(func(e *Edit) { e.Replace(a, z, t) }) == nil {
		caret := a + t.Len()
		doc.assignSelection(caret, caret, false)
		v.selAnchor, v.caret = doc.anchor, doc.caret
		v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	}
	doc.mu.Unlock()
	v.mode, v.coalesce = newStep, false
	v.reveal = true
}

func (v *view) pastePlain() {
	d := v.services.ReadClipboardData(transfer.Text)
	if b, err := d.Read(transfer.Text); err == nil && len(b) > 0 {
		if v.doc.md != nil {
			v.insertPlain(string(b))
		} else {
			v.insert(strings.TrimSuffix(string(b), "\n"))
		}
		return
	}
	if s := v.services.ReadClipboard(); s != "" {
		if v.doc.md != nil {
			v.insertPlain(s)
		} else {
			v.insert(strings.TrimSuffix(s, "\n"))
		}
	}
}

func (v *view) insertPlain(s string) {
	if !strings.ContainsAny(s, "\r\n") {
		// Pasting a quote marker is input like typing it: Typora writes the
		// space between the marker and the content (record 2026-11-11,
		// probe83/paste-gt, paste-gt-x). The ATX marker is not: a paste never
		// completes the prefix a typed line still holds literal (record
		// 2026-11-12, seed16/c13).
		s = v.preparePastedPrefix(s)
		v.pfx.quoteSpace = quoteSpaceEdit{}
	}
	if v.transient != nil {
		if v.materializeTransient(func(e *Edit, at int, _ Attributes) error {
			// Typora writes the pasted text as it is and re-reads it as
			// Markdown: source copied from another editor that reads as a
			// marker becomes one (record-20261109-232354).
			return e.markdownIntent(EditRequest{Intent: IntentPastePlain, Start: at, End: at, Text: s})
		}, false) {
			return
		}
	}
	a, z := v.selection()
	v.record(false)
	if at := v.caretSource(); at >= 0 && !strings.ContainsAny(s, "\r\n") && v.insideMarker(at) {
		v.replaceSource(at, at, s, at+len(s))
		return
	}
	v.applyMarkdownIntent(EditRequest{Intent: IntentPastePlain, Start: a, End: z, Text: s})
}

func (v *view) insideMarker(at int) bool {
	for _, b := range v.boxes(v.shown.paraAt(v.caret)) {
		if at > b.openStart && at < b.openEnd || at > b.closeStart && at < b.closeEnd {
			return true
		}
	}
	return false
}

func (v *view) insertMarkdown(markdown string) {
	if v.transient != nil {
		if v.materializeTransient(func(e *Edit, at int, _ Attributes) error {
			return e.markdownIntent(EditRequest{Intent: IntentPasteMarkdown, Start: at, End: at, Text: markdown})
		}, false) {
			return
		}
	}
	a, z := v.selection()
	v.record(false)
	v.applyMarkdownIntent(EditRequest{Intent: IntentPasteMarkdown, Start: a, End: z, Text: markdown})
}

func (v *view) applyMarkdownIntent(req EditRequest) {
	doc := v.doc
	doc.lock()
	v.lockedSync()
	v.lockedPublishSource()
	var err error
	v.lockedEdit(func(e *Edit) {
		err = e.markdownIntent(req)
		if err != nil {
			e.fail(err)
		}
	})
	if err == nil {
		v.selAnchor, v.caret = doc.anchor, doc.caret
		v.selVersion = doc.selVersion
	}
	doc.mu.Unlock()
	v.mode, v.coalesce = newStep, false
	v.reveal = true
	v.holdPastedEmptyPair()
}

func (v *view) cutSelection() {
	a, z := v.selection()
	if a == z || v.opts.ReadOnly {
		return
	}
	v.copySelection()
	if from, to, ok := v.cutSourceRange(a, z); ok {
		v.record(false)
		v.replaceSource(from, to, "", from)
		v.holdEmptiedDocument()
		return
	}
	a, z = v.cutRange(a, z)
	v.deleteRange(a, z, false)
	v.holdEmptiedDocument()
}

func (v *view) cutSourceRange(a, z int) (int, int, bool) {
	d := v.doc
	d.lock()
	defer d.mu.Unlock()
	from, to := d.sourceAtDoc(a), d.sourceAtDoc(z)
	found := false
	pa, pz := v.shown.paraAt(a), v.shown.paraAt(z)
	for p := pa; p <= pz && p < v.source.Len(); p++ {
		ps := v.shown.paraStart(p)
		for _, b := range v.boxes(p) {
			if b.kind != MarkerLink || b.docEnd <= b.docStart {
				continue
			}
			bs, be := ps+b.docStart, ps+b.docEnd
			if a < be && z > bs {
				from, to, found = min(from, b.start), max(to, b.end), true
			}
		}
	}
	return from, to, found
}

func (v *view) cutRange(a, z int) (int, int) {
	pa, pz := v.shown.paraAt(a), v.shown.paraAt(z)
	for p := pa; p <= pz && p < v.source.Len(); p++ {
		ps := v.shown.paraStart(p)
		for _, b := range v.boxes(p) {
			if b.kind != MarkerLink || b.docEnd <= b.docStart {
				continue
			}
			bs, be := ps+b.docStart, ps+b.docEnd
			if a < be && z > bs {
				a, z = min(a, bs), max(z, be)
			}
		}
	}
	return a, z
}
