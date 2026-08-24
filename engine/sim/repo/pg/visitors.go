package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// VisitorsRepo reads and writes the visitor table — the durable mirror of the
// in-flight transient-visitor set (LLM-369). Visitors are firewalled from the
// 11-tier actor aggregate (ActorsRepo.SaveSnapshot skips every actor with
// VisitorState != nil), so this lean tier carries them separately so a traveler
// survives an engine restart instead of vanishing mid-scene.
//
// SaveSnapshot uses the single-table generation-marker pattern (same shape as
// labor_contract, LLM-259): advisory lock → nextval(gen) → per-row UPSERT
// stamping snapshot_gen → DELETE WHERE snapshot_gen < gen. A visitor that left
// the live set between checkpoints (departed + cleaned up) is absent from the
// map, so the trailing delete sweeps its row — the table stays a true mirror of
// the live in-flight set, crash-consistent with the rest of the checkpoint
// because it writes inside the same SaveWorld Tx.
//
// inside_structure_id is a soft TEXT ref to structure(id) with NO FK — the v2
// cross-aggregate posture (integrity revalidated Go-side at rehydrate), same as
// the actor aggregate's structure refs.
type VisitorsRepo struct {
	pool Pool
}

// NewVisitorsRepo constructs a VisitorsRepo against the given pool. Normal wiring
// path is pg.NewRepository which wires this internally.
func NewVisitorsRepo(pool Pool) *VisitorsRepo {
	return &VisitorsRepo{pool: pool}
}

// loadAllSQLV selects every visitor row. snapshot_gen omitted — pure sync
// bookkeeping with no in-memory representation. No ORDER BY — the in-memory
// model is map-keyed.
const loadAllSQLV = `
SELECT actor_id, display_name, archetype, origin, disposition,
       position_x, position_y, inside_structure_id, expires_at, phase, payload,
       recurring_visitor_id, plan
  FROM visitor`

// upsertSQLV writes one visitor row. snapshot_gen carries the new checkpoint gen
// so the trailing DELETE can prune stale rows.
const upsertSQLV = `
INSERT INTO visitor (
    actor_id, display_name, archetype, origin, disposition,
    position_x, position_y, inside_structure_id, expires_at, phase, payload,
    recurring_visitor_id, plan, snapshot_gen
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14
)
ON CONFLICT (actor_id) DO UPDATE SET
    display_name         = EXCLUDED.display_name,
    archetype            = EXCLUDED.archetype,
    origin               = EXCLUDED.origin,
    disposition          = EXCLUDED.disposition,
    position_x           = EXCLUDED.position_x,
    position_y           = EXCLUDED.position_y,
    inside_structure_id  = EXCLUDED.inside_structure_id,
    expires_at           = EXCLUDED.expires_at,
    phase                = EXCLUDED.phase,
    payload              = EXCLUDED.payload,
    recurring_visitor_id = EXCLUDED.recurring_visitor_id,
    plan                 = EXCLUDED.plan,
    snapshot_gen         = EXCLUDED.snapshot_gen`

// deleteStaleSQLV prunes visitor rows whose snapshot_gen is below the current
// checkpoint gen — the visitors absent from this snapshot (departed + cleaned
// up between checkpoints). Plain DELETE — no self-FK.
const deleteStaleSQLV = `DELETE FROM visitor WHERE snapshot_gen < $1`

// nextGenSQLV bumps the aggregate's gen sequence.
const nextGenSQLV = `SELECT nextval('visitor_snapshot_gen_seq')`

// advisoryLockSQLV is the single global lock for the visitor aggregate, held for
// the Tx duration to serialize concurrent SaveSnapshot calls. Multi-realm upgrade
// path: replace 0 with hashtext($realm_id) when realms land.
const advisoryLockSQLV = `SELECT pg_advisory_xact_lock(hashtext('visitor_snapshot'), 0)`

