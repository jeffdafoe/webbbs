package sim

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"
)

// Phase is the current daypart in the world. Salem operates on a simple
// two-phase day/night cycle driven by configurable dawn/dusk boundaries.
type Phase string

const (
	PhaseDay   Phase = "day"
	PhaseNight Phase = "night"
)

// WorldEnvironment carries world-level transient state: time-of-day,
// weather, atmosphere prose (the chronicler-replacement single-string mood
// line refreshed every ~4h), and timestamps the various tickers use to
// avoid re-firing a boundary they have already processed.
type WorldEnvironment struct {
	Now                     time.Time
	Weather                 string
	Atmosphere              string
	LastAtmosphereRefreshAt time.Time // last successful atmosphere refresh (UTC); see engine/sim/atmosphere.go. Restart-lossy by design — cosmetic prose, fresh fire after restart is acceptable.
	LastWeatherChangeAt     time.Time // last weather transition (UTC); see engine/sim/weather.go. Restart-lossy by design — the storm sweep boots to clear and reseeds this (SeedWeatherClear), so it is NOT persisted.
	StormDueAt              time.Time // earliest the next automatic storm may start (UTC); armed by the storm sweep (engine/sim/cascade/storm.go), zero = unarmed. Separate from LastWeatherChangeAt because the sweep re-arms it while the village is empty, and LastWeatherChangeAt also feeds WeatherChangedSinceAtmosphere. Transient — not persisted.
	LastTransitionAt        time.Time // last day↔night transition APPLY (UTC), real flip or idempotent re-apply — the phase ticker's boundary-dedupe stamp. Durable — persisted in world_state.last_transition_at.
	LastPhaseFlipAt         time.Time // last REAL day↔night flip (From != To, UTC). Feeds the public world DTO's last_transition_at, which the client reads as "when the current phase began" to position its sunset curve — a redundant force-phase must NOT re-baseline that or a resyncing client jumps toward the opposite pole (LLM-578 code_review). NOT persisted: boot-initialized from LastTransitionAt in LoadWorld, which is the real flip instant in every case except a redundant force with no flip after it before shutdown.
	LastRotationAt          time.Time // last daily asset rotation (UTC). Durable — persisted in world_state.last_rotation_at.
	LastNeedsTickAt         time.Time // last hourly needs increment (UTC, hour-truncated). Durable — persisted in world_state.last_needs_tick_at.
	TownChest               int       // coin the estate rate (LLM-652) has taken out of purses and not yet spent back. Durable — persisted in world_state.town_chest_coins: the coin has left the purses, so losing it on restart would destroy it.
}

