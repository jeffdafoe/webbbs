package sim

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// Produce tick (ZBBS-HOME-241, redesigned in LLM-319) — the production-cycle
// progress and landing resolver.
//
// Production is opt-in per batch: nothing is made unless the actor calls the
// `produce` tool, which validates, consumes the recipe inputs, and opens the
// actor's single ProductionActivity window (StartProductionCycle). This tick
// advances that window and lands the batch:
//
//   1. Anchor: every tick advances LastProgressAt, whether or not progress
//      accrues — so time spent away from the post is discarded, not credited
//      on return. A zero anchor (fresh start, or a restart reload) stamps
//      without crediting, the legacy first-observation posture.
//   2. Gate: progress accrues only while the actor is inside its work
//      structure AND awake (produceTickGate), and not degraded-shut
//      (LLM-304). A failed gate pauses the batch; it never cancels.
//   3. Credit: elapsed seconds scaled by the labor boost (LLM-224),
//      re-sampled per tick so a helper hired mid-batch speeds the remainder.
//   4. Landing: at RemainingSeconds <= 0 the batch mints (BatchQty plus any
//      booster bonus, LLM-248), the window clears, and
//      ProductionCycleCompleted fires — the completion beat that wakes the
//      actor to decide whether to make more.
//
// The old continuous auto-fill (per-item ProduceState anchors, units-owed
// regen math) is retired: it silently converted a keeper's coins into stock
// with no decision — the Hannah Boggs broke-keeper death spiral LLM-319
// exists to stop.

// ProductionActivity is an actor's single in-flight production cycle. Opened
// by StartProductionCycle (inputs already consumed), advanced and landed by
// ApplyProduceTick. Value struct (no nested pointers) so CloneActor copies it
// shallowly.
//
// Item/BatchQty/RemainingSeconds are CHECKPOINTED (a cycle runs tens of
// minutes and its inputs are already spent — losing it on restart would eat
// the inputs). LastProgressAt is deliberately NOT: a reload leaves it zero, so
// the first post-restart tick stamps the anchor without crediting and the
// downtime never counts as work.
type ProductionActivity struct {
	Item             ItemKind
	BatchQty         int   // units the cycle mints at landing (recipe OutputQty captured at start)
	RemainingSeconds int64 // work left at BASE rate; the labor boost shrinks it faster than wall time
	LastProgressAt   time.Time
}

// DefaultLaborProduceBoostPct is the per-worker production boost (LLM-224):
// each hired worker laboring AT the keeper's establishment adds this percent
// of the keeper's own base rate to the produce tick, so a wage buys real
// output instead of pure flavor (one helper at 50 → 1.5x, two → 2x). A
// non-positive WorldSettings.LaborProduceBoostPct disables the boost (the
// per-feature off-switch, mirroring FarmUpkeepCoinsPerShovel==0). Guesstimate,
// tuned live via the umbilical (settings/labor-produce-boost).
const DefaultLaborProduceBoostPct = 50

// laboringHelperCount counts the workers currently on an accepted job for
// employerID who are physically at the employer's work post — Working
// LaborLedger offers whose worker is at workStructureID (actorAtWorkpost: inside
// a building, or at the staff pin of a doorless stall). Ledger-authoritative
// like workerHasLiveJob (a Working row counts until the sweep settles it,
// regardless of its clock), and location-gated because the boost models
// hands-on help: a deal struck elsewhere speeds nothing until the worker is at
// the establishment. An EnRoute worker (relocating, not yet working) is skipped
// by the state check and starts counting only once the arrival subscriber flips
// them to Working at the post (LLM-229, LLM-224).
func laboringHelperCount(w *World, employerID ActorID, workStructureID StructureID) int {
	if workStructureID == "" {
		return 0
	}
	n := 0
	for _, o := range w.LaborLedger {
		if o == nil || o.State != LaborStateWorking || o.EmployerID != employerID {
			continue
		}
		worker := w.Actors[o.WorkerID]
		if worker == nil || !actorAtWorkpost(w, worker, workStructureID) {
			continue
		}
		n++
	}
	return n
}

