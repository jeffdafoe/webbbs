package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim/telemetry"
)

// umbilical.go — the read half of the out-of-band debug/control surface. These
// routes are NOT part of the player/client contract; they exist for an operator
// (work / home / jeff) to introspect a running engine over the standard HTTP
// server. Two gates stand between a caller and these handlers:
//
//   1. requireOperator (auth.go): a valid salem-realm token PLUS the llm-memory
//      plugins/administer capability — tighter than the normal read gate, since
//      every player is salem-realm but only operators hold plugins/administer.
//   2. Registration is conditional on SetTelemetry having been called, which
//      cmd/engine does only under UMBILICAL_ENABLED. Off by default → no route.
//
// The read surface is strictly additive and never a driver: it reads the same
// lock-free published snapshot the client routes read, plus the in-memory
// telemetry ring. It cannot influence the simulation (that's the control half,
// built separately and whitelisted). The invariant holds — the engine is fully
// correct with the umbilical disconnected.

// TelemetryRecordDTO is one buffered tick-lifecycle record on the wire. Mirrors
// sim.TickTelemetryRecord; Detail is the structured + REDACTED detail map (the
// sink contract guarantees no raw prompts / LLM responses / private text ever
// land in it), omitted when empty.
type TelemetryRecordDTO struct {
	At        time.Time         `json:"at"`
	ActorID   string            `json:"actor_id,omitempty"`
	AttemptID string            `json:"attempt_id,omitempty"`
	Kind      string            `json:"kind"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// TelemetryStatsDTO is the ring-buffer accounting: how much history is retained
// and whether the buffer is saturating (dropped climbing → reader behind the
// retention window, not an error).
type TelemetryStatsDTO struct {
	Capacity int    `json:"capacity"`
	Size     int    `json:"size"`
	Written  uint64 `json:"written"`
	Dropped  uint64 `json:"dropped"`
}

// UmbilicalTelemetryDTO is the GET /api/village/umbilical/telemetry response:
// the ring's accounting plus the buffered records, oldest first.
type UmbilicalTelemetryDTO struct {
	ContractVersion int                  `json:"contract_version"`
	Stats           TelemetryStatsDTO    `json:"stats"`
	Records         []TelemetryRecordDTO `json:"records"`
}

// UmbilicalStateDTO is the GET /api/village/umbilical/state response: a coarse
// introspection of the running engine off the published snapshot. World embeds
// the same coarse world DTO the client /world route serves; the rest is
// operator-only debug detail (in-flight tick count, entity-table sizes,
// telemetry accounting).
type UmbilicalStateDTO struct {
	ContractVersion int                `json:"contract_version"`
	PublishedAt     time.Time          `json:"published_at"`
	World           WorldStateDTO      `json:"world"`
	TicksInFlight   int                `json:"ticks_in_flight"`
	Counts          UmbilicalCountsDTO `json:"counts"`
	// CoinSupply is the on-map money-supply gauge (LLM-410): total coin held
	// across non-decorative actors, split resident vs transient-visitor.
	// Surfaced here because /state is the daily check-in and the import/export
	// tier is about to start changing the money supply — this makes the inflow
	// observable before it moves. Computed off the snapshot, decoratives excluded.
	CoinSupply sim.CoinSupply    `json:"coin_supply"`
	Telemetry  TelemetryStatsDTO `json:"telemetry"`
	// Checkpoint is the durable-checkpoint health summary — surfaced here too
	// (not just on /checkpoint-health) because /state is the daily check-in
	// route, and consecutive_failures is the at-a-glance durability signal.
	Checkpoint sim.CheckpointHealthSnapshot `json:"checkpoint"`
	// WS is the event-hub delivery accounting (WORK-434) — frame-drop /
	// slow-consumer / connected-client health. Surfaced here because /state is
	// the daily check-in route and a silent live-frame drop (the suspected cause
	// of stale-on-client noticeboards) is otherwise invisible off the box. Zero
	// when no event hub is attached (headless/test).
	WS WSDeliveryStatsDTO `json:"ws"`
	// ConfigWarnings (LLM-60) is the live data-config audit: one line per village
	// object that is misconfigured in a tolerated-but-wrong way (today: a gather/
	// eat source with no display_name, which the resolver silently can't reach).
	// Computed off the snapshot on every read via sim.ConfigWarnings, so it
	// reflects live edits, not just boot state. Omitted when clean.
	ConfigWarnings []string `json:"config_warnings,omitempty"`
}

// UmbilicalCountsDTO is the size of each published entity table — a cheap
// "what's loaded right now" view for an operator, derived purely from the
// snapshot (no new plumbing).
type UmbilicalCountsDTO struct {
	Actors         int `json:"actors"`
	Huddles        int `json:"huddles"`
	Scenes         int `json:"scenes"`
	Structures     int `json:"structures"`
	Orders         int `json:"orders"`
	VillageObjects int `json:"village_objects"`
	Quotes         int `json:"quotes"`
	PayLedger      int `json:"pay_ledger"`
	ActionLog      int `json:"action_log"`
	PriceBook      int `json:"price_book"`
}

// handleUmbilicalTelemetry dumps the tick-telemetry ring (oldest first) with
// its accounting. Gated by requireOperator + registered only when the ring is
// attached, so s.telemetry is never nil here.
func (s *Server) handleUmbilicalTelemetry(w http.ResponseWriter, _ *http.Request) {
	recs := s.telemetry.Snapshot()
	out := UmbilicalTelemetryDTO{
		ContractVersion: ContractVersion,
		Stats:           telemetryStatsDTO(s.telemetry.Stats()),
		Records:         make([]TelemetryRecordDTO, 0, len(recs)),
	}
	for _, r := range recs {
		out.Records = append(out.Records, TelemetryRecordDTO{
			At:        r.At,
			ActorID:   string(r.ActorID),
			AttemptID: string(r.AttemptID),
			Kind:      r.Kind,
			Detail:    r.Detail,
		})
	}
	writeJSON(w, out)
}

// handleUmbilicalState serves a coarse introspection of the running engine off
// the published snapshot plus the telemetry ring's accounting.
func (s *Server) handleUmbilicalState(w http.ResponseWriter, _ *http.Request) {
	out := umbilicalStateFromSnapshot(s.world.Published(), s.telemetry.Stats())
	out.Checkpoint = s.checkpointHealth.Snapshot()
	if s.hub != nil {
		out.WS = s.hub.Stats()
	}
	writeJSON(w, out)
}

// umbilicalStateFromSnapshot maps the published snapshot + ring stats to the
// state DTO. A nil snapshot (engine published nothing yet) yields a zero-valued
// world/counts view rather than panicking.
func umbilicalStateFromSnapshot(s *sim.Snapshot, st telemetry.Stats) UmbilicalStateDTO {
	out := UmbilicalStateDTO{
		ContractVersion: ContractVersion,
		Telemetry:       telemetryStatsDTO(st),
	}
	if s == nil {
		return out
	}
	out.PublishedAt = s.PublishedAt
	out.World = worldStateFromSnapshot(s)
	out.TicksInFlight = countTicksInFlight(s)
	out.Counts = UmbilicalCountsDTO{
		Actors:         len(s.Actors),
		Huddles:        len(s.Huddles),
		Scenes:         len(s.Scenes),
		Structures:     len(s.Structures),
		Orders:         len(s.Orders),
		VillageObjects: len(s.VillageObjects),
		Quotes:         len(s.Quotes),
		PayLedger:      len(s.PayLedger),
		ActionLog:      len(s.ActionLog),
		PriceBook:      len(s.PriceBook),
	}
	out.CoinSupply = sim.ComputeCoinSupply(s)
	out.ConfigWarnings = sim.ConfigWarnings(s.VillageObjects)
	return out
}

// countTicksInFlight counts actors mid-tick (an LLM tick dispatched but not yet
// resolved) in the snapshot — the headline "is the engine busy" debug signal.
func countTicksInFlight(s *sim.Snapshot) int {
	n := 0
	for _, a := range s.Actors {
		if a != nil && a.TickInFlight {
			n++
		}
	}
	return n
}

func telemetryStatsDTO(st telemetry.Stats) TelemetryStatsDTO {
	return TelemetryStatsDTO{
		Capacity: st.Capacity,
		Size:     st.Size,
		Written:  st.Written,
		Dropped:  st.Dropped,
	}
}

// Action-log view bounds. The action log is retention-bounded in the world
// (hours of history); the umbilical returns a tail of it, capped so a careless
// request can't serialize the whole thing.
const (
	defaultActionsLimit = 200
	maxActionsLimit     = 1000
)

// ActionLogEntryDTO is one committed agent/engine action on the wire. Unlike
// the tick telemetry (which is redacted to mechanics), this is the
// what-actually-happened trail — ActionType + the engine-authored Text + the
// HuddleID context. That content is the point: it's what surfaces an NPC that's
// ticking fine but behaving nonsensically (double-talking, speaking after
// leaving — `HuddleID` empty on a `spoke` is the tell — or oscillating between
// anchors, visible as a repeated `walked` pattern for one actor).
type ActionLogEntryDTO struct {
	ActorID    string    `json:"actor_id"`
	OccurredAt time.Time `json:"occurred_at"`
	ActionType string    `json:"action_type"`
	Text       string    `json:"text,omitempty"`
	HuddleID   string    `json:"huddle_id,omitempty"`
}

// UmbilicalActionsDTO is the GET /api/village/umbilical/actions response: a tail
// of the committed-action log (chronological, oldest-first within the window).
// Total is the full log size before filtering; Returned is how many entries
// this response carries after the optional actor filter + limit.
type UmbilicalActionsDTO struct {
	ContractVersion int                 `json:"contract_version"`
	Total           int                 `json:"total"`
	Returned        int                 `json:"returned"`
	Actions         []ActionLogEntryDTO `json:"actions"`
}

// handleUmbilicalActions serves a tail of the world's committed action log off
// the published snapshot. Query params: `actor` (optional — filter to one
// ActorID, e.g. to inspect a single NPC's recent behavior for an oscillation
// pattern), `limit` (optional — max entries, default 200, capped at 1000).
// Read-only and lock-free over the snapshot, like the other read routes.
func (s *Server) handleUmbilicalActions(w http.ResponseWriter, r *http.Request) {
	var log []sim.ActionLogEntry
	if snap := s.world.Published(); snap != nil {
		log = snap.ActionLog
	}
	total := len(log)

	q := r.URL.Query()
	if actor := q.Get("actor"); actor != "" {
		filtered := make([]sim.ActionLogEntry, 0, len(log))
		for _, e := range log {
			if string(e.ActorID) == actor {
				filtered = append(filtered, e)
			}
		}
		log = filtered
	}

	limit := parseActionsLimit(q.Get("limit"))
	// Tail: keep the most recent `limit`, preserving chronological order so a
	// per-actor scan reads left-to-right in time (the way an A→B→A oscillation
	// or a leave-then-speak sequence is easiest to spot).
	if len(log) > limit {
		log = log[len(log)-limit:]
	}

	out := UmbilicalActionsDTO{
		ContractVersion: ContractVersion,
		Total:           total,
		Returned:        len(log),
		Actions:         make([]ActionLogEntryDTO, 0, len(log)),
	}
	for _, e := range log {
		out.Actions = append(out.Actions, ActionLogEntryDTO{
			ActorID:    string(e.ActorID),
			OccurredAt: e.OccurredAt,
			ActionType: string(e.ActionType),
			Text:       e.Text,
			HuddleID:   string(e.HuddleID),
		})
	}
	writeJSON(w, out)
}

// parseActionsLimit reads the `limit` query value, clamping to (0, maxActionsLimit]
// and falling back to defaultActionsLimit when absent, unparseable, or <= 0.
func parseActionsLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultActionsLimit
	}
	if n > maxActionsLimit {
		return maxActionsLimit
	}
	return n
}

// TickerHealthEntryDTO is one interval goroutine's liveness AND its cadence
// contract on the wire (LLM-395).
//
// IntervalSeconds is what the ticker declared it should beat at, and Stale is the
// engine's own verdict against that declaration — so the reader no longer has to
// know every cadence by heart to interpret a LastFire. Registered=false marks a
// ticker that beats but never declared a cadence: it is listed, but it is
// invisible to the ticker_stale alarm, so it reads as a hole in the coverage
// rather than as a healthy ticker.
//
// LastFire is the zero value for a ticker that registered but has not yet beaten
// — normal for the first interval after boot, and permanent for a goroutine that
// never started.
type TickerHealthEntryDTO struct {
	Name            string    `json:"name"`
	Count           uint64    `json:"count"`
	LastFire        time.Time `json:"last_fire"`
	Registered      bool      `json:"registered"`
	IntervalSeconds float64   `json:"interval_seconds"`
	Stale           bool      `json:"stale"`
}

// UmbilicalTickerHealthDTO is the GET /api/village/umbilical/ticker-health
// response: per-interval-goroutine cadence contract + last-fire + cumulative fire
// count, sorted by name. The signal: a ticker goroutine that died or wedged stops
// beating, so a LastFire stale relative to that ticker's DECLARED cadence (or a
// Count that stops advancing across two polls) flags a silently-stopped cadence
// driver. `now` is the server's wall-clock at response time so the operator
// computes staleness without assuming clock alignment.
//
// This is the pull view of the same judgement the ticker_stale alarm makes and
// stamps onto every umbilical response (httpapi/alarms.go) — the alarm carries the
// names, this route carries the per-ticker detail behind them.
//
// The list is COMPLETE: both the sim-package tickers and the cascade-package ones
// (atmosphere, consolidation, storm, visitor, …) fold into the one registry.
type UmbilicalTickerHealthDTO struct {
	ContractVersion int                    `json:"contract_version"`
	Now             time.Time              `json:"now"`
	Tickers         []TickerHealthEntryDTO `json:"tickers"`
}

// handleUmbilicalTickerHealth serves the per-ticker liveness view off the
// world's TickerHealth registry (its own mutex — safe to read off the world
// goroutine). Read-only, like the other umbilical read routes.
func (s *Server) handleUmbilicalTickerHealth(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	entries := s.world.TickerHealthSnapshot()
	out := UmbilicalTickerHealthDTO{
		ContractVersion: ContractVersion,
		Now:             now,
		Tickers:         make([]TickerHealthEntryDTO, 0, len(entries)),
	}
	for _, e := range entries {
		out.Tickers = append(out.Tickers, TickerHealthEntryDTO{
			Name:            e.Name,
			Count:           e.Count,
			LastFire:        e.LastFire,
			Registered:      e.Registered,
			IntervalSeconds: e.Interval.Seconds(),
			Stale:           e.IsStale(now),
		})
	}
	writeJSON(w, out)
}

// UmbilicalWorldCommandHealthDTO is the GET
// /api/village/umbilical/world-command-health response: the liveness of the single
// world command goroutine, measured directly by a periodic no-op probe (LLM-402).
//
// The pull view behind the world_command_stalled alarm, and the sibling of
// /ticker-health: that route says WHETHER the cadence drivers are beating, this one
// says whether the loop they all feed is still serving. When every ticker has gone
// quiet, these two together are what separate "the world is wedged" from "the world
// is fine and the tickers are dying" — a distinction staleness alone cannot make.
//
// It also carries the signal the ALARM deliberately withholds: slowest_round_trip_ms.
// A world loop whose probes are landing at 4.5s of a 5s deadline is about to fall
// over, and that is worth seeing coming — but a successful probe is a successful
// probe, and a fire alarm that fires on "getting slower" is one nobody trusts. The
// early warning belongs on a route you can watch; the alarm stays for emergencies.
type UmbilicalWorldCommandHealthDTO struct {
	ContractVersion int                            `json:"contract_version"`
	Now             time.Time                      `json:"now"`
	Health          sim.WorldCommandHealthSnapshot `json:"health"`
}

// handleUmbilicalWorldCommandHealth serves the world-command liveness view off the
// world's own recorder (its own mutex — safe to read off the world goroutine, which
// is the entire point: a route that had to ASK the world goroutine how it was doing
// would hang in exactly the incident it exists to report). Read-only, operator-gated
// like every other umbilical read.
func (s *Server) handleUmbilicalWorldCommandHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, UmbilicalWorldCommandHealthDTO{
		ContractVersion: ContractVersion,
		Now:             time.Now().UTC(),
		Health:          s.world.WorldCommandHealthSnapshot(),
	})
}

// UmbilicalCheckpointHealthDTO is the GET /api/village/umbilical/checkpoint-health
// response: the durable-checkpoint health snapshot plus the contract version.
type UmbilicalCheckpointHealthDTO struct {
	ContractVersion int                          `json:"contract_version"`
	Health          sim.CheckpointHealthSnapshot `json:"health"`
}

// handleUmbilicalCheckpointHealth serves the durable-checkpoint health view.
// Read-only, like the other umbilical read routes. s.checkpointHealth may be
// nil if the recorder wasn't wired (Snapshot is nil-safe and returns the zero
// value), so the route never panics.
func (s *Server) handleUmbilicalCheckpointHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, UmbilicalCheckpointHealthDTO{
		ContractVersion: ContractVersion,
		Health:          s.checkpointHealth.Snapshot(),
	})
}

// umbilicalBasePath is the umbilical route prefix. The manifest lives at exactly
// this path; every other umbilical route hangs off it (basePath + "/telemetry"
// etc.).
const umbilicalBasePath = "/api/village/umbilical"

// umbilicalRoute describes one umbilical route. It is the single source of truth
// for the surface: Server.Handler iterates the table to register handlers, and
// handleUmbilicalManifest renders the same table — so a route cannot be added
// without it appearing in the manifest, and the manifest cannot claim a route
// that isn't registered. (This is deliberately unlike the old hand-written help
// blobs — e.g. the WarrantKind const list — which silently drifted.)
type umbilicalRoute struct {
	method  string
	path    string
	summary string
	control bool // true = world-mutating; armed only when controlEnabled
	handler http.HandlerFunc
}

// umbilicalRoutes returns the umbilical route table. The handler fields are bound
// method values on s, so this must be called on the live Server. Order here is
// the order routes register and the order the manifest lists them: the manifest
// itself first, then the read surface, then the control whitelist.
func (s *Server) umbilicalRoutes() []umbilicalRoute {
	return []umbilicalRoute{
		{http.MethodGet, umbilicalBasePath, "Self-describing manifest of the currently-armed umbilical routes (this endpoint).", false, s.handleUmbilicalManifest},

		// Read surface — always armed when the umbilical is enabled.
		{http.MethodGet, umbilicalBasePath + "/telemetry", "Dump the tick-telemetry ring buffer (redacted per-tick lifecycle records, oldest first) with retention accounting.", false, s.handleUmbilicalTelemetry},
		{http.MethodGet, umbilicalBasePath + "/telemetry/summary", "Rolled-up telemetry rates: counts by kind / terminal status / LLM error class, plus mean and p95 tick duration.", false, s.handleUmbilicalTelemetrySummary},
		{http.MethodGet, umbilicalBasePath + "/state", "Coarse engine introspection: phase/tick, in-flight tick count, and per-table entity counts off the published snapshot.", false, s.handleUmbilicalState},
		{http.MethodGet, umbilicalBasePath + "/actions", "Tail of the committed action log (behavioral trail). Query params: actor, limit.", false, s.handleUmbilicalActions},
		{http.MethodGet, umbilicalBasePath + "/agent", "One actor's full live picture: needs, position, inventory, rest windows, reactor/warrant state, in-flight move target, recent ticks and actions. Query param: id (required).", false, s.handleUmbilicalAgent},
		{http.MethodGet, umbilicalBasePath + "/agent/prompts", "One actor's recent RENDERED DELIBERATION PROMPTS (what it actually perceived per tick), raw text, oldest first. Query params: id (required), limit (optional, default all retained). Empty when prompt capture is off.", false, s.handleUmbilicalAgentPrompts},
		{http.MethodGet, umbilicalBasePath + "/chat", "One scene's engine<->model exchange: the rendered perception (tx) and the model's responses + tool calls (rx) for that scene_id, oldest first. Query params: scene (required), limit (optional, default all retained). Empty when chat capture is off.", false, s.handleUmbilicalChat},
		{http.MethodGet, umbilicalBasePath + "/reactor", "Tick-eligibility across all actors: warranted / due-now / in-flight / idle counts plus the queued-actor list.", false, s.handleUmbilicalReactor},
		{http.MethodGet, umbilicalBasePath + "/ticker-health", "Per-interval-goroutine liveness: last-fire time and cumulative fire count for each cadence driver.", false, s.handleUmbilicalTickerHealth},
		{http.MethodGet, umbilicalBasePath + "/world-command-health", "Liveness of the single world command goroutine every ticker feeds, MEASURED by a periodic no-op probe rather than inferred from silence: round-trip times, consecutive missed deadlines, and which half of the round-trip expired (queue saturated vs loop wedged). A non-zero consecutive_timeouts means the world is not processing commands; a slowest_round_trip_ms creeping toward the deadline means it is about to stop.", false, s.handleUmbilicalWorldCommandHealth},
		{http.MethodGet, umbilicalBasePath + "/checkpoint-health", "Durable-checkpoint health: last success/failure/attempt times, consecutive-failure streak, totals, and last error. A non-zero consecutive_failures or a stale last_success_at means durability is broken.", false, s.handleUmbilicalCheckpointHealth},
		{http.MethodGet, umbilicalBasePath + "/alarms", "Critical engine-health alarms currently firing: durability broken (the checkpointer has failed N times running), the checkpoint had to clamp impossible values, cadence drivers have stopped beating, or the world command loop is not processing commands. Empty when healthy. The SAME alarms are stamped onto every umbilical response (top-level ALARMS key + X-Umbilical-Alarms header), so you do not need to poll this — it exists for the client's operator ticker.", false, s.handleUmbilicalAlarms},
		{http.MethodGet, umbilicalBasePath + "/errors", "Recent non-2xx responses the engine returned (server-observed) for remote visibility into client-facing failures.", false, s.handleUmbilicalErrors},
		{http.MethodGet, umbilicalBasePath + "/client-errors", "Client-reported (untrusted) runtime-error feed beaconed by the Godot client.", false, s.handleUmbilicalClientErrors},
		{http.MethodGet, umbilicalBasePath + "/deadlocks", "Recent locomotion soft-block deadlock hard-stops (mover + occupant + whether re-plan found no detour) for remote visibility into live freeze frequency.", false, s.handleUmbilicalDeadlocks},
		{http.MethodGet, umbilicalBasePath + "/actors", "Full actor roster with live needs (who's starving/exhausted) — the companion read for picking set-needs targets.", false, s.handleUmbilicalActors},
		{http.MethodGet, umbilicalBasePath + "/structures", "Establishment roster off the published snapshot: per structure its keeper(s) (VA, schedule, on-shift, state) and room tally (common/private/staff + private-occupied). Query param: scope=keepered (default) | all.", false, s.handleUmbilicalStructures},
		{http.MethodGet, umbilicalBasePath + "/objects", "Placed village objects off the published snapshot (read side of the object/* control routes): per object its asset, position (world-pixel + tile), display-name, state, owner, entry-policy, loiter-offset, tags, refresh-policy (including each row's live gatherable stock), attached-to, and whether it backs a structure. ALSO the mutable runtime counters: `wear` (accrued maintenance demand on an owned business), `rate_owed` (unpaid town-rate arrears), and `equipment_use` (LLM-648 deep-maintenance demand, reset by the wright's service), all omitted at zero, and both of which /settings reports the thresholds for but nothing else reads live; plus `hearth_lit_until` (when a hearth's fire burns out), omitted only when the fire has NEVER been lit — a timestamp in the past is kept, and is how a dead fire is told apart from a hearth nobody has ever stoked. Raw numbers only — no derived degraded/needs-repair flag; owner_actor_id + tags + the /settings thresholds are all the terms those judgments take. Query filters (all optional): id, owner, tag, structure — `?tag=business` narrows to the rateable/wearable businesses.", false, s.handleUmbilicalObjects},
		{http.MethodGet, umbilicalBasePath + "/pay-ledger", "Live pay-ledger off the published snapshot (most-recent first): per-entry buyer/seller/CONSUMER split, item, qty, coins offered, consume_now, state, timestamps. Reads in-memory state the checkpointed DB lags. Query param: limit (optional).", false, s.handleUmbilicalPayLedger},
		{http.MethodGet, umbilicalBasePath + "/labor-ledger", "Live labor ledger off the published snapshot (most-recent first): per-offer labor_id, worker + employer (id AND name), state (pending/en_route/working/terminal), reward (coins + in-kind goods), duration, huddle/scene ids, and lifecycle timestamps (created/expires/accepted/work-started/working-until/en-route-deadline/resolved). An in-progress hire settles only at completion, so it shows up in no durable view — this is the only live window on it. Query param: limit (optional).", false, s.handleUmbilicalLaborLedger},
		{http.MethodGet, umbilicalBasePath + "/sell-through", "Per-(seller, item) recent sell-through off the published snapshot's price book — the demand + weekly-P&L signal the reseller restock cue reasons against: units sold, sale count, sales coins, buy cost (restocking spend), distinct buyers over a trailing window, highest-throughput first. Query params: actor (filter to one seller), item, window_hours (default 168).", false, s.handleUmbilicalSellThrough},
		{http.MethodGet, umbilicalBasePath + "/turns", "Raw LLM turn(s) for an NPC straight off memory-api: the composed system_prompt, the perception sent, the model's response, token counts, cost, and provider status/error. Proxied with the operator's own token (the full turn lives only in memory-api, never in the engine). Query params: scene, agent, conversation (a hud-<hex> huddle id), since, status, limit (all optional).", false, s.handleUmbilicalTurns},
		{http.MethodGet, umbilicalBasePath + "/huddles", "List ACTIVE huddles (live conversations): members with per-member recent-utterance counts, structure, started/last-activity times — spot a stuck or one-sided huddle at a glance. In-memory, so only the currently-running engine's huddles.", false, s.handleUmbilicalHuddles},
		{http.MethodGet, umbilicalBasePath + "/npc-routes", "Live NPC ROUTE state (town crier tour / washerwoman run / constable rounds): per route its label, phase, install gen, stop cursor + full stop list, whether he has reached the current stop, whether he is dwelling, and whether a dwell timer is present. `dwell_timer_present` is the one to read for a stalled carrier — `dwelling` alone cannot tell 'parked on a dwell' from 'parked with nothing left to move him' (LLM-537); it reports pointer PRESENCE, so false is the trustworthy direction (no dwell scheduled), while true can briefly outlive a fired timer. Two arrival fields on purpose: `arrived_at_current_stop` is the route's own strict dispatch condition (exact tile), `reached_current_stop_on_foot` the tolerant co-location test — a carrier who walked himself to the stop reads true/false respectively, and that gap has been a real bug shape (LLM-530, LLM-531). /agent reports the current WALK LEG; this reports the route driving it. In-memory and never persisted, so only the currently-running engine's routes. Query param: actor (optional, filters to one carrier; unknown actor yields an empty list).", false, s.handleUmbilicalNPCRoutes},
		{http.MethodGet, umbilicalBasePath + "/summon-errands", "Live summon errands (state machine + per-state timestamps) PLUS a bounded ring of recently finished ones with outcomes (met / expired / refused / …) — the diagnosis lens for 'the keeper agreed and no one came' (LLM-414). In-memory, so only the currently-running engine's errands.", false, s.handleUmbilicalSummonErrands},
		{http.MethodGet, umbilicalBasePath + "/huddle", "One huddle's detail: members (with per-member recent-utterance counts) + the recent-conversation ring (oldest first). conversation_id pivots to /turns?conversation= for the full raw turns. Query param: id (required). In-memory: a just-concluded huddle is still fetchable by id while retained (until the next boot clears it); for a durable past-huddle lookup use /turns?conversation= (raw LLM turns) or /transcript?huddle= (committed both-speaker transcript).", false, s.handleUmbilicalHuddle},
		{http.MethodGet, umbilicalBasePath + "/transcript", "Complete durable committed-action transcript of one huddle (every participant — agent, player, engine — oldest-first) read from agent_action_log: the durable companion to the retention-bounded /huddle ring. Query param: huddle (required).", false, s.handleUmbilicalTranscript},
		{http.MethodGet, umbilicalBasePath + "/settlements", "Durable accepted pay-with-item settlements off the agent_action_log 'paid' beat, most-recent first — the audit lens for 'did a free-food settlement happen' (each row carries coins + barter goods + a `free` flag, so a give-away is unambiguous). Reaches settlements that happened outside a huddle, unlike /transcript. Optional query params: actor (buyer id — a RESIDENT's actor uuid; a transient visitor's rows carry actor_id NULL, so find a traveler's purchases by buyer_name and expect an empty buyer_id on them), since, until (RFC3339), ledger (a ledger id), limit. Rows from before LLM-105 carry has_legacy=true (no goods leg recorded).", false, s.handleUmbilicalSettlements},
		{http.MethodGet, umbilicalBasePath + "/recipes", "Live item-recipe catalog (read side of recipe/set): per recipe its output batch, production rate, inputs, optional boost_inputs, and wholesale/retail price. Query param: item (filter to one, case-insensitive against the canonical catalog key).", false, s.handleUmbilicalRecipes},
		{http.MethodGet, umbilicalBasePath + "/items", "Live item catalog (read side of item/set-satisfies): per item kind its label, category, capabilities, eat-here-only flag, and per-need satiation entries (immediate amount + dwell triple where authored). Production rate/inputs/price live on /recipes. Query param: item (filter to one, case-insensitive against the canonical catalog key).", false, s.handleUmbilicalItems},
		{http.MethodGet, umbilicalBasePath + "/narration", "Live NARRATION PHRASE REGISTRY — every pool the engine speaks from directly (businessowner greet/handover/farewell per flavour, the lodging day-cycle pools, the NPC retire farewell, the establishment closing call), with the compile-time `seed` lines and the LLM-generated `extra` lines kept APART. That split is the point: 'is this line ours or the model's' is otherwise unanswerable off the box, and a drifted pool had to be diagnosed by inference (LLM-535, where a reserved farewell expanded from a leave-taking into 'Off with you, now.'). `description` is the meta prose the expansion prompt is built from — the wording that produced the lines beside it. Also carries the draw/cap state: `draws` (TRANSIENT — the counters are not checkpointed and the village restarts often, so a low count means a recent restart, not silence), `expands_at_draws` (the next nudge threshold, ABSENT when the pool will never expand again because it is at cap), `in_flight`, `at_cap`. The merged pool lives only in memory, so this is the ONLY read of what the engine is actually drawing from — the narration_pool_expansion table shows persisted rows, not the merge. Query param: key (optional, filters to one pool, case-insensitive against the registry key; unknown key yields an empty list).", false, s.handleUmbilicalNarration},
		{http.MethodGet, umbilicalBasePath + "/settings", "Live world settings. `all` is EVERY setting key the engine loads, as raw stored-string values, with per-key write metadata in `all_meta` (value kind, whether a write takes effect immediately / on_restart / is unaudited, whether it persists) — the complete read side, and the map to drive settings/set from. The named fields alongside it are unchanged and report EFFECTIVE values for the knobs whose consumers clamp them (a stored 0 resolving to a default, a return-max clamping up to its companion min); where a key appears in both and differs, that difference is the clamp.", false, s.handleUmbilicalSettings},

		// Control whitelist — world-mutating; armed only when control is also enabled.
		{http.MethodPost, umbilicalBasePath + "/nudge", "Force a reactor tick for one actor, optionally injecting an in-world felt-impulse directive. Body: {actor_id, message?}.", true, s.handleUmbilicalNudge},
		{http.MethodPost, umbilicalBasePath + "/phase", "Force a day/night phase transition. Body: {phase}.", true, s.handleUmbilicalPhase},
		{http.MethodPost, umbilicalBasePath + "/weather", "Force the world weather to storm or clear on demand — ungated by PC presence, so it works on an empty village for demo/testing (LLM-117). Body: {weather}.", true, s.handleUmbilicalWeather},
		{http.MethodPost, umbilicalBasePath + "/settle", "Clear one actor's pending warrant cycle (stop a spiraling NPC). Body: {actor_id}.", true, s.handleUmbilicalSettle},
		{http.MethodPost, umbilicalBasePath + "/rotate", "Force a daily-rotation pass. Body: {tag?}.", true, s.handleUmbilicalRotate},
		{http.MethodPost, umbilicalBasePath + "/settings/set", "Set ANY world setting by key (LLM-577) — the generic write that covers every key GET /settings reports under `all`, including the ones no bespoke route ever got: the whole visitor_* family, world_dawn_time / world_dusk_time, the cold and hearth rates. value is a string in the setting table's own encoding (\"570\", \"true\", \"19:00\"; a duration is a scalar whose unit comes from the key suffix). Persisted on the next checkpoint. The response's takes_effect says whether the running village picks the change up now (immediately), only after a restart (on_restart), or whether that has not been audited for this key (unaudited). Body: {key, value}.", true, s.handleUmbilicalSettingSet},
		{http.MethodPost, umbilicalBasePath + "/npc/set-schedule", "Set an NPC's work-shift window on the running engine (LLM-577) — the operator-gated sibling of /admin/npc/set-schedule, which requires an in-world admin actor and so is unreachable for operators. Bounds are minute-of-day in the WORLD timezone, not UTC. Both omitted clears the window (inherit world dawn/dusk); supplying one without the other is rejected. Persisted on the next checkpoint, so no engine restart. Body: {npc_id, schedule_start_minute, schedule_end_minute}.", true, s.handleUmbilicalNPCSetSchedule},
		{http.MethodPost, umbilicalBasePath + "/settings/need-threshold", "Live-tune one need's red-line threshold. PERSISTED across restart as of LLM-577 (the registry carries the *_red_threshold keys into the checkpoint) — it no longer resets. Body: {need, value}.", true, s.handleUmbilicalNeedThreshold},
		{http.MethodPost, umbilicalBasePath + "/settings/huddle-loop", "Live-tune the huddle conversational-loop sweep (LLM-159): master enable + thresholds, PERSISTED across restart. All fields optional, at least one required. Body: {timeout_seconds? (0 disables the sweep), repeat_percent? (1-100), cadence_seconds?}.", true, s.handleUmbilicalHuddleLoop},
		{http.MethodPost, umbilicalBasePath + "/settings/seek-work-ceiling", "Live-tune the seek-work coin ceiling (LLM-194): a workless worker stops seeking/soliciting work at/above this coin balance and drains its purse via ordinary consumption. PERSISTED across restart. Body: {coin_ceiling (>=1)}.", true, s.handleUmbilicalSeekWorkCeiling},
		{http.MethodPost, umbilicalBasePath + "/settings/seek-work-need-margin", "Live-tune the seek-work→eat redirect margin (LLM-276): a workless idle worker whose hunger/thirst sits within this many points below its red-line, and who can resolve it now (carries food, holds coin, or a free source is nearby), is woken to eat/drink instead of to seek work. PERSISTED across restart. Body: {margin (>=1)}.", true, s.handleUmbilicalSeekWorkNeedMargin},
		{http.MethodPost, umbilicalBasePath + "/settings/labor-produce-boost", "Live-tune the per-worker produce boost (LLM-224): each worker laboring at their employer's establishment adds this percent of the keeper's base rate to the produce tick, so a wage buys real output. PERSISTED across restart. Body: {boost_pct (>=0; 0 disables)}.", true, s.handleUmbilicalLaborBoost},
		{http.MethodPost, umbilicalBasePath + "/settings/merchant-coin-floor", "Live-tune the merchant working-capital floor (LLM-294): a keeper whose purse dips below this AND is sitting on unsold sellable stock is steered to conserve coin (hold off buying, sell down its shelves) instead of restocking. PERSISTED across restart. Body: {coin_floor (>=0; 0 disables)}.", true, s.handleUmbilicalMerchantCoinFloor},
		{http.MethodPost, umbilicalBasePath + "/settings/eco-mode", "Live-tune eco mode (LLM-313): while no player character has a fresh presence stamp, the reactor paces social-bucket warrant cycles (npc_spoke, huddle beats) by social_gap_seconds and economy-bucket cycles (restock, production choice, farm upkeep, stall repair, seek work) by economy_gap_seconds; survival/duty/commerce warrants are never slowed. The plain idle backstop also pauses while unwatched (visitor spawning does not — the visitor cascade runs even unwatched, LLM-502). PERSISTED across restart. All fields optional, at least one required. Body: {enabled?, social_gap_seconds?, economy_gap_seconds?, audience_idle_seconds?} — gaps >= 0 (0 disables that throttle) and strictly below the warrant stale horizon (MaxWarrantAge, default 90s) so a parked cycle always comes due before it can age out. audience_idle_seconds > 0 (LLM-466) is how long a connected client may go with NO player input before it stops counting as an audience and the candle prompt asks; a click on the prompt (POST /api/village/pc/attend) restores it. Shorten it to verify eco engagement live without waiting out the 1h default.", true, s.handleUmbilicalEcoMode},
		{http.MethodPost, umbilicalBasePath + "/grant", "Give or claw back coins/items to/from any actor. Body: {actor_id, coins?, items?}.", true, s.handleUmbilicalGrant},
		{http.MethodPost, umbilicalBasePath + "/set-needs", "Set an actor's needs to ABSOLUTE values [0..24]. Body: {actor_id} or {all:true}, plus {needs:{\"hunger\":20,\"tiredness\":0}} (unlisted needs untouched). Omit needs to set every need to 0 (back-to-0 shortcut). Setting tiredness to 0 also clears the actor's rest window.", true, s.handleUmbilicalSetNeeds},
		{http.MethodPost, umbilicalBasePath + "/set-position", "Teleport an actor to a walkable TILE coordinate (the units /actors reports). Cancels any in-flight walk, reconciles inside-structure attribution, and removes the actor from a huddle it was displaced away from. Unwalkable/out-of-bounds targets are refused. Body: {actor_id, x, y}.", true, s.handleUmbilicalSetPosition},
		{http.MethodPost, umbilicalBasePath + "/route", "Force an NPC route (town_crier / washerwoman / constable) to dispatch NOW, bypassing the schedule-window (crier/washerwoman) or interval (constable) gate — reproduce a crier tour or a constable rounds tour on demand instead of waiting for a boundary or restart. Does NOT consume the real schedule boundary / rounds stamp. Body: {attr, start?}.", true, s.handleUmbilicalRoute},
		{http.MethodPost, umbilicalBasePath + "/worker/provision", "Mint a sprite-only DECORATIVE into a live Worker: assign a backing VA (default salem-vendor), grant the `worker` attribute, and reclassify its Kind in memory so it comes online and takes solicit_work jobs WITHOUT a restart (the checkpoint persists the link+attribute for the next reload). Refuses an already-live NPC (409). An unscheduled worker is then day-active on the world dawn/dusk window automatically (LLM-137). Body: {actor_id, agent?}.", true, s.handleUmbilicalProvisionWorker},
		{http.MethodPost, umbilicalBasePath + "/worker/retire", "Retire an actor from Worker duty (inverse of worker/provision): remove the `worker` attribute so the seek-work backstop + solicit_work no longer engage it — live, no restart (removing an attribute doesn't change Kind, so no reclassify and no race). With {to_decorative:true} it ALSO unlinks the VA, reclassifies to decorative, and resets reactor state so the actor goes fully inert (zero LLM cost; re-provision to bring it back). Body: {actor_id, to_decorative?}.", true, s.handleUmbilicalRetireWorker},

		// Object lifecycle (LLM-61) — live add/edit/remove of placed village objects.
		// The operator-gated counterparts to /admin/object/* (same sim Commands,
		// without the in-world admin-actor gate operators can't pass).
		{http.MethodPost, umbilicalBasePath + "/object/create", "Place a new village object (operator live-ops). World-pixel position. Body: {asset_id, x, y, attached_to?}.", true, s.handleUmbilicalObjectCreate},
		{http.MethodPost, umbilicalBasePath + "/object/move", "Reposition a placed village object to a new world-pixel anchor. Body: {object_id, x, y}.", true, s.handleUmbilicalObjectMove},
		{http.MethodPost, umbilicalBasePath + "/object/delete", "Remove a placed village object and its attached overlays (refused for a structure-backed object). Body: {object_id}.", true, s.handleUmbilicalObjectDelete},
		{http.MethodPost, umbilicalBasePath + "/object/set-display-name", "Set or clear a placed object's display-name override (e.g. name a nameless gather/eat source live). Body: {object_id, display_name}.", true, s.handleUmbilicalObjectSetDisplayName},
		{http.MethodPost, umbilicalBasePath + "/object/set-state", "Set a placed object's current_state (free-form catalog state; unknown renders as the asset fallback). Body: {object_id, state}.", true, s.handleUmbilicalObjectSetState},
		{http.MethodPost, umbilicalBasePath + "/object/set-owner", "Set or clear a placed object's owning actor (empty owner_actor_id clears). Body: {object_id, owner_actor_id}.", true, s.handleUmbilicalObjectSetOwner},
		{http.MethodPost, umbilicalBasePath + "/object/set-loiter-offset", "Set or clear a placed object's loiter offset (both x,y or neither). Body: {object_id, x?, y?}.", true, s.handleUmbilicalObjectSetLoiterOffset},
		{http.MethodPost, umbilicalBasePath + "/object/set-entry-policy", "Set a placed object's entry policy (\"\", open, owner-only, closed). Body: {object_id, entry_policy}.", true, s.handleUmbilicalObjectSetEntryPolicy},
		{http.MethodPost, umbilicalBasePath + "/object/add-tag", "Add a per-instance tag to a placed object (idempotent). Body: {object_id, tag}.", true, s.handleUmbilicalObjectAddTag},
		{http.MethodPost, umbilicalBasePath + "/object/remove-tag", "Remove a per-instance tag from a placed object (idempotent). Body: {object_id, tag}.", true, s.handleUmbilicalObjectRemoveTag},
		{http.MethodPost, umbilicalBasePath + "/object/set-refresh", "Replace a placed object's refresh-policy set wholesale (empty rows clears; the partner to set-display-name for fixing a gather/eat source live). Body: {object_id, rows}.", true, s.handleUmbilicalObjectSetRefresh},

		// Restock policy (LLM-95) — live per-entry edit of what an NPC
		// produces / restocks / forages at work. Mutates the actor's attribute
		// params and re-projects RestockPolicy; durability rides the attribute
		// checkpoint.
		{http.MethodPost, umbilicalBasePath + "/restock/set", "Add or update one restock entry on an actor (produce/buy/forage). Validates the item exists; a produce entry requires a recipe. Body: {actor_id, item, source, cap}.", true, s.handleUmbilicalRestockSet},
		{http.MethodPost, umbilicalBasePath + "/restock/remove", "Remove one restock entry (by item) from an actor. Body: {actor_id, item}.", true, s.handleUmbilicalRestockRemove},

		// Stall wear (LLM-118) — live-tune the market-stall wear/repair knobs
		// without a restart; applied in memory and persisted on the next checkpoint.
		{http.MethodPost, umbilicalBasePath + "/stall-wear/set", "Live-tune the stall wear knobs (LLM-118) without a restart. All fields optional (at least one required), non-negative ints; stall_degraded_produce_pct (LLM-446) is 0-100 — the production rate while the owner's business is degraded (0 = full block, 100 = no penalty). Body: {stall_wear_per_coin?, stall_wear_repair_threshold?, stall_wear_degrade_threshold?, stall_nails_per_repair?, stall_repair_duration_seconds?, stall_degraded_produce_pct?}.", true, s.handleUmbilicalStallWearSet},

		// Farm upkeep wealth tax (LLM-215) — live-tune the per-farm shovel levy
		// without a restart; applied in memory and persisted on the next checkpoint.
		{http.MethodPost, umbilicalBasePath + "/farm-upkeep/set", "Live-tune the farm wealth-tax knobs (LLM-215) without a restart. Both fields optional (at least one required), non-negative ints; farm_upkeep_coins_per_shovel=0 disables the feature. Body: {farm_upkeep_floor?, farm_upkeep_coins_per_shovel?}.", true, s.handleUmbilicalFarmUpkeepSet},
		{http.MethodPost, umbilicalBasePath + "/town-rate/set", "Live-tune the constable's town rate (LLM-557) without a restart. Both fields optional (at least one required), non-negative ints; town_rate_coins_per_day=0 stops further accrual (the off-switch) WITHOUT forgiving outstanding arrears, town_rate_max_owed=0 means uncapped. Body: {town_rate_coins_per_day?, town_rate_max_owed?}.", true, s.handleUmbilicalTownRateSet},

		// Constable rounds (LLM-514) — live-tune how often the constable walks his
		// rounds and how long he pauses at each business; applied in memory and
		// persisted on the next checkpoint.
		{http.MethodPost, umbilicalBasePath + "/constable-rounds/set", "Live-tune the constable rounds cadence (LLM-514) without a restart. All fields optional (at least one required), non-negative seconds; interval_seconds=0 disables rounds, dwell_seconds=0 resolves to the 45s default, quiet_seconds=0 resolves to the 90s default. quiet_seconds (LLM-537) is how long the stop's conversation must have gone SILENT before he walks on — a liveness window over the huddle's last activity, not its 2h lifecycle, which used to park him at the stop for as long as the huddle stayed open. Body: {interval_seconds?, dwell_seconds?, quiet_seconds?}.", true, s.handleUmbilicalConstableRoundsSet},

		// Item definition (LLM-200) — live create/edit of one item_kind row (the
		// definition: label, category, sort order, capabilities, counting nouns,
		// dwell narration). The create leg of the all-live new-good flow
		// (item/set → recipe/set → restock/set), so unlike the other two it can
		// introduce a brand-new good. Durable write to item_kind + in-memory
		// catalog update; needs SetItemKindWriter wired (else 503).
		{http.MethodPost, umbilicalBasePath + "/item/set", "Add or update one item-kind definition (upsert keyed on name; creates a brand-new good or edits an existing one, leaving its satiation rows intact). Body: {name, display_label, category, sort_order, capabilities:[...], display_label_singular, display_label_plural, consume_dwell_narration}.", true, s.handleUmbilicalItemSet},

		// Recipe (LLM-97) — live edit/add of an item recipe (existing items
		// only). Durable write to item_recipe + in-memory catalog update; needs
		// SetRecipeWriter wired (else 503).
		{http.MethodPost, umbilicalBasePath + "/recipe/set", "Add or update one item recipe (upsert; output + input items must already exist). Body: {output_item, output_qty, rate_qty, rate_per_hours, inputs:[{item,qty}], boost_inputs:[{item,qty,bonus_qty}] (optional per-execution yield boosters, LLM-248), wholesale_price, retail_price}.", true, s.handleUmbilicalRecipeSet},

		// Item satiation (LLM-119) — live edit of how much consuming one unit of
		// an item eases a need (the item_satisfies immediate amount). Durable
		// write to item_satisfies + in-memory catalog update; needs
		// SetSatisfiesWriter wired (else 503).
		{http.MethodPost, umbilicalBasePath + "/item/set-satisfies", "Add or update one item's immediate per-unit need-ease magnitude (upsert keyed item+attribute; the item must already exist and the attribute must be a tracked need). Edits preserve any authored dwell triple. Body: {item, attribute, amount}.", true, s.handleUmbilicalSetSatisfies},
	}
}

// UmbilicalRouteDTO is one route on the manifest wire.
type UmbilicalRouteDTO struct {
	Path    string `json:"path"`
	Method  string `json:"method"`
	Summary string `json:"summary"`
	Control bool   `json:"control"`
}

// UmbilicalManifestDTO is the GET /api/village/umbilical response: the in-band,
// runtime-aware description of the surface. The thing a static codebase note
// can't report is exactly what this carries — whether control is armed on THIS
// deploy and which routes are therefore actually live. `enabled` is always true
// in a served response (the route only registers when the umbilical is on; when
// off the operator gets a 404, which is itself the answer).
type UmbilicalManifestDTO struct {
	ContractVersion int                 `json:"contract_version"`
	Enabled         bool                `json:"enabled"`
	ControlEnabled  bool                `json:"control_enabled"`
	Routes          []UmbilicalRouteDTO `json:"routes"`
}

// handleUmbilicalManifest renders the route table, filtered to what is actually
// armed right now: read routes always (the umbilical is on or this handler
// wouldn't be registered), control routes only when controlEnabled — the same
// filter Server.Handler applies at registration, so the manifest matches the
// live mux exactly.
func (s *Server) handleUmbilicalManifest(w http.ResponseWriter, _ *http.Request) {
	routes := s.umbilicalRoutes()
	out := UmbilicalManifestDTO{
		ContractVersion: ContractVersion,
		Enabled:         true,
		ControlEnabled:  s.controlEnabled,
		Routes:          make([]UmbilicalRouteDTO, 0, len(routes)),
	}
	for _, rt := range routes {
		if rt.control && !s.controlEnabled {
			continue
		}
		out.Routes = append(out.Routes, UmbilicalRouteDTO{
			Path:    rt.path,
			Method:  rt.method,
			Summary: rt.summary,
			Control: rt.control,
		})
	}
	writeJSON(w, out)
}
