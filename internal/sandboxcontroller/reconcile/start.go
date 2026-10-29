package reconcile

import (
	"context"
	"errors"
	"fmt"

	"pdd/services/internal/sandboxcontroller/build"
	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/source"
	"pdd/services/internal/sandboxcontroller/workspace"
)

// begin moves a Pending run to Preparing and prepares it in the background, so that downloading
// a large repository never delays the timeout checks of the other runs.
func (c *Controller) begin(ctx context.Context, run model.Run) {
	if !c.update(ctx, &run, func(s *model.Status) {
		s.Phase, s.Message = model.Preparing, "preparing the workspace"
	}) {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.inflight[run.ID] = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			cancel()
			c.mu.Lock()
			delete(c.inflight, run.ID)
			c.mu.Unlock()
			c.Trigger()
		}()
		c.prepare(runCtx, run)
	}()
}

// prepare fetches the source, builds the workspace and starts the container.
func (c *Controller) prepare(ctx context.Context, run model.Run) {
	spec := run.Spec
	if err := c.Runtime.CheckRuntime(ctx, c.set.OCIRuntime); err != nil {
		c.fail(ctx, &run, model.Error, model.SandboxUnavailable, "sandbox unavailable: "+err.Error(), nil)
		return
	}
	if err := c.fetch(ctx, run); err != nil {
		c.failPrepare(ctx, &run, err)
		return
	}
	tool, wrapper, err := build.Detect(c.Workspaces.SrcDir(run.ID))
	if err != nil {
		c.fail(ctx, &run, model.Error, model.UnsupportedProject, "not supported: only Maven and Gradle projects are run, but neither pom.xml nor build.gradle(.kts) was found at the repository root", nil)
		return
	}
	if err := c.Workspaces.WriteFiles(run.ID, spec.Files); err != nil {
		c.fail(ctx, &run, model.Error, model.Internal, "write test files: "+err.Error(), nil)
		return
	}
	network, err := c.Runtime.EnsureNetwork(ctx, c.set.NetworkName)
	if err != nil {
		c.failPrepare(ctx, &run, err)
		return
	}
	proxyHost := c.set.ProxyHost
	if proxyHost == "" {
		proxyHost = network.Gateway
	}
	proxy := build.Proxy{Host: proxyHost, Port: c.set.ProxyPort}
	if err := build.WriteInit(c.Workspaces.InitDir(run.ID), proxy); err != nil {
		c.fail(ctx, &run, model.Error, model.Internal, err.Error(), nil)
		return
	}
	cache, err := workspace.CacheDir(c.set.CacheDir, spec.ProjectID, spec.RepoID)
	if err != nil {
		c.fail(ctx, &run, model.Error, model.Internal, err.Error(), nil)
		return
	}
	c.update(ctx, &run, func(s *model.Status) {
		s.BuildTool = tool
		s.SetCondition("Prepared", true, c.Now(), "source fetched and tests written")
	})

	plan := build.NewPlan(tool, wrapper, spec.TestClasses, proxy)
	id, err := c.Runtime.Create(ctx, c.containerSpec(run.ID, plan, cache))
	if err == nil {
		err = c.Runtime.Start(ctx, id)
		if err != nil {
			_ = c.Runtime.Remove(context.WithoutCancel(ctx), id)
		}
	}
	if err != nil {
		c.failPrepare(ctx, &run, fmt.Errorf("start container: %w", err))
		return
	}
	started := c.Now().UTC()
	c.update(ctx, &run, func(s *model.Status) {
		s.Phase, s.Message, s.StartedAt = model.Running, "tests are running", &started
		s.SetCondition("Started", true, started, "container "+id)
	})
	c.Log.Info("run started", "run", run.ID, "buildTool", tool, "container", id)
}

// fetch downloads the archive and unpacks it into a fresh workspace.
func (c *Controller) fetch(ctx context.Context, run model.Run) error {
	body, err := c.Source.Archive(ctx, run.Spec)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	if err := c.Workspaces.Init(run.ID); err != nil {
		return err
	}
	return c.Workspaces.Extract(run.ID, body)
}

// failPrepare maps a preparation error to the terminal status of the run.
func (c *Controller) failPrepare(ctx context.Context, run *model.Run, err error) {
	switch {
	case ctx.Err() != nil:
		c.Log.Info("preparation cancelled", "run", run.ID)
	case errors.Is(err, source.ErrUnavailable), errors.Is(err, workspace.ErrArchive):
		c.fail(ctx, run, model.Error, model.SourceUnavailable, "source unavailable: "+err.Error(), nil)
	case errors.Is(err, runtime.ErrUnavailable):
		c.fail(ctx, run, model.Error, model.SandboxUnavailable, "sandbox unavailable: "+err.Error(), nil)
	default:
		c.fail(ctx, run, model.Error, model.Internal, err.Error(), nil)
	}
}

func (c *Controller) containerSpec(runID string, plan build.Plan, cacheDir string) runtime.ContainerSpec {
	image := c.set.ImageMaven
	if plan.Tool == model.Gradle {
		image = c.set.ImageGradle
	}
	return runtime.ContainerSpec{
		Name:       "pdd-sandbox-" + runID,
		Image:      image,
		Labels:     map[string]string{runtime.LabelRun: runID},
		Entrypoint: plan.Entrypoint,
		Cmd:        plan.Cmd,
		Env:        plan.Env,
		WorkingDir: plan.WorkingDir,
		User:       c.set.User,
		Runtime:    c.set.OCIRuntime,
		Network:    c.set.NetworkName,
		Mounts: []runtime.Mount{
			{Source: c.Workspaces.Dir(runID), Target: build.WorkspaceMount},
			{Source: cacheDir, Target: build.CacheMount},
		},
		Tmpfs:          map[string]string{"/tmp": "rw,exec,nosuid,size=512m"},
		CPUs:           c.set.CPUs,
		MemoryBytes:    c.set.MemoryBytes,
		PidsLimit:      c.set.Pids,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
	}
}
