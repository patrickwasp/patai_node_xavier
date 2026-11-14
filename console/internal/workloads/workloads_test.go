package workloads

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const compose = "services:\n  server:\n    image: x\n"

func recipe(t *testing.T, root, name, composeFile string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFile), 0o644)
	for file, text := range files {
		os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644)
	}
}

type rig struct{ recipes, data, models string }

func newRig(t *testing.T) rig {
	t.Helper()
	root := t.TempDir()
	r := rig{filepath.Join(root, "recipes"), filepath.Join(root, "data"), filepath.Join(root, "models")}
	os.MkdirAll(filepath.Join(r.data, ".partial"), 0o755)
	recipe(t, r.recipes, "llama", compose, map[string]string{"manifest.json": `{"name": "llama", "version": "1",
		"image": {"ref": "x", "id": "sha256:llama", "digest": "sha256:llama", "size": 1000},
		"models": [{"name": "m.gguf", "dir": "", "sha256": "aaa", "size": 5, "url": "https://example.com/m.gguf"}]}`})
	recipe(t, r.recipes, "comfy", compose, map[string]string{"manifest.json": `{"name": "comfy", "version": "2",
		"image": {"ref": "x", "id": "sha256:comfy", "digest": "sha256:comfy", "size": 2000},
		"models": [{"name": "c.safetensors", "dir": "checkpoints", "sha256": "bbb", "size": 3, "url": "https://example.com/c"}]}`})
	recipe(t, r.recipes, "yolo", "services:\n  bench:\n    image: x\n", map[string]string{"manifest.json": `{"name": "yolo",
		"image": {"ref": "x", "id": "sha256:yolo", "digest": "sha256:yolo", "size": 3000}, "models": []}`})
	return r
}

func (r rig) stacks(here func(string) bool) []Stack {
	return ReadStacks(r.recipes, r.data, r.models, here, nil, nil, nil, nil)
}

func TestOnlyStacksThatCanServe(t *testing.T) {
	stacks := newRig(t).stacks(nil)
	if len(stacks) != 2 || stacks[0].Name != "comfy" || stacks[1].Name != "llama" || stacks[1].Version != "1" {
		t.Fatalf("%+v", stacks)
	}
	image, models := stacks[1].Image, stacks[1].Models
	if image.Name != "x" || image.Size != 1000 || image.Here || len(models) != 1 {
		t.Fatalf("%+v %+v", image, models)
	}
	if model := models[0]; model.Name != "m.gguf" || model.Size != 5 || model.Here {
		t.Fatalf("%+v", model)
	}
}

func TestAStackWithNoModel(t *testing.T) {
	r := newRig(t)
	recipe(t, r.recipes, "plain", compose, map[string]string{"manifest.json": `{"name": "plain",
		"image": {"ref": "x", "id": "sha256:plain", "digest": "sha256:plain", "size": 1}, "models": []}`})
	found := false
	for _, stack := range r.stacks(nil) {
		if stack.Name == "plain" {
			found = true
			if stack.Models == nil || len(stack.Models) != 0 {
				t.Fatalf("%+v", stack)
			}
		}
	}
	if !found {
		t.Fatal("not listed")
	}
}

func TestALibrary(t *testing.T) {
	r := newRig(t)
	recipe(t, r.recipes, "pick", compose, map[string]string{"manifest.json": `{"name": "pick",
		"image": {"ref": "x", "id": "sha256:pick", "digest": "sha256:pick", "size": 1},
		"models": [{"name": "a.gguf", "title": "A", "default": true, "dir": "", "sha256": "aaa", "size": 3, "url": "https://e/a"},
			{"name": "b.gguf", "title": "B", "dir": "", "sha256": "bbb", "size": 4, "url": "https://e/b"}],
		"model": {"env": "PICK_MODEL", "dir": "", "formats": ["gguf"]}}`})
	find := func(stacks []Stack) Stack {
		for _, stack := range stacks {
			if stack.Name == "pick" {
				return stack
			}
		}
		t.Fatal("not listed")
		return Stack{}
	}
	plain := find(r.stacks(nil))
	if !plain.OneModel || len(plain.Formats) != 1 || !plain.Models[0].Kept || plain.Models[1].Kept || plain.Models[0].Title != "A" {
		t.Fatalf("defaults: %+v", plain)
	}
	mine := r.model(t, "pick", "", "mine.gguf", "12345")
	os.WriteFile(filepath.Join(r.data, ".pick-fetched"), []byte(markerFor(t, "ccc", mine)+"\n"), 0o644)
	lib := Library{Have: []string{"b.gguf", "mine.gguf"}, Use: "mine.gguf",
		Own: []OwnModel{{Name: "mine.gguf", SHA256: "ccc", Size: 5, URL: "https://example.com/x/mine.gguf"}}}
	got := find(ReadStacks(r.recipes, r.data, r.models, nil, map[string]Library{"pick": lib}, nil, nil, nil))
	if len(got.Models) != 3 || got.Models[0].Kept || !got.Models[1].Kept || got.Models[1].InUse {
		t.Fatalf("pinned: %+v", got.Models)
	}
	if own := got.Models[2]; !own.Own || !own.Kept || !own.InUse || !own.Here || own.Source != "example.com" {
		t.Fatalf("own: %+v", own)
	}
}

