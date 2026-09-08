package sim

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// settings_registry.go — LLM-577. ONE table describing every world setting the
// engine loads, so the three surfaces that used to be hand-maintained lists can
// be derived from it instead of drifting apart:
//
//   - the LOADER (repo/pg buildSettings) — key → field, with a default
//   - the CHECKPOINT (MutableWorldSettings → SaveMutableSettings) — which keys
//     survive a restart
//   - the OPERATOR API (GET /umbilical/settings, POST /umbilical/settings/set)
//
// Before this table those were three independent lists. The loader read 110
// keys, the API reported 47 of them and could write 15, and the checkpoint
// persisted a different 28 — so a knob added to the loader was invisible to an
// operator until someone remembered to widen two more files. Nobody ever did:
// the entire visitor_* family was readable and unwritable, and world_dusk_time
// was neither. A drift test in repo/pg (settings_registry_coverage_test.go)
// fails the build if a key is added to the loader and not registered here.
//
// The registry deliberately does NOT own defaults. Those stay in the loader
// next to their *Default constants, where the per-key clamp/warning behaviour
// (clampNonNegSetting and friends) already lives — moving them here would buy
// nothing and would put boot-time correctness behind a second indirection.

// SettingKind is the value shape of a setting, which decides how a stored
// string is parsed and how a live value is formatted back to a string.
type SettingKind string

const (
	SettingKindInt SettingKind = "int"
	// SettingKindDuration is a SCALAR INT in the setting table whose unit comes
	// from the key's suffix (_ms / _seconds / _minutes / _hours) — never
	// time.ParseDuration syntax. See DurationUnitForKey.
	SettingKindDuration SettingKind = "duration"
	SettingKindBool     SettingKind = "bool"
	SettingKindFloat    SettingKind = "float"
	SettingKindString   SettingKind = "string"
)

// SettingEffect says WHEN a live write to a setting starts changing behaviour.
// It is a claim about the consuming code, so it is only ever set from something
// actually read in that code — never guessed.
type SettingEffect string

const (
	// SettingEffectImmediate — the consumer reads w.Settings at the point of
	// use (or the value is already live-tuned by one of the pre-LLM-577
	// bespoke routes), so a write is in force on the next read.
	SettingEffectImmediate SettingEffect = "immediately"
	// SettingEffectOnRestart — the consumer reads the value ONCE at startup to
	// size a ticker or a worker pool. The write still lands in memory and is
	// persisted, but nothing changes until the process restarts. The engine
	// says so itself, e.g. readCheckpointInterval: "Read once at checkpointer
	// startup; a Settings change mid-run takes effect on the next process
	// start."
	SettingEffectOnRestart SettingEffect = "on_restart"
	// SettingEffectUnaudited — the write lands in memory and persists, but
	// nobody has traced this key's consumer to establish whether it re-reads.
	// Reported honestly rather than defaulted to "immediately", because a
	// false liveness promise is the one failure an operator cannot see: the
	// call returns 200, the value reads back changed, and the village keeps
	// using the old one. Narrowing one of these to a verified value is a
	// one-line change once someone reads the consumer.
	SettingEffectUnaudited SettingEffect = "unaudited"
)

// SettingSpec describes one setting key end to end.
type SettingSpec struct {
	Key    string
	Kind   SettingKind
	Effect SettingEffect
	// Persist marks a key the checkpoint writes back to the setting table, so a
	// live change survives a restart.
	//
	// OWNERSHIP POLICY — every registered key is Persist:true today, and that
	// is a deliberate change of ownership, not just drift cleanup. Before
	// LLM-577 the checkpoint wrote 28 keys and the rest of the setting table was
	// operator-owned: you could edit a row directly and the engine would leave
	// it alone. Now the engine owns every REGISTERED key and will overwrite a
	// direct edit to one at the next checkpoint.
	//
	// That is the coherent counterpart to making them all writable through the
	// umbilical: a knob you can set live has to survive a restart, or the API
	// lies. It also costs little that was real — an edit made against a running
	// engine never took effect anyway, since the value is only read at boot and
	// the checkpoint would beat the restart to it. Stop → edit → start still
	// works exactly as before, because Load reads the table before any
	// checkpoint runs.
	//
	// An UNREGISTERED row is still untouched (SaveMutableSettings writes only
	// what the registry projects, never a full replace) — pinned by
	// TestSaveMutableSettings_LeavesUnregisteredRowsAlone.
	//
	// The flag stays on SettingSpec so a genuinely operator-owned key can be
	// marked Persist:false later without reintroducing a hand-maintained list.
	Persist bool

	get func(*WorldSettings) string
	set func(*WorldSettings, string) error
}

