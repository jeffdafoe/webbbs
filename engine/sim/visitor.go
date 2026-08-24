package sim

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"time"
)

// visitor.go — transient salem-visitor archetype substrate (Phase 3
// Group A). Visitors are shared-VA NPCs that arrive on a random map edge,
// walk to the tavern (or any tagged fallback structure), hang around for
// hours-to-a-day, then walk off another edge. v1 used the chronicler's
// "outside news injection" role for the same purpose; v2 carries it as a
// first-class actor population.
//
// Substrate (this file): pools, sprite map, persona generation, spawn /
// despawn / cleanup Commands, edge-tile + destination pickers. The
// cascade driver (engine/sim/cascade/visitor.go) owns the ticker that
// pumps TickVisitorCascade on a cadence.
//
// Phase 1 scope: spawn / despawn / cleanup framework + perception cue.
// Payloads (news / rumor / letter / goods / quest_hook), recurring-visitor
// returner state — deferred. The feature is gated by default
// (VisitorSpawnChancePermille == 0), so deploying the framework with no
// admin opt-in is a no-op.
//
// Gates and cross-cascade behavior — visitors are KindNPCShared but use
// the VisitorState != nil predicate to skip:
//   - RecordInteraction (relationship_commands.go)
//   - FindConsolidationCandidates / ApplyConsolidation (C1)
//   - FindNarrativeConsolidationCandidates / ApplyNarrativeConsolidation /
//     StampNarrativeConsolidated (C2)
//   - EvaluateIdleBackstop scope
//
// What stays unchanged:
//   - Action-log subscribers (Spoke / Paid / Consumed / Delivered /
//     Walked) fire for visitors naturally — emit, log, atmosphere digest
//     picks them up.
//   - Acquaintance recording (persistent NPCs DO remember meeting a
//     visitor by display name; the entry survives the visitor's removal).
//   - Speech / huddle reactors stamp warrants when a visitor joins a
//     huddle, so visitors react to nearby PC / NPC speech the way any
//     other shared-VA NPC does.
//   - LLM routing: Actor.LLMAgent = VisitorAgentName ("salem-visitor")
//     points the cascade slices that DO fire (warranted reactor ticks at
//     huddle scope) at the shared salem-visitor VA. Per-visitor identity
//     is engine-injected per call from VisitorState; the VA itself stays
//     stateless across visitors (dream_mode='none').
//
// Concurrency: TickVisitorCascade runs on the world goroutine (issued via
// SendContext from the cascade ticker). Spawn / despawn / cleanup are
// three inline phases of a single Command — atomic from the rest of the
// world's perspective, no inter-phase SendContext round-trip.

// Default constants — fall back when WorldSettings.* zero values are
// observed. Tests that bypass the environment loader get these for free.
const (
	// The three per-tick spawn-roll chances (LLM-626), all permille and all
	// defaulting to 0 = that flow disabled — the Phase-1 deploy posture: the
	// framework is no-op until an admin raises the knobs. They replace the
	// single master roll + class-share pair (visitor_spawn_chance_permille /
	// visitor_passer_through_chance_permille), which entangled the corrective
	// merchant flow with the flavor visitor flow: arming the coin valve made
	// the correction rate hostage to overall visitor traffic.
	//
	// DefaultVisitorMerchantTrickleChancePermille — the IN-BAND merchant roll
	// (resident coin inside the valve band, or the band unconfigured): a
	// grounded trader arrives on ordinary business, direction picked by the
	// visitor_sell_weight_permille weighted random. This is what keeps the
	// "cheese-buyer come from Lynn" texture while the valve is quiet.
	DefaultVisitorMerchantTrickleChancePermille = 0

	// DefaultVisitorMerchantCorrectionChancePermille — the OUT-OF-BAND
	// merchant roll: resident coin at/above visitor_coin_band_high forces the
	// arrival to a SELLER (the factor, draining coin), at/below the low band
	// a BUYER (injecting). Sized hotter than the trickle by the operator so an
	// out-of-band state resolves in hours, not days.
	DefaultVisitorMerchantCorrectionChancePermille = 0

	// DefaultVisitorPasserSpawnChancePermille — the independent flavor roll:
	// a passer-through (preacher, musician, scholar, …) arrives on its own
	// cadence, no longer a share carved out of the merchant pipeline.
	DefaultVisitorPasserSpawnChancePermille = 0

	// Default stay-window bounds in minutes. Per-visitor stay is a
	// uniform random pull from [min, max] at spawn time.
	DefaultVisitorMinStayMinutes = 240
	DefaultVisitorMaxStayMinutes = 1440

	// DefaultVisitorMaxConcurrent caps simultaneous visitors. Zero on the
	// settings field falls back to this default — the documented halt-spawn
	// dial is the three spawn-roll chances at 0, not a sentinel here.
	DefaultVisitorMaxConcurrent = 2

	// DefaultVisitorTickInterval is the cadence the cascade driver pumps
	// TickVisitorCascade on when WorldSettings.VisitorTickInterval is
	// zero. 60s matches v1's runServerTickOnce cadence the visitor
	// handlers piggybacked on. Owned by cascade in spirit (the driver
	// reads it); defined here so the substrate's tests can construct a
	// realistic-looking settings block.
	DefaultVisitorTickInterval = 60 * time.Second

	// VisitorCleanupGraceMinutes is the slack past ExpiresAt before a
	// visitor is hard-removed. Lets a despawn walk complete (or fail-
	// and-stall) before the actor row disappears. 5 min covers a
	// cross-village walk at default speed.
	VisitorCleanupGraceMinutes = 5

	// VisitorAgentName is the shared memory-api VA slug every visitor's
	// Actor.LLMAgent points at. Provisioned once at operator setup on
	// memory-api with dream_mode='none' / learning_enabled=false /
	// cache_prompts=false. Per-visitor identity flows from VisitorState
	// + engine-injected perception preface; the VA itself stays stateless
	// across visitors.
	VisitorAgentName = "salem-visitor"

	// VisitorEdgeScanMaxDepth caps how far inward the edge-tile picker
	// scans from each map edge. 30 tiles is roughly 1/6 of map width
	// (200 tiles) — enough slack for villages with a setback approach
	// road, tight enough that "arriving from outside" still reads.
	VisitorEdgeScanMaxDepth = 30

	// surnameScrubMaxTries is the cap on profile re-rolls when scrubbing
	// visitor surnames against seated actors. 5 tries is enough headroom
	// in practice — collision residual rate at ~33% per-roll drops well
	// under 1% after 5 independent rolls.
	surnameScrubMaxTries = 5

	// VisitorPerceptionRadius is a reserved bounding-box (Chebyshev) tile radius
	// for a possible future "a traveler is about across the room" ambient observer
	// line — a wider scan than the observer cue that shipped. LLM-370's cue is
	// co-presence-scoped instead: it names a co-present traveler by archetype +
	// origin in the regular "## Around you" company line, off the same
	// ColocatedAudienceIDs earshot set every other nearby-actor line uses (see
	// perception.travelerCoPresentLabel). Disposition is deliberately NOT surfaced
	// to observers — it colors only the traveler's own self-identity preface. This
	// radius has no consumer yet; 2 tiles ≈ same-tile, adjacent, one-step-away.
	VisitorPerceptionRadius = 2

	// VisitorSpawnEarliestMinute is the earliest minute-of-day a visitor may spawn (LLM-455):
	// 900 = 3 PM, the tavern's open hour, so a merchant's afternoon errand flows into the
	// tavern's evening. Clamped up to dawn if dawn is later. A const, not a knob — it tracks
	// the tavern-open convention; if that schedule changes, adjust here.
	VisitorSpawnEarliestMinute = 900

	// VisitorSpawnDuskMarginMinutes is how long before dusk the spawn window closes (LLM-455),
	// so a freshly-arrived merchant has time to reach his counterparty and trade before the
	// day-shops shut at dusk rather than arriving to a shut village.
	VisitorSpawnDuskMarginMinutes = 90

	// RoadWordLookback bounds how far back selectRoadWord reaches into
	// the action log for a grounded item of news to hand a spawning traveler (LLM-371).
	// The log itself is retention-bounded (DefaultActionLogRetention, 48h); this
	// tighter window keeps the carried word feeling like recent news ("lately",
	// "this week") rather than something stale from two days ago.
	RoadWordLookback = 24 * time.Hour
)

// VisitorTagTavern is the per-instance VillageObject tag the destination
// picker prefers. Tavern is the village's natural gathering point for
// outsiders; falls back to any tagged structure if no tavern is placed.
const VisitorTagTavern = "tavern"

// visitorNamePool — period-flavored full names for spawned visitors.
// Male-coded only because every available sprite family in
// visitorArchetypeSprite is male-coded (Merchant / Old Man / Man) — a
// female-coded name on a male sprite reads as a sprite-asset bug, not a
// stylistic choice. Surnames are chosen to not match Salem's seated
// villagers; the dynamic surname scrub in dispatchVisitorSpawn handles
// drift as new villagers are added or this pool grows.
var visitorNamePool = []string{
	"Master Whitcombe", "Brother Ashford", "Elias Drum",
	"Roger Standish", "Tobias Hewes", "Master Babbage",
	"Jonas Penhallow", "Jeremiah Soames", "Nathaniel Pratt",
	"Caleb Wendell", "Obadiah Brewster", "Ephraim Pollard",
	"Silas Withrow", "Asa Larkin", "Daniel Holcomb",
}

// FactorArchetype is the wholesale-factor persona label (LLM-410, generalized LLM-455):
// the SELL instance of a merchant errand. A factor deals with the village distributor — he
// sells imported cloth / iron / salt into the village and buys its surplus to carry off. It
// is no longer a random pool archetype: a factor now ARISES from a sell trade direction
// (visitorMerchantLabel returns this for a sell errand), so the const survives only as the
// sell-errand label + returner-detection key.
const FactorArchetype = "factor"

// FactorOrigin is where a wholesale factor hails from — the city he trades out of
// (LLM-410). Forced on a sell errand instead of a random next-village pull so the persona
// preface and the keeper's cue read "a factor out of Boston." Boston per the ticket.
const FactorOrigin = "Boston"

// Direction weight (LLM-455). Applied by the settings loaders (repo/pg + repo/mem)
// when the setting row is absent; the use site only clamps to [0,1000], so an explicit 0
// genuinely means "off" (no sellers) — real operator control and a deterministic force for
// tests.
const (
	// DefaultVisitorSellWeightPermille — the TRICKLE (in-band) chance a merchant
	// visitor is a SELLER (the factor) rather than a buyer. Low on purpose: a seller drops a
	// full import shipment and pack-magnitude scaling is deferred, so keeping sellers a
	// minority pins the factor cadence near today's while buyers become the common merchant.
	// Out-of-band arrivals ignore it — the correction roll forces their direction (LLM-626).
	DefaultVisitorSellWeightPermille = 150
)

// passerThroughArchetypePool — the voice-flavor travelers who carry NO trade errand
// (LLM-455): they pass through, socialize, lodge, and leave, but do no commerce. Merchant
// personas are no longer pool entries — a merchant's label is DERIVED from its bound errand
// (visitorMerchantLabel), which keeps the persona from ever naming a trade the village can't
// service. Adding one here requires a passerThroughSprite entry below (init enforces).
var passerThroughArchetypePool = []string{
	"messenger", "itinerant musician", "circuit preacher",
	"traveling scholar", "wandering surgeon",
}

// visitorOriginPool — fictional/historical next-village strings. Drives
// the "from <origin>" prose in the perception cue and the LLM identity
// preface.
var visitorOriginPool = []string{
	"Boston", "Marblehead", "Andover", "Ipswich", "Topsfield",
	"Lynn", "Salem Town", "the next valley over",
	"the coast road", "Beverly", "Wenham", "Rowley",
}

// visitorDispositionPool — short adjectives the model can use to color
// voice on the per-call preface ("you are weary today" / "you are
// curious about Salem").
var visitorDispositionPool = []string{
	"weary", "warm", "reserved", "curious", "mercenary",
	"talkative", "wary", "earnest", "wry", "withdrawn",
}

// passerThroughSprite maps each passer-through archetype to an npc_sprite.name; merchants
// resolve their sprite by CLASS instead (visitorSpriteName), since a merchant's label is a
// derived string, not a fixed pool entry. Spawn / rehydrate resolve the name to the
// uuid-keyed SpriteID via the loaded catalog and stamp it on the Actor, so the client
// renders the traveler instead of drawing nothing (LLM-379). The init() below enforces
// every passerThroughArchetypePool entry has an entry here.
var passerThroughSprite = map[string]string{
	"messenger":          "Man A (v00)",
	"itinerant musician": "Man B (v00)",
	"circuit preacher":   "Old Man B (v00)",
	"traveling scholar":  "Old Man A (v01)",
	"wandering surgeon":  "Old Man A (v02)",
}

// VisitorArchetypeSprite is the passer-through archetype→sprite map, exported so tests
// build a fake sprite catalog from it (the live catalog is uuid-keyed with the display name
// on Sprite.Name). Merchant sprites are NOT keyed here — see visitorSpriteName /
// VisitorSpriteCatalogNames.
var VisitorArchetypeSprite = passerThroughSprite

