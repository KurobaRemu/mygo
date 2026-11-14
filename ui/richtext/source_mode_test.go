package richtext

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
)

func sourceRange(d *Document, anchor, caret int) (int, int) {
	d.lock()
	sel := d.sourceSelectionForDocLocked(anchor, caret)
	d.mu.Unlock()
	return min(sel.Anchor.Offset, sel.Caret.Offset), max(sel.Anchor.Offset, sel.Caret.Offset)
}

func currentSourceRange(d *Document) (int, int) {
	sel := d.SourceSelection()
	return min(sel.Anchor.Offset, sel.Caret.Offset), max(sel.Anchor.Offset, sel.Caret.Offset)
}

func TestSourceModeRoundTripWithoutEdit(t *testing.T) {
	src := "# Head\r\n\r\n- **bold** item\n\r\n> quote\r\n"
	d, v := markerView(src)
	d.shownIn = weak.Make(v)
	d.Edit(func(e *Edit) {
		at := strings.Index(src, "bold")
		e.ReplaceSource(at, at+4, "Bold")
	})
	wantSource := d.Source()
	wantUndo := len(d.undo)
	wantRedo := len(d.redo)
	a, z := sourceRange(d, 6, 6)
	d.selectFromSource(a, z)
	var lastBuffer *editor.Buffer
	for range 3 {
		s := NewSourceModeSession(d)
		if err := s.Enter(); err != nil {
			t.Fatal(err)
		}
		if s.Buffer == lastBuffer {
			t.Fatal("source mode reused its buffer")
		}
		lastBuffer = s.Buffer
		if got := d.Source(); got != wantSource {
			t.Fatalf("enter made source %q", got)
		}
		if len(d.undo) != wantUndo || len(d.redo) != wantRedo {
			t.Fatalf("enter made undo/redo %d/%d, want %d/%d", len(d.undo), len(d.redo), wantUndo, wantRedo)
		}
		if !s.State.SoftWrap || s.State.LineNumbers != editor.LineNumbersOn ||
			s.State.Whitespace != editor.WhitespaceAll || s.State.TabSize != 2 {
			t.Fatalf("source mode options %+v", s.State)
		}
		if s.Buffer.Language() != editor.LanguageNamed("markdown") {
			t.Fatalf("source mode language %v", s.Buffer.Language())
		}
		if got, want := s.Buffer.String(), newEOLTable(wantSource).normalized; got != want {
			t.Fatalf("source buffer %q, want %q", got, want)
		}
		if err := s.Leave(); err != nil {
			t.Fatal(err)
		}
		if got := d.Source(); got != wantSource {
			t.Fatalf("leave made source %q", got)
		}
		if len(d.undo) != wantUndo || len(d.redo) != wantRedo {
			t.Fatalf("leave made undo/redo %d/%d, want %d/%d", len(d.undo), len(d.redo), wantUndo, wantRedo)
		}
		if s.Buffer != nil {
			t.Fatal("source buffer was kept")
		}
		if gotA, gotZ := currentSourceRange(d); gotA != a || gotZ != z {
			t.Fatalf("selection %d-%d, want %d-%d", gotA, gotZ, a, z)
		}
	}
}

