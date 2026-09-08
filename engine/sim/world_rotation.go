package sim

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"
)

// world_rotation.go — daily rotation substrate. In-memory port of v1's
// engine/world_rotation.go.
//
// At the configured WorldSettings.RotationTime (default 00:00 in the
// world's Location), the rotation ticker fires a rotation pass that
// flips every village_object currently sitting in a "rotatable"-tagged
// state to a new rotation-target state. The new state is picked per
// the asset's RotationAlgo:
//
//   - "random_per_object": each instance picks independently from the
//     asset's rotatable pool. Max visual variety per sweep (every laundry
//     line different).
//   - "random_per_asset": all instances of one asset flip to the SAME
//     newly chosen target. Visually uniform ("the noticeboards all show
//     today's prose").
//   - "deterministic": cycle through the rotatable pool in
//     AssetStateID order, wrapping. Predictable progression.
//
// State scoping mirrors the phase-transition substrate:
//
//   - RotationScope.Tag narrows the candidate set to objects whose
//     current state ALSO carries the supplied tag (e.g. "laundry" so the
//     washerwoman route only sees laundry tiles).
//   - RotationScope.ExcludeTags filters OUT objects whose current state
//     carries any of the listed tags (so the bulk rotation can hand off
//     NPC-domain tags to per-NPC route dispatch — washerwoman / town_crier
//     will own those once npc_behaviors ports).
//
// Flips fire asynchronously via the shared scheduleFlips helper, each
// stamped with RotationFlipGen so a later rotation cleanly invalidates
// stale flips (same mechanism as ApplyPhaseTransition, on its own
// counter — see ZBBS-HOME-447).
//
// What's deferred:
//
//   - Decorative-NPC tiredness reset at the rotation boundary (v1's
//     resetSleptTiredness). Tracked as part of the broader NPC sleep
//     work, not part of the rotation substrate proper.
//   - WS broadcast on rotation. Hub/WS layer hasn't ported.
//   - HTTP admin force-rotate endpoint. HTTP layer hasn't ported. The
//     ApplyDailyRotation command is admin-invokable from any in-process
//     caller today.
//
// Lamplighter / washerwoman / town_crier are NPC-route behaviors that
// live in the npc_behaviors slice (next slice). This file provides the
// substrate + the ExcludeTags carve-out hook those routes will use.

const (
	// TagRotatable identifies AssetStates that participate in the daily
	// rotation pool. Mirrors v1's asset_state_tag value.
	TagRotatable = "rotatable"

	// RotationAlgoRandomPerObject — each instance picks an independent
	// target from its asset's rotatable pool. Drives "every laundry
	// line different" visual.
	RotationAlgoRandomPerObject = "random_per_object"

	// RotationAlgoRandomPerAsset — every instance of one asset flips to
	// the same newly chosen target this pass. Drives "all noticeboards
	// show today's prose" visual.
	RotationAlgoRandomPerAsset = "random_per_asset"

	// RotationAlgoDeterministic — cycle through the rotatable pool in
	// AssetStateID order, wrapping past the last entry. Predictable
	// progression for assets where variety isn't the goal.
	RotationAlgoDeterministic = "deterministic"

	// RotationTickerInterval is how often RunRotationTicker wakes to
	// check whether today's rotation boundary has been processed. One
	// minute matches RunPhaseTicker's cadence.
	RotationTickerInterval = time.Minute
)

// MostRecentRotationBoundary returns the wall-clock instant of the most
// recent daily-rotation boundary at or before now. Rotation runs once
// per day at the (h, m) wall-clock time interpreted in now's location.
// If today's boundary hasn't yet passed, returns yesterday's at the same
// wall-clock time.
//
// DST safety: yesterday is computed via time.Date(y, mo, d-1, ...) NOT
// today.Add(-24*time.Hour). On a spring-forward / fall-back day the two
// differ by an hour; we want the wall-clock semantic (yesterday at
// HH:MM in loc) so a noon rotation stays at noon across the transition.
func MostRecentRotationBoundary(now time.Time, h, m int) time.Time {
	loc := now.Location()
	y, mo, d := now.Date()
	today := time.Date(y, mo, d, h, m, 0, 0, loc)
	if today.After(now) {
		return time.Date(y, mo, d-1, h, m, 0, 0, loc)
	}
	return today
}

