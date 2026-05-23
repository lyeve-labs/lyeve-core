-- No DDL to reverse for 007_sys_users_email_lowercase.
--
-- The up migration folds a value and keeps no record of what it replaced, so
-- the original capitalization cannot be restored. Writing a down script that
-- guessed at it would invent data.
--
-- The collision guard needs no counterpart either. Folding removes rows from
-- no table and merges nothing, so rolling back leaves exactly the accounts
-- that existed before, addressed in lower case. A reader that compares with
-- LOWER() on both sides, or against a handler-folded argument, still matches
-- a folded row.

SELECT 1;
