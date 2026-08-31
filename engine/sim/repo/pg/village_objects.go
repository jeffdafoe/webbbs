package pg

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// VillageObjectsRepo reads and writes VillageObject rows against
// village_object plus the object_refresh child table. Owns both as one
// aggregate: the v1 sidecar village_object_tag was collapsed into the
// parent's tags TEXT[] column by migration ZBBS-WORK-237. The
// object_refresh supply/regen/dwell columns are the prod baseline shape
// (HOME ZBBS-090 / ZBBS-172); migration ZBBS-WORK-238 adds only the
// gen-marker snapshot_gen bookkeeping on top.
//
// SaveSnapshot semantics use the generation-marker pattern (Slice 9 —
// see `shared/notes/codebase/salem-engine-v2/pg-snapshot-pattern`).
// Both tables get per-row UPSERT inside the caller's checkpoint Tx,
// stamping the new gen on every snapshot row, then per-table
// `DELETE WHERE snapshot_gen < $gen` prunes anything absent. The
// supplied map IS the complete VillageObject + Refreshes set.
//
// The two tables share the parent's advisory lock (acquired at the
// start of SaveSnapshot) — Refreshes never SaveSnapshot independently;
// it's always part of the same Tx. Each table owns its own sequence
// for the gen counter so the tiers are independent (writes to
// village_object don't perturb the object_refresh counter).
type VillageObjectsRepo struct {
	pool Pool
}

// NewVillageObjectsRepo constructs a VillageObjectsRepo against the
// given pool. Mainly for callers that want to swap just this sub-repo;
// the normal path is pg.NewRepository which wires this internally.
func NewVillageObjectsRepo(pool Pool) *VillageObjectsRepo {
	return &VillageObjectsRepo{pool: pool}
}

// loadAllSQL selects every village_object row. No JOIN — tags collapsed
// onto the parent (TEXT[]) by migration ZBBS-WORK-237. owner is the v1
// legacy string still in place for v1 reader compat; v2 reads
// owner_actor_id (typed) which the migration backfilled via the actor
// table join.
//
// snapshot_gen is not selected — it's pure sync bookkeeping for the
// SaveSnapshot delete-stale step and has no in-memory representation.
//
// No ORDER BY — the in-memory model is map-keyed, so row order doesn't
// matter. Skipping the sort keeps LoadAll cheap at restart.
const loadAllSQLVO = `
SELECT
    id, asset_id, current_state, x, y, placed_by, display_name,
    entry_policy, owner_actor_id, attached_to,
    loiter_offset_x, loiter_offset_y, available_quantity, tags, wear,
    hearth_lit_until, rate_owed, equipment_use
FROM village_object`

// upsertSQLVO writes one VillageObject row. snapshot_gen is included
// in both INSERT and UPDATE branches — every snapshot row carries the
// gen for this checkpoint so the trailing DELETE can prune stale rows
// (i.e., rows present in pg but absent from the snapshot).
//
// ON CONFLICT (id) infers village_object's PRIMARY KEY (id) — UUID,
// established by ZBBS-006-assets. If a future migration changes the
// key shape, this conflict target needs to follow.
//
// On conflict, every v2-owned column gets updated (this IS the snapshot
// — we're asserting the row's current state in full). The v1 legacy
// owner column is NOT touched; v1 readers still own that. created_at
// is NOT touched on update (audit-only field; preserved from original
// INSERT).
const upsertSQLVO = `
INSERT INTO village_object (
    id, asset_id, current_state, x, y, placed_by, display_name,
    entry_policy, owner_actor_id, attached_to,
    loiter_offset_x, loiter_offset_y, available_quantity, tags,
    wear, hearth_lit_until, rate_owed, snapshot_gen, equipment_use
) VALUES (
    $1::uuid, $2::uuid, $3, $4, $5, $6, $7,
    $8, $9, $10::uuid,
    $11, $12, $13, $14,
    $15, $16, $17, $18, $19
)
ON CONFLICT (id) DO UPDATE SET
    asset_id           = EXCLUDED.asset_id,
    current_state      = EXCLUDED.current_state,
    x                  = EXCLUDED.x,
    y                  = EXCLUDED.y,
    placed_by          = EXCLUDED.placed_by,
    display_name       = EXCLUDED.display_name,
    entry_policy       = EXCLUDED.entry_policy,
    owner_actor_id     = EXCLUDED.owner_actor_id,
    attached_to        = EXCLUDED.attached_to,
    loiter_offset_x    = EXCLUDED.loiter_offset_x,
    loiter_offset_y    = EXCLUDED.loiter_offset_y,
    available_quantity = EXCLUDED.available_quantity,
    tags               = EXCLUDED.tags,
    wear               = EXCLUDED.wear,
    hearth_lit_until   = EXCLUDED.hearth_lit_until,
    rate_owed          = EXCLUDED.rate_owed,
    equipment_use      = EXCLUDED.equipment_use,
    snapshot_gen       = EXCLUDED.snapshot_gen`