// NextRotationBoundary returns the next daily-rotation boundary after now —
// today's rotation time if it's still ahead, else tomorrow's. The countdown
// counterpart to MostRecentRotationBoundary, used by the admin config read
// (GET /api/village/config) for the "next rotation" readout.
func NextRotationBoundary(now time.Time, h, m int) time.Time {
	loc := now.Location()
	y, mo, d := now.Date()
	today := time.Date(y, mo, d, h, m, 0, 0, loc)
	if today.After(now) {
		return today
	}
	return time.Date(y, mo, d+1, h, m, 0, 0, loc)
}

// RotationScope narrows the candidate set for a rotation pass. Tag
// matches v1's per-domain narrowing (washerwoman scopes to "laundry",
// town_crier scopes to "notice-board"). ExcludeTags is the parallel —
// remove objects whose current state carries any listed tag from the
// bulk pass, so a separate dispatch mechanism (per-NPC route) can
// process them. Both empty = full rotatable pool.
type RotationScope struct {
	Tag         string
	ExcludeTags []string
}

// RotationTickInputs carries the per-tick inputs the rotation pass
// reads. Bundled as a struct so callers can construct a deterministic
// input set in tests (overriding Now, RNG) without piggybacking on
// world state. Mirrors VisitorTickInputs.
//
// Now: wall-clock instant the pass fires; stamped into
// Environment.LastRotationAt.
//
// Rand: random source for random_per_object / random_per_asset picks.
// Production uses a per-driver Rand seeded once at registration; tests
// pass a deterministic seed.
type RotationTickInputs struct {
	Now  time.Time
	Rand *rand.Rand
}

// RotationResult is what ApplyDailyRotation returns through the command
// reply. Mirrors PhaseTransitionResult.
type RotationResult struct {
	At              time.Time
	Gen             uint64
	ObjectsAffected int
}

// ApplyDailyRotation returns a Command that runs one rotation pass. The
// pass:
//
//  1. Computes the per-object PendingFlip list via DetermineRotationFlips
//     against (world, scope, inputs.Rand).
//  2. Stamps World.Environment.LastRotationAt = inputs.Now.
//  3. Bumps RotationFlipGen and stamps it (+ FlipDomainRotation) onto
//     every flip — its own counter, so an in-flight phase transition's
//     spread flips are not invalidated (ZBBS-HOME-447).
//  4. Schedules the flips via scheduleFlips (same helper the phase path
//     uses — async fire with TransitionSpreadSeconds stagger).
//  5. Emits a RotationApplied event so subscribers (npc_behaviors cascade,
//     when it lands) can react.
//
// inputs.Rand MUST be non-nil. Production callers (RunRotationTicker)
// supply a per-driver seeded source; tests supply a deterministic seed.
// scope is value-copied — caller may reuse / mutate after calling.
//
// Idempotency: redundant invocations against an already-converged world
// produce zero flips and an event with ObjectsAffected=0. LastRotationAt
// is still stamped — admins force-rotating want the stamp regardless of
// whether anything actually flipped (semantic parity with
// ApplyPhaseTransition).
func ApplyDailyRotation(inputs RotationTickInputs, scope RotationScope) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if inputs.Rand == nil {
				return nil, fmt.Errorf("ApplyDailyRotation: Rand required")
			}
			now := inputs.Now
			if now.IsZero() {
				now = time.Now().UTC()
			}

			flips := determineRotationFlips(w, scope, inputs.Rand)

			w.Environment.LastRotationAt = now
			gen := w.RotationFlipGen.Add(1)
			for i := range flips {
				flips[i].Gen = gen
				flips[i].Domain = FlipDomainRotation
			}

			scheduleFlips(w, flips)

			// Defensive copy of ExcludedTags so subscribers can't accidentally
			// mutate the caller's slice (event payloads are by-ref via the
			// pointer-only Event interface; sharing the caller's slice would
			// leak mutations across subscriber boundaries).
			excluded := append([]string(nil), scope.ExcludeTags...)
			w.emit(&RotationApplied{
				At:              now,
				Gen:             gen,
				ObjectsAffected: len(flips),
				ExcludedTags:    excluded,
			})

			log.Printf("sim/world_rotation: applied at %s (gen %d, %d flips scheduled, scope=%+v)",
				now.Format(time.RFC3339), gen, len(flips), scope)

			return RotationResult{
				At:              now,
				Gen:             gen,
				ObjectsAffected: len(flips),
			}, nil
		},
	}
}