// visitorPlanJSON is the serialized shape of the visitor.plan jsonb column — the
// traveler's mutable day-plan (LLM-373). It gathers state that spans the visit and
// must survive the constant deploys, drawn from two live sources at checkpoint:
// the itinerary from VisitorState, and the pack / purse / booked-room grant from
// the live Actor (the same actor the tier already reads Pos and DisplayName off).
// None of it is reconcile-critical — the boot reconcile keys on the typed
// expires_at + phase columns — so it rides as one document rather than a spray of
// typed columns, the way labor_contract.reward_items does.
type visitorPlanJSON struct {
	// Rounds — VisitorState.VisitedBusinesses (the keeper-businesses he has called at).
	VisitedBusinesses []string `json:"visited_businesses,omitempty"`
	// The people the traveler has passed his carried word to (LLM-545) —
	// VisitorState.PayloadSharedWith. Persisted for the same reason the payload
	// column itself is: a mid-visit deploy must not resurrect the word as unspent,
	// or the traveler reopens a matter his listener already answered.
	PayloadSharedWith []string `json:"payload_shared_with,omitempty"`
	// Trade — the merchant visitor's bound errand (LLM-455), generalizing the LLM-410
	// DistributorOnly flag. nil for a passer-through. Rides the plan jsonb (no dedicated
	// column) so a mid-visit redeploy resumes the traveler on the same errand rather than as
	// a plain peddler.
	Trade *tradeErrandJSON `json:"trade,omitempty"`
	// Pack + purse — Actor.Inventory / Actor.Coins. The barter wares and coins the
	// traveler pays for its room and trades on its circuit with.
	Inventory map[string]int `json:"inventory,omitempty"`
	Coins     int            `json:"coins,omitempty"`
	// Remaining trip spend budget — VisitorState.SpendBudget (LLM-644). A
	// pointer so an exhausted budget (0) still round-trips as an explicit
	// value: absent means the row predates the field, and applyVisitorPlan
	// rehydrates that visitor with budget = wallet (the old uncapped
	// behavior, for that one in-flight visit).
	SpendBudget *int `json:"spend_budget,omitempty"`
	// Booked room(s) — Actor.RoomAccess. A visitor is firewalled out of the actor
	// aggregate that writes room_access, so its lodging grant is NOT in that table;
	// it rides here so the room stays booked across a restart. Crash-consistent with
	// the sale: the completed nights_stay pay_ledger row is itself written only at the
	// SaveWorld checkpoint (by Orders.SaveSnapshot), the SAME Tx this visitor tier
	// writes in — so a crash before the checkpoint loses the grant AND the sale
	// together (the traveler simply wasn't booked yet), never one without the other.
	RoomAccess []visitorGrantJSON `json:"room_access,omitempty"`
}

// tradeErrandJSON is the on-disk shape of a merchant visitor's TradeErrand (LLM-455) inside
// the plan jsonb. Strings, not the typed enums, so the document stays decode-tolerant of a
// Go-side rename.
type tradeErrandJSON struct {
	Direction    string `json:"direction"`
	Good         string `json:"good"`
	Counterparty string `json:"counterparty"`
	Settled      bool   `json:"settled,omitempty"`
	// ShipmentQty is the seller's arrival quantity of Good; Delivered is how much of it has
	// reached the errand COUNTERPARTY business specifically — not a general count of what
	// left his pack, since a transfer elsewhere (a bystander, another keeper, an ungated
	// `give`) credits nothing. Together they are the baseline and running total the
	// sell-side settle compares (LLM-553); see TradeErrand.Delivered for the full scope
	// rule before reusing either.
	//
	// Both omitempty, so a buyer's errand (which leaves them 0) keeps the document it had
	// before, and a row written before these fields existed decodes to 0/0 — which
	// sellErrandDelivered reads as "no baseline", leaving that visitor to wind down at dusk
	// as it did before.
	ShipmentQty int `json:"shipment_qty,omitempty"`
	Delivered   int `json:"delivered,omitempty"`
}

// visitorGrantJSON is the on-disk element shape for a persisted RoomAccess grant.
type visitorGrantJSON struct {
	RoomID    int64      `json:"room_id"`
	Source    string     `json:"source"`
	LedgerID  int64      `json:"ledger_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"created_at"`
}

