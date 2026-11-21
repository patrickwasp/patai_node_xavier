package api

// Where backups go (docs/console.md, Backups, Where they go): a bucket, a share on a NAS, a folder on a server over SFTP
// or WebDAV, or a disk plugged into the unit. Each is a backup.Target, and a folder in it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/rootd"
)

// The kinds of place backups go.
const (
	kindS3     = "s3"
	kindSMB    = "smb"
	kindSFTP   = "sftp"
	kindWebDAV = "webdav"
	kindDisk   = "disk"
)

// destination is where backups go: one kind of target, and the folder in it. Secret is S3's secret key or the
// password, which is never shown again.
type destination struct {
	Kind   string `json:"kind"`
	Folder string `json:"folder"`
	// S3
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
	// SMB and SFTP: a name or an address, with :port if it isn't the usual one.
	Host   string `json:"host,omitempty"`
	Share  string `json:"share,omitempty"`
	Domain string `json:"domain,omitempty"`
	User   string `json:"user,omitempty"`
	// SFTP: the server's key, as the owner confirmed it, and whether the unit signs in with its own key.
	HostKey string `json:"host_key,omitempty"`
	UnitKey bool   `json:"unit_key,omitempty"`
	// WebDAV: the address of the folder the folder above is in.
	URL string `json:"url,omitempty"`
	// A disk on the unit: its filesystem's ID, and what it was called when it was chosen.
	Disk     string `json:"disk,omitempty"`
	DiskName string `json:"disk_name,omitempty"`
	Secret   string `json:"secret,omitempty"`
}

// destinationJSON is a destination as it's shown: never its secret, only whether there is one.
type destinationJSON struct {
	destination
	HasSecret bool `json:"has_secret"`
}

func (d destination) shown() destinationJSON {
	c := d
	c.Secret = ""
	return destinationJSON{destination: c, HasSecret: d.Secret != ""}
}

// about is where it is, in a line, for the audit log and the page.
func (d destination) about() string {
	folder := strings.TrimSuffix("/"+d.Folder, "/")
	switch d.Kind {
	case kindS3:
		return d.Endpoint + "/" + d.Bucket + folder
	case kindSMB:
		return "//" + d.Host + "/" + d.Share + folder
	case kindSFTP:
		return "sftp://" + d.User + "@" + d.Host + "/" + d.Folder
	case kindWebDAV:
		return strings.TrimSuffix(d.URL, "/") + folder
	case kindDisk:
		return "disk " + d.DiskName + "/patai-backups"
	}
	return d.Kind
}

