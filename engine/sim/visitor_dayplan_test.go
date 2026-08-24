package sim_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// visitor_dayplan_test.go — LLM-373 behavioral coverage for the traveler day-plan
// through a running world: the daytime spawn gate, spawn-seeded pack, daybreak-
// anchored departure, and the dusk turn to lodging.

func et(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

// seedDayPlanSettings forces chance=1000 (spawn roll always fires) and a
// 06:00–18:00 village day in America/New_York, so the daytime gate + daybreak
// anchoring are exercised deterministically.
func seedDayPlanSettings(t *testing.T, w *sim.World, loc *time.Location) {
	t.Helper()
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		world.Settings.VisitorMerchantTrickleChancePermille = 1000
		world.Settings.VisitorMaxConcurrent = 1 // one visitor → deterministic firstVisitor across ticks
		world.Settings.DawnTime = "06:00"
		world.Settings.DuskTime = "18:00"
		world.Settings.Location = loc
		return nil, nil
	}}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func firstVisitor(t *testing.T, w *sim.World) *sim.ActorSnapshot {
	t.Helper()
	for _, a := range w.Published().Actors {
		if a.VisitorState != nil {
			return a
		}
	}
	return nil
}

func firstVisitorID(t *testing.T, w *sim.World) sim.ActorID {
	t.Helper()
	for id, a := range w.Published().Actors {
		if a.VisitorState != nil {
			return id
		}
	}
	return ""
}

func TestVisitorSpawn_DayPlanSeeds(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	now := time.Date(2026, 7, 12, 15, 0, 0, 0, loc) // 15:00 — daytime
	res, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: now, Rand: rand.New(rand.NewSource(7))}))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tm := res.(sim.VisitorCascadeTelemetry); tm.Spawned != 1 {
		t.Fatalf("Spawned = %d, want 1 (reason=%q)", tm.Spawned, tm.SpawnSkipReason)
	}

	got := firstVisitor(t, w)
	if got == nil {
		t.Fatal("no visitor after daytime spawn")
	}
	if got.VisitorState.Phase != sim.VisitorPhaseArriving {
		t.Errorf("spawn phase = %q, want arriving", got.VisitorState.Phase)
	}
	// Pack: at least one ware seeded, and a purse in the seeded range.
	wares := 0
	for _, q := range got.Inventory {
		wares += q
	}
	if wares == 0 {
		t.Error("spawned traveler carries no wares")
	}
	if got.Coins < 30 || got.Coins > 50 {
		t.Errorf("purse = %d, want [30,50]", got.Coins)
	}
	// LLM-644: the trip budget seeds equal to the purse — what he arrived
	// carrying is exactly what he may spend here.
	if got.VisitorState.SpendBudget != got.Coins {
		t.Errorf("SpendBudget = %d, want the purse (%d)", got.VisitorState.SpendBudget, got.Coins)
	}
	// Departure anchored to the next daybreak: 2026-07-13 06:00 ET.
	wantDepart := time.Date(2026, 7, 13, 6, 0, 0, 0, loc)
	if !got.VisitorState.ExpiresAt.Equal(wantDepart) {
		t.Errorf("ExpiresAt = %v, want next daybreak %v", got.VisitorState.ExpiresAt, wantDepart)
	}
}

func TestVisitorSpawn_SkippedAtNight(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	night := time.Date(2026, 7, 12, 22, 0, 0, 0, loc) // 22:00 — after dusk
	res, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: night, Rand: rand.New(rand.NewSource(7))}))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	tm := res.(sim.VisitorCascadeTelemetry)
	if tm.Spawned != 0 {
		t.Errorf("Spawned = %d at night, want 0 (daytime gate)", tm.Spawned)
	}
	if firstVisitor(t, w) != nil {
		t.Error("a visitor spawned at night despite the daytime gate")
	}
}

