package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// umbilical_views.go — the richer operator read views, built on top of the
// core read routes (telemetry / state / actions in umbilical.go). All
// operator-gated + registered with the read surface. Two of the three
// (agent, reactor) read LIVE world state via a command (SendContext): they
// surface the reactor-evaluator fields (WarrantedSince/WarrantDueAt/Warrants)
// and the rest windows (BreakUntil/SleepingUntil) that are deliberately NOT on
// the published snapshot. The command closure extracts plain values into a DTO
// on the world goroutine and returns them — it never hands an *Actor pointer
// back to the HTTP goroutine, so there's no shared-state race. These are pure
// reads: they mutate nothing.

// ---- Telemetry summary (ring rollup) ------------------------------------

// TelemetrySummaryDTO rolls the raw tick-telemetry ring up into rates, so an
// operator can answer "is the failure rate climbing / are ticks slowing down"
// without scrolling the event log. Computed over the ring snapshot — no new
// plumbing.
type TelemetrySummaryDTO struct {
	ContractVersion  int               `json:"contract_version"`
	Stats            TelemetryStatsDTO `json:"stats"`
	ByKind           map[string]int    `json:"by_kind"`            // deferred/started/completed/failed/stale
	ByTerminalStatus map[string]int    `json:"by_terminal_status"` // success/done/budget_forced/failed_*/...
	ByLLMErrorClass  map[string]int    `json:"by_llm_error_class"` // error classes seen among failures
	DurationMsMean   int               `json:"duration_ms_mean"`
	DurationMsP95    int               `json:"duration_ms_p95"`
	DurationSamples  int               `json:"duration_samples"` // records that carried a parseable duration_ms
}

// handleUmbilicalTelemetrySummary aggregates the telemetry ring.
func (s *Server) handleUmbilicalTelemetrySummary(w http.ResponseWriter, _ *http.Request) {
	recs := s.telemetry.Snapshot()
	out := TelemetrySummaryDTO{
		ContractVersion:  ContractVersion,
		Stats:            telemetryStatsDTO(s.telemetry.Stats()),
		ByKind:           map[string]int{},
		ByTerminalStatus: map[string]int{},
		ByLLMErrorClass:  map[string]int{},
	}
	durations := make([]int, 0, len(recs))
	for _, r := range recs {
		out.ByKind[r.Kind]++
		if r.Detail == nil {
			continue
		}
		if ts := r.Detail["terminal_status"]; ts != "" {
			out.ByTerminalStatus[ts]++
		}
		if ec := r.Detail["llm_error_class"]; ec != "" {
			out.ByLLMErrorClass[ec]++
		}
		if ms, err := strconv.Atoi(r.Detail["duration_ms"]); err == nil && ms >= 0 {
			durations = append(durations, ms)
		}
	}
	out.DurationSamples = len(durations)
	if len(durations) > 0 {
		sort.Ints(durations)
		sum := 0
		for _, d := range durations {
			sum += d
		}
		out.DurationMsMean = sum / len(durations)
		// p95 index, clamped to the last element.
		idx := (len(durations) * 95) / 100
		if idx >= len(durations) {
			idx = len(durations) - 1
		}
		out.DurationMsP95 = durations[idx]
	}
	writeJSON(w, out)
}

// ---- Per-agent deep view ------------------------------------------------

