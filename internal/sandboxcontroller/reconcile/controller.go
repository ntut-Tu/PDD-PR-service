// Package reconcile is the control loop of the sandbox-controller. Each pass compares the
// SandboxRun records (desired state) with the containers labelled pdd.sandbox/run=<id> and the
// workspaces on disk (observed state) and repairs the difference. Every step is idempotent, so
// a crash at any point is repaired by the next pass.
package reconcile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/workspace"
)

// OCIRuntime is the only container runtime sandboxes run with.
const OCIRuntime = "runsc"

const stopGrace = 5 * time.Second

// Store is the persistence the loop needs.
type Store interface {
	List(ctx context.Context) ([]model.Run, error)
	UpdateStatus(ctx context.Context, id string, status model.Status) error
	Delete(ctx context.Context, id string) error
}

// Source provides the repository archive of a run.
type Source interface {
	Archive(ctx context.Context, spec model.Spec) (io.ReadCloser, error)
}

// Settings are the tunables of the loop.
type Settings struct {
	// OCIRuntime defaults to runsc. Only tests change it.
	OCIRuntime    string
	MaxConcurrent int
	Timeout       time.Duration
	CPUs          float64
	MemoryBytes   int64
	Pids          int64
	User          string

	NetworkName string
	// ProxyHost overrides the address sandboxes use to reach the proxy; empty derives it from
	// the network. ProxyPort is the port the proxy listens on.
	ProxyHost string
	ProxyPort string

	ImageMaven  string
	ImageGradle string
	// CacheDir is the root of the per-repository dependency caches.
	CacheDir string

	OutputMaxBytes     int
	WorkspaceRetention time.Duration
}

// Deps are the collaborators of the Controller.
type Deps struct {
	Store      Store
	Runtime    runtime.Runtime
	Workspaces *workspace.Manager
	Source     Source
	Log        *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Controller runs the reconcile loop.
type Controller struct {
	Deps
	set Settings

	trigger chan struct{}
	// mu serializes passes.
	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	wg       sync.WaitGroup
}

// New returns a Controller.
func New(deps Deps, set Settings) *Controller {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if set.OCIRuntime == "" {
		set.OCIRuntime = OCIRuntime
	}
	return &Controller{Deps: deps, set: set, trigger: make(chan struct{}, 1), inflight: map[string]context.CancelFunc{}}
}

// Trigger asks for a pass soon; it never blocks.
func (c *Controller) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles on every trigger and every interval until ctx ends, then waits for the
// runs that were being prepared.
func (c *Controller) Run(ctx context.Context, resync time.Duration) {
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("reconcile failed", "error", err)
		}
		select {
		case <-ctx.Done():
			c.Wait()
			return
		case <-ticker.C:
		case <-c.trigger:
		}
	}
}

// Wait blocks until all background preparation of runs has finished.
func (c *Controller) Wait() { c.wg.Wait() }

// SandboxAvailable reports whether runsc is registered with the container runtime.
func (c *Controller) SandboxAvailable(ctx context.Context) bool {
	return c.Runtime.CheckRuntime(ctx, c.set.OCIRuntime) == nil
}

// Reconcile runs one pass.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	runs, err := c.Store.List(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: list runs: %w", err)
	}
	containers, listErr := c.Runtime.List(ctx)
	if listErr != nil {
		c.Log.Warn("list containers failed; skipping container repairs", "error", listErr)
	}
	byRun := map[string][]runtime.Container{}
	for _, ct := range containers {
		byRun[ct.RunID] = append(byRun[ct.RunID], ct)
	}
	known := map[string]bool{}
	claimed := map[string]bool{}
	active := 0
	var pending []model.Run

	for _, run := range runs {
		known[run.ID] = true
		cts := byRun[run.ID]
		switch {
		case run.DeletionRequested:
			c.deleteRun(ctx, run, cts, listErr)
			continue
		case run.Status.Phase == model.Pending:
			pending = append(pending, run)
		case run.Status.Phase == model.Preparing || run.Status.Phase == model.Running:
			active++
			for _, ct := range cts {
				claimed[ct.ID] = true
			}
			if _, busy := c.inflight[run.ID]; busy || listErr != nil {
				continue
			}
			var finished bool
			if run.Status.Phase == model.Preparing {
				finished = c.adoptPreparing(ctx, run, cts)
			} else {
				finished = c.observeRunning(ctx, run, cts)
			}
			if finished {
				active--
			}
		default:
			c.retainTerminal(run)
		}
	}

	for _, run := range pending {
		if active >= c.set.MaxConcurrent {
			break
		}
		active++
		c.begin(ctx, run)
	}
	if listErr == nil {
		c.removeOrphanContainers(ctx, containers, claimed)
	}
	c.removeOrphanWorkspaces(known)
	return nil
}