// Read returns the setting's current value formatted as it would be stored.
func (s SettingSpec) Read(ws *WorldSettings) string { return s.get(ws) }

// Apply parses raw and writes it into ws. The error is model/operator-facing.
func (s SettingSpec) Apply(ws *WorldSettings, raw string) error { return s.set(ws, raw) }

// DurationUnitForKey returns the time unit implied by a duration key's suffix.
// Suffix-driven so a new duration setting needs no change here as long as the
// key follows the convention. The pg loader delegates to this so the registry
// and the loader can never disagree about what "60" means for a given key.
func DurationUnitForKey(key string) (time.Duration, bool) {
	switch {
	case strings.HasSuffix(key, "_ms"):
		return time.Millisecond, true
	case strings.HasSuffix(key, "_seconds"):
		return time.Second, true
	case strings.HasSuffix(key, "_minutes"):
		return time.Minute, true
	case strings.HasSuffix(key, "_hours"):
		return time.Hour, true
	}
	return 0, false
}

// --- spec constructors -------------------------------------------------------
//
// Each takes a pointer-to-field accessor so an entry in the table below is one
// line. The accessor closes over nothing; it just projects the field out of
// whichever WorldSettings it is handed, which is what lets the same spec serve
// a read of the live world and a write into it.

// intSetting is a whole-number setting that must not be negative.
//
// Every int setting in the table is a count, a percentage, a permille, a
// threshold, a coin amount, or an hour/minute of day — none of which has a
// meaningful negative value. Enforcing that here is not merely tidiness: for
// the keys the loader reads through clampNonNegSetting (the cold_* rates), a
// negative accepted live would sit in memory as -5 and come back from the next
// restart as 0, because the loader clamps it. That live-vs-stored divergence is
// invisible from the API — the read-back shows the value you wrote, and only a
// restart reveals it never really took. Rejecting at the door removes the whole
// class.
//
// Where the loader does NOT clamp, this leaves the API stricter than the
// loader: a hand-edited negative row still boots. That asymmetry is the safe
// direction (a refusal is visible; a silent divergence is not), and the loader
// keeps its permissive-with-fallback posture on purpose, so a bad row can never
// stop the always-live village from booting.
func intSetting(key string, effect SettingEffect, field func(*WorldSettings) *int) SettingSpec {
	return SettingSpec{
		Key: key, Kind: SettingKindInt, Effect: effect, Persist: true,
		get: func(ws *WorldSettings) string { return strconv.Itoa(*field(ws)) },
		set: func(ws *WorldSettings, raw string) error {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be a whole number, got %q", key, raw)
			}
			if n < 0 {
				return fmt.Errorf("%s must be 0 or greater, got %q", key, raw)
			}
			*field(ws) = n
			return nil
		},
	}
}

// pctSetting is intSetting bounded to a whole percentage, 0–100 inclusive. For a
// knob that scales a quantity by pct/100, a value above 100 is not a bigger
// effect but a broken one — the estate rate would debit a purse below its own
// floor — so the setter refuses it rather than leaving the consumer to clamp
// (LLM-652, code_review).
func pctSetting(key string, effect SettingEffect, field func(*WorldSettings) *int) SettingSpec {
	spec := intSetting(key, effect, field)
	setInt := spec.set
	spec.set = func(ws *WorldSettings, raw string) error {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil && n > 100 {
			return fmt.Errorf("%s must be between 0 and 100, got %q", key, raw)
		}
		return setInt(ws, raw)
	}
	return spec
}