// seedVisitorSprites seeds one npc_sprite per distinct name a visitor may render
// with, keyed by a synthetic id — mirroring the live catalog (id = uuid, Name =
// display name) so visitorSpriteID resolves whatever the spawn rolls. Sourced
// from VisitorSpriteCatalogNames, which covers the merchant (factor + buyer)
// sheets as well as the passer-through archetypes: seeding only the archetype map
// left a merchant roll resolving no sprite, which a sprite assertion would read as
// a spawn bug rather than an under-seeded fixture. Call before load().
func (vw *visitorWorld) seedVisitorSprites(t *testing.T) {
	t.Helper()
	sprites := map[sim.SpriteID]*sim.Sprite{}
	for _, name := range sim.VisitorSpriteCatalogNames() {
		id := sim.SpriteID("sprite-" + name) // unique per name; stands in for the uuid PK
		sprites[id] = &sim.Sprite{ID: id, Name: name}
	}
	vw.handles.Sprites.Seed(sprites)
}

// TestVisitorSpawn_SetsSprite — LLM-379: a spawned traveler carries a non-empty
// SpriteID resolved from its archetype (and a Facing), so the client draws it
// instead of nothing.
func TestVisitorSpawn_SetsSprite(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	vw.seedVisitorSprites(t)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	now := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	if _, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: now, Rand: rand.New(rand.NewSource(7))})); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := firstVisitor(t, w)
	if got == nil {
		t.Fatal("no visitor after daytime spawn")
	}
	if got.SpriteID == "" {
		t.Fatalf("spawned traveler has empty SpriteID — renders invisible (archetype=%q)", got.VisitorState.Archetype)
	}
	// The resolved sprite is the one mapped for its archetype.
	wantName := sim.VisitorArchetypeSprite[got.VisitorState.Archetype]
	if want := sim.SpriteID("sprite-" + wantName); got.SpriteID != want {
		t.Errorf("SpriteID = %q, want %q (archetype %q → %q)", got.SpriteID, want, got.VisitorState.Archetype, wantName)
	}
	if got.Facing == "" {
		t.Error("spawned traveler has empty Facing")
	}
}

// TestVisitorSpawn_MissingSpriteCatalog — a spawn with no sprite for the archetype
// (empty catalog) logs and ships the traveler spriteless rather than crashing: a
// missing sheet must never be fatal to the spawn.
func TestVisitorSpawn_MissingSpriteCatalog(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	// No seedVisitorSprites — the catalog is empty.
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	now := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	res, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: now, Rand: rand.New(rand.NewSource(7))}))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if tm := res.(sim.VisitorCascadeTelemetry); tm.Spawned != 1 {
		t.Fatalf("Spawned = %d, want 1 despite missing sprite (reason=%q)", tm.Spawned, tm.SpawnSkipReason)
	}
	got := firstVisitor(t, w)
	if got == nil {
		t.Fatal("visitor was dropped when its sprite was missing; want spawned spriteless")
	}
	if got.SpriteID != "" {
		t.Errorf("SpriteID = %q, want empty (no catalog seeded)", got.SpriteID)
	}
}

// seedBusiness places a shop (asset + VillageObject + Structure) with a present
// keeper inside it — the minimum for keeperPresentAt(shop) to read true, so the
// circuit routes a traveler there. Call before load().
func (vw *visitorWorld) seedBusiness(t *testing.T, id sim.StructureID, name string, pos sim.WorldPos) {
	t.Helper()
	assetID := sim.AssetID(string(id) + "-asset")
	vw.handles.Assets.Seed(map[sim.AssetID]*sim.Asset{
		assetID: {ID: assetID, Category: "structure", DoorOffsetX: intpV(1), DoorOffsetY: intpV(2)},
	})
	vw.handles.VillageObjects.Seed(map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(id): {
			ID: sim.VillageObjectID(id), AssetID: assetID, Pos: pos, EntryPolicy: sim.EntryPolicyOpen,
			Tags: []string{sim.TagBusiness},
		},
	})
	vw.handles.Structures.Seed(map[sim.StructureID]*sim.Structure{
		id: {ID: id, DisplayName: name},
	})
	keeperID := sim.ActorID("keeper-" + string(id))
	vw.handles.Actors.Seed(map[sim.ActorID]*sim.Actor{
		keeperID: {
			ID:                 keeperID,
			DisplayName:        name + " Keeper",
			Kind:               sim.KindNPCStateful,
			State:              sim.StateIdle,
			WorkStructureID:    id,
			InsideStructureID:  id,
			Pos:                pos.Tile(),
			Needs:              map[sim.NeedKey]int{},
			Inventory:          map[sim.ItemKind]int{},
			BusinessownerState: &sim.BusinessownerState{Flavor: "smith"},
		},
	})
}

