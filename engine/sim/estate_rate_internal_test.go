package sim

import (
	"math/rand"
	"testing"
	"time"
)

// estate_rate_internal_test.go — LLM-652. Internal (package sim) coverage for the
// levy's three pieces: the pure rate rule, who it falls on, and the daily pass that
// moves the coin and writes the records. Reaches assessEstateRate directly against
// a hand-built World, mirroring town_rate_internal_test.go.

func TestEstateRateDue(t *testing.T) {
	cases := []struct {
		name              string
		coins, floor, pct int
		want              int
	}{
		// The live roster on 2026-09-08 at the defaults.
		{"Joseph 864", 864, 100, 5, 38},
		{"Prudence 486", 486, 100, 5, 19},
		{"Ezekiel 339", 339, 100, 5, 11}, // (339-100)*5/100 = 11.95 → 11
		{"Moses 47 is under the floor", 47, 100, 5, 0},
		{"exactly at the floor", 100, 100, 5, 0},
		{"one above the floor rounds to nothing", 101, 100, 5, 0},
		{"twenty above the floor is the first whole coin", 120, 100, 5, 1},
		{"pct 0 disables", 864, 100, 0, 0},
		{"negative pct disables", 864, 100, -5, 0},
		{"floor 0 taxes from the first coin", 200, 0, 5, 10},
		{"a higher rate", 864, 100, 10, 76},
		// A percentage above 100 is refused by the setter but could still arrive
		// from a malformed persisted row; it must never debit below the floor.
		{"pct above 100 is clamped to the excess", 200, 100, 101, 100},
		{"an absurd pct still takes only the excess", 200, 100, 1_000_000, 100},
		{"empty purse", 0, 100, 5, 0},
		{"a negative balance owes nothing", -5, 100, 5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EstateRateDue(c.coins, c.floor, c.pct); got != c.want {
				t.Errorf("EstateRateDue(coins=%d, floor=%d, pct=%d) = %d, want %d",
					c.coins, c.floor, c.pct, got, c.want)
			}
		})
	}
}

// estateRateWorld builds the assessment's scope matrix: every actor kind the
// village carries, all holding the same surplus, so the pass has to tell them apart
// by predicate and nothing else.
func estateRateWorld() *World {
	w := &World{
		Settings: WorldSettings{
			EstateRateFloor:     100,
			EstateRatePctPerDay: 5,
		},
		Actors: map[ActorID]*Actor{
			"joseph":   {ID: "joseph", DisplayName: "Joseph Scott", Kind: KindNPCShared, Coins: 864},
			"prudence": {ID: "prudence", DisplayName: "Prudence Ward", Kind: KindNPCStateful, Coins: 486},
			"moses":    {ID: "moses", DisplayName: "Moses James", Kind: KindNPCShared, Coins: 47},
			"wendy":    {ID: "wendy", DisplayName: "Wendy", Kind: KindPC, Coins: 500},
			"cow":      {ID: "cow", DisplayName: "Villager 3", Kind: KindDecorative, Coins: 500},
			"vstr-0a1b2c3d": {
				ID: "vstr-0a1b2c3d", DisplayName: "Jonas Penhallow the factor",
				Kind: KindNPCShared, Coins: 500, VisitorState: &VisitorState{},
			},
			"gideon": {
				ID: "gideon", DisplayName: "Constable Gideon Marsh", Kind: KindNPCStateful, Coins: 500,
				Attributes: map[string][]byte{AttrConstable: nil},
			},
		},
	}
	return w
}

func totalCoins(w *World) int {
	sum := 0
	for _, a := range w.Actors {
		sum += a.Coins
	}
	return sum
}

func TestAssessEstateRate_WhoPaysAndConservation(t *testing.T) {
	w := estateRateWorld()
	w.Environment.TownChest = 300 // a chest already holding earlier days' rate
	before := totalCoins(w) + w.Environment.TownChest
	now := time.Unix(1_700_000_000, 0).UTC()

	assessEstateRate(w, now)

	want := map[ActorID]int{
		"joseph":        864 - 38,
		"prudence":      486 - 19,
		"moses":         47,  // under the floor
		"wendy":         500, // a player
		"cow":           500, // decorative
		"vstr-0a1b2c3d": 500, // a visitor carries his purse in and out
		"gideon":        500, // the collector is not a ratepayer
	}
	for id, coins := range want {
		if got := w.Actors[id].Coins; got != coins {
			t.Errorf("%s: coins = %d after assessment, want %d", id, got, coins)
		}
	}
	if w.Environment.TownChest != 300+38+19 {
		t.Errorf("TownChest = %d, want %d — the chest must gain exactly what left the purses", w.Environment.TownChest, 300+38+19)
	}
	if after := totalCoins(w) + w.Environment.TownChest; after != before {
		t.Errorf("Σ purses + chest = %d after assessment, want %d — the levy must be coin-neutral", after, before)
	}
}

