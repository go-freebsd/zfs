package zfs

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-freebsd/nvlist/pkg/nvlist"
	"github.com/go-freebsd/zfs/pkg/ioctl"
)

// Property errors. They are wrapped with the property name, so test them
// with errors.Is.
var (
	ErrUnknownProp     = errors.New("unknown property")
	ErrReadOnlyProp    = errors.New("property is read-only")
	ErrCreateOnly      = errors.New("property can only be set when the dataset is created")
	ErrNotInheritable  = errors.New("property cannot be inherited")
	ErrBadValue        = errors.New("invalid property value")
	ErrUnsupportedProp = errors.New("property is not supported yet")
)

const (
	maxUserPropValue = 8192 // ZAP_MAXVALUELEN
)

func propErr(prop string, err error, detail string, args ...any) error {
	if detail == "" {
		return fmt.Errorf("zfs: property %q: %w", prop, err)
	}
	return fmt.Errorf("zfs: property %q: %w: %s", prop, err, fmt.Sprintf(detail, args...))
}

// SetProp sets one property; see SetProps.
func (c *Client) SetProp(dataset, prop string, value any) error {
	return c.SetProps(dataset, map[string]any{prop: value})
}

// SetProps sets properties of a filesystem or volume (on a snapshot only
// user properties can be set). Values are given in natural Go types:
//
//	on/off properties   bool, or "on"/"off"
//	enumerations        the name ("lz4"), or the raw number as an integer
//	sizes               an integer number of bytes, or a string such as
//	                    "10G", "1.5T", "512k"; "none" for quota, refquota,
//	                    reservation and refreservation
//	numbers             an integer or a decimal string; "none" for the limits
//	strings, user props a string
//
// The values are checked and converted before anything is sent. If the
// kernel refuses some properties the error is a *SetPropsError naming each.
func (c *Client) SetProps(dataset string, props map[string]any) error {
	if len(props) == 0 {
		return nil
	}
	snapshot := strings.Contains(dataset, "@")
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)

	in := nvlist.New()
	for _, name := range names {
		v, err := convertProp(name, props[name], snapshot)
		if err != nil {
			return err
		}
		if err := in.Add(name, v); err != nil {
			return propErr(name, ErrBadValue, "%v", err)
		}
	}
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(dataset); err != nil {
		return err
	}
	if _, err := c.c.Do(ioctl.IOCSetProp, cmd, in); err != nil {
		return setPropsError(dataset, err)
	}
	return nil
}

// InheritProp removes the local value of a property so that the dataset
// takes the value of its parent again.
func (c *Client) InheritProp(dataset, prop string) error {
	info, ok := LookupProp(prop)
	switch {
	case !ok:
		return propErr(prop, ErrUnknownProp, "")
	case info.ReadOnly:
		return propErr(prop, ErrReadOnlyProp, "")
	case !info.Inherits:
		return propErr(prop, ErrNotInheritable, "")
	}
	cmd := ioctl.NewCmd()
	if err := cmd.SetName(dataset); err != nil {
		return err
	}
	if err := cmd.SetValue(prop); err != nil {
		return err
	}
	return c.c.Ioctl(ioctl.IOCInheritProp, cmd)
}

// SetPropsError reports the properties the kernel refused.
type SetPropsError struct {
	Dataset string
	// Errors maps each refused property to its errno.
	Errors map[string]error
}

func (e *SetPropsError) names() []string {
	n := make([]string, 0, len(e.Errors))
	for p := range e.Errors {
		n = append(n, p)
	}
	sort.Strings(n)
	return n
}

func (e *SetPropsError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, p := range e.names() {
		parts = append(parts, fmt.Sprintf("%s: %v", p, e.Errors[p]))
	}
	return fmt.Sprintf("zfs: cannot set properties of %s: %s", e.Dataset, strings.Join(parts, "; "))
}

// Unwrap returns the errno of every refused property, so errors.Is works.
func (e *SetPropsError) Unwrap() []error {
	errs := make([]error, 0, len(e.Errors))
	for _, p := range e.names() {
		errs = append(errs, e.Errors[p])
	}
	return errs
}

func setPropsError(dataset string, err error) error {
	var ie *ioctl.Error
	if !errors.As(err, &ie) || ie.Detail == nil {
		return err
	}
	m := map[string]error{}
	for _, p := range ie.Detail.Pairs() {
		if n, ok := p.Value.(int32); ok {
			m[p.Name] = syscall.Errno(n)
		}
	}
	if len(m) == 0 {
		return err
	}
	return &SetPropsError{Dataset: dataset, Errors: m}
}

