package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SFTP is a folder on a server reached over SSH: a NAS, or any computer running an SSH server.
type SFTP struct {
	Host     string // host or host:port, port 22 by default
	User     string
	Password string
	Key      string // the unit's own private key, OpenSSH PEM: Password or Key
	HostKey  string // the server's key, one authorized_keys line, as the owner confirmed it
	Folder   string // relative to the login folder, or absolute
}

var (
	errHostKey = errors.New("the server's key isn't the one that was confirmed")
	errScanned = errors.New("scanned")
)

// sftpConn is one connection to the server, for one operation.
type sftpConn struct {
	tcp  net.Conn
	ssh  *ssh.Client
	sftp *sftp.Client
	stop func() bool
}

func (c *sftpConn) Close() error {
	c.stop()
	c.sftp.Close()
	return c.ssh.Close()
}

// handshake opens an SSH connection to host. Once ctx is done, the connection is closed.
func handshake(ctx context.Context, host string, config *ssh.ClientConfig) (net.Conn, *ssh.Client, func() bool, error) {
	addr := withPort(host, "22")
	tcp, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the server didn't answer: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { tcp.Close() })
	tcp.SetDeadline(time.Now().Add(time.Minute))
	conn, chans, reqs, err := ssh.NewClientConn(tcp, addr, config)
	if err != nil {
		stop()
		tcp.Close()
		switch {
		case ctx.Err() != nil:
			return nil, nil, nil, ctx.Err()
		case errors.Is(err, errHostKey), errors.Is(err, errScanned):
			return nil, nil, nil, err
		case strings.Contains(err.Error(), "unable to authenticate"):
			return nil, nil, nil, errors.New("the user name, password or key was refused")
		}
		return nil, nil, nil, fmt.Errorf("the SSH connection failed: %w", err)
	}
	return tcp, ssh.NewClient(conn, chans, reqs), stop, nil
}

func (s *SFTP) dial(ctx context.Context) (*sftpConn, error) {
	if strings.TrimSpace(s.HostKey) == "" {
		return nil, errors.New("the server's key hasn't been confirmed")
	}
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostKey))
	if err != nil {
		return nil, errors.New("the server's key can't be read")
	}
	var auth []ssh.AuthMethod
	if s.Key != "" {
		signer, err := ssh.ParsePrivateKey([]byte(s.Key))
		if err != nil {
			return nil, errors.New("the unit's key can't be read")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if s.Password != "" {
		// Some servers ask for the password only as a keyboard-interactive question.
		answer := func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = s.Password
			}
			return answers, nil
		}
		auth = append(auth, ssh.Password(s.Password), ssh.KeyboardInteractive(answer))
	}
	if len(auth) == 0 {
		return nil, errors.New("there's no password or key to log in with")
	}
	// Ask for the pinned key's kind only, so a server with several keys shows the one that was confirmed.
	algorithms := []string{pinned.Type()}
	if pinned.Type() == ssh.KeyAlgoRSA {
		algorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	config := &ssh.ClientConfig{
		User: s.User,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(key.Marshal(), pinned.Marshal()) {
				return errHostKey
			}
			return nil
		},
		HostKeyAlgorithms: algorithms,
	}
	tcp, client, stop, err := handshake(ctx, s.Host, config)
	if err != nil {
		return nil, err
	}
	files, err := sftp.NewClient(client)
	if err != nil {
		stop()
		client.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("the server doesn't offer SFTP")
	}
	tcp.SetDeadline(time.Time{})
	return &sftpConn{tcp: tcp, ssh: client, sftp: files, stop: stop}, nil
}

// ScanHostKey connects to host only as far as it takes to learn the server's key, for the owner to confirm: as an
// authorized_keys line, and its SHA-256 fingerprint as ssh-keygen shows it.
func ScanHostKey(ctx context.Context, host string) (line, fingerprint string, err error) {
	var key ssh.PublicKey
	config := &ssh.ClientConfig{
		User: "patai",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errScanned
		},
		// OpenSSH's own order, so it's the key the owner's ssh has shown them.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
	}
	_, client, stop, err := handshake(ctx, host, config)
	if err == nil {
		stop()
		client.Close()
	}
	if key == nil {
		if err == nil {
			err = errors.New("the server showed no key")
		}
		return "", "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), ssh.FingerprintSHA256(key), nil
}

// NewSSHKey makes a key for the unit to log in with: the private key in OpenSSH's PEM, and the public key as a line
// for the server's authorized_keys.
func NewSSHKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "patai backup")
	if err != nil {
		return "", "", err
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " patai backup", nil
}

func (s *SFTP) path(name string) string {
	if s.Folder == "" {
		return name
	}
	return path.Join(s.Folder, name)
}

