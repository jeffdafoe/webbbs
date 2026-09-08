// config_handlers.go — the admin world-config surface (ZBBS-WORK-363), the v2
// port of v1's world-config read/write that the Godot config panel drives.
//
// Read:  GET  /api/village/config        — the panel's populate fetch.
// Write: POST /api/village/admin/zoom-settings, /agent-ticks, /force-rotate
//
//	(force-phase already lives in write_handlers.go).
//
// The writes mutate the runtime-tunable WorldSettings subset in-memory + emit a
// WS event for live client updates; durability rides the periodic checkpoint
// (CheckpointSnapshot.MutableSettings → pg.SaveWorld), the same model object
// placement uses — there is no immediate write-through to pg.
//
// The public, hot-path GET /api/village/world carries only the camera zoom
// floors (every client needs its floor); everything else admin-only lives here
// so it stays off the per-frame world poll. All routes are admin-gated the same
// way the object/npc writes are: requireAuth (valid salem session) PLUS the
// in-world Actor.IsAdmin check, resolved on the world goroutine via adminCommand
// (findAdminByLogin) — no TOCTOU, and the read sees live w.Settings with no
// snapshot-publish lag.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// handleConfig serves the admin world-config panel read. Runs the build through
// the command channel under adminCommand: a non-admin caller (or one with no
// matching actor) gets errAdminForbidden → 403, mirroring the admin write
// routes; an admin gets the live config.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		writeAuthError(w, "invalid")
		return
	}

	res, err := s.world.SendContext(r.Context(), adminCommand(user.Username, func(world *sim.World) (any, error) {
		return buildWorldConfig(world), nil
	}))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errAdminForbidden) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read world config")
		return
	}

	cfg, ok := res.(WorldConfigDTO)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected config result")
		return
	}
	writeJSON(w, cfg)
}

// buildWorldConfig assembles the admin config DTO from live world state. Called
// inside the adminCommand Fn (on the world goroutine), so it reads w.Settings /
// w.Environment directly with no race. The next-transition / next-rotation
// countdowns are best-effort: a malformed HH:MM boundary leaves those fields
// zero rather than failing the whole read.
func buildWorldConfig(world *sim.World) WorldConfigDTO {
	st := world.Settings
	env := world.Environment
	dto := WorldConfigDTO{
		Timezone:         st.Timezone,
		LastTransitionAt: env.LastTransitionAt,
		DawnTime:         st.DawnTime,
		DuskTime:         st.DuskTime,
		RotationTime:     st.RotationTime,
		LastRotationAt:   env.LastRotationAt,
		AgentTicksPaused: st.AgentTicksPaused,
		ZoomMinAdmin:     st.ZoomMinAdmin,
		ZoomMinRegular:   st.ZoomMinRegular,
		TownChestCoins:   env.TownChest,
	}

	loc := st.Location
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	// Next day↔night transition (the panel's transition countdown).
	if dawnH, dawnM, derr := sim.ParseHM(st.DawnTime); derr == nil {
		if duskH, duskM, uerr := sim.ParseHM(st.DuskTime); uerr == nil {
			phase, at := sim.NextBoundary(now, dawnH, dawnM, duskH, duskM)
			dto.NextTransitionAt = at.UTC()
			dto.NextTransitionPhase = string(phase)
		}
	}

	// Next daily asset rotation. RotationTime defaults to "00:00" via the
	// environment loader; defensive fallback mirrors checkAndRotate.
	rotSpec := st.RotationTime
	if rotSpec == "" {
		rotSpec = sim.DefaultRotationTime
	}
	if rotH, rotM, rerr := sim.ParseHM(rotSpec); rerr == nil {
		dto.NextRotationAt = sim.NextRotationBoundary(now, rotH, rotM).UTC()
	}

	return dto
}

// ---- writes ----

// adminZoomSettingsRequest is the POST /api/village/admin/zoom-settings body.
// Both floors are independently optional (the panel can save one or both);
// nil = leave that floor unchanged. At least one must be present.
type adminZoomSettingsRequest struct {
	ZoomMinAdmin   *float64 `json:"zoom_min_admin"`
	ZoomMinRegular *float64 `json:"zoom_min_regular"`
}

