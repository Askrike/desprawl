package link

import (
	"fmt"
	"strings"

	"github.com/askrike/desprawl/internal/index"
	"github.com/askrike/desprawl/internal/rules"
)

// Confidence says how strongly the data supports a reference.
type Confidence string

const (
	// Observed: the value is the target's own, assigned by its provider,
	// and no other resource owns it.
	Observed Confidence = "observed"
	// Derived: a documented rule says the attribute names this kind of
	// resource, or the value is unique but only an input of the target.
	Derived Confidence = "derived"
	// Inferred: a rule judged from real infrastructure, or a plain
	// name matched in a free-form map.
	Inferred Confidence = "inferred"
)

// evidence is why a reference was made, and what it doesn't establish.
type evidence struct {
	confidence  Confidence
	basis       string
	limitations []string
}

// mentionable reports whether value, found inside a larger string (a policy
// document, a description), is reliably a mention of c. A structured
// identifier (ARN, path, URL) is. A bare value qualifies only when the
// provider assigned it: a name the author chose (a statement ID) turns up in
// documents for reasons other than naming that resource.
func mentionable(c index.Ref, value string) bool {
	return c.Assigned || strings.ContainsAny(value, ":/")
}

// own reports whether the value is the target's own identifier: assigned
// by the provider, or its id.
func own(c index.Ref) bool { return c.Assigned || c.Attr == "id" }

func uniqueEvidence(c index.Ref, embedded bool) evidence {
	e := evidence{confidence: Observed, basis: fmt.Sprintf("the value is %s, the target's own identifier; no other resource owns it", c)}
	if !own(c) {
		e = evidence{
			confidence:  Derived,
			basis:       fmt.Sprintf("the value equals %s; no other resource in the export has it", c),
			limitations: []string{"the target holds this value as a setting, not an ID assigned to it, so a resource outside the export could hold it too"},
		}
	}
	if embedded {
		e.basis = "found inside a longer string: " + e.basis
		e.limitations = append(e.limitations, "the rest of the string stays literal")
	}
	return e
}

func namedEvidence(c index.Ref) evidence {
	return evidence{
		confidence: Inferred,
		basis:      fmt.Sprintf("the value is %s, the target's own name, in a free-form map; no other resource has or sets it", c),
		limitations: []string{
			"matched by name inside a free-form map; the plan can't reject a wrong match",
			"a same-named resource outside the export can't be ruled out",
		},
	}
}

// ruleEvidence explains a rule match: idShaped says whether the value
// looks like a generated identifier, targets how many resources of the
// rule's target type have it.
// why says how the target was chosen when several resources have the value.
func ruleEvidence(r rules.Rule, c index.Ref, idShaped bool, targets int, why string) evidence {
	if idShaped && targets == 1 && own(c) {
		return evidence{
			confidence: Observed,
			basis:      fmt.Sprintf("the value is %s, the target's own identifier, and %s.%s refers to %s per a rule", c, r.Source, r.Path, r.Target),
		}
	}
	var e evidence
	switch r.Origin {
	case rules.OriginLLM:
		e.confidence = Inferred
		e.basis = fmt.Sprintf("a rule judged from real infrastructure says %s.%s refers to %s.%s", r.Source, r.Path, r.Target, r.Attr)
		if r.Note != "" {
			e.basis += ": " + r.Note
		}
	case rules.OriginDocs:
		e.confidence = Derived
		e.basis = fmt.Sprintf("the provider docs set %s.%s to %s.%s", r.Source, r.Path, r.Target, r.Attr)
		if r.Support > 1 {
			e.basis += fmt.Sprintf(" (%d examples)", r.Support)
		}
	default:
		e.confidence = Derived
		e.basis = fmt.Sprintf("a rule says %s.%s refers to %s.%s", r.Source, r.Path, r.Target, r.Attr)
	}
	switch {
	case targets > 1:
		e.limitations = append(e.limitations, fmt.Sprintf("%d resources of type %s have this value; chose the one %s", targets, r.Target, why))
	case !idShaped:
		e.limitations = append(e.limitations, "matched by name, not by a unique ID: a same-named resource outside the export can't be ruled out")
	}
	return e
}
