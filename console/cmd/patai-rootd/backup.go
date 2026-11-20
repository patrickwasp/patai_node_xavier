package main

// Backups and restores, for the console (docs/console.md, Backups). A backup is gathered here, because most of what
// makes a unit itself is root's: a copy of what it holds is made first, with the apps that keep data paused for the
// moment it takes, so it's one moment's state; then it's written into a pipe the console reads, encrypted to the
// owner's key, which the console sends on. A restore comes the other way, into folders only root can reach, and is
// put in place early in the next boot by patai-restore, before anything that uses it has started.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/durable"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
	"patai/console/internal/workloads"
)

const (
	backupOnData   = "/data/.patai-backup"
	backupInMemory = "/run/patai-backup"
	// The console's copy of its database, which it makes for each backup in its own runtime folder.
	consoleSnapshot = "/run/patai-api/console-backup.db"

	restorePersist = "/persist/.restore" // a restore's /persist parts, until the next boot puts them in place
	restoreData    = "/data/.patai-restore"
	restoreMarker  = "/persist/restore"
	restoreDone    = restorePersist + "/complete"
	// Left by patai-restore when /persist's parts are in place and the apps' folders wait for /data.
	restoreDataMarker = "/persist/restore-data"

	// How long the apps that keep data may stay paused while their folders are copied.
	pauseLimit = 10 * time.Minute
	// How long a pipe may go quiet before a backup or a restore through it is given up.
	pipeStall = 10 * time.Minute
)

// One backup or restore at a time, and never both.
var backups struct {
	sync.Mutex
	current *stream
}

type stream struct {
	id       string
	done     chan struct{}
	err      error
	manifest backup.Manifest
	sha256   string
}

// clearBackups takes away what a backup or a restore left, when this helper starts: none is running then. A
// restore waiting for the next boot is kept. Apps left paused by a helper that stopped mid-copy are let go.
func clearBackups() {
	os.RemoveAll(backupOnData)
	os.RemoveAll(backupInMemory)
	if _, err := os.Stat(restoreMarker); err != nil {
		os.RemoveAll(restorePersist)
		if _, err := os.Stat(restoreDataMarker); err != nil {
			os.RemoveAll(restoreData)
		}
	}
	if exec.Command("systemctl", "is-active", "--quiet", "docker.service").Run() == nil {
		if ids, err := containersOf(nil, "paused"); err == nil && len(ids) > 0 {
			run(time.Minute, "docker", append([]string{"unpause"}, ids...)...)
		}
	}
}

func newStream() (*stream, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	return &stream{id: hex.EncodeToString(random), done: make(chan struct{})}, nil
}

func takeTurn() (*stream, error) {
	if backups.current != nil {
		select {
		case <-backups.current.done:
		default:
			return nil, errors.New("a backup or a restore is running")
		}
	}
	for _, marker := range []string{restoreMarker, restoreDataMarker} {
		if _, err := os.Stat(marker); err == nil {
			return nil, errors.New("a restore is waiting for the unit to restart")
		}
	}
	s, err := newStream()
	if err != nil {
		return nil, err
	}
	backups.current = s
	return s, nil
}

// dataFolders are the services of this release that keep something on /data, and whether each one's files are
// written once, so a link to each will do for the copy (docs/apps.md, What an app declares).
func dataFolders() map[string]bool {
	folders := map[string]bool{}
	for _, name := range workloads.ServiceNames(workloads.Recipes) {
		m, err := workloads.ReadManifest(workloads.Recipes, name)
		if err == nil && m.Data != nil && backup.ServiceName(name) {
			folders[name] = m.Data.Backup == "link"
		}
	}
	return folders
}

func mounted(path string) bool { return exec.Command("mountpoint", "-q", path).Run() == nil }

