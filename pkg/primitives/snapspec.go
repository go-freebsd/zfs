package primitives

import (
	"fmt"
	"strings"
	"syscall"
)

// DestroyPlan is what a destroy of snapshots by specification would do.
type DestroyPlan struct {
	// Snapshots are the full names, grouped by dataset, oldest first.
	Snapshots []string
	// Datasets are the filesystems and volumes a destroy of a dataset would
	// remove, and Order everything (snapshots too) in the order it happens:
	// a clone before its origin, snapshots before their dataset. Only the
	// plan of a dataset fills them.
	Datasets, Order []string
	// Reclaim is the space destroying them would free (the "would reclaim"
	// of zfs destroy -nv).
	Reclaim uint64
}

// expandSnapSpec returns, for one dataset, the snapshots a specification
// names: "a", "a%b" (inclusive range), "%b" (from the oldest), "a%" (to the
// newest), "%" (all), joined by commas, as zfs_iter_snapspec does. Names
// that do not exist are skipped; ok is false if there were any.
func expandSnapSpec(snaps []string, spec string) (picked map[string]bool, ok bool) {
	picked = map[string]bool{}
	ok = true
	has := map[string]bool{}
	for _, s := range snaps {
		has[s] = true
	}
	for _, item := range strings.Split(spec, ",") {
		first, last, isRange := strings.Cut(item, "%")
		if !isRange {
			if has[item] {
				picked[item] = true
			} else {
				ok = false
			}
			continue
		}
		if last != "" && !has[last] {
			ok = false
			continue
		}
		seenFirst, seenLast := first == "", false
		for _, s := range snaps {
			if seenLast {
				break
			}
			if !seenFirst && s == first {
				seenFirst = true
			}
			if s == last {
				seenLast = true
			}
			if seenFirst {
				picked[s] = true
			}
		}
		if !seenFirst || (last != "" && !seenLast) {
			ok = false
		}
	}
	return picked, ok
}

func shortOf(snapshot string) string {
	_, s, _ := strings.Cut(snapshot, "@")
	return s
}

// PlanDestroySnapshots works out what zfs destroy [-r] [-d] dataset@spec
// would destroy, and what that frees, without destroying anything. spec is
// the part after the @ and may name ranges and lists (see the zfs
// documentation of destroy): "a%c", "%c", "a%", "%", "a,b%d". With
// recursive the same specification is applied to every descendant that has
// such snapshots. Names that are missing are skipped; if nothing is left the
// error wraps ENOENT. A snapshot with clones is refused with a
// *DependentsError (EBUSY) unless deferred or unless clones is set, which
// destroys what depends on the snapshots first (zfs destroy -R; Order of the
// plan lists it).
func (c *Client) PlanDestroySnapshots(dataset, spec string, recursive, deferred, clones bool) (*DestroyPlan, error) {
	plan, _, err := c.planSnapshots(dataset, spec, recursive, deferred, clones)
	return plan, err
}

// planSnapshots is PlanDestroySnapshots and also returns the dependents to
// destroy first (Clones), as the steps of destroyItems.
func (c *Client) planSnapshots(dataset, spec string, recursive, deferred, clones bool) (*DestroyPlan, []destroyItem, error) {
	if dataset == "" || strings.ContainsAny(dataset, "@#") || spec == "" {
		return nil, nil, fmt.Errorf("zfs: destroy snapshots: need a dataset and a specification, got %q @ %q", dataset, spec)
	}
	root, err := c.get(dataset, false, nil)
	if err != nil {
		return nil, nil, err
	}
	if root.Type != Filesystem && root.Type != Volume {
		return nil, nil, fmt.Errorf("zfs: destroy snapshots: %s is not a filesystem or volume", dataset)
	}
	datasets := []string{dataset}
	if recursive {
		err := c.Walk(dataset, ListOptions{Types: Filesystem | Volume, Depth: Unlimited, Simple: true}, func(d *Dataset) error {
			if d.Name != dataset {
				datasets = append(datasets, d.Name)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}

	plan := &DestroyPlan{}
	var deps []destroyItem
	seen := map[string]bool{}
	for _, ds := range datasets {
		all, err := c.snapshotsOf(ds)
		if err != nil {
			return nil, nil, err
		}
		names := make([]string, len(all))
		for i, s := range all {
			names[i] = shortOf(s.Name)
		}
		picked, _ := expandSnapSpec(names, spec) // missing names are no error, as in zfs_do_destroy
		var range0, prev string
		flush := func() error {
			if range0 == "" {
				return nil
			}
			sp, err := c.SnapshotRangeSpace(ds+"@"+range0, ds+"@"+prev)
			plan.Reclaim += sp.Used
			range0, prev = "", ""
			return err
		}
		for i, s := range all {
			if !picked[names[i]] {
				if err := flush(); err != nil {
					return nil, nil, err
				}
				continue
			}
			if clones {
				// zfs destroy -R: what depends on the snapshot goes first, and
				// clones are not deferred (only snapshots can be).
				ds, err := c.dependents(s, false)
				if err != nil {
					return nil, nil, err
				}
				for _, d := range ds {
					if !seen[d.Name] {
						seen[d.Name] = true
						deps = append(deps, destroyItem{d, false})
					}
				}
			} else if !deferred {
				if cl := s.Clones(); len(cl) > 0 {
					return nil, nil, &DependentsError{Target: s.Name, Clones: cl}
				}
			}
			if range0 == "" {
				range0 = names[i]
			}
			prev = names[i]
			plan.Snapshots = append(plan.Snapshots, s.Name)
		}
		if err := flush(); err != nil {
			return nil, nil, err
		}
	}
	if len(plan.Snapshots) == 0 {
		return nil, nil, fmt.Errorf("zfs: could not find any snapshots to destroy; check snapshot names: %w", syscall.ENOENT)
	}
	for _, it := range deps {
		plan.Order = append(plan.Order, it.d.Name)
		if it.d.Type != Snapshot {
			plan.Datasets = append(plan.Datasets, it.d.Name)
		}
	}
	plan.Order = append(plan.Order, plan.Snapshots...)
	return plan, deps, nil
}

// DestroySnapshotSpec destroys what PlanDestroySnapshots names, all or none
// (zfs destroy dataset@spec). It returns the names destroyed.
func (c *Client) DestroySnapshotSpec(dataset, spec string, recursive, deferred, clones bool) ([]string, error) {
	plan, deps, err := c.planSnapshots(dataset, spec, recursive, deferred, clones)
	if err != nil {
		return nil, err
	}
	if len(deps) > 0 {
		// The dependents first: a clone has to go before its origin.
		if err := c.destroyItems(dataset, deps, &DestroyOptions{Recursive: true, Clones: true}); err != nil {
			return nil, err
		}
	}
	if err := c.DestroySnapshots(plan.Snapshots, deferred); err != nil {
		return nil, err
	}
	return plan.Snapshots, nil
}
