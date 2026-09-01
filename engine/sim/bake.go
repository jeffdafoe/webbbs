package sim

import (
	"fmt"
	"log"
	"time"
)

// bake.go — LLM-454. The daytime bake-bread occupation: a shared, per-home activity
// that fills the home-idle stretch of the DAY before dusk — the mid-afternoon window
// where the household otherwise turboyaps "let's make bread" without doing it (the
// LLM-453 daytime loop, which disperse failed to close: the models never took an exit,
// they wanted the task). A resident at home during the day starts the household's
// bread; others home lend a hand at the SAME batch. Everyone is occupied (BusyAtSource
// shelves the LLM tick, so no looping) until dusk; then the batch lands to the
// initiator, who shares it around. (Dusk → bedtime stays LLM-447/leisure territory.)
//
// Built on the SourceActivity dwell substrate (produce is workplace-locked): a
// SourceActivityBake window per participant, plus one HomeBake session per home
// holding the shared batch. Both are transient — a restart drops them, which costs
// nothing because flour is consumed only at completion (the household just re-forms
// the bake and still finishes by dusk; persistent inventory stays consistent).

// Bake tuning — deliberately low. This is a daytime TIME SINK, not an economy: the
// output that matters is "the afternoon is spent, not looped," and the loaves are a
// byproduct the household eats. All tunable constants.
const (
	BakeFlourItem = ItemKind("flour")
	BakeBreadItem = ItemKind("bread")
	// BakeFlourCost is the flour the INITIATOR provides — checked at start, consumed
	// at completion (so a restart mid-bake forfeits nothing).
	BakeFlourCost = 2
	// BakeBatchQty is the loaves minted to the initiator when the bake lands.
	BakeBatchQty = 3
	// MinBakeWindow is the least of the day that must remain before dusk for a bake
	// to be worth starting.
	MinBakeWindow = 30 * time.Minute
)

// HomeBake is the single shared bake in progress at one home structure (keyed by
// StructureID in World.HomeBakes). Transient — NOT checkpointed; a restart drops it
// along with the participants' SourceActivity, and since flour is consumed only at
// completion the household simply re-forms the bake, forfeiting nothing.
type HomeBake struct {
	StructureID StructureID
	InitiatorID ActorID
	BatchQty    int
	FlourCost   int
	StartedAt   time.Time
	DoneAt      time.Time // the next dusk — when the batch lands
}

// duskInstant returns the next dusk instant (DuskTime in the world timezone) strictly
// after now — the cue an at-home daytime bake runs until — and ok=false when the clock
// is unconfigured. Modeled on nextDaybreak. Unlike a fail-open fallback (which would let
// the sim accept a bake perception never advertised), the caller REJECTS on !ok, so the
// sim and the perception gate agree on the same dusk rather than diverging.
func duskInstant(w *World, now time.Time) (time.Time, bool) {
	_, dusk, ok := worldDawnDuskMinutes(w) // [dawn, dusk) minutes
	loc := w.Settings.Location
	if !ok || loc == nil {
		return time.Time{}, false
	}
	local := now.In(loc)
	d := time.Date(local.Year(), local.Month(), local.Day(), dusk/60, dusk%60, 0, 0, loc)
	if !d.After(now) {
		d = d.AddDate(0, 0, 1)
	}
	return d, true
}

