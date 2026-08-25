package perception

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// trade_value.go — LLM-125. The "## What your wares fetch" cue. In a barter
// (goods-for-goods) an NPC has no coin yardstick to value the wares changing
// hands, so it invents numbers: the live Ezekiel⇄John trade had a homeless smith
// guess his own nails at "1 coin" with nothing behind it, while the tavernkeeper
// drifted stew 4→3 coins tick to tick. The engine already HOLDS the value —
// item_recipe carries a wholesale and a retail price per unit — it just never
// rendered it into the trade moment.
//
// This cue surfaces, when the actor is in company (a huddle — where a trade can
// actually happen), the coin worth of the goods of THEIR OWN TRADE: the
// wholesale–retail spread as the bargaining range, plus what they have actually
// been getting for it of late (sellerRecentSales over the weekly window) when
// they have a coin sales history to draw on.
//
// Scoped to the actor's OWN wares ON PURPOSE — the goods it produces
// (RestockPolicy.ProduceEntries) AND the goods it resells (BuyEntries) — never the
// going rate of wares outside its trade: knowledge of others' prices stays "earned
// by patronage" (the PriceBook buyer-side path in recovery_options / consumable_
// vendors). For a resold good the cue also surfaces the actor's OWN recent purchase
// cost (buyerRecentPurchases) so a reseller can mark up off what it actually paid —
// the supply-side price anchor a pure reseller (empty ProduceEntries) otherwise
// lacked entirely (LLM-191). A PRODUCED good whose recipe has inputs gets the
// matching cost anchor from its ingredient side (LLM-226): each input priced by the
// actor's own purchase history, recipe wholesale as the fallback, spoken per-unit so
// the model never divides. Unlike "## Your trade" this is NOT gated on being
// at the workplace: a smith knows a nail is worth 1–2 coins whether stood at the
// forge or pitching it across a tavern table.

// TradeValueView is the content-gated "## What your wares fetch" section. A nil
// view (or empty Items with no RepairReserve) means render omits the section.
type TradeValueView struct {
	Items []TradeValueItem

	// RepairReserve, when non-nil, appends the earmarked-goods line (LLM-292): the
	// actor owns a business worn to the repair threshold and carries some of the
	// nails the mend takes, so those nails are spoken for — not wares. Live: Josiah
	// bought repair nails from the smith, then resold 5 of them before mending
	// (nothing in perception marked them earmarked, and the role-agnostic coin band
	// made any in-band offer look fair), landing back at 0 nails with the mend nag
	// still live. This line rides the wares cue because the sale it guards against
	// happens exactly where this cue renders: weighing a buyer's offer in company.
	RepairReserve *RepairReserveView

	// SellFirst marks the coin-poor-but-stock-rich keeper (LLM-294): purse below the
	// operator working-capital floor AND overstocked (merchantConserve). When set —
	// and the cue only builds inHuddle, so an audience is co-present — render appends a
	// nudge to offer the overstocked ware to someone here via the sell tool, the
	// actionable half of the conserve steer (the buy-cue softening is Tier 1, in
	// restock.go). SellFirstWare is the display label of the most-overstocked ware;
	// SellFirstCoins is the purse for the prose.
	SellFirst      bool
	SellFirstWare  string
	SellFirstCoins int
}

// RepairReserveView is the earmark behind the repair-reserve line: how many nails
// the standing mend takes, how many the actor carries, and which business needs
// them. Built only for an OWNER with an active mend obligation (the same
// WearableStallToMend + StallRepairable pair every repair cue keys on, so earmark
// and mend nag can't drift) who holds at least one nail.
type RepairReserveView struct {
	ItemLabel    string // display label of the repair material ("nails")
	BusinessName string // the worn business's display name; "" → generic noun
	Needed       int    // nails one repair consumes
	Held         int    // nails the actor currently carries (>= 1 when the view exists)
}

// makingsMargin is the engine-derived judgment on a produced good's realized sale
// price against its own makings cost (LLM-475). The engine computes the tier and
// render selects the phrase — the felt-needs shape, so the comparison arithmetic
// never reaches a weak model. Deliberately has no above-cost tier: a keeper needs
// telling when its work earns nothing, not praising when it pays.
type makingsMargin int

const (
	makingsMarginNone   makingsMargin = iota // no realized sale, no cost basis, or selling above cost
	makingsMarginAtCost                      // realized rate exactly meets the makings cost
	makingsMarginBelowCost
)

