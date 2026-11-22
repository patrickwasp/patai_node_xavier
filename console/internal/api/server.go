// Package api is the console's HTTP surface: the one API behind the web console, the status screen,
// scripts and, later, the cloud (docs/console.md).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"patai/console/internal/acme"
	"patai/console/internal/auth"
	"patai/console/internal/certs"
	"patai/console/internal/jobs"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

// Mode says how far this start got.
const (
	ModeNormal   = "normal"
	ModeRecovery = "recovery" // an identified storage fault: a notice and nothing else
	ModeFailed   = "failed"   // this release can't use its state, so the health check must fail
)

// Features this release's API offers. Clients go by these, not by version.
var features = []string{"claim", "login", "recovery-codes", "users", "tokens", "audit", "jobs",
	"status", "workloads", "reboot", "gateway", "disk", "factory-reset", "ssh-keys", "updates",
	"workload-fetch", "network", "clock", "power-mode", "rename", "shutdown", "certificate", "logs", "support-bundle", "resources", "models",
	"apps", "keys", "api-usage", "tailscale", "backups", "domain"}

type Config struct {
	Hostname    string
	Release     string
	BootID      string
	Mode        string
	Fault       string
	Fingerprint string // the unit's own certificate's
	UnitPin     string // the SHA-256 of the unit's own key's SubjectPublicKeyInfo, which phones pin (node.go)
	NodeID      string // for phones (node.go); empty in recovery mode

	// The node key (certs.KeyFile), made the first time a way of reaching the unit needs it; nil in recovery mode.
	NodeKey *certs.KeyFile

	TLS           *certs.Selector
	OwnerCertPath string // where an owner's certificate is kept; empty in recovery mode

	Store *store.Store
	Rootd rootd.Caller
	Jobs  *jobs.Runner
	UI    fs.FS

	ReportPath    string // written by patai-report
	ClaimPath     string // the claim code; its presence is what makes a unit claimable
	ValidatedPath string // left by the health check once this boot has passed
	RecipesPath   string
	AppsPath      string
	UploadPath    string // where an uploaded bundle lands, for patai-rootd to take
	DataPath      string // the data disk, to see whether a model upload has room
	ResultsDir    string // update results and rollback reports, for a support bundle
	ThermalDir    string
	FrontPath     string // the app the owner put in front (front.go); empty in recovery mode
	OutsidePath   string // models from services (outside.go); empty in recovery mode

	// Backups (backups.go): what the console keeps of them, empty in recovery mode; where it puts its copy of its
	// database for the helper to take; and what patai-restore says of the last restore.
	BackupPath     string
	BackupSnapshot string
	RestoreReport  string

	// Remote access with Tailscale (tailscale.go): tailscaled's socket, the mark patai-rootd leaves while it's on, its
	// binary, and the owner's settings, kept with the console's state, which is empty in recovery mode.
	TailscaleSocket string
	TailscaleOnPath string
	TailscaleBinary string
	TailscalePath   string

	// Remote access, Your own domain (domain.go): the owner's settings, kept with the console's state, which is empty in
	// recovery mode, beside the credentials and the certificate; and the CA, Let's Encrypt's unless a test gives another.
	DomainPath string
	Issuer     acme.Issuer
}

type Server struct {
	Config
	sessions  *auth.Sessions
	throttle  *auth.Throttle
	uploading sync.Mutex
	clockLock sync.Mutex
	certLock  sync.Mutex
	bundling  sync.Mutex
	follows   follows

	// The gateway's (gateway.go, images.go).
	views    views
	book     book
	inflight inflight
	slots    slots
	proxies  proxies
	imaging  imaging
	persons  people
	front    front
	outside  outside
	tailnet  tailnetWatch
	domain   domainWatch
	backups  backupState
}

func New(c Config) *Server {
	return &Server{Config: c, sessions: auth.NewSessions(), throttle: auth.NewThrottle()}
}

// Principal is who a request is acting as.
type Principal struct {
	Name    string
	Role    string
	UserID  int64 // 0 for a token
	TokenID int64 // 0 for a person
	Session string
	Kind    string // session or token
}

