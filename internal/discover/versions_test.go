package discover

import (
	"testing"

	"github.com/askrike/desprawl/internal/model"
)

func TestVersionsLookup(t *testing.T) {
	v := NewVersions(map[string][]model.Target{
		"x_def": {
			{Display: "web", Identity: map[string]any{"account_id": "1", "family": "web", "revision": float64(12)}},
		},
		"x_job": {
			{Identity: map[string]any{"arn": "arn:x:job-definition/jobs:3"}},
		},
	})
	for _, c := range []struct {
		value string
		typ   string
	}{
		{"web:12", "x_def"},
		{"arn:x:region:1:task-definition/web:12", "x_def"},
		{"web:13", ""},
		{"api:12", ""},
		{"12", ""},
		{"jobs:3", "x_job"},
		{"arn:x:job-definition/jobs:3", "x_job"},
		{"jobs:30", ""},
	} {
		got, ok := v.Lookup(c.value)
		if (c.typ == "") == ok || (ok && got.Type != c.typ) {
			t.Errorf("Lookup(%q) = %v %v, want %q", c.value, got.Type, ok, c.typ)
		}
	}
}

func TestAddNamed(t *testing.T) {
	existing := []model.Target{{Type: "x_def", Name: "web", Display: "web", Identity: map[string]any{"family": "web", "revision": float64(14)}}}
	add := []model.Target{
		{Type: "x_def", Display: "web", Identity: map[string]any{"family": "web", "revision": float64(12)}},
		{Type: "x_def", Display: "web", Identity: map[string]any{"family": "web", "revision": float64(14)}}, // already there
	}
	got := AddNamed(existing, add)
	if len(got) != 2 || got[0].Name != "web" || got[1].Name != "web_12" {
		t.Errorf("got %+v", got)
	}
}
