package perception

import (
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

func tradeValueRecipes() map[sim.ItemKind]*sim.ItemRecipe {
	return map[sim.ItemKind]*sim.ItemRecipe{
		"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
		"water":   {OutputItem: "water", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
		"stew":    {OutputItem: "stew", OutputQty: 10, RateQty: 30, RatePerHours: 6, WholesalePrice: 3, RetailPrice: 5},
	}
}

func tvProducePolicy(items ...sim.ItemKind) *sim.RestockPolicy {
	var entries []sim.RestockEntry
	for _, it := range items {
		entries = append(entries, sim.RestockEntry{Item: it, Source: sim.RestockSourceProduce, Max: 20})
	}
	return &sim.RestockPolicy{Restock: entries}
}

// TestBuildTradeValue_OwnTradeRangeOnly: a producer in company sees the wholesale–
// retail range for the goods of ITS OWN trade, and nothing for a priced good it
// does not produce (own-trade-only — the no-omniscience guard).
func TestBuildTradeValue_OwnTradeRangeOnly(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("nail", "skillet")}
	snap := &sim.Snapshot{
		Actors:  map[sim.ActorID]*sim.ActorSnapshot{"ezekiel": subj},
		Recipes: tradeValueRecipes(),
	}
	v := buildTradeValue(snap, "ezekiel", subj, true)
	if v == nil || len(v.Items) != 2 {
		t.Fatalf("want 2 own-trade items, got %+v", v)
	}
	if v.Items[0].itemKind != "nail" || v.Items[0].Low != 1 || v.Items[0].High != 2 {
		t.Errorf("nail item wrong: %+v", v.Items[0])
	}
	if v.Items[1].itemKind != "skillet" || v.Items[1].Low != 5 || v.Items[1].High != 10 {
		t.Errorf("skillet item wrong: %+v", v.Items[1])
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	for _, want := range []string{"## What your wares fetch", "nail: 1 to 2 coins each.", "skillet: 5 to 10 coins each."} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	// stew is priced but not produced by this actor — must not be valued.
	if strings.Contains(out, "stew") {
		t.Errorf("stew leaked into wares-worth (not own-trade):\n%s", out)
	}
}

// TestBuildTradeValue_SinglePriceCollapse: a good whose wholesale == retail renders
// as a single amount ("1 coin each"), not a degenerate "1 to 1" range.
func TestBuildTradeValue_SinglePriceCollapse(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("water")}
	snap := &sim.Snapshot{
		Actors:  map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
		Recipes: tradeValueRecipes(),
	}
	v := buildTradeValue(snap, "a", subj, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].Low != 1 || v.Items[0].High != 1 {
		t.Fatalf("want single water item 1/1, got %+v", v)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	if !strings.Contains(out, "water: 1 coin each.") {
		t.Errorf("single-price water should read '1 coin each':\n%s", out)
	}
	if strings.Contains(out, "water: 1 to") {
		t.Errorf("single-price water should not render a range:\n%s", out)
	}
}

// TestBuildTradeValue_RecentPriceClause: when the actor has its own coin sales of a
// good in the weekly window, render appends the rounded recent realized unit price.
// 7 coins over 2 units rounds to 4 each.
func TestBuildTradeValue_RecentPriceClause(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("nail")}
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	pb := sim.NewRingBuffer[sim.PriceObservation](4)
	pb.Push(sim.PriceObservation{BuyerID: "buyer", Amount: 7, Qty: 2, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"ezekiel": subj},
		Recipes:     tradeValueRecipes(),
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ezekiel", Item: "nail"}: pb,
		},
	}
	v := buildTradeValue(snap, "ezekiel", subj, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].RecentUnit != 4 {
		t.Fatalf("want nail recentUnit=4, got %+v", v)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "of late you have sold for about 4 coins each") {
		t.Errorf("missing recent-price clause:\n%s", b.String())
	}
}

// TestBuildTradeValue_PriceNormalization locks the wholesale/retail normalization
// edge cases: a missing wholesale or retail collapses to the single configured
// price, a reversed config is sorted into a range, and a good with no price at all
// is dropped entirely.
func TestBuildTradeValue_PriceNormalization(t *testing.T) {
	tests := []struct {
		name      string
		wholesale int
		retail    int
		wantNil   bool
		wantLow   int
		wantHigh  int
	}{
		{name: "normal range", wholesale: 1, retail: 2, wantLow: 1, wantHigh: 2},
		{name: "wholesale missing", wholesale: 0, retail: 5, wantLow: 5, wantHigh: 5},
		{name: "retail missing", wholesale: 5, retail: 0, wantLow: 5, wantHigh: 5},
		{name: "reversed", wholesale: 10, retail: 5, wantLow: 5, wantHigh: 10},
		{name: "both missing", wholesale: 0, retail: 0, wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("x")}
			snap := &sim.Snapshot{
				Actors: map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
				Recipes: map[sim.ItemKind]*sim.ItemRecipe{
					"x": {OutputItem: "x", WholesalePrice: tt.wholesale, RetailPrice: tt.retail},
				},
			}
			v := buildTradeValue(snap, "a", subj, true)
			if tt.wantNil {
				if v != nil {
					t.Fatalf("want nil (no priced own-trade good), got %+v", v)
				}
				return
			}
			if v == nil || len(v.Items) != 1 {
				t.Fatalf("want one item, got %+v", v)
			}
			if got := v.Items[0]; got.Low != tt.wantLow || got.High != tt.wantHigh {
				t.Fatalf("range = %d..%d, want %d..%d", got.Low, got.High, tt.wantLow, tt.wantHigh)
			}
		})
	}
}

// TestBuildTradeValue_NotInCompanyNil: alone, there is no one to trade with, so the
// cue is suppressed regardless of own-trade goods.
func TestBuildTradeValue_NotInCompanyNil(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("nail")}
	snap := &sim.Snapshot{
		Actors:  map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
		Recipes: tradeValueRecipes(),
	}
	if v := buildTradeValue(snap, "a", subj, false); v != nil {
		t.Errorf("alone (not in company) should yield nil, got %+v", v)
	}
}

// TestBuildTradeValue_NoOwnTradeNil: the cue is nil when there is no priced own ware
// to value — a buy good with no catalog recipe (ale is unpriced here) yields nothing,
// as does a nil RestockPolicy. A priced resold good IS valued now (LLM-191) — that
// positive case is TestBuildTradeValue_ResoldGoodCostBasis.
func TestBuildTradeValue_NoOwnTradeNil(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("ale", 20)}
	snap := &sim.Snapshot{
		Actors:  map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
		Recipes: tradeValueRecipes(),
	}
	if v := buildTradeValue(snap, "a", subj, true); v != nil {
		t.Errorf("unpriced buy good should yield nil, got %+v", v)
	}
	if v := buildTradeValue(snap, "a", &sim.ActorSnapshot{}, true); v != nil {
		t.Errorf("nil RestockPolicy should yield nil, got %+v", v)
	}
}