var ranks = map[string]int{store.KeyScope: 1, "viewer": 2, "operator": 3, "owner": 4}

func (p *Principal) has(role string) bool { return p != nil && ranks[p.Role] >= ranks[role] }

// isKey is true of an API key, which reaches the apps' APIs and nothing else.
func (p *Principal) isKey() bool { return p != nil && p.Role == store.KeyScope }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

func (s *Server) failStore(w http.ResponseWriter, err error) {
	log.Printf("store: %v", err)
	if store.IsStorageFault(err) {
		// Work that can't be recorded doesn't happen.
		fail(w, http.StatusInsufficientStorage, "storage", "the unit can't record this change")
		return
	}
	fail(w, http.StatusInternalServerError, "internal", "something went wrong")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "bad-request", "that request couldn't be read")
		return false
	}
	return true
}

func source(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) audit(r *http.Request, actor, action, target, result string) store.Audit {
	return store.Audit{BootID: s.BootID, Actor: actor, Origin: "local", Source: source(r),
		Action: action, Target: target, Result: result}
}

// principal finds who's calling, from a bearer token or a session cookie.
func (s *Server) principal(r *http.Request) *Principal {
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, "Bearer ") {
		t, err := s.Store.TokenByHash(auth.HashSecret(strings.TrimPrefix(header, "Bearer ")))
		if err != nil {
			return nil
		}
		if t.ExpiresAt != nil {
			if at, err := time.Parse(time.RFC3339, *t.ExpiresAt); err == nil && time.Now().After(at) {
				return nil
			}
		}
		return &Principal{Name: "token:" + t.Name, Role: t.Scope, TokenID: t.ID, Kind: "token"}
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	sess := s.sessions.Get(cookie.Value, !polling(r))
	if sess == nil {
		return nil
	}
	return &Principal{Name: sess.Name, Role: sess.Role, UserID: sess.UserID, Session: cookie.Value, Kind: "session"}
}

// polling is the console's own background refresh, which isn't a person doing something. Following a log is
// that too, or a tab left open would keep its login alive.
func polling(r *http.Request) bool {
	return r.Method == http.MethodGet && (r.URL.Path == "/api/v1/status" || r.URL.Path == "/api/v1/updates" ||
		r.URL.Path == "/api/v1/workloads/status" || r.URL.Path == "/api/v1/network" || r.URL.Path == "/api/v1/clock" ||
		r.URL.Path == "/api/v1/power-mode" || r.URL.Path == "/api/v1/resources" || r.URL.Path == "/api/v1/tailscale" || r.URL.Path == "/api/v1/backups" || strings.HasPrefix(r.URL.Path, "/api/v1/jobs") ||
		strings.HasPrefix(r.URL.Path, "/api/v1/logs/") && strings.HasSuffix(r.URL.Path, "/follow"))
}

type authed func(http.ResponseWriter, *http.Request, *Principal)

// need checks the role, and for a session the same-origin rules that keep another site from using
// it. Tokens aren't sent by browsers on their own, so they need no such check.
func (s *Server) need(role string, next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.usable(w) {
			return
		}
		p := s.principal(r)
		if p == nil {
			fail(w, http.StatusUnauthorized, "login", "log in first")
			return
		}
		if !p.has(role) || p.isKey() {
			fail(w, http.StatusForbidden, "forbidden", "your role can't do that")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if p.Kind == "session" && !sameOrigin(r) {
				fail(w, http.StatusForbidden, "origin", "this request came from another site")
				return
			}
			if !s.validated() {
				fail(w, http.StatusServiceUnavailable, "starting", "the unit is still starting")
				return
			}
		}
		next(w, r, p)
	}
}

// open is for the few changes made before there's a login: claim, login and recovery.
func (s *Server) open(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.usable(w) {
			return
		}
		if !sameOrigin(r) {
			fail(w, http.StatusForbidden, "origin", "this request came from another site")
			return
		}
		if !s.validated() {
			fail(w, http.StatusServiceUnavailable, "starting", "the unit is still starting")
			return
		}
		next(w, r)
	}
}