var (
	folderPart  = regexp.MustCompile(`^[A-Za-z0-9._ @+()-]{1,100}$`)
	shareName   = regexp.MustCompile(`^[A-Za-z0-9._ $-]{1,80}$`)
	accountName = regexp.MustCompile(`^[A-Za-z0-9._@\\-]{1,128}$`)
	hostPort    = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]{1,255}$`)
)

// checkFolder is a folder by names joined with /, as each kind of target takes: nothing that leaves it.
func checkFolder(folder string) error {
	if folder == "" {
		return nil
	}
	if len(folder) > 200 {
		return errors.New("that folder's name is too long")
	}
	for _, part := range strings.Split(folder, "/") {
		if !folderPart.MatchString(part) || part == "." || part == ".." || strings.TrimSpace(part) != part {
			return errors.New("give the folder as names joined with /, such as patai or backups/patai")
		}
	}
	return nil
}

// tidy trims what an owner typed, so the same place is always kept the same way.
func (d *destination) tidy() {
	d.Kind = strings.TrimSpace(d.Kind)
	d.Folder = strings.Trim(strings.TrimSpace(d.Folder), "/")
	d.Endpoint = strings.TrimRight(strings.TrimSpace(d.Endpoint), "/")
	d.URL = strings.TrimRight(strings.TrimSpace(d.URL), "/") + "/"
	if d.URL == "/" {
		d.URL = ""
	}
	for _, s := range []*string{&d.Region, &d.Bucket, &d.AccessKey, &d.Host, &d.Share, &d.Domain, &d.User, &d.HostKey, &d.Disk} {
		*s = strings.TrimSpace(*s)
	}
}

// check is the shape of what's given for its kind. Whether it answers is tested after.
func (d destination) check() error {
	if err := checkFolder(d.Folder); err != nil {
		return err
	}
	if len(d.Secret) > 1024 {
		return errors.New("that password is too long")
	}
	switch d.Kind {
	case kindS3:
		if err := checkBase(d.Endpoint); err != nil {
			return err
		}
		if d.Bucket == "" || d.AccessKey == "" || len(d.Bucket) > 63 || len(d.Region) > 64 || len(d.AccessKey) > 256 {
			return errors.New("give the bucket, its access key and its secret")
		}
	case kindSMB, kindSFTP:
		if !hostPort.MatchString(d.Host) || !accountName.MatchString(d.User) {
			return errors.New("give the server's name or address, and the user to sign in as")
		}
		if d.Kind == kindSMB && (!shareName.MatchString(d.Share) || d.Domain != "" && !accountName.MatchString(d.Domain)) {
			return errors.New("give the share's name, such as backups")
		}
		if d.Kind == kindSFTP && len(d.HostKey) > 16<<10 {
			return errors.New("that isn't a server's key")
		}
	case kindWebDAV:
		if err := checkBase(d.URL); err != nil {
			return err
		}
		if d.User != "" && !accountName.MatchString(d.User) {
			return errors.New("that isn't a user name")
		}
	case kindDisk:
		if d.Disk == "" || len(d.Disk) > 64 || len(d.DiskName) > 200 {
			return errors.New("choose a disk")
		}
	default:
		return errors.New("backups go to a bucket, a share, an SFTP or WebDAV server, or a disk")
	}
	return nil
}

// needsSecret says whether it can't be reached without a secret.
func (d destination) needsSecret() bool {
	switch d.Kind {
	case kindS3, kindSMB:
		return true
	case kindSFTP:
		return !d.UnitKey
	}
	return false
}

// sameAccount is whether two destinations sign in as the same account at the same place, so a secret given for one
// can be kept for the other.
func (d destination) sameAccount(o destination) bool {
	return d.Kind == o.Kind && d.Endpoint == o.Endpoint && d.AccessKey == o.AccessKey && d.Host == o.Host &&
		d.User == o.User && d.Domain == o.Domain && d.URL == o.URL
}

// localHost is whether every address a name has is on the unit's own networks: a private one, its own, or a tailnet's.
// SMB isn't for the internet.
func localHost(ctx context.Context, host string) error {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, strings.Trim(name, "[]"))
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("%s can't be found", name)
	}
	_, tailnet, _ := net.ParseCIDR("100.64.0.0/10")
	for _, ip := range ips {
		if !ip.IP.IsPrivate() && !ip.IP.IsLoopback() && !ip.IP.IsLinkLocalUnicast() && !tailnet.Contains(ip.IP) {
			return errors.New("a share has to be on this network or the tailnet: SMB isn't safe over the internet")
		}
	}
	return nil
}

// target is the destination as something backups can be listed, sent and fetched through.
func (s *Server) target(cfg backupConfig) (backup.Target, error) {
	d := cfg.Destination
	if d == nil {
		return nil, errors.New("choose where backups go first")
	}
	switch d.Kind {
	case kindS3:
		prefix := d.Folder
		if prefix != "" {
			prefix += "/"
		}
		return &backup.Bucket{Endpoint: d.Endpoint, Region: d.Region, Name: d.Bucket, AccessKey: d.AccessKey,
			SecretKey: d.Secret, PathStyle: d.PathStyle, Prefix: prefix}, nil
	case kindSMB:
		return &backup.SMB{Host: d.Host, Share: d.Share, User: d.User, Password: d.Secret, Domain: d.Domain, Folder: d.Folder}, nil
	case kindSFTP:
		t := &backup.SFTP{Host: d.Host, User: d.User, HostKey: d.HostKey, Folder: d.Folder}
		if d.UnitKey {
			if cfg.SSHKey == nil {
				return nil, errors.New("the unit has no key of its own to sign in with")
			}
			t.Key = cfg.SSHKey.Private
		} else {
			t.Password = d.Secret
		}
		return t, nil
	case kindWebDAV:
		u := d.URL
		for _, part := range strings.Split(d.Folder, "/") {
			if part != "" {
				u += url.PathEscape(part) + "/"
			}
		}
		return &backup.WebDAV{URL: u, User: d.User, Password: d.Secret}, nil
	case kindDisk:
		opener, ok := s.Rootd.(rootd.Opener)
		if !ok {
			return nil, errors.New("the root helper can't reach disks")
		}
		return diskTarget{rootd: s.Rootd, opener: opener, id: d.Disk}, nil
	}
	return nil, errors.New("backups can't go there")
}

// sshKey is the unit's own key for signing in to an SFTP server: made on the unit, and kept on it, never shown.
type sshKey struct {
	Private string `json:"private"`
	Public  string `json:"public"`
}

// diskTarget is a disk plugged into the unit, its folder of backups, through the root helper, which mounts it and
// opens its files for the console.
type diskTarget struct {
	rootd  rootd.Caller
	opener rootd.Opener
	id     string
}

func (t diskTarget) req(verb, name string) rootd.Request {
	return rootd.Request{Verb: verb, Disk: &rootd.Disk{ID: t.id, Name: name}}
}

func (t diskTarget) List(ctx context.Context) ([]backup.Object, error) {
	out, err := t.rootd.Call(ctx, t.req("backup-disk-files", ""))
	if err != nil {
		return nil, err
	}
	var files []rootd.DiskFile
	if err := json.Unmarshal([]byte(out.Output), &files); err != nil {
		return nil, err
	}
	objects := make([]backup.Object, 0, len(files))
	for _, f := range files {
		objects = append(objects, backup.Object{Name: f.Name, Size: f.Size, Modified: f.Modified})
	}
	return objects, nil
}

func (t diskTarget) Get(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	f, out, err := t.opener.Open(ctx, t.req("backup-disk-read", name))
	if err != nil {
		return nil, 0, err
	}
	size, _ := strconv.ParseInt(out, 10, 64)
	return f, size, nil
}

func (t diskTarget) Delete(ctx context.Context, name string) error {
	_, err := t.rootd.Call(ctx, t.req("backup-disk-delete", name))
	return err
}

func (t diskTarget) Put(ctx context.Context, name string, data []byte) error {
	_, _, err := t.Upload(ctx, name, strings.NewReader(string(data)), func() error { return nil }, nil)
	return err
}

func (t diskTarget) Upload(ctx context.Context, name string, r io.Reader, done func() error, progress func(int64)) (int64, string, error) {
	f, id, err := t.opener.Open(ctx, t.req("backup-disk-write", name))
	if err != nil {
		return 0, "", err
	}
	abort := func(err error) (int64, string, error) {
		f.Close()
		t.rootd.Call(context.WithoutCancel(ctx), rootd.Request{Verb: "backup-disk-abort", Target: id})
		return 0, "", err
	}
	size, sum, err := backup.Copy(ctx, f, r, progress)
	if err != nil {
		return abort(fmt.Errorf("the disk didn't take it: %w", err))
	}
	if err := f.Close(); err != nil {
		return abort(err)
	}
	if err := done(); err != nil {
		return abort(err)
	}
	if _, err := t.rootd.Call(ctx, rootd.Request{Verb: "backup-disk-commit", Target: id}); err != nil {
		return 0, "", err
	}
	return size, sum, nil
}

// listDisks is each disk plugged into the unit that backups could go on.
func (s *Server) listBackupDisks(w http.ResponseWriter, r *http.Request, p *Principal) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	out, err := s.Rootd.Call(ctx, rootd.Request{Verb: "backup-disks"})
	if err != nil {
		fail(w, http.StatusBadGateway, "disks", err.Error())
		return
	}
	var list []rootd.BackupDisk
	if err := json.Unmarshal([]byte(out.Output), &list); err != nil {
		fail(w, http.StatusInternalServerError, "internal", "something went wrong")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": list})
}

// formatBackupDisk erases a disk and makes it one for backups. Everything on it is lost.
func (s *Server) formatBackupDisk(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Device          string `json:"device"`
		CurrentPassword string `json:"current_password"`
	}
	if !s.backupsUsable(w) || !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	out, err := s.Rootd.Call(ctx, rootd.Request{Verb: "backup-disk-format", Disk: &rootd.Disk{Device: body.Device}})
	result := "ok"
	if err != nil {
		result = err.Error()
	}
	if aerr := s.Store.AddAudit(s.audit(r, p.Name, "backup-disk-format", body.Device, result)); aerr != nil {
		s.failStore(w, aerr)
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, "format", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": strings.TrimSpace(out.Output)})
}

// backupSSHKey is the unit's own key for an SFTP server, made the first time it's asked for: the public half, for the
// owner to add to the server.
func (s *Server) backupSSHKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	if !s.backupsUsable(w) {
		return
	}
	cfg, err := s.changeBackups(func(c *backupConfig) error {
		if c.SSHKey != nil {
			return nil
		}
		private, public, err := backup.NewSSHKey()
		if err != nil {
			return err
		}
		// Named for the unit, so the owner can tell its key from others on the server.
		if fields := strings.Fields(public); len(fields) >= 2 {
			public = fields[0] + " " + fields[1] + " patai-backup@" + s.Hostname
		}
		c.SSHKey = &sshKey{Private: private, Public: public}
		return nil
	})
	if err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public": cfg.SSHKey.Public})
}

// scanHostKey is the key an SFTP server has, for the owner to check against the server's own before it's trusted.
func (s *Server) scanHostKey(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Host string `json:"host"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	host := strings.TrimSpace(body.Host)
	if !hostPort.MatchString(host) {
		fail(w, http.StatusBadRequest, "bad-destination", "give the server's name or address")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	line, fingerprint, err := backup.ScanHostKey(ctx, host)
	if err != nil {
		fail(w, http.StatusBadGateway, "destination", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"host_key": line, "fingerprint": fingerprint})
}
