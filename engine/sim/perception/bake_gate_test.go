package perception

import (
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// bake_gate_test.go — LLM-454. The daytime bake affordance gate (buildBakeChoice) and
// its cue (renderBakeChoice). Reuses the evening_leisure_test dawn/dusk snapshot (07:00
// dawn / 19:00 dusk) with a homed UNSCHEDULED day-worker — the Walker-women shape — in
// the at-home daytime window (before dusk).

// homeBaker is a homed UNSCHEDULED day-worker (no schedule, carries the worker attribute)
// standing at `inside`, carrying `flour`. This is the looping-homebody shape the daytime
// bake targets — distinct from the SCHEDULED eveningWorker, which is on shift all day.
func homeBaker(inside sim.StructureID, flour int) *sim.ActorSnapshot {
	a := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		AttributeSlugs:    []string{sim.AttrWorker}, // unscheduled worker — day-active, no post obligation
		HomeStructureID:   "cottage",
		InsideStructureID: inside,
		Needs:             map[sim.NeedKey]int{},
	}
	if flour > 0 {
		a.Inventory = map[sim.ItemKind]int{sim.BakeFlourItem: flour}
	}
	return a
}

func TestBuildBakeChoice(t *testing.T) {
	const daytime = 16 * 60 // 16:00 — inside [dawn 07:00, dusk 19:00), 3h before dusk

	if v := buildBakeChoice(eveningSnap(daytime), homeBaker("cottage", 2)); v == nil || v.Joining {
		t.Errorf("at-home daytime with flour: got %+v, want START (non-nil, Joining=false)", v)
	}
	if v := buildBakeChoice(eveningSnap(daytime), homeBaker("tavern", 2)); v != nil {
		t.Errorf("away from home: got %+v, want nil", v)
	}
	if v := buildBakeChoice(eveningSnap(20*60), homeBaker("cottage", 2)); v != nil {
		t.Errorf("after dusk (20:00): got %+v, want nil (baking is a daytime task)", v)
	}
	if v := buildBakeChoice(eveningSnap(18*60+45), homeBaker("cottage", 2)); v != nil {
		t.Errorf("too close to dusk (< 30m left): got %+v, want nil", v)
	}
	if v := buildBakeChoice(eveningSnap(daytime), homeBaker("cottage", 0)); v != nil {
		t.Errorf("no flour and no bake going: got %+v, want nil", v)
	}
	// A SCHEDULED actor on its shift belongs at its post, not the hearth — even at home.
	sched := eveningWorker("cottage") // scheduled 07:00–19:00, on shift at 16:00
	sched.Inventory = map[sim.ItemKind]int{sim.BakeFlourItem: 2}
	if v := buildBakeChoice(eveningSnap(daytime), sched); v != nil {
		t.Errorf("scheduled worker on its shift: got %+v, want nil (belongs at post)", v)
	}
	// A household bake already going here → a flourless resident JOINS it.
	snap := eveningSnap(daytime)
	snap.HomeBakesActive = map[sim.StructureID]bool{"cottage": true}
	if v := buildBakeChoice(snap, homeBaker("cottage", 0)); v == nil || !v.Joining {
		t.Errorf("flourless with a bake going: got %+v, want JOIN (non-nil, Joining=true)", v)
	}
}