// passerThroughVocation gives each passer-through archetype its one-sentence vocation —
// what the calling DOES, spoken into the traveler's identity preface (LLM-566). Before
// this the bare two-word label was the only vocational signal on the wire, so every
// passer-through toured as the same genial news-carrier under a different name (the live
// case: a circuit preacher whose whole clergy was "the Lord willing" squeezed from the
// label). Diegetic prose, not an instruction — the sentence describes who the traveler
// is and the model draws the behavior. Merchants get NO entry: their label is derived
// from the bound errand (visitorMerchantLabel) and the errand cue already carries their
// purpose. The init() below enforces every pool entry has a vocation.
var passerThroughVocation = map[string]string{
	"messenger":          "The news is your trade: you carry letters and word for pay, deliver them brisk and exact, and are back on the road as soon as they are passed.",
	"itinerant musician": "You live by your fiddle and your voice — wherever folk gather you look for a corner and an audience, and offer a tune for a meal or a coin.",
	"circuit preacher":   "You carry the Word as well as the news: you bless households, ask after souls, and would not leave a village without a bit of scripture spoken at a hearth or the meeting house.",
	"traveling scholar":  "You travel for learning's sake, hungry for books, letters, and learned talk — you ask more questions than you answer and set down what you hear.",
	"wandering surgeon":  "You mend folk for your bread: you ask after ailments and injuries wherever you call, and offer what remedy your kit and learning allow.",
}

// VisitorVocation returns the archetype's vocation sentence for the identity preface,
// or "" for an archetype without one (merchant-derived labels, unknowns) — the preface
// drops the sentence on empty.
func VisitorVocation(archetype string) string {
	return passerThroughVocation[archetype]
}

// FactorSpriteName is the sprite a wholesale factor (a sell errand) renders with — a
// well-to-do merchant (LLM-410/455).
const FactorSpriteName = "Merchant A (v00)"

// merchantBuyerSpriteNames is the sprite pool a buy-merchant renders with (LLM-455), picked
// DETERMINISTICALLY by the errand good (stableStringIndex) so a mid-visit rehydrate
// reproduces the same sprite without persisting it. The Merchant family reads as a traveling
// trader.
var merchantBuyerSpriteNames = []string{
	"Merchant B (v00)", "Merchant C (v00)", "Merchant A (v01)", "Merchant C (v01)",
}

func init() {
	for _, archetype := range passerThroughArchetypePool {
		if _, ok := passerThroughSprite[archetype]; !ok {
			panic("sim/visitor: passer-through archetype " + archetype + " has no sprite mapping in passerThroughSprite")
		}
		if _, ok := passerThroughVocation[archetype]; !ok {
			panic("sim/visitor: passer-through archetype " + archetype + " has no vocation line in passerThroughVocation")
		}
	}
}

// VisitorSpriteCatalogNames returns every sprite name a visitor may render with — the
// passer-through sprites plus the merchant (factor + buyer) sprites (LLM-455). Tests build a
// fake sprite catalog from it so a spawned merchant resolves a real SpriteID.
func VisitorSpriteCatalogNames() []string {
	names := make([]string, 0, len(passerThroughSprite)+len(merchantBuyerSpriteNames)+1)
	for _, n := range passerThroughSprite {
		names = append(names, n)
	}
	names = append(names, FactorSpriteName)
	names = append(names, merchantBuyerSpriteNames...)
	return names
}

// visitorSpriteName returns the npc_sprite.name a visitor renders with, derived from its
// state (LLM-455): a passer-through by its archetype (passerThroughSprite); a factor (sell
// errand) by FactorSpriteName; a buyer by a deterministic pick from merchantBuyerSpriteNames
// keyed on the errand good so spawn and rehydrate agree without persisting the sprite. ""
// for an unresolvable state (a legacy archetype with no mapping) — the caller ships
// spriteless.
func visitorSpriteName(vs *VisitorState) string {
	if vs == nil {
		return ""
	}
	if vs.Trade != nil {
		if vs.Trade.Direction == TradeDirectionSell {
			return FactorSpriteName
		}
		if len(merchantBuyerSpriteNames) == 0 {
			return ""
		}
		return merchantBuyerSpriteNames[stableStringIndex(string(vs.Trade.Good), len(merchantBuyerSpriteNames))]
	}
	return passerThroughSprite[vs.Archetype]
}

// visitorSpriteID resolves a visitor's sprite NAME (visitorSpriteName) to the uuid-keyed
// SpriteID the client renders by. World.Sprites is keyed by id with the display name on
// Sprite.Name, so the lookup is a name scan of the loaded catalog. Spawn / rehydrate stamp
// the result on the Actor; without it a visitor ships with an empty sprite_id and the client
// draws nothing (LLM-379).
//
// ok=false when the state resolves to no name (a legacy/unmapped archetype) or the named
// sheet isn't in the loaded catalog. The caller logs and ships spriteless rather than
// failing the spawn — an invisible traveler is a lesser fault than a dropped one.
func visitorSpriteID(w *World, vs *VisitorState) (SpriteID, bool) {
	if w == nil {
		return "", false
	}
	name := visitorSpriteName(vs)
	if name == "" {
		return "", false
	}
	for id, sp := range w.Sprites {
		if sp != nil && sp.Name == name {
			return id, true
		}
	}
	return "", false
}

// stableStringIndex maps s to a stable index in [0, n) via an FNV-1a-style byte hash —
// deterministic across processes so a rehydrate reproduces a buyer's sprite without
// persisting it. n must be > 0.
func stableStringIndex(s string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// VisitorCascadeTelemetry captures what each tick did. Used by the
// cascade driver for log lines and load-bearing for the substrate-side
// unit tests. Fields parallel the three inline phases of
// TickVisitorCascade.
//
// SpawnSkipChance currently lumps two cases — "feature disabled by
// config" (chance=0) AND "roll didn't fire on an enabled cascade." Until
// the separate SpawnDisabled counter lands (deferred from R1 review),
// consumers must branch on SpawnSkipReason to disambiguate; a dashboard
// or alert that treats SpawnSkipChance == 1 as "unlucky roll" without
// reading SpawnSkipReason will misreport disabled worlds as low-roll
// luck. See shared/notes/codebase/salem-engine-v2/visitor "Future work."
type VisitorCascadeTelemetry struct {
	DespawnsStarted  int // visitors whose despawn walk was issued this tick
	CleanedUp        int // visitor rows removed past ExpiresAt + grace
	Spawned          int // new visitors created (0 or 1 per tick)
	RoundsPaced      int // stationary travelers woken this tick to reconsider their rounds (LLM-379 pacing)
	CircuitToLodging int // visitors that turned to the lodging phase this tick (dusk)
	SpawnSkipChance  int // 1 if spawn skipped — chance=0 OR unlucky roll; check SpawnSkipReason
	SpawnSkipCap     int // 1 if spawn skipped because MaxConcurrent reached
	SpawnSkipReason  string
}

// VisitorTickInputs carries the per-tick inputs the dispatcher reads.
// Bundled as a struct so callers can construct a deterministic input set
// in tests (overriding Now, RNG, etc.) without piggybacking on world
// state.
//
// Now: wall-clock moment the tick fires. Drives ExpiresAt comparison for
// despawn / cleanup, and stamps spawn-time fields.
//
// Rand: random source. Drives spawn-chance roll, persona pick, edge-tile
// shuffle. Passed in so tests can use a deterministic seed; production
// uses a per-driver rand seeded once at registration.
type VisitorTickInputs struct {
	Now  time.Time
	Rand *rand.Rand
}

// TickVisitorCascade returns a Command that runs the three inline visitor
// phases in order: despawn → cleanup → spawn. Single round-trip per tick
// — all three phases run atomically inside one Command.Fn on the world
// goroutine.
//
// Despawn before cleanup ensures a visitor that just expired this tick
// gets a chance to start its return walk before being eligible for hard
// removal (which only fires after VisitorCleanupGraceMinutes past
// ExpiresAt, so first-tick removal is impossible regardless of ordering
// — the order is for clarity, not correctness).
//
// Spawn last so a freshly-created visitor doesn't get hit by despawn /
// cleanup on the same tick.
//
// Telemetry captures what the tick did; the cascade driver logs it
// when any field is non-zero.
func TickVisitorCascade(inputs VisitorTickInputs) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			t := VisitorCascadeTelemetry{}
			dispatchVisitorDespawn(w, inputs, &t)
			dispatchVisitorCleanup(w, inputs, &t)
			// Deliberately NOT eco-gated (LLM-502, reversing the LLM-313 pause):
			// a traveler stays until next daybreak (LLM-373), so one spawned
			// unwatched is still in the village — lodged at the inn — when a
			// player logs in later. Gating starved the village of visitors
			// entirely (spawn rolls only happened during the rare watched
			// minutes), and gating pacing alone would strand an unwatched
			// arrival mid-arc: never making rounds, never flipping to lodging
			// at dusk. Cost is bounded by VisitorMaxConcurrent and the spawn
			// permille. Pacing before spawn so a freshly-spawned visitor
			// (still walking in from the road) isn't paced on its spawn tick.
			dispatchVisitorPacing(w, inputs, &t)
			dispatchVisitorSpawn(w, inputs, &t)
			return t, nil
		},
	}
}

// dispatchVisitorDespawn finds visitors whose stay window has expired and
// who have not already had a despawn walk issued, then issues a MoveActor
// command targeting a fresh edge tile picked via pickVisitorEdgeTile. Each
// visitor may exit a different edge than they arrived on — narratively
// reads as "wandered off down the road," not "retraced their steps."
//
// VisitorState.Phase == VisitorPhaseDeparting is the one-shot gate. v1 used
// "actor still in a structure" as the despawn-eligibility proxy; v2's substrate
// has the dedicated phase so the gate doesn't entangle with whatever happened
// to the actor's InsideStructureID mid-walk (e.g. a stale back-ref clear).
//
// On any failure (no edge tile, no path) we still set the departing phase so
// the despawn isn't re-attempted every tick — cleanup will collect the
// stranded actor after the grace window regardless.
func dispatchVisitorDespawn(w *World, inputs VisitorTickInputs, t *VisitorCascadeTelemetry) {
	now := inputs.Now
	r := inputsRandOrDefault(inputs.Rand)
	for id, actor := range w.Actors {
		if actor == nil || actor.VisitorState == nil {
			continue
		}
		if actor.VisitorState.Phase == VisitorPhaseDeparting {
			continue
		}
		if !now.After(actor.VisitorState.ExpiresAt) {
			continue
		}
		// Pick a fresh anchor (any visitor destination) to validate the
		// edge tile is connected to the village core. If no destination
		// is placed at all, leave the visitor alone — cleanup will
		// collect them after the grace window.
		_, anchorTile, ok := pickVisitorDestination(w)
		if !ok {
			actor.VisitorState.Phase = VisitorPhaseDeparting
			continue
		}
		grid, err := buildWalkGrid(w)
		if err != nil {
			log.Printf("sim/visitor: dispatchDespawn build walk grid: %v", err)
			actor.VisitorState.Phase = VisitorPhaseDeparting
			continue
		}
		edgeTile, ok := pickVisitorEdgeTile(w, grid, anchorTile, r)
		if !ok {
			actor.VisitorState.Phase = VisitorPhaseDeparting
			continue
		}
		dest := NewPositionDestination(edgeTile)
		// LeaveHuddleFirst=true so a visitor mid-conversation can still
		// be despawn-dispatched (rather than the cascade silently stalling
		// because the visitor is gossiping). MoveActor's huddle-leave
		// emits HuddleLeft / HuddleConcluded events as appropriate.
		if _, err := MoveActor(id, dest, true, now).Fn(w); err != nil {
			// No path is typical for a visitor stranded somewhere
			// unreachable. Cleanup will hard-remove past the grace
			// window regardless.
			log.Printf("sim/visitor: dispatchDespawn MoveActor %s: %v", id, err)
		}
		actor.VisitorState.Phase = VisitorPhaseDeparting
		t.DespawnsStarted++
	}
}

// dispatchVisitorCleanup hard-removes visitor actor rows whose ExpiresAt
// passed more than VisitorCleanupGraceMinutes ago. Position-agnostic — a
// visitor stranded with no walk path still gets cleaned up after the
// grace window so we don't leak rows. Emits ActorDeparted before delete
// so subscribers can capture the departure event.
func dispatchVisitorCleanup(w *World, inputs VisitorTickInputs, t *VisitorCascadeTelemetry) {
	now := inputs.Now
	r := inputsRandOrDefault(inputs.Rand)
	grace := time.Duration(VisitorCleanupGraceMinutes) * time.Minute
	for id, actor := range w.Actors {
		if actor == nil || actor.VisitorState == nil {
			continue
		}
		if !now.After(actor.VisitorState.ExpiresAt.Add(grace)) {
			continue
		}
		// LLM-372: a promoted returner leaving schedules its comeback — stamp
		// last_seen + next_return_at on the durable row before the actor row goes.
		// A one-shot (unpromoted) visitor has no RecurringID and is simply gone.
		if actor.VisitorState.RecurringID != "" {
			w.scheduleReturnerDeparture(RecurringVisitorID(actor.VisitorState.RecurringID),
				now, r, w.Settings.VisitorReturnMinDays, w.Settings.VisitorReturnMaxDays)
		}
		// Capture before-removal state for the event.
		evt := &ActorDeparted{
			ActorID:               id,
			DisplayName:           actor.DisplayName,
			LastInsideStructureID: actor.InsideStructureID,
			LastPosition:          actor.Pos,
			VisitorContext:        cloneVisitorState(actor.VisitorState),
			At:                    now,
		}
		// Emit BEFORE removal so subscribers can still look up the actor
		// in w.Actors mid-event if they need to. The event already carries
		// the load-bearing pre-removal fields directly, but the actor row
		// remains reachable for any subscriber that wants more (e.g. a
		// future debug logger reading Acquaintances).
		w.emit(evt)
		// Remove from secondary indices. setActorInsideStructure handles
		// outdoorActors / actorsByStructure transitions when we drop the
		// actor's inside flag; the actorsByHuddle index doesn't have a
		// removal helper, so detach inline.
		if actor.CurrentHuddleID != "" {
			if members, ok := w.actorsByHuddle[actor.CurrentHuddleID]; ok {
				delete(members, id)
				if len(members) == 0 {
					delete(w.actorsByHuddle, actor.CurrentHuddleID)
				}
			}
		}
		setActorInsideStructure(w, actor, "")
		delete(w.outdoorActors, id)
		delete(w.Actors, id)
		t.CleanedUp++
	}
}

