package agentd

import (
	"context"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/web"
)

type Executor interface {
	Status(ctx context.Context) (commands.StatusReport, error)
	Diff(ctx context.Context, params web.DiffParams) (string, error)
	Push(ctx context.Context, confirm bool) (commands.PushReport, error)
	Pull(ctx context.Context, confirm bool) (commands.PullReport, error)
}

type localExecutor struct {
	homerHome string
}

// NewLocalExecutor returns the production task executor. The commands package
// remains the sole implementation of Homer status/diff/push/pull semantics;
// this adapter only supplies the daemon's workspace and frozen options.
func NewLocalExecutor(homerHome string) Executor {
	return &localExecutor{homerHome: homerHome}
}

func (e *localExecutor) Status(ctx context.Context) (commands.StatusReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	report, err := commands.RunStatus(commands.StatusOptions{HomerHome: e.homerHome})
	if err != nil {
		return commands.StatusReport{}, err
	}
	if err := contextError(ctx); err != nil {
		return commands.StatusReport{}, err
	}
	return report, nil
}

func (e *localExecutor) Diff(ctx context.Context, params web.DiffParams) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	text, err := commands.RunDiff(commands.DiffOptions{
		HomerHome: e.homerHome,
		Adapter:   params.Adapter,
		Category:  params.Category,
	})
	if err != nil {
		return "", err
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	return text, nil
}

func (e *localExecutor) Push(ctx context.Context, confirm bool) (commands.PushReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	report := commands.RunPush(commands.PushOptions{HomerHome: e.homerHome, Yes: confirm}, &commands.PushDeps{UI: commands.HeadlessUI{}})
	if err := contextError(ctx); err != nil {
		return commands.PushReport{}, err
	}
	return report, nil
}

func (e *localExecutor) Pull(ctx context.Context, confirm bool) (commands.PullReport, error) {
	if err := contextError(ctx); err != nil {
		return commands.PullReport{}, err
	}
	report := commands.RunPull(commands.PullOptions{HomerHome: e.homerHome, Yes: confirm}, &commands.PullDeps{UI: commands.HeadlessUI{}})
	if err := contextError(ctx); err != nil {
		return commands.PullReport{}, err
	}
	return report, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

var _ Executor = (*localExecutor)(nil)