// TestBuildTradeValue_ResoldGoodCostBasis: a pure reseller (all-buy restock) in
// company sees its resold goods valued from the recipe range AND its own recent
// per-unit purchase cost (LLM-191). 8 coins over 4 units rounds to 2 each; with no
// sale history the "sold for" clause is absent.
func TestBuildTradeValue_ResoldGoodCostBasis(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("cheese", 10)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "cheese"}: buys,
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 resold item, got %+v", v)
	}
	if got := v.Items[0]; got.itemKind != "cheese" || got.Low != 3 || got.High != 6 || got.PaidUnit != 2 || got.RecentUnit != 0 || got.AtOrBelowCost || got.AskUnit != 6 {
		t.Fatalf("cheese item wrong: %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	// LLM-385: with a purchase cost but NO realized sale on record, we can't say the
	// good is being sold at or below cost — so the caution no longer fires. LLM-627:
	// a not-yet-sold resale line DOES carry the ask anchor — before the first sale is
	// exactly when the anchor is needed most; paid 2 → cost-plus 3, clamped up to
	// retail 6.
	if !strings.Contains(out, "cheese: 3 to 6 coins each; you have lately paid about 2 coins each for it — ask 6 coins or more at your counter; your shop lives on the difference.") {
		t.Errorf("missing cost-basis clause with ask anchor:\n%s", out)
	}
	if strings.Contains(out, "selling below your costs") {
		t.Errorf("no realized sale — below-cost caution must not fire (LLM-385):\n%s", out)
	}
	if strings.Contains(out, "sold for") {
		t.Errorf("no sale history — should not render a sold-for clause:\n%s", out)
	}
}

// TestBuildTradeValue_ResoldGoodBothClauses: a resold good the actor has BOTH bought
// (cost basis) and sold (realized price) renders both clauses — paid then sold — so
// the pair brackets the markup (LLM-191, the Q3 decision). Buy 8/4 = 2, sale 20/4 = 5.
func TestBuildTradeValue_ResoldGoodBothClauses(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("cheese", 10)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-24 * time.Hour)})
	sales := sim.NewRingBuffer[sim.PriceObservation](4)
	sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: 20, Qty: 4, Consumers: 1, At: published.Add(-12 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "cheese"}: buys,  // josiah as buyer (cost)
			{SellerID: "josiah", Item: "cheese"}:     sales, // josiah as seller (realized)
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.PaidUnit != 2 || got.RecentUnit != 5 || got.AtOrBelowCost || got.StrictlyBelowCost {
		t.Fatalf("want PaidUnit=2 RecentUnit=5 not-below-cost, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	// LLM-292/191: paid then sold, in order — the pair brackets the markup.
	// LLM-385: a healthy markup (sold 5 > paid 2) is NOT sold at or below cost, so the
	// caution no longer fires AT ALL — the paid/sold pair renders as a bare fact. This
	// is the boilerplate fix: pre-LLM-385 every resold line carried the caution.
	if !strings.Contains(b.String(), "you have lately paid about 2 coins each for it; of late you have sold for about 5 coins each.") {
		t.Errorf("want paid-then-sold clauses with NO below-cost caution:\n%s", b.String())
	}
	if strings.Contains(b.String(), "selling below your costs") {
		t.Errorf("healthy markup must not get the below-cost caution (LLM-385):\n%s", b.String())
	}
	if strings.Contains(b.String(), "negotiate lower costs or raise your price") {
		t.Errorf("healthy markup should not get the underwater lever hint:\n%s", b.String())
	}
	// LLM-627: a line already earning its markup (sold 5 ≥ cost-plus 3) carries no
	// ask nudge either — suppression compares the raw sale rate against cost-plus,
	// not the retail-clamped ask, so a profitable under-retail line stays clean.
	if strings.Contains(b.String(), "ask ") {
		t.Errorf("healthy markup must not get the ask nudge (LLM-627):\n%s", b.String())
	}
}

// TestBuildTradeValue_ResoldGoodUnderwater: a resold good whose realized sale price sits
// BELOW its realized buy cost (RecentUnit < PaidUnit) is demonstrably underwater, so
// render escalates past the bare caution to name the two levers a merchant holds — buy
// cheaper or charge more (LLM-332). The live case that motivated it: Josiah the
// distributor's milk, bought ~2, sold ~1, which he rationally cut rather than carry at a
// loss. Buy 8/4 = 2, sale 4/4 = 1.
func TestBuildTradeValue_ResoldGoodUnderwater(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("milk", 10)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-24 * time.Hour)})
	sales := sim.NewRingBuffer[sim.PriceObservation](4)
	sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: 4, Qty: 4, Consumers: 1, At: published.Add(-12 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", WholesalePrice: 1, RetailPrice: 2},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "milk"}: buys,  // josiah as buyer (cost)
			{SellerID: "josiah", Item: "milk"}:     sales, // josiah as seller (realized)
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.PaidUnit != 2 || got.RecentUnit != 1 || !got.AtOrBelowCost || !got.StrictlyBelowCost {
		t.Fatalf("want PaidUnit=2 RecentUnit=1 below-cost, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	// LLM-332 named the two levers; LLM-627 replaces that advisory with the lever
	// made concrete — the cost-anchored ask. Paid 2 → cost-plus 3, above retail 2,
	// so the pass-through arm carries the number.
	if !strings.Contains(out, "of late you have sold for about 1 coin each — selling below your costs loses you coin; ask 3 coins or more at your counter.") {
		t.Errorf("want underwater caution with concrete ask:\n%s", out)
	}
	if strings.Contains(out, "negotiate lower costs or raise your price") {
		t.Errorf("ask anchor must replace the vague two-lever advisory (LLM-627):\n%s", out)
	}
}

// TestBuildTradeValue_ResoldGoodSubCoinUnderwater is the LLM-385 crux: a good whose
// loss is smaller than a whole coin. Bought at 1.39/unit (139 coins / 100 units) and
// sold at 1.30/unit (130 / 100), BOTH round to "about 1 coin each" for display — so
// the old whole-coin RecentUnit < PaidUnit test (1 < 1) saw break-even and stayed
// silent. The sub-coin cross-multiplication catches it: 130*100 < 139*100, so the
// caution AND the two-lever escalation both fire despite the identical displayed
// numbers. This is the milk/carrots bleed that emptied Josiah's purse.
func TestBuildTradeValue_ResoldGoodSubCoinUnderwater(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("milk", 10)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 139, Qty: 100, Consumers: 1, At: published.Add(-24 * time.Hour)})
	sales := sim.NewRingBuffer[sim.PriceObservation](4)
	sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: 130, Qty: 100, Consumers: 1, At: published.Add(-12 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", WholesalePrice: 1, RetailPrice: 2},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "milk"}: buys,  // josiah as buyer (cost)
			{SellerID: "josiah", Item: "milk"}:     sales, // josiah as seller (realized)
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	// Both DISPLAY as 1 coin (rounded), but the sub-coin comparison flags the loss.
	if got := v.Items[0]; got.PaidUnit != 1 || got.RecentUnit != 1 || !got.AtOrBelowCost || !got.StrictlyBelowCost {
		t.Fatalf("want display 1/1 but below-cost flags set, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	// LLM-627: the ask replaces the two-lever advisory. Paid displays 1 → cost-plus
	// 2, which equals retail — "ask 2".
	if !strings.Contains(b.String(), "of late you have sold for about 1 coin each — selling below your costs loses you coin; ask 2 coins or more at your counter.") {
		t.Errorf("sub-coin loss must fire caution + concrete ask despite 1/1 display:\n%s", b.String())
	}
}

// TestBuildTradeValue_ResoldGoodAtCost pins the "at or below" boundary (LLM-385): a
// good sold at EXACTLY its buy rate (paid 2, sold 2) gets the bare caution — no margin
// is a leak once overhead is counted — but NOT the two-lever escalation, which is for
// a strict loss. Buy 8/4 = 2, sale 8/4 = 2.
func TestBuildTradeValue_ResoldGoodAtCost(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("cheese", 10)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-24 * time.Hour)})
	sales := sim.NewRingBuffer[sim.PriceObservation](4)
	sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-12 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "cheese"}: buys,
			{SellerID: "josiah", Item: "cheese"}:     sales,
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; !got.AtOrBelowCost || got.StrictlyBelowCost {
		t.Fatalf("at-cost should set AtOrBelowCost but not StrictlyBelowCost, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	// LLM-627: at-cost is a margin bleed, so the caution now carries the concrete
	// ask (paid 2 → cost-plus 3, clamped up to retail 6) instead of standing bare.
	if !strings.Contains(out, "selling below your costs loses you coin; ask 6 coins or more at your counter.") {
		t.Errorf("at-cost should carry the caution with the ask anchor:\n%s", out)
	}
	if strings.Contains(out, "negotiate lower costs or raise your price") {
		t.Errorf("at-cost (not strictly below) must not get the two-lever hint:\n%s", out)
	}
}

// TestBuildTradeValue_NoSpreadGood pins the LLM-627 no-spread gates. A good with
// no authored spread (wholesale = retail — water, carrots) NEVER gets the ask
// anchor: integer coins put retail at the floor already, and a higher ask is the
// documented wrong fix (water at 2 breaks the nail and porridge chains). The
// below-cost caution splits: BREAKEVEN at the floor is the line's ordinary state
// and stays silent (pre-627 it fired every turn as unactionable noise), but a
// STRICT loss — the good bought above its fixed price — still warns, with the
// advisory narrowed to the buy-side lever ("negotiate lower costs"), never
// "raise your price".
func TestBuildTradeValue_NoSpreadGood(t *testing.T) {
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	newSnap := func(buyAmt, buyQty, saleAmt, saleQty int) *sim.Snapshot {
		buys := sim.NewRingBuffer[sim.PriceObservation](4)
		buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: buyAmt, Qty: buyQty, Consumers: 1, At: published.Add(-24 * time.Hour)})
		sales := sim.NewRingBuffer[sim.PriceObservation](4)
		sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: saleAmt, Qty: saleQty, Consumers: 1, At: published.Add(-12 * time.Hour)})
		return &sim.Snapshot{
			PublishedAt: published,
			Recipes: map[sim.ItemKind]*sim.ItemRecipe{
				"water": {OutputItem: "water", WholesalePrice: 1, RetailPrice: 1},
			},
			PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
				{SellerID: "mill", Item: "water"}:   buys,  // josiah as buyer (cost)
				{SellerID: "josiah", Item: "water"}: sales, // josiah as seller (realized)
			},
		}
	}
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("water", 20)}
	// Breakeven at the floor (bought 1, sold 1) — the ordinary state: silent.
	v := buildTradeValue(newSnap(4, 4, 4, 4), "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.AtOrBelowCost || got.StrictlyBelowCost || got.AskUnit != 0 {
		t.Fatalf("no-spread breakeven must carry no caution flags and no ask, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if out := b.String(); strings.Contains(out, "selling below your costs") || strings.Contains(out, "ask ") {
		t.Errorf("no-spread breakeven must render neither caution nor ask (LLM-627):\n%s", out)
	}
	// Strict loss (bought 3, sold 1) — a real bleed: caution fires, advisory
	// narrowed to the buy lever, still no ask.
	v2 := buildTradeValue(newSnap(12, 4, 4, 4), "josiah", subj, true)
	if got := v2.Items[0]; !got.AtOrBelowCost || !got.StrictlyBelowCost || got.AskUnit != 0 {
		t.Fatalf("no-spread strict loss must flag below-cost with no ask, got %+v", got)
	}
	var b2 strings.Builder
	renderTradeValue(&b2, v2)
	out2 := b2.String()
	if !strings.Contains(out2, "selling below your costs loses you coin; you may need to negotiate lower costs.") {
		t.Errorf("no-spread strict loss wants the buy-lever-only advisory:\n%s", out2)
	}
	if strings.Contains(out2, "raise your price") || strings.Contains(out2, "ask ") {
		t.Errorf("no-spread good must never be told to raise its price (LLM-627):\n%s", out2)
	}
}

