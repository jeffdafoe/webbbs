package perception

import (
	"fmt"
	"math"
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// equipment_service.go — LLM-648 perception, both sides of the wright's trade.
//
// OWNER side ("## Your equipment"): a keeper whose owned business has accrued
// EquipmentUse past the due threshold gets a tiered scene line — due, then
// long-past-due at the accrual cap — and, when a wright shares the huddle, the
// act-now imperative naming pay_with_item. Silence below the threshold is the
// common case (the felt-needs posture: the quietest tier is still a line, but
// not-due is not a tier).
//
// WRIGHT side ("## Your trade"): the specialist's rounds steer — resupply
// first when he holds no whetstone, else the due business with the MOST
// accrued use, as a walk-to with a bearing or, co-present with its owner, the
// speak imperative to offer the service. The owner's own cue carries the
// pay_with_item; the wright's job is to show up and name his rate — the two
// cues split the deal the way the trader-visit cue splits the factor's.
//
// Slice 1 gates nothing: no penalty rides the counter, so both cues are pure
// steering (the LLM-648 observe-compliance decision).

// EquipmentServiceView is the owner-side cue: the subject's own business is
// due for the wright's service.
type EquipmentServiceView struct {
	// Business is the owned structure's display name; Gear the per-business
	// diegetic phrase ("the millstones", "the bellows and anvil").
	Business string
	Gear     string
	// Overdue is true at the accrual cap — the counter has stopped counting
	// and the scene should stop being polite.
	Overdue bool
	// WrightName is the co-present wright's display name; "" = no wright in
	// the huddle, so the cue stays a standing fact with no imperative.
	WrightName string
	// ServiceItem is the catalog kind the imperative names (resolved by
	// capability, not hardcoded); Price its retail rate, 0 = unpriced (the
	// imperative renders without a figure).
	ServiceItem sim.ItemKind
	Price       int
}

// WrightRoundsView is the wright-side cue: where the trade wants him next.
type WrightRoundsView struct {
	// NoStone: he holds no whetstone — resupply before the rounds.
	NoStone bool
	// The due call (zero-valued when NoStone or nothing is due): the owner,
	// their business, its gear phrase, and how to get there.
	OwnerName string
	Business  string
	Gear      string
	Overdue   bool
	// CoPresent: the owner shares the huddle — offer now (speak) instead of
	// walking.
	CoPresent bool
	// Walk is the bearing phrase ("a fair walk east"); "" when co-present or
	// coincident.
	Walk string
}

// equipmentGearPhrase maps a business's tags to its diegetic gear — a bare
// noun phrase (no article; the templates supply "the"/"The"). Checked in
// specificity order — the Tavern carries lodging AND tavern, the farms carry
// wholesaler AND farm — so the more telling tag wins. The default keeps the
// cue rendering for an untagged-but-wearable business rather than going
// silent on a phrase lookup.
func equipmentGearPhrase(obj *sim.VillageObject) string {
	switch {
	case obj == nil:
		return "tools of the trade"
	case obj.HasTag("smithy"):
		return "bellows and anvil"
	case obj.HasTag("farm"):
		return "blades and barrow"
	case obj.HasTag(sim.VisitorTagTavern):
		return "coppers and taps"
	case obj.HasTag("lodging"):
		return "coppers and hearth-irons"
	case obj.HasTag("shop"), obj.HasTag(sim.TagMarketStall):
		return "scales and fittings"
	case obj.HasTag(sim.TagWholesaler):
		return "millstones"
	default:
		return "tools of the trade"
	}
}

// equipmentServiceItemKind resolves the catalog kind carrying the
// equipment-service capability — the exact name the pay_with_item imperative
// must use. Catalog-driven (no hardcoded name, the delivery arms' posture);
// deterministic on the pathological many-kinds catalog by lowest name.
func equipmentServiceItemKind(snap *sim.Snapshot) (sim.ItemKind, int) {
	var kind sim.ItemKind
	price := 0
	for name, def := range snap.ItemKinds {
		if def == nil || !def.HasCapability(sim.CapabilityEquipmentService) {
			continue
		}
		if kind != "" && name >= kind {
			continue
		}
		kind = name
		if r := snap.Recipes[name]; r != nil {
			price = r.RetailPrice
		}
	}
	return kind, price
}

// equipmentOverdue reports whether a due business has hit the accrual cap.
// Mirrors AccrueEquipmentUse's cap arithmetic; threshold is already known > 0
// at both call sites (due-ness gated first).
func equipmentOverdue(use, threshold int) bool {
	return use >= threshold*2
}

// buildEquipmentService returns the owner-side cue when the subject's owned
// business is due for the wright's service, else nil. Resident keepers only —
// a visitor owns nothing here.
func buildEquipmentService(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, members []HuddleMember) *EquipmentServiceView {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState != nil {
		return nil
	}
	threshold := snap.EquipmentServiceDueThreshold
	stall := sim.DueOwnedBusiness(snap.VillageObjects, actorID, threshold)
	if stall == nil {
		return nil
	}
	kind, price := equipmentServiceItemKind(snap)
	if kind == "" {
		return nil // no service item in the catalog — nothing purchasable to cue
	}
	label, ok := resolveStructureLabel(snap, sim.StructureID(stall.ID))
	if !ok || label == "" {
		label = stall.DisplayName
	}
	v := &EquipmentServiceView{
		Business:    label,
		Gear:        equipmentGearPhrase(stall),
		Overdue:     equipmentOverdue(stall.EquipmentUse, threshold),
		ServiceItem: kind,
		Price:       price,
	}
	for _, m := range members {
		ms := snap.Actors[m.ID]
		if ms == nil || m.ID == actorID {
			continue
		}
		if sim.ActorIsWright(snap.VillageObjects, ms.WorkStructureID) {
			v.WrightName = m.DisplayName
			break
		}
	}
	return v
}

// renderEquipmentService writes the owner-side "## Your equipment" cue.
// Content-gated.
func renderEquipmentService(b *strings.Builder, v *EquipmentServiceView) {
	if v == nil {
		return
	}
	business := sanitizeInline(v.Business)
	gear := sanitizeInline(v.Gear)
	b.WriteString("## Your equipment\n")
	if v.Overdue {
		fmt.Fprintf(b, "The %s at %s are long past due — dull, dragging, working against you every batch.", gear, business)
	} else {
		fmt.Fprintf(b, "The %s at %s have seen a season's hard use and want the wright's attention.", gear, business)
	}
	if v.WrightName != "" {
		name := sanitizeInline(v.WrightName)
		fmt.Fprintf(b, " %s is here now with his stones — have him see to it: pay_with_item (seller \"%s\", item \"%s\", qty 1, consume_now false, coins in amount", name, name, v.ServiceItem)
		if v.Price > 0 {
			fmt.Fprintf(b, " — his rate is about %d", v.Price)
		}
		b.WriteString(").\n\n")
		return
	}
	b.WriteString(" It will keep until the wright calls.\n\n")
}

// buildWrightRounds returns the wright-side cue for a subject who works the
// trade, else nil. NoStone preempts the rounds — no service without a stone.
// The due call is the business with the MOST accrued use (tie: lowest object
// id), never the wright's own shop.
func buildWrightRounds(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, members []HuddleMember) *WrightRoundsView {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState != nil {
		return nil
	}
	if !sim.ActorIsWright(snap.VillageObjects, actorSnap.WorkStructureID) {
		return nil
	}
	if actorSnap.Inventory[sim.WhetstoneKind] < sim.WhetstonesPerService {
		return &WrightRoundsView{NoStone: true}
	}
	threshold := snap.EquipmentServiceDueThreshold
	var best *sim.VillageObject
	for _, obj := range snap.VillageObjects {
		if obj == nil || obj.OwnerActorID == actorID {
			continue
		}
		// The same wearable-business predicate the accrual seam, the owner cue,
		// and the delivery gate run (code_review): a non-business owned object
		// with a stray persisted EquipmentUse must never become a rounds
		// destination — ServiceEquipment could not reset it.
		if !sim.IsWearableStall(obj) {
			continue
		}
		if !sim.EquipmentServiceDue(obj, threshold) {
			continue
		}
		if best == nil || obj.EquipmentUse > best.EquipmentUse ||
			(obj.EquipmentUse == best.EquipmentUse && obj.ID < best.ID) {
			best = obj
		}
	}
	if best == nil {
		return nil
	}
	owner := snap.Actors[best.OwnerActorID]
	if owner == nil {
		return nil
	}
	label, ok := resolveStructureLabel(snap, sim.StructureID(best.ID))
	if !ok || label == "" {
		label = best.DisplayName
	}
	v := &WrightRoundsView{
		OwnerName: owner.DisplayName,
		Business:  label,
		Gear:      equipmentGearPhrase(best),
		Overdue:   equipmentOverdue(best.EquipmentUse, threshold),
	}
	for _, m := range members {
		if m.ID == best.OwnerActorID {
			v.CoPresent = true
			return v
		}
	}
	toTile := best.Pos.Tile()
	dist := math.Max(
		math.Abs(float64(toTile.X-actorSnap.Pos.X)),
		math.Abs(float64(toTile.Y-actorSnap.Pos.Y)),
	)
	dir := cardinalDirection(float64(actorSnap.Pos.X), float64(actorSnap.Pos.Y), float64(toTile.X), float64(toTile.Y))
	if dir != "" {
		v.Walk = qualitativeDistance(dist) + " " + dir
	}
	return v
}

// renderWrightRounds writes the wright-side "## Your trade" cue.
// Content-gated.
func renderWrightRounds(b *strings.Builder, v *WrightRoundsView) {
	if v == nil {
		return
	}
	// "## The wright's rounds", not "## Your trade" — that header belongs to
	// the production-choice cue and the golden invariants police it.
	b.WriteString("## The wright's rounds\n")
	if v.NoStone {
		b.WriteString("You're out of whetstones — no service without a stone. Buy one before you make your rounds.\n\n")
		return
	}
	owner := sanitizeInline(v.OwnerName)
	business := sanitizeInline(v.Business)
	gear := sanitizeInline(v.Gear)
	urgency := "due for your attention"
	if v.Overdue {
		urgency = "long past due — they'll be feeling it in every batch"
	}
	if v.CoPresent {
		fmt.Fprintf(b, "You're with %s now, and the %s at %s are %s. Offer your service — tell them your rate and that your stone is ready (speak).\n\n", owner, gear, business, urgency)
		return
	}
	if v.Walk != "" {
		fmt.Fprintf(b, "The %s at %s are %s — %s. Head there (move_to \"%s\") and offer %s your service.\n\n", gear, business, urgency, v.Walk, business, owner)
		return
	}
	fmt.Fprintf(b, "The %s at %s are %s. Head there (move_to \"%s\") and offer %s your service.\n\n", gear, business, urgency, business, owner)
}
