package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

// fakeRootd answers update-status from a script of answers, keeping the last.
type fakeRootd struct {
	mu       sync.Mutex
	statuses []string
	failures int
	verbs    []string
}

func (f *fakeRootd) Call(ctx context.Context, req rootd.Request) (rootd.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verbs = append(f.verbs, req.Verb+" "+req.Target)
	if req.Verb != "update-status" {
		return rootd.Response{OK: true}, nil
	}
	if f.failures > 0 {
		f.failures--
		return rootd.Response{}, errors.New("no answer")
	}
	status := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return rootd.Response{OK: true, Output: status}, nil
}

const (
	stillBusy = `{"busy":true,"history":[{"version":"v2","state":"running"}]}`
	worked    = `{"history":[{"job":1,"version":"v2","state":"ok","detail":"installed v2"}]}`
	refused   = `{"history":[{"job":1,"version":"v2","state":"failed","detail":"refused"}]}`
	nothing   = `{"history":[{"version":"v1","state":"ok"}]}`
	earlier   = `{"history":[{"job":99,"version":"v2","state":"ok"}]}`
	overtaken = `{"history":[{"version":"v3","state":"ok"},{"job":1,"version":"v2","state":"ok","detail":"installed v2"}]}`
)

func rig(t *testing.T, f *fakeRootd, kind string) (*Runner, store.Job) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	j, _, err := st.CreateJob(store.Job{Kind: kind, Target: "v2", Actor: "ada", BootID: "boot-1"}, "key", store.Audit{Action: kind})
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, f, "boot-1")
	r.InstallPoll = time.Millisecond
	return r, j
}

func runUntilDone(t *testing.T, r *Runner, id int64) store.Job {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		j, err := r.Store.Job(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State == store.JobSucceeded || j.State == store.JobFailed {
			return j
		}
	}
	t.Fatal("the job never finished")
	return store.Job{}
}

func TestAnInstallFinishesWhenTheServiceDoes(t *testing.T) {
	f := &fakeRootd{statuses: []string{stillBusy, stillBusy, worked}, failures: 2}
	r, j := rig(t, f, "update-install")
	got := runUntilDone(t, r, j.ID)
	if got.State != store.JobSucceeded || got.Result != "installed v2" {
		t.Fatalf("%+v", got)
	}
	if f.verbs[0] != "update-install v2" {
		t.Fatal(f.verbs)
	}
}

func TestAnInstallSaysWhyItFailed(t *testing.T) {
	r, j := rig(t, &fakeRootd{statuses: []string{refused}}, "update-upload")
	if got := runUntilDone(t, r, j.ID); got.State != store.JobFailed || got.Result != "refused" {
		t.Fatalf("%+v", got)
	}
}

func TestAnInstallThatNeverStartedIsNoSuccess(t *testing.T) {
	r, j := rig(t, &fakeRootd{statuses: []string{nothing}}, "update-install")
	if got := runUntilDone(t, r, j.ID); got.State != store.JobFailed {
		t.Fatalf("%+v", got)
	}
}

// The console died after it marked the job running and before it asked for the install.
func TestAnEarlierInstallOfTheSameReleaseProvesNothing(t *testing.T) {
	r, j := rig(t, &fakeRootd{statuses: []string{earlier}}, "update-install")
	if err := r.Store.SetJob(j.ID, store.JobRunning, "", "boot-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := runUntilDone(t, r, j.ID); got.State != store.JobFailed {
		t.Fatalf("%+v", got)
	}
}

func TestAnInstallIsFoundEvenIfTheTimerRanSince(t *testing.T) {
	r, j := rig(t, &fakeRootd{statuses: []string{overtaken}}, "update-install")
	if got := runUntilDone(t, r, j.ID); got.State != store.JobSucceeded {
		t.Fatalf("%+v", got)
	}
}

func TestAnInstallOutlivesARestartOfTheConsole(t *testing.T) {
	f := &fakeRootd{statuses: []string{stillBusy, worked}}
	r, j := rig(t, f, "update-install")
	if err := r.Store.SetJob(j.ID, store.JobRunning, "", "boot-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Job(j.ID); got.State != store.JobRunning {
		t.Fatalf("%+v", got)
	}
	if got := runUntilDone(t, r, j.ID); got.State != store.JobSucceeded {
		t.Fatalf("%+v", got)
	}
	for _, verb := range f.verbs {
		if verb == "update-install v2" {
			t.Fatal("it asked for the install a second time")
		}
	}
}

func TestAPowerCutEndsAnInstallJob(t *testing.T) {
	r, j := rig(t, &fakeRootd{statuses: []string{worked}}, "update-install")
	if err := r.Store.SetJob(j.ID, store.JobRunning, "", "boot-0"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Store.Job(j.ID); got.State != store.JobFailed {
		t.Fatalf("%+v", got)
	}
}