// convertProp turns a Go value into what the kernel stores: a uint64 for
// numbers and enumerations, a string for strings.
func convertProp(name string, v any, snapshot bool) (any, error) {
	info, ok := LookupProp(name)
	if !ok {
		return nil, propErr(name, ErrUnknownProp, "not a known property or a user property (module:name)")
	}
	switch {
	case info.ReadOnly:
		return nil, propErr(name, ErrReadOnlyProp, "")
	case info.CreateOnly:
		return nil, propErr(name, ErrCreateOnly, "")
	case snapshot && info.Kind != KindUser:
		return nil, propErr(name, ErrReadOnlyProp, "only user properties can be set on a snapshot")
	case strings.Contains(name, "quota@"):
		return nil, propErr(name, ErrUnsupportedProp, "per-user, -group and -project quotas")
	}

	switch info.Kind {
	case KindUser:
		s, ok := v.(string)
		if !ok {
			return nil, propErr(name, ErrBadValue, "a user property takes a string, not %T", v)
		}
		if len(s) > maxUserPropValue {
			return nil, propErr(name, ErrBadValue, "value is %d bytes, the limit is %d", len(s), maxUserPropValue)
		}
		return s, nil

	case KindString:
		s, ok := v.(string)
		if !ok {
			return nil, propErr(name, ErrBadValue, "expected a string, not %T", v)
		}
		return s, nil

	case KindBool:
		switch x := v.(type) {
		case bool:
			if x {
				return info.Values[1].Num, nil
			}
			return info.Values[0].Num, nil
		case string:
			if n, ok := info.valueNum(x); ok {
				return n, nil
			}
		}
		return nil, propErr(name, ErrBadValue, "expected a bool or one of %s, got %v", strings.Join(info.valueNames(), ", "), v)

	case KindEnum:
		switch x := v.(type) {
		case string:
			if n, ok := info.valueNum(x); ok {
				return n, nil
			}
		default:
			if n, ok := toUint64(v); ok {
				return n, nil // the raw number, for values the table does not list
			}
		}
		return nil, propErr(name, ErrBadValue, "%v is not one of %s", v, strings.Join(info.valueNames(), ", "))

	case KindSize, KindNumber:
		if s, ok := v.(string); ok {
			if s == "none" {
				switch info.None {
				case NoneIsZero:
					return uint64(0), nil
				case NoneIsMax:
					return uint64(math.MaxUint64), nil
				}
				return nil, propErr(name, ErrBadValue, `"none" is not accepted`)
			}
			var n uint64
			var err error
			if info.Kind == KindSize {
				n, err = ParseSize(s)
			} else {
				n, err = strconv.ParseUint(s, 10, 64)
			}
			if err != nil {
				return nil, propErr(name, ErrBadValue, "%v", err)
			}
			return n, nil
		}
		if n, ok := toUint64(v); ok {
			return n, nil
		}
		return nil, propErr(name, ErrBadValue, "expected a non-negative integer or a string, not %v (%T)", v, v)
	}
	return nil, propErr(name, ErrReadOnlyProp, "")
}

// toUint64 converts any non-negative Go integer.
func toUint64(v any) (uint64, bool) {
	switch x := v.(type) {
	case uint64:
		return x, true
	case uint:
		return uint64(x), true
	case uint32:
		return uint64(x), true
	case uint16:
		return uint64(x), true
	case uint8:
		return uint64(x), true
	case int:
		return uint64(x), x >= 0
	case int64:
		return uint64(x), x >= 0
	case int32:
		return uint64(x), x >= 0
	case int16:
		return uint64(x), x >= 0
	case int8:
		return uint64(x), x >= 0
	}
	return 0, false
}

var sizeUnits = map[string]uint{
	"": 0, "B": 0,
	"K": 10, "KB": 10, "KIB": 10,
	"M": 20, "MB": 20, "MIB": 20,
	"G": 30, "GB": 30, "GIB": 30,
	"T": 40, "TB": 40, "TIB": 40,
	"P": 50, "PB": 50, "PIB": 50,
	"E": 60, "EB": 60, "EIB": 60,
}

// ParseSize parses a size the way zfs(8) does: a number, optionally with a
// decimal fraction, and an optional binary unit (B, K, M, G, T, P, E, with
// or without a trailing B or iB, in any case): "512", "64k", "1.5G". A
// fraction must give a whole number of bytes.
func ParseSize(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	i := 0
	dots := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		if t[i] == '.' {
			dots++
		}
		i++
	}
	num, unit := t[:i], strings.ToUpper(strings.TrimSpace(t[i:]))
	if num == "" || num == "." || dots > 1 || strings.HasSuffix(num, ".") || strings.HasPrefix(num, ".") {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	shift, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%q: unknown unit %q", s, t[i:])
	}
	r, ok := new(big.Rat).SetString(num)
	if !ok {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift)))
	if !r.IsInt() {
		return 0, fmt.Errorf("%q is not a whole number of bytes", s)
	}
	if !r.Num().IsUint64() {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return r.Num().Uint64(), nil
}
