package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"patai/console/internal/certs"
	"patai/console/internal/jobs"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

const (
	testCode = "ABCDE-FGHIJ-KLMNO-PQRST"
	password = "correct horse battery"
)

type fakeRootd struct {
	mu        sync.Mutex
	calls     []rootd.Request
	err       error
	disk      string            // the state disk-status reports; ready if empty
	outputs   map[string]string // what a verb answers
	refuse    string            // the reason, if it says no
	only      string            // the one verb it says no to, if not all of them
	respond   func(rootd.Request) (string, bool)
	fail      func(rootd.Request) error // a refusal for this request, if any
	uploadDir string                    // where OpenUpload's file goes
	open      func(rootd.Request) (*os.File, string, error)
}

// Open is the helper's answer with a pipe, for a backup or a restore.
func (f *fakeRootd) Open(ctx context.Context, req rootd.Request) (*os.File, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	open := f.open
	f.mu.Unlock()
	if open == nil {
		return nil, "", errors.New("no pipe here")
	}
	return open(req)
}

func (f *fakeRootd) Call(ctx context.Context, req rootd.Request) (rootd.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.refuse != "" && (f.only == "" || f.only == req.Verb) {
		return rootd.Response{Error: f.refuse}, errors.New(f.refuse)
	}
	if f.fail != nil {
		if err := f.fail(req); err != nil {
			return rootd.Response{Error: err.Error()}, err
		}
	}
	output := f.outputs[req.Verb]
	if req.Verb == "disk-status" {
		state := f.disk
		if state == "" {
			state = "ready"
		}
		output = `{"state":"` + state + `","size":256,"used":0}`
	}
	if req.Verb == "update-status" && output == "" {
		output = "{}"
	}
	if f.respond != nil {
		if answer, ok := f.respond(req); ok {
			output = answer
		}
	}
	return rootd.Response{OK: f.err == nil, Output: output}, f.err
}

// OpenUpload is the helper's file for a model upload, in uploadDir: a file, not a pipe, which is the same to a writer.
func (f *fakeRootd) OpenUpload(ctx context.Context, size int64) (*os.File, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rootd.Request{Verb: "model-upload-open", Size: size})
	file, err := os.OpenFile(filepath.Join(f.uploadDir, "model"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	return file, "upload-1", err
}

func (f *fakeRootd) called(verb string) []rootd.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := []rootd.Request{}
	for _, c := range f.calls {
		if c.Verb == verb {
			found = append(found, c)
		}
	}
	return found
}

type rig struct {
	t      *testing.T
	s      *Server
	dir    string
	rootd  *fakeRootd
	cookie string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	recipes := filepath.Join(dir, "recipes")
	for stack, compose := range map[string]string{"llama-cpp": "services:\n  server:\n    image: x\n", "yolo": "services:\n  bench:\n    image: x\n"} {
		os.MkdirAll(filepath.Join(recipes, stack), 0o755)
		os.WriteFile(filepath.Join(recipes, stack, "compose.yaml"), []byte(compose), 0o644)
		os.WriteFile(filepath.Join(recipes, stack, "manifest.json"), []byte(`{"name": "`+stack+`"}`), 0o644)
	}
	apps := filepath.Join(dir, "apps")
	os.MkdirAll(filepath.Join(apps, "ai-api"), 0o755)
	os.WriteFile(filepath.Join(apps, "ai-api", "manifest.json"), []byte(`{"name": "ai-api", "apis": [
		{"name": "chat", "title": "Chat", "service": "llama-cpp", "always": true, "routes": ["/v1/chat/completions"]},
		{"name": "images", "title": "Images", "service": "comfyui", "routes": ["/v1/images/generations"]}]}`), 0o644)
	claim := filepath.Join(dir, "claim-code")
	os.WriteFile(claim, []byte(testCode+"\n"), 0o600)
	validated := filepath.Join(dir, "validated")
	os.WriteFile(validated, nil, 0o644)
	f := &fakeRootd{}
	unit, err := certs.Load("", "", "patai-0001")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Hostname: "patai-0001", Release: "test", BootID: "boot-1", Mode: ModeNormal, Fingerprint: "AA:BB",
		TLS: certs.NewSelector(unit), OwnerCertPath: filepath.Join(dir, "owner-tls.pem"),
		Store: st, Rootd: f, UI: fstest.MapFS{"index.html": {Data: []byte("<html>")}},
		ReportPath: filepath.Join(dir, "report.json"), ClaimPath: claim, ValidatedPath: validated, RecipesPath: recipes, AppsPath: apps,
		DataPath: dir})
	f.uploadDir = dir
	return &rig{t: t, s: s, dir: dir, rootd: f}
}

type opts struct {
	cookie, bearer, origin, host, idem, remote string
	local                                      string // the unit's address the request came in on
	noOrigin                                   bool
	headers                                    map[string]string
}

func (r *rig) do(method, path string, body any, o opts) *httptest.ResponseRecorder {
	r.t.Helper()
	var reader *strings.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, "https://10.0.0.5"+path, reader)
	req.Host = "10.0.0.5"
	if o.host != "" {
		req.Host = o.host
	}
	if o.remote != "" {
		req.RemoteAddr = o.remote
	}
	if o.local != "" {
		addr, _ := net.ResolveTCPAddr("tcp", o.local)
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
	}
	if !o.noOrigin {
		origin := "https://" + req.Host
		if o.origin != "" {
			origin = o.origin
		}
		req.Header.Set("Origin", origin)
	}
	if o.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: o.cookie})
	}
	if o.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+o.bearer)
	}
	if o.idem != "" {
		req.Header.Set("Idempotency-Key", o.idem)
	}
	for key, value := range o.headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(w, req)
	return w
}

func want(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("got %d, want %d: %s", w.Code, status, w.Body.String())
	}
}

func cookieOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("session cookie is too loose: %+v", c)
			}
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func (r *rig) claim() (cookie string, codes []string) {
	r.t.Helper()
	w := r.do("POST", "/api/v1/claim", map[string]string{"code": testCode, "name": "ada", "password": password}, opts{})
	want(r.t, w, 201)
	var out struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return cookieOf(r.t, w), out.RecoveryCodes
}

func (r *rig) user(cookie, name, role string) string {
	r.t.Helper()
	want(r.t, r.do("POST", "/api/v1/users", map[string]string{"name": name, "role": role, "password": password,
		"current_password": password}, opts{cookie: cookie}), 201)
	w := r.do("POST", "/api/v1/session", map[string]string{"name": name, "password": password}, opts{})
	want(r.t, w, 200)
	return cookieOf(r.t, w)
}

func (r *rig) token(cookie, name, scope string) string {
	r.t.Helper()
	w := r.do("POST", "/api/v1/tokens", map[string]any{"name": name, "scope": scope, "current_password": password}, opts{cookie: cookie})
	want(r.t, w, 201)
	var out struct{ Secret string }
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Secret
}

func TestClaimOnlyWithTheCode(t *testing.T) {
	r := newRig(t)
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": "AAAAA-AAAAA-AAAAA-AAAAA", "name": "eve", "password": password}, opts{}), 403)
	if r.s.State() != "unclaimed" {
		t.Fatal("a wrong code changed the unit")
	}
	cookie, codes := r.claim()
	if len(codes) != recoveryCodes || cookie == "" {
		t.Fatalf("got %d recovery codes", len(codes))
	}
	if _, err := os.Stat(r.s.ClaimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the code outlived the claim")
	}
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": testCode, "name": "eve", "password": password}, opts{}), 409)
}

