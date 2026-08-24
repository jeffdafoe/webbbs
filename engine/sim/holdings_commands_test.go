package sim_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/repo/mem"
)

// holdings_commands_test.go — sim-level coverage of AdjustActorHoldings
// (ZBBS-WORK-330): signed coins + signed per-item deltas to/from any actor (PC
// or NPC), validate-all-then-apply, delete-on-zero, floor + overflow guards.

type holdingsActorSpec struct {
	id        sim.ActorID
	kind      sim.ActorKind
	coins     int
	inventory map[sim.ItemKind]int
}

func buildHoldingsTestWorld(t *testing.T, actors []holdingsActorSpec) (*sim.World, func()) {
	t.Helper()
	repo, handles := mem.NewRepository()
	handles.ItemKinds.Seed(mem.SeedItemKinds())

	seed := make(map[sim.ActorID]*sim.Actor, len(actors))
	for _, s := range actors {
		seed[s.id] = &sim.Actor{
			ID:          s.id,
			DisplayName: string(s.id),
			Kind:        s.kind,
			State:       sim.StateIdle,
			Coins:       s.coins,
			Inventory:   s.inventory,
		}
	}
	handles.Actors.Seed(seed)

	w, err := sim.LoadWorld(context.Background(), repo)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	return w, func() { cancel(); <-done }
}

// grant sends an AdjustActorHoldings command and returns the result + error.
func grant(t *testing.T, w *sim.World, id sim.ActorID, coins int, items []sim.ActorInventoryRow) (sim.ActorHoldingsResult, error) {
	t.Helper()
	res, err := w.Send(sim.AdjustActorHoldings(id, coins, items))
	if err != nil {
		return sim.ActorHoldingsResult{}, err
	}
	out, ok := res.(sim.ActorHoldingsResult)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	return out, nil
}

func TestAdjustHoldings_CoinsCreditAndDebit(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "npc", kind: sim.KindNPCShared, coins: 10}})
	defer stop()

	out, err := grant(t, w, "npc", 15, nil)
	if err != nil {
		t.Fatalf("credit: %v", err)
	}
	if out.Coins != 25 {
		t.Errorf("after +15: coins=%d, want 25", out.Coins)
	}

	out, err = grant(t, w, "npc", -20, nil)
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if out.Coins != 5 {
		t.Errorf("after -20: coins=%d, want 5", out.Coins)
	}
}

func TestAdjustHoldings_CoinsDebitFloor_Atomic(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{
		{id: "npc", kind: sim.KindNPCShared, coins: 5, inventory: map[sim.ItemKind]int{"bread": 2}},
	})
	defer stop()

	// Debit beyond the floor must reject the WHOLE call — including the item
	// give bundled with it — leaving coins AND inventory untouched.
	_, err := grant(t, w, "npc", -6, []sim.ActorInventoryRow{{ItemKind: "ale", Quantity: 3}})
	if !errors.Is(err, sim.ErrHoldingsUnderflow) {
		t.Fatalf("over-debit: err=%v, want ErrHoldingsUnderflow", err)
	}
	// Verify nothing changed.
	state := readHoldings(t, w, "npc")
	if state.Coins != 5 {
		t.Errorf("coins=%d after rejected over-debit, want 5 (atomic)", state.Coins)
	}
	if state.inv["bread"] != 2 || state.inv["ale"] != 0 {
		t.Errorf("inventory mutated after rejected call: %v", state.inv)
	}
}

