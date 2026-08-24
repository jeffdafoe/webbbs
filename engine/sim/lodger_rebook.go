package sim

import (
	"fmt"
	"log"
	"time"
)

// lodger_rebook.go — ZBBS-HOME-296 PR2. Engine-auto rebook backstop: the
// last resort that keeps a lodger housed when the LLM-driven renewal
// negotiation doesn't fire in time.
//
// v2 shape (a deliberate departure from v1's pay_ledger-driven sweep):
//
//   - The lodger relationship IS an active ledger RoomAccess (checkpointed),
//     not a pay_ledger row. So candidate-finding is an in-memory scan of
//     w.Actors for the soonest active ledger grant in the renewal window —
//     no SQL CTE, no location gate (v1 gated on "physically inside the inn",
//     a pay_ledger-era heuristic; in v2 holding an active grant IS being a
//     lodger). The sweep is gated to PCs (LLM-37): only a human player's grant
//     auto-renews. An NPC's grant lapses at checkout and relies on the keeper-
//     LLM renewal negotiation (or the NPC finds another bed) — Ezekiel Crane is
//     the live NPC lapse-path case.
//   - The renewal is an in-place extension of that grant's ExpiresAt plus a
//     direct coin transfer (lodger -> keeper), exactly how pay_commands.go /
//     pay_with_item_commands.go move coins.
//   - The audit is an ActionLog row (ActionTypePaid) written to BOTH the in-
//     memory ring (umbilical /actions + PC talk panel) and the durable
//     agent_action_log sink (AppendActionLogDurable, LLM-37) — NOT a fabricated
//     delivered pay_ledger row. World.PayLedger models buyer-initiated pay
//     OFFERS (offer -> accept -> deliver); an engine-auto rebook is not an
//     offer, so it stays off that substrate and uses the action-log audit
//     funnel instead. The durable row matters because the in-memory ring is
//     wiped each boot, so before LLM-37 a renewal across a restart left no
//     visible record at all. (ZBBS-HOME-296 §5 predates this v2 pivot; cleared
//     with Jeff 2026-05-23.)
//
// The whole sweep runs as one Command on the world goroutine, so it's
// atomic and naturally consistent — no cross-command race, no partial debit.
// Idempotent: extending ExpiresAt pushes the grant past the window, so a
// re-run (or an LLM renewal that already extended the same grant in place)
// no-ops the next tick. Folded into RunRoomSweep, ahead of ExpireRoomAccess,
// so a still-active grant is renewed before the expiry sweep could flip it.
//
// Live room-scoped narration ("X settled for another night") is deferred —
// the ActionLog entry is the durable, perception-visible record (it surfaces
// in the lodger's "since your last turn" and feeds consolidation); the cosmetic
// Hub broadcast can ride a follow-on once the engine is live.

// autoRebookLeadTime is how far ahead of a grant's expiry the sweep renews.
// 6h is the engine giving up on the LLM — every escalating perception cue
// (the ## Your lodging tiers + the affordability cue) has fired across the
// prior 48h; this catches the "they didn't act" outcome.
const autoRebookLeadTime = 6 * time.Hour

// RebookLodgerRecord is one renewal performed this sweep — returned for
// telemetry / logging (and, later, narration).
type RebookLodgerRecord struct {
	LodgerID     ActorID
	KeeperID     ActorID
	StructureID  StructureID
	Nightly      int
	NewExpiresAt time.Time
}

// RebookLodgersResult carries the renewals performed this sweep.
type RebookLodgersResult struct {
	Renewals []RebookLodgerRecord
	// Holds counts free grant-extensions for offline players this sweep (LLM-450):
	// the room is frozen and held, not billed, while the player is away.
	Holds int
}

