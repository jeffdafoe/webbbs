package perception

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// satiation.go — ZBBS-HOME-304. The "## What you can eat or drink" perception
// section: surfaces, to a hungry or thirsty NPC, how to resolve the pressing
// need — the satisfying items it ALREADY CARRIES (consume-first), free public
// sources it can walk to (a well, a fruit tree), and nearby vendors selling a
// satisfier. Port of v1's engine/satiation.go, adapted to v2: vendor cues run
// through the shared findVendorConsumables finder (see consumable_vendors.go),
// the same structural-vendorship + workplace surface the recovery-options
// remedy arm uses.
//
// Free-source arm (ZBBS-HOME-359; re-based on memory by LLM-79): hunger/thirst
// had no analogue to the recovery section's free rest spots (gatherFreeRestSpots),
// so a thirsty NPC away from the well never saw it — the only path was the gather
// cue, which fires only once already standing at the source. It carries each
// source's object id so move_to can walk there (the id / name paths resolve a
// bare refresh source to an object visit — sim/move_to.go). LLM-79 retired the
// omniscient all-wells-in-the-village scan: gatherFreeSatiationSources now
// surfaces the UNION of sources the actor REMEMBERS using (known-places, any
// distance) and sources within the scene radius (newly-seen) — so an NPC
// remembers the well it drank at yesterday but is no longer god-shown a well it
// has never visited across the map.
//
// Why the own-stock line is the load-bearing half (v1's ZBBS-123 diagnosis): a
// herbalist hungry on shift while holding 50 berries walked to a store instead
// of eating them, because nothing connected "you're hungry" to the consume
// tool — the inventory read framed the berries as merchandise. The own-stock
// line states the connection outright: "you have berries — consume to eat."
//
// Scope is hunger + thirst only. Tiredness's resolution is spatial (rest spot /
// home / inn / brew), so it lives in recovery_options.go's unified list, not
// here — same split v1 settled on (ZBBS-HOME-206).

// satiationNeeds is the fixed render order — hunger before thirst, matching the
// order pressing needs appear elsewhere in the prompt.
var satiationNeeds = []sim.NeedKey{"hunger", "thirst"}

// SatiationView is the content-gated "## What you can eat or drink" section.
// A nil view (or empty Needs) means render omits the section.
type SatiationView struct {
	Needs []SatiationNeedView
}

// SatiationNeedView is one pressing consumable need's resolution surface.
type SatiationNeedView struct {
	Need sim.NeedKey // "hunger" | "thirst"
	Verb string      // "eat" | "drink"

	// Level is the actor's CURRENT level of this need at build time, feeding
	// the per-unit sufficiency clause (ZBBS-WORK-392): when one unit of an
	// offered item would fully zero it, the render says so ("a single one
	// would fully satisfy your hunger") — the informed-buying fact whose
	// absence let a starving buyer accept a seller-pitched 10-meat bundle.
	Level int

	OwnStock []OwnStockItem // satisfiers the actor already carries (shared shape)

	// CoPresentPeers are huddle peers standing with the subject RIGHT NOW who
	// carry an item that eases this need — the immediately-actionable
	// buy-from-the-person-beside-you affordance (ZBBS-HOME-342). pay_with_item
	// resolves the seller as any co-present huddle peer holding the goods (it is
	// NOT vendor-gated), so the transaction substrate already supports this; the
	// only gap was that perception never named the co-present holder. Rendered
	// AHEAD of Vendors and carries NO structure_id — they're already here, so
	// nothing should tempt a move_to.
	CoPresentPeers []SatiationPeerOffer

	// FreeSources are free, public refresh placements the actor can walk to and
	// consume in place at no cost — a well (thirst), a fruit tree (hunger). The
	// hunger/thirst analogue of the recovery section's free rest spots. Rendered
	// AHEAD of Vendors (free beats paid) and AFTER CoPresentPeers (a walk is more
	// than buying from someone already beside you). ZBBS-HOME-359.
	FreeSources []SatiationFreeSource

	Vendors []SatiationVendor // nearby places selling a satisfier (walk to)

	// BridgeToMeal is set when the actor carries only snack-class food for this
	// need (nothing at satiationMealFloor) yet the walk-to directory offers a real
	// meal — the LLM-307 snacking-loop case. Render prints a bridging line between
	// the consume-first own-stock line and the directory, saying plainly that the
	// snack won't resolve the need and to see the meal options below. False in the
	// ordinary case (a meal-class satisfier on hand, or the directory already
	// riding at the red tier), where no bridge is needed.
	BridgeToMeal bool
}

// SatiationFreeSource is one free, public refresh source for the need — a well
// (thirst), a fruit tree (hunger) — the actor can walk to and consume at in
// place. The object-keyed analogue of the recovery section's free rest spots
// (RecoveryOption Kind "rest"). ObjectID is the move_to handle: a bare refresh
// source resolves by id or name to an object visit (sim/move_to.go), so the cue
// surfaces it as a structure_id the same way every other actionable cue does.
// ZBBS-HOME-359.
type SatiationFreeSource struct {
	Label     string              // "Well" — the source's display name
	ObjectID  sim.VillageObjectID // move_to handle (rendered as structure_id)
	Magnitude int                 // immediate need eased on arrival (positive)
	Distance  string              // qualitative ("a short walk"); "" when unknown
	Direction string              // cardinal ("northeast"); "" when coincident

	// sortKey is the actor→source tile distance used to order bullets (nearest
	// first). Unexported — never rendered.
	sortKey float64
	// sourceKey is the object id — a stable final tie-break so equal-distance,
	// equal-label sources order deterministically (VillageObjects is a map).
	// Unexported — never rendered.
	sourceKey string
}

