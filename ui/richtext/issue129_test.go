package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestIssue129HeldParagraphTakesInputOnItsOwnLine pins what the user-style
// fuzz found after issue 127: a fence followed by blank lines holds a tail
// paragraph whose preview line is not the preview's end. Down into the line
// past the fence, Shift+Enter, a rune, Left, Enter and Delete leave the
// caret drawn in the empty paragraph the bytes spell, and the probe rune
// then opens a second paragraph after that line.
func TestIssue129HeldParagraphTakesInputOnItsOwnLine(t *testing.T) {
	d := NewMarkdownDocument("```go\ncode\n```\n")
	tt, _ := richTester(t, d)
	tt.Frame()

	tt.Key(0, ui.KeyRight)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	tt.Type("\u8bd5")
	tt.Frame()
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	assertSource(t, d, "```go\ncode\n```\n\n\n\n\u8bd5\n")
	assertCaretIsInput(t, d, tt, "after the split above the line")

	tt.Key(0, ui.KeyDelete)
	tt.Frame()
	assertSource(t, d, "```go\ncode\n```\n\n\n")
	assertCaretIsInput(t, d, tt, "after deleting the emptied tail paragraph")

	// The deletion leaves the caret in the paragraph the view holds, and the
	// next rune goes there. The blank lines above it are the paragraph's own
	// separator: written past the line it starts on, the rune opened a second
	// paragraph while the caret stayed drawn in the first.
	tt.Type("q")
	tt.Frame()
	assertSource(t, d, "```go\ncode\n```\n\nq\n")
	assertCaretIsInput(t, d, tt, "after typing in the held paragraph")
}
