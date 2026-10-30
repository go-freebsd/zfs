package zfs

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

// Errors to test with errors.Is. The kernel's errors map onto them, so a
// failed call can be classified without looking at errno values.
var (
	ErrNotFound   = fs.ErrNotExist   // the dataset, snapshot, bookmark or hold does not exist
	ErrExists     = fs.ErrExist      // the name is taken
	ErrPermission = fs.ErrPermission // not root, or not delegated, or a read-only pool
	ErrBusy       = syscall.EBUSY    // mounted, held, cloned, or otherwise in use
	ErrNoSpace    = syscall.ENOSPC   // the pool or a quota is full

	// ErrInvalidOption is returned before anything is sent when an Option
	// does not apply to the operation, for example Parents() to Set.
	ErrInvalidOption = errors.New("zfs: option does not apply to this operation")

	// ErrKey wraps the failure of a key operation: a wrong key, a key of the
	// wrong size, or a dataset that is not encrypted.
	ErrKey = errors.New("zfs: key error")
)

// OpError is the error of a failed operation. Err is the error of the
// layer below: a *primitives.BatchError, an *ioctl.Error, or a value error
// such as primitives.ErrBadValue; errors.As reaches them.
type OpError struct {
	Op   string // "create", "snapshot", "destroy", ...
	Name string // the dataset, snapshot or bookmark
	Err  error
}

func (e *OpError) Error() string { return fmt.Sprintf("zfs: %s %s: %v", e.Op, e.Name, e.Err) }
func (e *OpError) Unwrap() error { return e.Err }

func wrap(op, name string, err error) error {
	if err == nil {
		return nil
	}
	return &OpError{Op: op, Name: name, Err: err}
}

func wrapKey(op, name string, err error) error {
	if err == nil {
		return nil
	}
	return &OpError{Op: op, Name: name, Err: fmt.Errorf("%w: %w", ErrKey, err)}
}
