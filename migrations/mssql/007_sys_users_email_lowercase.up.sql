-- Fold sys_users.email to lower case so the three engines agree on identity.
-- See the psql tree for the full reasoning.
--
-- Under the default case-insensitive collation the unique constraint already
-- refused a second row differing only by case, so the guard below cannot fire
-- on a stock install. It is here for one configured with a case-sensitive
-- collation, where the pair is possible.
--
-- Both sides of the comparison carry an explicit binary collation because the
-- default one reports email = LOWER(email) for every row, which would rewrite
-- the whole table.

-- 2048 is the widest message THROW accepts. A collision list longer than that
-- truncates rather than failing, which still names enough addresses to act on.
DECLARE @collisions NVARCHAR(2048);

SELECT @collisions = STRING_AGG(CAST(d.folded AS NVARCHAR(MAX)), ', ')
  FROM (
      SELECT LOWER(email) AS folded
        FROM sys_users
       GROUP BY LOWER(email)
      HAVING COUNT(*) > 1
  ) AS d;

IF @collisions IS NOT NULL
BEGIN
    DECLARE @msg NVARCHAR(2048) = N'sys_users holds accounts differing only by the case of their address: '
        + @collisions
        + N'. Decide which row keeps each address, re-address or remove the others, then run this migration again.';
    THROW 50016, @msg, 1;
END;

UPDATE sys_users
   SET email = LOWER(email)
 WHERE email COLLATE Latin1_General_BIN2 <> LOWER(email) COLLATE Latin1_General_BIN2;
