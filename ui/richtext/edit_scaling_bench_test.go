package richtext

// These benchmarks measure ticket 65: how the cost of one keystroke grows
// with the size of a Markdown document. They separate the edit path (the
// document edit alone, without drawing) from the layout path (a frame of an
// input method's composition, which does not edit the document), and the
// end-to-end frames of a typed rune and of an input method's commit.
//
// Tiers span two orders of magnitude. The real-corpus tiers concatenate the
// documents of fidelity/testdata/corpus, so the fixtures have the shapes
// Markdown actually takes (headings, lists, fences, tables, links), not the
// shapes a generator would like.
//
// The numbers are host reference values: they do not share the VM's
// measurement method or hardware with the phase-2 data of ticket 61, so the
// two sets cannot be compared directly.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown/parser"
)

// benchTier is one document size measured by the benchmarks below.
type benchTier struct {
	name string
	src  string
}

// corpusDir holds the fidelity corpus, relative to this package's directory.
const corpusDir = "fidelity/testdata/corpus"

// markdownParagraphs returns Markdown of n generated paragraph bodies with
// the mix a note has: headings, list items, quotes, fenced code, and the
// inline emphasis, code and links real prose carries.
func markdownParagraphs(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch i % 12 {
		case 0:
			fmt.Fprintf(&b, "## Section %d: notes on layout\n\n", i)
		case 3:
			fmt.Fprintf(&b, "- Item %d with **bold words**, an *italic one* and `code`.\n\n", i)
		case 4:
			fmt.Fprintf(&b, "- [Link %d](https://example.com/%d) in a list item long enough to wrap.\n\n", i, i)
		case 7:
			fmt.Fprintf(&b, "> Quote %d with **bold words** and `code` in it.\n\n", i)
		case 9:
			fmt.Fprintf(&b, "```go\nfunc f%d() int { return %d }\n```\n\n", i, i)
		default:
			fmt.Fprintf(&b, "Paragraph %d has **bold words**, an *italic one*, and a [link](https://example.com) in a line long enough to wrap in a narrow window, so that the paragraph wraps in the editor's column as prose does.\n\n", i)
		}
	}
	return b.String()
}

// corpusFiles returns the corpus's Markdown files, in path order.
func corpusFiles(b *testing.B) []string {
	var files []string
	err := filepath.WalkDir(corpusDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// corpusSource concatenates files until it holds at least target bytes, or
// all of them when target is zero.
func corpusSource(b *testing.B, files []string, target int) string {
	var bld strings.Builder
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
		bld.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			bld.WriteByte('\n')
		}
		bld.WriteByte('\n')
		if target > 0 && bld.Len() >= target {
			break
		}
	}
	return bld.String()
}

// benchTiers returns the documents of the benchmarks: three generated sizes
// spanning two orders of magnitude, and two tiers of the real corpus (an
// eighth of it and all of it).
func benchTiers(b *testing.B) []benchTier {
	files := corpusFiles(b)
	total := 0
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		total += int(info.Size())
	}
	return []benchTier{
		{"synth-1k", markdownParagraphs(1000)},
		{"synth-10k", markdownParagraphs(10000)},
		{"synth-100k", markdownParagraphs(100000)},
		{"real-corpus-1of8", corpusSource(b, files, total/8)},
		{"real-corpus", corpusSource(b, files, 0)},
	}
}

// tierMetrics reports the document size and projected paragraph count of a
// tier in the benchmark's output, so the slope can be read per byte and per
// paragraph.
func tierMetrics(b *testing.B, src string, paras int) {
	b.ReportMetric(float64(len(src)), "src-B")
	b.ReportMetric(float64(paras), "paras")
}

// tierBlocks reports a tier's source size and top-level block count, which
// is what the parser copies after an edit.
func tierBlocks(b *testing.B, src string, blocks int) {
	b.ReportMetric(float64(len(src)), "src-B")
	b.ReportMetric(float64(blocks), "blocks")
}

// BenchmarkMarkdownEdit measures the edit path alone: one document edit at
// the middle, inserting a rune and taking it back on alternating
// iterations, without drawing, so the document stays the same size. This is
// where the two full-source works of ticket 65 are: the parser's source copy
// and the changedBytes scan.
func BenchmarkMarkdownEdit(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			d := NewMarkdownDocument(tier.src)
			paras := d.Text().ParagraphCount()
			at := d.Text().Len() / 2
			insert := true
			b.ReportAllocs()
			for b.Loop() {
				if insert {
					d.Edit(func(e *Edit) { e.ReplaceString(at, at, "x", Attributes{}) })
				} else {
					d.Edit(func(e *Edit) { e.ReplaceString(at, at+1, "", Attributes{}) })
				}
				insert = !insert
			}
			tierMetrics(b, tier.src, paras)
		})
	}
}

