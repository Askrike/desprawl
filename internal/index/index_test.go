package index

import (
	"testing"

	"github.com/askrike/desprawl/internal/rules"
)

// testAliases are the alias patterns of a provider whose revisioned objects
// are named "family:revision" after the last "/", and whose secrets get a
// random six-character suffix.
var testAliases = []rules.Alias{
	{Match: `^.*/([^/:\s]+:[0-9]+)$`},
	{Match: `^(arn:.+[:/].+)-[A-Za-z0-9]{6}$`},
}

func aliasSet(t *testing.T) *rules.Set {
	t.Helper()
	rs, err := rules.NewSet(nil).WithAliases(testAliases)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestAliases(t *testing.T) {
	ix := &Index{byValue: map[string][]Ref{}, byPrefix: map[string][]string{}, aliases: map[string]string{}}
	add := func(v, addr string) {
		ix.byValue[v] = append(ix.byValue[v], Ref{Address: addr, Type: "x", Attr: "arn", Assigned: true})
	}
	add("arn:p:ecs:r:1:task/webapp:12", "x.web")
	add("arn:p:sm:r:1:secret:app/env-AbC123", "x.secret")
	add("arn:p:sm:r:1:secret:other-XyZ987", "x.other")
	add("arn:p:ecs:r:1:task/git:9", "x.git")
	add("arn:p:sm:r:1:secret:other", "x.decoy") // same spelling as an alias: ambiguous
	ix.buildAliases(aliasSet(t).Aliases())

	if got := ix.Lookup("webapp:12"); len(got) != 1 || got[0].Address != "x.web" {
		t.Errorf("revision alias: %v", got)
	}
	if !ix.Unique("webapp:12") || !ix.IsAlias("webapp:12") {
		t.Error("revision alias should be unique and an alias")
	}
	if got := ix.Lookup("arn:p:sm:r:1:secret:app/env"); len(got) != 1 || got[0].Address != "x.secret" {
		t.Errorf("suffix alias: %v", got)
	}
	if got := ix.Lookup("arn:p:sm:r:1:secret:other"); len(got) != 1 || got[0].Address != "x.decoy" {
		t.Errorf("exact value must win over an alias: %v", got)
	}
	if got := ix.Lookup("git:9"); len(got) != 1 || got[0].Address != "x.git" {
		t.Errorf("short revision alias: %v", got)
	}
	plain := &Index{byValue: map[string][]Ref{"arn:p:ecs:r:1:task/webapp:12": {{Address: "x.web", Attr: "arn", Assigned: true}}}, byPrefix: map[string][]string{}, aliases: map[string]string{}}
	plain.buildAliases(nil)
	if plain.IsAlias("webapp:12") {
		t.Error("no pack patterns: no aliases")
	}
	if sp := ix.Embedded("x git:9 y"); len(sp) != 0 {
		t.Errorf("short alias must not match inside strings: %v", sp)
	}
	if got := ix.Lookup("webapp:13"); got != nil {
		t.Errorf("unknown revision matched: %v", got)
	}
	spans := ix.Embedded("{{resolve:arn:p:sm:r:1:secret:app/env:KEY::}}")
	if len(spans) != 1 || spans[0].Value != "arn:p:sm:r:1:secret:app/env" {
		t.Errorf("embedded alias: %v", spans)
	}
}

func TestSpell(t *testing.T) {
	ix := &Index{scalars: map[string]map[string]string{
		"x.web":  {"arn": "arn:p:ecs:r:1:task/web:12", "id": "web", "family": "web", "revision": "12", "cpu": "1024"},
		"x.env":  {"arn": "arn:p:sm:r:1:secret:app/env-AbC123", "id": "arn:p:sm:r:1:secret:app/env-AbC123", "name": "app/env", "region": "r"},
		"x.bare": {"arn": "arn:p:ecs:r:1:task/job:3"},
	}}
	show := func(ps []Piece) string {
		var s string
		for _, p := range ps {
			if p.Attr != "" {
				s += "{" + p.Attr + "}"
			} else {
				s += p.Lit
			}
		}
		return s
	}
	for _, c := range []struct{ alias, addr, want string }{
		{"web:12", "x.web", "{family}:{revision}"},
		{"arn:p:sm:r:1:secret:app/env", "x.env", "arn:p:sm:r:1:secret:{name}"},
		{"job:3", "x.bare", ""},                       // nothing spells it: stays literal
		{"arn:p:sm:r:1:secret:xapp/env", "x.env", ""}, // "app/env" not on a word boundary
	} {
		got := show(ix.Spell(c.alias, Ref{Address: c.addr, Attr: "arn"}))
		if got != c.want {
			t.Errorf("Spell(%q) = %q, want %q", c.alias, got, c.want)
		}
	}
}

// A value a resource is assigned belongs to it, not to resources that take
// it as an input, even when the owner is listed after them.
func TestOwner(t *testing.T) {
	const arn = "arn:x:secret:s-123"
	ix := &Index{byValue: map[string][]Ref{arn: {
		{Address: "x_secret_policy.p", Attr: "secret_arn"},
		{Address: "x_secret.s", Attr: "arn", Assigned: true},
	}}}
	if got, ok := ix.Owner(arn); !ok || got.Address != "x_secret.s" {
		t.Errorf("owner = %v, %v; want x_secret.s", got, ok)
	}
	ix.byValue["arn:x:other:o-1"] = []Ref{{Address: "a.b", Attr: "x"}, {Address: "c.d", Attr: "y"}}
	if _, ok := ix.Owner("arn:x:other:o-1"); ok {
		t.Error("two holders, neither assigned: no owner")
	}
}

func TestRefString(t *testing.T) {
	for _, c := range []struct {
		r    Ref
		want string
	}{
		{Ref{Address: "x.a", Attr: "identity.0.principal_id"}, "x.a.identity[0].principal_id"},
		{Ref{Address: "x.a", Attr: "tags.my-key"}, "x.a.tags.my-key"},
		{Ref{Address: "x.a", Attr: "labels.app/name"}, `x.a.labels["app/name"]`},
		{Ref{Address: "x.a", Attr: "labels.app.kubernetes.io/name", Key: "app.kubernetes.io/name"}, `x.a.labels["app.kubernetes.io/name"]`},
	} {
		if got := c.r.String(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
}