type adminZoomSettingsResponse struct {
	ZoomMinAdmin   float64 `json:"zoom_min_admin"`
	ZoomMinRegular float64 `json:"zoom_min_regular"`
}

// handleAdminZoomSettings updates the camera zoom floors (admin-gated). The
// SetZoomSettings command mutates w.Settings + emits ZoomSettingsChanged
// (→ zoom_settings_changed WS frame, so connected clients reload live);
// durability rides the next checkpoint. Mirrors handleAdminPhase's shape.
func (s *Server) handleAdminZoomSettings(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		writeAuthError(w, "invalid")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	dec := json.NewDecoder(r.Body)
	var req adminZoomSettingsRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	res, err := s.world.SendContext(r.Context(), adminCommand(user.Username, func(world *sim.World) (any, error) {
		return sim.SetZoomSettings(req.ZoomMinAdmin, req.ZoomMinRegular).Fn(world)
	}))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errAdminForbidden) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, sim.ErrInvalidZoomSetting) {
			writeError(w, http.StatusBadRequest, "provide zoom_min_admin and/or zoom_min_regular as positive numbers")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	out, ok := res.(sim.SetZoomSettingsResult)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected zoom result")
		return
	}
	writeJSON(w, adminZoomSettingsResponse{
		ZoomMinAdmin:   out.ZoomMinAdmin,
		ZoomMinRegular: out.ZoomMinRegular,
	})
}

// adminAgentTicksRequest is the POST /api/village/admin/agent-ticks body — the
// global LLM-agent pause toggle. paused is a pointer so a missing field is a
// 400 rather than a silent "resume".
type adminAgentTicksRequest struct {
	Paused *bool `json:"paused"`
}

type adminAgentTicksResponse struct {
	AgentTicksPaused bool `json:"agent_ticks_paused"`
}

// handleAdminAgentTicks toggles the global LLM-agent activity pause
// (admin-gated). Mutates w.Settings + emits AgentTicksPausedChanged; durability
// rides the next checkpoint.
func (s *Server) handleAdminAgentTicks(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		writeAuthError(w, "invalid")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	dec := json.NewDecoder(r.Body)
	var req adminAgentTicksRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Paused == nil {
		writeError(w, http.StatusBadRequest, "paused is required")
		return
	}

	res, err := s.world.SendContext(r.Context(), adminCommand(user.Username, func(world *sim.World) (any, error) {
		return sim.SetAgentTicksPaused(*req.Paused).Fn(world)
	}))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errAdminForbidden) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	out, ok := res.(sim.SetAgentTicksPausedResult)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected agent-ticks result")
		return
	}
	writeJSON(w, adminAgentTicksResponse{AgentTicksPaused: out.Paused})
}

type adminForceRotateResponse struct {
	ObjectsAffected int `json:"objects_affected"`
}

// handleAdminForceRotate runs one daily asset-rotation pass on demand
// (admin-gated) — the v2 port of v1's force-rotate. Reuses ApplyDailyRotation
// with the production bulk scope (empty RotationScope = rotate everything,
// matching RunRotationTicker); the rotation emits RotationApplied, so the
// washerwoman/town_crier routes + object_state flips propagate as usual.
// Accepts an empty body or "{}" (the client sends "{}") and rejects anything
// else, bounded by MaxBytesReader like the other admin writes.
func (s *Server) handleAdminForceRotate(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	if user == nil {
		writeAuthError(w, "invalid")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&struct{}{}); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	res, err := s.world.SendContext(r.Context(), adminCommand(user.Username, func(world *sim.World) (any, error) {
		// Allocate the PRNG inside the gate so forbidden requests don't pay for
		// it; used only here on the world goroutine, never shared.
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		return sim.ApplyDailyRotation(sim.RotationTickInputs{Now: time.Now().UTC(), Rand: rng}, sim.RotationScope{}).Fn(world)
	}))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if errors.Is(err, errAdminForbidden) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	out, ok := res.(sim.RotationResult)
	if !ok {
		writeError(w, http.StatusInternalServerError, "unexpected rotate result")
		return
	}
	writeJSON(w, adminForceRotateResponse{ObjectsAffected: out.ObjectsAffected})
}