// selectRoadWord picks one grounded item of news for a spawning traveler to
// carry (LLM-371). It draws from the in-memory action log — the same
// recent-happenings ring the atmosphere digest reads — filtered to carry-worthy
// beats within RoadWordLookback whose subject is a real resident (not another
// visitor, not the PC, not decorative), and renders one to a diegetic past-tense
// clause. This is the v2-faithful stand-in for the ticket's "recent
// village_event": engine-v2 has no village_event table, but the action log
// records every actor's real beats (a stateful keeper's delivery / a shared-VA
// vendor's sale alike), so a traveler can carry checkable word about anyone in
// the village. Returns "" when nothing carry-worthy is on hand — the caller
// leaves Payload empty and the preface drops the clause. Random pick (not
// most-recent) so back-to-back spawns don't all echo the same freshest beat.
// Runs on the world goroutine (called from dispatchVisitorSpawn), so reading
// w.ActionLog / w.Actors is race-free.
//
// This is NOT a rumor and must not be confused with one (LLM-597). The rumor
// layer in rumor.go is deliberately FALLIBLE — a frozen claim that escalates a
// rung per retelling and is free to outgrow the fact that seeded it. A
// traveler's word is the opposite: grounded, checkable, never distorted, and
// never escalated. The two systems shared the noun "rumor" until LLM-597, which
// is how LLM-594 came to be filed against the wrong mechanism.
func selectRoadWord(w *World, r *rand.Rand, now time.Time) string {
	if w == nil || len(w.ActionLog) == 0 {
		return ""
	}
	cutoff := now.Add(-RoadWordLookback)
	var candidates []string
	for _, e := range w.ActionLog {
		if e.OccurredAt.Before(cutoff) {
			continue
		}
		subject := w.Actors[e.ActorID]
		if subject == nil || subject.VisitorState != nil {
			continue // subject must be a resident villager, not a passing traveler
		}
		if subject.Kind == KindPC || subject.Kind == KindDecorative {
			continue // word is about the village's own, not the player or props
		}
		if clause := renderRoadWordClause(w, e); clause != "" {
			candidates = append(candidates, clause)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[r.Intn(len(candidates))]
}

// renderRoadWordClause turns one action-log entry into the diegetic, past-tense
// clause a traveler carries as word from the road — "Ezekiel Crane turned out a plow for the
// Hale farm" — or "" for a beat that is not worth carrying. The
// preface owns the "Word reached you on the road that …" framing
// (renderTravelerPreface); this returns just the grounded fact. Deliberately a
// curated allow-set of the socially legible economic beats: the private
// (consumed / took_break), the dull (walked / departed), the utterance itself
// (spoke — long, contextual, and already carried by the speaker's own memory),
// and the feed-only negotiation types (offered / declined / countered, filtered
// everywhere NPC-facing) all render "". Amounts and exact coin counts are dropped
// on purpose — scene, not ledger. The subject name is resolved by the caller's
// guard (w.Actors[e.ActorID] non-nil), re-checked here for safety.
func renderRoadWordClause(w *World, e ActionLogEntry) string {
	subject := w.Actors[e.ActorID]
	if subject == nil || subject.DisplayName == "" {
		return ""
	}
	name := subject.DisplayName
	switch e.ActionType {
	case ActionTypePaid:
		if e.CounterpartyName == "" {
			return "" // a payment to no one named is not worth carrying
		}
		clause := name + " settled up with " + e.CounterpartyName
		if e.Text != "" {
			clause += " over " + e.Text
		}
		return clause
	case ActionTypeDelivered:
		if e.Text == "" {
			return ""
		}
		clause := name + " turned out " + e.Text
		if e.CounterpartyName != "" {
			clause += " for " + e.CounterpartyName
		}
		return clause
	case ActionTypeLabored:
		if e.CounterpartyName != "" {
			return name + " put in a day's work for " + e.CounterpartyName
		}
		return name + " took on a piece of work"
	case ActionTypeHired:
		if e.CounterpartyName == "" {
			return ""
		}
		return name + " took " + e.CounterpartyName + " on for a job"
	case ActionTypeSolicitedWork:
		if e.CounterpartyName != "" {
			return name + " went looking to work for " + e.CounterpartyName
		}
		return name + " was about looking for work"
	case ActionTypeOfferedWork:
		if e.CounterpartyName == "" {
			return ""
		}
		return name + " offered " + e.CounterpartyName + " a piece of work"
	case ActionTypeGathered:
		if e.Text == "" {
			return ""
		}
		clause := name + " was out gathering " + e.Text
		if e.CounterpartyName != "" {
			clause += " at " + WithDefiniteArticle(e.CounterpartyName)
		}
		return clause
	case ActionTypeRepairing:
		if e.Text != "" {
			return name + " was mending " + WithDefiniteArticle(e.Text)
		}
		return name + " was busy at repairs"
	default:
		return ""
	}
}

// dispatchVisitorSpawn makes the per-tick spawn decision (rollVisitorSpawn —
// the LLM-626 two-flow split) and — when a roll fires and the concurrent cap
// isn't reached — generates a persona, picks an arrival edge tile +
// destination, inserts a fresh Actor + VisitorState, seeds need rows, and
// issues a MoveActor targeting the destination.
//
// The off-switch is all three spawn chances at 0 (the deploy default). Other
// failure paths (no destination placed, no walkable edge tile) log + skip the
// cycle without setting any telemetry counters — they're configuration / map
// issues, not skip reasons of architectural interest.
func dispatchVisitorSpawn(w *World, inputs VisitorTickInputs, t *VisitorCascadeTelemetry) {
	// Afternoon spawn window (LLM-455, narrowing the LLM-373 daytime gate): a merchant
	// arrives in the afternoon — late enough that his evening at the tavern overlaps the dusk
	// company, early enough to finish his trade before the day-shops shut at dusk. The window
	// is [max(dawn, VisitorSpawnEarliestMinute=3 PM/tavern-open), dusk − margin]. A world with
	// no usable dawn/dusk clock spawns anytime (fail-open, matching how the perception evening
	// gates degrade on a bad clock).
	if dawn, dusk, ok := worldDawnDuskMinutes(w); ok {
		if !inAfternoonSpawnWindow(dawn, dusk, localMinuteOfDay(w, inputs.Now)) {
			t.SpawnSkipReason = "outside afternoon spawn window"
			return
		}
	}
	// Cap BEFORE the rolls (LLM-626): a corrective roll that fired at cap would
	// otherwise burn its firing on a tick that can't spawn. Zero / unset falls
	// back to default (matches every other settings field in this file); the
	// documented "halt spawn" admin dial is the spawn chances at 0, not
	// VisitorMaxConcurrent — keeping both as halt-spawn signals would give two
	// ways to say the same thing.
	maxConcurrent := w.Settings.VisitorMaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultVisitorMaxConcurrent
	}
	current := 0
	for _, a := range w.Actors {
		if a != nil && a.VisitorState != nil {
			current++
		}
	}
	if current >= maxConcurrent {
		t.SpawnSkipCap = 1
		t.SpawnSkipReason = fmt.Sprintf("at cap %d/%d", current, maxConcurrent)
		return
	}
	r := inputsRandOrDefault(inputs.Rand)
	roll := rollVisitorSpawn(w, r)
	if roll.Class == visitorSpawnNone {
		t.SpawnSkipChance = 1
		t.SpawnSkipReason = roll.Reason
		return
	}

	// Persona FIRST (moved ahead of the spatial picks): the class (merchant vs passer-through)
	// and, for a merchant, the bound errand decide the archetype label, origin, pack, and
	// arrival target (the errand counterparty, not the tavern), so they must be known before
	// we pick a destination. A due returner (LLM-372) comes back as the SAME person — prefer
	// one over a fresh stranger, reusing its established persona verbatim (and skipping the
	// surname scrub, since the name is already in play and unique enough). Only READ the
	// returner here; the durable mutation (beginReturnerVisit — bump visit count, clear
	// next_return_at) is deferred until AFTER the actor is committed below, so a spawn that
	// bails out (edge-tile miss, ID-mint exhaustion) leaves the returner still due to try again
	// rather than consumed-but-not-arrived. Otherwise roll a new persona and scrub its surname
	// against seated villagers.
	//
	// A CORRECTIVE merchant spawn (LLM-626) is never preempted by a returner:
	// the band fired it to move coin, and a returner comes back as a
	// passer-through (errand re-binding is a later refinement), which would
	// silently swallow the correction. The returner stays due and rides the
	// next trickle or flavor spawn instead.
	var returnerID string
	var dueReturner *RecurringVisitor
	var profile visitorProfile
	if rv, ok := w.pickDueReturner(inputs.Now); ok && !(roll.Class == visitorSpawnMerchant && roll.Corrective) {
		profile = visitorProfile{Name: rv.Name, Archetype: rv.Archetype, Origin: rv.Origin, Disposition: rv.Disposition}
		returnerID = string(rv.ID)
		dueReturner = rv
	} else {
		existing := loadActorSurnames(w)
		profile = generateVisitorProfile(r)
		for tries := 0; tries < surnameScrubMaxTries; tries++ {
			if !existing[extractSurname(profile.Name)] {
				break
			}
			profile = generateVisitorProfile(r)
		}
		if existing[extractSurname(profile.Name)] {
			log.Printf("sim/visitor: dispatchSpawn: surname for %q still collides after %d tries; shipping anyway",
				profile.Name, surnameScrubMaxTries)
		}
	}

	// Errand (LLM-455; class decided by rollVisitorSpawn since LLM-626). A merchant spawn is
	// bound to a real errand in the roll's direction unless the economy can't service one (no
	// open keeper for a buyer, no distributor for a seller) — then he arrives a passer-through
	// carrying only voice-flavor, the standing fallback. The errand grounds the persona: the
	// archetype label is DERIVED from the bound good (cheese -> "cheese-buyer") so it can never
	// name an untradeable trade — the root fix for the ungrounded "wool-buyer" loop. A returner
	// keeps its stored persona verbatim and comes back as a passer-through (merchant-returner
	// errand re-binding is a later refinement).
	var trade *TradeErrand
	if dueReturner == nil && roll.Class == visitorSpawnMerchant {
		if bound, ok := bindVisitorErrand(w, r, roll.Direction); ok {
			trade = bound
			profile.Archetype = visitorMerchantLabel(w, trade)
			if trade.Direction == TradeDirectionSell {
				profile.Origin = FactorOrigin // a factor hails from the city he trades out of
			}
		}
	}

	// Arrival target. A merchant makes straight for his errand counterparty — the one must-hit
	// stop (LLM-455) — rather than the neutral tavern anchor; a passer-through (and a merchant
	// whose counterparty is somehow unplaced) falls back to the ordinary tavern/gathering-place
	// picker.
	destID, destAnchor, ok := pickArrivalDestination(w, trade)
	if !ok {
		log.Printf("sim/visitor: dispatchSpawn: no destination structure placed; skipping")
		return
	}
	grid, err := buildWalkGrid(w)
	if err != nil {
		log.Printf("sim/visitor: dispatchSpawn build walk grid: %v", err)
		return
	}
	edgeTile, ok := pickVisitorEdgeTile(w, grid, destAnchor, r)
	if !ok {
		log.Printf("sim/visitor: dispatchSpawn: no valid edge tile this cycle; skipping")
		return
	}

	// Departure is schedule-anchored to the next daybreak (LLM-373), replacing the
	// old random [min,max] stay: a traveler stays the night and leaves at first
	// light. Default one night; a multi-night stay is a later setting. The
	// Foundation despawn/cleanup reconcile is unchanged — it just reads this
	// ExpiresAt. nextDaybreak fails open to a one-day fallback if the world has no
	// usable dawn/dusk clock, so a bad clock can't mint a never-expiring visitor.
	expiresAt := nextDaybreak(w, inputs.Now)

	// Display name uniqueness — "Name the Archetype" with a numeric
	// disambiguator on collision. Collisions with persistent NPCs are
	// unlikely given the period names; the suffix covers same-tick
	// concurrent visitors with the same pull.
	displayName := fmt.Sprintf("%s the %s", profile.Name, profile.Archetype)
	if displayNameInUse(w, displayName) {
		displayName = fmt.Sprintf("%s the %s (%d)", profile.Name, profile.Archetype, inputs.Now.Unix()%10000)
	}

	// Mint actor ID with collision retry. 8 hex chars = 32 bits of
	// entropy — collision is astronomically unlikely at Salem scale but
	// not impossible, and a collision means silently replacing an
	// existing actor row. The retry loop checks against w.Actors and
	// caps at 10 attempts; on exhaustion (genuinely shouldn't happen),
	// log + skip this spawn.
	id := ActorID("")
	for attempt := 0; attempt < 10; attempt++ {
		candidate := ActorID(newVisitorActorID())
		if _, exists := w.Actors[candidate]; !exists {
			id = candidate
			break
		}
	}
	if id == "" {
		log.Printf("sim/visitor: dispatchSpawn: actor-ID minting exhausted 10 retries; skipping")
		return
	}
	// Means to pay (LLM-373 / LLM-455): the traveler spawns carrying a pack + purse sized to
	// his errand. A wholesale factor (a SELL errand) carries a heavy bale of imported
	// cloth/iron/salt to sell plus a large purse to buy the village's surplus — operator-tunable
	// (visitor_factor_pack_units / visitor_factor_purse_*), clamped here against misconfig. A
	// buyer carries a fuller purse and no wares: injecting coin is the point, and he pays for his
	// good and his room in coin. A passer-through carries the ordinary small mixed pack (its
	// lodging barter + trade-talk flavor).
	var pack map[ItemKind]int
	var purse int
	switch {
	case trade != nil && trade.Direction == TradeDirectionSell:
		units := w.Settings.VisitorFactorPackUnits
		if units < 1 {
			units = DefaultVisitorFactorPackUnits
		}
		ironUnits := w.Settings.VisitorFactorIronUnits
		if ironUnits < 1 {
			ironUnits = DefaultVisitorFactorIronUnits
		}
		saltUnits := w.Settings.VisitorFactorSaltUnits
		if saltUnits < 1 {
			saltUnits = DefaultVisitorFactorSaltUnits
		}
		threadUnits := w.Settings.VisitorFactorThreadUnits
		if threadUnits < 1 {
			threadUnits = DefaultVisitorFactorThreadUnits
		}
		purseMin := w.Settings.VisitorFactorPurseMin
		if purseMin < 0 {
			purseMin = 0
		}
		purseMax := w.Settings.VisitorFactorPurseMax
		if purseMax < purseMin {
			purseMax = purseMin
		}
		pack, purse = seedFactorPack(r, units, ironUnits, saltUnits, threadUnits, purseMin, purseMax)
		// Stamp the shipment he arrived with, so the sell-side settle has a baseline to
		// measure drawdown against (LLM-553). Read off the seeded pack rather than the
		// units knob: seedFactorPack jitters the count, and the errand Good is the bale's
		// headline kind, so the pack is the one place the true arrival quantity exists.
		trade.ShipmentQty = pack[trade.Good]
	case trade != nil && trade.Direction == TradeDirectionBuy:
		pack, purse = seedBuyerPack(r)
	default:
		pack, purse = seedVisitorPack(r)
	}

	vs := &VisitorState{
		Archetype:   profile.Archetype,
		Origin:      profile.Origin,
		Disposition: profile.Disposition,
		ExpiresAt:   expiresAt,
		// Arriving: on the road, walking in to his first stop. Pacing flips it to
		// making_rounds on arrival (LLM-373).
		Phase: VisitorPhaseArriving,
		// A returner's PERSONA is stable across visits, but its road-word is fresh each
		// trip: (re)selected here every spawn, NOT stored on the recurring_visitor row (LLM-372).
		Payload:     selectRoadWord(w, r, inputs.Now),
		RecurringID: returnerID, // "" for a fresh stranger; set for a returning traveler
		// The bound trade errand (LLM-455): non-nil for a merchant, nil for a passer-through.
		// Drives the rounds cue, the commerce-confinement steer/gate, and the coin-valve.
		Trade: trade,
		// What he may SPEND here = what he arrived carrying (LLM-644). Sales
		// credit the wallet but never this, so a seller factor cannot recycle
		// his bale's proceeds into surplus-buying.
		SpendBudget: purse,
	}

	// Give the traveler a visible form: resolve his sprite (by archetype for a passer, by
	// class for a merchant) to the uuid-keyed SpriteID the client draws by (LLM-379/455). ""
	// on miss ships him spriteless — logged, but the spawn still proceeds.
	spriteID, ok := visitorSpriteID(w, vs)
	if !ok {
		log.Printf("sim/visitor: dispatchSpawn: no sprite for visitor (archetype=%q, merchant=%v); shipping spriteless",
			profile.Archetype, trade != nil)
	}
	visitor := &Actor{
		ID:                id,
		DisplayName:       displayName,
		Kind:              KindNPCShared,
		LLMAgent:          VisitorAgentName,
		Pos:               edgeTile,
		InsideStructureID: "",
		SpriteID:          spriteID,
		Facing:            "south",
		Needs:             seedVisitorNeeds(),
		Inventory:         pack,
		Coins:             purse,
		VisitorState:      vs,
		State:             StateIdle,
	}
	w.Actors[id] = visitor
	w.outdoorActors[id] = struct{}{}

	// Tell every connected client the traveler exists, BEFORE the walk-in below is
	// issued (LLM-552). add_npc_from_broadcast is the only path that puts an actor
	// into the client's placed_npcs after page load, and both _on_npc_walking and
	// _on_npc_arrived drop frames for an id they don't hold — so without this frame
	// a client connected at spawn time never renders the visitor at all, for his
	// whole stay, while the villagers he huddles with talk to nobody. Reusing
	// NPCCreated mirrors the despawn side, which reuses ActorDeparted -> npc_deleted;
	// the client handler is id-idempotent, so a client that already has him from a
	// page load ignores it.
	//
	// Ordering is load-bearing: the npc_walking from issueVisitorWalk is dropped
	// unless the create precedes it on the wire. It does — the chain is order-
	// preserving end to end: emit calls subscribers synchronously on the world
	// goroutine, Hub.Handle sends onto the one broadcast channel from that same
	// goroutine, and Hub.Run drains it in a single goroutine onto each client's
	// FIFO send channel. The one way the create is lost is a full broadcast buffer,
	// which drops frames of every type alike and is accounted in ws.frames_dropped.
	// A spriteless visitor (sprite lookup missed above) emits a nil Sprite, which
	// marshals to no sprite key and the client skips — matching the "ships
	// spriteless" outcome already logged, not a new failure.
	w.emit(&NPCCreated{
		ActorID:     id,
		DisplayName: displayName,
		Kind:        KindNPCShared,
		LLMAgent:    VisitorAgentName,
		X:           edgeTile.X,
		Y:           edgeTile.Y,
		Facing:      visitor.Facing,
		Sprite:      w.Sprites[spriteID],
		At:          inputs.Now.UTC(),
	})

	// The spawn is committed — now record the returner's arrival (bump visit count,
	// clear next_return_at) on the durable row it came back as (LLM-372).
	if dueReturner != nil {
		dueReturner.beginReturnerVisit()
		log.Printf("sim/visitor: returner arrived — %s the %s from %s (rvis=%s, id=%s, visit #%d)",
			dueReturner.Name, dueReturner.Archetype, dueReturner.Origin, dueReturner.ID, id, dueReturner.VisitCount)
	}

	// Walk in from the road (LLM-379): head to the town's gathering place (the tavern
	// / destID from pickVisitorDestination) — the neutral village anchor, NOT a shop.
	// This is lifecycle, getting him off the edge tile into the perceivable core; the
	// engine no longer picks which shop he trades at (that was the v1 circuit fighting
	// his own move_to). Arrival at the anchor stamps the arrival warrant, so his first
	// decision tick sees the "## Your rounds" cue and he chooses his own first stop.
	// Entry policy decides interior vs a visitor slot; leaveHuddle=false (a fresh spawn
	// is in no conversation). A dead-end leaves him at the edge tile for despawn/cleanup
	// after the stay window.
	issueVisitorWalk(w, id, destID, inputs.Now)
	t.Spawned++
	log.Printf("sim/visitor: spawn %s (id=%s, archetype=%s, origin=%s, disposition=%s, depart=%s, anchor=%s, edge=(%d,%d))",
		displayName, id, profile.Archetype, profile.Origin, profile.Disposition,
		expiresAt.Format("2006-01-02 15:04"), destID, edgeTile.X, edgeTile.Y)
}

// rehydrateVisitorsOnLoad restores the durable in-flight visitor mirror
// (LLM-369) into World.Actors so a restart resumes travelers instead of dropping
// them — the reverse of ActorsRepo.SaveSnapshot's filter that keeps visitors out
// of the actor aggregate. Runs from FinalizeLoad AFTER rebuildIndices (so the
// secondary-index maps exist to append to); world-goroutine-only (FinalizeLoad
// runs before Run starts).
//
// Reconcile against the wall-clock ExpiresAt: a visitor still within its stay
// window is rebuilt into a live Actor at its checkpointed tile; one whose stay
// elapsed while the engine was down is dropped — not resurrected for another
// stay, not walked off — and its row is swept from the table on the next
// checkpoint (absent from cp.Actors -> delete-stale). A dropped / dup / loader-
// inconsistent row is logged, never fatal: a live village must boot, and a lost
// visitor is data-clean (transient by design). Such a row never arises from a
// consistent checkpoint (a visitor and the rest of the world write in the SAME
// SaveWorld Tx); it means a manual / out-of-band edit.
//
// The Actor is reconstructed the way dispatchVisitorSpawn mints one —
// KindNPCShared, the shared salem-visitor VA, needs seeded to 0, StateIdle —
// differing in the persisted identity / position / VisitorState and, from LLM-373,
// the day-plan pack / purse / booked-room grant restored off the plan jsonb.
// Secondary-index membership (outdoorActors / actorsByStructure) is set to match
// the loaded InsideStructureID.
func (w *World) rehydrateVisitorsOnLoad(ctx context.Context) error {
	visitors, err := w.repo.Visitors.LoadAll(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var restored, elapsed int
	for id, lv := range visitors {
		if lv == nil {
			continue
		}
		if lv.ID != id {
			log.Printf("sim: rehydrate visitor: map key %q != LoadedVisitor.ID %q (loader inconsistency) — dropping", id, lv.ID)
			continue
		}
		if lv.VisitorState == nil {
			log.Printf("sim: rehydrate visitor %q: nil VisitorState — dropping", id)
			continue
		}
		if _, exists := w.Actors[id]; exists {
			log.Printf("sim: rehydrate visitor %q: id already present in loaded actors — dropping visitor row", id)
			continue
		}
		if !lv.VisitorState.Phase.Valid() {
			log.Printf("sim: rehydrate visitor %q: invalid phase %q — dropping", id, lv.VisitorState.Phase)
			continue
		}
		// inside_structure_id is a soft ref (no FK). The normal actor structure-ref
		// validation ran in LoadWorld before visitors are added here, so validate it
		// now — a dangling ref (only possible from an out-of-band edit; a consistent
		// checkpoint writes structures and visitors in one Tx) would otherwise index
		// the visitor under a structure that doesn't exist.
		if lv.InsideStructureID != "" {
			if w.Structures[lv.InsideStructureID] == nil {
				log.Printf("sim: rehydrate visitor %q: inside_structure_id %q not among loaded structures — dropping", id, lv.InsideStructureID)
				continue
			}
		}
		if now.After(lv.VisitorState.ExpiresAt) {
			elapsed++
			continue
		}
		// Pack / purse / booked-room grant ride on the plan jsonb (LLM-373), restored
		// here so a mid-stay deploy resumes the traveler with its wares to pay with
		// and its room still booked. nil maps seed empty (a traveler that carried
		// nothing, or a row written before the plan column existed).
		inventory := lv.Inventory
		if inventory == nil {
			inventory = map[ItemKind]int{}
		}
		roomAccess := lv.RoomAccess
		if roomAccess == nil {
			roomAccess = map[RoomAccessKey]*RoomAccess{}
		}
		// Sprite is derived from the visitor state (archetype for a passer, class for a
		// merchant — both persisted), not stored separately, so a restart resolves it the
		// same way spawn does and doesn't strand the traveler invisible (LLM-379/455).
		spriteID, ok := visitorSpriteID(w, lv.VisitorState)
		if !ok {
			log.Printf("sim: rehydrate visitor %q: no sprite for archetype %q (merchant=%v); restoring spriteless",
				id, lv.VisitorState.Archetype, lv.VisitorState.Trade != nil)
		}
		actor := &Actor{
			ID:                id,
			DisplayName:       lv.DisplayName,
			Kind:              KindNPCShared,
			LLMAgent:          VisitorAgentName,
			Pos:               lv.Pos,
			InsideStructureID: lv.InsideStructureID,
			SpriteID:          spriteID,
			Facing:            "south",
			Needs:             seedVisitorNeeds(),
			Inventory:         inventory,
			Coins:             lv.Coins,
			RoomAccess:        roomAccess,
			VisitorState:      lv.VisitorState,
			State:             StateIdle,
		}
		w.Actors[id] = actor
		if actor.InsideStructureID == "" {
			w.outdoorActors[id] = struct{}{}
		} else {
			if w.actorsByStructure[actor.InsideStructureID] == nil {
				w.actorsByStructure[actor.InsideStructureID] = make(map[ActorID]struct{})
			}
			w.actorsByStructure[actor.InsideStructureID][id] = struct{}{}
		}
		restored++
	}
	if restored > 0 || elapsed > 0 {
		log.Printf("sim: rehydrated %d in-flight visitor(s); dropped %d whose stay elapsed while down", restored, elapsed)
	}
	return nil
}

// DefaultVisitorPaceInterval is how long a STATIONARY traveler may stand quiet on his
// rounds before the engine wakes him to reconsider his next move (LLM-379). The engine
// no longer chooses his stops — every arrival already stamps a decision tick, so this
// only paces the gaps when he stands still. Short enough to feel lively, long enough to
// bound token cost; eco-gated to zero while unwatched. Wall-clock: the Salem clock is
// real-time.
const DefaultVisitorPaceInterval = 5 * time.Minute

// dispatchVisitorPacing keeps each in-flight traveler LIVELY without the engine
// choosing where he goes (LLM-379). The engine renders his situation (perception's
// "## Your rounds" and "## A bed for the night") and he navigates himself with move_to;
// finishArrival already stamps a decision-tick warrant on every arrival, so the model
// gets a beat each time a move lands. This fills the STATIONARY gaps: it flips the
// daytime→evening phase (swapping the rounds cue for the seek-a-bed cue), advances the
// arriving→making_rounds phase, and — for a traveler standing still, unengaged, and
// quiet past the pace interval — stamps a VisitorRoundsWarrant so he reconsiders his
// next stop rather than freezing between legs.
//
// The engine issues NO movement here. The old v1 circuit picked his shop and force-
// walked him there (and to the tavern at dusk), fighting his own move_to — that is
// gone. The evening-leisure / seek-a-bed cues draw him to the inn, the same model-
// driven pull a resident feels. A traveler who won't move on or won't book is a prompt
// bug, not something the engine forces (soul-doc). Departure (daybreak) stays with
// despawn.
//
// MUST run inside a Command.Fn on the world goroutine. Eco-gated by the caller
// (TickVisitorCascade), so a visitor costs nothing while unwatched.
func dispatchVisitorPacing(w *World, inputs VisitorTickInputs, t *VisitorCascadeTelemetry) {
	now := inputs.Now
	for _, actor := range w.Actors {
		if actor == nil || actor.VisitorState == nil {
			continue
		}
		vs := actor.VisitorState
		if vs.Phase == VisitorPhaseDeparting {
			continue // despawn owns it
		}
		// Settle a buy-merchant's errand the moment he holds his errand good (LLM-455). An
		// INVENTORY check, not an event: a buyer spawns empty-packed (seedBuyerPack) and his
		// commerce is confined to his counterparty (the talk-only gate), so holding the good
		// means the purchase landed — an exact match on the errand good, and it can't misfire
		// on a meal or a room (a consumable is eaten, a service is never held). Turns the rounds
		// cue to the wind-down. A seller (factor) has an open-ended two-way deal and winds down
		// on the dusk phase flip instead.
		if tr := vs.Trade; tr != nil && tr.Direction == TradeDirectionBuy && !tr.Settled && actor.Inventory[tr.Good] > 0 {
			tr.Settled = true
		}
		// Settle a SELL errand once the shipment he came to deliver is substantially
		// landed (LLM-553). The mirror of the buy check above: a buyer settles on HOLDING
		// the good he came for, a seller on having PARTED with the one he came to move.
		//
		// Before this, no sell errand could ever settle — the comment above said a factor
		// "winds down on the dusk phase flip instead", but `## Your rounds` renders its
		// trade steer off `!Settled`, so for the whole daylight visit the one cue carrying
		// a destination named his counterparty, and commerce confinement meant that was
		// also the only place he could transact. He bid the keeper farewell, found every
		// other stop talk-only, and came back — for hours. The settled-seller wind-down
		// prose has existed since LLM-507 and was unreachable; this is what reaches it.
		if tr := vs.Trade; tr != nil && tr.Direction == TradeDirectionSell && !tr.Settled &&
			sellErrandDelivered(tr.Delivered, tr.ShipmentQty) {
			tr.Settled = true
		}
		// Dusk: turn from the daytime rounds to the evening. Only the phase flips — the
		// seek-a-bed / evening-leisure cues (not an engine walk) draw him to the inn.
		if vs.Phase != VisitorPhaseLodging && !visitorDaytime(w, now) {
			vs.Phase = VisitorPhaseLodging
			t.CircuitToLodging++
		}
		// Once he is in the village on the day, he is making his rounds — the phase that
		// gates the rounds cue. (A legacy 'present' row folds in here too.)
		if vs.Phase == VisitorPhaseArriving || vs.Phase == VisitorPhasePresent {
			vs.Phase = VisitorPhaseMakingRounds
		}
		// Where he stands, counted as a call if there is a keeper to call on (LLM-575).
		// A live-state re-check, the same shape as the errand settles above — and for the
		// same reason: the arrival event alone samples at a moment the OTHER party chooses.
		recordVisitorCallInPlace(w, actor)
		// Pace a STATIONARY traveler. The arrival warrant covers the just-moved case;
		// this covers the gaps. Leave him be while walking (a move in flight), asleep or
		// resting (sacrosanct), already warranted (a beat is pending), or mid-tick. Then,
		// if he has stood quiet past the pace interval, wake him to reconsider — the cue
		// tells him what is around and how the light is going, and he chooses with move_to
		// (a next stop, or the inn).
		//
		// NOT gated on CurrentHuddleID: a huddle lingers after a conversation ends until
		// someone leaves, so gating on it would suppress pacing forever and freeze a
		// traveler who finished trading but stayed in the huddle (code_review). The quiet
		// timer already handles an ACTIVE conversation — it keeps ticking the reactor, so
		// visitorPaceElapsed reads not-quiet; only a huddle gone quiet past the interval
		// paces, which is exactly when he should move on.
		switch {
		case actor.MoveIntent != nil,
			actor.State == StateSleeping, actor.State == StateResting,
			actor.WarrantedSince != nil, actor.TickInFlight:
			continue
		}
		if !visitorPaceElapsed(w, actor, now) {
			continue
		}
		if tryStampWarrant(w, actor, WarrantMeta{
			TriggerActorID: actor.ID,
			Force:          false,
			Reason:         VisitorRoundsWarrantReason{},
		}, now) {
			t.RoundsPaced++
		}
	}
}

// visitorPaceElapsed reports whether a stationary traveler has stood quiet past the
// rounds pace interval — long enough to wake him to reconsider (LLM-379). Mirrors the
// idle-backstop quiet computation: the last reactor tick if any, else the world's
// LoadedAt anchor (so a just-spawned or just-loaded visitor whose walk has ended paces
// promptly). A zero/backward clock never reads as elapsed.
func visitorPaceElapsed(w *World, a *Actor, now time.Time) bool {
	effective := w.LoadedAt
	if lastTick, ok := lastReactorTickAt(a); ok && lastTick.After(effective) {
		effective = lastTick
	}
	if effective.IsZero() {
		return false
	}
	return now.Sub(effective) > DefaultVisitorPaceInterval
}

// RecordVisitorArrival marks a keeper-business as one the traveler has actually called
// at, on a genuine co-present arrival (LLM-379). Wired to ActorArrived via
// cascade/visitor_arrival.go, and re-sampled from the pacing pass by
// recordVisitorCallInPlace (LLM-575), it is the ONLY writer of
// VisitorState.VisitedBusinesses now that the engine no longer chooses his stops —
// "visited" is a fact about where he went and found someone to trade with, never a
// target the engine picked. The rounds cue renders these back so he routes onward
// instead of repeating a shop.
//
// structureID is the arrival's DestStructureID (set for a walk INTO a shop and for a
// doorstep/knock at its visitor slot alike). Records only during his daytime rounds,
// only a non-inn BUSINESS, and only when that structure's keeper is actually present —
// the same at-post signal the arrival huddle uses, so a walk past a shut shop or a stop
// at the inn is not counted. Idempotent (appendUniqueStructure dedupes). MUST run on the
// world goroutine.
func RecordVisitorArrival(actorID ActorID, structureID StructureID) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if structureID == "" {
				return nil, nil
			}
			actor, ok := w.Actors[actorID]
			if !ok || actor == nil || actor.VisitorState == nil {
				return nil, nil
			}
			vs := actor.VisitorState
			switch vs.Phase {
			case VisitorPhaseArriving, VisitorPhaseMakingRounds, VisitorPhasePresent:
				// on his daytime rounds
			default:
				return nil, nil // evening/departing — an inn stop is lodging, not a round
			}
			// Verify he is ACTUALLY at this structure right now — inside it, or at its
			// loiter-pin slot within conversational scope. Reading live position (not the
			// arrival event's coordinates) means a stale/misrouted ActorArrived, or a
			// direct call for a traveler who is elsewhere, records nothing (code_review).
			// This is the SAME scope the trade huddle forms in, so "visited" means "was
			// here and could trade", never a shop he never reached.
			if conversationalScopeStructure(w, actor) != structureID {
				return nil, nil
			}
			// A round is a call on a place of BUSINESS. keeperPresentAt below only asks
			// whether someone whose workplace this is stands here awake, which the
			// constable at his Meeting House post satisfies — so without this the meeting
			// house entered his called-at list and kept rendering there (LLM-554).
			if !structureIsBusiness(w, structureID) {
				return nil, nil
			}
			if structureIsLodging(w, structureID) {
				return nil, nil // the inn is the evening venue, not a round stop
			}
			if !keeperPresentAt(w, structureID) {
				return nil, nil // no keeper tending — nothing to call on
			}
			vs.VisitedBusinesses = appendUniqueStructure(vs.VisitedBusinesses, structureID)
			return nil, nil
		},
	}
}

