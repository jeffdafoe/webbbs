package perception

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// RenderConfig holds the deterministic limits the prompt renderer enforces.
// Every limit is a hard cap applied after deterministic ordering, so the
// same Payload + RenderConfig always produce the same RenderedPrompt and
// the same DroppedWarrants set.
//
// Any field left <= 0 falls back to its DefaultRenderConfig value — the
// same "<= 0 means default" convention the engine's WorldSettings use.
type RenderConfig struct {
	// MaxWarrants is the most warrants rendered into the "what just
	// happened" section. Warrants past the cap are dropped (carried
	// forward), not silently consumed.
	MaxWarrants int

	// MaxBytesPerWarrant caps the untrusted free-text payload of a single
	// warrant (e.g. a speech excerpt). Text past the cap is truncated with
	// a marker; the warrant is still rendered.
	MaxBytesPerWarrant int

	// MaxSectionBytes caps the total byte size of the rendered warrant
	// section. Once a warrant would push the section past the cap, that
	// warrant and every warrant after it are dropped (carried forward).
	MaxSectionBytes int
}

// DefaultRenderConfig returns the baseline limits. These are mechanism
// defaults — sized to keep the prompt bounded, not tuned for final prompt
// content (content fills in incrementally in later work).
func DefaultRenderConfig() RenderConfig {
	return RenderConfig{
		MaxWarrants:        12,
		MaxBytesPerWarrant: 600,
		MaxSectionBytes:    4000,
	}
}

// normalized returns a copy with every <= 0 field replaced by its default.
func (c RenderConfig) normalized() RenderConfig {
	d := DefaultRenderConfig()
	if c.MaxWarrants <= 0 {
		c.MaxWarrants = d.MaxWarrants
	}
	if c.MaxBytesPerWarrant <= 0 {
		c.MaxBytesPerWarrant = d.MaxBytesPerWarrant
	}
	if c.MaxSectionBytes <= 0 {
		c.MaxSectionBytes = d.MaxSectionBytes
	}
	return c
}

// RenderedPrompt is the output of Render: the prompt text plus the
// accounting the harness loop needs.
type RenderedPrompt struct {
	// Text is the DURABLE turn — the "since your last turn" events, what the NPC
	// should REMEMBER. This is what the chat adapter persists and replays as
	// conversation history (lean sim-history, ZBBS-WORK-364). Self-state (## You)
	// was moved OUT of here into EphemeralText by ZBBS-WORK-410 — it is point-in-
	// time and a prior tick's stale "Coins in your purse: 0" was replaying as if
	// it were the actor's current balance.
	Text string

	// EphemeralText is per-tick decision-support that must NOT persist into
	// history: the ## You self-state (coins/needs/carried goods, ZBBS-WORK-410),
	// surroundings, affordances (rest/food/lodging), owed orders, pay
	// offers, and the act-now coda. The adapter attaches it to the CURRENT turn
	// only (memory-api: /chat/send ephemeral_context). Splitting it out keeps
	// replayed history lean — neither the static furniture nor the stale self-
	// state can pile up once per historical tick.
	EphemeralText string

	// StableText is the per-actor DAILY-stable identity context — the
	// "## Who you are" name line + synthesized soul prose (shared villagers
	// only; a stateful NPC's identity lives in its VA system prompt and this is
	// empty for it). The adapter puts it in the provider-CACHED zone (memory-
	// api: /chat/send stable_context → appended to the system prompt), never in
	// the volatile user turn and never into persisted history (LLM-501).
	//
	// Why its own stream: the soul is the largest stable chunk a shared NPC
	// carries (a synthesized memoir can run 1k+ tokens) and its bytes change at
	// most nightly, but riding the ephemeral stream it re-billed COLD on every
	// call — provider prefix caching dies at the first volatile byte, and the
	// ephemeral body opens with needs/coins. In the system zone it forms a
	// per-actor stable prefix the provider cache holds all day.
	//
	// This stream also retires LLM-468's ContinuationEphemeralText (the
	// rounds-after-the-first soul drop): every round now carries the soul in
	// the cached zone at warm prices, so there is nothing left to strip.
	StableText string

	// RenderedWarrantCount is how many warrants made it into the prompt.
	RenderedWarrantCount int

	// TruncatedWarrants is how many rendered warrants had their free-text
	// payload truncated by MaxBytesPerWarrant. They were still rendered —
	// this is a quality signal, not a drop.
	TruncatedWarrants int

	// DroppedWarrants are warrants that were consumed by the tick but did
	// not fit under MaxWarrants / MaxSectionBytes. They MUST be carried
	// forward — the harness loop puts them in TickResult.UnaddressedWarrants
	// so CompleteReactorTick re-opens them. Dropping them silently would
	// recreate the "consumed but never addressed" state the warrant system
	// exists to eliminate.
	DroppedWarrants []sim.WarrantMeta
}

// Render turns a Payload into a prompt string. It is a pure function:
// deterministic ordering (already applied in Build) is preserved, the
// caps in cfg are applied after ordering, and dropped warrants are
// surfaced for carry-forward rather than discarded.
//
// PR 3c ships the rendering *mechanism* — section structure, escaping of
// untrusted text, the deterministic caps, and the drop→carry-forward
// path. The prompt *content* (the exact prose, the persona framing, the
// tool-schema block) fills in incrementally; this is intentionally a
// plain, structured rendering.
func Render(p Payload, cfg RenderConfig) RenderedPrompt {
	cfg = cfg.normalized()

	var out RenderedPrompt
	// Two streams (lean sim-history, ZBBS-WORK-364). `durable` is the "what just
	// happened" events — what the NPC should REMEMBER; the chat adapter persists
	// and replays it as conversation history. `ephemeral` is per-tick decision-
	// support (self-state, identity, surroundings, affordances, owed orders, pay
	// offers, the act-now coda) the adapter attaches to the CURRENT turn only and
	// never persists, so the static furniture can't accumulate once per historical
	// tick. The split is by SECTION — each renderer below is routed to one stream.
	var durable strings.Builder
	var ephemeral strings.Builder
	// stable carries the daily-stable identity sections (see
	// RenderedPrompt.StableText) — routed to the provider-cached zone by the
	// adapter, so nothing volatile may ever be written to it (LLM-501).
	var stable strings.Builder

	// Self-state (## You: coins, felt needs, carried goods) is per-tick decision-
	// support, NOT durable memory — it is point-in-time and goes stale the instant
	// the tick ends. It rides the EPHEMERAL stream (and is prepended to the post-
	// speak continuation body below), so it shows on every round of the CURRENT
	// tick but never enters the persisted/replayed history. When it was durable, a
	// prior tick's "Coins in your purse: 0" replayed as if current and the NPC
	// behaved as though broke (ZBBS-WORK-410). Rendered once, reused for both
	// ephemeral bodies.
	var selfState strings.Builder
	renderActor(&selfState, p.Actor)

	// Durable: a transient traveler's own persona opens the message ahead of the
	// turn header (LLM-370) — durable leads the assembled prompt
	// (harness.fullPerceptionPrompt), so this is the first thing the stateless
	// salem-visitor VA reads, framing the whole turn in-character. Nothing for a
	// non-traveler subject (SelfTraveler nil).
	renderTravelerPreface(&durable, p.SelfTraveler)
	// Durable: just the turn header here; the "since your last turn" events append
	// below (## You is ephemeral now — ZBBS-WORK-410).
	durable.WriteString("# Your turn\n\n")

	// nameOf resolves an actor UUID to the subject's name for them — "you" for
	// self, the acquaintance-gated label (Build's WarrantActorNames) for
	// others, "someone" when unresolvable. The fix for warrant lines leaking
	// raw UUIDs ("[arrived] involving 019da6af…"). ZBBS-HOME-339.
	nameOf := func(id sim.ActorID) string {
		if id == "" {
			return "someone"
		}
		if id == p.ActorID {
			return "you"
		}
		if label, ok := p.WarrantActorNames[id]; ok && label != "" {
			return sanitizeInline(label)
		}
		return "someone"
	}

	// placeNameOf resolves a destination id (structure or village object) named
	// by an arrival warrant to its display name, "" when unresolvable — the
	// counterpart to nameOf for the "You arrived at <place>" line (ZBBS-WORK-358).
	placeNameOf := func(id string) string {
		if id == "" {
			return ""
		}
		return sanitizeInline(p.WarrantPlaceNames[id])
	}

	// placeKeeperOf resolves an arrived-at structure id to its keeper's display
	// name, "" when the structure has no keeper other than the arriver — the
	// possessive counterpart to placeNameOf that lets the arrival line read "You
	// arrived at <keeper>'s <place>" (LLM-284).
	placeKeeperOf := func(id string) string {
		if id == "" {
			return ""
		}
		return sanitizeInline(p.WarrantPlaceKeepers[id])
	}

	// eatHereKind reports whether a kind always settles eat-here (Build's
	// EatHereKinds set, ZBBS-WORK-405) — the quote warrant line states the
	// disposition fact so the model never plans a carry-out it can't have.
	eatHereKind := func(kind sim.ItemKind) bool {
		return p.EatHereKinds[kind]
	}

	// buyRedundancy reports, for a quoted item, whether the buyer MAKES it itself
	// (produced) or already holds it at cap (atCap) — LLM-171. renderWarrantLine
	// uses it to strip the actionable take from a buy-quote whose every line is
	// redundant, so a co-present seller's mis-pitched quote can't drive the buyer
	// to buy back its own ware or overflow its carry.
	buyRedundancy := func(kind sim.ItemKind) (produced, atCap bool) {
		return p.OwnProducedKinds[kind], p.AtCapKinds[kind]
	}

	// Pay offers render as an actionable decision section (renderPayOffers)
	// so the seller gets the ledger_id it must echo into accept_pay/
	// decline_pay/counter_pay. Sourced from the standing ledger scan
	// (Payload.PayOffersForMe, ZBBS-HOME-453), NOT the consumed warrant
	// batch — the offer warrant only wakes the seller's first tick, and a
	// seller who speaks through that tick must keep seeing the offer until
	// it resolves or expires. The same PendingPayOffers(p) predicate drives
	// the handlers tool-gate (gateTools), so the rendered offer and the
	// advertised response tools cannot drift. Rendering them in a dedicated,
	// uncapped section (rather than as a capped warrant line) guarantees the
	// ledger_id is present whenever the tools are advertised.
	payOffers := PendingPayOffers(p)

	// Ephemeral: self-state first (ZBBS-WORK-410), then identity, surroundings,
	// anchors, steers, relationships, the offers awaiting this actor's decision,
	// owed orders, recovery/satiation/restock/lodging affordances, summons, scene.
	ephemeral.WriteString(selfState.String())
	renderLaborSelfState(&ephemeral, p.Laboring, nameOf, p.RenderedAt)
	// LLM-229: the pre-work leg — a worker who accepted a job and is relocating
	// to (or waiting at) the employer's workplace. Mutually exclusive with the
	// in-progress line above (a worker is either relocating or working).
	renderLaborEnRoute(&ephemeral, p.LaborEnRoute, nameOf)
	// LLM-202: the employer-side mirror — workers currently on a job for this
	// actor, so they don't re-hire or pay again for work already covered.
	renderWorkersForMe(&ephemeral, p.WorkersForMe, nameOf, p.RenderedAt)
	renderPendingLaborOfferOut(&ephemeral, p.PendingLaborOfferOut, nameOf)
	// Stable: the identity block renders to its own stream (LLM-501) — the
	// adapter routes it to the provider-cached system zone, so the memoir
	// stops re-billing cold inside the volatile per-tick body.
	renderNarrativeState(&stable, p.NarrativeState)
	renderVendorOperating(&ephemeral, p.AtOwnBusinessOperating, p.VendorTradeSlow)
	renderKeeperAwayFromPost(&ephemeral, p.KeeperAwayFromPost)
	renderSurroundings(&ephemeral, p.Surroundings)
	renderAnchors(&ephemeral, p.Anchors, p.DutySteer != nil && p.DutySteer.AtPost, p.Surroundings.InsideStructureID)
	renderDutySteer(&ephemeral, p.DutySteer)
	renderEveningLeisure(&ephemeral, p.EveningLeisure)
	renderBakeChoice(&ephemeral, p.BakeChoice)
	renderRelationships(&ephemeral, p.Relationships)
	// LLM-572: the counted record, immediately after the impression distilled from
	// it. The adjacency IS the fix — a money claim and the ground truth about that
	// money have to be readable in one breath, or the more confident account wins.
	renderCoinDealings(&ephemeral, p.CoinDealings)
	// LLM-387: gossip the actor carries about people NOT in the scene — the
	// word-of-mouth layer's read surface. Sits beside "what you remember of those
	// here" (its co-present twin) but is about the ABSENT, and is framed as
	// fallible talk rather than a faithful readout.
	renderVillageWord(&ephemeral, p.VillageWord)
	// LLM-217: the subject's own recent deeds render just above the spoken
	// turns — together they are the actor's short-term memory of the scene,
	// and the action trail is what makes a self-loop (leave ↔ bounce back)
	// visible to the model.
	renderSelfActions(&ephemeral, p.SelfActions, p.RenderedAt)
	renderRecentConversation(&ephemeral, p.RecentConversation, p.RenderedAt)
	// The decision section renders ABOVE the affordance dumps (it used to land
	// after them): a buyer's coin on the table is the seller's most actionable
	// fact, and burying it under eat/drink and room-to-let cues let the
	// seller's own mild needs outrank a waiting customer for whole minutes
	// (conversation hud-6c849d…, ZBBS-HOME-424). renderTriage reinforces the
	// same priority at the decision point.
	renderPayOffers(&ephemeral, payOffers, nameOf, p.PayOfferShortfalls, p.RoomAlreadySoldOrderByLedger, p.PayOfferWorth)
	// LLM-138: a gift offered TO this actor is the same "someone wants my answer"
	// decision class as a pay offer, so it renders right alongside.
	renderGiftsForMe(&ephemeral, p.GiftsForMe)
	// LLM-26: the employer's pending work-offer decisions sit alongside pay
	// offers (both are "someone wants my answer"); the worker affordance cue
	// follows so a free worker sees the option to offer their labor.
	renderLaborOffers(&ephemeral, p.LaborOffersForMe, p.Actor.Coins, p.SubjectProducesGoods, nameOf)
	renderLaborAffordance(&ephemeral, p.CanSolicitWork, p.SolicitableEmployers, nameOf)
	// LLM-346: the hiring-side twin of the affordance above. Sits immediately after
	// it so the two mints of the labor market read as one pair — offer your labor,
	// or ask for someone else's.
	renderOfferWorkAffordance(&ephemeral, p.HireableWorkers, nameOf)
	// LLM-152/160: the directional half of seek-work — the town's businesses to head
	// to, by their resolvable names. Sits with the labor affordance; non-empty
	// whenever the subject is a broke idle worker with no employer present to solicit
	// (a STANDING cue, see the build-side gate), so move_to always has a real target.
	renderSeekWorkPlaces(&ephemeral, p.SeekWorkPlaces)
	renderOfferableCustomers(&ephemeral, p.OfferableCustomers)
	renderTradeValue(&ephemeral, p.TradeValue)
	renderStandingQuotesFromMe(&ephemeral, p.StandingQuotesFromMe)
	// LLM-409: the resolution twin of the standing-offers cue — a lot the seller
	// spent the goods out from under, just flipped to shortfall. Sits right after
	// so "still standing" and "fell through" read as a pair.
	renderUncoverableOffersFromMe(&ephemeral, p.UncoverableOffersFromMe)
	// LLM-551: the buyer-side twin of the standing-quotes cue above — offers
	// posted in THIS subject's name that are still hers to take. Sits with the
	// seller pair so the three read as one ledger of open quotes: what I've put
	// out, what fell through, what has been put to me.
	renderStandingQuotesToMe(&ephemeral, p.StandingQuotesToMe, buyRedundancy)
	renderPendingDeliveriesFromMe(&ephemeral, p.PendingDeliveriesFromMe, p.LocalDateUTC, p.RenderedAt)
	renderPendingDeliveriesToMe(&ephemeral, p.PendingDeliveriesToMe, p.LocalDateUTC, p.RenderedAt)
	renderPendingOffersFromMe(&ephemeral, p.PendingOffersFromMe)
	renderRecentlyResolvedOffersFromMe(&ephemeral, p.RecentlyResolvedOffersFromMe)
	// LLM-138: the giver-side gift counterparts — own gifts still standing, then
	// own gifts just settled.
	renderGiftsFromMe(&ephemeral, p.GiftsFromMe)
	renderSettledGiftsFromMe(&ephemeral, p.SettledGiftsFromMe)
	renderCountersAwaitingMyResponse(&ephemeral, p.CountersAwaitingMyResponse)
	renderRecoveryOptions(&ephemeral, p.RecoveryOptions)
	renderSatiation(&ephemeral, p.Satiation)
	renderProductionInputs(&ephemeral, p.ProductionInputs)
	renderForgeChoice(&ephemeral, p.ForgeChoice)
	renderStallRepair(&ephemeral, p.StallRepair)
	renderStallCondition(&ephemeral, p.StallCondition)
	renderStallRepairBuy(&ephemeral, p.StallRepairBuy)
	renderHearth(&ephemeral, p.Hearth)
	renderHearthCooking(&ephemeral, p.HearthCooking)
	renderFarmUpkeep(&ephemeral, p.FarmUpkeep)
	renderWorkClothes(&ephemeral, p.WorkClothes)
	renderTownRate(&ephemeral, p.TownRate)
	renderRestocking(&ephemeral, p.Restocking)
	renderForage(&ephemeral, p.Forage)
	renderLodging(&ephemeral, p.Lodging)
	// LLM-447: the evening's exit — the single bedtime cue.
	renderTurnInChoice(&ephemeral, p.TurnInChoice)
	renderKeeperLodging(&ephemeral, p.KeeperLodging)
	renderKeeperHeldLodgers(&ephemeral, p.KeeperHeldLodgers)
	renderLodgingOffer(&ephemeral, p.LodgingOffer)
	// Traveler day-plan cues (LLM-373): rounds framing by day, the seek-a-bed
	// booking cue of an evening. At most one fires per turn (day vs evening phase).
	renderTravelerRounds(&ephemeral, p.TravelerRounds)
	// Merchant errand keeper-facing cue (LLM-455): the counterparty keeper's "a trader's come
	// to deal" surface (buy or sell). The trader's own errand steer is folded into the rounds cue.
	renderErrandVisit(&ephemeral, p.ErrandVisit)
	// Equipment service (LLM-648): the owner's due-gear standing fact (+ the
	// act-now imperative when a wright is co-present) and the wright's own
	// rounds steer.
	renderEquipmentService(&ephemeral, p.EquipmentService)
	renderWrightRounds(&ephemeral, p.WrightRounds)
	renderTravelerSeekBed(&ephemeral, p.TravelerSeekBed)
	renderSummonsForYou(&ephemeral, p.SummonsForYou)
	renderSummonRefusal(&ephemeral, p.SummonRefusal)
	renderScene(&ephemeral, p)
	// "## Other scenes in play" (renderSecondary) was dropped — it surfaced raw
	// scene/huddle UUIDs and a "N signal(s)" count the LLM can't act on
	// (ZBBS-HOME-339). Secondary-scene warrants still render in the flat
	// "since your last turn" list; only the machine telemetry block is gone.

	// Some warrants drive the wake tick but are NOT rendered, because a standing
	// cue above is already the single voice for the thing they woke him about.
	// Filtering here also keeps them out of the cap / carry-forward budget;
	// consuming them unrendered is fine since their job is to wake the actor,
	// which the tick already did.
	warrants := nonStandingCueWarrants(p.Warrants)
	if len(payOffers) > 0 {
		warrants = nonPayOfferWarrants(warrants)
	}
	// Durable: the "since your last turn" events are the NPC's memory of the
	// scene. Skip the generic block only when the pay-offer section already
	// covered the whole batch; otherwise render it (this also preserves the
	// routine-check-in line for the genuinely-empty case). Warrant caps +
	// carry-forward accounting land in `out` as before.
	if len(warrants) > 0 || len(payOffers) == 0 {
		renderWarrants(&durable, warrants, nameOf, placeNameOf, placeKeeperOf, eatHereKind, buyRedundancy, p.RenderedAt, cfg, &out)
	}

	// Ephemeral: the turn-state nudge, the act-now coda, and the rest-first steer
	// are instructions for THIS tick, not facts to remember. The turn-line lands
	// before the coda so the coda's "weigh everything above" sees it; the coda
	// itself swaps to a wait-framing when the actor is awaiting a reply.
	// LLM-160: a populated SeekWorkPlaces means a workless worker with no employer
	// present — the directive is "leave for a business". That overrides the
	// conversational reply-pressure (suppress the owed-reply nag) and swaps the coda
	// to a decisive go-line, so the model stops agree-looping and actually moves.
	seekWorkDirective := len(p.SeekWorkPlaces) > 0
	// LLM-169: a looping huddle (members re-echoing a settled agreement) ALSO
	// suppresses the owed-reply nag — that nag is exactly what manufactures the
	// echo — while renderTriage's coda swaps to an "act now or done()" steer below.
	conversationLooping := p.TurnState.ConversationLooping
	// LLM-333: the endurance wind-down suppresses the owed-reply nag for the same
	// reason the loop steer does — reply-pressure is what keeps the over-long
	// conversation alive.
	conversationRunLong := p.TurnState.ConversationRunLong
	// LLM-397: a conversation that has merely run long suppresses the owed-reply
	// nag too. The nag is reply-pressure, and reply-pressure is what keeps a scene
	// that should be ending alive for another beat.
	conversationLingering := p.TurnState.ConversationLingering
	// LLM-416: an actor mid item-dwell (eating a bought meal at an eat-here source)
	// is mechanically pinned — its dwell self-cue says leaving now wastes the food
	// and leaves it hungry (DwellStayClause). The run-long and lingering codas below
	// tell it to bring the talk to a close / turn to its own affairs — advice it
	// cannot follow, so it emits a fresh farewell every tick instead of eating in
	// silence (the live Inn breakfast farewell storm, 2026-07-15). Suppress those
	// two leave codas while pinned so the coda falls through to the wait/decision
	// codas, which permit a silent done(). Item dwells only: object dwells (well,
	// shade tree) are open-ended with no waste-on-leave pin, so an actor resting
	// there may still be wound down. The owed-reply-nag suppression below keeps the
	// ungated flags — a pinned eater should get no reply pressure either.
	midItemDwell := false
	for _, c := range p.Actor.ActiveDwellCredits {
		if c.Source == sim.DwellSourceItem {
			midItemDwell = true
			break
		}
	}
	// Named locals so the gate reads clearly at the call. Only the two leave codas
	// are gated; a pinned eater then falls through to the awaiting-reply coda (itself
	// wait-permitting: "do not repeat yourself — wait and call done()") or the default
	// decision coda — neither of which tells it to leave. AwaitingReply() is passed
	// through UNgated on purpose: gating it would demote a spoke-then-eating actor
	// from that anti-repeat line down to the weaker default coda.
	triageRunLong := conversationRunLong && !midItemDwell
	triageLingering := conversationLingering && !midItemDwell
	renderTurnState(&ephemeral, p.TurnState, seekWorkDirective || conversationLooping || conversationRunLong || conversationLingering)
	renderTriage(&ephemeral, p.Actor.Needs, p.Actor.NeedThresholds, p.TurnState.AwaitingReply(), conversationLooping, triageRunLong, triageLingering, p.NeedRedirect, seekWorkDirective, len(payOffers) > 0, p.Actor.InFlightMove, p.Actor.InFlightSourceActivity)

	out.Text = durable.String()
	out.EphemeralText = ephemeral.String()
	// Trimmed so the section's own trailing separator doesn't stack with the
	// joins downstream (combined debug views, the adapter's block wrapper).
	out.StableText = strings.TrimRight(stable.String(), "\n")
	return out
}

// renderTravelerPreface opens a transient traveler's own user_message with its
// persona as prose (LLM-370): "You are Elias Drum, a peddler making your way
// through Salem. You hail from Boston, and your manner today is weary." The shared
// salem-visitor VA is stateless and carries no per-visitor identity in its system
// prompt, so this engine-injected preface is what makes it speak as this specific
// traveler. Written to the DURABLE stream ahead of the turn header (see Render), so
// it leads the assembled prompt. Missing persona slots drop their own clause; a nil
// view (every non-traveler subject) writes nothing.
func renderTravelerPreface(b *strings.Builder, v *TravelerSelfView) {
	if v == nil {
		return
	}
	name := sanitizeInline(v.Name)
	if name == "" {
		name = "a traveler"
	}
	b.WriteString("You are ")
	b.WriteString(name)
	if v.Archetype != "" {
		fmt.Fprintf(b, ", %s making your way through Salem", sanitizeInline(sim.WithIndefiniteArticle(v.Archetype)))
	}
	b.WriteString(".")

	origin := sanitizeInline(v.Origin)
	disposition := sanitizeInline(v.Disposition)
	switch {
	case origin != "" && disposition != "":
		fmt.Fprintf(b, " You hail from %s, and your manner today is %s.", origin, disposition)
	case origin != "":
		fmt.Fprintf(b, " You hail from %s.", origin)
	case disposition != "":
		fmt.Fprintf(b, " Your manner today is %s.", disposition)
	}

	// The archetype's vocation sentence (LLM-566): what this calling DOES, so the
	// model plays a preacher/musician/surgeon rather than a generic news-carrier
	// wearing the label. Empty for merchant-derived labels and unknown archetypes.
	if vocation := sanitizeInline(v.Vocation); vocation != "" {
		b.WriteString(" ")
		b.WriteString(vocation)
	}

	// The grounded rumor the traveler carries (LLM-371). One real recent village
	// beat, selected at spawn from the action log and framed as word picked up on
	// the road — so the stateless salem-visitor VA has something true to trade in
	// conversation rather than empty small-talk. Dropped when empty (no
	// rumor-worthy beat was on hand at spawn).
	//
	// LLM-545 tiers the clause by present company. Once everyone in the scene has
	// already had the word from him (and answered — the stamp requires an active
	// conversant), the fresh-news framing is what made a returning traveler reopen
	// a matter his listener had settled, so the clause reframes as spent instead.
	// Mixed company keeps the fresh line (there is a new listener) and names who
	// has heard it already. Someone told elsewhere and not in the scene is never
	// mentioned — the memory matters face to face.
	if word := sanitizeInline(v.RoadWord); word != "" {
		if v.RoadWordSpentWithAllPresent {
			fmt.Fprintf(b, " The word you picked up on the road — that %s — you have already passed to %s, and heard what they had to say; that matter is spent between you.",
				word, joinNames(v.RoadWordSharedWith))
		} else {
			fmt.Fprintf(b, " Word reached you on the road that %s.", word)
			if len(v.RoadWordSharedWith) > 0 {
				fmt.Fprintf(b, " You have already passed that word to %s.", joinNames(v.RoadWordSharedWith))
			}
		}
	}

	// Returner continuity (LLM-372): a traveler who has walked this road before —
	// and may remember specific townsfolk. Voiced as the stateless VA's own memory,
	// tiered by visit count and by how recently it last saw a known player, never a
	// raw count or date (scenes-not-stats). Only present for a repeat visit
	// (VisitCount >= 2), so a first-time traveler never claims to have been here.
	if v.VisitCount >= 2 {
		fmt.Fprintf(b, " %s", returnerVisitClause(v.VisitCount))
		if known := renderReturnerKnownClause(v.KnownHere); known != "" {
			fmt.Fprintf(b, " %s", known)
		}
	}
	b.WriteString("\n\n")
}

// returnerVisitClause tiers how many times a returner has passed through Salem
// into prose — the felt-needs pattern applied to a visit count.
func returnerVisitClause(visitCount int) string {
	switch {
	case visitCount <= 2:
		return "You have passed through Salem before."
	case visitCount <= 4:
		return "You have come through Salem a few times before."
	default:
		return "Salem is no stranger to you — you have come through many times."
	}
}