// TradeValueItem is one of the actor's own wares — produced or resold — with its
// coin worth. Low/High are the wholesale–retail spread (Low ≤ High); a good priced
// with a single number has Low == High. RecentUnit is the actor's recent realized
// per-unit SALE price over the weekly window, 0 when it has no coin sales of the good
// to draw on (a pure-barter good like nails). PaidUnit is the reseller's recent
// per-unit PURCHASE cost over the same window — the cost basis to mark up from — set
// only for resold (buy-restock) goods and 0 otherwise. Render omits each clause when
// its value is 0.
//
// CostBatch/CostQty carry the produce-side cost basis (LLM-226): the estimated
// ingredient cost of one recipe batch and the batch's output count, set only for a
// produced good whose recipe has inputs. Kept as the exact pair — not a pre-divided
// per-unit coin — so render can phrase the fraction honestly instead of rounding it
// away. CostFloor marks a cost sum missing an unpriceable input, which render
// qualifies as "at least". CostBatch 0 omits the clause.
type TradeValueItem struct {
	ItemLabel  string
	itemKind   sim.ItemKind // unexported sort tiebreak
	Low        int
	High       int
	RecentUnit int
	PaidUnit   int
	CostBatch  int
	CostQty    int
	CostFloor  bool

	// AtOrBelowCost / StrictlyBelowCost gate the resale below-cost caution off a
	// SUB-COIN comparison of the actor's realized sale rate against its realized buy
	// rate (LLM-385), NOT the whole-coin RecentUnit/PaidUnit fields render displays.
	// Those display fields are nearest-rounded for legibility, so milk bought at 1.39
	// and sold at 1.30 both showed "about 1 coin each" and the loss was invisible to a
	// RecentUnit < PaidUnit test. buildTradeValue compares the raw (coins, units) rates
	// by cross-multiplication — no float, no rounding: AtOrBelowCost = the good has a
	// realized sale AND its rate <= the realized buy rate; StrictlyBelowCost = the same
	// with a strict <. Both are false unless the good is a resale with BOTH a purchase
	// AND a sale on record — so the caution fires only for goods actually being sold at
	// or below cost, never as boilerplate on a profitable or not-yet-sold line.
	AtOrBelowCost     bool
	StrictlyBelowCost bool

	// AskUnit is the cost-anchored ask floor for a resold good (LLM-627): what the
	// actor should be charging at its counter, derived from its own recent purchase
	// cost — PaidUnit plus a proportional markup (a quarter, minimum one coin),
	// never below the catalog retail (High). Anchoring on cost rather than a fixed
	// number is deliberate: a hard price would stop tracking the market, but a
	// cost-plus floor rises when upstream prices rise, so pass-through survives.
	//
	// Live case: Josiah Thorne's realized cheese margin fell from ~2 coins/unit to
	// 0.08 in a week. The cue's own trailing averages were the cause — "of late you
	// have sold for about 3" certified his past undersell as the fair price (his
	// accept reasoning, verbatim: "that's exactly what I sell cheese for... Fair
	// terms"), while each accepted counter raised his "lately paid" anchor. The
	// spread only ever narrowed. This field gives the model the RIGHT anchor
	// instead; it self-anchors regardless (LLM-227's no-numbers rule assumed it
	// wouldn't, and the live evidence says otherwise).
	//
	// 0 (clause omitted) when: the good is not a resale with a paid cost basis; the
	// good has no authored spread (Low == High — water, carrots: retail already sits
	// at the integer-coin floor, so a higher ask is the documented wrong fix); or
	// the actor already sells at or above the ask (raw-rate compare — presence of
	// the phrase is itself the signal, the LLM-475 principle, so a winning line
	// carries no nudge).
	AskUnit int

	// MakingsMargin is the produce-side sibling of AtOrBelowCost (LLM-475): the
	// actor's realized SALE rate for a good it MAKES, judged against what the
	// makings actually cost it. The resold-goods path had this register from the
	// start ("you pay about 1 and resell at about 1, so it earns you nothing on its
	// own") and produced goods never got it — live, the miller swapped five sacks of
	// flour for five sheaves of wheat, a full day's grinding for nothing, and called
	// it fair. Decided on the RAW rates by cross-multiplication for the same
	// sub-coin reason AtOrBelowCost is (see above): realized coins/units against
	// CostBatch/CostQty, no float, no rounding. makingsMarginNone unless the good has
	// BOTH a realized sale AND a positive makings cost on record — so a
	// not-yet-sold line and a profitable one both carry no phrase (there is no
	// praise tier; only a loss is worth a keeper's attention). Break-even against a
	// CostFloor sum is likewise unjudged: an unpriceable input leaves the true cost
	// UNKNOWN above the sum, not known to be higher, and a verdict meant to be acted
	// on must not rest on a number the actor's own books don't support.
	MakingsMargin makingsMargin

	// ReserveFloor / ReserveHeld / ReserveMakes carry the production-input
	// reservation (LLM-609): this good is a required input of one of the actor's
	// OWN produce recipes, so the stock it holds is makings rather than
	// merchandise. Live case — Ezekiel Crane sold Elizabeth Ellis his water at
	// 1 coin, and within the hour all three of his recipes (nail, shovel,
	// skillet) were input-short and the forge had stopped. The wares cue priced
	// the water as stock to move and the only judgment on the line was about
	// margin, so nothing gave the model a reason to hold it back.
	//
	// ReserveFloor is sim.ReorderFloors for the item — the SAME floor the reorder
	// threshold and the sell-first nudge already read, deliberately not a second
	// number. It means "two batches of the largest draw", so a good priced above
	// it is stock the actor would not be rebuying anyway, and a good priced below
	// it is stock it is about to send someone out to replace. Reusing it keeps the
	// reservation and the restock warrant from ever disagreeing about what counts
	// as spare.
	//
	// ReserveHeld is the on-hand quantity and ReserveMakes the display labels of
	// the goods the item feeds, in policy order — render phrases the claim from
	// those rather than from the recipe mechanics. All three are 0/nil unless the
	// actor holds at least one of a floored good: at zero on hand there is nothing
	// to reserve and the buy-side cues own the state, the same carve-out
	// buildRepairReserve makes.
	//
	// ReserveReason widens the claim past recipe inputs (LLM-636): the shared
	// spoken-for reservation (sim.SpokenFor) also holds back a non-food buy line
	// the actor only ever uses (mending's thread, a boost salt, a bench tool) and
	// the one garment on its back — the same units the carry line marks "not for
	// trade" and the pay_with_item gate refuses. For those ReserveMakes is nil (no
	// recipe names them) and render phrases the claim from the reason instead, so
	// this cue can never price as wares what the line above it just said was
	// not. SpokenForNone when the reservation is the LLM-609 recipe floor alone.
	ReserveFloor  int
	ReserveHeld   int
	ReserveMakes  []string
	ReserveReason sim.SpokenForReason

	// WholesaleTo, when non-empty, marks this as a wholesale producer's OWN
	// produce (sim.IsOwnProduce) and names the village distributor it sells to —
	// the sole legitimate buyer (LLM-223/252). Render then draws the wholesale-
	// channel line instead of a retail spread, so a producer isn't nudged to hawk
	// its produce to whoever it's standing with (the retail negotiation the
	// PayWithItem wholesale gate then refuses — live hud-9b23…, LLM-291). Low
	// carries the wholesale unit price (what the distributor pays) for that line.
	WholesaleTo string

	// BulkUnit / ShopUnit carry the two figures on the wholesale-channel line
	// (WholesaleTo set), resolved observed-first (LLM-295): BulkUnit is what the
	// producer has actually been getting selling the good (its own sellerRecentSales),
	// ShopUnit is what the shops have actually been reselling it for (observedShopSales),
	// each falling back to the catalog seed (Low / High) only until real transactions
	// exist. *Observed flags record which source won so render phrases a lived rate
	// ("the shop has lately paid you about N") apart from a seed estimate ("the fair
	// bulk rate is about N"). Both 0 only if the good has no catalog price either.
	BulkUnit     int
	BulkObserved bool
	ShopUnit     int
	ShopObserved bool
}

