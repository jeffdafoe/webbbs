package sim_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/repo/mem"
)

// pay_commands_test.go — sim-level coverage of the Pay Command's
// world-state validation, coin transfer, event emission, and bidirectional
// RecordInteraction matrix.
//
// Handler-level static validation (decode + bounds) lives in
// handlers/pay_test.go. Subscriber tests (warrant minting) live in
// handlers/pay_reactor_test.go.

// payActorSpec — mirrors actorSpec from speak_commands_test.go but adds
// Coins seeding for pay's balance gate.
type payActorSpec struct {
	id           sim.ActorID
	displayName  string
	kind         sim.ActorKind
	huddleID     sim.HuddleID
	coins        int
	moveInFlight bool
}

func buildPayTestWorld(t *testing.T, specs ...payActorSpec) (*sim.World, func()) {
	t.Helper()
	repo, handles := mem.NewRepository()
	seed := make(map[sim.ActorID]*sim.Actor, len(specs))
	for _, s := range specs {
		a := &sim.Actor{
			ID:              s.id,
			DisplayName:     s.displayName,
			Kind:            s.kind,
			State:           sim.StateIdle,
			CurrentHuddleID: s.huddleID,
			Coins:           s.coins,
			RecentActions:   sim.NewRingBuffer[sim.Action](4),
		}
		if s.moveInFlight {
			a.MoveIntent = &sim.MoveIntent{AttemptID: sim.MovementAttemptID(1)}
		}
		seed[s.id] = a
	}
	handles.Actors.Seed(seed)

	w, err := sim.LoadWorld(context.Background(), repo)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	return w, func() { cancel(); <-done }
}

// capturePaid registers a subscriber that records every emitted Paid event
// into the returned slice. Same pattern as captureSpoke — Subscribe routes
// through w.Send so it runs on the world goroutine.
func capturePaid(t *testing.T, w *sim.World) *[]sim.Paid {
	t.Helper()
	var out []sim.Paid
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		world.Subscribe(sim.SubscriberFunc(func(_ *sim.World, evt sim.Event) {
			if p, ok := evt.(*sim.Paid); ok {
				out = append(out, *p)
			}
		}))
		return nil, nil
	}}); err != nil {
		t.Fatalf("capturePaid subscribe: %v", err)
	}
	return &out
}

// --- TestPay_HappyPath: same-huddle pair, sufficient coins → transfer,
// Paid event emits, 2 RecordInteraction writes (both directions).
func TestPay_HappyPath(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1", coins: 2},
	)
	defer stop()

	captured := capturePaid(t, w)
	at := time.Now().UTC()
	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 3, "ale", at)); err != nil {
		t.Fatalf("Pay: %v", err)
	}

	// Event emitted.
	if len(*captured) != 1 {
		t.Fatalf("Paid events = %d, want 1", len(*captured))
	}
	got := (*captured)[0]
	if got.BuyerID != "hannah" || got.SellerID != "ezekiel" {
		t.Errorf("Paid actors = %q→%q, want hannah→ezekiel", got.BuyerID, got.SellerID)
	}
	if got.Amount != 3 {
		t.Errorf("Paid.Amount = %d, want 3", got.Amount)
	}
	if got.ForText != "ale" {
		t.Errorf("Paid.ForText = %q, want %q", got.ForText, "ale")
	}
	if !got.At.Equal(at) {
		t.Errorf("Paid.At = %v, want %v", got.At, at)
	}

	// Coin balances updated.
	snap := w.Published()
	if got, want := snap.Actors["hannah"].Coins, 7; got != want {
		t.Errorf("hannah.Coins = %d, want %d", got, want)
	}
	if got, want := snap.Actors["ezekiel"].Coins, 5; got != want {
		t.Errorf("ezekiel.Coins = %d, want %d", got, want)
	}

	// Bidirectional relationship facts.
	hannah := snap.Actors["hannah"]
	if rel := hannah.Relationships["ezekiel"]; rel == nil {
		t.Fatal("hannah.Relationships[ezekiel] missing")
	} else if len(rel.SalientFacts) != 1 {
		t.Errorf("hannah→ezekiel facts = %d, want 1", len(rel.SalientFacts))
	} else {
		fact := rel.SalientFacts[0]
		if fact.Kind != sim.InteractionPaid {
			t.Errorf("hannah→ezekiel fact.Kind = %q, want Paid", fact.Kind)
		}
		if fact.Text != "I paid Ezekiel Crane 3 coins for ale." {
			t.Errorf("hannah→ezekiel fact.Text = %q", fact.Text)
		}
		if !fact.At.Equal(at) {
			t.Errorf("hannah→ezekiel fact.At = %v, want %v", fact.At, at)
		}
	}
	ezekiel := snap.Actors["ezekiel"]
	if rel := ezekiel.Relationships["hannah"]; rel == nil {
		t.Fatal("ezekiel.Relationships[hannah] missing")
	} else if len(rel.SalientFacts) != 1 {
		t.Errorf("ezekiel→hannah facts = %d, want 1", len(rel.SalientFacts))
	} else {
		fact := rel.SalientFacts[0]
		if fact.Kind != sim.InteractionPaidBy {
			t.Errorf("ezekiel→hannah fact.Kind = %q, want PaidBy", fact.Kind)
		}
		if fact.Text != "Hannah paid me 3 coins for ale." {
			t.Errorf("ezekiel→hannah fact.Text = %q", fact.Text)
		}
	}
}

