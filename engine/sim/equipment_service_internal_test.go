package sim

import (
	"strings"
	"testing"
	"time"
)

// equipmentCatalog is the minimal catalog for the LLM-648 tests: the service,
// its whetstone consumable, and a plain good for pass-through cases.
func equipmentCatalog() map[ItemKind]*ItemKindDef {
	return map[ItemKind]*ItemKindDef{
		WhetstoneKind:       {Name: WhetstoneKind},
		"equipment_service": {Name: "equipment_service", Capabilities: []string{"service", CapabilityEquipmentService}},
		"flour":             {Name: "flour"},
	}
}

// equipmentWorld builds the minimal World for the transferOrderGoods
// equipment-service arm: Lewis working a TagWright workshop with `stones`
// whetstones, and Joseph owning a TagBusiness mill carrying `use` accrued
// EquipmentUse against a due threshold of 100.
func equipmentWorld(stones, use int) (*World, *Actor, *Actor) {
	seller := &Actor{
		ID: "lewis", DisplayName: "Lewis Walker", WorkStructureID: "workshop",
		Inventory: map[ItemKind]int{},
	}
	if stones > 0 {
		seller.Inventory[WhetstoneKind] = stones
	}
	buyer := &Actor{
		ID: "joseph", DisplayName: "Joseph Scott",
		Inventory: map[ItemKind]int{},
	}
	w := &World{
		ItemKinds: equipmentCatalog(),
		Actors:    map[ActorID]*Actor{seller.ID: seller, buyer.ID: buyer},
		VillageObjects: map[VillageObjectID]*VillageObject{
			"workshop": {ID: "workshop", Tags: []string{TagWright}},
			"mill":     {ID: "mill", OwnerActorID: "joseph", Tags: []string{TagBusiness}, EquipmentUse: use},
		},
	}
	w.Settings.EquipmentServiceDueThreshold = 100
	return w, seller, buyer
}

func equipmentOrder(buyer ActorID) *Order {
	return &Order{ID: 1, Item: "equipment_service", Qty: 1, BuyerID: buyer, ConsumerIDs: []ActorID{buyer}}
}

// TestAccrueEquipmentUse — accrual lands on the owner's wearable business,
// caps at 2x the threshold, and no-ops on the off switch (threshold 0), a
// business-less owner, and non-positive units.
func TestAccrueEquipmentUse(t *testing.T) {
	t.Run("accrues onto the owned business", func(t *testing.T) {
		w, _, _ := equipmentWorld(0, 40)
		AccrueEquipmentUse(w, "joseph", 25)
		if got := w.VillageObjects["mill"].EquipmentUse; got != 65 {
			t.Errorf("EquipmentUse = %d, want 65", got)
		}
	})
	t.Run("caps at twice the threshold", func(t *testing.T) {
		w, _, _ := equipmentWorld(0, 190)
		AccrueEquipmentUse(w, "joseph", 500)
		if got := w.VillageObjects["mill"].EquipmentUse; got != 200 {
			t.Errorf("EquipmentUse = %d, want the 200 cap", got)
		}
	})
	t.Run("threshold 0 is the off switch", func(t *testing.T) {
		w, _, _ := equipmentWorld(0, 40)
		w.Settings.EquipmentServiceDueThreshold = 0
		AccrueEquipmentUse(w, "joseph", 25)
		if got := w.VillageObjects["mill"].EquipmentUse; got != 40 {
			t.Errorf("EquipmentUse = %d, want the untouched 40", got)
		}
	})
	t.Run("an owner with no business accrues nothing", func(t *testing.T) {
		w, _, _ := equipmentWorld(0, 40)
		AccrueEquipmentUse(w, "lewis", 25) // the workshop has no owner set
		if got := w.VillageObjects["mill"].EquipmentUse; got != 40 {
			t.Errorf("someone else's business accrued: %d", got)
		}
	})
	t.Run("non-positive units no-op", func(t *testing.T) {
		w, _, _ := equipmentWorld(0, 40)
		AccrueEquipmentUse(w, "joseph", 0)
		AccrueEquipmentUse(w, "joseph", -3)
		if got := w.VillageObjects["mill"].EquipmentUse; got != 40 {
			t.Errorf("EquipmentUse = %d, want the untouched 40", got)
		}
	})
}

