-- ZBBS-002: Auth changes, doors schema, migration tracking

ALTER TABLE "user" DROP COLUMN is_verified;
ALTER TABLE "user" ALTER COLUMN email DROP NOT NULL;

CREATE SCHEMA IF NOT EXISTS doors;

CREATE TABLE IF NOT EXISTS migrations_applied (
    migration_id VARCHAR(50) PRIMARY KEY,
    applied_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
);

INSERT INTO migrations_applied (migration_id) VALUES ('ZBBS-001') ON CONFLICT DO NOTHING;
INSERT INTO migrations_applied (migration_id) VALUES ('ZBBS-002') ON CONFLICT DO NOTHING;
