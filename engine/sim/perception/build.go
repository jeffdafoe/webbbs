package perception

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// BuildOption tunes an otherwise-default Build. The zero set of options
// reproduces the turn-start perception; the harness passes options only when
// re-perceiving mid-tick, where a snapshot-derived cue is knowably stale
// relative to a command the actor has already run this tick (LLM-173).
type BuildOption func(*buildOptions)

type buildOptions struct {
	// resolvedPayOffers holds the pay-ledger ids the actor has already answered
	// THIS tick (accept/decline/counter/withdraw). They are withheld from the
	// seller-side "## Offers awaiting your decision" cue.
	resolvedPayOffers map[sim.LedgerID]struct{}
}

// WithResolvedPayOffers withdraws pay offers the actor has already resolved this
// tick from the "## Offers awaiting your decision" cue. The within-tick
// self-state refresh (LLM-88, handlers.RunTick) re-perceives from the turn-start
// snapshot, whose ledger still shows a just-accepted offer as pending — so
// without this the cue re-invites the seller to settle an offer it already
// settled, and the weak model burns its remaining rounds re-accepting into the
// LLM-104 resolvedLedgerThisTick reject. This is the subtractive half of that
// guard: stop showing the cue rather than only rejecting the re-fire after the
// fact. Pay-only by design — accept_work moves no coins at accept, so the
// employer's self-state doesn't change, the refresh never fires, and the labor
// cue is never re-rendered by this path (its re-fire stays guarded by LLM-164).
// A nil/empty set is a no-op, so the turn-start Build is unaffected.
func WithResolvedPayOffers(ids map[sim.LedgerID]struct{}) BuildOption {
	return func(o *buildOptions) {
		o.resolvedPayOffers = ids
	}
}

// Build turns a published snapshot plus an actor's consumed warrant batch
// into a Payload. It is a pure function: it reads only the immutable
// *sim.Snapshot and the passed warrants, mutates nothing, and never
// touches the world goroutine.
//
// The warrants are the batch the reactor evaluator consumed and carried in
// the ReactorTickDue event (handlers copies them onto its tickJob, since
// consume clears them from the live actor). Build takes them directly
// rather than the unexported handlers.tickJob so this package stays free
// of any handlers dependency.
//
// Build is total — it never panics and always returns a usable Payload.
// A nil snapshot or an actor absent from the snapshot yields a degraded
// Payload (empty views, Baseline == BaselineMissingNoScene) with the
// reason recorded in SelectionReason; the harness decides what to do with
// a degraded perception.
func Build(snap *sim.Snapshot, actorID sim.ActorID, warrants []sim.WarrantMeta, opts ...BuildOption) Payload {
	var o buildOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}

	p := Payload{
		ActorID:  actorID,
		Warrants: orderWarrants(warrants),
		Baseline: BaselineMissingNoScene,
	}

	if snap == nil {
		p.SelectionReason = "nil snapshot — no scene resolvable"
		return p
	}

	actorSnap := snap.Actors[actorID]
	if actorSnap == nil {
		p.SelectionReason = fmt.Sprintf("actor %q not in snapshot — no perception context", actorID)
		return p
	}

	// ZBBS-HOME-413: drop any seller-side pay-offer warrant whose ledger entry
	// is no longer pending. The PayOfferWarrant is stamped once (on
	// PayOfferReceived) and only cleared when the seller's next reactor tick
	// consumes it; resolution stamps the BUYER, never the seller's standing
	// warrant. So an offer that resolves out-of-band (buyer withdrew, TTL
	// expired, or the seller was rest-gated and couldn't tick) before the
	// seller consumes it would otherwise render a dead "since your last turn"
	// line. Build runs on the immutable snapshot, so checking live ledger
	// state is race-free here. (Since ZBBS-HOME-453 the decision section and
	// the tool gate no longer read the warrant batch — see PayOffersForMe
	// below — so this filter now only protects the generic warrant render.)
	p.Warrants = filterStalePayOfferWarrants(p.Warrants, snap)

	// LLM-208: drop a lodging (nights_stay) room-quote warrant when the subject
	// already has a home. A homed guest can't take a room — the buyer-side
	// pay_with_item guard rejects it (LLM-182) — so dangling the offer only pulls
	// it into a doomed nightly negotiation. Per-viewer: a homeless seeker in the
	// same scene still perceives a public room quote; only the homed subject is
	// spared it. Complements the seller-side creation gate
	// (scene_quote_commands.go), which can only pre-check a targeted quote.
	p.Warrants = filterHomedLodgingQuoteWarrants(p.Warrants, snap, actorSnap)

	// LLM-276: the seek-work backstop stamps a tend-need warrant (in place of the
	// go-earn one) when a workless idle worker has grown hungry/thirsty and can
	// resolve it now. When present, job-hunting yields to eating exactly as a red
	// need makes it yield — the businesses directory + solicit affordance suppress
	// and the need-redirect steer fires — so the food cues win. Keyed off the
	// warrant (which already carries the sim-side "pressing + resolvable" decision)
	// rather than recomputing the band, so the warrant and the cues can't disagree.
	tendNeedActive := tendNeedWarrantActive(p.Warrants)

	// ZBBS-HOME-453: the standing seller-side offer view, scanned from
	// snap.PayLedger every tick. The warrant above only WAKES the seller's
	// first tick; this scan is what keeps "## Offers awaiting your decision"
	// and the accept/decline/counter tools present until the entry leaves
	// Pending, so a seller who speaks through the warranted tick instead of
	// resolving can still settle the offer on any later tick.
	p.PayOffersForMe = buildPayOffersForMe(snap, actorID, o.resolvedPayOffers)
	// LLM-303: per-offer stock shortfall on the asked good, so renderPayOffers can
	// warn "you hold no/only N <good>" for ANY offeree short of the ask — not just
	// a vendor already carrying some of the kind. Read from the subject's own
	// inventory + the catalog (services excluded); render stays catalog-free.
	p.PayOfferShortfalls = buildPayOfferShortfalls(snap, p.PayOffersForMe, actorSnap)
	p.RoomAlreadySoldOrderByLedger = buildRoomAlreadySold(snap, actorID, p.PayOffersForMe)
	// LLM-598: price a barter offer's two sides against each other, so a seller
	// weighing goods-for-goods reads a verdict instead of being left to multiply
	// the wares cue's per-unit figures himself (live, the miller did not).
	p.PayOfferWorth = buildPayOfferWorth(snap, actorID, p.PayOffersForMe)
	// LLM-138: the recipient-side gift decision view (gifts offered TO me),
	// the gift counterpart to PayOffersForMe. Drives the "## Gifts offered to
	// you" cue + the accept_gift / decline_gift tool gate.
	p.GiftsForMe = buildGiftsForMe(snap, actorID, actorSnap)

	// LLM-26: the standing labor views, scanned from snap.LaborLedger every
	// tick (same posture as PayOffersForMe). LaborOffersForMe is the employer's
	// pending-offer decision view; WorkersForMe is the employer's in-progress
	// jobs (LLM-202); Laboring is the worker's own in-progress job. Built before
	// buildWarrantActorNames so the worker/employer names they reference resolve
	// in render.
	p.LaborOffersForMe = buildLaborOffersForMe(snap, actorID)
	p.SubjectProducesGoods = subjectProducesGoods(snap, actorSnap)
	p.WorkersForMe = buildWorkersForMe(snap, actorID)
	p.Laboring = buildLaboring(snap, actorID)
	p.LaborEnRoute = buildLaborEnRoute(snap, actorID)
	p.PendingLaborOfferOut = buildPendingLaborOfferOut(snap, actorID)

	p.Actor = buildActorView(snap, actorID, actorSnap)
	// LLM-370: when the perceiving actor is itself a transient traveler, carry its
	// persona so Render can open the message with the self-identity preface. Read
	// off the VisitorState the substrate mirrors onto the snapshot.
	p.SelfTraveler = buildTravelerSelf(snap, actorID, actorSnap)
	p.WarrantPlaceNames = buildWarrantPlaceNames(snap, p.Warrants)
	p.WarrantPlaceKeepers = buildWarrantPlaceKeepers(snap, p.Warrants)
	p.EatHereKinds = buildEatHereKinds(snap)
	p.Surroundings = buildSurroundings(snap, actorID, actorSnap)
	// LLM-26: a free worker can solicit work — carries AttrWorker, isn't already
	// laboring, has no pending offer already out (one bid at a time, the mirror
	// of SolicitWork's gate), and has someone SOLICITABLE to offer to. The one
	// signal renders the solicit_work affordance cue AND gates the solicit_work
	// tool. Built after Surroundings so the audience is populated.
	//
	// LLM-145: the final term narrows HasAudience() to hasSolicitableAudience —
	// at least one co-present actor who is NOT a housemate or a co-worker. A
	// broke worker shut in with only its own family (the Walkers all share the
	// Walker Residence) would otherwise be told to bid its kin for coin they
	// don't have; now the affordance hides and the seek-work backstop keeps
	// nudging it toward a real employer. SolicitWork re-checks the resolved
	// employer for defense-in-depth.
	// LLM-194: a coin-comfortable WORKLESS worker is not offered the solicit affordance
	// — it doesn't need odd jobs, so it shouldn't bid a co-present keeper for work. The
	// comfort suppression is SCOPED to workless (no resolvable workplace), matching the
	// warrant gate and the directory gate below — both of which check workless before
	// comfort. Unlike those two, the base CanSolicitWork gate is NOT workless-only (an
	// EMPLOYED worker with a solicitable audience can still bid for a side job), so the
	// ceiling must not silence an employed worker; comfort only gates the workless seeker
	// (code_review). subjectIsComfortable is the shared predicate (mirrors
	// sim.workerIsComfortable on the warrant side).
	comfortableWorklessSeeker := !subjectHasResolvableWorkplace(snap, actorSnap) && subjectIsComfortable(snap, actorSnap)
	// LLM-205 / LLM-353: a worker who has gone to the tavern for the evening is off the
	// clock — don't offer the solicit affordance/tool (this one CanSolicitWork signal
	// gates both). Keyed on tookEveningLeisure (inside the venue, or walking in to it),
	// NOT on inEveningLeisure: LLM-353 dropped the coin gate, so keying on the evening
	// window alone would silence every homed worker regardless of whether he went out.
	// A worker who has not taken the evening — broke, homeless, or simply still standing
	// in the road at dusk — can still bid for work.
	p.CanSolicitWork = subjectIsWorker(actorSnap) &&
		!comfortableWorklessSeeker &&
		!tookEveningLeisure(snap, actorSnap) &&
		// LLM-210: a red need outranks job-hunting (see the SeekWorkPlaces gate below).
		// The two seek-work gates suppress symmetrically, as they do for evening leisure.
		!hasRedNeed(actorSnap, snap) &&
		// LLM-276: a sub-red but pressing-and-resolvable need does too — the seek-work
		// backstop has stamped a tend-need warrant, so eating outranks soliciting work.
		!tendNeedActive &&
		p.Laboring == nil &&
		// LLM-229: a worker relocating to an accepted job is already committed —
		// don't offer the solicit affordance for a second one.
		p.LaborEnRoute == nil &&
		!subjectHasPendingLaborOfferOut(snap, actorID) &&
		// LLM-346: an employer has asked this worker to lend a hand and is waiting
		// on the answer. Soliciting past a job already on the table is the wrong
		// verb — the decision section names the offer and the answer tools.
		!subjectHasLaborOfferToAnswer(snap, actorID) &&
		hasSolicitableAudience(snap, actorID, actorSnap, p.Surroundings)
	// LLM-564: the acquainted subset of that audience, so the solicit cue can name
	// who to ask. Presentation only — the gate above is untouched, and an empty
	// slice under a true CanSolicitWork just renders the unnamed fallback wording.
	if p.CanSolicitWork {
		p.SolicitableEmployers = buildSolicitableEmployers(snap, actorID, actorSnap, p.Surroundings)
	}
	// LLM-346: the hiring-side affordance. A non-empty slice both renders the
	// offer_work cue (renderOfferWorkAffordance) and gates the offer_work tool, so
	// the two cannot drift. Built after Surroundings so the audience is populated,
	// and after CanSolicitWork so the two labor mints stay visibly symmetric.
	p.HireableWorkers = buildHireableWorkers(snap, actorID, actorSnap, p.Surroundings)
	// Resolved last among the name-bearing views, because HireableWorkers is the
	// newest of them and the offer_work cue must render real names — a "someone
	// takes work for pay" line names a target offer_work cannot resolve.
	p.WarrantActorNames = buildWarrantActorNames(snap, actorSnap, actorID, p.Warrants, p.PayOffersForMe, p.LaborOffersForMe, p.WorkersForMe, p.Laboring, p.LaborEnRoute, p.PendingLaborOfferOut, p.HireableWorkers, p.SolicitableEmployers)
	// LLM-160: the businesses directory is a STANDING cue for a workless idle
	// worker with no employer present — not a rare warrant-gated one. Pre-fix it
	// rode only the paced seek-work warrant tick (LLM-152, ~7% of ticks), so on the
	// ordinary conversational ticks that dominate the model had no list of real
	// place names and invented destinations ("the market", "the Well") that move_to
	// can't resolve — it talked about leaving and never left. When a solicitable
	// employer IS present the affordance (CanSolicitWork) already covers "offer your
	// labor to them"; when none is, surface the town's businesses by their
	// resolvable names every tick so move_to actually has a target it can hit.
	//
	// LLM-168: gated on WORKLESS — no RESOLVABLE work_structure_id — not broke
	// (Coins==0). A workless worker has no post to keep, so seeking odd jobs is its
	// only on-shift activity whether or not it is penniless; the brand-new Walker
	// family idled all shift holding a few coins because the old broke gate never
	// surfaced this for them. "Resolvable" matches the duty steer (which keys off
	// anchors.WorkID — set by buildAnchors only for a workplace PRESENT in the
	// snapshot), so the two cues agree: a set-but-dangling WorkStructureID reads as
	// workless here too, not "has a post", avoiding a dead zone where the duty steer
	// can't resolve the target AND seek-work is suppressed. A worker with a resolvable
	// workplace is steered to its post by the duty steer instead.
	//
	// "idle" is load-bearing: a populated SeekWorkPlaces IS the render-side directive
	// bit (it suppresses the owed-reply nag and swaps the triage coda to "go now"), so
	// it must mean the worker is actually free to leave. Exclude an in-flight walk or
	// a mid source-activity — those are their own coda states, and the directive must
	// not fire (or drop the reply nag) while the worker is already committed to one.
	//
	// LLM-194: also gated on NOT comfortable — a workless worker holding coin at/above
	// the seek-work ceiling reads as a plain idle villager (no businesses directory, no
	// "go now" coda), draining its purse via ordinary consumption instead of being
	// pushed to a business. Mirrors the warrant gate (workerIsComfortable) and the
	// CanSolicitWork gate above.
	//
	// LLM-205 / LLM-353: also suppressed once a worker has taken the evening at the tavern
	// (tookEveningLeisure — inside the venue, or walking in to it), mirroring the
	// CanSolicitWork gate. Not keyed on inEveningLeisure: LLM-353 dropped the coin gate, so
	// a worker who has not gone out — penniless, or just still in the road at dusk — keeps
	// the businesses directory.
	if subjectIsWorker(actorSnap) &&
		!subjectHasResolvableWorkplace(snap, actorSnap) &&
		!subjectIsComfortable(snap, actorSnap) &&
		!tookEveningLeisure(snap, actorSnap) &&
		// LLM-210: a red need outranks job-hunting. Suppressing the directory for a
		// red-tired (or red-hungry/thirsty) worker lets the need cue + its remedy
		// already in the perception (the free bed / a food source) win, so the NPC
		// rests or eats on its own — subtractive, mirroring the duty-steer and
		// evening-leisure hasRedNeed gates. The directory resumes the tick it clears.
		!hasRedNeed(actorSnap, snap) &&
		// LLM-276: likewise yield the directory to a sub-red pressing-and-resolvable
		// need (tend-need warrant present) so the eat/drink cue + need-redirect win.
		!tendNeedActive &&
		p.Laboring == nil &&
		// LLM-229: a worker relocating to (or waiting at) an accepted job is
		// committed — don't push the businesses directory at them. The
		// in-flight-move gate already covers the walking leg; this also covers a
		// worker who has arrived and is waiting at the loiter for the owner.
		p.LaborEnRoute == nil &&
		p.Actor.InFlightMove == nil &&
		p.Actor.InFlightSourceActivity == nil &&
		!subjectHasPendingLaborOfferOut(snap, actorID) &&
		// LLM-346: a worker holding an unanswered offer of work has a job in front
		// of them — don't send them across town looking for another.
		!subjectHasLaborOfferToAnswer(snap, actorID) &&
		!hasSolicitableAudience(snap, actorID, actorSnap, p.Surroundings) {
		p.SeekWorkPlaces = buildSeekWorkPlaces(snap, actorSnap)
	}
	p.TurnState = buildTurnState(snap, actorID, actorSnap, p.Surroundings.HuddleMembers)
	p.Anchors = buildAnchors(snap, actorSnap)
	p.NarrativeState = buildNarrativeState(actorSnap)
	p.Businessowner = actorSnap.BusinessownerState != nil
	// AtOwnBusiness narrows Businessowner to "at my own post" — a businessowner is
	// only open for trade while physically at their business structure (the
	// WorkStructureID anchor that renders as "you keep your trade at X"). The vendor
	// cues gate on this, not bare Businessowner, so a keeper who is a customer
	// elsewhere (Prudence pitching Water mid-meal in John Ellis's tavern) isn't told
	// to sell. ZBBS-WORK-385.
	p.AtOwnBusiness = p.Businessowner && actorSnap.WorkStructureID != "" && actorSnap.InsideStructureID == actorSnap.WorkStructureID
	// AtOwnBusinessOperating gates the trade-conduct cue on operating hours, not
	// merely on being at-post (LLM-123): a keeper at its closed stall off-shift was
	// told to "tend to your trade" at midnight, which — fighting its needs-pull to
	// the Tavern — drove the off-shift forge<->Tavern oscillation. keeperOperating
	// is on-shift OR a live stay_open commitment. The customer-facing cues keep
	// gating on bare AtOwnBusiness (location).
	p.AtOwnBusinessOperating = p.AtOwnBusiness && keeperOperating(snap, actorSnap)
	// LLM-413: the concession line inside the trade-conduct block ("meet a willing
	// buyer partway on price") renders only when trade at the post actually IS
	// slow — an engine judgment off the keeper's weekly sell-through, not a model
	// guess. Unconditional, it was a standing licence to discount.
	p.VendorTradeSlow = p.AtOwnBusinessOperating && keeperTradeSlow(snap, actorID, actorSnap)
	// LLM-611: the off-post complement. A keeper away from its own business is
	// there to BUY, and the at-post trade block does not follow it — so nothing
	// told it what the goods in its own pack are worth when it hands them over as
	// payment. Gated on an audience because the rule is about what you give
	// someone: with no one within earshot there is no one to pay, and a standing
	// line on every solo walking turn is noise the whole prompt pays for.
	// WorkStructureID != "" is required, not implied by !AtOwnBusiness: a
	// businessowner with no post configured is not "away from" one, and telling it
	// its goods come off its own shelf names a shelf that does not exist. Silence
	// is the safer reading of a malformed keeper.
	p.KeeperAwayFromPost = p.Businessowner && actorSnap.WorkStructureID != "" && !p.AtOwnBusiness &&
		(len(p.Surroundings.HuddleMembers) > 0 || len(p.Surroundings.CoPresent) > 0)
	heardNow := currentHeardExcerpts(p.Warrants)
	p.Relationships = buildRelationships(actorSnap, p.Surroundings.HuddleMembers, heardNow)
	// LLM-572: the record behind the impression. Built right beside Relationships
	// because it is that section's counterpart — judgment there, counted coin here —
	// but unlike it this covers every actor kind, the stateful NPC included.
	p.CoinDealings = buildCoinDealings(snap, actorID, actorSnap, p.Surroundings.HuddleMembers, snap.PublishedAt)
	// LLM-387: gossip the subject carries about people NOT in the scene, the
	// absent-subject twin of Relationships. Built off Surroundings so present
	// peers can be filtered (no gossiping to someone's face).
	p.VillageWord = buildVillageWord(actorSnap, p.Surroundings, snap.PublishedAt)
	p.RecentConversation, p.ConveyedSpeech = buildRecentConversation(snap, actorID, actorSnap, heardNow)
	p.SelfActions = buildSelfActions(snap, actorID, actorSnap)
	p.OfferableCustomers = buildOfferableCustomers(snap, actorID, p.AtOwnBusiness, p.Surroundings.HuddleMembers, p.Actor.Inventory)
	p.StandingQuotesFromMe = buildStandingQuotesFromMe(snap, actorID, actorSnap)
	// LLM-551: the buyer-side twin, built right after its seller-side mirror.
	// Reads p.EatHereKinds (set above) for the carry-out disposition and
	// p.Warrants (finalized above, after the stale-offer and homed-lodging
	// filters) so a quote announced by THIS tick's warrant isn't also stood up
	// in the section — the warrant line already carries it, verbatim.
	p.StandingQuotesToMe = buildStandingQuotesToMe(snap, actorID, actorSnap, p.EatHereKinds, p.Warrants)
	p.UncoverableOffersFromMe = buildRecentlyShortfallQuotesFromMe(snap, actorID, actorSnap)
	p.PendingDeliveriesFromMe, p.PendingDeliveriesToMe = buildPendingOrderViews(snap, actorID)
	p.PendingOffersFromMe = buildPendingOffersFromMe(snap, actorID, actorSnap)
	p.RecentlyResolvedOffersFromMe = buildRecentlyResolvedOffersFromMe(snap, actorID, actorSnap)
	// LLM-138: the giver-side gift views — own pending gifts (standing) + own
	// recently-settled gifts (resolution) — gift counterparts to the two lines
	// above.
	p.GiftsFromMe = buildGiftsFromMe(snap, actorID, actorSnap)
	p.SettledGiftsFromMe = buildSettledGiftsFromMe(snap, actorID, actorSnap)
	p.CountersAwaitingMyResponse = buildCountersAwaitingMyResponse(snap, actorID, actorSnap)
	p.LocalDateUTC = snap.LocalDateUTC // world "today" for the order-book date split (ZBBS-HOME-403)
	p.RenderedAt = snap.PublishedAt    // render instant for the order-expiry clause (LLM-106)
	p.RecoveryOptions = buildRecoveryOptions(snap, actorID, actorSnap)
	p.Satiation = buildSatiation(snap, actorID, actorSnap)
	p.NeedRedirect = buildNeedRedirect(snap, actorSnap, p.Satiation) // LLM-176: looping-coda redirect; reads p.Satiation
	p.Restocking = buildRestocking(snap, actorID, actorSnap)
	p.ProductionInputs = buildProductionInputs(snap, actorID, actorSnap)
	p.ForgeChoice = buildForgeChoice(snap, actorID, actorSnap)
	// TradeValue (LLM-125): the coin worth of the actor's own-trade goods, shown
	// when it is in a huddle (someone to trade with). Location-independent — a
	// smith knows a nail's worth away from the forge — so it is gated on company,
	// not on AtOwnBusiness like the forge/restock cues.
	p.TradeValue = buildTradeValue(snap, actorID, actorSnap, len(p.Surroundings.HuddleMembers) > 0)
	// LLM-171: the buyer-side producer-awareness sets — the goods the subject makes
	// itself, and the goods it already holds at cap. Render strips the actionable
	// take from a buy-quote whose every line falls in these sets (buying back your
	// own ware / overflowing your carry), so a co-present seller's mis-pitched quote
	// can't close a degenerate loop.
	p.OwnProducedKinds = buildOwnProducedKinds(actorSnap)
	p.AtCapKinds = buildAtCapKinds(actorSnap, p.Actor.Inventory)
	p.StallRepair = buildStallRepair(snap, actorID, actorSnap)
	p.StallCondition = buildStallCondition(snap, actorID, actorSnap)
	p.StallRepairBuy = buildStallRepairBuy(snap, actorID, actorSnap)
	p.Hearth = buildHearth(snap, actorID, actorSnap)
	p.HearthCooking = buildHearthCooking(snap, actorSnap)
	p.FarmUpkeep = buildFarmUpkeep(snap, actorID, actorSnap)
	p.WorkClothes = buildWorkClothes(snap, actorID, actorSnap)
	p.TownRate = buildTownRate(snap, actorID, actorSnap)
	// customerEngaged (LLM-90): the seller-side "someone's at my stall right now"
	// signal — a buyer's pending offer awaiting my decision (PayOffersForMe), a
	// quote I have standing out to a buyer (StandingQuotesFromMe), or simply a
	// co-present companion while I'm at my own post (a live interaction at the
	// stall). buildForage defers the harvest cue on it so a grower finishes the
	// encounter before walking off to her bushes, rather than abandoning someone
	// mid-transaction. The co-presence arm is the raw at-own-post huddle check, NOT
	// p.OfferableCustomers — that view needs goods on hand to fire, so an empty-
	// shelf grower (exactly when the harvest cue triggers) with a customer in front
	// of her would slip through it.
	//
	// The arm counts only members who could BE custom — a peer mid-job is excluded
	// (hasCustomAtPost). That is LLM-231's rule, which buildOfferableCustomers has
	// applied since it shipped: a laboring peer is not a sale target, not even to
	// their own employer. The co-presence arm never asked, so an employer's own
	// hired worker read as a live sale at the stall. Because a contract runs for
	// hours, that was not the brief "finish the encounter" deferral this signal is
	// for — it silenced the harvest cue for the whole shift, and the payload
	// contradicted itself: the same prompt said "X is working a job for you" while
	// this treated X as custom.
	customerEngaged := len(p.PayOffersForMe) > 0 ||
		len(p.StandingQuotesFromMe) > 0 ||
		(p.AtOwnBusiness && hasCustomAtPost(p.Surroundings.HuddleMembers))
	p.Forage = buildForage(snap, actorID, actorSnap, customerEngaged)
	// DutySteer is built AFTER Restocking + Forage (ZBBS-HOME-400 Option B /
	// LLM-90): the return-to-post cue is suppressed while a restock OR forage
	// errand is active, and the at-post stabilizer flips to a step-out line under a
	// forage errand. LLM-620 narrows those two signals from "the cue rendered" to
	// "the cue is ACTIONABLE" — a cue naming no ripe bush and no reachable supplier
	// is a standing want the agent cannot take one step toward, and level-triggering
	// the suppression on it unpinned both ends of the shift indefinitely. That is
	// the same test LLM-277 already applies to hasFarmUpkeepErrand just below.
	// (p.Forage already encodes "not mid-customer" via customerEngaged.)
	// LLM-277 adds the owner supply errands to the suppressor set: an owner who has
	// left her post to buy nails to mend her worn business (p.StallRepairBuy) or
	// shovels the season owes (p.FarmUpkeep) must not be yanked back before she has
	// fetched them — the trip away IS the errand, the same posture the restock/forage
	// errands take. The farm-upkeep half only counts when it has an ACTIONABLE buy
	// path (a co-present seller or a surviving walk-to supplier): buildFarmUpkeep,
	// unlike buildStallRepairBuy, still renders a generic "from the blacksmith"
	// fallback when no supplier resolves (the LLM-216 posture), and suppressing the
	// nag on that dead-end would strand an owner who owes shovels but can't buy them
	// anywhere. buildStallRepairBuy already drops itself in the no-path case, so its
	// mere presence is enough.
	hasFarmUpkeepErrand := p.FarmUpkeep != nil && (p.FarmUpkeep.CoPresentSeller != "" || len(p.FarmUpkeep.ShovelVendors) > 0)
	hasUpkeepErrand := p.StallRepairBuy != nil || hasFarmUpkeepErrand
	p.DutySteer = buildDutySteer(snap, actorID, actorSnap, p.Anchors, p.Restocking.Actionable(), p.Forage.ActionableErrand(), hasUpkeepErrand)
	p.DutyPending = buildDutyPending(snap, actorSnap, p.Anchors)
	// LLM-149 (Lever 2): the evening "tavern's open" cue. Built off the same
	// anchors; on the evening window it replaces the off-shift go-home steer
	// buildDutySteer just suppressed. Placed before degeneracy thinning so a
	// flagged actor's movement invitation is stripped in lockstep with the steers.
	p.EveningLeisure = buildEveningLeisure(snap, actorSnap, p.Anchors)
	p.BakeChoice = buildBakeChoice(snap, actorSnap)
	// LLM-459: the bake affordance yields to an active seek-work directive. LLM-454
	// built bake for the villagers who are home BECAUSE they have no reason to seek
	// work — the homebodies who otherwise narrate "let's make bread" for an hour
	// without doing it. A worker the seek-work directory has engaged is by
	// construction not that population: it is workless, under the coin ceiling, and
	// free to leave. Rendering both puts two action invitations in one prompt, and
	// the seek-work one wins on placement regardless — it is the closing triage coda
	// and the prompt's only imperative ("call move_to now", render.go), while the
	// bake line sits mid-scene. Live 2026-07-18: Silence Walker, 8 coins and 4 flour,
	// read both and did laps (home -> Tavern -> Blueberry Bush -> home) while her
	// three housemates over the ceiling baked normally.
	//
	// Subtractive, like the leisure-venue and summons blocks below: drop the losing
	// cue rather than adding a counter-instruction the weak model argues with. Nilling
	// the view drops the bake TOOL in lockstep (handlers.gateTools reads the same
	// BakeChoice signal), so the actor is never left holding a tool the prompt tells
	// it to walk away from.
	if len(p.SeekWorkPlaces) > 0 {
		p.BakeChoice = nil
		// LLM-562: the suppression must reach the scene, not just the affordance.
		// With BakeChoice nilled, a housemate's "(at the hearth, baking just now)"
		// annotation is the strongest bread line left in the prompt, and a seek-work
		// worker walking in on a household bake reads it and narrates joining — nine
		// home-arrival ticks of "let's get a batch in" against a toolset offering
		// only move_to (Patience Walker, live 2026-07-29, 179 calls in a day). Strip
		// the bake annotation from THIS observer's co-present view; the housemate
		// stays present and greetable, and any observer not under seek-work still
		// reads them as baking. The other kinds (repair/stoke/gather) stand — they
		// don't invite joining, so they set no trap. CoPresent is the only view that
		// renders busyActivityPhrase (renderHuddleMember carries no busy annotation),
		// so this is the whole surface.
		for i := range p.Surroundings.CoPresent {
			if m := &p.Surroundings.CoPresent[i]; m.SourceActivityKind == sim.SourceActivityBake {
				m.SourceActivityBusy = false
				m.SourceActivityKind = ""
				m.SourceActivityLabel = ""
			}
		}
	}
	// LLM-345: inside a leisure venue, on the evening, the walk-away work-errand cues
	// yield to the room. Each of these tells the agent to LEAVE and go buy or gather
	// something — shovels from the smith, restock from a supplier, nails for a mend,
	// berries from its bushes — and each renders under the coda that ranks obligations
	// above idle matters, so an evening in the tavern loses to a shovel every time. Off
	// the evening window, or anywhere but a venue, they all stand as before: an agent
	// that chooses to run an errand on its way home is not the bug. The wares cue
	// (## What your wares fetch) deliberately SURVIVES — it names no destination and
	// carries no leave-imperative, it is what lets a trade happen across the tavern
	// table (LLM-125), and silencing it would put invented prices back in the one room
	// this ticket exists to fill.
	//
	// SETTLED, not merely inside: an agent that has already committed to a walk out — to
	// the smith for those very shovels — keeps the cue that explains its own in-flight
	// move, or the prompt could no longer account for where it was going (code_review).
	//
	// Applied after buildDutySteer so the steer's own errand suppressors read the
	// unmodified views; the steer is nil here regardless (inEveningLeisure suppresses
	// its off-shift arm), but the ordering keeps that a coincidence rather than a
	// dependency.
	if settledAtLeisureVenue(snap, actorSnap) {
		p.FarmUpkeep = nil
		p.Restocking = nil
		p.StallRepairBuy = nil
		p.Forage = nil
		p.WorkClothes = nil
	}
	// LLM-414: a live summons also silences the walk-away work-errand cues,
	// the same subtractive treatment the settled-evening block above applies —
	// each names a DIFFERENT destination and renders under the triage coda's
	// obligations-first ranking, so a summoned keeper would lose the meeting
	// to a shovel. The wares cue survives here too (no destination, no
	// leave-imperative). Same post-buildDutySteer ordering note as above: the
	// steer already yielded on its own summonsActive gate.
	if summonsActive(snap, actorSnap) {
		p.FarmUpkeep = nil
		p.Restocking = nil
		p.StallRepairBuy = nil
		p.Forage = nil
		p.WorkClothes = nil
	}
	// Stay-open choice (ZBBS-WORK-387): a keeper standing at its own post on an
	// off-shift wind-down may keep its business open instead of closing up. Surface
	// the option, and encourage it when a concrete reason is present (the hybrid
	// gate — an owed order, a co-present buyer, or a pending offer; the same class
	// of "unfinished business" signal the HOME-400 to-work gate reads). Computed
	// here, after buildDutySteer, off the already-built order/offer/customer views.
	if p.DutySteer != nil && !p.DutySteer.ToWork && !p.DutySteer.AtPost && p.AtOwnBusiness {
		p.DutySteer.OfferStayOpen = true
		p.DutySteer.StayOpenReason = stayOpenReason(
			len(p.PendingDeliveriesFromMe) > 0,
			p.OfferableCustomers != nil,
			len(p.PendingOffersFromMe) > 0,
		)
	}
	// Degeneracy Stage-1 thinning (LLM-94). A flagged actor is in a sustained
	// futile loop — the live case is move_to the substrate rejects every tick,
	// driven by a steering cue that names a place the actor cannot reach. The
	// weak-model lesson (telling it NOT to move while a "go to X" cue still
	// stands makes it move anyway) makes the response SUBTRACTIVE: drop the
	// place-naming movement steers outright so nothing prompts the walk, rather
	// than adding a counter-instruction. The move_to TOOL is gated in lockstep
	// (handlers.gateTools, same DegenStage signal) so the affordance goes too.
	// Self-reversing: a productive tick clears the flag (DegenStage→None) and
	// the steers return next tick. Placed before every return path below so a
	// flagged actor is thinned even when no scene resolves.
	if degeneracyFlagged(actorSnap) {
		thinDegenerateSteer(&p)
	}
	// LLM-491: reconcile the at-post stabilizer with whichever supply cue is
	// sending the subject out. Derived HERE, at the end of the mutation chain,
	// rather than inside buildDutySteer: the leisure-venue and summons blocks above
	// and the degeneracy thinning all nil the very sections this reads, and every
	// one of them runs AFTER buildDutySteer. Reading the pre-thinning views would
	// reframe the stabilizer for an errand whose cue no longer renders — a step-out
	// line with nowhere to step out to, which is the "told to move" residue those
	// subtractions exist to remove.
	if p.DutySteer != nil && p.DutySteer.AtPost {
		p.DutySteer.SupplyErrand = hasAtPostSupplyErrand(&p)
	}
	p.Lodging = buildLodgingView(snap, actorID, actorSnap, p.Surroundings.HuddleMembers)
	// LLM-447: the voluntary bed-down affordance — the evening's exit. Built here,
	// after Surroundings, because the line is shaped by whether there is company to
	// bid goodnight to. Covers home AND lodging; it is the single bedtime cue.
	p.TurnInChoice = buildTurnInChoice(snap, actorSnap, p.Surroundings.HuddleMembers)
	p.KeeperLodging = buildKeeperLodgingView(snap, actorSnap, p.Surroundings.HuddleMembers)
	// The held-lodger signal is informational, like "## Your inn" — ungated by
	// location so a keeper affirms a settled guest wherever they meet (LLM-38).
	// It keys off KeeperLodging, so it inherits the LLM-22 awake-peer gate: a
	// co-present held lodger conversing IS an awake peer, so the cue still fires
	// exactly when there's someone to affirm to.
	p.KeeperHeldLodgers = buildKeeperHeldLodgers(snap, actorID, p.KeeperLodging, p.Surroundings.HuddleMembers)
	// The offer cue is location-bound the way vendor cues are (ZBBS-WORK-385's
	// at-own-post principle): a keeper drinking at someone ELSE's
	// establishment must not be steered to sell their own rooms into that
	// huddle (observed live: Hannah pitching her Inn's rooms from inside
	// John's Tavern, and a guest buying one there — ZBBS-HOME-424). Gated on
	// the location predicate directly rather than p.AtOwnBusiness because the
	// keeper-lodging views key on WorkStructureID alone, not on
	// BusinessownerState — an innkeeper without vendor state still keeps
	// rooms. The informational "## Your inn" status section stays ungated by
	// LOCATION (a keeper sees their own inn's vacancy from anywhere); it has its
	// own audience gate inside buildKeeperLodgingView (LLM-22 — no awake peer,
	// no section). Only the act-now offer instruction is location-bound.
	if actorSnap.WorkStructureID != "" && actorSnap.InsideStructureID == actorSnap.WorkStructureID {
		p.LodgingOffer = buildLodgingOfferCue(snap, actorID, p.KeeperLodging, p.Surroundings.HuddleMembers)
	}
	// Traveler day-plan cues (LLM-373): the daytime "## On your rounds" circuit
	// framing, and the evening "## A bed for the night" booking cue. Both nil for a
	// non-traveler subject; the seek-a-bed cue additionally needs a homeless traveler,
	// the civil evening, and a co-present innkeeper (see traveler_dayplan.go).
	p.TravelerRounds = buildTravelerRounds(snap, actorSnap, p.Surroundings.HuddleMembers)
	p.TravelerSeekBed = buildTravelerSeekBed(snap, actorSnap, p.Surroundings.HuddleMembers)
	// Merchant errand cues (LLM-455): the counterparty keeper's "a trader's come to deal" cue
	// (buy or sell), and — for the tool gate — whether the visitor's commerce is confined to
	// talk-only right now (not co-present with his errand keeper or a tavern/inn).
	p.ErrandVisit = buildErrandVisit(snap, actorID, actorSnap, p.Surroundings.HuddleMembers)
	p.VisitorCommerceStripped = visitorCommerceStripped(snap, actorSnap, p.Surroundings.HuddleMembers)
	p.SummonsForYou = buildSummonsForYou(snap, actorSnap)
	p.SummonRefusal = buildSummonRefusal(actorSnap)

	// Group the consumed warrants by the scene they reference. Only event-
	// sourced warrants carry a scene (the zero-lineage invariant: full
	// lineage or none), so a non-empty SceneID always rides a nonzero
	// SourceEventID — which makes the max-SourceEventID primary selection
	// below well defined and unique.
	sceneGroups := groupBySceneID(p.Warrants)
	p.MultiSceneWarrantCount = len(sceneGroups)

	primarySceneID, reason := resolvePrimaryScene(snap, actorSnap, p.Warrants, sceneGroups)
	p.SelectionReason = reason

	if primarySceneID == "" {
		// No scene resolved. Every scene-bearing warrant (if any) just
		// renders in the flat Warrants list; there is nothing to diff.
		p.Baseline = BaselineMissingNoScene
		return p
	}

	scene := snap.Scenes[primarySceneID]
	if scene == nil {
		// resolvePrimaryScene only returns IDs backed by a non-nil scene,
		// so this is unreachable today — but the guard keeps Build's "never
		// panics" contract locally obvious rather than dependent on
		// reasoning about resolvePrimaryScene.
		p.Baseline = BaselineMissingNoScene
		p.SelectionReason += " — resolved scene was nil in the snapshot"
		return p
	}
	p.Baseline, p.Primary = buildPrimaryScene(scene, actorID, actorSnap, sceneGroups[primarySceneID])
	p.Secondary = buildSecondary(snap, sceneGroups, primarySceneID)
	return p
}

