package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// config_handlers_test.go — ZBBS-WORK-363 admin world-config read + write
// routes. The okAuth caller is "tester"; seedAdmin makes that actor IsAdmin.

func TestHandleConfig_Admin(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	w.Environment.TownChest = 69 // LLM-652: the chest rides the config read
	srv := NewServer(w, okAuth{})

	rec := get(t, srv, "/api/village/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var cfg WorldConfigDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.TownChestCoins != 69 {
		t.Errorf("town_chest_coins = %d, want 69", cfg.TownChestCoins)
	}
}

// Non-admin caller (no seedAdmin) → 403, same as the admin write routes. Built
// inline because the get helper asserts 200.
func TestHandleConfig_Forbidden(t *testing.T) {
	srv := NewServer(seededWorld(t), okAuth{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/village/config", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminZoomSettings_Accepted(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/zoom-settings", `{"zoom_min_admin":0.15,"zoom_min_regular":0.25}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var res adminZoomSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.ZoomMinAdmin != 0.15 || res.ZoomMinRegular != 0.25 {
		t.Errorf("response = %+v, want 0.15/0.25", res)
	}
}

func TestHandleAdminZoomSettings_Forbidden(t *testing.T) {
	srv := NewServer(seededWorld(t), okAuth{})
	rec := post(t, srv, "/api/village/admin/zoom-settings", `{"zoom_min_admin":0.15}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminZoomSettings_NeitherProvided(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/zoom-settings", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminZoomSettings_NonPositive(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/zoom-settings", `{"zoom_min_admin":-0.1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminAgentTicks_Accepted(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/agent-ticks", `{"paused":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var res adminAgentTicksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.AgentTicksPaused {
		t.Errorf("response = %+v, want agent_ticks_paused=true", res)
	}
}

func TestHandleAdminAgentTicks_MissingPaused(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/agent-ticks", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminForceRotate_Accepted(t *testing.T) {
	w := seededWorld(t)
	seedAdmin(t, w, "admin-tester", "tester")
	srv := NewServer(w, okAuth{})

	rec := post(t, srv, "/api/village/admin/force-rotate", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdminForceRotate_Forbidden(t *testing.T) {
	srv := NewServer(seededWorld(t), okAuth{})
	rec := post(t, srv, "/api/village/admin/force-rotate", `{}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// The two new config WS frames translate with the exact keys the client reads.
func TestTranslateEvent_ConfigFrames(t *testing.T) {
	zf, ok := TranslateEvent(&sim.ZoomSettingsChanged{ZoomMinAdmin: 0.1, ZoomMinRegular: 0.2})
	if !ok || zf.Type != "zoom_settings_changed" {
		t.Fatalf("zoom frame = %+v ok=%v, want zoom_settings_changed", zf, ok)
	}
	zd, ok := zf.Data.(zoomSettingsChangedWireDTO)
	if !ok || zd.ZoomMinAdmin != 0.1 || zd.ZoomMinRegular != 0.2 {
		t.Errorf("zoom data = %#v, want 0.1/0.2", zf.Data)
	}

	af, ok := TranslateEvent(&sim.AgentTicksPausedChanged{Paused: true})
	if !ok || af.Type != "agent_ticks_paused_changed" {
		t.Fatalf("agent-ticks frame = %+v ok=%v, want agent_ticks_paused_changed", af, ok)
	}
	ad, ok := af.Data.(agentTicksPausedChangedWireDTO)
	if !ok || !ad.AgentTicksPaused {
		t.Errorf("agent-ticks data = %#v, want AgentTicksPaused=true", af.Data)
	}
}
