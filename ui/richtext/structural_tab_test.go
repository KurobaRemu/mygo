package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// At a content start that cannot indent, Typora still writes a tab: it
// keeps it text behind an invisible U+200B.
func TestTabAtUnindentableBlockStartWritesTab(t *testing.T) {
	tests := []struct {
		name   string
		source string
		tabbed string
		want   string
	}{
		{"paragraph", "alpha beta\n", "\u200b\talpha beta\n", "\u200b\tXalpha beta\n"},
		{"quote", "> alpha beta\n", "> \u200b\talpha beta\n", "> \u200b\tXalpha beta\n"},
		{"bullet", "- alpha beta\n", "- \u200b\talpha beta\n", "- \u200b\tXalpha beta\n"},
		{"ordered", "1. alpha beta\n", "1. \u200b\talpha beta\n", "1. \u200b\tXalpha beta\n"},
		{"task", "- [ ] alpha beta\n", "- [ ] \u200b\talpha beta\n", "- [ ] \u200b\tXalpha beta\n"},
		{"nested first item", "- parent\n  - alpha\n  - beta\n", "- parent\n  - \u200b\talpha\n  - beta\n", "- parent\n  - \u200b\tXalpha\n  - beta\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.source)
			tester, _ := richTester(t, d)
			at := runeAtText(t, d, "alpha", 0)
			d.SetSelection(at, at)
			tester.Frame()
			if d.CanUndo() {
				t.Fatal("document can undo before Tab")
			}
			tester.Key(0, ui.KeyTab)
			assertSource(t, d, tt.tabbed)
			if !d.CanUndo() {
				t.Fatal("Tab wrote no undo step")
			}
			tester.Type("X")
			assertSource(t, d, tt.want)
		})
	}
}

func TestTabStillInsertsWhereTextCanIndent(t *testing.T) {
	tests := []struct {
		name   string
		source string
		offset int
		want   string
	}{
		{"paragraph middle", "alpha beta\n", 5, "alpha\tX beta\n"},
		{"bullet first middle", "- alpha beta\n", 5, "- alpha\tX beta\n"},
		{"heading start", "# alpha beta\n", 0, "# \tXalpha beta\n"},
		{"code start", "```go\nalpha beta\n```\n", 0, "```go\n\tXalpha beta\n```\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.source)
			tester, _ := richTester(t, d)
			at := runeAtText(t, d, "alpha beta", tt.offset)
			d.SetSelection(at, at)
			tester.Frame()
			tester.Key(0, ui.KeyTab)
			tester.Type("X")
			assertSource(t, d, tt.want)
		})
	}
}

func TestTabKeepsTheSelection(t *testing.T) {
	tests := []struct {
		name          string
		source        string
		mods          ui.Modifiers
		anchor, caret int
		want          string
		wantA, wantC  int
	}{
		{"outdent", "- parent\n  - child\n", ui.Shift, 13, 18, "- parent\n- child\n", 11, 16},
		{"outdent backward", "- parent\n  - child\n", ui.Shift, 18, 13, "- parent\n- child\n", 16, 11},
		{"outdent inside", "- parent\n  - child\n", ui.Shift, 14, 16, "- parent\n- child\n", 12, 14},
		{"indent", "- parent\n- child\n", 0, 11, 16, "- parent\n  - child\n", 13, 18},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.source)
			tester, _ := richTester(t, d)
			d.SetSourceSelection(SourceSelection{Anchor: sourcePoint(tt.anchor, BoundaryUpstream), Caret: sourcePoint(tt.caret, BoundaryDownstream)})
			tester.Frame()
			tester.Key(tt.mods, ui.KeyTab)
			assertSource(t, d, tt.want)
			if got := d.SourceSelection(); got.Anchor.Offset != tt.wantA || got.Caret.Offset != tt.wantC {
				t.Fatalf("selection = [%d,%d), want [%d,%d)", got.Anchor.Offset, got.Caret.Offset, tt.wantA, tt.wantC)
			}
		})
	}
}

// Shift-Tab that lifts a block out of its container keeps the caret in the
// content it was on, as Typora does (record 2026-11-10, difffuzz seed47).
func TestShiftTabLiftKeepsCaretOnContent(t *testing.T) {
	t.Run("quote", func(t *testing.T) {
		d := NewMarkdownDocument("> alpha\n")
		tester, _ := richTester(t, d)
		at := runeAtText(t, d, "alpha", 5)
		d.SetSelection(at, at)
		tester.Frame()
		tester.Key(ui.Shift, ui.KeyTab)
		assertSource(t, d, "alpha\n")
		if got := d.SourceSelection().Caret.Offset; got != 5 {
			t.Fatalf("caret = %d, want 5", got)
		}
	})
	t.Run("task item", func(t *testing.T) {
		d := NewMarkdownDocument("- [ ] alpha\n- [ ] beta\n")
		tester, _ := richTester(t, d)
		at := runeAtText(t, d, "beta", 4)
		d.SetSelection(at, at)
		tester.Frame()
		tester.Type(" - alqj")
		tester.Key(ui.Shift, ui.KeyTab)
		assertSource(t, d, "- [ ] alpha\n\nbeta - alqj\n")
		if got := d.SourceSelection().Caret.Offset; got != 24 {
			t.Fatalf("caret = %d, want 24", got)
		}
	})
}

func TestSelectionInHiddenBlockPrefixSnapsToContent(t *testing.T) {
	tests := []struct {
		source string
		at     int
		want   int
	}{
		{"## alpha\n", 0, 3},
		{"## alpha\n", 2, 3},
		{"- parent\n  - child\n", 11, 13},
		{"> quote\n", 1, 2},
		{"alpha\n\n## beta\n", 6, 6},
	}
	for _, tt := range tests {
		d := NewMarkdownDocument(tt.source)
		d.SetSourceSelection(sourceCaret(tt.at))
		if got := d.SourceSelection().Caret.Offset; got != tt.want {
			t.Errorf("%q at %d: caret %d, want %d", tt.source, tt.at, got, tt.want)
		}
	}
}

// Indenting rewrites the marker bytes only. A caret that a previous key
// moved inside a hidden closing marker pair stays inside it; deriving the
// caret back from the visible text would drop it before the pair (record
// 2026-11-10, seed4/c5).
func TestTabKeepsCaretInsideHiddenMarkers(t *testing.T) {
	d := NewMarkdownDocument("- alpha\n- beta\n")
	tester, _ := richTester(t, d)
	at := runeAtText(t, d, "beta", 4)
	d.SetSelection(at, at)
	tester.Frame()
	tester.Type("1. ")
	tester.Frame()
	tester.Type("**gk**")
	tester.Frame()
	tester.Key(0, ui.KeyLeft)
	tester.Frame()
	tester.Key(0, ui.KeyTab)
	assertSource(t, d, "- alpha\n  - beta1. **gk**\n")
	got := d.SourceSelection().Caret
	if got.Offset != 24 || got.Affinity != BoundaryUpstream {
		t.Fatalf("caret = %d/%v, want 24/upstream", got.Offset, got.Affinity)
	}
}