// openLogin is open without the wait for the health check: a login changes nothing a rollback could
// lose, and someone reconnecting after a restart shouldn't be turned away from the unit they're
// watching come back.
func (s *Server) openLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.usable(w) {
			return
		}
		if !sameOrigin(r) {
			fail(w, http.StatusForbidden, "origin", "this request came from another site")
			return
		}
		next(w, r)
	}
}

func (s *Server) usable(w http.ResponseWriter) bool {
	if s.Mode != ModeNormal {
		fail(w, http.StatusServiceUnavailable, "recovery", "this unit needs service")
		return false
	}
	return true
}

// validated is false until this boot has passed the health check, so nothing a person does can land
// in a slot that might still roll back.
func (s *Server) validated() bool {
	_, err := os.Stat(s.ValidatedPath)
	return err == nil
}

// sameOrigin compares the browser's Origin with the Host it asked for. Browsers always send Origin on
// these requests, so one with none didn't come from a page.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Cookie") == ""
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && u.Host == r.Host
}

// hostAllowed stops DNS rebinding: a page on an attacker's name, pointed at this unit, arrives with
// that name in Host. An IP address can't be rebound.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	name := strings.ToLower(s.Hostname)
	// An owner's certificate names are the owner's, so nobody else can point them at the unit.
	return host == name || host == name+".local" || host == "localhost" || (s.TLS != nil && s.TLS.Serves(host))
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// None, and any a browser kept is forgotten: an owner's certificate that ran out would otherwise lock its
		// name out with an error that can't be clicked through.
		h.Set("Strict-Transport-Security", "max-age=0")
		next.ServeHTTP(w, r)
	})
}

