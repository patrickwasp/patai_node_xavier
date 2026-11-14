package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAFormatThisReleaseCantReadIsTheReleasesFault(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = ? WHERE key = 'state_format'`, strconv.Itoa(FormatReadsUpTo+1)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(dir)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("got %v", err)
	}
	if IsStorageFault(err) {
		t.Fatal("a bad release would hide in recovery mode")
	}
}

func TestDamageIsAStorageFault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "console.db"), []byte("this isn't a database, and it's long enough to notice"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil || !IsStorageFault(err) {
		t.Fatalf("got %v", err)
	}
}

func TestAMissingDirectoryIsAStorageFault(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "gone"))
	if err == nil || !IsStorageFault(err) {
		t.Fatalf("got %v", err)
	}
}

func TestStateFormatIsWrittenForTheInstallCheck(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err := os.ReadFile(filepath.Join(dir, "state-format"))
	if err != nil || string(data) != strconv.Itoa(FormatWrites)+"\n" {
		t.Fatalf("%q, %v", data, err)
	}
}

func TestReopeningKeepsWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("ada", "hash", []string{"code"}, Audit{BootID: "b"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	owned, err := s.HasOwner()
	if err != nil || !owned {
		t.Fatalf("owner lost: %v", err)
	}
	if _, err := s.Claim("eve", "hash", nil, Audit{}); !errors.Is(err, ErrClaimed) {
		t.Fatalf("got %v", err)
	}
}

func TestAuditIsBounded(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, _ := s.db.Begin()
	if _, err := tx.Exec(`INSERT INTO audit (seq, boot_id, at, actor, origin, source, action, target, result)
		VALUES (?, 'b', 't', 'a', 'local', 's', 'x', '', 'ok')`, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = 'audit'`, auditKeep+499); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if err := s.AddAudit(Audit{BootID: "b"}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM audit`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows kept, want 1: the old row should have gone", n)
	}
}
