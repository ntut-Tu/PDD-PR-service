package reconcile

import (
	"context"
	"fmt"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/report"
	"pdd/services/internal/sandboxcontroller/runtime"
)

// logTailFactor is how much more log than output.maxBytes is read, so that condensing can still
// pick error lines out of it.
const logTailFactor = 4

func (c *Controller) logs(ctx context.Context, ct runtime.Container) string {
	out, err := c.Runtime.Logs(ctx, ct.ID, c.set.OutputMaxBytes*logTailFactor)
	if err != nil {
		c.Log.Warn("read container logs", "container", ct.ID, "run", ct.RunID, "error", err)
		return ""
	}
	return out
}

// collect turns the exited container of a run into its final status, then removes the container.
func (c *Controller) collect(ctx context.Context, run model.Run, ct runtime.Container) {
	suites := report.Collect(c.Workspaces.SrcDir(run.ID))
	outcome := report.Evaluate(run.Spec.TestClasses, suites, ct.ExitCode, c.logs(ctx, ct), c.set.OutputMaxBytes)
	if ct.OOMKilled && outcome.Phase != model.Succeeded {
		outcome.Reason = model.TestsFailed
		outcome.Message = fmt.Sprintf("container ran out of memory (limit %d MiB)", c.set.MemoryBytes>>20)
	}
	c.Log.Info("run finished", "run", run.ID, "phase", outcome.Phase, "reason", outcome.Reason, "message", outcome.Message)
	now := c.Now().UTC()
	stored := c.update(ctx, &run, func(s *model.Status) {
		s.Phase, s.Reason, s.Message, s.FinishedAt = outcome.Phase, outcome.Reason, outcome.Message, &now
		s.Result = &outcome.Result
		s.SetCondition("Completed", outcome.Phase == model.Succeeded, now, outcome.Message)
	})
	if stored {
		c.removeContainer(ctx, ct)
	}
}

// timeout stops a run that is past limits.timeout and keeps what it produced so far.
func (c *Controller) timeout(ctx context.Context, run model.Run, ct runtime.Container) {
	if err := c.Runtime.Stop(ctx, ct.ID, stopGrace); err != nil {
		c.Log.Warn("stop container", "container", ct.ID, "run", run.ID, "error", err)
	}
	suites := report.Collect(c.Workspaces.SrcDir(run.ID))
	result := report.Partial(run.Spec.TestClasses, suites, c.logs(ctx, ct), c.set.OutputMaxBytes)
	c.fail(ctx, &run, model.Failed, model.Timeout, fmt.Sprintf("run exceeded the time limit of %s", c.set.Timeout), &result)
	c.removeContainer(ctx, ct)
}

func (c *Controller) removeContainer(ctx context.Context, ct runtime.Container) {
	if err := c.Runtime.Remove(ctx, ct.ID); err != nil {
		// The next pass finds it as an orphan and retries.
		c.Log.Warn("remove container", "container", ct.ID, "run", ct.RunID, "error", err)
	}
}