// setVisitorState mutates the single in-flight visitor's actor state on the world
// goroutine — used to simulate an arrival (or a failed arrival) without driving the
// real multi-tick walk.
func setVisitorState(t *testing.T, w *sim.World, mutate func(a *sim.Actor)) {
	t.Helper()
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		for _, a := range world.Actors {
			if a.VisitorState != nil {
				mutate(a)
			}
		}
		return nil, nil
	}}); err != nil {
		t.Fatalf("mutate visitor: %v", err)
	}
}

func tickCircuit(t *testing.T, w *sim.World, now time.Time) {
	t.Helper()
	if _, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: now, Rand: rand.New(rand.NewSource(7))})); err != nil {
		t.Fatalf("circuit tick: %v", err)
	}
}

// TestVisitorSpawn_EngineDoesNotPickShop — LLM-379: the engine no longer chooses the
// traveler's stops. With a shop open, a spawned visitor is walked to the neutral village
// anchor (the tavern), never the shop, and the engine marks nothing visited — he chooses
// his own rounds with move_to.
func TestVisitorSpawn_EngineDoesNotPickShop(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	tavern := vw.seedTavern(t)
	const smithy sim.StructureID = "smithy"
	vw.seedBusiness(t, smithy, "Blacksmith", sim.WorldPos{X: 288, Y: 320})
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	tickCircuit(t, w, time.Date(2026, 7, 12, 15, 0, 0, 0, loc))
	got := firstVisitor(t, w)
	if got == nil {
		t.Fatal("no visitor spawned")
	}
	// The engine's one walk is to the anchor, never the shop.
	if got.MoveDestStructureID == smithy {
		t.Errorf("engine routed the visitor to the shop %q; it must not pick his stops", smithy)
	}
	if got.MoveDestStructureID != tavern {
		t.Errorf("spawn move dest = %q, want the neutral anchor %q", got.MoveDestStructureID, tavern)
	}
	if len(got.VisitorState.VisitedBusinesses) != 0 {
		t.Errorf("engine marked %v visited at spawn; recording is arrival-driven only", got.VisitorState.VisitedBusinesses)
	}
}

