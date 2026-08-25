package sim_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// scene_quote_churn_test.go — LLM-645. The seller-side half of the LLM-555
// churn memory: a TARGETED quote for a kind the target sold the seller within
// SoldToPeerMemoryTTL is refused at creation.
//
// Why creation and not acceptance: the buyer-side gate already refuses the
// buy-back, but it stands aside for an active targeted quote (the LLM-551
// escape hatch, pinned open by TestPayWithItem_TradeChurnGate). Vendors quote,
// so in every live churn case the buyer of the first leg posted the reverse
// offer himself and handed the original seller the hatch key — live 2026-08-24,
// three legs of the same woolens between Josiah and a factor inside an hour,
// each middle leg riding a freshly posted quote. Refusing the tainted quote at
// creation is what lets the hatch stay open for the legitimate case it exists
// for.
//
// Helpers: buildQuoteTestWorld / captureSceneQuoteCreated (scene_quote_test.go),
// rememberSoldTo (pay_with_item_trade_churn_test.go), mustSend.

// buildQuoteChurnWorld seeds the live shape: a factor sold Josiah ale earlier
// (the memory sits on the factor), and Josiah now holds the stock.
func buildQuoteChurnWorld(t *testing.T) (*sim.World, func(), time.Time) {
	t.Helper()
	w, stop := buildQuoteTestWorld(t, "h1", "sc1", []quoteTestActor{
		{id: "josiah", displayName: "Josiah", kind: sim.KindNPCStateful, huddleID: "h1", inventory: map[sim.ItemKind]int{"ale": 3, "bread": 4}},
		{id: "factor", displayName: "Roger the factor", kind: sim.KindNPCShared, huddleID: "h1", inventory: map[sim.ItemKind]int{}},
	})
	return w, stop, time.Now().UTC()
}

func TestSceneQuoteCreate_ChurnTargetRejected(t *testing.T) {
	const wantSteer = "you don't offer it straight back"

	// The arbitrating case: Josiah quotes the ale back at the man who sold it to
	// him an hour ago. Refused before any quote exists, so the buyer-side gate's
	// escape hatch has nothing to open on.
	t.Run("quote_back_at_the_man_who_sold_it_is_refused", func(t *testing.T) {
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "factor", "josiah", "ale", at.Add(-time.Hour))

		captured := captureSceneQuoteCreated(t, w)
		_, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "ale", Qty: 1}}, 4, false, "Roger the factor", nil, at))
		if err == nil || !strings.Contains(err.Error(), wantSteer) {
			t.Fatalf("quoting a good back at its seller must be refused: err = %v", err)
		}
		if len(*captured) != 0 {
			t.Errorf("a quote was minted despite the churn reject: %+v", *captured)
		}
	})

	// A bundle is refused if ANY line is the churn kind — a tainted line inside a
	// basket would otherwise be the hole the buy-back walks through, exactly as
	// the capture half stamps every settled bundle line.
	t.Run("a_bundle_carrying_the_churn_kind_is_refused_whole", func(t *testing.T) {
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "factor", "josiah", "ale", at.Add(-time.Hour))

		_, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "bread", Qty: 1}, {ItemName: "ale", Qty: 1}}, 6, false, "Roger the factor", nil, at))
		if err == nil || !strings.Contains(err.Error(), wantSteer) {
			t.Fatalf("a bundle with a churn line must be refused: err = %v", err)
		}
		if !strings.Contains(err.Error(), "ale") {
			t.Errorf("the reject should name the offending kind: %v", err)
		}
	})

	// The cases that keep ordinary commerce whole.
	t.Run("a_different_good_at_the_same_man_still_quotes", func(t *testing.T) {
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "factor", "josiah", "ale", at.Add(-time.Hour))

		if _, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "bread", Qty: 1}}, 2, false, "Roger the factor", nil, at)); err != nil {
			t.Fatalf("an unrelated good must still be quotable: %v", err)
		}
	})

	t.Run("a_public_quote_is_not_gated", func(t *testing.T) {
		// A public quote never opens the buyer-side escape hatch
		// (activeTargetedQuoteOffers requires TargetBuyer), so the original
		// seller who tries to take it is still refused by that gate. Blocking
		// the public quote would cost every OTHER buyer in the scene the offer.
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "factor", "josiah", "ale", at.Add(-time.Hour))

		if _, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "ale", Qty: 1}}, 4, false, "", nil, at)); err != nil {
			t.Fatalf("a public quote must not be churn-gated: %v", err)
		}
	})

	t.Run("the_memory_lapses_and_the_quote_stands", func(t *testing.T) {
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "factor", "josiah", "ale", at.Add(-sim.SoldToPeerMemoryTTL-time.Minute))

		if _, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "ale", Qty: 1}}, 4, false, "Roger the factor", nil, at)); err != nil {
			t.Fatalf("past the TTL the pair may deal in that direction again: %v", err)
		}
	})

	// Directional: the memory reads "the TARGET sold me this". The seller's own
	// memory of selling the target this kind must NOT gate a fresh quote — that
	// is an ordinary repeat sale to a returning customer.
	t.Run("a_repeat_sale_to_a_returning_customer_is_untouched", func(t *testing.T) {
		w, stop, at := buildQuoteChurnWorld(t)
		defer stop()
		rememberSoldTo(t, w, "josiah", "factor", "ale", at.Add(-time.Hour))

		if _, err := w.Send(sim.SceneQuoteCreate("josiah", []sim.QuoteLineInput{{ItemName: "ale", Qty: 1}}, 4, false, "Roger the factor", nil, at)); err != nil {
			t.Fatalf("selling the same man more of the same must stand: %v", err)
		}
	})
}
