package backup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/webdav"
)

func TestWebDAV(t *testing.T) {
	dir := t.TempDir()
	dav := &webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	var chunked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "backup" || password != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPut && r.ContentLength == -1 {
			chunked.Store(true)
		}
		dav.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	checkTarget(t, &WebDAV{URL: server.URL + "/back%20ups/unit", User: "backup", Password: "secret"}, 3<<20+1000)
	if _, err := os.Stat(filepath.Join(dir, "back ups/unit/one"+Extension)); err != nil || !chunked.Load() {
		t.Fatal("not where it should be, or not sent in chunks:", err, chunked.Load())
	}
	_, err := (&WebDAV{URL: server.URL + "/back%20ups/unit", User: "backup", Password: "wrong"}).List(context.Background())
	if err == nil || err.Error() != "the user name or password was refused" {
		t.Fatal("a wrong password:", err)
	}
	for _, bad := range []string{"ftp://x/", "http:///x", "https://u:p@host/x", "https://host/x?y"} {
		if _, err := (&WebDAV{URL: bad}).List(context.Background()); err == nil || !strings.Contains(err.Error(), "address") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// TestWebDAVServer runs against a real server, PATAI_WEBDAV_TEST=url,user,password, with the URL of a folder that
// isn't there yet.
func TestWebDAVServer(t *testing.T) {
	env := os.Getenv("PATAI_WEBDAV_TEST")
	if env == "" {
		t.Skip("PATAI_WEBDAV_TEST isn't set")
	}
	f := strings.Split(env, ",")
	checkTarget(t, &WebDAV{URL: f[0], User: f[1], Password: f[2]}, 20<<20+1000)
	if _, err := (&WebDAV{URL: f[0], User: f[1], Password: "wrong"}).List(context.Background()); err == nil ||
		err.Error() != "the user name or password was refused" {
		t.Fatal("a wrong password:", err)
	}
}