func TestLowerCaseCodeWithoutDashesWorks(t *testing.T) {
	r := newRig(t)
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": "abcdefghijklmnopqrst", "name": "ada", "password": password}, opts{}), 201)
}

func TestMissingStateIsNeverAClaimableUnit(t *testing.T) {
	r := newRig(t)
	os.Remove(r.s.ClaimPath)
	if got := r.s.State(); got != "unclaimable" {
		t.Fatalf("state %q with no claim code", got)
	}
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": "", "name": "eve", "password": password}, opts{}), 409)
}

func TestLeftoverCodeCantClaimAClaimedUnit(t *testing.T) {
	r := newRig(t)
	r.claim()
	os.WriteFile(r.s.ClaimPath, []byte(testCode), 0o600)
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": testCode, "name": "eve", "password": password}, opts{}), 409)
}

func TestHostChecksStopDNSRebinding(t *testing.T) {
	r := newRig(t)
	for host, status := range map[string]int{
		"evil.example": 421, "10.0.0.5": 200, "[fe80::1]": 200, "patai-0001": 200, "patai-0001.local": 200, "PATAI-0001.LOCAL.": 200,
	} {
		want(t, r.do("GET", "/api/identity", nil, opts{host: host, noOrigin: true}), status)
	}
}

func TestAnotherSiteCantUseASession(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: cookie, origin: "https://evil.example", idem: "1"}), 403)
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: cookie, noOrigin: true, idem: "2"}), 403)
	want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": password}, opts{origin: "https://evil.example"}), 403)
}

func TestRoles(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "op", "operator")
	viewer := r.user(owner, "vi", "viewer")

	want(t, r.do("GET", "/api/v1/status", nil, opts{}), 401)
	want(t, r.do("GET", "/api/v1/status", nil, opts{cookie: viewer}), 200)
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: viewer, idem: "a"}), 403)
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: operator, idem: "b"}), 202)
	want(t, r.do("GET", "/api/v1/users", nil, opts{cookie: operator}), 403)
	want(t, r.do("GET", "/api/v1/audit", nil, opts{cookie: operator}), 403)
	want(t, r.do("GET", "/api/v1/audit", nil, opts{cookie: owner}), 200)
}

func TestTokensCantManageTheUnit(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	op := r.token(owner, "ci", "operator")
	load, _ := r.key(owner, "app", store.Limits{})

	want(t, r.do("POST", "/api/v1/reboot", nil, opts{bearer: op, noOrigin: true, idem: "t"}), 202)
	for _, path := range []string{"/api/v1/users", "/api/v1/tokens", "/api/v1/audit"} {
		want(t, r.do("GET", path, nil, opts{bearer: op, noOrigin: true}), 403)
	}
	want(t, r.do("GET", "/api/v1/status", nil, opts{bearer: load, noOrigin: true}), 403)
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{bearer: load, noOrigin: true, idem: "u"}), 403)
	want(t, r.do("POST", "/api/v1/tokens", map[string]any{"name": "x", "scope": "owner", "current_password": password}, opts{cookie: owner}), 400)
	want(t, r.do("GET", "/api/v1/status", nil, opts{bearer: "patai_nonsense", noOrigin: true}), 401)
}

func TestRevokedAndExpiredTokensStop(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	secret := r.token(owner, "ci", "viewer")
	list, _ := r.s.Store.Tokens()
	want(t, r.do("DELETE", "/api/v1/tokens/"+itoa(list[0].ID), nil, opts{cookie: owner}), 204)
	want(t, r.do("GET", "/api/v1/status", nil, opts{bearer: secret, noOrigin: true}), 401)

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	r.s.Store.CreateToken(store.Token{Name: "old", Scope: "viewer", CreatedBy: "ada", ExpiresAt: &past}, hashOf("patai_old"), store.Audit{})
	want(t, r.do("GET", "/api/v1/status", nil, opts{bearer: "patai_old", noOrigin: true}), 401)
}

func TestLoginThrottles(t *testing.T) {
	r := newRig(t)
	r.claim()
	for range 5 {
		want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": "wrong password"}, opts{}), 401)
	}
	want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": password}, opts{}), 429)
}

func TestRecoveryCodeWorksOnceAndEndsSessions(t *testing.T) {
	r := newRig(t)
	cookie, codes := r.claim()
	newPassword := "another fine password"
	want(t, r.do("POST", "/api/v1/recover", map[string]string{"name": "ada", "code": "AAAAA-AAAAA-AAAAA-AAAAA", "password": newPassword}, opts{}), 403)
	want(t, r.do("POST", "/api/v1/recover", map[string]string{"name": "ada", "code": codes[0], "password": newPassword}, opts{}), 204)
	want(t, r.do("GET", "/api/v1/status", nil, opts{cookie: cookie}), 401)
	want(t, r.do("POST", "/api/v1/recover", map[string]string{"name": "ada", "code": codes[0], "password": password}, opts{}), 403)
	want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": newPassword}, opts{}), 200)
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	r := newRig(t)
	one, _ := r.claim()
	w := r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": password}, opts{})
	two := cookieOf(t, w)
	want(t, r.do("POST", "/api/v1/me/password", map[string]string{"current_password": "wrong", "new_password": "a new password"}, opts{cookie: one}), 403)
	want(t, r.do("POST", "/api/v1/me/password", map[string]string{"current_password": password, "new_password": "a new password"}, opts{cookie: one}), 204)
	want(t, r.do("GET", "/api/v1/status", nil, opts{cookie: one}), 200)
	want(t, r.do("GET", "/api/v1/status", nil, opts{cookie: two}), 401)
}

func TestLastOwnerStays(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	users, _ := r.s.Store.Users()
	want(t, r.do("DELETE", "/api/v1/users/"+itoa(users[0].ID), map[string]string{"current_password": password}, opts{cookie: owner}), 409)
}

func TestNothingChangesUntilTheBootPasses(t *testing.T) {
	r := newRig(t)
	os.Remove(r.s.ValidatedPath)
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": testCode, "name": "ada", "password": password}, opts{}), 503)
	os.WriteFile(r.s.ValidatedPath, nil, 0o644)
	owner, _ := r.claim()
	os.Remove(r.s.ValidatedPath)
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: owner, idem: "x"}), 503)
	want(t, r.do("GET", "/api/v1/status", nil, opts{cookie: owner}), 200)
	want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": password}, opts{}), 200)
	want(t, r.do("POST", "/api/v1/recover", map[string]string{"name": "ada", "code": "x", "password": "another fine password"}, opts{}), 503)
}

