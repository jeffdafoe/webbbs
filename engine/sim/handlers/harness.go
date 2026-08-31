package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/llm"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/perception"
)

// Preflight snapshot freshness wait (LLM-275). RunTick must not classify a
// tick stale/superseded from a published snapshot that predates the tick's
// own dispatch: such a snapshot reflects nothing at or after the dispatch, so
// a "not in flight" / mismatched-attempt reading off it is just pre-dispatch
// state, not evidence of supersession. waitForFreshSnapshot waits for AtTick
// to pass dispatchTick before any preflight stale decision is made.
//
// The wait is a fast path of runtime.Gosched() yields — the dispatching
// command's republish is normally microseconds away on the world goroutine,
// which Gosched yields to — followed, if that doesn't resolve, by short
// sleeps up to a ceiling. Gosched alone is unsound as a bound: it yields but
// does not force the (possibly saturated) world goroutine to be scheduled, so
// a fixed spin count can expire in microseconds under exactly the high-churn
// load where the snapshot lags most (the false-stale storm LLM-275 fixed).
// Sleeping parks the worker so the scheduler runs the world goroutine.
const (
	// preflightSnapshotGoschedSpins is how many Gosched yields the freshness
	// wait tries before switching to sleeps.
	preflightSnapshotGoschedSpins = 16

	// preflightSnapshotSleepStep is the per-iteration sleep once the Gosched
	// fast path is exhausted, applied until the wait ceiling.
	preflightSnapshotSleepStep = 200 * time.Microsecond

	// DefaultPreflightSnapshotWaitMax is the default ceiling on the freshness
	// wait (HarnessConfig.PreflightSnapshotWaitMax overrides). A tick's LLM
	// call dominates total latency, so tens of ms spent letting a lagging
	// snapshot catch up is cheap next to the seconds a false-stale retry
	// costs via re-emit + reactor jitter.
	DefaultPreflightSnapshotWaitMax = 75 * time.Millisecond
)

// Per-tick default budgets. Settle exact values empirically during PR 3d
// integration; the defaults are conservative.
const (
	// DefaultIterationBudget is the per-tick LLM-call cap, matching v1's
	// agentTickBudget. A tick that does not terminate within budget ends
	// as TickStatusBudgetForced.
	DefaultIterationBudget = 6

	// DefaultMaxToolCallsPerResponse caps the number of tool calls the
	// harness will process per LLM response. Independent of the iteration
	// budget — without it a single 500-call response is a runaway commit
	// path (multi-call invariant 2 in the design note §5).
	DefaultMaxToolCallsPerResponse = 8

	// DefaultMaxObservationRounds is how many observation-only LLM rounds
	// (e.g. the model spends a whole round just calling recall to gather
	// context) are allowed PER TICK on top of the action budget. Such rounds
	// do NOT consume IterationBudget — thinking isn't penalized as acting
	// (ZBBS-WORK-321) — but they're still bounded so a model that only ever
	// recalls can't loop forever: the hard per-tick ceiling is
	// IterationBudget + MaxObservationRounds total LLM rounds.
	DefaultMaxObservationRounds = 3
)

// HarnessConfig is the wiring + budgets the Harness needs. Client and
// Registry are required; the rest have sensible defaults.
type HarnessConfig struct {
	// Client is the provider-neutral LLM client. Required.
	Client llm.Client

	// Registry is the tool registry. Required. The harness reads
	// AdvertisedSpecs() once per tick to build Request.Tools, and
	// dispatches validated calls through the entries returned by the
	// validator.
	Registry *Registry

	// Validator is the call validator. Optional; defaults to
	// NewValidator(Registry).
	Validator *Validator

	// IterationBudget caps per-tick ACTION iterations (rounds that dispatch
	// a commit or end the tick). Zero → DefaultIterationBudget.
	IterationBudget int

	// MaxObservationRounds caps per-tick observation-only rounds (e.g. a
	// round the model spends only calling recall). These do NOT count
	// against IterationBudget — thinking isn't penalized as acting — but
	// the per-tick hard ceiling is IterationBudget + MaxObservationRounds
	// total LLM rounds. Zero → DefaultMaxObservationRounds.
	MaxObservationRounds int

	// MaxToolCallsPerResponse caps the number of tool calls processed
	// per LLM response. Zero → DefaultMaxToolCallsPerResponse.
	MaxToolCallsPerResponse int

	// PerceptionRenderConfig controls prompt-render limits. Zero-valued
	// fields fall back to perception.DefaultRenderConfig() defaults.
	PerceptionRenderConfig perception.RenderConfig

	// ToolDispatchTimeout caps how long a single commit-tool dispatch
	// (sim.RunTickToolCommand → World.SendContext) is allowed to take.
	// Zero → no harness-imposed timeout beyond the parent ctx.
	ToolDispatchTimeout time.Duration

	// PreflightSnapshotWaitMax caps how long RunTick's preflight waits for
	// the published snapshot to catch up to the tick's dispatch before it
	// gives up and treats the tick as a snapshot-lag retry (LLM-275). Zero →
	// DefaultPreflightSnapshotWaitMax. A worker-side timing knob, kept here
	// (like ToolDispatchTimeout) rather than in world-goroutine WorldSettings.
	PreflightSnapshotWaitMax time.Duration

	// PromptSink, when set, receives each tick's rendered deliberation prompt
	// for the operator-gated umbilical debug surface (ZBBS-HOME-360). Nil (the
	// umbilical-disabled default) means prompts are not captured — zero cost.
	PromptSink sim.PromptSink

	// ChatSink, when set, receives the engine<->model chat exchange (the
	// rendered perception tx + each round's response rx) keyed by scene for the
	// operator-gated umbilical (ZBBS-HOME-382). Nil = not captured, zero cost.
	ChatSink sim.ChatSink

	// Clock is injectable for tests. Zero → time.Now.
	Clock func() time.Time
}

// Harness is the real tickRunner — replaces PR 3b's stubRunner. It owns
// the per-tick iteration loop: preflight stale-check, perception build +
// render (once per tick), within-tick transcript continuation, multi-call
// dispatch by tool class, attempt-guarded commits via
// sim.RunTickToolCommand, and the LLM error classification table.
//
// All RunTick exit paths return a populated sim.TickResult; the worker
// (handlers/worker.go) ferries it to sim.CompleteReactorTick exactly
// once. The harness does NOT call CompleteReactorTick directly.
type Harness struct {
	client    llm.Client
	registry  *Registry
	validator *Validator

	iterationBudget          int
	maxObservationRounds     int
	maxToolCallsPerResponse  int
	renderConfig             perception.RenderConfig
	toolDispatchTimeout      time.Duration
	preflightSnapshotWaitMax time.Duration

	// promptSink captures rendered deliberation prompts for the umbilical
	// (ZBBS-HOME-360); nil when the umbilical is disabled.
	promptSink sim.PromptSink

	// chatSink captures the engine<->model chat exchange per scene for the
	// umbilical (ZBBS-HOME-382); nil when the umbilical is disabled.
	chatSink sim.ChatSink

	clock func() time.Time
}

