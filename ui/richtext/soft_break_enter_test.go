package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// Enter beside a line break inside a paragraph turns the break into a block
// boundary (Typora 1.14.15, oracle followup/soft-break/*).
func TestEnterBesideLineBreakSplitsTheBlock(t *testing.T) {
	tests := []struct {
		src   string
		at    int
		want  string
		caret int
	}{
		{"a\nb\n", 1, "a\n\nb\n", 3},
		{"a\nb\n", 2, "a\n\nb\n", 3},
		{"> a\n> b\n", 3, "> a\n>\n> b\n", 8},
		{"> a\n> b\n", 6, "> a\n>\n> b\n", 8},
		{"> a\nb\n", 3, "> a\n>\n> b\n", 8},
		{"> > a\n> > b\n", 5, "> > a\n> >\n> > b\n", 14},
		{"- a\n  b\n", 3, "- a\n- b\n", 6},
		{"- a\n  b\n", 6, "- a\n- b\n", 6},
		{"1. a\n   b\n2. c\n", 4, "1. a\n2. b\n3. c\n", 8},
		{"a  \nb\n", 1, "a\n\nb\n", 3},
		{"a\r\nb\r\n", 1, "a\r\n\r\nb\r\n", 5},
		{"**a**\nb\n", 5, "**a**\n\nb\n", 7},
	}
	for _, tt := range tests {
		d := NewMarkdownDocument(tt.src)
		tester, _ := richTester(t, d)
		d.selectFromSource(tt.at, tt.at)
		tester.Frame()
		tester.Key(0, ui.KeyEnter)
		tester.Frame()
		if got := d.Source(); got != tt.want {
			t.Errorf("%q at %d: wrote %q, want %q", tt.src, tt.at, got, tt.want)
			continue
		}
		if got := d.SourceSelection().Caret.Offset; got != tt.caret {
			t.Errorf("%q at %d: caret %d, want %d", tt.src, tt.at, got, tt.caret)
		}
		tester.Key(ui.Cmd, ui.KeyZ)
		tester.Frame()
		if got := d.Source(); got != tt.src {
			t.Errorf("%q at %d: undo left %q", tt.src, tt.at, got)
		}
	}
}