func TestRecoveryModeShowsANoticeAndNothingElse(t *testing.T) {
	r := newRig(t)
	r.s.Mode, r.s.Fault = ModeRecovery, "/persist isn't mounted"
	r.s.Store = nil
	want(t, r.do("GET", "/api/v1/status", nil, opts{}), 503)
	want(t, r.do("POST", "/api/v1/claim", map[string]string{"code": testCode, "name": "eve", "password": password}, opts{}), 503)
	want(t, r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": password}, opts{}), 503)
	w := r.do("GET", "/api/identity", nil, opts{noOrigin: true})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"state":"recovery"`) {
		t.Fatal(w.Body.String())
	}
}

func TestHealthCheck(t *testing.T) {
	loop := opts{remote: "127.0.0.1:5000", host: "127.0.0.1", noOrigin: true}
	r := newRig(t)
	want(t, r.do("GET", "/healthz", nil, loop), 200)
	want(t, r.do("GET", "/healthz", nil, opts{noOrigin: true}), 404)

	r.rootd.err = errors.New("no helper")
	want(t, r.do("GET", "/healthz", nil, loop), 500)
	r.rootd.err = nil

	// A storage fault passes, so it can't roll back both slots. A bad release must not.
	st := r.s.Store
	r.s.Store, r.s.Mode = nil, ModeRecovery
	want(t, r.do("GET", "/healthz", nil, loop), 200)
	r.s.Mode = ModeFailed
	want(t, r.do("GET", "/healthz", nil, loop), 500)
	r.s.Store, r.s.Mode = st, ModeNormal

	r.s.UI = fstest.MapFS{}
	want(t, r.do("GET", "/healthz", nil, loop), 500)
}

func TestIdempotencyKeyMakesOneJob(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: owner}), 400)
	one := r.do("POST", "/api/v1/reboot", nil, opts{cookie: owner, idem: "same"})
	two := r.do("POST", "/api/v1/reboot", nil, opts{cookie: owner, idem: "same"})
	want(t, one, 202)
	want(t, two, 202)
	var a, b store.Job
	json.Unmarshal(one.Body.Bytes(), &a)
	json.Unmarshal(two.Body.Bytes(), &b)
	if a.ID != b.ID {
		t.Fatalf("two jobs: %d and %d", a.ID, b.ID)
	}
	list, _ := r.s.Store.Jobs(10)
	if len(list) != 1 {
		t.Fatalf("%d jobs", len(list))
	}
}

func TestAppJobsTakeOnlyApps(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	want(t, r.do("POST", "/api/v1/apps/ai-api/start", nil, opts{cookie: owner, idem: "1"}), 202)
	// A service isn't an app.
	want(t, r.do("POST", "/api/v1/apps/llama-cpp/start", nil, opts{cookie: owner, idem: "2"}), 404)
	want(t, r.do("POST", "/api/v1/apps/..%2f..%2fetc/start", nil, opts{cookie: owner, idem: "3"}), 404)
	want(t, r.do("POST", "/api/v1/apps/no-such-app/stop", nil, opts{cookie: owner, idem: "4"}), 404)
	want(t, r.do("POST", "/api/v1/apps/..%2f..%2fetc/stop", nil, opts{cookie: owner, idem: "5"}), 404)
	want(t, r.do("POST", "/api/v1/apps/no-such-app/fetch", nil, opts{cookie: owner, idem: "6"}), 404)
	// An API key is no longer a token the Access page makes.
	want(t, r.do("POST", "/api/v1/tokens", map[string]any{"name": "app", "scope": "workload", "current_password": password},
		opts{cookie: owner}), 400)
}

func TestAPIJobs(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	viewer := r.user(owner, "vi", "viewer")
	w := r.do("POST", "/api/v1/apps/ai-api/apis/images/on", nil, opts{cookie: owner, idem: "1"})
	want(t, w, 202)
	var job store.Job
	json.Unmarshal(w.Body.Bytes(), &job)
	if job.Kind != "app-api-on" || job.Target != "ai-api/images" {
		t.Fatalf("%+v", job)
	}
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/images/off", nil, opts{cookie: owner, idem: "2"}), 202)
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/chat/off", nil, opts{cookie: owner, idem: "3"}), 409)
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/video/on", nil, opts{cookie: owner, idem: "4"}), 404)
	want(t, r.do("POST", "/api/v1/apps/nothing/apis/chat/on", nil, opts{cookie: owner, idem: "5"}), 404)
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/images/on", nil, opts{cookie: viewer, idem: "6"}), 403)
	r.rootd.disk = "undecided"
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/images/on", nil, opts{cookie: owner, idem: "7"}), 409)
	// Off frees memory, and needs nothing from the disk.
	want(t, r.do("POST", "/api/v1/apps/ai-api/apis/images/off", nil, opts{cookie: owner, idem: "8"}), 202)
}

func TestReconcileNeverRunsOldWork(t *testing.T) {
	r := newRig(t)
	st := r.s.Store
	queued, _, _ := st.CreateJob(store.Job{Kind: "reboot", Target: "x", Actor: "a", BootID: "old"}, "q", store.Audit{})
	running, _, _ := st.CreateJob(store.Job{Kind: "reboot", Target: "x", Actor: "a", BootID: "old"}, "r", store.Audit{})
	st.SetJob(running.ID, store.JobRunning, "", "old")
	stopped, _, _ := st.CreateJob(store.Job{Kind: "workload-stop", Target: "x", Actor: "a", BootID: "old"}, "s", store.Audit{})
	st.SetJob(stopped.ID, store.JobRunning, "", "old")

	runner := jobs.New(st, r.rootd, "new-boot")
	if err := runner.Reconcile(); err != nil {
		t.Fatal(err)
	}
	check := func(id int64, state string) {
		t.Helper()
		j, _ := st.Job(id)
		if j.State != state {
			t.Fatalf("job %d is %s, want %s", id, j.State, state)
		}
	}
	check(queued.ID, store.JobFailed)
	check(running.ID, store.JobSucceeded)
	check(stopped.ID, store.JobFailed)
	if len(r.rootd.calls) != 0 {
		t.Fatalf("reconcile ran something: %+v", r.rootd.calls)
	}
}

func TestAuditRecordsWhoDidWhat(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	r.do("POST", "/api/v1/session", map[string]string{"name": "ada", "password": "wrong password"}, opts{})
	r.do("POST", "/api/v1/reboot", nil, opts{cookie: owner, idem: "z"})
	list, _ := r.s.Store.AuditList(10, 0)
	got := map[string]string{}
	for _, a := range list {
		got[a.Action] = a.Actor + ":" + a.Result
		if a.BootID != "boot-1" {
			t.Fatalf("no boot id on %+v", a)
		}
	}
	for action, line := range map[string]string{"claim": "ada:ok", "login": "ada:refused", "reboot": "ada:queued"} {
		if got[action] != line {
			t.Fatalf("%s: got %q, want %q", action, got[action], line)
		}
	}
}

func TestPlainPortOffersNoLogin(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest("GET", "http://10.0.0.5/api/v1/status", nil)
	w := httptest.NewRecorder()
	r.s.Plain().ServeHTTP(w, req)
	want(t, w, 200)
	body := w.Body.String()
	if !strings.Contains(body, "https://10.0.0.5/") || !strings.Contains(body, "AA:BB") {
		t.Fatal(body)
	}
	req.Host = "evil.example"
	w = httptest.NewRecorder()
	r.s.Plain().ServeHTTP(w, req)
	want(t, w, 421)
}

func TestStaticHasNoDirectoryListings(t *testing.T) {
	r := newRig(t)
	r.s.UI = fstest.MapFS{
		"index.html":            {Data: []byte("<html>front")},
		"claim/index.html":      {Data: []byte("<html>claim")},
		"_next/static/chunk.js": {Data: []byte("js")},
	}
	want(t, r.do("GET", "/_next/static/", nil, opts{noOrigin: true}), 404)
	want(t, r.do("GET", "/_next/static/chunk.js", nil, opts{noOrigin: true}), 200)
	w := r.do("GET", "/claim/", nil, opts{noOrigin: true})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), "claim") {
		t.Fatal(w.Body.String())
	}
}

func TestAuditNamesTheUserRemoved(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	r.user(owner, "vi", "viewer")
	u, _ := r.s.Store.UserByName("vi")
	want(t, r.do("DELETE", "/api/v1/users/"+itoa(u.ID), map[string]string{"current_password": password}, opts{cookie: owner}), 204)
	list, _ := r.s.Store.AuditList(1, 0)
	if list[0].Action != "delete-user" || list[0].Target != "vi" {
		t.Fatalf("%+v", list[0])
	}
}

func TestTokenNamesAreUnique(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	r.token(owner, "ci", "viewer")
	want(t, r.do("POST", "/api/v1/tokens", map[string]any{"name": "ci", "scope": "viewer", "current_password": password}, opts{cookie: owner}), 409)
}

func TestWorkloadsWaitForTheDisk(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	for _, state := range []string{"undecided", "foreign", "absent", "unmounted"} {
		r.rootd.disk = state
		w := r.do("POST", "/api/v1/apps/ai-api/start", nil, opts{cookie: cookie, idem: "start-" + state})
		want(t, w, 409)
		if !strings.Contains(w.Body.String(), "disk-not-ready") {
			t.Fatal(w.Body.String())
		}
	}
	// Stopping one has nothing to wait for.
	want(t, r.do("POST", "/api/v1/apps/ai-api/stop", nil, opts{cookie: cookie, idem: "stop"}), 202)
	r.rootd.disk = "ready"
	want(t, r.do("POST", "/api/v1/apps/ai-api/start", nil, opts{cookie: cookie, idem: "start"}), 202)
}

func TestOnlyAnOwnerAnswersForTheDisk(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	viewer := r.user(owner, "viewer", "viewer")
	token := r.token(owner, "script", "operator")
	r.rootd.disk = "undecided"

	want(t, r.do("GET", "/api/v1/disk", nil, opts{cookie: viewer}), 200)
	body := map[string]string{"current_password": password}
	for _, path := range []string{"/api/v1/disk/keep", "/api/v1/disk/erase"} {
		want(t, r.do("POST", path, body, opts{}), 401)
		want(t, r.do("POST", path, body, opts{cookie: operator, idem: "a"}), 403)
		want(t, r.do("POST", path, body, opts{cookie: viewer, idem: "b"}), 403)
		want(t, r.do("POST", path, body, opts{bearer: token, idem: "c"}), 403)
		want(t, r.do("POST", path, map[string]string{"current_password": "wrong password"}, opts{cookie: owner, idem: "d"}), 403)
		want(t, r.do("POST", path, body, opts{cookie: owner, origin: "https://evil.example", idem: "e"}), 403)
	}
	w := r.do("POST", "/api/v1/disk/erase", body, opts{cookie: owner, idem: "f"})
	want(t, w, 202)
	if !strings.Contains(w.Body.String(), `"disk-erase"`) {
		t.Fatal(w.Body.String())
	}
}

func TestADiskInUseHasNothingToAnswer(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	body := map[string]string{"current_password": password}
	r.rootd.disk = "ready"
	want(t, r.do("POST", "/api/v1/disk/keep", body, opts{cookie: owner, idem: "a"}), 409)
	want(t, r.do("POST", "/api/v1/disk/erase", body, opts{cookie: owner, idem: "b"}), 409)
	// There's nothing to keep on a disk that isn't set up, only something to erase.
	r.rootd.disk = "foreign"
	want(t, r.do("POST", "/api/v1/disk/keep", body, opts{cookie: owner, idem: "c"}), 409)
	want(t, r.do("POST", "/api/v1/disk/erase", body, opts{cookie: owner, idem: "d"}), 202)
}

func TestFactoryReset(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	token := r.token(owner, "script", "operator")
	r.rootd.outputs = map[string]string{"factory-reset": "NEXTC-ODEXX-XXXXX-XXXXX"}
	body := map[string]any{"name": "patai-0001", "keep_data": true, "current_password": password}

	want(t, r.do("POST", "/api/v1/factory-reset", body, opts{}), 401)
	want(t, r.do("POST", "/api/v1/factory-reset", body, opts{cookie: operator}), 403)
	want(t, r.do("POST", "/api/v1/factory-reset", body, opts{bearer: token}), 403)
	want(t, r.do("POST", "/api/v1/factory-reset", body, opts{cookie: owner, origin: "https://evil.example"}), 403)
	want(t, r.do("POST", "/api/v1/factory-reset", map[string]any{"name": "patai-0001", "current_password": "wrong password"}, opts{cookie: owner}), 403)
	want(t, r.do("POST", "/api/v1/factory-reset", map[string]any{"name": "patai-0002", "current_password": password}, opts{cookie: owner}), 400)
	if calls := r.rootd.called("factory-reset"); len(calls) != 0 {
		t.Fatalf("got as far as the helper: %+v", calls)
	}

	w := r.do("POST", "/api/v1/factory-reset", body, opts{cookie: owner})
	want(t, w, 202)
	if !strings.Contains(w.Body.String(), "NEXTC-ODEXX-XXXXX-XXXXX") {
		t.Fatal(w.Body.String())
	}
	if calls := r.rootd.called("factory-reset"); len(calls) != 1 || !calls[0].KeepData {
		t.Fatalf("got %+v", calls)
	}

	// The code goes to the person who asked, and nowhere anyone else can read it.
	w = r.do("GET", "/api/v1/audit", nil, opts{cookie: owner})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"factory-reset"`) || strings.Contains(w.Body.String(), "NEXTC") {
		t.Fatal(w.Body.String())
	}
}

