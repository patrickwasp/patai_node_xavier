package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"patai/console/internal/diag"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

func cursorN(i int) string {
	return fmt.Sprintf("s=a7838606f9e64af1abe0af30db30697b;i=%x;b=42a447525ef648fc80bf40d169b2b6de;m=1;t=1;x=1", i)
}

func pageJSON(cursor string, entries ...diag.Entry) string {
	data, _ := json.Marshal(diag.Page{Entries: entries, Cursor: cursor})
	return string(data)
}

func TestLogsByRole(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "olga", "operator")
	viewer := r.user(owner, "vic", "viewer")
	r.rootd.outputs = map[string]string{
		"logs":          pageJSON(cursorN(2), diag.Entry{Cursor: cursorN(2), Message: "hello"}),
		"log-boots":     `[{"id":"42a447525ef648fc80bf40d169b2b6de","index":0}]`,
		"workload-logs": pageJSON("2026-11-03T00:42:12Z", diag.Entry{Cursor: "2026-11-03T00:42:12Z", Message: "ready"}),
	}

	want(t, r.do("GET", "/api/v1/logs", nil, opts{cookie: viewer}), 403)
	want(t, r.do("GET", "/api/v1/logs/all", nil, opts{cookie: viewer}), 403)
	w := r.do("GET", "/api/v1/logs", nil, opts{cookie: operator})
	want(t, w, 200)
	var index struct {
		Sources []logSource `json:"sources"`
		Boots   []diag.Boot `json:"boots"`
	}
	json.Unmarshal(w.Body.Bytes(), &index)
	ids := []string{}
	for _, s := range index.Sources {
		ids = append(ids, s.ID)
	}
	if slices.Contains(ids, diag.SignIns) || !slices.Contains(ids, "kernel") || !slices.Contains(ids, diag.Output) || len(index.Boots) != 1 {
		t.Fatalf("an operator was offered %v and %d boots", ids, len(index.Boots))
	}
	w = r.do("GET", "/api/v1/logs", nil, opts{cookie: owner})
	if !strings.Contains(w.Body.String(), `"sign-ins"`) {
		t.Fatalf("an owner wasn't offered sign-ins: %s", w.Body.String())
	}

	want(t, r.do("GET", "/api/v1/logs/all?count=5000&important=1", nil, opts{cookie: operator}), 200)
	calls := r.rootd.called("logs")
	if q := calls[len(calls)-1].Logs; q == nil || q.Source != "all" || q.Count != pageLines || !q.Important || q.After != "" {
		t.Fatalf("asked the helper for %+v", q)
	}
	want(t, r.do("GET", "/api/v1/logs/sign-ins", nil, opts{cookie: operator}), 403)
	want(t, r.do("GET", "/api/v1/logs/sign-ins", nil, opts{cookie: owner}), 200)
	want(t, r.do("GET", "/api/v1/logs/journal", nil, opts{cookie: owner}), 404)
	want(t, r.do("GET", "/api/v1/logs/all?boot=-1", nil, opts{cookie: owner}), 400)
	want(t, r.do("GET", "/api/v1/logs/output?stack=llama-cpp", nil, opts{cookie: operator}), 200)
	if q := r.rootd.called("workload-logs")[0].Logs; q.Stack != "llama-cpp" || q.Source != diag.Output {
		t.Fatalf("asked the helper for %+v", q)
	}

	// A token for an operator reads them too; a workload's doesn't.
	op := r.token(owner, "script", "operator")
	want(t, r.do("GET", "/api/v1/logs/kernel", nil, opts{bearer: op, noOrigin: true}), 200)
	load, _ := r.key(owner, "load", store.Limits{})
	want(t, r.do("GET", "/api/v1/logs/kernel", nil, opts{bearer: load, noOrigin: true}), 403)
}

// stream opens a follow over real HTTP, since a recorder can't be read while it's written.
func stream(t *testing.T, srv *httptest.Server, path, cookie, lastID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body)
}

