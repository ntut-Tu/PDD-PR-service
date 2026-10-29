package containerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	ctrd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"

	"pdd/services/internal/sandboxcontroller/runtime"
)

const (
	labelNetns   = "pdd.sandbox/netns"
	labelNetwork = "pdd.sandbox/network"
	labelStarted = "pdd.sandbox/started"

	cgroupRoot = "/sys/fs/cgroup"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (c *Client) logPath(id string) string   { return filepath.Join(c.cfg.LogDir, id+".log") }
func (c *Client) hostsPath(id string) string { return filepath.Join(c.cfg.LogDir, id+".hosts") }

// load returns the container or runtime.ErrNotFound.
func (c *Client) load(ctx context.Context, id string) (ctrd.Container, error) {
	if !idPattern.MatchString(id) {
		return nil, runtime.ErrNotFound
	}
	ct, err := c.client.LoadContainer(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil, runtime.ErrNotFound
	}
	return ct, wrap("load container "+id, err)
}

// task returns the task of a container, or nil when it has none.
func task(ctx context.Context, ct ctrd.Container) (ctrd.Task, error) {
	t, err := ct.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil, nil
	}
	return t, wrap("load task of "+ct.ID(), err)
}

// image returns the image in the content store, pulling it first when it is missing. Concurrent
// requests for the same image share one pull.
func (c *Client) image(ctx context.Context, name string) (ctrd.Image, error) {
	ref, err := reference.ParseDockerRef(name)
	if err != nil {
		return nil, fmt.Errorf("containerd: image %q: %w", name, err)
	}
	img, err := c.client.GetImage(ctx, ref.String())
	if errdefs.IsNotFound(err) {
		_, err, _ = c.pulls.Do(ref.String(), func() (any, error) {
			pullCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pullTimeout)
			defer cancel()
			_, err := c.client.Pull(pullCtx, ref.String(), ctrd.WithPullUnpack, ctrd.WithPullSnapshotter(snapshotter))
			return nil, err
		})
		if err != nil {
			return nil, wrap("pull "+ref.String(), err)
		}
		img, err = c.client.GetImage(ctx, ref.String())
	}
	if err != nil {
		return nil, wrap("get image "+ref.String(), err)
	}
	if unpacked, err := img.IsUnpacked(ctx, snapshotter); err != nil {
		return nil, wrap("check image "+ref.String(), err)
	} else if !unpacked {
		if err := img.Unpack(ctx, snapshotter); err != nil {
			return nil, wrap("unpack image "+ref.String(), err)
		}
	}
	return img, nil
}

// Create implements runtime.Runtime. A missing image is pulled once. The network namespace is
// set up before the container exists and released again when creating the container fails.
func (c *Client) Create(ctx context.Context, spec runtime.ContainerSpec) (string, error) {
	id := spec.Name
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("containerd: invalid container name %q", id)
	}
	if _, err := specOpts(spec, nil, sandboxFiles{}); err != nil {
		return "", err
	}
	rt, err := c.ociRuntime(spec.Runtime)
	if err != nil {
		return "", err
	}
	ctx = c.ctx(ctx)
	defer c.locks.lock(id)()

	if _, err := c.load(ctx, id); err == nil {
		return id, nil
	} else if !errors.Is(err, runtime.ErrNotFound) {
		return "", err
	}
	img, err := c.image(ctx, spec.Image)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := os.MkdirAll(c.cfg.LogDir, 0o755); err != nil {
		return "", fmt.Errorf("containerd: %w", err)
	}
	att, err := c.attach(ctx, spec.Network, id)
	if err != nil {
		return "", err
	}
	if err := c.create(ctx, id, spec, img, rt, att); err != nil {
		cleanup := context.WithoutCancel(ctx)
		if derr := c.detach(cleanup, spec.Network, id, att.netns); derr != nil {
			err = errors.Join(err, derr)
		}
		_ = os.Remove(c.hostsPath(id))
		return "", err
	}
	return id, nil
}

func (c *Client) create(ctx context.Context, id string, spec runtime.ContainerSpec, img ctrd.Image, rt ociRuntime, att attachment) error {
	files := sandboxFiles{Netns: att.netns, Hosts: c.hostsPath(id)}
	if err := os.WriteFile(files.Hosts, hostsFile(hostname(id), att.ip), 0o644); err != nil {
		return fmt.Errorf("containerd: %w", err)
	}
	opts, err := specOpts(spec, img, files)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[labelNetns], labels[labelNetwork] = att.netns, spec.Network
	_, err = c.client.NewContainer(ctx, id,
		ctrd.WithContainerLabels(labels),
		ctrd.WithImage(img),
		ctrd.WithSnapshotter(snapshotter),
		ctrd.WithNewSnapshot(id, img),
		ctrd.WithRuntime(rt.Type, rt.Options),
		ctrd.WithNewSpec(opts...),
	)
	return wrap("create container "+id, err)
}