func TestFactoryResetSaysWhenTheHelperDoesnt(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	r.rootd.err = errors.New("connection refused")
	body := map[string]any{"name": "patai-0001", "current_password": password}
	w := r.do("POST", "/api/v1/factory-reset", body, opts{cookie: owner})
	want(t, w, 502)
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatal(w.Body.String())
	}
}

func TestSSHKeys(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	r.rootd.outputs = map[string]string{"ssh-keys-list": `[{"id":"abc"}]`, "ssh-key-add": "SHA256:xyz"}
	body := map[string]string{"key": "ssh-ed25519 AAAA ada@laptop", "current_password": password}

	want(t, r.do("GET", "/api/v1/ssh-keys", nil, opts{cookie: operator}), 403)
	want(t, r.do("POST", "/api/v1/ssh-keys", body, opts{cookie: operator}), 403)
	want(t, r.do("POST", "/api/v1/ssh-keys", map[string]string{"key": "x", "current_password": "wrong password"}, opts{cookie: owner}), 403)

	w := r.do("GET", "/api/v1/ssh-keys", nil, opts{cookie: owner})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"abc"`) {
		t.Fatal(w.Body.String())
	}
	want(t, r.do("POST", "/api/v1/ssh-keys", body, opts{cookie: owner}), 204)
	want(t, r.do("DELETE", "/api/v1/ssh-keys/abc", map[string]string{"current_password": password}, opts{cookie: owner}), 204)
	if calls := r.rootd.called("ssh-key-remove"); len(calls) != 1 || calls[0].Key != "abc" {
		t.Fatalf("got %+v", calls)
	}

	r.rootd.refuse = "that isn't an SSH public key this unit accepts"
	w = r.do("POST", "/api/v1/ssh-keys", map[string]string{"key": "nope", "current_password": password}, opts{cookie: owner})
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "isn't an SSH public key") {
		t.Fatal(w.Body.String())
	}
}

func updateStatus(fields string) string {
	return `{"channel":"http://files:8088","auto_install":true,"auto_restart":false,"slots":[],"history":[]` + fields + `}`
}

func (r *rig) offering(version string) {
	r.rootd.outputs = map[string]string{"update-status": updateStatus(`,"check":{"ok":true,"offered":"` + version + `"}`)}
}

func TestUpdatesByRole(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.offering("v2")
	viewer := r.token(cookie, "v", "viewer")
	operator := r.token(cookie, "o", "operator")
	want(t, r.do("GET", "/api/v1/updates", nil, opts{bearer: viewer}), 200)
	want(t, r.do("POST", "/api/v1/updates/check", nil, opts{bearer: viewer, idem: "a"}), 403)
	want(t, r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v2"}, opts{bearer: viewer, idem: "b"}), 403)
	want(t, r.do("POST", "/api/v1/updates/check", nil, opts{bearer: operator, idem: "c"}), 202)
	want(t, r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v2"}, opts{bearer: operator, idem: "d"}), 202)
	// The owner's choices stay the owner's, whatever an operator can do with them.
	settings := map[string]any{"channel": "http://evil", "current_password": password}
	want(t, r.do("PUT", "/api/v1/updates/settings", settings, opts{bearer: operator}), 403)
	want(t, r.do("POST", "/api/v1/updates/upload", nil, opts{bearer: operator}), 403)
	want(t, r.do("POST", "/api/v1/updates/upload/install", map[string]string{"version": "v2", "current_password": password},
		opts{bearer: operator}), 403)
	want(t, r.do("DELETE", "/api/v1/updates/upload", nil, opts{bearer: operator}), 403)
}

func TestInstallTakesOnlyWhatsOffered(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.offering("v2")
	want(t, r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v1"}, opts{cookie: cookie, idem: "a"}), 409)
	r.rootd.outputs = map[string]string{"update-status": updateStatus(`,"busy":true,"check":{"offered":"v2"}`)}
	want(t, r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v2"}, opts{cookie: cookie, idem: "b"}), 409)
	r.offering("v2")
	w := r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v2"}, opts{cookie: cookie, idem: "c"})
	want(t, w, 202)
	var job store.Job
	json.Unmarshal(w.Body.Bytes(), &job)
	if job.Kind != "update-install" || job.Target != "v2" {
		t.Fatalf("%+v", job)
	}
}

func TestUpdateSettingsTakeAPassword(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	body := map[string]any{"channel": "http://files:8088/", "channel_name": "stable", "auto_install": false, "auto_restart": true,
		"current_password": "wrong"}
	want(t, r.do("PUT", "/api/v1/updates/settings", body, opts{cookie: cookie}), 403)
	if len(r.rootd.called("update-settings")) != 0 {
		t.Fatal("a wrong password reached the helper")
	}
	body["current_password"] = password
	want(t, r.do("PUT", "/api/v1/updates/settings", body, opts{cookie: cookie}), 204)
	got := r.rootd.called("update-settings")[0].Settings
	if got.Channel != "http://files:8088/" || got.ChannelName != "stable" || got.AutoInstall || !got.AutoRestart {
		t.Fatalf("%+v", got)
	}
	entries, err := r.s.Store.AuditList(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Action != "update-settings" || !strings.Contains(entries[0].Target, "stable at http://files:8088") {
		t.Fatalf("%+v", entries[0])
	}
}

func (r *rig) upload(cookie, body string, length int64) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest("POST", "https://10.0.0.5/api/v1/updates/upload", strings.NewReader(body))
	req.Host = "10.0.0.5"
	req.ContentLength = length
	req.Header.Set("Origin", "https://10.0.0.5")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	w := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(w, req)
	return w
}

func TestUpload(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.UploadPath = filepath.Join(r.dir, "update.raucb")
	r.rootd.outputs = map[string]string{"update-upload-verify": `{"version":"v9","size":5}`}

	want(t, r.upload(cookie, "bytes", -1), 411)
	want(t, r.upload(cookie, "bytes", 3<<30), 413)
	want(t, r.upload(cookie, "byt", 5), 400)
	if _, err := os.Stat(r.s.UploadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a short upload was kept")
	}

	w := r.upload(cookie, "bytes", 5)
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), "v9") {
		t.Fatal(w.Body.String())
	}
	if data, _ := os.ReadFile(r.s.UploadPath); string(data) != "bytes" {
		t.Fatalf("got %q", data)
	}
	if len(r.rootd.called("update-upload-verify")) != 1 {
		t.Fatal("nobody checked it")
	}
	if len(r.rootd.called("update-upload")) != 0 {
		t.Fatal("an upload alone must install nothing")
	}
}

func TestUploadOnlyOneAtATime(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.UploadPath = filepath.Join(r.dir, "update.raucb")
	r.s.uploading.Lock()
	defer r.s.uploading.Unlock()
	want(t, r.upload(cookie, "bytes", 5), 409)
}

func TestUploadThatDoesntFit(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.UploadPath = filepath.Join(r.dir, "no-such-directory", "update.raucb")
	want(t, r.upload(cookie, "bytes", 5), 507)
}

func TestARefusedUploadGoesAway(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.UploadPath = filepath.Join(r.dir, "update.raucb")
	r.rootd.refuse, r.rootd.only = "that isn't a signed update for this unit", "update-upload-verify"
	w := r.upload(cookie, "bytes", 5)
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "signed update") {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(r.s.UploadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused upload was kept")
	}
	entries, _ := r.s.Store.AuditList(10, 0)
	if entries[0].Action != "update-file" || entries[0].Result != "refused" {
		t.Fatalf("%+v", entries[0])
	}
}

func TestInstallingAFileTakesAPassword(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.rootd.outputs = map[string]string{"update-status": updateStatus(`,"upload":{"version":"v9","size":5}`)}
	body := map[string]string{"version": "v9", "current_password": "wrong"}
	want(t, r.do("POST", "/api/v1/updates/upload/install", body, opts{cookie: cookie, idem: "a"}), 403)
	body["current_password"] = password
	body["version"] = "v8"
	want(t, r.do("POST", "/api/v1/updates/upload/install", body, opts{cookie: cookie, idem: "b"}), 409)
	body["version"] = "v9"
	want(t, r.do("POST", "/api/v1/updates/upload/install", body, opts{cookie: cookie, idem: "c"}), 202)
}

func TestAnUpdateHoldsOffARestartAndAnUpload(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.s.UploadPath = filepath.Join(r.dir, "update.raucb")
	r.rootd.outputs = map[string]string{"update-status": updateStatus(`,"busy":true`)}
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: cookie, idem: "a"}), 409)
	want(t, r.upload(cookie, "bytes", 5), 409)
	if _, err := os.Stat(r.s.UploadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("it took the file anyway")
	}
	r.offering("v2")
	want(t, r.do("POST", "/api/v1/reboot", nil, opts{cookie: cookie, idem: "b"}), 202)
}

func TestDownloadingAWorkload(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.rootd.outputs = map[string]string{"workload-status": `{"disk":"ready","stacks":[]}`}
	viewer := r.token(cookie, "v", "viewer")
	operator := r.token(cookie, "o", "operator")
	want(t, r.do("GET", "/api/v1/workloads/status", nil, opts{bearer: viewer}), 200)
	want(t, r.do("POST", "/api/v1/apps/ai-api/fetch", nil, opts{bearer: viewer, idem: "a"}), 403)
	want(t, r.do("POST", "/api/v1/apps/ai-api/fetch/cancel", nil, opts{bearer: viewer, idem: "b"}), 403)
	// Only an app has something to get ready, and names go nowhere near a path.
	want(t, r.do("POST", "/api/v1/apps/yolo/fetch", nil, opts{bearer: operator, idem: "c"}), 404)
	want(t, r.do("POST", "/api/v1/apps/..%2Fetc/fetch", nil, opts{bearer: operator, idem: "d"}), 404)
	w := r.do("POST", "/api/v1/apps/ai-api/fetch", nil, opts{bearer: operator, idem: "e"})
	want(t, w, 202)
	var job store.Job
	json.Unmarshal(w.Body.Bytes(), &job)
	if job.Kind != "workload-fetch" || job.Target != "ai-api" {
		t.Fatalf("%+v", job)
	}
	want(t, r.do("POST", "/api/v1/apps/ai-api/fetch/cancel", nil, opts{bearer: operator, idem: "f"}), 202)
}

func TestADownloadWaitsForTheDisk(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.rootd.disk = "undecided"
	w := r.do("POST", "/api/v1/apps/ai-api/fetch", nil, opts{cookie: cookie, idem: "a"})
	want(t, w, 409)
	if !strings.Contains(w.Body.String(), "disk-not-ready") {
		t.Fatal(w.Body.String())
	}
	// Stopping one that's running doesn't need the disk to be ready.
	want(t, r.do("POST", "/api/v1/apps/ai-api/fetch/cancel", nil, opts{cookie: cookie, idem: "b"}), 202)
}

func TestWatchingUpdatesDoesntKeepASessionAlive(t *testing.T) {
	for _, path := range []string{"/api/v1/updates", "/api/v1/workloads/status"} {
		if !polling(httptest.NewRequest("GET", "https://10.0.0.5"+path, nil)) {
			t.Fatal("a page polls", path)
		}
	}
}

func TestNoUpdateWorkBeforeTheBootPasses(t *testing.T) {
	r := newRig(t)
	cookie, _ := r.claim()
	r.offering("v2")
	os.Remove(r.s.ValidatedPath)
	want(t, r.do("POST", "/api/v1/updates/install", map[string]string{"version": "v2"}, opts{cookie: cookie, idem: "a"}), 503)
	want(t, r.do("GET", "/api/v1/updates", nil, opts{cookie: cookie}), 200)
}

func TestNetwork(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	viewer := r.token(owner, "v", "viewer")
	ownersToken := r.token(owner, "o", "operator")
	r.rootd.outputs = map[string]string{
		"network-status": `{"ports":[],"trial":null,"last":null,"problem":"","trial_s":300}`,
		"network-try":    `{"id":"0123456789abcdef","port":"gbe2","applied":false,"left_s":302,"urls":[]}`,
	}

	w := r.do("GET", "/api/v1/network", nil, opts{bearer: viewer, local: "192.168.4.26:443"})
	want(t, w, 200)
	var status map[string]any
	json.Unmarshal(w.Body.Bytes(), &status)
	if status["via"] != "192.168.4.26" || status["trial_s"] != 300.0 {
		t.Fatalf("%+v", status)
	}

	setting := map[string]any{"mode": "static", "address": "192.168.250.26/16", "gateway": "192.168.0.1",
		"dns": []string{"192.168.1.2"}, "current_password": password}
	// How everyone reaches the unit is the owner's alone: never a token, an operator or a stale password.
	want(t, r.do("PUT", "/api/v1/network/gbe2", setting, opts{bearer: ownersToken}), 403)
	want(t, r.do("PUT", "/api/v1/network/gbe2", setting, opts{cookie: operator}), 403)
	want(t, r.do("POST", "/api/v1/network/keep", map[string]string{"id": "0123456789abcdef"}, opts{cookie: operator}), 403)
	want(t, r.do("POST", "/api/v1/network/undo", map[string]string{"id": "0123456789abcdef"}, opts{bearer: ownersToken}), 403)
	setting["current_password"] = "wrong"
	want(t, r.do("PUT", "/api/v1/network/gbe2", setting, opts{cookie: owner}), 403)
	setting["current_password"] = password
	want(t, r.do("PUT", "/api/v1/network/eth0", setting, opts{cookie: owner}), 404)
	want(t, r.do("PUT", "/api/v1/network/gbe2", map[string]any{"mode": "static", "address": "192.168.250.26/16\nDNS=1.1.1.1",
		"current_password": password}, opts{cookie: owner}), 400)
	if len(r.rootd.called("network-try")) != 0 {
		t.Fatal("a refused change reached the helper")
	}

	w = r.do("PUT", "/api/v1/network/gbe2", setting, opts{cookie: owner})
	want(t, w, 202)
	if !strings.Contains(w.Body.String(), "0123456789abcdef") {
		t.Fatal(w.Body.String())
	}
	got := r.rootd.called("network-try")[0]
	if got.Target != "gbe2" || got.Network.Address != "192.168.250.26/16" || got.Network.DNS[0] != "192.168.1.2" {
		t.Fatalf("%+v", got)
	}
	entries, _ := r.s.Store.AuditList(10, 0)
	if entries[0].Action != "network-try" ||
		entries[0].Target != "GbE 2: static 192.168.250.26/16, gateway 192.168.0.1, DNS 192.168.1.2 (change 0123456789abcdef)" {
		t.Fatalf("%+v", entries[0])
	}

	want(t, r.do("POST", "/api/v1/network/keep", map[string]string{"id": "--force"}, opts{cookie: owner}), 400)
	want(t, r.do("POST", "/api/v1/network/keep", map[string]string{"id": "0123456789abcdef"}, opts{cookie: owner}), 204)
	if calls := r.rootd.called("network-keep"); len(calls) != 1 || calls[0].Target != "0123456789abcdef" {
		t.Fatalf("%+v", calls)
	}
	r.rootd.refuse, r.rootd.only = "too late: it's going back", "network-undo"
	w = r.do("POST", "/api/v1/network/undo", map[string]string{"id": "0123456789abcdef"}, opts{cookie: owner})
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "too late") {
		t.Fatal(w.Body.String())
	}
}

func TestClock(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	viewer := r.token(owner, "v", "viewer")
	r.rootd.outputs = map[string]string{
		"clock-status": `{"now_ms":946688421000,"synced":false,"set":null}`,
		"clock-set":    `{"now_ms":1793636635000,"synced":false,"set":{"from_ms":946688421000,"to_ms":1793636635000,"ago_s":0}}`,
	}
	want(t, r.do("GET", "/api/v1/clock", nil, opts{bearer: viewer}), 200)

	later := time.Now().Add(time.Hour).UnixMilli()
	body := map[string]any{"time_ms": later, "current_password": password}
	want(t, r.do("PUT", "/api/v1/clock", body, opts{cookie: operator}), 403)
	body["current_password"] = "wrong"
	want(t, r.do("PUT", "/api/v1/clock", body, opts{cookie: owner}), 403)
	body["current_password"] = password
	for _, bad := range []int64{0, -5, 1e15} {
		want(t, r.do("PUT", "/api/v1/clock", map[string]any{"time_ms": bad, "current_password": password}, opts{cookie: owner}), 400)
	}
	r.rootd.outputs["clock-status"] = `{"synced":true}`
	want(t, r.do("PUT", "/api/v1/clock", body, opts{cookie: owner}), 409)
	if len(r.rootd.called("clock-set")) != 0 {
		t.Fatal("a refused setting reached the helper")
	}

	r.rootd.outputs["clock-status"] = `{"synced":false}`
	want(t, r.do("PUT", "/api/v1/clock", body, opts{cookie: owner}), 200)
	calls := r.rootd.called("clock-set")
	if len(calls) != 1 {
		t.Fatalf("%+v", calls)
	}
	// The time the browser sent, and as long again as the password check took.
	set, _ := strconv.ParseInt(calls[0].Target, 10, 64)
	if set < later || set > later+5_000 {
		t.Fatalf("set %d for %d", set, later)
	}
	entries, _ := r.s.Store.AuditList(1, 0)
	if entries[0].Action != "clock-set" || !strings.HasSuffix(entries[0].Target, time.UnixMilli(set).UTC().Format(time.RFC3339)) {
		t.Fatalf("%+v", entries[0])
	}
}

func TestSettingTheClockBackCantReviveAToken(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":false}`, "clock-set": `{}`}
	gone := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	live := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r.s.Store.CreateToken(store.Token{Name: "old", Scope: "viewer", CreatedBy: "ada", ExpiresAt: &gone}, "h1", store.Audit{})
	r.s.Store.CreateToken(store.Token{Name: "new", Scope: "viewer", CreatedBy: "ada", ExpiresAt: &live}, "h2", store.Audit{})
	r.s.Store.CreateToken(store.Token{Name: "forever", Scope: "viewer", CreatedBy: "ada"}, "h3", store.Audit{})

	// Forward first: nothing that has run out can come back that way.
	want(t, r.do("PUT", "/api/v1/clock", map[string]any{"time_ms": time.Now().Add(time.Minute).UnixMilli(),
		"current_password": password}, opts{cookie: owner}), 200)
	if list, _ := r.s.Store.Tokens(); len(list) != 3 {
		t.Fatalf("%d tokens", len(list))
	}
	want(t, r.do("PUT", "/api/v1/clock", map[string]any{"time_ms": time.Now().Add(-2 * time.Hour).UnixMilli(),
		"current_password": password}, opts{cookie: owner}), 200)
	list, _ := r.s.Store.Tokens()
	if len(list) != 2 || list[0].Name != "new" || list[1].Name != "forever" {
		t.Fatalf("%+v", list)
	}
	entries, _ := r.s.Store.AuditList(1, 0)
	if !strings.Contains(entries[0].Target, "removing 1 token(s)") {
		t.Fatalf("%+v", entries[0])
	}
}

