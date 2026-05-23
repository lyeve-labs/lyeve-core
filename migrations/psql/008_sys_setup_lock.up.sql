-- Sys_setup_lock, the row first-run setup locks.
--
-- Creating the first super admin counts sys_users and inserts one row. Two
-- callers that count at the same moment both see zero, so the count and the
-- insert run in one transaction that first updates this row: the update holds
-- the row lock until commit, the second caller waits on it, and its count then
-- sees the first caller's account. A lock on a known row behaves the same on
-- every dialect and at every isolation level, which a range lock on an empty
-- table does not. The table holds exactly one row and no tenant data.

BEGIN;

CREATE TABLE IF NOT EXISTS sys_setup_lock (
    id         INTEGER PRIMARY KEY,
    claimed_at TIMESTAMPTZ
);

INSERT INTO sys_setup_lock (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

COMMIT;
