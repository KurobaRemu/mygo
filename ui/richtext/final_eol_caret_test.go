package richtext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

// A source selection at or inside the document's final line ending is the
// last block's content end: there is no paragraph after the bytes. Typing
// there writes before the ending, as it does at the caret an open document
// starts with.
func TestFinalEOLTypesAtLastBlockEnd(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
		at   int
	}{
		{"lf", "alpha\n", "alphaX\n", 6},
		{"without final eol", "alpha", "alphaX", 6},
		{"crlf", "alpha\r\n", "alphaX\r\n", 6},
		{"blank line", "a\n\nb\n", "a\n\nbX\n", 5},
		{"heading", "# h\n", "# hX\n", 4},
		{"list", "- a\n", "- aX\n", 4},
		{"empty list item", "- a\n- \n", "- a\n- X\n", 7},
		{"quote", "> a\n", "> aX\n", 4},
		{"code block", "```\ncode\n```\n", "```\ncodeX\n```\n", 9},
		{"empty", "", "X", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			rt, _ := richTester(t, d)
			d.selectFromSource(len(tt.src), len(tt.src))
			rt.Frame()
			rt.Type("X")
			rt.Frame()
			if got := d.Source(); got != tt.want {
				t.Fatalf("wrote %q, want %q", got, tt.want)
			}
			sel := d.SourceSelection()
			if sel.Anchor.Offset != tt.at || sel.Caret.Offset != tt.at {
				t.Fatalf("selection = %+v, want caret at %d", sel, tt.at)
			}
		})
	}
}

func TestFinalEOLInputAtLastBlockEnd(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		name := map[string]string{"\n": "lf", "\r\n": "crlf"}[eol]
		t.Run(name+"/type", func(t *testing.T) {
			d := NewMarkdownDocument("alpha" + eol)
			tt, _ := richTester(t, d)
			d.selectFromSource(len("alpha"+eol), len("alpha"+eol))
			tt.Frame()
			tt.Type("X")
			tt.Frame()
			if got, want := d.Source(), "alphaX"+eol; got != want {
				t.Fatalf("wrote %q, want %q", got, want)
			}
		})
		t.Run(name+"/paste", func(t *testing.T) {
			d := NewMarkdownDocument("alpha" + eol)
			tt, _ := richTester(t, d)
			d.selectFromSource(len("alpha"+eol), len("alpha"+eol))
			tt.Frame()
			tt.SetClipboard("X")
			tt.Command("paste")
			tt.Frame()
			if got, want := d.Source(), "alphaX"+eol; got != want {
				t.Fatalf("pasted %q, want %q", got, want)
			}
		})
		t.Run(name+"/ime", func(t *testing.T) {
			d := NewMarkdownDocument("alpha" + eol)
			tt, _ := richTester(t, d)
			d.selectFromSource(len("alpha"+eol), len("alpha"+eol))
			tt.Frame()
			tt.Compose("hui", 3)
			tt.Frame()
			tt.Type("灰")
			tt.Frame()
			if got, want := d.Source(), "alpha灰"+eol; got != want {
				t.Fatalf("composed %q, want %q", got, want)
			}
		})
	}
}

// Enter at the final EOL opens the view-only empty paragraph at the last
// block's content end. The first input materializes the separator bytes and
// the content in one step, so a list item that Enter emptied becomes the
// paragraph after the list instead of extending the item.
func TestFinalEOLEnterThenType(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		anchor int
		want   string
		at     int
	}{
		{"lf", "alpha\n", 5, "alpha\n\nX\n", 8},
		{"without final eol", "alpha", 5, "alpha\n\nX", 8},
		{"crlf", "alpha\r\n", 5, "alpha\r\n\r\nX\r\n", 10},
		{"blank line", "a\n\nb\n", 4, "a\n\nb\n\nX\n", 7},
		{"heading", "# h\n", 3, "# h\n\nX\n", 6},
		{"list", "- a\n", 3, "- a\n- X\n", 7},
		{"empty list item", "- a\n- \n", 3, "- a\n\nX\n", 6},
		{"quote", "> a\n", 3, "> a\n>\n> X\n", 9},
		{"empty", "", 0, "\n\nX", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			rt, _ := richTester(t, d)
			d.selectFromSource(len(tt.src), len(tt.src))
			rt.Frame()
			rt.Key(0, ui.KeyEnter)
			rt.Frame()
			d.lock()
			tr := d.activeView().transient
			anchor := -1
			if tr != nil {
				anchor = tr.anchor.Offset
			}
			d.mu.Unlock()
			if tr == nil {
				t.Fatal("Enter at the document end opened no transient paragraph")
			}
			if anchor != tt.anchor {
				t.Fatalf("transient anchor = %d, want %d", anchor, tt.anchor)
			}
			rt.Type("X")
			rt.Frame()
			if got := d.Source(); got != tt.want {
				t.Fatalf("wrote %q, want %q", got, tt.want)
			}
			sel := d.SourceSelection()
			if sel.Anchor.Offset != tt.at || sel.Caret.Offset != tt.at {
				t.Fatalf("selection = %+v, want caret at %d", sel, tt.at)
			}
		})
	}
}

