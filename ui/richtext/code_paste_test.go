package richtext

import "testing"

func TestPastePlainInCodeBlockKeepsBytes(t *testing.T) {
	src := "```py\ndef f(a, b):\n    return a / b\n```\n"
	for _, tc := range []struct{ paste, want string }{
		{"return a / b if b else 0", "```py\ndef f(a, b):\n    return a / b if b else 0\n```\n"},
		{"x = *a*\ny = [b]", "```py\ndef f(a, b):\n    x = *a*\ny = [b]\n```\n"},
	} {
		d := NewMarkdownDocument(src)
		at := runeAtText(t, d, "return a / b", 0)
		end := runeAtText(t, d, "return a / b", len("return a / b"))
		if err := d.PastePlain(at, end, tc.paste); err != nil {
			t.Fatal(err)
		}
		assertSource(t, d, tc.want)
	}
	for _, tc := range []struct{ src, want string }{
		{"- item\n\n  ```\n  old\n  ```\n", "- item\n\n  ```\n  x\n  y\n  ```\n"},
		{"> ```\n> old\n> ```\n", "> ```\n> x\n> y\n> ```\n"},
	} {
		d := NewMarkdownDocument(tc.src)
		at := runeAtText(t, d, "old", 0)
		if err := d.PastePlain(at, at+3, "x\ny"); err != nil {
			t.Fatal(err)
		}
		assertSource(t, d, tc.want)
	}
}