// TestGatherAccruesEquipmentUse — harvesting an OWNED source accrues onto the
// harvester's business; a commons source accrues nothing (the well is nobody's
// equipment).
func TestGatherAccruesEquipmentUse(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	build := func(owner ActorID) (*World, *Actor, *VillageObject, *ObjectRefresh) {
		w, _, buyer := equipmentWorld(0, 40)
		avail, max := 5, 5
		row := &ObjectRefresh{AvailableQuantity: &avail, MaxQuantity: &max, GatherItem: "flour"}
		src := &VillageObject{ID: "field", DisplayName: "Field", OwnerActorID: owner, Refreshes: []*ObjectRefresh{row}}
		w.VillageObjects["field"] = src
		return w, buyer, src, row
	}
	t.Run("owned source accrues", func(t *testing.T) {
		w, actor, src, row := build("joseph")
		if _, err := applyGatherMint(w, actor, src.ID, src, row, "flour", 3, at); err != nil {
			t.Fatalf("applyGatherMint: %v", err)
		}
		if got := w.VillageObjects["mill"].EquipmentUse; got != 43 {
			t.Errorf("EquipmentUse = %d, want 43 (40 + 3 harvested)", got)
		}
	})
	t.Run("a commons source accrues nothing", func(t *testing.T) {
		w, actor, src, row := build("")
		if _, err := applyGatherMint(w, actor, src.ID, src, row, "flour", 3, at); err != nil {
			t.Fatalf("applyGatherMint: %v", err)
		}
		if got := w.VillageObjects["mill"].EquipmentUse; got != 40 {
			t.Errorf("EquipmentUse = %d, want the untouched 40", got)
		}
	})
}

// TestTransferOrderGoods_EquipmentService — the happy path resets the buyer's
// due business and draws the wright's whetstone; the gate arms reject a
// non-wright workplace, a stoneless wright, and a not-due buyer without
// mutating state. The mending arm's contract, stone for thread.
func TestTransferOrderGoods_EquipmentService(t *testing.T) {
	at := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	t.Run("service resets use and draws the stone", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		if err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at); err != nil {
			t.Fatalf("transferOrderGoods: %v", err)
		}
		if got := w.VillageObjects["mill"].EquipmentUse; got != 0 {
			t.Errorf("EquipmentUse = %d after service, want 0", got)
		}
		if got := seller.Inventory[WhetstoneKind]; got != 2-WhetstonesPerService {
			t.Errorf("seller whetstones = %d, want %d", got, 2-WhetstonesPerService)
		}
		if buyer.Inventory["equipment_service"] != 0 {
			t.Error("a service must transfer no goods to the buyer")
		}
	})

	t.Run("the last stone is drawn and deleted on zero", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(WhetstonesPerService, 150)
		if err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at); err != nil {
			t.Fatalf("transferOrderGoods: %v", err)
		}
		if _, held := seller.Inventory[WhetstoneKind]; held {
			t.Errorf("zeroed whetstone must be deleted from inventory (delete-on-zero): %v", seller.Inventory)
		}
	})

	t.Run("a non-wright workplace rejects", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		w.VillageObjects["workshop"].Tags = nil
		err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at)
		if err == nil || !strings.Contains(err.Error(), "wright") {
			t.Fatalf("want a wright's-trade rejection, got %v", err)
		}
		if got := w.VillageObjects["mill"].EquipmentUse; got != 150 {
			t.Error("a rejected service must not touch the counter")
		}
	})

	t.Run("a stoneless wright rejects", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(0, 150)
		err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at)
		if err == nil || !strings.Contains(err.Error(), "whetstone") {
			t.Fatalf("want a no-whetstone rejection, got %v", err)
		}
	})

	t.Run("a not-due buyer rejects without drawing the stone", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 50)
		err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at)
		if err == nil || !strings.Contains(err.Error(), "due for service") {
			t.Fatalf("want a not-due rejection, got %v", err)
		}
		if got := seller.Inventory[WhetstoneKind]; got != 2 {
			t.Errorf("a rejected service must not draw the stone: %d", got)
		}
	})

	t.Run("a non-self consumer rejects", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		o := equipmentOrder(buyer.ID)
		o.ConsumerIDs = []ActorID{seller.ID}
		if err := transferOrderGoods(w, o, seller, []*Actor{seller}, at); err == nil {
			t.Fatal("want a sole-self-consumer rejection, got nil")
		}
	})

	t.Run("a multi-unit order rejects at the delivery boundary", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		o := equipmentOrder(buyer.ID)
		o.Qty = 2
		err := transferOrderGoods(w, o, seller, []*Actor{buyer}, at)
		if err == nil || !strings.Contains(err.Error(), "qty must be 1") {
			t.Fatalf("want the defensive qty-1 rejection, got %v", err)
		}
		if got := seller.Inventory[WhetstoneKind]; got != 2 {
			t.Errorf("a rejected multi-unit order must not draw the stone: %d", got)
		}
	})

	t.Run("equipment_service without service is a misconfigured catalog", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		w.ItemKinds["equipment_service"].Capabilities = []string{CapabilityEquipmentService}
		err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{buyer}, at)
		if err == nil || !strings.Contains(err.Error(), "misconfigured") {
			t.Fatalf("want the misconfigured-catalog rejection, got %v", err)
		}
	})

	t.Run("a resolved consumer other than the buyer rejects", func(t *testing.T) {
		w, seller, buyer := equipmentWorld(2, 150)
		other := &Actor{ID: "other", DisplayName: "Someone Else", Inventory: map[ItemKind]int{}}
		w.Actors[other.ID] = other
		err := transferOrderGoods(w, equipmentOrder(buyer.ID), seller, []*Actor{other}, at)
		if err == nil || !strings.Contains(err.Error(), "other than buyer") {
			t.Fatalf("want the resolved-consumer identity rejection, got %v", err)
		}
		if got := w.VillageObjects["mill"].EquipmentUse; got != 150 {
			t.Error("the mismatched delivery must not service the business")
		}
	})
}

