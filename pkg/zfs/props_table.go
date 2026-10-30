package zfs

import "fmt"

// The property table. It lists what the kernel does not tell: the kind of
// each property and, for enumerations, the names of the stored numbers. The
// numbers are the kernel's; the FreeBSD CI tests set every value through
// zfs(8) and fail if a number here differs, and fail if the kernel returns
// a property that is missing from this table.

const (
	tFS   = Filesystem
	tVol  = Volume
	tFV   = Filesystem | Volume
	tSnap = Snapshot
	tAll  = Filesystem | Volume | Snapshot
)

var (
	onOff = []PropValue{{"off", 0}, {"on", 1}}
	noYes = []PropValue{{"no", 0}, {"yes", 1}}
)

func vals(pairs ...any) []PropValue {
	out := make([]PropValue, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, PropValue{pairs[i].(string), uint64(pairs[i+1].(int))})
	}
	return out
}

// zstdLevels are zstd-1 to zstd-19: ZIO_COMPRESS_ZSTD with the level in the
// bits above SPA_COMPRESSBITS (7). The "zstd-fast" levels are not listed.
func zstdLevels() []PropValue {
	out := make([]PropValue, 0, 19)
	for n := uint64(1); n <= 19; n++ {
		out = append(out, PropValue{fmt.Sprintf("zstd-%d", n), 16 | n<<7})
	}
	return out
}

func boolProp(name string, types Type, def string) PropInfo {
	return PropInfo{Name: name, Kind: KindBool, Values: onOff, Inherits: true, Types: types}.withDefault(def)
}

func enumProp(name string, types Type, def string, v []PropValue) PropInfo {
	return PropInfo{Name: name, Kind: KindEnum, Values: v, Inherits: true, Types: types}.withDefault(def)
}

func roSize(name string, types Type) PropInfo {
	return PropInfo{Name: name, Kind: KindSize, ReadOnly: true, Types: types}
}

func roNumber(name string, types Type) PropInfo {
	return PropInfo{Name: name, Kind: KindNumber, ReadOnly: true, Types: types}
}

