package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"patai/console/internal/backup"
)

// A restore stages what this unit takes of a backup, where patai-restore expects it, and drops the rest.
func TestStagingTakesWhatThisUnitCan(t *testing.T) {
	for _, same := range []bool{true, false} {
		persist, data := t.TempDir(), t.TempDir()
		pr, _ := os.OpenRoot(persist)
		dr, _ := os.OpenRoot(data)
		s := &staging{persist: pr, data: dr, samePlatform: same,
			apps: map[string]string{"chat-postgres": backup.FormPostgres, "chat-server": backup.FormFiles}}
		uid, gid := os.Getuid(), os.Getgid()
		put := func(name, kind, content string) {
			t.Helper()
			e := backup.Entry{Name: name, Type: kind, Mode: 0o700, UID: uid, GID: gid, Size: int64(len(content))}
			if kind == "link" {
				e.Link, e.Mode = content, 0o777
			}
			if err := s.Put(e, time.Now(), strings.NewReader(content)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		put("unit/provision", "dir", "")
		put("unit/provision/hostname", "file", "patai-dev")
		put("platform/provision", "dir", "")
		put("platform/provision/power-mode", "file", "0")
		put("apps/chat-server/files", "dir", "")
		put("apps/chat-server/files/files", "dir", "")
		put("apps/chat-server/files/files/a.jpg", "file", "jpg")
		put("apps/chat-server/files/files/b.jpg", "link", "a.jpg")
		put("apps/chat-postgres/postgres", "dir", "")
		put("apps/chat-postgres/postgres/"+backup.DumpName, "file", "PGDMP")
		put("apps/gone/files", "dir", "")
		put("apps/gone/files/x", "file", "x")
		s.close()

		read := func(path string) string {
			data, _ := os.ReadFile(path)
			return string(data)
		}
		if read(filepath.Join(persist, "provision", "hostname")) != "patai-dev" {
			t.Error("the unit's part isn't staged")
		}
		if got := read(filepath.Join(persist, "platform", "power-mode")); (got == "0") != same {
			t.Errorf("same platform %v: the platform's file staged %q", same, got)
		}
		if read(filepath.Join(data, "chat-server", "files", "a.jpg")) != "jpg" {
			t.Error("an app's files aren't staged as its folder")
		}
		if link, err := os.Readlink(filepath.Join(data, "chat-server", "files", "b.jpg")); err != nil || link != "a.jpg" {
			t.Error("an app's link isn't staged")
		}
		if read(filepath.Join(data, "chat-postgres", backup.DumpName)) != "PGDMP" {
			t.Error("a database's dump isn't staged in its folder, for its image to load")
		}
		if _, err := os.Lstat(filepath.Join(data, "gone")); err == nil {
			t.Error("a service this release doesn't have was staged")
		}
	}
}