func TestDocumentSourceSelectionMappings(t *testing.T) {
	for _, tt := range []struct {
		name string
		src  string
		doc  int
		want int
	}{
		{"heading", "# Head\n", 0, 2},
		{"bullet", "- item\n", 0, 2},
		{"ordered", "12. item\n", 0, 4},
		{"quote", "> quote\n", 0, 2},
		{"task", "- [ ] task\n", 0, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := NewMarkdownDocument(tt.src)
			a, z := sourceRange(d, tt.doc, tt.doc)
			if a != tt.want || z != tt.want {
				t.Fatalf("mapped to %d-%d, want %d", a, z, tt.want)
			}
			ra, rz := d.RichSelectionFromSource(0, len(tt.src))
			if ra != 0 || rz != d.Text().Len() {
				t.Fatalf("source whole mapped to rich %d-%d", ra, rz)
			}
		})
	}

	d := NewMarkdownDocument("\\* &amp; end\n")
	a, z := d.RichSelectionFromSource(1, 1)
	if a != 0 || z != 1 {
		t.Fatalf("atomic object mapped to %d-%d", a, z)
	}
	a, z = sourceRange(d, 0, 1)
	if a != 0 || z != 2 {
		t.Fatalf("selected atomic object mapped to %d-%d", a, z)
	}
	a, z = sourceRange(d, 1, 0)
	if a != 0 || z != 2 {
		t.Fatalf("reverse selected atomic object mapped to %d-%d", a, z)
	}

	d = NewMarkdownDocument("- \n- x\n")
	a, z = sourceRange(d, 0, 0)
	if a != 2 || z != 2 {
		t.Fatalf("synthetic paragraph mapped to %d-%d", a, z)
	}
	d = NewMarkdownDocument("- a\n- \n")
	a, z = sourceRange(d, 2, 2)
	if a != 6 || z != 6 {
		t.Fatalf("synthetic paragraph after content mapped to %d-%d", a, z)
	}
	for _, source := range []int{4, 5} {
		a, z = d.RichSelectionFromSource(source, source)
		if a != 2 || z != 2 {
			t.Fatalf("empty list prefix source %d mapped to %d-%d", source, a, z)
		}
	}

	d = NewMarkdownDocument("para\n\n[id]: https://example.com\n")
	source := strings.Index(d.Source(), "https")
	a, z = d.RichSelectionFromSource(source, source)
	if a != d.Text().Len() || z != d.Text().Len() {
		t.Fatalf("reference definition mapped to rich %d-%d", a, z)
	}
	d.selectFromSource(source, source)
	if a, z = currentSourceRange(d); a != source || z != source {
		t.Fatalf("reference definition selection mapped to %d-%d", a, z)
	}
}

func TestSourceSelectionRoundTripStability(t *testing.T) {
	for _, src := range []string{
		"# Head\n\n- item\n\n> quote\n",
		"pre **bold** *em* `code` [link](u) post\n",
		"\\* &amp; ![alt](img.png) end\n",
		"- \n- [ ] task\n",
		"para\n\n[id]: https://example.com\n",
		"a\r\nb\nc\r\n",
	} {
		d := NewMarkdownDocument(src)
		for i := 0; i <= d.Text().Len(); i++ {
			a, z := sourceRange(d, i, i)
			ra, rz := d.RichSelectionFromSource(a, z)
			sa, sz := sourceRange(d, ra, rz)
			if sa > sz {
				t.Fatalf("%q text %d mapped %d-%d then %d-%d", src, i, a, z, sa, sz)
			}
			ra2, rz2 := d.RichSelectionFromSource(sa, sz)
			sa2, sz2 := sourceRange(d, ra2, rz2)
			if sa2 != sa || sz2 != sz || ra2 != ra || rz2 != rz {
				t.Fatalf("%q text %d was unstable: source %d-%d rich %d-%d, then source %d-%d rich %d-%d", src, i, sa, sz, ra, rz, sa2, sz2, ra2, rz2)
			}
		}
		for i := 0; i <= len(src); i++ {
			a, z := d.RichSelectionFromSource(i, i)
			sa, sz := sourceRange(d, a, z)
			ra2, rz2 := d.RichSelectionFromSource(sa, sz)
			sa2, sz2 := sourceRange(d, ra2, rz2)
			if sa2 != sa || sz2 != sz || ra2 != a || rz2 != z {
				t.Fatalf("%q source %d was unstable: source %d-%d rich %d-%d, then source %d-%d rich %d-%d", src, i, sa, sz, a, z, sa2, sz2, ra2, rz2)
			}
		}
	}
}

func TestCanonicalRichSourcePositionsRoundTrip(t *testing.T) {
	for _, src := range []string{
		"plain text\n",
		"# Head\n",
		"- [ ] task\n",
		"pre **bold** *em* `code` [link](u) post\n",
		"\\* &amp; ![alt](img.png) end\n",
		"- \n- x\n",
	} {
		d := NewMarkdownDocument(src)
		for i := 0; i <= d.Text().Len(); i++ {
			a, z := sourceRange(d, i, i)
			if a != z {
				continue
			}
			ra, rz := d.RichSelectionFromSource(a, z)
			if ra != i || rz != i {
				t.Fatalf("%q rich position %d mapped to source %d and back to rich %d-%d", src, i, a, ra, rz)
			}
		}
	}
}

