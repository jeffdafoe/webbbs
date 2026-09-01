package sim_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/repo/mem"
)

// bake_test.go — LLM-454. Integration tests for the daytime bake occupation against a
// real (mem-repo) world: start creates the shared per-home session and occupies the
// baker WITHOUT consuming flour up front (restart-safety); a co-resident joins the
// same batch flourless; completion mints the batch to the initiator and consumes its
// flour there.

// buildBakeTestWorld seeds a home with two residents — alice (holds 2 flour) and bob
// (holds none) — both inside it, plus a deterministic 07:00 dawn / 19:00 dusk clock.
// Both are UNSCHEDULED workers. Returns a daytime "now" (16:00), three hours before dusk.
func buildBakeTestWorld(t *testing.T) (*sim.World, context.CancelFunc, time.Time) {
	t.Helper()
	repo, handles := mem.NewRepository()
	handles.ItemKinds.Seed(map[sim.ItemKind]*sim.ItemKindDef{
		"flour": {Name: "flour", Category: sim.ItemCategoryMaterial},
		"bread": {Name: "bread", Category: sim.ItemCategoryFood},
	})
	handles.Assets.Seed(map[sim.AssetID]*sim.Asset{
		"home-asset": {ID: "home-asset", Name: "Walker Residence"},
	})
	zero := 0
	handles.VillageObjects.Seed(map[sim.VillageObjectID]*sim.VillageObject{
		"home": {
			ID: "home", DisplayName: "Walker Residence", AssetID: "home-asset", CurrentState: "default",
			LoiterOffsetX: &zero, LoiterOffsetY: &zero, Pos: sim.WorldPos{X: 50, Y: 50},
		},
	})
	handles.Actors.Seed(map[sim.ActorID]*sim.Actor{
		"alice": {ID: "alice", DisplayName: "Silence Walker", LLMAgent: "silence",
			Kind: sim.KindNPCStateful, HomeStructureID: "home", InsideStructureID: "home",
			Attributes: map[string][]byte{sim.AttrWorker: nil}, // unscheduled worker (the Walker-women shape)
			Inventory:  map[sim.ItemKind]int{"flour": 2}},
		"bob": {ID: "bob", DisplayName: "Patience Walker", LLMAgent: "patience",
			Kind: sim.KindNPCStateful, HomeStructureID: "home", InsideStructureID: "home",
			Attributes: map[string][]byte{sim.AttrWorker: nil},
			Inventory:  map[sim.ItemKind]int{}},
	})
	w, err := sim.LoadWorld(context.Background(), repo)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	mustSend(t, w, func(world *sim.World) {
		world.Settings.Location = time.UTC
		world.Settings.LodgingBedtimeHour = 22
		world.Settings.DawnTime = "07:00"
		world.Settings.DuskTime = "19:00"
		world.Settings.NeedThresholds = sim.DefaultNeedThresholds()
	})
	now := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC) // daytime (before the 19:00 dusk), three hours before dusk
	return w, cancel, now
}

func homeBakeSession(t *testing.T, w *sim.World) *sim.HomeBake {
	t.Helper()
	res, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		return world.HomeBakes["home"], nil
	}})
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	hb, _ := res.(*sim.HomeBake)
	return hb
}

func bakeActivityKind(t *testing.T, w *sim.World, id sim.ActorID) sim.SourceActivityKind {
	t.Helper()
	res, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		a := world.Actors[id]
		if a == nil || a.SourceActivity == nil {
			return sim.SourceActivityKind(""), nil
		}
		return a.SourceActivity.Kind, nil
	}})
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	return res.(sim.SourceActivityKind)
}

func TestBake_StartCreatesSessionAndOccupiesWithoutConsumingFlour(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	res, err := w.Send(sim.StartOrJoinBake("alice", "I'll get the bread on", false, now))
	if err != nil {
		t.Fatalf("StartOrJoinBake: %v", err)
	}
	if sr := res.(sim.SourceActivityStartResult); !sr.Started || sr.Kind != sim.SourceActivityBake {
		t.Fatalf("start result = %+v, want started bake", sr)
	}
	if k := bakeActivityKind(t, w, "alice"); k != sim.SourceActivityBake {
		t.Errorf("alice activity = %q, want bake (occupied)", k)
	}
	hb := homeBakeSession(t, w)
	if hb == nil || hb.InitiatorID != "alice" || hb.BatchQty != sim.BakeBatchQty {
		t.Fatalf("session = %+v, want alice's batch of %d", hb, sim.BakeBatchQty)
	}
	// Flour NOT consumed at start — spent only at completion, so a restart forfeits none.
	if got := inventoryOf(t, w, "alice", "flour"); got != 2 {
		t.Errorf("flour = %d after start, want 2 (consumed only at completion)", got)
	}
}

