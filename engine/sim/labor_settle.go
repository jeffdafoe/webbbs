package sim

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"time"
)

// labor_settle.go — LLM-26 background lifecycle for LaborOffers. One sweep,
// three jobs:
//
//  1. Expire PENDING offers past ExpiresAt (the employer never answered).
//  2. Void EN_ROUTE offers past EnRouteDeadline — a hired worker who never
//     reached the post with the owner present (walk deadlock, or an owner who
//     never showed) is freed and the offer resolves FailedUnavailable unpaid;
//     no work happened, so nothing moves and no relationship facts are written
//     (LLM-229). The bounded-wait backstop the start-on-arrival model needs:
//     WorkingUntil is nil until work starts, so the completion sweep (job 3)
//     can never fire on an offer stuck EnRoute.
//  3. Complete WORKING offers past WorkingUntil — transfer the reward from
//     the employer to the worker, clear the worker's StateLaboring/
//     LaboringUntil mirror, and finalize Completed. If the employer can no
//     longer cover the reward, resolve FailedUnavailable instead (the
//     finished work goes unpaid).
//
// then reap terminal offers past the retention window. This is the labor
// analog of pay_ledger_sweep.go and shares its exact coalesced AfterFunc
// self-rearm scheduling; the only difference is the Working→Completed
// settlement, which the pay machine has no equivalent of (pay settles
// atomically at accept, labor at completion).
//
// AcceptWork also drives an in-band Expired flip when a pending offer is
// accepted past its TTL (gate 5). The sweep is the backstop for offers
// nobody answers AND the sole driver of completion for accepted work —
// without it an accepted job would never pay out.

// effectiveLaborLedgerSweepCadence returns the labor sweep cadence. No
// WorldSettings override is plumbed in the MVP, so this is the constant.
func effectiveLaborLedgerSweepCadence() time.Duration {
	return LaborLedgerSweepCadenceDefault
}

// RunLaborLedgerSweep owns the labor-ledger sweep's periodic schedule.
// Caller starts this in a goroutine alongside World.Run (next to
// RunPayLedgerSweep); returns when ctx is cancelled. The first sweep is
// kicked immediately so an offer already past its window at startup doesn't
// wait a full cadence interval.
func RunLaborLedgerSweep(ctx context.Context, w *World) {
	_, err := w.SendContext(ctx, kickLaborLedgerSweep())
	if err != nil && ctx.Err() == nil {
		log.Printf("sim/labor: initial sweep arm failed: %v", err)
	}
	<-ctx.Done()
}

// kickLaborLedgerSweep returns a Command whose Fn arms the first sweep on
// the world goroutine — mirrors kickPayLedgerSweep.
func kickLaborLedgerSweep() Command {
	return Command{
		Fn: func(w *World) (any, error) {
			armNextLaborLedgerSweep(w)
			return nil, nil
		},
	}
}

// armNextLaborLedgerSweep schedules the next sweep after one cadence
// interval. MUST be called from inside a Command.Fn — touches
// w.laborLedgerSweep.scheduled without coordination. Coalescing: no-op when
// a sweep is already scheduled.
func armNextLaborLedgerSweep(w *World) {
	if w.laborLedgerSweep.scheduled {
		return
	}
	w.laborLedgerSweep.scheduled = true
	cadence := effectiveLaborLedgerSweepCadence()
	time.AfterFunc(cadence, func() { fireScheduledLaborLedgerSweep(w) })
}

// fireScheduledLaborLedgerSweep is the AfterFunc callback body. Uses
// LifecycleContext so a shutdown-while-armed unblocks SendContext instead of
// deadlocking on a send to a dead cmds channel. Mirrors
// fireScheduledPayLedgerSweep.
func fireScheduledLaborLedgerSweep(w *World) {
	ctx := w.LifecycleContext()
	if ctx.Err() != nil {
		return
	}
	w.beatTicker("labor_ledger_sweep")
	_, err := w.SendContext(ctx, evaluateLaborLedgerAndRearm(time.Now()))
	if err != nil && ctx.Err() == nil {
		log.Printf("sim/labor: scheduled sweep failed: %v", err)
	}
}