// UmbilicalAgentDTO is the full operator picture of one actor: identity +
// spatial + needs/inventory + the rest windows + the reactor-evaluator state,
// plus its recent tick records (from the ring) and recent actions (from the
// action log). The live fields come from a command read; the histories are
// filtered off the ring / published snapshot.
type UmbilicalAgentDTO struct {
	ContractVersion int    `json:"contract_version"`
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role,omitempty"`
	Kind            string `json:"kind"`
	LLMAgent        string `json:"llm_memory_agent,omitempty"`
	LoginUsername   string `json:"login_username,omitempty"`
	IsAdmin         bool   `json:"is_admin"`

	// Attributes is the actor's role-marker slug set (sorted): worker, messenger,
	// town_crier, keeper/businessowner, etc. — the same set the worker routes and
	// the npc_attributes_changed frame emit (shared helper, so they can't drift).
	// Presence-only markers; the params values are unused, so slugs suffice. Not
	// omitempty — always emitted, as `[]` when the actor carries none, so the
	// operator can distinguish "no markers" from "field absent" (LLM-421).
	Attributes []string `json:"attributes"`

	TileX             int    `json:"tile_x"`
	TileY             int    `json:"tile_y"`
	State             string `json:"state"`
	InsideStructureID string `json:"inside_structure_id,omitempty"`
	InsideRoomID      int64  `json:"inside_room_id,omitempty"`
	CurrentHuddleID   string `json:"current_huddle_id,omitempty"`
	HomeStructureID   string `json:"home_structure_id,omitempty"`
	WorkStructureID   string `json:"work_structure_id,omitempty"`

	Needs     map[string]int `json:"needs,omitempty"`
	Inventory map[string]int `json:"inventory,omitempty"`
	Coins     int            `json:"coins"`

	// SpendBudget is a visitor's remaining trip spend budget (LLM-644) —
	// what of the coins above he may still put into purchases; the rest is
	// takings bound for home. Omitted for residents (no cap). Pointer so an
	// exhausted budget (0) still renders, distinct from absence.
	SpendBudget *int `json:"spend_budget,omitempty"`

	// What the actor produces / restocks at work — the read counterpart to the
	// restock/set control route (LLM-111). RestockPolicy lists the managed items
	// + supply mode (produce/buy/forage) + personal-carry cap; it reuses the
	// control route's umbilicalRestockEntry wire type so the read and write sides
	// can't drift. Production is the in-flight one-shot batch (LLM-319), nil when
	// nothing is in the works. Both empty when the actor manages nothing.
	RestockPolicy []umbilicalRestockEntry `json:"restock_policy,omitempty"`
	Production    *ProductionActivityDTO  `json:"production,omitempty"`

	// Rest windows (nil = not resting).
	BreakUntil    *time.Time `json:"break_until,omitempty"`
	SleepingUntil *time.Time `json:"sleeping_until,omitempty"`

	// Source activity — an in-flight timed eat/drink/harvest at a village object
	// (LLM-54; nil = not engaged). Like the rest windows, deliberately not on the
	// published snapshot — surfaced here for operator inspection only.
	SourceActivity *SourceActivityDTO `json:"source_activity,omitempty"`

	// Live labor job (LLM-272) — the worker mirror of an accepted, in-progress
	// hire. LaborID / LaboringUntil are set only while the worker is Working (the
	// EnRoute relocation leg leaves them clear per labor_ledger.go); LaborState is
	// the offer's live state read off World.LaborLedger. All omitted when the
	// actor is not on a job. The full offer (employer, reward, huddle, timestamps)
	// is on /labor-ledger — this is just the "what is this actor doing right now"
	// companion, the natural place to look when the actor reads state=laboring.
	LaborID       uint64     `json:"labor_id,omitempty"`
	LaborState    string     `json:"labor_state,omitempty"`
	LaboringUntil *time.Time `json:"laboring_until,omitempty"`

	// Tick scheduling + reactor-evaluator state — the "is this agent queued /
	// mid-tick / idle" picture that's NOT on the published snapshot.
	LastTickedAt   *time.Time `json:"last_ticked_at,omitempty"`
	WarrantedSince *time.Time `json:"warranted_since,omitempty"`
	WarrantDueAt   *time.Time `json:"warrant_due_at,omitempty"`
	WarrantCount   int        `json:"warrant_count"`
	TickInFlight   bool       `json:"tick_in_flight"`
	TickAttemptID  string     `json:"tick_attempt_id,omitempty"`

	// In-flight movement target — empty/false when the actor isn't moving. For
	// diagnosing stuck or off-grid walks: MoveTargetTile is the resolved goal
	// tile (grid-free — door tile for structure_enter, loiter pin for
	// structure_visit, exact tile for position), so an actor pathing to an
	// off-grid tile shows up directly here.
	Moving              bool   `json:"moving"`
	MoveDestKind        string `json:"move_dest_kind,omitempty"`
	MoveDestStructureID string `json:"move_dest_structure_id,omitempty"`
	MoveTargetTileX     *int   `json:"move_target_tile_x,omitempty"`
	MoveTargetTileY     *int   `json:"move_target_tile_y,omitempty"`
	MoveAttemptID       uint64 `json:"move_attempt_id,omitempty"`

	RecentTicks   []TelemetryRecordDTO `json:"recent_ticks"`
	RecentActions []ActionLogEntryDTO  `json:"recent_actions"`
}