// openBackup gathers the unit's state and answers with the read end of a pipe the backup comes out of.
func openBackup(req *rootd.Backup) (*os.File, string, error) {
	if req == nil || req.Key == nil || req.Manifest == nil {
		return nil, "", errors.New("bad backup")
	}
	if !mounted("/persist") {
		return nil, "", errors.New("/persist isn't mounted")
	}
	backups.Lock()
	defer backups.Unlock()
	s, err := takeTurn()
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (*os.File, string, error) {
		s.err = err
		close(s.done)
		return nil, "", err
	}
	staging := backupInMemory
	dataReady := mounted("/data")
	if dataReady {
		staging = backupOnData
	}
	os.RemoveAll(staging)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fail(err)
	}
	sources, services, err := snapshot(staging, dataReady)
	if err != nil {
		os.RemoveAll(staging)
		return fail(err)
	}
	m := *req.Manifest
	m.Serial = serial()
	m.Services = services
	if m.Files, m.Size, err = backup.Count(sources); err != nil {
		os.RemoveAll(staging)
		return fail(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		os.RemoveAll(staging)
		return fail(err)
	}
	s.manifest = m
	go func() {
		s.err = backup.Write(&stallWriter{write}, *req.Key, m, sources)
		write.Close()
		os.RemoveAll(staging)
		close(s.done)
	}()
	data, _ := json.Marshal(map[string]any{"id": s.id, "manifest": m})
	return read, string(data), nil
}

// stallWriter gives up on a reader that stops reading.
type stallWriter struct{ f *os.File }

func (w *stallWriter) Write(p []byte) (int, error) {
	w.f.SetWriteDeadline(time.Now().Add(pipeStall))
	return w.f.Write(p)
}

// snapshot copies what a backup holds into staging, and says what it holds: /persist's parts, and with a data disk,
// the folder of each service that keeps one, copied while the apps using them are paused.
func snapshot(staging string, data bool) ([]backup.Source, []string, error) {
	var sources []backup.Source
	part := func(name string, from string, take func(string) bool) error {
		to := filepath.Join(staging, "persist", name)
		if err := os.MkdirAll(to, 0o700); err != nil {
			return err
		}
		// The folder as it is, owner and mode, which a restore puts back: sshd reads admin's keys in provision as admin.
		if info, err := os.Lstat(from); err == nil && info.IsDir() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				if err := os.Chown(to, int(st.Uid), int(st.Gid)); err != nil {
					return err
				}
			}
			if err := os.Chmod(to, info.Mode().Perm()); err != nil {
				return err
			}
		}
		if err := copyFiles(from, to, take); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		sources = append(sources, backup.Source{Name: "persist/" + name, Dir: to})
		return nil
	}
	// The console's database is the copy the console made, not the file it's writing. The claim code isn't kept by a
	// claimed unit, and the rest is the release's or made again.
	if err := part("console", "/persist/console", func(name string) bool {
		return !strings.HasPrefix(name, "console.db") && name != "claim-code" && name != "state-format" &&
			!strings.HasSuffix(name, ".tmp") && !strings.HasPrefix(name, ".")
	}); err != nil {
		return nil, nil, err
	}
	if err := copyConsoleSnapshot(filepath.Join(staging, "persist", "console", "console.db")); err != nil {
		return nil, nil, err
	}
	if err := part("provision", "/persist/provision", func(name string) bool {
		return name != "claim-code" && name != "format-nvme" && !strings.HasSuffix(name, ".tmp")
	}); err != nil {
		return nil, nil, err
	}
	// The host keys, not the machine-id, which is this installation's.
	if err := part("identity", "/persist/identity", func(name string) bool { return strings.HasPrefix(name, "ssh_host_") }); err != nil {
		return nil, nil, err
	}
	// Whether Tailscale is on and its logs go to Tailscale; not its state, which is this node in the tailnet.
	if err := part("tailscale", tailscaleDir, func(name string) bool { return name == "on" || name == "env" }); err != nil {
		return nil, nil, err
	}

	folders := dataFolders()
	services := make([]string, 0, len(folders))
	for name := range folders {
		services = append(services, name)
	}
	slices.Sort(services)
	if !data {
		return sources, nil, nil
	}
	var present []string
	for _, name := range services {
		if info, err := os.Lstat(filepath.Join("/data", name)); err == nil && info.IsDir() {
			present = append(present, name)
		}
	}
	if err := os.MkdirAll(filepath.Join(staging, "data"), 0o700); err != nil {
		return nil, nil, err
	}
	if err := paused(present, func() error {
		for _, name := range present {
			// GNU cp 8.30 on focal: no --reflink=never, and ext4 has no reflinks anyway.
			args := []string{"-a"}
			if folders[name] {
				args = []string{"-al"}
			}
			args = append(args, "--", filepath.Join("/data", name), filepath.Join(staging, "data", name))
			if _, err := run(pauseLimit, "cp", args...); err != nil {
				return fmt.Errorf("couldn't copy %s: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	for _, name := range present {
		sources = append(sources, backup.Source{Name: "data/" + name, Dir: filepath.Join(staging, "data", name)})
	}
	// Every service's folder is named, held or not: a restore takes away one the backup didn't have.
	return sources, services, nil
}

// copyFiles copies the regular files directly in from that take accepts, as they are, owner and mode too. Nothing
// is followed: a link there is left out.
func copyFiles(from, to string, take func(string) bool) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !take(entry.Name()) {
			continue
		}
		if err := copyFile(filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name()), -1); err != nil {
			return err
		}
	}
	return nil
}

