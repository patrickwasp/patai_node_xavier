package api

// Backups (docs/console.md, Backups): where they go, the owner's key, when they run, and restoring one. The console
// keeps what it needs in backupPath, readable by it alone: where backups go and how it signs in there, the schedule,
// and the public half of the owner's backup key with its seal. It never keeps the owner's key itself.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/durable"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

// The jobs a backup, a check and a restore are, run beside the queue (jobs.Runner.Aside).
const (
	jobBackup  = "backup"
	jobCheck   = "backup-check"
	jobRestore = "restore"
)

type backupConfig struct {
	Series      string          `json:"series"`
	Key         *backup.Key     `json:"key,omitempty"`
	Destination *destination    `json:"destination,omitempty"`
	SSHKey      *sshKey         `json:"ssh_key,omitempty"`
	Schedule    backup.Schedule `json:"schedule"`
	Since       string          `json:"since,omitempty"`     // when the schedule was last saved: no slot before it is run
	LastSlot    string          `json:"last_slot,omitempty"` // the last slot a scheduled backup was made for
}

type backupState struct {
	mu       sync.Mutex
	read     bool
	cfg      backupConfig
	pending  *pendingKey
	secrets  map[int64]jobSecret
	progress *backupProgress
	sidecars map[string]backup.Sidecar // by name, as read from where backups go: a backup never changes
}

// pendingKey is a key made and shown, waiting for the owner to give it back, which shows they kept it.
type pendingKey struct {
	secret string
	key    backup.Key
	until  time.Time
}

// jobSecret is the owner's key for a check or a restore, held in memory for its job and never written anywhere.
type jobSecret struct {
	owner        string
	keepIdentity bool
}

type backupProgress struct {
	Job   int64  `json:"job"`
	Phase string `json:"phase"` // gathering, sending, fetching, checking or restoring
	Bytes int64  `json:"bytes"`
	Size  int64  `json:"size,omitempty"`
}

func (s *Server) backupNow() backupConfig {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	return s.backupLocked()
}

func (s *Server) backupLocked() backupConfig {
	if !s.backups.read && s.BackupPath != "" {
		if data, err := os.ReadFile(s.BackupPath); err == nil {
			if err := json.Unmarshal(data, &s.backups.cfg); err != nil {
				log.Printf("backups: %v", err)
			}
		}
		s.backups.read = true
	}
	cfg := s.backups.cfg
	if cfg.Schedule.Zone == "" {
		cfg.Schedule = backup.DefaultSchedule("UTC")
	}
	return cfg
}

// changeBackups changes what's kept, under the lock.
func (s *Server) changeBackups(change func(*backupConfig) error) (backupConfig, error) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	cfg := s.backupLocked()
	if cfg.Series == "" {
		random := make([]byte, 8)
		rand.Read(random)
		cfg.Series = hex.EncodeToString(random)
	}
	if err := change(&cfg); err != nil {
		return backupConfig{}, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return backupConfig{}, err
	}
	if err := durable.Write(s.BackupPath, data, 0o600); err != nil {
		return backupConfig{}, err
	}
	s.backups.cfg = cfg
	return cfg, nil
}

func (s *Server) setProgress(job int64, phase string, bytes, size int64) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	s.backups.progress = &backupProgress{Job: job, Phase: phase, Bytes: bytes, Size: size}
}

func (s *Server) clearProgress() {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	s.backups.progress = nil
}

func (s *Server) backupsUsable(w http.ResponseWriter) bool {
	if s.BackupPath == "" {
		fail(w, http.StatusConflict, "recovery", "the unit can't keep this change in recovery mode")
		return false
	}
	return true
}

