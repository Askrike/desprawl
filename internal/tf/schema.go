package tf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/askrike/desprawl/internal/prof"
	"os"
	"strings"
)

// Schemas holds the parts of `terraform providers schema -json` we use,
// merged across all providers in the configuration.
type Schemas struct {
	Resources  map[string]*SchemaBlock    `json:"resources"`
	Lists      map[string]*SchemaBlock    `json:"lists"`
	Identities map[string]*IdentitySchema `json:"identities"`
	// DataSources are used to find inventory sources and scope parameters.
	DataSources map[string]*SchemaBlock `json:"data_sources"`
}

type SchemaBlock struct {
	Attributes  map[string]*SchemaAttr `json:"attributes,omitempty"`
	BlockTypes  map[string]*BlockType  `json:"block_types,omitempty"`
	Description string                 `json:"description,omitempty"`
}

type BlockType struct {
	Block *SchemaBlock `json:"block"`
	// NestingMode is "single", "group", "list", "set" or "map".
	NestingMode string `json:"nesting_mode,omitempty"`
	MinItems    int    `json:"min_items,omitempty"`
}

type SchemaAttr struct {
	Type        json.RawMessage `json:"type,omitempty"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Optional    bool            `json:"optional,omitempty"`
	Computed    bool            `json:"computed,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
}

type IdentitySchema struct {
	Attributes map[string]struct {
		Type              json.RawMessage `json:"type"`
		RequiredForImport bool            `json:"required_for_import,omitempty"`
		OptionalForImport bool            `json:"optional_for_import,omitempty"`
	} `json:"attributes"`
}

// ProvidersSchema runs `terraform providers schema -json`.
func (r Runner) ProvidersSchema(ctx context.Context) (*Schemas, error) {
	out, err := r.providersSchemaJSON(ctx)
	if err != nil {
		return nil, err
	}
	return ParseProvidersSchema(out)
}

func (r Runner) providersSchemaJSON(ctx context.Context) ([]byte, error) {
	c := r.cmd(ctx, "providers", "schema", "-json")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	end := r.span("providers schema")
	out, err := c.Output()
	end(prof.Err(err)...)
	if err != nil {
		return nil, fmt.Errorf("terraform providers schema: %w\n%s", err, stderr.String())
	}
	return out, nil
}

func ParseProvidersSchema(b []byte) (*Schemas, error) {
	s, _, err := parseProvidersSchema(b)
	return s, err
}

