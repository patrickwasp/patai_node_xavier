package api

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/jobs"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

// bucket is enough of S3 for the console: objects, listings and multipart uploads, with no signatures checked.
type bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
}

func (b *bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/backups"), "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		type content struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		list := []content{}
		for k, v := range b.objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				list = append(list, content{k, len(v)})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
		xml.NewEncoder(w).Encode(struct {
			XMLName  xml.Name  `xml:"ListBucketResult"`
			Contents []content `xml:"Contents"`
		}{Contents: list})
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := strconv.Itoa(len(b.uploads) + 1)
		b.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		b.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", `"x"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := b.uploads[q.Get("uploadId")]
		var whole []byte
		for n := 1; n <= len(parts); n++ {
			whole = append(whole, parts[n]...)
		}
		b.objects[key] = whole
		io.WriteString(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(b.uploads, q.Get("uploadId"))
	case r.Method == http.MethodPut:
		b.objects[key] = body
	case r.Method == http.MethodGet:
		data, ok := b.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(b.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (b *bucket) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var names []string
	for k := range b.objects {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// helperBackups is what patai-rootd does for a backup and a restore, on a folder of the test's.
type helperBackups struct {
	mu       sync.Mutex
	tree     string
	done     chan error
	restored map[string]string
	applied  *rootd.Backup
}

func (h *helperBackups) open(req rootd.Request) (*os.File, string, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	h.done = make(chan error, 1)
	switch req.Verb {
	case "backup-open":
		m := *req.Backup.Manifest
		sources := []backup.Source{{Name: "unit/console", Dir: filepath.Join(h.tree, "console")}}
		m.Files, m.Size, _ = backup.Count(sources)
		key := *req.Backup.Key
		go func() {
			err := backup.Write(write, key, m, sources)
			write.Close()
			h.done <- err
		}()
		out, _ := json.Marshal(map[string]any{"id": "b1", "manifest": m})
		return read, string(out), nil
	case "restore-open":
		owner := req.Backup.Owner
		go func() {
			sink := &memorySink{files: map[string]string{}}
			_, err := backup.Read(read, owner, sink)
			read.Close()
			h.mu.Lock()
			h.restored = sink.files
			h.mu.Unlock()
			h.done <- err
		}()
		return write, "r1", nil
	}
	return nil, "", fmt.Errorf("no %s", req.Verb)
}

type memorySink struct{ files map[string]string }

func (m *memorySink) Begin(backup.Manifest) error { return nil }
func (m *memorySink) Put(e backup.Entry, _ time.Time, content io.Reader) error {
	data, err := io.ReadAll(content)
	if e.Type == "file" {
		m.files[e.Name] = string(data)
	}
	return err
}

func backupRig(t *testing.T) (*rig, string, *bucket, *helperBackups) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.BackupPath = filepath.Join(r.dir, "backup.json")
	r.s.BackupSnapshot = filepath.Join(r.dir, "console-backup.db")
	b := &bucket{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	tree := t.TempDir()
	os.MkdirAll(filepath.Join(tree, "console"), 0o700)
	os.WriteFile(filepath.Join(tree, "console", "outside.json"), []byte(`{"providers":[]}`), 0o600)
	h := &helperBackups{tree: tree}
	r.rootd.open = h.open
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		switch req.Verb {
		case "backup-finish", "restore-finish":
			if err := <-h.done; err != nil {
				return "", false
			}
			return `{"manifest":{"unit":"patai-0001","created":"2026-11-19T03:00:00Z"}}`, true
		case "restore-apply":
			h.mu.Lock()
			h.applied = req.Backup
			h.mu.Unlock()
			return "restoring", true
		}
		return "", false
	}
	runner := jobs.New(r.s.Store, r.rootd, "boot-1")
	runner.Aside = r.s.BackupJobs()
	r.s.Jobs = runner
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go runner.Run(ctx)
	want(t, r.do("PUT", "/api/v1/backups/destination", map[string]any{"kind": "s3", "endpoint": server.URL, "bucket": "backups",
		"folder": "patai", "access_key": "a", "secret": "s", "path_style": true, "current_password": password},
		opts{cookie: cookie}), 200)
	return r, cookie, b, h
}

func (r *rig) waitJob(cookie string, w *httptest.ResponseRecorder) store.Job {
	r.t.Helper()
	want(r.t, w, 202)
	var j store.Job
	json.Unmarshal(w.Body.Bytes(), &j)
	return r.waitJobID(j.ID)
}

func (r *rig) waitJobID(id int64) store.Job {
	r.t.Helper()
	for range 200 {
		j, err := r.s.Store.Job(id)
		if err == nil && (j.State == store.JobSucceeded || j.State == store.JobFailed) {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("job %d didn't finish", id)
	return store.Job{}
}

func TestBackupAndRestore(t *testing.T) {
	r, cookie, b, h := backupRig(t)
	if names := b.names(); len(names) != 0 {
		t.Fatalf("the destination's test was left: %v", names)
	}
	want(t, r.do("POST", "/api/v1/backups/run", nil, opts{cookie: cookie, idem: "run-0"}), 409)

	w := r.do("POST", "/api/v1/backups/key", map[string]string{"current_password": password}, opts{cookie: cookie})
	want(t, w, 200)
	var made struct{ Secret, ID string }
	json.Unmarshal(w.Body.Bytes(), &made)
	other, _, _ := backup.NewKey()
	want(t, r.do("POST", "/api/v1/backups/key/confirm", map[string]string{"secret": other}, opts{cookie: cookie}), 400)
	want(t, r.do("POST", "/api/v1/backups/key/confirm", map[string]string{"secret": made.Secret}, opts{cookie: cookie}), 200)
	if data, _ := os.ReadFile(r.s.BackupPath); bytes.Contains(data, []byte(made.Secret)) || bytes.Contains(data, []byte(strings.ToLower(made.Secret))) {
		t.Fatal("the owner's key was kept on the unit")
	}

	j := r.waitJob(cookie, r.do("POST", "/api/v1/backups/run", nil, opts{cookie: cookie, idem: "run-1"}))
	if j.State != store.JobSucceeded {
		t.Fatalf("backup: %+v", j)
	}
	names := b.names()
	if len(names) != 2 || !strings.HasPrefix(names[0], "patai/patai-0001-") || !strings.HasSuffix(names[1], backup.Extension+".json") {
		t.Fatalf("bucket: %v", names)
	}
	w = r.do("GET", "/api/v1/backups/list", nil, opts{cookie: cookie})
	want(t, w, 200)
	var list struct{ Backups []listedBackup }
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Backups) != 1 || !list.Backups[0].ThisUnit || !list.Backups[0].ThisKey || !list.Backups[0].Record {
		t.Fatalf("list: %s", w.Body.String())
	}
	name := list.Backups[0].Name

	j = r.waitJob(cookie, r.do("POST", "/api/v1/backups/check", map[string]string{"name": name, "secret": made.Secret}, opts{cookie: cookie, idem: "check-1"}))
	if j.State != store.JobSucceeded || !strings.Contains(j.Result, "opens with the key") {
		t.Fatalf("check: %+v", j)
	}
	// A bucket that changes a byte is caught without the key.
	b.mu.Lock()
	saved := append([]byte(nil), b.objects["patai/"+name]...)
	b.objects["patai/"+name][100] ^= 1
	b.mu.Unlock()
	j = r.waitJob(cookie, r.do("POST", "/api/v1/backups/check", map[string]string{"name": name}, opts{cookie: cookie, idem: "check-2"}))
	if j.State != store.JobFailed {
		t.Fatalf("a changed backup passed: %+v", j)
	}
	b.mu.Lock()
	b.objects["patai/"+name] = saved
	b.mu.Unlock()

	want(t, r.do("POST", "/api/v1/backups/restore", map[string]any{"name": name, "secret": made.Secret, "current_password": "wrong"},
		opts{cookie: cookie, idem: "restore-0"}), 403)
	j = r.waitJob(cookie, r.do("POST", "/api/v1/backups/restore", map[string]any{"name": name, "secret": made.Secret,
		"keep_identity": true, "current_password": password}, opts{cookie: cookie, idem: "restore-1"}))
	if j.State != store.JobSucceeded {
		t.Fatalf("restore: %+v", j)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.restored["unit/console/outside.json"] != `{"providers":[]}` || h.applied == nil || !h.applied.KeepIdentity {
		t.Fatalf("restored %v, applied %+v", h.restored, h.applied)
	}
}

func TestScheduledBackup(t *testing.T) {
	r, cookie, b, _ := backupRig(t)
	w := r.do("POST", "/api/v1/backups/key", map[string]string{"current_password": password}, opts{cookie: cookie})
	var made struct{ Secret string }
	json.Unmarshal(w.Body.Bytes(), &made)
	want(t, r.do("POST", "/api/v1/backups/key/confirm", map[string]string{"secret": made.Secret}, opts{cookie: cookie}), 200)
	now := time.Now().UTC()
	at := now.Add(2 * time.Minute).Format("15:04")
	want(t, r.do("PUT", "/api/v1/backups/schedule", map[string]any{"on": true, "at": at, "zone": "UTC", "keep_days": 7,
		"keep_weeks": 4, "keep_months": 6}, opts{cookie: cookie}), 200)

	r.s.backupTick(now)
	if jobs, _ := r.s.Store.Jobs(10); len(jobs) != 0 && jobs[0].Kind == jobBackup {
		t.Fatal("a slot from before the schedule was saved ran")
	}
	later := now.Add(3 * time.Minute)
	r.s.backupTick(later)
	r.s.backupTick(later.Add(time.Minute))
	list, _ := r.s.Store.Jobs(10)
	var scheduled []store.Job
	for _, j := range list {
		if j.Kind == jobBackup {
			scheduled = append(scheduled, j)
		}
	}
	if len(scheduled) != 1 || scheduled[0].Actor != "schedule" {
		t.Fatalf("jobs: %+v", list)
	}
	if j := r.waitJobID(scheduled[0].ID); j.State != store.JobSucceeded {
		t.Fatalf("scheduled: %+v", j)
	}
	if len(b.names()) != 2 {
		t.Fatalf("bucket: %v", b.names())
	}
}

// A disk plugged into the unit is reached through the root helper, which opens its files: here, a folder of the test's.
func TestBackupToADisk(t *testing.T) {
	r, cookie, _, _ := backupRig(t)
	disk := t.TempDir()
	writes := map[string][2]string{}
	var mu sync.Mutex
	open, respond := r.rootd.open, r.rootd.respond
	r.rootd.open = func(req rootd.Request) (*os.File, string, error) {
		switch req.Verb {
		case "backup-disk-read":
			f, err := os.Open(filepath.Join(disk, req.Disk.Name))
			if err != nil {
				return nil, "", err
			}
			info, _ := f.Stat()
			return f, strconv.FormatInt(info.Size(), 10), nil
		case "backup-disk-write":
			part := filepath.Join(disk, backup.PartName(req.Disk.Name))
			f, err := os.Create(part)
			mu.Lock()
			id := strconv.Itoa(len(writes) + 1)
			writes[id] = [2]string{part, filepath.Join(disk, req.Disk.Name)}
			mu.Unlock()
			return f, id, err
		}
		return open(req)
	}
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Verb {
		case "backup-disk-files":
			entries, _ := os.ReadDir(disk)
			files := []rootd.DiskFile{}
			for _, e := range entries {
				info, _ := e.Info()
				files = append(files, rootd.DiskFile{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
			}
			data, _ := json.Marshal(files)
			return string(data), true
		case "backup-disk-commit":
			w := writes[req.Target]
			return "", os.Rename(w[0], w[1]) == nil
		case "backup-disk-abort":
			os.Remove(writes[req.Target][0])
			return "", true
		case "backup-disk-delete":
			return "", os.Remove(filepath.Join(disk, req.Disk.Name)) == nil
		}
		mu.Unlock()
		defer mu.Lock()
		return respond(req)
	}
	want(t, r.do("PUT", "/api/v1/backups/destination", map[string]any{"kind": "disk", "disk": "6A2B-11F0",
		"disk_name": "USB drive", "current_password": password}, opts{cookie: cookie}), 200)
	w := r.do("POST", "/api/v1/backups/key", map[string]string{"current_password": password}, opts{cookie: cookie})
	var made struct{ Secret string }
	json.Unmarshal(w.Body.Bytes(), &made)
	want(t, r.do("POST", "/api/v1/backups/key/confirm", map[string]string{"secret": made.Secret}, opts{cookie: cookie}), 200)
	if j := r.waitJob(cookie, r.do("POST", "/api/v1/backups/run", nil, opts{cookie: cookie, idem: "disk-1"})); j.State != store.JobSucceeded {
		t.Fatalf("backup: %+v", j)
	}
	entries, _ := os.ReadDir(disk)
	if len(entries) != 2 || !strings.HasSuffix(entries[0].Name(), backup.Extension) {
		t.Fatalf("disk: %v", entries)
	}
	w = r.do("GET", "/api/v1/backups/list", nil, opts{cookie: cookie})
	var list struct{ Backups []listedBackup }
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Backups) != 1 || !list.Backups[0].Record {
		t.Fatalf("list: %s", w.Body.String())
	}
	j := r.waitJob(cookie, r.do("POST", "/api/v1/backups/check", map[string]string{"name": list.Backups[0].Name,
		"secret": made.Secret}, opts{cookie: cookie, idem: "disk-check"}))
	if j.State != store.JobSucceeded {
		t.Fatalf("check: %+v", j)
	}
}
