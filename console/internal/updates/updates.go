// Package updates is what the console knows about OS updates (docs/updates.md): the owner's settings,
// what patai-update left behind, and what RAUC says. The work itself is done by patai-update, in a
// service of its own, so it outlives a restart of the console.
package updates

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"patai/console/internal/durable"
)

const (
	Provision = "/persist/provision"
	Results   = "/persist/report"
	Tried     = "/persist/rauc/update-tried"

	// patai-api writes an upload here. It's memory, so a unit needs no NVMe for this and a power cut
	// leaves nothing behind.
	Incoming = "/run/patai-api/update.raucb"
	// patai-rootd moves it here before looking at it, out of reach of the account that wrote it.
	Bundle     = "/run/patai-rootd/update.raucb"
	BundleInfo = "/run/patai-rootd/update.json"
	// A bundle nobody installs doesn't hold 1 GB of memory for long.
	UploadLife = time.Hour
	// patai-update says here what it's downloading from a channel, to one of the two places it can.
	Download = "/run/patai/update-download.json"
)

var downloadDirs = []string{"/data/.patai-update/", "/run/patai-update/"}

// ErrBad and ErrBadName are settings the owner can correct.
var (
	ErrBad     = errors.New("that isn't a usable address")
	ErrBadName = errors.New("that isn't a usable channel name")
)

// patai-update takes the same.
var version = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+~_-]{0,63}$`)

func ValidVersion(v string) bool { return version.MatchString(v) }

// A channel's signed latest.json carries its name (docs/updates.md, Channels), so a file from another channel,
// which may name an older release, isn't taken for this one's.
var channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func ValidChannelName(s string) bool { return channelName.MatchString(s) }

// Settings are files in /persist/provision, so a provisioned unit and the console mean the same thing.
type Settings struct {
	Channel     string `json:"channel"`
	ChannelName string `json:"channel_name"` // the same channel on a mirror keeps it
	AutoInstall bool   `json:"auto_install"`
	AutoRestart bool   `json:"auto_restart"`
}

// CheckChannel gives the address to store, or says what's wrong with it. It ends up on the command lines of
// curl and RAUC as root, and patai-update puts latest.json on the end of it, so it has to be a plain
// address. A password in it would be on show to everyone who can read the unit's status.
func CheckChannel(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	if len(raw) > 512 || strings.ContainsFunc(raw, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return "", fmt.Errorf("%w: it has spaces or odd characters, or is too long", ErrBad)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", fmt.Errorf("%w: it has to start with http:// or https://", ErrBad)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: it can't hold a password", ErrBad)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: it has to be the folder that holds latest.json", ErrBad)
	}
	return raw, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	return line
}

func ReadSettings(dir string) Settings {
	return Settings{
		Channel:     firstLine(filepath.Join(dir, "update-channel")),
		ChannelName: firstLine(filepath.Join(dir, "update-channel-name")),
		AutoInstall: !exists(filepath.Join(dir, "update-hold")),
		AutoRestart: exists(filepath.Join(dir, "update-reboot")),
	}
}

func flag(path string, on bool) error {
	if on {
		return os.WriteFile(path, nil, 0o644)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WriteSettings stores them, or changes nothing if the address or name won't do. The timer can run at any
// moment and a power cut can come at any moment, so what limits the unit goes first and what frees it goes
// last. Halfway through, it's only ever more careful than either the old settings or the new: an address and a
// name that don't belong together get nothing, because the channel's file carries its name.
func WriteSettings(dir string, s Settings) error {
	channel, err := CheckChannel(s.Channel)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(s.ChannelName)
	if channel != "" && name == "" {
		return fmt.Errorf("%w: the channel needs one, which comes with its address", ErrBadName)
	}
	if channel != "" && !ValidChannelName(name) {
		return fmt.Errorf("%w: it can only have lower-case letters, digits and dashes", ErrBadName)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	hold, reboot := filepath.Join(dir, "update-hold"), filepath.Join(dir, "update-reboot")
	if !s.AutoInstall {
		if err := flag(hold, true); err != nil {
			return err
		}
	}
	if !s.AutoRestart {
		if err := flag(reboot, false); err != nil {
			return err
		}
	}
	path, namePath := filepath.Join(dir, "update-channel"), filepath.Join(dir, "update-channel-name")
	if channel == "" {
		if err := flag(path, false); err != nil {
			return err
		}
		if err := flag(namePath, false); err != nil {
			return err
		}
	} else {
		if err := durable.Write(namePath, []byte(name+"\n"), 0o644); err != nil {
			return err
		}
		if err := durable.Write(path, []byte(channel+"\n"), 0o644); err != nil {
			return err
		}
	}
	if s.AutoRestart {
		if err := flag(reboot, true); err != nil {
			return err
		}
	}
	if s.AutoInstall {
		if err := flag(hold, false); err != nil {
			return err
		}
	}
	return durable.SyncDir(dir)
}

// Check is the last look at the channel. Offered is a release other than the running one. Channel and Sequence
// are from a file signed by the fleet, so they're missing when the check found none.
type Check struct {
	Time        string `json:"time"`
	OK          bool   `json:"ok"`
	Detail      string `json:"detail"`
	Offered     string `json:"offered,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	Channel     string `json:"channel,omitempty"`
	Sequence    int64  `json:"sequence,omitempty"` // Unix seconds when the channel's file was signed
}

// Install is one attempt, newest first in the history.
type Install struct {
	Job     int64  `json:"job,omitempty"` // the console job that asked; none for the timer
	Time    string `json:"time"`
	Version string `json:"version"`
	Source  string `json:"source"` // automatic, channel or file
	State   string `json:"state"`  // running, ok or failed
	Detail  string `json:"detail"`
}

