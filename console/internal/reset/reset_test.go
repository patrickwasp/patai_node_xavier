package reset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstAnswerStands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory-reset")
	first, err := Begin(path, true)
	if err != nil || first == "" {
		t.Fatal(first, err)
	}
	second, err := Begin(path, false)
	if err != nil || second != first {
		t.Fatalf("a retry changed the code: %q %v", second, err)
	}
	data, _ := os.ReadFile(path)
	if want := "keep\n" + first + "\n"; string(data) != want {
		t.Fatalf("got %q", data)
	}
}

func TestMarkerIsForRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory-reset")
	if _, err := Begin(path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "wipe\n") {
		t.Fatalf("got %q", data)
	}
}
