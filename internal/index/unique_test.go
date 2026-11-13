package index

import "testing"

func TestUniqueOwnID(t *testing.T) {
	ix := &Index{byValue: map[string][]Ref{}, byPrefix: map[string][]string{}, aliases: map[string]string{}}
	ix.byValue["rtb-0a2959964b2dbb7d3"] = []Ref{
		{Address: "x_route.a", Attr: "route_table_id"},
		{Address: "x_assoc.b", Attr: "route_table_id"},
		{Address: "x_table.t", Attr: "id"},
	}
	if !ix.Unique("rtb-0a2959964b2dbb7d3") {
		t.Error("the resource whose id it is owns the value; children holding it as a parent id don't")
	}
	ix.byValue["sg-0123456789abcdef0"] = []Ref{
		{Address: "x_group.a", Attr: "id"},
		{Address: "x_group.b", Attr: "id"},
	}
	if ix.Unique("sg-0123456789abcdef0") {
		t.Error("two resources with the same id are ambiguous")
	}
	ix.byValue["name-with-1"] = []Ref{
		{Address: "x_a.a", Attr: "name"},
		{Address: "x_b.b", Attr: "name"},
	}
	if ix.Unique("name-with-1") {
		t.Error("no id or assigned owner: two holders stay ambiguous")
	}
	// Run4/5: subnets linked their zone to the mount target in it.
	ix.scalars = map[string]map[string]string{
		"x_snapshot.s":     {"id": "snap-1"},
		"x_mount_target.m": {"id": "fsmt-1"},
		"x_lb.l":           {"id": "arn:p:lb/app-1"},
		"x_network.n":      {"id": "net-0123456789"},
		"x_function.f":     {"id": "fn-name"},
	}
	ix.setBy = map[string][]string{
		"euc1-az3":                      {"x_subnet.s"},
		"eu-central-1a":                 {"x_subnet.s"},
		"eu-central-1b":                 {"x_subnet.s"},
		"net-0123456789":                {"x_subnet.s"},
		"arn:p:fn:r:1:function:fn-name": {"x_target.t"}, // an event target's target
	}
	ix.byValue["euc1-az3"] = []Ref{{Address: "x_mount_target.m", Attr: "availability_zone_id", Assigned: true}}
	if ix.Unique("euc1-az3") {
		t.Error("a computed copy of a property another resource sets owns it")
	}
	// Run6: the snapshots' zone is a recognisable copy; the mount target's
	// differently named one must not become the owner.
	ix.byValue["eu-central-1a"] = []Ref{
		{Address: "x_snapshot.s", Attr: "availability_zone", Assigned: true},
		{Address: "x_mount_target.m", Attr: "availability_zone_name", Assigned: true},
	}
	if ix.Unique("eu-central-1a") {
		t.Error("a shared property is owned by whichever holder names it differently")
	}
	// Run7: with the snapshots skipped, only the differently named copy
	// is left; something still sets the zone, so it's a property.
	ix.byValue["eu-central-1b"] = []Ref{{Address: "x_mount_target.m", Attr: "availability_zone_name", Assigned: true}}
	if ix.Unique("eu-central-1b") {
		t.Error("a zone another resource sets is owned by the one computing it under another name")
	}
	// Run6: security groups linked their network to a snapshot or a load
	// balancer computing a copy of it; the network's own id owns it.
	ix.byValue["net-0123456789"] = []Ref{
		{Address: "x_snapshot.s", Attr: "vpc_id", Assigned: true},
		{Address: "x_lb.l", Attr: "vpc_id", Assigned: true},
		{Address: "x_network.n", Attr: "id"},
	}
	if r, ok := ix.Owner("net-0123456789"); !ok || r.Address != "x_network.n" {
		t.Errorf("owner of a copied network id = %v, %v; want x_network.n", r, ok)
	}
	// A function's ARN that another resource sets as its target is still
	// the function's own identifier.
	ix.byValue["arn:p:fn:r:1:function:fn-name"] = []Ref{{Address: "x_function.f", Attr: "arn", Assigned: true}}
	if r, ok := ix.Owner("arn:p:fn:r:1:function:fn-name"); !ok || r.Address != "x_function.f" {
		t.Errorf("owner of a function ARN = %v, %v; want x_function.f", r, ok)
	}
	// A mapping computing a copy of its function's ARN isn't a second
	// owner: the ARN holds the function's name, not the mapping's id.
	ix.scalars["x_mapping.m"] = map[string]string{"id": "0f3c6a1e-1111-2222-3333-444455556666"}
	ix.byValue["arn:p:fn:r:1:function:fn-name"] = []Ref{
		{Address: "x_function.f", Attr: "arn", Assigned: true},
		{Address: "x_mapping.m", Attr: "function_arn", Assigned: true},
	}
	if r, ok := ix.Owner("arn:p:fn:r:1:function:fn-name"); !ok || r.Address != "x_function.f" {
		t.Errorf("owner of a function ARN a mapping copies = %v, %v; want x_function.f", r, ok)
	}
}