// WorldSettings carries world-level config — checkpoint cadence, phase
// boundary times, admin-tunable thresholds. Fields expand per subsystem
// port; nothing here is hot-path on the tick.
type WorldSettings struct {
	CheckpointInterval time.Duration

	// Phase boundary times in HH:MM, interpreted in Timezone.
	DawnTime     string
	DuskTime     string
	RotationTime string
	Timezone     string
	Location     *time.Location

	// Client-side zoom floors — different for admins vs regular users.
	// Pure UI config; the sim package carries the values so admin endpoints
	// have one place to read/write.
	ZoomMinAdmin   float64
	ZoomMinRegular float64

	// AgentTicksPaused, when true, suppresses LLM agent activity globally —
	// reactive NPC ticks and chronicler fires both gated. Worker schedulers,
	// social hours, lamplighter, and rotation continue running. Used to halt
	// agent activity mid-session when a bad loop is being investigated.
	AgentTicksPaused bool

	// LodgingCheckOutHour is the wall-clock hour (in WorldSettings.Location)
	// a lodging grant expires on its final day. v1's companion
	// lodging_check_in_hour gate ("room not ready until 3pm") was deliberately
	// NOT ported (ZBBS-HOME-312 #4): it modeled real-hotel housekeeping
	// turnaround, which Salem has no analog for — actual room availability is
	// already enforced by AssignBedroomForLodger's occupancy check, so the hour
	// gate only added friction + a dead checkout→checkin window.
	LodgingCheckOutHour int

	// LodgingBedtimeHour is the wall-clock hour (in WorldSettings.Location) a
	// lodger retires for the night at the inn it rents — the civil night bedtime
	// that decouples a lodger's bed-down from any work shift. A scheduled lodger
	// (e.g. a blacksmith boarding at the tavern) was previously force-slept the
	// moment its day-job shift ended; the lodger night window is
	// [LodgingBedtimeHour, DawnTime), kept later than the village's dusk so a
	// guest keeps later hours (LLM-14). Settings key: lodging_bedtime_hour.
	// Default 22 (DefaultLodgingBedtimeHour); an out-of-range value falls back to
	// the default in lodgerNightWindow.
	LodgingBedtimeHour int

	// LodgingDefaultWeeklyRate is the operator-set rent for a private room,
	// stored weekly (the booking/cadence unit) but billed and quoted per
	// night as LodgingNightlyRate(rate) = rate/7 — "4 a night" reads better
	// in a haggle than "28 a week", and the per-night figure is what the
	// keeper advertises. Consumed by the keeper/lodger perception rate hints,
	// the lodger affordability cue, and the engine-auto rebook sweep. 0 (or
	// any value < 7, which floors the nightly rate to 0) disables the rate
	// surfaces and the auto-rebook. Default 28 (4/night).
	LodgingDefaultWeeklyRate int

	// ShiftLatenessWindowMinutes staggers NPC arrivals at work so the whole
	// village doesn't head out on the same minute when shifts begin (the v2 port
	// of v1's per-NPC lateness_window_minutes, reshaped to one global tunable —
	// ZBBS-HOME-309). Each NPC's to-work duty is delayed by a deterministic
	// offset in [0, window) seeded by (actor id, shift-start) — see
	// shiftLatenessOffset in shift_duty.go. 0 disables (all due NPCs leave on the
	// same minute, the pre-HOME-309 behavior). Settings key:
	// shift_lateness_window_minutes. DB-configured only; no editor UI.
	ShiftLatenessWindowMinutes int

	// NPCSleepMaxDurationHours is the safety cap on an auto-bedded NPC's
	// sleep — wakeExpiredNPCSleepers clears SleepingUntil at this cap or at
	// shift-start, whichever comes first. Default 12.
	NPCSleepMaxDurationHours int

	// Constable rounds (LLM-514). How often the on-shift constable (AttrConstable)
	// leaves his post to walk a circuit of every business. A wall-clock cadence (not
	// a schedule boundary) carrying a per-carrier deterministic phase offset — see
	// ConstableRoundsDue. <= 0 disables rounds entirely (the per-feature off-switch
	// posture, like StallWearPerCoin==0). Settings key:
	// constable_rounds_interval_seconds. Live-tunable via the umbilical, persisted on
	// the checkpoint. Default 2h.
	//
	// The pace of the round is no longer the engine's to set (LLM-548): he walks
	// himself and stays where the conversation keeps him, so the old per-stop dwell
	// and quiet-window knobs are gone. This interval — how often a round is OWED —
	// is the only rounds tunable left.
	ConstableRoundsInterval time.Duration

	// Needs tunables. NeedsTickAmount is the per-hour increment magnitude
	// applied to every eligible actor. NeedThresholds carries the per-need
	// "red" boundary; TirednessCriticalThreshold is the absolute (not pct)
	// threshold at which on-shift recovery gates lift.
	// MovementFatiguePerTileX100 is fatigue per tile of movement, stored ×100.
	NeedsTickAmount            int
	NeedThresholds             NeedThresholds
	TirednessCriticalThreshold int
	MovementFatiguePerTileX100 int

	// TirednessRecoveryPerMinuteX100 is how fast tiredness drops per
	// wall-clock minute while an actor is asleep or on break, stored ×100
	// (10 → 0.1/min). 0 disables recovery. Consumed by RunTirednessRecoveryTicker.
	TirednessRecoveryPerMinuteX100 int

	// RestockReorderPct is the reorder threshold for the buy-side restock
	// producer (ZBBS-WORK-322), expressed as a whole percent of an entry's
	// personal-carry cap: a reseller's `buy` RestockEntry is "low" — and
	// warrants a restock tick — when its on-hand quantity is strictly below
	// cap * pct / 100. Default 25 (a quarter). 0 disables the producer (no
	// restock warrant ever fires), the same off-switch posture as the other
	// per-feature tunables. Integer-percent storage, not a float, matching
	// the project's _x100 / permille / pct convention. Consumed by the
	// restock producer and the "## Restocking" perception gate.
	RestockReorderPct int

	// Stall wear & repair (LLM-118; generalized to all owned businesses in
	// LLM-247). An owned business accrues Wear in proportion to the coin it turns
	// over (StallWearPerCoin × sale amount, accrued at commitPayTransfer to the
	// seller's owned business). Crossing StallWearRepairThreshold stamps a repair
	// warrant; crossing StallWearDegradeThreshold closes it for refill until mended. A
	// repair consumes StallNailsPerRepair nails and runs a SourceActivity
	// window of StallRepairDurationSeconds, then resets Wear to 0. All six are
	// live-tunable (umbilical) — the defaults are guesstimates calibrated
	// against the smith's nail output. StallWearPerCoin==0 disables wear
	// entirely (the per-feature off-switch posture).
	//
	// StallDegradedProducePct (LLM-446) is the production-rate multiplier while
	// the owner's business is degraded: production LIMPS instead of stopping, so
	// the sole producer of the repair input (the smith and his nails) can always
	// claw back enough to self-mend — a degraded business must never be a
	// terminal state. 0 restores the legacy full block (and with it the
	// self-repair deadlock — an operator's rope); 100 removes the penalty;
	// restock BUYING stays blocked at any value (the LLM-304 posture: don't
	// spend coin refilling shelves you should mend first).
	StallWearPerCoin           int
	StallWearRepairThreshold   int
	StallWearDegradeThreshold  int
	StallNailsPerRepair        int
	StallRepairDurationSeconds int
	StallDegradedProducePct    int

	// Equipment service (LLM-648; equipment_service.go). An owned business
	// accrues EquipmentUse per unit of output; at this threshold it is DUE for
	// the wright's service, which resets it. 0 disables the mechanism (no
	// accrual, nothing due). Live-tunable via the settings row.
	EquipmentServiceDueThreshold int

	// Cold exposure (LLM-412; cold.go). Per-minute ×100 rates the exposure
	// sweep applies by situation — storm accrual outdoors / under an unheated
	// roof, recovery by a lit hearth / under a clear sky — plus the night
	// multiplier on accrual and the production-rate sap while red-or-worse
	// cold. ColdStormOutdoorsPerMinuteX100==0 turns storm cold off entirely
	// (nothing ever accrues; the off-switch posture).
	ColdStormOutdoorsPerMinuteX100 int
	ColdStormIndoorsPerMinuteX100  int
	// ColdWarmGarmentPerMinuteX100 caps outdoor storm accrual for an actor carrying
	// a SOUND CapabilityWarms garment (LLM-410) — the coat-is-your-roof relief. Only
	// ever lowers the situational rate; 0 makes a warm garment full outdoor relief.
	ColdWarmGarmentPerMinuteX100 int
	// ColdThreadbareGarmentPerMinuteX100 is the same cap for a THREADBARE warms
	// garment (LLM-422) — worse relief than a sound one (the wind gets through the
	// worn cloth), so a failing coat has real cold stakes. Also min-only.
	ColdThreadbareGarmentPerMinuteX100 int
	ColdNightMultiplierX100            int
	ColdWarmRecoveryPerMinuteX100      int
	ColdClearRecoveryPerMinuteX100     int
	ColdProduceSapPct                  int

	// Garment wear (LLM-422; garment_wear.go). A working actor's in-use unit of a
	// wearable garment (item_kind.wear_minutes > 0) loses GarmentWearPerMinute
	// worked minutes per real minute; a garment worn into its last
	// GarmentThreadbareFractionX100 percent reads THREADBARE (warms less, prompts
	// replacement). GarmentWearPerMinute <= 0 disables garment wear (the
	// off-switch, mirroring StallWearPerCoin == 0).
	GarmentWearPerMinute          int
	GarmentThreadbareFractionX100 int

	// Hearth (LLM-412; hearth.go). A stoke consumes StokeWoodPerStoke firewood
	// over StokeDurationSeconds and buys HearthBurnMinutesPerWood of fire per
	// stick, banked at most HearthMaxBankMinutes ahead; a fire within
	// HearthLowMinutes of out is "low" (cue + storm warrant + stoke gate).
	HearthBurnMinutesPerWood int
	HearthMaxBankMinutes     int
	HearthLowMinutes         int
	StokeWoodPerStoke        int
	StokeDurationSeconds     int

	// Farm upkeep wealth tax (LLM-215). Each game-day a farm-tagged producer's
	// owner owes one upkeep shovel per FarmUpkeepCoinsPerShovel coins held above
	// FarmUpkeepFloor, bought from the smith (the LLM-83 circulation lever). Stock-
	// based, so there is no per-object accumulator — the obligation is a pure
	// function of the owner's coins (FarmUpkeepObligation), assessed on the daily
	// rotation boundary (assessFarmUpkeep). Live-tunable (umbilical);
	// FarmUpkeepCoinsPerShovel<=0 disables the feature entirely (the off-switch,
	// mirroring StallWearPerCoin==0). Both ride the Snapshot so the perception cue
	// derives the obligation on the same values the assessment enforces.
	FarmUpkeepFloor          int
	FarmUpkeepCoinsPerShovel int

	// Town rate (LLM-557). Each game-day every owned business owes the constable
	// TownRateCoinsPerDay in rate, accrued on the daily rotation boundary
	// (assessTownRate) and capped at TownRateMaxOwed so a keeper he cannot catch on
	// shift never faces a shock bill. Unlike the farm-upkeep levy this one carries a
	// per-object accumulator (VillageObject.RateOwed) — "have you paid today" is a
	// record, not a stock — so neither knob rides the Snapshot: the perception cues
	// read the stored balance, not a derived obligation. Live-tunable (umbilical);
	// TownRateCoinsPerDay<=0 disables further accrual (the off-switch, mirroring
	// FarmUpkeepCoinsPerShovel==0) but does NOT forgive outstanding arrears.
	TownRateCoinsPerDay int
	TownRateMaxOwed     int

	// Estate rate (LLM-652). Once a game-day every resident NPC pays
	// EstateRatePctPerDay percent of the coin held above EstateRateFloor into the
	// town chest (WorldEnvironment.TownChest), assessed on the daily rotation
	// boundary (assessEstateRate). Unlike the day's rate the ENGINE moves this
	// coin — see estate_rate.go for why. Live-tunable (umbilical);
	// EstateRatePctPerDay<=0 disables the levy (the off-switch).
	EstateRateFloor     int
	EstateRatePctPerDay int

	// Reactor evaluator tunables (Phase 2 PR 2). Settings-driven gross
	// gates — no per-call cost calculation; llm-memory-api's per-VA dollar
	// budgets (MEM-052) own the hard $ ceiling.
	//
	// ReactorJitterMin/Max: stamped at warrant time as now+jitter. Provides
	// conversational pacing (1-4s default — fires feel like turn-taking,
	// not LLM-speed turbo).
	//
	// ReactorEvaluatorCadence: how often the evaluator runs. 250ms gives
	// ±250ms timing precision around the jitter floor, which is fine for
	// conversational scale.
	//
	// MaxWarrantAge: cleared on LoadWorld; not currently used at runtime
	// (warrants are ephemeral). Kept for future use if persistence lands.
	//
	// MaxReactorTicksPerActorPerMinute: per-actor rate cap over a rolling
	// 1-minute window. Drops to 0 (disabled) by default; turn on if a
	// noisy environment produces sub-jitter ping-pong loops in practice.
	// Capped actors get their WarrantDueAt pushed to the next allowed
	// time rather than silently skipped each scan. Distinct from
	// MinReactorTickGap below — that is the always-on per-tick pacing
	// floor; this is the rolling-window ceiling.
	//
	// MaxWarrantsPerActor: cap on the per-actor Warrants list size. When
	// exceeded, oldest entries drop (freshest signals are most relevant).
	// 0 = uncapped.
	//
	// MinReactorTickGap: per-actor minimum wall-clock gap between reactor
	// ticks — an always-on pacing floor independent of the optional per-
	// minute rate cap above. Default 5s (defaultMinReactorTickGap). A
	// warrant coming due inside the gap has its WarrantDueAt pushed to the
	// gap boundary; a Force warrant bypasses it.
	//
	// AdmissionBackoff: how far the evaluator pushes an actor's
	// WarrantDueAt when tick admission control turns it away (downstream
	// worker pool at capacity). Default 250ms (defaultAdmissionBackoff) ≈
	// the evaluator cadence, so a deferred warrant is re-examined on
	// roughly the next scan. The warrants stay OPEN — a deferral consumes
	// nothing.
	//
	// TickWorkerCount: number of off-world goroutines in PR 3's tick worker
	// pool. Defaults to 1 (handlers.defaultTickWorkerCount) — a pool >1
	// gives nondeterministic cross-actor commit order, so the default must
	// not imply an ordering guarantee the system lacks. The pool derives
	// its bounded job-buffer size from this; backpressure is a feature.
	//
	// LaborReplyCadence: the minimum wall-clock gap between conversational
	// replies a laboring worker gives to NPC speech (LLM-230). A mid-job worker
	// is otherwise fully shelved from NPC-speech ticks (actorCanReactNow); this
	// lets one reply through per window so she can answer "can't stop just now,
	// I'm minding the shelves" without the pre-190 per-line babble. Default 3m
	// (defaultLaborReplyCadence). PC speech, operator nudges, and red hunger/
	// thirst still tick her immediately — the cadence bounds NPC chatter only.
	ReactorJitterMin                 time.Duration
	ReactorJitterMax                 time.Duration
	ReactorEvaluatorCadence          time.Duration
	MaxWarrantAge                    time.Duration
	MaxReactorTicksPerActorPerMinute int
	MaxWarrantsPerActor              int
	MinReactorTickGap                time.Duration
	LaborReplyCadence                time.Duration
	AdmissionBackoff                 time.Duration
	TickWorkerCount                  int

	// Weighted starvation-age fairness for shared-VA tick allocation (LLM-258).
	// The per-agent rate gate paces a shared VA (salem-vendor backs many NPCs)
	// under memory-api's per-agent limit; these govern how the limited slots are
	// shared so chatty NPCs can't starve quiet on-shift producers on the same
	// slug. Each falls back to a safe default (the agentRate* helpers in
	// reactor.go) when unset — the cap itself is unchanged, only the allocation
	// within it.
	//
	//   - AgentRateStarvationReserve: paced slots held back from chatter for
	//     starved producers (default 2). Clamped to leave >=1 general slot.
	//   - AgentRateReserveAgeThreshold: min wait since the actor's last served
	//     tick before it may claim a reserved slot (default 45s).
	//   - AgentRateStarvationCeiling: a served actor starved longer than this is
	//     admitted unconditionally — the bounded worst-case tick-latency
	//     guarantee for an on-shift producer (default 2m).
	AgentRateStarvationReserve   int
	AgentRateReserveAgeThreshold time.Duration
	AgentRateStarvationCeiling   time.Duration

	// Degeneracy observer (LLM-94, engine/sim/degeneracy.go). Detects an
	// agent stuck burning LLM ticks that accomplish nothing and damps the
	// waste. Deliberately OFF by default and tuned conservatively — it only
	// acts on obviously-egregious SUSTAINED futility.
	//
	// DegeneracyThinAfterTicks is the MASTER ENABLE plus the Stage-1
	// threshold: consecutive obviously-futile scored ticks before the actor
	// is flagged (and its driving perception thinned). <= 0 disables the
	// whole observer — the safe default, since the observer can suppress an
	// agent's ticks. The remaining three are Stage-2 (surgical wake-threshold
	// throttle) sub-knobs; each falls back to a safe default when unset.
	//
	//   - DegeneracyThrottleAfterTicks: consecutive futile ticks before the
	//     throttle (default 20).
	//   - DegeneracyThrottleMinDuration: the streak must ALSO span at least
	//     this wall-clock duration before throttling — so a fast tick burst
	//     can't trip the clamp early (default 15m).
	//   - DegeneracyThrottleBackoff: how far a throttled actor's ambient
	//     wake is pushed out (default 5m).
	DegeneracyThinAfterTicks      int
	DegeneracyThrottleAfterTicks  int
	DegeneracyThrottleMinDuration time.Duration
	DegeneracyThrottleBackoff     time.Duration

	// Oscillation arm (LLM-124, engine/sim/degeneracy.go). Layered on the
	// per-tick yield scorer: an actor shuttling between a tight set of
	// structures with no goal progress reads as futile even though each move_to
	// leg individually state-changed (the live Ezekiel Crane Blacksmith<->Tavern
	// loop the zero-yield arms missed). Active whenever the observer is enabled;
	// each knob falls back to a safe default when unset.
	//
	//   - DegeneracyOscillationWindow: scored ticks of structure history kept
	//     for the arm (default 8). The arm fires only on a full window.
	//   - DegeneracyOscillationMinTransitions: minimum structure changes within
	//     the window to count as oscillating (default 3).
	//   - DegeneracyOscillationMaxDistinct: maximum distinct structures the actor
	//     may touch and still count as a tight loop (default 2).
	DegeneracyOscillationWindow         int
	DegeneracyOscillationMinTransitions int
	DegeneracyOscillationMaxDistinct    int

	// Staleness decay for level-triggered warrants (LLM-233,
	// engine/sim/stale_wake.go). An all-ambient warrant cycle whose every
	// kind was already ticked under the actor's current situation
	// fingerprint is deferred to lastEmit + base·2^streak instead of firing
	// at full producer rate — "an unchanged situation must not re-warrant at
	// full rate."
	//
	//   - StaleWakeDecayBase: master enable + the base interval the backoff
	//     doubles from. <= 0 disables the gate (the zero-value default for
	//     directly-constructed settings; the pg loader defaults it ON at 1m).
	//     Settings key: stale_wake_decay_base_seconds.
	//   - StaleWakeDecayCap: backoff ceiling — a fully-decayed unchanged
	//     situation is still re-observed this often (default 30m). Settings
	//     key: stale_wake_decay_cap_minutes.
	StaleWakeDecayBase time.Duration
	StaleWakeDecayCap  time.Duration

	// Conversation turn-state liveness windows (ZBBS-WORK-370). How long an
	// actor's outgoing "I addressed X, awaiting their reply" edge stays live
	// before the turn-taking backstop stops suppressing a re-initiation and
	// perception drops the "wait for their reply" line — so a conversation with
	// an unresponsive party re-opens rather than locking up. Keyed on the
	// ADDRESSEE's kind (Fork 3): a human player is slow, an NPC answers at tick
	// speed. Both fall back to Default{PC,NPC}AwaitReplyWindow when zero (see
	// World.awaitReplyWindow). Settings keys: pc_await_reply_window_seconds /
	// npc_await_reply_window_seconds.
	PCAwaitReplyWindow  time.Duration
	NPCAwaitReplyWindow time.Duration

	// Idle-backstop tunables (engine/sim/cascade/idle_backstop.go). Both
	// fall back to defaults when zero, so tests that bypass the
	// environment loader get sensible behavior without seeding them.
	//
	// IdleBackstopThreshold: how long an actor must go without a reactor
	// tick before the idle-backstop sweep stamps a WarrantKindIdleBackstop
	// warrant. Default 30 min (defaultIdleBackstopThreshold in
	// reactor.go) — engine-injected liveness for actors no other warrant
	// has engaged. Production can tune up; sandbox / dev keeps the
	// default for visible behavior.
	//
	// IdleBackstopSweepInterval: how often the idle-backstop sweep walks
	// the actor list. Default 5 min (defaultIdleBackstopSweepInterval in
	// engine/sim/cascade/idle_backstop.go — owned by cascade since cascade
	// owns the goroutine driver). Detection latency ≤ this interval
	// against the threshold; oversample cost is trivial (per-actor field
	// reads on the world goroutine, no allocations).
	IdleBackstopThreshold     time.Duration
	IdleBackstopSweepInterval time.Duration

	// Red-need backstop tunables (ZBBS-HOME-363 —
	// engine/sim/red_need_backstop_commands.go +
	// engine/sim/cascade/red_need_backstop.go). The fast, cost-paced
	// companion to the hourly needs-tick re-warrant: it re-engages an
	// actor sitting on an unresolved red need that has gone idle, without
	// waiting the full hour (needs tick) or 30 min (idle backstop). All
	// fall back to defaults when zero.
	//
	//   - RedNeedBackstopBaseDelay: the first/floor re-warrant gap for a
	//     red-need idle actor. Default 90 s (defaultRedNeedBackstopBaseDelay
	//     in reactor.go) — snappy enough that a transiently-stuck actor (a
	//     keeper just returned, stock replenished) retries quickly. Doubles
	//     each sweep the need makes no progress.
	//   - RedNeedBackstopMaxDelay: the cap the exponential backoff
	//     converges to for a genuinely-unresolvable red need. Default
	//     30 min (defaultRedNeedBackstopMaxDelay) — i.e. no worse than the
	//     idle-backstop rate, bounding the steady-state LLM cost of a stuck
	//     actor.
	//   - RedNeedBackstopSweepInterval: how often the sweep walks the actor
	//     list. Default 30 s (defaultRedNeedBackstopSweepInterval in
	//     engine/sim/cascade/red_need_backstop.go — owned by cascade since
	//     cascade owns the goroutine driver). Sets the detection latency for
	//     a newly-red actor; cheap (per-actor field reads on the world
	//     goroutine, no allocations).
	RedNeedBackstopBaseDelay     time.Duration
	RedNeedBackstopMaxDelay      time.Duration
	RedNeedBackstopSweepInterval time.Duration

	// AtmosphereRefreshInterval is the cadence at which the atmosphere
	// refresh cascade slice fires a salem-generic LLM call to rewrite
	// World.Environment.Atmosphere. Default 4h
	// (defaultAtmosphereRefreshInterval in
	// engine/sim/cascade/atmosphere.go — owned by cascade since cascade
	// owns the goroutine driver). Settings-driven from day one so dev /
	// staging can tune it down for testing without rebuilding.
	AtmosphereRefreshInterval time.Duration

	// Storm weather cascade tunables (engine/sim/weather.go +
	// engine/sim/cascade/storm.go — LLM-117 Half A). Both fall back to
	// the cascade-owned defaults (defaultStormInterval /
	// defaultStormDuration) when zero, so a test or a fresh world that
	// bypasses the environment loader still gets sane behavior.
	//
	//   - StormInterval: the gap between automatic storms, measured from
	//     the last weather change (clear → storm). Default 3h. Settings-
	//     driven so dev / staging can tune it down to seconds for testing
	//     without a rebuild (same posture as AtmosphereRefreshInterval).
	//   - StormDuration: how long an automatic storm holds before it
	//     clears (storm → clear). Default 15m.
	StormInterval time.Duration
	StormDuration time.Duration

	// Action-log substrate tunables (engine/sim/action_log.go +
	// engine/sim/cascade/action_log.go). Both fall back to defaults
	// when zero, so tests that bypass the environment loader get
	// sensible behavior without seeding them.
	//
	// ActionLogRetention: how far back the in-memory action log
	// keeps entries. Compaction sweep drops entries with OccurredAt
	// before (now - retention). Default 48h
	// (sim.DefaultActionLogRetention) — covers atmosphere's 4h refresh
	// interval with headroom and consolidation's expected 24h window
	// cleanly. Dev / staging can tune down.
	//
	// ActionLogSweepInterval: how often the compaction sweep fires.
	// Default 1h (defaultActionLogSweepInterval in
	// engine/sim/cascade/action_log.go — owned by cascade since
	// cascade owns the goroutine driver). Stale entries past
	// retention are still tens of hours old; the sweep cadence just
	// controls how promptly memory is reclaimed.
	ActionLogRetention     time.Duration
	ActionLogSweepInterval time.Duration

	// Per-pair conversational recency tunables (engine/sim/contact_ledger.go).
	// Both fall back to their Default* constants when zero.
	//
	//   - ContactBrakeWindow: how recently a conversation must have happened
	//     for the scene to read "you have already had your word with them"
	//     rather than offering it as history to build on. Default 2h.
	//   - ContactRecallHorizon: how far back a conversation is remembered at
	//     all; past it the pair reads as strangers and the row is dropped at
	//     load. Default 8h.
	ContactBrakeWindow   time.Duration
	ContactRecallHorizon time.Duration

	// CoinRecordWindow is how far back coin between a pair is remembered for the
	// "## Coin between you" cue (engine/sim/coin_record.go, LLM-572). Falls back
	// to DefaultCoinRecordWindow (7 days) when zero.
	CoinRecordWindow time.Duration

	// Visitor cascade tunables (engine/sim/visitor.go +
	// engine/sim/cascade/visitor.go). All fall back to *Default constants
	// in engine/sim/visitor.go when zero, so tests that bypass the
	// environment loader get sensible behavior without seeding them.
	//
	// The three spawn-roll chances (LLM-626, replacing the retired master
	// visitor_spawn_chance_permille + visitor_passer_through_chance_permille
	// pair): per-tick per-thousand probabilities, all defaulting 0 = that
	// flow off (the Phase-1 no-op posture; all three at 0 is the halt-spawn
	// dial). At VisitorTickInterval = 60s a value of ~10-30 on a flow
	// produces roughly one such visitor per game day.
	//
	// VisitorMerchantTrickleChancePermille: the IN-BAND merchant roll — a
	// grounded trader arrives on ordinary business, direction picked by
	// the VisitorSellWeightPermille weighted random.
	//
	// VisitorMerchantCorrectionChancePermille: the OUT-OF-BAND merchant
	// roll — resident coin at/above VisitorCoinBandHigh forces a SELLER
	// (drain), at/below the low band a BUYER (inject). Operators size it
	// hotter than the trickle so an out-of-band state resolves in hours.
	//
	// VisitorPasserSpawnChancePermille: the independent flavor roll — a
	// passer-through arrives on its own cadence, no longer a share carved
	// out of the merchant pipeline.
	//
	// VisitorMaxConcurrent: cap on simultaneous visitors. Zero or unset
	// falls back to DefaultVisitorMaxConcurrent (2).
	//
	// VisitorMinStayMinutes / VisitorMaxStayMinutes: stay-window bounds.
	// Concrete stay is a uniform random pull from [min, max] at spawn.
	// Defaults 240 / 1440 (4h floor, 24h ceiling).
	//
	// VisitorTickInterval: how often the cascade slice runs its
	// dispatchers (despawn → cleanup → pacing → spawn). Default 60s —
	// matches v1's runServerTickOnce cadence.
	VisitorMerchantTrickleChancePermille    int
	VisitorMerchantCorrectionChancePermille int
	VisitorPasserSpawnChancePermille        int
	VisitorMaxConcurrent                    int
	VisitorMinStayMinutes                   int
	VisitorMaxStayMinutes                   int
	VisitorTickInterval                     time.Duration

	// VisitorReturnMinDays / VisitorReturnMaxDays: when a promoted returner
	// (LLM-372) departs, next_return_at is set a uniform-random number of
	// wall-clock days in [min, max] out — long enough that the absence reads as
	// "across the seasons." Both fall back to DefaultVisitorReturnMinDays /
	// MaxDays (14 / 45) when zero, so a test or a fresh DB gets a sane rhythm; a
	// live run can shorten them to see returns sooner. Settings keys
	// visitor_return_min_days / visitor_return_max_days.
	VisitorReturnMinDays int
	VisitorReturnMaxDays int

	// Factor pack + purse (LLM-410). A wholesale factor spawns carrying a bale of
	// imported cloth/charms to sell (VisitorFactorPackUnits of each factorWareKind) and a
	// purse pulled from [VisitorFactorPurseMin, VisitorFactorPurseMax] — heavier than an
	// ordinary traveler's 30..50 so he can buy the village's surplus and inject coin. All
	// fall back to their Default* consts (2 / 120 / 200) when zero/unset, and are clamped
	// against misconfig at spawn. Settings keys visitor_factor_pack_units /
	// visitor_factor_purse_min / visitor_factor_purse_max.
	VisitorFactorPackUnits int
	VisitorFactorPurseMin  int
	VisitorFactorPurseMax  int

	// VisitorFactorIronUnits (LLM-442): bars of iron per factor visit, seeded
	// separately from the uniform per-kind pack quantity because iron is a
	// production INPUT consumed batch-by-batch (1 bar boosts one nail batch),
	// not a durable garment — at the clothing-scale 2-per-visit, the rare
	// factor (14–45 day visitor return cooldowns) could never keep the forge
	// supplied and the rough-nails fallback would become the everyday path.
	// Falls back to DefaultVisitorFactorIronUnits when zero/unset; settings
	// key visitor_factor_iron_units.
	VisitorFactorIronUnits int

	// VisitorFactorSaltUnits (LLM-444): sacks of salt per factor visit, seeded
	// separately from the uniform per-kind pack quantity for the same reason as
	// iron — salt is a cooking INPUT consumed batch-by-batch across several
	// kitchens (1 sack boosts one dish batch), not a durable garment. At the
	// clothing-scale 2-per-visit the rare factor (14–45 day visitor return
	// cooldowns) could never keep the kitchens supplied, so the salt cue would
	// sit silent most of the time and the coin drain barely fire. Unlike iron,
	// undershooting is harmless (nothing is gated — cooking just goes unboosted).
	// Falls back to DefaultVisitorFactorSaltUnits when zero/unset; settings key
	// visitor_factor_salt_units.
	VisitorFactorSaltUnits int

	// VisitorFactorThreadUnits (LLM-625): spools of thread per factor visit,
	// a shipment-scale quantity for the iron/salt reason — thread is the
	// mender's consumable (1 spool per wardrobe mend at the mending shop), so
	// the rare factor visit must land enough to bridge the village's mending
	// between calls. Like salt, undershooting is soft: mending just goes
	// unoffered until the next visit. Falls back to
	// DefaultVisitorFactorThreadUnits when zero/unset; settings key
	// visitor_factor_thread_units.
	VisitorFactorThreadUnits int

	// Coin-valve band (LLM-455). A merchant visitor's trade direction — buy (pays the
	// village, injects coin) vs sell (the factor; the village pays him, drains coin) — is
	// biased by the resident money supply (residentCoinOnMap) against this operator band:
	// resident coin at/above VisitorCoinBandHigh forces a SELLER (drain the excess),
	// at/below VisitorCoinBandLow forces a BUYER (inject), in-band leaves it to the weighted
	// random below. Both default 0 = "band unconfigured" — no active money-supply
	// management, so direction is always the weighted random (which keeps sellers a minority,
	// so import cadence stays ~today's). Set a high-water mark to turn the drain on. Settings
	// keys visitor_coin_band_low / visitor_coin_band_high.
	VisitorCoinBandLow  int
	VisitorCoinBandHigh int

	// VisitorSellWeightPermille (LLM-455): the per-thousand probability that an in-band (or
	// band-unconfigured) merchant visitor is a SELLER (the factor) rather than a buyer.
	// Deliberately low — a seller drops a full import shipment (cloth/iron/salt), and
	// pack-magnitude scaling is deferred, so common sellers would balloon imports; keeping it
	// a minority pins the factor cadence near today's while buyers become the common merchant.
	// The DEFAULT (150) is applied by the settings loaders (repo/pg + repo/mem) when the row is
	// ABSENT; the use site (effectiveVisitorSellWeight) only clamps to [0,1000], so an explicit
	// 0 means "no sellers" and a direct-constructed world with the field left zero gets 0 (not
	// the default). Settings key visitor_sell_weight_permille.
	VisitorSellWeightPermille int

	// Businessowner cascade tunables (engine/sim/businessowner.go +
	// engine/sim/cascade/businessowner.go). Both fall back to
	// *Default constants when zero, so tests that bypass the
	// environment loader get sensible behavior without seeding them.
	//
	// BusinessownerGreetCooldownMinutes: per-(keeper, customer) gap
	// between engine-spoken greet lines. Default 30 min — covers "the
	// customer popped out for an errand and came back" with a re-greet
	// on the second visit, but suppresses the redundant "welcome friend"
	// when the same customer rejoins the huddle ten seconds later after
	// stepping outside to fetch something.
	//
	// BusinessownerFarewellCooldownMinutes: mirrors the greet cooldown.
	// Same UX reasoning.
	//
	// Handover (OrderDelivered) has no cooldown by design — every
	// transaction deserves a verbal handover line.
	BusinessownerGreetCooldownMinutes    int
	BusinessownerFarewellCooldownMinutes int

	// DefaultOutdoorSceneRadius is the conversational radius used by
	// SceneBoundArea when callers don't specify one explicitly. Measured
	// in king's-move (Chebyshev) tiles around the bound's Anchor.
	// normalizeOutdoorSceneRadius applies the default and the bounds
	// clamp at LoadWorld:
	//   - 0 / unset / negative → DefaultOutdoorSceneRadiusValue (3 tiles)
	//   - above DefaultOutdoorSceneRadiusMax (10) → clamped to max
	DefaultOutdoorSceneRadius int

	// Scene-quote substrate tunables (Phase 3 PR S3). Both fall back to
	// scene_quote.go's *Default constants when zero, so tests that
	// bypass the environment loader get sensible behavior without
	// seeding them.
	//
	// SceneQuoteTTL: how long a freshly minted quote stays Active before
	// the aging sweep flips it Expired. Default 10 min — asymmetric
	// (longer) with the pay-ledger pending TTL (2-5 min) since a
	// quote is a passive ad rather than a staked offer.
	//
	// SceneQuoteSweepCadence: how often the aging sweep scans
	// World.Quotes for expired entries. Default 60s — gives ±60s expiry
	// latency against the 10-min TTL, invisible at gameplay scale.
	SceneQuoteTTL          time.Duration
	SceneQuoteSweepCadence time.Duration

	// Pay-ledger substrate tunables (Phase 3 PR S4). Both fall back to
	// pay_ledger.go's *Default constants when zero. Shorter TTL than
	// SceneQuoteTTL — a pending pay offer has the buyer staked into a
	// social moment, which decays faster than a passive quote ad does.
	//
	// PayLedgerTTL: how long a freshly minted pending entry stays
	// Pending before the aging sweep flips it Expired. Default 3 min —
	// middle of architecture § 3's 2-5 minute range.
	//
	// PayLedgerSweepCadence: how often the aging sweep scans
	// World.PayLedger for expired pending entries. Default 60s —
	// matches the scene-quote sweep cadence so admin tuning sees one
	// mental model.
	//
	// PayLedgerTerminalRetention: how long a terminal entry lingers in
	// World.PayLedger before the sweep reaps it. Bounds the otherwise-
	// unbounded growth of the offer-side map (terminal entries are never
	// otherwise removed). Default 1h; floored at PayLedgerInResponseToWindow
	// so a countered parent is never reaped while still referenceable via
	// in_response_to. See pay_ledger.go.
	PayLedgerTTL               time.Duration
	PayLedgerSweepCadence      time.Duration
	PayLedgerTerminalRetention time.Duration

	// Order substrate tunables (Phase 3 PR S6). Both fall back to
	// order.go's *Default constants when zero. The order TTL is the
	// post-acceptance fulfillment window — longer than PayLedgerTTL
	// since at this stage the buyer has already committed (coins
	// debited) and we want plenty of room for the seller's reactor
	// to fire and deliver.
	//
	// OrderTTL: how long an Order at OrderStateReady sits before
	// the aging sweep flips it OrderStateExpired. Default 10 min.
	//
	// OrderSweepCadence: how often the aging sweep scans World.Orders
	// for expired entries. Default 60s — matches the PayLedger and
	// SceneQuote sweep cadences.
	OrderTTL          time.Duration
	OrderSweepCadence time.Duration

	// PCPresenceStaleAfter is how long a PC may go without a presence stamp
	// before the presence sweep treats it as an absent ghost (ZBBS-WORK-326;
	// signal moved to the WS heartbeat in LLM-342). The server re-stamps every
	// PCPresenceHeartbeatInterval (15s) while the socket is up, so the default
	// (40s) rides out a missed heartbeat or brief blip while still clearing a
	// dropped socket quickly. Falls back to DefaultPCPresenceStaleAfter when zero/unset (read
	// via PCPresenceStaleAfter); tunable via the pc_presence_stale_seconds
	// setting.
	PCPresenceStaleAfter time.Duration

	// Huddle silence-conclusion tunables (ZBBS-HOME-417). A staffed
	// structure's huddle has no last-member-leave path (the keeper is always
	// present), so the silence sweep is the only routine conclusion: a huddle
	// idle past HuddleSilenceTimeout is concluded, which also re-keys its
	// conversation_id for the next exchange.
	//
	// HuddleSilenceTimeout: how long a huddle may go with no spoken line,
	// join, or completed transaction before the sweep concludes it. Default
	// 2h (HuddleSilenceTimeoutDefault) — long enough that a returning patron
	// resumes the same conversation rather than a fresh one, short enough that
	// a structure's day breaks into per-session conversations instead of one
	// multi-day blob. Tunable via huddle_silence_timeout_minutes (minutes,
	// matching the scene-quote / pay-ledger / order TTL convention).
	//
	// HuddleSilenceSweepCadence: how often the sweep scans World.Huddles.
	// Default 60s (HuddleSilenceSweepCadenceDefault) — matches the pay-ledger
	// / scene-quote / order sweeps so admin tuning sees one mental model.
	HuddleSilenceTimeout      time.Duration
	HuddleSilenceSweepCadence time.Duration

	// HuddleLiveWindow is how recently a huddle must have seen activity — a
	// spoken line, a join, a completed transaction — to still count as a LIVE
	// conversation for the noop-skip preflight (LLM-467). It is deliberately far
	// shorter than HuddleSilenceTimeout above: the sweep's 2h answers "when is
	// this conversation over", which wants to be generous so a returning patron
	// resumes rather than restarts, while this answers "is anyone still talking",
	// which wants to be tight. Between the two, every member of a finished-but-
	// unswept huddle was paying a full LLM call per idle backstop to rediscover
	// it had nothing to say. Falls back to HuddleLiveWindowDefault when zero/unset
	// (read via HuddleIsLive); tunable via huddle_live_window_seconds.
	HuddleLiveWindow time.Duration

	// Huddle loop-conclusion tunables (LLM-159). The silence sweep concludes a
	// DORMANT huddle; this concludes the inverse — a huddle that is hyper-active
	// but going nowhere: members repeating near-identical lines ("let's go to the
	// market" x50, never moving), burning an LLM tick every few seconds. It is
	// the degeneracy observer's structural blind spot — every speak succeeds, an
	// audience is present, and no one moves, so the per-actor observer scores
	// every tick productive and never flags it.
	//
	// HuddleLoopTimeout is the MASTER ENABLE plus the persistence gate: a huddle
	// must stay in a high-repetition, progress-free conversation for at least
	// this long before the sweep concludes it. <= 0 disables the whole loop sweep
	// — the safe default, since concluding a LIVE conversation is heavier than
	// the silence sweep's dormant-conclude. Mirrors DegeneracyThinAfterTicks's
	// "one positive number both enables and tunes" posture. Tunable via
	// huddle_loop_timeout_seconds.
	//
	// HuddleLoopRepeatPercent is the repetition threshold (0-100): the percent of
	// the huddle's content-bearing recent turns (filler-only lines like "Yes."
	// are excluded from the count) that must be near-duplicates of another turn
	// for the conversation to read as looping. Default 60
	// (HuddleLoopRepeatPercentDefault). Tunable via huddle_loop_repeat_percent.
	//
	// HuddleLoopSweepCadence is how often the sweep scans World.Huddles. Default
	// 30s (HuddleLoopSweepCadenceDefault) — finer than the silence sweep's 60s
	// because the persistence gate is minutes, not hours. Tunable via
	// huddle_loop_sweep_cadence_seconds.
	//
	// HuddleLoopMaxTurns is the endurance arm's turn budget (LLM-333): spoken
	// lines a huddle may accumulate with no progress event before it reads as
	// stuck regardless of wording — the content-blind arm that catches the
	// paraphrase loops the repetition metric measurably cannot (0.00 vs the
	// 0.60 threshold on the live farewell loop). <= 0 falls back to
	// HuddleLoopMaxTurnsDefault (16); rides the sweep's master enable. Tunable
	// via huddle_loop_max_turns.
	//
	// HuddleConversationWindDown is the lingering arm's clock (LLM-397): how long
	// a CONVERSATION may run — measured on Huddle.ConversationSince, which
	// survives the huddle churn — before the wind-down steer arms and the
	// persistence gate starts running toward a silent conclude. It is the only
	// arm that can see a healthy, productive conversation (the others all require
	// a pathology: repetition, a dead deal, or turns spent on nothing), and the
	// steer is its point — the conclude one HuddleLoopTimeout later is just the
	// backstop for a scene that won't close itself. <= 0 falls back to
	// HuddleConversationWindDownDefault (12m); rides the sweep's master enable.
	// Tunable via huddle_conversation_wind_down_seconds.
	HuddleLoopTimeout          time.Duration
	HuddleLoopRepeatPercent    int
	HuddleLoopSweepCadence     time.Duration
	HuddleLoopMaxTurns         int
	HuddleConversationWindDown time.Duration

	// HuddleContinuityWindow is how long after a structure huddle concludes a
	// re-formation among the same speakers still counts as the SAME conversation
	// (LLM-170). Within it, a new huddle at that structure inherits the prior
	// conversation's recent-utterance ring (no cross-huddle re-greeting) and loop
	// state (so churn can't evade the loop sweep). Default 5m
	// (HuddleContinuityWindowDefault) — spans the observed Walker churn cycle.
	// Tunable via huddle_continuity_window_seconds. Unlike the loop sweep this is
	// ON by default: the ring carry-over is pure perception legibility, and the
	// loop-state carry is inert unless HuddleLoopTimeout enables the sweep.
	HuddleContinuityWindow time.Duration

	// SeekWorkCoinCeiling is the wealth shelf (LLM-194): a workless worker stops
	// seeking/soliciting work once its coins reach this value, becoming a plain idle
	// villager that drains its purse via ordinary consumption until it dips under the
	// ceiling and re-enters the labor market. <= 0 falls back to
	// SeekWorkCoinCeilingDefault (25) via effectiveSeekWorkCoinCeiling — a zero ceiling
	// would suppress seek-work for everyone. Live-tunable + persisted
	// (settings/seek-work-ceiling, read side GET /settings). Mirrored onto the
	// snapshot at publish so the perception gates read it without racing on w.Settings.
	SeekWorkCoinCeiling int

	// SeekWorkNeedYieldMargin is the width, below each need's red-line threshold, of
	// the upper-felt band in which the seek-work backstop redirects a resolvable-need
	// worker to eat/drink instead of hunting odd jobs (LLM-276). A workless idle worker
	// whose hunger/thirst sits in [threshold-margin, threshold) and can resolve it now
	// is woken with a tend-need felt impulse rather than the seek-work one. <= 0 falls
	// back to SeekWorkNeedYieldMarginDefault (5) via effectiveSeekWorkNeedYieldMargin.
	// Live-tunable + persisted (settings/seek-work-need-margin, read side GET /settings).
	// Engine-side only — perception keys the matching directory suppression + need-
	// redirect off the stamped tend-need warrant, so no snapshot mirror is needed.
	SeekWorkNeedYieldMargin int

	// LaborProduceBoostPct is the per-worker production boost (LLM-224): each hired
	// worker laboring at the keeper's establishment adds this percent of the keeper's
	// own base rate to the produce tick (50 → one helper makes goods arrive 1.5x as
	// fast). <= 0 disables the boost — the per-feature off-switch, mirroring
	// FarmUpkeepCoinsPerShovel==0 (the pg loader seeds DefaultLaborProduceBoostPct
	// when the setting key is absent). Live-tunable + persisted
	// (settings/labor-produce-boost, read side GET /settings). Engine-side only —
	// perception deliberately carries no hire-value pitch (they hire willingly
	// without one; the experiential after-the-fact beat is a separate ticket).
	LaborProduceBoostPct int

	// MerchantCoinFloor is the working-capital floor (LLM-294): a keeper whose purse
	// dips below this AND is sitting on unsold sellable stock is steered to conserve
	// coin (hold off buying, sell down its shelves) rather than restock. Unlike the
	// seek-work knobs there is no effective-value fallback — the pg loader seeds
	// MerchantCoinFloorDefault when the key is absent, and an explicit 0 STICKS and
	// disables the gate (the off-switch, mirroring FarmUpkeepCoinsPerShovel==0 /
	// LaborProduceBoostPct==0). Live-tunable + persisted (settings/merchant-coin-floor,
	// read side GET /settings). Mirrored raw onto the snapshot at publish so the
	// perception gates read it without racing on w.Settings; a 0 there (incl. a
	// directly-constructed test snapshot) means the feature is off.
	MerchantCoinFloor int

	// EcoEnabled is eco mode's master switch (LLM-313): when true and no player
	// character has a fresh presence stamp (AudienceActive), the reactor paces
	// social/economy warrant cycles by EcoSocialGap/EcoEconomyGap, the plain idle
	// backstop stops stamping, and visitor spawning pauses. Survival, duty, and
	// commerce-commitment warrants are never slowed (see ecoWarrantGap). The pg
	// loader seeds true when the eco_enabled key is absent; an explicit false
	// STICKS. Live-tunable + persisted (settings/eco-mode, read side GET
	// /settings). Engine-side only — perception never sees eco state.
	EcoEnabled bool

	// EcoSocialGap is the per-actor pacing floor for a social-bucket warrant
	// cycle (npc_spoke, huddle beats) while eco mode is engaged. 0 disables the
	// social throttle; a negative value never persists (SetEcoMode validates)
	// and reads as the default. Seeded DefaultEcoSocialGap when the
	// eco_social_gap_seconds key is absent.
	EcoSocialGap time.Duration

	// EcoEconomyGap is EcoSocialGap's economy-bucket twin (restock, production
	// choice, farm upkeep, stall repair, seek work). Seeded
	// DefaultEcoEconomyGap when the eco_economy_gap_seconds key is absent.
	EcoEconomyGap time.Duration

	// PCAudienceIdleAfter is how long a connected client may go without any
	// player input before it stops counting as an audience and the candle prompt
	// asks whether anyone is there (LLM-466, pc_idle_audience.go). Seeded
	// DefaultPCAudienceIdleAfter (1h) when the eco_audience_idle_seconds key is
	// absent; 0 reads as the default rather than "never idle", so a missing or
	// zeroed row can't silently restore the always-an-audience bug. Live-tunable
	// + persisted (settings/eco-mode) chiefly so a live verification doesn't have
	// to sit idle for an hour.
	PCAudienceIdleAfter time.Duration

	// SettingWarnings records settings that were out of range at load and clamped
	// to a safe bound (LLM-439). Populated by the pg settings loader — today the
	// cold rate knobs, each of which must be >= 0 (a negative recovery rate would
	// otherwise flip recovery into accrual). Surfaced on the umbilical /settings
	// route so an operator sees a misconfigured value rather than a silently
	// corrected one; NOT checkpoint-persisted — it is regenerated at every boot
	// from the (durable) stored value, so it survives the village's frequent
	// restarts for as long as the misconfiguration itself does. Empty when every
	// setting loaded in range (the common case).
	SettingWarnings []SettingWarning
}

