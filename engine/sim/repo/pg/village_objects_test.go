package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// pgxmock-based tests for VillageObjectsRepo. Asserts SQL shape + arg
// bindings + scan mapping. Real-pg smoke lives at cutover (testcontainers
// slice).
//
// Test IDs use syntactically valid UUID strings so test fixtures match
// the production schema's UUID column types (id, asset_id, attached_to)
// — pgxmock doesn't enforce the ::uuid cast, but real Postgres would.
// Code_review R1 flag.

// Predictable UUID literals — different leading bytes make failures
// easier to read in test output than v4 random UUIDs.
const (
	uuidObj1    = "11111111-1111-1111-1111-111111111111"
	uuidObj2    = "22222222-2222-2222-2222-222222222222"
	uuidObj3    = "33333333-3333-3333-3333-333333333333"
	uuidOverlay = "00000000-0000-0000-0000-000000000099"

	uuidAssetWell  = "a0000000-0000-0000-0000-000000000001"
	uuidAssetBench = "a0000000-0000-0000-0000-000000000002"
	uuidAssetLamp  = "a0000000-0000-0000-0000-000000000003"
)

// sp returns a pointer to s. LoadAll scans asset_id / placed_by /
// display_name into *string (they're nullable in the prod baseline), so
// mock rows must supply *string for those columns.
func sp(s string) *string { return &s }

func newMockPoolVO(t *testing.T) (pgxmock.PgxPoolIface, *VillageObjectsRepo) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, NewVillageObjectsRepo(mock)
}

// expectSaveSnapshotPrelude programs the common nextval + advisory lock
// expectations on the mock. The lock is matched as Exec (the SELECT
// returns void and the production code uses Exec); nextval as Query.
// MatchExpectationsInOrder must be set by the caller — these two are
// only ordered relative to each other (lock before nextval).
//
// The nextval pattern is anchored on the parent's sequence name so it
// doesn't collide with the refresh-side nextval expectation added by
// expectSaveSnapshotRefreshTail.
func expectSaveSnapshotPrelude(mock pgxmock.PgxPoolIface, gen int64) {
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT nextval\('village_object_snapshot_gen_seq`).
		WillReturnRows(pgxmock.NewRows([]string{"nextval"}).AddRow(gen))
}

// expectSaveSnapshotOrphanCheck programs the orphan-check expectation
// (count = 0 means no invariant violation; happy path).
func expectSaveSnapshotOrphanCheck(mock pgxmock.PgxPoolIface, gen int64, count int64) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM village_object fresh`).
		WithArgs(gen).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(count))
}

// expectSaveSnapshotRefreshTail programs the refresh-side tail of
// SaveSnapshot: the second nextval (on object_refresh's sequence) and
// the delete-stale that prunes absent refresh rows. Per-test UPSERT
// expectations on object_refresh are added separately for objects
// that carry Refreshes.
func expectSaveSnapshotRefreshTail(mock pgxmock.PgxPoolIface, refreshGen int64) {
	mock.ExpectQuery(`SELECT nextval\('object_refresh_snapshot_gen_seq`).
		WillReturnRows(pgxmock.NewRows([]string{"nextval"}).AddRow(refreshGen))
	mock.ExpectExec(`DELETE FROM object_refresh WHERE snapshot_gen < \$1`).
		WithArgs(refreshGen).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
}

// emptyRefreshRows returns a no-row pgxmock row set for the
// object_refresh query in LoadAll. Used by LoadAll tests whose
// fixtures don't exercise refresh data — keeps the second query
// satisfied without growing every test by a fixture builder.
func emptyRefreshRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"object_id", "attribute", "amount",
		"max_quantity", "available_quantity",
		"refresh_mode", "refresh_period_hours", "last_refresh_at",
		"dwell_delta", "dwell_period_minutes", "gather_item",
	})
}

// --- LoadAll happy path ---------------------------------------------------

