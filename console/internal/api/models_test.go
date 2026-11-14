package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"patai/console/internal/store"
)

func TestModelJobs(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	viewer := r.token(cookie, "v", "viewer")
	operator := r.token(cookie, "o", "operator")
	for i, action := range []string{"use", "fetch", "remove"} {
		path := "/api/v1/workloads/llama-cpp/models/Qwen2.5-1.5B.gguf/" + action
		want(t, r.do("POST", path, nil, opts{bearer: viewer, idem: "v" + action}), 403)
		w := r.do("POST", path, nil, opts{bearer: operator, idem: action})
		want(t, w, 202)
		var job store.Job
		json.Unmarshal(w.Body.Bytes(), &job)
		if job.Kind != "model-"+action || job.Target != "llama-cpp/Qwen2.5-1.5B.gguf" {
			t.Fatalf("%d: %+v", i, job)
		}
	}
	// Names go nowhere near a path, and only an app that serves has models to choose.
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models/..%2Fx/use", nil, opts{bearer: operator, idem: "a"}), 404)
	want(t, r.do("POST", "/api/v1/workloads/yolo/models/x.gguf/use", nil, opts{bearer: operator, idem: "b"}), 404)
	r.rootd.disk = "undecided"
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models/x.gguf/use", nil, opts{bearer: operator, idem: "c"}), 409)
	// Taking one off doesn't need the disk to be in use.
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models/x.gguf/remove", nil, opts{bearer: operator, idem: "d"}), 202)
}

func TestAddingAModelFromAnAddress(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	operator := r.token(cookie, "o", "operator")
	body := map[string]string{"name": "mine.gguf", "url": "https://example.com/mine.gguf"}
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models", body, opts{bearer: operator}), 403)
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models", body, opts{cookie: cookie}), 202)
	calls := r.rootd.called("model-add-url")
	if len(calls) != 1 || calls[0].Target != "llama-cpp/mine.gguf" || calls[0].URL != body["url"] {
		t.Fatalf("%+v", calls)
	}
	for _, bad := range []map[string]string{{"name": "mine.gguf", "url": "file:///etc/passwd"},
		{"name": "mine.gguf", "url": "https://e/" + strings.Repeat("x", 2048)}} {
		want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models", bad, opts{cookie: cookie}), 400)
	}
	want(t, r.do("POST", "/api/v1/workloads/llama-cpp/models", map[string]string{"name": "../x", "url": "https://e/x"},
		opts{cookie: cookie}), 404)
	if len(r.rootd.called("model-add-url")) != 1 {
		t.Fatal("a refused one reached the helper")
	}
}

func (r *rig) uploadModel(cookie, name, body string, length int64) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest("PUT", "https://10.0.0.5/api/v1/workloads/llama-cpp/models/upload/"+name, strings.NewReader(body))
	req.Host = "10.0.0.5"
	req.ContentLength = length
	req.Header.Set("Origin", "https://10.0.0.5")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	w := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(w, req)
	return w
}

func TestUploadingAModel(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	upload := filepath.Join(r.dir, "model")

	want(t, r.uploadModel(cookie, "mine.gguf", "GGUF", -1), 411)
	want(t, r.uploadModel(cookie, "mine.gguf", "GGUF", maxModel+1), 413)
	want(t, r.uploadModel(cookie, "..%2Fmine.gguf", "GGUF", 4), 404)
	want(t, r.uploadModel(cookie, "mine.gguf", "GG", 4), 400)
	if len(r.rootd.called("model-upload-discard")) != 1 {
		t.Fatal("a short upload wasn't discarded")
	}
	want(t, r.uploadModel(cookie, "mine.gguf", "GGUF", 4), 202)
	data, _ := os.ReadFile(upload)
	takes := r.rootd.called("model-upload-take")
	if string(data) != "GGUF" || len(takes) != 1 || takes[0].Target != "llama-cpp/mine.gguf" || takes[0].Upload != "upload-1" {
		t.Fatalf("%q %+v", data, takes)
	}
	// If the helper won't take it, it's discarded.
	r.rootd.refuse, r.rootd.only = "no", "model-upload-take"
	want(t, r.uploadModel(cookie, "mine.gguf", "GGUF", 4), 400)
	if discards := r.rootd.called("model-upload-discard"); len(discards) != 2 || discards[1].Upload != "upload-1" {
		t.Fatal("a refused upload wasn't discarded")
	}
}
