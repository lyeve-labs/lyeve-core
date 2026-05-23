-- Fold sys_users.email to lower case so the three engines agree on identity.
-- See the psql tree for the full reasoning.
--
-- Under the default case-insensitive collation the unique constraint already
-- refused a second row differing only by case, so the guard below cannot fire
-- on a stock install. It is here for one configured with a case-sensitive
-- collation, where the pair is possible and the bare UPDATE would otherwise
-- fail with a duplicate-key error that names one address and no reason.
--
-- The comparison casts to BINARY because a case-insensitive collation reports
-- email = LOWER(email) for every row, which would update the whole table.
--
-- The work sits in a procedure because MySQL accepts SIGNAL only inside a
-- stored program. No DELIMITER is needed: the server parses the compound body
-- itself, and the client never splits the file.
--
-- If the guard does fire, the remedy is the one the psql tree spells out:
-- decide which row keeps each address, re-address or remove the others, then
-- run this migration again. It is not in the raised message because MySQL caps
-- that at 128 characters.

DROP PROCEDURE IF EXISTS lyeve_migrate_016_lower_emails;

CREATE PROCEDURE lyeve_migrate_016_lower_emails()
BEGIN
    DECLARE collisions TEXT;

    SELECT GROUP_CONCAT(DISTINCT LOWER(email) ORDER BY LOWER(email) SEPARATOR ', ')
      INTO collisions
      FROM sys_users
     WHERE LOWER(email) IN (
         SELECT d.folded FROM (
             SELECT LOWER(email) AS folded
               FROM sys_users
              GROUP BY LOWER(email)
             HAVING COUNT(*) > 1
         ) AS d
     );

    -- MESSAGE_TEXT is capped at 128 characters and SIGNAL raises error 1648
    -- rather than truncating, so a longer message aborts the migration with a
    -- complaint about the message and no mention of the addresses. LEFT keeps
    -- it inside the cap, and the addresses come first because they are the
    -- part an operator cannot reconstruct from this file. Do not remove the
    -- LEFT to make the wording nicer.
    IF collisions IS NOT NULL THEN
        SET @lyeve_migrate_016_msg = LEFT(CONCAT(
            'case-duplicate addresses in sys_users: ', collisions), 128);
        SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = @lyeve_migrate_016_msg;
    END IF;

    UPDATE sys_users
       SET email = LOWER(email)
     WHERE CAST(email AS BINARY) <> CAST(LOWER(email) AS BINARY);
END;

CALL lyeve_migrate_016_lower_emails();

DROP PROCEDURE IF EXISTS lyeve_migrate_016_lower_emails;