// TestBuildBakeChoiceRedNeedBlocksStartNotJoin is the LLM-465 case: a pressing (red)
// need bars STARTING a bake but not lending a hand at one already going. Starting is a
// whole afternoon's commitment and a starving villager should see to that first; joining
// costs no flour, mints no batch, and leaves the need fully actionable — gateTools'
// bakingMayMove keeps move_to for a red hunger/thirst, and the reactor's
// hasBreakInterruptingNeedWarrant ticks him through the shelve for it. Live 2026-07-18:
// Lewis Walker was red on hunger while Anne and Patience baked, so the pre-fix gate gave
// him no bake affordance at all, left him unshelved in his own kitchen, and he burned 24
// turns in 70 minutes asking how the loaves were coming — arming bakeReplyDue for BOTH
// bakers with every question.
func TestBuildBakeChoiceRedNeedBlocksStartNotJoin(t *testing.T) {
	const daytime = 16 * 60 // 16:00 — inside [dawn 07:00, dusk 19:00), 3h before dusk

	// Red on hunger at the default threshold (18 — the live line Lewis was over).
	hungry := func(flour int) *sim.ActorSnapshot {
		a := homeBaker("cottage", flour)
		a.Needs = map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold}
		return a
	}

	// Positive control: the SAME actor with no pressing need does get the start cue,
	// so the negative assertion below can't pass because bake broke outright.
	if v := buildBakeChoice(eveningSnap(daytime), homeBaker("cottage", 2)); v == nil || v.Joining {
		t.Fatal("unpressed resident with flour: got no START cue — the control for the red-need " +
			"assertions below is broken, so they prove nothing")
	}

	// Nothing going to join: starting is an afternoon's commitment, so the need wins.
	if v := buildBakeChoice(eveningSnap(daytime), hungry(2)); v != nil {
		t.Errorf("red-need resident with flour and no bake going: got %+v, want nil — starting a "+
			"to-dusk bake does not outrank a pressing need", v)
	}

	// A household bake already going here: the join stays open to him.
	snap := eveningSnap(daytime)
	snap.HomeBakesActive = map[sim.StructureID]bool{"cottage": true}
	if v := buildBakeChoice(snap, hungry(0)); v == nil || !v.Joining {
		t.Errorf("red-need resident with a bake going: got %+v, want JOIN (non-nil, Joining=true) — "+
			"lending a hand costs nothing and he keeps move_to for the need, so refusing him the "+
			"join protects him from nothing and leaves him loose and fully tickable (LLM-465)", v)
	}
	// Holding flour changes nothing while a batch is going — he joins it rather than
	// starting a second one, so the red-need branch is never reached.
	if v := buildBakeChoice(snap, hungry(4)); v == nil || !v.Joining {
		t.Errorf("red-need resident holding flour with a bake going: got %+v, want JOIN", v)
	}

	// THIRST behaves identically to hunger — the guarantee is stated over every need the
	// shelve leaves actionable, so asserting only hunger would let an asymmetric
	// need-gating regression through (code_review).
	thirsty := homeBaker("cottage", 0)
	thirsty.Needs = map[sim.NeedKey]int{"thirst": sim.DefaultThirstRedThreshold}
	if v := buildBakeChoice(snap, thirsty); v == nil || !v.Joining {
		t.Errorf("red-THIRST resident with a bake going: got %+v, want JOIN — move_to survives for "+
			"thirst exactly as it does for hunger, so the join is equally safe", v)
	}
	thirstyNoBake := homeBaker("cottage", 2)
	thirstyNoBake.Needs = map[sim.NeedKey]int{"thirst": sim.DefaultThirstRedThreshold}
	if v := buildBakeChoice(eveningSnap(daytime), thirstyNoBake); v != nil {
		t.Errorf("red-THIRST resident with flour and no bake going: got %+v, want nil — starting "+
			"is still barred by any pressing need", v)
	}

	// TIREDNESS is the one red need that does NOT open the join, because it is
	// excluded from both carve-outs that make joining safe: bakingMayMove keeps
	// move_to for hunger/thirst/cold but not tiredness, and the reactor does not tick
	// a shelved actor for a red-tiredness warrant. An exhausted joiner would sit at
	// the hearth until dusk with no way out — a worse trap than the loose-in-the-
	// kitchen bug this ticket fixes, so the guard must not widen past the carve-outs.
	exhausted := homeBaker("cottage", 0)
	exhausted.Needs = map[sim.NeedKey]int{"tiredness": sim.DefaultTirednessRedThreshold}
	if v := buildBakeChoice(snap, exhausted); v != nil {
		t.Errorf("red-TIRED resident with a bake going: got %+v, want nil — nothing ticks an "+
			"exhausted baker and move_to is stripped for tiredness, so this join is a trap "+
			"until dusk, not a free hand at the bread (LLM-465)", v)
	}
}

func TestRenderBakeChoice(t *testing.T) {
	var b strings.Builder
	renderBakeChoice(&b, &BakeChoiceView{Joining: false})
	if !strings.Contains(b.String(), "Call bake to start") {
		t.Errorf("start cue missing the bake tool: %q", b.String())
	}
	b.Reset()
	renderBakeChoice(&b, &BakeChoiceView{Joining: true})
	if !strings.Contains(b.String(), "Call bake to join") {
		t.Errorf("join cue missing the bake tool: %q", b.String())
	}
	b.Reset()
	renderBakeChoice(&b, nil)
	if b.String() != "" {
		t.Errorf("nil view should render nothing, got %q", b.String())
	}
}

