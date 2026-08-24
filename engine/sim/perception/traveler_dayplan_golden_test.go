package perception

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// traveler_dayplan_golden_test.go — golden scenarios + cross-scenario invariants
// for the LLM-373 traveler day-plan cues (## On your rounds, ## A bed for the
// night). Registered into perceptionScenarios via init() so the whole-prompt golden
// + determinism harness (TestPerceptionGoldens) covers them alongside the rest.

func init() {
	perceptionScenarios = append(perceptionScenarios,
		perceptionScenario{
			name: "traveler_making_rounds_at_shop",
			summary: "LLM-455: a nail-buyer stands inside the Blacksmith — his errand counterparty — with the smith " +
				"co-present. '## Your rounds' cues the trade-here moment: buy the good he came for, naming pay_with_item " +
				"with the exact item kind. His commerce is confined to this one keeper.",
			build: travelerMakingRoundsScenario,
		},
		perceptionScenario{
			name: "traveler_seeking_bed_at_inn",
			summary: "LLM-373: a homeless peddler at the inn of an evening, the innkeeper co-present. The prompt " +
				"carries '## A bed for the night' — the booking cue that names pay_with_item for a nights_stay — " +
				"so the traveler books through the real lodging flow.",
			build: travelerSeekingBedScenario,
		},
		perceptionScenario{
			name: "traveler_between_legs_navigates",
			summary: "LLM-455: a nail-buyer between legs of his rounds — out in the open, not in any shop. '## Your " +
				"rounds' points him at his errand counterparty (the Smithy) with a bearing, casts the other open shop " +
				"(the Weaver's) as a talk-only social call, and shows the failing light — never a single 'go here " +
				"next' target. He navigates with move_to; his commerce is confined to the Smithy.",
			build: travelerBetweenLegsScenario,
		},
		perceptionScenario{
			name: "traveler_errand_settled_winds_down",
			summary: "LLM-455/508: a nail-buyer whose purchase has settled, with the light going — his errand is " +
				"done and it is late enough for bed. '## Your rounds' turns to the wind-down (his business is done, " +
				"the tavern's the place now for supper and a bed) instead of pressing his rounds — the legible " +
				"'business concluded' state that kills the loop.",
			build: travelerErrandSettledScenario,
		},
		perceptionScenario{
			name: "traveler_settled_pack_is_provisions",
			summary: "LLM-544: a settled provisioner mid-afternoon carrying the journeycakes he bought here plus " +
				"cheese and flour given him at a farm. The carrying line names the pack as his own — come by here and " +
				"bound home with him, not stock to sell — so the model has a stated purpose for the goods instead of " +
				"writing itself a travelling-salesman motive and hawking its own road food. The mixed pack is the " +
				"point: 'come by' covers a gift as well as a purchase, which 'bought' would not. The booked room in " +
				"the same pack is what the coda's 'goods' excludes (LLM-574).",
			build: travelerSettledPackScenario,
		},
		perceptionScenario{
			name: "traveler_settled_pack_is_errand_good",
			summary: "LLM-574: the live 2026-07-30 shape — Tobias Hewes the nail-buyer, his nine nails bought from " +
				"the smith, calling at a farm that is NOT his counterparty. The case LLM-544 did not cover: the pack " +
				"holds the ERRAND good and the persona is named for it, so 'a nail-buyer with nine nails' read as a " +
				"travelling salesman and he offered them to everyone he called on. Pins all three halves of the " +
				"answer — the preface says what a nail-buyer does with what he buys, the inventory line carries the " +
				"bound-home coda where the goods actually are, and the settled lead counts them as nine. The " +
				"visited line names the smithy he bought them at as well as the farm he called at after (LLM-575).",
			build: travelerSettledErrandGoodScenario,
		},
		perceptionScenario{
			name: "traveler_rounds_skip_meeting_house",
			summary: "LLM-554: a factor between legs of his rounds, with the constable at his post in the Meeting " +
				"House and the weaver at hers. '## Your rounds' offers the Weaver's alone — a meeting house is not a " +
				"place of business, however plainly somebody is working in it, and the live factor sent to pass the " +
				"news at one apologized for the intrusion himself.",
			build: travelerRoundsMeetingHouseScenario,
		},
		perceptionScenario{
			name: "traveler_factor_shipment_delivered",
			summary: "LLM-553: a wholesale factor whose iron shipment has gone into the village, still standing " +
				"with the distributor keeper mid-afternoon. His SELL errand has settled — which no sell errand " +
				"could do before, so '## Your rounds' pressed the trade-here steer at this one keeper all day " +
				"while commerce confinement made every other stop talk-only, and he walked back to this counter " +
				"over and over. Pins the settled-SELLER wind-down, written in LLM-507 and never rendered until " +
				"now: his business is done and the day is his for social calls, even though cloth and charms " +
				"remain in the bale.",
			build: travelerFactorShipmentDeliveredScenario,
		},
		perceptionScenario{
			name: "traveler_errand_settled_midday",
			summary: "LLM-507/508: the same settled nail-buyer, but at midday — hours of daylight left. The settled " +
				"lead gives him the day for social calls instead of pitching supper-and-bed (which had him announcing " +
				"goodnight all afternoon), and the nightfall line renders the social-circuit variant (visit the other " +
				"businesses) instead of 'for your trade' — neither line may contradict 'your business is done'.",
			build: travelerErrandSettledMiddayScenario,
		},
	)
}

// travelerErrandSettledMiddayScenario is the settled wind-down scenario with the clock
// pulled back to midday, so minutes-to-dusk lands above the bed-pressure boundary: the
// settled lead renders its rest-of-the-day social variant (LLM-508) and the nightfall
// line its social-circuit variant (LLM-507).
func travelerErrandSettledMiddayScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id, warrants := travelerErrandSettledScenario()
	midday := 780 // 13:00 — five hours to dusk (1080), but his trade is behind him
	snap.LocalMinuteOfDay = &midday
	return snap, id, warrants
}