// renderReturnerKnownClause names the players a returner remembers: the most
// recent by name with a recency, plus a second if there is one. Returns "" when
// the returner recorded no one. Names are sanitized; empties are skipped.
func renderReturnerKnownClause(known []TravelerKnownPC) string {
	var names []string
	var firstRecency sim.RecencyTier
	var firstSummary string
	for _, k := range known {
		n := sanitizeInline(k.Name)
		if n == "" {
			continue
		}
		if len(names) == 0 {
			firstRecency = k.Recency
			// Bound defensively: the store path already caps the summary, but an
			// out-of-band DB edit could set one up to the looser DB CHECK, and this
			// text goes verbatim into every returner preface.
			firstSummary = sim.BoundReturnerSummary(strings.TrimSpace(sanitizeInline(k.Summary)))
		}
		names = append(names, n)
		if len(names) == 2 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	rec := returnerRecencyClause(firstRecency)
	var clause string
	switch {
	case len(names) == 1 && rec != "":
		clause = fmt.Sprintf("You know %s here — you last saw them %s.", names[0], rec)
	case len(names) == 1:
		clause = fmt.Sprintf("You know %s here.", names[0])
	case rec != "":
		clause = fmt.Sprintf("You know %s and %s here — you last saw %s %s.", names[0], names[1], names[0], rec)
	default:
		clause = fmt.Sprintf("You know %s and %s here.", names[0], names[1])
	}
	// Episodic memory (LLM-383): weave the returner's folded impression of the
	// primary (most-recent) remembered player as prose after the recognition
	// clause — the remembered specifics that turn "recognized" into "remembered"
	// ("did that nail hold? you were fretting over the fence line last time"). Only
	// the folded SUMMARY is surfaced — already an in-voice impression the visit-end
	// fold distilled; raw facts are never re-surfaced (ZBBS-HOME-412). Present only
	// after the first fold; empty before then, and the clause reads as plain
	// coarse familiarity (the LLM-372 behavior).
	if firstSummary != "" {
		clause += " " + firstSummary
	}
	return clause
}

// returnerRecencyClause maps a recency tier to the phrase vocabulary that fills a
// "you last saw them ___" slot. "" for an unknown tier.
func returnerRecencyClause(t sim.RecencyTier) string {
	switch t {
	case sim.RecencyRecent:
		return "only lately"
	case sim.RecencyDays:
		return "some days back"
	case sim.RecencyWeeks:
		return "a few weeks back"
	case sim.RecencyMonths:
		return "a month or more ago"
	case sim.RecencyLong:
		return "a long while ago"
	default:
		return ""
	}
}

// renderTurnState writes the conversation turn-state lines (ZBBS-WORK-370): who
// the actor owes a reply to, and who it is awaiting a reply from. The awaiting
// line is the cadence fix — it tells the model it has already spoken and must
// not re-pitch a peer who hasn't answered; renderTriage's coda swap reinforces
// it. Both lists are acquaintance-gated labels resolved at build time. Emits
// nothing when there is no pending turn (the common case).
func renderTurnState(b *strings.Builder, ts TurnStateView, suppressOwedReply bool) {
	// suppressOwedReply drops the "X is waiting for your reply" nag (LLM-160): when
	// the actor's one productive move is to leave for work (the seek-work directive),
	// the reply-pressure is exactly what kept it agree-looping instead of going. The
	// "you already spoke, wait" half below still renders — it discourages re-pitching
	// and is aligned with leaving.
	if !suppressOwedReply {
		for _, name := range ts.OwedReplyTo {
			fmt.Fprintf(b, "%s is waiting for your reply.\n", sanitizeInline(name))
		}
	}
	if len(ts.AwaitingReplyFrom) > 0 {
		fmt.Fprintf(b,
			"You already spoke to %s and are waiting for their reply. Do not repeat "+
				"yourself or address them again — attend to your own work, or simply wait.\n",
			joinNames(ts.AwaitingReplyFrom))
	}
}

// joinNames renders a name list as readable prose: "A", "A and B", or
// "A, B, and C". Each name is sanitized inline (the build-time labels are
// already acquaintance-gated). Returns "" for an empty list.
func joinNames(names []string) string {
	clean := make([]string, 0, len(names))
	for _, n := range names {
		clean = append(clean, sanitizeInline(n))
	}
	switch len(clean) {
	case 0:
		return ""
	case 1:
		return clean[0]
	case 2:
		return clean[0] + " and " + clean[1]
	default:
		return strings.Join(clean[:len(clean)-1], ", ") + ", and " + clean[len(clean)-1]
	}
}

// dormantClause renders the co-present sleepers and resters as a single
// not-addressable clause, e.g. " Prudence Ward is asleep and Goodman Stark is
// resting; neither will respond if you speak to them." (leading space, trailing
// period, so it appends cleanly after a presence line). Same-state members are
// grouped ("X and Y are asleep") and the two groups joined; the tail agrees in
// number. Empty when no one nearby is dormant. ZBBS-WORK-426.
func dormantClause(asleep, resting []HuddleMember) string {
	n := len(asleep) + len(resting)
	if n == 0 {
		return ""
	}
	groups := make([]string, 0, 2)
	if len(asleep) > 0 {
		groups = append(groups, stateGroup(asleep, "asleep"))
	}
	if len(resting) > 0 {
		groups = append(groups, stateGroup(resting, "resting"))
	}
	if n == 1 {
		return fmt.Sprintf(" %s and won't respond if you speak to them.", groups[0])
	}
	tail := "neither will respond if you speak to them"
	if n >= 3 {
		tail = "none of them will respond if you speak to them"
	}
	// At most two groups (asleep, resting), so a plain " and " join reads right.
	return fmt.Sprintf(" %s; %s.", strings.Join(groups, " and "), tail)
}

// steppedAwayClause renders co-present huddle members who have gone quiet — a
// player whose client dropped (LLM-342) — as a single not-addressable clause,
// e.g. " Jefferey has stepped away." (leading space, trailing period, so it
// appends cleanly after a presence line). Same name-vs-descriptor gating as the
// dormant clause; "has"/"have" agrees in number. Empty when none.
func steppedAwayClause(away []HuddleMember) string {
	if len(away) == 0 {
		return ""
	}
	names := make([]string, len(away))
	for i, m := range away {
		names[i] = descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
	}
	verb := "has"
	if len(names) > 1 {
		verb = "have"
	}
	// The clause always follows a period (both the company line and the away-only
	// line end with one), so it starts a new sentence — capitalize the first word,
	// which may be a lowercase descriptor ("a stranger") rather than a name.
	return " " + capitalizeFirst(fmt.Sprintf("%s %s stepped away.", joinNames(names), verb))
}

// stateGroup renders one same-state set of dormant actors with name-vs-descriptor
// gating, e.g. "Prudence Ward and the farmer are asleep" / "Goodman Stark is
// resting". ZBBS-WORK-426.
func stateGroup(members []HuddleMember, state string) string {
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
	}
	verb := "is"
	if len(names) > 1 {
		verb = "are"
	}
	return joinNames(names) + " " + verb + " " + state
}

// renderNeedRedirect writes the LLM-176 need-driven loop coda: in place of the
// generic "do what you've agreed" line — which endorses a confabulated plan when
// the agreement is imaginary ("there's bread in the kitchen") — it names the one
// affordance the engine knows resolves the actor's pressing consumable need plus
// the imperative to act on it now. Need-agnostic phrasing via Verb (eat/drink);
// the move targets carry the inline structure_id every actionable cue does, so
// move_to resolves them. Mirrors the seek-work go-line and the duty steer.
func renderNeedRedirect(v NeedRedirectView) string {
	switch v.Kind {
	case NeedRedirectConsume:
		return fmt.Sprintf("You and the others here keep saying the same thing, but you already carry %s. Don't talk it over again — consume it now to %s.\n",
			sanitizeInline(v.ItemLabel), v.Verb)
	case NeedRedirectBuy:
		return fmt.Sprintf("You and the others here keep saying the same thing, but there is nothing to %s here. Don't talk it over again — go to %s (destination: %s) now and buy %s to %s.\n",
			v.Verb, sanitizeInline(v.TargetLabel), sanitizeInline(v.TargetID), sanitizeInline(v.ItemLabel), v.Verb)
	default: // NeedRedirectFree
		return fmt.Sprintf("You and the others here keep saying the same thing, but there is nothing to %s here. Don't talk it over again — go to %s (destination: %s) now and %s.\n",
			v.Verb, sanitizeInline(v.TargetLabel), sanitizeInline(v.TargetID), v.Verb)
	}
}

// renderTriage writes the closing prioritization instruction — the synthesis
// keystone (ZBBS-HOME-355). The per-tick prompt is a set of equal-weight context
// sections (felt needs, return-to-post, owed orders, vendor cues, what-just-
// happened), several of which can carry an imperative at once (e.g. "Address
// now: hunger" AND "head to your post"). Nothing told the model how to choose
// between them, so it acted on whatever was most salient and drifted. This line
// does NOT impose an engine-computed ranking (the model is capable — the
// prioritization stays in the model); it just instructs the model to weigh the
// context and commit to ONE action, nudging the KIND of triage that the live
// wandering exposed: obligations to others and pressing needs over idle drift.
// Rendered unconditionally — Render is only called on the NPC reactor-tick path
// (handlers.Harness.RunTick), never for a PC.
func renderTriage(b *strings.Builder, needs map[sim.NeedKey]int, thresholds sim.NeedThresholds, awaitingReply bool, conversationLooping bool, conversationRunLong bool, conversationLingering bool, needRedirect *NeedRedirectView, seekWork bool, hasPayOffers bool, inFlightMove *InFlightMoveView, inFlightSourceActivity *InFlightSourceActivityView) {
	// A buyer's offer awaiting this actor's answer outranks everything below —
	// including the actor's own felt needs, which the coda's "pressing needs"
	// phrasing otherwise licenses to win. Without this, a starving seller read
	// his own hunger as the obligation and let a customer's coin sit for whole
	// minutes (conversation hud-6c849d…, ZBBS-HOME-424).
	if hasPayOffers {
		b.WriteString("A buyer's offer awaits your answer — settle it first with accept_pay, decline_pay, or counter_pay, before tending to your own needs.\n")
	}
	switch {
	case inFlightSourceActivity != nil:
		// Mid-activity coda (LLM-69) — the source-activity analogue of the mid-walk
		// coda below. A tick that fires while the actor is mid eat/drink/harvest
		// (a PC speaking, a red need — the interrupts the reactor now lets through)
		// must not render the act-now coda and steer the model into a move that
		// abandons the pick. Make finishing the legible default; responding stays
		// available when what the tick surfaced gives real cause.
		fmt.Fprintf(b,
			"You are %s and it will finish on its own %s. Weigh what's above — "+
				"answer anyone who needs you, but do not walk away without real cause: "+
				"leaving now abandons it. Otherwise call done() and let it finish.\n",
			sourceActivityPhrase(*inFlightSourceActivity),
			sourceActivityCompletionHorizon(inFlightSourceActivity.Kind))
	case inFlightMove != nil:
		// Mid-walk coda (ZBBS-HOME-439) — the walking analogue of WORK-370's
		// awaiting-reply swap. A tick that fires while the actor has a
		// committed walk used to render the act-now coda ("Choose one
		// action") against a toolset the walk gating had narrowed to
		// essentially stop / move_to / done — and the model obliged with
		// stop, killing its own commute (live: Josiah cancelled both of his
		// morning walks to the General Store within seconds, 2026-06-12).
		// Make continuing the legible default; stop stays available for a
		// genuine change of course prompted by what the tick surfaced.
		fmt.Fprintf(b,
			"You are already %s. Weigh what's above — unless it gives you a real "+
				"reason to change course, call done() and keep walking. Do not stop "+
				"without cause.\n",
			renderInFlightMove(*inFlightMove))
	case seekWork:
		// Seek-work directive coda (LLM-160/168): a workless worker with no employer
		// present has one productive move — leave and go to a business. The awaiting-
		// reply and default codas let the huddle's "X is waiting for your reply" social
		// pressure win, and the model re-agreed ("yes, let's go") tick after tick without
		// ever calling move_to (the live Walker agree-loop). Make leaving the imperative;
		// the businesses directory rendered above carries the resolvable destination
		// names. Coins-neutral — a workless worker may hold a little coin and still have
		// no work of its own (LLM-168), so the coda asserts only the actionable facts (no
		// hirer here, go now), not the purse state, which the self-state line already
		// carries. Ordered below the in-flight codas so an actor already walking keeps walking.
		b.WriteString("No one here can hire you. Don't keep talking about going — pick one of the businesses listed above and call move_to now.\n")
	case conversationLooping:
		// Conversational-loop coda (LLM-169): the actor's huddle is going in
		// circles — members re-stating the same agreement without it converting to
		// action (the live Walker "let's go to the well" / "let's go!" echo). The
		// default and awaiting-reply codas both let the reply-pressure win and the
		// echo re-arms; this names the loop and makes resolving it the imperative —
		// act on what's agreed, or let it rest with done(), anything but say it
		// again. The social-loop analogue of the seek-work go-line above; the
		// owed-reply nag is suppressed in renderTurnState so the two steers agree.
		// Ordered below seek-work so a workless worker still gets the leave-for-work
		// directive, and above awaiting-reply since "looping" is the more specific
		// read of why a reply is pending.
		//
		// LLM-176: a need-driven loop circles a CONFABULATED plan ("check the kitchen
		// for bread"), and the generic line above tells them to "do what you've
		// agreed" — endorsing the confabulation. When the actor has a felt consumable
		// need with a real listed source, swap in a concrete redirect that names the
		// engine's known affordance + a move_to/consume imperative (the duty-steer
		// pattern). Falls back to the generic line when no target resolves.
		if needRedirect != nil {
			b.WriteString(renderNeedRedirect(*needRedirect))
		} else {
			b.WriteString("You and the others here keep saying the same thing — the matter is already settled between you. Don't say it again: do what you've agreed — move, tend your work or a need — or call done() and let the moment rest. Speak again only if you truly have something new.\n")
		}
	// LLM-416: conversationRunLong arrives already gated off while the actor is mid
	// item-dwell (see midItemDwell at the render call site) — a pinned eater cannot
	// obey "bring it to a close", so it falls through to the wait/decision coda
	// instead of farewelling every tick.
	case conversationRunLong:
		// Endurance wind-down coda (LLM-333): the huddle has talked for a long
		// stretch with nothing coming of it — no trade, no one new, no player —
		// without lexically looping (the model paraphrases, so the case above
		// never fires; the live farewell loop measured 0.00 repetition). The
		// scene's truth is "this has run its course", so say exactly that and
		// make ending it the imperative. Ordered below conversationLooping
		// (never true together — publish picks the more specific diagnosis) and
		// above awaitingReply for the same reason the loop coda is: "run long"
		// is the more specific read of why a reply is pending. The needRedirect
		// swap is deliberately NOT applied here — it exists to break a
		// confabulated plan-loop, and this case is by definition not a loop.
		b.WriteString("This conversation has gone on a good while and nothing new is coming of it. Bring it to a close — say a brief farewell or simply turn to your own affairs, then call done(). Do not start a new topic.\n")
	// LLM-416: also arrives gated off while mid item-dwell, same as
	// conversationRunLong above — the pinned eater falls through rather than being
	// told to let the talk end.
	case conversationLingering:
		// Lingering wind-down coda (LLM-397). Unlike every case above, this one is
		// not a fault: the conversation may have been warm, varied, and genuinely
		// productive — the live scene that motivated the arm sold a bowl of
		// porridge and then turned to a widow's memories of her husband. It has
		// simply run long. So the line says only that, and says it kindly: the
		// endurance coda's "nothing new is coming of it" would be a plain lie here,
		// and a weak model told an obvious falsehood about the scene in front of it
		// argues with the premise instead of acting on it. Nothing is forbidden —
		// the actor may still answer a need, take an offer, keep a duty; it is only
		// asked to let the talk end rather than open another topic. The sweep's
		// silent conclude one persistence gate later exists for the case where it
		// doesn't, and getting a graceful in-world farewell here instead is the
		// entire point of the arm.
		b.WriteString("You have been talking here a long while now, and the day is getting on. Let the conversation come to its natural end — say your farewells, or simply turn back to your own affairs, then call done(). Do not open a new topic.\n")
	case awaitingReply:
		// Turn-state coda (ZBBS-WORK-370): the actor has spoken and is awaiting a
		// reply. The default "choose one thing and do it" imperative is exactly
		// what drove the re-pitch loop (live-trace finding #2) — it commands an
		// action every tick even when the right move is to wait. Swap it for a
		// wait-permitting framing: real needs/obligations above still license an
		// action, but "nothing new to add" now resolves to done() instead of a
		// repeated pitch.
		b.WriteString("Weigh everything above. If the most pressing matter is simply awaiting someone's reply, do not repeat yourself — wait and call done(). Otherwise act on what matters most: obligations to others and pressing needs come before idle matters.\n")
	default:
		// Universal decision section (ZBBS-WORK-374), replacing the bare HOME-355
		// "choose one thing and do it" coda. Weigh the context, act on what matters,
		// take one action. Speaking is terminal (LLM-321): a successful speak ends
		// the tick on its own, so the old "after you speak, call done()" turn-
		// discipline — the prompt half of the WORK-375 re-pitch fix — is now
		// enforced by the engine rather than the prompt, and is dropped here so the
		// instruction doesn't contradict the mechanic. done() still ends a turn that
		// took a non-terminal action (or none).
		b.WriteString("Weigh what's in front of you — obligations and pressing needs come before idle matters. Choose one action, then call done() when nothing pressing remains.\n")
	}
	// Rest-first steer (ZBBS-WORK-354). When the actor is deeply fatigued AND
	// another need is also pressing, the model otherwise flip-flops between "buy
	// food" and "I need rest" and resolves neither. Steer it to rest first: an
	// actor that sleeps both clears tiredness and pauses all other need growth
	// (IncrementNeedsTick skips a sleeping actor), so resolving rest first is
	// unambiguously the better ordering. Gated on Peak fatigue only — while
	// merely mild/moderately tired the model is free to choose food-vs-rest
	// itself (Jeff: "early on they can make a choice").
	if deepFatigueDominatesNeeds(needs, thresholds) {
		b.WriteString("You are exhausted — rest before tending to other needs; you will handle them better once you have recovered.\n")
	}
}

// deepFatigueDominatesNeeds reports whether the rest-first triage steer should
// fire: tiredness is at NeedPeak (maxed — "exhausted") AND at least one other
// need (hunger or thirst) is also pressing (NeedRed or worse). This is the
// dual-distress case the steer targets. Returns false below Peak fatigue (the
// model chooses freely) or when tiredness alone is pressing (nothing to order
// against). nil thresholds is safe — NeedThresholds.Get falls back to registry
// defaults. ZBBS-WORK-354.
func deepFatigueDominatesNeeds(needs map[sim.NeedKey]int, thresholds sim.NeedThresholds) bool {
	tiredValue, ok := needs["tiredness"]
	if !ok {
		return false
	}
	if sim.NeedLabelTier(tiredValue, thresholds.Get("tiredness")) < sim.NeedPeak {
		return false
	}
	for _, key := range []sim.NeedKey{"hunger", "thirst"} {
		value, ok := needs[key]
		if !ok {
			continue
		}
		if sim.NeedLabelTier(value, thresholds.Get(key)) >= sim.NeedRed {
			return true
		}
	}
	return false
}

// spokenForAnnotation is the carry-line clause for a good the spoken-for
// reservation touches (LLM-636), or "" when every unit is spare. afterUse
// marks that a "used to produce X" clause precedes it, so the not-for-trade
// tail reads "— not for trade" off that clause instead of restating the
// makings reason it already gave.
func spokenForAnnotation(it InventoryItem, afterUse bool) string {
	switch {
	case it.SpokenFor == sim.SpokenForNone || it.Spare >= it.Qty:
		return ""
	case it.Spare > 0:
		return fmt.Sprintf(", %d to spare", it.Spare)
	case it.SpokenFor == sim.SpokenForGarment:
		return ", your own clothes — not for trade"
	case afterUse:
		return " — not for trade"
	default:
		return ", kept to work with — not for trade"
	}
}