// TestVillageObjectsRepo_LoadAll_HappyPath — covers the full column set
// including the various nullable combinations: owner_actor_id present
// vs NULL, attached_to NULL (top-level placement) vs present (overlay),
// loiter offsets present vs NULL.
func TestVillageObjectsRepo_LoadAll_HappyPath(t *testing.T) {
	mock, repo := newMockPoolVO(t)

	loiterX := 3
	loiterY := -2
	ownerStr := "alice"
	parentRef := uuidObj1

	rows := pgxmock.NewRows([]string{
		"id", "asset_id", "current_state", "x", "y", "placed_by",
		"display_name", "entry_policy", "owner_actor_id", "attached_to",
		"loiter_offset_x", "loiter_offset_y", "available_quantity", "tags", "wear",
		"hearth_lit_until", "rate_owed", "equipment_use",
	}).
		// Top-level placement, owned, with loiter offsets, tags, worn, and two days
		// of town rate outstanding (LLM-557).
		AddRow(uuidObj1, sp(uuidAssetWell), "default", 640.0, 320.0, sp("admin"),
			sp("Old Well"), "closed", &ownerStr, (*string)(nil),
			&loiterX, &loiterY, 10, []string{"vendor", "well"}, 120, (*time.Time)(nil), 2, 45).
		// Top-level placement, unowned, no loiter, empty tags.
		AddRow(uuidObj2, sp(uuidAssetBench), "variant-1", 1000.0, 500.0, sp(""),
			sp(""), "open", (*string)(nil), (*string)(nil),
			(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0).
		// Overlay attached to obj-1, owner-only, no tags.
		AddRow(uuidObj3, sp(uuidAssetLamp), "lit", 645.0, 325.0, sp("admin"),
			sp("Lamp"), "owner-only", &ownerStr, &parentRef,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0)

	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).WillReturnRows(rows)
	mock.ExpectQuery(`SELECT[\s\S]+FROM object_refresh`).WillReturnRows(emptyRefreshRows())

	got, err := repo.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("loaded %d objects, want 3", len(got))
	}

	o1 := got[sim.VillageObjectID(uuidObj1)]
	if o1 == nil {
		t.Fatal("obj-1 missing")
	}
	if o1.AssetID != sim.AssetID(uuidAssetWell) {
		t.Errorf("o1.AssetID = %q, want %q", o1.AssetID, uuidAssetWell)
	}
	if o1.EntryPolicy != sim.EntryPolicyClosed {
		t.Errorf("o1.EntryPolicy = %q, want closed", o1.EntryPolicy)
	}
	if o1.OwnerActorID != "alice" {
		t.Errorf("o1.OwnerActorID = %q, want alice", o1.OwnerActorID)
	}
	if o1.AttachedTo != "" {
		t.Errorf("o1.AttachedTo = %q, want empty (top-level)", o1.AttachedTo)
	}
	if o1.LoiterOffsetX == nil || *o1.LoiterOffsetX != 3 {
		t.Errorf("o1.LoiterOffsetX = %v, want 3", o1.LoiterOffsetX)
	}
	if o1.AvailableQuantity != 10 {
		t.Errorf("o1.AvailableQuantity = %d, want 10", o1.AvailableQuantity)
	}
	if len(o1.Tags) != 2 {
		t.Errorf("o1.Tags = %v, want 2 entries", o1.Tags)
	}
	if o1.Wear != 120 {
		t.Errorf("o1.Wear = %d, want 120", o1.Wear)
	}
	// LLM-557: town-rate arrears round-trip off the row rather than defaulting to 0.
	if o1.RateOwed != 2 {
		t.Errorf("o1.RateOwed = %d, want 2", o1.RateOwed)
	}
	// LLM-648: equipment-use demand round-trips off the row too.
	if o1.EquipmentUse != 45 {
		t.Errorf("o1.EquipmentUse = %d, want 45", o1.EquipmentUse)
	}
	if o2 := got[sim.VillageObjectID(uuidObj2)]; o2 != nil && o2.RateOwed != 0 {
		t.Errorf("o2.RateOwed = %d, want 0", o2.RateOwed)
	}
	if o1.Refreshes != nil {
		t.Errorf("o1.Refreshes = %v, want nil (this test's fixture has no refresh rows)", o1.Refreshes)
	}

	o2 := got[sim.VillageObjectID(uuidObj2)]
	if o2.OwnerActorID != "" {
		t.Errorf("o2.OwnerActorID = %q, want empty (NULL → empty ActorID)", o2.OwnerActorID)
	}
	if o2.LoiterOffsetX != nil {
		t.Errorf("o2.LoiterOffsetX = %v, want nil (NULL → nil)", o2.LoiterOffsetX)
	}
	if o2.EntryPolicy != sim.EntryPolicyOpen {
		t.Errorf("o2.EntryPolicy = %q, want open", o2.EntryPolicy)
	}
	// Empty Postgres array '{}' scanned through pgx may produce empty
	// slice or nil depending on the type path. LoadAll normalizes to
	// empty slice so callers don't have to nil-check (Slice 9 R1 fix).
	if o2.Tags == nil {
		t.Errorf("o2.Tags = nil, want empty slice (normalize-nil-tags)")
	}

	o3 := got[sim.VillageObjectID(uuidObj3)]
	if string(o3.AttachedTo) != uuidObj1 {
		t.Errorf("o3.AttachedTo = %q, want %q", o3.AttachedTo, uuidObj1)
	}
	if o3.EntryPolicy != sim.EntryPolicyOwner {
		t.Errorf("o3.EntryPolicy = %q, want owner-only", o3.EntryPolicy)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestVillageObjectsRepo_LoadAll_Empty(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	rows := pgxmock.NewRows([]string{
		"id", "asset_id", "current_state", "x", "y", "placed_by",
		"display_name", "entry_policy", "owner_actor_id", "attached_to",
		"loiter_offset_x", "loiter_offset_y", "available_quantity", "tags", "wear",
		"hearth_lit_until", "rate_owed", "equipment_use",
	})
	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).WillReturnRows(rows)
	mock.ExpectQuery(`SELECT[\s\S]+FROM object_refresh`).WillReturnRows(emptyRefreshRows())

	got, err := repo.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty result returned %d objects", len(got))
	}
}