// copyFile copies a regular file without following a link, keeping its owner and mode. With owner 0 or more, the
// file has to be that user's.
func copyFile(from, to string, owner int) error {
	src, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || owner >= 0 && int(st.Uid) != owner {
		return fmt.Errorf("%s isn't a plain file of its own", from)
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(dst, src); err == nil {
		err = dst.Chown(int(st.Uid), int(st.Gid))
	}
	if err == nil {
		err = dst.Chmod(info.Mode().Perm())
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		os.Chtimes(to, info.ModTime(), info.ModTime())
	}
	return err
}

// copyConsoleSnapshot takes the console's copy of its database from its runtime folder, which the console's account
// owns: so only a plain file of that account's, never a link.
func copyConsoleSnapshot(to string) error {
	uid, _, err := apiIDs()
	if err != nil {
		return err
	}
	if err := copyFile(consoleSnapshot, to, uid); err != nil {
		return fmt.Errorf("the console's copy of its state isn't there: %w", err)
	}
	return nil
}

// paused runs work with the containers of these services paused, and lets them go whatever happens. Docker isn't
// asked anything unless it's running, as a question would start it.
func paused(services []string, work func() error) error {
	if len(services) == 0 || exec.Command("systemctl", "is-active", "--quiet", "docker.service").Run() != nil {
		return work()
	}
	ids, err := containersOf(services, "running")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return work()
	}
	if _, err := run(time.Minute, "docker", append([]string{"pause"}, ids...)...); err != nil {
		run(time.Minute, "docker", append([]string{"unpause"}, ids...)...)
		return err
	}
	start := time.Now()
	defer func() {
		if _, err := run(time.Minute, "docker", append([]string{"unpause"}, ids...)...); err != nil {
			log.Printf("backup: unpausing: %v", err)
		}
		log.Printf("backup: %d containers of %s paused for %.1f s", len(ids), strings.Join(services, ", "), time.Since(start).Seconds())
	}()
	return work()
}

