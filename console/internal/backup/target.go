package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Target is a place backups are kept, and the folder in it they go in: a bucket, a share on a NAS, a folder on a
// server over SFTP or WebDAV, or a disk plugged into the unit (docs/console.md, Backups). A name is a file's in that
// folder, with no slash: a backup, or the record beside it.
type Target interface {
	// List is the files in the folder, not what's in folders under it. A folder that isn't there yet is empty.
	List(ctx context.Context) ([]Object, error)
	// Put writes a small file whole, in place of one by that name.
	Put(ctx context.Context, name string, data []byte) error
	// Get opens a file, and says how big it is. Reading it gives up only once it stops arriving (Stall).
	Get(ctx context.Context, name string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, name string) error
	// Upload writes what r gives as name, and says how big it was and its SHA-256. Before the file takes its name, it
	// asks done whether all it was given was good: until then there's no file by that name, and a failure leaves none.
	Upload(ctx context.Context, name string, r io.Reader, done func() error, progress func(int64)) (int64, string, error)
}

// Object is a file in a target's folder.
type Object struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Stall is how long a transfer may go without a byte before it's given up. A slow link is fine; a dead one isn't.
const Stall = 2 * time.Minute

// FileName is a name a file in a target's folder can have: no folder, nothing hidden, and no part of an upload.
func FileName(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, "/\\\x00") && name[0] != '.' && name != ".."
}

// PartName is the name an upload has until it's whole.
func PartName(name string) string { return "." + name + ".part" }

// ReadFile is a small file, up to limit bytes.
func ReadFile(ctx context.Context, t Target, name string, limit int64) ([]byte, error) {
	body, _, err := t.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("that file is bigger than it should be")
	}
	return data, err
}

// Copy writes what r gives to w, for an upload, and says how much it was and its SHA-256, telling progress as it goes.
func Copy(ctx context.Context, w io.Writer, r io.Reader, progress func(int64)) (int64, string, error) {
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	var size int64
	last := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return size, "", err
		}
		n, readErr := r.Read(buffer)
		if n > 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				return size, "", err
			}
			hash.Write(buffer[:n])
			size += int64(n)
			if progress != nil && time.Since(last) > time.Second {
				progress(size)
				last = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return size, "", readErr
		}
	}
	if progress != nil {
		progress(size)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// TestName is the file Test writes.
const TestName = "patai-test"

// Test lists the folder, then writes, reads back and deletes a small file in it: what a backup needs a target to do.
func Test(ctx context.Context, t Target) error {
	if _, err := t.List(ctx); err != nil {
		return err
	}
	random := make([]byte, 16)
	rand.Read(random)
	if err := t.Put(ctx, TestName, random); err != nil {
		return fmt.Errorf("it can be read but not written: %w", err)
	}
	got, err := ReadFile(ctx, t, TestName, 64)
	if err == nil && string(got) != string(random) {
		err = errors.New("it gave back something else")
	}
	if derr := t.Delete(ctx, TestName); err == nil && derr != nil {
		err = fmt.Errorf("it can't delete, so old backups couldn't go: %w", derr)
	}
	return err
}

// GuardStall closes what's being read, through cancel, once nothing has come for Stall. A read that's cut off that way
// says so.
func GuardStall(body io.ReadCloser, cancel func()) io.ReadCloser {
	g := &stallGuard{body: body, cancel: cancel}
	g.timer = time.AfterFunc(Stall, g.fire)
	return g
}

type stallGuard struct {
	body   io.ReadCloser
	cancel func()
	timer  *time.Timer
	mu     sync.Mutex
	fired  bool
}

func (g *stallGuard) fire() {
	g.mu.Lock()
	g.fired = true
	g.mu.Unlock()
	g.cancel()
}

func (g *stallGuard) Read(p []byte) (int, error) {
	n, err := g.body.Read(p)
	if n > 0 {
		g.timer.Reset(Stall)
	}
	if err != nil && err != io.EOF {
		g.mu.Lock()
		fired := g.fired
		g.mu.Unlock()
		if fired {
			err = fmt.Errorf("nothing arrived for %s", Stall)
		}
	}
	return n, err
}

func (g *stallGuard) Close() error {
	g.timer.Stop()
	err := g.body.Close()
	g.cancel()
	return err
}

// transport is for every target reached over HTTP. It has no limit on a whole request, which a backup of many
// gigabytes over a slow link would pass, only on each step of one: a request's own deadline is its context's.
var transport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 2 * time.Minute,
	ExpectContinueTimeout: time.Second,
	IdleConnTimeout:       time.Minute,
	MaxIdleConnsPerHost:   2,
	ForceAttemptHTTP2:     true,
}

// HTTPClient is a client for targets over HTTP.
func HTTPClient() *http.Client { return &http.Client{Transport: transport} }