// --- TestPay_NoForText: same shape, ForText omitted produces the
// "X paid Y N coins." form without the trailing "for ...".
func TestPay_NoForText(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 5, "", time.Now().UTC())); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	snap := w.Published()
	fact := snap.Actors["hannah"].Relationships["ezekiel"].SalientFacts[0]
	if fact.Text != "I paid Ezekiel Crane 5 coins." {
		t.Errorf("fact.Text = %q, want %q", fact.Text, "I paid Ezekiel Crane 5 coins.")
	}
}

// --- TestPay_SingularCoin: amount=1 produces "1 coin" not "1 coins".
func TestPay_SingularCoin(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 1, "", time.Now().UTC())); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	snap := w.Published()
	fact := snap.Actors["hannah"].Relationships["ezekiel"].SalientFacts[0]
	if !strings.Contains(fact.Text, "1 coin.") || strings.Contains(fact.Text, "1 coins") {
		t.Errorf("singular coin missing: %q", fact.Text)
	}
}

// --- TestPay_NoHuddle: buyer not in any huddle rejects.
func TestPay_NoHuddle(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared},
	)
	defer stop()

	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for no-huddle, got nil")
	}
	if !strings.Contains(err.Error(), "not in a conversation") {
		t.Errorf("error lacks 'not in a conversation' guidance: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on rejected pay: %v", *captured)
	}
	// No coin change, no relationship write.
	snap := w.Published()
	if got := snap.Actors["hannah"].Coins; got != 10 {
		t.Errorf("hannah.Coins changed: %d", got)
	}
	if len(snap.Actors["hannah"].Relationships) != 0 {
		t.Errorf("hannah Relationships after reject: %v", snap.Actors["hannah"].Relationships)
	}
}

// --- TestPay_WalkInFlight: buyer with MoveIntent rejects.
func TestPay_WalkInFlight(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10, moveInFlight: true},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for walk-in-flight, got nil")
	}
	if !strings.Contains(err.Error(), "walking") {
		t.Errorf("error lacks 'walking' guidance: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on rejected pay: %v", *captured)
	}
}

// --- TestPay_RecipientNotInHuddle: recipient name doesn't match any peer
// in the buyer's huddle (the actor exists, just not in this conversation).
func TestPay_RecipientNotInHuddle(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "bob", displayName: "Bob", kind: sim.KindNPCShared, huddleID: "h1"},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared}, // NOT in huddle h1
	)
	defer stop()

	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for recipient-not-in-huddle, got nil")
	}
	if !strings.Contains(err.Error(), `"Ezekiel Crane"`) {
		t.Errorf("error message should name the missing recipient; got: %v", err)
	}
	if !strings.Contains(err.Error(), "no one named") {
		t.Errorf("error lacks 'no one named' guidance: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on rejected pay: %v", *captured)
	}
}

