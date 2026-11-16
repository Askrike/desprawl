package version

import "testing"

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "v0.1.9", true}, {"v0.1.10", "v0.1.9", true}, {"0.2.0", "v0.2.0", false},
		{"v0.1.9", "v0.2.0", false}, {"", "v0.1.0", false}, {"v0.1.0", "dev", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
