package rootd

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The open file crosses the socket, and the caller writes into the helper's file through it.
func TestOpenUpload(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "rootd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	target := filepath.Join(dir, "model")
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadBytes('\n')
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return
		}
		defer file.Close()
		AnswerWithFile(conn.(*net.UnixConn), file, "upload-7")
	}()
	file, id, err := Client{Path: socket}.OpenUpload(context.Background(), 4)
	if err != nil || id != "upload-7" {
		t.Fatal(id, err)
	}
	file.WriteString("GGUF")
	file.Close()
	if data, _ := os.ReadFile(target); string(data) != "GGUF" {
		t.Fatalf("%q", data)
	}
}

func TestOpenUploadRefused(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "rootd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadBytes('\n')
		conn.Write([]byte(`{"ok":false,"error":"the data disk isn't mounted"}` + "\n"))
	}()
	if _, _, err := (Client{Path: socket}).OpenUpload(context.Background(), 4); err == nil || err.Error() != "the data disk isn't mounted" {
		t.Fatal(err)
	}
}

func copying(t *testing.T, size int64, send string, close bool) error {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "model"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	go func() {
		write.WriteString(send)
		if close {
			write.Close()
		}
	}()
	defer write.Close()
	return CopyUpload(file, read, size, 300*time.Millisecond)
}

// Exactly the size it said, then the end.
func TestCopyUpload(t *testing.T) {
	if err := copying(t, 4, "GGUF", true); err != nil {
		t.Fatal(err)
	}
	if err := copying(t, 4, "GG", true); err == nil {
		t.Fatal("short")
	}
	if err := copying(t, 4, "GGUF and more", true); err == nil {
		t.Fatal("more than it said")
	}
	if err := copying(t, 4, "GGUF", false); err == nil {
		t.Fatal("never ended")
	}
	if err := copying(t, 4, "G", false); err == nil {
		t.Fatal("stalled")
	}
}
