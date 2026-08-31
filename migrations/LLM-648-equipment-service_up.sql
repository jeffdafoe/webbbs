-- LLM-648: equipment service — Lewis Walker takes up the wright's trade.
--
-- Every owned business accrues EquipmentUse in proportion to its OUTPUT
-- (produced batches, harvests from the owner's own sources — engine
-- equipment_service.go), and only the wright's bought equipment_service
-- resets it: the mending idiom (a service whose delivery routes to an effect)
-- applied to the tools of a trade. High-production actors are the village's
-- coin pools, so the wright's bill lands on the wealthiest with no
-- purse-reading — a recirculation mechanism, not a drain (design record on
-- the Jira ticket).
--
-- WHAT this adds (the LLM-625 mending shape):
--   1. village_object.equipment_use — the checkpointed counter (the wear /
--      rate_owed pattern).
--   2. item_kind `whetstone` — the service consumable, a REAL forged good
--      (Ezekiel produces it from iron, deepening the LLM-442 iron sink; Jeff:
--      reusing nails would be uncreative). Priced w2/r4.
--   3. item_kind `equipment_service` — the service itself (category
--      `service`, capabilities {service, equipment_service}). Sellable only
--      by a keeper of a wright-tagged structure (engine gate); fixed price
--      w6/r10 — the recirculation rate scales through FREQUENCY, not price.
--   4. Ezekiel's blacksmith restock gains {whetstone, produce, max 4}; Lewis's
--      worker restock gains {whetstone, buy, max 4} (a consumable behind a
--      cue needs its restock entry — the LLM-324/LLM-608 rule).
--   5. The spare "Innkeeeper's Residence" structure becomes Lewis's Workshop
--      (Jeff's call, 2026-08-31): renamed, tagged business + wright, owned by
--      Lewis; Lewis gets role wright, the workshop as his work structure, and
--      a 9:00-18:00 local shift.
--   6. One-time whetstone seeds at Lewis and on Ezekiel's shelf so the trade
--      is live from deploy rather than waiting on the first forge batch.
--
-- ENGINE-OWNED tables throughout; deploy.sh runs stop -> migrate -> start, so
-- all of this applies engine-STOPPED.
--
-- Rerun-safe: ADD COLUMN IF NOT EXISTS; catalog upserts ON CONFLICT DO UPDATE
-- (corrective); restock appends are corrective rewrites converging on one
-- canonical entry; tag appends ANY-guarded; actor/object assignments are
-- plain idempotent UPDATEs; inventory seeds ON CONFLICT DO NOTHING
-- (engine-owned live stock after go-live). Loud validation at the end.

BEGIN;

-- 1. The counter column (the wear / rate_owed pattern).
ALTER TABLE village_object ADD COLUMN IF NOT EXISTS equipment_use integer NOT NULL DEFAULT 0;

-- 2. The whetstone item kind — a forged, portable consumable.
INSERT INTO item_kind
    (name, display_label, display_label_singular, display_label_plural,
     category, sort_order, capabilities, description)
VALUES
    ('whetstone', 'Whetstone', 'whetstone', 'whetstones',
     'material', 430, '{portable}'::text[],
     'A fine-grained sharpening stone off the smith''s bench — the wright''s whole trade rides on a good one.')
ON CONFLICT (name) DO UPDATE SET
    display_label          = EXCLUDED.display_label,
    display_label_singular = EXCLUDED.display_label_singular,
    display_label_plural   = EXCLUDED.display_label_plural,
    category               = EXCLUDED.category,
    sort_order             = EXCLUDED.sort_order,
    capabilities           = EXCLUDED.capabilities,
    description            = EXCLUDED.description;

-- 3. The equipment_service kind. {service, equipment_service}: `service`
--    skips the stock gates (no inventory backing), `equipment_service` routes
--    delivery to the equipment-restoration arm (transferOrderGoods, engine
--    LLM-648).
INSERT INTO item_kind
    (name, display_label, display_label_singular, display_label_plural,
     category, sort_order, capabilities, description)
VALUES
    ('equipment_service', 'Equipment service', 'equipment service', 'equipment service',
     'service', 440, '{service,equipment_service}'::text[],
     'The wright''s call: stones dressed, blades whetted, fittings trued — the tools of a trade brought back to keen.')
ON CONFLICT (name) DO UPDATE SET
    display_label          = EXCLUDED.display_label,
    display_label_singular = EXCLUDED.display_label_singular,
    display_label_plural   = EXCLUDED.display_label_plural,
    category               = EXCLUDED.category,
    sort_order             = EXCLUDED.sort_order,
    capabilities           = EXCLUDED.capabilities,
    description            = EXCLUDED.description;

-- 4. Recipes. The whetstone is a REAL forge recipe (iron in, stone out — the
--    nail/shovel shape); equipment_service is an inert price anchor (no
--    producer, empty inputs — the mending shape).
INSERT INTO item_recipe
    (output_item, output_qty, rate_qty, rate_per_hours, inputs,
     wholesale_price, retail_price)
VALUES
    ('whetstone', 1, 1, 1, '[{"item": "iron", "qty": 1}]'::jsonb, 2, 4),
    ('equipment_service', 1, 1, 1, '[]'::jsonb, 6, 10)
ON CONFLICT (output_item) DO UPDATE SET
    output_qty      = EXCLUDED.output_qty,
    rate_qty        = EXCLUDED.rate_qty,
    rate_per_hours  = EXCLUDED.rate_per_hours,
    inputs          = EXCLUDED.inputs,
    wholesale_price = EXCLUDED.wholesale_price,
    retail_price    = EXCLUDED.retail_price,
    updated_at      = now();

-- 5a. Ezekiel Crane's blacksmith restock gains the whetstone produce line.
--     Corrective rewrite: EVERY existing whetstone entry (any source — a prior
--     partial run or hand edit could have left a wrong-source one) is stripped
--     before the canonical {whetstone, produce, max 4} is appended, so a rerun
--     converges on exactly one entry (code_review).
UPDATE actor_attribute
   SET params = jsonb_set(
       params,
       '{restock}',
       COALESCE(
           (SELECT jsonb_agg(e)
              FROM jsonb_array_elements(
                   CASE WHEN jsonb_typeof(params->'restock') = 'array'
                        THEN params->'restock' ELSE '[]'::jsonb END) AS e
             WHERE e->>'item' IS DISTINCT FROM 'whetstone'),
           '[]'::jsonb
       ) || '[{"item": "whetstone", "source": "produce", "max": 4}]'::jsonb
   )
 WHERE actor_id = '019da6f9-1b4c-7dda-bb6b-3248cdafb2c4'  -- Ezekiel Crane
   AND slug = 'blacksmith';

-- 5b. Lewis Walker's worker restock gains the whetstone buy line (his params
--     are {} today; jsonb_set creates the restock key).
UPDATE actor_attribute
   SET params = jsonb_set(
       params,
       '{restock}',
       COALESCE(
           (SELECT jsonb_agg(e)
              FROM jsonb_array_elements(
                   CASE WHEN jsonb_typeof(params->'restock') = 'array'
                        THEN params->'restock' ELSE '[]'::jsonb END) AS e
             WHERE e->>'item' IS DISTINCT FROM 'whetstone'),
           '[]'::jsonb
       ) || '[{"item": "whetstone", "source": "buy", "max": 4}]'::jsonb
   )
 WHERE actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c'  -- Lewis Walker
   AND slug = 'worker';

