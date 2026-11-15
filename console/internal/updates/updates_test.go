package updates

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckChannel(t *testing.T) {
	for raw, want := range map[string]string{
		"":                             "",
		"  ":                           "",
		"http://192.168.4.177:8088":    "http://192.168.4.177:8088",
		"http://192.168.4.177:8088/":   "http://192.168.4.177:8088",
		"https://files.example.com/a/": "https://files.example.com/a",
		" http://[fd00::1]:8088/x\n":   "http://[fd00::1]:8088/x",
	} {
		got, err := CheckChannel(raw)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"files.example.com",
		"file:///etc/shadow",
		"ftp://example.com/x",
		"http://",
		"http://user:secret@example.com/x",
		"http://example.com/latest.json?a=b",
		"http://example.com/x#y",
		"http://example.com/a b",
		"http://example.com/a\nb",
		"--output=/etc/passwd",
		"http://example.com/" + strings.Repeat("x", 600),
	} {
		if got, err := CheckChannel(raw); !errors.Is(err, ErrBad) {
			t.Errorf("%q: got %q, %v", raw, got, err)
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provision")
	want := Settings{Channel: "http://10.0.0.1:8088", ChannelName: "stable", AutoInstall: false, AutoRestart: true}
	if err := WriteSettings(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := ReadSettings(dir); got != want {
		t.Fatalf("got %+v", got)
	}
	if err := WriteSettings(dir, Settings{AutoInstall: true}); err != nil {
		t.Fatal(err)
	}
	if got := ReadSettings(dir); got != (Settings{AutoInstall: true}) {
		t.Fatalf("got %+v", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

// A unit provisioned before the console has no hold file and installs by itself, as it always has.
func TestProvisionedFilesMeanTheSame(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "update-channel"), []byte("http://a:1\nignored\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "update-channel-name"), []byte("stable\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "update-reboot"), nil, 0o644)
	if got, want := ReadSettings(dir), (Settings{Channel: "http://a:1", ChannelName: "stable", AutoInstall: true, AutoRestart: true}); got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestABadAddressChangesNothing(t *testing.T) {
	dir := t.TempDir()
	good := Settings{Channel: "http://a:1", ChannelName: "stable", AutoInstall: true}
	if err := WriteSettings(dir, good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		s    Settings
		want error
	}{
		{Settings{Channel: "nope", ChannelName: "beta", AutoRestart: true}, ErrBad},
		{Settings{Channel: "http://b:1", AutoRestart: true}, ErrBadName},
		{Settings{Channel: "http://b:1", ChannelName: "Beta", AutoRestart: true}, ErrBadName},
		{Settings{Channel: "http://b:1", ChannelName: "beta stable", AutoRestart: true}, ErrBadName},
		{Settings{Channel: "http://b:1", ChannelName: "-rf", AutoRestart: true}, ErrBadName},
	} {
		if err := WriteSettings(dir, bad.s); !errors.Is(err, bad.want) {
			t.Fatalf("%+v: %v", bad.s, err)
		}
		if got := ReadSettings(dir); got != good {
			t.Fatalf("got %+v", got)
		}
	}
	// No channel needs no name, and leaves none behind.
	if err := WriteSettings(dir, Settings{ChannelName: "stable"}); err != nil {
		t.Fatal(err)
	}
	if got := ReadSettings(dir); got != (Settings{}) {
		t.Fatalf("got %+v", got)
	}
}

// The timer mustn't find the new channel with the old, looser settings, so a failure partway stays careful.
func TestTheCarefulSettingsGoFirst(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, Settings{AutoInstall: true, AutoRestart: true}); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, "update-channel.tmp"), 0o755)
	err := WriteSettings(dir, Settings{Channel: "http://a:1", ChannelName: "stable"})
	if err == nil {
		t.Fatal("the channel can't have been written")
	}
	if got := ReadSettings(dir); got.Channel != "" || got.AutoInstall || got.AutoRestart {
		t.Fatalf("got %+v", got)
	}
	os.Remove(filepath.Join(dir, "update-channel.tmp"))
	if err := WriteSettings(dir, Settings{AutoInstall: true}); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, "update-channel.tmp"), 0o755)
	if err := WriteSettings(dir, Settings{Channel: "http://a:1", ChannelName: "stable", AutoInstall: true, AutoRestart: true}); err == nil {
		t.Fatal("the channel can't have been written")
	}
	if got := ReadSettings(dir); got.AutoRestart {
		t.Fatalf("freed before the channel was in: %+v", got)
	}
}

