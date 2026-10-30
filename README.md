# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** Reading works (datasets, snapshots, bookmarks,
> properties, holds, space, pools). Creating, changing and destroying things,
> and send/receive, are next. Nothing here is released yet.

| Package | What it is |
|---|---|
| `pkg/zfs` | The high-level client: `Open`, `Get`, `Exists`, `List`/`Walk`, `Bookmarks`, `Holds`, `SpaceWritten`, `SnapshotRangeSpace`, `Pools`. |
| `pkg/ioctl` | `zfs_cmd_t`, the `ZFS_IOC_*` numbers and `Conn` for `/dev/zfs` (FreeBSD). `Conn.Call` reaches any ioctl. |

The packed-nvlist payload of every ZFS ioctl is handled by
[go-freebsd/nvlist](https://github.com/go-freebsd/nvlist), a separate module with
struct-tag `Marshal`/`Unmarshal`.

```go
c, err := zfs.Open()
if err != nil { ... }
defer c.Close()

// Like "zfs list -r -t all tank": datasets, then their snapshots and bookmarks.
all := zfs.Filesystem | zfs.Volume | zfs.Snapshot | zfs.Bookmark
ds, err := c.List("tank", zfs.ListOptions{Types: all, Depth: zfs.Unlimited})
for _, d := range ds {
    fmt.Println(d.Name, d.Type, d.Used())
}

d, _ := c.Get("tank/home")
fmt.Println(d.Mountpoint(), d.Props["compression"]) // enumerated properties are numbers
```

Property values are returned as the kernel stores them (a `uint64` or a string,
with the dataset they were set on). Enumerated properties such as `compression`
are therefore numbers until the property tables arrive with the write path.
Errors are `*ioctl.Error` and unwrap to the errno: `errors.Is(err, fs.ErrNotExist)`.

## Read ioctls

| ioctl | Wrapped as |
|---|---|
| `OBJSET_STATS` | `Get`, `Exists` |
| `DATASET_LIST_NEXT`, `SNAPSHOT_LIST_NEXT` | `List`, `Walk` |
| `GET_HOLDS` | `Holds` |
| `GET_BOOKMARKS` | `Bookmarks` |
| `SPACE_WRITTEN`, `SPACE_SNAPS` | `SpaceWritten`, `SnapshotRangeSpace` |
| `POOL_CONFIGS` | `Pools` |
| everything else | `ioctl.Conn.Call` / `Do` / `Ioctl` |

## Testing

The portable code is unit-tested everywhere against an in-memory fake kernel that
enforces the list cursor protocol. CI also runs FreeBSD 14.3 and 15.0 VMs with a real
pool on a file vdev, builds a set of fixtures (`internal/ci/fixtures.sh`: nested
datasets, a volume, snapshots, holds, a bookmark, a clone, 150 large user
properties, 120 children) and compares what this library reads with `zfs(8)` and
`zpool(8)`.