// SettingWarning records one WorldSettings value that was out of range at load
// and clamped to a safe bound (LLM-439). The clamp keeps the always-live village
// booting on a fat-fingered config while the warning — carried to the umbilical
// /settings surface — makes the bad value visible. Regenerated at every boot, so
// it persists across restarts as long as the stored value stays out of range.
type SettingWarning struct {
	Key     string `json:"key"`     // the setting-table key
	Raw     int    `json:"raw"`     // the out-of-range stored value
	Clamped int    `json:"clamped"` // the safe value the engine is using instead
	Reason  string `json:"reason"`  // plain-English why, for an operator reading it cold
}

// DefaultOutdoorSceneRadiusValue is the fallback radius used when
// callers don't specify one. 3 tiles is a reasonable "stop-and-chat"
// distance on the village grid.
const DefaultOutdoorSceneRadiusValue = 3

// DefaultOutdoorSceneRadiusMax caps the configured radius. Larger
// values are clamped down at LoadWorld. Conversational radii beyond
// 10 tiles are unlikely to reflect "people standing close enough to
// chat" — the cap is a sanity floor, not a hard physics constraint.
const DefaultOutdoorSceneRadiusMax = 10

// normalizeOutdoorSceneRadius applies the default + clamp to the
// settings at load time. Called from LoadWorld after the environment
// loader returns. Unexported by design.
func normalizeOutdoorSceneRadius(s *WorldSettings) {
	if s == nil {
		return
	}
	switch {
	case s.DefaultOutdoorSceneRadius <= 0:
		s.DefaultOutdoorSceneRadius = DefaultOutdoorSceneRadiusValue
	case s.DefaultOutdoorSceneRadius > DefaultOutdoorSceneRadiusMax:
		s.DefaultOutdoorSceneRadius = DefaultOutdoorSceneRadiusMax
	}
}

// SpeechHelper is the generic-dialogue pool. Pull(type, fromActor, toActor)
// returns a line for a typed scenario; both actors nullable. v1 ignores
// actors and selects randomly; future context-aware selection becomes a
// helper-internal change (callsites already wire both actors through).
//
// TODO: port from scattered hardcoded line arrays + per-tick LLM generic
// speech during speech subsystem port.
type SpeechHelper struct{}

// reactorEvaluatorState carries the coalescing flag that gates the
// AfterFunc self-rearm chain. Owned by the world (mutated only from the
// world goroutine), exposed to the timer callback that drives the next
// evaluation. No mutex needed — the flag is read/written exclusively from
// inside Command.Fn.
type reactorEvaluatorState struct {
	scheduled bool
}

// locomotionTickerState carries the coalescing flag for the locomotion
// ticker's AfterFunc self-rearm chain (Phase 2 PR 4). Same shape and
// rules as reactorEvaluatorState — read/written exclusively from inside
// Command.Fn, so no mutex.
type locomotionTickerState struct {
	scheduled bool
}

// sceneQuoteSweepState carries the coalescing flag for the scene-quote
// aging sweep's AfterFunc self-rearm chain (Phase 3 PR S3). Same shape
// and rules as locomotionTickerState — read/written exclusively from
// inside Command.Fn.
type sceneQuoteSweepState struct {
	scheduled bool
}

// payLedgerSweepState carries the coalescing flag for the pay-ledger
// aging sweep's AfterFunc self-rearm chain (Phase 3 PR S4 step 8).
// Same shape and rules as sceneQuoteSweepState.
type payLedgerSweepState struct {
	scheduled bool
}

// huddleSilenceSweepState carries the coalescing flag for the huddle
// silence-conclusion sweep's AfterFunc self-rearm chain (ZBBS-HOME-417).
// Same shape and rules as payLedgerSweepState.
type huddleSilenceSweepState struct {
	scheduled bool
}

// huddleLoopSweepState carries the coalescing flag for the huddle
// loop-conclusion sweep's AfterFunc self-rearm chain (LLM-159). Same shape
// and rules as payLedgerSweepState.
type huddleLoopSweepState struct {
	scheduled bool
}

// ecoConcludeSweepState carries the coalescing flag for the eco
// conversation-arc sweep's AfterFunc self-rearm chain (LLM-334). Same shape
// and rules as payLedgerSweepState.
type ecoConcludeSweepState struct {
	scheduled bool
}

