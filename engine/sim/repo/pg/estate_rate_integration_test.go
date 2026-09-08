package pg

import (
	"context"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// estate_rate_integration_test.go — real-pg coverage for the two durable edges of
// the estate rate (LLM-652). Run against embedded Postgres with the prod baseline +
// post-baseline migrations applied (so the LLM-652 migration itself is under
// test); skipped under `go test -short`.

// The chest is the one piece of state the levy adds that MUST survive a restart:
// the coin has left the purses, so a chest that reloaded as zero would have
// destroyed it. Proves the column the migration adds, the upsert that writes it and
// the scan that reads it back agree, through the real checkpoint and load paths.
func TestIntegration_EstateRate_TownChestSurvivesCheckpoint(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	repo := NewRepository(f.Pool)

	w := checkpointableWorld(repo)
	w.Environment.TownChest = 69
	if err := SaveWorld(ctx, repo, w.BuildCheckpointSnapshot()); err != nil {
		t.Fatalf("SaveWorld: %v", err)
	}

	loaded, err := LoadWorld(ctx, repo, true /*requireAllImpl*/)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	if loaded.Environment.TownChest != 69 {
		t.Errorf("TownChest = %d after checkpoint + load, want 69", loaded.Environment.TownChest)
	}

	// A second checkpoint UPDATES the singleton rather than inserting beside it —
	// the chest must follow the ON CONFLICT arm too.
	loaded.Environment.TownChest = 107
	if err := SaveWorld(ctx, repo, loaded.BuildCheckpointSnapshot()); err != nil {
		t.Fatalf("SaveWorld (second): %v", err)
	}
	var chest int
	if err := f.Pool.QueryRow(ctx, `SELECT town_chest_coins FROM world_state WHERE id = 1`).Scan(&chest); err != nil {
		t.Fatalf("read world_state.town_chest_coins: %v", err)
	}
	if chest != 107 {
		t.Errorf("world_state.town_chest_coins = %d after the second checkpoint, want 107", chest)
	}
}

// An estate-rate row is a `paid` row with result ok and a payer, which is exactly
// the shape the coin-record seed selects — only the estate_rate marker keeps it
// out. The chest is not an actor, so a row that got through could never resolve to
// a pair; it would only be miscounted as a departed visitor at boot.
func TestCoinRecordsRepo_Integration_ExcludesEstateRateRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	const (
		payerID = "44444444-4444-4444-4444-444444444444"
		otherID = "55555555-5555-5555-5555-555555555555"
	)
	for _, a := range []struct{ id, name string }{
		{payerID, "Joseph Scott"},
		{otherID, "Moses James"},
	} {
		if _, err := f.Pool.Exec(ctx,
			`INSERT INTO actor (id, display_name, current_x, current_y) VALUES ($1, $2, 0, 0)`,
			a.id, a.name,
		); err != nil {
			t.Fatalf("seed actor %s: %v", a.name, err)
		}
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(at time.Time, source, payload string) {
		t.Helper()
		if _, err := f.Pool.Exec(ctx,
			`INSERT INTO agent_action_log (actor_id, occurred_at, source, action_type, payload, result, speaker_name)
			 VALUES ($1, $2, $3, 'paid', $4::jsonb, 'ok', 'Joseph Scott')`,
			payerID, at, source, payload,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// The exact payload assessEstateRate writes.
	insert(base.Add(-2*time.Hour), "engine",
		`{"recipient": "the town", "amount": 38, "for": "the rate on your estate", "estate_rate": true, "chest_after": 38}`)
	// An ordinary purchase from the same payer, which must still come back.
	insert(base.Add(-time.Hour), "agent",
		`{"recipient": "Moses James", "recipient_actor_id": "`+otherID+`", "amount": 4, "ledger_id": 8123}`)

	rows, err := NewCoinRecordsRepo(f.Pool).LoadPaymentsSince(ctx, base.Add(-3*time.Hour))
	if err != nil {
		t.Fatalf("LoadPaymentsSince: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d row(s), want 1 (the purchase only): %+v", len(rows), rows)
	}
	if rows[0].Amount != "4" || rows[0].CounterpartyActorID != otherID {
		t.Errorf("surviving row = %+v, want the 4-coin purchase from Moses", rows[0])
	}
	_ = sim.ActorID("") // keep the sim import honest if the assertions above change shape
}
