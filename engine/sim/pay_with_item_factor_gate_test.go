package sim_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// pay_with_item_factor_gate_test.go — LLM-410 wholesale factor. A factor's TRADE goods
// move only between him and the village distributor, both ways: the engine backstop
// rejects a non-distributor buying his cloth (factor = seller) AND the factor buying a
// trade good from anyone but the distributor (factor = buyer), while EXEMPTING his own
// self-provisioning (a service or a consumable — a room or a meal) so he still rides the
// ordinary lodging/eating lifecycle. The gate (TradeErrandSteer, LLM-455) keys on the errand's
// counterparty STRUCTURE, not the item; the item only matters for the buy-side self-provisioning exemption.
//
// Helpers (buildPayWithItemWorld, pwiActor, mustSend) live in
// pay_with_item_commands_test.go — same sim_test package.

// tagFactorWorld seeds a factor (Elias, a sell-errand visitor holding a coat to sell)
// plus the distributor (Josiah, keeper of the distributor-tagged General Store, holding a
// wheat surplus) and a plain innkeeper (Hannah, non-distributor, holding wheat + bread),
// all co-present in one huddle. A "coat" clothing kind is added to the catalog so it
// resolves. WorkStructureID + tags + the visitor flag are set on the world goroutine.
func tagFactorWorld(t *testing.T) (*sim.World, func(), time.Time) {
	t.Helper()
	w, stop := buildPayWithItemWorld(t, "h1", "sc1", []pwiActor{
		{id: "elias", displayName: "Elias Drum the factor", kind: sim.KindNPCShared, huddleID: "h1", coins: 200, inventory: map[sim.ItemKind]int{"coat": 4}},
		{id: "josiah", displayName: "Josiah Thorne", kind: sim.KindNPCStateful, huddleID: "h1", coins: 60, inventory: map[sim.ItemKind]int{"wheat": 30}},
		{id: "hannah", displayName: "Hannah Boggs", kind: sim.KindNPCShared, huddleID: "h1", coins: 60, inventory: map[sim.ItemKind]int{"wheat": 10, "bread": 10}},
	})
	mustSend(t, w, func(world *sim.World) {
		if world.VillageObjects == nil {
			world.VillageObjects = map[sim.VillageObjectID]*sim.VillageObject{}
		}
		// The imported clothing kind the factor deals in (slice 2's catalog; a lean stand-in
		// here). Non-consumable, non-service — a trade good the gate governs.
		world.ItemKinds["coat"] = &sim.ItemKindDef{
			Name: "coat", DisplayLabel: "a coat", Category: "clothing", Capabilities: []string{sim.CapabilityWarms},
		}
		world.Actors["josiah"].WorkStructureID = "general_store"
		world.Actors["hannah"].WorkStructureID = "the_inn"
		world.VillageObjects["general_store"] = &sim.VillageObject{
			ID: "general_store", OwnerActorID: "josiah", Tags: []string{sim.TagDistributor},
		}
		// Elias is a wholesale factor — the SELL instance of a merchant errand whose counterparty
		// is the distributor's General Store (LLM-455).
		world.Actors["elias"].VisitorState = &sim.VisitorState{
			Archetype: sim.FactorArchetype, Origin: sim.FactorOrigin,
			Phase: sim.VisitorPhaseMakingRounds,
			Trade: &sim.TradeErrand{Direction: sim.TradeDirectionSell, Good: "coat", Counterparty: "general_store"},
			// LLM-644: budget = purse, as at spawn.
			SpendBudget: 200,
		}
	})
	return w, stop, time.Now().UTC()
}

