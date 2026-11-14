package richtext

import (
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
)

func markerView(src string) (*Document, *view) {
	d := NewMarkdownDocument(src)
	v := &view{doc: d, reveal: true, width: 800, viewH: 400, hoverPara: -1, hoverBox: -1}
	v.shown, v.source = d.Text(), d.SourceMap()
	v.selAnchor, v.caret = d.Selection()
	v.anchorAffinity, v.caretAffinity = BoundaryDownstream, BoundaryDownstream
	v.paras = make([]paraView, v.shown.ParagraphCount())
	v.look = look{Theme: Theme{Muted: ui.RGB(120, 120, 120), Text: ui.RGB(0, 0, 0)}}
	v.base = ui.Font{Size: 14}
	return d, v
}

func displayText(p DisplayParagraph) string {
	var b strings.Builder
	for _, span := range p.Spans {
		b.WriteString(span.Text)
	}
	return b.String()
}

func findPointer(tt *ui.Tester) (float32, float32, bool) {
	for y := float32(2); y < 160; y += 2 {
		for x := float32(2); x < 360; x += 2 {
			tt.Move(x, y)
			if tt.Cursor() == ui.CursorPointer {
				return x, y, true
			}
		}
	}
	return 0, 0, false
}

func setSourceCaret(v *view, source int) {
	v.setSourceSelection(sourceCaret(source))
}

func TestMarkerRevealDisplayMapAllInlineKinds(t *testing.T) {
	src := "# Head\n\n**s** *e* `c` ~~x~~ [l](u \"t\") [r][id] <https://e.test> <u>h</u>\n\n[id]: https://r.test\n"
	_, v := markerView(src)
	p := 1
	boxes := v.boxes(p)
	kinds := map[MarkerKind]bool{}
	for _, b := range boxes {
		kinds[b.kind] = true
	}
	for _, kind := range []MarkerKind{MarkerStrong, MarkerEmphasis, MarkerCode, MarkerStrikethrough, MarkerLink, MarkerHTML} {
		if !kinds[kind] {
			t.Fatalf("missing marker kind %d in %+v", kind, boxes)
		}
	}
	if strings.Contains(displayText(v.displayParagraph(p, Attributes{})), "#") {
		t.Fatal("block prefix was revealed")
	}
	setSourceCaret(v, boxes[0].contentFrom)
	shown := displayText(v.displayParagraph(p, Attributes{}))
	if !strings.Contains(shown, "**s**") {
		t.Fatalf("strong was not revealed in %q", shown)
	}
	m := v.displayParagraph(p, Attributes{}).Map
	inner := m.DisplayOfSource(boxes[0].openEnd, false)
	o := m.At(inner)
	if o.Source != boxes[0].openEnd || o.Doc < v.shown.paraStart(p) || o.Doc > v.shown.paraStart(p)+v.para(p).n {
		t.Fatalf("bad display to source map: %+v", o)
	}
}

func TestMarkerRevealDelimiterVariants(t *testing.T) {
	for _, c := range []struct {
		name   string
		src    string
		para   int
		kind   MarkerKind
		source string
	}{
		{"strong star", "**alpha**\n", 0, MarkerStrong, "**alpha**"},
		{"strong underscore", "__alpha__\n", 0, MarkerStrong, "__alpha__"},
		{"emphasis star", "*alpha*\n", 0, MarkerEmphasis, "*alpha*"},
		{"emphasis underscore", "_alpha_\n", 0, MarkerEmphasis, "_alpha_"},
		{"code", "`alpha`\n", 0, MarkerCode, "`alpha`"},
		{"strikethrough", "~~alpha~~\n", 0, MarkerStrikethrough, "~~alpha~~"},
		{"inline link", "[alpha](url \"title\")\n", 0, MarkerLink, "[alpha](url \"title\")"},
		{"reference link", "[alpha][id]\n\n[id]: https://example.com\n", 0, MarkerLink, "[alpha][id]"},
		{"angle autolink", "<https://example.com>\n", 0, MarkerLink, "<https://example.com>"},
		{"underline", "<u>alpha</u>\n", 0, MarkerHTML, "<u>alpha</u>"},
		{"html strike", "<s>alpha</s>\n", 0, MarkerHTML, "<s>alpha</s>"},
		{"html bold", "<b>alpha</b>\n", 0, MarkerHTML, "<b>alpha</b>"},
		{"html italic", "<i>alpha</i>\n", 0, MarkerHTML, "<i>alpha</i>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, v := markerView(c.src)
			var found box
			for _, b := range v.boxes(c.para) {
				if b.kind == c.kind {
					found = b
					break
				}
			}
			if found.kind == 0 {
				t.Fatalf("no marker box: %+v", v.boxes(c.para))
			}
			setSourceCaret(v, found.contentFrom)
			if got := displayText(v.displayParagraph(c.para, Attributes{})); !strings.Contains(got, c.source) {
				t.Fatalf("display %q does not contain %q", got, c.source)
			}
		})
	}
}

