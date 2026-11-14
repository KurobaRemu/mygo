package parser

import "testing"

// TestIssue187AdjacentStrongRuns pins seed23/c6: Typora's live editor reads
// each self-pasted `****` group as its own strong boundary. The second group's
// stars stay literal instead of pairing across the first group's closing run
// (VM records 2026-11-14, record-20261114-033056 / record-20261114-033730).
func TestIssue187AdjacentStrongRuns(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"> alpha**u****u****wd****wd**\n", "<blockquote>\n<p>alpha<strong>u*</strong>*u****wd****wd**</p>\n</blockquote>\n"},
		{"alpha**u****u****wd****wd**\n", "<p>alpha<strong>u*</strong>*u****wd****wd**</p>\n"},
		{"> alpha**u****u****wd**\n", "<blockquote>\n<p>alpha<strong>u*</strong>*u****wd**</p>\n</blockquote>\n"},
		{"**a*****b**\n", "<p><strong>a*</strong><strong>b</strong></p>\n"},
		{"x**a****b**\n", "<p>x<strong>a*</strong>*b**</p>\n"},
		{"***a***\n", "<p><em><strong>a</strong></em></p>\n"},
		{"**a**\n", "<p><strong>a</strong></p>\n"},
		{"*a*\n", "<p><em>a</em></p>\n"},
	} {
		if got := render(tt.src, Typora); got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.src, got, tt.want)
		}
	}
}