-- 6. The workshop: the spare "Innkeeeper's Residence" (typo and all) becomes
--    Lewis's Workshop — renamed, tagged, owned. Pinned by id per the
--    conversion rule.
UPDATE village_object
   SET display_name = 'Lewis''s Workshop',
       owner_actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c'
 WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b';

UPDATE village_object
   SET tags = tags || '{business}'::text[]
 WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b'
   AND NOT ('business' = ANY(tags));

UPDATE village_object
   SET tags = tags || '{wright}'::text[]
 WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b'
   AND NOT ('wright' = ANY(tags));

-- 7. Lewis takes up the trade: role, workplace, a 9:00-18:00 local shift
--    (the Josiah 540-1260 convention, shorter — his afternoons are rounds).
UPDATE actor
   SET role = 'wright',
       work_structure_id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b',
       schedule_start_minute = 540,
       schedule_end_minute = 1080
 WHERE id = '019da6d4-24d2-7461-88b0-72b2b288bd5c';

-- 8. One-time whetstone seeds — bootstrap liveness, NOT a convergent
--    invariant (DO NOTHING, never DO UPDATE).
INSERT INTO actor_inventory (actor_id, item_kind, quantity)
SELECT '019da6d4-24d2-7461-88b0-72b2b288bd5c', 'whetstone', 2
 WHERE EXISTS (SELECT 1 FROM actor WHERE id = '019da6d4-24d2-7461-88b0-72b2b288bd5c')