// produceRateScalePct resolves the keeper's production-rate scale for this
// tick: 100 (base rate) plus LaborProduceBoostPct per laboring helper at the
// establishment, then sapped by ColdProduceSapPct while the keeper is
// red-or-worse cold (LLM-412 — miserable and productivity-sapping, never
// lethal), then by StallDegradedProducePct while the keeper's business is
// degraded (LLM-446 — the forge limps rather than stopping, so the sole
// producer of the repair nails can always claw back enough to self-mend). The
// saps compose multiplicatively with the boost, so hired help genuinely
// shortens a degraded recovery. Sampled once per keeper per tick — the 1-min
// cadence bounds how stale a mid-window hire/finish (or a warming-up keeper)
// can read.
func produceRateScalePct(w *World, employerID ActorID, employer *Actor) int {
	// int64 intermediates: helperCount × an extreme LaborProduceBoostPct (the
	// knob is not range-capped) could overflow an int mid-computation and go
	// negative — a corrupted rate. The saps only shrink the value, so computing
	// wide and clamping once at the end keeps every path in range (code_review,
	// LLM-446).
	scale := int64(100)
	if boostPct := w.Settings.LaborProduceBoostPct; boostPct > 0 {
		scale += int64(laboringHelperCount(w, employerID, employer.WorkStructureID)) * int64(boostPct)
	}
	if sap := w.Settings.ColdProduceSapPct; sap > 0 && sap < 100 && actorRedCold(w, employer) {
		scale = scale * int64(sap) / 100
	}
	// The 0 and >=100 guards match the cold sap's posture: 0 is not a sap here
	// (it's the full block, applied at degradedProduceBlocked before progress is
	// credited), and >=100 means no penalty — an out-of-range stored value can
	// never boost a degraded business.
	if sap := w.Settings.StallDegradedProducePct; sap > 0 && sap < 100 && ownerStallDegraded(w, employerID) {
		scale = scale * int64(sap) / 100
	}
	// Clamp to int32 range so the caller's elapsed×scale credit multiply stays
	// far inside int64 no matter the knob values. The floor can't be hit today
	// (helpers and saps never subtract below the 100 base), but a clamped
	// posture beats trusting every future term.
	if scale > math.MaxInt32 {
		scale = math.MaxInt32
	}
	if scale < 0 {
		scale = 0
	}
	return int(scale)
}

// actorRedCold reports whether the actor's cold sits at or past its red
// threshold — the "works slower" line the production sap keys on. A missing
// need row reads 0 (not cold).
func actorRedCold(w *World, a *Actor) bool {
	if a == nil || a.Needs == nil {
		return false
	}
	return a.Needs[ColdNeedKey] >= w.Settings.NeedThresholds.Get(ColdNeedKey)
}

// ProduceEvent records one LANDED production batch for the recent-production
// readout the trade cue shows a producer (LLM-116). Restart-lossy — a
// transient decision-support signal, never checkpointed (same posture as the
// price book).
type ProduceEvent struct {
	Item ItemKind
	Qty  int
	At   time.Time
}

// RecentProduceCapacity bounds the per-actor recent-production ring. 32 covers a
// busy smith's recent run; the windowed count the cue shows (restockSalesWindow)
// reads only what fits, like the price book's 20-deep ring.
const RecentProduceCapacity = 32

// recordRecentProduce appends one landed batch to the actor's RecentProduce
// ring, dropping the oldest over capacity.
func recordRecentProduce(a *Actor, item ItemKind, qty int, now time.Time) {
	if qty <= 0 {
		return
	}
	a.RecentProduce = append(a.RecentProduce, ProduceEvent{Item: item, Qty: qty, At: now})
	if len(a.RecentProduce) > RecentProduceCapacity {
		a.RecentProduce = a.RecentProduce[len(a.RecentProduce)-RecentProduceCapacity:]
	}
}