type Slot struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Version string `json:"version"` // what RAUC installed there; a flash isn't recorded
	Booted  bool   `json:"booted"`
	Primary bool   `json:"primary"`
}

// Upload is a file that has been verified and is waiting for a person.
type Upload struct {
	Version string `json:"version"`
	Size    int64  `json:"size"`
}

type Status struct {
	Settings
	Slots []Slot `json:"slots"`
	Check *Check `json:"check"`
	Tried string `json:"tried"` // installed once, so the timer leaves it alone

	Busy       bool   `json:"busy"`       // patai-update is running, for whatever reason
	Installing bool   `json:"installing"` // and it's writing a slot
	Percent    int    `json:"percent"`
	Step       string `json:"step"`

	History []Install `json:"history"`
	Upload  *Upload   `json:"upload"`
}

// ReadResults gets what patai-update left in dir. An install it was running when something stopped it
// says so here, because nothing else will until the next one.
func ReadResults(dir string, busy bool) (*Check, []Install) {
	var check *Check
	if data, err := os.ReadFile(filepath.Join(dir, "update-check.json")); err == nil {
		check = &Check{}
		if json.Unmarshal(data, check) != nil {
			check = nil
		}
	}
	history := []Install{}
	if data, err := os.ReadFile(filepath.Join(dir, "update-history.json")); err == nil {
		if json.Unmarshal(data, &history) != nil || history == nil {
			history = []Install{}
		}
	}
	if !busy {
		for i := range history {
			if history[i].State == "running" {
				history[i].State, history[i].Detail = "failed", "it was interrupted"
			}
		}
	}
	return check, history
}

// Downloading is how far patai-update has got with a bundle from a channel. Done is measured from the file, not
// reported by it, and Total is 0 for a channel file signed before sizes were.
type Downloading struct {
	Version string `json:"version"`
	File    string `json:"file"`
	Total   int64  `json:"total"`
	Done    int64  `json:"-"`
}

// ReadDownload is the download going on, if there is one, from what patai-update left in path.
func ReadDownload(path string, dirs []string) *Downloading {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var d Downloading
	if json.Unmarshal(data, &d) != nil || d.Total < 0 {
		return nil
	}
	if dirs == nil {
		dirs = downloadDirs
	}
	for _, dir := range dirs {
		if strings.HasPrefix(d.File, dir) && !strings.Contains(d.File[len(dir):], "/") {
			if info, err := os.Lstat(d.File); err == nil && info.Mode().IsRegular() {
				d.Done = info.Size()
			}
			return &d
		}
	}
	return nil
}

// Step says how far a download has got, for the console's progress bar and the line under it.
func (d *Downloading) Step() (int, string) {
	if d.Total <= 0 {
		return 0, fmt.Sprintf("Downloading the release, %.1f GB so far", float64(d.Done)/1e9)
	}
	return int(min(d.Done*100/d.Total, 100)), fmt.Sprintf("Downloading the release, %.1f of %.1f GB",
		float64(d.Done)/1e9, float64(d.Total)/1e9)
}

// ReadUpload is the bundle waiting for a person, if there is one.
func ReadUpload(bundle, info string) *Upload {
	if !exists(bundle) {
		return nil
	}
	data, err := os.ReadFile(info)
	if err != nil {
		return nil
	}
	var upload Upload
	if json.Unmarshal(data, &upload) != nil || !ValidVersion(upload.Version) {
		return nil
	}
	return &upload
}

// ParseSlots reads `rauc status --detailed --output-format=json`.
func ParseSlots(output []byte) ([]Slot, error) {
	var status struct {
		Booted  string `json:"booted"`
		Primary string `json:"boot_primary"`
		Slots   []map[string]struct {
			Bootname   string `json:"bootname"`
			BootStatus string `json:"boot_status"`
			SlotStatus struct {
				Bundle struct {
					Version string `json:"version"`
				} `json:"bundle"`
			} `json:"slot_status"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return nil, err
	}
	slots := []Slot{}
	for _, entry := range status.Slots {
		for name, slot := range entry {
			slots = append(slots, Slot{Name: slot.Bootname, State: slot.BootStatus, Version: slot.SlotStatus.Bundle.Version,
				Booted: slot.Bootname == status.Booted, Primary: name == status.Primary})
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	return slots, nil
}

// Rauc is what its Installer says over D-Bus, from `busctl --json=short get-property` for Progress,
// Operation and Compatible: one JSON document a line.
type Rauc struct {
	Percent    int
	Step       string
	Operation  string
	Compatible string
}

func ParseRauc(output []byte) (Rauc, error) {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != 3 {
		return Rauc{}, fmt.Errorf("expected 3 properties, got %d", len(lines))
	}
	var progress struct {
		Data []any `json:"data"`
	}
	var operation, compatible struct {
		Data string `json:"data"`
	}
	for i, target := range []any{&progress, &operation, &compatible} {
		if err := json.Unmarshal([]byte(lines[i]), target); err != nil {
			return Rauc{}, err
		}
	}
	r := Rauc{Operation: operation.Data, Compatible: compatible.Data}
	if len(progress.Data) >= 2 {
		if percent, ok := progress.Data[0].(float64); ok {
			r.Percent = int(percent)
		}
		r.Step, _ = progress.Data[1].(string)
	}
	return r, nil
}

// BundleFacts are what `rauc info --output-format=json` says of a bundle whose signature it accepted.
type BundleFacts struct {
	Compatible string `json:"compatible"`
	Version    string `json:"version"`
}

func ParseBundle(output []byte) (BundleFacts, error) {
	var facts BundleFacts
	if err := json.Unmarshal(output, &facts); err != nil {
		return facts, err
	}
	if !ValidVersion(facts.Version) {
		return facts, fmt.Errorf("bad version %q", facts.Version)
	}
	return facts, nil
}
