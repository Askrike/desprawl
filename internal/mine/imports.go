package mine

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ImportExamples are documented ways to import one resource type: import
// IDs and identities as shown in the provider docs. They tell an agent (or a
// person) what to look up in the cloud to import a type that discovery
// can't list.
type ImportExamples struct {
	IDs        []string         `json:"ids,omitempty"`
	Identities []map[string]any `json:"identities,omitempty"`
}

// `terraform import aws_iam_role.example my-role` in console examples.
var importCmd = regexp.MustCompile(`terraform import\s+(?:'|")?([a-z0-9_]+)\.[^\s'"]+(?:'|")?\s+(.+)$`)

// Imports mines import examples from every markdown file under dir.
func Imports(dir string) (map[string]*ImportExamples, error) {
	out := map[string]*ImportExamples{}
	get := func(typ string) *ImportExamples {
		if out[typ] == nil {
			out[typ] = &ImportExamples{}
		}
		return out[typ]
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMarkdown(d.Name()) {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, block := range codeBlocks(b) {
			importBlocks(block, get)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "%"))
			if m := importCmd.FindStringSubmatch(line); m != nil {
				id := strings.Trim(strings.TrimSpace(m[2]), `'"`)
				addID(get(m[1]), id)
			}
		}
		return nil
	})
	for _, ex := range out {
		sort.Strings(ex.IDs)
	}
	return out, err
}

func addID(ex *ImportExamples, id string) {
	for _, x := range ex.IDs {
		if x == id {
			return
		}
	}
	ex.IDs = append(ex.IDs, id)
}

func importBlocks(src []byte, get func(string) *ImportExamples) {
	f, diags := hclsyntax.ParseConfig(src, "example.tf", hcl.InitialPos)
	if diags.HasErrors() || f == nil {
		return
	}
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		if b.Type != "import" {
			continue
		}
		to, ok := b.Body.Attributes["to"]
		if !ok {
			continue
		}
		t, d := hcl.AbsTraversalForExpr(to.Expr)
		if d.HasErrors() || len(t) < 2 {
			continue
		}
		ex := get(t.RootName())
		if a, ok := b.Body.Attributes["id"]; ok {
			if v, d := a.Expr.Value(nil); !d.HasErrors() && v.Type().FriendlyName() == "string" && v.IsKnown() && !v.IsNull() {
				addID(ex, v.AsString())
			}
		}
		if a, ok := b.Body.Attributes["identity"]; ok {
			if v, d := a.Expr.Value(nil); !d.HasErrors() && v.IsKnown() && !v.IsNull() && (v.Type().IsObjectType() || v.Type().IsMapType()) {
				m := map[string]any{}
				for k, x := range v.AsValueMap() {
					if x.Type().FriendlyName() == "string" && x.IsKnown() && !x.IsNull() {
						m[k] = x.AsString()
					}
				}
				if len(m) > 0 && len(ex.Identities) < 3 {
					ex.Identities = append(ex.Identities, m)
				}
			}
		}
	}
}
