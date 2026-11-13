// Package privacy keeps account data out of provider packs. Packs are
// shared across accounts, but some of their content is inferred from a
// real one: whoever adds a rule or an inventory mapping from a workspace's
// suggestions sees resource names and IDs, and may repeat them. A Guard knows every token of
// the account's values and the provider's own vocabulary (schema and
// docs); a token in pack content that comes from the account and not from
// the provider is a leak.
package privacy

import (
	"regexp"
	"sort"
	"strings"

	"github.com/askrike/desprawl/internal/tf"
)

// Guard detects account data in text.
type Guard struct {
	private map[string]bool // account tokens not in the provider vocabulary
	vocab   map[string]bool
}

// NewGuard builds a guard from texts holding the account's values
// (imported values, generated config, targets, inventory reports) and the
// provider's vocabulary: its schemas, plus public text such as mined docs
// rules and import examples.
func NewGuard(account []string, schemas *tf.Schemas, public ...string) *Guard {
	vocab := map[string]bool{}
	addText := func(s string) {
		for _, t := range tokens(s) {
			vocab[t] = true
		}
	}
	if schemas != nil {
		for _, m := range []map[string]*tf.SchemaBlock{schemas.Resources, schemas.DataSources, schemas.Lists} {
			for name, b := range m {
				addText(name)
				addBlock(b, addText)
			}
		}
	}
	for _, s := range public {
		addText(s)
	}
	for _, s := range public {
		for _, w := range identifier.FindAllString(s, -1) {
			vocab[strings.ToLower(w)] = true
		}
	}
	g := &Guard{private: map[string]bool{}, vocab: vocab}
	for _, text := range account {
		for _, t := range tokens(text) {
			if !vocab[t] {
				g.private[t] = true
			}
		}
	}
	return g
}

func addBlock(b *tf.SchemaBlock, add func(string)) {
	if b == nil {
		return
	}
	add(b.Description)
	for name, a := range b.Attributes {
		add(name)
		add(a.Description)
	}
	for name, bt := range b.BlockTypes {
		add(name)
		addBlock(bt.Block, add)
	}
}

var (
	word       = regexp.MustCompile(`[A-Za-z0-9]+`)
	identifier = regexp.MustCompile(`[A-Za-z0-9_]+`)
)

// Public reports whether an identifier (a map key such as "ClusterName")
// is part of the provider's vocabulary.
func (g *Guard) Public(id string) bool { return g.vocab[strings.ToLower(id)] }

// tokens splits text into lower-case alphanumeric words of at least 4
// characters: shorter ones ("dev", "api") are too common to tell apart.
func tokens(s string) []string {
	var out []string
	for _, w := range word.FindAllString(s, -1) {
		if len(w) >= 4 {
			out = append(out, strings.ToLower(w))
		}
	}
	return out
}

// Redact replaces the account tokens in s with "…".
func (g *Guard) Redact(s string) string {
	return word.ReplaceAllStringFunc(s, func(w string) string {
		if len(w) >= 4 && g.private[strings.ToLower(w)] {
			return "…"
		}
		return w
	})
}

// Leaks returns the account tokens in s.
func (g *Guard) Leaks(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokens(s) {
		if g.private[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