// parseProvidersSchema also returns the sources of the providers b holds.
func parseProvidersSchema(b []byte) (*Schemas, []string, error) {
	type wrapped struct {
		Block *SchemaBlock `json:"block"`
	}
	var raw struct {
		ProviderSchemas map[string]struct {
			ResourceSchemas         map[string]wrapped         `json:"resource_schemas"`
			ListResourceSchemas     map[string]wrapped         `json:"list_resource_schemas"`
			ResourceIdentitySchemas map[string]*IdentitySchema `json:"resource_identity_schemas"`
			DataSourceSchemas       map[string]wrapped         `json:"data_source_schemas"`
		} `json:"provider_schemas"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, err
	}
	s := &Schemas{
		Resources:   map[string]*SchemaBlock{},
		Lists:       map[string]*SchemaBlock{},
		Identities:  map[string]*IdentitySchema{},
		DataSources: map[string]*SchemaBlock{},
	}
	var sources []string
	for src, p := range raw.ProviderSchemas {
		sources = append(sources, src)
		for name, rs := range p.ResourceSchemas {
			s.Resources[name] = rs.Block
		}
		for name, ls := range p.ListResourceSchemas {
			s.Lists[name] = ls.Block
		}
		for name, is := range p.ResourceIdentitySchemas {
			s.Identities[name] = is
		}
		for name, ds := range p.DataSourceSchemas {
			s.DataSources[name] = ds.Block
		}
	}
	return s, sources, nil
}

// RequiredListArgs returns the required arguments of a list resource's
// config block.
func (s *Schemas) RequiredListArgs(typ string) []string {
	b := s.Lists[typ]
	if b == nil {
		return nil
	}
	var out []string
	for name, a := range b.Attributes {
		if a.Required {
			out = append(out, name)
		}
	}
	for name, bt := range b.BlockTypes {
		if bt.MinItems > 0 {
			out = append(out, name)
		}
	}
	return out
}

// ComputedStrings returns the computed-only string attributes of a resource
// type: values the provider assigns (ids, ARNs, self links), which are what
// other resources refer to.
func (s *Schemas) ComputedStrings(typ string) []string {
	b := s.Resources[typ]
	if b == nil {
		return nil
	}
	var out []string
	for name, a := range b.Attributes {
		if a.Computed && !a.Optional && !a.Required && string(a.Type) == `"string"` {
			out = append(out, name)
		}
	}
	return out
}

// Describe returns the schema description for a dotted attribute path. Path
// segments past the deepest schema attribute (keys inside JSON documents or
// maps) are reported in rest.
func (s *Schemas) Describe(typ, path string) (desc string, rest string) {
	b := s.Resources[typ]
	if b == nil {
		return "", path
	}
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		if a, ok := b.Attributes[seg]; ok {
			return a.Description, strings.Join(segs[i+1:], ".")
		}
		bt, ok := b.BlockTypes[seg]
		if !ok || bt.Block == nil {
			return "", strings.Join(segs[i:], ".")
		}
		b = bt.Block
	}
	return b.Description, ""
}

// ListArgDescription returns the description of a list resource's config argument.
func (s *Schemas) ListArgDescription(typ, arg string) string {
	if b := s.Lists[typ]; b != nil {
		if a := b.Attributes[arg]; a != nil {
			return a.Description
		}
	}
	return ""
}

// LoadSchemas reads schemas saved with SaveSchemas.
func LoadSchemas(path string) (*Schemas, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Schemas
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

func SaveSchemas(path string, s *Schemas) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// MapKey returns the map key a dotted attribute path of a resource type
// goes through, if it goes into the keys of a map attribute
// ("environment.variables.API_URL" -> "API_URL"). Map keys are chosen by
// whoever wrote the config, unless the provider defines them. Keys inside
// JSON documents held in string attributes don't count.
func (s *Schemas) MapKey(typ, path string) (string, bool) {
	b := s.Resources[typ]
	segs := strings.Split(path, ".")
	for i := 0; b != nil && i < len(segs); i++ {
		if a := b.Attributes[segs[i]]; a != nil {
			return typeMapKey(a.Type, segs[i+1:])
		}
		bt := b.BlockTypes[segs[i]]
		if bt == nil {
			return "", false
		}
		b = bt.Block
	}
	return "", false
}

// MapTypes counts the resource types with a map attribute at a dotted
// path ("tags", "environment.variables"). A map many types have is a
// convention (tags, labels), not one type's own setting.
func (s *Schemas) MapTypes(path string) int {
	n := 0
	for typ := range s.Resources {
		if _, ok := s.MapKey(typ, path+".k"); ok {
			n++
		}
	}
	return n
}

func typeMapKey(t json.RawMessage, rest []string) (string, bool) {
	if len(rest) == 0 {
		return "", false
	}
	var parts []json.RawMessage
	if json.Unmarshal(t, &parts) != nil || len(parts) != 2 {
		return "", false // a primitive: a string may hold a JSON document
	}
	var kind string
	json.Unmarshal(parts[0], &kind)
	switch kind {
	case "map":
		return rest[0], true
	case "list", "set":
		return typeMapKey(parts[1], rest)
	case "object":
		var fields map[string]json.RawMessage
		if json.Unmarshal(parts[1], &fields) != nil {
			return "", false
		}
		if ft, ok := fields[rest[0]]; ok {
			return typeMapKey(ft, rest[1:])
		}
	}
	return "", false
}
