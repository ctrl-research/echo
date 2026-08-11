-- Enqueue is idempotent on dedupe_key: a watcher firing several events for one
-- file write produces one job. Touching run_after on conflict lets a repeated
-- event push the work later, which is what debouncing an active writer wants.
-- run_after is computed by the database, never by the caller. A client-supplied
-- timestamp makes scheduling depend on the app and database clocks agreeing: a
-- host a few milliseconds ahead produces jobs that are not yet claimable when
-- the NOTIFY arrives, so every one of them waits for the next poll instead.
-- name: EnqueueJob :one
INSERT INTO jobs (type, payload, priority, dedupe_key, run_after, max_attempts)
VALUES ($1, $2, $3, sqlc.narg(dedupe_key)::text, now() + sqlc.arg(delay)::interval, $4)
ON CONFLICT (dedupe_key) DO UPDATE
SET run_after = GREATEST(jobs.run_after, EXCLUDED.run_after),
    priority  = GREATEST(jobs.priority, EXCLUDED.priority)
RETURNING *;

-- The canonical multi-worker claim. SKIP LOCKED lets every worker drain the
-- queue concurrently without contending on the same row or double-claiming.
-- name: ClaimJob :one
UPDATE jobs
SET state = 'running', started_at = now(), attempts = attempts + 1
WHERE id = (
    SELECT id FROM jobs
    WHERE state = 'queued' AND run_after <= now()
    ORDER BY priority DESC, run_after
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- Clearing dedupe_key on completion lets the same logical work be enqueued
-- again later; leaving it would make the unique index reject every future
-- enqueue for that key.
-- name: CompleteJob :exec
UPDATE jobs
SET state = 'done', finished_at = now(), error = NULL, dedupe_key = NULL
WHERE id = $1;

-- A job below its attempt ceiling goes back to 'queued' with a backoff;
-- otherwise it is terminal.
-- name: RetryOrFailJob :one
UPDATE jobs
SET state = CASE WHEN attempts >= max_attempts THEN 'failed'::job_state
                 ELSE 'queued'::job_state END,
    error = sqlc.arg(error)::text,
    run_after = CASE WHEN attempts >= max_attempts THEN run_after
                     ELSE now() + sqlc.arg(backoff)::interval END,
    finished_at = CASE WHEN attempts >= max_attempts THEN now() ELSE NULL END,
    dedupe_key = CASE WHEN attempts >= max_attempts THEN NULL ELSE dedupe_key END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: ListJobs :many
SELECT * FROM jobs
WHERE (sqlc.narg(state)::job_state IS NULL OR state = sqlc.narg(state)::job_state)
ORDER BY created_at DESC
LIMIT $1;

-- name: CountJobsByState :many
SELECT state, count(*) AS count FROM jobs GROUP BY state;

-- Requeues jobs left 'running' by a process that died. Without this a crash
-- during a scan strands the work forever, since nothing else revisits a row
-- that was already claimed.
-- name: RequeueStaleJobs :execrows
UPDATE jobs
SET state = 'queued', started_at = NULL
WHERE state = 'running' AND started_at < now() - sqlc.arg(stale_after)::interval;

-- name: DeleteFinishedJobsBefore :execrows
DELETE FROM jobs
WHERE state IN ('done', 'failed') AND finished_at < now() - sqlc.arg(older_than)::interval;