// NewHarness constructs a Harness from cfg. Returns an error on missing
// required fields (Client, Registry) — a wiring bug that should fail
// loudly at startup rather than mid-tick.
func NewHarness(cfg HarnessConfig) (*Harness, error) {
	if cfg.Client == nil {
		return nil, errors.New("handlers: HarnessConfig.Client is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("handlers: HarnessConfig.Registry is required")
	}
	v := cfg.Validator
	if v == nil {
		v = NewValidator(cfg.Registry)
	}
	if cfg.IterationBudget <= 0 {
		cfg.IterationBudget = DefaultIterationBudget
	}
	if cfg.MaxObservationRounds <= 0 {
		cfg.MaxObservationRounds = DefaultMaxObservationRounds
	}
	if cfg.MaxToolCallsPerResponse <= 0 {
		cfg.MaxToolCallsPerResponse = DefaultMaxToolCallsPerResponse
	}
	if cfg.PreflightSnapshotWaitMax <= 0 {
		cfg.PreflightSnapshotWaitMax = DefaultPreflightSnapshotWaitMax
	}
	clk := cfg.Clock
	if clk == nil {
		clk = time.Now
	}
	return &Harness{
		client:                   cfg.Client,
		registry:                 cfg.Registry,
		validator:                v,
		iterationBudget:          cfg.IterationBudget,
		maxObservationRounds:     cfg.MaxObservationRounds,
		maxToolCallsPerResponse:  cfg.MaxToolCallsPerResponse,
		renderConfig:             cfg.PerceptionRenderConfig,
		toolDispatchTimeout:      cfg.ToolDispatchTimeout,
		preflightSnapshotWaitMax: cfg.PreflightSnapshotWaitMax,
		promptSink:               cfg.PromptSink,
		chatSink:                 cfg.ChatSink,
		clock:                    clk,
	}, nil
}

// waitForFreshSnapshot returns the newest published snapshot together with
// whether it is fresh enough to make a preflight stale/superseded decision
// for a job dispatched at dispatchTick — i.e. its AtTick is strictly past
// dispatchTick, so it reflects world state at or after this attempt's
// dispatch. A snapshot at or below dispatchTick predates the dispatch and
// therefore cannot witness a supersession of this attempt; classifying the
// attempt stale from it is the LLM-275 false positive.
//
// The wait starts with runtime.Gosched() yields (the dispatching command's
// republish is normally microseconds away on the world goroutine, which
// Gosched yields to, so this resolves without sleeping in the common case),
// then backs off to short sleeps up to h.preflightSnapshotWaitMax. Sleeping —
// unlike Gosched, which merely offers to yield — parks the worker so the
// scheduler runs a saturated world goroutine. The returned duration is how
// long the wait took, surfaced on TickResult.PreflightWait for tuning.
//
// ctx cancellation (worker-pool shutdown) is observed once the snapshot has
// been read and is still not fresh, so a cancelled worker returns within at
// most one sleep step (preflightSnapshotSleepStep) instead of sitting out the
// whole ceiling — which matters because PreflightSnapshotWaitMax is
// operator-configurable. The check follows the Published() read so the
// returned snap is non-nil (letting the caller reach the shutdown branch
// rather than the missing-snapshot branch); a ready-fresh snapshot always
// wins, since RunTick's own iteration loop then observes the cancellation. A
// cancelled wait returns fresh=false; the caller distinguishes it from a plain
// lag timeout via ctx.Err() and maps it to TickStatusShutdown. The small fixed
// sleep step keeps cancel latency tight without allocating a timer per
// iteration (a lag spell can be hundreds of iterations, all on the hot path).
//
// dispatchTick == 0 marks a hand-built test job with no real dispatch: there
// is nothing to wait for, so the current snapshot is reported fresh.
func (h *Harness) waitForFreshSnapshot(ctx context.Context, w *sim.World, dispatchTick uint64) (snap *sim.Snapshot, fresh bool, waited time.Duration) {
	waitMax := h.preflightSnapshotWaitMax
	if waitMax <= 0 {
		waitMax = DefaultPreflightSnapshotWaitMax
	}
	start := h.clock()
	deadline := start.Add(waitMax)
	for spins := 0; ; spins++ {
		snap = w.Published()
		if snap == nil {
			return nil, false, h.clock().Sub(start)
		}
		if dispatchTick == 0 || snap.AtTick > dispatchTick {
			return snap, true, h.clock().Sub(start)
		}
		// Not fresh yet — bail if the worker is being shut down. DELIBERATE:
		// cancellation is observed only between sleeps (not mid-sleep), so
		// post-cancel latency is at most one preflightSnapshotSleepStep. This
		// trades ~200µs of shutdown latency for zero per-iteration timer
		// allocation — a lag spell is hundreds of iterations on the tick hot
		// path. Do NOT "fix" this into a per-iteration time.NewTimer/select;
		// if immediate cancellation is ever needed, reuse ONE timer across the
		// whole loop and keep the step small.
		if ctx.Err() != nil {
			return snap, false, h.clock().Sub(start)
		}
		now := h.clock()
		if !now.Before(deadline) {
			return snap, false, now.Sub(start)
		}
		if spins < preflightSnapshotGoschedSpins {
			runtime.Gosched()
			continue
		}
		// Clamp the final sleep so the wait never overshoots the ceiling by
		// more than scheduler slop.
		sleep := preflightSnapshotSleepStep
		if remaining := deadline.Sub(now); remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

// RunTick implements the tickRunner interface. Always returns a populated
// sim.TickResult — every code path sets TerminalStatus, Duration, and the
// diagnostic fields the worker passes to telemetry + CompleteReactorTick.
//
// Multi-call invariant 6 in the design note ("CompleteReactorTick exactly
// once on every exit path") is enforced by the worker, not by RunTick —
// the worker calls CompleteReactorTick once after RunTick returns
// regardless of which path RunTick took. RunTick's contract is: always
// return, never panic.
func (h *Harness) RunTick(ctx context.Context, w *sim.World, job tickJob) (result sim.TickResult) {
	start := h.clock()
	// Named return so the deferred Duration stamp mutates the slot the
	// caller observes. A non-named return would copy result into the
	// return slot BEFORE the defer ran, leaving callers with Duration=0.
	defer func() { result.Duration = h.clock().Sub(start) }()

	result = sim.TickResult{
		AttemptID:      job.attemptID,
		ActorID:        job.actorID,
		TerminalStatus: sim.TickStatusUnknown,
	}

	// --- preflight: snapshot read + stale check ---
	// Cheap: no world goroutine round-trip, no LLM tokens spent.
	//
	// This job was enqueued from inside the dispatching command's synchronous
	// emit (subscriber.go's handleEvent runs inline on the world goroutine),
	// but the snapshot reflecting our TickInFlight dispatch is not republished
	// until that command returns (world.go command loop: Fn → TickCounter++ →
	// republish). Until then the newest published snapshot still shows the
	// PRE-dispatch actor state (TickInFlight=false), which must NOT be read as
	// a supersession. waitForFreshSnapshot blocks (Gosched fast path, then
	// short sleeps) until the snapshot reaches past our dispatch tick, so the
	// stale check below only ever runs against a snapshot fresh enough to
	// witness it (LLM-275).
	snap, fresh, wait := h.waitForFreshSnapshot(ctx, w, job.dispatchTick)
	result.PreflightWait = wait
	if snap == nil {
		// Defensive: a missing published snapshot means the world has not
		// been initialized for snapshots, which is a wiring bug. Carry the
		// batch forward as before-render.
		return failBeforeRender(result, job, "")
	}
	if !fresh {
		if ctx.Err() != nil {
			// Worker-pool shutdown / cancellation interrupted the freshness
			// wait before this tick rendered. Classify as shutdown (mirroring
			// the iteration-loop cancellation path below), NOT a snapshot-lag
			// retry — the world is going away. Carry the consumed batch
			// forward for symmetry with the other before-render exits; the
			// completion won't land anyway (SendContext fails on the dead
			// ctx) and post-restart re-engagement re-warrants the actor.
			result.TerminalStatus = sim.TickStatusShutdown
			result.LLMErrorClass = llm.ErrorContextCancelled.String()
			result.UnaddressedWarrants = copyWarrants(job.warrants)
			return result
		}
		// The freshness wait expired with the snapshot still predating our
		// dispatch — the world goroutine is lagging (a high-churn actor
		// saturates it). A snapshot older than our own dispatch cannot
		// witness a supersession of this attempt, so reading TickInFlight /
		// TickAttemptID / actor-presence off it would be a false-positive
		// stale (the LLM-275 storm). Carry the whole consumed batch forward
		// for a clean retry under a distinct label; the world-goroutine guards
		// (RunTickToolCommand, CompleteReactorTick) remain the authoritative
		// supersession protection. No LLM call happened; nothing was addressed.
		result.TerminalStatus = sim.TickStatusStale
		result.StaleStage = sim.StaleStageSnapshotLag
		result.UnaddressedWarrants = copyWarrants(job.warrants)
		return result
	}
	actor, ok := snap.Actors[job.actorID]
	if !ok {
		// Actor absent from a FRESH snapshot: genuinely gone (deleted /
		// off-stage since dispatch). An old snapshot could not prove this,
		// which is why the freshness gate above runs first.
		return failBeforeRender(result, job, "")
	}
	if !actor.TickInFlight || actor.TickAttemptID != job.attemptID {
		// The world has moved past this attempt (typed out, superseded),
		// witnessed by a snapshot fresh enough to prove it. All consumed
		// warrants carry forward — none of them have been addressed.
		result.TerminalStatus = sim.TickStatusStale
		result.StaleStage = sim.StaleStageBeforeRender
		result.UnaddressedWarrants = copyWarrants(job.warrants)
		return result
	}

	// --- perception build (cheap; no rendering yet) ---
	payload := perception.Build(snap, job.actorID, job.warrants)

	// LLM-186 diagnostic: a worker the world holds in StateLaboring whose
	// perception shows no in-progress job (Payload.Laboring == nil) is the
	// PW-Apothecary inconsistency — the seek-work directive stays live and the
	// hired worker is steered to re-solicit. Rare by construction (the snapshot is
	// a coherent point-in-time clone), so this warn fires only on the bug.
	if actor.State == sim.StateLaboring && payload.Laboring == nil {
		log.Printf("sim/labor: WARN actor %s is StateLaboring but perception Laboring is nil — hired worker would be steered to seek work [LLM-186] (tick=%d)", job.actorID, snap.AtTick)
	}

	// Degeneracy observer yield facts (LLM-94). Derived from this tick's
	// perception so CompleteReactorTick can score the tick without
	// re-perceiving. Set unconditionally here (cheap field reads); harmless on
	// the skip path below, since CompleteReactorTick only scores substantive
	// ticks. The loop-detection Diff (Primary.Diff) is non-nil only when the
	// baseline is present.
	result.BaselinePresent = payload.Baseline == perception.BaselinePresent
	if payload.Primary != nil && payload.Primary.Diff != nil {
		result.StateChanged = payload.Primary.Diff.AnyChange
	}
	result.HadAudience = payload.Surroundings.HasAudience()

	// --- noop-skip preflight (pre-LLM gate) ---
	// If perception has nothing actionable (no co-present peer, no need
	// at red) AND the consumed batch carries only low-information warrant
	// kinds (idle-backstop / huddle-concluded / huddle-left), the LLM
	// call would produce a noop tick. Skip it — the consumed warrants
	// land in recently-consumed via terminalStatusAddresses so they
	// don't re-fire on the next scan.
	//
	// Replaces v1's salem-vendor-only skip at engine/agent_tick.go:
	// 211-221 (ZBBS-WORK-235). Order is deliberate: this runs BEFORE
	// perception.Render, so the prompt-build allocations are skipped
	// too on the gate-hit path.
	if shouldSkipNoop(payload, snap.NeedThresholds, job.warrants) {
		result.TerminalStatus = sim.TickStatusSkipped
		return result
	}

	// --- render (allocates the prompt) ---
	rendered := perception.Render(payload, h.renderConfig)
	// Capture the rendered prompt for the umbilical debug surface (ZBBS-HOME-360),
	// before the transcript continuation appends tool results — this is the
	// perception the model deliberated from. Nil sink (umbilical off) → skipped.
	// Placed after the noop-skip gate so only actually-rendered prompts are
	// captured, never a tick that never built one.
	if h.promptSink != nil {
		h.promptSink.WritePrompt(sim.PromptRecord{
			At:        h.clock().UTC(),
			ActorID:   job.actorID,
			AttemptID: job.attemptID,
			Prompt:    fullPerceptionPrompt(rendered),
		})
	}
	// Render-time drops are the "consumed but not addressed" set the
	// harness must carry forward. Subsequent paths append to this set on
	// failures (e.g. mid-tick LLM error).
	if len(rendered.DroppedWarrants) > 0 {
		result.UnaddressedWarrants = copyWarrants(rendered.DroppedWarrants)
	}
	// LLM-542: the mirror of the above — stimuli the prompt DID contain but
	// whose warrant this attempt never consumed, because they landed between
	// the emit and the snapshot this perception was built from. Collected here,
	// after Render, so a tick that never built a prompt reports none.
	result.DischargedSourceKeys = perception.CollectDischargedSourceKeys(payload, rendered.DroppedWarrants)

	// --- transcript init ---
	// PR 3d ships single-user-message perception. A separate system
	// prompt is the cutover layer's responsibility (the VA system loads
	// <Self> from context/soul there) — see PR 3d design note §3.1.
	transcript := []llm.Message{
		{Role: llm.RoleUser, Content: rendered.Text},
	}
	tools := gateTools(h.registry, payload, snap)

	// Move targets this tick's perception surfaced — collected once and threaded
	// into every tool dispatch so move_to can resolve a place NAME to any id the
	// actor was shown (a distant vendor / rest cue), not just anchors + scene
	// radius. ZBBS-HOME-389.
	perceivedPlaces := perception.CollectPerceivedPlaces(payload)

	// The actor's DURABLE known places (LLM-77) — split by kind and threaded to
	// move_to as the name-resolver's memory-backed FALLBACK source (LLM-78), so
	// the model can walk by name to a place it has personally experienced even
	// when this tick's cues don't name it. Collected off the published snapshot,
	// like perceivedPlaces; the live (shown) sources still win a shared name.
	rememberedPlaces := sim.CollectRememberedPlaces(actor.KnownPlaces)

	// Scene + VA-routing context for every Complete + persist call this
	// tick. SceneID is minted once and reused so the API's per-scene
	// history loader (chat_messages.scene_id filter) sees a coherent
	// conversation across iterations. Model is the actor's VA slug.
	sceneID := llm.NewSceneID()
	model := actor.LLMAgent
	// ZBBS-HOME-397: the conversation_id grouping key, threaded alongside the
	// per-tick sceneID so memory-api can group the whole exchange under one
	// conversation in the admin chat viewer (see conversationIDFromPayload).
	conversationID := conversationIDForChat(actor, payload)

	// ZBBS-HOME-382: capture the rendered perception (tx) into the per-scene
	// chat ring so the umbilical /chat route shows what the model was sent
	// alongside its responses (rx, captured per round below). Keyed by the same
	// sceneID stamped on the llm-memory chat rows. Nil sink -> skipped (zero cost).
	if h.chatSink != nil {
		h.chatSink.WriteChat(sim.ChatRecord{
			At:        h.clock().UTC(),
			SceneID:   sceneID,
			ActorID:   job.actorID,
			AttemptID: job.attemptID,
			Model:     model,
			Direction: "perception",
			Content:   fullPerceptionPrompt(rendered),
		})
	}

	// Defer persist-on-exit for the orphan-tool_use case. v1's bug: a
	// terminal-class tool (done() / TerminalOnSuccess commit) ends the
	// tick without firing another Complete to deliver tool_result rows
	// to the API. The assistant message that contained the terminal
	// tool_call sits in chat_messages history with no matching tool_use
	// row, breaking the NEXT tool-use call against the same VA
	// ("tool_use without tool_result"). The defer runs on every exit
	// path; persistTickToolResults gates on TerminalStatus + the
	// presence of trailing tool messages, so spurious exits skip the
	// call. See engine/sim/llm/memapi package doc.
	defer func() {
		h.persistTickToolResults(ctx, model, sceneID, conversationID, transcript, result.TerminalStatus)
	}()

	// --- iteration loop ---
	// IterationBudget bounds ACTION rounds (a round that dispatches a commit
	// or ends the tick). Observation-only rounds — the model spends a whole
	// LLM round just gathering context (recall) — do NOT consume it
	// (ZBBS-WORK-321: thinking isn't penalized as acting). maxTotalRounds is
	// the hard ceiling so a model that only ever recalls can't loop forever.
	maxTotalRounds := h.iterationBudget + h.maxObservationRounds
	actionRounds := 0
	// offeredThisTick holds the dedup key of every pay_with_item OFFER this actor
	// has successfully placed this tick (ZBBS-HOME-395 same-tick repeat-offer
	// guard). Pre-395 a placed offer came
	// back as a bare [ok] with no "now pending, await their answer" signal, so the
	// model had no within-tick reason to stop and re-offered the same item to the
	// same seller every round to the iteration budget (the Josiah×Moses carrot
	// storm). Each offer also spawned its own ledger row that later rendered a
	// separate "fell through" line, so the storm multiplied the NEXT tick's
	// perception too. One offer per (seller, item, disposition) per tick: after
	// that the buyer awaits the seller's accept/decline/counter — at any terms.
	// See payOfferKey for the keying rationale (price AND qty deliberately
	// excluded).
	offeredThisTick := map[string]struct{}{}
	// paidThisTick holds the dedup key of every bare `pay` this actor has
	// successfully SETTLED this tick (LLM-202 — the pay analogue of
	// offeredThisTick). Unlike pay_with_item's offer, a bare pay settles coins
	// instantly and irreversibly, with no pending ledger row and no "now
	// pending" signal, so a weak model that re-emits the identical call settles
	// it a SECOND time (live: John Ellis paid Silence Walker 4 coins twice in one
	// tick, 8 coins for one verbal arrangement). `pay` carries no resolution id to
	// key on (it has no ledger), so the key is (recipient, for) — amount excluded,
	// the same rationale that excludes price from payOfferKey, so a re-fire at a
	// drifted amount still matches. The `for` text IS in the key so one tick can
	// still carry two genuinely distinct payments to the same recipient (a wage
	// and a separate gift); only a repeat of the same (recipient, reason) is the
	// double-settle. Recorded on SUCCESS like offeredThisTick — a pay that bounced
	// (insufficient funds, a wrong-channel redirect) moved no coins and stays
	// retryable.
	paidThisTick := map[string]struct{}{}
	// quotedThisTick holds the dedup key of every scene_quote this actor has
	// successfully POSTED this tick (ZBBS-HOME-433 — the seller-side analogue
	// of offeredThisTick). Pre-433 a posted quote came back as a bare [ok], so
	// the model had no within-tick reason to stop and re-posted the identical
	// quote every round to the iteration budget (the live John×Ezekiel bread
	// storm: five scene_quote calls in one tick). One quote per (item, qty,
	// disposition, target) per tick; see sceneQuoteKey for the keying
	// rationale (price deliberately excluded, qty kept).
	quotedThisTick := map[string]struct{}{}
	// triedThisTick holds the identical-call key (name + canonical decoded args)
	// of every action this actor has ATTEMPTED this tick under the ZBBS-HOME-414
	// general guard — the action tools without their own same-tick guard. Recorded
	// on FIRST attempt regardless of outcome (unlike offeredThisTick, which records
	// on success), so a byte-identical retry of a call that itself
	// FAILED is rejected, not just a repeat of a successful one — the degenerate
	// case is a deliver_order(7) re-fired after the hand-over already failed. See the
	// guard in the dispatch loop and genericCallKey for what is in/out of scope.
	triedThisTick := map[string]struct{}{}
	// gatheredThisTick / craftedThisTick record that this actor has SUCCESSFULLY
	// started a gather / chosen a production focus this tick (LLM-120). Both are
	// name-only flags (not genericCallKey's name+args) because each tool's args
	// don't distinguish a meaningful re-fire: gather's `qty` is vestigial (LLM-87)
	// and craft's item resolves through aliases (LLM-113: Nail/nail/nails → one
	// kind). Set in the outcome.success block so a bounced first attempt isn't
	// recorded and a legitimate retry still lands (mirrors offeredThisTick). The
	// dispatch-loop guards reject a second gather/craft once
	// the flag is set. One of each is all that helps in a tick: a started pick runs
	// for seconds, and a crafter forges one good at a time.
	gatheredThisTick := false
	craftedThisTick := false
	// solicitedThisTick records that this actor has SUCCESSFULLY placed a labor
	// offer this tick (LLM-163, the solicit_work analogue of gatheredThisTick). The
	// one-pending-offer-per-worker rule makes a second solicit this tick always
	// redundant, so a name-only flag suffices; the post-success steer in
	// commitResultContent is the soft half, this guard is the teeth.
	solicitedThisTick := false
	// solicitAttemptedThisTick records that this actor has TRIED to solicit work this
	// tick, success or failure (LLM-195). solicitedThisTick (above) is success-only,
	// and a successful solicit is terminal-on-success (LLM-180) — so the ONLY way to
	// reach a second solicit_work in a tick is a FAILED first one, which the
	// success-only flag never caught. The weak model then re-fires the rejected offer
	// to the round budget (live: solicit_work x6 to a co-resident "employer", each
	// bounced by the co-resident gate). Recorded on the first attempt regardless of
	// outcome, like resolvedLaborThisTick below; a name-only flag, since one work
	// offer per tick is all that helps whether or not it lands.
	solicitAttemptedThisTick := false
	// retriggeredIntent marks a tick that was ITSELF triggered by an LLM-414
	// unfinished_intent warrant. The post-terminal skip loop then leaves
	// result.SkippedIntentTools empty, so an over-batching model gets exactly
	// one retry per split, never a self-sustaining re-tick chain.
	retriggeredIntent := false
	for _, m := range job.warrants {
		if m.Kind() == sim.WarrantKindUnfinishedIntent {
			retriggeredIntent = true
			break
		}
	}
	// resolvedLaborThisTick holds the LaborID of every labor offer this actor has
	// answered this tick as EMPLOYER via accept_work / decline_work. LLM-163 left
	// these two unguarded on the theory that a re-fire "hits the substrate's
	// not-pending gate" so no flag was needed — but that gate returns a RAW
	// "offer N is no longer pending (currently working) — nothing to accept" error,
	// not a model-stopping steer, and the turn-start perception keeps rendering the
	// offer in "## Work offers awaiting your decision" every round (it is not rebuilt
	// mid-turn), so the weak model re-accepts to the iteration budget (live: John
	// Ellis accept_work×6 in one turn against a single pending offer — 1 real accept
	// then 5 raw not-pending errors). This is the exact failure resolvedLedgerThisTick
	// fixes for the pay-offer family (LLM-104); the labor mirror keys on the labor id,
	// shared across accept_work/decline_work, and records on first attempt regardless
	// of outcome (the first answer resolves the offer to working/declined/terminal, so
	// any second answer to the same id this tick is provably useless).
	resolvedLaborThisTick := map[LenientID]struct{}{}
	// resolvedLedgerThisTick holds the LedgerID of every pay-offer this actor has
	// already answered this tick via the resolution family (accept_pay / decline_pay
	// / counter_pay / withdraw_pay). The FIRST answer moves the ledger out of
	// `pending`, so any SECOND resolution call against the same id this tick — same
	// tool or a different one — is provably useless and only reaches the command's
	// "no longer pending (currently …)" error. This is a SUPERSET of what the
	// ZBBS-HOME-414 generic guard caught for these four tools (LLM-104): that guard
	// keys on name + full decoded args, so a counter re-fired with a `message` added
	// (the model satisfying the perception's "say a brief word with the counter" cue
	// by stuffing the word into the tool instead of speak), or a counter followed by
	// an accept of that same just-countered ledger, both present as DIFFERENT keys
	// and slipped through. Keying on the ledger id alone, shared across the family,
	// closes both. Recorded on first attempt regardless of outcome, like
	// triedThisTick. The four tools therefore leave genericCallKey's allowlist.
	resolvedLedgerThisTick := map[LenientID]struct{}{}
	// consumedNothingThisTick holds the item key of every consume this actor has
	// made this tick that EASED NO NEED — the actor was already sated for what that
	// item eases (ConsumeResult.EasedNeed == false; it still ate and wasted a unit,
	// since consuming while full wastes a unit by design — ZBBS-WORK-391). This is
	// the LLM-91 semantic replacement for keeping consume on the ZBBS-HOME-414
	// general guard. consume is OFF that guard (see genericCallKey) because a
	// byte-identical repeat while still in need is PRODUCTIVE — it eats another
	// unit and eases the need further. The only senseless consume is one that eases
	// nothing; the first such attempt still dispatches so the model gets the
	// honest "you're full" feedback, and a REPEAT of it is rejected here. Keyed by
	// consumeItemKey (normalized item), recorded after dispatch on a no-op result.
	consumedNothingThisTick := map[string]struct{}{}
	// ephemeralText is the recency-dominant decision-support body sent with each
	// round's Complete call — the full per-tick perception furniture (affordances
	// + act-now coda). The LLM-88 self-state refresh below re-renders it in place
	// when a commit moves the actor's own needs/coins/goods, so a stale eat/drink
	// affordance can't prime a re-fire on a later round.
	ephemeralText := rendered.EphemeralText
	// stableText is the daily-stable identity body (the "## Who you are" soul)
	// sent as Request.StableContext on every round — the adapter routes it to
	// the provider-cached system zone, so re-sending it costs warm-cache prices
	// (LLM-501; this retires LLM-468's rounds-after-the-first soul strip). Not
	// touched by the LLM-88 refresh below: nothing in it is self-state.
	stableText := rendered.StableText
	// lastSelf is the self-state the current ephemeral body reflects. The LLM-88
	// refresh re-renders only when a commit actually moved needs/coins/goods vs
	// this — not on a no-op commit. Seeded with the tick-open snapshot (actor
	// presence checked in preflight); advanced on each refresh.
	lastSelf := snap.Actors[job.actorID]
	// simActorID / simActorName attribute every deliberation turn this tick to
	// the acting in-world actor, so a shared-VA (salem-vendor) turn is logged
	// against the character rather than only the switchboard agent (LLM-236).
	// Resolved once — the actor's identity is fixed for the whole tick.
	simActorID := string(job.actorID)
	simActorName := ""
	if lastSelf != nil {
		simActorName = lastSelf.DisplayName
	}
	// nudgedForBareContent guards the LLM-378 one-shot reprompt below: a weak
	// model under a heavy character prompt sometimes answers a conversational
	// turn as bare assistant prose ("Lewis! Good to see you…") with NO speak
	// tool call. Speech only reaches the scene through the speak commit, so
	// that prose is heard by no one and the waiting party stalls forever. We
	// give the model exactly ONE chance per tick to re-emit its reply through
	// speak(); a second bare-content response falls through to the plain
	// content-only tick end. Flag caps it at one reprompt so a stubborn model
	// can't burn the whole round budget re-narrating.
	nudgedForBareContent := false
	for round := 0; round < maxTotalRounds; round++ {
		result.IterationCount = round + 1

		if err := ctx.Err(); err != nil {
			result.TerminalStatus = sim.TickStatusShutdown
			result.LLMErrorClass = llm.ErrorContextCancelled.String()
			return result
		}

		resp, err := h.client.Complete(ctx, llm.Request{
			Model:            model,
			SceneID:          sceneID,
			ConversationID:   conversationID,
			Messages:         transcript,
			Tools:            tools,
			EphemeralContext: ephemeralText,
			StableContext:    stableText,
			SimActorID:       simActorID,
			SimActorName:     simActorName,
		})
		if err != nil {
			cls := llm.Classify(err)
			result.LLMErrorClass = cls.String()
			result.TerminalStatus = llmErrorToStatus(cls, round)
			return result
		}

		// ZBBS-HOME-382: capture the model's response (rx) into the per-scene
		// chat ring — content + a compact tool-call summary — so the umbilical
		// /chat route shows the full exchange for this scene. Nil sink -> skipped.
		if h.chatSink != nil {
			h.chatSink.WriteChat(sim.ChatRecord{
				At:        h.clock().UTC(),
				SceneID:   sceneID,
				ActorID:   job.actorID,
				AttemptID: job.attemptID,
				Model:     model,
				Direction: "response",
				Content:   resp.Content,
				ToolCalls: summarizeToolCalls(resp.ToolCalls),
			})
		}

		// No tool calls = content-only response. Two cases:
		//
		//  1. LLM-378: the model wrote a reply as bare prose but never called
		//     speak(), so nothing reaches the scene and the party it was
		//     answering waits forever. When there is non-empty content here,
		//     we haven't already reprompted this tick, AND a reprompt round is
		//     still left, append the model's own line and steer it to say the
		//     words through speak() (or done() if it truly has nothing to
		//     say), then loop once more. This lets the model re-emit CLEAN
		//     speech text — its `*stage narration*` stays in content and out
		//     of the "X said:" line — rather than us guessing how to split it.
		//     The predicate is "non-empty content", not speech detection: a
		//     line the model meant as private musing gets the same steer and
		//     simply calls done() out of it.
		//
		//  2. Genuine end-of-turn: empty content, the model already got its
		//     one reprompt, or no reprompt round remains (a nudge here would
		//     just fall through the exhausted loop to a misleading
		//     BudgetForced with the reply still dropped). Treat as the model
		//     being done — successful tick end, as before.
		if len(resp.ToolCalls) == 0 {
			if !nudgedForBareContent && strings.TrimSpace(resp.Content) != "" && round+1 < maxTotalRounds {
				nudgedForBareContent = true
				transcript = append(transcript, llm.Message{
					Role:    llm.RoleAssistant,
					Content: resp.Content,
				})
				transcript = append(transcript, llm.Message{
					Role: llm.RoleUser,
					Content: "[not spoken] No one heard that — words reach others only through the speak tool, not by writing them in your reply. " +
						"Call speak() now with what you meant to say aloud, or done() if you truly have nothing to say.",
				})
				continue
			}
			result.TerminalStatus = sim.TickStatusSuccess
			return result
		}

		// Append the assistant message with ALL tool calls (the provider
		// requires the assistant turn to carry every call ID it emitted;
		// truncated calls still need a matching `tool` reply below).
		transcript = append(transcript, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: append([]llm.RawToolCall(nil), resp.ToolCalls...),
		})

		// Multi-call cap (invariant 2). Calls beyond the cap are dropped
		// and surfaced as typed errors.
		calls := resp.ToolCalls
		var truncated []llm.RawToolCall
		if len(calls) > h.maxToolCallsPerResponse {
			truncated = calls[h.maxToolCallsPerResponse:]
			calls = calls[:h.maxToolCallsPerResponse]
		}

		// observationOnly tracks whether this round was pure thinking (every
		// call a successfully-dispatched observation). Dropped (truncated)
		// calls mean the model tried to act beyond the cap — not a clean
		// think — so the round counts against the action budget.
		observationOnly := true
		if len(truncated) > 0 {
			observationOnly = false
		}

		// Walk in-budget calls in order. A terminal call ends the batch.
		batchEnded := false
		var endedAt int
		var endedStatus sim.TickTerminalStatus

		for i, call := range calls {
			result.ToolsRequested = append(result.ToolsRequested, call.Name)

			// Validate.
			vc, verr := h.validator.Validate(call)
			if verr != nil {
				observationOnly = false // a malformed action attempt isn't a clean think
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
				transcript = append(transcript, toolResultMsg(call.ID, formatValidationError(verr)))
				continue // invariant 4: validation failure is non-terminal
			}
			if vc.Entry.Class != ClassObservation {
				observationOnly = false
			}

			// ZBBS-HOME-395: same-tick repeat-offer guard — the pay analogue of
			// the speak guard above. A buyer that placed an offer and got a bare
			// [ok] back (no "now pending" signal pre-395) re-offered the same item
			// to the same seller every round to the iteration budget, each offer
			// spawning a ledger row that later rendered its own "fell through"
			// line (the observed Josiah×Moses carrot storm). Reject a second offer
			// for the same (seller, item, disposition) this tick model-facing so
			// the buyer awaits the seller's answer or calls done(). One offer per
			// (seller, item, disposition) per tick: the key excludes BOTH price
			// and qty (see payOfferKey), so a re-offer at drifting terms (5 coins,
			// then 10; or a changed quantity) still matches — the buyer reconsiders
			// next tick, after the seller responds. Quote-take and counter-response
			// paths are exempt (see payOfferKey).
			if key, isOffer := payOfferKey(vc); isOffer {
				if _, dup := offeredThisTick[key]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_offered] you already made an offer for that item to that seller this turn — wait for their answer, or call done()."))
					continue
				}
			}

			// LLM-202: same-tick repeat-PAY guard — the bare-pay analogue of the
			// repeat-offer guard above. A bare pay settles coins instantly and
			// irreversibly with no "now pending" signal, so a weak model that
			// re-emits the identical call settles it a second time (the live John
			// Ellis pay×2 double). Reject a second pay for the same (recipient,
			// reason) this tick model-facing so the coins move once. Recorded on
			// SUCCESS below (paidThisTick) — a bounced pay moved nothing and stays
			// retryable. Keyed on (recipient, for) via payDedupKey, amount excluded.
			if key, isPay := payDedupKey(vc); isPay {
				if _, dup := paidThisTick[key]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_paid] you already paid them for that this turn — the coins have moved; do not pay again, say a word or call done()."))
					continue
				}
			}

			// ZBBS-HOME-433: same-tick repeat-QUOTE guard, the seller-side
			// analogue of the offer guard above. A posted quote already stands
			// before the whole scene — re-posting it (at any price) buys
			// nothing within this tick; the buyer answers on their own tick.
			if key, isQuote := sceneQuoteKey(vc); isQuote {
				if _, dup := quotedThisTick[key]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_quoted] your offer for that item already stands from this turn — the room has heard it; await an answer or call done()."))
					continue
				}
			}

			// LLM-104: pay-offer resolution same-tick guard, shared across the
			// resolution family (accept_pay / decline_pay / counter_pay /
			// withdraw_pay). One answer per offer per tick: the first resolution
			// moves the ledger out of `pending`, so a second call against the same
			// id this tick is a guaranteed no-op that only reaches the command's
			// "no longer pending" error. Keyed on the LEDGER ID alone — not name +
			// args like genericCallKey below — so it catches the two slip-throughs
			// that guard missed: a counter re-fired with its spoken `say` added
			// (different args → different generic key), and a counter followed by an
			// accept of that same just-countered ledger (different tool name →
			// different generic key). Recorded on first attempt regardless of outcome.
			// The reject below may still name speak: a rejected call is NOT terminal,
			// so the tick continues and the model can reach it (unlike the success
			// path, whose terminal response ends the tick — LLM-350).
			if id, ok := ledgerResolutionID(vc); ok {
				if _, dup := resolvedLedgerThisTick[id]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_did_that] you already answered that offer this turn — it's the other party's move now; call speak or done()."))
					continue
				}
				resolvedLedgerThisTick[id] = struct{}{}
			}

			// LLM-120: same-tick gather + craft guards. Both tools STARTED a within-
			// tick re-fire loop in the wild — gather at a Blueberry Bush, craft×6 at the
			// forge — because a repeat fell through to a domain error (gather "already
			// busy") or a bare [ok] re-invite (craft), and the weak model re-fired to
			// the iteration budget. Each is keyed on the tool NAME ALONE, not
			// genericCallKey's name+args: gather's `qty` is vestigial (LLM-87 — it
			// always picks the source clean) and craft's item resolves through aliases
			// (LLM-113: Nail/nail/nails → one kind), so a byte-identical key would miss
			// a drifted re-fire of either. Both RECORD ON SUCCESS (in the outcome.success
			// block below), mirroring speak/offer/quote: a bounced first attempt isn't
			// recorded, so a legitimate retry still lands — gather that bounced on the
			// wrong spot (its only in-tick fix, move_to, is terminal and ends the tick
			// anyway), or craft that named an unmakeable good and is re-called with a
			// valid one. The reject is the model-facing teeth; the post-success steer in
			// commitResultContent is the soft half.
			if vc.Name == "gather" && gatheredThisTick {
				observationOnly = false
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
				transcript = append(transcript, toolResultMsg(call.ID, "[error: already_gathering] you're already gathering here this turn — the pick is under way and the harvest lands in your pack shortly; do not gather again, just wait or call done()."))
				continue
			}
			if vc.Name == craftToolName && craftedThisTick {
				observationOnly = false
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
				transcript = append(transcript, toolResultMsg(call.ID, "[error: already_chose] you already chose what to produce this turn — your focus is set and you keep making it until you choose again; do not choose again now, tend your post or call done()."))
				continue
			}
			// solicit_work same-tick guard. Two re-fire cases, both observed live:
			//   - solicitedThisTick: a prior solicit this tick SUCCEEDED. terminal-on-
			//     success (LLM-180) normally ends the tick before a second call, so this
			//     arm is belt-and-suspenders — the offer stands, wait for the answer.
			//   - solicitAttemptedThisTick: a prior solicit this tick FAILED. This is the
			//     real case (LLM-195) — a successful solicit is terminal, so a second
			//     solicit is only reachable after a failed first one, which the success-
			//     only solicitedThisTick flag never recorded. The weak model re-fires the
			//     bounced offer to the round budget (live: solicit_work x6 to a co-
			//     resident "employer"). Name-only: varying the reward dodges a byte-
			//     identical guard, so the cap is one solicit attempt per tick.
			if vc.Name == "solicit_work" {
				if solicitedThisTick {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_offered] you already offered your labor this turn — your offer stands; wait for their answer or call done(). Do not offer again."))
					continue
				}
				if solicitAttemptedThisTick {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: offer_not_placed] your offer to work this turn didn't go through, and repeating solicit_work won't change that this turn — say a brief word, tend your post, or call done(); you can try a different employer on a later turn."))
					continue
				}
				solicitAttemptedThisTick = true
			}
			// LLM-164: employer-side labor-resolution same-tick guard, the labor
			// mirror of the resolvedLedgerThisTick guard above (accept_pay family). The
			// first accept_work/decline_work moves the offer out of `pending` (to
			// working or declined), so a second answer to the SAME offer this tick is a
			// guaranteed no-op that only reaches AcceptWork's raw "no longer pending"
			// error — which the weak model does not read as "stop", and which the
			// turn-start "## Work offers awaiting your decision" section keeps
			// re-inviting all turn (perception is not rebuilt between rounds). Keyed on
			// the labor id alone, shared across the pair, recorded on first attempt
			// regardless of outcome.
			if id, ok := laborResolutionID(vc); ok {
				if _, dup := resolvedLaborThisTick[id]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_answered] you already answered that work offer this turn — it is settled (they are at the work now, or you declined it); do not answer again, say a brief word or call done()."))
					continue
				}
				resolvedLaborThisTick[id] = struct{}{}
			}

			// ZBBS-HOME-414: tool-agnostic same-tick identical-call guard for the
			// action tools that lack their own (deliver_order / move_to). The weak
			// model re-fires a byte-identical call until the iteration budget —
			// deliver_order(7) after the hand-over already failed, move_to(here)
			// again — burning rounds and bloating the durable transcript later ticks replay.
			// Unlike the speak/offer guards above (record on SUCCESS, so a bounced
			// line may be retried after the situation changes), this records on the
			// FIRST attempt regardless of outcome: the degenerate case IS the
			// identical retry of a call that FAILED, which a record-on-success guard
			// would never catch. An identical repeat is provably useless for these
			// tools, so rejecting it model-facing costs nothing and steers the model
			// to a different action or done(). consume is deliberately NOT on this
			// list (LLM-91): an identical repeat consume while still in need is
			// productive, so it earns the result-aware guard below instead. gather and
			// craft are NOT here either (LLM-120) — name-only guards above, not name+args.
			if key, ok := genericCallKey(vc); ok {
				if _, dup := triedThisTick[key]; dup {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_did_that] you already tried that exact action this turn — it won't change anything; do something different or call done()."))
					continue
				}
				triedThisTick[key] = struct{}{}
			}

			// LLM-91: semantic same-tick guard for consume. A consume only fails to
			// make sense when it eases no need — the actor is already sated for what
			// that item eases (it still ate and wasted a unit). That is detectable
			// only AFTER the command runs (consumedNothingThisTick is recorded
			// post-dispatch), so the FIRST no-op consume still dispatches and earns
			// the honest "you're full" feedback; only a REPEAT of an item that
			// already eased the actor nothing this tick is rejected here. A productive
			// repeat (still peckish, ate another bite) is never blocked — that is the
			// behavior the byte-identical ZBBS-HOME-414 guard wrongly suppressed.
			if key, isConsume := consumeItemKey(vc); isConsume {
				if _, full := consumedNothingThisTick[key]; full {
					observationOnly = false
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
					transcript = append(transcript, toolResultMsg(call.ID, "[error: already_full] you have eaten your fill of that this turn — it will not ease you further; eat something else or call done()."))
					continue
				}
			}

			// Dispatch by class. The memory partition prefix + date stamp are
			// derived once from the acting actor / snapshot (LLM-356) and threaded
			// to the off-world memory tools via HandlerInput; sim.MemoryPartition is
			// the single derivation point shared with the perception tool-gate.
			memSlugPrefix, memHasPartition := sim.MemoryPartition(actor.Kind, actor.DisplayName)
			memDateStamp := memoryDateStamp(snap.LocalDateUTC)
			content, outcome := h.dispatch(ctx, w, job, vc, actor.LLMAgent, memSlugPrefix, memHasPartition, memDateStamp, perceivedPlaces, rememberedPlaces)
			transcript = append(transcript, toolResultMsg(call.ID, content))

			if outcome.stale {
				// Invariant 7: stale mid-batch ends the batch as stale.
				// Record the stale call itself as failed, and surface the
				// remaining in-budget + truncated calls as both requested
				// AND failed — ToolsFailedRejected MUST stay a subset of
				// ToolsRequested per the TickResult contract.
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
				for j := i + 1; j < len(calls); j++ {
					result.ToolsRequested = append(result.ToolsRequested, calls[j].Name)
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, calls[j].Name)
					transcript = append(transcript, toolResultMsg(calls[j].ID, "[error: stale_skip] earlier call in this batch went stale"))
				}
				for _, c := range truncated {
					result.ToolsRequested = append(result.ToolsRequested, c.Name)
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, c.Name)
					transcript = append(transcript, toolResultMsg(c.ID, "[error: stale_skip] earlier call in this batch went stale"))
				}
				result.TerminalStatus = sim.TickStatusStale
				result.StaleStage = sim.StaleStageAtTool
				return result
			}

			if outcome.success {
				result.ToolsSucceeded = append(result.ToolsSucceeded, call.Name)
				// LLM-91: a consume that absorbed nothing (already sated) is the only
				// senseless consume. Record the item so a REPEAT this tick is rejected
				// by the result-aware guard above; the productive consumes that ran
				// before it stay un-recorded and therefore re-runnable.
				if outcome.consumedNothing {
					if key, isConsume := consumeItemKey(vc); isConsume {
						consumedNothingThisTick[key] = struct{}{}
					}
				}
				// ZBBS-HOME-395: record a placed offer so a later round (or a
				// later call in this same batch) re-offering the same (seller,
				// item, disposition) is rejected by the guard above. Only a
				// SUCCESSFUL offer is recorded — a bounced offer never created a
				// ledger row, so it is not a repeat to guard against and the model
				// may retry the bounced offer.
				if key, isOffer := payOfferKey(vc); isOffer {
					offeredThisTick[key] = struct{}{}
				}
				// LLM-202: record a settled bare pay so a later round (or a later
				// call in this same batch) re-paying the same (recipient, reason) is
				// rejected by the guard above. Success-only, like the offer record —
				// a bounced pay moved no coins and may be retried.
				if key, isPay := payDedupKey(vc); isPay {
					paidThisTick[key] = struct{}{}
				}
				// ZBBS-HOME-433: record a posted quote so a later round (or a
				// later call in this same batch) re-posting the same (item,
				// disposition, target) is rejected by the guard above. Success-
				// only, like the offer record — a bounced quote created nothing
				// and may be retried.
				if key, isQuote := sceneQuoteKey(vc); isQuote {
					quotedThisTick[key] = struct{}{}
				}
				// LLM-120: record a started gather / started batch so a second of
				// either this tick is rejected by the name-only guards above. Success-
				// only, like the speak/offer/quote records — a gather that bounced (wrong
				// spot) or a produce that named an unmakeable good may be retried. gather's
				// nil-error result is always Started (sim.StartHarvest) and produce's is
				// always an opened cycle (sim.StartProductionCycle), so outcome.success
				// alone is the signal — no result inspection needed.
				if vc.Name == "gather" {
					gatheredThisTick = true
				}
				if vc.Name == craftToolName {
					craftedThisTick = true
				}
				// LLM-163: record a SUCCESSFULLY placed labor offer so a second
				// solicit_work this tick is rejected by the guard above with the "offer
				// stands" steer. The FAILED-attempt case is handled separately by
				// solicitAttemptedThisTick (recorded pre-dispatch, LLM-195); this flag
				// stays success-only so the two arms can give distinct feedback.
				if vc.Name == "solicit_work" {
					solicitedThisTick = true
				}
				// LLM-88: a non-terminal commit that moved the actor's own material
				// state (a consume that eased a need and spent stock, a buy that moved
				// coins/goods) makes the tick-open `## You` block and the eat/drink/buy
				// affordances stale — they were rendered once and are re-sent verbatim
				// each round, priming a re-fire loop (live: Josiah ate, then re-consumed
				// against a still-"you feel thirsty / consume to drink" furniture). When
				// the post-commit self-state actually changed, re-perceive from it so the
				// furniture reflects reality. Only the subject's snapshot entry is
				// patched, so every external section (surroundings, warrants, scene)
				// re-renders byte-identical; only the self-state sections move.
				//
				// LLM-173: the one external section also narrowed here is the seller's
				// "## Offers awaiting your decision" cue — WithResolvedPayOffers drops
				// the offers this actor already answered this tick, so the post-accept
				// re-render stops re-inviting a settlement that already happened (the
				// subtractive complement to the LLM-104 resolvedLedgerThisTick reject).
				if outcome.postSelfState != nil && selfStateChanged(lastSelf, outcome.postSelfState) {
					refreshed := perception.Render(
						perception.Build(snap.WithActor(job.actorID, outcome.postSelfState), job.actorID, job.warrants,
							perception.WithResolvedPayOffers(resolvedPayOfferIDs(resolvedLedgerThisTick))),
						h.renderConfig,
					)
					ephemeralText = refreshed.EphemeralText
					lastSelf = outcome.postSelfState
				}
			} else {
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, call.Name)
			}

			if outcome.ended {
				// Invariant 3: post-terminal calls skipped + logged.
				//
				// LLM-414: a skipped COMMIT call is the actor's own declared,
				// unfinished intent — the live incident's speak (terminal) +
				// summon batch dropped the summon here with nothing to bring
				// it back for 12.5 minutes. Collect the commit-class names
				// into result.SkippedIntentTools so CompleteReactorTick
				// stamps a prompt unfinished_intent re-tick. Exclusions:
				//   - speak — re-ticking to re-say is the LLM-184 verb storm;
				//   - non-commit classes (done, observations) — nothing to do;
				//   - unresolvable names — an unknown tool declares nothing;
				//   - a tick ITSELF triggered by unfinished_intent (one retry,
				//     never a storm).
				for j := i + 1; j < len(calls); j++ {
					result.ToolsRequested = append(result.ToolsRequested, calls[j].Name)
					result.ToolsFailedRejected = append(result.ToolsFailedRejected, calls[j].Name)
					transcript = append(transcript, toolResultMsg(calls[j].ID, "[skipped: post_terminal] earlier call in this batch ended the tick"))
					if calls[j].Name == "speak" || retriggeredIntent {
						continue
					}
					if entry, ok := h.validator.Registry.Lookup(calls[j].Name); ok && entry.Class == ClassCommit {
						result.SkippedIntentTools = append(result.SkippedIntentTools, calls[j].Name)
					}
				}
				batchEnded = true
				endedAt = i
				endedStatus = outcome.terminalStatus
				break
			}
		}

		// Surface truncated calls as typed validation failures (invariant 2).
		// Only if we didn't already account for them via the stale path.
		if !batchEnded || endedStatus != sim.TickStatusStale {
			for _, c := range truncated {
				result.ToolsRequested = append(result.ToolsRequested, c.Name)
				result.ToolsFailedRejected = append(result.ToolsFailedRejected, c.Name)
				transcript = append(transcript, toolResultMsg(c.ID, "[error: excess_calls_truncated] dropped per MaxToolCallsPerResponse"))
			}
		}

		_ = endedAt
		if batchEnded {
			result.TerminalStatus = endedStatus
			return result
		}

		// Round complete, tick continues. An observation-only round (recall
		// to gather context) is "thinking" — it does NOT consume the action
		// budget (ZBBS-WORK-321); it's bounded only by maxTotalRounds. Any
		// other round (a commit, a validation failure, dropped calls) is an
		// action round and counts.
		if !observationOnly {
			actionRounds++
			if actionRounds >= h.iterationBudget {
				result.TerminalStatus = sim.TickStatusBudgetForced
				result.BudgetHit = true
				return result
			}
		}
	}

	// Per-tick hard ceiling hit (IterationBudget + MaxObservationRounds) —
	// e.g. a model that only ever recalls and never commits.
	result.TerminalStatus = sim.TickStatusBudgetForced
	result.BudgetHit = true
	return result
}

