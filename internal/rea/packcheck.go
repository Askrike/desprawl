package rea

// Checks against the provider for inventory entries added to a pack (by
// hand or by an agent): a partition must do what it says, and a kind's
// mapping must import. Entries that equal the shipped pack's were checked
// when they were added, so only the others are checked, on each discover.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/askrike/desprawl/internal/inventory"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/providers"
)

// shippedInventory is the inventory.json desprawl ships for the provider.
func (w Workspace) shippedInventory() *inventory.Inventory {
	inv := &inventory.Inventory{}
	if b, err := providers.ReadFile("", w.Provider().Name, "inventory.json"); err == nil {
		json.Unmarshal(b, inv)
	}
	return inv
}

// isShipped says whether s is one of shipped's sources, whichever
// provider block it is read with.
func isShipped(shipped *inventory.Inventory, s inventory.Source) bool {
	s.Provider = ""
	return slices.ContainsFunc(shipped.Sources, func(o inventory.Source) bool { return reflect.DeepEqual(o, s) })
}

// checkPartitions reads each source whose partition isn't the shipped one
// without it, and checks the partition against what it reported. A
// partition that fails is taken off its source (which is still read, cut
// off) and returned with why, by source.
func (w Workspace) checkPartitions(ctx context.Context, inv *inventory.Inventory, targets []model.Target) map[string]string {
	shipped := w.shippedInventory()
	bad := map[string]string{}
	for i, s := range inv.Sources {
		if s.Partition == nil || isShipped(shipped, s) {
			continue
		}
		plain := s
		plain.Partition = nil
		res, err := inventory.ReadSource(ctx, w.Workspace, w.runner(), fmt.Sprintf("inventory/pcheck%d", i), plain, w.params, targets)
		if err == nil {
			err = inventory.CheckPartition(ctx, w.Workspace, w.runner(), fmt.Sprintf("inventory/p%d", i), s, w.params, targets, res.Kinds)
		}
		if err != nil {
			bad[s.Label()] = oneLine(err.Error())
			inv.Sources[i] = plain
			Log.Debug("inventory source %s: partition left out: %v", s.Label(), oneLine(err.Error()))
		}
	}
	return bad
}

// checkMappings imports a sample of each ID shape of the kinds whose
// mapping isn't the shipped one. It returns found less the targets of
// mappings that don't import, and why each failed, by kind.
func (w Workspace) checkMappings(ctx context.Context, inv *inventory.Inventory, entries []inventory.Entry, found []model.Target) ([]model.Target, map[string]string, error) {
	shipped := w.shippedInventory()
	kinds := inventory.Kinds(entries)
	var names []string
	var samples []model.Target
	for _, k := range slices.Sorted(maps.Keys(inv.Kinds)) {
		m := inv.Kinds[k]
		if m.Type == "" || reflect.DeepEqual(shipped.Kinds[k], m) {
			continue
		}
		for _, id := range kinds[k].Samples() {
			if t, ok := m.Target(id); ok {
				names = append(names, k)
				samples = append(samples, t)
			}
		}
	}
	if len(samples) == 0 {
		return found, nil, nil
	}
	Log.Verbose("checking %d inventory mappings added to the pack with an import plan", len(samples))
	fails, err := inventory.CheckImports(ctx, w.Workspace, w.runner(), "inventory/check", samples)
	if err != nil {
		return nil, nil, err
	}
	bad := map[string]string{}
	for _, i := range slices.Sorted(maps.Keys(fails)) {
		e := fails[i]
		if _, seen := bad[names[i]]; !seen {
			bad[names[i]] = fmt.Sprintf("%s (sample import ID %q): %s", inv.Kinds[names[i]].Type, samples[i].ID, oneLine(e))
			Log.Debug("inventory kind %s: mapping left out: %s", names[i], bad[names[i]])
		}
	}
	if len(bad) == 0 {
		return found, nil, nil
	}
	// The failing kinds' targets.
	gone := map[string]bool{}
	for _, e := range entries {
		if _, failed := bad[e.Kind]; failed {
			if t, ok := inv.Kinds[e.Kind].Target(e.ID); ok {
				gone[t.Key()] = true
			}
		}
	}
	found = slices.DeleteFunc(slices.Clone(found), func(t model.Target) bool { return gone[t.Key()] })
	return found, bad, nil
}

// checkPerParent reads each source added to the pack that is read once
// per parent both ways: per parent it must find everything a single read
// finds. One that misses entries is read once instead (its parent taken
// off) and returned with why, by source.
func (w Workspace) checkPerParent(ctx context.Context, inv *inventory.Inventory, targets []model.Target) map[string]string {
	shipped := w.shippedInventory()
	bad := map[string]string{}
	for i, s := range inv.Sources {
		if s.Parent == "" || isShipped(shipped, s) {
			continue
		}
		single := s
		single.Parent, single.Args = "", nil
		before, err := inventory.ReadSource(ctx, w.Workspace, w.runner(), fmt.Sprintf("inventory/single%d", i), single, w.params, targets)
		if err != nil {
			continue // can't be read once: nothing to compare
		}
		after, err := inventory.ReadSource(ctx, w.Workspace, w.runner(), fmt.Sprintf("inventory/per%d", i), s, w.params, targets)
		if err != nil {
			bad[s.Label()] = "reading it per " + s.Parent + " failed: " + oneLine(err.Error())
		} else {
			ids := map[string]bool{}
			for _, e := range after.Entries {
				ids[e.ID] = true
			}
			missed := 0
			for _, e := range before.Entries {
				if !ids[e.ID] {
					missed++
				}
			}
			if missed > 0 {
				bad[s.Label()] = fmt.Sprintf("read per %s it misses %d of the %d entries a single read finds", s.Parent, missed, len(before.Entries))
			}
		}
		if why, ok := bad[s.Label()]; ok {
			inv.Sources[i] = single
			Log.Debug("inventory source %s: read once instead: %s", s.Label(), why)
		}
	}
	return bad
}