// determineRotationFlips computes the per-object state changes needed
// for one rotation pass. Walks World.VillageObjects, filters to objects
// currently in a "rotatable"-tagged state matching scope, then picks
// the next state per asset's RotationAlgo.
//
// For RotationAlgoRandomPerAsset, the per-asset target is decided once
// over the FULL candidate set: pick a pool state that no candidate
// instance currently occupies (so all instances flip). When every pool
// state is already occupied by some instance, fall back to any random
// pool state — convergence still happens (instances at the target are
// no-op flips, instances elsewhere flip to it).
//
// This is a v2-improved semantic vs v1's "decide target from the first
// instance encountered" (which produced non-deterministic targets based
// on map iteration order and could undercount ObjectsAffected when a
// late-encountered instance was already at the memo'd target). See
// code_review R1 finding 1 in the world-rotation codebase note.
//
// Unexported by design (matches determineTransitionFlips). Tests reach
// it via DetermineRotationFlipsForTest in export_test.go.
//
// MUST be called from inside a Command.Fn — reads w.VillageObjects /
// w.Assets directly.
//
// Stable order is NOT guaranteed (Go map iteration is randomized).
// Callers that need a deterministic flip order must sort the result.
// scheduleFlips doesn't care about order.
func determineRotationFlips(w *World, scope RotationScope, r *rand.Rand) []PendingFlip {
	if r == nil {
		return nil
	}

	// Pass 1: collect the candidate set and (for random_per_asset assets)
	// the per-asset set of current states. The two-pass shape is what
	// makes "all instances flip" actually true for random_per_asset:
	// pick a target that no candidate currently occupies.
	type candidate struct {
		id    VillageObjectID
		asset *Asset
		obj   *VillageObject
		pool  []*AssetState
		algo  string
	}
	var candidates []candidate
	perAssetCurrents := map[AssetID]map[string]struct{}{}

	for id, obj := range w.VillageObjects {
		if obj == nil {
			continue
		}
		asset, ok := w.Assets[obj.AssetID]
		if !ok {
			continue
		}
		if asset.RotationAlgo == "" {
			// Asset opts out of rotation entirely — even if a state is
			// tagged rotatable. Defensive against config drift.
			continue
		}
		current := asset.FindState(obj.CurrentState)
		if current == nil || !current.HasTag(TagRotatable) {
			continue
		}
		if scope.Tag != "" && !current.HasTag(scope.Tag) {
			continue
		}
		if excludedByScope(current, scope.ExcludeTags) {
			continue
		}
		pool := asset.RotatablePool()
		if len(pool) == 0 {
			continue
		}
		candidates = append(candidates, candidate{
			id:    id,
			asset: asset,
			obj:   obj,
			pool:  pool,
			algo:  asset.RotationAlgo,
		})
		if asset.RotationAlgo == RotationAlgoRandomPerAsset {
			set, ok := perAssetCurrents[obj.AssetID]
			if !ok {
				set = map[string]struct{}{}
				perAssetCurrents[obj.AssetID] = set
			}
			set[obj.CurrentState] = struct{}{}
		}
	}

	// Pre-compute per-asset targets for RotationAlgoRandomPerAsset over
	// the full candidate set. Pick a pool state that no candidate currently
	// occupies, if any exist; otherwise pick any random pool member (every
	// pool state is occupied by some candidate; convergence still happens).
	assetTarget := map[AssetID]string{}
	for assetID, currents := range perAssetCurrents {
		// Find the canonical pool for this asset from one of the candidates.
		// Every candidate of the same asset shares the same pool slice
		// (asset.RotatablePool returns deterministic content), so reading
		// from any one of them is fine.
		var pool []*AssetState
		for _, c := range candidates {
			if c.obj.AssetID == assetID {
				pool = c.pool
				break
			}
		}
		var nonCurrent []*AssetState
		for _, s := range pool {
			if _, occupied := currents[s.State]; !occupied {
				nonCurrent = append(nonCurrent, s)
			}
		}
		if len(nonCurrent) > 0 {
			assetTarget[assetID] = nonCurrent[r.Intn(len(nonCurrent))].State
		} else if len(pool) > 0 {
			assetTarget[assetID] = pool[r.Intn(len(pool))].State
		}
	}

	// Pass 2: emit flips against the candidate set.
	var flips []PendingFlip
	for _, c := range candidates {
		var nextState string
		switch c.algo {
		case RotationAlgoRandomPerObject:
			nextState = pickRandomExcluding(c.pool, c.obj.CurrentState, r)
		case RotationAlgoRandomPerAsset:
			nextState = assetTarget[c.obj.AssetID]
		case RotationAlgoDeterministic:
			nextState = pickDeterministicNext(c.pool, c.obj.CurrentState)
		default:
			log.Printf("sim/world_rotation: unknown RotationAlgo %q for asset %s — skipping",
				c.algo, c.obj.AssetID)
			continue
		}

		if nextState == c.obj.CurrentState {
			continue
		}
		flips = append(flips, PendingFlip{
			ObjectID:      c.id,
			NewState:      nextState,
			SpreadSeconds: c.asset.TransitionSpreadSeconds,
		})
	}
	return flips
}

