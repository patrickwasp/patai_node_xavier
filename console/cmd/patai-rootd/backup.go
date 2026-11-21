package main

// Backups and restores, for the console (docs/console.md, Backups). A backup is gathered here, because most of what
// makes a unit itself is root's: a copy of what it holds is made first, with the apps that keep data paused for the
// moment it takes, so it's one moment's state; then it's written into a pipe the console reads, encrypted to the
// owner's key, which the console sends on. A restore comes the other way, into folders only root can reach, and is
// put in place early in the next boot by patai-restore, before anything that uses it has started.

import (
	"bytes"
	"context"
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
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/durable"
	"patai/console/internal/formats"
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

// dataFolders are the services of this release that keep something on /data, and how each one's is backed up
// (docs/apps.md, What an app declares), with the image its recipe pins.
func dataFolders() map[string]dataFolder {
	folders := map[string]dataFolder{}
	for _, name := range workloads.ServiceNames(workloads.Recipes) {
		m, err := workloads.ReadManifest(workloads.Recipes, name)
		if err == nil && m.Data != nil && backup.ServiceName(name) {
			folders[name] = dataFolder{DataFolder: *m.Data, image: m.Image.ID}
		}
	}
	return folders
}

type dataFolder struct {
	workloads.DataFolder
	image string
}

// form is how a backup holds the folder.
func (d dataFolder) form() string {
	if d.Backup == workloads.BackupPostgres {
		return backup.FormPostgres
	}
	return backup.FormFiles
}

// platform is the kind of node this is, as its updates name it: a bundle installs only onto a system of the same.
func platform() string {
	data, _ := os.ReadFile("/etc/rauc/system.conf")
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "compatible="); ok {
			return strings.TrimSpace(value)
		}
	}
	return "unknown"
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
	sources, apps, err := snapshot(staging, dataReady)
	if err != nil {
		os.RemoveAll(staging)
		return fail(err)
	}
	m := *req.Manifest
	m.Platform, m.Arch, m.Apps = platform(), runtime.GOARCH, apps
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

