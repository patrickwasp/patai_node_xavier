package workloads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The recipes and apps this repo ships: workloads/ and apps/ beside console/, and /workloads and /apps in the console's
// image build.
const (
	shipped     = "../../../workloads"
	shippedApps = "../../../apps"
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fileName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)
	envKey    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	envValue  = regexp.MustCompile(`^[A-Za-z0-9._:/-]{0,256}$`)
	ghcrRef   = regexp.MustCompile(`^ghcr\.io/patrickwasp/patai-[a-z0-9-]+:[A-Za-z0-9._-]+$`)
)

// mirrors is Wasupu's bucket, which holds each model whole at models/<sha256>/<name>.
const mirrors = "https://wasupu-xavier-rogue.s3.bhs.io.cloud.ovh.net/models/"

// mirrored is where the bucket holds a file pinned by its hash.
func mirrored(sha256, name string) string { return mirrors + sha256 + "/" + name }

func digest(s string) bool {
	return strings.HasPrefix(s, "sha256:") && sha256Hex.MatchString(strings.TrimPrefix(s, "sha256:"))
}

// What patai-fetch and patai-workload take on trust from the OS, held to what they'd accept, and to the recipe
// beside it.
func TestShippedManifests(t *testing.T) {
	entries, err := os.ReadDir(shipped)
	if err != nil {
		t.Fatal(err)
	}
	apps := 0
	images := map[string]ImagePin{}
	for _, entry := range entries {
		compose, err := os.ReadFile(filepath.Join(shipped, entry.Name(), "compose.yaml"))
		if !entry.IsDir() || err != nil {
			continue
		}
		name := entry.Name()
		m, err := ReadManifest(shipped, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		// An image a node pulls from the registry by digest: every layer's size, adding up to what it downloads.
		image := m.Image
		var layers int64
		for _, layer := range image.Layers {
			if !digest(layer.Digest) || layer.Size <= 0 {
				t.Errorf("%s: layer %+v", name, layer)
			}
			layers += layer.Size
		}
		if !digest(image.ID) || !digest(image.Digest) || !ghcrRef.MatchString(image.Ref) || len(image.Layers) == 0 ||
			image.Size != layers {
			t.Errorf("%s: image %+v", name, image)
		}
		if !strings.Contains(string(compose), "\n    image: "+image.Ref+"\n") && !strings.Contains(string(compose), "\n  image: "+image.Ref+"\n") {
			t.Errorf("%s: compose.yaml doesn't build %s", name, image.Ref)
		}
		// A recipe without a Dockerfile runs another's image, and has to pin it the same.
		if other, ok := images[image.Ref]; ok && !reflect.DeepEqual(other, image) {
			t.Errorf("%s: pins %s unlike another recipe", name, image.Ref)
		}
		images[image.Ref] = image
		// Each name is one file: a model's own, or one of its files, which two models can share.
		models, files := map[string]bool{}, map[string]ModelFile{}
		for _, model := range m.Models {
			models[filepath.Join(model.Dir, model.Name)] = true
		}
		for _, model := range m.Models {
			if !fileName.MatchString(model.Name) || (model.Dir != "" && !fileName.MatchString(model.Dir)) ||
				!sha256Hex.MatchString(model.SHA256) || model.Size <= 0 || !strings.HasPrefix(model.URL, "https://") ||
				model.Mirror != "" && model.Mirror != mirrored(model.SHA256, model.Name) {
				t.Errorf("%s: model %+v", name, model)
			}
			checkNotices(t, name, model)
			// A model's files reach the server by its settings, as the path it sees them at.
			for _, file := range model.Files {
				path := filepath.Join(model.Dir, file.Name)
				if other, ok := files[path]; models[path] || ok && other != file {
					t.Errorf("%s: %s is pinned as two files", name, path)
				}
				files[path] = file
				if !fileName.MatchString(file.Name) || !sha256Hex.MatchString(file.SHA256) || file.Size <= 0 ||
					!strings.HasPrefix(file.URL, "https://") || file.Mirror != "" && file.Mirror != mirrored(file.SHA256, file.Name) ||
					file.Mirror != "" && model.Mirror == "" {
					t.Errorf("%s: %s's file %+v", name, model.Name, file)
				}
				passed := false
				for _, value := range model.Env {
					passed = passed || strings.HasSuffix(value, "/"+filepath.Join(model.Dir, file.Name))
				}
				if !passed {
					t.Errorf("%s: %s doesn't pass on %s", name, model.Name, file.Name)
				}
			}
		}
		checkNetworks(t, name, string(compose), m.Person)
		// A folder on /data is backed up as it says: a Postgres one as a dump of the database its recipe makes.
		if d := m.Data; d != nil {
			// A Postgres folder keeps each cluster apart, and only one says it has one (docs/updates.md, Data across slots).
			if (d.Backup == BackupPostgres) != (d.Cluster != "") {
				t.Errorf("%s: a cluster without a Postgres folder, or the other way round", name)
			}
			// Its image runs the cluster it says, and has the binaries for the majors it says it upgrades from.
			if d.Cluster != "" {
				dockerfile, _ := os.ReadFile(filepath.Join(shipped, name, "Dockerfile"))
				upgrades := strings.Trim(fmt.Sprint(d.Upgrades), "[]")
				if !strings.Contains(string(dockerfile), "\nARG CLUSTER="+d.Cluster+"\n") ||
					!strings.Contains(string(dockerfile), "\nARG UPGRADES="+upgrades+"\n") {
					t.Errorf("%s: its Dockerfile's CLUSTER and UPGRADES aren't %s and %v", name, d.Cluster, d.Upgrades)
				}
			}
			if d.Postgres != "" {
				if other, err := ReadManifest(shipped, d.Postgres); err != nil || other.Data == nil || other.Data.Backup != BackupPostgres {
					t.Errorf("%s: its database is in %s, which isn't a Postgres service", name, d.Postgres)
				}
			}
			mount := "/data/" + name
			switch d.Backup {
			case BackupCopy, BackupLink:
				if d.Database != "" || d.User != "" {
					t.Errorf("%s: a database for a folder that isn't backed up as one", name)
				}
			case BackupPostgres:
				if !envValue.MatchString(d.Database) || !envValue.MatchString(d.User) || d.Database == "" || d.User == "" ||
					!strings.Contains(string(compose), "\n      - POSTGRES_DB="+d.Database+"\n") ||
					!strings.Contains(string(compose), "\n      - POSTGRES_USER="+d.User+"\n") {
					t.Errorf("%s: backs up a database its recipe doesn't make: %+v", name, d)
				}
				mount = "\n      - /data/" + name + ":/var/lib/postgresql\n"
			default:
				t.Errorf("%s: no backup is %q", name, d.Backup)
			}
			if d.UID <= 0 || !strings.Contains(string(compose), mount) {
				t.Errorf("%s: its folder on /data isn't mounted as its data says", name)
			}
			// A cache is a folder of its own at the top of the service's, as a backup leaves it out by name.
			for _, cache := range d.Cache {
				if cache == "" || cache == "." || cache == ".." || strings.ContainsAny(cache, "/\\\x00") ||
					d.Backup == BackupPostgres {
					t.Errorf("%s: a backup can't leave out %q", name, cache)
				}
			}
		}
		if !IsService(shipped, name) {
			continue
		}
		apps++
		if m.Title == "" {
			t.Errorf("%s: no title", name)
		}
		defaults := 0
		for _, model := range m.Models {
			if model.Title == "" {
				t.Errorf("%s: %s has no title", name, model.Name)
			}
			if model.Default {
				defaults++
			}
		}
		if len(m.Models) > 0 && defaults == 0 {
			t.Errorf("%s: no model is kept by default", name)
		}
		for _, form := range m.Model.Formats {
			if form != "gguf" && form != "safetensors" {
				t.Errorf("%s: owners' models can't be %q, which patai-fetch can't check", name, form)
			}
		}
		if m.Model.Env != "" && (!envKey.MatchString(m.Model.Env) || !strings.Contains(string(compose), "${"+m.Model.Env)) {
			t.Errorf("%s: its model goes to %s, which its recipe doesn't use", name, m.Model.Env)
		}
		if image.Unpacked <= 0 {
			t.Errorf("%s: no unpacked size, which patai-fetch needs room for", name)
		}
		// One with no HTTP has a health check of its own, which Docker runs. One run for each person answers at a path on
		// its own port on loopback, and Docker never restarts it.
		if m.Person {
			if !strings.HasPrefix(m.Ready, "/") || !strings.Contains(string(compose), "\n    restart: \"no\"\n") ||
				!strings.Contains(string(compose), "\"127.0.0.1::") {
				t.Errorf("%s: a person's own answers at %q, or isn't on loopback, or restarts", name, m.Ready)
			}
		} else if !strings.HasPrefix(m.Ready, "http://127.0.0.1:") && (m.Ready != "healthcheck" || !strings.Contains(string(compose), "\n    healthcheck:\n")) {
			t.Errorf("%s: answers at %q, not on loopback", name, m.Ready)
		}
		if _, ok := m.Profiles[m.Profile]; !ok {
			t.Errorf("%s: no profile %q", name, m.Profile)
		}
		// A setting reaches the server through the recipe: in it as ${KEY}, or passed into the container by name.
		uses := func(key string) bool {
			return strings.Contains(string(compose), "${"+key) || strings.Contains(string(compose), "\n      - "+key+"\n")
		}
		for id, profile := range m.Profiles {
			if profile.MemoryMiB <= 0 || profile.About == "" {
				t.Errorf("%s: profile %s has no budget or says nothing", name, id)
			}
			for key, value := range profile.Env {
				if !envKey.MatchString(key) || !envValue.MatchString(value) || !uses(key) {
					t.Errorf("%s: profile %s sets %s=%q, which its recipe doesn't use or can't be passed on", name, id, key, value)
				}
			}
		}
		for _, model := range m.Models {
			if len(model.Env) > 0 && m.Model.Env == "" {
				t.Errorf("%s: %s has settings, but every model runs at once", name, model.Name)
			}
			for key, value := range model.Env {
				if !envKey.MatchString(key) || !envValue.MatchString(value) || !uses(key) || key == m.Model.Env {
					t.Errorf("%s: %s sets %s=%q, which its recipe doesn't use or can't be passed on", name, model.Name, key, value)
				}
			}
		}
		checkSettings(t, name, m, uses)
		// Without the gate, Docker would start it by itself after a power cut (docs/apps.md).
		gate := "        source: /run/patai/apps/" + name + "\n"
		if !strings.Contains(string(compose), gate) || !strings.Contains(string(compose), "          create_host_path: false\n") {
			t.Errorf("%s: its server has no gate", name)
		}
	}
	if apps < 2 {
		t.Fatalf("%d apps", apps)
	}
}

// Every app names services this release has, and routes the gateway can send.
func TestShippedApps(t *testing.T) {
	apps := ReadApps(shippedApps)
	if len(apps) == 0 {
		t.Fatalf("%d apps", len(apps))
	}
	entries, _ := os.ReadDir(shippedApps)
	if len(entries) != len(apps) {
		t.Fatalf("%d folders, %d apps", len(entries), len(apps))
	}
	routes := map[string]string{}
	ports := map[int]string{}
	for _, app := range apps {
		if app.Title == "" || app.About == "" {
			t.Errorf("%s: no title or about", app.Name)
		}
		if app.UI != nil {
			if other, ok := ports[app.UI.Port]; ok {
				t.Errorf("%s: its pages are on %d, as %s's are", app.Name, app.UI.Port, other)
			}
			ports[app.UI.Port] = app.Name
		}
		if len(app.ServicesFor(apiNames(app.APIs))) == 0 {
			t.Errorf("%s: runs nothing", app.Name)
		}
		for _, name := range app.ServicesFor(apiNames(app.APIs)) {
			if !IsService(shipped, name) {
				t.Errorf("%s: no service %s", app.Name, name)
			}
		}
		if app.Key != "" {
			checkOwnKey(t, app)
		}
		// A service run for each person is only an app's of its own key, which says whose a request is, and nothing else.
		for _, api := range app.APIs {
			if !api.Person {
				continue
			}
			if m, err := ReadManifest(shipped, api.Service); err != nil || !m.Person || app.Key == "" || slices.Contains(app.Services, api.Service) {
				t.Errorf("%s: %s isn't a service of its own run for each person", app.Name, api.Name)
			}
		}
		// A route another API has can only be taken by one that can be off, which gives it back.
		owners := map[string]API{}
		names := map[string]bool{}
		for _, api := range app.APIs {
			if !ValidName(api.Name) || names[api.Name] || api.Title == "" || len(api.Routes) == 0 {
				t.Errorf("%s: API %+v", app.Name, api)
			}
			names[api.Name] = true
			// An app of its own key has routes of its own, which its key alone reaches.
			if app.Key != "" {
				for _, route := range api.Routes {
					if !strings.HasPrefix(route, "/") {
						t.Errorf("%s: route %s", app.Name, route)
					}
				}
				continue
			}
			for _, route := range api.Routes {
				earlier, taken := owners[route]
				if !strings.HasPrefix(route, "/v1/") || routes[route] != "" && !taken || taken && (api.Always || !earlier.Always) {
					t.Errorf("%s: route %s isn't under /v1/, or another API has it", app.Name, route)
				}
				routes[route] = app.Name + "/" + api.Name
				owners[route] = api
			}
		}
	}
}

// checkOwnKey holds an app with a key of its own to what patai-workload gives its services: the key and its secrets, as
// variables a recipe of its own passes on, and its pages from one of them.
func checkOwnKey(t *testing.T, app AppManifest) {
	t.Helper()
	passed := func(key string) bool {
		for _, name := range app.Services {
			compose, _ := os.ReadFile(filepath.Join(shipped, name, "compose.yaml"))
			if strings.Contains(string(compose), "${"+key+"}") || strings.Contains(string(compose), "\n      - "+key+"\n") {
				return true
			}
		}
		return false
	}
	if !envKey.MatchString(app.Key) || !passed(app.Key) {
		t.Errorf("%s: its key goes to %s, which none of its services passes on", app.Name, app.Key)
	}
	// An API the owner can turn off is one the app has to know is off, from PATAI_APIS.
	if slices.ContainsFunc(app.APIs, func(api API) bool { return !api.Always }) && !passed("PATAI_APIS") {
		t.Errorf("%s: none of its services passes on PATAI_APIS, so it can't know which of its APIs are on", app.Name)
	}
	for _, secret := range app.Secrets {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`).MatchString(secret.Name) || !envKey.MatchString(secret.Env) || !passed(secret.Env) {
			t.Errorf("%s: secret %+v isn't one its services pass on", app.Name, secret)
		}
	}
	if app.UI == nil {
		return
	}
	if !slices.Contains(app.Services, app.UI.Service) || app.UI.Port <= 1024 || app.UI.Port == 18000 {
		t.Errorf("%s: its pages %+v", app.Name, app.UI)
	}
	for prefix, service := range app.UI.Routes {
		if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") || !slices.Contains(app.Services, service) {
			t.Errorf("%s: its pages send %s to %s", app.Name, prefix, service)
		}
	}
}

// The services that reach outside the node, through patai-web. Every other stays on it but a person's computer, whose
// network the firewall lets out to the internet (docs/apps.md, The internet).
var outside = []string{"agentic-api", "chat-server", "searxng"}

var sharedNetwork = regexp.MustCompile(`\n    name: ([a-z0-9-]+)\n    external: true\n`)

// checkNetworks holds a recipe to the networks patai-workload makes as they should be: each it shares is in its
// manifest, patai-web only for those above, and one there isn't on its own network as well, which Docker could make its
// way out, refused by the firewall. Every other asks nobody outside for a name.
func checkNetworks(t *testing.T, name, compose string, person bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(shipped, name, "manifest.json"))
	var m struct {
		Networks []string `json:"networks"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	web := false
	for _, found := range sharedNetwork.FindAllStringSubmatch(compose, -1) {
		if !slices.Contains(m.Networks, found[1]) {
			t.Errorf("%s: joins %s, which its manifest doesn't name", name, found[1])
		}
		web = web || found[1] == "patai-web"
	}
	if web != slices.Contains(outside, name) || web && person {
		t.Errorf("%s: on patai-web is %v", name, web)
	}
	if web && (strings.Contains(compose, "\n      default:") || strings.Contains(compose, "[default")) {
		t.Errorf("%s: on its own network beside patai-web", name)
	}
	// Docker's resolver asks the node's for any other name, from the node, where the firewall can't refuse it.
	if refused := strings.Contains(compose, "\n    dns: [192.0.2.1]\n"); refused == (web || person) {
		t.Errorf("%s: asks 192.0.2.1 for names is %v", name, refused)
	}
}

// checkSettings holds a recipe's settings to what patai-workload takes: a name, a type it knows, a default it would
// accept, and a way to the server the recipe has. A model's defaults and limits are for settings that exist.
func checkSettings(t *testing.T, name string, m *Manifest, uses func(string) bool) {
	t.Helper()
	settingName := regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	declared := map[string]Setting{}
	for _, s := range m.Settings {
		if !settingName.MatchString(s.Name) || s.Title == "" || declared[s.Name].Name != "" {
			t.Errorf("%s: setting %+v", name, s)
		}
		declared[s.Name] = s
		// A default of null is the model's own, which the server isn't told.
		if err := s.Check(s.Default, nil); s.Default != nil && err != nil {
			t.Errorf("%s: %s's default: %v", name, s.Name, err)
		}
		reaches := s.Env != "" || len(s.Envs) > 0
		if s.For == "gateway" == reaches || s.For != "" && s.For != "gateway" {
			t.Errorf("%s: %s reaches the server and the gateway, or neither", name, s.Name)
		}
		if s.Env != "" && (!envKey.MatchString(s.Env) || !uses(s.Env)) {
			t.Errorf("%s: %s sets %s, which its recipe doesn't use", name, s.Name, s.Env)
		}
		for value, env := range s.Envs {
			for key, setting := range env {
				if !envKey.MatchString(key) || !envValue.MatchString(setting) || !uses(key) {
					t.Errorf("%s: %s=%s sets %s=%q, which its recipe doesn't use or can't be passed on", name, s.Name, value, key, setting)
				}
			}
		}
	}
	for _, secret := range m.Secrets {
		if !settingName.MatchString(secret.Name) || secret.Title == "" || !envKey.MatchString(secret.Env) || !uses(secret.Env) {
			t.Errorf("%s: secret %+v isn't one its recipe passes on", name, secret)
		}
	}
	for _, model := range m.Models {
		for key, value := range model.Settings {
			s, ok := declared[key]
			if !ok {
				t.Errorf("%s: %s has a default for %s, which isn't a setting", name, model.Name, key)
			} else if err := s.Check(value, model.Limits); err != nil {
				t.Errorf("%s: %s's default for %s: %v", name, model.Name, key, err)
			}
		}
		for key := range model.Limits {
			if _, ok := declared[key]; !ok {
				t.Errorf("%s: %s has a limit for %s, which isn't a setting", name, model.Name, key)
			}
		}
	}
}

// A mirrored model has its licence beside it in the bucket, and a notice is a file in its recipe's folder, named so
// that none of a model's notices takes another's place there, nor that of one of its files.
func checkNotices(t *testing.T, app string, model ModelPin) {
	t.Helper()
	if model.Mirror == "" {
		if len(model.Notices) > 0 {
			t.Errorf("%s: %s has notices and no mirror", app, model.Name)
		}
		return
	}
	names := map[string]bool{model.Name: true}
	for _, file := range model.Files {
		names[file.Name] = true
	}
	licence := false
	for _, notice := range model.Notices {
		base := filepath.Base(notice)
		info, err := os.Stat(filepath.Join(shipped, app, notice))
		if !strings.HasPrefix(notice, "notices/") || filepath.Clean(notice) != notice || err != nil ||
			!info.Mode().IsRegular() || info.Size() == 0 || names[base] {
			t.Errorf("%s: %s's notice %s", app, model.Name, notice)
		}
		names[base] = true
		licence = licence || base == "LICENSE"
	}
	if !licence {
		t.Errorf("%s: %s is mirrored without its LICENSE", app, model.Name)
	}
}

// What a bundle and a channel's latest.json carry of the release's data (docs/updates.md, Data across slots).
func TestShippedDataFormats(t *testing.T) {
	release, err := DataFormats(shipped)
	if err != nil {
		t.Fatal(err)
	}
	if release["chat-server"].Format == nil || release["chat-postgres"].Cluster == "" {
		t.Errorf("Chat's format and cluster: %+v", release)
	}
}
