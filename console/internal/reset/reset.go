// Package reset is the marker that asks for a factory reset. All the erasing happens early in the next
// boot, in overlay/usr/local/sbin/patai-factory-reset, which reads what this writes.
package reset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"patai/console/internal/auth"
)

const Marker = "/persist/factory-reset"

// Begin records the reset and returns the claim code for the unit's next owner. A second call changes
// nothing and gets the same code, so a retry can't lose it or turn a keep into a wipe.
func Begin(path string, keepData bool) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if lines := strings.Split(string(data), "\n"); len(lines) > 1 && lines[1] != "" {
			return lines[1], nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	mode := "wipe"
	if keepData {
		mode = "keep"
	}
	code := auth.NewCode()
	return code, writeSync(path, mode+"\n"+code+"\n")
}

func writeSync(path, content string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(content); err == nil {
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
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
