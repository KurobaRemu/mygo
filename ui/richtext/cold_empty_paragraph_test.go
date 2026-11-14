package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// A cold-open empty paragraph keeps its caret at the position where input
// writes, one byte after the paragraph's source anchor. Down from the
// paragraph before it must not move the next input into that paragraph.
func TestColdOpenEmptyParagraphTakesInputAtWritePosition(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		at   int
		want string
	}{
		{"four newlines", "p1.\n\n\n\nH1\n", 3, "p1.\n\nnext\n\nH1\n"},
		{"heading", "before\n\n# body\n\n\n\nafter\n", 14, "before\n\n# body\n\nnext\n\nafter\n"},
		{"six newlines", "p1.\n\n\n\n\n\nH1\n", 3, "p1.\n\nnext\n\n\n\nH1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument(c.src)
			tt, _ := richTester(t, d)
			d.selectFromSource(c.at, c.at)
			tt.Frame()
			tt.Key(0, ui.KeyDown)
			tt.Frame()
			tt.Type("next")
			tt.Frame()
			if got := d.Source(); got != c.want {
				t.Fatalf("wrote %q, want %q", got, c.want)
			}
		})
	}
}