// SatiationPeerOffer is one co-present huddle peer who carries a satisfier for
// the need — a buy-it-now-from-them affordance with no walk. PeerLabel is the
// acquaintance-gated name (descriptorLabel), never a raw UUID.
type SatiationPeerOffer struct {
	PeerLabel string       // "Hannah" | "the herbalist" | "a stranger"
	ItemLabel string       // "stew"
	Magnitude int          // immediate need eased per unit (positive)
	peerID    sim.ActorID  // sort key only — never rendered (huddle order is by ID)
	itemKind  sim.ItemKind // sort tie-break only — never rendered
}

// SatiationVendor is one (workplace, item) buy opportunity.
type SatiationVendor struct {
	StructureLabel string          // "PW Apothecary" — where the buyer walks to
	StructureID    sim.StructureID // the workplace's key — passed to move_to to walk there
	ItemLabel      string          // "stew"
	Magnitude      int
	CostText       string // "~3 coins" | "about 2 coins each" (multi-unit, LLM-620) | "ask the seller"

	// costCoins is the per-buyer last-paid price as a number, 0 when the actor has
	// never bought this item here (CostText is then the "ask the seller" fallback).
	// Unexported — never rendered; the LLM-176 need-redirect reads it to skip a
	// vendor whose remembered price the looping actor can't meet, while leaving an
	// unknown-price (0) vendor a valid redirect to go and learn the price.
	costCoins int

	// Barter is true when the buyer cannot cover this vendor's price with coins
	// but holds goods it could put up in trade — the means-to-pay "barter" state
	// (LLM-222). Render swaps the coin-price hint for a goods-in-trade steer. A
	// vendor the buyer can pay by coin (or an unknown-price vendor it holds coins
	// for) carries Barter=false and renders the normal buy line; a vendor the
	// buyer can neither pay nor barter for is dropped at build (gatherSatiation-
	// Vendors) and never reaches a SatiationVendor at all. This replaces the old
	// experiential-shut annotation: a remembered-shut vendor is now DROPPED like a
	// dead-end supplier (LLM-216 restock parity), not surfaced with a "found it
	// shut up" note the weak model toured anyway.
	Barter bool

	// OutOfStock is true when the subject has a live experiential memory of
	// trying to buy THIS item here and finding it out of stock within the decay
	// window — render annotates the line so the model deprioritizes the trip.
	// ZBBS-HOME-363.
	OutOfStock bool

	// EatHere is true when the item always settles eat-here (consumable,
	// neither service nor portable — ItemKindDef.EatHereOnly). Render states
	// the fact on the line so the buyer plans a sit-down, not a carry-out
	// the WORK-405 clamp would quietly rewrite. ZBBS-WORK-405.
	EatHere bool
}

// personalOwnStock returns only the own-stock items that are NOT the actor's
// trade goods — its personal provisions. Used to demote a producer's own
// merchandise/ingredients out of the eat cue below the desperation tier
// (LLM-134). Returns nil when nothing personal remains, so the caller's
// empty-section gate omits the cue rather than rendering an empty line.
func personalOwnStock(items []OwnStockItem) []OwnStockItem {
	var out []OwnStockItem
	for _, it := range items {
		if it.TradeStock {
			continue
		}
		out = append(out, it)
	}
	return out
}