// next reads up to the next blank line, an event's end.
func next(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended after %q: %v", block.String(), err)
		}
		if line == "\n" {
			return block.String()
		}
		block.WriteString(line)
	}
}

func TestFollow(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "olga", "operator")
	looks := 0
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		if req.Verb != "logs" {
			return "", false
		}
		looks++
		if looks == 1 {
			// Two lines of the source's, then one that wasn't.
			return pageJSON(cursorN(4), diag.Entry{Cursor: cursorN(2), Message: "one"},
				diag.Entry{Cursor: cursorN(3), Message: "two"}), true
		}
		return pageJSON(req.Logs.After), true
	}
	srv := httptest.NewServer(r.s.Handler())
	defer srv.Close()

	resp, events := stream(t, srv, "/api/v1/logs/console/follow?after="+url.QueryEscape(cursorN(1)), operator, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	if got := next(t, events); got != "retry: 3000\n" {
		t.Fatalf("first: %q", got)
	}
	if got := next(t, events); !strings.HasPrefix(got, "id: "+cursorN(2)+"\nevent: entry\ndata: {") || !strings.Contains(got, `"one"`) {
		t.Fatalf("first line: %q", got)
	}
	next(t, events)
	if got := next(t, events); got != "id: "+cursorN(4)+"\n" {
		t.Fatalf("past the line left out: %q", got)
	}
	calls := r.rootd.called("logs")
	if q := calls[0].Logs; q.After != cursorN(1) || q.Count != followLines || q.Source != "console" {
		t.Fatalf("first look: %+v", q)
	}

	// The same session can't hold more than two, and a reconnect carries on from Last-Event-ID.
	_, second := stream(t, srv, "/api/v1/logs/console/follow?after="+url.QueryEscape(cursorN(1)), operator, cursorN(9))
	next(t, second)
	third, _ := stream(t, srv, "/api/v1/logs/kernel/follow?after="+url.QueryEscape(cursorN(1)), operator, "")
	if third.StatusCode != 429 {
		t.Fatalf("a third stream got %d", third.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(r.rootd.called("logs"), func(c rootd.Request) bool { return c.Logs.After == cursorN(9) }) {
		if time.Now().After(deadline) {
			t.Fatal("the reconnect didn't carry on from Last-Event-ID")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Logging out ends both.
	want(t, r.do("DELETE", "/api/v1/session", nil, opts{cookie: operator}), 204)
	for _, reader := range []*bufio.Reader{events, second} {
		for {
			block := next(t, reader)
			if strings.HasPrefix(block, "event: end") {
				if !strings.Contains(block, `"login"`) {
					t.Fatalf("ended with %q", block)
				}
				break
			}
		}
	}

	// Following needs a line to start from, and an operator for this.
	viewer := r.user(owner, "vic", "viewer")
	if resp, _ := stream(t, srv, "/api/v1/logs/console/follow?after="+url.QueryEscape(cursorN(1)), viewer, ""); resp.StatusCode != 403 {
		t.Fatalf("a viewer got %d", resp.StatusCode)
	}
	if resp, _ := stream(t, srv, "/api/v1/logs/console/follow", owner, ""); resp.StatusCode != 400 {
		t.Fatalf("no cursor got %d", resp.StatusCode)
	}
	if resp, _ := stream(t, srv, "/api/v1/logs/console/follow?after=nonsense", owner, ""); resp.StatusCode != 400 {
		t.Fatalf("a bad cursor got %d", resp.StatusCode)
	}
}

func TestSupportBundle(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "olga", "operator")
	r.s.ResultsDir = filepath.Join(r.dir, "report")
	r.s.ThermalDir = filepath.Join(r.dir, "thermal")
	os.MkdirAll(r.s.ResultsDir, 0o755)
	os.MkdirAll(r.s.ThermalDir, 0o755)
	os.WriteFile(r.s.ReportPath, []byte(`{"serial":"1420","note":"Bearer patai_`+strings.Repeat("q", 43)+`"}`), 0o644)
	os.WriteFile(filepath.Join(r.s.ResultsDir, "rollback-2026.11.01-1.json"), []byte(`{"reason":"failed units: x"}`), 0o644)
	os.WriteFile(filepath.Join(r.s.ResultsDir, "rollback-2026.11.01-1.log"), []byte(
		"Nov 03 00:42:10 patai-0001 systemd[1]: Started thing.\n"+
			"Nov 03 00:42:11 patai-0001 sshd[99]: Invalid user hunter2 from 192.168.4.177\n"+
			"    and the rest of it\n"+
			"Nov 03 00:42:12 patai-0001 patai-api[5]: store: disk full\n"), 0o644)
	os.WriteFile(filepath.Join(r.s.ThermalDir, "thermal.csv"), []byte("time,cpu\n1,40\n"), 0o644)
	this, before := "42a447525ef648fc80bf40d169b2b6de", "0123456789abcdef0123456789abcdef"
	r.rootd.outputs = map[string]string{
		"log-boots":    `[{"id":"` + this + `","index":0},{"id":"` + before + `","index":-1},{"id":"` + strings.Repeat("e", 32) + `","index":-2}]`,
		"clock-status": `{"synced":true}`,
	}
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		if req.Verb != "logs" {
			return "", false
		}
		return pageJSON(cursorN(2), diag.Entry{Cursor: cursorN(2), Time: 1793667065509104, Ident: "x",
			Message: "boot " + req.Logs.Boot + " claim " + testCode}), true
	}

	want(t, r.do("POST", "/api/v1/support-bundle", nil, opts{cookie: operator}), 403)
	op := r.token(owner, "script", "operator")
	want(t, r.do("POST", "/api/v1/support-bundle", nil, opts{bearer: op, noOrigin: true}), 403)
	want(t, r.do("POST", "/api/v1/support-bundle", nil, opts{cookie: owner, origin: "https://evil.example"}), 403)
	r.s.bundling.Lock()
	want(t, r.do("POST", "/api/v1/support-bundle", nil, opts{cookie: owner}), 409)
	r.s.bundling.Unlock()

	w := r.do("POST", "/api/v1/support-bundle", nil, opts{cookie: owner})
	want(t, w, 200)
	if w.Header().Get("Content-Type") != "application/zip" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("headers: %v", w.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	names := []string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(data)
		names = append(names, f.Name)
	}
	if names[0] != "README.txt" {
		t.Fatalf("the README isn't first: %v", names)
	}
	for _, name := range []string{"status.json", "updates.json", "network.json", "disk.json", "clock.json", "power-mode.json",
		"workloads.json", "boots.json", "journal/this-boot.txt", "journal/boot-before.txt",
		"rollbacks/rollback-2026.11.01-1.json", "rollbacks/rollback-2026.11.01-1.log", "thermal.csv"} {
		if _, ok := files[name]; !ok {
			t.Errorf("no %s in %v", name, names)
		}
	}
	if len(files) != 14 {
		t.Errorf("only two boots' journals: %v", names)
	}
	if !strings.Contains(files["journal/boot-before.txt"], "boot "+before) || !strings.Contains(files["journal/this-boot.txt"], "boot "+this) {
		t.Fatal("each boot's journal isn't its own")
	}
	all := strings.Join(slices.Collect(func(yield func(string) bool) {
		for _, data := range files {
			yield(data)
		}
	}), "\n")
	for _, secret := range []string{testCode, "patai_" + strings.Repeat("q", 43), "hunter2", "and the rest of it"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q is in the bundle", secret)
		}
	}
	if !strings.Contains(files["rollbacks/rollback-2026.11.01-1.log"], "store: disk full") ||
		!strings.Contains(files["rollbacks/rollback-2026.11.01-1.log"], "Started thing.") {
		t.Fatalf("the rollback lines lost more than sign-ins: %q", files["rollbacks/rollback-2026.11.01-1.log"])
	}
	if !strings.Contains(files["README.txt"], "a time server keeps") || !strings.Contains(files["status.json"], `"serial": "1420"`) {
		t.Fatalf("README: %s\nstatus: %s", files["README.txt"], files["status.json"])
	}
	for _, c := range r.rootd.called("logs") {
		if c.Logs.Source != "all" || c.Logs.Count != diag.MaxCount {
			t.Fatalf("the bundle asked for %+v", c.Logs)
		}
	}
	w = r.do("GET", "/api/v1/audit", nil, opts{cookie: owner})
	if !strings.Contains(w.Body.String(), `"support-bundle"`) {
		t.Fatal("the bundle isn't in the audit log")
	}

	// What the helper couldn't give is said in the README, and the rest still comes.
	r.rootd.refuse, r.rootd.only = "no network", "network-status"
	w = r.do("POST", "/api/v1/support-bundle", nil, opts{cookie: owner})
	want(t, w, 200)
	zr, _ = zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	rc, _ := zr.File[0].Open()
	readme, _ := io.ReadAll(rc)
	if !strings.Contains(string(readme), "network.json: no network") {
		t.Fatalf("README: %s", readme)
	}
}

func TestJournalTextKeepsTheNewest(t *testing.T) {
	entries := []diag.Entry{}
	for i := range 40000 {
		entries = append(entries, diag.Entry{Time: int64(i), Ident: "x", Message: fmt.Sprintf("line %d %s", i, strings.Repeat("y", 80))})
	}
	text := string(journalText(diag.Page{Entries: entries}, diag.Boot{ID: "b"}))
	if len(text) > bundleJournal+200 || !strings.Contains(text, "line 39999 ") || strings.Contains(text, "line 0 ") ||
		!strings.Contains(text, "Older lines are left out") {
		t.Fatalf("%d bytes", len(text))
	}
}

// A read that takes a while doesn't carry its lines past a logout that came meanwhile.
func TestFollowLogoutDuringARead(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "olga", "operator")
	reading, release := make(chan struct{}), make(chan struct{})
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		if req.Verb != "logs" {
			return "", false
		}
		close(reading)
		<-release
		return pageJSON(cursorN(2), diag.Entry{Cursor: cursorN(2), Message: "after the logout"}), true
	}
	srv := httptest.NewServer(r.s.Handler())
	defer srv.Close()
	_, events := stream(t, srv, "/api/v1/logs/console/follow?after="+url.QueryEscape(cursorN(1)), operator, "")
	next(t, events)
	<-reading
	want(t, r.do("DELETE", "/api/v1/session", nil, opts{cookie: operator}), 204)
	close(release)
	if block := next(t, events); block != "event: end\ndata: \"login\"\n" {
		t.Fatalf("got %q", block)
	}
}

// Over HTTP/2 a write deadline that runs out resets the stream even while nothing is written, so a quiet log
// mustn't be left with one.
func TestFollowStaysOpenWhenQuietOverHTTP2(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	stall := followStall
	followStall = 300 * time.Millisecond
	t.Cleanup(func() { followStall = stall })
	looks := 0
	r.rootd.respond = func(req rootd.Request) (string, bool) {
		if req.Verb != "logs" {
			return "", false
		}
		if looks++; looks == 3 {
			return pageJSON(cursorN(2), diag.Entry{Cursor: cursorN(2), Message: "after a quiet spell"}), true
		}
		return pageJSON(req.Logs.After), true
	}
	srv := httptest.NewUnstartedServer(r.s.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	resp, events := stream(t, srv, "/api/v1/logs/all/follow?after="+url.QueryEscape(cursorN(1)), owner, "")
	if resp.ProtoMajor != 2 {
		t.Fatalf("not HTTP/2: %s", resp.Proto)
	}
	next(t, events)
	if block := next(t, events); !strings.Contains(block, "after a quiet spell") {
		t.Fatalf("got %q", block)
	}
	// Closed before the server, which waits for the stream's handler.
	resp.Body.Close()
}