// Start implements runtime.Runtime. The output of the container goes to its log file.
func (c *Client) Start(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout)
	defer cancel()
	defer c.locks.lock(id)()

	ct, err := c.load(ctx, id)
	if err != nil {
		return err
	}
	t, err := task(ctx, ct)
	if err != nil {
		return err
	}
	if t != nil {
		if status, err := t.Status(ctx); err == nil && status.Status == ctrd.Running {
			return nil
		}
		// A task that ran before is replaced; its exit status is lost, which only happens
		// when a container is started twice.
		if err := deleteTask(ctx, t); err != nil {
			return wrap("delete task of "+id, err)
		}
	}
	t, err = ct.NewTask(ctx, cio.LogFile(c.logPath(id)))
	if err != nil {
		return wrap("create task of "+id, err)
	}
	if err := t.Start(ctx); err != nil {
		_ = deleteTask(context.WithoutCancel(ctx), t)
		return wrap("start "+id, err)
	}
	// Best effort: without the label StartedAt is zero and the reconcile loop uses its own clock.
	_, _ = ct.SetLabels(ctx, map[string]string{labelStarted: time.Now().UTC().Format(time.RFC3339Nano)})
	return nil
}

// Inspect implements runtime.Runtime. A container without a task is Created, which is also what
// remains of a started container after the runtime container restarted (tasks do not survive
// it). The exit code of a stopped task is its exit status. OOMKilled is true when the kernel
// counted an out-of-memory kill in the cgroup of the container (memory.events on cgroup v2,
// memory.oom_control on v1); the cgroup and its counter stay until the task is deleted. Under
// gVisor the limit applies to the sandbox processes, so a container that exceeds it is killed by
// the kernel and counted the same way.
func (c *Client) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout)
	defer cancel()
	ct, err := c.load(ctx, id)
	if err != nil {
		return runtime.Container{}, err
	}
	return c.inspect(ctx, ct)
}

func (c *Client) inspect(ctx context.Context, ct ctrd.Container) (runtime.Container, error) {
	info, err := ct.Info(ctx, ctrd.WithoutRefreshedMetadata)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return runtime.Container{}, runtime.ErrNotFound
		}
		return runtime.Container{}, wrap("inspect "+ct.ID(), err)
	}
	out := runtime.Container{
		ID: info.ID, Name: info.ID, RunID: info.Labels[runtime.LabelRun], State: runtime.StateCreated,
		StartedAt: parseTime(info.Labels[labelStarted]),
	}
	t, err := task(ctx, ct)
	if err != nil || t == nil {
		return out, err
	}
	status, err := t.Status(ctx)
	if errdefs.IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return runtime.Container{}, wrap("status of "+ct.ID(), err)
	}
	switch status.Status {
	case ctrd.Created:
	case ctrd.Stopped:
		out.State, out.ExitCode, out.FinishedAt = runtime.StateExited, int(status.ExitStatus), status.ExitTime
		out.OOMKilled = c.oomKilled(ctx, ct)
	default:
		out.State = runtime.StateRunning
	}
	return out, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// oomKilled reads the out-of-memory kill counter of the container's cgroup.
func (c *Client) oomKilled(ctx context.Context, ct ctrd.Container) bool {
	spec, err := ct.Spec(ctx)
	if err != nil || spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return false
	}
	// cgroup v2 first, then the memory controller of cgroup v1.
	for _, file := range []string{
		filepath.Join(cgroupRoot, spec.Linux.CgroupsPath, "memory.events"),
		filepath.Join(cgroupRoot, "memory", spec.Linux.CgroupsPath, "memory.oom_control"),
	} {
		if data, err := os.ReadFile(file); err == nil {
			return oomKillCount(string(data)) > 0
		}
	}
	return false
}

// oomKillCount returns the oom_kill counter of a memory.events (v2) or memory.oom_control (v1) file.
func oomKillCount(events string) int {
	for _, line := range strings.Split(events, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), " "); ok && key == "oom_kill" {
			n, _ := strconv.Atoi(value)
			return n
		}
	}
	return 0
}

