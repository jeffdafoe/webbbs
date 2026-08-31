package perception

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// pack_worth_test.go — LLM-647. The pack-worth half of the keeper's "## A
// trader's come to deal" cue: buildPackGoods prices a selling visitor's pack for
// the keeper (realized-first, catalog seed fallback, silence for the unpriced),
// and renderPackGoods writes the listing with the weigh-his-asks counsel only
// when at least one good priced. The golden distributor_views_factor pins the
// full rendered shape; these tests pin the resolution order and the arms the
// golden's single fixture can't hold at once.

// packWorthSnap builds a minimal snapshot for buildPackGoods: a keeper with
// optional realized sales history, and a catalog seed for iron and salt.
func packWorthSnap(published time.Time) *sim.Snapshot {
	return &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"iron": {WholesalePrice: 6},
			"salt": {WholesalePrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"iron": {Name: "iron", DisplayLabel: "Iron", DisplayLabelSingular: "iron ingot", DisplayLabelPlural: "iron ingots"},
			"salt": {Name: "salt", DisplayLabel: "Salt", DisplayLabelSingular: "salt", DisplayLabelPlural: "salt"},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{},
	}
}

func TestBuildPackGoods(t *testing.T) {
	published := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	const keeperID = sim.ActorID("josiah")

	t.Run("catalog seed, sorted, unpriced named without figure", func(t *testing.T) {
		snap := packWorthSnap(published)
		seller := &sim.ActorSnapshot{Inventory: map[sim.ItemKind]int{
			"iron":          11,
			"salt":          5,
			"silver_locket": 2, // no recipe, no history — Worth must stay 0
			"thread":        0, // zero-qty entries never list
		}}
		got := buildPackGoods(snap, keeperID, seller)
		want := []PackGood{
			{Noun: "iron ingots", Qty: 11, Worth: 6, Source: worthFromCatalog},
			{Noun: "salt", Qty: 5, Worth: 2, Source: worthFromCatalog},
			{Noun: "silver_locket", Qty: 2, Worth: 0, Source: worthNone},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d goods, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("good %d: got %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	// The resolution ladder, pinned rung by rung (code_review, LLM-647): sales
	// beat purchases beat catalog, and each rung carries its provenance so
	// render can word it honestly.
	t.Run("keeper's realized sales beat purchases and catalog", func(t *testing.T) {
		snap := packWorthSnap(published)
		// The shop has been getting 10 an ingot from its counter — that, not the
		// 12 it once paid a factor nor the 6-coin catalog seed, is the honest
		// ceiling on what buying more is worth.
		ironSales := sim.NewRingBuffer[sim.PriceObservation](8)
		ironSales.Push(sim.PriceObservation{BuyerID: "ezekiel", Amount: 30, Qty: 3, Consumers: 1, At: published.Add(-24 * time.Hour)})
		snap.PriceBook[sim.PriceBookKey{SellerID: keeperID, Item: "iron"}] = ironSales
		ironBuys := sim.NewRingBuffer[sim.PriceObservation](8)
		ironBuys.Push(sim.PriceObservation{BuyerID: keeperID, Amount: 24, Qty: 2, Consumers: 1, At: published.Add(-30 * time.Hour)})
		snap.PriceBook[sim.PriceBookKey{SellerID: "vstr-x", Item: "iron"}] = ironBuys
		seller := &sim.ActorSnapshot{Inventory: map[sim.ItemKind]int{"iron": 4}}
		got := buildPackGoods(snap, keeperID, seller)
		if len(got) != 1 || got[0].Worth != 10 || got[0].Source != worthFromSales {
			t.Fatalf("got %+v, want one iron entry at realized sale worth 10 (worthFromSales)", got)
		}
	})

	t.Run("purchase history with no sales beats catalog, marked as a purchase", func(t *testing.T) {
		snap := packWorthSnap(published)
		// No sales of iron — only what he paid a visitor for it. The figure wins
		// over the catalog seed but must carry purchase provenance, because it
		// may BE a past overpayment and render must not call it a realization.
		ironBuys := sim.NewRingBuffer[sim.PriceObservation](8)
		ironBuys.Push(sim.PriceObservation{BuyerID: keeperID, Amount: 24, Qty: 2, Consumers: 1, At: published.Add(-30 * time.Hour)})
		snap.PriceBook[sim.PriceBookKey{SellerID: "vstr-x", Item: "iron"}] = ironBuys
		seller := &sim.ActorSnapshot{Inventory: map[sim.ItemKind]int{"iron": 4}}
		got := buildPackGoods(snap, keeperID, seller)
		if len(got) != 1 || got[0].Worth != 12 || got[0].Source != worthFromPurchases {
			t.Fatalf("got %+v, want one iron entry at purchase worth 12 (worthFromPurchases)", got)
		}
	})

	t.Run("empty pack builds nothing", func(t *testing.T) {
		snap := packWorthSnap(published)
		if got := buildPackGoods(snap, keeperID, &sim.ActorSnapshot{}); got != nil {
			t.Fatalf("got %+v, want nil for an empty pack", got)
		}
	})
}

// TestRenderPackGoodsProvenanceWording — each provenance gets its own honest
// claim (code_review, LLM-647): a sale-derived figure is a counter realization,
// a purchase-derived figure is only what he has been paying (never "fetches"),
// a catalog figure is the going rate. The counsel renders only when at least one
// figure is present — an all-unpriced pack has nothing to weigh against.
func TestRenderPackGoodsProvenanceWording(t *testing.T) {
	const counsel = "Weigh his asking prices"
	var priced strings.Builder
	renderPackGoods(&priced, []PackGood{
		{Noun: "iron ingots", Qty: 4, Worth: 10, Source: worthFromSales},
		{Noun: "thread", Qty: 12, Worth: 2, Source: worthFromPurchases},
		{Noun: "salt", Qty: 5, Worth: 2, Source: worthFromCatalog},
	})
	out := priced.String()
	if !strings.Contains(out, counsel) {
		t.Errorf("priced pack render lacks the counsel: %q", out)
	}
	if !strings.Contains(out, "4 iron ingots (fetches about 10 each from your counter)") {
		t.Errorf("sale-derived figure lacks the counter-realization wording: %q", out)
	}
	if !strings.Contains(out, "12 thread (you've been paying about 2 each)") {
		t.Errorf("purchase-derived figure lacks the paying wording: %q", out)
	}
	if strings.Contains(out, "thread (fetches") {
		t.Errorf("purchase-derived figure must never claim a counter realization: %q", out)
	}
	if !strings.Contains(out, "5 salt (worth about 2 at the going rate)") {
		t.Errorf("catalog figure lacks the going-rate wording: %q", out)
	}
	var unpriced strings.Builder
	renderPackGoods(&unpriced, []PackGood{{Noun: "silver_locket", Qty: 2}})
	if strings.Contains(unpriced.String(), counsel) {
		t.Errorf("all-unpriced pack render must not carry the counsel: %q", unpriced.String())
	}
	if !strings.Contains(unpriced.String(), "2 silver_locket") {
		t.Errorf("all-unpriced pack render must still name the goods: %q", unpriced.String())
	}
}

// TestGoldensPackListingOnlyInTraderCue — cross-scenario invariant: the pack
// listing is a clause of the keeper's "## A trader's come to deal" cue and must
// never render outside it.
func TestGoldensPackListingOnlyInTraderCue(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if strings.Contains(out, "In his pack:") && !strings.Contains(out, "## A trader's come to deal") {
				t.Errorf("scenario %q: pack listing rendered outside the trader cue", sc.name)
			}
		})
	}
}