// travelerSettledPackScenario reproduces the live 2026-07-27 shape (LLM-544): Brother
// Ashford, a settled buy-errand provisioner, mid-afternoon with hours of light left and
// a MIXED pack — the journeycakes his errand bought plus the cheese and flour Elizabeth
// Ellis handed him at her farm. The multi-item join and the count-aware nouns are what
// this pins; the single-good case rides the two nail-buyer settled goldens. A real
// ItemKinds catalog is supplied so the nouns render as authored phrases rather than raw
// keys, and nights_stay is present to pin that a booked room never turns the coda into a
// claim about a granted room.
func travelerSettledPackScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		buyerID = sim.ActorID("vstr-e10f3a91")
		tavern  = sim.StructureID("tavern")
	)
	now := 900 // 15:00 — three hours to dusk (1080), well clear of bed pressure
	buyer := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Brother Ashford the provisioner",
		State:       sim.StateIdle,
		Pos:         sim.TilePos{X: 40, Y: 44},
		Coins:       75,
		// nights_stay rides here deliberately: a booked room is a grant, not a carried
		// good (see travelerPackBoundHome).
		Inventory: map[sim.ItemKind]int{"journeycake": 4, "cheese": 1, "flour": 1, "nights_stay": 1},
		Needs:     map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			// LLM-644: budget = purse, as at spawn.
			SpendBudget:       75,
			Archetype:         "provisioner",
			Origin:            "Salem Town",
			Disposition:       "weary",
			Phase:             sim.VisitorPhaseMakingRounds,
			VisitedBusinesses: []sim.StructureID{tavern},
			Trade:             &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "journeycake", Counterparty: tavern, Settled: true},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{buyerID: buyer},
		Structures:       map[sim.StructureID]*sim.Structure{tavern: plainStructure(tavern, "Tavern")},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {ID: sim.VillageObjectID(tavern), Pos: sim.WorldPos{X: 320, Y: 352}, Tags: []string{sim.TagBusiness}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"journeycake": {Name: "journeycake", DisplayLabel: "Journeycake", DisplayLabelSingular: "journeycake", DisplayLabelPlural: "journeycakes"},
			"cheese":      {Name: "cheese", DisplayLabel: "Cheese", DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese"},
			"flour":       {Name: "flour", DisplayLabel: "Flour", DisplayLabelSingular: "sack of flour", DisplayLabelPlural: "sacks of flour"},
			"nights_stay": {Name: "nights_stay", DisplayLabel: "Night's Stay", DisplayLabelSingular: "night's stay", Capabilities: []string{"service"}},
		},
	}
	return snap, buyerID, nil
}

// travelerSettledErrandGoodScenario reproduces the live 2026-07-30 shape (LLM-574):
// Tobias Hewes, a nail-buyer out of Lynn, nine nails bought from Ezekiel Crane at the
// Blacksmith and stowed, mid-afternoon with light left, standing at Ellis Farm with the
// farm's keeper — a shop that is NOT his errand counterparty, where his commerce tools
// are stripped and every word he can say about the nails is talk. He hawked them anyway.
//
// Deliberately distinct from travelerSettledPackScenario: there the pack is road food
// bought incidentally and the persona ("provisioner") is not named for it. Here the pack
// IS the errand good and the label is minted from it, which is the combination that
// beat the LLM-544 line.
func travelerSettledErrandGoodScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		buyerID  = sim.ActorID("vstr-fb50246e")
		keeperID = sim.ActorID("elizabeth")
		smithy   = sim.StructureID("blacksmith")
		farm     = sim.StructureID("ellis_farm")
	)
	now := 900 // 15:00 — three hours to dusk (1080), well clear of bed pressure
	buyer := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Tobias Hewes the nail-buyer",
		State:       sim.StateIdle,
		Pos:         sim.TilePos{X: 96, Y: 120},
		Coins:       54,
		Inventory:   map[sim.ItemKind]int{"nail": 9},
		Needs:       map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 54,
			Archetype:   "nail-buyer",
			Origin:      "Lynn",
			Disposition: "reserved",
			Phase:       sim.VisitorPhaseMakingRounds,
			// The smithy FIRST, then the farm — the order he called at them. The
			// counterparty belongs in this list like any other stop (LLM-575); live it
			// was missing, because the smith was at his wood pile when Tobias walked in
			// and the arrival found nobody to call on. He then told the apothecary he
			// had got his nails at the General Store, which is what the list named.
			VisitedBusinesses: []sim.StructureID{smithy, farm},
			Trade:             &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "nail", Counterparty: smithy, Settled: true},
		},
	}
	keeper := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Elizabeth Ellis",
		Role:               "farmer",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 96, Y: 118},
		WorkStructureID:    farm,
		InsideStructureID:  farm,
		CurrentHuddleID:    "h1",
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "farmer"},
	}
	buyer.CurrentHuddleID = "h1"
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{buyerID: buyer, keeperID: keeper},
		Structures: map[sim.StructureID]*sim.Structure{
			smithy: plainStructure(smithy, "Blacksmith"),
			farm:   plainStructure(farm, "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(smithy): {ID: sim.VillageObjectID(smithy), Pos: sim.WorldPos{X: 640, Y: 0}, Tags: []string{sim.TagBusiness}},
			sim.VillageObjectID(farm):   {ID: sim.VillageObjectID(farm), Pos: sim.WorldPos{X: 768, Y: 944}, Tags: []string{sim.TagBusiness}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{buyerID: {}, keeperID: {}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
		},
	}
	return snap, buyerID, nil
}

func travelerErrandSettledScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		buyerID = sim.ActorID("vstr-0000abcd")
		smithID = sim.ActorID("ezekiel")
		smithy  = sim.StructureID("smithy")
	)
	now := 1050 // 17:30 — half an hour to dusk (1080): the light going, bed-pressure time (LLM-508)
	buyer := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Elias Drum the nail-buyer",
		State:       sim.StateIdle,
		Pos:         sim.TilePos{X: 80, Y: 120},
		Coins:       78,
		Inventory:   map[sim.ItemKind]int{"nail": 6},
		Needs:       map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget:       78,
			Archetype:         "nail-buyer",
			Origin:            "Boston",
			Disposition:       "weary",
			Phase:             sim.VisitorPhaseMakingRounds,
			VisitedBusinesses: []sim.StructureID{smithy},
			Trade:             &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "nail", Counterparty: smithy, Settled: true},
		},
	}
	smith := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Ezekiel Crane",
		Role:               "blacksmith",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 80, Y: 112},
		WorkStructureID:    smithy,
		InsideStructureID:  smithy,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "smith"},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{buyerID: buyer, smithID: smith},
		Structures:       map[sim.StructureID]*sim.Structure{smithy: plainStructure(smithy, "Smithy")},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(smithy): {ID: sim.VillageObjectID(smithy), Pos: sim.WorldPos{X: 640, Y: 0}, Tags: []string{sim.TagBusiness}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
		},
	}
	return snap, buyerID, nil
}