func TestPowerMode(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	r.rootd.outputs = map[string]string{
		"power-status": `{"modes":[{"id":0,"name":"MAXN"},{"id":7,"name":"MODE_15W_DESKTOP"}],"default":7,"current":7,"saved":null}`,
		"power-mode":   `{"current":0}`,
	}
	want(t, r.do("GET", "/api/v1/power-mode", nil, opts{cookie: operator}), 200)
	want(t, r.do("PUT", "/api/v1/power-mode", map[string]any{"mode": 7, "current_password": password}, opts{cookie: operator}), 403)
	want(t, r.do("PUT", "/api/v1/power-mode", map[string]any{"mode": 3, "current_password": password}, opts{cookie: owner}), 400)
	// The unit can't see its supply, so MAXN takes the owner's word for it.
	w := r.do("PUT", "/api/v1/power-mode", map[string]any{"mode": 0, "current_password": password}, opts{cookie: owner})
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "15 V") {
		t.Fatal(w.Body.String())
	}
	if len(r.rootd.called("power-mode")) != 0 {
		t.Fatal("a refused mode reached the helper")
	}
	want(t, r.do("PUT", "/api/v1/power-mode", map[string]any{"mode": 0, "supply_checked": true,
		"current_password": password}, opts{cookie: owner}), 200)
	if calls := r.rootd.called("power-mode"); len(calls) != 1 || calls[0].Target != "0" {
		t.Fatalf("%+v", calls)
	}
	entries, _ := r.s.Store.AuditList(1, 0)
	if entries[0].Action != "power-mode" || entries[0].Target != "from MODE_15W_DESKTOP to MAXN" {
		t.Fatalf("%+v", entries[0])
	}
}

