-- Sys_setup_lock, the row first-run setup locks. See the psql migration.
CREATE TABLE IF NOT EXISTS sys_setup_lock (
    id         INT NOT NULL PRIMARY KEY,
    claimed_at DATETIME(6) NULL
) ENGINE=InnoDB;

INSERT IGNORE INTO sys_setup_lock (id) VALUES (1);
