package sim

import (
	"regexp"
	"strings"
	"time"
)

// ActorID identifies an actor uniquely within the world.
type ActorID string

// persistedActorIDPattern is the shape of an id in the uuid-keyed `actor`
// table: canonical 8-4-4-4-12 hex, case-insensitive. Both populations that
// reach that table carry it — NPCs and PCs alike are minted as uuids (a PC's
// login username is a separate column, not its id).
var persistedActorIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsPersistedActorID reports whether id is BINDABLE against a uuid column that
// FKs to the `actor` table, such as agent_action_log.actor_id. It is a check on
// shape alone — it says nothing about whether such an actor exists, and a
// well-formed id naming nobody is a legitimate empty match.
//
// It exists for the read side of that column. Binding a non-uuid string against
// it is not an empty match but a query ERROR ("invalid input syntax for type
// uuid"), so a caller filtering by a transient visitor's "vstr-" id — a real
// actor id in this world, just never one this column holds — got a 500 where it
// wanted "none". The rough complement of IsVisitorActorID, but stated positively
// and about the column rather than the population, because that is the question
// a query filter is actually asking.
//
// Case-insensitive, and indifferent to uuid version/variant bits: Postgres
// accepts both as uuid input, so constraining further would reject values the
// column would have taken.
func IsPersistedActorID(id ActorID) bool {
	return persistedActorIDPattern.MatchString(string(id))
}

// ActorKind discriminates the four populations: stateful NPCs (own VA with
// memory), shared-VA NPCs (salem-vendor / salem-visitor backed), human PCs,
// and decorative sprite-only actors that the engine moves but never ticks.
type ActorKind int

const (
	KindNPCStateful ActorKind = iota
	KindNPCShared
	KindPC
	KindDecorative
)

// Shared-VA agent slugs. An actor whose llm_memory_agent points at one of
// these is KindNPCShared: the VA is a stateless switchboard with no private
// memory, not the actor's own persistent VA. VisitorAgentName lives with the
// visitor lifecycle code (visitor.go). salem-generic backs the
// atmosphere/noticeboard cascades rather than any actor, but it is included
// here so an actor that ever points at it is never mistaken for stateful.
const (
	VendorAgentName  = "salem-vendor"
	GenericAgentName = "salem-generic"
)

// SharedVASlugs returns the canonical shared-VA driver slugs — the stateless
// switchboard VAs any actor may be linked to (vendor / visitor / generic), as
// opposed to a dedicated 1:1 persistent VA. Order is not guaranteed; callers
// that need determinism sort. Used by the boot rate-limit query
// (cmd/engine.agentSlugsToQuery) and the editor's assignable-driver catalog
// (GET /api/village/agent-drivers, LLM-256).
func SharedVASlugs() []string {
	return []string{VendorAgentName, VisitorAgentName, GenericAgentName}
}

// isSharedVAAgent reports whether an llm_memory_agent slug is one of the
// shared switchboard VAs rather than an actor's own private VA.
func isSharedVAAgent(agent string) bool {
	switch agent {
	case VendorAgentName, VisitorAgentName, GenericAgentName:
		return true
	default:
		return false
	}
}

// ClassifyActorKind derives an actor's Kind from its persisted driver columns.
// There is no actor_kind column, so Kind is reconstructed on every DB load
// from login_username + llm_memory_agent (a CHECK constraint keeps the two
// mutually exclusive). This mirrors the Kind that create_pc / create_npc set
// in memory at creation time:
//   - login_username present        -> KindPC (human player)
//   - llm_memory_agent is a shared VA -> KindNPCShared (vendor / visitor)
//   - llm_memory_agent present        -> KindNPCStateful (own persistent VA)
//   - neither                         -> KindDecorative (sprite-only, never ticked)
func ClassifyActorKind(loginUsername, llmAgent string) ActorKind {
	loginUsername = strings.TrimSpace(loginUsername)
	llmAgent = strings.TrimSpace(llmAgent)
	if loginUsername != "" {
		return KindPC
	}
	if isSharedVAAgent(llmAgent) {
		return KindNPCShared
	}
	if llmAgent != "" {
		return KindNPCStateful
	}
	return KindDecorative
}

// ActorIsDriven reports whether something drives this actor — an LLM agent or
// a human login — as opposed to it being sprite-only scenery. It is the
// complement of KindDecorative, expressed in the two PERSISTED COLUMNS rather
// than the derived Kind.
//
// Reading the columns is deliberate, not incidental (LLM-586). This predicate
// is the in-memory mirror of the actor_display_name_excl constraint, whose SQL
// is character-for-character the same test:
//
//	WHERE (llm_memory_agent IS NOT NULL OR login_username IS NOT NULL)
//
// Going through Kind instead would make the two equivalent only so long as
// Kind is recomputed synchronously on every write to either column — a
// standing invariant to maintain, and one whose breach would silently let a
// row into the database predicate without passing the in-memory gate. Off the
// columns there is no invariant to keep.
//
// The comparison is a bare != "" and MUST NOT trim. The write path's
// nilOnEmpty maps only the exact empty string to SQL NULL, so a whitespace-only
// value like " " persists as NOT NULL and is DRIVEN to the constraint. Trimming
// here would call that same actor decorative, and the in-memory gate would then
// wave through a duplicate that the deferred constraint rejects at COMMIT —
// which is a wedged checkpoint, not a returned error. Non-NULL is exactly
// `!= ""` on this side; keep the two spelled the same way.
//
// This deliberately diverges from ClassifyActorKind, which DOES trim: a " "
// agent is scenery as far as ticking is concerned but occupies a name as far
// as the database is concerned. Erring toward "driven" is the safe direction —
// it over-enforces uniqueness rather than under-enforcing it.
func ActorIsDriven(a *Actor) bool {
	return a != nil && (a.LLMAgent != "" || a.LoginUsername != "")
}

// MemoryPartition is the SINGLE source of truth for where an actor's private
// memory lives inside its llm-memory namespace (LLM-356). It returns the slug
// prefix that scopes that actor's memory and whether the actor has memory at
// all. Both the memory tools (recall reads it as the search slug_prefix,
// memorize writes under it) and the perception tool-gate derive from this one
// function, so isolation can't drift between "what's advertised" and "what's
// searched/written".
//
//   - KindNPCStateful → ("", true): a dedicated-VA NPC owns its namespace
//     outright (zbbs-<name>), so its memory is the whole namespace — recall
//     spans its notes, dreams, and impressions, and memorize writes under
//     "memory/" within it. No prefix needed.
//   - KindNPCShared → ("<name>/", true): a shared-VA NPC lives in a pooled
//     namespace (salem-vendor) with six others, so its memory is sectioned
//     under a per-NPC slug prefix derived from its display name. Returns
//     (\"\", false) if the name slugifies to empty — better no memory than a
//     broken "/memory/…" prefix that would pool with any other nameless actor.
//   - KindPC / KindDecorative → ("", false): no VA-backed memory.
func MemoryPartition(kind ActorKind, displayName string) (slugPrefix string, hasMemory bool) {
	switch kind {
	case KindNPCStateful:
		return "", true
	case KindNPCShared:
		slug := Slugify(displayName)
		if slug == "" {
			return "", false
		}
		return slug + "/", true
	default:
		return "", false
	}
}

// Slugify lowercases a string and collapses each run of non-alphanumeric
// characters into a single hyphen, trimming leading/trailing hyphens —
// "Anne Walker" → "anne-walker", "The Blacksmith's Name" →
// "the-blacksmith-s-name". Used to derive a shared-VA NPC's memory partition
// prefix from its display name and a memory's slug from its topic (LLM-356);
// the result is a slug segment the memory-api's sanitize.identifier accepts
// verbatim.
func Slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastHyphen = false
			continue
		}
		// Any other rune (space, punctuation, apostrophe in "O'Brien") becomes a
		// single hyphen; runs collapse so we never emit "--".
		if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// ActorState is the macro-state of an actor: what it is doing right now at
// a coarse level. Set softly by engine handlers when they observe a change;
// there is no strict FSM that validates transitions. Consumer switches must
// always include a default branch so adding new state values never breaks
// them. See shared/tasks/engine-in-memory-rewrite/state-model-sketch.
type ActorState string

const (
	StateIdle          ActorState = "idle"
	StateWalking       ActorState = "walking"
	StateConversing    ActorState = "conversing"
	StateLaboring      ActorState = "laboring" // fulfilling an accepted solicit_work commitment (LLM-26)
	StateResting       ActorState = "resting"  // take_break, dwell-credit accumulating
	StateSleeping      ActorState = "sleeping"
	StateShopping      ActorState = "shopping"       // buy_walker active
	StateInTransaction ActorState = "in_transaction" // pay flow open
	StateEating        ActorState = "eating"         // mid-consume
)

// Action is one LLM tool call (or engine-initiated action) recorded in the
// actor's RecentActions ring buffer. Used by perception build to diff
// against previous tick and by debug surfaces.
type Action struct {
	At      time.Time
	Tool    string // "speak", "move_to", "pay", ...
	Params  map[string]any
	Outcome string
	SceneID SceneID
}

// NeedKey identifies a kind of need: "hunger", "thirst", "tiredness", etc.
type NeedKey string

// ItemKind identifies a kind of item in inventory: "bread", "ale", etc.
type ItemKind string

// DwellCreditSource discriminates the two flavors of dwell credit:
// "object" (persistent while the actor is at a recovery-tagged village
// object — a Shade Tree, a Well) and "item" (one-shot countdown unlocked
// by consuming an item with a dwell effect — bread that keeps satiating
// you for a few minutes after eating).
type DwellCreditSource string

const (
	DwellSourceObject DwellCreditSource = "object"
	DwellSourceItem   DwellCreditSource = "item"
)

// DwellCreditKey is the composite primary key for an actor's dwell-credit
// row: object + attribute + source. Multiple rows on one (actor, object)
// are allowed — a shaded oak credits both tiredness and hunger
// independently, and "object" + "item" credits on the same attribute are
// separate rows.
type DwellCreditKey struct {
	ObjectID  VillageObjectID
	Attribute NeedKey
	Source    DwellCreditSource
}

// DwellCredit accumulates "I've been here long enough" toward periodic
// need recovery (ZBBS-172). The per-minute dwell tick reads these rows,
// applies DwellDelta to the actor when a DwellPeriodMinutes window has
// elapsed since LastCreditedAt, and advances the anchor.
//
// Source="object" credits persist as long as the actor stays at the
// object; their RemainingTicks is nil (open-ended). Source="item"
// credits have a finite RemainingTicks countdown that decrements per
// applied period and removes the row at zero.
//
// Kind carries the ItemKind that created an item-source credit so
// perception ("you are currently eating stew at the tavern") and event
// payloads can identify the meal without a separate lookup. Empty for
// source=object credits (no item involved).
type DwellCredit struct {
	ObjectID           VillageObjectID
	Kind               ItemKind // empty for source=object
	Attribute          NeedKey
	Source             DwellCreditSource
	LastCreditedAt     time.Time
	RemainingTicks     *int // nil for source=object; >0 for source=item
	DwellDelta         int  // negative — applied per period
	DwellPeriodMinutes int
}

// Acquaintance is a per-actor "do I know this person by name?" marker.
// Keyed by display name on the actor's Acquaintances map (TEXT-keyed in
// the underlying npc_acquaintance table so NPC↔PC pairs work without a
// cross-table FK). Applies to ALL NPCs regardless of Kind — even stateful
// NPCs need the gate so perception renders strangers as descriptors
// ("the blacksmith") rather than greeting unknowns by name.
//
// Written by a subscriber to ActorMet, fired on huddle membership change.
// Symmetric in concept but stored as directed pairs — the subscriber
// writes both directions.
type Acquaintance struct {
	FirstInteractedAt time.Time
}

// Relationship is the per-pair narrative state for a SHARED-VA NPC's
// view of another actor: a summary + an append-only trail of recent
// interactions, plus consolidation bookkeeping. Stateful NPCs do NOT
// populate Relationships — their own VA carries continuity via memory-
// api. Gate: Actor.Kind == KindNPCShared.
//
// SalientFacts is hard-bounded by MaxSalientFactsPerRelationship in
// RecordInteraction (FIFO eviction, DroppedFactCount telemetry) and
// further bounded by consolidation (when it lands), which rewrites
// SummaryText from the trail and prunes consolidated facts. Per-fact
// Text is truncated at write time to MaxSalientFactTextLen runes.
type Relationship struct {
	SummaryText        string
	SalientFacts       []SalientFact
	InteractionCount   int
	LastInteractionAt  *time.Time
	LastConsolidatedAt *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	// DroppedFactCount counts FIFO evictions when SalientFacts hit
	// MaxSalientFactsPerRelationship. Per-pair telemetry: admin views
	// can spot relationships that are churning facts faster than
	// consolidation prunes them. Never decremented.
	DroppedFactCount int
}

// SalientFact is one entry in a Relationship's interaction trail. Mirrors
// the v1 JSONB element shape {at, kind, text} so the pg-impl SaveSnapshot
// can round-trip without a separate intermediate.
type SalientFact struct {
	At   time.Time
	Kind InteractionKind
	Text string
}