func TestRename(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	body := map[string]any{"name": "lab-2", "current_password": password}
	want(t, r.do("POST", "/api/v1/rename", body, opts{cookie: operator, idem: "1"}), 403)
	for _, bad := range []string{"Lab", "lab.local", "patai-0001", "localhost", ""} {
		want(t, r.do("POST", "/api/v1/rename", map[string]any{"name": bad, "current_password": password},
			opts{cookie: owner, idem: "2"}), 400)
	}
	r.rootd.outputs = map[string]string{"update-status": `{"busy":true}`}
	want(t, r.do("POST", "/api/v1/rename", body, opts{cookie: owner, idem: "3"}), 409)
	r.rootd.outputs = nil
	w := r.do("POST", "/api/v1/rename", body, opts{cookie: owner, idem: "4"})
	want(t, w, 202)
	var j store.Job
	json.Unmarshal(w.Body.Bytes(), &j)
	if j.Kind != "rename" || j.Target != "lab-2" {
		t.Fatalf("%+v", j)
	}
}

func TestShutdown(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	viewer := r.user(owner, "v", "viewer")
	operator := r.user(owner, "ops", "operator")
	ownersToken := r.token(owner, "o", "operator")
	want(t, r.do("POST", "/api/v1/shutdown", nil, opts{cookie: viewer, idem: "1"}), 403)
	// It takes someone beside the unit to unplug it.
	want(t, r.do("POST", "/api/v1/shutdown", nil, opts{bearer: ownersToken, idem: "2"}), 403)
	r.rootd.outputs = map[string]string{"update-status": `{"busy":true}`}
	want(t, r.do("POST", "/api/v1/shutdown", nil, opts{cookie: operator, idem: "3"}), 409)
	r.rootd.outputs = nil
	w := r.do("POST", "/api/v1/shutdown", nil, opts{cookie: operator, idem: "4"})
	want(t, w, 202)
	var j store.Job
	json.Unmarshal(w.Body.Bytes(), &j)
	if j.Kind != "shutdown" {
		t.Fatalf("%+v", j)
	}
}

