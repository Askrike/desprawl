package inventory

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

// Only parameters a source refers to decide whether a read is stale.
func TestUses(t *testing.T) {
	inv := &Inventory{Sources: []Source{
		{Config: map[string]string{"scope": `"projects/{project}"`}},
		{Partition: &Partition{Only: `"type:{kind} in {region}"`}},
	}}
	got := inv.Uses(map[string]string{"project": "p", "region": "r", "account": "a"})
	if want := map[string]string{"project": "p", "region": "r"}; !maps.Equal(got, want) {
		t.Errorf("uses %v, want %v", got, want)
	}
}

func TestTargets(t *testing.T) {
	inv := &Inventory{Kinds: map[string]Mapping{
		"zones":    {Type: "x_dns_zone", Match: `^/rg/(?P<rg>[^/]+)/(?i:dnszones)/(?P<name>[^/]+)$`, ID: "/rg/{rg}/dnsZones/{name}"},
		"vaults":   {Type: "x_vault"},
		"children": {Note: "part of another resource"},
	}}
	got, unmapped, misfit := inv.Targets([]Entry{
		{Kind: "zones", ID: "/rg/g/dnszones/example.com"},
		{Kind: "zones", ID: "/elsewhere/example.com"},
		{Kind: "vaults", ID: "/rg/g/vaults/kv", Name: "kv"},
		{Kind: "vaults", ID: "/rg/g/vaults/kv"},
		{Kind: "children", ID: "/rg/g/children/c"},
		{Kind: "unknown", ID: "/rg/g/unknown/u"},
	})
	if len(got) != 2 || got[0].ID != "/rg/g/dnsZones/example.com" || got[0].Display != "example.com" || got[1].ID != "/rg/g/vaults/kv" || got[1].Display != "kv" {
		t.Errorf("targets = %+v", got)
	}
	if a := got[0].Attrs; a["rg"] != "g" || a["name"] != "example.com" || got[1].Attrs != nil {
		t.Errorf("attrs = %v, %v", a, got[1].Attrs)
	}
	if unmapped["unknown"] != 1 || misfit["zones"] != 1 || len(unmapped)+len(misfit) != 2 {
		t.Errorf("unmapped = %v, misfit = %v", unmapped, misfit)
	}
	if _, ok := (Mapping{Match: `(?P<a>x)`, ID: "{b}"}).ImportID("x"); ok {
		t.Error("unknown placeholder accepted")
	}
}

func TestEntryID(t *testing.T) {
	s := Source{ID: "{meta.group}/{meta.name}", Name: "meta.name"}
	obj := map[string]any{"meta": map[string]any{"group": "g", "name": "n"}}
	if got := s.EntryID(obj); got != "g/n" {
		t.Errorf("EntryID = %q", got)
	}
	if got := s.EntryID(map[string]any{"meta": map[string]any{"name": "n"}}); got != "n" {
		t.Errorf("EntryID without group = %q", got)
	}
	if got := Field(obj, s.Name); got != "n" {
		t.Errorf("Field = %q", got)
	}
	if got := (Source{ID: "id"}).EntryID(map[string]any{"id": "x"}); got != "x" {
		t.Errorf("plain EntryID = %q", got)
	}
}

// A numeric ID past float64's exact range keeps every digit: entries are
// decoded with json.Number, which Field writes as read.
func TestFieldLargeNumber(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"id": 9007199254740993}`))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	if got := Field(obj, "id"); got != "9007199254740993" {
		t.Errorf("Field = %q, want 9007199254740993", got)
	}
}
