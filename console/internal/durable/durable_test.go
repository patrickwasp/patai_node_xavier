package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	for _, text := range []string{"first", "second"} {
		if err := Write(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(path); string(data) != text {
			t.Fatalf("got %q", data)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left %v", entries)
	}
}

func TestAFailedWriteLeavesWhatWasThere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	os.WriteFile(path, []byte("before"), 0o644)
	os.Mkdir(path+".tmp", 0o755)
	if err := Write(path, []byte("after"), 0o644); err == nil {
		t.Fatal("it can't have written that")
	}
	if data, _ := os.ReadFile(path); string(data) != "before" {
		t.Fatalf("got %q", data)
	}
}
