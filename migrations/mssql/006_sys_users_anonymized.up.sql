-- Add anonymized flag to sys_users for GDPR right-to-be-forgotten tracking.

ALTER TABLE [sys_users] ADD [anonymized] BIT NOT NULL CONSTRAINT [df_sys_users_anonymized] DEFAULT 0;
