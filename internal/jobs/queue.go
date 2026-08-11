// Package jobs is a PostgreSQL-backed work queue.
//
// Claims use SELECT … FOR UPDATE SKIP LOCKED, the canonical pattern for letting
// many workers drain one queue without contending on the same row. LISTEN/NOTIFY
// wakes idle workers immediately, so latency does not depend on a poll interval.
//
// No Redis and no separate broker: at this scale a table is the simpler and
// more durable choice, and enqueueing in the same transaction as the work that
// caused it is a property an external broker cannot offer.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jonathanng/echo/internal/db/dbgen"
)

// Channel is the LISTEN/NOTIFY channel used to wake idle workers.
const Channel = "echo_jobs"

// Job types.
const (
	TypeScanRoot  = "scan_root"
	TypeScanFile  = "scan_file"
	TypeCacheEvic = "cache_evict"
)

// Handler processes one job. Returning an error retries it with backoff until
// max_attempts is reached.
type Handler func(ctx context.Context, job dbgen.Job) error

// Queue enqueues work and runs the worker pool.
type Queue struct {
	pool     *pgxpool.Pool
	q        *dbgen.Queries
	log      *slog.Logger
	handlers map[string]Handler

	// pollInterval is the fallback when no NOTIFY arrives. NOTIFY is the
	// primary wake signal; this only has to pick up jobs whose run_after
	// passed while the worker was idle — a retry backoff expiring, or a
	// watcher debounce landing. A couple of seconds keeps that responsive at
	// the cost of one indexed query per worker per interval.
	pollInterval time.Duration
	// staleAfter requeues jobs left 'running' by a process that died.
	staleAfter time.Duration
}

func New(pool *pgxpool.Pool, log *slog.Logger) *Queue {
	return &Queue{
		pool:         pool,
		q:            dbgen.New(pool),
		log:          log,
		handlers:     map[string]Handler{},
		pollInterval: 2 * time.Second,
		staleAfter:   15 * time.Minute,
	}
}

// Register attaches a handler to a job type. Must be called before Run.
func (qu *Queue) Register(jobType string, h Handler) { qu.handlers[jobType] = h }

// EnqueueOpts tunes a single enqueue.
type EnqueueOpts struct {
	// DedupeKey collapses repeat enqueues of the same logical work. An
	// existing queued job with this key is pushed later rather than duplicated.
	DedupeKey string
	// Priority is claimed in descending order.
	Priority int32
	// Delay defers the job.
	Delay time.Duration
	// MaxAttempts defaults to 3.
	MaxAttempts int32
}

// Enqueue adds a job and wakes a worker.
func (qu *Queue) Enqueue(ctx context.Context, jobType string, payload any, opts EnqueueOpts) (dbgen.Job, error) {
	encoded := []byte("{}")
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return dbgen.Job{}, fmt.Errorf("marshal payload: %w", err)
		}
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}

	params := dbgen.EnqueueJobParams{
		Type:        jobType,
		Payload:     encoded,
		Priority:    opts.Priority,
		Delay:       interval(opts.Delay),
		MaxAttempts: opts.MaxAttempts,
	}
	if opts.DedupeKey != "" {
		params.DedupeKey = &opts.DedupeKey
	}

	job, err := qu.q.EnqueueJob(ctx, params)
	if err != nil {
		return dbgen.Job{}, fmt.Errorf("enqueue %s: %w", jobType, err)
	}

	// Best-effort wake. A missed notification only means the job waits for the
	// next poll, so a failure here is not worth failing the enqueue over.
	if _, err := qu.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, jobType); err != nil {
		qu.log.Warn("notify failed", "error", err)
	}
	return job, nil
}

// Run starts n workers plus a listener, and blocks until ctx is cancelled.
func (qu *Queue) Run(ctx context.Context, workers int) {
	if workers <= 0 {
		workers = 1
	}

	// Jobs stranded in 'running' by a previous crash would otherwise never be
	// revisited, because nothing else looks at an already-claimed row.
	if n, err := qu.q.RequeueStaleJobs(ctx, interval(qu.staleAfter)); err != nil {
		qu.log.Error("requeue stale jobs failed", "error", err)
	} else if n > 0 {
		qu.log.Warn("requeued jobs stranded by a previous run", "count", n)
	}

	wake := make(chan struct{}, 1)
	go qu.listen(ctx, wake)

	for i := range workers {
		go qu.worker(ctx, i, wake)
	}
	qu.log.Info("job workers started", "workers", workers)
	<-ctx.Done()
}