func TestResults(t *testing.T) {
	dir := t.TempDir()
	if check, history := ReadResults(dir, false); check != nil || history == nil || len(history) != 0 {
		t.Fatal(check, history)
	}
	os.WriteFile(filepath.Join(dir, "update-check.json"),
		[]byte(`{"time":"t","ok":true,"detail":"d","offered":"v2","last_success":"t"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "update-history.json"),
		[]byte(`[{"time":"t","version":"v2","source":"channel","state":"running","detail":""}]`), 0o644)
	check, history := ReadResults(dir, true)
	if check.Offered != "v2" || history[0].State != "running" {
		t.Fatal(check, history)
	}
	// Nothing is running, so a power cut or a kill ended it.
	if _, history := ReadResults(dir, false); history[0].State != "failed" || history[0].Detail == "" {
		t.Fatal(history)
	}
	os.WriteFile(filepath.Join(dir, "update-check.json"), []byte("not json"), 0o644)
	os.WriteFile(filepath.Join(dir, "update-history.json"), []byte("null"), 0o644)
	if check, history := ReadResults(dir, false); check != nil || history == nil {
		t.Fatal(check, history)
	}
}

func TestReadUpload(t *testing.T) {
	dir := t.TempDir()
	bundle, info := filepath.Join(dir, "b"), filepath.Join(dir, "i")
	if ReadUpload(bundle, info) != nil {
		t.Fatal("nothing there")
	}
	os.WriteFile(info, []byte(`{"version":"v1","size":3}`), 0o600)
	if ReadUpload(bundle, info) != nil {
		t.Fatal("no bundle")
	}
	os.WriteFile(bundle, []byte("abc"), 0o600)
	if got := ReadUpload(bundle, info); got == nil || got.Version != "v1" || got.Size != 3 {
		t.Fatal(got)
	}
}

func TestReadDownload(t *testing.T) {
	root := t.TempDir()
	dir, info := filepath.Join(root, "d")+"/", filepath.Join(root, "download.json")
	dirs := []string{dir}
	os.Mkdir(dir, 0o700)
	if ReadDownload(info, dirs) != nil {
		t.Fatal("nothing there")
	}
	say := func(file string, total int64) {
		data, _ := json.Marshal(map[string]any{"version": "v2", "file": file, "total": total})
		os.WriteFile(info, data, 0o600)
	}
	say(dir+"update.raucb", 4000)
	got := ReadDownload(info, dirs)
	if got == nil || got.Version != "v2" || got.Done != 0 {
		t.Fatalf("not started: %+v", got)
	}
	os.WriteFile(dir+"update.raucb", make([]byte, 1000), 0o600)
	if percent, step := ReadDownload(info, dirs).Step(); percent != 25 || step != "Downloading the release, 0.0 of 0.0 GB" {
		t.Errorf("a quarter: %d %q", percent, step)
	}
	say(dir+"update.raucb", 0)
	if percent, step := ReadDownload(info, dirs).Step(); percent != 0 || !strings.Contains(step, "so far") {
		t.Errorf("no size: %d %q", percent, step)
	}
	// Only a file in one of the two places is looked at, never a path it was given.
	for _, file := range []string{"/etc/shadow", dir + "../download.json", dir + "a/b"} {
		say(file, 10)
		if got := ReadDownload(info, dirs); got != nil {
			t.Errorf("%s: %+v", file, got)
		}
	}
	os.WriteFile(info, []byte("not json"), 0o600)
	if ReadDownload(info, dirs) != nil {
		t.Error("not json")
	}
}

// As a unit prints it: a flashed slot has no version, an installed one does.
const raucStatus = `{"compatible":"patai-node-xavier","variant":"","booted":"A","boot_primary":"rootfs.1","slots":[
{"rootfs.1":{"class":"rootfs","bootname":"B","state":"inactive","boot_status":"good","slot_status":{"bundle":{"compatible":"patai-node-xavier","version":"2026.11.02-1"},"status":"ok"}}},
{"rootfs.0":{"class":"rootfs","bootname":"A","state":"booted","boot_status":"good","slot_status":{"bundle":{"compatible":null}}}}],"artifact-repositories":[]}`

func TestParseSlots(t *testing.T) {
	slots, err := ParseSlots([]byte(raucStatus))
	if err != nil || len(slots) != 2 {
		t.Fatal(slots, err)
	}
	if s := slots[0]; s.Name != "A" || !s.Booted || s.Primary || s.Version != "" || s.State != "good" {
		t.Errorf("%+v", s)
	}
	if s := slots[1]; s.Name != "B" || s.Booted || !s.Primary || s.Version != "2026.11.02-1" {
		t.Errorf("%+v", s)
	}
}

func TestParseRauc(t *testing.T) {
	output := `{"type":"(isi)","data":[42,"Copying image to rootfs.1",2]}
{"type":"s","data":"installing"}
{"type":"s","data":"patai-node-xavier"}
`
	got, err := ParseRauc([]byte(output))
	want := Rauc{Percent: 42, Step: "Copying image to rootfs.1", Operation: "installing", Compatible: "patai-node-xavier"}
	if err != nil || got != want {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := ParseRauc([]byte("{}\n")); err == nil {
		t.Fatal("a short answer")
	}
}

func TestParseBundle(t *testing.T) {
	facts, err := ParseBundle([]byte(`{"compatible":"c","version":"2026.11.01-2","format":"verity"}`))
	if err != nil || facts.Version != "2026.11.01-2" || facts.Compatible != "c" {
		t.Fatal(facts, err)
	}
	for _, bad := range []string{`{"compatible":"c"}`, `{"version":"a b"}`, `{"version":"-rf"}`, "nonsense"} {
		if _, err := ParseBundle([]byte(bad)); err == nil {
			t.Error(bad)
		}
	}
}
