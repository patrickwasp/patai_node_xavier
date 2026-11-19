package backup

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schedule is when backups run by themselves: each day at a time in the owner's time zone, so that it stays at night
// across a change of daylight saving.
type Schedule struct {
	On     bool   `json:"on"`
	At     string `json:"at"`   // HH:MM
	Zone   string `json:"zone"` // an IANA zone, such as Europe/Warsaw
	Days   int    `json:"keep_days"`
	Weeks  int    `json:"keep_weeks"`
	Months int    `json:"keep_months"`
}

// DefaultSchedule is a backup each night at 03:00, keeping one for each of the last 7 days, 4 weeks and 6 months: 17
// at most, so a fault noticed late can still be gone back on.
func DefaultSchedule(zone string) Schedule {
	if _, err := time.LoadLocation(zone); err != nil || zone == "" {
		zone = "UTC"
	}
	return Schedule{At: "03:00", Zone: zone, Days: 7, Weeks: 4, Months: 6}
}

func (s Schedule) Check() error {
	if _, err := s.clock(); err != nil {
		return err
	}
	if _, err := time.LoadLocation(s.Zone); err != nil || s.Zone == "" {
		return errors.New("that isn't a time zone")
	}
	if s.Days < 1 || s.Days > 90 || s.Weeks < 0 || s.Weeks > 52 || s.Months < 0 || s.Months > 120 {
		return errors.New("keep 1 to 90 days, up to 52 weeks and up to 120 months")
	}
	return nil
}

func (s Schedule) clock() (int, error) {
	t, err := time.Parse("15:04", s.At)
	if err != nil {
		return 0, errors.New("give the time as HH:MM")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Slot is the latest time a backup was due at or before now. A unit that was off then makes it when it's back.
func (s Schedule) Slot(now time.Time) time.Time {
	zone, err := time.LoadLocation(s.Zone)
	if err != nil {
		zone = time.UTC
	}
	minutes, _ := s.clock()
	local := now.In(zone)
	slot := time.Date(local.Year(), local.Month(), local.Day(), minutes/60, minutes%60, 0, 0, zone)
	if slot.After(local) {
		slot = time.Date(local.Year(), local.Month(), local.Day()-1, minutes/60, minutes%60, 0, 0, zone)
	}
	return slot.UTC()
}

// Next is when the next one is due after now.
func (s Schedule) Next(now time.Time) time.Time {
	zone, err := time.LoadLocation(s.Zone)
	if err != nil {
		zone = time.UTC
	}
	slot := s.Slot(now).In(zone)
	return time.Date(slot.Year(), slot.Month(), slot.Day()+1, slot.Hour(), slot.Minute(), 0, 0, zone).UTC()
}

// Prune says which of a series' backups the schedule no longer keeps: all but the newest of each of the last days,
// weeks and months it keeps, by the owner's zone. The newest is always kept.
func (s Schedule) Prune(created []time.Time) []time.Time {
	zone, err := time.LoadLocation(s.Zone)
	if err != nil {
		zone = time.UTC
	}
	list := append([]time.Time(nil), created...)
	sort.Slice(list, func(i, j int) bool { return list[i].After(list[j]) })
	keep := map[time.Time]bool{}
	period := func(count int, of func(time.Time) string) {
		seen := map[string]bool{}
		for _, t := range list {
			p := of(t.In(zone))
			if seen[p] {
				continue
			}
			if len(seen) == count {
				return
			}
			seen[p] = true
			keep[t] = true
		}
	}
	period(s.Days, func(t time.Time) string { return t.Format("2006-01-02") })
	period(s.Weeks, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprintf("%d-W%02d", y, w) })
	period(s.Months, func(t time.Time) string { return t.Format("2006-01") })
	if len(list) > 0 {
		keep[list[0]] = true
	}
	var gone []time.Time
	for _, t := range list {
		if !keep[t] {
			gone = append(gone, t)
		}
	}
	return gone
}

// Sidecar is what a bucket holds beside each backup, `<backup>.json`: what it is without opening it, and its hash, so
// that the unit can check the bucket still holds it as it was sent. It's sealed with the key's seal, so a bucket can't
// change what it says, and it holds nothing the bucket couldn't see anyway.
type Sidecar struct {
	Format   string `json:"format"`
	Created  string `json:"created"`
	Unit     string `json:"unit"`
	Release  string `json:"release"`
	KeyID    string `json:"key_id"`
	Series   string `json:"series"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Contents int64  `json:"contents"` // the size of the files in it
	Seal     string `json:"seal,omitempty"`
}

func (c Sidecar) signed() []byte {
	c.Seal = ""
	data, _ := json.Marshal(c)
	return data
}

// Sign seals it with a key's seal.
func (c *Sidecar) Sign(seal []byte) {
	c.Seal = hex.EncodeToString(mac(seal, c.signed()))
}

// Sealed says whether the seal is the key's.
func (c Sidecar) Sealed(seal []byte) bool {
	got, err := hex.DecodeString(c.Seal)
	return err == nil && hmac.Equal(got, mac(seal, c.signed()))
}

// Name is the backup's name in its bucket, under prefix: the unit and the time, so a listing sorts by when.
func Name(prefix, unit string, created time.Time) string {
	return prefix + unit + "-" + created.UTC().Format("20060102T150405Z") + Extension
}

// CheckPrefix is a folder in a bucket: empty, or names ending with a slash.
func CheckPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") || len(prefix) > 200 {
		return errors.New("give the folder as a name ending with /, such as patai/")
	}
	for _, c := range prefix {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return errors.New("that folder has a character it can't")
		}
	}
	return nil
}
