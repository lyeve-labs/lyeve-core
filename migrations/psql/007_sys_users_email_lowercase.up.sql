-- Fold sys_users.email to lower case so the three engines agree on identity.
--
-- sys_users.email is UNIQUE, but PostgreSQL compares TEXT case sensitively
-- while MySQL and SQL Server fold case under their default collations. The
-- same pair of addresses would be two accounts here and a constraint violation
-- there. The engine normalizes on every write and looks the address up
-- with a plain equality that the unique index can serve, so the stored rows
-- have to be folded to match.
--
-- A collision aborts the migration. Two rows differing only by case are two
-- accounts, with two password hashes, two role sets and two audit trails.
-- Picking a winner here would delete somebody's account, and a migration is
-- the wrong place to make that call: an operator decides which row keeps the
-- address and re-addresses or removes the rest, then runs this again.
--
-- Only PostgreSQL can be holding such a pair. On the other two engines the
-- unique constraint refused the second row when it was written.

BEGIN;

DO $$
DECLARE
    collisions TEXT;
BEGIN
    SELECT string_agg(DISTINCT lower(email), ', ' ORDER BY lower(email))
      INTO collisions
      FROM sys_users
     WHERE lower(email) IN (
         SELECT lower(email)
           FROM sys_users
          GROUP BY lower(email)
         HAVING count(*) > 1
     );

    IF collisions IS NOT NULL THEN
        RAISE EXCEPTION 'sys_users holds accounts differing only by the case of their address: %. Decide which row keeps each address, re-address or remove the others, then run this migration again.', collisions;
    END IF;
END
$$;

UPDATE sys_users SET email = lower(email) WHERE email <> lower(email);

COMMIT;