// evaluateLaborLedgerAndRearm clears the scheduled flag, runs one sweep, and
// re-arms — all in one Fn on the world goroutine. Clearing the flag first
// means the re-arm starts a fresh chain rather than no-opping.
func evaluateLaborLedgerAndRearm(now time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			w.laborLedgerSweep.scheduled = false
			res, err := EvaluateLaborLedgerSweep(now).Fn(w)
			armNextLaborLedgerSweep(w)
			return res, err
		},
	}
}

// EvaluateLaborLedgerSweep returns a Command that, in one pass: flips every
// pending offer past ExpiresAt to Expired; settles every working offer past
// WorkingUntil to Completed (crediting the worker); then reaps terminal
// offers past the retention window. Exposed as a Command (not just an
// internal Fn) so tests can drive sweeps deterministically without the
// AfterFunc timing chain.
//
// Ids are collected before any mutation and processed in sorted order so
// LaborResolved events emit stably — w.emit dispatches subscribers
// synchronously, and a subscriber could in principle mutate the ledger map,
// so iterating it while mutating is unsafe even on the single world
// goroutine.
func EvaluateLaborLedgerSweep(now time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if len(w.LaborLedger) == 0 {
				return nil, nil
			}
			var expired, enRouteExpired, completed []LaborID
			for id, o := range w.LaborLedger {
				if o == nil {
					continue
				}
				switch o.State {
				case LaborStatePending:
					if o.ExpiresAt.IsZero() || now.Before(o.ExpiresAt) {
						continue
					}
					expired = append(expired, id)
				case LaborStateEnRoute:
					if o.EnRouteDeadline.IsZero() || now.Before(o.EnRouteDeadline) {
						continue
					}
					enRouteExpired = append(enRouteExpired, id)
				case LaborStateWorking:
					if o.WorkingUntil == nil || now.Before(*o.WorkingUntil) {
						continue
					}
					completed = append(completed, id)
				}
			}
			sort.Slice(expired, func(i, j int) bool { return expired[i] < expired[j] })
			sort.Slice(enRouteExpired, func(i, j int) bool { return enRouteExpired[i] < enRouteExpired[j] })
			sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
			for _, id := range expired {
				o, ok := w.LaborLedger[id]
				if !ok || o == nil || o.State != LaborStatePending {
					continue
				}
				finalizeLaborTerminal(w, o, LaborTerminalStateExpired, false, now)
			}
			for _, id := range enRouteExpired {
				o, ok := w.LaborLedger[id]
				if !ok || o == nil || o.State != LaborStateEnRoute {
					continue
				}
				// LLM-229 bounded-wait backstop: the worker never reached the post
				// with the owner present before EnRouteDeadline. No work happened,
				// so void unpaid (workPerformed=false → no relationship facts,
				// matching the accept-time never-started path). There is no worker
				// mirror to clear — an EnRoute offer never set StateLaboring /
				// LaboringUntil (that only happens on the flip to Working).
				finalizeLaborTerminal(w, o, LaborTerminalStateFailedUnavailable, false, now)
			}
			for _, id := range completed {
				o, ok := w.LaborLedger[id]
				if !ok || o == nil || o.State != LaborStateWorking {
					continue
				}
				settleCompletedLabor(w, o, now)
			}
			reapTerminalLaborOffers(w, now)
			return nil, nil
		},
	}
}

