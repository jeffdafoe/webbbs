package sim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// settings_registry_table.go — LLM-577. The table itself. One line per setting
// key the loader reads; see settings_registry.go for the machinery and for why
// this exists.
//
// EFFECT CLASSIFICATION, and why most of it is "unaudited":
//
// SettingEffectImmediate is claimed here ONLY where the consumer was read and
// shown to take the value off w.Settings at the point of use (cold.go's rate
// lookup, npc_sleep.go's bedtime, shift_duty.go's dusk parse, the visitor
// dispatcher's spawn clamps, …), or where a pre-LLM-577 bespoke route already
// advertised the key as live-tunable.
//
// SettingEffectOnRestart is claimed only where the engine documents the
// once-at-startup read in its own comments — readCheckpointInterval and
// cascade/idle_backstop.go's readSweepInterval both say a mid-run change "takes
// effect on the next process start", and handlers/pool.go sizes its worker pool
// from TickWorkerCount at construction.
//
// Everything else is SettingEffectUnaudited: the write lands and persists, but
// its consumer has not been traced. That is deliberately the default. Guessing
// "immediately" would produce the one failure mode an operator cannot detect —
// a 200, a changed read-back, and a village still running the old value.
// Promoting a key to a verified value is a one-line edit after reading its
// consumer, and is worth doing opportunistically whenever one is touched.

// settingSpecs is built once at init and never mutated.
var settingSpecs = buildSettingRegistry()

// SettingSpecs returns every registered setting, ordered by key.
func SettingSpecs() []SettingSpec {
	out := make([]SettingSpec, len(settingSpecs))
	copy(out, settingSpecs)
	return out
}

// SettingSpecByKey looks up one spec.
func SettingSpecByKey(key string) (SettingSpec, bool) {
	for _, s := range settingSpecs {
		if s.Key == key {
			return s, true
		}
	}
	return SettingSpec{}, false
}

// SettingKeys returns every registered key, sorted. The drift test in repo/pg
// compares this against the keys the loader actually reads.
func SettingKeys() []string {
	out := make([]string, 0, len(settingSpecs))
	for _, s := range settingSpecs {
		out = append(out, s.Key)
	}
	sort.Strings(out)
	return out
}

// ReadAllSettings projects every registered key out of ws as stored-string
// values — the complete read side of GET /umbilical/settings.
func ReadAllSettings(ws *WorldSettings) map[string]string {
	out := make(map[string]string, len(settingSpecs))
	for _, s := range settingSpecs {
		out[s.Key] = s.Read(ws)
	}
	return out
}

// PersistableSettingRows projects the keys the checkpoint writes back to the
// setting table. This REPLACES the hand-listed rows SaveMutableSettings used to
// carry: a key registered here is durable by construction, so a live tune can
// no longer silently revert on the next restart.
func PersistableSettingRows(ws *WorldSettings) map[string]string {
	out := make(map[string]string, len(settingSpecs))
	for _, s := range settingSpecs {
		if !s.Persist {
			continue
		}
		out[s.Key] = s.Read(ws)
	}
	return out
}

// ApplySetting parses raw and writes it into ws, returning the spec so the
// caller can report the key's kind and take-effect timing. An unknown key is an
// error rather than a silent no-op — a typo'd key must not read as success.
func ApplySetting(ws *WorldSettings, key, raw string) (SettingSpec, error) {
	spec, ok := SettingSpecByKey(strings.TrimSpace(key))
	if !ok {
		return SettingSpec{}, fmt.Errorf("unknown setting %q", key)
	}
	if err := spec.Apply(ws, raw); err != nil {
		return SettingSpec{}, err
	}
	return spec, nil
}