// orderSweepState carries the coalescing flag for the Order aging
// sweep's AfterFunc self-rearm chain (Phase 3 PR S6). Same shape
// and rules as payLedgerSweepState.
type orderSweepState struct {
	scheduled bool
}

// laborLedgerSweepState carries the coalescing flag for the labor-ledger
// sweep's AfterFunc self-rearm chain (LLM-26). Same shape and rules as
// payLedgerSweepState — the labor sweep does double duty (expire pending
// offers AND settle completed work windows) but the scheduling machinery
// is identical.
type laborLedgerSweepState struct {
	scheduled bool
}

// World is the in-memory state of one realm's simulation. A single
// goroutine (started by World.Run) owns all mutable fields below — every
// mutation must go through the cmds channel. Readers consume the published
// Snapshot via atomic.Pointer (World.Published).
//
// Per design: zero locks, zero races by construction.
type World struct {
	// Primary state — source of truth.
	Actors         map[ActorID]*Actor
	Structures     map[StructureID]*Structure
	Huddles        map[HuddleID]*Huddle
	Scenes         map[SceneID]*Scene
	Orders         map[OrderID]*Order
	VillageObjects map[VillageObjectID]*VillageObject

	// HomeBakes holds the single shared evening bake in progress at each home
	// structure (LLM-454), keyed by home StructureID. TRANSIENT — deliberately NOT
	// checkpointed: a restart drops it (and the participants' SourceActivity), which
	// forfeits nothing because bake flour is consumed only at completion, so the
	// household just re-forms the bake and still finishes by bedtime.
	HomeBakes map[StructureID]*HomeBake

	// Quotes is the world-level flat map of all SceneQuotes (active and
	// terminal). Keyed by QuoteID — the LLM-visible uint64 the buyer
	// references in pay_with_item(quote_id=N, ...) at fast-path time.
	// Mirrored by a per-scene reverse index at Scene.QuoteIDs (rebuilt
	// at LoadWorld from this map; the canonical entries live here).
	//
	// Phase 3 PR S3 substrate. No checkpoint persistence layer, and
	// none planned — pending quotes are intentionally restart-lossy
	// (decided 2026-05-20 — see work/tasks/payledger-restart-lossy/decision).
	// NewWorld / LoadWorld both start with an empty Quotes map, which
	// IS the intended restart behavior: a pending quote crossing a
	// restart should re-emit fresh via the next scene_quote tool call,
	// not be resurrected with stale ExpiresAt.
	Quotes map[QuoteID]*SceneQuote

	// PayLedger is the world-level flat map of all PayLedgerEntries
	// (pending and terminal). Keyed by LedgerID — the LLM-visible
	// uint64 the seller references in accept_pay / decline_pay /
	// counter_pay, and the buyer references in withdraw_pay /
	// pay_with_item(in_response_to=N).
	//
	// Phase 3 PR S4 substrate. Sole source of truth for the offer-side
	// state machine — there is no durable backing at all. Pending
	// pay_ledger entries are intentionally restart-lossy (decided
	// 2026-05-20 — see work/tasks/payledger-restart-lossy/decision):
	// no PayLedgerRepo, no projection sink, and pg.SaveWorld does not
	// checkpoint pending entries. NewWorld / LoadWorld both start with
	// an empty PayLedger map and stay that way until live commerce
	// mints fresh entries; the LoadWorld restart re-stamp pass is
	// dormant by design (nothing to re-stamp). Accepted entries that
	// became Orders persist separately via OrdersRepo on the shared
	// pay_ledger table.
	PayLedger map[LedgerID]*PayLedgerEntry

	// LaborLedger is the world-level flat map of all LaborOffers (LLM-26),
	// pending and terminal. Keyed by LaborID — the LLM-visible uint64 the
	// employer references in accept_work / decline_work. Sole source of truth
	// for the labor offer-side state machine; like PayLedger it has no durable
	// backing and is intentionally restart-lossy (the same 2026-05-20 call).
	// NewWorld / LoadWorld start it empty and it stays empty until a worker
	// solicits — there is no labor table to re-stamp on load. Restart-loss is
	// clean: no coins are ever held (the reward only moves at completion), so a
	// lost offer is just a deal that didn't happen. See labor_ledger.go.
	LaborLedger map[LaborID]*LaborOffer

	// ContactLedger is the per-pair conversational recency trail (LLM-547),
	// keyed subject → peer → the times they were in a conversation together.
	// Written on the Speak commit path for every huddle peer in both
	// directions; read by perception to tell an actor who it has already had
	// its word with. Covers EVERY actor kind — PCs and visitors included —
	// which is why it is separate from actor.Relationships (shared-VA only,
	// visitors skipped) rather than an extension of it.
	//
	// Durable, like RecurringVisitors and unlike PayLedger / LaborLedger:
	// checkpointed and rehydrated at boot, pruned to the recall horizon at load
	// rather than by a sweep. See contact_ledger.go for why restart-loss was
	// rejected (LLM-546).
	ContactLedger map[ActorID]map[ActorID]*ContactRecord

	// CoinRecord is the per-pair coin tally (LLM-572), keyed subject → peer →
	// what each has handed the other inside CoinRecordWindow. Written from the
	// cascade action-log subscribers on every settled payment; read by perception
	// so a money claim can be checked against the record in the scene where it is
	// made. Covers every actor kind, like ContactLedger and for the same reason.
	//
	// Neither checkpointed nor restart-lossy: it is SEEDED AT BOOT from the
	// durable agent_action_log rows the engine already writes, so it survives a
	// restart without a table of its own. See coin_record.go.
	CoinRecord map[ActorID]map[ActorID]*CoinPairRecord

	// RecurringVisitors is the durable set of memorable returners (LLM-372) —
	// promoted travelers who dealt with a player and come back across the seasons.
	// Keyed by the stable rvis-<8hex> id. UNLIKE most world maps this is genuinely
	// durable, not restart-lossy: loaded from the recurring_visitor tables at boot
	// (FinalizeLoad), mutated in memory (promotion on ActorMet, return scheduling on
	// departure), and re-persisted every checkpoint (RecurringVisitorsRepo — plain
	// upsert, NO generation-marker sweep, since a returner outlives the visit). The
	// legitimate durable case per GUIDELINES: survives restart AND fires a return
	// days-to-weeks out. See recurring_visitor.go.
	RecurringVisitors map[RecurringVisitorID]*RecurringVisitor

	// BusinessownerCooldowns is the per-(speaker, listener, trigger) gap
	// map used by the businessowner cascade slice to suppress redundant
	// engine-spoken hospitality lines (e.g. don't re-greet the same
	// customer who just popped out and came back in seconds). Lazy-
	// allocated on first stamp; nil-readable as empty. World-goroutine-
	// only; restart-loss is acceptable (first-greet on re-encounter post-
	// restart is a UX wrinkle, not a correctness failure).
	BusinessownerCooldowns map[BusinessownerCooldownKey]time.Time

	// BusinessownerSpeechAt stamps the last engine-authored hospitality
	// speech instant per keeper actor. Consulted by actorCanReactNow to
	// suppress an LLM follow-up tick on the same triggering event for
	// businessownerEngineSpeechSuppressionTTL (5s). Lazy-allocated on
	// first stamp; nil-readable as empty. World-goroutine-only; restart-
	// loss is acceptable (the in-flight reactor schedule the suppression
	// guards against is itself lost on restart).
	BusinessownerSpeechAt map[ActorID]time.Time

	// SummonErrands holds the in-flight summon messenger-errand state
	// machines (ZBBS-HOME-311). Keyed by ErrandID; nil-readable as empty
	// (lazy-allocated on first DispatchSummon). The ActorArrived subscriber
	// (handleSummonArrival) advances an arrived participant's errand; the
	// suppressArrivalWarrant hook reads it to keep an errand participant
	// from LLM-ticking mid-errand. World-goroutine-only; restart-loss is
	// accepted — matches v1's transient ticker, same posture as
	// BusinessownerCooldowns / ActiveRoutes. EVERY terminal path removes the
	// entry (see finishErrand) so a leaked errand can't suppress the
	// summoner's warrants forever.
	SummonErrands map[ErrandID]*summonErrand

	// recentSummonErrands is the bounded post-mortem ring of FINISHED summon
	// errands (LLM-414 observability): finishErrand appends every terminal
	// outcome with its state-transition history so the umbilical
	// summon-errands view can answer "where did that errand die" after the
	// live map entry is gone. Oldest first, capped at summonErrandHistoryCap.
	// World-goroutine-only; restart-lossy like SummonErrands itself.
	recentSummonErrands []FinishedSummonErrand

	// establishmentCloseupDeadline holds the active close-up grace deadline per
	// establishment (LLM-129). Keyed by StructureID; nil-readable as empty
	// (lazy-allocated when a keeper beds down and arms the close-up). It is the
	// generation guard for the eviction timer: arming overwrites the entry, so a
	// superseded timer (keeper woke and re-bedded inside the window) sees a
	// deadline that no longer matches its own and no-ops rather than evicting on a
	// shortened second window. World-goroutine-only; restart-loss is accepted —
	// same transient posture as SummonErrands / ActiveRoutes. The matching timer
	// removes its entry when it fires (fireEstablishmentCloseup), so entries don't
	// accumulate.
	establishmentCloseupDeadline map[StructureID]time.Time

	// ActiveRoutes holds the in-flight per-NPC scheduled-route state
	// machines (lamplighter / washerwoman / town_crier). Keyed by the
	// agentRateLimits is the per-shared-VA tick-rate cap the reactor paces
	// emission against (LLM-156). A shared VA slug (salem-vendor) backs many
	// NPCs, but the memory-api rate limit is keyed per agent-NAME, so without
	// pacing the pool's aggregate ticks burst past the cap and drop the whole
	// pool into a silent cooldown. Keyed by Actor.LLMAgent; a slug absent here
	// is ungated (fail-open — never worse than no pacing). Populated once at
	// startup from the /v1/agent/rate-limit query (SetAgentRateLimits); never
	// mutated at runtime. World-goroutine-only, never checkpointed — a fresh
	// process re-queries at boot and an empty window post-restart is correct.
	agentRateLimits map[string]AgentRateLimit

	// agentRecentTicks is the per-shared-VA ring of recent reactor-tick emit
	// times — the counting substrate for the agentRateLimits gate, aggregating
	// ticks across every actor sharing the slug. Lazy-allocated on first
	// record; world-goroutine-only; never checkpointed (LLM-156).
	agentRecentTicks map[string]*RingBuffer[time.Time]

	// running NPC's ActorID; nil-readable as empty (lazy-allocated on
	// first StartNPCRoute). The cascade ActorArrived subscriber consults
	// this map to advance an arrived actor's route — most arrivals match
	// no entry and are no-ops.
	//
	// World-goroutine-only; restart-loss is acceptable. A lamplighter or
	// washerwoman walking mid-route across an engine restart loses the
	// in-flight route; the next phase / rotation boundary re-triggers
	// the cycle. The carved-out objects sit at their pre-route state
	// until then — same UX wrinkle as a missed boundary, not a
	// correctness failure.
	ActiveRoutes map[ActorID]*NPCRoute

	// routeInstallSeq is a monotonically-increasing counter stamped onto each
	// NPCRoute.Gen at StartNPCRoute install (LLM-514 fix A). It gives every route
	// install a distinct identity so a dwell-timer callback that already fired
	// before its route was superseded can detect the supersede even when the
	// replacement occupies the same StopIdx. World-goroutine-only (only StartNPCRoute
	// touches it); never persisted — a fresh process restarts the sequence, which is
	// fine because it only needs to distinguish routes within one process lifetime.
	routeInstallSeq uint64

	// RouteBoundaryStamps records, per route attribute slug (washerwoman /
	// town_crier), the schedule-window boundary last acted on by the
	// route-schedule trigger (ZBBS-HOME-446) — the edge re-fire guard. Keyed by
	// attribute slug rather than actor because each route attribute has a
	// single carrier; nil-readable as empty (lazy-allocated on first stamp).
	//
	// World-goroutine-only; restart-loss is DESIRABLE here, not just
	// acceptable: a missing stamp makes the most recent boundary fire once
	// on the first tick after boot, which is the boot catch-up that lands
	// laundry/boards in the right state for the current time of day. The
	// directional candidate builders make that catch-up idempotent.
	RouteBoundaryStamps map[string]time.Time

	// ConstableRoundsStamps records, PER constable actor, the wall-clock time its
	// last rounds tour was dispatched (LLM-514). Keyed by ActorID — NOT by the
	// attribute slug like RouteBoundaryStamps — because the constable fires on a
	// per-carrier JITTERED interval: a shared slug key would let one carrier's stamp
	// suppress another's due beat, collapsing the desync the phase offset exists to
	// provide. ConstableRoundsDue reads it; runConstableRounds stamps it per carrier.
	// nil-readable as empty (lazy-allocated on first stamp). World-goroutine-only;
	// restart-loss is desirable — same boot-catch-up posture as RouteBoundaryStamps.
	ConstableRoundsStamps map[ActorID]time.Time

	// NoticeboardContent stores per-board authored prose — what the
	// town crier reads on arrival, what NPCs loitering at the board
	// will perceive once that read path ports. Keyed by VillageObjectID
	// of the noticeboard placement; nil-readable as empty (lazy-
	// allocated on first SaveNoticeboardContent).
	//
	// World-goroutine-only; restart-loss is acceptable. A board with
	// stamped content across a restart loses it; the next rotation
	// cycle authors fresh content. First cycle after cold start: crier
	// reads nothing (board empty); subsequent cycles read normally.
	NoticeboardContent map[VillageObjectID]*NoticeboardContent

	// ActionLog is the world-level append-only audit trail of
	// committed agent + engine-source actions. Consumed by the
	// atmosphere refresh cascade (group-by-actor-by-action since
	// last fire) and per-actor narrative consolidation (own + peer
	// rows within a recent window). See engine/sim/action_log.go
	// for the entry shape and the package doc explaining what's
	// dropped vs v1's pg schema.
	//
	// In-memory only at MVP — no repo wire-through; mem package
	// keeps the noopActionLog sink. Restart-loss is acceptable:
	// atmosphere's last-fire stamp resets on restart and C2
	// re-snapshots from current state.
	ActionLog []ActionLogEntry

	// actionLogSeq backs ActionLogEntry.Seq — strictly increasing,
	// incremented by AppendActionLogEntry on the world goroutine.
	// Starts at zero each boot (the log itself is restart-lossy);
	// cursor readers detect the reset via the feed's latest_seq.
	actionLogSeq uint64

	// PriceBook is the in-memory per-(seller, item) ring buffer of
	// recent accepted-price observations — v2's substrate for v1's
	// price-history perception cues ("you paid X coins last time").
	// Keyed by (SellerID, Item); each ring buffer holds the latest
	// PriceBookRingCapacity transactions across all buyers. Per-buyer
	// reads filter the buffer; per-seller reads aggregate it. See
	// engine/sim/price_book.go for the substrate contract.
	//
	// In-memory only — no checkpoint pass, no projection sink.
	// pay_ledger remains the source of truth; this is a perception
	// cache. Seeded at LoadWorld from OrdersRepo.LoadRecentPrices
	// over a PriceBookSeedWindow-wide tail; maintained via the
	// PayWithItemResolved{Accepted} subscriber in
	// cascade/price_book.go.
	//
	// Lazy-allocated on first SeedPriceBook or RecordPriceObservation;
	// nil-readable as empty.
	PriceBook map[PriceBookKey]*RingBuffer[PriceObservation]

	// NarrationPools is the expandable narration phrase registry
	// (ZBBS-WORK-399) — businessowner hospitality, lodging day-cycle,
	// NPC retire farewell. Seeded by NewWorld from the compile-time
	// authoring tables; DB-persisted expansions merge in at boot via
	// MergeNarrationExpansions and accrue at runtime via
	// FinishNarrationExpansion. Draw counters inside each pool are
	// transient; generated phrases are durable in the
	// narration_pool_expansion table (write-through, not checkpointed).
	// World-goroutine-only — see engine/sim/narration_pool.go.
	NarrationPools map[string]*NarrationPool

	// Asset catalog — reference state, loaded at startup. Looked up by
	// VillageObject.AssetID for state resolution, footprint, anchor, etc.
	Assets map[AssetID]*Asset

	// Sprite catalog — reference state, loaded at startup. The character-
	// render definitions; looked up by Actor.SpriteID to resolve the sheet
	// + animation rows for the client read surface. Separate catalog from
	// Assets (object/terrain art). Hot-reload on SIGHUP when admin edits
	// land. See sprite.go.
	Sprites map[SpriteID]*Sprite

	// Attribute-definition catalog — reference state, loaded at startup. The
	// actor-assignable attribute vocabulary (scope actor/both), keyed by slug.
	// Surfaced to the editor's attribute-add dropdown via the npc-behaviors
	// read endpoint. Distinct from the actor_attribute rows on each Actor
	// (those are assignments; this is the catalog of what can be assigned).
	// Hot-reload on SIGHUP, same lifecycle as Assets/Sprites. See
	// attribute_definition.go.
	AttributeDefinitions map[string]*AttributeDefinition

	// Recipe catalog — reference state. Keyed by OutputItem. Used by
	// produce_tick (rate + inputs + output_qty) and pay-deliberation
	// (wholesale/retail prices).
	Recipes map[ItemKind]*ItemRecipe

	// recipeUses is the memoized reverse of Recipes (input item -> the items it
	// helps produce), so perception and the consume rejection can name an
	// inedible ingredient's purpose without scanning the catalog per call
	// (LLM-166). Built lazily via ensureRecipeUses and refreshed in place by
	// SetRecipe; aliased onto the published Snapshot like Recipes. See
	// recipe_uses.go.
	recipeUses map[ItemKind][]ItemKind

	// ItemKind catalog — reference state. Keyed by Name (== ItemKind). The
	// definitional source for an item's display label, category, default
	// price, sort order, and per-need satisfies entries (port of v1's
	// item_kind + item_satisfies tables). Loaded at startup; hot-reloaded
	// on SIGHUP when admin edits land. See item_kind.go.
	//
	// IMMUTABILITY CONTRACT: the published Snapshot ALIASES this map (not a
	// clone — see Snapshot.ItemKinds). Two rules keep that race-free, and the
	// future SIGHUP hot-reload MUST preserve both: (1) never mutate the map or
	// a *ItemKindDef in place after LoadWorld — rebuild wholesale via LoadAll
	// and reassign the field (an already-published snapshot then keeps its old,
	// still-immutable map); (2) do that reassignment on the world goroutine
	// (e.g. via a Command), so it can't race a republish reading this field.
	ItemKinds map[ItemKind]*ItemKindDef

	// Terrain — reference state, loaded once at startup. MapW * MapH
	// bytes of per-tile terrain type. Hot-reload on SIGHUP if needed.
	Terrain *Terrain

	// Secondary indices — rebuildable from primary state at LoadWorld time
	// and kept consistent by command handlers thereafter.
	actorsByStructure map[StructureID]map[ActorID]struct{}
	actorsByHuddle    map[HuddleID]map[ActorID]struct{}
	// carryoverByStructure holds the most-recent concluded conversation per
	// structure (LLM-170), so a huddle re-forming there can inherit its ring +
	// loop state across the churn. Keyed by StructureID, so bounded by structure
	// count; transient (never checkpointed, cleared at boot — chatter is restart-
	// lossy by design). World-goroutine only.
	carryoverByStructure map[StructureID]*conversationCarryover
	// outdoorActors tracks every actor with InsideStructureID == "". Hot-
	// path optimization for the arrival-encounter subscriber
	// (handleArrivalEncounter): at 200+ actors, scanning w.Actors
	// linearly on every ActorArrived is the wrong shape. Most actors are
	// indoor at any moment (sleeping, working, dining), so the outdoor set
	// is a small fraction of the population and the scan stays bounded by
	// outdoor density rather than total population.
	//
	// Maintained in lockstep with InsideStructureID by setActorInside-
	// Structure (the single mutation chokepoint) and rebuilt from primary
	// state by rebuildIndices. Iterated read-only via ForEachOutdoorActor.
	outdoorActors map[ActorID]struct{}

	Environment WorldEnvironment
	Phase       Phase
	Settings    WorldSettings
	TickCounter uint64

	// LoadedAt is the wall-clock moment LoadWorld populated this world
	// from the repository. Set once by LoadWorld; never modified
	// afterward. Read by the idle-backstop cascade slice as the cold-
	// start anchor for actors with no RecentReactorTicks history (a
	// fresh-loaded actor is "active at LoadedAt," not "idle forever").
	// Other consumers don't need this — lastReactorTickAt is the
	// authoritative source for per-actor tick history, and its
	// nil-RecentReactorTicks "never ticked" semantics is what the
	// MinReactorTickGap pacing floor and rate gate both rely on.
	LoadedAt time.Time

	Speech             *SpeechHelper
	reactorEval        reactorEvaluatorState
	locomotionTick     locomotionTickerState
	waterfowlTick      waterfowlTickerState
	grazerTick         grazerTickerState
	sceneQuoteSweep    sceneQuoteSweepState
	payLedgerSweep     payLedgerSweepState
	laborLedgerSweep   laborLedgerSweepState
	orderSweep         orderSweepState
	huddleSilenceSweep huddleSilenceSweepState
	huddleLoopSweep    huddleLoopSweepState
	ecoConcludeSweep   ecoConcludeSweepState

	// waterfowl is the per-duck transient wander state (LLM-579). Lazily
	// created by the waterfowl ticker; never checkpointed or snapshotted —
	// restart-lossy by design (the checkpointed actor position is the
	// re-seed anchor). World-goroutine-only.
	waterfowl map[ActorID]*waterfowlState

	// grazers is the per-animal transient wander state for the grazer
	// ticker (grazer.go) — same lifecycle rules as waterfowl above.
	grazers map[ActorID]*grazerState

	// quoteSeq is the monotonic per-run QuoteID counter — same shape
	// and rules as eventSeq. Incremented before assignment; first
	// minted QuoteID is 1 (QuoteID(0) reserved as the unset sentinel).
	// World-goroutine-only (touched exclusively from inside Command.Fn).
	quoteSeq uint64

	// payLedgerSeq is the monotonic per-run LedgerID counter — same
	// shape and rules as quoteSeq. Incremented before assignment;
	// first minted LedgerID is 1 (LedgerID(0) reserved as the unset
	// sentinel / "no parent" / "no quote referenced").
	// World-goroutine-only (touched exclusively from inside Command.Fn).
	payLedgerSeq uint64

	// errandSeq is the monotonic per-run ErrandID counter (ZBBS-HOME-311) —
	// same shape and rules as payLedgerSeq. Incremented before assignment; first
	// minted ErrandID is 1 (ErrandID(0) reserved as the unset sentinel).
	// World-goroutine-only. Restart-lossy by design (errands are in-memory),
	// so there is no LoadWorld safety-floor pass — it always starts at 0.
	errandSeq uint64

	// laborLedgerSeq is the monotonic per-run LaborID counter (LLM-26) — same
	// shape and rules as payLedgerSeq. Incremented before assignment; first
	// minted LaborID is 1 (LaborID(0) reserved as the unset sentinel).
	// World-goroutine-only. Since LLM-259 the accepted (en_route/working) subset
	// of the ledger is checkpointed (labor_contract), so FinalizeLoad floors this
	// counter above the max loaded LaborID (rehydrateLaborContractsOnLoad) — the
	// same safety-floor posture as payLedgerSeq — to keep a post-restart mint from
	// reusing a persisted id. pending/terminal offers stay restart-lossy.
	laborLedgerSeq uint64

	// suppressArrivalWarrant, when non-nil, is consulted by the locomotion
	// ticker's finishArrival immediately before it stamps an
	// ArrivalWarrantReason: the warrant is stamped only when the hook is nil
	// or returns false. Installed by RegisterSummonSubscriber to keep an
	// active summon-errand participant (notably the summoner, a VA NPC) from
	// LLM-ticking and wandering off mid-errand. nil = no suppression (the
	// default, e.g. in tests that don't register the subscriber).
	// World-goroutine-only (read inside finishArrival, set at registration).
	suppressArrivalWarrant func(*Actor) bool

	// terminalOrderSink is the synchronous durable-write target for Order
	// terminal transitions (Slice 6 write-through-then-prune). Nil by
	// default; SetTerminalOrderSink installs the pg impl at production
	// startup. When nil, finalizeOrderTerminal preserves the legacy
	// no-prune behavior so unit tests that build a world via NewWorld
	// without wiring a sink continue to see terminal entries remain in
	// w.Orders. See TerminalOrderSink doc for the contract.
	terminalOrderSink TerminalOrderSink

	// orderlessSettlementSink is the synchronous durable-write target for
	// accepted pay-ledger settlements that mint no Order — consume_now
	// eat-here singles and bundle quote-takes (LLM-246). Nil by default;
	// SetOrderlessSettlementSink installs the pg impl at production
	// startup. When nil, writeOrderlessSettlement is a no-op and those
	// settlements stay in-memory only (tests, headless worlds).
	orderlessSettlementSink OrderlessSettlementSink

	// actionLogSink is the async durable-write target for the agent_action_log
	// audit table (ZBBS-WORK-376). Nil by default; SetActionLogSink installs the
	// pg impl at production startup. When nil, AppendActionLogDurable is a no-op,
	// so tests and the in-memory consumers (atmosphere / C2, which read
	// World.ActionLog directly) are unaffected. Unlike terminalOrderSink the
	// write is async: Append enqueues here on the world goroutine and a writer
	// goroutine drains to pg off-goroutine. See the ActionLogSink doc.
	actionLogSink ActionLogSink

	// narrationExpandCh is the buffered nudge channel narrationDraw pokes
	// when a pool crosses its expansion threshold (ZBBS-WORK-399). Nil by
	// default (pools never expand); cascade.RegisterNarrationExpansion
	// installs it via SetNarrationExpansionTrigger. Send is non-blocking.
	narrationExpandCh chan<- string

	// narrationExpansionSink is the durable narration_pool_expansion
	// writer. Nil by default (in-memory-only expansion); main.go installs
	// the pg impl via SetNarrationExpansionSink before Run.
	narrationExpansionSink NarrationExpansionSink

	cmds      chan Command
	published atomic.Pointer[Snapshot]

	// runCtx is the lifecycle context the world goroutine is running
	// under. Set by Run on entry and INTENTIONALLY RETAINED after Run
	// exits, so callbacks firing post-shutdown observe the cancelled
	// ctx (rather than a fresh background ctx) and abort cleanly via
	// ctx.Err() instead of parking on a dead cmds channel.
	//
	// Used by long-lived goroutines launched outside the ticker loop
	// (notably time.AfterFunc-driven scheduled flips) via
	// World.LifecycleContext.
	//
	// Atomic so non-world-goroutine readers (the flip timer callbacks)
	// can pick it up without going through the command channel.
	runCtx atomic.Pointer[context.Context]

	// PhaseFlipGen / RotationFlipGen invalidate scheduled object flips,
	// PER SUBSYSTEM: phase transitions bump PhaseFlipGen, daily rotations
	// bump RotationFlipGen, and a spread-out flip (time.AfterFunc) captures
	// its own subsystem's generation at schedule time, skipping itself when
	// that subsystem has moved on (a rapid force-day -> force-night
	// sequence, a second rotation).
	//
	// Deliberately TWO counters, not one (ZBBS-HOME-447): the subsystems
	// flip disjoint object sets, and the previous shared counter let a
	// rotation silently invalidate in-flight phase flips. That bit at boot
	// catch-up — an engine started after dawn having also missed midnight
	// applies both boundaries back-to-back, and the rotation's bump landed
	// inside the phase flips' spread window, stranding the campfires lit
	// all day (2026-06-12).
	//
	// Atomic so the goroutine-launched scheduler can read them without
	// going through the command channel. Writers (inside the world
	// goroutine) use Add to make the bump observable.
	PhaseFlipGen    atomic.Uint64
	RotationFlipGen atomic.Uint64

	// subscribers receive in-world Events emitted from command handlers.
	// Registered via Subscribe before Run starts; each event is dispatched
	// to every subscriber in registration order, synchronously inside the
	// world goroutine. See events.go for the contract.
	subscribers []EventSubscriber

	// tickerHealth records last-fire + count for each interval goroutine
	// started by startTickers (the umbilical's "are the cadence drivers
	// alive" view). Written from the ticker goroutines (not the world
	// goroutine), so the registry carries its own mutex. Always non-nil for
	// a NewWorld-built world; beatTicker is nil-safe regardless. See
	// ticker_health.go.
	tickerHealth *TickerHealth

	// deadlockLog records recent MoveStoppedDeadlocked events (ZBBS-WORK-340)
	// for the umbilical /deadlocks read route. Recorded from the world
	// goroutine inside advanceActorLocomotion when a mover's per-MoveIntent
	// stuck counter trips; read from HTTP request goroutines, so the ring
	// carries its own mutex. Always non-nil for a NewWorld-built world;
	// RecordDeadlock is nil-safe regardless. See deadlock_log.go.
	deadlockLog *DeadlockLog

	// worldCmdHealth records the liveness of THIS goroutine's command loop —
	// the round-trip time of a periodic no-op probe (LLM-402). It lives on the
	// World, rather than being built in cmd/engine and injected like
	// CheckpointHealth, because it describes the world's own loop: every
	// wiring gets it without plumbing. Written by the prober goroutine, read
	// from HTTP request goroutines, so it carries its own mutex. Always
	// non-nil for a NewWorld-built world; every method is nil-safe regardless.
	// See world_command_probe.go.
	worldCmdHealth *WorldCommandHealth

	// eventSeq is the monotonic per-run event counter. emit increments it
	// and assigns the value as the new event's EventID. World-goroutine-
	// only — emit runs exclusively inside Command.Fn, so no atomic is
	// needed. Starts at 0; the first emitted event gets ID 1, leaving
	// EventID(0) as the unset sentinel.
	eventSeq uint64

	// currentRootEventID is the ambient causal root for events emitted
	// within the current cascade. 0 means no cascade is active — the next
	// emit becomes a fresh root. Set and restored by withRoot (defer-
	// scoped, panic-safe). World-goroutine-only.
	currentRootEventID EventID

	// tickAdmission gates the reactor evaluator — consulted before an
	// actor's warrants are consumed (Option A — admit before consume).
	// Never nil: NewWorld sets alwaysAdmit, and PR 3's worker pool installs
	// a real one via SetTickAdmissionController.
	tickAdmission TickAdmissionController

	repo Repository
}

