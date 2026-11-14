package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"patai/console/internal/diag"
	"patai/console/internal/rootd"
)

// A support bundle (docs/console.md, Diagnostics) is a ZIP an owner can open and read before sending it to anyone.
// It's made in memory and handed straight over, so nothing is written to the eMMC or kept on the unit. Workload
// output, which is the one place a prompt or an output can be, never goes in, nor do sign-ins.

const (
	bundleJournal = 3 << 20 // the newest this much of each boot's journal
	bundleThermal = 1 << 20
	bundleTime    = 90 * time.Second
	bundleSend    = 2 * time.Minute
)

type bundleFile struct {
	name string
	data []byte
}

type bundle struct {
	files   []bundleFile
	missing []string
	codes   []string
}

func (b *bundle) add(name string, data []byte) {
	b.files = append(b.files, bundleFile{name, []byte(diag.Redact(string(data), b.codes))})
}

func (b *bundle) miss(what string, err error) {
	b.missing = append(b.missing, fmt.Sprintf("%s: %v", what, err))
}

// fromRootd keeps what one of the helper's verbs says, as the file it names.
func (s *Server) fromRootd(ctx context.Context, b *bundle, name, verb string) string {
	ctx, cancel := context.WithTimeout(ctx, rootdWait)
	defer cancel()
	resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: verb})
	if err != nil {
		b.miss(name, err)
		return ""
	}
	b.add(name, indent(resp.Output))
	return resp.Output
}

