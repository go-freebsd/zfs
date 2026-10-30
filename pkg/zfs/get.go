package zfs

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// dmu_objset_type_t
const (
	ostZFS  = 2
	ostZVOL = 3
)

// datasetFrom builds a Dataset from the Cmd and the property list the
// kernel returned. name is used if the kernel did not rewrite zc_name.
func datasetFrom(cmd *ioctl.Cmd, props *nvlist.List) (*Dataset, error) {
	st := &cmd.ObjsetStats
	d := &Dataset{
		Name:         cmd.GetName(),
		GUID:         st.GUID,
		CreationTxg:  st.CreationTxg,
		NumClones:    st.NumClones,
		Origin:       st.OriginName(),
		Inconsistent: st.Inconsistent != 0,
	}
	switch {
	case st.IsSnapshot != 0:
		d.Type = Snapshot
	case st.Type == ostZFS:
		d.Type = Filesystem
	case st.Type == ostZVOL:
		d.Type = Volume
	}
	if props != nil {
		if err := nvlist.Decode(props, &d.Props); err != nil {
			return nil, fmt.Errorf("zfs: properties of %s: %w", d.Name, err)
		}
	}
	return d, nil
}

func isNotExist(err error) bool { return errors.Is(err, syscall.ENOENT) }

// Exists reports whether a dataset, snapshot or bookmark exists.
func (c *Client) Exists(name string) (bool, error) {
	if i := strings.IndexByte(name, '#'); i >= 0 {
		_, err := c.Get(name)
		if isNotExist(err) {
			return false, nil
		}
		return err == nil, err
	}
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(name); err != nil {
		return false, err
	}
	cmd.Simple = 1 // no property list needed
	if _, err := c.c.Do(ioctl.IOCObjsetStats, cmd, nil); err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Get returns a filesystem, volume, snapshot or bookmark (name#bookmark).
// If props are named, only those properties are kept.
func (c *Client) Get(name string, props ...string) (*Dataset, error) {
	return c.get(name, false, props)
}

// get is Get; simple skips the property list (and for a bookmark asks only
// for its identity).
func (c *Client) get(name string, simple bool, props []string) (*Dataset, error) {
	var d *Dataset
	if i := strings.IndexByte(name, '#'); i >= 0 {
		bp := props
		if simple && len(bp) == 0 {
			bp = []string{"guid", "createtxg"}
		}
		bms, err := c.Bookmarks(name[:i], bp...)
		if err != nil {
			return nil, err
		}
		for _, b := range bms {
			if b.Name == name {
				d = b
			}
		}
		if d == nil {
			return nil, &ioctl.Error{Ioc: ioctl.IOCGetBookmarks, Name: name, Errno: syscall.ENOENT}
		}
	} else {
		cmd := ioctl.NewCmd()
		if err := cmd.SetName(name); err != nil {
			return nil, err
		}
		if simple {
			cmd.Simple = 1
		}
		out, err := c.c.Do(ioctl.IOCObjsetStats, cmd, nil)
		if err != nil {
			return nil, err
		}
		if d, err = datasetFrom(cmd, out); err != nil {
			return nil, err
		}
	}
	if len(props) > 0 {
		keep := make(map[string]Property, len(props))
		for _, p := range props {
			if v, ok := d.Props[p]; ok {
				keep[p] = v
			}
		}
		d.Props = keep
	}
	return d, nil
}