// dropWholesalerProduce removes a wholesaler owner's own produce from the eat cue
// at EVERY tier — stricter than the LLM-134 TradeStock demotion, which lets trade
// stock return at the red/distress tier as a last resort. A wholesaler's produce is
// stock to sell, never its larder (LLM-267), so it must never be offered as food,
// even at starvation. Keyed on the SAME sim.IsOwnProduce the Consume guard rejects
// on, so the cue can't offer what the guard would block. No-op for non-wholesalers
// (SellerAtWholesaler is false), so an ordinary producer's cue is untouched.
func dropWholesalerProduce(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, items []OwnStockItem) []OwnStockItem {
	if len(items) == 0 || snap == nil || actorSnap == nil {
		return items
	}
	var out []OwnStockItem
	for _, it := range items {
		if sim.IsOwnProduce(snap.VillageObjects, actorSnap.WorkStructureID, actorSnap.RestockPolicy, it.kind) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// dropDrinkSatisfiers removes drink-category satisfiers from an own-stock list —
// the LLM-318 hunger-line filter. A drink that also eases hunger (ale is belly-
// filling) is a bonus source of hunger relief, not food, so the "what you can
// eat" hunger line must not offer it alongside real food; the caller applies
// this to the hunger group only. Keyed on the intrinsic item category
// (ItemKindDef.IsDrink), nil-safe for a discovery-minted kind absent from the
// catalog (kept, since it isn't a known drink). Returns nil when nothing
// survives, so the caller's empty-section gate omits the cue.
func dropDrinkSatisfiers(snap *sim.Snapshot, items []OwnStockItem) []OwnStockItem {
	if len(items) == 0 || snap == nil {
		return items
	}
	var out []OwnStockItem
	for _, it := range items {
		if snap.ItemKinds[it.kind].IsDrink() {
			continue
		}
		out = append(out, it)
	}
	return out
}

// holdsBarterableGoods reports whether the actor carries any goods it could put
// up in a pay_with_item / offer_trade bundle — the "goods" half of the LLM-222
// means-to-pay gate. Delegates to the shared sim predicate (LLM-406), which the
// restock cue's own means-to-pay gate and its warrant-side mirror
// (sim.buyerCanTransact) also read, so every buy gate in the engine agrees on what
// "has something to pay with" means and none of them can drift back to coins-only.
//
// No exclusion ("" ): the consumer buy cue is a buyer paying for something it means
// to CONSUME, so every SPARE tradeable good it carries is fair payment — a service or
// an eat-here-only food is not (resolvePayItems rejects both; LLM-445), and neither
// are the goods it keeps to work with or the clothes on its back (the spoken-for
// reservation, LLM-636), so a pack holding nothing else reads as no means to barter.
// The restock cue passes the item being bought instead — restocking a line of stock
// by offering that same stock is not a payment path (see HoldsBarterableGoodsExcept).
func holdsBarterableGoods(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot) bool {
	return sim.HoldsBarterableGoodsExcept(snap.ItemKinds, snap.Recipes, sim.SnapshotBarterHolder(snap, actorSnap), "")
}

// satiationMealFloor is the itemFeltAmount magnitude at which a satisfier reads
// as a full meal — hunger "a good meal", thirst "a drink" — rather than a snack
// (a nibble / small bite for hunger, a sip for thirst). LLM-307 keys the
// consume-first suppression on it: own stock at/above the floor is a meal that can
// resolve the need; below it is a snack that can't. Kept in lockstep with
// itemFeltAmount's bands (both use 5 as the snack→meal boundary for hunger and
// thirst) — recalibrate the two together if the felt scale changes.
const satiationMealFloor = 5

// isMealClassSatisfier reports whether a per-unit need magnitude reads as a full
// meal (or a real drink) rather than a snack — see satiationMealFloor.
func isMealClassSatisfier(magnitude int) bool {
	return magnitude >= satiationMealFloor
}

// ownStockIsSnackOnly reports whether EVERY satisfier the actor carries is
// snack-class (below the meal floor). This is the LLM-307 condition under which
// the consume-first line must NOT hide the meal directory: nibbling a snack can't
// resolve a persisting hunger, so a hungry actor holding only berries loops
// (nibble → still hungry → only own stock shown → nibble again) while a meal it
// could walk to stays invisible. Callers guard len(own) > 0 first — an empty list
// satisfies this vacuously, but no own stock opens the directory on its own path.
func ownStockIsSnackOnly(own []OwnStockItem) bool {
	for _, it := range own {
		if isMealClassSatisfier(it.Magnitude) {
			return false
		}
	}
	return true
}

// satiationDirectoryHasMeal reports whether the walk-to directory (co-present
// peers, free sources, vendors) offers at least one meal-class satisfier. LLM-307
// gates the snack-loop re-open on it: re-opening the directory only helps if it
// actually holds a meal the snack-carrying actor can move to — with nothing but
// more snacks out there, the LLM-139 suppression stands (a bridging line pointing
// at "the options below" must have a meal to point at).
func satiationDirectoryHasMeal(peers []SatiationPeerOffer, free []SatiationFreeSource, vendors []SatiationVendor) bool {
	for _, v := range vendors {
		if isMealClassSatisfier(v.Magnitude) {
			return true
		}
	}
	for _, p := range peers {
		if isMealClassSatisfier(p.Magnitude) {
			return true
		}
	}
	for _, f := range free {
		if isMealClassSatisfier(f.Magnitude) {
			return true
		}
	}
	return false
}

// buildSatiation builds the eat/drink view for actorSnap, or nil when no
// consumable need is felt or no satisfier exists. Pure over the snapshot.
func buildSatiation(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot) *SatiationView {
	if snap == nil || actorSnap == nil {
		return nil
	}
	var needs []SatiationNeedView
	for _, need := range satiationNeeds {
		// Gate on AWARENESS, not distress: the eat/drink options surface on the
		// SAME boundary as the "You feel …" line (renderFeltNeeds → the NeedSilent
		// floor), not the higher red threshold. Gating the resolution at red while
		// the felt line fired at the silent floor opened a dead zone — the NPC was
		// told it felt thirsty yet shown no way to slake it until nearly parched,
		// so it wandered toward a remembered tavern instead of the free well (the
		// Ezekiel well-blind cycle, ZBBS-WORK-414). If a need is felt, its options
		// ride along; below the silent floor it isn't aware of the need, so the
		// section stays closed — and the felt line is silent there too.
		tier := sim.NeedLabelTier(actorSnap.Needs[need], snap.NeedThresholds.Get(need))
		if tier == sim.NeedSilent {
			continue
		}
		own := gatherOwnStock(snap, actorSnap, need)
		// LLM-318: a drink that also eases hunger (belly-filling ale) is not food.
		// Its hunger relief is a bonus, not what it's for — so keep it OUT of the
		// hunger "what you can eat" line, where it would otherwise sit next to bread
		// as if it were a meal. It still surfaces under thirst (its own identity)
		// and still eases hunger when consumed; this only stops perception from
		// advertising a drink as a hunger solution. Thirst is untouched (drinks
		// belong there). Applied before the trade/tier demotions below — a drink is
		// never a hunger option at any tier.
		if need == "hunger" {
			own = dropDrinkSatisfiers(snap, own)
		}
		// LLM-267: a wholesaler owner's own produce is stock to sell, not food —
		// strip it at EVERY tier, BEFORE the LLM-134 tier demotion below (which
		// otherwise lets own trade stock return at red). Keyed on sim.IsOwnProduce,
		// the same predicate the Consume guard blocks on, so cue and block agree.
		own = dropWholesalerProduce(snap, actorSnap, own)
		// Demote the actor's own TRADE stock to a desperation-only option
		// (LLM-134). The own-stock cue exists so a hungry actor eats what it
		// carries rather than starving en route to a shop (ZBBS-123), but it
		// can't tell personal provisions from the merchandise/ingredients the
		// actor produces or buys to sell — so a producer was nudged to graze its
		// own goods (Moses eating his farm carrots, Elizabeth her cheese). Below
		// the red/distress tier we drop the actor's own trade-manifest items from
		// the eat cue, steering it to a real meal / vendor / forage; at red-or-
		// worse (about to starve, nothing else) the trade stock returns as the
		// last resort the cue was built to be. Personal food (not in the manifest)
		// always rides at the felt floor.
		if tier < sim.NeedRed {
			own = personalOwnStock(own)
		}
		// LLM-139: at a mildly-felt need, personal food already on hand is reason
		// enough to just eat what you carry — the own-stock "consume to eat" line is
		// the answer, and the walk-to directory of peers / free sources / vendors is
		// noise (the 14-line food directory shown to a peckish NPC already carrying
		// food). So below the red/distress tier, when personal stock survives, suppress
		// the directory.
		//
		// LLM-307 narrows that suppression to be meal-aware. Presence alone isn't
		// enough: a SNACK-only larder (all nibble/small-bite for hunger, a sip for
		// thirst — nothing at satiationMealFloor) can't resolve a persisting need, so
		// hiding the meal directory behind it produces a starvation-by-snacking loop
		// (nibble → still hungry → only own stock shown → nibble again; the need never
		// climbs to red, so the directory never returns). When the surviving stock is
		// snack-only, re-open the directory and flag a bridging line — but only if it
		// truly offers a meal to walk to (satiationDirectoryHasMeal, checked after
		// gather); with nothing but more snacks out there, the suppression stands. A
		// meal-class satisfier on hand still suppresses exactly as before.
		showDirectory := tier >= sim.NeedRed || len(own) == 0
		bridgeToMeal := false
		if !showDirectory && ownStockIsSnackOnly(own) {
			showDirectory = true
			bridgeToMeal = true
		}
		var peers []SatiationPeerOffer
		var free []SatiationFreeSource
		var vendors []SatiationVendor
		if showDirectory {
			peers = gatherCoPresentPeerOffers(snap, actorID, actorSnap, need)
			free = gatherFreeSatiationSources(snap, actorID, actorSnap, need)
			vendors = gatherSatiationVendors(snap, actorID, actorSnap, need)
		}
		if bridgeToMeal && !satiationDirectoryHasMeal(peers, free, vendors) {
			// Nothing but snacks (or nothing) reachable — the bridge would point at a
			// meal that isn't there, so fall back to the LLM-139 suppression.
			peers, free, vendors = nil, nil, nil
			bridgeToMeal = false
		}
		if len(own) == 0 && len(peers) == 0 && len(free) == 0 && len(vendors) == 0 {
			continue
		}
		needs = append(needs, SatiationNeedView{
			Need:           need,
			Verb:           satiationVerb(need),
			Level:          actorSnap.Needs[need],
			OwnStock:       own,
			CoPresentPeers: peers,
			FreeSources:    free,
			Vendors:        vendors,
			BridgeToMeal:   bridgeToMeal,
		})
	}
	if len(needs) == 0 {
		return nil
	}
	return &SatiationView{Needs: needs}
}

// maxSatiationVendors caps the buy menu to the N nearest vendor structures per
// need (ZBBS-HOME-363 altitude). The live prompt had ~20 undifferentiated
// options — the Tavern repeated once per item it sells, no distance bound — so a
// hungry NPC re-sampled a sprawling menu each tick instead of picking the
// closest place to eat. The cap + dedup-by-structure + nearest-first sort below
// turn that into a short, ordered list.
const maxSatiationVendors = 4

// satiationVendorCandidate is one structure's chosen representative offer plus
// the keys the altitude pass orders + dedups on.
type satiationVendorCandidate struct {
	sv         SatiationVendor
	distTiles  float64
	outOfStock bool
	magnitude  int
	itemKind   sim.ItemKind
}

// gatherSatiationVendors maps the shared vendor-consumable finder into the
// satiation bullet shape and applies ALTITUDE (ZBBS-HOME-363): exclude the
// buyer's own workplace, DEDUP to one representative item per vendor structure
// (so the Tavern stops repeating per item), order the structures NEAREST-FIRST,
// and cap to maxSatiationVendors. The per-structure representative prefers an
// item NOT remembered out of stock, then the strongest satisfier — so the menu
// shows something the NPC can actually buy when possible.
//
// It also applies the LLM-222 buy-cue gates, mirroring the LLM-216 restock drop
// so the two consumer cues never disagree:
//   - Seller-availability: DROP a vendor the buyer remembers finding shut (was an
//     annotate-only "found it shut up" note the weak model toured anyway).
//   - Means-to-pay (coin-OR-goods): a vendor the buyer can pay by coin renders
//     normally; one it can't cover by coin but can barter for carries Barter;
//     one it can neither pay nor barter for is a hard payment dead-end and is
//     DROPPED. Read coin-or-goods, not coin-only (restock's gate), because the
//     consumer can barter — a coin-only check would re-introduce false dead-ends.
func gatherSatiationVendors(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, need sim.NeedKey) []SatiationVendor {
	coins := actorSnap.SpendableCoins()
	hasGoods := holdsBarterableGoods(snap, actorSnap)
	byStructure := make(map[sim.StructureID]satiationVendorCandidate)
	for _, vc := range findVendorConsumables(snap, actorID, need, "ask the seller") {
		// No resolvable workplace → no structure_id for move_to (unactionable);
		// own workplace excluded so a hungry vendor doesn't get steered to buy
		// from itself (the Josiah-buys-at-his-own-General-Store case).
		if vc.StructureID == "" || vc.StructureID == actorSnap.WorkStructureID {
			continue
		}
		// LLM-222 seller-availability gate: drop a vendor the buyer remembers
		// finding shut, mirroring LLM-216's restock drop. Annotating it (the old
		// ZBBS-HOME-353 posture) left the weak model touring the dead ends (the
		// live Ezekiel→Inn→asleep-Hannah walk). The shut memory is experiential and
		// TTL-decayed — now capturing an abed keeper too (LLM-126) — so the vendor
		// reappears once it lapses, preserving the retry without the wasted trip.
		if businessRememberedShut(snap, actorSnap, vc.StructureID) {
			continue
		}
		// LLM-222 means-to-pay gate: gate on the ability to TRANSACT, not on
		// brokenness. A 0-coin buyer is not a dead-end now that barter works, so
		// suppressing on coins==0 would hide a viable goods-in-trade path — the
		// exact anti-pattern this ticket fixes. The only hard dead-end is no means
		// of payment at all.
		barter := false
		switch {
		case vc.costCoins > 0 && coins >= vc.costCoins:
			// Coins cover the remembered price — normal buy line.
		case vc.costCoins == 0 && coins > 0:
			// Unknown price but the buyer has coins to spend — keep a normal buy
			// line (walk over and pay / learn the price), the same call LLM-216
			// makes for an unknown-price restock supplier; it can still barter on
			// arrival.
		case hasGoods:
			// Coins can't cover it (none, or below a known price) but the buyer
			// holds goods to put up in trade — keep the cue, steered to barter.
			barter = true
		default:
			// No coins that cover it and nothing to trade — the genuine payment
			// dead-end (the 55-hit pay_with_item no-offer rejection). Drop the
			// line; the free-food / own-stock cues already cover this actor.
			continue
		}
		outOfStock := businessRememberedOutOfStock(snap, actorSnap, vc.StructureID, vc.ItemKind)
		cand := satiationVendorCandidate{
			sv: SatiationVendor{
				StructureLabel: vc.StructureLabel,
				StructureID:    vc.StructureID,
				// LLM-113: the full buy-cue noun — "a wedge of cheese" when the kind
				// has a singular phrase, else the bare menu label (no article glued
				// onto a phrase-less label). Render prints it verbatim.
				ItemLabel:  buyCueNoun(snap, vc.ItemKind),
				Magnitude:  vc.Magnitude,
				CostText:   vc.CostText,
				costCoins:  vc.costCoins,
				Barter:     barter,
				OutOfStock: outOfStock,
				EatHere:    snap.ItemKinds[vc.ItemKind].EatHereOnly(),
			},
			distTiles:  vendorStructureDistanceTiles(snap, actorSnap, vc.StructureID),
			outOfStock: outOfStock,
			magnitude:  vc.Magnitude,
			itemKind:   vc.ItemKind,
		}
		if existing, ok := byStructure[vc.StructureID]; !ok || betterSatiationRepresentative(cand, existing) {
			byStructure[vc.StructureID] = cand
		}
	}
	if len(byStructure) == 0 {
		return nil
	}
	cands := make([]satiationVendorCandidate, 0, len(byStructure))
	for _, c := range byStructure {
		cands = append(cands, c)
	}
	// Nearest-first; ties by label then structure_id for deterministic output.
	// Remembered-shut vendors were already dropped above (LLM-222), so every
	// candidate here is an actionable, payable destination.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].distTiles != cands[j].distTiles {
			return cands[i].distTiles < cands[j].distTiles
		}
		if cands[i].sv.StructureLabel != cands[j].sv.StructureLabel {
			return cands[i].sv.StructureLabel < cands[j].sv.StructureLabel
		}
		return cands[i].sv.StructureID < cands[j].sv.StructureID
	})
	if len(cands) > maxSatiationVendors {
		cands = cands[:maxSatiationVendors]
	}
	out := make([]SatiationVendor, len(cands))
	for i, c := range cands {
		out[i] = c.sv
	}
	return out
}

