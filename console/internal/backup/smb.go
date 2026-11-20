package backup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strings"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// SMB is a folder in a Windows or Samba share, such as a NAS's, logged in to with NTLM.
type SMB struct {
	Host     string // host or host:port, port 445 by default
	Share    string
	User     string
	Password string
	Domain   string
	Folder   string // inside the share
}

// smbConn is one connection to the share, for one operation.
type smbConn struct {
	tcp     net.Conn
	session *smb2.Session
	share   *smb2.Share
	stop    func() bool
}

func (c *smbConn) Close() error {
	c.stop()
	// A dead link mustn't hold up saying goodbye.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.share.WithContext(ctx).Umount()
	c.session.WithContext(ctx).Logoff()
	return c.tcp.Close()
}

// NTSTATUS codes a server answers with.
const (
	statusLogonFailure    = 0xC000006D
	statusWrongPassword   = 0xC000006A
	statusNoSuchUser      = 0xC0000064
	statusAccountDisabled = 0xC0000072
	statusLockedOut       = 0xC0000234
	statusPasswordExpired = 0xC0000071
	statusBadNetworkName  = 0xC00000CC
)

// smbError is err, said plainly where it can be.
func smbError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var refused *smb2.ResponseError
	if errors.As(err, &refused) {
		switch refused.Code {
		case statusLogonFailure, statusWrongPassword, statusNoSuchUser, statusAccountDisabled, statusLockedOut, statusPasswordExpired:
			return errors.New("the user name or password was refused")
		case statusBadNetworkName:
			return errors.New("there's no share by that name")
		}
	}
	if errors.Is(err, fs.ErrPermission) {
		return errors.New("the share doesn't allow that")
	}
	return err
}

func (s *SMB) dial(ctx context.Context) (*smbConn, error) {
	if s.Host == "" {
		return nil, errors.New("that isn't a server's address")
	}
	if s.Share == "" || strings.ContainsAny(s.Share, `/\`) {
		return nil, errors.New("that isn't a share's name")
	}
	addr := withPort(s.Host, "445")
	host, _, _ := net.SplitHostPort(addr)
	tcp, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("the server didn't answer: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { tcp.Close() })
	d := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: s.User, Password: s.Password, Domain: s.Domain}}
	session, err := d.DialConn(ctx, tcp, host)
	if err != nil {
		stop()
		tcp.Close()
		return nil, smbError(ctx, err)
	}
	share, err := session.WithContext(ctx).Mount(s.Share)
	if err != nil {
		stop()
		session.WithContext(ctx).Logoff()
		tcp.Close()
		return nil, smbError(ctx, err)
	}
	return &smbConn{tcp: tcp, session: session, share: share.WithContext(ctx), stop: stop}, nil
}

// folder is the folder inside the share, with / between its parts, and nothing for the share's top.
func (s *SMB) folder() string {
	return strings.Trim(path.Clean("/"+strings.ReplaceAll(s.Folder, `\`, "/")), "/")
}

func (s *SMB) path(name string) string {
	if folder := s.folder(); folder != "" {
		return folder + "/" + name
	}
	return name
}

func (s *SMB) List(ctx context.Context) ([]Object, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	infos, err := c.share.ReadDir(s.folder())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the folder couldn't be listed: %w", smbError(ctx, err))
	}
	var objects []Object
	for _, info := range infos {
		if !info.IsDir() {
			objects = append(objects, Object{Name: info.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	return objects, nil
}

// Put writes a small file as an upload does, so a cut-off write never leaves half of one.
func (s *SMB) Put(ctx context.Context, name string, data []byte) error {
	_, _, err := s.Upload(ctx, name, bytes.NewReader(data), func() error { return nil }, nil)
	return err
}

func (s *SMB) Get(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	c, err := s.dial(ctx)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	f, err := c.share.Open(s.path(name))
	var info fs.FileInfo
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
		return nil, 0, fmt.Errorf("the file couldn't be opened: %w", smbError(ctx, err))
	}
	body := &readCloser{Reader: bufio.NewReaderSize(f, 1<<20), close: func() error {
		f.Close()
		return c.Close()
	}}
	return GuardStall(body, cancel), info.Size(), nil
}

func (s *SMB) Delete(ctx context.Context, name string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.share.Remove(s.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the file couldn't be deleted: %w", smbError(ctx, err))
	}
	return nil
}

func (s *SMB) Upload(ctx context.Context, name string, r io.Reader, done func() error, progress func(int64)) (int64, string, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return 0, "", err
	}
	defer c.Close()
	if folder := s.folder(); folder != "" {
		if err := c.share.MkdirAll(folder, 0o755); err != nil {
			return 0, "", fmt.Errorf("the folder couldn't be made: %w", smbError(ctx, err))
		}
	}
	part := s.path(PartName(name))
	f, err := c.share.Create(part)
	if err != nil {
		return 0, "", fmt.Errorf("the file couldn't be made: %w", smbError(ctx, err))
	}
	in := newSending(ctx, r, progress, func() { c.tcp.Close() })
	_, err = f.ReadFrom(in)
	if err == nil {
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
		if err = c.share.Rename(part, s.path(name)); err != nil {
			err = fmt.Errorf("the file couldn't take its name: %w", smbError(ctx, err))
		}
	}
	if err != nil {
		s.discard(c, part)
		return 0, "", err
	}
	return in.result()
}

// discard removes an upload that won't be finished, over a new connection if this one is gone.
func (s *SMB) discard(c *smbConn, part string) {
	if err := c.share.Remove(part); err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if c, err := s.dial(ctx); err == nil {
		c.share.Remove(part)
		c.Close()
	}
}