var propTable = []PropInfo{
	// On/off switches.
	boolProp("atime", tFS, "on"),
	boolProp("relatime", tFS, "on"),
	boolProp("devices", tFS, "on"),
	boolProp("exec", tFS, "on"),
	boolProp("setuid", tFS, "on"),
	boolProp("readonly", tFV, "off"),
	boolProp("jailed", tFS, "off"),
	boolProp("zoned", tFS, "off"),
	boolProp("nbmand", tFS, "off"),
	boolProp("vscan", tFS, "off"),
	boolProp("overlay", tFS, "on"),
	boolProp("volthreading", tVol, "on"), // FreeBSD 15
	PropInfo{Name: "longname", Kind: KindBool, Values: onOff, CreateOnly: true, Types: tFS}.withDefault("off"), // FreeBSD 15
	PropInfo{Name: "utf8only", Kind: KindBool, Values: onOff, CreateOnly: true, Types: tFS}.withDefault("off"),
	{Name: "mounted", Kind: KindBool, Values: noYes, ReadOnly: true, Types: tFS},
	{Name: "defer_destroy", Kind: KindBool, Values: onOff, ReadOnly: true, Types: tSnap},

	// Enumerations.
	enumProp("aclinherit", tFS, "restricted", vals("discard", 0, "noallow", 1, "restricted", 4, "passthrough", 3, "passthrough-x", 5)),
	enumProp("aclmode", tFS, "discard", vals("discard", 0, "groupmask", 2, "passthrough", 3, "restricted", 4)),
	enumProp("acltype", tFS, "nfsv4", vals("off", 0, "posix", 1, "nfsv4", 2)),
	enumProp("canmount", tFS, "on", vals("off", 0, "on", 1, "noauto", 2)),
	PropInfo{Name: "casesensitivity", Kind: KindEnum, Values: vals("sensitive", 0, "insensitive", 1, "mixed", 2), CreateOnly: true, Types: tFS}.withDefault("sensitive"),
	enumProp("checksum", tFV, "on", vals("on", 1, "off", 2, "fletcher2", 6, "fletcher4", 7, "sha256", 8,
		"noparity", 10, "sha512", 11, "skein", 12, "edonr", 13, "blake3", 14)),
	enumProp("compression", tFV, "on", append(vals("on", 1, "off", 2, "lzjb", 3,
		"gzip-1", 5, "gzip-2", 6, "gzip-3", 7, "gzip-4", 8, "gzip-5", 9, "gzip", 10, "gzip-7", 11, "gzip-8", 12, "gzip-9", 13,
		"zle", 14, "lz4", 15, "zstd", 16), zstdLevels()...)),
	enumProp("copies", tFV, "1", vals("1", 1, "2", 2, "3", 3)),
	enumProp("dedup", tFV, "off", vals("off", 2, "on", 1, "verify", 257, "sha256", 8, "sha256,verify", 264,
		"sha512", 11, "sha512,verify", 267, "skein", 12, "skein,verify", 268,
		"edonr,verify", 269, "blake3", 14, "blake3,verify", 270)),
	enumProp("dnodesize", tFS, "legacy", vals("legacy", 0, "auto", 1, "1k", 1024, "2k", 2048, "4k", 4096, "8k", 8192, "16k", 16384)),
	enumProp("logbias", tFV, "latency", vals("latency", 0, "throughput", 1)),
	enumProp("primarycache", tFV, "all", vals("none", 0, "metadata", 1, "all", 2)),
	enumProp("prefetch", tFV, "all", vals("none", 0, "metadata", 1, "all", 2)),
	enumProp("direct", tFV, "standard", vals("disabled", 0, "standard", 1, "always", 2)), // OpenZFS 2.3 / FreeBSD 15`
	enumProp("secondarycache", tFV, "all", vals("none", 0, "metadata", 1, "all", 2)),
	enumProp("redundant_metadata", tFV, "all", vals("all", 0, "most", 1, "some", 2, "none", 3)),
	enumProp("snapdev", tFV, "hidden", vals("hidden", 0, "visible", 1)),
	enumProp("snapdir", tFS, "hidden", vals("hidden", 0, "visible", 1)),
	enumProp("sync", tFV, "standard", vals("standard", 0, "always", 1, "disabled", 2)),
	enumProp("volmode", tVol, "default", vals("default", 0, "full", 1, "dev", 2, "none", 3)),
	{Name: "xattr", Kind: KindEnum, Inherits: true, Types: tFS, Values: vals("off", 0, "dir", 1, "sa", 2)}, // "on" is 1 on FreeBSD 14.3 and 2 on 15.0: use "dir" or "sa"
	PropInfo{Name: "encryption", Kind: KindEnum, CreateOnly: true, Types: tFV, Values: vals("off", 2, "on", 1,
		"aes-128-ccm", 3, "aes-192-ccm", 4, "aes-256-ccm", 5, "aes-128-gcm", 6, "aes-192-gcm", 7, "aes-256-gcm", 8)}.withDefault("off"),
	PropInfo{Name: "keyformat", Kind: KindEnum, CreateOnly: true, Types: tFV, Values: vals("none", 0, "raw", 1, "hex", 2, "passphrase", 3)}.withDefault("none"),
	{Name: "keystatus", Kind: KindEnum, ReadOnly: true, Types: tFV, Values: vals("none", 0, "unavailable", 1, "available", 2)},
	{Name: "type", Kind: KindEnum, ReadOnly: true, Types: tAll, Values: vals("filesystem", 2, "volume", 3)}, // the object set type; see Dataset.Enum

	// Sizes, in bytes.
	roSize("used", tAll),
	roSize("available", tFV),
	roSize("referenced", tAll),
	roSize("logicalused", tAll),
	roSize("logicalreferenced", tAll),
	roSize("usedbysnapshots", tFV),
	roSize("usedbydataset", tFV),
	roSize("usedbychildren", tFV),
	roSize("usedbyrefreservation", tFV),
	roSize("written", tAll),
	PropInfo{Name: "quota", Kind: KindSize, Types: tFS, None: NoneIsZero}.withDefaultNum(0),
	PropInfo{Name: "refquota", Kind: KindSize, Types: tFS, None: NoneIsZero}.withDefaultNum(0),
	PropInfo{Name: "reservation", Kind: KindSize, Types: tFV, None: NoneIsZero}.withDefaultNum(0),
	PropInfo{Name: "refreservation", Kind: KindSize, Types: tFV, None: NoneIsZero}.withDefaultNum(0),
	{Name: "volsize", Kind: KindSize, Types: tVol},
	{Name: "volblocksize", Kind: KindSize, CreateOnly: true, Types: tVol},
	PropInfo{Name: "recordsize", Kind: KindSize, Inherits: true, Types: tFS}.withDefaultNum(131072),
	PropInfo{Name: "special_small_blocks", Kind: KindSize, Inherits: true, Types: tFS}.withDefaultNum(0),

	// Numbers.
	roNumber("guid", tAll),
	roNumber("createtxg", tAll),
	roNumber("numclones", tSnap),
	roNumber("objsetid", tAll),
	roNumber("userrefs", tSnap),
	roNumber("useraccounting", tAll),
	roSize("unique", tSnap),
	roNumber("snapshot_count", tFV),
	roNumber("filesystem_count", tFS),
	{Name: "version", Kind: KindNumber, Types: tFS},
	{Name: "pbkdf2salt", Kind: KindNumber, ReadOnly: true, Types: tFV}, // chosen by Create for passphrase keys
	PropInfo{Name: "pbkdf2iters", Kind: KindNumber, CreateOnly: true, Types: tFV}.withDefaultNum(0),
	PropInfo{Name: "snapshot_limit", Kind: KindNumber, Types: tFV, None: NoneIsMax}.withDefaultNum(^uint64(0)),
	PropInfo{Name: "filesystem_limit", Kind: KindNumber, Types: tFS, None: NoneIsMax}.withDefaultNum(^uint64(0)),

	// Times and ratios.
	{Name: "creation", Kind: KindTime, ReadOnly: true, Types: tAll},
	{Name: "snapshots_changed", Kind: KindTime, ReadOnly: true, Types: tFV},
	{Name: "compressratio", Kind: KindRatio, ReadOnly: true, Types: tAll},
	{Name: "refcompressratio", Kind: KindRatio, ReadOnly: true, Types: tAll},

	// Strings.
	{Name: "mountpoint", Kind: KindString, Inherits: true, Types: tFS},
	PropInfo{Name: "sharenfs", Kind: KindString, Inherits: true, Types: tFS}.withDefaultStr("off"),
	PropInfo{Name: "sharesmb", Kind: KindString, Inherits: true, Types: tFS}.withDefaultStr("off"),
	PropInfo{Name: "mlslabel", Kind: KindString, Inherits: true, Types: tFS}.withDefaultStr("none"),
	PropInfo{Name: "context", Kind: KindString, Types: tFS}.withDefaultStr("none"),
	PropInfo{Name: "fscontext", Kind: KindString, Types: tFS}.withDefaultStr("none"),
	PropInfo{Name: "defcontext", Kind: KindString, Types: tFS}.withDefaultStr("none"),
	PropInfo{Name: "rootcontext", Kind: KindString, Types: tFS}.withDefaultStr("none"),
	PropInfo{Name: "keylocation", Kind: KindString, Types: tFV}.withDefaultStr("none"),
	{Name: "origin", Kind: KindString, ReadOnly: true, Types: tFV},
	{Name: "encryptionroot", Kind: KindString, ReadOnly: true, Types: tFV},
	{Name: "receive_resume_token", Kind: KindString, ReadOnly: true, Types: tFV},
	{Name: "prevsnap", Kind: KindString, ReadOnly: true, Types: tFV},

	// Returned as the kernel gives them.
	{Name: "redact_snaps", Kind: KindOpaque, ReadOnly: true, Types: tAll},

	// Lists.
	{Name: "clones", Kind: KindList, ReadOnly: true, Types: tSnap},
}
