package zfs

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Key sizes and limits, from <sys/dsl_crypt.h> and libzfs.
const (
	wrappingKeyLen    = 32
	pbkdf2SaltLen     = 8
	DefaultIterations = 350000 // pbkdf2iters when none is given
	minIterations     = 100000
	minPassphraseLen  = 8
	maxPassphraseLen  = 512
)

// Errors from key handling.
var (
	ErrBadKey      = errors.New("invalid encryption key")
	ErrNoKeyNeeded = errors.New("the dataset has no key of its own")
)

// KeyFormat is the keyformat property.
type KeyFormat string

// The key formats.
const (
	KeyRaw        KeyFormat = "raw"        // 32 bytes
	KeyHex        KeyFormat = "hex"        // 64 hexadecimal digits
	KeyPassphrase KeyFormat = "passphrase" // 8 to 512 bytes, stretched with PBKDF2
)

// Key is the key material of an encrypted dataset. Build one with RawKey,
// HexKey or PassphraseKey.
type Key struct {
	format   KeyFormat
	material []byte
	// Iterations is the PBKDF2 cost of a passphrase key when it is created;
	// zero means DefaultIterations. It is ignored when loading a key, where
	// the dataset's own value is used.
	Iterations uint64
	// Location is the keylocation to record when the key is created, such
	// as "prompt" (the default) or "file:///path". The kernel only stores it.
	Location string
}

// RawKey is a 32 byte key used as is.
func RawKey(b []byte) *Key { return &Key{format: KeyRaw, material: append([]byte(nil), b...)} }

// HexKey is a 256 bit key written as 64 hexadecimal digits.
func HexKey(s string) *Key { return &Key{format: KeyHex, material: []byte(s)} }

// PassphraseKey is a passphrase of 8 to 512 bytes.
func PassphraseKey(s string) *Key { return &Key{format: KeyPassphrase, material: []byte(s)} }

// Format returns the key format.
func (k *Key) Format() KeyFormat { return k.format }

// wrapping returns the 32 byte wrapping key. For passphrases salt and
// iterations are those stored with (or chosen for) the dataset.
func (k *Key) wrapping(salt, iters uint64) ([]byte, error) {
	switch k.format {
	case KeyRaw:
		if len(k.material) != wrappingKeyLen {
			return nil, fmt.Errorf("zfs: %w: a raw key is %d bytes, not %d", ErrBadKey, wrappingKeyLen, len(k.material))
		}
		return append([]byte(nil), k.material...), nil
	case KeyHex:
		if len(k.material) != 2*wrappingKeyLen {
			return nil, fmt.Errorf("zfs: %w: a hex key is %d digits, not %d", ErrBadKey, 2*wrappingKeyLen, len(k.material))
		}
		b, err := hex.DecodeString(string(k.material))
		if err != nil {
			return nil, fmt.Errorf("zfs: %w: %v", ErrBadKey, err)
		}
		return b, nil
	case KeyPassphrase:
		if n := len(k.material); n < minPassphraseLen || n > maxPassphraseLen {
			return nil, fmt.Errorf("zfs: %w: a passphrase is %d to %d bytes, not %d", ErrBadKey, minPassphraseLen, maxPassphraseLen, n)
		}
		if iters < minIterations {
			return nil, fmt.Errorf("zfs: %w: pbkdf2iters %d is below %d", ErrBadKey, iters, minIterations)
		}
		var s [pbkdf2SaltLen]byte
		binary.LittleEndian.PutUint64(s[:], salt) // libzfs: LE_64(salt)
		return pbkdf2SHA1(k.material, s[:], int(iters), wrappingKeyLen), nil
	}
	return nil, fmt.Errorf("zfs: %w: unknown format %q", ErrBadKey, k.format)
}

