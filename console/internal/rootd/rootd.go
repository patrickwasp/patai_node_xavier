// Package rootd is the protocol between the unprivileged console and the root helper: one JSON line
// each way over a Unix socket, for a fixed list of verbs.
package rootd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"patai/console/internal/backup"
	"patai/console/internal/diag"
	"patai/console/internal/network"
	"patai/console/internal/updates"
)

const Socket = "/run/patai-rootd/rootd.sock"

type Request struct {
	Verb     string            `json:"verb"`
	Target   string            `json:"target,omitempty"` // what it acts on: a stack, a release, a port or a change
	Job      int64             `json:"job,omitempty"`
	KeepData bool              `json:"keep_data,omitempty"`
	Key      string            `json:"key,omitempty"`
	Settings *updates.Settings `json:"settings,omitempty"`
	Network  *network.Setting  `json:"network,omitempty"`
	Logs     *diag.Query       `json:"logs,omitempty"`
	URL      string            `json:"url,omitempty"`    // where an owner's model comes from
	Size     int64             `json:"size,omitempty"`   // how big an upload says it is
	Upload   string            `json:"upload,omitempty"` // which upload a take or a discard is for
	Values   json.RawMessage   `json:"values,omitempty"` // a service's settings to change, {"name": value}
	// One of a person's computers: whether it's temporary, and what the app calls it.
	Temporary bool   `json:"temporary,omitempty"`
	Label     string `json:"label,omitempty"`
	Backup    *Backup `json:"backup,omitempty"`
}

// Backup is what a backup or a restore needs (docs/console.md, Backups). For a backup: the key it's encrypted to and
// sealed with, and the manifest's start, which the helper finishes. For a restore: the owner's key, which the helper
// uses and forgets, the hash of what was sent when it's done, and whether the unit keeps its own name, address,
// certificate and host keys.
type Backup struct {
	Key          *backup.Key      `json:"key,omitempty"`
	Manifest     *backup.Manifest `json:"manifest,omitempty"`
	Owner        string           `json:"owner,omitempty"`
	SHA256       string           `json:"sha256,omitempty"`
	KeepIdentity bool             `json:"keep_identity,omitempty"`
}

type Response struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Caller is what the console needs from the helper. Tests supply their own.
type Caller interface {
	Call(ctx context.Context, req Request) (Response, error)
}

// Uploader is the helper taking an upload of size bytes onto the data disk. What crosses the socket is the write
// end of a pipe: the helper writes the file, so the console needs no way to the disk, and can't send more than it
// said.
type Uploader interface {
	OpenUpload(ctx context.Context, size int64) (pipe *os.File, id string, err error)
}

type Client struct{ Path string }

func (c Client) Call(ctx context.Context, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Minute))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// OpenUpload asks for the pipe a model upload goes into, which comes as a right on the answer.
func (c Client) OpenUpload(ctx context.Context, size int64) (*os.File, string, error) {
	return c.Open(ctx, Request{Verb: "model-upload-open", Size: size})
}

// Opener is the helper answering with one end of a pipe: a backup to read, or a restore to write.
type Opener interface {
	Open(ctx context.Context, req Request) (pipe *os.File, output string, err error)
}

// Open asks for a pipe, which comes as a right on the answer.
func (c Client) Open(ctx context.Context, req Request) (*os.File, string, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, "", err
	}
	conn := raw.(*net.UnixConn)
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	} else {
		conn.SetDeadline(time.Now().Add(time.Minute))
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, "", err
	}
	buffer, rights := make([]byte, 4096), make([]byte, syscall.CmsgSpace(4))
	n, m, _, _, err := conn.ReadMsgUnix(buffer, rights)
	if err != nil {
		return nil, "", err
	}
	var fds []int
	if messages, err := syscall.ParseSocketControlMessage(rights[:m]); err == nil && len(messages) == 1 {
		fds, _ = syscall.ParseUnixRights(&messages[0])
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(buffer[:n]), &resp); err != nil || !resp.OK || len(fds) != 1 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		if resp.Error != "" {
			return nil, "", errors.New(resp.Error)
		}
		return nil, "", errors.New("the helper didn't give a file")
	}
	return os.NewFile(uintptr(fds[0]), req.Verb), resp.Output, nil
}

// CopyUpload copies exactly size bytes from the pipe to the file, then wants the pipe closed: a byte more is a
// failure. A pipe that stalls for longer than stall is one.
func CopyUpload(file, pipe *os.File, size int64, stall time.Duration) error {
	buffer := make([]byte, 1<<20)
	var got int64
	for got < size {
		pipe.SetReadDeadline(time.Now().Add(stall))
		n, err := pipe.Read(buffer[:min(int64(len(buffer)), size-got)])
		if n > 0 {
			if _, err := file.Write(buffer[:n]); err != nil {
				return err
			}
			got += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("got %d of %d bytes", got, size)
		}
		if err != nil {
			return err
		}
	}
	pipe.SetReadDeadline(time.Now().Add(stall))
	if n, err := pipe.Read(buffer[:1]); n > 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("it sent more than the %d bytes it said, or didn't end", size)
	}
	return file.Sync()
}

// AnswerWithFile sends an answer and an open file with it, for OpenUpload.
func AnswerWithFile(conn *net.UnixConn, file *os.File, output string) error {
	data, err := json.Marshal(Response{OK: true, Output: output})
	if err != nil {
		return err
	}
	_, _, err = conn.WriteMsgUnix(append(data, '\n'), syscall.UnixRights(int(file.Fd())), nil)
	return err
}