// TestBuildTradeValue_AskProportionalMarkup pins the LLM-627 markup arithmetic on a
// dear good: the markup is ceil(paid/4), not a flat coin, so a 20-coin cost asks 25
// — and cost-plus above the catalog retail wins the clamp (pass-through: upstream
// price rises raise the ask rather than being eaten). No sale history, so the ask
// renders unsuppressed.
func TestBuildTradeValue_AskProportionalMarkup(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("coat", 4)}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 40, Qty: 2, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"coat": {OutputItem: "coat", WholesalePrice: 9, RetailPrice: 15},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "factor", Item: "coat"}: buys,
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	// Paid 40/2 = 20; markup ceil(20/4) = 5; cost-plus 25 > retail 15 → ask 25.
	if got := v.Items[0]; got.PaidUnit != 20 || got.AskUnit != 25 {
		t.Fatalf("want PaidUnit=20 AskUnit=25, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "you have lately paid about 20 coins each for it — ask 25 coins or more at your counter; your shop lives on the difference.") {
		t.Errorf("want proportional pass-through ask on a dear good:\n%s", b.String())
	}
}

// TestBuildTradeValue_ResoldGoodFreeAcquisition pins the LLM-385 zero-cost guard: a
// good acquired for FREE (barter, paidCoins 0) is NOT flagged below cost even when it is
// also sold for free — 0 <= 0 must not read as "at or below cost" when there is no cost
// basis. But a zero-coin SALE against a POSITIVE paid cost still warns (a real give-away
// loss). Requires the paidCoins > 0 gate in buildTradeValue.
func TestBuildTradeValue_ResoldGoodFreeAcquisition(t *testing.T) {
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	newSnap := func(buyAmt, buyQty, saleAmt, saleQty int) *sim.Snapshot {
		buys := sim.NewRingBuffer[sim.PriceObservation](4)
		buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: buyAmt, Qty: buyQty, Consumers: 1, At: published.Add(-24 * time.Hour)})
		sales := sim.NewRingBuffer[sim.PriceObservation](4)
		sales.Push(sim.PriceObservation{BuyerID: "martha", Amount: saleAmt, Qty: saleQty, Consumers: 1, At: published.Add(-12 * time.Hour)})
		return &sim.Snapshot{
			PublishedAt: published,
			Recipes:     map[sim.ItemKind]*sim.ItemRecipe{"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6}},
			PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
				{SellerID: "ellis_farm", Item: "cheese"}: buys,  // josiah as buyer (cost)
				{SellerID: "josiah", Item: "cheese"}:     sales, // josiah as seller (realized)
			},
		}
	}
	subj := &sim.ActorSnapshot{RestockPolicy: buyPolicy("cheese", 10)}
	// Free in (0 coins / 4 units), free out (0 coins / 4 units) → no cost basis, no caution.
	v := buildTradeValue(newSnap(0, 4, 0, 4), "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.AtOrBelowCost || got.StrictlyBelowCost {
		t.Fatalf("free-in/free-out must not flag below cost, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if strings.Contains(b.String(), "selling below your costs") {
		t.Errorf("free acquisition must carry no below-cost caution:\n%s", b.String())
	}
	// A zero-coin give-away against a POSITIVE paid cost (8/4 = 2) IS a loss → warn.
	v2 := buildTradeValue(newSnap(8, 4, 0, 4), "josiah", subj, true)
	if got := v2.Items[0]; !got.AtOrBelowCost || !got.StrictlyBelowCost {
		t.Fatalf("giving away a good that cost coin must flag below cost, got %+v", got)
	}
}

