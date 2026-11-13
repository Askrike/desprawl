package generate

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/askrike/desprawl/internal/tf"
)

func TestSensitiveIdentifiers(t *testing.T) {
	plan := `{"resource_changes":[
	 {"address":"x_net.main","mode":"managed","type":"x_net","change":{"actions":["no-op"],
	  "before":{"id":"net-0a1b2c3d4e","name":"main"},"before_sensitive":{}}},
	 {"address":"x_param.net","mode":"managed","type":"x_param","change":{"actions":["no-op"],
	  "before":{"id":"/p/net","value":"net-0a1b2c3d4e"},"before_sensitive":{"value":true}}},
	 {"address":"x_param.secret","mode":"managed","type":"x_param","change":{"actions":["no-op"],
	  "before":{"id":"/p/secret","value":"hunter2-hunter2"},"before_sensitive":{"value":true}}}]}`
	refs, err := sensitiveIdentifiers([]byte(plan), nil, &tf.Schemas{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Address != "x_param.net" || refs[0].Attr != "value" || refs[0].Value != "net-0a1b2c3d4e" || refs[0].Owner.String() != "x_net.main.id" {
		t.Fatalf("got %+v", refs)
	}
	for _, r := range refs {
		if strings.Contains(r.Value, "hunter2") {
			t.Error("a value matching nothing was returned")
		}
	}
}

func TestRepairPlaceholderForRequiredString(t *testing.T) {
	f, diags := hclwrite.ParseConfig([]byte("resource \"x_key\" \"k\" {\n  name = \"k\"\n}\n"), "", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	body := f.Body().Blocks()[0].Body()
	schema := &tf.SchemaBlock{Attributes: map[string]*tf.SchemaAttr{
		"name":       {Type: []byte(`"string"`), Optional: true},
		"public_key": {Type: []byte(`"string"`), Required: true},
	}}
	what, ok := repair(body, schema, nil, nil, tf.Diagnostic{Summary: "Missing required argument", Detail: `The argument "public_key" is required, but no definition was found.`})
	if !ok {
		t.Fatal("not repaired")
	}
	out := string(f.Bytes())
	if !strings.Contains(what, "public_key") || !strings.Contains(out, "public_key") || !strings.Contains(out, "ignore_changes") {
		t.Errorf("%s\n%s", what, out)
	}
}
