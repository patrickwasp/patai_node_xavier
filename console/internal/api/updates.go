package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/updates"
)

const (
	// A release is under 1 GB.
	maxBundle = 2 << 30
	// What an upload must leave free, of memory and of /run, which plenty else depends on.
	keepFree = 512 << 20
	// A dropped connection shouldn't keep the one upload slot, or the memory, for long.
	uploadStall = time.Minute
)

func (s *Server) readUpdates(w http.ResponseWriter, r *http.Request) (updates.Status, bool) {
	var status updates.Status
	output, ok := s.ask(w, r, rootd.Request{Verb: "update-status"})
	if !ok {
		return status, false
	}
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say how its updates are")
		return status, false
	}
	return status, true
}

func (s *Server) getUpdates(w http.ResponseWriter, r *http.Request, p *Principal) {
	if output, ok := s.ask(w, r, rootd.Request{Verb: "update-status"}); ok {
		writeJSON(w, http.StatusOK, json.RawMessage(output))
	}
}

func (s *Server) checkUpdates(w http.ResponseWriter, r *http.Request, p *Principal) {
	s.startJob(w, r, p, "update-check", "channel")
}

// installUpdate takes only what the owner's channel is offering, and only the release the person was
// shown. That's why it's for operators and tokens, with no password: it can't choose anything.
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	status, ok := s.readUpdates(w, r)
	if !ok || !s.free(w, status) {
		return
	}
	if status.Check == nil || status.Check.Offered != body.Version {
		fail(w, http.StatusConflict, "not-offered", "the channel isn't offering that release now")
		return
	}
	s.startJob(w, r, p, "update-install", body.Version)
}

func (s *Server) free(w http.ResponseWriter, status updates.Status) bool {
	if status.Busy {
		fail(w, http.StatusConflict, "update-running", "an update is already running")
		return false
	}
	return true
}

// saveUpdateSettings is the owner choosing where this unit's software comes from, so it takes a fresh
// password. A channel can only offer what's signed, but it can offer an older release.
func (s *Server) saveUpdateSettings(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		updates.Settings
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	if _, ok := s.ask(w, r, rootd.Request{Verb: "update-settings", Settings: &body.Settings}); !ok {
		return
	}
	target := "no channel"
	if body.Channel != "" {
		target = strings.TrimSpace(body.ChannelName) + " at " + strings.TrimSpace(body.Channel)
	}
	target += fmt.Sprintf(", install by itself %t, restart by itself %t", body.AutoInstall, body.AutoRestart)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "update-settings", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadBundle takes a bundle and has it checked, and installs nothing. It's held in memory, so a unit
// needs no NVMe and a power cut leaves nothing behind, and there's only ever one. Only a session gets
// here, because tokens stop at operator. The body is the file, so that the password goes with the
// request that installs it, not with a request lasting minutes.
func (s *Server) uploadBundle(w http.ResponseWriter, r *http.Request, p *Principal) {
	size := r.ContentLength
	if size <= 0 {
		fail(w, http.StatusLengthRequired, "no-length", "the upload has to say how big it is")
		return
	}
	if size > maxBundle {
		fail(w, http.StatusRequestEntityTooLarge, "too-big", "that file is too big to be an update")
		return
	}
	if !s.uploading.TryLock() {
		fail(w, http.StatusConflict, "upload-running", "another upload is running")
		return
	}
	defer s.uploading.Unlock()
	if status, ok := s.readUpdates(w, r); !ok || !s.free(w, status) {
		return
	}
	if !roomFor(filepath.Dir(s.UploadPath), size) {
		fail(w, http.StatusInsufficientStorage, "no-room",
			"there isn't enough free memory for that file: stop the workload, then try again")
		return
	}
	if err := receive(w, r, s.UploadPath, size); err != nil {
		log.Printf("upload: %v", err)
		os.Remove(s.UploadPath)
		fail(w, http.StatusBadRequest, "upload-failed", "the file didn't arrive whole")
		return
	}
	output, ok := s.ask(w, r, rootd.Request{Verb: "update-upload-verify"})
	if !ok {
		os.Remove(s.UploadPath)
		if err := s.Store.AddAudit(s.audit(r, p.Name, "update-file", "", "refused")); err != nil {
			log.Printf("audit: %v", err)
		}
		return
	}
	var upload updates.Upload
	if err := json.Unmarshal([]byte(output), &upload); err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't say what that file is")
		return
	}
	if err := s.Store.AddAudit(s.audit(r, p.Name, "update-file", upload.Version, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, upload)
}

// receive writes exactly size bytes to path. It keeps the connection's read deadline moving, because a
// slow phone may need minutes while a connection that has died should end soon.
func receive(w http.ResponseWriter, r *http.Request, path string, size int64) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return receiveInto(w, r, out, size)
}

// receiveInto is receive into a file already open, which it closes.
func receiveInto(w http.ResponseWriter, r *http.Request, out *os.File, size int64) error {
	defer out.Close()
	control := http.NewResponseController(w)
	buffer := make([]byte, 1<<20)
	body := io.LimitReader(r.Body, size)
	var written int64
	for {
		// Not every connection has a deadline to set, and that's not a failure.
		_ = control.SetReadDeadline(time.Now().Add(uploadStall))
		n, err := body.Read(buffer)
		if n > 0 {
			if _, err := out.Write(buffer[:n]); err != nil {
				return err
			}
			written += int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if written != size {
		return fmt.Errorf("got %d of %d bytes", written, size)
	}
	return out.Close()
}

func roomFor(dir string, size int64) bool {
	return roomOnDisk(dir, size) && memoryAvailable()-size >= keepFree
}

// roomOnDisk is roomFor a file that goes to the disk, not to memory.
func roomOnDisk(dir string, size int64) bool {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(dir, &disk); err != nil {
		return false
	}
	return int64(disk.Bavail)*int64(disk.Bsize)-size >= keepFree
}

func memoryAvailable() int64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func (s *Server) discardBundle(w http.ResponseWriter, r *http.Request, p *Principal) {
	if _, ok := s.ask(w, r, rootd.Request{Verb: "update-upload-discard"}); ok {
		w.WriteHeader(http.StatusNoContent)
	}
}

// installBundle needs the owner and a fresh password. Unlike the channel, a file can be any signed release.
func (s *Server) installBundle(w http.ResponseWriter, r *http.Request, p *Principal) {
	var body struct {
		Version         string `json:"version"`
		CurrentPassword string `json:"current_password"`
	}
	if !readJSON(w, r, &body) || !s.confirmPassword(w, p, body.CurrentPassword) {
		return
	}
	status, ok := s.readUpdates(w, r)
	if !ok || !s.free(w, status) {
		return
	}
	if status.Upload == nil || status.Upload.Version != body.Version {
		fail(w, http.StatusConflict, "no-upload", "that file isn't here any more, so send it again")
		return
	}
	s.startJob(w, r, p, "update-upload", body.Version)
}