// listen forwards NOTIFY events to the workers on a dedicated connection.
func (qu *Queue) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := qu.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			qu.log.Warn("job listener dropped, reconnecting", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (qu *Queue) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := qu.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		signal(wake)
	}
}

// signal is a non-blocking send: the channel has capacity 1 and carries "there
// may be work", so a pending wake already conveys everything a second would.
func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (qu *Queue) worker(ctx context.Context, id int, wake <-chan struct{}) {
	ticker := time.NewTicker(qu.pollInterval)
	defer ticker.Stop()

	for {
		// Drain until the queue is empty before sleeping, so one NOTIFY does
		// not leave a backlog waiting on the poll interval.
		for {
			claimed, err := qu.step(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				qu.log.Error("job step failed", "worker", id, "error", err)
				break
			}
			if !claimed {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

// step claims and runs at most one job, reporting whether one was claimed.
func (qu *Queue) step(ctx context.Context) (bool, error) {
	job, err := qu.q.ClaimJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}

	handler, ok := qu.handlers[job.Type]
	if !ok {
		// An unregistered type is a deployment mistake, not a transient fault.
		// Fail it outright rather than retry until the attempt ceiling.
		qu.log.Error("no handler registered for job type", "type", job.Type, "job", job.ID)
		qu.failPermanently(ctx, job.ID, "no handler registered for type "+job.Type)
		return true, nil
	}

	start := time.Now()
	err = qu.runHandler(ctx, handler, job)
	if err != nil {
		// A cancelled context means shutdown, not a bad job. Leave it running
		// so the stale sweep requeues it rather than burning an attempt.
		if ctx.Err() != nil {
			return true, nil
		}
		qu.log.Error("job failed", "type", job.Type, "job", job.ID,
			"attempt", job.Attempts, "error", err)
		qu.retryOrFail(ctx, job, err)
		return true, nil
	}

	if err := qu.q.CompleteJob(ctx, job.ID); err != nil {
		qu.log.Error("mark job done failed", "job", job.ID, "error", err)
	}
	qu.log.Debug("job done", "type", job.Type, "job", job.ID, "duration", time.Since(start))
	return true, nil
}

// runHandler converts a panic into an error so one bad job cannot take the
// worker — and with it every other job — down.
func (qu *Queue) runHandler(ctx context.Context, h Handler, job dbgen.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, job)
}

func (qu *Queue) retryOrFail(ctx context.Context, job dbgen.Job, cause error) {
	// Exponential backoff, capped: 2s, 4s, 8s … up to 5 minutes.
	backoff := min(time.Duration(1<<job.Attempts)*time.Second, 5*time.Minute)

	updated, err := qu.q.RetryOrFailJob(ctx, dbgen.RetryOrFailJobParams{
		ID:      job.ID,
		Error:   cause.Error(),
		Backoff: interval(backoff),
	})
	if err != nil {
		qu.log.Error("record job failure", "job", job.ID, "error", err)
		return
	}
	if updated.State == dbgen.JobStateFailed {
		qu.log.Error("job exhausted retries", "type", job.Type, "job", job.ID,
			"attempts", updated.Attempts, "error", cause)
	}
}

func (qu *Queue) failPermanently(ctx context.Context, id uuid.UUID, reason string) {
	if _, err := qu.pool.Exec(ctx,
		`UPDATE jobs SET state = 'failed', error = $2, finished_at = now(),
		 dedupe_key = NULL WHERE id = $1`, id, reason); err != nil {
		qu.log.Error("mark job failed", "job", id, "error", err)
	}
}

// interval converts a Go duration to the Postgres interval the queries take.
func interval(d time.Duration) pgtype.Interval {
	if d < 0 {
		d = 0
	}
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