// deleteRun removes everything of a run, then its record. A run that is still being prepared is
// cancelled and deleted by a later pass.
func (c *Controller) deleteRun(ctx context.Context, run model.Run, cts []runtime.Container, listErr error) {
	if cancel, busy := c.inflight[run.ID]; busy {
		cancel()
		return
	}
	if listErr != nil {
		return
	}
	for _, ct := range cts {
		if err := c.Runtime.Remove(ctx, ct.ID); err != nil {
			c.Log.Warn("remove container of deleted run", "run", run.ID, "error", err)
			return
		}
	}
	if err := c.Workspaces.Remove(run.ID); err != nil {
		c.Log.Warn("remove workspace of deleted run", "run", run.ID, "error", err)
		return
	}
	if err := c.Store.Delete(ctx, run.ID); err != nil {
		c.Log.Warn("delete run record", "run", run.ID, "error", err)
		return
	}
	c.Log.Info("run deleted", "run", run.ID)
}

// retainTerminal removes the workspace of a finished run once the retention has passed; the
// record stays.
func (c *Controller) retainTerminal(run model.Run) {
	if run.Status.FinishedAt == nil || c.Now().Sub(*run.Status.FinishedAt) < c.set.WorkspaceRetention {
		return
	}
	if err := c.Workspaces.Remove(run.ID); err != nil {
		c.Log.Warn("remove expired workspace", "run", run.ID, "error", err)
	}
}

// adoptPreparing repairs a run that was Preparing when the previous process stopped. It
// reports whether the run ended.
func (c *Controller) adoptPreparing(ctx context.Context, run model.Run, cts []runtime.Container) bool {
	if len(cts) == 0 {
		c.fail(ctx, &run, model.Error, model.Internal, "run was interrupted while preparing (controller restart)", nil)
		return true
	}
	ct := cts[0]
	if ct.State == runtime.StateCreated {
		if err := c.Runtime.Start(ctx, ct.ID); err != nil {
			c.fail(ctx, &run, model.Error, model.Internal, "start container: "+err.Error(), nil)
			return true
		}
		ct.StartedAt = c.Now()
	}
	started := ct.StartedAt
	if started.IsZero() {
		started = c.Now()
	}
	c.update(ctx, &run, func(s *model.Status) {
		s.Phase, s.Message, s.StartedAt = model.Running, "container started", &started
		s.SetCondition("Started", true, c.Now(), "container adopted after restart")
	})
	return false
}

// observeRunning checks the container of a Running run and reports whether the run ended.
func (c *Controller) observeRunning(ctx context.Context, run model.Run, cts []runtime.Container) bool {
	if len(cts) == 0 {
		c.fail(ctx, &run, model.Error, model.Internal, "container is gone (host restart?)", nil)
		return true
	}
	ct := cts[0]
	switch ct.State {
	case runtime.StateExited:
		c.collect(ctx, run, ct)
		return true
	case runtime.StateRunning:
		started := run.Status.StartedAt
		if started != nil && c.Now().Sub(*started) > c.set.Timeout {
			c.timeout(ctx, run, ct)
			return true
		}
		return false
	default:
		c.fail(ctx, &run, model.Error, model.Internal, "container never started", nil)
		return true
	}
}

func (c *Controller) removeOrphanContainers(ctx context.Context, containers []runtime.Container, claimed map[string]bool) {
	for _, ct := range containers {
		if claimed[ct.ID] {
			continue
		}
		if err := c.Runtime.Remove(ctx, ct.ID); err != nil {
			c.Log.Warn("remove orphan container", "container", ct.ID, "run", ct.RunID, "error", err)
			continue
		}
		c.Log.Info("removed orphan container", "container", ct.ID, "run", ct.RunID)
	}
}

func (c *Controller) removeOrphanWorkspaces(known map[string]bool) {
	names, err := c.Workspaces.List()
	if err != nil {
		c.Log.Warn("list workspaces", "error", err)
		return
	}
	for _, name := range names {
		if known[name] || !model.ValidID(name) {
			continue
		}
		if err := c.Workspaces.Remove(name); err != nil {
			c.Log.Warn("remove orphan workspace", "run", name, "error", err)
			continue
		}
		c.Log.Info("removed orphan workspace", "run", name)
	}
}

// update applies mutate to the run's status and stores it.
func (c *Controller) update(ctx context.Context, run *model.Run, mutate func(*model.Status)) bool {
	if ctx.Err() != nil {
		return false
	}
	status := run.Status
	status.Conditions = append([]model.Condition(nil), status.Conditions...)
	mutate(&status)
	if err := c.Store.UpdateStatus(ctx, run.ID, status); err != nil {
		c.Log.Error("update run status", "run", run.ID, "error", err)
		return false
	}
	run.Status = status
	return true
}

// fail ends a run in a terminal phase.
func (c *Controller) fail(ctx context.Context, run *model.Run, phase model.Phase, reason model.Reason, message string, result *model.Result) {
	c.Log.Info("run finished", "run", run.ID, "phase", phase, "reason", reason, "message", message)
	now := c.Now().UTC()
	c.update(ctx, run, func(s *model.Status) {
		s.Phase, s.Reason, s.Message, s.FinishedAt = phase, reason, trimMessage(message), &now
		if result != nil {
			s.Result = result
		}
	})
}

func trimMessage(m string) string {
	m = strings.Join(strings.Fields(m), " ")
	if len(m) > 300 {
		m = strings.ToValidUTF8(m[:300], "") + "..."
	}
	return m
}
