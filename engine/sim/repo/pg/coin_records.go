package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// CoinRecordsRepo seeds the per-pair coin tally (LLM-572) from agent_action_log.
//
// READ-ONLY — there is no coin_record table and nothing to checkpoint. The tally is
// derived at boot from the durable rows the engine already writes on every
// coin-moving settlement (`paid` and `labored`), which is what lets the mechanism
// survive a restart without a schema change of its own. See engine/sim/coin_record.go
// for why that was chosen over a checkpointed aggregate.
type CoinRecordsRepo struct {
	pool Pool
}

// NewCoinRecordsRepo constructs a CoinRecordsRepo against the given pool. Normal
// wiring is pg.NewRepository.
func NewCoinRecordsRepo(pool Pool) *CoinRecordsRepo {
	return &CoinRecordsRepo{pool: pool}
}

// loadPaymentsSinceSQL pulls the settled coin payments inside the window.
//
// All three `paid` writers — handlePaidActionLog (bare coin),
// handlePayResolvedActionLog (a pay-with-item ledger entry settling ACCEPTED) and
// lodger_rebook (the nightly lodging auto-charge) — store the same
// {recipient, amount} payload keys. `labored` (LLM-613) is the fourth row type that
// moves coin: handleLaborResolvedActionLog appends one when a labor contract settles
// Completed, which is the only terminal that pays.
//
// THE TWO SHAPES ARE DIRECTION-INVERTED and this query does NOT normalize that — it
// returns action_type and the caller decides. A `paid` row's actor_id is the payer;
// a `labored` row's is the worker, i.e. the payee. Resolving it here would mean a
// CASE per column and would put the rule in SQL, where sim.CoinPaymentSource
// documents it in Go beside the code that acts on it.
//
// The counterparty is selected by ACTION TYPE rather than by coalescing the two
// shapes' key names. Coalescing would read correctly today — of 3,708 live rows, none
// carries both a `recipient` and an `employer` key — but it makes correctness rest on
// a property of the writers rather than of this query, and the failure would be
// silent: a future writer that put both on one payload would have every wage
// attributed to the recipient instead. The row type is the authority on which key
// means what, the same principle sim.CoinPaymentSource applies to the direction
// (code_review). Both id keys are forward-only — recipient_actor_id since LLM-572 and
// on the lodging path only since LLM-615, employer_actor_id since LLM-613 — and the
// caller falls back to the display name for older rows of either shape.
//
// The amount comes back as TEXT and is parsed in Go rather than cast here. The
// column is jsonb, so a single malformed value would fail the cast and take the
// whole boot query with it — and Postgres gives no ordering guarantee that would
// let a WHERE clause filter the bad row out before the SELECT list evaluates it.
// A barter settles for 0 coin and still writes a `paid` row, so the caller drops
// non-positive amounts: this is a record of coin, and no coin passed.
//
// rate_settled (LLM-607) is present only on rows whose coin discharged town-rate
// arrears, and only since that ticket. Absent on every purchase and on every
// historical row, both of which read back as an ordinary payment — the same
// forward-only shape recipient_actor_id has. Read as TEXT for the reason amount is.
//
// ledger_id is a goods marker (LLM-612) and is NOT forward-only: it has been
// stamped unconditionally by handlePayResolvedActionLog since LLM-105, so it is on
// every ledger-settled row in the table's history. Only its presence is read, so it
// comes back as TEXT and is never parsed — which also means a value of any shape
// (the column has held a bare integer throughout, but nothing here depends on that)
// cannot fail the boot query.
//
// lodging_grant is the other goods marker (LLM-615), on rows where the rebook took
// the night's rate and extended the room grant together. Forward-only like
// rate_settled, so the lodging rows already in the table read back Unstated — which
// is what the live tally holds for them too, since the same ticket added the call
// that credits it. Presence-only and never parsed, for the reason ledger_id is.
//
// result = 'ok' excludes rejected/failed/declined/countered attempts — nothing
// moved on those. actor_id NOT NULL excludes the engine-authored rows that carry no
// payer.
//
// NOT (payload ? 'estate_rate') excludes the estate-rate assessments (LLM-652):
// coin the engine moved from a purse into the town chest. The chest is not an
// actor, so the row has no counterparty to pair with, and letting it through would
// only count it as an unresolved counterparty at boot — a bucket the header
// reserves for departed visitors. Keyed on the marker's PRESENCE (the jsonb `?`
// operator), not its text value, so a marked row whose value is null is still an
// assessment and never a peer payment. The marker is engine-owned (estate_rate.go
// writes it and nothing else does), the same footing as ledger_id and
// lodging_grant.
//
// No index is needed or added: the scan is bounded by the occurred_at window and
// runs exactly once, at boot.
const loadPaymentsSinceSQL = `
SELECT action_type,
       actor_id,
       occurred_at,
       COALESCE(payload->>'amount', ''),
       COALESCE(CASE WHEN action_type = 'labored' THEN payload->>'employer_actor_id'
                     ELSE payload->>'recipient_actor_id' END, ''),
       COALESCE(CASE WHEN action_type = 'labored' THEN payload->>'employer'
                     ELSE payload->>'recipient' END, ''),
       COALESCE(payload->>'rate_settled', ''),
       COALESCE(payload->>'ledger_id', ''),
       COALESCE(payload->>'lodging_grant', '')
  FROM agent_action_log
 WHERE action_type IN ('paid', 'labored')
   AND result = 'ok'
   AND actor_id IS NOT NULL
   AND NOT (payload ? 'estate_rate')
   AND occurred_at >= $1
 ORDER BY occurred_at`