// encodeVisitorPlan marshals the live day-plan of one visitor to the plan jsonb
// text. The itinerary comes off VisitorState; the pack / purse / grant come off
// the live Actor. Bound as text and cast to ::jsonb in the UPSERT (pgx encodes a
// Go string as text), matching encodeRewardItems. SaveSnapshot only calls this for
// a VisitorState != nil actor; the nil guard hardens the helper against a future
// caller.
func encodeVisitorPlan(a *sim.Actor) (string, error) {
	if a == nil || a.VisitorState == nil {
		return "{}", nil
	}
	vs := a.VisitorState
	spendBudget := vs.SpendBudget
	plan := visitorPlanJSON{
		Coins:       a.Coins,
		SpendBudget: &spendBudget,
	}
	if vs.Trade != nil {
		plan.Trade = &tradeErrandJSON{
			Direction:    string(vs.Trade.Direction),
			Good:         string(vs.Trade.Good),
			Counterparty: string(vs.Trade.Counterparty),
			Settled:      vs.Trade.Settled,
			ShipmentQty:  vs.Trade.ShipmentQty,
			Delivered:    vs.Trade.Delivered,
		}
	}
	for _, sid := range vs.VisitedBusinesses {
		plan.VisitedBusinesses = append(plan.VisitedBusinesses, string(sid))
	}
	for _, aid := range vs.PayloadSharedWith {
		plan.PayloadSharedWith = append(plan.PayloadSharedWith, string(aid))
	}
	if len(a.Inventory) > 0 {
		plan.Inventory = make(map[string]int, len(a.Inventory))
		for kind, qty := range a.Inventory {
			plan.Inventory[string(kind)] = qty
		}
	}
	for _, ra := range a.RoomAccess {
		if ra == nil {
			continue
		}
		plan.RoomAccess = append(plan.RoomAccess, visitorGrantJSON{
			RoomID:    int64(ra.RoomID),
			Source:    string(ra.Source),
			LedgerID:  ra.LedgerID,
			ExpiresAt: ra.ExpiresAt,
			Active:    ra.Active,
			CreatedAt: ra.CreatedAt,
		})
	}
	b, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// applyVisitorPlan parses the plan jsonb back onto a LoadedVisitor: the itinerary
// onto its VisitorState, and the pack / purse / booked-room grant onto the fields
// rehydrateVisitorsOnLoad rebuilds the live Actor from. Empty/absent bytes leave
// the visitor at its zero plan (a freshly-spawned row before its first checkpoint,
// or one written by an engine that predates the column).
func applyVisitorPlan(raw []byte, lv *sim.LoadedVisitor) error {
	if len(raw) == 0 {
		return nil
	}
	var plan visitorPlanJSON
	if err := json.Unmarshal(raw, &plan); err != nil {
		return err
	}
	for _, s := range plan.VisitedBusinesses {
		lv.VisitorState.VisitedBusinesses = append(lv.VisitorState.VisitedBusinesses, sim.StructureID(s))
	}
	// Dedup + drop empties + truncate at the cap on the way in (code_review):
	// the engine-side writer (recordRoadWordShared) already keeps the set
	// unique and under sim.MaxPayloadSharedWith, so a duplicate, blank, or
	// oversized array here is out-of-band plan data — the same trust posture the
	// Trade validation above takes. Truncate rather than reject: the rest of the
	// plan (pack, purse, booked room) is independently valid, and a partial
	// memory only risks a re-told rumour, not an inconsistency. An unknown actor
	// id stays harmless downstream (perception intersects against the live
	// snapshot before rendering).
	seenShared := make(map[string]bool, len(plan.PayloadSharedWith))
	for _, s := range plan.PayloadSharedWith {
		if s == "" || seenShared[s] {
			continue
		}
		if len(lv.VisitorState.PayloadSharedWith) >= sim.MaxPayloadSharedWith {
			break
		}
		seenShared[s] = true
		lv.VisitorState.PayloadSharedWith = append(lv.VisitorState.PayloadSharedWith, sim.ActorID(s))
	}
	if plan.Trade != nil {
		// Validate the persisted errand against the Go-owned allowlist before rebuilding it
		// (code_review): a corrupt/stale plan (only possible from an out-of-band edit — a
		// consistent checkpoint writes a valid errand) with an unknown direction or a missing
		// good/counterparty is DROPPED, so the traveler rehydrates as a passer-through rather
		// than a merchant with an errand the rest of the engine reads inconsistently.
		dir := sim.TradeDirection(plan.Trade.Direction)
		if dir.Valid() && plan.Trade.Good != "" && plan.Trade.Counterparty != "" {
			// Negative quantities are nonsense the settle arithmetic should never see
			// (only reachable from an out-of-band edit); clamp both to 0, which reads as
			// "no baseline" and leaves the traveler to the dusk wind-down rather than
			// settling him on arrival.
			shipmentQty := plan.Trade.ShipmentQty
			if shipmentQty < 0 {
				shipmentQty = 0
			}
			delivered := plan.Trade.Delivered
			if delivered < 0 {
				delivered = 0
			}
			lv.VisitorState.Trade = &sim.TradeErrand{
				Direction:    dir,
				Good:         sim.ItemKind(plan.Trade.Good),
				Counterparty: sim.StructureID(plan.Trade.Counterparty),
				Settled:      plan.Trade.Settled,
				ShipmentQty:  shipmentQty,
				Delivered:    delivered,
			}
		}
	}
	lv.Coins = plan.Coins
	// Spend budget (LLM-644). Absent = a row written before the field
	// existed: rehydrate with budget = wallet, the old uncapped behavior,
	// so one in-flight legacy visit finishes as it began rather than being
	// frozen out of buying. A negative value is out-of-band nonsense the
	// draw arithmetic should never see (same posture as the Trade clamps
	// above); clamp to 0.
	budget := plan.Coins
	if plan.SpendBudget != nil {
		budget = *plan.SpendBudget
	}
	if budget < 0 {
		budget = 0
	}
	lv.VisitorState.SpendBudget = budget
	if len(plan.Inventory) > 0 {
		lv.Inventory = make(map[sim.ItemKind]int, len(plan.Inventory))
		for kind, qty := range plan.Inventory {
			lv.Inventory[sim.ItemKind(kind)] = qty
		}
	}
	for _, g := range plan.RoomAccess {
		if lv.RoomAccess == nil {
			lv.RoomAccess = make(map[sim.RoomAccessKey]*sim.RoomAccess)
		}
		key := sim.RoomAccessKey{RoomID: sim.RoomID(g.RoomID), Source: sim.RoomAccessSource(g.Source)}
		lv.RoomAccess[key] = &sim.RoomAccess{
			RoomID:    sim.RoomID(g.RoomID),
			Source:    sim.RoomAccessSource(g.Source),
			LedgerID:  g.LedgerID,
			ExpiresAt: g.ExpiresAt,
			Active:    g.Active,
			CreatedAt: g.CreatedAt,
		}
	}
	return nil
}

// LoadAll loads every visitor row into a map of the reload-DTO the rehydrate pass
// (World.rehydrateVisitorsOnLoad) rebuilds a live Actor from. Runs against the
// pool directly (no Tx) — read-only restart path, same posture as the other
// LoadAll implementations.
func (r *VisitorsRepo) LoadAll(ctx context.Context) (map[sim.ActorID]*sim.LoadedVisitor, error) {
	rows, err := r.pool.Query(ctx, loadAllSQLV)
	if err != nil {
		return nil, fmt.Errorf("pg visitors LoadAll query: %w", err)
	}
	defer rows.Close()

	out := make(map[sim.ActorID]*sim.LoadedVisitor)
	for rows.Next() {
		var (
			actorID     string
			displayName string
			archetype   string
			origin      string
			disposition string
			posX        int
			posY        int
			insideID    *string
			expiresAt   time.Time
			phase       string
			payload     string
			recurringID *string
			plan        []byte
		)
		if err := rows.Scan(&actorID, &displayName, &archetype, &origin, &disposition,
			&posX, &posY, &insideID, &expiresAt, &phase, &payload, &recurringID, &plan); err != nil {
			return nil, fmt.Errorf("pg visitors LoadAll scan: %w", err)
		}
		var inside sim.StructureID
		if insideID != nil {
			inside = sim.StructureID(*insideID)
		}
		var recurring string
		if recurringID != nil {
			recurring = *recurringID
		}
		lv := &sim.LoadedVisitor{
			ID:                sim.ActorID(actorID),
			DisplayName:       displayName,
			Pos:               sim.TilePos{X: posX, Y: posY},
			InsideStructureID: inside,
			VisitorState: &sim.VisitorState{
				Archetype:   archetype,
				Origin:      origin,
				Disposition: disposition,
				ExpiresAt:   expiresAt,
				Phase:       sim.VisitorPhase(phase),
				Payload:     payload,
				RecurringID: recurring,
			},
		}
		if err := applyVisitorPlan(plan, lv); err != nil {
			return nil, fmt.Errorf("pg visitors LoadAll: parse plan for %s: %w", actorID, err)
		}
		out[sim.ActorID(actorID)] = lv
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg visitors LoadAll iter: %w", err)
	}
	return out, nil
}

// SaveSnapshot writes the in-flight visitor set durably via the generation-marker
// pattern, inside the caller's checkpoint Tx. It is the COMPLEMENT of
// ActorsRepo.SaveSnapshot: it is handed the same cp.Actors map and persists
// exactly the actors that aggregate skips (VisitorState != nil), so the two
// partition the actor set with no overlap.
//
//  0. Advisory lock.
//  1. nextval(visitor_snapshot_gen_seq) → $gen.
//  2. Per-row UPSERT of the visitor subset, stamping snapshot_gen = $gen.
//     Substrate-boundary validation: reject map-key ↔ a.ID mismatch, empty
//     DisplayName, empty phase (Go owns the phase allowlist; a spawn/cascade bug
//     that left it unset is worth surfacing on the failing checkpoint).
//  3. DELETE visitor WHERE snapshot_gen < $gen — sweep the visitors absent from
//     the snapshot (departed + cleaned up since the last checkpoint).
//
// Empty / visitor-less actors map: the gen still bumps, no UPSERTs run, the
// DELETE sweeps the whole table. nil entries are skipped (ActorsRepo.SaveSnapshot
// is the one that errors on a nil actor entry; here a nil is simply not a
// visitor).
func (r *VisitorsRepo) SaveSnapshot(ctx context.Context, tx sim.Tx, actors map[sim.ActorID]*sim.Actor) error {
	if tx == nil {
		return fmt.Errorf("pg visitors SaveSnapshot: nil tx")
	}

	if _, err := tx.Exec(ctx, advisoryLockSQLV); err != nil {
		return fmt.Errorf("pg visitors SaveSnapshot: advisory lock: %w", err)
	}

	var gen int64
	if err := tx.QueryRow(ctx, nextGenSQLV).Scan(&gen); err != nil {
		return fmt.Errorf("pg visitors SaveSnapshot: nextval: %w", err)
	}

	for key, a := range actors {
		if a == nil || a.VisitorState == nil {
			continue // not a visitor — the actor aggregate owns (or rejects) it
		}
		if a.ID != key {
			return fmt.Errorf("pg visitors SaveSnapshot: map key=%s does not match a.ID=%s", key, a.ID)
		}
		if strings.TrimSpace(a.DisplayName) == "" {
			return fmt.Errorf("pg visitors SaveSnapshot: id=%s has empty DisplayName", a.ID)
		}
		vs := a.VisitorState
		if !vs.Phase.Valid() {
			return fmt.Errorf("pg visitors SaveSnapshot: id=%s has invalid visitor phase %q (Go owns the allowlist)", a.ID, vs.Phase)
		}
		// inside_structure_id: bind "" as SQL NULL so the column round-trips
		// outdoors-or-inside cleanly (matches the visitor_inside_structure_id_nonempty
		// CHECK, which allows NULL but not '').
		var insideArg any
		if a.InsideStructureID != "" {
			insideArg = string(a.InsideStructureID)
		}
		// recurring_visitor_id: bind "" as SQL NULL — a not-yet-promoted stranger
		// has no returner identity, and the format CHECK allows NULL but not ''.
		var recurringArg any
		if vs.RecurringID != "" {
			recurringArg = vs.RecurringID
		}
		// The day-plan document (LLM-373): itinerary off VisitorState, pack / purse /
		// booked-room grant off the live Actor. Bound as text and cast ::jsonb.
		planJSON, err := encodeVisitorPlan(a)
		if err != nil {
			return fmt.Errorf("pg visitors SaveSnapshot: encode plan id=%s: %w", a.ID, err)
		}
		if _, err := tx.Exec(ctx, upsertSQLV,
			string(a.ID),     // $1  actor_id
			a.DisplayName,    // $2  display_name
			vs.Archetype,     // $3  archetype
			vs.Origin,        // $4  origin
			vs.Disposition,   // $5  disposition
			a.Pos.X,          // $6  position_x
			a.Pos.Y,          // $7  position_y
			insideArg,        // $8  inside_structure_id (nullable)
			vs.ExpiresAt,     // $9  expires_at
			string(vs.Phase), // $10 phase
			vs.Payload,       // $11 payload
			recurringArg,     // $12 recurring_visitor_id (nullable)
			planJSON,         // $13 plan (::jsonb)
			gen,              // $14 snapshot_gen
		); err != nil {
			return fmt.Errorf("pg visitors SaveSnapshot: upsert id=%s: %w", a.ID, err)
		}
	}

	if _, err := tx.Exec(ctx, deleteStaleSQLV, gen); err != nil {
		return fmt.Errorf("pg visitors SaveSnapshot: delete stale: %w", err)
	}
	return nil
}