func TestMarkerRevealBareAutolinkHasNoDelimiterBox(t *testing.T) {
	for _, src := range []string{"https://example.com\n", "www.example.com\n"} {
		_, v := markerView(src)
		for _, b := range v.boxes(0) {
			if b.kind == MarkerLink {
				t.Fatalf("%q produced link box %+v", src, b)
			}
		}
	}
}

func TestMarkerRevealBlockPrefixesStayHidden(t *testing.T) {
	src := "# head\n\n- bullet\n\n> quote\n\n1. ordered\n\n- [ ] task\n"
	_, v := markerView(src)
	for _, p := range []int{0, 1, 2, 3, 4} {
		setSourceCaret(v, v.source.start(p))
		got := displayText(v.displayParagraph(p, Attributes{}))
		for _, prefix := range []string{"#", "-", ">", "1."} {
			if strings.HasPrefix(strings.TrimSpace(got), prefix) {
				t.Fatalf("paragraph %d displayed block prefix in %q", p, got)
			}
		}
	}
}

func TestMarkerRevealBlockPrefixesStayHiddenWhileInlineRevealed(t *testing.T) {
	src := "# **head**\n\n- **item**\n\n> **quote**\n\n1. **ordered**\n\n- [ ] **task**\n"
	_, v := markerView(src)
	for p := range v.shown.ParagraphCount() {
		var strong box
		for _, b := range v.boxes(p) {
			if b.kind == MarkerStrong {
				strong = b
			}
		}
		if strong.kind == 0 {
			t.Fatalf("paragraph %d has no Strong", p)
		}
		setSourceCaret(v, strong.contentFrom)
		got := strings.TrimSpace(displayText(v.displayParagraph(p, Attributes{})))
		if !strings.HasPrefix(got, "**") {
			t.Fatalf("paragraph %d did not reveal Strong: %q", p, got)
		}
		for _, prefix := range []string{"#", "-", ">", "1."} {
			if strings.HasPrefix(got, prefix) {
				t.Fatalf("paragraph %d displayed block prefix in %q", p, got)
			}
		}
	}
}

// A paragraph's spans all take its style: a quotation colors every span of
// the line, whatever its runs, and its markers stay hidden.
func TestDisplayParagraphSpansShareParagraphStyle(t *testing.T) {
	_, v := markerView("> **bold** plain *italic*\n\nend\n")
	v.look.QuoteText = ui.RGB(10, 20, 30)
	if st := v.para(0).style; !st.Quote {
		t.Fatalf("style = %+v, want a quotation", st)
	}
	display := v.displayParagraph(0, Attributes{})
	if len(display.Spans) < 3 {
		t.Fatalf("spans = %d, want one per run", len(display.Spans))
	}
	for _, span := range display.Spans {
		if span.Text != "" && span.Color != v.look.QuoteText {
			t.Fatalf("span %q color = %v, want the paragraph's quotation color %v", span.Text, span.Color, v.look.QuoteText)
		}
	}
	if got := displayText(display); got != "bold plain italic" {
		t.Fatalf("display = %q, want %q", got, "bold plain italic")
	}
}

func TestMarkerRevealDoesNotMutateDocument(t *testing.T) {
	src := "**alpha** and [link](url)\n"
	d, v := markerView(src)
	beforeText := d.Text().String()
	beforeSource := d.Source()
	beforeUndo := d.CanUndo()
	boxes := v.boxes(0)
	for _, b := range boxes {
		setSourceCaret(v, b.contentFrom)
		v.displayParagraph(0, Attributes{})
	}
	if got := d.Text().String(); got != beforeText {
		t.Fatalf("text %q, want %q", got, beforeText)
	}
	if got := d.Source(); got != beforeSource {
		t.Fatalf("source %q, want %q", got, beforeSource)
	}
	if got := d.CanUndo(); got != beforeUndo {
		t.Fatalf("undo changed")
	}
}