func TestPayWithItem_FactorGate(t *testing.T) {
	t.Run("distributor_buys_factor_cloth_allowed", func(t *testing.T) {
		w, stop, at := tagFactorWorld(t)
		defer stop()
		// Josiah (the distributor) buys the factor's coat — the sell leg of the two-way trade.
		res, err := w.Send(sim.PayWithItem("josiah", "Elias Drum the factor", "coat", 1, 12, false, nil, nil, 0, 0, "", at))
		if err != nil {
			t.Fatalf("the distributor buying the factor's cloth should pass the gate: %v", err)
		}
		if res.(sim.PayWithItemResult).State != sim.PayLedgerStatePending {
			t.Errorf("state = %q, want pending (mints normally)", res.(sim.PayWithItemResult).State)
		}
	})

	t.Run("non_distributor_buys_factor_cloth_rejected", func(t *testing.T) {
		w, stop, at := tagFactorWorld(t)
		defer stop()
		// Hannah (an innkeeper, not the distributor) tries to buy the factor's coat — rejected:
		// his wares go only to the distributor.
		_, err := w.Send(sim.PayWithItem("hannah", "Elias Drum the factor", "coat", 1, 12, false, nil, nil, 0, 0, "", at))
		if err == nil || !strings.Contains(err.Error(), "deals only with") {
			t.Fatalf("non-distributor buying the factor's cloth err = %v, want factor-gate steer", err)
		}
		// LLM-292 copy constraint: the steer speaks in-world, never the mechanic word.
		if strings.Contains(strings.ToLower(err.Error()), "distributor") {
			t.Errorf("steer %q carries the mechanic-role term \"distributor\" (LLM-292)", err.Error())
		}
		if !strings.Contains(err.Error(), "Josiah Thorne") {
			t.Errorf("steer %q should name the distributor (Josiah Thorne)", err.Error())
		}
	})

	t.Run("factor_buys_surplus_from_distributor_allowed", func(t *testing.T) {
		w, stop, at := tagFactorWorld(t)
		defer stop()
		// The factor buys the distributor's wheat surplus (non-consumable, so this exercises the
		// counterparty-is-distributor allow, not the self-provisioning exemption).
		res, err := w.Send(sim.PayWithItem("elias", "Josiah Thorne", "wheat", 1, 3, false, nil, nil, 0, 0, "", at))
		if err != nil {
			t.Fatalf("the factor buying surplus from the distributor should pass: %v", err)
		}
		if res.(sim.PayWithItemResult).State != sim.PayLedgerStatePending {
			t.Errorf("state = %q, want pending", res.(sim.PayWithItemResult).State)
		}
	})

	t.Run("factor_buys_trade_good_from_non_distributor_rejected", func(t *testing.T) {
		w, stop, at := tagFactorWorld(t)
		defer stop()
		// The factor tries to buy a trade good (wheat) from a non-distributor — rejected: he
		// sources his carry-home goods only from the distributor.
		_, err := w.Send(sim.PayWithItem("elias", "Hannah Boggs", "wheat", 1, 3, false, nil, nil, 0, 0, "", at))
		if err == nil || !strings.Contains(err.Error(), "deal only with") {
			t.Fatalf("factor buying a trade good from a non-distributor err = %v, want factor-gate steer", err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "distributor") {
			t.Errorf("steer %q carries the mechanic-role term \"distributor\" (LLM-292)", err.Error())
		}
	})

	t.Run("factor_buys_food_from_non_distributor_allowed", func(t *testing.T) {
		w, stop, at := tagFactorWorld(t)
		defer stop()
		// The factor buys a meal (bread — a consumable) from the innkeeper: self-provisioning is
		// exempt, so he still eats and lodges on his stay even though she isn't the distributor.
		res, err := w.Send(sim.PayWithItem("elias", "Hannah Boggs", "bread", 1, 2, false, nil, nil, 0, 0, "", at))
		if err != nil {
			t.Fatalf("the factor buying food for himself should be exempt from the gate: %v", err)
		}
		if res.(sim.PayWithItemResult).State != sim.PayLedgerStatePending {
			t.Errorf("state = %q, want pending", res.(sim.PayWithItemResult).State)
		}
	})
}