// hasCustomAtPost reports whether any huddle member could be custom the subject
// is mid-encounter with — the co-presence arm of customerEngaged. A member
// fulfilling a hired job (m.Laboring, set for every observer) is not custom and
// does not count: that is LLM-231's rule, the same one buildOfferableCustomers
// applies when it drops a laboring peer as a sale target even for their own
// employer.
//
// The distinction matters because of DURATION, not just correctness. The
// customerEngaged deferral is built for a transient encounter — finish the sale,
// then step out to the bushes. A labor contract runs for hours at the employer's
// own post, so counting a worker as custom converted a brief deferral into a
// shift-long blackout of the harvest cue. Live: Joseph Scott, the village's only
// permitted water gatherer, hired Lewis Walker to haul at the Mill from 11:13 to
// 15:13; his water cue rendered correctly on the walk in (0 of 20 on hand, a full
// well a short walk northeast) and vanished on the first tick after Lewis started
// work, while the same prompt told him "Lewis Walker is working a job for you".
//
// Deliberately NOT narrowed to the subject's OWN workers: a peer mid-job for
// anyone is occupied and already excluded as a sale target, so the broader read
// is both simpler and consistent with LLM-231.
func hasCustomAtPost(members []HuddleMember) bool {
	for _, m := range members {
		if !m.Laboring {
			return true
		}
	}
	return false
}

// degeneracyFlagged reports whether the degeneracy observer (LLM-94) has the
// actor at Stage 1 or higher (sim.DegeneracyFlagged / …Throttled) — the signal
// the Stage-1 steer thinning engages on, read off the snapshot projection. The
// observer is OFF by default, so this is DegeneracyNone for every actor unless
// an operator has enabled it and the actor has sustained a futile streak. False
// for a nil actor (conservative: don't thin perception we can't attribute).
func degeneracyFlagged(a *sim.ActorSnapshot) bool {
	if a == nil {
		return false
	}
	return a.DegenStage >= sim.DegeneracyFlagged
}

// thinDegenerateSteer removes the place-naming MOVEMENT cues from a flagged
// actor's payload — the "return to your post" / "go home" / "go to your inn"
// steers, the restock errand, and the forage errand that drive the futile
// move_to loop. Everything that does not point the actor at a place to walk to
// is left intact: the at-post stabilizer (a DutySteer with no TargetID — a
// "stay put" cue) and the placeless wander nudge both survive, as does every
// non-movement section. An audience-bearing or need-driven cue still gets
// through and can produce the productive tick that clears the flag.
//
// Two surgical details:
//   - A DutySteer carrying a TargetID is a "go to X" arm (to-work, go-home,
//     lodging) — dropped whole; the stay-open / lodging modifiers ride with it.
//   - The at-post stabilizer survives, but its ForageErrand modifier (the
//     "step out and return" reframe) is cleared in lockstep with p.Forage —
//     otherwise the actor would read a step-out line with no forage cue behind
//     it, the exact "told to move" residue the thinning exists to remove (the
//     live Prudence-at-her-apothecary forage-loop shape).
//
// The buy-side SupplyErrand modifier needs no such lockstep clearing: Build derives
// it from the surviving views AFTER this runs, so a stabilizer here can never be
// left reframed for a cue this function just removed.
func thinDegenerateSteer(p *Payload) {
	p.Restocking = nil
	p.Forage = nil
	// The evening leisure cue (LLM-149) is a place-naming "head to the tavern"
	// invitation — the same movement-steer class this thinning removes for a
	// flagged actor, so it goes too.
	p.EveningLeisure = nil
	p.BakeChoice = nil
	if p.DutySteer == nil {
		return
	}
	if p.DutySteer.TargetID != "" {
		p.DutySteer = nil
		return
	}
	p.DutySteer.ForageErrand = ForageErrandNone
}

// hasAtPostSupplyErrand reports whether any section of the assembled payload hands
// the subject an off-scene supplier to walk to (LLM-491). These four are the cues
// that can render a "(destination: <id>)" for a BUY while the subject stands at its
// own post — the state in which the duty stabilizer otherwise says "stay and look
// after your work" in the same prompt.
//
// Each view answers for itself, mirroring its own renderer's branch order, so this
// can never claim an errand a section didn't actually print. Deliberately NOT in
// the set: StallRepairBuy (its whole premise is an owner already AWAY from her
// post, so it cannot co-occur with AtPost) and the cold-garment coat vendors (they
// render only in the storm-and-outdoors branch, and an actor outdoors is not inside
// its workplace). Adding a fifth walk-to cue that CAN fire at post means adding it
// here — the golden invariant TestAtPostSteerNeverContradictsSupplyErrand fails
// loudly if it isn't.
func hasAtPostSupplyErrand(p *Payload) bool {
	return p.Restocking.HasWalkToSupplier() ||
		p.StallRepair.HasWalkToSupplier() ||
		p.FarmUpkeep.HasWalkToSupplier() ||
		p.WorkClothes.HasWalkToSupplier() ||
		p.Hearth.HasWalkToSupplier()
}

// orderWarrants returns a copy of the batch ordered by SourceEventID
// ascending — PR 3a's monotonic EventID is the authoritative causal order.
// Zero-lineage warrants (SourceEventID == 0) sort first; the sort is
// stable so ties hold the evaluator's input order. A copy is returned so
// Build never mutates the caller's slice.
func orderWarrants(in []sim.WarrantMeta) []sim.WarrantMeta {
	if len(in) == 0 {
		return nil
	}
	out := make([]sim.WarrantMeta, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].SourceEventID < out[j].SourceEventID
	})
	return out
}

// groupBySceneID buckets warrants by their (non-empty) SceneID, preserving
// each bucket's incoming order — since the input is already ordered by
// SourceEventID, each bucket is too. Warrants with no SceneID are not
// bucketed (they are not scene-scoped).
func groupBySceneID(ordered []sim.WarrantMeta) map[sim.SceneID][]sim.WarrantMeta {
	groups := make(map[sim.SceneID][]sim.WarrantMeta)
	for _, w := range ordered {
		if w.SceneID == "" {
			continue
		}
		groups[w.SceneID] = append(groups[w.SceneID], w)
	}
	return groups
}

// resolvePrimaryScene applies the scene-resolution order from the PR 3
// design note:
//
//  1. the scene of the consumed warrant with the maximum SourceEventID
//     (the most recent causal signal) — but skip a warrant whose scene is
//     absent from the snapshot (a stale reference, e.g. an area scene
//     deleted when its huddle concluded) and fall through to the next;
//  2. else the actor's active-huddle scene, if the huddle is observed by
//     exactly resolvable scene state;
//  3. else none.
//
// It returns the resolved SceneID ("" when none) and a human-readable
// SelectionReason.
func resolvePrimaryScene(
	snap *sim.Snapshot,
	actorSnap *sim.ActorSnapshot,
	ordered []sim.WarrantMeta,
	sceneGroups map[sim.SceneID][]sim.WarrantMeta,
) (sim.SceneID, string) {
	// Step 1: scene-bearing warrants, highest SourceEventID first.
	if len(sceneGroups) > 0 {
		byEventDesc := make([]sim.WarrantMeta, 0, len(ordered))
		for _, w := range ordered {
			if w.SceneID != "" {
				byEventDesc = append(byEventDesc, w)
			}
		}
		sort.SliceStable(byEventDesc, func(i, j int) bool {
			return byEventDesc[i].SourceEventID > byEventDesc[j].SourceEventID
		})
		for _, w := range byEventDesc {
			// A present-but-nil map entry counts as absent — buildPrimaryScene
			// would dereference it.
			if sc := snap.Scenes[w.SceneID]; sc != nil {
				return w.SceneID, fmt.Sprintf(
					"primary scene %q from warrant (SourceEventID %d, max among %d scene-bearing warrant(s) across %d scene(s))",
					w.SceneID, w.SourceEventID, len(byEventDesc), len(sceneGroups))
			}
		}
		// Every scene-bearing warrant pointed at a scene no longer in the
		// snapshot — fall through to huddle resolution.
	}

	// Step 2: the actor's active-huddle scene. A huddle can be observed by
	// more than one scene over its lifetime (Scene→Huddles is many-to-many),
	// so pick deterministically — the lexicographically lowest SceneID.
	if actorSnap.CurrentHuddleID != "" {
		var candidates []sim.SceneID
		for id, sc := range snap.Scenes {
			if sc == nil {
				continue
			}
			if _, ok := sc.Huddles[actorSnap.CurrentHuddleID]; ok {
				candidates = append(candidates, id)
			}
		}
		if len(candidates) > 0 {
			sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
			chosen := candidates[0]
			if len(sceneGroups) > 0 {
				return chosen, fmt.Sprintf(
					"primary scene %q from actor's active huddle %q (no scene-bearing warrant resolved to a live scene; %d candidate scene(s))",
					chosen, actorSnap.CurrentHuddleID, len(candidates))
			}
			return chosen, fmt.Sprintf(
				"primary scene %q from actor's active huddle %q (no scene-bearing warrants; %d candidate scene(s))",
				chosen, actorSnap.CurrentHuddleID, len(candidates))
		}
	}

	// Step 3: nothing resolved.
	if len(sceneGroups) > 0 {
		return "", "no scene resolved — scene-bearing warrant(s) reference scenes absent from the snapshot, and the actor's huddle resolved no scene"
	}
	if actorSnap.CurrentHuddleID != "" {
		return "", "no scene resolved — no scene-bearing warrants, and the actor's active huddle is observed by no scene in the snapshot"
	}
	return "", "no scene resolved — no scene-bearing warrants and the actor is not in a huddle"
}

// buildPrimaryScene resolves the BaselineStatus for the subject actor
// against the primary scene and assembles its SceneView. The "unknown,
// never no-change" contract is enforced here: a Diff is attached ONLY when
// the actor has a genuine origin snapshot in the scene.
func buildPrimaryScene(
	scene *sim.Scene,
	actorID sim.ActorID,
	actorSnap *sim.ActorSnapshot,
	sceneWarrants []sim.WarrantMeta,
) (BaselineStatus, *SceneView) {
	view := &SceneView{
		SceneID:    scene.ID,
		OriginKind: scene.OriginKind,
		OriginAt:   scene.OriginAt,
		Warrants:   sceneWarrants,
	}

	switch {
	case len(scene.ParticipantStateAtOrigin) == 0:
		// The scene captured no participant baseline at all (e.g. an
		// unbounded atmosphere-refresh scene). Absence here carries no
		// "joined after" signal — no one has a baseline.
		return BaselineMissingNoOriginSnapshot, view

	case scene.ParticipantStateAtOrigin[actorID] == nil:
		// Other participants were captured at origin but this actor was
		// not — so it joined after the scene was minted.
		return BaselineMissingJoinedAfterOrigin, view

	default:
		origin := scene.ParticipantStateAtOrigin[actorID]
		view.Diff = computeDiff(origin, actorSnap)
		return BaselinePresent, view
	}
}

// buildSecondary turns every scene group other than the primary into a
// SceneSignal, sorted by SceneID for determinism. A secondary scene
// carries no baseline diff by design — see SceneSignal's doc comment.
// Groups whose scene is absent from the snapshot are skipped (their
// warrants still appear in the flat Payload.Warrants list).
func buildSecondary(
	snap *sim.Snapshot,
	sceneGroups map[sim.SceneID][]sim.WarrantMeta,
	primary sim.SceneID,
) []SceneSignal {
	var out []SceneSignal
	for sceneID, group := range sceneGroups {
		if sceneID == primary {
			continue
		}
		// A present-but-nil entry counts as absent, same as in resolvePrimaryScene.
		if sc := snap.Scenes[sceneID]; sc == nil {
			continue
		}
		out = append(out, SceneSignal{
			SceneID:  sceneID,
			HuddleID: representativeHuddle(group),
			Warrants: group,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SceneID < out[j].SceneID })
	return out
}

// representativeHuddle picks the HuddleID of the highest-SourceEventID
// warrant in a group — the most recent signal — as the group's
// representative huddle. Warrants in one scene group usually share a
// huddle, but a scene can observe several; the most-recent one is the
// deterministic choice.
func representativeHuddle(group []sim.WarrantMeta) sim.HuddleID {
	var best sim.WarrantMeta
	for i, w := range group {
		if i == 0 || w.SourceEventID > best.SourceEventID {
			best = w
		}
	}
	return best.HuddleID
}

// computeDiff is the loop-detection seam — it compares the actor's frozen
// origin snapshot against its current snapshot field by field. AnyChange
// is the OR every consumer reads: false across consecutive ticks is the
// "this actor is stuck" signal.
func computeDiff(origin, current *sim.ActorSnapshot) *Diff {
	d := &Diff{
		StateChanged:     origin.State != current.State,
		PositionChanged:  origin.Pos.X != current.Pos.X || origin.Pos.Y != current.Pos.Y,
		StructureChanged: origin.InsideStructureID != current.InsideStructureID,
		HuddleChanged:    origin.CurrentHuddleID != current.CurrentHuddleID,
		CoinsChanged:     origin.Coins != current.Coins,
		InventoryChanged: origin.InventoryHash != current.InventoryHash,
		NeedsChanged:     !needsEqual(origin.Needs, current.Needs),
	}
	d.AnyChange = d.StateChanged || d.PositionChanged || d.StructureChanged ||
		d.HuddleChanged || d.CoinsChanged || d.InventoryChanged || d.NeedsChanged
	return d
}

// needsEqual reports whether two need maps carry the same key/value set.
// A missing key and a zero value are treated as distinct — needs are
// always fully populated by snapshotActor, so a key appearing or
// disappearing is itself a real change worth surfacing.
func needsEqual(a, b map[sim.NeedKey]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || bv != v {
			return false
		}
	}
	return true
}

// buildActorView lifts the subject actor's own decision-relevant state out
// of its ActorSnapshot. The Needs map is copied so the Payload does not
// alias the snapshot's map. Active dwell credits are projected from
// a.DwellCredits with StructureLabel resolved against snap.Structures
// (preferred) or snap.VillageObjects (fallback for object-source
// credits whose pin is a free-standing object like a well or shade
// tree, not a structure).
func buildActorView(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) ActorView {
	var needs map[sim.NeedKey]int
	if len(a.Needs) > 0 {
		needs = make(map[sim.NeedKey]int, len(a.Needs))
		for k, v := range a.Needs {
			needs[k] = v
		}
	}
	return ActorView{
		State:                  a.State,
		InsideStructureID:      a.InsideStructureID,
		Position:               sim.Position{X: a.Pos.X, Y: a.Pos.Y},
		CurrentHuddleID:        a.CurrentHuddleID,
		Coins:                  a.SpendableCoins(),
		VisitorTakings:         a.Coins - a.SpendableCoins(),
		Needs:                  needs,
		NeedThresholds:         snap.NeedThresholds,
		ActiveDwellCredits:     buildActiveDwellCredits(snap, a),
		InFlightMove:           buildInFlightMove(snap, a),
		InFlightSourceActivity: buildInFlightSourceActivity(snap, a),
		Rounds:                 buildRounds(snap, a),
		Inventory:              buildInventoryView(snap, a),
		PackBoundHome:          travelerPackBoundHome(snap, a),
		HoursAwake:             computeHoursAwake(snap.LocalMinuteOfDay, a.ScheduleStartMin, a.ScheduleEndMin),
		InFlightProduction:     buildInFlightProduction(snap, actorID, a),
		Cold:                   buildColdSelf(snap, a),
	}
}

// buildRounds projects the subject's in-flight constable rounds route (LLM-514)
// into a render-ready view, or nil when the actor isn't walking his rounds. Gated
// on RouteLabel == AttrConstable — stamped only on the actor who OWNS the route
// (world.go republish) — so the cue is self-only by construction: an onlooker's
// snapshot is a different actor whose route fields are empty. AtBusiness resolves
// the arrived-at stop's display name the same way move / dwell labels do
// (resolveDwellPinLabel), and is empty while he is walking between stops (the
// republish stamps RouteStopObjectID only once he has arrived).
func buildRounds(snap *sim.Snapshot, a *sim.ActorSnapshot) *RoundsView {
	if !actorOnRound(a) {
		return nil
	}
	return &RoundsView{
		AtBusiness:   resolveDwellPinLabel(snap, a.RouteStopObjectID),
		StopsAhead:   a.RouteStopsAhead,
		NextBusiness: resolveDwellPinLabel(snap, a.RouteNextStopObjectID),
	}
}

// actorOnRound reports whether the subject is carrying a rounds circuit right
// now. RouteLabel is stamped only on the actor who OWNS the route (world.go
// republish), so this is self-only by construction — an onlooker's snapshot is a
// different actor whose route fields are empty.
//
// Extracted so the rounds cue and the LLM-547 contact line ask the same
// question of the same field rather than each testing RouteLabel themselves.
// The contact line needs it to choose between "this round" and a bare time
// phrase, and the two must never disagree about whether a round is under way.
func actorOnRound(a *sim.ActorSnapshot) bool {
	return a != nil && a.RouteLabel == sim.AttrConstable
}

// buildInFlightProduction projects the subject's in-progress production cycle
// (LLM-319) into a render-ready view, or nil when nothing is in the works.
// Truthful wherever the actor is: the batch exists (its inputs are spent)
// whether or not the actor is at the post, and the WorkLeft phrase is the
// base-rate estimate, so away-from-post the line reads as work still owed —
// progress simply isn't accruing until the actor is back (the produce-tick
// pause). Its presence is also what keeps buildForgeChoice nil, so the trade
// cue and this standing line never show together.
func buildInFlightProduction(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) *InFlightProductionView {
	if a.ProductionItem == "" {
		return nil
	}
	view := &InFlightProductionView{
		ItemLabel: itemDisplayLabel(snap, a.ProductionItem),
		WorkLeft:  sim.HumanizeWorkDuration(a.ProductionRemainingSeconds),
	}
	// LLM-446: a degraded business drags on the batch — slowed at a positive
	// pct, halted outright at 0 (legacy). Carried so the line can't promise a
	// clock the engine isn't running; >=100 is no penalty, so neither is set.
	if ownerBusinessDegraded(snap, actorID) && snap.StallDegradedProducePct < 100 {
		if snap.StallDegradedProducePct > 0 {
			view.Slowed = true
		} else {
			view.Halted = true
		}
	}
	return view
}

// computeHoursAwake returns whole hours the actor has been awake, measured from
// its shift-start — but ONLY while it is on-shift. On-shift guarantees
// continuous wakefulness since shift-start: NPCs wake at shift-start
// (ZBBS-HOME-435) and only auto-sleep off-shift, so the elapsed-since-start is
// true hours-awake. Off-shift the schedule alone can't tell "still up since this
// morning" from "slept, now awake before the next shift" — the modular elapsed
// would overstate (e.g. ~23h for a day-shift NPC awake before dawn) — so the
// tail is dropped and renderTiredness falls back to the bare tier phrase. The
// modulo handles wrap-midnight shifts (start 16:00, now 02:00 → 10h on-shift).
// Returns nil off-shift, unscheduled (nil bounds), or with no clock. LLM-85.
func computeHoursAwake(nowMin, startMin, endMin *int) *int {
	if nowMin == nil {
		return nil
	}
	// OnShiftAtMinute returns false for nil bounds, so a true result also
	// guarantees startMin is non-nil for the deref below.
	if !sim.OnShiftAtMinute(startMin, endMin, *nowMin) {
		return nil
	}
	minutesAwake := ((*nowMin-*startMin)%1440 + 1440) % 1440
	hours := minutesAwake / 60
	return &hours
}

// buildInFlightSourceActivity projects the subject's in-flight SourceActivity
// (the read-path Kind/ObjectID/Attribute fields the snapshot carries while the
// window is live) into a render-ready view, or nil when the actor isn't engaged
// at a source. SourceLabel resolves the same way dwell-pin and move labels do
// (resolveDwellPinLabel against snap.Structures / snap.VillageObjects). LLM-69.
func buildInFlightSourceActivity(snap *sim.Snapshot, a *sim.ActorSnapshot) *InFlightSourceActivityView {
	if !actorMidSourceActivity(a) {
		return nil
	}
	return &InFlightSourceActivityView{
		Kind:        a.SourceActivityKind,
		SourceLabel: resolveDwellPinLabel(snap, a.SourceActivityObjectID),
		Attribute:   a.SourceActivityAttribute,
	}
}

// buildInventoryView resolves the actor's carried goods (positive quantities)
// into the standing inventory readout — display labels via itemDisplayLabel,
// sorted by label then ItemKind so the line is deterministic (Inventory is a
// map). Returns nil for an empty inventory so Render omits the line.
// ZBBS-HOME-361.
func buildInventoryView(snap *sim.Snapshot, a *sim.ActorSnapshot) []InventoryItem {
	if len(a.Inventory) == 0 {
		return nil
	}
	out := make([]InventoryItem, 0, len(a.Inventory))
	// LLM-636: the goods this pack keeps to work with, and the garment on its
	// back — the same reservation the barter steers and the pay_with_item intake
	// gate read, so the carry line names as "not for trade" exactly what the tool
	// will refuse.
	spokenFor := sim.SpokenFor(snap.ItemKinds, snap.Recipes, sim.SnapshotBarterHolder(snap, a))
	for kind, qty := range a.Inventory {
		if qty <= 0 {
			continue
		}
		label := itemDisplayLabel(snap, kind)
		// Count-aware noun for the carry line so a liquid/mass good reads with
		// its period vessel ("flasks of water") instead of a bare label the model
		// fills with an invented container ("buckets"). Falls back to the display
		// label for a kind with no authored singular/plural (LLM-113).
		noun := label
		def := snap.ItemKinds[kind]
		if def != nil {
			if cn := def.CountNoun(qty); cn != "" {
				noun = cn
			}
		}
		out = append(out, InventoryItem{
			Label:     label,
			CountNoun: noun,
			Qty:       qty,
			Use:       inventoryItemUse(snap, kind),
			// LLM-445: the eat-here disposition + barterability travel with the
			// item so render can annotate the carry line and key the coinless
			// barter cue on goods the resolver would actually accept.
			EatHere:    def.EatHereOnly(),
			Spare:      sim.SpareQty(a.Inventory, spokenFor, kind),
			SpokenFor:  spokenFor[kind].Reason,
			Barterable: sim.KindBarterable(def) && sim.SpareQty(a.Inventory, spokenFor, kind) > 0,
			kind:       kind,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].kind < out[j].kind
	})
	return out
}

// inventoryItemUse returns the "used to produce X" annotation for a carried item
// that is INEDIBLE and a recipe input (LLM-166), else "". An edible item is left
// to the satiation cue; a non-ingredient inedible (a horseshoe in no recipe) has
// nothing to say. Reads the precomputed reverse index off the snapshot — no
// per-build catalog scan.
func inventoryItemUse(snap *sim.Snapshot, kind sim.ItemKind) string {
	def := snap.ItemKinds[kind]
	if def == nil || def.Consumable() {
		return ""
	}
	outs := snap.RecipeUses[kind]
	if len(outs) == 0 {
		return ""
	}
	labels := make([]string, 0, len(outs))
	for _, o := range outs {
		labels = append(labels, itemDisplayLabel(snap, o))
	}
	return sim.RecipeUseClause(labels)
}

// buildInFlightMove projects the subject's in-flight MoveIntent (the
// read-path destination fields on the snapshot) into a render-ready view, or
// nil when the actor isn't moving. The label resolves the same way dwell-pin
// labels do — structure DisplayName first, then village-object DisplayName —
// except a bare Position move has no pin to name, so it renders its tile
// coordinate. ZBBS-HOME-336.
func buildInFlightMove(snap *sim.Snapshot, a *sim.ActorSnapshot) *InFlightMoveView {
	if a.MoveDestKind == "" {
		return nil
	}
	var label string
	switch a.MoveDestKind {
	case sim.MoveDestinationStructureEnter, sim.MoveDestinationStructureVisit:
		label = resolveDwellPinLabel(snap, sim.VillageObjectID(a.MoveDestStructureID))
	case sim.MoveDestinationObjectVisit:
		label = resolveDwellPinLabel(snap, a.MoveDestObjectID)
	case sim.MoveDestinationPosition:
		label = fmt.Sprintf("(%d, %d)", a.MoveDestPos.X, a.MoveDestPos.Y)
	default:
		// Unrecognized kind — a corrupt snapshot or a destination kind added
		// to the engine but not yet wired into perception. Don't render a
		// vague "walking to your destination" that masks the gap; surface it
		// as not-moving so the omission is visible rather than papered over.
		return nil
	}
	return &InFlightMoveView{Kind: a.MoveDestKind, DestinationLabel: label}
}

// buildActiveDwellCredits projects the actor's DwellCredits map into a
// deterministic, render-ready slice. Returns nil for an empty map.
// StructureLabel resolution:
//
//   - Look up the credit's ObjectID in snap.Structures first — for
//     item-source credits the pin is usually the structure (tavern,
//     bakery) where the actor ate.
//   - Fall back to snap.VillageObjects.DisplayName — covers
//     object-source credits whose pin is a free-standing object (well,
//     shade tree).
//   - Empty when neither resolves.
//
// Order: (Source ascending, Attribute ascending, ObjectID ascending)
// — stable for golden tests and admin replay.
func buildActiveDwellCredits(snap *sim.Snapshot, a *sim.ActorSnapshot) []DwellCreditView {
	if len(a.DwellCredits) == 0 {
		return nil
	}
	out := make([]DwellCreditView, 0, len(a.DwellCredits))
	for _, c := range a.DwellCredits {
		if c == nil {
			continue
		}
		// Co-location gate (LLM-68): render a credit as an active dwell only
		// while the actor is still at its pin. The credit lingers in the map
		// until the next dwell-tick walk-away sweep deletes it; without this
		// gate perception keeps asserting "you are resting at X" after the actor
		// has walked off, steering the model to stay put and do nothing. Mirrors
		// the dwell-tick walk-away check actorAtCreditObject (ok && id ==
		// credit.ObjectID): an empty CurrentLoiterObjectID means the actor
		// stands at no pin (resolver returned !ok), so every credit drops.
		if a.CurrentLoiterObjectID == "" || c.ObjectID != a.CurrentLoiterObjectID {
			continue
		}
		// Satisfied-need gate (LLM-376): sibling of the co-location gate above,
		// scoped to OBJECT dwells (well / shade tree / bush). Those are open-ended
		// with no countdown — they end only when the floor-hit terminator clears
		// them — so a credit whose need is already at the floor has nothing left
		// to ease, and asserting "you are drinking … until your thirst is
		// quenched" when the actor is already quenched is unfaithful and pins them
		// in place. The grant guard in applyObjectRefreshEffect keeps such a credit
		// from being stamped; this drops any that still slips through (a checkpoint
		// edge, a future grant path). Item dwells are left alone: their lifecycle
		// is RemainingTicks-driven and they carry the "don't waste the coins you
		// paid" framing, so they are not need-gated here.
		if c.Source == sim.DwellSourceObject && a.Needs[c.Attribute] <= 0 {
			continue
		}
		view := DwellCreditView{
			ObjectID:       c.ObjectID,
			StructureLabel: resolveDwellPinLabel(snap, c.ObjectID),
			Source:         c.Source,
			Kind:           c.Kind,
			Attribute:      c.Attribute,
			PeriodMinutes:  c.DwellPeriodMinutes,
			DwellDelta:     c.DwellDelta,
			LastCreditedAt: c.LastCreditedAt,
		}
		if c.RemainingTicks != nil {
			rt := *c.RemainingTicks
			view.RemainingTicks = &rt
		}
		out = append(out, view)
	}
	// Every credit may have been co-location-gated out (the actor walked off
	// all its pins) — return nil so this renders identically to the no-credits
	// case and Render omits the line, matching buildInventoryView's posture.
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Attribute != out[j].Attribute {
			return out[i].Attribute < out[j].Attribute
		}
		return out[i].ObjectID < out[j].ObjectID
	})
	return out
}

// resolveDwellPinLabel resolves the human-facing label for a dwell
// pin. The pin's ObjectID may be either a StructureID (item-source
// credits pin to the structure where the actor ate) or a
// VillageObjectID (object-source credits pin to a free-standing
// object — a well, a shade tree). Try structure first, then village
// object, and return "" when neither has a label so render can fall
// back to a generic phrasing.
func resolveDwellPinLabel(snap *sim.Snapshot, objID sim.VillageObjectID) string {
	if objID == "" {
		return ""
	}
	if st := snap.Structures[sim.StructureID(objID)]; st != nil && st.DisplayName != "" {
		return st.DisplayName
	}
	if obj := snap.VillageObjects[objID]; obj != nil && obj.DisplayName != "" {
		return obj.DisplayName
	}
	return ""
}

// buildAnchors projects the actor's own home and work structures into the
// always-on move-target view. Returns nil when the actor has neither anchor (a
// PC, or an unanchored NPC) so Render omits the line. The structure_ids are
// surfaced verbatim — they're what the model passes to move_to; the labels are
// best-effort (a structure with no DisplayName yields an empty label, which
// Render replaces with a generic phrase while still carrying the id).
func buildAnchors(snap *sim.Snapshot, a *sim.ActorSnapshot) *AnchorsView {
	v := &AnchorsView{}
	// Only surface an anchor whose id actually RESOLVES to a structure in the
	// snapshot. Surfacing an id that isn't in the world would render an
	// actionable-looking move_to target the engine then rejects — the exact
	// "bouncing target" failure this change exists to remove. A resolved
	// structure with no DisplayName still surfaces (the id is what move_to
	// needs; render uses a generic phrase for the empty label).
	if label, ok := resolveStructureLabel(snap, a.WorkStructureID); ok {
		v.WorkID = a.WorkStructureID
		v.WorkLabel = label
	}
	if label, ok := resolveStructureLabel(snap, a.HomeStructureID); ok {
		v.HomeID = a.HomeStructureID
		v.HomeLabel = label
	}
	if v.WorkID == "" && v.HomeID == "" {
		return nil
	}
	v.SamePlace = v.WorkID != "" && v.WorkID == v.HomeID
	return v
}

// resolveStructureLabel resolves a StructureID to its human label. ok is true
// when the id names a structure (or shared village_object — structures share
// ids with village_objects) PRESENT in the snapshot, false when the id is empty
// or absent. A present structure with no DisplayName returns ("", true): the
// caller still surfaces the actionable id and renders a generic phrase.
func resolveStructureLabel(snap *sim.Snapshot, sid sim.StructureID) (string, bool) {
	if sid == "" {
		return "", false
	}
	if st := snap.Structures[sid]; st != nil {
		return st.DisplayName, true
	}
	if obj := snap.VillageObjects[sim.VillageObjectID(sid)]; obj != nil {
		return obj.DisplayName, true
	}
	return "", false
}

// buildSurroundings assembles the actor's immediate context — the
// structure it occupies and the other members of its current huddle.
// insideRelationLabel names how the structure the actor is INSIDE relates to it
// — "your home", "your workplace", or "your home and workplace" when it is both
// (a keeper who lives at their shop, e.g. John Ellis at the Tavern). Empty when
// the structure is neither (or the actor is outdoors: inside == ""). Drives the
// SurroundingsView.InsideRelation annotation on the location line (LLM-212).
func insideRelationLabel(inside, home, work sim.StructureID) string {
	isHome := inside != "" && inside == home
	isWork := inside != "" && inside == work
	switch {
	case isHome && isWork:
		return "your home and workplace"
	case isHome:
		return "your home"
	case isWork:
		return "your workplace"
	}
	return ""
}