// getBackups is everything the Backups page shows but the list, which comes from where backups go.
func (s *Server) getBackups(w http.ResponseWriter, r *http.Request, p *Principal) {
	cfg := s.backupNow()
	s.backups.mu.Lock()
	progress := s.backups.progress
	pending := s.backups.pending != nil && time.Now().Before(s.backups.pending.until)
	s.backups.mu.Unlock()
	out := map[string]any{"key": nil, "destination": nil, "schedule": cfg.Schedule, "next": nil, "running": progress,
		"pending_key": pending, "last": nil, "last_good": nil, "restored": s.restored()}
	if cfg.Key != nil {
		out["key"] = map[string]string{"id": cfg.Key.ID, "made": cfg.Key.Made}
	}
	if d := cfg.Destination; d != nil {
		out["destination"], out["where"] = d.shown(), d.about()
	}
	if cfg.SSHKey != nil {
		out["ssh_key"] = cfg.SSHKey.Public
	}
	if cfg.Schedule.On {
		out["next"] = cfg.Schedule.Next(time.Now()).Format(time.RFC3339)
	}
	if jobs, err := s.Store.Jobs(50); err == nil {
		for _, j := range jobs {
			if j.Kind != jobBackup {
				continue
			}
			if out["last"] == nil && (j.State == store.JobSucceeded || j.State == store.JobFailed) {
				out["last"] = j
			}
			if j.State == store.JobSucceeded {
				out["last_good"] = j.UpdatedAt
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// restored is what the last restore onto this unit said, which patai-restore leaves on /persist.
func (s *Server) restored() any {
	if s.RestoreReport == "" {
		return nil
	}
	data, err := os.ReadFile(s.RestoreReport)
	if err != nil {
		return nil
	}
	var report any
	if json.Unmarshal(data, &report) != nil {
		return nil
	}
	return report
}

type listedBackup struct {
	Name     string `json:"name"`
	Created  string `json:"created"`
	Unit     string `json:"unit,omitempty"`
	Release  string `json:"release,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	Size     int64  `json:"size"`
	Contents int64  `json:"contents,omitempty"`
	ThisUnit bool   `json:"this_unit"` // of this unit's series, which the schedule prunes
	ThisKey  bool   `json:"this_key"`  // made with the key the unit has now
	Record   bool   `json:"record"`    // its record beside it is there, and sealed if it's this key's
}

// listBackups is what's where backups go, newest first.
func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, p *Principal) {
	cfg := s.backupNow()
	if cfg.Destination == nil {
		fail(w, http.StatusConflict, "no-destination", "choose where backups go first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	list, err := s.backupList(ctx, cfg)
	if err != nil {
		fail(w, http.StatusBadGateway, "destination", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

func (s *Server) backupList(ctx context.Context, cfg backupConfig) ([]listedBackup, error) {
	target, err := s.target(cfg)
	if err != nil {
		return nil, err
	}
	objects, err := target.List(ctx)
	if err != nil {
		return nil, err
	}
	records := map[string]bool{}
	for _, o := range objects {
		if strings.HasSuffix(o.Name, backup.Extension+".json") && backup.FileName(o.Name) {
			records[strings.TrimSuffix(o.Name, ".json")] = true
		}
	}
	var seal []byte
	if cfg.Key != nil {
		seal, _ = cfg.Key.SealKey()
	}
	list := []listedBackup{}
	for _, o := range objects {
		if !backupName(o.Name) {
			continue
		}
		b := listedBackup{Name: o.Name, Created: o.Modified.UTC().Format(time.RFC3339), Size: o.Size}
		if records[o.Name] {
			if c, err := s.sidecar(ctx, target, o.Name); err == nil && c.Size == o.Size {
				b.Created, b.Unit, b.Release, b.KeyID, b.Contents = c.Created, c.Unit, c.Release, c.KeyID, c.Contents
				b.ThisUnit = c.Series == cfg.Series
				b.ThisKey = cfg.Key != nil && c.KeyID == cfg.Key.ID
				b.Record = !b.ThisKey || c.Sealed(seal)
			}
		}
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created > list[j].Created })
	return list, nil
}

func (s *Server) sidecar(ctx context.Context, target backup.Target, name string) (backup.Sidecar, error) {
	s.backups.mu.Lock()
	c, ok := s.backups.sidecars[name]
	s.backups.mu.Unlock()
	if ok {
		return c, nil
	}
	data, err := backup.ReadFile(ctx, target, name+".json", 64<<10)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil || c.Format != backup.Format {
		return c, errors.New("its record can't be read")
	}
	s.backups.mu.Lock()
	if s.backups.sidecars == nil {
		s.backups.sidecars = map[string]backup.Sidecar{}
	}
	s.backups.sidecars[name] = c
	s.backups.mu.Unlock()
	return c, nil
}

// putDestination sets where backups go, once it has taken a test file and given it back. A secret is kept unless a
// new one is given, for the same account. An SFTP server's key has to be confirmed: without one, the answer is the
// key it has, for the owner to check.
func (s *Server) putDestination(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		destination
		CurrentPassword string `json:"current_password"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	d := body.destination
	d.tidy()
	if err := d.check(); err != nil {
		fail(w, http.StatusBadRequest, "bad-destination", err.Error())
		return
	}
	cfg := s.backupNow()
	if d.Secret == "" && cfg.Destination != nil && cfg.Destination.sameAccount(d) {
		d.Secret = cfg.Destination.Secret
	}
	if !d.needsSecret() && d.Kind != kindWebDAV {
		d.Secret = ""
	}
	if d.needsSecret() && d.Secret == "" {
		fail(w, http.StatusBadRequest, "bad-destination", "give its password")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	switch d.Kind {
	case kindSMB:
		if err := localHost(ctx, d.Host); err != nil {
			fail(w, http.StatusBadRequest, "bad-destination", err.Error())
			return
		}
	case kindSFTP:
		if d.UnitKey && cfg.SSHKey == nil {
			fail(w, http.StatusConflict, "no-ssh-key", "make the unit's key first")
			return
		}
		// The owner confirmed the server's key, from POST /backups/host-key: SFTP checks it's the server's.
		if d.HostKey == "" {
			fail(w, http.StatusBadRequest, "host-key", "confirm the server's key first")
			return
		}
	}
	cfg.Destination = &d
	target, err := s.target(cfg)
	if err == nil {
		err = backup.Test(ctx, target)
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "destination", err.Error())
		return
	}
	if _, err := s.changeBackups(func(c *backupConfig) error { c.Destination = &d; return nil }); err != nil {
		s.failStore(w, err)
		return
	}
	s.backups.mu.Lock()
	s.backups.sidecars = nil
	s.backups.mu.Unlock()
	if err := s.Store.AddAudit(s.audit(r, p.Name, "backup-destination", d.about(), "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	s.getBackups(w, r, p)
}

// newBackupKey makes a key and shows it once. It's kept only when the owner gives it back (confirmBackupKey).
func (s *Server) newBackupKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		CurrentPassword string `json:"current_password"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	secret, key, err := backup.NewKey()
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	s.backups.mu.Lock()
	s.backups.pending = &pendingKey{secret: secret, key: key, until: time.Now().Add(30 * time.Minute)}
	s.backups.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "id": key.ID, "unit": s.Hostname})
}

func (s *Server) confirmBackupKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Secret string `json:"secret"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) {
		return
	}
	_, key, err := backup.Parse(body.Secret)
	s.backups.mu.Lock()
	pending := s.backups.pending
	s.backups.mu.Unlock()
	if pending == nil || time.Now().After(pending.until) {
		fail(w, http.StatusConflict, "no-key", "make a key first")
		return
	}
	if err != nil || key.Recipient != pending.key.Recipient {
		fail(w, http.StatusBadRequest, "wrong-key", "that isn't the key just made")
		return
	}
	key.Made = time.Now().UTC().Format(time.RFC3339)
	if _, err := s.changeBackups(func(c *backupConfig) error { c.Key = &key; return nil }); err != nil {
		s.failStore(w, err)
		return
	}
	s.backups.mu.Lock()
	s.backups.pending = nil
	s.backups.mu.Unlock()
	if err := s.Store.AddAudit(s.audit(r, p.Name, "backup-key", key.ID, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	s.getBackups(w, r, p)
}

// checkBackupKey says whether a key is the one backups are made with now.
func (s *Server) checkBackupKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Secret string `json:"secret"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	_, key, err := backup.Parse(body.Secret)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-key", err.Error())
		return
	}
	cfg := s.backupNow()
	writeJSON(w, http.StatusOK, map[string]any{"id": key.ID, "matches": cfg.Key != nil && cfg.Key.Recipient == key.Recipient})
}

func (s *Server) putBackupSchedule(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body backup.Schedule
	if !s.backupsUsable(w) || !readJSON(w, r, &body) {
		return
	}
	if err := body.Check(); err != nil {
		fail(w, http.StatusBadRequest, "bad-schedule", err.Error())
		return
	}
	cfg := s.backupNow()
	if body.On && (cfg.Key == nil || cfg.Destination == nil) {
		fail(w, http.StatusConflict, "not-ready", "make a key and choose where backups go first")
		return
	}
	if _, err := s.changeBackups(func(c *backupConfig) error {
		c.Schedule, c.Since = body, time.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		s.failStore(w, err)
		return
	}
	state := "off"
	if body.On {
		state = "daily at " + body.At + " " + body.Zone
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "backup-schedule", state, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	s.getBackups(w, r, p)
}

// queueJob is startJob for a job that needs something held for it before it can start.
func (s *Server) queueJob(w http.ResponseWriter, r *http.Request, p *Principal, kind, target string, hold func(store.Job)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		fail(w, http.StatusBadRequest, "idempotency-key", "send an Idempotency-Key header")
		return
	}
	j, created, err := s.Store.CreateJob(store.Job{Kind: kind, Target: target, Actor: p.Name, BootID: s.BootID}, key,
		s.audit(r, p.Name, kind, target, "queued"))
	if err != nil {
		s.failStore(w, err)
		return
	}
	if created && hold != nil {
		hold(j)
	}
	if created && s.Jobs != nil {
		s.Jobs.Wake()
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) backupReady(w http.ResponseWriter) (backupConfig, bool) {
	cfg := s.backupNow()
	switch {
	case cfg.Key == nil:
		fail(w, http.StatusConflict, "no-key", "make a backup key first")
	case cfg.Destination == nil:
		fail(w, http.StatusConflict, "no-destination", "choose where backups go first")
	default:
		return cfg, true
	}
	return cfg, false
}

func (s *Server) runBackupNow(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !s.backupsUsable(w) {
		return
	}
	if _, ok := s.backupReady(w); ok {
		s.startJob(w, r, p, jobBackup, "now")
	}
}

// backupName is a name a backup can have where backups go.
func backupName(name string) bool {
	return backup.FileName(name) && strings.HasSuffix(name, backup.Extension) && name != backup.Extension
}

// checkBackup checks a backup where backups go: that it's whole and as it was sent, and with the owner's key, that it
// opens and is sealed.
func (s *Server) checkBackup(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) {
		return
	}
	cfg := s.backupNow()
	if cfg.Destination == nil || !backupName(body.Name) {
		fail(w, http.StatusNotFound, "not-found", "no such backup")
		return
	}
	if body.Secret != "" {
		if _, _, err := backup.Parse(body.Secret); err != nil {
			fail(w, http.StatusBadRequest, "bad-key", err.Error())
			return
		}
	}
	s.queueJob(w, r, p, jobCheck, body.Name, func(j store.Job) { s.holdSecret(j.ID, jobSecret{owner: body.Secret}) })
}

// restoreBackup replaces everything on the unit with what a backup holds, and restarts it.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Name            string `json:"name"`
		Secret          string `json:"secret"`
		KeepIdentity    bool   `json:"keep_identity"`
		CurrentPassword string `json:"current_password"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	cfg := s.backupNow()
	if cfg.Destination == nil || !backupName(body.Name) {
		fail(w, http.StatusNotFound, "not-found", "no such backup")
		return
	}
	if _, _, err := backup.Parse(body.Secret); err != nil {
		fail(w, http.StatusBadRequest, "bad-key", err.Error())
		return
	}
	if status, ok := s.readUpdates(w, r); !ok || !s.free(w, status) {
		return
	}
	s.queueJob(w, r, p, jobRestore, body.Name, func(j store.Job) {
		s.holdSecret(j.ID, jobSecret{owner: body.Secret, keepIdentity: body.KeepIdentity})
	})
}

func (s *Server) holdSecret(job int64, secret jobSecret) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if s.backups.secrets == nil {
		s.backups.secrets = map[int64]jobSecret{}
	}
	s.backups.secrets[job] = secret
}

func (s *Server) takeSecret(job int64) (jobSecret, bool) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	secret, ok := s.backups.secrets[job]
	delete(s.backups.secrets, job)
	return secret, ok
}

// BackupJobs are the jobs the runner hands back to the console.
func (s *Server) BackupJobs() map[string]func(context.Context, store.Job) (string, error) {
	return map[string]func(context.Context, store.Job) (string, error){
		jobBackup: s.makeBackup, jobCheck: s.checkJob, jobRestore: s.restoreJob,
	}
}

// pipeReader gives up on a helper that stops writing.
type pipeReader struct{ f *os.File }

func (p pipeReader) Read(b []byte) (int, error) {
	p.f.SetReadDeadline(time.Now().Add(10 * time.Minute))
	return p.f.Read(b)
}

// makeBackup has the helper gather a backup, sends it where backups go with a record beside it, then lets the
// schedule's keeping prune this unit's older ones.
func (s *Server) makeBackup(ctx context.Context, j store.Job) (string, error) {
	cfg := s.backupNow()
	if cfg.Key == nil || cfg.Destination == nil {
		return "", errors.New("there's no backup key, or nowhere to send backups")
	}
	opener, ok := s.Rootd.(rootd.Opener)
	if !ok {
		return "", errors.New("the root helper can't make backups")
	}
	defer s.clearProgress()
	s.setProgress(j.ID, "gathering", 0, 0)
	created := time.Now().UTC().Truncate(time.Second)
	if err := s.Store.Snapshot(s.BackupSnapshot, j.ID); err != nil {
		return "", fmt.Errorf("the console's state couldn't be copied: %w", err)
	}
	defer os.Remove(s.BackupSnapshot)
	openCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	pipe, out, err := opener.Open(openCtx, rootd.Request{Verb: "backup-open", Backup: &rootd.Backup{Key: cfg.Key,
		Manifest: &backup.Manifest{Created: created.Format(time.RFC3339), Unit: s.Hostname, Release: s.Release,
			StateFormat: store.FormatWrites, Series: cfg.Series}}})
	cancel()
	if err != nil {
		return "", err
	}
	defer pipe.Close()
	var opened struct {
		ID       string          `json:"id"`
		Manifest backup.Manifest `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &opened); err != nil {
		return "", err
	}
	name := backup.Name(s.Hostname, created)
	target, err := s.target(cfg)
	if err != nil {
		return "", err
	}
	s.setProgress(j.ID, "sending", 0, opened.Manifest.Size)
	size, sum, err := target.Upload(ctx, name, pipeReader{pipe}, func() error {
		_, err := s.Rootd.Call(ctx, rootd.Request{Verb: "backup-finish", Target: opened.ID})
		return err
	}, func(n int64) { s.setProgress(j.ID, "sending", n, opened.Manifest.Size) })
	if err != nil {
		return "", err
	}
	record := backup.Sidecar{Format: backup.Format, Created: opened.Manifest.Created, Unit: s.Hostname, Release: s.Release,
		KeyID: cfg.Key.ID, Series: cfg.Series, Size: size, SHA256: sum, Contents: opened.Manifest.Size}
	seal, err := cfg.Key.SealKey()
	if err != nil {
		return "", err
	}
	record.Sign(seal)
	data, _ := json.MarshalIndent(record, "", "  ")
	if err := target.Put(ctx, name+".json", data); err != nil {
		return "", fmt.Errorf("the backup was sent, but not its record: %w", err)
	}
	result := fmt.Sprintf("%s, %s", path.Base(name), sizeText(size))
	if pruned, err := s.pruneBackups(ctx, cfg); err != nil {
		result += "; older ones weren't pruned: " + err.Error()
	} else if pruned > 0 {
		result += fmt.Sprintf("; %d older pruned", pruned)
	}
	return result, nil
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}

// pruneBackups deletes this unit's backups the schedule's keeping no longer wants, each with its record.
func (s *Server) pruneBackups(ctx context.Context, cfg backupConfig) (int, error) {
	list, err := s.backupList(ctx, cfg)
	if err != nil {
		return 0, err
	}
	byTime := map[time.Time]string{}
	var times []time.Time
	for _, b := range list {
		if !b.ThisUnit || !b.Record {
			continue
		}
		at, err := time.Parse(time.RFC3339, b.Created)
		if err != nil {
			continue
		}
		byTime[at] = b.Name
		times = append(times, at)
	}
	target, err := s.target(cfg)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, at := range cfg.Schedule.Prune(times) {
		name := byTime[at]
		if err := target.Delete(ctx, name); err != nil {
			return pruned, err
		}
		target.Delete(ctx, name+".json")
		s.backups.mu.Lock()
		delete(s.backups.sidecars, name)
		s.backups.mu.Unlock()
		pruned++
	}
	return pruned, nil
}

// checkJob reads a backup back from where backups go and checks it against its record, and with the owner's key,
// opens it and checks every file against its seal. Nothing is kept.
func (s *Server) checkJob(ctx context.Context, j store.Job) (string, error) {
	secret, _ := s.takeSecret(j.ID)
	cfg := s.backupNow()
	if cfg.Destination == nil {
		return "", errors.New("there's nowhere backups go")
	}
	defer s.clearProgress()
	target, err := s.target(cfg)
	if err != nil {
		return "", err
	}
	record, err := s.sidecar(ctx, target, j.Target)
	if err != nil {
		return "", fmt.Errorf("its record isn't there: %w", err)
	}
	thisKey := cfg.Key != nil && record.KeyID == cfg.Key.ID
	if thisKey {
		if seal, err := cfg.Key.SealKey(); err != nil || !record.Sealed(seal) {
			return "", errors.New("its record isn't sealed with this unit's key")
		}
	}
	body, _, err := target.Get(ctx, j.Target)
	if err != nil {
		return "", err
	}
	defer body.Close()
	hash := sha256.New()
	counted := &counting{r: io.TeeReader(body, hash), progress: func(n int64) { s.setProgress(j.ID, "checking", n, record.Size) }}
	if secret.owner != "" {
		if _, err := backup.Read(counted, secret.owner, backup.Discard{}); err != nil {
			return "", err
		}
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return "", err
	}
	if counted.n != record.Size || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return "", errors.New("it isn't there as it was sent")
	}
	switch {
	case secret.owner != "":
		return "whole, and it opens with the key", nil
	case thisKey:
		return "whole, as it was sent", nil
	}
	return "whole, as its record says; made with another key", nil
}

type counting struct {
	r        io.Reader
	n        int64
	progress func(int64)
	last     time.Time
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.progress != nil && time.Since(c.last) > time.Second {
		c.progress(c.n)
		c.last = time.Now()
	}
	return n, err
}

// restoreJob fetches a backup into the helper, which opens it with the owner's key and checks its seal, then has it
// put in place at a restart. What arrives is checked against its record too.
func (s *Server) restoreJob(ctx context.Context, j store.Job) (string, error) {
	secret, ok := s.takeSecret(j.ID)
	if !ok {
		return "", errors.New("the key was forgotten in a restart of the console: start the restore again")
	}
	cfg := s.backupNow()
	if cfg.Destination == nil {
		return "", errors.New("there's nowhere backups are")
	}
	opener, ok := s.Rootd.(rootd.Opener)
	if !ok {
		return "", errors.New("the root helper can't restore")
	}
	defer s.clearProgress()
	target, err := s.target(cfg)
	if err != nil {
		return "", err
	}
	record, recordErr := s.sidecar(ctx, target, j.Target)
	body, size, err := target.Get(ctx, j.Target)
	if err != nil {
		return "", err
	}
	defer body.Close()
	pipe, id, err := opener.Open(ctx, rootd.Request{Verb: "restore-open", Backup: &rootd.Backup{Owner: secret.owner}})
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	counted := &counting{r: io.TeeReader(body, hash), progress: func(n int64) { s.setProgress(j.ID, "fetching", n, size) }}
	_, copyErr := io.Copy(pipe, counted)
	pipe.Close()
	sum := hex.EncodeToString(hash.Sum(nil))
	out, err := s.Rootd.Call(ctx, rootd.Request{Verb: "restore-finish", Target: id, Backup: &rootd.Backup{SHA256: sum}})
	if err != nil {
		return "", err
	}
	if copyErr != nil {
		return "", copyErr
	}
	if recordErr == nil && (record.SHA256 != sum || record.Size != counted.n) {
		s.Rootd.Call(ctx, rootd.Request{Verb: "restore-discard"})
		return "", errors.New("it isn't there as it was sent")
	}
	var finished struct {
		Manifest backup.Manifest `json:"manifest"`
	}
	json.Unmarshal([]byte(out.Output), &finished)
	s.setProgress(j.ID, "restoring", counted.n, size)
	which := finished.Manifest.Unit + " " + finished.Manifest.Created
	if err := s.Store.AddAudit(store.Audit{BootID: s.BootID, Actor: j.Actor, Origin: "local", Source: "unit",
		Action: "restore", Target: which, Result: "restarting"}); err != nil {
		return "", err
	}
	if _, err := s.Rootd.Call(ctx, rootd.Request{Verb: "restore-apply", Backup: &rootd.Backup{KeepIdentity: secret.keepIdentity}}); err != nil {
		s.Rootd.Call(ctx, rootd.Request{Verb: "restore-discard"})
		return "", err
	}
	return "restoring " + which + ": the unit restarts", nil
}

// WatchBackups runs the schedule: once the latest slot it was due at is past and hasn't been made, it queues a backup,
// marked first, so a restart can't make it twice. A unit that was off then makes it when it's back. It also records a
// restore that the last boot put in place.
func (s *Server) WatchBackups(ctx context.Context) {
	s.noteRestore()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		s.backupTick(time.Now())
	}
}

// backupTick queues the scheduled backup if one is due now.
func (s *Server) backupTick(now time.Time) {
	cfg := s.backupNow()
	if !cfg.Schedule.On || cfg.Key == nil || cfg.Destination == nil || !s.validated() {
		return
	}
	slot := cfg.Schedule.Slot(now)
	since, _ := time.Parse(time.RFC3339, cfg.Since)
	last, _ := time.Parse(time.RFC3339, cfg.LastSlot)
	if !slot.After(since) || !slot.After(last) {
		return
	}
	at := slot.Format(time.RFC3339)
	if _, err := s.changeBackups(func(c *backupConfig) error { c.LastSlot = at; return nil }); err != nil {
		log.Printf("backups: %v", err)
		return
	}
	_, _, err := s.Store.CreateJob(store.Job{Kind: jobBackup, Target: "scheduled", Actor: "schedule", BootID: s.BootID},
		"schedule:"+at, store.Audit{BootID: s.BootID, Actor: "schedule", Origin: "local", Source: "unit",
			Action: jobBackup, Target: "scheduled " + at, Result: "queued"})
	if err != nil {
		log.Printf("backups: %v", err)
		return
	}
	if s.Jobs != nil {
		s.Jobs.Wake()
	}
}

// noteRestore puts a restore the last boot finished in the audit log, once.
func (s *Server) noteRestore() {
	if s.RestoreReport == "" || s.Store == nil {
		return
	}
	data, err := os.ReadFile(s.RestoreReport)
	if err != nil {
		return
	}
	sum := sha256.Sum256(data)
	seen := hex.EncodeToString(sum[:])
	if was, _ := s.Store.Meta("restore_seen"); was == seen {
		return
	}
	var report struct {
		Unit    string `json:"unit"`
		Created string `json:"created"`
	}
	json.Unmarshal(data, &report)
	if err := s.Store.AddAudit(store.Audit{BootID: s.BootID, Actor: "restore", Origin: "local", Source: "unit",
		Action: "restored", Target: report.Unit + " " + report.Created, Result: "ok"}); err == nil {
		s.Store.SetMeta("restore_seen", seen)
	}
}
