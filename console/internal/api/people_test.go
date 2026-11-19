package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

// A terminal that answers HTTP with what it was sent, and a WebSocket by taking its sign-in and saying it back.
type terminal struct {
	requests chan *http.Request
	signIns  chan map[string]any
}

func (f *terminal) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !isWebSocket(req) {
		f.requests <- req
		io.WriteString(w, `{"cwd": "/home/user"}`)
		return
	}
	conn, buffered, _ := w.(http.Hijacker).Hijack()
	defer conn.Close()
	io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+accept(req.Header.Get("Sec-WebSocket-Key"))+"\r\n\r\n")
	message, err := readText(buffered.Reader)
	if err != nil {
		return
	}
	var signIn map[string]any
	json.Unmarshal(message, &signIn)
	f.signIns <- signIn
	// A server's frame isn't masked.
	conn.Write(append([]byte{0x81, byte(len("hello"))}, "hello"...))
}

func (r *rig) chatWithTerminal(t *testing.T, f *terminal) string {
	t.Helper()
	key := r.chatApp(t, http.NotFoundHandler(), http.NotFoundHandler(), http.NotFoundHandler(), false, "terminal")
	os.WriteFile(filepath.Join(r.dir, "apps", "chat", "manifest.json"), []byte(`{"name": "chat", "title": "Chat", "about": "x",
		"apis": [
		{"name": "chat", "title": "Chat", "service": "llm", "always": true, "routes": ["/v1/chat/completions"]},
		{"name": "terminal", "title": "Terminal", "service": "terminal", "person": true, "routes": ["/terminal/"]}],
		"services": ["pages"], "key": "PATAI_GATEWAY_KEY", "ui": {"port": 8443, "service": "pages"}}`), 0o644)
	backend := httptest.NewServer(f)
	t.Cleanup(backend.Close)
	r.rootd.outputs["person-start"] = `{"address": "` + backend.URL + `", "key": "the-containers-key"}`
	r.s.views = views{}
	return key
}

// computerRequest is a request of the app's to one of a person's computers.
func computerRequest(r *rig, method, path, bearer, who, label, kind string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://172.17.0.1:18000"+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if who != "" {
		req.Header.Set("X-User-Id", who)
	}
	if label != "" {
		req.Header.Set("X-Computer", label)
	}
	if kind != "" {
		req.Header.Set("X-Computer-Kind", kind)
	}
	w := httptest.NewRecorder()
	r.s.AppsHandler().ServeHTTP(w, req)
	return w
}

func nameHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func TestAppsGatewaySendsEachComputerToItsOwn(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 8), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	owner, _ := r.claim()
	ownersKey, _ := r.key(owner, "mine", store.Limits{})
	do := func(bearer, who, label string) *httptest.ResponseRecorder {
		return computerRequest(r, "GET", "/terminal/files/cwd?x=1", bearer, who, label, "")
	}
	want(t, do("", "someone", "chat:1"), 401)
	want(t, do(ownersKey, "someone", "chat:1"), 401)
	want(t, do(key, "", "chat:1"), 400)
	want(t, do(key, "someone", ""), 400)
	want(t, do(key, "someone", "../etc"), 400)
	want(t, do(key, "someone", "chat:1"), 200)
	req := <-f.requests
	if req.URL.Path != "/files/cwd" || req.URL.RawQuery != "x=1" || req.Header.Get("Authorization") != "Bearer the-containers-key" {
		t.Fatalf("%s %v", req.URL, req.Header)
	}
	started := r.rootd.called("person-start")
	if len(started) != 1 || started[0].Target != "terminal/"+nameHash("chat", "someone")+"/"+nameHash("chat", "someone", "chat:1") ||
		started[0].Label != "chat:1" || started[0].Temporary {
		t.Fatalf("%+v", started)
	}
	// Known to run, it isn't started again; another of theirs is.
	want(t, do(key, "someone", "chat:1"), 200)
	<-f.requests
	if len(r.rootd.called("person-start")) != 1 {
		t.Fatal("started again")
	}
	want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", "chat:2", "temporary"), 200)
	<-f.requests
	if started := r.rootd.called("person-start"); len(started) != 2 || !started[1].Temporary {
		t.Fatalf("%+v", started)
	}
}

func TestAPersonRunsThreeComputersAtOnce(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 8), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	for _, label := range []string{"chat:1", "chat:2", "chat:3"} {
		want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", label, ""), 200)
		<-f.requests
	}
	// A fourth stops the one used longest ago.
	want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", "chat:4", ""), 200)
	<-f.requests
	stopped := r.rootd.called("person-stop")
	if len(stopped) != 1 || stopped[0].Target != "terminal/"+nameHash("chat", "someone")+"/"+nameHash("chat", "someone", "chat:1") {
		t.Fatalf("%+v", stopped)
	}
	// Someone else's don't count.
	want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "another", "chat:1", ""), 200)
	<-f.requests
	if len(r.rootd.called("person-stop")) != 1 {
		t.Fatal("stopped another's")
	}
}

