package discover

import (
	"strings"
	"testing"
)

func TestWithProviderAttrs(t *testing.T) {
	src := "terraform {}\n\nprovider \"p\" {\n  region = \"r\"\n  max_retries = 9\n}\n\nprovider \"q\" {\n}\n"
	b, err := withProviderAttrs([]byte(src), map[string]any{"max_retries": float64(4), "flag": true}, "")
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"max_retries = 9", "max_retries = 4", "flag        = true"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "max_retries") != 2 {
		t.Errorf("an attribute providers.tf sets was overwritten:\n%s", got)
	}
	if _, err := withProviderAttrs([]byte(src), map[string]any{"x": []any{1}}, ""); err == nil {
		t.Error("want an error for a list value")
	}
}
