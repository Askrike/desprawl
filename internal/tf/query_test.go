package tf

import "testing"

func TestHungDetail(t *testing.T) {
	got := hungDetail(0, []string{"list.x.q0: Starting query..."}, "a\nb\nc\nd\ne\nf\ng\n")
	want := " (0 found before stopping); last messages: list.x.q0: Starting query...; stderr: c | d | e | f | g"
	if got != want {
		t.Errorf("hungDetail = %q, want %q", got, want)
	}
	if got, want := hungDetail(3, nil, ""), " (3 found before stopping)"; got != want {
		t.Errorf("hungDetail = %q, want %q", got, want)
	}
}
