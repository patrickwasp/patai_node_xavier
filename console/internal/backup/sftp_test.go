package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sshServer serves SFTP on dir, to user with password or with the key whose authorized_keys line is authorized. It
// gives its address and its key's line.
func sshServer(t *testing.T, dir, user, password, authorized string) (addr, hostKey string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	allowed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(p) == password {
				return nil, nil
			}
			return nil, os.ErrPermission
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == user && bytes.Equal(key.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, os.ErrPermission
		},
	}
	config.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveSSH(conn, config, dir)
		}
	}()
	return l.Addr().String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func serveSSH(conn net.Conn, config *ssh.ServerConfig, dir string) {
	defer conn.Close()
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range requests {
				if req.Type != "subsystem" || len(req.Payload) < 4 || string(req.Payload[4:]) != "sftp" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				server, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(dir))
				if err == nil {
					server.Serve()
				}
				return
			}
		}()
	}
}

func TestSFTP(t *testing.T) {
	ctx := context.Background()
	private, public, err := NewSSHKey()
	if err != nil || !strings.HasPrefix(private, "-----BEGIN OPENSSH PRIVATE KEY-----") ||
		!strings.HasPrefix(public, "ssh-ed25519 ") || !strings.HasSuffix(public, " patai backup") {
		t.Fatalf("key %q %v", public, err)
	}
	home := t.TempDir()
	addr, hostKey := sshServer(t, home, "backup", "secret", public)

	line, fingerprint, err := ScanHostKey(ctx, addr)
	key, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil || line != hostKey || fingerprint != ssh.FingerprintSHA256(key) {
		t.Fatalf("scanned %q %q %v", line, fingerprint, err)
	}

	t.Run("password", func(t *testing.T) {
		checkTarget(t, &SFTP{Host: addr, User: "backup", Password: "secret", HostKey: line, Folder: "backups/unit"}, 3<<20+1000)
		if _, err := os.Stat(filepath.Join(home, "backups/unit/one"+Extension)); err != nil {
			t.Fatal("not in the login folder:", err)
		}
	})
	t.Run("key", func(t *testing.T) {
		checkTarget(t, &SFTP{Host: addr, User: "backup", Key: private, HostKey: line, Folder: filepath.Join(t.TempDir(), "a/b")}, 5000)
	})
	t.Run("refused", func(t *testing.T) {
		_, other, _ := NewSSHKey()
		otherPrivate, _, _ := NewSSHKey()
		for _, tc := range []struct {
			s    SFTP
			want string
		}{
			{SFTP{Host: addr, User: "backup", Password: "wrong", HostKey: line}, "refused"},
			{SFTP{Host: addr, User: "backup", Key: otherPrivate, HostKey: line}, "refused"},
			{SFTP{Host: addr, User: "backup", Password: "secret", HostKey: other}, "isn't the one that was confirmed"},
			{SFTP{Host: addr, User: "backup", Password: "secret"}, "hasn't been confirmed"},
		} {
			if _, err := tc.s.List(ctx); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%+v: %v", tc.s, err)
			}
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		s := &SFTP{Host: addr, User: "backup", Password: "secret", HostKey: line}
		if _, err := s.List(ctx); err == nil {
			t.Fatal("listed with a cancelled context")
		}
	})
}

// TestSFTPServer runs against a real server, PATAI_SFTP_TEST=host:port,user,password,folder.
func TestSFTPServer(t *testing.T) {
	env := os.Getenv("PATAI_SFTP_TEST")
	if env == "" {
		t.Skip("PATAI_SFTP_TEST isn't set")
	}
	f := strings.Split(env, ",")
	line, fingerprint, err := ScanHostKey(context.Background(), f[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Log("host key", fingerprint)
	checkTarget(t, &SFTP{Host: f[0], User: f[1], Password: f[2], HostKey: line, Folder: f[3]}, 20<<20+1000)
	if _, err := (&SFTP{Host: f[0], User: f[1], Password: "wrong", HostKey: line}).List(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "refused") {
		t.Fatal("a wrong password:", err)
	}
}
