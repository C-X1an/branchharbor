//go:build linux

package store

import (
	"os"
	"syscall"
)

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, fail("invalid", "unsafe private file")
	}
	return f, nil
}
func acquire(path string) (*os.File, error) {
	f, err := openPrivate(path, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fail("unavailable", "store already owned")
	}
	return f, nil
}