func TestAssessEstateRate_OffSwitchLeavesEverythingAlone(t *testing.T) {
	w := estateRateWorld()
	w.Settings.EstateRatePctPerDay = 0
	w.Environment.TownChest = 7
	before := totalCoins(w)

	assessEstateRate(w, time.Unix(1_700_000_000, 0).UTC())

	if w.Actors["joseph"].Coins != 864 {
		t.Errorf("Joseph's coins = %d with the levy disabled, want 864", w.Actors["joseph"].Coins)
	}
	if w.Environment.TownChest != 7 {
		t.Errorf("TownChest = %d with the levy disabled, want 7 (untouched)", w.Environment.TownChest)
	}
	if totalCoins(w) != before {
		t.Errorf("purses moved with the levy disabled")
	}
	if len(w.ActionLog) != 0 {
		t.Errorf("%d action-log entries written with the levy disabled, want 0", len(w.ActionLog))
	}
}

// The assessment compounds day over day toward floor + income/rate: with no income
// the purse decays geometrically and never crosses the floor.
func TestAssessEstateRate_ConvergesOnTheFloor(t *testing.T) {
	w := estateRateWorld()
	now := time.Unix(1_700_000_000, 0).UTC()
	prev := w.Actors["joseph"].Coins
	for day := 0; day < 400; day++ {
		assessEstateRate(w, now.Add(time.Duration(day)*24*time.Hour))
		got := w.Actors["joseph"].Coins
		if got > prev {
			t.Fatalf("day %d: coins rose %d → %d", day, prev, got)
		}
		if got < w.Settings.EstateRateFloor {
			t.Fatalf("day %d: coins %d fell below the floor %d", day, got, w.Settings.EstateRateFloor)
		}
		prev = got
	}
	// 5% of the excess, floored: the last whole coin is taken when the excess is
	// 20, so the purse settles at floor + 19.
	if got := w.Actors["joseph"].Coins; got != 119 {
		t.Errorf("after 400 days Joseph holds %d, want 119 (floor 100 + the 19 the rounding leaves)", got)
	}
}

// Both records are written in the same pass as the debit, and they say what the
// coin was (the LLM-572 lesson): the ring entry names the town and the purpose so
// "What you've recently done" reads as a rate paid, not a debt owed back; the
// durable row carries the estate_rate marker and NO recipient actor id, so the
// coin-record boot seed can never credit anyone with it.
func TestAssessEstateRate_WritesBothRecords(t *testing.T) {
	w := estateRateWorld()
	sink := &recordingActionLogSink{}
	w.SetActionLogSink(sink)
	now := time.Unix(1_700_000_000, 0).UTC()

	assessEstateRate(w, now)

	if len(w.ActionLog) != 2 {
		t.Fatalf("ring entries = %d, want 2 (one per payer)", len(w.ActionLog))
	}
	for _, e := range w.ActionLog {
		if e.ActionType != ActionTypePaid {
			t.Errorf("ring entry type = %q, want %q", e.ActionType, ActionTypePaid)
		}
		if e.CounterpartyName != "the town" || e.Text != "the rate on your estate" {
			t.Errorf("ring entry = %q for %q, want the town / the rate on your estate", e.CounterpartyName, e.Text)
		}
		if e.OccurredAt != now {
			t.Errorf("ring entry stamped %v, want the assessment time %v", e.OccurredAt, now)
		}
		wantAmount := map[ActorID]int{"joseph": 38, "prudence": 19}[e.ActorID]
		if e.Amount != wantAmount {
			t.Errorf("%s: ring amount = %d, want %d", e.ActorID, e.Amount, wantAmount)
		}
	}

	if len(sink.rows) != 2 {
		t.Fatalf("durable rows = %d, want 2", len(sink.rows))
	}
	seenChest := map[int]bool{}
	for _, row := range sink.rows {
		if row.ActionType != ActionTypePaid || row.Source != "engine" {
			t.Errorf("durable row = %s/%s, want paid/engine", row.ActionType, row.Source)
		}
		if row.Payload["estate_rate"] != true {
			t.Errorf("%s: durable row lacks the estate_rate marker", row.ActorID)
		}
		if _, has := row.Payload["recipient_actor_id"]; has {
			t.Errorf("%s: durable row names a recipient actor — the chest is not an actor", row.ActorID)
		}
		if row.Payload["recipient"] != "the town" || row.Payload["for"] != "the rate on your estate" {
			t.Errorf("%s: durable row = %v", row.ActorID, row.Payload)
		}
		if row.SpeakerName != w.Actors[row.ActorID].DisplayName {
			t.Errorf("%s: SpeakerName = %q", row.ActorID, row.SpeakerName)
		}
		chest, _ := row.Payload["chest_after"].(int)
		seenChest[chest] = true
	}
	// Map order is not fixed, so the two chest_after stamps are 38+19 and either 38
	// or 19 — whichever payer went first.
	if !seenChest[57] || !(seenChest[38] || seenChest[19]) {
		t.Errorf("chest_after stamps = %v, want the running chest after each debit", seenChest)
	}

	// And no coin-pair record: the chest is not a peer.
	if pairs := countCoinPairs(w.CoinRecord); pairs != 0 {
		t.Errorf("coin record holds %d pair(s) after the assessment, want 0", pairs)
	}
}

