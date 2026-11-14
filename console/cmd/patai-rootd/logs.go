package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"patai/console/internal/diag"
	"patai/console/internal/reset"
)

// Reading logs for the console (docs/console.md, Diagnostics). journalctl runs with fixed arguments, at low
// priority, for a bounded time and amount, and a workload's output is read from Docker's file for it. diag decides
// which lines are the source's and takes credentials out before anything goes back over the socket.

const consoleClaim = "/persist/console/claim-code"

// logReaders keeps people looking at logs from taking the unit's CPU and disk from what it's for.
var logReaders = make(chan struct{}, 4)

func takeReader() (func(), error) {
	select {
	case logReaders <- struct{}{}:
		return func() { <-logReaders }, nil
	case <-time.After(2 * time.Second):
		return nil, errors.New("the unit is busy reading logs, so try again")
	}
}

// lowly runs a command behind everything else for CPU and disk.
func lowly(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "nice", append([]string{"-n", "10", "ionice", "-c", "2", "-n", "7", name}, args...)...)
	cmd.Env = append(os.Environ(), "TZ=UTC", "LANG=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=", "PAGER=")
	return cmd
}

// claimCodes are the codes the unit holds now, to be taken out however they were typed.
func claimCodes() []string {
	var codes []string
	if data, err := os.ReadFile(consoleClaim); err == nil {
		codes = append(codes, strings.TrimSpace(string(data)))
	}
	if data, err := os.ReadFile(reset.Marker); err == nil {
		if lines := strings.Split(string(data), "\n"); len(lines) > 1 {
			codes = append(codes, strings.TrimSpace(lines[1]))
		}
	}
	return codes
}

func logBoots() (string, error) {
	release, err := takeReader()
	if err != nil {
		return "", err
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), diag.ReadTime)
	defer cancel()
	out, err := lowly(ctx, "journalctl", "--list-boots", "--no-pager").Output()
	if err != nil {
		return "", errors.New("the journal can't list its boots")
	}
	boots := diag.ReadBoots(string(out))
	if len(boots) > 10 {
		boots = boots[len(boots)-10:]
	}
	slices.Reverse(boots)
	data, err := json.Marshal(boots)
	return string(data), err
}

const journalFields = "__CURSOR,__REALTIME_TIMESTAMP,PRIORITY,SYSLOG_IDENTIFIER,_COMM,_SYSTEMD_UNIT,UNIT,_PID,MESSAGE"

// readLogs gives the newest lines of a source, or what came after a cursor, oldest first either way.
func readLogs(q *diag.Query) (string, error) {
	if q == nil || q.Source == diag.Output || !q.Check() {
		return "", errors.New("bad request")
	}
	source, _ := diag.Find(q.Source)
	release, err := takeReader()
	if err != nil {
		return "", err
	}
	defer release()
	boot := "--boot=0"
	if q.Boot != "" {
		boot = "--boot=" + q.Boot
	}
	args := []string{"--no-pager", "--all", "--output=json", "--output-fields=" + journalFields, boot}
	if q.Important {
		args = append(args, "--priority=warning")
	}
	if q.After != "" {
		args = append(args, "--after-cursor="+q.After)
	} else {
		// One more than it shows says whether there are older lines. A source picked out of everything reads on.
		lines := q.Count + 1
		if source.Scan {
			lines = diag.MaxCount + 1
		}
		args = append(args, "--reverse", "--lines="+strconv.Itoa(lines))
	}
	page, err := journal(append(args, source.Matches()...), source, q.Count)
	if err != nil {
		return "", err
	}
	if q.After == "" {
		slices.Reverse(page.Entries)
		page.Cursor = page.First
		if page.Cursor == "" {
			// Nothing of the source's yet: following starts after the newest line of all.
			all, _ := diag.Find("all")
			if newest, err := journal([]string{"--no-pager", "--output=json", "--output-fields=__CURSOR", boot,
				"--reverse", "--lines=1"}, all, 1); err == nil {
				page.Cursor = newest.First
			}
		}
	}
	data, err := json.Marshal(page)
	return string(data), err
}

func journal(args []string, source diag.Source, count int) (diag.Page, error) {
	ctx, cancel := context.WithTimeout(context.Background(), diag.ReadTime)
	defer cancel()
	cmd := lowly(ctx, "journalctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &capped{buf: &stderr, max: 4096}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return diag.Page{}, err
	}
	if err := cmd.Start(); err != nil {
		return diag.Page{}, err
	}
	page := diag.ReadJournal(out, source, count, claimCodes())
	// Once it has enough, it would otherwise read on to the end of the journal.
	if page.More {
		cmd.Process.Kill()
	}
	io.Copy(io.Discard, out)
	err = cmd.Wait()
	if ctx.Err() != nil {
		page.More = true
		return page, nil
	}
	if err != nil && !page.More {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return diag.Page{}, errors.New(lines[len(lines)-1])
	}
	return page, nil
}

