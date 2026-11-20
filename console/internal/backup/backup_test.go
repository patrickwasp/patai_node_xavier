package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type kept struct {
	begun   Manifest
	entries []Entry
	files   map[string]string
}

func (k *kept) Begin(m Manifest) error { k.begun = m; return nil }
func (k *kept) Put(e Entry, _ time.Time, content io.Reader) error {
	data, err := io.ReadAll(content)
	if k.files == nil {
		k.files = map[string]string{}
	}
	k.entries = append(k.entries, e)
	if e.Type == "file" {
		k.files[e.Name] = string(data)
	}
	return err
}

func tree(t *testing.T) []Source {
	t.Helper()
	root := t.TempDir()
	write := func(name, text string) {
		path := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("console/console.db", "users")
	write("console/outside.json", `{"providers":[]}`)
	write("data/files/a/photo.jpg", strings.Repeat("x", 3<<20))
	os.Symlink("photo.jpg", filepath.Join(root, "data/files/a/link"))
	os.MkdirAll(filepath.Join(root, "data/files/empty"), 0o700)
	return []Source{{Name: "persist/console", Dir: filepath.Join(root, "console")}, {Name: "data/chat-server", Dir: filepath.Join(root, "data")}}
}

func backupOf(t *testing.T, key Key, sources []Source) []byte {
	t.Helper()
	var out bytes.Buffer
	files, size, err := Count(sources)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Created: "2026-11-19T03:00:00Z", Unit: "patai-dev", Release: "2026.11.19-backup1", Services: []string{"chat-server"},
		Files: files, Size: size}
	if err := Write(&out, key, m, sources); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRoundTrip(t *testing.T) {
	secret, key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "AGE-SECRET-KEY-PQ-1") || len(key.ID) != 9 {
		t.Fatalf("key %q, id %q", secret[:20], key.ID)
	}
	data := backupOf(t, key, tree(t))
	var got kept
	m, err := Read(bytes.NewReader(data), "# my key\n"+strings.ToLower(secret)+"\n", &got)
	if err != nil {
		t.Fatal(err)
	}
	if m.Unit != "patai-dev" || m.KeyID != key.ID || m.Files != 3 || got.begun.Unit != "patai-dev" {
		t.Fatalf("manifest %+v", m)
	}
	if got.files["persist/console/console.db"] != "users" || len(got.files["data/chat-server/files/a/photo.jpg"]) != 3<<20 {
		t.Fatalf("files %v", len(got.files))
	}
	links := 0
	for _, e := range got.entries {
		if e.Type == "link" && e.Link == "photo.jpg" {
			links++
		}
	}
	if links != 1 {
		t.Fatalf("entries %+v", got.entries)
	}
}

func TestAnotherKeyCantOpen(t *testing.T) {
	_, key, _ := NewKey()
	other, _, _ := NewKey()
	data := backupOf(t, key, tree(t))
	if _, err := Read(bytes.NewReader(data), other, Discard{}); err == nil || !strings.Contains(err.Error(), "wasn't made with this key") {
		t.Fatalf("got %v", err)
	}
}

// One made with the public half alone, without the seal, isn't taken: whoever holds a bucket can't plant one.
func TestUnsealedIsRefused(t *testing.T) {
	secret, key, _ := NewKey()
	forged := key
	forged.Seal = strings.Repeat("ab", 32)
	data := backupOf(t, forged, tree(t))
	if _, err := Read(bytes.NewReader(data), secret, Discard{}); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("got %v", err)
	}
}

func TestCutShortIsRefused(t *testing.T) {
	secret, key, _ := NewKey()
	data := backupOf(t, key, tree(t))
	for _, keep := range []int{len(data) - 1, len(data) / 2, 200} {
		if _, err := Read(bytes.NewReader(data[:keep]), secret, Discard{}); err == nil {
			t.Fatalf("a backup cut to %d of %d bytes was taken", keep, len(data))
		}
	}
	changed := append([]byte(nil), data...)
	changed[len(changed)/2] ^= 1
	if _, err := Read(bytes.NewReader(changed), secret, Discard{}); err == nil {
		t.Fatal("a changed byte went unnoticed")
	}
}

func TestNamesThatLeave(t *testing.T) {
	for _, name := range []string{"persist/../etc/passwd", "/persist/console/x", "persist/rauc/x", "data/../x", "data/Chat/x",
		"elsewhere/x", "persist/console/./x", "persist"} {
		if _, err := entryOf(&tar.Header{Name: name, Typeflag: tar.TypeReg}); err == nil {
			t.Errorf("%q was taken", name)
		}
	}
	if _, err := entryOf(&tar.Header{Name: "persist/console/x", Typeflag: tar.TypeSymlink, Linkname: "/etc"}); err == nil {
		t.Error("a link under persist/ was taken")
	}
	if _, err := entryOf(&tar.Header{Name: "data/chat-server/files/x", Typeflag: tar.TypeReg}); err != nil {
		t.Error(err)
	}
}