// --- TestPay_RecipientNonExistent: name doesn't match any actor at all.
// Same error message as "exists but not in huddle" — the buyer can't tell
// the difference from inside the conversation, and the error matches the
// conversational frame ("no one HERE named X").
func TestPay_RecipientNonExistent(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "bob", displayName: "Bob", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	_, err := w.Send(sim.Pay("hannah", "Phantom", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for nonexistent recipient, got nil")
	}
	if !strings.Contains(err.Error(), "no one named") {
		t.Errorf("error message = %v", err)
	}
}

// --- TestPay_SelfByName: paying via the buyer's own display name reads as
// "no one named X" (the buyer is excluded from the peer scan), not as a
// self-pay-specific error. This matches the conversational framing: from
// inside the huddle, "who is here besides me?" is the model's view.
func TestPay_SelfByName(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "bob", displayName: "Bob", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	_, err := w.Send(sim.Pay("hannah", "Hannah", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for self-by-name, got nil")
	}
	if !strings.Contains(err.Error(), "no one named") {
		t.Errorf("error message = %v, want 'no one named' framing", err)
	}
}

// --- TestPay_InsufficientCoins: balance < amount rejects.
func TestPay_InsufficientCoins(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 2},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 5, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for insufficient coins, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient coins") {
		t.Errorf("error lacks 'insufficient coins': %v", err)
	}
	if !strings.Contains(err.Error(), "have 2") || !strings.Contains(err.Error(), "need 5") {
		t.Errorf("error should include exact balance and amount; got: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on rejected pay: %v", *captured)
	}
	// No coin movement.
	snap := w.Published()
	if got := snap.Actors["hannah"].Coins; got != 2 {
		t.Errorf("hannah.Coins moved: %d", got)
	}
	if got := snap.Actors["ezekiel"].Coins; got != 0 {
		t.Errorf("ezekiel.Coins moved: %d", got)
	}
}

// --- TestPay_ExactBalance: balance == amount succeeds (boundary case).
func TestPay_ExactBalance(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 5},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 5, "", time.Now().UTC())); err != nil {
		t.Fatalf("Pay at exact balance: %v", err)
	}
	snap := w.Published()
	if got := snap.Actors["hannah"].Coins; got != 0 {
		t.Errorf("hannah.Coins = %d, want 0", got)
	}
	if got := snap.Actors["ezekiel"].Coins; got != 5 {
		t.Errorf("ezekiel.Coins = %d, want 5", got)
	}
}

// --- TestPay_CaseInsensitiveRecipient: "ezekiel crane" matches "Ezekiel Crane".
func TestPay_CaseInsensitiveRecipient(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	cases := []string{"ezekiel crane", "EZEKIEL CRANE", "EzEkIeL cRaNe"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			// Fresh receiver balance each subtest — reuse the same world but
			// re-test the lookup, not the transfer accumulating.
			if _, err := w.Send(sim.Pay("hannah", name, 1, "", time.Now().UTC())); err != nil {
				t.Errorf("Pay(%q): %v", name, err)
			}
		})
	}
}

// --- TestPay_UnknownBuyer: buyer ID not in w.Actors errors.
func TestPay_UnknownBuyer(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
	)
	defer stop()

	_, err := w.Send(sim.Pay("ghost", "Hannah", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want error for unknown buyer, got nil")
	}
	if !strings.Contains(err.Error(), "not in world") {
		t.Errorf("error = %v, want 'not in world'", err)
	}
}

// --- TestPay_KindNPCSharedGate_Matrix: persistence matrix from the
// design — 4 combinations of (buyer kind, seller kind). Same shape as
// speak's gate matrix.
func TestPay_KindNPCSharedGate_Matrix(t *testing.T) {
	cases := []struct {
		name         string
		buyerKind    sim.ActorKind
		sellerKind   sim.ActorKind
		buyerWrites  bool
		sellerWrites bool
	}{
		{"shared_shared", sim.KindNPCShared, sim.KindNPCShared, true, true},
		{"shared_stateful", sim.KindNPCShared, sim.KindNPCStateful, true, false},
		{"stateful_shared", sim.KindNPCStateful, sim.KindNPCShared, false, true},
		{"stateful_stateful", sim.KindNPCStateful, sim.KindNPCStateful, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, stop := buildPayTestWorld(t,
				payActorSpec{id: "b", displayName: "Buyer", kind: tc.buyerKind, huddleID: "h1", coins: 10},
				payActorSpec{id: "s", displayName: "Seller", kind: tc.sellerKind, huddleID: "h1"},
			)
			defer stop()

			if _, err := w.Send(sim.Pay("b", "Seller", 3, "", time.Now().UTC())); err != nil {
				t.Fatalf("Pay: %v", err)
			}
			snap := w.Published()
			buyerSideWrote := snap.Actors["b"].Relationships["s"] != nil
			sellerSideWrote := snap.Actors["s"].Relationships["b"] != nil
			if buyerSideWrote != tc.buyerWrites {
				t.Errorf("buyer side wrote = %v, want %v", buyerSideWrote, tc.buyerWrites)
			}
			if sellerSideWrote != tc.sellerWrites {
				t.Errorf("seller side wrote = %v, want %v", sellerSideWrote, tc.sellerWrites)
			}
			// Coin transfer ALWAYS happens (the KindNPCShared gate is on the
			// relationship writes only — the transfer itself is unconditional).
			if got := snap.Actors["b"].Coins; got != 7 {
				t.Errorf("buyer.Coins = %d, want 7 (transfer should always fire)", got)
			}
			if got := snap.Actors["s"].Coins; got != 3 {
				t.Errorf("seller.Coins = %d, want 3 (transfer should always fire)", got)
			}
		})
	}
}

