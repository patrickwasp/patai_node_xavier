package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheAddressOpensAnAppTheOwnerPutsInFront(t *testing.T) {
	r := newRig(t)
	pages := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, "chat "+req.URL.Path) })
	r.chatApp(t, nil, nil, pages, false)
	r.s.FrontPath = filepath.Join(r.dir, "front")
	owner, _ := r.claim()
	viewer := r.user(owner, "bo", "viewer")
	front := r.s.Front()
	at := func(path string) string {
		req := httptest.NewRequest("GET", "https://10.0.0.5"+path, nil)
		req.Host = "10.0.0.5"
		w := httptest.NewRecorder()
		front.ServeHTTP(w, req)
		return w.Body.String()
	}

	// The console's, until an owner chooses.
	if got := r.do("GET", "/api/v1/front", nil, opts{cookie: viewer}); got.Code != 200 ||
		!strings.Contains(got.Body.String(), `"app":""`) || !strings.Contains(got.Body.String(), `"admin_port":9443`) {
		t.Fatal(got.Body.String())
	}
	if at("/") != "<html>" {
		t.Fatal(at("/"))
	}
	want(t, r.do("PUT", "/api/v1/front", map[string]string{"app": "chat"}, opts{cookie: viewer}), 403)
	want(t, r.do("PUT", "/api/v1/front", map[string]string{"app": "ai-api"}, opts{cookie: owner}), 400)
	want(t, r.do("PUT", "/api/v1/front", map[string]string{"app": "chat"}, opts{cookie: owner}), 200)

	// Chat's pages at the address, but the AI API and the health check stay the console's.
	if at("/") != "chat /" || at("/c/1") != "chat /c/1" {
		t.Fatal(at("/"), at("/c/1"))
	}
	if strings.HasPrefix(at("/v1/models"), "chat") || strings.HasPrefix(at("/healthz"), "chat") {
		t.Fatal(at("/v1/models"), at("/healthz"))
	}
	// The page on 80 sends people to Chat, and owners to the console's port.
	req := httptest.NewRequest("GET", "http://10.0.0.5/", nil)
	req.Host = "10.0.0.5"
	w := httptest.NewRecorder()
	r.s.Plain().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `Chat is at <a href="https://10.0.0.5/">`) ||
		!strings.Contains(w.Body.String(), `<a href="https://10.0.0.5:9443/">`) {
		t.Fatal(w.Body.String())
	}
	// Kept for the next start.
	if data, _ := os.ReadFile(r.s.FrontPath); string(data) != "chat\n" {
		t.Fatalf("%q", data)
	}

	// With Chat off, the console is there again, so the owner can turn it on.
	r.rootd.outputs["apps-shown"] = `{"chosen": []}`
	r.s.views = views{}
	if at("/") != "<html>" {
		t.Fatal(at("/"))
	}
	want(t, r.do("PUT", "/api/v1/front", map[string]string{"app": ""}, opts{cookie: owner}), 200)
	if data, _ := os.ReadFile(r.s.FrontPath); string(data) != "\n" {
		t.Fatalf("%q", data)
	}
}
