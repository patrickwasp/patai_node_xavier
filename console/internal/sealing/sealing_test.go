package sealing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Name)
	k, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("the key file: %v %v", info, err)
	}
	sealed := k.Seal("backup destination", "hunter2")
	if strings.Contains(sealed, "hunter2") || !strings.HasPrefix(sealed, prefix) {
		t.Fatalf("sealed %q", sealed)
	}
	if k.Seal("backup destination", "hunter2") == sealed {
		t.Fatal("the same secret sealed the same twice")
	}
	// The same file opens it again, as after a restart.
	again, _ := Open(path)
	if got, err := again.Unseal("backup destination", sealed); err != nil || got != "hunter2" {
		t.Fatalf("unsealed %q %v", got, err)
	}
	if _, err := again.Unseal("backup ssh key", sealed); err != ErrOpen {
		t.Fatal("a secret opened for another purpose")
	}
	other, _ := Open(filepath.Join(t.TempDir(), Name))
	if _, err := other.Unseal("backup destination", sealed); err != ErrOpen {
		t.Fatal("another unit's key opened it")
	}
	for _, bad := range []string{"hunter2", prefix + "!!", prefix} {
		if _, err := k.Unseal("backup destination", bad); err != ErrOpen {
			t.Errorf("%q opened", bad)
		}
	}
	if k.Seal("x", "") != "" {
		t.Fatal("nothing was sealed as something")
	}
	os.WriteFile(filepath.Join(dir, "short"), []byte("short"), 0o600)
	if _, err := Open(filepath.Join(dir, "short")); err == nil {
		t.Fatal("a short key was taken")
	}
}
