package tfmap

// Catalogs say what each resource type is on the map: a component of its
// own, configuration folded into one, a connection between two, or wiring.

import (
	"encoding/json"
	"io/fs"
	"os"
	"strings"
)

// Rule says how a resource type behaves when the graph is collapsed to an
// architecture view.
//
//	primary   — a logical component: something that runs code, stores data or
//	            takes traffic (service, function, database, bucket, queue, load
//	            balancer). Everything else is wiring.
//	child     — configuration of one primary; folded into it (bucket ACL, API stage)
//	connector — exists to link two primaries; becomes an edge (lambda permission)
//	satellite — plumbing; never a component of its own. Folded into the one
//	            component that uses it (or the one compute component among its
//	            users); when shared it becomes see-through, so each user gets its
//	            effects (a shared role's grants show as access from every service)
//	helper    — no architectural meaning; hidden and see-through (random_pet, archive_file)
//	ignore    — background everything sits in (VPC, network): hidden, and it
//	            absorbs references instead of passing them on; its children
//	            (subnets, route tables) are ignored with it
type Rule struct {
	Role       string `json:"role"`
	Category   string `json:"category,omitempty"`
	Label      string `json:"label,omitempty"`
	ParentAttr string `json:"parent_attr,omitempty"` // child: attribute referencing the parent
	FromAttr   string `json:"from_attr,omitempty"`   // connector: attribute referencing the source
	ToAttr     string `json:"to_attr,omitempty"`     // connector: attribute referencing the target
	Verb       string `json:"verb,omitempty"`        // connector: edge label, e.g. "invokes"
	Attaches   bool   `json:"attaches,omitempty"`    // connector: the from end is used by the to end (policy attached to role)
	Inverse    bool   `json:"inverse,omitempty"`     // resource policy: what it references may access its parent, so edges are reversed
	// InverseAttrs are attributes whose references run the other way: a
	// security group's ingress names who may connect to it.
	InverseAttrs []string `json:"inverse_attrs,omitempty"`
}

// nameAttrs are the attributes that name a component (a table's name, a
// bucket's bucket, a function's function_name).
var nameAttrs = map[string]bool{
	"name": true, "bucket": true, "function_name": true, "identifier": true, "cluster_identifier": true,
}

// selfNaming reports attributes that name, tag or describe their own
// resource rather than point at another.
func selfNaming(attr string) bool {
	root, _, _ := strings.Cut(attr, ".")
	return nameAttrs[attr] || root == "tags" || root == "labels" || attr == "name_prefix" || attr == "description"
}

// isCompute: wiring shared by several components joins the one that runs
// code, if there is exactly one (a target group belongs to its service, not
// the load balancer).
func isCompute(r Rule) bool {
	return r.Role == "primary" && r.Category == "compute"
}

type catalogFile struct {
	Provider    string          `json:"provider"`
	Resources   map[string]Rule `json:"resources"`
	DataSources map[string]Rule `json:"data_sources"`
}

type Catalog struct {
	resources   map[string]Rule
	dataSources map[string]Rule
}

// LoadCatalog reads every <provider>/catalog.json in each of dirs in turn:
// the catalogs built into the binary, then a workspace's packs, whose
// entries add to or replace the built-in ones type by type.
func LoadCatalog(dirs ...fs.FS) (*Catalog, error) {
	c := &Catalog{resources: map[string]Rule{}, dataSources: map[string]Rule{}}
	for _, fsys := range dirs {
		if err := c.read(fsys); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Catalog) read(fsys fs.FS) error {
	files, err := fs.Glob(fsys, "*/catalog.json")
	if err != nil {
		return err
	}
	for _, path := range files {
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		var f catalogFile
		if err := json.Unmarshal(b, &f); err != nil {
			return &os.PathError{Op: "parse catalog", Path: path, Err: err}
		}
		for k, v := range f.Resources {
			c.resources[k] = v
		}
		for k, v := range f.DataSources {
			c.dataSources[k] = v
		}
	}
	return nil
}

// Providers whose resources never carry architectural meaning.
var helperProviders = map[string]bool{
	"random": true, "null": true, "time": true, "archive": true, "local": true,
	"tls": true, "external": true, "http": true, "terraform": true,
}

// classify returns the rule for a node and whether it came from the catalog.
// Types missing from the catalog fall back to naming heuristics.
func (c *Catalog) classify(n Node, typesInConfig map[string]bool) (Rule, bool) {
	switch n.Kind {
	case "resource":
		if r, ok := c.resources[n.Type]; ok {
			return r, true
		}
	case "data":
		if r, ok := c.dataSources[n.Type]; ok {
			return r, true
		}
		return Rule{Role: "helper", Category: "other"}, false
	case "module":
		return Rule{Role: "primary", Category: "module", Label: "module"}, true
	default:
		return Rule{Role: "meta"}, true
	}

	provider, _, _ := strings.Cut(n.Type, "_")
	if helperProviders[provider] {
		return Rule{Role: "helper", Category: "other"}, false
	}
	if strings.Contains(n.Type, "_attachment") || strings.Contains(n.Type, "_association") {
		return Rule{Role: "connector", Category: "other"}, false
	}
	// aws_s3_bucket_acl is configuration of aws_s3_bucket: find the longest
	// other type that is a name prefix and treat this one as its child.
	parent := ""
	for t := range typesInConfig {
		if t != n.Type && strings.HasPrefix(n.Type, t+"_") && len(t) > len(parent) {
			parent = t
		}
	}
	if parent != "" {
		return Rule{Role: "child", Category: "other", ParentAttr: "type:" + parent}, false
	}
	// Only catalogued kinds are components; anything unknown is plumbing.
	return Rule{Role: "satellite", Category: "other"}, false
}
