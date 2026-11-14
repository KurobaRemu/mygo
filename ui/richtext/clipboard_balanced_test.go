package richtext

import (
	"testing"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
)

// sourceSelection selects the source bytes from to to, as a drag over the
// text there does.
func sourceSelection(from, to int) SourceSelection {
	return SourceSelection{
		Anchor: SourcePoint{Offset: from, Affinity: BoundaryUpstream},
		Caret:  SourcePoint{Offset: to, Affinity: BoundaryDownstream},
	}
}

// TestCopyNestedBalancedSlice covers the minimal balanced source slice of a
// selection that covers the whole text of nested inline elements: the
// markers of the elements come from the source as they are, opening
// outermost first and closing innermost first, so the slice parses as the
// same nesting.
func TestCopyNestedBalancedSlice(t *testing.T) {
	tests := []struct {
		name string
		src  string
		from int
		to   int
		want string
	}{
		{"strike around strong", "a ~~**b**~~ c\n", 6, 7, "~~**b**~~"},
		{"strong around emphasis", "**a *b* c**\n", 5, 6, "***b***"},
		{"emphasis around strong", "*a **b** c*\n", 5, 6, "***b***"},
		{"triple emphasis", "x ***y*** z\n", 5, 6, "***y***"},
		{"strong around link label", "[**t**](u)\n", 3, 4, "[**t**](u)"},
		{"strong around emphasis in strike", "a ~~**b *c* d**~~ e\n", 9, 10, "~~***c***~~"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			ttt, _ := richTester(t, d)
			d.SetSourceSelection(sourceSelection(tt.from, tt.to))
			ttt.Frame()
			if got := d.markdownSlice(d.span()); got != tt.want {
				t.Fatalf("markdownSlice = %q, want %q", got, tt.want)
			}
			ttt.Key(ui.Cmd, ui.KeyC)
			ttt.Frame()
			plain, err := ttt.ClipboardData().Read(transfer.Text)
			if err != nil {
				t.Fatal(err)
			}
			if string(plain) != tt.want {
				t.Fatalf("copied text = %q, want %q", plain, tt.want)
			}
		})
	}
}

// TestCutNestedBalancedSlice covers the clipboard of a cut over nested
// inline elements, and the source it leaves. The clipboard is the same
// balanced slice copy makes; the cut deletes the selected bytes and keeps
// the empty strong markers, as Backspace does.
func TestCutNestedBalancedSlice(t *testing.T) {
	tests := []struct {
		name string
		src  string
		from int
		to   int
		cut  string
		left string
	}{
		{"strike around strong", "a ~~**b**~~ c\n", 6, 7, "~~**b**~~", "a ~~****~~ c\n"},
		{"triple emphasis", "x ***y*** z\n", 5, 6, "***y***", "x ****** z\n"},
		{"strong around emphasis", "**a *b* c**\n", 5, 6, "***b***", "**a ** c**\n"},
		{"emphasis around strong", "*a **b** c*\n", 5, 6, "***b***", "*a **** c*\n"},
		{"strong around link label", "[**t**](u)\n", 3, 4, "[**t**](u)", "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			ttt, _ := richTester(t, d)
			d.SetSourceSelection(sourceSelection(tt.from, tt.to))
			ttt.Frame()
			ttt.Key(ui.Cmd, ui.KeyX)
			ttt.Frame()
			plain, err := ttt.ClipboardData().Read(transfer.Text)
			if err != nil {
				t.Fatal(err)
			}
			if string(plain) != tt.cut {
				t.Fatalf("cut text = %q, want %q", plain, tt.cut)
			}
			if got := d.Source(); got != tt.left {
				t.Fatalf("source after cut = %q, want %q", got, tt.left)
			}
			ttt.Key(ui.Cmd, ui.KeyZ)
			ttt.Frame()
			if got := d.Source(); got != tt.src {
				t.Fatalf("source after undo = %q, want %q", got, tt.src)
			}
			ttt.Key(ui.Cmd|ui.Shift, ui.KeyZ)
			ttt.Frame()
			if got := d.Source(); got != tt.left {
				t.Fatalf("source after redo = %q, want %q", got, tt.left)
			}
		})
	}
}