// Per-member acquaintance status is resolved against the subject
// actor's Acquaintances map so Render can swap name vs. descriptor
// without re-reading the snapshot.
func buildSurroundings(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) SurroundingsView {
	s := SurroundingsView{
		InsideStructureID: a.InsideStructureID,
		HuddleID:          a.CurrentHuddleID,
		Atmosphere:        snap.Environment.Atmosphere,
		Weather:           snap.Environment.Weather,
		LocalMinuteOfDay:  snap.LocalMinuteOfDay,
		OnRound:           actorOnRound(a),
	}
	if item, source, ok := findGatherableCue(snap, actorID, a); ok {
		s.GatherableItem = item
		s.GatherableSource = source
		s.GatherableNoun = itemPlural(snap, item)
	}
	var curStructureID sim.StructureID
	if a.InsideStructureID != "" {
		curStructureID = a.InsideStructureID
		if st := snap.Structures[a.InsideStructureID]; st != nil {
			s.StructureName = st.DisplayName
		}
		s.InsideRelation = insideRelationLabel(a.InsideStructureID, a.HomeStructureID, a.WorkStructureID)
	} else {
		// Outdoors: name the structure whose loiter slot the actor is standing
		// at (a keeper at their own stall, a customer outside a shop), so Render
		// can say "outdoors by the General Store" rather than dumping coords.
		s.NearbyStructureName, curStructureID = findLoiterStructure(snap, a)
	}
	// LLM-154: a live dead-end read on the place the actor is physically at —
	// e.g. standing at a business no keeper is tending. Recomputed cold each tick
	// (not an arrival-event memory), so it fires the moment they arrive and
	// persists while they linger, robust to a failed/rerun arrival tick.
	if isShutBusiness(snap, a, curStructureID) {
		s.LocationDeadEnd = DeadEndShutBusiness
	} else if hungerDE, thirstDE := consumableDeadEndHere(snap, actorID, a); hungerDE || thirstDE {
		// LLM-176: the actor feels hunger/thirst, holds nothing to ease it, and no
		// source is co-located here — name the dead end so a weak model can't
		// confabulate food where there is none ("bread in the kitchen" at a foodless
		// residence). Disjoint from shut-business in practice (a residence has no
		// worker), so the else-if precedence only matters at a rare overlap.
		s.LocationDeadEnd = DeadEndNoConsumableHere
		s.DeadEndHunger = hungerDE
		s.DeadEndThirst = thirstDE
	}
	if a.CurrentHuddleID != "" {
		if h := snap.Huddles[a.CurrentHuddleID]; h != nil {
			// A co-present PC whose client has gone quiet (WS dropped, presence stamp
			// stale) stays in the huddle but is routed to HuddleAway, not the
			// addressable HuddleMembers — so it leaves every cue that reads
			// HuddleMembers (offerable customers, greet/respond, HasAudience) without
			// vanishing from "## Around you" (LLM-342). A published snapshot always
			// carries the threshold (> 0); a directly-constructed test snapshot that
			// omits it reads 0, which turns away-routing OFF (all co-present PCs stay
			// addressable — the legacy behavior), matching the SeekWorkCoinCeiling
			// test-snapshot convention.
			// LLM-467: is this a live conversation or just a room people haven't
			// left yet? Stamped off the same published huddle the members come
			// from, so the two can't disagree about which huddle was read.
			s.HuddleLive = sim.HuddleIsLive(h, snap.PublishedAt, snap.HuddleLiveWindow)
			staleAfter := snap.PCPresenceStaleAfter
			for memberID := range h.Members {
				if memberID == actorID {
					continue
				}
				m := resolveCoPresentMember(snap, actorID, a, memberID)
				if peer := snap.Actors[memberID]; staleAfter > 0 && peer != nil && peer.Kind == sim.KindPC &&
					sim.PCPresenceStale(peer.LastPCSeenAt, snap.PublishedAt, staleAfter) {
					s.HuddleAway = append(s.HuddleAway, m)
					continue
				}
				s.HuddleMembers = append(s.HuddleMembers, m)
			}
			sort.Slice(s.HuddleMembers, func(i, j int) bool {
				return s.HuddleMembers[i].ID < s.HuddleMembers[j].ID
			})
			sort.Slice(s.HuddleAway, func(i, j int) bool {
				return s.HuddleAway[i].ID < s.HuddleAway[j].ID
			})
		}
	} else {
		// Not huddled: surface who is within earshot — the set the speak path would
		// reach if the actor spoke now (ActorSnapshot.ColocatedAudienceIDs, computed
		// world-side so this line and the speak no-audience gate share one scope
		// rule). Same acquaintance gating as the huddle roster; the IDs arrive
		// pre-sorted from the world-side helper. ZBBS-WORK-407.
		for _, id := range a.ColocatedAudienceIDs {
			m := resolveCoPresentMember(snap, actorID, a, id)
			// A resting audience member can't be roused by THIS NPC's speech
			// (NPC-to-NPC speech doesn't interrupt rest — reactor.go
			// actorCanReactNow; only a PC / red-tier need / operator nudge does),
			// so it would sit silent if addressed. Route it to the not-addressable
			// clause like a sleeper. It stays in ColocatedAudienceIDs (the shared
			// audience / speak-gate set), so a PC — who CAN wake a rester — is
			// unaffected (ZBBS-WORK-426).
			if peer := snap.Actors[id]; peer != nil && peer.State == sim.StateResting {
				s.CoPresentResting = append(s.CoPresentResting, m)
				continue
			}
			m.JustArrived = coPresentJustArrived(snap, id)
			s.CoPresent = append(s.CoPresent, m)
		}
		// Co-present sleepers (ZBBS-WORK-426): excluded from the audience entirely
		// (colocatedSleeperIDs), surfaced here so Render marks them not-addressable
		// rather than dropping them from the actor's view.
		for _, id := range a.ColocatedSleeperIDs {
			s.CoPresentAsleep = append(s.CoPresentAsleep, resolveCoPresentMember(snap, actorID, a, id))
		}
	}
	return s
}

// resolveCoPresentMember builds a HuddleMember view for memberID: display name +
// role from the snapshot, acquaintance status from the subject's Acquaintances
// map. Shared by the huddle roster and the co-presence line (ZBBS-WORK-407) so
// both render with identical name-vs-descriptor gating.
func resolveCoPresentMember(snap *sim.Snapshot, subjectID sim.ActorID, subj *sim.ActorSnapshot, memberID sim.ActorID) HuddleMember {
	m := HuddleMember{ID: memberID}
	if peer := snap.Actors[memberID]; peer != nil {
		m.DisplayName = peer.DisplayName
		m.Role = peer.Role
		m.SolicitTie = laborTieFor(subj, peer)
		// LLM-547: what the subject's own history with this member says. Attached
		// HERE, in the one builder both the huddle roster and the co-presence line
		// share, so the fact cannot appear for a peer in one section and go missing
		// for the same peer in the other.
		//
		// Observer-relative, unlike every other annotation below: the lookup is
		// keyed (subject → member), so two actors looking at the same third party
		// legitimately get different tiers. Reading it off the co-present builder
		// also gives the co-presence guard for free — a pair with a rich history
		// renders nothing at all unless the peer is actually in the scene, because
		// this function only ever runs for someone who is.
		m.ContactTier, m.ContactRecentCount = snap.ContactTierFor(subjectID, memberID, snap.PublishedAt)
		// LLM-231: a peer fulfilling a hired job is dropped from the seller
		// offer/quote cue (m.Laboring, set for every observer — even the employer
		// shouldn't pitch a sale to their own mid-job worker) and rendered as busy
		// in "## Around you" for BYSTANDERS only (LaboringBystander). The peer's own
		// employer is suppressed from the annotation: they get the richer "## Workers
		// currently working for you" cue, and naming the employer to themselves reads
		// wrong. Gate the ledger scan on the cheap State mirror first; the ledger is
		// authoritative for the employer/until.
		if peer.State == sim.StateLaboring {
			if o := laboringOfferFor(snap, memberID); o != nil {
				m.Laboring = true
				if o.EmployerID != subjectID {
					m.LaboringBystander = true
					m.LaboringForLabel = employerLabelFor(snap, subj, o.EmployerID)
				}
			}
		}
		// LLM-416: surface a co-present member mid item-dwell (eating/drinking a
		// bought consumable at an eat-here source) as busy-eating in "## Around you",
		// so an onlooker — a proprietor especially — reads a lingering diner as still
		// at their meal rather than as someone about to leave, and stops re-issuing
		// farewells. Reuse buildActiveDwellCredits so the co-location + item/object
		// gating is identical to the eater's own self-cue and can't drift; take the
		// first item-source credit (an eater holds one).
		for _, dc := range buildActiveDwellCredits(snap, peer) {
			if dc.Source == sim.DwellSourceItem {
				m.Eating = true
				m.EatingItemLabel = itemDisplayLabel(snap, dc.Kind)
				break
			}
		}
		// LLM-440: surface a co-present member mid a timed source activity (mending a
		// business, tending a hearth fire, gathering at a source) as busy in "## Around
		// you", so an onlooker reads a keeper deep in a repair/stoke/gather as occupied
		// rather than free to greet or pitch. The observer half of the LLM-435 self-cue
		// suppression. The snapshot's SourceActivityKind is already BusyAtSource-gated at
		// projection (world.go) — its presence here means the window is genuinely in
		// flight, the SAME signal the subject's own in-flight self-line reads, so observer
		// and subject can't drift on who is busy. Gate to the "work" kinds that
		// busyActivityPhrase renders — repair/stoke/gather plus bake (LLM-454, the evening
		// home occupation): refresh (eat/drink at a source) is deliberately left to the
		// Eating annotation, and confining the flag to the handled kinds keeps
		// SourceActivityBusy and a rendered phrase in lockstep — a busy flag never renders
		// silent (nor would a future unhandled kind).
		switch peer.SourceActivityKind {
		case sim.SourceActivityRepair, sim.SourceActivityStoke, sim.SourceActivityHarvest, sim.SourceActivityBake:
			m.SourceActivityBusy = true
			m.SourceActivityKind = peer.SourceActivityKind
			m.SourceActivityLabel = resolveDwellPinLabel(snap, peer.SourceActivityObjectID)
		}
		// LLM-370: a co-present transient traveler is named by archetype + origin in
		// "## Around you" while the observer doesn't yet know them by name.
		if peer.VisitorState != nil {
			m.Traveler = true
			m.TravelerArchetype = peer.VisitorState.Archetype
			m.TravelerOrigin = peer.VisitorState.Origin
			// A returner reuses its DisplayName across visits, so a persistent NPC
			// that met it before already recognizes it BY NAME through the base
			// acquaintance path below (m.Acquainted) — no returner-specific observer
			// cue is needed here. The returner's own continuity (greeting a player it
			// remembers) rides the self-preface, not this observer line. (LLM-372)
		}
	}
	if m.DisplayName != "" {
		_, m.Acquainted = subj.Acquaintances[m.DisplayName]
	}
	return m
}

// employerLabelFor resolves the acquaintance-gated label of employerID from the
// subject's point of view — the name if the subject knows them, else the role or a
// generic descriptor — for the LLM-231 busy annotation. Empty when the employer
// isn't in the snapshot (render then omits the name). Mirrors the name-vs-descriptor
// gating resolveCoPresentMember applies to the member itself.
func employerLabelFor(snap *sim.Snapshot, subj *sim.ActorSnapshot, employerID sim.ActorID) string {
	if snap == nil || subj == nil {
		return ""
	}
	emp := snap.Actors[employerID]
	if emp == nil {
		return ""
	}
	_, acq := subj.Acquaintances[emp.DisplayName]
	return descriptorLabel(emp.DisplayName, emp.Role, acq)
}

// laborTieFor classifies how a co-present peer is bound to the subject for the
// no-solicit annotation (LLM-157): laborTieNone unless the SUBJECT is a worker
// (only a worker solicits for pay) AND shares the peer's household and/or
// workplace. Reuses the LLM-145 household/workplace predicates so the annotation
// and the solicit_work affordance gate read co-residence/co-employment identically.
func laborTieFor(subj, peer *sim.ActorSnapshot) laborTie {
	if !subjectIsWorker(subj) {
		return laborTieNone
	}
	switch {
	case sharesHousehold(subj, peer):
		return laborTieHousehold
	case sharesWorkplace(subj, peer):
		return laborTieWorkplace
	default:
		return laborTieNone
	}
}

// coPresentJustArrivedWindow bounds how long after an actor's arrival a
// co-present observer still reads it as "just arrived" in "## Around you"
// (ZBBS-WORK-422). The window trades catch-rate against staleness: a peer
// arrival stamps NO warrant on observers (the deliberate no-force-wake choice —
// greet/encounter huddles already cover the must-react cases), so an unhuddled
// observer only sees the tag when it ticks for its own reasons. The window must
// comfortably exceed the gap to that next organic tick, yet stay short enough
// that "just arrived" doesn't linger on someone who has clearly settled in.
const coPresentJustArrivedWindow = 90 * time.Second

// coPresentJustArrived reports whether memberID reached its current spot within
// coPresentJustArrivedWindow of the snapshot's publish time. It reads the
// arrival straight from the snapshot action log (every arrival is recorded as
// an ActionTypeWalked entry — see the action-log substrate), so it needs no new
// per-actor state and no checkpoint column for what is a transient signal. A
// member's most recent ActionTypeWalked IS its arrival at the current spot
// (moving away mints a fresh entry), so any such entry within the window means
// it just got here. O(log) per member; the log is small (capped retention).
func coPresentJustArrived(snap *sim.Snapshot, memberID sim.ActorID) bool {
	cutoff := snap.PublishedAt.Add(-coPresentJustArrivedWindow)
	for i := range snap.ActionLog {
		e := snap.ActionLog[i]
		if e.ActorID == memberID && e.ActionType == sim.ActionTypeWalked && !e.OccurredAt.Before(cutoff) {
			return true
		}
	}
	return false
}

// buildTurnState derives the subject's conversation turn-state (ZBBS-WORK-370)
// from the directed awaiting-reply edges among its present huddle peers. For
// each peer it answers two questions off the snapshot maps, applying the
// addressee-kind liveness window (snap.PublishedAt as the clock) so a lapsed
// edge is ignored — keeping the rendered nudge in lockstep with the sim.Speak
// backstop's expiry:
//
//   - does the SUBJECT await a live reply FROM this peer?  (subject's own edge,
//     window keyed on the peer = the addressee) -> AwaitingReplyFrom: "you spoke
//     to them, wait."
//   - does this PEER await a live reply from the SUBJECT? (peer's edge to me,
//     window keyed on the subject = the addressee) -> OwedReplyTo: "they are
//     waiting for your reply."
//
// Names are the same acquaintance-gated labels the huddle roster renders
// (descriptorLabel). members is already sorted by ID (buildSurroundings), so the
// output slices are deterministic. Returns the zero value (no lines) when the
// actor has no present peers or no live edges.
func buildTurnState(snap *sim.Snapshot, actorID sim.ActorID, subj *sim.ActorSnapshot, members []HuddleMember) TurnStateView {
	var ts TurnStateView
	if snap == nil || subj == nil || len(members) == 0 {
		return ts
	}
	now := snap.PublishedAt
	// Both windows are widened by the ADDRESSEE's reply pacing (LLM-536,
	// sim.ActorSnapshot.ReplyPacingWindow): a mid-job or mid-bake actor answers
	// NPC speech on a cadence, so an edge pointed at one has not lapsed merely
	// because the plain 60s window ran out — the addressee has not had its turn
	// yet. Un-widened, the "they are waiting for your reply" line expires ~2
	// minutes before a laboring worker can speak, and she draws her tick with
	// nothing marking the question as owed. Zero for everyone else, so this is a
	// no-op outside the two paced states.
	subjWindow := awaitWindowForKind(snap, subj.Kind) + subj.ReplyPacingWindow
	for _, m := range members {
		peer := snap.Actors[m.ID]
		if peer == nil {
			continue
		}
		label := descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
		// Subject addressed this peer and awaits their reply — the addressee is
		// the peer, so the window is keyed on the peer's kind and pacing.
		if awaitEdgeLive(subj.AwaitingReplyFrom, m.ID, now, awaitWindowForKind(snap, peer.Kind)+peer.ReplyPacingWindow) {
			ts.AwaitingReplyFrom = append(ts.AwaitingReplyFrom, label)
		}
		// This peer addressed the subject and awaits the subject's reply — the
		// addressee is the subject, so the window is keyed on the subject's kind
		// and pacing.
		if awaitEdgeLive(peer.AwaitingReplyFrom, actorID, now, subjWindow) {
			ts.OwedReplyTo = append(ts.OwedReplyTo, label)
		}
	}
	// LLM-232: extend the "await their reply" anchor to a plain spoken proposal
	// to the sole awake peer of a two-body huddle, at the coarser
	// ReaskSuppressWindow — the re-ask storm WORK-370's directed 60s edge misses
	// (an ask that named no addressee opened no edge; a directed one lapses
	// between minutes-apart re-asks). Only when no live directed edge already
	// anchors a peer (so the line never double-names one), only for the
	// unambiguous single-awake-peer case, and NOT while the huddle is in an armed
	// conversational loop (LLM-169): a looper is steered to act, not to wait — a
	// "wait for their reply" line would fight the loop-breaking coda, the same
	// reason ConversationLooping suppresses the owed-reply nag. Feeds the same
	// AwaitingReplyFrom the render + coda already consume — no new render path.
	// LLM-397: a lingering conversation suppresses the re-ask anchor for the same
	// reason the two loop flags do — every steer here is telling the actor to
	// close the scene, and a "wait for their reply" line would hold it open.
	if len(ts.AwaitingReplyFrom) == 0 && !subj.ConversationLooping &&
		!subj.ConversationRunLong && !subj.ConversationLingering {
		if label, ok := solePeerReaskAnchor(snap, actorID, subj, members); ok {
			ts.AwaitingReplyFrom = append(ts.AwaitingReplyFrom, label)
		}
	}
	// LLM-169: carry the publish-time armed-loop flag through so render can swap
	// the reply-pressure nudge for the "you've agreed, act now" coda. LLM-333:
	// the endurance flag rides the same way for the wind-down variant. LLM-397:
	// so does the lingering flag, for the ran-its-course variant.
	ts.ConversationLooping = subj.ConversationLooping
	ts.ConversationRunLong = subj.ConversationRunLong
	ts.ConversationLingering = subj.ConversationLingering
	return ts
}

// solePeerReaskAnchor computes the LLM-232 re-ask anchor for the subject: when
// exactly one present huddle peer is awake (not asleep or resting) and the
// subject last spoke in the huddle within sim.ReaskSuppressWindow AND more
// recently than that peer (the peer has said nothing back), it returns that
// peer's acquaintance-gated label to fold into AwaitingReplyFrom. Mirrors the
// sim.SpeakTo backstop (soleAwaitedPeerForReask) so the rendered "wait, don't
// repeat" line and the hard gate agree on when a re-ask is idle. Reads the
// huddle's recent-conversation ring off the snapshot; "now" is PublishedAt.
func solePeerReaskAnchor(snap *sim.Snapshot, actorID sim.ActorID, subj *sim.ActorSnapshot, members []HuddleMember) (string, bool) {
	h := snap.Huddles[subj.CurrentHuddleID]
	if h == nil {
		return "", false
	}
	var awake *HuddleMember
	count := 0
	for i := range members {
		p := snap.Actors[members[i].ID]
		if p == nil || p.State == sim.StateSleeping || p.State == sim.StateResting {
			continue
		}
		count++
		awake = &members[i]
	}
	if count != 1 {
		return "", false
	}
	subjLast := h.LastUtteranceAtBy(actorID)
	if subjLast.IsZero() || snap.PublishedAt.Sub(subjLast) >= sim.ReaskSuppressWindow {
		return "", false
	}
	if peerLast := h.LastUtteranceAtBy(awake.ID); !peerLast.Before(subjLast) {
		return "", false
	}
	return descriptorLabel(awake.DisplayName, awake.Role, awake.Acquainted), true
}

// awaitWindowForKind picks the turn-state liveness window for an edge whose
// ADDRESSEE is of the given kind, off the resolved snapshot windows (the
// Default*AwaitReplyWindow fallback is already applied at publish). PC addressee
// → the long window; every NPC kind → the short one.
func awaitWindowForKind(snap *sim.Snapshot, addresseeKind sim.ActorKind) time.Duration {
	if addresseeKind == sim.KindPC {
		return snap.PCAwaitReplyWindow
	}
	return snap.NPCAwaitReplyWindow
}

// awaitEdgeLive reports whether `edges` holds an entry for `key` that is still
// live at `now` under `window`. A missing entry, or one older than the window,
// is not live. window <= 0 means "no expiry configured" → an existing entry
// counts as live (the hand-built-snapshot posture; a published snapshot always
// carries a positive resolved window).
func awaitEdgeLive(edges map[sim.ActorID]time.Time, key sim.ActorID, now time.Time, window time.Duration) bool {
	stamp, ok := edges[key]
	if !ok {
		return false
	}
	if window <= 0 {
		return true
	}
	return now.Sub(stamp) < window
}

// actorMidSourceActivity reports whether the subject has a timed source activity
// in flight (gather/repair/stoke/refresh). The source-activity-START cues — the
// gatherable cue, the stall-repair cue, and the hearth cue — suppress on it.
// While a window is open the substrate rejects a fresh start ("you are already
// busy ..."), and the minted result lands only at completion, so the source
// object still reads as actionable mid-window (bush still stocked, stall still
// worn, fire still low). An un-suppressed cue would re-advertise its start tool
// to a busy actor and bait that reject on every reactor tick inside the window;
// the mid-activity triage coda (renderTriage) is what holds the actor to done()
// instead. The source-activity analogue of the walkIncompatibleTools drop for an
// in-flight move.
//
// This is the single definition of "mid a source activity" — buildInFlightSource-
// Activity (the coda/self-line view) gates on it too, so the suppression and the
// "you are …" self-state can never disagree about the state. Fail-closed by
// design: ANY non-empty kind counts, including an unknown/future one, because
// starting any timed source activity while one runs rejects. Nil-safe. LLM-435.
func actorMidSourceActivity(a *sim.ActorSnapshot) bool {
	return a != nil && a.SourceActivityKind != ""
}

// findGatherableCue resolves the gatherable bush the subject is loitering at and
// returns (item, sourceName, true), or ("", "", false) to suppress. It calls the
// SAME sim.ResolveGatherSource the gather command uses (LLM-93) — identical gate,
// loiter-pin math (computeLoiterTile, via snap.Assets), and candidate ranking — so
// the cue advertises exactly the bush gather will harvest, never a different one.
//
// The resolver returns an owned-by-other nearest so the COMMAND can raise
// ErrNotYourSource; the CUE instead SUPPRESSES it (don't dangle another's bush) and
// suppresses a non-gatherable nearest (obj/row nil). The gather tool advertisement
// (gateTools) reads this same SurroundingsView field, so suppressing un-advertises
// gather. Owner-gate: LLM-50 D2.
func findGatherableCue(snap *sim.Snapshot, subjectID sim.ActorID, a *sim.ActorSnapshot) (sim.ItemKind, string, bool) {
	if actorMidSourceActivity(a) {
		return "", "", false // mid a source-activity window — a fresh gather bounces "already busy at the source"
	}
	low := sim.LowForageItems(a.RestockPolicy, a.Inventory, snap.RestockReorderPct)
	_, obj, row := sim.ResolveGatherSource(snap.VillageObjects, snap.Assets, a.Pos, subjectID, a.GatherTargetObjectID, low, sim.ForageItems(a.RestockPolicy))
	if obj == nil || row == nil || obj.OwnedByOther(subjectID) {
		return "", "", false
	}
	// Suppress the cue when the resolved source is depleted. In a sparse plot
	// (bushes spaced wider than LoiterAttributionTiles) the only in-reach
	// candidate after a clean harvest is the empty bush just stripped, so
	// advertising it only baits futile gather attempts (LLM-98). The command
	// path resolves the same depleted source and errors cleanly, so dropping
	// the cue here introduces no cue↔command divergence — it just stops the cue
	// dangling a gather that can't succeed.
	if !row.HasStock() {
		return "", "", false
	}
	return sim.ItemKind(strings.TrimSpace(string(row.GatherItem))), obj.DisplayName, true
}

// findLoiterStructure returns the DisplayName and id of the structure whose
// loiter slot the subject is standing at (nearest within
// sim.LoiterAttributionTiles, Chebyshev), or ("", "") when none. A structure
// shares its id with a VillageObject (the placement), so the loiter pin is that
// object's tile anchor plus its EFFECTIVE loiter offset (sim.EffectiveLoiterOffset:
// per-instance → door → footprint fallback) — the same pin the substrate parks
// visitors on and move_to's effectiveLoiterTile checks against. The name drives the OUTDOORS
// position phrasing; the id is what the dead-end check (LLM-154) keys on. When
// the actor is genuinely inside a structure the caller reads InsideStructureID
// instead. Ties break by lowest structure id for determinism.
func findLoiterStructure(snap *sim.Snapshot, a *sim.ActorSnapshot) (string, sim.StructureID) {
	bestCheb := -1
	var bestName string
	var bestID sim.StructureID
	for stID, st := range snap.Structures {
		if st == nil || st.DisplayName == "" {
			continue
		}
		vobj := snap.VillageObjects[sim.VillageObjectID(stID)]
		if vobj == nil {
			continue
		}
		// Attribute the actor using the SAME effective loiter pin the substrate
		// parks visitors on — per-instance → door → footprint fallback
		// (sim.EffectiveLoiterOffset) — NOT the raw per-instance offset. A structure
		// with no explicit loiter override (the common case) parks visitors below
		// its door, several tiles from the anchor; reading the raw offset defaulted
		// the pin to the anchor tile, so an actor standing where move_to actually put
		// it (the door-pin) fell outside LoiterAttributionTiles and was never
		// attributed — the "outdoors by X" line AND the DeadEndShutBusiness shut cue
		// both silently dropped (LLM-327). EffectiveLoiterOffset is the single source
		// of truth move_to's effectiveLoiterTile resolves through.
		offX, offY := sim.EffectiveLoiterOffset(vobj, snap.Assets[vobj.AssetID])
		pin := vobj.Pos.Tile().Add(sim.TileOffset{DX: offX, DY: offY})
		cheb := a.Pos.Chebyshev(pin)
		if cheb > sim.LoiterAttributionTiles {
			continue
		}
		if bestCheb == -1 || cheb < bestCheb || (cheb == bestCheb && stID < bestID) {
			bestCheb = cheb
			bestName = st.DisplayName
			bestID = stID
		}
	}
	return bestName, bestID
}

// descriptorLabel renders an actor reference as the subject would name them:
// their DisplayName when acquainted, else "the <role>" (e.g. "the blacksmith")
// for a known trade, else "a stranger". The single source of truth for the
// name-vs-descriptor swap shared by HuddleMembers (renderHuddleMember) and the
// warrant actor-name map (buildWarrantActorNames) — ZBBS-HOME-339.
func descriptorLabel(displayName, role string, acquainted bool) string {
	if acquainted && displayName != "" {
		return displayName
	}
	if role != "" {
		return "the " + role
	}
	return "a stranger"
}

// buildTravelerSelf projects the subject's own VisitorState into the self-identity
// preface view (LLM-370), or nil when the subject is not a transient traveler.
func buildTravelerSelf(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) *TravelerSelfView {
	if a == nil || a.VisitorState == nil {
		return nil
	}
	v := &TravelerSelfView{
		Name:        travelerPersonaName(a.DisplayName),
		Archetype:   a.VisitorState.Archetype,
		Origin:      a.VisitorState.Origin,
		Disposition: a.VisitorState.Disposition,
		Vocation:    sim.VisitorVocation(a.VisitorState.Archetype),
		RoadWord:    a.VisitorState.Payload,
	}
	if v.Vocation == "" {
		v.Vocation = travelerBuyerVocation(snap, a.VisitorState)
	}
	if v.RoadWord != "" {
		v.RoadWordSharedWith, v.RoadWordSpentWithAllPresent = roadWordSharedWith(snap, actorID, a)
	}
	// LLM-372: a returner on a repeat visit carries continuity — how many times
	// they've passed through, and the players they remember (most-recent first).
	// ActorSnapshot.Returner is non-nil only for VisitCount >= 2, so a freshly
	// promoted first-visit traveler never claims to have been here before.
	if r := a.Returner; r != nil {
		v.VisitCount = r.VisitCount
		for _, k := range r.KnownHere {
			v.KnownHere = append(v.KnownHere, TravelerKnownPC{Name: k.DisplayName, Recency: k.Recency, Summary: k.Summary})
		}
	}
	return v
}

// travelerBuyerVocation is the merchant BUYER's vocation sentence (LLM-574), the arm
// sim.VisitorVocation leaves empty: its map covers the passer-through archetypes, and a
// merchant's label is minted from his errand instead (visitorMerchantLabel).
//
// "nail-buyer" names a trade and says nothing about what that trade DOES, so the model
// filled it in with the nearest real-world prior — a man going shop to shop with a bag
// of nails is selling them. Naming where the goods are bound gives the label its other
// half. Buyers only: a factor's calling IS to sell what he carries, and his errand cue
// already says so.
func travelerBuyerVocation(snap *sim.Snapshot, vs *sim.VisitorState) string {
	if snap == nil || vs == nil || vs.Trade == nil || vs.Trade.Direction != sim.TradeDirectionBuy {
		return ""
	}
	// The plural is what the sentence is built on ("You buy nails … carry them home"), so
	// an errand good with no catalog phrase drops the sentence rather than rendering "You
	// buy nail". Every live errand good is bound from a catalogued kind at spawn; this is
	// the same drop-on-empty posture VisitorVocation takes for an unknown archetype.
	// ItemKindDef.Plural is nil-receiver-safe, so an absent catalog row lands on the
	// same empty string as an unlabeled one.
	def := snap.ItemKinds[vs.Trade.Good]
	good := def.Plural()
	if good == "" {
		return ""
	}
	if vs.Origin != "" {
		return fmt.Sprintf("You buy %s in villages like this one and carry them home to %s, where your trade is.", good, vs.Origin)
	}
	return fmt.Sprintf("You buy %s in villages like this one and carry them home, where your trade is.", good)
}

