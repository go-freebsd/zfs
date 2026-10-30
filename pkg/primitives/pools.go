package primitives

import (
	"sort"

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
