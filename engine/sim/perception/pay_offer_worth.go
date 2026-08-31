package perception

import (
	"math"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// offerWorth is the engine's judgment of a BARTER offer's two sides against each
// other — what the seller would hand over, against what the buyer puts up for it
// (LLM-598). The engine does the arithmetic; render selects the phrase. That split
// is the felt-needs / makingsMargin shape, and it exists because the arithmetic is
// exactly what a weak model skips: live, the miller was shown "7 wheat for 7 flour"
// on the offer line and the coin worth of both goods further down the same prompt,
// recited both rates correctly, and accepted 21 coins of flour for 7 coins of wheat
// because seven and seven look even (virtual_agent_calls 155501, pay_ledger 3710).
//
// Deliberately has no praise tier, following makingsMargin: a keeper needs telling
// when it is about to give its work away, not congratulating when a deal is good.
// Silence is the common case.
type offerWorth int

const (
	// offerWorthUnknown covers every case the engine cannot total honestly — a
	// pure-coin offer (already in one unit, nothing to translate), an unpriced
	// good on either side, or an empty offer. Render writes nothing.
	offerWorthUnknown offerWorth = iota
	offerWorthFair
	offerWorthThin
	offerWorthShort
)

// offerWorthShortDivisor is the band cutoff: payment worth this fraction or less of
// the goods asked for reads as plainly short rather than merely thin. Coarse on
// purpose — the per-unit worths feeding it are whole coins, so a finer grade would
// be grading noise (the same reasoning as the restock profit bands).
const offerWorthShortDivisor = 2

// offerWorthCeiling bounds every side total. Both sides are (ledger quantity ×
// ledger-derived worth), and a product that wrapped would not merely garble the
// verdict — a negative total enters the short band and renders a confident "far
// less" on a generous offer (code_review). Nothing in the village approaches this;
// a total past it means the inputs are junk, and junk earns silence.
const offerWorthCeiling = int64(math.MaxInt32)

// offerSideValue is unit × qty, guarded. The bound is checked by DIVISION before the
// multiply, so nothing is ever computed that could wrap. Reports false for a
// non-positive operand or a product past the ceiling — both of which abandon the
// whole judgment rather than contributing a wrong figure to it.
func offerSideValue(unit, qty int) (int64, bool) {
	if unit <= 0 || qty <= 0 {
		return 0, false
	}
	if int64(qty) > offerWorthCeiling/int64(unit) {
		return 0, false
	}
	return int64(unit) * int64(qty), true
}

// offerItemUnitWorth is what one unit of a good is worth TO THIS ACTOR, in whole
// coins, resolved realized-first:
//
//  1. what it has actually been getting for the good (its own recent sales),
//  2. else what it has actually been paying for it (its own recent purchases),
//  3. else the catalog seed (wholesale, else retail),
//  4. else 0 — unpriced, which makes the whole offer unjudged.
//
// The same function prices BOTH sides. An asymmetric basis (sell rate for what goes
// out, buy rate for what comes in) would book the retail-wholesale spread as a loss
// on every honest trade and make the clause boilerplate.
//
// Realized rates beat the catalog because the catalog is a seed and the ledger is
// what the village actually does — the observed-first resolution BulkUnit/ShopUnit
// already use. A zero-coin history is not a price signal (a barter or gift leg has
// units with no coins), so it falls through rather than pricing the good at nothing.
//
// Rounding is to the nearest coin, matching the figures the wares cue displays: this
// verdict shares a prompt with those numbers and must not contradict them. A good
// worth under half a coin therefore rounds to 0 and reads as unpriced — silence,
// which is the safe direction.
//
// This is MARKET worth, not worth-to-this-actor-in-use: a good the actor could
// transform into something dearer is priced at what the village pays for it. That is
// conservative in NEITHER direction — pricing an incoming input at market
// UNDERSTATES the payment, which biases toward a false SHORT, not toward silence
// (code_review corrected this; the reverse claim stood here and was wrong). The
// exposure is a recipe that multiplies units: paying a baker in flour, where 2 flour
// becomes 6 journeycakes, reads thin at market prices though the baker does well out
// of it. It does not bite the 1-for-1 conversions the village mostly runs on — the
// mill turns 5 wheat into 5 flour, so flour-for-wheat at parity really is a morning's
// grinding for nothing.
//
// Accepted because the cue GATES NOTHING: a false short costs a seller a phrase he
// may disregard, and he keeps accept_pay either way. Pricing transformation value is
// a redesign, not a clause.
func offerItemUnitWorth(snap *sim.Snapshot, actorID sim.ActorID, kind sim.ItemKind) int {
	worth, _ := offerItemUnitWorthSource(snap, actorID, kind)
	return worth
}

// worthSource is which rung of offerItemUnitWorth's resolution ladder produced a
// figure (LLM-647). The pack-worth cue words each figure by its provenance — a
// purchase-derived number is what the keeper has been PAYING, and presenting it
// as what the good "fetches from your counter" would launder a past overpayment
// into a retail realization (code_review, LLM-647).
type worthSource int

const (
	worthNone worthSource = iota
	worthFromSales
	worthFromPurchases
	worthFromCatalog
)

// offerItemUnitWorthSource is offerItemUnitWorth with the resolution rung kept:
// the same realized-first ladder, returning WHERE the figure came from alongside
// the figure. The single resolver both callers share — the offer verdict discards
// the source (its clause carries no numbers), the pack-worth cue words by it.
func offerItemUnitWorthSource(snap *sim.Snapshot, actorID sim.ActorID, kind sim.ItemKind) (int, worthSource) {
	if snap == nil {
		return 0, worthNone
	}
	if units, coins := sellerRecentSales(snap, actorID, kind, restockSalesWindow); units > 0 && coins > 0 {
		return (coins + units/2) / units, worthFromSales
	}
	if units, coins := buyerRecentPurchases(snap, actorID, kind, restockSalesWindow); units > 0 && coins > 0 {
		return (coins + units/2) / units, worthFromPurchases
	}
	recipe := snap.Recipes[kind]
	if recipe == nil {
		return 0, worthNone
	}
	if recipe.WholesalePrice > 0 {
		return recipe.WholesalePrice, worthFromCatalog
	}
	if recipe.RetailPrice > 0 {
		return recipe.RetailPrice, worthFromCatalog
	}
	return 0, worthNone
}

// offerWorthOf judges one pending offer. Barter only: an offer paid purely in coin
// is left unknown because the seller is already comparing coins to goods with the
// price book in hand — the translation this clause exists to do has nothing to do.
// Coins in a MIXED payment do count, at face value.
//
// Any single unpriced good — on either side — abandons the whole judgment rather
// than totalling what is left, since a partial sum understates the payment and
// would invent a shortfall out of missing data. A MALFORMED payment leg is treated
// the same way and deliberately not skipped: skipping a zero- or negative-quantity
// entry would quietly price a barter offer off its coin leg alone and read as short
// (code_review). Every leg must be judgeable or the offer is not judged.
func offerWorthOf(snap *sim.Snapshot, actorID sim.ActorID, o sim.PayOfferWarrantReason) offerWorth {
	if len(o.PayItems) == 0 || o.Qty <= 0 || o.Amount < 0 {
		return offerWorthUnknown
	}
	askedUnit := offerItemUnitWorth(snap, actorID, o.Item)
	out, ok := offerSideValue(askedUnit, o.Qty)
	if !ok {
		return offerWorthUnknown
	}

	in := int64(o.Amount) // coins on a mixed payment; 0 for pure barter
	for _, pi := range o.PayItems {
		leg, ok := offerSideValue(offerItemUnitWorth(snap, actorID, pi.Kind), pi.Qty)
		if !ok {
			return offerWorthUnknown
		}
		if in > offerWorthCeiling-leg {
			return offerWorthUnknown
		}
		in += leg
	}

	// Compared by DIVIDING the goods asked for, never by multiplying the payment:
	// division cannot overflow for positive operands, and both sides are guaranteed
	// positive and bounded above (offerSideValue). Truncation rounds the threshold
	// DOWN, so the short band is only ever harder to enter — the same reasoning
	// restockMarginTierOf records.
	switch {
	case in <= out/offerWorthShortDivisor:
		return offerWorthShort
	case in < out:
		return offerWorthThin
	default:
		return offerWorthFair
	}
}

// buildPayOfferWorth judges every pending offer staked against the subject, keyed by
// LedgerID for renderPayOffers. Returns nil when nothing is judgeable, keeping
// render free of the catalog. Entries that grade fair or unknown are omitted
// entirely — render has no phrase for them, and carrying them would invite a later
// reader to add one.
func buildPayOfferWorth(snap *sim.Snapshot, actorID sim.ActorID, offers []sim.PayOfferWarrantReason) map[sim.LedgerID]offerWorth {
	if snap == nil || len(offers) == 0 {
		return nil
	}
	var out map[sim.LedgerID]offerWorth
	for _, o := range offers {
		tier := offerWorthOf(snap, actorID, o)
		if tier != offerWorthThin && tier != offerWorthShort {
			continue
		}
		if out == nil {
			out = make(map[sim.LedgerID]offerWorth)
		}
		out[o.LedgerID] = tier
	}
	return out
}

// offerWorthPhrase is the render half: one clause, no numbers. The figures are
// already in the wares cue; repeating them here would hand the model the same
// arithmetic it failed to do. Names the asked good so the clause reads as a
// sentence about the trade rather than a verdict floating free.
func offerWorthPhrase(tier offerWorth, askedLabel string) string {
	switch tier {
	case offerWorthThin:
		return " — a little less than the " + askedLabel + " is worth"
	case offerWorthShort:
		return " — far less than the " + askedLabel + " is worth"
	default:
		return ""
	}
}
