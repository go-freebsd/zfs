package zfs

import (
	"fmt"
	"sort"
	"time"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Holds lists the holds on a snapshot, ordered by tag.
func (c *Client) Holds(snapshot string) ([]Hold, error) {
	out, err := c.c.Call(ioctl.IOCGetHolds, snapshot, nil)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	holds := make([]Hold, 0, out.Len())
	for _, p := range out.Pairs() {
		secs, ok := p.Value.(uint64)
		if !ok {
			return nil, fmt.Errorf("zfs: hold %q on %s: unexpected %s pair", p.Name, snapshot, p.Type)
		}
		holds = append(holds, Hold{Tag: p.Name, Created: time.Unix(int64(secs), 0)})
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].Tag < holds[j].Tag })
	return holds, nil
}
