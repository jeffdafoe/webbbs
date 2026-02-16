-- ZBBS-002 rollback

DELETE FROM migrations_applied WHERE migration_id IN ('ZBBS-001', 'ZBBS-002');
DROP TABLE IF EXISTS migrations_applied;

DROP SCHEMA IF EXISTS doors;

ALTER TABLE "user" ADD COLUMN is_verified BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE "user" ALTER COLUMN email SET NOT NULL;
