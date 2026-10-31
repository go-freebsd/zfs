package primitives

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Record types of dmu_replay_record_t.
const (
	drrBegin = iota
	drrObject
	drrFreeObjects
	drrWrite
	drrFree
	drrEnd
	drrWriteByref
	drrSpill
	drrWriteEmbedded
	drrObjectRange
	drrRedact
)

func p2roundup8(n uint64) uint64 { return (n + 7) &^ 7 }

// payloadSize is how many bytes follow a record of the stream, from the
// fields libzfs's recv_skip reads (offsets are those of struct
// dmu_replay_record_t: type and payloadlen, then the union at byte 8).
func payloadSize(bo binary.ByteOrder, rec []byte) (uint64, error) {
	switch t := bo.Uint32(rec[0:]); t {
	case drrBegin:
		return uint64(bo.Uint32(rec[4:])), nil
	case drrObject:
		if raw := bo.Uint32(rec[36:]); raw != 0 { // drr_raw_bonuslen
			return uint64(raw), nil
		}
		return p2roundup8(uint64(bo.Uint32(rec[28:]))), nil // drr_bonuslen
	case drrWrite:
		if rec[50] != 0 { // drr_compressiontype
			return bo.Uint64(rec[96:]), nil // drr_compressed_size
		}
		return bo.Uint64(rec[32:]), nil // drr_logical_size
	case drrSpill:
		if c := bo.Uint64(rec[40:]); c != 0 { // drr_compressed_size
			return c, nil
		}
		return bo.Uint64(rec[16:]), nil // drr_length
	case drrWriteEmbedded:
		return p2roundup8(uint64(bo.Uint32(rec[52:]))), nil // drr_psize
	case drrFreeObjects, drrFree, drrWriteByref, drrObjectRange, drrRedact, drrEnd:
		return 0, nil
	default:
		return 0, fmt.Errorf("%w: invalid record type %d", ErrNotAStream, t)
	}
}

// skipStream reads the rest of a single stream whose header record h has
// been read, up to and including its END record, and returns how many bytes
// that was. It does what libzfs does for zfs receive -n: the records are
// walked by their sizes, not checksummed. A stream that ends early gives
// ioctl.ErrStreamTruncated.
func skipStream(r io.Reader, h *StreamHeader, name string) (uint64, error) {
	truncated := func() error {
		return &ioctl.Error{Ioc: ioctl.IOCRecvNew, Name: name, Errno: ioctl.ErrStreamTruncated}
	}
	var n uint64
	skip := func(size uint64) error {
		got, err := io.CopyN(io.Discard, r, int64(size))
		n += uint64(got)
		if err == io.EOF {
			return truncated()
		}
		return err
	}
	if err := skip(uint64(h.PayloadLen)); err != nil {
		return n, err
	}
	var rec [ioctl.ReplayRecordSize]byte
	for {
		got, err := io.ReadFull(r, rec[:])
		n += uint64(got)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return n, truncated()
		}
		if err != nil {
			return n, err
		}
		size, err := payloadSize(h.order, rec[:])
		if err != nil {
			return n, err
		}
		if size > 1<<30 {
			return n, fmt.Errorf("%w: record payload of %d bytes", ErrNotAStream, size)
		}
		if err := skip(size); err != nil {
			return n, err
		}
		if h.order.Uint32(rec[0:]) == drrEnd {
			return n, nil
		}
	}
}