// dispatchOutcome bundles a single tool dispatch result.
type dispatchOutcome struct {
	success        bool
	ended          bool                   // tick ends after this call
	stale          bool                   // commit dispatch returned ErrTickAttemptStale
	terminalStatus sim.TickTerminalStatus // populated when ended
	// postSelfState is the acting actor's snapshot taken right after a commit ran
	// (sim.TickToolResult.PostActorSnapshot); nil for observations, terminal
	// classes, failures, and commits that returned no result. The loop uses it to
	// re-perceive own-state mid-tick when a commit changed needs/coins/goods
	// (LLM-88).
	postSelfState *sim.ActorSnapshot
	// consumedNothing is true when this was a consume that eased no need
	// (sim.ConsumeResult.EasedNeed == false — the actor is already sated for what
	// the item eases; it still ate and wasted a unit). The loop uses it to arm the
	// LLM-91 semantic repeat-consume guard: the first such consume runs (and earns
	// its "you're full" feedback); a repeat of it this tick is then rejected.
	consumedNothing bool
}

// dispatch executes one validated call. Returns the content string for
// the resulting "tool" message and an outcome describing what happened.
//
// Routing:
//   - ClassObservation → run the ObservationFn inline; observations never
//     end the tick (TerminalPolicy == TerminalNever guaranteed by the
//     constructor).
//   - ClassCommit → build the sim.Command via CommitFn, wrap with
//     sim.RunTickToolCommand for the attempt guard, submit via
//     World.SendContext. A successful submission with
//     TerminalPolicy=TerminalOnSuccess ends the tick.
//   - ClassTerminal → no handler; the tick ends.
//
// memoryDateStamp renders the village's current calendar date as "YYYY-MM-DD"
// for a memory slug (LLM-356). LocalDateUTC is already midnight UTC of the
// village date, so formatting in UTC yields that date without a timezone shift.
// Returns "" for a zero time (a hand-built snapshot with no clock) so memorize
// can fall back rather than stamp year 1.
func memoryDateStamp(localDateUTC time.Time) string {
	if localDateUTC.IsZero() {
		return ""
	}
	return localDateUTC.UTC().Format("2006-01-02")
}