func TestEstateRateAssessable(t *testing.T) {
	if estateRateAssessable(nil) {
		t.Error("nil actor is assessable")
	}
	w := estateRateWorld()
	want := map[ActorID]bool{
		"joseph": true, "prudence": true, "moses": true,
		"wendy": false, "cow": false, "vstr-0a1b2c3d": false, "gideon": false,
	}
	for id, ok := range want {
		if got := estateRateAssessable(w.Actors[id]); got != ok {
			t.Errorf("estateRateAssessable(%s) = %v, want %v", id, got, ok)
		}
	}
	// A visitor is excluded by id shape alone, even before VisitorState is stamped.
	bare := &Actor{ID: "vstr-deadbeef", Kind: KindNPCShared, Coins: 500}
	if estateRateAssessable(bare) {
		t.Error("a vstr- id with no VisitorState is assessable")
	}
}

// A setter-side guard for the same invariant: the registry refuses a percentage
// above 100 outright, so a live tune cannot put the levy into the state
// EstateRateDue clamps against.
func TestEstateRatePctSettingIsBounded(t *testing.T) {
	ws := WorldSettings{}
	if _, err := ApplySetting(&ws, "estate_rate_pct_per_day", "101"); err == nil {
		t.Error("ApplySetting accepted estate_rate_pct_per_day=101")
	}
	if _, err := ApplySetting(&ws, "estate_rate_pct_per_day", "-1"); err == nil {
		t.Error("ApplySetting accepted estate_rate_pct_per_day=-1")
	}
	for _, ok := range []string{"0", "5", "100"} {
		if _, err := ApplySetting(&ws, "estate_rate_pct_per_day", ok); err != nil {
			t.Errorf("ApplySetting rejected estate_rate_pct_per_day=%s: %v", ok, err)
		}
	}
	if ws.EstateRatePctPerDay != 100 {
		t.Errorf("EstateRatePctPerDay = %d after the last accepted write, want 100", ws.EstateRatePctPerDay)
	}
}

// A positive-balance actor never ends an assessment below the floor, whatever the
// (possibly malformed) percentage — the invariant the floor exists for.
func TestAssessEstateRate_NeverDebitsBelowTheFloor(t *testing.T) {
	for _, pct := range []int{1, 5, 50, 100, 101, 5000} {
		w := estateRateWorld()
		w.Settings.EstateRatePctPerDay = pct
		before := map[ActorID]int{}
		for id, a := range w.Actors {
			before[id] = a.Coins
		}
		assessEstateRate(w, time.Unix(1_700_000_000, 0).UTC())
		for id, a := range w.Actors {
			floor := w.Settings.EstateRateFloor
			switch {
			case before[id] <= floor && a.Coins != before[id]:
				t.Errorf("pct %d: %s started at %d (at or under the floor) and was debited to %d", pct, id, before[id], a.Coins)
			case before[id] > floor && a.Coins < floor:
				t.Errorf("pct %d: %s ended at %d coins, below the floor %d", pct, id, a.Coins, floor)
			}
		}
	}
}

// The umbilical force-rotate calls ApplyDailyRotation directly; the levy is bound
// to the boundary crossing in checkAndRotate instead, so a forced rotation must
// never assess anyone (the same seam the farm-upkeep and day's-rate passes use).
func TestApplyDailyRotation_DoesNotAssessEstateRate(t *testing.T) {
	w := estateRateWorld()
	w.Environment.TownChest = 11
	before := map[ActorID]int{}
	for id, a := range w.Actors {
		before[id] = a.Coins
	}
	if _, err := ApplyDailyRotation(RotationTickInputs{Now: time.Unix(1_700_000_000, 0).UTC(), Rand: rand.New(rand.NewSource(1))}, RotationScope{}).Fn(w); err != nil {
		t.Fatalf("ApplyDailyRotation: %v", err)
	}
	if w.Environment.TownChest != 11 {
		t.Errorf("TownChest = %d after a bare rotation, want 11 — the rotation itself must not levy", w.Environment.TownChest)
	}
	for id, a := range w.Actors {
		if a.Coins != before[id] {
			t.Errorf("%s: coins %d → %d across a bare rotation", id, before[id], a.Coins)
		}
	}
}
