package primitives

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Stream header: the first record of a send stream, struct drr_begin in
// dmu_replay_record_t.
const (
	streamMagic = 0x2F5bacbac

	hdrSubstream = 1 // DMU_SUBSTREAM: one snapshot
	hdrCompound  = 2 // DMU_COMPOUNDSTREAM: a replication stream, which this package does not receive
)

// StreamFeature is a bit of the feature flags in a stream header.
type StreamFeature uint64

// The stream features, DMU_BACKUP_FEATURE_*.
const (
	FeatureDedup         StreamFeature = 1 << 0
	FeatureDedupProps    StreamFeature = 1 << 1
	FeatureSASpill       StreamFeature = 1 << 2
	FeatureEmbedData     StreamFeature = 1 << 16
	FeatureLZ4           StreamFeature = 1 << 17
	FeatureLargeBlocks   StreamFeature = 1 << 19
	FeatureResuming      StreamFeature = 1 << 20
	FeatureRedacted      StreamFeature = 1 << 21
	FeatureCompressed    StreamFeature = 1 << 22
	FeatureLargeDnode    StreamFeature = 1 << 23
	FeatureRaw           StreamFeature = 1 << 24
	FeatureZstd          StreamFeature = 1 << 25
	FeatureHolds         StreamFeature = 1 << 26
	FeatureSwitchToLarge StreamFeature = 1 << 27
)

var featureNames = []struct {
	f StreamFeature
	n string
}{
	{FeatureDedup, "dedup"}, {FeatureDedupProps, "dedupprops"}, {FeatureSASpill, "sa_spill"},
	{FeatureEmbedData, "embed_data"}, {FeatureLZ4, "lz4"}, {FeatureLargeBlocks, "large_blocks"},
	{FeatureResuming, "resuming"}, {FeatureRedacted, "redacted"}, {FeatureCompressed, "compressed"},
	{FeatureLargeDnode, "large_dnode"}, {FeatureRaw, "raw"}, {FeatureZstd, "zstd"},
	{FeatureHolds, "holds"}, {FeatureSwitchToLarge, "switch_to_large_blocks"},
}

// Names returns the names of the set features.
func (f StreamFeature) Names() []string {
	var out []string
	for _, x := range featureNames {
		if f&x.f != 0 {
			out = append(out, x.n)
		}
	}
	return out
}

func (f StreamFeature) String() string { return strings.Join(f.Names(), ",") }

// StreamHeader is the decoded first record of a send stream.
type StreamHeader struct {
	// Compound is set for a replication stream (zfs send -R or -p), which
	// is a header followed by several snapshot streams. Receive does not
	// accept those.
	Compound   bool
	Features   StreamFeature
	Created    int64 // creation time of the snapshot, seconds since the epoch
	Type       Type  // KindFilesystem or KindVolume
	ToGUID     uint64
	FromGUID   uint64 // 0 for a full stream
	ToName     string // "pool/fs@snap" as on the sender
	PayloadLen uint32 // bytes that follow the record, such as the properties of a resumed stream
	Resuming   bool   // the stream continues an interrupted one
	// Clone is set for a stream that creates a clone (DRR_FLAG_CLONE): its
	// FromGUID is then the origin snapshot, not an incremental base.
	Clone bool

	swapped bool
	order   binary.ByteOrder
	raw     [ioctl.ReplayRecordSize]byte
}

// Incremental reports whether the stream is based on an earlier snapshot.
func (h *StreamHeader) Incremental() bool { return h.FromGUID != 0 }

// Raw reports whether the stream is a raw (zfs send -w) stream.
func (h *StreamHeader) Raw() bool { return h.Features&FeatureRaw != 0 }

// Snapshot is the part of ToName after the @.
func (h *StreamHeader) Snapshot() string {
	_, s, _ := strings.Cut(h.ToName, "@")
	return s
}

// Dataset is the part of ToName before the @.
func (h *StreamHeader) Dataset() string {
	d, _, _ := strings.Cut(h.ToName, "@")
	return d
}

// Bytes returns the 312 byte record as read, which is what the kernel is
// handed when receiving.
func (h *StreamHeader) Bytes() []byte { return append([]byte(nil), h.raw[:]...) }

// ErrNotAStream is returned when the data does not start with a ZFS stream
// header.
var ErrNotAStream = errors.New("zfs: not a ZFS send stream")

// ErrCompoundStream is returned for replication streams (zfs send -R, -p,
// -h), which libzfs takes apart in userland.
var ErrCompoundStream = errors.New("zfs: replication streams (send -R/-p) are not supported, only single snapshot streams")

