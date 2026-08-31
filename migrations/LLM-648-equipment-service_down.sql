-- LLM-648 rollback: remove the equipment-service economy — the counter
-- column, the whetstone/equipment_service catalog rows, the restock entries,
-- Lewis's wright assignment, and the workshop conversion (restored to the
-- spare "Innkeeeper's Residence" it was, typo and all).
--
-- Engine-STOPPED like the up (deploy.sh stop -> migrate -> start).

BEGIN;

-- Lewis back to a workless worker.
UPDATE actor
   SET role = '',
       work_structure_id = NULL,
       schedule_start_minute = NULL,
       schedule_end_minute = NULL
 WHERE id = '019da6d4-24d2-7461-88b0-72b2b288bd5c'
   AND role = 'wright';

-- The workshop back to the unowned, untagged spare it was — including
-- dropping the structure row the up created (the spare was decoration-only;
-- Lewis's work_structure_id is nulled above, so nothing references it).
UPDATE village_object
   SET display_name = 'Innkeeeper''s Residence',
       owner_actor_id = NULL,
       tags = array_remove(array_remove(tags, 'wright'), 'business')
 WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b';

DELETE FROM structure WHERE id = '019e0e3c-56fb-71a2-96b3-0f0740b6077b';

-- Strip the restock entries.
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
       ))
 WHERE (actor_id = '019da6f9-1b4c-7dda-bb6b-3248cdafb2c4' AND slug = 'blacksmith')
    OR (actor_id = '019da6d4-24d2-7461-88b0-72b2b288bd5c' AND slug = 'worker');

-- Catalog + stock. Ledger rows referencing the kinds FK-cascade per the
-- item_kind ON UPDATE CASCADE posture; historical pay_ledger rows keep their
-- item_kind via the FK, so the kinds can only go once nothing references
-- them — delete stock and recipes first, then the kinds (skipped with a
-- notice if still referenced).
DELETE FROM actor_inventory WHERE item_kind IN ('whetstone', 'equipment_service');
DELETE FROM item_recipe WHERE output_item IN ('whetstone', 'equipment_service');
DO $$
BEGIN
    DELETE FROM item_kind WHERE name IN ('whetstone', 'equipment_service');
EXCEPTION WHEN foreign_key_violation THEN
    RAISE NOTICE 'LLM-648 down: item kinds kept — pay_ledger history references them';
END $$;

ALTER TABLE village_object DROP COLUMN IF EXISTS equipment_use;

COMMIT;
