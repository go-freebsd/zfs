package zfs

import (
	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// SpaceWritten returns the data written to newer (a filesystem, volume or
// snapshot) since older (a snapshot of the same dataset, or a bookmark such
// as "tank/fs#mark"): the "written@" property for an arbitrary point.
func (c *Client) SpaceWritten(newer, older string) (Space, error) {
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(newer); err != nil {
		return Space{}, err
	}
	if err := cmd.SetValue(older); err != nil {
		return Space{}, err
	}
	if err := c.c.Ioctl(ioctl.IOCSpaceWritten, cmd); err != nil {
		return Space{}, err
	}
	return Space{Written: cmd.Cookie, Compressed: cmd.ObjsetType, Uncompressed: cmd.PermAction}, nil
}

// SnapshotRangeSpace returns the space that destroying the snapshots from
// first through last (both snapshots of one dataset) would free.
func (c *Client) SnapshotRangeSpace(first, last string) (Space, error) {
	in := nvlist.New()
	if err := in.Add("firstsnap", first); err != nil {
		return Space{}, err
	}
	out, err := c.c.Call(ioctl.IOCSpaceSnaps, last, in)
	if err != nil {
		return Space{}, err
	}
	var r struct {
		Used         uint64 `nvlist:"used"`
		Compressed   uint64 `nvlist:"compressed"`
		Uncompressed uint64 `nvlist:"uncompressed"`
	}
	if out != nil {
		if err := nvlist.Decode(out, &r); err != nil {
			return Space{}, err
		}
	}
	return Space{Used: r.Used, Compressed: r.Compressed, Uncompressed: r.Uncompressed}, nil
}