// betterSatiationRepresentative reports whether candidate a should replace b as
// a structure's single buy-menu entry: prefer an item NOT remembered out of
// stock (show something buyable), then the stronger satisfier, then a stable
// ItemKind tie-break (Inventory iteration is unordered).
func betterSatiationRepresentative(a, b satiationVendorCandidate) bool {
	if a.outOfStock != b.outOfStock {
		return !a.outOfStock
	}
	if a.magnitude != b.magnitude {
		return a.magnitude > b.magnitude
	}
	return a.itemKind < b.itemKind
}

// vendorStructureDistanceTiles is the actor→vendor-workplace distance in
// internal-grid tiles, for nearest-first ordering. Structures share their id
// with their village_object placement, so the structure tile comes from
// snap.VillageObjects[id].Pos.Tile() (world pixels → tile), same conversion the
// free-source scan uses. An unplaced structure sorts last (+Inf).
func vendorStructureDistanceTiles(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, structureID sim.StructureID) float64 {
	obj := snap.VillageObjects[sim.VillageObjectID(structureID)]
	if obj == nil {
		return math.Inf(1)
	}
	t := obj.Pos.Tile()
	dx := float64(t.X - actorSnap.Pos.X)
	dy := float64(t.Y - actorSnap.Pos.Y)
	return math.Sqrt(dx*dx + dy*dy)
}