func TestModelTarget(t *testing.T) {
	if app, model, ok := ModelTarget("llama-cpp/Qwen2.5-7B.gguf"); !ok || app != "llama-cpp" || model != "Qwen2.5-7B.gguf" {
		t.Fatal(app, model, ok)
	}
	for _, bad := range []string{"llama-cpp", "llama-cpp/", "../x/y.gguf", "llama-cpp/../y", "llama-cpp/a/b", "/a"} {
		if _, _, ok := ModelTarget(bad); ok {
			t.Fatalf("%q", bad)
		}
	}
}

func TestARecipeNeedsItsOwnManifest(t *testing.T) {
	r := newRig(t)
	recipe(t, r.recipes, "bare", compose, nil)
	recipe(t, r.recipes, "borrowed", compose, map[string]string{"manifest.json": `{"name": "llama"}`})
	recipe(t, r.recipes, "broken", compose, map[string]string{"manifest.json": `{"name": "broken",`})
	if got := ServiceNames(r.recipes); len(got) != 2 || got[0] != "comfy" || got[1] != "llama" {
		t.Fatalf("%v", got)
	}
	for _, name := range []string{"bare", "borrowed", "broken", "yolo", "../llama", ""} {
		if IsService(r.recipes, name) {
			t.Fatalf("%q", name)
		}
	}
}

func TestSeveralModels(t *testing.T) {
	r := newRig(t)
	recipe(t, r.recipes, "pair", compose, map[string]string{"manifest.json": `{"name": "pair",
		"image": {"ref": "x", "id": "sha256:pair", "digest": "sha256:pair", "size": 1},
		"models": [{"name": "a.gguf", "dir": "", "sha256": "aaa", "size": 3, "url": "https://example.com/a"},
			{"name": "b.gguf", "dir": "more", "sha256": "bbb", "size": 4, "url": "https://example.com/b"}]}`})
	a := r.model(t, "pair", "", "a.gguf", "123")
	b := r.model(t, "pair", "more", "b.gguf", "1234")
	os.WriteFile(filepath.Join(r.data, ".pair-fetched"), []byte(markerFor(t, "aaa", a)+"\n"+markerFor(t, "bbb", b)+"\n"), 0o644)
	var pair Stack
	for _, stack := range r.stacks(nil) {
		if stack.Name == "pair" {
			pair = stack
		}
	}
	if len(pair.Models) != 2 || !pair.Models[0].Here || !pair.Models[1].Here {
		t.Fatalf("%+v", pair)
	}
}

// A model with a file beside it, such as a vision projector, is here once both are, and its size is both.
func TestAModelWithAFile(t *testing.T) {
	r := newRig(t)
	recipe(t, r.recipes, "sees", compose, map[string]string{"manifest.json": `{"name": "sees",
		"image": {"ref": "x", "id": "sha256:sees", "digest": "sha256:sees", "size": 1},
		"models": [{"name": "m.gguf", "dir": "", "sha256": "aaa", "size": 5, "url": "https://example.com/m",
			"files": [{"name": "p.gguf", "sha256": "ppp", "size": 3, "url": "https://example.com/p"}]}]}`})
	find := func() Part {
		for _, stack := range r.stacks(nil) {
			if stack.Name == "sees" {
				return stack.Models[0]
			}
		}
		t.Fatal("not listed")
		return Part{}
	}
	model := r.model(t, "sees", "", "m.gguf", "12345")
	r.mark(t, "sees", markerFor(t, "aaa", model))
	os.WriteFile(filepath.Join(r.data, ".partial", "p.gguf.progress"), []byte(`{"bytes": 1}`), 0o644)
	if got := find(); got.Size != 8 || got.Here || got.Unchecked || got.Partial != 6 {
		t.Fatalf("its file downloading: %+v", got)
	}
	projector := r.model(t, "sees", "", "p.gguf", "123")
	if got := find(); got.Here || !got.Unchecked || got.Partial != 0 {
		t.Fatalf("its file unchecked: %+v", got)
	}
	r.mark(t, "sees", markerFor(t, "aaa", model)+"\n"+markerFor(t, "ppp", projector))
	if got := find(); !got.Here || got.Unchecked {
		t.Fatalf("both checked: %+v", got)
	}
}

func TestImageIsAskedAboutByItsPin(t *testing.T) {
	stacks := newRig(t).stacks(func(id string) bool { return id == "sha256:llama" })
	if stacks[0].Image.Here || !stacks[1].Image.Here {
		t.Fatalf("%+v", stacks)
	}
}