// recordVisitorCallInPlace re-samples RecordVisitorArrival's predicate against where the
// traveler is standing NOW, so a call is recorded whichever of the two parties arrived
// last (LLM-575). Called from the visitor pacing pass; MUST run on the world goroutine.
//
// The predicate — "he is at a keeper-business whose keeper is tending it" — is a fact
// about a MOMENT, but until this it was sampled at exactly one: his own ActorArrived. A
// keeper who happens to be off the premises as the traveler walks in loses the stop for
// good, because his returning a few seconds later fires no arrival event for the
// TRAVELER and nothing re-reads the world. That is not a corner: Tobias Hewes the
// nail-buyer walked into the Blacksmith on 2026-07-30 while Ezekiel Crane was thirty
// seconds away at the wood pile, bought nine nails off him at 19:04, and spent the next
// half hour being told he had called at the General Store, the apothecary and Ellis Farm
// but never the smithy — so he told the apothecary he had got his nails at the General
// Store. It self-healed only when he wandered back to the smithy at 19:32.
//
// Sampling here instead of adding a keeper-arrival subscriber keeps ONE predicate: the
// Command already validates everything against live actor state and is idempotent
// (appendUniqueStructure), so a re-check costs a map walk and can only ever record what
// a correctly-timed arrival would have. It also covers orderings an arrival event cannot
// see at all — a keeper waking at his own shop, a scope that resolved late.
//
// Skipped mid-walk: passing THROUGH a shop on the way somewhere else is not calling at
// it, and the arrival path already owns the moment he comes to rest. The skip DEFERS,
// never drops — finishArrival clears the intent, so the next pass sees him standing
// still and records then.
//
// "Standing still in a keeper-business's conversational scope" IS the call, deliberately
// — a traveler idling at the counter after a failed haggle has called there as surely as
// one who greeted the keeper on the doorstep, and the arrival path would have recorded
// him had he walked in a minute later. The equivalence is the point, not a heuristic.
//
// The Command never returns an error (its Fn's every exit is nil), so there is nothing
// to handle here; the cascade subscriber discards it the same way.
func recordVisitorCallInPlace(w *World, actor *Actor) {
	if actor.MoveIntent != nil {
		return
	}
	structureID := conversationalScopeStructure(w, actor)
	if structureID == "" {
		return // out on open ground — nowhere to have called
	}
	_, _ = RecordVisitorArrival(actor.ID, structureID).Fn(w)
}