// ProductionActivityDTO is the operator view of an actor's in-flight one-shot
// production cycle (LLM-319): what's cooking, the batch size that lands, and
// the base-rate work remaining (the labor boost shortens the real wall time).
// last_progress_at is the transient tick anchor — omitted when it hasn't been
// stamped since a restart.
type ProductionActivityDTO struct {
	Item             string     `json:"item"`
	BatchQty         int        `json:"batch_qty"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	LastProgressAt   *time.Time `json:"last_progress_at,omitempty"`
}

// errAgentNotFound is returned by the agent-view command when the id is unknown.
var errAgentNotFound = errors.New("actor not found")

// handleUmbilicalAgent serves the full operator view of one actor. Query param
// `id` (required) is the ActorID. 400 missing id, 404 unknown actor, 200 ok.
func (s *Server) handleUmbilicalAgent(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// Live read on the world goroutine: copy the actor's fields into the DTO
	// (no *Actor escapes the closure). Histories are added after, off the ring
	// and published snapshot.
	res, err := s.world.SendContext(r.Context(), sim.Command{Fn: func(world *sim.World) (any, error) {
		a, ok := world.Actors[sim.ActorID(id)]
		if !ok {
			return nil, errAgentNotFound
		}
		dto := UmbilicalAgentDTO{
			ContractVersion:   ContractVersion,
			ID:                string(a.ID),
			DisplayName:       a.DisplayName,
			Role:              a.Role,
			Kind:              actorKindString(a.Kind),
			LLMAgent:          a.LLMAgent,
			LoginUsername:     a.LoginUsername,
			IsAdmin:           a.IsAdmin,
			TileX:             a.Pos.X,
			TileY:             a.Pos.Y,
			State:             string(a.State),
			InsideStructureID: string(a.InsideStructureID),
			InsideRoomID:      int64(a.InsideRoomID),
			CurrentHuddleID:   string(a.CurrentHuddleID),
			HomeStructureID:   string(a.HomeStructureID),
			WorkStructureID:   string(a.WorkStructureID),
			Coins:             a.Coins,
			BreakUntil:        clonePtrTime(a.BreakUntil),
			SleepingUntil:     clonePtrTime(a.SleepingUntil),
			SourceActivity:    sourceActivityDTO(a.SourceActivity),
			LastTickedAt:      clonePtrTime(a.LastTickedAt),
			WarrantedSince:    clonePtrTime(a.WarrantedSince),
			WarrantDueAt:      clonePtrTime(a.WarrantDueAt),
			WarrantCount:      len(a.Warrants),
			TickInFlight:      a.TickInFlight,
			TickAttemptID:     string(a.TickAttemptID),
			Attributes:        sim.SortedAttributeSlugs(a),
		}
		if a.VisitorState != nil {
			budget := a.VisitorState.SpendBudget
			dto.SpendBudget = &budget
		}
		if len(a.Needs) > 0 {
			dto.Needs = make(map[string]int, len(a.Needs))
			for k, v := range a.Needs {
				dto.Needs[string(k)] = v
			}
		}
		if len(a.Inventory) > 0 {
			dto.Inventory = make(map[string]int, len(a.Inventory))
			for k, v := range a.Inventory {
				dto.Inventory[string(k)] = v
			}
		}
		if a.RestockPolicy != nil && len(a.RestockPolicy.Restock) > 0 {
			// Preserve policy order (first-listed wins on ties — recipe.go).
			dto.RestockPolicy = make([]umbilicalRestockEntry, 0, len(a.RestockPolicy.Restock))
			for _, e := range a.RestockPolicy.Restock {
				dto.RestockPolicy = append(dto.RestockPolicy, umbilicalRestockEntry{
					Item:   string(e.Item),
					Source: string(e.Source),
					Cap:    e.Cap(),
				})
			}
		}
		if pa := a.ProductionActivity; pa != nil {
			dto.Production = &ProductionActivityDTO{
				Item:             string(pa.Item),
				BatchQty:         pa.BatchQty,
				RemainingSeconds: pa.RemainingSeconds,
				LastProgressAt:   ptrTimeIfSet(pa.LastProgressAt),
			}
		}
		if a.MoveIntent != nil {
			dto.Moving = true
			dto.MoveDestKind = string(a.MoveIntent.Destination.Kind)
			if a.MoveIntent.Destination.StructureID != nil {
				dto.MoveDestStructureID = string(*a.MoveIntent.Destination.StructureID)
			}
			dto.MoveAttemptID = uint64(a.MoveIntent.AttemptID)
			if tgt, ok := sim.ResolveMoveTargetTile(world, a); ok {
				tx, ty := tgt.X, tgt.Y
				dto.MoveTargetTileX = &tx
				dto.MoveTargetTileY = &ty
			}
		}
		// Live labor job — the worker mirror is set only while Working; read the
		// offer's live state off the ledger for the operator (LLM-272).
		if a.LaborID != 0 {
			dto.LaborID = uint64(a.LaborID)
			dto.LaboringUntil = clonePtrTime(a.LaboringUntil)
			if o := world.LaborLedger[a.LaborID]; o != nil {
				dto.LaborState = string(o.State)
			}
		}
		return dto, nil
	}})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errAgentNotFound) {
			writeError(w, http.StatusNotFound, "actor not found")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	dto, ok := res.(UmbilicalAgentDTO)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected agent result")
		return
	}

	// Recent tick records for this actor (filter the ring, chronological).
	dto.RecentTicks = make([]TelemetryRecordDTO, 0)
	for _, rec := range s.telemetry.Snapshot() {
		if string(rec.ActorID) != id {
			continue
		}
		dto.RecentTicks = append(dto.RecentTicks, TelemetryRecordDTO{
			At:        rec.At,
			ActorID:   string(rec.ActorID),
			AttemptID: string(rec.AttemptID),
			Kind:      rec.Kind,
			Detail:    rec.Detail,
		})
	}
	// Recent actions for this actor (filter the published action log).
	dto.RecentActions = make([]ActionLogEntryDTO, 0)
	if snap := s.world.Published(); snap != nil {
		for _, e := range snap.ActionLog {
			if string(e.ActorID) != id {
				continue
			}
			dto.RecentActions = append(dto.RecentActions, ActionLogEntryDTO{
				ActorID:    string(e.ActorID),
				OccurredAt: e.OccurredAt,
				ActionType: string(e.ActionType),
				Text:       e.Text,
				HuddleID:   string(e.HuddleID),
			})
		}
	}
	writeJSON(w, dto)
}

// ---- Reactor / queue view ----------------------------------------------

// UmbilicalReactorDTO summarizes the reactor's tick-eligibility state across
// all actors — the backlog the umbilical couldn't see before (only in-flight
// count was on the snapshot). Warranted = a pending signal cycle; DueNow =
// warranted and past its due time (the evaluator should be emitting it);
// InFlight = mid-LLM-tick; Idle = none of the above.
type UmbilicalReactorDTO struct {
	ContractVersion int               `json:"contract_version"`
	Now             time.Time         `json:"now"`
	TotalActors     int               `json:"total_actors"`
	Warranted       int               `json:"warranted"`
	DueNow          int               `json:"due_now"`
	InFlight        int               `json:"in_flight"`
	Idle            int               `json:"idle"`
	WarrantedActors []ReactorActorDTO `json:"warranted_actors"`
}

// ReactorActorDTO is one currently-warranted (or in-flight) actor in the
// reactor view, so an operator can see exactly who's queued and for how long.
type ReactorActorDTO struct {
	ActorID        string     `json:"actor_id"`
	WarrantedSince *time.Time `json:"warranted_since,omitempty"`
	WarrantDueAt   *time.Time `json:"warrant_due_at,omitempty"`
	WarrantCount   int        `json:"warrant_count"`
	TickInFlight   bool       `json:"tick_in_flight"`
}

// handleUmbilicalReactor serves the reactor tick-eligibility summary.
func (s *Server) handleUmbilicalReactor(w http.ResponseWriter, r *http.Request) {
	res, err := s.world.SendContext(r.Context(), sim.Command{Fn: func(world *sim.World) (any, error) {
		now := time.Now()
		dto := UmbilicalReactorDTO{
			ContractVersion: ContractVersion,
			Now:             now,
			TotalActors:     len(world.Actors),
			WarrantedActors: []ReactorActorDTO{},
		}
		for _, a := range world.Actors {
			warranted := a.WarrantedSince != nil
			switch {
			case a.TickInFlight:
				dto.InFlight++
			case warranted:
				dto.Warranted++
				if a.WarrantDueAt != nil && !a.WarrantDueAt.After(now) {
					dto.DueNow++
				}
			default:
				dto.Idle++
			}
			if warranted || a.TickInFlight {
				dto.WarrantedActors = append(dto.WarrantedActors, ReactorActorDTO{
					ActorID:        string(a.ID),
					WarrantedSince: clonePtrTime(a.WarrantedSince),
					WarrantDueAt:   clonePtrTime(a.WarrantDueAt),
					WarrantCount:   len(a.Warrants),
					TickInFlight:   a.TickInFlight,
				})
			}
		}
		// Deterministic order: soonest due first, then by id.
		sort.Slice(dto.WarrantedActors, func(i, j int) bool {
			di, dj := dto.WarrantedActors[i].WarrantDueAt, dto.WarrantedActors[j].WarrantDueAt
			switch {
			case di != nil && dj != nil && !di.Equal(*dj):
				return di.Before(*dj)
			case (di == nil) != (dj == nil):
				return di != nil // non-nil due times sort ahead of nil
			default:
				return dto.WarrantedActors[i].ActorID < dto.WarrantedActors[j].ActorID
			}
		})
		return dto, nil
	}})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	dto, ok := res.(UmbilicalReactorDTO)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected reactor result")
		return
	}
	writeJSON(w, dto)
}

// ---- Actor roster -------------------------------------------------------

// UmbilicalActorRowDTO is one actor on the roster / reset views: just enough to
// triage village health at a glance (who's starving/exhausted) and pick a
// reset/nudge/settle target. Needs are read LIVE — they are deliberately NOT on
// the published snapshot's client AgentDTO, so this is the only one-shot
// "everyone's needs" view. Shared by /actors (list) and /set-needs
// (post-mutation echo).
type UmbilicalActorRowDTO struct {
	ID          string         `json:"id"`
	DisplayName string         `json:"display_name"`
	Kind        string         `json:"kind"`
	State       string         `json:"state"`
	Needs       map[string]int `json:"needs,omitempty"`
	Coins       int            `json:"coins"`
	TileX       int            `json:"tile_x"`
	TileY       int            `json:"tile_y"`
	// Present is PC-only (omitted for every other kind): whether the player's
	// client is currently attached, i.e. the presence stamp is fresh by the same
	// PCPresenceStale gate the ghost-ejection sweep and eco mode (LLM-313) trust —
	// now driven by the WS heartbeat (LLM-342). The read side of "does the village
	// think anyone is watching."
	Present *bool `json:"present,omitempty"`
	// Attributes is the actor's sorted role-marker slug set (worker, messenger,
	// town_crier, keeper/businessowner, …) — the village-wide "who carries what"
	// scan (LLM-421). omitempty here (unlike /agent's always-emit): the roster is
	// many rows and most carry none, so the field appears only on the rows that
	// have markers, making carriers stand out. Same shared helper as /agent.
	Attributes []string `json:"attributes,omitempty"`
}

// UmbilicalActorsDTO is the GET /api/village/umbilical/actors response: the full
// actor roster (every actor, sorted by id) each with its live needs — the "who
// needs a reset" companion to the /set-needs control route.
type UmbilicalActorsDTO struct {
	ContractVersion int                    `json:"contract_version"`
	Now             time.Time              `json:"now"`
	Total           int                    `json:"total"`
	Actors          []UmbilicalActorRowDTO `json:"actors"`
}

// actorRowDTO copies one live actor's roster fields into a value DTO. Must run on
// the world goroutine (it reads an *Actor and, for a PC's presence, world
// settings); no pointer escapes the closure. A nil/empty Needs map yields an
// omitted needs field (the actor tracks no needs). For PCs the row carries
// present (fresh presence stamp per the shared PCPresenceStale gate, WS-driven
// since LLM-342); every other kind omits it.
func actorRowDTO(world *sim.World, a *sim.Actor, now time.Time) UmbilicalActorRowDTO {
	row := UmbilicalActorRowDTO{
		ID:          string(a.ID),
		DisplayName: a.DisplayName,
		Kind:        actorKindString(a.Kind),
		State:       string(a.State),
		Coins:       a.Coins,
		TileX:       a.Pos.X,
		TileY:       a.Pos.Y,
	}
	if len(a.Needs) > 0 {
		row.Needs = make(map[string]int, len(a.Needs))
		for k, v := range a.Needs {
			row.Needs[string(k)] = v
		}
	}
	if a.Kind == sim.KindPC {
		present := !sim.PCPresenceStale(a.LastPCSeenAt, now, sim.PCPresenceStaleAfter(world))
		row.Present = &present
	}
	if len(a.Attributes) > 0 {
		row.Attributes = sim.SortedAttributeSlugs(a)
	}
	return row
}

// handleUmbilicalActors serves the full actor roster with live needs. Read via a
// world command (needs aren't on the published snapshot), sorted by id for a
// stable read. Pure read — mutates nothing.
func (s *Server) handleUmbilicalActors(w http.ResponseWriter, r *http.Request) {
	res, err := s.world.SendContext(r.Context(), sim.Command{Fn: func(world *sim.World) (any, error) {
		dto := UmbilicalActorsDTO{
			ContractVersion: ContractVersion,
			Now:             time.Now().UTC(),
			Actors:          make([]UmbilicalActorRowDTO, 0, len(world.Actors)),
		}
		for _, a := range world.Actors {
			dto.Actors = append(dto.Actors, actorRowDTO(world, a, dto.Now))
		}
		dto.Total = len(dto.Actors)
		sort.Slice(dto.Actors, func(i, j int) bool { return dto.Actors[i].ID < dto.Actors[j].ID })
		return dto, nil
	}})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	dto, ok := res.(UmbilicalActorsDTO)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected actors result")
		return
	}
	writeJSON(w, dto)
}

// SourceActivityDTO is the operator-view projection of an in-flight
// Actor.SourceActivity (LLM-54): which timed action (eat/drink/harvest), at
// which object, finishing when. Plain values copied on the world goroutine.
type SourceActivityDTO struct {
	Kind     string     `json:"kind"`
	ObjectID string     `json:"object_id,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
}

// sourceActivityDTO projects a live *sim.SourceActivity into the DTO, copying
// the Until timestamp so the result never aliases live world state. nil → nil
// (not engaged), which the omitempty JSON tag drops from the payload.
func sourceActivityDTO(sa *sim.SourceActivity) *SourceActivityDTO {
	if sa == nil {
		return nil
	}
	until := sa.Until
	return &SourceActivityDTO{
		Kind:     string(sa.Kind),
		ObjectID: string(sa.ObjectID),
		Until:    &until,
	}
}

// clonePtrTime copies a *time.Time so the returned DTO never aliases a live
// Actor's pointer (the command closure runs on the world goroutine; the DTO
// crosses back to the HTTP goroutine).
func clonePtrTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// ptrTimeIfSet returns a pointer to a copy of t, or nil when t is the zero time —
// so a zero LastProducedAt renders as an omitted field rather than the Go zero
// date. The copy keeps the DTO from aliasing live world state.
func ptrTimeIfSet(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	c := t
	return &c
}