// Handler is the console: on AdminPort, and on 443 unless an app is in front (front.go).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/identity", s.identity)

	mux.HandleFunc("POST /api/v1/claim", s.open(s.claim))
	mux.HandleFunc("POST /api/v1/session", s.openLogin(s.login))
	mux.HandleFunc("POST /api/v1/recover", s.open(s.recover))

	mux.HandleFunc("GET /api/v1/session", s.need("viewer", s.whoami))
	mux.HandleFunc("DELETE /api/v1/session", s.need("viewer", s.logout))
	mux.HandleFunc("POST /api/v1/me/password", s.need("viewer", s.changePassword))

	mux.HandleFunc("GET /api/v1/capabilities", s.need("viewer", s.capabilities))
	mux.HandleFunc("GET /api/v1/status", s.need("viewer", s.status))
	mux.HandleFunc("GET /api/v1/resources", s.need("viewer", s.resources))
	mux.HandleFunc("GET /api/v1/workloads", s.need("viewer", s.workloads))
	mux.HandleFunc("GET /api/v1/workloads/status", s.need("viewer", s.workloadStatus))
	mux.HandleFunc("POST /api/v1/apps/{app}/start", s.need("operator", s.appJob("workload-start")))
	mux.HandleFunc("POST /api/v1/apps/{app}/stop", s.need("operator", s.appJob("workload-stop")))
	mux.HandleFunc("POST /api/v1/apps/{app}/fetch", s.need("operator", s.appJob("workload-fetch")))
	mux.HandleFunc("POST /api/v1/apps/{app}/fetch/cancel", s.need("operator", s.appJob("workload-fetch-cancel")))
	mux.HandleFunc("POST /api/v1/apps/{app}/apis/{api}/on", s.need("operator", s.apiJob("app-api-on")))
	mux.HandleFunc("POST /api/v1/apps/{app}/apis/{api}/off", s.need("operator", s.apiJob("app-api-off")))
	mux.HandleFunc("GET /api/v1/api-usage", s.need("operator", s.apiUsage))
	mux.HandleFunc("GET /api/v1/api-requests", s.need("operator", s.apiRequests))
	mux.HandleFunc("POST /api/v1/workloads/{stack}/models/{model}/use", s.need("operator", s.modelJob("model-use")))
	mux.HandleFunc("POST /api/v1/workloads/{stack}/models/{model}/fetch", s.need("operator", s.modelJob("model-fetch")))
	mux.HandleFunc("POST /api/v1/workloads/{stack}/models/{model}/remove", s.need("operator", s.modelJob("model-remove")))
	mux.HandleFunc("POST /api/v1/workloads/{stack}/models", s.need("owner", s.addModel))
	mux.HandleFunc("PUT /api/v1/workloads/{stack}/models/upload/{model}", s.need("owner", s.uploadModel))
	mux.HandleFunc("PUT /api/v1/workloads/{stack}/settings", s.need("operator", s.serviceSettings))
	mux.HandleFunc("PUT /api/v1/workloads/{stack}/secrets/{name}", s.need("owner", s.serviceSecret))
	mux.HandleFunc("POST /api/v1/reboot", s.need("operator", s.reboot))
	mux.HandleFunc("POST /api/v1/shutdown", s.need("operator", s.shutdown))
	mux.HandleFunc("GET /api/v1/clock", s.need("viewer", s.getClock))
	mux.HandleFunc("GET /api/v1/power-mode", s.need("viewer", s.getPowerMode))
	mux.HandleFunc("GET /api/v1/updates", s.need("viewer", s.getUpdates))
	mux.HandleFunc("POST /api/v1/updates/check", s.need("operator", s.checkUpdates))
	mux.HandleFunc("POST /api/v1/updates/install", s.need("operator", s.installUpdate))
	mux.HandleFunc("GET /api/v1/jobs", s.need("viewer", s.listJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.need("viewer", s.getJob))
	mux.HandleFunc("GET /api/v1/logs", s.need("operator", s.logIndex))
	mux.HandleFunc("GET /api/v1/logs/{source}", s.need("operator", s.getLogs))
	mux.HandleFunc("GET /api/v1/logs/{source}/follow", s.need("operator", s.followLogs))

	mux.HandleFunc("GET /api/v1/users", s.need("owner", s.listUsers))
	mux.HandleFunc("POST /api/v1/users", s.need("owner", s.createUser))
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.need("owner", s.deleteUser))
	mux.HandleFunc("POST /api/v1/users/{id}/password", s.need("owner", s.resetPassword))
	mux.HandleFunc("GET /api/v1/tokens", s.need("owner", s.listTokens))
	mux.HandleFunc("POST /api/v1/tokens", s.need("owner", s.createToken))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.need("owner", s.deleteToken))
	mux.HandleFunc("GET /api/v1/keys", s.need("owner", s.listKeys))
	mux.HandleFunc("POST /api/v1/keys", s.need("owner", s.createKey))
	mux.HandleFunc("PUT /api/v1/keys/{id}/limits", s.need("owner", s.setKeyLimits))
	mux.HandleFunc("DELETE /api/v1/keys/{id}", s.need("owner", s.deleteToken))
	mux.HandleFunc("GET /api/v1/audit", s.need("owner", s.listAudit))
	mux.HandleFunc("POST /api/v1/support-bundle", s.need("owner", s.supportBundle))

	mux.HandleFunc("GET /api/v1/disk", s.need("viewer", s.disk))
	mux.HandleFunc("POST /api/v1/disk/keep", s.need("owner", s.diskAnswer("disk-keep", "undecided")))
	mux.HandleFunc("POST /api/v1/disk/erase", s.need("owner", s.diskAnswer("disk-erase", "undecided", "foreign")))
	mux.HandleFunc("POST /api/v1/factory-reset", s.need("owner", s.factoryReset))
	mux.HandleFunc("PUT /api/v1/updates/settings", s.need("owner", s.saveUpdateSettings))
	mux.HandleFunc("POST /api/v1/updates/upload", s.need("owner", s.uploadBundle))
	mux.HandleFunc("DELETE /api/v1/updates/upload", s.need("owner", s.discardBundle))
	mux.HandleFunc("POST /api/v1/updates/upload/install", s.need("owner", s.installBundle))
	mux.HandleFunc("GET /api/v1/ssh-keys", s.need("owner", s.listSSHKeys))
	mux.HandleFunc("POST /api/v1/ssh-keys", s.need("owner", s.addSSHKey))
	mux.HandleFunc("DELETE /api/v1/ssh-keys/{id}", s.need("owner", s.removeSSHKey))
	mux.HandleFunc("GET /api/v1/network", s.need("viewer", s.getNetwork))
	mux.HandleFunc("GET /api/v1/tailscale", s.need("owner", s.getTailscale))
	mux.HandleFunc("POST /api/v1/tailscale/on", s.need("owner", s.tailscaleTurnOn))
	mux.HandleFunc("POST /api/v1/tailscale/off", s.need("owner", s.tailscaleTurnOff))
	mux.HandleFunc("POST /api/v1/tailscale/login", s.need("owner", s.tailscaleLogin))
	mux.HandleFunc("POST /api/v1/tailscale/logout", s.need("owner", s.tailscaleLogout))
	mux.HandleFunc("PUT /api/v1/tailscale/settings", s.need("owner", s.putTailscaleSettings))
	mux.HandleFunc("GET /api/v1/domain", s.need("owner", s.getDomain))
	mux.HandleFunc("PUT /api/v1/domain", s.need("owner", s.setDomain))
	mux.HandleFunc("DELETE /api/v1/domain", s.need("owner", s.removeDomain))
	mux.HandleFunc("PUT /api/v1/network/{port}", s.need("owner", s.tryNetwork))
	mux.HandleFunc("POST /api/v1/network/keep", s.need("owner", s.networkDecision("network-keep")))
	mux.HandleFunc("POST /api/v1/network/undo", s.need("owner", s.networkDecision("network-undo")))
	mux.HandleFunc("PUT /api/v1/clock", s.need("owner", s.setClock))
	mux.HandleFunc("PUT /api/v1/power-mode", s.need("owner", s.setPowerMode))
	mux.HandleFunc("POST /api/v1/rename", s.need("owner", s.rename))
	mux.HandleFunc("GET /api/v1/front", s.need("viewer", s.getFront))
	mux.HandleFunc("PUT /api/v1/front", s.need("owner", s.putFront))
	mux.HandleFunc("GET /api/v1/outside", s.need("owner", s.getOutside))
	mux.HandleFunc("PUT /api/v1/outside/services/{id}", s.need("owner", s.putProvider))
	mux.HandleFunc("DELETE /api/v1/outside/services/{id}", s.need("owner", s.deleteProvider))
	mux.HandleFunc("PUT /api/v1/outside/services/{id}/models", s.need("owner", s.putProviderModels))
	mux.HandleFunc("PUT /api/v1/outside/services/{id}/data", s.need("owner", s.putProviderData))
	mux.HandleFunc("GET /api/v1/outside/services/{id}/catalogue", s.need("owner", s.catalogue))
	mux.HandleFunc("PUT /api/v1/outside/apps", s.need("owner", s.putOutsideApps))
	mux.HandleFunc("GET /api/v1/backups", s.need("owner", s.getBackups))
	mux.HandleFunc("GET /api/v1/backups/list", s.need("owner", s.listBackups))
	mux.HandleFunc("PUT /api/v1/backups/destination", s.need("owner", s.putDestination))
	mux.HandleFunc("GET /api/v1/backups/disks", s.need("owner", s.listBackupDisks))
	mux.HandleFunc("POST /api/v1/backups/disks/format", s.need("owner", s.formatBackupDisk))
	mux.HandleFunc("POST /api/v1/backups/ssh-key", s.need("owner", s.backupSSHKey))
	mux.HandleFunc("POST /api/v1/backups/host-key", s.need("owner", s.scanHostKey))
	mux.HandleFunc("POST /api/v1/backups/key", s.need("owner", s.newBackupKey))
	mux.HandleFunc("POST /api/v1/backups/key/confirm", s.need("owner", s.confirmBackupKey))
	mux.HandleFunc("POST /api/v1/backups/key/check", s.need("owner", s.checkBackupKey))
	mux.HandleFunc("PUT /api/v1/backups/schedule", s.need("owner", s.putBackupSchedule))
	mux.HandleFunc("POST /api/v1/backups/run", s.need("owner", s.runBackupNow))
	mux.HandleFunc("POST /api/v1/backups/check", s.need("owner", s.checkBackup))
	mux.HandleFunc("POST /api/v1/backups/restore", s.need("owner", s.restoreBackup))
	mux.HandleFunc("GET /api/v1/certificate", s.need("viewer", s.getCertificate))
	mux.HandleFunc("POST /api/v1/certificate/check", s.need("owner", s.checkCertificate))
	mux.HandleFunc("PUT /api/v1/certificate", s.need("owner", s.setCertificate))
	mux.HandleFunc("POST /api/v1/certificate/remove", s.need("owner", s.removeCertificate))

	mux.Handle("/v1/", s.apis())
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusNotFound, "not-found", "no such route")
	}))
	mux.Handle("/", s.static())
	return s.guard(mux)
}