// ProduceTickInventoryChange is a per-item inventory delta produced by
// ApplyProduceTick — one landed batch. The Hub layer (when ported) translates
// these to actor_inventory_changed broadcasts; today they surface via the
// command result.
type ProduceTickInventoryChange struct {
	ActorID       ActorID
	Item          ItemKind
	QuantityAdded int
	NewQuantity   int
}

// ProduceTickResult is the command-reply payload — number of batches landed
// plus the per-item changes.
type ProduceTickResult struct {
	Executions int
	Changes    []ProduceTickInventoryChange
}

// makeableRecipe reports whether item has a recipe that can actually produce —
// it exists with a positive rate. The single definition of "makeable" shared by
// StartProductionCycle (the produce gate), shouldChooseProduction (the wake
// gate), and the trade cue, so all three agree on the choice set.
// (It does NOT require recipe inputs — an origin producer like nail makes its good
// from an empty inputs list and is fully makeable.)
func makeableRecipe(w *World, item ItemKind) bool {
	r, ok := w.Recipes[item]
	return ok && r != nil && r.RateQty > 0 && r.RatePerHours > 0
}

// makeableProduceCount counts how many of the produce entries are makeable
// (recipe-backed with positive rate).
func makeableProduceCount(w *World, entries []RestockEntry) int {
	n := 0
	for _, e := range entries {
		if makeableRecipe(w, e.Item) {
			n++
		}
	}
	return n
}

// HasProduceInputs reports whether inventory holds enough of every required
// input to run one production cycle of recipe. A recipe with no inputs (an
// origin producer like nail or water) is trivially satisfied. Exported so the
// perception trade cue applies the SAME inputs test StartProductionCycle's
// start-time consumption uses, keeping the cue and the tool in lockstep on
// "can this be made right now" — the inputs axis makeableRecipe deliberately
// omits (LLM-257).
func HasProduceInputs(recipe *ItemRecipe, inventory map[ItemKind]int) bool {
	if recipe == nil {
		return false
	}
	for _, in := range recipe.Inputs {
		if in.Qty <= 0 {
			continue
		}
		if inventory[in.Item] < in.Qty {
			return false
		}
	}
	return true
}

// craftableNow reports whether the actor could actually start a cycle of
// entry.Item right now: it is makeableRecipe (recipe with positive rate), a
// WHOLE batch fits under its carry cap, AND the actor holds the inputs for one
// cycle. Whole-batch headroom (not merely below-cap) because a landed cycle
// mints its full BatchQty — a start from one-below-cap would overshoot the cap
// by nearly a batch, stock the old continuous clamp never allowed
// (code_review). The shared choice-worthiness gate that keeps the
// production-choice warrant (shouldChooseProduction) and the produce tool
// (StartProductionCycle) from steering a keeper onto a good it cannot
// presently make, and that the trade cue mirrors via HasProduceInputs +
// the same headroom test in its Full tier (LLM-257).
func craftableNow(w *World, a *Actor, entry RestockEntry) bool {
	if !makeableRecipe(w, entry.Item) {
		return false
	}
	if !batchFitsCap(a.Inventory[entry.Item], entry.Cap(), recipeBatchQty(w.Recipes[entry.Item])) {
		return false
	}
	return HasProduceInputs(w.Recipes[entry.Item], a.Inventory)
}

// batchFitsCap reports whether a whole batch of batchQty lands at or under
// cap from onHand. Uncapped (cap <= 0) always fits. Exported to perception in
// spirit via the trade cue's Full tier, which applies the same test so the cue
// and the tool can't drift on "room for another batch".
func batchFitsCap(onHand, cap, batchQty int) bool {
	if cap <= 0 {
		return true
	}
	return onHand+batchQty <= cap
}

// recipeBatchQty is the units one cycle of recipe mints (OutputQty floored at
// 1) — the same figure StartProductionCycle captures into the window.
func recipeBatchQty(recipe *ItemRecipe) int {
	if recipe == nil || recipe.OutputQty < 1 {
		return 1
	}
	return recipe.OutputQty
}