// snapshot copies what a backup holds into staging, by the names it has in the backup, and says what's held of each
// service that keeps data: /persist's parts, split into the unit's and the platform's, and with a data disk, each
// service's folder, copied while the apps using them are paused, or for a database, dumped from its server then.
func snapshot(staging string, data bool) ([]backup.Source, []backup.App, error) {
	var sources []backup.Source
	part := func(name string, from string, take func(string) bool) error {
		to := filepath.Join(staging, filepath.FromSlash(name))
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
		sources = append(sources, backup.Source{Name: name, Dir: to})
		return nil
	}
	// The console's database is the copy the console made, not the file it's writing. The claim code isn't kept by a
	// claimed unit, and the rest is the release's or made again.
	if err := part("unit/console", "/persist/console", func(name string) bool {
		return !strings.HasPrefix(name, "console.db") && name != "claim-code" && name != "state-format" &&
			!strings.HasSuffix(name, ".tmp") && !strings.HasPrefix(name, ".") && !slices.Contains(unitOnly, name)
	}); err != nil {
		return nil, nil, err
	}
	if err := copyConsoleSnapshot(filepath.Join(staging, "unit", "console", "console.db")); err != nil {
		return nil, nil, err
	}
	platformFile := func(name string) bool { return slices.Contains(backup.PlatformFiles, name) }
	if err := part("unit/provision", "/persist/provision", func(name string) bool {
		return name != "claim-code" && name != "format-nvme" && !strings.HasSuffix(name, ".tmp") && !platformFile(name)
	}); err != nil {
		return nil, nil, err
	}
	if err := part("platform/provision", "/persist/provision", platformFile); err != nil {
		return nil, nil, err
	}
	// The host keys, not the machine-id, which is this installation's.
	if err := part("unit/identity", "/persist/identity", func(name string) bool { return strings.HasPrefix(name, "ssh_host_") }); err != nil {
		return nil, nil, err
	}
	// Whether Tailscale is on and its logs go to Tailscale; not its state, which is this node in the tailnet.
	if err := part("unit/tailscale", tailscaleDir, func(name string) bool { return name == "on" || name == "env" }); err != nil {
		return nil, nil, err
	}
	if !data {
		return sources, nil, nil
	}

	folders := dataFolders()
	names := make([]string, 0, len(folders))
	for name := range folders {
		names = append(names, name)
	}
	slices.Sort(names)
	// Every service's folder is named, held or not: a restore takes away one the backup didn't have.
	var apps []backup.App
	var writers, databases []string
	for _, name := range names {
		if info, err := os.Lstat(filepath.Join("/data", name)); err != nil || !info.IsDir() {
			apps = append(apps, backup.App{Service: name, Form: backup.FormNone})
			continue
		}
		app := backup.App{Service: name, Form: folders[name].form(), Cluster: folders[name].Cluster}
		// What the service last said of its data. Paused mid-migration, it says the version it was moving to.
		if folders[name].Format != nil {
			if record, err := formats.ReadRecord(filepath.Join("/data", name)); err == nil && record != nil && record.Version > 0 {
				data := record.Data()
				app.Format = &data
			}
		}
		apps = append(apps, app)
		if folders[name].form() == backup.FormPostgres {
			databases = append(databases, name)
		} else {
			writers = append(writers, name)
		}
	}
	// A database is dumped from its server, which keeps running: what writes to it is paused, so the dump and the files
	// are of one moment.
	if err := paused(writers, func() error {
		for _, name := range databases {
			if err := dumpDatabase(name, folders[name], filepath.Join(staging, "apps", name, backup.FormPostgres)); err != nil {
				return err
			}
		}
		for _, name := range writers {
			if err := os.MkdirAll(filepath.Join(staging, "apps", name), 0o700); err != nil {
				return err
			}
			if err := copyFolder(folders[name], filepath.Join("/data", name),
				filepath.Join(staging, "apps", name, backup.FormFiles)); err != nil {
				return fmt.Errorf("couldn't copy %s: %w", name, err)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	for _, app := range apps {
		if app.Form != backup.FormNone {
			sources = append(sources, backup.Source{Name: path.Join("apps", app.Service, app.Form),
				Dir: filepath.Join(staging, "apps", app.Service, app.Form)})
		}
	}
	return sources, apps, nil
}

// copyFolder copies a service's folder to to, as it is, but for its cache, which the service makes again.
func copyFolder(d dataFolder, from, to string) error {
	// GNU cp 8.30 on focal: no --reflink=never, and ext4 has no reflinks anyway.
	args := []string{"-a"}
	if d.Backup == workloads.BackupLink {
		args = []string{"-al"}
	}
	if len(d.Cache) == 0 {
		_, err := run(pauseLimit, "cp", append(args, "--", from, to)...)
		return err
	}
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no owner for %s", from)
	}
	if err := os.Mkdir(to, 0o700); err != nil {
		return err
	}
	if err := os.Lchown(to, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if err := os.Chmod(to, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if slices.Contains(d.Cache, entry.Name()) {
			continue
		}
		if _, err := run(pauseLimit, "cp", append(args, "--", filepath.Join(from, entry.Name()), to)...); err != nil {
			return err
		}
	}
	return nil
}

// dumpDatabase dumps a service's Postgres database into to, as the service's user owns its folder: from its server if
// it's running, or with Postgres run on its folder alone, with no network, while nothing else can start the service.
// The dump is Postgres's custom format, which its pg_restore loads on any processor.
func dumpDatabase(name string, d dataFolder, to string) error {
	if exec.Command("systemctl", "is-active", "--quiet", "docker.service").Run() != nil {
		return fmt.Errorf("Docker isn't running, so %s's database can't be saved", name)
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	if err := os.Chown(to, d.UID, d.UID); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(to, backup.DumpName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chown(d.UID, d.UID); err != nil {
		return err
	}
	dump := []string{"--lock-wait-timeout=60s", "--format=custom", "--username=" + d.User, "--dbname=" + d.Database}
	ids, err := containersOf([]string{name}, "running")
	if err != nil {
		return err
	}
	var args []string
	if len(ids) > 0 {
		args = append([]string{"exec", ids[0], "pg_dump"}, dump...)
	} else {
		unlock, err := lockWorkloads(2 * time.Minute)
		if err != nil {
			return fmt.Errorf("%s's database can't be saved while the apps are being changed", name)
		}
		defer unlock()
		if ids, err := containersOf([]string{name}, "running"); err != nil || len(ids) > 0 {
			return fmt.Errorf("%s started while its database was being saved", name)
		}
		script := `set -e
pg_ctl -D "$PGDATA" -o "-c listen_addresses= -c unix_socket_directories=/tmp" -l /tmp/postgres.log -w start >/dev/null
pg_dump --host=/tmp "$@"
pg_ctl -D "$PGDATA" -m fast -w stop >/dev/null`
		args = append([]string{"run", "--rm", "--network", "none", "--user", fmt.Sprintf("%d:%d", d.UID, d.UID),
			"--volume", "/data/" + name + ":/var/lib/postgresql", "--entrypoint", "sh", d.image, "-c", script, "sh"}, dump...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pauseLimit)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = f, &stderr
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return fmt.Errorf("%s's database couldn't be saved: %s", name, lines[len(lines)-1])
	}
	return f.Sync()
}

// lockWorkloads takes patai-workload's lock, which it holds while it starts and stops what runs, waiting up to limit.
func lockWorkloads(limit time.Duration) (func(), error) {
	f, err := os.OpenFile("/run/patai/workload.lock", os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(limit)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			return nil, err
		}
		time.Sleep(time.Second)
	}
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
// opened as a root that nothing in the backup can name its way out of. The platform's files wait in their own folder
// there, and only for a unit of the same platform.
type staging struct {
	persist      *os.Root
	data         *os.Root
	dirs         []dirTime
	samePlatform bool
	apps         map[string]string // the services this release has, by the form the backup holds each in
	skipped      []string
}

// stagedRestore is what a checked restore holds, which restore-apply puts in place.
type stagedRestore struct {
	Manifest backup.Manifest `json:"manifest"`
	Services []string        `json:"services"`          // the apps' folders on /data it replaces
	Skipped  []string        `json:"skipped,omitempty"` // what it holds that this unit doesn't take
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

// Begin takes what this unit can of a backup: the unit's parts whatever made it; the platform's settings only from a
// unit of the same platform, as another's mean nothing here or something else; and each service's data that this
// release has, in the form it keeps it.
func (s *staging) Begin(m backup.Manifest) error {
	if m.StateFormat < store.FormatReadsFrom || m.StateFormat > store.FormatReadsUpTo {
		return fmt.Errorf("this release can't read the console's state in that backup (format %d)", m.StateFormat)
	}
	s.samePlatform = m.Platform == platform()
	if !s.samePlatform {
		s.skipped = append(s.skipped, "the power mode, network settings and update channel, which are for "+m.Platform)
	}
	own := dataFolders()
	release, err := workloads.DataFormats(workloads.Recipes)
	if err != nil {
		return err
	}
	s.apps = map[string]string{}
	for _, app := range m.Apps {
		d, ok := own[app.Service]
		if !ok {
			s.skipped = append(s.skipped, app.Service+"'s data, as this release has no "+app.Service)
			continue
		}
		if app.Form != backup.FormNone && app.Form != d.form() {
			return fmt.Errorf("this release keeps %s's data in another form than that backup has it in", app.Service)
		}
		if app.Form != backup.FormNone {
			if r := formats.Restore(app.Service, app.Format, app.Cluster, release); r != nil {
				return r
			}
		}
		s.apps[app.Service] = app.Form
	}
	if !mounted("/data") {
		if len(s.apps) > 0 {
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

// where is where an entry of a backup is staged, if this unit takes it: unit/<part>/... in restorePersist as
// <part>/..., platform/provision/... there as platform/..., and apps/<service>/<form>/... in restoreData as
// <service>/..., which becomes /data/<service>.
func (s *staging) where(name string) (*os.Root, string, error) {
	parts := strings.SplitN(name, "/", 4)
	switch parts[0] {
	case "unit":
		return s.persist, strings.TrimPrefix(name, "unit/"), nil
	case "platform":
		if !s.samePlatform {
			return nil, "", nil
		}
		return s.persist, "platform" + strings.TrimPrefix(name, "platform/provision"), nil
	case "apps":
		if _, ok := s.apps[parts[1]]; !ok {
			return nil, "", nil
		}
		if s.data == nil {
			return nil, "", errors.New("the data disk isn't ready")
		}
		if len(parts) == 4 {
			return s.data, parts[1] + "/" + parts[3], nil
		}
		return s.data, parts[1], nil
	}
	return nil, "", fmt.Errorf("nowhere to put %s", name)
}

func (s *staging) Put(e backup.Entry, modTime time.Time, content io.Reader) error {
	root, name, err := s.where(e.Name)
	if err != nil {
		return err
	}
	if root == nil {
		_, err := io.Copy(io.Discard, content)
		return err
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
	record := stagedRestore{Manifest: m, Services: []string{}, Skipped: s.skipped}
	for name := range s.apps {
		record.Services = append(record.Services, name)
	}
	slices.Sort(record.Services)
	data, err := json.MarshalIndent(record, "", "  ")
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
	var record stagedRestore
	if err := json.Unmarshal(data, &record); err != nil {
		return "", err
	}
	m := record.Manifest
	return whenFree(func() (string, error) {
		if halting() {
			return "", errors.New("the unit is shutting down")
		}
		staged := func(part ...string) string { return filepath.Join(append([]string{restorePersist}, part...)...) }
		os.Remove(staged("console", "claim-code"))
		os.Remove(staged("provision", "claim-code"))
		os.Remove(staged("provision", "format-nvme"))
		// Where backups go and its sealed sign-in are this unit's, which it just restored from, never a backup's.
		for _, name := range unitOnly {
			if err := replaceWith(filepath.Join("/persist/console", name), staged("console", name)); err != nil {
				return "", err
			}
		}
		if keepIdentity {
			for _, name := range []string{"tls.crt", "tls.key", "node-id"} {
				if err := replaceWith(filepath.Join("/persist/console", name), staged("console", name)); err != nil {
					return "", err
				}
			}
			if err := replaceWith("/persist/provision/hostname", staged("provision", "hostname")); err != nil {
				return "", err
			}
			os.RemoveAll(staged("identity"))
		}
		// The platform's files go into provision from the backup only when it's from this platform; the unit's own are
		// kept otherwise, and its network settings are kept too when it keeps its identity.
		if err := os.MkdirAll(staged("provision"), 0o755); err != nil {
			return "", err
		}
		for _, name := range backup.PlatformFiles {
			from := staged("platform", name)
			if _, err := os.Lstat(staged("platform")); err != nil || keepIdentity && name == "network.json" {
				if err := replaceWith(filepath.Join("/persist/provision", name), staged("provision", name)); err != nil {
					return "", err
				}
				continue
			}
			os.Remove(staged("provision", name))
			if err := os.Rename(from, staged("provision", name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		}
		os.RemoveAll(staged("platform"))
		// Tailscale's state is this machine's node in the tailnet, never in a backup: one the unit has is kept, and
		// with none it takes the backup's settings, on and logged out.
		if _, err := os.Lstat(tailscaleDir); err == nil {
			os.RemoveAll(staged("tailscale"))
		}
		services := strings.Join(record.Services, "\n")
		if services != "" {
			services += "\n"
		}
		if err := durable.Write(staged("services"), []byte(services), 0o600); err != nil {
			return "", err
		}
		exec.Command("sync").Run()
		marker, _ := json.Marshal(map[string]any{"created": m.Created, "unit": m.Unit, "release": m.Release,
			"platform": m.Platform, "arch": m.Arch, "skipped": record.Skipped, "keep_identity": keepIdentity,
			"asked": time.Now().UTC().Format(time.RFC3339)})
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

// unitOnly are the console's files a backup never holds and a restore keeps the unit's own of: where backups go, with
// its secrets sealed, and the key that seals them, which never leaves the unit (internal/sealing).
var unitOnly = []string{"backup-target.json", "sealing-key"}

// replaceWith puts the unit's own file in place of the backup's, or takes the backup's away if the unit has none.
func replaceWith(own, staged string) error {
	os.Remove(staged)
	if _, err := os.Lstat(own); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return copyFile(own, staged, -1)
}