// MaxPayloadSharedWith caps the shared-word memory (LLM-545). The engine-side
// writer is naturally bounded by the actors a one-day visitor actually converses
// with (village population ~25), so the cap exists for the PERSISTENCE boundary:
// visitor.plan is jsonb, and an edited/corrupt row must not be able to grow an
// unbounded slice that every checkpoint re-encodes and perception re-intersects.
// Enforced at both ends — the stamp stops appending at the cap, and the plan
// decoder truncates to it.
const MaxPayloadSharedWith = 64

// recordRoadWordShared marks the traveler's carried word (VisitorState.Payload)
// as given to the huddle's other ACTIVE conversants — peers who have themselves
// spoken (LLM-545). Called from the Speak commit path beside propagateRumorOnSpeak,
// and deliberately the same shape: the token moves through actual conversation, not
// mere co-presence, and the utterance is never read. A real two-way exchange while
// carrying a payload is taken as the word having been passed — the stateless VA
// reliably works its one piece of road-news into any genuine exchange, and the
// failure the memory exists to stop (reopening a matter the listener already
// answered — Brother Ashford re-raising the Josiah Thorne rumour on Hannah Boggs
// half an hour after she set him straight) costs far more than the mild one this
// approximation risks (a word left ungiven to someone he only traded pleasantries
// with). Idempotent per peer; visit-scoped like the Payload itself. MUST run on the
// world goroutine.
func recordRoadWordShared(w *World, h *Huddle, speakerID ActorID) {
	if w == nil || h == nil {
		return
	}
	speaker := w.Actors[speakerID]
	if speaker == nil || speaker.VisitorState == nil || speaker.VisitorState.Payload == "" {
		return
	}
	vs := speaker.VisitorState
	for id := range h.Members {
		if id == speakerID {
			continue
		}
		if w.Actors[id] == nil {
			// Belt-and-braces (code_review): by the huddle invariant a member is a
			// live actor, but a stale id must not become a stamp — read-side company
			// is snapshot-filtered, so a junk stamp would render nothing, yet it
			// still eats cap space and persists.
			//
			// Membership in World.Actors IS the complete liveness definition here:
			// deletion (visitor cleanup) is the only way an actor stops existing,
			// and there is no live-but-departed state that keeps a huddle seat —
			// the huddle invariant (Members[a] iff Actor[a].CurrentHuddleID == h.ID)
			// evicts leavers at the moment they leave. No further presence check
			// belongs on this side: the condition being recorded is "was an active
			// conversant in THIS conversation", which the utterance ring below
			// establishes — a peer who spoke here and later dozes off or walks away
			// was still given the word. Present-company filtering is the READ
			// side's job (roadWordSharedWith), where "who is in the scene NOW"
			// is actually the question.
			continue
		}
		if h.LastUtteranceAtBy(id).IsZero() {
			continue // silent bystander — the word lands through conversation
		}
		if len(vs.PayloadSharedWith) >= MaxPayloadSharedWith {
			return // cap reached — the memory is full, not a queue to rotate
		}
		vs.PayloadSharedWith = appendUniqueActor(vs.PayloadSharedWith, id)
	}
}

