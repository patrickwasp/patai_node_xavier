// Package jobs runs the console's long work, one job at a time, so that installing, switching slots
// and rebooting can't race. Jobs live in the store and outlive a restart of the daemon or the unit.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"slices"
	"time"

	"patai/console/internal/rootd"
	"patai/console/internal/store"
	"patai/console/internal/updates"
)

const (
	// How long a reboot or a shutdown may take to happen before the job says it didn't.
	rebootGrace = 5 * time.Minute
	// Streaming a bundle over a slow line can take an hour, and this only stops a stuck one holding up
	// the queue for good.
	installLimit = 2 * time.Hour
)

type Runner struct {
	Store       *store.Store
	Rootd       rootd.Caller
	BootID      string
	Name        string // this boot's hostname, which a rename job comes back to check
	InstallPoll time.Duration
	wake        chan struct{}
}

func New(s *store.Store, r rootd.Caller, bootID string) *Runner {
	return &Runner{Store: s, Rootd: r, BootID: bootID, InstallPoll: 2 * time.Second, wake: make(chan struct{}, 1)}
}

// installs run in a service of their own, so these jobs only watch.
func installs(kind string) bool { return kind == "update-install" || kind == "update-upload" }

// downs take the unit down, so they finish only when the next boot sees them.
func downs(kind string) bool { return kind == "reboot" || kind == "rename" || kind == "shutdown" }

// cameBack is what a new boot can say about the job that took the unit down. A shutdown and a start
// again can't be told from a cut and a start again, so it says no more than that.
func (r *Runner) cameBack(j store.Job) (string, string) {
	switch j.Kind {
	case "rename":
		if r.Name != j.Target {
			return store.JobFailed, "the unit restarted as " + r.Name
		}
		return store.JobSucceeded, "unit restarted as " + j.Target
	case "shutdown":
		return store.JobSucceeded, "unit stopped, and has started again"
	}
	return store.JobSucceeded, "unit restarted"
}

func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Reconcile settles what a restart interrupted. Nothing runs again by itself: a reboot queued before
// a power cut must not fire when the unit comes back.
func (r *Runner) Reconcile() error {
	open, err := r.Store.OpenJobs()
	if err != nil {
		return err
	}
	for _, j := range open {
		if j.BootID == r.BootID {
			// An install carries on without this daemon, and Run goes back to watching it.
			if j.State == store.JobRunning && !installs(j.Kind) {
				r.finish(j, store.JobFailed, "interrupted by a restart of the console")
			}
			continue
		}
		if downs(j.Kind) && j.State == store.JobRunning {
			state, result := r.cameBack(j)
			r.finish(j, state, result)
		} else {
			r.finish(j, store.JobFailed, "interrupted by a restart of the unit")
		}
	}
	return nil
}

func (r *Runner) finish(j store.Job, state, result string) {
	if err := r.Store.SetJob(j.ID, state, result, r.BootID); err != nil {
		log.Printf("job %d: %v", j.ID, err)
	}
}

// Run works through the queue until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	for {
		open, err := r.Store.OpenJobs()
		if err != nil {
			log.Printf("jobs: %v", err)
		}
		for _, j := range open {
			if j.State == store.JobQueued {
				r.run(ctx, j)
			} else if downs(j.Kind) {
				r.waitForReboot(ctx, j)
			} else if installs(j.Kind) {
				r.waitForInstall(ctx, j)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(time.Minute):
		}
	}
}

func (r *Runner) run(ctx context.Context, j store.Job) {
	if err := r.Store.SetJob(j.ID, store.JobRunning, "", r.BootID); err != nil {
		log.Printf("job %d: %v", j.ID, err)
		return
	}
	req := rootd.Request{Verb: j.Kind, Target: j.Target, Job: j.ID}
	resp, err := r.Rootd.Call(ctx, req)
	var refused *net.OpError
	switch {
	case errors.As(err, &refused):
		log.Printf("job %d: %v", j.ID, err)
		r.finish(j, store.JobFailed, "the unit's root helper isn't answering")
	case err != nil:
		r.finish(j, store.JobFailed, err.Error())
	case downs(j.Kind):
		// Finished only when the next boot sees it.
		r.waitForReboot(ctx, j)
	case installs(j.Kind):
		r.waitForInstall(ctx, j)
	default:
		r.finish(j, store.JobSucceeded, resp.Output)
	}
}

// waitForInstall watches patai-update until it stops. Whether it worked is in what it left behind, and
// a helper that doesn't answer for a while isn't an answer: the install carries on regardless.
func (r *Runner) waitForInstall(ctx context.Context, j store.Job) {
	limit := time.After(installLimit)
	for {
		select {
		case <-ctx.Done():
			return
		case <-limit:
			r.finish(j, store.JobFailed, "the update didn't finish")
			return
		case <-time.After(r.InstallPoll):
		}
		resp, err := r.Rootd.Call(ctx, rootd.Request{Verb: "update-status"})
		if err != nil {
			continue
		}
		var status updates.Status
		if err := json.Unmarshal([]byte(resp.Output), &status); err != nil || status.Busy {
			continue
		}
		// By job, not by release: this release may have been installed before, and a timer may have run since.
		attempt := slices.IndexFunc(status.History, func(i updates.Install) bool { return i.Job == j.ID })
		switch {
		case attempt < 0:
			r.finish(j, store.JobFailed, "the update didn't start")
		case status.History[attempt].State == "ok":
			r.finish(j, store.JobSucceeded, status.History[attempt].Detail)
		default:
			r.finish(j, store.JobFailed, status.History[attempt].Detail)
		}
		return
	}
}

func (r *Runner) waitForReboot(ctx context.Context, j store.Job) {
	select {
	case <-ctx.Done():
	case <-time.After(rebootGrace):
		if j.Kind == "shutdown" {
			r.finish(j, store.JobFailed, "the unit didn't stop")
		} else {
			r.finish(j, store.JobFailed, "the unit didn't restart")
		}
	}
}