// StartOrJoinBake is the commit for the bake tool. If a HomeBake is already going at
// the actor's home it JOINS it (no flour, no new batch — the shared household bake);
// otherwise it STARTS one (the initiator provides the flour). Either way the actor
// breaks off any conversation and is occupied at a SourceActivityBake window until
// dusk. MUST run on the world goroutine.
func StartOrJoinBake(actorID ActorID, say string, hasNewNews bool, now time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			actor, ok := w.Actors[actorID]
			if !ok || actor == nil {
				return nil, fmt.Errorf("StartOrJoinBake: actor %q not in world", actorID)
			}
			if actor.MoveIntent != nil {
				return nil, ModelFacingError{Msg: "you are walking — get home before you start baking."}
			}
			// Land a finished-but-not-yet-swept window first so a stale activity
			// doesn't spuriously read as "still busy" (matches StartStoke).
			completeIfDue(w, actorID, actor, now)
			if actor.SourceActivity != nil {
				return nil, ModelFacingError{Msg: "you are already busy — finish what you're doing first."}
			}
			home := actor.HomeStructureID
			if home == "" || actor.InsideStructureID != home {
				return nil, ModelFacingError{Msg: "you can only bake in your own home."}
			}
			// Re-validate the advertised gate at the substrate — a stale or forged tool
			// call must not bake at the wrong time. The perception BakeChoice gate is an
			// optimization; StartOrJoinBake is the authority (mirrors StartStoke's posture).
			if actor.State == StateSleeping {
				return nil, ModelFacingError{Msg: "you are asleep."}
			}
			nowMinute := localMinuteOfDay(w, now)
			// isActorOnShift is false for an unscheduled worker (no schedule), so its
			// day-active window is NOT treated as a binding shift — it can bake at home
			// during the day, which is exactly the looping homebodies this fills. Only a
			// SCHEDULED actor within its shift is turned away to its post. (actorOnShift,
			// which reads the unscheduled worker's day-active pseudo-shift as "on", would
			// wrongly reject them.)
			if isActorOnShift(actor, nowMinute) {
				return nil, ModelFacingError{Msg: "you're on your shift — the baking waits until the day's work is done."}
			}
			// Mirror the perception gate (inDaytimeHomeWindow): an UNSCHEDULED actor must
			// be a worker — an unscheduled non-worker's home is its resting state, never
			// offered the bake, so the commit path rejects a forged/stale call the same
			// way (tool-cue lockstep). A SCHEDULED actor passed the on-shift check above
			// and needs no worker gate (both windows offer to a scheduled actor regardless
			// of the attribute).
			if actor.ScheduleStartMin == nil && actor.ScheduleEndMin == nil && !actorIsWorker(actor) {
				return nil, ModelFacingError{Msg: "the baking is a household worker's task."}
			}
			dawn, dusk, ok := worldDawnDuskMinutes(w)
			if !ok {
				return nil, ModelFacingError{Msg: "you can't tell the time of day."}
			}
			if nowMinute < dawn || nowMinute >= dusk {
				return nil, ModelFacingError{Msg: "it's past the time for it — the baking is a daytime task, finished before dusk."}
			}
			doneAt, ok := duskInstant(w, now)
			if !ok {
				return nil, ModelFacingError{Msg: "you can't tell how much of the day is left."}
			}
			if !doneAt.After(now.Add(MinBakeWindow)) {
				return nil, ModelFacingError{Msg: "there's not enough of the day left to bake before dusk."}
			}

			// LLM-650: mirror of shiftStartsBeforeDusk (perception). A bake runs to
			// dusk, so a shift that begins before then would pin the baker at the
			// hearth through it — the on-shift check above only asks about now.
			if actor.ScheduleStartMin != nil && actor.ScheduleEndMin != nil &&
				nowMinute < *actor.ScheduleStartMin && *actor.ScheduleStartMin < dusk {
				return nil, ModelFacingError{Msg: "your shift starts before the loaves would be done — the baking is for a day you're not at your post."}
			}

			// A stale session — its initiator lost the bake by some non-completion path,
			// or its window has already passed — self-heals here so it can never orphan
			// and block new bakes: drop it and fall through to START.
			session := w.HomeBakes[home]
			if session != nil && !homeBakeLive(w, session, now) {
				delete(w.HomeBakes, home)
				session = nil
			}
			// A red need the shelve can't serve bars the bake either way — see
			// bakeTrappingRedNeed (perception) for why tiredness alone is a trap: it
			// is excluded from both the move_to carve-out and the reactor interrupt,
			// so an exhausted joiner would sit at the hearth until dusk with no way
			// out. Mirrors buildBakeChoice's join arm (LLM-465).
			if actor.Needs["tiredness"] >= w.Settings.NeedThresholds.Get("tiredness") {
				return nil, ModelFacingError{Msg: "you are too worn to stand at the hearth — rest first."}
			}
			if session == nil {
				// START — the initiator provides the flour (checked now, consumed at
				// completion).
				//
				// The remaining red needs (hunger/thirst/cold) are START-ONLY, in
				// lockstep with buildBakeChoice (LLM-465). Starting commits the actor
				// to the whole afternoon, so a pressing need outranks it; JOINING
				// costs nothing and leaves those needs fully actionable
				// (bakingMayMove keeps move_to for them, and
				// hasBreakInterruptingNeedWarrant ticks him for them), so it stays
				// open to a hungry housemate. Checked HERE rather than above the
				// session resolution so the substrate rejects exactly what perception
				// declines to advertise.
				if countRedNeeds(w.Settings, actor) > 0 {
					return nil, ModelFacingError{Msg: "see to what's pressing first — the baking can wait for a quiet hour."}
				}
				if actor.Inventory[BakeFlourItem] < BakeFlourCost {
					return nil, ModelFacingError{Msg: fmt.Sprintf(
						"baking a batch takes %d bags of flour and you have %d — buy more from the store first.",
						BakeFlourCost, actor.Inventory[BakeFlourItem])}
				}
				session = &HomeBake{
					StructureID: home,
					InitiatorID: actorID,
					BatchQty:    BakeBatchQty,
					FlourCost:   BakeFlourCost,
					StartedAt:   now,
					DoneAt:      doneAt,
				}
				w.HomeBakes[home] = session
			}

			// Break off any conversation to go to the hearth — the parting word (if
			// any) rides the tool, spoken to the room before leaving (best-effort,
			// like scene_quote's say). Baking then shelves the tick until bed.
			if actor.CurrentHuddleID != "" {
				if say != "" {
					if _, serr := SpeakTo(actorID, say, "", nil, hasNewNews, now).Fn(w); serr != nil {
						log.Printf("sim: bake %q announced but the say was refused: %v", actorID, serr)
					}
				}
				leaveCurrentHuddle(w, actor, now)
			}

			homeObj := w.VillageObjects[VillageObjectID(home)]
			actor.SourceActivity = &SourceActivity{
				Kind:      SourceActivityBake,
				ObjectID:  VillageObjectID(home),
				StartedAt: now,
				Until:     doneAt,
			}
			w.emit(&SourceActivityStarted{
				ActorID:  actorID,
				ObjectID: VillageObjectID(home),
				Kind:     SourceActivityBake,
				Until:    doneAt,
				At:       now,
			})
			return SourceActivityStartResult{
				Started:    true,
				Kind:       SourceActivityBake,
				ObjectID:   VillageObjectID(home),
				SourceName: sourceActivityObjectName(w, homeObj),
				Until:      doneAt,
			}, nil
		},
	}
}

