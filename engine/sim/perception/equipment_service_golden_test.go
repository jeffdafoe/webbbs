package perception

import (
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// equipment_service_golden_test.go — golden scenarios + cross-scenario
// invariants for the LLM-648 wright's trade: the owner-side "## Your
// equipment" due cue (standing fact alone; act-now imperative with the wright
// co-present) and the wright-side "## Your trade" rounds steer (resupply /
// walk-to / co-present offer). Registered into perceptionScenarios so
// TestPerceptionGoldens covers them and the terminal-verb invariant sweeps
// their imperatives.

func init() {
	perceptionScenarios = append(perceptionScenarios,
		perceptionScenario{
			name: "owner_equipment_due",
			summary: "LLM-648: Joseph's mill has accrued past the due threshold with no wright around. " +
				"'## Your equipment' renders the standing fact (millstones want the wright's attention) and no " +
				"imperative — it keeps until the wright calls.",
			build: ownerEquipmentDueScenario,
		},
		perceptionScenario{
			name: "owner_equipment_wright_copresent",
			summary: "LLM-648: same due mill, but Lewis the wright shares the huddle. The cue escalates to the " +
				"act-now imperative naming pay_with_item (seller Lewis, item equipment_service) with his ~10-coin rate.",
			build: ownerEquipmentWrightCopresentScenario,
		},
		perceptionScenario{
			name: "wright_rounds_walk",
			summary: "LLM-648: Lewis, stone in hand, with the mill long past due across the village. '## Your " +
				"trade' steers the walk (move_to the mill's label, with a bearing) to offer Joseph his service.",
			build: wrightRoundsWalkScenario,
		},
		perceptionScenario{
			name: "wright_rounds_copresent",
			summary: "LLM-648: Lewis co-present with Joseph, mill due. '## Your trade' switches to the offer " +
				"imperative (speak — name the rate); the owner's own cue carries the pay half of the deal.",
			build: wrightRoundsCopresentScenario,
		},
		perceptionScenario{
			name: "wright_no_stone",
			summary: "LLM-648: Lewis holds no whetstone, so '## Your trade' preempts the rounds with the " +
				"resupply steer — no service without a stone — even though the mill is due.",
			build: wrightNoStoneScenario,
		},
	)
}

// equipmentScenarioBase builds the shared snapshot: Joseph owning a due mill,
// Lewis working the wright-tagged workshop, catalog + recipe for the service.
// millUse and lewisStones vary per scenario; huddle "" leaves everyone apart.
func equipmentScenarioBase(millUse, lewisStones int, huddle sim.HuddleID) (*sim.Snapshot, map[sim.ActorID]*sim.ActorSnapshot) {
	now := 600 // 10:00 daytime
	joseph := &sim.ActorSnapshot{
		Kind:               sim.KindNPCShared,
		DisplayName:        "Joseph Scott",
		Role:               "miller",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 40, Y: 40},
		WorkStructureID:    "mill",
		InsideStructureID:  "mill",
		CurrentHuddleID:    huddle,
		Coins:              600,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "miller"},
	}
	lewis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCShared,
		DisplayName:     "Lewis Walker",
		Role:            "wright",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 41, Y: 40},
		WorkStructureID: "workshop",
		CurrentHuddleID: huddle,
		Coins:           24,
		Needs:           map[sim.NeedKey]int{},
		Inventory:       map[sim.ItemKind]int{},
	}
	if lewisStones > 0 {
		lewis.Inventory[sim.WhetstoneKind] = lewisStones
	}
	actors := map[sim.ActorID]*sim.ActorSnapshot{"joseph": joseph, "lewis": lewis}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:             &now,
		NeedThresholds:               sim.NeedThresholds{},
		EquipmentServiceDueThreshold: 100,
		Actors:                       actors,
		Structures: map[sim.StructureID]*sim.Structure{
			"mill":     plainStructure("mill", "The Mill"),
			"workshop": plainStructure("workshop", "Lewis's Workshop"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"mill": {ID: "mill", Pos: sim.WorldPos{X: 320, Y: 320}, OwnerActorID: "joseph",
				Tags: []string{sim.TagBusiness, sim.TagWholesaler}, EquipmentUse: millUse},
			"workshop": {ID: "workshop", Pos: sim.WorldPos{X: 328, Y: 320}, OwnerActorID: "lewis",
				Tags: []string{sim.TagBusiness, sim.TagWright}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"equipment_service": {Name: "equipment_service", DisplayLabel: "Equipment service",
				Capabilities: []string{"service", sim.CapabilityEquipmentService}},
			sim.WhetstoneKind: {Name: sim.WhetstoneKind, DisplayLabel: "Whetstone"},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"equipment_service": {WholesalePrice: 6, RetailPrice: 10},
		},
	}
	if huddle != "" {
		snap.Huddles = map[sim.HuddleID]*sim.Huddle{
			huddle: {Members: map[sim.ActorID]struct{}{"joseph": {}, "lewis": {}}},
		}
	}
	return snap, actors
}

func ownerEquipmentDueScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actors := equipmentScenarioBase(120, 2, "")
	// No wright around: Lewis stays across the village, out of any huddle.
	actors["lewis"].Pos = sim.TilePos{X: 90, Y: 90}
	return snap, "joseph", nil
}

func ownerEquipmentWrightCopresentScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, _ := equipmentScenarioBase(120, 2, "h1")
	return snap, "joseph", nil
}

func wrightRoundsWalkScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actors := equipmentScenarioBase(200, 2, "")
	// Lewis far from the mill so the steer carries a bearing.
	actors["lewis"].Pos = sim.TilePos{X: 90, Y: 90}
	return snap, "lewis", nil
}

func wrightRoundsCopresentScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, _ := equipmentScenarioBase(150, 2, "h1")
	return snap, "lewis", nil
}

func wrightNoStoneScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actors := equipmentScenarioBase(150, 0, "")
	actors["lewis"].Pos = sim.TilePos{X: 90, Y: 90}
	return snap, "lewis", nil
}

// TestWrightRoundsSkipsNonBusinessObjects — the rounds pick runs the same
// wearable-business predicate as accrual and delivery (code_review): a
// non-business owned object with a stray persisted EquipmentUse must never
// outrank a due wearable business, however high its counter reads.
func TestWrightRoundsSkipsNonBusinessObjects(t *testing.T) {
	snap, actors := equipmentScenarioBase(150, 2, "")
	actors["lewis"].Pos = sim.TilePos{X: 90, Y: 90}
	// A stray owned decoration carrying a higher counter than the due mill —
	// no TagBusiness, so ServiceEquipment could never reset it.
	snap.VillageObjects["shed"] = &sim.VillageObject{
		ID: "shed", DisplayName: "Old Shed", Pos: sim.WorldPos{X: 100, Y: 100},
		OwnerActorID: "joseph", EquipmentUse: 999,
	}
	v := buildWrightRounds(snap, "lewis", snap.Actors["lewis"], nil)
	if v == nil {
		t.Fatal("the due mill must still produce a rounds steer")
	}
	if v.Business != "The Mill" {
		t.Errorf("rounds picked %q, want the wearable business The Mill", v.Business)
	}
}

// TestWrightCuesRequireKeeperOwnership — workplace assignment alone is not the
// trade (code_review): a hired hand assigned to the wright's workshop neither
// gets the rounds steer nor counts as the co-present wright who escalates the
// owner's cue to the pay imperative.
func TestWrightCuesRequireKeeperOwnership(t *testing.T) {
	snap, _ := equipmentScenarioBase(150, 2, "h1")
	// Reassign the workshop to someone else: Lewis still WORKS there but no
	// longer owns it, so he is no longer its keeper.
	snap.VillageObjects["workshop"].OwnerActorID = "someone-else"
	if v := buildWrightRounds(snap, "lewis", snap.Actors["lewis"], nil); v != nil {
		t.Errorf("a non-keeper at the workshop got the rounds steer: %+v", v)
	}
	members := []HuddleMember{{ID: "lewis", DisplayName: "Lewis Walker"}}
	if v := buildEquipmentService(snap, "joseph", snap.Actors["joseph"], members); v == nil || v.WrightName != "" {
		t.Errorf("owner cue: want the standing fact with no wright imperative, got %+v", v)
	}
}

// TestGoldensEquipmentCueOnlyForDueOwner — "## Your equipment" may render only
// for a resident subject whose OWN business has accrued to the due threshold.
// The gate and the cue share DueOwnedBusiness, so a render without a due owned
// business means the build gate broke.
func TestGoldensEquipmentCueOnlyForDueOwner(t *testing.T) {
	const marker = "## Your equipment"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, marker) {
				return
			}
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState != nil {
				t.Fatalf("scenario %q: %q rendered for a visitor/absent subject", sc.name, marker)
			}
			if sim.DueOwnedBusiness(snap.VillageObjects, actorID, snap.EquipmentServiceDueThreshold) == nil {
				t.Errorf("scenario %q: %q rendered with no due owned business", sc.name, marker)
			}
		})
	}
}

// TestGoldensWrightTradeCueOnlyForWright — "## Your trade" may render only for
// a resident subject working a wright-tagged structure.
func TestGoldensWrightTradeCueOnlyForWright(t *testing.T) {
	const marker = "## The wright's rounds"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, marker) {
				return
			}
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState != nil || !sim.ActorIsWright(snap.VillageObjects, a.WorkStructureID, actorID) {
				t.Errorf("scenario %q: %q rendered for a non-wright subject", sc.name, marker)
			}
		})
	}
}

// TestGoldensEquipmentPayImperativeNeedsWright — the owner-side pay_with_item
// imperative ("have him see to it") may render only when a wright shares the
// subject's huddle; without one the cue must stay a standing fact.
func TestGoldensEquipmentPayImperativeNeedsWright(t *testing.T) {
	const imperative = "have him see to it"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, imperative) {
				return
			}
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.CurrentHuddleID == "" {
				t.Fatalf("scenario %q: the pay imperative rendered outside any huddle", sc.name)
			}
			h := snap.Huddles[a.CurrentHuddleID]
			if h == nil {
				t.Fatalf("scenario %q: subject's huddle missing from the snapshot", sc.name)
			}
			hasWright := false
			for id := range h.Members {
				if id == actorID {
					continue
				}
				if m := snap.Actors[id]; m != nil && sim.ActorIsWright(snap.VillageObjects, m.WorkStructureID, id) {
					hasWright = true
				}
			}
			if !hasWright {
				t.Errorf("scenario %q: the pay imperative rendered with no wright co-present", sc.name)
			}
		})
	}
}
