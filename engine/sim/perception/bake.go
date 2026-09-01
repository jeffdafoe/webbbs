package perception

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// bake.go — LLM-454. The daytime bake affordance: the cue + tool-gate signal for a
// resident who could bake the household's bread at home during the day, before dusk.
// The presence of BakeChoiceView is the SINGLE gate both the bake tool
// (handlers/tool_gating.go) and the cue (renderBakeChoice) read, so they can't drift
// (tool-cue lockstep, discussion-109). It fills the home-idle daytime stretch where
// the homebodies otherwise loop "let's make bread" without doing it — the thing they
// keep narrating, made a real, invokable task.

// bakeMinWindowMinutes mirrors sim.MinBakeWindow (30 min) — the least of the day that
// must remain before dusk for a bake to be worth offering.
const bakeMinWindowMinutes = 30

// BakeChoiceView is the daytime bake affordance for a resident at home. nil when
// baking isn't on the table (not home, not daytime, on shift, busy, a pressing need,
// or neither the flour to start nor a household bake to join).
type BakeChoiceView struct {
	// Joining is true when a household bake is already in progress here — the actor
	// lends a hand at the same batch and needs no flour of its own; false when it
	// would START the batch (and provides the flour).
	Joining bool
}

// buildBakeChoice returns the daytime bake affordance for a resident at home, or nil.
// Conditions (the OffersBake gate, mirrored by the sim-side StartOrJoinBake checks):
// an awake agent NPC, inside its own home (a home is implicitly a kitchen — no hearth
// tag, LLM-454), in the at-home daytime window (before dusk, not on a scheduled shift)
// with enough of the day left before dusk, nothing pressing, not already busy, and
// either holding the flour to start OR a household bake already going here to join.
func buildBakeChoice(snap *sim.Snapshot, a *sim.ActorSnapshot) *BakeChoiceView {
	if snap == nil || a == nil || snap.LocalMinuteOfDay == nil {
		return nil
	}
	if a.Kind != sim.KindNPCStateful && a.Kind != sim.KindNPCShared {
		return nil
	}
	if a.State == sim.StateSleeping {
		return nil
	}
	home := a.HomeStructureID
	if home == "" || a.InsideStructureID != home {
		return nil // must be settled inside its own home
	}
	if _, ok := resolveStructureLabel(snap, home); !ok {
		return nil // home must resolve in the snapshot
	}
	// The at-home daytime window: before dusk and NOT on an explicit scheduled shift.
	// An unscheduled labor vendor's day-active window is not a binding post, so it
	// qualifies — that's the looping homebodies this fills; a scheduled keeper on its
	// shift belongs at its post and is turned away (see inDaytimeHomeWindow).
	if !inDaytimeHomeWindow(snap, a) {
		return nil
	}
	// LLM-650: a bake runs to dusk, so a scheduled actor whose shift BEGINS before
	// then would be pinned at the hearth through it. inDaytimeHomeWindow only asks
	// whether the shift is on now — Lewis Walker, the wright, joined the 08:27
	// household bake and missed his 09:00–18:00 shift entire (live 2026-09-01).
	if shiftStartsBeforeDusk(snap, a) {
		return nil
	}
	// Enough of the day left before dusk to bother.
	if *snap.LocalMinuteOfDay >= snap.DuskMinute-bakeMinWindowMinutes {
		return nil
	}
	// An in-flight activity outranks the bake either way — the actor is already
	// occupied at a source and has nothing to gain from a second commitment.
	if actorMidSourceActivity(a) {
		return nil
	}
	joining := snap.HomeBakesActive[home]
	if joining {
		// JOINING under a pressing need is fine for every need the bake shelve leaves
		// actionable, but NOT for tiredness — see bakeTrappingRedNeed.
		if bakeTrappingRedNeed(a, snap) {
			return nil
		}
	} else {
		// STARTING is a whole afternoon's commitment, so ANY pressing (red) need
		// outranks it — see to what's pressing first.
		//
		// JOINING is not (LLM-465). It costs no flour and mints no batch; it is a
		// hand at bread the household is already baking, and refusing it protected
		// the actor from nothing — it only left him loose and fully tickable in a
		// kitchen he had no affordance in. Live 2026-07-18: Lewis Walker, red on
		// hunger while Anne and Patience baked, burned 24 turns in 70 minutes asking
		// how the loaves were coming, and each question armed bakeReplyDue for BOTH
		// bakers, making him the pump on the loop.
		if hasRedNeed(a, snap) {
			return nil
		}
		if a.Inventory[sim.BakeFlourItem] < sim.BakeFlourCost {
			return nil // nothing going to join, and no flour to start
		}
	}
	return &BakeChoiceView{Joining: joining}
}

// bakeTrappingRedNeed reports whether the subject holds a red need that joining a bake
// would leave it unable to act on — the narrow case where the join is a trap rather than
// a free hand at the bread (LLM-465).
//
// A red hunger, thirst or cold is safe: gateTools' bakingMayMove (via
// laboringMayBreakOffToEat) keeps move_to in the advertised set for those, and the
// reactor's hasBreakInterruptingNeedWarrant ticks the actor through the source-activity
// shelve for them, so it can walk out to food, drink or warmth and something will wake
// it to do so.
//
// TIREDNESS is deliberately excluded from BOTH of those carve-outs — a break cures
// tiredness, so it never justifies abandoning work in progress — which is exactly what
// makes it the one need a joiner cannot serve: nothing ticks an exhausted baker, and
// move_to is stripped for it. Admitting that join would shelve a red-tired villager at
// the hearth until dusk with no way out, which is a worse bug than the one this fixes.
// So tiredness still bars the join, matching the carve-outs rather than widening past
// them.
func bakeTrappingRedNeed(a *sim.ActorSnapshot, snap *sim.Snapshot) bool {
	// Literal key, in lockstep with handlers.laboringMayBreakOffToEat's own "tiredness"
	// check — if a need is ever added that the shelve can't serve, update both.
	return a.Needs["tiredness"] >= snap.NeedThresholds.Get("tiredness")
}

// renderBakeChoice writes the evening bake affordance as a scene (LLM-454): the same
// signal that gates the bake tool, so cue and tool surface together. It names the
// bake tool the way the forge cue names produce, and reads as a warm evening moment
// rather than a stat line (scenes, not stats).
func renderBakeChoice(b *strings.Builder, v *BakeChoiceView) {
	if v == nil {
		return
	}
	if v.Joining {
		b.WriteString("The bread is already going in the kitchen — you could lend a hand and see it through the afternoon. It is a good few hours' work, and the loaves will be ready by dusk. Call bake to join in.\n\n")
		return
	}
	b.WriteString("The house is quiet and the household is about. There is flour in the crock — you could get the bread on for the week, an afternoon's work at the hearth that leaves fresh loaves by dusk. Call bake to start.\n\n")
}

// shiftStartsBeforeDusk reports whether a scheduled actor's shift begins between
// now and dusk — the window a bake would occupy. An unscheduled actor has no
// shift to start; a shift already running is inDaytimeHomeWindow's concern.
// The rule itself is sim.ShiftStartsBeforeDusk, shared with StartOrJoinBake so
// the cue and the command cannot drift (tool-cue lockstep); this wrapper only
// unpacks the snapshot.
func shiftStartsBeforeDusk(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || snap.LocalMinuteOfDay == nil || !snap.DawnDuskMinuteOK {
		return false
	}
	return sim.ShiftStartsBeforeDusk(a.ScheduleStartMin, a.ScheduleEndMin, *snap.LocalMinuteOfDay, snap.DuskMinute)
}