func (h *Harness) dispatch(ctx context.Context, w *sim.World, job tickJob, vc *ValidatedCall, llmMemoryAgent, memSlugPrefix string, memHasPartition bool, memDateStamp string, perceivedPlaces perception.PerceivedPlaces, rememberedPlaces sim.RememberedPlaces) (string, dispatchOutcome) {
	in := HandlerInput{
		ActorID:            job.actorID,
		AttemptID:          job.attemptID,
		RootEventID:        job.rootEventID,
		LLMMemoryAgent:     llmMemoryAgent,
		MemorySlugPrefix:   memSlugPrefix,
		MemoryHasPartition: memHasPartition,
		MemoryDateStamp:    memDateStamp,
		Args:               vc.DecodedArgs,
		// Object move targets this tick's perception surfaced (ZBBS-HOME-389) — the
		// move_to commit resolves a name that matches no village structure against
		// these (a well, a fruit tree). Structures resolve by village geography
		// directly (LLM-142). Empty for non-move ticks.
		PerceivedObjectIDs: perceivedPlaces.ObjectIDs,
		// The actor's durable known places (LLM-78) — move_to's memory-backed
		// OBJECT name-resolution fallback, tried after the perceived sources miss.
		RememberedPlaces: rememberedPlaces,
		// New-news signal for the turn-state gate (ZBBS-WORK-370). Computed from
		// the tick's consumed warrant batch; only the speak commit consumes it.
		HasNewNews: batchHasNewNews(job.warrants),
	}

	switch vc.Entry.Class {
	case ClassObservation:
		fn := vc.Entry.Observation()
		if fn == nil {
			log.Printf("handlers: dispatch %q: observation handler is nil (registration bug)", vc.Name)
			return "[error: handler_missing] observation handler is nil", dispatchOutcome{}
		}
		content, err := fn(ctx, in)
		if err != nil {
			// A modelSafeError is a hand-authored, model-safe rejection (a
			// post-decode static check the handler ran — empty-after-trim, a
			// control char): surface its reason so the model self-corrects,
			// matching the command layer's sim.ModelFacingError echo below
			// (ZBBS-WORK-413). Every other handler error stays generic —
			// handler-internal detail (file paths, stack traces, API
			// responses) must not leak into the LLM transcript.
			var safe modelSafeError
			if errors.As(err, &safe) {
				log.Printf("handlers: dispatch %q: observation handler rejected: %v", vc.Name, err)
				return fmt.Sprintf("[error] %s", safe.Error()), dispatchOutcome{}
			}
			log.Printf("handlers: dispatch %q: observation handler failed: %v", vc.Name, err)
			return "[error: handler_failed] tool handler returned an error", dispatchOutcome{}
		}
		return content, dispatchOutcome{success: true}

	case ClassCommit:
		fn := vc.Entry.Commit()
		if fn == nil {
			log.Printf("handlers: dispatch %q: commit handler is nil (registration bug)", vc.Name)
			return "[error: handler_missing] commit handler is nil", dispatchOutcome{}
		}
		cmd, err := fn(in)
		if err != nil {
			// Model-safe post-decode static-validation rejection → surface the
			// reason; any other build error stays generic (see the observation
			// branch above and ZBBS-WORK-413).
			var safe modelSafeError
			if errors.As(err, &safe) {
				log.Printf("handlers: dispatch %q: commit handler rejected: %v", vc.Name, err)
				return fmt.Sprintf("[error] %s", safe.Error()), dispatchOutcome{}
			}
			log.Printf("handlers: dispatch %q: commit handler failed: %v", vc.Name, err)
			return "[error: handler_failed] tool handler returned an error", dispatchOutcome{}
		}

		dispatchCtx := ctx
		if h.toolDispatchTimeout > 0 {
			var cancel context.CancelFunc
			dispatchCtx, cancel = context.WithTimeout(ctx, h.toolDispatchTimeout)
			defer cancel()
		}

		cmdResult, err := w.SendContext(dispatchCtx, sim.RunTickToolCommand(job.actorID, job.attemptID, job.rootEventID, cmd))
		if err != nil {
			if errors.Is(err, sim.ErrTickAttemptStale) {
				return "[error: stale] tick attempt superseded", dispatchOutcome{stale: true}
			}
			// LLM-209: a NO-OP rest verb (walk to where the actor already is / is
			// already walking to; take a break while already on break) is TERMINAL.
			// Echo its model-facing reason and END the tick, so a weak model can't
			// re-fire the identical no-op every round to the iteration budget (the
			// move_to×6 / take_break×6 budget_forced storm). Checked BEFORE the
			// generic ModelFacingError echo below, which is non-terminal (a genuinely
			// correctable error — a bad structure_id, an unreachable target — still
			// gets a retry).
			var noop sim.TerminalNoOpError
			if errors.As(err, &noop) {
				return fmt.Sprintf("[ok] %s", noop.Error()), dispatchOutcome{success: true, ended: true, terminalStatus: sim.TickStatusSuccess}
			}
			// LLM-317: the NON-terminal sibling — a no-op that echoes its [ok] message
			// but does NOT end the tick (the confabulated "kitchen" phantom arrival).
			// The actor never moved and may be about to produce/act, so it keeps this
			// tick. Same success shape as a non-terminal observation/produce round
			// (success:true, ended:false, nil postSelfState — nothing changed, so the
			// loop's postSelfState re-perception is correctly skipped).
			var ntNoop sim.NonTerminalNoOpError
			if errors.As(err, &ntNoop) {
				return fmt.Sprintf("[ok] %s", ntNoop.Error()), dispatchOutcome{success: true, ended: false}
			}
			// Echo the command validator's rejection reason to the model so it can
			// correct its next call ("no one named X in this conversation", "use a
			// structure_id you can see in your perception"). These reasons are
			// authored to be model-facing and reach us tagged as ModelFacingError
			// (see RunTickToolCommand). Returning a generic "command_failed" here
			// previously severed self-correction — the model never learned WHY a
			// call failed and retried the same bad call indefinitely (e.g. paying a
			// structure name as if it were a person, or move_to'ing a name instead
			// of a structure_id). Only ModelFacingError is echoed; every other
			// dispatch error (actor-not-found race, nil command, invalid root,
			// context deadline) stays generic so internal detail never leaks.
			var modelErr sim.ModelFacingError
			if errors.As(err, &modelErr) {
				log.Printf("handlers: dispatch %q: command rejected: %v", vc.Name, err)
				return fmt.Sprintf("[error] %s", modelErr.Error()), dispatchOutcome{}
			}
			log.Printf("handlers: dispatch %q: command send failed: %v", vc.Name, err)
			return "[error: command_failed] world command rejected the tool", dispatchOutcome{}
		}

		// LLM-88: RunTickToolCommand wraps the tool's result with the actor's
		// post-commit self-state. Unwrap so commitResultContent still sees the
		// inner domain result, and carry the snapshot up for the loop's own-state
		// re-perception. Reachable only on the err==nil path, where the wrapper
		// always returns a TickToolResult, so the assertion holds. Unwrapped before
		// the terminal decision below so the LLM-201 no-switch flip can read it.
		wrapped, _ := cmdResult.(sim.TickToolResult)

		ended := vc.Entry.TerminalPolicy == TerminalOnSuccess
		// (The LLM-201 no-switch produce terminal-flip is retired with the
		// continuous focus: under one-shot production (LLM-319) every successful
		// produce STARTS a batch — real work, never a "tend your post" no-op —
		// and a mid-cycle re-produce bounces as a ModelFacingError before
		// reaching here. Non-terminal stands so the actor can speak its social
		// beat in the same tick.)
		//
		// LLM-468: except when it already spoke that beat. `produce` is registered
		// non-terminal so a producer can still act again, but a produce that
		// carried a `say` HAS uttered — and an utterance ends a tick, the same
		// invariant that makes `speak` terminal-on-success. Without this the model
		// could say its piece on the acting call and a second thing through
		// `speak`, which is exactly the double-utterance the terminal-verb rule
		// exists to prevent.
		if producedWithSpeech(wrapped.Result) {
			ended = true
		}
		out := dispatchOutcome{success: true, ended: ended}
		if ended {
			out.terminalStatus = sim.TickStatusSuccess
		}
		out.postSelfState = wrapped.PostActorSnapshot
		// LLM-91: flag a consume that eased no need so the loop can arm the
		// semantic repeat-consume guard (consumeNoop: a ConsumeResult with
		// EasedNeed == false — the actor is already sated; it still wasted a unit).
		out.consumedNothing = consumeNoop(wrapped.Result)
		return commitResultContent(vc, wrapped.Result), out

	case ClassTerminal:
		return "[done]", dispatchOutcome{
			success:        true,
			ended:          true,
			terminalStatus: sim.TickStatusDone,
		}

	default:
		log.Printf("handlers: dispatch %q: unknown class %v (typed-constructor invariant violated)", vc.Name, vc.Entry.Class)
		return "[error: unknown_class] tool dispatch encountered an unknown class", dispatchOutcome{}
	}
}