// gatherFreeSatiationSources returns a free-source bullet for each free, public
// village object that eases `need` on arrival (a well's thirst refresh, a fruit
// tree's hunger refresh), skipping objects whose finite supply is exhausted. The
// hunger/thirst analogue of recovery_options' gatherFreeRestSpots — same
// distance/direction derivation in INTERNAL-GRID TILE space (actor Pos is a
// padded tile; an object's Pos is world pixels, converted via obj.Pos.Tile()
// before measuring) and same nearest-first ordering. Each source carries its
// object id so the rendered cue is actionable through move_to. ZBBS-HOME-359.
//
// FREE SOURCES ARE COMMON KNOWLEDGE (Jeff, 2026-06-24): everyone always knows the
// village's public wells and free-food placements, so this scans EVERY such
// source for the need with NO discovery gate — a deliberate exception to the
// world-memory no-omniscience posture, because a public good is common knowledge.
// LLM-79 had gated this behind the known-places set + scene proximity; that
// stranded an NPC whose only resolvable source was an off-site PAID vendor and
// who remembered no free source (the Moses James post<->stall cycle), so the
// unconditional scan is restored. Forage (an owner's own bushes) and move_to
// name-resolution stay discovery-gated; only the free public sources are common
// knowledge. Liveness is implicit: consider() re-reads the live object's refresh
// magnitude, so a well gone dry or removed drops out.
//
// The raw scan is then put through an ALTITUDE pass (LLM-139): collapsed to one
// nearest representative per label and capped to maxSatiationFreeSources, so the
// cue stays a short ordered list instead of a per-object data-dump.
const maxSatiationFreeSources = 4

