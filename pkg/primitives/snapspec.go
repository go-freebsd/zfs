package primitives

import (
	"fmt"
	"sort"
	"strings"
	"syscall"
)

// DestroyPlan is what a destroy of snapshots by specification would do.
type DestroyPlan struct {
	// Snapshots are the full names, grouped by dataset, oldest first.
	Snapshots []string
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
// error wraps ENOENT. A snapshot with clones is refused with EBUSY unless
// deferred, as zfs destroy refuses it.
func (c *Client) PlanDestroySnapshots(dataset, spec string, recursive, deferred bool) (*DestroyPlan, error) {
	if dataset == "" || strings.ContainsAny(dataset, "@#") || spec == "" {
		return nil, fmt.Errorf("zfs: destroy snapshots: need a dataset and a specification, got %q @ %q", dataset, spec)
	}
	root, err := c.get(dataset, false, nil)
	if err != nil {
		return nil, err
	}
	if root.Type != Filesystem && root.Type != Volume {
		return nil, fmt.Errorf("zfs: destroy snapshots: %s is not a filesystem or volume", dataset)
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
			return nil, err
		}
	}

	plan := &DestroyPlan{}
	for _, ds := range datasets {
		all, err := c.snapshotsOf(ds)
		if err != nil {
			return nil, err
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
					return nil, err
				}
				continue
			}
			if !deferred {
				if clones := s.Clones(); len(clones) > 0 {
					sort.Strings(clones)
					return nil, fmt.Errorf("zfs: cannot destroy %q: snapshot has dependent clones (%s): %w", s.Name, strings.Join(clones, ", "), syscall.EBUSY)
				}
			}
			if range0 == "" {
				range0 = names[i]
			}
			prev = names[i]
			plan.Snapshots = append(plan.Snapshots, s.Name)
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(plan.Snapshots) == 0 {
		return nil, fmt.Errorf("zfs: could not find any snapshots to destroy; check snapshot names: %w", syscall.ENOENT)
	}
	return plan, nil
}

// DestroySnapshotSpec destroys what PlanDestroySnapshots names, all or none
// (zfs destroy dataset@spec). It returns the names destroyed.
func (c *Client) DestroySnapshotSpec(dataset, spec string, recursive, deferred bool) ([]string, error) {
	plan, err := c.PlanDestroySnapshots(dataset, spec, recursive, deferred)
	if err != nil {
		return nil, err
	}
	if err := c.DestroySnapshots(plan.Snapshots, deferred); err != nil {
		return nil, err
	}
	return plan.Snapshots, nil
}