func TestAdjustHoldings_ItemsAddRemoveAndDeleteOnZero(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{
		{id: "npc", kind: sim.KindNPCShared, inventory: map[sim.ItemKind]int{"bread": 5}},
	})
	defer stop()

	// Add ale, reduce bread.
	if _, err := grant(t, w, "npc", 0, []sim.ActorInventoryRow{
		{ItemKind: "ale", Quantity: 3},
		{ItemKind: "bread", Quantity: -2},
	}); err != nil {
		t.Fatalf("add/remove: %v", err)
	}
	state := readHoldings(t, w, "npc")
	if state.inv["ale"] != 3 || state.inv["bread"] != 3 {
		t.Errorf("after add ale+3 / bread-2: %v, want ale=3 bread=3", state.inv)
	}

	// Remove bread to exactly zero → delete-on-zero (entry gone, not a 0 row).
	if _, err := grant(t, w, "npc", 0, []sim.ActorInventoryRow{{ItemKind: "bread", Quantity: -3}}); err != nil {
		t.Fatalf("remove to zero: %v", err)
	}
	state = readHoldings(t, w, "npc")
	if _, present := state.inv["bread"]; present {
		t.Errorf("bread entry should be deleted at zero, got %v", state.inv)
	}
}

func TestAdjustHoldings_RemoveMoreThanHeld_Atomic(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{
		{id: "npc", kind: sim.KindNPCShared, coins: 10, inventory: map[sim.ItemKind]int{"bread": 2}},
	})
	defer stop()

	// Bundle a valid coin credit with an impossible item removal — the whole
	// call must reject, coins included.
	_, err := grant(t, w, "npc", 5, []sim.ActorInventoryRow{{ItemKind: "bread", Quantity: -3}})
	if !errors.Is(err, sim.ErrHoldingsUnderflow) {
		t.Fatalf("over-remove: err=%v, want ErrHoldingsUnderflow", err)
	}
	state := readHoldings(t, w, "npc")
	if state.Coins != 10 || state.inv["bread"] != 2 {
		t.Errorf("state mutated after rejected over-remove: coins=%d inv=%v", state.Coins, state.inv)
	}
}

func TestAdjustHoldings_UnknownItemKind(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "npc", kind: sim.KindNPCShared}})
	defer stop()

	_, err := grant(t, w, "npc", 0, []sim.ActorInventoryRow{{ItemKind: "dragon-egg", Quantity: 1}})
	if !errors.Is(err, sim.ErrUnknownItemKind) {
		t.Fatalf("unknown item: err=%v, want ErrUnknownItemKind", err)
	}
}

func TestAdjustHoldings_DuplicateKind(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "npc", kind: sim.KindNPCShared}})
	defer stop()

	// Case-insensitive resolution means "Bread" and "bread" are the same kind —
	// two deltas for it is ambiguous and rejected.
	_, err := grant(t, w, "npc", 0, []sim.ActorInventoryRow{
		{ItemKind: "bread", Quantity: 1},
		{ItemKind: "Bread", Quantity: 2},
	})
	if !errors.Is(err, sim.ErrInvalidInventory) {
		t.Fatalf("dup kind: err=%v, want ErrInvalidInventory", err)
	}
}

func TestAdjustHoldings_UnknownActor(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "npc", kind: sim.KindNPCShared}})
	defer stop()

	if _, err := grant(t, w, "ghost", 5, nil); !errors.Is(err, sim.ErrActorNotFound) {
		t.Fatalf("unknown actor: err=%v, want ErrActorNotFound", err)
	}
}

// TestAdjustHoldings_PCTarget is the capability SetActorInventory cannot do:
// give coins + items to a PC (editableNPC rejects PCs).
func TestAdjustHoldings_PCTarget(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "pc", kind: sim.KindPC, coins: 0}})
	defer stop()

	out, err := grant(t, w, "pc", 50, []sim.ActorInventoryRow{{ItemKind: "bread", Quantity: 4}})
	if err != nil {
		t.Fatalf("grant to PC: %v", err)
	}
	if out.Coins != 50 {
		t.Errorf("PC coins=%d, want 50", out.Coins)
	}
	state := readHoldings(t, w, "pc")
	if state.inv["bread"] != 4 {
		t.Errorf("PC bread=%d, want 4", state.inv["bread"])
	}
}

