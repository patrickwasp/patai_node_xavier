package sshkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func line(t *testing.T, comment string) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment
}

func TestAddListRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provision", "authorized_keys")
	if keys, err := List(path); err != nil || len(keys) != 0 {
		t.Fatal(keys, err)
	}
	first, err := Add(path, line(t, "ada@laptop"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Add(path, line(t, "bo@desk"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := List(path)
	if err != nil || len(keys) != 2 || keys[0].Comment != "ada@laptop" {
		t.Fatal(keys, err)
	}
	if err := Remove(path, first.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ = List(path)
	if len(keys) != 1 || keys[0].ID != second.ID {
		t.Fatal(keys)
	}
	if err := Remove(path, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAddingTwiceChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	key := line(t, "ada@laptop")
	for range 2 {
		if _, err := Add(path, key); err != nil {
			t.Fatal(err)
		}
	}
	if keys, _ := List(path); len(keys) != 1 {
		t.Fatal(keys)
	}
}

func TestProvisionedLinesSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	provisioned := "# bench\nrestrict " + line(t, "with-options") + "\n"
	if err := os.WriteFile(path, []byte(provisioned), 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := Add(path, line(t, "ada@laptop"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(path, key.ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != provisioned {
		t.Fatalf("got %q", data)
	}
}

func TestRefused(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&weak.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"empty":     "",
		"garbage":   "not a key",
		"options":   `command="/bin/sh" ` + line(t, "x"),
		"two lines": line(t, "a") + "\n" + line(t, "b"),
		"weak rsa":  string(ssh.MarshalAuthorizedKey(pub)),
	} {
		if _, _, err := Parse(bad); !errors.Is(err, ErrBad) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCommentCantBreakTheFile(t *testing.T) {
	_, stored, err := Parse(line(t, strings.Repeat("x", 300)+"\x7f"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(stored, "\n\x7f") || len(stored) > 200 {
		t.Fatalf("stored %q", stored)
	}
}