func TestAnAppListsAndDeletesAPersonsComputers(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 8), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	r.rootd.outputs["person-list"] = `[{"computer": "x", "label": "chat:1", "kind": "temporary", "made": "2026-11-18T00:00:00Z",
		"used": "2026-11-18T01:00:00Z", "size": 42, "running": true}]`
	listed := computerRequest(r, "GET", "/computers/", key, "someone", "", "")
	want(t, listed, 200)
	if !strings.Contains(listed.Body.String(), `"name":"chat:1","temporary":true`) || !strings.Contains(listed.Body.String(), `"size":42`) {
		t.Fatal(listed.Body.String())
	}
	if got := r.rootd.called("person-list"); len(got) != 1 || got[0].Target != "terminal/"+nameHash("chat", "someone") {
		t.Fatalf("%+v", got)
	}
	want(t, computerRequest(r, "DELETE", "/computers/chat:1", key, "someone", "", ""), 204)
	want(t, computerRequest(r, "DELETE", "/computers/", key, "someone", "", ""), 204)
	deleted := r.rootd.called("person-delete")
	person := "terminal/" + nameHash("chat", "someone")
	if len(deleted) != 2 || deleted[0].Target != person+"/"+nameHash("chat", "someone", "chat:1") || deleted[1].Target != person {
		t.Fatalf("%+v", deleted)
	}
	want(t, computerRequest(r, "DELETE", "/computers/", key, "", "", ""), 400)
	want(t, computerRequest(r, "DELETE", "/computers/../x", key, "someone", "", ""), 400)
	want(t, computerRequest(r, "DELETE", "/computers/", "", "someone", "", ""), 401)
}

// socket opens a WebSocket to the gateway and signs in with token, as the app does, and says what came back.
func socket(t *testing.T, address, token string) string {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(address, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /terminal/api/terminals/abc HTTP/1.1\r\nHost: patai-gateway:18000\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"+
		"X-User-Id: someone\r\nX-Computer: chat:1\r\n\r\n")
	reader := bufio.NewReader(conn)
	answer, err := http.ReadResponse(reader, nil)
	if err != nil || answer.StatusCode != 101 || answer.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("%v %v", answer, err)
	}
	signIn, _ := json.Marshal(map[string]any{"type": "auth", "token": token})
	writeText(conn, signIn)
	head := make([]byte, 2)
	if _, err := io.ReadFull(reader, head); err != nil {
		return ""
	}
	body := make([]byte, head[1]&0x7f)
	io.ReadFull(reader, body)
	return string(body)
}

func TestAppsGatewaySignsAPersonsWebSocketInWithTheirContainersKey(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 4), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	gateway := httptest.NewServer(r.s.AppsHandler())
	t.Cleanup(gateway.Close)
	if got := socket(t, gateway.URL, "not-the-key"); got != "" {
		t.Fatalf("answered %q without the app's key", got)
	}
	if len(r.rootd.called("person-start")) != 0 {
		t.Fatal("started without the app's key")
	}
	if got := socket(t, gateway.URL, key); got != "hello" {
		t.Fatalf("%q", got)
	}
	if signIn := <-f.signIns; signIn["token"] != "the-containers-key" || signIn["type"] != "auth" {
		t.Fatalf("%v", signIn)
	}
}

// Beside a large model the unit has room for one computer: another isn't started, and nothing stops for it until the
// app asks, for one that isn't busy.
func TestNoRoomStopsNothingUntilTheAppAsks(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 8), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	running := 0
	r.rootd.fail = func(req rootd.Request) error {
		switch req.Verb {
		case "person-start":
			if running >= 1 {
				return errors.New("there's no room for another terminal now: it would take what's on to 26.4 GiB")
			}
			running++
		case "person-stop":
			running--
		}
		return nil
	}
	want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", "chat:1", ""), 200)
	<-f.requests
	full := computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", "chat:2", "")
	want(t, full, 503)
	if !strings.Contains(full.Body.String(), `"code":"no_room"`) || strings.Contains(full.Body.String(), "GiB") {
		t.Fatal(full.Body.String())
	}
	if stopped := r.rootd.called("person-stop"); len(stopped) != 0 {
		t.Fatalf("%+v", stopped)
	}
	// Busy while a request to it goes on.
	one := "terminal/" + nameHash("chat", "someone") + "/" + nameHash("chat", "someone", "chat:1")
	r.rootd.outputs["person-list"] = `[{"label": "chat:1", "kind": "kept", "running": true}]`
	r.s.persons.mu.Lock()
	r.s.persons.byName[one].open++
	r.s.persons.mu.Unlock()
	if listed := computerRequest(r, "GET", "/computers/", key, "someone", "", ""); !strings.Contains(listed.Body.String(), `"running":true,"busy":true`) {
		t.Fatal(listed.Body.String())
	}
	want(t, computerRequest(r, "POST", "/computers/chat:1/stop", key, "someone", "", ""), 409)
	r.s.persons.mu.Lock()
	r.s.persons.byName[one].open--
	r.s.persons.mu.Unlock()
	if listed := computerRequest(r, "GET", "/computers/", key, "someone", "", ""); !strings.Contains(listed.Body.String(), `"running":true,"busy":false`) {
		t.Fatal(listed.Body.String())
	}
	want(t, computerRequest(r, "POST", "/computers/chat:1/stop", key, "someone", "", ""), 204)
	if stopped := r.rootd.called("person-stop"); len(stopped) != 1 || stopped[0].Target != one {
		t.Fatalf("%+v", stopped)
	}
	want(t, computerRequest(r, "GET", "/terminal/files/cwd", key, "someone", "chat:2", ""), 200)
	<-f.requests
	want(t, computerRequest(r, "POST", "/computers//stop", key, "someone", "", ""), 400)
}