// travelerRoundsMeetingHouseScenario reproduces the live 2026-07-28 shape (LLM-554):
// Daniel Holcomb the factor between legs of his rounds, the constable standing his post
// inside the Meeting House. The constable is the shape that broke it — a keeper-like
// actor whose WorkStructureID is a structure with no TagBusiness on its object — so he
// is built with no BusinessownerState, exactly as the live constable has none. The
// weaver stands beside him in the fixture on purpose: without a genuine open shop the
// OpenShops line would vanish entirely and the golden could not tell "the meeting house
// was excluded" from "the line never rendered".
func travelerRoundsMeetingHouseScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		factorID     = sim.ActorID("vstr-3bcaba3e")
		constableID  = sim.ActorID("gideon")
		weaverID     = sim.ActorID("goodwife-mary")
		store        = sim.StructureID("general_store")
		meetingHouse = sim.StructureID("meeting_house")
		weaver       = sim.StructureID("weaver")
	)
	now := 960 // 16:00 — the afternoon wearing on (dusk 18:00), his rounds still open
	factor := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Daniel Holcomb the factor",
		State:       sim.StateIdle,
		Pos:         sim.TilePos{X: 80, Y: 120}, // out in the open, no shop, no huddle
		Coins:       203,
		Inventory:   map[sim.ItemKind]int{"iron": 1},
		Needs:       map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 203,
			Archetype:   "factor",
			Origin:      "Boston",
			Disposition: "warm",
			Phase:       sim.VisitorPhaseMakingRounds,
			Trade:       &sim.TradeErrand{Direction: sim.TradeDirectionSell, Good: "iron", Counterparty: store},
		},
	}
	// At his post, awake, on shift — everything snapshotKeeperPresent asks for. What he
	// is NOT is a shopkeeper, and the Meeting House is not a shop.
	constable := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Constable Gideon Marsh",
		Role:              "constable",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 80, Y: 112},
		WorkStructureID:   meetingHouse,
		InsideStructureID: meetingHouse,
		Needs:             map[sim.NeedKey]int{},
	}
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
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			factorID: factor, constableID: constable, weaverID: weav,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store:        plainStructure(store, "General Store"),
			meetingHouse: plainStructure(meetingHouse, "Meeting House"),
			weaver:       plainStructure(weaver, "Weaver's"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store):  {ID: sim.VillageObjectID(store), Pos: sim.WorldPos{X: 320, Y: 320}, Tags: []string{sim.TagBusiness, sim.TagDistributor}},
			sim.VillageObjectID(weaver): {ID: sim.VillageObjectID(weaver), Pos: sim.WorldPos{X: 1120, Y: 256}, Tags: []string{sim.TagBusiness}},
			// The live Meeting House object carries "meeting-house" and nothing else.
			sim.VillageObjectID(meetingHouse): {ID: sim.VillageObjectID(meetingHouse), Pos: sim.WorldPos{X: 640, Y: 0}, Tags: []string{"meeting-house"}},
		},
	}
	return snap, factorID, nil
}

func travelerBetweenLegsScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		peddlerID = sim.ActorID("vstr-0000abcd")
		smithID   = sim.ActorID("ezekiel")
		weaverID  = sim.ActorID("goodwife-mary")
		smithy    = sim.StructureID("smithy")
		weaver    = sim.StructureID("weaver")
		cooper    = sim.StructureID("cooper") // already called at
	)
	now := 960 // 16:00 — the afternoon wearing on (dusk 18:00)
	peddler := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Elias Drum the nail-buyer",
		State:       sim.StateIdle,
		Pos:         sim.TilePos{X: 80, Y: 120}, // out in the open, no shop, no huddle (padded tile)
		Coins:       90,
		Needs:       map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget:       90,
			Archetype:         "nail-buyer",
			Origin:            "Boston",
			Disposition:       "weary",
			Phase:             sim.VisitorPhaseMakingRounds,
			VisitedBusinesses: []sim.StructureID{cooper},
			// His errand: buy nails from the Smithy (his must-hit counterparty). The weaver is a
			// talk-only social call.
			Trade: &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "nail", Counterparty: smithy},
		},
	}
	// Two shops still open, at distinct bearings from the peddler; the smith is nearer
	// (north), the weaver a short way east.
	smith := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Ezekiel Crane",
		Role:               "blacksmith",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 80, Y: 112}, // north of the peddler
		WorkStructureID:    smithy,
		InsideStructureID:  smithy,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "smith"},
	}
	weav := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Goodwife Mary",
		Role:               "weaver",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 95, Y: 120}, // a short way east of the peddler
		WorkStructureID:    weaver,
		InsideStructureID:  weaver,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "weaver"},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			peddlerID: peddler, smithID: smith, weaverID: weav,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			smithy: plainStructure(smithy, "Smithy"),
			weaver: plainStructure(weaver, "Weaver's"),
			cooper: plainStructure(cooper, "Cooper's"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// WorldPos.Tile() adds PadX=60/PadY=112: smithy → tile {80,112} (due north,
			// ~8 tiles), weaver → tile {95,120} (east, ~15 tiles).
			sim.VillageObjectID(smithy): {ID: sim.VillageObjectID(smithy), Pos: sim.WorldPos{X: 640, Y: 0}, Tags: []string{sim.TagBusiness}},
			sim.VillageObjectID(weaver): {ID: sim.VillageObjectID(weaver), Pos: sim.WorldPos{X: 1120, Y: 256}, Tags: []string{sim.TagBusiness}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
		},
	}
	return snap, peddlerID, nil
}

func travelerMakingRoundsScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		peddlerID  = sim.ActorID("vstr-0000abcd")
		smithID    = sim.ActorID("ezekiel")
		blacksmith = sim.StructureID("blacksmith")
	)
	now := 540 // 09:00 — daytime
	peddler := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the nail-buyer",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		InsideStructureID: blacksmith,
		CurrentHuddleID:   "h1",
		Coins:             90,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 90,
			Archetype:   "nail-buyer",
			Origin:      "Boston",
			Disposition: "weary",
			Phase:       sim.VisitorPhaseMakingRounds,
			// The Blacksmith IS his errand counterparty — he stands with the smith, the trade-here moment.
			Trade: &sim.TradeErrand{Direction: sim.TradeDirectionBuy, Good: "nail", Counterparty: blacksmith},
		},
	}
	smith := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Ezekiel Crane",
		Role:               "blacksmith",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 11, Y: 10},
		WorkStructureID:    blacksmith,
		InsideStructureID:  blacksmith,
		CurrentHuddleID:    "h1",
		Coins:              12,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "smith"},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{peddlerID: peddler, smithID: smith},
		Structures:       map[sim.StructureID]*sim.Structure{blacksmith: plainStructure(blacksmith, "Blacksmith")},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{peddlerID: {}, smithID: {}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
		},
	}
	return snap, peddlerID, nil
}

func travelerSeekingBedScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		peddlerID = sim.ActorID("vstr-0000abcd")
		keeperID  = sim.ActorID("hannah")
		inn       = sim.StructureID("inn")
	)
	now := 1170 // 19:30 — evening (past dusk 18:00, before bedtime 22:00)
	peddler := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 20, Y: 20},
		InsideStructureID: inn,
		CurrentHuddleID:   "h1",
		Coins:             40,
		Inventory:         map[sim.ItemKind]int{"cheese": 4, "iron": 2},
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 40,
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
			Phase:       sim.VisitorPhaseLodging,
		},
	}
	keeper := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Goodwife Hannah",
		Role:               "innkeeper",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 21, Y: 20},
		WorkStructureID:    inn,
		InsideStructureID:  inn,
		CurrentHuddleID:    "h1",
		Coins:              25,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "innkeeper"},
	}
	snap := &sim.Snapshot{
		PublishedAt:          time.Date(2026, 7, 12, 19, 30, 0, 0, time.UTC),
		LocalMinuteOfDay:     &now,
		DawnMinute:           360,
		DuskMinute:           1080,
		DawnDuskMinuteOK:     true,
		LodgingBedtimeMinute: 1320,
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{peddlerID: peddler, keeperID: keeper},
		Structures:           map[sim.StructureID]*sim.Structure{inn: plainStructure(inn, "Hannah's Inn")},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn): {
				ID:   sim.VillageObjectID(inn),
				Pos:  sim.WorldPos{X: 320, Y: 320},
				Tags: []string{sim.VisitorTagTavern, "lodging", sim.TagBusiness},
			},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{peddlerID: {}, keeperID: {}}},
		},
	}
	return snap, peddlerID, nil
}

// TestRoundsSettledNoClockStaysSocial — on an unusable dawn/dusk clock the settled
// wind-down renders its social variant and the nightfall line is suppressed entirely
// (LLM-508): a bedtime claim needs a clock to stand on, and MinutesToDusk's zero
// value must not read as dusk.
func TestRoundsSettledNoClockStaysSocial(t *testing.T) {
	var b strings.Builder
	renderTravelerRounds(&b, &TravelerRoundsView{
		Errand: &RoundsErrand{Buy: true, GoodLabel: "nail", Settled: true},
	})
	out := b.String()
	for _, phrase := range []string{"supper and a bed", "see about a bed", "light has all but gone"} {
		if strings.Contains(out, phrase) {
			t.Errorf("no-clock settled rounds rendered bedtime copy %q:\n%s", phrase, out)
		}
	}
	if !strings.Contains(out, "the rest of the day is yours") {
		t.Errorf("no-clock settled rounds missing the social wind-down lead:\n%s", out)
	}
}

// TestGoldensNoDaylightBedContradiction — a prompt that says there is plenty of
// light left must not, anywhere, press toward supper and a bed (LLM-508): the
// settled wind-down and the nightfall line key on the same roundsBedPressureMins
// boundary precisely so this pair can never co-occur. Cross-scenario invariant
// over the whole matrix; the positive cases are pinned by the settled goldens.
func TestGoldensNoDaylightBedContradiction(t *testing.T) {
	daylight := []string{"plenty of daylight left", "plenty of light left"}
	bed := []string{"supper and a bed", "see about a bed"}
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			for _, d := range daylight {
				if !strings.Contains(out, d) {
					continue
				}
				for _, b := range bed {
					if strings.Contains(out, b) {
						t.Errorf("scenario %q: prompt claims %q yet presses %q — the daylight and bed-pressure copy contradict (LLM-508)", sc.name, d, b)
					}
				}
			}
		})
	}
}

