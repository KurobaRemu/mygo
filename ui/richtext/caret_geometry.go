package richtext

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

// CaretGeometry locates the view caret: the paragraph it is shown in, the
// visual line within that paragraph, its x relative to the paragraph's text
// box, and whether it lies inside the editor's viewport. Paragraph counts the
// view's paragraphs, not source blocks, so a code block is one paragraph per
// line and a transient paragraph is one of them.
type CaretGeometry struct {
	Paragraph  int     `json:"paragraph"`
	VisualLine int     `json:"visual_line"`
	X          float32 `json:"x"`
	InViewport bool    `json:"in_viewport"`
}

// TyporaCaretGeometry reads the caret of doc's active view. The view must have
// been laid out (it has painted once), or the paragraph and visual line are
// meaningless: the error says so and the caller can retry after a frame.
func TyporaCaretGeometry(doc *Document) (CaretGeometry, error) {
	if doc == nil {
		return CaretGeometry{}, fmt.Errorf("nil document")
	}
	doc.lock()
	v := doc.activeView()
	if v == nil {
		doc.mu.Unlock()
		return CaretGeometry{}, fmt.Errorf("document has no active view")
	}
	if v.doc != doc {
		doc.mu.Unlock()
		return CaretGeometry{}, fmt.Errorf("active view belongs to another document")
	}
	if v.width <= 0 || v.rect.W <= 0 {
		doc.mu.Unlock()
		return CaretGeometry{}, fmt.Errorf("editor has not been laid out")
	}
	caret, compose, affinity := v.caret, v.composeCaret, v.affinity
	doc.mu.Unlock()

	p := max(0, min(v.shown.paraAt(caret), v.shown.ParagraphCount()-1))
	pv := v.paraLayout(p)
	if pv == nil || pv.layout == nil {
		return CaretGeometry{}, fmt.Errorf("paragraph %d has no layout", p)
	}
	local := v.local(p, caret)
	if compose != 0 && v.shown.paraAt(caret) == p {
		local = min(local+compose, len(pv.units)-1)
	} else {
		local = min(local, len(pv.units)-1)
	}
	position := ui.TextCaretPosition{Index: pv.units[local], Affinity: affinity}
	rect := pv.layout.CaretAt(position)
	_, top := v.origin(p)
	y := v.rect.Y + v.padY + float32(top) + rect.Y - float32(v.scroll)
	return CaretGeometry{
		Paragraph:  p,
		VisualLine: pv.layout.VisualLine(position),
		X:          rect.X,
		InViewport: y >= v.rect.Y && y+rect.H <= v.rect.Y+v.rect.H,
	}, nil
}
