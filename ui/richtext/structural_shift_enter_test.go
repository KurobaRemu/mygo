package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestShiftEnterInCodeBlockInsertsNewline(t *testing.T) {
	const source = "```go\nalpha beta\n```\n"
	tests := []struct {
		name   string
		offset int
		want   string
	}{
		{"start", 0, "```go\n\nXalpha beta\n```\n"},
		{"middle", 5, "```go\nalpha\nX beta\n```\n"},
		{"end", 10, "```go\nalpha beta\nX\n```\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(source)
			tester, _ := richTester(t, d)
			at := runeAtText(t, d, "alpha beta", tt.offset)
			d.SetSelection(at, at)
			tester.Frame()
			tester.Key(ui.Shift, ui.KeyEnter)
			tester.Type("X")
			assertSource(t, d, tt.want)
		})
	}
}