// appendUniqueActor appends id to ids unless already present — the ActorID twin of
// appendUniqueStructure. The sets it maintains are tiny (bounded by the actors a
// one-day visitor actually converses with), so a linear scan is fine.
func appendUniqueActor(ids []ActorID, id ActorID) []ActorID {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// issueVisitorWalk walks a freshly-spawned visitor in from the road toward the village
// anchor, entering the interior when entry policy allows and falling back to a loiter
// slot outside otherwise (StructureEnter → StructureVisit). A fresh spawn is in no
// conversation, so no leave-huddle is needed. A dead-end (no path either way) is logged,
// never fatal — despawn/cleanup collect a stranded visitor after its stay window. This
// is the ONLY engine-issued visitor move; once he is in the village he navigates himself
// with move_to (LLM-379).
func issueVisitorWalk(w *World, id ActorID, target StructureID, now time.Time) {
	dest := NewStructureEnterDestination(target)
	if _, err := MoveActor(id, dest, false, now).Fn(w); err != nil {
		dest = NewStructureVisitDestination(target)
		if _, err := MoveActor(id, dest, false, now).Fn(w); err != nil {
			log.Printf("sim/visitor: spawn: %s no walk to %s: %v", id, target, err)
		}
	}
}

// structureIsLodging reports whether a structure is the village's inn (its backing
// VillageObject carries the "lodging" tag) — the evening venue, excluded from the
// daytime rounds cue and from arrival marking.
func structureIsLodging(w *World, sid StructureID) bool {
	vobj, ok := villageObjectForStructureOnly(w, sid)
	if !ok || vobj == nil {
		return false
	}
	return vobj.HasTag("lodging")
}

// structureIsBusiness reports whether a structure is a place of business (its backing
// VillageObject carries TagBusiness) — the same predicate namedVillageDestinations and
// the constable's rounds circuit resolve a business by, and the world-side twin of
// perception.structureSnapIsBusiness. A residence, and the meeting house, are not.
func structureIsBusiness(w *World, sid StructureID) bool {
	vobj, ok := villageObjectForStructureOnly(w, sid)
	if !ok || vobj == nil {
		return false
	}
	return vobj.HasTag(TagBusiness)
}

// appendUniqueStructure appends id to s only if not already present — the visited-
// businesses set stays deduped as the arrival hook records each shop the traveler
// actually calls at (LLM-379).
func appendUniqueStructure(s []StructureID, id StructureID) []StructureID {
	for _, x := range s {
		if x == id {
			return s
		}
	}
	return append(s, id)
}

// worldDawnDuskMinutes returns the world's dawn and dusk as minute-of-day in the
// village timezone, or ok=false when the configured DawnTime/DuskTime don't parse
// (callers fail open). Mirrors effectiveShiftWindow's unscheduled branch.
func worldDawnDuskMinutes(w *World) (dawn, dusk int, ok bool) {
	dawnH, dawnM, err := ParseHM(w.Settings.DawnTime)
	if err != nil {
		return 0, 0, false
	}
	duskH, duskM, err := ParseHM(w.Settings.DuskTime)
	if err != nil {
		return 0, 0, false
	}
	return dawnH*60 + dawnM, duskH*60 + duskM, true
}

// visitorDaytime reports whether now falls in the village's business hours
// [dawn, dusk) — when the circuit runs. Fails open to daytime on an unusable
// dawn/dusk clock (the circuit keeps running rather than lodging on a bad clock;
// ExpiresAt still bounds the stay).
func visitorDaytime(w *World, now time.Time) bool {
	dawn, dusk, ok := worldDawnDuskMinutes(w)
	if !ok {
		return true
	}
	nowMin := localMinuteOfDay(w, now)
	return nowMin >= dawn && nowMin < dusk
}

// inAfternoonSpawnWindow reports whether nowMin (minute-of-day) falls in the merchant
// spawn window [max(dawn, VisitorSpawnEarliestMinute), dusk − VisitorSpawnDuskMarginMinutes)
// (LLM-455) — inclusive lower bound, exclusive upper. earliest clamps up to dawn so the
// window never opens before daylight; if the margin makes the upper bound fall at or below
// the lower bound the window is empty and every clocked spawn is rejected (the safe result).
func inAfternoonSpawnWindow(dawn, dusk, nowMin int) bool {
	earliest := VisitorSpawnEarliestMinute
	if earliest < dawn {
		earliest = dawn
	}
	latest := dusk - VisitorSpawnDuskMarginMinutes
	return nowMin >= earliest && nowMin < latest
}

// nextDaybreak returns the next daybreak instant (the village dawn minute in the
// world timezone strictly after now) — the traveler's schedule-anchored departure
// deadline (LLM-373). Fails open to a one-day fallback if the world has no usable
// dawn clock, so a misconfiguration can't mint a never-expiring visitor.
func nextDaybreak(w *World, now time.Time) time.Time {
	dawn, _, ok := worldDawnDuskMinutes(w)
	loc := w.Settings.Location
	if !ok || loc == nil {
		return now.Add(24 * time.Hour)
	}
	local := now.In(loc)
	dawnInstant := time.Date(local.Year(), local.Month(), local.Day(), dawn/60, dawn%60, 0, 0, loc)
	if !dawnInstant.After(now) {
		dawnInstant = dawnInstant.AddDate(0, 0, 1)
	}
	return dawnInstant
}

// visitorWareKinds are the trade goods a traveler may spawn carrying (LLM-373),
// drawn from the seeded item catalog so a keeper values them for barter. A small
// varied pack reads as a peddler's stock and, with the purse, pays for a room
// (LLM-353). Generic, not archetype-specific — per-archetype packs are a later
// refinement.
//
// Every kind here must be one the keeper he lodges with would plausibly want. A
// passer-through has no Trade errand, so visitorCommerceStripped leaves him pay/offer/
// quote tools ONLY when co-present with a tavern/inn keeper — every shop stop is
// talk-only. Nothing mechanically rejects a kind at that barter (pay_with_item resolves
// any seeded kind); the constraint is that the ONLY counterparty who can transact with
// him is a victualler, so a good no victualler wants has no exit at all. A factor-owned
// import in this pool would therefore tour the village on display and leave with him,
// unbuyable by the very actors who demand it (LLM-567) — factorImportKinds names those
// and TestVisitorWareKindsExcludeFactorImports keeps them out.
var visitorWareKinds = []ItemKind{"cheese", "ale", "bread"}

// seedVisitorPack returns the pack (inventory) and purse (coins) a freshly-spawned
// traveler carries: two distinct wares (a few units each) plus a modest purse. The
// coins guarantee it can cover a room (default 4/night); the wares give the
// barter-first payment its flavor and stock the circuit's trade talk. r is non-nil.
// Requires len(visitorWareKinds) >= 2 — two DISTINCT wares is the contract, and the
// distinct-index pick panics on r.Intn(0) below rather than silently returning one
// ware if the pool is ever cut to a single kind.
func seedVisitorPack(r *rand.Rand) (map[ItemKind]int, int) {
	pack := map[ItemKind]int{}
	i := r.Intn(len(visitorWareKinds))
	j := (i + 1 + r.Intn(len(visitorWareKinds)-1)) % len(visitorWareKinds)
	pack[visitorWareKinds[i]] = 3 + r.Intn(4) // 3..6
	pack[visitorWareKinds[j]] = 2 + r.Intn(3) // 2..4
	purse := 30 + r.Intn(21)                  // 30..50
	return pack, purse
}

// seedBuyerPack returns the pack + purse a buy-merchant carries (LLM-455): a fuller purse
// than an ordinary traveler and NO starting wares. Injecting coin is the point of a buyer —
// he pays for the village good he came for and for his room in coin — so he arrives with coin
// to spend, not a barter bale. r is non-nil.
func seedBuyerPack(r *rand.Rand) (map[ItemKind]int, int) {
	return map[ItemKind]int{}, 70 + r.Intn(41) // 70..110
}

// visitorSpawnClass names what one tick's spawn decision produced (LLM-626).
type visitorSpawnClass int

const (
	visitorSpawnNone     visitorSpawnClass = iota // neither roll fired — no spawn this tick
	visitorSpawnMerchant                          // a grounded trader arrives (Direction set)
	visitorSpawnPasser                            // a voice-flavor passer-through arrives
)

// visitorSpawnRoll is one tick's spawn decision — the LLM-626 split of the old
// master-roll + class-share + direction chain into two independent flows.
type visitorSpawnRoll struct {
	Class     visitorSpawnClass
	Direction TradeDirection // merchant only
	// Corrective marks a band-forced merchant (coin outside the valve band):
	// an economic instrument, not a social call — so a due returner must NOT
	// preempt it the way it preempts a trickle or flavor spawn.
	Corrective bool
	Reason     string // telemetry skip reason when Class == visitorSpawnNone
}

// rollVisitorSpawn makes the per-tick spawn decision (LLM-626). Two independent
// flows, merchant rolled first (at most one visitor spawns per tick, and the
// corrective merchant is the roll with a job to do):
//
//   - MERCHANT — band-state driven. Resident coin at/above VisitorCoinBandHigh
//     rolls VisitorMerchantCorrectionChancePermille for a forced SELLER (the
//     factor, draining the excess); at/below the low band, the same knob for a
//     forced BUYER (injecting). In-band — or the band unconfigured (high <= 0)
//     — rolls VisitorMerchantTrickleChancePermille instead, direction picked by
//     the VisitorSellWeightPermille weighted random: the ordinary grounded
//     trader passing through on business.
//   - PASSER — VisitorPasserSpawnChancePermille, the independent flavor roll.
//
// All three chances default 0 = that flow off (the Phase-1 no-op posture).
// Runs on the world goroutine (reads live coin).
func rollVisitorSpawn(w *World, r *rand.Rand) visitorSpawnRoll {
	trickleChance := clampPermille(w.Settings.VisitorMerchantTrickleChancePermille)
	correctionChance := clampPermille(w.Settings.VisitorMerchantCorrectionChancePermille)
	passerChance := clampPermille(w.Settings.VisitorPasserSpawnChancePermille)
	merchantChance := trickleChance
	corrective := false
	direction := TradeDirection("")
	if high := w.Settings.VisitorCoinBandHigh; high > 0 {
		resident := residentCoinOnMap(w)
		if resident >= high {
			corrective, direction = true, TradeDirectionSell
		} else if low := w.Settings.VisitorCoinBandLow; low > 0 && resident <= low {
			corrective, direction = true, TradeDirectionBuy
		}
		if corrective {
			merchantChance = correctionChance
		}
	}
	if merchantChance > 0 && r.Intn(1000) < merchantChance {
		if !corrective {
			direction = TradeDirectionBuy
			if r.Intn(1000) < effectiveVisitorSellWeight(w) {
				direction = TradeDirectionSell
			}
		}
		return visitorSpawnRoll{Class: visitorSpawnMerchant, Direction: direction, Corrective: corrective}
	}
	if passerChance > 0 && r.Intn(1000) < passerChance {
		return visitorSpawnRoll{Class: visitorSpawnPasser}
	}
	// "Disabled" reads the three CONFIGURED chances, not the band-selected
	// merchant chance — a trickle-on world whose valve is out of band with the
	// correction off is a flow standing quiet, not a disabled feature, and
	// telemetry claiming "disabled" there would send an operator to the wrong
	// knob (code_review).
	if trickleChance == 0 && correctionChance == 0 && passerChance == 0 {
		return visitorSpawnRoll{Class: visitorSpawnNone, Reason: "disabled (all spawn chances 0)"}
	}
	return visitorSpawnRoll{Class: visitorSpawnNone, Reason: "rolls didn't fire"}
}

// clampPermille clamps a permille chance to [0,1000]. The DEFAULTS are applied
// by the settings loaders when the row is absent, NOT re-defaulted here — so an
// explicit 0 genuinely means "off" and a test can force a deterministic flow.
func clampPermille(v int) int {
	if v < 0 {
		return 0
	}
	if v > 1000 {
		return 1000
	}
	return v
}

// effectiveVisitorSellWeight clamps the trickle seller weight to [0,1000] (LLM-455). The
// DEFAULT (150) is applied by the settings loaders (repo/pg + repo/mem) when the row is absent,
// NOT re-defaulted here — so an explicit 0 genuinely means "no sellers" and a test can force a
// deterministic direction.
func effectiveVisitorSellWeight(w *World) int {
	return clampPermille(w.Settings.VisitorSellWeightPermille)
}

// bindVisitorErrand binds a merchant visitor's grounded errand from live world state
// (LLM-455) in the given direction, or ok=false when the economy can't service one — the
// caller then falls back to a passer-through. Runs on the world goroutine.
func bindVisitorErrand(w *World, r *rand.Rand, direction TradeDirection) (*TradeErrand, bool) {
	if direction == TradeDirectionSell {
		return bindSellErrand(w, r)
	}
	return bindBuyErrand(w, r)
}

// bindSellErrand binds the factor's sell errand (LLM-455): Counterparty = the village
// distributor (the import absorber), Good = iron as the headline of the bale (the
// load-bearing import — the forge's nail boost) though the pack carries the full
// cloth/iron/salt shipment. ok=false when no distributor structure is placed — no one to
// absorb imports, so no sell errand is possible.
func bindSellErrand(w *World, r *rand.Rand) (*TradeErrand, bool) {
	distID, _, ok := pickDistributorDestination(w)
	if !ok {
		return nil, false
	}
	return &TradeErrand{Direction: TradeDirectionSell, Good: factorIronKind, Counterparty: distID}, true
}

// bindBuyErrand binds a buyer's errand from live state (LLM-455): scan businessowners at an
// open post (keeperPresentAt) and bind Good + Counterparty to one of the exportable goods
// they sell. Skips wholesale-tier sellers (they sell only to the distributor — a walk-in
// buyer can't trade there) and non-exportable goods (a service / eat-here-only good can't be
// carried off the map). Candidates are sorted for deterministic test reproducibility (map
// iteration order is otherwise unstable), then one is picked with r. ok=false when no
// (open keeper, exportable good) pair exists — the economy can't service a buyer right now.
func bindBuyErrand(w *World, r *rand.Rand) (*TradeErrand, bool) {
	type candidate struct {
		structure StructureID
		good      ItemKind
	}
	var cands []candidate
	for _, a := range w.Actors {
		if a == nil || a.BusinessownerState == nil || a.VisitorState != nil {
			continue
		}
		sid := a.WorkStructureID
		if sid == "" || a.RestockPolicy == nil {
			continue
		}
		if !keeperPresentAt(w, sid) {
			continue // shop shut / keeper away — nothing to buy right now
		}
		if SellerAtWholesaler(w.VillageObjects, sid) {
			continue // wholesale-tier: sells only to the distributor, not a walk-in buyer
		}
		for _, e := range a.RestockPolicy.ProduceEntries() {
			good := e.Item
			if good == "" || !KindBarterable(w.ItemKinds[good]) {
				continue // a service / eat-here-only good can't be carried off the map
			}
			if a.Inventory[good] <= 0 {
				continue // the keeper produces it but has none on hand right now — not a real trade (code_review)
			}
			cands = append(cands, candidate{structure: sid, good: good})
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].structure != cands[j].structure {
			return cands[i].structure < cands[j].structure
		}
		return cands[i].good < cands[j].good
	})
	pick := cands[r.Intn(len(cands))]
	return &TradeErrand{Direction: TradeDirectionBuy, Good: pick.good, Counterparty: pick.structure}, true
}

