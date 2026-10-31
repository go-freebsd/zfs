package primitives

import (
	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// receivedProps returns the properties a dataset or snapshot got from a
// receive (ZFS_IOC_OBJSET_RECVD_PROPS), whatever has been set on it since:
// the values zfs get -s received shows. Their source is "$recvd".
func (c *Client) receivedProps(name string) (map[string]Property, error) {
	out, err := c.c.Call(ioctl.IOCObjsetRecvdProps, name, nil)
	if err != nil {
		return nil, err
	}
	props := map[string]Property{}
	if out == nil {
		return props, nil
	}
	for _, p := range out.Pairs() {
		pr := propertyFrom(p.Value)
		if _, ok := p.Value.(*nvlist.List); ok && pr.Source == "" {
			pr.Source = "$recvd"
		}
		props[p.Name] = pr
	}
	return props, nil
}
