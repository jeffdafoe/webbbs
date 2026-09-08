package sim

import (
	"log"
	"time"
)

// estate_rate.go — LLM-652. The town's rate on estate: once a game-day, every
// resident NPC holding coin above a floor pays a share of the excess into the town
// chest.
//
// Why it exists. Resident coin concentrates: the village's flows run one way —
// wages out of the producers, spent at the shops, which buy their flour and water
// from the same producers — and the loop closes on two or three purses with a
// standing surplus and nothing that takes it back. Four months of levers acted on
// the edges (the visitor coin band and factor purse) or as flat per-business fees
// (the LLM-557 day's rate, LLM-648 equipment service, stall wear); none touched
// where the coin sat. On 2026-09-08 three purses held ~1700 of ~1960 resident coin
// while the shopkeeper, the innkeeper and the dairykeeper ended every day near
// zero. This is the first half of a fiscal loop — a progressive rate assessed on
// coin held. The second half (the town spending the chest back in at the bottom
// through the constable: public-works hires, provisions for the poor) is a later
// slice, deliberately, so the distribution effect can be observed on its own.
//
// THE ENGINE MOVES THE COIN. That is a deliberate departure from the LLM-557 day's
// rate, whose central choice was that the engine only accrues an obligation and the
// keeper hands the coin over himself when the constable calls. That channel is
// right for one coin a day and wrong for this: it is capped at three because an
// uncatchable keeper must never face a shock bill, it depends on the model choosing
// to pay, and the constable has to be standing there. An assessment of thirty-eight
// coins cannot ride it. The precedent for engine-moved coin is the LLM-615 lodging
// auto-charge (lodger_rebook.go): the coin moves and the record is written in the
// same command, so the two can never disagree.
//
// Progressive, not flat. The obligation is a share of coin held ABOVE
// EstateRateFloor, so a purse at or under the floor pays nothing and the incidence
// falls on the surplus alone. Equilibrium for any actor is
// floor + net_income / rate: with a 5% rate and a floor of 100, a miller netting
// 18 coins a day settles near 480 rather than climbing without limit; at 10% near
// 290. The rate is the knob and it is live (umbilical /settings/set).
//
// Coin-neutral against the village as a whole: what leaves purses lands in
// World.Environment.TownChest, which is durable (world_state.town_chest_coins) —
// the coin has left the purses, so losing the chest on restart would destroy it.
// Σ resident coin + chest changes only by visitor legs, grants and wages to
// visitors, which is the invariant the coin sweep can check.
//
// Seams: assessEstateRate fires once per game-day from checkAndRotate
// (world_rotation.go), beside ApplyTownRate on the same durable LastRotationAt
// boundary; the chest rides WorldEnvironment through the checkpoint; the knobs are
// registry settings (settings_registry_table.go). There is no perception cue in
// this slice — the payer's own action ring carries the explanation (see
// assessEstateRate).

const (
	// DefaultEstateRateFloor is the coin an actor keeps untouched. Sized above every
	// working purse in the village on 2026-09-08 (the largest non-surplus purse was
	// 47) so the levy reaches the three pooled purses and nobody else.
	DefaultEstateRateFloor = 100

	// DefaultEstateRatePctPerDay is the share of coin above the floor taken each
	// game-day, in whole percent. A non-positive value disables the levy (the
	// per-feature off-switch, mirroring TownRateCoinsPerDay<=0).
	DefaultEstateRatePctPerDay = 5

	// estateRateForText is the payment's stated purpose on both records — the
	// in-memory ring ("You paid the town 38 coins for the rate on your estate") and
	// the durable row's `for`.
	estateRateForText = "the rate on your estate"

	// estateRateRecipientName is the counterparty name on both records. It names no
	// actor on purpose: the chest is not a peer, so the coin-record boot seed must
	// not resolve it to anyone (see loadPaymentsSinceSQL, which excludes these rows
	// outright by the estate_rate marker rather than by failing the name lookup).
	estateRateRecipientName = "the town"
)