// RebookLodgersDue returns a Command that renews every lodger whose soonest
// active ledger grant expires within autoRebookLeadTime, charging the
// per-night rate and extending the grant by one night. Skips (lets the grant
// lapse) when the lodger can't afford a night or the keeper is gone.
func RebookLodgersDue(now time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if now.IsZero() {
				// `now` stamps the audit entry's OccurredAt; a zero value
				// would make AppendActionLogEntry reject the audit AFTER the
				// coins+grant were already mutated, breaking the
				// "renewed implies audited" invariant. Reject up front so no
				// renewal happens without an audit (code_review).
				return RebookLodgersResult{}, fmt.Errorf("RebookLodgersDue: zero now")
			}
			nightly := LodgingNightlyRate(w.Settings.LodgingDefaultWeeklyRate)
			if nightly <= 0 {
				// Rate unset / disabled (or < 7, can't bill 1/night) — the
				// whole backstop is off; existing grants expire naturally.
				return RebookLodgersResult{}, nil
			}
			loc := w.Settings.Location
			if loc == nil {
				loc = time.UTC
			}
			checkOut := w.Settings.LodgingCheckOutHour

			var result RebookLodgersResult
			for lodgerID, lodger := range w.Actors {
				if lodger == nil {
					continue
				}
				if lodger.Kind != KindPC {
					// Only PCs auto-rebook (LLM-37). An NPC's grant lapses at
					// checkout and relies on the keeper-LLM renewal negotiation
					// (or the NPC finds another bed) — the engine no longer
					// silently keeps NPC lodgers housed.
					continue
				}
				grant := soonestActiveLedgerGrant(lodger, now)
				if grant == nil {
					continue
				}
				if grant.ExpiresAt.Sub(now) > autoRebookLeadTime {
					continue // not yet in the renewal window
				}
				if lodgerCoveredBeyond(lodger, now, *grant.ExpiresAt) {
					// Another active grant already covers a later window —
					// renewing the soonest is redundant. Also the idempotency
					// guard against an LLM renewal that landed a second grant.
					continue
				}

				// Validate the physical room before either path — a grant for a
				// deleted/malformed room is not renewable OR holdable (code_review).
				room := findRoom(w, grant.RoomID)
				if room == nil {
					continue
				}

				if PCPresenceStale(lodger.LastPCSeenAt, now, PCPresenceStaleAfter(w)) {
					// LLM-450: an offline player's room is FROZEN, not billed.
					// Extend the grant for free — no coin debit, no keeper credit,
					// no audit row — so it never lapses while the player is away.
					// This is the suspended-animation hold: it keeps the grant
					// active so AutoBedOfflineLodgerPCs can bed them and the
					// checkout/eviction sweeps never fire mid-absence, without
					// draining an absent purse. Paid auto-rebook resumes the moment
					// they reconnect (presence goes fresh). Extending pushes
					// ExpiresAt past the lead-time window, so this fires ~once per
					// night rather than every sweep. Logged so a held room can't
					// become silent capacity leakage (code_review); low-volume by
					// construction (~once per night per absent lodger).
					newExpires := ComputeLodgerUntil(*grant.ExpiresAt, 1, checkOut, loc)
					grant.ExpiresAt = &newExpires
					result.Holds++
					log.Printf("sim/lodger_rebook: holding room %d for offline player %q free until %v (not billed)",
						grant.RoomID, lodgerID, newExpires)
					continue
				}

				keeperID, keeper := keeperForStructure(w, room.StructureID)
				if keeper == nil {
					log.Printf("sim/lodger_rebook: no keeper at structure %q for lodger %q — skipping renewal (grant will lapse)",
						room.StructureID, lodgerID)
					continue
				}
				if keeperID == lodgerID {
					// The keeper holding a lodging grant at their own
					// structure would debit+credit the same actor (net zero)
					// and fabricate a paid renewal + audit. Skip — a keeper
					// isn't a paying lodger of their own inn (code_review).
					continue
				}
				if lodger.SpendableCoins() < nightly {
					// A broke lodger stays a candidate every minute across the
					// whole 6h window — logging each skip would emit ~360 lines
					// per lodger and drown real signal (work's note). Log only
					// in the final window before the grant lapses, so we still
					// get the "went homeless" beat once, near when it matters,
					// without the spam. Stateless — no per-grant bookkeeping.
					if grant.ExpiresAt.Sub(now) <= 2*RoomSweepInterval {
						log.Printf("sim/lodger_rebook: lodger %q has %d coins to spend; need %d — room lapsing (can't afford renewal)",
							lodgerID, lodger.SpendableCoins(), nightly)
					}
					continue
				}

				newExpires := ComputeLodgerUntil(*grant.ExpiresAt, 1, checkOut, loc)
				lodger.Coins -= nightly
				keeper.Coins += nightly
				drawVisitorSpend(lodger, nightly)
				grant.ExpiresAt = &newExpires

				// Fall back to the id for the durable row's speaker_name and
				// recipient so the persisted audit never carries a blank — mirrors
				// actorDisplayName on the pay path. The lean ring keeps the raw
				// keeper DisplayName, so a nameless keeper degrades to
				// renderActionLogEntry's counterparty-less phrasing (matching
				// actorDisplayNameOrEmpty); in practice a keeper always has a name.
				speakerName := lodger.DisplayName
				if speakerName == "" {
					speakerName = string(lodgerID)
				}
				keeperName := keeper.DisplayName
				if keeperName == "" {
					keeperName = string(keeperID)
				}
				const lodgingForText = "a night's lodging"

				// Lean in-memory ring entry (umbilical /actions + PC talk panel).
				// CounterpartyName + Amount let renderActionLogEntry narrate it as
				// "<lodger> pays <keeper> N coins for a night's lodging" rather
				// than a bare "makes a payment"; Text is the short for-phrase,
				// mirroring how a normal pay sets ForText (cascade/action_log.go).
				if _, err := AppendActionLogEntry(ActionLogEntry{
					ActorID:          lodgerID,
					OccurredAt:       now,
					ActionType:       ActionTypePaid,
					Text:             lodgingForText,
					HuddleID:         lodger.CurrentHuddleID,
					CounterpartyName: keeper.DisplayName,
					Amount:           nightly,
				}).Fn(w); err != nil {
					// Append failed (empty ActorID / zero time — caller bug). The
					// coin transfer + extension already happened; log loudly rather
					// than roll back, so the lodger stays housed.
					log.Printf("sim/lodger_rebook: action-log append failed for lodger %q: %v", lodgerID, err)
				}

				// Durable mirror to agent_action_log (LLM-37). The in-memory ring
				// above is wiped every boot, so without this the renewal left no
				// record a player/operator could see after a restart — coins moved
				// silently. Same shape as handlePaidActionLog's durable row. Source
				// is "engine" (not player/agent): the engine auto-charged this, so
				// it stays distinguishable from a real player pay in operator reads.
				// A PC carries no llm_memory_agent, so this row is audit-only —
				// never pulled into NPC day-note distillation.
				w.AppendActionLogDurable(DurableActionLogRow{
					ActorID:    lodgerID,
					OccurredAt: now,
					ActionType: ActionTypePaid,
					Payload: map[string]any{
						"recipient": keeperName,
						// The coin-record seed prefers the id and falls back to an
						// exact display-name match that must be unique, dropping the
						// row when it isn't (LLM-572). This path resolved by name
						// only; the keeper's id is right here, so stamp it.
						"recipient_actor_id": string(keeperID),
						"amount":             nightly,
						"for":                lodgingForText,
						// lodging_grant is this path's goods marker (LLM-615), the
						// counterpart of ledger_id on a pay-with-item settlement. It
						// names the room grant the coin extended, so the boot seed
						// classifies the row from the settlement the engine actually
						// made and never from the `for` text beside it. Forward-only:
						// rows written before this ticket carry no marker and seed as
						// Unstated, the wording the record used before any of this.
						"lodging_grant": string(room.StructureID),
					},
					SpeakerName: speakerName,
					HuddleID:    lodger.CurrentHuddleID,
					Source:      "engine",
				})

				// Credit the live tally beside the durable row, the way the cascade
				// subscribers do — co-locating the two writes is what makes the live
				// record and a post-restart seed agree on which events are payments
				// and how each is classified (LLM-615). Without this the tally missed
				// the charge until the next boot and held it afterwards, so the pair
				// answered differently depending on uptime.
				//
				// ForGoods because a night's occupancy was delivered: the grant
				// extension above IS that delivery, committed in the same command as
				// the debit. A stay bought by hand settles through the pay ledger and
				// already classifies ForGoods, so the same purchase must not read two
				// ways because the backstop settled it instead of a negotiation.
				w.RecordCoinPaid(lodgerID, keeperID, nightly, now, CoinPaymentForGoods)

				result.Renewals = append(result.Renewals, RebookLodgerRecord{
					LodgerID:     lodgerID,
					KeeperID:     keeperID,
					StructureID:  room.StructureID,
					Nightly:      nightly,
					NewExpiresAt: newExpires,
				})
			}
			return result, nil
		},
	}
}

