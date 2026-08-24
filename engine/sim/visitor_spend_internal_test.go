package sim

import (
	"math"
	"testing"
)

// visitor_spend_internal_test.go — unit coverage for the LLM-644 trip-budget
// primitives. The door-level behavior (Pay's gate, the plan codec, spawn
// seeding) is covered where each door lives; this file pins the arithmetic
// every door shares.

func TestSpendableCoins(t *testing.T) {
	cases := []struct {
		name    string
		coins   int
		visitor *VisitorState
		want    int
	}{
		{"resident is uncapped", 131, nil, 131},
		{"visitor capped by budget", 131, &VisitorState{SpendBudget: 40}, 40},
		{"visitor capped by wallet", 10, &VisitorState{SpendBudget: 40}, 10},
		{"visitor budget spent", 131, &VisitorState{SpendBudget: 0}, 0},
		{"visitor broke", 0, &VisitorState{SpendBudget: 40}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Actor{Coins: tc.coins, VisitorState: tc.visitor}
			if got := a.SpendableCoins(); got != tc.want {
				t.Errorf("Actor.SpendableCoins() = %d; want %d", got, tc.want)
			}
			var vsSnap *VisitorState
			if tc.visitor != nil {
				vsSnap = &VisitorState{SpendBudget: tc.visitor.SpendBudget}
			}
			s := &ActorSnapshot{Coins: tc.coins, VisitorState: vsSnap}
			if got := s.SpendableCoins(); got != tc.want {
				t.Errorf("ActorSnapshot.SpendableCoins() = %d; want %d", got, tc.want)
			}
		})
	}
}

func TestDrawVisitorSpend(t *testing.T) {
	// Resident: no-op by construction.
	resident := &Actor{Coins: 50}
	drawVisitorSpend(resident, 10)

	v := &Actor{Coins: 50, VisitorState: &VisitorState{SpendBudget: 12}}
	drawVisitorSpend(v, 10)
	if v.VisitorState.SpendBudget != 2 {
		t.Errorf("SpendBudget after draw 10 = %d; want 2", v.VisitorState.SpendBudget)
	}
	// Over-draw clamps at 0 (settle-time coin movement must not wedge on a
	// stale budget), and a zero/negative amount is a no-op.
	drawVisitorSpend(v, 10)
	if v.VisitorState.SpendBudget != 0 {
		t.Errorf("SpendBudget after over-draw = %d; want 0 (clamped)", v.VisitorState.SpendBudget)
	}
	drawVisitorSpend(v, 0)
	drawVisitorSpend(v, -3)
	if v.VisitorState.SpendBudget != 0 {
		t.Errorf("SpendBudget after no-op draws = %d; want 0", v.VisitorState.SpendBudget)
	}
	drawVisitorSpend(nil, 5)
}

func TestRefundVisitorSpendSaturates(t *testing.T) {
	v := &Actor{VisitorState: &VisitorState{SpendBudget: math.MaxInt - 2}}
	refundVisitorSpend(v, 5)
	if v.VisitorState.SpendBudget != math.MaxInt {
		t.Errorf("SpendBudget = %d; want saturation at MaxInt, not a wrap", v.VisitorState.SpendBudget)
	}
}

func TestRefundVisitorSpend(t *testing.T) {
	v := &Actor{Coins: 50, VisitorState: &VisitorState{SpendBudget: 3}}
	refundVisitorSpend(v, 4)
	if v.VisitorState.SpendBudget != 7 {
		t.Errorf("SpendBudget after refund 4 = %d; want 7", v.VisitorState.SpendBudget)
	}
	refundVisitorSpend(v, 0)
	refundVisitorSpend(v, -2)
	if v.VisitorState.SpendBudget != 7 {
		t.Errorf("SpendBudget after no-op refunds = %d; want 7", v.VisitorState.SpendBudget)
	}
	resident := &Actor{Coins: 50}
	refundVisitorSpend(resident, 4)
	refundVisitorSpend(nil, 4)
}
