//go:build !linux

package store

import "os"

func openPrivate(path string, flags int) (*os.File, error) {
	return nil, fail("unavailable", "only Linux local filesystems are supported")
}
func acquire(path string) (*os.File, error) {
	return nil, fail("unavailable", "only Linux local filesystems are supported")
}