// selfStateChanged reports whether a commit moved the actor's own MATERIAL
// state in a way the ## You block and the eat/drink/buy affordances reflect:
// needs (a consume / eat-in-place eases them), coins, or carried goods (a buy
// or sale moves them), plus the crafter's production focus (a craft commit sets
// it). These axes drive the LLM-88 mid-tick refresh — the affordance-staleness
// re-fire loop. The focus axis (LLM-128) is what flips the forge cue's lead from
// "choose what to forge" to "you are crafting X now" mid-tick, the moment the
// first craft sets the focus, so the within-tick re-prompt stops re-inviting a
// pick the model already made. Position / huddle / rest-state changes are
// deliberately out of scope (a move_to is terminal and never loops; take_break's
// resting line is not an action primer).
//
// InventoryHash is the snapshot's own per-actor quantity sum, so it catches any
// net stock change a single commit makes (consume/buy/sale never swap two kinds
// to an equal total). Needs are compared key-by-key because a missing vs zero
// key is itself a real change.
func selfStateChanged(pre, post *sim.ActorSnapshot) bool {
	if pre == nil || post == nil {
		return post != nil
	}
	if pre.Coins != post.Coins || pre.InventoryHash != post.InventoryHash {
		return true
	}
	if pre.ProductionItem != post.ProductionItem {
		return true // LLM-319: a batch started or landed — re-render so the trade cue / in-progress line flips
	}
	if len(pre.Needs) != len(post.Needs) {
		return true
	}
	for k, v := range post.Needs {
		if pv, ok := pre.Needs[k]; !ok || pv != v {
			return true
		}
	}
	return false
}

// --- helpers --------------------------------------------------------------

// paySettlementFellThroughContent renders the seller-facing echo for a pay
// offer that settled to a NON-accepted terminal (no stock, buyer short of
// coins/goods, either party moved on, offer lapsed). It is shared by accept_pay
// and by counter_pay's non-increasing-coercion — a seller "countering" at or
// below the offered price is a yes, so the coerced accept settles under the
// counter_pay name and can still fail a gate. Without this echo that path fell
// through to a bare "[ok]" that reads as a completed sale — the same misread
// that let a seller "confirm" goods it never held (LLM-302).
//
// Returns ("", false) for any state that is not one of these fell-through
// terminals, so each caller can fall through to its own verb-specific handling
// (Accepted / Countered / Declined steers). These echoes state the outcome with
// NO retry verb: the offer is already terminal, so there is nothing left to
// decline_pay / counter_pay — unlike the still-pending accept_pay stock
// rejection, which is a retryable ModelFacingError raised upstream (LLM-302) and
// never reaches this success-path switch.
// sayEcho renders the acknowledgment fragment for a tool that folded a spoken
// line into its own terminal act — sell, offer_work, and the five pay/labor
// responses (LLM-343 / LLM-346 / LLM-350). The caller appends it to its own
// outcome sentence, so it ends with a trailing space; "" when nothing was said.
//
// Echoing the line back is the same acknowledgment a bare speak returns, and it
// is the ONLY signal the model gets that the room heard it. announced is false
// when the act committed but SpeakTo refused the words — the act stands either
// way, so the echo reports the refusal and passes SpeakTo's own reason through
// rather than guessing which gate rejected it.
func sayEcho(say string, announced bool, sayRefused string) string {
	s := strings.TrimSpace(say)
	if s == "" {
		return ""
	}
	switch {
	case announced:
		return fmt.Sprintf("You said: %q. ", s)
	case sayRefused != "":
		return fmt.Sprintf("Your words went unsaid: %s ", sayRefused)
	default:
		return "Your words went unsaid. "
	}
}

// payResponseSayEcho is sayEcho over whichever of the three pay-response arg
// types vc carries. Returns "" when the response was wordless.
func payResponseSayEcho(vc *ValidatedCall, announced bool, sayRefused string) string {
	switch args := vc.DecodedArgs.(type) {
	case AcceptPayArgs:
		return sayEcho(args.Say, announced, sayRefused)
	case DeclinePayArgs:
		return sayEcho(args.Say, announced, sayRefused)
	case CounterPayArgs:
		return sayEcho(args.Say, announced, sayRefused)
	default:
		return ""
	}
}

// laborNoHireContent renders accept_work's no-hire outcomes: the reason nobody
// was taken on, then the fate of the words the acceptor tried to say in the same
// breath. A gate-driven flip resolves before the utterance goes out, so the line
// is never spoken — say so, rather than let the acceptor believe the room heard
// her thank a worker she never hired (LLM-351). said carries sayEcho's own
// trailing space and is "" for a wordless accept; trim the tail either way.
func laborNoHireContent(reason, said string) string {
	return strings.TrimRight("[ok] "+reason+" "+said, " ")
}

func paySettlementFellThroughContent(state sim.PayLedgerState) (string, bool) {
	switch state {
	case sim.PayLedgerStateFailedInsufficientStock:
		return "[ok] You agreed, but you don't have enough stock to fill it — the sale fell through.", true
	case sim.PayLedgerStateFailedInsufficientFunds:
		return "[ok] You agreed, but the buyer couldn't cover the price — the sale fell through.", true
	case sim.PayLedgerStateFailedInsufficientGoods:
		return "[ok] You agreed, but the buyer no longer had the goods they offered — the sale fell through.", true
	case sim.PayLedgerStateFailedUnavailable:
		return "[ok] That sale couldn't be completed — you or the buyer had moved on.", true
	case sim.PayLedgerStateExpired:
		return "[ok] That offer had already expired — too late to accept it.", true
	}
	return "", false
}

