//go:build darwin && !ios

package text

import (
	"strconv"
	"strings"
	"testing"
)

// BenchmarkCoreTextWindowParagraph wraps one paragraph of n bytes, as a
// long paste or a minified line does: English, Chinese, and English in
// runs of bold, italic and monospaced text. windowThreshold of 1<<30
// keeps the whole paragraph's typesetter, as before windows.
func BenchmarkCoreTextWindowParagraph(b *testing.B) {
	english := "The quick brown fox jumps over the lazy dog, while 42 typists in 1987 wrote flowing prose about kerning, ligatures and office affairs. "
	chinese := "长途综合对比卡片需要在一个超长段落里排版，中文没有空格，标点“如此”，数字123和English混排。"
	looks := []Span{{Weight: 700}, {Italic: true}, {Family: "Menlo"}, {}}
	rich := func(n int) []Span {
		var spans []Span
		for end, i := 23, 0; end < n+23; end, i = end+23, i+1 {
			sp := looks[i%len(looks)]
			sp.End = min(end, n)
			spans = append(spans, sp)
		}
		return spans
	}
	for _, n := range []int{50 << 10, 200 << 10, 1 << 20} {
		for _, c := range []struct {
			name string
			text string
			rich bool
		}{{"english", english, false}, {"chinese", chinese, false}, {"rich", english, true}} {
			text := strings.Repeat(c.text, n/len(c.text)+1)[:n]
			text = strings.ToValidUTF8(text, "")
			var spans []Span
			if c.rich {
				spans = rich(len([]rune(text)))
			}
			for _, mode := range []struct {
				name      string
				threshold int
			}{{"windows", windowThreshold}, {"whole", 1 << 30}} {
				if mode.threshold != windowThreshold && n > 200<<10 {
					continue
				}
				b.Run(c.name+"/"+sizeName(n)+"/"+mode.name, func(b *testing.B) {
					old := windowThreshold
					windowThreshold = mode.threshold
					defer func() { windowThreshold = old }()
					s := newSystem()
					p := Params{Text: text, Width: 480, Style: Style{Size: 14}, Spans: EncodeSpans(spans)}
					b.ReportAllocs()
					for b.Loop() {
						s.ReleaseShaping()
						s.mu.Lock()
						s.layout(p)
						s.mu.Unlock()
					}
				})
			}
		}
	}
}

func sizeName(n int) string {
	if n >= 1<<20 {
		return "1MiB"
	}
	return strconv.Itoa(n>>10) + "KiB"
}