func TestVillageObjectsRepo_LoadAll_QueryError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).
		WillReturnError(errors.New("conn closed"))
	_, err := repo.LoadAll(context.Background())
	if err == nil {
		t.Fatal("expected error from query failure")
	}
}

// --- SaveSnapshot ---------------------------------------------------------

// TestVillageObjectsRepo_SaveSnapshot_HappyPath — full lifecycle:
// advisory lock → nextval → per-row UPSERTs → safer DELETE → orphan
// check returns 0.
func TestVillageObjectsRepo_SaveSnapshot_HappyPath(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 5)

	// Two upserts (order varies due to map iteration).
	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "default", 640.0, 320.0, "admin",
			"Old Well", "closed", "alice", nil,
			(*int)(nil), (*int)(nil), 0, []string{"vendor"}, 120, nil, 0, int64(5), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj2, uuidAssetBench, "variant-1", 1000.0, 500.0, "", "",
			"open", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(5), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	// Safer DELETE — protects fresh children whose parents are stale.
	mock.ExpectExec(`DELETE FROM village_object stale[\s\S]+WHERE stale.snapshot_gen < \$1`).
		WithArgs(int64(5)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	expectSaveSnapshotOrphanCheck(mock, 5, 0)

	// Refresh tail — neither object carries Refreshes, so just nextval +
	// delete-stale on the child table.
	expectSaveSnapshotRefreshTail(mock, 50)

	// Relaxed only because the two parent UPSERTs run in map-iteration
	// order (undefined). The lock/nextval/delete/orphan-check/refresh-tail
	// calls have distinct SQL patterns so they can't cross-match with the
	// UPSERTs; tests with deterministic parent ordering (EmptyMap,
	// NilObjectSkipped, OwnerNullVsValue, WithRefreshes) keep the
	// default in-order matching and cover refresh-tail-after-orphan-check
	// ordering.
	mock.MatchExpectationsInOrder(false)

	objects := map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {
			ID:           sim.VillageObjectID(uuidObj1),
			AssetID:      sim.AssetID(uuidAssetWell),
			CurrentState: "default",
			Pos:          sim.WorldPos{X: 640, Y: 320},
			PlacedBy:     "admin",
			DisplayName:  "Old Well",
			EntryPolicy:  sim.EntryPolicyClosed,
			OwnerActorID: "alice",
			Tags:         []string{"vendor"},
			Wear:         120,
		},
		sim.VillageObjectID(uuidObj2): {
			ID:           sim.VillageObjectID(uuidObj2),
			AssetID:      sim.AssetID(uuidAssetBench),
			CurrentState: "variant-1",
			Pos:          sim.WorldPos{X: 1000, Y: 500},
			EntryPolicy:  sim.EntryPolicyOpen,
			// nil tags → empty slice; nil owner → SQL NULL.
		},
	}

	if err := repo.SaveSnapshot(context.Background(), tx, objects); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_SaveSnapshot_EmptyMap — lock + gen still