// TestPreflightEquipmentServiceEntry — the commit-time invariant boundary:
// coins move before the delivery branch in commitPayTransfer, so every way a
// service can fail must reject in the preflight, and an entry with no
// equipment-service involvement must pass through untouched.
func TestPreflightEquipmentServiceEntry(t *testing.T) {
	w, seller, buyer := equipmentWorld(2, 150)

	t.Run("no service involvement passes", func(t *testing.T) {
		if err := preflightEquipmentServiceEntry(w, buyer, seller, &PayLedgerEntry{ItemKind: "flour"}); err != nil {
			t.Fatalf("a plain goods entry must pass the preflight: %v", err)
		}
	})
	t.Run("a deliverable service passes", func(t *testing.T) {
		entry := &PayLedgerEntry{ID: 7, ItemKind: "equipment_service", Qty: 1, BuyerID: buyer.ID, SellerID: seller.ID}
		if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err != nil {
			t.Fatalf("a deliverable service must pass the preflight: %v", err)
		}
	})
	t.Run("service inside a bundle rejects", func(t *testing.T) {
		entry := &PayLedgerEntry{ID: 7, Lines: []QuoteLine{{ItemKind: "flour"}, {ItemKind: "equipment_service"}}}
		if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err == nil || !strings.Contains(err.Error(), "bundle") {
			t.Fatalf("want the bundle rejection, got %v", err)
		}
	})
	t.Run("a gifted service rejects", func(t *testing.T) {
		entry := &PayLedgerEntry{ID: 7, ItemKind: "equipment_service", IsGift: true, BuyerID: buyer.ID}
		if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err == nil || !strings.Contains(err.Error(), "gift") {
			t.Fatalf("want the gift rejection, got %v", err)
		}
	})
	t.Run("a multi-unit service rejects before coins move", func(t *testing.T) {
		// Delivery resets one business and draws one stone, so a qty > 1 entry
		// would charge for undeliverable units (code_review). The preflight is
		// the boundary BEFORE commitPayTransfer moves coins.
		entry := &PayLedgerEntry{ID: 7, ItemKind: "equipment_service", Qty: 2, BuyerID: buyer.ID, SellerID: seller.ID}
		if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err == nil || !strings.Contains(err.Error(), "qty must be 1") {
			t.Fatalf("want the qty-1 rejection, got %v", err)
		}
	})
	t.Run("a non-buyer consumer rejects", func(t *testing.T) {
		entry := &PayLedgerEntry{ID: 7, ItemKind: "equipment_service", Qty: 1, BuyerID: buyer.ID, ConsumerIDs: []ActorID{seller.ID}}
		if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err == nil || !strings.Contains(err.Error(), "non-buyer") {
			t.Fatalf("want the non-buyer consumer rejection, got %v", err)
		}
	})
	t.Run("an undeliverable service rejects without mutating", func(t *testing.T) {
		wDry, sellerDry, buyerDry := equipmentWorld(0, 150)
		entry := &PayLedgerEntry{ID: 7, ItemKind: "equipment_service", Qty: 1, BuyerID: buyerDry.ID}
		if err := preflightEquipmentServiceEntry(wDry, buyerDry, sellerDry, entry); err == nil || !strings.Contains(err.Error(), "whetstone") {
			t.Fatalf("want the no-whetstone rejection, got %v", err)
		}
		if got := wDry.VillageObjects["mill"].EquipmentUse; got != 150 {
			t.Error("the preflight must not mutate the business")
		}
	})
}