// excludedByScope reports whether state carries any of the excludeTags.
// Empty / nil excludeTags → never excluded. Nil state → never excluded
// (defensive; production callers always pass non-nil but the helper is
// exported via export_test.go).
func excludedByScope(state *AssetState, excludeTags []string) bool {
	if state == nil || len(excludeTags) == 0 {
		return false
	}
	for _, tag := range excludeTags {
		if state.HasTag(tag) {
			return true
		}
	}
	return false
}

// pickRandomExcluding returns a random state name from pool other than
// current. Falls back to pool[0] when the pool has a single member
// (no alternative to current is available).
func pickRandomExcluding(pool []*AssetState, current string, r *rand.Rand) string {
	if len(pool) == 0 {
		return current
	}
	if len(pool) == 1 {
		return pool[0].State
	}
	// At most N retries — bounded so a degenerate pool (every state ==
	// current somehow) can't loop forever. Pool entries above pool size
	// would only matter if current isn't in the pool (covered below).
	for attempt := 0; attempt < len(pool)*4; attempt++ {
		idx := r.Intn(len(pool))
		if pool[idx].State != current {
			return pool[idx].State
		}
	}
	// Fallback — pick the first non-current state, or pool[0] if all
	// match (impossible given the len == 1 short-circuit above).
	for _, s := range pool {
		if s.State != current {
			return s.State
		}
	}
	return pool[0].State
}

// pickDeterministicNext returns the pool entry after current, wrapping
// past the last. If current isn't in the pool (unexpected — caller
// already filtered to states with the rotatable tag), returns pool[0].
func pickDeterministicNext(pool []*AssetState, current string) string {
	if len(pool) == 0 {
		return current
	}
	for i, s := range pool {
		if s.State == current {
			return pool[(i+1)%len(pool)].State
		}
	}
	return pool[0].State
}

// RunRotationTicker owns the rotation-boundary ticker goroutine. Wakes
// every RotationTickerInterval, computes the most recent daily boundary,
// and submits an ApplyDailyRotation command if the boundary hasn't been
// processed (LastRotationAt before boundary).
//
// Caller starts this in a goroutine alongside World.Run + RunPhaseTicker.
// Returns when ctx is cancelled.
//
// scope is the rotation scope for ticker-driven passes. Production wires
// ExcludeTags: {TagLaundry, TagNoticeBoard} (ZBBS-HOME-443, cmd/engine
// startTickers) — the npc_behaviors cutover carve-out: the washerwoman /
// town-crier route dispatches fire on RotationApplied only when their
// domain tag is excluded from the bulk pass, and they flip those objects
// stop-by-stop instead. An empty scope bulk-rotates everything and
// leaves the routes dormant (the pre-443 production state, which kept
// the crier from ever walking).
//
// A per-driver *rand.Rand is seeded once at goroutine entry from
// time.Now().UnixNano() and threaded through every ApplyDailyRotation
// call. Production tuning happens via the WorldSettings.RotationTime
// field + restart, not hot-reload.
func RunRotationTicker(ctx context.Context, w *World, scope RotationScope) {
	t := time.NewTicker(RotationTickerInterval)
	defer t.Stop()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Immediate-first-check: if the world just loaded and we're already
	// past today's boundary with an old LastRotationAt, catch it on the
	// first tick rather than waiting up to RotationTickerInterval.
	checkAndRotate(ctx, w, r, scope)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.beatTicker("rotation")
			checkAndRotate(ctx, w, r, scope)
		}
	}
}

