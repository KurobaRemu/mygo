package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// Text without a language of its own shows in the app's language: the
// editor must shape it so, not leave it to the system language of the
// process. A window app and the headless tester resolve different system
// languages, so leaving it unset made them disagree on caret positions
// beside Han and Latin letters (research/evidence/difffuzz/difffuzz-window-20261109.md).
func TestEditorTextShowsInTheAppsLanguage(t *testing.T) {
	const src = "h试y\n"
	const caret = 4 // source offset after 试
	caretX := func(locale string, font ui.Font) float32 {
		doc := NewMarkdownDocument(src)
		tt := ui.NewTester(func(c *ui.Context) {
			Editor(c, doc, EditorOptions{Font: font}).Fill()
		}, 500, 400)
		tt.SetLocale(locale)
		tt.Frame()
		doc.selectFromSource(caret, caret)
		tt.Frame()
		g, err := TyporaCaretGeometry(doc)
		if err != nil {
			t.Fatal(err)
		}
		return g.X
	}
	en := caretX("en", ui.Font{Lang: "en"})
	if got := caretX("en", ui.Font{}); got != en {
		t.Errorf("no language under en shaped at x=%v, with en at x=%v", got, en)
	}
	if got := caretX("zh-Hans", ui.Font{Lang: "en"}); got != en {
		t.Errorf("font language en under zh-Hans shaped at x=%v, want %v", got, en)
	}
}
