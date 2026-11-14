package richtext

import "testing"

func TestBlankLineSeparatesBlocksTightListDoesNot(t *testing.T) {
	_, v := markerView("one\n\ntwo\n\n- x\n- y\n\n> a\n>\n> b\n")
	v.look.size = 14
	gap := func(p int) float32 {
		above, _ := v.space(p, v.para(p).style)
		return above
	}
	if gap(0) != 0 {
		t.Errorf("first paragraph has %v above", gap(0))
	}
	for _, p := range []int{1, 2, 4, 5} {
		if gap(p) == 0 {
			t.Errorf("paragraph %d after a blank line has no room above", p)
		}
	}
	if gap(3) != 0 {
		t.Errorf("item of a tight list has %v above", gap(3))
	}
}