// EstateRateDue returns what a purse owes this assessment: pct percent of the coin
// held strictly above floor, floored to whole coins. A non-positive pct disables
// the levy (returns 0 — the off-switch); a balance at or below the floor owes
// nothing. Pure, so the assessment and anything reasoning about the rate read the
// same rule.
func EstateRateDue(coins, floor, pct int) int {
	if pct <= 0 || coins <= floor {
		return 0
	}
	return (coins - floor) * pct / 100
}

// estateRateAssessable gates who the levy falls on: a resident NPC. Visitors carry
// their purse in and out of the village, PCs are players, decoratives are never
// ticked and hold a seed purse nobody spends, and the constable is the collector
// rather than a ratepayer. Nil-safe.
func estateRateAssessable(a *Actor) bool {
	if a == nil {
		return false
	}
	if a.Kind != KindNPCStateful && a.Kind != KindNPCShared {
		return false
	}
	if a.VisitorState != nil || IsVisitorActorID(a.ID) {
		return false
	}
	return !ActorIsConstable(a)
}

// assessEstateRate runs one daily pass over every resident NPC, moving each purse's
// due into the town chest and recording the payment on both the in-memory ring and
// the durable action log. A non-positive EstateRatePctPerDay disables the feature
// entirely. Called on the world goroutine from checkAndRotate, so the mutations are
// serialized with every other world write.
//
// Both records are written in the same command as the debit, the way the lodging
// auto-charge does it (LLM-615), and for the LLM-572 reason: coin that leaves a
// purse with nothing saying why gets read as a debt someone owes back. The ring
// entry is what the payer sees under "What you've recently done" for the next few
// hours; the durable row is what the operator and the day-note distiller see. The
// durable row carries the `estate_rate` marker and NO recipient actor id, so the
// coin-record seed (rehydrateCoinRecordOnLoad) never credits a pair with it — and
// RecordCoinPaid is deliberately not called here for the same reason: the chest is
// not an actor, and crediting the constable would tell him he holds coin he does
// not.
func assessEstateRate(w *World, now time.Time) {
	if w == nil || w.Settings.EstateRatePctPerDay <= 0 {
		return
	}
	floor := w.Settings.EstateRateFloor
	pct := w.Settings.EstateRatePctPerDay
	for _, a := range w.Actors {
		if !estateRateAssessable(a) {
			continue
		}
		due := EstateRateDue(a.Coins, floor, pct)
		if due <= 0 {
			continue
		}
		a.Coins -= due
		w.Environment.TownChest += due

		speakerName := a.DisplayName
		if speakerName == "" {
			speakerName = string(a.ID)
		}
		if _, err := AppendActionLogEntry(ActionLogEntry{
			ActorID:          a.ID,
			OccurredAt:       now,
			ActionType:       ActionTypePaid,
			Text:             estateRateForText,
			HuddleID:         a.CurrentHuddleID,
			CounterpartyName: estateRateRecipientName,
			Amount:           due,
		}).Fn(w); err != nil {
			// Append failed (empty ActorID / zero time — caller bug). The coin has
			// moved; log loudly rather than roll back, so the chest and the purses
			// stay consistent with each other.
			log.Printf("sim/estate_rate: action-log append failed for %q: %v", a.ID, err)
		}
		w.AppendActionLogDurable(DurableActionLogRow{
			ActorID:    a.ID,
			OccurredAt: now,
			ActionType: ActionTypePaid,
			Payload: map[string]any{
				"recipient":   estateRateRecipientName,
				"amount":      due,
				"for":         estateRateForText,
				"estate_rate": true,
				"chest_after": w.Environment.TownChest,
			},
			SpeakerName: speakerName,
			HuddleID:    a.CurrentHuddleID,
			Source:      "engine",
		})
	}
}

// ApplyEstateRate wraps the daily assessment as a Command so the rotation driver
// can run it on the world goroutine. Mirrors ApplyTownRate / ApplyFarmUpkeep.
func ApplyEstateRate(now time.Time) Command {
	return Command{Fn: func(w *World) (any, error) {
		assessEstateRate(w, now)
		return nil, nil
	}}
}