// TestRecordVisitorArrival — LLM-379: VisitedBusinesses is written only on a genuine
// co-present arrival at a keeper-business, never for a shut shop, the inn, or an evening
// (lodging) arrival. This is the sole writer now that the engine picks no destinations.
func TestRecordVisitorArrival(t *testing.T) {
	loc := et(t)
	day := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	const smithy sim.StructureID = "smithy"

	newWorld := func(t *testing.T) (*sim.World, func(), sim.ActorID) {
		vw := newVisitorWorld()
		vw.seedTavern(t)
		vw.seedBusiness(t, smithy, "Blacksmith", sim.WorldPos{X: 288, Y: 320})
		w, cancel := vw.load(t)
		seedDayPlanSettings(t, w, loc)
		tickCircuit(t, w, day) // spawn
		id := firstVisitorID(t, w)
		if id == "" {
			cancel()
			t.Fatal("no visitor spawned")
		}
		return w, cancel, id
	}
	record := func(t *testing.T, w *sim.World, id sim.ActorID, sid sim.StructureID) {
		if _, err := w.Send(sim.RecordVisitorArrival(id, sid)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// atSmithy puts the visitor co-present inside the smithy — the location the recording
	// gate now requires (a shop is marked only when he is actually there).
	atSmithy := func(t *testing.T, w *sim.World) {
		setVisitorState(t, w, func(a *sim.Actor) { a.InsideStructureID = smithy })
	}

	t.Run("co-present keeper-shop is recorded", func(t *testing.T) {
		w, cancel, id := newWorld(t)
		defer cancel()
		atSmithy(t, w)
		record(t, w, id, smithy)
		if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 1 || visitedOf(got)[0] != smithy {
			t.Fatalf("visited=%v, want [smithy]", visitedOf(got))
		}
	})

	t.Run("not co-present (elsewhere) is not recorded", func(t *testing.T) {
		w, cancel, id := newWorld(t)
		defer cancel()
		// The visitor is still out by the anchor, NOT at the smithy; a stale / misrouted
		// arrival (or a stray direct call) must record nothing even though the keeper is
		// present — else a shop is "visited" the traveler never reached.
		record(t, w, id, smithy)
		if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
			t.Fatalf("visited=%v, want none (visitor isn't at the shop)", visitedOf(got))
		}
	})

	t.Run("shut shop (keeper away) is not recorded", func(t *testing.T) {
		w, cancel, id := newWorld(t)
		defer cancel()
		atSmithy(t, w)
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			for _, a := range world.Actors {
				if a.WorkStructureID == smithy {
					a.InsideStructureID = ""
					a.Pos = sim.TilePos{X: sim.PadX + 1, Y: sim.PadY + 1} // keeper wandered far off
				}
			}
			return nil, nil
		}}); err != nil {
			t.Fatal(err)
		}
		record(t, w, id, smithy)
		if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
			t.Fatalf("visited=%v, want none (shut shop)", visitedOf(got))
		}
	})

	t.Run("evening arrival is lodging, not a round", func(t *testing.T) {
		w, cancel, id := newWorld(t)
		defer cancel()
		atSmithy(t, w)
		setVisitorState(t, w, func(a *sim.Actor) { a.VisitorState.Phase = sim.VisitorPhaseLodging })
		record(t, w, id, smithy)
		if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
			t.Fatalf("visited=%v, want none (evening — lodging, not rounds)", visitedOf(got))
		}
	})
}

// seedPost seeds a NON-business structure with somebody stationed in it — the Meeting
// House and its constable (LLM-554). Everything the recording gate's keeper check looks
// at is identical to seedBusiness: an actor whose WorkStructureID is this structure,
// awake, standing inside it. The two differences are the ones that matter — the object
// carries no TagBusiness, and the man at his post keeps no shop, so no
// BusinessownerState.
func (vw *visitorWorld) seedPost(t *testing.T, id sim.StructureID, name string, pos sim.WorldPos) {
	t.Helper()
	assetID := sim.AssetID(string(id) + "-asset")
	vw.handles.Assets.Seed(map[sim.AssetID]*sim.Asset{
		assetID: {ID: assetID, Category: "structure", DoorOffsetX: intpV(1), DoorOffsetY: intpV(2)},
	})
	vw.handles.VillageObjects.Seed(map[sim.VillageObjectID]*sim.VillageObject{
		sim.VillageObjectID(id): {
			ID: sim.VillageObjectID(id), AssetID: assetID, Pos: pos, EntryPolicy: sim.EntryPolicyOpen,
			Tags: []string{"meeting-house"},
		},
	})
	vw.handles.Structures.Seed(map[sim.StructureID]*sim.Structure{
		id: {ID: id, DisplayName: name},
	})
	constableID := sim.ActorID("constable-" + string(id))
	vw.handles.Actors.Seed(map[sim.ActorID]*sim.Actor{
		constableID: {
			ID:                constableID,
			DisplayName:       "Constable Gideon Marsh",
			Kind:              sim.KindNPCStateful,
			State:             sim.StateIdle,
			WorkStructureID:   id,
			InsideStructureID: id,
			Pos:               pos.Tile(),
			Needs:             map[sim.NeedKey]int{},
			Inventory:         map[sim.ItemKind]int{},
		},
	})
}