// buildTradeValue builds the wares-worth view for an actor that has goods of its
// own trade AND is in company (inHuddle — the situation where a trade can occur),
// else nil. The repair-reserve earmark (LLM-292) shares the gate: an offer for
// earmarked nails also arrives in company, so the two surface together. Pure over
// the snapshot.
func buildTradeValue(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, inHuddle bool) *TradeValueView {
	if snap == nil || actorSnap == nil {
		return nil
	}
	if !inHuddle {
		return nil // no one to trade with — nothing to value against
	}
	if actorSnap.RestockPolicy == nil || snap.Recipes == nil {
		// No own trade to value — but a worn-business owner still carries the
		// repair-reserve earmark (it keys on ownership + inventory, not the policy).
		if reserve := buildRepairReserve(snap, actorID, actorSnap); reserve != nil {
			return &TradeValueView{RepairReserve: reserve}
		}
		return nil
	}
	// Resolved once for the scan: the label a wholesale producer's own-produce
	// lines route buyers to. Cheap (one object-map pass) and the cue is already
	// gated to inHuddle + has-own-wares, so it isn't a hot path.
	distLabel := distributorLabel(snap)
	// LLM-609: the produce-input floors, read from the same sim.ReorderFloors
	// catalog the restock warrant and the sell-first nudge use. The nudge half of
	// THIS view already refused to name a production input as the ware to sell
	// down (LLM-462, working_capital.go); the item list above it never got the
	// same discriminator and went on pricing the smith's water as merchandise.
	floors := sim.ReorderFloors(snap.Recipes, actorSnap.RestockPolicy)
	// LLM-636: the wider spoken-for reservation — the same map the carry line and
	// the pay_with_item intake gate read — so a non-recipe making (thread, salt)
	// and the worn garment are held back here too, not just the recipe floors.
	spokenFor := sim.SpokenFor(snap.ItemKinds, snap.Recipes, sim.SnapshotBarterHolder(snap, actorSnap))
	// Built before the item walk so a good already earmarked for a mend can't also
	// pick up a bench reservation and render two claims on the same stock.
	reserve := buildRepairReserve(snap, actorID, actorSnap)
	var items []TradeValueItem
	seen := make(map[sim.ItemKind]bool)
	// valueGood appends the wares-worth line for one of the actor's own goods. isResale
	// marks a buy-restock good, for which the cost-basis (what it paid) clause applies;
	// a produced good leaves PaidUnit 0. seen dedupes a kind listed under both sources.
	valueGood := func(item sim.ItemKind, isResale bool) {
		if seen[item] {
			return
		}
		recipe := snap.Recipes[item]
		if recipe == nil {
			return
		}
		lo, hi := recipe.WholesalePrice, recipe.RetailPrice
		if lo > hi {
			lo, hi = hi, lo
		}
		if lo <= 0 {
			lo = hi // only one price configured (no wholesale floor) — collapse to it
		}
		if hi <= 0 {
			return // no price configured at all — nothing to surface
		}
		recentUnit := 0
		recentUnits, recentCoins := 0, 0
		// sellerRecentSales is the actor's OWN realized coin sales of this good over
		// the weekly window — the lived "what I've been getting for it" signal, only
		// shown when there is history (a coin market for the good actually exists). The
		// raw (units, coins) are kept alongside the rounded display value for the
		// sub-coin below-cost test below (LLM-385).
		if units, coins := sellerRecentSales(snap, actorID, item, restockSalesWindow); units > 0 {
			recentUnit = (coins + units/2) / units // round to nearest coin (display only)
			recentUnits, recentCoins = units, coins
		}
		// For a resold good, buyerRecentPurchases is the actor's OWN recent per-unit
		// cost — the anchor a reseller marks up from. Window-averaged and rounded like
		// the sale price; 0 (clause omitted) when it hasn't restocked the good of late.
		paidUnit := 0
		paidUnits, paidCoins := 0, 0
		if isResale {
			if units, coins := buyerRecentPurchases(snap, actorID, item, restockSalesWindow); units > 0 {
				paidUnit = (coins + units/2) / units // display only
				paidUnits, paidCoins = units, coins
			}
		}
		// LLM-385: decide the below-cost caution on the RAW rates, not the rounded
		// display units — a sub-coin loss (milk paid 1.39/unit, sold 1.30/unit) rounds
		// both to "1 coin" and is invisible to a RecentUnit < PaidUnit test. Cross-
		// multiply to compare the sale rate (recentCoins/recentUnits) against the buy
		// rate (paidCoins/paidUnits) with no float and no rounding. Only meaningful when
		// the good has BOTH a realized sale and a realized purchase on record, so a
		// profitable line and a not-yet-sold line both carry no caution.
		//
		// paidCoins > 0 guards a ZERO-cost basis: a barter/free acquisition (Amount 0,
		// Qty > 0) has paidUnits > 0 but paidCoins 0, so a free-in/free-out good would
		// read 0 <= 0 as "at or below cost" and warn though it cost nothing. Requiring a
		// positive paid cost drops that false positive; a zero-coin SALE against a
		// positive cost still (correctly) warns, since recentCoins 0 < paidCoins·units.
		atOrBelowCost, strictlyBelowCost := false, false
		if recentUnits > 0 && paidUnits > 0 && paidCoins > 0 {
			saleTimesBuyUnits := recentCoins * paidUnits
			buyTimesSaleUnits := paidCoins * recentUnits
			atOrBelowCost = saleTimesBuyUnits <= buyTimesSaleUnits
			strictlyBelowCost = saleTimesBuyUnits < buyTimesSaleUnits
			// LLM-627: on a no-spread good (water, carrots — wholesale = retail = 1)
			// breakeven at the floor is the ordinary state of the line, not a bleed —
			// integer coins put retail at the floor already, so the at-cost caution
			// fired every turn as unactionable noise, teaching the model the clause
			// means nothing on the lines where it does. A STRICT loss still warns:
			// the good can be bought above its fixed price, and that loss is real —
			// the lever there is the buy side (render narrows the advisory to it).
			if lo == hi {
				atOrBelowCost = strictlyBelowCost
			}
		}
		// For a produced good with real recipe inputs, estimate the cost of goods —
		// the produce-side sibling of the reseller cost-basis clause (LLM-226).
		// Each input is priced by the actor's own recent purchases (what it actually
		// pays for its milk), falling back to the input's catalog price (wholesale,
		// else retail) when it has no purchase history. An input priceable by
		// neither is left out of the sum and flags the total as a floor.
		costBatch, costQty, costFloor := 0, 0, false
		if !isResale && len(recipe.Inputs) > 0 {
			costQty = recipe.OutputQty
			if costQty <= 0 {
				costQty = 1
			}
			for _, in := range recipe.Inputs {
				// LLM-475: a durable tool is a presence requirement that WEARS, never
				// an input consumed by the batch (tool_wear.go) — so it is not a cost
				// of these makings at all. Charging the whole skillet against every
				// serving told Hannah Boggs her fried meat cost ~10 coins to make when
				// the meat alone was ~5, and every honest sale read as a loss against
				// her never-sell-below-cost coda. No amortization: the tool's own
				// replacement is a restock the reorder floor already drives.
				if sim.DurableToolUses(snap.ItemKinds, in.Item) > 0 {
					continue
				}
				unitCost := 0
				// History prices by CEILING division, unlike paidUnit's
				// nearest-rounding: paidUnit reports what was paid, this feeds a
				// don't-sell-below-this floor, where rounding a bulk-bought cheap
				// input (1 coin for 10 milk) down to a free ingredient silently
				// understates the cost the clause exists to reveal. Zero-coin
				// history is no price signal — fall through to the catalog.
				if units, coins := buyerRecentPurchases(snap, actorID, in.Item, restockSalesWindow); units > 0 && coins > 0 {
					unitCost = (coins + units - 1) / units
				} else if inRecipe := snap.Recipes[in.Item]; inRecipe != nil {
					unitCost = inRecipe.WholesalePrice
					if unitCost <= 0 {
						unitCost = inRecipe.RetailPrice
					}
				}
				if unitCost <= 0 {
					costFloor = true
					continue
				}
				costBatch += in.Qty * unitCost
			}
			if costBatch <= 0 {
				// No positive input cost was found — no useful cost signal, omit the clause.
				costQty, costFloor = 0, false
			}
		}
		// LLM-475: judge the good's realized SALE rate against its own makings cost.
		// Sits here, BEFORE the wholesale branch below, so it depends on nothing that
		// branch does: it reads only the raw rates and the cost basis, both final at
		// this point. (Ordering it after would have worked today only because the
		// branch happens to clear the rounded display fields and not the raw ones —
		// a coupling a later wholesale refactor could silently break, taking the
		// verdict with it while every render test still passed.)
		//
		// Cross-multiplied like AtOrBelowCost above and for the same sub-coin reason:
		// realized coins/units against costBatch/costQty, no float, no rounding. Widened
		// to int64 for the multiply — the same defensive posture as ToolRunwayUses, since
		// realized ledger totals accumulate independently of the recipe-side values and a
		// wrapped product would REVERSE the verdict rather than merely garble it.
		// Requires a realized sale AND a positive cost, so a not-yet-sold good is never
		// judged. Anchored on the actor's OWN cost, never the catalog band.
		makingsTier := makingsMarginNone
		if recentUnits > 0 && costBatch > 0 && costQty > 0 {
			saleTimesCostQty := int64(recentCoins) * int64(costQty)
			costTimesSaleUnits := int64(costBatch) * int64(recentUnits)
			switch {
			case saleTimesCostQty < costTimesSaleUnits:
				// True cost is at least costBatch, so a rate below it is a loss whether
				// or not the sum was a floor.
				makingsTier = makingsMarginBelowCost
			case saleTimesCostQty == costTimesSaleUnits && !costFloor:
				makingsTier = makingsMarginAtCost
			}
			// Break-even against a FLOOR cost deliberately gets NO verdict. An
			// unpriceable input means the cost is UNKNOWN above the sum, not known to
			// be higher — the input could genuinely be free. Calling that a loss would
			// be the same species of defect this ticket fixes (telling a keeper a
			// number its own books don't support), so uncertainty stays silent.
		}
		// LLM-291: a wholesale producer's own produce sells only to the village
		// distributor, never retail — so this good gets the wholesale-channel line,
		// not a retail spread + "set a fair price" framing that nudges the producer
		// to hawk it to whoever it's standing with (the sale the PayWithItem
		// wholesale gate then refuses). Keyed on the SAME sim.IsOwnProduce the
		// Consume guard and eat-cue filter on, so cue and block agree; item-scoped,
		// so a wholesaler's RESOLD retail goods (isResale) are untouched. The retail
		// spread and recent-sale clauses don't apply to the wholesale line, so clear
		// them — render draws it from WholesaleTo + BulkUnit/ShopUnit.
		//
		// LLM-475: the makings cost is the exception and CARRIES THROUGH. Clearing it
		// here left wholesale producers the one class of maker never shown its own
		// margin: Joseph Scott's live flour line named what the shop paid him and
		// never what the grinding cost him, so the 2026-07-19 flour reprice could not
		// reach him and he went on selling at a coin a sack for a week.
		wholesaleTo := ""
		bulkUnit, bulkObserved := 0, false
		shopUnit, shopObserved := 0, false
		if sim.IsOwnProduce(snap.VillageObjects, actorSnap.WorkStructureID, actorSnap.RestockPolicy, item) {
			wholesaleTo = distLabel
			recentUnit, paidUnit = 0, 0
			// LLM-295: both figures observed-first, catalog band as seed fallback.
			// Bulk = what the producer has actually been getting for the good (its own
			// realized sales — i.e. what the shop has paid it). Shop = what the shops
			// have actually been reselling it for (observedShopSales). The catalog
			// wholesale (lo) / retail (hi) stand in only until real trades exist.
			if units, coins := sellerRecentSales(snap, actorID, item, restockSalesWindow); units > 0 {
				bulkUnit, bulkObserved = (coins+units/2)/units, true
			} else {
				bulkUnit = lo
			}
			if units, coins := observedShopSales(snap, item, restockSalesWindow); units > 0 {
				shopUnit, shopObserved = (coins+units/2)/units, true
			} else {
				shopUnit = hi
			}
		}
		// LLM-627: the cost-anchored ask floor (see the AskUnit field comment).
		// Computed AFTER the wholesale branch on purpose: an own-produce good has
		// its paidUnit cleared there, so this stays 0 and the wholesale-channel
		// line keeps sole ownership of that good's pricing. Derived from the
		// DISPLAYED paid cost, not the raw rates — the ask renders beside "you have
		// lately paid about N", and the two must not disagree about what N is.
		// The markup is ceil(paid/4): proportional so a dear good (a 20-coin cloak)
		// earns shopkeeper's margin, floored at 1 coin so a cheap one still does.
		// Clamped up to the catalog retail — in the normal case (cost at or under
		// wholesale) the ask IS the authored shelf price; cost-plus takes over only
		// when cost genuinely rises above the band, which is pass-through working.
		askUnit := 0
		if isResale && paidUnit > 0 && lo != hi {
			costPlus := paidUnit + (paidUnit+3)/4
			askUnit = costPlus
			if askUnit < hi {
				askUnit = hi
			}
			// Suppression compares against costPlus, NOT the clamped ask: the clause
			// exists to stop a margin bleed, not to enforce the catalog shelf price.
			// A line already earning its markup (paid 2, sold 5, retail 6) is a
			// winning line and carries no nudge — the LLM-385 no-boilerplate-on-
			// profitable-lines principle. Raw sale rate, so a sub-coin-thin margin
			// can't hide behind display rounding. Division rather than the cross-
			// multiply the caution above uses: floor(coins/units) >= costPlus is
			// exactly equivalent to coins >= costPlus*units for an INTEGER threshold,
			// and a ledger-sum multiply is one overflow away from suppressing the ask
			// on the losing line it exists for.
			if recentUnits > 0 && recentCoins/recentUnits >= costPlus {
				askUnit = 0
			}
		}
		// LLM-609: what of this good is spoken for by the actor's own bench. Keyed
		// on the recipe EXISTING in the policy, never on it being runnable this
		// minute: gating on "can he run a batch right now" recreates the LLM-608
		// deadlock shape from the other side — short of flour, so the water is
		// unreserved and sold, and by the time the flour is bought the water is
		// gone. Skipped for a good the mend has already claimed (the repair line
		// below states a stronger version of the same thing, and two lines on one
		// stock contradict each other).
		reserveFloor, reserveHeld := 0, 0
		var reserveMakes []string
		reserveReason := sim.SpokenForNone
		mendClaimed := reserve != nil && item == sim.NailItemKind
		if floor := floors[item]; floor > 0 && !mendClaimed {
			if held := actorSnap.Inventory[item]; held > 0 {
				reserveFloor, reserveHeld = floor, held
				reserveMakes = reservedMakings(snap, actorSnap.RestockPolicy, item)
			}
		} else if c := spokenFor[item]; c.Qty > 0 && !mendClaimed {
			// LLM-636: no recipe floors it, but the shared reservation does — a
			// making the actor only uses, or the garment on its back. The claim's
			// qty is the floor; ReserveMakes stays nil and the reason carries the
			// phrasing.
			reserveFloor, reserveHeld, reserveReason = c.Qty, actorSnap.Inventory[item], c.Reason
		}
		seen[item] = true
		items = append(items, TradeValueItem{
			ItemLabel:         itemDisplayLabel(snap, item),
			itemKind:          item,
			Low:               lo,
			High:              hi,
			RecentUnit:        recentUnit,
			PaidUnit:          paidUnit,
			CostBatch:         costBatch,
			CostQty:           costQty,
			CostFloor:         costFloor,
			AtOrBelowCost:     atOrBelowCost,
			StrictlyBelowCost: strictlyBelowCost,
			AskUnit:           askUnit,
			MakingsMargin:     makingsTier,
			ReserveFloor:      reserveFloor,
			ReserveHeld:       reserveHeld,
			ReserveMakes:      reserveMakes,
			ReserveReason:     reserveReason,
			WholesaleTo:       wholesaleTo,
			BulkUnit:          bulkUnit,
			BulkObserved:      bulkObserved,
			ShopUnit:          shopUnit,
			ShopObserved:      shopObserved,
		})
	}
	// Produced goods first, so a kind somehow listed under both sources values as
	// own-production (no cost-basis clause); resold goods (LLM-191) then extend the cue
	// to the pure reseller, which produces nothing and otherwise got no anchor at all.
	for _, e := range actorSnap.RestockPolicy.ProduceEntries() {
		valueGood(e.Item, false)
	}
	for _, e := range actorSnap.RestockPolicy.BuyEntries() {
		valueGood(e.Item, true)
	}
	// LLM-646: goods actually ON HAND are priced too, policy entry or not.
	// The buy directory already defines vendorship structurally — a seller is
	// whoever HOLDS the good, qty > 0 (eachVendorOffer, consumable_vendors.go)
	// — so buyers were being routed to stock whose holder was never shown a
	// price for it: this cue alone scoped by restock policy, and the two
	// surfaces disagreed about what the actor's trade is. Live 2026-08-25:
	// Josiah Thorne resold 40 bars of factor-bale iron at ~3 coins each
	// against a ~5-coin import cost — his render carried lines for all 16
	// policy kinds and NONE for the iron he was holding and trading daily
	// (salt and thread likewise, priced blind). This walk closes the
	// divergence: the sellable set is what you hold, same as the directory.
	// The directory's buyer-scoped filters (wholesale allowance, workplace
	// routing) deliberately don't apply — an actor may always price its own
	// stock. valueGood's own gates keep the addition quiet where it should
	// be: an uncataloged or unpriced kind renders nothing, a making or worn
	// garment renders its LLM-636 reservation line instead of a pitch, and
	// seen-dedup leaves every policy kind exactly as the walks above valued
	// it. Resale semantics on purpose: held-but-not-produced stock was bought
	// or bartered in, so the LLM-226 cost-basis clause and the LLM-627 ask
	// floor apply — the anchor follows the goods. Map iteration order is
	// irrelevant: inclusion is total over held kinds and the sort below fixes
	// the render order.
	for item, qty := range actorSnap.Inventory {
		if qty > 0 {
			valueGood(item, true)
		}
	}
	if len(items) == 0 && reserve == nil {
		return nil
	}
	// Stable, deterministic order: by label, then kind.
	sort.Slice(items, func(i, j int) bool {
		if items[i].ItemLabel != items[j].ItemLabel {
			return items[i].ItemLabel < items[j].ItemLabel
		}
		return items[i].itemKind < items[j].itemKind
	})
	view := &TradeValueView{Items: items, RepairReserve: reserve}
	// LLM-294 Tier 2: a coin-poor + overstocked keeper is nudged to offer its most-
	// overstocked ware to whoever is co-present (the cue is inHuddle-gated, so an
	// audience is present) via the sell tool. Shared determination with the Tier-1
	// buy-cue softening (buildRestocking).
	if c := merchantConserve(snap, actorID, actorSnap); c.Active {
		view.SellFirst = true
		view.SellFirstWare = c.OverstockedWare
		view.SellFirstCoins = c.Coins
	}
	return view
}