// happen; safer DELETE runs (removing every row since none has the new
// gen); orphan check returns 0.
func TestVillageObjectsRepo_SaveSnapshot_EmptyMap(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 7)
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(7)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 7, 0)
	expectSaveSnapshotRefreshTail(mock, 70)

	if err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_SaveSnapshot_NilTx — substrate-boundary nil
// check. Symmetric with OrdersRepo's nil-tx guard.
func TestVillageObjectsRepo_SaveSnapshot_NilTx(t *testing.T) {
	_, repo := newMockPoolVO(t)
	err := repo.SaveSnapshot(context.Background(), nil, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {ID: sim.VillageObjectID(uuidObj1), AssetID: sim.AssetID(uuidAssetWell)},
	})
	if err == nil {
		t.Fatal("expected error for nil tx")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_NilObjectSkipped — nil entries are
// silently skipped; full lifecycle (lock + gen + delete + orphan check)
// still runs.
func TestVillageObjectsRepo_SaveSnapshot_NilObjectSkipped(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 1)
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(1)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 1, 0)
	expectSaveSnapshotRefreshTail(mock, 10)

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): nil,
	})
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_SaveSnapshot_AdvisoryLockError — lock acquisition
// fails (e.g., connection issue). Surfaces as substrate error; nextval +
// upserts + delete + orphan check don't run.
func TestVillageObjectsRepo_SaveSnapshot_AdvisoryLockError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WillReturnError(errors.New("connection lost"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {ID: sim.VillageObjectID(uuidObj1), AssetID: sim.AssetID(uuidAssetWell)},
	})
	if err == nil {
		t.Fatal("expected error from advisory lock failure")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_NextvalError — sequence call
// fails. Surfaces as substrate error; upsert + delete don't run.
func TestVillageObjectsRepo_SaveSnapshot_NextvalError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WillReturnResult(pgconn.NewCommandTag("SELECT 1"))
	mock.ExpectQuery(`SELECT nextval`).WillReturnError(errors.New("sequence unavailable"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {ID: sim.VillageObjectID(uuidObj1), AssetID: sim.AssetID(uuidAssetWell)},
	})
	if err == nil {
		t.Fatal("expected error from nextval failure")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_UpsertError — upsert fails after
// gen bump. Delete-stale doesn't run (caller's Tx rolls back).
func TestVillageObjectsRepo_SaveSnapshot_UpsertError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 1)
	mock.ExpectExec(`INSERT INTO village_object`).
		WillReturnError(errors.New("CHECK constraint violation"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {ID: sim.VillageObjectID(uuidObj1), AssetID: sim.AssetID(uuidAssetWell), EntryPolicy: "bogus"},
	})
	if err == nil {
		t.Fatal("expected error from upsert failure")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_OrphanCheckViolation — orphan
// check returns >0, meaning a fresh child references a stale parent
// (world-side parent/child-same-gen invariant got broken). SaveSnapshot
// errors out so the violation surfaces loudly. Caller's Tx rolls back.
func TestVillageObjectsRepo_SaveSnapshot_OrphanCheckViolation(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 3)

	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "default", 0.0, 0.0, "", "",
			"open", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(3), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(3)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	// Orphan check returns 2 — two fresh children reference stale parents.
	expectSaveSnapshotOrphanCheck(mock, 3, 2)

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {
			ID:           sim.VillageObjectID(uuidObj1),
			AssetID:      sim.AssetID(uuidAssetWell),
			CurrentState: "default",
			EntryPolicy:  sim.EntryPolicyOpen,
		},
	})
	if err == nil {
		t.Fatal("expected error from orphan-check violation")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_OwnerNullVsValue — verifies
// nullable owner_actor_id binding: empty ActorID → SQL NULL, non-empty
// → string value. Same for attached_to.
func TestVillageObjectsRepo_SaveSnapshot_OwnerNullVsValue(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 2)

	// Owned overlay (both owner_actor_id + attached_to non-NULL).
	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidOverlay, uuidAssetLamp, "lit", 100.0, 100.0, "",
			"", "open", "alice", uuidObj1,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(2), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(2)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	expectSaveSnapshotOrphanCheck(mock, 2, 0)
	expectSaveSnapshotRefreshTail(mock, 20)

	objects := map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidOverlay): {
			ID: sim.VillageObjectID(uuidOverlay), AssetID: sim.AssetID(uuidAssetLamp), CurrentState: "lit",
			Pos:          sim.WorldPos{X: 100, Y: 100},
			EntryPolicy:  sim.EntryPolicyOpen,
			OwnerActorID: "alice",
			AttachedTo:   sim.VillageObjectID(uuidObj1),
		},
	}
	if err := repo.SaveSnapshot(context.Background(), tx, objects); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// --- LoadAll with refreshes -----------------------------------------------

