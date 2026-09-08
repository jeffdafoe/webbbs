-- LLM-652: the estate rate — a daily assessment on coin held above a floor,
-- paid into a town chest.
--
-- Once a game-day the engine moves estate_rate_pct_per_day percent of each
-- resident NPC's coin above estate_rate_floor into the town chest (engine
-- estate_rate.go, on the rotation boundary beside the farm-upkeep and day's-rate
-- passes). The chest is the only new durable state: the coin has LEFT the
-- purses, so losing it on restart would destroy coin — it rides world_state
-- with the rest of the checkpointed environment.
--
-- The two knobs (estate_rate_floor 100, estate_rate_pct_per_day 5) are registry
-- settings with compiled defaults; they need no setting rows here and are
-- live-tunable through the umbilical (/settings/set), persisting on the next
-- checkpoint.
--
-- ENGINE-OWNED table; deploy.sh runs stop -> migrate -> start, so this applies
-- engine-STOPPED. Rerun-safe: ADD COLUMN IF NOT EXISTS. Loud validation at the
-- end.

BEGIN;

ALTER TABLE world_state ADD COLUMN IF NOT EXISTS town_chest_coins integer NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                    WHERE table_name = 'world_state' AND column_name = 'town_chest_coins') THEN
        RAISE EXCEPTION 'LLM-652: world_state.town_chest_coins missing after ALTER';
    END IF;
END $$;

COMMIT;
