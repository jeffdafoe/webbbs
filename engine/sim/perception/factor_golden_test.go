package perception

import (
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// factor_golden_test.go — golden scenarios + cross-scenario invariants for the wholesale factor,
// now the SELL instance of a merchant errand (LLM-455, generalizing LLM-410). His trade steer is
// folded into the errand-anchored "## Your rounds" surface (at the distributor: the two-way deal
// naming pay_with_item; between legs: the steer to the distributor with the other shops cast as
// talk-only social calls). The distributor keeper's heads-up is the generalized "## A trader's
// come to deal" cue. Registered into perceptionScenarios so TestPerceptionGoldens covers them.

func init() {
	perceptionScenarios = append(perceptionScenarios,
		perceptionScenario{
			name: "factor_at_distributor",
			summary: "LLM-455: a Boston factor stands in the General Store with the distributor co-present. " +
				"'## Your rounds' cues the two-way deal — sell his bale, buy the surplus — and names pay_with_item " +
				"for the buy leg.",
			build: factorAtDistributorScenario,
		},
		perceptionScenario{
			name: "factor_seeks_distributor",
			summary: "LLM-455: a factor between legs, out in the open, with a weaver's shop also open. '## Your " +
				"rounds' points him at the distributor's store (his errand counterparty) with a bearing, and casts " +
				"the weaver as a talk-only social call (no trading there).",
			build: factorSeeksDistributorScenario,
		},
		perceptionScenario{
			name: "distributor_views_factor",
			summary: "LLM-455: the distributor's own view with a factor co-present. '## A trader's come to deal' " +
				"tells the keeper who he is and that he deals both ways, and names pay_with_item for the leg the " +
				"keeper drives (buying the factor's bale). LLM-647: the cue lists the factor's actual pack with " +
				"the keeper's own worth per good (catalog-seeded here — coat and cloak price, the silver_locket " +
				"doesn't and renders without a figure) plus the weigh-his-asks counsel, so an above-market ask " +
				"has counter-pressure in the scene.",
			build: distributorViewsFactorScenario,
		},
	)
}

// factorActor builds the standard factor subject snapshot used by the scenarios.
func factorActor(pos sim.TilePos, inside sim.StructureID, huddle sim.HuddleID) *sim.ActorSnapshot {
	return &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Master Whitcombe the factor",
		State:             sim.StateIdle,
		Pos:               pos,
		InsideStructureID: inside,
		CurrentHuddleID:   huddle,
		Coins:             180,
		Inventory:         map[sim.ItemKind]int{"coat": 3, "cloak": 3, "silver_locket": 2},
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			// LLM-644: budget = purse, as at spawn.
			SpendBudget: 180,
			Archetype:   sim.FactorArchetype,
			Origin:      sim.FactorOrigin,
			Disposition: "mercenary",
			Phase:       sim.VisitorPhaseMakingRounds,
			// The factor is the SELL instance of a merchant errand (LLM-455): his counterparty
			// is the distributor's General Store.
			Trade: &sim.TradeErrand{Direction: sim.TradeDirectionSell, Good: "iron", Counterparty: "general_store"},
		},
	}
}

// distributorKeeper builds Josiah, the distributor keeper, at the distributor-tagged store.
func distributorKeeper(pos sim.TilePos, huddle sim.HuddleID) *sim.ActorSnapshot {
	return &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Josiah Thorne",
		Role:               "storekeeper",
		State:              sim.StateIdle,
		Pos:                pos,
		WorkStructureID:    "general_store",
		InsideStructureID:  "general_store",
		CurrentHuddleID:    huddle,
		Coins:              60,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "storekeeper"},
	}
}

// distributorObjects returns the General Store village-object (distributor-tagged) + structure.
func distributorObjects() (map[sim.VillageObjectID]*sim.VillageObject, map[sim.StructureID]*sim.Structure) {
	return map[sim.VillageObjectID]*sim.VillageObject{
			"general_store": {ID: "general_store", Pos: sim.WorldPos{X: 320, Y: 320}, Tags: []string{sim.TagBusiness, sim.TagDistributor}},
		},
		map[sim.StructureID]*sim.Structure{
			"general_store": plainStructure("general_store", "The General Store"),
		}
}

func factorAtDistributorScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const factorID = sim.ActorID("vstr-0000fac0")
	const josiahID = sim.ActorID("josiah")
	now := 540 // 09:00 daytime
	factor := factorActor(sim.TilePos{X: 40, Y: 40}, "general_store", "h1")
	josiah := distributorKeeper(sim.TilePos{X: 41, Y: 40}, "h1")
	vobjs, structs := distributorObjects()
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{factorID: factor, josiahID: josiah},
		Structures:       structs,
		VillageObjects:   vobjs,
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{factorID: {}, josiahID: {}}},
		},
	}
	return snap, factorID, nil
}

func factorSeeksDistributorScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		factorID = sim.ActorID("vstr-0000fac0")
		josiahID = sim.ActorID("josiah")
		weaverID = sim.ActorID("goodwife-mary")
		weaver   = sim.StructureID("weaver")
	)
	now := 600                                                // 10:00 daytime
	factor := factorActor(sim.TilePos{X: 80, Y: 120}, "", "") // out in the open, no huddle
	josiah := distributorKeeper(sim.TilePos{X: 100, Y: 100}, "")
	// A weaver's shop is also open — the ordinary rounds cue WOULD list it, but a factor's
	// cue must not: he trades with no one but the distributor.
	weav := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Goodwife Mary",
		Role:               "weaver",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 95, Y: 120},
		WorkStructureID:    weaver,
		InsideStructureID:  weaver,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "weaver"},
	}
	vobjs, structs := distributorObjects()
	vobjs[sim.VillageObjectID(weaver)] = &sim.VillageObject{ID: sim.VillageObjectID(weaver), Pos: sim.WorldPos{X: 1120, Y: 256}, Tags: []string{sim.TagBusiness}}
	structs[weaver] = plainStructure(weaver, "Weaver's")
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			factorID: factor, josiahID: josiah, weaverID: weav,
		},
		Structures:     structs,
		VillageObjects: vobjs,
	}
	return snap, factorID, nil
}

func distributorViewsFactorScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const factorID = sim.ActorID("vstr-0000fac0")
	const josiahID = sim.ActorID("josiah")
	now := 540 // 09:00 daytime
	factor := factorActor(sim.TilePos{X: 40, Y: 40}, "general_store", "h1")
	josiah := distributorKeeper(sim.TilePos{X: 41, Y: 40}, "h1")
	vobjs, structs := distributorObjects()
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{factorID: factor, josiahID: josiah},
		Structures:       structs,
		VillageObjects:   vobjs,
		// Catalog seeds for the LLM-647 pack-worth line: the keeper has no ledger
		// history here, so coat and cloak price off the catalog while the
		// silver_locket (no recipe) stays unpriced and must render without a figure.
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"coat":  {WholesalePrice: 5},
			"cloak": {WholesalePrice: 4},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{factorID: {}, josiahID: {}}},
		},
	}
	return snap, josiahID, nil // subject is the DISTRIBUTOR, not the factor
}

// TestGoldensErrandVisitCueOnlyForKeeper — "## A trader's come to deal" (LLM-455) may render
// only for a resident KEEPER subject (a businessowner at his own post, never a visitor); it is
// the counterparty keeper's heads-up that the merchant he's bound to is co-present.
func TestGoldensErrandVisitCueOnlyForKeeper(t *testing.T) {
	const marker = "## A trader's come to deal"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, marker) {
				return
			}
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState != nil || a.BusinessownerState == nil || a.WorkStructureID == "" {
				t.Errorf("scenario %q: %q rendered for a non-keeper subject", sc.name, marker)
			}
		})
	}
}

// TestGoldensMerchantRoundsConfinesCommerce — a merchant on his rounds carries "## Your rounds"
// (the factor cue is folded into it, LLM-455), and whenever that cue lists other open shops it
// spells out that they are talk-only ("no trading there") — the legible half of the errand
// commerce-confinement.
func TestGoldensMerchantRoundsConfinesCommerce(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			isMerchant := a != nil && a.VisitorState != nil && a.VisitorState.Trade != nil
			if !isMerchant {
				return
			}
			onRounds := a.VisitorState.Phase == sim.VisitorPhaseArriving ||
				a.VisitorState.Phase == sim.VisitorPhaseMakingRounds ||
				a.VisitorState.Phase == sim.VisitorPhasePresent
			if onRounds && !strings.Contains(out, "## Your rounds") {
				t.Errorf("scenario %q: a merchant on his rounds lacks '## Your rounds'", sc.name)
			}
			if strings.Contains(out, "to look in on and pass the news") && !strings.Contains(out, "no trading there") {
				t.Errorf("scenario %q: rounds listed other shops without the talk-only confinement", sc.name)
			}
		})
	}
}