// nextEventSeq increments the per-run event counter and returns the new
// EventID. World-goroutine-only (called from emit). The counter starts at
// 0, so the first event is EventID(1) — EventID(0) is never assigned.
func (w *World) nextEventSeq() EventID {
	w.eventSeq++
	return EventID(w.eventSeq)
}

// withRoot runs fn with currentRootEventID set to root, restoring the
// previous value on return — including on panic, via defer. World-
// goroutine-only; no atomic. Used by emit (to establish a fresh cascade
// root) and by Run (to continue an inherited root across the worker seam).
func (w *World) withRoot(root EventID, fn func()) {
	prev := w.currentRootEventID
	w.currentRootEventID = root
	defer func() { w.currentRootEventID = prev }()
	fn()
}

// NewWorld constructs an empty World bound to the given Repository.
//
// Call LoadWorld for production startup (populates primary state from
// persistence); tests typically use NewWorld + direct map seeding so they
// can control the initial state precisely.
//
// The cmds channel is buffered to absorb bursts without blocking
// producers; the world goroutine drains it.
func NewWorld(repo Repository) *World {
	w := &World{
		Actors:               make(map[ActorID]*Actor),
		Structures:           make(map[StructureID]*Structure),
		Huddles:              make(map[HuddleID]*Huddle),
		Scenes:               make(map[SceneID]*Scene),
		HomeBakes:            make(map[StructureID]*HomeBake),
		Orders:               make(map[OrderID]*Order),
		VillageObjects:       make(map[VillageObjectID]*VillageObject),
		Quotes:               make(map[QuoteID]*SceneQuote),
		PayLedger:            make(map[LedgerID]*PayLedgerEntry),
		LaborLedger:          make(map[LaborID]*LaborOffer),
		RecurringVisitors:    make(map[RecurringVisitorID]*RecurringVisitor),
		Assets:               make(map[AssetID]*Asset),
		Sprites:              make(map[SpriteID]*Sprite),
		AttributeDefinitions: make(map[string]*AttributeDefinition),
		Recipes:              make(map[ItemKind]*ItemRecipe),
		ItemKinds:            make(map[ItemKind]*ItemKindDef),
		NarrationPools:       narrationSeedPools(),
		actorsByStructure:    make(map[StructureID]map[ActorID]struct{}),
		actorsByHuddle:       make(map[HuddleID]map[ActorID]struct{}),
		carryoverByStructure: make(map[StructureID]*conversationCarryover),
		outdoorActors:        make(map[ActorID]struct{}),
		Speech:               &SpeechHelper{},
		cmds:                 make(chan Command, 256),
		tickAdmission:        alwaysAdmit{},
		repo:                 repo,
		tickerHealth:         newTickerHealth(),
		worldCmdHealth:       newWorldCommandHealth(),
		deadlockLog:          newDeadlockLog(0),
	}
	w.republish()
	return w
}

// SetTerminalOrderSink installs the synchronous durable-write target the
// world invokes during Order terminal transitions (Slice 6 write-through-
// then-prune). Passing nil clears the sink and restores legacy no-prune
// behavior. Safe to call before Run, or from inside a Command.Fn.
//
// Wiring order matters at production startup: SetTerminalOrderSink must
// be called BEFORE LoadWorld so the LoadWorld-time
// restartExpirePendingOrders pass also write-through-prunes any orders
// whose ExpiresAt elapsed during downtime. Calling it after LoadWorld
// leaves those restart-time terminal entries sitting in w.Orders until
// the next checkpoint reconciles them.
func (w *World) SetTerminalOrderSink(s TerminalOrderSink) {
	w.terminalOrderSink = s
}

// SetOrderlessSettlementSink installs the durable pay_ledger sink for
// accepted settlements that mint no Order — consume_now eat-here singles
// and bundle quote-takes (LLM-246). Without it those settlements exist
// only in memory, so the price-book restart seed (LoadRecentPrices)
// never sees the village's highest-frequency trades. Passing nil clears
// it (the default). Safe to call before Run, or from inside a Command.Fn.
func (w *World) SetOrderlessSettlementSink(s OrderlessSettlementSink) {
	w.orderlessSettlementSink = s
}

// writeOrderlessSettlement forwards an accepted order-less settlement to
// the durable sink when one is installed; a no-op otherwise. Same error
// posture as finalizeOrderTerminal's write-through: log and continue —
// the settlement already committed in memory, and the durable row is
// price-history/audit data whose loss degrades recall, not consistency.
// Called by commitPayTransfer on the world goroutine.
func (w *World) writeOrderlessSettlement(e *PayLedgerEntry, at time.Time) {
	sink := w.orderlessSettlementSink
	if sink == nil {
		return
	}
	if err := sink.WriteOrderlessSettlement(w.LifecycleContext(), e, at); err != nil {
		log.Printf("sim: orderless settlement write-through for ledger %d failed: %v", e.ID, err)
	}
}

// SetActionLogSink installs the durable agent_action_log sink the world
// forwards committed action rows to via AppendActionLogDurable (ZBBS-WORK-376).
// Passing nil clears it (the default — AppendActionLogDurable becomes a no-op).
// Safe to call before Run, or from inside a Command.Fn. The production impl is
// async (see ActionLogSink), so this only stores the reference; the caller owns
// starting and draining the sink's writer goroutine.
func (w *World) SetActionLogSink(s ActionLogSink) {
	w.actionLogSink = s
}

