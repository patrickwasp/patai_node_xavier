package api

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestAppsGatewaySendsEachPersonToTheirOwn(t *testing.T) {
	r := newRig(t)
	f := &terminal{requests: make(chan *http.Request, 4), signIns: make(chan map[string]any, 4)}
	key := r.chatWithTerminal(t, f)
	owner, _ := r.claim()
	ownersKey, _ := r.key(owner, "mine", store.Limits{})
	do := func(bearer, who string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://172.17.0.1:18000/terminal/files/cwd?x=1", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if who != "" {
			req.Header.Set("X-User-Id", who)
		}
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}
	want(t, do("", "someone"), 401)
	want(t, do(ownersKey, "someone"), 401)
	want(t, do(key, ""), 400)
	want(t, do(key, "someone"), 200)
	req := <-f.requests
	if req.URL.Path != "/files/cwd" || req.URL.RawQuery != "x=1" || req.Header.Get("Authorization") != "Bearer the-containers-key" {
		t.Fatalf("%s %v", req.URL, req.Header)
	}
	sum := sha256.Sum256([]byte("chat\x00someone"))
	started := r.rootd.called("person-start")
	if len(started) != 1 || started[0].Target != "terminal/"+hex.EncodeToString(sum[:8]) {
		t.Fatalf("%+v", started)
	}
	// Known to run, it isn't started again.
	want(t, do(key, "someone"), 200)
	<-f.requests
	if len(r.rootd.called("person-start")) != 1 {
		t.Fatal("started again")
	}
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
		"X-User-Id: someone\r\n\r\n")
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