// settleCompletedLabor settles a finished work window: it frees the worker
// (clears StateLaboring/LaboringUntil), then transfers the reward from the
// employer to the worker and finalizes Completed. Caller guarantees
// offer.State == Working and now is at or past *offer.WorkingUntil.
//
// Nothing was held during the window — no coins and no goods (LLM-225's
// in-kind leg is deliberately NOT escrowed either: the ledger is
// restart-lossy, so items held against a row a restart destroys would
// vanish with it). The transfer happens HERE, at completion
// (settle-at-completion, not escrow-at-accept), so the payment is
// re-checked authoritatively now: the employer's coins AND promised goods
// can have drifted across a long window. If the employer is gone, can no
// longer cover either leg of the reward, or paying would overflow the
// worker's purse or pack, the deal falls through unpaid — terminal
// FailedUnavailable, nothing moves. The worker already did the work; that
// is the risk of working for someone who turns out unable to pay. Either
// way the worker is freed first (the work IS finished regardless of
// whether payment lands).
func settleCompletedLabor(w *World, offer *LaborOffer, now time.Time) {
	worker := w.Actors[offer.WorkerID]
	employer := w.Actors[offer.EmployerID]

	// Free the worker regardless of the payment outcome — but ONLY if the
	// worker's ownership key still points at THIS offer. AcceptWork's ledger-
	// based busy-gate already makes two concurrent Working offers per worker
	// unreachable, so in practice this always matches; the id guard is
	// defense-in-depth so settling a stale offer can never free a worker who
	// has since taken a different job — a true ownership check, not a window-
	// timestamp proxy (code_review). StateLaboring is always paired with a
	// non-zero LaborID; with it cleared the actor returns to idle and the next
	// tick's observers re-derive conversing if still huddled.
	if worker != nil && worker.LaborID == offer.ID {
		worker.LaborID = 0
		worker.LaboringUntil = nil
		if worker.State == StateLaboring {
			worker.State = StateIdle
		}
	}

	// Authoritative payment re-check, both legs (LLM-225): coins + the
	// in-kind goods promised (employerCanCoverLaborReward — the same
	// predicate the solicit auto-decline and accept gate 8 use), plus the
	// worker-side receive-overflow guards. All-or-nothing: a shortfall on
	// EITHER leg resolves the whole reward unpaid (failed_unavailable,
	// WorkPerformed=true → the LLM-165 stiffed-worker facts) rather than
	// part-paying — a partial wage is a new ambiguity the fiction then has
	// to explain, and the unpaid path already narrates cleanly.
	canPay := worker != nil &&
		employer != nil &&
		employerCanCoverLaborReward(w, employer, offer) &&
		worker.Coins <= math.MaxInt-offer.Reward
	if canPay {
		for _, ri := range offer.RewardItems {
			if worker.Inventory[ri.Kind] > math.MaxInt-ri.Qty {
				canPay = false
				break
			}
		}
	}
	if !canPay {
		if employer == nil || worker == nil {
			log.Printf("sim/labor: completion of offer %d found worker/employer missing — resolving unpaid", offer.ID)
		}
		finalizeLaborTerminal(w, offer, LaborTerminalStateFailedUnavailable, true, now)
		return
	}

	// Atomic transfer: the employer pays the worker now — coins and goods
	// together. Every leg was validated above (holdings + overflow), so the
	// applies below cannot fail mid-way. RewardItems kinds are unique by
	// construction (resolvePayItems rejects duplicate canonical kinds at
	// solicit), so a plain per-line move is safe; the employer side deletes
	// on zero to keep inventories sparse (the commitPayTransfer convention).
	employer.Coins -= offer.Reward
	worker.Coins += offer.Reward
	// LLM-644: a visitor employer's wage draws his trip budget like any other
	// coin he pays out (canPay above asked employerCanCoverLaborReward →
	// buyerCanAfford, which already compared against it).
	drawVisitorSpend(employer, offer.Reward)
	for _, ri := range offer.RewardItems {
		if remaining := employer.Inventory[ri.Kind] - ri.Qty; remaining > 0 {
			employer.Inventory[ri.Kind] = remaining
		} else {
			delete(employer.Inventory, ri.Kind)
		}
		if worker.Inventory == nil {
			worker.Inventory = make(map[ItemKind]int)
		}
		worker.Inventory[ri.Kind] += ri.Qty
	}
	// finalizeLaborTerminal emits LaborResolved(Completed); its
	// handlers/labor_settle_reactor.go subscriber stamps the LLM-498 settle
	// warrant on BOTH parties, so neither perceives the paid job as still owed
	// (before it, a mid-shift settle was invisible to both sides and the
	// employer would pay a second time on the worker's "shall I settle up?").
	finalizeLaborTerminal(w, offer, LaborTerminalStateCompleted, true, now)
	// LLM-190: if the job ran up to the keeper's closing time (the employer is an
	// establishment keeper now off shift), the keeper ALSO announces the close-out
	// aloud — "we're shut, your work's done, here's your pay" — the social beat on
	// top of the settle warrant's self-perception line.
	announceLaborCloseoutIfShopClosed(w, employer, worker, formatPayment(offer.Reward, offer.RewardItems), now)
}