// capped keeps the start of what's written to it, so a command that says too much can't fill memory.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// How far looking for a container may go. Docker made what's there, but the disk has been elsewhere before.
const (
	maxContainers = 256
	maxConfig     = 256 << 10
	findBytes     = 16 << 20
	findTime      = 5 * time.Second
)

// readOutput gives what a workload's server printed, from Docker's own file for it on the data disk: Docker isn't
// asked anything (diag/output.go says why).
func readOutput(q *diag.Query) (string, error) {
	if q == nil || q.Source != diag.Output || !q.Check() {
		return "", errors.New("bad request")
	}
	if err := checkService(q.Stack); err != nil {
		return "", err
	}
	release, err := takeReader()
	if err != nil {
		return "", err
	}
	defer release()
	file, rotated, absent := containerLog(q.Stack)
	if absent != "" {
		data, err := json.Marshal(diag.Page{Entries: []diag.Entry{}, Cursor: q.After, Absent: absent})
		return string(data), err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(diag.ReadLocalLog(file, info.Size(), rotated, q.After, q.Count, claimCodes()))
	return string(data), err
}

// containerLog opens the log of the stack's newest server container, found by its Compose labels in Docker's
// records, and says when Docker last moved lines out of it. absent says why there's none: no disk in use, or no
// container, which stopping a workload removes. Everything is opened within /data, and each container's files
// within its own directory, so no link can take root elsewhere.
func containerLog(stack string) (*os.File, time.Time, string) {
	var never time.Time
	if exec.Command("mountpoint", "-q", "/data").Run() != nil {
		return nil, never, "disk"
	}
	data, err := os.OpenRoot("/data")
	if err != nil {
		return nil, never, "disk"
	}
	defer data.Close()
	containers, err := data.OpenRoot("docker/containers")
	if err != nil {
		return nil, never, "container"
	}
	defer containers.Close()
	list, err := containers.Open(".")
	if err != nil {
		return nil, never, "container"
	}
	defer list.Close()
	var newest string
	var created time.Time
	var examined int
	var read int64
	until := time.Now().Add(findTime)
	for examined < maxContainers && read < findBytes && time.Now().Before(until) {
		entries, err := list.ReadDir(64)
		for _, entry := range entries {
			// A DirEntry is what Lstat says, so a link isn't a directory here.
			if !entry.IsDir() || !containerID.MatchString(entry.Name()) || examined >= maxContainers {
				continue
			}
			examined++
			config, err := readIn(containers, entry.Name()+"/config.v2.json", maxConfig)
			read += int64(len(config))
			if err != nil {
				continue
			}
			var c struct {
				Created time.Time
				Config  struct{ Labels map[string]string }
			}
			if json.Unmarshal(config, &c) != nil || c.Config.Labels["com.docker.compose.project"] != stack ||
				c.Config.Labels["com.docker.compose.service"] != "server" || c.Config.Labels["com.docker.compose.oneoff"] != "False" {
				continue
			}
			if newest == "" || c.Created.After(created) {
				newest, created = entry.Name(), c.Created
			}
		}
		if err != nil {
			break
		}
	}
	if newest == "" {
		return nil, never, "container"
	}
	own, err := containers.OpenRoot(newest)
	if err != nil {
		return nil, never, "container"
	}
	defer own.Close()
	file, err := openIn(own, "local-logs/container.log")
	if err != nil {
		return nil, never, "container"
	}
	return file, rotatedAt(own), ""
}

// rotatedAt is when Docker last moved lines out of the log: the newest older file's time, from gzip's header once
// Docker has compressed it.
func rotatedAt(own *os.Root) time.Time {
	if info, err := own.Lstat("local-logs/container.log.1"); err == nil && info.Mode().IsRegular() {
		return info.ModTime()
	}
	file, err := openIn(own, "local-logs/container.log.1.gz")
	if err != nil {
		return time.Time{}
	}
	defer file.Close()
	header := make([]byte, 10)
	if _, err := io.ReadFull(file, header); err != nil || header[0] != 0x1f || header[1] != 0x8b {
		return time.Time{}
	}
	if stamp := binary.LittleEndian.Uint32(header[4:8]); stamp != 0 {
		return time.Unix(int64(stamp)+1, 0)
	}
	return time.Time{}
}

// openIn opens a regular file for reading within a root, never waiting on a fifo.
func openIn(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("not a regular file")
	}
	return file, nil
}

func readIn(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := openIn(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("too big")
	}
	return data, err
}