// TestGoldensProvisionsLineOnlyForSettledBuyer — the pack-is-your-own claim (LLM-544,
// moved onto the inventory line by LLM-574) may render ONLY for a traveler on a SETTLED
// BUY errand. It rests on a buyer spawning empty-packed, so everything he carries was
// come by here; a factor's pack is imported trade stock and calling it "not stock to
// sell" would be false, and would undercut the two-way deal his own cue is pressing.
// Cross-scenario matrix guard — the positive cases are pinned by the settled goldens.
func TestGoldensProvisionsLineOnlyForSettledBuyer(t *testing.T) {
	const marker = "your own, come by here"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if !strings.Contains(renderScenario(sc), marker) {
				return
			}
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState == nil || a.VisitorState.Trade == nil {
				t.Fatalf("scenario %q: provisions line rendered for a subject carrying no merchant errand (LLM-544)", sc.name)
			}
			if trade := a.VisitorState.Trade; trade.Direction != sim.TradeDirectionBuy || !trade.Settled {
				t.Errorf("scenario %q: provisions line rendered for a %s errand (settled=%v) — it is scoped to a settled BUY (LLM-544)",
					sc.name, trade.Direction, trade.Settled)
			}
		})
	}
}

// TestBuyerVocationCatalogFallbacks — the buyer vocation sentence is built on the
// errand good's PLURAL ("You buy nails … carry them home"), so a good with no catalog
// phrase must drop the sentence rather than render "You buy nail". The uncatalogued
// case also pins a cross-package contract this leans on: sim.ItemKindDef.Plural is
// nil-receiver-safe (it checks d == nil before anything else), so the map miss returns
// an empty string instead of panicking. If a later edit in sim makes Plural dereference
// its receiver, this test fails here instead of panicking in a live prompt build
// (code_review). A SELLER never gets the sentence at all — his calling IS to sell what
// he carries.
func TestBuyerVocationCatalogFallbacks(t *testing.T) {
	catalog := map[sim.ItemKind]*sim.ItemKindDef{
		"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
	}
	cases := []struct {
		name  string
		kinds map[sim.ItemKind]*sim.ItemKindDef
		good  sim.ItemKind
		dir   sim.TradeDirection
		orign string
		want  string
	}{
		{"catalogued buyer", catalog, "nail", sim.TradeDirectionBuy, "Lynn",
			"You buy nails in villages like this one and carry them home to Lynn, where your trade is."},
		{"catalogued buyer, no origin", catalog, "nail", sim.TradeDirectionBuy, "",
			"You buy nails in villages like this one and carry them home, where your trade is."},
		{"uncatalogued good", catalog, "dried_fish", sim.TradeDirectionBuy, "Lynn", ""},
		{"empty catalog", nil, "nail", sim.TradeDirectionBuy, "Lynn", ""},
		{"seller", catalog, "nail", sim.TradeDirectionSell, "Boston", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &sim.Snapshot{ItemKinds: tc.kinds}
			vs := &sim.VisitorState{
				Origin: tc.orign,
				Trade:  &sim.TradeErrand{Direction: tc.dir, Good: tc.good},
			}
			if got := travelerBuyerVocation(snap, vs); got != tc.want {
				t.Errorf("vocation = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuyerVocationNoErrand — a passer-through carries no trade errand at all, and the
// preface's own vocation map already speaks for him. nil VisitorState / nil Trade must
// return empty rather than panic, since buildTravelerSelf calls this for every traveler.
func TestBuyerVocationNoErrand(t *testing.T) {
	snap := &sim.Snapshot{}
	if got := travelerBuyerVocation(snap, nil); got != "" {
		t.Errorf("nil visitor state: vocation = %q, want empty", got)
	}
	if got := travelerBuyerVocation(snap, &sim.VisitorState{Archetype: "messenger"}); got != "" {
		t.Errorf("errandless traveler: vocation = %q, want empty", got)
	}
}

// TestGoldensPackClaimRidesTheInventoryLine — wherever the bound-home claim renders, it
// renders ON the carrying line (LLM-574). That adjacency IS the fix: LLM-544's wording
// was already in the live nail-buyer's prompt, a section and a conversation away from
// the inventory readout, and it lost to the readout — which for every other actor in the
// village is the sellable-stock line. A later edit that moves the claim back into its own
// paragraph would restore the defect while every other assertion here still passed.
func TestGoldensPackClaimRidesTheInventoryLine(t *testing.T) {
	const marker = "your own, come by here"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, line := range strings.Split(renderScenario(sc), "\n") {
				if strings.Contains(line, marker) && !strings.HasPrefix(line, "You are carrying: ") {
					t.Errorf("scenario %q: the bound-home claim rendered off the carrying line (LLM-574):\n%s", sc.name, line)
				}
			}
		})
	}
}

// TestGoldensVisitedListNamesEveryRecordedStop — every business the engine recorded as a
// stop is named back to him, the errand counterparty included (LLM-575). The render
// filters the counterparty out of the OPEN-shops list three lines away, and rightly so —
// it is offered as the must-hit stop instead of a talk-only social call — but the two
// lists answer different questions, and a filter that grew to cover both would tell a
// traveler he had never been where he did his business. That is the shape of the live
// defect: he told the apothecary he had got his nails at the General Store because the
// smithy was missing from this line. Matrix-wide, so it holds for any traveler scenario.
//
// Distinct display names are a fixture requirement, not an assumption about the village:
// two shops sharing a name would make a substring hit ambiguous, and the goldens name
// them apart.
func TestGoldensVisitedListNamesEveryRecordedStop(t *testing.T) {
	const visitedLinePrefix = "So far you've called at "
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState == nil || len(a.VisitorState.VisitedBusinesses) == 0 {
				return
			}
			// Against the visited LINE, not the whole prompt: a counterparty's display
			// name also appears in the errand steer and in "You're with <keeper> at
			// <shop>", so a whole-prompt search would pass on exactly the scenario this
			// exists to guard (code_review).
			var visitedLine string
			for _, line := range strings.Split(renderScenario(sc), "\n") {
				if strings.HasPrefix(line, visitedLinePrefix) {
					visitedLine = line
					break
				}
			}
			if visitedLine == "" {
				t.Fatalf("scenario %q: %d stop(s) recorded but no %q line rendered at all (LLM-575)",
					sc.name, len(a.VisitorState.VisitedBusinesses), visitedLinePrefix)
			}
			for _, sid := range a.VisitorState.VisitedBusinesses {
				st := snap.Structures[sid]
				if st == nil || st.DisplayName == "" {
					continue
				}
				if !strings.Contains(visitedLine, st.DisplayName) {
					t.Errorf("scenario %q: %q was recorded as a stop he called at but is missing from his visited line (LLM-575):\n%s",
						sc.name, st.DisplayName, visitedLine)
				}
			}
		})
	}
}