// CycleDurationSeconds is the base-rate work one production cycle of recipe
// takes: OutputQty units at the recipe rate (rate_qty per rate_per_hours
// hours). Porridge (10 per cycle, 8/h) → 4500s ≈ 75 min. Computed as one
// rounded division over the whole batch — dividing per-unit first truncates
// (7 units at 7/h came out 3598s, not 3600 — code_review). Returns 0 for a
// nil or rate-less recipe (not makeable — the caller gates on makeableRecipe).
func CycleDurationSeconds(recipe *ItemRecipe) int64 {
	if recipe == nil || recipe.RateQty <= 0 || recipe.RatePerHours <= 0 {
		return 0
	}
	period := int64(recipe.RatePerHours) * 3600
	units := int64(recipeBatchQty(recipe))
	return (period*units + int64(recipe.RateQty)/2) / int64(recipe.RateQty)
}

// ProductionCycleStarted / ProductionCycleCompleted are the lifecycle seams of
// a production cycle (LLM-319), mirroring SourceActivityStarted/Completed.
// Completed drives the NPC completion-beat warrant (production_cycle_reactor
// on the handlers side) and is the PC-HUD surfacing seam.
type ProductionCycleStarted struct {
	EventBase
	ActorID         ActorID
	Item            ItemKind
	BatchQty        int
	DurationSeconds int64
	At              time.Time
}

func (ProductionCycleStarted) isSimEvent() {}

// ProductionCycleCompleted carries the landed yield (batch plus booster
// bonus), self-contained so a subscriber can narrate without re-reading world
// state — the SourceActivityCompleted posture.
type ProductionCycleCompleted struct {
	EventBase
	ActorID ActorID
	Item    ItemKind
	Qty     int
	At      time.Time
}

func (ProductionCycleCompleted) isSimEvent() {}

// ProductionDoneWarrantReason captures the NPC completion beat for a landed
// production cycle (LLM-319): the batch is in the stores and the actor should
// get a tick to see it — and, via the idle trade cue now visible, decide
// whether to start another. Minted by the handlers-side ProductionCycleCompleted
// subscriber with the narration pre-rendered (the SourceActivityCompleted
// posture). DedupDiscriminator 0 — each completion is 1:1 with its landing
// sweep, nothing to dedup.
type ProductionDoneWarrantReason struct {
	Item          ItemKind
	Qty           int
	NarrationText string
}

func (ProductionDoneWarrantReason) isWarrantReason()           {}
func (ProductionDoneWarrantReason) Kind() WarrantKind          { return WarrantKindProductionDone }
func (ProductionDoneWarrantReason) DedupDiscriminator() uint64 { return 0 }

// ProductionCompletionNarration is the felt-language self-perception line for
// a landed batch — the production sibling of SourceActivityCompletionNarration.
// noun is the catalog plural display phrase (producePluralNoun).
func ProductionCompletionNarration(noun string, qty int) string {
	if qty <= 0 || noun == "" {
		return ""
	}
	return fmt.Sprintf("You finish the batch — %d %s ready in your stores.", qty, noun)
}