// containersOf lists the containers in a state whose Compose project is one of services, or one of a service's run
// for each person, <service>_<person>_<computer>. No services means any of ours.
func containersOf(services []string, status string) ([]string, error) {
	out, err := stdout(time.Minute, "docker", "ps", "--no-trunc", "--filter", "status="+status,
		"--format", `{{.ID}} {{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, project, _ := strings.Cut(line, " ")
		if id == "" || project == "" {
			continue
		}
		base, _, _ := strings.Cut(project, "_")
		if services == nil || slices.Contains(services, base) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func serial() string {
	data, _ := os.ReadFile("/proc/device-tree/serial-number")
	return strings.TrimSpace(strings.TrimRight(string(data), "\x00"))
}

func apiIDs() (int, int, error) {
	account, err := user.Lookup(apiAccount)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	return uid, gid, err
}

// finishBackup waits for the backup being written to end, and says whether it was written whole.
func finishBackup(id string) (string, error) {
	backups.Lock()
	s := backups.current
	backups.Unlock()
	if s == nil || s.id != id {
		return "", errors.New("no such backup")
	}
	select {
	case <-s.done:
	case <-time.After(pipeStall):
		return "", errors.New("the backup didn't finish")
	}
	if s.err != nil {
		return "", s.err
	}
	data, _ := json.Marshal(s.manifest)
	return string(data), nil
}

// openRestore answers with the write end of a pipe a backup is restored from, into folders only root can reach. The
// owner's key opens it, and is forgotten when it's done. With check, nothing is kept: it only says whether the backup
// can be restored.
func openRestore(req *rootd.Backup, check bool) (*os.File, string, error) {
	if req == nil || req.Owner == "" {
		return nil, "", errors.New("bad restore")
	}
	if _, _, err := backup.Parse(req.Owner); err != nil {
		return nil, "", err
	}
	if !mounted("/persist") {
		return nil, "", errors.New("/persist isn't mounted")
	}
	backups.Lock()
	defer backups.Unlock()
	s, err := takeTurn()
	if err != nil {
		return nil, "", err
	}
	var sink backup.Sink = backup.Discard{}
	if !check {
		staged, err := newStaging()
		if err != nil {
			s.err = err
			close(s.done)
			return nil, "", err
		}
		sink = staged
	}
	read, write, err := os.Pipe()
	if err != nil {
		s.err = err
		close(s.done)
		return nil, "", err
	}
	owner := req.Owner
	go func() {
		defer close(s.done)
		defer read.Close()
		hash := sha256.New()
		m, err := backup.Read(io.TeeReader(&stallReader{read}, hash), owner, sink)
		if err == nil {
			// Anything after the backup's end isn't part of it.
			if n, _ := io.Copy(io.Discard, &stallReader{read}); n > 0 {
				err = errors.New("there's more after the backup's end")
			}
		}
		if staged, ok := sink.(*staging); ok {
			staged.close()
			if err == nil {
				err = staged.finish(m)
			}
			if err != nil {
				staged.discard()
			}
		}
		s.manifest, s.err, s.sha256 = m, err, hex.EncodeToString(hash.Sum(nil))
	}()
	return write, s.id, nil
}

type stallReader struct{ f *os.File }

func (r *stallReader) Read(p []byte) (int, error) {
	r.f.SetReadDeadline(time.Now().Add(pipeStall))
	return r.f.Read(p)
}

// finishRestore waits for a restore to have been read whole, and checks that what came is what the console sent.
func finishRestore(id, sum string) (string, error) {
	backups.Lock()
	s := backups.current
	backups.Unlock()
	if s == nil || s.id != id {
		return "", errors.New("no such restore")
	}
	select {
	case <-s.done:
	case <-time.After(pipeStall):
		return "", errors.New("the restore didn't finish")
	}
	if s.err != nil {
		return "", s.err
	}
	if sum != "" && sum != s.sha256 {
		discardRestore()
		return "", errors.New("what arrived isn't what was sent")
	}
	data, _ := json.Marshal(map[string]any{"manifest": s.manifest, "sha256": s.sha256})
	return string(data), nil
}

// staging is where a restore lands: /persist's parts in restorePersist, and the apps' folders in restoreData, each
// opened as a root that nothing in the backup can name its way out of.
type staging struct {
	persist *os.Root
	data    *os.Root
	dirs    []dirTime
}

type dirTime struct {
	root *os.Root
	name string
	at   time.Time
}

func newStaging() (*staging, error) {
	discardRestore()
	if err := os.Mkdir(restorePersist, 0o700); err != nil {
		return nil, err
	}
	persist, err := os.OpenRoot(restorePersist)
	if err != nil {
		return nil, err
	}
	return &staging{persist: persist}, nil
}

func (s *staging) Begin(m backup.Manifest) error {
	if m.StateFormat < store.FormatReadsFrom || m.StateFormat > store.FormatReadsUpTo {
		return fmt.Errorf("this release can't read the console's state in that backup (format %d)", m.StateFormat)
	}
	if !mounted("/data") {
		if len(m.Services) > 0 {
			return errors.New("the data disk isn't ready: answer it on the System page first")
		}
		return nil
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs("/data", &disk); err != nil || int64(disk.Bavail)*disk.Bsize < m.Size+(1<<30) {
		return errors.New("the data disk doesn't have room for that backup")
	}
	if err := os.Mkdir(restoreData, 0o700); err != nil {
		return err
	}
	data, err := os.OpenRoot(restoreData)
	if err != nil {
		return err
	}
	s.data = data
	return nil
}

func (s *staging) Put(e backup.Entry, modTime time.Time, content io.Reader) error {
	root, name := s.persist, strings.TrimPrefix(e.Name, "persist/")
	if strings.HasPrefix(e.Name, "data/") {
		if s.data == nil {
			return errors.New("the data disk isn't ready")
		}
		root, name = s.data, strings.TrimPrefix(e.Name, "data/")
	}
	mode := fs.FileMode(e.Mode & 0o777)
	switch e.Type {
	case "dir":
		if err := root.Mkdir(name, 0o700); err != nil {
			return err
		}
		s.dirs = append(s.dirs, dirTime{root, name, modTime})
	case "link":
		if err := root.Symlink(e.Link, name); err != nil {
			return err
		}
		return root.Lchown(name, e.UID, e.GID)
	case "file":
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, content)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := root.Chtimes(name, modTime, modTime); err != nil {
			return err
		}
	}
	if err := root.Lchown(name, e.UID, e.GID); err != nil {
		return err
	}
	// Set after the owner: a change of owner clears setuid and setgid.
	return root.Chmod(name, mode|setBits(e.Mode))
}

func setBits(mode int64) fs.FileMode {
	var m fs.FileMode
	if mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

func (s *staging) close() {
	// A folder's time is set last, as what's made in it changes it.
	for i := len(s.dirs) - 1; i >= 0; i-- {
		d := s.dirs[i]
		d.root.Chtimes(d.name, d.at, d.at)
	}
	s.persist.Close()
	if s.data != nil {
		s.data.Close()
	}
}

// finish records that the staging is whole and checked, with what it holds, durably: only then can it be put in place.
func (s *staging) finish(m backup.Manifest) error {
	exec.Command("sync").Run()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return durable.Write(restoreDone, data, 0o600)
}

func (s *staging) discard() { discardRestore() }

func discardRestore() {
	os.RemoveAll(restorePersist)
	os.RemoveAll(restoreData)
}

// applyRestore leaves what patai-restore puts in place at the next boot, and restarts the unit. Keeping its identity,
// for a unit restored while the one it came from still runs, it keeps its own name, network settings, certificate and
// host keys.
func applyRestore(keepIdentity bool) (string, error) {
	backups.Lock()
	defer backups.Unlock()
	if s := backups.current; s != nil {
		select {
		case <-s.done:
		default:
			return "", errors.New("the restore is still arriving")
		}
	}
	data, err := os.ReadFile(restoreDone)
	if err != nil {
		return "", errors.New("there's no restore ready")
	}
	var m backup.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	return whenFree(func() (string, error) {
		if halting() {
			return "", errors.New("the unit is shutting down")
		}
		staged := func(part ...string) string { return filepath.Join(append([]string{restorePersist}, part...)...) }
		os.Remove(staged("console", "claim-code"))
		os.Remove(staged("provision", "claim-code"))
		os.Remove(staged("provision", "format-nvme"))
		if keepIdentity {
			for _, name := range []string{"tls.crt", "tls.key"} {
				if err := replaceWith(filepath.Join("/persist/console", name), staged("console", name)); err != nil {
					return "", err
				}
			}
			for _, name := range []string{"hostname", "network.json"} {
				if err := replaceWith(filepath.Join("/persist/provision", name), staged("provision", name)); err != nil {
					return "", err
				}
			}
			os.RemoveAll(staged("identity"))
		}
		// Tailscale's state is this machine's node in the tailnet, never in a backup: one the unit has is kept, and
		// with none it takes the backup's settings, on and logged out.
		if _, err := os.Lstat(tailscaleDir); err == nil {
			os.RemoveAll(staged("tailscale"))
		}
		services := strings.Join(m.Services, "\n")
		if services != "" {
			services += "\n"
		}
		if err := durable.Write(staged("services"), []byte(services), 0o600); err != nil {
			return "", err
		}
		exec.Command("sync").Run()
		marker, _ := json.Marshal(map[string]any{"created": m.Created, "unit": m.Unit, "release": m.Release,
			"keep_identity": keepIdentity, "asked": time.Now().UTC().Format(time.RFC3339)})
		if err := durable.Write(restoreMarker, marker, 0o600); err != nil {
			return "", err
		}
		if _, err := reboot(); err != nil {
			os.Remove(restoreMarker)
			durable.SyncDir("/persist")
			return "", err
		}
		return "restoring the backup of " + m.Created, nil
	})
}

// replaceWith puts the unit's own file in place of the backup's, or takes the backup's away if the unit has none.
func replaceWith(own, staged string) error {
	os.Remove(staged)
	if _, err := os.Lstat(own); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return copyFile(own, staged, -1)
}