// --- TestPay_RejectionEmitsNoPaid: every reject path leaves no Paid event
// and no coin movement.
func TestPay_RejectionEmitsNoPaid(t *testing.T) {
	cases := []struct {
		name string
		set  func(t *testing.T) (*sim.World, func(), sim.ActorID, string, int)
	}{
		{
			name: "no_huddle",
			set: func(t *testing.T) (*sim.World, func(), sim.ActorID, string, int) {
				w, stop := buildPayTestWorld(t,
					payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, coins: 10},
					payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared},
				)
				return w, stop, "hannah", "Ezekiel Crane", 3
			},
		},
		{
			name: "walk_in_flight",
			set: func(t *testing.T) (*sim.World, func(), sim.ActorID, string, int) {
				w, stop := buildPayTestWorld(t,
					payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10, moveInFlight: true},
					payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
				)
				return w, stop, "hannah", "Ezekiel Crane", 3
			},
		},
		{
			name: "recipient_absent",
			set: func(t *testing.T) (*sim.World, func(), sim.ActorID, string, int) {
				w, stop := buildPayTestWorld(t,
					payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
					payActorSpec{id: "bob", displayName: "Bob", kind: sim.KindNPCShared, huddleID: "h1"},
				)
				return w, stop, "hannah", "Phantom", 3
			},
		},
		{
			name: "insufficient_coins",
			set: func(t *testing.T) (*sim.World, func(), sim.ActorID, string, int) {
				w, stop := buildPayTestWorld(t,
					payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 1},
					payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
				)
				return w, stop, "hannah", "Ezekiel Crane", 5
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, stop, buyerID, recipient, amount := tc.set(t)
			defer stop()
			captured := capturePaid(t, w)
			beforeSnap := w.Published()
			beforeBuyer := beforeSnap.Actors[buyerID].Coins
			_, err := w.Send(sim.Pay(buyerID, recipient, amount, "", time.Now().UTC()))
			if err == nil {
				t.Fatal("Pay: want error, got nil")
			}
			if len(*captured) != 0 {
				t.Errorf("Paid emitted on reject: %v", *captured)
			}
			// Coin balance unchanged.
			afterSnap := w.Published()
			if got := afterSnap.Actors[buyerID].Coins; got != beforeBuyer {
				t.Errorf("buyer balance moved on reject: before=%d after=%d", beforeBuyer, got)
			}
		})
	}
}

// --- TestPay_RejectsNonPositiveAmount: amount < 1 rejected by the Command
// itself (defense in depth — Pay is exported, non-handler callers must not
// be able to mint coins via amount<=0). Verifies no event, no transfer,
// no relationship writes.
func TestPay_RejectsNonPositiveAmount(t *testing.T) {
	cases := []struct {
		name   string
		amount int
	}{
		{"zero", 0},
		{"negative_small", -1},
		{"negative_large", -1000},
		{"int_min", math.MinInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, stop := buildPayTestWorld(t,
				payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
				payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1", coins: 5},
			)
			defer stop()
			captured := capturePaid(t, w)
			_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", tc.amount, "", time.Now().UTC()))
			if err == nil {
				t.Fatalf("Pay(amount=%d): want error, got nil", tc.amount)
			}
			if !strings.Contains(err.Error(), "at least 1") {
				t.Errorf("error lacks 'at least 1' guidance: %v", err)
			}
			if len(*captured) != 0 {
				t.Errorf("Paid emitted on amount=%d: %v", tc.amount, *captured)
			}
			snap := w.Published()
			if got := snap.Actors["hannah"].Coins; got != 10 {
				t.Errorf("hannah.Coins moved: %d (want 10)", got)
			}
			if got := snap.Actors["ezekiel"].Coins; got != 5 {
				t.Errorf("ezekiel.Coins moved: %d (want 5)", got)
			}
			if len(snap.Actors["hannah"].Relationships) != 0 {
				t.Errorf("hannah relationships after reject: %v", snap.Actors["hannah"].Relationships)
			}
		})
	}
}

