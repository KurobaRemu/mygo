package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestCompositionInNewEmptyListItemWritesIntoIt(t *testing.T) {
	doc := NewMarkdownDocument("- 补充\n- 整理\n- 跟进\n")
	tt, _ := richTester(t, doc)
	doc.SetSelection(5, 5)
	tt.Frame()
	tt.Key(0, ui.KeyTab)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Compose("lian", 4)
	tt.Frame()
	tt.Type("联调")
	tt.Frame()
	if got, want := doc.Source(), "- 补充\n  - 整理\n  - 联调\n- 跟进\n"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}