// deleteStaleSQLVO prunes village_object rows whose snapshot_gen is
// less than the just-bumped checkpoint gen — i.e., rows that exist in
// pg but were absent from the in-memory snapshot map. This is the
// generation-marker pattern's delete-absent step.
//
// FK village_object.attached_to → village_object.id ON DELETE CASCADE
// means dropping a parent would also drop its attached overlays. The
// safer DELETE refuses to drop a stale parent that still has a fresh
// child attached to it — a naive `DELETE WHERE snapshot_gen < $1`
// would silently destroy the fresh child via CASCADE. World-side
// invariants normally keep parent + child in the same gen tier; this
// guard is defense-in-depth, and the follow-up orphan check (see
// orphanCheckSQLVO) surfaces any cross-tier violation loudly.
const deleteStaleSQLVO = `
DELETE FROM village_object stale
 WHERE stale.snapshot_gen < $1
   AND NOT EXISTS (
       SELECT 1 FROM village_object fresh
        WHERE fresh.attached_to = stale.id
          AND fresh.snapshot_gen = $1
   )`

// orphanCheckSQLVO counts fresh children that reference a stale parent
// (one whose snapshot_gen is older than the current checkpoint gen).
// World-side invariants should keep parent + child in the same gen
// tier, so this count is always 0 in practice. A non-zero count means
// the world goroutine wrote a fresh child referencing a parent that
// isn't in the snapshot — a bug surface the caller needs to know
// about. SaveSnapshot returns an error and rolls back the Tx so the
// invariant violation is loud rather than silent.
const orphanCheckSQLVO = `
SELECT COUNT(*) FROM village_object fresh
  JOIN village_object stale ON fresh.attached_to = stale.id
 WHERE fresh.snapshot_gen = $1 AND stale.snapshot_gen < $1`

// nextGenSQLVO bumps the per-aggregate sequence and returns the new
// gen. Per-aggregate sequence (not a process-local counter) is atomic,
// persistent across restart, and avoids cross-aggregate coordination
// at checkpoint time.
const nextGenSQLVO = `SELECT nextval('village_object_snapshot_gen_seq')`

// timeOrZero unwraps a nullable timestamp scan target: NULL → the zero time
// (the in-memory "never" convention, e.g. VillageObject.HearthLitUntil).
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// advisoryLockSQLVO is the **single global lock for the village_object
// snapshot** — held for the Tx duration (released automatically on
// commit/rollback). Serializes ALL concurrent SaveSnapshot calls for
// this table. Today v2 is single-realm and SaveSnapshot is always the
// full village_object set, so "global per table" is equivalent to
// "per snapshot" — every SaveSnapshot covers the same aggregate
// instance.
//
// The gen-marker pattern is only correct when snapshots don't
// interleave — a concurrent older snapshot's delete-stale step would
// silently wipe a newer snapshot's just-written rows (gen=10 vs
// gen=11; the older Tx's `DELETE WHERE snapshot_gen < 10` runs after
// the newer Tx commits at gen=11, deleting nothing — BUT the newer
// Tx's `DELETE WHERE snapshot_gen < 11` runs after the older Tx
// commits at gen=10, deleting the older Tx's just-written set).
//
// In v2's single-world-goroutine architecture this is normally
// guaranteed by caller serialization (the world goroutine is the sole
// checkpoint writer). The advisory lock makes the invariant enforced
// rather than assumed — defense against future callers that bypass
// the world goroutine (admin tools, cutover scripts, parallel
// migrations).
//
// `pg_advisory_xact_lock(classid, objid)` takes two int4 args.
// `hashtext('village_object_snapshot')` is the classid; objid is 0.
// classid collisions across aggregate-type labels are statistically
// negligible (32-bit hash space); a false collision would just briefly
// serialize unrelated aggregates' snapshots without correctness loss.
//
// **Multi-realm upgrade path** (when realms land): the second arg
// should become a realm/world identifier so SaveSnapshot for realm A
// doesn't serialize against realm B's snapshot, e.g.
// `pg_advisory_xact_lock(hashtext('village_object_snapshot'), hashtext($1))`
// where $1 is the realm ID. Today single-realm so the global lock is
// correct and there's no parameter to pass.
const advisoryLockSQLVO = `SELECT pg_advisory_xact_lock(hashtext('village_object_snapshot'), 0)`