func TestFinalEOLTransientInputMethods(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"paragraph paste", "alpha\n", "alpha\n\nX\n"},
		{"paragraph ime", "alpha\n", "alpha\n\n灰\n"},
		{"list paste", "- a\n- \n", "- a\n\nX\n"},
		{"list ime", "- a\n- \n", "- a\n\n灰\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			rt, _ := richTester(t, d)
			d.selectFromSource(len(tt.src), len(tt.src))
			rt.Frame()
			rt.Key(0, ui.KeyEnter)
			rt.Frame()
			switch {
			case len(tt.name) >= 5 && tt.name[len(tt.name)-3:] == "ime":
				rt.Compose("hui", 3)
				rt.Frame()
				rt.Type("灰")
			default:
				rt.SetClipboard("X")
				rt.Command("paste")
			}
			rt.Frame()
			if got := d.Source(); got != tt.want {
				t.Fatalf("wrote %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFinalEOLUndoRedoContinue(t *testing.T) {
	d := NewMarkdownDocument("alpha\n")
	tt, _ := richTester(t, d)
	d.selectFromSource(6, 6)
	tt.Frame()
	tt.Type("X")
	tt.Frame()
	if got := d.Source(); got != "alphaX\n" {
		t.Fatalf("typed %q", got)
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if got := d.Source(); got != "alpha\n" {
		t.Fatalf("undo wrote %q", got)
	}
	if sel := d.SourceSelection(); sel.Anchor.Offset != 5 || sel.Caret.Offset != 5 {
		t.Fatalf("undo selection = %+v, want caret at 5", sel)
	}
	tt.Key(ui.Cmd|ui.Shift, ui.KeyZ)
	tt.Frame()
	if got := d.Source(); got != "alphaX\n" {
		t.Fatalf("redo wrote %q", got)
	}
	tt.Type("Y")
	tt.Frame()
	if got := d.Source(); got != "alphaXY\n" {
		t.Fatalf("continued %q", got)
	}
}

func TestFinalEOLUndoRedoTransient(t *testing.T) {
	d := NewMarkdownDocument("- a\n- \n")
	tt, _ := richTester(t, d)
	d.selectFromSource(7, 7)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Type("X")
	tt.Frame()
	if got := d.Source(); got != "- a\n\nX\n" {
		t.Fatalf("typed %q", got)
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	tt.Frame()
	if got := d.Source(); got != "- a\n" {
		t.Fatalf("undo wrote %q", got)
	}
	d.lock()
	tr := d.activeView().transient
	anchor := -1
	if tr != nil {
		anchor = tr.anchor.Offset
	}
	d.mu.Unlock()
	if tr == nil || anchor != 3 {
		t.Fatalf("undo transient = %+v, want anchor 3", tr)
	}
	tt.Key(ui.Cmd|ui.Shift, ui.KeyZ)
	tt.Frame()
	if got := d.Source(); got != "- a\n\nX\n" {
		t.Fatalf("redo wrote %q", got)
	}
}

func TestFinalEOLSaveReopenContinue(t *testing.T) {
	for _, c := range []struct {
		name, src, eol string
	}{
		{"lf", "alpha\n", "\n"},
		{"crlf", "alpha\r\n", "\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument(c.src)
			tt, _ := richTester(t, d)
			d.selectFromSource(len(c.src), len(c.src))
			tt.Frame()
			tt.Type("X")
			tt.Frame()
			path := filepath.Join(t.TempDir(), "doc.md")
			if err := d.SaveMarkdown(path); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(saved), "alphaX"+c.eol; got != want {
				t.Fatalf("saved %q, want %q", got, want)
			}
			reopened := NewMarkdownDocument(string(saved))
			rt, _ := richTester(t, reopened)
			rt.Frame()
			rt.Type("Y")
			rt.Frame()
			if got, want := reopened.Source(), "alphaXY"+c.eol; got != want {
				t.Fatalf("continued %q, want %q", got, want)
			}
			sel := reopened.SourceSelection()
			if sel.Anchor.Offset != 7 || sel.Caret.Offset != 7 {
				t.Fatalf("selection = %+v, want caret at 7", sel)
			}
		})
	}
}

func TestFinalEOLEnterSaveReopen(t *testing.T) {
	d := NewMarkdownDocument("alpha\r\n")
	tt, _ := richTester(t, d)
	d.selectFromSource(7, 7)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Type("X")
	tt.Frame()
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := d.SaveMarkdown(path); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(saved), "alpha\r\n\r\nX\r\n"; got != want {
		t.Fatalf("saved %q, want %q", got, want)
	}
	reopened := NewMarkdownDocument(string(saved))
	rt, _ := richTester(t, reopened)
	rt.Frame()
	if got := reopened.Source(); got != string(saved) {
		t.Fatalf("reopened source %q", got)
	}
}