func TestSourceMappingPrefixesLinksImagesAndDefinitions(t *testing.T) {
	for _, src := range []string{"# Head\n", "- item\n", "> quote\n", "- [ ] task\n"} {
		d := NewMarkdownDocument(src)
		for _, source := range []int{0, 1} {
			a, z := d.RichSelectionFromSource(source, source)
			if a != 0 || z != 0 {
				t.Fatalf("%q source %d mapped to rich %d-%d", src, source, a, z)
			}
		}
	}

	src := "a [link](u) ![alt](img.png) b\n"
	d, v := markerView(src)
	d.shownIn = weak.Make(v)
	linkTarget := strings.Index(src, "(u)")
	imageStart := strings.Index(src, "![")
	imageEnd := imageStart + len("![alt](img.png)")
	for _, source := range []int{linkTarget + 1, imageStart + 2, imageEnd - 2} {
		d.selectFromSource(source, source)
		if a, z := currentSourceRange(d); a != source || z != source {
			t.Fatalf("source %d round-tripped to %d-%d", source, a, z)
		}
	}
	text := d.Text().String()
	richStart := strings.Index(text, "![alt](img.png)")
	a, z := sourceRange(d, richStart, richStart+len("![alt](img.png)"))
	if a != imageStart || z != imageEnd {
		t.Fatalf("image selection mapped to %d-%d, want %d-%d", a, z, imageStart, imageEnd)
	}

	d = NewMarkdownDocument("para\n\n[id]: https://example.com\n")
	source := strings.Index(d.Source(), "example")
	d.selectFromSource(source, source)
	if a, z := currentSourceRange(d); a != source || z != source {
		t.Fatalf("reference definition source %d mapped to %d-%d", source, a, z)
	}
}

func TestSourceModeDelimiterSelectionRoundTrip(t *testing.T) {
	src := "pre **alpha** post\n"
	d, v := markerView(src)
	d.shownIn = weak.Make(v)
	var strong box
	for _, b := range v.boxes(0) {
		if b.kind == MarkerStrong {
			strong = b
		}
	}
	for _, source := range []int{strong.openStart + 1, strong.closeStart + 1} {
		d.selectFromSource(source, source)
		s := NewSourceModeSession(d)
		if err := s.Enter(); err != nil {
			t.Fatal(err)
		}
		sel := s.State.Selection()
		wantBuffer := s.eol.fromSource(source)
		if sel.Anchor != wantBuffer || sel.Head != wantBuffer {
			t.Fatalf("source %d selected buffer %+v, want %d", source, sel, wantBuffer)
		}
		if err := s.Leave(); err != nil {
			t.Fatal(err)
		}
		a, z := currentSourceRange(d)
		if a != source || z != source {
			t.Fatalf("source %d round-tripped to %d-%d", source, a, z)
		}
		v.sync()
		pv := v.paraLayout(0)
		at := pv.display.Map.DisplayOfSource(source, false)
		if got := pv.display.Map.At(at).Source; got != source {
			t.Fatalf("source %d displayed at %d", source, got)
		}
		if got := displayText(*pv.display); !strings.Contains(got, "**alpha**") {
			t.Fatalf("source %d did not reveal strong in %q", source, got)
		}
	}
}

func TestSourceSelectionRestoresInNewRichView(t *testing.T) {
	src := "pre **alpha** post\n"
	d := NewMarkdownDocument(src)
	source := strings.Index(src, "**") + 1
	d.selectFromSource(source, source)
	tt := ui.NewTester(func(c *ui.Context) {
		Editor(c, d).Fill()
	}, 640, 300)
	tt.Frame()
	a, z := currentSourceRange(d)
	if a != source || z != source {
		t.Fatalf("new view mapped source %d to %d-%d", source, a, z)
	}
	pv := d.activeView().paraLayout(0)
	if got := displayText(*pv.display); !strings.Contains(got, "**alpha**") {
		t.Fatalf("new view did not reveal strong in %q", got)
	}
}

