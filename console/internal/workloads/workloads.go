// Package workloads is what the console knows about apps and the services they're made of (docs/apps.md,
// docs/apps.md): what each app's manifest names, what each service's recipe pins, what's on the disk, and how
// patai-fetch is doing. Both ship in the signed OS.
package workloads

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// Manifest is what a service's recipe declares (docs/apps.md, What an app declares), as far as this release uses it.
type Manifest struct {
	Name     string             `json:"name"`
	Title    string             `json:"title"`
	Version  string             `json:"version"`
	Licences map[string]string  `json:"licences"`
	GPU      bool               `json:"gpu"`
	Ready    string             `json:"ready"`
	Image    ImagePin           `json:"image"`
	Models   []ModelPin         `json:"models"`
	Model    ModelUse           `json:"model"`
	Profile  string             `json:"profile"`
	Profiles map[string]Profile `json:"profiles"`
	Settings []Setting          `json:"settings,omitempty"`
	Secrets  []Secret           `json:"secrets,omitempty"`
}

// Secret is one an owner gives a service, such as a provider's key, or that it Made for itself, kept on the unit and
// passed as Env. One given Once, such as the password of an account the service makes at its first start, is passed
// until it has answered, and then removed. Nothing shows one once it's set.
type Secret struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Env   string `json:"env"`
	Made  bool   `json:"made,omitempty"`
	Once  bool   `json:"once,omitempty"`
}

// SecretState is an owner's secret and whether it's set, or for one given once, whether it was Used.
type SecretState struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Set   bool   `json:"set"`
	Once  bool   `json:"once,omitempty"`
	Used  bool   `json:"used,omitempty"`
}

// Setting is one an owner can change, with its default. It reaches the server as Env, or as one of Envs by its value,
// or, For the gateway, not at all. A pinned model can have its own default, and a limit lower than Max.
type Setting struct {
	Name    string                       `json:"name"`
	Title   string                       `json:"title"`
	About   string                       `json:"about,omitempty"`
	Type    string                       `json:"type"` // int, number, choice or bool
	Min     *float64                     `json:"min,omitempty"`
	Max     *float64                     `json:"max,omitempty"`
	Choices []any                        `json:"choices,omitempty"`
	Labels  map[string]string            `json:"labels,omitempty"` // what a choice is called, where its value won't do
	Unit    string                       `json:"unit,omitempty"`
	Default any                          `json:"default"`
	Env     string                       `json:"env,omitempty"`
	Envs    map[string]map[string]string `json:"envs,omitempty"`
	For     string                       `json:"for,omitempty"`
}

// Check says why a value, as JSON decodes it, can't be the setting's, as patai-workload would: of its type, one of
// its choices, and within its range, or the model's limit where it has one.
func (s Setting) Check(value any, limits map[string]float64) error {
	if value == nil {
		return fmt.Errorf("%s needs a value", s.Title)
	}
	switch s.Type {
	case "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s is on or off", s.Title)
		}
		return nil
	case "choice":
		if !slices.Contains(s.Choices, value) {
			return fmt.Errorf("%s can't be %v", s.Title, value)
		}
		return nil
	case "int", "number":
	default:
		return fmt.Errorf("%s has no type this release knows", s.Title)
	}
	number, ok := value.(float64)
	if !ok || s.Type == "int" && number != float64(int64(number)) {
		return fmt.Errorf("%s is a number", s.Title)
	}
	if len(s.Choices) > 0 && !slices.Contains(s.Choices, value) {
		return fmt.Errorf("%s can't be %v", s.Title, value)
	}
	high := s.Max
	if limit, ok := limits[s.Name]; ok {
		high = &limit
	}
	if s.Min != nil && number < *s.Min || high != nil && number > *high {
		return fmt.Errorf("%s is out of its range", s.Title)
	}
	return nil
}

// Settings are a service's settings for the model it runs, as patai-workload has them: each value and its default
// for that model, which the owner changed, and the model's own limits.
type Settings struct {
	Model    string             `json:"model"`
	Values   map[string]any     `json:"values"`
	Defaults map[string]any     `json:"defaults"`
	Changed  []string           `json:"changed"`
	Limits   map[string]float64 `json:"limits"`
}