// AppendActionLogDurable forwards a structured action row to the durable
// agent_action_log sink when one is installed; a no-op otherwise (tests,
// headless). The production sink's Append is a non-blocking enqueue, so this
// does not block the world goroutine on PG, and a write error is the sink's own
// concern (logged on its writer goroutine), never surfaced here. Called by the
// cascade action-log subscribers, which run inline on the world goroutine —
// hence the exported method, since those subscribers live in package cascade
// and can't reach the unexported field.
func (w *World) AppendActionLogDurable(row DurableActionLogRow) {
	sink := w.actionLogSink
	if sink == nil {
		return
	}
	// Waterfowl never reach the durable table either (LLM-593) — the same
	// gate the in-memory funnel applies, repeated because a subscriber calls
	// the two independently and a silent in-memory skip would otherwise still
	// let the row through to PG.
	if actionLogIgnoresActor(w, row.ActorID) {
		return
	}
	// Visitors are transient and live OUTSIDE the actor aggregate — their rows are
	// in the separate `visitor` table, firewalled out of ActorsRepo (LLM-369). The
	// durable audit sink's actor_id is a uuid with a FK to actor(id), so a "vstr-"
	// id can never satisfy it; every insert errored on the writer goroutine, which
	// LLM-379 stopped by dropping the row outright here.
	//
	// Dropping cost too much (LLM-573): agent_action_log is the SOLE input to the
	// day note feeding the nightly dream pipeline, so a stateful NPC who traded
	// with a visitor kept his own half of the scene and lost the visitor's — a
	// one-sided transcript where he answers questions nobody asked. The row is
	// kept now and its ActorID blanked instead, which the sink writes as SQL NULL:
	// no FK to violate, speaker_name still carries the display name, and the
	// overheard-speech leg of loadDayEventsSQL keys on huddle_id rather than
	// actor_id, so the visitor's speech reaches the co-present actor's day note.
	//
	// Blanking rather than storing the visitor id is deliberate: a "vstr-" id is
	// ephemeral — the `visitor` row is stale-deleted at the checkpoint following
	// departure — so persisting it would leave a reference that resolves to
	// nothing within the day, while the display name stays meaningful for life.
	//
	// The id-shape arm is not redundant with the VisitorState lookup: an emit
	// site that appends AFTER cleanup removed the visitor from w.Actors gets a
	// nil actor and would send the raw "vstr-" id to a uuid column, resuming the
	// LLM-379 error flood — and now losing the row, which is the whole defect.
	// IsVisitorActorID exists for exactly this "the in-memory actor is gone, the
	// id is the only signal" case, and matches the full ^vstr-[0-9a-f]{8}$ mint
	// shape, so a merely prefix-shaped out-of-band id still fails loudly at the
	// insert rather than being silently swallowed as a NULL-author row.
	if a := w.Actors[row.ActorID]; (a != nil && a.VisitorState != nil) || IsVisitorActorID(row.ActorID) {
		row.ActorID = ""
	}
	_ = sink.Append(w.LifecycleContext(), row)
}

// SetTickAdmissionController installs the controller the reactor evaluator
// consults before consuming an actor's warrants. PR 3's worker pool calls
// this at bootstrap (as one half of RegisterTickHandlers). A nil argument
// resets to the alwaysAdmit default.
//
// Safe to call before Run, or from inside a Command.Fn (the world
// goroutine). Calling it from an arbitrary goroutine while Run is
// processing races the evaluator — route it through a Command instead.
func (w *World) SetTickAdmissionController(c TickAdmissionController) {
	if c == nil {
		c = alwaysAdmit{}
	}
	w.tickAdmission = c
}

// LoadWorld constructs a World and populates primary state from the
// repository. Use this for production startup.
//
// Sub-repos implemented at this stage (Actors, Huddles, Environment)
// are loaded; remaining sub-repos land as subsystems get ported.
// Indices are rebuilt from primary state, snapshot is published, ready
// to Run.
func LoadWorld(ctx context.Context, repo Repository) (*World, error) {
	w := NewWorld(repo)

	actors, err := repo.Actors.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Actors = actors
	// Backfill any registry need missing from a loaded actor's rows (a need
	// added after the actor's rows were first written — e.g. cold, LLM-412) so
	// SnapshotNeeds never log-spams a missing key and the next checkpoint
	// persists the new row. Zero is the correct seed for every need (fully
	// sated / not cold).
	for _, a := range w.Actors {
		if a.Needs == nil {
			a.Needs = make(map[NeedKey]int, len(Needs))
		}
		for _, n := range Needs {
			if _, ok := a.Needs[n.Key]; !ok {
				a.Needs[n.Key] = 0
			}
		}
	}

	huddles, err := repo.Huddles.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Huddles = huddles

	scenes, err := repo.Scenes.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Scenes = scenes

	env, phase, settings, err := repo.Environment.Load(ctx)
	if err != nil {
		return nil, err
	}
	w.Environment = env
	w.Phase = phase
	w.Settings = settings

	assets, err := repo.Assets.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Assets = assets

	sprites, err := repo.Sprites.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Sprites = sprites

	attributeDefinitions, err := repo.AttributeDefinitions.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.AttributeDefinitions = attributeDefinitions

	recipes, err := repo.Recipes.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Recipes = recipes

	itemKinds, err := repo.ItemKinds.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.ItemKinds = itemKinds

	terrain, err := repo.Terrain.Load(ctx)
	if err != nil {
		return nil, err
	}
	w.Terrain = terrain

	structures, err := repo.Structures.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.Structures = structures

	villageObjects, err := repo.VillageObjects.LoadAll(ctx)
	if err != nil {
		return nil, err
	}
	w.VillageObjects = villageObjects

	if err := w.FinalizeLoad(ctx); err != nil {
		return nil, err
	}
	return w, nil
}

// FinalizeLoad runs the post-load housekeeping that turns a freshly
// populated World into a runnable one: index rebuild, reactor-state
// reset, the restart-time expiry/re-stamp passes, sequence-counter
// safety floors, the price-book seed, and the initial snapshot publish.
//
// Extracted from LoadWorld so the pg orchestrator (engine/sim/repo/pg)
// can reuse this exact finalize sequence: it lives in a different
// package and can't reach these unexported sim internals (rebuildIndices,
// the restart* helpers, the seq counters, republish) directly. Keeping
// the sequence in one place is the whole point — both LoadWorld and
// pg.LoadWorld stay in lockstep as housekeeping evolves.
//
// Callers MUST invoke this only after every aggregate has loaded — and,
// for pg.LoadWorld, after the cross-aggregate consistency checks and the
// actor carry-forwards (reconcileActorHuddleMembership in particular,
// since rebuildIndices reads actor.CurrentHuddleID).
func (w *World) FinalizeLoad(ctx context.Context) error {
	normalizeOutdoorSceneRadius(&w.Settings)

	// Boot-init the non-persisted real-flip stamp from the durable apply
	// stamp (LLM-578) — see the LastPhaseFlipAt field doc for why they
	// differ. Lives HERE, not in either LoadWorld, because sim.LoadWorld
	// and pg.LoadWorld are parallel orchestrators and only this finalize
	// step is shared — the first cut patched only sim.LoadWorld and
	// production (pg) booted with a zero stamp, verified live.
	w.Environment.LastPhaseFlipAt = w.Environment.LastTransitionAt

	w.rebuildIndices()
	// LLM-369: rehydrate in-flight visitors from their durable mirror into
	// World.Actors — the reverse of the SaveSnapshot filter that keeps them out
	// of the actor aggregate. AFTER rebuildIndices so the secondary-index maps
	// exist to place them in; a visitor whose stay elapsed while down is dropped.
	if err := w.rehydrateVisitorsOnLoad(ctx); err != nil {
		return fmt.Errorf("sim: FinalizeLoad: rehydrate visitors: %w", err)
	}
	// LLM-372: load the durable returner set AFTER visitors so it can validate the
	// in-flight visitor->recurring_visitor links against the loaded rows.
	if err := w.rehydrateRecurringVisitorsOnLoad(ctx); err != nil {
		return fmt.Errorf("sim: FinalizeLoad: rehydrate recurring visitors: %w", err)
	}
	// LLM-547: load the per-pair conversational recency trail. Order-free with
	// respect to the other aggregates — the trail holds no references it needs
	// resolved, and a row naming an actor that no longer exists is harmless
	// (every read is keyed by a co-present peer, and a departed actor is never
	// co-present). Placed after visitors only for readability.
	if err := w.rehydrateContactLedgerOnLoad(ctx); err != nil {
		return fmt.Errorf("sim: FinalizeLoad: rehydrate contact ledger: %w", err)
	}
	// LLM-572: seed the per-pair coin tally from the durable action log. This one
	// IS order-dependent — it resolves each historical row's recipient against
	// w.Actors by display name, so the actor aggregate has to be loaded first.
	// FinalizeLoad runs after LoadWorld has populated it, which is why the seed
	// lives here rather than in the loader.
	if err := w.rehydrateCoinRecordOnLoad(ctx); err != nil {
		return fmt.Errorf("sim: FinalizeLoad: seed coin record: %w", err)
	}
	// LLM-259: rehydrate the accepted (en_route/working) labor contracts from
	// their durable mirror into World.LaborLedger BEFORE the stranded-laboring
	// reconcile below. A worker whose working contract loaded then holds a live
	// ledger job and resumes, instead of being reverted to idle and re-soliciting
	// on every deploy. Also floors the LaborID allocator and restores the
	// transient working-worker mirror. Fails the load ONLY on a DB query error;
	// individual unusable rows (dangling ref, stale state) are warn-dropped so a
	// bad row can't wedge the village boot.
	if err := w.rehydrateLaborContractsOnLoad(ctx); err != nil {
		return fmt.Errorf("sim: FinalizeLoad: rehydrate labor contracts: %w", err)
	}
	// Reactor state (warrants + in-flight + attempt-id + recent-tick ring)
	// is ephemeral by design — payloads are interface-typed and weren't
	// designed to cross the checkpoint serialization boundary. Cascade
	// origins re-engage actors via fresh events post-restart; the warrant
	// list from before the crash isn't meaningful anymore (the
	// conversational moment passed).
	for _, a := range w.Actors {
		resetReactorStateOnLoad(a)
		// LLM-162 / LLM-259: a worker checkpointed mid-job reloads as StateLaboring
		// (State IS persisted via sim_state), but the LaborID/LaboringUntil mirror
		// is transient. If an accepted contract was rehydrated above, the worker
		// holds a live ledger job and resumes; otherwise it is a genuine orphan the
		// completion sweep can never free, so revert it to idle.
		reconcileStrandedLaboringOnLoad(w, a)
	}
	// LoadedAt is the wall-clock moment this world woke up (not
	// w.Environment.Now, which can lag arbitrarily on a long-crash
	// recovery). Read by the idle-backstop sweep so fresh-loaded actors
	// — who have no RecentReactorTicks history yet — are treated as
	// "active at world wake-up" rather than "never ticked, idle by
	// maximum duration." Without that, the first sweep after restart
	// would stamp idle warrants on every actor simultaneously. See
	// engine/sim/idle_backstop_commands.go.
	w.LoadedAt = time.Now().UTC()
	// Scene-quote restart housekeeping. Pending scene quotes are
	// intentionally restart-lossy (decided 2026-05-20 — see
	// work/tasks/payledger-restart-lossy/decision): there is no
	// QuotesRepo and none will be built, so w.Quotes always starts
	// empty and this pass is DORMANT BY DESIGN — it iterates an empty
	// map under both LoadWorld paths. Kept (not deleted) because it
	// encodes correct behavior if that decision is ever reversed: any
	// quote already past its ExpiresAt at restart would flip to expired
	// with ResolvedAt stamped, no event emitted (the original
	// SceneQuoteCreated event is gone, so a re-stamped expired event
	// would have nothing to reference causally — restart-noncritical
	// per scene-quote-design § 7). A pending quote crossing a restart
	// is meant to re-emit fresh via the next scene_quote tool call, not
	// be resurrected with stale ExpiresAt.
	restartExpireScannedQuotes(w, time.Now())
	// QuoteIDs reverse index is rebuilt from the canonical World.Quotes
	// map so any drift loaded from a repo can't persist past startup.
	rebuildSceneQuoteIndex(w)
	// Quote sequence counter safety floor: if the loaded counter is
	// somehow below the max QuoteID actually present, bump it so the
	// next mint doesn't collide. Idempotent — both paths produce the
	// same result when the counter was correct.
	for id := range w.Quotes {
		if uint64(id) > w.quoteSeq {
			w.quoteSeq = uint64(id)
		}
	}
	// Pay-ledger restart housekeeping. Pending pay_ledger entries are
	// intentionally restart-lossy (decided 2026-05-20 — see
	// work/tasks/payledger-restart-lossy/decision): there is no
	// PayLedgerRepo and none will be built, pending entries are not
	// checkpointed by pg.SaveWorld, so w.PayLedger always starts empty
	// and this pass is DORMANT BY DESIGN — it iterates an empty map
	// under both LoadWorld paths. Losing a pending entry on crash is
	// materially harmless (architecture section 2 — a pending entry locks
	// no coins, stock, or presence; accept_pay revalidates every gate),
	// and the 3-minute TTL means most pending offers would have expired
	// during any real downtime anyway. Kept (not deleted) because it
	// encodes correct behavior if the decision is ever reversed: any
	// pending entry already past its ExpiresAt would flip to expired
	// with ResolvedAt stamped, no event emitted (the original
	// PayOfferReceived event is gone, so the flip has no causal anchor).
	restartExpirePendingEntries(w, time.Now())
	// LedgerID sequence-counter safety floor. payLedgerSeq is the sole
	// allocator for pay_ledger ids — Orders adopt their LedgerID
	// (ZBBS-HOME-394), so there is no separate order counter. It MUST start
	// above every id still durably referenced, or a post-restart mint would
	// reuse one and the checkpoint upsert would clobber that historical row.
	// A ledger_id is durably referenced in exactly two places, and the floor
	// takes the GREATEST of both DB maxes:
	//   1. a pay_ledger row — MaxLedgerID. Covers Orders too: every checkpoint
	//      upserts each in-flight order's pay_ledger row, so w.Orders (the
	//      in-flight subset) and the restart-lossy in-memory PayLedger map
	//      (empty here) never hold an id above this max.
	//   2. a `paid` action-log payload — MaxPaidActionLogLedgerID. v2
	//      consume_now settlements mint a LedgerID and write NO pay_ledger row,
	//      so their ids live ONLY here; without this term the seed lands below
	//      the true high-water mark and a mint reuses a consume_now id,
	//      corrupting LLM-105's paid.ledger_id -> pay_ledger.id audit join
	//      (LLM-245).
	// This is persistence safety, not optional enrichment: fail the load on a
	// query error rather than start in a state where a mint could reuse an id
	// and the checkpoint upsert corrupt a historical row (code_review). The
	// >0 guards keep the (unreachable) negative out of the uint64 conversion.
	maxID, err := w.repo.Orders.MaxLedgerID(ctx)
	if err != nil {
		return fmt.Errorf("sim: FinalizeLoad: seed pay-ledger id allocator from MaxLedgerID: %w", err)
	}
	if maxID > 0 && uint64(maxID) > w.payLedgerSeq {
		w.payLedgerSeq = uint64(maxID)
	}
	actionLogMaxID, err := w.repo.Orders.MaxPaidActionLogLedgerID(ctx)
	if err != nil {
		return fmt.Errorf("sim: FinalizeLoad: seed pay-ledger id allocator from MaxPaidActionLogLedgerID: %w", err)
	}
	if actionLogMaxID > 0 && uint64(actionLogMaxID) > w.payLedgerSeq {
		w.payLedgerSeq = uint64(actionLogMaxID)
	}
	// Belt-and-suspenders: also floor from any in-memory pending entries
	// (a no-op today since pending entries are restart-lossy, but correct
	// if that ever changes).
	for id := range w.PayLedger {
		if uint64(id) > w.payLedgerSeq {
			w.payLedgerSeq = uint64(id)
		}
	}
	// Pay-offer warrant restart re-stamp (Phase 3 PR S4 step 7).
	// DORMANT BY DESIGN — pending pay_ledger entries are restart-lossy
	// (decided 2026-05-20 — see work/tasks/payledger-restart-lossy/decision),
	// so w.PayLedger is always empty here and there is nothing to
	// re-stamp. Kept (not deleted) because it documents the load-bearing
	// rationale for the WarrantReason.DedupDiscriminator migration: if
	// pending entries were ever reloaded, this pass would walk them and
	// stamp PayOfferWarrantReason on each seller so the seller's next
	// reactor tick still perceives the offer, with Discriminator =
	// uint64(LedgerID) so a normal-flow PayOfferReceived emit firing
	// AFTER this stamp dedupes against it cleanly. It would run after
	// restartExpirePendingEntries (so already-expired pendings are
	// skipped) and reach the actor's warrant slice directly via
	// tryStampWarrant (no subscriber needed).
	restartReStampPayOfferWarrants(w, time.Now())

	// Order restart housekeeping (Phase 3 PR S6). w.Orders is populated
	// under pg.LoadWorld (OrdersRepo.LoadAll) and empty under sim.LoadWorld
	// (the mem path doesn't load orders), so this pass is a no-op there and
	// live under pg: any Ready Order already past its
	// ExpiresAt at restart is flipped to Expired in-band, mirroring
	// restartExpirePendingEntries' pay-ledger pattern. Active Ready
	// orders survive the load with absolute ExpiresAt intact; the
	// aging sweep picks them up on its first pass.
	restartExpirePendingOrders(w, time.Now())

	// PC idle-input restart seed (LLM-473). LastPCInputAt is transient, and the
	// idle auto-bed reads a nil stamp as "never idle" — so without this, every PC
	// loaded from a checkpoint is permanently ineligible for the 15-minute
	// bed-down until it acts. Uses w.LoadedAt rather than a fresh time.Now() so
	// the whole restart pass shares one boot instant and the seeded idle clock is
	// deterministic under test. See restartSeedPCInputStamps.
	restartSeedPCInputStamps(w, w.LoadedAt)

	// No separate order sequence-counter floor: Orders adopt their LedgerID
	// (ZBBS-HOME-394), so the payLedgerSeq floor above — seeded from the DB
	// max — already covers every order id.

	// Price-book seed (Phase 4 Slice 7). Pulls the top-K most recent
	// accepted pay_ledger rows per (seller, item) within
	// PriceBookSeedWindow, populating the in-memory price book so
	// post-restart perception has v1-parity buyer recall ("you paid
	// X last time") and seller-side aggregates available without
	// a thundering herd of "ask the keeper" turns.
	//
	// Seed source is pay_ledger (state='accepted'). Take-home flows land
	// there via the Order checkpoint upsert; order-less settlements —
	// consume_now eat-here singles — via the accept-time write-through
	// (writeOrderlessSettlement, LLM-246). LoadRecentPrices reads it
	// directly without going through the (not-yet-loaded) Orders working
	// set. Known gap: consume_now settlements between the v2 cutover
	// (2026-05-26) and LLM-246 were never persisted anywhere seedable,
	// so they stay absent; the book refills through play.
	//
	// Failures here are non-fatal to LoadWorld: a missing seed
	// produces "ask the keeper" until the cascade subscriber re-
	// populates the book through normal play. Surfaced via log so
	// operator can spot a degraded restart in stderr.
	if seedRecords, err := w.repo.Orders.LoadRecentPrices(ctx, time.Now().UTC().Add(-PriceBookSeedWindow), PriceBookRingCapacity); err != nil {
		log.Printf("sim: FinalizeLoad price-book seed: %v (continuing with empty book)", err)
	} else {
		w.SeedPriceBook(seedRecords)
	}

	w.republish()
	return nil
}