// soonestActiveLedgerGrant returns the actor's active ledger RoomAccess with
// the nearest future expiry, or nil when they hold none. The renewal targets
// the soonest grant — the one about to lapse first.
func soonestActiveLedgerGrant(a *Actor, now time.Time) *RoomAccess {
	if a == nil {
		return nil
	}
	var best *RoomAccess
	for _, ra := range a.RoomAccess {
		if !IsActiveLedgerGrant(ra, now) {
			continue
		}
		// Tie-break equal expiries by RoomID so the selection is deterministic
		// across the map's randomized iteration order — otherwise two grants with
		// identical ExpiresAt could resolve to different rooms on different reads,
		// and the wind-down cue (perception lodgerInn) could disagree with this
		// warrant on which inn to steer toward (ZBBS-WORK-387).
		if best == nil || ra.ExpiresAt.Before(*best.ExpiresAt) ||
			(ra.ExpiresAt.Equal(*best.ExpiresAt) && ra.RoomID < best.RoomID) {
			best = ra
		}
	}
	return best
}

// lodgerCoveredBeyond reports whether the actor holds an active ledger grant
// expiring strictly later than exp — i.e. a later grant already covers the
// window the renewal would add, so renewing the soonest grant is redundant.
func lodgerCoveredBeyond(a *Actor, now time.Time, exp time.Time) bool {
	for _, ra := range a.RoomAccess {
		if !IsActiveLedgerGrant(ra, now) {
			continue
		}
		if ra.ExpiresAt.After(exp) {
			return true
		}
	}
	return false
}

// keeperForStructure returns the actor working at structureID (its keeper),
// or ("", nil) when none. When several actors work there (unusual), the
// lexicographically smallest ID is chosen so the resolved keeper — and thus
// the coin credit — is deterministic across runs (w.Actors is a map). Mirrors
// perception.keeperOf, on the live world.
func keeperForStructure(w *World, structureID StructureID) (ActorID, *Actor) {
	var bestID ActorID
	var best *Actor
	for id, a := range w.Actors {
		if a == nil || a.WorkStructureID != structureID {
			continue
		}
		if best == nil || id < bestID {
			bestID = id
			best = a
		}
	}
	return bestID, best
}
