package tfmap

import (
	"slices"
	"strings"
	"testing"
)

func TestChangedAttrs(t *testing.T) {
	a := "resource \"x\" \"y\" {\n  runtime = \"a\"\n  env {\n    k = 1\n  }\n  name = \"n\"\n}\n"
	b := "resource \"x\" \"y\" {\n  runtime    = \"b\"\n  env {\n    k = 2\n  }\n\n  name   = \"n\"\n}\n"
	if got := changedAttrs(a, b); !slices.Equal(got, []string{"runtime", "env"}) {
		t.Errorf("changedAttrs = %v, want [runtime env] (realigned name is a formatting change)", got)
	}
	if d := blockDiff(a, b); !strings.Contains(d, "-   runtime = \"a\"") || !strings.Contains(d, "+   runtime    = \"b\"") {
		t.Errorf("blockDiff:\n%s", d)
	}
}
