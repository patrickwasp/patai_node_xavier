// patai-api is the unit's console and API (docs/console.md). It runs unprivileged, serves HTTPS, and
// asks patai-rootd for anything that needs root. It starts whatever state it finds: with bad storage
// it comes up in recovery mode, showing a fault notice and nothing else.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"patai/console/internal/api"
	"patai/console/internal/certs"
	"patai/console/internal/jobs"
	"patai/console/internal/reset"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
	"patai/console/internal/updates"
	"patai/console/internal/web"
	"patai/console/internal/workloads"
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func firstLine(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	line, _, _ := strings.Cut(string(data), "\n")
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return fallback
}

// persistUsable is the check before touching the store: mounted, and taking writes. Ext4 remounts a
// damaged filesystem read-only, and that's a fault of the storage. It has to try a write: this
// service's sandbox shows the rest of /persist as read-only whatever state the disk is in.
func persistUsable(dir string) error {
	if !strings.HasPrefix(dir, "/persist/") {
		return nil
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	mounted := false
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == "/persist" {
			mounted = true
		}
	}
	if !mounted {
		return errors.New("/persist isn't mounted")
	}
	f, err := os.CreateTemp(dir, ".write-test")
	if err != nil {
		return fmt.Errorf("/persist can't be written: %w", err)
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

func main() {
	dir := env("PATAI_CONSOLE_DIR", "/persist/console")
	httpsAddr := env("PATAI_HTTPS_ADDR", ":443")
	httpAddr := env("PATAI_HTTP_ADDR", ":80")
	// The console's own port, where it always is, and the only one once an app is in front on 443 (front.go).
	adminAddr := env("PATAI_ADMIN_ADDR", fmt.Sprintf(":%d", api.AdminPort))
	// The gateway for apps, which patai-firewall lets only containers reach (docs/apps.md, The gateway).
	appsAddr := env("PATAI_APPS_ADDR", ":18000")
	screenPath := env("PATAI_SCREEN_FILE", "/run/patai-api/screen.json")

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "patai"
	}
	cfg := api.Config{
		Hostname:      hostname,
		Release:       firstLine("/etc/patai-version", "development"),
		BootID:        firstLine("/proc/sys/kernel/random/boot_id", "unknown"),
		Mode:          api.ModeNormal,
		Rootd:         rootd.Client{Path: env("PATAI_ROOTD_SOCKET", rootd.Socket)},
		UI:            web.UI(),
		ReportPath:    env("PATAI_REPORT", "/run/patai/report.json"),
		ClaimPath:     filepath.Join(dir, "claim-code"),
		ValidatedPath: env("PATAI_VALIDATED", "/run/patai/validated"),
		RecipesPath:   env("PATAI_RECIPES", "/usr/share/patai/workloads"),
		AppsPath:      env("PATAI_APPS", "/usr/share/patai/apps"),
		UploadPath:    env("PATAI_UPLOAD", updates.Incoming),
		DataPath:      env("PATAI_DATA", "/data"),
		ResultsDir:    updates.Results,
		ThermalDir:    "/persist/logs/thermal",
	}

	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	// The reset runs before this does, so its marker here means it couldn't finish. Whatever it left
	// mustn't look like a unit in use, or like a new one.
	if _, err := os.Stat(reset.Marker); err == nil {
		cfg.Mode, cfg.Fault = api.ModeRecovery, "a factory reset didn't finish"
	} else if err := persistUsable(dir); err != nil {
		cfg.Mode, cfg.Fault = api.ModeRecovery, err.Error()
	} else if s, err := store.Open(dir); err != nil {
		if store.IsStorageFault(err) {
			cfg.Mode, cfg.Fault = api.ModeRecovery, "state can't be read: "+err.Error()
		} else {
			cfg.Mode, cfg.Fault = api.ModeFailed, err.Error()
		}
	} else {
		cfg.Store = s
		cfg.FrontPath = filepath.Join(dir, "front")
		cfg.OutsidePath = filepath.Join(dir, "outside.json")
		cfg.TailscalePath = filepath.Join(dir, "tailscale.json")
	}
	cfg.TailscaleSocket = env("PATAI_TAILSCALE_SOCKET", "/run/tailscale/tailscaled.sock")
	cfg.TailscaleOnPath = env("PATAI_TAILSCALE_ON", "/persist/tailscale/on")
	cfg.TailscaleBinary = env("PATAI_TAILSCALED", "/usr/local/sbin/tailscaled")
	if cfg.Mode != api.ModeNormal {
		log.Printf("starting in %s mode: %s", cfg.Mode, cfg.Fault)
		certFile, keyFile = "", ""
	}
	cert, err := certs.Load(certFile, keyFile, hostname)
	if err != nil {
		// A key on /persist that won't load is a storage problem too. A certificate for this run
		// keeps the console reachable, and the screen shows the fingerprint that's in use.
		log.Printf("certificate: %v", err)
		if cert, err = certs.Load("", "", hostname); err != nil {
			log.Fatal(err)
		}
	}
	cfg.Fingerprint = certs.Fingerprint(cert)
	cfg.TLS = certs.NewSelector(cert)
	// An owner's certificate is on /persist too, so recovery mode leaves it alone. One that won't load leaves the
	// unit's own answering everywhere, and the page says why.
	if cfg.Mode == api.ModeNormal {
		cfg.OwnerCertPath = filepath.Join(dir, "owner-tls.pem")
		if about, owner, err := certs.LoadOwner(cfg.OwnerCertPath, hostname); err != nil {
			log.Printf("owner's certificate: %v", err)
			cfg.TLS.Fault(err.Error())
		} else if about != nil {
			cfg.TLS.SetOwner(about, &owner)
		}
	}

	ctx := context.Background()
	if cfg.Store != nil {
		cfg.Jobs = jobs.New(cfg.Store, cfg.Rootd, cfg.BootID)
		cfg.Jobs.Name = hostname
		if err := cfg.Jobs.Reconcile(); err != nil {
			log.Printf("jobs: %v", err)
		}
		go cfg.Jobs.Run(ctx)
	}
	server := api.New(cfg)
	if cfg.Mode == api.ModeNormal {
		server.KeepRequestLog()
		go server.WatchTailnet(ctx)
	}
	go publishScreen(server, cfg, screenPath)

	tlsConfig := cfg.TLS.Config()
	serve := func(addr string, h http.Handler, conf *tls.Config) {
		srv := &http.Server{Addr: addr, Handler: h, TLSConfig: conf, ReadHeaderTimeout: 10 * time.Second}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("%s: %v", addr, err)
		}
		if conf != nil {
			// Plain HTTP on a port that serves HTTPS is sent to https (upgrade.go).
			err = srv.ServeTLS(server.UpgradeHTTP(ln), "", "")
		} else {
			err = srv.Serve(ln)
		}
		log.Fatalf("%s: %v", addr, err)
	}
	go serve(httpAddr, server.Plain(), nil)
	go serve(appsAddr, server.AppsHandler(), nil)
	// Each app's own pages, on the port its manifest gives: they don't work under a path.
	for _, app := range workloads.ReadApps(cfg.AppsPath) {
		if app.UI != nil && app.UI.Port > 0 {
			go serve(fmt.Sprintf(":%d", app.UI.Port), server.AppUI(app), tlsConfig)
		}
	}
	go serve(adminAddr, server.Handler(), tlsConfig)
	serve(httpsAddr, server.Front(), tlsConfig)
}

// publishScreen tells patai-screen what it may show. The file is for root alone, because before a
// claim it holds the claim code.
func publishScreen(server *api.Server, cfg api.Config, path string) {
	last := ""
	for {
		doc := map[string]string{"state": server.State(), "fingerprint": cfg.Fingerprint, "fault": cfg.Fault}
		if doc["state"] == "unclaimed" {
			doc["claim_code"] = server.ClaimCode()
		}
		data, _ := json.Marshal(doc)
		if string(data) != last {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, data, 0o600); err == nil && os.Rename(tmp, path) == nil {
				last = string(data)
			}
		}
		time.Sleep(5 * time.Second)
	}
}
