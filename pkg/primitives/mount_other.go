//go:build !freebsd

package primitives

import "errors"

// osMounter is not available off FreeBSD: mounting needs nmount(2).
type osMounter struct{}

func (osMounter) nmount(string, string, []string) error { return errors.ErrUnsupported }
func (osMounter) unmount(string, bool) error            { return errors.ErrUnsupported }
func (osMounter) mounts() ([]Mount, error)              { return nil, errors.ErrUnsupported }
func (osMounter) mkdirAll(string) error                 { return errors.ErrUnsupported }
func (osMounter) isEmptyDir(string) (bool, error)       { return false, errors.ErrUnsupported }
func (osMounter) rmdir(string) error                    { return errors.ErrUnsupported }
func (osMounter) jailed() bool                          { return false }
