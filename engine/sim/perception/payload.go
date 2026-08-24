package perception

import (
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// BaselineStatus reports whether perception could establish a diff baseline
// for the subject actor against the primary scene's origin snapshot.
//
// The contract — see Build — is "unknown, never no-change": any Missing*
// status means perception MUST NOT claim "nothing changed since the scene
// started" and loop detection is inconclusive (not negative). A stuck-loop
// signal requires BaselinePresent; absence of evidence is not evidence of
// a loop.
type BaselineStatus int

const (
	// BaselineMissingNoScene — no scene resolved at all. Neither the
	// consumed warrants nor the actor's active huddle pointed at a scene
	// present in the snapshot. There is nothing to diff against.
	//
	// This is deliberately the zero value: a zero-value Payload is then
	// honestly degraded (no baseline) rather than falsely "present", so
	// Render(Payload{}) and any other unset path stays on the safe side of
	// the "unknown, never no-change" contract.
	BaselineMissingNoScene BaselineStatus = iota

	// BaselinePresent — the primary scene resolved and captured an origin
	// snapshot for the subject actor. SceneView.Diff is populated.
	BaselinePresent

	// BaselineMissingNoOriginSnapshot — a scene resolved but it captured no
	// participant baseline at all (ParticipantStateAtOrigin nil/empty —
	// e.g. an unbounded atmosphere-refresh scene). No actor has a baseline
	// here, so the absence carries no "joined after" signal.
	BaselineMissingNoOriginSnapshot

	// BaselineMissingJoinedAfterOrigin — a scene resolved and captured a
	// baseline for *other* participants, but not for the subject actor —
	// so the actor joined after the scene was minted. The diff baseline
	// would be meaningless; continuity claims about "since the scene
	// started" are weakened or omitted.
	BaselineMissingJoinedAfterOrigin
)

// String renders the status as a stable lowercase label — used in
// SelectionReason text, debug output, and telemetry Detail.
func (s BaselineStatus) String() string {
	switch s {
	case BaselinePresent:
		return "present"
	case BaselineMissingNoScene:
		return "missing_no_scene"
	case BaselineMissingNoOriginSnapshot:
		return "missing_no_origin_snapshot"
	case BaselineMissingJoinedAfterOrigin:
		return "missing_joined_after_origin"
	default:
		return "unknown"
	}
}

// Payload is the immutable result of Build — everything Render needs to
// produce a prompt, derived purely from a published *sim.Snapshot. It is
// "immutable" by convention (built once, never mutated), the same way
// sim.Snapshot is.
type Payload struct {
	ActorID sim.ActorID

	// SelfTraveler carries the subject's OWN transient-traveler persona, non-nil
	// only when the subject is a salem-visitor (VisitorState set on its snapshot).
	// Render opens the user_message with a prose self-identity preface built from
	// it, so the stateless salem-visitor VA speaks in-character as this specific
	// traveler. Nil for every persistent NPC / PC (the steady-state case). LLM-370.
	SelfTraveler *TravelerSelfView

	// TravelerRounds / TravelerSeekBed are the traveler's day-plan cues (LLM-373):
	// the daytime "## On your rounds" circuit framing, and the evening "## A bed for
	// the night" booking cue at the inn. Both non-nil only for a salem-visitor in the
	// matching phase; see traveler_dayplan.go.
	TravelerRounds  *TravelerRoundsView
	TravelerSeekBed *TravelerSeekBedView

	// ErrandVisit is the counterparty keeper's "## A trader's come to deal" cue (LLM-455) when
	// a merchant visitor whose errand is with THIS keeper's shop is co-present — buy or sell.
	// nil otherwise; see traveler_dayplan.go. (Generalizes the LLM-410 factor-visit cue; the
	// factor's own trade steer is now folded into the errand-aware TravelerRounds surface.)
	ErrandVisit *ErrandVisitView

	// VisitorCommerceStripped is true when the subject is a visitor whose commerce tools should
	// be withheld this tick (LLM-455) — the talk-only-rounds gate: a visitor not co-present with
	// his errand counterparty or a tavern/inn keeper trades nowhere. Read by handlers/tool_gating.go.
	VisitorCommerceStripped bool

	// Actor is the subject actor's own current decision-relevant state.
	Actor ActorView

	// Surroundings is where the actor is right now — structure, huddle,
	// and co-present actors.
	Surroundings SurroundingsView

	// TurnState is the subject's conversation turn-state for this tick
	// (ZBBS-WORK-370): which present huddle peers it is awaiting a reply from,
	// and which are awaiting a reply from it. Drives the turn-line and the
	// act-now coda swap that stop an NPC re-pitching a peer who hasn't answered.
	// Zero value (both slices empty) when the actor has no pending turn.
	TurnState TurnStateView

	// Anchors are the actor's OWN home and work structures, always surfaced
	// (when set) as standing move_to targets with their structure_ids — so a
	// wandering NPC can always head home or to work, not only when a need-cue
	// happens to point somewhere. nil for an actor with neither anchor (e.g. a
	// PC, or an unanchored NPC). ZBBS-HOME-349.
	Anchors *AnchorsView

	// DutySteer is the standing return-to-post cue (ZBBS-HOME-352): non-nil when
	// the subject is an agent NPC that, by its schedule (or the dawn/dusk
	// fallback) and current position, is on-shift away from work or off-shift
	// away from home. The always-present, level-triggered perception voice for
	// shift duty — the engine's edge-stamped ShiftDutyWarrant drives the wake
	// tick but its rendered line is filtered, so this is the single voice. NOT
	// need-suppressed (the model weighs duty against need). nil when at-post, off
	// scope, or the clock/anchors are unknown. See buildDutySteer.
	DutySteer *DutySteerView

	// DutyPending reports that the subject is off-post inside its shift window,
	// computed WITHOUT the cue-side suppressors that can nil DutySteer (the
	// HOME-362 red-need gate; HOME-400 Option B's mild-need / restock-errand /
	// pending-offer gate). It answers "does to-work duty APPLY this minute",
	// not "should the cue RENDER" — the noop-skip gate consumes it
	// (ZBBS-HOME-442) so an off-post keeper whose steer is suppressed by a
	// mild need still gets the tick that lets it address the need. Never
	// rendered. True whenever DutySteer is a to-work steer, and also through
	// the suppressed band. See buildDutyPending.
	DutyPending bool

	// EveningLeisure is the evening "tavern's open" cue (LLM-149, Lever 2 of the
	// living-evening epic LLM-147): a non-coercive invitation shown to a homed,
	// day-shift agent NPC that is off-shift and awake in the post-work evening
	// window [shift-end, 22:00). It names that the day's work is done and the
	// tavern is open of an evening and lets the model decide — head over, stay in,
	// or turn in — imposing NO walk. nil when out of scope (unhomed / unscheduled /
	// not an agent), outside the evening window, red-need-pressed, already settled
	// at home, already at the venue or walking there, or no tavern venue resolves.
	// Renders in ## Around you, REPLACING the off-shift go-home wind-down steer for
	// the window's duration: buildDutySteer suppresses that steer in-window so this
	// is the single voice (no "turn in" pressure before Lever 1's 22:00 bedtime),
	// and it holds the noop-skip gate open in the steer's place so the idle agent
	// still ticks and sees the invitation. See buildEveningLeisure.
	EveningLeisure *EveningLeisureView

	// BakeChoice is the evening bake affordance for a resident settled at home
	// (LLM-454): non-nil when it could start or join the household's evening bread.
	// The single signal that gates the bake tool and its cue (renderBakeChoice), so
	// they can't drift. Fills the evening-at-home gap EveningLeisure leaves once the
	// actor is home. See buildBakeChoice.
	BakeChoice *BakeChoiceView

	// TurnInChoice is the voluntary bed-down affordance (LLM-447): non-nil when
	// the actor is settled where it sleeps — its own home, or the inn it rents —
	// off-shift, and the village clock has reached the evening. The single signal
	// that gates the turn_in tool and its cue (renderTurnInChoice), so they can't
	// drift. It is what gives the evening a third act: before it, an at-home
	// household could only keep talking until the engine bedded it. See
	// buildTurnInChoice.
	TurnInChoice *TurnInChoiceView

	// Warrants is every consumed warrant, ordered by SourceEventID
	// ascending — PR 3a's monotonic EventID is the authoritative causal
	// order. Zero-lineage warrants (SourceEventID == 0, legacy/non-event-
	// sourced) sort first; ties hold input order (stable). This is the
	// canonical ordered list; the per-scene groupings below reference the
	// same WarrantMeta values.
	Warrants []sim.WarrantMeta

	// Primary is the scene the baseline diff is computed against — the
	// scene of the warrant with the maximum SourceEventID, or (when no
	// warrant carries a scene) the actor's active-huddle scene. nil when
	// no scene resolved (Baseline == BaselineMissingNoScene).
	Primary *SceneView

	// Secondary holds warrants that reference a scene *other* than the
	// primary one. They render as independent source signals with their
	// own SceneID/HuddleID — the primary scene's baseline is deliberately
	// NOT applied to them. Ordered by SceneID for determinism.
	Secondary []SceneSignal

	// Baseline reports whether Primary.Diff could be established.
	Baseline BaselineStatus

	// MultiSceneWarrantCount is the number of distinct scenes referenced
	// by the consumed warrant batch (1 for the common single-scene tick,
	// 0 when no warrant carries a scene). Surfaced for the handlers-layer
	// telemetry field of the same name.
	MultiSceneWarrantCount int

	// NarrativeState is the actor's engine-side identity continuity —
	// seed_text identity frame + evolving_summary the consolidator
	// rewrites. Non-nil ONLY for KindNPCShared actors that have a
	// populated NarrativeState in the snapshot. Stateful-VA actors get
	// this content from their own VA's <Self> system prompt block via
	// memory-api; injecting engine-side would duplicate or conflict.
	NarrativeState *NarrativeStateView

	// Relationships are per-co-huddle-peer relationship views for the
	// subject actor — summary + recent salient facts for each peer in
	// the actor's current huddle. Populated ONLY for KindNPCShared
	// actors and only for peers the actor has a Relationship row for;
	// empty otherwise. Stateful-VA actors don't get this for the same
	// reason as NarrativeState (their own VA's per-peer context notes
	// cover this — see the symmetric stateful-VA gap at
	// shared/tasks/pending/salem-stateful-va-missing-peer-context).
	//
	// Ordering: sorted by PeerID for determinism.
	Relationships []RelationshipPeerView

	// VillageWord is the fallible gossip the subject carries about residents who
	// are NOT present in the scene — the "## Word about the village" section
	// (LLM-387). Projected from the subject's own ActorSnapshot.Rumors, freshest
	// first, bounded to maxRenderedVillageWord, with expired entries dropped and
	// any rumor whose subject stands in the current scene filtered out (you don't
	// gossip about someone to their face — the render mirror of sim's
	// salientRumorToShare). This is the ABSENT-subject twin of Relationships
	// ("what you remember of those here"), and unlike a perception fact it is
	// explicitly NOT a faithful readout. Populated for NPC kinds only; empty for
	// PC / decorative subjects and for an actor holding nothing shareable here.
	// VILLAGE word, not ROAD word (LLM-597): this is local gossip and it is
	// fallible by design. TravelerSelfView.RoadWord is its true-by-design
	// counterpart, carried in from outside by a traveler.
	VillageWord []VillageRumorView

	// RecentConversation is the last few spoken lines in the subject's current
	// huddle, oldest-first — the cross-tick "## Recent conversation here" section
	// (ZBBS-HOME-412). Unlike Relationships (shared-VA only), this is populated
	// for EVERY actor with a huddle — stateful NPCs included — and reflects the
	// PC's own lines, so a re-engaging actor sees that it already spoke and what
	// the player asked. This is the cross-tick re-pitch driver the per-pair
	// relationship trail and the within-tick HOME-411 swap both miss. Sourced
	// from the huddle's transient RecentUtterances ring; nil when the subject has
	// no huddle. The subject's own lines carry IsSelf for "You said" rendering.
	RecentConversation []UtteranceView

	// ConveyedSpeech identifies the spoken lines this tick's prompt put in
	// front of the subject (LLM-542) — the discharge signal, never rendered.
	// Populated alongside RecentConversation because both read the same ring;
	// see ConveyedSpeechRef for why the two lists differ.
	ConveyedSpeech []ConveyedSpeechRef

	// SelfActions is the subject's own recent committed-action trail, most-
	// recent-first — the "## What you've recently done" section (LLM-217).
	// Sourced from snap.ActionLog filtered to the subject, window- and
	// count-capped. Populated for every actor kind; nil when the subject has
	// no recent entries (or the snapshot carries no clock, as in hand-built
	// test payloads — the window needs PublishedAt to measure against).
	SelfActions []SelfActionView

	// Businessowner reports whether the subject actor runs a business
	// (Actor.BusinessownerState != nil — the existing keeper predicate).
	// Carries the trade conduct rules that used to live in salem-vendor's
	// startup_instructions, moved engine-side so the whole decision prompt is
	// code-owned and the rules sit at the decision point. ZBBS-WORK-374.
	// The vendor cues no longer gate on this directly — see AtOwnBusiness.
	Businessowner bool

	// AtOwnBusiness narrows Businessowner: true iff the subject runs a business
	// AND is physically at it (InsideStructureID == WorkStructureID, both set —
	// the "you keep your trade at X" anchor). The vendor cues — renderVendorOperating
	// and the OfferableCustomers "offer your wares" cue — gate on THIS, so a keeper
	// away from their post (a hungry customer in someone else's tavern) isn't
	// prompted to sell. Expresses WHERE the keeper is, not just WHO they are.
	// ZBBS-WORK-385.
	AtOwnBusiness bool

	// AtOwnBusinessOperating narrows AtOwnBusiness to operating hours (LLM-123):
	// true iff the keeper is at its own post AND open for trade — on shift, or
	// off-shift with a live stay_open commitment. The trade-conduct cue
	// (renderVendorOperating) gates on THIS, not bare AtOwnBusiness, so a keeper
	// standing at its closed stall after hours isn't told to "see to the day's
	// business" at midnight (the off-shift forge<->Tavern oscillation). The
	// customer-facing cues stay on AtOwnBusiness (location only) — a buyer who walks
	// up off-hours can still be served; the keeper just isn't nudged to drum up
	// trade at a closed post.
	AtOwnBusinessOperating bool

	// VendorTradeSlow is the engine-computed "trade is slow" judgment behind the
	// trade-conduct block's concession line (LLM-413): true iff the keeper is
	// operating at its own post AND no ware it sold this past week reached a
	// steady week's movement (keeperTradeSlow — produced goods measured against
	// their own batch, everything else against a single unit). The concession
	// line ("meet a willing buyer partway on price") renders ONLY when this is
	// set, so the licence to discount is a felt fact about an actually-slow week
	// rather than a standing instruction; the margin floor beside it renders
	// unconditionally. Always false when AtOwnBusinessOperating is false — the
	// block it feeds isn't rendered then.
	VendorTradeSlow bool

	// KeeperAwayFromPost is the off-post counterpart of AtOwnBusiness: true iff the
	// subject runs a business, HAS a post (WorkStructureID set — a keeper with none
	// is not away from one), is not at it, and has someone within earshot to deal
	// with (LLM-611). It gates renderKeeperAwayFromPost — the
	// buyer-side conduct rule, which is the ONLY trade guidance a keeper carries
	// off its own post. The trade-conduct block (renderVendorOperating) is
	// deliberately at-post only, so a keeper out buying stock had no rule about
	// value at all, and paid for stock with merchandise valued at whatever the
	// seller granted. Carrying only the valuation clause — never a selling
	// imperative — is what keeps this from reviving the WORK-385 sell-pressure
	// (a keeper pitching its wares as a customer in someone else's place).
	KeeperAwayFromPost bool

	// OfferableCustomers is the seller-side "offer your wares" cue
	// (ZBBS-HOME-404): non-nil when the subject is a businessowner co-present
	// with one or more customers it could proactively offer goods to. Carries
	// the customers' acquaintance-gated names plus the seller's sellable goods,
	// so the keeper LLM can drive a sale via scene_quote instead of only
	// reacting to a buyer's pay_with_item. This only makes the existing
	// seller-initiated path LEGIBLE (the Finding-1 lesson applied to the sell
	// side) — the seller still decides whether/what/at-what-price, and the
	// buyer keeps full accept/decline agency. nil (render content-gates) when
	// the subject isn't a businessowner at their own post (ZBBS-WORK-385), has no
	// co-present customer, or carries nothing to sell. Built by buildOfferableCustomers.
	OfferableCustomers *OfferableCustomersView

	// StandingQuotesFromMe lists the subject's OWN still-active scene-quotes —
	// the offers-to-sell it posted as SELLER via sell/scene_quote — driving the
	// "## Offers you've put out" section (renderStandingQuotesFromMe). It is the
	// seller/scene_quote mirror of PendingOffersFromMe (the buyer/pay_with_item
	// HOME-413 view): buildOfferableCustomers suppresses a re-pitch once a quote
	// stands (sellerHasActiveQuoteToBuyer), but without this the seller has no
	// record of WHAT it offered to WHOM, so a weak model re-posts the quote and
	// confabulates a queue between co-present seekers even as its offer to the
	// asker stands (LLM-45, the John Ellis two-room scene). Sourced from
	// snap.Quotes (active quotes where SellerID == subject), both targeted and
	// public; NOT a warrant, since a scene-quote warrants only the targeted
	// buyer. nil (render content-gates) when the subject has no active quotes
	// out. Ordering: by QuoteID ascending for determinism.
	StandingQuotesFromMe []StandingQuoteView

	// StandingQuotesToMe lists the still-active scene-quotes addressed TO the
	// subject — offers-to-sell another actor posted naming her as the buyer,
	// which she takes with pay_with_item + quote_id. Drives the "## Offers made
	// to you" section (renderStandingQuotesToMe). The buyer-side twin of
	// StandingQuotesFromMe, and it exists for the same reason that view does
	// (LLM-45): a quote warrants its target ONCE, so before LLM-551 the buyer's
	// only sight of an offer was the tick it was posted. A buyer who spent that
	// tick answering aloud — speak is terminal, so this is the common case —
	// never saw the quote again and lost the quote_id with it, leaving a bare
	// pay_with_item that crosses the quote instead of taking it. Live: Patience
	// Walker spoke her acceptance, then spent 22 minutes and ~20 rejected calls
	// trying to buy flour that was already set out for her, while the seller's
	// own prompt showed the offer standing the whole time.
	//
	// Sourced from snap.Quotes (active quotes where TargetBuyer == subject);
	// NOT a warrant — that is the defect. TARGETED only: a public quote is an
	// advertisement to the room, and standing one in every bystander's prompt
	// every tick would pin passers-by to a stall (the same reason hasOfferedQuote
	// is targeted-only). Lodging quotes to an already-homed subject are dropped,
	// mirroring filterHomedLodgingQuoteWarrants — she cannot take the room, so
	// standing the offer would dangle a doomed negotiation (LLM-182/208). nil
	// (render content-gates) when nothing is addressed to her. Ordering: by
	// QuoteID ascending for determinism.
	StandingQuotesToMe []StandingQuoteToMeView

	// UncoverableOffersFromMe lists the subject's OWN sell lots that JUST fell
	// through because the subject spent/ate/paid the quoted goods away out from
	// under his own offer — lots the pre-publish coverage reconcile
	// (reconcileQuoteCoverage) flipped to terminal SceneQuoteStateShortfall
	// within recentlyResolvedOfferWindow of snap.PublishedAt (LLM-409). Drives
	// the flat "## An offer you couldn't keep" beat: the seller learns he can no
	// longer honour a promise he made aloud, instead of the lot silently
	// vanishing from StandingQuotesFromMe. Sourced from snap.Quotes (shortfall
	// lots where SellerID == subject); NOT a warrant. nil (render content-gates)
	// when nothing fell through recently. Ordering: by QuoteID ascending.
	UncoverableOffersFromMe []UncoverableOfferView

	// PendingDeliveriesFromMe lists open Orders where the subject is
	// the seller — items they owe to a buyer/consumers from a previously
	// accepted pay-with-item offer that hasn't been delivered yet.
	// Populated for any KindNPCShared/Stateful seller; empty for PCs.
	// Renders as "## Orders to deliver:" surfacing item, qty, buyer/
	// consumer names, age, and time-remaining.
	//
	// Ordering: sorted by Order.ID (uint64 ascending) for determinism.
	// Phase 3 PR S6 — perception is the only awareness mechanism for
	// pending delivery; no new warrant kind (S4's PayResolved warrant
	// handles the initial post-accept tick cue).
	PendingDeliveriesFromMe []OrderView

	// PendingDeliveriesToMe lists open Orders where the subject is
	// the buyer OR a member of ConsumerIDs — items they're waiting on
	// the seller to hand over. Populated for any NPC subject; PCs get
	// this via UI.
	//
	// Renders as "## Orders you're waiting on:" surfacing item, qty,
	// seller name, age, time-remaining. Same OrderView struct as
	// PendingDeliveriesFromMe; the renderer picks fields per subject
	// role.
	//
	// Ordering: sorted by Order.ID for determinism.
	PendingDeliveriesToMe []OrderView

	// PendingOffersFromMe lists the subject's OWN still-pending pay-with-item
	// offers — the buyer-side mirror of the seller's "## Offers awaiting your
	// decision" (renderPayOffers). The seller sees offers staked against them;
	// without this the buyer has no symmetric awareness of an offer they
	// already placed, so a hungry NPC re-perceives "I'm hungry, they have
	// meat" every tick and stakes the SAME offer again — a cross-tick
	// repeat-offer storm (ZBBS-HOME-413; the pay-path twin of the speech
	// re-pitch ZBBS-HOME-412 fixed). HOME-395's offeredThisTick guard only
	// dedups WITHIN a tick; these offers are ticks apart.
	//
	// Sourced from snap.PayLedger (entries where BuyerID == subject and
	// State == Pending) — NOT from a warrant, because pay offers warrant the
	// seller only. nil (render content-gates) when the subject has no pending
	// offers outstanding. Ordering: by LedgerID ascending for determinism.
	PendingOffersFromMe []PendingOfferView

	// RecentlyResolvedOffersFromMe lists the subject's OWN pay-with-item offers
	// that left Pending very recently — the buyer-side resolution view that
	// closes the blind window between an offer leaving the pending scan
	// (PendingOffersFromMe) and the timing-fragile PayResolvedWarrantReason event
	// surfacing. The resolution warrant can ride a tick BEHIND the buyer's
	// in-flight deliberation (it opens a fresh cycle when stamped mid-tick), so a
	// buyer whose offer was just accepted re-perceives "the seller has it for
	// sale" and re-buys a need already met (the Hannah×Josiah water 270→271
	// re-offer). Sourced from snap.PayLedger (terminal entries where BuyerID ==
	// subject, resolved within recentlyResolvedOfferWindow of snap.PublishedAt) —
	// robust to warrant timing because it is a per-tick scan, not a warrant. nil
	// (render content-gates) when nothing resolved recently. Ordering: by
	// LedgerID ascending for determinism.
	RecentlyResolvedOffersFromMe []ResolvedOfferView

	// CountersAwaitingMyResponse lists a seller's counter to an offer the
	// subject placed as buyer that the subject has NOT yet answered — the
	// buyer-side standing decision view, counterpart to the seller's
	// PayOffersForMe (the seller's "## Offers awaiting your decision"). Sourced
	// from snap.PayLedger (terminal Countered entries where BuyerID == subject,
	// un-answered, below the chain depth cap, within counterResponseWindow of
	// snap.PublishedAt) — NOT the PayResolvedWarrantReason{Countered} event,
	// which can ride a tick behind the buyer's in-flight deliberation and is the
	// ONLY thing that surfaces a counter (the recently-settled scan excludes
	// Countered), so a buyer could re-offer a need already in negotiation or
	// miss the counter if the warrant is evicted (LLM-21). Robust to warrant
	// timing because it is a per-tick scan. nil (render content-gates) when no
	// counter awaits. Ordering: by LedgerID ascending for determinism.
	CountersAwaitingMyResponse []CounterOfferView

	// PayOffersForMe lists the still-pending pay-with-item offers staked
	// AGAINST the subject (entries where SellerID == subject and State ==
	// Pending) — the standing seller-side decision view that drives the
	// "## Offers awaiting your decision" section (renderPayOffers) and the
	// accept_pay/decline_pay/counter_pay tool gate, via PendingPayOffers.
	//
	// Sourced from snap.PayLedger every tick, NOT from the consumed warrant
	// batch (ZBBS-HOME-453): the PayOfferWarrant only wakes the seller's
	// first tick and is consumed by it, so a seller who SPOKE through that
	// tick instead of resolving used to lose both the cue and the response
	// tools while the offer sat pending — a structural deadlock until the
	// TTL sweep expired the entry (the 2026-06-12 Ellis meat negotiation).
	// The ledger scan keeps cue + tools standing until the entry leaves
	// Pending. Reuses sim.PayOfferWarrantReason as the projection shape —
	// it carries exactly the offer terms render and gate need, and the
	// restart re-stamp already projects entry → reason the same way.
	//
	// nil when nothing is pending against the subject. Ordering: by
	// LedgerID ascending for determinism.
	PayOffersForMe []sim.PayOfferWarrantReason

	// GiftsForMe / GiftsFromMe / SettledGiftsFromMe are the one-way gift lane
	// (LLM-138), all scanned from snap.PayLedger's IsGift entries: pending gifts
	// offered TO the subject (the accept_gift / decline_gift decision view),
	// the subject's OWN pending gifts (don't-re-offer standing view), and its
	// recently-settled gifts (taken-or-not resolution view). Gift entries are
	// excluded from the buy-side pay scans and rendered in dedicated sections so
	// a gift never reads through buy-shaped copy. nil (render content-gates) when
	// empty. Ordering: by LedgerID ascending for determinism.
	GiftsForMe         []GiftOfferView
	GiftsFromMe        []StandingGiftView
	SettledGiftsFromMe []SettledGiftView

	// RoomAlreadySoldOrderByLedger maps a pending lodging offer (by its
	// LedgerID in PayOffersForMe) to an existing Ready lodging order this
	// keeper already owes the SAME buyer. It marks the duplicate-room
	// situation LLM-89's AcceptPay gate rejects: a nights_stay grant lands
	// only at deliver_order, so a keeper who accepts a second room offer
	// before handing over the first double-charges the guest. renderPayOffers
	// uses it to steer "deliver the room you already sold, don't sell
	// another." nil when no pending offer overlaps an undelivered room.
	RoomAlreadySoldOrderByLedger map[sim.LedgerID]sim.OrderID

	// LaborOffersForMe lists the still-pending labor offers AWAITING THE SUBJECT'S
	// ANSWER (snap.LaborLedger entries where State == Pending and the subject is
	// the responder — the party who did not mint it). That is a worker's
	// solicitation when the subject is the employer, and an employer's offer of
	// work when the subject is the worker (LLM-346). The standing decision view
	// that drives the "## Work offers awaiting your decision" section
	// (renderLaborOffers) and the accept_work/decline_work tool gate
	// (PendingLaborOffers). Sourced from snap.LaborLedger every tick, same
	// standing-view posture as PayOffersForMe. nil when nothing is pending.
	// Ordered by LaborID ascending. LLM-26.
	LaborOffersForMe []LaborOfferView

	// SubjectProducesGoods is true when the subject makes any goods itself — has a
	// recipe-backed (makeable) produce entry, the same notion of "produces" the
	// labor produce-boost keys on (produce_tick). Only a producing keeper actually
	// "gets more done" from hired help, so the returning-helper recall
	// (renderLaborOffers, LLM-228) claims added output for a producer and stays a
	// bare social beat for a non-producer. Subject-level, so it rides the payload
	// rather than each LaborOfferView.
	SubjectProducesGoods bool

	// WorkersForMe lists the subject's IN-PROGRESS jobs as EMPLOYER — Working
	// LaborOffers where EmployerID == subject — the employer-side mirror of the
	// worker's Laboring self-state (LLM-202). Without it the employer has only the
	// pending-decision view (LaborOffersForMe) and nothing telling them a job is
	// already underway, so they re-hire a second body for work already covered
	// (live: John Ellis booked Patience for serving ale ~30 min into Silence's
	// still-running contract for the same). Drives the "## Workers currently
	// working for you" cue (renderWorkersForMe). Sourced from snap.LaborLedger
	// every tick, same standing-view posture as LaborOffersForMe. nil when no one
	// is working for the subject. Ordered by LaborID ascending. LLM-202.
	WorkersForMe []WorkerForMeView

	// Laboring is non-nil when the subject is a WORKER currently fulfilling an
	// accepted job (a Working LaborOffer where WorkerID == subject). It carries
	// the employer and the completion deadline so the self-state line can say
	// "you're working for X, about N more minutes." nil when the subject isn't
	// on a job. LLM-26.
	Laboring *LaboringView

	// LaborEnRoute is non-nil when the subject is a WORKER who has accepted a job
	// but is still relocating to (or waiting at) the employer's workplace before
	// the work window starts — an EnRoute LaborOffer where WorkerID == subject
	// (LLM-229). It carries the employer and whether the worker has arrived and
	// is waiting for the owner, so the self-state line can say "on your way to X"
	// or "waiting at X's for them to show." Mutually exclusive with Laboring (a
	// worker is either relocating or working, never both).
	LaborEnRoute *LaborEnRouteView

	// PendingLaborOfferOut is non-nil when the subject MINTED a labor offer that
	// is still awaiting the other party's answer (a Pending LaborOffer where
	// InitiatedBy == subject) — a worker who solicited, or an employer who offered
	// work (LLM-346). It carries the counterparty and the offered terms so the
	// self-state line can say "you've offered to work for X — wait for their
	// answer," or "you've asked X to work for you — it's their move." The mirror of
	// Laboring for the awaiting-answer state: an actor who has just offered has no
	// Working job yet, so without this anchor they sit with no labor self-state
	// and — under the LLM-159 quiet backstop / "choose one action" pressure —
	// flail into an unrelated tool (live: a worker paid her own employer while
	// waiting). nil when the subject has no outgoing offer. LLM-164.
	PendingLaborOfferOut *PendingLaborOfferOutView

	// CanSolicitWork is true when the subject is a free worker — carries the
	// AttrWorker marker, is not currently laboring, holds no unanswered offer in
	// either direction, and has an audience to offer to. The standing affordance
	// that renders the solicit_work cue (renderLaborAffordance) and gates the
	// solicit_work tool; the one signal drives both so cue and tool can't drift
	// (discussion-109). LLM-26.
	CanSolicitWork bool

	// SolicitableEmployers are the acquainted members of that audience, in
	// display-name order — the people the solicit cue names so the ask is
	// grounded in the room (LLM-564). Presentation only: CanSolicitWork remains
	// the cue/tool gate, and this slice may be empty under a true
	// CanSolicitWork (every solicitable peer a stranger), in which case the cue
	// renders its unnamed fallback wording. Populated only when CanSolicitWork
	// is true.
	SolicitableEmployers []sim.ActorID

	// HireableWorkers are the co-present actors the subject could take on for an
	// odd job right now: AttrWorker carriers in the subject's audience who share
	// neither its household nor its workplace, hold no live job or unanswered
	// offer, and have not just refused the subject (LLM-346). Ordered by display
	// name for a stable prompt.
	//
	// A non-empty slice IS the offer_work gate: renderOfferWorkAffordance names
	// these actors and gateTools advertises the tool off the same field, so the
	// cue and the tool cannot drift (the discussion-109 invariant). It must NAME
	// them — unlike solicit_work, whose caller knows they are a worker from their
	// own self-knowledge, an employer has no other way to learn which of the
	// people in front of her takes work for pay, and offer_work needs the name
	// verbatim.
	HireableWorkers []sim.ActorID

	// SeekWorkPlaces are the town's businesses (village objects tagged
	// sim.TagBusiness, resolved to their structure names), surfaced as move_to
	// destinations when a broke worker is nudged to go earn. Build populates it
	// for a broke idle worker with no employer present (the gate in Build), so a
	// non-empty slice IS the render gate. Each entry carries a qualitative distance
	// + direction so the worker heads to a near, open shop; ordered nearest-first,
	// de-duped by name, and businesses the worker recently found shut are dropped
	// (LLM-155). Names only — each is navigable by move_to-by-name (LLM-142). The
	// directional half of the LLM-141 seek-work backstop (LLM-152).
	SeekWorkPlaces []SeekWorkPlace

	// LocalDateUTC is midnight UTC of the village's current calendar date,
	// copied from Snapshot.LocalDateUTC. Render's order-book split
	// (renderPendingDeliveries*) compares it against each OrderView.ReadyBy so
	// the ready/future/overdue classification uses the same world-TZ date that
	// ReadyBy was built from — not the host UTC day, which drifts by the UTC
	// offset near the boundary. Zero when the snapshot has no clock (hand-built
	// payloads); the renderer falls back to the host UTC day then. ZBBS-HOME-403.
	LocalDateUTC time.Time

	// RenderedAt is the render INSTANT (full timestamp), copied from
	// Snapshot.PublishedAt — distinct from LocalDateUTC, which is the village's
	// calendar DATE (timezone-aware midnight). The order-book expiry clause
	// (renderPendingDeliveries* → expiryClause) renders "expires in N minutes"
	// relative to this, so the duration is anchored to the snapshot instant every
	// actor perceiving this snapshot shares — NOT to wall-clock at render time,
	// which made two NPCs rendering the same snapshot a beat apart show different
	// remaining-time text (and made the render path non-deterministic for tests).
	// Zero on a hand-built payload with no clock → the expiry clause is omitted
	// (expiryClause's far-future-horizon guard eats a zero now). LLM-106.
	RenderedAt time.Time

	// RecoveryOptions surfaces how a tired-or-homeless actor could rest —
	// free tiredness-bearing objects (shade trees) and inns to rent a room.
	// nil when the actor isn't tired/homeless or no options exist (the
	// homeless arm fires every tick — the lodging bootstrap cue). ZBBS-HOME-297.
	RecoveryOptions *RecoveryOptionsView

	// Satiation surfaces how a hungry-or-thirsty actor could eat/drink — the
	// items the actor already carries (consume-first) and nearby vendors selling
	// a satisfier. nil when neither hunger nor thirst is at its red threshold, or
	// no satisfier exists anywhere. The consume-first own-stock line is the core:
	// it connects the pressing need to the consume tool. ZBBS-HOME-304.
	Satiation *SatiationView

	// NeedRedirect is a concrete one-target redirect for a SOCIALLY-LOOPING actor
	// with a felt consumable need and a resolvable source already listed in
	// Satiation (LLM-176). non-nil only when ActorSnapshot.ConversationLooping is
	// set: renderTriage's looping coda then names this single target + a move_to /
	// consume imperative instead of the generic "do what you've agreed" line, so a
	// huddle going in circles over a confabulated plan ("food in the kitchen") is
	// pointed at the engine's known affordance (the nearest free source / an
	// affordable vendor). Mirrors the duty steer's inline structure_id. nil → the
	// generic looping coda renders.
	NeedRedirect *NeedRedirectView

	// Restocking surfaces how a reseller could replenish its low `buy` stock —
	// each item below the reorder threshold (current/cap) and the suppliers
	// selling it (workplace + structure_id + per-buyer price hint). nil when the
	// actor holds no `buy` entries below threshold, or restock is disabled
	// (RestockReorderPct == 0). The reseller's LLM decides whether/what/how-much
	// and acts via move_to + pay_with_item. ZBBS-WORK-322.
	Restocking *RestockingView

	// ProductionInputs surfaces, to a producer, the inputs it BUYS that are also
	// consumed by a recipe it produces and are below the reorder threshold — the
	// production dependency and how many more of the output good its input stock
	// backs ("you use skillet to make stew … enough for about 30 more"). The
	// producer-side WHY behind a restock trip; the WHERE/HOW stays in Restocking,
	// which fires on the same threshold for the same item (the LLM-64 split). nil
	// when no produced recipe has a low bought input, or restock is disabled. LLM-82.
	ProductionInputs *ProductionInputsView

	// ForgeChoice is the "## Your trade" scene (LLM-116, redesigned LLM-319):
	// to ANY producer idle at its workplace — nothing in the works — each good
	// it makes, compiled into tiered felt language (stock, sell-through, batch
	// cost/means), so it decides whether to start a batch via the produce tool.
	// Its presence gates that tool, so mid-batch (or away from the post, or
	// degraded-shut) both the cue and the tool disappear together. nil in all
	// those states.
	ForgeChoice *ForgeChoiceView

	// TradeValue surfaces, to an actor in company, the coin worth of the goods of
	// its own trade — the wholesale–retail spread plus its recent realized price —
	// so a barter has a yardstick instead of invented numbers. Own-trade only, so
	// the actor doesn't omnisciently learn others' prices. nil when alone or with
	// no priced own-trade goods. LLM-125.
	TradeValue *TradeValueView

	// StallRepair surfaces, to a market-stall owner standing at their own worn
	// stall, that it needs mending and how (nail count + buy-from-the-smith
	// steer). nil when not at the stall or it hasn't worn to the repair
	// threshold. The same non-nil view gates the `repair` tool. LLM-118.
	StallRepair *StallRepairView

	// StallCondition surfaces a co-present worn market stall to a NON-owner
	// standing at it — environmental texture a passerby can remark on. nil when
	// not at a worn stall (or when the actor owns it — StallRepair covers them).
	// LLM-118.
	StallCondition *StallConditionView

	// StallRepairBuy surfaces the OFF-POST half of the repair errand (LLM-277): an
	// owner who has stepped away from her worn business, still short of the nails a
	// repair takes, is shown where to buy them (walk-to destinations, or a co-present
	// pay_with_item imperative at the smith). nil when at the business (StallRepair
	// covers the buy there), carrying enough nails, not responsible for a repairable
	// business, or no actionable buy path. Distinct from StallRepair so it never
	// gates the on-site-only `repair` tool.
	StallRepairBuy *StallRepairBuyView

	// Hearth surfaces, to the actor responsible for a structure's fireplace
	// (its owner, or a worker hired by the owner — "tend the fire" is work,
	// not leaving), standing inside it, that the fire is out or low and how to
	// feed it (firewood count + buy steer). nil when not responsible, not
	// inside, or the fire is burning well. The same non-nil view gates the
	// `stoke` tool. LLM-412.
	Hearth *HearthView

	// HearthCooking is the cook's stake in that same fire (LLM-474) — what a live
	// hearth is worth to the batch. Independent of Hearth: it renders while a fire
	// burns well (when Hearth is nil) and gates no tool.
	HearthCooking *HearthCookingView

	// FarmUpkeep surfaces, to a farm owner, that the season wore out their upkeep
	// shovels and they owe fresh ones from the blacksmith (shovel count + buy
	// steer). nil when not a farm owner or nothing is owed. Not co-location-gated —
	// the buy happens at the blacksmith. LLM-215.
	FarmUpkeep *FarmUpkeepView

	// WorkClothes surfaces, to a working actor, that what they labour in has worn
	// through, and where cloth can be had. nil when their working garment is sound
	// (the common case) or they are not in a working posture. Owns shift/breeches/
	// gown; outerwear stays with the cold self-line. Not co-location-gated — the
	// replacement is bought elsewhere. LLM-589.
	WorkClothes *WorkClothesView

	// TownRate surfaces the constable's levy to whichever side is reading it: to a
	// keeper, what his business owes and the constable standing with him; to the
	// constable, that the rate runs toward him and which co-present keepers are
	// behind. nil unless the two are co-present. Co-location-gated — the coin is
	// handed over where they stand, so off-scene the cue has nothing to offer.
	// LLM-557, widened to the settled case by LLM-572.
	TownRate *TownRateView

	// CoinDealings is what coin has actually passed between the subject and each
	// co-present acquaintance inside the recall window — the record against which a
	// money claim made in this scene can be checked (LLM-572). Empty when nobody
	// present is an acquaintance. Populated for EVERY actor kind, unlike
	// Relationships: the trigger case was a stateful NPC, whose Relationships are
	// nil, refunding five coins on a debt that never existed.
	CoinDealings []CoinDealingsPeerView

	// Forage surfaces a grower-seller's own forage-to-sell bushes when their
	// harvested stock of an item is low (< RestockReorderPct of cap) — each low
	// `forage` RestockEntry, the on-hand/cap, the ripe count across the actor's
	// owned bushes for that item, and a move_to handle to the ripest bush. nil
	// when no forage entry is below threshold or restock is disabled. The
	// produce/harvest-side mirror of Restocking; owner-only and distance-
	// independent (unlike the wild-bush proximity cue, findGatherableCue). LLM-59.
	Forage *ForageView

	// Lodging surfaces the subject's own active lodging — the inn their room
	// is at and when the grant expires — so a lodger NPC can renew before it
	// lapses. nil when the actor holds no active ledger RoomAccess (not a
	// lodger). Sourced from the soonest-expiring active ledger grant.
	// ZBBS-HOME-296 PR2 (lodger view). The affordability cue lands in a
	// follow-on slice with the rent-rate setting.
	Lodging *LodgingView

	// KeeperLodging surfaces room availability to an actor who keeps a
	// lodging structure (works at a structure with private bedrooms), so the
	// keeper LLM can answer "do you have a room?" from real occupancy. nil
	// when the subject doesn't work at a lodging structure. ZBBS-HOME-296
	// PR2. The salem-vendor identity/persona preface lands in a follow-on
	// (it needs a typed vendor_persona projection + the keeper data seed).
	KeeperLodging *KeeperLodgingView

	// LodgingOffer is the seller-side "offer a room" cue: when a lodging keeper
	// (free private rooms, a configured nightly rate) shares a huddle with a
	// structural lodging-seeker (no home, no active grant), it names that seeker
	// + the nights_stay scene_quote call so the keeper proactively offers the
	// room. nil when the subject isn't a keeper-with-vacancy or no seeker is
	// co-present. Built by buildLodgingOfferCue. ZBBS-WORK-382.
	LodgingOffer *LodgingOfferView

	// KeeperHeldLodgers names co-present guests who already hold a room at the
	// subject keeper's inn, so the keeper affirms rather than re-offering off the
	// passive "## Your inn" vacancy line (LLM-38). nil when the subject isn't a
	// keeper or no held lodger is co-present. Built by buildKeeperHeldLodgers.
	KeeperHeldLodgers *KeeperHeldLodgersView

	// SummonsForYou surfaces a pending summons delivered to the subject by a
	// messenger — "<summoner> asks you to come to <place>" — driving them to
	// move_to. nil when the actor has no pending summons. Fades after the
	// actor next acts (the cue is cleared on the reactor tick). ZBBS-HOME-311.
	SummonsForYou *SummonsForYouView

	// SummonRefusal surfaces, to a summoner whose messenger returned unable
	// to locate the target, "<target> could not be found." nil otherwise.
	// Fades after the actor next acts. ZBBS-HOME-311.
	SummonRefusal *SummonRefusalView

	// SelectionReason is a human-readable explanation of how Primary was
	// chosen (or why it wasn't) — debug/test output only, never prompt
	// content.
	SelectionReason string

	// WarrantActorNames maps every actor referenced by a warrant (or pay
	// offer) in this payload to its acquaintance-gated display label —
	// DisplayName when the subject knows them, else "the <role>", else "a
	// stranger". The same name-vs-descriptor gating SurroundingsView's
	// HuddleMembers use. Render consults this so a "## Since your last turn"
	// line reads "Goodwife Ellis arrived nearby" instead of leaking the raw
	// actor UUID (ZBBS-HOME-339). The subject's own ID is deliberately
	// absent — Render resolves self to "you". Empty when no warrant
	// references another actor.
	WarrantActorNames map[sim.ActorID]string

	// WarrantPlaceNames maps a destination id (structure or village object)
	// named by an ArrivalWarrantReason in this payload to its display name, so
	// Render can say "You arrived at the General Store" instead of the vacuous
	// "arrived nearby" (ZBBS-WORK-358). Keyed by the raw id string because
	// structure and object ids share one space (the shared-identity bridge).
	// Empty when no arrival warrant names a place.
	WarrantPlaceNames map[string]string

	// WarrantPlaceKeepers maps an arrived-at structure id to the display name of
	// its keeper — the actor whose WorkStructureID is that structure — so the
	// arrival line can render the possessive "You arrived at Ezekiel Crane's
	// Blacksmith" instead of the ownerless "the Blacksmith", the cue that let a
	// visitor greet the smith as if hosting his own shop (LLM-284). The arriver
	// is excluded, so reaching one's own workplace keeps the plain form; only
	// structures have keepers, so village-object arrivals never appear here.
	// Empty when no arrival names a keeper's workplace.
	WarrantPlaceKeepers map[string]string

	// EatHereKinds is the set of item kinds that always settle eat-here
	// (consumable, neither service nor portable — ItemKindDef.EatHereOnly).
	// Render consults it so a quote warrant line can state the disposition
	// fact ("offers you stew for 4 coins, to eat here") instead of leaving
	// the model to discover the WORK-405 clamp by tripping it. Built once
	// from the snapshot's catalog; empty when no kind is eat-here-only.
	// ZBBS-WORK-405.
	EatHereKinds map[sim.ItemKind]bool

	// OwnProducedKinds is the set of item kinds the subject MAKES itself — its
	// produce-source restock entries. Render consults it to strip the actionable
	// take from a buy-quote for a good the actor produces: the buyer-side half of
	// the producer-awareness guard against buying back your own ware (LLM-171).
	// Empty when the actor produces nothing.
	OwnProducedKinds map[sim.ItemKind]bool

	// AtCapKinds is the set of item kinds the subject already holds at or above
	// its restock cap, across all restock sources with a configured cap (produce,
	// buy, forage). Render strips the actionable take from a buy-quote for such a
	// good — at cap, buying more just overflows what the actor can carry (LLM-171).
	// Empty when nothing is capped or at cap.
	AtCapKinds map[sim.ItemKind]bool

	// PayOfferShortfalls carries, per pending pay offer (keyed by LedgerID), the
	// seller's own shortfall on the asked good — the data renderPayOffers turns
	// into the "you hold no <good>" / "you hold only N <good>" annotation so the
	// seller counters or declines against real stock instead of accepting an offer
	// the deliver gate would bounce. Present only for offers where the asked kind
	// is a real good (a "service" kind has no inventory backing, so its stock gate
	// is skipped by the engine and "you hold no X" would be a false alarm —
	// item_kind.go) AND the buyer asks more than the seller holds. Built in
	// buildPayOfferShortfalls from the catalog + the subject's inventory, keeping
	// render catalog-free. Empty when nothing is short. LLM-303 widened this to fire
	// at zero held (a non-vendor offeree), not just vendors carrying some stock.
	PayOfferShortfalls map[sim.LedgerID]StockShortfall

	// PayOfferWorth carries, per pending BARTER offer (keyed by LedgerID), the
	// engine's verdict on what the buyer puts up against what it asks for — the
	// data renderPayOffers turns into the "far less than the flour is worth"
	// clause (LLM-598). Present only for offers judged thin or short: a fair
	// offer, a pure-coin offer, and anything the engine cannot price are all
	// absent, because render has no phrase for them. Built in buildPayOfferWorth
	// from the ledger + the catalog, keeping render catalog-free.
	PayOfferWorth map[sim.LedgerID]offerWorth
}

// StockShortfall is the seller-side shortfall on a pending pay offer: how many of
// the asked good the seller actually holds (Held — 0 when none) and the good's
// plural counting noun for the zero-case copy ("you hold no nails"). Emitted via
// Payload.PayOfferShortfalls only when the asked kind is a real good and the ask
// bites; renderPayOffers reads Held to choose the "no <plural>" line (Held 0) vs
// the existing "only N <kind>" annotation. LLM-303.
type StockShortfall struct {
	Held int
	Noun string
}

// OrderView is the perception-side projection of one sim.Order.
// Same struct shape regardless of whether the subject is the seller
// (PendingDeliveriesFromMe) or a buyer/consumer (PendingDeliveriesToMe);
// the renderer interprets fields differently based on which slice
// the view appears in.
//
// Names are resolved at build time from the snapshot's ActorSnapshot
// map (DisplayName falls back to ActorID when the actor is missing
// from the snapshot — defensive).
type OrderView struct {
	ID            sim.OrderID
	Item          sim.ItemKind
	Qty           int
	BuyerName     string
	SellerName    string
	ConsumerNames []string // empty when ConsumerIDs == [BuyerID] (implicit buyer-is-consumer)
	// AbsentRecipientNames lists the consumers NOT co-present with the seller —
	// the recipients DeliverOrder's gate-6 co-presence check would reject a
	// handover to. Populated only for the seller-side PendingDeliveriesFromMe
	// bucket; empty => every recipient is here and the order is deliverable now.
	// ZBBS-WORK-373 (boot-collapse Finding 6 bundle).
	AbsentRecipientNames []string
	CreatedAt            time.Time
	ExpiresAt            time.Time
	// ReadyBy is the order's booked date (lodging check-in date for an
	// advance booking; the creation date for a same-day order). Midnight UTC
	// of a calendar date. Render uses it to split the seller view into
	// ready-to-hand-over-now vs upcoming reservations, and the buyer view into
	// waiting-on vs overdue. ZBBS-HOME-403.
	ReadyBy time.Time
	// AwaitingMake marks a commission (LLM-338) the seller has taken payment for
	// but doesn't yet hold the goods to fulfil — DeliverOrder's gate-5 stock check
	// would bounce a deliver_order call. Populated only for the seller-side
	// PendingDeliveriesFromMe bucket. When set, render shows the order passively
	// ("you've yet to make it") and does NOT cue deliver_order, so the keeper is
	// steered to forge it first rather than into a bounce loop.
	AwaitingMake bool
	// MissingInputs (LLM-635): with AwaitingMake set, the required inputs of the
	// good's recipe the seller is out of for even one batch (display labels,
	// sorted) — the reason the produce tool the "yet to make it" line leans on is
	// withdrawn (LLM-324). Render names them so the order book states what is
	// stopping the make instead of leaving the keeper to promise delivery he has
	// no tool for. Empty when the make is possible now, or the recipe is unknown.
	MissingInputs []string
	// BalanceDue is the coin still owed on a partial-payment commission
	// (LLM-357), collected from the buyer at deliver_order; DepositPaid is what
	// was already put down at accept, so the full price is DepositPaid +
	// BalanceDue. Both zero for a full-prepay order. Render surfaces them as a
	// scene ("five down, ten to come") on the seller and buyer sides when
	// BalanceDue > 0 — so the keeper knows to collect the rest and the buyer
	// knows to bring it.
	BalanceDue  int
	DepositPaid int
}

// DeliverableNow reports whether this seller-side order can be handed over on
// this tick: the good is on hand (not AwaitingMake — LLM-338) AND every
// recipient is co-present (no AbsentRecipientNames — ZBBS-WORK-373), so neither
// DeliverOrder's gate-5 (stock) nor gate-6 (co-presence) would bounce a
// deliver_order call. The single predicate shared by the "## Orders to deliver"
// instruction (render) and the deliver_order tool-advertising gate
// (handlers.gateTools), so the tool and its triggering cue can't drift — the
// "advertise a tool only with its triggering perception" invariant (discussion
// 109). Seller-relative — meaningful only for the PendingDeliveriesFromMe bucket.
func (o OrderView) DeliverableNow() bool {
	return !o.AwaitingMake && len(o.AbsentRecipientNames) == 0
}

// PendingOfferView is the buyer-side projection of one of the subject's own
// pending pay-with-item offers (ZBBS-HOME-413). Renders in "## Your pending
// offers" as a "you already offered X for Y — wait, don't re-offer" cue. The
// LedgerID is carried for parity with the seller-side line and so the buyer
// could withdraw_pay it, though the section's primary job is suppression of a
// duplicate offer, not driving a new tool call.
//
// SellerName is the acquaintance-gated label (descriptorLabel) — the same
// name-vs-descriptor gating the seller side uses for the buyer. PayItems are
// the goods offered to pay WITH (barter leg); Amount is the coin leg. Item/Qty
// are the goods being bought. Built from snap.PayLedger; no untrusted free text
// reaches the render (item kinds are sanitized inline at render time).
type PendingOfferView struct {
	LedgerID   sim.LedgerID
	SellerName string
	Item       sim.ItemKind
	Qty        int
	Amount     int
	PayItems   []sim.ItemKindQty
}

// StandingQuoteView is the seller-side projection of one of the subject's own
// active scene-quotes — an offer-to-sell it posted via sell/scene_quote
// (LLM-45). Renders in "## Offers you've put out" as a "you already offered X to
// Y — await their answer, don't re-post" cue: the seller/scene_quote mirror of
// the buyer/pay_with_item PendingOfferView (HOME-413). BuyerName is the
// acquaintance-gated label (descriptorLabel), empty for a public (untargeted)
// quote heard by the whole room. QuoteID is carried for trace parity (the
// section's job is suppression, not driving a tool call, so it isn't rendered).
// Item/Qty/Amount are the good, count, and coin price quoted. Built from
// snap.Quotes; item kinds are sanitized inline at render time.
type StandingQuoteView struct {
	QuoteID   sim.QuoteID
	BuyerName string
	// Lines are the offer's item lines (LLM-101) — single-element for the
	// common single-item quote, multi for a bundle.
	Lines  []sim.QuoteLine
	Amount int
}

// StandingQuoteToMeView is the buyer-side projection of one active scene-quote
// addressed to the subject (LLM-551) — the twin of StandingQuoteView. Unlike
// that view, whose job is suppression (don't re-post), this one is ACTIONABLE:
// it carries the QuoteID because the render's whole purpose is to keep the
// quote_id in front of the buyer for as long as the offer stands.
//
// SellerName is the acquaintance-gated label (descriptorLabel). EatHere marks a
// bundle clamped to eat-here at creation (any non-portable line, LLM-101), so
// the buyer doesn't plan a carry-out the clamp would rewrite. Item kinds are
// sanitized inline at render time.
type StandingQuoteToMeView struct {
	QuoteID    sim.QuoteID
	SellerName string
	// Lines are the offer's item lines — single-element for the common
	// single-item quote, multi for a bundle taken whole.
	Lines   []sim.QuoteLine
	Amount  int
	EatHere bool
}

// UncoverableOfferView is one of the subject's OWN sell lots that JUST fell
// through because the subject spent the quoted goods out from under it — the lot
// the pre-publish coverage reconcile (reconcileQuoteCoverage) flipped to the
// terminal SceneQuoteStateShortfall within recentlyResolvedOfferWindow of
// snap.PublishedAt (LLM-409). Drives the flat "## An offer you couldn't keep"
// beat, the seller/scene_quote resolution counterpart to the buyer-side
// ResolvedOfferView. It exists for conversational continuity: without it the lot
// silently vanishes, and a seller who announced the offer aloud keeps no thread
// when the buyer later comes to take a good he no longer has. BuyerName is the
// acquaintance-gated label (descriptorLabel), empty for a public lot heard by
// the whole room. Lines are the good(s) and count promised; item kinds are
// sanitized inline at render time. Built by buildRecentlyShortfallQuotesFromMe.
type UncoverableOfferView struct {
	BuyerName string
	Lines     []sim.QuoteLine
}

// ResolvedOfferView is one entry in RecentlyResolvedOffersFromMe — a
// just-settled offer the subject placed as buyer. Accepted distinguishes "the
// seller took it" (suppress any re-buy of the same need) from a close without a
// deal (declined / expired / failed — stop waiting). ConsumeNow marks an
// accepted eat-here / drink-now deal whose goods were taken on the spot.
// KeptUnits is the consume_now needs-clamp surplus pocketed to the buyer
// (LLM-188): when >0 the buyer ate Qty-KeptUnits on the spot and kept the
// rest, so the rendered line reconciles with their carried inventory instead
// of claiming all Qty were "had right away".
type ResolvedOfferView struct {
	LedgerID   sim.LedgerID
	SellerName string
	Item       sim.ItemKind
	Qty        int
	Amount     int
	PayItems   []sim.ItemKindQty
	Accepted   bool
	ConsumeNow bool
	KeptUnits  int

	// DeliveryPending is true when this accepted offer minted an Order the seller
	// has NOT delivered yet (LLM-512): the goods are still in the seller's hands,
	// not the buyer's pack. The render must then say "not in your pack yet — the
	// seller will hand it over when it's ready" rather than "it's in your pack now.
	// That deal is done", which is false for an undelivered commission and
	// contradicts the same prompt's "## Orders you're waiting on" line. Always
	// false for a ConsumeNow offer (no Order is minted) and for a CLOSED
	// (non-accepted) view.
	DeliveryPending bool

	// SellerStock is the seller's current on-hand of Item; SellerStocks says the
	// shortfall reason may be named for this view (the asked kind is a real good,
	// not a service). Populated only for a CLOSED (non-accepted) view, so the
	// declined line can name a stock shortfall as the engine-known "why" the deal
	// fell through — the buyer's mirror of the seller-side "you hold only N"
	// pay-offer cue (LLM-296). The render gates on the bite (Qty > SellerStock);
	// SellerStockNoun is the plural counting noun for the zero-held copy ("they
	// hold no nails"). A "service" kind has no inventory backing (item_kind.go), so
	// SellerStocks stays false and the clause is skipped. Zero/false for an
	// accepted view — the reason clause is for closes only. LLM-303 widened this to
	// fire at zero held (a non-vendor seller), not just sellers carrying some stock.
	SellerStock     int
	SellerStocks    bool
	SellerStockNoun string
}

// CounterOfferView is one entry in CountersAwaitingMyResponse — a seller's
// counter to an offer the subject placed as buyer, not yet answered. SellerName
// is the acquaintance-gated label (descriptorLabel), the same gating the
// pending/resolved buyer views use. Item/Qty are the goods being bought;
// CounterAmount + CounterPayItems are the seller's proposed terms (coins, goods,
// or both). The seller's free-text counter Message is deliberately not carried —
// the warrant render line omits it too, and keeping untrusted text out of the
// section avoids a cross-actor prompt-injection surface. LedgerID is the parent
// offer's id: the buyer answers with pay_with_item(in_response_to=LedgerID).
type CounterOfferView struct {
	LedgerID        sim.LedgerID
	SellerName      string
	Item            sim.ItemKind
	Qty             int
	CounterAmount   int
	CounterPayItems []sim.ItemKindQty
}

// LaborOfferView is one pending labor offer AWAITING THE SUBJECT'S ANSWER — a
// worker offering to do a job for pay (solicit_work, LLM-26), or an employer
// offering the subject a job (offer_work, LLM-346). It drives the "## Work offers
// awaiting your decision" section (renderLaborOffers) and the
// accept_work/decline_work tool gate (PendingLaborOffers). LaborID is the
// load-bearing field the subject must echo back into accept_work/decline_work.
//
// Both parties are named because the subject is one of them and the rendered
// sentence must name the OTHER. SubjectIsEmployer says which side the subject is
// on, and therefore which direction the offer came from — the subject is the
// responder by construction, so it is also the direction it was NOT minted from.
type LaborOfferView struct {
	LaborID  sim.LaborID
	Worker   sim.ActorID
	Employer sim.ActorID
	// EmployerInitiated records WHO MINTED the offer — the same domain fact
	// sim.LaborOffer.EmployerInitiated() carries, not a subject-relative flag. The
	// subject is the responder by construction, so their role follows: an
	// employer-initiated offer means the subject is the worker being asked; a
	// worker-initiated one means the subject is the employer being solicited.
	// Read it through SubjectIsWorker / SubjectIsEmployer rather than inline, so
	// no callsite has to redo that step.
	//
	// The zero value is the worker-initiated direction — the only one that existed
	// before LLM-346 and still the common one — so a hand-built payload that omits
	// the field renders the sentence it always did rather than silently addressing
	// the subject as the wrong party. PendingLaborOfferOutView carries the same
	// field with the same name, meaning and zero value, so the two cannot be
	// confused for having opposite polarity (code_review).
	EmployerInitiated bool
	// Reward is the coin leg; RewardItems the in-kind leg (goods the
	// EMPLOYER hands over at settle — LLM-225). At least one is non-empty.
	Reward      int
	RewardItems []sim.ItemKindQty
	// MissingRewardItems is the subset of RewardItems the employer does NOT
	// currently hold in full, resolved at build time against the subject's
	// snapshot inventory — the goods half of accept_work's gate-8 mirror
	// (the coin half stays a render-side Coins comparison, LLM-158). Non-nil
	// drives the "you do not hold what they ask" decline steer.
	//
	// Populated ONLY when the subject is the employer: it is built from the
	// subject's own inventory, and a worker weighing an offered job cannot see
	// into the keeper's purse. An employer who named a wage they no longer hold
	// fails accept_work's gate 8 the ordinary way (LLM-346).
	MissingRewardItems []sim.ItemKindQty
	DurationMin        int
	ExpiresAt          time.Time
	// HelpedBeforeRecently is true when this worker completed a paid job for the
	// subject within HelpedByWorkerMemoryTTL (the employer's ObservedHelpedByWorker
	// memory is Active). Drives the returning-helper recall line the decision
	// section adds above the accept/decline steer (LLM-228). Employer-side only —
	// the memory lives on the employer, keyed by the worker's PeerID.
	HelpedBeforeRecently bool
}

// SubjectIsWorker reports whether the subject of this offer is the one who would
// do the work — i.e. an employer asked them. The subject of a LaborOfferView is
// always the responder, so this is exactly "the employer minted it." LLM-346.
func (v LaborOfferView) SubjectIsWorker() bool { return v.EmployerInitiated }

// SubjectIsEmployer reports whether the subject is the one who would pay — i.e. a
// worker solicited them. The complement of SubjectIsWorker, named so no callsite
// has to reason about the negation. LLM-346.
func (v LaborOfferView) SubjectIsEmployer() bool { return !v.EmployerInitiated }

// LaboringView carries the subject's OWN in-progress job for the self-state
// line (LLM-26): who they're working for and when the work window completes.
//
// OffPost / EmployerAway + their labels (LLM-268) are the off-post surface: a
// laboring worker's move_to is stripped while she's committed (LLM-230), EXCEPT
// when she has wandered off the post (OffPost) or her employer has left it
// (EmployerAway) — then the tool is re-granted and a directional cue rendered so a
// marooned worker can walk back and a "come with me" errand can be followed. These
// flags are the SINGLE predicate that both re-grants the tool (gateTools) and
// renders the cue (renderLaborSelfState), so the two can't drift — the same at-post
// definition (sim.ActorAtWorkpost) the world-side return-to-post backstop uses.
type LaboringView struct {
	Employer sim.ActorID
	Until    time.Time
	// PostLabel is the employer's workplace name, for the return cue. "" when the
	// post can't be labelled (its structure has no display name).
	PostLabel string
	// OffPost is true when the worker is not physically at the post — she has
	// wandered off (a need-break that left her, or following the employer).
	OffPost bool
	// EmployerAway is true when the employer is not at the post — the "come with
	// me" accompany case; the worker may follow.
	EmployerAway bool
	// EmployerPlace is where the employer is now, for the accompany cue ("Josiah
	// has gone to the General Store"). "" when it can't be resolved — the cue then
	// says they've stepped away without naming a destination.
	EmployerPlace string
}

// LaborEnRouteView carries the subject's OWN accepted-but-not-yet-started job for
// the relocation self-state line (LLM-229): who they're headed to work for, and
// whether they have arrived at the workplace and are waiting for the owner to
// show (Waiting) as opposed to still walking there. The work window has not
// started — the reward is named nowhere here; the line just keeps the tickable
// relocating worker on task instead of wandering or soliciting a second job.
type LaborEnRouteView struct {
	Employer sim.ActorID
	Waiting  bool
}

// WorkerForMeView carries one of the subject's in-progress jobs as EMPLOYER for
// the "## Workers currently working for you" cue (LLM-202) — the employer-side
// mirror of LaboringView. Worker is who's doing the job, Reward + RewardItems
// the pay owed on completion (coins and/or goods — LLM-225), Until the
// work-window deadline so the line can say "about N minutes left." The employer
// pays nothing until then (settle-at-completion), so this is purely a "the job
// is covered, don't re-hire or pay again" signal.
type WorkerForMeView struct {
	Worker      sim.ActorID
	Reward      int
	RewardItems []sim.ItemKindQty
	Until       time.Time
}

// PendingLaborOfferOutView carries the subject's OWN outgoing labor offer that
// is still awaiting the employer's answer, for the worker-side "you've offered,
// wait for their answer" self-state anchor (LLM-164). The worker has no Working
// job yet, so this is the only labor self-state they get while waiting; without
// it they flail under action pressure. Employer plus the offered terms (the same
// reward + duration they solicited with) so the line can name what's on the table.
type PendingLaborOfferOutView struct {
	Employer sim.ActorID
	// Worker is the actor who would do the job. Set on both directions so the
	// employer-side line can name whom they asked (LLM-346).
	Worker sim.ActorID
	// EmployerInitiated records WHO MINTED the offer — the same field name,
	// meaning and zero value LaborOfferView carries, so the two adjacent labor
	// views cannot be confused for having opposite polarity (code_review). The
	// subject of this view is always the INITIATOR, so their role follows: an
	// employer-initiated offer means the subject is the employer who asked.
	// Read it through SubjectIsEmployer / SubjectIsWorker.
	EmployerInitiated bool
	// Reward is the coin leg; RewardItems the in-kind leg the worker would
	// be paid in (LLM-225).
	Reward      int
	RewardItems []sim.ItemKindQty
	DurationMin int
}

// SubjectIsEmployer reports whether the subject minted an offer of work
// (offer_work) and is waiting on the worker's answer. The subject of a
// PendingLaborOfferOutView is always the initiator, so this is exactly "the
// employer minted it." LLM-346.
func (v PendingLaborOfferOutView) SubjectIsEmployer() bool { return v.EmployerInitiated }

// SubjectIsWorker reports whether the subject solicited (solicit_work) and is
// waiting on the employer's answer. LLM-346.
func (v PendingLaborOfferOutView) SubjectIsWorker() bool { return !v.EmployerInitiated }

// OfferableCustomersView is the seller-side "offer your wares" cue's content
// (ZBBS-HOME-404). CustomerNames are the acquaintance-gated labels
// (descriptorLabel) of the co-present customers the businessowner may offer to
// — the same name a known buyer would carry into scene_quote's target_buyer.
// Goods are the seller's sellable item labels (their carried inventory's
// DisplayLabels), surfaced as the menu next to the tool so the model has the
// item_kind candidates in hand. Both non-empty when the view is non-nil (Build
// returns nil rather than an empty view). Render is content-gated on both.
type OfferableCustomersView struct {
	CustomerNames []string
	Goods         []OfferableGood
	// ProducerNotes flags any co-present customer who MAKES one or more of the
	// seller's pitchable goods themselves (the good is in that customer's produce
	// manifest). The seller's stock of such a good came from a maker like them, so
	// Render steers "don't offer those back to their own maker" — the seller-side
	// half of the producer-awareness guard against the degenerate buy-back loop
	// where a reseller pitches a smith's own skillet back at the smith (LLM-171).
	// Empty when no co-present customer produces any of the goods.
	ProducerNotes []ProducerNote
}

// OfferableGood is one sellable good in the "## Custom at hand" cue, carrying
// its on-hand count so the seller sizes a scene_quote against real stock rather
// than naming a round number it can't deliver (ZBBS-HOME-459 — the seller-side
// mirror of the buyer's ZBBS-WORK-392 sufficiency fact). OnHand is the seller's
// current Inventory[kind] at build time. kind is the unexported item kind, kept
// so Build can match a good against a co-present customer's produce manifest
// (LLM-171) without re-resolving the label.
type OfferableGood struct {
	Label  string
	OnHand int
	// Use is the "used to produce X" annotation for an INEDIBLE sellable
	// ingredient (LLM-166) — mirrors InventoryItem.Use so the for-sale listing
	// reads consistently with the carry readout. Empty otherwise.
	Use string
	// Commission marks a good the seller holds NONE of but can forge to order
	// (LLM-619): sim.CommissionableKind plus inputs on hand. acceptPendingOffer
	// waives its stock gate for exactly these, so the sale is real — but with an
	// empty shelf the good was previously absent from the listing entirely, and
	// the model read that as "cannot sell" and fell back to a spoken promise
	// nothing commits. OnHand is always 0 for these.
	Commission bool
	kind       sim.ItemKind
}

// ProducerNote names a co-present customer and the seller's goods that customer
// makes themselves — so Render can steer the seller not to pitch a maker their
// own ware back (LLM-171). CustomerName is the same acquaintance-gated descriptor
// used in CustomerNames; Goods are the overlapping good labels.
type ProducerNote struct {
	CustomerName string
	Goods        []string
}

// NarrativeStateView is the kind-aware "## Who you are" content for a
// shared-VA actor: the accreting first-person soul the per-actor narrative
// sweep synthesizes each day via the dream-sim-soul agent (LLM-199). Nil for
// stateful / PC actors, who get identity elsewhere (the VA's <Self> block /
// the player), and for transient travelers, whose identity preface is
// renderTravelerPreface (LLM-370).
//
// Name is the actor's own display name (LLM-432). The shared VA's system
// prompt is a generic sim context and AboutMe is first-person prose that
// doesn't reliably contain the name, so without this line the model never
// learns who it is — and a bystander hearing "ezekiel, you sleeping over
// there?" can't rule itself out as the addressee.
type NarrativeStateView struct {
	Name    string
	AboutMe string
}

// RelationshipPeerView is the per-peer entry in the "What you remember
// of those here:" section. RecentFacts holds the most-recent N facts
// (most-recent-first) — Build slices them from the actor's
// Relationships[peerID].SalientFacts, which is stored oldest-first.
type RelationshipPeerView struct {
	PeerID      sim.ActorID
	PeerName    string
	SummaryText string
	RecentFacts []sim.SalientFact
}

// VillageRumorView is one carried rumor projected for the "## Word about the
// village" render (LLM-387): the diegetic clause at its current embellishment
// rung, plus whether THIS holder witnessed it first-hand (true) or only heard it
// relayed (false) — render frames a first-hand line as something the actor saw
// and a hearsay line as talk going round Salem, so the model can weight its own
// certainty. No rung number, subject id, or HeardAt is carried; the clause is the
// whole diegetic payload, keeping the section scenes-not-stats.
type VillageRumorView struct {
	Clause    string
	FirstHand bool
}

// UtteranceView is one line in the "## Recent conversation here" section
// (ZBBS-HOME-412), projected from a Huddle's RecentUtterances ring. IsSelf marks
// the subject's own lines so the render reads "You said" vs "<Name> said",
// making turn-taking legible. SpeakerName is the speaker's display name.
// At is the moment the line was spoken (LLM-217): render stamps each line with
// its distance from RenderedAt ("just now" / "40s ago") so the model can tell
// rapid-fire churn from a normally paced exchange — without it, three
// "I'll head home now" lines minutes apart and three seconds apart read
// identically, and the anti-repeat instruction has no tempo to work with.
type UtteranceView struct {
	SpeakerName string
	Text        string
	IsSelf      bool
	At          time.Time
}

// ConveyedSpeechRef identifies one spoken line this tick's prompt put in front
// of the subject (LLM-542). Never rendered — it exists so the completion path
// can discharge the speech warrant that line stamped, instead of letting it
// fire a second reply to a line already answered.
//
// "Conveyed" is deliberately wider than "rendered in ## Recent conversation
// here". The rule is: the line's TEXT reached the prompt, by either heading.
// A ring line the heardNow de-dup drops is dropped BECAUSE its text is already
// under "## Since your last turn", so it counts — including one sitting past
// the maxRenderedConversationLines window, which this section would never have
// shown. A line past the window that nothing else carried does not count; a
// warrant it stamped is still owed. buildRecentConversation owns the rule.
//
// SpeakerIsPC selects which of the two speech warrant kinds the line stamped
// (pc_spoke vs npc_spoke); the discharge key needs both halves.
//
// ViaWarrants is non-empty when the line reached the prompt ONLY through
// another warrant's render — the de-dup case. Build runs before Render, so at
// Build time that is a conditional claim: a warrant can still be dropped by
// MaxWarrants / MaxSectionBytes, and if every carrier is dropped the text
// reached the prompt through neither heading. CollectDischargedSourceKeys
// settles it against the actual dropped set; ANY surviving carrier suffices.
//
// A list rather than one key because several warrants in a batch can match the
// same line — identical short utterances, or distinct long ones sharing the
// truncated dedup prefix. With one key the claim would turn on warrant order,
// and a dropped carrier could mask a rendered one (code_review).
//
// Empty means unconditional: the line rendered in "## Recent conversation here"
// in its own right.
type ConveyedSpeechRef struct {
	SpeechID    sim.SpeechID
	SpeakerIsPC bool
	ViaWarrants []sim.WarrantSourceKey
}

// SelfActionView is one line in the "## What you've recently done" section
// (LLM-217) — the subject's own recent committed actions, projected from
// snap.ActionLog. This is the NPC's only cross-tick memory of its own DEEDS:
// warrants live one tick and the conversation ring carries speech only, so
// without it an actor cannot see "I've left for home and bounced back three
// times" and self-loops (the Patience Walker go-home ↔ seek-work oscillation)
// stay invisible to the model. Fields mirror the ActionLogEntry the line came
// from; render phrases them second-person ("You arrived at the Tavern").
type SelfActionView struct {
	ActionType       sim.ActionType
	Text             string
	CounterpartyName string
	Amount           int
	// PayItems are the barter goods the subject handed over alongside Amount on a
	// pay_with_item settlement (LLM-374) — the non-coin leg. Empty for a pure-coin
	// pay. selfActionLine appends it so the self-trail shows the full tender, not
	// just the coins.
	PayItems []sim.ItemKindQty
	At       time.Time

	// FoundShut marks an ActionTypeWalked entry whose destination business the
	// subject still remembers finding shut (ObservedClosed, within TTL). The trail
	// then renders "You went to X but found it shut" instead of a neutral "You
	// arrived at X", giving LLM-217's churn-mirror the outcome it lacked: a run of
	// dead-end trips reads AS dead ends, not ordinary errands (LLM-366).
	FoundShut bool
}

// ActorView is the subject actor's own current state, lifted from the
// snapshot's ActorSnapshot. Slim by design — content fills in incrementally
// (PR 3c ships the mechanism, not the final prompt surface).
type ActorView struct {
	State             sim.ActorState
	InsideStructureID sim.StructureID
	Position          sim.Position
	CurrentHuddleID   sim.HuddleID
	// Coins is the subject's SPENDABLE coin (sim.ActorSnapshot.SpendableCoins,
	// LLM-644) — the wallet for a resident, the wallet capped by the remaining
	// trip budget for a visitor. Every affordability read downstream of the
	// view (the purse line, labor-offer weighing) then agrees with the pay
	// tools' gates by construction.
	Coins int
	// VisitorTakings is the coin the subject holds ABOVE what it can spend —
	// nonzero only for a visitor whose sales have outrun his trip budget
	// (LLM-644). The purse line renders it as takings bound for home, so the
	// wallet the model has watched grow doesn't silently vanish from the scene.
	VisitorTakings int
	Needs          map[sim.NeedKey]int

	// NeedThresholds is the red-tier boundary per need, copied from the
	// snapshot so Render can classify each need value into its felt tier
	// (peckish/hungry/starving) without re-reading world state. ZBBS-HOME-339
	// — replaces the raw "needs: hunger=24" dump with felt language.
	NeedThresholds sim.NeedThresholds

	// InFlightProduction is the actor's in-progress production cycle (LLM-319),
	// nil when nothing is in the works. Rendered as a standing "You are making a
	// batch of X — about N minutes of work left" self-state line on EVERY tick —
	// including a social tick when someone approaches — so the producer always
	// knows its own current work (a PC walking up to ask "what are you making?"
	// gets a real answer), and a tick firing mid-batch doesn't walk off the post
	// unawares.
	InFlightProduction *InFlightProductionView

	// ActiveDwellCredits is the actor's in-progress dwell credits at
	// snapshot time — meals being eaten, rests being taken. Renders as
	// "you are currently eating stew at the tavern, ~14 minutes
	// remaining" so the LLM can see the meal as a coherent state across
	// the per-minute event stream (DwellTickApplied warrants). The
	// structured surface is the load-bearing piece that keeps NPCs
	// parked: even if the per-tick narration warrant hasn't landed yet
	// for this tick, the structured field is always present while the
	// credit lives, so perception render can keep the cue in front of
	// the LLM on every tick.
	//
	// Sort order is deterministic: by (Source, Attribute, ObjectID) so
	// golden tests + admin replay see stable ordering.
	ActiveDwellCredits []DwellCreditView

	// InFlightMove is the subject's current walk, nil when not moving.
	// Rendered as "currently: walking to <label>" so a reactor tick firing
	// mid-walk reminds the LLM it already has a destination — the movement
	// analogue of ActiveDwellCredits. ZBBS-HOME-336.
	InFlightMove *InFlightMoveView

	// InFlightSourceActivity is the subject's in-flight timed eat/drink/harvest
	// at a source, nil when not engaged. Rendered as a STANDING "you are
	// gathering at the bush — stay put, walking off abandons it" self-state line
	// so a reactor tick that fires mid-activity (a PC speaking, a red need —
	// LLM-69 relaxed the reactor shelve for those) reads its own state and holds
	// rather than re-deciding. The source-activity analogue of InFlightMove /
	// ActiveDwellCredits. LLM-69.
	InFlightSourceActivity *InFlightSourceActivityView

	// Rounds is the constable's in-flight rounds tour (LLM-514), nil when he is
	// not walking his rounds. Rendered as a diegetic "You are walking your rounds
	// through the village." self-state line so any reactor tick during the tour is
	// in character (a suspicious constable), and — once he has arrived at a stop —
	// "You stand before the <business>." It is the sole movement voice while a tour
	// runs (renderActor suppresses the plain InFlightMove line), so the two don't
	// both speak.
	Rounds *RoundsView

	// Inventory is the actor's carried goods — the STANDING "what you're
	// carrying" readout (ZBBS-HOME-361), restored after the v2 rewrite dropped
	// v1's inventory line and left NPCs blind to their own pockets (a hungry
	// NPC holding cheese walked to a shop because nothing in perception told it
	// what it held). Deliberately NEUTRAL and UNGATED: it states possession, not
	// need-resolution. The "consume to eat / drink" push stays in the
	// need-gated satiation own-stock line — so a not-yet-pressing actor sees it
	// has cheese without being nudged to eat, and a vendor (e.g. an innkeeper)
	// sees its sellable stock regardless of its own needs. Resolved + sorted at
	// build time; empty (render omits the line) when carrying nothing.
	Inventory []InventoryItem

	// PackBoundHome marks the carried goods as a settled buy-errand traveler's own
	// provisions rather than stock (LLM-574) — rendered as a coda on the inventory
	// line itself, because that line is where the temptation to sell them lives. See
	// travelerPackBoundHome for what makes the claim true; false for everyone else.
	PackBoundHome bool

	// HoursAwake is whole hours since the actor woke at its shift-start, used to
	// anchor the tiredness line ("you've been awake for X hours") so the model
	// weighs rest against real elapsed time, not a bare adjective (LLM-85).
	// Resolved at build time and populated only while the actor is on-shift
	// (where wakefulness-since-shift-start holds); nil off-shift, unscheduled, or
	// with no clock, which the renderer reads as "drop the awake-hours tail".
	HoursAwake *int

	// Cold is the subject's felt cold plus its exposure context (LLM-412),
	// rendered as its own situated line after tiredness — a scene with the
	// relief the situation offers, never a number. nil below the awareness
	// floor (the common case).
	Cold *ColdSelfView
}

// InventoryItem is one carried item kind in the standing inventory readout —
// its display label + quantity. kind is the unexported sort tie-break so two
// kinds sharing a label order deterministically (Inventory is a map).
// ZBBS-HOME-361.
type InventoryItem struct {
	Label string
	Qty   int
	// CountNoun is the qty-aware counting phrase for the carry readout —
	// def.CountNoun(qty) ("flask of water" / "flasks of water"), so the model is
	// handed the period vessel word instead of a bare label it must invent a
	// container for ("Water (x20)" led NPCs to say "buckets of water"). Falls
	// back to Label for a kind with no authored singular/plural (LLM-113).
	// Distinct from Label, which stays the display label used for the
	// deterministic sort and the for-sale (offerable-goods) listing.
	CountNoun string
	// Use is the "used to produce X" annotation for an INEDIBLE carried
	// ingredient (LLM-166), so a hungry model doesn't mistake it for food.
	// Empty for an edible item (the satiation cue owns those) or a non-ingredient
	// (nothing to say). See buildInventoryView.
	Use string
	// EatHere marks an eat-here-only carried food (ItemKindDef.EatHereOnly —
	// LLM-445): the carry readout annotates it "to eat here — not for trade" so
	// the model doesn't plan a barter the resolver rejects. Mutually exclusive
	// with Use (Use is inedibles-only; EatHere is consumables-only).
	EatHere bool
	// Spare is how many held units are up for barter, and SpokenFor why the rest
	// are not (sim.SpokenFor, LLM-636): the makings a keeper works with, or the
	// one garment on its back. Render annotates a kind with no spare unit "not
	// for trade" and a partly-reserved one with its spare count, so the carry
	// line says what the pay_with_item intake gate will refuse. Spare == Qty and
	// SpokenForNone for an unreserved good.
	Spare     int
	SpokenFor sim.SpokenForReason
	// Barterable is whether this good could go up in a pay_items / offer_trade /
	// labor-reward bundle right now: its kind is tradeable at all
	// (sim.KindBarterable — not a service, not eat-here-only; LLM-445) AND at
	// least one unit is spare (LLM-636). Render's coinless "offer goods in trade"
	// line keys on it so the purse cue and the means-to-pay gates
	// (holdsBarterableGoods) cannot disagree about the same pack.
	Barterable bool
	kind       sim.ItemKind
}

// InFlightMoveView is the perception-side projection of the subject's
// in-flight MoveIntent. DestinationLabel is resolved at build time (structure
// / object DisplayName, or a tile coordinate for a bare position move); empty
// when the destination can't be resolved. Kind drives the render phrasing
// ("walking to enter X" for a structure-enter vs "walking to X" for a visit).
// ZBBS-HOME-336.
type InFlightMoveView struct {
	Kind             sim.MoveDestinationKind
	DestinationLabel string
}

// RoundsView is the perception-side projection of a round the constable owes
// (LLM-514). AtBusiness is the display name of the business he is STANDING at,
// empty when he is anywhere else — so the render adds "You stand before the
// <business>." at a stop and leaves it off between them.
//
// The view has no paused/walking distinction (LLM-548). A round is a standing
// obligation, not a procession he can be in the middle of: he owes these places
// today, and whether he is on a doorstep or at the well, what he needs told is the
// same — how many are left and which one is next.
type RoundsView struct {
	AtBusiness string

	// StopsAhead is how many businesses he still OWES a visit, counted from the
	// visited record rather than the cursor (LLM-543) so a shop he called at himself
	// is never offered twice. It carries the round forward ("Seven more places on
	// your round still lie ahead of you") — an empty stop is then a waypoint, not a
	// dead end (LLM-524). 0 on the last one, where the line is omitted.
	StopsAhead int

	// NextBusiness is the display name of the next stop he owes, empty when nothing
	// is left. Named verbatim in the cue (LLM-530) so it is a usable move_to
	// destination: "I am finished here" needs somewhere to resolve to, and his own
	// post was previously the only place the prompt named.
	NextBusiness string
}

// InFlightSourceActivityView is the perception-side projection of the subject's
// in-flight SourceActivity (LLM-69). Kind drives the verb ("gathering" for a
// harvest; eat/drink/rest for a refresh, picked from Attribute). SourceLabel is
// resolved at build time (the source object's DisplayName, the same
// resolveDwellPinLabel path dwell pins and move destinations use); empty when it
// can't be resolved. Attribute is the primary need a refresh eases (empty for a
// harvest, where the verb is need-independent).
type InFlightSourceActivityView struct {
	Kind        sim.SourceActivityKind
	SourceLabel string
	Attribute   sim.NeedKey
}

// InFlightProductionView is the perception-side projection of the subject's
// in-flight production cycle (LLM-319). ItemLabel is the batch good's display
// label; WorkLeft is the humanized base-rate work remaining ("about 40
// minutes" reads as an estimate on purpose — hired help shortens the real wall
// time, a degraded business stretches it). Resolved at build time so render
// stays a pure formatter.
//
// Slowed / Halted (LLM-446) carry the degraded-business drag on the batch so
// the standing line tells the truth about the clock: Slowed at a positive
// StallDegradedProducePct (the batch advances under the sap), Halted at pct 0
// (the legacy LLM-304 full pause — WITHOUT this line the frozen WorkLeft
// renders as "about 3 minutes of work left" every tick forever, the live
// "three more minutes, Josiah" loop). At most one is set.
type InFlightProductionView struct {
	ItemLabel string
	WorkLeft  string
	Slowed    bool
	Halted    bool
}

// DwellCreditView is the perception-side projection of one
// sim.DwellCredit. Carries the structure label resolved at build time
// (so render doesn't re-fetch from world state), plus the raw
// countdown fields so Hub clients can derive "next-tick at" and
// "remaining time" without tracking prior state.
//
// Kind is empty for source=object credits (resting under a tree —
// no item involved); render falls back to a generic phrasing in that
// case ("you are resting under the willow" rather than "you are
// currently eating X").
type DwellCreditView struct {
	ObjectID       sim.VillageObjectID
	StructureLabel string // resolved from snap.Structures or snap.VillageObjects; "" when neither resolves
	Source         sim.DwellCreditSource
	Kind           sim.ItemKind
	Attribute      sim.NeedKey
	RemainingTicks *int // nil for source=object; >0 for source=item
	PeriodMinutes  int
	DwellDelta     int // negative — applied per period
	LastCreditedAt time.Time
}

// SurroundingsView is the actor's immediate context — the structure it is
// in and the huddle it belongs to, with co-present actors named or
// rendered as descriptors based on acquaintance.
type SurroundingsView struct {
	InsideStructureID sim.StructureID

	// OnRound is true while the subject is carrying a rounds circuit. Used only
	// to choose between "this round" and a bare time phrase in the LLM-547
	// contact lines — the round itself is rendered from RoundsView, not here.
	// Shares actorOnRound with the rounds cue so the two can never disagree
	// about whether a round is under way.
	OnRound bool

	// StructureName is the structure's DisplayName, or empty when the
	// actor is outdoors or the structure is absent from the snapshot.
	StructureName string

	// InsideRelation names how the structure the actor is INSIDE relates to the
	// actor: "your home", "your workplace", or "your home and workplace" when it
	// is both (a keeper who lives at their shop). Empty when the actor is
	// outdoors, or inside a structure that is neither its home nor its workplace.
	// Render appends it to the location line ("inside the James Residence, your
	// home") so a weak model can tell at a glance it is already at its own anchor
	// — the legibility half of the move_to(home) confusion LLM-209 hardened
	// (LLM-212).
	InsideRelation string

	// NearbyStructureName is the DisplayName of the structure the actor is
	// standing at the loiter slot of while OUTDOORS (within
	// sim.LoiterAttributionTiles) — e.g. a shopkeeper at their own stall, a
	// customer outside a shop. Empty when inside a structure or not at any
	// structure's loiter slot. Lets Render say "outdoors by the General
	// Store" instead of dumping raw "(94, 126)" coordinates (ZBBS-HOME-339).
	NearbyStructureName string

	// LocationDeadEnd names a live reason the place the actor is physically at
	// can't serve them, or DeadEndNone when it can (LLM-154). A LIVE, situated
	// read recomputed cold each tick from the snapshot — the NPC is standing
	// here and can see the empty stall — distinct from the ObservedClosed
	// *memory* (consumable_vendors.go) that deprioritizes a FAR-AWAY cue. Render
	// states it plainly next to the location line ("The Tavern is shut — no one
	// is tending it.") so a weak model isn't left to infer "closed" from "the
	// keeper is asleep." Closed-business is the first wired reason; out-of-stock
	// / exhausted-source / locked-entry slot in as further values.
	LocationDeadEnd DeadEndReason

	// DeadEndHunger / DeadEndThirst qualify a DeadEndNoConsumableHere
	// LocationDeadEnd: which felt consumable need(s) have no source at this place,
	// so deadEndClause can name "eat" vs "drink" vs "eat or drink" (LLM-176). Unset
	// for any other LocationDeadEnd.
	DeadEndHunger bool
	DeadEndThirst bool

	// HuddleID is the actor's current huddle, empty when not huddled.
	HuddleID sim.HuddleID

	// HuddleMembers are the *other* members of the actor's current huddle
	// (the subject actor is excluded), sorted by ID for determinism.
	// Each carries acquaintance info so Render can pick name vs.
	// descriptor without re-reading the snapshot.
	//
	// Populated ONLY when the actor is in a huddle (CurrentHuddleID != ""),
	// empty otherwise — the mutually exclusive counterpart of CoPresent below
	// (Build fills exactly one of the two). Gates that key on huddle membership
	// rely on this: non-empty ⇒ the actor is huddled with a resolvable peer
	// (e.g. the pay-verb tool gate, LLM-329 — both pay verbs reject a
	// non-huddled buyer at the substrate).
	HuddleMembers []HuddleMember

	// HuddleLive reports whether the subject's huddle has seen activity — a
	// spoken line, a join, a completed transaction — within the snapshot's
	// HuddleLiveWindow (LLM-467, via sim.HuddleIsLive). False when the actor is
	// not huddled.
	//
	// It answers "is anyone still talking" where HuddleMembers answers "is anyone
	// standing here". The two came apart because a huddle outlives its
	// conversation by up to HuddleSilenceTimeout (2h) — deliberately, so a
	// returning patron resumes rather than restarts — and for most of that span
	// the room is silent. Read by the noop-skip preflight
	// (handlers.shouldSkipNoop), which previously treated bare presence as
	// conversational demand and billed every member a full LLM call per idle
	// backstop to rediscover the talk was over. Render does NOT consult this: a
	// tick that runs describes the room as it is, dormant or not.
	HuddleLive bool

	// HuddleAway are co-present huddle members who have stepped away — a PC whose
	// client has gone quiet (WS dropped, presence stamp stale; LLM-342). They keep
	// their huddle membership but are deliberately NOT in HuddleMembers, so they
	// drop out of every addressable cue that reads it (offerable customers,
	// greet/respond, HasAudience) while Render still names them "(stepped away)" in
	// "## Around you" — co-present but not someone to address. Populated only in the
	// huddle branch; empty otherwise. A returning player re-enters HuddleMembers the
	// moment its socket re-stamps presence.
	HuddleAway []HuddleMember

	// CoPresent are the other conversational actors within earshot when the
	// subject is NOT in a huddle — the read projection of ActorSnapshot.
	// ColocatedAudienceIDs (the set the speak path would reach), each carrying the
	// same acquaintance info as HuddleMembers so Render names or describes them
	// with identical gating. Empty when the actor is huddled (HuddleMembers carries
	// its company then) or genuinely alone in scope. Already sorted by ID (the
	// world-side helper sorts; Build preserves the order). ZBBS-WORK-407.
	CoPresent []HuddleMember

	// CoPresentAsleep are co-present SLEEPING actors when the subject is NOT in a
	// huddle — the read projection of ActorSnapshot.ColocatedSleeperIDs. Render
	// names them in a distinct not-addressable clause, never folded into CoPresent's
	// "here with you" set: they are visible but cannot be spoken to.
	// Empty when huddled or when no one nearby is asleep. Same acquaintance gating
	// and ID-sorted order as CoPresent. ZBBS-WORK-426 (residual of HOME-436).
	CoPresentAsleep []HuddleMember

	// CoPresentResting are co-present RESTING actors (StateResting) partitioned out
	// of the awake CoPresent set when the subject is NOT in a huddle. A rester stays
	// in the shared audience (a PC can wake it), but THIS NPC's speech can't rouse it
	// (reactor.go actorCanReactNow gates NPC-to-NPC speech against a rester), so
	// Render groups it with CoPresentAsleep in the not-addressable clause rather than
	// the addressable "here with you" set. ZBBS-WORK-426.
	CoPresentResting []HuddleMember

	// Atmosphere is the village-wide ambient line authored by the atmosphere
	// cascade (Environment.Atmosphere — LLM-phrased by the cheap salem-generic
	// VA ~every 4h on phase transitions, NOT the v1 chronicler). Surfaced into
	// every NPC's perception so the ambient mood colors deliberation; it reuses
	// the already-generated string, so it adds no LLM call (ZBBS-WORK-327, the
	// v1 atmosphere perception line restored). Empty until the cascade first
	// fires (restart-lossy by design) → the render omits the line.
	Atmosphere string

	// Weather is the live village weather token (World.Environment.Weather —
	// "clear" | "storm"; a free string, additive for future fog/snow), copied
	// from Snapshot.Environment.Weather. renderSurroundings turns a storm into a
	// felt rain line (weatherProse) right beside the time-of-day line; clear /
	// empty render nothing. Unlike Atmosphere this is deterministic and always
	// current — no LLM call, no refresh lag — which is why it (not the atmosphere
	// line, WORK-374) is what puts the weather in front of a deciding NPC. LLM-364.
	Weather string

	// LocalMinuteOfDay is the village wall-clock minute (0–1439), copied from
	// Snapshot.LocalMinuteOfDay, or nil when the clock isn't established.
	// renderSurroundings turns it into a time-of-day prose line (timeOfDayProse).
	// v2 rendered no time at all, so an NPC couldn't tell its working hours from
	// the dead of night — the missing context HOME-352 builds on. ZBBS-HOME-351.
	LocalMinuteOfDay *int

	// GatherableItem / GatherableSource carry the harvest affordance cue
	// (ZBBS-WORK-328): when the actor is loitering at a gatherable source (a
	// well, a berry bush), GatherableItem is the item the source yields and
	// GatherableSource is its display name. Both empty when not at a source.
	// Render emits a "you can gather X here" line from these; gateTools reads
	// the SAME fields to advertise the `gather` tool — so the cue and the tool
	// can't drift (the pay-offer-warrant pattern). Computed by an asset-free
	// snapshot scan (findGatherableCue) — a permissive advertising heuristic;
	// the sim.Gather Command is the authoritative resolver at gather time.
	GatherableItem   sim.ItemKind
	GatherableSource string
	// GatherableNoun is the plural counting phrase for GatherableItem (LLM-113)
	// woven into the "you can gather X here" cue ("raspberries", not the raw
	// key). Empty when there is no cue; render falls back to the key string.
	GatherableNoun string
}

// DeadEndReason enumerates the live reasons the actor's CURRENT location can't
// serve the purpose it was approached for (LLM-154). It is a situated read, not
// a memory: each value is recomputed from the snapshot every tick, so it fires
// the moment the actor arrives and persists while they linger. Adding a reason
// is a new value here plus its detection in buildSurroundings and its clause in
// renderSurroundings — the mechanism is shared, the conditions slot in.
type DeadEndReason string

const (
	// DeadEndNone — the current location can serve the actor (or isn't the kind
	// of place that can be a dead end). The zero value, so an unset view reads
	// "fine".
	DeadEndNone DeadEndReason = ""
	// DeadEndShutBusiness — the actor is at a business (a structure someone
	// works) with no keeper tending it right now: no awake worker of the
	// structure is present at it. The live complement of the ObservedClosed
	// memory; the first wired reason (LLM-154).
	DeadEndShutBusiness DeadEndReason = "shut_business"
	// DeadEndNoConsumableHere — the actor feels a consumable need (hunger and/or
	// thirst), holds nothing that eases it, and there is no source for it at the
	// place it is standing (no co-present peer with a satisfier, no free source on
	// its tile, no vendor structure it is at). Names the dead end so a weak model
	// can't confabulate food where there is none ("I saw bread in the kitchen" at
	// a residence with no larder) and dead-end on consume() for an item it doesn't
	// hold. Which needs are unserved here is carried on SurroundingsView
	// (DeadEndHunger / DeadEndThirst) so the clause names eat vs drink. LLM-176.
	DeadEndNoConsumableHere DeadEndReason = "no_consumable_here"
)

// HasAudience reports whether the subject has at least one awake, addressable
// actor to speak to right now — its huddle peers, or (unhuddled) the co-present
// actors within earshot. CoPresentAsleep / CoPresentResting are deliberately
// excluded: this NPC's speech can't rouse them, so they are not an audience for
// it. This is the SAME set the dispatch-side "there is no one here to hear you"
// speak gate and the "## Around you" co-presence line derive from (HuddleMembers
// ∪ CoPresent via the colocatedAudienceIDs scope rule), so the advertised-tool
// gate, the rendered cue, and the substrate can't drift. ZBBS-WORK-407; LLM-106.
func (s SurroundingsView) HasAudience() bool {
	return len(s.HuddleMembers) > 0 || len(s.CoPresent) > 0
}

// TurnStateView is the subject actor's conversation turn-state, derived in
// Build from the directed awaiting-reply edges (ActorSnapshot.AwaitingReplyFrom)
// among its present huddle peers (ZBBS-WORK-370). Names are the acquaintance-
// gated labels (descriptorLabel), resolved at build time like OrderView's, so a
// line never leaks a name a shared-VA NPC shouldn't know.
//
// Only LIVE edges are surfaced: an edge older than the addressee-kind window
// (Snapshot.{PC,NPC}AwaitReplyWindow, measured against PublishedAt) is dropped,
// matching the sim.Speak backstop's expiry so the nudge and the gate agree on
// when a turn has lapsed.
type TurnStateView struct {
	// AwaitingReplyFrom names the present peers this actor has addressed and is
	// still awaiting a live reply from. Non-empty → render "you spoke to them,
	// wait for their reply, don't re-address them" AND swap the act-now coda for
	// a wait-framing (the cadence fix — stops the re-pitch loop).
	AwaitingReplyFrom []string

	// OwedReplyTo names the present peers awaiting a live reply FROM this actor —
	// it is this actor's turn to answer them. Rendered as a "they are waiting for
	// your reply" nudge so an addressed NPC takes its turn.
	OwedReplyTo []string

	// ConversationLooping is set when this actor's huddle is in an armed
	// conversational loop (sim.ActorSnapshot.ConversationLooping, LLM-169): the
	// members keep restating the same agreement without it converting to action.
	// True → suppress the "X is waiting for your reply" nag (that nag is what
	// manufactures the echo) AND swap the coda for an "you've agreed, act now or
	// done()" steer — the social-loop analogue of the LLM-160 seek-work directive.
	ConversationLooping bool

	// ConversationRunLong is ConversationLooping's endurance-arm sibling
	// (LLM-333, sim.ActorSnapshot.ConversationRunLong): the conversation has
	// exhausted its no-progress turn budget without reading as a lexical loop.
	// Same reply-nag suppression, but the coda renders a wind-down line — the
	// scene's truth is "this has gone on with nothing coming of it", not "you
	// keep saying the same thing". Never true alongside ConversationLooping.
	ConversationRunLong bool

	// ConversationLingering is the wind-down for a conversation that has merely
	// run long (LLM-397, sim.ActorSnapshot.ConversationLingering) — not stuck, not
	// starved of progress, just old. Same reply-nag suppression and the same
	// "bring it to a close" shape as ConversationRunLong, but the line must not
	// claim the talk has been fruitless: this is the arm that fires on the scene
	// where the innkeeper sold breakfast and then told a story about her husband.
	// Never true alongside either flag above.
	ConversationLingering bool
}

// AwaitingReply reports whether the subject is awaiting at least one live reply
// — the condition that swaps the act-now coda for a wait-framing.
func (t TurnStateView) AwaitingReply() bool { return len(t.AwaitingReplyFrom) > 0 }

// NeedRedirectKind discriminates how the looping-coda redirect resolves the
// felt need: consume what's carried, walk to a free source, or walk to a vendor
// and buy. The render picks the imperative from this (LLM-176).
type NeedRedirectKind string

const (
	NeedRedirectConsume NeedRedirectKind = "consume" // already carries a satisfier
	NeedRedirectFree    NeedRedirectKind = "free"    // walk to a free public source
	NeedRedirectBuy     NeedRedirectKind = "buy"     // walk to a vendor and buy
)

// NeedRedirectView is the concrete, single-target steer the looping coda renders
// for a need-driven huddle loop (LLM-176): it names ONE affordance the engine
// already knows resolves the actor's most-pressing felt consumable need, plus
// the imperative to act on it now rather than talk the plan over again. Built
// from Payload.Satiation (buildNeedRedirect) only while the actor is looping.
type NeedRedirectView struct {
	Kind NeedRedirectKind
	Verb string // "eat" | "drink" — the need's resolution verb

	// ItemLabel names the item: the carried satisfier to consume (Consume), or
	// the good to buy (Buy). Empty for Free.
	ItemLabel string

	// TargetLabel / TargetID name the place to move_to (Free / Buy). TargetID is
	// the move_to handle — a free source's object id or a vendor's structure id,
	// rendered as a (structure_id: …) the same way every actionable cue is. Both
	// empty for Consume.
	TargetLabel string
	TargetID    string
}

// HuddleMember is one co-huddle peer's identity slice for the
// SurroundingsView. Render emits DisplayName when Acquainted is true,
// otherwise falls back to Role ("the blacksmith") or a generic
// stranger label. Mirrors v1's coLocatedHuddleMembers name-vs-
// descriptor gating.
// laborTie names a co-present member's standing relationship to the subject —
// housemate or workmate — so the model reads it as kin/crew rather than a paid-work
// prospect. The zero value (laborTieNone) renders no annotation. Set only when the
// SUBJECT carries AttrWorker (subjectIsWorker) — a non-worker never solicits, so the
// note would be noise. LLM-157: the affordance gate (LLM-145) already hides the
// solicit_work tool among kin/crew, but the seek-work backstop warrant still nudges a
// broke worker to "seek out someone," which the model satisfies by asking the
// housemate standing in the room as freeform speech; naming the relationship heads
// that off. Household takes precedence when a peer is both.
type laborTie int

const (
	laborTieNone laborTie = iota
	laborTieHousehold
	laborTieWorkplace
)

// TravelerSelfView is the render-ready projection of the subject's own
// VisitorState for the self-identity preface (LLM-370). Name is the bare persona
// name (the visitor's DisplayName minus the " the <archetype>" suffix that
// dispatchVisitorSpawn composes); Archetype / Origin / Disposition come straight
// from VisitorState. Empty-string fields degrade the preface gracefully — a
// missing origin or disposition simply drops that clause.
type TravelerSelfView struct {
	Name        string
	Archetype   string
	Origin      string
	Disposition string
	// Vocation is the archetype's one-sentence calling (sim.VisitorVocation,
	// LLM-566), rendered after the origin/manner clause — what a circuit preacher
	// DOES rather than just the label. Empty for merchant-derived labels and
	// unknown archetypes; the preface drops the sentence.
	Vocation string
	// RoadWord is the grounded clause the traveler carries (VisitorState.Payload,
	// LLM-371) — "Ezekiel Crane turned out a plow for the Hale farm". Empty when
	// no carry-worthy beat was on hand at spawn; the preface drops the "Word
	// reached you …" line entirely in that case.
	//
	// ROAD word, not VILLAGE word, and the pair is deliberate (LLM-597):
	// Payload.VillageWord above is the fallible gossip layer (rumor.go —
	// escalates a rung per retelling, free to outgrow the truth), while this
	// travelled in from outside and is grounded and checkable. Same noun,
	// opposite reliability; the qualifier is what keeps them apart.
	RoadWord string
	// RoadWordSharedWith names the CO-PRESENT peers the traveler has already passed
	// the word to (VisitorState.PayloadSharedWith ∩ present company, LLM-545),
	// sorted. Someone he told elsewhere and who is not in the scene never
	// renders — the fact only matters face to face. RoadWordSpentWithAllPresent is
	// true when present company is non-empty and EVERY member has had the word;
	// the preface then reframes the word as a matter already spent rather than
	// fresh news, which is what stops a returning traveler reopening it. Both
	// zero-valued whenever Word is empty.
	RoadWordSharedWith          []string
	RoadWordSpentWithAllPresent bool

	// Returner continuity (LLM-372). Non-zero only for a returning traveler on a
	// repeat visit (ActorSnapshot.Returner set, VisitCount >= 2). VisitCount tiers
	// the "you have passed through Salem before" line; KnownHere names the players
	// the traveler remembers — most-recent first, with a coarse recency — so the
	// preface can voice a specific bond ("you know Sarah Hale here"). Empty for a
	// one-shot stranger or a first-visit traveler; the preface drops the block.
	VisitCount int
	KnownHere  []TravelerKnownPC
}

// TravelerKnownPC is one player a returner remembers, projected for the preface.
type TravelerKnownPC struct {
	Name    string
	Recency sim.RecencyTier
	// Summary is the returner's folded episodic impression of this player (LLM-383),
	// woven into the preface as remembered specifics. Empty before the first fold.
	Summary string
}

type HuddleMember struct {
	ID          sim.ActorID
	DisplayName string
	Role        string
	Acquainted  bool

	// Traveler marks a co-present transient traveler (salem-visitor). When set and
	// the observer does not yet know them by name (not Acquainted), "## Around you"
	// names them by archetype + origin ("a peddler lately come from Boston") in
	// place of the bare "a stranger" descriptorLabel gives an unacquainted roleless
	// actor — the observer half of the traveler legibility cue. TravelerArchetype /
	// TravelerOrigin carry the persona slots; empty archetype falls back to the
	// generic descriptor. LLM-370.
	Traveler          bool
	TravelerArchetype string
	TravelerOrigin    string

	// SolicitTie marks a co-present member the subject shares a household or
	// workplace with — kin/crew the subject (when a worker) should not solicit for
	// paid work. laborTieNone for everyone else. LLM-157.
	SolicitTie laborTie

	// JustArrived marks a co-present actor that reached its current spot within
	// the last coPresentJustArrivedWindow (ZBBS-WORK-422). Only meaningful for
	// CoPresent members — render surfaces the newcomer-greet beat a stateless
	// NPC can't infer from a bare co-presence list. Left false on HuddleMembers
	// (a huddle peer's arrival is already conveyed by the huddle-join).
	JustArrived bool

	// Laboring marks a co-present member fulfilling a hired job (a Working
	// LaborOffer / StateLaboring), set truthfully for EVERY observer. The seller
	// offer/quote cue (buildOfferableCustomers) drops a laboring peer as a pitch
	// target regardless of who is looking — a worker mid-job is not a sale target,
	// not even for their own employer. It does NOT drop them from the speech
	// audience: unlike a sleeper, a laboring worker can still answer if spoken to.
	// LLM-231.
	Laboring bool

	// LaboringBystander is true when Laboring holds AND the observer is not the
	// peer's own employer — the case the "## Around you" busy annotation renders
	// for. The employer is suppressed here: they get the richer "## Workers
	// currently working for you" cue instead, and naming the employer to themselves
	// reads wrong. False for non-laboring peers. LLM-231.
	LaboringBystander bool

	// LaboringForLabel is the acquaintance-gated label of the employer the member
	// works for, woven into the busy annotation ("working a job for X"). Set only
	// alongside LaboringBystander; empty when the observer is the employer or the
	// employer can't be resolved (render then omits the name). LLM-231.
	LaboringForLabel string

	// Eating marks a co-present member who is mid item-dwell (eating or drinking a
	// bought consumable at an eat-here source right now), set for EVERY observer.
	// "## Around you" renders it as a busy annotation ("eating porridge") so an
	// onlooker — a proprietor especially — reads a lingering diner as still at
	// their meal rather than as someone about to leave, and stops re-issuing
	// farewells at them (the LLM-416 Inn breakfast farewell storm). Sourced from
	// the peer's active item DwellCredits, so it clears the moment the meal
	// finishes. Unlike Laboring it does NOT gate the seller offer/quote cue (an
	// eater is still a sale target) and does NOT drop them from the speech audience.
	Eating bool

	// EatingItemLabel is the display label of what the member is eating, woven into
	// the annotation ("eating porridge"). Set only alongside Eating; empty falls
	// back to a bare "eating here" in render. LLM-416.
	EatingItemLabel string

	// SourceActivityBusy marks a co-present member mid a timed source activity —
	// mending a worn business, tending a hearth fire, or gathering at a source —
	// set for EVERY observer. "## Around you" renders it as a busy annotation
	// ("mending the market stall just now") so an onlooker reads a keeper deep in a
	// repair/stoke/gather as occupied rather than free to greet or pitch. The
	// observer half of the LLM-435 self-suppression, the same legibility beat
	// Eating (LLM-416) and Laboring (LLM-231) give their own states. Sourced from
	// the peer's BusyAtSource-gated SourceActivityKind projection, so it clears the
	// moment the window completes. Like Eating and unlike Laboring it does NOT gate
	// the seller offer/quote cue or drop the member from the speech audience — a
	// keeper mid-repair can still sell and still answer if spoken to. LLM-440.
	SourceActivityBusy bool
	SourceActivityKind sim.SourceActivityKind

	// SourceActivityLabel is the display label of the place the activity is bound to
	// (the business being mended, the hearth's structure), woven into the repair
	// annotation ("mending the market stall"). Set only alongside SourceActivityBusy;
	// empty falls back to a place-less phrase in render. LLM-440.
	SourceActivityLabel string

	// ContactTier is what the observer's own history with this member says
	// (LLM-547) — silent when they have not spoken inside the recall horizon,
	// continuity when they have but not lately, and one of two brake tiers when
	// they have spoken recently. Derived world-side from the contact ledger so
	// render only selects phrasing.
	//
	// Unlike every other annotation on this struct it is OBSERVER-RELATIVE: two
	// actors looking at the same third party see different tiers, because the
	// fact is about the pair rather than about the member.
	ContactTier sim.ContactTier

	// ContactRecentCount is how many times they have spoken inside the brake
	// window, carried so the weighted line can say "twice" without re-deriving a
	// count that could drift from the tier. Zero below the brake tiers.
	ContactRecentCount int
}

// AnchorsView carries the actor's own home and work structures as standing
// move_to targets. Label is the structure's DisplayName (may be empty — render
// falls back to a generic phrase but always keeps the id); the id is the
// load-bearing field the model passes to move_to. SamePlace is true when home
// and work resolve to the same structure (a keeper who lives at their own
// establishment), so render says it once rather than naming the same place
// twice. A zero-value field (empty id) means that anchor isn't set.
type AnchorsView struct {
	WorkLabel string
	WorkID    sim.StructureID
	HomeLabel string
	HomeID    sim.StructureID
	SamePlace bool
}

// DutySteerView is the standing return-to-post / wind-down cue (ZBBS-HOME-352,
// reframed by ZBBS-WORK-387, AtPost added ZBBS-WORK-431). ToWork / AtPost / the
// off-shift wind-down are the three mutually-exclusive shapes. On the
// off-shift wind-down side the target is housing-dependent: the actor's own home,
// a lodger's rented inn room (Lodging), or — for a homeless keeper — no fixed
// place at all (TargetID == "", a directionless "find your rest" nudge;
// recovery_options carries the where). For a keeper standing at its own post the
// cue also surfaces the stay_open choice (OfferStayOpen). See buildDutySteer.
type DutySteerView struct {
	ToWork      bool // true = on-shift, head to work; false = off-shift, wind down for the night
	TargetID    sim.StructureID
	TargetLabel string // resolved DisplayName; may be empty (render falls back). Empty TargetID on an off-shift cue = homeless (no fixed place).

	// AtPost is the symmetric complement to the to-work yank (ZBBS-WORK-431):
	// on-shift and standing at your own work post. It renders the "stay put,
	// don't wander" stabilizer and reframes the anchors departure-invite, but is
	// RENDER-ONLY — deliberately excluded from the noop-skip gate
	// (shouldSkipNoop) so an idle at-post NPC with no stimulus still skips its
	// idle-backstops (HOME-441); the line only renders on ticks that already run.
	// No target: the actor is already there. Mutually exclusive with ToWork and
	// the off-shift wind-down fields.
	AtPost bool

	// AwayFromPost is the STATUS-ONLY arm: on-shift and away from your own post, but
	// legitimately so (an errand, a live transaction, a satiation source reached).
	// It states the shift fact where the to-work yank is suppressed — without it the
	// prompt carries an ambient hour and nothing tying it to this actor's shift, and
	// the model invents one (LLM-620).
	//
	// Two constraints, both load-bearing: carries NO destination id, or it becomes
	// the yank it replaced (HOME-349 — that token is what the model echoes into
	// move_to); and RENDER-ONLY like AtPost, excluded from shouldSkipNoop so it
	// cannot force an idle tick that used to skip (HOME-441). Mutually exclusive
	// with ToWork.
	AwayFromPost bool

	// ShiftEndMin is the keeper's effective close time as a wall-clock
	// minute-of-day (0–1439) — its own schedule end, else the day-active dusk
	// fallback (shiftWindowBounds). Set on the AtPost cue (LLM-40) and on the
	// AwayFromPost status arm, which states the same close time for the same
	// reason: the shift's end is what makes the ambient hour mean something. On
	// AtPost it makes "stay open later" a bounded decision rather than a vague
	// diligence reflex (the model otherwise reached for stay_open with no customer
	// to serve and no sense of how near close was); on AwayFromPost it is the anchor
	// that stops the hour being re-invented. nil → render omits the close-time
	// clause. Render voices it via sim.ClockHourProse.
	ShiftEndMin *int

	// ForageErrand modifies the AtPost stabilizer for a grower-seller who also has
	// an actionable forage errand this tick (ForageView.ActionableErrand — a bare
	// sell-shelf plus ripe stock somewhere the cue names, and NOT mid-customer, since
	// buildForage defers the harvest cue while a customer is engaged at the stall). It
	// flips the stabilizer's default "stay and look after your work" steer for a
	// "step out and return" line, so the at-post cue AGREES with the forage section
	// instead of pinning her against it (LLM-90). Only meaningful when AtPost; the
	// to-work arm separately defers a forage errand so she isn't yanked back mid-trip.
	//
	// The KIND picks the wording, because the two halves of ForageView are not the
	// same property (LLM-622): Items are the grower's own owner-gated bushes, while
	// WildSources are unowned commons anyone may draw from. One reframe line for both
	// told a miller to step out to "your own bushes" directly above a section saying
	// nobody owns the well he was being sent to.
	ForageErrand ForageErrandKind

	// SupplyErrand is the BUY-side twin of ForageErrand (LLM-491): set when some
	// other section of this same payload hands the at-post subject an off-scene
	// place to go buy at — a "## Restocking" walk-to supplier, the nails to mend a
	// worn business, the shovels the season owes, the firewood for a cold hearth.
	// Every one of those renders a "(destination: <id>)" the model is meant to walk
	// to, while the default stabilizer in the same prompt says stay and look after
	// your work. Live 2026-07-20: Josiah Thorne, at the General Store, was told to
	// stay put and simultaneously handed James Farm's destination id for the wheat
	// he was low on. Render flips the stabilizer to a step-out-and-return line so
	// the two agree, exactly as ForageErrand already does for the harvest cue.
	//
	// Derived from the assembled payload AFTER the leisure/summons/degeneracy
	// subtractions have run (Build), not from the pre-thinning views: those blocks
	// nil the very sections this reconciles with, and a stabilizer reframed for an
	// errand cue that no longer renders is the "told to move" residue the thinning
	// exists to remove. Only meaningful when AtPost; ForageErrand wins when both
	// are set (its line is the more specific one — it names the stock she goes to
	// take rather than the thing she goes to buy).
	SupplyErrand bool

	// Lodging marks the off-shift target as the actor's RENTED room at an inn
	// (a lodger) rather than its own home — render says "head to your rented room
	// at X". Only meaningful when !ToWork && TargetID != "". ZBBS-WORK-387.
	Lodging bool

	// OfferStayOpen surfaces the stay_open choice on an off-shift wind-down cue,
	// set only for a keeper standing at its own business (the close-or-stay-open
	// moment). When StayOpenReason is non-empty the cue ACTIVELY encourages
	// staying open (a concrete reason — owed order / co-present buyer / pending
	// offer); otherwise it is offered as a discretionary option. ZBBS-WORK-387.
	OfferStayOpen  bool
	StayOpenReason string
}

// EveningLeisureView is the evening leisure cue (LLM-149). VenueID/VenueLabel
// name the nearest tavern-tagged venue, resolved from the snapshot (never a
// hardcoded id) — the "tavern" venue tag lives on the VillageObject, whose id
// also names a Structure via the shared-identity bridge (the same idiom
// pickVisitorDestination uses). HomeID/HomeLabel carry the co-equal stay-home
// option so the cue is self-sufficient — it does not depend on the anchors line
// rendering the home id, matching the duty steer's inline-structure_id
// convention. See buildEveningLeisure / renderEveningLeisure.
//
// BatchHold is the LLM-335 variant: a keeper standing at its post with a batch
// in the works is pinned there (the LLM-319 pause model), so the tavern
// invitation would contradict the standing "you are making a batch of X" line.
// The view then carries only BatchHold + BatchItemLabel and render writes a
// quiet diegetic hold instead of the invitation; the venue/home fields are
// unused. Unlike the invitation this variant is render-only — it does NOT hold
// the noop-skip gate open (noop_skip.go), so a busy keeper still skips idle
// no-op ticks, mirroring the at-post duty-steer exclusion.
//
// SettledIn is the LLM-345 variant: the agent crossed the venue threshold and is
// passing its evening there. The invitation used to simply vanish at the door
// ("already at the tavern → acted on"), which left the day's standing work content
// as the loudest thing on the page — and under the render.go coda, which ranks
// obligations above "idle matters", the scene argued the lingerer straight back out
// (live: Elizabeth Ellis, ninety seconds in the Tavern reading her shovel inventory).
// The view then carries only SettledIn + VenueLabel and render writes the room
// itself, with no destination to walk to. Render-only like BatchHold, and for the
// same reason: an agent alone in the tavern with nothing to act on should skip its
// idle no-op ticks rather than re-deliberate its way out the door every tick.
type EveningLeisureView struct {
	VenueID    sim.StructureID
	VenueLabel string // resolved venue DisplayName; render falls back to "the tavern"
	HomeID     sim.StructureID
	HomeLabel  string // resolved home DisplayName; render falls back to "your home"

	BatchHold      bool   // LLM-335: pinned by an in-flight batch at post — render the hold, not the invitation
	BatchItemLabel string // the batch good's display label, for the hold line ("the batch of Cheese …")

	SettledIn bool // LLM-345: inside the venue, passing the evening — render the room, not the invitation
}

// Invitation reports whether the view is the movement invitation ("the tavern's
// open of an evening — you might head over") rather than one of the render-only
// variants. Only the invitation is a standing actionable signal, so only it holds
// the noop-skip gate open in the suppressed go-home steer's place (noop_skip.go);
// BatchHold and SettledIn are scenes the agent has nothing to act on, and forcing
// a tick for them would restore the constant idle deliberation each was added to
// remove. Nil-safe so callers can test a possibly-absent view in one expression.
func (v *EveningLeisureView) Invitation() bool {
	return v != nil && !v.BatchHold && !v.SettledIn
}

// SceneView describes the primary scene and, when a baseline could be
// established, the actor's diff against that scene's origin snapshot.
type SceneView struct {
	SceneID    sim.SceneID
	OriginKind string
	OriginAt   time.Time

	// Warrants are the consumed warrants that reference this scene,
	// ordered by SourceEventID ascending. May be empty when the primary
	// scene was resolved from the actor's active huddle rather than from a
	// scene-bearing warrant.
	Warrants []sim.WarrantMeta

	// Diff is the actor's change since the scene's origin snapshot. Set
	// iff the enclosing Payload's Baseline == BaselinePresent; nil
	// otherwise (the Missing* statuses all mean "no diff").
	Diff *Diff
}

// SceneSignal is a secondary scene referenced by the warrant batch — a
// scene other than the primary. It carries no baseline diff by design:
// the primary scene's origin snapshot says nothing about a different
// scene, and reverse-resolving a baseline per secondary scene would
// multiply the cost and the failure modes for marginal value.
type SceneSignal struct {
	SceneID  sim.SceneID
	HuddleID sim.HuddleID
	Warrants []sim.WarrantMeta
}

// Diff is the subject actor's change between a scene's origin snapshot and
// its current snapshot. It is the loop-detection seam: AnyChange == false
// across several consecutive ticks is the "this actor is stuck" signal.
// Every field is only meaningful when the enclosing Baseline is
// BaselinePresent.
type Diff struct {
	StateChanged     bool
	PositionChanged  bool
	StructureChanged bool
	HuddleChanged    bool
	CoinsChanged     bool
	InventoryChanged bool
	NeedsChanged     bool

	// AnyChange is the OR of every field above — the single value loop
	// detection reads.
	AnyChange bool
}