// --- TestPay_RejectsAmountOverMax: amount > MaxPayAmount rejected.
func TestPay_RejectsAmountOverMax(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: math.MaxInt},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()
	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", sim.MaxPayAmount+1, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay(amount=MaxPayAmount+1): want error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("error lacks 'exceeds maximum': %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on over-max amount: %v", *captured)
	}
}

// --- TestPay_RejectsSellerBalanceOverflow: seller already at near-MaxInt
// + a legitimate amount would wrap negative. Theoretical at village scale
// but mint-path-adjacent.
func TestPay_RejectsSellerBalanceOverflow(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 1000},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1", coins: math.MaxInt - 100},
	)
	defer stop()
	captured := capturePaid(t, w)
	// 500 + (MaxInt - 100) overflows MaxInt.
	_, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 500, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay near seller overflow: want error, got nil")
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Errorf("error lacks 'overflow': %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on overflow: %v", *captured)
	}
	snap := w.Published()
	if got := snap.Actors["hannah"].Coins; got != 1000 {
		t.Errorf("hannah.Coins moved on overflow reject: %d", got)
	}
}

// --- TestPay_AmbiguousRecipientRejects: two huddle peers share a
// case-insensitive DisplayName. Pay must reject rather than pick one
// non-deterministically (money-transfer determinism).
func TestPay_AmbiguousRecipientRejects(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "john1", displayName: "John", kind: sim.KindNPCShared, huddleID: "h1"},
		payActorSpec{id: "john2", displayName: "john", kind: sim.KindNPCShared, huddleID: "h1"}, // case-insensitive duplicate
	)
	defer stop()
	captured := capturePaid(t, w)
	_, err := w.Send(sim.Pay("hannah", "John", 3, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay ambiguous: want error, got nil")
	}
	if !strings.Contains(err.Error(), "more than one") {
		t.Errorf("error lacks 'more than one' guidance: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("Paid emitted on ambiguous reject: %v", *captured)
	}
	// Neither John receives coins, buyer balance unchanged.
	snap := w.Published()
	if got := snap.Actors["hannah"].Coins; got != 10 {
		t.Errorf("hannah.Coins moved: %d", got)
	}
	if got := snap.Actors["john1"].Coins; got != 0 {
		t.Errorf("john1.Coins moved: %d", got)
	}
	if got := snap.Actors["john2"].Coins; got != 0 {
		t.Errorf("john2.Coins moved: %d", got)
	}
}

// --- TestPay_TwoPaysAccumulate: pay twice → two SalientFacts on each side,
// InteractionCount=2, LastInteractionAt advances.
func TestPay_TwoPaysAccumulate(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "hannah", displayName: "Hannah", kind: sim.KindNPCShared, huddleID: "h1", coins: 10},
		payActorSpec{id: "ezekiel", displayName: "Ezekiel Crane", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	first := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 1, "", first)); err != nil {
		t.Fatalf("Pay 1: %v", err)
	}
	if _, err := w.Send(sim.Pay("hannah", "Ezekiel Crane", 2, "bread", second)); err != nil {
		t.Fatalf("Pay 2: %v", err)
	}
	snap := w.Published()
	rel := snap.Actors["hannah"].Relationships["ezekiel"]
	if rel == nil {
		t.Fatal("relationship missing")
	}
	if rel.InteractionCount != 2 {
		t.Errorf("InteractionCount = %d, want 2", rel.InteractionCount)
	}
	if len(rel.SalientFacts) != 2 {
		t.Fatalf("SalientFacts len = %d, want 2", len(rel.SalientFacts))
	}
	if !rel.LastInteractionAt.Equal(second) {
		t.Errorf("LastInteractionAt = %v, want %v", rel.LastInteractionAt, second)
	}
}