func TestMarkerRevealDisplayMapRoundTrip(t *testing.T) {
	src := "pre **alpha** post\n"
	_, v := markerView(src)
	var strong box
	for _, b := range v.boxes(0) {
		if b.kind == MarkerStrong {
			strong = b
		}
	}
	setSourceCaret(v, strong.contentFrom)
	display := v.displayParagraph(0, Attributes{})
	if got := display.Map.Len(); got != utf8.RuneCountInString("pre **alpha** post") {
		t.Fatalf("map length %d", got)
	}
	for source := strong.openStart; source <= strong.closeEnd; source++ {
		at := display.Map.DisplayOfSource(source, false)
		o := display.Map.At(at)
		if o.Source < strong.openStart || o.Source > strong.closeEnd {
			t.Fatalf("source %d mapped outside marker: %+v", source, o)
		}
	}
	open := display.Map.At(display.Map.DisplayOfSource(strong.openEnd, false))
	if open.Source != strong.openEnd {
		t.Fatalf("opening marker boundary %+v", open)
	}
	close := display.Map.At(display.Map.DisplayOfSource(strong.closeStart, false))
	if close.Source != strong.closeStart {
		t.Fatalf("closing marker boundary %+v", close)
	}
	markerRunes := 0
	for i := 0; i <= display.Map.Len(); i++ {
		o := display.Map.At(i)
		if o.Marker {
			markerRunes++
		}
	}
	if markerRunes != 4 {
		t.Fatalf("mapped %d marker runes", markerRunes)
	}
	for i := 1; i <= display.Map.Len(); i++ {
		if display.Map.At(i).Source < display.Map.At(i-1).Source {
			t.Fatalf("source map regressed at %d", i)
		}
		if display.Map.At(i).Doc < display.Map.At(i-1).Doc {
			t.Fatalf("doc map regressed at %d", i)
		}
	}
}

func TestMarkerRevealNestedCloseAtOuterEnd(t *testing.T) {
	// record 2026-11-09, seed5/c5：内层 strong 的闭合标记紧贴外层
	// emphasis 的闭合标记，光标停在 `***` 之后时内外两层都保持揭示。
	// 0 个前缀走扫描路径，阈值个前缀走索引路径。
	for _, pad := range []int{0, revealCloseIndexMin} {
		prefix := strings.Repeat("**w** ", pad)
		_, v := markerView(prefix + "*jf**hkrd***\n")
		boxes := v.boxes(0)
		if pad > 0 && len(boxes) < revealCloseIndexMin {
			t.Fatalf("%d boxes do not cross the index threshold %d", len(boxes), revealCloseIndexMin)
		}
		var outer, inner box
		for _, b := range boxes {
			if b.start < len(prefix) {
				continue
			}
			switch b.kind {
			case MarkerEmphasis:
				outer = b
			case MarkerStrong:
				inner = b
			}
		}
		if outer.kind == 0 || inner.kind == 0 {
			t.Fatalf("pad %d: nested boxes missing: %+v", pad, boxes)
		}
		if inner.end != outer.closeStart || inner.end >= outer.end {
			t.Fatalf("pad %d: inner %+v is not nested at outer close %+v", pad, inner, outer)
		}
		setSourceCaret(v, outer.end)
		active := v.revealedBoxes(0)
		if !containsBox(active, outer) || !containsBox(active, inner) {
			t.Fatalf("pad %d: caret at outer end %d revealed %+v", pad, outer.end, active)
		}
		if got := displayText(v.displayParagraph(0, Attributes{})); !strings.HasSuffix(got, "*jf**hkrd***") {
			t.Fatalf("pad %d: display %q does not reveal both closing pairs", pad, got)
		}
	}
}

func TestMarkerRevealClosedIntervalNestedAndSelection(t *testing.T) {
	src := "pre **a *b* c** post\n"
	_, v := markerView(src)
	p := 0
	boxes := v.boxes(p)
	var inner, outer box
	for _, b := range boxes {
		switch b.kind {
		case MarkerStrong:
			outer = b
		case MarkerEmphasis:
			inner = b
		}
	}
	for _, at := range []int{outer.start, outer.openStart + 1, outer.end} {
		setSourceCaret(v, at)
		active := v.revealedBoxes(p)
		if len(active) != 1 || active[0] != outer {
			t.Fatalf("caret at source %d revealed %+v", at, active)
		}
	}
	setSourceCaret(v, inner.contentFrom)
	active := v.revealedBoxes(p)
	if len(active) != 2 || active[0] != outer || active[1] != inner {
		t.Fatalf("inner caret revealed %+v", active)
	}
	setSourceCaret(v, outer.start-1)
	if got := v.revealedBoxes(p); len(got) != 0 {
		t.Fatalf("outside reveal %+v", got)
	}
	v.doc.SetSelection(0, v.shown.Len()-1)
	v.sync()
	if got := v.revealedBoxes(p); len(got) != 0 {
		t.Fatalf("selection revealed %+v", got)
	}
}

func TestMarkerRevealAcrossSoftBreak(t *testing.T) {
	_, v := markerView("**alpha\nbeta**\n")
	if got := v.shown.ParagraphCount(); got != 1 {
		t.Fatalf("paragraphs %d", got)
	}
	for _, at := range []int{3, 9} {
		setSourceCaret(v, at)
		display := displayText(v.displayParagraph(0, Attributes{}))
		if got := strings.ReplaceAll(display, string(LineSeparator), "\n"); got != "**alpha\nbeta**" {
			t.Fatalf("caret at %d displays %q", at, display)
		}
	}
}