// roadWordSharedWith intersects the traveler's shared-word memory
// (VisitorState.PayloadSharedWith, stamped on the Speak commit path — LLM-545)
// with present company, returning the display names of the co-present peers who
// have already had the word (sorted) and whether that covers EVERYONE present.
// Present company is the same set every other face-to-face cue reads: the
// huddle's members when huddled, else the co-located audience (which already
// excludes sleepers — someone who cannot hear fresh news is not a fresh
// listener). A peer told elsewhere and absent from the scene contributes
// nothing: the memory only matters face to face, mirroring the LLM-547 contact
// line's co-presence guard. allPresent is false for empty company — alone, the
// word is simply carried, not spent.
//
// Company keeps only ids the snapshot can actually resolve — a stale huddle
// member with no actor behind it must neither count toward "everyone here has
// heard it" nor block it. Away-routed PCs (stale presence, still huddle
// members) are deliberately KEPT: physically in the scene, an untold one keeps
// the fresh framing, which fails on the safe side.
func roadWordSharedWith(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) (names []string, allPresent bool) {
	if snap == nil || len(a.VisitorState.PayloadSharedWith) == 0 {
		return nil, false
	}
	shared := make(map[sim.ActorID]bool, len(a.VisitorState.PayloadSharedWith))
	for _, id := range a.VisitorState.PayloadSharedWith {
		shared[id] = true
	}
	var company []sim.ActorID
	appendPresent := func(id sim.ActorID) {
		if id != actorID && snap.Actors[id] != nil {
			company = append(company, id)
		}
	}
	if a.CurrentHuddleID != "" {
		if h := snap.Huddles[a.CurrentHuddleID]; h != nil {
			for id := range h.Members {
				appendPresent(id)
			}
		}
	} else {
		for _, id := range a.ColocatedAudienceIDs {
			appendPresent(id)
		}
	}
	if len(company) == 0 {
		return nil, false
	}
	allPresent = true
	for _, id := range company {
		if !shared[id] {
			allPresent = false
			continue
		}
		if name := snap.Actors[id].DisplayName; name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, allPresent && len(names) > 0
}

// travelerPersonaName recovers the bare persona name from a visitor's composed
// DisplayName ("Elias Drum the peddler", or "... (1234)" when disambiguated) by
// cutting at the LAST " the " — dispatchVisitorSpawn appends the archetype suffix
// last, so trimming the final marker keeps a persona name that itself contains
// " the " intact. Falls back to the whole DisplayName when the marker is absent (a
// hand-built snapshot), so the preface always has a name to open with.
func travelerPersonaName(displayName string) string {
	if i := strings.LastIndex(displayName, " the "); i >= 0 {
		return displayName[:i]
	}
	return displayName
}

// travelerCoPresentLabel renders a co-present transient traveler as a scene
// descriptor carrying archetype + origin ("a peddler lately come from Boston") —
// the observer half of the traveler legibility cue (LLM-370). Falls through to the
// generic descriptorLabel once the observer knows the traveler by name (acquainted)
// or for any non-traveler member, so acquaintance precedence matches every other
// co-present label. Archetype / origin are sanitized before composing (the same
// defense-in-depth the self-preface applies), and an archetype that sanitizes to
// empty degrades to the plain descriptor; an empty origin drops the "lately come
// from" clause.
func travelerCoPresentLabel(m HuddleMember) string {
	if !m.Traveler || m.Acquainted {
		return descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
	}
	archetype := sanitizeInline(m.TravelerArchetype)
	if archetype == "" {
		return descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
	}
	who := sim.WithIndefiniteArticle(archetype)
	if origin := sanitizeInline(m.TravelerOrigin); origin != "" {
		return who + " lately come from " + origin
	}
	return who
}

// buildWarrantPlaceNames resolves the place named by each warrant that carries
// one to its display name, so Render can name it instead of a vacuous phrase.
// Covers ArrivalWarrantReason — a structure (StructureEnter/Visit) or village
// object (ObjectVisit), for "You arrived at <place>" (ZBBS-WORK-358) — and
// StallRepairWarrantReason — the worn business, so the wake-from-anywhere repair
// nudge names it (LLM-247). Keyed by the raw id string (structure + object ids
// share one space under the shared-identity bridge). Returns nil when no warrant
// names a place (the common tick).
func buildWarrantPlaceNames(snap *sim.Snapshot, warrants []sim.WarrantMeta) map[string]string {
	// Build already returns early on a nil snapshot before reaching here, but
	// keep the helper independently safe for direct callers/tests (code_review).
	if snap == nil {
		return nil
	}
	var names map[string]string
	put := func(id, name string) {
		if id == "" || name == "" {
			return
		}
		if names == nil {
			names = make(map[string]string)
		}
		names[id] = name
	}
	for _, w := range warrants {
		switch r := w.Reason.(type) {
		case sim.ArrivalWarrantReason:
			if r.AtStructureID != "" {
				if st := snap.Structures[r.AtStructureID]; st != nil {
					put(string(r.AtStructureID), st.DisplayName)
				}
			}
			if r.AtObjectID != "" {
				if o := snap.VillageObjects[r.AtObjectID]; o != nil {
					put(string(r.AtObjectID), o.DisplayName)
				}
			}
		case sim.StallRepairWarrantReason:
			// Name the worn business (structure-first, then object) so the
			// wake-from-anywhere repair warrant line can say "Your <name> has worn."
			put(string(r.StallID), resolveDwellPinLabel(snap, r.StallID))
		case sim.StallRepairHiredWarrantReason:
			// LLM-271: hired-worker twin — name the employer's worn business the same
			// way, so its warrant line can name the place the worker is mending.
			put(string(r.StallID), resolveDwellPinLabel(snap, r.StallID))
		case sim.HearthLowWarrantReason:
			// LLM-412: name the structure whose fire wants wood, so the storm-time
			// wake can say "the fire at your <name>."
			put(string(r.HearthID), resolveDwellPinLabel(snap, r.HearthID))
		case sim.HearthStokeHiredWarrantReason:
			put(string(r.HearthID), resolveDwellPinLabel(snap, r.HearthID))
		}
	}
	return names
}

// buildWarrantPlaceKeepers resolves, for each arrival warrant whose destination
// structure has a keeper OTHER than the actor who arrived, that keeper's display
// name — so the arrival line can render the possessive "You arrived at
// <keeper>'s <structure>" (LLM-284). Keyed by the arrived structure id string.
// A keeper is any actor whose WorkStructureID is the arrived structure; the
// arriver is excluded so reaching one's own workplace keeps the plain form.
// Only structure arrivals carry a keeper — a village object (well, house) never
// resolves one, so ObjectVisit arrivals are skipped. Returns nil when no arrival
// names a keeper's workplace (the common tick).
func buildWarrantPlaceKeepers(snap *sim.Snapshot, warrants []sim.WarrantMeta) map[string]string {
	// Build returns early on a nil snapshot before reaching here; keep the helper
	// independently safe for direct callers/tests, mirroring buildWarrantPlaceNames.
	if snap == nil {
		return nil
	}
	var keepers map[string]string
	for _, w := range warrants {
		r, ok := w.Reason.(sim.ArrivalWarrantReason)
		if !ok || r.AtStructureID == "" {
			continue
		}
		// Only record a keeper when the structure itself resolves to a name, so a
		// keeper entry never outlives its WarrantPlaceNames entry (the arrival line
		// renders the possessive only when it already has a place name to attach it
		// to) — mirrors buildWarrantPlaceNames's snapshot-presence guard.
		if snap.Structures[r.AtStructureID] == nil {
			continue
		}
		name := snapshotStructureKeeperName(snap, r.AtStructureID, w.TriggerActorID)
		if name == "" {
			continue
		}
		if keepers == nil {
			keepers = make(map[string]string)
		}
		keepers[string(r.AtStructureID)] = name
	}
	return keepers
}

// buildEatHereKinds collects the kinds that always settle eat-here
// (ItemKindDef.EatHereOnly — consumable, neither service nor portable),
// so Render can state the disposition fact on a quote warrant line
// instead of leaving the model to discover the WORK-405 clamp by
// tripping it. Returns nil when the catalog has no eat-here-only kind.
func buildEatHereKinds(snap *sim.Snapshot) map[sim.ItemKind]bool {
	if snap == nil {
		return nil
	}
	var kinds map[sim.ItemKind]bool
	for kind, def := range snap.ItemKinds {
		if def.EatHereOnly() {
			if kinds == nil {
				kinds = make(map[sim.ItemKind]bool)
			}
			kinds[kind] = true
		}
	}
	return kinds
}

// buildOwnProducedKinds collects the item kinds the subject MAKES itself — its
// produce-source restock entries. The buyer-side producer-awareness guard
// (LLM-171) consults it to strip the actionable take from a buy-quote for a good
// the actor produces (a smith has no reason to buy back his own skillet).
// Returns nil when the actor has no produce policy.
func buildOwnProducedKinds(actorSnap *sim.ActorSnapshot) map[sim.ItemKind]bool {
	if actorSnap == nil || actorSnap.RestockPolicy == nil {
		return nil
	}
	var kinds map[sim.ItemKind]bool
	for _, e := range actorSnap.RestockPolicy.ProduceEntries() {
		if kinds == nil {
			kinds = make(map[sim.ItemKind]bool)
		}
		kinds[e.Item] = true
	}
	return kinds
}

// subjectProducesGoods reports whether the subject makes any goods itself — has
// at least one recipe-backed (makeable) produce entry. This is the same notion
// of "produces" the labor produce-boost keys on (produce_tick's makeableRecipe:
// a recipe present with a positive rate); a produce entry with no live recipe
// never actually mints, so it doesn't count. Only a producing keeper "gets more
// done" from hired help, so the returning-helper recall (renderLaborOffers,
// LLM-228) claims added output for a producer and stays a bare social beat
// otherwise. Pure over the snapshot; nil-safe.
func subjectProducesGoods(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot) bool {
	if snap == nil || actorSnap == nil || actorSnap.RestockPolicy == nil || snap.Recipes == nil {
		return false
	}
	for _, e := range actorSnap.RestockPolicy.ProduceEntries() {
		if r := snap.Recipes[e.Item]; r != nil && r.RateQty > 0 && r.RatePerHours > 0 {
			return true
		}
	}
	return false
}

// buildAtCapKinds collects the item kinds the subject already holds at or above
// its restock cap, across ALL restock sources with a configured cap (produce,
// buy, forage). The buyer-side guard (LLM-171) strips the actionable take from a
// buy-quote for such a good — at cap, buying more just overflows what the actor
// can carry. On-hand comes from the standing inventory readout (real carried
// goods). Returns nil when nothing is capped or at cap.
func buildAtCapKinds(actorSnap *sim.ActorSnapshot, inventory []InventoryItem) map[sim.ItemKind]bool {
	if actorSnap == nil || actorSnap.RestockPolicy == nil {
		return nil
	}
	// Sum per kind (+=, not =): the standing inventory is map-derived so kinds are
	// unique today, but the at-cap gate turns on an exact threshold compare — sum
	// defensively so a future multi-row inventory can't undercount past the cap.
	onHand := make(map[sim.ItemKind]int, len(inventory))
	for _, it := range inventory {
		onHand[it.kind] += it.Qty
	}
	var kinds map[sim.ItemKind]bool
	for _, e := range actorSnap.RestockPolicy.Restock {
		limit := e.Cap()
		if limit <= 0 {
			continue // no cap configured — never "at cap"
		}
		if onHand[e.Item] >= limit {
			if kinds == nil {
				kinds = make(map[sim.ItemKind]bool)
			}
			kinds[e.Item] = true
		}
	}
	return kinds
}

// customerProducedGoods returns the labels of the seller's goods that `customer`
// makes itself — the intersection of the seller's pitchable goods with the
// customer's produce manifest (LLM-171). Used to steer the seller off pitching a
// maker their own ware back (the buy-back loop). Nil-safe; returns nil when the
// customer makes none of them.
func customerProducedGoods(snap *sim.Snapshot, customer sim.ActorID, goods []OfferableGood) []string {
	if snap == nil {
		return nil
	}
	cust := snap.Actors[customer]
	if cust == nil || cust.RestockPolicy == nil {
		return nil
	}
	produced := make(map[sim.ItemKind]bool)
	for _, e := range cust.RestockPolicy.ProduceEntries() {
		produced[e.Item] = true
	}
	if len(produced) == 0 {
		return nil
	}
	var made []string
	for _, g := range goods {
		if produced[g.kind] {
			made = append(made, g.Label)
		}
	}
	return made
}

// buildWarrantActorNames resolves every OTHER actor referenced by a warrant in
// the batch to its acquaintance-gated label, so Render never leaks a raw actor
// UUID into the "## Since your last turn" lines (ZBBS-HOME-339). The subject's
// own ID is excluded — Render resolves self to "you". Returns nil when no
// warrant references another actor (the common single-actor tick).
func buildWarrantActorNames(snap *sim.Snapshot, subject *sim.ActorSnapshot, subjectID sim.ActorID, warrants []sim.WarrantMeta, payOffers []sim.PayOfferWarrantReason, laborOffers []LaborOfferView, workersForMe []WorkerForMeView, laboring *LaboringView, laborEnRoute *LaborEnRouteView, pendingLaborOut *PendingLaborOfferOutView, hireableWorkers []sim.ActorID, solicitableEmployers []sim.ActorID) map[sim.ActorID]string {
	var names map[sim.ActorID]string
	add := func(id sim.ActorID) {
		if id == "" || id == subjectID {
			return
		}
		if names == nil {
			names = make(map[sim.ActorID]string)
		}
		if _, done := names[id]; done {
			return
		}
		peer := snap.Actors[id]
		if peer == nil {
			// Actor gone from the snapshot (e.g. deleted between event and
			// publish). Leave it out; Render falls back to a neutral label.
			return
		}
		acquainted := false
		if peer.DisplayName != "" {
			_, acquainted = subject.Acquaintances[peer.DisplayName]
		}
		names[id] = descriptorLabel(peer.DisplayName, peer.Role, acquainted)
	}
	for _, w := range warrants {
		add(w.TriggerActorID)
		switch r := w.Reason.(type) {
		case sim.PCSpeechWarrantReason:
			add(r.Speaker)
		case sim.NPCSpeechWarrantReason:
			add(r.Speaker)
		case sim.PaidWarrantReason:
			add(r.Buyer)
		case sim.PayOfferWarrantReason:
			add(r.Buyer)
		case sim.SceneQuoteTargetedWarrantReason:
			add(r.SellerID)
		case sim.PayResolvedWarrantReason:
			add(r.Seller)
		case sim.ServeHandoverWarrantReason:
			add(r.Buyer)
		case sim.HuddlePartReason:
			// LLM-438: the actor's own join/leave names the huddle peers, so
			// each resolves through the same acquaintance gate — an
			// unacquainted peer renders as "the <role>" / "a stranger", never
			// its real name.
			for _, id := range r.PeerIDs {
				add(id)
			}
		}
	}
	// The standing offer view renders buyers on ticks that carry no offer
	// warrant (ZBBS-HOME-453), so their labels must resolve here too —
	// otherwise renderPayOffers falls back to "someone" the moment the
	// warranted tick has passed.
	for _, o := range payOffers {
		add(o.Buyer)
	}
	// LLM-26: the standing labor views render counterparties on ticks carrying
	// no warrant — the workers offering to me (LaborOffersForMe) and the
	// employer I'm working for (Laboring) — so their labels must resolve here
	// too, else render falls back to "someone."
	//
	// LLM-346: an offer awaiting my answer may name EITHER party — the worker who
	// solicited me, or the employer who asked me to lend a hand — so both sides of
	// every pending offer resolve. Adding only the worker left the employer-initiated
	// decision line reading "someone has asked you to do a job for them."
	for _, o := range laborOffers {
		add(o.Worker)
		add(o.Employer)
	}
	// LLM-202: the employer's active-job view (WorkersForMe) renders its workers
	// on ticks carrying no warrant — the standing "who's working for you" cue — so
	// their labels must resolve here too, else render falls back to "someone."
	for _, o := range workersForMe {
		add(o.Worker)
	}
	if laboring != nil {
		add(laboring.Employer)
	}
	// LLM-229: the relocation self-state (LaborEnRoute) renders its employer on
	// ticks carrying no warrant — the standing "you're on your way to / waiting
	// for X" cue — so the employer's label must resolve here too, else render
	// falls back to "someone."
	if laborEnRoute != nil {
		add(laborEnRoute.Employer)
	}
	// LLM-164: the subject's own pending-offer self-state (PendingLaborOfferOut)
	// renders its counterparty on ticks carrying no warrant — in particular the
	// idle/quiet-backstop wake the anchor exists to handle, whose only warrant
	// triggers on the subject itself — so that label must resolve here too, else
	// render falls back to "someone." Both roles, since either may have minted the
	// offer (LLM-346); the subject's own id resolves harmlessly.
	// LLM-346: the offer_work affordance names the co-present workers a keeper could
	// hire, on ticks carrying no warrant at all — it is a standing cue. Its whole
	// job is to supply a name the tool can resolve, so these must never fall back
	// to "someone." isHireableWorker already restricted the set to acquaintances,
	// so each resolves to a real DisplayName here.
	for _, id := range hireableWorkers {
		add(id)
	}
	// The solicit cue's named employers (LLM-564) — same contract as the
	// hireable workers above: buildSolicitableEmployers already restricted the
	// set to acquaintances, so each resolves to a real DisplayName here.
	for _, id := range solicitableEmployers {
		add(id)
	}
	if pendingLaborOut != nil {
		add(pendingLaborOut.Worker)
		add(pendingLaborOut.Employer)
	}
	return names
}

// minuteInWindow reports whether now (0..1439) falls in [start, end), handling
// wrap-midnight windows (start > end, e.g. a 16:00–03:00 tavern shift). Mirrors
// sim.minuteInShiftWindow / isActorOnShift — kept consistent on purpose:
// start == end is an EMPTY window (never on shift), not all-day. Replicated
// here (rather than calling into sim) so perception stays a pure reader of the
// snapshot and doesn't couple to the work-domain shift producer. ZBBS-HOME-352.
func minuteInWindow(start, end, now int) bool {
	if start == end {
		// Empty window — never on shift. Kept explicit (not folded into the
		// start<=end arm) so a later "simplify" can't turn it into an all-day
		// window; matches sim.minuteInShiftWindow's start==end rule.
		return false
	}
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}

// buildDutySteer computes the standing return-to-post cue: an agent NPC that is
// on-shift away from its workplace, or off-shift away from home. The shift
// window is the actor's own schedule when both bounds are set, else the world
// day-active (dawn/dusk) window from the snapshot — mirroring sim's
// effectiveShiftWindow. Position uses InsideStructureID (matching the engine's
// shiftDutyTarget notion of "at post"). Unlike the engine warrant it is NOT
// need-suppressed: it surfaces the duty and lets the model weigh it against any
// pressing need (the model-prioritizes design). Returns nil when at-post, out
// of scope, or the clock/anchors/window can't be resolved. ZBBS-HOME-352.
//
// a is guaranteed non-nil by Build's early return on a missing actor snapshot —
// the same invariant buildAnchors and the other sub-builders rely on.
func buildDutySteer(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot, anchors *AnchorsView, hasRestockErrand bool, forageErrand ForageErrandKind, hasUpkeepErrand bool) *DutySteerView {
	// Nil guard FIRST, so the a.Kind / clock dereferences below are safe even
	// when buildDutySteer is called directly (Build never passes a nil actor
	// snapshot, but the unit tests do). No anchors → no work/home to steer
	// toward; no clock → can't tell the hour. (code_review, HOME-400 Option B.)
	if snap == nil || a == nil || anchors == nil || snap.LocalMinuteOfDay == nil {
		return nil
	}
	// Agent NPCs only — PCs are player-driven; decoratives are walked directly
	// by the shift ticker and never get a perception prompt.
	if a.Kind != sim.KindNPCStateful && a.Kind != sim.KindNPCShared {
		return nil
	}
	// A pressing (red) need outranks duty: don't march an exhausted/starving NPC
	// to its post (or home) before it has addressed the need. Without this an
	// on-shift vendor with maxed tiredness deadlocks — at the stall the only rest
	// cue points elsewhere so it walks home to sleep, then this steer drags it
	// back before it ever rests, and the need never clears. Suppressing the steer
	// lets the recovery/satiation cues win this turn; once the need clears the
	// steer resumes next tick. The complementary "rest at your post" cue
	// (recovery_options.go) keeps an at-post vendor from leaving in the first
	// place, so the post stays manned. ZBBS-HOME-362. (The TO-WORK arm carries an
	// ADDITIONAL, softer mild-or-worse gate — ZBBS-HOME-400 Option B — in the
	// switch below; this red gate is the stronger one that also defers go-home.)
	if hasRedNeed(a, snap) {
		return nil
	}
	// LLM-414: a live summons outranks the routine steers — BOTH arms. The
	// live incident: the target was summoned just past dusk, the off-shift
	// go-home steer argued for home, and home won; the meeting never
	// happened. While the summons stands, the "## You have been summoned"
	// section is the single actionable movement voice (the model still
	// CHOOSES — decline by staying put or take_break is legitimate; the
	// engine just stops arguing against its own errand). Deliberately AFTER
	// the red-need gate: a starving target still eats first, and the summons
	// cue survives the detour (it no longer fades on unrelated acts).
	// shouldSkipNoop holds the noop gate open on SummonsForYou in this
	// steer's place, so the suppression cannot skip-lock the target.
	if summonsActive(snap, a) {
		return nil
	}
	// LLM-514: a constable mid-rounds is owned by the rounds cue — the single
	// movement voice while the tour runs. shiftDutyTarget already suppresses his
	// stand-at-post duty warrant while the route is active (w.ActiveRoutes), so the
	// perception steer must yield too, or it would argue "return to your post"
	// against the rounds he is deliberately walking. He returns to post on his own
	// when the tour ends and the route clears.
	if a.RouteLabel == sim.AttrConstable {
		return nil
	}
	nowMin := *snap.LocalMinuteOfDay

	start, end, windowOK := shiftWindowBounds(snap, a)
	if !windowOK {
		return nil // unscheduled and no usable day-active window
	}

	onShift := minuteInWindow(start, end, nowMin)
	atWork := anchors.WorkID != "" && a.InsideStructureID == anchors.WorkID
	atHome := anchors.HomeID != "" && a.InsideStructureID == anchors.HomeID

	switch {
	case onShift && anchors.WorkID != "" && !atWork:
		// ZBBS-HOME-400 Option B: don't yank an agent back to its post while it's
		// mid-business — an active restock errand or a pending outgoing offer
		// awaiting the seller's accept_pay (matching the shift-duty WARRANT). A RED
		// need already suppresses BOTH arms above (HOME-362). The mild-but-not-red
		// need gate HOME-400 also added here was REMOVED (ZBBS-HOME-463): a merely
		// peckish NPC should still clock in, and the mild gate stranded chronically-
		// needy NPCs (blocked from work yet not red enough to be driven to resolve —
		// e.g. a homeless blacksmith parked at the inn all shift). Scope: the to-work
		// arm ONLY — the go-home arm stays unsuppressed (going home is how an NPC rests).
		//
		// forageErrand (LLM-90): a grower stepping out to restock a bare sell-shelf
		// is the harvest-side twin of the restock errand —
		// the trip away from post IS the errand, so the to-work yank must defer it
		// too or it drags her back before she reaches the bushes (the buy-side
		// Josiah-Thorne oscillation, on the forage side). p.Forage is nil while a
		// customer is engaged (buildForage), so this never pulls her off a live sale.
		//
		// atResolvableSatiationSource (Moses James cycle, 2026-06-24): also don't
		// yank an agent that left its post for a felt hunger/thirst and has ARRIVED
		// at a source it can use right here — let it finish, or it ping-pongs
		// post<->source without ever consuming until the need goes red. Unlike the
		// removed HOME-463 mild gate this is LOCATION-gated (fires only once AT a
		// usable source) and coins-gated for paid vendors, so it can't re-strand the
		// homeless-blacksmith case — that NPC, broke and not yet at a free source,
		// still gets marched to work.
		//
		// hasUpkeepErrand (LLM-277): an owner off her post to buy nails to mend her
		// worn business (StallRepairBuy) or the shovels the season owes (FarmUpkeep)
		// is on a legitimate supply errand — the walk to the smith IS the errand — so
		// the to-work yank defers until she has fetched them, the buy-side twin of the
		// restock errand. Both cues clear once she carries enough, restoring the nag.
		// The to-work yank defers for an errand of EITHER kind — whose stock it is
		// only picks the at-post wording, never whether the trip counts (LLM-622).
		if hasRestockErrand || forageErrand.Errand() || hasUpkeepErrand || hasPendingOutgoingOffer(snap, actorID) || hasOfferedQuote(snap, actorID) || atResolvableSatiationSource(snap, actorID, a) {
			// LLM-620: suppress the YANK, keep the FACT. Returning nil here dropped the
			// only text in the prompt that ties the ambient hour to this actor's own
			// shift, and a weak model fills that vacuum by inventing the hour — Josiah
			// Thorne opened a turn "Morning's come" while standing in his house at 18:10
			// mid-shift. The status arm carries no destination id and no imperative, so
			// it cannot re-argue the walk this branch exists to defer; it only stops the
			// hour from being re-invented. Deliberately AFTER the red-need, summons and
			// rounds returns above: each of those is a case where duty framing is itself
			// unwanted, not merely its imperative.
			endMin := end
			return &DutySteerView{AwayFromPost: true, TargetLabel: anchors.WorkLabel, ShiftEndMin: &endMin}
		}
		return &DutySteerView{ToWork: true, TargetID: anchors.WorkID, TargetLabel: anchors.WorkLabel}
	case onShift && anchors.WorkID != "" && atWork:
		// At-post stabilizer (ZBBS-WORK-431): the symmetric complement to the
		// to-work yank above. On-shift and standing at its own post, an agent
		// previously got NO duty cue at all — and an idle owner with no custom
		// then read the anchors "head home whenever you wish" line as license to
		// wander, whereupon the away-from-post arm dragged it back, and it
		// oscillated (Prudence shop↔house, 2026-06-17). This view renders the
		// "stay put, don't wander" line and reframes the anchors invite. It is
		// render-only — excluded from shouldSkipNoop (AtPost), so an idle at-post
		// NPC with nothing happening still skips its idle-backstops (HOME-441).
		// Carry the effective close time (schedule end, else dusk fallback) so the
		// stabilizer can state when the shift ends — LLM-40.
		//
		// ForageErrand (LLM-90): when this same at-post grower has a bare sell-shelf
		// and ripe stock the forage cue names (forageErrand → p.Forage actionable,
		// which already excludes the mid-customer case), render flips the default
		// "stay and look after your work" steer for a "step out and return" line, so
		// the stabilizer agrees with the forage cue instead of contradicting it. She's
		// woken by the (now forage-aware) restock warrant, so this still renders only
		// on a tick that already runs. The kind rides through to render, which owns
		// the choice between the owned-bush and free-source wording (LLM-622).
		//
		// The buy-side twin, SupplyErrand (LLM-491), is deliberately NOT set here:
		// it reads sections the leisure/summons/degeneracy subtractions can still
		// nil after this returns, so Build derives it at the end of that chain.
		endMin := end
		return &DutySteerView{AtPost: true, ShiftEndMin: &endMin, ForageErrand: forageErrand}
	case !onShift:
		// Off-shift wind-down (ZBBS-WORK-387) — housing-dependent target. The
		// suppressors (windDownSuppressed: a mid-meal item dwell — WORK-386; an
		// unlapsed stay_open "open until" commitment while not peak-exhausted)
		// mirror shiftDutyTarget's go-home arm so cue and warrant agree.
		switch {
		case anchors.HomeID != "":
			// Homed → head home (the long-standing behavior).
			if atHome {
				return nil
			}
			// LLM-149 (Lever 2): inside the post-work evening window [shift-end,
			// 22:00) the go-home wind-down steer IS the "turn in" pressure the
			// epic forbids before Lever 1's 22:00 bedtime. Suppress it so the
			// evening leisure cue ("the tavern's open of an evening") is the
			// single voice in-window; after 22:00 this resumes and Lever 1 beds
			// the agent. buildEveningLeisure fires on the same window, and the
			// noop-skip gate keys on EveningLeisure in this steer's place so the
			// idle agent still ticks and sees the invitation.
			//
			// Suppression is keyed on the evening window (inEveningLeisure), not on
			// whether the evening cue actually rendered (code_review): "no turn-in
			// pressure during the evening" must hold even in the cue's cleared states —
			// most importantly once the agent has REACHED the tavern (the cue clears on
			// at-venue), it must not then be told to go home. Suppress-only-when-cue-
			// present would reintroduce exactly that nag. A village with no tavern placed
			// (degenerate) still counts as in-leisure: the agent gets no invitation and
			// still no turn-in pressure, idling cheaply until Lever 1 beds it.
			//
			// LLM-353: coin no longer gates the evening, so a purse-empty homed agent is
			// in evening leisure like anyone else — its wind-down is suppressed and it
			// gets the same evening. (The old LLM-205 broke-bed-at-shift-end behavior is
			// gone with the afford gate; Salem pays in goods, so "too poor for an evening"
			// described nobody.)
			if inEveningLeisure(snap, a) {
				return nil
			}
			if windDownSuppressed(a, snap) {
				return nil
			}
			return &DutySteerView{ToWork: false, TargetID: anchors.HomeID, TargetLabel: anchors.HomeLabel}
		default:
			// No home. Lodger → head to the rented room at the inn it lodges in
			// (the same soonest-active-grant the engine's windDownTarget resolves,
			// so cue and warrant agree).
			if innID, innName, ok := lodgerInn(snap, a); ok {
				if a.InsideStructureID == innID {
					return nil
				}
				// LLM-311: inside the evening window a lodger's wind-down is the same
				// premature "turn in" pressure the epic forbids before the 22:00
				// bedtime — suppress it so the evening-leisure cue is the single
				// voice, mirroring the homed arm above. inEveningLeisure now counts a
				// lodger with a night-place, so the cue (buildEveningLeisure) and this
				// suppression fire on the same window, and EveningLeisure holds the
				// noop-skip gate open in this steer's place.
				if inEveningLeisure(snap, a) {
					return nil
				}
				if windDownSuppressed(a, snap) {
					return nil
				}
				return &DutySteerView{ToWork: false, TargetID: innID, TargetLabel: innName, Lodging: true}
			}
			// Homeless → a directionless "find your rest for the night" nudge, fired
			// only while still lingering at the work post (atWork); once off the post
			// recovery_options + the homeless rest floor take over, and there is no
			// fixed place to march to. No TargetID — render gives the placeless line.
			if !atWork {
				return nil
			}
			if windDownSuppressed(a, snap) {
				return nil
			}
			return &DutySteerView{ToWork: false}
		}
	default:
		return nil
	}
}

// inEveningWindow reports whether the snapshot clock is inside the post-work
// evening window [shift-end, 22:00) for a homed day-shift agent — the slice
// during which the evening leisure cue (LLM-149) replaces the off-shift go-home
// wind-down steer. The window's open is the actor's effective shift end; its
// close is snap.LodgingBedtimeMinute (the 22:00 lodger bedtime — the same gate
// Lever 1 beds homed agents at, so cue and bedtime agree). Requires a normal
// (non-wrapping) shift whose end precedes the bedtime; wrap/night shifts and
// shifts running to or past bedtime have no evening (the in-scope day-workers
// end 19:00–21:00).
//
// LLM-352: an UNSCHEDULED worker is day-active on the world's dawn/dusk window
// (LLM-137), so shiftWindowBounds supplies its shift end and its evening becomes
// [dusk, bedtime) exactly like a dawn→dusk-scheduled worker's — the four labor
// vendors (the Walkers) were previously shut out because this keyed on a present
// schedule row. An unscheduled NON-worker has no shift at all (actorOnShift keeps
// it always-off — home is its resting state, HOME-204), so it gets no post-work
// evening: worker-gating here keeps inEveningWindow in step with actorOnShift
// rather than diverging via an ungated fallback. (See the HOME-204 tension noted
// on LLM-352 — leaving non-worker residents on the HOME-204 rule is deliberate.)
func inEveningWindow(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || snap.LocalMinuteOfDay == nil {
		return false
	}
	// A fully-scheduled actor's own window governs; a fully-UNSCHEDULED actor gets
	// the dawn/dusk fallback only if it is a worker (day-active). A partial schedule
	// (exactly one bound set) is malformed and earns no evening — matching the
	// pre-LLM-352 reject — and an unscheduled non-worker has no post-work evening
	// (home is its resting state, HOME-204).
	scheduled := a.ScheduleStartMin != nil && a.ScheduleEndMin != nil
	unscheduledWorker := a.ScheduleStartMin == nil && a.ScheduleEndMin == nil && subjectIsWorker(a)
	if !scheduled && !unscheduledWorker {
		return false
	}
	shiftStart, shiftEnd, ok := shiftWindowBounds(snap, a)
	if !ok {
		return false
	}
	if shiftStart >= shiftEnd {
		return false // wrap/degenerate shift — no simple post-work evening
	}
	bedtime := snap.LodgingBedtimeMinute
	if shiftEnd >= bedtime {
		return false // shift runs to/past bedtime — no evening window
	}
	nowMin := *snap.LocalMinuteOfDay
	return nowMin >= shiftEnd && nowMin < bedtime
}

// inDaytimeHomeWindow reports whether the actor is in its at-home daytime — the window
// the bake occupation (LLM-454) fills: the home-idle stretch of the DAY before dusk,
// where the household otherwise loops "let's make bread" without doing it (the LLM-453
// mid-afternoon loop). Daytime by the dawn/dusk clock, and NOT on an explicit scheduled
// shift. An UNSCHEDULED worker has no binding shift — its day-active window is not a post
// obligation, so it qualifies, which is exactly the looping homebodies; a SCHEDULED actor
// within its shift belongs at its post and is turned away. A non-worker unscheduled actor
// has no daytime occupation here (home is its resting state, mirroring inEveningWindow).
// Sibling of inEveningWindow (dusk→bedtime); this one is dawn→dusk.
func inDaytimeHomeWindow(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || snap.LocalMinuteOfDay == nil {
		return false
	}
	if !snap.DawnDuskMinuteOK || snap.DawnMinute >= snap.DuskMinute {
		return false
	}
	nowMin := *snap.LocalMinuteOfDay
	if nowMin < snap.DawnMinute || nowMin >= snap.DuskMinute {
		return false // before dawn or past dusk — not the daytime window
	}
	// A scheduled actor within its shift belongs at its post; an unscheduled actor (no
	// schedule) is never "on shift" here, so it falls through to the worker check. Same
	// half-open [start,end) with wrap as sim.isActorOnShift, so tool and cue agree.
	if a.ScheduleStartMin != nil && a.ScheduleEndMin != nil {
		s, e := *a.ScheduleStartMin, *a.ScheduleEndMin
		onShift := false
		if s <= e {
			onShift = nowMin >= s && nowMin < e
		} else {
			onShift = nowMin >= s || nowMin < e
		}
		return !onShift
	}
	return subjectIsWorker(a)
}

// subjectIsHomed reports whether the actor has a home structure that resolves in
// the snapshot — the same notion buildAnchors uses to set AnchorsView.HomeID.
func subjectIsHomed(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if a == nil || a.HomeStructureID == "" {
		return false
	}
	_, ok := resolveStructureLabel(snap, a.HomeStructureID)
	return ok
}

// subjectNightPlace returns the structure the actor winds down to for the night and
// a display label for it: its home if homed, else the inn where it holds an active
// lodging grant (lodgerInn). ok=false when the actor has neither — the genuinely
// homeless, who have no evening to spend. This is the "has somewhere to pass the
// evening" notion the living-evening scope keys on (LLM-311): a rent-a-room lodger
// (Ezekiel) has an evening exactly as a homed agent does, so the social-hour cue and
// the off-shift wind-down suppression must treat its rented inn as its home for the
// night. Home wins when both resolve.
func subjectNightPlace(snap *sim.Snapshot, a *sim.ActorSnapshot) (sim.StructureID, string, bool) {
	if subjectIsHomed(snap, a) {
		// resolveStructureLabel is guaranteed ok here — subjectIsHomed just checked it.
		label, _ := resolveStructureLabel(snap, a.HomeStructureID)
		return a.HomeStructureID, label, true
	}
	return lodgerInn(snap, a)
}

// inEveningLeisure reports whether the actor is in its post-work evening: a day-shift
// agent WITH A NIGHT-PLACE (homed, or a lodger holding an active room grant — LLM-311)
// inside the [shift-end, 22:00) window. It is the predicate behind the living-evening
// gates that don't care whether the agent has left its doorstep yet — the off-shift
// wind-down suppression (both the homed and lodger arms), the "tavern's open" cue
// (buildEveningLeisure), and settledAtLeisureVenue. An agent with no night-place at all
// is NOT in evening leisure: it has no evening, so the wind-down resumes (it beds /
// heads to its room at shift-end).
//
// LLM-353: coin no longer gates the evening. Salem pays in goods as readily as coin
// (pay_with_item carries a PayItems list beside its coin Amount), so a purse-empty agent
// with a full pack is not too poor for a mug of ale — the old canAffordLeisure floor
// measured the one field that isn't how the village pays. The model decides whether it
// wants a night out; a broke agent gets the invitation like anyone else. The two
// work-seeking suppressions no longer ride this predicate — afford-free, it degenerates
// to "off-shift with a bed" and would silence every homed worker's evening whether or not
// he ever went near the tavern. They key on tookEveningLeisure instead: on having
// actually gone to the pub.
func inEveningLeisure(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	_, _, hasNightPlace := subjectNightPlace(snap, a)
	return hasNightPlace && inEveningWindow(snap, a)
}

// insideLeisureVenue reports whether the actor is standing INSIDE a leisure venue —
// a structure whose shared-identity VillageObject carries the tavern venue tag. Keyed
// on the structure the actor actually occupies rather than on nearestTaggedVenue's
// pick, so a village with two taverns reads the one the agent is in (the nearest-venue
// comparison it replaces would have mistaken the farther tavern for "not at a venue").
func insideLeisureVenue(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || a.InsideStructureID == "" {
		return false
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(a.InsideStructureID)]
	return vobj != nil && vobj.HasTag(sim.VisitorTagTavern)
}

// headingToLeisureVenue reports whether the actor has an active move that will carry it
// INTO a leisure venue — walking in to the tavern. Mirrors insideLeisureVenue on the move
// destination: it is the same StructureEnter-aimed-at-a-tagged-venue shape buildEveningLeisure
// treats as "the invitation was acted on" (a move_to a structure resolves to a StructureEnter).
// Keyed on the destination carrying the venue tag rather than a specific venue id, so a
// village with two taverns reads whichever one the agent is walking to.
func headingToLeisureVenue(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || a.MoveDestKind != sim.MoveDestinationStructureEnter {
		return false
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(a.MoveDestStructureID)]
	return vobj != nil && vobj.HasTag(sim.VisitorTagTavern)
}

// leavingLeisureVenue reports whether an actor standing inside a venue has already
// committed to a walk out of it. InsideStructureID tracks the actor's CURRENT TILE
// (reconcileInsideAndNarrateDeparture keeps it honest each locomotion tick), so
// "inside the tavern AND walking to the blacksmith" is a real state that persists for
// every tick the actor is still crossing the tavern floor — not a transient. The
// departure is therefore any active move intent, with one exception: a StructureEnter
// aimed at the venue the actor already occupies is an arrival that has just reconciled,
// not a departure. A StructureVisit aimed at the same venue IS a departure — visitor
// slots stand outside the walls.
//
// Total rather than precondition-bound: an actor standing nowhere in particular cannot
// be leaving a structure, so the outdoors case answers false rather than relying on
// callers to have checked insideLeisureVenue first. code_review.
func leavingLeisureVenue(a *sim.ActorSnapshot) bool {
	if a == nil || a.InsideStructureID == "" || a.MoveDestKind == "" {
		return false
	}
	arrivingHere := a.MoveDestKind == sim.MoveDestinationStructureEnter && a.MoveDestStructureID == a.InsideStructureID
	return !arrivingHere
}

// settledAtLeisureVenue reports whether the actor is SETTLED in a leisure venue for its
// post-work evening (LLM-345) — inside, in-window, and not already walking out.
// This is the state in which the walk-away work-errand cues yield to the room (see Build).
//
// The "not leaving" half matters in both directions. An agent that has chosen to walk to
// the smith must not have the room re-argued at its back, and it must keep the upkeep cue
// that EXPLAINS the walk it is taking — suppressing the errand mid-errand would leave the
// prompt unable to account for its own in-flight move. code_review.
//
// inEveningLeisure already implies off-shift (its window opens at the actor's own shift
// end), so the tavernkeeper who lives and works in the tavern is excluded for free: his
// wrap schedule never enters an evening window, and his wares and restock cues survive in
// his own house.
func settledAtLeisureVenue(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	return insideLeisureVenue(snap, a) && !leavingLeisureVenue(a) && inEveningLeisure(snap, a)
}

// tookEveningLeisure reports whether the actor has actually TAKEN its post-work evening at
// the tavern — standing inside the venue, or walking in to it — during the evening window.
// This is the re-key of the two work-seeking suppressions (LLM-353: CanSolicitWork and the
// SeekWorkPlaces directory): job-hunting yields to a worker who has GONE to the pub, not to
// one who merely could afford a drink. A worker still standing in the road at dusk keeps its
// seek-work cues; one who has crossed the threshold (or is on its way) does not.
//
// Gated on inEveningWindow so it stays an evening behavior — a worker who ducks into the
// tavern mid-shift (never off-shift, so inEveningWindow is false) is unaffected, matching the
// window inEveningLeisure carried before the coin gate came out. Unlike inEveningLeisure it
// does not require a night-place: physically being at the pub is the signal, and the seek-work
// gates it feeds are already worker-scoped.
func tookEveningLeisure(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	return inEveningWindow(snap, a) && (insideLeisureVenue(snap, a) || headingToLeisureVenue(snap, a))
}

// nearestTaggedVenue returns the structure id + display label of the nearest
// VillageObject carrying tag that is bridged to a Structure (shared id), by
// Chebyshev distance to the actor, ties broken on the smaller structure id for
// determinism. The "tavern" venue tag lives on the VillageObject, not the
// Structure (Structure.Tags is unused), and a placed venue is the tagged object
// whose id also names a Structure — the same shared-identity bridge
// pickVisitorDestination and the create_pc lodging lookup use. Returns ok=false
// when no such venue is placed (then the caller renders no cue rather than
// hardcoding an id).
func nearestTaggedVenue(snap *sim.Snapshot, a *sim.ActorSnapshot, tag string) (sim.StructureID, string, bool) {
	if snap == nil || a == nil {
		return "", "", false
	}
	var bestID sim.StructureID
	var bestLabel string
	bestDist := -1
	for id, vobj := range snap.VillageObjects {
		if vobj == nil || !vobj.HasTag(tag) {
			continue
		}
		stID := sim.StructureID(id)
		st, ok := snap.Structures[stID]
		if !ok || st == nil {
			continue // tagged object with no backing Structure — not a venue we steer to
		}
		d := a.Pos.Chebyshev(vobj.Pos.Tile())
		if bestDist == -1 || d < bestDist || (d == bestDist && stID < bestID) {
			bestDist = d
			bestID = stID
			bestLabel = st.DisplayName
		}
	}
	if bestDist == -1 {
		return "", "", false
	}
	return bestID, bestLabel, true
}

// buildEveningLeisure computes the evening "tavern's open" cue (LLM-149, Lever 2
// of the living-evening epic). It fires for a homed, day-shift agent NPC that is
// off-shift and awake inside an AFFORDABLE evening window [shift-end, 22:00) —
// LLM-205 gates the cue on being able to afford the tavern's cheapest drink —
// naming the tavern as a place to pass the evening and letting the model choose — head
// over, stay in, or turn in — with NO forced walk. The off-shift go-home steer
// is suppressed on the same window (buildDutySteer) so this is the single voice.
//
// Clear/defer conditions — the cue-catalog discipline (a standing cue with no
// clear is a loop): a RED need outranks idle leisure (let satiation/recovery
// win, matching the duty steer); already at home → chose to stay in, don't pull
// back out; walking to either offered destination → acted on, don't re-pump (the
// Ezekiel lesson, both sides — the in-flight line + mid-walk coda keep a walking
// agent on course). Arriving at the venue does not clear the view but SWITCHES it
// to the settled-in tier (LLM-345): the invitation stops, the evening does not.
// A lodger with a paid room is IN scope (LLM-311) — its
// night-place is the rented inn; only the genuinely homeless (no home, no room
// grant) stay on the duty steer's placeless wind-down arm.
func buildEveningLeisure(snap *sim.Snapshot, a *sim.ActorSnapshot, anchors *AnchorsView) *EveningLeisureView {
	if snap == nil || a == nil {
		return nil
	}
	// Agent NPCs only — same scope as the duty steer (PCs are player-driven,
	// decoratives are never perceived).
	if a.Kind != sim.KindNPCStateful && a.Kind != sim.KindNPCShared {
		return nil
	}
	// Awake only (the ticket's "awake" requirement). A homed agent should be awake
	// through the evening window — Lever 1 beds it at 22:00, the window's close —
	// but guard explicitly so a sleeping actor that reaches perception is never
	// cued to the tavern. code_review.
	if a.State == sim.StateSleeping {
		return nil
	}
	// LLM-414: a live summons silences the evening invitation, same as it
	// silences the go-home steer — the "come to <place>" cue must be the
	// single movement voice while it stands, or the tavern argues against
	// the meeting the same way home did in the live incident.
	if summonsActive(snap, a) {
		return nil
	}
	// Transient traveler (LLM-373): a homeless visitor has no night-place of its
	// own, so the resident subjectNightPlace / inEveningLeisure gates below exclude
	// it — but of an evening it is drawn to the tavern like anyone, for company and
	// to seek its bed. Its own arm resolves the tavern and offers no home
	// destination; it needs no home anchor, so it runs BEFORE the resident anchors
	// guard (buildAnchors returns nil for a homeless, workless traveler). Residents
	// (VisitorState nil) fall through to the standard path unchanged.
	if a.VisitorState != nil {
		return buildVisitorEveningLeisure(snap, a)
	}
	// Residents past here need a resolved home/work anchor (the standard path reads
	// subjectNightPlace / offers a home destination).
	if anchors == nil {
		return nil
	}
	// Night-place: the structure the agent winds down to — its home if homed, else
	// the inn it holds an active room grant at (LLM-311). The homeless (neither) get
	// no cue and stay on the placeless wind-down arm. Resolved here rather than off
	// anchors.HomeID so a lodger (anchors.HomeID == "") is included; for a homed agent
	// this yields the same id/label buildAnchors set.
	nightID, nightLabel, ok := subjectNightPlace(snap, a)
	if !ok {
		return nil
	}
	// Inside the post-work evening window with a night-place (inEveningLeisure): the
	// day-shift scope guard behind the invitation. LLM-353 removed the coin gate, so a
	// broke agent is invited like anyone else — Salem pays in goods, and the model decides
	// whether it wants a night out. (Re-checks the night-place too — already guaranteed
	// above.)
	if !inEveningLeisure(snap, a) {
		return nil
	}
	// A pressing (red) need outranks an idle evening; the cue resumes once it
	// clears.
	if hasRedNeed(a, snap) {
		return nil
	}
	// Settled at the night-place → stay-in choice already made; don't re-pump back out.
	if a.InsideStructureID == nightID {
		return nil
	}
	// Inside the venue → the invitation was acted on, so it must not be re-pumped. But
	// LLM-345: the evening framing does NOT vanish at the threshold with it. A bare
	// `return nil` here left the agent's standing WORK content (the farm-upkeep errand,
	// the anchors line offering its workplace) as the only voice in the room, under a
	// coda that ranks obligations above idle matters — and the model, reading a farm
	// ledger where a tavern should be, walked back out. Render the room instead: a
	// destination-free scene the agent has nothing to act on, which is exactly why it
	// stays render-only (see Invitation) and why Build silences the errand cues beside it.
	if insideLeisureVenue(snap, a) {
		// Already walking out — the choice to leave is made; don't argue with it at the
		// agent's back (the same anti-pester posture as the walk guard below). Any
		// destination counts, not just the night-place: the model may walk out to the
		// smith or its farm, and the room must not be re-pumped over an in-flight walk.
		if leavingLeisureVenue(a) {
			return nil
		}
		venueLabel, _ := resolveStructureLabel(snap, a.InsideStructureID)
		return &EveningLeisureView{SettledIn: true, VenueLabel: venueLabel}
	}
	// Resolve the venue from the snapshot — no tavern placed → no cue.
	venueID, venueLabel, ok := nearestTaggedVenue(snap, a, sim.VisitorTagTavern)
	if !ok {
		return nil
	}
	// Walking to EITHER offered destination — the tavern, or the night-place (the cue
	// gives it as an actionable token too) → the choice is made; don't re-pump the
	// same invitation at the agent's back the whole walk (the Ezekiel lesson; the
	// in-flight move line + mid-walk coda already keep it on course). code_review.
	if a.MoveDestKind == sim.MoveDestinationStructureEnter &&
		(a.MoveDestStructureID == venueID || a.MoveDestStructureID == nightID) {
		return nil
	}
	// LLM-335: a keeper standing at its post with a batch in the works is pinned
	// there — under the LLM-319 pause model production only advances while it stays
	// inside its work structure, and the standing "you are making a batch of X — it
	// only moves along while you're at your post" line renders alongside. Handed the
	// tavern invitation on the same tick, the scene pulls the keeper two ways at once
	// (the pester that surfaced live: nagged to the tavern AND to stay for the cheese).
	// Yield to a quiet diegetic hold — the batch still wants their eye — and let the
	// invitation return once nothing is in the works. Checked here, past all the
	// invitation's own clear conditions, so the hold appears in exactly the states the
	// invitation would have (in-window, awake, a venue placed) and nowhere
	// new. AT-POST only: a keeper that wandered off with a paused batch is free to pass
	// the evening as it likes. Engine computes the pin; render picks ONE steer.
	atPost := a.WorkStructureID != "" && a.InsideStructureID == a.WorkStructureID
	if atPost && a.ProductionItem != "" {
		return &EveningLeisureView{
			BatchHold:      true,
			BatchItemLabel: itemDisplayLabel(snap, a.ProductionItem),
		}
	}
	return &EveningLeisureView{
		VenueID:    venueID,
		VenueLabel: venueLabel,
		HomeID:     nightID,
		HomeLabel:  nightLabel,
	}
}

// atResolvableSatiationSource reports whether the actor is standing AT a source
// that can satisfy a currently-felt hunger/thirst need right here — a co-present
// peer holding a satisfier, a free public source at its tile, or a vendor
// structure it is at and has coins to pay. It gates the to-work duty yank so an
// on-shift NPC that left its post to slake a need and has arrived at the source
// is allowed to finish (the Moses James post<->stall cycle). The felt-need gate
// matches buildSatiation's (NeedSilent floor), so the suppressor fires exactly
// when the eat/drink cue is offering a usable option here.
//
// Why this doesn't reopen ZBBS-HOME-463 (the removed mild gate that stranded the
// homeless blacksmith at the inn): it is LOCATION-gated — it fires only once the
// NPC is AT a usable source, never while it merely feels peckish somewhere with
// no resolution — and the paid-vendor arm is coins-gated, so a broke NPC at a
// stall it can't transact at is still marched to work. It self-clears the instant
// the need eases.
func atResolvableSatiationSource(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil {
		return false
	}
	for _, need := range satiationNeeds {
		if sim.NeedLabelTier(a.Needs[need], snap.NeedThresholds.Get(need)) == sim.NeedSilent {
			continue
		}
		// A co-present huddle peer carrying a satisfier — already beside the actor.
		if len(gatherCoPresentPeerOffers(snap, actorID, a, need)) > 0 {
			return true
		}
		// A free public source the actor is standing at (its loiter pin — the tile
		// locomotion parks a visitor on, which may be offset from the base tile).
		for _, obj := range snap.VillageObjects {
			if obj == nil || objectRefreshMagnitude(obj, need) <= 0 || obj.OwnedByOther(actorID) {
				continue
			}
			if a.Pos.Chebyshev(objectLoiterPin(obj)) <= sim.LoiterAttributionTiles {
				return true
			}
		}
		// A vendor structure the actor is standing at and can pay for (coins>0).
		// Pricing in v2 is negotiated per-transaction with no fixed retail price,
		// so coins>0 is the affordability proxy — it cleanly excludes the broke
		// homeless-blacksmith case while admitting the ordinary "I have money, let
		// me buy a drink" one.
		if a.SpendableCoins() > 0 {
			for _, vc := range findVendorConsumables(snap, actorID, need, "") {
				if vc.StructureID != "" && actorAtStructure(snap, a, vc.StructureID) {
					return true
				}
			}
		}
	}
	return false
}

// actorAtStructure reports whether the actor is at a structure: inside it, or
// standing within LoiterAttributionTiles of its loiter pin (the same "outdoors
// by X" attribution findLoiterStructure uses for the location line).
func actorAtStructure(snap *sim.Snapshot, a *sim.ActorSnapshot, stID sim.StructureID) bool {
	if snap == nil || a == nil || stID == "" {
		return false
	}
	if a.InsideStructureID == stID {
		return true
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(stID)]
	if vobj == nil {
		return false
	}
	return a.Pos.Chebyshev(objectLoiterPin(vobj)) <= sim.LoiterAttributionTiles
}

// consumableDeadEndHere reports, per consumable need, whether the structure the
// actor is INSIDE offers no way to resolve a felt hunger/thirst it holds no
// satisfier for — the LLM-176 dead end behind the "I saw bread in the kitchen"
// confabulation at a foodless residence. A need is a dead end here when it is FELT
// (NeedSilent floor — the same gate the eat/drink cue uses), the actor carries
// NOTHING that eases it (raw gatherOwnStock, so even desperation trade stock
// counts as food on hand), AND no source for it is co-located
// (consumableSourceColocated). Returns which needs are dead ends so deadEndClause
// can name eat vs drink vs both.
//
// Gated on being INSIDE a structure: the confabulation is a structure-bound
// belief ("food in the kitchen") and the cue's "here" reads as that room. An
// actor outdoors has no enclosed "here" to imagine food in, and firing in the
// open would spam the cue on every foodless tile — the satiation directory and
// the duty steer already guide a wandering hungry actor.
func consumableDeadEndHere(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot) (hunger, thirst bool) {
	if snap == nil || a == nil || a.InsideStructureID == "" {
		return false, false
	}
	for _, need := range satiationNeeds {
		if sim.NeedLabelTier(a.Needs[need], snap.NeedThresholds.Get(need)) == sim.NeedSilent {
			continue // not felt — no dead end for it
		}
		if len(gatherOwnStock(snap, a, need)) > 0 {
			continue // carries a satisfier — can consume in place, not a dead end
		}
		if consumableSourceColocated(snap, actorID, a, need) {
			continue // a usable source is right here
		}
		switch need {
		case "hunger":
			hunger = true
		case "thirst":
			thirst = true
		}
	}
	return hunger, thirst
}

// consumableSourceColocated reports whether a source that eases `need` is at the
// actor's current location: a co-present huddle peer holding a satisfier, a free
// public source on its tile, or a vendor structure it is standing at. It is the
// "here" half of atResolvableSatiationSource, but COINS-BLIND on the vendor arm:
// a shop at this spot means food EXISTS here (so it isn't the no-food dead end)
// even when the actor can't afford it — that's a purse problem, not a missing-
// affordance one. A shut shop is handled separately as DeadEndShutBusiness, which
// takes precedence in buildSurroundings.
func consumableSourceColocated(snap *sim.Snapshot, actorID sim.ActorID, a *sim.ActorSnapshot, need sim.NeedKey) bool {
	if len(gatherCoPresentPeerOffers(snap, actorID, a, need)) > 0 {
		return true
	}
	for _, obj := range snap.VillageObjects {
		if obj == nil || objectRefreshMagnitude(obj, need) <= 0 || obj.OwnedByOther(actorID) {
			continue
		}
		if a.Pos.Chebyshev(objectLoiterPin(obj)) <= sim.LoiterAttributionTiles {
			return true
		}
	}
	for _, vc := range findVendorConsumables(snap, actorID, need, "") {
		if vc.StructureID != "" && actorAtStructure(snap, a, vc.StructureID) {
			return true
		}
	}
	return false
}

// buildNeedRedirect returns the one-target loop-break steer for a socially-
// looping actor with a felt consumable need and a resolvable source already in
// the satiation view (LLM-176), or nil. Gated on ConversationLooping — it feeds
// only the looping coda. For the most-pressing felt need (a red need before a
// merely-felt one; hunger before thirst on a tie), it picks the first resolvable
// affordance in the same order the eat/drink cue lists them: consume what's
// carried, else the nearest free source, else the nearest usable vendor. nil when
// nothing resolves → the generic looping coda renders.
func buildNeedRedirect(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, sat *SatiationView) *NeedRedirectView {
	if snap == nil || actorSnap == nil || !actorSnap.ConversationLooping || sat == nil {
		return nil
	}
	for _, nv := range pressingFirst(snap, sat.Needs) {
		if r := needRedirectFor(nv, actorSnap.SpendableCoins()); r != nil {
			return r
		}
	}
	return nil
}

// needRedirectFor resolves the single redirect target for one felt need, in the
// satiation cue's own priority order: consume what's carried, else the nearest
// free source (FreeSources is already nearest-first), else the nearest usable
// vendor (Vendors is already nearest-first). A vendor is usable when it is not
// remembered out of stock and is payable — payability is experiential (LLM-176):
// a known remembered price the actor can't meet by coin skips it, but an unknown
// price (costCoins == 0, never bought there) does NOT — the actor walks over and
// learns it. A vendor the satiation gate marked Barter (coins short but goods to
// trade, LLM-222) also stays: it's a real target the actor can transact at, so
// skipping it would drift from the rendered buy cue. Remembered-shut vendors never
// reach this list — the satiation build gate drops them (LLM-222) — so no shut
// check is needed here. nil when the need has no resolvable target.
//
// LLM-307: consume-what-you-carry is skipped when the eat/drink section is
// bridging past snack-only stock to a real meal (nv.BridgeToMeal). The section
// tells the actor a nibble won't resolve the need and to look to the meal options
// below; a coda that then said "consume your nibble now" would contradict it, and
// re-arm the snacking loop the loop-break exists to end. Under BridgeToMeal the
// walk-to arms are also restricted to MEAL-class targets — a nibble free source or
// nibble vendor wouldn't resolve the need any better than the carried snack, so
// steering to one would just relocate the same contradiction. BridgeToMeal is set
// only when a meal-class option is actually reachable (buildSatiation's honesty
// gate), so a meal target is normally found; when the only meal is a co-present
// peer (which the redirect can't name as a move_to target) the loop falls through
// to nil and the generic looping coda renders — which does not contradict the
// section. With BridgeToMeal false (a meal on hand, or no own stock at all) the
// original order is unchanged.
func needRedirectFor(nv SatiationNeedView, coins int) *NeedRedirectView {
	if len(nv.OwnStock) > 0 && !nv.BridgeToMeal {
		return &NeedRedirectView{Kind: NeedRedirectConsume, Verb: nv.Verb, ItemLabel: nv.OwnStock[0].Label}
	}
	for _, fs := range nv.FreeSources {
		if nv.BridgeToMeal && !isMealClassSatisfier(fs.Magnitude) {
			continue // a snack source resolves the need no better than the carried snack
		}
		return &NeedRedirectView{Kind: NeedRedirectFree, Verb: nv.Verb, TargetLabel: fs.Label, TargetID: string(fs.ObjectID)}
	}
	for _, v := range nv.Vendors {
		if v.OutOfStock {
			continue
		}
		if v.costCoins > 0 && coins < v.costCoins && !v.Barter {
			continue // a remembered price the looping actor can neither meet by coin nor barter (LLM-222)
		}
		if nv.BridgeToMeal && !isMealClassSatisfier(v.Magnitude) {
			continue // under a meal bridge, only a meal-class vendor resolves the need
		}
		return &NeedRedirectView{Kind: NeedRedirectBuy, Verb: nv.Verb, ItemLabel: v.ItemLabel, TargetLabel: v.StructureLabel, TargetID: string(v.StructureID)}
	}
	return nil
}

// pressingFirst orders the felt consumable needs most-pressing first: a red-tier
// need before a merely-felt one, otherwise the fixed satiation order (hunger
// before thirst). Stable, so the fixed order breaks tier ties.
func pressingFirst(snap *sim.Snapshot, needs []SatiationNeedView) []SatiationNeedView {
	out := append([]SatiationNeedView(nil), needs...)
	sort.SliceStable(out, func(i, j int) bool {
		return needRedTier(snap, out[i]) && !needRedTier(snap, out[j])
	})
	return out
}

// needRedTier reports whether a satiation need is at its red threshold or worse —
// the redirect-priority lever (a pressing need outranks a merely-felt one). nil
// snap (no callers today — buildNeedRedirect guards it) yields false, so
// pressingFirst degrades to the stable satiation order rather than panicking.
func needRedTier(snap *sim.Snapshot, nv SatiationNeedView) bool {
	if snap == nil {
		return false
	}
	return sim.NeedLabelTier(nv.Level, snap.NeedThresholds.Get(nv.Need)) == sim.NeedRed
}

// objectLoiterPin returns the tile an actor stands on when "at" obj — its base
// tile plus any loiter offset. This is the pin locomotion parks visitors on and
// the pin findLoiterStructure attributes "outdoors by X" to, so co-location
// checks must measure to it, not the bare base tile.
func objectLoiterPin(vobj *sim.VillageObject) sim.TilePos {
	pin := vobj.Pos.Tile()
	off := sim.TileOffset{}
	if vobj.LoiterOffsetX != nil {
		off.DX = *vobj.LoiterOffsetX
	}
	if vobj.LoiterOffsetY != nil {
		off.DY = *vobj.LoiterOffsetY
	}
	return pin.Add(off)
}

// isShutBusiness reports whether stID is a business the actor is standing at
// that nobody is tending — the live, situated dead-end the at-location cue
// surfaces (LLM-154). It mirrors the sim-side capture gate (validBusiness
// composed with !businessTendedAt, closed_business.go) but reads the snapshot: a
// business is a structure someone works (snapshotStructureHasWorker), the
// actor's OWN workplace is excluded (you don't read your own shop as shut — you
// are its keeper), and "tended" means somebody is minding it
// (snapshotBusinessTended). False for an empty id, a residence (no workers), or
// an attended business.
func isShutBusiness(snap *sim.Snapshot, a *sim.ActorSnapshot, stID sim.StructureID) bool {
	if snap == nil || a == nil || stID == "" || stID == a.WorkStructureID {
		return false
	}
	return snapshotStructureHasWorker(snap, stID) && !snapshotBusinessTended(snap, stID)
}

// snapshotStructureHasWorker reports whether any actor in the snapshot has stID
// as its workplace — the snapshot mirror of sim.structureHasWorker. A structure
// no one works is a residence, not a business, so it can't read as "shut".
func snapshotStructureHasWorker(snap *sim.Snapshot, stID sim.StructureID) bool {
	if snap == nil {
		return false
	}
	for _, w := range snap.Actors {
		if w != nil && w.WorkStructureID == stID {
			return true
		}
	}
	return false
}

// snapshotKeeperPresent reports whether a worker of stID is currently tending it
// in the snapshot: at it (inside, or within its loiter pin — actorAtStructure)
// AND awake. The snapshot mirror of sim.keeperPresentAt, with the same gates:
// StateSleeping disqualifies (an innkeeper sleeps AT the inn, so an abed keeper
// reads shut — LLM-126), but a keeper briefly on break (StateResting) still
// counts (the business is open, just quiet). A worker who has wandered off is
// not present, so the business reads shut.
func snapshotKeeperPresent(snap *sim.Snapshot, stID sim.StructureID) bool {
	if snap == nil {
		return false
	}
	for _, w := range snap.Actors {
		if w == nil || w.WorkStructureID != stID || w.State == sim.StateSleeping {
			continue
		}
		if actorAtStructure(snap, w, stID) {
			return true
		}
	}
	return false
}

// snapshotBusinessTended reports whether ANYONE is minding stID — its own keeper
// (snapshotKeeperPresent), or a hired hand working a live job for that keeper and
// still at the place. The perception-snapshot mirror of sim.businessTendedAt
// (LLM-527); the two must agree or the rendered "is shut" line and the engine's
// own shut-business memory contradict each other on the same tick.
//
// The bug it fixes: a free laborer has no workplace of his own, so the
// keeper-only scan looked straight past a man at work in the farmyard and told a
// constable standing in front of him that the farm was shut and deserted — while
// the client rendered the same stall open, with him visibly in it.
//
// Only LaborStateWorking counts (an EnRoute hand is still on his way), and he
// must be awake and physically at the place — the same gates the keeper arm
// applies to a keeper who has drifted off.
func snapshotBusinessTended(snap *sim.Snapshot, stID sim.StructureID) bool {
	if snap == nil {
		return false
	}
	if snapshotKeeperPresent(snap, stID) {
		return true
	}
	for _, o := range snap.LaborLedger {
		if o == nil || o.State != sim.LaborStateWorking {
			continue
		}
		employer := snap.Actors[o.EmployerID]
		if employer == nil || employer.WorkStructureID != stID {
			continue
		}
		worker := snap.Actors[o.WorkerID]
		if worker == nil || worker.State == sim.StateSleeping {
			continue
		}
		if actorAtStructure(snap, worker, stID) {
			return true
		}
	}
	return false
}

// snapshotStructureKeeperName returns the display name of the structure's keeper
// — an actor whose WorkStructureID is stID — excluding the arriver so an actor
// reaching its own workplace resolves no keeper, and "" when the structure has
// no other keeper (a residence, well, or the arriver's own shop). Ownership, not
// presence: whose shop it is holds whether or not the keeper is standing there,
// so this does not gate on attendance (LLM-284). A Salem business has a single
// keeper; when more than one actor shares a workplace the smallest actor id wins
// so the rendered possessive is stable across ticks (snap.Actors is a Go map
// with no iteration order).
func snapshotStructureKeeperName(snap *sim.Snapshot, stID sim.StructureID, arriver sim.ActorID) string {
	if snap == nil || stID == "" {
		return ""
	}
	var keeperID sim.ActorID
	var keeperName string
	for id, w := range snap.Actors {
		if w == nil || w.WorkStructureID != stID || id == arriver || w.DisplayName == "" {
			continue
		}
		if keeperID == "" || id < keeperID {
			keeperID = id
			keeperName = w.DisplayName
		}
	}
	return keeperName
}

// shiftWindowBounds resolves the actor's effective shift window: its own
// schedule when both bounds are set, else the world day-active (dawn/dusk)
// window from the snapshot. ok=false when neither is usable. Shared by
// buildDutySteer and buildDutyPending so the cue and the gate signal agree
// on what "the shift window" is.
//
// The day-active fallback: DawnDuskMinuteOK rejects a partial/failed parse
// (which would otherwise derive a bogus window from one good + one zero
// bound); the inequality rejects a degenerate dawn==dusk empty window that
// reads as off-shift all day and would emit a perpetual "head home" cue
// (code_review).
func shiftWindowBounds(snap *sim.Snapshot, a *sim.ActorSnapshot) (start, end int, ok bool) {
	// Nil-safe on its own: both current callers pre-check, but the helper's
	// contract shouldn't depend on that — a future caller skipping the guard
	// would panic on the field derefs below (code_review, HOME-442).
	if snap == nil || a == nil {
		return 0, 0, false
	}
	switch {
	case a.ScheduleStartMin != nil && a.ScheduleEndMin != nil:
		return *a.ScheduleStartMin, *a.ScheduleEndMin, true
	case snap.DawnDuskMinuteOK && snap.DawnMinute != snap.DuskMinute:
		return snap.DawnMinute, snap.DuskMinute, true
	default:
		return 0, 0, false
	}
}

// keeperOperating reports whether a keeper standing at its own post is within
// operating hours — inside its shift window, or off-shift but holding an unlapsed
// stay_open commitment (it has chosen to keep the business open past close). The
// trade-conduct cue gates on this (via AtOwnBusinessOperating) so the "tend to
// your trade" steer is silent at a closed post after hours (LLM-123).
//
// When the clock or shift window can't be resolved from the snapshot it returns
// false: an operating-hours claim we can't substantiate must not drive
// work-pressure, the same way buildDutySteer goes silent on an unresolvable
// clock. In the live engine LocalMinuteOfDay is always published and a keeper
// has a schedule (or the dawn/dusk fallback), so this only suppresses for a
// malformed fixture. The stay_open check compares OpenUntil against the render
// instant (no minute-of-day needed) but still requires a real instant — a zero
// PublishedAt is an unsubstantiated clock, so it can't open the gate either
// (code_review).
func keeperOperating(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil {
		return false
	}
	if !snap.PublishedAt.IsZero() && a.OpenUntil != nil && a.OpenUntil.After(snap.PublishedAt) {
		return true
	}
	if snap.LocalMinuteOfDay == nil {
		return false
	}
	start, end, ok := shiftWindowBounds(snap, a)
	if !ok {
		return false
	}
	return minuteInWindow(start, end, *snap.LocalMinuteOfDay)
}

// buildDutyPending reports whether the actor is off-post inside its shift
// window — to-work duty APPLIES this minute — computed WITHOUT the cue-side
// suppressors that can nil buildDutySteer's to-work arm (the HOME-362
// red-need gate; HOME-400 Option B's mild-need / restock-errand /
// pending-offer gate). The noop-skip gate consumes it (ZBBS-HOME-442): an
// off-post on-shift keeper with a need in the mild band had NO rendered
// steer (Option B) and NO red need, so the gate ate its idle-backstops and
// it stood skip-locked for hours (the live Josiah case the HOME-441 steer
// condition turned out not to cover). The signal opens the gate; the cue
// stays suppressed — the tick that runs voices the mild need with no
// to-work line, the model addresses the need, and once every need drops
// below mild the steer renders and the next tick walks the actor to post.
//
// Strictly the TO-WORK arm: the go-home/wind-down side keeps its existing
// behavior (a rendered go-home steer already opens the gate via DutySteer;
// its suppressors — mid-meal dwell, stay-open — describe an actor that is
// mid-action, not stuck).
func buildDutyPending(snap *sim.Snapshot, a *sim.ActorSnapshot, anchors *AnchorsView) bool {
	if snap == nil || a == nil || anchors == nil || snap.LocalMinuteOfDay == nil {
		return false
	}
	if a.Kind != sim.KindNPCStateful && a.Kind != sim.KindNPCShared {
		return false
	}
	start, end, ok := shiftWindowBounds(snap, a)
	if !ok {
		return false
	}
	if !minuteInWindow(start, end, *snap.LocalMinuteOfDay) {
		return false
	}
	return anchors.WorkID != "" && a.InsideStructureID != anchors.WorkID
}

// windDownSuppressed reports whether the off-shift wind-down cue should be held
// back this tick despite the actor being off-shift and not yet wound down.
// Mirrors shiftDutyTarget's go-home suppressors so the perception cue and the
// engine warrant agree (ZBBS-WORK-386 + ZBBS-WORK-387):
//   - a live item-source dwell credit (mid-meal) — don't prompt it to abandon
//     the meal mid-dwell; the cue re-fires once the dwell ends.
//   - an unlapsed stay_open "open until" commitment — the keeper has chosen to
//     stay open, so suppress the routine wind-down.
//
// The peak-exhaustion override shiftDutyTarget applies (OpenUntil yields to peak,
// so the engine still force-beds an exhausted keeper) is deliberately NOT
// mirrored here: buildDutySteer already returns nil for ANY red-or-worse need
// (hasRedNeed, HOME-362) before this is reached, and peak is a strict subset of
// red — so a peak-exhausted keeper's wind-down cue is already silenced upstream
// and the engine's force-bed floor (MarchHome) / recovery_options drive the rest.
// When this runs, the actor is always sub-red.
func windDownSuppressed(a *sim.ActorSnapshot, snap *sim.Snapshot) bool {
	if sim.HasActiveItemDwell(a.DwellCredits) {
		return true
	}
	if a.OpenUntil != nil && a.OpenUntil.After(snap.PublishedAt) {
		return true
	}
	return false
}

// lodgerInn resolves the inn structure (id + display label) where the actor
// holds its soonest-expiring active ledger room grant, or ok=false when it holds
// none (i.e. isn't a lodger). The grant selection matches buildLodgingView and
// the engine's soonestActiveLedgerGrant, so the wind-down cue, the lodging
// section, and the engine warrant all point at the same inn. ZBBS-WORK-387.
func lodgerInn(snap *sim.Snapshot, a *sim.ActorSnapshot) (sim.StructureID, string, bool) {
	now := snap.PublishedAt
	var best *sim.RoomAccess
	for _, ra := range a.RoomAccess {
		if !sim.IsActiveLedgerGrant(ra, now) {
			continue
		}
		// Tie-break equal expiries by RoomID — deterministic across the map's
		// randomized iteration order, and matching the engine's
		// soonestActiveLedgerGrant so the cue and the warrant pick the same inn
		// when an actor holds two equally-expiring grants (ZBBS-WORK-387).
		if best == nil || ra.ExpiresAt.Before(*best.ExpiresAt) ||
			(ra.ExpiresAt.Equal(*best.ExpiresAt) && ra.RoomID < best.RoomID) {
			best = ra
		}
	}
	if best == nil {
		return "", "", false
	}
	s := structureForRoom(snap, best.RoomID)
	if s == nil {
		return "", "", false
	}
	return s.ID, innLabel(s), true
}

// stayOpenReason returns the concrete reason to ENCOURAGE a wind-down keeper to
// stay open (the hybrid gate, ZBBS-WORK-387 design C), or "" when none is present
// (the keeper is still OFFERED stay_open, just not actively encouraged). Ordered
// most-concrete-commitment first.
func stayOpenReason(hasOwedOrders, hasCoPresentBuyer, hasPendingOffer bool) string {
	switch {
	case hasOwedOrders:
		return "you still have orders to deliver"
	case hasCoPresentBuyer:
		return "a customer is still here with you"
	case hasPendingOffer:
		return "you have an offer still awaiting payment"
	}
	return ""
}

// hasRedNeed reports whether any of the actor's tracked needs is at or over its
// configured red-tier threshold. Iterates the canonical need registry (sim.Needs)
// and reads the same per-need boundary the recovery/satiation cues and the
// need-threshold warrant use (snap.NeedThresholds.Get, which falls back to the
// registry default when unset) so "red" means one thing across the prompt.
// Nil-safe (perception builders elsewhere have hit nil-snapshot edges).
// ZBBS-HOME-362.
func hasRedNeed(a *sim.ActorSnapshot, snap *sim.Snapshot) bool {
	if a == nil || snap == nil {
		return false
	}
	for _, n := range sim.Needs {
		if a.Needs[n.Key] >= snap.NeedThresholds.Get(n.Key) {
			return true
		}
	}
	return false
}

// tendNeedWarrantActive reports whether a TendNeedWarrantReason (LLM-276) is in the
// tick's warrants — the seek-work backstop's signal that this workless idle worker
// has grown hungry/thirsty and can resolve it now, so job-hunting yields to eating.
// Perception keys the seek-work directory/affordance suppression and the need-redirect
// steer off this rather than recomputing the sim-side band, so the warrant and the
// cues can't disagree (the LLM-168 warrant/directory-must-agree invariant).
func tendNeedWarrantActive(warrants []sim.WarrantMeta) bool {
	for _, wm := range warrants {
		if _, ok := wm.Reason.(sim.TendNeedWarrantReason); ok {
			return true
		}
	}
	return false
}

// hasPendingOutgoingOffer reports whether actorID has a pay-with-item offer it
// made (as buyer) still awaiting the seller's response. While one is pending, the
// return-to-post cue is suppressed so the buyer isn't pulled out of the
// conversation before the seller can accept_pay — acceptance re-checks that both
// parties are still co-present, so walking away fails the trade (ZBBS-HOME-400
// Option B). Scans the published pay ledger, which is bounded by the TTL sweep
// (RunPayLedgerSweep); if terminal entries are ever found to accumulate, index
// pending outgoing offers at snapshot build time instead (code_review). Nil-safe.
func hasPendingOutgoingOffer(snap *sim.Snapshot, actorID sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, e := range snap.PayLedger {
		if e != nil && e.BuyerID == actorID && e.State == sim.PayLedgerStatePending {
			return true
		}
	}
	return false
}

// hasOfferedQuote reports whether a seller has an active scene_quote addressed to
// actorID (as buyer) still standing. The buyer-side complement to
// hasPendingOutgoingOffer: a quote a seller has put in front of the buyer is an
// in-progress purchase, so the return-to-post cue is suppressed rather than
// yanking the buyer out of the deal before they can take it — pay_with_item
// re-checks co-presence, so walking off to the post loses the trade (the
// Prudence shop↔General-Store bounce, 2026-06-17, where the to-work yank fired
// every tick she stood at the stall mid-purchase because her mild hunger wasn't
// red and a settling consume_now buy never sits pending). Targeted quotes only
// (TargetBuyer == actorID): a public quote (TargetBuyer == "") isn't addressed to
// this buyer in particular, so it shouldn't pin a passer-by to the stall. Scans
// the published quote map, bounded by the TTL sweep (RunSceneQuoteSweep). Nil-safe.
func hasOfferedQuote(snap *sim.Snapshot, actorID sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, q := range snap.Quotes {
		if q != nil && q.TargetBuyer == actorID && q.State == sim.SceneQuoteStateActive {
			return true
		}
	}
	return false
}

// buildOfferableCustomers builds the seller-side "offer your wares" cue
// (ZBBS-HOME-404). When a businessowner is co-present with one or more
// customers, it surfaces those customers by name alongside the seller's
// sellable goods, so the keeper LLM can proactively offer a sale via
// scene_quote rather than only reacting to a buyer's pay_with_item. This makes
// the existing seller-initiated path LEGIBLE (the Finding-1 lesson applied to
// the sell side); it does not auto-complete anything — the seller decides
// whether/what/at-what-price and the buyer keeps full accept/decline agency, so
// any future "this seller won't deal" reason needs no new mechanism.
//
// Co-presence is the huddle (an active interaction), not mere loiter-presence —
// so the cue doesn't fire at someone merely passing the stall, and the vendor
// block's "don't pitch unless they show interest" rule still governs whether
// the model actually offers.
//
// Returns nil — Render content-gates — when the subject isn't a businessowner,
// has no co-present customer, or carries nothing to sell.
//
// Two storm guards drop a customer from the cue (the pay-offer / order-chase
// dedup discipline, so a stuck cue can't drive a re-offer loop):
//   - the customer already has a pending pay_with_item offer with this seller —
//     renderPayOffers already cues accept/decline/counter, so don't also drive
//     the seller to offer them; and
//   - the seller already has a live (Active) scene_quote out to that customer —
//     they've offered and await the buyer; re-cueing would re-post every tick.
//
// commissionableGoods lists the goods the seller can forge TO ORDER but holds none
// of, so the sale cue offers them alongside what is on the shelf (LLM-619).
//
// The gap it closes: "Your goods to sell" was built from physical inventory alone,
// so a good at zero on hand vanished from the listing even though
// acceptPendingOffer would have honoured the sale as a commission order. Ezekiel
// Crane held 0 shovels while forging one, Moses James named "shovel" six times,
// and neither ever called a commerce tool — the shovel simply was not on the list
// of things the cue said he could sell. They struck the bargain aloud instead, six
// times, which commits nothing, so no order row existed for LLM-518's farm-upkeep
// suppression to find and the buyer walked forge↔home for over an hour.
//
// Two gates beyond sim.CommissionableKind, both deliberate:
//   - already held is EXCLUDED — a good on the shelf is an ordinary take-home sale
//     and is already listed with its count; listing it twice would be noise.
//   - sim.HasProduceInputs is REQUIRED — the same inputs test the produce tool
//     consumes on (and the LLM-324 forge-choice filter applies), so the cue never
//     invites a seller to promise a good they cannot even start. A commission is a
//     promise to forge, and a promise with no makings is the vapour trade this cue
//     is otherwise meant to replace.
//
// Ordering is by the policy's produce entries, which are stable, and Render sorts
// the merged list anyway.
func commissionableGoods(snap *sim.Snapshot, subject sim.ActorID, held map[sim.ItemKind]bool) []OfferableGood {
	// nil snap is reachable: the empty-shelf guard above used to return before any
	// snapshot read, and a caller passing nil relied on that ordering.
	if snap == nil {
		return nil
	}
	actorSnap := snap.Actors[subject]
	if actorSnap == nil || actorSnap.RestockPolicy == nil {
		return nil
	}
	var out []OfferableGood
	for _, e := range actorSnap.RestockPolicy.ProduceEntries() {
		if held[e.Item] {
			continue
		}
		if !sim.CommissionableKind(snap.Recipes, snap.ItemKinds, actorSnap.RestockPolicy, e.Item) {
			continue
		}
		if !sim.HasProduceInputs(snap.Recipes[e.Item], actorSnap.Inventory) {
			continue
		}
		label := itemDisplayLabel(snap, e.Item)
		if label == "" {
			continue
		}
		out = append(out, OfferableGood{Label: label, OnHand: 0, Commission: true, kind: e.Item})
	}
	return out
}

func buildOfferableCustomers(snap *sim.Snapshot, subject sim.ActorID, atOwnBusiness bool, members []HuddleMember, inventory []InventoryItem) *OfferableCustomersView {
	// An EMPTY shelf is no longer a reason to bail: a maker with nothing on hand can
	// still take a commission (LLM-619). The len(goods) == 0 check below is the real
	// "nothing to offer" gate, and it now accounts for both sources.
	if !atOwnBusiness || len(members) == 0 {
		return nil
	}
	goods := make([]OfferableGood, 0, len(inventory))
	held := make(map[sim.ItemKind]bool, len(inventory))
	for _, it := range inventory {
		if it.Label == "" {
			continue
		}
		held[it.kind] = true
		// it.Use is already resolved by buildInventoryView (LLM-166), so the
		// for-sale listing reads consistently with the carry readout.
		goods = append(goods, OfferableGood{Label: it.Label, OnHand: it.Qty, Use: it.Use, kind: it.kind})
	}
	goods = append(goods, commissionableGoods(snap, subject, held)...)
	if len(goods) == 0 {
		return nil
	}
	// members is already sorted by ID (buildSurroundings), so names is deterministic.
	var names []string
	var producerNotes []ProducerNote
	for _, m := range members {
		// LLM-231: a peer mid-job (a Working LaborOffer) is not a valid sale
		// target — don't cue the seller to pitch someone busy working. m.Laboring
		// is set for every observer, so this drops the peer even when the seller is
		// the worker's OWN employer (who shouldn't pitch a sale to their mid-job
		// worker either). For a bystander, "## Around you" also annotates them busy.
		if m.Laboring {
			continue
		}
		if customerHasPendingOfferWithSeller(snap, m.ID, subject) {
			continue
		}
		if sellerHasActiveQuoteToBuyer(snap, subject, m.ID) {
			continue
		}
		label := descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
		names = append(names, label)
		// LLM-171: flag the goods this customer MAKES themselves, so Render steers
		// the seller off pitching a maker their own ware back — the seller's stock
		// of those came from a maker like them.
		if made := customerProducedGoods(snap, m.ID, goods); len(made) > 0 {
			producerNotes = append(producerNotes, ProducerNote{CustomerName: label, Goods: made})
		}
	}
	if len(names) == 0 {
		return nil
	}
	return &OfferableCustomersView{CustomerNames: names, Goods: goods, ProducerNotes: producerNotes}
}

// customerHasPendingOfferWithSeller reports whether `buyer` has a pending
// pay_with_item offer awaiting `seller`'s response. The reactive case is
// already cued by renderPayOffers (accept/decline/counter), so the proactive
// offer cue suppresses that customer to avoid double-driving the seller toward
// the same person. Nil-safe.
func customerHasPendingOfferWithSeller(snap *sim.Snapshot, buyer, seller sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, e := range snap.PayLedger {
		if e != nil && e.BuyerID == buyer && e.SellerID == seller && e.State == sim.PayLedgerStatePending {
			return true
		}
	}
	return false
}

// sellerHasActiveQuoteToBuyer reports whether `seller` already has a live
// (Active) scene_quote targeted at `buyer`. While one stands, the buyer can take
// it via pay_with_item — re-cueing the offer would have the seller re-post the
// same quote every tick (the re-offer storm hard-capped in ZBBS-HOME-395/381). A
// public (untargeted) quote is deliberately NOT counted: it isn't directed at
// this customer, so it doesn't pre-empt a directed offer. Nil-safe.
func sellerHasActiveQuoteToBuyer(snap *sim.Snapshot, seller, buyer sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, q := range snap.Quotes {
		if q != nil && q.SellerID == seller && q.TargetBuyer == buyer && q.State == sim.SceneQuoteStateActive {
			return true
		}
	}
	return false
}

// buildNarrativeState returns the kind-aware "## Who you are" content
// for shared-VA actors, or nil otherwise. Stateful and PC actors get
// no engine-side narrative — their identity comes from their own VA's
// <Self> block (stateful) or from the player (PC). Transient travelers
// are excluded too: their identity is the renderTravelerPreface
// (LLM-370), so this section would double-state it.
//
// Name is always set from the snapshot (LLM-432) — the shared VA's
// system prompt carries no per-actor identity, so the self-name line is
// content on its own even before the soul (AboutMe, LLM-199) has been
// synthesized. Nil only when both name and soul are empty, so Render
// never emits a bare header. SeedText/EvolvingSummary are not rendered
// (SeedText is never set for shared VAs; EvolvingSummary is legacy), so
// they don't open the section.
func buildNarrativeState(a *sim.ActorSnapshot) *NarrativeStateView {
	if a.Kind != sim.KindNPCShared || a.VisitorState != nil {
		return nil
	}
	v := &NarrativeStateView{Name: a.DisplayName}
	if a.Narrative != nil {
		v.AboutMe = a.Narrative.AboutMe
	}
	if v.Name == "" && v.AboutMe == "" {
		return nil
	}
	return v
}

// recentSalientFactsPerPeer is the per-peer ceiling on facts surfaced
// into perception. Mirrors v1's formatRelationshipsPerception which
// renders the most-recent 3. RecentFacts is the slice end of the
// stored oldest-first SalientFacts, reversed to most-recent-first.
const recentSalientFactsPerPeer = 3

// maxRenderedRelationshipPeers caps how many co-present peers' remembered
// impressions render in "## What you remember of those here" (LLM-322). That
// section re-sends each peer's consolidated summary every tick, so its cost
// scales with co-presence and spikes in a crowded morning-rush huddle. When a
// huddle holds more known peers than this, keep the impressions of the peers
// the subject has most recently dealt with and let the rest fall back to the
// bare "## Around you" name line (graceful degradation — they're still named,
// just without the remembered impression). Chosen so a normal conversation (a
// handful of people) is untouched and only a genuine crowd is trimmed.
const maxRenderedRelationshipPeers = 4

// maxRenderedVillageWord caps the "## Word about the village" bullets so a
// well-traveled gossip's known-set can't balloon the re-sent-every-tick section.
// Small on purpose — it is background colour, not a ledger; the sim-side known-set
// cap (sim.MaxKnownRumors) is looser, so the render trims to the freshest few.
const maxRenderedVillageWord = 3

// buildVillageWord projects the subject's own carried rumors
// (ActorSnapshot.Rumors) into the "## Word about the village" view (LLM-387): the
// freshest live rumors about residents who are NOT standing in the scene, capped
// at maxRenderedVillageWord. Expired rumors (past sim.RumorTTL as of now) are
// skipped, and any rumor whose subject is a present peer — huddled or merely
// co-present — is filtered out, the render mirror of sim.salientRumorToShare's
// "don't gossip to their face" rule. Returns nil for PC / decorative subjects
// (which never carry or voice gossip) and when the subject holds nothing
// shareable here. Pure over the immutable snapshot: it reads the snapshot's own
// Rumors copy and never mutates it (no in-place prune), so ordering the working
// set is done on a local copy.
func buildVillageWord(a *sim.ActorSnapshot, s SurroundingsView, now time.Time) []VillageRumorView {
	if a == nil || len(a.Rumors) == 0 {
		return nil
	}
	if a.Kind == sim.KindPC || a.Kind == sim.KindDecorative {
		return nil
	}
	present := make(map[sim.ActorID]bool, len(s.HuddleMembers)+len(s.CoPresent))
	for _, m := range s.HuddleMembers {
		present[m.ID] = true
	}
	for _, m := range s.CoPresent {
		present[m.ID] = true
	}
	shareable := make([]sim.KnownRumor, 0, len(a.Rumors))
	for _, r := range a.Rumors {
		if r.Expired(now) {
			continue
		}
		if present[r.SubjectID] {
			continue // don't voice gossip about someone standing right here
		}
		if r.Clause() == "" {
			continue // unknown topic or missing subject name — nothing to say
		}
		shareable = append(shareable, r)
	}
	if len(shareable) == 0 {
		return nil
	}
	// Freshest first so the cap keeps the most recently heard gossip.
	sort.SliceStable(shareable, func(i, j int) bool {
		return shareable[i].HeardAt.After(shareable[j].HeardAt)
	})
	if len(shareable) > maxRenderedVillageWord {
		shareable = shareable[:maxRenderedVillageWord]
	}
	out := make([]VillageRumorView, 0, len(shareable))
	for _, r := range shareable {
		out = append(out, VillageRumorView{Clause: r.Clause(), FirstHand: r.FirstHand})
	}
	return out
}

// buildRelationships projects per-co-huddle-peer relationship views
// from the subject actor's Relationships map. Populated only for
// shared-VA actors. Peers in the huddle without a Relationship row
// (e.g. just-met strangers — the Relationship is only written by
// speech/pay/serve/deliver handlers, not first-encounter) are omitted
// silently rather than rendered as empty views.
//
// Ordering: by PeerID, matching SurroundingsView.HuddleMembers'
// sort order, so a reader of both blocks sees the same peer order.
//
// Same-tick de-dup (ZBBS-WORK-374): a just-heard utterance is recorded as a
// `heard` SalientFact on the listener AND surfaced as a speech warrant in this
// tick's "## Since your last turn". Rendering it in both places shows the model
// the same line twice (the live "Hello" duplication) and reinforces it. heardNow
// maps speaker → the utterances they spoke in THIS batch; we drop a peer's fact
// whose text matches before taking the recent-N, so the most-recent slot
// backfills with genuinely-older context instead of a duplicate. Done here (not
// in Render) per the package contract: Build decides content, Render is content-
// agnostic.
func buildRelationships(a *sim.ActorSnapshot, members []HuddleMember, heardNow map[sim.ActorID]map[string][]sim.WarrantSourceKey) []RelationshipPeerView {
	if a.Kind != sim.KindNPCShared || len(a.Relationships) == 0 || len(members) == 0 {
		return nil
	}
	out := make([]RelationshipPeerView, 0, len(members))
	for _, m := range members {
		rel := a.Relationships[m.ID]
		if rel == nil {
			continue
		}
		facts := rel.SalientFacts
		if dups := heardNow[m.ID]; len(dups) > 0 {
			kept := make([]sim.SalientFact, 0, len(facts))
			for _, f := range facts {
				if _, heard := dups[f.Text]; f.Kind == sim.InteractionHeard && heard {
					continue // already in "## Since your last turn" this tick
				}
				kept = append(kept, f)
			}
			facts = kept
		}
		out = append(out, RelationshipPeerView{
			PeerID:      m.ID,
			PeerName:    m.DisplayName,
			SummaryText: rel.SummaryText,
			RecentFacts: recentFactsMostRecentFirst(facts, recentSalientFactsPerPeer),
		})
	}
	// LLM-322: in a crowded huddle, cap to the peers the subject has most
	// recently dealt with so the re-sent-every-tick impression blobs can't
	// balloon the prompt. A no-op when the huddle holds few enough peers.
	out = capRelationshipsToMostRecent(out, a.Relationships, maxRenderedRelationshipPeers)
	if len(out) == 0 {
		return nil
	}
	return out
}

// capRelationshipsToMostRecent trims the relationship views to at most `limit`,
// keeping the peers the subject most recently interacted with, so a crowded
// huddle can't balloon the re-sent-every-tick "## What you remember of those
// here" section (LLM-322). A peer that carries a consolidated summary — the only
// kind that renders a line, since renderRelationships skips an empty summary —
// is preferred over a summary-less row, so a freshly-met peer with no summary
// yet never displaces one the subject actually remembers. Among peers on equal
// footing, most-recent LastInteractionAt wins, then higher InteractionCount,
// then PeerID for a stable order. The kept set is returned in PeerID order so
// the block still matches "## Around you"'s peer ordering.
func capRelationshipsToMostRecent(views []RelationshipPeerView, rels map[sim.ActorID]*sim.Relationship, limit int) []RelationshipPeerView {
	if len(views) <= limit {
		return views
	}
	sort.SliceStable(views, func(i, j int) bool {
		si := strings.TrimSpace(views[i].SummaryText) != ""
		sj := strings.TrimSpace(views[j].SummaryText) != ""
		if si != sj {
			return si // a peer that will render a line sorts ahead of one that won't
		}
		ti, ci := relationshipRecency(rels[views[i].PeerID])
		tj, cj := relationshipRecency(rels[views[j].PeerID])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if ci != cj {
			return ci > cj
		}
		return views[i].PeerID < views[j].PeerID
	})
	kept := views[:limit]
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].PeerID < kept[j].PeerID })
	return kept
}