// --- TestPay_RedirectsToOpenQuoteSettlement: LLM-172. A bare pay naming a good
// the seller has an active quote for is rejected with a redirect to
// pay_with_item, and no coins move — bare pay transfers coins but never delivers
// the good or settles the quote, so letting it through leaks coins for nothing
// (the live Ezekiel/John stew loop). buildFastPathFixture posts Bob's active
// stew quote (id 7, qty 1, 4 coins) to Alice in scene sc1.
func TestPay_RedirectsToOpenQuoteSettlement(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()

	_, err := w.Send(sim.Pay("alice", "Bob", 4, "stew", at))
	if err == nil {
		t.Fatal("bare pay for a quoted good should be redirected to pay_with_item")
	}
	if !strings.Contains(err.Error(), "pay_with_item with quote_id 7") {
		t.Errorf("redirect missing the quote_id steer: %v", err)
	}
	if !strings.Contains(err.Error(), "won't deliver") {
		t.Errorf("redirect missing the no-delivery explanation: %v", err)
	}

	// The reject fires before any state change — no coins moved.
	snap := w.Published()
	if got := snap.Actors["alice"].Coins; got != 50 {
		t.Errorf("alice.Coins = %d, want 50 (no transfer on redirect)", got)
	}
	if got := snap.Actors["bob"].Coins; got != 0 {
		t.Errorf("bob.Coins = %d, want 0 (no transfer on redirect)", got)
	}
}

// --- TestPay_AllowsTipWhenForTextNamesNoQuotedGood: LLM-172 fall-through. A
// bare pay whose forText names no active quoted good (a tip / thanks) is not a
// botched purchase — it proceeds as a plain coin transfer. Same fixture (Bob
// has only a stew quote), but the pay is "for" something resolveItemKind can't
// canonicalize, so the open-quote guard doesn't fire.
func TestPay_AllowsTipWhenForTextNamesNoQuotedGood(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()

	if _, err := w.Send(sim.Pay("alice", "Bob", 3, "your kindness", at)); err != nil {
		t.Fatalf("a tip naming no quoted good should proceed: %v", err)
	}
	snap := w.Published()
	if got := snap.Actors["alice"].Coins; got != 47 {
		t.Errorf("alice.Coins = %d, want 47 (tip transferred)", got)
	}
	if got := snap.Actors["bob"].Coins; got != 3 {
		t.Errorf("bob.Coins = %d, want 3 (tip received)", got)
	}
}

// --- TestPay_CoinShortQuoteSteersToBargainNotSettlement: LLM-172 (code_review).
// A coin-short buyer for a quoted good must NOT be redirected to a pay_with_item
// it can't afford — that just loops with the right tool. Steer to bargain or
// barter instead, and move no coins. Alice is dropped below Bob's 4-coin stew
// quote.
func TestPay_CoinShortQuoteSteersToBargainNotSettlement(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()
	mustSend(t, w, func(world *sim.World) { world.Actors["alice"].Coins = 3 })

	_, err := w.Send(sim.Pay("alice", "Bob", 3, "stew", at))
	if err == nil {
		t.Fatal("coin-short bare pay for a quoted good should be rejected")
	}
	msg := err.Error()
	if strings.Contains(msg, "pay_with_item") {
		t.Errorf("coin-short buyer steered to an unaffordable settlement: %v", err)
	}
	if !strings.Contains(msg, "you only have 3") || !strings.Contains(msg, "offer_trade") {
		t.Errorf("missing the bargain/barter steer: %v", err)
	}

	snap := w.Published()
	if got := snap.Actors["alice"].Coins; got != 3 {
		t.Errorf("alice.Coins = %d, want 3 (no transfer)", got)
	}
	if got := snap.Actors["bob"].Coins; got != 0 {
		t.Errorf("bob.Coins = %d, want 0 (no transfer)", got)
	}
}

