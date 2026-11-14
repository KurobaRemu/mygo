package richtext

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

func assertSelectionProjection(t *testing.T, d *Document) {
	t.Helper()

	d.lock()
	sel := d.selection
	gotAnchor, gotCaret := d.anchor, d.caret
	wantAnchor, wantCaret := d.docSelectionForSourceLocked(sel)
	sourceLen := d.sourceLenLocked()
	d.mu.Unlock()

	if sel.Anchor.Offset < 0 || sel.Anchor.Offset > sourceLen ||
		sel.Caret.Offset < 0 || sel.Caret.Offset > sourceLen {
		t.Fatalf("source selection %+v outside source length %d", sel, sourceLen)
	}
	if sel.Anchor.Affinity > BoundaryDownstream || sel.Caret.Affinity > BoundaryDownstream {
		t.Fatalf("source selection has invalid affinity: %+v", sel)
	}
	if gotAnchor != wantAnchor || gotCaret != wantCaret {
		t.Fatalf("doc selection cache %d-%d, source selection %+v projects to %d-%d",
			gotAnchor, gotCaret, sel, wantAnchor, wantCaret)
	}
	d.lock()
	round := d.sourceSelectionForDocLocked(gotAnchor, gotCaret)
	roundAnchor, roundCaret := d.docSelectionForSourceLocked(round)
	roundAgain := d.sourceSelectionForDocLocked(roundAnchor, roundCaret)
	d.mu.Unlock()
	if roundAnchor != gotAnchor || roundCaret != gotCaret || roundAgain != round {
		t.Fatalf("source selection %+v did not stabilize: doc %d-%d -> source %+v -> doc %d-%d -> source %+v",
			sel, gotAnchor, gotCaret, round, roundAnchor, roundCaret, roundAgain)
	}
}

func TestSourceSelectionProjectionInvariant(t *testing.T) {
	t.Run("navigation", func(t *testing.T) {
		d := NewMarkdownDocument("alpha **bold** omega\n")
		tt, _ := richTester(t, d)
		d.selectFromSource(14, 14)
		tt.Frame()
		assertSelectionProjection(t, d)

		for _, key := range []ui.Key{ui.KeyRight, ui.KeyLeft, ui.KeyUp, ui.KeyDown} {
			tt.Key(0, key)
			tt.Frame()
			assertSelectionProjection(t, d)
		}
		tt.Key(ui.Cmd, ui.KeyRight)
		tt.Frame()
		assertSelectionProjection(t, d)
	})

	t.Run("formatting and typing", func(t *testing.T) {
		d := NewMarkdownDocument("alpha **bold** omega\n")
		tt, _ := richTester(t, d)
		d.selectFromSource(14, 14)
		tt.Frame()
		assertSelectionProjection(t, d)

		tt.Key(ui.Cmd, ui.KeyB)
		tt.Frame()
		assertSelectionProjection(t, d)
		tt.Type("X")
		tt.Frame()
		assertSelectionProjection(t, d)
	})

	t.Run("delimiter gaps", func(t *testing.T) {
		src := "pre **bold** post\n"
		d := NewMarkdownDocument(src)
		open := strings.Index(src, "**")
		close := strings.LastIndex(src, "**")
		for _, source := range []int{open + 1, close + 1, close + 2} {
			d.selectFromSource(source, source)
			assertSelectionProjection(t, d)
		}
	})

	t.Run("atomic node", func(t *testing.T) {
		src := "a ![alt](img.png) b\n"
		d := NewMarkdownDocument(src)
		d.selectFromSource(strings.Index(src, "![")+2, strings.Index(src, "![")+2)
		assertSelectionProjection(t, d)
	})

	t.Run("empty list item", func(t *testing.T) {
		d := NewMarkdownDocument("- \n")
		d.selectFromSource(2, 2)
		assertSelectionProjection(t, d)
	})

	t.Run("undo and redo", func(t *testing.T) {
		d := NewMarkdownDocument("alpha beta\n")
		d.selectFromSource(6, 10)
		d.InsertText("gamma")
		assertSelectionProjection(t, d)
		d.Undo()
		assertSelectionProjection(t, d)
		d.Redo()
		assertSelectionProjection(t, d)
	})

	t.Run("IME composition", func(t *testing.T) {
		d, v := markerView("a **bold** c\n")
		d.SetSelection(2, 6)
		assertSelectionProjection(t, d)
		v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
		assertSelectionProjection(t, d)
		v.ReplaceText(nil, "你")
		assertSelectionProjection(t, d)
	})

	t.Run("source mode", func(t *testing.T) {
		src := "pre **alpha** post\n"
		d := NewMarkdownDocument(src)
		source := strings.Index(src, "**") + 1
		d.selectFromSource(source, source)
		s := NewSourceModeSession(d)
		if err := s.Enter(); err != nil {
			t.Fatal(err)
		}
		assertSelectionProjection(t, d)
		sel := s.State.Selection()
		want := s.eol.fromSource(source)
		if sel.Anchor != want || sel.Head != want {
			t.Fatalf("source buffer selection %+v, want %d", sel, want)
		}
		s.State.SetSelections(editor.Selection{Anchor: want, Head: want})
		s.publishSourceSelection(false)
		assertSelectionProjection(t, d)
		if err := s.Leave(); err != nil {
			t.Fatal(err)
		}
		assertSelectionProjection(t, d)
	})
}