func TestAdjustHoldings_CombinedCoinsAndItems(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{
		{id: "npc", kind: sim.KindNPCShared, coins: 20, inventory: map[sim.ItemKind]int{"ale": 1}},
	})
	defer stop()

	out, err := grant(t, w, "npc", -5, []sim.ActorInventoryRow{
		{ItemKind: "ale", Quantity: 2},
		{ItemKind: "bread", Quantity: 1},
	})
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if out.Coins != 15 {
		t.Errorf("coins=%d, want 15", out.Coins)
	}
	state := readHoldings(t, w, "npc")
	if state.inv["ale"] != 3 || state.inv["bread"] != 1 {
		t.Errorf("inv=%v, want ale=3 bread=1", state.inv)
	}
}

func TestAdjustHoldings_OverflowGuard(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{
		{id: "npc", kind: sim.KindNPCShared, coins: 100},
	})
	defer stop()

	const maxInt = int(^uint(0) >> 1)
	if _, err := grant(t, w, "npc", maxInt, nil); !errors.Is(err, sim.ErrHoldingsOverflow) {
		t.Fatalf("coins overflow: err=%v, want ErrHoldingsOverflow", err)
	}
}

// --- read-back helper ---

type holdingsState struct {
	Coins int
	inv   map[sim.ItemKind]int
}

func readHoldings(t *testing.T, w *sim.World, id sim.ActorID) holdingsState {
	t.Helper()
	res, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		a := world.Actors[id]
		if a == nil {
			return holdingsState{inv: map[sim.ItemKind]int{}}, nil
		}
		inv := make(map[sim.ItemKind]int, len(a.Inventory))
		for k, v := range a.Inventory {
			inv[k] = v
		}
		return holdingsState{Coins: a.Coins, inv: inv}, nil
	}})
	if err != nil {
		t.Fatalf("readHoldings: %v", err)
	}
	return res.(holdingsState)
}

// TestAdjustHoldings_VisitorGrantRaisesBudget — the LLM-644 operator-fiat door:
// a positive coin grant to a visitor raises the trip spend budget alongside the
// wallet (fiat coin is meant to be spendable), a debit leaves the budget alone
// (spendable min()s against the smaller wallet), and the budget addition
// saturates at MaxInt instead of wrapping (code_review).
func TestAdjustHoldings_VisitorGrantRaisesBudget(t *testing.T) {
	w, stop := buildHoldingsTestWorld(t, []holdingsActorSpec{{id: "vstr-1", kind: sim.KindNPCShared, coins: 10}})
	defer stop()
	seedBudget := func(b int) {
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			world.Actors["vstr-1"].VisitorState = &sim.VisitorState{SpendBudget: b}
			return nil, nil
		}}); err != nil {
			t.Fatalf("seed visitor state: %v", err)
		}
	}
	readBudget := func() int {
		var b int
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			b = world.Actors["vstr-1"].VisitorState.SpendBudget
			return nil, nil
		}}); err != nil {
			t.Fatalf("read budget: %v", err)
		}
		return b
	}

	seedBudget(4)
	if _, err := grant(t, w, "vstr-1", 15, nil); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if got := readBudget(); got != 19 {
		t.Errorf("budget after +15 grant = %d, want 19", got)
	}
	if _, err := grant(t, w, "vstr-1", -20, nil); err != nil {
		t.Fatalf("debit: %v", err)
	}
	if got := readBudget(); got != 19 {
		t.Errorf("budget after -20 debit = %d, want 19 (debits leave the budget alone)", got)
	}

	seedBudget(math.MaxInt - 2)
	if _, err := grant(t, w, "vstr-1", 5, nil); err != nil {
		t.Fatalf("saturating credit: %v", err)
	}
	if got := readBudget(); got != math.MaxInt {
		t.Errorf("budget after near-max grant = %d, want saturation at MaxInt, not a wrap", got)
	}
}