// TestBuildTradeValue_ProducedCostFromWholesale: a produced good with recipe inputs
// and no purchase history prices its makings from the inputs' catalog wholesale
// (LLM-226). Porridge-shaped: 10 bowls from 3 milk (wholesale 1) + 5 water
// (wholesale 1) = 8 coins a batch → "nearly 1 coin each" (0.8 rounds UP in prose,
// never down to a misleading "about 1").
func TestBuildTradeValue_ProducedCostFromWholesale(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("porridge")}
	snap := &sim.Snapshot{
		Actors: map[sim.ActorID]*sim.ActorSnapshot{"hannah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", OutputQty: 1, WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", OutputQty: 1, WholesalePrice: 1, RetailPrice: 1},
		},
	}
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 8 || got.CostQty != 10 || got.CostFloor {
		t.Fatalf("want CostBatch=8 CostQty=10 floor=false, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you nearly 1 coin each.") {
		t.Errorf("missing makings-cost clause:\n%s", b.String())
	}
}

// TestBuildTradeValue_ProducedCostPurchaseHistoryWins: an input the producer has
// actually been buying is priced from its own purchases, not the catalog — 12 coins
// over 6 milk = 2 each beats the 1-coin wholesale. 3×2 + 5×1 = 11 per 10 → "a
// little over 1 coin each".
func TestBuildTradeValue_ProducedCostPurchaseHistoryWins(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("porridge")}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "hannah", Amount: 12, Qty: 6, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"hannah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", OutputQty: 1, WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", OutputQty: 1, WholesalePrice: 1, RetailPrice: 1},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "dairy", Item: "milk"}: buys,
		},
	}
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].CostBatch != 11 {
		t.Fatalf("want CostBatch=11 (history-priced milk), got %+v", v)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you a little over 1 coin each") {
		t.Errorf("missing history-priced makings clause:\n%s", b.String())
	}
}

// TestBuildTradeValue_ProducedCostCheapBulkHistoryNotFree: purchase history whose
// per-unit cost rounds below 1 coin (1 coin for 10 milk) must NOT price the input as
// free — history prices by ceiling division, so the input still costs 1 each and the
// makings clause renders. Pins the code_review blocking case: nearest-rounding here
// silently erased a known positive cost, the exact understatement the clause guards
// against. 3 milk x 1 + 5 water x 1 (wholesale) = 8 per 10.
func TestBuildTradeValue_ProducedCostCheapBulkHistoryNotFree(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("porridge")}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "hannah", Amount: 1, Qty: 10, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"hannah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", OutputQty: 1, WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", OutputQty: 1, WholesalePrice: 1, RetailPrice: 1},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "dairy", Item: "milk"}: buys,
		},
	}
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 8 || got.CostFloor {
		t.Fatalf("cheap bulk history must ceil to 1/unit (CostBatch=8, no floor), got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you nearly 1 coin each") {
		t.Errorf("makings clause must not vanish on cheap bulk history:\n%s", b.String())
	}
}

