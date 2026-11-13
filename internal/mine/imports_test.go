package mine

import "testing"

// Docs are external input: an identity that isn't an object is skipped, not a
// panic.
func TestImportBlocksOddIdentity(t *testing.T) {
	src := []byte(`
import {
  to       = x_thing.a
  identity = ["a"]
}
import {
  to       = x_thing.b
  identity = { name = "b" }
}
`)
	got := map[string]*ImportExamples{}
	importBlocks(src, func(typ string) *ImportExamples {
		if got[typ] == nil {
			got[typ] = &ImportExamples{}
		}
		return got[typ]
	})
	if ex := got["x_thing"]; ex == nil || len(ex.Identities) != 1 || ex.Identities[0]["name"] != "b" {
		t.Errorf("identities = %+v", ex)
	}
}
