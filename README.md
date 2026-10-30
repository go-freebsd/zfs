# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo, no dependencies. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** The low-level ioctl layer exists; the high-level
> dataset API is being built. Nothing here is released yet.

The packed-nvlist payload of every ZFS ioctl is handled by
[go-freebsd/nvlist](https://github.com/go-freebsd/nvlist), a separate module with
struct-tag `Marshal`/`Unmarshal`.

| Package | What it is |
|---|---|
| `pkg/ioctl` | `zfs_cmd_t`, the `ZFS_IOC_*` numbers, and `Conn` for `/dev/zfs` (FreeBSD). `Conn.Call` reaches any ioctl. |
| `pkg/zfs` | High-level API (datasets, snapshots, properties, send/receive). Planned. |

```go
c, _ := ioctl.Open()
defer c.Close()
out, err := c.Call(ioctl.IOCObjsetStats, "tank", nil) // properties of "tank"
```

The portable code builds and is unit-tested everywhere; talking to the kernel
needs FreeBSD (14.3 and 15.0 are covered by CI, which creates a real pool on a
file vdev in a VM).