// reservedMakings names the goods the actor's own produce recipes make WITH item
// — the concrete anchor the reservation line is phrased from ("for making your
// own nails, shovels and skillets"), in policy order and deduped by output kind.
// Reads the policy's produce entries rather than the recipe catalog at large, so
// only a recipe the actor is actually set up to run puts a claim on the stock.
//
// A durable tool input (a skillet in a stew recipe) is deliberately INCLUDED. It
// is not consumed by the batch, but selling the last one stops the work just as
// surely as selling the last flask of water, and the reservation line's phrasing
// — "for making your stew" — is true of a tool as well as of an ingredient.
func reservedMakings(snap *sim.Snapshot, policy *sim.RestockPolicy, item sim.ItemKind) []string {
	if snap == nil || policy == nil {
		return nil
	}
	var makes []string
	seen := make(map[sim.ItemKind]bool)
	for _, pe := range policy.ProduceEntries() {
		if seen[pe.Item] {
			continue
		}
		recipe := snap.Recipes[pe.Item]
		if recipe == nil {
			continue
		}
		for _, in := range recipe.Inputs {
			if in.Item != item || in.Qty <= 0 {
				continue
			}
			seen[pe.Item] = true
			makes = append(makes, sanitizeInline(itemDisplayLabel(snap, pe.Item)))
			break
		}
	}
	return makes
}

