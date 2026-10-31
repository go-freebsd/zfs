package primitives

import (
	"fmt"
	"sort"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Pools lists the names of the imported pools, sorted.
func (c *Client) Pools() ([]string, error) {
	cmd := ioctl.NewCmd() // cookie 0: the current configuration generation
	out, err := c.c.Do(ioctl.IOCPoolConfigs, cmd, nil)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	names := out.Names()
	sort.Strings(names)
	return names, nil
}

// PoolProps returns the properties of a pool (zpool get all): numbers as
// uint64, the rest as strings. The source of a pool property is not returned.
func (c *Client) PoolProps(pool string) (map[string]any, error) {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(pool); err != nil {
		return nil, err
	}
	out, err := c.c.Do(ioctl.IOCPoolGetProps, cmd, nil)
	if err != nil {
		return nil, err
	}
	props := map[string]any{}
	if out == nil {
		return props, nil
	}
	for _, p := range out.Pairs() {
		l, ok := p.Value.(*nvlist.List)
		if !ok {
			continue
		}
		if v, ok := l.Get("value"); ok {
			props[p.Name] = v
		}
	}
	return props, nil
}

// PoolReadOnly reports whether the pool is imported read-only
// (zpool import -o readonly=on): nothing can be written to it, so mounts are
// read-only and a send takes no holds.
func (c *Client) PoolReadOnly(pool string) (bool, error) {
	props, err := c.PoolProps(pool)
	if err != nil {
		return false, err
	}
	switch v := props["readonly"].(type) {
	case uint64:
		return v != 0, nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("zfs: pool %s: readonly property is %T", pool, v)
	}
}