// ApplyProduceTick advances every in-flight production cycle and lands the due
// ones. Two passes: progress (pure per-actor field writes, safe while ranging
// w.Actors) then landing (landProductionCycle emits ProductionCycleCompleted,
// whose subscribers run inline and may touch w.Actors — the
// completeDueSourceActivities posture).
func ApplyProduceTick(now time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			res := ProduceTickResult{}
			var due []ActorID
			for actorID, actor := range w.Actors {
				act := actor.ProductionActivity
				if act == nil {
					continue
				}
				// Advance the anchor unconditionally: elapsed is credited only
				// when the gate passes NOW, so time away from the post (or a
				// restart's zero anchor) is discarded, never banked. The 1-min
				// cadence bounds how much within-window absence can misread.
				elapsed := int64(now.Sub(act.LastProgressAt).Seconds())
				first := act.LastProgressAt.IsZero()
				act.LastProgressAt = now
				if first || elapsed <= 0 {
					continue
				}
				if !produceTickGate(actor, now) {
					continue
				}
				// LLM-446: a degraded business SLOWS the batch (the sap in
				// produceRateScalePct) rather than pausing it — the legacy LLM-304
				// full pause survives only at StallDegradedProducePct == 0. The old
				// unconditional pause froze RemainingSeconds while perception kept
				// rendering it ("about 3 minutes of work left" — forever), which is
				// exactly the live "three more minutes, Josiah" loop; and on the sole
				// nail producer it deadlocked his own 5-nail forge repair.
				if degradedProduceBlocked(w, actorID) {
					continue
				}
				// LLM-224: hired help speeds the batch — each worker laboring at
				// the establishment scales this tick's credit. Re-sampled per tick
				// so a helper hired mid-batch speeds the remainder. Rounded
				// division so a non-multiple-of-100 scale (the knob is
				// configurable) doesn't shed a fraction of a second every tick
				// (~48s/h at 133% — code_review). LLM-412: the scale can now also
				// dip BELOW 100 (a red-cold keeper works at ColdProduceSapPct), so
				// apply on any deviation from base, not just a boost.
				credit := elapsed
				if scale := produceRateScalePct(w, actorID, actor); scale != 100 && scale > 0 {
					credit = (elapsed*int64(scale) + 50) / 100
				}
				act.RemainingSeconds -= credit
				if act.RemainingSeconds <= 0 {
					due = append(due, actorID)
				}
			}
			for _, id := range due {
				actor := w.Actors[id]
				if actor == nil || actor.ProductionActivity == nil {
					continue
				}
				change := landProductionCycle(w, id, actor, actor.ProductionActivity, now)
				res.Executions++
				res.Changes = append(res.Changes, ProduceTickInventoryChange{
					ActorID:       id,
					Item:          change.Item,
					QuantityAdded: change.Added,
					NewQuantity:   change.NewQty,
				})
			}
			return res, nil
		},
	}
}

// produceTickGate is the progress gate: actor must be inside their work
// structure AND not currently sleeping. Matches the legacy auto-produce gate,
// now meaning "the batch advances" rather than "goods mint silently".
func produceTickGate(actor *Actor, now time.Time) bool {
	if actor.WorkStructureID == "" {
		return false
	}
	if actor.InsideStructureID != actor.WorkStructureID {
		return false
	}
	if actor.SleepingUntil != nil && now.Before(*actor.SleepingUntil) {
		return false
	}
	return true
}

type produceChange struct {
	Item   ItemKind
	Added  int
	NewQty int
}

