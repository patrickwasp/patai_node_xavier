package formats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chat(version, floor int) Release {
	return Release{"chat-server": {Format: &Format{Version: version, Floor: floor}}}
}

func postgres(cluster string, upgrades ...int) Release {
	return Release{"chat-postgres": {Cluster: cluster, Upgrades: upgrades}}
}

func record(version, floor int) Unit {
	return Unit{"chat-server": {Record: &Record{Version: version, Floor: floor, State: "ready"}}}
}

func clusters(current string, here ...string) Unit {
	c := Clusters{}
	for _, h := range here {
		cl, _ := ParseCluster(h)
		c.Here = append(c.Here, cl)
	}
	if cl, ok := ParseCluster(current); ok {
		c.Current = &cl
	}
	return Unit{"chat-postgres": {Clusters: &c}}
}

func TestUses(t *testing.T) {
	for _, c := range []struct {
		code, data Format
		ok         bool
	}{
		{Format{3, 2}, Format{3, 2}, true},
		{Format{3, 2}, Format{2, 1}, true},  // older data: it moves it forward
		{Format{3, 2}, Format{4, 3}, true},  // newer data whose floor it reaches
		{Format{3, 2}, Format{5, 4}, false}, // newer data above it
	} {
		if got := c.code.Uses(c.data); got != c.ok {
			t.Errorf("%v uses %v: %v, want %v", c.code, c.data, got, c.ok)
		}
	}
}

func TestInstall(t *testing.T) {
	for _, c := range []struct {
		name          string
		unit          Unit
		running, next Release
		refused       string
	}{
		{"an update that only adds", record(3, 2), chat(3, 2), chat(4, 3), ""},
		{"an update whose floor the running release is below", record(3, 2), chat(3, 2), chat(4, 4), "couldn't roll back"},
		{"no data yet, and the same rule", Unit{}, chat(3, 2), chat(4, 4), "couldn't roll back"},
		{"a downgrade one step back", record(4, 3), chat(4, 3), chat(3, 2), ""},
		{"a downgrade below the data's floor", record(5, 4), chat(5, 4), chat(3, 2), "too old for its data"},
		{"a migration that failed leaves the old version", Unit{"chat-server": {Record: &Record{Version: 3, Floor: 2, State: "failed"}}}, chat(4, 3), chat(3, 2), ""},
		{"a running release that declares nothing", record(3, 2), Release{}, chat(4, 4), ""},
		{"a new major, with the old folder kept", clusters("18", "18"), postgres("18"), postgres("19", 18), ""},
		{"a new major it can't upgrade from", clusters("17", "17"), postgres("17"), postgres("19", 18), "can't take over"},
		{"back to a major whose folder is kept", clusters("19", "18", "19"), postgres("19", 18), postgres("18"), ""},
		{"back to a major whose folder is gone", clusters("19", "19"), postgres("19", 18), postgres("18"), "newer than this release's 18"},
		{"a pgvector generation", clusters("18", "18"), postgres("18"), postgres("18.1"), ""},
		{"no Postgres data yet", clusters(""), postgres("18"), postgres("19", 18), ""},
		{"a new major without the running one's folder", clusters("17", "17"), postgres("18", 17), postgres("19", 17, 18), "no folder of its own"},
	} {
		got := Install(c.unit, c.running, c.next)
		switch {
		case c.refused == "" && len(got) > 0:
			t.Errorf("%s: refused: %v", c.name, got)
		case c.refused != "" && (len(got) == 0 || !strings.Contains(got[0].Error(), c.refused)):
			t.Errorf("%s: got %v, want %q", c.name, got, c.refused)
		}
	}
}

func TestStart(t *testing.T) {
	if r := Start("chat-server", record(5, 4)["chat-server"], chat(3, 2)); r == nil || !strings.Contains(r.Error(), "newer release") {
		t.Errorf("newer data: %v", r)
	}
	if r := Start("chat-server", record(4, 3)["chat-server"], chat(3, 2)); r != nil {
		t.Errorf("data whose floor it reaches: %v", r)
	}
	if r := Start("chat-server", Held{}, chat(3, 2)); r != nil {
		t.Errorf("no record: %v", r)
	}
	// Its service reads the data itself and says again; the node doesn't hold it back for good.
	for _, state := range []string{"failed", "migrating"} {
		held := Held{Record: &Record{Version: 5, Floor: 4, State: state}}
		if r := Start("chat-server", held, chat(3, 2)); r != nil {
			t.Errorf("a %s record: %v", state, r)
		}
	}
	if r := Start("chat-postgres", clusters("19", "19")["chat-postgres"], postgres("18")); r == nil {
		t.Error("a newer major and no folder of its own: started")
	}
}