// TestVillageObjectsRepo_LoadAll_WithRefreshes covers the full refresh
// column matrix: finite-supply continuous regen, finite-supply periodic
// regen, infinite-supply (no regen), dwell config.
func TestVillageObjectsRepo_LoadAll_WithRefreshes(t *testing.T) {
	mock, repo := newMockPoolVO(t)

	parentRows := pgxmock.NewRows([]string{
		"id", "asset_id", "current_state", "x", "y", "placed_by",
		"display_name", "entry_policy", "owner_actor_id", "attached_to",
		"loiter_offset_x", "loiter_offset_y", "available_quantity", "tags", "wear",
		"hearth_lit_until", "rate_owed", "equipment_use",
	}).
		AddRow(uuidObj1, sp(uuidAssetWell), "default", 640.0, 320.0, sp(""),
			sp("Well"), "open", (*string)(nil), (*string)(nil),
			(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0).
		AddRow(uuidObj2, sp(uuidAssetBench), "default", 0.0, 0.0, sp(""),
			sp("Shaded Oak"), "open", (*string)(nil), (*string)(nil),
			(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0)
	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).WillReturnRows(parentRows)

	// obj1: well with finite-supply continuous regen on thirst.
	// obj2: shaded oak with two refresh rows — finite-supply periodic
	// on hunger (acorns) plus infinite (shade) tiredness with dwell.
	max10, avail7 := 10, 7
	max20, avail0 := 20, 0
	period12, period24 := 12, 24
	dwellDelta, dwellPeriod := -1, 15
	wellAnchor := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC)
	oakAnchor := time.Date(2026, 5, 17, 6, 0, 0, 0, time.UTC)
	continuousMode := "continuous"
	periodicMode := "periodic"

	refreshRows := pgxmock.NewRows([]string{
		"object_id", "attribute", "amount",
		"max_quantity", "available_quantity",
		"refresh_mode", "refresh_period_hours", "last_refresh_at",
		"dwell_delta", "dwell_period_minutes", "gather_item",
	}).
		// Well: thirst -8, 7/10 finite, continuous, no dwell, harvestable (water).
		AddRow(uuidObj1, sp("thirst"), -8,
			&max10, &avail7,
			&continuousMode, &period12, &wellAnchor,
			(*int)(nil), (*int)(nil), sp("water")).
		// Oak (row 1): hunger -3, 0/20 finite, periodic, no dwell, not harvestable.
		AddRow(uuidObj2, sp("hunger"), -3,
			&max20, &avail0,
			&periodicMode, &period24, &oakAnchor,
			(*int)(nil), (*int)(nil), (*string)(nil)).
		// Oak (row 2): tiredness -1, infinite supply, dwell enabled. prod's
		// refresh_mode is NOT NULL DEFAULT 'continuous', so an infinite row
		// carries 'continuous' even though mode is irrelevant when
		// available_quantity IS NULL.
		AddRow(uuidObj2, sp("tiredness"), -1,
			(*int)(nil), (*int)(nil),
			&continuousMode, (*int)(nil), (*time.Time)(nil),
			&dwellDelta, &dwellPeriod, (*string)(nil))
	mock.ExpectQuery(`SELECT[\s\S]+FROM object_refresh`).WillReturnRows(refreshRows)

	got, err := repo.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	well := got[sim.VillageObjectID(uuidObj1)]
	if well == nil {
		t.Fatal("well missing")
	}
	if len(well.Refreshes) != 1 {
		t.Fatalf("well.Refreshes len=%d, want 1", len(well.Refreshes))
	}
	wr := well.Refreshes[0]
	if wr.Attribute != "thirst" {
		t.Errorf("well refresh Attribute=%q, want thirst", wr.Attribute)
	}
	if wr.Amount != -8 {
		t.Errorf("well refresh Amount=%d, want -8", wr.Amount)
	}
	if !wr.IsFinite() {
		t.Error("well refresh should be finite")
	}
	if wr.RefreshMode != sim.RefreshModeContinuous {
		t.Errorf("well refresh mode=%q, want continuous", wr.RefreshMode)
	}
	if wr.LastRefreshAt == nil || !wr.LastRefreshAt.Equal(wellAnchor) {
		t.Errorf("well refresh LastRefreshAt=%v, want %v", wr.LastRefreshAt, wellAnchor)
	}
	if wr.HasDwell() {
		t.Error("well refresh should not have dwell")
	}
	if !wr.IsGatherable() || wr.GatherItem != "water" {
		t.Errorf("well refresh GatherItem=%q (gatherable=%v), want water/true", wr.GatherItem, wr.IsGatherable())
	}

	oak := got[sim.VillageObjectID(uuidObj2)]
	if oak == nil {
		t.Fatal("oak missing")
	}
	if len(oak.Refreshes) != 2 {
		t.Fatalf("oak.Refreshes len=%d, want 2", len(oak.Refreshes))
	}
	// Refreshes ordered by query result; first row is hunger.
	hunger := oak.Refreshes[0]
	if hunger.Attribute != "hunger" || !hunger.IsFinite() ||
		hunger.RefreshMode != sim.RefreshModePeriodic {
		t.Errorf("oak hunger refresh = %+v", hunger)
	}
	tiredness := oak.Refreshes[1]
	// IsFinite() (available_quantity IS NULL) is the real infinite
	// discriminant — not the mode.
	if tiredness.Attribute != "tiredness" || tiredness.IsFinite() {
		t.Errorf("oak tiredness refresh should be infinite: %+v", tiredness)
	}
	if !tiredness.HasDwell() {
		t.Error("oak tiredness refresh should have dwell")
	}
	if tiredness.RefreshMode != sim.RefreshModeContinuous {
		t.Errorf("oak tiredness RefreshMode=%q, want continuous (prod NOT NULL default)", tiredness.RefreshMode)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_LoadAll_RefreshOrphanSkipped — a refresh row
// whose object_id isn't in the parent set is logged + skipped; LoadAll
// still succeeds for the well-formed parents. FK CASCADE makes this
// unreachable from valid writes, but the guard surfaces schema drift
// loudly rather than silently dropping the load.
func TestVillageObjectsRepo_LoadAll_RefreshOrphanSkipped(t *testing.T) {
	mock, repo := newMockPoolVO(t)

	parentRows := pgxmock.NewRows([]string{
		"id", "asset_id", "current_state", "x", "y", "placed_by",
		"display_name", "entry_policy", "owner_actor_id", "attached_to",
		"loiter_offset_x", "loiter_offset_y", "available_quantity", "tags", "wear",
		"hearth_lit_until", "rate_owed", "equipment_use",
	}).AddRow(uuidObj1, sp(uuidAssetWell), "default", 0.0, 0.0, sp(""),
		sp("Well"), "open", (*string)(nil), (*string)(nil),
		(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0)
	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).WillReturnRows(parentRows)

	// Two refresh rows: one for the real parent, one orphan.
	refreshRows := pgxmock.NewRows([]string{
		"object_id", "attribute", "amount",
		"max_quantity", "available_quantity",
		"refresh_mode", "refresh_period_hours", "last_refresh_at",
		"dwell_delta", "dwell_period_minutes", "gather_item",
	}).
		AddRow(uuidObj1, sp("thirst"), -4,
			(*int)(nil), (*int)(nil),
			(*string)(nil), (*int)(nil), (*time.Time)(nil),
							(*int)(nil), (*int)(nil), (*string)(nil)).
		AddRow(uuidObj3, sp("hunger"), -3, // uuidObj3 isn't in parent set
			(*int)(nil), (*int)(nil),
			(*string)(nil), (*int)(nil), (*time.Time)(nil),
			(*int)(nil), (*int)(nil), (*string)(nil))
	mock.ExpectQuery(`SELECT[\s\S]+FROM object_refresh`).WillReturnRows(refreshRows)

	got, err := repo.LoadAll(context.Background())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d parents, want 1", len(got))
	}
	well := got[sim.VillageObjectID(uuidObj1)]
	if len(well.Refreshes) != 1 {
		t.Errorf("well.Refreshes len=%d, want 1 (orphan skipped)", len(well.Refreshes))
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_LoadAll_RefreshQueryError — the refresh query
// itself fails (connection issue, etc.). LoadAll surfaces the error
// rather than returning partial data.
func TestVillageObjectsRepo_LoadAll_RefreshQueryError(t *testing.T) {
	mock, repo := newMockPoolVO(t)

	parentRows := pgxmock.NewRows([]string{
		"id", "asset_id", "current_state", "x", "y", "placed_by",
		"display_name", "entry_policy", "owner_actor_id", "attached_to",
		"loiter_offset_x", "loiter_offset_y", "available_quantity", "tags", "wear",
		"hearth_lit_until", "rate_owed", "equipment_use",
	}).AddRow(uuidObj1, sp(uuidAssetWell), "default", 0.0, 0.0, sp(""),
		sp(""), "open", (*string)(nil), (*string)(nil),
		(*int)(nil), (*int)(nil), 0, []string{}, 0, (*time.Time)(nil), 0, 0)
	mock.ExpectQuery(`SELECT[\s\S]+FROM village_object`).WillReturnRows(parentRows)
	mock.ExpectQuery(`SELECT[\s\S]+FROM object_refresh`).
		WillReturnError(errors.New("conn closed"))

	_, err := repo.LoadAll(context.Background())
	if err == nil {
		t.Fatal("expected error from refresh query failure")
	}
}

// --- SaveSnapshot with refreshes ------------------------------------------

// TestVillageObjectsRepo_SaveSnapshot_WithRefreshes covers the full
// refresh-column write matrix: finite continuous (with last_refresh_at
// non-NULL), infinite (supply/regen NULL, mode defaulted to 'continuous'),
// dwell-only on an infinite row, and a mix of refreshes on one parent.
func TestVillageObjectsRepo_SaveSnapshot_WithRefreshes(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 9)

	// Parent UPSERT — minimal fields, refresh-bearing.
	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "default", 0.0, 0.0, "", "",
			"open", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(9), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(9)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	expectSaveSnapshotOrphanCheck(mock, 9, 0)

	// Refresh nextval. Two upserts then delete-stale.
	mock.ExpectQuery(`SELECT nextval\('object_refresh_snapshot_gen_seq`).
		WillReturnRows(pgxmock.NewRows([]string{"nextval"}).AddRow(int64(90)))

	max10, avail3 := 10, 3
	period12 := 12
	anchor := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC)
	dwellDelta, dwellPeriod := -1, 15

	// Refresh 1: finite + continuous + no dwell. Args carry pointers
	// matching the production binding (pgx accepts nil *int / *string /
	// *time.Time as SQL NULL; non-nil dereferences are passed through
	// to the driver).
	mock.ExpectExec(`INSERT INTO object_refresh`).
		WithArgs(
			uuidObj1, "thirst", -8,
			&max10, &avail3,
			"continuous", &period12, &anchor,
			(*int)(nil), (*int)(nil),
			"water", // gather_item — well is harvestable
			int64(90),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	// Refresh 2: infinite + dwell enabled. refresh_mode is irrelevant for
	// infinite rows (discriminant is available_quantity IS NULL), but prod's
	// column is NOT NULL DEFAULT 'continuous' — SaveSnapshot writes the
	// 'continuous' default rather than NULL.
	mock.ExpectExec(`INSERT INTO object_refresh`).
		WithArgs(
			uuidObj1, "tiredness", -1,
			(*int)(nil), (*int)(nil),
			"continuous", (*int)(nil), (*time.Time)(nil),
			&dwellDelta, &dwellPeriod,
			nil, // gather_item — not harvestable → NULL
			int64(90),
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	mock.ExpectExec(`DELETE FROM object_refresh WHERE snapshot_gen < \$1`).
		WithArgs(int64(90)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))

	// Refreshes on one parent — order in the slice is preserved by the
	// production code's per-parent iteration, so expectations stay
	// in-order without MatchExpectationsInOrder relaxation.
	objects := map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {
			ID:           sim.VillageObjectID(uuidObj1),
			AssetID:      sim.AssetID(uuidAssetWell),
			CurrentState: "default",
			EntryPolicy:  sim.EntryPolicyOpen,
			Refreshes: []*sim.ObjectRefresh{
				{
					Attribute:          "thirst",
					Amount:             -8,
					MaxQuantity:        &max10,
					AvailableQuantity:  &avail3,
					RefreshMode:        sim.RefreshModeContinuous,
					RefreshPeriodHours: &period12,
					LastRefreshAt:      &anchor,
					GatherItem:         "water", // well is harvestable (ZBBS-WORK-328)
				},
				{
					Attribute:          "tiredness",
					Amount:             -1,
					DwellDelta:         &dwellDelta,
					DwellPeriodMinutes: &dwellPeriod,
				},
			},
		},
	}

	if err := repo.SaveSnapshot(context.Background(), tx, objects); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_SaveSnapshot_NilRefreshSkipped — a nil entry in
// a parent's Refreshes slice is silently skipped. Matches the
// defensive posture of CloneVillageObject.
func TestVillageObjectsRepo_SaveSnapshot_NilRefreshSkipped(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 4)

	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "", 0.0, 0.0, "", "",
			"open", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(4), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(4)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 4, 0)

	// Refresh tail with no upserts — the only Refreshes entries are
	// nil and get skipped.
	expectSaveSnapshotRefreshTail(mock, 40)

	objects := map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {
			ID:          sim.VillageObjectID(uuidObj1),
			AssetID:     sim.AssetID(uuidAssetWell),
			EntryPolicy: sim.EntryPolicyOpen,
			Refreshes:   []*sim.ObjectRefresh{nil, nil},
		},
	}
	if err := repo.SaveSnapshot(context.Background(), tx, objects); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestVillageObjectsRepo_SaveSnapshot_RefreshNextvalError — the