func gatherFreeSatiationSources(snap *sim.Snapshot, subjectID sim.ActorID, actorSnap *sim.ActorSnapshot, need sim.NeedKey) []SatiationFreeSource {
	if snap == nil || actorSnap == nil {
		return nil
	}
	ax := float64(actorSnap.Pos.X)
	ay := float64(actorSnap.Pos.Y)
	var out []SatiationFreeSource
	consider := func(id sim.VillageObjectID, obj *sim.VillageObject) {
		if obj == nil {
			return
		}
		mag := objectRefreshMagnitude(obj, need)
		if mag <= 0 {
			return // doesn't ease this need, or its finite supply is exhausted (liveness)
		}
		// Strict owner-gate (LLM-50 D2): an owned source isn't free food for a
		// non-owner — don't surface it (the eat-on-arrival path skips it too).
		if obj.OwnedByOther(subjectID) {
			return
		}
		objTile := obj.Pos.Tile()
		tx := float64(objTile.X)
		ty := float64(objTile.Y)
		dx := tx - ax
		dy := ty - ay
		distTiles := math.Sqrt(dx*dx + dy*dy)
		label := obj.DisplayName
		if label == "" {
			label = "a nearby source"
		}
		out = append(out, SatiationFreeSource{
			Label:     label,
			ObjectID:  id,
			Magnitude: mag,
			Distance:  qualitativeDistance(distTiles),
			Direction: cardinalDirection(ax, ay, tx, ty),
			sortKey:   distTiles,
			sourceKey: string(id),
		})
	}
	// Common knowledge: scan every free public source for the need. consider()
	// applies the magnitude/liveness and owner gates.
	for id, obj := range snap.VillageObjects {
		consider(id, obj)
	}
	// Nearest first; ties broken by label then object id for deterministic output
	// (VillageObjects is a map). Mirrors gatherFreeRestSpots' ordering.
	sort.Slice(out, func(i, j int) bool {
		if out[i].sortKey != out[j].sortKey {
			return out[i].sortKey < out[j].sortKey
		}
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].sourceKey < out[j].sourceKey
	})
	// ALTITUDE (LLM-139): the common-knowledge scan lists EVERY matching object,
	// so a farm's dozen co-located bushes or a town's several wells flood the cue
	// and bury the load-bearing own-stock line (the hud-6a887a… blast: four copies
	// of one bush kind + five of another). Walking to any source of a kind is
	// equivalent, so keep one representative per label — the nearest, since `out`
	// is already nearest-first — then cap to the nearest few kinds. Same altitude
	// posture as the paid arm's dedup-by-structure + maxSatiationVendors.
	seen := make(map[string]bool, len(out))
	deduped := make([]SatiationFreeSource, 0, len(out))
	for _, fs := range out {
		if seen[fs.Label] {
			continue
		}
		seen[fs.Label] = true
		deduped = append(deduped, fs)
	}
	if len(deduped) > maxSatiationFreeSources {
		deduped = deduped[:maxSatiationFreeSources]
	}
	return deduped
}

// objectRefreshMagnitude returns the positive amount of `need` eased by
// arriving at obj — the negated arrival decrement plus any dwell delta — or 0
// if obj doesn't ease `need` or its finite supply is exhausted. The shared core
// of the satiation free-source scan and recovery_options' tirednessRefreshMagnitude,
// generalised over the need key so a well (thirst) and a shade tree (tiredness)
// read the same way. ZBBS-HOME-359.
func objectRefreshMagnitude(obj *sim.VillageObject, need sim.NeedKey) int {
	for _, r := range obj.Refreshes {
		if r == nil || r.Attribute != need {
			continue
		}
		if r.IsFinite() && r.AvailableQuantity != nil && *r.AvailableQuantity <= 0 {
			continue
		}
		mag := -r.Amount // Amount is the negative arrival decrement
		if r.DwellDelta != nil {
			mag += -*r.DwellDelta
		}
		if mag < 0 {
			mag = 0
		}
		return mag
	}
	return 0
}

