package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// pgxmock-based tests for EnvironmentRepo (Slice 15 / ZBBS-WORK-242).
// Asserts the SQL shape + scan mapping for the world_state singleton
// row plus the parser fallbacks for the kv setting table. Real-pg
// behaviors (CHECK constraints, the world_state_singleton invariant,
// timestamp tz handling) land with the testcontainers smoke slice.

func newMockPoolE(t *testing.T) (pgxmock.PgxPoolIface, *EnvironmentRepo) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return mock, NewEnvironmentRepo(mock)
}

// worldStateColumns are the columns selected by loadWorldStateSQL, in
// order. Centralized so the fixtures stay in sync if the column set
// changes.
var worldStateColumns = []string{
	"phase", "last_transition_at", "last_rotation_at",
	"weather", "atmosphere", "last_needs_tick_at",
	"town_chest_coins",
}

// programWorldStateRow programs a single world_state row scan with
// the given env+phase values. lastNeedsTickAt may be nil to model
// SQL NULL.
func programWorldStateRow(mock pgxmock.PgxPoolIface, phase sim.Phase,
	lastTransitionAt, lastRotationAt time.Time,
	weather, atmosphere string,
	lastNeedsTickAt *time.Time,
) {
	programWorldStateRowWithChest(mock, phase, lastTransitionAt, lastRotationAt,
		weather, atmosphere, lastNeedsTickAt, 0)
}

// programWorldStateRowWithChest is programWorldStateRow with an explicit
// town_chest_coins value (LLM-652); the plain helper reads an empty chest.
func programWorldStateRowWithChest(mock pgxmock.PgxPoolIface, phase sim.Phase,
	lastTransitionAt, lastRotationAt time.Time,
	weather, atmosphere string,
	lastNeedsTickAt *time.Time,
	townChest int,
) {
	mock.ExpectQuery(`SELECT[\s\S]+FROM world_state[\s\S]+WHERE id = 1`).
		WillReturnRows(pgxmock.NewRows(worldStateColumns).
			AddRow(string(phase), lastTransitionAt, lastRotationAt,
				weather, atmosphere, lastNeedsTickAt, townChest))
}

// programSettingsRows programs the setting kv query returning the
// given key-value map. Keys are emitted in map order — fine because
// the loader doesn't depend on row order.
func programSettingsRows(mock pgxmock.PgxPoolIface, values map[string]string) {
	rows := pgxmock.NewRows([]string{"key", "value"})
	for k, v := range values {
		rows.AddRow(k, v)
	}
	mock.ExpectQuery(`SELECT key, value FROM setting WHERE value IS NOT NULL`).
		WillReturnRows(rows)
}

// --- Load: happy path -----------------------------------------------------