func TestAComebackSaysWhatHappened(t *testing.T) {
	r := newRig(t)
	st := r.s.Store
	job := func(kind, target, key string) int64 {
		j, _, _ := st.CreateJob(store.Job{Kind: kind, Target: target, Actor: "a", BootID: "old"}, key, store.Audit{})
		st.SetJob(j.ID, store.JobRunning, "", "old")
		return j.ID
	}
	renamed, notRenamed, stopped := job("rename", "lab-2", "a"), job("rename", "lab-3", "b"), job("shutdown", "x", "c")
	queued, _, _ := st.CreateJob(store.Job{Kind: "shutdown", Target: "x", Actor: "a", BootID: "old"}, "d", store.Audit{})

	runner := jobs.New(st, r.rootd, "new-boot")
	runner.Name = "lab-2"
	if err := runner.Reconcile(); err != nil {
		t.Fatal(err)
	}
	check := func(id int64, state, result string) {
		t.Helper()
		j, _ := st.Job(id)
		if j.State != state || j.Result != result {
			t.Fatalf("job %d is %s %q, want %s %q", id, j.State, j.Result, state, result)
		}
	}
	check(renamed, store.JobSucceeded, "unit restarted as lab-2")
	check(notRenamed, store.JobFailed, "the unit restarted as lab-2")
	check(stopped, store.JobSucceeded, "unit stopped, and has started again")
	check(queued.ID, store.JobFailed, "interrupted by a restart of the unit")
	if len(r.rootd.calls) != 0 {
		t.Fatalf("reconcile ran something: %+v", r.rootd.calls)
	}
}

