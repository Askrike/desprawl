package discover

import (
	"fmt"
	"testing"

	"github.com/askrike/desprawl/internal/model"
)

func TestParamsFoldCase(t *testing.T) {
	// 20 resources report the account in lower case, 2 in upper case: one
	// scope, spelled the common way.
	var targets []model.Target
	for i := range 22 {
		acct := "abcd-1234"
		if i < 2 {
			acct = "ABCD-1234"
		}
		targets = append(targets, model.Target{Type: "x_thing", Name: fmt.Sprint(i), Identity: map[string]any{"account": acct, "name": fmt.Sprint("thing", i)}})
	}
	got := Params(targets)
	if fmt.Sprint(got) != "map[account:abcd-1234]" {
		t.Errorf("params %v", got)
	}
}