// commitResultContent builds the "tool" message content a successful commit
// returns to the model. Most commits return the generic "[ok]"; speak and a
// newly-placed pay_with_item offer are the exceptions. speak echoes the line it
// just said back; a placed offer (ZBBS-HOME-395) echoes the pending offer plus
// an await-the-seller / done() steer, replacing a bare "[ok]" that read as
// "nothing happened, try again" and drove a same-tick pay_with_item×6 storm.
//
// The speak echo is now just a commit acknowledgment. speak is terminal-on-
// success (LLM-321): the tick ends on it, so the model never reads this result
// in a later within-tick round. The original ZBBS-WORK-368 rationale for echoing
// (a weak model emits an empty assistant content string on a tool call, so
// without the echo it can't saliently see it just spoke and re-speaks within the
// tick) and the ZBBS-WORK-375 "call done() now" continuation steer are both
// retired along with the second round they guarded — the utterance still reaches
// memory via the cross-tick replay path (memapi's `(I said aloud: "...")`).
//
// The text is re-trimmed to match what was actually spoken (sim.Speak trims);
// the success branch is only reached after the speak command committed, so the
// utterance is non-empty and control-char-clean by then. %q quotes + escapes
// it, so an utterance containing a quote can't break the echo's framing.
// cmdResult is the value the committed world command returned through
// RunTickToolCommand (nil for commands that return nothing). Most content
// below is composed from the call's decoded args alone; the consume branch
// needs the result because the ZBBS-WORK-391 needs-clamp decides the
// eaten/kept split on the world goroutine, after the args are fixed.
func commitResultContent(vc *ValidatedCall, cmdResult any) string {
	// A consume must tell the model what actually happened. A clamped consume
	// (Kept > 0) reports the eaten/kept split so a "consume 10" answered by a
	// bare [ok] isn't read as ten eaten. A need-moving consume with no surplus
	// (Kept == 0) voices the honest post-consume felt state (LLM-7) — without it
	// the stale within-tick eat-affordance furniture primed the weak model to
	// re-fire consume until the dedup guard or a stochastic break (live: Josiah
	// ate one loaf, bounced four more consumes, then greeted an empty room).
	if vc.Name == "consume" {
		if r, ok := cmdResult.(sim.ConsumeResult); ok {
			// LLM-113: render the count-aware catalog noun ("3 raspberries", "a
			// bowl of stew"), falling back to the raw kind key if the catalog
			// carried no phrase (a discovery-minted kind).
			noun := r.ConsumedNoun
			if noun == "" {
				noun = string(r.Kind)
			}
			if r.Kept > 0 {
				return fmt.Sprintf(
					"[ok] You consume %d %s — that satisfies you; the remaining %d stay in your pack. Do not consume more now.",
					r.Consumed, noun, r.Kept,
				)
			}
			// Honest post-consume state for the no-surplus case, mirroring the
			// pay_with_item eat feedback (buyerFeltAfterConsume): sated → an
			// explicit stop steer; still in need → the plain felt label with no
			// stop (the actor may legitimately act again; identical repeats are
			// still caught by the ZBBS-HOME-414 dedup guard).
			if r.Consumed > 0 && r.SatisfiesNeed != "" {
				var b strings.Builder
				fmt.Fprintf(&b, "[ok] You consume %d %s.", r.Consumed, noun)
				if r.FeltAfter != "" {
					fmt.Fprintf(&b, " You still feel %s.", r.FeltAfter)
				} else {
					// The need clause names the eased need; the verb follows the
					// item's CATEGORY (r.Verb), not the need, so a belly-filling
					// ale reads "Your hunger is met — drink no more now" (LLM-318).
					// Verb is empty only on legacy/zero-consume results this branch
					// never voices; fall back to the need-keyed verb there.
					verb := r.Verb
					switch r.SatisfiesNeed {
					case "hunger":
						if verb == "" {
							verb = "eat"
						}
						fmt.Fprintf(&b, " Your hunger is met — %s no more now.", verb)
					case "thirst":
						if verb == "" {
							verb = "drink"
						}
						fmt.Fprintf(&b, " Your thirst is met — %s no more now.", verb)
					default:
						b.WriteString(" That need is met — consume no more now.")
					}
				}
				return b.String()
			}
		}
	}
	// accept_pay: a gate-fail resolution (buyer short of coins/goods, either
	// party moved on, offer already lapsed) is NOT a tool error — sim.AcceptPay
	// returns the terminal PayLedgerState with a nil error, so a bare "[ok]"
	// told the seller "accepted" when the sale actually fell through
	// (ZBBS-WORK-432, the 271 dry-seller case: Josiah "accepted" water he no
	// longer had, got [ok], learned nothing). Report the real outcome in plain
	// modern English via paySettlementFellThroughContent. (The stock shortfall
	// is the one exception: LLM-302 makes it a retryable ModelFacingError raised
	// upstream, so it never reaches this success-path switch — the helper's
	// stock case is defensive here and live for counter_pay's coercion below.)
	//
	// A genuine Accepted (ZBBS-HOME-473) no longer falls through to a bare [ok]
	// either: it once closed the sale mute, the buyer walking off with no word
	// exchanged (observed live: Josiah×Prudence bread).
	//
	// These three arms used to end "Say a brief word …, then call done()". Every
	// pay response is terminal-on-success, so the tick returns the moment one
	// lands (see the batchEnded return in runTick) — the model never got another
	// round in which to say that word, and the instruction was unreachable text.
	// The words now ride on each tool's own `say` (LLM-350) and are echoed back
	// here, which is the only signal the model gets that the room heard it.
	if vc.Name == "accept_pay" {
		if state, announced, refused, ok := payResponseState(cmdResult); ok {
			if state == sim.PayLedgerStateAccepted {
				return "[ok] The sale is settled. " + payResponseSayEcho(vc, announced, refused) + "Do not accept again."
			}
			if msg, ok := paySettlementFellThroughContent(state); ok {
				return msg
			}
		}
	}
	if vc.Name == "decline_pay" {
		if state, announced, refused, ok := payResponseState(cmdResult); ok {
			switch state {
			case sim.PayLedgerStateDeclined:
				return "[ok] You declined. " + payResponseSayEcho(vc, announced, refused) + "Do not decline again."
			}
		}
	}
	if vc.Name == "counter_pay" {
		if state, announced, refused, ok := payResponseState(cmdResult); ok {
			switch state {
			case sim.PayLedgerStateCountered:
				return "[ok] Your counter stands. " + payResponseSayEcho(vc, announced, refused) + "Await their answer. Do not counter again."
			// A non-increasing pure-coin counter coerces to an accept in
			// sim.CounterPay (the "I'll let it go at your price" path), so the
			// sale settles under the counter_pay name and would otherwise miss
			// the handover steer accept_pay earns — the gap the HOME-473 ticket
			// glossed (LLM-13). Voice the settle.
			case sim.PayLedgerStateAccepted:
				return "[ok] The sale is settled. " + payResponseSayEcho(vc, announced, refused) + "Do not counter again."
			default:
				// That same coercion can fail a gate at settle time (seller has
				// no stock, buyer is short of coins, either party moved on, offer
				// lapsed) — it flips to a fell-through terminal that otherwise
				// dropped to the bare "[ok]" below, reading as a completed sale.
				// Echo the real outcome, the same fix accept_pay carries (LLM-302).
				// (Goods-shortfall can't arise: coercion is pure-coin only.)
				if msg, ok := paySettlementFellThroughContent(state); ok {
					return msg
				}
			}
		}
	}
	// gather: harvesting takes time (LLM-54) and is now tick-terminal (LLM-175) — a
	// started pick ends the tick, so the old "do not gather again / call done() now"
	// steer is moot (the engine ends the turn for it). Keep the result purely
	// informational: the pick is timed and the yield lands next turn, so a bare
	// "[ok]" would read as "done, nothing happened". The "you took it all / it's
	// bare" beat lands in the next-tick completion perception
	// (SourceActivityCompletionNarration), where the exact yield + depletion are
	// known.
	if vc.Name == "gather" {
		if r, ok := cmdResult.(sim.SourceActivityStartResult); ok && r.Started {
			at := ""
			if r.SourceName != "" {
				at = " at " + r.SourceName
			}
			return fmt.Sprintf("[ok] You start gathering%s. It finishes on its own in a few seconds; the harvest lands in your pack next turn.", at)
		}
	}
	// produce: StartProductionCycle opened a batch; the yield lands when the
	// cycle's work is done, so a bare "[ok]" would read as "nothing happened,
	// try again" (the LLM-120 re-craft loop). Confirm what was begun — size,
	// work, ingredients spent — mirroring the gather start confirmation. The
	// mechanical numbers belong HERE, in the post-decision result, not in the
	// deliberation scene (LLM-319).
	if vc.Name == craftToolName {
		if r, ok := cmdResult.(sim.ProductionStartResult); ok {
			noun := r.Noun
			if noun == "" {
				noun = string(r.Item)
			}
			msg := fmt.Sprintf("[ok] You start a batch of %s — %d when it's done, about %s of work at your post.",
				noun, r.BatchQty, sim.HumanizeWorkDuration(r.DurationSeconds))
			if r.InputsUsed != "" {
				msg += fmt.Sprintf(" You use %s.", r.InputsUsed)
			}
			// Durable-tool wear (LLM-330): pre-phrased world-side, where the
			// wear counter and catalog nouns live.
			if r.ToolWear != "" {
				msg += " " + r.ToolWear
			}
			return msg + " It finishes on its own while you work here — no need to produce again until it lands."
		}
	}
	if vc.Name == "speak" {
		if args, ok := vc.DecodedArgs.(SpeakArgs); ok {
			text := strings.TrimSpace(args.Text)
			if text != "" {
				return fmt.Sprintf("[ok] You said: %q.", text)
			}
		}
	}
	// scene_quote: a clamped disposition must be reported for the same
	// reason as the pay clamp below (ZBBS-WORK-405) — the seller model
	// would otherwise believe it posted a take-home quote it didn't. Both
	// variants carry the post-quote steer (ZBBS-HOME-433): pre-433 a posted
	// quote returned a bare [ok], so the model had no within-tick reason to
	// stop and re-posted the identical quote to the iteration budget. The
	// steer is the soft half; quotedThisTick's already_quoted reject is the
	// teeth.
	if vc.Name == "sell" {
		const quoteSteer = "The room has heard your offer — await an answer or call done(). Do not post the same offer again."
		// "Your offer now stands" only when the result proves a quote was
		// actually created (code_review #415) — an unexpected result shape
		// still steers, but doesn't assert state without evidence.
		if r, ok := cmdResult.(sim.SceneQuoteCreateResult); ok {
			said := ""
			if args, ok := vc.DecodedArgs.(SceneQuoteArgs); ok {
				said = sayEcho(args.Say, r.Announced, r.SayRefused)
			}
			if r.EatHereClamped {
				item := "those goods"
				if args, ok := vc.DecodedArgs.(SceneQuoteArgs); ok && len(args.Lines) == 1 {
					if it := strings.ToLower(strings.Join(strings.Fields(args.Lines[0].ItemKind), " ")); it != "" {
						item = it
					}
				}
				return fmt.Sprintf(
					"[ok] %sMind: %s can't be carried away — your offer stands as eat-here, taken on the spot. %s",
					said, item, quoteSteer,
				)
			}
			return "[ok] " + said + "Your offer now stands. " + quoteSteer
		}
		return "[ok] " + quoteSteer
	}
	// offer_trade lowers onto a PayWithItemArgs (ZBBS-HOME-407), so it carries
	// the same decoded shape and earns the same post-offer steer.
	if vc.Name == "pay_with_item" || vc.Name == "offer_trade" {
		// LLM-290: a coin-token "buy" was translated to a plain payment
		// (HandlePayWithItem builds sim.Pay), so by the time we narrate, the
		// coins have already moved — the pending-offer echo below would tell
		// the model to bide for an answer on an offer that doesn't exist.
		// Voice the settle and steer to done() instead. pay_with_item only:
		// offer_trade's coin want_item is steered at decode and never commits.
		if args, ok := vc.DecodedArgs.(PayWithItemArgs); ok && vc.Name == "pay_with_item" && sim.IsCoinToken(args.Item) {
			coins := args.Amount
			if coins <= 0 {
				coins = args.Qty
			}
			other := strings.TrimSpace(args.Seller)
			if other == "" {
				other = "them"
			}
			said := ""
			if r, ok := cmdResult.(payCoinTranslationResult); ok {
				said = sayEcho(args.Say, r.Announced, r.SayRefused)
			}
			return fmt.Sprintf(
				"[ok] Coins are payment, not goods to buy — this settled as a plain payment: you handed %s %d coins; the coins have moved and nothing is owed back. %sNext time, use pay to give someone coins.",
				other, coins, said,
			)
		}
		// ZBBS-WORK-405: when the engine clamped a take-home request to
		// eat-here (non-portable consumable), the feedback must say so —
		// same reasoning as the consume clamp above: a silently adjusted
		// action leaves the model believing it carried off goods it never
		// held. Rides the pending-offer steer below AND the generic [ok]
		// flows (quote take, counter-response).
		clampNote := ""
		if r, ok := cmdResult.(sim.PayWithItemResult); ok && r.EatHereClamped {
			clampItem := "those goods"
			if args, ok := vc.DecodedArgs.(PayWithItemArgs); ok {
				if it := strings.ToLower(strings.Join(strings.Fields(args.Item), " ")); it != "" {
					clampItem = it
				}
			}
			clampNote = fmt.Sprintf(
				" Mind: %s can't be carried away — this settles eat-here, taken on the spot.",
				clampItem,
			)
		}
		// A quote-take settles INSTANTLY (ZBBS-HOME-424): payment, goods,
		// and any consume_now meal all land inside this one tool call. The
		// old generic "[ok]" told the model nothing happened — and the
		// within-tick continuation body re-renders from the tick-start
		// snapshot, so the buyer's felt needs never legibly move either.
		// The model re-bought the same item to the iteration budget (six
		// meats in six seconds, live 2026-06-12). Voice what the settle
		// actually did — money, goods, the meal, and the post-meal felt
		// state computed from LIVE world state at commit — and steer to
		// done(). ZBBS-HOME-436; supersedes the pre-424 "quote-take
		// doesn't storm" assumption recorded below. Gated on Accepted so a
		// future fast-path result in any other state can't voice a settle
		// that didn't happen (code_review).
		if r, ok := cmdResult.(sim.PayWithItemResult); ok && r.FastPath && r.State == sim.PayLedgerStateAccepted {
			if args, ok := vc.DecodedArgs.(PayWithItemArgs); ok {
				return settledPayContent(args, r, clampNote)
			}
			if clampNote != "" {
				return "[ok]" + clampNote
			}
			return "[ok]"
		}
		if args, ok := vc.DecodedArgs.(PayWithItemArgs); ok {
			// A plain new offer (no quote_id / in_response_to) is now a pending
			// ledger entry the seller must accept, decline, or counter — the
			// buyer's move this tick is finished. Pre-395 this returned a bare
			// "[ok]", which read as "nothing happened, try again" and drove the
			// re-offer storm. Echo what was offered for salience (Llama-3.3 emits
			// empty assistant content on a tool call — the same weak-salience gap
			// the speak echo above closes) and steer to done(), forbidding the
			// re-offer. Counter-responses are a distinct flow that doesn't
			// storm, so they keep the generic "[ok]".
			if args.QuoteID == 0 && args.InResponseTo == 0 {
				item := strings.ToLower(strings.Join(strings.Fields(args.Item), " "))
				if item == "" {
					item = "those goods"
				}
				other := strings.TrimSpace(args.Seller)
				// A workplace-name reroute (ZBBS-HOME-460) resolved the offer to
				// the worker, not the building the model named — echo the real
				// recipient so "bide for their answer" points at a person who
				// can actually answer.
				if r, ok := cmdResult.(sim.PayWithItemResult); ok && r.ReroutedSellerName != "" {
					other = r.ReroutedSellerName
				}
				if other == "" {
					other = "them"
				}
				// offer_trade is a proposer-framed barter ("trade for X");
				// a plain pay_with_item buys ("buy X"). Same pending-offer
				// mechanics, different lead verb for legibility. Copy is light
				// period voice (ZBBS-HOME-421): NPCs mirror the register of what
				// they read, and the old contract language came back out of their
				// mouths verbatim. Keep the functional tokens (qty, item, seller,
				// accept/decline/counter) intact in any rewording.
				lead := fmt.Sprintf("Your offer to buy %d %s", args.Qty, item)
				if vc.Name == "offer_trade" {
					lead = fmt.Sprintf("Your offer to trade for %d %s", args.Qty, item)
				}
				// Echo the buyer's own words back (LLM-350). Both tools carry a
				// say; it stays optional on each, so said is "" for a wordless
				// offer and the sentence is unchanged.
				//
				// "call done()" is gone from this line for the same reason it left the
				// response results: pay_with_item and offer_trade are both
				// terminal-on-success, so the tick has already ended by the time the
				// model reads this (code_review).
				said := ""
				if r, ok := cmdResult.(sim.PayWithItemResult); ok {
					said = sayEcho(args.Say, r.Announced, r.SayRefused)
				}
				return fmt.Sprintf(
					"[ok] %s is before %s — bide for their answer. %sMake no second "+
						"offer; let them accept, decline, or counter.%s",
					lead, other, said, clampNote,
				)
			}
		}
		if clampNote != "" {
			return "[ok]" + clampNote
		}
	}
	// solicit_work (LLM-163 → LLM-180): a placed labor offer is now tick-terminal
	// (RegisterSolicitWork), so the old "say a brief word, then call done(), do not
	// offer again" steer is moot — the engine ends the turn for it. Keep the result
	// purely informational: echo who the offer went to and that they answer on
	// their turn. (The solicitedThisTick guard stays as belt-and-suspenders for any
	// caller/path that does not honor terminal policy; an errored solicit does not set
	// it, so it is retryable on a LATER tick — but a second errored solicit WITHIN this
	// tick is blocked by solicitAttemptedThisTick, LLM-195.)
	if vc.Name == "solicit_work" {
		if r, ok := cmdResult.(sim.LaborSolicitResult); ok {
			switch r.State {
			case sim.LaborStatePending:
				// When a `say` rode along it has already gone out; when SpeakTo
				// refused it, the offer still stands and the refusal reason is
				// surfaced rather than guessed at (mirrors offer_work below). LLM-564.
				if r.SayRefused != "" {
					return fmt.Sprintf("[ok] Your offer of labor to %s is on the table — they will answer on their turn. Your words did not carry: %s", r.EmployerName, r.SayRefused)
				}
				return fmt.Sprintf("[ok] Your offer of labor to %s is on the table — they will answer on their turn.", r.EmployerName)
			case sim.LaborStateDeclined:
				// LLM-193: the offer was auto-declined at mint because the employer
				// can cover neither the coin nor any in-kind wage — they hold no
				// tradeable goods either (LLM-243), so no hire is possible and they
				// were never woken. Tell the worker the real reason so it looks to a
				// shop that can pay rather than re-asking the same purse. "cannot pay
				// your requested reward" is precise for the reward-relative gate (they
				// may hold some coin, just less than you asked), unlike "hasn't the
				// coin" which reads as zero.
				return fmt.Sprintf("[ok] %s cannot pay your requested reward just now — look to another shop for work.", r.EmployerName)
			case sim.LaborStateBarterPossible:
				// LLM-243: the employer can't meet the exact terms you asked (coins
				// and/or a good they don't hold), but they DO hold tradeable goods and
				// can hire in kind — not a dead end. No offer was placed and they were
				// not woken; steer the worker to re-ask for goods in trade instead of
				// routing to another shop (the LLM-222 hiring-side mirror). The worker
				// can't see the employer's inventory (omniscience guard), so name the
				// lever, not the goods — the specific wares surface in conversation.
				return fmt.Sprintf("[ok] %s can't meet those exact terms, but they've goods to trade — offer to work for goods in kind (name what you'd take, or ask what they can spare) and solicit them again.", r.EmployerName)
			}
		}
	}
	// offer_work (LLM-346): the employer-side mint. Tick-terminal like
	// solicit_work, so the result is purely informational — who the job went to,
	// and that the answer is theirs to give. When a `say` line rode along it has
	// already gone out; when SpeakTo refused it, the offer still stands and the
	// refusal reason is surfaced rather than guessed at, so the keeper knows the
	// room did not hear her (mirrors sell's say handling).
	if vc.Name == "offer_work" {
		if r, ok := cmdResult.(sim.LaborOfferResult); ok && r.State == sim.LaborStatePending {
			if r.SayRefused != "" {
				return fmt.Sprintf("[ok] Your offer of work to %s stands — they will answer on their turn. Your words did not carry: %s", r.WorkerName, r.SayRefused)
			}
			return fmt.Sprintf("[ok] Your offer of work to %s stands — they will answer on their turn.", r.WorkerName)
		}
	}
	// accept_work (LLM-163): on a real accept the offer flips Working and the
	// worker is hired; a gate-driven flip (TTL elapsed, co-presence lost, can't
	// afford) resolves terminal with NO hire. A bare [ok] read "hired" on either
	// and gave no within-result reason to stop, so the employer re-fired
	// accept_work (the accept_pay posture). Report the real outcome + steer.
	//
	// Either party can be the acceptor (LLM-346), and the sentence must be written
	// from the caller's side: an employer answering a solicit_work has hired
	// someone; a worker answering an offer_work has taken a job on and is the one
	// who must now go and do it.
	//
	// As with the pay responses, the old "Say a brief word, then call done()"
	// tail was unreachable — accept_work is terminal, so the tick ends on it. The
	// acceptor's words ride on accept_work's own `say` now (LLM-350) and are
	// echoed back here instead.
	if vc.Name == "accept_work" {
		if r, ok := cmdResult.(sim.LaborAcceptResult); ok {
			// r.Payment names both reward legs ("5 coins", "1 porridge and 2 coins"
			// — LLM-225), pre-formatted by the Command. Fall back to the coin leg
			// for a result built without it (defensive — AcceptWork always sets it).
			payment := r.Payment
			if payment == "" {
				payment = fmt.Sprintf("%d coins", r.Reward)
				if r.Reward == 1 {
					payment = "1 coin"
				}
			}
			said := ""
			if args, ok := vc.DecodedArgs.(AcceptWorkArgs); ok {
				said = sayEcho(args.Say, r.Announced, r.SayRefused)
			}
			switch {
			case r.State == sim.LaborStateWorking && r.AcceptorIsWorker:
				return fmt.Sprintf("[ok] You took on the job for %s — you are at the work now, paid %s when you finish. %sDo not accept again.", r.EmployerName, payment, said)
			case r.State == sim.LaborStateWorking:
				return fmt.Sprintf("[ok] You hired %s — they are at the work now for %s, paid when they finish. %sDo not accept again.", r.WorkerName, payment, said)
			case r.State == sim.LaborStateEnRoute && r.AcceptorIsWorker:
				// LLM-229 from the worker's side: the deal was struck away from the
				// employer's workplace, so the worker is the one who must walk there.
				return fmt.Sprintf("[ok] You took on the job for %s — make your way to their workplace and get to work once you're both there, paid %s when you finish. %sDo not accept again.", r.EmployerName, payment, said)
			case r.State == sim.LaborStateEnRoute:
				// LLM-229: the deal was struck away from your workplace, so the
				// worker is making their way there and starts once they arrive with
				// you present. Same payment phrasing; no "until T" (the window
				// hasn't started).
				return fmt.Sprintf("[ok] You hired %s — they will make their way to your workplace and get to work once you're both there, paid %s when they finish. %sDo not accept again.", r.WorkerName, payment, said)
			case r.State == sim.LaborStateExpired:
				return laborNoHireContent("That offer had already expired — too late to take it up.", said)
			case r.State == sim.LaborStateFailedUnavailable:
				return laborNoHireContent("That couldn't be arranged — one of you was no longer available, the worker was already at a job, or the employer couldn't cover the pay agreed.", said)
			}
		}
	}
	// decline_work (LLM-163): a declined offer passed in silence on a bare [ok],
	// reading to the worker as ignored, with no reason for the employer to stop
	// re-firing. The refusal now rides on decline_work's `say` (LLM-350).
	if vc.Name == "decline_work" {
		if r, ok := cmdResult.(sim.LaborDeclineResult); ok && r.State == sim.LaborStateDeclined {
			said := ""
			if args, ok := vc.DecodedArgs.(DeclineWorkArgs); ok {
				said = sayEcho(args.Say, r.Announced, r.SayRefused)
			}
			return "[ok] You declined the work. " + said + "Do not decline again."
		}
	}
	return "[ok]"
}