// relationshipRecency returns the peer's last-interaction time and interaction
// count for the cap sort, tolerating a nil relationship / nil timestamp (a zero
// time sorts last).
func relationshipRecency(rel *sim.Relationship) (time.Time, int) {
	if rel == nil {
		return time.Time{}, 0
	}
	var last time.Time
	if rel.LastInteractionAt != nil {
		last = *rel.LastInteractionAt
	}
	return last, rel.InteractionCount
}

// recentConversationDedupKey normalizes an utterance to the 220-rune SalientFact
// form (what NewSalientFact stores at write time), so a ring line can be matched
// against currentHeardExcerpts — which indexes both this form and the full text
// (LLM-396). Short lines (the common case) are returned unchanged.
func recentConversationDedupKey(text string) string {
	r := []rune(text)
	if len(r) > sim.MaxSalientFactTextLen {
		return string(r[:sim.MaxSalientFactTextLen])
	}
	return text
}

// maxRenderedConversationLines caps how many lines of the current huddle's
// RecentUtterances ring render into "## Recent conversation here" (LLM-322). The
// ring holds up to MaxRecentUtterancesPerHuddle (8); the last few turns are the
// live thread the model needs to avoid re-pitching, so the older tail is dropped
// to save per-tick input tokens. Kept near maxSelfActionTrail — the same "last
// handful is enough" posture.
const maxRenderedConversationLines = 5