func TestAClockSetBackLeavesNoNegativeAge(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	os.WriteFile(r.s.ReportPath, []byte(`{"name":"x"}`), 0o644)
	ahead := time.Now().Add(time.Hour)
	os.Chtimes(r.s.ReportPath, ahead, ahead)
	w := r.do("GET", "/api/v1/status", nil, opts{cookie: owner})
	want(t, w, 200)
	var status struct {
		Age int `json:"age_s"`
	}
	json.Unmarshal(w.Body.Bytes(), &status)
	if status.Age != 0 {
		t.Fatalf("age %d", status.Age)
	}
}

func TestATokenThatRunsOutWhileTheClockIsSetStaysOut(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	// The helper set the clock from a time later than the console's first look, past this token's end.
	soon := time.Now().Add(30 * time.Second)
	at := soon.UTC().Format(time.RFC3339)
	r.s.Store.CreateToken(store.Token{Name: "soon", Scope: "viewer", CreatedBy: "ada", ExpiresAt: &at}, "h1", store.Audit{})
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":false}`,
		"clock-set": `{"set":{"from_ms":` + strconv.FormatInt(soon.Add(time.Minute).UnixMilli(), 10) + `}}`}
	want(t, r.do("PUT", "/api/v1/clock", map[string]any{"time_ms": time.Now().Add(-time.Hour).UnixMilli(),
		"current_password": password}, opts{cookie: owner}), 200)
	if list, _ := r.s.Store.Tokens(); len(list) != 0 {
		t.Fatalf("%+v", list)
	}
}

// ownersCertificate is a chain from a lab CA for names, and its key, valid from from for a year.
func ownersCertificate(t *testing.T, from time.Time, names ...string) (chain, key string) {
	t.Helper()
	sign := func(template *x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template.SerialNumber = big.NewInt(time.Now().UnixNano())
		if parent == nil {
			parent, parentKey = template, k
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &k.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c, k, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	root, rootKey, rootPEM := sign(&x509.Certificate{Subject: pkix.Name{CommonName: "Lab root"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: from.AddDate(-1, 0, 0),
		NotAfter: from.AddDate(5, 0, 0)}, nil, nil)
	_, leafKey, leafPEM := sign(&x509.Certificate{Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: from, NotAfter: from.AddDate(1, 0, 0)}, root, rootKey)
	der, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	return string(leafPEM) + string(rootPEM), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestOwnersCertificate(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	operator := r.user(owner, "ops", "operator")
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":true,"set":null}`}
	chain, key := ownersCertificate(t, time.Now().Add(-time.Hour), "gpu1.lab.example")

	w := r.do("GET", "/api/v1/certificate", nil, opts{cookie: operator})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"owner":null`) || !strings.Contains(w.Body.String(), `"unit_fingerprint":"AA:BB"`) {
		t.Fatal(w.Body.String())
	}
	body := map[string]any{"chain": chain, "key": key, "current_password": password}
	want(t, r.do("POST", "/api/v1/certificate/check", body, opts{cookie: operator}), 403)
	want(t, r.do("PUT", "/api/v1/certificate", body, opts{cookie: operator}), 403)
	w = r.do("POST", "/api/v1/certificate/check", map[string]any{"chain": chain, "key": "nonsense"}, opts{cookie: owner})
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "isn't PEM") {
		t.Fatal(w.Body.String())
	}
	w = r.do("POST", "/api/v1/certificate/check", body, opts{cookie: owner})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"names":["gpu1.lab.example"]`) {
		t.Fatal(w.Body.String())
	}
	// A check changes nothing, and nor does a wrong password.
	want(t, r.do("GET", "/api/identity", nil, opts{host: "gpu1.lab.example"}), 421)
	body["current_password"] = "wrong"
	want(t, r.do("PUT", "/api/v1/certificate", body, opts{cookie: owner}), 403)
	if _, err := os.Stat(r.s.OwnerCertPath); err == nil {
		t.Fatal("kept without the password")
	}

	body["current_password"] = password
	want(t, r.do("PUT", "/api/v1/certificate", body, opts{cookie: owner}), 200)
	if info, err := os.Stat(r.s.OwnerCertPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", info, err)
	}
	// The console answers to the owner's name now, and to nothing else new.
	want(t, r.do("GET", "/api/identity", nil, opts{host: "gpu1.lab.example"}), 200)
	want(t, r.do("GET", "/api/identity", nil, opts{host: "lab.example"}), 421)
	w = r.do("GET", "/api/v1/certificate", nil, opts{cookie: operator})
	if !strings.Contains(w.Body.String(), `"names":["gpu1.lab.example"]`) {
		t.Fatal(w.Body.String())
	}
	entries, _ := r.s.Store.AuditList(1, 0)
	if entries[0].Action != "certificate-set" || !strings.HasPrefix(entries[0].Target, "gpu1.lab.example, ") ||
		strings.Contains(entries[0].Target, "PRIVATE") {
		t.Fatalf("%+v", entries[0])
	}

	want(t, r.do("POST", "/api/v1/certificate/remove", map[string]any{"current_password": "wrong"}, opts{cookie: owner}), 403)
	want(t, r.do("POST", "/api/v1/certificate/remove", map[string]any{"current_password": password}, opts{cookie: owner}), 204)
	want(t, r.do("GET", "/api/identity", nil, opts{host: "gpu1.lab.example"}), 421)
	if _, err := os.Stat(r.s.OwnerCertPath); err == nil {
		t.Fatal("still kept after removal")
	}
}

func TestACertificatesDatesNeedAClockToCheckThem(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	chain, key := ownersCertificate(t, time.Now().AddDate(-2, 0, 0), "gpu1.lab.example")
	body := map[string]any{"chain": chain, "key": key}
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":true,"set":null}`}
	w := r.do("POST", "/api/v1/certificate/check", body, opts{cookie: owner})
	want(t, w, 400)
	if !strings.Contains(w.Body.String(), "ran out") {
		t.Fatal(w.Body.String())
	}
	// Set by an owner this boot counts as known.
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":false,"set":{"from_ms":1,"to_ms":2,"ago_s":3}}`}
	want(t, r.do("POST", "/api/v1/certificate/check", body, opts{cookie: owner}), 400)
	r.rootd.outputs = map[string]string{"clock-status": `{"synced":false,"set":null}`}
	w = r.do("POST", "/api/v1/certificate/check", body, opts{cookie: owner})
	want(t, w, 200)
	if !strings.Contains(w.Body.String(), `"dates_unsure":true`) {
		t.Fatal(w.Body.String())
	}
}

func TestAnOriginHasToBeHTTPS(t *testing.T) {
	r := newRig(t)
	owner, _ := r.claim()
	want(t, r.do("POST", "/api/v1/me/password", map[string]string{"current_password": password, "new_password": "x"},
		opts{cookie: owner, origin: "http://10.0.0.5"}), 403)
}

func TestNoStrictTransportSecurity(t *testing.T) {
	r := newRig(t)
	w := r.do("GET", "/api/identity", nil, opts{})
	if got := w.Header().Get("Strict-Transport-Security"); got != "max-age=0" {
		t.Fatalf("%q", got)
	}
}