func TestEOLTableMixedEndings(t *testing.T) {
	src := "a\r\nb\nc\r\n"
	eol := newEOLTable(src)
	if eol.normalized != "a\nb\nc\n" {
		t.Fatalf("normalized %q", eol.normalized)
	}
	if eol.dominant != "\r\n" {
		t.Fatalf("dominant %q", eol.dominant)
	}
	for _, tt := range []struct {
		source int
		buffer int
	}{
		{0, 0},
		{1, 1},
		{2, 1},
		{3, 2},
		{4, 3},
		{5, 4},
		{6, 5},
		{7, 5},
		{8, 6},
	} {
		if got := eol.fromSource(tt.source); got != tt.buffer {
			t.Fatalf("source %d mapped to buffer %d, want %d", tt.source, got, tt.buffer)
		}
	}
	for _, tt := range []struct {
		buffer int
		source int
	}{
		{0, 0},
		{1, 1},
		{2, 3},
		{3, 4},
		{4, 5},
		{5, 6},
		{6, 8},
	} {
		if got := eol.toSource(tt.buffer); got != tt.source {
			t.Fatalf("buffer %d mapped to source %d, want %d", tt.buffer, got, tt.source)
		}
	}
}

func TestSourceModePreservesUntouchedMixedEOL(t *testing.T) {
	d := NewMarkdownDocument("a\r\nb\nc\r\n")
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	s.Buffer.Edit(editor.Edit{Start: 2, End: 3, Text: "B"})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "a\r\nB\nc\r\n"; got != want {
		t.Fatalf("source %q, want %q", got, want)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	s.Buffer.Edit(editor.Edit{Start: 2, End: 2, Text: "\n"})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "a\r\n\nB\nc\r\n"; got != want {
		t.Fatalf("inserted line ending made %q, want %q", got, want)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "a\r\n\nB\nc\r\n"; got != want {
		t.Fatalf("leave made %q, want %q", got, want)
	}
}

func TestSourceModeInsertedNewlineInheritsCRLF(t *testing.T) {
	d := NewMarkdownDocument("a\r\nb\n")
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	s.Buffer.Edit(editor.Edit{Start: 1, End: 1, Text: "\n"})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "a\r\n\r\nb\n"; got != want {
		t.Fatalf("source %q, want %q", got, want)
	}
}

func TestSourceModeProjectionAndBufferStaySynchronized(t *testing.T) {
	src := "# Head\n\n- **bold** item\n\n> quote\n"
	d := NewMarkdownDocument(src)
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	at := strings.Index(src, "bold")
	start := s.eol.fromSource(at)
	s.Buffer.Edit(editor.Edit{Start: start, End: start + 4, Text: "汉字"})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), strings.Replace(src, "bold", "汉字", 1); got != want {
		t.Fatalf("source %q, want %q", got, want)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	text, _ := ParseMarkdownSource(d.Source())
	if got, want := d.Text().String(), text.String(); got != want {
		t.Fatalf("projection %q, want %q", got, want)
	}
	if got, want := s.Buffer.String(), newEOLTable(d.Source()).normalized; got != want {
		t.Fatalf("buffer %q, want %q", got, want)
	}
	if start, oldEnd, newEnd, ok := d.LastSourcePatch(); !ok || start != at || oldEnd != at+4 || newEnd != at+len("汉字") {
		t.Fatalf("last patch %d %d %d %v", start, oldEnd, newEnd, ok)
	}
}

func TestSourceModeSyncsExternalSourceChanges(t *testing.T) {
	d := NewMarkdownDocument("a\nb\n")
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	d.Edit(func(e *Edit) {
		e.ReplaceSource(0, 1, "A")
	})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Buffer.String(), "A\nb\n"; got != want {
		t.Fatalf("buffer %q, want %q", got, want)
	}
	s.Buffer.Edit(editor.Edit{Start: 2, End: 3, Text: "B"})
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "A\nB\n"; got != want {
		t.Fatalf("leave before sync made %q, want %q", got, want)
	}
}

func TestSourceModeSharedUndoRedo(t *testing.T) {
	d := NewMarkdownDocument("- **bold** item\n")
	at := strings.Index(d.Source(), "bold")
	d.Edit(func(e *Edit) {
		e.ReplaceSource(at, at+4, "粗体")
	})
	wysiwygSource := d.Source()
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	at = strings.Index(d.Source(), "item")
	start := s.eol.fromSource(at)
	s.Buffer.Edit(editor.Edit{Start: start, End: start, Text: "!"})
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	editedSource := d.Source()
	if got := len(d.undo); got != 2 {
		t.Fatalf("cross-mode edits made %d undo steps", got)
	}
	mod := ui.Cmd
	if runtime.GOOS != "darwin" {
		mod = ui.Ctrl
	}
	if !s.handleUndo(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyZ, Mods: mod}) {
		t.Fatal("undo was not intercepted")
	}
	if got := d.Source(); got != wysiwygSource {
		t.Fatalf("undo made %q, want %q", got, wysiwygSource)
	}
	if got, want := s.Buffer.String(), newEOLTable(wysiwygSource).normalized; got != want {
		t.Fatalf("undo buffer %q, want %q", got, want)
	}
	if !s.handleUndo(ui.InputEvent{Kind: ui.InputKeyDown, Key: ui.KeyZ, Mods: mod | ui.Shift}) {
		t.Fatal("redo was not intercepted")
	}
	if got := d.Source(); got != editedSource {
		t.Fatalf("redo made %q, want %q", got, editedSource)
	}
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	d.Undo()
	if got := d.Source(); got != wysiwygSource {
		t.Fatalf("cross-mode undo made %q, want %q", got, wysiwygSource)
	}
}

func TestSourceModeTypingCoalescesAndInterceptsUndoKeys(t *testing.T) {
	d := NewMarkdownDocument("item\n")
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(func(c *ui.Context) {
		s.View(c).Fill()
	}, 500, 300)
	tt.Frame()
	tt.Press(20, 20)
	tt.Release(20, 20)
	s.State.SetSelectionsNoReveal(editor.Selection{})
	tt.Frame()
	tt.Type("a")
	tt.Type("b")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "abitem\n"; got != want {
		t.Fatalf("typing made %q, want %q", got, want)
	}
	if got := len(d.undo); got != 1 {
		t.Fatalf("typing made %d undo steps", got)
	}
	mod := ui.Cmd
	if runtime.GOOS != "darwin" {
		mod = ui.Ctrl
	}
	tt.Key(mod, ui.KeyZ)
	if got, want := d.Source(), "item\n"; got != want {
		t.Fatalf("key undo made %q, want %q", got, want)
	}
	tt.Key(mod|ui.Shift, ui.KeyZ)
	if got, want := d.Source(), "abitem\n"; got != want {
		t.Fatalf("key redo made %q, want %q", got, want)
	}
}

func TestSourceModeEnterCommitsIME(t *testing.T) {
	d, v := markerView("a **bold** c\n")
	d.shownIn = weak.Make(v)
	d.SetSelection(2, 6)
	v.SetMarkedText(nil, "ni", ui.TextInputRange{Start: 2, End: 2})
	v.SetMarkedText(nil, "你", ui.TextInputRange{Start: 1, End: 1})
	s := NewSourceModeSession(d)
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	if got, want := d.Source(), "a **你** c\n"; got != want {
		t.Fatalf("enter made %q, want %q", got, want)
	}
	if v.pendingSet || v.compose != "" {
		t.Fatalf("composition stayed %q", v.compose)
	}
}

func TestSourceModeViewportAnchorWithinOneLine(t *testing.T) {
	var src strings.Builder
	for i := range 100 {
		fmt.Fprintf(&src, "line %03d\n\n", i)
	}
	d := NewMarkdownDocument(src.String())
	s := NewSourceModeSession(d)
	sourceMode := false
	tt := ui.NewTester(func(c *ui.Context) {
		if sourceMode {
			s.View(c).Fill()
			return
		}
		Editor(c, d).Fill()
	}, 640, 300)
	tt.Frame()
	targetSource := strings.Index(d.Source(), "line 060")
	d.SetScrollAnchor(ViewportAnchor{Source: targetSource})
	tt.Frame()
	wantLine := sourceLine(d.Source(), targetSource)
	if got := sourceLine(d.Source(), d.ScrollAnchor().Source); abs(got-wantLine) > 1 {
		t.Fatalf("rich anchor line %d, want %d", got, wantLine)
	}
	sourceMode = true
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	first, _ := s.State.VisibleLines()
	gotLine := s.eol.lineAtBuffer(s.eol.lines[first].bufferStart)
	if abs(gotLine-wantLine) > 1 {
		t.Fatalf("source anchor line %d, want %d", gotLine, wantLine)
	}
	sourceMode = false
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if got := sourceLine(d.Source(), d.ScrollAnchor().Source); abs(got-wantLine) > 1 {
		t.Fatalf("returned rich anchor line %d, want %d", got, wantLine)
	}
}

func TestSourceModeViewportPreservesVisibleCaretY(t *testing.T) {
	var src strings.Builder
	for i := range 100 {
		fmt.Fprintf(&src, "line %03d text\n", i)
	}
	d := NewMarkdownDocument(src.String())
	s := NewSourceModeSession(d)
	sourceMode := false
	tt := ui.NewTester(func(c *ui.Context) {
		if sourceMode {
			s.View(c).Fill()
			return
		}
		Editor(c, d).Fill()
	}, 640, 300)
	tt.Frame()
	targetSource := strings.Index(d.Source(), "line 060")
	at, _ := d.RichSelectionFromSource(targetSource, targetSource)
	d.SetSelection(at, at)
	tt.Frame()
	v := d.activeView()
	_, y, _ := v.caretAt(v.caret, 0, v.affinity)
	v.setScroll(y + float64(v.padY) - 60)
	tt.Frame()
	anchor := d.ScrollAnchor()
	if !anchor.CaretVisible || abs(int(anchor.CaretY)-60) > 1 {
		t.Fatalf("rich caret anchor %+v", anchor)
	}
	sourceMode = true
	if err := s.Enter(); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	_, sourceY, sourceH := s.sourceCaretRect()
	if got := sourceY - s.State.ScrollY(); abs(int(got)-60) > max(1, int(sourceH)) {
		t.Fatalf("source caret y %v, want 60 with line height %v", got, sourceH)
	}
	sourceMode = false
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	after := d.ScrollAnchor()
	if !after.CaretVisible || abs(int(after.CaretY)-60) > max(1, int(sourceH)) {
		t.Fatalf("returned caret anchor %+v, want y 60", after)
	}
}

func TestScrollAnchorRestoresVisibleCaret(t *testing.T) {
	var src strings.Builder
	for i := range 100 {
		fmt.Fprintf(&src, "line %03d text\n", i)
	}
	d := NewMarkdownDocument(src.String())
	tt := ui.NewTester(func(c *ui.Context) {
		Editor(c, d).Fill()
	}, 640, 300)
	tt.Frame()
	targetSource := strings.Index(d.Source(), "line 060")
	at, _ := d.RichSelectionFromSource(targetSource, targetSource)
	d.SetSelection(at, at)
	tt.Frame()
	v := d.activeView()
	_, y, _ := v.caretAt(v.caret, 0, v.affinity)
	v.setScroll(y + float64(v.padY) - 60)
	tt.Frame()
	anchor := d.ScrollAnchor()
	if !anchor.CaretVisible || abs(int(anchor.CaretY)-60) > 1 {
		t.Fatalf("caret anchor %+v", anchor)
	}
	v.setScroll(0)
	d.SetScrollAnchor(anchor)
	tt.Frame()
	v = d.activeView()
	_, y, _ = v.caretAt(v.caret, 0, v.affinity)
	if got := y + float64(v.padY) - v.scroll; abs(int(got)-60) > 1 {
		t.Fatalf("restored caret y %v, want 60", got)
	}
}

func sourceLine(src string, at int) int {
	return strings.Count(src[:max(0, min(at, len(src)))], "\n")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
