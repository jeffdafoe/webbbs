package pg

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// visitors_plan_test.go — pure round-trip coverage for the visitor.plan jsonb
// codec (LLM-373): encodeVisitorPlan (off the live Actor) → applyVisitorPlan (onto
// a LoadedVisitor). No DB — the DB integration path is covered by
// visitors_integration_test.go.

func TestVisitorPlanRoundTrip(t *testing.T) {
	roomExpiry := time.Now().UTC().Add(8 * time.Hour)
	created := time.Now().UTC().Add(-time.Hour)
	a := &sim.Actor{
		ID:        "vstr-00001234",
		Inventory: map[sim.ItemKind]int{"cheese": 3, "ale": 2},
		Coins:     42,
		RoomAccess: map[sim.RoomAccessKey]*sim.RoomAccess{
			{RoomID: 5, Source: sim.AccessSourceLedger}: {
				RoomID: 5, Source: sim.AccessSourceLedger, LedgerID: 12,
				ExpiresAt: &roomExpiry, Active: true, CreatedAt: created,
			},
		},
		VisitorState: &sim.VisitorState{
			VisitedBusinesses: []sim.StructureID{"str-a", "str-b"},
			// LLM-545 shared-word memory rides the plan jsonb.
			PayloadSharedWith: []sim.ActorID{"hannah", "john"},
			// LLM-455 merchant errand rides the plan jsonb.
			Trade: &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "cheese", Counterparty: "str-a", Settled: true},
			// LLM-644 trip budget rides the plan jsonb. Deliberately distinct
			// from Coins so a codec that conflated the two would fail here.
			SpendBudget: 17,
		},
	}

	js, err := encodeVisitorPlan(a)
	if err != nil {
		t.Fatalf("encodeVisitorPlan: %v", err)
	}
	lv := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan([]byte(js), lv); err != nil {
		t.Fatalf("applyVisitorPlan: %v", err)
	}

	if len(lv.VisitorState.VisitedBusinesses) != 2 ||
		lv.VisitorState.VisitedBusinesses[0] != "str-a" || lv.VisitorState.VisitedBusinesses[1] != "str-b" {
		t.Errorf("VisitedBusinesses = %v; want [str-a str-b]", lv.VisitorState.VisitedBusinesses)
	}
	if len(lv.VisitorState.PayloadSharedWith) != 2 ||
		lv.VisitorState.PayloadSharedWith[0] != "hannah" || lv.VisitorState.PayloadSharedWith[1] != "john" {
		t.Errorf("PayloadSharedWith = %v; want [hannah john]", lv.VisitorState.PayloadSharedWith)
	}
	if lv.Coins != 42 {
		t.Errorf("Coins = %d; want 42", lv.Coins)
	}
	if lv.VisitorState.SpendBudget != 17 {
		t.Errorf("SpendBudget = %d; want 17", lv.VisitorState.SpendBudget)
	}
	if lv.VisitorState.Trade == nil {
		t.Fatal("Trade errand did not round-trip through the plan jsonb")
	}
	if lv.VisitorState.Trade.Direction != sim.TradeDirectionBuy || lv.VisitorState.Trade.Good != "cheese" ||
		lv.VisitorState.Trade.Counterparty != "str-a" || !lv.VisitorState.Trade.Settled {
		t.Errorf("Trade round-trip = %+v; want buy cheese @ str-a settled", lv.VisitorState.Trade)
	}
	if lv.Inventory["cheese"] != 3 || lv.Inventory["ale"] != 2 {
		t.Errorf("Inventory = %v; want cheese:3 ale:2", lv.Inventory)
	}
	g := lv.RoomAccess[sim.RoomAccessKey{RoomID: 5, Source: sim.AccessSourceLedger}]
	if g == nil || g.LedgerID != 12 || !g.Active || g.ExpiresAt == nil || !g.ExpiresAt.Equal(roomExpiry) {
		t.Errorf("restored grant = %+v; want active ledger grant ledger=12 expiry=%v", g, roomExpiry)
	}
}