// ImagePin is the image a node pulls from the registry: its reference, the digest it's pulled by and the ID Docker
// gives it. Size is what it downloads, the sum of its layers, and Unpacked what it takes on the disk.
type ImagePin struct {
	Ref      string  `json:"ref"`
	Digest   string  `json:"digest"`
	ID       string  `json:"id"`
	Size     int64   `json:"size"`
	Unpacked int64   `json:"unpacked,omitempty"`
	Layers   []Layer `json:"layers,omitempty"`
}

// Layer is one of an image's layers as the registry has it, compressed.
type Layer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// ModelPin is a model, in Dir under the app's folder in /data/models. Default ones are kept unless an owner says
// otherwise. Mirror is where this project's GitHub release holds it, in pieces, tried before URL. Files are more it
// needs beside it, such as a vision projector, kept with it. Env is what it needs over the profile's settings while
// it runs.
type ModelPin struct {
	Name     string             `json:"name"`
	Title    string             `json:"title"`
	Default  bool               `json:"default"`
	Dir      string             `json:"dir"`
	SHA256   string             `json:"sha256"`
	Size     int64              `json:"size"`
	URL      string             `json:"url"`
	Mirror   string             `json:"mirror,omitempty"`
	Files    []ModelFile        `json:"files,omitempty"`
	Env      map[string]string  `json:"env,omitempty"`
	Settings map[string]any     `json:"settings,omitempty"`
	Limits   map[string]float64 `json:"limits,omitempty"`
}

// ModelFile is one of a pinned model's files beside its own.
type ModelFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
	Mirror string `json:"mirror,omitempty"`
}

// ModelUse is how an app takes its models. With Env, it runs one at a time, named by that setting; without, it
// sees every model it has. Formats are what an owner's own can be, and Dir is where they go.
type ModelUse struct {
	Env     string   `json:"env"`
	Dir     string   `json:"dir"`
	Formats []string `json:"formats"`
}

// Library is an app's models as patai-workload keeps them: which are kept on the unit, which runs, and the owner's
// own.
type Library struct {
	Have []string   `json:"have"`
	Use  string     `json:"use"`
	Own  []OwnModel `json:"own"`
}

// OwnModel is a model an owner added, with the hash taken when it was. URL is empty for an upload.
type OwnModel struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
}

// Profile is a set of settings and the most memory they were measured to need, its own and the GPU's.
type Profile struct {
	About     string            `json:"about"`
	Env       map[string]string `json:"env"`
	MemoryMiB int64             `json:"memory_mib"`
}

var appName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName is true of a name an app could have.
func ValidName(name string) bool { return appName.MatchString(name) }

