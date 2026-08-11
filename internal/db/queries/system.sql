-- Queries with no table dependencies. Their real job in M0 is to keep the
-- sqlc pipeline exercised before the schema exists; ServerTime is also a
-- genuine readiness probe, since it proves a round trip rather than just a
-- reachable socket the way Ping does.

-- name: ServerTime :one
SELECT now()::timestamptz AS server_time;
