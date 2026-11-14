package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// One blank line after the final EOL opens as an empty paragraph. Input
// there starts a paragraph of its own, as in Typora 1.14.15 (oracle
// followup/final-blank/cold-down), instead of continuing the one above.
func TestTrailingBlankParagraphTakesInput(t *testing.T) {
	tests := []struct {
		name, src string
		at        int
		down      bool
		want      string
	}{
		{"down", "p1.\n\n", 3, true, "p1.\n\nnext\n"},
		{"on blank line", "p1.\n\n", 4, false, "p1.\n\nnext\n"},
		{"crlf", "p1.\r\n\r\n", 3, true, "p1.\r\n\r\nnext\r\n"},
		{"after quote", "> a\n\n", 3, true, "> a\n\nnext\n"},
		{"after list", "- a\n\n", 3, true, "- a\n\nnext\n"},
	}
	for _, tt := range tests {
		for _, ime := range []bool{false, true} {
			name := tt.name
			if ime {
				name += " ime"
			}
			t.Run(name, func(t *testing.T) {
				d := NewMarkdownDocument(tt.src)
				tester, _ := richTester(t, d)
				d.selectFromSource(tt.at, tt.at)
				tester.Frame()
				if tt.down {
					tester.Key(0, ui.KeyDown)
					tester.Frame()
				}
				if ime {
					tester.Compose("next", 4)
					tester.Frame()
				}
				tester.Type("next")
				tester.Frame()
				assertSource(t, d, tt.want)
				if got := d.Text().ParagraphCount(); got != 2 {
					t.Fatalf("%d paragraphs, want 2", got)
				}
				tester.Key(ui.Cmd, ui.KeyZ)
				tester.Frame()
				assertSource(t, d, tt.src)
			})
		}
	}
}