func (s *SFTP) List(ctx context.Context) ([]Object, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	folder := s.Folder
	if folder == "" {
		folder = "."
	}
	infos, err := c.sftp.ReadDirContext(ctx, folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, failed(ctx, fmt.Errorf("the folder couldn't be listed: %w", err))
	}
	var objects []Object
	for _, info := range infos {
		if info.Mode().IsRegular() {
			objects = append(objects, Object{Name: info.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	return objects, nil
}

// Put writes a small file as an upload does, so a cut-off write never leaves half of one.
func (s *SFTP) Put(ctx context.Context, name string, data []byte) error {
	_, _, err := s.Upload(ctx, name, bytes.NewReader(data), func() error { return nil }, nil)
	return err
}

func (s *SFTP) Get(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	c, err := s.dial(ctx)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	f, err := c.sftp.Open(s.path(name))
	var info os.FileInfo
	if err == nil {
		if info, err = f.Stat(); err != nil {
			f.Close()
		}
	}
	if err != nil {
		c.Close()
		cancel()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, errMissing
		}
		return nil, 0, failed(ctx, fmt.Errorf("the file couldn't be opened: %w", err))
	}
	// A big buffer has the client ask for many pieces at once, which a link with a long round trip needs.
	body := &readCloser{Reader: bufio.NewReaderSize(f, 1<<20), close: func() error {
		f.Close()
		return c.Close()
	}}
	return GuardStall(body, cancel), info.Size(), nil
}

func (s *SFTP) Delete(ctx context.Context, name string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.sftp.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(ctx, fmt.Errorf("the file couldn't be deleted: %w", err))
	}
	return nil
}

func (s *SFTP) Upload(ctx context.Context, name string, r io.Reader, done func() error, progress func(int64)) (int64, string, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return 0, "", err
	}
	defer c.Close()
	if s.Folder != "" {
		if err := c.sftp.MkdirAll(s.Folder); err != nil {
			return 0, "", failed(ctx, fmt.Errorf("the folder couldn't be made: %w", err))
		}
	}
	part := s.path(PartName(name))
	f, err := c.sftp.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return 0, "", failed(ctx, fmt.Errorf("the file couldn't be made: %w", err))
	}
	in := newSending(ctx, r, progress, func() { c.tcp.Close() })
	// Many writes in flight at once, which a link with a long round trip needs.
	_, err = f.ReadFromWithConcurrency(in, 0)
	if _, ok := c.sftp.HasExtension("fsync@openssh.com"); ok && err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	err = in.end(ctx, err)
	if err == nil {
		err = done()
	}
	if err == nil {
		if err = c.rename(part, s.path(name)); err != nil {
			err = failed(ctx, fmt.Errorf("the file couldn't take its name: %w", err))
		}
	}
	if err != nil {
		s.discard(c, part)
		return 0, "", err
	}
	return in.result()
}

// rename puts from in to's place, replacing it.
func (c *sftpConn) rename(from, to string) error {
	if _, ok := c.sftp.HasExtension("posix-rename@openssh.com"); ok {
		return c.sftp.PosixRename(from, to)
	}
	if err := c.sftp.Remove(to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return c.sftp.Rename(from, to)
}

// discard removes an upload that won't be finished, over a new connection if this one is gone.
func (s *SFTP) discard(c *sftpConn, part string) {
	if err := c.sftp.Remove(part); err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if c, err := s.dial(ctx); err == nil {
		c.sftp.Remove(part)
		c.Close()
	}
}

// What follows is shared by the SFTP, SMB and WebDAV targets.

// errMissing is what Get says of a file that isn't there. It is fs.ErrNotExist.
var errMissing error = missingError{}

type missingError struct{}

func (missingError) Error() string        { return "that file isn't there" }
func (missingError) Is(target error) bool { return target == fs.ErrNotExist }

// failed is err, unless what made it fail was ctx ending.
func failed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// withPort is host with port added if it has none. host may be an IPv6 address, in brackets or not.
func withPort(host, port string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), port)
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r *readCloser) Close() error { return r.close() }

// sending is an upload's stream on its way to the far end. It counts and hashes what passes, says how far it's got,
// and gives up, through cancel, on a far end that has taken nothing for Stall. Only the time the far end holds a piece
// counts against it: a stream that's slow to give is fine.
type sending struct {
	ctx      context.Context
	r        io.Reader
	progress func(int64)
	timer    *time.Timer

	mu      sync.Mutex
	hash    hash.Hash
	size    int64
	eof     bool
	readErr error // the stream's own failure
	stalled bool
}

func newSending(ctx context.Context, r io.Reader, progress func(int64), cancel func()) *sending {
	s := &sending{ctx: ctx, r: r, progress: progress, hash: sha256.New()}
	s.timer = time.AfterFunc(Stall, func() {
		s.mu.Lock()
		s.stalled = true
		s.mu.Unlock()
		cancel()
	})
	s.timer.Stop()
	return s
}

func (s *sending) Read(p []byte) (int, error) {
	s.timer.Stop()
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	s.mu.Lock()
	s.hash.Write(p[:n])
	s.size += int64(n)
	size := s.size
	s.eof = s.eof || err == io.EOF
	if err != nil && err != io.EOF {
		s.readErr = err
	}
	s.mu.Unlock()
	if n > 0 && s.progress != nil {
		s.progress(size)
	}
	s.timer.Reset(Stall)
	return n, err
}

// end stops watching the far end, and says why sending failed if it did: the stream's own error as it gave it, or
// what went wrong at the far end.
func (s *sending) end(ctx context.Context, err error) error {
	s.timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil:
		return nil
	case s.readErr != nil:
		return s.readErr
	case s.stalled:
		return fmt.Errorf("the server took nothing for %s", Stall)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("the file couldn't be written: %w", err)
}

// whole is whether the stream was read to its end.
func (s *sending) whole() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eof
}

// result is how much was sent, and its SHA-256.
func (s *sending) result() (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size, hex.EncodeToString(s.hash.Sum(nil)), nil
}