// Logs implements runtime.Runtime.
func (c *Client) Logs(ctx context.Context, id string, tailBytes int) (string, error) {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout)
	defer cancel()
	out, err := tailFile(c.logPath(id), tailBytes)
	if errors.Is(err, os.ErrNotExist) {
		// No output yet, or no container.
		if _, err := c.load(ctx, id); err != nil {
			return "", err
		}
		return "", nil
	}
	return out, err
}

// tailFile returns at most the last n bytes of the file.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", fmt.Errorf("containerd: read log: %w", err)
	}
	offset := max(size-int64(n), 0)
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("containerd: read log: %w", err)
	}
	return string(buf), nil
}

// Stop implements runtime.Runtime: SIGTERM, then SIGKILL for the whole container after grace.
func (c *Client) Stop(ctx context.Context, id string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout+grace)
	defer cancel()
	defer c.locks.lock(id)()

	ct, err := c.load(ctx, id)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	t, err := task(ctx, ct)
	if err != nil || t == nil {
		return err
	}
	exited, err := t.Wait(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return wrap("wait for "+id, err)
	}
	if status, err := t.Status(ctx); err == nil && status.Status == ctrd.Stopped {
		return nil
	}
	if err := kill(ctx, t, syscall.SIGTERM); err != nil {
		return wrap("stop "+id, err)
	}
	select {
	case <-exited:
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := kill(ctx, t, syscall.SIGKILL, ctrd.WithKillAll); err != nil {
		return wrap("kill "+id, err)
	}
	select {
	case <-exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("containerd: %s did not stop: %w", id, ctx.Err())
	}
}

// kill signals the task and treats a task that is already gone or stopped as success. runsc
// reports "sandbox is not running" for a stopped task, which containerd cannot tell from a
// failure, so the status decides.
func kill(ctx context.Context, t ctrd.Task, sig syscall.Signal, opts ...ctrd.KillOpts) error {
	err := t.Kill(ctx, sig, opts...)
	if err == nil || errdefs.IsNotFound(err) || errdefs.IsFailedPrecondition(err) {
		return nil
	}
	if status, serr := t.Status(ctx); serr == nil && status.Status == ctrd.Stopped {
		return nil
	}
	return err
}

// deleteTask kills the task if it still runs and deletes it. Unlike containerd's
// WithProcessKill it does not signal a stopped task, which runsc answers with an error.
func deleteTask(ctx context.Context, t ctrd.Task) error {
	status, err := t.Status(ctx)
	if errdefs.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if status.Status != ctrd.Stopped {
		exited, err := t.Wait(ctx)
		if err != nil {
			return err
		}
		if err := kill(ctx, t, syscall.SIGKILL, ctrd.WithKillAll); err != nil {
			return err
		}
		select {
		case <-exited:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, err := t.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// Remove implements runtime.Runtime. It kills the container if needed and releases its task,
// address, network namespace, snapshot, log and hosts file; the container itself goes last, so
// an interrupted removal is repeated by the next call.
func (c *Client) Remove(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout)
	defer cancel()
	defer c.locks.lock(id)()

	ct, err := c.load(ctx, id)
	if errors.Is(err, runtime.ErrNotFound) {
		return c.removeFiles(id)
	} else if err != nil {
		return err
	}
	t, err := task(ctx, ct)
	if err != nil {
		return err
	}
	if t != nil {
		if err := deleteTask(ctx, t); err != nil {
			return wrap("delete task of "+id, err)
		}
	}
	labels, err := ct.Labels(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return wrap("labels of "+id, err)
	}
	if err := c.detach(ctx, labels[labelNetwork], id, labels[labelNetns]); err != nil {
		return fmt.Errorf("containerd: release network of %s: %w", id, err)
	}
	if err := ct.Delete(ctx, ctrd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return wrap("delete container "+id, err)
	}
	return c.removeFiles(id)
}

func (c *Client) removeFiles(id string) error {
	var errs []error
	for _, path := range []string{c.logPath(id), c.hostsPath(id)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// List implements runtime.Runtime.
func (c *Client) List(ctx context.Context) ([]runtime.Container, error) {
	ctx, cancel := context.WithTimeout(c.ctx(ctx), callTimeout)
	defer cancel()
	cts, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", runtime.LabelRun))
	if err != nil {
		return nil, wrap("list containers", err)
	}
	out := make([]runtime.Container, 0, len(cts))
	for _, ct := range cts {
		info, err := c.inspect(ctx, ct)
		if errors.Is(err, runtime.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}