func TestBake_JoinAttachesToSameBatchWithoutFlour(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Bob holds no flour but joins the batch alice started.
	if _, err := w.Send(sim.StartOrJoinBake("bob", "", false, now.Add(time.Minute))); err != nil {
		t.Fatalf("join (flourless): %v", err)
	}
	if k := bakeActivityKind(t, w, "bob"); k != sim.SourceActivityBake {
		t.Errorf("bob activity = %q, want bake (joined the batch)", k)
	}
	// Still ONE session, still alice's — joining minted no second batch.
	if hb := homeBakeSession(t, w); hb == nil || hb.InitiatorID != "alice" {
		t.Fatalf("session = %+v, want the single alice batch after bob joined", hb)
	}
}

func TestBake_CompletionMintsBatchToInitiatorAndConsumesFlour(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	res, err := w.Send(sim.StartOrJoinBake("alice", "", false, now))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	until := res.(sim.SourceActivityStartResult).Until
	// Drive the completion sweep past the window (bedtime).
	after := until.Add(time.Second)
	if _, err := w.Send(sim.Command{Fn: func(world *sim.World) (any, error) {
		return sim.CompleteDueSourceActivities(world, after), nil
	}}); err != nil {
		t.Fatalf("complete sweep: %v", err)
	}
	if got := inventoryOf(t, w, "alice", "bread"); got != sim.BakeBatchQty {
		t.Errorf("bread = %d, want %d (batch minted to the initiator)", got, sim.BakeBatchQty)
	}
	if got := inventoryOf(t, w, "alice", "flour"); got != 0 {
		t.Errorf("flour = %d, want 0 (consumed at completion)", got)
	}
	if hb := homeBakeSession(t, w); hb != nil {
		t.Errorf("session still present after completion: %+v", hb)
	}
}

// TestBake_RejectsWhenGateWouldNotOffer covers the LLM-454 review High: the commit
// path RE-VALIDATES the advertised gate, so a stale/forged call can't bake while
// asleep, on a scheduled shift, after dusk, or with a pressing need.
func TestBake_RejectsWhenGateWouldNotOffer(t *testing.T) {
	t.Run("asleep", func(t *testing.T) {
		w, cancel, now := buildBakeTestWorld(t)
		defer cancel()
		setActor(t, w, "alice", func(a *sim.Actor) { a.State = sim.StateSleeping })
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err == nil {
			t.Error("a sleeping actor started baking")
		}
	})
	t.Run("on shift", func(t *testing.T) {
		w, cancel, now := buildBakeTestWorld(t)
		defer cancel()
		setActor(t, w, "alice", func(a *sim.Actor) {
			s, e := 8*60, 23*60 // scheduled shift covering the 16:00 now
			a.ScheduleStartMin, a.ScheduleEndMin = &s, &e
			a.WorkStructureID = "home"
		})
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err == nil {
			t.Error("a scheduled on-shift actor started baking")
		}
	})
	t.Run("after dusk", func(t *testing.T) {
		w, cancel, _ := buildBakeTestWorld(t)
		defer cancel()
		evening := time.Date(2026, 7, 17, 20, 0, 0, 0, time.UTC) // past the 19:00 dusk
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, evening)); err == nil {
			t.Error("baking started after dusk (it's a daytime task)")
		}
	})
	t.Run("unscheduled non-worker", func(t *testing.T) {
		w, cancel, now := buildBakeTestWorld(t)
		defer cancel()
		// Strip the worker attribute: an unscheduled non-worker's home is its resting
		// state — perception never offers the bake, so the commit path must reject it too
		// (tool-cue lockstep, matching inDaytimeHomeWindow's subjectIsWorker requirement).
		setActor(t, w, "alice", func(a *sim.Actor) { a.Attributes = nil })
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err == nil {
			t.Error("an unscheduled non-worker started baking")
		}
	})
	// LLM-650: the shift need not be on NOW — one that begins before dusk would
	// hold the baker at the hearth through it (live: Lewis Walker, the wright).
	t.Run("shift starts before dusk", func(t *testing.T) {
		w, cancel, now := buildBakeTestWorld(t)
		defer cancel()
		setActor(t, w, "alice", func(a *sim.Actor) {
			s, e := 17*60, 23*60 // off shift at the 16:00 now, on shift an hour later
			a.ScheduleStartMin, a.ScheduleEndMin = &s, &e
			a.WorkStructureID = "home"
		})
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err == nil {
			t.Error("an actor whose shift starts before dusk started baking")
		}
	})
	// START-only since LLM-465: a red need bars committing the afternoon to a fresh
	// batch, but not joining one already going (TestBake_RedNeedJoinsExistingBatch).
	t.Run("red need, nothing to join", func(t *testing.T) {
		w, cancel, now := buildBakeTestWorld(t)
		defer cancel()
		setActor(t, w, "alice", func(a *sim.Actor) { a.Needs["hunger"] = sim.NeedMax })
		if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err == nil {
			t.Error("baking started with a pressing red need")
		}
	})
}