// InteractionKind tags what produced a SalientFact. Stored as plain
// string in JSONB; typed at the callsite to survive rename refactors
// and prevent typos.
type InteractionKind string

const (
	InteractionSpoke         InteractionKind = "spoke"
	InteractionHeard         InteractionKind = "heard"
	InteractionPaid          InteractionKind = "paid"
	InteractionPaidBy        InteractionKind = "paid_by"
	InteractionPayDeclinedBy InteractionKind = "pay_declined_by"
	InteractionDeclinedPay   InteractionKind = "declined_pay"
	InteractionCounteredBy   InteractionKind = "countered_by"
	InteractionCountered     InteractionKind = "countered"
	// InteractionGave / InteractionReceivedGift record a one-way gift
	// (LLM-138): the giver hands goods to a co-present recipient for
	// nothing in return. Gave is written giver→recipient, ReceivedGift
	// recipient→giver — the gift counterpart to the Paid/PaidBy pair (a
	// gift has no coin or return-goods leg, so Paid would mislabel it).
	InteractionGave         InteractionKind = "gave"
	InteractionReceivedGift InteractionKind = "received_gift"
	InteractionServed       InteractionKind = "served"
	InteractionServedBy     InteractionKind = "served_by"
	InteractionDelivered    InteractionKind = "delivered"
	InteractionReceived     InteractionKind = "received"
	// Labor (LLM-26 service-for-pay) relationship facts (LLM-165), written at
	// the labor terminals as the analogue of the pay family's Paid/Declined
	// writes. Each is one side of a bidirectional pair (worker→employer +
	// employer→worker). Worked/Hired record a completed, paid job;
	// WorkedUnpaid/LeftWorkerUnpaid record a job the worker finished but the
	// employer could no longer pay for (the completion-time failed_unavailable —
	// the aggrieved beat pay has no equivalent of); WorkDeclinedBy/DeclinedWork
	// record a refused offer. Expired offers and the accept-time
	// failed_unavailable fall-throughs write nothing — no social move happened.
	InteractionWorked           InteractionKind = "worked"
	InteractionHired            InteractionKind = "hired"
	InteractionWorkedUnpaid     InteractionKind = "worked_unpaid"
	InteractionLeftWorkerUnpaid InteractionKind = "left_worker_unpaid"
	InteractionWorkDeclinedBy   InteractionKind = "work_declined_by"
	InteractionDeclinedWork     InteractionKind = "declined_work"
	// Commission partial-payment expiry (LLM-357): a made-to-order deal with a
	// deposit that lapsed at Ready. KeptDeposit/ForfeitedDeposit are the
	// buyer-fault pair (the buyer never collected, so the seller kept the deposit
	// and re-shelved the goods); RefundedDeposit/DepositRefunded are the
	// seller-fault pair (the seller never made it, so the deposit went back).
	// Each is one side of a bidirectional write, like Delivered/Received — the
	// reputation seed both keepers carry forward. Full-prepay expiry writes
	// nothing (silent as before this ticket).
	InteractionKeptDeposit      InteractionKind = "kept_deposit"
	InteractionForfeitedDeposit InteractionKind = "forfeited_deposit"
	InteractionRefundedDeposit  InteractionKind = "refunded_deposit"
	InteractionDepositRefunded  InteractionKind = "deposit_refunded"
)

// MaxSalientFactTextLen caps per-fact Text at write time so a single
// rambling speech turn can't blow out a relationship's JSONB row. Mirrors
// v1's salientTextMaxLen (220 runes).
const MaxSalientFactTextLen = 220

// MaxSalientFactsPerRelationship caps stored SalientFacts per pair.
// Enforced in RecordInteraction with FIFO eviction (oldest dropped) +
// Relationship.DroppedFactCount increment. Sized to hold a full day of
// interactions so the once-daily consolidation (ConsolidationFloor)
// reflects the whole day, not just the most recent slice. The
// ConsolidationCeiling backstop is expected to fire before a pair
// reaches this cap, keeping FIFO eviction a last-resort safety net — a
// non-zero DroppedFactCount means a pair out-ran even the backstop.
const MaxSalientFactsPerRelationship = 200

// NewSalientFact builds a SalientFact with Text capped at
// MaxSalientFactTextLen runes, MARKED when it has to cut (LLM-405). Use this
// at every write callsite — never construct a SalientFact directly when the
// text comes from LLM output or other untrusted source.
//
// This is the last cut a fact takes before it is stored, and a stored fact is
// read straight back into a prompt (perception surfaces the most recent facts
// per peer). A bare prefix here is therefore a memory that ENDS MID-SENTENCE,
// which the reader answers as though something were owed — see text.go.
func NewSalientFact(at time.Time, kind InteractionKind, text string) SalientFact {
	return SalientFact{At: at, Kind: kind, Text: capRunesMarked(text, MaxSalientFactTextLen)}
}

// cloneRelationships deep-copies a Relationships map. Used by CloneActor
// and snapshotActor so the published Snapshot's Relationships are
// genuinely isolated from world state — a snapshot consumer mutating
// rel.SalientFacts[0].Text would otherwise corrupt the world's source
// of truth.
func cloneRelationships(src map[ActorID]*Relationship) map[ActorID]*Relationship {
	if src == nil {
		return nil
	}
	dst := make(map[ActorID]*Relationship, len(src))
	for k, v := range src {
		if v == nil {
			continue
		}
		vc := *v
		if v.LastInteractionAt != nil {
			t := *v.LastInteractionAt
			vc.LastInteractionAt = &t
		}
		if v.LastConsolidatedAt != nil {
			t := *v.LastConsolidatedAt
			vc.LastConsolidatedAt = &t
		}
		if v.SalientFacts != nil {
			// SalientFact is a value type with no inner pointers
			// (time.Time is a value), so slice copy is enough.
			vc.SalientFacts = append([]SalientFact(nil), v.SalientFacts...)
		}
		dst[k] = &vc
	}
	return dst
}

// cloneNarrativeState deep-copies a NarrativeState pointer. Same
// rationale as cloneRelationships — published snapshot must be
// isolated from world state.
func cloneNarrativeState(src *NarrativeState) *NarrativeState {
	if src == nil {
		return nil
	}
	nc := *src
	if src.LastConsolidatedAt != nil {
		t := *src.LastConsolidatedAt
		nc.LastConsolidatedAt = &t
	}
	return &nc
}