// LaborSettledWorkerNarration composes the worker-side settle beat for a paid
// labor completion (LLM-498): the wage arrived, named counterparty and exact
// quantities verbatim (formatPayment), so the worker's next turn reads the job
// as squared instead of role-playing being owed. Pre-rendered at the
// LaborResolved subscriber — the ProductionCompletionNarration posture.
// Pronoun-free on purpose: the engine doesn't know the employer's pronouns.
func LaborSettledWorkerNarration(employerName string, reward int, rewardItems []ItemKindQty) string {
	if employerName == "" {
		employerName = "your employer"
	}
	return fmt.Sprintf("Your work for %s is done — you've been paid %s, as agreed.",
		employerName, formatPayment(reward, rewardItems))
}

// LaborSettledEmployerNarration is the employer-side twin: the wage has left
// their purse, so their next turn sees the payment already made — closing the
// double-pay loop (the live Elizabeth Ellis pay-again) at its source.
func LaborSettledEmployerNarration(workerName string, reward int, rewardItems []ItemKindQty) string {
	if workerName == "" {
		workerName = "Your hired worker"
	}
	return fmt.Sprintf("%s's work for you is done — you've paid %s for it, as agreed.",
		workerName, formatPayment(reward, rewardItems))
}

// rehydrateLaborContractsOnLoad loads the durable accepted-contract mirror
// (LLM-259) into World.LaborLedger and re-establishes the transient state a
// restart would otherwise lose. MUST run from FinalizeLoad BEFORE the
// reconcileStrandedLaboringOnLoad pass, so a resumed worker isn't reverted to
// idle. World-goroutine-only (FinalizeLoad runs before Run starts).
//
// Only en_route/working offers are ever persisted (SaveWorld filters at build
// time). A loaded row that isn't a usable accepted contract — non-resumable
// state, empty or dangling worker/employer ref, a bad reward/duration/reward
// item, a `working` contract whose worker didn't reload StateLaboring, or one of
// SEVERAL accepted contracts for the same worker (the one-live-job invariant) —
// is dropped LOUDLY (per-row + aggregate log), NOT added to the live ledger. Such a
// row never arises from a consistent checkpoint (actor + contract are written in
// the SAME SaveWorld Tx, so they agree by construction); it means a manual /
// out-of-band edit (e.g. an actor deleted mid-job without its contract cleaned).
// A live village must still boot, and a dropped contract is data-clean (no coins
// move — settle-at-completion), so warn-and-drop, matching the LoadWorld
// dropStructureBoundOrphanScenes posture for out-of-band-mutable refs. The
// dropped row is swept from the table on the next checkpoint (absent from the
// ledger → absent from the snapshot).
//
// For each row (kept or dropped):
//
//   - floor laborLedgerSeq to the max loaded LaborID, so the next SolicitWork
//     mints a fresh id. The ledger id is transient — NOT a checkpointed actor
//     field — so without this the counter restarts at 0 and a new offer could
//     reuse a loaded id and clobber its row at the next checkpoint. Mirrors the
//     quoteSeq / payLedgerSeq safety floors in FinalizeLoad.
//
// For each KEPT row:
//
//   - a `working` contract restores the transient worker mirror LaborID +
//     LaboringUntil (from WorkingUntil). State is already StateLaboring (the
//     usable-check proved it), so it is not re-forced; the completion sweep then
//     settles the job normally at WorkingUntil.
//
//   - an `en_route` contract needs no mirror (en_route never sets StateLaboring):
//     the worker's walk to the post resumes via the boot ResumeCheckpointedWalks
//     sweep, and the resulting ActorArrived advances the rehydrated offer through
//     handleLaborArrivalOnArrival. A worker already waiting at the post
//     (EnRouteWaiting) resumes on the owner's arrival, or the bounded-wait
//     backstop voids the offer past EnRouteDeadline — same as the live path.
func (w *World) rehydrateLaborContractsOnLoad(ctx context.Context) error {
	contracts, err := w.repo.LaborContracts.LoadAll(ctx)
	if err != nil {
		return err
	}
	var dropped int
	// Pass 1: floor the LaborID allocator for every loaded row (even a dropped one,
	// so a post-restart mint never reuses a persisted id), drop the per-row-unusable
	// ones loudly, and collect the survivors + count them per worker. The
	// one-live-job-per-worker invariant (workerHasLiveJob gates accept) is a
	// cross-row property, so it's checked after per-row validity.
	survivors := make([]*LaborOffer, 0, len(contracts))
	acceptedPerWorker := make(map[ActorID]int)
	for id, offer := range contracts {
		if offer == nil {
			continue
		}
		if uint64(id) > w.laborLedgerSeq {
			w.laborLedgerSeq = uint64(id)
		}
		if reason := w.unusableLaborContract(id, offer); reason != "" {
			log.Printf("sim: rehydrate labor contract %d (worker=%q employer=%q state=%q): %s — dropping", id, offer.WorkerID, offer.EmployerID, offer.State, reason)
			dropped++
			continue
		}
		survivors = append(survivors, offer)
		acceptedPerWorker[offer.WorkerID]++
	}
	// Pass 2: add the survivors, but drop ALL of a worker's contracts if it has more
	// than one accepted (en_route/working) — a worker can't be laboring two jobs at
	// once. Dropping every conflicting one (rather than arbitrarily resuming the
	// "first", which is map-order-nondeterministic) is deterministic and refuses to
	// resume the wrong job; the worker falls to reconcileStrandedLaboringOnLoad and
	// re-enters the labor market cleanly.
	for _, offer := range survivors {
		if acceptedPerWorker[offer.WorkerID] > 1 {
			log.Printf("sim: rehydrate labor contract %d: worker %q has %d accepted contracts (one-live-job-per-worker invariant violated) — dropping all", offer.ID, offer.WorkerID, acceptedPerWorker[offer.WorkerID])
			dropped++
			continue
		}
		w.LaborLedger[offer.ID] = offer
		if offer.State != LaborStateWorking {
			continue
		}
		worker := w.Actors[offer.WorkerID]
		worker.LaborID = offer.ID
		if offer.WorkingUntil != nil {
			until := *offer.WorkingUntil
			worker.LaboringUntil = &until
		} else {
			worker.LaboringUntil = nil
		}
	}
	if dropped > 0 {
		log.Printf("sim: rehydrate labor contracts: dropped %d unusable/conflicting contract(s) — see per-row logs above", dropped)
	}
	return nil
}

