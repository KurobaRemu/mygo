package richtext

import (
	"fmt"
	"unicode/utf8"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// fenceLangInput is the language input of a fenced code block holding the
// focus, as Typora's SPAN.ty-input.ty-cm-lang-input does. start and end are
// the language bytes' range in the document, original the bytes the input
// took, and text what it shows: the document keeps the bytes until the focus
// leaves the input or the file saves, while the selection is the input's own,
// in runes of text (record 2026-11-11, probe92).
type fenceLangInput struct {
	start, end int
	original   string
	text       string
	anchor     int
	focus      int
	// shiftRight keeps the input's selection after the first Shift+Right
	// opened the block's exit paragraph; repeats of the key do not move it
	// (record 2026-11-14, seed23/c13).
	shiftRight bool
}

// enterFenceLanguageInput moves the focus into the language input of the
// fenced code block whose code content ends at the caret, selecting the whole
// language as Typora does on Right there (record 2026-11-11,
// probe92/right-right). The document's caret moves to the start of the
// block's code content, where Typora reports it while the input holds the
// focus.
func (v *view) enterFenceLanguageInput() bool {
	if v.lang != nil || v.transient != nil || v.opts.ReadOnly || v.selAnchor != v.caret {
		return false
	}
	caret := v.caretSource()
	if caret < 0 {
		return false
	}
	start, end, contentStart, ok := v.fenceLanguageAt(caret)
	if !ok {
		return false
	}
	v.focusFenceLanguageInput(start, end, contentStart)
	return true
}

// focusFenceLanguageInput makes the language at start:end the focused input
// with the whole language selected, and puts the document's caret at the
// start of the block's code content, where Typora reports it while the input
// holds the focus.
func (v *view) focusFenceLanguageInput(start, end, contentStart int) {
	src := v.doc.Source()
	v.lang = &fenceLangInput{
		start:    start,
		end:      end,
		original: src[start:end],
		text:     src[start:end],
		anchor:   0,
		focus:    utf8.RuneCountInString(src[start:end]),
	}
	v.setSourceCaret(contentStart)
}

// resumeFenceLanguageFocus puts the language input back in focus after Redo:
// the restored step left the document's caret at the start of the fenced
// block's code content, where the input put it, and the input takes the focus
// back there with the whole language selected (record 2026-11-12, seed18/c5:
// Down leaves the block at 6, Redo re-enters the input, and the text typed
// next replaces the info string instead of the first code line).
func (v *view) resumeFenceLanguageFocus() {
	if v.lang != nil || v.transient != nil || v.opts.ReadOnly || v.selAnchor != v.caret {
		return
	}
	caret := v.caretSource()
	if caret < 0 {
		return
	}
	start, end, contentStart, ok := v.fenceLanguageAtContentStart(caret)
	if !ok {
		return
	}
	v.focusFenceLanguageInput(start, end, contentStart)
}

// fenceLanguageAt locates the language of the fenced code block whose code
// content ends at source offset off: the language's byte range on the opening
// marker line and the offset the code content starts at.
func (v *view) fenceLanguageAt(off int) (start, end, contentStart int, ok bool) {
	return v.fenceLanguageAtPosition(off, false)
}

// fenceLanguageAtContentStart locates the language of the fenced code block
// whose code content starts at source offset off, the offset the language
// input leaves the document's caret on.
func (v *view) fenceLanguageAtContentStart(off int) (start, end, contentStart int, ok bool) {
	return v.fenceLanguageAtPosition(off, true)
}

// fenceLanguageAtPosition locates the language of the fenced code block whose
// code content ends at off, or starts at it when atContentStart is set.
func (v *view) fenceLanguageAtPosition(off int, atContentStart bool) (start, end, contentStart int, ok bool) {
	if v.doc == nil || v.doc.md == nil || off < 0 {
		return 0, 0, 0, false
	}
	p := v.doc.md.p
	src := p.String()
	if off > len(src) {
		return 0, 0, 0, false
	}
	for i := 1; i < p.Block(0).Next; i++ {
		b := p.Block(i)
		if b.Kind != parser.CodeBlock || !b.Fenced || b.Open || b.Start >= len(src) {
			continue
		}
		open := lineAt(src, b.Start)
		close := lineAt(src, max(0, b.End-1))
		if close.start <= open.stop {
			// The block has no code line: its empty content is the closing
			// marker's own line and the caret is never on it.
			continue
		}
		if atContentStart {
			if open.stop != off {
				continue
			}
		} else {
			at := close.start
			if at > 0 && src[at-1] == '\n' {
				at--
				if at > 0 && src[at-1] == '\r' {
					at--
				}
			}
			if at != off {
				continue
			}
		}
		run, runLen, found := fenceRunAt(src, open, true)
		if !found {
			return 0, 0, 0, false
		}
		start = run + runLen
		end = open.end
		for start < end && (src[start] == ' ' || src[start] == '\t') {
			start++
		}
		for end > start && (src[end-1] == ' ' || src[end-1] == '\t') {
			end--
		}
		return start, end, open.stop, true
	}
	return 0, 0, 0, false
}

// fenceLanguageContentStart is the offset the block's code content starts at
// in src, which holds the block's opening line from infoStart.
func fenceLanguageContentStart(src string, infoStart int) int {
	return lineAt(src, infoStart).stop
}

// fenceLangKey handles a key while the language input holds the focus. Keys
// that produce text return false and reach the text event after them.
func (v *view) fenceLangKey(key ui.Key, mods ui.Modifiers) bool {
	if typesText(key, mods) {
		return false
	}
	if key == ui.KeyRight && mods == ui.Shift && v.lang.shiftRight {
		return true
	}
	l := v.lang
	shift := mods&ui.Shift != 0
	cmd := mods&ui.Cmd != 0
	alt := mods&ui.Alt != 0
	lo, hi := min(l.anchor, l.focus), max(l.anchor, l.focus)
	n := utf8.RuneCountInString(l.text)
	collapse := func(at int) {
		l.anchor, l.focus = at, at
	}
	extend := func(at int) {
		l.focus = max(0, min(at, n))
	}
	switch key {
	case ui.KeyRight:
		switch {
		case cmd && !shift:
			// Cmd+Right puts the input's caret at the text's end (record
			// 2026-11-11, probe92b/cmd-right-escape).
			collapse(n)
		case alt && !shift:
			// Alt+Right changes nothing: the input keeps its whole selection
			// (record 2026-11-11, probe92b/alt-right-escape,
			// /alt-right-type-tab).
		case !shift:
			if lo != hi {
				collapse(hi)
			} else {
				collapse(min(hi+1, n))
			}
		case lo != hi && lo == l.focus:
			// A selection behind the focus turns around in one step: the
			// focus goes to its other end and the old focus anchors
			// (record 2026-11-11, probe92/right-shift-left).
			l.anchor, l.focus = l.focus, hi
		default:
			extend(l.focus + 1)
		}
	case ui.KeyLeft:
		switch {
		case (cmd || alt) && !shift:
			// Cmd+Left and Alt+Left put the caret before the language
			// (record 2026-11-11, probe92b/cmd-left-escape,
			// /alt-left-escape).
			collapse(0)
		case !shift:
			if lo != hi {
				collapse(lo)
			} else {
				collapse(max(lo-1, 0))
			}
		case lo != hi && hi == l.focus:
			l.anchor, l.focus = l.focus, lo
		default:
			extend(l.focus - 1)
		}
	case ui.KeyBackspace:
		if lo != hi {
			l.text = string([]rune(l.text)[:lo]) + string([]rune(l.text)[hi:])
			collapse(lo)
		} else if lo > 0 {
			runes := []rune(l.text)
			l.text = string(runes[:lo-1]) + string(runes[lo:])
			collapse(lo - 1)
		}
	case ui.KeyDelete:
		if lo != hi {
			l.text = string([]rune(l.text)[:lo]) + string([]rune(l.text)[hi:])
			collapse(lo)
		} else if lo < n {
			runes := []rune(l.text)
			l.text = string(runes[:lo]) + string(runes[lo+1:])
			collapse(lo)
		}
	case ui.KeyHome, ui.KeyEnd:
		// Home and End change nothing in the input, selection or not
		// (record 2026-11-11, probe92b/home-escape, /end-escape).
	case ui.KeyUp:
		v.leaveFenceLanguageByVertical(false)
	case ui.KeyDown:
		v.leaveFenceLanguageByVertical(true)
	case ui.KeyEnter:
		v.leaveFenceLanguageByEnter()
	case ui.KeyTab:
		v.commitFenceLanguage()
	case ui.KeyEscape:
		// Escape leaves the input focused and changes nothing (record
		// 2026-11-11, probe92/right-escape).
	}
	return true
}

// fenceLangType puts text into the language input at its selection.
func (v *view) fenceLangType(text string) {
	l := v.lang
	runes := []rune(l.text)
	lo, hi := min(l.anchor, l.focus), max(l.anchor, l.focus)
	l.text = string(runes[:lo]) + text + string(runes[hi:])
	at := lo + utf8.RuneCountInString(text)
	l.anchor, l.focus = at, at
	l.shiftRight = false
}

// leaveFenceLanguageByShiftRight writes an IME-committed info string, then
// leaves the input for the empty paragraph after the block while keeping the
// input's backward selection (record 2026-11-14, seed23/c13).
func (v *view) leaveFenceLanguageByShiftRight() bool {
	l := v.lang
	if l == nil || l.text == l.original || v.transient != nil {
		return false
	}
	v.commitFenceLanguageInPlace()
	v.lang = nil
	v.sync()
	block := v.shown.paraAt(v.caret)
	_, last, ok := v.codeBlockAt(block)
	if !ok || last != len(v.paras)-1 {
		return false
	}
	if !v.openTransient(IntentExitBlock, v.paragraphEndSource(last), true) {
		return false
	}
	transient := v.transient
	transient.mode = transientPhantomEOF
	transient.finalEOL = false
	// Typora leaves the caret of the committed CJK composition one rune into
	// its marked text and the selection's anchor at its start (record
	// 2026-11-14, seed23/c13 cp3).
	l.anchor, l.focus = 1, 0
	l.shiftRight = true
	v.lang = l
	v.anchorAffinity, v.caretAffinity = BoundaryUpstream, BoundaryDownstream
	v.setSourceSelectionLocked(SourceSelection{
		Anchor: sourcePoint(transient.preview.SourceSelection().Caret.Offset, BoundaryUpstream),
		Caret:  sourcePoint(transient.preview.SourceSelection().Caret.Offset, BoundaryDownstream),
	})
	return true
}

// commitFenceLanguage writes the input's text into the document, leaves the
// input, and puts the document's caret at the start of the block's code
// content, where Tab and Shift+Tab put it (record 2026-11-11,
// probe92/right-tab, /right-shift-tab).
func (v *view) commitFenceLanguage() {
	l := v.lang
	if l == nil {
		return
	}
	v.lang = nil
	src := v.doc.Source()
	if l.text != l.original {
		next := src[:l.start] + l.text + src[l.end:]
		v.replaceSource(l.start, l.end, l.text, fenceLanguageContentStart(next, l.start))
		return
	}
	v.setSourceCaret(fenceLanguageContentStart(src, l.start))
}

// leaveFenceLanguageByVertical leaves the input the way Up and Down do. Up
// puts the document's caret at the end of the block's last code line, which
// the block's own focus ends at; Down opens the empty paragraph after the
// block, the state an unchanged language leaves in on Enter (record
// 2026-11-11, probe92b/up-escape, /down-escape). Only the unchanged exit is
// recorded; a language the input changed is written first, which changes no
// bytes of either recorded landing.
func (v *view) leaveFenceLanguageByVertical(down bool) {
	l := v.lang
	if l == nil {
		return
	}
	if l.text != l.original {
		v.commitFenceLanguageInPlace()
	}
	block := v.shown.paraAt(v.caret)
	_, last, ok := v.codeBlockAt(block)
	if !ok {
		v.commitFenceLanguage()
		return
	}
	v.lang = nil
	if !down {
		v.setSourceCaret(v.paragraphEndSource(last))
		return
	}
	if last < len(v.paras)-1 {
		// The block has a paragraph after it: Down moves the caret into it.
		v.setSourceCaret(v.source.start(last + 1))
		return
	}
	if v.openTransient(IntentExitBlock, v.paragraphEndSource(last), true) {
		v.transient.mode = transientPhantomEOF
		return
	}
	v.setSourceCaret(v.paragraphEndSource(last))
}

// leaveFenceLanguageByEnter leaves the input the way Enter does. Enter on an
// unchanged language leaves the caret after the block with the empty
// paragraph that follows it waiting in the view; after the language changed,
// Enter writes it and keeps the input focused (record 2026-11-11,
// probe92/right-enter, /right-type-enter).
func (v *view) leaveFenceLanguageByEnter() {
	l := v.lang
	if l == nil {
		return
	}
	if l.text != l.original {
		v.commitFenceLanguageInPlace()
		return
	}
	block := v.shown.paraAt(v.caret)
	_, last, ok := v.codeBlockAt(block)
	if !ok {
		v.commitFenceLanguage()
		return
	}
	v.lang = nil
	atEOF := last == len(v.paras)-1
	if v.openTransient(IntentExitBlock, v.paragraphEndSource(last), atEOF) {
		v.transient.mode = transientPhantomEOF
		return
	}
	v.setSourceCaret(fenceLanguageContentStart(v.doc.Source(), l.start))
}

// fenceLangPaste replaces the input's selection with the clipboard's plain
// text. Unlike typing, a paste writes the language into the document at once,
// and the input's caret then stands before the pasted text (record 2026-11-11,
// probe92b/cmd-v-escape, /cmd-v-type-tab: rb:0:0, then a typed c makes crb).
func (v *view) fenceLangPaste() {
	l := v.lang
	if l == nil {
		return
	}
	text := v.fenceLangClipboardText()
	if text == "" {
		return
	}
	runes := []rune(l.text)
	lo, hi := min(l.anchor, l.focus), max(l.anchor, l.focus)
	l.text = string(runes[:lo]) + text + string(runes[hi:])
	l.anchor, l.focus = 0, 0
	v.commitFenceLanguageInPlace()
}

// fenceLangClipboardText is the clipboard's plain text, as a paste into the
// language input takes it.
func (v *view) fenceLangClipboardText() string {
	d := v.services.ReadClipboardData(transfer.Text)
	if b, err := d.Read(transfer.Text); err == nil && len(b) > 0 {
		return string(b)
	}
	return v.services.ReadClipboard()
}

// commitFenceLanguageInPlace writes the input's text into the document and
// keeps the input focused, as Enter does after the language changed.
func (v *view) commitFenceLanguageInPlace() {
	l := v.lang
	if l == nil || l.text == l.original {
		return
	}
	src := v.doc.Source()
	next := src[:l.start] + l.text + src[l.end:]
	v.replaceSource(l.start, l.end, l.text, fenceLanguageContentStart(next, l.start))
	l.end = l.start + len(l.text)
	l.original = l.text
}

// commitFenceLanguageForSave writes the input's text for a save without
// leaving the input focused, as Typora's file serialization commits it.
func (v *view) commitFenceLanguageForSave() {
	if v.lang == nil {
		return
	}
	src := v.doc.Source()
	if v.lang.text != v.lang.original {
		v.commitFenceLanguageInPlace()
		return
	}
	v.setSourceCaret(fenceLanguageContentStart(src, v.lang.start))
}

// materializeActiveLangInput writes the language text of the active view's
// focused input, as Typora commits it when the file is saved.
func (d *Document) materializeActiveLangInput() {
	d.lock()
	v := d.activeView()
	var lang *fenceLangInput
	if v != nil {
		lang = v.lang
	}
	d.mu.Unlock()
	if v == nil || lang == nil {
		return
	}
	v.commitFenceLanguageForSave()
}

// fenceLangFocus reports the focused language input as Typora's focus_type
// names it: the input's text and its own selection in runes.
func fenceLangFocus(l *fenceLangInput) string {
	if l == nil {
		return ""
	}
	return fmt.Sprintf("fence-language-input:%s:%d:%d", l.text, l.anchor, l.focus)
}