// cloneAcquaintances copies an Acquaintances map. Acquaintance is a
// value type with no inner pointers, so a per-key value-copy is enough.
func cloneAcquaintances(src map[string]Acquaintance) map[string]Acquaintance {
	if src == nil {
		return nil
	}
	dst := make(map[string]Acquaintance, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// NarrativeState is the engine-side continuity layer for shared-VA NPCs.
// Nil for stateful-VA actors — their own VA loads context/soul into the
// system prompt.
//
// AboutMe is the rendered identity: the accreting first-person "soul" the
// per-actor narrative sweep synthesizes each day via the dream-sim-soul agent
// (LLM-199), shown in perception's "## Who you are" block. SeedText (dream-
// pipeline input, never populated for shared VAs) and EvolvingSummary (the
// older flat-paragraph consolidation output) are kept as distinct columns for
// round-trip, but neither is rendered: SeedText was the only field render ever
// emitted and shared VAs have none; EvolvingSummary is legacy (the sweep that
// wrote it now writes AboutMe instead).
type NarrativeState struct {
	SeedText           string
	EvolvingSummary    string
	AboutMe            string
	LastConsolidatedAt *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// VisitorState is the per-visitor archetype state. Non-nil marks the actor
// as a transient salem-visitor — a shared-VA NPC that arrived on a random
// map edge, hangs around the tavern for hours-to-a-day, then departs.
// Nil for every non-visitor actor (stateful NPCs, persistent shared-VA
// vendors, PCs, decoratives).
//
// Visitors are KindNPCShared but cross-cascade gates check VisitorState !=
// nil to skip narrative state accumulation (relationships, narrative
// consolidation, idle backstops). The pointer-presence check is the
// "transient visitor" predicate across the engine; see
// shared/notes/codebase/salem-engine-v2/visitor for the full surface.
//
// TradeDirection is which way a merchant visitor's errand moves coin between him
// and the village (LLM-455). A BUY visitor pays the village for a real good and
// carries it off the map — coin flows IN (an injection). A SELL visitor brings a
// good the village can't make (the wholesale factor's cloth / iron / salt) and the
// village pays him for it — the coin he is paid leaves the map when he departs (a
// drain). Which one a given traveler is is chosen at spawn by the coin-valve
// (chooseVisitorTradeDirection), biased by the resident money supply against an
// operator band.
type TradeDirection string

const (
	// TradeDirectionBuy — the visitor buys a real village good from its keeper and
	// carries it off; his coin stays behind. The village's monetary injection.
	TradeDirectionBuy TradeDirection = "buy"
	// TradeDirectionSell — the visitor sells the village an import it can't make,
	// through the distributor; the coin he is paid leaves the map with him. The drain,
	// and the first (and today only) instance is the wholesale factor (LLM-410).
	TradeDirectionSell TradeDirection = "sell"
)

// Valid reports whether d is a known trade direction — the Go-owned allowlist the
// persistence boundary checks, mirroring VisitorPhase.Valid.
func (d TradeDirection) Valid() bool {
	return d == TradeDirectionBuy || d == TradeDirectionSell
}

// TradeErrand is a merchant visitor's grounded reason for coming (LLM-455) — the
// generalization of the LLM-410 wholesale factor from a hardcoded one-off into
// first-class state. A traveler carrying a non-nil Trade came to do ONE real trade
// with ONE real keeper; a nil Trade is a passer-through (messenger / musician /
// preacher / scholar / surgeon) who carries only voice-flavor and no commerce.
//
// Binding the errand to a REAL catalog good and a REAL open keeper AT SPAWN is what
// fixes the ungrounded-persona loop (the "wool-buyer" with no wool to buy, walking
// forever toward a place that does not exist): the persona can only ever name a trade
// the economy can actually service, because the good + counterparty are chosen from
// live world state before the archetype label is derived from them.
//
// The errand also anchors the daytime rounds — the Counterparty is the one must-hit
// stop, and commerce is confined to it (TradeErrandSteer + the talk-only tool gate),
// so the traveler trades where his errand is and only passes news elsewhere.
type TradeErrand struct {
	// Direction — buy (visitor pays the village) or sell (village pays the visitor).
	// Drives the coin-valve framing and which side initiates the trade.
	Direction TradeDirection
	// Good is the REAL catalog item the errand is about — the good he buys (a village
	// export) or the headline of the bale he sells (an import). Chosen from live state
	// at spawn; the archetype label is derived from it ("cheese" -> "cheese-buyer").
	Good ItemKind
	// Counterparty is the REAL keeper-business the trade happens at — the open keeper
	// who sells the Good (buy) or the village distributor who absorbs imports (sell).
	// The one must-hit stop; commerce is gated to it.
	Counterparty StructureID
	// Settled flips true once the errand trade completes (the pay settles) or is proven
	// impossible for the day (the counterparty shut / the day's trade window closed) —
	// the legible "business concluded" state that winds the traveler down to the tavern
	// instead of looping his rounds forever.
	Settled bool
	// ShipmentQty is how much of Good a SELLER arrived carrying — the size of the shipment
	// he came to deliver, stamped at spawn. The target Delivered is measured against; a
	// buyer leaves it 0 (he arrives empty-packed and settles on holding the good instead).
	ShipmentQty int
	// Delivered is how much of Good this SELLER has handed specifically to the errand
	// Counterparty business — NOT a general count of what has left his pack. It is
	// accumulated at the one goods-transfer choke point (transferItem, via
	// sellErrandCredit) and never decremented.
	//
	// Read the scope literally before reusing this field: a transfer of the errand good to
	// a bystander, to a different keeper, or as an ungated `give` credits NOTHING. That is
	// the point — a SELL errand is completed by delivering to the business he came to deal
	// with, and a counter that also counted disposal would settle a factor who gave his
	// iron away having sold none of it. It is not a global transfer ledger and must not be
	// used as one.
	//
	// It exists because current holdings cannot tell "never sold it" from "sold it and
	// bought some back", and the factor's errand is a TWO-WAY deal — he sells his bale and
	// buys the village's surplus in the same breath. A factor who moved all ten bars and
	// then took three back off the keeper's shelf holds three either way, so any test on
	// Inventory[Good] either fails to settle him or settles a man who has sold nothing.
	// A monotonic count of what reached the counterparty is the honest measure.
	Delivered int
}

// Archetype / Origin / Disposition come from per-spawn random pools in
// engine/sim/visitor.go and feed the perception "Visitors here" block plus
// the per-call identity preface the shared salem-visitor VA reads.
// ExpiresAt is the wall-clock departure deadline; Phase is the visitor's
// lifecycle state — VisitorPhaseDeparting means the despawn walk has been
// issued (so the visitor ticker doesn't keep re-issuing it tick after tick).
type VisitorState struct {
	Archetype   string
	Origin      string
	Disposition string
	ExpiresAt   time.Time
	Phase       VisitorPhase
	// Payload is the one grounded item of news the traveler carries — a diegetic,
	// past-tense clause about a real thing that happened in the village
	// recently ("Ezekiel Crane turned out a plow for the Hale farm"),
	// selected at spawn from the in-memory action log (selectRoadWord in
	// engine/sim/visitor.go) and voiced through the identity preface
	// (renderTravelerPreface, LLM-371). "" when no carry-worthy beat was on
	// hand at spawn — the preface simply drops the clause. Persisted in the
	// visitor.payload column so the carried word survives a deploy restart
	// (the action log is restart-wiped, so re-selecting on rehydrate would
	// draw from an empty pool). Not live-updated: it is a snapshot of what
	// the traveler "heard on the road," fixed for the visit.
	Payload string

	// RecurringID links this in-flight traveler to its durable returner identity
	// (recurring_visitor.id, rvis-<8hex>) once promoted — set the first time the
	// traveler shares a scene with a player (handleVisitorReturnerMeet, LLM-372),
	// or at spawn for a returner coming back. "" for a not-yet-promoted stranger.
	// Persisted in the visitor.recurring_visitor_id column (same checkpoint Tx as
	// the recurring_visitor row it points at) so a mid-visit deploy keeps the
	// linkage instead of re-promoting the same traveler as a duplicate persona.
	RecurringID string

	// Trade is a merchant visitor's grounded errand (LLM-455) — non-nil for a merchant
	// (a bound buy or sell with a real good + real counterparty), nil for a passer-through
	// (pure voice-flavor, no commerce). Generalizes the LLM-410 DistributorOnly factor
	// flag: the wholesale factor is now the "sell" instance whose Counterparty is the
	// village distributor. Set once at spawn (bindVisitorErrand), immutable but for the
	// Settled flip; persisted in the visitor.plan jsonb so a mid-visit redeploy resumes
	// the traveler on the same errand rather than as a plain peddler. Drives the errand
	// rounds cue, the commerce-confinement steer (TradeErrandSteer) + talk-only tool gate,
	// and the coin-valve. A trade is confined to the Counterparty keeper; self-provisioning
	// (a meal, a bed) stays reachable anywhere.
	Trade *TradeErrand

	// Day-plan rounds (LLM-373, model-driven since LLM-379). During the daytime
	// portion of his stay the traveler makes his rounds of the village, trading and
	// passing news. He navigates HIMSELF with move_to — the engine renders his
	// situation ("## Your rounds") and paces him (dispatchVisitorPacing) but no longer
	// chooses his stops. Persisted (with the pack/purse and any booked-room grant) in
	// the visitor.plan jsonb column so a mid-stay deploy resumes him.
	//
	//   - VisitedBusinesses: keeper-businesses he has actually called at, recorded on
	//     real co-present arrival (cascade/visitor_arrival.go). The rounds cue renders
	//     these back so a stateless shared VA "remembers" where it has been and routes
	//     onward instead of repeating a shop.
	VisitedBusinesses []StructureID

	// PayloadSharedWith is the people-half of the same memory (LLM-545): the
	// actors the traveler has already passed his carried word to. Stamped on the
	// Speak commit path (recordRoadWordShared) for every ACTIVE conversant in
	// the huddle — a peer who has themselves spoken — mirroring how the rumor.go
	// layer moves its token through conversation rather than co-presence, and
	// without reading the utterance: a real two-way exchange while carrying a
	// payload is taken as the word having been given. The identity preface reads
	// it back per co-present peer (TravelerSelfView.RoadWordSharedWith) so a
	// traveler who returns to someone stops reopening a matter already spent
	// between them. Visit-scoped like the Payload itself — no TTL, no decay —
	// and persisted in the visitor.plan jsonb so the constant deploys don't
	// resurrect the word as unspent mid-visit (the LLM-547 lesson: three deploys
	// sat inside one innkeeper's afternoon).
	PayloadSharedWith []ActorID

	// SpendBudget is the coin this traveler may still SPEND in the village —
	// seeded equal to the purse at spawn and drawn down by every coin he pays
	// out, by any door (LLM-644). Sale proceeds credit Coins as always but
	// never raise this, so what he earns here is takings bound for home, not
	// fresh buying power. Without the split a forced-seller factor recycled
	// his whole bale's proceeds into surplus-buying and every "drain" visit
	// netted the village +purse; with it his spending is capped at what he
	// arrived carrying and the coin-band SELL direction can actually drain.
	// The one question every affordability gate asks is SpendableCoins()
	// (min of wallet and budget — nil VisitorState means no cap); mutate only
	// through drawVisitorSpend / refundVisitorSpend, which clamp at 0.
	// Persisted in the visitor.plan jsonb; a pre-LLM-644 row rehydrates with
	// budget = wallet (the old uncapped behavior, for that one in-flight
	// visit). An operator /grant raises it alongside the wallet — fiat coin
	// is meant to be spendable (the LLM-410 float precedent).
	SpendBudget int
}

// VisitorPhase is the visitor's lifecycle state — a small Go-owned enum
// persisted to the visitor.phase column. A string, not a bool: the lifecycle
// is inherently multi-state and grows — LLM-373 adds arriving / making_rounds
// / lodging between present and departing. Go owns the allowlist; the column
// carries no DB CHECK (a CHECK refusing a Go-side value would wedge the
// checkpoint Tx, matching labor_contract.state).
type VisitorPhase string

const (
	// VisitorPhasePresent — in the village, not leaving. The pre-LLM-373 spawn
	// phase; retained for backward compatibility so a visitor row checkpointed by
	// an older engine rehydrates cleanly (the circuit treats it like arriving).
	VisitorPhasePresent VisitorPhase = "present"
	// VisitorPhaseArriving — spawned on a road edge, walking in to the first stop
	// on the circuit. Becomes making_rounds once the traveler reaches a business
	// (LLM-373).
	VisitorPhaseArriving VisitorPhase = "arriving"
	// VisitorPhaseMakingRounds — the daytime business circuit: the traveler visits
	// each open, tagged business once, trading and passing news, then moves on
	// (LLM-373). The engine steps the route; the VA speaks.
	VisitorPhaseMakingRounds VisitorPhase = "making_rounds"
	// VisitorPhaseLodging — the evening: the traveler is drawn to the tavern by the
	// evening-leisure cue and seeks a bed for the night, booking a room from its
	// pack through the real lodging flow (LLM-373). Entered when the civil evening
	// window opens or the rounds are exhausted.
	VisitorPhaseLodging VisitorPhase = "lodging"
	// VisitorPhaseDeparting — ExpiresAt passed and the despawn walk to a map
	// edge has been issued. One-shot: dispatchVisitorDespawn won't re-fire once
	// a visitor is in this phase.
	VisitorPhaseDeparting VisitorPhase = "departing"
)

// Valid reports whether p is a known visitor phase. The persistence boundary
// uses it to reject an unknown phase on write (SaveSnapshot — a Go-side bug) and
// to drop an unknown phase on rehydrate (an out-of-band DB edit): "Go owns the
// allowlist". Grows with the enum — add new values here as LLM-373 introduces
// arriving / making_rounds / lodging.
func (p VisitorPhase) Valid() bool {
	switch p {
	case VisitorPhasePresent, VisitorPhaseArriving, VisitorPhaseMakingRounds,
		VisitorPhaseLodging, VisitorPhaseDeparting:
		return true
	default:
		return false
	}
}

// LoadedVisitor is the persisted-and-reloaded form of an in-flight visitor
// (LLM-369) — what VisitorsRepo.LoadAll returns and rehydrateVisitorsOnLoad
// rebuilds a live Actor from. It carries the reconcile-critical typed columns
// plus the day-plan (LLM-373) parsed off the plan jsonb; the rest of the Actor
// (Kind, LLMAgent, seeded needs, StateIdle) is reconstructed the way spawn mints
// one.
type LoadedVisitor struct {
	ID                ActorID
	DisplayName       string
	Pos               TilePos
	InsideStructureID StructureID
	VisitorState      *VisitorState

	// Day-plan mutable state (LLM-373), restored from the plan jsonb onto the
	// rebuilt Actor: the pack (Inventory) and purse (Coins) the traveler carries,
	// and any booked-room RoomAccess grant. nil Inventory / RoomAccess mean the
	// traveler carried nothing / had no room — rehydrate seeds an empty map.
	Inventory  map[ItemKind]int
	Coins      int
	RoomAccess map[RoomAccessKey]*RoomAccess
}

// cloneVisitorState deep-copies a VisitorState pointer. The scalar fields
// (string / time.Time / VisitorPhase) copy by value with the struct copy; the
// VisitedBusinesses (LLM-373) and PayloadSharedWith (LLM-545) slices and the
// Trade errand pointer (LLM-455) are deep-copied so a snapshot never aliases
// the world's mutable rounds/errand/shared-word state.
func cloneVisitorState(src *VisitorState) *VisitorState {
	if src == nil {
		return nil
	}
	cp := *src
	if src.VisitedBusinesses != nil {
		cp.VisitedBusinesses = append([]StructureID(nil), src.VisitedBusinesses...)
	}
	if src.PayloadSharedWith != nil {
		cp.PayloadSharedWith = append([]ActorID(nil), src.PayloadSharedWith...)
	}
	if src.Trade != nil {
		trade := *src.Trade
		cp.Trade = &trade
	}
	return &cp
}

// Actor is the in-memory model of one participant in the simulation: NPC,
// PC, or decorative. One actor's data is logically one aggregate from the
// repository's perspective — ActorsRepo owns this entity plus all child
// tables (needs, inventory, relationships, acquaintances, narrative, dwell
// credits, attributes).
type Actor struct {
	ID          ActorID
	DisplayName string
	Role        string
	Kind        ActorKind

	// Identity routing — which VA backs this actor, login binding for PCs,
	// visitor archetype state, businessowner attribute (engine-authored
	// hospitality speech for shopkeepers / innkeepers / smiths — see
	// engine/sim/businessowner.go).
	LLMAgent           string
	LoginUsername      string
	VisitorState       *VisitorState
	BusinessownerState *BusinessownerState

	// IsAdmin gates the admin/editor write routes on the HTTP surface
	// (force-phase, object reposition/delete). Externally managed — set
	// directly in the DB for the human operators who administer the
	// village; the sim never writes it, and the checkpoint UPSERT
	// deliberately omits it so a save can't clobber the operator-set value
	// (LoadWorld reads it, SaveWorld leaves it). See migration ZBBS-WORK-271.
	IsAdmin bool

	// Spatial — current location. Pos is the actor's tile (padded grid
	// coords); see geom.go. Was a CurrentX/CurrentY int pair — folded into
	// TilePos so it can never be mixed with a world-pixel coordinate.
	InsideStructureID StructureID
	InsideRoomID      RoomID // 0 when not in a room
	Pos               TilePos
	CurrentHuddleID   HuddleID

	// Render identity — client-facing only, the engine never branches on
	// these. SpriteID references the npc_sprite catalog (World.Sprites) for
	// the sheet + animation rows; the client read surface inlines the
	// resolved sprite onto the agent DTO. Facing is the initial/spawn render
	// direction (north/south/east/west). The v2 engine does NOT update Facing
	// per-tick — the client derives live facing from movement delta — so it
	// round-trips through checkpoint as the last-persisted value, restoring
	// spawn orientation on restart (interior-facing writeback is a far-out
	// follow-up). Both empty for actors without a sprite (some PCs / purely
	// logical actors).
	SpriteID SpriteID
	Facing   string

	// Anchors — home and work bindings (empty for actors without them).
	HomeStructureID StructureID
	WorkStructureID StructureID

	// Schedule (minute-of-day; nil if unset — falls back to world dawn/dusk).
	// Persisted as nullable SMALLINT; nil round-trips through SQL NULL.
	ScheduleStartMin *int
	ScheduleEndMin   *int

	// Mutable state.
	Needs     map[NeedKey]int
	Inventory map[ItemKind]int
	Coins     int

	// ToolWear is the per-kind wear state of this actor's durable tools
	// (LLM-330): uses REMAINING on the in-use unit of each tool kind. A
	// missing entry means no unit has been taken up — all on-hand units are
	// fresh. Worn by applyToolWear once per produce execution; the entry
	// clears when the unit is spent (and its inventory count drops). Only
	// kinds with catalog DurabilityUses > 0 ever get entries. Checkpointed as
	// actor_inventory.uses_left on the kind's inventory row, so wear dies with
	// the stock that carries it.
	ToolWear map[ItemKind]int

	// GarmentWear is the per-kind wear state of this actor's wearable garments
	// (LLM-422): worked MINUTES remaining on the in-use unit of each garment
	// kind. The worked-minute sibling of ToolWear — same in-use-unit model (a
	// missing entry = no unit taken up, all on-hand units fresh; the entry
	// clears when the unit is spent and its inventory count drops), but drawn by
	// applyGarmentWear on the per-minute garment-wear sweep while the bearer is
	// in a working posture (actorWearsGarments), not per produce execution. Only
	// kinds with catalog WearMinutes > 0 ever get entries. Checkpointed as
	// actor_inventory.worn_minutes_left on the kind's inventory row (a separate
	// column from ToolWear's uses_left — a kind is a tool or a garment, never
	// both), so wear dies with the stock that carries it.
	GarmentWear map[ItemKind]int

	// Activity windows.
	BreakUntil    *time.Time
	SleepingUntil *time.Time

	// LaborID is the accepted labor offer the worker is currently committed to
	// (LLM-26), or LaborID(0) when not on a job. The AUTHORITATIVE per-actor
	// ownership key: set by AcceptWork to the offer's id, cleared by the
	// completion sweep when THAT id settles. The settle path guards on this id
	// (not on the window timestamp) so settling a stale offer can never free a
	// worker who has since taken a different job (code_review). StateLaboring is
	// always paired with a non-zero LaborID.
	//
	// LaboringUntil is the matching completion deadline, kept as the activity-
	// window mirror (the BreakUntil/SleepingUntil pattern). Set/cleared in
	// lockstep with LaborID. The authoritative window lives on the LaborOffer
	// (WorkingUntil); this copy documents the job's end on the actor.
	//
	// Both are TRANSIENT — deliberately NOT checkpointed, unlike BreakUntil/
	// SleepingUntil. Their settlement authority, World.LaborLedger, is itself
	// in-memory-only and restart-lossy (the sibling PayLedger's accepted
	// 2026-05-20 design). Persisting them WITHOUT the ledger would be the
	// WORK-410 orphan in reverse: a restored StateLaboring actor with no offer
	// left to settle it, stuck laboring forever. So they are lost together with
	// the ledger — on restart the actor reverts cleanly to idle, and no coins
	// are stranded (the reward only ever moves at completion, never before).
	// LaboringUntil is cloned in CloneActor (pointer field); LaborID rides the
	// value copy.
	LaborID       LaborID
	LaboringUntil *time.Time

	// SourceActivity is an in-flight, timed action AT a village object — eating
	// or drinking in place at a refresh source, or harvesting a gatherable
	// source (LLM-54). The actor is occupied until SourceActivity.Until; the
	// completion sweep (RunSourceActivityTicker) applies the effect then. nil
	// when not engaged at a source.
	//
	// TRANSIENT — deliberately NOT checkpointed (unlike BreakUntil/SleepingUntil),
	// like OpenUntil. The window is seconds-scale, so restart-loss is wholly
	// benign: a lost in-flight bite/harvest just never applied its effect (the
	// persistent need/inventory/supply mutation lands atomically at completion,
	// never mid-window), so there is no torn state to recover and the actor
	// simply re-engages on its next arrival/tool call. A durable column for a
	// 3-second timer would be exactly the "Postgres as cadence store" the
	// architecture avoids.
	SourceActivity *SourceActivity

	// OpenUntil is a keeper's commitment to stay open past the end of its shift,
	// until this instant (ZBBS-WORK-387 stay_open). The inverse of BreakUntil:
	// while set it SUPPRESSES the off-shift wind-down (the go-home / to-inn duty
	// in shiftDutyTarget and the renderDutySteer perception cue) so the
	// level-triggered shift producer stops re-ticking the keeper home every
	// cycle — UNLESS the keeper is peak-exhausted, in which case the needs floor
	// wins and it closes early. Set by sim.StayOpen, read by shiftDutyTarget;
	// mirrored onto ActorSnapshot.OpenUntil for buildDutySteer.
	//
	// TRANSIENT — deliberately NOT checkpointed (no repo round-trip), unlike
	// BreakUntil. Restart-loss is benign: a lost commitment just reverts the
	// keeper to the default close-on-schedule (the safe direction), self-heals via
	// the level-triggered shift producer, and couples with no persistent write
	// (open/closed is presence-derived in occupancy.go, not a stored flag).
	// Contrast BreakUntil, which IS checkpointed because it gates an in-flight
	// needs-recovery process whose interruption is a real regression (WORK-410).
	OpenUntil *time.Time

	// LastTirednessRecoveryAt is the cursor the tiredness-recovery sweep
	// advances as it credits recovery while BreakUntil/SleepingUntil are
	// open. It doubles as the fractional carry: the sweep advances it by
	// exactly the time represented by whole recovered units, so sub-unit
	// minutes stay in the next pass's window. Cleared the moment the actor
	// stops resting (or its window ends) so a fresh window can't be credited
	// against a stale cursor.
	//
	// TRANSIENT — deliberately not persisted (no repo round-trip), unlike
	// BreakUntil/SleepingUntil which ARE checkpointed. So on a LoadWorld
	// where an actor was mid-sleep, the window is restored but the cursor is
	// nil and re-inits to "now" — forfeiting all recovery accrued since
	// bed-down, which can be many units, not just a sub-unit fraction. That
	// loss is bounded and practically nil: a full night over-recovers past
	// NeedMax, and HOME-282 wakes NPCs on shift-start regardless of
	// tiredness, so a restored-mid-sleep NPC still wakes fully rested.
	// Re-persisting the cursor would reintroduce a durable cadence field we
	// deliberately avoid (Postgres is durable storage, not a cadence store).
	LastTirednessRecoveryAt *time.Time

	// ColdCarryX100 is the sub-unit remainder of the cold exposure sweep
	// (cold.go, LLM-412) — the ×100 fraction of a cold unit accrued/recovered
	// but not yet applied to Needs["cold"]. The integer twin of the
	// LastTirednessRecoveryAt fractional carry.
	//
	// TRANSIENT — not persisted (like LastTirednessRecoveryAt): losing a
	// fraction of one cold unit on restart is nothing. Rides CloneActor's
	// value copy.
	ColdCarryX100 int

	// LastPCInputAt is the wall-clock instant of this PC's last deliberate
	// action (move / speak / pay), stamped by touchPCInput. It drives two PC
	// sleep behaviors (pc_sleep.go): the idle-auto-bed sweep beds a lodger PC
	// once this is older than the idle threshold, and any action that stamps it
	// also input-wakes a sleeping PC. nil for NPCs and for a PC that hasn't
	// acted since load. v1 parity: actor.last_pc_input_at.
	//
	// TRANSIENT — not persisted (like LastTirednessRecoveryAt). On a LoadWorld
	// a restored PC starts with a nil cursor, so the idle sweep won't bed them
	// until they next act; harmless (an idle-but-never-acted PC simply isn't
	// auto-bedded until its first stamped action) and keeps cadence state out
	// of durable storage.
	LastPCInputAt *time.Time

	// LastPCSeenAt is the wall-clock instant this PC was last seen present. The
	// server re-stamps it every PCPresenceHeartbeatInterval for as long as the
	// player's WebSocket is connected (LLM-342, RunPCPresenceHeartbeat), and
	// deliberate PC actions (TouchPCInput) stamp it too. A fresh LastPCSeenAt means
	// "a live client is driving this PC"; once the socket drops (closed tab, sleep,
	// network loss) the stamps stop and it goes stale. Originally fed by the /pc/me
	// poll (ZBBS-WORK-326), but that rode the render loop and died with a hidden
	// tab. The presence sweep (pc_presence.go) ejects a stale PC from its huddle,
	// and the encounter cascades skip stale PCs, so co-located NPCs stop burning
	// ticks greeting an absent player (v1 parity: the last_pc_seen_at
	// presence-cleanup sweep; the prod ghost-PC cost bug). nil for NPCs and for a PC
	// no client has attached this session.
	//
	// TRANSIENT — not persisted (like LastPCInputAt / LastTirednessRecoveryAt).
	// Presence is live-session state, not durable: a restored PC starts nil (=
	// treated as absent until its client re-attaches), which is correct — after a
	// restart the client must reconnect its WebSocket to be "present". Keeps
	// ephemeral cadence state out of durable storage.
	LastPCSeenAt *time.Time

	// LastPCActivityAt is the wall-clock instant a HUMAN last did something with
	// this PC (LLM-466) — every deliberate action (TouchPCInput), the WS connect
	// (opening a client is an input), and the /pc/attend idle ack. Deliberately
	// NOT the presence heartbeat: LastPCSeenAt answers "is the socket alive",
	// which an abandoned browser tab holds true forever, and eco mode's audience
	// predicate needs "is a human there" instead (pc_idle_audience.go). Distinct
	// from LastPCInputAt, which means "last in-world action" and drives the
	// idle-auto-bed timer — the candle ack proves a watcher without being an act
	// of the character, so it must not defer a lodger's bed-down.
	//
	// TRANSIENT — not persisted, like the two stamps above. A restored PC starts
	// nil (= idle), and its client's reconnect stamps it immediately.
	LastPCActivityAt *time.Time

	// IdlePromptPending is true while this PC's candle prompt is up: the idle
	// sweep has asked whether anyone is still watching and no answer has arrived
	// (LLM-466). Edge-trigger state for the sweep — it exists so a PC that stays
	// idle is prompted once rather than every 15s pass — and it is NOT the eco
	// gate: AudienceActive reads the stamps directly, so the throttle applies
	// whether or not a prompt happens to be pending.
	//
	// TRANSIENT — not persisted. A restart clears it, and the sweep re-raises the
	// prompt on its next pass if the client is still connected and still idle.
	IdlePromptPending bool

	// Tick scheduling.
	LastTickedAt *time.Time

	// Reactor-evaluator state — Phase 2 PR 2. WarrantedSince + WarrantDueAt
	// + Warrants together form the actor's tick-eligibility record:
	//
	//   - WarrantedSince: timestamp the warrant cycle began (earliest stamp
	//     in this cycle). Nil = no pending signal.
	//   - WarrantDueAt: now + jitter, stamped at warrant time. The evaluator
	//     emits ReactorTickDue when now >= WarrantDueAt.
	//   - Warrants: list of signals accumulated during this warrant cycle.
	//     Cleared at evaluator emit time; new stamps during the in-flight
	//     LLM call start a fresh cycle that fires after completion. See
	//     reactor.go for the full design rationale.
	//
	// All three are ephemeral — wiped on LoadWorld so checkpoint reload
	// doesn't wedge actors with stale interface-typed payloads.
	WarrantedSince *time.Time
	WarrantDueAt   *time.Time
	Warrants       []WarrantMeta

	// TickInFlight gates the evaluator from re-emitting an actor whose LLM
	// call is pending. TickAttemptID is the generation that disambiguates
	// stale completions — a late-arriving completion from a timed-out
	// attempt must not clear a newer attempt's in-flight flag.
	//
	// Both wiped on LoadWorld.
	TickInFlight  bool
	TickAttemptID TickAttemptID

	// RecentReactorTicks is the per-actor ring of recent reactor-tick
	// emission timestamps. Drives the per-minute gross gate
	// (MaxReactorTicksPerActorPerMinute). Lazily allocated on first emit.
	RecentReactorTicks *RingBuffer[time.Time]

	// Red-need backstop pacing (ZBBS-HOME-363). Per-actor exponential
	// backoff for the red-need re-warrant sweep
	// (engine/sim/red_need_backstop_commands.go). The hourly needs-tick
	// re-warrant (needs_tick.go, HOME-329 level-trigger) is too slow to
	// re-engage an actor that burned a tick failing to resolve a red need
	// and then went idle — it sits frozen until the next hour boundary or
	// the 30-min idle backstop. The backstop sweep re-warrants such an
	// actor promptly, but a genuinely-unresolvable red need must NOT
	// re-warrant on a tight loop: every warrant is an LLM deliberation, so
	// a stuck actor would burn tokens indefinitely. The cadence therefore
	// backs off exponentially toward the idle-backstop floor and resets to
	// base only when the need actually drops (real progress).
	//
	//   - RedNeedNextWarrantAt: earliest wall-clock the sweep may stamp the
	//     next red-need warrant. Nil = eligible immediately (never paced).
	//   - RedNeedBackoffLevel: escalation level; the delay is
	//     base << level, capped at RedNeedBackstopMaxDelay.
	//   - RedNeedLastKey / RedNeedLastValue: the need + its value recorded
	//     at the last stamp, so the next sweep detects progress (value
	//     dropped → reset to base) vs. stall (unchanged → escalate).
	//
	// All ephemeral — wiped on LoadWorld with the rest of the reactor
	// pacing state, so a fresh-loaded actor starts un-paced.
	RedNeedNextWarrantAt *time.Time
	RedNeedBackoffLevel  int
	RedNeedLastKey       NeedKey
	RedNeedLastValue     int

	// Seek-work backstop pacing (LLM-141) — the broke-worker analog of the
	// RedNeed* fields above. SeekWorkNextWarrantAt is the earliest wall-clock
	// the sweep may stamp the next seek-work warrant (nil = eligible
	// immediately); SeekWorkBackoffLevel is the escalation level (delay is
	// base << level, capped at the 30-min idle-backstop rate). Eligibility is
	// binary (coins == 0), so there is no last-value to track — going
	// ineligible clears both via clearSeekWorkBackstop. Ephemeral: reset on load.
	SeekWorkNextWarrantAt *time.Time
	SeekWorkBackoffLevel  int

	// Return-to-post backstop pacing (LLM-268) — the off-post-laboring analog of
	// the SeekWork* fields above. ReturnToPostNextWarrantAt is the earliest
	// wall-clock the sweep may stamp the next return-to-post warrant (nil =
	// eligible immediately); ReturnToPostBackoffLevel is the escalation level
	// (delay is base << level, capped at the idle-backstop rate). Eligibility is
	// binary (off-post while the employer still holds the post), so there is no
	// last-value to track — going ineligible (back at the post, job ended) clears
	// both via clearReturnToPostBackstop. Ephemeral: reset on load.
	ReturnToPostNextWarrantAt *time.Time
	ReturnToPostBackoffLevel  int

	// Hired-repair backstop pacing (LLM-280) — the on-post analog of the
	// ReturnToPost* fields above, re-waking a laboring hired worker to mend her
	// employer's still-worn business after she declined the one-shot wake.
	// HiredRepairNextWarrantAt is the earliest wall-clock the sweep may re-stamp the
	// hired-repair warrant (nil = eligible immediately); HiredRepairBackoffLevel is
	// the escalation level (delay is base << level, capped at the idle-backstop
	// rate). Eligibility is binary (on-post at a worn business with nails), so there
	// is no last-value to track — going ineligible (mended, job ended, off-post, out
	// of nails) clears both via clearHiredRepairBackstop. Ephemeral: reset on load.
	HiredRepairNextWarrantAt *time.Time
	HiredRepairBackoffLevel  int

	// Degeneracy observer (LLM-94). Per-actor tracking of consecutive
	// "zero-yield" reactor ticks — substantive (LLM-deliberated) ticks that
	// accomplished nothing (a present scene baseline showing no change, no
	// successful world-mutating commit, and either every tool rejected or no
	// audience to perceive the act). A sustained streak of obviously-futile
	// ticks is the signal that an agent is stuck in a loop (the live Prudence
	// shop-bounce); the observer surfaces it via telemetry and, in later
	// stages, damps the waste. See engine/sim/degeneracy.go.
	//
	//   - DegenStreak: consecutive obviously-futile scored ticks. 0 = none.
	//   - DegenStreakSince: wall-clock start of the current streak; nil = none.
	//   - DegenStage: escalation level (none / flagged / throttled).
	//   - DegenVisits: the oscillation window (LLM-124) — the last few scored
	//     ticks' post-tick structure + red-need snapshot, oldest first, capped
	//     at DegeneracyOscillationWindow. Feeds the structure-oscillation arm so
	//     an actor shuttling between a tight set of structures with no goal
	//     progress is caught even though each move_to leg counts as productive.
	//
	// All ephemeral — wiped on LoadWorld with the rest of the reactor pacing
	// state, so a fresh-loaded actor starts unflagged.
	DegenStreak      int
	DegenStreakSince *time.Time
	DegenStage       DegeneracyStage
	DegenVisits      []DegenVisit

	// StaleWake is the per-ambient-warrant-kind staleness-decay ledger
	// (LLM-233, engine/sim/stale_wake.go): for each kind, the situation
	// fingerprint at its last emitted tick, the consecutive same-fingerprint
	// emit streak, and the last emit time. The reactor defers an all-ambient
	// cycle whose every kind has already been paid for under the current
	// fingerprint. Ephemeral — wiped on LoadWorld like the pacing state
	// above; nil until the first ambient emit.
	StaleWake map[WarrantKind]*StaleWakeEntry

	// inFlightSourceKeys is the set of WarrantSourceKeys consumed into the
	// actor's current in-flight tick attempt — recorded at ReactorTickDue
	// emit, consulted by tryStampWarrant's in-flight dedup path, and
	// resolved by CompleteReactorTick's terminal-status policy. nil when no
	// tick is in flight. Unexported — internal dedup bookkeeping, not part
	// of the observable reactor contract. Ephemeral: wiped on LoadWorld.
	inFlightSourceKeys map[WarrantSourceKey]struct{}

	// recentlyConsumedSourceKeys is the bounded per-actor set of warrant
	// source keys whose tick attempt addressed them — tryStampWarrant's
	// third dedup path, suppressing a delayed duplicate of an already-
	// addressed stimulus. The value is the insertion time, for TTL expiry
	// (recentlyConsumedTTL) and oldest-first eviction (recentlyConsumedCap).
	// Unexported; ephemeral — wiped on LoadWorld.
	recentlyConsumedSourceKeys map[WarrantSourceKey]time.Time

	// awaitingReplyFrom is this actor's turn-state as a SPEAKER: for each
	// peer it has addressed and is awaiting a reply from, the wall-clock
	// time it last addressed them. The single authoritative directed edge
	// — "is it my turn / am I owed a reply" is DERIVED from peers' maps
	// (some peer holds awaitingReplyFrom[me]), never stored separately, so
	// the two views can't drift. Set when this actor speaks to a resolved
	// addressee (sim.Speak / Spoke.AddressedID); cleared when the awaited
	// party speaks (any utterance by them IS the reply) or on huddle
	// leave/conclude. Keyed by addressee ActorID. Drives the ZBBS-WORK-370
	// turn-taking gate: the sim.Speak backstop reads it to reject an idle
	// re-pitch (turn_state.go), and perception renders a turn-line off the
	// snapshot copy. (Supersedes the retired HOME-331 heard-speech miss-counter,
	// ZBBS-WORK-371.) Unexported;
	// ephemeral — wiped on LoadWorld, copied in CloneActor so the published
	// snapshot sees it.
	awaitingReplyFrom map[ActorID]time.Time

	// Locomotion — Phase 2 PR 4.
	//
	// MoveIntent is the actor's in-flight movement state, nil when the
	// actor is not moving. The locomotion ticker re-plans a path against
	// it every tick (it deliberately caches no path — see MoveIntent).
	//
	// MoveAttemptCounter is the per-actor monotonic generation:
	// incremented on every accepted MoveActor command and stamped as the
	// new MoveIntent.AttemptID, so async subscribers can tell a
	// superseded attempt's events from the current one.
	//
	// The counter is checkpointed (it must stay monotonic across
	// restarts). MoveIntent itself is NOT — what the checkpoint carries
	// is the intent's DESTINATION (actor.move_destination, derived from
	// the live MoveIntent at every checkpoint write), which comes back as
	// ResumeDestination below and is re-dispatched through MoveActor at
	// boot. ZBBS-HOME-449: without that, a deploy restart stranded any
	// mid-walk actor wherever the final checkpoint caught them.
	MoveIntent         *MoveIntent
	MoveAttemptCounter MovementAttemptID

	// ResumeDestination is the checkpointed destination of a walk the
	// PREVIOUS process had in flight at shutdown (ZBBS-HOME-449).
	// Load-only: pg LoadAll populates it from actor.move_destination; the
	// boot resume sweep (ResumeCheckpointedWalks) re-dispatches it through
	// the normal MoveActor — path re-planned from the checkpointed tile,
	// arrival warrant fires as usual — and clears it. Never written back:
	// checkpoint writes derive move_destination from the live MoveIntent,
	// so a walk that ends normally clears its column on the next write.
	ResumeDestination *MoveDestination

	// lastStrandedWarrantAt rate-limits the anomalous-position backstop
	// (ZBBS-HOME-450): the idle-backstop sweep stamps at most one
	// StrandedWarrantReason per strandedWarrantCooldown on a still-
	// stranded actor, so an actor that deliberates and CHOOSES to stand
	// in the open doesn't burn an LLM call every sweep. In-memory,
	// restart-lossy on purpose — the first post-boot sweep re-fires for a
	// still-stranded actor, which doubles as boot recovery.
	lastStrandedWarrantAt time.Time

	// Relationships (per-actor views, not a global graph).
	Acquaintances map[string]Acquaintance
	Relationships map[ActorID]*Relationship
	Narrative     *NarrativeState

	// Rumors is this actor's known-set of village rumors (LLM-387): fallible
	// social beliefs it carries about OTHER actors, seeded by witnessing a
	// coin-short settlement and spread + escalated through conversation. Capped
	// (MaxKnownRumors) and TTL'd (RumorTTL); see rumor.go. Distinct from
	// Relationships (faithful per-pair interaction memory) — a rumor may be
	// false and never re-syncs from world state.
	Rumors []KnownRumor

	// Behavior history — load-bearing for diff-against-previous and loop
	// detection. RecentActions and LastSnapshot are in-memory only (not
	// checkpointed); post-restart blind spot for the first few ticks is
	// acceptable.
	RecentActions *RingBuffer[Action]
	LastSnapshot  *ActorSnapshot

	// Macro-state — soft transitions, engine sets on observation (no strict
	// FSM validation). State is checkpointed so restart resumes in the same
	// state.
	State ActorState

	DwellCredits map[DwellCreditKey]*DwellCredit

	// Observed is this actor's decaying, in-memory experiential memory of volatile
	// place conditions — businesses found shut (ObservedClosed, HOME-353) and
	// (vendor, item) pairs found out of stock (ObservedOutOfStock, HOME-363) —
	// folded into one store keyed by (structure, item, condition). Perception
	// deprioritizes a cue pointing at a remembered-shut/dry place; the memory
	// self-clears when the place is re-observed otherwise; each condition DECAYS
	// after its TTL so the NPC retries rather than believing it shut/dry forever.
	// Experiential (learned only by going / trying, never map-wide omniscience)
	// and restart-lossy by design — contrast KnownPlaces below, which is durable
	// positive knowledge. Written by the capture subscribers in closed_business.go
	// / out_of_stock.go; the zero value is an empty store. LLM-80 (epic LLM-76).
	// See observed_state.go.
	Observed ObservedStates

	// KnownPlaces is this actor's DURABLE world-memory: the places/sources it
	// knows and what each is good for (its Affordances). Unlike the decaying,
	// in-memory Observed store above (negative "found it shut/dry just now"
	// observations), a known place is PERMANENT positive knowledge — a location
	// doesn't move, you don't un-know your own farm — and
	// is checkpointed to actor_known_place (same durability tier as
	// salient_facts). Populated on affordance-bearing experience
	// (gather/purchase/consume-at-source) by the known_place.go capture path, and
	// seeded a-priori for owned sources + home/work anchors at LoadWorld.
	// nil/empty when the actor knows no places yet — a loaded actor carries an
	// empty (non-nil) map like the other child collections. LLM-77 (epic
	// LLM-76); ships inert — no resolver/cue reads it yet (LLM-78/79).
	KnownPlaces map[PlaceRef]*KnownPlace

	// RestockPolicy carries this actor's produce/buy entries, unioned
	// across their role attributes (tavernkeeper + worker, etc.). Read
	// from actor_attribute.params.restock in legacy; nil for actors
	// without a restock-bearing attribute.
	RestockPolicy *RestockPolicy

	// GatherTargetObjectID is the village object an agent NPC deliberately
	// walked to (ActorArrived.DestObjectID, stamped by handleGatherTargetOnArrival),
	// so a later gather / StartHarvest prefers THAT bush over the nearest one.
	// The fix for a dense interleaved plot where nearest-wins resolution handed
	// her a depleted or wrong-item bush (LLM-93). An arrival at a structure or a
	// bare position carries an empty DestObjectID, which clears a stale bush
	// target, and committing ANY move clears it too (LLM-618) — so it means
	// exactly "the source I walked to, until I walk somewhere else". Transient
	// (not checkpointed); validity (in reach + stocked) is re-checked at gather
	// time, so a lingering id is harmless.
	GatherTargetObjectID VillageObjectID

	// ProductionActivity is the actor's ONE in-flight production cycle
	// (LLM-319): a `produce` call consumed the recipe inputs and opened this
	// window; the batch lands when the remaining work reaches zero. Progress
	// accrues only while the actor is at its work structure and awake (the
	// produce tick decrements it), so walking off pauses the batch rather than
	// cancelling it. nil = nothing in the works — the idle state the trade cue
	// and the production-choice warrant key off.
	//
	// CHECKPOINTED (unlike SourceActivity, whose windows are seconds): a cycle
	// runs tens of minutes and the inputs are already consumed, so losing the
	// window on restart would eat the inputs — the exact coin bleed LLM-319
	// exists to stop. LastProgressAt is the transient exception (see the field).
	ProductionActivity *ProductionActivity

	// ProductionNagAt is when the production-choice warrant last woke this
	// actor to decide (or a batch last landed — the completion beat is itself
	// the wake). EvaluateProductionChoice re-nags an idle producer only after
	// ProductionRenagInterval, so declining to produce is a decision that
	// sticks, not one re-litigated every scan (LLM-319). Transient — a restart
	// re-nags once, which is harmless.
	ProductionNagAt time.Time

	// RecentProduce is a restart-lossy ring of this actor's ACTUAL production
	// mints (LLM-116) — what it has forged recently, not at-cap anchor advances —
	// windowed-counted into the forge-choice cue's "made N this past week"
	// readout. Newest last; capped at RecentProduceCapacity. Not checkpointed
	// (transient decision-support, same posture as the price book).
	RecentProduce []ProduceEvent

	// RoomAccess — this actor's grants to enter private/staff rooms.
	// Keyed by (RoomID, Source). Stamped by AssignBedroomForLodger
	// (source=ledger) and flipped to Active=false by ExpireRoomAccess
	// when ExpiresAt passes.
	RoomAccess map[RoomAccessKey]*RoomAccess

	// Free-form behavior specs (typed lazily per subsystem during port).
	Attributes map[string][]byte

	// Summon errand perception cues (ZBBS-HOME-311, fade rules reworked
	// LLM-414). Both transient, in-memory only (restart-lossy like the
	// errand machine itself):
	//
	//   - PendingSummon is set on the TARGET when a messenger delivers a
	//     summons ("come to <place>"), driving them to move_to the meet
	//     place. Non-nil drives the "## You have been summoned" perception
	//     section AND suppresses the competing movement steers (go-home /
	//     evening leisure / walk-away errands) so the summons is the single
	//     actionable voice. Cleared by the meeting itself, a take_break, or
	//     the errand's TTL — NOT by the target's speech or walk (see
	//     handleSummonResponseFade).
	//   - SummonRefusal is set on the SUMMONER when their messenger returns
	//     unable to find the target. Non-nil drives the "## Your messenger
	//     returned" perception section; fades on any move/speak/break.
	//
	// Deep-cloned by CloneActor + mirrored into ActorSnapshot so perception
	// (which runs purely off the snapshot) can read them.
	PendingSummon *PendingSummon
	SummonRefusal *SummonRefusal
}

// PendingSummon is the target-side perception cue: a messenger delivered a
// summons asking this actor to come to a place. Value-cloned (no inner
// pointers).
type PendingSummon struct {
	// ErrandID ties the cue to the errand that stamped it, so the errand's
	// terminal paths clear exactly their own cue (a re-summoned target's
	// fresh cue survives an old errand's expiry). Never rendered.
	ErrandID     ErrandID
	SummonerName string
	// Place / PlaceStructureID name the meet place — the summoner's own
	// place at delivery time. The structure id is the move_to token the cue
	// renders inline (the incident target had only a bare label it could
	// not resolve from across the village).
	Place            string
	PlaceStructureID StructureID
	Reason           string // "" when the summoner gave none
	At               time.Time
}

// SummonRefusal is the summoner-side perception cue: the messenger returned
// unable to locate the target. Value-cloned (no inner pointers).
type SummonRefusal struct {
	TargetName string
	At         time.Time
}

// clonePendingSummon / cloneSummonRefusal deep-copy the cue structs. They
// carry no inner pointers, so a dereference suffices; named helpers keep the
// clone idiom uniform with the other CloneActor helpers.
func clonePendingSummon(p *PendingSummon) *PendingSummon {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func cloneSummonRefusal(r *SummonRefusal) *SummonRefusal {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

// CloneActor returns a deep copy of an Actor suitable for the mem-repo
// serialization boundary. Mutated containers (Needs, Inventory,
// DwellCredits, RoomAccess, Acquaintances, Relationships)
// and pointer fields commands rebind (BreakUntil, SleepingUntil,
// LaboringUntil, LastTickedAt, Narrative) are cloned.
// Attributes is
// deep-cloned including each []byte payload. RecentActions is cloned
// via RingBuffer.Clone. MoveIntent is deep-cloned via
// cloneMoveIntent (its MoveDestination carries StructureID / Position
// pointer fields that would otherwise alias across the boundary).
//
// Aliased today (NOT cloned) because no current command mutates them:
//   - LastSnapshot — placeholder/empty struct
//
// TODO: clone RestockPolicy when a command starts mutating it. Read-only
// post-load today but future admin edits could mutate it via a command;
// aliasing now is correct but fragile against future command authors.
//
// Used by mem.ActorsRepo.Seed / LoadAll / SaveSnapshot to enforce that a
// round-trip through the repo breaks pointer identity, the way the pg
// impl will at cutover.
func CloneActor(a *Actor) *Actor {
	if a == nil {
		return nil
	}
	cp := *a

	// `cp := *a` copied these two *int by VALUE, leaving the clone pointing at
	// the live actor's ints — a hole in the deep-clone contract that
	// CloneActorSnapshot never had. It went unnoticed while every reader treated
	// the snapshot as read-only, but the checkpoint clamp pass (LLM-392) writes
	// through such a pointer to project an out-of-range minute back into
	// [0,1439], and it runs OFF the world goroutine: without this, that write
	// would land in the live actor while the world was reading him.
	cp.ScheduleStartMin = copyIntPtr(a.ScheduleStartMin)
	cp.ScheduleEndMin = copyIntPtr(a.ScheduleEndMin)

	if a.Needs != nil {
		cp.Needs = make(map[NeedKey]int, len(a.Needs))
		for k, v := range a.Needs {
			cp.Needs[k] = v
		}
	}
	if a.Inventory != nil {
		cp.Inventory = make(map[ItemKind]int, len(a.Inventory))
		for k, v := range a.Inventory {
			cp.Inventory[k] = v
		}
	}
	if a.ToolWear != nil {
		cp.ToolWear = make(map[ItemKind]int, len(a.ToolWear))
		for k, v := range a.ToolWear {
			cp.ToolWear[k] = v
		}
	}
	if a.GarmentWear != nil {
		cp.GarmentWear = make(map[ItemKind]int, len(a.GarmentWear))
		for k, v := range a.GarmentWear {
			cp.GarmentWear[k] = v
		}
	}
	if a.BreakUntil != nil {
		t := *a.BreakUntil
		cp.BreakUntil = &t
	}
	if a.SleepingUntil != nil {
		t := *a.SleepingUntil
		cp.SleepingUntil = &t
	}
	if a.LaboringUntil != nil {
		t := *a.LaboringUntil
		cp.LaboringUntil = &t
	}
	if a.SourceActivity != nil {
		// Value struct with no nested pointers — a shallow copy breaks aliasing.
		sa := *a.SourceActivity
		cp.SourceActivity = &sa
	}
	if a.OpenUntil != nil {
		t := *a.OpenUntil
		cp.OpenUntil = &t
	}
	if a.LastTirednessRecoveryAt != nil {
		t := *a.LastTirednessRecoveryAt
		cp.LastTirednessRecoveryAt = &t
	}
	if a.LastPCInputAt != nil {
		t := *a.LastPCInputAt
		cp.LastPCInputAt = &t
	}
	if a.LastPCSeenAt != nil {
		t := *a.LastPCSeenAt
		cp.LastPCSeenAt = &t
	}
	if a.LastPCActivityAt != nil {
		t := *a.LastPCActivityAt
		cp.LastPCActivityAt = &t
	}
	if a.LastTickedAt != nil {
		t := *a.LastTickedAt
		cp.LastTickedAt = &t
	}
	if a.WarrantedSince != nil {
		t := *a.WarrantedSince
		cp.WarrantedSince = &t
	}
	if a.WarrantDueAt != nil {
		t := *a.WarrantDueAt
		cp.WarrantDueAt = &t
	}
	if a.Warrants != nil {
		// WarrantMeta is a value type whose Reason field holds an interface
		// over concrete value structs (BasicWarrantReason, PCSpeechWarrantReason,
		// NPCSpeechWarrantReason).
		// Slice copy is safe — appending to one side won't reflect in the
		// other, and the concrete reason structs have no inner pointers
		// today. If a future WarrantReason adds inner pointers, deep-clone
		// it here.
		cp.Warrants = append([]WarrantMeta(nil), a.Warrants...)
	}
	if a.RecentReactorTicks != nil {
		cp.RecentReactorTicks = a.RecentReactorTicks.Clone()
	}
	if a.DegenStreakSince != nil {
		t := *a.DegenStreakSince
		cp.DegenStreakSince = &t
	}
	if a.DegenVisits != nil {
		cp.DegenVisits = append([]DegenVisit(nil), a.DegenVisits...)
	}
	if a.StaleWake != nil {
		cp.StaleWake = make(map[WarrantKind]*StaleWakeEntry, len(a.StaleWake))
		for k, v := range a.StaleWake {
			e := *v
			cp.StaleWake[k] = &e
		}
	}
	if a.inFlightSourceKeys != nil {
		cp.inFlightSourceKeys = make(map[WarrantSourceKey]struct{}, len(a.inFlightSourceKeys))
		for k := range a.inFlightSourceKeys {
			cp.inFlightSourceKeys[k] = struct{}{}
		}
	}
	if a.recentlyConsumedSourceKeys != nil {
		cp.recentlyConsumedSourceKeys = make(map[WarrantSourceKey]time.Time, len(a.recentlyConsumedSourceKeys))
		for k, v := range a.recentlyConsumedSourceKeys {
			cp.recentlyConsumedSourceKeys[k] = v
		}
	}
	if a.awaitingReplyFrom != nil {
		cp.awaitingReplyFrom = make(map[ActorID]time.Time, len(a.awaitingReplyFrom))
		for k, v := range a.awaitingReplyFrom {
			cp.awaitingReplyFrom[k] = v
		}
	}
	if a.Acquaintances != nil {
		cp.Acquaintances = cloneAcquaintances(a.Acquaintances)
	}
	if a.Relationships != nil {
		cp.Relationships = cloneRelationships(a.Relationships)
	}
	if a.Narrative != nil {
		cp.Narrative = cloneNarrativeState(a.Narrative)
	}
	if a.VisitorState != nil {
		cp.VisitorState = cloneVisitorState(a.VisitorState)
	}
	if a.BusinessownerState != nil {
		cp.BusinessownerState = cloneBusinessownerState(a.BusinessownerState)
	}
	if a.RecentActions != nil {
		cp.RecentActions = a.RecentActions.Clone()
	}
	if a.DwellCredits != nil {
		cp.DwellCredits = cloneDwellCredits(a.DwellCredits)
	}
	// cp := *a aliased the backing map; Clone breaks the alias (cheap no-op when
	// the store is empty).
	cp.Observed = a.Observed.Clone()
	if a.KnownPlaces != nil {
		cp.KnownPlaces = cloneKnownPlaces(a.KnownPlaces)
	}
	if a.ProductionActivity != nil {
		// Value struct with no nested pointers — a shallow copy breaks aliasing.
		pa := *a.ProductionActivity
		cp.ProductionActivity = &pa
	}
	if a.RecentProduce != nil {
		cp.RecentProduce = append([]ProduceEvent(nil), a.RecentProduce...)
	}
	if a.Rumors != nil {
		// KnownRumor is a pure value type — a slice copy fully breaks the alias
		// cp := *a left in place (same posture as RecentProduce above). LLM-387.
		cp.Rumors = append([]KnownRumor(nil), a.Rumors...)
	}
	cp.RoomAccess = cloneRoomAccess(a.RoomAccess)
	if a.Attributes != nil {
		cp.Attributes = make(map[string][]byte, len(a.Attributes))
		for k, v := range a.Attributes {
			cp.Attributes[k] = append([]byte(nil), v...)
		}
	}
	if a.MoveIntent != nil {
		cp.MoveIntent = cloneMoveIntent(a.MoveIntent)
	}
	if a.ResumeDestination != nil {
		dest := cloneMoveDestination(*a.ResumeDestination)
		cp.ResumeDestination = &dest
	}
	if a.PendingSummon != nil {
		cp.PendingSummon = clonePendingSummon(a.PendingSummon)
	}
	if a.SummonRefusal != nil {
		cp.SummonRefusal = cloneSummonRefusal(a.SummonRefusal)
	}
	return &cp
}

// ActorSnapshot is the slim immutable view of an actor's decision-relevant
// state at the moment of the last tick. Consumed by:
//   - Snapshot publishing (admin reads, perception diff against previous)
//   - Checkpoint writes (serialized to actor_snapshot row)
//   - Scene origin capture (Scene.ParticipantStateAtOrigin) for diff-against-
//     scene-start in perception build
//
// MoveIntent is deliberately NOT part of this slim view. In-flight
// movement state crosses the mem-repo / checkpoint boundary on the full
// Actor (via CloneActor); a consumer that needs it reads the Actor, not
// the snapshot.
type ActorSnapshot struct {
	AtTick      uint64
	DisplayName string
	Kind        ActorKind
	State       ActorState // checkpointed; restart resumes in same state
	Role        string

	// LLMAgent mirrors the live Actor's LLM-agent slug (VA backing this
	// actor in llm-memory-api). Off-world consumers — notably the
	// reactor-tick harness — read this to populate llm.Request.Model when
	// calling Complete. Empty for actors with no VA backing (PCs, purely
	// decorative NPCs).
	LLMAgent string

	// LoginUsername mirrors the live Actor's PC login (the PC counterpart to
	// LLMAgent — empty for NPCs). Carried so the read surface (httpapi pc/me)
	// can resolve the caller's own PC from the authenticated session by
	// scanning the published snapshot, instead of a command-channel round trip
	// into live world state for a pure read.
	LoginUsername string

	// LastPCSeenAt mirrors the live Actor's presence stamp (nil for NPCs and for a
	// PC no client has attached this session). Carried so read-path
	// consumers can apply the same presence-staleness gate as the sim side
	// (PCPresenceStale) — notably the pc/me indoor co-located roster, which
	// must not advertise a stale (logged-out) PC the speak path's
	// EnsureColocatedHuddle would exclude (ZBBS-HOME-371).
	LastPCSeenAt *time.Time

	InsideStructureID StructureID
	// InsideRoomID mirrors the live Actor's current room (0 when not in a
	// room). Carried so the read surface (httpapi pc/me) can compute the
	// private-room audience scope — the v2 port of v1 actorPrivateRoomScope —
	// purely over the snapshot: look the id up in the actor's
	// InsideStructureID Rooms and scope speech when its Kind is private/staff.
	InsideRoomID    RoomID
	Pos             TilePos // padded grid tile; was CurrentX/CurrentY (see geom.go)
	CurrentHuddleID HuddleID
	// ConversationLooping is set at publish (World.republish) when this actor's
	// current huddle is in an armed conversational loop right now — the same
	// huddleLoopArmed signal the loop sweep uses, surfaced per-tick so perception
	// can steer the actor to act on the agreement instead of re-echoing it
	// (LLM-169), well before the sweep's persistence gate silently concludes the
	// huddle. False for an unhuddled actor or a healthy, advancing conversation.
	ConversationLooping bool
	// ConversationRunLong is ConversationLooping's endurance-arm sibling
	// (LLM-333): the huddle has exhausted its no-progress turn budget
	// (huddleEnduranceArmed) without reading as a lexical or ledger loop.
	// Perception renders a wind-down steer ("this has run its course") rather
	// than the loop arm's "you keep saying the same thing", which would be
	// false of a varied conversation. Mutually exclusive with
	// ConversationLooping at publish — looping wins as the more specific
	// diagnosis.
	ConversationRunLong bool
	// ConversationLingering is the lingering arm's steer (LLM-397): this
	// conversation has simply run longer than HuddleConversationWindDown. It
	// asserts nothing about the talk being stuck — the huddle may be varied,
	// productive, and carrying real memories — only that it has gone on, so
	// perception renders a graceful wind-down rather than the endurance line's
	// "nothing is coming of it" (which on the live case would have been a lie: a
	// sale had just closed). Set at publish only when the two flags above are
	// not, and never for a huddle carrying a live deal.
	ConversationLingering bool
	Needs                 map[NeedKey]int
	InventoryHash         uint64 // fast-compare; computed at snapshot time
	Coins                 int

	// SpriteID + Facing mirror the live Actor's render identity at snapshot
	// time so the client read surface (httpapi) can resolve + inline the
	// sprite without a world-goroutine round trip. Both checkpointed (carried
	// on the full *Actor via CloneActor); these snapshot copies are the
	// read-path view. See Actor.SpriteID / Actor.Facing.
	SpriteID SpriteID
	Facing   string

	// In-flight movement read-path projection (ZBBS-HOME-336). The
	// value-typed destination of the actor's MoveIntent at snapshot time —
	// MoveDestKind is "" when the actor is not moving. This is NOT the live
	// MoveIntent (deliberately excluded, per the doc-comment above); it is the
	// read-path view perception uses to remind the subject of its own
	// in-progress walk ("currently: walking to the Tavern"), the movement
	// analogue of the ActiveDwellCredits cue that keeps an NPC from abandoning
	// an in-progress meal. Resolved to a label in perception.buildActorView
	// against snap.Structures / snap.VillageObjects.
	MoveDestKind        MoveDestinationKind
	MoveDestStructureID StructureID
	MoveDestObjectID    VillageObjectID
	MoveDestPos         TilePos

	// Editor read-path config — mirrors the live Actor's anchors + schedules
	// at snapshot time so the client read surface (httpapi AgentDTO) can show
	// current state without a world-goroutine round trip, the same posture as
	// SpriteID/Facing above. The engine never branches on these snapshot copies
	// — it reads the live Actor; these exist only for the editor/HUD read API.
	// AttributeSlugs is the SORTED set of the actor's attribute keys (the live
	// Actor.Attributes map's keys); the editor renders them as chips and only
	// needs the slugs, so the opaque param payloads are deliberately NOT carried
	// here. HomeStructureID/WorkStructureID are the actor's home/work anchors
	// (empty when unset). ScheduleStartMin/EndMin are the work-shift window
	// (nil = unset → the editor shows "inherit dawn/dusk"); the *int fields are
	// copied into fresh pointers by snapshotActor so the published snapshot
	// never aliases the live Actor's pointers.
	AttributeSlugs   []string
	HomeStructureID  StructureID
	WorkStructureID  StructureID
	ScheduleStartMin *int
	ScheduleEndMin   *int

	// Per-actor knowledge state — read by perception build:
	//   - Acquaintances gates "Around you" name-vs-descriptor rendering
	//     (all NPC kinds — stateful and shared).
	//   - Relationships + Narrative populate the shared-only "Who you
	//     are:" / "What you remember of those here:" sections; nil/empty
	//     for stateful and PC kinds.
	// All three deep-cloned by snapshotActor so the published Snapshot is
	// isolated from world state.
	Acquaintances map[string]Acquaintance
	Relationships map[ActorID]*Relationship
	Narrative     *NarrativeState

	// Rumors mirrors the live Actor's carried rumor known-set at snapshot time
	// (LLM-387) so perception can surface the "## Word about the village" line —
	// the fallible gossip this actor has picked up about ABSENT residents. Unlike
	// Relationships (a faithful dyadic record) a KnownRumor is a deliberately-
	// distortable social belief that decays out of the set; see rumor.go. A pure
	// value slice — KnownRumor holds no maps or pointers — so the plain append in
	// snapshotActor is a full deep copy and the published snapshot never aliases
	// the live Actor's slice. Empty for an actor carrying no rumors and for PC /
	// decorative kinds (which never gossip). See Actor.Rumors.
	Rumors []KnownRumor

	// AwaitingReplyFrom mirrors the live Actor's turn-state edge
	// (Actor.awaitingReplyFrom) at snapshot time: addressee -> when this actor
	// last addressed them and is awaiting a reply. Deep-cloned by snapshotActor so
	// the published snapshot doesn't alias the world's mutable map. Perception
	// build (ZBBS-WORK-370) reads the subject's OWN edges AND its present peers'
	// edges to derive the turn-line ("you spoke to X, wait for their reply" / "X
	// is waiting for your reply") and the act-now coda swap. nil until the actor
	// first addresses someone. Ephemeral on the live Actor (wiped on load); this
	// snapshot copy is the read-path view.
	AwaitingReplyFrom map[ActorID]time.Time

	// ReplyPacingWindow is how long this actor's replies to NPC speech are
	// currently paced by — the resolved LaborReplyCadence while it is mid-job or
	// mid-bake, 0 otherwise (replyPacedCadence). Perception's buildTurnState adds
	// it to the await-reply liveness window whenever THIS actor is the addressee,
	// in both directions: "they are waiting for your reply" survives on a paced
	// worker long enough for it to actually get its turn, and "you spoke to them,
	// wait" survives on the speaker so it waits out the pacing instead of
	// re-pitching into it. The 60s default window is shorter than the 180s
	// cadence, so without this widening an owed reply stops rendering ~2 minutes
	// before the paced worker can speak (LLM-536).
	//
	// A per-publish read projection computed world-side (replyPacedCadence needs
	// WorldSettings, which snapshotActor has no access to) — NOT checkpointed,
	// recomputed each republish, the same posture as ColocatedAudienceIDs below.
	// The hard re-pitch gate in SpeakTo widens by the same value read off live
	// state, so the rendered line and the reject agree.
	ReplyPacingWindow time.Duration

	// ColocatedAudienceIDs are the conversational actors an UNHUDDLED actor would
	// reach if it spoke from its current position — the non-mutating read mirror
	// of the audience the speak path assembles (EnsureColocatedHuddle forms/joins
	// the structure huddle, then buildHuddlePeerSet). Computed world-side by
	// colocatedAudienceIDs at publish time so perception's "## Around you"
	// co-presence line and the speak "there is no one here to hear you" gate
	// derive from ONE scope rule rather than two that can drift (ZBBS-WORK-407).
	// Empty for a huddled actor (its company is the huddle, surfaced via
	// SurroundingsView.HuddleMembers) and for an actor genuinely alone in scope.
	// A derived per-publish read projection — NOT checkpointed (the checkpoint
	// serializes the live *Actor's columns, not this struct), recomputed each
	// republish like the MoveDest* projections above.
	ColocatedAudienceIDs []ActorID

	// ColocatedSleeperIDs are the co-present SLEEPING conversational actors an
	// UNHUDDLED actor can see in its scope — the asleep counterpart to
	// ColocatedAudienceIDs, which omits sleepers. Surfaced so perception's
	// "## Around you" can mark a sleeper "(asleep)" instead of dropping it (a
	// sleeper used to vanish from the speaker's view entirely, who then
	// addressed it expecting a reply — ZBBS-WORK-426, residual of HOME-436).
	// Sleepers stay OUT of ColocatedAudienceIDs, so they are never a speak
	// target and the no-audience gate is unchanged. Same per-publish projection
	// posture as ColocatedAudienceIDs — NOT checkpointed, recomputed each
	// republish.
	ColocatedSleeperIDs []ActorID

	// CurrentLoiterObjectID is the named village object whose loiter pin owns
	// the actor's current tile (resolveLoiteringObject, Chebyshev <=
	// LoiterAttributionTiles), or "" when the actor stands at no pin. It is the
	// co-location signal perception's buildActiveDwellCredits gates on: a
	// DwellCredit renders as an active "you are <verb> at X" self-state line
	// only while its ObjectID matches this — so a credit that lingers in the
	// map after a walk-away (until the next dwell-tick sweep deletes it) stops
	// being asserted as live the instant the actor leaves the pin (LLM-68).
	// Resolved world-side with the SAME resolver/radius the dwell-tick
	// walk-away check uses (actorAtCreditObject), so perception and the engine
	// agree on keep-vs-drop. Stamped only when the actor holds a dwell credit
	// (the sole consumer). Same per-publish projection posture as
	// ColocatedAudienceIDs — NOT checkpointed, recomputed each republish.
	CurrentLoiterObjectID VillageObjectID

	// GatherTargetObjectID mirrors Actor.GatherTargetObjectID onto the published
	// snapshot so the at-bush gather cue (findGatherableCue) can prefer the bush
	// the actor walked to, in lockstep with the gather command (LLM-93). Stamped
	// in snapshotActor from the live actor (not recomputed at republish like
	// CurrentLoiterObjectID).
	GatherTargetObjectID VillageObjectID

	// SourceActivityKind / SourceActivityObjectID / SourceActivityAttribute are
	// the read-path projection of an in-flight timed eat/drink/harvest at a
	// source (Actor.SourceActivity, LLM-54). Kind == "" when the actor is not
	// engaged. Surfaced so perception renders a STANDING "you are picking at the
	// bush — stay put, walking off abandons it" self-state line (the source-
	// activity analogue of the MoveDest* in-progress-walk cue): whatever ticks
	// the actor mid-window — a PC speaking, a red need — it reads its own state
	// and holds rather than re-deciding from scratch (LLM-69). Attribute is the
	// primary need a refresh eases (drives the eat/drink verb); empty for a
	// harvest. ObjectID resolves the source's display label in perception
	// (resolveDwellPinLabel), the same way MoveDest* / dwell pins do. Projected
	// only while the window is live (BusyAtSource) — an expired-but-unswept
	// window, cleared by the next completion sweep, reads as not-engaged. Same
	// per-publish projection posture as ColocatedAudienceIDs — NOT checkpointed,
	// recomputed each republish.
	SourceActivityKind      SourceActivityKind
	SourceActivityObjectID  VillageObjectID
	SourceActivityAttribute NeedKey

	// RouteLabel / RouteStopObjectID are the read-path projection of an in-flight
	// scheduled NPC route the actor is walking (LLM-514) — today only the constable
	// rounds route surfaces them, so perception can render a diegetic "you are
	// walking your rounds" self-state cue that keeps any reactor tick during the
	// tour in character. RouteLabel is the NPCRoute.Label (== AttrConstable);
	// RouteStopObjectID is the current stop's business, set ONLY once he has
	// arrived at it (RouteStopArrived) so the cue can add "you stand before the
	// <business>" at a stop while omitting it mid-walk. Both empty when the actor
	// is not on a rounds route. Projected each republish from w.ActiveRoutes (NOT
	// checkpointed), the same per-publish posture as SourceActivity*.
	RouteLabel        string
	RouteStopObjectID VillageObjectID

	// RouteStopsAhead is how many stops still lie ahead AFTER the one the actor is
	// standing at — set only alongside RouteStopObjectID (i.e. only once arrived),
	// 0 at the final stop. It lets the rounds cue carry the circuit forward
	// ("Seven more places on your round still lie ahead of you"), so an EMPTY stop
	// reads as a waypoint rather than a dead end (LLM-524). Without it the model
	// sees only "you stand before the <shut, empty business>" and reasonably
	// concludes the round is pointless and returns to post, ending the tour at the
	// first quiet shop.
	RouteStopsAhead int

	// RouteNextStopObjectID is the NEXT stop on the circuit after the one the actor
	// is standing at — set alongside RouteStopObjectID, empty at the final stop. The
	// rounds cue NAMES it (LLM-530) so "I am finished here" has somewhere to resolve
	// to. Without a name, the only destination the prompt offered was his own post,
	// so every tour ended there: move_to is how he says "done with this place", and
	// he could only say it about home.
	RouteNextStopObjectID VillageObjectID

	// VisitorState mirrors the live Actor's transient-visitor state at
	// snapshot time. Non-nil marks the actor as a salem-visitor; the
	// perception "Visitors here" block reads Archetype/Origin/Disposition
	// from it. Nil for every non-visitor actor (the steady-state case).
	// Deep-cloned by snapshotActor so published snapshots don't alias the
	// world's mutable visitor record.
	VisitorState *VisitorState

	// Returner projects this traveler's durable returner identity (LLM-372) into
	// the render view — visit count + the players it remembers, most-recent first.
	// Non-nil ONLY for a returner on a repeat visit (VisitCount >= 2); nil for a
	// one-shot stranger or a first-visit traveler, so a freshly-promoted stranger
	// never claims "you've been here before." Built at publish from
	// World.RecurringVisitors (buildReturnerSnapshot), NOT persisted on the actor —
	// the recurring_visitor tables are the source of truth, re-projected each
	// publish so recency stays fresh.
	Returner *ReturnerSnapshot

	// BusinessownerState mirrors the live Actor's businessowner attribute
	// at snapshot time. Non-nil marks the actor as a shopkeeper / innkeeper
	// / smith eligible for engine-authored hospitality speech. Flavor
	// selects the phrase pool (see engine/sim/businessowner.go). Deep-cloned
	// by snapshotActor so published snapshots don't alias the world's
	// mutable record.
	BusinessownerState *BusinessownerState

	// DwellCredits mirror the live Actor's per-pin recovery credits at
	// snapshot time so perception build can surface "you are currently
	// eating stew at the tavern" as part of the actor's self-state. Deep-
	// cloned by snapshotActor so the published Snapshot does not alias
	// the world's mutable credit map.
	DwellCredits map[DwellCreditKey]*DwellCredit

	// Observed mirrors the live Actor's decaying experiential observed-state
	// memory (shut businesses, out-of-stock vendor-items) at snapshot time, so
	// perception can deprioritize a cue pointing at a remembered-shut/dry place.
	// Deep-cloned by snapshotActor so published snapshots don't alias the world's
	// mutable store. The zero value is an empty store. See Actor.Observed and
	// observed_state.go. LLM-80 (epic LLM-76).
	Observed ObservedStates

	// KnownPlaces mirrors the live Actor's durable world-memory at snapshot time
	// so the (future LLM-78/79) move_to resolver + perception cues can read
	// remembered places off the published Snapshot. Deep-cloned by snapshotActor
	// (via cloneKnownPlaces) so published snapshots don't alias the world's
	// mutable map. nil/empty when the actor knows no places. See
	// Actor.KnownPlaces. LLM-77.
	KnownPlaces map[PlaceRef]*KnownPlace

	// RoomAccess mirrors the live Actor's private/staff-room grants at
	// snapshot time so perception build can surface the lodger view ("your
	// room at the inn is paid through <day>") and compute keeper-side room
	// occupancy off the snapshot — both pure over the published Snapshot,
	// never the live Actor. Deep-cloned by snapshotActor (via
	// cloneRoomAccess) so published snapshots don't alias the world's
	// mutable grant map. Keyed by (RoomID, Source) like Actor.RoomAccess.
	RoomAccess map[RoomAccessKey]*RoomAccess

	// OpenUntil mirrors the live Actor's stay-open commitment at snapshot time
	// (ZBBS-WORK-387) so buildDutySteer can suppress the off-shift wind-down cue
	// for a keeper that has committed to staying open late — agreeing with the
	// shiftDutyTarget warrant, which reads the live Actor.OpenUntil. nil when no
	// commitment is held. Carried by CloneActorSnapshot's struct copy like the
	// other *time.Time snapshot fields (the published snapshot is immutable, so
	// no per-clone deep copy is needed). See Actor.OpenUntil.
	OpenUntil *time.Time

	// Inventory mirrors the live Actor's item-kind→quantity map at snapshot
	// time so the read surface (httpapi pc/me) can serve a player's held
	// items without a world-goroutine round trip. InventoryHash above stays
	// the fast-compare digest; this is the full contents. Value-typed map,
	// so snapshotActor copies it with a plain per-entry copy (no pointer
	// cloning needed), the same posture as Needs. Empty/nil for actors with
	// no items.
	Inventory map[ItemKind]int

	// ToolWear mirrors the live Actor's durable-tool wear map (LLM-330) —
	// uses remaining on the in-use unit per tool kind — so the "## Keeping up
	// production" cue can render a tool input's true runway
	// (sim.ToolRunwayUses) without a world-goroutine round trip. Value-typed
	// map copied per-entry by snapshotActor, the same posture as Inventory.
	// Empty/nil for actors that have never worn a tool.
	ToolWear map[ItemKind]int

	// GarmentWear mirrors the live Actor's wearable-garment wear map (LLM-422) —
	// worked minutes remaining on the in-use unit per garment kind — so the cold
	// self-line can judge whether a warms garment is threadbare
	// (actorWarmGarmentTier) without a world-goroutine round trip. Value-typed
	// map copied per-entry by snapshotActor, the same posture as ToolWear.
	// Empty/nil for actors that have never worn a garment.
	GarmentWear map[ItemKind]int

	// RestockPolicy mirrors the live Actor's RestockPolicy at snapshot time so
	// the "## Restocking" perception section can surface a reseller's low
	// `buy` stock + caps without a world-goroutine round trip. ALIASED, not
	// cloned — RestockPolicy is read-only post-load (same posture as
	// CloneActor; see the TODO there) and perception only reads it. nil for
	// actors with no restock-bearing attribute.
	RestockPolicy *RestockPolicy

	// ProductionItem / ProductionBatchQty / ProductionRemainingSeconds mirror
	// the live Actor's in-flight production cycle (LLM-319) so perception can
	// render the "you are making X — about N minutes of work left" standing
	// line, and its absence (Item == "") the idle trade cue, without a
	// world-goroutine round trip. RemainingSeconds is base-rate work left;
	// hired help (LLM-224) shortens the wall time, so the rendered estimate
	// reads "about".
	ProductionItem             ItemKind
	ProductionBatchQty         int
	ProductionRemainingSeconds int64

	// RecentProduce mirrors the live actor's recent-production ring (LLM-116) so
	// the forge-choice cue can read "made N this past week" off the snapshot.
	RecentProduce []ProduceEvent

	// TickInFlight + TickAttemptID mirror the live Actor fields so PR 3d's
	// harness can do a cheap pre-LLM stale-check by reading the snapshot
	// alone (no world-goroutine round trip). A worker that observes its
	// job.attemptID no longer matching the snapshot's TickAttemptID — or
	// observes TickInFlight false — can short-circuit before spending
	// tokens on a tick the world has already moved past.
	//
	// Both fields are ephemeral on the live Actor (cleared on LoadWorld);
	// they appear here only for the snapshot-time view the harness needs.
	TickInFlight  bool
	TickAttemptID TickAttemptID

	// DegenStage is the EFFECTIVE degeneracy-observer stage (LLM-94) at snapshot
	// time, projected by snapshotActor so the two snapshot-only readers of it —
	// perception.Build (Stage-1 steer thinning) and handlers.gateTools (the
	// move_to gate) — can see it without a world-goroutine round trip, the same
	// posture as the movement projection above. "Effective" = forced to
	// DegeneracyNone when the observer is disabled (so disabling lifts Stage-1
	// immediately, not on the actor's next scored tick); otherwise mirrors the
	// live Actor.DegenStage. DegeneracyNone for the overwhelming majority of
	// actors. Ephemeral on the live Actor (reset on LoadWorld); this is the
	// read-path copy. Value type, so CloneActorSnapshot's struct copy carries it.
	DegenStage DegeneracyStage

	// PendingSummon / SummonRefusal mirror the live Actor's summon cues at
	// snapshot time so perception build (which reads only the snapshot) can
	// surface the target-side "you have been summoned" and summoner-side
	// "your messenger returned" sections (ZBBS-HOME-311). Deep-cloned by
	// snapshotActor. nil for the overwhelming majority of actors with no
	// summon in flight.
	PendingSummon *PendingSummon
	SummonRefusal *SummonRefusal
}

// CloneActorSnapshot returns a deep copy of an ActorSnapshot. Needed by
// any aggregate that captures snapshots and then crosses the published-
// Snapshot or mem-repo serialization boundary (notably Scene's
// ParticipantStateAtOrigin map).
func CloneActorSnapshot(s *ActorSnapshot) *ActorSnapshot {
	if s == nil {
		return nil
	}
	cp := *s
	if s.Needs != nil {
		cp.Needs = make(map[NeedKey]int, len(s.Needs))
		for k, v := range s.Needs {
			cp.Needs[k] = v
		}
	}
	if s.Acquaintances != nil {
		cp.Acquaintances = cloneAcquaintances(s.Acquaintances)
	}
	if s.Relationships != nil {
		cp.Relationships = cloneRelationships(s.Relationships)
	}
	if s.Narrative != nil {
		cp.Narrative = cloneNarrativeState(s.Narrative)
	}
	if s.VisitorState != nil {
		cp.VisitorState = cloneVisitorState(s.VisitorState)
	}
	if s.BusinessownerState != nil {
		cp.BusinessownerState = cloneBusinessownerState(s.BusinessownerState)
	}
	if s.DwellCredits != nil {
		cp.DwellCredits = cloneDwellCredits(s.DwellCredits)
	}
	cp.Observed = s.Observed.Clone()
	if s.KnownPlaces != nil {
		cp.KnownPlaces = cloneKnownPlaces(s.KnownPlaces)
	}
	if s.AttributeSlugs != nil {
		cp.AttributeSlugs = append([]string(nil), s.AttributeSlugs...)
	}
	cp.ScheduleStartMin = copyIntPtr(s.ScheduleStartMin)
	cp.ScheduleEndMin = copyIntPtr(s.ScheduleEndMin)
	cp.PendingSummon = clonePendingSummon(s.PendingSummon)
	cp.SummonRefusal = cloneSummonRefusal(s.SummonRefusal)
	return &cp
}

// cloneKnownPlaces deep-copies a KnownPlaces map. The value is a *KnownPlace,
// so the struct is cloned (not aliased) and its Affordances slice is copied —
// mutating the snapshot copy's affordances must not touch the live actor.
// Returns nil for a nil source; skips nil entries. LLM-77.
func cloneKnownPlaces(src map[PlaceRef]*KnownPlace) map[PlaceRef]*KnownPlace {
	if src == nil {
		return nil
	}
	dst := make(map[PlaceRef]*KnownPlace, len(src))
	for k, v := range src {
		if v == nil {
			continue
		}
		vc := *v
		if v.Affordances != nil {
			vc.Affordances = append([]string(nil), v.Affordances...)
		}
		dst[k] = &vc
	}
	return dst
}

// cloneDwellCredits deep-copies a DwellCredits map. RemainingTicks is a
// pointer so it must be cloned separately; the other fields are value
// types and a per-entry struct copy is enough.
func cloneDwellCredits(src map[DwellCreditKey]*DwellCredit) map[DwellCreditKey]*DwellCredit {
	if src == nil {
		return nil
	}
	dst := make(map[DwellCreditKey]*DwellCredit, len(src))
	for k, v := range src {
		if v == nil {
			continue
		}
		vc := *v
		if v.RemainingTicks != nil {
			rt := *v.RemainingTicks
			vc.RemainingTicks = &rt
		}
		dst[k] = &vc
	}
	return dst
}

// cloneRoomAccess deep-copies a RoomAccess map. ExpiresAt is a pointer so
// it must be cloned separately; the other fields are value types and a
// per-entry struct copy is enough. Shared by CloneActor (the repo
// serialization boundary) and snapshotActor (the published read view) so
// neither aliases the world's mutable grant map.
func cloneRoomAccess(src map[RoomAccessKey]*RoomAccess) map[RoomAccessKey]*RoomAccess {
	if src == nil {
		return nil
	}
	dst := make(map[RoomAccessKey]*RoomAccess, len(src))
	for k, v := range src {
		if v == nil {
			continue
		}
		vc := *v
		if v.ExpiresAt != nil {
			t := *v.ExpiresAt
			vc.ExpiresAt = &t
		}
		dst[k] = &vc
	}
	return dst
}
