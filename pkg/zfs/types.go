package zfs

import (
	"fmt"
	"io"

	"github.com/go-freebsd/zfs/pkg/ioctl"
	"github.com/go-freebsd/zfs/pkg/primitives"
)

// Byte sizes, for quotas, record sizes and volumes: Quota(10 * zfs.GiB).
const (
	KiB uint64 = 1 << (10 * (iota + 1))
	MiB
	GiB
	TiB
	PiB
)

// Kind is the kind of a dataset-like object.
type Kind = primitives.Type

// The kinds. They can be or-ed together where a set is wanted.
const (
	KindFilesystem = primitives.Filesystem
	KindVolume     = primitives.Volume
	KindSnapshot   = primitives.Snapshot
	KindBookmark   = primitives.Bookmark
)

// Compression is the compression property.
type Compression string

// The compression algorithms. GZIP is gzip-6; use GZIPLevel and ZSTDLevel
// for the others.
const (
	CompressionOn  Compression = "on" // the default algorithm, currently lz4
	CompressionOff Compression = "off"
	LZJB           Compression = "lzjb"
	LZ4            Compression = "lz4"
	ZLE            Compression = "zle"
	GZIP           Compression = "gzip"
	ZSTD           Compression = "zstd"
)

// GZIPLevel is gzip-1 to gzip-9 (gzip-6 is GZIP).
func GZIPLevel(n int) Compression {
	if n == 6 {
		return GZIP
	}
	return Compression(fmt.Sprintf("gzip-%d", n))
}

// ZSTDLevel is zstd-1 to zstd-19 (zstd is level 3).
func ZSTDLevel(n int) Compression { return Compression(fmt.Sprintf("zstd-%d", n)) }

// Checksum is the checksum property.
type Checksum string

// The checksum algorithms.
const (
	ChecksumOn  Checksum = "on" // the default algorithm, currently fletcher4
	ChecksumOff Checksum = "off"
	Fletcher2   Checksum = "fletcher2"
	Fletcher4   Checksum = "fletcher4"
	SHA256      Checksum = "sha256"
	SHA512      Checksum = "sha512"
	Skein       Checksum = "skein"
	Edonr       Checksum = "edonr"
	Blake3      Checksum = "blake3"
	NoParity    Checksum = "noparity"
)

// SyncMode is the sync property.
type SyncMode string

// The sync modes.
const (
	SyncStandard SyncMode = "standard"
	SyncAlways   SyncMode = "always"
	SyncDisabled SyncMode = "disabled"
)

// Algorithm is an encryption algorithm. An unencrypted dataset reports
// EncryptionOff.
type Algorithm string

// The encryption algorithms. EncryptionOn picks the default, aes-256-gcm.
const (
	EncryptionOff Algorithm = "off"
	EncryptionOn  Algorithm = "on"
	AES128CCM     Algorithm = "aes-128-ccm"
	AES192CCM     Algorithm = "aes-192-ccm"
	AES256CCM     Algorithm = "aes-256-ccm"
	AES128GCM     Algorithm = "aes-128-gcm"
	AES192GCM     Algorithm = "aes-192-gcm"
	AES256GCM     Algorithm = "aes-256-gcm"
)

// KeyStatus says whether the key of an encrypted dataset is loaded.
type KeyStatus string

// The key states.
const (
	KeyNone        KeyStatus = "none" // not encrypted
	KeyUnavailable KeyStatus = "unavailable"
	KeyAvailable   KeyStatus = "available"
)

// Key is the key material of an encrypted dataset.
type Key = primitives.Key

// RawKey is a 32 byte key used as is.
func RawKey(b []byte) *Key { return primitives.RawKey(b) }

// HexKey is a 256 bit key written as 64 hexadecimal digits.
func HexKey(s string) *Key { return primitives.HexKey(s) }

// PassphraseKey is a passphrase of 8 to 512 bytes. Set the Iterations field
// of the result to raise the cost above the default.
func PassphraseKey(s string) *Key { return primitives.PassphraseKey(s) }

// Hold is a user reference on a snapshot.
type Hold = primitives.Hold

// Space is a space accounting result, in bytes.
type Space = primitives.Space

// Received describes one stream that was received: its header, the name of
// the snapshot it made, how many bytes the kernel read, and properties of the
// stream that could not be set.
type Received = primitives.Received

// StreamHeader is the first record of a send stream.
type StreamHeader = primitives.StreamHeader

// SendProgress is the state of a running send.
type SendProgress = primitives.SendProgress

// ResumeToken is a decoded receive_resume_token.
type ResumeToken = primitives.ResumeToken

// Stream errors.
var (
	// ErrReplicationStream is no longer returned: replication streams (zfs
	// send -R, -p, -I, -h) are received.
	//
	// Deprecated: kept so that existing code compiles.
	ErrReplicationStream = primitives.ErrCompoundStream
	// ErrBadReplicationStream is returned for a replication stream whose
	// header is damaged: a payload that is not an nvlist or a checksum that
	// does not match.
	ErrBadReplicationStream = primitives.ErrBadPackage
	// ErrPartialReceive is returned, after all the data of a replication
	// stream was received, when the target could not be brought in line with
	// the sender: something to destroy, rename or promote was left.
	ErrPartialReceive = primitives.ErrPartialReceive
	// ErrIncompatibleOptions is returned, before anything is sent, for send
	// options that zfs send refuses together (a redacted or resumed
	// replication stream, a redacted raw stream).
	ErrIncompatibleOptions = primitives.ErrIncompatibleOptions
	// ErrSendIncomplete is returned, after the stream was written, when some
	// datasets or snapshots could not be sent; the stream holds the rest.
	ErrSendIncomplete = primitives.ErrSendIncomplete
	// ErrNotAStream is returned for input that is not a ZFS send stream.
	ErrNotAStream = primitives.ErrNotAStream
	// ErrNotMountable is matched by the error of mounting a dataset that has
	// no place to be mounted at: mountpoint none or legacy, canmount off, a
	// volume, a redacted or jailed dataset.
	ErrNotMountable = primitives.ErrNotMountable
	// ErrMountpointNotEmpty is returned when the directory has content and
	// overlay mounts are off.
	ErrMountpointNotEmpty = primitives.ErrMountpointNotEmpty

	// ErrStreamTruncated is the kernel's answer to a stream that ends early.
	ErrStreamTruncated = ioctl.ErrStreamTruncated
)

// ReadStreamHeader reads the first record of a send stream, to see what a
// stream is before it is received.
func ReadStreamHeader(r io.Reader) (*StreamHeader, error) { return primitives.ReadStreamHeader(r) }

// ParseResumeToken decodes a receive_resume_token.
func ParseResumeToken(token string) (*ResumeToken, error) { return primitives.ParseResumeToken(token) }

// Mount is a mounted ZFS filesystem.
type Mount = primitives.Mount