// TestBuildBakeChoiceShiftStartingBeforeDuskBlocks is the LLM-650 case: a bake runs
// to dusk, so a scheduled actor whose shift begins before then must not be offered
// it, or the hearth pin holds him through his post hours. Live 2026-09-01: Lewis
// Walker, the wright (09:00–18:00), joined the 08:27 household bake and missed the
// shift entire. The gate keys on the START falling inside [now, dusk): a shift that
// already ended, or one starting after dusk, leaves the day free.
func TestBuildBakeChoiceShiftStartingBeforeDuskBlocks(t *testing.T) {
	const daytime = 16 * 60 // 16:00, three hours before the 19:00 dusk
	snap := eveningSnap(daytime)
	snap.HomeBakesActive = map[sim.StructureID]bool{"cottage": true}

	pre := eveningWorker("cottage")
	pre.ScheduleStartMin, pre.ScheduleEndMin = evMinPtr(17*60), evMinPtr(23*60)
	if v := buildBakeChoice(snap, pre); v != nil {
		t.Errorf("shift starting at 17:00, before dusk: got %+v, want nil", v)
	}

	night := eveningWorker("cottage")
	night.ScheduleStartMin, night.ScheduleEndMin = evMinPtr(20*60), evMinPtr(23*60)
	if v := buildBakeChoice(snap, night); v == nil || !v.Joining {
		t.Errorf("shift starting at 20:00, after dusk: got %+v, want JOIN", v)
	}

	done := eveningWorker("cottage")
	done.ScheduleStartMin, done.ScheduleEndMin = evMinPtr(5*60), evMinPtr(8*60)
	if v := buildBakeChoice(snap, done); v == nil || !v.Joining {
		t.Errorf("shift already over: got %+v, want JOIN", v)
	}

	// Boundaries (code_review): the rule is half-open at dusk, and an overnight
	// shift is judged by its start alone.
	atDusk := eveningWorker("cottage")
	atDusk.ScheduleStartMin, atDusk.ScheduleEndMin = evMinPtr(19*60), evMinPtr(23*60)
	if v := buildBakeChoice(snap, atDusk); v == nil || !v.Joining {
		t.Errorf("shift starting exactly at dusk: got %+v, want JOIN (loaves are done by then)", v)
	}
	overnightPre := eveningWorker("cottage")
	overnightPre.ScheduleStartMin, overnightPre.ScheduleEndMin = evMinPtr(18*60), evMinPtr(6*60)
	if v := buildBakeChoice(snap, overnightPre); v != nil {
		t.Errorf("overnight shift starting at 18:00, before dusk: got %+v, want nil", v)
	}
	overnightPost := eveningWorker("cottage")
	overnightPost.ScheduleStartMin, overnightPost.ScheduleEndMin = evMinPtr(20*60), evMinPtr(4*60)
	if v := buildBakeChoice(snap, overnightPost); v == nil || !v.Joining {
		t.Errorf("overnight shift starting at 20:00, after dusk: got %+v, want JOIN", v)
	}
}

// TestNoPreShiftKeeperIsOfferedBake is the LLM-650 corpus invariant: no scenario whose
// subject is scheduled, off shift now, and due at its post before dusk renders the bake
// invitation. The join arm is the cheapest form of the cue and the one that pinned Lewis
// Walker through his first shift as the wright.
func TestNoPreShiftKeeperIsOfferedBake(t *testing.T) {
	var eligible int
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			if !shiftStartsBeforeDusk(snap, snap.Actors[actorID]) {
				return
			}
			eligible++
			if strings.Contains(renderScenario(sc), "Call bake to") {
				t.Errorf("scenario %q offers the bake to a keeper whose shift starts before dusk — "+
					"the pin would hold them at the hearth through their post hours (LLM-650).", sc.name)
			}
		})
	}
	// Vacuity floor: scheduled_keeper_preshift_gets_no_bake_cue carries the shape.
	if eligible == 0 {
		t.Error("no scenario in the matrix has a scheduled subject due at its post before dusk — " +
			"scheduled_keeper_preshift_gets_no_bake_cue was removed or its schedule drifted; the invariant checked nothing.")
	}
}
