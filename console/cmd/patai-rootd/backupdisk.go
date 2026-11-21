package main

// Backup disks (docs/console.md, Backups, Where they go): a disk plugged into the unit, such as a USB drive, that
// backups are kept on. It's known by its filesystem's ID, so which port it's in and what Linux calls it don't matter.
// Only root mounts it, here, when the console needs it, and lets it go a little after: the console reads and writes
// its files through ones this opens and hands over, under the disk's folder of backups and nowhere else, as a disk
// that's been elsewhere can hold links to anywhere.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/rootd"
)

const (
	diskMount = "/run/patai-backup-disk"
	// DiskFolder is the folder on a backup disk that backups go in.
	diskFolder = "patai-backups"
	// How long a backup disk stays mounted after the console last used it.
	diskIdle = 30 * time.Second
	// The label a disk formatted for backups has.
	diskLabel = "patai-backup"
)

// The filesystems a backup disk can have: ext4, as this formats one, and exFAT, which most drives come with. NTFS is
// read-only here, and FAT32 can't hold a file over 4 GiB.
var diskTypes = []string{"ext4", "exfat"}

var fsID = regexp.MustCompile(`^[0-9A-Fa-f-]{4,36}$`)

var disks struct {
	sync.Mutex
	id     string // the filesystem mounted, if one is
	busy   int    // calls using it now
	idle   *time.Timer
	writes map[string]diskWrite
}

type diskWrite struct {
	id, part, name string
}

// lsblk's columns, as util-linux 2.34 (focal) gives them in JSON: every value a string or null, a size in bytes.
type blockDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Type       string        `json:"type"`
	Size       flexible      `json:"size"`
	FSType     *string       `json:"fstype"`
	UUID       *string       `json:"uuid"`
	Label      *string       `json:"label"`
	Mountpoint *string       `json:"mountpoint"`
	Tran       *string       `json:"tran"`
	Model      *string       `json:"model"`
	Children   []blockDevice `json:"children"`
}

// flexible is a number lsblk gives as a string in some versions and a number in others.
type flexible string

func (f *flexible) UnmarshalJSON(b []byte) error {
	*f = flexible(strings.Trim(string(b), `"`))
	return nil
}