// TestBuildTradeValue_ProducedCostUnpriceableInputFloor: an input with no purchase
// history and no catalog price is left out of the sum, and the clause is qualified
// as a floor ("at least") rather than silently understating the cost.
func TestBuildTradeValue_ProducedCostUnpriceableInputFloor(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("stew2")}
	snap := &sim.Snapshot{
		Actors: map[sim.ActorID]*sim.ActorSnapshot{"john": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"stew2": {OutputItem: "stew2", OutputQty: 1, WholesalePrice: 3, RetailPrice: 5,
				Inputs: []sim.RecipeInput{{Item: "meat", Qty: 2}, {Item: "mystery", Qty: 1}}},
			"meat": {OutputItem: "meat", OutputQty: 1, WholesalePrice: 3, RetailPrice: 6},
			// "mystery" has no recipe row at all — unpriceable.
		},
	}
	v := buildTradeValue(snap, "john", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 6 || !got.CostFloor {
		t.Fatalf("want CostBatch=6 floor=true, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you at least 6 coins each") {
		t.Errorf("missing floor-qualified makings clause:\n%s", b.String())
	}
}

// TestBuildTradeValue_ProducedCostFractionalFloor: when the priced part of the cost
// is itself fractional (1 coin of priced inputs per 10-unit batch) AND an unpriceable
// input flags the floor, the "at least" phrase uses the whole-coin CEILING of the
// known part — "at least 1 coin each", not "at least 0". Deliberately a conservative
// warning rather than a strict mathematical floor (the known floor is 0.1/unit): the
// clause guards against underpricing, and the true cost includes the unpriced input
// anyway. Pins the behavior as intentional (code_review round 1).
func TestBuildTradeValue_ProducedCostFractionalFloor(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("gruel")}
	snap := &sim.Snapshot{
		Actors: map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"gruel": {OutputItem: "gruel", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}, {Item: "mystery", Qty: 1}}},
			"water": {OutputItem: "water", OutputQty: 1, WholesalePrice: 1, RetailPrice: 1},
			// "mystery" has no recipe row — unpriceable, flags the floor.
		},
	}
	v := buildTradeValue(snap, "a", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 1 || got.CostQty != 10 || !got.CostFloor {
		t.Fatalf("want CostBatch=1 CostQty=10 floor=true, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you at least 1 coin each") {
		t.Errorf("fractional floor should ceil to 'at least 1 coin each':\n%s", b.String())
	}
}

// TestBuildTradeValue_NoInputsNoCostClause: a produced origin good (empty inputs)
// carries no makings clause — there is no ingredient cost to speak of.
func TestBuildTradeValue_NoInputsNoCostClause(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("nail")}
	snap := &sim.Snapshot{
		Actors:  map[sim.ActorID]*sim.ActorSnapshot{"a": subj},
		Recipes: tradeValueRecipes(),
	}
	v := buildTradeValue(snap, "a", subj, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].CostBatch != 0 {
		t.Fatalf("origin good should carry no batch cost, got %+v", v)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if strings.Contains(b.String(), "makings") {
		t.Errorf("origin good should render no makings clause:\n%s", b.String())
	}
}

// tvMakingsSnap builds a one-produced-good fixture for the LLM-475 makings tests:
// `out` is made from `inputs`, the actor's realized sales of it are (saleCoins over
// saleUnits) inside the window, and toolInputs are added to the recipe as durable
// tool kinds. Inputs price off the catalog (no purchase history) unless a recipe is
// supplied for them here.
func tvMakingsSnap(saleUnits, saleCoins int, inputs []sim.RecipeInput, toolKinds ...sim.ItemKind) (*sim.Snapshot, *sim.ActorSnapshot) {
	subj := &sim.ActorSnapshot{RestockPolicy: tvProducePolicy("fried_meat")}
	published := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	recipes := map[sim.ItemKind]*sim.ItemRecipe{
		"fried_meat": {OutputItem: "fried_meat", OutputQty: 1, WholesalePrice: 4, RetailPrice: 7, Inputs: inputs},
		"meat":       {OutputItem: "meat", OutputQty: 1, WholesalePrice: 5, RetailPrice: 8},
		"skillet":    {OutputItem: "skillet", OutputQty: 1, WholesalePrice: 5, RetailPrice: 10},
	}
	kinds := map[sim.ItemKind]*sim.ItemKindDef{
		"fried_meat": {Name: "fried_meat", DisplayLabel: "fried_meat"},
		"meat":       {Name: "meat", DisplayLabel: "meat"},
		"skillet":    {Name: "skillet", DisplayLabel: "skillet"},
	}
	for _, k := range toolKinds {
		kinds[k].DurabilityUses = 20
	}
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"hannah": subj},
		Recipes:     recipes,
		ItemKinds:   kinds,
	}
	if saleUnits > 0 {
		sales := sim.NewRingBuffer[sim.PriceObservation](4)
		sales.Push(sim.PriceObservation{BuyerID: "buyer", Amount: saleCoins, Qty: saleUnits, Consumers: 1, At: published.Add(-24 * time.Hour)})
		snap.PriceBook = map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "hannah", Item: "fried_meat"}: sales,
		}
	}
	return snap, subj
}

// TestBuildTradeValue_ToolInputNotAMakingsCost is the live Hannah Boggs defect
// (LLM-475): fried_meat's recipe requires a skillet, a durable TOOL that wears
// rather than being consumed, and the cost anchor charged the whole 5-coin skillet
// against every single serving. Her line read "the makings run you about 10 coins
// each" against a true meat cost of 5, so every honest sale tripped her
// never-sell-below-cost coda and she sat at 0 coins while trading briskly. The tool
// must contribute NOTHING to the sum — not an amortized share, nothing.
func TestBuildTradeValue_ToolInputNotAMakingsCost(t *testing.T) {
	inputs := []sim.RecipeInput{{Item: "meat", Qty: 1}, {Item: "skillet", Qty: 1}}
	snap, subj := tvMakingsSnap(0, 0, inputs, "skillet")
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 5 || got.CostQty != 1 || got.CostFloor {
		t.Fatalf("tool must not be costed: want CostBatch=5 CostQty=1 floor=false, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "the makings run you about 5 coins each") {
		t.Errorf("want meat-only makings cost:\n%s", b.String())
	}
}

// TestBuildTradeValue_AllInputsToolsNoCostClause: a recipe whose ONLY inputs are
// tools has no cost of goods at all, so the clause is omitted entirely rather than
// rendering a zero — and with no cost there is nothing to judge a sale against.
func TestBuildTradeValue_AllInputsToolsNoCostClause(t *testing.T) {
	snap, subj := tvMakingsSnap(4, 4, []sim.RecipeInput{{Item: "skillet", Qty: 1}}, "skillet")
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 0 || got.CostQty != 0 || got.MakingsMargin != makingsMarginNone {
		t.Fatalf("tool-only recipe should carry no cost and no verdict, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	for _, unwanted := range []string{"makings", "a loss", "earns you nothing"} {
		if strings.Contains(b.String(), unwanted) {
			t.Errorf("tool-only recipe leaked %q:\n%s", unwanted, b.String())
		}
	}
}

// TestBuildTradeValue_MakingsMarginTiers walks the LLM-475 verdict boundary on the
// RAW rates. Cost is a flat 5 coins per serving (meat only). The at-cost row is the
// live miller's closed treadmill — realized exactly meets cost — and the just-above
// row pins that a profitable line gets NO phrase at all (there is no praise tier,
// so the verdict's presence is itself the signal). The sub-coin row is the reason
// the comparison is cross-multiplied rather than run on the rounded display units:
// 19 coins over 4 servings is 4.75 each, which rounds to "5 coins" for display and
// would read as break-even to a whole-coin test.
func TestBuildTradeValue_MakingsMarginTiers(t *testing.T) {
	tests := []struct {
		name             string
		saleUnits        int
		saleCoins        int
		wantTier         makingsMargin
		wantPhrase       string
		unwantedInRender string
	}{
		{"below cost", 4, 12, makingsMarginBelowCost, " (a loss)", "earns you nothing"},
		{"sub-coin below cost", 4, 19, makingsMarginBelowCost, " (a loss)", "earns you nothing"},
		{"exactly at cost", 4, 20, makingsMarginAtCost, " (it earns you nothing)", "a loss"},
		{"just above cost", 4, 21, makingsMarginNone, "", "("},
		{"well above cost", 4, 40, makingsMarginNone, "", "("},
		{"no realized sale", 0, 0, makingsMarginNone, "", "("},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, subj := tvMakingsSnap(tc.saleUnits, tc.saleCoins, []sim.RecipeInput{{Item: "meat", Qty: 1}})
			v := buildTradeValue(snap, "hannah", subj, true)
			if v == nil || len(v.Items) != 1 {
				t.Fatalf("want 1 item, got %+v", v)
			}
			if got := v.Items[0].MakingsMargin; got != tc.wantTier {
				t.Fatalf("tier: want %d, got %d (item %+v)", tc.wantTier, got, v.Items[0])
			}
			var b strings.Builder
			renderTradeValue(&b, v)
			out := b.String()
			if tc.wantPhrase != "" && !strings.Contains(out, "the makings run you about 5 coins each"+tc.wantPhrase) {
				t.Errorf("want verdict %q on the makings clause:\n%s", tc.wantPhrase, out)
			}
			if tc.unwantedInRender != "" && strings.Contains(out, tc.unwantedInRender) {
				t.Errorf("unexpected %q in render:\n%s", tc.unwantedInRender, out)
			}
		})
	}
}

