package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"patai/console/internal/store"
)

func TestNodeIDIsRandomMadeOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadNodeID(dir)
	if err != nil || !nodeIDForm.MatchString(id) {
		t.Fatalf("%q %v", id, err)
	}
	if again, err := LoadNodeID(dir); err != nil || again != id {
		t.Fatalf("made again: %q %v", again, err)
	}
	// Another unit has another, and a file that isn't one is replaced.
	if other, _ := LoadNodeID(t.TempDir()); other == id {
		t.Fatal("two units, one id")
	}
	os.WriteFile(filepath.Join(dir, "node-id"), []byte("serial 1234\n"), 0o644)
	if fresh, err := LoadNodeID(dir); err != nil || fresh == id || !nodeIDForm.MatchString(fresh) {
		t.Fatalf("%q %v", fresh, err)
	}
}

func TestAppsGatewaySaysWhereTheNodeIsToAnAppsKeyOnly(t *testing.T) {
	r := newRig(t)
	key := r.chatApp(t, http.NotFoundHandler(), http.NotFoundHandler(), http.NotFoundHandler(), false)
	r.s.NodeID = "6f1c1a0e-2d3b-4c5d-8e9f-0a1b2c3d4e5f"
	r.rootd.outputs["network-status"] = `{"ports":[{"id":"gbe1","addresses":["10.4.4.50/24"]},{"id":"gbe2","addresses":[]}]}`
	owner, _ := r.claim()
	ownersKey, _ := r.key(owner, "mine", store.Limits{})
	do := func(method, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://172.17.0.1:18000/node", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		r.s.AppsHandler().ServeHTTP(w, req)
		return w
	}
	want(t, do("GET", ""), 401)
	want(t, do("GET", ownersKey), 401)
	w := do("GET", key)
	want(t, w, 200)
	var got struct {
		ID, Name, Fingerprint string
		Addresses             []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != r.s.NodeID || got.Name != "patai-0001" || got.Fingerprint != "AA:BB" ||
		!slices.Equal(got.Addresses, []string{"patai-0001.local", "10.4.4.50"}) {
		t.Fatalf("%s", w.Body)
	}
	// Only a GET, and nothing it would log.
	want(t, do("POST", key), 404)
	if len(r.logged()) != 0 {
		t.Fatalf("%+v", r.logged())
	}
}