func TestOffer(t *testing.T) {
	offered := []Offered{{"a", chat(2, 1)}, {"b", chat(4, 3)}, {"c", chat(3, 2)}}
	if got := Offer(Offered{"d", chat(4, 3)}, offered); len(got) != 0 {
		t.Errorf("the next one: %v", got)
	}
	if got := Offer(Offered{"x", chat(2, 1)}, offered); len(got) == 0 || !strings.Contains(got[0].Error(), "b made version 4") {
		t.Errorf("back below what b left: %v", got)
	}
	if got := Offer(Offered{"y", chat(5, 5)}, offered); len(got) == 0 || !strings.Contains(got[0].Error(), "couldn't roll back") {
		t.Errorf("a floor above the current one: %v", got)
	}
	if got := Offer(Offered{"z", postgres("18")}, []Offered{{"p", postgres("19", 18)}}); len(got) == 0 {
		t.Error("an older major offered after a newer one")
	}
	if got := Offer(Offered{"first", chat(1, 1)}, nil); len(got) != 0 {
		t.Errorf("a channel's first: %v", got)
	}
}

func TestRestore(t *testing.T) {
	release := Release{"chat-server": {Format: &Format{3, 2}}, "chat-postgres": {Cluster: "18"}}
	if r := Restore("chat-server", &Format{5, 4}, "", release); r == nil {
		t.Error("a newer release's data restored")
	}
	if r := Restore("chat-server", &Format{4, 3}, "", release); r != nil {
		t.Errorf("data whose floor it reaches: %v", r)
	}
	if r := Restore("chat-server", nil, "", release); r != nil {
		t.Errorf("a backup that doesn't say: %v", r)
	}
	if r := Restore("chat-postgres", nil, "19", release); r == nil {
		t.Error("a dump from a newer Postgres restored")
	}
	if r := Restore("chat-postgres", nil, "17", release); r != nil {
		t.Errorf("a dump from an older Postgres: %v", r)
	}
	if r := Restore("chat-postgres", nil, "18.1", release); r != nil {
		t.Errorf("a dump from a newer generation of the same major: %v", r)
	}
}

func TestReadRecordAndClusters(t *testing.T) {
	dir := t.TempDir()
	if r, err := ReadRecord(dir); r != nil || err != nil {
		t.Fatalf("no record: %v %v", r, err)
	}
	os.WriteFile(filepath.Join(dir, RecordName), []byte(`{"version":4,"floor":3,"state":"failed","error":"x"}`), 0o644)
	r, err := ReadRecord(dir)
	if err != nil || r.Data() != (Format{4, 3}) || r.State != "failed" {
		t.Fatalf("record: %+v %v", r, err)
	}
	os.WriteFile(filepath.Join(dir, RecordName), []byte(`{"version":2,"floor":3}`), 0o644)
	if _, err := ReadRecord(dir); err == nil {
		t.Error("a floor above its version was taken")
	}
	os.Remove(filepath.Join(dir, RecordName))
	os.Symlink("/etc/passwd", filepath.Join(dir, RecordName))
	if _, err := ReadRecord(dir); err == nil {
		t.Error("a link was followed")
	}

	pg := t.TempDir()
	for _, c := range []string{"17", "18", "18.1"} {
		os.MkdirAll(filepath.Join(pg, c, "docker"), 0o700)
		os.WriteFile(filepath.Join(pg, c, "docker", "PG_VERSION"), []byte(c+"\n"), 0o600)
	}
	os.MkdirAll(filepath.Join(pg, "19", "docker"), 0o700) // no cluster in it
	os.WriteFile(filepath.Join(pg, "restore.dump"), nil, 0o600)
	c, err := ReadClusters(pg)
	if err != nil || len(c.Here) != 3 || c.Current.String() != "18.1" {
		t.Fatalf("clusters: %+v %v", c, err)
	}
	os.WriteFile(filepath.Join(pg, "current"), []byte("17\n"), 0o644)
	if c, _ := ReadClusters(pg); c.Current.String() != "17" {
		t.Errorf("current: %v", c.Current)
	}
	if c, err := ReadClusters(filepath.Join(pg, "none")); err != nil || len(c.Here) != 0 || c.Current != nil {
		t.Errorf("no folder: %+v %v", c, err)
	}
}

func TestParseCluster(t *testing.T) {
	for s, ok := range map[string]bool{"18": true, "18.1": true, "018": false, "18.0": false, "18.": false, "x": false, "": false} {
		if _, got := ParseCluster(s); got != ok {
			t.Errorf("%q: %v", s, got)
		}
	}
	a, _ := ParseCluster("18.1")
	b, _ := ParseCluster("19")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 {
		t.Error("order")
	}
}