func indent(raw string) []byte {
	var out bytes.Buffer
	if json.Indent(&out, []byte(raw), "", "  ") != nil {
		return []byte(raw)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// supportBundle is an owner's, logged in: it's what the owner hands to someone else.
func (s *Server) supportBundle(w http.ResponseWriter, r *http.Request, p *Principal) {
	if p.Kind != "session" {
		fail(w, http.StatusForbidden, "forbidden", "a support bundle is for an owner who's logged in")
		return
	}
	if !s.bundling.TryLock() {
		fail(w, http.StatusConflict, "busy", "a support bundle is being made already")
		return
	}
	archive, files, err := s.makeBundle(r.Context())
	// Making it is what takes the unit's time. Sending it to a slow browser mustn't keep the next one waiting.
	s.bundling.Unlock()
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "the bundle couldn't be made")
		return
	}
	target := fmt.Sprintf("%d files, %d KB", files, len(archive)/1024)
	if err := s.Store.AddAudit(s.audit(r, p.Name, "support-bundle", target, "ok")); err != nil {
		s.failStore(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="patai-support.zip"`)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", fmt.Sprint(len(archive)))
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(bundleSend))
	w.WriteHeader(http.StatusOK)
	w.Write(archive)
}

func (s *Server) makeBundle(ctx context.Context) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, bundleTime)
	defer cancel()
	made := time.Now()
	b := &bundle{codes: []string{s.ClaimCode()}}

	if data, err := os.ReadFile(s.ReportPath); err == nil {
		b.add("status.json", indent(string(data)))
	} else {
		b.miss("status.json", err)
	}
	s.fromRootd(ctx, b, "updates.json", "update-status")
	s.fromRootd(ctx, b, "network.json", "network-status")
	s.fromRootd(ctx, b, "disk.json", "disk-status")
	clock := s.fromRootd(ctx, b, "clock.json", "clock-status")
	s.fromRootd(ctx, b, "power-mode.json", "power-status")
	s.fromRootd(ctx, b, "workloads.json", "workload-status")
	var boots []diag.Boot
	if out := s.fromRootd(ctx, b, "boots.json", "log-boots"); out != "" {
		json.Unmarshal([]byte(out), &boots)
	}
	for _, boot := range boots {
		name := map[int]string{0: "journal/this-boot.txt", -1: "journal/boot-before.txt"}[boot.Index]
		if name == "" {
			continue
		}
		page, err := s.readLogs(ctx, diag.Query{Source: "all", Boot: boot.ID, Count: diag.MaxCount})
		if err != nil {
			b.miss(name, err)
			continue
		}
		b.add(name, journalText(page, boot))
	}
	s.addRollbacks(b)
	if data, err := tail(filepath.Join(s.ThermalDir, "thermal.csv"), bundleThermal); err == nil {
		b.add("thermal.csv", data)
	} else if !os.IsNotExist(err) {
		b.miss("thermal.csv", err)
	}

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	readme := s.readme(b, made, clock)
	for _, f := range append([]bundleFile{{"README.txt", readme}}, b.files...) {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: zip.Deflate, Modified: made})
		if err == nil {
			_, err = fw.Write(f.data)
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return archive.Bytes(), len(b.files) + 1, nil
}

// journalText is a boot's journal as a person reads it, the newest bundleJournal of it.
func journalText(page diag.Page, boot diag.Boot) []byte {
	lines := make([]string, len(page.Entries))
	size := 0
	for i, entry := range page.Entries {
		lines[i] = entry.Text() + "\n"
		size += len(lines[i])
	}
	start := 0
	for size > bundleJournal && start < len(lines) {
		size -= len(lines[start])
		start++
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Boot %s, from %s to %s by the unit's clock. Times are UTC. Sign-ins are left out.\n",
		boot.ID, boot.First, boot.Last)
	if start > 0 || page.More {
		out.WriteString("# Older lines are left out.\n")
	}
	for _, line := range lines[start:] {
		out.WriteString(line)
	}
	return []byte(out.String())
}

// A line of journalctl's short format, as the rollback report keeps them: "Nov 03 00:42:10 host ident[pid]: text".
var shortLine = regexp.MustCompile(`^\w{3} [ \d]\d \d\d:\d\d:\d\d \S+ `)

// dropSignIns leaves out the sign-ins' lines. A line that doesn't start like one carries on the line before.
func dropSignIns(text string) string {
	var out strings.Builder
	dropping := false
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if shortLine.MatchString(line) {
			dropping = diag.SignInText.MatchString(line)
		}
		if !dropping {
			out.WriteString(line + "\n")
		}
	}
	return out.String()
}

// addRollbacks keeps why each release that didn't work went back, and the lines saved from its slot.
func (s *Server) addRollbacks(b *bundle) {
	reports, _ := filepath.Glob(filepath.Join(s.ResultsDir, "rollback-*.json"))
	for _, report := range reports {
		base := strings.TrimSuffix(filepath.Base(report), ".json")
		if data, err := readSmall(report, 64<<10); err == nil {
			b.add("rollbacks/"+base+".json", data)
		}
		if data, err := readSmall(filepath.Join(s.ResultsDir, base+".log"), 1<<20); err == nil {
			b.add("rollbacks/"+base+".log", []byte(dropSignIns(string(data))))
		}
	}
}

// readSmall reads a regular file, refusing one bigger than limit.
func readSmall(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%s isn't a small file", filepath.Base(path))
	}
	return os.ReadFile(path)
}

// tail is the last limit bytes of a file, from a line's start.
func tail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a file", filepath.Base(path))
	}
	start := max(0, info.Size()-limit)
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}

func (s *Server) readme(b *bundle, made time.Time, clock string) []byte {
	var c struct {
		Synced bool            `json:"synced"`
		Set    json.RawMessage `json:"set"`
	}
	trust := "may be wrong: no time server has set it, and nobody has this boot"
	if json.Unmarshal([]byte(clock), &c) == nil {
		if c.Synced {
			trust = "a time server keeps"
		} else if len(c.Set) > 0 && string(c.Set) != "null" {
			trust = "its owner set this boot"
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Support bundle from an patai unit\n\n")
	fmt.Fprintf(&out, "Made %s by the unit's clock, which %s.\n", made.UTC().Format("2006-01-02 15:04:05 UTC"), trust)
	fmt.Fprintf(&out, "Release %s, boot %s.\n\n", s.Release, s.BootID)
	out.WriteString(`What's here
  status.json              The unit's own report: release, slots, failed units, temperatures, disk, workload
  updates.json             The update channel, the last check, installs and the slots
  network.json             Each port's setting, addresses and link
  disk.json                The data disk
  clock.json               The clock, and what set it
  power-mode.json          The power mode and the modes this release has
  workloads.json           The workloads, their images and models, and any download
  boots.json               This slot's boots
  journal/this-boot.txt    This boot's journal, newest 3 MiB, with sign-ins left out
  journal/boot-before.txt  The boot before on this slot, the same way
  rollbacks/               Why releases that didn't work went back, and lines saved from their slot
  thermal.csv              Peak temperatures and the fan, a line a minute

What isn't
  What the workloads printed, which can include what people sent them. Who signed in and what they ran with sudo.
  The audit log. The console's database, keys and certificates. Anything shaped like a password, token, key or
  claim code was taken out of the rest.

What says whose unit this is
  The unit's name and serial, its addresses and update channel, and addresses of devices on its network.
`)
	if len(b.missing) > 0 {
		out.WriteString("\nWhat the unit couldn't get\n")
		for _, m := range b.missing {
			out.WriteString("  " + m + "\n")
		}
	}
	return []byte(out.String())
}