func TestSidecarSeal(t *testing.T) {
	_, key, _ := NewKey()
	seal, _ := key.SealKey()
	c := Sidecar{Format: Format, Created: "2026-11-19T03:00:00Z", Unit: "patai-dev", Size: 10, SHA256: "ab"}
	c.Sign(seal)
	if !c.Sealed(seal) {
		t.Fatal("its own seal")
	}
	c.SHA256 = "cd"
	if c.Sealed(seal) {
		t.Fatal("a changed hash kept its seal")
	}
}

func TestSchedule(t *testing.T) {
	s := DefaultSchedule("America/Toronto")
	now := time.Date(2026, 11, 19, 7, 30, 0, 0, time.UTC) // 02:30 in Toronto
	if got := s.Slot(now); !got.Equal(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("slot %v", got)
	}
	if got := s.Next(now); !got.Equal(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("next %v", got)
	}
	// After daylight saving starts it's still 03:00 there.
	march := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	if got := s.Slot(march); !got.Equal(time.Date(2026, 3, 9, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("slot after the change %v", got)
	}

	var created []time.Time
	for day := 0; day < 400; day++ {
		created = append(created, time.Date(2025, 10, 15, 8, 0, 0, 0, time.UTC).AddDate(0, 0, day))
	}
	created = append(created, created[len(created)-1].Add(time.Hour)) // two on the last day
	gone := s.Prune(created)
	if kept := len(created) - len(gone); kept < 7 || kept > 17 {
		t.Fatalf("kept %d", kept)
	}
	for _, g := range gone {
		if g.Equal(created[len(created)-1]) {
			t.Fatal("pruned the newest")
		}
	}
	if len(s.Prune(created[:1])) != 0 {
		t.Fatal("pruned the only one")
	}
}

// AWS's own example of a signed request, ListObjects with max-keys=2 and prefix=J (Signature Version 4, examples).
func TestSignatureMatchesAWS(t *testing.T) {
	b := &Bucket{Endpoint: "https://s3.amazonaws.com", Name: "examplebucket", Region: "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	u, err := b.target("", url.Values{"max-keys": {"2"}, "prefix": {"J"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.URL = u
	b.sign(req, emptyHash, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	if want := "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"; !strings.HasSuffix(req.Header.Get("Authorization"), want) {
		t.Fatalf("got %s", req.Header.Get("Authorization"))
	}
}

// fakeBucket keeps objects and multipart uploads, as S3 would, without checking signatures.
type fakeBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	parts   int
	failAt  int
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		type content struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		var list []content
		for k, v := range f.objects {
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
		id := strconv.Itoa(len(f.uploads) + 1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		f.parts++
		if f.parts == f.failAt {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := f.uploads[q.Get("uploadId")]
		var whole []byte
		for n := 1; n <= len(parts); n++ {
			whole = append(whole, parts[n]...)
		}
		f.objects[key] = whole
		delete(f.uploads, q.Get("uploadId"))
		io.WriteString(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>")
			return
		}
		w.Write(data)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestBucket(t *testing.T) {
	fake := &fakeBucket{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	b := &Bucket{Endpoint: server.URL, Name: "bucket", PathStyle: true, AccessKey: "a", SecretKey: "s"}
	ctx := context.Background()

	data := bytes.Repeat([]byte("0123456789abcdef"), (PartSize*2+1000)/16)
	var seen int64
	size, sum, err := b.Upload(ctx, "patai/one"+Extension, bytes.NewReader(data), func() error { return nil }, func(n int64) { seen = n })
	if err != nil || size != int64(len(data)) || seen != size || len(sum) != 64 {
		t.Fatalf("upload: %d %s %v", size, sum, err)
	}
	if !bytes.Equal(fake.objects["patai/one"+Extension], data) {
		t.Fatal("the object isn't what was sent")
	}
	if err := b.Put(ctx, "patai/one.json", []byte("{}"), "application/json"); err != nil {
		t.Fatal(err)
	}
	list, err := b.List(ctx, "patai/")
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	if got, err := b.Read(ctx, "patai/one.json", 1<<10); err != nil || string(got) != "{}" {
		t.Fatalf("read %q %v", got, err)
	}
	if _, err := b.Read(ctx, "patai/none", 10); err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Fatalf("missing: %v", err)
	}

	// A stream that turns out bad leaves nothing.
	if _, _, err := b.Upload(ctx, "patai/two"+Extension, bytes.NewReader(data[:100]), func() error { return errors.New("bad") }, nil); err == nil {
		t.Fatal("a bad stream was finished")
	}
	if _, ok := fake.objects["patai/two"+Extension]; ok || len(fake.uploads) != 0 {
		t.Fatal("a bad stream left something")
	}
	if err := b.Delete(ctx, "patai/one.json"); err != nil {
		t.Fatal(err)
	}
}