// makingsList joins the reserved-for labels into a readable series. Local to this
// cue rather than shared: every list-joining site in the package inlines its own
// so each can pick the conjunction its sentence needs.
func makingsList(labels []string) string {
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " and " + labels[1]
	default:
		return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
	}
}

// buildRepairReserve builds the earmarked-nails view (LLM-292), or nil. Non-nil
// only when the actor OWNS a business worn to the repair threshold (hired menders
// are excluded — the nails at stake are the owner's own repair supplies, the same
// carve-out the buy-nails errand cues make) and carries at least one nail. Keys on
// the SAME WearableStallToMend + StallRepairable pair as the repair cues, so the
// earmark appears exactly while the mend nag is live and clears when the mend
// lands or the wear decays below threshold.
func buildRepairReserve(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot) *RepairReserveView {
	stall, hired := sim.WearableStallToMend(snap.VillageObjects, snap.LaborLedger, actorID)
	if stall == nil || hired {
		return nil
	}
	if !sim.StallRepairable(stall, snap.StallWearRepairThreshold, snap.StallWearDegradeThreshold) {
		return nil
	}
	needed := snap.StallNailsPerRepair
	if needed <= 0 {
		return nil // a repair costs no nails (feature off / misconfig) — nothing to earmark
	}
	held := actorSnap.Inventory[sim.NailItemKind]
	if held <= 0 {
		return nil // carrying none — the buy-nails errand cues own this state
	}
	return &RepairReserveView{
		ItemLabel:    itemDisplayLabel(snap, sim.NailItemKind),
		BusinessName: resolveDwellPinLabel(snap, stall.ID),
		Needed:       needed,
		Held:         held,
	}
}