// checkAndRotate is the per-tick body. Reads settings + LastRotationAt
// via a SendContext (since the ticker runs off the world goroutine),
// computes today's most recent boundary, fires ApplyDailyRotation when
// LastRotationAt sits before it.
func checkAndRotate(ctx context.Context, w *World, r *rand.Rand, scope RotationScope) {
	if ctx.Err() != nil {
		return
	}
	res, err := w.SendContext(ctx, Command{Fn: func(world *World) (any, error) {
		// Resolve rotation HH:MM. WorldSettings.RotationTime defaults to
		// "00:00" via the environment loader; defensive fallback here so
		// a misconfigured / unset value doesn't busy-fire every minute.
		spec := world.Settings.RotationTime
		if spec == "" {
			spec = DefaultRotationTime
		}
		hour, minute, parseErr := ParseHM(spec)
		if parseErr != nil {
			return nil, parseErr
		}
		loc := world.Settings.Location
		if loc == nil {
			loc = time.Local
		}
		now := time.Now().In(loc)
		boundary := MostRecentRotationBoundary(now, hour, minute)
		if !world.Environment.LastRotationAt.Before(boundary) {
			return nil, nil // already processed
		}
		return boundary, nil
	}})
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: ticker check: %v", err)
		}
		return
	}
	if res == nil {
		return
	}
	// boundary returned — boundary processing not yet done. Stamp the
	// boundary itself (not wall-clock execution time) so a force-rotate
	// invocation later in the day can't accidentally land a stamp earlier
	// than today's boundary and re-fire on the next ticker. Code_review
	// R1 finding 3: "if LastRotationAt is meant to mean 'boundary
	// processed,' then ticker should stamp/use the boundary." Adopting.
	boundary := res.(time.Time)
	if _, err := w.SendContext(ctx, ApplyDailyRotation(
		RotationTickInputs{Now: boundary, Rand: r},
		scope,
	)); err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: apply: %v", err)
		}
	}
	// Farm upkeep wealth tax (LLM-215): once per game-day, wear out farms' upkeep
	// shovels and wake owners to re-buy from the smith. Bound to the daily boundary
	// crossing HERE — not inside ApplyDailyRotation — so the umbilical force-rotate
	// (which calls ApplyDailyRotation directly for state flips) never levies the
	// tax. Keyed to the same durable boundary as the rotation, so it inherits the
	// once-per-day + restart-idempotent guarantee. A no-op when disabled.
	if _, err := w.SendContext(ctx, ApplyFarmUpkeep(boundary)); err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: farm upkeep: %v", err)
		}
	}
	// Town rate (LLM-557): once per game-day, add the day's coin to what each owned
	// business owes the constable. Bound to the daily boundary crossing HERE, beside
	// the farm-upkeep levy and for the same reasons — the umbilical force-rotate
	// (which calls ApplyDailyRotation directly for state flips) must never levy the
	// rate, and keying to the durable boundary inherits the once-per-day +
	// restart-idempotent guarantee. Moves no coin; a no-op when disabled.
	if _, err := w.SendContext(ctx, ApplyTownRate(boundary)); err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: town rate: %v", err)
		}
	}
	// Estate rate (LLM-652): once per game-day, move each resident purse's share
	// of coin above the floor into the town chest. Bound to the daily boundary
	// crossing HERE beside the other two levies and for the same reasons — the
	// umbilical force-rotate must never assess it, and keying to the durable
	// boundary inherits the once-per-day + restart-idempotent guarantee. This one
	// DOES move coin (see estate_rate.go); a no-op when disabled.
	if _, err := w.SendContext(ctx, ApplyEstateRate(boundary)); err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: estate rate: %v", err)
		}
	}
	// Coin record (LLM-572): drop pairs whose payments have all aged out of the
	// recall window. Reclamation only — it changes nothing an actor can perceive,
	// since reads already apply the window. Bound here beside the levies because
	// the daily boundary is a convenient once-a-day seam, not because the sweep is
	// day-sensitive: it is idempotent and safe to run at any cadence.
	if _, err := w.SendContext(ctx, SweepCoinRecord(boundary)); err != nil {
		if ctx.Err() == nil {
			log.Printf("sim/world_rotation: coin record sweep: %v", err)
		}
	}
}