// Run owns the world goroutine. Processes commands until ctx is cancelled
// or the cmds channel is closed. Returns when the loop exits.
//
// Caller is responsible for starting this in a goroutine. After ctx
// cancel, in-flight commands complete; queued commands are dropped.
//
// Stamps w.runCtx so callbacks scheduled inside commands (e.g. phase-
// transition flip timers) can ride the same shutdown signal — see
// World.LifecycleContext. Deliberately does NOT clear runCtx on exit:
// if the timer fires after Run has returned, the stored ctx is already
// cancelled, so the callback's SendContext sees ctx.Err() != nil and
// returns immediately instead of parking forever on the cmds channel.
func (w *World) Run(ctx context.Context) {
	w.runCtx.Store(&ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case cmd, ok := <-w.cmds:
			if !ok {
				return
			}
			var value any
			var err error
			if cmd.inheritedRoot != 0 {
				// Cross-boundary command (PR 3's worker tool-call): run the
				// whole handler under the inherited cascade root so events
				// it emits continue that root and it cannot bleed into the
				// next command. See newRootedCommand.
				w.withRoot(cmd.inheritedRoot, func() {
					value, err = cmd.Fn(w)
				})
			} else {
				value, err = cmd.Fn(w)
			}
			w.TickCounter++
			// Capture the pre-republish published snapshot so emitNeedsDeltas can
			// diff this command's need changes against it. Loaded before republish
			// overwrites it; this is the state as of the previous command.
			prevSnap := w.published.Load()
			// LLM-409: flip any standing sell lot the seller can no longer cover to
			// terminal shortfall BEFORE republish, so no published snapshot ever
			// advertises stock the seller spent out from under his own offer,
			// whichever inventory-drain path this command took. Sampled here (not
			// republish's later time.Now()) so a lot's ResolvedAt is <= the
			// snapshot's PublishedAt the beat window compares against.
			w.reconcileQuoteCoverage(time.Now())
			w.republish()
			w.emitNeedsDeltas(prevSnap)
			w.emitDormancyDeltas(prevSnap)
			w.emitCoinsDeltas(prevSnap)
			if cmd.Reply != nil {
				cmd.Reply <- CommandResult{Value: value, Err: err}
			}
		}
	}
}