// refresh-side nextval call fails after VO is fully written. Surfaces
// as substrate error; refresh upserts + delete-stale don't run.
func TestVillageObjectsRepo_SaveSnapshot_RefreshNextvalError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 1)
	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "", 0.0, 0.0, "", "",
			"", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(1), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(1)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 1, 0)

	mock.ExpectQuery(`SELECT nextval\('object_refresh_snapshot_gen_seq`).
		WillReturnError(errors.New("sequence unavailable"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {ID: sim.VillageObjectID(uuidObj1), AssetID: sim.AssetID(uuidAssetWell)},
	})
	if err == nil {
		t.Fatal("expected error from refresh nextval failure")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_RefreshUpsertError — a refresh
// upsert fails (e.g., CHECK constraint violation). Surfaces as
// substrate error; delete-stale doesn't run.
func TestVillageObjectsRepo_SaveSnapshot_RefreshUpsertError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 1)
	mock.ExpectExec(`INSERT INTO village_object`).
		WithArgs(
			uuidObj1, uuidAssetWell, "", 0.0, 0.0, "", "",
			"open", nil, nil,
			(*int)(nil), (*int)(nil), 0, []string{}, 0, nil, 0, int64(1), 0,
		).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(1)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 1, 0)

	mock.ExpectQuery(`SELECT nextval\('object_refresh_snapshot_gen_seq`).
		WillReturnRows(pgxmock.NewRows([]string{"nextval"}).AddRow(int64(10)))
	mock.ExpectExec(`INSERT INTO object_refresh`).
		WillReturnError(errors.New("CHECK constraint violation"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(uuidObj1): {
			ID:          sim.VillageObjectID(uuidObj1),
			AssetID:     sim.AssetID(uuidAssetWell),
			EntryPolicy: sim.EntryPolicyOpen,
			Refreshes: []*sim.ObjectRefresh{
				// Misconfigured: finite without mode would fail the
				// finite_regen CHECK in real pg; mock simulates the
				// error directly.
				{Attribute: "thirst", Amount: -1},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error from refresh upsert failure")
	}
}

// TestVillageObjectsRepo_SaveSnapshot_DeleteStaleRefreshError — final
// delete-stale step on the child table fails. SaveSnapshot returns the
// error so the caller's Tx rolls back.
func TestVillageObjectsRepo_SaveSnapshot_DeleteStaleRefreshError(t *testing.T) {
	mock, repo := newMockPoolVO(t)
	tx := fakeTx{mock: mock}

	expectSaveSnapshotPrelude(mock, 1)
	mock.ExpectExec(`DELETE FROM village_object stale`).
		WithArgs(int64(1)).
		WillReturnResult(pgconn.NewCommandTag("DELETE 0"))
	expectSaveSnapshotOrphanCheck(mock, 1, 0)

	mock.ExpectQuery(`SELECT nextval\('object_refresh_snapshot_gen_seq`).
		WillReturnRows(pgxmock.NewRows([]string{"nextval"}).AddRow(int64(10)))
	mock.ExpectExec(`DELETE FROM object_refresh WHERE snapshot_gen < \$1`).
		WithArgs(int64(10)).
		WillReturnError(errors.New("disk full"))

	err := repo.SaveSnapshot(context.Background(), tx, map[sim.VillageObjectID]*sim.VillageObject{})
	if err == nil {
		t.Fatal("expected error from refresh delete-stale failure")
	}
}