// TestBuildTradeValue_MakingsMarginFloorUncertainty walks the CostFloor boundary,
// where the cue knows only part of what a batch costs (an input priceable by
// neither purchase history nor catalog is dropped from the sum and flags it a
// floor). The asymmetry is the point, and code_review round 1 is what forced it:
// BELOW the floor is provably a loss, because the true cost is at least the sum we
// did compute — but BREAKING EVEN against a floor proves nothing. "Unpriceable"
// means the input's cost is unknown, not that it is positive; it may genuinely be
// free. Calling that a loss would be this ticket's own defect in a new place —
// telling a keeper a number its books don't support — so uncertainty stays silent
// rather than guessing in the alarming direction.
func TestBuildTradeValue_MakingsMarginFloorUncertainty(t *testing.T) {
	// water has no recipe and no purchase history → unpriceable → floor. Known part
	// of the batch is the 5-coin cut of meat.
	floorInputs := []sim.RecipeInput{{Item: "meat", Qty: 1}, {Item: "water", Qty: 2}}
	tests := []struct {
		name       string
		saleUnits  int
		saleCoins  int
		wantTier   makingsMargin
		wantRender string
	}{
		{"break-even against a floor is unjudged", 4, 20, makingsMarginNone, "the makings run you at least 5 coins each."},
		{"below a floor is still provably a loss", 4, 12, makingsMarginBelowCost, "the makings run you at least 5 coins each (a loss)."},
		{"above a floor is unjudged as ever", 4, 40, makingsMarginNone, "the makings run you at least 5 coins each."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, subj := tvMakingsSnap(tc.saleUnits, tc.saleCoins, floorInputs)
			v := buildTradeValue(snap, "hannah", subj, true)
			if v == nil || len(v.Items) != 1 {
				t.Fatalf("want 1 item, got %+v", v)
			}
			if got := v.Items[0]; !got.CostFloor || got.MakingsMargin != tc.wantTier {
				t.Fatalf("want floor=true tier=%d, got %+v", tc.wantTier, got)
			}
			var b strings.Builder
			renderTradeValue(&b, v)
			if !strings.Contains(b.String(), tc.wantRender) {
				t.Errorf("want %q:\n%s", tc.wantRender, b.String())
			}
		})
	}
}

// TestBuildTradeValue_MakingsAllInputsUnpriceableNoCost: when NO input can be
// priced the sum collapses to nothing, so the clause is dropped entirely and the
// floor flag is cleared with it — there is no partial cost to qualify and nothing
// to judge a sale against. Pins that the floor path can't leak a bare "at least 0"
// or a verdict with no figure behind it.
func TestBuildTradeValue_MakingsAllInputsUnpriceableNoCost(t *testing.T) {
	snap, subj := tvMakingsSnap(4, 12, []sim.RecipeInput{{Item: "water", Qty: 2}})
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 0 || got.CostFloor || got.MakingsMargin != makingsMarginNone {
		t.Fatalf("all-unpriceable recipe should carry no cost, no floor, no verdict, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	for _, unwanted := range []string{"makings", "at least", "a loss", "earns you nothing"} {
		if strings.Contains(b.String(), unwanted) {
			t.Errorf("all-unpriceable recipe leaked %q:\n%s", unwanted, b.String())
		}
	}
}

// TestBuildTradeValue_MakingsMarginZeroQtyInputNoCost: an input listed at quantity
// zero contributes nothing to the batch even when it IS priceable, so a recipe
// whose only real entry is a zero-qty input carries no cost basis and no verdict.
func TestBuildTradeValue_MakingsMarginZeroQtyInputNoCost(t *testing.T) {
	snap, subj := tvMakingsSnap(4, 12, []sim.RecipeInput{{Item: "meat", Qty: 0}})
	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	if got := v.Items[0]; got.CostBatch != 0 || got.MakingsMargin != makingsMarginNone {
		t.Fatalf("zero-qty input should contribute no cost and no verdict, got %+v", got)
	}
}

// TestBuildTradeValue_WholesaleLineCarriesMakingsCost is the live Joseph Scott
// defect (LLM-475 #3): the LLM-292 wholesale-channel branch replaced the whole
// clause set, cost anchor included, so wholesale producers were the one class of
// maker never shown their own margin. His flour line named what the shop paid him
// (1 coin) and what folk paid in the shops (2), and never what the grinding cost —
// so he traded five sacks of flour for five sheaves of wheat and called it fair,
// and the 2026-07-19 flour reprice could not reach him at all. The cost sentence
// and the verdict must now ride the wholesale line too.
func TestBuildTradeValue_WholesaleLineCarriesMakingsCost(t *testing.T) {
	const (
		josephID = sim.ActorID("joseph")
		josiahID = sim.ActorID("josiah")
		mill     = sim.StructureID("mill")
		store    = sim.StructureID("general_store")
	)
	published := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	joseph := &sim.ActorSnapshot{
		DisplayName:     "Joseph Scott",
		WorkStructureID: mill,
		RestockPolicy:   tvProducePolicy("flour"),
	}
	josiah := &sim.ActorSnapshot{DisplayName: "Josiah Thorne", WorkStructureID: store}
	// His own realized flour sales: 24 sacks to the shop for 24 coins — 1 flat.
	millSales := sim.NewRingBuffer[sim.PriceObservation](8)
	millSales.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 24, Qty: 24, Consumers: 1, At: published.Add(-12 * time.Hour)})
	// The shop's resales to folk at 2 each, so the line still leads with folk-pay.
	shopSales := sim.NewRingBuffer[sim.PriceObservation](8)
	shopSales.Push(sim.PriceObservation{BuyerID: "walker", Amount: 4, Qty: 2, Consumers: 1, At: published.Add(-6 * time.Hour)})
	// Wheat priced off the catalog at 1: 5 wheat -> 5 flour is a closed treadmill,
	// cost 1/unit against a realized 1/unit.
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{josephID: joseph, josiahID: josiah},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"flour": {OutputItem: "flour", OutputQty: 5, WholesalePrice: 3, RetailPrice: 4,
				Inputs: []sim.RecipeInput{{Item: "wheat", Qty: 5}}},
			"wheat": {OutputItem: "wheat", OutputQty: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(mill):  {ID: sim.VillageObjectID(mill), OwnerActorID: josephID, Tags: []string{sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: josephID, Item: "flour"}: millSales,
			{SellerID: josiahID, Item: "flour"}: shopSales,
		},
	}
	v := buildTradeValue(snap, josephID, joseph, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want 1 item, got %+v", v)
	}
	got := v.Items[0]
	if got.WholesaleTo != "Josiah Thorne" {
		t.Fatalf("want the wholesale-channel line, got %+v", got)
	}
	if got.CostBatch != 5 || got.CostQty != 5 {
		t.Fatalf("wholesale line must keep its cost basis, got CostBatch=%d CostQty=%d", got.CostBatch, got.CostQty)
	}
	if got.MakingsMargin != makingsMarginAtCost {
		t.Fatalf("1-coin flour against 1-coin makings is break-even, got tier %d", got.MakingsMargin)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	want := "Folk have lately paid about 2 coins each in the shops, it costs you about 1 coin each to produce, " +
		"but the shop has lately paid you about 1 coin each (it earns you nothing). Send other buyers to Josiah Thorne."
	if !strings.Contains(b.String(), want) {
		t.Errorf("want the LLM-475 target shape:\nwant substring: %s\ngot:\n%s", want, b.String())
	}
}