// ParseStreamHeader decodes the first record of a send stream. The streams
// of either byte order are understood.
func ParseStreamHeader(rec []byte) (*StreamHeader, error) {
	if len(rec) < ioctl.ReplayRecordSize {
		return nil, fmt.Errorf("%w: header is %d bytes, need %d", ErrNotAStream, len(rec), ioctl.ReplayRecordSize)
	}
	var bo binary.ByteOrder = binary.NativeEndian
	swapped := false
	// drr_type (4), drr_payloadlen (4), then drr_begin.
	if binary.NativeEndian.Uint64(rec[8:]) != streamMagic {
		swapped = true
		if binary.NativeEndian.Uint64(rec[8:]) != swap64(streamMagic) {
			return nil, ErrNotAStream
		}
		if binary.LittleEndian.Uint16([]byte{1, 0}) == 1 { // native is little endian
			bo = binary.BigEndian
		} else {
			bo = binary.LittleEndian
		}
	}
	if t := bo.Uint32(rec[0:]); t != 0 { // DRR_BEGIN
		return nil, fmt.Errorf("%w: first record is type %d, not BEGIN", ErrNotAStream, t)
	}
	vi := bo.Uint64(rec[16:])
	h := &StreamHeader{
		swapped:    swapped,
		order:      bo,
		Clone:      bo.Uint32(rec[36:])&1 != 0,
		PayloadLen: bo.Uint32(rec[4:]),
		Compound:   vi&3 == hdrCompound,
		Features:   StreamFeature((vi >> 2) & (1<<30 - 1)),
		Created:    int64(bo.Uint64(rec[24:])),
		ToGUID:     bo.Uint64(rec[40:]),
		FromGUID:   bo.Uint64(rec[48:]),
	}
	switch bo.Uint32(rec[32:]) {
	case 2:
		h.Type = Filesystem
	case 3:
		h.Type = Volume
	}
	name := rec[56:312]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	h.ToName = string(name)
	h.Resuming = h.Features&FeatureResuming != 0
	copy(h.raw[:], rec)
	if vi&3 != hdrSubstream && !h.Compound {
		return nil, fmt.Errorf("%w: unknown header type %d", ErrNotAStream, vi&3)
	}
	return h, nil
}

// ReadStreamHeader reads and decodes the first record from r.
func ReadStreamHeader(r io.Reader) (*StreamHeader, error) {
	var rec [ioctl.ReplayRecordSize]byte
	if _, err := io.ReadFull(r, rec[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = fmt.Errorf("%w: truncated header", ErrNotAStream)
		}
		return nil, err
	}
	return ParseStreamHeader(rec[:])
}

func swap64(v uint64) uint64 {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return binary.LittleEndian.Uint64(b[:])
}

// ResumeToken is the decoded receive_resume_token of a dataset with an
// interrupted resumable receive.
type ResumeToken struct {
	ToName   string // the snapshot being received, "pool/fs@snap" as on the sender
	ToGUID   uint64
	FromGUID uint64 // 0 for a full stream
	Object   uint64
	Offset   uint64
	Bytes    uint64 // bytes already received

	LargeBlocks, EmbedData, Compress, Raw, Saved bool

	// Redaction snapshots of a redacted stream, as GUIDs.
	RedactSnaps []uint64

	// List is the token's whole nvlist.
	List *nvlist.List
}

// ParseResumeToken decodes a resume token as zfs send -t does: the token is
// "1-<fletcher4>-<length>-<hex of the zlib compressed packed nvlist>".
func ParseResumeToken(token string) (*ResumeToken, error) {
	bad := func(why string) error { return fmt.Errorf("zfs: resume token is corrupt (%s)", why) }
	parts := strings.SplitN(strings.TrimSpace(token), "-", 4)
	if len(parts) != 4 {
		return nil, bad("invalid format")
	}
	if parts[0] != "1" {
		return nil, bad("invalid version " + parts[0])
	}
	sum, err1 := strconv.ParseUint(parts[1], 16, 64)
	plen, err2 := strconv.ParseUint(parts[2], 16, 64)
	if err1 != nil || err2 != nil {
		return nil, bad("invalid format")
	}
	comp, err := hex.DecodeString(parts[3])
	if err != nil {
		return nil, bad("payload is not hex-encoded")
	}
	if fletcher4Word0(comp) != sum {
		return nil, bad("incorrect checksum")
	}
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, bad("decompression failed")
	}
	packed, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil || uint64(len(packed)) != plen {
		return nil, bad("decompression failed")
	}
	l, err := nvlist.Unpack(packed)
	if err != nil {
		return nil, bad("nvlist_unpack failed")
	}
	t := &ResumeToken{List: l}
	var ok1, ok2, ok3, ok4, ok5 bool
	t.ToName, ok1 = l.String("toname")
	t.Object, ok2 = l.Uint64("object")
	t.Offset, ok3 = l.Uint64("offset")
	t.Bytes, ok4 = l.Uint64("bytes")
	t.ToGUID, ok5 = l.Uint64("toguid")
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return nil, bad("required fields are missing")
	}
	t.FromGUID, _ = l.Uint64("fromguid")
	t.LargeBlocks, t.EmbedData, t.Compress = l.Has("largeblockok"), l.Has("embedok"), l.Has("compressok")
	t.Raw, t.Saved = l.Has("rawok"), l.Has("savedok")
	t.RedactSnaps, _ = l.Uint64s("redact_snaps")
	return t, nil
}

// fletcher4Word0 is the first word of fletcher_4_native_varsize: the sum of
// the 32 bit words of b. Trailing bytes that do not make a whole word are
// not summed, as in the scalar implementation.
func fletcher4Word0(b []byte) uint64 {
	var a uint64
	for i := 0; i+4 <= len(b); i += 4 {
		a += uint64(binary.NativeEndian.Uint32(b[i:]))
	}
	return a
}

// Redacted reports whether the stream is a redacted one (zfs send --redact).
func (h *StreamHeader) Redacted() bool { return h.Features&FeatureRedacted != 0 }
