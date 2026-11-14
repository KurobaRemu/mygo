package richtext

import (
	"os"
	"strings"
	"testing"
)

func typeMarkdown(d *Document, at int, s string) error {
	var err error
	d.SetSelection(at, at)
	a := d.TypingAttributes()
	d.Edit(func(e *Edit) {
		e.ReplaceString(at, at, s, a)
		err = e.Err()
	})
	return err
}

func TestMarkdownBoldEnd(t *testing.T) {
	d := NewMarkdownDocument("a **bold** end\n")
	at := strings.Index(d.Text().String(), "bold") + len("bold")
	d.SetSelection(at, at)
	var err error
	d.Edit(func(e *Edit) {
		e.ReplaceString(at, at, "X", Attributes{Weight: 700})
		err = e.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Source(); got != "a **boldX** end\n" {
		t.Fatalf("source %q", got)
	}
	if _, caret := d.Selection(); caret != at+1 {
		t.Fatalf("caret %d", caret)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	d.Undo()
	if got := d.Source(); got != "a **bold** end\n" {
		t.Fatalf("undo %q", got)
	}
	if _, caret := d.Selection(); caret != at {
		t.Fatalf("undo caret %d", caret)
	}
	d.Redo()
	if got := d.Source(); got != "a **boldX** end\n" {
		t.Fatalf("redo %q", got)
	}
}

func TestMarkdownTypingAtEdges(t *testing.T) {
	for _, c := range []struct {
		src, after, typed, want string
		before                  bool
	}{
		{"a **b** c\n", "b", "X", "a **bX** c\n", false},
		{"a **b** c\n", "b", "X", "a X**b** c\n", true},
		{"**b** c\n", "b", "X", "**Xb** c\n", true},
		{"a *b* c\n", "b", "X", "a *bX* c\n", false},
		{"a _b_ c\n", "b", "X", "a _bX_ c\n", false},
		{"a ~~b~~ c\n", "b", "X", "a ~~bX~~ c\n", false},
		{"a <u>b</u> c\n", "b", "X", "a <u>bX</u> c\n", false},
		{"a `b` c\n", "b", "X", "a `b`X c\n", false},
		{"a [b](u) c\n", "b", "X", "a [b](u)X c\n", false},
		{"a [b](u) c\n", "b", "X", "a X[b](u) c\n", true},
		{"a **x *b*** c\n", "b", "X", "a **x *bX*** c\n", false},
		{"a \\* c\n", "*", "X", "a \\*X c\n", false},
		{"a \\* c\n", "*", "X", "a X\\* c\n", true},
		{"a &amp; c\n", "&", "X", "a &amp;X c\n", false},
		{"a b c\n", "b", "*", "a b* c\n", false},
		{"a *b c\n", "b", "*", "a *b* c\n", false},
		{"a b c\n", "b", "中文", "a b中文 c\n", false},
		{"> a\n> b\n", "a", "X", "> aX\n> b\n", false},
		{"- a\n- b\n", "b", "X", "- a\n- bX\n", false},
		{"```\ncode\n```\n", "code", "*", "```\ncode*\n```\n", false},
		{"# h\n\np\n", "h", "X", "# hX\n\np\n", false},
	} {
		d := NewMarkdownDocument(c.src)
		at := strings.Index(d.Text().String(), c.after)
		if !c.before {
			at += len([]rune(c.after))
		}
		if err := typeMarkdown(d, at, c.typed); err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got := d.Source(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
		wantCaret := at + len([]rune(c.typed))
		if c.src == "a *b c\n" {
			wantCaret = at - 1
		}
		if _, caret := d.Selection(); caret != wantCaret {
			t.Errorf("%q: caret %d, want %d", c.src, caret, wantCaret)
		}
		if err := d.CheckSource(); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

func TestMarkdownTypingAttributesChooseSide(t *testing.T) {
	d := NewMarkdownDocument("a **bold** end\n")
	at := strings.Index(d.Text().String(), "bold") + len("bold")
	d.SetSelection(at, at)
	d.Edit(func(e *Edit) { e.ReplaceString(at, at, "X", Attributes{}) })
	if got := d.Source(); got != "a **bold**X end\n" {
		t.Fatalf("source %q", got)
	}
}

func TestMarkdownDelete(t *testing.T) {
	for _, c := range []struct {
		src, del, want string
	}{
		{"a **bold** c\n", "ol", "a **bd** c\n"},
		{"a **bold** c\n", "bold", "a  c\n"},
		{"a **b**c d\n", "bc", "a  d\n"},
		{"x **a** **b** y\n", "a b", "x  y\n"},
		{"a \\* &amp; c\n", "* &", "a  c\n"},
		{"a <span>x</span> c\n", "x", "a <span></span> c\n"},
		{"a\nb\n", "a\nb", "\n"},
	} {
		d := NewMarkdownDocument(c.src)
		at := strings.Index(d.Text().String(), c.del)
		var err error
		d.Edit(func(e *Edit) {
			e.ReplaceString(at, at+len([]rune(c.del)), "", Attributes{})
			err = e.Err()
		})
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got := d.Source(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.src, got, c.want)
		}
		if err := d.CheckSource(); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}

func TestMarkdownAcceptsStructuralEdit(t *testing.T) {
	tests := []struct {
		name string
		fn   func(e *Edit)
		want string
	}{
		{"enter", func(e *Edit) { e.ReplaceString(1, 1, "\n", Attributes{}) }, "# a\n\n\n\nb\n"},
		{"delete", func(e *Edit) { e.ReplaceString(0, 3, "", Attributes{}) }, "\n"},
		{"format", func(e *Edit) { e.Format(0, 1, func(a *Attributes) { a.Italic = true }) }, "# *a*\n\nb\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument("# a\n\nb\n")
			var err error
			d.Edit(func(e *Edit) {
				tt.fn(e)
				err = e.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Source(); got != tt.want {
				t.Fatalf("source %q, want %q", got, tt.want)
			}
			if err := d.CheckSource(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarkdownSourceUnchanged(t *testing.T) {
	for _, src := range []string{"", "a\r\nb  \r\n", "x\n\n\n", "  trailing  ", "# h #\n\n- [ ] t\n"} {
		if got := NewMarkdownDocument(src).Source(); got != src {
			t.Errorf("%q: %q", src, got)
		}
	}
}

func TestMarkdownRollbackKeepsEarlierChanges(t *testing.T) {
	d := NewMarkdownDocument("a **b** c\n")
	var err error
	d.Edit(func(e *Edit) {
		e.ReplaceString(0, 0, "x", Attributes{})
		e.ReplaceString(1, 1, "\n", Attributes{})
		err = e.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Source() != "x\n\na **b** c\n" {
		t.Fatalf("source %q", d.Source())
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownSave(t *testing.T) {
	src := "a\r\n**b**  \n\n\n  "
	path := t.TempDir() + "/note.md"
	d := NewMarkdownDocument(src)
	if err := d.SaveMarkdown(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != src {
		t.Fatalf("saved %q, %v", got, err)
	}
}

func benchmarkMarkdownProjection(b *testing.B, paras int) {
	unit := "Some **bold** and *italic* text with `code`.\n\n"
	d := NewMarkdownDocument(strings.Repeat(unit, paras))
	at := paras / 2 * len(unit)
	d.md.p.Edit(at+2, at+2, "x")
	relinks := d.md.p.Relinks()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, _, _, ok := d.md.project(d.text, 1, relinks); !ok {
			b.Fatal("projection failed")
		}
	}
}

func BenchmarkMarkdownProjection10k(b *testing.B)  { benchmarkMarkdownProjection(b, 10000) }
func BenchmarkMarkdownProjection100k(b *testing.B) { benchmarkMarkdownProjection(b, 100000) }

func TestMarkdownDocumentOpensWithCaretAtEnd(t *testing.T) {
	doc := NewMarkdownDocument("# a\n\nb **c**\n")
	if a, c := doc.Selection(); a != doc.Text().Len() || c != a {
		t.Fatalf("selection %d,%d, want end %d", a, c, doc.Text().Len())
	}
	if sel := doc.SourceSelection(); sel.Anchor.Offset != len("# a\n\nb **c**") || sel.Caret.Offset != sel.Anchor.Offset {
		t.Fatalf("source selection %+v, want after the bold", sel)
	}
}
