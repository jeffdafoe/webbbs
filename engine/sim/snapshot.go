package sim

import "time"

// Snapshot is the immutable, slim view of the world that admin endpoints,
// perception build, and the checkpoint writer all consume. The world
// goroutine publishes a fresh Snapshot via World.published (atomic.Pointer)
// after every command, so readers atomic.Load and serialize without
// touching the world goroutine.
//
// The snapshot deliberately omits secondary indices, mutable handler state,
// and any field that consumers can recompute or don't need.
type Snapshot struct {
	AtTick      uint64
	PublishedAt time.Time

	Actors         map[ActorID]*ActorSnapshot
	Huddles        map[HuddleID]*Huddle
	Scenes         map[SceneID]*Scene
	Structures     map[StructureID]*Structure
	Orders         map[OrderID]*Order
	VillageObjects map[VillageObjectID]*VillageObject

	// NoticeboardContent is the published snapshot of World.NoticeboardContent —
	// per-board authored prose, keyed by the board's village_object id. Cascade-
	// authored (mutable on the world goroutine via SaveNoticeboardContent), so
	// unlike the immutable reference catalogs it MUST ride the snapshot to be
	// read lock-free; the client read surface (ObjectDTO content_text /
	// content_posted_at, ZBBS-HOME-291) reads it here. Value-copied per entry by
	// republish (the struct is flat). nil/absent for boards with no authored
	// content yet.
	NoticeboardContent map[VillageObjectID]*NoticeboardContent

	// Quotes is the published snapshot of World.Quotes — every scene
	// quote in the world (active and terminal), deep-cloned via
	// CloneSceneQuote so snapshot readers can't reach back into world
	// state. PC client perception build looks up
	// Snapshot.Scenes[sceneID].QuoteIDs and dereferences each ID
	// against this map; NPC perception build reads the same data on
	// the world goroutine via the live World.Quotes (no snapshot
	// trip needed). Phase 3 PR S3.
	Quotes map[QuoteID]*SceneQuote

	// PayLedger is the published snapshot of World.PayLedger — every
	// pay-offer entry in the world (pending and terminal), deep-cloned
	// via ClonePayLedgerEntry. Source of truth for admin reconciliation
	// against the projection store (the projection is best-effort;
	// authoritative state lives here). Phase 3 PR S4.
	PayLedger map[LedgerID]*PayLedgerEntry

	// ContactLedger is the published snapshot of World.ContactLedger — the
	// per-pair conversational recency trail (LLM-547), deep-cloned via
	// CloneContactLedger. Perception reads it to tell an actor who it has
	// already had its word with.
	//
	// The two windows ride alongside it because the tier derivation is a pure
	// function of the trail plus these, and perception must not reach back into
	// live World.Settings off the snapshot.
	ContactLedger        map[ActorID]map[ActorID]*ContactRecord
	ContactBrakeWindow   time.Duration
	ContactRecallHorizon time.Duration

	// CoinRecord is the published snapshot of World.CoinRecord — the per-pair
	// coin tally (LLM-572), deep-cloned via CloneCoinRecord. Perception reads it
	// so an actor answering a money claim can see what actually passed between
	// them. The window rides alongside for the reason the contact windows do:
	// CoinDealingsFor is a pure function of the record plus the window, and
	// perception must not reach back into live World.Settings.
	CoinRecord       map[ActorID]map[ActorID]*CoinPairRecord
	CoinRecordWindow time.Duration

	// LaborLedger is the published snapshot of World.LaborLedger — every
	// labor offer in the world (pending, working, and terminal), deep-cloned
	// via CloneLaborOffer so snapshot readers can't reach back into world
	// state. Source of truth for admin/umbilical inspection of the labor
	// machine; like PayLedger it has no durable projection. LLM-26.
	LaborLedger map[LaborID]*LaborOffer

	// ActionLog is the published snapshot of World.ActionLog — the
	// append-only audit trail of committed agent + engine-source
	// actions, value-copied via CloneActionLog. Consumed by the
	// atmosphere refresh cascade and per-actor narrative
	// consolidation. See engine/sim/action_log.go for the entry
	// shape. nil for an empty log.
	ActionLog []ActionLogEntry

	// PriceBook is the published snapshot of World.PriceBook — the
	// per-(seller, item) ring buffer of recent accepted-price
	// observations, deep-cloned via ClonePriceBook so snapshot
	// readers can't reach back into world state. Consumed by the
	// (not-yet-ported) buyer-side recovery-options / satiation
	// perception blocks and the future seller-side pricing
	// perception. See engine/sim/price_book.go for the substrate
	// contract. nil for an empty book.
	PriceBook map[PriceBookKey]*RingBuffer[PriceObservation]

	Environment WorldEnvironment
	Phase       Phase

	// LocalMinuteOfDay is the wall-clock minute-of-day (0–1439) in the village
	// timezone at publish time, or nil when the clock can't be established
	// (hand-built snapshots, or before settings load a Location). Computed once
	// at publish via localMinuteOfDay so consumers read the local clock without
	// the village *time.Location, which is not on the snapshot. Perception
	// renders it as time-of-day prose (ZBBS-HOME-351); schedule-aware steering
	// compares it to schedule_start/end_minute (ZBBS-HOME-352). Pointer so a
	// hand-built snapshot (nil) is distinguishable from real midnight (0).
	LocalMinuteOfDay *int

	// LocalDateUTC is midnight UTC of the village's current calendar DATE (the
	// date in WorldSettings.Location), computed once at publish via
	// orderDateUTC. It is the "today" the order-book perception split compares
	// ReadyBy against (ZBBS-HOME-403) — using the same world-TZ-date-as-midnight-
	// UTC convention ReadyBy itself is built with, so the ready/future/overdue
	// classification doesn't drift by the UTC offset near the day boundary.
	// Zero on a hand-built snapshot (no clock); perception falls back to the
	// host UTC day then.
	LocalDateUTC time.Time

	// DawnMinute / DuskMinute are the world's dawn/dusk boundary times as
	// minute-of-day (e.g. 420 = 07:00, 1140 = 19:00), parsed once at publish
	// from WorldSettings.DawnTime/DuskTime. They are the day-active window used
	// as the shift fallback for an NPC with no explicit schedule (mirrors
	// effectiveShiftWindow), so schedule-aware return-to-post steering
	// (ZBBS-HOME-352) can resolve the same window perception-side without
	// w.Settings. DawnDuskMinuteOK is true only when BOTH boundaries parsed —
	// perception uses the window only then, so a partial/failed parse can't
	// derive a bogus window from one good + one zero bound (code_review).
	DawnMinute       int
	DuskMinute       int
	DawnDuskMinuteOK bool

	// NeedThresholds is a cloned view of WorldSettings.NeedThresholds —
	// the per-need red-tier boundary. Consumers reading the snapshot
	// off-world (perception, noop-skip preflight) read thresholds here
	// rather than racing on w.Settings directly.
	NeedThresholds NeedThresholds

	// PCPresenceStaleAfter mirrors the effective PC-presence stale threshold
	// (PCPresenceStaleAfter(w), always > 0 on a published snapshot) so perception
	// can classify a co-present PC as present or stepped-away off the snapshot
	// rather than racing on w.Settings (LLM-342). Same mirror pattern as
	// NeedThresholds. A directly-constructed test snapshot that omits it reads 0,
	// which turns away-routing OFF (co-present PCs stay addressable — the legacy
	// behavior), matching the SeekWorkCoinCeiling test-snapshot convention.
	PCPresenceStaleAfter time.Duration

	// HuddleLiveWindow mirrors the EFFECTIVE WorldSettings.HuddleLiveWindow
	// (LLM-467), resolved at publish via EffectiveHuddleLiveWindow so it is never
	// 0 on a published snapshot. Perception — pure over the snapshot — reads it
	// here to classify the subject's huddle as a live conversation or a dormant
	// room. Same mirror pattern as PCPresenceStaleAfter above. A directly-
	// constructed test snapshot that omits it reads 0, which HuddleIsLive treats
	// as "always live" — the pre-LLM-467 behavior, matching the
	// SeekWorkCoinCeiling / PCPresenceStaleAfter test-snapshot convention.
	HuddleLiveWindow time.Duration

	// SeekWorkCoinCeiling mirrors the EFFECTIVE WorldSettings.SeekWorkCoinCeiling
	// (LLM-194), copied at publish via effectiveSeekWorkCoinCeiling so it is the
	// resolved shelf value (never 0 on a published snapshot). The perception seek-work
	// gates read it off the snapshot rather than racing on w.Settings;
	// subjectIsComfortable compares the subject's Coins against it (treating a 0 on a
	// directly-constructed test snapshot as the default, so test snapshots that omit it
	// keep the pre-LLM-194 always-seek behavior below the default).
	SeekWorkCoinCeiling int

	// LodgingDefaultWeeklyRate mirrors WorldSettings.LodgingDefaultWeeklyRate
	// (the operator-set weekly rent) so perception — pure over the snapshot —
	// can surface the keeper/lodger nightly-rate hints and the affordability
	// cue without racing on w.Settings. Derive the per-night figure via
	// sim.LodgingNightlyRate. Plain int, copied at publish.
	LodgingDefaultWeeklyRate int

	// LodgingBedtimeMinute is the lodger bedtime as minute-of-day in the village
	// timezone (LodgingBedtimeHour*60, with the DefaultLodgingBedtimeHour fallback
	// already applied), computed once at publish via lodgerBedtimeMinute. It is
	// the OPEN of the lodger night window [LodgingBedtimeMinute, DawnMinute) — the
	// same window the sim bed/wake gates use — so the LLM-36 retire cue, pure over
	// the snapshot, fires exactly when the engine would bed the lodger. Plain int,
	// copied at publish.
	LodgingBedtimeMinute int

	// LodgingCheckOutMinute is the lodging check-out hour as minute-of-day in the
	// village timezone (LodgingCheckOutHour*60), copied at publish. With
	// LodgingBedtimeMinute it lets perception derive the renewal-due window — the
	// span from the lodger bedtime on the final night to check-out the next
	// morning — without the village *time.Location (not on the snapshot). See
	// perception.lodgingRenewalWindow (LLM-96).
	LodgingCheckOutMinute int

	// HomeBakesActive is the set of home structure ids with a shared evening bake in
	// progress (LLM-454), projected from World.HomeBakes at publish. Perception's
	// buildBakeChoice reads it so a co-resident at a home where the bread is already
	// going can JOIN the batch (no flour of its own) rather than start a second. nil
	// when no bake is running.
	HomeBakesActive map[StructureID]bool

	// RestockReorderPct mirrors WorldSettings.RestockReorderPct — the reorder
	// threshold (whole percent of cap) for buy-side restock. Carried so the
	// "## Restocking" perception section can gate on the same boundary the
	// restock producer warrants on, pure over the snapshot rather than racing
	// on w.Settings. Plain int, copied at publish. 0 = restock disabled.
	RestockReorderPct int

	// Stall wear thresholds (LLM-118) mirror the WorldSettings knobs, carried so
	// perception can gate the owner repair cue, the co-present "battered stall"
	// line, and the degraded-sales steer on the SAME boundaries the engine
	// enforces — pure over the snapshot rather than racing on w.Settings. The
	// engine-only knobs (StallWearPerCoin, StallRepairDurationSeconds) are not
	// carried; perception never reads them. StallDegradedProducePct (LLM-446)
	// is carried so the degraded cues describe the state the engine actually
	// applies — slowed production at a positive pct, the legacy full block at 0.
	StallWearRepairThreshold  int
	StallWearDegradeThreshold int
	StallNailsPerRepair       int
	// EquipmentServiceDueThreshold is the LLM-648 due level for the wright's
	// equipment service; 0 = the mechanism is off (nothing is ever due).
	EquipmentServiceDueThreshold int
	StallDegradedProducePct      int

	// Hearth knobs (LLM-412) mirror the WorldSettings values so the hearth
	// cue, the stoke-tool gate, and the cold relief steer classify a fire
	// ("low", "out", wood per stoke) on the SAME boundaries the engine
	// enforces — pure over the snapshot, using PublishedAt as the clock. A
	// directly-constructed test snapshot that omits HearthLowMinutes treats
	// only an OUT fire as needing stoking (HearthNeedsStoking's non-positive
	// lowMinutes rule).
	HearthLowMinutes  int
	StokeWoodPerStoke int

	// GarmentThreadbareFractionX100 mirrors the WorldSettings knob (LLM-422) so the
	// cold self-line grades a warms garment's wear (ResolveWarmGarmentTier) on the
	// SAME threadbare boundary the cold sweep enforces, pure over the snapshot. A
	// directly-constructed test snapshot that omits it (0) treats no garment as
	// threadbare (garmentUnitThreadbare's non-positive fraction rule), so an unworn
	// world reads every warms garment as sound.
	GarmentThreadbareFractionX100 int

	// FarmUpkeepFloor / FarmUpkeepCoinsPerShovel mirror the WorldSettings knobs
	// (LLM-215) so the owner upkeep cue derives the shovel obligation
	// (FarmUpkeepObligation) on the SAME values assessFarmUpkeep enforces, pure over
	// the snapshot rather than racing on w.Settings. A non-positive
	// FarmUpkeepCoinsPerShovel disables the feature (obligation 0), so a
	// directly-constructed test snapshot that omits these gets no farm-upkeep cue.
	FarmUpkeepFloor          int
	FarmUpkeepCoinsPerShovel int

	// MerchantCoinFloor mirrors WorldSettings.MerchantCoinFloor RAW (LLM-294) — the
	// working-capital floor below which a stock-rich keeper is steered to conserve
	// coin. No effective-value fallback (unlike SeekWorkCoinCeiling): the perception
	// gate reads it as "MerchantCoinFloor > 0 && coins < MerchantCoinFloor", so a 0
	// here — including a directly-constructed test snapshot that omits it — means the
	// feature is off. Pure over the snapshot rather than racing on w.Settings.
	MerchantCoinFloor int

	// DefaultOutdoorSceneRadius mirrors WorldSettings.DefaultOutdoorSceneRadius —
	// the "what is around me" tile radius the move_to name-resolver and scene
	// bound use. Carried so perception can bound a proximity scan to the SAME
	// radius without racing on w.Settings (LLM-79: the satiation free-source cue
	// surfaces remembered sources at any distance UNION sources within this
	// radius). Plain int, copied at publish; <= 0 means use DefaultOutdoorSceneRadiusValue.
	DefaultOutdoorSceneRadius int

	// Assets aliases World.Assets (reference data, read-only post-load) so the
	// perception gather cue can resolve a loiter pin via the SAME asset-aware
	// computeLoiterTile the gather command uses (ResolveGatherSource) — keeping cue
	// and command in exact lockstep on which bush is harvested (LLM-93). The map is
	// aliased, not deep-copied, like RestockPolicy: assets never mutate after load.
	Assets map[AssetID]*Asset

	// ZoomMinAdmin / ZoomMinRegular mirror WorldSettings.ZoomMin* — the
	// client-side camera zoom floors (admin vs regular user). Carried on the
	// snapshot because the public GET /api/village/world read (handleWorld) is
	// lock-free off the snapshot and every client reads its zoom floor from
	// there; the admin config write routes mutate w.Settings on the world
	// goroutine, so a republish surfaces a saved change. Floats, copied at
	// publish. (The rest of the admin-tunable config — timezone, rotation time,
	// agent-ticks — is NOT mirrored here: the admin-only GET /api/village/config
	// read runs through the command channel and reads live w.Settings directly.)
	ZoomMinAdmin   float64
	ZoomMinRegular float64

	// PCAwaitReplyWindow / NPCAwaitReplyWindow are the resolved conversation
	// turn-state liveness windows (ZBBS-WORK-370), copied from WorldSettings at
	// publish with the Default*AwaitReplyWindow fallback already applied (via
	// World.awaitReplyWindow). Perception build uses them — with PublishedAt as
	// the clock — to decide whether an awaiting-reply edge is still live when
	// rendering the turn-line, so the perception nudge and the sim.Speak backstop
	// apply the same expiry. Keyed on the ADDRESSEE's kind: an edge addressed at a
	// PC uses PCAwaitReplyWindow, at an NPC uses NPCAwaitReplyWindow.
	PCAwaitReplyWindow  time.Duration
	NPCAwaitReplyWindow time.Duration

	// ItemKinds is an ALIASED reference to World.ItemKinds — the item→satisfies
	// reference catalog loaded once at startup (LoadWorld) and never mutated
	// afterward (ItemKindDef is documented read-only). Unlike the mutable
	// aggregates above it is NOT cloned at publish: the map and its *ItemKindDef
	// values are immutable, so sharing the reference is race-free and avoids
	// re-cloning the whole catalog on every command's republish. Perception —
	// pure over the snapshot — reads it to answer "what does this item ease, and
	// by how much?" for the recovery-options consumable arm and the seller-side
	// satiation cues. nil only before the first LoadWorld (hand-built test
	// snapshots that don't exercise item perception leave it nil).
	ItemKinds map[ItemKind]*ItemKindDef

	// Recipes is an ALIASED reference to World.Recipes — the item_recipe catalog
	// (how each item is produced, at what rate, with what inputs). Same posture as
	// ItemKinds: reference state loaded once at startup, hot-reload swaps the whole
	// map (never mutated in place), so sharing the reference is race-free and not
	// cloned at publish. Perception reads it to answer "what does making this good
	// consume, and how long will my inputs last?" for the producer-input runway cue
	// (perception/production_inputs.go). nil only before the first LoadWorld.
	Recipes map[ItemKind]*ItemRecipe

	// RecipeUses is an ALIASED reference to World.recipeUses — the reverse of
	// Recipes (input item -> the items it helps produce), memoized on the World
	// and refreshed whenever the catalog changes. Perception reads it to annotate
	// an inedible carried/for-sale ingredient with its purpose ("used to produce
	// stew") so a hungry model doesn't try to eat it (LLM-166). Same aliased,
	// not-cloned posture as Recipes.
	RecipeUses map[ItemKind][]ItemKind
}

// WithActor returns a shallow copy of the snapshot with one actor's entry
// overridden. The Actors map is rebuilt (its values are pointers, so the copy
// is cheap) so the override never mutates the shared published snapshot —
// preserving the lock-free read contract. Every other field (other actors,
// ledger, objects, settings, catalogs) is shared by reference: it is immutable
// for the snapshot's lifetime.
//
// The tick harness uses this to re-perceive an actor's own-state sections
// mid-tick from a post-commit ActorSnapshot (LLM-88) without forcing a fresh
// world snapshot: only the subject's entry changes, so every perception section
// derived from other actors / world state re-renders byte-identical, and only
// the self-state (## You, eat/drink/buy affordances, since-you-got-here diff)
// moves.
func (s *Snapshot) WithActor(id ActorID, a *ActorSnapshot) *Snapshot {
	cp := *s
	cp.Actors = make(map[ActorID]*ActorSnapshot, len(s.Actors))
	for k, v := range s.Actors {
		cp.Actors[k] = v
	}
	cp.Actors[id] = a
	return &cp
}