// TestVisitorPlanPayloadSharedWithSanitized — the LLM-545 decode posture for
// out-of-band plan data: duplicates and blank ids are dropped on the way in (the
// engine-side writer keeps the set unique, so they can only come from an edited
// row), a JSON null field decodes to an empty set, and a structurally corrupt
// array (non-string element) fails the whole plan apply like any other corrupt
// plan. Unknown-but-well-formed actor ids are RETAINED — perception intersects
// against the live snapshot before rendering, so they stay harmless.
func TestVisitorPlanPayloadSharedWithSanitized(t *testing.T) {
	lv := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	raw := []byte(`{"payload_shared_with":["hannah","","hannah","john","hannah"]}`)
	if err := applyVisitorPlan(raw, lv); err != nil {
		t.Fatalf("applyVisitorPlan: %v", err)
	}
	got := lv.VisitorState.PayloadSharedWith
	if len(got) != 2 || got[0] != "hannah" || got[1] != "john" {
		t.Errorf("PayloadSharedWith = %v; want deduped [hannah john] with blanks dropped", got)
	}

	lv2 := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan([]byte(`{"payload_shared_with":null}`), lv2); err != nil {
		t.Fatalf("applyVisitorPlan(null field): %v", err)
	}
	if lv2.VisitorState.PayloadSharedWith != nil {
		t.Errorf("null field decoded to %v; want empty", lv2.VisitorState.PayloadSharedWith)
	}

	lv3 := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan([]byte(`{"payload_shared_with":[42]}`), lv3); err == nil {
		t.Error("non-string array element must fail the plan apply like any corrupt plan")
	}

	// An oversized array (only reachable from an edited/corrupt row — the writer
	// caps at sim.MaxPayloadSharedWith) is truncated at the cap, not rejected:
	// the rest of the plan is independently valid. Blanks and duplicates are
	// interleaved so the ordering is pinned: dedup/drop first, THEN cap — junk
	// entries never consume cap slots, and the retained set is the first cap
	// distinct ids in stored order.
	big, err := json.Marshal(struct {
		IDs []string `json:"payload_shared_with"`
	}{IDs: func() []string {
		var ids []string
		for i := 0; i < sim.MaxPayloadSharedWith+10; i++ {
			ids = append(ids, fmt.Sprintf("actor-%03d", i), "", fmt.Sprintf("actor-%03d", i))
		}
		return ids
	}()})
	if err != nil {
		t.Fatalf("marshal oversized plan: %v", err)
	}
	lv4 := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan(big, lv4); err != nil {
		t.Fatalf("applyVisitorPlan(oversized): %v", err)
	}
	kept := lv4.VisitorState.PayloadSharedWith
	if len(kept) != sim.MaxPayloadSharedWith {
		t.Fatalf("oversized array retained %d ids; want truncation at the cap (%d)", len(kept), sim.MaxPayloadSharedWith)
	}
	for i, id := range kept {
		if want := sim.ActorID(fmt.Sprintf("actor-%03d", i)); id != want {
			t.Fatalf("kept[%d] = %q; want %q — dedup/blank-drop must run before the cap, preserving first-seen order", i, id, want)
		}
	}
}

// TestVisitorPlanEmpty — an absent / empty plan (an old-engine row, or a
// freshly-spawned visitor before its first checkpoint) applies as a clean no-op,
// leaving the LoadedVisitor at its zero plan.
func TestVisitorPlanEmpty(t *testing.T) {
	lv := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{Phase: sim.VisitorPhasePresent}}
	if err := applyVisitorPlan(nil, lv); err != nil {
		t.Fatalf("applyVisitorPlan(nil): %v", err)
	}
	if err := applyVisitorPlan([]byte("{}"), lv); err != nil {
		t.Fatalf("applyVisitorPlan({}): %v", err)
	}
	if lv.Coins != 0 || lv.Inventory != nil || lv.RoomAccess != nil ||
		lv.VisitorState.VisitedBusinesses != nil {
		t.Errorf("empty plan mutated the visitor: coins=%d inv=%v room=%v visited=%v",
			lv.Coins, lv.Inventory, lv.RoomAccess, lv.VisitorState.VisitedBusinesses)
	}

	// An actor carrying nothing encodes to a minimal object that round-trips clean.
	empty := &sim.Actor{ID: "vstr-0000eeee", VisitorState: &sim.VisitorState{}}
	js, err := encodeVisitorPlan(empty)
	if err != nil {
		t.Fatalf("encodeVisitorPlan(empty): %v", err)
	}
	lv2 := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan([]byte(js), lv2); err != nil {
		t.Fatalf("applyVisitorPlan(empty): %v", err)
	}
	if lv2.Coins != 0 || len(lv2.Inventory) != 0 || len(lv2.RoomAccess) != 0 {
		t.Errorf("empty actor did not round-trip clean: %+v", lv2)
	}
}

// TestVisitorPlanSpendBudgetDecode — the LLM-644 decode matrix for the trip
// budget. Absent (a pre-LLM-644 row) rehydrates with budget = wallet, the old
// uncapped behavior, so one in-flight legacy visit finishes as it began. An
// explicit 0 stays 0 — a spent-out factor must not refill by riding a deploy
// (the pointer-typed JSON field exists exactly so 0 and absent stay distinct).
// A negative value (out-of-band edit) clamps to 0.
func TestVisitorPlanSpendBudgetDecode(t *testing.T) {
	cases := []struct {
		name string
		plan string
		want int
	}{
		{"absent defaults to wallet", `{"coins": 42}`, 42},
		{"explicit zero stays zero", `{"coins": 42, "spend_budget": 0}`, 0},
		{"explicit value kept", `{"coins": 42, "spend_budget": 9}`, 9},
		{"negative clamps to zero", `{"coins": 42, "spend_budget": -5}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lv := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
			if err := applyVisitorPlan([]byte(tc.plan), lv); err != nil {
				t.Fatalf("applyVisitorPlan: %v", err)
			}
			if lv.VisitorState.SpendBudget != tc.want {
				t.Errorf("SpendBudget = %d; want %d", lv.VisitorState.SpendBudget, tc.want)
			}
		})
	}

	// A spent budget survives the encode side too: 0 must be written
	// explicitly, not dropped by omitempty into the legacy-default path.
	spent := &sim.Actor{ID: "vstr-0000ffff", Coins: 42, VisitorState: &sim.VisitorState{SpendBudget: 0}}
	js, err := encodeVisitorPlan(spent)
	if err != nil {
		t.Fatalf("encodeVisitorPlan: %v", err)
	}
	lv := &sim.LoadedVisitor{VisitorState: &sim.VisitorState{}}
	if err := applyVisitorPlan([]byte(js), lv); err != nil {
		t.Fatalf("applyVisitorPlan: %v", err)
	}
	if lv.VisitorState.SpendBudget != 0 {
		t.Errorf("spent budget round-trip = %d; want 0 (encode must write the field explicitly)", lv.VisitorState.SpendBudget)
	}
}