// TestRecordVisitorArrivalSkipsNonBusiness — LLM-554: a round is a call on a place of
// BUSINESS. The constable standing his post satisfies every gate the recording side had
// (on rounds, not lodging, someone whose workplace this is awake and here), so a factor
// who walked into the Meeting House had it written into his called-at list and it kept
// rendering back at him for the rest of his stay.
//
// Both halves run against ONE world, so the smithy arm is the control: if it stopped
// recording too, the tag gate is not what excluded the meeting house.
func TestRecordVisitorArrivalSkipsNonBusiness(t *testing.T) {
	loc := et(t)
	const (
		smithy       sim.StructureID = "smithy"
		meetingHouse sim.StructureID = "meeting_house"
	)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	vw.seedBusiness(t, smithy, "Blacksmith", sim.WorldPos{X: 288, Y: 320})
	vw.seedPost(t, meetingHouse, "Meeting House", sim.WorldPos{X: 352, Y: 320})
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)
	tickCircuit(t, w, time.Date(2026, 7, 12, 15, 0, 0, 0, loc))
	id := firstVisitorID(t, w)
	if id == "" {
		t.Fatal("no visitor spawned")
	}

	// He walks into the meeting house and passes the time of day with the constable.
	setVisitorState(t, w, func(a *sim.Actor) { a.InsideStructureID = meetingHouse })
	if _, err := w.Send(sim.RecordVisitorArrival(id, meetingHouse)); err != nil {
		t.Fatalf("record meeting house: %v", err)
	}
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
		t.Fatalf("visited=%v, want none — a meeting house is not a business he called at (LLM-554)", visitedOf(got))
	}

	// Control: the smithy differs by its business tag (and a keeper who keeps shop), and
	// must still record.
	setVisitorState(t, w, func(a *sim.Actor) { a.InsideStructureID = smithy })
	if _, err := w.Send(sim.RecordVisitorArrival(id, smithy)); err != nil {
		t.Fatalf("record smithy: %v", err)
	}
	got := firstVisitor(t, w)
	if got == nil || len(visitedOf(got)) != 1 || visitedOf(got)[0] != smithy {
		t.Fatalf("visited=%v, want [smithy] — the control arm must still record, or the gate above proves nothing", visitedOf(got))
	}

	// Inverse: tag that same meeting house a business and nothing else changes — same
	// constable, same post, same arrival — and it records. The tag is the discriminator,
	// not anything about the man standing in it (code_review).
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		vobj := world.VillageObjects[sim.VillageObjectID(meetingHouse)]
		vobj.Tags = append(vobj.Tags, sim.TagBusiness)
		return nil, nil
	}}); err != nil {
		t.Fatal(err)
	}
	setVisitorState(t, w, func(a *sim.Actor) { a.InsideStructureID = meetingHouse })
	if _, err := w.Send(sim.RecordVisitorArrival(id, meetingHouse)); err != nil {
		t.Fatalf("record tagged meeting house: %v", err)
	}
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 2 {
		t.Fatalf("visited=%v, want smithy AND the now-tagged meeting house — the exclusion above is not the tag gate", visitedOf(got))
	}
}