// buildRecentConversation projects the subject's current-huddle RecentUtterances
// ring into the "## Recent conversation here" view (ZBBS-HOME-412), oldest-first.
// Populated for EVERY actor with a live huddle — NOT gated to shared VAs like
// buildRelationships — so stateful NPCs and PC-facing vendors get cross-tick
// conversational continuity (they see their own prior lines and the player's).
// The subject's own lines are marked IsSelf. A line whose text matches an
// utterance already surfaced in this tick's "## Since your last turn" (heardNow) is
// dropped so the live turn isn't shown twice — the same de-dup buildRelationships
// applies to heard facts (ZBBS-WORK-374). Returns nil when the subject has no
// huddle or nothing survives the de-dup.
//
// The second return is the LLM-542 conveyance record. The rule it implements:
// a ring line is CONVEYED iff its text reached the prompt — which is a wider
// question than whether it rendered here.
//
//	inside the window, not de-duped — rendered here. Conveyed.
//	inside the window, de-duped      — not rendered, but the de-dup fires
//	                                   precisely BECAUSE the text is already in
//	                                   "## Since your last turn". Conveyed.
//	outside the window, de-duped     — same: the text is in the prompt under the
//	                                   other heading, just not through this
//	                                   section. Conveyed (code_review).
//	outside the window, not de-duped — nothing carried it. NOT conveyed; a
//	                                   warrant it stamped is still owed.
//
// The de-dup matches on speaker + TEXT, not on id, so a line whose warrant is
// still pending can collide with a consumed warrant's excerpt and vanish from
// this section. Without the conveyance record that pending warrant would fire a
// second reply to a line the model has already seen.
func buildRecentConversation(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, heardNow map[sim.ActorID]map[string][]sim.WarrantSourceKey) ([]UtteranceView, []ConveyedSpeechRef) {
	huddleID := actorSnap.CurrentHuddleID
	if huddleID == "" {
		return nil, nil
	}
	h := snap.Huddles[huddleID]
	if h == nil || len(h.RecentUtterances) == 0 {
		return nil, nil
	}
	// LLM-322: consider only the most recent lines of the ring, THEN drop any
	// already shown this tick in "## Since your last turn". The ring is
	// oldest-first, so slice the tail before de-duping — capping AFTER the de-dup
	// would let an older ring line leak in when the newest lines de-dup out (they
	// shrink `out` below the cap, so the tail slice never triggers).
	// The whole ring is walked, but only the tail window renders — conveyance
	// reaches further back than the section does (see the doc comment).
	utts := h.RecentUtterances
	windowStart := 0
	if len(utts) > maxRenderedConversationLines {
		windowStart = len(utts) - maxRenderedConversationLines
	}
	out := make([]UtteranceView, 0, len(utts)-windowStart)
	var conveyed []ConveyedSpeechRef
	for i, u := range utts {
		inWindow := i >= windowStart
		viaWarrants, alreadyHeard := heardNow[u.SpeakerID][recentConversationDedupKey(u.Text)]

		// LLM-542 conveyance. The subject's own lines are excluded: an actor
		// never warrants itself for its own speech, so there is nothing to
		// discharge. A zero SpeechID (recorded outside the emit path) has no
		// event to key on.
		if (inWindow || alreadyHeard) && u.SpeakerID != actorID && u.SpeechID != 0 {
			// Speaker kind decides which speech warrant kind this line stamped
			// on its listeners. An unknown speaker (gone from the snapshot)
			// falls back to NPC — the same fail-closed default the speech
			// reactor takes for an unattributable utterance.
			speakerIsPC := false
			if sp := snap.Actors[u.SpeakerID]; sp != nil && sp.Kind == sim.KindPC {
				speakerIsPC = true
			}
			ref := ConveyedSpeechRef{SpeechID: u.SpeechID, SpeakerIsPC: speakerIsPC}
			// A de-duped line is carried ONLY by the warrant render — this
			// section drops it precisely because the warrant is showing the
			// text. That is conditional on the warrant surviving the prompt
			// caps, and Build runs BEFORE Render, so the dependency is
			// recorded here and settled after (code_review). A line that
			// renders in this section in its own right depends on nothing.
			if alreadyHeard {
				ref.ViaWarrants = viaWarrants
			}
			conveyed = append(conveyed, ref)
		}

		if !inWindow || alreadyHeard {
			continue // outside the section, or already shown under "## Since your last turn"
		}
		out = append(out, UtteranceView{
			SpeakerName: u.SpeakerName,
			Text:        u.Text,
			IsSelf:      u.SpeakerID == actorID,
			At:          u.At,
		})
	}
	if len(out) == 0 {
		return nil, conveyed
	}
	return out, conveyed
}

// selfActionTrailWindow bounds how far back the "## What you've recently done"
// trail reaches. Wide enough that even a SLOW oscillation (need-refire-paced
// cycles of 5-10 minutes, like the off-shift forge↔Tavern loop) shows several
// repeats, narrow enough that this morning's errands don't clutter an evening
// turn — the trail is loop-noticing context, not a diary. maxSelfActionTrail
// is the real prompt budget: an active NPC fills the cap long before the
// window binds, so the window only trims stale context for sparse actors.
// LLM-217.
const selfActionTrailWindow = 30 * time.Minute

// maxSelfActionTrail caps the trail's line count. Small on purpose, matching
// the MaxRecentUtterancesPerHuddle posture: the last handful of deeds is what
// self-loop detection needs; anything durable is the consolidation cascade's
// job. Trimmed 6→5 in LLM-322 to shave per-tick input tokens — still wide
// enough to show a repeating oscillation. LLM-217.
const maxSelfActionTrail = 5

// selfActionTrailTypes is the set of committed actions the trail renders — the
// deed types renderSelfActions has second-person phrasing for. Notably absent:
// summoned (delivered TO the actor, not done by it) and stayed_open (a
// commitment, not a deed; the operating state already renders standing).
var selfActionTrailTypes = map[sim.ActionType]bool{
	sim.ActionTypeSpoke:         true,
	sim.ActionTypePaid:          true,
	sim.ActionTypeConsumed:      true,
	sim.ActionTypeDelivered:     true,
	sim.ActionTypeWalked:        true,
	sim.ActionTypeDeparted:      true,
	sim.ActionTypeTookBreak:     true,
	sim.ActionTypeLabored:       true,
	sim.ActionTypeSolicitedWork: true,
	sim.ActionTypeOfferedWork:   true,
	sim.ActionTypeHired:         true,
}

// buildSelfActions projects the subject's own recent committed actions out of
// snap.ActionLog into the "## What you've recently done" view (LLM-217),
// most-recent-first. This is the self-action memory the prompt otherwise
// lacks: warrant beats live one tick and the conversation ring carries speech
// only, so a vacillating NPC (the Patience Walker go-home ↔ seek-work loop)
// could not see its own churn. Scans from the log tail — the log is Seq-
// ordered, so everything relevant sits within the window at the end. A spoke
// entry from the subject's CURRENT huddle is skipped: the conversation ring
// already renders those lines, and the trail's job is the deeds (and prior-
// huddle speech, e.g. "I'll head home now" said before walking out) the ring
// can't show. Returns nil when the snapshot has no clock (hand-built payloads
// — the window needs PublishedAt) or nothing qualifies.
func buildSelfActions(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot) []SelfActionView {
	if snap.PublishedAt.IsZero() || len(snap.ActionLog) == 0 {
		return nil
	}
	cutoff := snap.PublishedAt.Add(-selfActionTrailWindow)
	var out []SelfActionView
	for i := len(snap.ActionLog) - 1; i >= 0 && len(out) < maxSelfActionTrail; i-- {
		e := snap.ActionLog[i]
		if e.OccurredAt.Before(cutoff) {
			// No early break: the log is Seq-ordered and OccurredAt is only
			// APPROXIMATELY monotonic (see ActionLogEntry.Seq), so an in-window
			// entry can sit behind an out-of-window one. The log is retention-
			// capped, so the full tail scan stays cheap.
			continue
		}
		if e.ActorID != actorID || !selfActionTrailTypes[e.ActionType] {
			continue
		}
		if e.ActionType == sim.ActionTypeSpoke && e.HuddleID != "" && e.HuddleID == actorSnap.CurrentHuddleID {
			continue // the current huddle's ring already renders this line
		}
		// LLM-366: flag a walked entry as a dead end only when the active shut memory
		// was observed at or before this walk. The walked entry and the ObservedClosed
		// stamp share the same arrival timestamp (both ActorArrived.At —
		// cascade/action_log.go, closed_business.go), so the arrival that found it shut
		// has walkTime == observedAt, while an earlier walk to the same structure has
		// walkTime < observedAt and must NOT be retroactively marked (it found the place
		// open, which would have cleared the memory). A raw ObservedClosed read, NOT
		// businessRememberedShut: this is a PAST-trip outcome, so the in-flight-
		// destination guard must not suppress it even while the actor re-walks there.
		//
		// Keyed off DestStructureID (where the trip was AIMED) rather than the entry's
		// StructureID (the conversational scope the actor came to rest in). LLM-463: the
		// scope is deliberately blank for an actor loitering outside a SHUT structure
		// (loiterScopeConversable), so keying off it silently lost the dead-end mark for
		// exactly the trips this exists to name — every arrival at the shut Tavern from
		// the moment its loiter pin moved outside its footprint.
		foundShut := false
		if e.ActionType == sim.ActionTypeWalked && e.DestStructureID != "" {
			key := sim.ObservedStateKey{StructureID: e.DestStructureID, Condition: sim.ObservedClosed}
			if observedAt, ok := actorSnap.Observed.At(key); ok &&
				!e.OccurredAt.Before(observedAt) &&
				actorSnap.Observed.Active(key, snap.PublishedAt) {
				foundShut = true
			}
		}
		out = append(out, SelfActionView{
			ActionType:       e.ActionType,
			Text:             e.Text,
			CounterpartyName: e.CounterpartyName,
			Amount:           e.Amount,
			PayItems:         append([]sim.ItemKindQty(nil), e.PayItems...),
			At:               e.OccurredAt,
			FoundShut:        foundShut,
		})
	}
	return out
}

// currentHeardExcerpts indexes the speech utterances in this tick's consumed
// warrant batch by speaker, so buildRelationships can drop a `heard` SalientFact
// that the "## Since your last turn" section already renders (ZBBS-WORK-374), and
// buildRecentConversation can drop the same line from the huddle ring.
//
// Both forms of every utterance are indexed (LLM-396). The warrant Excerpt now
// carries the FULL text, but the two things it is matched against are still
// stored in the 220-rune SalientFact form:
//
//   - buildRelationships compares a SalientFact.Text, truncated at write time by
//     NewSalientFact to MaxSalientFactTextLen.
//   - buildRecentConversation compares recentConversationDedupKey(ring text),
//     which normalizes to that same form.
//
// Indexing the full excerpt alone would silently fail both matches on any
// utterance over 220 runes (40% of them), re-introducing the ZBBS-WORK-374
// duplication — the model shown the same line twice and reinforcing it. Indexing
// both keys keeps every comparison exact regardless of length; for a short
// utterance the two keys coincide and the set simply dedups them.
//
// Returns nil when the batch carries no speech (the common non-conversational
// tick).
// The value is a LIST of source keys, not one (LLM-542, code_review). Several
// speech warrants in a batch can land on the same index key — identical short
// lines, or distinct long ones sharing a prefix, since the dedup key is a
// truncation. Keeping only the last would make the de-dup's conveyance claim
// depend on warrant order, and a dropped carrier could shadow a rendered one.
// The rule the discharge needs is "ANY surviving carrier is enough", which
// takes the whole set.
func currentHeardExcerpts(warrants []sim.WarrantMeta) map[sim.ActorID]map[string][]sim.WarrantSourceKey {
	var bySpeaker map[sim.ActorID]map[string][]sim.WarrantSourceKey
	addKey := func(speaker sim.ActorID, index string, key sim.WarrantSourceKey) {
		// The index key is always recorded — its PRESENCE is what drives the
		// de-dup, and that must not depend on the carrier being usable.
		carriers, seen := bySpeaker[speaker][index]
		if !seen {
			carriers = []sim.WarrantSourceKey{}
		}
		// A zero discriminator is WarrantSourceKey's "not event-sourced"
		// sentinel. It bypasses SOURCE-AWARE dedup — tryStampWarrant's three
		// key-matched paths and this file's carrier accounting — but NOT the
		// index-based speech de-dup above, which is why the key is recorded
		// either way. So it is not a usable carrier and must not be stored as
		// one (code_review). The line then reads as unconditionally conveyed,
		// which is the right default: the excerpt renders regardless of its
		// SpeechID. Unreachable from the emit path (EventIDs start at 1), but
		// this is where the invariant belongs.
		if key.Discriminator != 0 {
			for _, existing := range carriers {
				if existing == key {
					bySpeaker[speaker][index] = carriers
					return
				}
			}
			carriers = append(carriers, key)
		}
		bySpeaker[speaker][index] = carriers
	}
	add := func(speaker sim.ActorID, excerpt string, key sim.WarrantSourceKey) {
		if speaker == "" || excerpt == "" {
			return
		}
		if bySpeaker == nil {
			bySpeaker = make(map[sim.ActorID]map[string][]sim.WarrantSourceKey)
		}
		if bySpeaker[speaker] == nil {
			bySpeaker[speaker] = make(map[string][]sim.WarrantSourceKey)
		}
		addKey(speaker, excerpt, key)
		// For a short utterance the two index keys coincide; addKey's identity
		// check keeps the list from carrying the same carrier twice.
		addKey(speaker, recentConversationDedupKey(excerpt), key)
	}
	for _, w := range warrants {
		switch r := w.Reason.(type) {
		case sim.PCSpeechWarrantReason:
			add(r.Speaker, r.Excerpt, sim.WarrantSourceKey{Kind: sim.WarrantKindPCSpoke, Discriminator: uint64(r.SpeechID)})
		case sim.NPCSpeechWarrantReason:
			add(r.Speaker, r.Excerpt, sim.WarrantSourceKey{Kind: sim.WarrantKindNPCSpoke, Discriminator: uint64(r.SpeechID)})
		}
	}
	return bySpeaker
}

// buildPendingOrderViews scans snap.Orders for open Orders touching
// the subject and returns two slices:
//   - fromMe: Orders where subject is the seller (handed-over duty).
//   - toMe:   Orders where subject is the buyer OR a consumer
//     (incoming delivery).
//
// Only OrderStateReady entries appear; terminal Orders are filtered
// out (no actionable signal). Returns nil for empty results so render
// can content-gate cheaply.
//
// Names are resolved via snap.Actors; missing actors fall back to the
// stringified ActorID (defensive, e.g. if an actor was deleted between
// Order creation and the next snapshot).
//
// ConsumerNames is populated only when ConsumerIDs differs from
// [BuyerID] — the implicit "buyer is the consumer" case leaves it
// empty so render skips the "and others" embellishment.
//
// Ordering: by Order.ID ascending. Deterministic across runs.
//
// AbsentRecipientNames is populated for the fromMe bucket only: the consumers
// not currently sharing the seller's huddle, whom DeliverOrder's gate-6
// co-presence check would reject. Empty => deliverable now. The toMe bucket
// leaves it nil (not meaningful buyer-side). ZBBS-WORK-373.
func buildPendingOrderViews(snap *sim.Snapshot, subject sim.ActorID) (fromMe, toMe []OrderView) {
	if snap == nil || len(snap.Orders) == 0 {
		return nil, nil
	}
	resolveName := func(id sim.ActorID) string {
		if a := snap.Actors[id]; a != nil && a.DisplayName != "" {
			return a.DisplayName
		}
		return string(id)
	}
	// Pre-collect IDs so we can sort deterministically before
	// resolving names + building views.
	var fromIDs, toIDs []sim.OrderID
	for id, o := range snap.Orders {
		if o == nil || o.State != sim.OrderStateReady {
			continue
		}
		if o.SellerID == subject {
			fromIDs = append(fromIDs, id)
			continue
		}
		// toMe: subject is buyer or in ConsumerIDs.
		if o.BuyerID == subject {
			toIDs = append(toIDs, id)
			continue
		}
		for _, cid := range o.ConsumerIDs {
			if cid == subject {
				toIDs = append(toIDs, id)
				break
			}
		}
	}
	sort.Slice(fromIDs, func(i, j int) bool { return fromIDs[i] < fromIDs[j] })
	sort.Slice(toIDs, func(i, j int) bool { return toIDs[i] < toIDs[j] })

	toView := func(o *sim.Order) OrderView {
		v := OrderView{
			ID:          o.ID,
			Item:        o.Item,
			Qty:         o.Qty,
			BuyerName:   resolveName(o.BuyerID),
			SellerName:  resolveName(o.SellerID),
			CreatedAt:   o.CreatedAt,
			ExpiresAt:   o.ExpiresAt,
			ReadyBy:     o.ReadyBy,
			BalanceDue:  sim.OrderBalanceDue(o),
			DepositPaid: o.Deposit,
		}
		// Only populate ConsumerNames when there's more than just
		// the implicit buyer-as-consumer entry.
		if len(o.ConsumerIDs) > 1 || (len(o.ConsumerIDs) == 1 && o.ConsumerIDs[0] != o.BuyerID) {
			v.ConsumerNames = make([]string, 0, len(o.ConsumerIDs))
			for _, cid := range o.ConsumerIDs {
				v.ConsumerNames = append(v.ConsumerNames, resolveName(cid))
			}
		}
		return v
	}
	if len(fromIDs) > 0 {
		seller := snap.Actors[subject]
		fromMe = make([]OrderView, 0, len(fromIDs))
		for _, id := range fromIDs {
			o := snap.Orders[id]
			v := toView(o)
			v.AbsentRecipientNames = absentRecipientNames(snap, seller, o, resolveName)
			v.AwaitingMake = orderAwaitingMake(seller, o)
			if v.AwaitingMake {
				// LLM-635: if the make is stalled on an input, say which — the
				// "yet to make it" line otherwise steers toward a produce tool
				// LLM-324 has withdrawn.
				v.MissingInputs = missingProduceInputs(snap, seller, o.Item)
			}
			fromMe = append(fromMe, v)
		}
	}
	if len(toIDs) > 0 {
		toMe = make([]OrderView, 0, len(toIDs))
		for _, id := range toIDs {
			toMe = append(toMe, toView(snap.Orders[id]))
		}
	}
	return fromMe, toMe
}

// absentRecipientNames returns the display names (sorted) of an order's
// consumers who do NOT currently share the seller's huddle — the recipients
// DeliverOrder's gate-6 co-presence check (order_commands.go) would reject a
// handover to. An empty result means every recipient is here and the order is
// deliverable now. A nil seller or a seller in no huddle makes every consumer
// absent: a keeper in no conversation can hand nothing over. Seller-relative,
// so it is meaningful only for the seller-side PendingDeliveriesFromMe bucket.
// ZBBS-WORK-373 (boot-collapse Finding 6 bundle).
func absentRecipientNames(snap *sim.Snapshot, seller *sim.ActorSnapshot, o *sim.Order, resolveName func(sim.ActorID) string) []string {
	var sellerHuddle sim.HuddleID
	if seller != nil {
		sellerHuddle = seller.CurrentHuddleID
	}
	var absent []string
	for _, cid := range o.ConsumerIDs {
		coPresent := sellerHuddle != ""
		if coPresent {
			c := snap.Actors[cid]
			coPresent = c != nil && c.CurrentHuddleID == sellerHuddle
		}
		if !coPresent {
			absent = append(absent, resolveName(cid))
		}
	}
	sort.Strings(absent)
	return absent
}

// orderAwaitingMake reports whether a seller-side Ready order can't be handed
// over yet but the seller could MAKE the good: one it PRODUCES, held below the
// order's need — so DeliverOrder's gate-5 stock check would bounce a
// deliver_order call. Mirrors that gate (seller.Inventory[Item] >= Qty *
// len(ConsumerIDs)) so the cue and the substrate agree on "can this be handed
// over now." In practice this is exactly a made-to-order commission (LLM-338): a
// normal produced-good take-home is delivered at accept (never Ready here) and
// service / lodging orders aren't Produces()'d. But the check is behavioral, not
// order-identity-based — Produces(o.Item) alone can't prove commission origin — so
// it also correctly steers "make it first" for ANY Ready produced-good order gate
// 5 would reject (e.g. a seller who lost stock after accept), which is the right
// behavior in every case. Seller-relative — meaningful only for the
// PendingDeliveriesFromMe bucket. Nil seller / policy, or a malformed order shape,
// makes nothing.
func orderAwaitingMake(seller *sim.ActorSnapshot, o *sim.Order) bool {
	if seller == nil || o == nil {
		return false
	}
	if !seller.RestockPolicy.Produces(o.Item) {
		return false
	}
	// Defensive against a malformed order shape (mirrors isCommissionOrder and
	// DeliverOrder's own guards): reject non-positive qty/consumers and the
	// multiplication overflow rather than let `needed` wrap negative and mark an
	// unfulfillable order deliverable.
	consumers := len(o.ConsumerIDs)
	if consumers <= 0 || o.Qty <= 0 || o.Qty > math.MaxInt/consumers {
		return false
	}
	needed := o.Qty * consumers
	return seller.Inventory[o.Item] < needed
}