func buildSettingRegistry() []SettingSpec {
	specs := []SettingSpec{
		// --- world clock + phase boundaries --------------------------------
		// dawn/dusk are parsed at the point of use (shift_duty.go's ParseHM,
		// visitor.go's worldDawnDuskMinutes, npc_sleep.go's lodger window), so
		// a change lands on the next read. These two were previously reachable
		// only by hand-editing the setting table over SSH.
		stringSetting("world_dawn_time", SettingEffectImmediate, func(s *WorldSettings) *string { return &s.DawnTime }),
		stringSetting("world_dusk_time", SettingEffectImmediate, func(s *WorldSettings) *string { return &s.DuskTime }),
		stringSetting("world_rotation_time", SettingEffectUnaudited, func(s *WorldSettings) *string { return &s.RotationTime }),
		timezoneSetting(),

		durationSetting("checkpoint_interval_seconds", SettingEffectOnRestart, func(s *WorldSettings) *time.Duration { return &s.CheckpointInterval }),

		// --- client / operator toggles (live via the admin config panel) ----
		floatSetting("world_zoom_min_admin", SettingEffectImmediate, func(s *WorldSettings) *float64 { return &s.ZoomMinAdmin }),
		floatSetting("world_zoom_min_regular", SettingEffectImmediate, func(s *WorldSettings) *float64 { return &s.ZoomMinRegular }),
		boolSetting("agent_ticks_paused", SettingEffectImmediate, func(s *WorldSettings) *bool { return &s.AgentTicksPaused }),

		// --- lodging -------------------------------------------------------
		intSetting("lodging_check_out_hour", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.LodgingCheckOutHour }),
		intSetting("lodging_bedtime_hour", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.LodgingBedtimeHour }),
		intSetting("lodging_default_weekly_rate", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.LodgingDefaultWeeklyRate }),

		// --- shifts + sleep ------------------------------------------------
		intSetting("shift_lateness_window_minutes", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ShiftLatenessWindowMinutes }),
		intSetting("npc_sleep_max_duration_hours", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.NPCSleepMaxDurationHours }),

		// --- constable rounds (LLM-514) ------------------------------------
		durationSetting("constable_rounds_interval_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.ConstableRoundsInterval }),

		// --- needs ---------------------------------------------------------
		intSetting("attribute_tick_amount", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.NeedsTickAmount }),
		intSetting("tiredness_critical_threshold", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.TirednessCriticalThreshold }),
		intSetting("movement_fatigue_per_tile_x100", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.MovementFatiguePerTileX100 }),
		intSetting("tiredness_recovery_per_minute_x100", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.TirednessRecoveryPerMinuteX100 }),
		intSetting("restock_reorder_pct", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.RestockReorderPct }),

		// --- stall wear & repair (LLM-118/LLM-247) -------------------------
		intSetting("stall_wear_per_coin", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallWearPerCoin }),
		intSetting("stall_wear_repair_threshold", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallWearRepairThreshold }),
		intSetting("stall_wear_degrade_threshold", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallWearDegradeThreshold }),
		intSetting("stall_nails_per_repair", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallNailsPerRepair }),
		intSetting("stall_repair_duration_seconds", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallRepairDurationSeconds }),
		intSetting("stall_degraded_produce_pct", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StallDegradedProducePct }),
		intSetting("equipment_service_due_threshold", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.EquipmentServiceDueThreshold }),

		// --- farm upkeep (LLM-215) / town rate (LLM-557) -------------------
		intSetting("farm_upkeep_floor", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.FarmUpkeepFloor }),
		intSetting("farm_upkeep_coins_per_shovel", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.FarmUpkeepCoinsPerShovel }),
		intSetting("town_rate_coins_per_day", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.TownRateCoinsPerDay }),
		intSetting("town_rate_max_owed", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.TownRateMaxOwed }),

		// --- estate rate (LLM-652) ------------------------------------------
		intSetting("estate_rate_floor", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.EstateRateFloor }),
		pctSetting("estate_rate_pct_per_day", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.EstateRatePctPerDay }),

		// --- cold exposure + hearth (LLM-412) ------------------------------
		// cold.go reads every one of these off w.Settings inside the per-minute
		// rate lookup.
		//
		// These are the keys the loader reads through clampNonNegSetting: a
		// stored negative boots as 0 with a SettingWarning attached. That makes
		// them the reason intSetting refuses a negative outright — accepting one
		// live would leave memory holding -5 while the next restart resolves the
		// same key to 0, a divergence the API cannot show you.
		intSetting("cold_storm_outdoors_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdStormOutdoorsPerMinuteX100 }),
		intSetting("cold_storm_indoors_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdStormIndoorsPerMinuteX100 }),
		intSetting("cold_warm_garment_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdWarmGarmentPerMinuteX100 }),
		intSetting("cold_threadbare_garment_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdThreadbareGarmentPerMinuteX100 }),
		intSetting("cold_night_multiplier_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdNightMultiplierX100 }),
		intSetting("cold_warm_recovery_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdWarmRecoveryPerMinuteX100 }),
		intSetting("cold_clear_recovery_per_minute_x100", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdClearRecoveryPerMinuteX100 }),
		intSetting("cold_produce_sap_pct", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.ColdProduceSapPct }),
		intSetting("hearth_burn_minutes_per_wood", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.HearthBurnMinutesPerWood }),
		intSetting("hearth_max_bank_minutes", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.HearthMaxBankMinutes }),
		intSetting("hearth_low_minutes", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.HearthLowMinutes }),
		intSetting("stoke_wood_per_stoke", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StokeWoodPerStoke }),
		intSetting("stoke_duration_seconds", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.StokeDurationSeconds }),

		// --- garment wear (LLM-422) ----------------------------------------
		intSetting("garment_wear_per_minute", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.GarmentWearPerMinute }),
		intSetting("garment_threadbare_fraction_x100", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.GarmentThreadbareFractionX100 }),

		// --- reactor / tick pipeline ---------------------------------------
		durationSetting("reactor_jitter_min_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.ReactorJitterMin }),
		durationSetting("reactor_jitter_max_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.ReactorJitterMax }),
		durationSetting("reactor_evaluator_cadence_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.ReactorEvaluatorCadence }),
		durationSetting("max_warrant_age_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.MaxWarrantAge }),
		intSetting("max_reactor_ticks_per_actor_per_minute", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.MaxReactorTicksPerActorPerMinute }),
		intSetting("max_warrants_per_actor", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.MaxWarrantsPerActor }),
		durationSetting("min_reactor_tick_gap_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.MinReactorTickGap }),
		durationSetting("labor_reply_cadence_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.LaborReplyCadence }),
		durationSetting("admission_backoff_ms", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.AdmissionBackoff }),
		// handlers/pool.go sizes the worker pool and its channel buffer from
		// this at construction — a live change cannot resize a running pool.
		intSetting("tick_worker_count", SettingEffectOnRestart, func(s *WorldSettings) *int { return &s.TickWorkerCount }),

		// --- degeneracy observer (LLM-94 / LLM-124) ------------------------
		intSetting("degeneracy_thin_after_ticks", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.DegeneracyThinAfterTicks }),
		intSetting("degeneracy_throttle_after_ticks", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.DegeneracyThrottleAfterTicks }),
		durationSetting("degeneracy_throttle_min_duration_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.DegeneracyThrottleMinDuration }),
		durationSetting("degeneracy_throttle_backoff_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.DegeneracyThrottleBackoff }),
		intSetting("degeneracy_oscillation_window", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.DegeneracyOscillationWindow }),
		intSetting("degeneracy_oscillation_min_transitions", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.DegeneracyOscillationMinTransitions }),
		intSetting("degeneracy_oscillation_max_distinct", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.DegeneracyOscillationMaxDistinct }),

		// --- staleness decay (LLM-233) -------------------------------------
		durationSetting("stale_wake_decay_base_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.StaleWakeDecayBase }),
		durationSetting("stale_wake_decay_cap_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.StaleWakeDecayCap }),

		// --- backstops -----------------------------------------------------
		durationSetting("idle_backstop_threshold_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.IdleBackstopThreshold }),
		// cascade/idle_backstop.go readSweepInterval: read once at sweep
		// startup, "the new value takes effect on the next process start".
		durationSetting("idle_backstop_sweep_interval_minutes", SettingEffectOnRestart, func(s *WorldSettings) *time.Duration { return &s.IdleBackstopSweepInterval }),
		durationSetting("red_need_backstop_base_delay_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.RedNeedBackstopBaseDelay }),
		durationSetting("red_need_backstop_max_delay_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.RedNeedBackstopMaxDelay }),
		durationSetting("red_need_backstop_sweep_interval_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.RedNeedBackstopSweepInterval }),

		// --- atmosphere / weather cascades ---------------------------------
		durationSetting("atmosphere_refresh_interval_hours", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.AtmosphereRefreshInterval }),
		durationSetting("storm_interval_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.StormInterval }),
		durationSetting("storm_duration_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.StormDuration }),

		// --- action log substrate ------------------------------------------
		durationSetting("action_log_retention_hours", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.ActionLogRetention }),
		durationSetting("action_log_sweep_interval_hours", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.ActionLogSweepInterval }),

		// --- visitor cascade (LLM-437 et al) -------------------------------
		// The spawn/return dispatchers read w.Settings and clamp at spawn time,
		// so a change applies to the next visitor. This whole family was
		// readable and unwritable before LLM-577.
		intSetting("visitor_merchant_trickle_chance_permille", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorMerchantTrickleChancePermille }),
		intSetting("visitor_merchant_correction_chance_permille", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorMerchantCorrectionChancePermille }),
		intSetting("visitor_passer_spawn_chance_permille", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorPasserSpawnChancePermille }),
		intSetting("visitor_max_concurrent", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorMaxConcurrent }),
		intSetting("visitor_min_stay_minutes", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorMinStayMinutes }),
		intSetting("visitor_max_stay_minutes", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorMaxStayMinutes }),
		durationSetting("visitor_tick_interval_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.VisitorTickInterval }),
		intSetting("visitor_return_min_days", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorReturnMinDays }),
		intSetting("visitor_return_max_days", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorReturnMaxDays }),
		intSetting("visitor_factor_pack_units", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorPackUnits }),
		intSetting("visitor_factor_purse_min", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorPurseMin }),
		intSetting("visitor_factor_purse_max", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorPurseMax }),
		intSetting("visitor_factor_iron_units", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorIronUnits }),
		intSetting("visitor_factor_salt_units", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorSaltUnits }),
		intSetting("visitor_factor_thread_units", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorFactorThreadUnits }),
		intSetting("visitor_coin_band_low", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorCoinBandLow }),
		intSetting("visitor_coin_band_high", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorCoinBandHigh }),
		intSetting("visitor_sell_weight_permille", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.VisitorSellWeightPermille }),

		// --- businessowner cooldowns / scene radius ------------------------
		intSetting("businessowner_greet_cooldown_minutes", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.BusinessownerGreetCooldownMinutes }),
		intSetting("businessowner_farewell_cooldown_minutes", SettingEffectUnaudited, func(s *WorldSettings) *int { return &s.BusinessownerFarewellCooldownMinutes }),
		intSetting("default_outdoor_scene_radius", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.DefaultOutdoorSceneRadius }),

		// --- scene quote / pay ledger / order sweeps -----------------------
		durationSetting("scene_quote_ttl_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.SceneQuoteTTL }),
		durationSetting("scene_quote_sweep_cadence_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.SceneQuoteSweepCadence }),
		durationSetting("pay_ledger_ttl_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.PayLedgerTTL }),
		durationSetting("pay_ledger_sweep_cadence_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.PayLedgerSweepCadence }),
		durationSetting("order_ttl_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.OrderTTL }),
		durationSetting("order_sweep_cadence_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.OrderSweepCadence }),

		// --- huddles -------------------------------------------------------
		durationSetting("huddle_silence_timeout_minutes", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.HuddleSilenceTimeout }),
		durationSetting("huddle_silence_sweep_cadence_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.HuddleSilenceSweepCadence }),
		durationSetting("huddle_live_window_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.HuddleLiveWindow }),
		durationSetting("huddle_loop_timeout_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.HuddleLoopTimeout }),
		intSetting("huddle_loop_repeat_percent", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.HuddleLoopRepeatPercent }),
		durationSetting("huddle_loop_sweep_cadence_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.HuddleLoopSweepCadence }),
		intSetting("huddle_loop_max_turns", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.HuddleLoopMaxTurns }),
		durationSetting("huddle_conversation_wind_down_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.HuddleConversationWindDown }),
		durationSetting("huddle_continuity_window_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.HuddleContinuityWindow }),

		// --- seek work / labor / merchant ----------------------------------
		intSetting("seek_work_coin_ceiling", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.SeekWorkCoinCeiling }),
		intSetting("seek_work_need_yield_margin", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.SeekWorkNeedYieldMargin }),
		intSetting("labor_produce_boost_pct", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.LaborProduceBoostPct }),
		intSetting("merchant_coin_floor", SettingEffectImmediate, func(s *WorldSettings) *int { return &s.MerchantCoinFloor }),

		// --- eco mode (LLM-313 / LLM-466) ----------------------------------
		boolSetting("eco_enabled", SettingEffectImmediate, func(s *WorldSettings) *bool { return &s.EcoEnabled }),
		durationSetting("eco_social_gap_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.EcoSocialGap }),
		durationSetting("eco_economy_gap_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.EcoEconomyGap }),
		durationSetting("eco_audience_idle_seconds", SettingEffectImmediate, func(s *WorldSettings) *time.Duration { return &s.PCAudienceIdleAfter }),

		// --- PC / NPC conversation liveness --------------------------------
		durationSetting("pc_presence_stale_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.PCPresenceStaleAfter }),
		durationSetting("pc_await_reply_window_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.PCAwaitReplyWindow }),
		durationSetting("npc_await_reply_window_seconds", SettingEffectUnaudited, func(s *WorldSettings) *time.Duration { return &s.NPCAwaitReplyWindow }),
	}

	// Per-need red-line thresholds. These keys are not literals anywhere — the
	// loader walks the sim.Needs registry — so they are generated the same way
	// here. Adding a need therefore adds its setting to the operator surface
	// with no edit to this file.
	for _, n := range Needs {
		specs = append(specs, needThresholdSetting(n))
	}
	return specs
}

// timezoneSetting is bespoke because Timezone and Location must move together:
// everything that converts a world instant uses Location, so writing the name
// without re-resolving the zone would leave the engine reporting one timezone
// and computing in another. An unloadable zone is rejected with BOTH fields
// untouched, so a typo cannot half-apply. Marked on-restart: the derived
// Location is captured in enough places at boot that a live swap is not a
// claim worth making without an audit.
func timezoneSetting() SettingSpec {
	const key = "world_timezone"
	return SettingSpec{
		Key: key, Kind: SettingKindString, Effect: SettingEffectOnRestart, Persist: true,
		get: func(ws *WorldSettings) string { return ws.Timezone },
		set: func(ws *WorldSettings, raw string) error {
			name := strings.TrimSpace(raw)
			if name == "" {
				return fmt.Errorf("%s cannot be empty", key)
			}
			loc, err := time.LoadLocation(name)
			if err != nil {
				return fmt.Errorf("%s %q is not a known timezone: %w", key, name, err)
			}
			ws.Timezone = name
			ws.Location = loc
			return nil
		},
	}
}

// needThresholdSetting exposes one need's red-line. The value lives in the
// NeedThresholds map rather than a struct field, so it cannot use the
// pointer-to-field constructors. A nil map is allocated on write rather than
// panicking — a WorldSettings built by hand in a test may not have one.
func needThresholdSetting(n Need) SettingSpec {
	key := n.ThresholdSettingKey
	needKey := n.Key
	return SettingSpec{
		Key: key, Kind: SettingKindInt, Effect: SettingEffectImmediate, Persist: true,
		get: func(ws *WorldSettings) string { return strconv.Itoa(ws.NeedThresholds[needKey]) },
		set: func(ws *WorldSettings, raw string) error {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be a whole number, got %q", key, raw)
			}
			// Same non-negative floor intSetting applies: a need's red-line is a
			// point on a 0..NeedMax scale, and a negative one would put every
			// actor permanently past it.
			if v < 0 {
				return fmt.Errorf("%s must be 0 or greater, got %q", key, raw)
			}
			if ws.NeedThresholds == nil {
				ws.NeedThresholds = make(NeedThresholds, len(Needs))
			}
			ws.NeedThresholds[needKey] = v
			return nil
		},
	}
}