// TestVisitorPacingRecordsCallAfterKeeperReturns — LLM-575: the call is recorded
// whichever of the two parties got there last. The arrival event samples "is a keeper
// tending here" at a moment the OTHER man chooses: Tobias Hewes the nail-buyer walked
// into the Blacksmith on 2026-07-30 while Ezekiel Crane was at the wood pile, bought
// nine nails off him half a minute later, and was told for the next half hour that he
// had called at the General Store, the apothecary and Ellis Farm but never the smithy —
// so he told the apothecary he had got his nails at the General Store. The pacing pass
// re-reads the world, so the stop lands on the next tick instead of never.
//
// The keeper-away arm is the control: if it recorded, the re-check below would prove
// nothing about ordering.
func TestVisitorPacingRecordsCallAfterKeeperReturns(t *testing.T) {
	loc := et(t)
	day := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	const smithy sim.StructureID = "smithy"
	smithyPos := sim.WorldPos{X: 288, Y: 320}

	vw := newVisitorWorld()
	vw.seedTavern(t)
	vw.seedBusiness(t, smithy, "Blacksmith", smithyPos)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)
	tickCircuit(t, w, day) // spawn
	id := firstVisitorID(t, w)
	if id == "" {
		t.Fatal("no visitor spawned")
	}

	// The keeper steps out to his wood pile.
	moveKeeper := func(t *testing.T, inside sim.StructureID, pos sim.TilePos) {
		t.Helper()
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			for _, a := range world.Actors {
				if a.WorkStructureID == smithy {
					a.InsideStructureID = inside
					a.Pos = pos
				}
			}
			return nil, nil
		}}); err != nil {
			t.Fatalf("move keeper: %v", err)
		}
	}
	moveKeeper(t, "", sim.TilePos{X: sim.PadX + 1, Y: sim.PadY + 1})

	// He walks in and comes to rest at the forge while the smith is away. The arrival
	// fires and finds nobody to call on.
	setVisitorState(t, w, func(a *sim.Actor) {
		a.InsideStructureID = smithy
		a.MoveIntent = nil
	})
	if _, err := w.Send(sim.RecordVisitorArrival(id, smithy)); err != nil {
		t.Fatalf("record arrival: %v", err)
	}
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
		t.Fatalf("visited=%v, want none — the smith is away, so there was nobody to call on", visitedOf(got))
	}

	// The smith comes back. Nothing fires for the TRAVELER — no second arrival of his own
	// — which is exactly why the stop used to be lost for good.
	moveKeeper(t, smithy, smithyPos.Tile())
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
		t.Fatalf("visited=%v, want still none — the keeper's own return records nothing", visitedOf(got))
	}

	tickCircuit(t, w, day.Add(time.Minute))
	got := firstVisitor(t, w)
	if got == nil || len(visitedOf(got)) != 1 || visitedOf(got)[0] != smithy {
		t.Fatalf("visited=%v, want [smithy] — the pacing pass must count the stop he is standing in", visitedOf(got))
	}

	// Idempotent: standing there for a second tick does not double-count him.
	tickCircuit(t, w, day.Add(2*time.Minute))
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 1 {
		t.Fatalf("visited=%v, want [smithy] once — re-sampling must not append a duplicate", visitedOf(got))
	}
}

// TestVisitorPacingDefersCallWhileWalking — LLM-575: passing THROUGH a shop on the way
// somewhere else is not calling at it. The re-check is a re-sample of an arrival, not a
// new "anywhere he happens to be" rule, so a traveler with a walk in flight is left to
// the arrival path that owns the moment he comes to rest.
//
// Both halves matter. The skip must DEFER, not drop: a traveler who stops where he
// stands has to be recorded on the next pass rather than left waiting on an arrival
// event that has already been and gone (code_review). The second half clears the intent
// directly — it pins the deferred re-check, NOT that finishArrival is what clears it.
func TestVisitorPacingDefersCallWhileWalking(t *testing.T) {
	loc := et(t)
	day := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	const smithy sim.StructureID = "smithy"
	smithyPos := sim.WorldPos{X: 288, Y: 320}

	vw := newVisitorWorld()
	tavern := vw.seedTavern(t)
	vw.seedBusiness(t, smithy, "Blacksmith", smithyPos)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)
	tickCircuit(t, w, day) // spawn — and the engine's one walk, to the tavern anchor
	if id := firstVisitorID(t, w); id == "" {
		t.Fatal("no visitor spawned")
	}

	// Mid-stride across the smithy's floor, still bound for the tavern.
	setVisitorState(t, w, func(a *sim.Actor) {
		a.InsideStructureID = smithy
		if a.MoveIntent == nil {
			t.Fatalf("visitor has no walk in flight; the spawn walk to %q is what this test rides on", tavern)
		}
	})

	tickCircuit(t, w, day.Add(time.Minute))
	if got := firstVisitor(t, w); got == nil || len(visitedOf(got)) != 0 {
		t.Fatalf("visited=%v, want none — he is walking through, not calling in", visitedOf(got))
	}

	// He thinks better of the tavern and stops where he is. The stop was deferred, not
	// lost.
	setVisitorState(t, w, func(a *sim.Actor) { a.MoveIntent = nil })
	tickCircuit(t, w, day.Add(2*time.Minute))
	got := firstVisitor(t, w)
	if got == nil || len(visitedOf(got)) != 1 || visitedOf(got)[0] != smithy {
		t.Fatalf("visited=%v, want [smithy] — the mid-walk skip must defer the call, not drop it", visitedOf(got))
	}
}

