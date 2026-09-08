-- LLM-652 rollback: drop the town chest.
--
-- Whatever the chest holds is DESTROYED by this rollback — the coin was taken
-- out of purses by the assessment and nothing puts it back. Read
-- world_state.town_chest_coins before running this if the amount matters.
-- Engine-STOPPED like the up (deploy.sh stop -> migrate -> start).

BEGIN;

ALTER TABLE world_state DROP COLUMN IF EXISTS town_chest_coins;

COMMIT;