// TestBake_RedNeedJoinsExistingBatch is the sim-side mirror of the LLM-465 perception
// fix (TestBuildBakeChoiceRedNeedBlocksStartNotJoin): the substrate must accept exactly
// what buildBakeChoice advertises, or the tool-cue lockstep breaks and a cued housemate
// gets a rejection. A red need bars STARTING (covered above) but not lending a hand at
// a batch already going — joining costs no flour, mints nothing, and leaves the need
// actionable, since bakingMayMove keeps move_to for a red hunger/thirst and the reactor
// ticks him through the shelve for it. Live: Lewis Walker, red on hunger, shut out of
// his own household's bake and left loose in the kitchen for 70 minutes.
func TestBake_RedNeedJoinsExistingBatch(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("start: %v", err)
	}
	setActor(t, w, "bob", func(a *sim.Actor) { a.Needs["hunger"] = sim.NeedMax })
	if _, err := w.Send(sim.StartOrJoinBake("bob", "", false, now.Add(time.Minute))); err != nil {
		t.Fatalf("join with a red need: %v — joining is not the afternoon-long commitment "+
			"the red-need gate guards against, and perception now offers it to him", err)
	}
	if k := bakeActivityKind(t, w, "bob"); k != sim.SourceActivityBake {
		t.Errorf("bob activity = %q, want bake (a hungry housemate still lends a hand)", k)
	}
	// One session, still alice's — the hungry joiner minted no second batch.
	if hb := homeBakeSession(t, w); hb == nil || hb.InitiatorID != "alice" {
		t.Fatalf("session = %+v, want the single alice batch after the red-need join", hb)
	}
}

// TestBake_CompletionWithVanishedFlourMintsNothing covers the review Medium: a
// completion whose flour was spent out-of-band mints no bread and still concludes the
// session (never orphans).
func TestBake_CompletionWithVanishedFlourMintsNothing(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()
	res, err := w.Send(sim.StartOrJoinBake("alice", "", false, now))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	until := res.(sim.SourceActivityStartResult).Until
	// Flour spent out-of-band (an operator command) between start and completion.
	setActor(t, w, "alice", func(a *sim.Actor) { delete(a.Inventory, "flour") })
	mustSend(t, w, func(world *sim.World) { sim.CompleteDueSourceActivities(world, until.Add(time.Second)) })
	if got := inventoryOf(t, w, "alice", "bread"); got != 0 {
		t.Errorf("bread = %d, want 0 (no flour, no batch)", got)
	}
	if hb := homeBakeSession(t, w); hb != nil {
		t.Errorf("session still present after a flourless completion: %+v", hb)
	}
}

