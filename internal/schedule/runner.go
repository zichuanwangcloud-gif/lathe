package schedule

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 桩：先红。
const (
	ClaimLimit             = 200
	ExecConcurrency        = 4
	LeaseTTL               = 2 * time.Minute
	LeaseHeartbeat         = 20 * time.Second
	FenceMargin            = 45 * time.Second
	MaxConsecutiveFailures = 5
	linearClientTimeout    = 30 * time.Second
)

const ReplayStalePending = false

var errStub = errors.New("schedule: 未实现")

type IssueCreator interface {
	CreateIssue(ctx context.Context, in linear.CreateIssueInput) (*linear.CreatedIssue, error)
	IssueByID(ctx context.Context, id string) (*linear.CreatedIssue, error)
	LabelIDByName(ctx context.Context, teamID, name string) (string, error)
}

type LinearHandle struct {
	Client IssueCreator
	UserID string
	Redact func(string) string
}

type LinearFor func(ctx context.Context, userID int64) (LinearHandle, error)

type Settings interface {
	WebhookTriggerLabel(ctx context.Context) string
}

type leaseRenewer interface {
	RenewLeases(ctx context.Context, tokens []string, ttl time.Duration) (int64, error)
}

type RunRef struct{ RunID, ScheduleID int64 }

type TickResult struct{ Claimed, Recovered []RunRef }

type Runner struct {
	Store         *store.Store
	Linear        LinearFor
	Settings      Settings
	Now           func() time.Time
	Tick          time.Duration
	CatchUpWindow time.Duration
	Node          string

	leaseTTL, heartbeat, fenceMargin time.Duration
	concurrency                      int
	replay                           *bool
	renewer                          leaseRenewer
	hold                             chan struct{}
	log                              *slog.Logger
}

func checkLeaseConstants(ttl, hb, fence, client time.Duration) error { return errStub }

func newUUID() string { return "00000000-0000-4000-8000-000000000000" }

func (r *Runner) TickOnce(ctx context.Context) (TickResult, error) { return TickResult{}, errStub }

func (r *Runner) FireNow(ctx context.Context, scheduleID, userID int64) (*store.ScheduleRunRow, error) {
	return nil, errStub
}

func (r *Runner) Drain(ctx context.Context) error { return nil }

func (r *Runner) Close() {}

func (r *Runner) Run(ctx context.Context) { <-ctx.Done() }
