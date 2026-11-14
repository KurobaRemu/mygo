package richtext

import (
	"fmt"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestTypingAfterArrowingToLinkTextStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start int
		keys  []ui.Key
	}{
		{"left", 2, []ui.Key{ui.KeyLeft}},
		{"rightTwice", 0, []ui.Key{ui.KeyRight, ui.KeyRight}},
		{"leftTwice", 3, []ui.Key{ui.KeyLeft, ui.KeyLeft}},
	} {
		for _, compose := range []bool{false, true} {
			t.Run(fmt.Sprint(tc.name, compose), func(t *testing.T) {
				doc := NewMarkdownDocument("见[链接](https://x.y)。\n")
				tt, _ := richTester(t, doc)
				doc.SetSelection(tc.start, tc.start)
				tt.Frame()
				for _, k := range tc.keys {
					tt.Key(0, k)
					tt.Frame()
				}
				if compose {
					tt.Compose("zhoubao", 7)
					tt.Frame()
				}
				tt.Type("周报")
				tt.Frame()
				if got, want := doc.Source(), "见[周报链接](https://x.y)。\n"; got != want {
					t.Fatalf("wrote %q, want %q", got, want)
				}
			})
		}
	}
}
