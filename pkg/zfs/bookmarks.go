package zfs

import (
	"fmt"
	"sort"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// defaultBookmarkProps are requested when Bookmarks is called without names.
var defaultBookmarkProps = []string{"guid", "createtxg", "creation"}

// Bookmarks lists the bookmarks of a filesystem or volume, ordered by
// creation transaction. props names the bookmark properties to fetch
// (guid, createtxg, creation by default).
func (c *Client) Bookmarks(fs string, props ...string) ([]*Dataset, error) {
	if len(props) == 0 {
		props = defaultBookmarkProps
	}
	in := nvlist.New()
	for _, p := range props {
		if err := in.Add(p, nvlist.Flag{}); err != nil {
			return nil, err
		}
	}
	out, err := c.c.Call(ioctl.IOCGetBookmarks, fs, in)
	if err != nil {
		return nil, err
	}
	var bms []*Dataset
	if out == nil {
		return nil, nil
	}
	for _, p := range out.Pairs() {
		bl, ok := p.Value.(*nvlist.List)
		if !ok {
			return nil, fmt.Errorf("zfs: bookmark %s#%s: unexpected %s pair", fs, p.Name, p.Type)
		}
		d := &Dataset{Name: fs + "#" + p.Name, Type: Bookmark, Props: map[string]Property{}}
		for _, bp := range bl.Pairs() {
			d.Props[bp.Name] = propertyFrom(bp.Value)
		}
		d.GUID = d.num("guid")
		d.CreationTxg = d.num("createtxg")
		bms = append(bms, d)
	}
	sort.Slice(bms, func(i, j int) bool {
		if bms[i].CreationTxg != bms[j].CreationTxg {
			return bms[i].CreationTxg < bms[j].CreationTxg
		}
		return bms[i].Name < bms[j].Name
	})
	return bms, nil
}

// propertyFrom accepts both shapes the kernel uses: a nested {value, source}
// list, and a bare value.
func propertyFrom(v any) Property {
	l, ok := v.(*nvlist.List)
	if !ok {
		return Property{Value: v}
	}
	val, _ := l.Get("value")
	src, _ := l.String("source")
	return Property{Value: val, Source: src}
}