// renderTradeValue writes the "## What your wares fetch" section. Content-gated:
// a nil/empty view writes nothing. One line per own ware — the coin range to anchor
// a price, plus the recent purchase cost (resold goods) and the recent realized sale
// price, each when on record. For a resold good the two together bracket the markup:
// what it cost, what it has fetched.
func renderTradeValue(b *strings.Builder, v *TradeValueView) {
	if v == nil || (len(v.Items) == 0 && v.RepairReserve == nil) {
		return
	}
	b.WriteString("## What your wares fetch\n")
	b.WriteString("What your wares fetch in coin — use it to set a fair price or weigh a trade:\n")
	for _, it := range v.Items {
		// LLM-609: a good the actor's own recipes need is not merchandise. Two
		// tiers, the shape the repair-reserve line below already uses: at or under
		// the floor the reservation REPLACES the price entirely (a priced line and
		// a keep-it line in the same section contradict each other, and the margin
		// judgment on the price actively argued for the sale — Ezekiel's water read
		// "selling below your costs loses you coin", which frames a break-even
		// trade as the problem to solve rather than the sale as the problem);
		// above it the good keeps its price and states the keep-back, so a maker
		// who legitimately trades a good it also uses (the dairy selling milk it
		// also turns into cheese) is not shut out of its own market.
		//
		// The stake is named and no imperative is issued — the scene is the
		// argument. Nothing here blocks the sale; the engine still lets him sell it.
		//
		// LLM-636 widens the same two tiers to the shared spoken-for claim: a
		// making no recipe names (ReserveMakes nil, ReserveReason makings) and the
		// garment on the actor's back (ReserveReason garment) — phrased from the
		// reason, since there is no "for making your own X" to say.
		if it.ReserveHeld > 0 && it.ReserveHeld <= it.ReserveFloor {
			switch {
			case len(it.ReserveMakes) > 0:
				fmt.Fprintf(b, "- %s: the %d you carry are for making your own %s — makings, not wares. Selling them leaves you nothing to work with.\n",
					sanitizeInline(it.ItemLabel), it.ReserveHeld, makingsList(it.ReserveMakes))
				continue
			case it.ReserveReason == sim.SpokenForGarment:
				fmt.Fprintf(b, "- %s: the %d you carry are your own clothes, not wares.\n",
					sanitizeInline(it.ItemLabel), it.ReserveHeld)
				continue
			case it.ReserveReason == sim.SpokenForMakings:
				fmt.Fprintf(b, "- %s: the %d you carry you keep to work with — makings, not wares. Selling them leaves you nothing to work with.\n",
					sanitizeInline(it.ItemLabel), it.ReserveHeld)
				continue
			}
		}
		// The surplus clause rides whichever line renders below (retail or
		// wholesale-channel), so the two can't drift on how a keep-back is spoken.
		keepBack := ""
		if it.ReserveFloor > 0 && it.ReserveHeld > it.ReserveFloor {
			switch {
			case len(it.ReserveMakes) > 0:
				keepBack = fmt.Sprintf(" Keep %d back for making your own %s; only the %d beyond that are yours to sell.",
					it.ReserveFloor, makingsList(it.ReserveMakes), it.ReserveHeld-it.ReserveFloor)
			case it.ReserveReason == sim.SpokenForGarment:
				keepBack = fmt.Sprintf(" Keep %d back — your own clothes; only the %d beyond that are yours to sell.",
					it.ReserveFloor, it.ReserveHeld-it.ReserveFloor)
			case it.ReserveReason == sim.SpokenForMakings:
				keepBack = fmt.Sprintf(" Keep %d back to work with; only the %d beyond that are yours to sell.",
					it.ReserveFloor, it.ReserveHeld-it.ReserveFloor)
			}
		}
		// LLM-291: a wholesale producer's own produce isn't sold retail — it goes
		// in bulk to the shop that stocks it. Draw the wholesale-channel line (who
		// buys it, what they pay, where to send other would-be buyers) instead of a
		// retail spread, so the producer doesn't negotiate a street sale the
		// PayWithItem wholesale gate then refuses. LLM-292 added the two ends of
		// the band stated by role — folk pay the HIGH end at the shop's shelf, the
		// fair bulk rate selling TO the shop is the LOW end — so the producer stops
		// taking a shelf price for a bulk sale (the live Elizabeth milk leg: the
		// role-agnostic "1 to 2 coins" band let her take 2+ from the store's bulk
		// buys). LLM-295: both figures are observed-first — a lived rate is phrased as
		// such ("the shop has lately paid you about N") and a catalog SEED as a
		// designer estimate ("the fair bulk rate is about N") — so the producer is
		// never told a hand-authored guess is a rate the market has set. Copy
		// constraint (Jeff): never the mechanic-role word "distributor" — the NPC is
		// told who stocks its goods, not what engine role that actor holds.
		if it.WholesaleTo != "" {
			to := sanitizeInline(it.WholesaleTo)
			label := sanitizeInline(it.ItemLabel)
			bulk := fmt.Sprintf("the fair bulk rate selling to the shop is about %s each", coinsPhrase(it.BulkUnit))
			if it.BulkObserved {
				bulk = fmt.Sprintf("the shop has lately paid you about %s each", coinsPhrase(it.BulkUnit))
			}
			// LLM-475: the makings cost belongs on this line too — it is the only
			// figure here anchored on the producer's OWN books rather than on what
			// the shop chooses to pay, and without it a reprice of the catalog band
			// is invisible to him. Leads the clause list when there is no folk-pay
			// figure to lead it. The margin judgment rides the bulk clause, since the
			// bulk rate is what the cost is being judged against.
			leads := []string{}
			if it.ShopUnit > it.BulkUnit {
				// Shop markup worth naming: folk-pay leads, the bulk rate follows with "but".
				folk := fmt.Sprintf("Folk pay about %s each in the shops", coinsPhrase(it.ShopUnit))
				if it.ShopObserved {
					folk = fmt.Sprintf("Folk have lately paid about %s each in the shops", coinsPhrase(it.ShopUnit))
				}
				leads = append(leads, folk)
			}
			if cost := makingsCostPhrase(it); cost != "" {
				leads = append(leads, fmt.Sprintf("it costs you %s to produce", cost))
			}
			sentence := bulk
			if len(leads) > 0 {
				sentence = strings.Join(leads, ", ") + ", but " + bulk
			}
			// Capitalized for the sentence start. Every phrasing built above begins
			// with an ASCII word ("Folk", "it", "the"), so a single-byte upper is the
			// right operation; the length guard keeps that from being a panic the day
			// a future clause can be empty.
			if sentence != "" {
				sentence = strings.ToUpper(sentence[:1]) + sentence[1:]
			}
			fmt.Fprintf(b,
				"- %s: your own produce — it sells in bulk to %s, whose shop stocks it for the village, not to folk directly. %s%s. Send other buyers to %s.%s\n",
				label, to, sentence, makingsMarginPhrase(it.MakingsMargin), to, keepBack,
			)
			continue
		}
		worth := fmt.Sprintf("%s each", coinsPhrase(it.High))
		if it.Low != it.High {
			worth = fmt.Sprintf("%d to %s each", it.Low, coinsPhrase(it.High))
		}
		// Cost basis first (what a resold good cost you), then the achieved sale price;
		// together they bracket the markup. Each clause is omitted when its value is 0.
		clauses := ""
		if it.PaidUnit > 0 {
			clauses += fmt.Sprintf("; you have lately paid about %s each for it", coinsPhrase(it.PaidUnit))
		}
		if it.RecentUnit > 0 {
			clauses += fmt.Sprintf("; of late you have sold for about %s each", coinsPhrase(it.RecentUnit))
		}
		// LLM-292: the resale cost clause carries its stake. The bare fact failed
		// live — Josiah's rendered line held "paid about 2… sold for about 1" and he
		// accepted a 1-coin offer on that very prompt; all data, no consequence. Per
		// the prompt-prose principle the clause says what the number is FOR. Resale
		// goods only — the produced-goods makings clause below deliberately stays a
		// directive-free fact (LLM-227), because a producer may run a loss-leader; a
		// reseller selling below its own cost is never that, just a leak.
		//
		// LLM-332: when the good is DEMONSTRABLY underwater, escalate from the bare
		// caution to the two levers a merchant actually holds: buy cheaper or charge
		// more. Live case: Josiah the distributor cut milk as a rational loss-cut
		// rather than carry a losing line, which starved the stew chain downstream. The
		// caution alone gave him no path back to margin; naming the levers does.
		// Advisory ("you may need to"), not a number to anchor haggling on.
		//
		// LLM-385: the caution now fires ONLY for a good actually sold at or below cost,
		// decided on the SUB-COIN AtOrBelowCost / StrictlyBelowCost rates (see the
		// struct fields). This fixes two failures of the old `PaidUnit > 0` trigger: it
		// fired as boilerplate on PROFITABLE resale lines (cheese paid 2, sold 4 carried
		// the same "selling below your costs" tag as a real loser, so the model couldn't
		// tell them apart), and the whole-coin `RecentUnit < PaidUnit` escalation missed
		// sub-coin losses (milk paid 1.39, sold 1.30 both rounded to 1). A profitable or
		// not-yet-sold line now carries no caution at all.
		//
		// LLM-627: where the cost-anchored ask is available it REPLACES the LLM-332
		// two-lever advisory — "raise your price" with a number is that advice made
		// actionable, and two price hints in one clause would hand a weak model a
		// choice where the point is an anchor. Rides LAST in the clause list so the
		// directive with a number is the final word against the trailing-average
		// self-anchor ("of late you have sold for about 3") it exists to break.
		switch {
		case it.AtOrBelowCost && it.AskUnit > 0:
			// The loss statement carries the stake; the ask is the lever.
			clauses += fmt.Sprintf(" — selling below your costs loses you coin; ask %s or more at your counter", coinsPhrase(it.AskUnit))
		case it.AtOrBelowCost:
			clauses += " — selling below your costs loses you coin"
			if it.StrictlyBelowCost {
				// LLM-627: a no-spread good (Low == High) never gets "raise your
				// price" — retail sits at the integer-coin floor and raising it is
				// the documented wrong fix (the water-at-2 chain break). The only
				// lever a merchant holds there is the buy side.
				if it.Low == it.High {
					clauses += "; you may need to negotiate lower costs"
				} else {
					clauses += "; you may need to negotiate lower costs or raise your price"
				}
			}
		case it.AskUnit > 0:
			clauses += fmt.Sprintf(" — ask %s or more at your counter; your shop lives on the difference", coinsPhrase(it.AskUnit))
		}
		// A produced good's cost of goods (LLM-226). All arithmetic is done HERE —
		// the model gets a per-unit phrase to compare against its price, never a
		// batch fraction to divide. Stated as a fact, not a pricing directive
		// (LLM-227): the NPC decides what to do with its cost; a command here
		// over-anchors weak models into rigid haggling and fights a deliberately
		// set loss-leader price.
		//
		// LLM-475: the fact alone was not enough. The miller read flour at ~1 coin
		// beside wheat at ~1 coin and offered five sacks for five sheaves — a day's
		// grinding for nothing — because nothing in the line said the two figures
		// met. The margin judgment states the consequence and stops there: no
		// directive, no number to haggle from, and no praise tier when the good
		// pays, so the phrase's presence is itself the signal.
		if cost := makingsCostPhrase(it); cost != "" {
			clauses += fmt.Sprintf("; the makings run you %s%s", cost, makingsMarginPhrase(it.MakingsMargin))
		}
		fmt.Fprintf(b, "- %s: %s%s.%s\n", sanitizeInline(it.ItemLabel), worth, clauses, keepBack)
	}
	// LLM-292: the earmarked repair nails, last — not a ware with a price but a
	// holding a buyer's offer must be weighed against. States the obligation and
	// the stake (Jeff's live case: Josiah resold 5 repair nails before mending —
	// 10 coins lost, shop still broken). When he carries MORE than the mend takes,
	// the keep-back split is stated so the surplus stays sellable.
	if r := v.RepairReserve; r != nil {
		name := sanitizeInline(r.BusinessName)
		if name == "" {
			name = "place of business"
		}
		label := sanitizeInline(r.ItemLabel)
		if r.Held <= r.Needed {
			fmt.Fprintf(b, "- %s: you need %d of these to mend your %s — the %d you carry are for that mend, not for sale. Selling them leaves your business broken.\n",
				label, r.Needed, name, r.Held)
		} else {
			fmt.Fprintf(b, "- %s: you need %d of these to mend your %s — keep %d back for the mend; only the %d beyond that are yours to sell.\n",
				label, r.Needed, name, r.Needed, r.Held-r.Needed)
		}
	}
	// LLM-294 Tier 2: coin-poor + overstocked → nudge a sale to someone co-present.
	// The cue builds only inHuddle, so there IS an audience to offer to; naming the
	// single most-overstocked ware gives the weak model one concrete thing to act on.
	// Points at the `sell` tool (scene_quote's model-facing name) — a general commerce
	// tool available with an audience — not a buy verb. Stake stated (short of coin).
	if v.SellFirst && v.SellFirstWare != "" {
		fmt.Fprintf(b, "You are short of coin (%d) and holding more %s than folk have been buying. If anyone here would take some, offer it to them now — use the sell tool to name your price.\n",
			v.SellFirstCoins, sanitizeInline(v.SellFirstWare))
	}
	b.WriteString("\n")
}

