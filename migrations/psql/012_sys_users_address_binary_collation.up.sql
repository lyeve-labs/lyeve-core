-- sys_users.email and provider_email compare by byte on MySQL and SQL Server
-- from this version on, as they always have here.
--
-- PostgreSQL compares text by byte under the default collation, so two
-- addresses that differ by an accent, by a sharp s or by a ligature are
-- already two values for its unique keys and lookups. Nothing changes on this
-- engine. The file keeps the version numbers of the three engines equal.

SELECT 1;
