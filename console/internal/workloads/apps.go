package workloads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// AppManifest is an app: what an owner turns on, made of services (docs/apps.md). Each of its APIs is served by a
// service, and runs while that API is on. Services run whatever its APIs. An app with a Key of its own reaches its
// APIs with it, through the gateway for apps: the node makes it, and passes it to the app's Services as that variable,
// with the app's own made Secrets. Its APIs are then its own, which owners' keys can't use. UI is its own web pages.
type AppManifest struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	About    string   `json:"about"`
	APIs     []API    `json:"apis"`
	Services []string `json:"services"`
	Key      string   `json:"key,omitempty"`
	Secrets  []Secret `json:"secrets,omitempty"`
	UI       *UI      `json:"ui,omitempty"`
}

// UI is an app's own web pages, which the console serves from one of its services on a port of their own. People sign
// in to them with the app's own accounts. Routes sends the paths under a prefix to another of its services, such as an
// API beside the pages, so a browser sees one origin.
type UI struct {
	Port    int               `json:"port"`
	Service string            `json:"service"`
	Routes  map[string]string `json:"routes,omitempty"`
}

// ServiceFor is the service a path on the app's pages goes to: the one whose route is the longest prefix of it, or the
// pages' own.
func (ui *UI) ServiceFor(path string) string {
	service, longest := ui.Service, 0
	for prefix, name := range ui.Routes {
		if strings.HasPrefix(path, prefix) && len(prefix) > longest {
			service, longest = name, len(prefix)
		}
	}
	return service
}

// API is one of an app's APIs: the service behind it, others it runs With it, and the paths the gateway sends it. A
// route ending in / is every path under it. One Always on can't be turned off, and a Default one is on until an owner
// says otherwise. A route two APIs have goes to the later one while it's on. One whose service is run for each Person,
// such as Chat's terminal, starts nothing with its app: the gateway starts a person's own when they use it.
type API struct {
	Name    string   `json:"name"`
	Title   string   `json:"title"`
	About   string   `json:"about"`
	Service string   `json:"service"`
	With    []string `json:"with,omitempty"`
	Always  bool     `json:"always,omitempty"`
	Default bool     `json:"default,omitempty"`
	Person  bool     `json:"person,omitempty"`
	Routes  []string `json:"routes"`
}

// AppStatus is an app as it stands: whether it's on, which of its APIs are, and the services that gives it.
type AppStatus struct {
	AppManifest
	On      bool     `json:"on"`
	APIsOn  []string `json:"apis_on"`
	Running []string `json:"running"`
	Missing []string `json:"missing,omitempty"` // services it names that this release doesn't have
}

// ReadApp reads an app's manifest. One that names another app isn't this one's.
func ReadApp(dir, name string) (*AppManifest, error) {
	if !appName.MatchString(name) {
		return nil, fmt.Errorf("bad app name")
	}
	data, err := os.ReadFile(filepath.Join(dir, name, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m AppManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s/manifest.json: %w", name, err)
	}
	if m.Name != name {
		return nil, fmt.Errorf("%s/manifest.json names %q", name, m.Name)
	}
	return &m, nil
}

// ReadApps reads every app this release has, by name.
func ReadApps(dir string) []AppManifest {
	apps := []AppManifest{}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if m, err := ReadApp(dir, entry.Name()); err == nil && entry.IsDir() {
			apps = append(apps, *m)
		}
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps
}

// API finds one of the app's APIs by name.
func (m *AppManifest) API(name string) (API, bool) {
	for _, api := range m.APIs {
		if api.Name == name {
			return api, true
		}
	}
	return API{}, false
}

// ServicesFor is what the app runs with these APIs on, in order, as patai-workload has it: the services behind
// them, then the rest. A service run for each person isn't one.
func (m *AppManifest) ServicesFor(on []string) []string {
	names := []string{}
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, api := range m.APIs {
		if !api.Person && (api.Always || slices.Contains(on, api.Name)) {
			add(api.Service)
			for _, name := range api.With {
				add(name)
			}
		}
	}
	for _, name := range m.Services {
		add(name)
	}
	return names
}

// Shown is what patai-workload show says: the apps on, each app's APIs that are on, the services that gives, and
// each service's models. Used are the secrets given once that a service has used, and Keys each app's own key, as its
// SHA-256 in hex.
type Shown struct {
	Chosen   []string                   `json:"chosen"`
	APIs     map[string][]string        `json:"apis"`
	Services []string                   `json:"services"`
	Models   map[string]Library         `json:"models"`
	Settings map[string]Settings        `json:"settings"`
	Secrets  map[string]map[string]bool `json:"secrets"`
	Used     map[string][]string        `json:"used"`
	Keys     map[string]string          `json:"keys"`
	People   map[string][]string        `json:"people"` // each service run for each person: the people whose runs
	Memory   *Memory                    `json:"memory"`
}

// AppStatuses puts each app this release has together with what's chosen.
func AppStatuses(dir, recipes string, shown Shown) []AppStatus {
	list := []AppStatus{}
	for _, m := range ReadApps(dir) {
		app := AppStatus{AppManifest: m, On: slices.Contains(shown.Chosen, m.Name), APIsOn: shown.APIs[m.Name]}
		if app.APIsOn == nil {
			app.APIsOn = []string{}
		}
		app.Running = m.ServicesFor(app.APIsOn)
		for _, name := range m.ServicesFor(apiNames(m.APIs)) {
			if !IsService(recipes, name) {
				app.Missing = append(app.Missing, name)
			}
		}
		if app.APIs == nil {
			app.APIs = []API{}
		}
		list = append(list, app)
	}
	return list
}

func apiNames(apis []API) []string {
	names := []string{}
	for _, api := range apis {
		names = append(names, api.Name)
	}
	return names
}