// loadAllSQLOR selects every object_refresh row across all parents.
// Group-by-object_id happens in Go after the query (LoadAll stitches
// the slice into each parent's Refreshes field, in row order).
//
// ORDERED so a parent's Refreshes slice is byte-identical across restarts
// (LLM-576). Without it Postgres may hand back rows in any order, and anything
// that reads a row BY POSITION — sim.gatherableSupply picks the row that dates a
// crop's growth stage — could silently pick a different one after a restart, on
// any object carrying more than one gatherable row. (A bush can: the raspberry
// bushes hold a need-bearing row that is also gatherable.)
//
// COALESCE, not a bare ORDER BY, because the sort must agree with the Go-side
// key. attribute and gather_item are nullable and scan into "" below, while
// Postgres sorts NULLs LAST by default — so an unguarded ORDER BY would put a
// yield-only row after the named ones while Go's empty string sorts it first.
// Keep this in step with sim.refreshOrderKey.
//
// snapshot_gen is not selected — same posture as the parent table:
// gen is pure sync bookkeeping, no in-memory representation.
const loadAllSQLOR = `
SELECT
    object_id, attribute, amount,
    max_quantity, available_quantity,
    refresh_mode, refresh_period_hours, last_refresh_at,
    dwell_amount, dwell_period_minutes, gather_item
FROM object_refresh
ORDER BY object_id, COALESCE(gather_item, ''), COALESCE(attribute, '')`

// upsertNeedSQLOR / upsertYieldSQLOR each write one object_refresh row. LLM-264
// moved the row's PK to a surrogate id (so attribute could go nullable) and gave
// each row type its own conflict target so both UPDATE in place on conflict:
//   - upsertNeedSQLOR: a need-bearing row (attribute IS NOT NULL) conflicts on
//     (object_id, attribute) — the full UNIQUE constraint
//     object_refresh_object_attribute_key, via a bare ON CONFLICT (also what the
//     already-applied LLM-50 / LLM-58 seed migrations rely on).
//   - upsertYieldSQLOR: a yield-only row (attribute IS NULL, which the constraint
//     above leaves unconstrained since NULLs are distinct) conflicts on
//     (object_id, gather_item) — the object_refresh_yield_key partial index.
//
// Because both update in place, a re-saved row keeps its surrogate id — no churn,
// no reliance on the trailing delete-stale to dedup a re-inserted NULL row. The
// save loop picks the statement by whether the row carries an attribute.
//
// snapshot_gen is included in both INSERT and UPDATE branches; the trailing DELETE
// step prunes rows absent from the snapshot. On conflict, every mutable v2-owned
// column is refreshed (the snapshot is authoritative) EXCEPT the row's own conflict
// key (attribute for a need row, gather_item for a yield row), which never changes.
//
// Column-name note (prod baseline ZBBS-090 / ZBBS-172):
//   - object_refresh's config-rate column is `dwell_amount` (smallint,
//     CHECK < 0). The in-memory field is ObjectRefresh.DwellDelta — kept
//     "Delta" because it's stored negative and is copied verbatim onto
//     the per-actor DwellCredit.DwellDelta. The separate per-actor credit
//     snapshot column `dwell_delta` lives on actor_dwell_credit, not here.
//   - refresh_mode is NOT NULL DEFAULT 'continuous' in prod; SaveSnapshot
//     writes 'continuous' for infinite rows (mode is irrelevant when
//     available_quantity IS NULL, but the column can't be NULL).
const upsertNeedSQLOR = `
INSERT INTO object_refresh (
    object_id, attribute, amount,
    max_quantity, available_quantity,
    refresh_mode, refresh_period_hours, last_refresh_at,
    dwell_amount, dwell_period_minutes, gather_item,
    snapshot_gen
) VALUES (
    $1::uuid, $2, $3,
    $4, $5,
    $6, $7, $8,
    $9, $10, $11,
    $12
)
ON CONFLICT (object_id, attribute) DO UPDATE SET
    amount               = EXCLUDED.amount,
    max_quantity         = EXCLUDED.max_quantity,
    available_quantity   = EXCLUDED.available_quantity,
    refresh_mode         = EXCLUDED.refresh_mode,
    refresh_period_hours = EXCLUDED.refresh_period_hours,
    last_refresh_at      = EXCLUDED.last_refresh_at,
    dwell_amount         = EXCLUDED.dwell_amount,
    dwell_period_minutes = EXCLUDED.dwell_period_minutes,
    gather_item          = EXCLUDED.gather_item,
    snapshot_gen         = EXCLUDED.snapshot_gen`

