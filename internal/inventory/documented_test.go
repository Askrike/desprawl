package inventory

import (
	"slices"
	"testing"
)

// Types documented with the same import ID as a type share its objects'
// IDs; a type whose example has only the shape doesn't.
func TestSameID(t *testing.T) {
	docs := map[string][]string{
		"net":        {"/subs/0000/rg/group1/providers/Net/nets/net1/subnets/sub1"},
		"net_assoc":  {"/subs/0000/rg/group1/providers/Net/nets/net1/subnets/sub1"},
		"net_assoc2": {"/subs/0000/rg/group1/providers/Net/nets/net1/subnets/sub1"},
		"peering":    {"/subs/0000/rg/group1/providers/Net/nets/net1/peerings/peer1"},
		"other":      {"/subs/0000/rg/group1/providers/Net/things/t1"},
	}
	id := "/subs/abcd/rg/main/providers/Net/nets/vnet/subnets/app"
	if got, want := SameID(id, "net", docs, 10), []string{"net_assoc", "net_assoc2"}; !slices.Equal(got, want) {
		t.Errorf("SameID = %v, want %v", got, want)
	}
	if got := SameID(id, "net", docs, 1); len(got) != 1 {
		t.Errorf("SameID with max 1 = %v", got)
	}
	if got := SameID(id, "undocumented", docs, 10); got != nil {
		t.Errorf("SameID of an undocumented type = %v, want none", got)
	}
}