ON CONFLICT (actor_id, item_kind) DO NOTHING;

INSERT INTO actor_inventory (actor_id, item_kind, quantity)
SELECT '019da6f9-1b4c-7dda-bb6b-3248cdafb2c4', 'whetstone', 2
 WHERE EXISTS (SELECT 1 FROM actor WHERE id = '019da6f9-1b4c-7dda-bb6b-3248cdafb2c4')
ON CONFLICT (actor_id, item_kind) DO NOTHING;

-- Validate loud. Catalog rows always land; the actor-facing steps are
-- asserted only where the underlying rows exist (a schema-only harness skips
-- them).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                    WHERE table_name = 'village_object' AND column_name = 'equipment_use') THEN
        RAISE EXCEPTION 'LLM-648: village_object.equipment_use missing after ALTER';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM item_kind WHERE name = 'whetstone' AND category = 'material'
                      AND 'portable' = ANY(capabilities)) THEN
        RAISE EXCEPTION 'LLM-648: whetstone item_kind missing or wrong shape after insert';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM item_kind WHERE name = 'equipment_service' AND category = 'service'
                      AND 'service' = ANY(capabilities) AND 'equipment_service' = ANY(capabilities)) THEN
        RAISE EXCEPTION 'LLM-648: equipment_service item_kind missing or wrong capability shape after insert';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM item_recipe WHERE output_item = 'whetstone'
                      AND wholesale_price = 2 AND retail_price = 4
                      AND inputs @> '[{"item": "iron", "qty": 1}]'::jsonb) THEN
        RAISE EXCEPTION 'LLM-648: whetstone recipe missing or wrong shape after insert';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM item_recipe WHERE output_item = 'equipment_service'
                      AND wholesale_price = 6 AND retail_price = 10) THEN
        RAISE EXCEPTION 'LLM-648: equipment_service price anchor missing after insert';
    END IF;

    IF EXISTS (SELECT 1 FROM actor) THEN
        IF NOT EXISTS (SELECT 1 FROM actor_attribute
                        WHERE actor_id = '019da6f9-1b4c-7dda-bb6b-3248cdafb2c4' AND slug = 'blacksmith'
                          AND params->'restock' @> '[{"item": "whetstone", "source": "produce", "max": 4}]'::jsonb) THEN
            RAISE EXCEPTION 'LLM-648: Ezekiel''s whetstone produce entry did not land';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM actor_attribute
                        WHERE actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c' AND slug = 'worker'
                          AND params->'restock' @> '[{"item": "whetstone", "source": "buy", "max": 4}]'::jsonb) THEN
            RAISE EXCEPTION 'LLM-648: Lewis''s whetstone buy entry did not land';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM village_object
                        WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b'
                          AND display_name = 'Lewis''s Workshop'
                          AND owner_actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c'
                          AND 'business' = ANY(tags) AND 'wright' = ANY(tags)) THEN
            RAISE EXCEPTION 'LLM-648: the workshop 019e0e3c... did not convert (stale id?)';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM actor
                        WHERE id = '019da6d4-24d2-7461-88b0-72b2b288bd5c'
                          AND role = 'wright'
                          AND work_structure_id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b') THEN
            RAISE EXCEPTION 'LLM-648: Lewis''s wright assignment did not land';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM actor_inventory
                        WHERE actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c' AND item_kind = 'whetstone') THEN
            RAISE EXCEPTION 'LLM-648: Lewis''s whetstone seed missing';
        END IF;
    END IF;
END $$;

COMMIT;