// distributorLabel names the village distributor for a wholesale producer's
// own-produce cue — the person to route would-be buyers to. The snapshot-side
// sibling of sim.DistributorSteerLabel (which reads live *Actor): prefer the
// distributor structure's owner display name, fall back to the object's own
// display name, then a generic phrase. Best-effort — degrades to "the village
// storekeeper" when none is configured, matching the engine steer's fallback so
// cue and reject stay in step (an in-world phrase on purpose: rendered prose
// never carries the mechanic-role word "distributor", LLM-292). First match wins
// (one distributor by convention).
func distributorLabel(snap *sim.Snapshot) string {
	if snap == nil {
		return "the village storekeeper"
	}
	for _, obj := range snap.VillageObjects {
		if !sim.IsDistributorStructure(obj) {
			continue
		}
		if obj.OwnerActorID != "" {
			if owner := snap.Actors[obj.OwnerActorID]; owner != nil && owner.DisplayName != "" {
				return owner.DisplayName
			}
		}
		if obj.DisplayName != "" {
			return obj.DisplayName
		}
	}
	return "the village storekeeper"
}

// makingsCostPhrase renders a produced good's cost of goods as per-unit prose
// ("about 2 coins each", "at least 3 coins each"), or "" when the item carries no
// cost basis. Shared by the retail line and the wholesale-channel line (LLM-475) so
// the two can't drift on how a cost is spoken or on the floor qualifier.
func makingsCostPhrase(it TradeValueItem) string {
	if it.CostBatch <= 0 || it.CostQty <= 0 {
		return ""
	}
	if it.CostFloor {
		// The sum is missing an unpriceable input, so the true cost exceeds it —
		// state the known part as a whole-coin floor (ceiling division; erring high
		// is the safe direction for a cost estimate).
		ceilUnit := (it.CostBatch + it.CostQty - 1) / it.CostQty
		return fmt.Sprintf("at least %s each", coinsPhrase(ceilUnit))
	}
	return costEachPhrase(it.CostBatch, it.CostQty)
}