// static serves the exported UI. A path with no file gets its .html page, then the front page.
func (s *Server) static() http.Handler {
	files := http.FileServerFS(s.UI)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
		path := strings.Trim(r.URL.Path, "/")
		if path == "" {
			files.ServeHTTP(w, r)
			return
		}
		info, err := fs.Stat(s.UI, path)
		if err == nil && info.IsDir() {
			// A page is a directory with an index. Any other directory isn't for browsing.
			if _, err := fs.Stat(s.UI, path+"/index.html"); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		if errors.Is(err, fs.ErrNotExist) {
			if _, err := fs.Stat(s.UI, path+".html"); err == nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + path + ".html"
				// FileServer would redirect a .html path back to itself.
				http.ServeFileFS(w, r2, s.UI, path+".html")
				return
			}
			http.ServeFileFS(w, r, s.UI, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}

// healthz is the boot health check's question, asked over loopback (overlay/usr/sbin/
// user_rootfs_validation.sh). It proves the UI, the store and the helper, not the network, a display
// or the NVMe. Recovery passes because a storage fault isn't this release's fault; anything else fails,
// so a bad release can't hide there.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if ip := net.ParseIP(source(r)); ip == nil || !ip.IsLoopback() {
		http.NotFound(w, r)
		return
	}
	report := map[string]any{"mode": s.Mode, "fault": s.Fault}
	healthy := s.Mode != ModeFailed
	if _, err := fs.Stat(s.UI, "index.html"); err != nil {
		report["ui"] = err.Error()
		healthy = false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.Rootd.Call(ctx, rootd.Request{Verb: "ping"}); err != nil {
		report["rootd"] = err.Error()
		healthy = false
	}
	if s.Mode == ModeNormal {
		if _, err := s.Store.HasOwner(); err != nil {
			report["store"] = err.Error()
			healthy = false
		}
	}
	status := http.StatusOK
	if !healthy {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, report)
}

// State is what a unit says about itself to anyone, and never changes shape, so a cloud can list a
// unit on an API it doesn't know.
func (s *Server) State() string {
	if s.Mode != ModeNormal {
		return "recovery"
	}
	if claimed, err := s.Store.HasOwner(); err != nil || claimed {
		return "claimed"
	}
	if s.ClaimCode() != "" {
		return "unclaimed"
	}
	return "unclaimable"
}

func (s *Server) ClaimCode() string {
	data, err := os.ReadFile(s.ClaimPath)
	if err != nil {
		return ""
	}
	return auth.NormalizeCode(strings.TrimSpace(string(data)))
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"product":      "patai",
		"name":         s.Hostname,
		"release":      s.Release,
		"api_versions": []string{"v1"},
		"state":        s.State(),
		"fault":        s.Fault,
		"fingerprint":  s.Fingerprint,
	})
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request, p *Principal) {
	writeJSON(w, http.StatusOK, map[string]any{
		"api": "v1", "release": s.Release, "state_format": store.FormatWrites, "features": features,
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, p *Principal) {
	data, err := os.ReadFile(s.ReportPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"report": nil, "age_s": nil})
		return
	}
	var report any
	if err := json.Unmarshal(data, &report); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"report": nil, "age_s": nil})
		return
	}
	age := -1
	if info, err := os.Stat(s.ReportPath); err == nil {
		// A clock set back leaves the file's time ahead of now until the report is written again.
		age = max(0, int(time.Since(info.ModTime()).Seconds()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "age_s": age})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad-request", "that id isn't a number")
		return 0, false
	}
	return id, true
}