// recentlyResolvedOfferWindow bounds how long a just-settled offer stays in the
// buyer's "## Recently settled offers" view. Short — the view bridges the gap
// until the buyer's next deliberation registers the resolution, it is not a
// purchase log. ~3 min matches the pending-offer TTL scale (the conversational
// moment); the terminal ledger entry itself lingers up to
// PayLedgerTerminalRetention (1h), far longer than we want to keep narrating it.
const recentlyResolvedOfferWindow = 3 * time.Minute

// stockShortNoun is the plural counting noun for the pay-offer "hold no <plural>"
// shortfall copy (e.g. "nails"), used by both the seller-side pending cue
// (buildPayOfferShortfalls) and the buyer-side settled reason
// (buildRecentlyResolvedOffersFromMe) at zero held. Falls back to the raw kind key
// when the catalog carries no plural phrase (a discovery-minted kind,
// ZBBS-WORK-412) or the def is absent. ItemKindDef.Plural is nil-safe. LLM-303.
func stockShortNoun(def *sim.ItemKindDef, kind sim.ItemKind) string {
	if n := def.Plural(); n != "" {
		return n
	}
	return string(kind)
}

// buildPayOfferShortfalls computes, per pending pay offer, the seller's shortfall
// on the asked good — the data renderPayOffers turns into the "you hold no/only N"
// annotation (Payload.PayOfferShortfalls). An offer is included only when the
// asked kind is a real good (a "service" kind has no inventory backing, so the
// engine skips its accept_pay stock gate — item_kind.go — and "you hold no X"
// would be a false alarm) AND the buyer asks more than the seller holds (Held read
// from the subject's own inventory, 0 when the seller carries none). Returns nil
// when nothing is short, keeping render catalog-free. LLM-303: this is what makes
// the warning fire for a non-vendor offeree holding zero of the asked item, the
// gap that let a live NPC confabulate stock it never had.
func buildPayOfferShortfalls(snap *sim.Snapshot, offers []sim.PayOfferWarrantReason, actorSnap *sim.ActorSnapshot) map[sim.LedgerID]StockShortfall {
	if snap == nil || actorSnap == nil || len(offers) == 0 {
		return nil
	}
	var out map[sim.LedgerID]StockShortfall
	for _, o := range offers {
		def := snap.ItemKinds[o.Item]
		if def != nil && def.HasCapability("service") {
			continue // no inventory backing — stock is meaningless
		}
		held := actorSnap.Inventory[o.Item] // 0 when the seller holds none
		if o.Qty <= held {
			continue // the ask doesn't bite — sufficient stock adds nothing
		}
		if out == nil {
			out = make(map[sim.LedgerID]StockShortfall)
		}
		out[o.LedgerID] = StockShortfall{Held: held, Noun: stockShortNoun(def, o.Item)}
	}
	return out
}