// makingsMarginPhrase selects the prose for the engine-derived margin tier
// (LLM-475), or "" for makingsMarginNone. Parenthetical and terse on purpose: it
// rides the end of a cost clause as a verdict on the two figures just stated, and
// a longer form would read as the pricing directive LLM-227 keeps out of this cue.
// There is no above-cost phrase — a keeper is told when its work earns nothing,
// never congratulated when it pays.
func makingsMarginPhrase(m makingsMargin) string {
	switch m {
	case makingsMarginBelowCost:
		return " (a loss)"
	case makingsMarginAtCost:
		return " (it earns you nothing)"
	}
	return ""
}

// coinsPhrase renders a coin count with singular/plural unit ("1 coin", "5 coins").
func coinsPhrase(n int) string {
	if n == 1 {
		return "1 coin"
	}
	return fmt.Sprintf("%d coins", n)
}

// costEachPhrase renders a batch cost as bucketed per-unit prose ("nearly 1 coin
// each", "a little over 2 coins each"). Weak models can compare a phrase against
// their asking price but cannot be trusted to divide batch/qty themselves, so the
// division happens here and the fraction is spoken, not rounded away — 8 coins per
// 10 bowls must NOT collapse to "about 1 coin each" (that erases the margin the
// clause exists to reveal). The half-and-up bucket rounds the phrase UPWARD
// ("nearly N+1") so approximation never understates cost — the failure mode this
// cue guards against is pricing below cost, not above it.
func costEachPhrase(batch, qty int) string {
	if batch <= 0 || qty <= 0 {
		return "" // defensive: render gates on both being positive already
	}
	whole, rem := batch/qty, batch%qty
	switch {
	case rem == 0:
		return fmt.Sprintf("about %s each", coinsPhrase(whole))
	case whole == 0 && rem*2 < qty:
		return "under half a coin each"
	case rem*2 < qty:
		return fmt.Sprintf("a little over %s each", coinsPhrase(whole))
	default:
		return fmt.Sprintf("nearly %s each", coinsPhrase(whole+1))
	}
}