func renderActor(b *strings.Builder, a ActorView) {
	b.WriteString("## You\n")
	if line := renderFeltNeeds(a.Needs, a.NeedThresholds); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	// Tiredness renders on its own situated line (LLM-85), separate from the
	// hunger/thirst felt line above: a descriptive tier phrase anchored to hours
	// awake, with NO "address this" imperative — the actionable rest affordances
	// live in the "## How you can rest" menu (buildRecoveryOptions).
	if v, ok := a.Needs[recoveryTirednessNeed]; ok {
		if line := renderTiredness(v, a.NeedThresholds.Get(recoveryTirednessNeed), a.HoursAwake); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	// Cold renders as its own situated line too (LLM-412): tier phrase plus the
	// relief the situation offers — always including a free path (a roof, the
	// fire it is standing by, the clearing sky) so a cold actor is never shown
	// a dead end.
	renderColdSelf(b, a.Cold)
	// An empty purse is a hard constraint on paying, not just a number (LLM-153).
	// Without the consequence spelled out, 0-coin NPCs burned tool calls attempting
	// buys the pay path rejects (engine/sim/pay_commands.go). But "cannot pay for
	// anything" is only true with no TRADEABLE goods either: a 0-coin actor holding
	// barterable goods can still offer them in trade (pay_with_item / offer_trade,
	// ZBBS-HOME-393/407), and the satiation buy cue now steers exactly that
	// (LLM-222) — so asserting it can't pay would contradict that cue in the same
	// prompt. The any-Barterable scan is the render-side mirror of the satiation
	// gate's holdsBarterableGoods (both = holds >=1 good the resolver would accept
	// as payment, stamped from the same catalog at build; LLM-445), so the two
	// lines agree — a pack holding only eat-here food (porridge, stew) reads as no
	// means to barter, because offering it is rejected at resolvePayItems.
	holdsTradeableGoods := false
	for _, it := range a.Inventory {
		if it.Barterable {
			holdsTradeableGoods = true
			break
		}
	}
	switch {
	// LLM-644: a visitor whose takings have outrun his trip budget sees the
	// split, not the raw wallet — the coin he can still put into a purchase,
	// and the rest named as takings bound for home so the earnings he watched
	// come in don't silently vanish from the scene. Without this a
	// proceeds-flush factor read a fat purse and looped offers the pay gates
	// refuse.
	case a.VisitorTakings > 0 && a.Coins == 0:
		fmt.Fprintf(b, "Coins in your purse: %d — but all of it is the trip's takings, bound for home. Your buying purse for this journey is spent; trade goods you carry if you still want something.\n", a.VisitorTakings)
	case a.VisitorTakings > 0:
		fmt.Fprintf(b, "Coins in your purse: %d, of which %d is left to spend on this trip — the other %d is takings, bound for home.\n", a.Coins+a.VisitorTakings, a.Coins, a.VisitorTakings)
	case a.Coins == 0 && holdsTradeableGoods:
		b.WriteString("Coins in your purse: 0 — you have no coins to spend, but you may be able to offer goods you carry in trade.\n")
	case a.Coins == 0:
		b.WriteString("Coins in your purse: 0 — you have no coins to spend, so you cannot pay for anything until you earn some.\n")
	default:
		fmt.Fprintf(b, "Coins in your purse: %d.\n", a.Coins)
	}
	// Standing inventory readout (ZBBS-HOME-361): neutral statement of what the
	// actor holds, so it's aware of its own goods (to eat, to sell, to give)
	// without being pushed to act — the "consume to eat" nudge stays in the
	// need-gated satiation own-stock line. Omitted when carrying nothing.
	if len(a.Inventory) > 0 {
		b.WriteString("You are carrying: ")
		for i, it := range a.Inventory {
			if i > 0 {
				b.WriteString(", ")
			}
			// Count-aware noun (LLM-339): "flasks of water (x20)" not "Water
			// (x20)", so the model isn't left inventing a container ("buckets").
			// Fall back to the display label for a directly-constructed item that
			// didn't resolve a count noun (e.g. the for-sale test fixtures).
			noun := it.CountNoun
			if noun == "" {
				noun = it.Label
			}
			// The use annotation folds into the quantity parens (LLM-166) so the
			// comma-separated item list stays unambiguous: "cuts of meat (x7, used
			// to produce stew)". Empty for edibles / non-ingredients. An eat-here
			// food gets its disposition instead (LLM-445) — "porridge (x8, to eat
			// here — not for trade)" — so the model spends it on its own hunger
			// rather than planning a barter the resolver rejects. Mutually
			// exclusive with Use (Use is inedibles-only).
			//
			// The spoken-for reservation (LLM-636) rides the same parens: a good
			// with no spare unit is "not for trade" with its reason (the makings
			// the keeper works with, or the garment on its back), and a partly
			// reserved one names how many are to spare — so the model plans its
			// barter from the count the pay_with_item gate will actually accept.
			// It folds after Use ("used to produce stew — not for trade") and
			// yields to EatHere, which already says not-for-trade.
			fmt.Fprintf(b, "%s (x%d", sanitizeInline(noun), it.Qty)
			switch {
			case it.Use != "":
				fmt.Fprintf(b, ", %s", sanitizeInline(it.Use))
				b.WriteString(spokenForAnnotation(it, true))
			case it.EatHere:
				b.WriteString(", to eat here — not for trade")
			default:
				b.WriteString(spokenForAnnotation(it, false))
			}
			b.WriteString(")")
		}
		// LLM-574: a settled buy-errand traveler's pack is his own provisions, and the
		// claim rides HERE rather than in his rounds cue. LLM-544 first wrote it into
		// "## Your rounds", a whole section and a conversation away from this line —
		// and Tobias Hewes the nail-buyer, carrying both, spent his afternoon offering
		// the nails he had just bought from the smith to everyone he called on. For
		// every other actor in the village this line IS the sellable-stock readout, so
		// the correction belongs on it. "goods", not "what you carry": a booked room
		// can ride the inventory as a nights_stay, and a granted room is not bound home
		// with anyone.
		//
		// The wording is LLM-544's and is kept deliberately: it states a FACT and does
		// not forbid the pitch, because the scene is the argument. "come by here", not
		// "bought here" — the empty-spawn-pack invariant establishes only that the goods
		// were acquired in the village, not HOW, and a gift over a threshold is as likely
		// as a purchase (Elizabeth Ellis handed Brother Ashford a wedge of cheese and a
		// sack of flour the same afternoon). The claim has to be one the data actually
		// supports, or it is one more thing in the prompt the model can catch out.
		if a.PackBoundHome {
			b.WriteString(" — what goods you carry are your own, come by here and bound home with you, not stock to sell")
		}
		b.WriteString(".\n")
	}
	// Standing in-progress batch (LLM-319): the producer's current work,
	// surfaced on EVERY tick — including a social one when someone approaches —
	// so it can always say what it is making (a PC can ask), and a tick firing
	// mid-batch knows to stay at the post rather than re-decide from scratch.
	if a.InFlightProduction != nil {
		switch {
		case a.InFlightProduction.Halted:
			// LLM-446: at pct 0 the batch is fully paused by the degrade gate —
			// say so rather than re-promising the frozen WorkLeft every tick (the
			// live "three more minutes" loop).
			fmt.Fprintf(b, "You are making a batch of %s, but the work stands still — your business is too worn to carry it forward until you mend it.\n",
				sanitizeInline(a.InFlightProduction.ItemLabel))
		case a.InFlightProduction.Slowed:
			fmt.Fprintf(b, "You are making a batch of %s — about %s of work left, though the disrepair of your business drags the work out longer than that; it only moves along while you're at your post.\n",
				sanitizeInline(a.InFlightProduction.ItemLabel), a.InFlightProduction.WorkLeft)
		default:
			fmt.Fprintf(b, "You are making a batch of %s — about %s of work left; it only moves along while you're at your post.\n",
				sanitizeInline(a.InFlightProduction.ItemLabel), a.InFlightProduction.WorkLeft)
		}
	}
	// In-progress activity reads as felt self-state. A meal/rest/walk already
	// under way is surfaced so a tick firing mid-activity doesn't re-pick a
	// goal from scratch (the dwell-credit/in-flight-move parking fix). These
	// also cover the resting/walking macro-states, so the bare state line only
	// fires when nothing else already conveys what the actor is doing.
	activity := false
	// The constable's rounds tour (LLM-514) is the dominant self-state voice while
	// it runs: a diegetic "you are walking your rounds" line that keeps any tick
	// during the tour in character, plus "you stand before the <business>" once he
	// has arrived at a stop. It suppresses the plain in-flight-move line below so
	// the two don't both narrate his movement.
	if a.Rounds != nil {
		// One voice for the round, wherever he is (LLM-548). The engine no longer
		// walks him between stops, so there is no procession to be interrupted and no
		// paused state to announce: he owes these places today, and the line says so
		// whether he is on a doorstep or off at the well. The old split spoke of the
		// round as "left part-walked" whenever he stepped away, which read as a lapse
		// to answer for rather than a morning's work still to do.
		if a.Rounds.AtBusiness != "" {
			fmt.Fprintf(b, "You are walking your rounds through the village. You stand before the %s.\n",
				sanitizeInline(a.Rounds.AtBusiness))
		} else {
			b.WriteString("You are walking your rounds through the village.\n")
		}
		// Carry the circuit forward (LLM-524). A quiet stop otherwise reads as a dead
		// end — the model finds a shut, empty shop, concludes the round is pointless
		// and walks back to its post, ending the tour at the first closed door.
		// Naming what still lies ahead makes the stop a waypoint. Deliberately NOT an
		// imperative ("go on to the next"): the scene is the argument, and where he
		// goes next is his to choose. Omitted when nothing is left to name.
		if line := renderRoundsStopsAhead(a.Rounds.StopsAhead, a.Rounds.NextBusiness); line != "" {
			b.WriteString(line)
		}
		activity = true
	}
	// A timed source activity (eat/drink/harvest in flight, LLM-69) leads — it is
	// the most occupied state, and the reactor now lets high-value interrupts tick
	// the actor mid-window, so this standing line is what tells it to hold rather
	// than walk off (the live forage→walk-off bug).
	if a.InFlightSourceActivity != nil {
		fmt.Fprintf(b, "You are %s.\n", renderInFlightSourceActivity(*a.InFlightSourceActivity))
		activity = true
	}
	for _, c := range a.ActiveDwellCredits {
		fmt.Fprintf(b, "You are %s.\n", renderActiveDwellCredit(c))
		activity = true
	}
	// No longer suppressed during a round (LLM-548). The rounds line used to narrate
	// the engine's own walking, and two movement voices in one prompt contradicted
	// each other; now nothing dispatches him, so every leg he walks is his own and
	// this line is the only thing that says where he is headed. The rounds line
	// states what he owes, this one what he is doing about it.
	if a.InFlightMove != nil {
		fmt.Fprintf(b, "You are %s.\n", renderInFlightMove(*a.InFlightMove))
		activity = true
	}
	if !activity {
		if line := renderFeltState(a.State); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
}

// renderFeltNeeds turns the hunger/thirst need values into felt language in the
// fixed hunger→thirst order. Needs below the awareness floor stay silent.
// Red/peak needs lead with an "Address now:" imperative — v1's 2026-05-02 fix
// that made NPCs act on distress instead of reading a flat integer they
// couldn't calibrate (the original "needs: hunger=24" dump gave the model no
// sense that 24 is peak starvation). Tiredness is intentionally NOT handled here
// (LLM-85) — it renders as its own situated, descriptive line, renderTiredness.
// Returns "" when nothing is surfaced. ZBBS-HOME-339.
func renderFeltNeeds(needs map[sim.NeedKey]int, thresholds sim.NeedThresholds) string {
	if len(needs) == 0 {
		return ""
	}
	var felt, pressing []string
	for _, key := range []sim.NeedKey{"hunger", "thirst"} {
		value, ok := needs[key]
		if !ok {
			continue
		}
		n, ok := sim.FindNeed(key)
		if !ok {
			continue
		}
		tier := n.Tier(value, thresholds.Get(key))
		label := n.Label(tier)
		if label == "" {
			continue // NeedSilent — below the awareness floor
		}
		felt = append(felt, label)
		if tier >= sim.NeedRed {
			pressing = append(pressing, string(key))
		}
	}
	if len(felt) == 0 {
		return ""
	}
	if len(pressing) > 0 {
		return fmt.Sprintf("Address now: %s. You feel %s.",
			strings.Join(pressing, ", "), strings.Join(felt, ", "))
	}
	return fmt.Sprintf("You feel %s.", strings.Join(felt, ", "))
}

// renderTiredness renders the actor's tiredness as its own situated, descriptive
// line: the qualitative tier (a little tired / weary / exhausted) anchored to how
// long the actor has been awake, so the model weighs rest against real elapsed
// time instead of over-reacting to a bare adjective (LLM-85 — a merchant closed
// his shop 4h on a mild "tired"). It deliberately carries NO "address this"
// imperative at any tier: the concrete rest affordances live in the "## How you
// can rest" menu (buildRecoveryOptions), and dropping the imperative everywhere
// completes LLM-67 (the felt imperative was the stimulus for the re-take_break
// loop). hoursAwake is nil off-shift, for an unscheduled NPC, or a clock-less
// snapshot — then the awake-hours tail is dropped and only the tier phrase
// renders. Returns "" below the awareness floor.
func renderTiredness(value, threshold int, hoursAwake *int) string {
	n, ok := sim.FindNeed(recoveryTirednessNeed)
	if !ok {
		return ""
	}
	var lead string
	switch n.Tier(value, threshold) {
	case sim.NeedMild:
		lead = "You're starting to feel a little tired"
	case sim.NeedRed:
		lead = "You're weary"
	case sim.NeedPeak:
		lead = "You're exhausted"
	default:
		return "" // NeedSilent — below the awareness floor
	}
	if hoursAwake != nil && *hoursAwake >= 1 {
		unit := "hours"
		if *hoursAwake == 1 {
			unit = "hour"
		}
		return fmt.Sprintf("%s — you've been awake for %d %s.", lead, *hoursAwake, unit)
	}
	return lead + "."
}

// renderFeltState renders a macro-state as a felt line, or "" for states that
// carry no standalone meaning (idle) or are already conveyed by the dwell/move
// lines (walking). Only reached when renderActor surfaced no in-progress
// activity. ZBBS-HOME-339.
func renderFeltState(state sim.ActorState) string {
	switch state {
	case sim.StateResting:
		return "You are taking a rest."
	case sim.StateSleeping:
		return "You are asleep."
	case sim.StateConversing:
		return "You are in conversation."
	case sim.StateShopping:
		return "You are out shopping."
	case sim.StateInTransaction:
		return "You are in the middle of a transaction."
	case sim.StateEating:
		return "You are eating."
	default: // idle, walking, unknown — nothing standalone to add
		return ""
	}
}

// renderInFlightMove produces the felt-language self-perception line for an
// actor mid-walk ("walking to enter the Tavern"). The movement analogue of
// renderActiveDwellCredit: present on every perception build while the walk is
// live, so a reactor tick triggered mid-journey (by heard speech, a need,
// anything) shows the LLM it already has a destination and shouldn't re-pick
// one from scratch — the fix for the senseless goal-flipping that the
// dwell-credit line already prevents for meals. ZBBS-HOME-336.
func renderInFlightMove(m InFlightMoveView) string {
	dest := m.DestinationLabel
	if dest == "" {
		return "walking to your destination"
	}
	if m.Kind == sim.MoveDestinationStructureEnter {
		return fmt.Sprintf("walking to enter %s", sim.WithDefiniteArticle(sanitizeInline(dest)))
	}
	if m.Kind == sim.MoveDestinationPosition {
		// A bare coordinate label ("(41, 44)") names no place — no article.
		return fmt.Sprintf("walking to %s", sanitizeInline(dest))
	}
	return fmt.Sprintf("walking to %s", sim.WithDefiniteArticle(sanitizeInline(dest)))
}

// sourceActivityVerb picks the second-person verb for an in-flight source
// activity: "gathering" for a harvest, and eat/drink/rest for a refresh keyed on
// the eased need (falling back to "busy" for an unknown attribute). LLM-69.
func sourceActivityVerb(v InFlightSourceActivityView) string {
	switch v.Kind {
	case sim.SourceActivityHarvest:
		return "gathering"
	case sim.SourceActivityRepair:
		return "mending"
	case sim.SourceActivityStoke:
		return "tending the fire"
	case sim.SourceActivityBake:
		return "baking bread"
	}
	switch v.Attribute {
	case "hunger":
		return "eating"
	case "thirst":
		return "drinking"
	case "tiredness":
		return "resting"
	}
	return "busy"
}

// sourceActivityPhrase is the bare "<verb> at <source>" clause shared by the
// standing self-state line and the mid-activity triage coda, so both name the
// activity identically. Drops the "at <source>" when the label didn't resolve.
func sourceActivityPhrase(v InFlightSourceActivityView) string {
	verb := sourceActivityVerb(v)
	if v.SourceLabel != "" {
		return verb + " at " + sanitizeInline(v.SourceLabel)
	}
	return verb
}

// sourceActivityCompletionHorizon is how soon the mid-activity coda promises the
// activity will land on its own. Every kind but bake finishes inside a single turn or
// close to it (refresh 3s, harvest 5s, stoke 30s), so "shortly" tells them the truth.
// Repair at 15m is the loosest fit — deliberately kept as "shortly", since a quarter
// hour is still "soon" to a villager and the coda's job is to stop the actor walking
// off, not to time the work. A bake runs until DUSK — hours — and the hardcoded "shortly"
// was a lie the model faithfully passed on to the household: live 2026-07-18, the
// engine told Anne Walker at midday that her five-hour batch would finish shortly
// and she announced "the loaves are nearly ready — just a few more minutes by the
// hearth" (LLM-464). Bake names the same horizon its own cue does ("fresh loaves by
// dusk"), so the two surfaces agree. Same failure class as LLM-446's frozen
// WorkLeft, one layer up: a constant that stopped being true when a slower kind
// joined the substrate.
// Written as an exhaustive switch over the kinds rather than a bake special-case with a
// default: Go won't enforce exhaustiveness, but listing every kind puts the question in
// front of whoever adds the next one, which is the step that got skipped last time. An
// unlisted kind still falls through to "shortly" — the conservative answer for anything
// short, and wrong only for another to-dusk activity.
func sourceActivityCompletionHorizon(kind sim.SourceActivityKind) string {
	switch kind {
	case sim.SourceActivityBake:
		return "by dusk"
	case sim.SourceActivityRefresh, sim.SourceActivityHarvest, sim.SourceActivityStoke, sim.SourceActivityRepair:
		return "shortly"
	}
	return "shortly"
}

// renderInFlightSourceActivity produces the standing self-perception line for an
// actor mid eat/drink/harvest ("gathering at the bush — stay where you are; if
// you walk off now you abandon the pick"). The source-activity analogue of
// renderInFlightMove: present on every perception build while the window is live,
// so a reactor tick that fires mid-activity (a PC speaking, a red need) shows the
// LLM it is occupied and holds it in place rather than re-deciding into a move
// that abandons the pick (LLM-69). The caller prepends "You are ".
func renderInFlightSourceActivity(v InFlightSourceActivityView) string {
	tail := "if you walk off now you won't finish"
	switch v.Kind {
	case sim.SourceActivityHarvest:
		tail = "if you walk off now you abandon the pick and gather nothing"
	case sim.SourceActivityRepair:
		tail = "if you walk off now the mending is unfinished and the stall stays worn"
	case sim.SourceActivityStoke:
		tail = "if you walk off now the wood is wasted and the fire stays low"
	case sim.SourceActivityBake:
		// LLM-454: the evening bake is a SHARED, sociable occupation, so its standing
		// line invites the one housemate reply the reactor deliberately ticks a baker
		// for (bakeReplyDue) — unlike the solitary repair/harvest/stoke tails that only
		// say "stay put." A committed move still abandons the bread, so hold her at the
		// hearth. Returned whole (not via the "stay where you are" template) so the
		// speech-permissive framing replaces it.
		return "at the hearth with the household's bread — a word to those about you is fine, but stay with it; if you walk off now the bread won't be finished"
	}
	return fmt.Sprintf("%s — stay where you are; %s", sourceActivityPhrase(v), tail)
}

// renderActiveDwellCredit produces the felt-language self-perception
// line for one in-progress dwell credit ("eating stew at the tavern, it
// will take you 14 more minutes to finish eating it all. ..."). The
// load-bearing prompt line that keeps
// LLM-driven NPCs from walking away mid-meal: every perception build
// during the meal renders this, so plan-stage always sees the active
// effect even if no per-tick narration warrant landed this turn.
//
// Source=item with a known Kind → "eating <kind> at <where>".
// Source=item with empty Kind → "having a meal at <where>" (fallback).
// Source=object → "resting at <where>" / "drawing from <where>" by
// attribute (covers shade-tree tiredness, well thirst, berry-bush
// hunger).
func renderActiveDwellCredit(c DwellCreditView) string {
	where := c.StructureLabel
	if where == "" && c.ObjectID != "" {
		where = string(c.ObjectID)
	}
	verb := dwellActivityVerb(c)
	var subject string
	if c.Source == sim.DwellSourceItem && c.Kind != "" {
		subject = fmt.Sprintf("%s %s", verb, sanitizeInline(string(c.Kind)))
	} else {
		subject = verb
	}
	if where != "" {
		subject = fmt.Sprintf("%s at %s", subject, sanitizeInline(where))
	}
	if c.RemainingTicks != nil && c.PeriodMinutes > 0 {
		// ZBBS-WORK-409: "~N minute(s) remaining" never said remaining OF WHAT —
		// it read as a countdown until the actor was free to go, so NPCs walked
		// off mid-meal and forfeited the slow-burn payoff (the credit deletes on
		// walk-away). Spell out, in prose, how long it takes to FINISH and what
		// leaving costs (sim.DwellStayClause — shared with the settle feedback so
		// the buyer hears one consistent message), so this load-bearing parking
		// line does its job instead of inviting an exit. No coins clause here: an
		// item dwell can also be self-consumed pack food, not a purchase.
		minutes := (*c.RemainingTicks) * c.PeriodMinutes
		subject = fmt.Sprintf("%s, %s", subject, sim.DwellStayClause(minutes, c.Attribute, ""))
	} else if c.Source == sim.DwellSourceObject {
		// ZBBS-WORK-411: object dwells (shade tree, well, berry bush) are free,
		// open-ended recovery sources with no countdown, so they skip the item
		// branch above and would otherwise render bare ("You are resting at the
		// old oak") — no stake, leaving NPCs free to wander off mid-recovery while
		// the duty-steer / "## How you can rest" alternatives pull them away. The
		// open-ended sibling clause says staying keeps easing the need and that
		// leaving stops it.
		subject = fmt.Sprintf("%s, %s", subject, sim.ObjectDwellStayClause(c.Attribute))
	}
	return subject
}

// dwellActivityVerb picks the verb for a dwell-in-progress line based
// on source + attribute. Item-source meals lead with "eating" /
// "drinking" / "resting" by attribute; object-source lines lead with
// the activity matching the pin (resting under a tree, sipping at a
// well). Defaults to "lingering" when nothing fits.
func dwellActivityVerb(c DwellCreditView) string {
	if c.Source == sim.DwellSourceItem {
		switch c.Attribute {
		case "hunger":
			return "eating"
		case "thirst":
			return "drinking"
		case "tiredness":
			return "resting with"
		}
		return "having"
	}
	switch c.Attribute {
	case "hunger":
		return "foraging"
	case "thirst":
		return "drinking"
	case "tiredness":
		return "resting"
	}
	return "lingering"
}

// timeOfDayProse maps a village wall-clock minute-of-day (0–1439) to a felt
// ambient sentence — the deterministic, in-world time-of-day analogue of the
// LLM-authored atmosphere line. The engine itself tracks only a binary
// day/night Phase (dawn/dusk flips); these finer narration bands are a
// presentation detail derived from the clock minute, so the NPC gets a sense of
// the hour without the engine modelling more than it needs. ZBBS-HOME-351.
func timeOfDayProse(minute int) string {
	if minute < 0 || minute > 1439 {
		return "" // out of range — fail closed rather than render a misleading band
	}
	switch {
	case minute < 300: // before 05:00
		return "It is the dead of night."
	case minute < 420: // 05:00–07:00
		return "Dawn is breaking over the village."
	case minute < 720: // 07:00–12:00
		return "It is morning in the village."
	case minute < 840: // 12:00–14:00
		return "It is midday."
	case minute < 1080: // 14:00–18:00
		return "The afternoon wears on."
	case minute < 1260: // 18:00–21:00
		return "Evening settles over the village."
	default: // 21:00–24:00
		return "Night lies over the village."
	}
}

// weatherStormProse is the felt line an NPC reads when a storm is overhead — the
// perception-side analogue of the client's rain / scene-darkening / lightning FX
// (LLM-117). A plain scene, not a "weather: storm" stat: it hands the model
// something concrete to reason over (shelter, mud) and lets the scene be the
// argument, with no imperative. A named const so the golden invariant
// (TestGoldensRainLineIffStorm) and weatherProse can't drift on the wording.
//
// Sourced from sim (LLM-399) so this line and the sky the atmosphere prompt is
// told to write to are the same sentence, not two hand-written descriptions of
// the same storm that can drift apart.
const weatherStormProse = sim.WeatherStormScene

// weatherProse maps World.Environment.Weather to a felt ambient sentence — the
// deterministic, in-world weather analogue of timeOfDayProse. Only the active
// storm state renders (LLM-364); clear / empty (the calm default) render
// nothing, matching how the atmosphere prompt treats clear as "no weather line"
// (cascade/atmosphere.go). An unrecognized future token (fog, snow) also renders
// nothing rather than leak a raw stat — additive by design: a new state surfaces
// once its scene is added here, the same graceful-degradation posture as the
// atmosphere digest's verb map.
func weatherProse(weather string) string {
	switch strings.TrimSpace(weather) {
	case sim.WeatherStorm:
		return weatherStormProse
	default:
		return ""
	}
}

// deadEndClause renders the at-location dead-end sentence for the place the
// actor is physically at (LLM-154), or "" when the place can serve them. The
// place is named from the same SurroundingsView fields the location line uses
// (StructureName inside, NearbyStructureName outdoors), so the clause and the
// "You are ..." line can never name different places. Sentence-start, so the
// mid-clause article from WithDefiniteArticle is capitalized ("the Tavern" →
// "The Tavern").
func deadEndClause(s SurroundingsView) string {
	switch s.LocationDeadEnd {
	case DeadEndShutBusiness:
		name := s.StructureName
		if name == "" {
			name = s.NearbyStructureName
		}
		if name == "" {
			return ""
		}
		place := capitalizeFirst(sim.WithDefiniteArticle(sanitizeInline(name)))
		return place + " is shut — no one is tending it."
	case DeadEndNoConsumableHere:
		// LLM-176: name the missing affordance at a foodless spot so a weak model
		// can't confabulate food here ("bread in the kitchen"). "Here" is unambiguous
		// next to the location line; phrasing follows which felt need has no source.
		switch {
		case s.DeadEndHunger && s.DeadEndThirst:
			return "There's nothing to eat or drink here — you'll need to forage or buy it elsewhere."
		case s.DeadEndThirst:
			return "There's nothing to drink here — you'll need to find a well or buy a drink elsewhere."
		default:
			return "There's no food to be had here — you'll need to forage or buy a meal elsewhere."
		}
	default:
		return ""
	}
}

// capitalizeFirst upper-cases the leading letter of a mid-clause label so it can
// open a sentence — WithDefiniteArticle yields "the Tavern", and a sentence-
// start caller (the LLM-154 shut-business clause) needs "The Tavern". Rune-aware
// so a non-ASCII leading character in a display name is upper-cased, not mangled
// byte-wise; empty in, empty out.
func capitalizeFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return ""
	}
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// maxRenderedContactLines caps how many contact-recency facts render in one
// scene. A crowded room where the subject has spoken with everyone would
// otherwise add a line per person to a section that is re-sent every tick. Three
// is enough to carry the case the cue exists for — the constable working a room
// he has already worked — without the block becoming a list.
//
// The cap keeps the LOUDEST tiers: a brake outranks continuity, because a brake
// is a reason not to repeat yourself and continuity is only an invitation. What
// is dropped is therefore always the least consequential fact present.
const maxRenderedContactLines = 3

// contactRecencyLines renders the subject's conversational history with the
// people in the scene (LLM-547), most consequential first.
//
// Register, all four constraints from the ticket:
//   - The peer's VERBATIM DisplayName, never an honorific. resolveAddressee
//     matches a peer by full display name or first name only, so "Mistress Ward"
//     resolves to nobody and the utterance silently degrades to addressing the
//     whole huddle. Period flavour lives in "had your word with", not the name.
//   - No place is ever named. The rounds cue names the NEXT stop verbatim
//     because it must work as a move_to token; that reasoning inverts here —
//     naming shops already walked hands the model fresh destinations, which is
//     the behaviour being fixed. People for what is done, places for what remains.
//   - No imperative. The scene is the argument; the model draws the conclusion.
//   - No absorbing state. These are scene facts only. Nothing here suppresses a
//     verb or gates speech: calling on someone again hours later is legitimate
//     and stays available at every tier.
//
// Deliberately pronoun-free. The ticket's example line read "she has said her
// piece", but actors carry no gender or pronoun field — deriving one from a name
// would be a guess rendered as fact into a prompt, so the weighted tier carries
// the peer's state through the clause instead.
func contactRecencyLines(s SurroundingsView) []string {
	type fact struct {
		tier  sim.ContactTier
		count int
		name  string
	}
	var facts []fact
	seen := make(map[sim.ActorID]struct{})
	// Huddle peers first, then merely co-present. A member cannot appear in both
	// (the huddle roster and the co-presence line are disjoint sets), but the
	// guard keeps a future overlap from double-rendering the same person.
	for _, group := range [][]HuddleMember{s.HuddleMembers, s.CoPresent} {
		for _, m := range group {
			if m.ContactTier == sim.ContactTierNone || m.DisplayName == "" {
				continue
			}
			// An unacquainted peer is named "a stranger" / "the blacksmith" by
			// descriptorLabel everywhere else in this section. This line CANNOT
			// follow that gating — it must carry the verbatim DisplayName or the
			// model's reply degrades silently (see the register note above) — so
			// when the name is withheld the whole fact is withheld with it.
			//
			// Otherwise the scene contradicts itself in adjacent lines: "You are
			// outdoors, with a stranger. You had your word with Gideon Marsh a
			// short while ago." That leaks a name the acquaintance gate exists to
			// withhold, and asks the model to reconcile two descriptions of one
			// person. Losing the fact is the cheaper failure — a nameless "you
			// spoke with someone earlier" argues nothing.
			if !m.Acquainted {
				continue
			}
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			facts = append(facts, fact{tier: m.ContactTier, count: m.ContactRecentCount, name: m.DisplayName})
		}
	}
	if len(facts) == 0 {
		return nil
	}
	// Loudest tier first, then by name so a tie is stable run to run.
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].tier != facts[j].tier {
			return facts[i].tier > facts[j].tier
		}
		return facts[i].name < facts[j].name
	})
	if len(facts) > maxRenderedContactLines {
		facts = facts[:maxRenderedContactLines]
	}
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		if line := contactRecencyLine(f.tier, f.count, f.name, s.OnRound); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// contactRecencyLine maps one tier to its sentence.
//
// "this round" only when a round is actually under way; otherwise a bare time
// phrase, so an actor with no circuit is never told about a round it does not
// have.
func contactRecencyLine(tier sim.ContactTier, count int, name string, onRound bool) string {
	name = sanitizeInline(name)
	if name == "" {
		return ""
	}
	switch tier {
	case sim.ContactTierContinuity:
		// Not lately — history to build on rather than a reason to hold off.
		return fmt.Sprintf("You had your word with %s earlier today.", name)
	case sim.ContactTierBrakeQuiet:
		if onRound {
			return fmt.Sprintf("You have already had your word with %s this round.", name)
		}
		return fmt.Sprintf("You had your word with %s a short while ago.", name)
	case sim.ContactTierBrakeWeighted:
		// The tier that carries the PEER's state, not only the subject's history:
		// there is nothing left to draw out of them. Ward's rebuke landed on the
		// third approach; the scene should let him feel it before she delivers it.
		when := "in this short while"
		if onRound {
			when = "already this round"
		}
		return fmt.Sprintf("You have had your word with %s %s %s — there was little left unsaid by the end of it.",
			name, contactTimesPhrase(count), when)
	default:
		return ""
	}
}

// contactTimesPhrase words a repeat count. Caps out at "several times" rather
// than counting indefinitely — past a few the exact number stops carrying
// meaning, and a raw tally would read as a stat in a scene.
func contactTimesPhrase(count int) string {
	switch {
	case count <= 2:
		return "twice"
	case count == 3:
		return "three times"
	case count == 4:
		return "four times"
	default:
		return "several times"
	}
}

func renderSurroundings(b *strings.Builder, s SurroundingsView) {
	b.WriteString("## Around you\n")

	// Location + company in one felt sentence. The struct-field form ("inside:
	// outdoors" / "huddle: not in a huddle") was a raw dump and engine jargon —
	// "huddle" is a word the LLM was never taught. ZBBS-HOME-339.
	var location string
	switch {
	case s.InsideStructureID != "":
		name := s.StructureName
		if name == "" {
			name = "a building"
		}
		location = "inside " + sim.WithDefiniteArticle(sanitizeInline(name))
		// LLM-212: annotate when the actor is inside its OWN home/workplace, so a
		// weak model reads "inside the James Residence, your home" and can tell it
		// is already at its anchor (the legibility half of the move_to(home)
		// confusion). Set only for the inside branch (Build computes it from the
		// actor's home/work ids); empty otherwise.
		if s.InsideRelation != "" {
			location += ", " + s.InsideRelation
		}
	case s.NearbyStructureName != "":
		// Standing at a structure's loiter slot while outdoors — a keeper at
		// their own stall, a customer outside a shop. Names where they are so
		// the model doesn't read raw coordinates and re-walk to a place it is
		// already standing at.
		location = "outdoors by " + sim.WithDefiniteArticle(sanitizeInline(s.NearbyStructureName))
	default:
		location = "outdoors"
	}
	// Co-present sleepers and resters are visible but not addressable by THIS
	// actor — sleep is never interrupted by speech, and an NPC's speech can't
	// rouse a rester either (reactor.go actorCanReactNow; only a PC / red-tier
	// need / operator nudge wakes a rester). Render them in a distinct
	// not-addressable clause so the actor doesn't talk to someone who won't
	// answer and read the silence as rudeness (ZBBS-WORK-426).
	dormant := dormantClause(s.CoPresentAsleep, s.CoPresentResting)
	// A stepped-away huddle member (a player whose client went quiet, LLM-342) is
	// named but not addressable — appended after the company line so the NPC reads
	// the room as briefly without an answer, not as someone ignoring it.
	away := steppedAwayClause(s.HuddleAway)
	switch {
	case len(s.HuddleMembers) > 0:
		// A huddle is a conversational cluster, so "with" names who the actor
		// is gathered with — the speak tool reaches exactly these people.
		// (CoPresentAsleep/Resting are only populated for an unhuddled actor, so
		// there is no dormant clause to append in this case.)
		fmt.Fprintf(b, "You are %s, with %s.%s\n", location, joinHuddleMembers(s.HuddleMembers), away)
	case len(s.HuddleAway) > 0:
		// Huddled, but the only other member(s) have stepped away — no one live to
		// converse with right now. Name them as away so the NPC doesn't keep
		// addressing an absent player; a returning player becomes addressable again
		// the moment its socket re-stamps presence.
		fmt.Fprintf(b, "You are %s.%s\n", location, away)
	case len(s.CoPresent) > 0:
		// Not conversing, but others are within earshot. Name them (every turn) so
		// the actor can address someone and start conversing, instead of
		// discovering "no one here to hear you" by tripping the speak gate. This is
		// the SAME set the speak path would reach (ZBBS-WORK-407). Presence is
		// stated neutrally — no "speak to them" coaching. The directive fired on
		// every arrival and pushed NPCs into unprompted monologues at whoever was
		// present, PCs included; naming alone is enough for a greeting to happen
		// when the actor has a social reason for one (LLM-220).
		names := make([]string, len(s.CoPresent))
		for i, m := range s.CoPresent {
			label := travelerCoPresentLabel(m)
			if m.JustArrived {
				// ZBBS-WORK-422: flag a newcomer so a stateless NPC reads the
				// "someone just walked up — greet them" beat. Without it a fresh
				// arrival is indistinguishable from someone who has stood here a
				// while, since "## Around you" only lists standing presence.
				label += " (just arrived)"
			}
			label += laborTiePhrase(m.SolicitTie)
			label += laboringPhrase(m)
			label += eatingPhrase(m)
			label += busyActivityPhrase(m)
			names[i] = label
		}
		verb := "is"
		if len(names) > 1 {
			verb = "are"
		}
		fmt.Fprintf(b, "You are %s. %s %s here with you.%s\n",
			location, joinNames(names), verb, dormant)
	case len(s.CoPresentAsleep) > 0 || len(s.CoPresentResting) > 0:
		// No one awake within earshot, but someone is here asleep or resting. Name
		// them so the actor knows the room isn't empty, while making clear there's
		// no one it can talk to right now (ZBBS-WORK-426).
		fmt.Fprintf(b, "You are %s.%s There is no one awake here to hear you speak.\n",
			location, dormant)
	default:
		// No one within earshot. State it plainly, every turn, so the actor turns
		// to a solo task or moves to find company rather than speaking to an empty
		// room. Echoes the speak gate's wording ("no one here to hear you").
		fmt.Fprintf(b, "You are %s, with no one else here to hear you speak.\n", location)
	}

	// LLM-547: what the subject's own history with the people here says. Placed
	// directly under the company line so each fact sits with the person it is
	// about, and branch-independent for the same reason deadEndClause is — the
	// history holds whether they are huddled or merely co-present.
	for _, line := range contactRecencyLines(s) {
		b.WriteString(line)
		b.WriteString("\n")
	}

	// LLM-154: a live dead-end at the actor's current location, stated plainly on
	// its own line so a weak model isn't left to infer "closed" from "the keeper
	// is asleep". Branch-independent (fires whether the actor is huddled, has
	// company, or is alone) and named from the same fields the location line uses,
	// so the two can't name different places.
	if clause := deadEndClause(s); clause != "" {
		b.WriteString(clause)
		b.WriteString("\n")
	}

	// Time of day as ambient prose (ZBBS-HOME-351). v2 rendered no clock at all,
	// so an NPC couldn't tell its working hours from the dead of night — the
	// missing context HOME-352 (return-to-post) builds on. nil only for a
	// hand-built snapshot with no clock established; in a running engine the
	// publish path always sets it, so the line is always present there.
	if s.LocalMinuteOfDay != nil {
		if prose := timeOfDayProse(*s.LocalMinuteOfDay); prose != "" {
			b.WriteString(prose)
			b.WriteString("\n")
		}
	}

	// Weather as ambient prose (LLM-364), right after the time-of-day line so the
	// two read as one ambient frame ("It is morning in the village. / Rain falls
	// steady …"). Read from the live Environment.Weather snapshot, so — unlike the
	// LLM-authored atmosphere line removed below — it's deterministic and never
	// lags the sky. Clear / empty renders nothing.
	if prose := weatherProse(s.Weather); prose != "" {
		b.WriteString(prose)
		b.WriteString("\n")
	}

	// ZBBS-WORK-374: the LLM-authored literary atmosphere line (ZBBS-WORK-327,
	// "The night abideth over the village in a sober hush…") is NOT rendered into
	// the decision prompt — ~45 words of restart-lossy scene prose irrelevant to
	// the action at hand, part of the low-signal bulk that buried the actual
	// stimulus. The deterministic time-of-day line above is kept (it's the clock
	// context HOME-352 relies on). SurroundingsView.Atmosphere stays populated for
	// any other consumer; we just don't spend prompt budget on it here.

	// Harvest affordance (ZBBS-WORK-328): the model often stands at a well/bush
	// without connecting "I'm here" to "I can gather." This line makes the
	// affordance explicit. Same SurroundingsView fields drive gateTools'
	// gather advertising, so the cue and the offered tool can't drift.
	if s.GatherableItem != "" {
		source := strings.TrimSpace(sanitizeInline(s.GatherableSource))
		if source == "" {
			source = "this spot"
		}
		// LLM-113: render the plural counting phrase ("raspberries"), not the raw
		// catalog key. GatherableNoun is empty when a caller builds the view
		// directly (some tests) — fall back to the key so the cue still renders.
		noun := s.GatherableNoun
		if noun == "" {
			noun = string(s.GatherableItem)
		}
		fmt.Fprintf(b, "You're at %s — you can gather %s here.\n",
			source, sanitizeInline(noun))
	}
	b.WriteString("\n")
}