func visitedOf(a *sim.ActorSnapshot) []sim.StructureID {
	if a == nil || a.VisitorState == nil {
		return nil
	}
	return a.VisitorState.VisitedBusinesses
}

func TestVisitorCircuit_DuskTurnsToLodging(t *testing.T) {
	loc := et(t)
	vw := newVisitorWorld()
	vw.seedTavern(t)
	w, cancel := vw.load(t)
	defer cancel()
	seedDayPlanSettings(t, w, loc)

	// Spawn in the afternoon.
	day := time.Date(2026, 7, 12, 15, 0, 0, 0, loc)
	if _, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: day, Rand: rand.New(rand.NewSource(7))})); err != nil {
		t.Fatalf("spawn tick: %v", err)
	}
	if got := firstVisitor(t, w); got == nil || got.VisitorState.Phase != sim.VisitorPhaseArriving {
		t.Fatalf("post-spawn phase = %v, want arriving", got)
	}

	// A later tick, now past dusk (no more spawn, so chance stays high but the visitor
	// is at cap): the circuit turns the in-flight traveler to the lodging phase.
	evening := time.Date(2026, 7, 12, 19, 30, 0, 0, loc)
	if _, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{Now: evening, Rand: rand.New(rand.NewSource(7))})); err != nil {
		t.Fatalf("evening tick: %v", err)
	}
	got := firstVisitor(t, w)
	if got == nil {
		t.Fatal("visitor vanished")
	}
	if got.VisitorState.Phase != sim.VisitorPhaseLodging {
		t.Errorf("evening phase = %q, want lodging", got.VisitorState.Phase)
	}
}