// pbkdf2SHA1 is PBKDF2-HMAC-SHA1 (RFC 8018), which is what OpenZFS uses.
// crypto/pbkdf2 only exists since Go 1.24.
func pbkdf2SHA1(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha1.New, password)
	var out []byte
	for block := uint32(1); len(out) < keyLen; block++ {
		prf.Reset()
		prf.Write(salt)
		prf.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hiddenArgs(wkey []byte) *nvlist.List {
	h := nvlist.New()
	h.Add("wkeydata", wkey)
	return h
}

// LoadKey loads the key of an encrypted dataset, so that it can be mounted
// and read. For a passphrase the salt and iteration count the dataset
// stores are used. A wrong key fails with EACCES.
func (c *Client) LoadKey(dataset string, key *Key) error { return c.loadKey(dataset, key, false) }

// CheckKey tests a key without loading it.
func (c *Client) CheckKey(dataset string, key *Key) error { return c.loadKey(dataset, key, true) }

func (c *Client) loadKey(dataset string, key *Key, noop bool) error {
	var salt, iters uint64
	if key.format == KeyPassphrase {
		d, err := c.get(dataset, false, []string{"pbkdf2salt", "pbkdf2iters", "keyformat"})
		if err != nil {
			return err
		}
		salt, _ = d.Uint64("pbkdf2salt")
		iters, _ = d.Uint64("pbkdf2iters")
	}
	wkey, err := key.wrapping(salt, iters)
	if err != nil {
		return err
	}
	in := nvlist.New()
	in.Add("hidden_args", hiddenArgs(wkey))
	if noop {
		in.Add("noop", nvlist.Flag{})
	}
	_, err = c.c.Call(ioctl.IOCLoadKey, dataset, in)
	return err
}

// UnloadKey removes the key of an encrypted dataset from memory. The
// dataset must be unmounted.
func (c *Client) UnloadKey(dataset string) error {
	_, err := c.c.Call(ioctl.IOCUnloadKey, dataset, nil)
	return err
}

// ChangeKey re-wraps the dataset's encryption with a new key, which makes
// the dataset an encryption root. The dataset's key must be loaded.
func (c *Client) ChangeKey(dataset string, key *Key) error {
	props, wkey, err := newKeyProps(key)
	if err != nil {
		return err
	}
	in := nvlist.New()
	in.Add("crypt_cmd", uint64(dcpNewKey))
	in.Add("props", props)
	in.Add("hidden_args", hiddenArgs(wkey))
	_, err = c.c.Call(ioctl.IOCChangeKey, dataset, in)
	return err
}

// InheritKey makes the dataset use the key of its parent's encryption root
// again.
func (c *Client) InheritKey(dataset string) error {
	in := nvlist.New()
	in.Add("crypt_cmd", uint64(dcpInherit))
	_, err := c.c.Call(ioctl.IOCChangeKey, dataset, in)
	return err
}

// dcp_cmd_t from <sys/dsl_crypt.h>.
const (
	dcpNewKey  = 2
	dcpInherit = 3
)

// newKeyProps builds the properties that describe a new key (keyformat,
// keylocation and, for passphrases, a random salt and the iteration count)
// and the wrapping key itself.
func newKeyProps(key *Key) (*nvlist.List, []byte, error) {
	if key == nil {
		return nil, nil, fmt.Errorf("zfs: %w: no key", ErrBadKey)
	}
	var salt, iters uint64
	if key.format == KeyPassphrase {
		var b [pbkdf2SaltLen]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, nil, err
		}
		salt = binary.LittleEndian.Uint64(b[:])
		iters = key.Iterations
		if iters == 0 {
			iters = DefaultIterations
		}
	}
	wkey, err := key.wrapping(salt, iters)
	if err != nil {
		return nil, nil, err
	}
	fmtInfo, _ := LookupProp("keyformat")
	n, ok := fmtInfo.valueNum(string(key.format))
	if !ok {
		return nil, nil, fmt.Errorf("zfs: %w: unknown format %q", ErrBadKey, key.format)
	}
	props := nvlist.New()
	props.Add("keyformat", n)
	loc := key.Location
	if loc == "" {
		loc = "prompt"
	}
	if !strings.HasPrefix(loc, "file://") && !strings.HasPrefix(loc, "https://") && !strings.HasPrefix(loc, "http://") && loc != "prompt" {
		return nil, nil, fmt.Errorf("zfs: %w: keylocation %q must be prompt, file://... or https://...", ErrBadKey, loc)
	}
	props.Add("keylocation", loc)
	if key.format == KeyPassphrase {
		props.Add("pbkdf2iters", iters)
		props.Add("pbkdf2salt", salt)
	}
	return props, wkey, nil
}