// landProductionCycle mints a finished batch: BatchQty units (captured at
// start, so a mid-flight recipe change can't rewrite what the consumed inputs
// bought) plus any booster bonus, then clears the window and emits the
// completion. ProductionNagAt is stamped so the production-choice scan doesn't
// re-nag on top of the completion beat — the completion warrant IS the wake to
// decide about the next batch.
//
// Boosters (LLM-248) are evaluated here, at landing — the same instant the old
// continuous tick consumed them (mint time). Per booster: holding Qty consumes
// it and adds BonusQty, clamped to remaining cap headroom; a fully clamped
// bonus skips consumption, a partially clamped one still consumes in full (the
// clamp trims yield, not cost — the herb went into the batch either way).
//
// State boosters (LLM-474) are weighed at the same instant against a world
// condition rather than the producer's inventory, and consume nothing. Both
// kinds are strictly additive: neither can skip an execution or reduce base
// yield, so a recipe always produces its full BatchQty with every booster
// unmet.
func landProductionCycle(w *World, actorID ActorID, actor *Actor, act *ProductionActivity, now time.Time) produceChange {
	if actor.Inventory == nil {
		actor.Inventory = make(map[ItemKind]int)
	}
	total := act.BatchQty
	if recipe := w.Recipes[act.Item]; recipe != nil {
		cap := 0
		if entry, ok := produceEntry(actor, act.Item); ok {
			cap = entry.Cap()
		}
		for _, bi := range recipe.BoostInputs {
			if bi.Qty <= 0 || bi.BonusQty <= 0 {
				continue
			}
			if actor.Inventory[bi.Item] < bi.Qty {
				continue
			}
			bonus := bi.BonusQty
			if cap > 0 {
				if room := cap - (actor.Inventory[act.Item] + total); bonus > room {
					bonus = room
				}
			}
			if bonus <= 0 {
				continue
			}
			actor.Inventory[bi.Item] -= bi.Qty
			if actor.Inventory[bi.Item] <= 0 {
				delete(actor.Inventory, bi.Item)
			}
			total += bonus
		}
		// State boosters (LLM-474): the same elective posture as the item
		// boosters above, keyed on a world condition instead of inventory and
		// consuming nothing — the firewood behind a lit hearth was already
		// spent by an earlier stoke. An unmet state adds nothing and takes
		// nothing; base yield stands. This must never gate an execution.
		for _, bs := range recipe.BoostState {
			if bs.BonusQty <= 0 || !recipeBoostStateMet(w, actor, bs.State, now) {
				continue
			}
			bonus := bs.BonusQty
			if cap > 0 {
				if room := cap - (actor.Inventory[act.Item] + total); bonus > room {
					bonus = room
				}
			}
			if bonus <= 0 {
				continue
			}
			total += bonus
		}
	}
	actor.Inventory[act.Item] += total
	actor.ProductionActivity = nil
	actor.ProductionNagAt = now
	recordRecentProduce(actor, act.Item, total, now)
	// Equipment service (LLM-648): every minted unit — booster bonuses
	// included, the tools worked for those too — accrues deep-maintenance
	// demand on the producer's owned business.
	AccrueEquipmentUse(w, actorID, total)
	w.emit(&ProductionCycleCompleted{
		ActorID: actorID,
		Item:    act.Item,
		Qty:     total,
		At:      now,
	})
	return produceChange{
		Item:   act.Item,
		Added:  total,
		NewQty: actor.Inventory[act.Item],
	}
}

// recipeBoostStateMet evaluates one BoostState condition against the world at
// LANDING time. Landing-time evaluation matches the BoostInputs precedent (a
// booster is weighed when the batch mints, not when it started), so a fire that
// died mid-batch forfeits the bonus and one stoked late still earns it. Keep in
// lockstep with sim.ValidRecipeBoostState — a state that validates but has no
// arm here is a booster that never pays.
func recipeBoostStateMet(w *World, actor *Actor, state RecipeBoostState, now time.Time) bool {
	switch state {
	case BoostStateHearthLit:
		// The WORK structure's hearth — the pot cooked where the business is.
		//
		// In practice this is the same structure the actor is standing in:
		// produceTickGate is applied before elapsed time is credited, so an
		// actor off-post accrues nothing and never reaches landing at all
		// (stepping out DEFERS the batch, it does not land it elsewhere).
		// WorkStructureID is used anyway because it names the intent directly
		// and does not quietly depend on that gate ordering staying put.
		//
		// Nil-safe the whole way down: a structure with no hearth object
		// resolves nil and reads as unlit, which is what holds every
		// non-hearth kitchen at exactly today's yield.
		return HearthLit(StructureHearth(w.VillageObjects, actor.WorkStructureID), now)
	}
	return false
}

// ProduceTickerInterval is how often RunProduceTicker wakes. The 1-min cadence
// is fine-grained against cycle durations measured in tens of minutes.
const ProduceTickerInterval = time.Minute

// RunProduceTicker owns the produce-tick goroutine. Wakes every
// ProduceTickerInterval and submits ApplyProduceTick.
func RunProduceTicker(ctx context.Context, w *World) {
	t := time.NewTicker(ProduceTickerInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.beatTicker("produce")
			_, err := w.SendContext(ctx, ApplyProduceTick(time.Now().UTC()))
			if err != nil && ctx.Err() == nil {
				log.Printf("sim/produce_ticker: %v", err)
			}
		}
	}
}
