package sim

import "fmt"

// equipment_service.go — LLM-648. Deep equipment maintenance: every owned
// business accrues EquipmentUse in proportion to its OUTPUT (produced batches
// and harvests from its owner's own sources), and only the village wright's
// bought "equipment_service" restores it — the mending idiom (a service whose
// delivery routes to an effect) applied to the tools of a trade.
//
// Why a second counter beside Wear: the two measure different things and are
// answered by different people. Wear is coin turned over at the counter and the
// OWNER mends it with nails (stall_wear.go — the smith's protected demand,
// LLM-442/511). EquipmentUse is the millstones dulling, the bellows cracking,
// the pruning knives going blunt — work only a travelling specialist does, for
// coin. (Actor.ToolWear, LLM-330, is a third and unrelated layer: durability
// of a CARRIED tool's in-use unit, answered by rebuying the tool. Business
// equipment is not an inventory item, which is why it needs a counter on the
// object instead.) That split is also the economic point: high-PRODUCTION actors are the
// village's coin pools, so the wright's bill lands on the wealthiest without
// the engine ever reading a purse — targeting is emergent and diegetic.
//
// The model, mirroring mending piece for piece:
//   - The wright is resolved from WHERE THE SELLER WORKS: a structure carrying
//     TagWright (operator-assignable via /object/add-tag — no actor named in
//     code, the TagMending posture).
//   - Each service consumes WhetstonesPerService of the seller's whetstones —
//     an Ezekiel-forged good with an iron input, so the service line deepens
//     the iron sink instead of reusing nails.
//   - One service restores the BUYER's owned business in full: EquipmentUse
//     resets to 0. Fixed price (catalog w6/r10); the recirculation rate scales
//     through FREQUENCY — a business producing twice as much falls due twice
//     as often.
//   - Slice 1 carries no overdue penalty: the owner-side cue escalates by tier
//     and the sale is the only mechanism. If owners ignore the cue live, the
//     produce-pace penalty (the LLM-446 75% shape, never touching forage — the
//     LLM-634 lesson) is the follow-up.

// TagWright marks a structure whose keeper offers equipment service — the
// seller-eligibility anchor for the "equipment_service" item, resolved from
// where the seller WORKS (the TagMending posture). Operator-assignable live
// via /object/add-tag.
const TagWright = "wright"

// CapabilityEquipmentService is the item-capability token that routes a bought
// service to equipment restoration at delivery (transferOrderGoods) — the
// mending/lodging idiom. The item must also carry "service" (no inventory
// backing).
const CapabilityEquipmentService = "equipment_service"

// WhetstoneKind is the consumable a service draws from the seller — a forged
// good (iron x1, an Ezekiel recipe) so the wright's trade keeps the forge and
// the iron import line load-bearing.
const WhetstoneKind = ItemKind("whetstone")

// WhetstonesPerService is how many whetstones one service consumes. One stone
// services a whole visit: the service item's retail is the economic lever, not
// the material burn — the MendThreadPerMend posture.
const WhetstonesPerService = 1

// DefaultEquipmentServiceDueThreshold is the EquipmentUse level at which a
// business is DUE for service. Calibrated against observed output (2026-08-31):
// the top producers move ~130-190 units/week, the Inn/Tavern ~50-90 — so the
// wright calls on the rich roughly weekly and the rest fortnightly. 0 disables
// the mechanism entirely (no accrual, nothing due — the per-feature off-switch
// posture).
const DefaultEquipmentServiceDueThreshold = 100

// equipmentUseCapMultiple bounds EquipmentUse at this multiple of the due
// threshold. Accrual past "long overdue" tells the cue nothing new, and an
// unbounded counter on a never-serviced business is just a widening number a
// future penalty tier would have to clamp anyway.
const equipmentUseCapMultiple = 2

// IsWrightStructure reports whether obj carries the wright tag. Nil-safe,
// no-owner-required — the IsMendingStructure posture.
func IsWrightStructure(obj *VillageObject) bool {
	return obj != nil && obj.HasTag(TagWright)
}