// upsertYieldSQLOR — the yield-only row variant. Conflict key is
// (object_id, gather_item) (attribute is NULL and stays NULL); gather_item is the
// key so it is NOT in the UPDATE SET.
const upsertYieldSQLOR = `
INSERT INTO object_refresh (
    object_id, attribute, amount,
    max_quantity, available_quantity,
    refresh_mode, refresh_period_hours, last_refresh_at,
    dwell_amount, dwell_period_minutes, gather_item,
    snapshot_gen
) VALUES (
    $1::uuid, $2, $3,
    $4, $5,
    $6, $7, $8,
    $9, $10, $11,
    $12
)
ON CONFLICT (object_id, gather_item) WHERE attribute IS NULL DO UPDATE SET
    amount               = EXCLUDED.amount,
    max_quantity         = EXCLUDED.max_quantity,
    available_quantity   = EXCLUDED.available_quantity,
    refresh_mode         = EXCLUDED.refresh_mode,
    refresh_period_hours = EXCLUDED.refresh_period_hours,
    last_refresh_at      = EXCLUDED.last_refresh_at,
    dwell_amount         = EXCLUDED.dwell_amount,
    dwell_period_minutes = EXCLUDED.dwell_period_minutes,
    snapshot_gen         = EXCLUDED.snapshot_gen`

// deleteStaleSQLOR prunes object_refresh rows whose snapshot_gen is
// below the just-bumped checkpoint gen — i.e., rows that exist in pg
// but were absent from the in-memory snapshot's Refreshes slices.
//
// Two categories of stale row get pruned here:
//  1. Parent survived but a refresh attribute was removed (e.g.,
//     admin deleted "hunger" from a shaded oak that used to refresh
//     both hunger and tiredness).
//  2. Parent rows already FK-CASCADE-deleted by the village_object
//     delete-stale step would normally take their refreshes down
//     with them; in the unlikely case a stale refresh row survives
//     (orphan; FK didn't fire), this DELETE sweeps it.
//
// No safer-DELETE variant needed — object_refresh has no self-FK,
// so no CASCADE pathology to defend against.
const deleteStaleSQLOR = `DELETE FROM object_refresh WHERE snapshot_gen < $1`

// nextGenSQLOR bumps the object_refresh sequence and returns the new
// gen. Independent from the village_object sequence so the two tables
// have separate gen tiers (writes to one don't perturb the other's
// counter).
const nextGenSQLOR = `SELECT nextval('object_refresh_snapshot_gen_seq')`