// TestPackBoundHomeScopedToSettledBuy — the negative cases asserted DIRECTLY rather
// than only through the matrix guard, which passes vacuously if the covering scenario
// is ever dropped from perceptionScenarios (code_review). A factor carries real imported
// stock to sell, and an unsettled buyer is still mid-errand; neither may be told his
// pack is his own. The subject is a settled buyer in every case, mutated one field at a
// time, so a failure names the field that carried it.
func TestPackBoundHomeScopedToSettledBuy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*sim.TradeErrand)
	}{
		{"settled seller", func(t *sim.TradeErrand) { t.Direction = sim.TradeDirectionSell }},
		{"unsettled buyer", func(t *sim.TradeErrand) { t.Settled = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t2 *testing.T) {
			snap, actorID, _ := travelerSettledPackScenario()
			a := snap.Actors[actorID]
			tc.mutate(a.VisitorState.Trade)
			if travelerPackBoundHome(snap, a) {
				t2.Errorf("pack read as bound home for a %s — the claim is scoped to a settled BUY errand (LLM-574)", tc.name)
			}
		})
	}
}

// TestPackBoundHomeNonTraveler — a resident keeper's stock is never his provisions,
// however full his pack. The gate reads VisitorState, so this pins that a nil one
// (every persistent NPC and PC) returns false rather than panicking.
func TestPackBoundHomeNonTraveler(t *testing.T) {
	snap, actorID, _ := travelerSettledPackScenario()
	a := snap.Actors[actorID]
	a.VisitorState = nil
	if travelerPackBoundHome(snap, a) {
		t.Error("pack read as bound home for a non-traveler subject (LLM-574)")
	}
}

// TestPackBoundHomeUncataloguedKind — an inventory kind with NO catalog row at all still
// counts as a carried good. This pins the nil-def path through the service check: only
// HasCapability needs the def != nil guard, and an unknown kind must not be silently
// treated as a service (which would suppress the coda for a pack that holds real goods).
func TestPackBoundHomeUncataloguedKind(t *testing.T) {
	snap, actorID, _ := travelerSettledPackScenario()
	a := snap.Actors[actorID]
	a.Inventory = map[sim.ItemKind]int{"dried_fish": 2} // deliberately absent from snap.ItemKinds
	if !travelerPackBoundHome(snap, a) {
		t.Error("an uncatalogued carried kind must still read as a good bound home (LLM-574)")
	}
}

// TestPackBoundHomeServicesOnly — a service in the pack (a booked night's stay) is a
// granted room, not a carried good. A pack holding NOTHING else carries no coda, since
// there is no good for the claim to be about; a pack holding a service beside real goods
// still does. Unit-level because the golden pins only the assembled prose.
func TestPackBoundHomeServicesOnly(t *testing.T) {
	snap, actorID, _ := travelerSettledPackScenario()
	a := snap.Actors[actorID]
	if !travelerPackBoundHome(snap, a) {
		t.Fatal("the scenario pack (goods plus a nights_stay) must read as bound home (LLM-574)")
	}
	a.Inventory = map[sim.ItemKind]int{"nights_stay": 1}
	if travelerPackBoundHome(snap, a) {
		t.Error("a pack holding only a booked room must carry no bound-home coda (LLM-574)")
	}
}

// TestSettledBuyLeadAgreesWithPackCount — nine nails are announced as "the nails are
// bought", one as "the nail is bought" (LLM-574). The live nail-buyer's lead said "the
// nail is bought" over a pack of nine, two sections below an inventory line reading
// "nails (x9)". Falls back to the abstract singular when the good has left his pack.
func TestSettledBuyLeadAgreesWithPackCount(t *testing.T) {
	cases := []struct {
		qty  int
		want string
	}{
		{9, "the nails are bought"},
		{1, "the nail is bought"},
		{0, "the nail is bought"}, // given away / eaten — no pack noun to speak of
	}
	for _, tc := range cases {
		snap, actorID, _ := travelerErrandSettledScenario()
		a := snap.Actors[actorID]
		a.Inventory = map[sim.ItemKind]int{"nail": tc.qty}
		snap.ItemKinds = map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails"},
		}
		var b strings.Builder
		renderTravelerRounds(&b, buildTravelerRounds(snap, a, nil))
		if !strings.Contains(b.String(), tc.want) {
			t.Errorf("pack of %d: settled lead missing %q:\n%s", tc.qty, tc.want, b.String())
		}
	}
}

// TestGoldensRoundsCueOnlyForTraveler — the "## On your rounds" section may render
// only for a transient-traveler subject; it must never leak into a persistent NPC /
// PC prompt. One-directional matrix guard (the positive case is pinned by the
// traveler_making_rounds_at_shop golden).
func TestGoldensRoundsCueOnlyForTraveler(t *testing.T) {
	const marker = "## On your rounds"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			if !strings.Contains(renderScenario(sc), marker) {
				return
			}
			if a := snap.Actors[actorID]; a == nil || a.VisitorState == nil {
				t.Errorf("scenario %q: %q rendered for a non-traveler subject — the rounds cue must be traveler-only (LLM-373)", sc.name, marker)
			}
		})
	}
}

// TestGoldensSeekBedCueTravelerOnlyAndNamesTool — the "## A bed for the night"
// section may render only for a traveler subject, and whenever it renders it must
// name the pay_with_item / nights_stay call (cue-tool lockstep): a booking cue with
// no tool named is a dead end for the weak model.
func TestGoldensSeekBedCueTravelerOnlyAndNamesTool(t *testing.T) {
	const marker = "## A bed for the night"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, marker) {
				return
			}
			snap, actorID, _ := sc.build()
			if a := snap.Actors[actorID]; a == nil || a.VisitorState == nil {
				t.Errorf("scenario %q: %q rendered for a non-traveler subject — the seek-a-bed cue must be traveler-only (LLM-373)", sc.name, marker)
			}
			if !strings.Contains(out, "pay_with_item") || !strings.Contains(out, "nights_stay") {
				t.Errorf("scenario %q: %q rendered without naming pay_with_item / nights_stay — the booking cue must name its tool (LLM-373)", sc.name, marker)
			}
		})
	}
}