// TestBuildTradeValue_WholesaleRawProducerUnchanged: a raw producer's own produce
// (no recipe inputs — Moses's carrots) has no makings cost, so its wholesale line
// keeps exactly the LLM-292/295 shape with no cost sentence and no verdict. Pins
// that LLM-475 is additive for the farms, which is why no existing golden moved.
func TestBuildTradeValue_WholesaleRawProducerUnchanged(t *testing.T) {
	const (
		mosesID  = sim.ActorID("moses")
		josiahID = sim.ActorID("josiah")
		farm     = sim.StructureID("james_farm")
		store    = sim.StructureID("general_store")
	)
	moses := &sim.ActorSnapshot{DisplayName: "Moses James", WorkStructureID: farm, RestockPolicy: tvProducePolicy("carrots")}
	josiah := &sim.ActorSnapshot{DisplayName: "Josiah Thorne", WorkStructureID: store}
	snap := &sim.Snapshot{
		PublishedAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses, josiahID: josiah},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"carrots": {OutputItem: "carrots", OutputQty: 1, WholesalePrice: 1, RetailPrice: 3},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
	}
	v := buildTradeValue(snap, mosesID, moses, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].CostBatch != 0 || v.Items[0].MakingsMargin != makingsMarginNone {
		t.Fatalf("raw producer should carry no cost and no verdict, got %+v", v)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	for _, unwanted := range []string{"it costs you", "a loss", "earns you nothing"} {
		if strings.Contains(b.String(), unwanted) {
			t.Errorf("raw producer line leaked %q:\n%s", unwanted, b.String())
		}
	}
}

// TestCostEachPhrase locks the prose buckets: exact whole coins say "about",
// a sub-half fraction over a whole says "a little over", half-and-up rounds the
// phrase upward to "nearly N+1" (never understating cost), and a cost under half
// a coin gets its own floor phrase.
func TestCostEachPhrase(t *testing.T) {
	tests := []struct {
		batch, qty int
		want       string
	}{
		{batch: 8, qty: 10, want: "nearly 1 coin each"},
		{batch: 5, qty: 10, want: "nearly 1 coin each"},
		{batch: 4, qty: 10, want: "under half a coin each"},
		{batch: 10, qty: 10, want: "about 1 coin each"},
		{batch: 6, qty: 1, want: "about 6 coins each"},
		{batch: 11, qty: 10, want: "a little over 1 coin each"},
		{batch: 15, qty: 10, want: "nearly 2 coins each"},
		{batch: 25, qty: 10, want: "nearly 3 coins each"},
		{batch: 0, qty: 10, want: ""}, // defensive guards — render gates on both
		{batch: 8, qty: 0, want: ""},  // being positive, but the helper must not panic
	}
	for _, tt := range tests {
		if got := costEachPhrase(tt.batch, tt.qty); got != tt.want {
			t.Errorf("costEachPhrase(%d, %d) = %q, want %q", tt.batch, tt.qty, got, tt.want)
		}
	}
}

// TestBuildTradeValue_DualSourceProducedWins: a kind listed under BOTH produce and buy
// entries is valued ONCE, as own-production — the produced loop runs first and the
// seen-set dedupe suppresses the buy pass, so no cost-basis clause attaches even with
// a purchase history (LLM-191). Pins the loop order against an accidental flip.
func TestBuildTradeValue_DualSourceProducedWins(t *testing.T) {
	subj := &sim.ActorSnapshot{RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
		{Item: "cheese", Source: sim.RestockSourceProduce, Max: 10},
		{Item: "cheese", Source: sim.RestockSourceBuy, Max: 10},
	}}}
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ellis_farm", Item: "cheese"}: buys,
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 {
		t.Fatalf("want exactly 1 deduped item, got %+v", v)
	}
	if got := v.Items[0]; got.PaidUnit != 0 {
		t.Errorf("produced-source should win → PaidUnit 0, got %+v", got)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if strings.Contains(b.String(), "lately paid") {
		t.Errorf("produced good should carry no cost-basis clause:\n%s", b.String())
	}
}