// renderAnchors writes the actor's standing home/work move targets as a prose
// line carrying each structure_id. Always emitted when the view is non-nil, so
// a wandering NPC always has its own home and work as reachable destinations —
// not only when a need-cue happens to point somewhere (the gap that let John
// Ellis cycle to a closed farm with no id for his own tavern to head back to).
// The "(structure_id: …)" form matches the satiation / restock / shift-duty
// cues — it's the load-bearing token the model echoes into move_to.
// ZBBS-HOME-349.
func renderAnchors(b *strings.Builder, v *AnchorsView, atPost bool, insideID sim.StructureID) {
	if v == nil {
		return
	}
	work := anchorPlace(v.WorkLabel, "your workplace")
	home := anchorPlace(v.HomeLabel, "your home")
	// You can't "head back to" the structure you're standing in — pointing the
	// model at the CURRENT structure's id is the LLM-214 no-op move it looped on
	// (Lewis Walker, inside the Walker Residence, calling move_to{residence} every
	// tick). When the actor is inside its own home/work, state that in-place (no move
	// id) and keep only the OTHER anchor as a reachable target.
	insideHome := v.HomeID != "" && insideID == v.HomeID
	insideWork := v.WorkID != "" && insideID == v.WorkID
	switch {
	case v.SamePlace:
		if insideHome { // SamePlace ⇒ insideHome and insideWork coincide
			b.WriteString("You're at your home and workplace.\n\n")
		} else {
			fmt.Fprintf(b, "Your home and your trade are both at %s (destination: %s) — you can head back there whenever you wish.\n\n", work, v.WorkID)
		}
	case v.WorkID != "" && v.HomeID != "":
		switch {
		case atPost:
			// On-shift AT its own post, the open "head to either whenever you wish"
			// invitation actively pulls an idle owner home (the Prudence shop↔house
			// oscillation, ZBBS-WORK-431). Home gets NO destination id and no open
			// condition (LLM-643): "head home once your work is done" read as
			// satisfied to an idle keeper with nothing to sell and nothing ripe —
			// Moses James lapped farm↔house on it, with the to-work yank marching
			// him straight back. "After you close" pins the departure to the close
			// hour the at-post duty steer states directly below. The id would not
			// stop the walk anyway — move_to resolves labels ("home") as well as
			// ids — but dropping it removes the echo bait (HOME-349); the off-shift
			// wind-down steer carries home's id when it is actually time to go.
			fmt.Fprintf(b, "You keep your trade at %s (destination: %s); your home is at %s — head home after you close.\n\n", work, v.WorkID, home)
		case insideHome:
			// Standing at home: its id is a no-op move target, so state it in-place and
			// keep the workplace as the reachable anchor (LLM-214).
			fmt.Fprintf(b, "You're home. You keep your trade at %s (destination: %s) — you can head there whenever you wish.\n\n", work, v.WorkID)
		case insideWork:
			// Standing at the workplace off-shift (atPost handles on-shift above): state
			// it in-place and keep home as the reachable anchor (LLM-214).
			fmt.Fprintf(b, "You're at your workplace. Your home is at %s (destination: %s) — you can head home whenever you wish.\n\n", home, v.HomeID)
		default:
			// Away from both: state the two anchors and stop. The old "— you can head
			// to either whenever you wish" tail was an open invitation to leave
			// whatever the actor is in the middle of, and it fired on EVERY tick away
			// from home and post. Live (LLM-528), it pulled the constable off his
			// rounds: he finished a real beat at the Ellis Farm — questioned the
			// keeper, paid her toward the nails she needed — said "I'll be about my
			// rounds", and then walked to the Meeting House anyway. The same
			// invitation was already found harmful at-post (the Prudence shop↔house
			// oscillation, see the atPost branch above, which reworded rather than
			// removed it). Both structure_ids stay — they are the load-bearing move_to
			// tokens (HOME-349); only the standing invitation goes. Where an actor
			// SHOULD head somewhere, a duty steer or errand cue says so with a reason.
			fmt.Fprintf(b, "You keep your trade at %s (destination: %s), and your home is at %s (destination: %s).\n\n", work, v.WorkID, home, v.HomeID)
		}
	case v.WorkID != "":
		if insideWork {
			b.WriteString("You're at your workplace.\n\n")
		} else {
			fmt.Fprintf(b, "You keep your trade at %s (destination: %s) — you can head back there whenever you wish.\n\n", work, v.WorkID)
		}
	case v.HomeID != "":
		if insideHome {
			b.WriteString("You're home.\n\n")
		} else {
			fmt.Fprintf(b, "Your home is at %s (destination: %s) — you can head back there whenever you wish.\n\n", home, v.HomeID)
		}
	}
}

// anchorPlace returns the sanitized structure label, or a generic fallback
// phrase when the structure has no DisplayName (the id still rides in the
// caller's line, so the target stays actionable even unlabeled).
func anchorPlace(label, fallback string) string {
	if label == "" {
		return fallback
	}
	return sim.WithDefiniteArticle(sanitizeInline(label))
}

// dutySteerToPostMarker is the distinguishing clause of the to-work steer's line,
// pulled out of the format string so a test can assert on the steer's PRESENCE or
// ABSENCE without copying its wording (LLM-540 — TestGoldensNoDutySteerWhileOnRounds
// pins that a constable mid-round is never argued back to his post, which is what
// lets shift duty keep waking him throughout a round he still owes). Naming it here means a
// reword either keeps this clause, and the test still matches, or changes it here,
// and the test follows — rather than going quietly vacuous against a stale literal.
const dutySteerToPostMarker = "you are away from your post"

// renderDutySteer writes the standing return-to-post cue (ZBBS-HOME-352) — the
// single voice for shift duty (the engine's ShiftDutyWarrant line is filtered
// out in Render). It carries the destination's structure_id inline — the
// load-bearing token the model echoes into move_to, matching the anchors /
// satiation / restock cues — so the cue is self-sufficient and does not depend
// on another section rendering the id (code_review).
func renderDutySteer(b *strings.Builder, v *DutySteerView) {
	if v == nil {
		return
	}
	// At-post stabilizer (ZBBS-WORK-431): on-shift, standing at your own post.
	// The symmetric complement to the to-work line — without it an idle owner
	// with no custom wanders off and the away-from-post arm drags it back
	// (Prudence shop↔house). The anchors line is reframed in tandem (renderAnchors
	// atPost) so the two cues agree: you belong here right now.
	//
	// LLM-337: dropped the explicit "wait here for customers rather than wandering
	// off" pin — a llama-era crutch that also suppressed legitimate restock trips
	// (a keeper leaving post to buy an off-circuit input, e.g. sage from the
	// apothecary). The stronger model doesn't need it: the mild "stay and look
	// after your work" steer plus the close time remain, and the away-from-post
	// arm still recovers a keeper that genuinely wandered.
	if v.AtPost {
		// State the close time (LLM-40) so "stay open later" is a bounded
		// decision — the model otherwise read the diligence cues as license to
		// extend with no customer present and no sense of how near close was.
		closeAt := ""
		if v.ShiftEndMin != nil {
			closeAt = sim.ClockHourProse(*v.ShiftEndMin)
		}
		// LLM-90: a bare sell-shelf plus ripe stock the forage cue names, and NOT
		// mid-customer (buildForage defers the harvest cue while a customer is engaged
		// at the stall). The default stabilizer's "stay and look after your work" steer
		// pulls against the forage cue's "walk out" — so swap it for a step-out-and-
		// return line the two cues agree on. Stepping out to restock an empty shelf is
		// tending the trade; the post stays the home base to come back to. The to-work
		// arm defers a forage errand (buildDutySteer), so the actor isn't yanked back
		// once set off.
		//
		// The two arms differ ONLY in whose stock is being fetched (LLM-622). Calling a
		// commons well "your own bushes" put the stabilizer at odds with the "## Free
		// sources you can gather from" section printed directly beneath it, which says
		// no one owns them — live for Joseph Scott at the Mill, 41 turns in 14 days.
		// The free-source arm claims nothing and borrows that section's own verb.
		switch v.ForageErrand {
		case ForageErrandOwnBushes:
			if closeAt != "" {
				fmt.Fprintf(b, "It is your working hours and you are at your post (you close at %s), but your shelves are bare — step out to your own bushes to restock, then return to your post.\n\n", closeAt)
			} else {
				b.WriteString("It is your working hours and you are at your post, but your shelves are bare — step out to your own bushes to restock, then return to your post.\n\n")
			}
			return
		case ForageErrandFreeSources:
			if closeAt != "" {
				fmt.Fprintf(b, "It is your working hours and you are at your post (you close at %s), but your shelves are bare — step out to gather what you need, then return to your post.\n\n", closeAt)
			} else {
				b.WriteString("It is your working hours and you are at your post, but your shelves are bare — step out to gather what you need, then return to your post.\n\n")
			}
			return
		}
		if v.SupplyErrand {
			// LLM-491: the buy-side twin of the forage reframe above. Some other
			// section of this same prompt is naming a place to go buy at — a
			// restock supplier, nails for a worn business, the season's shovels, wood
			// for a cold hearth — and the default "stay and look after your work"
			// line contradicts it outright. Live: Josiah Thorne was pinned to the
			// General Store and handed James Farm's destination id in the same turn.
			//
			// This line PERMITS rather than instructs, which is where it departs from
			// the forage variant. The forage cue's own "walk out to your bushes" is
			// the only movement voice in that case; here the supply section below
			// already carries both the imperative and the destination id, and a
			// second movement instruction in the steer would be two voices ordering
			// the same walk. So the stabilizer's job is only to stop arguing against
			// it — and to say the post is somewhere he comes back to, so a trip out
			// doesn't read as abandoning it for the day.
			if closeAt != "" {
				fmt.Fprintf(b, "It is your working hours and you are at your post (you close at %s), but what you need is not to be had here — going to fetch it and coming back is part of minding your trade.\n\n", closeAt)
			} else {
				b.WriteString("It is your working hours and you are at your post, but what you need is not to be had here — going to fetch it and coming back is part of minding your trade.\n\n")
			}
			return
		}
		if closeAt != "" {
			fmt.Fprintf(b, "It is your working hours and you are at your post — stay and look after your work; you close at %s.\n\n", closeAt)
		} else {
			b.WriteString("It is your working hours and you are at your post — stay and look after your work.\n\n")
		}
		return
	}
	// LLM-620 status arm. Mirrors the at-post line's shape deliberately (same
	// opening, same parenthesised close time): that pairing of ambient hour with
	// shift fact is what the model reads correctly at post. No destination id and no
	// verb, unlike the ToWork arm below — that is what keeps it a fact rather than
	// the yank its caller just chose to defer.
	if v.AwayFromPost {
		if v.ShiftEndMin != nil {
			fmt.Fprintf(b, "It is your working hours and "+dutySteerToPostMarker+" at %s (you close at %s).\n\n",
				anchorPlace(v.TargetLabel, "your workplace"), sim.ClockHourProse(*v.ShiftEndMin))
		} else {
			fmt.Fprintf(b, "It is your working hours and "+dutySteerToPostMarker+" at %s.\n\n",
				anchorPlace(v.TargetLabel, "your workplace"))
		}
		return
	}
	if v.ToWork {
		fmt.Fprintf(b, "It is your working hours, yet "+dutySteerToPostMarker+" — make your way to %s (destination: %s) now.\n\n",
			anchorPlace(v.TargetLabel, "your workplace"), v.TargetID)
		return
	}
	// Off-shift wind-down (ZBBS-WORK-387). Housing-dependent target line, plus —
	// for a keeper standing at its post — the stay_open choice appended after it.
	switch {
	case v.TargetID == "":
		// Homeless: no fixed place. The WHERE (rent a room / a shade tree) is
		// carried by the recovery-options cue; this is only the schedule beat.
		b.WriteString("Your working hours are over — it is time to close up for the night and find yourself a place to rest.")
	case v.Lodging:
		if l := sanitizeInline(v.TargetLabel); l != "" {
			fmt.Fprintf(b, "Your working hours are over — close up and head to your rented room at %s (destination: %s) to rest for the night.", l, v.TargetID)
		} else {
			fmt.Fprintf(b, "Your working hours are over — close up and head to your rented room at the inn (destination: %s) to rest for the night.", v.TargetID)
		}
	default:
		if l := sanitizeInline(v.TargetLabel); l != "" {
			fmt.Fprintf(b, "Your working hours are over and you are not yet home — head home to %s (destination: %s) now.", l, v.TargetID)
		} else {
			fmt.Fprintf(b, "Your working hours are over and you are not yet home — head home (destination: %s) now.", v.TargetID)
		}
	}
	// The stay-open choice: encouraged when a concrete reason is present, else
	// offered as a discretionary option. Always names that the closing hour must
	// be supplied (until_hour) — the stay_open tool requires it.
	if v.OfferStayOpen {
		if v.StayOpenReason != "" {
			fmt.Fprintf(b, " However, %s — if you wish to keep your business open later instead, call stay_open and state the hour you will close (until_hour).", v.StayOpenReason)
		} else {
			b.WriteString(" Or, if you have reason to keep your business open later instead, you may call stay_open and state the hour you will close (until_hour).")
		}
	}
	b.WriteString("\n\n")
}

// renderEveningLeisure writes the evening "tavern's open" cue (LLM-149) — a
// non-coercive invitation: the day's work is done, the tavern is open of an
// evening, and the agent may head over, pass a quiet evening at home, or turn in
// as it likes. It carries the tavern's structure_id (the new move_to token) and
// the home structure_id (the co-equal stay-home choice) inline, so the cue is
// self-sufficient like the duty steer. No imperative and no "turn in" pressure —
// the three options are equal-weight; bedtime is Lever 1's 22:00 gate. Renders
// in ## Around you, in the slot the off-shift go-home steer occupies the rest of
// the day (suppressed in-window so this is the single voice).
func renderEveningLeisure(b *strings.Builder, v *EveningLeisureView) {
	if v == nil {
		return
	}
	// LLM-335: a batch in the works pins the keeper to its post, so the invitation
	// yields to a quiet diegetic hold that agrees with the standing "you are making a
	// batch of X" line rather than contradicting it. Hung on "the batch" (singular) so
	// it reads for mass nouns (cheese) and count nouns (nails) alike, and names the
	// good the same way the in-flight line does ("a batch of Cheese"). No destination —
	// the steer is "stay put a little longer", and the invitation returns on the tick
	// the batch lands.
	if v.BatchHold {
		fmt.Fprintf(b, "Your day's work is nearly done, but the batch of %s still wants a few more minutes of your eye before you can call it a day.\n\n",
			sanitizeInline(v.BatchItemLabel))
		return
	}
	// LLM-345: the settled-in tier — the agent took the invitation and is standing in
	// the venue. The invitation has been acted on, so the cue stops offering places to
	// walk to and simply IS the room. No imperative and no "stay" instruction: the room
	// is the argument. The closing clause is the load-bearing one — it answers, in the
	// diegesis rather than as an instruction, the coda's "obligations before idle
	// matters", whose plain reading at seven in the evening sent the lingerer home.
	if v.SettledIn {
		fmt.Fprintf(b, "Your day's work is behind you, and here you are inside %s of an evening — the fire lit, the room warm. Whatever the morning asks of you can wait for the morning.\n\n",
			anchorPlace(v.VenueLabel, "the tavern"))
		return
	}
	venue := anchorPlace(v.VenueLabel, "the tavern")
	if v.HomeID == "" {
		// Transient traveler (LLM-373): no home of its own to offer as the stay-in
		// alternative — the tavern is where it passes the evening AND seeks its bed.
		fmt.Fprintf(b, "Your rounds are done, and the tavern is open of an evening — you might make your way to %s (destination: %s) for company and a bed for the night, or bide where you are, as you please.\n\n",
			venue, v.VenueID)
		return
	}
	home := anchorPlace(v.HomeLabel, "your home")
	fmt.Fprintf(b, "Your day's work is done, and the tavern is open of an evening — you might make your way to %s (destination: %s) for company, pass a quiet evening at %s (destination: %s), or turn in for the night, as you please.\n\n",
		venue, v.VenueID, home, v.HomeID)
}

// joinHuddleMembers renders co-huddle peers with name-vs-descriptor
// gating per Acquaintance. Acquainted → DisplayName; unacquainted with
// a Role → "the <role>"; otherwise → "a stranger". Mirrors v1's
// coLocatedHuddleMembers descriptor swap so unknown others don't get
// greeted by name.
func joinHuddleMembers(members []HuddleMember) string {
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = renderHuddleMember(m)
	}
	return strings.Join(parts, ", ")
}

func renderHuddleMember(m HuddleMember) string {
	return sanitizeInline(descriptorLabel(m.DisplayName, m.Role, m.Acquainted)) + laborTiePhrase(m.SolicitTie) + laboringPhrase(m) + eatingPhrase(m)
}

// laboringPhrase renders the LLM-231 busy annotation for a co-present member who is
// mid-job (fulfilling a Working LaborOffer). It names the employer when the subject
// can resolve them, otherwise omits the name. The wording signals the member is
// occupied and not a trade prospect right now — it deliberately does NOT say "won't
// respond" the way a sleeper is rendered: a laboring worker can still answer speech
// (LLM-230), it just shouldn't be pitched a sale. Empty for a non-laboring member.
func laboringPhrase(m HuddleMember) string {
	if !m.LaboringBystander {
		return ""
	}
	if m.LaboringForLabel != "" {
		return fmt.Sprintf(" (working a job for %s just now — not free to trade)", sanitizeInline(m.LaboringForLabel))
	}
	return " (working a job just now — not free to trade)"
}

// eatingPhrase annotates a co-present member who is mid item-dwell as busy at
// their meal in "## Around you" (LLM-416) — the proprietor-side half of the
// farewell-storm fix, so an onlooker reads a lingering diner as still eating
// rather than about to leave. Gated on m.Eating directly (no bystander split, as
// an eater has no employer to suppress the label toward). Names the dish when
// known, falling back to a bare "eating here". Empty for a non-eating member.
func eatingPhrase(m HuddleMember) string {
	if !m.Eating {
		return ""
	}
	if m.EatingItemLabel != "" {
		return fmt.Sprintf(" (eating %s just now)", sanitizeInline(m.EatingItemLabel))
	}
	return " (eating here just now)"
}

// busyActivityPhrase annotates a co-present member mid a timed source activity as
// busy in "## Around you" (LLM-440) — the observer-facing counterpart to the
// subject's own in-flight self-line (renderInFlightSourceActivity), so an onlooker
// reads a keeper deep in a repair/stoke/gather/bake as occupied rather than free to
// greet or pitch. Third-person, keyed on kind. Repair names the business it is bound
// to with the same "at <label>" framing and label source (resolveDwellPinLabel) the
// self-line uses, so the two can't drift; an unresolved label falls back to a
// place-less phrase. Stoke, gather, and bake need no place — the fire, the forager,
// and the home hearth are already at the observed spot. Empty for a member not mid a
// source activity. Like eatingPhrase, purely a legibility beat: it gates neither
// trade nor speech — and for bake it reads as "join her at the bread," not
// "interrupt her" (LLM-454).
func busyActivityPhrase(m HuddleMember) string {
	if !m.SourceActivityBusy {
		return ""
	}
	switch m.SourceActivityKind {
	case sim.SourceActivityRepair:
		// Sanitize before the emptiness check: a label that sanitizes down to nothing
		// must fall back to the place-less phrase, not render "(mending at  just now)".
		if label := sanitizeInline(m.SourceActivityLabel); label != "" {
			return fmt.Sprintf(" (mending at %s just now)", label)
		}
		return " (mending just now)"
	case sim.SourceActivityStoke:
		return " (tending the fire just now)"
	case sim.SourceActivityHarvest:
		return " (gathering just now)"
	case sim.SourceActivityBake:
		return " (at the hearth, baking just now)"
	}
	return ""
}

// laborTiePhrase names a co-present member's relationship to the subject —
// housemate or workmate (LLM-157) — so a worker reads them as kin/crew rather than a
// paid-work prospect, without the engine spelling out the instruction. Empty for
// laborTieNone, so it composes onto any member label without adding a separator of
// its own.
func laborTiePhrase(t laborTie) string {
	switch t {
	case laborTieHousehold:
		return " (your housemate)"
	case laborTieWorkplace:
		return " (your workmate)"
	default:
		return ""
	}
}

// renderNarrativeState writes the "## Who you are" section for shared-VA
// actors. Content-gated: a nil view skips the section entirely so
// stateful and PC actors don't see an empty block. The contract
// matches the perception note — Render is kind-agnostic; Build is the
// one that gates on Kind.
//
// The section opens with the actor's own name (LLM-432): the shared VA's
// system prompt is a generic sim context and the AboutMe prose doesn't
// reliably state the name, so without this line the model cannot tell
// whether overheard second-person speech ("ezekiel, you sleeping over
// there?") is addressed to it. The body is the actor's AboutMe — the
// accreting first-person soul the per-actor narrative sweep synthesizes
// each day via the dream-sim-soul agent (LLM-199). Build gates the view on
// having a name or a soul, so the section never renders as a bare header
// (the original empty-block bug). SeedText/EvolvingSummary are not rendered —
// SeedText is never populated for shared VAs, and EvolvingSummary was the
// frozen, unconsolidated diary prose that primed the repeat-pitch loop
// (ZBBS-WORK-374); the identity-framed soul prompt is what avoids that loop.
// Renders into the STABLE stream (LLM-501) — the section's bytes change at
// most nightly, so it belongs in the provider-cached zone, not the volatile
// per-tick body.
func renderNarrativeState(b *strings.Builder, n *NarrativeStateView) {
	if n == nil {
		return
	}
	// Gate on the SANITIZED values: a name or soul made only of content
	// sanitization strips (control characters, whitespace) must not emit a
	// bare header or a dangling "You are ." line. Build's raw-empty gate is
	// just the fast path; this is the authoritative check.
	name := sanitizeInline(n.Name)
	aboutMe := sanitizeInline(n.AboutMe)
	if name == "" && aboutMe == "" {
		return
	}
	b.WriteString("## Who you are\n")
	if name != "" {
		b.WriteString("You are ")
		b.WriteString(name)
		b.WriteString(".\n")
	}
	if aboutMe == "" {
		b.WriteString("\n")
		return
	}
	b.WriteString(aboutMe)
	b.WriteString("\n\n")
}

// renderVendorOperating writes the businessowner trade-conduct block — the
// operating rules that used to live in salem-vendor's startup_instructions (the
// memory-api <Instructions> system block) and drove the "instant room pitch on a
// bare Hello" sell-pressure. Moved engine-side (ZBBS-WORK-374) so the whole
// decision prompt is code-owned and the rules sit near the decision point rather
// than in a detached, far-away system preamble. Gated on AtOwnBusinessOperating
// — a businessowner physically at their own post (ZBBS-WORK-385) AND within
// operating hours (on shift, or staying open past close; LLM-123) — so it reaches
// vendors (innkeeper, farmers, shopkeepers) tending their business during the day,
// but not visitors, stateful NPCs, a keeper off-post in someone else's place, or a
// keeper standing at their own CLOSED post after hours (the off-shift forge<->Tavern
// oscillation). The scoped wording replaces "always be closing" with "a greeting is
// not a sale". ZBBS-HOME-385 restores the "tend to your trade" working framing that
// the WORK-374 port dropped (the producers were drifting off-post with nothing to
// do); kept generic ("your trade", not "your stall") since a vendor may keep a
// stall or a building.
//
// LLM-413 rebalances the block, which read as a one-way discount ratchet: "what
// goes unsold earns nothing" framed stock as pure loss, and the unconditional
// "when trade is slow, make a reasonable deal" was a standing licence to concede
// with no counterweight — nothing ever told a vendor to make a profit, which is
// survivable for a producer and fatal for the reseller who lives on the spread.
// Now: the tend line keeps its anti-idle intent without pricing stock at zero, a
// margin floor states the other side (a sale below what the goods cost is worse
// than no sale), and the concession line renders only when tradeSlow — the
// engine's own weekly sell-through judgment (keeperTradeSlow), not a model
// guess — and restates the floor even then.
func renderVendorOperating(b *strings.Builder, atOwnBusinessOperating, tradeSlow bool) {
	if !atOwnBusinessOperating {
		return
	}
	b.WriteString("How you trade:\n")
	b.WriteString("- Tend to your trade — your living depends on it. Look after your goods and your custom, and see to the day's business rather than let it pass idle.\n")
	b.WriteString("- You live by the difference between what your goods cost you — in coin or in labor — and what they fetch. Never sell a thing for less than it cost you: a sale at a loss is worse than no sale, for goods keep their worth on the shelf. Decline plainly if a stranger's purse is short.\n")
	b.WriteString("- If someone only greets you, greet them and let them state their business — don't quote prices or pitch your goods or rooms unless they ask or show interest.\n")
	if tradeSlow {
		b.WriteString("- Trade has been thin this week. A fair bargain is better than wares sitting idle — meet a willing buyer partway on price, but never below what a thing cost you.\n")
	}
	b.WriteString("Plain 1692 New England speech; no modern idioms.\n\n")
}

// renderKeeperAwayFromPost writes the buyer-side conduct rule a businessowner
// carries when it is away from its own business (LLM-611) — the only trade
// guidance it has off-post, since renderVendorOperating above is at-post only.
//
// The gap it closes: the at-post block's margin floor ("never sell a thing for
// less than it cost you") governs SELLING, and a keeper out restocking is
// buying. Josiah Thorne, the village distributor, ran every purchase from a
// purse holding 1 coin and settled it in goods instead — 289 units handed over
// as pay_items in a week against ~250 sold for coin, wheat bought at 0.43 and
// pushed straight back out as payment. Merchandise left the shop at whatever the
// seller granted for it, so a healthy per-unit margin (flour 1.02 in, 3.00 out)
// never converted to coin, and the whole village consolidated the resulting
// lowball bargaining as "he'll try the short count".
//
// Deliberately ONE line, and deliberately about valuation only. This rides every
// off-post prompt where the keeper has company, and it renders in exactly the
// situation WORK-385 narrowed the vendor cues to avoid — a keeper standing in
// someone else's place. A selling imperative here would recreate that bug
// (Prudence pitching Water mid-meal in John Ellis's tavern); a rule about what
// the keeper's own goods are worth when it pays with them cannot.
//
// The header is "How you buy:", NOT the at-post block's "How you trade:". The two
// blocks are mutually exclusive (at-post vs off-post), so a distinct header costs
// the model nothing and names the situation it is actually in — and it keeps the
// LLM-123 / LLM-413 cross-scenario invariants, which assert on the at-post block's
// header, asserting the thing they were written to assert.
func renderKeeperAwayFromPost(b *strings.Builder, awayFromPost bool) {
	if !awayFromPost {
		return
	}
	b.WriteString("How you buy:\n")
	b.WriteString("- Goods you hand over in payment come off your own shelf — count them at what they would fetch, not what they cost you.\n\n")
}

// renderOfferableCustomers writes the seller-side "offer your wares" cue
// (ZBBS-HOME-404): the businessowner's co-present customers, the goods they
// carry, and the scene_quote mechanism with its args spelled out — so the
// keeper LLM can proactively offer a sale instead of only reacting to a buyer's
// pay_with_item. It names the tool + arg form (the Finding-1 idiom: an
// actionable cue, not the bare "what goes unsold earns nothing" exhortation),
// but the decision stays with the model — it judges interest (the vendor
// block's "don't pitch unless they show interest" rule still governs) and sets
// the price, and the buyer keeps full accept/decline agency via pay_with_item.
// ZBBS-HOME-467 sharpens the in-cue trigger: scene_quote is for a ware the
// buyer has actually named (or asked the price of); a generic opener ("I'm
// hungry" / "what do you have") should get the menu, not a guessed-item quote.
// The constraint sits next to the tool here because the distant vendor-block
// "don't pitch unless they show interest" rule wasn't biting on a 70B keeper.
// Content-gated: a nil/empty view skips the section. Build guarantees both
// slices are non-empty when the view is non-nil.
func renderOfferableCustomers(b *strings.Builder, v *OfferableCustomersView) {
	if v == nil || len(v.CustomerNames) == 0 || len(v.Goods) == 0 {
		return
	}
	b.WriteString("## Custom at hand\n")
	who := joinNames(v.CustomerNames) // sanitizes each name inline
	verb := "is"
	if len(v.CustomerNames) > 1 {
		verb = "are"
	}
	goods := make([]string, 0, len(v.Goods))
	for _, g := range v.Goods {
		if s := sanitizeInline(g.Label); s != "" {
			// The on-hand count is the sizing fact (ZBBS-HOME-459): the cue asks
			// the seller to name a quantity, so it must see what it actually holds.
			// An inedible ingredient also carries its use (LLM-166), folded into
			// the same parens as the carry readout.
			switch {
			case g.Commission:
				// None on hand, but his to forge (LLM-619). Say both halves: that
				// the shelf is bare, and that the sale is still his to make. The
				// bare count alone read as "cannot sell" and sent the model to a
				// spoken promise that commits nothing.
				goods = append(goods, fmt.Sprintf("%s (none on hand — yours to make to order)", s))
			case g.Use != "":
				goods = append(goods, fmt.Sprintf("%s (%d on hand, %s)", s, g.OnHand, sanitizeInline(g.Use)))
			default:
				goods = append(goods, fmt.Sprintf("%s (%d on hand)", s, g.OnHand))
			}
		}
	}
	if len(goods) == 0 {
		// Defensive: Build filters raw empty labels, but a label could sanitize
		// down to empty — render nothing rather than an empty goods list.
		return
	}
	// LLM-343: the cue names ONE tool. It used to ask the seller to speak the
	// price and THEN call sell — but speak and sell are both tick-terminal, so
	// obeying that sentence in the order written ended the turn on the speech
	// and the offer was never posted. sell's `say` argument carries the words
	// now, making the price and the offer a single act.
	fmt.Fprintf(b, "%s %s here with you. If one of them names a specific good they want, or asks the price of a specific good, call sell — the named item and quantity in lines, your price in coins in amount, and the words you speak aloud in say, naming the coins outright rather than asking whether they would like to hear the price. The offer reaches their pay screen as you say it. Do not name a price with the speak tool: speaking ends your turn, and the offer would never be made. If they name several goods at once, give each its own line in the SAME offer under one total price. If they speak only in general — that they are hungry, ask what you have, or ask the cost of a meal without naming a dish — tell them what is for sale and let them choose; do not sell unless the buyer has named the good. Use target_buyer only for a named person you know; for a stranger or someone known only by trade, omit target_buyer to offer the whole room. The buyer is then free to take it or leave it.\n", who, verb)
	// ZBBS-HOME-407: the barter counterpart to the coin-sale cue above. When a
	// customer is carrying goods the keeper would rather have than coin, point
	// at offer_trade so a goods-for-goods swap has a legible execution path
	// instead of dissolving into a verbal agreement nothing commits. "goods that
	// travel" keeps the cue off eat-here food (a bowl of porridge in a buyer's
	// hands is a meal mid-eating, not trade stock) — asking for one is rejected
	// at resolvePayItems (LLM-445), so don't advertise it.
	fmt.Fprintf(b, "If one of them is carrying something you would rather have than coin — goods that travel, not a meal served to eat here — you can instead propose a direct trade — call offer_trade with the goods you will give and what you want from them, and the words you speak aloud in say. Do not put the trade to them with the speak tool: speaking ends your turn, and the offer would never be made. They are then free to accept, decline, or counter.\n")
	fmt.Fprintf(b, "Your goods to sell: %s.\n", strings.Join(goods, ", "))
	// LLM-619: the listing alone is not enough. With an empty shelf the model reads
	// a bare good as unsellable and answers with a spoken promise — "come back at
	// sunrise" — which commits nothing, leaves no order, and sends the buyer round
	// the same walk every tick. Say plainly that the sale is still his to make, and
	// name `sell` again so the path out is the tool and not the sentence. Rendered
	// only when something is actually commissionable, so a seller with a full shelf
	// never carries the line.
	for _, g := range v.Goods {
		if g.Commission {
			b.WriteString("A good you have none of but can make is still yours to sell — call sell for it as you would any other, and they pay now for what you forge after. Do not settle it with the speak tool: a promise spoken aloud binds nothing, and they will be back asking again.\n")
			break
		}
	}
	// LLM-171: a co-present customer who MAKES one of these goods is the wrong
	// person to pitch it to — your stock of it came from a maker like them. Name
	// the overlap so the keeper doesn't sell a smith his own skillet back (which a
	// 70B keeper otherwise does, reading the maker's own sell-offer as a buy-ask).
	for _, note := range v.ProducerNotes {
		fmt.Fprintf(b, "%s makes %s themselves — don't pitch those back to their own maker; offer them to other customers instead.\n", sanitizeInline(note.CustomerName), joinNames(note.Goods))
	}
	b.WriteString("\n")
}