// settledPayContent composes the buyer's tool feedback for an instantly-
// settled quote-take (ZBBS-HOME-436). The copy is light period voice
// (ZBBS-HOME-421) with the functional tokens (done()) intact. Each sentence
// states something the model cannot see anywhere else this tick: the settle
// happened, what it ate vs pocketed, and how it feels NOW — the perception
// body it re-reads after this message still shows tick-start needs.
// formatBundleGoods renders a bundle's lines as a readable phrase for the
// buyer's settle feedback: "2 blueberries", "2 blueberries and 2 raspberries",
// or "2 blueberries, 2 raspberries, and 3 bread" (LLM-101).
func formatBundleGoods(lines []sim.QuoteLine) string {
	parts := make([]string, 0, len(lines))
	for _, ln := range lines {
		item := strings.ToLower(strings.Join(strings.Fields(string(ln.ItemKind)), " "))
		parts = append(parts, fmt.Sprintf("%d %s", ln.Qty, item))
	}
	switch len(parts) {
	case 0:
		return "those goods"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

func settledPayContent(args PayWithItemArgs, r sim.PayWithItemResult, clampNote string) string {
	// Defensive: decoded args drive the display. The command layer rejects
	// qty < 1 / amount < 0 before any settle, so these can't co-occur with
	// an Accepted result — but if they ever do, degrade to the generic ok
	// rather than voice a nonsensical settle ("0 meat", a negative price
	// normalized into a free purchase). Same no-claims-without-evidence
	// rule as the nil-result path (code_review).
	if args.Qty <= 0 || args.Amount < 0 {
		if clampNote != "" {
			return "[ok]" + clampNote
		}
		return "[ok]"
	}
	item := strings.ToLower(strings.Join(strings.Fields(args.Item), " "))
	if item == "" {
		item = "those goods"
	}
	seller := strings.TrimSpace(args.Seller)
	if seller == "" {
		seller = "them"
	}
	// LLM-101: a bundle take names every line ("2 blueberries and 2
	// raspberries"); a single-item take keeps "<qty> <item>". args.Item/Qty are
	// only the representative first line on a bundle, so the result's Lines are
	// authoritative when present.
	goods := fmt.Sprintf("%d %s", args.Qty, item)
	if len(r.Lines) > 0 {
		goods = formatBundleGoods(r.Lines)
	}
	var b strings.Builder
	if args.Amount > 0 {
		coinWord := "coins"
		if args.Amount == 1 {
			coinWord = "coin"
		}
		fmt.Fprintf(&b, "[ok] Settled on the spot — you pay %s %d %s for %s.", seller, args.Amount, coinWord, goods)
	} else {
		fmt.Fprintf(&b, "[ok] Settled on the spot — %s hands over %s for nothing.", seller, goods)
	}
	b.WriteString(clampNote)
	// ZBBS-WORK-409: an eat-here meal/drink keeps easing the need for MealMinutes
	// after the first bite, but only while the buyer stays put — walking off
	// deletes the dwell credit, wasting the food and the coins and leaving the
	// need to return. The generic "call done() now unless something else needs
	// you" closer read as "free to leave" and let NPCs bolt mid-meal (Prudence,
	// Inn, 2026-06-15), since nothing told them a sit-down meal means staying.
	// Voice the same stay message as the perception dwell line (sim.DwellStayClause)
	// so the buyer hears it consistently, fold in the WORK-391/405 anti-rebuy
	// guard, and make clear done() keeps them seated. Gated to hunger/thirst —
	// the only needs purchasable eat-here items satisfy; any other dwell falls
	// through to the generic closer below.
	if r.MealMinutes > 0 && (r.SatisfiesNeed == "hunger" || r.SatisfiesNeed == "thirst") {
		gerund := "eating"
		noMore := " Buy no more food now."
		if r.SatisfiesNeed == "thirst" {
			gerund = "drinking"
			noMore = " Buy no more drink now."
		}
		stay := sim.DwellStayClause(r.MealMinutes, r.SatisfiesNeed, " and the coins you paid")
		stay = strings.ToUpper(stay[:1]) + stay[1:]
		fmt.Fprintf(&b, " You start %s it now. %s.", gerund, stay)
		if r.KeptToInventory > 0 {
			fmt.Fprintf(&b, " The other %d goes into your pack.", r.KeptToInventory)
		}
		b.WriteString(noMore)
		fmt.Fprintf(&b, " Call done() to keep %s where you sit.", gerund)
		return b.String()
	}
	switch {
	case r.BuyerAte > 0 && r.KeptToInventory > 0:
		fmt.Fprintf(&b, " You eat %d now; %d goes into your pack — you can absorb no more.", r.BuyerAte, r.KeptToInventory)
	case r.BuyerAte == 1:
		b.WriteString(" You eat it now.")
	case r.BuyerAte > 1:
		fmt.Fprintf(&b, " You eat %d now.", r.BuyerAte)
	case r.KeptToInventory > 0:
		// Group order: others ate, the surplus pockets to the buyer.
		fmt.Fprintf(&b, " %d uneaten goes into your pack.", r.KeptToInventory)
	}
	if r.BuyerAte > 0 {
		if r.FeltAfter != "" {
			fmt.Fprintf(&b, " You still feel %s.", r.FeltAfter)
		} else {
			switch r.SatisfiesNeed {
			case "hunger":
				b.WriteString(" Your hunger is met — buy no more food now.")
			case "thirst":
				b.WriteString(" Your thirst is met — buy no more drink now.")
			default:
				b.WriteString(" You are satisfied — buy no more now.")
			}
		}
	}
	if r.TookHome {
		b.WriteString(" The goods are in your pack.")
	}
	if r.Booked {
		b.WriteString(" Your lodging is booked — the keeper will see you checked in.")
	}
	if r.LodgedNow {
		b.WriteString(" The room is yours — return to it tonight to sleep.")
	}
	if r.MendedNow {
		b.WriteString(" Your clothes are mended — good as new.")
	}
	if r.ServicedNow {
		b.WriteString(" Your equipment is dressed and trued — good for a long while yet.")
	}
	b.WriteString(" Call done() now unless something else needs you.")
	return b.String()
}

// consumeItemKey returns the normalized same-tick key for a consume call and
// true, or ("", false) for any non-consume tool or a consume whose item is empty
// after normalization (the decode/handler layer rejects empty anyway). The key
// is the item name lowercased + inner-whitespace-collapsed, so "Cheese" and
// "cheese" collapse to one key. Used by the
// LLM-91 result-aware repeat-consume guard. Unlike genericCallKey, the key is the
// ITEM only — qty is deliberately excluded: once an item has fed the actor
// nothing this tick (already sated), re-eating it at ANY quantity is the
// senseless repeat, so a key that included qty would let a re-eat at a different
// amount slip through.
func consumeItemKey(vc *ValidatedCall) (string, bool) {
	if vc == nil || vc.Name != "consume" {
		return "", false
	}
	args, ok := vc.DecodedArgs.(ConsumeArgs)
	if !ok {
		return "", false
	}
	item := strings.ToLower(strings.Join(strings.Fields(args.Item), " "))
	if item == "" {
		return "", false
	}
	return item, true
}

// consumeNoop reports whether a dispatched command result is a consume that
// eased no need — a sim.ConsumeResult with EasedNeed == false (the actor was
// already sated for what that item eases). This is the senseless-repeat signal
// the LLM-91 guard arms on. It is deliberately NOT keyed on Consumed == 0: a
// sated consume still eats and wastes a unit (Consumed >= 1) by design
// (ZBBS-WORK-391 — consuming while full wastes a unit), so "absorbed zero units"
// never happens and would make this guard dead (LLM-107). Any other result type,
// including a nil/absent result or a productive consume (EasedNeed == true), is
// not a no-op.
func consumeNoop(result any) bool {
	cr, ok := result.(sim.ConsumeResult)
	return ok && !cr.EasedNeed
}

// producedWithSpeech reports whether a commit result is a production start that
// carried a spoken `say` to the room (LLM-468). It is the conditional half of
// produce's terminality: silent produce keeps the tick open so the actor can act
// again, a spoken one ends it like any other utterance. Same result-inspection
// shape as consumeNoop above.
func producedWithSpeech(result any) bool {
	pr, ok := result.(sim.ProductionStartResult)
	return ok && pr.Spoke
}

// payOfferKey returns the normalized same-tick dedup key for a pay_with_item
// call and true, or ("", false) for any other tool or a pay call that is NOT a
// plain new offer. The key is (seller, item, disposition) — the offer's identity
// MINUS its terms. Both price (amount) AND quantity (qty) are excluded by
// design: the rule is one pending offer per (seller, item, disposition) per
// tick. Once an offer is before the seller, the buyer awaits their
// accept/decline/counter rather than placing a second offer the seller has not
// yet seen — at ANY terms; a genuine change of price or quantity belongs in the
// NEXT tick, after the response. Excluding the terms is also what makes the
// guard robust to the observed storm, which re-offered the same item at a
// drifting price (5 coins, then 10, then 10…) every round — a key that included
// the terms would let each drifted re-offer straight through.
//
// Scoped to the default pending-offer path: a quote take (quote_id) closes the
// deal instantly and a counter-response (in_response_to) is a deliberate,
// distinct move, so neither storms — both pass through untouched, matching the
// commitResultContent steer's scope. Seller and item
// are lowercased + whitespace-collapsed so trivial spacing/case
// drift in a repeat still matches. The disposition byte (keep vs consume-now)
// keeps a genuine "buy one to keep AND one to eat now" pair distinct.
func payOfferKey(vc *ValidatedCall) (string, bool) {
	// offer_trade lowers onto a PayWithItemArgs (ZBBS-HOME-407), so it mints
	// the same kind of pending offer and earns the same same-tick dedup.
	if vc == nil || (vc.Name != "pay_with_item" && vc.Name != "offer_trade") {
		return "", false
	}
	args, ok := vc.DecodedArgs.(PayWithItemArgs)
	if !ok {
		return "", false
	}
	if args.QuoteID != 0 || args.InResponseTo != 0 {
		return "", false
	}
	seller := strings.ToLower(strings.Join(strings.Fields(args.Seller), " "))
	item := strings.ToLower(strings.Join(strings.Fields(args.Item), " "))
	if seller == "" || item == "" {
		return "", false
	}
	disposition := "keep"
	if args.ConsumeNow {
		disposition = "consume"
	}
	return seller + "\x00" + item + "\x00" + disposition, true
}

// payDedupKey returns the same-tick dedup key for a bare `pay` call —
// normalized (recipient, for) — and true when vc is a pay (LLM-202). The
// recipient and `for` are normalized exactly as payOfferKey normalizes the
// seller (lowercase + whitespace-collapse) because the model types free-text
// that drifts in casing and spacing across rounds. Amount is deliberately
// excluded — the same rationale that excludes price from payOfferKey: a re-fire
// at a drifted amount is still the same intended payment and must match. The
// `for` text IS part of the key so one tick can carry two genuinely distinct
// payments to the same recipient (a wage and a separate gift); only a repeat of
// the same (recipient, reason) is the double-settle this guards. An empty `for`
// keys cleanly (two bare same-recipient pays with no stated reason are a
// double). Not a resolution-id key like ledgerResolutionID — a bare pay has no
// ledger to key on.
func payDedupKey(vc *ValidatedCall) (string, bool) {
	if vc == nil || vc.Name != "pay" {
		return "", false
	}
	args, ok := vc.DecodedArgs.(PayArgs)
	if !ok {
		return "", false
	}
	recipient := strings.ToLower(strings.Join(strings.Fields(args.Recipient), " "))
	if recipient == "" {
		return "", false
	}
	forText := strings.ToLower(strings.Join(strings.Fields(args.For), " "))
	return recipient + "\x00" + forText, true
}

// sceneQuoteKey returns the same-tick repeat-QUOTE dedup key for a scene_quote
// call (ZBBS-HOME-433 — the seller-side analogue of payOfferKey). One quote per
// (item, qty, disposition, target) per tick: price (amount) is deliberately
// EXCLUDED, so a re-post at a drifting price (4 coins, then 5) still matches —
// within one tick a re-quote of the same lot is churn at any price (the live
// John×Ezekiel bread storm: five identical scene_quote calls in one tick, all
// succeeding, terminal budget_forced). Qty IS part of the key — unlike a
// buyer's pending offer, a different lot size (1 bread vs 3 bread) is a
// genuinely distinct standing offer, and the substrate's own supersede/coexist
// rules should decide its fate (code_review #415). Cross-tick re-pricing stays
// legal and rides the supersede path. Returns ("", false) for non-quote calls
// or undecodable args.
func sceneQuoteKey(vc *ValidatedCall) (string, bool) {
	if vc == nil || vc.Name != "sell" {
		return "", false
	}
	args, ok := vc.DecodedArgs.(SceneQuoteArgs)
	if !ok {
		return "", false
	}
	if len(args.Lines) == 0 {
		return "", false
	}
	// Order-independent key over the bundle's lines (LLM-101): each "item\x01qty",
	// sorted, so "blueberries+raspberries" and the reverse hash the same. Amount
	// is excluded (a re-post at a drifting price rides the substrate's supersede
	// path); qty IS part of each line (a different lot size is a distinct offer).
	parts := make([]string, 0, len(args.Lines))
	for _, ln := range args.Lines {
		item := strings.ToLower(strings.Join(strings.Fields(ln.ItemKind), " "))
		if item == "" {
			return "", false
		}
		parts = append(parts, item+"\x01"+strconv.Itoa(ln.Qty))
	}
	sort.Strings(parts)
	target := strings.ToLower(strings.Join(strings.Fields(args.TargetBuyer), " "))
	disposition := "keep"
	if args.ConsumeNow {
		disposition = "consume"
	}
	return strings.Join(parts, "\x00") + "\x00" + disposition + "\x00" + target, true
}

// genericCallKey returns the same-tick identical-call dedup key for the
// ZBBS-HOME-414 guard: the tool name plus a canonical JSON rendering of the
// DECODED args. Returns ("", false) — the guard does NOT apply — unless the tool
// is on the explicit action allowlist below.
//
// The allowlist (rather than a broad "any non-observation commit" test) is
// deliberate: the guard is tool-agnostic in MECHANISM but explicit in SCOPE, so
// (a) a newly-added commit tool does not silently inherit same-args dedup that
// may be wrong for it, and (b) the boundary the code enforces matches the
// boundary this comment documents (code_review HOME-414). The offer family is
// also excluded — it owns its own broader, success-only same-tick guard
// (payOfferKey) — as are observation-class calls (pure thinking is not
// penalized, ZBBS-WORK-321). speak is excluded by an explicit name guard below:
// speech cadence is not generic dedup's to own, and a production speak is
// terminal-on-success (LLM-321) so a repeat can't reach here anyway. consume is also excluded
// (LLM-91): a byte-identical repeat consume while still in need is PRODUCTIVE
// (it eats another unit and eases the need further), so the syntactic "identical
// = useless" premise is false for it. It has its own result-aware guard keyed on
// a no-op outcome (consumeItemKey + dispatchOutcome.consumedNothing).
//
// The key is canonical JSON (json.Marshal), not %#v: for structs encoding/json
// preserves field order and for maps it sorts keys, so the "canonical decoded
// args" claim holds for any future allowlisted tool's arg shape rather than
// relying on the implicit invariant that every arg type is a flat struct.
func genericCallKey(vc *ValidatedCall) (string, bool) {
	if vc == nil || vc.Entry == nil {
		return "", false
	}
	// speak is never generically deduped: speech cadence is not a "byte-identical
	// = useless" case, and a production speak is terminal-on-success (LLM-321) so a
	// repeat can't reach here. Explicit name guard so the boundary holds regardless
	// of how a test or custom registry classes a tool named speak.
	if vc.Name == "speak" {
		return "", false
	}
	if _, isOffer := payOfferKey(vc); isOffer {
		return "", false
	}
	if vc.Entry.Class == ClassObservation {
		return "", false
	}
	switch vc.Name {
	case "deliver_order", "move_to":
		// The action tools where a byte-identical repeat in one tick is provably
		// useless: deliver_order cannot hand the same order over twice, and move_to
		// to your current place (or a re-fire of the same destination) is a no-op.
		// The pay-offer resolution family (accept_pay / decline_pay / counter_pay /
		// withdraw_pay) left this allowlist in LLM-104: resolvedLedgerThisTick now
		// guards them earlier and more broadly, keyed on the ledger id (shared across
		// the family) rather than this narrower name + full-args key. consume is NOT
		// here — a repeat consume while still in need is productive; see consumeItemKey
		// for its result-aware guard. gather and craft are NOT here either (LLM-120):
		// both key on the tool NAME alone (gatheredThisTick / craftedThisTick in the
		// dispatch loop), not name+args — gather's `qty` is vestigial (LLM-87) and
		// craft's item resolves through aliases (LLM-113: Nail/nail/nails → one kind),
		// so a byte-identical name+args key would miss a drifted re-fire of either.
	default:
		return "", false
	}
	b, err := json.Marshal(vc.DecodedArgs)
	if err != nil {
		// A non-marshalable args value can't be keyed — fail open (no dedup)
		// rather than risk a wrong collision. Not expected for any allowlisted
		// tool's args.
		return "", false
	}
	return vc.Name + "\x00" + string(b), true
}

// ledgerResolutionID returns the pay-offer LedgerID that a resolution-family call
// (accept_pay / decline_pay / counter_pay / withdraw_pay) acts on, and true; any
// other call returns (0, false) and the resolvedLedgerThisTick guard does not
// apply. Each of these four tools answers a single pending pay-offer addressed by
// `ledger_id`, and the first answer this tick moves that ledger out of `pending`,
// so the guard keys on the id alone (shared across the family) to reject a second
// answer — see the guard in the dispatch loop. The match binds BOTH the tool name
// and the decoded-arg shape and fails closed on a mismatch: name alone would miss a
// future arg-struct reuse, while shape alone would wrongly guard any other tool that
// happens to decode to one of these structs (or a test / custom registry that pairs
// a different name with these decoders). Over-blocking a dispatch guard is worse
// than under-blocking — under it, the command's own "no longer pending" check still
// applies — so when the name is right but the shape is wrong we return (0, false).
func ledgerResolutionID(vc *ValidatedCall) (LenientID, bool) {
	if vc == nil {
		return 0, false
	}
	switch vc.Name {
	case "accept_pay":
		if a, ok := vc.DecodedArgs.(AcceptPayArgs); ok {
			return a.LedgerID, true
		}
	case "decline_pay":
		if a, ok := vc.DecodedArgs.(DeclinePayArgs); ok {
			return a.LedgerID, true
		}
	case "counter_pay":
		if a, ok := vc.DecodedArgs.(CounterPayArgs); ok {
			return a.LedgerID, true
		}
	case "withdraw_pay":
		if a, ok := vc.DecodedArgs.(WithdrawPayArgs); ok {
			return a.LedgerID, true
		}
	}
	return 0, false
}

// resolvedPayOfferIDs projects the resolvedLedgerThisTick guard set (keyed by
// the lenient wire id) onto the sim.LedgerID set perception.WithResolvedPayOffers
// wants, so the within-tick re-render withholds an already-answered offer from
// the seller cue (LLM-173). Both underlying types are uint64. Returns nil for an
// empty set so the conversion allocates nothing on a refresh that follows a
// non-resolution commit (e.g. a consume), leaving WithResolvedPayOffers a no-op.
func resolvedPayOfferIDs(resolved map[LenientID]struct{}) map[sim.LedgerID]struct{} {
	if len(resolved) == 0 {
		return nil
	}
	out := make(map[sim.LedgerID]struct{}, len(resolved))
	for id := range resolved {
		out[sim.LedgerID(id)] = struct{}{}
	}
	return out
}

// laborResolutionID returns the LaborID that an employer-side labor-resolution
// call (accept_work / decline_work) acts on, and true; any other call returns
// (0, false) and the resolvedLaborThisTick guard does not apply. Both tools
// answer a single pending labor offer addressed by `labor_id`, and the first
// answer this tick moves that offer out of `pending` (to working or declined),
// so the guard keys on the id alone — shared across the pair — to reject a second
// answer this tick before it reaches AcceptWork's raw "no longer pending" error.
// The labor mirror of ledgerResolutionID (LLM-164 / LLM-104): it binds BOTH the
// tool name and the decoded-arg shape and fails closed on a mismatch, since
// over-blocking a dispatch guard is worse than under-blocking — under it, the
// command's own not-pending gate still backstops the call.
func laborResolutionID(vc *ValidatedCall) (LenientID, bool) {
	if vc == nil {
		return 0, false
	}
	switch vc.Name {
	case "accept_work":
		if a, ok := vc.DecodedArgs.(AcceptWorkArgs); ok {
			return a.LaborID, true
		}
	case "decline_work":
		if a, ok := vc.DecodedArgs.(DeclineWorkArgs); ok {
			return a.LaborID, true
		}
	}
	return 0, false
}

// conversationIDFromPayload returns the narrative-beat scene id the perception
// was built within (sim.Scene.ID, via the primary SceneView) — the cross-tick,
// cross-participant conversation_id (ZBBS-HOME-397). All ticks of one
// conversation beat, by every participant, resolve to the same primary scene, so
// stamping it on the chat rows lets memory-api collapse the whole exchange into
// one conversation in the admin viewer. Empty when no primary scene resolved (a
// solo tick with no active huddle) so the row stays ungrouped, like
// companion-mode chat.
func conversationIDFromPayload(p perception.Payload) string {
	if p.Primary == nil {
		return ""
	}
	return string(p.Primary.SceneID)
}

// conversationIDForChat derives the conversation_id grouping key for the chat
// rows this tick emits (ZBBS-HOME-417). It prefers the actor's huddle id — the
// actual conversation unit, which begins when an exchange starts and ends when
// the silence sweep concludes it — so the admin viewer groups one exchange per
// huddle. It falls back to the scene id (conversationIDFromPayload) for
// huddle-less speech (a solo tick), preserving that case's prior grouping.
//
// Why the huddle, not the scene: an indoor structure scene is intentionally
// durable and reused across every conversation at the structure (it anchors the
// pay ledger), so keying conversation_id on it collapsed a busy structure's
// entire multi-day history into one "conversation" in the viewer. The huddle
// rotates per exchange — which is the grouping HOME-397 intended. The admin
// chat viewer is the only consumer of conversation_id, so this is a pure
// grouping-granularity change with no behavioral effect.
func conversationIDForChat(actor *sim.ActorSnapshot, p perception.Payload) string {
	if actor != nil && actor.CurrentHuddleID != "" {
		return string(actor.CurrentHuddleID)
	}
	return conversationIDFromPayload(p)
}

// fullPerceptionPrompt joins the stable identity block, the durable turn, and
// the ephemeral current-tick context into the single prompt the model
// effectively saw, for the umbilical debug surface (ZBBS-WORK-364). The three
// travel separately on the wire (stable = /chat/send stable_context in the
// system zone; durable = persisted message; ephemeral = ephemeral_context
// attached to the current turn), but the operator wants to read the whole
// perception. Stable leads, mirroring its system-prompt placement (LLM-501).
func fullPerceptionPrompt(r perception.RenderedPrompt) string {
	parts := make([]string, 0, 3)
	if r.StableText != "" {
		parts = append(parts, r.StableText)
	}
	parts = append(parts, r.Text)
	if r.EphemeralText != "" {
		parts = append(parts, r.EphemeralText)
	}
	return strings.Join(parts, "\n\n")
}

// failBeforeRender stages a "couldn't even render perception" exit:
// nothing addressed, the whole consumed batch carries forward.
func failBeforeRender(result sim.TickResult, job tickJob, llmErrClass string) sim.TickResult {
	result.TerminalStatus = sim.TickStatusFailedBeforeRender
	result.UnaddressedWarrants = copyWarrants(job.warrants)
	if llmErrClass != "" {
		result.LLMErrorClass = llmErrClass
	}
	return result
}

// llmErrorToStatus maps an llm.ErrorClass into the matching
// TickTerminalStatus for the harness's CompleteReactorTick handoff. The
// first-iteration distinction matters: a failure on iteration 0 means
// nothing was addressed (FailedBeforeRender semantically — the model
// never produced any response the actor could act on), while later
// iterations imply rendered content the actor *did* address (so prior
// successful tool calls count and FailedAfterRender is correct).
func llmErrorToStatus(cls llm.ErrorClass, iter int) sim.TickTerminalStatus {
	if cls == llm.ErrorContextCancelled {
		return sim.TickStatusShutdown
	}
	if iter == 0 {
		return sim.TickStatusFailedBeforeRender
	}
	return sim.TickStatusFailedAfterRender
}

// toolResultMsg builds a "tool" role message with the given content and
// matching tool_call_id. Centralized so the harness can't accidentally
// emit a tool message without the call_id (the provider would reject the
// transcript on the next Complete).
func toolResultMsg(callID, content string) llm.Message {
	return llm.Message{
		Role:       llm.RoleTool,
		ToolCallID: callID,
		Content:    content,
	}
}

// formatValidationError produces the "tool" message content the model
// sees for a rejected call. Format: `[error: <kind>] <message>` — the
// kind is the stable label the design contract pins (e.g.
// "tool_unavailable_in_this_build"), the message is the per-call detail.
func formatValidationError(v *ValidationError) string {
	if v == nil {
		return "[error: unknown] validation error"
	}
	return fmt.Sprintf("[error: %s] %s", v.Kind, v.Message)
}

// copyWarrants returns a fresh slice with the same warrants. Defensive
// against later mutation by either the caller's job.warrants or the
// recipient (CompleteReactorTick).
func copyWarrants(src []sim.WarrantMeta) []sim.WarrantMeta {
	if len(src) == 0 {
		return nil
	}
	out := make([]sim.WarrantMeta, len(src))
	copy(out, src)
	return out
}

// persistTickToolResults runs at tick-exit (deferred from RunTick) and
// writes the last batch's tool-result rows to the provider's history
// via the optional llm.ToolResultPersister. Closes the v1 orphan-
// tool_use defect: when a terminal-class tool ends the tick without
// firing another Complete, the assistant's tool_call sits in history
// with no matching tool_result row, breaking the next tool-use call.
//
// Gates:
//
//   - Adapter must implement llm.ToolResultPersister. FakeClient does;
//     a hypothetical non-history adapter would skip this branch.
//   - Model must be non-empty (no VA → no persist target).
//   - Status must be one of the "clean exit with possibly-orphan
//     tool_results" set: TickStatusDone (terminal tool fired),
//     TickStatusSuccess (TerminalOnSuccess commit ended the tick), or
//     TickStatusBudgetForced (budget exhausted before next Complete).
//     Other statuses are either no-op (Skipped, Shutdown, Stale,
//     FailedBeforeRender) or have already had their results delivered
//     via a prior Complete (FailedAfterRender — the failed Complete
//     posted iter N-1's results).
//   - Transcript must end in one or more tool messages after the last
//     assistant — that's the unpersisted last batch.
//
// Errors are logged, not returned: the TickResult is already populated
// and the worker's CompleteReactorTick contract is the harness's
// authoritative exit. A persist failure leaves orphans in history —
// surfaced via logs for monitoring.
// maxToolCallArgSummaryLen bounds each tool call's argument blob in the chat-ring
// summary (ZBBS-HOME-382) so a large argument can't bloat a debug record.
const maxToolCallArgSummaryLen = 200

// summarizeToolCalls renders a response's tool calls into a compact one-line
// summary for the umbilical chat ring (ZBBS-HOME-382): "name(args); name(args)",
// each argument blob truncated on a rune boundary. Debug-surface display only —
// never parsed. strings.Builder keeps this cheap on the tick-worker path.
func summarizeToolCalls(calls []llm.RawToolCall) string {
	var b strings.Builder
	for i, c := range calls {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('(')
		b.WriteString(truncateUTF8(string(c.Arguments), maxToolCallArgSummaryLen))
		b.WriteByte(')')
	}
	return b.String()
}

// truncateUTF8 caps s at max bytes without splitting a UTF-8 rune (a tool-arg
// blob can carry multibyte runes), appending "..." when it truncates.
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}

func (h *Harness) persistTickToolResults(
	ctx context.Context,
	model, sceneID, conversationID string,
	transcript []llm.Message,
	status sim.TickTerminalStatus,
) {
	if model == "" {
		return
	}
	switch status {
	case sim.TickStatusDone, sim.TickStatusSuccess, sim.TickStatusBudgetForced:
		// proceed
	default:
		return
	}
	persister, ok := h.client.(llm.ToolResultPersister)
	if !ok {
		return
	}
	results := extractTrailingToolResults(transcript)
	if len(results) == 0 {
		return
	}
	// On engine shutdown (parent ctx cancelled): skip persist — the
	// orphan is acceptable, engine restart is the bigger concern. On
	// any other live path: run persist on a fresh detached context
	// with a short budget so a future per-tick deadline (not yet
	// implemented but plausible) doesn't abort cleanup that's
	// specifically there to prevent provider-side corruption (R1
	// finding #7).
	if ctx.Err() != nil {
		return
	}
	persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	err := persister.PersistToolResults(persistCtx, llm.PersistRequest{
		Model:          model,
		SceneID:        sceneID,
		ConversationID: conversationID,
		Results:        results,
	})
	if err != nil {
		log.Printf("handlers: persist tick tool results (model=%q scene=%q n=%d): %v",
			model, sceneID, len(results), err)
	}
}

// persistTimeout caps the deferred persist call. v1's per-attempt
// budget is 90s but the retry schedule sums to ~800ms; 5s here is
// generous for the typical case and bounded enough that engine
// shutdown won't block on it past the operator's patience.
const persistTimeout = 5 * time.Second

// extractTrailingToolResults walks the transcript from the end,
// collecting tool messages until the first non-tool boundary (the
// preceding assistant message). Defensive: tool messages without
// ToolCallID are skipped silently (toolResultMsg ensures the ID is
// set, but a future caller could violate that).
func extractTrailingToolResults(transcript []llm.Message) []llm.ToolResult {
	// Find first non-tool from the end.
	end := len(transcript)
	start := end
	for i := end - 1; i >= 0; i-- {
		if transcript[i].Role != llm.RoleTool {
			start = i + 1
			break
		}
		if i == 0 {
			start = 0
		}
	}
	if start >= end {
		return nil
	}
	out := make([]llm.ToolResult, 0, end-start)
	for i := start; i < end; i++ {
		m := transcript[i]
		if m.ToolCallID == "" {
			continue
		}
		out = append(out, llm.ToolResult{ID: m.ToolCallID, Content: m.Content})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