// LoadPaymentsSince reads the settled payments at or after `since`, oldest-first.
// Read-only restart path off the pool, the same posture as the LoadAll
// implementations.
//
// Deliberately does no resolution, filtering or validation beyond the query: the
// caller (FinalizeLoad, via rehydrateCoinRecordOnLoad) owns the clock, the actor
// index and the ambiguity rule, so those live in one testable place without a
// database.
func (r *CoinRecordsRepo) LoadPaymentsSince(ctx context.Context, since time.Time) ([]sim.CoinPaymentRow, error) {
	rows, err := r.pool.Query(ctx, loadPaymentsSinceSQL, since)
	if err != nil {
		return nil, fmt.Errorf("pg coin records LoadPaymentsSince query: %w", err)
	}
	defer rows.Close()

	var out []sim.CoinPaymentRow
	for rows.Next() {
		var (
			actionType       string
			actorID          string
			at               time.Time
			amount           string
			counterpartyID   string
			counterpartyName string
			rateSettled      string
			ledgerID         string
			lodgingGrant     string
		)
		if err := rows.Scan(&actionType, &actorID, &at, &amount, &counterpartyID, &counterpartyName, &rateSettled, &ledgerID, &lodgingGrant); err != nil {
			return nil, fmt.Errorf("pg coin records LoadPaymentsSince scan: %w", err)
		}
		// The predicate admits exactly these two, so anything else is unreachable;
		// defaulting to the `paid` shape rather than switching on it keeps an
		// unexpected type from silently inverting a direction.
		source := sim.CoinPaymentSourcePaid
		if actionType == string(sim.ActionTypeLabored) {
			source = sim.CoinPaymentSourceLabored
		}
		out = append(out, sim.CoinPaymentRow{
			Source:              source,
			ActorID:             sim.ActorID(actorID),
			At:                  at,
			Amount:              amount,
			CounterpartyActorID: counterpartyID,
			CounterpartyName:    counterpartyName,
			RateSettled:         rateSettled,
			LedgerID:            ledgerID,
			LodgingGrant:        lodgingGrant,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg coin records LoadPaymentsSince iter: %w", err)
	}
	return out, nil
}