// unusableLaborContract returns a non-empty reason string if a loaded contract
// cannot be resumed and must be dropped, or "" if it is a usable accepted
// contract. World-goroutine-only (reads w.Actors). See
// rehydrateLaborContractsOnLoad for the warn-and-drop rationale.
func (w *World) unusableLaborContract(id LaborID, o *LaborOffer) string {
	if o.ID != id {
		return fmt.Sprintf("map key %d != offer.ID %d (loader inconsistency)", id, o.ID)
	}
	if o.State != LaborStateEnRoute && o.State != LaborStateWorking {
		return fmt.Sprintf("non-resumable state %q", o.State)
	}
	if o.WorkerID == "" || o.EmployerID == "" {
		return "empty worker/employer id"
	}
	worker := w.Actors[o.WorkerID]
	if worker == nil {
		return "worker missing from loaded actors"
	}
	if _, ok := w.Actors[o.EmployerID]; !ok {
		return "employer missing from loaded actors"
	}
	if o.Reward < 0 {
		return fmt.Sprintf("negative reward %d", o.Reward)
	}
	if o.DurationMin <= 0 {
		return fmt.Sprintf("non-positive duration_min %d", o.DurationMin)
	}
	for _, ri := range o.RewardItems {
		if ri.Kind == "" || ri.Qty <= 0 {
			return fmt.Sprintf("invalid reward item {kind:%q qty:%d}", ri.Kind, ri.Qty)
		}
	}
	// A working contract's worker MUST have reloaded StateLaboring — actor +
	// contract checkpoint atomically in one Tx. A disagreement means the actor was
	// edited out-of-band without cleaning the contract; drop the stale obligation
	// rather than force the actor back to laboring.
	if o.State == LaborStateWorking && worker.State != StateLaboring {
		return fmt.Sprintf("working but worker loaded as %q not laboring (actor/contract disagreement)", worker.State)
	}
	return ""
}

