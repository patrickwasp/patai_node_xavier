package backup

import (
	"archive/tar"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Format is this kind of backup. A backup is age (age-encryption.org), holding zstd, holding a tar, so an owner can
// open one with the age and zstd tools anywhere, with no unit.
const Format = "patai-backup/2"

// Extension is a backup's name's end.
const Extension = ".tar.zst.age"

// The entries a backup starts and ends with, around what it holds.
const (
	ManifestName = "patai-backup/manifest.json"
	SealName     = "patai-backup/seal.json"
	MacName      = "patai-backup/seal.mac"
)

// A backup is in three layers (docs/console.md, Backups), so it restores onto any kind of node:
//
//	unit/<part>/...              what the unit is anywhere: these folders of /persist, less the platform's files
//	platform/provision/<file>    settings only this kind of node has, restored only onto one of the same platform
//	apps/<service>/<form>/...    a service's data, in a form any node reads: its folder's files, or a dump
//
// Nothing in one names a device, a disk or a port's driver: a restore puts each part where its own release keeps it.
var UnitParts = []string{"console", "provision", "identity", "tailscale"}

// PlatformFiles are the files of /persist/provision that belong to the kind of node, not to the unit: the power mode
// is one of its modes, the network settings are by its ports, and its updates are images for it.
var PlatformFiles = []string{"power-mode", "network.json", "update-channel", "update-channel-name"}

// The forms a service's data can be in: its folder's files as they are, or a dump of a Postgres database, which loads
// into that version of Postgres or a newer one on any processor. A service that had no folder is in the manifest only.
const (
	FormFiles    = "files"
	FormPostgres = "postgres"
	FormNone     = "none"
)

// DumpName is a Postgres service's dump, in its folder: the service's image loads it into an empty database.
const DumpName = "restore.dump"

// Manifest is a backup's first entry: what it is and what's in it.
type Manifest struct {
	Format      string `json:"format"`
	Created     string `json:"created"` // RFC 3339, UTC, by the unit's clock
	Unit        string `json:"unit"`    // its name then
	Release     string `json:"release"`
	Platform    string `json:"platform"`     // the kind of node, as its updates name it: RAUC's compatible
	Arch        string `json:"arch"`         // its processor's, as Go names it: arm64, amd64
	StateFormat int    `json:"state_format"` // of /persist/console (console/STATE_FORMAT)
	KeyID       string `json:"key_id"`
	Series      string `json:"series"` // the unit's run of backups, which a restore carries on
	Apps        []App  `json:"apps"`   // every service of the release that keeps data, held or not
	Size        int64  `json:"size"`   // of the files in it, before compression
	Files       int    `json:"files"`
}

// App is a service's data, as a backup holds it.
type App struct {
	Service string `json:"service"`
	Form    string `json:"form"`
}

// Entry is one file, folder or link, as the seal records it. A restore takes nothing the seal doesn't name as it was.
type Entry struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // file, dir or link
	Mode   int64  `json:"mode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Size   int64  `json:"size,omitempty"`
	Link   string `json:"link,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Source is a folder on disk and the name it has in a backup, such as data/chat-postgres.
type Source struct {
	Name string
	Dir  string
}

// Count is how many files a set of folders holds, and how big they are, for the manifest.
func Count(sources []Source) (files int, size int64, err error) {
	for _, src := range sources {
		err = filepath.WalkDir(src.Dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				files++
				size += info.Size()
			}
			return nil
		})
		if err != nil {
			return 0, 0, err
		}
	}
	return files, size, nil
}

// Write makes a backup of sources into dst: the manifest, every file, folder and link under each source, and the
// seal, a list of them all with each file's hash, signed with the seal key. Anything else, such as a socket, is left
// out. Times are kept to the second.
func Write(dst io.Writer, key Key, m Manifest, sources []Source) error {
	recipient, err := key.Recipients()
	if err != nil {
		return err
	}
	seal, err := key.SealKey()
	if err != nil {
		return err
	}
	encrypted, err := age.Encrypt(dst, recipient)
	if err != nil {
		return err
	}
	compressed, err := zstd.NewWriter(encrypted, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(2))
	if err != nil {
		return err
	}
	w := &writer{tar: tar.NewWriter(compressed)}
	m.Format, m.KeyID = Format, key.ID
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := w.bytes(ManifestName, manifest); err != nil {
		return err
	}
	for _, src := range sources {
		if err := w.tree(src); err != nil {
			return err
		}
	}
	list, err := json.Marshal(map[string]any{"entries": w.entries})
	if err != nil {
		return err
	}
	if err := w.raw(SealName, list); err != nil {
		return err
	}
	if err := w.raw(MacName, []byte(hex.EncodeToString(mac(seal, list))+"\n")); err != nil {
		return err
	}
	if err := w.tar.Close(); err != nil {
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	return encrypted.Close()
}

type writer struct {
	tar     *tar.Writer
	entries []Entry
}

var stamp = time.Unix(0, 0).UTC()

// raw writes an entry the seal doesn't list: the seal itself.
func (w *writer) raw(name string, data []byte) error {
	h := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(data)), ModTime: stamp, Format: tar.FormatPAX}
	if err := w.tar.WriteHeader(h); err != nil {
		return err
	}
	_, err := w.tar.Write(data)
	return err
}