// SendContext enqueues a command and waits for the reply, honoring ctx
// cancellation on both the send and receive halves. Returns ctx.Err() if
// the context expires before the world goroutine accepts the command or
// before the reply comes back.
//
// Use this from tickers / long-lived goroutines that need to unblock when
// the world is shutting down — Send (no context) deadlocks if Run has
// already exited.
//
// Caller MUST NOT call SendContext from inside a command Fn — that would
// deadlock the single world goroutine. Use direct mutation instead.
func (w *World) SendContext(ctx context.Context, cmd Command) (any, error) {
	reply := make(chan CommandResult, 1)
	cmd.Reply = reply
	select {
	case w.cmds <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.Value, r.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send enqueues a command and waits for the reply. Returns the command's
// Value and Err.
//
// SAFETY CONTRACT: only call Send when the caller knows the world is
// running. There is no context plumbed in — if the world goroutine has
// already exited (or hasn't started), Send blocks on the cmds channel
// forever. Tickers, long-lived background goroutines, and anything
// launched via time.AfterFunc MUST use SendContext with a context that
// gets cancelled on shutdown (see World.LifecycleContext for the
// world's own ctx).
//
// Caller MUST NOT call Send from inside a command Fn — that would
// deadlock the single world goroutine. Use direct mutation (you already
// hold the world goroutine) instead.
func (w *World) Send(cmd Command) (any, error) {
	return w.SendContext(context.Background(), cmd)
}

// LifecycleContext returns the context Run is currently using, or a
// background context if Run has never been called. Goroutines launched
// from inside a command (notably time.AfterFunc-driven scheduled flips)
// call this to get a ctx that unblocks on world shutdown.
//
// After Run exits the cancelled ctx remains in place, so a callback
// firing post-shutdown sees ctx.Err() != nil and aborts cleanly instead
// of deadlocking on a send to a dead cmds channel.
//
// Pulled fresh each time — the schedule-to-fire window can be many
// seconds, and an admin force-phase mid-window could in principle
// re-enter Run with a new ctx in the future (not today; Run is run-once).
func (w *World) LifecycleContext() context.Context {
	if p := w.runCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// Submit enqueues a fire-and-forget command. Returns immediately. Caller
// does not get to observe the outcome — use Send if you need the result.
func (w *World) Submit(fn func(*World) (any, error)) {
	w.cmds <- Command{Fn: fn}
}

// Subscribe registers an EventSubscriber to receive in-world Events emitted
// by command handlers. Subscribers run synchronously inside the world
// goroutine after each event is emitted; they may mutate world state
// freely (atomic with the emitting command) but MUST NOT block on I/O or
// call Send/SendContext (would deadlock the single goroutine).
//
// Safe to call (a) before Run has started, or (b) from inside a Command.Fn
// (which runs on the world goroutine). Calling from an arbitrary goroutine
// while Run is processing commands races against the dispatch loop in
// emit — surface those registrations through a Command instead.
//
// Subscribers fire in registration order; later subscribers see any state
// changes earlier subscribers made.
func (w *World) Subscribe(s EventSubscriber) {
	w.subscribers = append(w.subscribers, s)
}

// emit assigns the event its per-run identity and dispatches it to every
// registered subscriber. Called from command Fn implementations after the
// underlying state mutation lands. Inline dispatch keeps subscriber side
// effects atomic with the mutation — readers of the next Snapshot see the
// post-mutation, post-subscriber state.
//
// Identity: every event gets a fresh monotonic EventID. The RootEventID
// depends on whether a cascade is already active:
//
//   - No ambient root (currentRootEventID == 0): this is a fresh-origin
//     event and is its own causal root. Subscriber dispatch runs under
//     withRoot(id, ...) so events emitted by subscribers (the cascade)
//     inherit this root, and the ambient root restores to 0 on unwind —
//     even if a subscriber panics.
//
//   - Ambient root set: this is a consequent event; it inherits the
//     ambient cascade root. Dispatch needs no extra withRoot — the
//     ambient value is already correct for any nested emits.
func (w *World) emit(evt Event) {
	id := w.nextEventSeq()
	root := w.currentRootEventID
	if root == 0 {
		root = id
		evt.setEventBase(id, root)
		w.withRoot(root, func() {
			for _, s := range w.subscribers {
				s.Handle(w, evt)
			}
		})
		return
	}
	evt.setEventBase(id, root)
	for _, s := range w.subscribers {
		s.Handle(w, evt)
	}
}

// Published returns the most recently published Snapshot. Safe to call
// from any goroutine — atomic load, no coordination.
func (w *World) Published() *Snapshot {
	return w.published.Load()
}

// rebuildIndices populates the actorsByStructure / actorsByHuddle /
// outdoorActors secondary indices from primary state. Called by
// LoadWorld and as a defensive recovery path if drift is ever detected.
func (w *World) rebuildIndices() {
	w.actorsByStructure = make(map[StructureID]map[ActorID]struct{})
	w.actorsByHuddle = make(map[HuddleID]map[ActorID]struct{})
	w.outdoorActors = make(map[ActorID]struct{})
	for id, a := range w.Actors {
		if a.InsideStructureID != "" {
			if w.actorsByStructure[a.InsideStructureID] == nil {
				w.actorsByStructure[a.InsideStructureID] = make(map[ActorID]struct{})
			}
			w.actorsByStructure[a.InsideStructureID][id] = struct{}{}
		} else {
			w.outdoorActors[id] = struct{}{}
		}
		if a.CurrentHuddleID != "" {
			if w.actorsByHuddle[a.CurrentHuddleID] == nil {
				w.actorsByHuddle[a.CurrentHuddleID] = make(map[ActorID]struct{})
			}
			w.actorsByHuddle[a.CurrentHuddleID][id] = struct{}{}
		}
	}
}

// ForEachOutdoorActor invokes fn for every actor currently outdoors
// (InsideStructureID == ""). Iteration stops if fn returns false. Order
// is undefined; callers needing a deterministic order must sort the
// IDs they collect.
//
// Backed by the outdoorActors secondary index — O(K) where K is the
// outdoor population, not O(N) where N is total actor count. Intended
// for hot-path subscribers (encounter detection on ActorMoved /
// ActorArrived) at 200+ actor scale.
//
// MUST be called from inside a Command.Fn or a subscriber dispatched
// from emit (both run on the world goroutine).
//
// SNAPSHOT SEMANTICS. Iteration is over a snapshot of outdoor IDs taken
// at entry, then each ID is re-checked against w.outdoorActors and
// w.Actors before fn is invoked. So fn MAY safely mutate world state —
// including calls that flow through setActorInsideStructure — without
// breaking iteration: an actor moved indoor mid-iteration is skipped
// on its re-check, and newly-outdoor actors after entry are not seen
// by this call (they will be by the next ForEachOutdoorActor on the
// next event). Allocation is O(K) per call; this is intentional to
// avoid exposing range-while-mutating map semantics to callbacks.
func (w *World) ForEachOutdoorActor(fn func(*Actor) bool) {
	ids := make([]ActorID, 0, len(w.outdoorActors))
	for id := range w.outdoorActors {
		ids = append(ids, id)
	}
	for _, id := range ids {
		// Re-check membership: fn from a prior iteration may have moved
		// this actor indoor (e.g. by calling setActorInsideStructure via
		// a command). Skip rather than visit a now-indoor actor.
		if _, ok := w.outdoorActors[id]; !ok {
			continue
		}
		a, ok := w.Actors[id]
		if !ok {
			// Defensive: index drift would only happen if a caller
			// bypassed setActorInsideStructure or removed an actor
			// without unhooking the index. Skip rather than panic.
			continue
		}
		if !fn(a) {
			return
		}
	}
}

// republish builds and atomically swaps a fresh Snapshot. Called from the
// world goroutine after every command.
//
// Per-aggregate snapshot helpers deep-copy each entity so the published
// Snapshot is genuinely immutable from a reader's perspective — readers
// can't reach into world state through a Snapshot pointer to race against
// the world goroutine.
//
// v1 publishes a fresh map per command (cheap allocations). If snapshot
// allocation becomes hot on profiling, the contained replacement is a
// copy-on-write per-entity scheme — same external Snapshot type, lower
// allocation pressure.
func (w *World) republish() {
	// Local wall-clock minute-of-day in the village timezone, for time-of-day
	// perception (ZBBS-HOME-351) and schedule-aware steering (ZBBS-HOME-352).
	// Computed once here so the snapshot carries it without the village
	// *time.Location (which isn't on the snapshot). Taken as &localMin so a
	// hand-built snapshot (nil) is distinguishable from real midnight (0).
	// Sampled once so PublishedAt and LocalMinuteOfDay describe the same instant
	// (a second time.Now() could straddle a minute/day boundary).
	now := time.Now()
	localMin := localMinuteOfDay(w, now)
	// Day-active window (dawn/dusk) as minute-of-day — the shift fallback for an
	// NPC with no explicit schedule (ZBBS-HOME-352). DawnDuskMinuteOK is true
	// only when BOTH boundaries parse, so perception never derives a window from
	// a partial parse (one good + one zero bound).
	dawnMin, duskMin := 0, 0
	dawnOK, duskOK := false, false
	if h, m, err := ParseHM(w.Settings.DawnTime); err == nil {
		dawnMin, dawnOK = h*60+m, true
	}
	if h, m, err := ParseHM(w.Settings.DuskTime); err == nil {
		duskMin, duskOK = h*60+m, true
	}
	snap := &Snapshot{
		AtTick:                        w.TickCounter,
		PublishedAt:                   now,
		Actors:                        make(map[ActorID]*ActorSnapshot, len(w.Actors)),
		Huddles:                       make(map[HuddleID]*Huddle, len(w.Huddles)),
		Scenes:                        make(map[SceneID]*Scene, len(w.Scenes)),
		Structures:                    make(map[StructureID]*Structure, len(w.Structures)),
		Orders:                        make(map[OrderID]*Order, len(w.Orders)),
		VillageObjects:                make(map[VillageObjectID]*VillageObject, len(w.VillageObjects)),
		Quotes:                        make(map[QuoteID]*SceneQuote, len(w.Quotes)),
		PayLedger:                     make(map[LedgerID]*PayLedgerEntry, len(w.PayLedger)),
		LaborLedger:                   make(map[LaborID]*LaborOffer, len(w.LaborLedger)),
		ActionLog:                     CloneActionLog(w.ActionLog),
		ContactLedger:                 CloneContactLedger(w.ContactLedger),
		ContactBrakeWindow:            w.ContactBrakeWindow(),
		ContactRecallHorizon:          w.ContactRecallHorizon(),
		CoinRecord:                    CloneCoinRecord(w.CoinRecord),
		CoinRecordWindow:              w.CoinRecordWindow(),
		NoticeboardContent:            make(map[VillageObjectID]*NoticeboardContent, len(w.NoticeboardContent)),
		PriceBook:                     ClonePriceBook(w.PriceBook),
		Environment:                   w.Environment,
		Phase:                         w.Phase,
		LocalMinuteOfDay:              &localMin,
		LocalDateUTC:                  orderDateUTC(now, w.Settings.Location),
		DawnMinute:                    dawnMin,
		DuskMinute:                    duskMin,
		DawnDuskMinuteOK:              dawnOK && duskOK,
		NeedThresholds:                w.Settings.NeedThresholds.Clone(),
		PCPresenceStaleAfter:          PCPresenceStaleAfter(w),
		HuddleLiveWindow:              EffectiveHuddleLiveWindow(w.Settings),
		SeekWorkCoinCeiling:           effectiveSeekWorkCoinCeiling(w.Settings),
		LodgingDefaultWeeklyRate:      w.Settings.LodgingDefaultWeeklyRate,
		LodgingBedtimeMinute:          lodgerBedtimeMinute(w),
		HomeBakesActive:               homeBakesActiveSet(w),
		LodgingCheckOutMinute:         w.Settings.LodgingCheckOutHour * 60,
		RestockReorderPct:             w.Settings.RestockReorderPct,
		StallWearRepairThreshold:      w.Settings.StallWearRepairThreshold,
		StallWearDegradeThreshold:     w.Settings.StallWearDegradeThreshold,
		EquipmentServiceDueThreshold:  w.Settings.EquipmentServiceDueThreshold,
		StallNailsPerRepair:           w.Settings.StallNailsPerRepair,
		StallDegradedProducePct:       w.Settings.StallDegradedProducePct,
		HearthLowMinutes:              w.Settings.HearthLowMinutes,
		StokeWoodPerStoke:             w.Settings.StokeWoodPerStoke,
		GarmentThreadbareFractionX100: w.Settings.GarmentThreadbareFractionX100,
		FarmUpkeepFloor:               w.Settings.FarmUpkeepFloor,
		FarmUpkeepCoinsPerShovel:      w.Settings.FarmUpkeepCoinsPerShovel,
		MerchantCoinFloor:             w.Settings.MerchantCoinFloor,
		DefaultOutdoorSceneRadius:     w.Settings.DefaultOutdoorSceneRadius,
		Assets:                        w.Assets,
		ZoomMinAdmin:                  w.Settings.ZoomMinAdmin,
		ZoomMinRegular:                w.Settings.ZoomMinRegular,
		// Resolved (default-applied) conversation turn-state windows, so
		// perception build reads the same expiry the sim.Speak backstop uses.
		PCAwaitReplyWindow:  w.awaitReplyWindow(KindPC),
		NPCAwaitReplyWindow: w.awaitReplyWindow(KindNPCShared),
		// Aliased, not cloned — immutable post-startup catalogs. See Snapshot.ItemKinds / Snapshot.Recipes.
		ItemKinds:  w.ItemKinds,
		Recipes:    w.Recipes,
		RecipeUses: w.ensureRecipeUses(),
	}
	// LLM-309: the huddles in a silent transactional-futility loop, computed once
	// per publish (a single O(ledger) pass) so the per-actor steer below is a map
	// lookup rather than a per-member ledger walk in this hot, per-command path.
	// Gated on the loop sweep's master enable — the same gate the utterance steer reads.
	var ledgerLoopHuddles map[HuddleID]struct{}
	// LLM-397: the live-deal set for the lingering steer, same once-per-publish
	// posture. A huddle mid-deal must not be told to wrap up — the wind-down line
	// would land on a buyer with coin already on the table.
	var commerceHuddles map[HuddleID]struct{}
	if huddleLoopEnabled(w.Settings) {
		_, ledgerLoopHuddles = ledgerStandoffHuddles(w, now)
		commerceHuddles = ledgerCommerceHuddles(w)
	}
	for id, a := range w.Actors {
		sa := snapshotActor(a, w.TickCounter, w.Settings.degeneracyEnabled())
		// LLM-372: project a returner's durable continuity onto the snapshot here
		// (not in snapshotActor, a *World-less free function) — buildReturnerSnapshot
		// reads World.RecurringVisitors and needs the wall-clock now for recency.
		sa.Returner = buildReturnerSnapshot(w, a, now)
		// LLM-536: project how long this actor's NPC-speech replies are paced by,
		// so perception's turn-line can hold an owed-reply edge live across the
		// wait the pacing imposes. Computed here rather than in snapshotActor for
		// the same reason as Returner above — it needs WorldSettings.
		sa.ReplyPacingWindow = replyPacedCadence(a, now, w.Settings)
		// Co-presence for the unhuddled (ZBBS-WORK-407): precompute who an
		// unhuddled conversational NPC would reach if it spoke now, so perception's
		// "## Around you" line and the speak no-audience gate share one scope rule
		// (colocatedAudienceIDs). Skip the huddled (their company is the huddle
		// roster) and non-conversational kinds (PCs and decoratives get no NPC
		// decision prompt, so the line is never rendered for them).
		if a.CurrentHuddleID == "" {
			switch a.Kind {
			case KindNPCStateful, KindNPCShared:
				sa.ColocatedAudienceIDs = colocatedAudienceIDs(w, a, now)
				// Co-present sleepers in the same scope (ZBBS-WORK-426): surfaced
				// for perception to mark "(asleep)" but kept out of the audience
				// above, so a visible sleeper no longer vanishes from the speaker's
				// "## Around you" while staying a non-target for the speak gate.
				sa.ColocatedSleeperIDs = colocatedSleeperIDs(w, a, now)
			}
		}
		// Co-location for active dwell credits (LLM-68): resolve the named
		// object whose loiter pin owns the actor's tile so perception renders a
		// "you are <verb> at X" self-state line only while the actor is still at
		// the pin — not after a walk-away, when the credit lingers in the map
		// until the next dwell-tick sweep deletes it. Same resolver + radius as
		// the dwell-tick walk-away check (actorAtCreditObject) so the two agree.
		// Only for credit-holders — the sole consumer — to skip the resolve for
		// everyone else.
		if len(a.DwellCredits) > 0 {
			if objID, ok := resolveLoiteringObject(w, a.Pos, LoiterAttributionTiles); ok {
				sa.CurrentLoiterObjectID = objID
			}
		}
		// In-flight timed source activity (LLM-69): project the live window so
		// perception renders a standing "you are picking/eating at X — stay put,
		// walking off abandons it" self-state line, whatever ticks the actor
		// mid-window. Gate on BusyAtSource so an expired-but-unswept window (the
		// next completion sweep clears it) reads as not-engaged rather than
		// still-in-progress. Resolve the refresh primary need world-side for the
		// eat/drink verb; harvest needs none.
		if act := a.SourceActivity; act != nil && a.BusyAtSource(now) {
			sa.SourceActivityKind = act.Kind
			sa.SourceActivityObjectID = act.ObjectID
			if act.Kind == SourceActivityRefresh {
				if obj := w.VillageObjects[act.ObjectID]; obj != nil {
					sa.SourceActivityAttribute = primaryRefreshNeed(obj)
				}
			}
		}
		// In-flight constable rounds route (LLM-514): project the route label (so
		// perception renders "you are walking your rounds" on any tick during the
		// tour) and, only once he has arrived at the current business stop, that
		// stop's object (so the cue adds "you stand before the <business>" at a stop
		// but not mid-walk). Stamped here, not in snapshotActor, because ActiveRoutes
		// lives on *World. Constable-only for now — the other routes surface their
		// effect through the object states they flip, not through route membership.
		if r := w.ActiveRoutes[a.ID]; r != nil && r.Label == AttrConstable {
			sa.RouteLabel = r.Label
			// What he still OWES and where to go next are projected UNCONDITIONALLY —
			// no arrival requirement (LLM-548). The cue's job is to keep a standing
			// obligation legible wherever he happens to be, and nothing else tells
			// him a round is under way; a man at the well needs the count and the
			// next name every bit as much as a man on a doorstep. Gating these on
			// arrival is what used to collapse the cue to a bare "you are walking
			// your rounds" — no place, no count, nowhere to go — at exactly the
			// moments he most needed telling.
			// Where he is STANDING, which is the one part that does need him to have
			// arrived — it is what lets the cue say "you stand before the smithy".
			// Tolerant, because he reaches stops on his own feet and move_to parks
			// him beside the pin rather than on it.
			//
			// It also anchors the count and the next name. Both are measured from
			// where he IS when he is at a stop, and from the cursor otherwise. The
			// stop under his feet must not be counted among the places still ahead of
			// him — that is the "seven places lie ahead" line read by a man standing
			// in the seventh — and it must not be offered as the next one either.
			// Anchoring on the cursor alone would do both for the moment between his
			// arriving somewhere and the beat crediting it.
			anchor := r.StopIdx
			if idx, atStop := r.stopIndexAt(w, a); atStop {
				anchor = idx
				sa.RouteStopObjectID = r.Stops[idx].ObjectID
			}
			sa.RouteStopsAhead = r.unvisitedExcluding(anchor)
			if nextIdx, ok := r.nextUnvisitedFrom(anchor); ok {
				// The move_to token. The next place he still owes, computed the SAME
				// way the beat advancer moves its cursor, so the cue and the record
				// can never name different places (LLM-543).
				sa.RouteNextStopObjectID = r.Stops[nextIdx].ObjectID
			}
		}
		// Per-tick conversational-loop flag (LLM-169): when this actor's huddle is
		// in an armed loop right now (the same huddleLoopArmed signal the loop sweep
		// arms on), perception swaps the reply-pressure nudge for an "you've agreed —
		// act now" steer, nudging the huddle to self-resolve before the sweep's
		// persistence gate silently concludes it. Gated on the loop sweep's master
		// enable so one knob governs all loop handling, and on the conversational NPC
		// kinds — Render is the NPC reactor-tick path (never a PC or decorative), so
		// the flag would be inert noise on any other kind (matches the co-presence gate above).
		if huddleLoopEnabled(w.Settings) && (a.Kind == KindNPCStateful || a.Kind == KindNPCShared) && a.CurrentHuddleID != "" {
			if h := w.Huddles[a.CurrentHuddleID]; h != nil && !huddlePCAttended(h, now) {
				// The steer arms on EITHER a repetitive utterance loop or a silent
				// transactional-futility loop (LLM-309), so an all-mechanical
				// offer→decline standoff gets the same gentle nudge as a chatty one.
				// The endurance arm (LLM-333) steers through the separate
				// ConversationRunLong flag — its situation is "this has gone on and
				// on", not "you keep saying the same thing", and the render line must
				// state what is actually true of the scene. Looping wins when both
				// hold: it is the more specific diagnosis.
				//
				// LLM-397: the lingering arm's steer (ConversationLingering) is last
				// and least specific — it says only "this has run long", which is all
				// that is TRUE of a conversation that has been productive and varied
				// and simply hasn't stopped. It must not borrow the endurance line's
				// "nothing is coming of it": on the live inn conversation that line
				// would have been false — a bowl of porridge had just been bought,
				// paid for, and served. Suppressed mid-deal, like the sweep's arm.
				_, ledgerArmed := ledgerLoopHuddles[a.CurrentHuddleID]
				switch {
				case huddleLoopArmed(w.Settings, h, now) || ledgerArmed:
					sa.ConversationLooping = true
				case huddleEnduranceArmed(w.Settings, h, now):
					sa.ConversationRunLong = true
				case huddleLingeringArmed(w.Settings, h, now) &&
					!huddleCarriesLiveCommerce(w, h, commerceHuddles):
					sa.ConversationLingering = true
				}
			}
		}
		snap.Actors[id] = sa
	}
	for id, h := range w.Huddles {
		snap.Huddles[id] = CloneHuddle(h)
	}
	for id, s := range w.Scenes {
		snap.Scenes[id] = CloneScene(s)
	}
	for id, s := range w.Structures {
		snap.Structures[id] = CloneStructure(s)
	}
	for id, o := range w.Orders {
		snap.Orders[id] = CloneOrder(o)
	}
	for id, v := range w.VillageObjects {
		snap.VillageObjects[id] = CloneVillageObject(v)
	}
	for id, q := range w.Quotes {
		snap.Quotes[id] = CloneSceneQuote(q)
	}
	for id, e := range w.PayLedger {
		snap.PayLedger[id] = ClonePayLedgerEntry(e)
	}
	for id, o := range w.LaborLedger {
		snap.LaborLedger[id] = CloneLaborOffer(o)
	}
	for id, n := range w.NoticeboardContent {
		if n == nil {
			continue
		}
		nc := *n
		snap.NoticeboardContent[id] = &nc
	}
	w.published.Store(snap)
}

// emitNeedsDeltas broadcasts an NPCNeedsChanged for every actor whose
// hunger/thirst/tiredness changed between the prior published snapshot (prev)
// and the one republish just stored. Called once per command from the command
// loop, immediately after republish — a single change-detection point that
// covers every need-mutation path (hourly tick, consumption, movement fatigue,
// item consume, dwell, the tiredness-recovery sweep) without each one emitting.
//
// Needs are a pure display projection: the only consumer is the client's editor
// needs readout (apply_npc_needs_changed). Nothing in the cascade substrate
// reacts — every substrate subscriber is type-switched on its own events, so a
// new event type is inert there. Emitting here (post-command, no ambient root)
// is the supported "fresh root" path in emit().
//
// A newly created actor is absent from prev: its baseline is treated as 0/0/0
// (what npc_created delivers, since that frame's AgentDTO carries no needs), so it
// emits a correcting delta only when it spawned with non-zero needs — never a
// redundant zero frame for the common fresh-at-zero case.
func (w *World) emitNeedsDeltas(prev *Snapshot) {
	if prev == nil {
		return
	}
	cur := w.published.Load()
	if cur == nil {
		return
	}
	for id, a := range cur.Actors {
		hunger, thirst, tiredness := DisplayNeeds(a.Needs)
		var prevHunger, prevThirst, prevTiredness int
		if prevActor, ok := prev.Actors[id]; ok {
			prevHunger, prevThirst, prevTiredness = DisplayNeeds(prevActor.Needs)
		}
		if prevHunger == hunger && prevThirst == thirst && prevTiredness == tiredness {
			continue
		}
		w.emit(&NPCNeedsChanged{
			ActorID:   id,
			Hunger:    hunger,
			Thirst:    thirst,
			Tiredness: tiredness,
		})
	}
}

// dormantDisplayState projects an actor's macro-state to the dormancy token the
// client renders a sleep marker for: "sleeping" or "resting" for the two dormant
// states, "" for everything else (awake). Both sleeping and resting get the same
// Zzz + dim treatment client-side (resting is a short sleep), but the specific
// token is carried so a later build can distinguish them without a wire change.
func dormantDisplayState(s ActorState) string {
	switch s {
	case StateSleeping:
		return "sleeping"
	case StateResting:
		return "resting"
	default:
		return ""
	}
}

// emitDormancyDeltas broadcasts an NPCDormancyChanged for every agent NPC whose
// dormancy token (dormantDisplayState) changed between the prior published
// snapshot (prev) and the one republish just stored. Called once per command from
// the command loop, immediately after emitNeedsDeltas — the same single
// change-detection point, which is what lets one diff cover both the centralized
// sleep transitions and the scattered rest transitions without per-site emits.
//
// Gated to agent NPCs (stateful + shared): PCs carry their own
// pc_sleep_started/pc_sleep_ended frames and a distinct client render path, and
// decoratives never sleep. A newly created actor is absent from prev; its
// baseline token is "" (awake), so a spawn straight into a dormant state emits a
// correcting frame while the common fresh-awake case stays silent.
func (w *World) emitDormancyDeltas(prev *Snapshot) {
	if prev == nil {
		return
	}
	cur := w.published.Load()
	if cur == nil {
		return
	}
	for id, a := range cur.Actors {
		if a.Kind != KindNPCStateful && a.Kind != KindNPCShared {
			continue
		}
		token := dormantDisplayState(a.State)
		var prevToken string
		if prevActor, ok := prev.Actors[id]; ok {
			prevToken = dormantDisplayState(prevActor.State)
		}
		if prevToken == token {
			continue
		}
		w.emit(&NPCDormancyChanged{
			ActorID: id,
			State:   token,
		})
	}
}

// emitCoinsDeltas broadcasts an NPCCoinsChanged for every actor whose purse
// balance changed between the prior published snapshot (prev) and the one
// republish just stored. Called once per command from the command loop,
// immediately after emitDormancyDeltas — the same single change-detection point,
// which is what lets one diff cover every coin-mutation path (pay, pay-with-item,
// order settlement, lodger rebook, the umbilical grant) without per-site emits.
// Coins move on transactions rather than the needs tick, but every transaction
// runs as a command and republishes here, so the per-publish diff catches them all.
//
// Not kind-gated: the editor villager row shows coins for PCs and agent NPCs alike,
// and decoratives never transact so their balance never changes (no emit). A newly
// created actor is absent from prev; its baseline is treated as 0, so a spawn with a
// non-zero starting purse emits one correcting frame while the common fresh-at-zero
// case stays silent.
func (w *World) emitCoinsDeltas(prev *Snapshot) {
	if prev == nil {
		return
	}
	cur := w.published.Load()
	if cur == nil {
		return
	}
	for id, a := range cur.Actors {
		var prevCoins int
		if prevActor, ok := prev.Actors[id]; ok {
			prevCoins = prevActor.Coins
		}
		if prevCoins == a.Coins {
			continue
		}
		w.emit(&NPCCoinsChanged{
			ActorID: id,
			Coins:   a.Coins,
		})
	}
}

// snapshotActor produces an ActorSnapshot — the slim immutable view of an
// actor for consumers.
//
// InventoryHash is a v1 stub (sum of quantities). Future change to a real
// hash (xxhash over sorted kind+qty) is a contained change behind the same
// type.
func snapshotActor(a *Actor, atTick uint64, degeneracyEnabled bool) *ActorSnapshot {
	// Project the EFFECTIVE degeneracy stage (LLM-94): force None when the
	// observer is disabled, so the snapshot-only Stage-1 readers (perception
	// thinning, the move_to gate) lift the moment an operator turns it off —
	// without waiting for the actor's next scored tick to clear the live stage
	// via updateDegeneracy. The live Actor.DegenStage is left as-is; this is the
	// read-path projection only, the same posture as the movement fields.
	degenStage := a.DegenStage
	if !degeneracyEnabled {
		degenStage = DegeneracyNone
	}
	var hash uint64
	var inventoryCopy map[ItemKind]int
	if len(a.Inventory) > 0 {
		inventoryCopy = make(map[ItemKind]int, len(a.Inventory))
	}
	for k, q := range a.Inventory {
		hash += uint64(q)
		inventoryCopy[k] = q
	}
	var toolWearCopy map[ItemKind]int
	if len(a.ToolWear) > 0 {
		toolWearCopy = make(map[ItemKind]int, len(a.ToolWear))
		for k, v := range a.ToolWear {
			toolWearCopy[k] = v
		}
	}
	var garmentWearCopy map[ItemKind]int
	if len(a.GarmentWear) > 0 {
		garmentWearCopy = make(map[ItemKind]int, len(a.GarmentWear))
		for k, v := range a.GarmentWear {
			garmentWearCopy[k] = v
		}
	}
	needsCopy := make(map[NeedKey]int, len(a.Needs))
	for k, v := range a.Needs {
		needsCopy[k] = v
	}
	// Attribute slugs for the editor chip list — sorted keys only, the param
	// payloads stay on the live Actor (the read surface never needs them).
	var attributeSlugs []string
	if len(a.Attributes) > 0 {
		attributeSlugs = make([]string, 0, len(a.Attributes))
		for slug := range a.Attributes {
			attributeSlugs = append(attributeSlugs, slug)
		}
		sort.Strings(attributeSlugs)
	}
	// In-flight movement read-path projection (ZBBS-HOME-336). Value-typed
	// destination fields lifted from the live MoveIntent so perception can
	// remind the subject of its own walk; moveDestKind stays "" when the actor
	// is not moving. Not the live *MoveIntent (that crosses the checkpoint
	// boundary on the full Actor — see the ActorSnapshot doc-comment); this is
	// the read-path view, the same posture as SpriteID / Facing.
	var moveDestKind MoveDestinationKind
	var moveDestStructureID StructureID
	var moveDestObjectID VillageObjectID
	var moveDestPos TilePos
	if a.MoveIntent != nil {
		d := a.MoveIntent.Destination
		moveDestKind = d.Kind
		if d.StructureID != nil {
			moveDestStructureID = *d.StructureID
		}
		if d.ObjectID != nil {
			moveDestObjectID = *d.ObjectID
		}
		if d.Position != nil {
			moveDestPos = *d.Position
		}
	}
	// Deep-copy the presence stamp (no-alias invariant — like CloneActor):
	// the published snapshot must not share the live Actor's *time.Time.
	var lastPCSeenAt *time.Time
	if a.LastPCSeenAt != nil {
		t := *a.LastPCSeenAt
		lastPCSeenAt = &t
	}
	// In-flight production cycle (LLM-319): mirror the scalar view perception
	// renders from. Item == "" means idle (no activity).
	var productionItem ItemKind
	var productionBatchQty int
	var productionRemainingSeconds int64
	if pa := a.ProductionActivity; pa != nil {
		productionItem = pa.Item
		productionBatchQty = pa.BatchQty
		productionRemainingSeconds = pa.RemainingSeconds
	}
	return &ActorSnapshot{
		AtTick:                     atTick,
		DisplayName:                a.DisplayName,
		Kind:                       a.Kind,
		State:                      a.State,
		Role:                       a.Role,
		LLMAgent:                   a.LLMAgent,
		LoginUsername:              a.LoginUsername,
		LastPCSeenAt:               lastPCSeenAt,
		InsideStructureID:          a.InsideStructureID,
		InsideRoomID:               a.InsideRoomID,
		Pos:                        a.Pos,
		CurrentHuddleID:            a.CurrentHuddleID,
		GatherTargetObjectID:       a.GatherTargetObjectID,
		SpriteID:                   a.SpriteID,
		Facing:                     a.Facing,
		MoveDestKind:               moveDestKind,
		MoveDestStructureID:        moveDestStructureID,
		MoveDestObjectID:           moveDestObjectID,
		MoveDestPos:                moveDestPos,
		AttributeSlugs:             attributeSlugs,
		HomeStructureID:            a.HomeStructureID,
		WorkStructureID:            a.WorkStructureID,
		ScheduleStartMin:           copyIntPtr(a.ScheduleStartMin),
		ScheduleEndMin:             copyIntPtr(a.ScheduleEndMin),
		Needs:                      needsCopy,
		InventoryHash:              hash,
		Inventory:                  inventoryCopy,
		ToolWear:                   toolWearCopy,
		GarmentWear:                garmentWearCopy,
		Coins:                      a.Coins,
		Acquaintances:              cloneAcquaintances(a.Acquaintances),
		Relationships:              cloneRelationships(a.Relationships),
		Narrative:                  cloneNarrativeState(a.Narrative),
		Rumors:                     append([]KnownRumor(nil), a.Rumors...), // pure-value deep copy (LLM-387); see ActorSnapshot.Rumors
		AwaitingReplyFrom:          cloneAwaitingReplyFrom(a.awaitingReplyFrom),
		VisitorState:               cloneVisitorState(a.VisitorState),
		BusinessownerState:         cloneBusinessownerState(a.BusinessownerState),
		DwellCredits:               cloneDwellCredits(a.DwellCredits),
		Observed:                   a.Observed.Clone(),
		KnownPlaces:                cloneKnownPlaces(a.KnownPlaces),
		RoomAccess:                 cloneRoomAccess(a.RoomAccess),
		OpenUntil:                  copyTimePtr(a.OpenUntil),
		RestockPolicy:              a.RestockPolicy,
		ProductionItem:             productionItem,
		ProductionBatchQty:         productionBatchQty,
		ProductionRemainingSeconds: productionRemainingSeconds,
		RecentProduce:              append([]ProduceEvent(nil), a.RecentProduce...),
		TickInFlight:               a.TickInFlight,
		TickAttemptID:              a.TickAttemptID,
		DegenStage:                 degenStage,
		PendingSummon:              clonePendingSummon(a.PendingSummon),
		SummonRefusal:              cloneSummonRefusal(a.SummonRefusal),
	}
}
