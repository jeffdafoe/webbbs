package perception

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// bale_leftover_golden_test.go — LLM-646. The wares-fetch cue prices what the
// actor HOLDS, converging on the buy directory's structural vendorship
// (eachVendorOffer: whoever holds the good, qty > 0, IS its seller). Before
// this the cue walked only the restock policy, so opportunistic stock — the
// factor-bale leftovers a keeper takes in barter — was priced blind while
// buyers were routed to it off his inventory.
//
// Modelled on the live case (2026-08-25): Josiah Thorne resold 40 bars of
// factor iron at ~3 coins each against a ~5-coin import cost. Iron, salt and
// thread had no policy entries, so his render carried NO line for any of them
// — no band, no paid cost, no below-cost caution — and the only number in the
// trade was the buyer's catalog retail of 3.

func init() {
	perceptionScenarios = append(perceptionScenarios,
		perceptionScenario{
			name: "keeper_prices_bale_leftover_iron",
			summary: "LLM-646 held-goods pricing: Josiah Thorne keeps the General Store with a flour buy entry and stands " +
				"in company holding 2 bars of factor-bale iron he has NO policy entry for. His PriceBook carries both sides " +
				"of the live defect — iron bought from the factor at ~5/bar and resold at ~3/bar. The golden pins the iron " +
				"line rendering with full resale semantics off held stock alone: the catalog band, the paid-cost anchor " +
				"('you have lately paid about 5 coins each'), the realized-sale clause, the below-cost caution, and the " +
				"LLM-627 cost-plus ask ('ask 7 coins or more') sitting ABOVE the 2-to-3 catalog band — pass-through " +
				"pricing from his own books, which no band tune could supply. The flour policy line renders unchanged " +
				"beside it. Cross-scenario guard: TestGoldensEveryHeldPricedGoodRendersAWaresLine.",
			build: keeperPricesBaleLeftoverIron,
		},
	)
}

func keeperPricesBaleLeftoverIron() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		guestID  = sim.ActorID("ezekiel")
		store    = sim.StructureID("general_store")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1200
	now := 600
	published := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "storekeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"flour": 10, "iron": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "flour", Source: sim.RestockSourceBuy, Max: 20},
		}},
	}
	guest := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
	}
	// The live pair of rates: 10 bars bought off a visiting factor for 50 coins
	// (5 each), 6 resold for 18 (3 each). The factor is long gone — only the
	// PriceBook remembers him, which is the point: the anchor follows the goods.
	ironBuys := sim.NewRingBuffer[sim.PriceObservation](4)
	ironBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 50, Qty: 10, Consumers: 1, At: published.Add(-20 * time.Hour)})
	ironSales := sim.NewRingBuffer[sim.PriceObservation](4)
	ironSales.Push(sim.PriceObservation{BuyerID: guestID, Amount: 18, Qty: 6, Consumers: 1, At: published.Add(-16 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, guestID: guest},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, guestID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"flour": {OutputItem: "flour", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 3, RetailPrice: 4},
			"iron":  {OutputItem: "iron", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 2, RetailPrice: 3},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			// Category matters: a CONSUMABLE on a non-recipe buy line is a resale
			// line and makes no SpokenFor claim (means_to_pay.go) — omitting it
			// read the flour as a bench making and reserved all ten sacks.
			"flour": {Name: "flour", Capabilities: []string{"portable"}, DisplayLabel: "Flour",
				DisplayLabelSingular: "sack of flour", DisplayLabelPlural: "sacks of flour",
				Category: sim.ItemCategoryFood},
			"iron": {Name: "iron", Capabilities: []string{"portable"}, DisplayLabel: "Iron",
				DisplayLabelSingular: "bar of iron", DisplayLabelPlural: "bars of iron"},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "factor", Item: "iron"}:  ironBuys,
			{SellerID: josiahID, Item: "iron"}:  ironSales,
			{SellerID: josiahID, Item: "flour"}: sim.NewRingBuffer[sim.PriceObservation](4),
		},
	}
	return snap, josiahID, nil
}

// TestGoldensEveryHeldPricedGoodRendersAWaresLine is the matrix form of the
// LLM-646 property: whenever the wares-fetch section renders for a scenario's
// subject, every kind that subject HOLDS (qty > 0) with a positive catalog
// price must appear in the section — as a priced line, a reservation, an
// earmark, or the wholesale channel, but never silently absent. Silence is the
// defect this ticket fixes: a good the buy directory offers to buyers while
// its holder prices it blind.
func TestGoldensEveryHeldPricedGoodRendersAWaresLine(t *testing.T) {
	const header = "## What your wares fetch"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			actorSnap := snap.Actors[actorID]
			if actorSnap == nil {
				t.Skip("scenario subject absent")
			}
			v := buildTradeValue(snap, actorID, actorSnap, actorSnap.CurrentHuddleID != "")
			if v == nil || len(v.Items) == 0 {
				return // cue absent for this scenario — nothing to check
			}
			var b strings.Builder
			renderTradeValue(&b, v)
			section := b.String()
			if !strings.Contains(section, header) {
				return
			}
			for kind, qty := range actorSnap.Inventory {
				if qty <= 0 {
					continue
				}
				recipe := snap.Recipes[kind]
				if recipe == nil || (recipe.RetailPrice <= 0 && recipe.WholesalePrice <= 0) {
					continue // unpriced — legitimately silent
				}
				label := itemDisplayLabel(snap, kind)
				if !strings.Contains(section, label) {
					t.Errorf("held priced good %q (label %q) has no line in the wares section:\n%s", kind, label, section)
				}
			}
		})
	}
}