// ActorIsWright reports whether the actor stationed at workStructureID offers
// equipment service — their workplace carries TagWright. Takes the object map
// so it serves both the live World and a perception Snapshot. An actor with no
// workplace is never a wright.
func ActorIsWright(objects map[VillageObjectID]*VillageObject, workStructureID StructureID) bool {
	if workStructureID == "" {
		return false
	}
	return IsWrightStructure(objects[VillageObjectID(workStructureID)])
}

// EquipmentServiceDue reports whether an owned business has accrued enough use
// to be worth the wright's visit. threshold <= 0 = the mechanism is off,
// nothing is ever due.
func EquipmentServiceDue(obj *VillageObject, threshold int) bool {
	return obj != nil && threshold > 0 && obj.EquipmentUse >= threshold
}

// AccrueEquipmentUse adds units of output to the owner's wearable business,
// capped at equipmentUseCapMultiple x threshold. The two callers are the
// production landing (landProductionCycle — every minted unit including
// booster bonuses) and the owned-source harvest (applyGatherMint — units
// gathered from a source whose OwnerActorID is the gatherer). A commons
// source (the village well) is nobody's equipment and accrues nothing.
// No-op when the mechanism is off (threshold <= 0), when the owner has no
// wearable business, or for non-positive units.
func AccrueEquipmentUse(w *World, ownerID ActorID, units int) {
	if w == nil || units <= 0 {
		return
	}
	threshold := w.Settings.EquipmentServiceDueThreshold
	if threshold <= 0 {
		return
	}
	stall := OwnedWearableStall(w.VillageObjects, ownerID)
	if stall == nil {
		return
	}
	next := stall.EquipmentUse + units
	if cap := threshold * equipmentUseCapMultiple; next > cap {
		next = cap
	}
	stall.EquipmentUse = next
}

// DueOwnedBusiness returns the buyer's wearable business when it is due for
// service, else nil — the buyer-side half of the delivery predicate, shared
// with the owner-side perception cue so the gate and the cue can never
// disagree on due-ness.
func DueOwnedBusiness(objects map[VillageObjectID]*VillageObject, ownerID ActorID, threshold int) *VillageObject {
	stall := OwnedWearableStall(objects, ownerID)
	if !EquipmentServiceDue(stall, threshold) {
		return nil
	}
	return stall
}

// ValidateEquipmentServiceDelivery is the ONE non-mutating statement of every
// precondition a service must meet, shared by the intake gates, the accept
// gate, the commit-time preflight, and the delivery arm itself — the
// ValidateMendingDelivery contract (coins move before the delivery branch in
// commitPayTransfer and an error return does not roll them back, so every way
// a service can fail must be checkable BEFORE payment). nil means the service
// can be delivered right now.
func ValidateEquipmentServiceDelivery(w *World, seller, buyer *Actor, kind ItemKind) error {
	if !itemHasCapability(w, kind, "service") {
		return fmt.Errorf("item %q has the equipment_service capability without service — misconfigured catalog", kind)
	}
	if seller == nil {
		return fmt.Errorf("equipment service: no seller")
	}
	if !ActorIsWright(w.VillageObjects, StructureID(seller.WorkStructureID)) {
		return fmt.Errorf("%s does not work a wright's trade", seller.DisplayName)
	}
	if seller.Inventory[WhetstoneKind] < WhetstonesPerService {
		return fmt.Errorf("%s has no whetstone to work with", seller.DisplayName)
	}
	if buyer == nil {
		return fmt.Errorf("equipment service: no buyer")
	}
	if DueOwnedBusiness(w.VillageObjects, buyer.ID, w.Settings.EquipmentServiceDueThreshold) == nil {
		return fmt.Errorf("%s has no business equipment due for service", buyer.DisplayName)
	}
	return nil
}

// ServiceEquipment restores the buyer's due business: EquipmentUse resets to 0.
// Returns the serviced object, or nil when nothing was due (the caller treats
// that as a defensive error — ValidateEquipmentServiceDelivery already
// guaranteed due-ness, and a service that touched nothing must never charge a
// whetstone, the MendGarments posture).
func ServiceEquipment(w *World, buyerID ActorID) *VillageObject {
	if w == nil {
		return nil
	}
	stall := DueOwnedBusiness(w.VillageObjects, buyerID, w.Settings.EquipmentServiceDueThreshold)
	if stall == nil {
		return nil
	}
	stall.EquipmentUse = 0
	return stall
}
