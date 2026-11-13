package privacy

import (
	"reflect"
	"testing"

	"github.com/askrike/desprawl/internal/tf"
)

func TestGuard(t *testing.T) {
	schemas := &tf.Schemas{Resources: map[string]*tf.SchemaBlock{
		"x_function": {Attributes: map[string]*tf.SchemaAttr{"environment": {Description: "Environment variables of the function."}}},
	}}
	account := []string{
		`{"name": "quill-runner", "environment": {"QUILL_AUTH_SECRET_NAME": "zephyr-dev/secret", "LOG_LEVEL": "info"}, "account": "210987654321"}`,
	}
	g := NewGuard(account, schemas, "log_level controls verbosity", "ClusterName", "the name of a secret, with auth")
	for text, want := range map[string][]string{
		"This env var holds the name of the secret the function reads.": nil,
		"environment.variables.QUILL_AUTH_SECRET_NAME":                  {"quill"},
		"The zephyr-dev runner in 210987654321":                         {"210987654321", "runner", "zephyr"},
		"LOG_LEVEL sets verbosity":                                      nil,
	} {
		if got := g.Leaks(text); !reflect.DeepEqual(got, want) {
			t.Errorf("Leaks(%q) = %v, want %v", text, got, want)
		}
	}
	if !g.Public("ClusterName") || g.Public("QUILL_AUTH_SECRET_NAME") {
		t.Error("Public: documented keys are public, account keys aren't")
	}
}