// TestEnvironmentRepo_Load_HappyPath — fully populated world_state row
// + a representative settings map. Verifies env, phase, and a sampling
// of WorldSettings fields across each parser type.
func TestEnvironmentRepo_Load_HappyPath(t *testing.T) {
	mock, repo := newMockPoolE(t)

	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	lastRotation := at.Add(-24 * time.Hour)
	lastNeeds := at.Add(-1 * time.Hour)

	programWorldStateRow(mock, sim.PhaseDay, at, lastRotation,
		"clear skies", "morning fog hangs heavy", &lastNeeds)

	programSettingsRows(mock, map[string]string{
		"world_dawn_time":                   "06:30",
		"world_dusk_time":                   "20:00",
		"world_timezone":                    "America/New_York",
		"world_zoom_min_admin":              "0.05",
		"world_zoom_min_regular":            "0.25",
		"agent_ticks_paused":                "true",
		"hunger_red_threshold":              "18",
		"thirst_red_threshold":              "12",
		"tiredness_red_threshold":           "20",
		"tiredness_critical_threshold":      "22",
		"movement_fatigue_per_tile_x100":    "12",
		"reactor_jitter_min_ms":             "1000",
		"reactor_jitter_max_ms":             "4000",
		"idle_backstop_threshold_minutes":   "30",
		"atmosphere_refresh_interval_hours": "4",
		"action_log_retention_hours":        "48",
		"visitor_spawn_chance_permille":     "0",
		"visitor_tick_interval_seconds":     "60",
		"scene_quote_ttl_minutes":           "10",
		"pay_ledger_ttl_minutes":            "3",
		"order_ttl_minutes":                 "10",
		"checkpoint_interval_seconds":       "60",
	})

	env, phase, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if phase != sim.PhaseDay {
		t.Errorf("phase = %q, want %q", phase, sim.PhaseDay)
	}
	if !env.LastTransitionAt.Equal(at) {
		t.Errorf("LastTransitionAt = %v, want %v", env.LastTransitionAt, at)
	}
	if !env.LastRotationAt.Equal(lastRotation) {
		t.Errorf("LastRotationAt = %v, want %v", env.LastRotationAt, lastRotation)
	}
	if !env.LastNeedsTickAt.Equal(lastNeeds) {
		t.Errorf("LastNeedsTickAt = %v, want %v", env.LastNeedsTickAt, lastNeeds)
	}
	if env.Weather != "clear skies" {
		t.Errorf("Weather = %q", env.Weather)
	}
	if env.Atmosphere != "morning fog hangs heavy" {
		t.Errorf("Atmosphere = %q", env.Atmosphere)
	}
	if settings.DawnTime != "06:30" {
		t.Errorf("DawnTime = %q", settings.DawnTime)
	}
	if settings.Timezone != "America/New_York" || settings.Location == nil {
		t.Errorf("Timezone/Location not populated: tz=%q loc=%v", settings.Timezone, settings.Location)
	}
	if settings.ZoomMinAdmin != 0.05 {
		t.Errorf("ZoomMinAdmin = %v", settings.ZoomMinAdmin)
	}
	if !settings.AgentTicksPaused {
		t.Errorf("AgentTicksPaused = false, want true")
	}
	if settings.ReactorJitterMin != 1*time.Second {
		t.Errorf("ReactorJitterMin = %v", settings.ReactorJitterMin)
	}
	if settings.IdleBackstopThreshold != 30*time.Minute {
		t.Errorf("IdleBackstopThreshold = %v", settings.IdleBackstopThreshold)
	}
	if settings.AtmosphereRefreshInterval != 4*time.Hour {
		t.Errorf("AtmosphereRefreshInterval = %v", settings.AtmosphereRefreshInterval)
	}
	if settings.TirednessCriticalThreshold != 22 {
		t.Errorf("TirednessCriticalThreshold = %d", settings.TirednessCriticalThreshold)
	}
	if settings.NeedThresholds.Get("hunger") != 18 {
		t.Errorf("NeedThresholds[hunger] = %d", settings.NeedThresholds.Get("hunger"))
	}
	if settings.CheckpointInterval != 60*time.Second {
		t.Errorf("CheckpointInterval = %v", settings.CheckpointInterval)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// --- Load: defaults when settings missing ---------------------------------

// TestEnvironmentRepo_Load_AllDefaults_WhenSettingsEmpty — empty kv
// table means every WorldSettings field falls back to its code
// default. Sanity-check a sampling of every parser flavor.
func TestEnvironmentRepo_Load_AllDefaults_WhenSettingsEmpty(t *testing.T) {
	mock, repo := newMockPoolE(t)
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	programWorldStateRow(mock, sim.PhaseDay, at, at, "", "", nil)
	programSettingsRows(mock, map[string]string{})

	_, _, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if settings.DawnTime != sim.DefaultDawn {
		t.Errorf("DawnTime = %q, want %q", settings.DawnTime, sim.DefaultDawn)
	}
	if settings.Timezone != sim.DefaultTimezone {
		t.Errorf("Timezone = %q", settings.Timezone)
	}
	if settings.ZoomMinAdmin != sim.DefaultZoomMinAdmin {
		t.Errorf("ZoomMinAdmin = %v, want %v", settings.ZoomMinAdmin, sim.DefaultZoomMinAdmin)
	}
	if settings.NeedsTickAmount != sim.DefaultNeedsTickAmount {
		t.Errorf("NeedsTickAmount = %d", settings.NeedsTickAmount)
	}
	if settings.NeedThresholds.Get("hunger") != sim.DefaultHungerRedThreshold {
		t.Errorf("NeedThresholds[hunger] = %d", settings.NeedThresholds.Get("hunger"))
	}
	wantCrit := (sim.NeedMax*sim.DefaultTirednessCriticalThresholdPct + 99) / 100
	if settings.TirednessCriticalThreshold != wantCrit {
		t.Errorf("TirednessCriticalThreshold = %d, want %d", settings.TirednessCriticalThreshold, wantCrit)
	}
	if settings.ReactorJitterMin != 1*time.Second {
		t.Errorf("ReactorJitterMin default = %v", settings.ReactorJitterMin)
	}
	if settings.IdleBackstopThreshold != 30*time.Minute {
		t.Errorf("IdleBackstopThreshold default = %v", settings.IdleBackstopThreshold)
	}
	if settings.AtmosphereRefreshInterval != 4*time.Hour {
		t.Errorf("AtmosphereRefreshInterval default = %v", settings.AtmosphereRefreshInterval)
	}
	if settings.AgentTicksPaused {
		t.Errorf("AgentTicksPaused default = true, want false")
	}
	if settings.CheckpointInterval != 60*time.Second {
		t.Errorf("CheckpointInterval default = %v", settings.CheckpointInterval)
	}
}

// --- Load: hard failures --------------------------------------------------

// TestEnvironmentRepo_Load_NoWorldStateRow_HardFails — defensive
// against the impossible-post-migration case. Surfaces loudly so a
// fresh deploy without the ZBBS-038 seed gets a clear diagnostic.
//
// Models pgx behavior accurately: an empty result set causes
// QueryRow.Scan to return pgx.ErrNoRows, not Query itself to return
// an error.
func TestEnvironmentRepo_Load_NoWorldStateRow_HardFails(t *testing.T) {
	mock, repo := newMockPoolE(t)
	mock.ExpectQuery(`SELECT[\s\S]+FROM world_state`).
		WillReturnRows(pgxmock.NewRows(worldStateColumns))

	_, _, _, err := repo.Load(context.Background())
	if err == nil {
		t.Fatal("Load should error on missing world_state row")
	}
	// Two independent assertions: (1) the error chain still surfaces
	// pgx.ErrNoRows for callers using errors.Is, AND (2) the
	// user-facing message names the missing row. Gating the substring
	// check on (!errors.Is) would let a regression to bare ErrNoRows
	// silently pass. (code_review R2.)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("error = %v, want wrapped pgx.ErrNoRows", err)
	}
	if got := err.Error(); !strings.Contains(got, "world_state row missing") {
		t.Errorf("error = %q, want substring about missing world_state row", got)
	}
}

// --- Load: NULL last_needs_tick_at ----------------------------------------

// TestEnvironmentRepo_Load_NullLastNeedsTickAt — NULL scans to a nil
// pointer and the loader leaves env.LastNeedsTickAt as zero time.
func TestEnvironmentRepo_Load_NullLastNeedsTickAt(t *testing.T) {
	mock, repo := newMockPoolE(t)
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	programWorldStateRow(mock, sim.PhaseDay, at, at, "", "", nil /*last_needs_tick_at NULL*/)
	programSettingsRows(mock, map[string]string{})

	env, _, _, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !env.LastNeedsTickAt.IsZero() {
		t.Errorf("LastNeedsTickAt = %v, want zero (NULL in DB)", env.LastNeedsTickAt)
	}
}

// --- Load: invalid timezone falls back -------------------------------------

// TestEnvironmentRepo_Load_InvalidTimezoneFallsBack — bad tz string
// in settings logs a warning and falls back to DefaultTimezone. Doesn't
// hard-fail boot.
func TestEnvironmentRepo_Load_InvalidTimezoneFallsBack(t *testing.T) {
	mock, repo := newMockPoolE(t)
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	programWorldStateRow(mock, sim.PhaseDay, at, at, "", "", nil)
	programSettingsRows(mock, map[string]string{
		"world_timezone": "Not/A/Real/Zone",
	})

	_, _, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if settings.Timezone != sim.DefaultTimezone {
		t.Errorf("Timezone = %q, want fallback %q", settings.Timezone, sim.DefaultTimezone)
	}
	if settings.Location == nil {
		t.Error("Location is nil after fallback")
	}
}

// --- Load: malformed int value falls back ---------------------------------

// TestEnvironmentRepo_Load_MalformedIntFallsBack — a non-numeric value
// for an int setting logs a warning and uses the default.
func TestEnvironmentRepo_Load_MalformedIntFallsBack(t *testing.T) {
	mock, repo := newMockPoolE(t)
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	programWorldStateRow(mock, sim.PhaseDay, at, at, "", "", nil)
	programSettingsRows(mock, map[string]string{
		"hunger_red_threshold": "not-a-number",
	})

	_, _, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := settings.NeedThresholds.Get("hunger"); got != sim.DefaultHungerRedThreshold {
		t.Errorf("NeedThresholds[hunger] = %d, want fallback %d", got, sim.DefaultHungerRedThreshold)
	}
}

// --- SaveSnapshot: happy path ---------------------------------------------

// TestEnvironmentRepo_SaveSnapshot_HappyPath — full UPSERT fires with
// the expected args. Plain UPSERT, no nextval/advisory-lock prelude.
func TestEnvironmentRepo_SaveSnapshot_HappyPath(t *testing.T) {
	mock, repo := newMockPoolE(t)
	tx := fakeTx{mock: mock}

	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	rotation := at.Add(-24 * time.Hour)
	needs := at.Add(-1 * time.Hour)
	env := sim.WorldEnvironment{
		LastTransitionAt: at,
		LastRotationAt:   rotation,
		LastNeedsTickAt:  needs,
		Weather:          "clear",
		Atmosphere:       "morning fog",
		TownChest:        69,
	}

	mock.ExpectExec(`INSERT INTO world_state`).
		WithArgs(string(sim.PhaseDay), at, rotation, "clear", "morning fog", needs, 69).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	if err := repo.SaveSnapshot(context.Background(), tx, env, sim.PhaseDay); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// --- SaveSnapshot: nil tx -------------------------------------------------

// TestEnvironmentRepo_SaveSnapshot_NilTx — substrate-boundary nil check
// mirrors HuddlesRepo / VillageObjectsRepo conventions.
func TestEnvironmentRepo_SaveSnapshot_NilTx(t *testing.T) {
	_, repo := newMockPoolE(t)
	err := repo.SaveSnapshot(context.Background(), nil, sim.WorldEnvironment{}, sim.PhaseDay)
	if err == nil {
		t.Fatal("SaveSnapshot(nil tx) should error")
	}
}

// --- SaveSnapshot: zero LastTransitionAt / LastRotationAt -----------------

// TestEnvironmentRepo_SaveSnapshot_ZeroLastTransition_Error — substrate-
// boundary guard against scheduler corruption. Zero time.Time encodes
// as PG year-0001 (NOT NULL passes), which would silently corrupt the
// next phase-transition decision. The guard surfaces upstream bugs
// before they reach the DB.
func TestEnvironmentRepo_SaveSnapshot_ZeroLastTransition_Error(t *testing.T) {
	mock, repo := newMockPoolE(t)
	tx := fakeTx{mock: mock}
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	env := sim.WorldEnvironment{
		// LastTransitionAt intentionally zero.
		LastRotationAt: at,
	}
	err := repo.SaveSnapshot(context.Background(), tx, env, sim.PhaseDay)
	if err == nil {
		t.Fatal("SaveSnapshot(zero LastTransitionAt) should error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL fired: %v", err)
	}
}

// TestEnvironmentRepo_SaveSnapshot_ZeroLastRotation_Error — mirror of
// the LastTransitionAt guard for the daily asset rotation gate.
func TestEnvironmentRepo_SaveSnapshot_ZeroLastRotation_Error(t *testing.T) {
	mock, repo := newMockPoolE(t)
	tx := fakeTx{mock: mock}
	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	env := sim.WorldEnvironment{
		LastTransitionAt: at,
		// LastRotationAt intentionally zero.
	}
	err := repo.SaveSnapshot(context.Background(), tx, env, sim.PhaseDay)
	if err == nil {
		t.Fatal("SaveSnapshot(zero LastRotationAt) should error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL fired: %v", err)
	}
}

// --- SaveSnapshot: invalid phase ------------------------------------------

// TestEnvironmentRepo_SaveSnapshot_InvalidPhase — phase outside the
// 'day' | 'night' enum trips the substrate-boundary check before any
// SQL fires (defends against forgotten phase validation upstream).
func TestEnvironmentRepo_SaveSnapshot_InvalidPhase(t *testing.T) {
	mock, repo := newMockPoolE(t)
	tx := fakeTx{mock: mock}
	err := repo.SaveSnapshot(context.Background(), tx, sim.WorldEnvironment{}, sim.Phase("twilight"))
	if err == nil {
		t.Fatal("SaveSnapshot(invalid phase) should error")
	}
	// Mock should have NO expectations met (no SQL fired).
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL fired: %v", err)
	}
}

// --- SaveSnapshot: zero LastNeedsTickAt → SQL NULL ------------------------

// TestEnvironmentRepo_SaveSnapshot_ZeroLastNeedsTick_NULL — zero
// time.Time encodes as untyped-nil so the UPSERT writes SQL NULL
// rather than '0001-01-01 00:00:00 UTC'.
func TestEnvironmentRepo_SaveSnapshot_ZeroLastNeedsTick_NULL(t *testing.T) {
	mock, repo := newMockPoolE(t)
	tx := fakeTx{mock: mock}

	at := time.Date(2026, 5, 19, 7, 0, 0, 0, time.UTC)
	env := sim.WorldEnvironment{
		LastTransitionAt: at,
		LastRotationAt:   at,
		// LastNeedsTickAt zero on purpose — "never run yet".
	}

	mock.ExpectExec(`INSERT INTO world_state`).
		WithArgs(string(sim.PhaseDay), at, at, "", "", nil /*last_needs_tick_at NULL*/, 0).
		WillReturnResult(pgconn.NewCommandTag("INSERT 0 1"))

	if err := repo.SaveSnapshot(context.Background(), tx, env, sim.PhaseDay); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// --- parser unit tests ----------------------------------------------------

// TestParseDurationSetting — every suffix flavor, missing key, malformed
// value, unrecognized suffix. Table-driven across all four units.
func TestParseDurationSetting(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  string
		def  time.Duration
		want time.Duration
	}{
		{"ms suffix", "reactor_jitter_min_ms", "1000", 5 * time.Second, 1 * time.Second},
		{"seconds suffix", "checkpoint_interval_seconds", "120", 60 * time.Second, 120 * time.Second},
		{"minutes suffix", "idle_backstop_threshold_minutes", "45", 30 * time.Minute, 45 * time.Minute},
		{"hours suffix", "atmosphere_refresh_interval_hours", "8", 4 * time.Hour, 8 * time.Hour},
		{"missing key falls to default", "checkpoint_interval_seconds", "", 60 * time.Second, 60 * time.Second},
		{"malformed value falls to default", "checkpoint_interval_seconds", "abc", 60 * time.Second, 60 * time.Second},
		{"unrecognized suffix falls to default", "checkpoint_interval_centuries", "1", 5 * time.Second, 5 * time.Second},
		{"negative value falls to default", "checkpoint_interval_seconds", "-1", 60 * time.Second, 60 * time.Second},
		{"overflowing value falls to default", "atmosphere_refresh_interval_hours", "99999999999", 4 * time.Hour, 4 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			if tc.raw != "" {
				values[tc.key] = tc.raw
			}
			got := parseDurationSetting(values, tc.key, tc.def)
			if got != tc.want {
				t.Errorf("parseDurationSetting(%q=%q, def=%v) = %v, want %v",
					tc.key, tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

// TestParseBoolSetting — true/false strings, malformed, missing.
func TestParseBoolSetting(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		def  bool
		want bool
	}{
		{"true literal", "true", false, true},
		{"false literal", "false", true, false},
		{"case-tolerant", "TRUE", false, true},
		{"malformed", "yes", false, false},
		{"missing", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			if tc.raw != "" {
				values["agent_ticks_paused"] = tc.raw
			}
			got := parseBoolSetting(values, "agent_ticks_paused", tc.def)
			if got != tc.want {
				t.Errorf("parseBoolSetting(%q, def=%v) = %v, want %v",
					tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

// TestParseIntSetting — happy + fallback paths.
func TestParseIntSetting(t *testing.T) {
	values := map[string]string{
		"good_int":      "42",
		"trimmed_int":   "  17  ",
		"malformed_int": "fortytwo",
	}
	if got := parseIntSetting(values, "good_int", 0); got != 42 {
		t.Errorf("good_int = %d", got)
	}
	if got := parseIntSetting(values, "trimmed_int", 0); got != 17 {
		t.Errorf("trimmed_int = %d", got)
	}
	if got := parseIntSetting(values, "missing", 99); got != 99 {
		t.Errorf("missing = %d, want 99", got)
	}
	if got := parseIntSetting(values, "malformed_int", 99); got != 99 {
		t.Errorf("malformed_int = %d, want fallback 99", got)
	}
}

// TestClampNonNegSetting — a valid value passes through, a negative clamps to 0
// and records a warning, and a missing row falls to the (in-range) default
// without a warning (LLM-439).
func TestClampNonNegSetting(t *testing.T) {
	values := map[string]string{
		"ok":  "25",
		"neg": "-25",
	}
	var warnings []sim.SettingWarning

	if got := clampNonNegSetting(values, "ok", 100, &warnings); got != 25 {
		t.Errorf("ok = %d, want 25", got)
	}
	if got := clampNonNegSetting(values, "missing", 100, &warnings); got != 100 {
		t.Errorf("missing = %d, want default 100", got)
	}
	if len(warnings) != 0 {
		t.Fatalf("in-range + missing recorded warnings: %+v", warnings)
	}

	if got := clampNonNegSetting(values, "neg", 100, &warnings); got != 0 {
		t.Errorf("neg = %d, want clamp to 0", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("negative recorded %d warnings, want 1: %+v", len(warnings), warnings)
	}
	if warnings[0].Key != "neg" || warnings[0].Raw != -25 || warnings[0].Clamped != 0 {
		t.Errorf("warning = %+v, want {key:neg raw:-25 clamped:0}", warnings[0])
	}
}

// coldRateKnobs maps every cold setting key to the WorldSettings field it loads
// into — the full set clampNonNegSetting is applied to (LLM-439). Kept beside the
// buildSettings test so a new cold knob added without validation is caught.
var coldRateKnobs = []struct {
	key string
	get func(sim.WorldSettings) int
}{
	{"cold_storm_outdoors_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdStormOutdoorsPerMinuteX100 }},
	{"cold_storm_indoors_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdStormIndoorsPerMinuteX100 }},
	{"cold_warm_garment_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdWarmGarmentPerMinuteX100 }},
	{"cold_threadbare_garment_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdThreadbareGarmentPerMinuteX100 }},
	{"cold_night_multiplier_x100", func(s sim.WorldSettings) int { return s.ColdNightMultiplierX100 }},
	{"cold_warm_recovery_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdWarmRecoveryPerMinuteX100 }},
	{"cold_clear_recovery_per_minute_x100", func(s sim.WorldSettings) int { return s.ColdClearRecoveryPerMinuteX100 }},
	{"cold_produce_sap_pct", func(s sim.WorldSettings) int { return s.ColdProduceSapPct }},
}

// TestBuildSettings_ClampsNegativeColdRates — end to end through buildSettings for
// EVERY cold knob: a stored negative clamps the field to 0 and records exactly one
// SettingWarning naming that key (LLM-439). The recovery keys are the dangerous
// case — un-clamped they would flip recovery into accrual.
func TestBuildSettings_ClampsNegativeColdRates(t *testing.T) {
	for _, knob := range coldRateKnobs {
		t.Run(knob.key, func(t *testing.T) {
			s := buildSettings(map[string]string{knob.key: "-7"})
			if got := knob.get(s); got != 0 {
				t.Errorf("%s = %d, want clamp to 0", knob.key, got)
			}
			var flagged []sim.SettingWarning
			for _, wrn := range s.SettingWarnings {
				if wrn.Key == knob.key {
					flagged = append(flagged, wrn)
				}
			}
			if len(flagged) != 1 {
				t.Fatalf("%s recorded %d warnings, want 1: %+v", knob.key, len(flagged), s.SettingWarnings)
			}
			if flagged[0].Raw != -7 || flagged[0].Clamped != 0 {
				t.Errorf("%s warning = %+v, want {raw:-7 clamped:0}", knob.key, flagged[0])
			}
		})
	}
}

// TestBuildSettings_ZeroColdRatesPreservedNoWarning — 0 is in range for every cold
// knob (no accrual / full relief / no sap / plain daytime rate for the multiplier),
// so a stored 0 is kept verbatim and generates NO warning (LLM-439).
func TestBuildSettings_ZeroColdRatesPreservedNoWarning(t *testing.T) {
	values := map[string]string{}
	for _, knob := range coldRateKnobs {
		values[knob.key] = "0"
	}
	s := buildSettings(values)
	for _, knob := range coldRateKnobs {
		if got := knob.get(s); got != 0 {
			t.Errorf("%s = %d, want 0 preserved", knob.key, got)
		}
	}
	if len(s.SettingWarnings) != 0 {
		t.Errorf("zero-value cold knobs recorded warnings: %+v", s.SettingWarnings)
	}
}

// TestParseFloatSetting — happy + fallback paths.
func TestParseFloatSetting(t *testing.T) {
	values := map[string]string{
		"good_float":      "0.25",
		"malformed_float": "not-a-float",
	}
	if got := parseFloatSetting(values, "good_float", 0); got != 0.25 {
		t.Errorf("good_float = %v", got)
	}
	if got := parseFloatSetting(values, "missing", 0.1); got != 0.1 {
		t.Errorf("missing = %v, want 0.1", got)
	}
	if got := parseFloatSetting(values, "malformed_float", 0.1); got != 0.1 {
		t.Errorf("malformed_float = %v, want fallback 0.1", got)
	}
}

// TestParseStringSetting — happy, empty value treated as missing.
func TestParseStringSetting(t *testing.T) {
	values := map[string]string{
		"present": "hello",
		"empty":   "",
	}
	if got := parseStringSetting(values, "present", "default"); got != "hello" {
		t.Errorf("present = %q", got)
	}
	if got := parseStringSetting(values, "empty", "default"); got != "default" {
		t.Errorf("empty = %q, want default", got)
	}
	if got := parseStringSetting(values, "missing", "default"); got != "default" {
		t.Errorf("missing = %q, want default", got)
	}
}

// TestLoadNeedThresholds_DrivesOffRegistry — every key in sim.Needs gets
// loaded; missing rows fall back to the registry's DefaultThreshold so
// adding a new need slot doesn't require touching the loader.
func TestLoadNeedThresholds(t *testing.T) {
	values := map[string]string{
		"hunger_red_threshold":    "15", // override default 18
		"tiredness_red_threshold": "22", // override default 20
		// thirst missing → registry default 12
	}
	got := loadNeedThresholds(values)
	if got.Get("hunger") != 15 {
		t.Errorf("hunger = %d, want 15", got.Get("hunger"))
	}
	if got.Get("thirst") != sim.DefaultThirstRedThreshold {
		t.Errorf("thirst = %d, want default %d", got.Get("thirst"), sim.DefaultThirstRedThreshold)
	}
	if got.Get("tiredness") != 22 {
		t.Errorf("tiredness = %d, want 22", got.Get("tiredness"))
	}
	// Every registered need has an entry.
	for _, n := range sim.Needs {
		if _, ok := got[n.Key]; !ok {
			t.Errorf("missing entry for need key %q (registry-driven loop is broken)", n.Key)
		}
	}
}

// TestDurationUnitForKey — exercises every suffix recognized by the
// loader. Driven separately so a new suffix addition has an obvious
// test landing site.
func TestDurationUnitForKey(t *testing.T) {
	cases := []struct {
		key  string
		want time.Duration
		ok   bool
	}{
		{"foo_ms", time.Millisecond, true},
		{"foo_seconds", time.Second, true},
		{"foo_minutes", time.Minute, true},
		{"foo_hours", time.Hour, true},
		{"foo_centuries", 0, false},
		{"foo", 0, false},
	}
	for _, tc := range cases {
		got, ok := durationUnitForKey(tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("durationUnitForKey(%q) = (%v, %v), want (%v, %v)",
				tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

// Compile-time interface satisfaction check — keeps the test file
// failing loudly if the EnvironmentRepo signature drifts from the
// sim.EnvironmentRepo contract.
var _ sim.EnvironmentRepo = (*EnvironmentRepo)(nil)

// Pull errNotImpl reference into the test build so an unused-symbol
// warning never surfaces here (the symbol is exercised by load_world_test.go
// via fakeEnvironment{err: errNotImpl}; this guard is paranoia
// against future imports moving around).
var _ = errors.Is

// --- Load: town chest (LLM-652) --------------------------------------------

// TestEnvironmentRepo_Load_TownChest — the estate rate's chest rides the
// world_state singleton and comes back on the environment. It is the one piece
// of state the levy adds that MUST survive a restart: the coin has left the
// purses, so a chest that reloaded as zero would have destroyed it.
func TestEnvironmentRepo_Load_TownChest(t *testing.T) {
	mock, repo := newMockPoolE(t)

	at := time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)
	programWorldStateRowWithChest(mock, sim.PhaseNight, at, at, "", "", nil, 69)
	programSettingsRows(mock, map[string]string{
		"estate_rate_floor":       "150",
		"estate_rate_pct_per_day": "10",
	})

	env, _, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env.TownChest != 69 {
		t.Errorf("TownChest = %d, want 69", env.TownChest)
	}
	if settings.EstateRateFloor != 150 || settings.EstateRatePctPerDay != 10 {
		t.Errorf("estate rate settings = floor %d / pct %d, want 150 / 10",
			settings.EstateRateFloor, settings.EstateRatePctPerDay)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestEnvironmentRepo_Load_EstateRateDefaults — with no setting rows the two
// knobs fall back to the compiled defaults, so a deploy needs no seed rows.
func TestEnvironmentRepo_Load_EstateRateDefaults(t *testing.T) {
	mock, repo := newMockPoolE(t)

	at := time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)
	programWorldStateRow(mock, sim.PhaseDay, at, at, "", "", nil)
	programSettingsRows(mock, map[string]string{})

	env, _, settings, err := repo.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env.TownChest != 0 {
		t.Errorf("TownChest = %d on an empty chest, want 0", env.TownChest)
	}
	if settings.EstateRateFloor != sim.DefaultEstateRateFloor || settings.EstateRatePctPerDay != sim.DefaultEstateRatePctPerDay {
		t.Errorf("estate rate settings = floor %d / pct %d, want the defaults %d / %d",
			settings.EstateRateFloor, settings.EstateRatePctPerDay,
			sim.DefaultEstateRateFloor, sim.DefaultEstateRatePctPerDay)
	}
}