// ProvisionerArchetype labels a buyer whose errand good is food (LLM-503): a
// traveler buying journeycake or cheese is provisioning for the road, not
// journeying to fetch a snack — "journeycake-buyer" misread the stop as the
// trip's purpose. Non-food buyers keep the grounded "<good>-buyer" persona.
const ProvisionerArchetype = "provisioner"

// visitorMerchantLabel derives a merchant's archetype label from its errand (LLM-455): a
// buyer is "<good>-buyer" (cheese -> "cheese-buyer"), grounded in the real good so the persona
// can never name an untradeable trade — except a FOOD good, where the persona is
// "provisioner" (LLM-503); a seller keeps the established "factor" persona (he
// deals with the distributor, already grounded). Uses the good's singular display noun.
func visitorMerchantLabel(w *World, trade *TradeErrand) string {
	if trade == nil {
		return ""
	}
	if trade.Direction == TradeDirectionSell {
		return FactorArchetype
	}
	noun := string(trade.Good)
	if def := w.ItemKinds[trade.Good]; def != nil {
		if kindSatisfiesHunger(def) {
			return ProvisionerArchetype
		}
		noun = def.Singular()
	}
	return noun + "-buyer"
}

// Factor pack + purse defaults (LLM-410), used when the WorldSettings knobs are unset.
// A factor carries a bale of cloth/charms to sell and a heavier purse than an ordinary
// traveler (30..50) so he can buy the village's surplus and inject coin. Operator-tunable
// via visitor_factor_pack_units / visitor_factor_purse_min / visitor_factor_purse_max.
const (
	DefaultVisitorFactorPackUnits = 2
	DefaultVisitorFactorPurseMin  = 120
	DefaultVisitorFactorPurseMax  = 200
	// DefaultVisitorFactorIronUnits (LLM-442) — bars of iron per visit, a
	// SHIPMENT rather than the clothing-scale per-kind quantity: factor visits
	// are rare (visitor return cooldowns run 14–45 days), and each bar backs
	// only one boosted nail batch, so the pack must bridge the gap between
	// visits or the forge falls back to rough nails as its everyday path.
	DefaultVisitorFactorIronUnits = 10
	// DefaultVisitorFactorSaltUnits (LLM-444) — sacks of salt per visit, a
	// SHIPMENT for the same reason as iron: salt is consumed batch-by-batch
	// across the tavern and inn kitchens (1 sack per boosted dish), so the rare
	// factor must land enough to bridge the gap between visits or the salt cue
	// sits silent and the coin drain barely fires. Sized a little above iron
	// because salt feeds several kitchens rather than one forge; tunable.
	DefaultVisitorFactorSaltUnits = 12
	// DefaultVisitorFactorThreadUnits (LLM-625) — spools of thread per visit, a
	// SHIPMENT like iron and salt: thread is the mender's consumable (1 spool
	// per mend at the inn), so the rare factor visit must land enough to bridge
	// the village's mending between calls. Sized at salt's scale — a wardrobe
	// mend is about as frequent as a boosted dish.
	DefaultVisitorFactorThreadUnits = 12
)

// sellErrandRemainderDivisor sets how much of his shipment a seller may leave undelivered and
// still count as done: a 1/N share of what he arrived with. A quarter is deliberately generous —
// a factor's iron and salt are SHIPMENT-sized (10 and 12 by default) precisely so one rare visit
// bridges the forge's and the kitchens' burn between calls, and the last bar or two routinely
// goes unsold because nobody needs it that afternoon. Holding out for the whole bale would leave
// him looping over a remainder no one wants, which is the defect.
const sellErrandRemainderDivisor = 4

// sellErrandDelivered reports whether a seller has landed enough of his shipment to call the
// errand done: delivered at least all but a 1/sellErrandRemainderDivisor share of what he
// arrived with.
//
// It reads TradeErrand.Delivered — a monotonic count taken at transferItem — NOT current
// holdings. Holdings cannot answer the question: the factor's deal is two-way, so a man who
// sold all ten bars and bought three back holds the same three as a man who sold seven and
// bought nothing, and only one of them is done.
//
// shipmentQty <= 0 means the arrival quantity was never stamped (a visitor checkpointed before
// LLM-553, or an errand good the pack never carried). There is no baseline to measure against,
// so the errand does not settle here and the visit winds down at the dusk phase flip exactly as
// it did before — no worse than the old behavior, and never a spuriously early settle. Not
// estimated from the pack knob: seedFactorPack jitters the count, so the knob is the wrong
// number and would put the target out of reach or trip it instantly.
//
// Subtraction, not multiplication, so a corrupt persisted quantity cannot overflow the compare.
func sellErrandDelivered(delivered, shipmentQty int) bool {
	if shipmentQty <= 0 || delivered <= 0 {
		return false
	}
	return delivered >= shipmentQty-shipmentQty/sellErrandRemainderDivisor
}

// factorWareKinds are the goods a wholesale factor spawns carrying to SELL into the
// village (LLM-410) — the imported clothing + charm catalog added in slice 2. Drawn from
// the seeded item_kind rows so the distributor values them for the two-way trade; the
// warms garments (coat/cloak) are what close the cold-relief loop. Which kinds exist is
// itself operator-tunable via item/set; the per-visit quantity is visitor_factor_pack_units.
var factorWareKinds = []ItemKind{"coat", "cloak", "homespun", "woolens", "linens", "silver_locket", "whalebone_charm"}

// factorIronKind is the imported smith's input the factor carries in SHIPMENT
// quantity (LLM-442) — seeded via ironUnits, not the per-kind unitsPerKind, so
// the rare factor visit can bridge the forge's batch-by-batch iron burn without
// inflating the garment bale to shipment size.
const factorIronKind = ItemKind("iron")

// factorSaltKind is the imported cooking input the factor carries in SHIPMENT
// quantity (LLM-444) — seeded via saltUnits, not the per-kind unitsPerKind, for
// the same reason as iron: salt is consumed batch-by-batch across the kitchens,
// so the rare visit must bring a sack, not a per-kind pinch.
const factorSaltKind = ItemKind("salt")

// factorThreadKind is the imported mending input the factor carries in SHIPMENT
// quantity (LLM-625) — the same kind mending.go's MendThreadKind names on the
// consumption side. Seeded via threadUnits for the iron/salt reason: thread is
// drawn spool-by-spool as the village brings its mending in, so the rare visit
// must land a shipment.
const factorThreadKind = MendThreadKind

// factorImportKinds names the strategic imports whose SUPPLY is deliberately owned by the
// wholesale factor errand, where the operator tuning levers live (visitor_sell_weight_permille,
// visitor_factor_iron_units / _salt_units / _thread_units). It is a policy list, not a runtime gate: no
// production path consults it — its one consumer is the guard test that keeps these kinds out
// of the generic passer-through pool, whose packs would put an import on display that the
// actors demanding it cannot buy (LLM-567). Spawn is the path it covers, and it is the only
// one that needs covering: the pack switch in dispatchVisitorSpawn is exhaustive on errand
// direction, so a no-errand visitor can only ever draw from visitorWareKinds.
var factorImportKinds = []ItemKind{factorIronKind, factorSaltKind, factorThreadKind}

// seedFactorPack returns the pack (clothing/charm goods to sell, plus iron,
// salt and thread shipments — LLM-442/LLM-444/LLM-625) and purse (a heavier
// coin float than an ordinary traveler) a wholesale factor spawns carrying
// (LLM-410). unitsPerKind of each ware kind, ironUnits bars of iron, saltUnits
// sacks of salt, and threadUnits spools of thread, each plus a small jitter so
// back-to-back factors don't carry identical bales; purse a uniform pull from
// [purseMin, purseMax]. r is non-nil; the caller clamps unitsPerKind >= 1,
// ironUnits >= 1, saltUnits >= 1, threadUnits >= 1, and purseMin <= purseMax.
func seedFactorPack(r *rand.Rand, unitsPerKind, ironUnits, saltUnits, threadUnits, purseMin, purseMax int) (map[ItemKind]int, int) {
	pack := map[ItemKind]int{}
	for _, kind := range factorWareKinds {
		pack[kind] = unitsPerKind + r.Intn(2) // unitsPerKind..unitsPerKind+1
	}
	pack[factorIronKind] = ironUnits + r.Intn(3)     // ironUnits..ironUnits+2
	pack[factorSaltKind] = saltUnits + r.Intn(3)     // saltUnits..saltUnits+2
	pack[factorThreadKind] = threadUnits + r.Intn(3) // threadUnits..threadUnits+2
	purse := purseMin
	if purseMax > purseMin {
		purse = purseMin + r.Intn(purseMax-purseMin+1)
	}
	return pack, purse
}

// visitorProfile holds the four persona slots a freshly-spawned visitor
// receives. Drawn from the hardcoded pools above.
type visitorProfile struct {
	Name        string
	Archetype   string
	Origin      string
	Disposition string
}

// generateVisitorProfile pulls one entry from each pool using the supplied random source.
// r is non-nil — callers thread the per-driver seeded rand in production and a deterministic
// seed in tests. The Archetype is a passer-through default (LLM-455): spawn OVERRIDES it with
// a derived label when the traveler binds a merchant errand, so a fresh profile carries a
// flavor archetype and only a passer-through keeps it.
func generateVisitorProfile(r *rand.Rand) visitorProfile {
	return visitorProfile{
		Name:        visitorNamePool[r.Intn(len(visitorNamePool))],
		Archetype:   passerThroughArchetypePool[r.Intn(len(passerThroughArchetypePool))],
		Origin:      visitorOriginPool[r.Intn(len(visitorOriginPool))],
		Disposition: visitorDispositionPool[r.Intn(len(visitorDispositionPool))],
	}
}

