package zfs

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

func boolProp(name string, types Type) PropInfo {
	return PropInfo{Name: name, Kind: KindBool, Values: onOff, Inherits: true, Types: types}
}

func enumProp(name string, types Type, v []PropValue) PropInfo {
	return PropInfo{Name: name, Kind: KindEnum, Values: v, Inherits: true, Types: types}
}

func roSize(name string, types Type) PropInfo {
	return PropInfo{Name: name, Kind: KindSize, ReadOnly: true, Types: types}
}

func roNumber(name string, types Type) PropInfo {
	return PropInfo{Name: name, Kind: KindNumber, ReadOnly: true, Types: types}
}

var propTable = []PropInfo{
	// On/off switches.
	boolProp("atime", tFS),
	boolProp("relatime", tFS),
	boolProp("devices", tFS),
	boolProp("exec", tFS),
	boolProp("setuid", tFS),
	boolProp("readonly", tFV),
	boolProp("jailed", tFS),
	boolProp("zoned", tFS),
	boolProp("nbmand", tFS),
	boolProp("vscan", tFS),
	boolProp("overlay", tFS),
	{Name: "utf8only", Kind: KindBool, Values: onOff, CreateOnly: true, Types: tFS},
	{Name: "mounted", Kind: KindBool, Values: noYes, ReadOnly: true, Types: tFS},
	{Name: "defer_destroy", Kind: KindBool, Values: noYes, ReadOnly: true, Types: tSnap},

	// Enumerations.
	enumProp("aclinherit", tFS, vals("discard", 0, "noallow", 1, "restricted", 4, "passthrough", 3, "passthrough-x", 5)),
	enumProp("aclmode", tFS, vals("discard", 0, "groupmask", 2, "passthrough", 3, "restricted", 4)),
	enumProp("acltype", tFS, vals("off", 0, "posix", 1, "nfsv4", 2)),
	enumProp("canmount", tFS, vals("off", 0, "on", 1, "noauto", 2)),
	{Name: "casesensitivity", Kind: KindEnum, Values: vals("sensitive", 0, "insensitive", 1, "mixed", 2), CreateOnly: true, Types: tFS},
	enumProp("checksum", tFV, vals("on", 1, "off", 2, "fletcher2", 6, "fletcher4", 7, "sha256", 8,
		"noparity", 10, "sha512", 11, "skein", 12, "edonr", 13, "blake3", 14)),
	enumProp("compression", tFV, vals("on", 1, "off", 2, "lzjb", 3,
		"gzip-1", 5, "gzip-2", 6, "gzip-3", 7, "gzip-4", 8, "gzip-5", 9, "gzip", 10, "gzip-7", 11, "gzip-8", 12, "gzip-9", 13,
		"zle", 14, "lz4", 15)),
	enumProp("copies", tFV, vals("1", 1, "2", 2, "3", 3)),
	enumProp("dedup", tFV, vals("off", 2, "on", 1, "verify", 257, "sha256", 8, "sha256,verify", 264,
		"sha512", 11, "sha512,verify", 267, "skein", 12, "skein,verify", 268,
		"edonr", 13, "edonr,verify", 269, "blake3", 14, "blake3,verify", 270)),
	enumProp("dnodesize", tFS, vals("legacy", 0, "auto", 1, "1k", 1024, "2k", 2048, "4k", 4096, "8k", 8192, "16k", 16384)),
	enumProp("logbias", tFV, vals("latency", 0, "throughput", 1)),
	enumProp("primarycache", tFV, vals("none", 0, "metadata", 1, "all", 2)),
	enumProp("secondarycache", tFV, vals("none", 0, "metadata", 1, "all", 2)),
	enumProp("redundant_metadata", tFV, vals("all", 0, "most", 1, "some", 2, "none", 3)),
	enumProp("snapdev", tFV, vals("hidden", 0, "visible", 1)),
	enumProp("snapdir", tFS, vals("hidden", 0, "visible", 1)),
	enumProp("sync", tFV, vals("standard", 0, "always", 1, "disabled", 2)),
	enumProp("volmode", tVol, vals("default", 0, "full", 1, "dev", 2, "none", 3)),
	enumProp("xattr", tFS, vals("off", 0, "dir", 1, "on", 2, "sa", 2)),
	{Name: "encryption", Kind: KindEnum, CreateOnly: true, Types: tFV, Values: vals("off", 2, "on", 1,
		"aes-128-ccm", 3, "aes-192-ccm", 4, "aes-256-ccm", 5, "aes-128-gcm", 6, "aes-192-gcm", 7, "aes-256-gcm", 8)},
	{Name: "keyformat", Kind: KindEnum, CreateOnly: true, Types: tFV, Values: vals("none", 0, "raw", 1, "hex", 2, "passphrase", 3)},
	{Name: "keystatus", Kind: KindEnum, ReadOnly: true, Types: tFV, Values: vals("none", 0, "unavailable", 1, "available", 2)},
	{Name: "type", Kind: KindEnum, ReadOnly: true, Types: tAll, Values: vals("filesystem", 1, "snapshot", 2, "volume", 4, "bookmark", 16)},

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
	{Name: "quota", Kind: KindSize, Types: tFS, None: NoneIsZero},
	{Name: "refquota", Kind: KindSize, Types: tFS, None: NoneIsZero},
	{Name: "reservation", Kind: KindSize, Types: tFV, None: NoneIsZero},
	{Name: "refreservation", Kind: KindSize, Types: tFV, None: NoneIsZero},
	{Name: "volsize", Kind: KindSize, Types: tVol},
	{Name: "volblocksize", Kind: KindSize, CreateOnly: true, Types: tVol},
	{Name: "recordsize", Kind: KindSize, Inherits: true, Types: tFS},
	{Name: "special_small_blocks", Kind: KindSize, Inherits: true, Types: tFS},

	// Numbers.
	roNumber("guid", tAll),
	roNumber("createtxg", tAll),
	roNumber("numclones", tSnap),
	roNumber("objsetid", tAll),
	roNumber("userrefs", tSnap),
	roNumber("snapshot_count", tFV),
	roNumber("filesystem_count", tFS),
	{Name: "version", Kind: KindNumber, Types: tFS},
	{Name: "pbkdf2iters", Kind: KindNumber, CreateOnly: true, Types: tFV},
	{Name: "snapshot_limit", Kind: KindNumber, Types: tFV, None: NoneIsMax},
	{Name: "filesystem_limit", Kind: KindNumber, Types: tFS, None: NoneIsMax},

	// Times and ratios.
	{Name: "creation", Kind: KindTime, ReadOnly: true, Types: tAll},
	{Name: "snapshots_changed", Kind: KindTime, ReadOnly: true, Types: tFV},
	{Name: "compressratio", Kind: KindRatio, ReadOnly: true, Types: tAll},
	{Name: "refcompressratio", Kind: KindRatio, ReadOnly: true, Types: tAll},

	// Strings.
	{Name: "mountpoint", Kind: KindString, Inherits: true, Types: tFS},
	{Name: "sharenfs", Kind: KindString, Inherits: true, Types: tFS},
	{Name: "sharesmb", Kind: KindString, Inherits: true, Types: tFS},
	{Name: "mlslabel", Kind: KindString, Inherits: true, Types: tFS},
	{Name: "keylocation", Kind: KindString, Types: tFV},
	{Name: "origin", Kind: KindString, ReadOnly: true, Types: tFV},
	{Name: "encryptionroot", Kind: KindString, ReadOnly: true, Types: tFV},
	{Name: "receive_resume_token", Kind: KindString, ReadOnly: true, Types: tFV},

	// Lists.
	{Name: "clones", Kind: KindList, ReadOnly: true, Types: tSnap},
}
