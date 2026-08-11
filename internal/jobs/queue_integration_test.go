//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonathanng/echo/internal/db/dbgen"
	"github.com/jonathanng/echo/internal/dbtest"
	"github.com/jonathanng/echo/internal/jobs"
)

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

const testType = "test_job"

// waitFor polls until cond holds or the deadline passes. The queue is
// asynchronous, so assertions have to wait for it rather than assume timing.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestJobRunsAndCompletes(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	var ran atomic.Int64
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		ran.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 2)

	job, err := q.Enqueue(context.Background(), testType, map[string]string{"k": "v"}, jobs.EnqueueOpts{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitFor(t, 10*time.Second, "the job to run", func() bool { return ran.Load() == 1 })

	waitFor(t, 5*time.Second, "the job to be marked done", func() bool {
		row, err := dbgen.New(pool).GetJob(context.Background(), job.ID)
		return err == nil && row.State == dbgen.JobStateDone
	})

	// dedupe_key is cleared on completion so the same logical work can be
	// enqueued again later.
	row, _ := dbgen.New(pool).GetJob(context.Background(), job.ID)
	if row.DedupeKey != nil {
		t.Errorf("dedupe_key = %v after completion, want NULL", *row.DedupeKey)
	}
}

// The dedupe key is what stops a watcher firing three events for one file write
// from producing three scans.
func TestDedupeKeyCollapsesEnqueues(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())
	ctx := context.Background()

	for range 5 {
		if _, err := q.Enqueue(ctx, testType, nil, jobs.EnqueueOpts{DedupeKey: "same"}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("job count = %d, want 1; the dedupe key did not collapse enqueues", n)
	}
}

// Different keys must stay distinct, or unrelated work would be swallowed.
func TestDifferentDedupeKeysCoexist(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if _, err := q.Enqueue(ctx, testType, nil, jobs.EnqueueOpts{DedupeKey: key}); err != nil {
			t.Fatalf("enqueue %s: %v", key, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("job count = %d, want 3", n)
	}
}

// The point of SKIP LOCKED: many workers drain one queue, and no job is ever
// handed to two of them.
func TestConcurrentWorkersEachJobRunsOnce(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	const total = 40
	var (
		mu   sync.Mutex
		seen = map[string]int{}
	)
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		mu.Lock()
		seen[job.ID.String()]++
		mu.Unlock()
		// Long enough that workers genuinely overlap.
		time.Sleep(5 * time.Millisecond)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 8)

	for i := range total {
		if _, err := q.Enqueue(context.Background(), testType,
			map[string]int{"i": i}, jobs.EnqueueOpts{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	waitFor(t, 30*time.Second, "all jobs to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == total
	})

	mu.Lock()
	defer mu.Unlock()
	for id, count := range seen {
		if count != 1 {
			t.Errorf("job %s ran %d times, want exactly 1", id, count)
		}
	}
}

func TestFailedJobRetriesThenFails(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	var attempts atomic.Int64
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		attempts.Add(1)
		return errors.New("always fails")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)

	job, err := q.Enqueue(context.Background(), testType, nil, jobs.EnqueueOpts{MaxAttempts: 2})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitFor(t, 30*time.Second, "the job to exhaust its retries", func() bool {
		row, err := dbgen.New(pool).GetJob(context.Background(), job.ID)
		return err == nil && row.State == dbgen.JobStateFailed
	})

	if got := attempts.Load(); got != 2 {
		t.Errorf("handler ran %d times, want 2 (max_attempts)", got)
	}

	row, _ := dbgen.New(pool).GetJob(context.Background(), job.ID)
	if row.Error == nil || *row.Error == "" {
		t.Error("the failure reason was not recorded")
	}
}

// A transient failure must not be terminal: the second attempt should succeed
// and the job should end up done.
func TestJobSucceedsOnRetry(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	var attempts atomic.Int64
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		if attempts.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)

	job, err := q.Enqueue(context.Background(), testType, nil, jobs.EnqueueOpts{MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitFor(t, 30*time.Second, "the retry to succeed", func() bool {
		row, err := dbgen.New(pool).GetJob(context.Background(), job.ID)
		return err == nil && row.State == dbgen.JobStateDone
	})
}

// A panicking handler must not take the worker down with it; the queue has to
// keep draining.
func TestPanicIsContainedAndWorkerSurvives(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	var ran atomic.Int64
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		if ran.Add(1) == 1 {
			panic("boom")
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)

	if _, err := q.Enqueue(context.Background(), testType, nil,
		jobs.EnqueueOpts{MaxAttempts: 1}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// The second job proves the worker is still alive after the panic.
	second, err := q.Enqueue(context.Background(), testType, nil, jobs.EnqueueOpts{DedupeKey: "second"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitFor(t, 30*time.Second, "the worker to keep draining after a panic", func() bool {
		row, err := dbgen.New(pool).GetJob(context.Background(), second.ID)
		return err == nil && row.State == dbgen.JobStateDone
	})
}

// An unregistered type is a deployment mistake, not a transient fault, so it
// should fail immediately rather than burn every retry.
func TestUnknownJobTypeFailsWithoutRetrying(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 1)

	job, err := q.Enqueue(context.Background(), "no_such_type", nil,
		jobs.EnqueueOpts{MaxAttempts: 5})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waitFor(t, 15*time.Second, "the job to fail", func() bool {
		row, err := dbgen.New(pool).GetJob(context.Background(), job.ID)
		return err == nil && row.State == dbgen.JobStateFailed
	})

	row, _ := dbgen.New(pool).GetJob(context.Background(), job.ID)
	if row.Attempts > 1 {
		t.Errorf("attempts = %d; an unknown type should not be retried", row.Attempts)
	}
}

// A delayed job must not run early, or a debounce would be pointless.
func TestDelayedJobWaits(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())

	var ran atomic.Bool
	q.Register(testType, func(ctx context.Context, job dbgen.Job) error {
		ran.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx, 2)

	if _, err := q.Enqueue(context.Background(), testType, nil,
		jobs.EnqueueOpts{Delay: 2 * time.Second}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	if ran.Load() {
		t.Fatal("the job ran before its delay elapsed")
	}
	waitFor(t, 30*time.Second, "the delayed job to run", func() bool { return ran.Load() })
}

// Higher priority is claimed first. Enqueued while no workers are running so
// the ordering is observable rather than a race.
func TestPriorityOrdersClaims(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())
	ctx := context.Background()

	low, err := q.Enqueue(ctx, testType, nil, jobs.EnqueueOpts{DedupeKey: "low", Priority: 1})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	high, err := q.Enqueue(ctx, testType, nil, jobs.EnqueueOpts{DedupeKey: "high", Priority: 100})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	first, err := dbgen.New(pool).ClaimJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if first.ID != high.ID {
		t.Errorf("claimed %s first, want the higher-priority job %s (low was %s)",
			first.ID, high.ID, low.ID)
	}
}

// Without a stale sweep, a crash mid-job strands the work forever: nothing else
// revisits a row that was already claimed.
func TestStaleRunningJobsAreRequeued(t *testing.T) {
	pool := dbtest.New(t)
	q := jobs.New(pool, dbtest.DiscardLogger())
	ctx := context.Background()

	job, err := q.Enqueue(ctx, testType, nil, jobs.EnqueueOpts{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Model a worker that claimed the job and then died.
	if _, err := pool.Exec(ctx,
		`UPDATE jobs SET state = 'running', started_at = now() - interval '1 hour' WHERE id = $1`,
		job.ID); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	var ran atomic.Bool
	q.Register(testType, func(ctx context.Context, j dbgen.Job) error {
		ran.Store(true)
		return nil
	})

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(runCtx, 1)

	waitFor(t, 20*time.Second, "the stranded job to be requeued and run", func() bool {
		return ran.Load()
	})
}