// extractSurname returns the lowercase last whitespace-delimited token of
// a display name. "Master Whitcombe" → "whitcombe"; "Ezekiel Crane" →
// "crane". Empty string for empty / whitespace-only names; for single-
// token names the token itself (treats "Tobias" as both first name AND
// surname for collision purposes — defensive).
func extractSurname(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(parts[len(parts)-1])
}

// loadActorSurnames returns a set of last-token surnames for every
// non-visitor actor in the world. Used by spawn to avoid colliding with
// a seated villager. Visitors themselves are excluded so two visitors
// don't collide-check against each other when the second rolls.
//
// MUST be called from inside a Command.Fn (reads w.Actors directly).
func loadActorSurnames(w *World) map[string]bool {
	out := map[string]bool{}
	for _, a := range w.Actors {
		if a == nil || a.VisitorState != nil {
			continue
		}
		if s := extractSurname(a.DisplayName); s != "" {
			out[s] = true
		}
	}
	return out
}

// displayNameInUse reports whether any existing DRIVEN actor in the world has
// the supplied display_name. Linear in actor count — fine at Salem scale (a
// few dozen actors). v1 ran the same check via a SELECT.
//
// KindDecorative actors are skipped (LLM-586): several ducks may all be called
// "Duck". A decorative is sprite-only and never ticked, so it is never a
// huddle member, and every name-to-actor resolution in the engine (speak's
// `to`, the vocative fallback, pay / pay_with_item targets, PC quotes) scans
// huddle peers rather than the world — a decorative cannot enter any of those
// candidate sets, so a shared name is unambiguous everywhere it is read.
//
// This MUST stay a mirror of the actor_display_name_excl database constraint.
// It uses ActorIsDriven — the same two persisted columns the constraint's
// predicate names — rather than testing Kind, so the two cannot drift even if
// some future path sets llm_memory_agent or login_username without
// recomputing Kind.
func displayNameInUse(w *World, name string) bool {
	for _, a := range w.Actors {
		if ActorIsDriven(a) && a.DisplayName == name {
			return true
		}
	}
	return false
}

// displayNameInUseAny is displayNameInUse without the decorative exemption —
// "is this string spoken for by ANY actor". Used only by CreateNPC's
// blank-name self-dedupe, which is an editor-legibility nicety rather than a
// correctness gate: unnamed placements get distinct defaults so they stay
// tellable apart in the NPC list. Do NOT use it to gate a rename; that is what
// displayNameInUse is for, and it is the one that mirrors the database
// constraint.
func displayNameInUseAny(w *World, name string) bool {
	for _, a := range w.Actors {
		if a != nil && a.DisplayName == name {
			return true
		}
	}
	return false
}

// pickVisitorDestination picks a structure for a freshly-spawned visitor
// to walk to. Prefers the tavern (oldest VillageObject tagged "tavern");
// falls back to any tagged VillageObject backed by a Structure. Returns
// (structureID, anchor-tile, true) on success, or false when no eligible
// destination is placed in the village.
//
// MUST be called from inside a Command.Fn (reads world maps directly).
func pickVisitorDestination(w *World) (StructureID, GridPoint, bool) {
	// Pass 1: smallest-ID tavern that is also backed by a Structure
	// (shared-identity bridge). v2's VillageObject doesn't carry created_at
	// today; iterating w.VillageObjects in map order isn't stable, so the
	// determinism tie-break is the lexicographic smallest ID. Filtering on
	// structureIDValid HERE (rather than after picking smallest) ensures a
	// rare tavern-without-Structure row doesn't make us fall through to
	// Pass 2 and pick a non-tavern when another structureIDValid tavern
	// would have qualified.
	var tavern VillageObjectID
	for id, vobj := range w.VillageObjects {
		if vobj == nil || !vobj.HasTag(VisitorTagTavern) {
			continue
		}
		if !structureIDValid(w, id) {
			continue
		}
		if tavern == "" || id < tavern {
			tavern = id
		}
	}
	if tavern != "" {
		anchor := w.VillageObjects[tavern].Pos.Tile()
		return StructureID(tavern), anchor, true
	}
	// Pass 2: smallest-ID VillageObject backed by a Structure. Untagged
	// or otherwise-tagged structures qualify; decoratives without a
	// Structure entry don't. v1 used random() which is less reproducible.
	var fallback VillageObjectID
	for id, vobj := range w.VillageObjects {
		if vobj == nil {
			continue
		}
		if !structureIDValid(w, id) {
			continue
		}
		if fallback == "" || id < fallback {
			fallback = id
		}
	}
	if fallback != "" {
		anchor := w.VillageObjects[fallback].Pos.Tile()
		return StructureID(fallback), anchor, true
	}
	return "", GridPoint{}, false
}

// pickArrivalDestination picks the structure a freshly-spawned traveler walks in toward
// (LLM-455, generalizing the LLM-410 factor target). A MERCHANT makes straight for his
// errand counterparty — the one must-hit stop (the open keeper he buys from, or the
// distributor he sells to) — so he arrives with his business in front of him. A
// passer-through, or a merchant whose counterparty has somehow become unbacked, falls back
// to the neutral village anchor (the tavern / gathering place, pickVisitorDestination) so he
// still arrives somewhere sensible. MUST be called inside a Command.Fn (reads world maps
// directly).
func pickArrivalDestination(w *World, trade *TradeErrand) (StructureID, GridPoint, bool) {
	if trade != nil && trade.Counterparty != "" {
		if structureIDValid(w, VillageObjectID(trade.Counterparty)) {
			if vobj := w.VillageObjects[VillageObjectID(trade.Counterparty)]; vobj != nil {
				return trade.Counterparty, vobj.Pos.Tile(), true
			}
		}
	}
	return pickVisitorDestination(w)
}

// pickDistributorDestination picks the smallest-ID distributor-tagged VillageObject that
// is also backed by a Structure (LLM-410) — the wholesale factor's arrival target. Returns
// false when no distributor structure is placed (the caller falls back to the ordinary
// tavern anchor). Lexicographic tie-break for determinism, mirroring pickVisitorDestination
// — one distributor by data convention, so order only matters if an operator tags two.
// MUST be called inside a Command.Fn.
func pickDistributorDestination(w *World) (StructureID, GridPoint, bool) {
	var pick VillageObjectID
	for id, vobj := range w.VillageObjects {
		if !IsDistributorStructure(vobj) {
			continue
		}
		if !structureIDValid(w, id) {
			continue
		}
		if pick == "" || id < pick {
			pick = id
		}
	}
	if pick != "" {
		return StructureID(pick), w.VillageObjects[pick].Pos.Tile(), true
	}
	return "", GridPoint{}, false
}

// structureIDValid reports whether a VillageObject's ID also exists in
// World.Structures — i.e. the placement is backed by a Structure (the
// shared-identity bridge). Decoratives / trees / wells have VillageObject
// rows without matching Structure rows; visitors don't walk to those.
func structureIDValid(w *World, id VillageObjectID) bool {
	_, ok := w.Structures[StructureID(id)]
	return ok
}

// pickVisitorEdgeTile picks a road tile near a randomly-chosen map edge
// for a visitor to spawn or depart on. In-memory port of v1's
// engine/visitor.go pickVisitorEdgeTile.
//
// Algorithm:
//
//  1. Shuffle the four edges (top / bottom / left / right) using the
//     supplied random source.
//  2. For each edge in order, sweep depths in [0, VisitorEdgeScanMaxDepth)
//     (exclusive upper bound — matches v1's loop, depth tile 30 itself is
//     not sampled) perpendicular to the edge. At each depth, collect tiles
//     whose raw terrain byte is TerrainDirt or TerrainCobblestone (roads).
//     Shuffle candidates at that depth and return the first one that is
//     both walkable in the obstacle-aware WalkGrid AND path-connected to
//     anchorTile via FindPathToAdjacent.
//  3. If no edge yields a candidate within the depth cap, return ok=false.
//     Caller skips this cycle.
//
// Edges blocked entirely by impassable terrain (Salem's north edge has
// continuous water) are skipped naturally — zero road candidates at any
// depth, the algorithm rotates to the next shuffled edge.
//
// r is non-nil. anchorTile is in internal grid coords (PadX/PadY-offset).
//
// MUST be called from inside a Command.Fn (reads w.Terrain).
func pickVisitorEdgeTile(w *World, grid *WalkGrid, anchorTile GridPoint, r *rand.Rand) (Position, bool) {
	if w.Terrain == nil || len(w.Terrain.Data) != MapW*MapH {
		return Position{}, false
	}
	if grid == nil {
		return Position{}, false
	}
	isRoadByte := func(b byte) bool { return b == TerrainDirt || b == TerrainCobblestone }

	type edgeMap struct {
		coord    func(depth, along int) GridPoint
		alongLen int
	}
	edges := []edgeMap{
		{func(d, a int) GridPoint { return GridPoint{X: a, Y: d} }, MapW},
		{func(d, a int) GridPoint { return GridPoint{X: a, Y: MapH - 1 - d} }, MapW},
		{func(d, a int) GridPoint { return GridPoint{X: d, Y: a} }, MapH},
		{func(d, a int) GridPoint { return GridPoint{X: MapW - 1 - d, Y: a} }, MapH},
	}
	r.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })

	for _, e := range edges {
		for depth := 0; depth < VisitorEdgeScanMaxDepth; depth++ {
			var candidates []GridPoint
			for along := 0; along < e.alongLen; along++ {
				p := e.coord(depth, along)
				if p.X < 0 || p.X >= MapW || p.Y < 0 || p.Y >= MapH {
					continue
				}
				if isRoadByte(w.Terrain.Data[p.Y*MapW+p.X]) {
					candidates = append(candidates, p)
				}
			}
			if len(candidates) == 0 {
				continue
			}
			r.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
			for _, c := range candidates {
				if !grid.CanWalk(c.X, c.Y) {
					continue
				}
				if path, _ := FindPathToAdjacent(grid, c, anchorTile); path != nil {
					return Position{X: c.X, Y: c.Y}, true
				}
			}
		}
	}
	return Position{}, false
}

// seedVisitorNeeds returns an Actor.Needs map populated with every entry
// in the Needs registry at value 0. v1's spawn path called the standard
// seedNeedRowsIfMissing helper (ZBBS-HOME-255 fix); v2's substrate-side
// equivalent is this constructor — atomic with Actor insert because both
// run in the same Command.Fn on the world goroutine.
func seedVisitorNeeds() map[NeedKey]int {
	out := make(map[NeedKey]int, len(Needs))
	for _, n := range Needs {
		out[n.Key] = 0
	}
	return out
}

// VisitorActorIDPrefix marks a transient visitor's per-visit actor id.
// Prefix "vstr-" so visitor rows are visually distinguishable from
// persistent NPC IDs (UUID-style) and PC IDs (login-username derived) in
// admin reads, AND — because visitors are intentionally not persisted to
// the uuid `actor` table (partitioned persistence) — so the id alone
// discriminates a non-persistable member wherever the actor row is absent.
const VisitorActorIDPrefix = "vstr-"

// visitorActorIDPattern is the EXACT minted-visitor id shape, identical to
// the visitor table's actor_id CHECK (^vstr-[0-9a-f]{8}$, migrations/LLM-369).
// Matching the full format — not just the prefix — is deliberate: this
// discriminator decides whether a dangling huddle_member row is pruned as a
// benign visitor membership or fataled as corruption, so a merely
// prefix-shaped but malformed id (e.g. an out-of-band `vstr-not-a-visitor`
// row) must fall through to the fatal path, not be silently swallowed.
var visitorActorIDPattern = regexp.MustCompile(`^` + VisitorActorIDPrefix + `[0-9a-f]{8}$`)

// IsVisitorActorID reports whether id is a well-formed transient-visitor id
// and so is NOT persisted to the uuid `actor` table. It is the
// load/checkpoint-boundary discriminator for "non-persistable member id": at
// LoadWorld time the visitor's in-memory actor is gone, so the id itself is
// the only signal that a dangling huddle_member row is a benign visitor
// membership rather than real corruption (LLM-452). Requires the full
// ^vstr-[0-9a-f]{8}$ shape — every id newVisitorActorID mints satisfies it,
// and anything else (including a malformed vstr- prefixed id) is NOT treated
// as a visitor.
func IsVisitorActorID(id ActorID) bool {
	return visitorActorIDPattern.MatchString(string(id))
}

// newVisitorActorID mints a fresh ActorID for a spawned visitor. Uses
// crypto/rand via randomHex for collision resistance.
//
// randomHex takes a BYTE count and hex-encodes (2 chars/byte), so 4 bytes =
// 8 hex chars — matching the visitor table's actor_id CHECK
// (^vstr-[0-9a-f]{8}$, migrations/LLM-369). randomHex(8) would mint 16 hex
// chars and violate it, so every checkpoint upsert failed (LLM-379).
func newVisitorActorID() string {
	return VisitorActorIDPrefix + randomHex(4)
}

// inputsRandOrDefault returns r when non-nil, otherwise a fresh
// time-seeded *rand.Rand. Production callers (the cascade driver) always
// supply a non-nil source seeded once at registration; the nil-fallback
// exists so tests that construct VisitorTickInputs ad-hoc don't have to
// thread a Rand through for codepaths that don't strictly need
// determinism (cleanup, the no-op spawn path under chance=0).
func inputsRandOrDefault(r *rand.Rand) *rand.Rand {
	if r != nil {
		return r
	}
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}
