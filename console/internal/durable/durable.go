// Package durable writes files that have to survive a power cut. Writing to a temporary file and renaming
// it isn't enough: ext4 flushes the data first only when the rename replaces a file, so a new file can
// come back empty, which is how a chosen workload was lost on the bench.
package durable

import (
	"os"
	"path/filepath"
)

// Write leaves either what was there or all of data.
func Write(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir makes what has been created, renamed or removed in dir stick.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