func text(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// listDisks is each disk backups could go on, which is any the unit isn't using: not its own, not the data disk,
// nothing mounted. Each filesystem on one is listed, and a disk with none.
func listDisks() ([]rootd.BackupDisk, error) {
	out, err := stdout(30*time.Second, "lsblk", "--json", "--bytes", "--output",
		"NAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,MOUNTPOINT,TRAN,MODEL")
	if err != nil {
		return nil, err
	}
	var tree struct {
		Devices []blockDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, err
	}
	accepted, _ := os.ReadFile("/persist/data-disk")
	dataDisk, _ := stdout(10*time.Second, "/usr/local/sbin/patai-data-disk", "device")
	list := []rootd.BackupDisk{}
	for _, d := range tree.Devices {
		size, _ := strconv.ParseInt(string(d.Size), 10, 64)
		if d.Type != "disk" || size < 1<<30 || d.Path == strings.TrimSpace(string(dataDisk)) ||
			strings.HasPrefix(d.Name, "mmcblk") || strings.HasPrefix(d.Name, "zram") || strings.HasPrefix(d.Name, "loop") {
			continue
		}
		parts := append([]blockDevice{d}, d.Children...)
		inUse := false
		for _, p := range parts {
			if text(p.Mountpoint) != "" && !strings.HasPrefix(text(p.Mountpoint), diskMount) || text(p.Label) == "data" ||
				text(p.UUID) != "" && text(p.UUID) == strings.TrimSpace(string(accepted)) {
				inUse = true
			}
		}
		if inUse {
			continue
		}
		disk := rootd.BackupDisk{Device: d.Name, Model: text(d.Model), Transport: text(d.Tran), Size: size}
		for _, p := range parts {
			kind := text(p.FSType)
			if kind == "" || text(p.UUID) == "" {
				continue
			}
			partSize, _ := strconv.ParseInt(string(p.Size), 10, 64)
			f := rootd.BackupFilesystem{ID: text(p.UUID), Label: text(p.Label), Type: kind, Size: partSize}
			switch {
			case slices.Contains(diskTypes, kind):
				f.Usable = true
			case kind == "vfat":
				f.Why = "FAT32 can't hold a backup over 4 GiB"
			case kind == "ntfs":
				f.Why = "an NTFS disk can only be read here"
			default:
				f.Why = "it isn't ext4 or exFAT"
			}
			disk.Filesystems = append(disk.Filesystems, f)
		}
		list = append(list, disk)
	}
	return list, nil
}

// findDisk is a usable filesystem on a disk listDisks gives, and the partition it's on.
func findDisk(id string) (rootd.BackupFilesystem, string, error) {
	if !fsID.MatchString(id) {
		return rootd.BackupFilesystem{}, "", errors.New("no such disk")
	}
	list, err := listDisks()
	if err != nil {
		return rootd.BackupFilesystem{}, "", err
	}
	for _, d := range list {
		for _, f := range d.Filesystems {
			if f.ID == id && f.Usable {
				out, err := stdout(10*time.Second, "blkid", "--uuid", id)
				if err != nil {
					return f, "", errors.New("that disk isn't plugged in")
				}
				return f, strings.TrimSpace(string(out)), nil
			}
		}
	}
	return rootd.BackupFilesystem{}, "", errors.New("that disk isn't plugged in, or can't be used for backups")
}

// useDisk mounts a backup disk if it isn't, and opens its folder of backups, made if it isn't there. done lets it go.
func useDisk(id string) (*os.Root, func(), error) {
	disks.Lock()
	defer disks.Unlock()
	if disks.idle != nil {
		disks.idle.Stop()
	}
	if disks.id != "" && disks.id != id {
		if disks.busy > 0 || len(disks.writes) > 0 {
			return nil, nil, errors.New("another backup disk is in use")
		}
		if err := unmountDisk(); err != nil {
			return nil, nil, err
		}
	}
	if disks.id == "" {
		f, device, err := findDisk(id)
		if err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(diskMount, 0o700); err != nil {
			return nil, nil, err
		}
		if _, err := run(time.Minute, "mount", "-t", f.Type, "-o", "nosuid,nodev,noexec,noatime", device, diskMount); err != nil {
			return nil, nil, errors.New("the disk couldn't be mounted: it may need checking on a computer")
		}
		disks.id = id
	}
	top, err := os.OpenRoot(diskMount)
	if err == nil {
		err = top.MkdirAll(diskFolder, 0o755)
	}
	var folder *os.Root
	if err == nil {
		folder, err = top.OpenRoot(diskFolder)
	}
	if top != nil {
		top.Close()
	}
	if err != nil {
		idleLater()
		return nil, nil, fmt.Errorf("the disk's %s folder can't be used: %w", diskFolder, err)
	}
	disks.busy++
	return folder, func() {
		folder.Close()
		disks.Lock()
		defer disks.Unlock()
		disks.busy--
		idleLater()
	}, nil
}

// idleLater unmounts the disk once nothing has used it for a while. A file the console still has open keeps it busy,
// so it's tried again then. Called with disks held.
func idleLater() {
	if disks.idle != nil {
		disks.idle.Stop()
	}
	disks.idle = time.AfterFunc(diskIdle, func() {
		disks.Lock()
		defer disks.Unlock()
		if disks.busy > 0 || len(disks.writes) > 0 {
			return
		}
		if err := unmountDisk(); err != nil {
			idleLater()
		}
	})
}

func init() {
	disks.writes = map[string]diskWrite{}
}

// unmountDisk lets the mounted backup disk go, all written first. Called with disks held.
func unmountDisk() error {
	if disks.id == "" {
		return nil
	}
	exec.Command("sync", "-f", diskMount).Run()
	if _, err := run(time.Minute, "umount", diskMount); err != nil {
		return errors.New("the backup disk is still in use")
	}
	log.Printf("backup disk %s unmounted", disks.id)
	disks.id = ""
	return nil
}

// diskFile is a file in a backup disk's folder, by a name the console can give.
func diskFile(req *rootd.Disk) error {
	if req == nil || !backup.FileName(req.Name) {
		return errors.New("no such file")
	}
	return nil
}

func listDiskFiles(req *rootd.Disk) (string, error) {
	if req == nil {
		return "", errors.New("no such disk")
	}
	folder, done, err := useDisk(req.ID)
	if err != nil {
		return "", err
	}
	defer done()
	dir, err := folder.Open(".")
	if err != nil {
		return "", err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return "", err
	}
	files := []rootd.DiskFile{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, rootd.DiskFile{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	data, _ := json.Marshal(files)
	return string(data), nil
}

// openDiskFile opens a file on a backup disk for the console to read, and says how big it is.
func openDiskFile(req *rootd.Disk) (*os.File, string, error) {
	if err := diskFile(req); err != nil {
		return nil, "", err
	}
	folder, done, err := useDisk(req.ID)
	if err != nil {
		return nil, "", err
	}
	defer done()
	f, err := folder.OpenFile(req.Name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, "", errors.New("that file isn't on the disk")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, "", errors.New("that isn't a file")
	}
	return f, strconv.FormatInt(info.Size(), 10), nil
}

// createDiskFile opens a new file on a backup disk for the console to write, by its part's name until it's committed.
func createDiskFile(req *rootd.Disk) (*os.File, string, error) {
	if err := diskFile(req); err != nil {
		return nil, "", err
	}
	folder, done, err := useDisk(req.ID)
	if err != nil {
		return nil, "", err
	}
	defer done()
	random := make([]byte, 16)
	rand.Read(random)
	w := diskWrite{id: hex.EncodeToString(random), part: backup.PartName(req.Name), name: req.Name}
	folder.Remove(w.part)
	f, err := folder.OpenFile(w.part, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, "", err
	}
	disks.Lock()
	disks.writes[w.id] = w
	disks.busy++
	disks.Unlock()
	return f, w.id, nil
}

// finishDiskFile ends a write: committed, it's flushed to the disk and takes its name; otherwise it's removed.
func finishDiskFile(id string, commit bool) (string, error) {
	disks.Lock()
	w, ok := disks.writes[id]
	delete(disks.writes, id)
	diskID := disks.id
	disks.Unlock()
	if !ok {
		return "", errors.New("no such write")
	}
	folder, done, err := useDisk(diskID)
	disks.Lock()
	disks.busy--
	disks.Unlock()
	if err != nil {
		return "", err
	}
	defer done()
	if !commit {
		folder.Remove(w.part)
		return "", nil
	}
	f, err := folder.OpenFile(w.part, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		err = f.Sync()
		f.Close()
	}
	if err == nil {
		err = folder.Rename(w.part, w.name)
	}
	if err == nil {
		var dir *os.File
		if dir, err = folder.Open("."); err == nil {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		folder.Remove(w.part)
		return "", fmt.Errorf("the disk didn't take it: %w", err)
	}
	return "", nil
}

func deleteDiskFile(req *rootd.Disk) (string, error) {
	if err := diskFile(req); err != nil {
		return "", err
	}
	folder, done, err := useDisk(req.ID)
	if err != nil {
		return "", err
	}
	defer done()
	if err := folder.Remove(req.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return "", nil
}

// formatDisk erases a disk the listing offers and makes one ext4 filesystem on it for backups, and says its ID.
func formatDisk(req *rootd.Disk) (string, error) {
	if req == nil || req.Device == "" {
		return "", errors.New("no such disk")
	}
	list, err := listDisks()
	if err != nil {
		return "", err
	}
	var device string
	for _, d := range list {
		if d.Device == req.Device {
			device = "/dev/" + d.Device
		}
	}
	if device == "" {
		return "", errors.New("that disk isn't plugged in, or can't be used for backups")
	}
	disks.Lock()
	if disks.id != "" && (disks.busy > 0 || len(disks.writes) > 0) {
		disks.Unlock()
		return "", errors.New("a backup disk is in use")
	}
	err = unmountDisk()
	disks.Unlock()
	if err != nil {
		return "", err
	}
	if _, err := run(time.Minute, "wipefs", "--all", device); err != nil {
		return "", err
	}
	// Named fields and the Linux-filesystem GUID: util-linux 2.34 rejects the short form with name= (patai-data-disk).
	table := "label: gpt\ntype=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=" + diskLabel + "\n"
	if _, err := runInput(time.Minute, table, "sfdisk", "--quiet", device); err != nil {
		return "", err
	}
	run(time.Minute, "udevadm", "settle")
	out, err := stdout(10*time.Second, "lsblk", "--list", "--noheadings", "--paths", "--output", "NAME", device)
	lines := strings.Fields(string(out))
	if err != nil || len(lines) < 2 {
		return "", errors.New("the disk's new partition didn't appear")
	}
	partition := lines[1]
	if _, err := run(30*time.Minute, "mkfs.ext4", "-q", "-F", "-L", diskLabel, "-m", "0", partition); err != nil {
		return "", err
	}
	run(time.Minute, "udevadm", "settle")
	id, err := stdout(10*time.Second, "blkid", "--probe", "--output", "value", "--match-tag", "UUID", partition)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(id)), nil
}

// letDisksGo unmounts a backup disk left mounted by a helper that stopped.
func letDisksGo() {
	if mounted(diskMount) {
		run(time.Minute, "umount", "--lazy", diskMount)
	}
}