// LoadAll loads every village_object row and its object_refresh
// children into memory.
//
// Runs against the pool directly (no Tx) — read-only restart path,
// same posture as OrdersRepo.LoadAll. This relies on LoadAll running
// before the world goroutine starts and before any checkpoint writer
// can mutate these tables. Without that startup guarantee, the parent
// and child queries could observe different committed states under
// READ COMMITTED — a fresh refresh row could be visible to the
// child query but its newly-inserted parent invisible to the parent
// query (or vice versa for a delete), producing an orphan-skip log
// and a missing refresh.
//
// Refresh rows referencing an object_id that isn't in the loaded
// parent set are skipped with a log line (FK CASCADE makes this
// impossible from valid writes; the guard surfaces schema drift
// loudly rather than silently dropping data).
func (r *VillageObjectsRepo) LoadAll(ctx context.Context) (map[sim.VillageObjectID]*sim.VillageObject, error) {
	rows, err := r.pool.Query(ctx, loadAllSQLVO)
	if err != nil {
		return nil, fmt.Errorf("pg village_objects LoadAll query: %w", err)
	}
	defer rows.Close()

	out := make(map[sim.VillageObjectID]*sim.VillageObject)
	for rows.Next() {
		var (
			id             string
			assetID        *string // nullable in baseline; required in-memory (validated below)
			currentState   string
			x, y           float64
			placedBy       *string // nullable in baseline; required in-memory (validated below)
			displayName    *string // nullable in baseline; required in-memory (validated below)
			entryPolicy    string
			ownerActorID   *string // NULL when no owner
			attachedTo     *string // NULL when not an overlay
			loiterX        *int
			loiterY        *int
			availableQty   int
			tags           []string
			wear           int
			hearthLitUntil *time.Time // NULL when the fire has never been lit (zero time in-memory)
			rateOwed       int
			equipmentUse   int
		)
		if err := rows.Scan(
			&id, &assetID, &currentState, &x, &y, &placedBy, &displayName,
			&entryPolicy, &ownerActorID, &attachedTo,
			&loiterX, &loiterY, &availableQty, &tags, &wear,
			&hearthLitUntil, &rateOwed, &equipmentUse,
		); err != nil {
			return nil, fmt.Errorf("pg village_objects LoadAll scan: %w", err)
		}

		// asset_id / placed_by / display_name are nullable in the prod
		// baseline but the in-memory model requires them (asset_id is the
		// catalog ref; the other two scan into non-pointer Go strings). A
		// NULL means malformed data (legacy / external write). Refuse to
		// load with a precise, logged reason naming the offending row +
		// column — better than an opaque scan error or silently coercing
		// to "". Operator runs the village_object NULL data fixup (home
		// mail 2026-05-20) to clear these before the engine can start.
		if assetID == nil || placedBy == nil || displayName == nil {
			var col string
			switch {
			case assetID == nil:
				col = "asset_id"
			case placedBy == nil:
				col = "placed_by"
			default:
				col = "display_name"
			}
			log.Printf("pg village_objects LoadAll: village_object %s has NULL %s — refusing to load; engine cannot start until the village_object NULL data fixup runs", id, col)
			return nil, fmt.Errorf("pg village_objects LoadAll: village_object %s has NULL %s (required column) — aborting load", id, col)
		}
		var owner sim.ActorID
		if ownerActorID != nil {
			owner = sim.ActorID(*ownerActorID)
		}
		var attached sim.VillageObjectID
		if attachedTo != nil {
			attached = sim.VillageObjectID(*attachedTo)
		}
		// Normalize nil → empty slice. The column is NOT NULL DEFAULT
		// '{}' so this should never trigger in practice, but a future
		// schema drift (manual SQL writing NULL) shouldn't propagate as
		// nil into the in-memory shape — SaveSnapshot normalizes nil to
		// empty too, so consistent semantic on both sides.
		if tags == nil {
			tags = []string{}
		}
		out[sim.VillageObjectID(id)] = &sim.VillageObject{
			ID:                sim.VillageObjectID(id),
			AssetID:           sim.AssetID(*assetID),
			CurrentState:      currentState,
			Pos:               sim.WorldPos{X: x, Y: y},
			PlacedBy:          *placedBy,
			DisplayName:       *displayName,
			EntryPolicy:       sim.EntryPolicy(entryPolicy),
			OwnerActorID:      owner,
			AttachedTo:        attached,
			LoiterOffsetX:     loiterX,
			LoiterOffsetY:     loiterY,
			Tags:              tags,
			AvailableQuantity: availableQty,
			Wear:              wear,
			RateOwed:          rateOwed,
			EquipmentUse:      equipmentUse,
			HearthLitUntil:    timeOrZero(hearthLitUntil),
			// Refreshes populated below by loadAllRefreshes.
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg village_objects LoadAll iter: %w", err)
	}

	if err := r.loadAllRefreshes(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// loadAllRefreshes reads every object_refresh row and attaches it to
// the corresponding parent in `objects` as an entry in its Refreshes
// slice. Rows whose parent is absent from the map are logged + skipped
// (FK CASCADE should make this impossible from valid writes).
func (r *VillageObjectsRepo) loadAllRefreshes(ctx context.Context, objects map[sim.VillageObjectID]*sim.VillageObject) error {
	rows, err := r.pool.Query(ctx, loadAllSQLOR)
	if err != nil {
		return fmt.Errorf("pg village_objects LoadAll refreshes query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			objectID       string
			attribute      *string // nullable (LLM-264): NULL on a yield-only row
			amount         int
			maxQty         *int
			availableQty   *int
			refreshMode    *string
			periodHours    *int
			lastRefreshAt  *time.Time
			dwellDelta     *int
			dwellPeriodMin *int
			gatherItem     *string
		)
		if err := rows.Scan(
			&objectID, &attribute, &amount,
			&maxQty, &availableQty,
			&refreshMode, &periodHours, &lastRefreshAt,
			&dwellDelta, &dwellPeriodMin, &gatherItem,
		); err != nil {
			return fmt.Errorf("pg village_objects LoadAll refreshes scan: %w", err)
		}

		// attribute is nullable (LLM-264): a yield-only row carries NULL, mapped to
		// the in-memory empty NeedKey.
		attr := ""
		if attribute != nil {
			attr = *attribute
		}

		parent, ok := objects[sim.VillageObjectID(objectID)]
		if !ok {
			// FK CASCADE makes this unreachable from valid writes; log
			// + skip surfaces schema drift loudly without dropping the
			// load. Don't fail LoadAll — the engine can come up with
			// the missing rows pruned.
			log.Printf("pg village_objects LoadAll: orphan refresh row object_id=%s attribute=%q (parent missing) — skipped",
				objectID, attr)
			continue
		}

		mode := ""
		if refreshMode != nil {
			mode = *refreshMode
		}
		gather := ""
		if gatherItem != nil {
			gather = *gatherItem
		}
		parent.Refreshes = append(parent.Refreshes, &sim.ObjectRefresh{
			Attribute:          sim.NeedKey(attr),
			Amount:             amount,
			GatherItem:         sim.ItemKind(gather),
			AvailableQuantity:  availableQty,
			MaxQuantity:        maxQty,
			RefreshMode:        sim.RefreshMode(mode),
			RefreshPeriodHours: periodHours,
			LastRefreshAt:      lastRefreshAt,
			DwellDelta:         dwellDelta,
			DwellPeriodMinutes: dwellPeriodMin,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("pg village_objects LoadAll refreshes iter: %w", err)
	}
	return nil
}

// SaveSnapshot writes the full VillageObject + Refreshes set durably
// using the generation-marker pattern. The supplied map IS the
// complete snapshot for both tables; any village_object row whose id
// is absent gets DELETEd, and any object_refresh row absent from the
// snapshot's per-parent Refreshes slices gets DELETEd.
//
// Steps inside the caller's checkpoint Tx (order matters — VO snapshot
// fully settles before refreshes are synced against the surviving
// parent set):
//
//  0. Advisory lock — shared by both tables (Refreshes is co-managed
//     under the same Tx; no separate lock for the child).
//  1. nextval(village_object_snapshot_gen_seq) → $genVO.
//  2. Per-row UPSERT village_object, stamping snapshot_gen = $genVO.
//  3. Safer DELETE village_object — FK CASCADE drops orphan refresh
//     rows for deleted parents.
//  4. Orphan check village_object — error + rollback on cross-tier
//     parent/child invariant violation.
//  5. nextval(object_refresh_snapshot_gen_seq) → $genOR.
//  6. Per-row UPSERT object_refresh, stamping snapshot_gen = $genOR.
//     Only iterates parents in the snapshot — refreshes for absent
//     parents were cascade-dropped in step 3.
//  7. DELETE object_refresh WHERE snapshot_gen < $genOR — prunes
//     refreshes where the parent survived but the refresh attribute
//     was removed from its slice.
//
// All steps share the Tx so the checkpoint is atomic — a crash
// mid-snapshot rolls back, leaving pg consistent with the prior
// checkpoint.
//
// Empty map: both gens are still bumped, no UPSERTs happen, both
// DELETEs remove every row in their respective tables (snapshot
// semantic: "this is everything; empty means nothing should be
// here"). Step 3 cascades to clear refreshes; step 7 is then a no-op
// against the empty table.
//
// nil object entries in the map are silently skipped (matches Slice 5
// OrdersRepo.SaveSnapshot pattern). nil refresh entries in an
// object's Refreshes slice are skipped at the upsert loop (matches
// CloneVillageObject's defensive posture).
func (r *VillageObjectsRepo) SaveSnapshot(ctx context.Context, tx sim.Tx, objects map[sim.VillageObjectID]*sim.VillageObject) error {
	if tx == nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: nil tx")
	}

	// Step 0: acquire the per-aggregate advisory lock. Held for the Tx
	// duration; serializes concurrent SaveSnapshot calls for this
	// aggregate. See advisoryLockSQLVO doc for rationale.
	if _, err := tx.Exec(ctx, advisoryLockSQLVO); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: advisory lock: %w", err)
	}

	// Step 1: bump the sequence for this checkpoint.
	var gen int64
	if err := tx.QueryRow(ctx, nextGenSQLVO).Scan(&gen); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: nextval: %w", err)
	}

	// Step 2: upsert each object, stamping the new gen. Order roots (no
	// attached_to) before overlays (attached_to set) so a single-level
	// overlay's parent is written first — a defensive eager pass. The
	// attached_to self-FK is DEFERRABLE INITIALLY DEFERRED (ZBBS-WORK-237),
	// so deeper attachment chains and any residual ordering still resolve at
	// commit; this pass just keeps the common case eager and intent clear.
	ordered := make([]*sim.VillageObject, 0, len(objects))
	for _, obj := range objects {
		if obj != nil && obj.AttachedTo == "" {
			ordered = append(ordered, obj)
		}
	}
	for _, obj := range objects {
		if obj != nil && obj.AttachedTo != "" {
			ordered = append(ordered, obj)
		}
	}
	for _, obj := range ordered {
		// owner_actor_id is nullable; convert empty ActorID to nil so pg
		// stores NULL (not the empty string — semantically "no owner",
		// matching the nullable column intent).
		var ownerArg any
		if obj.OwnerActorID != "" {
			ownerArg = string(obj.OwnerActorID)
		}
		var attachedArg any
		if obj.AttachedTo != "" {
			attachedArg = string(obj.AttachedTo)
		}
		// tags: nil → empty slice (pg column is NOT NULL DEFAULT '{}').
		tags := obj.Tags
		if tags == nil {
			tags = []string{}
		}
		// hearth_lit_until: zero time → NULL (never lit / not a hearth), so
		// the column stays NULL for the overwhelmingly common non-hearth row.
		var hearthArg any
		if !obj.HearthLitUntil.IsZero() {
			hearthArg = obj.HearthLitUntil
		}
		if _, err := tx.Exec(ctx, upsertSQLVO,
			string(obj.ID),          // $1 id (UUID)
			string(obj.AssetID),     // $2 asset_id (UUID)
			obj.CurrentState,        // $3 current_state
			obj.Pos.X,               // $4 x
			obj.Pos.Y,               // $5 y
			obj.PlacedBy,            // $6 placed_by
			obj.DisplayName,         // $7 display_name
			string(obj.EntryPolicy), // $8 entry_policy
			ownerArg,                // $9 owner_actor_id (nullable)
			attachedArg,             // $10 attached_to (nullable UUID)
			obj.LoiterOffsetX,       // $11 loiter_offset_x (nullable)
			obj.LoiterOffsetY,       // $12 loiter_offset_y (nullable)
			obj.AvailableQuantity,   // $13 available_quantity
			tags,                    // $14 tags (text[])
			obj.Wear,                // $15 wear
			hearthArg,               // $16 hearth_lit_until (nullable — NULL for a never-lit fire)
			obj.RateOwed,            // $17 rate_owed (LLM-557 town-rate arrears)
			gen,                     // $18 snapshot_gen
			obj.EquipmentUse,        // $19 equipment_use (LLM-648 deep-maintenance demand)
		); err != nil {
			return fmt.Errorf("pg village_objects SaveSnapshot: upsert id=%s: %w", obj.ID, err)
		}
	}

	// Step 3: prune absent rows. The safer DELETE keeps stale parents
	// alive when they still have fresh children attached, so the FK
	// CASCADE never destroys fresh data.
	if _, err := tx.Exec(ctx, deleteStaleSQLVO, gen); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: delete stale: %w", err)
	}

	// Step 4: orphan check. Any fresh child referencing a stale parent
	// means the world-side parent/child-same-gen invariant got broken
	// (the safer DELETE preserved the stale parent so the FK didn't
	// CASCADE-destroy the child, but the in-memory snapshot is internally
	// inconsistent). Error + rollback so the violation surfaces.
	var orphanCount int64
	if err := tx.QueryRow(ctx, orphanCheckSQLVO, gen).Scan(&orphanCount); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: orphan check: %w", err)
	}
	if orphanCount > 0 {
		return fmt.Errorf("pg village_objects SaveSnapshot: world invariant violation — %d fresh children reference stale parents (cross-tier snapshot; world goroutine bug)", orphanCount)
	}

	// Step 5: bump the object_refresh sequence — independent gen tier
	// from the parent. Shares the same Tx + advisory lock.
	var refreshGen int64
	if err := tx.QueryRow(ctx, nextGenSQLOR).Scan(&refreshGen); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: nextval refresh: %w", err)
	}

	// Step 6: upsert every refresh on every snapshot parent. Refreshes
	// for absent parents were already FK-CASCADE-dropped at step 3, so
	// iterating only `objects` is correct (no missing parents to
	// reference). nil refresh entries are skipped.
	for _, obj := range objects {
		if obj == nil {
			continue
		}
		for _, ref := range obj.Refreshes {
			if ref == nil {
				continue
			}
			// refresh_mode is NOT NULL DEFAULT 'continuous' in prod
			// (ZBBS-090). The finite/infinite discriminant is
			// available_quantity IS NULL — not the mode — so an infinite
			// row's mode is irrelevant, but the column still can't be NULL.
			// In-memory infinite rows carry RefreshMode == ""; write the
			// 'continuous' default for them so the NOT NULL holds. Finite
			// rows ride through as their set mode ('continuous'|'periodic')
			// for the mode_check CHECK to validate.
			modeArg := string(ref.RefreshMode)
			if modeArg == "" {
				modeArg = string(sim.RefreshModeContinuous)
			}
			// gather_item: "" (not gatherable) → NULL so the common
			// consume-in-place row carries no value. ZBBS-WORK-328.
			var gatherArg any
			if ref.GatherItem != "" {
				gatherArg = string(ref.GatherItem)
			}
			// attribute: "" (a yield-only row eases no need — LLM-264) → NULL so the
			// nullable column carries no placeholder; a need-bearing row writes its need.
			var attrArg any
			if ref.Attribute != "" {
				attrArg = string(ref.Attribute)
			}
			// Pick the conflict target by row type (LLM-264): a yield-only row (empty
			// attribute) upserts on its (object_id, gather_item) natural key, a
			// need-bearing row on (object_id, attribute). Both update in place.
			upsertQ := upsertNeedSQLOR
			if ref.Attribute == "" {
				upsertQ = upsertYieldSQLOR
			}
			if _, err := tx.Exec(ctx, upsertQ,
				string(obj.ID),         // $1 object_id (UUID)
				attrArg,                // $2 attribute (nullable — LLM-264)
				ref.Amount,             // $3 amount
				ref.MaxQuantity,        // $4 max_quantity (nullable)
				ref.AvailableQuantity,  // $5 available_quantity (nullable)
				modeArg,                // $6 refresh_mode (nullable)
				ref.RefreshPeriodHours, // $7 refresh_period_hours (nullable)
				ref.LastRefreshAt,      // $8 last_refresh_at (nullable)
				ref.DwellDelta,         // $9 dwell_amount (nullable; prod col name)
				ref.DwellPeriodMinutes, // $10 dwell_period_minutes (nullable)
				gatherArg,              // $11 gather_item (nullable)
				refreshGen,             // $12 snapshot_gen
			); err != nil {
				return fmt.Errorf("pg village_objects SaveSnapshot: upsert refresh oid=%s attr=%s: %w",
					obj.ID, ref.Attribute, err)
			}
		}
	}

	// Step 7: prune absent refresh rows. Catches the "parent survived
	// but refresh attribute dropped" case. No CASCADE concerns here —
	// object_refresh has no children.
	if _, err := tx.Exec(ctx, deleteStaleSQLOR, refreshGen); err != nil {
		return fmt.Errorf("pg village_objects SaveSnapshot: delete stale refresh: %w", err)
	}
	return nil
}
