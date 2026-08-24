package perception

import (
	"fmt"
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// copresent_buy.go — LLM-299. The shared situation-awareness for an owner supply
// errand that puts a seller of the needed item in the buyer's huddle: the nail
// repair-buy ("## Nails to mend your business", stall_repair.go) and the shovel
// farm-upkeep buy ("## Farm upkeep", farm_upkeep.go). Both errands walk the owner
// to a producer and, once co-present, issue the shared renderCoPresentBuy "Buy it
// now" imperative (restock.go). LLM-297 made the nail path situation-aware — cap the
// ask at the seller's live stock, and drop the goad when the purse can't take it on
// or the negotiation has dead-ended — as nail-specific code. This file lifts that one
// mechanism out so nails and shovels run it identically instead of drifting as two
// parallel copies.

// copresentBuyBlock decides how a co-present-buy render treats a seller sharing the
// buyer's huddle. The zero value goads the buy; the blocked variants replace the
// imperative with a hold-off so the cue stops pushing the model to re-offer into an
// empty forge, an unaffordable / already-declined negotiation, or against the
// working-capital gate's own hold-off advice.
type copresentBuyBlock int

const (
	copresentBuyOK             copresentBuyBlock = iota // seller has stock and a deal is plausible — goad the buy
	copresentBuyBlockedNoStock                          // seller holds nothing right now (defensive — coPresentSellerForItem's qty>0 gate normally prevents it)
	copresentBuyBlockedCoin                             // conserve mode or an empty purse — coins must recover before this buy can close
	copresentBuyBlockedTerms                            // repeated declines / unfilled offers this huddle — the deal isn't meeting; try later
)

// copresentStandoffDeclineThreshold is how many seller declines / unfilled-offer
// failures to the same co-present seller in the current huddle mark the negotiation as
// stuck (LLM-297). One decline is ordinary haggling; a second means the terms aren't
// going to meet. An engine-hard insufficient-funds rejection short-circuits to a coin
// block on the first occurrence — an empty purse won't clear by re-offering the same coins.
//
// Aliased to the sim-side constant rather than restated (LLM-525): the same count
// stamps the buyer's ObservedSaleStandoff memory, which is what carries the hold-off
// past the end of the conversation. If the two ever drifted apart, the buyer would be
// told to come back later while still holding a directory entry sending her straight
// back — the exact oscillation the memory exists to stop.
const copresentStandoffDeclineThreshold = sim.SaleStandoffDeclineThreshold

// classifyCoPresentBuy resolves, for a seller of kind sharing the buyer's huddle, the
// seller's live stock of that item and whether the buy is worth goading. Shared by the
// nail repair-buy (buildStallRepairBuy) and the shovel farm-upkeep buy (buildFarmUpkeep)
// so both run one situation-aware mechanism. Callers invoke it only once a seller is
// co-present and no offer is already standing (the pending-offer bide steer wins first).
// The block is chosen in priority order:
//   - no stock (defensive; coPresentSellerForItem's qty>0 gate normally keeps this >=1),
//   - the working-capital gate telling this keeper to hold off (merchantConserve — the
//     same signal the "## Restocking" hold-off rides, so the two cues can never contradict),
//     which reads as a coin block,
//   - no means to pay at all — an empty purse and no SPARE good to put up (the LLM-406
//     gate with its LLM-636 spoken-for reservation), which reads as a coin block too. The
//     walk-to directory already drops such a supplier; without this arm the co-present goad
//     kept saying "Buy it now … goods you carry (pay_items)" to a keeper whose every good the
//     intake gate refuses, and a refused offer mints no ledger entry, so the standoff arm
//     below could never latch and the goad rode every tick (the Hannah↔Josiah treadmill),
//   - a recent negotiation with the seller has dead-ended (coPresentBuyStandoff → coin/terms).
func classifyCoPresentBuy(snap *sim.Snapshot, buyer sim.ActorID, buyerSnap *sim.ActorSnapshot, seller sim.ActorID, kind sim.ItemKind) (int, copresentBuyBlock) {
	stock := 0
	if s := snap.Actors[seller]; s != nil {
		stock = s.Inventory[kind]
	}
	switch {
	case stock <= 0:
		return stock, copresentBuyBlockedNoStock
	case merchantConserve(snap, buyer, buyerSnap).Active:
		return stock, copresentBuyBlockedCoin
	case buyerSnap.SpendableCoins() <= 0 && !sim.HoldsBarterableGoodsExcept(snap.ItemKinds, snap.Recipes, sim.SnapshotBarterHolder(snap, buyerSnap), kind):
		return stock, copresentBuyBlockedCoin
	default:
		return stock, coPresentBuyStandoff(snap, buyer, seller, buyerSnap.CurrentHuddleID, kind)
	}
}

// coPresentBuyStandoff classifies how the buyer's offers of kind to the co-present
// seller have dead-ended in this huddle: copresentBuyBlockedCoin when the purse couldn't
// cover one recently (a single failed_insufficient_funds is definitive), copresentBuyBlockedTerms
// when the seller has declined / couldn't fill at least copresentStandoffDeclineThreshold
// offers, or copresentBuyOK when neither. Mirrors hasPendingOfferTo's ledger walk (buyer,
// seller, item, huddle) but keys on the negative terminal states. Decline counting takes the
// huddle's WHOLE lifetime — no recency filter. It used to count only declines resolved within
// recentlyResolvedOfferWindow (3 min), but re-offers are paced by the staleness wake backoff at
// multi-minute gaps, so consecutive declines aged out of the window before a second could join
// them and the standoff could trip only transiently, never latch — the live Elizabeth↔Ezekiel
// nail loop ran 13 declines in 53 minutes through it (LLM-510). The huddle scope is the
// staleness bound: a fresh conversation starts a fresh counter, and mid-huddle stock recovery
// self-corrects at that boundary too (the soften's own "come back later" exit). The coin arm
// keeps the recency window — a purse can genuinely recover within minutes (a completed sale,
// a labor payout), and merchantConserve already covers ongoing poverty, so only a RECENT
// insufficient-funds fail reads as a coin block. An empty huddle disables the scan —
// co-presence with no shared huddle can't scope "this negotiation".
func coPresentBuyStandoff(snap *sim.Snapshot, buyer, seller sim.ActorID, huddle sim.HuddleID, kind sim.ItemKind) copresentBuyBlock {
	if seller == "" || huddle == "" {
		return copresentBuyOK
	}
	declines := 0
	for _, e := range snap.PayLedger {
		if e == nil || e.BuyerID != buyer || e.SellerID != seller || e.ItemKind != kind || e.HuddleID != huddle {
			continue
		}
		if e.ResolvedAt.IsZero() {
			continue // mid-construction — a terminal state without its resolve stamp yet
		}
		switch e.State {
		case sim.PayLedgerStateFailedInsufficientFunds:
			// Bounded on both sides — a malformed future ResolvedAt must not read as recent.
			if age := snap.PublishedAt.Sub(e.ResolvedAt); age >= 0 && age <= recentlyResolvedOfferWindow {
				return copresentBuyBlockedCoin
			}
		default:
			// The dead-end terminals, via the shared sim predicate (LLM-525): the same
			// count stamps the buyer's ObservedSaleStandoff memory, which is what carries
			// this hold-off past the end of the conversation. Two parallel switches would
			// let the rendered soften and the remembered standoff drift apart.
			if sim.SaleStandoffLedgerState(e.State) {
				declines++
			}
		}
	}
	if declines >= copresentStandoffDeclineThreshold {
		return copresentBuyBlockedTerms
	}
	return copresentBuyOK
}

// renderCoPresentBuyPending writes the bide steer for a co-present seller when an offer of
// the item already stands with them: wait for the answer rather than re-offering and
// churning (the LLM-64 co-present-offer guard). itemLabel is the singular item noun ("nail",
// "shovel"). Inputs are sanitized here, so callers pass raw snapshot strings.
func renderCoPresentBuyPending(b *strings.Builder, seller, itemLabel string) {
	fmt.Fprintf(b, "%s is here with you and your %s offer is still with them — wait here for their answer; do not re-offer or leave.\n",
		sanitizeInline(seller), itemLabel)
}

// renderCoPresentBuySoften writes the hold-off prose that replaces the "Buy it now" goad
// when the co-present buy is blocked. itemLabel is the plural item noun ("nails", "shovels").
// A copresentBuyOK block writes nothing (the caller renders the goad / cap instead). Inputs
// are sanitized here, so callers pass raw snapshot strings.
func renderCoPresentBuySoften(b *strings.Builder, seller, itemLabel string, block copresentBuyBlock) {
	s := sanitizeInline(seller)
	switch block {
	case copresentBuyBlockedNoStock:
		// Defensive: a co-present seller normally holds >=1, but if his stock has emptied,
		// say so instead of goading a buy he can't fill. Item-neutral (LLM-308) — the
		// helper now serves restock goods (sage, milk) as well as the smith-forged nails
		// and shovels, so no "still forging" verb that only fits a smith.
		fmt.Fprintf(b, "%s is here with you but has no %s to spare just now — come back once he has more rather than pressing him for stock he hasn't got.\n", s, itemLabel)
	case copresentBuyBlockedCoin:
		// The purse can't take this on (conserve mode, or an offer already failed for want
		// of coin) — soften to a hold-off that harmonizes with the "## Restocking" advice
		// instead of goading a re-offer it can't afford.
		fmt.Fprintf(b, "%s is here with you, but your purse can't take on %s just now — hold off buying and let your coins recover before you come back for them.\n", s, itemLabel)
	case copresentBuyBlockedTerms:
		// Repeated offers have gone nowhere this huddle — the terms aren't meeting, so stop
		// pressing and come back later rather than re-offering into the same no.
		fmt.Fprintf(b, "%s is here with you, but your offers for %s aren't finding a deal right now — hold off and come back later rather than pressing them into a no.\n", s, itemLabel)
	}
}

// renderCoPresentBuyCapped writes the partial-fill arm: the seller can't cover the whole
// shortfall, so name what he holds and cap the pay_with_item "qty up to N" at his stock
// instead of goading the full shortfall for stock he can't deliver (the live case: the
// buyer needed 5 nails, the smith held only 1). itemLabel is the plural item noun; stock is
// the seller's live count. Item-neutral "come back once he has more" (LLM-308) — no
// forge-specific verb, so the arm reads for a restock good as well as a forged one. Inputs
// are sanitized here, so callers pass raw snapshot strings.
func renderCoPresentBuyCapped(b *strings.Builder, seller, itemLabel string, kind sim.ItemKind, stock int) {
	fmt.Fprintf(b, "%s can spare only %d just now, so buy what he has and come back for the rest once he has more. ", sanitizeInline(seller), stock)
	renderCoPresentBuy(b, seller, itemLabel, kind, stock)
}