// renderRelationships writes the "## What you remember of those here" section —
// the consolidated per-peer SUMMARY only. ZBBS-HOME-412 moved the turn-by-turn
// to "## Recent conversation here" (renderRecentConversation, sourced from the
// huddle ring for ALL NPCs), so the per-peer RecentFacts list is no longer
// rendered here — in particular the [heard] re-surface that drove the cross-tick
// re-pitch (a remembered ask read as a live one). A peer with no consolidated
// summary contributes nothing now, so the section is skipped entirely when no
// peer has one. (Still shared-VA-only: Build leaves Relationships nil for
// stateful/PC kinds.)
func renderRelationships(b *strings.Builder, peers []RelationshipPeerView) {
	if len(peers) == 0 {
		return
	}
	wrote := false
	for _, p := range peers {
		if strings.TrimSpace(p.SummaryText) == "" {
			continue
		}
		if !wrote {
			b.WriteString("## What you remember of those here\n")
			wrote = true
		}
		name := sanitizeInline(p.PeerName)
		if name == "" {
			name = string(p.PeerID)
		}
		fmt.Fprintf(b, "- %s: %s\n", name, sanitizeInline(p.SummaryText))
	}
	if wrote {
		b.WriteString("\n")
	}
}

// renderVillageWord writes the "## Word about the village" section (LLM-387): the
// fallible gossip the subject has picked up about residents who are NOT present —
// the word-of-mouth layer surfaced into perception. A first-hand line is framed
// as something the actor witnessed; a relayed line as talk going round Salem, so
// the model can weigh its own certainty: a first-hand line is a fact the actor
// witnessed and can trust, while a relayed line is marked as hearsay ("Word has
// it that ..."), so an NPC may repeat it, doubt it, or embroider it further when
// it speaks. The header is written lazily on the first non-empty line, so a list
// of all-empty clauses renders nothing.
func renderVillageWord(b *strings.Builder, rumors []VillageRumorView) {
	if len(rumors) == 0 {
		return
	}
	wrote := false
	for _, r := range rumors {
		clause := sanitizeInline(r.Clause)
		if clause == "" {
			continue
		}
		if !wrote {
			b.WriteString("## Word about the village\n")
			wrote = true
		}
		if r.FirstHand {
			fmt.Fprintf(b, "- You saw it yourself: %s.\n", clause)
			continue
		}
		fmt.Fprintf(b, "- Word has it that %s.\n", clause)
	}
	if wrote {
		b.WriteString("\n")
	}
}

// renderRecentConversation writes the "## Recent conversation here" section
// (ZBBS-HOME-412) — the huddle's last few spoken turns, oldest-first, marking
// the subject's own lines "You said" and everyone else "<Name> said". This is
// the cross-tick conversational continuity EVERY NPC (stateful included) and the
// player's own lines feed into, so a re-engaging actor sees that it already
// spoke and what was just asked, instead of re-pitching. Each line carries an
// interval stamp ("said (40s ago):") measured against renderedAt (LLM-217), so
// the model can tell rapid-fire churn from a normally paced exchange; the stamp
// is omitted when either clock is missing (hand-built payloads). Empty list
// skips the section.
func renderRecentConversation(b *strings.Builder, lines []UtteranceView, renderedAt time.Time) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("## Recent conversation here\n")
	for _, u := range lines {
		text, _ := sanitizeText(u.Text, 0)
		said := "said"
		if stamp := AgoPhrase(u.At, renderedAt); stamp != "" {
			said = "said (" + stamp + ")"
		}
		if u.IsSelf {
			fmt.Fprintf(b, "- You %s: %s\n", said, text)
			continue
		}
		name := sanitizeInline(u.SpeakerName)
		if name == "" {
			name = "someone"
		}
		fmt.Fprintf(b, "- %s %s: %s\n", name, said, text)
	}
	b.WriteString("\n")
}

// AgoPhrase renders how long before now `at` happened, in the coarse buckets a
// prompt line needs: "just now" inside 15s, whole seconds under 90s, then whole
// minutes, then whole hours under a day. Past a day (LLM-390) the buckets go
// prose — "a day ago", "two days ago", "a week ago", "a month ago" — because
// the long scales read inside sentences (recall's "From two days ago — <topic>")
// where "49h ago" would not. Duration-based, not calendar-based: "a day ago" is
// 24–48h elapsed, which can differ by one from the calendar-day count around
// midnight — acceptable at this granularity, and it keeps the helper free of
// the village timezone. Returns "" when either clock is zero (hand-built
// test payloads) — callers drop the stamp rather than show a bogus interval.
// LLM-217. Exported for the recall tool's memory-age framing (LLM-390).
func AgoPhrase(at, now time.Time) string {
	if at.IsZero() || now.IsZero() {
		return ""
	}
	d := now.Sub(at)
	day := 24 * time.Hour
	switch {
	case d < 15*time.Second:
		// Covers negative deltas too: an At a hair after the snapshot's
		// publish instant (clock race) clamps to "just now" rather than
		// leaking a nonsense future interval.
		return "just now"
	case d < 90*time.Second:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < day:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 2*day:
		return "a day ago"
	case d < 7*day:
		return countWord(int(d/day)) + " days ago"
	case d < 14*day:
		return "a week ago"
	case d < 30*day:
		return countWord(int(d/(7*day))) + " weeks ago"
	case d < 60*day:
		return "a month ago"
	case d < 365*day:
		return countWord(int(d/(30*day))) + " months ago"
	default:
		return "over a year ago"
	}
}

// renderRoundsStopsAhead voices how much of the constable's circuit still lies
// ahead of the stop he is standing at (LLM-524), or "" at the final stop (n <= 0),
// where there is nothing left to name. Spelled as a word, in the round's own
// register — the count is scene, not statistic.
//
// It NAMES the next stop (LLM-530), which is the whole point: move_to is how this
// NPC says "I am finished with this place", so the round has to give him somewhere
// to say it about. It previously did not, and his own post was the only place the
// prompt named — so every tour ended there, three times over, each with him stating
// he meant to carry on ("I'll continue my rounds", "nothing more for me here").
// An earlier attempt to talk him out of moving at all ("your feet will carry you
// on") failed twice over: it never actually said that something else was doing the
// walking, and it asked him to end his turn at the exact moment his own conclusion
// was that he was done here. Naming the next business works WITH that instinct
// instead of against it; advanceActiveRoute treats a walk to a stop still on the
// circuit as staying on the round, not leaving it.
func renderRoundsStopsAhead(n int, next string) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	if n == 1 {
		b.WriteString("One more place on your round still lies ahead of you.")
	} else {
		word := countWord(n)
		fmt.Fprintf(&b, "%s more places on your round still lie ahead of you.",
			strings.ToUpper(word[:1])+word[1:])
	}
	if next != "" {
		fmt.Fprintf(&b, " The next is the %s.", sanitizeInline(next))
	}
	b.WriteString("\n")
	return b.String()
}

// countWord spells out the small counts the AgoPhrase prose buckets produce
// (2–12 covers every reachable value: days cap at 6, weeks at 4, months at 12).
// Falls back to digits defensively.
func countWord(n int) string {
	words := map[int]string{
		2: "two", 3: "three", 4: "four", 5: "five", 6: "six", 7: "seven",
		8: "eight", 9: "nine", 10: "ten", 11: "eleven", 12: "twelve",
	}
	if w, ok := words[n]; ok {
		return w
	}
	return fmt.Sprintf("%d", n)
}

// renderSelfActions writes the "## What you've recently done" section (LLM-217)
// — the subject's own recent committed actions, most-recent-first, each with an
// interval stamp. This is the self-action memory that lets a vacillating NPC
// SEE its own churn ("You arrived at the Tavern (just now). You left the Tavern
// (2m ago). You arrived at the Tavern (4m ago).") and break the loop — the
// information gap behind the live go-home ↔ seek-work oscillation. Phrasing
// mirrors the talk-panel narration (httpapi renderActionLogEntry) in second
// person; an entry whose type has no phrasing here should not have passed the
// build-side selfActionTrailTypes filter, but is skipped defensively. Empty
// list skips the section.
func renderSelfActions(b *strings.Builder, actions []SelfActionView, renderedAt time.Time) {
	if len(actions) == 0 {
		return
	}
	wrote := false
	for _, a := range actions {
		line := selfActionLine(a)
		if line == "" {
			continue
		}
		if !wrote {
			b.WriteString("## What you've recently done\n")
			b.WriteString("Most recent first.\n")
			wrote = true
		}
		if stamp := AgoPhrase(a.At, renderedAt); stamp != "" {
			fmt.Fprintf(b, "- %s (%s)\n", line, stamp)
			continue
		}
		fmt.Fprintf(b, "- %s\n", line)
	}
	if wrote {
		b.WriteString("\n")
	}
}

// selfActionLine phrases one SelfActionView second-person, no trailing period
// (the interval stamp follows). Degrades on missing counterparty/amount the
// same way the talk-panel narration does. Returns "" for a type it can't
// phrase.
func selfActionLine(a SelfActionView) string {
	coins := func(n int) string {
		if n == 1 {
			return "1 coin"
		}
		return fmt.Sprintf("%d coins", n)
	}
	switch a.ActionType {
	case sim.ActionTypeSpoke:
		text, _ := sanitizeText(a.Text, 0)
		if text == "" {
			return ""
		}
		return fmt.Sprintf("You said: %q", text)
	case sim.ActionTypePaid:
		if a.CounterpartyName == "" {
			return "You made a payment"
		}
		line := "You paid " + sanitizeInline(a.CounterpartyName)
		// LLM-374: render the full tender — coins AND any barter goods — so a
		// pay_with_item settlement doesn't read as coins-only. FormatPayment
		// yields "4 coins and 3 cheese", "3 cheese", or "4 coins" as appropriate.
		if a.Amount > 0 || len(a.PayItems) > 0 {
			line += " " + sim.FormatPayment(a.Amount, a.PayItems)
		}
		if a.Text != "" {
			line += " for " + sanitizeInline(a.Text)
		}
		return line
	case sim.ActionTypeConsumed:
		if a.Text == "" {
			return ""
		}
		return "You consumed " + sanitizeInline(a.Text)
	case sim.ActionTypeDelivered:
		if a.Text == "" {
			return ""
		}
		line := "You delivered " + sanitizeInline(a.Text)
		if a.CounterpartyName != "" {
			line += " to " + sanitizeInline(a.CounterpartyName)
		}
		return line
	case sim.ActionTypeWalked:
		if a.Text == "" {
			return "You arrived"
		}
		if a.FoundShut {
			// LLM-366: name the dead end so a churn of these reads as dead ends.
			return "You went to " + sim.WithDefiniteArticle(sanitizeInline(a.Text)) + " but found it shut, no one tending it"
		}
		return "You arrived at " + sim.WithDefiniteArticle(sanitizeInline(a.Text))
	case sim.ActionTypeDeparted:
		if a.Text == "" {
			return "You left"
		}
		return "You left " + sim.WithDefiniteArticle(sanitizeInline(a.Text))
	case sim.ActionTypeTookBreak:
		return "You stepped away to rest"
	case sim.ActionTypeLabored:
		switch {
		case a.Amount > 0 && a.CounterpartyName != "":
			return "You earned " + coins(a.Amount) + " working for " + sanitizeInline(a.CounterpartyName)
		case a.Amount > 0:
			return "You earned " + coins(a.Amount) + " for a job"
		case a.CounterpartyName != "":
			return "You finished a job for " + sanitizeInline(a.CounterpartyName)
		default:
			return "You finished a job"
		}
	case sim.ActionTypeSolicitedWork:
		switch {
		case a.Amount > 0 && a.CounterpartyName != "":
			return "You offered to work for " + sanitizeInline(a.CounterpartyName) + " for " + coins(a.Amount)
		case a.CounterpartyName != "":
			return "You offered to work for " + sanitizeInline(a.CounterpartyName)
		default:
			return "You offered to work for coin"
		}
	case sim.ActionTypeOfferedWork:
		// LLM-564: the employer-side mint, written from the employer's seat.
		switch {
		case a.Amount > 0 && a.CounterpartyName != "":
			return "You asked " + sanitizeInline(a.CounterpartyName) + " to work for you for " + coins(a.Amount)
		case a.CounterpartyName != "":
			return "You asked " + sanitizeInline(a.CounterpartyName) + " to work for you"
		default:
			return "You offered someone work for pay"
		}
	case sim.ActionTypeHired:
		switch {
		case a.Amount > 0 && a.CounterpartyName != "":
			return "You hired " + sanitizeInline(a.CounterpartyName) + " for " + coins(a.Amount)
		case a.CounterpartyName != "":
			return "You hired " + sanitizeInline(a.CounterpartyName)
		default:
			return "You took someone on"
		}
	default:
		return ""
	}
}

// fallbackToday derives the order-book "today" for a hand-built payload that
// supplied no village calendar date (LocalDateUTC zero): the UTC day of the
// render instant (now) when present, else the host UTC day. A real snapshot
// always supplies LocalDateUTC, so this is only reached by hand-built test
// payloads — deriving from `now` keeps such a fixture deterministic when it sets
// a clock, and only a fully clockless payload touches the wall clock. LLM-106.
func fallbackToday(now time.Time) time.Time {
	if now.IsZero() {
		return startOfUTCDay(time.Now())
	}
	return startOfUTCDay(now)
}

// renderPendingDeliveriesFromMe writes the seller-side order book, split by the
// order's ReadyBy date (ZBBS-HOME-403): orders due today (or earlier) render as
// "## Orders to deliver" — the actionable hand-over section — and orders booked
// for a future day render as "## Upcoming bookings" — a passive reservation
// list with no deliver_order nudge. Empty list skips both.
//
// Phase 3 PR S6 — surfacing pending deliveries to the seller's LLM
// is the load-bearing perception mechanism (no warrant kind for
// Order state; the seller relies on baseline perception to remember
// to call deliver_order).
func renderPendingDeliveriesFromMe(b *strings.Builder, orders []OrderView, today, now time.Time) {
	if len(orders) == 0 {
		return
	}
	if today.IsZero() {
		today = fallbackToday(now)
	}
	var ready, future []OrderView
	for _, o := range orders {
		// A future booking renders as a reservation; everything else (due
		// today, overdue, or with no booked date) is ready to hand over now.
		if !o.ReadyBy.IsZero() && startOfUTCDay(o.ReadyBy).After(today) {
			future = append(future, o)
		} else {
			ready = append(ready, o)
		}
	}
	renderOrdersReadyToHandOver(b, ready, now)
	renderFutureReservations(b, future)
}

// renderOrdersReadyToHandOver writes the actionable "## Orders to deliver"
// section — one line per order due now, with the deliver_order nudge.
//
// ZBBS-WORK-372 — the section closes with an explicit actionable
// instruction naming the deliver_order tool + order_id arg, mirroring
// the pay-offer section. Before this, a bare list of order ids read as
// data, not an action: keepers spoke a delivery promise and never fired
// the tool, so orders sat open forever (boot-collapse Finding 1).
//
// ZBBS-WORK-373 — co-presence gate. DeliverOrder's gate 6 rejects a handover to
// any recipient not sharing the seller's huddle, so an order whose recipient has
// stepped away renders passively ("waiting for X to return"), and the actionable
// instruction is suppressed unless at least one order is deliverable now — the
// keeper isn't cued to chase an absent buyer (boot-collapse Finding 6 bundle).
func renderOrdersReadyToHandOver(b *strings.Builder, orders []OrderView, now time.Time) {
	if len(orders) == 0 {
		return
	}
	b.WriteString("## Orders to deliver\n")
	anyDeliverable := false
	for _, o := range orders {
		itemDesc := string(o.Item)
		if o.Qty > 1 {
			itemDesc = fmt.Sprintf("%d %s", o.Qty, o.Item)
		}
		buyer := sanitizeInline(o.BuyerName)
		fmt.Fprintf(b, "- #%d: %s for %s", uint64(o.ID), itemDesc, buyer)
		if len(o.ConsumerNames) > 0 {
			fmt.Fprintf(b, " (to deliver to: %s)", sanitizeInline(strings.Join(o.ConsumerNames, ", ")))
		}
		// Two gates decide whether this order is deliverable NOW — if not, render
		// it passively and don't count it toward the deliver_order instruction:
		//   - Commission not yet forged (LLM-338): the seller took payment for a
		//     good it still has to make, so DeliverOrder gate 5 (stock) would bounce
		//     a deliver_order call. Steer to making it, not into a bounce loop.
		//   - Absent recipient (ZBBS-WORK-373): the recipient isn't in the seller's
		//     huddle, so gate 6 (co-presence) would reject the handover — never name
		//     the absent buyer as a chase target.
		switch {
		case o.AwaitingMake:
			b.WriteString(" — you've yet to make it")
			// LLM-635: name the input the make is stalled on, so the line states
			// WHY the good isn't made rather than reading as a standing "forge it"
			// while the produce tool is withdrawn (LLM-324). The keeper then knows
			// the noun to fetch (the "## Restocking" / forage cues carry the where)
			// instead of promising sundown delivery he has no tool for.
			if len(o.MissingInputs) > 0 {
				fmt.Fprintf(b, ", and you've no %s for it", missingInputsPhrase(o.MissingInputs))
			}
		case len(o.AbsentRecipientNames) > 0:
			fmt.Fprintf(b, " — waiting for %s to return", sanitizeInline(strings.Join(o.AbsentRecipientNames, ", ")))
		}
		// The same DeliverableNow predicate the deliver_order tool-advertising gate
		// reads (handlers.gateTools), so the cue's instruction and the tool surface
		// together — never one without the other.
		// Partial-payment commission (LLM-357): show what's still owed so the
		// keeper knows to collect the balance when handing it over.
		b.WriteString(balanceClause(o, false))
		if o.DeliverableNow() {
			anyDeliverable = true
		}
		if clause, ok := expiryClause(o.ExpiresAt, now); ok {
			b.WriteString(clause)
		}
		b.WriteString("\n")
	}
	// Only surface the actionable instruction when at least one order is
	// deliverable now. Telling the keeper to "call deliver_order — the recipient
	// must be here" while nobody is present is the exact absent-recipient chase
	// the gate guards against.
	if anyDeliverable {
		// ZBBS-WORK-373 handover-line nudge: deliver_order is silent (it moves the
		// goods + writes the interaction fact, no speech) and non-terminal, so the
		// keeper can chain deliver_order(N) -> speak in the same tick. Ask for the
		// line so a delivery reads as "here's your bread, Ezekiel" rather than items
		// silently appearing — the "actions are socially expressed" convention.
		b.WriteString("To hand one of these over, call deliver_order with the order's number as order_id (the recipient must be here with you). The handover itself is silent, so say a word to them as you pass it across.\n")
	}
	b.WriteString("\n")
}

// renderFutureReservations writes the seller's upcoming bookings — orders whose
// ReadyBy hasn't arrived yet. Framed as reservations, not deliveries, with an
// explicit "don't hand over yet" so the keeper doesn't waste a deliver_order on
// a booking that isn't due (DeliverOrder's gate 4b rejects a premature check-in,
// so the call would fail anyway) or forget the booking exists. The live case is
// an advance lodging booking. ZBBS-HOME-403.
func renderFutureReservations(b *strings.Builder, orders []OrderView) {
	if len(orders) == 0 {
		return
	}
	b.WriteString("## Upcoming bookings\n")
	for _, o := range orders {
		itemDesc := string(o.Item)
		if o.Qty > 1 {
			itemDesc = fmt.Sprintf("%d %s", o.Qty, o.Item)
		}
		buyer := sanitizeInline(o.BuyerName)
		fmt.Fprintf(b, "- #%d: %s for %s — booked for %s\n",
			uint64(o.ID), itemDesc, buyer, o.ReadyBy.Format("Mon Jan 2"))
	}
	b.WriteString("These aren't due yet — don't hand them over until the booked day arrives.\n\n")
}

// renderPendingDeliveriesToMe writes the buyer/consumer-side view, split by the
// order's ReadyBy date (ZBBS-HOME-403): orders still within their window render
// as "## Orders you're waiting on", and orders whose ReadyBy has passed without
// delivery render as "## Overdue — paid but not delivered" — the buyer-side
// robbery cue. Empty list skips both.
//
// Phase 3 PR S6 — gives the buyer's LLM a structured "I'm waiting
// for X from Y" cue so they can speak follow-ups ("Hannah, where's
// my stew?") or make wait/depart decisions.
func renderPendingDeliveriesToMe(b *strings.Builder, orders []OrderView, today, now time.Time) {
	if len(orders) == 0 {
		return
	}
	if today.IsZero() {
		today = fallbackToday(now)
	}
	var waiting, overdue []OrderView
	for _, o := range orders {
		if !o.ReadyBy.IsZero() && startOfUTCDay(o.ReadyBy).Before(today) {
			overdue = append(overdue, o)
		} else {
			waiting = append(waiting, o)
		}
	}
	renderOrdersWaitingOn(b, waiting, now)
	renderOverdueOrders(b, overdue)
}

