# zfs

A pure-Go ZFS client that talks to the kernel through `/dev/zfs` ioctls: no
libzfs, no libzfs_core, no cgo. The sibling of
[go-freebsd/jail](https://github.com/go-freebsd/jail).

> **Status: work in progress.** Reading works (datasets, snapshots, bookmarks,
> typed properties, holds, space, pools) and so does setting properties.
> Creating and destroying datasets, and send/receive, are next. Nothing here is
> released yet.

| Package | What it is |
|---|---|
| `pkg/zfs` | The high-level client: `Open`, `Get`, `Exists`, `List`/`Walk`, `Bookmarks`, `Holds`, `SpaceWritten`, `SnapshotRangeSpace`, `Pools`, `SetProps`, `InheritProp`. |
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

## Properties

`Dataset.Props` holds the raw values the kernel returns (a `uint64` or a string,
with the dataset they were set on). Typed helpers turn them into Go types using a
built-in property table:

```go
d, _ := c.Get("tank/home")
d.Enum("compression")      // "lz4"   (the kernel stores the number 15)
d.Bool("atime")            // true
d.Value("quota")           // uint64 bytes
d.Time("creation")         // time.Time
d.Ratio("compressratio")   // 1.5
d.Source("compression")    // inherited from tank
d.Mountpoint()             // the effective one, as zfs get shows it

c.SetProps("tank/home", map[string]any{
    "compression": "lz4",     // enum name
    "atime":       false,     // on/off as bool
    "quota":       "10G",     // size string or an integer number of bytes
    "recordsize":  "64k",
    "copies":      2,
    "acme:owner":  "web",     // user property
})
c.InheritProp("tank/home", "compression")
```

Values are checked and converted before anything is sent; a wrong value fails with a
message naming the property and the allowed choices. If the kernel refuses some
properties, the error is a `*zfs.SetPropsError` listing each. `quota`, `refquota`,
`reservation` and `refreservation` accept `"none"`, the limits too. `zfs.ParseSize`
parses sizes the way `zfs set` does. Per-user quotas (`userquota@...`),
`refreservation=auto` and the `zstd` levels are not covered yet.

The table (`props_table.go`) is checked in CI against the kernel: every enumerated value
is set through `zfs(8)` and the stored number compared, and any property the kernel
returns that the table does not know fails the build.

Errors are `*ioctl.Error` and unwrap to the errno: `errors.Is(err, fs.ErrNotExist)`.

## Wrapped ioctls

| ioctl | Wrapped as |
|---|---|
| `OBJSET_STATS` | `Get`, `Exists` |
| `DATASET_LIST_NEXT`, `SNAPSHOT_LIST_NEXT` | `List`, `Walk` |
| `GET_HOLDS` | `Holds` |
| `GET_BOOKMARKS` | `Bookmarks` |
| `SPACE_WRITTEN`, `SPACE_SNAPS` | `SpaceWritten`, `SnapshotRangeSpace` |
| `POOL_CONFIGS` | `Pools` |
| `SET_PROP`, `INHERIT_PROP` | `SetProps`, `SetProp`, `InheritProp` |
| everything else | `ioctl.Conn.Call` / `Do` / `Ioctl` |

## Testing

The portable code is unit-tested everywhere against an in-memory fake kernel that
enforces the list cursor protocol. CI also runs FreeBSD 14.3 and 15.0 VMs with a real
pool on a file vdev, builds a set of fixtures (`internal/ci/fixtures.sh`: nested
datasets, a volume, snapshots, holds, a bookmark, a clone, 150 large user
properties, 120 children) and compares what this library reads with `zfs(8)` and
`zpool(8)`.