// gatherCoPresentPeerOffers scans the subject's CURRENT HUDDLE PEERS for any who
// carry an item that eases `need` on the immediate hit, returning one entry per
// (peer, item). This is the buy-from-the-person-beside-you affordance: the
// peers are co-present, so pay_with_item can resolve them as the seller right
// now with no walk (it resolves any co-present huddle peer holding the goods —
// it is NOT vendor-gated). Distinct from the workplace-vendor scan: this is a
// peer-INVENTORY read over the huddle set, not a structural-vendorship scan, so
// a peer surfaces here whether or not they're also a structural vendor (they're
// different affordances — the same peer can appear in both lists).
//
// The peer name is acquaintance-gated via descriptorLabel — the same name-vs-
// descriptor treatment huddle members get in "Around you" — so an unacquainted
// peer reads as "the <role>" / "a stranger", never their DisplayName.
//
// Excluded: the subject themselves, PCs (they don't sell through the NPC
// commerce path — same exclusion the vendor scan applies), and any actor not in
// the subject's huddle. Output is sorted strongest-offer-first (magnitude desc,
// then PeerLabel, ItemLabel, peerID, ItemKind), mirroring the own-stock / vendor
// finders' "usefulness before identity" ordering with fully deterministic
// tie-breaks (huddle membership + Inventory are maps). Empty when not huddled or
// no co-present peer carries a satisfier.
func gatherCoPresentPeerOffers(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, need sim.NeedKey) []SatiationPeerOffer {
	if snap == nil || actorSnap == nil || actorSnap.CurrentHuddleID == "" {
		return nil
	}
	h := snap.Huddles[actorSnap.CurrentHuddleID]
	if h == nil {
		return nil
	}
	// LLM-242 means-to-pay gate — the consumer-side sibling of the LLM-222 vendor
	// gate in gatherSatiationVendors. pay_with_item needs SOME means of payment:
	// coins to spend or goods to put up in trade (pay_items). A buyer with neither
	// hits the same hard pay_with_item dead-end LLM-222 suppresses on the vendor cue
	// (the 55-hit no-offer rejection class), so drop every peer buy offer too. No
	// per-peer price here (pay_with_item resolves the co-present seller, terms
	// negotiated on their turn), so the gate is a flat coin>0-or-goods, not the
	// vendor cue's costCoins affordability tier. Reuses holdsBarterableGoods so the
	// two buy-food affordances read the SAME goods signal (discussion-109 no-drift).
	if actorSnap.SpendableCoins() <= 0 && !holdsBarterableGoods(snap, actorSnap) {
		return nil
	}
	wholesale := sim.BuyerWholesaleAllowance(snap.VillageObjects, snap.Recipes, actorSnap.WorkStructureID, actorSnap.RestockPolicy)
	var out []SatiationPeerOffer
	for peerID := range h.Members {
		if peerID == actorID {
			continue
		}
		peer := snap.Actors[peerID]
		if peer == nil || peer.Kind == sim.KindPC {
			continue
		}
		// Wholesale gate (LLM-289) — the peer-cue sibling of the LLM-223/252
		// vendor-scan filter in eachVendorOffer. pay_with_item rejects any buyer
		// without a wholesale allowance for the good, from a seller whose WORK
		// ANCHOR is wholesaler-tagged, wherever the seller stands (the gate keys
		// on the anchor, not the venue), so a huddled wholesaler-farmer carrying
		// his own produce must not be advertised as directly buyable. Live
		// hud-843da92a: "Moses is here with you, carrying Carrots — buy it now"
		// → 40 of 57 turns burned on guaranteed wholesale rejections. Same
		// predicate as the dispatch gate so cue and gate can't disagree; the
		// per-item arm is applied inside the inventory walk below (LLM-477).
		peerIsWholesale := sim.SellerAtWholesaler(snap.VillageObjects, peer.WorkStructureID)
		if peerIsWholesale && !wholesale.Any() {
			continue
		}
		// Acquaintance gating mirrors buildSurroundings: the subject knows the
		// peer iff their DisplayName is in the subject's Acquaintances map.
		acquainted := false
		if peer.DisplayName != "" {
			_, acquainted = actorSnap.Acquaintances[peer.DisplayName]
		}
		label := descriptorLabel(peer.DisplayName, peer.Role, acquainted)
		for kind, qty := range peer.Inventory {
			if qty <= 0 {
				continue
			}
			if peerIsWholesale && !wholesale.Allows(kind) {
				continue
			}
			mag := itemNeedMagnitude(snap, kind, need)
			if mag <= 0 {
				continue
			}
			// Degenerate-buy gate (LLM-138): if the subject ALREADY carries this
			// same item, buying the peer's copy is pointless — suppress the line.
			// Live hud-6a887a…: two NPCs at a free blueberry bush, each already
			// holding blueberries, were each told to BUY the other's blueberries —
			// the only peer-food cue — so they narrated hollow "I can offer thee
			// blueberries" beats backed by no transaction. A subject with the item
			// in hand has no reason to buy more of it from someone beside them; its
			// own-stock "consume to eat" line already covers the need.
			if actorSnap.Inventory[kind] > 0 {
				continue
			}
			// A purchase already in flight must not be re-urged: the cross-tick
			// duplicate gate (ZBBS-WORK-391) rejects exactly the pay_with_item
			// this line cues, so rendering it puts the prompt at war with the
			// gate — the model obeys the cue, collects [error: already_offered],
			// and loops (the hud-6c849d… churn, ZBBS-HOME-424). The "## Your
			// pending offers" section already says to wait; other peers/items
			// stay listed as legitimate alternatives.
			if hasPendingOfferTo(snap, actorID, peerID, kind) {
				continue
			}
			out = append(out, SatiationPeerOffer{
				PeerLabel: label,
				ItemLabel: itemDisplayLabel(snap, kind),
				Magnitude: mag,
				peerID:    peerID,
				itemKind:  kind,
			})
		}
	}
	// Strongest offer first — matches the own-stock / vendor finders' "usefulness
	// before identity" ordering, so the most-satisfying buy reads at the top of
	// the prompt. PeerLabel then ItemLabel give a stable human-meaningful order
	// within a magnitude tier; peerID + itemKind are the final deterministic
	// tie-breaks (huddle membership + Inventory are maps, so the comparator must
	// fully order equal-magnitude/label entries).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Magnitude != out[j].Magnitude {
			return out[i].Magnitude > out[j].Magnitude
		}
		if out[i].PeerLabel != out[j].PeerLabel {
			return out[i].PeerLabel < out[j].PeerLabel
		}
		if out[i].ItemLabel != out[j].ItemLabel {
			return out[i].ItemLabel < out[j].ItemLabel
		}
		if out[i].peerID != out[j].peerID {
			return out[i].peerID < out[j].peerID
		}
		return out[i].itemKind < out[j].itemKind
	})
	return out
}

// hasPendingOfferTo reports whether `buyer` already has a still-pending
// pay-ledger offer to `seller` for `kind`, regardless of disposition or terms.
// Deliberately BROADER than the duplicate gate's (buyer, seller, item,
// disposition) key: any live offer for the same goods to the same peer means
// "wait", whatever the disposition — the cue's job is to surface NEW options,
// not re-urge an in-flight one. Expired-but-unswept entries don't suppress
// (mirrors the gate's expiry skip). ZBBS-HOME-424.
func hasPendingOfferTo(snap *sim.Snapshot, buyer, seller sim.ActorID, kind sim.ItemKind) bool {
	now := snap.PublishedAt
	for _, e := range snap.PayLedger {
		if e == nil || e.State != sim.PayLedgerStatePending {
			continue
		}
		if e.BuyerID != buyer || e.SellerID != seller || e.ItemKind != kind {
			continue
		}
		if !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt) {
			continue
		}
		return true
	}
	return false
}

