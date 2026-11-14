package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// Enter on a live-typed fence leaves the caret on the closing fence
// boundary. Typora treats that boundary as the code block's empty line:
// typing pushes the closing fence down and writes the text inside the
// block. Replay p0/live-syntax/after-enter/fence.
func TestLiveFenceTypingAtClosingBoundary(t *testing.T) {
	doc := NewMarkdownDocument("alpha\n")
	tt, _ := richTester(t, doc)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	for _, r := range "```body" {
		tt.Type(string(r))
		tt.Frame()
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	// Typora keeps the caret on the closing fence boundary after Enter.
	if sel := doc.SourceSelection(); sel.Caret.Offset != 15 {
		t.Fatalf("caret after Enter = %d, want 15", sel.Caret.Offset)
	}
	for _, r := range "next" {
		tt.Type(string(r))
		tt.Frame()
	}
	tt.Key(ui.Cmd, ui.KeyUp)
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyDown)
	tt.Frame()

	const want = "alpha\n\n```body\nnext\n```\n\n"
	if got := doc.Source(); got != want {
		t.Fatalf("source = %q, want %q", got, want)
	}
	if sel := doc.SourceSelection(); sel.Caret.Offset != len("alpha\n\n```body\nnext") {
		t.Fatalf("caret = %d, want %d", sel.Caret.Offset, len("alpha\n\n```body\nnext"))
	}
}

// An IME commit on the same boundary takes the document's write path too,
// so it writes the line ending before the closing fence.
func TestLiveFenceIMECommitAtClosingBoundary(t *testing.T) {
	doc := NewMarkdownDocument("alpha\n")
	tt, _ := richTester(t, doc)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	for _, r := range "```body" {
		tt.Type(string(r))
		tt.Frame()
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Compose("next", 4)
	tt.Frame()
	tt.Type("next")
	tt.Frame()

	const want = "alpha\n\n```body\nnext\n```\n\n"
	if got := doc.Source(); got != want {
		t.Fatalf("source = %q, want %q", got, want)
	}
	if sel := doc.SourceSelection(); sel.Caret.Offset != len("alpha\n\n```body\nnext") {
		t.Fatalf("caret = %d, want %d", sel.Caret.Offset, len("alpha\n\n```body\nnext"))
	}
}
