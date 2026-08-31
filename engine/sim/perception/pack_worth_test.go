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
			{Noun: "iron ingots", Qty: 11, Worth: 6},
			{Noun: "salt", Qty: 5, Worth: 2},
			{Noun: "silver_locket", Qty: 2, Worth: 0},
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

	t.Run("keeper's realized sales beat the catalog seed", func(t *testing.T) {
		snap := packWorthSnap(published)
		// The shop has been getting 10 an ingot from its counter — that, not the
		// 6-coin catalog seed, is the honest ceiling on what buying more is worth.
		ironSales := sim.NewRingBuffer[sim.PriceObservation](8)
		ironSales.Push(sim.PriceObservation{BuyerID: "ezekiel", Amount: 30, Qty: 3, Consumers: 1, At: published.Add(-24 * time.Hour)})
		snap.PriceBook[sim.PriceBookKey{SellerID: keeperID, Item: "iron"}] = ironSales
		seller := &sim.ActorSnapshot{Inventory: map[sim.ItemKind]int{"iron": 4}}
		got := buildPackGoods(snap, keeperID, seller)
		if len(got) != 1 || got[0].Worth != 10 {
			t.Fatalf("got %+v, want one iron entry at realized worth 10", got)
		}
	})

	t.Run("empty pack builds nothing", func(t *testing.T) {
		snap := packWorthSnap(published)
		if got := buildPackGoods(snap, keeperID, &sim.ActorSnapshot{}); got != nil {
			t.Fatalf("got %+v, want nil for an empty pack", got)
		}
	})
}

// TestRenderPackGoodsCounselOnlyWhenPriced — the weigh-his-asks counsel exists to
// point at figures in the same listing; an all-unpriced pack has none, so the
// counsel must not render (there is nothing to weigh against).
func TestRenderPackGoodsCounselOnlyWhenPriced(t *testing.T) {
	const counsel = "Weigh his asking prices"
	var priced strings.Builder
	renderPackGoods(&priced, []PackGood{{Noun: "iron ingots", Qty: 4, Worth: 10}})
	if !strings.Contains(priced.String(), counsel) {
		t.Errorf("priced pack render lacks the counsel: %q", priced.String())
	}
	if !strings.Contains(priced.String(), "4 iron ingots (fetches about 10 each here)") {
		t.Errorf("priced pack render lacks the worth figure: %q", priced.String())
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