// satiationVerb is the consume verb for the need's dominant modality.
func satiationVerb(need sim.NeedKey) string {
	if need == "thirst" {
		return "drink"
	}
	return "eat"
}

// satiationBridgeLine is the LLM-307 bridging sentence: the carried snack won't
// resolve the need, so look to the meal options listed below. Per-need so the
// prose reads plainly for hunger vs thirst — plain modern English, no invented
// register (the weak-model copy rule). Explicitly scoped to the needs with
// authored prose (mirrors needSufficiencyPhrase): an unsupported need returns ""
// and the caller renders no bridge, rather than emitting hunger prose for it.
func satiationBridgeLine(need sim.NeedKey) string {
	switch need {
	case "hunger":
		return "A nibble won't quiet this hunger, though — for a real meal, see the options below."
	case "thirst":
		return "A sip won't slake this thirst, though — for a real drink, see the options below."
	default:
		return ""
	}
}

// renderSatiation writes the "## What you can eat or drink" section. Content-
// gated: a nil/empty view writes nothing. Numeric (~N) magnitudes, matching
// the recovery-options style.
func renderSatiation(b *strings.Builder, v *SatiationView) {
	if v == nil || len(v.Needs) == 0 {
		return
	}
	b.WriteString("## What you can eat or drink\n")
	for _, n := range v.Needs {
		if len(n.OwnStock) > 0 {
			fmt.Fprintf(b, "You have %s on hand — consume to %s.\n", renderOwnStockLine(n.OwnStock, n.Need, n.Level), n.Verb)
		}
		// LLM-307 bridge: when the own stock is snack-only and a real meal is
		// reachable, say plainly the snack won't resolve the need and point to the
		// options below — otherwise the consume-first line reads as the whole answer
		// and the actor snacks in a loop. Rendered right after the own-stock line and
		// before the walk-to directory it refers to.
		if n.BridgeToMeal {
			if line := satiationBridgeLine(n.Need); line != "" {
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
		// Co-present peers come BEFORE the walk-to-vendor list: a peer standing
		// with you is immediately actionable (pay_with_item resolves them as the
		// seller now), so it shouldn't be buried under shops you'd have to walk
		// to. NO structure_id on these lines — they're already here, and a
		// structure_id would only tempt a needless move_to (ZBBS-HOME-342).
		for _, pr := range n.CoPresentPeers {
			fmt.Fprintf(b, "%s is here with you, carrying %s (%s) — you could offer to buy it from them now with pay_with_item, paying with coins or goods you carry (pay_items). No need to walk anywhere.\n",
				sanitizeInline(pr.PeerLabel), sanitizeInline(pr.ItemLabel), feltAmountWithSufficiency(pr.Magnitude, n.Need, n.Level))
		}
		// Free public sources come BEFORE the walk-to-vendor list: water at a
		// well costs nothing, so it shouldn't read as second to a shop you'd pay
		// at. The object id rides the structure_id field — move_to resolves a
		// bare refresh source by id (or name) to an object visit (ZBBS-HOME-359).
		if len(n.FreeSources) > 0 {
			fmt.Fprintf(b, "Free to %s nearby:\n", n.Verb)
			for _, fs := range n.FreeSources {
				b.WriteString("- ")
				b.WriteString(sanitizeInline(fs.Label))
				if fs.Magnitude > 0 {
					fmt.Fprintf(b, " — %s", itemFeltAmount(fs.Magnitude, n.Need))
				}
				b.WriteString(", free")
				if fs.Distance != "" {
					fmt.Fprintf(b, ", %s", fs.Distance)
					if fs.Direction != "" {
						fmt.Fprintf(b, " %s", fs.Direction)
					}
				}
				if fs.ObjectID != "" {
					fmt.Fprintf(b, " (destination: %s)", fs.ObjectID)
				}
				b.WriteString("\n")
			}
		}
		if len(n.Vendors) > 0 {
			fmt.Fprintf(b, "Nearby to buy (%s):\n", string(n.Need))
			for _, vd := range n.Vendors {
				b.WriteString("- ")
				b.WriteString(sanitizeInline(vd.StructureLabel))
				fmt.Fprintf(b, " — buy %s", sanitizeInline(vd.ItemLabel))
				if vd.Magnitude > 0 {
					fmt.Fprintf(b, " (%s)", feltAmountWithSufficiency(vd.Magnitude, n.Need, n.Level))
				}
				// Means-to-pay phrasing (LLM-222): a vendor the buyer can't cover
				// with coins but can barter for carries Barter — steer it to a goods
				// offer instead of a coin price it can't meet (the coin hint would
				// only invite a pay_with_item the buyer can't fund). Otherwise the
				// normal last-paid coin hint, when one is on record.
				if vd.Barter {
					b.WriteString(", which your coins won't cover — offer goods you carry in trade instead (use pay_with_item with pay_items)")
				} else if vd.CostText != "" {
					fmt.Fprintf(b, ", %s", vd.CostText)
				}
				// Eat-here disposition fact (ZBBS-WORK-405): this class of
				// goods can't be carried away, so the buyer should plan a
				// sit-down, not a carry-out the clamp would quietly rewrite.
				if vd.EatHere {
					b.WriteString(", to eat there")
				}
				// The structure_id is the load-bearing field: move_to(structure_id)
				// is how the buyer actually walks here, and the tool rejects a bare
				// name. Same id-in-perception contract restock + shift_duty use.
				if vd.StructureID != "" {
					fmt.Fprintf(b, " (destination: %s)", vd.StructureID)
				}
				if vd.OutOfStock {
					b.WriteString(outOfStockAnnotation)
				}
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
}