// buildRecentlyResolvedOffersFromMe scans snap.PayLedger for the subject's OWN
// offers that left Pending within recentlyResolvedOfferWindow of
// snap.PublishedAt — entries where the subject is the BUYER and the state is a
// terminal resolution (Countered excluded: it is an active flow the buyer must
// still answer, not a closed deal — surfaced separately by
// buildCountersAwaitingMyResponse). It is the buyer-side
// resolution companion to buildPendingOffersFromMe: it closes the blind window
// between an offer leaving the pending scan and the PayResolvedWarrantReason
// event surfacing, which can lag a tick behind the buyer's in-flight
// deliberation and let the buyer re-buy a need already met. Sourced from the
// ledger, not a warrant, so it is robust to warrant emit timing. Seller name is
// acquaintance-gated like the pending view. Returns nil for none. Ordering: by
// LedgerID ascending, deterministic.
func buildRecentlyResolvedOffersFromMe(snap *sim.Snapshot, subject sim.ActorID, subjectSnap *sim.ActorSnapshot) []ResolvedOfferView {
	if snap == nil || len(snap.PayLedger) == 0 {
		return nil
	}
	var ids []sim.LedgerID
	for id, e := range snap.PayLedger {
		if e == nil || e.BuyerID != subject {
			continue
		}
		if e.IsGift {
			continue // the subject's settled gifts render via buildSettledGiftsFromMe (LLM-138)
		}
		if e.State == sim.PayLedgerStatePending || e.State == sim.PayLedgerStateCountered {
			continue
		}
		if e.ResolvedAt.IsZero() {
			continue
		}
		if snap.PublishedAt.Sub(e.ResolvedAt) > recentlyResolvedOfferWindow {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	resolveSeller := func(id sim.ActorID) string {
		seller := snap.Actors[id]
		if seller == nil {
			return string(id)
		}
		acquainted := false
		if subjectSnap != nil && seller.DisplayName != "" {
			_, acquainted = subjectSnap.Acquaintances[seller.DisplayName]
		}
		return descriptorLabel(seller.DisplayName, seller.Role, acquainted)
	}

	// LLM-512: a ledger whose accept minted an Order still at Ready means the
	// goods have NOT been handed over yet — the seller holds them until
	// deliver_order. The settled-offers line must not claim "it's in your pack now"
	// for these (false, and it contradicts the same prompt's "## Orders you're
	// waiting on" section, which seeds a false-possession memory). Scan snap.Orders
	// once, keyed by the originating ledger, for orders the subject is waiting on.
	//
	// Contract this relies on: Ready is the sole pre-delivery Order state (order.go
	// — deliver_order flips it to the terminal Delivered), so a non-Ready order has
	// already been handed over (or expired) and correctly leaves the flag unset.
	// LedgerID(0) is the reserved unset sentinel (pay_ledger.go); a real
	// ledger-minted order carries LedgerID >= 1 and a PayLedger entry's own ID is
	// likewise >= 1, so a ledger-less order (LedgerID 0) can never pair with a
	// settled entry — skip it rather than seed a spurious pendingByLedger[0].
	pendingByLedger := map[sim.LedgerID]bool{}
	for _, o := range snap.Orders {
		if o == nil || o.State != sim.OrderStateReady || o.LedgerID == 0 {
			continue
		}
		waiting := o.BuyerID == subject
		for _, cid := range o.ConsumerIDs {
			if cid == subject {
				waiting = true
				break
			}
		}
		if waiting {
			pendingByLedger[o.LedgerID] = true
		}
	}

	views := make([]ResolvedOfferView, 0, len(ids))
	for _, id := range ids {
		e := snap.PayLedger[id]
		accepted := e.State == sim.PayLedgerStateAccepted
		// LLM-296: for a CLOSE (not an accept), carry the seller's current on-hand
		// of the bought kind so the render can name a stock shortfall as the
		// engine-known reason the deal fell through — the buyer's mirror of the
		// seller-side "you hold only N" pay-offer cue. Read straight from the
		// seller's snapshot inventory (0 when they hold none). LLM-303: name the
		// shortfall for ANY real good the seller is short on, including zero held —
		// the live case was a non-vendor seller holding no nails, whose bare "it's
		// closed" left the buyer to re-offer into the void. Only a "service" kind
		// (no inventory backing — item_kind.go) leaves SellerStocks false, since
		// its stock is meaningless. The render gates the annotation on the bite
		// (Qty > stock), so a seller who holds enough carries the count but shows
		// no shortfall.
		sellerStock, sellerStocks, sellerStockNoun := 0, false, ""
		if !accepted {
			def := snap.ItemKinds[e.ItemKind]
			isService := def != nil && def.HasCapability("service")
			// Name the shortfall only when we can actually read the seller's stock
			// (a non-nil snapshot) and the asked kind carries inventory (not a
			// service) — otherwise fall back to the bare "it's closed" rather than
			// assert a stock level we never inspected.
			if seller := snap.Actors[e.SellerID]; seller != nil && !isService {
				sellerStocks = true
				sellerStock = seller.Inventory[e.ItemKind] // 0 when the seller holds none
				sellerStockNoun = stockShortNoun(def, e.ItemKind)
			}
		}
		views = append(views, ResolvedOfferView{
			LedgerID:        e.ID,
			SellerName:      resolveSeller(e.SellerID),
			Item:            e.ItemKind,
			Qty:             e.Qty,
			Amount:          e.Amount,
			PayItems:        e.PayItems,
			Accepted:        accepted,
			ConsumeNow:      e.ConsumeNow,
			DeliveryPending: accepted && pendingByLedger[e.ID],
			KeptUnits:       e.KeptUnits,
			SellerStock:     sellerStock,
			SellerStocks:    sellerStocks,
			SellerStockNoun: sellerStockNoun,
		})
	}
	return views
}

// counterResponseWindow bounds how long a seller's counter is surfaced to the
// buyer as awaiting a response. A countered parent entry is terminal and lingers
// in the ledger up to PayLedgerTerminalRetention (1h); without a window the buyer
// would be nagged about a stale counter long after the moment passed. Matched to
// the pending-offer TTL scale (the window an un-acted offer would have expired
// in) so a counter stops reading as "live" on the same cadence.
const counterResponseWindow = 3 * time.Minute

// buildCountersAwaitingMyResponse scans snap.PayLedger for a seller's counter the
// subject (as buyer) has not yet answered: terminal Countered entries where the
// subject is the BUYER, resolved within counterResponseWindow of
// snap.PublishedAt, still below the counter-chain depth cap (validateInResponseTo
// rejects a response once parent.Depth reaches MaxPayCounterChainDepth, so a
// capped counter can't be taken — don't steer one), and with no child entry
// chained via ParentID (a buyer's response creates such a child, so a counter
// with one has been answered).
//
// It is the buyer-side standing decision view of a counter — the counterpart to
// the seller's buildPayOffersForMe standing scan (ZBBS-HOME-453). It reads the
// ledger rather than the PayResolvedWarrantReason{Countered} event because of
// LLM-21: that warrant can ride a tick behind the buyer's in-flight deliberation
// (a counter stamped mid-tick opens a fresh cycle the in-flight tick never
// carries), and unlike an accept/decline the recently-settled scan deliberately
// excludes Countered, so the warrant is the ONLY thing that surfaces a counter —
// a buyer could re-offer a need already in negotiation, or miss the counter
// entirely if the warrant is evicted while the buyer is shelved. The per-tick
// ledger scan is robust to that timing.
//
// Seller name acquaintance-gated like the pending view. Returns nil for none.
// Ordering: by LedgerID ascending, deterministic across runs.
func buildCountersAwaitingMyResponse(snap *sim.Snapshot, subject sim.ActorID, subjectSnap *sim.ActorSnapshot) []CounterOfferView {
	if snap == nil || len(snap.PayLedger) == 0 {
		return nil
	}
	// A buyer's response to a counter is a fresh entry chained via ParentID, so a
	// countered entry that already has such a child has been answered and must
	// not be re-surfaced. Collect answered parents in one pass.
	answered := make(map[sim.LedgerID]struct{})
	for _, e := range snap.PayLedger {
		if e != nil && e.ParentID != 0 {
			answered[e.ParentID] = struct{}{}
		}
	}
	var ids []sim.LedgerID
	for id, e := range snap.PayLedger {
		if e == nil || e.BuyerID != subject {
			continue
		}
		if e.State != sim.PayLedgerStateCountered {
			continue
		}
		if e.Depth >= sim.MaxPayCounterChainDepth {
			continue
		}
		if e.ResolvedAt.IsZero() {
			continue
		}
		if snap.PublishedAt.Sub(e.ResolvedAt) > counterResponseWindow {
			continue
		}
		if _, done := answered[id]; done {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	resolveSeller := func(id sim.ActorID) string {
		seller := snap.Actors[id]
		if seller == nil {
			return string(id)
		}
		acquainted := false
		if subjectSnap != nil && seller.DisplayName != "" {
			_, acquainted = subjectSnap.Acquaintances[seller.DisplayName]
		}
		return descriptorLabel(seller.DisplayName, seller.Role, acquainted)
	}

	views := make([]CounterOfferView, 0, len(ids))
	for _, id := range ids {
		e := snap.PayLedger[id]
		views = append(views, CounterOfferView{
			LedgerID:      e.ID,
			SellerName:    resolveSeller(e.SellerID),
			Item:          e.ItemKind,
			Qty:           e.Qty,
			CounterAmount: e.CounterAmount,
			// snap.PayLedger entries are deep-cloned at publish, so aliasing the
			// snapshot's CounterPayItems slice into the read-only, per-tick view
			// is safe — same posture as buildPendingOffersFromMe's PayItems.
			CounterPayItems: e.CounterPayItems,
		})
	}
	return views
}

// buildPendingOffersFromMe scans snap.PayLedger for the subject's OWN still-
// pending pay-with-item offers — entries where the subject is the BUYER and the
// state is Pending (the only non-terminal pay-ledger state) — and projects each
// to a PendingOfferView for the "## Offers you have standing" cue (ZBBS-HOME-413).
//
// This is the buyer-side counterpart to the seller's PayOfferWarrants: the
// seller learns of an offer via a warrant stamped on them, but the buyer gets
// NO warrant for an offer they placed, so without this scan a buyer has no
// cross-tick memory of an outstanding offer and re-stakes the same one every
// tick (the repeat-offer storm). The data comes from the ledger, not a warrant,
// for exactly that reason.
//
// The seller's name is acquaintance-gated (descriptorLabel against the
// subject's Acquaintances) — the same name-vs-descriptor gating the seller side
// applies to the buyer. Returns nil for no pending offers so render content-
// gates cheaply. Ordering: by LedgerID ascending, deterministic across runs.
func buildPendingOffersFromMe(snap *sim.Snapshot, subject sim.ActorID, subjectSnap *sim.ActorSnapshot) []PendingOfferView {
	if snap == nil || len(snap.PayLedger) == 0 {
		return nil
	}
	// Pre-collect IDs so the views sort deterministically by LedgerID.
	var ids []sim.LedgerID
	for id, e := range snap.PayLedger {
		if e == nil || e.State != sim.PayLedgerStatePending {
			continue
		}
		if e.BuyerID != subject {
			continue
		}
		if e.IsGift {
			continue // the subject's own pending gifts render via buildGiftsFromMe (LLM-138)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	resolveSeller := func(id sim.ActorID) string {
		seller := snap.Actors[id]
		if seller == nil {
			return string(id)
		}
		acquainted := false
		if subjectSnap != nil && seller.DisplayName != "" {
			_, acquainted = subjectSnap.Acquaintances[seller.DisplayName]
		}
		return descriptorLabel(seller.DisplayName, seller.Role, acquainted)
	}

	views := make([]PendingOfferView, 0, len(ids))
	for _, id := range ids {
		e := snap.PayLedger[id]
		views = append(views, PendingOfferView{
			LedgerID:   e.ID,
			SellerName: resolveSeller(e.SellerID),
			Item:       e.ItemKind,
			Qty:        e.Qty,
			Amount:     e.Amount,
			// snap.PayLedger entries are deep-cloned at publish, so the
			// snapshot's PayItems slice is already isolated from world state;
			// aliasing it into the (read-only, per-tick) view is safe.
			PayItems: e.PayItems,
		})
	}
	return views
}

// buildStandingQuotesFromMe scans snap.Quotes for the subject's OWN still-active
// scene-quotes — the offers-to-sell it posted as SELLER (sell / scene_quote) —
// and projects each to a StandingQuoteView for the seller-side "## Offers you've
// put out" cue (LLM-45).
//
// This is the seller/scene_quote counterpart to buildPendingOffersFromMe (the
// buyer/pay_with_item HOME-413 scan), and it exists for the identical reason: a
// seller has NO cross-tick memory of an offer it posted. buildOfferableCustomers
// already suppresses a re-pitch once a quote stands (sellerHasActiveQuoteToBuyer),
// but nothing then tells the seller WHAT it offered to WHOM — so a weak model
// loses the thread, re-posts the same quote (the already_quoted thrash), and
// confabulates a queue between two co-present seekers ("I offered Ezekiel, you
// must wait") even as its own offer to the asker stands. The data comes from the
// live quote map (bounded by RunSceneQuoteSweep's TTL), not a warrant, for
// exactly that reason.
//
// Both targeted (TargetBuyer set) and public (TargetBuyer == "") quotes surface:
// sellerHasActiveQuoteToBuyer only tracks targeted quotes, so a public offer is
// otherwise invisible to its own author. The buyer's name is acquaintance-gated
// (descriptorLabel) like the buyer-side scan. Returns nil for none so render
// content-gates cheaply. Ordering: by QuoteID ascending, deterministic.
func buildStandingQuotesFromMe(snap *sim.Snapshot, subject sim.ActorID, subjectSnap *sim.ActorSnapshot) []StandingQuoteView {
	if snap == nil || len(snap.Quotes) == 0 {
		return nil
	}
	var ids []sim.QuoteID
	for id, q := range snap.Quotes {
		if q == nil || q.State != sim.SceneQuoteStateActive {
			continue
		}
		if q.SellerID != subject {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	resolveBuyer := func(id sim.ActorID) string {
		buyer := snap.Actors[id]
		if buyer == nil {
			// A targeted buyer who has left the snapshot (rare) falls back to a
			// generic descriptor rather than leaking the raw internal actor id
			// into the prompt — the same "someone" token the render layer uses.
			return "someone"
		}
		acquainted := false
		if subjectSnap != nil && buyer.DisplayName != "" {
			_, acquainted = subjectSnap.Acquaintances[buyer.DisplayName]
		}
		return descriptorLabel(buyer.DisplayName, buyer.Role, acquainted)
	}

	views := make([]StandingQuoteView, 0, len(ids))
	for _, id := range ids {
		q := snap.Quotes[id]
		buyerName := ""
		if q.TargetBuyer != "" {
			buyerName = resolveBuyer(q.TargetBuyer)
		}
		views = append(views, StandingQuoteView{
			QuoteID:   q.ID,
			BuyerName: buyerName,
			Lines:     q.Lines,
			Amount:    q.Amount,
		})
	}
	return views
}

// buildRecentlyShortfallQuotesFromMe scans snap.Quotes for the subject's OWN
// sell lots that JUST fell through — quotes the pre-publish coverage reconcile
// (reconcileQuoteCoverage) flipped to terminal SceneQuoteStateShortfall within
// recentlyResolvedOfferWindow of snap.PublishedAt — and projects each to an
// UncoverableOfferView for the flat "## An offer you couldn't keep" beat (LLM-409).
//
// This is the seller/scene_quote resolution counterpart to
// buildRecentlyResolvedOffersFromMe (the buyer/pay_with_item settlement view):
// buildStandingQuotesFromMe drops a lot the instant it goes terminal, so a
// seller who spent the quoted goods away sees his standing-offer row simply
// vanish. That fixes the absorbing state but loses the thread — he announced the
// offer aloud, and without a beat he has no memory of it when the buyer comes to
// take a good he no longer holds. This surfaces the broken promise so he can own
// it. The short window (same 3-minute horizon as the buyer-side resolution view)
// ages the beat out on its own — it is a one-time "you welched" nudge, not a
// standing row.
//
// The buyer's name is acquaintance-gated (descriptorLabel), empty for a public
// lot — mirroring buildStandingQuotesFromMe. Returns nil for none so render
// content-gates cheaply. Ordering: by QuoteID ascending, deterministic.
func buildRecentlyShortfallQuotesFromMe(snap *sim.Snapshot, subject sim.ActorID, subjectSnap *sim.ActorSnapshot) []UncoverableOfferView {
	if snap == nil || len(snap.Quotes) == 0 {
		return nil
	}
	var ids []sim.QuoteID
	for id, q := range snap.Quotes {
		if q == nil || q.SellerID != subject {
			continue
		}
		if q.State != sim.SceneQuoteStateShortfall {
			continue
		}
		// Fail closed on a future-dated ResolvedAt (age < 0): production stamps
		// ResolvedAt before PublishedAt in World.Run, but this reads arbitrary
		// snapshots, and a negative age would otherwise slip past the window as if
		// fresh (code_review, LLM-409).
		age := snap.PublishedAt.Sub(q.ResolvedAt)
		if q.ResolvedAt.IsZero() || age < 0 || age > recentlyResolvedOfferWindow {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	resolveBuyer := func(id sim.ActorID) string {
		buyer := snap.Actors[id]
		if buyer == nil {
			return "someone"
		}
		acquainted := false
		if subjectSnap != nil && buyer.DisplayName != "" {
			_, acquainted = subjectSnap.Acquaintances[buyer.DisplayName]
		}
		return descriptorLabel(buyer.DisplayName, buyer.Role, acquainted)
	}

	views := make([]UncoverableOfferView, 0, len(ids))
	for _, id := range ids {
		q := snap.Quotes[id]
		buyerName := ""
		if q.TargetBuyer != "" {
			buyerName = resolveBuyer(q.TargetBuyer)
		}
		views = append(views, UncoverableOfferView{
			BuyerName: buyerName,
			Lines:     q.Lines,
		})
	}
	return views
}

// buildStandingQuotesToMe collects the active scene-quotes ADDRESSED TO the
// subject and projects each to a StandingQuoteToMeView for the buyer-side
// "## Offers made to you" section (LLM-551). The mirror of
// buildStandingQuotesFromMe, filtering on TargetBuyer rather than SellerID.
//
// Every filter here answers ONE question: can the subject actually take this
// offer on this tick? The section's lines end in "call pay_with_item with
// quote_id N", so anything it lists and the buy path would refuse is a fresh
// instance of the very defect this ticket fixes — a cue at war with its gate.
// Hence:
//
//   - TARGETED only. A public quote (empty TargetBuyer) is an advertisement to
//     the room, not an offer in this subject's name, and standing one in every
//     bystander's prompt would pin passers-by to a stall they never approached.
//   - NOT EXPIRED. State alone is not enough: RunSceneQuoteSweep flips a lapsed
//     quote to expired, so between its ExpiresAt and the next sweep the map
//     still reads Active. activeTargetedQuoteOffers (the dispatch-side scan)
//     checks the deadline, so without the same check here perception would hand
//     out a take-instruction for a quote the fast path then refuses.
//   - SELLER CO-PRESENT, in this subject's own huddle. pay_with_item resolves
//     its seller against huddle peers and rejects with "no one named X in this
//     conversation" otherwise. A quote's scene can span several huddles, so
//     scene membership is too weak a test for an ACTIONABLE line: the buyer must
//     be able to reach the seller now.
//   - NOT THE SUBJECT'S OWN. scene_quote rejects self-targeting, so this cannot
//     arise from the command path; checked anyway rather than resting the
//     section's correctness on another component's validation holding for
//     malformed or legacy rows.
//
// A lodging quote to a subject who already has a home is dropped — she cannot
// take the room (the buyer-side pay_with_item guard rejects it, LLM-182), so
// standing the offer would dangle a doomed nightly negotiation (LLM-208). Same
// per-viewer rule filterHomedLodgingQuoteWarrants applies to the warrant path;
// a homeless seeker still sees it.
//
// A quote announced by THIS tick's warrant batch is skipped: renderWarrantLine
// already states its terms and take-instruction, and standing a second copy in
// the section below would say the same thing twice in one prompt. The section is
// what the buyer sees on every LATER tick, once that one-shot warrant is spent —
// which is the whole defect. Keyed on QuoteID off SceneQuoteTargetedWarrantReason.
//
// LLM-171 redundancy (the buyer makes this good itself / is at cap) is applied
// at RENDER, not here, exactly as it is for the warrant line — the view carries
// the facts and render decides whether to withhold the take.
//
// Nil-safe; returns nil for none so render content-gates cheaply. Ordering: by
// the quote's own ID ascending, deterministic — collected from SceneQuote.ID
// rather than the map key so a key/ID divergence in malformed data cannot order
// the list by one identifier and render another.
func buildStandingQuotesToMe(
	snap *sim.Snapshot,
	subject sim.ActorID,
	subjectSnap *sim.ActorSnapshot,
	eatHereKinds map[sim.ItemKind]bool,
	warrants []sim.WarrantMeta,
) []StandingQuoteToMeView {
	if snap == nil || len(snap.Quotes) == 0 || subjectSnap == nil {
		return nil
	}
	announcedThisTick := make(map[sim.QuoteID]bool, len(warrants))
	for _, w := range warrants {
		if r, ok := w.Reason.(sim.SceneQuoteTargetedWarrantReason); ok {
			announcedThisTick[r.QuoteID] = true
		}
	}
	homed := subjectSnap.HomeStructureID != ""
	// The buyer must be in a conversation at all: pay_with_item resolves its
	// seller among the peers of the caller's current huddle.
	huddle := subjectSnap.CurrentHuddleID
	if huddle == "" {
		return nil
	}
	var eligible []*sim.SceneQuote
	for _, q := range snap.Quotes {
		if q == nil || q.State != sim.SceneQuoteStateActive {
			continue
		}
		if q.TargetBuyer != subject || q.SellerID == subject {
			continue
		}
		if !q.ExpiresAt.IsZero() && !snap.PublishedAt.Before(q.ExpiresAt) {
			continue
		}
		if seller := snap.Actors[q.SellerID]; seller == nil || seller.CurrentHuddleID != huddle {
			continue
		}
		if announcedThisTick[q.ID] {
			continue
		}
		if homed && quoteGrantsLodging(snap, q.Lines) {
			continue
		}
		eligible = append(eligible, q)
	}
	if len(eligible) == 0 {
		return nil
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })

	views := make([]StandingQuoteToMeView, 0, len(eligible))
	for _, q := range eligible {
		sellerName := ""
		if seller := snap.Actors[q.SellerID]; seller != nil {
			acquainted := false
			if subjectSnap != nil && seller.DisplayName != "" {
				_, acquainted = subjectSnap.Acquaintances[seller.DisplayName]
			}
			sellerName = descriptorLabel(seller.DisplayName, seller.Role, acquainted)
		} else {
			// A seller who has left the snapshot (rare) falls back to the same
			// generic token the seller-side view uses, rather than leaking a raw
			// actor id into the prompt.
			sellerName = "someone"
		}
		// A bundle is eat-here if ANY line is non-portable — the whole bundle was
		// clamped at quote creation (LLM-101), same rule the warrant path applies.
		eatHere := false
		for _, ln := range q.Lines {
			if eatHereKinds[ln.ItemKind] {
				eatHere = true
				break
			}
		}
		views = append(views, StandingQuoteToMeView{
			QuoteID:    q.ID,
			SellerName: sellerName,
			Lines:      q.Lines,
			Amount:     q.Amount,
			EatHere:    eatHere,
		})
	}
	return views
}

// quoteGrantsLodging reports whether any line of a quote is a lodging good —
// the bundle-aware form of itemGrantsLodging, matching how
// filterHomedLodgingQuoteWarrants tests a warrant's lines.
func quoteGrantsLodging(snap *sim.Snapshot, lines []sim.QuoteLine) bool {
	for _, ln := range lines {
		if itemGrantsLodging(snap, ln.ItemKind) {
			return true
		}
	}
	return false
}

// buildPayOffersForMe scans snap.PayLedger for the still-pending offers staked
// AGAINST the subject — entries where the subject is the SELLER and the state
// is Pending — and projects each to a sim.PayOfferWarrantReason for the
// standing "## Offers awaiting your decision" section and the
// accept/decline/counter tool gate (ZBBS-HOME-453).
//
// This is the seller-side counterpart to buildPendingOffersFromMe (the
// buyer's HOME-413 scan), and it exists for the same reason: a warrant is a
// one-shot wake-up, not cross-tick memory. The PayOfferWarrant is consumed by
// the first tick it triggers, so a seller who speaks through that tick
// instead of resolving used to lose the cue AND the response tools while the
// offer sat pending — structurally unable to accept until the TTL sweep
// expired the entry (the 2026-06-12 Ellis meat deadlock). The data comes
// from the ledger, not the warrant, for exactly that reason.
//
// The projection mirrors restartReStampPayOfferWarrants' entry → reason
// mapping. snap.PayLedger entries are deep-cloned at publish, so aliasing
// PayItems / ConsumerIDs into the (read-only, per-tick) view is safe — same
// posture as buildPendingOffersFromMe. Returns nil for no pending offers so
// render and gate content-gate cheaply. Ordering: by LedgerID ascending,
// deterministic across runs.
//
// resolvedThisTick (LLM-173) withholds offers the actor has already answered
// this tick: on a mid-tick re-render the turn-start snapshot still shows a
// just-accepted offer as pending, so without this the cue re-invites a
// settlement that already happened. Empty/nil on the turn-start Build, so it
// only narrows the within-tick refresh.
func buildPayOffersForMe(snap *sim.Snapshot, subject sim.ActorID, resolvedThisTick map[sim.LedgerID]struct{}) []sim.PayOfferWarrantReason {
	if snap == nil || len(snap.PayLedger) == 0 {
		return nil
	}
	var ids []sim.LedgerID
	for id, e := range snap.PayLedger {
		if e == nil || e.State != sim.PayLedgerStatePending {
			continue
		}
		if e.SellerID != subject {
			continue
		}
		if e.IsGift {
			continue // gifts render in the LLM-138 gift lane (buildGiftsForMe), not here
		}
		if _, done := resolvedThisTick[id]; done {
			continue // already answered this tick (LLM-173) — don't re-invite the settlement
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// LLM-357: the deposit is only HONORED when the offer resolves to a
	// commission at accept (depositChargeForEntry re-checks isCommissionOrder).
	// Carry it onto the seller's decision cue ONLY when the offer is currently
	// commission-shaped — the seller produces this good and is short of it — so an
	// in-stock sale (delivered in full at accept) or a lodging/service offer isn't
	// mis-rendered as "N down as a deposit".
	seller := snap.Actors[subject]
	offers := make([]sim.PayOfferWarrantReason, 0, len(ids))
	for _, id := range ids {
		e := snap.PayLedger[id]
		deposit := 0
		if offerIsCommissionShaped(seller, e) {
			deposit = e.Deposit
		}
		offers = append(offers, sim.PayOfferWarrantReason{
			LedgerID:    e.ID,
			Buyer:       e.BuyerID,
			Item:        e.ItemKind,
			Qty:         e.Qty,
			Amount:      e.Amount,
			Deposit:     deposit,
			PayItems:    e.PayItems,
			ConsumeNow:  e.ConsumeNow,
			ConsumerIDs: e.ConsumerIDs,
			ExpiresAt:   e.ExpiresAt,
			Depth:       e.Depth,
		})
	}
	return offers
}

// offerIsCommissionShaped reports whether a pending pay offer would resolve to a
// commission at accept — the seller produces the good and is currently short of
// it, so accept can't deliver in full. Mirrors the sim isCommissionOrder core
// (coin-only, non-gift, non-consume-now, producer, stock-short) using snapshot
// data, so the seller's decision cue frames a deposit as a deposit only when the
// deal really is made-to-order. Raw inventory (no reservation subtraction), like
// orderAwaitingMake. LLM-357.
func offerIsCommissionShaped(seller *sim.ActorSnapshot, e *sim.PayLedgerEntry) bool {
	if seller == nil || e == nil || e.IsGift || e.ConsumeNow || len(e.PayItems) > 0 {
		return false
	}
	if seller.RestockPolicy == nil || !seller.RestockPolicy.Produces(e.ItemKind) {
		return false
	}
	consumers := len(e.ConsumerIDs)
	if consumers == 0 {
		consumers = 1
	}
	if e.Qty <= 0 || e.Qty > math.MaxInt/consumers {
		return false
	}
	return seller.Inventory[e.ItemKind] < e.Qty*consumers
}

// buildLaborOffersForMe scans snap.LaborLedger for the still-pending labor
// offers AWAITING THE SUBJECT'S ANSWER (state Pending, Responder() == subject),
// projecting each to a LaborOfferView for the "## Work offers awaiting your
// decision" section and the accept_work/decline_work tool gate (LLM-26). The
// responder is the employer on a solicited offer and the worker on an offered
// job (LLM-346), so this one view feeds both directions of the decision section
// — and, because the tool gate reads the same slice, a worker who has been
// offered a job is handed the answer tools exactly when the cue names the offer.
//
// The labor analog of buildPayOffersForMe; snap.LaborLedger is deep-cloned at
// publish, so the per-tick read-only view is race-free. Returns nil for no
// pending offers. Ordering: by LaborID ascending.
func buildLaborOffersForMe(snap *sim.Snapshot, subject sim.ActorID) []LaborOfferView {
	if snap == nil || len(snap.LaborLedger) == 0 {
		return nil
	}
	var ids []sim.LaborID
	for id, o := range snap.LaborLedger {
		if o == nil || o.State != sim.LaborStatePending {
			continue
		}
		if o.Responder() != subject {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// Employer holdings for the in-kind affordability mirror (LLM-225). Only
	// meaningful when the subject IS the employer: their snapshot inventory is
	// then the same map accept_work's gate 8 will check (buyerHoldsPayItems). A
	// worker weighing an offered job cannot see the keeper's purse, so the
	// affordability steer is employer-side only.
	subjectSnap := snap.Actors[subject]
	var employerInv map[sim.ItemKind]int
	if subjectSnap != nil {
		employerInv = subjectSnap.Inventory
	}
	out := make([]LaborOfferView, 0, len(ids))
	for _, id := range ids {
		o := snap.LaborLedger[id]
		subjectIsEmployer := o.EmployerID == subject
		var missing []sim.ItemKindQty
		if subjectIsEmployer {
			for _, ri := range o.RewardItems {
				if employerInv[ri.Kind] < ri.Qty {
					missing = append(missing, ri)
				}
			}
		}
		// LLM-228: did this worker complete a paid job for the subject within the
		// memory window? The memory lives on the EMPLOYER's Observed store, keyed by
		// the worker's PeerID, so the recall only applies when the subject is the
		// employer. When Active, the decision section adds a returning-helper line.
		helpedBefore := subjectIsEmployer && subjectSnap != nil && subjectSnap.Observed.Active(
			sim.ObservedStateKey{PeerID: o.WorkerID, Condition: sim.ObservedHelpedByWorker},
			snap.PublishedAt,
		)
		out = append(out, LaborOfferView{
			LaborID:              o.ID,
			Worker:               o.WorkerID,
			Employer:             o.EmployerID,
			EmployerInitiated:    o.EmployerInitiated(),
			Reward:               o.Reward,
			RewardItems:          o.RewardItems,
			MissingRewardItems:   missing,
			DurationMin:          o.DurationMin,
			ExpiresAt:            o.ExpiresAt,
			HelpedBeforeRecently: helpedBefore,
		})
	}
	return out
}

// buildLaboring scans snap.LaborLedger for a Working offer where the subject is
// the WORKER, returning the self-state view (employer + completion deadline) or
// nil if the subject isn't on a job (LLM-26). A worker carries at most one live
// job (AcceptWork forbids double-booking); the lowest LaborID is taken if more
// than one ever appears, for determinism.
func buildLaboring(snap *sim.Snapshot, subject sim.ActorID) *LaboringView {
	o := laboringOfferFor(snap, subject)
	if o == nil {
		return nil
	}
	v := &LaboringView{Employer: o.EmployerID, Until: *o.WorkingUntil}
	// Off-post surface (LLM-268): re-grant move_to + a directional cue when the
	// worker has wandered off the post, or her employer has left it. Resolved from
	// the same at-post definition (sim.ActorAtWorkpost) the world-side
	// return-to-post backstop uses, so the tool the gate advertises and the tick
	// that wakes her can't disagree on where the post is.
	worker := snap.Actors[subject]
	employer := snap.Actors[o.EmployerID]
	if worker == nil || employer == nil {
		return v
	}
	post := employer.WorkStructureID
	if post == "" {
		return v // an in-place hire (workless employer) — no post to be off
	}
	if label, ok := resolveStructureLabel(snap, post); ok {
		v.PostLabel = label
	}
	v.OffPost = !sim.ActorAtWorkpost(snap.VillageObjects, snap.Assets, worker.InsideStructureID, worker.Pos, post)
	v.EmployerAway = !sim.ActorAtWorkpost(snap.VillageObjects, snap.Assets, employer.InsideStructureID, employer.Pos, post)
	if v.EmployerAway {
		v.EmployerPlace = laboringActorPlaceLabel(snap, employer)
	}
	return v
}

// laboringActorPlaceLabel names where an actor currently is, for the accompany
// cue (LLM-268): the structure they're inside, else the named structure whose
// loiter pin they stand at, else "" (the cue then says they've stepped away
// without naming a destination). Used for the employer's whereabouts.
func laboringActorPlaceLabel(snap *sim.Snapshot, a *sim.ActorSnapshot) string {
	if a.InsideStructureID != "" {
		if label, ok := resolveStructureLabel(snap, a.InsideStructureID); ok && label != "" {
			return label
		}
	}
	if name, _ := findLoiterStructure(snap, a); name != "" {
		return name
	}
	return ""
}

// laboringOfferFor returns the live Working LaborOffer that workerID is fulfilling,
// or nil. Shared by buildLaboring (the subject's own self-state) and the co-present
// peer busy-annotation (LLM-231) so both read the ledger identically. A worker holds
// at most one live job (AcceptWork forbids double-booking); if a stale unswept offer
// ever coexists with a newer one, the latest WorkingUntil wins, then the lowest
// LaborID, for determinism. WorkingUntil is non-nil on the returned offer.
func laboringOfferFor(snap *sim.Snapshot, workerID sim.ActorID) *sim.LaborOffer {
	if snap == nil {
		return nil
	}
	// Delegate to the shared selector so this (which drives OffPost/EmployerAway
	// + the self-state cue) and the world-side return-to-post backstop pick the
	// same offer for a worker — same employer, same post, no drift (LLM-268).
	return sim.WorkerWorkingOffer(snap.LaborLedger, workerID)
}

// buildLaborEnRoute scans snap.LaborLedger for an EnRoute offer where the
// subject is the WORKER, returning the relocation self-state view (employer +
// whether they have arrived and are waiting for the owner) or nil if the subject
// isn't relocating to a job (LLM-229). A worker holds at most one live job
// (accept forbids double-booking); the lowest LaborID is taken if more than one
// ever appears, for determinism. Mirrors buildLaboring for the pre-work leg.
func buildLaborEnRoute(snap *sim.Snapshot, subject sim.ActorID) *LaborEnRouteView {
	if snap == nil || len(snap.LaborLedger) == 0 {
		return nil
	}
	var best *sim.LaborOffer
	for _, o := range snap.LaborLedger {
		if o == nil || o.State != sim.LaborStateEnRoute || o.WorkerID != subject {
			continue
		}
		if best == nil || o.ID < best.ID {
			best = o
		}
	}
	if best == nil {
		return nil
	}
	return &LaborEnRouteView{Employer: best.EmployerID, Waiting: best.EnRouteWaiting}
}

// buildWorkersForMe scans snap.LaborLedger for the Working offers where the
// subject is the EMPLOYER, projecting each to a WorkerForMeView for the
// "## Workers currently working for you" cue (LLM-202) — the employer-side
// mirror of buildLaboring. Unlike a worker (at most one live job), an employer
// can have several workers at once (John Ellis hired two), so this is a list,
// ordered by LaborID ascending for a stable render. Returns nil when no one is
// working for the subject. A Working offer with a nil WorkingUntil is skipped
// defensively — AcceptWork always sets it on the flip to Working.
func buildWorkersForMe(snap *sim.Snapshot, subject sim.ActorID) []WorkerForMeView {
	if snap == nil || len(snap.LaborLedger) == 0 {
		return nil
	}
	var ids []sim.LaborID
	for id, o := range snap.LaborLedger {
		if o == nil || o.State != sim.LaborStateWorking || o.EmployerID != subject || o.WorkingUntil == nil {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]WorkerForMeView, 0, len(ids))
	for _, id := range ids {
		o := snap.LaborLedger[id]
		out = append(out, WorkerForMeView{
			Worker:      o.WorkerID,
			Reward:      o.Reward,
			RewardItems: o.RewardItems,
			Until:       *o.WorkingUntil,
		})
	}
	return out
}

// buildPendingLaborOfferOut scans snap.LaborLedger for a Pending offer the
// subject MINTED, returning the initiator-side self-state view (counterparty +
// offered terms) or nil if the subject has no outgoing offer (LLM-164). Covers
// both mints: a worker who solicited, and an employer who offered work (LLM-346).
// The mirror of buildLaboring for the awaiting-answer state. The
// one-pending-offer-out gate (SolicitWork / OfferWork) means at most one exists;
// lowest LaborID wins for determinism if more ever appear.
func buildPendingLaborOfferOut(snap *sim.Snapshot, subject sim.ActorID) *PendingLaborOfferOutView {
	if snap == nil || len(snap.LaborLedger) == 0 {
		return nil
	}
	var best *sim.LaborOffer
	for _, o := range snap.LaborLedger {
		if o == nil || o.State != sim.LaborStatePending || o.Initiator() != subject {
			continue
		}
		if best == nil || o.ID < best.ID {
			best = o
		}
	}
	if best == nil {
		return nil
	}
	return &PendingLaborOfferOutView{
		Employer:          best.EmployerID,
		Worker:            best.WorkerID,
		EmployerInitiated: best.EmployerInitiated(),
		Reward:            best.Reward,
		RewardItems:       best.RewardItems,
		DurationMin:       best.DurationMin,
	}
}

// laborOfferLivePending reports whether o is a pending offer that has not yet run
// out its TTL as of the snapshot instant — the perception mirror of the substrate's
// `!now.Before(o.ExpiresAt)` skip (workerPendingLaborOffer, activeLaborBetween).
//
// The two must agree or the affordances drift (code_review). A pending offer sits
// in the ledger for up to a full sweep cadence (60s) after its 3-minute TTL
// elapses, because only the aging sweep flips it Expired. Suppressing on the bare
// `State == Pending` therefore hides solicit_work / offer_work for as much as a
// minute against an offer the substrate would cheerfully mint past — a keeper
// watching a hireable worker vanish from her prompt for no reason she can see.
// Judged against snap.PublishedAt, so every actor perceiving one snapshot agrees.
//
// A zero instant means a clock-free fixture (hand-built snapshots, the golden
// matrix); those rows are treated as live, matching how the ledger reads a
// zero ExpiresAt.
func laborOfferLivePending(o *sim.LaborOffer, at time.Time) bool {
	if o == nil || o.State != sim.LaborStatePending {
		return false
	}
	if at.IsZero() || o.ExpiresAt.IsZero() {
		return true
	}
	return at.Before(o.ExpiresAt)
}

// subjectHasPendingLaborOfferOut reports whether the subject MINTED a live pending
// labor offer that is still awaiting the other party's answer — the perception
// mirror of SolicitWork's / OfferWork's one-pending-offer-out gate, so the
// affordance cue and the tool both hide while an offer is outstanding
// (code_review). LLM-26, LLM-346.
func subjectHasPendingLaborOfferOut(snap *sim.Snapshot, subject sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, o := range snap.LaborLedger {
		if laborOfferLivePending(o, snap.PublishedAt) && o.Initiator() == subject {
			return true
		}
	}
	return false
}

// subjectHasLaborOfferToAnswer reports whether a live pending labor offer awaits
// the subject's accept_work / decline_work (LLM-346). Suppresses the solicit_work
// and offer_work affordances: an actor holding an unanswered offer should answer
// it, not open a second bargain — and a worker who has just been ASKED to lend a
// hand must not be told to go and ask for work. The perception mirror of the
// substrate's duplicate-offer gates, which reject the second mint outright.
func subjectHasLaborOfferToAnswer(snap *sim.Snapshot, subject sim.ActorID) bool {
	if snap == nil {
		return false
	}
	for _, o := range snap.LaborLedger {
		if laborOfferLivePending(o, snap.PublishedAt) && o.Responder() == subject {
			return true
		}
	}
	return false
}

// subjectIsWorker reports whether the subject carries the AttrWorker marker,
// read from the snapshot's sorted AttributeSlugs projection (LLM-26).
func subjectIsWorker(actorSnap *sim.ActorSnapshot) bool {
	if actorSnap == nil {
		return false
	}
	for _, slug := range actorSnap.AttributeSlugs {
		if slug == sim.AttrWorker {
			return true
		}
	}
	return false
}

// subjectHasResolvableWorkplace reports whether the actor's WorkStructureID names a
// structure (or shared village_object) PRESENT in the snapshot — the exact resolution
// buildAnchors applies to anchors.WorkID. The seek-work directory keys on this, not
// the raw field, so it agrees with the duty steer: a worker is "workless" (and seeks
// work) precisely when it has no usable post to be steered to. A set-but-dangling
// WorkStructureID (no matching structure) reads as workless, so a worker the duty
// steer can't route to its post still gets the seek-work directory rather than
// falling into a dead zone where neither cue fires (LLM-168).
func subjectHasResolvableWorkplace(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot) bool {
	if actorSnap == nil {
		return false
	}
	_, ok := resolveStructureLabel(snap, actorSnap.WorkStructureID)
	return ok
}

// SeekWorkPlace is one business in the seek-work directory: a structure name a
// broke worker can move_to, tagged with how far it is and in which direction so
// the worker heads to a near, open shop instead of trekking to a far-flung farm
// (LLM-155). Name stays the bare move_to-by-name token — no structure_id. sortKey
// is the raw tile distance, kept only to order nearest-first.
type SeekWorkPlace struct {
	Name      string
	Distance  string // qualitativeDistance phrase, e.g. "a short walk"
	Direction string // 8-point compass bearing; empty when coincident
	// Visited marks a business the worker called at recently (a live
	// ObservedSeekWorkVisited memory, LLM-563). Rendered as a "you called there
	// not long ago" aside, and the ordering ranks these after untried businesses.
	Visited   bool
	visitedAt time.Time // stamp time of the visited memory; orders visited entries least-recent first
	sortKey   float64
	sourceID  sim.VillageObjectID // tie-break for the representative; never rendered
}

// buildSeekWorkPlaces lists the town's businesses as move_to destinations for a
// worker nudged to seek work (LLM-152). Businesses are village objects tagged
// sim.TagBusiness; each shares its id with the co-located structure (the identity
// bridge), so resolveStructureLabel yields the clean structure name the worker
// navigates to by name (LLM-142); falls back to the object's own DisplayName if no
// structure resolves.
//
// Each entry carries a qualitative distance + direction from the actor (LLM-155),
// derived in tile space exactly like the eat/drink free-source cue (actor Pos is a
// padded tile; obj.Pos is world pixels, converted via Tile()). The list is ordered
// nearest-first so a weak model favours a close shop, and a business the worker
// recently found shut (earned ObservedClosed memory, 4h TTL) is DROPPED — sending
// them back to a closed door wastes the trip, and the entry reappears once the
// memory decays. A business whose keeper recently DECLINED this worker's labor
// offer (earned ObservedDeclinedWork memory, 12h TTL) is dropped the same way
// (LLM-198), so the worker tries the next-nearest business instead of walking
// back to a refusal. A business whose keeper the worker last found on break
// (earned ObservedNoHiring memory) is dropped the same way (LLM-210) — a resting
// keeper is "open" but cannot take on a worker, so routing back there just loops.
// A business the worker merely CALLED AT recently (earned ObservedSeekWorkVisited
// memory, 2h TTL — LLM-563) is NOT dropped but ranked after every untried one,
// least-recently-visited first: "I was just there" is far weaker evidence than a
// refusal, and dropping on it would empty the directory after one tour of town
// while the seek-work impulse kept nagging with no destination to offer.
// De-duped by name, keeping the nearest representative.
func buildSeekWorkPlaces(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot) []SeekWorkPlace {
	if snap == nil || actorSnap == nil {
		return nil
	}
	ax := float64(actorSnap.Pos.X)
	ay := float64(actorSnap.Pos.Y)
	best := make(map[string]SeekWorkPlace)
	for _, obj := range snap.VillageObjects {
		if obj == nil || !obj.HasTag(sim.TagBusiness) {
			continue
		}
		structureID := sim.StructureID(obj.ID)
		if businessRememberedShut(snap, actorSnap, structureID) {
			continue
		}
		if workerRememberedDeclinedWork(snap, actorSnap, structureID) {
			continue
		}
		if workerRememberedNoHiring(snap, actorSnap, structureID) {
			continue
		}
		label, ok := resolveStructureLabel(snap, structureID)
		if !ok || label == "" {
			label = obj.DisplayName
		}
		if label == "" {
			continue
		}
		visitedAt, visited := workerRememberedVisited(snap, actorSnap, structureID)
		objTile := obj.Pos.Tile()
		tx := float64(objTile.X)
		ty := float64(objTile.Y)
		dx := tx - ax
		dy := ty - ay
		distTiles := math.Sqrt(dx*dx + dy*dy)
		candidate := SeekWorkPlace{
			Name:      label,
			Distance:  qualitativeDistance(distTiles),
			Direction: cardinalDirection(ax, ay, tx, ty),
			Visited:   visited,
			visitedAt: visitedAt,
			sortKey:   distTiles,
			sourceID:  obj.ID,
		}
		// A name can resolve from more than one co-located business object; with
		// nearest-first ordering, keep the closest instance — and on an exact
		// distance tie, the lowest object id — so the representative (and its
		// rendered direction) stays deterministic despite unordered map iteration.
		if existing, seen := best[label]; seen {
			if existing.sortKey < candidate.sortKey {
				continue
			}
			if existing.sortKey == candidate.sortKey && existing.sourceID <= candidate.sourceID {
				continue
			}
		}
		best[label] = candidate
	}
	if len(best) == 0 {
		return nil
	}
	places := make([]SeekWorkPlace, 0, len(best))
	for _, p := range best {
		places = append(places, p)
	}
	// Untried businesses first, nearest-first among them; a business the worker
	// called at recently (a live visited memory, LLM-563) sorts after every
	// untried one, least-recently-visited first, so the directory converges on
	// doors not yet knocked instead of the one just left. Ties broken by
	// distance then name for a deterministic order (map iteration is unordered);
	// the untried half mirrors the eat/drink free-source ordering.
	sort.Slice(places, func(i, j int) bool {
		if places[i].Visited != places[j].Visited {
			return !places[i].Visited
		}
		if places[i].Visited && !places[i].visitedAt.Equal(places[j].visitedAt) {
			return places[i].visitedAt.Before(places[j].visitedAt)
		}
		if places[i].sortKey != places[j].sortKey {
			return places[i].sortKey < places[j].sortKey
		}
		return places[i].Name < places[j].Name
	})
	return places
}

// workerRememberedDeclinedWork reports whether the subject has an earned
// experiential memory (LLM-198) of soliciting work at structureID and being
// declined, still within its TTL of the snapshot clock. buildSeekWorkPlaces uses
// it to DROP that business from the worker's directory so it stops walking back
// to an employer who just refused it — the seek-work sibling of
// businessRememberedShut. The memory is stamped by the LaborResolved subscriber
// (sim/declined_work.go); the TTL decay is applied by Observed.Active at read
// time so a stale refusal fades (the worker retries) without the world goroutine
// sweeping the store. False when the subject has no such memory, the snapshot has
// no clock baseline, or the memory has expired.
func workerRememberedDeclinedWork(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, structureID sim.StructureID) bool {
	if snap == nil || actorSnap == nil {
		return false
	}
	return actorSnap.Observed.Active(sim.ObservedStateKey{StructureID: structureID, Condition: sim.ObservedDeclinedWork}, snap.PublishedAt)
}

// workerRememberedNoHiring reports whether the subject has an earned experiential
// memory (LLM-210) of arriving at structureID and finding its keeper present but on
// break — not hireable — still within its TTL of the snapshot clock.
// buildSeekWorkPlaces uses it to DROP that business from the worker's directory so it
// stops routing back to a shop whose keeper cannot take it on right now, the
// resting-keeper sibling of workerRememberedDeclinedWork / businessRememberedShut. The
// memory is stamped by the NoHiring arrival subscriber (sim/no_hiring.go); the TTL
// decay is applied by Observed.Active at read time so a stale belief fades (the worker
// retries) without a world-goroutine sweep. False when the subject has no such memory,
// the snapshot has no clock baseline, or the memory has expired.
func workerRememberedNoHiring(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, structureID sim.StructureID) bool {
	if snap == nil || actorSnap == nil {
		return false
	}
	return actorSnap.Observed.Active(sim.ObservedStateKey{StructureID: structureID, Condition: sim.ObservedNoHiring}, snap.PublishedAt)
}

// workerRememberedVisited reports whether the subject has an earned experiential
// memory (LLM-563) of having called at structureID looking for work, still within
// its TTL of the snapshot clock — and, when it does, the time of that visit.
// Unlike its three DROP siblings above, buildSeekWorkPlaces uses this only for
// ORDERING: a visited business ranks after every untried one (least-recently-
// visited first) so the directory converges on doors not yet knocked, without
// ever emptying — a visit where nothing happened is far weaker evidence than a
// refusal. The memory is stamped by the visited arrival subscriber
// (sim/seek_work_visited.go); the TTL decay is applied by Observed.Active at read
// time so the business regains its normal nearest-first standing without a
// world-goroutine sweep. False when the subject has no such memory, the snapshot
// has no clock baseline, or the memory has expired.
func workerRememberedVisited(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, structureID sim.StructureID) (time.Time, bool) {
	if snap == nil || actorSnap == nil {
		return time.Time{}, false
	}
	key := sim.ObservedStateKey{StructureID: structureID, Condition: sim.ObservedSeekWorkVisited}
	if !actorSnap.Observed.Active(key, snap.PublishedAt) {
		return time.Time{}, false
	}
	visitedAt, _ := actorSnap.Observed.At(key)
	return visitedAt, true
}

// subjectIsComfortable reports whether the subject worker holds enough coin to stop
// hustling for work (LLM-194): coins at or above the effective seek-work ceiling. The
// perception-side mirror of sim.workerIsComfortable — both reduce to coins >= ceiling,
// and the ceiling magnitude lives in one place (sim.SeekWorkCoinCeilingDefault), so the
// warrant gate and the directory/affordance gates can't disagree. The ceiling is read
// off the snapshot (snap.SeekWorkCoinCeiling, the effective value copied at publish); a
// 0 means the snapshot was built directly in a test without going through publish, so
// it resolves to the default (test snapshots that omit it keep the pre-ceiling
// always-seek behavior below the default).
func subjectIsComfortable(snap *sim.Snapshot, subject *sim.ActorSnapshot) bool {
	if snap == nil || subject == nil {
		return false
	}
	ceiling := snap.SeekWorkCoinCeiling
	if ceiling <= 0 {
		ceiling = sim.SeekWorkCoinCeilingDefault
	}
	return subject.Coins >= ceiling
}

// hasSolicitableAudience reports whether at least one awake, addressable actor in
// the subject's audience (huddle peers ∪ co-present) is someone the subject could
// actually solicit work from — i.e. NOT a member of its own household (same home
// structure) or its own workplace crew (same work structure), AND not an employer
// who has already declined this worker (LLM-181). It is the narrowed successor to
// SurroundingsView.HasAudience() in the CanSolicitWork gate: a broke worker shut in
// with only family present HAS an audience but no one worth bidding (LLM-145).
// CoPresentAsleep / CoPresentResting are already partitioned out upstream —
// HuddleMembers and CoPresent are the same addressable set HasAudience reads, so
// this can't advertise solicit_work against someone the speak path couldn't reach.
// subjectID identifies the worker so the decline check can match the ledger's
// WorkerID (ActorSnapshot carries no self id).
func hasSolicitableAudience(snap *sim.Snapshot, subjectID sim.ActorID, subject *sim.ActorSnapshot, surr SurroundingsView) bool {
	if snap == nil || subject == nil {
		return false
	}
	for _, m := range surr.HuddleMembers {
		if isSolicitableEmployer(snap, subjectID, subject, m.ID) {
			return true
		}
	}
	for _, m := range surr.CoPresent {
		if isSolicitableEmployer(snap, subjectID, subject, m.ID) {
			return true
		}
	}
	return false
}

// buildSolicitableEmployers lists the co-present actors the subject could offer
// its labor to right now AND knows by name — the worker-side naming mirror of
// buildHireableWorkers (LLM-564). The solicit cue names these people so the ask
// is grounded in the room ("Hannah Boggs might have work that wants doing")
// rather than a standing conditional the model must first invent a counterfactual
// for — measured live, the unnamed cue converted 26 of 1,427 renders in a week.
//
// This slice is PRESENTATION ONLY: the solicit gate stays CanSolicitWork, which
// accepts an unacquainted audience too (isSolicitableEmployer carries no
// acquaintance clause — a worker can bid a stranger it has spoken with). The
// acquaintance filter here exists for isHireableWorker's reason: solicit_work
// resolves its employer by exact display name, and an unacquainted peer renders
// as a descriptor ("the shopkeeper"), so naming them would hand the model a
// target the tool must then refuse. When every solicitable peer is a stranger
// the cue falls back to its unnamed wording; the tool stays advertised either way.
func buildSolicitableEmployers(snap *sim.Snapshot, subjectID sim.ActorID, subject *sim.ActorSnapshot, surr SurroundingsView) []sim.ActorID {
	if snap == nil || subject == nil {
		return nil
	}
	seen := make(map[sim.ActorID]struct{})
	var out []sim.ActorID
	for _, group := range [][]HuddleMember{surr.HuddleMembers, surr.CoPresent} {
		for _, m := range group {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			if !isSolicitableEmployer(snap, subjectID, subject, m.ID) {
				continue
			}
			other := snap.Actors[m.ID]
			if other == nil || other.DisplayName == "" {
				continue
			}
			if _, acquainted := subject.Acquaintances[other.DisplayName]; !acquainted {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return snap.Actors[out[i]].DisplayName < snap.Actors[out[j]].DisplayName
	})
	return out
}

// isSolicitableEmployer reports whether candidate (by id) is a co-present actor
// the subject could solicit — present in the snapshot, sharing neither the
// subject's household nor its workplace (LLM-145), and not already on record as
// having declined this worker (LLM-181).
func isSolicitableEmployer(snap *sim.Snapshot, subjectID sim.ActorID, subject *sim.ActorSnapshot, candidate sim.ActorID) bool {
	other := snap.Actors[candidate]
	if other == nil {
		return false
	}
	if employerDeclinedSubject(snap, subjectID, candidate) {
		return false
	}
	return !sharesHousehold(subject, other) && !sharesWorkplace(subject, other)
}

// buildHireableWorkers lists the co-present actors the subject could offer an odd
// job to right now — the hiring-side mirror of hasSolicitableAudience (LLM-346).
// A non-empty result both names the workers in the offer_work cue and advertises
// the tool, so the cue and the tool read one signal (discussion-109).
//
// The audience is the same addressable set the speak gate reads (huddle peers ∪
// co-present), because offer_work needs a huddle peer at the substrate and the
// tool's huddle bootstrap forms one from a co-present walk-in. Sorted by display
// name so the prompt is stable across ticks.
func buildHireableWorkers(snap *sim.Snapshot, subjectID sim.ActorID, subject *sim.ActorSnapshot, surr SurroundingsView) []sim.ActorID {
	if snap == nil || subject == nil {
		return nil
	}
	seen := make(map[sim.ActorID]struct{})
	var out []sim.ActorID
	for _, group := range [][]HuddleMember{surr.HuddleMembers, surr.CoPresent} {
		for _, m := range group {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			if !isHireableWorker(snap, subjectID, subject, m.ID) {
				continue
			}
			seen[m.ID] = struct{}{}
			out = append(out, m.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return snap.Actors[out[i]].DisplayName < snap.Actors[out[j]].DisplayName
	})
	return out
}

// isHireableWorker reports whether candidate is someone the subject could take on
// for an odd job: present in the snapshot, known to the subject BY NAME, carrying
// the AttrWorker marker, not the subject itself, sharing neither the subject's
// household nor its workplace (LLM-145's gate, mirrored), free of any live job or
// unanswered offer, and not on record as having just refused this employer (the
// LLM-181 mirror — an employer who re-offers to the worker who declined them loops
// the same refusal).
//
// Every clause here has a substrate counterpart in sim.OfferWork, so a named worker
// is one the tool will actually accept — with one clause that has no substrate twin
// and belongs to the cue alone: acquaintance. An unacquainted peer renders as
// "the laborer" or "a stranger" (descriptorLabel), and offer_work resolves its
// target by exact DisplayName among huddle peers. Naming a descriptor would hand
// the keeper a target the tool must then refuse. So a keeper hires people she
// knows; the rest she must speak to first, which is how she comes to know them.
func isHireableWorker(snap *sim.Snapshot, subjectID sim.ActorID, subject *sim.ActorSnapshot, candidate sim.ActorID) bool {
	if candidate == subjectID {
		return false
	}
	other := snap.Actors[candidate]
	if other == nil || !subjectIsWorker(other) {
		return false
	}
	if other.DisplayName == "" {
		return false
	}
	if _, acquainted := subject.Acquaintances[other.DisplayName]; !acquainted {
		return false
	}
	if sharesHousehold(subject, other) || sharesWorkplace(subject, other) {
		return false
	}
	if workerDeclinedSubject(snap, subjectID, candidate) {
		return false
	}
	for _, o := range snap.LaborLedger {
		if o == nil || o.WorkerID != candidate {
			continue
		}
		switch o.State {
		case sim.LaborStatePending:
			// Only a LIVE pending offer occupies the worker. A row past its TTL is
			// dead — the sweep just hasn't flipped it yet — and sim.OfferWork would
			// mint straight past it, so hiding the worker here would drift the cue
			// from the tool for up to a sweep cadence (code_review).
			if laborOfferLivePending(o, snap.PublishedAt) {
				return false
			}
		case sim.LaborStateEnRoute, sim.LaborStateWorking:
			return false // already committed to a job
		}
	}
	return true
}

// workerDeclinedSubject reports whether the candidate worker's MOST RECENT
// EMPLOYER-INITIATED offer from this employer ended in a decline (LLM-346) — the
// hiring-side mirror of employerDeclinedSubject. An employer whose offer of work
// was just refused must stop counting that worker as hireable, or the cue names
// them again next tick and the keeper re-offers into the same refusal.
//
// Scoped to employer-initiated offers: a worker who SOLICITED and was declined is
// still perfectly hireable by that employer later on better terms — that decline
// was the employer's own, and holding it against the worker would foreclose a
// hire the employer themselves refused. Resolves by the latest offer for the pair
// (LaborID is minted monotonically) and suppresses only when that offer is a
// declined offer_work. The suppression ages out with the ledger reaper
// (LaborLedgerTerminalRetentionDefault, 1h).
//
// The 1h window is deliberate and symmetric, though it can look otherwise
// (code_review). There are TWO refusal memories in the labor system, and only one
// of them has a hiring-side counterpart:
//
//   - The CO-PRESENT audience suppression — "don't re-ask the person standing in
//     front of you who just said no." That is employerDeclinedSubject on the
//     solicit side and this function on the hire side. Both are pure ledger scans,
//     both last exactly as long as the declined row does (1h). Symmetric.
//   - The DIRECTORY suppression — the worker's 12h ObservedDeclinedWork memory
//     (LLM-198), which drops a shop from buildSeekWorkPlaces so the worker doesn't
//     walk back across town to a closed door. There is no hiring-side counterpart
//     because an employer never travels to find workers; she hires whoever is in
//     the room. Nothing to drop from a directory she does not have.
//
// So a 12h employer-side observed state would suppress nothing the 1h ledger scan
// doesn't already cover, and would keep a keeper from re-asking a worker whose
// circumstances changed the same afternoon.
func workerDeclinedSubject(snap *sim.Snapshot, employerID, candidate sim.ActorID) bool {
	var latest *sim.LaborOffer
	for _, o := range snap.LaborLedger {
		if o == nil || o.WorkerID != candidate || o.EmployerID != employerID || !o.EmployerInitiated() {
			continue
		}
		if latest == nil || o.ID > latest.ID {
			latest = o
		}
	}
	return latest != nil && latest.State == sim.LaborStateDeclined
}

// employerDeclinedSubject reports whether the candidate employer's MOST RECENT labor
// offer from this worker ended in a decline. A worker who solicited an employer and
// was turned down must stop counting that employer as a live prospect: the decline IS
// the engine's "no one here can hire you" memory, and treating the refuser as
// still-solicitable is exactly what suppressed the seek-work directive and trapped a
// worker re-soliciting the same refusal tick after tick (LLM-181 — Lewis Walker at
// the General Store). Dropping the declined employer from the audience re-arms
// SeekWorkPlaces, so the worker is steered to a business instead.
//
// Resolves by the LATEST offer for the (worker, employer) pair — LaborID is minted
// monotonically (nextLaborSeq), so the highest id is the most recent — and suppresses
// only when that latest offer is Declined. A newer pending re-ask or a later completed
// job therefore un-suppresses even if an older declined offer for the same pair is
// still lingering in the ledger (code_review). Scoped to LaborStateDeclined: an
// Expired (no answer) or FailedUnavailable offer is a different signal, left for a
// follow-up. The suppression ages out only when the ledger reaper removes the declined
// offer (LaborLedgerTerminalRetentionDefault, 1h) — walking away does not clear it.
// Pure ledger scan, mirroring subjectHasPendingLaborOfferOut.
//
// EMPLOYER-INITIATED offers are skipped (LLM-346): a decline is the responder's
// refusal, so on an offer_work it was the WORKER who said no. Counting it here
// would have the worker treat an employer who tried to hire them as one who turned
// them away — and stop them soliciting the very keeper who wanted their help.
func employerDeclinedSubject(snap *sim.Snapshot, subjectID, candidate sim.ActorID) bool {
	var latest *sim.LaborOffer
	for _, o := range snap.LaborLedger {
		if o == nil || o.WorkerID != subjectID || o.EmployerID != candidate || o.EmployerInitiated() {
			continue
		}
		if latest == nil || o.ID > latest.ID {
			latest = o
		}
	}
	return latest != nil && latest.State == sim.LaborStateDeclined
}

// sharesHousehold reports whether a and b live in the same (non-empty) home
// structure. An empty HomeStructureID never matches — a homeless actor (Ezekiel)
// shares a household with no one. LLM-145.
func sharesHousehold(a, b *sim.ActorSnapshot) bool {
	return a.HomeStructureID != "" && a.HomeStructureID == b.HomeStructureID
}

// sharesWorkplace reports whether a and b are anchored to the same (non-empty)
// work structure — the same employer's crew, who wouldn't take each other on for
// pay. An empty WorkStructureID never matches. LLM-145.
func sharesWorkplace(a, b *sim.ActorSnapshot) bool {
	return a.WorkStructureID != "" && a.WorkStructureID == b.WorkStructureID
}

// buildRoomAlreadySold maps each pending lodging offer (by its LedgerID) to an
// existing Ready lodging order this keeper already owes the SAME buyer — the
// duplicate-room situation LLM-89's AcceptPay gate rejects (a nights_stay grant
// lands only at deliver_order, so accepting a second room before handing over
// the first double-charges the guest). renderPayOffers reads it to steer the
// keeper to deliver the room already sold rather than accept another. nil when
// no pending offer overlaps an undelivered room.
func buildRoomAlreadySold(snap *sim.Snapshot, keeper sim.ActorID, offers []sim.PayOfferWarrantReason) map[sim.LedgerID]sim.OrderID {
	if snap == nil || len(offers) == 0 || len(snap.Orders) == 0 {
		return nil
	}
	var out map[sim.LedgerID]sim.OrderID
	for _, o := range offers {
		if !itemGrantsLodging(snap, o.Item) {
			continue
		}
		oid, ok := readyLodgingOrderFor(snap, keeper, o.Buyer)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[sim.LedgerID]sim.OrderID)
		}
		out[o.LedgerID] = oid
	}
	return out
}

// readyLodgingOrderFor returns the ID of a Ready (undelivered) lodging order
// from keeper to buyer, and true, or (0, false) when none. The seller-side
// mirror of the engine's undeliveredLodgingOrderFor, read off the snapshot;
// buyer matches as the order's BuyerID or any of its ConsumerIDs.
func readyLodgingOrderFor(snap *sim.Snapshot, keeper, buyer sim.ActorID) (sim.OrderID, bool) {
	for _, o := range snap.Orders {
		if o == nil || o.State != sim.OrderStateReady || o.SellerID != keeper {
			continue
		}
		if !itemGrantsLodging(snap, o.Item) {
			continue
		}
		if o.BuyerID == buyer {
			return o.ID, true
		}
		for _, cid := range o.ConsumerIDs {
			if cid == buyer {
				return o.ID, true
			}
		}
	}
	return 0, false
}

// filterStalePayOfferWarrants removes PayOfferWarrantReason warrants whose
// pay-ledger entry is missing or no longer pending (ZBBS-HOME-413). See the
// callsite in Build for the why. All other warrant kinds pass through
// untouched, and the input slice is returned unchanged (same backing array)
// when nothing is stale — the common case, so the steady state allocates
// nothing.
func filterStalePayOfferWarrants(warrants []sim.WarrantMeta, snap *sim.Snapshot) []sim.WarrantMeta {
	if len(warrants) == 0 || snap == nil {
		return warrants
	}
	stale := func(w sim.WarrantMeta) bool {
		r, ok := w.Reason.(sim.PayOfferWarrantReason)
		if !ok {
			return false
		}
		e := snap.PayLedger[r.LedgerID]
		return e == nil || e.State != sim.PayLedgerStatePending
	}
	anyStale := false
	for _, w := range warrants {
		if stale(w) {
			anyStale = true
			break
		}
	}
	if !anyStale {
		return warrants
	}
	out := make([]sim.WarrantMeta, 0, len(warrants))
	for _, w := range warrants {
		if !stale(w) {
			out = append(out, w)
		}
	}
	return out
}

// filterHomedLodgingQuoteWarrants drops a scene-quote warrant advertising a
// lodging (nights_stay) room to a subject who already has a home. Such a buyer
// can't take the room — the buyer-side pay_with_item guard rejects it (LLM-182)
// — so surfacing the offer only dangles a doomed nightly negotiation (LLM-208).
// Per-viewer: the check is against THIS subject's HomeStructureID, so a homeless
// seeker in the same scene still perceives a public room quote — only a homed
// subject is spared it. Covers a public/overheard quote the seller-side creation
// gate (scene_quote_commands.go) can't pre-check per-buyer, and backstops the
// targeted path. Pure over the snapshot; reuses itemGrantsLodging (lodging.go).
func filterHomedLodgingQuoteWarrants(warrants []sim.WarrantMeta, snap *sim.Snapshot, subjectSnap *sim.ActorSnapshot) []sim.WarrantMeta {
	if len(warrants) == 0 || snap == nil || subjectSnap == nil || subjectSnap.HomeStructureID == "" {
		return warrants
	}
	suppress := func(w sim.WarrantMeta) bool {
		r, ok := w.Reason.(sim.SceneQuoteTargetedWarrantReason)
		if !ok {
			return false
		}
		for _, ln := range r.Lines {
			if itemGrantsLodging(snap, ln.ItemKind) {
				return true
			}
		}
		return false
	}
	anySuppressed := false
	for _, w := range warrants {
		if suppress(w) {
			anySuppressed = true
			break
		}
	}
	if !anySuppressed {
		return warrants
	}
	out := make([]sim.WarrantMeta, 0, len(warrants))
	for _, w := range warrants {
		if !suppress(w) {
			out = append(out, w)
		}
	}
	return out
}

// recentFactsMostRecentFirst returns up to n facts from the tail of
// the oldest-first stored slice, reversed so the most-recent is first.
// Returns nil for an empty input.
func recentFactsMostRecentFirst(facts []sim.SalientFact, n int) []sim.SalientFact {
	if len(facts) == 0 || n <= 0 {
		return nil
	}
	start := len(facts) - n
	if start < 0 {
		start = 0
	}
	tail := facts[start:]
	out := make([]sim.SalientFact, len(tail))
	for i, f := range tail {
		out[len(tail)-1-i] = f
	}
	return out
}
