// Package formats keeps code off data it can't use (docs/updates.md, Data across slots). Only the OS is A/B: what the
// apps keep on /data is shared by both slots, so a release can meet data a newer one wrote, after a rollback, a
// channel signed to go back, or a backup restored from a newer release.
//
// A service that keeps data in a format of its own, such as Chat's database schema, declares it in its manifest: the
// version its code brings the data to, and its floor, the oldest version whose code still works on that. The service
// writes what it last did to format.json in its folder. Code can use data at a newer version only if it reaches that
// data's floor. Postgres's own data is one-way by major version, and by any change such as an extension's update, so
// each such cluster has a folder of its own, and an older one stays until a later release no longer needs it.
//
// bundle/hook (through patai-formats), scripts/sign-latest, a restore and patai-workload each ask these.
package formats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Format is what a service's code says of its data: the version its code brings the data to, and the oldest version
// whose code still works on that.
type Format struct {
	Version int `json:"version"`
	Floor   int `json:"floor"`
}

// Uses says whether code of this format works on data at d.
func (f Format) Uses(d Format) bool { return d.Version <= f.Version || d.Floor <= f.Version }

// Record is what a service last said of its data, in RecordName in its folder. A version of 0 is no data yet. While
// it migrates it says the version it's moving to, so it never says less than the data holds; a failure says the
// version the data is still at, and why.
type Record struct {
	Version int    `json:"version"`
	Floor   int    `json:"floor"`
	State   string `json:"state"` // ready, migrating or failed
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Release string `json:"release,omitempty"` // the app's own version, which wrote it
	At      string `json:"at,omitempty"`
}

// RecordName is a service's record of its data, in its folder.
const RecordName = "format.json"

// Data is the record's version and floor.
func (r Record) Data() Format { return Format{Version: r.Version, Floor: r.Floor} }