// reconcileStrandedLaboringOnLoad frees an actor checkpointed mid-job whose
// backing contract did NOT survive the restart (the genuine orphan). Actor.State
// IS persisted (sim_state), so a worker in StateLaboring at checkpoint reloads
// still laboring — but its LaborOffer only survives if it was an accepted
// (en_route/working) contract that rehydrateLaborContractsOnLoad just restored
// into World.LaborLedger (LLM-259). The only path that clears StateLaboring is
// the completion sweep, which settles off the ledger; a laboring actor with NO
// live ledger job would never be freed and would sit occupied forever, so it is
// reverted to idle here.
//
// Because rehydrate runs first, the check is now conditional: a worker whose
// working contract loaded holds a live ledger job (workerHasLiveJob) and RESUMES
// — untouched here; only a laboring actor with no restored contract is stranded
// and reset. No coins are touched: the reward only ever moves at completion, so
// there is no torn coin state to recover. Idempotent; world-goroutine-only
// (called from FinalizeLoad, after rehydrateLaborContractsOnLoad).
func reconcileStrandedLaboringOnLoad(w *World, a *Actor) {
	if a == nil || a.State != StateLaboring {
		return
	}
	if workerHasLiveJob(w, a.ID) {
		return // a rehydrated working contract — resumes, not stranded
	}
	a.State = StateIdle
	a.LaborID = 0
	a.LaboringUntil = nil
}

// reapTerminalLaborOffers removes terminal LaborOffers from World.LaborLedger
// once they are older than LaborLedgerTerminalRetentionDefault (measured from
// ResolvedAt). This bounds the offer-side map: terminal offers
// (completed / declined / expired / failed_unavailable) are otherwise never
// removed, and the map is their sole home (restart-lossy, no checkpoint, no
// sink). The active states (pending, working) are skipped; an offer with a
// nil ResolvedAt is skipped defensively (a terminal offer should always
// carry one). MUST be called from inside a Command.Fn (world goroutine).
func reapTerminalLaborOffers(w *World, now time.Time) {
	if w == nil || len(w.LaborLedger) == 0 {
		return
	}
	for id, o := range w.LaborLedger {
		if o == nil {
			delete(w.LaborLedger, id)
			continue
		}
		if o.State == LaborStatePending || o.State == LaborStateWorking {
			continue
		}
		if o.ResolvedAt == nil {
			continue
		}
		if now.Sub(*o.ResolvedAt) > LaborLedgerTerminalRetentionDefault {
			delete(w.LaborLedger, id)
		}
	}
}