// renderOrdersWaitingOn writes the buyer's "## Orders you're waiting on"
// section — one line per order still within its delivery window.
func renderOrdersWaitingOn(b *strings.Builder, orders []OrderView, now time.Time) {
	if len(orders) == 0 {
		return
	}
	b.WriteString("## Orders you're waiting on\n")
	for _, o := range orders {
		seller := sanitizeInline(o.SellerName)
		// "<seller> owes you <n> <item>" states the direction explicitly. The
		// prior "<item> from <seller>" left "from" as the only direction cue, and
		// the weak model flipped it — memorizing the buyer as the debtor who owes
		// the seller the goods (LLM-512). "owes you" is unflippable. The count is
		// always shown so the singular reads naturally ("owes you 1 shovel").
		fmt.Fprintf(b, "- #%d: %s owes you %d %s", uint64(o.ID), seller, o.Qty, o.Item)
		// Partial-payment commission (LLM-357): the balance the buyer still owes
		// and must bring to collect.
		b.WriteString(balanceClause(o, true))
		if clause, ok := expiryClause(o.ExpiresAt, now); ok {
			b.WriteString(clause)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// renderOverdueOrders writes the buyer's "## Overdue" section — orders the buyer
// paid for whose ReadyBy has passed but the seller still hasn't delivered (the
// orders carried here are all still Ready; buildPendingOrderViews drops terminal
// ones). The live case is a lodging booking the keeper never honored. The cue is
// informative, not prescriptive — the LLM decides whether to chase, complain, or
// let it go; the engine refunds the coins when the order finally expires
// (ZBBS-HOME-403).
func renderOverdueOrders(b *strings.Builder, orders []OrderView) {
	if len(orders) == 0 {
		return
	}
	b.WriteString("## Overdue — paid but not delivered\n")
	for _, o := range orders {
		seller := sanitizeInline(o.SellerName)
		// Same explicit "<seller> owes you" direction as renderOrdersWaitingOn
		// (LLM-512) — the buyer is owed the goods, never the debtor.
		fmt.Fprintf(b, "- #%d: %s owes you %d %s — was due %s, still not delivered\n",
			uint64(o.ID), seller, o.Qty, o.Item, o.ReadyBy.Format("Mon Jan 2"))
	}
	b.WriteString("\n")
}

// balanceClause renders the partial-payment scene (LLM-357) for an open order
// that still carries an unpaid balance — a diegetic "N down, M to come" line,
// not a stat dump. Empty for a full-prepay order (BalanceDue == 0). isBuyer
// flips the voice: the buyer owes the balance and brings it; the seller collects
// it on handover.
func balanceClause(o OrderView, isBuyer bool) string {
	if o.BalanceDue <= 0 {
		return ""
	}
	if isBuyer {
		return fmt.Sprintf(" — you've put %d coins down; %d still to settle when you collect", o.DepositPaid, o.BalanceDue)
	}
	return fmt.Sprintf(" — %d coins down; %d still to collect when you hand it over", o.DepositPaid, o.BalanceDue)
}

// startOfUTCDay returns midnight UTC of the calendar date `t` falls on. Shared
// by the order-book date splits; ReadyBy is already midnight UTC of its date, so
// applying this to it is a defensive normalization (and gives a single notion of
// "today" to compare against). ZBBS-HOME-403.
func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// maxRenderableExpiryHorizon bounds how far out an order deadline can be and
// still render a literal "expires in X" clause. An order's real TTL is minutes
// (OrderTTLDefault is 10m), so anything beyond a generous day is not a real
// deadline — it is the NULL-expires_at sentinel the PG loader substitutes for
// legacy v1 rows (9999-12-31, orders.go), or an overflow. Feeding that to
// humanizeDurationUntil produced "expires in 153722867 minutes" (~292 years —
// time.Time.Sub saturating at MaxInt64 ns) in a live NPC's prompt (ZBBS-HOME-357).
const maxRenderableExpiryHorizon = 24 * time.Hour

// expiryClause returns the " — expires in X" suffix for an order deadline, and
// ok=false (render nothing) when there is no meaningful expiry: a zero deadline
// (never set) OR an implausibly-far one (the legacy NULL sentinel / an overflow
// — see maxRenderableExpiryHorizon). Gating on the horizon here, at the render
// boundary, fixes the garbage duration regardless of which upstream sentinel or
// overflow produced the far-future time. ZBBS-HOME-357.
func expiryClause(deadline, now time.Time) (string, bool) {
	// No deadline, or no render clock (a hand-built payload that supplied no
	// PublishedAt → RenderedAt): nothing meaningful to render. The explicit
	// now-zero guard keeps "no clock omits expiry" obvious here rather than
	// leaning on the far-future-horizon check below to swallow deadline.Sub(zero).
	// LLM-106.
	if deadline.IsZero() || now.IsZero() {
		return "", false
	}
	if deadline.Sub(now) > maxRenderableExpiryHorizon {
		return "", false
	}
	return " — expires in " + humanizeDurationUntil(deadline, now), true
}

// humanizeDurationUntil renders a coarse "X minute(s)" string for a
// future time relative to now. Returns "now" when the deadline has
// passed (clamped to 0) — keeps the render readable even if a clock
// drift causes a brief past-due window before the sweep flips state.
func humanizeDurationUntil(deadline, now time.Time) string {
	d := deadline.Sub(now)
	if d <= 0 {
		return "now"
	}
	mins := int(d / time.Minute)
	if mins <= 0 {
		return "<1 minute"
	}
	if mins == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", mins)
}

// renderScene renders the loop-detection cue — "what's changed since you got
// here" — when a scene baseline is established. The raw "scene: <uuid> — origin
// <kind>" header and the "(missing_no_scene)" baseline enum it used to print
// were engine jargon the LLM can't use, so they're gone; the no-scene case now
// renders nothing at all rather than an empty diagnostic. ZBBS-HOME-339.
func renderScene(b *strings.Builder, p Payload) {
	// ZBBS-WORK-374: render the loop-detection cue only when a real baseline diff
	// exists. The missing-baseline branch used to print "You can't yet tell
	// whether anything has changed." — pure filler that carries no loop signal
	// (the actual stuck-loop signal is the BaselinePresent + AnyChange==false case
	// in renderDiff, unaffected here). Dropping it removes a noise section from
	// conversational and freshly-joined ticks without weakening loop detection.
	if p.Primary == nil || p.Baseline != BaselinePresent {
		return
	}
	b.WriteString("## Since you got here\n")
	b.WriteString(renderDiff(p.Primary.Diff))
	b.WriteString("\n\n")
}

// renderDiff renders the loop-detection line as felt prose. When nothing
// changed it says so explicitly — the "you may be looping" signal — but it
// never asserts "no change" unless the Diff is real (Build only attaches a
// Diff for BaselinePresent).
func renderDiff(d *Diff) string {
	if d == nil {
		return "You can't yet tell whether anything has changed."
	}
	if !d.AnyChange {
		return "Nothing about your situation has changed — if this keeps up, you may be repeating yourself."
	}
	var parts []string
	if d.StateChanged {
		parts = append(parts, "what you're doing")
	}
	if d.PositionChanged {
		parts = append(parts, "where you stand")
	}
	if d.StructureChanged {
		parts = append(parts, "where you are")
	}
	if d.HuddleChanged {
		parts = append(parts, "who you're with")
	}
	if d.CoinsChanged {
		parts = append(parts, "your coins")
	}
	if d.InventoryChanged {
		parts = append(parts, "what you're carrying")
	}
	if d.NeedsChanged {
		parts = append(parts, "how you feel")
	}
	return "What's changed: " + strings.Join(parts, ", ") + "."
}

// renderWarrants renders the "since your last turn" section and fills in the
// RenderedPrompt accounting. Warrants arrive already ordered by
// SourceEventID (Build's job); the caps are applied here, after ordering,
// and any warrant past a cap is moved to DroppedWarrants for carry-forward.
// PendingPayOffers returns the offers currently pending against this actor
// as seller — the payload's standing ledger view (Build's buildPayOffersForMe
// scan over snap.PayLedger, ZBBS-HOME-453). It is the single source of truth
// shared by the perception offer-decision section (renderPayOffers, below)
// and the handlers tool-gate (gateTools): the rendered offer and the
// advertised accept_pay/decline_pay/counter_pay tools both key off this one
// predicate so they cannot drift.
//
// Until HOME-453 this read the consumed warrant batch instead — which gave
// the seller exactly ONE tick with the cue and the tools (the warrant is
// consumed by the tick it triggers), and a seller who spoke through it was
// locked out of resolving until the TTL sweep expired the offer.
//
// Contract: the "these offers are pending against p.ActorID as seller"
// invariant is established by Build (buildPayOffersForMe filters on
// SellerID == subject and State == Pending); this accessor trusts the
// field and does not re-verify it — the projection shape carries no
// SellerID to verify against. A payload assembled outside Build (tests)
// is responsible for honoring that invariant itself.
func PendingPayOffers(p Payload) []sim.PayOfferWarrantReason {
	return p.PayOffersForMe
}

// PendingLaborOffers returns the labor offers currently pending against this
// actor as EMPLOYER — the payload's standing ledger view (Build's
// buildLaborOffersForMe scan over snap.LaborLedger, LLM-26). The single source
// of truth shared by the perception decision section (renderLaborOffers) and
// the handlers tool-gate (gateTools): the rendered offer and the advertised
// accept_work/decline_work tools both key off this one predicate so they
// cannot drift (discussion-109). Same contract as PendingPayOffers — Build
// established the "pending against subject as employer" invariant; this
// accessor trusts the field.
func PendingLaborOffers(p Payload) []LaborOfferView {
	return p.LaborOffersForMe
}

// nonPayOfferWarrants returns the consumed batch with pay-offer warrants
// removed — they render in the dedicated decision section (renderPayOffers)
// instead of the generic "since your last turn" list, so they must not also
// appear there, nor consume the warrant-section cap / carry-forward budget
// (a rendered offer is addressed).
func nonPayOfferWarrants(warrants []sim.WarrantMeta) []sim.WarrantMeta {
	out := make([]sim.WarrantMeta, 0, len(warrants))
	for _, w := range warrants {
		if _, ok := w.Reason.(sim.PayOfferWarrantReason); ok {
			continue
		}
		out = append(out, w)
	}
	return out
}

// nonStandingCueWarrants returns the consumed batch with the warrants removed whose
// subject a STANDING CUE already voices. They still drive the wake tick; their own
// line is not rendered, because rendering it would say the same thing twice in one
// prompt and the cue says it better.
//
//   - Shift duty → the DutySteer cue (renderDutySteer) is the single voice for
//     return-to-post (ZBBS-HOME-352).
//   - Constable rounds → the rounds cue is the single voice for what he owes and
//     where to go next (LLM-549). Without this the prompt would carry the scene
//     ("Six more places on your round still lie ahead of you. The next is the Ellis
//     Farm.") and then the generic fallback line naming the raw warrant kind
//     underneath it — a stat beside the scene it duplicates.
//
// Dropping them here also keeps them out of the warrant-section cap / carry-forward
// budget; consuming them unrendered is correct since their purpose (waking the
// actor) is already done.
//
// Only add a kind here when a cue genuinely carries its content. A wake with no
// voice anywhere is a tick the model cannot account for.
func nonStandingCueWarrants(warrants []sim.WarrantMeta) []sim.WarrantMeta {
	out := make([]sim.WarrantMeta, 0, len(warrants))
	for _, w := range warrants {
		switch w.Reason.(type) {
		case sim.ShiftDutyWarrantReason, sim.ConstableRoundsWarrantReason:
			continue
		}
		out = append(out, w)
	}
	return out
}

// renderPayOffers renders the pending-pay-offer decision section: one line
// per offer carrying the ledger_id (the load-bearing field — the model must
// echo it back into accept_pay/decline_pay/counter_pay), the buyer, the goods
// (qty x item), the amount, and whether the buyer wants it consumed now or
// kept. There is no untrusted free-text payload, so nothing is truncated;
// buyer and item are structurally sanitized like other inline fields.
//
// Uncapped by design: pay offers are inherently few (bounded by co-present
// buyers), and the section must always carry the ledger_id whenever gateTools
// advertises the response tools.
// formatOfferPayment renders a barter offer's payment terms for a
// perception line: coins, goods, or both ("5 coins", "5 nails", "5 nails
// and 3 coins", "5 nails, 2 hammers and 3 coins"). Item kinds are
// sanitized inline (they reach the prompt). Returns "nothing" only for an
// all-empty payment, a state the intake gates reject. ZBBS-HOME-393.
func formatOfferPayment(amount int, payItems []sim.ItemKindQty) string {
	parts := make([]string, 0, len(payItems)+1)
	for _, pi := range payItems {
		name := sanitizeInline(string(pi.Kind))
		if name == "" {
			name = "item"
		}
		parts = append(parts, fmt.Sprintf("%d %s", pi.Qty, name))
	}
	if amount > 0 {
		unit := "coins"
		if amount == 1 {
			unit = "coin"
		}
		parts = append(parts, fmt.Sprintf("%d %s", amount, unit))
	}
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func renderPayOffers(b *strings.Builder, offers []sim.PayOfferWarrantReason, nameOf func(sim.ActorID) string, shortfalls map[sim.LedgerID]StockShortfall, roomAlreadySold map[sim.LedgerID]sim.OrderID, worth map[sim.LedgerID]offerWorth) {
	if len(offers) == 0 {
		return
	}
	b.WriteString("## Offers awaiting your decision\n")
	for i, o := range offers {
		disposition := "to keep"
		if o.ConsumeNow {
			disposition = "to consume now"
		}
		buyer := nameOf(o.Buyer)
		item := sanitizeInline(string(o.Item))
		if item == "" {
			item = "item"
		}
		// Payment may be coins, goods (barter), or both (ZBBS-HOME-393) —
		// render whatever the buyer offered so the seller judges the goods
		// the same way they judge coins.
		payment := formatOfferPayment(o.Amount, o.PayItems)
		fmt.Fprintf(b, "%d. %s offers %s for %d %s %s (offer id %d)",
			i+1, buyer, payment, o.Qty, item, disposition, o.LedgerID)
		// LLM-357: a partial-payment commission — surface that only the deposit
		// lands now and the rest comes at collection, so the seller weighs the
		// deal honestly rather than as a full-price sale.
		if o.Deposit > 0 && o.Deposit < o.Amount {
			fmt.Fprintf(b, " — %d down now as a deposit, the remaining %d when they collect", o.Deposit, o.Amount-o.Deposit)
		}
		// ZBBS-HOME-459: when the buyer asks for more than the seller actually
		// holds, surface the gap so they counter or decline against real stock
		// instead of accepting an offer the deliver gate would then bounce. Fact
		// only, and only when it bites (buildPayOfferShortfalls carries an entry
		// only then, services excluded). LLM-303: fire at zero held too — "you hold
		// no nails" for a non-vendor offeree, not just a vendor short of some stock.
		if sf, short := shortfalls[o.LedgerID]; short {
			if sf.Held == 0 {
				fmt.Fprintf(b, " — you hold no %s", sf.Noun)
			} else {
				fmt.Fprintf(b, " — you hold only %d %s", sf.Held, item)
			}
		}
		// LLM-89: this buyer already holds a room from you that you have not
		// handed over. Accepting a second mints a duplicate order (and the
		// AcceptPay gate now rejects it), so steer to deliver the one already
		// sold rather than sell another night.
		if oid, ok := roomAlreadySold[o.LedgerID]; ok {
			fmt.Fprintf(b, " — you already sold %s a room (order #%d) you have not handed over; deliver that with deliver_order before accepting another", buyer, oid)
		}
		// LLM-598: what the buyer puts up, weighed against what it asks for. Barter
		// only, and only when the payment falls short — buildPayOfferWorth carries an
		// entry for no other case. No numbers: the wares cue above already prints the
		// per-unit worths, and it was doing the arithmetic ACROSS those two sections
		// that the miller skipped, judging 7 flour for 7 wheat an even hand.
		b.WriteString(offerWorthPhrase(worth[o.LedgerID], item))
		b.WriteString("\n")
	}
	// ONE tool, and the words ride on it (LLM-350). This cue used to read "Respond
	// first with accept_pay… Then also use speak", which no NPC could obey: the pay
	// responses and speak are all terminal-on-success, so whichever landed first
	// ended the tick and the other was skipped as post_terminal. Obeying the order
	// as written settled the sale in silence; obeying it literally — speaking first —
	// cost the seller the sale, because the offer went unanswered and expired.
	// The response no longer passes in silence, so the cue no longer says it does.
	// Mirrors the seller cue's sell(say=…) shape (LLM-343).
	b.WriteString("Respond with accept_pay, decline_pay, or counter_pay, passing the offer id as ledger_id and the words you speak aloud in say. Do not reply with the speak tool: speaking ends your turn, and the offer would go unanswered.\n")
}

// renderLaborOffers renders the pending-work-offer decision section for whoever
// must ANSWER: one line per offer carrying the labor_id (the load-bearing field
// the model must echo into accept_work/decline_work), the other party, the
// reward, and how long the job takes. Uncapped by design — labor offers are
// inherently few (bounded by co-present actors), and the section must always
// carry the labor_id whenever gateTools advertises the response tools (the
// discussion-109 invariant). LLM-26.
//
// Two directions share the section (LLM-346), keyed off LaborOfferView.SubjectIsWorker().
// When the subject is the employer (the zero value of EmployerInitiated), a worker has offered to do a
// job and the affordability steer applies — the subject would be the one paying.
// When the subject is the worker, an employer has asked them to lend a hand: no
// affordability steer (they cannot see the keeper's purse), no returning-helper
// recall (that memory is the employer's), and the pay is something they would
// RECEIVE, not spend.
func renderLaborOffers(b *strings.Builder, offers []LaborOfferView, employerCoins int, employerProduces bool, nameOf func(sim.ActorID) string) {
	if len(offers) == 0 {
		return
	}
	b.WriteString("## Work offers awaiting your decision\n")
	anyAffordable, anyUnaffordable := false, false
	for i, o := range offers {
		// The pay may be coins, goods the employer holds, or both (LLM-225) —
		// formatOfferPayment renders whichever legs are present ("5 coins",
		// "1 porridge", "1 porridge and 2 coins").
		if o.SubjectIsWorker() {
			fmt.Fprintf(b, "%d. %s has asked you to do a job for them — %s for about %s of work (offer id %d)\n",
				i+1, nameOf(o.Employer), formatOfferPayment(o.Reward, o.RewardItems), humanizeWorkMinutes(o.DurationMin), o.LaborID)
			anyAffordable = true // no coin gate on the worker's side — the pay comes to them
			continue
		}
		worker := nameOf(o.Worker)
		fmt.Fprintf(b, "%d. %s offers to do a job for you for %s — about %s of work (offer id %d)\n",
			i+1, worker, formatOfferPayment(o.Reward, o.RewardItems), humanizeWorkMinutes(o.DurationMin), o.LaborID)
		// LLM-228: the returning-helper recall. When this worker completed a paid
		// job for the employer within the memory window, name the past help so the
		// re-hire choice is informed experientially — not by an engine hire-value
		// pitch at the decision point (a pitch removed in #691). Plain recall, no
		// directive: it states the fact and leaves accept/decline to the model.
		// Only a producing keeper actually "got more done" from the help; a
		// non-producer gets the bare social beat so the line never claims output
		// that never happened.
		if o.HelpedBeforeRecently {
			if employerProduces {
				fmt.Fprintf(b, "You remember %s lending you a hand recently, and you got more done for it.\n", worker)
			} else {
				fmt.Fprintf(b, "You remember %s lending you a hand recently.\n", worker)
			}
		}
		// A reward the employer can't cover is a doomed accept: accept_work's
		// gate 8 would only flip the offer to failed_unavailable
		// (employerCanCoverLaborReward, labor_commands.go), so the model
		// "accepts" verbally and the deal dies in silence. Steer the employer
		// to decline WITH a spoken reason instead — carried in decline_work's own
		// `say` (LLM-350), not a second speak call, which being terminal would
		// have skipped the decline or been skipped by it. The two checks here
		// mirror gate 8's two legs exactly — Coins < Reward, and the
		// build-time MissingRewardItems holdings scan — so the cue and the
		// substrate never disagree. LLM-158; goods leg LLM-225.
		shortOnCoins := employerCoins < o.Reward
		missing := o.MissingRewardItems
		if shortOnCoins || len(missing) > 0 {
			anyUnaffordable = true
			switch {
			case shortOnCoins && len(missing) > 0:
				fmt.Fprintf(b, "You only have %s and do not hold the %s they ask to be paid in, so you cannot pay for this — call decline_work (offer id %d), telling them in say that you cannot pay what they ask.\n",
					coinsPhrase(employerCoins), formatOfferPayment(0, missing), o.LaborID)
			case len(missing) > 0:
				fmt.Fprintf(b, "You do not hold the %s they ask to be paid in, so you cannot pay for this — call decline_work (offer id %d), telling them in say that you cannot pay what they ask.\n",
					formatOfferPayment(0, missing), o.LaborID)
			default:
				fmt.Fprintf(b, "You only have %s, so you cannot pay for this — call decline_work (offer id %d), telling them in say that you have not enough coin to take them on.\n",
					coinsPhrase(employerCoins), o.LaborID)
			}
			continue
		}
		anyAffordable = true
	}
	// One tool, and the words ride on it — the same shape the pay decision section
	// uses, for the same reason (LLM-350): accept_work, decline_work and speak are
	// all terminal, so a cue naming two of them can only ever have one obeyed. When
	// SOME offers are unaffordable, scope the footer to the affordable ones so a
	// weak model can't apply a generic "accept_work or decline_work" to an offer
	// that was just steered to decline. Suppressed entirely when EVERY offer is
	// unaffordable — each carried its own decline steer above. LLM-158.
	switch {
	case anyAffordable && anyUnaffordable:
		b.WriteString("For an offer you can afford, respond with accept_work or decline_work, passing the offer id as labor_id and the words you speak aloud in say; decline_work the ones you cannot pay. Do not reply with the speak tool: speaking ends your turn, and the offer would go unanswered.\n")
	case anyAffordable:
		b.WriteString("Respond with accept_work or decline_work, passing the offer id as labor_id and the words you speak aloud in say. Do not reply with the speak tool: speaking ends your turn, and the offer would go unanswered.\n")
	}
}

// renderLaborSelfState renders the worker's own in-progress job as a self-state
// line (LLM-26) — who they're working for and roughly how much longer, with the
// nudge to stay with it. Placed in the self-state block (top) because it is
// point-in-time "what I'm doing right now." Content-gated on Laboring != nil.
//
// Off-post surface (LLM-268): when the worker has wandered off the post (OffPost),
// or her employer has left it (EmployerAway), the line becomes a directional cue —
// head back, or follow along — paired with the move_to gateTools re-grants for the
// same LaboringView predicate, so cue and tool can't drift. The employer-away
// (accompany) case takes precedence: if the employer has left, there is no held
// post to return to, so "head back" would be wrong.
func renderLaborSelfState(b *strings.Builder, laboring *LaboringView, nameOf func(sim.ActorID) string, renderedAt time.Time) {
	if laboring == nil {
		return
	}
	employer := nameOf(laboring.Employer)
	post := "the workplace"
	if laboring.PostLabel != "" {
		post = sim.WithDefiniteArticle(sanitizeInline(laboring.PostLabel))
	}
	switch {
	case laboring.EmployerAway:
		if laboring.EmployerPlace != "" {
			fmt.Fprintf(b, "You are in the middle of a job for %s, but they have left %s and gone to %s. If they want you along, follow after them with move_to; otherwise carry on with the work. You are paid when the job is done.\n",
				employer, post, sim.WithDefiniteArticle(sanitizeInline(laboring.EmployerPlace)))
			return
		}
		fmt.Fprintf(b, "You are in the middle of a job for %s, but they have stepped away from %s. If they want you along, follow after them with move_to; otherwise carry on with the work. You are paid when the job is done.\n",
			employer, post)
		return
	case laboring.OffPost:
		fmt.Fprintf(b, "You took on a job for %s at %s, but you have wandered off from it — and you are still on the clock. Head back there with move_to and get on with the work; you are paid when it is done.\n",
			employer, post)
		return
	}
	mins := minutesUntil(laboring.Until, renderedAt)
	if mins <= 0 {
		fmt.Fprintf(b, "You are finishing a job for %s — the work is just about done; you'll be paid as you finish.\n", employer)
		return
	}
	fmt.Fprintf(b, "You are working a job for %s — about %s of work left. Stay with it until it's done; you are paid when you finish.\n",
		employer, humanizeWorkMinutes(mins))
}

// renderLaborEnRoute renders the relocation self-state for a worker who has
// accepted a job but not yet started it (LLM-229): they are on their way to the
// employer's workplace, or waiting there for the owner to show. It keeps the
// tickable relocating worker on task — go to the post and get to work — rather
// than wandering off or soliciting a second job. No reward is named; the work
// window hasn't started. Placed in the self-state block, mutually exclusive with
// renderLaborSelfState's in-progress line. Content-gated on LaborEnRoute != nil.
func renderLaborEnRoute(b *strings.Builder, enRoute *LaborEnRouteView, nameOf func(sim.ActorID) string) {
	if enRoute == nil {
		return
	}
	employer := nameOf(enRoute.Employer)
	if enRoute.Waiting {
		fmt.Fprintf(b, "You've taken on a job for %s and you're at their workplace waiting for them to arrive so you can start — stay put until they do; you are paid once the work is done.\n", employer)
		return
	}
	fmt.Fprintf(b, "You've taken on a job for %s — make your way to their workplace and get to work once you are there (if they aren't in yet, wait for them); you are paid once the work is done.\n", employer)
}

// renderWorkersForMe renders the employer-side active-labor cue (LLM-202): the
// workers currently on a job for the subject, with roughly how much longer and
// what they're owed on completion, plus a steer not to double up. The
// employer-side mirror of renderLaborSelfState's worker line — where the worker
// gets "you are working for X," the employer gets "X is working for you."
// Without it the employer sees only the pending-decision view (renderLaborOffers)
// and has no signal an accepted job is already underway, so they re-hire a second
// body or pay by hand for work already covered (the live John Ellis re-hire of
// Patience mid-way through Silence's contract). A standing situational line in the
// self-state block; one line per worker (an employer can have several), then one
// shared steer. Content-gated on a non-empty WorkersForMe.
func renderWorkersForMe(b *strings.Builder, workers []WorkerForMeView, nameOf func(sim.ActorID) string, renderedAt time.Time) {
	if len(workers) == 0 {
		return
	}
	for _, wkr := range workers {
		worker := nameOf(wkr.Worker)
		// The owed pay may be coins, goods, or both (LLM-225).
		payment := formatOfferPayment(wkr.Reward, wkr.RewardItems)
		mins := minutesUntil(wkr.Until, renderedAt)
		if mins <= 0 {
			fmt.Fprintf(b, "%s is finishing a job for you — almost done; %s owed as they finish.\n",
				worker, payment)
			continue
		}
		fmt.Fprintf(b, "%s is working a job for you — about %s left; %s owed when it's done.\n",
			worker, humanizeWorkMinutes(mins), payment)
	}
	// Trailing blank line so the following section keeps its separator, matching
	// the self-state-gap convention (renderNarrativeState / renderVendorOperating).
	b.WriteString("That work is already covered and the pay settles on its own when it's finished — don't hire someone else for it or pay again by hand.\n\n")
}

// renderPendingLaborOfferOut renders the subject's OWN outgoing labor offer that
// is still awaiting the other party's answer (LLM-164) — the awaiting-acceptance
// mirror of renderLaborSelfState's in-progress line. Whoever minted an offer has
// no Working job yet, so this is the only labor self-state they get while waiting;
// it names what's on the table and says plainly to sit tight, the anchor that
// keeps the weak model from flailing into an unrelated tool under the quiet
// backstop / "choose one action" pressure. Content-gated on PendingLaborOfferOut.
//
// Both mints get a line (LLM-346): the worker who solicited waits on the employer,
// the employer who offered work waits on the worker. Without the second one a
// keeper who has just asked someone to lend a hand has no anchor at all, and the
// quiet backstop pushes her to ask again.
func renderPendingLaborOfferOut(b *strings.Builder, offer *PendingLaborOfferOutView, nameOf func(sim.ActorID) string) {
	if offer == nil {
		return
	}
	// The pay may be coins, goods, or both (LLM-225).
	payment := formatOfferPayment(offer.Reward, offer.RewardItems)
	duration := humanizeWorkMinutes(offer.DurationMin)
	if offer.SubjectIsEmployer() {
		fmt.Fprintf(b, "You've asked %s to work for you for %s (about %s) — your offer stands and it is their move now. There's nothing more to do on it; wait for their answer, say a brief word if you like, then call done().\n",
			nameOf(offer.Worker), payment, duration)
		return
	}
	fmt.Fprintf(b, "You've offered to work for %s for %s (about %s) — your offer stands and it is their move now. There's nothing more to do on it; wait for their answer, say a brief word if you like, then call done().\n",
		nameOf(offer.Employer), payment, duration)
}

// renderLaborAffordance renders the free-worker option cue (LLM-26): the
// subject takes work for pay and has someone here to offer it to. Content-gated
// on CanSolicitWork, the same signal that gates the solicit_work tool.
//
// LLM-564 rewrote it from the old standing conditional ("You take work for pay.
// If someone here... has a task") to a grounded ask that NAMES the acquainted
// employers in the room — the register renderOfferWorkAffordance already uses,
// arriving on the worker side for the same measured reason: the unnamed cue
// converted 26 of 1,427 renders in a week, and the one hire it produced came
// from the employer's tool answering a bare speak-ask. Both wordings warn off
// the terminal speak (the ask rides solicit_work's own `say` now), because a
// worker who voices the offer first never reaches the tool.
//
// employers may be empty under a true canSolicit — every solicitable peer a
// stranger the cue must not name (solicit_work resolves by exact display name;
// see buildSolicitableEmployers) — and falls back to the unnamed wording.
func renderLaborAffordance(b *strings.Builder, canSolicit bool, employers []sim.ActorID, nameOf func(sim.ActorID) string) {
	if !canSolicit {
		return
	}
	const askShape = "name them, the pay you want (coins, goods they hold such as a meal, or both), and roughly how long you'd work. Speak your ask in solicit_work's `say`, in your own voice; do NOT ask with speak first — speaking ends your turn and no offer is ever made.\n"
	// Only REAL names reach the named branch. buildSolicitableEmployers already
	// restricts the slice to acquaintances, but the render must not trust that
	// alone: a resolver returning "" or its "someone" fallback here would put a
	// target string in the cue that solicit_work must refuse (code_review). Any
	// such entry is dropped, and a slice that empties falls through to the
	// unnamed wording.
	names := make([]string, 0, len(employers))
	for _, id := range employers {
		if n := nameOf(id); n != "" && n != "someone" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		b.WriteString("You take work for pay. If someone here outside your own household or trade could use a hand and you want the pay, make the offer with solicit_work — " + askShape)
		return
	}
	fmt.Fprintf(b, "%s might have work that wants doing, and you take work for pay. If you want the coin, make the offer with solicit_work — %s",
		joinNames(names), askShape)
}

// renderOfferWorkAffordance renders the hiring-side option cue (LLM-346): people
// are here who take work for pay, and the subject may ask one of them to lend a
// hand. Content-gated on a non-empty HireableWorkers — the same slice that gates
// the offer_work tool, so the cue and the tool surface together or not at all
// (discussion-109).
//
// It NAMES them because nothing else in the prompt does. Whether a villager takes
// odd jobs is not visible from the co-presence line, and offer_work resolves its
// target by exact display name — a keeper left to guess spends her turn being told
// the person she asked is not a worker.
//
// It also warns off the terminal speak, for the reason LLM-343 folded `say` into
// sell: offer_work and speak both end the tick, so a keeper who voices the request
// first never reaches the tool, and the offer she just made aloud does not exist.
func renderOfferWorkAffordance(b *strings.Builder, workers []sim.ActorID, nameOf func(sim.ActorID) string) {
	if len(workers) == 0 {
		return
	}
	names := make([]string, 0, len(workers))
	for _, id := range workers {
		names = append(names, nameOf(id))
	}
	takes := "takes"
	if len(names) > 1 {
		takes = "take"
	}
	fmt.Fprintf(b, "%s %s work for pay and could lend you a hand. If you have a task worth paying for, ask with offer_work — name them, the pay you will hand over when the work is done (coins, goods you hold such as a meal, or both), and roughly how long the job will take. Put what you say to them in offer_work's `say`, in your own voice; do NOT ask with speak first, because speaking ends your turn and the offer would never reach them.\n",
		joinNames(names), takes)
}

// renderSeekWorkPlaces lists the town's businesses as move_to destinations for a
// broke worker nudged to go earn (LLM-152) — the directional companion to the
// seek-work impulse line (the "go seek work" warrant renders separately in the
// what-just-happened block). Content-gated on a non-empty list, which Build
// populates only for a broke idle worker with no employer present. Each business is
// a bullet carrying its qualitative distance + direction (LLM-155), matching the
// eat/drink cue's "a fair walk south" phrasing so the worker favours a near, open
// shop. Names only: each is a structure navigable by move_to-by-name (LLM-142).
// A business the worker called at recently (Visited, LLM-563) — ranked after the
// untried ones by Build — carries a terse "you called there not long ago" aside,
// so the ordering reads as a reason rather than an arbitrary shuffle and the
// model favours a door not yet knocked.
func renderSeekWorkPlaces(b *strings.Builder, places []SeekWorkPlace) {
	if len(places) == 0 {
		return
	}
	b.WriteString("If you mean to take paid work, use move_to to head to one of the town's businesses and offer your labor once you arrive:\n")
	for _, p := range places {
		b.WriteString("- ")
		b.WriteString(sanitizeInline(p.Name))
		if p.Distance != "" {
			fmt.Fprintf(b, " — %s", p.Distance)
			if p.Direction != "" {
				fmt.Fprintf(b, " %s", p.Direction)
			}
		}
		if p.Visited {
			b.WriteString(" — you called there not long ago")
		}
		b.WriteString("\n")
	}
}

// humanizeWorkMinutes renders a work duration in minutes as legible prose for a
// weak model ("45 minutes", "2 hours", "1 hour 30 minutes") — concrete time,
// not a terse count (the salem-prose convention). LLM-26.
func humanizeWorkMinutes(min int) string {
	if min < 60 {
		return fmt.Sprintf("%d minutes", min)
	}
	h := min / 60
	m := min % 60
	hUnit := "hours"
	if h == 1 {
		hUnit = "hour"
	}
	if m == 0 {
		return fmt.Sprintf("%d %s", h, hUnit)
	}
	return fmt.Sprintf("%d %s %d minutes", h, hUnit, m)
}

// minutesUntil returns whole minutes from now to t, floored at 1 for any
// positive sub-minute remainder (so "about 1 minute" rather than "0"), and 0
// when t is at or before now. A zero renderedAt (hand-built payload with no
// clock) yields a far-future duration; callers content-gate on Laboring, so a
// missing clock just renders a long "left" value rather than crashing. LLM-26.
func minutesUntil(t, now time.Time) int {
	d := t.Sub(now)
	if d <= 0 {
		return 0
	}
	m := int(d / time.Minute)
	if m == 0 {
		m = 1
	}
	return m
}

// renderPendingOffersFromMe renders the buyer-side "## Offers you have
// standing" section — the subject's OWN pay-with-item offers still awaiting the
// seller's answer (ZBBS-HOME-413; copy re-registered to light period voice in
// ZBBS-HOME-421 — NPCs mirror the register of what they read, and the old
// contract language came back out of their mouths verbatim). Semantics and
// functional tokens (offer ids, tool names, counts) are load-bearing; rewordings
// must keep them intact. It is the mirror of renderPayOffers (the seller's
// "offers awaiting your decision"): the seller sees offers staked AGAINST them;
// the buyer sees offers they HAVE staked. Its job is suppression — a hungry
// buyer who already has an open offer should wait, not re-stake the same offer
// next tick (the cross-tick repeat-offer storm). One line per offer; the
// closing line is an explicit "don't re-offer" instruction.
//
// Uncapped by design, like renderPayOffers: pending outgoing offers are bounded
// by the buyer's own tool calls (few), and the whole point is that every open
// offer is visible so none gets re-staked. SellerName is already acquaintance-
// gated at build time; item kinds are sanitized inline here (they reach the
// prompt). Payment terms reuse formatOfferPayment so the buyer reads the same
// "5 nails and 3 coins" shape the seller sees.
func renderPendingOffersFromMe(b *strings.Builder, offers []PendingOfferView) {
	if len(offers) == 0 {
		return
	}
	b.WriteString("## Offers you have standing\n")
	for i, o := range offers {
		seller := sanitizeInline(o.SellerName)
		if seller == "" {
			seller = "someone"
		}
		item := sanitizeInline(string(o.Item))
		if item == "" {
			item = "item"
		}
		payment := formatOfferPayment(o.Amount, o.PayItems)
		fmt.Fprintf(b, "%d. You have asked %s for %d %s, %s offered — they have yet to give their answer (offer id %d).\n",
			i+1, seller, o.Qty, item, payment, o.LedgerID)
	}
	b.WriteString("Bide for their answer; make no second offer for the same goods while this one stands. Should you think better of it, withdraw_pay recalls it.\n")
}

// renderStandingQuotesFromMe renders the seller-side "## Offers you've put out"
// section — the subject's OWN active scene-quotes still awaiting a buyer's answer
// (LLM-45). It is the seller/scene_quote mirror of renderPendingOffersFromMe (the
// buyer/pay_with_item "## Offers you have standing"): there the buyer sees offers
// it staked; here the seller sees the wares it has offered. The job is the same —
// give cross-tick memory so the seller neither re-posts a standing quote (the
// already_quoted thrash) nor invents a queue between two co-present askers because
// it can't recall whom it already served (the John Ellis two-room scene). One line
// per quote, targeted or public; the closing line is an explicit "await, don't
// re-offer".
//
// Distinct header from the buyer-side "## Offers you have standing": a keeper can
// hold both at once — its own quotes here AND pending pay offers it must answer
// under "## Offers awaiting your decision". Uncapped, like its buyer twin:
// standing quotes are bounded by the seller's own tool calls, and every open offer
// must stay visible so none gets re-posted. BuyerName is acquaintance-gated at
// build time; item kinds sanitized inline. Price reuses formatOfferPayment (coins
// only — a scene-quote names a coin price; any barter leg rides the buyer's
// pay_with_item) for the shape the other offer sections use.
func renderStandingQuotesFromMe(b *strings.Builder, quotes []StandingQuoteView) {
	if len(quotes) == 0 {
		return
	}
	b.WriteString("## Offers you've put out\n")
	for i, q := range quotes {
		items := formatQuoteLines(q.Lines)
		if items == "" {
			items = "item"
		}
		price := formatOfferPayment(q.Amount, nil)
		if q.BuyerName != "" {
			fmt.Fprintf(b, "%d. You have offered %s %s for %s — they have yet to answer.\n",
				i+1, sanitizeInline(q.BuyerName), items, price)
			continue
		}
		fmt.Fprintf(b, "%d. You have offered %s for %s to anyone here — none has yet taken it.\n",
			i+1, items, price)
	}
	// Steer against re-posting a STANDING offer (the already_quoted thrash), not
	// against making a fresh offer to a different buyer — a keeper with rooms or
	// stock to spare can legitimately offer the same kind to a second seeker
	// (the two-room scene this fixes). So the close names the listed offers, not
	// "the same goods" (which the buyer-side close uses, where double-buying a
	// single need IS wrong).
	b.WriteString("Bide for an answer; an offer listed above already stands — do not post it again.\n")
}

// renderStandingQuotesToMe renders the buyer-side "## Offers made to you"
// section — scene-quotes another actor has posted in the subject's name that are
// still hers to take (LLM-551). The actionable twin of
// renderStandingQuotesFromMe: that section exists so a seller doesn't re-post,
// this one so a buyer can still ACT on an offer whose warrant has long since
// been consumed.
//
// Every line carries the quote_id take-instruction (quoteTakeInstruction, shared
// with the warrant line). That is the point of the section — a buyer who spoke
// on the tick the quote arrived kept the memory of the deal in the conversation
// but lost the id, and a bare pay_with_item crosses the quote rather than taking
// it (ZBBS-HOME-424).
//
// redundancy (LLM-171) is honoured per quote exactly as on the warrant line: a
// quote whose every line is a good the buyer makes herself or already holds at
// cap loses its take and gets a decline steer instead, so a mis-pitched standing
// offer can't drive a buy-back of her own ware or an over-cap purchase.
//
// Uncapped, like both offer twins: every open offer must stay visible, and the
// count is bounded by how many sellers have addressed this buyer.
func renderStandingQuotesToMe(
	b *strings.Builder,
	quotes []StandingQuoteToMeView,
	buyRedundancy func(sim.ItemKind) (produced, atCap bool),
) {
	if len(quotes) == 0 {
		return
	}
	b.WriteString("## Offers made to you\n")
	for i, q := range quotes {
		items := formatQuoteLines(q.Lines)
		if items == "" {
			items = "item"
		}
		unit := "coins"
		if q.Amount == 1 {
			unit = "coin"
		}
		disposition := ""
		if q.EatHere {
			disposition = ", to eat here (it can't be carried away)"
		}
		take := quoteTakeInstruction(q.QuoteID, q.Lines, q.Amount)
		switch buyQuoteRedundancyReason(q.Lines, buyRedundancy) {
		case "produced":
			take = " But these are wares you make yourself — there's no reason to buy them. Decline and tend to your own work."
		case "atcap":
			take = " But you already hold all of these you can carry — there's no reason to buy more. Decline and move on."
		}
		fmt.Fprintf(b, "%d. %s has %s set out for you at %d %s%s — it still stands.%s\n",
			i+1, sanitizeInline(q.SellerName), items, q.Amount, unit, disposition, take)
	}
}

// renderUncoverableOffersFromMe renders the flat "## An offer you couldn't keep"
// beat — a sell lot the subject posted and then spent the goods out from under,
// which the coverage reconcile just flipped to shortfall (built by
// buildRecentlyShortfallQuotesFromMe, LLM-409). One diegetic line per fallen-
// through lot: the seller announced the offer aloud, so this closes the thread
// he'd otherwise lose when the buyer comes to take a good he no longer holds. A
// shortfall is a discrete event, not a gradient, so the register is a single
// plain beat rather than a tiered felt escalation. Item kinds are sanitized
// inline. Uncapped — bounded by the seller's own recent lots and the short
// resolution window.
func renderUncoverableOffersFromMe(b *strings.Builder, offers []UncoverableOfferView) {
	if len(offers) == 0 {
		return
	}
	b.WriteString("## An offer you couldn't keep\n")
	for i, o := range offers {
		items := formatQuoteLines(o.Lines)
		if items == "" {
			items = "what you offered"
		}
		if o.BuyerName != "" {
			fmt.Fprintf(b, "%d. You offered %s %s and no longer have it to give.\n",
				i+1, sanitizeInline(o.BuyerName), items)
			continue
		}
		fmt.Fprintf(b, "%d. You offered %s to anyone here and no longer have it to give.\n",
			i+1, items)
	}
	b.WriteString("You can no longer make good on it — own it to them plainly, and offer only what you still hold.\n")
}

// renderRecentlyResolvedOffersFromMe renders the buyer-side "## Recently settled
// offers" section — the subject's OWN offers that JUST resolved (built by
// buildRecentlyResolvedOffersFromMe). It is the reliable, snapshot-scanned
// counterpart to the PayResolvedWarrantReason event line, which can arrive a
// tick late (the warrant opens a fresh cycle when the seller accepts mid-tick),
// leaving the buyer to re-perceive "the seller has it for sale" and re-buy a
// need already met. An accepted line says the deal is done (and, for an eat-here
// deal, that the goods were used on the spot) and tells the buyer not to offer
// for it again; a close-without-a-deal line tells the buyer to stop waiting.
// Copy is plain modern English on purpose — the weak stateful models parse it
// more reliably than period voice. Item kinds are sanitized inline. Uncapped —
// bounded by the buyer's own recent offers and the short resolution window.
func renderRecentlyResolvedOffersFromMe(b *strings.Builder, offers []ResolvedOfferView) {
	if len(offers) == 0 {
		return
	}
	b.WriteString("## Recently settled offers\n")
	for i, o := range offers {
		seller := sanitizeInline(o.SellerName)
		if seller == "" {
			seller = "someone"
		}
		item := sanitizeInline(string(o.Item))
		if item == "" {
			item = "item"
		}
		if o.Accepted {
			payment := formatOfferPayment(o.Amount, o.PayItems)
			if o.DeliveryPending {
				// LLM-512: the accept minted an order the seller hasn't delivered
				// yet — the goods are still in the seller's hands, not the buyer's
				// pack. "it's in your pack now. That deal is done" is false here and
				// contradicts the "## Orders you're waiting on" line in the same
				// prompt. State delivery-pending; keep the don't-re-offer guard
				// (the buyer has already ordered it, just not received it).
				fmt.Fprintf(b, "%d. %s accepted your offer — you paid %s for %d %s; it's not in your pack yet, %s will hand it over when it's ready. You've already ordered it, so don't offer for it again (offer id %d).\n",
					i+1, seller, payment, o.Qty, item, seller, o.LedgerID)
				continue
			}
			gotIt := "it's in your pack now"
			if o.ConsumeNow {
				gotIt = "you had it right away"
				// LLM-188: when the needs-clamp ate fewer than purchased
				// (consumableUnits, ZBBS-WORK-391), say what was eaten vs kept
				// so this line reconciles with the carried-inventory count
				// rather than asserting all Qty were consumed on the spot — the
				// contradiction that made buyers confabulate a short-count. The
				// 0 < KeptUnits < Qty guard holds for the self-consume case (the
				// clamp floors at 1, so kept <= Qty-1); a rare group-order split
				// that breaks the invariant falls back to the plain line.
				if o.KeptUnits > 0 && o.KeptUnits < o.Qty {
					gotIt = fmt.Sprintf("you ate %d on the spot and kept the other %d", o.Qty-o.KeptUnits, o.KeptUnits)
				}
			}
			fmt.Fprintf(b, "%d. %s accepted your offer — you paid %s for %d %s; %s. That deal is done — don't offer for it again (offer id %d).\n",
				i+1, seller, payment, o.Qty, item, gotIt, o.LedgerID)
			continue
		}
		// LLM-296: name what was OFFERED (not just the want-item) so two declines
		// aren't byte-identical — the thin line gave the standing "never repeat
		// what you said" instruction nothing to bind to, and the model re-posted
		// the same bundle. Where the engine knows the seller is short the bought
		// kind, append it as the informed "why" the deal closed (the buyer's
		// mirror of the seller-side "you hold only N"); only when it bites.
		offered := formatOfferPayment(o.Amount, o.PayItems)
		reason := "it's closed, so stop waiting on it"
		if o.SellerStocks && o.Qty > o.SellerStock {
			// LLM-303: at zero held, name it "they hold no nails" (plural noun)
			// rather than the awkward "only 0 nail"; above zero keeps the LLM-296
			// "they hold only N <kind>" form on the raw kind key.
			if o.SellerStock == 0 {
				reason = fmt.Sprintf("they hold no %s, so it's closed; stop waiting on it", o.SellerStockNoun)
			} else {
				reason = fmt.Sprintf("they hold only %d %s, so it's closed; stop waiting on it", o.SellerStock, item)
			}
		}
		fmt.Fprintf(b, "%d. Your offer of %s to %s for %d %s didn't go through — %s (offer id %d).\n",
			i+1, offered, seller, o.Qty, item, reason, o.LedgerID)
	}
}

// renderCountersAwaitingMyResponse renders the buyer-side "## A counter to your
// offer" section — a seller's counter to an offer the buyer placed that the
// buyer has not yet answered, surfaced from the standing ledger scan
// (buildCountersAwaitingMyResponse) rather than the timing-fragile
// PayResolvedWarrantReason{Countered} event so it cannot ride a tick late or
// vanish if the warrant is evicted while the buyer is shelved (LLM-21). It is the
// buyer-side mirror of renderPayOffers (the seller's "offers awaiting your
// decision"): the buyer learns the seller wants different terms and how to act on
// them. Copy is plain modern English, like its settled-offers sibling, for the
// weak stateful models — it tells the buyer to answer with a fresh pay_with_item
// carrying in_response_to, or let the counter go. Payment terms reuse
// formatOfferPayment so the buyer reads the same "5 nails and 3 coins" shape the
// seller proposed. Item kinds sanitized inline. Uncapped — bounded by the buyer's
// own recent counters and the short response window.
func renderCountersAwaitingMyResponse(b *strings.Builder, counters []CounterOfferView) {
	if len(counters) == 0 {
		return
	}
	b.WriteString("## A counter to your offer\n")
	for i, c := range counters {
		seller := sanitizeInline(c.SellerName)
		if seller == "" {
			seller = "someone"
		}
		item := sanitizeInline(string(c.Item))
		if item == "" {
			item = "item"
		}
		terms := formatOfferPayment(c.CounterAmount, c.CounterPayItems)
		fmt.Fprintf(b, "%d. %s countered your offer for %d %s — they now want %s (offer id %d).\n",
			i+1, seller, c.Qty, item, terms, c.LedgerID)
	}
	b.WriteString("To take a counter, make a fresh offer at their terms with pay_with_item, setting in_response_to to the offer id above. If the new terms don't suit you, you may simply let it go.\n")
}

// isSectionSurfacedKind reports whether a warrant kind wakes the actor for a
// tick but must NOT emit a generic "## Since your last turn" line — rendering one
// produced the vague "something happened nearby" catch-all (ZBBS-WORK-407).
// These warrants are still consumed to wake the actor (that is how it ticks to
// read the rest of the prompt); they just have no standalone event line. Most
// carry their content in a dedicated section; the bare operator nudge has no
// in-world content at all:
//   - pay_offer   -> "## Offers awaiting your decision" (PayOffersForMe)
//   - labor_offer -> "## Work offers awaiting your decision" (LaborOffersForMe,
//     LLM-187). The employer is woken to accept_work / decline_work; its
//     content is the labor decision section, so it must not also fabricate a
//     bare "something happened" line.
//   - shift_duty -> the return-to-post steer (DutySteer)
//   - admin      -> a bare operator force-tick (umbilical /nudge with no
//     message). Not an in-world event, so it falls to the routine
//     check-in line rather than a fabricated "something happened"
//     (ZBBS-WORK-418). A nudge WITH a message keeps its felt-
//     impulse line (WarrantKindImpulse) — that is real content.
func isSectionSurfacedKind(k sim.WarrantKind) bool {
	switch k {
	case sim.WarrantKindPayOffer, sim.WarrantKindLaborOffer, sim.WarrantKindShiftDuty, sim.WarrantKindAdmin:
		return true
	default:
		return false
	}
}

func renderWarrants(b *strings.Builder, warrants []sim.WarrantMeta, nameOf func(sim.ActorID) string, placeNameOf func(string) string, placeKeeperOf func(string) string, eatHereKind func(sim.ItemKind) bool, buyRedundancy func(sim.ItemKind) (produced, atCap bool), renderedAt time.Time, cfg RenderConfig, out *RenderedPrompt) {
	// Nil-safe for direct/test callers — the main Render path always passes
	// its closure, but the signature grew by a callback (ZBBS-WORK-405) and
	// a nil here must degrade to "no eat-here tag", not panic (code_review).
	if eatHereKind == nil {
		eatHereKind = func(sim.ItemKind) bool { return false }
	}
	// Same nil-safety for the LLM-171 buyer-redundancy callback: a nil here must
	// degrade to "never redundant" (every quote keeps its actionable take).
	if buyRedundancy == nil {
		buyRedundancy = func(sim.ItemKind) (bool, bool) { return false, false }
	}
	// Same nil-safety for the LLM-284 keeper-possessive callback: a nil here must
	// degrade to "no keeper", so an arrival line keeps its plain, articled form.
	if placeKeeperOf == nil {
		placeKeeperOf = func(string) string { return "" }
	}
	// ZBBS-WORK-407: drop warrants already surfaced by a dedicated section so they
	// don't double-render as the vague "something happened nearby" catch-all. They
	// still WAKE the actor (the reactor consumed them — that's how it ticks to read
	// the section); they just have no standalone "since your last turn" line. Filter
	// a local copy so the caller's p.Warrants (scene grouping, telemetry) is
	// untouched and the surviving lines keep contiguous numbering.
	renderable := warrants[:0:0]
	for _, wm := range warrants {
		if isSectionSurfacedKind(wm.Kind()) {
			continue
		}
		renderable = append(renderable, wm)
	}
	warrants = renderable
	// Neutral event log, not an imperative: a self-caused beat (you arrived where
	// you walked to) is nothing to "address", and the act-now coda already carries
	// the "respond to this" weight, so "— address these" over-claimed (ZBBS-WORK-419).
	// "Since your last turn", not "What just happened" (LLM-316): a carried-forward,
	// shelve-delayed, or slept-through warrant can be minutes-to-hours old by the
	// time it renders, and the batch semantics ARE "what accumulated since you last
	// acted" — the header shouldn't promise a recency the queue can't guarantee.
	// The per-line AgoPhrase stamp below carries the actual staleness.
	b.WriteString("## Since your last turn\n")
	if len(warrants) == 0 {
		b.WriteString("(nothing specific — this is a routine check-in)\n")
		return
	}

	// Render each candidate warrant into its own line first, so the
	// MaxSectionBytes accounting can measure real rendered size before
	// committing it.
	var section strings.Builder
	sectionBytes := 0
	cutoff := len(warrants)
	for i, w := range warrants {
		if i >= cfg.MaxWarrants {
			cutoff = i
			break
		}
		line, truncated := renderWarrantLine(i+1, w, nameOf, placeNameOf, placeKeeperOf, eatHereKind, buyRedundancy, cfg.MaxBytesPerWarrant)
		// Interval-stamp each signal against the render clock (LLM-316), the
		// LLM-217 treatment the conversation ring and self-action trail already
		// get: a carried-forward or shelve-delayed warrant renders honestly as
		// "(4m ago)" instead of masquerading as fresh. AgoPhrase returns "" for
		// zero clocks (hand-built payloads / unstamped metas) — no stamp then.
		if stamp := AgoPhrase(w.OccurredAt, renderedAt); stamp != "" {
			line = strings.TrimSuffix(line, "\n") + " (" + stamp + ")\n"
		}
		if sectionBytes+len(line) > cfg.MaxSectionBytes && i > 0 {
			// At least one warrant already rendered; this one would
			// overflow the section cap — stop here and carry the rest.
			cutoff = i
			break
		}
		section.WriteString(line)
		sectionBytes += len(line)
		out.RenderedWarrantCount++
		if truncated {
			out.TruncatedWarrants++
		}
	}

	b.WriteString(section.String())

	if cutoff < len(warrants) {
		dropped := warrants[cutoff:]
		out.DroppedWarrants = make([]sim.WarrantMeta, len(dropped))
		copy(out.DroppedWarrants, dropped)
		fmt.Fprintf(b, "(%d more signal(s) not shown here — they are carried forward to your next turn)\n",
			len(out.DroppedWarrants))
	}
}

// renderWarrantLine renders one warrant as a single numbered prose line. Every
// actor reference is resolved to a name via nameOf (never a raw UUID), and the
// old "[kind] (scene <uuid>)" machine prefix is gone — each kind reads as a
// sentence. The untrusted free-text payload (a speech excerpt) is sanitized and
// capped; the returned bool reports whether that text was truncated.
// ZBBS-HOME-339.
func renderWarrantLine(n int, w sim.WarrantMeta, nameOf func(sim.ActorID) string, placeNameOf func(string) string, placeKeeperOf func(string) string, eatHereKind func(sim.ItemKind) bool, buyRedundancy func(sim.ItemKind) (produced, atCap bool), maxTextBytes int) (string, bool) {
	switch r := w.Reason.(type) {
	case sim.PCSpeechWarrantReason:
		return renderSpeechWarrantLine(n, nameOf(r.Speaker), r.Excerpt, maxTextBytes)
	case sim.NPCSpeechWarrantReason:
		return renderSpeechWarrantLine(n, nameOf(r.Speaker), r.Excerpt, maxTextBytes)
	case sim.PaidWarrantReason:
		return renderPaidWarrantLine(n, nameOf(r.Buyer), r.Amount, r.ForText, maxTextBytes)
	case sim.IdleBackstopWarrantReason:
		return renderIdleBackstopWarrantLine(n, r.QuietDuration), false
	case sim.StrandedWarrantReason:
		return renderStrandedWarrantLine(n), false
	case sim.RestockWarrantReason:
		return renderRestockWarrantLine(n, r.Item, r.Source), false
	case sim.DwellEndedWarrantReason:
		return renderNarrationWarrantLine(n, w.Kind(), r.NarrationText, nameOf(w.TriggerActorID), maxTextBytes)
	case sim.DwellTickAppliedWarrantReason:
		// ZBBS-WORK-407: the per-tick beat used to be suppressed (fell through to
		// the vague "something happened" fallback) because it fired every minute.
		// The wake is now cadenced to the red-tier boundary (handlers/dwell_reactor.go),
		// so this fires at most once per dwell — render its felt line like its
		// DwellEnded sibling.
		return renderNarrationWarrantLine(n, w.Kind(), r.NarrationText, nameOf(w.TriggerActorID), maxTextBytes)
	case sim.SourceActivityCompletedWarrantReason:
		// LLM-69: the NPC completion beat for a finished eat/drink/harvest, pre-
		// rendered at the subscriber — same narration-line path as the dwell beats.
		return renderNarrationWarrantLine(n, w.Kind(), r.NarrationText, nameOf(w.TriggerActorID), maxTextBytes)
	case sim.AdminDirectiveWarrantReason:
		return renderImpulseWarrantLine(n, r.Message, maxTextBytes)
	case sim.SeekWorkWarrantReason:
		// LLM-141/168: a workless worker (no post of its own) is woken to go find
		// odd jobs. Engine-authored felt impulse, generic (no named hirer) — the
		// worker decides freely where to go and whom to ask. Wake-from-anywhere
		// nudge in the style of the stall-repair / production-choice lines; the
		// standing labor affordance ("you take work for pay … solicit_work") renders
		// separately once it is co-present with someone. Framed on having no work of
		// its own, not an empty purse — the nudge fires for a workless worker whether
		// or not it holds coin (LLM-168).
		return fmt.Sprintf("%d. You have no work of your own to tend, and you take work for pay — seek out someone who could use a hand and offer your labor.\n", n), false
	case sim.AtEaseWarrantReason:
		// LLM-352: a comfortable (coin-rich) workless worker with nothing pressing.
		// No go-earn nudge (it doesn't need the work) — a diegetic "the day is your
		// own" scene that leaves the choice open: pass time with neighbors, look in at
		// the tavern, or see to a want of its own. No named target; the model draws its
		// own conclusion, the same no-coercion register as the seek-work line.
		return fmt.Sprintf("%d. Your purse is easy enough for now and no one is looking to hire — the day is your own. You might pass a while with the neighbors, look in at the tavern, or see to some want of your own.\n", n), false
	case sim.ReturnToPostWarrantReason:
		// LLM-268: the felt pull that wakes a laboring worker who has wandered off
		// the post. Generic — the actionable specifics (which post, whose job, and
		// the move_to to get there) render from her LaboringView self-state
		// (renderLaborSelfState), the same predicate that re-grants her move_to, so
		// this line and the tool can't drift. Mirrors the SeekWork felt-impulse line.
		return fmt.Sprintf("%d. It weighs on you that you have drifted away from the paid job you are in the middle of — you should get back to it.\n", n), false
	case sim.VisitorRoundsWarrantReason:
		// LLM-379: the pacing beat for a traveler on his rounds — a felt pause to
		// consider his next move. No named target (the engine does not choose his
		// stop); the "## Your rounds" section lists the shops still open and their
		// bearings, and he decides with move_to. Same no-coercion register as the
		// seek-work / at-ease impulse lines.
		return fmt.Sprintf("%d. You pause a moment on your rounds — thinking where to carry your pack and your news next, or whether the light is going and it's time to see about a bed.\n", n), false
	case sim.UnfinishedIntentWarrantReason:
		// LLM-414: the actor's own last turn queued an action after a terminal
		// one and the harness dropped it (a spoken agreement is terminal, so
		// "I'll send for him" + summon lost the summon). Felt continuation
		// beat, with the dropped tool named exactly — the model re-decides
		// with fresh perception rather than replaying stale args.
		if r.Tools != "" {
			return fmt.Sprintf("%d. Your words ended your turn before you could act on what you meant to do (%s) — if you still mean to, do it now.\n", n, sanitizeInline(r.Tools)), false
		}
		return fmt.Sprintf("%d. Your words ended your turn before you could act on what you meant to do — if you still mean to, do it now.\n", n), false
	case sim.ArrivalWarrantReason:
		return renderArrivalWarrantLine(n, nameOf(w.TriggerActorID), r, placeNameOf, placeKeeperOf), false
	case sim.NeedThresholdWarrantReason:
		return renderNeedNudgeLine(n, r.Need), false
	case sim.TendNeedWarrantReason:
		// LLM-276: the gentle "you've grown peckish and have the means to see to it"
		// pull for a workless idle worker, stamped by the seek-work backstop in place
		// of the go-earn impulse. Generic felt line — the actionable targets (what to
		// eat/drink and where) render from the "## What you can eat or drink" section
		// and the need-redirect steer, both keyed off this same warrant.
		return renderTendNeedLine(n, r.Need), false
	case sim.SceneQuoteTargetedWarrantReason:
		// A bundle is eat-here if ANY line is non-portable (the whole bundle
		// was clamped to eat-here at quote creation, LLM-101).
		eatHere := false
		for _, ln := range r.Lines {
			if eatHereKind(ln.ItemKind) {
				eatHere = true
				break
			}
		}
		// LLM-171: when EVERY line in the quote is a good this buyer makes itself
		// or already holds at cap, the take is degenerate — strip the actionable
		// take and steer them off it. A mixed bundle (≥1 genuinely wanted good)
		// renders with its normal take.
		redundancy := buyQuoteRedundancyReason(r.Lines, buyRedundancy)
		return renderQuoteWarrantLine(n, nameOf(r.SellerID), r, eatHere, redundancy), false
	case sim.PayResolvedWarrantReason:
		return renderPayResolvedWarrantLine(n, nameOf(r.Seller), r, maxTextBytes), false
	case sim.ServeHandoverWarrantReason:
		return renderServeHandoverWarrantLine(n, nameOf(r.Buyer), r), false
	case sim.ProductionChoiceWarrantReason:
		// LLM-116/LLM-319: nothing is in the works at the actor's post — the
		// "## Your trade" cue carries the scene + the produce tool; this line is
		// just the "why you ticked" beat, like the idle-backstop / need-nudge
		// lines. Deliberately not an instruction to produce: whether to make
		// more is the decision the tick exists to grant.
		return fmt.Sprintf("%d. Your thoughts turn to your trade — nothing is in the works right now.\n", n), false
	case sim.ProductionDoneWarrantReason:
		// LLM-319: a production cycle landed its batch — pre-rendered at the
		// subscriber, same narration-line path as the source-activity beat.
		return renderNarrationWarrantLine(n, w.Kind(), r.NarrationText, nameOf(w.TriggerActorID), maxTextBytes)
	case sim.LaborSettledWarrantReason:
		// LLM-498: a finished job's wage transferred at the completion sweep —
		// pre-rendered per side at the subscriber (worker: wage received;
		// employer: wage paid), so neither party's next turn treats the settled
		// job as still owed. Same narration-line path as the production beat.
		return renderNarrationWarrantLine(n, w.Kind(), r.NarrationText, nameOf(r.Counterparty), maxTextBytes)
	case sim.StallRepairWarrantReason:
		// LLM-118 (generalized LLM-247): the business just wore through the repair
		// threshold. At the business the "## Your business" cue carries the nail
		// count + buy-from-the-smith steer; this is the wake-from-anywhere nudge to
		// go tend it. The place name resolves the worn business (structure/object);
		// falls back to a generic noun if unnamed.
		place := placeNameOf(string(r.StallID))
		if place == "" {
			place = "place of business"
		}
		return fmt.Sprintf("%d. Your %s has worn from use and needs mending — go to it and repair it (you'll need nails; the smith sells them).\n", n, place), false
	case sim.StallRepairHiredWarrantReason:
		// LLM-271: hired-worker twin. Same wake-to-mend nudge, framed as the
		// employer's premises the worker was taken on to help with — the "## The
		// business you're working at" cue carries the nail count + repair steer.
		place := placeNameOf(string(r.StallID))
		if place == "" {
			return fmt.Sprintf("%d. The business you're working at has worn from use and needs mending — you can repair it (you'll need nails; the smith sells them).\n", n), false
		}
		return fmt.Sprintf("%d. The %s you're working at has worn from use and needs mending — you can repair it (you'll need nails; the smith sells them).\n", n, place), false
	case sim.FarmUpkeepWarrantReason:
		// LLM-215: the season wore out the farm's upkeep shovels. The "## Farm upkeep"
		// cue carries the shovel count + buy-from-the-blacksmith steer; this is the
		// wake-from-anywhere "why you ticked" nudge, like production-choice / seek-work.
		return fmt.Sprintf("%d. Your farm tools are worn from the season's work — buy fresh shovels from the blacksmith.\n", n), false
	case sim.HearthLowWarrantReason:
		// LLM-412: a storm is on and the owner's hearth fire is out or low. At the
		// hearth the "## Your hearth" cue carries the firewood count + buy steer;
		// this is the wake-from-anywhere nudge to go see to the fire. Diegetic, not
		// imperative past the pointing — the scene is the argument.
		place := placeNameOf(string(r.HearthID))
		if place == "" {
			place = "place"
		}
		return fmt.Sprintf("%d. The storm outside puts you in mind of the fire at your %s — it is burning low, if it burns at all.\n", n, place), false
	case sim.HearthStokeHiredWarrantReason:
		// LLM-412: hired-worker twin — the employer's fire, framed truthfully as
		// the place the worker was taken on to help at.
		place := placeNameOf(string(r.HearthID))
		if place == "" {
			return fmt.Sprintf("%d. The storm outside puts you in mind of the fire where you're working — it is burning low, if it burns at all.\n", n), false
		}
		return fmt.Sprintf("%d. The storm outside puts you in mind of the fire at the %s you're working at — it is burning low, if it burns at all.\n", n, place), false
	case sim.HuddlePartReason:
		// LLM-438: the actor's own join/leave, with the huddle peers named
		// through the acquaintance-gated warrant actor-name map.
		return renderHuddlePartWarrantLine(n, r, nameOf), false
	default:
		return renderBasicWarrantLine(n, w.Kind(), nameOf(w.TriggerActorID)), false
	}
}

// renderArrivalWarrantLine renders an arrival as "<who> arrived at <place>."
// naming the destination the mover walked to (ZBBS-WORK-358) — decision-useful
// ("you reached the General Store, do what you came for") rather than the old
// vacuous "arrived nearby". Falls back to "<who> arrived." when the destination
// was a bare position with no nameable place. who is the pre-resolved subject
// ("you" for self), capitalized to match the huddle self-lines.
//
// When the arrived-at structure has a keeper (someone other than the mover works
// there), the place reads as that keeper's possessive — "<keeper>'s <place>",
// article-free — so the model sees whose shop it walked into and hosts as a
// guest instead of greeting the keeper as if it owned the place (LLM-284). The
// keeper name is a proper noun, so it takes no definite article; sim.Possessive
// forms the case (a name ending in "s" gets a bare apostrophe), and only the
// plain, keeperless form runs through WithDefiniteArticle.
func renderArrivalWarrantLine(n int, who string, r sim.ArrivalWarrantReason, placeNameOf func(string) string, placeKeeperOf func(string) string) string {
	subject := who
	if subject == "you" {
		subject = "You"
	}
	// A valid MoveDestination names exactly one kind, so at most one of these
	// is set; if a malformed reason ever set both, structure wins by design.
	place := placeNameOf(string(r.AtStructureID))
	if place == "" {
		place = placeNameOf(string(r.AtObjectID))
	}
	if place == "" {
		return fmt.Sprintf("%d. %s arrived.\n", n, subject)
	}
	// A keeper only ever resolves for the structure destination (objects have
	// none), so key the possessive off AtStructureID regardless of which id named
	// the place above.
	if keeper := placeKeeperOf(string(r.AtStructureID)); keeper != "" {
		return fmt.Sprintf("%d. %s arrived at %s %s.\n", n, subject, sim.Possessive(keeper), place)
	}
	return fmt.Sprintf("%d. %s arrived at %s.\n", n, subject, sim.WithDefiniteArticle(place))
}

// renderBasicWarrantLine renders the kinds carried by BasicWarrantReason (the
// huddle lifecycle events) plus any future kind without a dedicated case. The
// huddle events get felt prose; "joined"/"left" are stamped on the actor
// themselves (so the subject is "you"), the "peer_" variants on the others (so
// the trigger is the peer who came or went). An unrecognized kind falls back to
// a quiet, name-resolved line rather than a raw "[kind] involving <uuid>".
func renderBasicWarrantLine(n int, kind sim.WarrantKind, who string) string {
	switch kind {
	case sim.WarrantKindHuddleJoined:
		return fmt.Sprintf("%d. You joined a conversation.\n", n)
	case sim.WarrantKindHuddleLeft:
		return fmt.Sprintf("%d. You left the conversation.\n", n)
	case sim.WarrantKindHuddlePeerJoined:
		return fmt.Sprintf("%d. %s joined your conversation.\n", n, who)
	case sim.WarrantKindHuddlePeerLeft:
		return fmt.Sprintf("%d. %s stepped away from your conversation.\n", n, who)
	case sim.WarrantKindHuddlePeerRetired:
		// LLM-447: a peer who went to BED, not one who merely stepped away. The
		// finality is the point — it tells the household the evening is over and
		// they may follow, where "stepped away" reads as "will be back".
		return fmt.Sprintf("%d. %s has turned in for the night.\n", n, who)
	case sim.WarrantKindHuddleConcluded:
		return fmt.Sprintf("%d. Your conversation has broken up.\n", n)
	default:
		// A kind with no felt template lands here — an unhandled warrant kind, or
		// a narration warrant (dwell/consumed) whose text came back empty and fell
		// through renderNarrationWarrantLine. The line is useless to the model
		// either way; we tag it with the originating warrant kind (ZBBS-WORK-417)
		// so an operator who spots a vague "something happened" can trace its
		// source from the prompt alone — the kind is otherwise consumed by this
		// switch and never shown, and the engine's per-tick ring resets on restart.
		if who != "" && who != "someone" && who != "you" {
			return fmt.Sprintf("%d. Something happened involving %s. [debug: unrendered warrant kind=%q]\n", n, who, kind)
		}
		return fmt.Sprintf("%d. Something happened nearby. [debug: unrendered warrant kind=%q]\n", n, kind)
	}
}

// renderHuddlePartWarrantLine renders the actor's own huddle join/leave with
// the peers named (LLM-438) — "You left the conversation with Mercy and a
// stranger." — episodic continuity against the instant-re-greet failure mode,
// where the bare "You left the conversation." carried nothing to hold the
// departure in mind. Peer labels come pre-gated from the warrant actor-name
// map (display name only when acquainted), so no name can leak here. An empty
// or fully-unresolvable peer list (lone-member dissolve, peers gone from the
// snapshot) falls back to the bare BasicWarrantReason sentence, as does a
// future kind this reason doesn't know a peer-bearing sentence for.
func renderHuddlePartWarrantLine(n int, r sim.HuddlePartReason, nameOf func(sim.ActorID) string) string {
	peers := huddlePeerPhrase(r.PeerIDs, nameOf)
	if peers == "" {
		return renderBasicWarrantLine(n, r.K, "")
	}
	switch r.K {
	case sim.WarrantKindHuddleJoined:
		return fmt.Sprintf("%d. You joined a conversation with %s.\n", n, peers)
	case sim.WarrantKindHuddleLeft:
		return fmt.Sprintf("%d. You left the conversation with %s.\n", n, peers)
	default:
		return renderBasicWarrantLine(n, r.K, "")
	}
}

// huddlePeerPhrase composes the peer clause for a huddle join/leave line from
// the acquaintance-gated labels: named/role peers lead in warrant order, any
// unacquainted peers fold into ONE trailing stranger phrase (so two of them
// read "two strangers", never "a stranger and a stranger"), and a long list
// caps at two labels plus "and others". A peer that no longer resolves at all
// (gone from the snapshot — nameOf's "someone" fallback) is dropped rather
// than guessed at; returns "" when nothing survives, which the caller turns
// into the bare no-peers sentence.
//
// The "someone" / "a stranger" cases match the literal sentinel labels minted
// by the Render nameOf closure and descriptorLabel (build.go) — the same
// contract every "!= \"someone\"" check in this file leans on. Rewording
// either label there without updating here degrades gracefully (the label
// renders in the list instead of merging/dropping), and the phrase-edge unit
// test pins the pairing.
func huddlePeerPhrase(ids []sim.ActorID, nameOf func(sim.ActorID) string) string {
	var labels []string
	strangers := 0
	// Stamp sites collect peers from huddle member SETS, so duplicates can't
	// occur today — the seen-guard is insurance so a future caller passing a
	// rebuilt list can't double-name a peer or inflate the stranger count.
	seen := make(map[sim.ActorID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		switch label := nameOf(id); label {
		case "someone":
			// Unresolvable — deleted between event and publish. Skip.
		case "a stranger":
			strangers++
		default:
			labels = append(labels, label)
		}
	}
	switch strangers {
	case 0:
	case 1:
		labels = append(labels, "a stranger")
	case 2:
		labels = append(labels, "two strangers")
	default:
		labels = append(labels, "some strangers")
	}
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	case 2:
		return labels[0] + " and " + labels[1]
	default:
		return labels[0] + ", " + labels[1] + ", and others"
	}
}

// renderNeedNudgeLine renders a need-threshold warrant as a felt pang. The
// "## You" needs line carries the real urgency (Address now: …); this is the
// in-the-moment beat that the need just crossed into distress. Falls back to a
// generic pang for an unrecognized need key.
func renderNeedNudgeLine(n int, need sim.NeedKey) string {
	switch need {
	case "hunger":
		return fmt.Sprintf("%d. Your hunger is pressing on you.\n", n)
	case "thirst":
		return fmt.Sprintf("%d. Your thirst is pressing on you.\n", n)
	case "tiredness":
		return fmt.Sprintf("%d. Weariness is settling over you.\n", n)
	default:
		return fmt.Sprintf("%d. A need is pressing on you.\n", n)
	}
}

// renderTendNeedLine renders a tend-need warrant (LLM-276) as a gentle felt pull to
// eat or drink before the need grows sharp — the seek-work backstop's redirect for a
// workless idle worker who has grown hungry/thirsty and can resolve it now. Softer
// than renderNeedNudgeLine (which is the red-tier "pressing on you" distress beat):
// this fires below the red-line, so it reads as foresight, not urgency. The
// actionable specifics (what to eat/drink, where) render from the satiation section
// + the need-redirect steer. Falls back to a generic pull for an unrecognized need.
func renderTendNeedLine(n int, need sim.NeedKey) string {
	switch need {
	case "hunger":
		return fmt.Sprintf("%d. Hunger is beginning to tug at you, and you have the means to see to it — better to get something to eat now than to let it grow sharp.\n", n)
	case "thirst":
		return fmt.Sprintf("%d. A thirst is creeping up on you, and you have the means to see to it — better to get a drink now than to let it grow sharp.\n", n)
	default:
		return fmt.Sprintf("%d. A need is beginning to press, and you have the means to see to it now.\n", n)
	}
}

// buyQuoteRedundancyReason classifies whether a buy-quote is pointless for the
// buyer: every line is a good they MAKE themselves ("produced") or already hold
// at cap ("atcap"), so taking it just buys back their own ware or overflows
// their carry (LLM-171). Returns "" when at least one line is a good worth
// buying, so the quote renders with its normal actionable take. "produced" wins
// the label when every line is produced; a mix of produced + at-cap lines is
// "atcap" so the steer leads with the carry reason. Nil/empty inputs → "".
func buyQuoteRedundancyReason(lines []sim.QuoteLine, redundant func(sim.ItemKind) (produced, atCap bool)) string {
	if len(lines) == 0 || redundant == nil {
		return ""
	}
	allProduced := true
	for _, ln := range lines {
		produced, atCap := redundant(ln.ItemKind)
		if !produced && !atCap {
			return "" // a genuinely wanted good — render the normal take
		}
		if !produced {
			allProduced = false
		}
	}
	if allProduced {
		return "produced"
	}
	return "atcap"
}

// renderQuoteWarrantLine renders a vendor's scene quote aimed directly at this
// actor — a standing offer they can take by paying. Names the seller; the
// terms come straight off the warrant payload. The take-instruction carries
// the quote_id: without it the buyer model answered a standing quote with a
// bare pay_with_item, minting a crossing offer that deadlocked against the
// quote (ZBBS-HOME-424) — the fast path existed but was never legible.
//
// redundancy (LLM-171), when non-empty, replaces the actionable take with a
// steer: "produced" — the buyer makes these wares itself; "atcap" — it already
// holds all of these it can carry. Either way there is no reason to buy, so the
// quote_id take is withheld and the line tells the buyer to decline.
func renderQuoteWarrantLine(n int, seller string, r sim.SceneQuoteTargetedWarrantReason, eatHere bool, redundancy string) string {
	unit := "coins"
	if r.Amount == 1 {
		unit = "coin"
	}
	items := formatQuoteLines(r.Lines)
	// The eat-here disposition fact (ZBBS-WORK-405): goods of this class
	// can't be carried away, so say so up front rather than letting the
	// buyer plan a take-home the clamp will quietly rewrite.
	disposition := ""
	if eatHere {
		disposition = ", to eat here (it can't be carried away)"
	}
	// The take-instruction carries the quote_id. A bundle (LLM-101) is taken
	// whole, so it needs only the quote_id + total amount; a single-item quote
	// names the concrete item/qty/amount (LLM-172 — the prior "the same item,
	// qty, and amount" phrasing had no anchor, so a buyer carrying other goods
	// bound "item" to one of those: pay_with_item then rejected the term
	// mismatch and the model fell back to a bare pay, leaking coins for an
	// undelivered good with the quote still open).
	//
	// LLM-136: a single-item quote is the COIN settlement path — goods can't ride
	// a quote_id (that rejects). A coin-short buyer isn't stuck, though: barter is
	// a first-class path via a SEPARATE offer_trade that names the item it wants.
	// Saying so on the take-line keeps a coinless buyer (e.g. a homeless smith
	// eyeing a room) from looping on a price it can't meet. The want_item is the
	// concrete kind, not "this", so a weak model sends the real machine value.
	// Bundles stay coin-only here: offer_trade takes one item kind, and a bundle
	// has no single want_item to name.
	//
	// LLM-457: every take-line carries the anti-speak-first guard, mirroring the
	// co-present buy cue (restock.go) and the pay_with_item tool description. A
	// posted coin quote is taken with pay_with_item, but a buyer who instead voices
	// acceptance through the terminal speak tool ends the turn without paying and
	// livelocks re-announcing the same deal (Nathaniel Cole ↔ John Ellis, porridge
	// quote open ~13m, 2026-07-17). Folding the utterance into pay's say is the one
	// move that both speaks and settles.
	take := quoteTakeInstruction(r.QuoteID, r.Lines, r.Amount)
	// LLM-171: the buyer makes or is at cap on every quoted good — withhold the
	// take entirely and steer them to decline, so a mis-pitched quote can't drive
	// a buy-back of their own ware or an over-cap purchase.
	switch redundancy {
	case "produced":
		take = " But these are wares you make yourself — there's no reason to buy them. Decline and tend to your own work."
	case "atcap":
		take = " But you already hold all of these you can carry — there's no reason to buy more. Decline and move on."
	}
	// An overheard public quote (huddle fan-out, ZBBS-HOME-431) is an ad
	// announced to the conversation, not a direct address — "offers" not
	// "offers you", so the actor doesn't perceive a personal offer.
	offers := "offers you"
	if r.Overheard {
		offers = "offers"
	}
	return fmt.Sprintf("%d. %s %s %s for %d %s%s.%s\n", n, seller, offers, items, r.Amount, unit, disposition, take)
}

// quoteTakeInstruction builds the "here is how you take it" sentence appended to
// a quote the buyer can act on — the quote_id-bearing pay_with_item call, the
// anti-speak-first guard, and (single-item only) the barter fallback. Leading
// space included; the caller concatenates it onto its own line.
//
// Extracted from renderQuoteWarrantLine (LLM-551) so the one-shot warrant line
// and the standing "## Offers made to you" section emit the SAME instruction.
// They must not drift: the quote_id is the whole reason either line exists
// (ZBBS-HOME-424 — without it the buyer answers a standing quote with a bare
// pay_with_item and mints a crossing offer that deadlocks against the quote),
// and a second copy of this text is a second place for it to go missing.
func quoteTakeInstruction(quoteID sim.QuoteID, lines []sim.QuoteLine, amount int) string {
	switch {
	case len(lines) > 1:
		// LLM-561: the bundle arm needs its own way out. Before this it ended at
		// "settles at once" — no counter, no partial take, no alternative named —
		// and a buyer who wanted four of five goods was at a dead end (the live
		// cost: pay_with_item with item "bundle", then a prose haggle settled as
		// 35 coins on a 3-coin salt). The escape decomposes: a free-form
		// pay_with_item (no quote_id) carries one item kind, the seller answers
		// via accept_pay/counter_pay/decline_pay, so any subset at any price is a
		// series of single-good offers. Deliberately NOT offer_trade — a bundle
		// has no single want_item to name (LLM-136), and coin offers are the
		// path the seller-side machinery already settles.
		return fmt.Sprintf(" To take the whole bundle, call pay_with_item with quote_id %d and amount %d — it settles at once. Say your piece in the same breath — pass it in the call's say; don't speak first, because speaking ends your turn and the quote goes unpaid. If you want only part of the bundle, or at another price, don't use that quote_id — make your own offer instead: call pay_with_item with no quote_id, naming the one good you want, its qty, and your coins, one good at a time; they can accept or counter.", quoteID, amount)
	case len(lines) == 1:
		return fmt.Sprintf(" To take this coin quote, call pay_with_item with quote_id %d, item %q, qty %d, and amount %d — it settles at once. Say your piece in the same breath — pass it in the call's say; don't speak first, because speaking ends your turn and the quote goes unpaid. Don't put goods on a quote_id; if you lack coins but have goods to offer, propose a separate trade instead — call offer_trade with the goods you'll give and want_item %q; they can accept or counter.", quoteID, string(lines[0].ItemKind), lines[0].Qty, amount, string(lines[0].ItemKind))
	default:
		// Defensive (code_review): a quote with zero lines shouldn't reach here —
		// sell/scene_quote require ≥1 item — but the single-item arm indexes
		// lines[0], so guard the empty case instead of risking a panic on
		// malformed/legacy warrant data. Bare coin take, no item to name.
		return fmt.Sprintf(" To take it, call pay_with_item with quote_id %d and the stated amount — it settles at once. Say your piece in the same breath — pass it in the call's say; don't speak first, because speaking ends your turn and the quote goes unpaid.", quoteID)
	}
}

// formatQuoteLines renders a quote's item lines as a readable phrase:
// "2 blueberries", or for a bundle "2 blueberries and 2 raspberries", or
// "2 blueberries, 2 raspberries, and 3 bread" (LLM-101). Qty 1 drops the
// leading count, matching the prior single-item rendering. Items are
// sanitized inline (catalog keys, but defensive against odd labels).
func formatQuoteLines(lines []sim.QuoteLine) string {
	parts := make([]string, 0, len(lines))
	for _, ln := range lines {
		item := sanitizeInline(string(ln.ItemKind))
		if ln.Qty > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", ln.Qty, item))
		} else {
			parts = append(parts, item)
		}
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// renderPayResolvedWarrantLine renders, to the buyer, how the seller resolved
// their pay-with-item offer. Only the buyer-meaningful terminal states get a
// bespoke line; the rest collapse to a neutral "fell through" so the buyer
// stops waiting without the engine narrating internal ledger states.
func renderPayResolvedWarrantLine(n int, seller string, r sim.PayResolvedWarrantReason, maxTextBytes int) string {
	item := sanitizeInline(string(r.ItemKind))
	qty := item
	if r.Qty > 1 {
		qty = fmt.Sprintf("%d %s", r.Qty, item)
	}
	switch r.TerminalState {
	case sim.PayTerminalStateAccepted:
		return fmt.Sprintf("%d. %s accepted your offer of %d for %s.\n", n, seller, r.Amount, qty)
	case sim.PayTerminalStateDeclined:
		return fmt.Sprintf("%d. %s declined your offer for %s.\n", n, seller, qty)
	case sim.PayTerminalStateCountered:
		// Counter terms may be coins, goods (barter), or both (ZBBS-HOME-393).
		return fmt.Sprintf("%d. %s countered: %s for %s.\n", n, seller, formatOfferPayment(r.CounterAmount, r.CounterPayItems), qty)
	default:
		return fmt.Sprintf("%d. Your offer to %s for %s fell through.\n", n, seller, qty)
	}
}

// renderServeHandoverWarrantLine renders, to the SELLER, the moment a buyer
// instantly took their posted quote (ZBBS-WORK-423). The settle already
// happened — coins and goods have changed hands — so this isn't a decision
// cue; it states the sale and steers the handover BEAT. The instant quote-take
// is the one serving path that never ticks the seller, so unlike deliver_order
// (whose tool description steers "pair with a brief speak") there's nothing
// else asking the keeper to acknowledge the customer. "Hand it over with a
// word" is that steer, kept to a greeting beat — not a re-pitch (a greeting is
// not a sale). The model voices the line in character; the engine doesn't
// supply the words. buyer is pre-resolved by the caller.
func renderServeHandoverWarrantLine(n int, buyer string, r sim.ServeHandoverWarrantReason) string {
	if buyer == "" {
		buyer = "someone"
	}
	unit := "coins"
	if r.Amount == 1 {
		unit = "coin"
	}
	item := sanitizeInline(string(r.ItemKind))
	qty := item
	if r.Qty > 1 {
		qty = fmt.Sprintf("%d %s", r.Qty, item)
	}
	// ConsumeNow is the buyer's disposition term (ZBBS-WORK-402): when they're
	// eating on the spot, say so, so the keeper's line fits a sit-down serve
	// rather than a counter handoff.
	if r.ConsumeNow {
		return fmt.Sprintf("%d. %s paid you %d %s for %s, to eat here now. Hand it over with a word.\n", n, buyer, r.Amount, unit, qty)
	}
	return fmt.Sprintf("%d. %s paid you %d %s for %s. Hand it over with a word.\n", n, buyer, r.Amount, unit, qty)
}

// renderNarrationWarrantLine renders a felt-language self-perception beat
// (ZBBS-HOME-302): the consume self-line and the dwell started/ended lines all
// carry a pre-rendered second-person NarrationText. Surfaces it as the warrant
// line, sanitized + capped like the speech excerpt to bound prompt cost.
//
// DwellTickApplied is deliberately NOT routed here — the per-tick "another
// bite" beat would be prompt spam, and the sustained state is already conveyed
// by the ActiveDwellCredits projection; the per-tick warrant keeps its bare
// fallback line.
//
// Empty narration (e.g. a catalog-unknown dwell end) falls back to the generic
// kind line so the warrant still registers rather than vanishing.
func renderNarrationWarrantLine(n int, kind sim.WarrantKind, narration, who string, maxTextBytes int) (string, bool) {
	if narration == "" {
		return renderBasicWarrantLine(n, kind, who), false
	}
	sanitized, truncated := sanitizeText(narration, maxTextBytes)
	return fmt.Sprintf("%d. %s\n", n, sanitized), truncated
}

// renderSpeechWarrantLine renders the warrant line for both PC- and NPC-speech
// warrant reasons (structurally identical — SpeechID / Speaker / Excerpt). The
// speaker is already name-resolved by the caller. An empty excerpt renders "X
// spoke to you" rather than a dangling `said: ""`.
func renderSpeechWarrantLine(n int, speaker, excerpt string, maxTextBytes int) (string, bool) {
	if speaker == "" {
		speaker = "someone"
	}
	sanitized, truncated := sanitizeText(excerpt, maxTextBytes)
	if strings.TrimSpace(sanitized) == "" {
		return fmt.Sprintf("%d. %s spoke to you.\n", n, speaker), truncated
	}
	return fmt.Sprintf("%d. %s said: \"%s\"\n", n, speaker, sanitized), truncated
}

// renderPaidWarrantLine renders the warrant line for a PaidWarrantReason.
// Surfaces the (name-resolved) buyer, amount, and optional flavor text to the
// seller's perception prompt — the seller's next reactor tick reads this and
// decides what to do (speak thanks, walk over, ignore).
//
// Without ForText: `N. <buyer> paid you N coins.`
// With ForText:    `N. <buyer> paid you N coins — "<for>"`.
//
// The ForText excerpt is sanitized + capped like the speech excerpt to keep
// the per-tick prompt cost bounded. Returned bool reports truncation.
func renderPaidWarrantLine(n int, buyer string, amount int, forText string, maxTextBytes int) (string, bool) {
	if buyer == "" {
		buyer = "someone"
	}
	unit := "coins"
	if amount == 1 {
		unit = "coin"
	}
	if strings.TrimSpace(forText) == "" {
		return fmt.Sprintf("%d. %s paid you %d %s.\n", n, buyer, amount, unit), false
	}
	sanitized, truncated := sanitizeText(forText, maxTextBytes)
	return fmt.Sprintf("%d. %s paid you %d %s — \"%s\"\n", n, buyer, amount, unit, sanitized), truncated
}

// renderIdleBackstopWarrantLine renders the warrant line for an
// IdleBackstopWarrantReason — the engine-injected liveness tick for an
// actor that no other warrant has engaged.
//
// Surfaces the quiet duration so the actor's LLM tick can decide what
// (if anything) to do: pursue a need, walk somewhere, sit and wait.
// The replacement for v1's chronicler-attend-to dispatch; v1 had the
// chronicler decide who to engage, v2 lets the actor's own tick decide
// what to do given that they've been quiet.
//
// Form: `N. You've been quiet for <duration> — consider what to do next.`
// The duration is rounded to whole seconds (sub-second resolution is noise at
// the minute-scale this warrant fires at).
//
// Returned without truncation since there's no untrusted free-text
// payload — the line is composed of fixed prose and an engine-computed
// duration.
func renderIdleBackstopWarrantLine(n int, quiet time.Duration) string {
	if quiet <= 0 {
		return fmt.Sprintf("%d. You've been quiet — consider what to do next.\n", n)
	}
	return fmt.Sprintf("%d. You've been quiet for %s — consider what to do next.\n",
		n, quiet.Round(time.Second))
}

// renderStrandedWarrantLine renders the anomalous-position backstop line
// (ZBBS-HOME-450): the actor is standing in the open at no anchor with
// nothing under way. The wording is a neutral observation of the actor's
// situation — it names where they are, not what to do, so the model
// re-decides freely (the same no-coercion discipline as the felt-impulse
// and atmosphere lines). Fixed prose, no untrusted payload.
func renderStrandedWarrantLine(n int) string {
	return fmt.Sprintf("%d. You find yourself standing out in the open, between places, with nothing under way.\n", n)
}

// renderRestockWarrantLine renders the warrant line for a RestockWarrantReason —
// the reorder producer's nudge to an actor whose sell-stock has dropped below the
// reorder threshold. It names the representative low item; the actionable detail
// (current/cap, suppliers or bushes, structure_ids) is in the section the line
// points to, so the line stays a short pointer. The Source routes the pointer:
// a `forage` low (LLM-90) points at "## Your bushes to harvest", everything else
// at "## Restocking" — so the cue line never sends a grower to a buy-side section
// she has no entries in.
//
// Form: `N. Your stock of <item> is running low — see <section>.`
// Form (no item): `N. Your shop stock is running low — see <section>.`
//
// Rendered without truncation: the item is an engine-controlled catalog key,
// not model- or user-supplied text.
func renderRestockWarrantLine(n int, item sim.ItemKind, source sim.RestockSource) string {
	section := "Restocking"
	if source == sim.RestockSourceForage {
		section = "Your bushes to harvest"
	}
	if item == "" {
		return fmt.Sprintf("%d. Your shop stock is running low — see %s.\n", n, section)
	}
	return fmt.Sprintf("%d. Your stock of %s is running low — see %s.\n", n, item, section)
}

// renderImpulseWarrantLine renders the warrant line for an
// AdminDirectiveWarrantReason — an operator-authored directive injected via the
// umbilical /nudge route (ZBBS-WORK-329). The operator's message is wrapped in
// an in-world felt-impulse frame so the NPC reads it as a spontaneous internal
// urge, NOT an out-of-world instruction — the same in-world-voice discipline the
// atmosphere + noticeboard prompts keep. The colon form keeps the line
// grammatical regardless of how the operator phrases the directive (it does not
// assume the message completes a "pull to …" clause).
//
// Form: `N. You feel a strong, insistent pull: <message>`
//
// The message is untrusted operator free text, so it is sanitized + capped like
// the speech excerpt; the returned bool reports truncation. An empty message
// does not reach here in practice — the handler stamps this reason only for a
// non-empty directive (an empty nudge stamps the bare admin reason) — but it is
// handled defensively so a stray empty directive renders a bare impulse rather
// than a dangling colon.
func renderImpulseWarrantLine(n int, message string, maxTextBytes int) (string, bool) {
	sanitized, truncated := sanitizeText(message, maxTextBytes)
	if sanitized == "" {
		return fmt.Sprintf("%d. You feel a strong, insistent pull to act.\n", n), false
	}
	return fmt.Sprintf("%d. You feel a strong, insistent pull: %s\n", n, sanitized), truncated
}

// sanitizeText neutralizes untrusted free text for inclusion in the prompt
// and caps its length. Control characters — crucially newlines — are
// collapsed to spaces so the text cannot inject a fake prompt section or
// otherwise break the prompt's structure. This is structural escaping, not
// semantic injection defense: it cannot stop a payload that reads like an
// instruction, only one that forges prompt *layout*. The returned bool
// reports whether the text was truncated by maxBytes.
func sanitizeText(s string, maxBytes int) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		// Replace C0 controls (incl. \n \r \t) and DEL with a space — those
		// are what could forge prompt layout. U+FFFD is left intact: ranging
		// over invalid UTF-8 already yields it (so the rebuilt string is
		// valid UTF-8 regardless), and a legitimate U+FFFD in trusted input
		// is indistinguishable from a decode-error one — stripping it would
		// be data loss with no structural benefit.
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
		} else {
			prevSpace = false
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	return capBytes(cleaned, maxBytes)
}

// sanitizeInline is sanitizeText with no length cap — used for short
// trusted-ish fields (structure names, origin kinds) that still must not
// carry newlines into the prompt.
func sanitizeInline(s string) string {
	out, _ := sanitizeText(s, 0)
	return out
}

// elisionMarker terminates any free-text payload capBytes had to shorten. It is
// the sole signal to a reading NPC that it is NOT seeing the whole line — without
// it a clipped utterance reads as an unfinished sentence, and the listener answers
// by asking the speaker to finish, forever (LLM-396). Named rather than inlined so
// the render path and the invariant that enforces it share one definition.
//
// The write paths cap by rune against a storage bound, this one caps by byte
// against the prompt budget, and both must mark a cut identically or "am I seeing
// the whole line?" stops having one answer — so the marker itself is declared once,
// in sim, and aliased here (LLM-405).
const elisionMarker = sim.ElisionMarker

// capBytes truncates s to at most maxBytes bytes on a rune boundary,
// appending elisionMarker when it truncates. maxBytes <= 0 means no
// cap. The returned bool reports whether truncation happened.
//
// The byte cap is hard: when maxBytes is smaller than the marker itself,
// capBytes returns an empty string rather than emit a marker that would
// exceed the cap (and rather than a raw byte slice that could split a
// rune). Such a tiny cap is a misconfiguration — RenderConfig's defaults
// are far larger — but capBytes still honors the contract.
func capBytes(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, false
	}
	const marker = elisionMarker
	if maxBytes < len(marker) {
		return "", true
	}
	budget := maxBytes - len(marker)
	// Largest rune-start index <= budget; s[:n] is then whole runes only.
	n := 0
	for i := range s {
		if i > budget {
			break
		}
		n = i
	}
	return s[:n] + marker, true
}