// ReadRecord reads a service's record, nil if it has none. It's the service's to write, so it's read as no more than a
// small JSON file, through no link.
func ReadRecord(folder string) (*Record, error) {
	f, err := os.OpenFile(filepath.Join(folder, RecordName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a file", RecordName)
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", RecordName, err)
	}
	if r.Version < 0 || r.Floor < 0 || r.Floor > r.Version {
		return nil, fmt.Errorf("%s says version %d with floor %d", RecordName, r.Version, r.Floor)
	}
	return &r, nil
}

// Cluster names a Postgres data folder, <cluster>/docker in its service's folder: its major version, and after a
// dot a generation for a one-way change within it, such as pgvector's ALTER EXTENSION UPDATE: "18", "18.1".
type Cluster struct{ Major, Gen int }

func ParseCluster(s string) (Cluster, bool) {
	major, gen, dotted := strings.Cut(s, ".")
	m, err := strconv.Atoi(major)
	if err != nil || m < 1 || strconv.Itoa(m) != major {
		return Cluster{}, false
	}
	c := Cluster{Major: m}
	if dotted {
		g, err := strconv.Atoi(gen)
		if err != nil || g < 1 || strconv.Itoa(g) != gen {
			return Cluster{}, false
		}
		c.Gen = g
	}
	return c, true
}

func (c Cluster) String() string {
	if c.Gen == 0 {
		return strconv.Itoa(c.Major)
	}
	return fmt.Sprintf("%d.%d", c.Major, c.Gen)
}

func (c Cluster) Compare(o Cluster) int {
	if c.Major != o.Major {
		return c.Major - o.Major
	}
	return c.Gen - o.Gen
}

// Declared is what one service of a release says of the data it keeps, from its manifest.
type Declared struct {
	Title  string  `json:"title,omitempty"` // for an owner, as its manifest has it
	Format *Format `json:"format,omitempty"`
	// A Postgres service: the cluster its image runs, and the older majors it has the binaries to take one over from
	// with pg_upgrade. An older generation of its own major it takes over by copying.
	Cluster  string `json:"cluster,omitempty"`
	Upgrades []int  `json:"upgrades,omitempty"`
}

// Release is what each of a release's services declares, by name: data-formats.json in a bundle and a release's
// folder, and the data field of a channel's latest.json.
type Release map[string]Declared

// Check says what's wrong with a release's declarations, if anything.
func (r Release) Check() error {
	for name, d := range r {
		if d.Format != nil && (d.Format.Version < 1 || d.Format.Floor < 1 || d.Format.Floor > d.Format.Version) {
			return fmt.Errorf("%s: version %d with floor %d", name, d.Format.Version, d.Format.Floor)
		}
		if d.Cluster != "" {
			c, ok := ParseCluster(d.Cluster)
			if !ok {
				return fmt.Errorf("%s: cluster %q", name, d.Cluster)
			}
			for _, m := range d.Upgrades {
				if m < 1 || m >= c.Major {
					return fmt.Errorf("%s: can't upgrade from Postgres %d into %s", name, m, d.Cluster)
				}
			}
		}
	}
	return nil
}

// Clusters is what a Postgres service's folder holds: each cluster there, and the one its image last ran, which it
// writes to "current" when it starts.
type Clusters struct {
	Here    []Cluster `json:"here"`
	Current *Cluster  `json:"current,omitempty"`
}

// ReadClusters reads a Postgres service's folder. One that isn't there holds none.
func ReadClusters(folder string) (Clusters, error) {
	var c Clusters
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	for _, e := range entries {
		cluster, ok := ParseCluster(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		if info, err := os.Lstat(filepath.Join(folder, e.Name(), "docker", "PG_VERSION")); err == nil && info.Mode().IsRegular() {
			c.Here = append(c.Here, cluster)
		}
	}
	slices.SortFunc(c.Here, Cluster.Compare)
	if data, err := os.ReadFile(filepath.Join(folder, "current")); err == nil {
		if cluster, ok := ParseCluster(strings.TrimSpace(string(data))); ok && slices.Contains(c.Here, cluster) {
			c.Current = &cluster
		}
	}
	if c.Current == nil && len(c.Here) > 0 {
		newest := c.Here[len(c.Here)-1]
		c.Current = &newest
	}
	return c, nil
}

// Takes says why a Postgres service declared as d can't start on the clusters in its folder, or nil: it runs its own
// cluster if it's there, even an older one, which is how a rollback finds its own, or else takes over the one last
// run if that's older and it can.
func (d Declared) Takes(c Clusters) error {
	own, _ := ParseCluster(d.Cluster)
	if c.Current == nil || slices.Contains(c.Here, own) {
		return nil
	}
	cur := *c.Current
	switch {
	case cur.Compare(own) > 0:
		return fmt.Errorf("its database is in Postgres %s, newer than this release's %s, whose folder isn't there", cur, own)
	case cur.Major == own.Major || slices.Contains(d.Upgrades, cur.Major):
		return nil
	default:
		return fmt.Errorf("this release's Postgres %s can't take over its database from Postgres %s", own, cur)
	}
}

// Held is what the unit holds of one service's data.
type Held struct {
	Record   *Record   `json:"record,omitempty"`
	Clusters *Clusters `json:"clusters,omitempty"`
}

// Unit is what the unit holds of each service's data that some release declares.
type Unit map[string]Held

// ReadUnit reads the record or the clusters of each service that any of these releases declares, from data, /data.
func ReadUnit(data string, releases ...Release) (Unit, error) {
	unit := Unit{}
	for _, r := range releases {
		for name, d := range r {
			held := unit[name]
			folder := filepath.Join(data, name)
			if d.Format != nil && held.Record == nil {
				record, err := ReadRecord(folder)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				held.Record = record
			}
			if d.Cluster != "" && held.Clusters == nil {
				clusters, err := ReadClusters(folder)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				held.Clusters = &clusters
			}
			unit[name] = held
		}
	}
	return unit, nil
}

// Refusal is why an install, an offer, a restore or a start can't go ahead: for an owner, the service's title and the
// reason, then the versions.
type Refusal struct {
	Service string
	Reason  string
	Detail  string
}

func (r Refusal) Error() string { return r.Service + ": " + r.Reason + " (" + r.Detail + ")" }

func refuse(name string, d Declared, reason, detail string, args ...any) Refusal {
	title := d.Title
	if title == "" {
		title = name
	}
	return Refusal{Service: title, Reason: reason, Detail: fmt.Sprintf(detail, args...)}
}

// Install says why the unit shouldn't install next while it runs running: next is older than the floor of data the
// unit holds, or running couldn't use what next leaves, so a rollback would fail.
func Install(unit Unit, running, next Release) []Refusal {
	var refused []Refusal
	for _, name := range sorted(next) {
		d, held, was := next[name], unit[name], running[name]
		if d.Format != nil {
			if r := held.Record; r != nil && r.Version > 0 && !d.Format.Uses(r.Data()) {
				refused = append(refused, refuse(name, d, "this release is too old for its data on this unit",
					"its data is at version %d, which needs %d or newer; this release is %d", r.Version, r.Floor, d.Format.Version))
			}
			if was.Format != nil && !was.Format.Uses(*d.Format) {
				refused = append(refused, refuse(name, d, "the running release couldn't use its data after this one, so it couldn't roll back",
					"this release makes version %d, which needs %d or newer; the running release is %d",
					d.Format.Version, d.Format.Floor, was.Format.Version))
			}
		}
		if d.Cluster != "" && held.Clusters != nil {
			if err := d.Takes(*held.Clusters); err != nil {
				refused = append(refused, refuse(name, d, "this release's Postgres can't use its database on this unit", "%v", err))
			}
			// Its cluster stays, so a rollback finds its own; but only if it has one.
			own, _ := ParseCluster(d.Cluster)
			if was.Cluster != "" && len(held.Clusters.Here) > 0 {
				old, _ := ParseCluster(was.Cluster)
				if own.Compare(old) > 0 && !slices.Contains(held.Clusters.Here, old) {
					refused = append(refused, refuse(name, d, "the running release's Postgres would have no database to go back to",
						"this release moves it to Postgres %s, and Postgres %s has no folder of its own here", own, old))
				}
			}
		}
	}
	return refused
}

// Start says why release can't start a service on what the unit holds of its data, or nil. Only a record its service
// wrote once it was ready is taken: one that's migrating may say more than the data holds, and one that failed is from
// a start that read the data itself, as the next will, so the service is left to decide and to write it again. Taken,
// those would keep it from starting for good once the data was put right.
func Start(name string, held Held, release Release) *Refusal {
	d := release[name]
	if d.Format != nil && held.Record != nil && held.Record.State == "ready" && held.Record.Version > 0 &&
		!d.Format.Uses(held.Record.Data()) {
		r := refuse(name, d, "its data is from a newer release, which this one can't use",
			"its data is at version %d, which needs %d or newer; this release is %d", held.Record.Version, held.Record.Floor, d.Format.Version)
		return &r
	}
	if d.Cluster != "" && held.Clusters != nil {
		if err := d.Takes(*held.Clusters); err != nil {
			r := refuse(name, d, "this release's Postgres can't use its database", "%v", err)
			return &r
		}
	}
	return nil
}

// Offered is one release a channel has offered, with what it declared.
type Offered struct {
	Version string
	Data    Release
}

// Offer says why a channel that has offered these, the last of them what it offers now, shouldn't offer next: units on
// it may have data from any of them, and those on the current one install next only if they could roll back.
func Offer(next Offered, offered []Offered) []Refusal {
	var refused []Refusal
	for _, name := range sorted(next.Data) {
		d := next.Data[name]
		var newest *Format
		var newestOf string
		var cluster *Cluster
		var clusterOf string
		for _, o := range offered {
			had := o.Data[name]
			if had.Format != nil && (newest == nil || had.Format.Version > newest.Version) {
				newest, newestOf = had.Format, o.Version
			}
			if c, ok := ParseCluster(had.Cluster); ok && (cluster == nil || c.Compare(*cluster) > 0) {
				cluster, clusterOf = &c, o.Version
			}
		}
		if d.Format != nil && newest != nil && !d.Format.Uses(*newest) {
			refused = append(refused, refuse(name, d, "units on this channel may have its data from a release this one is too old for",
				"%s made version %d, which needs %d or newer; %s is %d", newestOf, newest.Version, newest.Floor, next.Version, d.Format.Version))
		}
		if len(offered) > 0 {
			current := offered[len(offered)-1]
			if was := current.Data[name].Format; d.Format != nil && was != nil && !was.Uses(*d.Format) {
				refused = append(refused, refuse(name, d, "units on the release this channel offers now couldn't roll back from this one, so they'd refuse it",
					"%s makes version %d, which needs %d or newer; %s is %d", next.Version, d.Format.Version, d.Format.Floor, current.Version, was.Version))
			}
		}
		if own, ok := ParseCluster(d.Cluster); ok && cluster != nil && own.Compare(*cluster) < 0 {
			refused = append(refused, refuse(name, d, "units on this channel may have its database in a newer Postgres than this release has",
				"%s has Postgres %s; %s has %s", clusterOf, cluster, next.Version, own))
		}
	}
	return refused
}

// Restore says why a release can't take a service's data from a backup that held it as this, or nil: data from a newer
// release than it reaches, or a dump from a newer Postgres than it runs. A dump loads into its own major or a newer one.
func Restore(name string, format *Format, cluster string, release Release) *Refusal {
	d := release[name]
	if d.Format != nil && format != nil && format.Version > 0 && !d.Format.Uses(*format) {
		r := refuse(name, d, "that backup's data is from a newer release, which this one can't use",
			"it's at version %d, which needs %d or newer; this release is %d", format.Version, format.Floor, d.Format.Version)
		return &r
	}
	if from, ok := ParseCluster(cluster); ok && d.Cluster != "" {
		own, _ := ParseCluster(d.Cluster)
		if from.Major > own.Major {
			r := refuse(name, d, "that backup's database is from a newer Postgres than this release has",
				"Postgres %d; this release has %d", from.Major, own.Major)
			return &r
		}
	}
	return nil
}

func sorted(r Release) []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