// BenchmarkMarkdownParserEdit measures the parser's share of the edit path:
// the same alternating edit, through parser.Document.Edit alone, without
// the projection, the source map, or the document's undo state. It lives
// beside the other benchmarks so every tier shares one corpus reader, and it
// uses the parser's public API only.
func BenchmarkMarkdownParserEdit(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			p := parser.Parse(tier.src)
			blocks := p.Len() - 1
			at := len(tier.src) / 2
			for !utf8.RuneStart(tier.src[at]) {
				at++
			}
			insert := true
			b.ReportAllocs()
			for b.Loop() {
				if insert {
					p.Edit(at, at, "x")
				} else {
					p.Edit(at, at+1, "")
				}
				insert = !insert
			}
			tierBlocks(b, tier.src, blocks)
		})
	}
}

// BenchmarkMarkdownMarkFrame measures the layout path alone: an input
// method's composition at the middle of the document, which changes no
// document state, and the frame that lays it out and draws it. It is the
// host counterpart of the mark handles of ticket 61.
func BenchmarkMarkdownMarkFrame(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			doc := NewMarkdownDocument(tier.src)
			paras := doc.Text().ParagraphCount()
			tt := ui.NewTester(func(c *ui.Context) { Editor(c, doc).Fill() }, 1000, 800)
			tt.Press(500, 400)
			tt.Release(500, 400)
			mid := doc.Text().Len() / 2
			doc.SetSelection(mid, mid)
			tt.Frame()
			compose := []string{"n", "ni"}
			i := 0
			b.ReportAllocs()
			for b.Loop() {
				s := compose[i&1]
				tt.Compose(s, len(s))
				i++
			}
			tierMetrics(b, tier.src, paras)
		})
	}
}

// BenchmarkMarkdownTypeFrame measures a typing frame end to end: the edit
// path and the layout path of one typed rune at the middle, as a key that
// commits text does.
func BenchmarkMarkdownTypeFrame(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			doc := NewMarkdownDocument(tier.src)
			paras := doc.Text().ParagraphCount()
			tt := ui.NewTester(func(c *ui.Context) { Editor(c, doc).Fill() }, 1000, 800)
			tt.Press(500, 400)
			tt.Release(500, 400)
			mid := doc.Text().Len() / 2
			doc.SetSelection(mid, mid)
			tt.Frame()
			b.ReportAllocs()
			for b.Loop() {
				tt.Type("x")
			}
			tierMetrics(b, tier.src, paras)
		})
	}
}

// BenchmarkMarkdownCommitFrame measures an input method's commit end to
// end: a composition at the middle, then the rune that commits it. It is
// the host counterpart of the commit handles of ticket 61; subtracting
// BenchmarkMarkdownMarkFrame's cost leaves the commit path.
func BenchmarkMarkdownCommitFrame(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			doc := NewMarkdownDocument(tier.src)
			paras := doc.Text().ParagraphCount()
			tt := ui.NewTester(func(c *ui.Context) { Editor(c, doc).Fill() }, 1000, 800)
			tt.Press(500, 400)
			tt.Release(500, 400)
			mid := doc.Text().Len() / 2
			doc.SetSelection(mid, mid)
			tt.Frame()
			b.ReportAllocs()
			for b.Loop() {
				tt.Compose("ni", 2)
				tt.Type("你")
			}
			tierMetrics(b, tier.src, paras)
		})
	}
}

// BenchmarkSelectionSnap measures the view path's per-key selection
// snapping: clampSourceSelectionLocked walks the top-level block list once
// for setext underlines and once for fenced code markers, at both ends of
// the selection. It is O(blocks), not O(bytes), and Document.Edit alone does
// not pay it.
func BenchmarkSelectionSnap(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			d := NewMarkdownDocument(tier.src)
			paras := d.Text().ParagraphCount()
			sel := sourceCaret(len(d.Source()) / 2)
			b.ReportAllocs()
			d.mu.Lock()
			for b.Loop() {
				d.assignSourceSelection(sel, false)
			}
			d.mu.Unlock()
			tierMetrics(b, tier.src, paras)
		})
	}
}

// benchChanged keeps changedBytes's results alive.
var benchChanged [3]int

// BenchmarkChangedBytes measures the second full-source work of the edit
// path on its own: the two-ended scan that finds the changed region between
// the source before and after one insertion in the middle.
func BenchmarkChangedBytes(b *testing.B) {
	for _, tier := range benchTiers(b) {
		b.Run(tier.name, func(b *testing.B) {
			a := tier.src
			at := len(a) / 2
			for !utf8.RuneStart(a[at]) {
				at++
			}
			changed := a[:at] + "x" + a[at:]
			b.ReportAllocs()
			for b.Loop() {
				benchChanged[0], benchChanged[1], benchChanged[2] = changedBytes(a, changed)
			}
			b.ReportMetric(float64(len(a)), "src-B")
		})
	}
}