func (w *writer) bytes(name string, data []byte) error {
	if err := w.raw(name, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	w.entries = append(w.entries, Entry{Name: name, Type: "file", Mode: 0o600, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	return nil
}

func (w *writer) tree(src Source) error {
	return filepath.WalkDir(src.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src.Dir, p)
		if err != nil {
			return err
		}
		name := src.Name
		if rel != "." {
			name = path.Join(src.Name, filepath.ToSlash(rel))
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		uid, gid := 0, 0
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
		e := Entry{Name: name, Mode: tarMode(info.Mode()), UID: uid, GID: gid}
		h := &tar.Header{Name: name, Mode: e.Mode, Uid: uid, Gid: gid, ModTime: info.ModTime().Truncate(time.Second),
			Format: tar.FormatPAX}
		switch {
		case info.IsDir():
			e.Type, h.Typeflag, h.Name = "dir", tar.TypeDir, name+"/"
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			e.Type, e.Link, h.Typeflag, h.Linkname = "link", link, tar.TypeSymlink, link
		case info.Mode().IsRegular():
			e.Type, e.Size, h.Typeflag, h.Size = "file", info.Size(), tar.TypeReg, info.Size()
		default:
			return nil
		}
		if err := w.tar.WriteHeader(h); err != nil {
			return err
		}
		if e.Type == "file" {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			hash := sha256.New()
			n, err := io.Copy(io.MultiWriter(w.tar, hash), io.LimitReader(f, e.Size))
			if err != nil {
				return err
			}
			if n != e.Size {
				return fmt.Errorf("%s changed while it was copied", name)
			}
			e.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
		w.entries = append(w.entries, e)
		return nil
	})
}

func tarMode(m fs.FileMode) int64 {
	mode := int64(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// Sink takes what a backup holds as a restore reads it, before the seal has been checked: nothing it's given may be
// used until Read has returned without an error.
type Sink interface {
	// Begin is told what the backup says it is, and may refuse it.
	Begin(Manifest) error
	// Put is each entry under persist/ or data/, with a file's content.
	Put(e Entry, modTime time.Time, content io.Reader) error
}

// Discard checks a backup and keeps nothing.
type Discard struct{}

func (Discard) Begin(Manifest) error { return nil }
func (Discard) Put(e Entry, _ time.Time, content io.Reader) error {
	_, err := io.Copy(io.Discard, content)
	return err
}

// ErrNotSealed is a backup whose seal isn't the key's: damaged, or not made by a unit that held this key.
var ErrNotSealed = errors.New("this backup wasn't sealed with this key")

const (
	maxManifest = 1 << 16
	maxSeal     = 64 << 20
)

// Read opens a backup with the owner's key and hands what's in it to sink, then checks it against its seal: every entry
// as it was, no more and no fewer, signed with the key's seal. Only then does it return the manifest without an error.
func Read(src io.Reader, owner string, sink Sink) (Manifest, error) {
	identity, key, err := Parse(owner)
	if err != nil {
		return Manifest{}, err
	}
	seal, err := key.SealKey()
	if err != nil {
		return Manifest{}, err
	}
	decrypted, err := age.Decrypt(src, identity)
	if err != nil {
		var none *age.NoIdentityMatchError
		if errors.As(err, &none) {
			return Manifest{}, errors.New("this backup wasn't made with this key")
		}
		return Manifest{}, fmt.Errorf("this isn't a backup that can be opened: %w", err)
	}
	decompressed, err := zstd.NewReader(decrypted, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return Manifest{}, err
	}
	defer decompressed.Close()
	r := tar.NewReader(decompressed)

	h, err := r.Next()
	if err != nil || h.Name != ManifestName || h.Size > maxManifest {
		return Manifest{}, errors.New("this backup doesn't start with its manifest")
	}
	manifestData, err := io.ReadAll(io.LimitReader(r, maxManifest))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(manifestData, &m); err != nil || m.Format != Format {
		return Manifest{}, fmt.Errorf("this backup isn't one this release can read (%q)", m.Format)
	}
	if err := sink.Begin(m); err != nil {
		return m, err
	}
	sum := sha256.Sum256(manifestData)
	got := []Entry{{Name: ManifestName, Type: "file", Mode: 0o600, Size: int64(len(manifestData)), SHA256: hex.EncodeToString(sum[:])}}

	var list, signature []byte
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("this backup is damaged: %w", err)
		}
		if list != nil && signature != nil {
			return m, errors.New("this backup has more after its seal")
		}
		switch h.Name {
		case SealName:
			if list, err = io.ReadAll(io.LimitReader(r, maxSeal)); err != nil {
				return m, err
			}
			continue
		case MacName:
			if signature, err = io.ReadAll(io.LimitReader(r, 1024)); err != nil {
				return m, err
			}
			continue
		}
		if list != nil {
			return m, errors.New("this backup has more after its seal")
		}
		e, err := entryOf(h)
		if err != nil {
			return m, err
		}
		hash := sha256.New()
		if err := sink.Put(e, h.ModTime, io.TeeReader(io.LimitReader(r, e.Size), hash)); err != nil {
			return m, err
		}
		if e.Type == "file" {
			e.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
		got = append(got, e)
	}
	// What's left is the end of the archive, and reading it is what has age check the last of the file.
	if _, err := io.Copy(io.Discard, decompressed); err != nil {
		return m, fmt.Errorf("this backup is damaged: %w", err)
	}
	if list == nil || signature == nil {
		return m, errors.New("this backup has no seal: it may be cut short")
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || !hmac.Equal(want, mac(seal, list)) {
		return m, ErrNotSealed
	}
	var sealed struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(list, &sealed); err != nil {
		return m, ErrNotSealed
	}
	if len(sealed.Entries) != len(got) {
		return m, errors.New("this backup doesn't hold what its seal lists")
	}
	for i := range got {
		if got[i] != sealed.Entries[i] {
			return m, fmt.Errorf("%s isn't as its seal says", got[i].Name)
		}
	}
	return m, nil
}

// entryOf checks what a restore would make: a file, folder or link under one of the parts a backup holds, by a name
// that can't leave it.
func entryOf(h *tar.Header) (Entry, error) {
	name := strings.TrimSuffix(h.Name, "/")
	if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return Entry{}, fmt.Errorf("this backup has a name it can't hold: %q", h.Name)
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == ".." || part == "." {
			return Entry{}, fmt.Errorf("this backup has a name it can't hold: %q", h.Name)
		}
	}
	e := Entry{Name: name, Mode: h.Mode & 0o7777, UID: h.Uid, GID: h.Gid}
	switch h.Typeflag {
	case tar.TypeDir:
		e.Type = "dir"
	case tar.TypeReg:
		e.Type, e.Size = "file", h.Size
	case tar.TypeSymlink:
		e.Type, e.Link = "link", h.Linkname
	default:
		return Entry{}, fmt.Errorf("this backup holds something it can't: %q", h.Name)
	}
	if !allowed(parts, e.Type) {
		return Entry{}, fmt.Errorf("this backup holds something it can't: %q", h.Name)
	}
	return e, nil
}

// allowed says whether a backup may hold this, by its name's parts: links only among an app's files, a platform's
// files only by their names, and a Postgres dump alone in its folder.
func allowed(parts []string, kind string) bool {
	switch {
	case len(parts) < 2:
		return false
	case parts[0] == "unit":
		return contains(UnitParts, parts[1]) && kind != "link"
	case parts[0] == "platform":
		return parts[1] == "provision" && (len(parts) == 2 && kind == "dir" ||
			len(parts) == 3 && kind == "file" && contains(PlatformFiles, parts[2]))
	case parts[0] == "apps":
		if len(parts) < 3 || !ServiceName(parts[1]) {
			return false
		}
		switch parts[2] {
		case FormFiles:
			return true
		case FormPostgres:
			return len(parts) == 3 && kind == "dir" || len(parts) == 4 && kind == "file" && parts[3] == DumpName
		}
	}
	return false
}

// AppsOf is the services a manifest names, by their form.
func (m Manifest) AppsOf() map[string]string {
	apps := map[string]string{}
	for _, a := range m.Apps {
		apps[a.Service] = a.Form
	}
	return apps
}

// ServiceName is a name a service's folder can have.
func ServiceName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '-' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