// completeHomeBake lands the shared batch for the session at home when its INITIATOR
// finishes: it consumes the initiator's flour (re-checked; the baker was shelved so
// it is still on hand) and mints the loaves to it. Returns the minted qty (0 for a
// joiner, an orphaned/concluded session, or a vanished-flour edge) so the caller's
// completion beat can name the yield. Deletes the session on the initiator's landing.
func completeHomeBake(w *World, actorID ActorID, actor *Actor, home StructureID) int {
	session := w.HomeBakes[home]
	if session == nil || session.InitiatorID != actorID {
		return 0 // a joiner, or the session already concluded — a hand lent, no batch
	}
	// The session concludes on the initiator's landing regardless of outcome, so a
	// spent-flour or moved-out completion can't leave it to orphan.
	delete(w.HomeBakes, home)
	// Only a batch that finished where it started, with the flour still on hand, lands.
	// The baker is shelved (BusyAtSource) the whole window, so in the normal path
	// neither changes; these guards cover an out-of-band relocation or a flour spend
	// (an operator command) between start and completion. The read/consume/mint is
	// serialized on the world goroutine, so it is atomic.
	if actor.InsideStructureID != home {
		return 0
	}
	if actor.Inventory == nil {
		actor.Inventory = map[ItemKind]int{}
	}
	if actor.Inventory[BakeFlourItem] < session.FlourCost {
		return 0 // flour gone — nothing to show for it, and the session is already cleared
	}
	actor.Inventory[BakeFlourItem] -= session.FlourCost
	if actor.Inventory[BakeFlourItem] == 0 {
		delete(actor.Inventory, BakeFlourItem)
	}
	actor.Inventory[BakeBreadItem] += session.BatchQty
	return session.BatchQty
}

// homeBakeLive reports whether the session's initiator is still actively baking it —
// present, holding a bake SourceActivity, and the window not yet past. A session that
// fails this has orphaned (the initiator's bake was cleared by sleep, an operator
// interrupt, removal, or reload) and StartOrJoinBake drops it rather than let a
// co-resident join a bake that will never land. Read on the world goroutine.
func homeBakeLive(w *World, session *HomeBake, now time.Time) bool {
	if session == nil || !session.DoneAt.After(now) {
		return false
	}
	a := w.Actors[session.InitiatorID]
	return a != nil && a.SourceActivity != nil && a.SourceActivity.Kind == SourceActivityBake
}

// concludeAbandonedBake ends the household bake if the actor walking away WAS its
// initiator (its SourceActivityBake was just move-cancelled): the initiator carried
// the flour and the yield, so nothing lands. Joiners still baking finish their own
// windows to no batch. A no-op for a joiner's departure or when no bake is running.
func concludeAbandonedBake(w *World, actorID ActorID, home StructureID) {
	if session := w.HomeBakes[home]; session != nil && session.InitiatorID == actorID {
		delete(w.HomeBakes, home)
	}
}

// homeBakeInProgress reports whether a shared bake is already running at home — the
// signal that a fresh bake tool call JOINS rather than STARTS (so it needs no flour).
// Read on the world goroutine.
func homeBakeInProgress(w *World, home StructureID) bool {
	return home != "" && w.HomeBakes[home] != nil
}

// homeBakesActiveSet projects the in-progress home bakes into a snapshot-ready set of
// structure ids (nil when none) — the read side perception's buildBakeChoice uses to
// know a bake is already going at a home, so a co-resident can JOIN it without flour.
func homeBakesActiveSet(w *World) map[StructureID]bool {
	if len(w.HomeBakes) == 0 {
		return nil
	}
	out := make(map[StructureID]bool, len(w.HomeBakes))
	for id := range w.HomeBakes {
		out[id] = true
	}
	return out
}