// durationSetting stores and reports the SCALAR (e.g. 90 for
// max_warrant_age_seconds), matching the setting table. The unit comes from the
// key suffix, so a key without one is a programming error and is rejected at
// write time rather than silently storing a nanosecond count.
func durationSetting(key string, effect SettingEffect, field func(*WorldSettings) *time.Duration) SettingSpec {
	return SettingSpec{
		Key: key, Kind: SettingKindDuration, Effect: effect, Persist: true,
		get: func(ws *WorldSettings) string {
			unit, ok := DurationUnitForKey(key)
			if !ok {
				return "0"
			}
			return strconv.FormatInt(int64(*field(ws)/unit), 10)
		},
		set: func(ws *WorldSettings, raw string) error {
			unit, ok := DurationUnitForKey(key)
			if !ok {
				return fmt.Errorf("%s has no recognized duration suffix (_ms / _seconds / _minutes / _hours)", key)
			}
			n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be a whole number of %s, got %q", key, unitNoun(unit), raw)
			}
			// Same three guards the loader applies (parseDurationSetting):
			// negative cadences would produce tight loops or immediate expiry,
			// and an unbounded multiply wraps time.Duration negative. Zero stays
			// legal — many keys use it as an off-switch.
			if n < 0 {
				return fmt.Errorf("%s must be 0 or greater, got %q", key, raw)
			}
			if n > int64(maxDuration)/int64(unit) {
				return fmt.Errorf("%s is too large: %q %s overflows the duration it is stored in", key, raw, unitNoun(unit))
			}
			*field(ws) = time.Duration(n) * unit
			return nil
		},
	}
}

func boolSetting(key string, effect SettingEffect, field func(*WorldSettings) *bool) SettingSpec {
	return SettingSpec{
		Key: key, Kind: SettingKindBool, Effect: effect, Persist: true,
		get: func(ws *WorldSettings) string { return strconv.FormatBool(*field(ws)) },
		set: func(ws *WorldSettings, raw string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("%s must be true or false, got %q", key, raw)
			}
			*field(ws) = b
			return nil
		},
	}
}

// floatSetting is a decimal setting that must be finite and non-negative.
//
// strconv.ParseFloat accepts "NaN", "Inf" and "-Inf" by design, and FormatFloat
// round-trips them straight back into the setting table. A NaN is the worst of
// the three: every comparison against it is false, so a zoom floor of NaN would
// silently disable the clamp it exists to enforce rather than failing visibly,
// and the poisoned value would then persist. Both float settings today are zoom
// floors, for which negative is equally meaningless.
func floatSetting(key string, effect SettingEffect, field func(*WorldSettings) *float64) SettingSpec {
	return SettingSpec{
		Key: key, Kind: SettingKindFloat, Effect: effect, Persist: true,
		get: func(ws *WorldSettings) string { return strconv.FormatFloat(*field(ws), 'f', -1, 64) },
		set: func(ws *WorldSettings, raw string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				return fmt.Errorf("%s must be a number, got %q", key, raw)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Errorf("%s must be a finite number, got %q", key, raw)
			}
			if f < 0 {
				return fmt.Errorf("%s must be 0 or greater, got %q", key, raw)
			}
			*field(ws) = f
			return nil
		},
	}
}

// stringSetting rejects an empty value because the loader treats an empty
// string as "not set" (parseStringSetting) — storing one would silently revert
// the key to its default on the next boot, which is not what a caller writing
// "" would expect.
func stringSetting(key string, effect SettingEffect, field func(*WorldSettings) *string) SettingSpec {
	return SettingSpec{
		Key: key, Kind: SettingKindString, Effect: effect, Persist: true,
		get: func(ws *WorldSettings) string { return *field(ws) },
		set: func(ws *WorldSettings, raw string) error {
			v := strings.TrimSpace(raw)
			if v == "" {
				return fmt.Errorf("%s cannot be empty — the loader reads an empty value as unset and would fall back to the default", key)
			}
			*field(ws) = v
			return nil
		},
	}
}

const maxDuration = time.Duration(1<<63 - 1)

func unitNoun(unit time.Duration) string {
	switch unit {
	case time.Millisecond:
		return "milliseconds"
	case time.Second:
		return "seconds"
	case time.Minute:
		return "minutes"
	case time.Hour:
		return "hours"
	}
	return "units"
}