func (r rig) model(t *testing.T, stack, inner, name, text string) string {
	t.Helper()
	dir := filepath.Join(r.models, stack, inner)
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, name)
	os.WriteFile(path, []byte(text), 0o644)
	return path
}

func (r rig) mark(t *testing.T, stack, marker string) {
	t.Helper()
	os.WriteFile(filepath.Join(r.data, "."+stack+"-fetched"), []byte(marker+"\n"), 0o644)
}

func markerFor(t *testing.T, sha256, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return Marker(sha256, info)
}

func TestAModelCountsOnlyOnceCheckedForThisPin(t *testing.T) {
	r := newRig(t)
	path := r.model(t, "llama", "", "m.gguf", "12345")
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("never checked")
	}
	r.mark(t, "llama", markerFor(t, "old", path))
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("checked for another pin")
	}
	// A line that names no file.
	r.mark(t, "llama", "aaa")
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("says nothing of the file")
	}
	r.mark(t, "llama", markerFor(t, "aaa", path))
	if !r.stacks(nil)[1].Models[0].Here {
		t.Fatal("checked for this one")
	}
	comfy := r.model(t, "comfy", "checkpoints", "c.safetensors", "123")
	r.mark(t, "comfy", markerFor(t, "bbb", comfy))
	if !r.stacks(nil)[0].Models[0].Here {
		t.Fatal("in its directory")
	}
}

func TestAFileThatLooksRightIsNotADownload(t *testing.T) {
	r := newRig(t)
	path := r.model(t, "llama", "", "m.gguf", "12345")
	if got := r.stacks(nil)[1].Models[0]; got.Here || !got.Unchecked {
		t.Fatalf("%+v", got)
	}
	r.mark(t, "llama", markerFor(t, "aaa", path))
	if got := r.stacks(nil)[1].Models[0]; !got.Here || got.Unchecked {
		t.Fatalf("%+v", got)
	}
	os.Truncate(path, 3)
	if got := r.stacks(nil)[1].Models[0]; got.Here || got.Unchecked {
		t.Fatalf("%+v", got)
	}
}

func TestAChangedModelStopsCounting(t *testing.T) {
	r := newRig(t)
	path := r.model(t, "llama", "", "m.gguf", "12345")
	r.mark(t, "llama", markerFor(t, "aaa", path))
	time.Sleep(20 * time.Millisecond)
	// The same size, and the same modification time if someone sets it back.
	info, _ := os.Stat(path)
	os.WriteFile(path, []byte("54321"), 0o644)
	os.Chtimes(path, info.ModTime(), info.ModTime())
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("changed since it was checked")
	}
}

func TestAShortenedModelStopsCounting(t *testing.T) {
	r := newRig(t)
	path := r.model(t, "llama", "", "m.gguf", "12345")
	r.mark(t, "llama", markerFor(t, "aaa", path))
	os.Truncate(path, 3)
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("cut short since")
	}
}

func TestALinkIsNotAModel(t *testing.T) {
	r := newRig(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, []byte("12345"), 0o644)
	os.MkdirAll(filepath.Join(r.models, "llama"), 0o755)
	link := filepath.Join(r.models, "llama", "m.gguf")
	os.Symlink(target, link)
	r.mark(t, "llama", markerFor(t, "aaa", link))
	if r.stacks(nil)[1].Models[0].Here {
		t.Fatal("a link a container could leave")
	}
}

func TestPartialDownloads(t *testing.T) {
	r := newRig(t)
	os.WriteFile(filepath.Join(r.data, ".partial", "m.gguf"), []byte("12345"), 0o644)
	os.WriteFile(filepath.Join(r.data, ".partial", "m.gguf.progress"), []byte(`{"chunk": 1, "done": [0, 1], "bytes": 2}`), 0o644)
	stack := r.stacks(nil)[1]
	if stack.Models[0].Partial != 2 || stack.Image.Partial != 0 {
		t.Fatalf("%+v %+v", stack.Image, stack.Models)
	}
}

func TestReadFetch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fetch.json")
	if ReadFetch(path, false) != nil {
		t.Fatal("nothing yet")
	}
	os.WriteFile(path, []byte(`{"stack":"llama","step":"model","done":4,"total":10,"source":"example.com"}`), 0o644)
	got := ReadFetch(path, true)
	if got == nil || got.Step != "model" || got.Done != 4 || got.Total != 10 {
		t.Fatalf("%+v", got)
	}
	// Cancelled or killed.
	if got := ReadFetch(path, false); got.Step != "stopped" || got.Done != 4 {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(path, []byte(`{"stack":"llama","step":"checking","total":10}`), 0o644)
	if got := ReadFetch(path, false); got.Step != "stopped" {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(path, []byte(`{"stack":"llama","step":"failed","detail":"no room"}`), 0o644)
	if got := ReadFetch(path, false); got.Step != "failed" || got.Detail != "no room" {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(path, []byte("nonsense"), 0o644)
	if ReadFetch(path, true) != nil {
		t.Fatal("unreadable")
	}
}