// --- TestPay_VisitorBudgetGate (LLM-644): a visitor's bare pay is capped at
// his remaining trip budget, not his wallet — a factor flush with bale
// proceeds must not be able to hand them back. The gate names the SPENDABLE
// figure; a pay within budget draws it down.
func TestPay_VisitorBudgetGate(t *testing.T) {
	w, stop := buildPayTestWorld(t,
		payActorSpec{id: "factor", displayName: "Elias Drum", kind: sim.KindNPCShared, huddleID: "h1", coins: 131},
		payActorSpec{id: "josiah", displayName: "Josiah Thorne", kind: sim.KindNPCShared, huddleID: "h1"},
	)
	defer stop()

	// Stamp visitor state after load: wallet 131 (proceeds included), only 5
	// left of the arrival purse. The wallet alone would cover every pay below.
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		world.Actors["factor"].VisitorState = &sim.VisitorState{SpendBudget: 5}
		return nil, nil
	}}); err != nil {
		t.Fatalf("seed visitor state: %v", err)
	}

	_, err := w.Send(sim.Pay("factor", "Josiah Thorne", 10, "", time.Now().UTC()))
	if err == nil {
		t.Fatal("Pay: want budget rejection for a visitor over trip budget, got nil")
	}
	if !strings.Contains(err.Error(), "have 5 to spend") || !strings.Contains(err.Error(), "need 10") {
		t.Errorf("error should name the spendable figure, not the wallet: %v", err)
	}

	if _, err := w.Send(sim.Pay("factor", "Josiah Thorne", 4, "", time.Now().UTC())); err != nil {
		t.Fatalf("Pay within budget: %v", err)
	}
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		factor := world.Actors["factor"]
		if factor.Coins != 127 {
			t.Errorf("factor.Coins = %d; want 127", factor.Coins)
		}
		if got := factor.VisitorState.SpendBudget; got != 1 {
			t.Errorf("SpendBudget = %d; want 1 (drawn by the pay)", got)
		}
		if got := world.Actors["josiah"].Coins; got != 4 {
			t.Errorf("josiah.Coins = %d; want 4", got)
		}
		return nil, nil
	}}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// --- TestPay_RejectsBundleMemo: LLM-649. A bare pay whose memo names several
// goods is a purchase on the wrong tool — the LLM-172 guard resolves the whole
// memo as one item and sees nothing in a list, so the coins moved and no goods
// did (live 2026-08-31: a factor's "5x wheat, 3x flour, 2x firewood" at
// Josiah's counter). Refuse it with the pay_with_item steer and move nothing.
func TestPay_RejectsBundleMemo(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()

	// The last three are code_review's whitespace variants around "and": a
	// single-space split was bypassable by a second space, a tab, or a newline.
	for _, memo := range []string{
		"bread, ale", "5x wheat, 3x bread", "bread and ale", "bread; ale", "2 x bread & 1 ale",
		"5x wheat and  3x bread", "bread\tand\tale", "wheat\nand\nbread",
	} {
		_, err := w.Send(sim.Pay("alice", "Bob", 7, memo, at))
		if err == nil {
			t.Fatalf("bare pay for %q should be refused", memo)
		}
		if !strings.Contains(err.Error(), "pay_with_item") || !strings.Contains(err.Error(), "only hands over coins") {
			t.Errorf("memo %q: steer missing: %v", memo, err)
		}
	}
	snap := w.Published()
	if got := snap.Actors["alice"].Coins; got != 50 {
		t.Errorf("alice.Coins = %d, want 50 (no transfer on refusal)", got)
	}
	if got := snap.Actors["bob"].Coins; got != 0 {
		t.Errorf("bob.Coins = %d, want 0 (no transfer on refusal)", got)
	}
}

// --- TestPay_RejectsCountedGoodMemo: LLM-649. One good with an explicit count
// ("5x wheat") is the same purchase shape as a bundle and is refused the same way.
func TestPay_RejectsCountedGoodMemo(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()

	_, err := w.Send(sim.Pay("alice", "Bob", 10, "5x wheat", at))
	if err == nil || !strings.Contains(err.Error(), "pay_with_item") {
		t.Fatalf("counted single good should be refused with the pay_with_item steer, got %v", err)
	}
	if got := w.Published().Actors["alice"].Coins; got != 50 {
		t.Errorf("alice.Coins = %d, want 50 (no transfer on refusal)", got)
	}
}

// --- TestPay_AllowsUncountedSingleGoodMemo: LLM-649 scope boundary. One good
// with no count and no open quote for it ("the ale") is the debt-memo shape and
// still transfers — Bob has a stew quote only, so the LLM-172 guard is silent too.
func TestPay_AllowsUncountedSingleGoodMemo(t *testing.T) {
	w, stop, at := buildFastPathFixture(t, 7)
	defer stop()

	if _, err := w.Send(sim.Pay("alice", "Bob", 2, "the ale", at)); err != nil {
		t.Fatalf("a single uncounted good with no quote should transfer as a debt memo: %v", err)
	}
	if got := w.Published().Actors["bob"].Coins; got != 2 {
		t.Errorf("bob.Coins = %d, want 2", got)
	}
}