// TestSellErrandSettlesOnDelivery — LLM-553, the end-to-end guard. A factor who has handed his
// shipment over has his errand settled by the pacing tick, which is what turns "## Your rounds"
// from the trade-here steer to the wind-down. Before this, `Settled` was reachable only for a
// BUY errand, so a factor was steered back at his counterparty from arrival to dusk while
// commerce confinement made every other stop talk-only — he bid the keeper farewell and came
// straight back, for hours.
//
// Delivery is driven through the REAL goods-transfer path rather than by setting the counter,
// so the test covers the accounting and the settle together. The three cases are the ones that
// distinguish the honest measure from the naive one:
//
//   - sold the bale                  -> settles
//   - sold nothing                   -> must NOT settle (else the wind-down fires on arrival
//     and he never trades at all)
//   - sold the bale, bought some back -> settles anyway; this is the case a test on current
//     holdings gets wrong, and it is what the live factor actually did when he swapped a
//     locket for a bar of iron
func TestSellErrandSettlesOnDelivery(t *testing.T) {
	const (
		factorID = sim.ActorID("vstr-deadbeef")
		keeperID = sim.ActorID("josiah")
		iron     = sim.ItemKind("iron")
		shipment = 10
	)

	// settledAfterSelling seeds a factor carrying the whole shipment, moves `sold` bars to the
	// keeper and `boughtBack` bars back, then runs one pacing tick and reports the errand.
	settledAfterSelling := func(t *testing.T, sold, boughtBack int) bool {
		t.Helper()
		loc := et(t)
		vw := newVisitorWorld()
		vw.seedTavern(t)
		vw.seedVisitorSprites(t)
		w, cancel := vw.load(t)
		defer cancel()
		seedDayPlanSettings(t, w, loc)
		// Spawning off — the seeded factor is the subject, and a second visitor would make
		// the assertions ambiguous.
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			world.Settings.VisitorMerchantTrickleChancePermille = 0
			return nil, nil
		}}); err != nil {
			t.Fatalf("disable spawn: %v", err)
		}

		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			world.Actors[keeperID] = &sim.Actor{
				ID: keeperID, DisplayName: "Josiah Thorne", Kind: sim.KindNPCStateful,
				// He must WORK at the errand counterparty: delivery is credited only when
				// the shipment reaches that business, not merely when it leaves the factor.
				WorkStructureID: "tavern",
				Pos:             sim.TilePos{X: sim.PadX + 5, Y: sim.PadY + 4},
				Needs:           sim.SeedVisitorNeedsForTest(),
				Inventory:       map[sim.ItemKind]int{},
			}
			world.Actors[factorID] = &sim.Actor{
				ID:          factorID,
				DisplayName: "Daniel Holcomb the factor",
				Kind:        sim.KindNPCShared,
				LLMAgent:    sim.VisitorAgentName,
				Pos:         sim.TilePos{X: sim.PadX + 4, Y: sim.PadY + 4},
				Needs:       sim.SeedVisitorNeedsForTest(),
				Inventory:   map[sim.ItemKind]int{iron: shipment},
				VisitorState: &sim.VisitorState{
					Archetype: "factor",
					Phase:     sim.VisitorPhaseMakingRounds,
					ExpiresAt: time.Now().Add(6 * time.Hour),
					Trade: &sim.TradeErrand{
						Direction:    sim.TradeDirectionSell,
						Good:         iron,
						Counterparty: "tavern",
						ShipmentQty:  shipment,
					},
				},
			}
			sim.RebuildIndicesForTest(world)
			return nil, nil
		}}); err != nil {
			t.Fatalf("seed actors: %v", err)
		}

		if sold > 0 {
			if _, err := w.Send(sim.TransferItemForTest(factorID, keeperID, iron, sold)); err != nil {
				t.Fatalf("factor sells %d iron: %v", sold, err)
			}
		}
		if boughtBack > 0 {
			if _, err := w.Send(sim.TransferItemForTest(keeperID, factorID, iron, boughtBack)); err != nil {
				t.Fatalf("factor buys %d iron back: %v", boughtBack, err)
			}
		}

		// Midday in the village day seeded above, so the dusk phase flip cannot be what ends
		// his rounds — the settle has to come from the delivery accounting itself.
		noon := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC) // 12:00 America/New_York
		if _, err := w.Send(sim.TickVisitorCascade(sim.VisitorTickInputs{
			Now: noon, Rand: rand.New(rand.NewSource(3)),
		})); err != nil {
			t.Fatalf("TickVisitorCascade: %v", err)
		}

		var settled bool
		if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
			actor := world.Actors[factorID]
			if actor == nil || actor.VisitorState == nil || actor.VisitorState.Trade == nil {
				t.Fatal("seeded factor lost his errand")
				return nil, nil
			}
			if actor.VisitorState.Phase == sim.VisitorPhaseLodging {
				t.Fatal("the factor flipped to lodging — the dusk path ran, so this test proves nothing")
			}
			settled = actor.VisitorState.Trade.Settled
			return nil, nil
		}}); err != nil {
			t.Fatalf("read errand: %v", err)
		}
		return settled
	}

	t.Run("shipment delivered settles the errand", func(t *testing.T) {
		if !settledAfterSelling(t, shipment, 0) {
			t.Error("a factor who handed over his whole shipment is still unsettled — the rounds cue keeps steering him back to his counterparty all afternoon")
		}
	})
	t.Run("a full bale does not settle", func(t *testing.T) {
		if settledAfterSelling(t, 0, 0) {
			t.Error("a factor who has sold nothing settled — the wind-down would fire on arrival and he would never trade")
		}
	})
	t.Run("buying stock back does not un-deliver the shipment", func(t *testing.T) {
		if !settledAfterSelling(t, shipment, 3) {
			t.Error("a factor who landed his bale and then bought three bars back is unsettled — current holdings cannot tell him from a man who sold seven, which is why the errand counts what it delivered")
		}
	})
}
