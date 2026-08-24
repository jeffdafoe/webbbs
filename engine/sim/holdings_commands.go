package sim

import "errors"

// holdings_commands.go — operator holdings adjustment (ZBBS-WORK-330).
//
// AdjustActorHoldings is the substrate for the umbilical /grant route: a SIGNED,
// additive give-or-take of coins and inventory items to/from ANY actor (PC or
// NPC). It is deliberately distinct from the editor admin surface
// (SetActorInventory, actor_admin.go) on three counts:
//
//   - it resolves the target from w.Actors directly, so it accepts PCs —
//     editableNPC rejects them by design (actor_admin.go);
//   - it applies SIGNED DELTAS (give and claw-back), not a whole-set replace;
//   - it touches coins, which no editor command does.
//
// Out-of-world: no narration, no WS frame, no in-world event — the operator just
// makes holdings appear/vanish. PCs see the change on the next /pc/me poll; NPCs
// in next-tick perception (inventory + coins already surface there). See
// shared/notes/codebase/salem-engine-v2/umbilical.

// ErrHoldingsUnderflow is returned by AdjustActorHoldings when a debit would push
// coins below zero or remove more of an item than the actor holds. The whole
// adjustment is rejected (validate-all-then-apply); nothing is applied.
var ErrHoldingsUnderflow = errors.New("holdings adjustment would underflow")

// ErrHoldingsOverflow is returned when a credit would wrap the int coin/quantity
// counter — a defensive guard against an operator-entered absurd (near-MaxInt)
// delta, not a normal-use path.
var ErrHoldingsOverflow = errors.New("holdings adjustment would overflow")

// ActorHoldingsResult echoes an actor's authoritative post-mutation holdings:
// the new coin balance and the full inventory as sorted rows (catalog SortOrder
// then item_kind, via sortInventoryRows — same ordering SetActorInventory uses).
type ActorHoldingsResult struct {
	ID    ActorID
	Coins int
	Rows  []ActorInventoryRow
}

// AdjustActorHoldings returns a Command that applies a signed coins delta and a
// set of signed per-item-kind deltas to actorID atomically. itemDeltas reuses
// ActorInventoryRow with Quantity carrying a SIGNED delta (positive = give,
// negative = claw back).
//
// Validation is all-or-nothing — every check runs against the CURRENT state
// before anything mutates, so a single bad row rejects the whole call and leaves
// the actor untouched (mirrors the validate-before-mutate discipline in
// transferItem / Consume):
//
//   - actorID resolves in w.Actors (ANY kind — PC or NPC); else ErrActorNotFound.
//   - each row's ItemKind resolves via resolveItemKind (case-insensitive + trim);
//     else ErrUnknownItemKind.
//   - a resolved kind appears at most once across rows; else ErrInvalidInventory
//     (two deltas for one kind is ambiguous — the caller should net them).
//   - no debit underflows: coins+delta >= 0, and held+delta >= 0 per item; else
//     ErrHoldingsUnderflow.
//   - no credit overflows the int counter; else ErrHoldingsOverflow.
//
// On success: coins are set to the new balance; each item delta is applied with
// the delete-on-zero invariant (an entry reaching 0 is removed, not left as a
// 0-count row). The result echoes the full post-mutation holdings.
func AdjustActorHoldings(id ActorID, coinsDelta int, itemDeltas []ActorInventoryRow) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			a, ok := w.Actors[id]
			if !ok {
				return nil, ErrActorNotFound
			}

			// --- validate coins ---
			newCoins, err := addChecked(a.Coins, coinsDelta)
			if err != nil {
				return nil, err
			}
			if newCoins < 0 {
				return nil, ErrHoldingsUnderflow
			}

			// --- validate item deltas into a proposed post-state ---
			// Resolve each row to a canonical kind, dup-detect on that kind, and
			// compute the post-mutation quantity. Collect into a change list so the
			// apply step runs only after every row passes.
			seen := make(map[ItemKind]bool, len(itemDeltas))
			type change struct {
				kind ItemKind
				qty  int // post-mutation quantity (>= 0, validated)
			}
			changes := make([]change, 0, len(itemDeltas))
			for _, row := range itemDeltas {
				kind, ok := resolveItemKind(w, row.ItemKind)
				if !ok {
					return nil, ErrUnknownItemKind
				}
				if seen[kind] {
					return nil, ErrInvalidInventory
				}
				seen[kind] = true
				newQty, err := addChecked(a.Inventory[kind], row.Quantity)
				if err != nil {
					return nil, err
				}
				if newQty < 0 {
					return nil, ErrHoldingsUnderflow
				}
				changes = append(changes, change{kind: kind, qty: newQty})
			}

			// --- all validated: apply atomically ---
			a.Coins = newCoins
			// LLM-644: an operator coin GRANT to a visitor is meant to be
			// spendable (the LLM-410 float precedent), so it raises the trip
			// budget alongside the wallet. A debit needs nothing — spendable
			// is min(wallet, budget), so the smaller wallet already caps it.
			if a.VisitorState != nil && coinsDelta > 0 {
				a.VisitorState.SpendBudget += coinsDelta
			}
			for _, c := range changes {
				if c.qty == 0 {
					delete(a.Inventory, c.kind) // delete-on-zero invariant
					continue
				}
				if a.Inventory == nil {
					a.Inventory = make(map[ItemKind]int)
				}
				a.Inventory[c.kind] = c.qty
			}

			rows := make([]ActorInventoryRow, 0, len(a.Inventory))
			for kind, qty := range a.Inventory {
				rows = append(rows, ActorInventoryRow{ItemKind: string(kind), Quantity: qty})
			}
			sortInventoryRows(w, rows)
			return ActorHoldingsResult{ID: id, Coins: a.Coins, Rows: rows}, nil
		},
	}
}

// addChecked returns a+b, or ErrHoldingsOverflow if the signed addition would
// wrap the int range in either direction. The legitimate "debit below a floor"
// case is NOT an overflow — that is caught by the >= 0 checks at the call site;
// addChecked guards only against pathological operator input wrapping the
// counter.
func addChecked(a, b int) (int, error) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, ErrHoldingsOverflow
	}
	return s, nil
}
