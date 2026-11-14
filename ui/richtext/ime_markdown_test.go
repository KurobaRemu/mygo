package richtext

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestIMECompositionReplacesSelectionAtomically(t *testing.T) {
	d, v := markerView("a **bold** c\n")
	d.SetSelection(2, 6)
	beforeSource, beforeVersion := d.Source(), d.Version()
	beforeTyping := d.TypingAttributes()
	v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
	if got := d.Source(); got != beforeSource {
		t.Fatalf("composition changed source to %q", got)
	}
	if d.Version() != beforeVersion || d.CanUndo() {
		t.Fatal("composition wrote the document")
	}
	if got := d.TypingAttributes(); got != beforeTyping {
		t.Fatalf("composition changed typing attributes from %+v to %+v", beforeTyping, got)
	}
	pending := v.pendingSelection
	v.ReplaceText(nil, "你")
	if got := d.Source(); got != "a **你** c\n" {
		t.Fatalf("commit made %q from pending %v", got, pending)
	}
	if len(d.undo) != 1 {
		t.Fatalf("commit made %d undo steps", len(d.undo))
	}
	if a, z := d.Selection(); a != 3 || z != 3 {
		t.Fatalf("caret at %d-%d, shown %q view %d-%d", a, z, d.Text().String(), v.selAnchor, v.caret)
	}
	d.Undo()
	if got := d.Source(); got != beforeSource {
		t.Fatalf("undo made %q", got)
	}
}

func TestIMECompositionCancellationKeepsSelection(t *testing.T) {
	d, v := markerView("a **bold** c\n")
	d.SetSelection(2, 6)
	beforeSource, beforeVersion := d.Source(), d.Version()
	v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
	v.ReplaceText(nil, "")
	if got := d.Source(); got != beforeSource || d.Version() != beforeVersion {
		t.Fatalf("cancel changed %q at %d", got, d.Version())
	}
	if a, z := d.Selection(); a != 2 || z != 6 {
		t.Fatalf("cancel selected %d-%d", a, z)
	}
	if d.CanUndo() {
		t.Fatal("cancel made an undo step")
	}
	v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
	v.SetMarkedText(nil, "", ui.TextInputRange{})
	if got := d.Source(); got != beforeSource || d.Version() != beforeVersion {
		t.Fatalf("empty marked text changed %q at %d", got, d.Version())
	}
	if a, z := d.Selection(); a != 2 || z != 6 {
		t.Fatalf("empty marked text selected %d-%d", a, z)
	}
}

func TestIMECompositionAtDelimiterBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		at   int
		want string
	}{
		{"opening", 5, "IME1 遍**bold**\n"},
		{"closing", 9, "IME1 **bold遍**\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d, v := markerView("IME1 **bold**\n")
			d.SetSelection(tt.at, tt.at)
			v.SetMarkedText(nil, "bian", ui.TextInputRange{Start: 4, End: 4})
			v.ReplaceText(nil, "遍")
			if got := d.Source(); got != tt.want {
				t.Fatalf("commit made %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIMECompositionDisplayAndGeometry(t *testing.T) {
	d, v := markerView("**bold**\n")
	d.SetSelection(0, 0)
	v.rect = ui.Rect{W: 800, H: 400}
	v.width = 800
	v.viewH = 400
	v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
	display := v.displayParagraph(0, Attributes{})
	if got := displayText(display); got != "ni**bold**" {
		t.Fatalf("display %q", got)
	}
	if got, actual := v.TextForRange(ui.TextInputRange{Start: 0, End: 2}); got != "ni" || actual != (ui.TextInputRange{Start: 0, End: 2}) {
		t.Fatalf("range %q actual %+v", got, actual)
	}
	if got := v.Selection(); got.Range != (ui.TextInputRange{Start: 2, End: 2}) {
		t.Fatalf("selection %+v", got)
	}
	if marked, ok := v.MarkedRange(); !ok || marked != (ui.TextInputRange{Start: 0, End: 2}) {
		t.Fatalf("marked %+v %v", marked, ok)
	}
	v.layout(800, 400)
	b, actual, ok := v.BoundsForRange(ui.TextInputRange{Start: 0, End: 2})
	if !ok || actual != (ui.TextInputRange{Start: 0, End: 2}) {
		t.Fatalf("bounds %+v actual %+v ok %v", b, actual, ok)
	}
	if at, ok := v.IndexForPoint(ui.Point{X: b.X, Y: b.Y}); !ok || at != 0 {
		t.Fatalf("index %d ok %v", at, ok)
	}
}