// TestBuildTradeValue_DurableToolInputIsReserved pins the LLM-609 policy that a
// durable TOOL input is reserved from sale alongside the consumables. It is a
// deliberate asymmetry with LLM-475, which excludes the same tool from the makings
// COST: cost asks "what did this batch consume" (nothing — the tool wears), and the
// reservation asks "what does the work need present" (the tool, or the work stops).
// A skillet is not eaten by the stew, but a keeper who sells his last one has
// stopped cooking just as surely as one who sells his last flask of water, and the
// reorder floor already keeps two on hand for exactly that reason.
func TestBuildTradeValue_DurableToolInputIsReserved(t *testing.T) {
	inputs := []sim.RecipeInput{{Item: "meat", Qty: 1}, {Item: "skillet", Qty: 1}}
	snap, subj := tvMakingsSnap(0, 0, inputs, "skillet")
	subj.RestockPolicy.Restock = append(subj.RestockPolicy.Restock,
		sim.RestockEntry{Item: "skillet", Source: sim.RestockSourceBuy, Max: 4})
	subj.Inventory = map[sim.ItemKind]int{"skillet": 2} // exactly the 2-batch floor

	v := buildTradeValue(snap, "hannah", subj, true)
	if v == nil {
		t.Fatal("want a view")
	}
	var skillet *TradeValueItem
	for i := range v.Items {
		if v.Items[i].itemKind == "skillet" {
			skillet = &v.Items[i]
		}
	}
	if skillet == nil {
		t.Fatalf("skillet missing from the wares view: %+v", v.Items)
	}
	if skillet.ReserveFloor != 2 || skillet.ReserveHeld != 2 {
		t.Errorf("tool input not reserved: floor=%d held=%d", skillet.ReserveFloor, skillet.ReserveHeld)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if want := "skillet: the 2 you carry are for making your own fried_meat — makings, not wares."; !strings.Contains(b.String(), want) {
		t.Errorf("render missing the tool reservation %q:\n%s", want, b.String())
	}
	// The LLM-475 exclusion must survive alongside it: the tool is still not a cost.
	for i := range v.Items {
		if v.Items[i].itemKind == "fried_meat" && v.Items[i].CostBatch != 5 {
			t.Errorf("tool leaked back into the makings cost: CostBatch=%d, want 5", v.Items[i].CostBatch)
		}
	}
}

// TestBuildTradeValue_MendClaimBeatsBenchReserve pins the LLM-609 tie-break: when a
// good is BOTH earmarked for a mend (LLM-292) and a required input of the actor's
// own recipes, only the repair line renders. Two claims on one pile of nails would
// contradict each other — the mend says the whole holding is spoken for, the bench
// reservation would price the surplus above its floor — and the mend's stake is the
// stronger of the two (the business is already broken).
//
// No live recipe consumes nails today, so this is a guard against a catalog edit
// rather than a fix for an observed scene.
func TestBuildTradeValue_MendClaimBeatsBenchReserve(t *testing.T) {
	const ownerID = sim.ActorID("ezekiel")
	subj := &sim.ActorSnapshot{
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "shovel", Source: sim.RestockSourceProduce, Max: 5},
			{Item: sim.NailItemKind, Source: sim.RestockSourceBuy, Max: 20},
		}},
		Inventory: map[sim.ItemKind]int{sim.NailItemKind: 3},
	}
	zero := 0
	snap := &sim.Snapshot{
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{ownerID: subj},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"forge": {
				ID: "forge", DisplayName: "Blacksmith",
				OwnerActorID: ownerID, Tags: []string{sim.TagBusiness}, Wear: 450,
				LoiterOffsetX: &zero, LoiterOffsetY: &zero,
			},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			// A shovel forged from nails — the catalog edit this guards against.
			"shovel":         {OutputItem: "shovel", OutputQty: 1, WholesalePrice: 5, RetailPrice: 10, Inputs: []sim.RecipeInput{{Item: sim.NailItemKind, Qty: 1}}},
			sim.NailItemKind: {OutputItem: sim.NailItemKind, OutputQty: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"shovel":         {Name: "shovel", DisplayLabel: "shovels"},
			sim.NailItemKind: {Name: sim.NailItemKind, DisplayLabel: "nails"},
		},
	}
	v := buildTradeValue(snap, ownerID, subj, true)
	if v == nil || v.RepairReserve == nil {
		t.Fatalf("want a view carrying the repair reserve, got %+v", v)
	}
	for i := range v.Items {
		if v.Items[i].itemKind == sim.NailItemKind && v.Items[i].ReserveFloor != 0 {
			t.Errorf("mend-claimed nails also picked up a bench reservation: %+v", v.Items[i])
		}
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	out := b.String()
	if !strings.Contains(out, "to mend your Blacksmith") {
		t.Errorf("repair line missing:\n%s", out)
	}
	if strings.Contains(out, "makings, not wares") {
		t.Errorf("bench reservation rendered alongside the mend claim:\n%s", out)
	}
}

// LLM-646 — the sellable set is what you HOLD, converging this cue on the buy
// directory's structural vendorship (eachVendorOffer: holds, qty>0). The live
// case: Josiah resold factor-bale iron priced blind, because iron had no
// restock-policy entry and this cue's walk was policy-scoped while buyers were
// routed to his iron off his inventory.

// TestBuildTradeValue_HeldGoodOffPolicy: a priced good the actor HOLDS but
// neither produces nor has a buy entry for renders with full resale semantics —
// the cost-basis clause and the LLM-627 ask floor — exactly as if it were
// bought-in stock, because it is.
func TestBuildTradeValue_HeldGoodOffPolicy(t *testing.T) {
	subj := &sim.ActorSnapshot{
		RestockPolicy: buyPolicy("cheese", 10),
		Inventory:     map[sim.ItemKind]int{"iron": 2},
	}
	published := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	buys := sim.NewRingBuffer[sim.PriceObservation](4)
	buys.Push(sim.PriceObservation{BuyerID: "josiah", Amount: 10, Qty: 2, Consumers: 1, At: published.Add(-24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt: published,
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
			"iron":   {OutputItem: "iron", WholesalePrice: 2, RetailPrice: 3},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "factor", Item: "iron"}: buys,
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 2 {
		t.Fatalf("want cheese + held iron, got %+v", v)
	}
	var iron *TradeValueItem
	for i := range v.Items {
		if v.Items[i].itemKind == "iron" {
			iron = &v.Items[i]
		}
	}
	if iron == nil {
		t.Fatalf("held off-policy iron missing from wares-worth: %+v", v.Items)
	}
	// Paid 10/2 = 5; cost-plus 5+ceil(5/4) = 7, above retail 3 so unclamped —
	// pass-through pricing working: the import cost, not the band, sets the ask.
	if iron.Low != 2 || iron.High != 3 || iron.PaidUnit != 5 || iron.AskUnit != 7 {
		t.Fatalf("iron item wrong: %+v", *iron)
	}
	var b strings.Builder
	renderTradeValue(&b, v)
	if !strings.Contains(b.String(), "iron: 2 to 3 coins each; you have lately paid about 5 coins each for it — ask 7 coins or more at your counter; your shop lives on the difference.") {
		t.Errorf("held iron must carry the cost anchor and ask floor:\n%s", b.String())
	}
}

// TestBuildTradeValue_HeldGoodGates: the inventory walk inherits valueGood's
// gates unchanged — an uncataloged kind renders nothing, zero stock renders
// nothing, and a kind already valued off the policy is not doubled.
func TestBuildTradeValue_HeldGoodGates(t *testing.T) {
	subj := &sim.ActorSnapshot{
		RestockPolicy: buyPolicy("cheese", 10),
		Inventory: map[sim.ItemKind]int{
			"cheese":  4, // also a policy kind — must not double
			"mystery": 3, // no catalog entry — must stay silent
			"iron":    0, // zero stock — must stay silent
		},
	}
	snap := &sim.Snapshot{
		PublishedAt: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Actors:      map[sim.ActorID]*sim.ActorSnapshot{"josiah": subj},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", WholesalePrice: 3, RetailPrice: 6},
			"iron":   {OutputItem: "iron", WholesalePrice: 2, RetailPrice: 3},
		},
	}
	v := buildTradeValue(snap, "josiah", subj, true)
	if v == nil || len(v.Items) != 1 || v.Items[0].itemKind != "cheese" {
		t.Fatalf("want exactly the one cheese line, got %+v", v)
	}
}