func TestMarkerRevealDisplayMapComposition(t *testing.T) {
	d, v := markerView("**alpha**\n")
	setSourceCaret(v, 2)
	base := v.displayParagraph(0, Attributes{})
	_ = base
	before := d.Source()
	v.compose = "拼"
	display := v.displayParagraph(0, Attributes{})
	if got := displayText(display); got != "**拼alpha**" {
		t.Fatalf("display %q", got)
	}
	if got := d.Source(); got != before {
		t.Fatalf("source changed to %q", got)
	}
	if got := display.Map.Len(); got != utf8.RuneCountInString(displayText(display)) {
		t.Fatalf("map length %d", got)
	}
	o := display.Map.At(2)
	if o.Doc != 0 || o.Source != 2 {
		t.Fatalf("composition offset %+v", o)
	}
}

func TestMarkerRevealDisplayMapTextInputGeometry(t *testing.T) {
	d, v := markerView("**alpha**\n")
	d.SetSelection(1, 1)
	v.sync()
	v.rect = ui.Rect{W: 800, H: 400}
	v.layout(800, 400)
	x, y, h := v.caretAt(1, 0, v.affinity)
	b, actual, ok := v.BoundsForRange(ui.TextInputRange{Start: 1, End: 1})
	if !ok || actual != (ui.TextInputRange{Start: 1, End: 1}) {
		t.Fatalf("bounds %+v actual %+v ok %v", b, actual, ok)
	}
	if b.X != v.padX+x || b.Y != v.padY+float32(y-v.scroll) || b.H != h {
		t.Fatalf("bounds %+v, caret %v %v %v", b, x, y, h)
	}
	if at, ok := v.IndexForPoint(ui.Point{X: b.X, Y: b.Y}); !ok || at != 1 {
		t.Fatalf("index %d ok %v", at, ok)
	}
}

func TestMarkerRevealHardBreaks(t *testing.T) {
	for _, c := range []struct {
		src, marker string
		at          int
	}{
		{"a  \nb\n", "↓", 1},
		{"a\\\nb\n", "\\", 1},
	} {
		_, v := markerView(c.src)
		setSourceCaret(v, c.at)
		display := v.displayParagraph(0, Attributes{})
		got := displayText(display)
		if !strings.Contains(got, c.marker) {
			t.Errorf("%q: display %q lacks %q: %+v", c.src, got, c.marker, v.boxes(0))
		}
		muted := false
		for _, span := range display.Spans {
			if span.Text == c.marker && span.Color == v.look.Muted {
				muted = true
			}
		}
		if !muted {
			t.Errorf("%q: marker is not muted: %+v", c.src, display.Spans)
		}
	}
}

func TestMarkerRevealArrowNavigation(t *testing.T) {
	_, v := markerView("**alpha** tail\n")
	v.doc.SetSelection(0, 0)
	v.sync()
	for _, want := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9} {
		if !v.stepDisplay(false, false) {
			t.Fatalf("right did not move at %d", want-1)
		}
		if got := v.caretSource(); got != want {
			t.Fatalf("right source %d, want %d", got, want)
		}
	}
	if got := len(v.revealedBoxes(0)); got != 1 {
		t.Fatalf("closing boundary revealed %d boxes", got)
	}
	for _, want := range []int{8, 7, 6, 5, 4, 3, 2, 1, 0} {
		if !v.stepDisplay(true, false) {
			t.Fatalf("left did not move at %d", want+1)
		}
		if got := v.caretSource(); got != want {
			t.Fatalf("left source %d, want %d", got, want)
		}
	}
}

func TestMarkerRevealWordJumpCollapses(t *testing.T) {
	word := ui.Alt
	if runtime.GOOS != "darwin" {
		word = ui.Ctrl
	}
	for _, key := range []ui.Key{ui.KeyRight, ui.KeyLeft} {
		_, v := markerView("**alpha** tail\n")
		if key == ui.KeyRight {
			v.doc.SetSelection(0, 0)
		} else {
			v.doc.SetSelection(v.shown.Len(), v.shown.Len())
		}
		v.sync()
		if !v.key(key, word) {
			t.Fatalf("%v word jump was not handled", key)
		}
		if got := v.revealedBoxes(0); len(got) != 0 {
			t.Fatalf("%v word jump revealed %+v", key, got)
		}
		v.key(ui.KeyRight, 0)
		if v.wordMove {
			t.Fatalf("%v normal arrow kept word movement active", key)
		}
	}
}