// ReadManifest reads the recipe's manifest. One that names another app isn't this one's.
func ReadManifest(recipes, name string) (*Manifest, error) {
	if !appName.MatchString(name) {
		return nil, fmt.Errorf("bad app name")
	}
	data, err := os.ReadFile(filepath.Join(recipes, name, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s/manifest.json: %w", name, err)
	}
	if m.Name != name {
		return nil, fmt.Errorf("%s/manifest.json names %q", name, m.Name)
	}
	return &m, nil
}

// IsService is true of a recipe that can serve: one with a manifest and a server. Others are run by hand.
func IsService(recipes, name string) bool {
	if _, err := ReadManifest(recipes, name); err != nil {
		return false
	}
	compose, err := os.ReadFile(filepath.Join(recipes, name, "compose.yaml"))
	return err == nil && server.Match(compose)
}

// ServiceNames lists the recipes that can serve, by name.
func ServiceNames(recipes string) []string {
	names := []string{}
	entries, _ := os.ReadDir(recipes)
	for _, entry := range entries {
		if entry.IsDir() && IsService(recipes, entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

const (
	// Upload is where patai-api puts a model it receives, for patai-fetch to copy.
	Upload   = "/data/.uploads/model"
	Recipes  = "/usr/share/patai/workloads"
	AppsDir  = "/usr/share/patai/apps"
	Progress = "/run/patai/fetch.json"
	Models   = "/data/models"
	Data     = "/data/workloads"
)

// Part is an image or a model. Here means it has been checked against its pin. Unchecked is a file that
// looks right and hasn't been, such as one on a disk kept from before. Partial is how much of a download
// is waiting to be carried on. For a model: Kept is on the unit's list to have, InUse runs, and Own is an owner's.
type Part struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size"`
	Here      bool   `json:"here"`
	Unchecked bool   `json:"unchecked,omitempty"`
	Partial   int64  `json:"partial,omitempty"`
	Kept      bool   `json:"kept,omitempty"`
	InUse     bool   `json:"in_use,omitempty"`
	Own       bool   `json:"own,omitempty"`
	Source    string `json:"source,omitempty"`
}

// Service is one of an app's services as it runs now. State is Docker's, or absent with no container. Healthy is
// its ready check, while it runs. Memory is its own, resident and swapped, and GPUMemory nvmap's count for its
// processes; either is nil when it can't be read.
type Service struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Healthy   *bool  `json:"healthy"`
	StartedAt string `json:"started_at,omitempty"`
	Restarts  int    `json:"restarts"`
	Memory    *int64 `json:"memory"`
	GPUMemory *int64 `json:"gpu_memory"`
}

// Stack is one recipe that can serve: a service, with its containers as Services. OneModel is true of one that runs
// one model at a time, and Formats are what an owner's own models can be.
type Stack struct {
	Name     string        `json:"name"`
	Title    string        `json:"title"`
	Version  string        `json:"version"`
	Services []Service     `json:"services"`
	Image    Part          `json:"image"`
	Models   []Part        `json:"models"`
	OneModel bool          `json:"one_model"`
	Formats  []string      `json:"formats"`
	Settings []Setting     `json:"settings"`
	Values   *Settings     `json:"values,omitempty"`
	Secrets  []SecretState `json:"secrets"`
}

// Fetch is what patai-fetch last said: Done of Total bytes, for a download.
type Fetch struct {
	Stack  string `json:"stack"`
	Step   string `json:"step"` // image, model, checking, loading, ready, failed or stopped
	Total  int64  `json:"total"`
	Done   int64  `json:"done"`
	Source string `json:"source"`
	Detail string `json:"detail"`
	Adding string `json:"adding,omitempty"` // an owner's model, if that's what it's getting
	Time   string `json:"time"`
}

type Status struct {
	Chosen  []string    `json:"chosen"`  // the apps that are on
	Running []string    `json:"running"` // the services they need
	Apps    []AppStatus `json:"apps"`
	Disk    string      `json:"disk"`
	Busy    bool        `json:"busy"`
	Fetch   *Fetch      `json:"fetch"`
	Stacks  []Stack     `json:"stacks"`
	Memory  *Memory     `json:"memory"`
}

// Memory is what the apps that are on need, by each service's measured peak, against the budget for apps, in MiB.
type Memory struct {
	Budget   int64            `json:"budget"`
	Services map[string]int64 `json:"services"`
}

var server = regexp.MustCompile(`(?m)^  server:$`)

func size(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// downloaded is how much of a download patai-fetch has done, from the record it keeps beside it: the file itself is
// its full size from the start, since its pieces arrive at once.
func downloaded(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var progress struct {
		Bytes int64 `json:"bytes"`
	}
	if json.Unmarshal(data, &progress) != nil || progress.Bytes < 0 {
		return 0
	}
	return progress.Bytes
}

// Marker is what patai-fetch records once it has checked a model: the hash, and the file that hash was
// true of. Any change to a file moves its ctime, and nothing in a container can set that. A stack's markers
// share a file, a line each.
func Marker(sha256 string, info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%s %d %d", sha256, stat.Ino, stat.Ctim.Nano())
}

func marked(path, marker string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

// ReadStacks lists the recipes that can serve, and what of each is on the disk. imageHere asks Docker, and
// is nil when Docker can't be asked. data is /data/workloads and models is /data/models. libraries are each app's
// models as patai-workload keeps them: without one, an app keeps its defaults.
func ReadStacks(recipes, data, models string, imageHere func(id string) bool, libraries map[string]Library,
	settings map[string]Settings, secrets map[string]map[string]bool, used map[string][]string) []Stack {
	stacks := []Stack{}
	for _, name := range ServiceNames(recipes) {
		m, err := ReadManifest(recipes, name)
		if err != nil {
			continue
		}
		stack := Stack{Name: name, Title: m.Title, Version: m.Version, Models: []Part{}, OneModel: m.Model.Env != "",
			Formats: []string{}, Services: []Service{}, Settings: []Setting{}}
		stack.Settings = append(stack.Settings, m.Settings...)
		if values, ok := settings[name]; ok {
			stack.Values = &values
		}
		stack.Secrets = []SecretState{}
		for _, secret := range m.Secrets {
			if !secret.Made {
				stack.Secrets = append(stack.Secrets, SecretState{Name: secret.Name, Title: secret.Title, Set: secrets[name][secret.Name],
					Once: secret.Once, Used: secret.Once && slices.Contains(used[name], secret.Name)})
			}
		}
		stack.Formats = append(stack.Formats, m.Model.Formats...)
		stack.Image = Part{Name: m.Image.Ref, Size: m.Image.Size}
		if imageHere != nil {
			stack.Image.Here = imageHere(m.Image.ID)
		}
		lib, known := libraries[name]
		pins := append([]ModelPin{}, m.Models...)
		for _, own := range lib.Own {
			pins = append(pins, ModelPin{Name: own.Name, Dir: m.Model.Dir, SHA256: own.SHA256, Size: own.Size, URL: own.URL})
		}
		for i, pin := range pins {
			model := Part{Name: pin.Name, Title: pin.Title, Own: i >= len(m.Models)}
			if model.Own && pin.URL != "" {
				model.Source = host(pin.URL)
			}
			model.Kept = pin.Default
			if known {
				model.Kept = slices.Contains(lib.Have, pin.Name)
				model.InUse = lib.Use == pin.Name
			}
			// The model is here once its files are too, and its size is theirs together.
			files := append([]ModelFile{{Name: pin.Name, SHA256: pin.SHA256, Size: pin.Size}}, pin.Files...)
			present, checked := 0, 0
			for _, file := range files {
				model.Size += file.Size
				info, err := os.Lstat(filepath.Join(models, name, pin.Dir, file.Name))
				if err == nil && info.Mode().IsRegular() && info.Size() == file.Size {
					present++
					model.Partial += file.Size
					if marked(filepath.Join(data, "."+name+"-fetched"), Marker(file.SHA256, info)) {
						checked++
					}
				} else {
					model.Partial += downloaded(filepath.Join(data, ".partial", file.Name+".progress"))
				}
			}
			model.Here = checked == len(files)
			model.Unchecked = present == len(files) && !model.Here
			if model.Here || model.Unchecked {
				model.Partial = 0
			}
			stack.Models = append(stack.Models, model)
		}
		stacks = append(stacks, stack)
	}
	return stacks
}

// host is where an owner's model came from, without the rest of its address.
func host(address string) string {
	if parsed, err := url.Parse(address); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return ""
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ModelTarget splits a job's target, app/model, and checks each could be a name. The helper checks the rest.
func ModelTarget(target string) (app, model string, ok bool) {
	app, model, ok = strings.Cut(target, "/")
	return app, model, ok && appName.MatchString(app) && modelName.MatchString(model)
}

// ReadFetch reads what patai-fetch left. If nothing is running, then whatever it was doing has stopped:
// someone cancelled it, or it was killed, and it says nothing for itself.
func ReadFetch(path string, busy bool) *Fetch {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var fetch Fetch
	if json.Unmarshal(data, &fetch) != nil {
		return nil
	}
	switch fetch.Step {
	case "image", "model", "checking", "loading":
		if !busy {
			fetch.Step = "stopped"
		}
	}
	return &fetch
}