// TestRoundsOpenShopsGatedOnBusinessTag pins the LLM-554 discriminator itself, which
// the golden alone cannot: a golden with the Meeting House absent reads the same
// whether the tag gate excluded it or the fixture never offered it. So this renders
// the scenario twice — once as live (the Meeting House object carries "meeting-house"
// and nothing else), once with TagBusiness added to that same object and NOTHING else
// changed. The constable, his post and his shift are identical across both; only the
// tag moves, and the line must follow it.
func TestRoundsOpenShopsGatedOnBusinessTag(t *testing.T) {
	const shops = "Others keeping shop this hour"

	snap, actorID, warrants := travelerRoundsMeetingHouseScenario()
	out := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
	if !strings.Contains(out, shops) {
		t.Fatalf("open-shops line missing entirely — the fixture must offer the Weaver's, or this test proves nothing:\n%s", out)
	}
	if strings.Contains(out, "Meeting House") {
		t.Errorf("the Meeting House was offered as a shop keeping shop; a meeting house is not a place of business (LLM-554):\n%s", out)
	}

	snap, actorID, warrants = travelerRoundsMeetingHouseScenario()
	mh := snap.VillageObjects["meeting_house"]
	mh.Tags = append(mh.Tags, sim.TagBusiness)
	out = combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
	if !strings.Contains(out, "Meeting House") {
		t.Errorf("tagging the same structure TagBusiness did not bring it back into the open-shops line, so the exclusion above is not the tag gate — check the fixture's keeper before trusting it:\n%s", out)
	}
}

// travelerFactorShipmentDeliveredScenario reproduces the live 2026-07-28 shape (LLM-553):
// Daniel Holcomb, a wholesale factor whose iron shipment has gone into the village, standing
// mid-afternoon with the distributor keeper he came to deal with. His errand is a SELL, and it
// has settled — which until LLM-553 was unreachable, because only a BUY errand could ever set
// Settled. So "## Your rounds" spent the whole daylight visit rendering the trade-here steer at
// this same keeper while commerce confinement made every other stop talk-only, and he walked
// back to this counter over and over.
//
// What this pins is the settled-SELLER wind-down prose, written in LLM-507 and never once
// rendered in production or in a golden until now. Note the pack he still carries: cloth and
// charms are the secondary bale, and the settled line must NOT claim those are sold — the
// errand is the iron headline, and the wind-down speaks to the day being his, not the pack
// being bare. He stays co-present with the keeper deliberately: the wind-down has to hold even
// standing at the counter, since that is exactly where the loop kept putting him.
func travelerFactorShipmentDeliveredScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		factorID     = sim.ActorID("vstr-3bcaba3e")
		keeperID     = sim.ActorID("josiah")
		generalStore = sim.StructureID("general_store")
	)
	now := 900 // 15:00 — three hours of daylight left (dusk 1080), well clear of bed pressure
	factor := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Daniel Holcomb the factor",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 94, Y: 126},
		InsideStructureID: generalStore,
		CurrentHuddleID:   "h1",
		Coins:             180,
		// The bale as the live factor carried it once his iron and salt had gone: the
		// clothing line largely unmoved, one bar of iron left of the ten he brought.
		Inventory: map[sim.ItemKind]int{
			"iron": 1, "salt": 1, "woolens": 2, "homespun": 3, "silver_locket": 2, "whalebone_charm": 3,
		},
		Needs: map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget:       180,
			Archetype:         "factor",
			Origin:            "Boston",
			Disposition:       "curious",
			Phase:             sim.VisitorPhaseMakingRounds,
			VisitedBusinesses: []sim.StructureID{generalStore},
			Trade: &sim.TradeErrand{
				Direction:    sim.TradeDirectionSell,
				Good:         "iron",
				Counterparty: generalStore,
				Settled:      true,
				ShipmentQty:  10,
			},
		},
	}
	keeper := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Josiah Thorne",
		Role:               "merchant",
		State:              sim.StateIdle,
		Pos:                sim.TilePos{X: 93, Y: 126},
		WorkStructureID:    generalStore,
		InsideStructureID:  generalStore,
		CurrentHuddleID:    "h1",
		Coins:              110,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{Flavor: "merchant"},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       360,
		DuskMinute:       1080,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{factorID: factor, keeperID: keeper},
		Structures:       map[sim.StructureID]*sim.Structure{generalStore: plainStructure(generalStore, "General Store")},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(generalStore): {ID: sim.VillageObjectID(generalStore), Pos: sim.WorldPos{X: 752, Y: 1008}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {Members: map[sim.ActorID]struct{}{factorID: {}, keeperID: {}}},
		},
	}
	return snap, factorID, nil
}

// TestGoldensSettledErrandNeverPressesTheTrade — LLM-553 cross-scenario invariant. A merchant
// whose errand has settled must never also be told to go and deal: "your business here is done"
// and the trade-here instruction are contradictory in the same breath, and the trade-here line
// is the one that carries a destination, so a scene holding both keeps walking him back to the
// counter he has already finished with. That is the loop this ticket closes, stated as a
// property rather than pinned to the one factor scenario that surfaced it.
//
// Direction-agnostic on purpose: it held for buyers by construction before LLM-553 (only a buy
// errand could settle), and this is what stops a future settle path from reintroducing the loop
// for either kind.
func TestGoldensSettledErrandNeverPressesTheTrade(t *testing.T) {
	const tradeHere = "the one keeper you came to deal with"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			actor := snap.Actors[actorID]
			if actor == nil || actor.VisitorState == nil || actor.VisitorState.Trade == nil {
				return
			}
			if !actor.VisitorState.Trade.Settled {
				return
			}
			if strings.Contains(renderScenario(sc), tradeHere) {
				t.Errorf("scenario %q: errand is settled, yet the prompt still presses %q — "+
					"a merchant told his business is done must not also be steered to trade at his counterparty",
					sc.name, tradeHere)
			}
		})
	}
}