func TestReplaceSourceDirectMarkerEditAndUndo(t *testing.T) {
	d := NewMarkdownDocument("a **alpha** tail\n")
	if err := d.ReplaceSource(2, 3, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.Source(); got != "a *alpha** tail\n" {
		t.Fatalf("source %q", got)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	d.Undo()
	if got := d.Source(); got != "a **alpha** tail\n" {
		t.Fatalf("undo %q", got)
	}
	d.Redo()
	if got := d.Source(); got != "a *alpha** tail\n" {
		t.Fatalf("redo %q", got)
	}
}

func TestReplaceSourceChangesAttributesAndSelection(t *testing.T) {
	d := NewMarkdownDocument("**alpha** tail\n")
	d.SetSelection(1, 2)
	var err error
	d.Edit(func(e *Edit) {
		e.ReplaceSource(0, 9, "_alpha_")
		e.Select(1, 1)
		err = e.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Source(); got != "_alpha_ tail\n" {
		t.Fatalf("source %q", got)
	}
	if got := d.Text().String(); got != "alpha tail" {
		t.Fatalf("text %q", got)
	}
	if err := d.CheckSource(); err != nil {
		t.Fatal(err)
	}
	d.Undo()
	if got := d.Source(); got != "**alpha** tail\n" {
		t.Fatalf("undo source %q", got)
	}
	if a, c := d.Selection(); a != 1 || c != 2 {
		t.Fatalf("undo selection %d-%d", a, c)
	}
	d.Redo()
	if got := d.Source(); got != "_alpha_ tail\n" {
		t.Fatalf("redo source %q", got)
	}
	if a, c := d.Selection(); a != 1 || c != 1 {
		t.Fatalf("redo selection %d-%d", a, c)
	}
}

func TestMarkerRevealMarkerByteEdits(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(tt *ui.Tester)
		want string
	}{
		{"type between opening stars", func(tt *ui.Tester) {
			tt.Key(0, ui.KeyRight)
			tt.Type("X")
		}, "*X*alpha**\n"},
		{"delete opening star", func(tt *ui.Tester) {
			tt.Key(0, ui.KeyDelete)
		}, "*alpha**\n"},
		{"backspace closing star", func(tt *ui.Tester) {
			tt.Key(0, ui.KeyRight)
			tt.Key(0, ui.KeyRight)
			tt.Key(0, ui.KeyBackspace)
		}, "*alpha**\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument("**alpha**\n")
			tt, _ := richTester(t, d)
			d.SetSelection(0, 0)
			tt.Frame()
			c.edit(tt)
			if got := d.Source(); got != c.want {
				t.Fatalf("source %q, want %q", got, c.want)
			}
			if err := d.CheckSource(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarkerRevealLinkDestinationEdits(t *testing.T) {
	for _, c := range []struct {
		name   string
		rights int
		key    ui.Key
		text   string
		want   string
	}{
		{"type URL", 8, 0, "X", "[alpha](Xurl \"title\")\n"},
		{"delete URL", 8, ui.KeyDelete, "", "[alpha](rl \"title\")\n"},
		{"type title", 13, 0, "X", "[alpha](url \"Xtitle\")\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument("[alpha](url \"title\")\n")
			tt, _ := richTester(t, d)
			d.SetSelection(0, 0)
			tt.Frame()
			for range c.rights {
				tt.Key(0, ui.KeyRight)
			}
			if c.key != 0 {
				tt.Key(0, c.key)
			}
			if c.text != "" {
				tt.Type(c.text)
			}
			if got := d.Source(); got != c.want {
				t.Fatalf("source %q, want %q", got, c.want)
			}
			if err := d.CheckSource(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarkerRevealDirectSourceCaretMapping(t *testing.T) {
	d, v := markerView("[alpha](url \"title\")\n")
	setSourceCaret(v, 10)
	v.replaceSource(10, 10, "X", 11)
	if got := v.caretSource(); got != 11 {
		t.Fatalf("caret source %d", got)
	}
	if got := d.SourceSelection().Caret.Offset; got != 11 {
		t.Fatalf("document caret source %d", got)
	}
	pv := v.paraLayout(0)
	want := pv.display.Map.DisplayOfSource(11, false)
	if got := v.local(0, v.caret); got != want {
		t.Fatalf("caret display %d, want %d", got, want)
	}
}

func TestMarkerRevealTypingInsideDelimiter(t *testing.T) {
	d, v := markerView("**alpha**\n")
	_ = v
	tt, _ := richTester(t, d)
	d.SetSelection(0, 0)
	tt.Frame()
	tt.Key(0, ui.KeyRight)
	tt.Type("X")
	if got := d.Source(); got != "*X*alpha**\n" {
		t.Fatalf("source %q", got)
	}
}

func TestMarkerRevealRightAffinity(t *testing.T) {
	for _, c := range []struct {
		src, typed, want string
		left, inside     bool
	}{
		{"**alpha**\n", "X", "**alpha**X\n", false, false},
		{"**alpha**\n", "X", "X**alpha**\n", true, false},
		{"**alpha**\n", "X", "**Xalpha**\n", false, true},
		{"__alpha__\n", "X", "__alpha__X\n", false, false},
		{"*alpha*\n", "X", "*alpha*X\n", false, false},
		{"_alpha_\n", "X", "_alpha_X\n", false, false},
		{"~~alpha~~\n", "X", "~~alpha~~X\n", false, false},
		{"<u>alpha</u>\n", "X", "<u>alpha</u>X\n", false, false},
		{"`alpha`\n", "Y", "`alpha`Y\n", false, false},
		{"[alpha](u)\n", "Y", "[alpha](u)Y\n", false, false},
	} {
		d, v := markerView(c.src)
		tt, _ := richTester(t, d)
		d.SetSelection(0, 0)
		tt.Frame()
		if c.left {
			tt.Key(0, ui.KeyLeft)
		} else if c.inside {
			tt.Key(0, ui.KeyRight)
			tt.Key(0, ui.KeyRight)
		} else {
			steps := 0
			if boxes := v.boxes(0); len(boxes) > 0 {
				steps = boxes[0].end
			}
			for range steps {
				tt.Key(0, ui.KeyRight)
			}
		}
		tt.Type(c.typed)
		if got := d.Source(); got != c.want {
			t.Errorf("%q: got %q want %q", c.src, got, c.want)
		}
	}
}

func TestMarkerRevealClickAffinity(t *testing.T) {
	for _, c := range []struct {
		name string
		dx   float32
		want string
	}{
		{"last character right half", -1, "A **alphaX** tail\n"},
		{"space after element", 1, "A **alpha**X tail\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, v := markerView("A **alpha** tail\n")
			d.SetSelection(0, 0)
			v.sync()
			v.layout(400, 200)
			x, y, h := v.caretAt(7, 0, ui.TextDownstream)
			v.press(ui.InputEvent{
				Kind: ui.InputPointerDown, Button: 0, Clicks: 1,
				X: x + c.dx + v.padX, Y: float32(y+float64(h)/2) + v.padY - float32(v.scroll),
			})
			v.release(ui.InputEvent{Kind: ui.InputPointerUp})
			v.insert("X")
			if got := d.Source(); got != c.want {
				t.Fatalf("source %q, want %q", got, c.want)
			}
		})
	}
}

func TestMarkerRevealClickInsideLinkDestination(t *testing.T) {
	d, v := markerView("[alpha](url \"title\")\n")
	setSourceCaret(v, 1)
	v.layout(800, 400)
	pv := v.paraLayout(0)
	l := pv.display.Map.DisplayOfSource(9, false)
	r := pv.layout.CaretAt(ui.TextCaretPosition{Index: pv.units[l], Affinity: ui.TextDownstream})
	ox, oy := v.origin(0)
	v.press(ui.InputEvent{
		Kind: ui.InputPointerDown, Button: 0, Clicks: 1,
		X: ox + r.X + v.padX + 1, Y: float32(oy+float64(r.Y+r.H/2)) + v.padY - float32(v.scroll),
	})
	source := d.SourceSelection().Caret.Offset
	if source < 8 || source > 14 {
		t.Fatalf("caret source %d is outside the destination", source)
	}
	pv = v.paraLayout(0)
	at := pv.display.Map.DisplayOfSource(source, false)
	if o := pv.display.Map.At(at); o.Source != source {
		t.Fatalf("source %d maps to display %d as %+v", source, at, o)
	}
}

func TestMarkerRevealEmptyContainerRetentionAndTyping(t *testing.T) {
	d, _ := markerView("**a**\n")
	tt, _ := richTester(t, d)
	d.SetSelection(1, 1)
	tt.Frame()
	tt.Key(0, ui.KeyBackspace)
	if got := d.Source(); got != "****\n" {
		t.Fatalf("retained source %q", got)
	}
	tt.Type("new")
	if got := d.Source(); got != "**new**\n" {
		t.Fatalf("typed source %q", got)
	}
}

func TestMarkerRevealEmptyContainerDeleteSelection(t *testing.T) {
	d := NewMarkdownDocument("**a**\n")
	tt, _ := richTester(t, d)
	d.SetSelection(0, 1)
	tt.Frame()
	tt.Key(0, ui.KeyDelete)
	if got := d.Source(); got != "****\n" {
		t.Fatalf("retained source %q", got)
	}
}

func TestMarkerRevealEmptyContainersAllKinds(t *testing.T) {
	for _, c := range []struct {
		name  string
		src   string
		empty string
		typed string
	}{
		{"strong star", "**a**\n", "****\n", "**new**\n"},
		{"strong underscore", "__a__\n", "____\n", "__new__\n"},
		{"emphasis star", "*a*\n", "**\n", "*new*\n"},
		{"emphasis underscore", "_a_\n", "__\n", "_new_\n"},
		{"code", "`a`\n", "``\n", "`new`\n"},
		{"strikethrough", "~~a~~\n", "~~~~\n", "~~new~~\n"},
		{"link", "[a](u)\n", "[](u)\n", "[new](u)\n"},
		{"html", "<u>a</u>\n", "<u></u>\n", "<u>new</u>\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := NewMarkdownDocument(c.src)
			tt, _ := richTester(t, d)
			a, z := d.Text().ParagraphRange(0)
			d.SetSelection(a, z)
			tt.Frame()
			tt.Key(0, ui.KeyBackspace)
			if got := d.Source(); got != c.empty {
				t.Fatalf("retained source %q, want %q", got, c.empty)
			}
			tt.Type("new")
			if got := d.Source(); got != c.typed {
				t.Fatalf("typed source %q, want %q", got, c.typed)
			}
			if err := d.CheckSource(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMarkerRevealEmptyContainerDisplay(t *testing.T) {
	d, v := markerView("~~alpha~~\n")
	var strike box
	for _, b := range v.boxes(0) {
		if b.kind == MarkerStrikethrough {
			strike = b
		}
	}
	v.doc.SetSelection(0, v.shown.Len())
	v.sync()
	v.replaceBoxContent(strike)
	if got := d.Source(); got != "~~~~\n" {
		t.Fatalf("source %q", got)
	}
	if got := displayText(v.displayParagraph(0, Attributes{})); !strings.Contains(got, "~~~~") {
		t.Fatalf("display %q", got)
	}
	if got := d.SourceSelection().Caret.Offset; got != 2 {
		t.Fatalf("caret source %d", got)
	}
}

func TestMarkerRevealEmptyContainerUndo(t *testing.T) {
	d := NewMarkdownDocument("**a**\n")
	tt, _ := richTester(t, d)
	d.SetSelection(0, 1)
	tt.Frame()
	tt.Key(0, ui.KeyBackspace)
	if got := d.Source(); got != "****\n" {
		t.Fatalf("source %q", got)
	}
	tt.Key(ui.Cmd, ui.KeyZ)
	if got := d.Source(); got != "**a**\n" {
		t.Fatalf("undo source %q", got)
	}
	if a, c := d.Selection(); a != 0 || c != 1 {
		t.Fatalf("undo selection %d-%d", a, c)
	}
	tt.SetFocused(false)
	if got := d.Source(); got != "**a**\n" {
		t.Fatalf("focus after undo source %q", got)
	}
	tt.SetFocused(true)
	tt.Key(ui.Cmd|ui.Shift, ui.KeyZ)
	if got := d.Source(); got != "****\n" {
		t.Fatalf("redo source %q", got)
	}
	if a, c := d.Selection(); a != 0 || c != 0 {
		t.Fatalf("redo selection %d-%d", a, c)
	}
}

func TestMarkerRevealEmptyContainerCleanup(t *testing.T) {
	for _, key := range []ui.Key{ui.KeyBackspace, ui.KeyDelete} {
		d, _ := markerView("**a**\n")
		tt, _ := richTester(t, d)
		d.SetSelection(1, 1)
		tt.Frame()
		tt.Key(0, ui.KeyBackspace)
		tt.Key(0, key)
		if got := d.Source(); got != "\n" {
			t.Errorf("%v cleanup source %q", key, got)
		}
		if got := d.TypingAttributes(); got != (Attributes{}) {
			t.Errorf("%v left typing attributes %+v", key, got)
		}
	}
}

func TestMarkerRevealEmptyContainerLeaves(t *testing.T) {
	t.Run("arrow", func(t *testing.T) {
		// The pair's right edge is still the pair; only a caret past it
		// leaves (record 2026-11-10, probe/pair/right-to-end). The text
		// after the pair gives the arrow somewhere to go.
		d := NewMarkdownDocument("**a** tail\n")
		tt, _ := richTester(t, d)
		a, _ := d.Text().ParagraphRange(0)
		d.SetSelection(a, a+1)
		tt.Frame()
		tt.Key(0, ui.KeyBackspace)
		for range 4 {
			tt.Key(0, ui.KeyRight)
		}
		if got := d.Source(); got != " tail\n" {
			t.Fatalf("source %q", got)
		}
		if got := d.TypingAttributes(); got != (Attributes{}) {
			t.Fatalf("typing attributes %+v", got)
		}
	})
	t.Run("focus", func(t *testing.T) {
		d := NewMarkdownDocument("**a**\n")
		tt, _ := richTester(t, d)
		a, z := d.Text().ParagraphRange(0)
		d.SetSelection(a, z)
		tt.Frame()
		tt.Key(0, ui.KeyBackspace)
		tt.SetFocused(false)
		if got := d.Source(); got != "\n" {
			t.Fatalf("source %q", got)
		}
		if got := d.TypingAttributes(); got != (Attributes{}) {
			t.Fatalf("typing attributes %+v", got)
		}
	})
	t.Run("mouse", func(t *testing.T) {
		d := NewMarkdownDocument("**a** tail\n")
		tt, _ := richTester(t, d)
		d.SetSelection(0, 1)
		tt.Frame()
		tt.Key(0, ui.KeyBackspace)
		tt.ClickAt(350, 20)
		if got := d.Source(); got != " tail\n" {
			t.Fatalf("source %q", got)
		}
		if got := d.TypingAttributes(); got != (Attributes{}) {
			t.Fatalf("typing attributes %+v", got)
		}
	})
}

func TestMarkerRevealLinkPlainClickAndCommandClick(t *testing.T) {
	opened := ""
	d, _ := markerView("[site](https://example.com)\n")
	d.SetSelection(d.Text().Len(), d.Text().Len())
	tt := ui.NewTester(func(c *ui.Context) {
		Editor(c, d, EditorOptions{OnLink: func(url string) { opened = url }}).Fill().Changed()
	}, 400, 200)
	x, y, ok := findPointer(tt)
	if !ok {
		t.Fatal("no link hit")
	}
	tt.ClickAt(x, y)
	if opened != "" {
		t.Fatalf("plain click opened %q", opened)
	}
	if a, c := d.Selection(); a != c || c == d.Text().Len() {
		t.Fatalf("plain click selection %d-%d", a, c)
	}
	mod := ui.Cmd
	if runtime.GOOS != "darwin" {
		mod = ui.Ctrl
	}
	tt.Advance(time.Second)
	tt.ClickAtWith(mod, x, y)
	if opened != "https://example.com" {
		t.Fatalf("command click opened %q", opened)
	}
}

func TestMarkerRevealLinkHoverState(t *testing.T) {
	d, v := markerView("pre [site](https://example.com) post\n")
	d.SetSelection(0, 0)
	v.sync()
	v.layout(800, 400)
	x, y, h := v.caretAt(5, 0, ui.TextDownstream)
	v.move(ui.InputEvent{X: x + v.padX, Y: float32(y+float64(h)/2) + v.padY - float32(v.scroll)})
	if v.hoverPara != 0 || v.hoverBox < 0 {
		t.Fatalf("hover state %d %d", v.hoverPara, v.hoverBox)
	}
	if got := displayText(v.displayParagraph(0, Attributes{})); !strings.Contains(got, "[site](https://example.com)") {
		t.Fatalf("hover display %q", got)
	}
	if a, c := d.Selection(); a != 0 || c != 0 {
		t.Fatalf("hover moved document selection to %d-%d", a, c)
	}
	d.SetSelection(0, v.shown.Len())
	v.sync()
	if got := displayText(v.displayParagraph(0, Attributes{})); !strings.Contains(got, "[site](https://example.com)") {
		t.Fatalf("hover display with selection %q", got)
	}
}

func TestMarkerRevealHeadingFragment(t *testing.T) {
	d, _ := markerView("# First\n\n[go](#second)\n\n## Second\n")
	tt := ui.NewTester(func(c *ui.Context) {
		Editor(c, d).Fill()
	}, 400, 240)
	mod := ui.Cmd
	if runtime.GOOS != "darwin" {
		mod = ui.Ctrl
	}
	x, y, ok := findPointer(tt)
	if !ok {
		t.Fatal("no heading link hit")
	}
	tt.ClickAtWith(mod, x, y)
	if _, caret := d.Selection(); caret != strings.Index(d.Text().String(), "Second") {
		t.Fatalf("caret %d", caret)
	}
}

// A selection that extends leftward over the empty pair Cmd+B left keeps its
// bytes and draws them as nothing (record 2026-11-10, seed7/c15).
func TestMarkerRevealEmptyContainerKeptUnderSelection(t *testing.T) {
	d := NewMarkdownDocument("")
	tt, _ := richTester(t, d)
	tt.Frame()
	tt.Key(0, ui.KeyRight)
	tt.Key(ui.Cmd, ui.KeyB)
	tt.Frame()
	if got := d.Source(); got != "****" {
		t.Fatalf("pair source %q", got)
	}
	for range 3 {
		tt.Key(ui.Shift, ui.KeyLeft)
		tt.Frame()
	}
	if got := d.Source(); got != "****" {
		t.Fatalf("selection source %q", got)
	}
	if sel := d.SourceSelection(); sel.Anchor != (SourcePoint{2, BoundaryUpstream}) ||
		sel.Caret != (SourcePoint{0, BoundaryDownstream}) {
		t.Fatalf("selection %+v", sel)
	}
	if got := displayText(d.activeView().displayParagraph(0, Attributes{})); got != "" {
		t.Fatalf("display %q", got)
	}
}