// TestBake_StaleSessionSelfHeals covers the review High: an orphaned session (its
// initiator's bake cleared by a non-completion path) must not block new bakes — the
// next attempt drops it and starts fresh.
func TestBake_StaleSessionSelfHeals(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()
	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Orphan the session: clear alice's bake activity as sleep / an operator interrupt
	// would, leaving the HomeBake behind with no live initiator.
	setActor(t, w, "alice", func(a *sim.Actor) { a.SourceActivity = nil })
	// A fresh attempt must self-heal (drop the orphan) and start, not be blocked.
	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now.Add(time.Minute))); err != nil {
		t.Fatalf("re-bake after orphan: %v", err)
	}
	if k := bakeActivityKind(t, w, "alice"); k != sim.SourceActivityBake {
		t.Errorf("alice activity = %q, want bake (self-healed)", k)
	}
	if hb := homeBakeSession(t, w); hb == nil || hb.InitiatorID != "alice" {
		t.Fatalf("session = %+v, want a fresh alice session", hb)
	}
}

// TestBake_RedTirednessBarsJoin is the LLM-465 boundary: joining opens up under the red
// needs the bake shelve leaves actionable (hunger/thirst/cold — move_to survives and the
// reactor ticks for them), but NOT under tiredness, which is deliberately excluded from
// both carve-outs. Admitting it would shelve an exhausted villager at the hearth until
// dusk with nothing to wake him and no move_to — a worse trap than the loose-in-the-
// kitchen bug the ticket fixes. The substrate must refuse exactly what perception does.
func TestBake_RedTirednessBarsJoin(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("start: %v", err)
	}
	setActor(t, w, "bob", func(a *sim.Actor) { a.Needs["tiredness"] = sim.NeedMax })
	if _, err := w.Send(sim.StartOrJoinBake("bob", "", false, now.Add(time.Minute))); err == nil {
		t.Error("an exhausted actor joined a bake — nothing would tick him through the shelve " +
			"and move_to is stripped for tiredness, so he'd be stuck at the hearth until dusk")
	}
}

// TestBake_RedNeedWithStaleSessionIsTreatedAsStart pins the ordering the LLM-465 fix
// depends on (code_review). The START-only red-need guard sits AFTER the stale-session
// self-heal, so the branch an actor lands in must be decided by whether a LIVE bake
// exists — not by whether a HomeBake row happens to be present. An orphaned session
// resolves to nil, which makes a red-need actor a starter and rejects him; if the guard
// or the self-heal ever moved relative to each other, he would instead be waved through
// as a "joiner" of a batch that isn't being baked, committing him to a hearth with no
// initiator and no bread at the end of it.
func TestBake_RedNeedWithStaleSessionIsTreatedAsStart(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()

	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Orphan the session (sleep / operator interrupt), leaving the row with no live baker.
	setActor(t, w, "alice", func(a *sim.Actor) { a.SourceActivity = nil })
	setActor(t, w, "bob", func(a *sim.Actor) { a.Needs["hunger"] = sim.NeedMax })

	if _, err := w.Send(sim.StartOrJoinBake("bob", "", false, now.Add(time.Minute))); err == nil {
		t.Error("a red-need actor joined a STALE session — the self-heal drops it to nil, so he is " +
			"starting a fresh batch, and starting is exactly what a pressing need still bars")
	}
	if k := bakeActivityKind(t, w, "bob"); k == sim.SourceActivityBake {
		t.Error("bob was committed to a bake he was rejected from")
	}
}

// TestBake_ShiftStartingAfterDuskDoesNotBar is the LLM-650 boundary: the gate keys
// on a shift that would BEGIN while the loaves are still in — a night shift starting
// after dusk leaves the day free, so the bake goes ahead.
func TestBake_ShiftStartingAfterDuskDoesNotBar(t *testing.T) {
	w, cancel, now := buildBakeTestWorld(t)
	defer cancel()
	setActor(t, w, "alice", func(a *sim.Actor) {
		s, e := 20*60, 23*60 // starts an hour after the 19:00 dusk
		a.ScheduleStartMin, a.ScheduleEndMin = &s, &e
		a.WorkStructureID = "home"
	})
	if _, err := w.Send(sim.StartOrJoinBake("alice", "", false, now)); err != nil {
		t.Fatalf("a shift starting after dusk should not bar the bake: %v", err)
	}
	if got := bakeActivityKind(t, w, "alice"); got != sim.SourceActivityBake {
		t.Errorf("activity = %q, want bake", got)
	}
}
