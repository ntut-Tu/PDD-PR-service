// Package runtimetest provides an in-memory container runtime for tests.
package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"pdd/services/internal/sandboxcontroller/runtime"
)

// Fake is a Runtime that runs nothing. Tests (or the OnStart hook) decide how a container ends
// by calling Finish.
type Fake struct {
	mu         sync.Mutex
	containers map[string]*entry
	seq        int

	// Set before use. Zero values mean: runsc registered, no errors.
	NoRunsc     bool
	NetworkInfo runtime.NetworkInfo
	CheckErr    error
	NetworkErr  error
	CreateErr   error
	StartErr    error
	ListErr     error
	InspectErr  error
	RemoveErr   error
	// Now stamps start and finish times; defaults to time.Now.
	Now func() time.Time
	// OnStart runs inside Start, after the container is marked running. It may call Finish.
	OnStart func(spec runtime.ContainerSpec, id string)

	Created []runtime.ContainerSpec
	Started []string
	Stopped []string
	Removed []string
	Checked []string
}

type entry struct {
	spec runtime.ContainerSpec
	info runtime.Container
	logs string
}

// New returns an empty fake.
func New() *Fake { return &Fake{containers: map[string]*entry{}} }

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// CheckRuntime implements runtime.Runtime.
func (f *Fake) CheckRuntime(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Checked = append(f.Checked, name)
	if f.CheckErr != nil {
		return f.CheckErr
	}
	if f.NoRunsc {
		return fmt.Errorf("%w: %s is not registered", runtime.ErrUnavailable, name)
	}
	return nil
}

// EnsureNetwork implements runtime.Runtime.
func (f *Fake) EnsureNetwork(context.Context, string) (runtime.NetworkInfo, error) {
	if f.NetworkErr != nil {
		return runtime.NetworkInfo{}, f.NetworkErr
	}
	if f.NetworkInfo == (runtime.NetworkInfo{}) {
		return runtime.NetworkInfo{Gateway: "172.30.0.1"}, nil
	}
	return f.NetworkInfo, nil
}

// Create implements runtime.Runtime.
func (f *Fake) Create(_ context.Context, spec runtime.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return "", f.CreateErr
	}
	for id, e := range f.containers {
		if e.spec.Name == spec.Name {
			return id, nil
		}
	}
	f.seq++
	id := fmt.Sprintf("c%04d", f.seq)
	f.containers[id] = &entry{spec: spec, info: runtime.Container{
		ID: id, Name: spec.Name, RunID: spec.Labels[runtime.LabelRun], State: runtime.StateCreated}}
	f.Created = append(f.Created, spec)
	return id, nil
}

// Start implements runtime.Runtime.
func (f *Fake) Start(_ context.Context, id string) error {
	f.mu.Lock()
	e, ok := f.containers[id]
	switch {
	case f.StartErr != nil:
		f.mu.Unlock()
		return f.StartErr
	case !ok:
		f.mu.Unlock()
		return runtime.ErrNotFound
	}
	e.info.State = runtime.StateRunning
	e.info.StartedAt = f.now()
	f.Started = append(f.Started, id)
	hook, spec := f.OnStart, e.spec
	f.mu.Unlock()
	if hook != nil {
		hook(spec, id)
	}
	return nil
}

// Finish makes a running container exit with the given code and log output.
func (f *Fake) Finish(id string, exitCode int, logs string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.containers[id]; ok {
		e.info.State = runtime.StateExited
		e.info.ExitCode = exitCode
		e.info.FinishedAt = f.now()
		e.logs = logs
	}
}

// Add inserts a container as if it had been created by an earlier controller process.
func (f *Fake) Add(runID string, state runtime.State) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("c%04d", f.seq)
	spec := runtime.ContainerSpec{Name: "pdd-sandbox-" + runID, Labels: map[string]string{runtime.LabelRun: runID}}
	f.containers[id] = &entry{spec: spec, info: runtime.Container{ID: id, Name: spec.Name, RunID: runID, State: state, StartedAt: f.now()}}
	return id
}

// Inspect implements runtime.Runtime.
func (f *Fake) Inspect(_ context.Context, id string) (runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InspectErr != nil {
		return runtime.Container{}, f.InspectErr
	}
	e, ok := f.containers[id]
	if !ok {
		return runtime.Container{}, runtime.ErrNotFound
	}
	return e.info, nil
}

// Logs implements runtime.Runtime.
func (f *Fake) Logs(_ context.Context, id string, tailBytes int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.containers[id]
	if !ok {
		return "", runtime.ErrNotFound
	}
	if len(e.logs) > tailBytes {
		return e.logs[len(e.logs)-tailBytes:], nil
	}
	return e.logs, nil
}

// Stop implements runtime.Runtime; a stopped container exits with code 137.
func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Stopped = append(f.Stopped, id)
	if e, ok := f.containers[id]; ok && e.info.State == runtime.StateRunning {
		e.info.State = runtime.StateExited
		e.info.ExitCode = 137
		e.info.FinishedAt = f.now()
	}
	return nil
}

// Remove implements runtime.Runtime.
func (f *Fake) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	f.Removed = append(f.Removed, id)
	delete(f.containers, id)
	return nil
}

// List implements runtime.Runtime.
func (f *Fake) List(context.Context) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]runtime.Container, 0, len(f.containers))
	for _, e := range f.containers {
		out = append(out, e.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ForRun returns the containers of a run.
func (f *Fake) ForRun(runID string) []runtime.Container {
	list, _ := f.List(context.Background())
	var out []runtime.Container
	for _, c := range list {
		if c.RunID == runID {
			out = append(out, c)
		}
	}
	return out
}

// Count is the number of containers.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

// ErrBoom is an error for fault injection.
var ErrBoom = errors.New("boom")

// SetOOMKilled marks a container as killed by the out-of-memory killer.
func (f *Fake) SetOOMKilled(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.containers[id]; ok {
		e.info.OOMKilled = true
	}
}