// travelerFactorPurseScenario builds the LLM-644 purse-split situation: a
// mid-errand SELL factor at his counterparty's counter whose sales have outrun
// his trip budget — wallet holds the takings, budget holds what is left of the
// purse he arrived with. The two goldens over it pin the purse line's split and
// spent tiers, the scene half of the fix (the pay gates are the tool half): the
// model must be told the fat wallet is takings bound for home, or it loops
// offers the gates refuse.
func travelerFactorPurseScenario(wallet, budget int) func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
		const (
			factorID     = sim.ActorID("vstr-4dcbca9f")
			keeperID     = sim.ActorID("josiah")
			generalStore = sim.StructureID("general_store")
		)
		now := 780 // 13:00 — mid-afternoon, errand still live
		factor := &sim.ActorSnapshot{
			Kind:              sim.KindNPCShared,
			DisplayName:       "Daniel Holcomb the factor",
			State:             sim.StateIdle,
			Pos:               sim.TilePos{X: 94, Y: 126},
			InsideStructureID: generalStore,
			CurrentHuddleID:   "h1",
			Coins:             wallet,
			// Half the bale sold already — the wallet above is mostly its proceeds.
			Inventory: map[sim.ItemKind]int{"iron": 5, "salt": 6, "woolens": 2},
			Needs:     map[sim.NeedKey]int{},
			VisitorState: &sim.VisitorState{
				SpendBudget:       budget,
				Archetype:         "factor",
				Origin:            "Boston",
				Disposition:       "curious",
				Phase:             sim.VisitorPhaseMakingRounds,
				VisitedBusinesses: []sim.StructureID{generalStore},
				Trade: &sim.TradeErrand{
					Direction:    sim.TradeDirectionSell,
					Good:         "iron",
					Counterparty: generalStore,
					ShipmentQty:  10,
					Delivered:    5,
				},
			},
		}
		keeper := &sim.ActorSnapshot{
			Kind:               sim.KindNPCStateful,
			DisplayName:        "Josiah Thorne",
			Role:               "merchant",
			State:              sim.StateIdle,
			Pos:                sim.TilePos{X: 93, Y: 126},
			WorkStructureID:    generalStore,
			InsideStructureID:  generalStore,
			CurrentHuddleID:    "h1",
			Coins:              40,
			Needs:              map[sim.NeedKey]int{},
			BusinessownerState: &sim.BusinessownerState{Flavor: "merchant"},
		}
		snap := &sim.Snapshot{
			LocalMinuteOfDay: &now,
			DawnMinute:       360,
			DuskMinute:       1080,
			DawnDuskMinuteOK: true,
			NeedThresholds:   sim.NeedThresholds{},
			Actors:           map[sim.ActorID]*sim.ActorSnapshot{factorID: factor, keeperID: keeper},
			Structures:       map[sim.StructureID]*sim.Structure{generalStore: plainStructure(generalStore, "General Store")},
			VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
				sim.VillageObjectID(generalStore): {ID: sim.VillageObjectID(generalStore), Pos: sim.WorldPos{X: 752, Y: 1008}},
			},
			Huddles: map[sim.HuddleID]*sim.Huddle{
				"h1": {Members: map[sim.ActorID]struct{}{factorID: {}, keeperID: {}}},
			},
			ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
				"iron": {Name: "iron", DisplayLabel: "Iron", DisplayLabelSingular: "iron bar", DisplayLabelPlural: "iron bars"},
			},
		}
		return snap, factorID, nil
	}
}

func init() {
	perceptionScenarios = append(perceptionScenarios,
		perceptionScenario{
			name: "traveler_factor_takings_bound_home",
			summary: "LLM-644: a SELL factor mid-errand whose bale proceeds (wallet 131) have outrun his trip " +
				"budget (24 left of his arrival purse). The purse line renders the split — what is left to spend " +
				"on this trip vs the takings bound for home — so the model does not read the fat wallet as buying " +
				"power and loop offers the pay gates refuse. This is the scene half of the proceeds-recycling fix.",
			build: travelerFactorPurseScenario(131, 24),
		},
		perceptionScenario{
			name: "traveler_factor_buying_purse_spent",
			summary: "LLM-644: the same factor with his arrival purse fully spent (budget 0, wallet 131). The " +
				"purse line names every coin he holds as takings bound for home and his buying purse as spent, " +
				"steering him to goods-in-trade if he still wants something — never a coin offer.",
			build: travelerFactorPurseScenario(131, 0),
		},
	)
}

// TestGoldensVisitorPurseNeverShowsTakingsAsSpendable — LLM-644 cross-scenario
// invariant. Whenever the subject is a visitor whose wallet exceeds his remaining
// trip budget, the prompt must carry the takings-bound-home purse phrasing and
// must NOT state the raw wallet as a plain spendable purse figure. The plain
// figure is exactly the render that had a proceeds-flush factor looping offers
// the pay gates refuse; stated as a property so a future purse-line rewrite
// cannot quietly reintroduce it for any scenario in the matrix.
func TestGoldensVisitorPurseNeverShowsTakingsAsSpendable(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState == nil || a.Coins <= a.VisitorState.SpendBudget {
				return
			}
			prompt := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
			if !strings.Contains(prompt, "bound for home") {
				t.Errorf("scenario %q: visitor wallet %d exceeds budget %d but the prompt never names the takings as bound for home",
					sc.name, a.Coins, a.VisitorState.SpendBudget)
			}
			if strings.Contains(prompt, fmt.Sprintf("Coins in your purse: %d.", a.Coins)) {
				t.Errorf("scenario %q: prompt states the raw wallet (%d) as a plain spendable purse figure", sc.name, a.Coins)
			}
		})
	}
}
