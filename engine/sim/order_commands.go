package sim

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// order_commands.go — Phase 3 PR S6 commands for the post-acceptance
// fulfillment state machine.
//
// Two flows:
//
//   - createOrderForPayWithItem — internal helper called from
//     commitPayTransfer's !ConsumeNow branch when AcceptPay commits a
//     take-home offer. Mints the Order, emits OrderCreated.
//
//   - DeliverOrder — exposed Command for the deliver_order tool. Runs
//     the 7-gate validation matrix (existence + auth + state + TTL +
//     seller-stock + co-presence + catalog), atomically transfers
//     goods to each consumer, writes bidirectional buyer↔seller
//     SalientFacts, and flips the Order to Delivered.

// MaxOrderReadyInDays caps how far ahead a lodging booking can be placed.
// A month is plenty for the village's cadence and it bounds the derived
// ExpiresAt horizon, so a fat-fingered ready_in_days can't park an order
// Ready for years. ZBBS-HOME-403.
const MaxOrderReadyInDays = 30

// resolveOrderReadyBy computes an order's booked date from the buyer's
// ready_in_days offset, returning midnight UTC of the resolved calendar date
// (orderDateUTC). Rules (ZBBS-HOME-403):
//
//   - days <= 0 → carry the parent's booked date when this is a
//     counter-response (a price haggle never moves the date the buyer asked
//     for), else today (immediate / same-day — the default for every
//     non-lodging order and for lodging booked for tonight). ready_in_days
//     decodes to 0 whether omitted or explicitly today, so a haggle that
//     leaves it out preserves the original booking. A carried FUTURE date is
//     still subject to the lodging-only rule, so a counter can't swap a
//     lodging booking for a non-lodging item and keep the future date.
//   - days > 0 → advance booking, lodging-only, and it overrides any parent
//     date (the buyer is re-specifying terms, as in_response_to already
//     allows for qty/item). A physical good is handed over when paid for, so
//     a future date on a non-lodging item would just strand the order at
//     Ready until it expired; reject it up front with a model-legible reason.
//     Capped at MaxOrderReadyInDays.
//
// MUST be called from inside a Command.Fn (reads w.PayLedger / w.Settings).
func resolveOrderReadyBy(w *World, kind ItemKind, parentID LedgerID, days int, at time.Time) (time.Time, error) {
	today := orderDateUTC(at, w.Settings.Location)
	if days <= 0 {
		if parentID != 0 {
			if parent := w.PayLedger[parentID]; parent != nil && !parent.ReadyBy.IsZero() {
				// A carried FUTURE date is an advance booking and is lodging-only,
				// same as a fresh ready_in_days — so a counter that swaps a lodging
				// booking for a non-lodging item can't inherit the future date. A
				// same-day-or-past carried date is harmless on any item.
				if parent.ReadyBy.After(today) && !itemHasCapability(w, kind, "lodging") {
					return time.Time{}, fmt.Errorf(
						"ready_in_days is only for booking lodging ahead — %s is delivered when you pay for it.",
						kind,
					)
				}
				return parent.ReadyBy, nil
			}
		}
		return today, nil
	}
	if !itemHasCapability(w, kind, "lodging") {
		return time.Time{}, fmt.Errorf(
			"ready_in_days is only for booking lodging ahead — %s is delivered when you pay for it (drop ready_in_days).",
			kind,
		)
	}
	if days > MaxOrderReadyInDays {
		return time.Time{}, fmt.Errorf(
			"ready_in_days is too far ahead (got %d, max %d).",
			days, MaxOrderReadyInDays,
		)
	}
	return today.AddDate(0, 0, days), nil
}

// advancePastHeldLodging advances a lodging booking's ready_by PAST the buyer's
// latest active lodging coverage at this seller, so a "renewal" extends the stay
// instead of re-booking a night already held (LLM-47). A renew for tonight when
// the buyer already has tonight would otherwise mint a second nights_stay for the
// same (buyer, seller, ready_by); delivering it violates the
// pay_ledger_lodging_active_once unique index and wedges every checkpoint (the
// 2026-06-19 Ezekiel↔John Ellis incident).
//
// Coverage is read from the buyer's durable RoomAccess grants — NOT w.Orders. A
// delivered lodging order is pruned from w.Orders at delivery (finalizeOrderTerminal
// + the terminal sink, cmd/engine/main.go) and is never reloaded after a restart
// (pg LoadAll filters to ready/pending), so it is absent from w.Orders exactly when
// a renewal needs to see it — an earlier w.Orders scan here was a no-op in
// production. The RoomAccess ledger grant is the persistent lodging relationship
// and is present.
//
// Target = the latest ExpiresAt across the buyer's active ledger grants at one of
// THIS seller's own private rooms, mapped to its calendar checkout date (a grant
// runs through the night before its check-out morning, so that date is the first
// night past the held block). This is "append after the latest held checkout", NOT
// a gap-filling "first free night" walk: lodging coverage is contiguous in practice
// (AssignBedroomForLodger EXTENDS a single grant in place rather than adding
// per-night rows), and RoomAccess carries only ExpiresAt, no per-night spans to
// walk. That contract is sufficient for the invariant this protects — the new
// ready_by is strictly past every existing delivered (buyer, seller, ready_by) for
// this seller, so it can never duplicate one (it may, harmlessly, skip a free gap
// night in the rare non-contiguous case). Matches work's lodgingRenewalReadyBy, the
// sibling shipped in #505. Returns readyBy unchanged when the buyer holds nothing
// here or the requested night is already past coverage (an explicit future booking
// is never pushed further out). MUST run on the world goroutine.
func advancePastHeldLodging(w *World, buyerID, sellerID ActorID, readyBy, at time.Time) time.Time {
	seller := w.Actors[sellerID]
	if seller == nil || seller.WorkStructureID == "" {
		return readyBy
	}
	structure := w.Structures[StructureID(seller.WorkStructureID)]
	if structure == nil {
		return readyBy
	}
	buyer := w.Actors[buyerID]
	if buyer == nil {
		return readyBy
	}
	// RoomID is globally unique (legacy BIGSERIAL; ux_room_access_one_private_active
	// keys on room_id alone), so membership in THIS structure's private-room set
	// correctly scopes a grant to this seller's inn — a grant the buyer holds at
	// another inn has a RoomID absent from the set. (Same scoping buildKeeperHeldLodgers uses.)
	privateRooms := make(map[RoomID]bool)
	for _, r := range structure.Rooms {
		if r != nil && r.Kind == RoomKindPrivate {
			privateRooms[r.ID] = true
		}
	}
	var latest time.Time
	for _, ra := range buyer.RoomAccess {
		if ra == nil || !privateRooms[ra.RoomID] || !IsActiveLedgerGrant(ra, at) {
			continue
		}
		if ra.ExpiresAt.After(latest) {
			latest = *ra.ExpiresAt
		}
	}
	if latest.IsZero() {
		return readyBy
	}
	nextNight := orderDateUTC(latest, w.Settings.Location)
	if nextNight.After(readyBy) {
		return nextNight
	}
	return readyBy
}

// createOrderForPayWithItem mints a new Order for a !ConsumeNow
// pay-with-item acceptance. Called from commitPayTransfer on the
// world goroutine. Returns the new OrderID.
//
// ConsumerIDs is normalized at creation — when the PayLedger entry
// had no explicit consumers, we populate ConsumerIDs with [buyerID]
// (matches the architecture-lock "buyer is the consumer when
// implicit" semantic and simplifies downstream code).
//
// Goods do NOT transfer here — that's the deliver_order tool's job.
// Stock stays in the seller's inventory until DeliverOrder fires its
// transferItem call per consumer.
func createOrderForPayWithItem(w *World, entry *PayLedgerEntry, at time.Time) OrderID {
	consumers := append([]ActorID(nil), entry.ConsumerIDs...)
	if len(consumers) == 0 {
		consumers = []ActorID{entry.BuyerID}
	}
	// ReadyBy defaults to the creation date for a same-day / immediate order;
	// a lodging advance booking carries a future date stamped at intake
	// (ZBBS-HOME-403). The lodging-aware ExpiresAt derives from it so a future
	// booking survives until it is actually due rather than expiring on the
	// 10-minute takeaway TTL.
	readyBy := entry.ReadyBy
	if readyBy.IsZero() {
		readyBy = orderDateUTC(at, w.Settings.Location)
	}
	// Order.ID IS the pay_ledger row id (== LedgerID): an Order is its
	// durable pay_ledger row, 1:1. Adopt entry.ID rather than a separate
	// per-run counter so Order.ID == LedgerID — the domain invariant the
	// checkpoint enforces (pg orders SaveSnapshot) and the same id the load
	// path keys on — which makes every persistence write target the correct
	// row. ZBBS-HOME-394.
	id := OrderID(entry.ID)
	o := &Order{
		ID:       id,
		State:    OrderStateReady,
		BuyerID:  entry.BuyerID,
		SellerID: entry.SellerID,
		Item:     entry.ItemKind,
		Qty:      entry.Qty,
		Amount:   entry.Amount,
		// LLM-493: carried purely so it reaches pay_ledger.pay_items on the
		// checkpoint upsert, which is what lets the boot price seed apply the same
		// barter guard as the live subscriber. Cloned, not aliased — the entry is
		// still live in w.PayLedger until the reaper takes it.
		PayItems:    cloneItemKindQtys(entry.PayItems),
		ConsumerIDs: consumers,
		LedgerID:    entry.ID,
		CreatedAt:   at,
		ReadyBy:     readyBy,
		ExpiresAt:   orderExpiresAt(w, entry.ItemKind, readyBy, at),
	}
	w.Orders[id] = o
	w.emit(&OrderCreated{
		OrderID:     id,
		BuyerID:     o.BuyerID,
		SellerID:    o.SellerID,
		Item:        o.Item,
		Qty:         o.Qty,
		ConsumerIDs: append([]ActorID(nil), consumers...),
		Amount:      o.Amount,
		LedgerID:    o.LedgerID,
		At:          at,
	})
	return id
}

// DeliverOrder returns a Command that finalizes an Order: validates
// the 7-gate matrix, transfers goods to each consumer, writes
// SalientFacts, flips state to Delivered, and emits OrderDelivered.
//
// Validation gates (first-failure-wins):
//
//  1. Order exists.
//  2. Auth: caller == Order.SellerID.
//  3. State: Order.State == OrderStateReady (idempotent rejects on
//     Delivered/Expired).
//  4. Live TTL: if at >= Order.ExpiresAt, flip Ready→Expired in-band
//     and reject. Mirrors PayLedger's accept_pay TTL gate (PR S4).
//  5. Seller stock: seller.Inventory[Item] >= Qty * len(ConsumerIDs).
//     Should always hold since accept reserved goods by NOT moving
//     them; defensive against the seller having consumed/transferred
//     their own stock between accept and deliver.
//  6. Co-presence per consumer: each ConsumerID shares CurrentHuddleID
//     with the seller. Take-home implies the recipient is present to
//     receive; if a consumer wandered off, the Order stays Ready and
//     the seller can retry when they come back (or the safety-net
//     sweep eventually expires).
//  7. ItemKind catalog: World.ItemKinds[Item] != nil. Defensive
//     against catalog deprecation between accept and deliver.
//
// On all-gate-pass: atomic transfer (one transferItem per consumer),
// bidirectional buyer↔seller InteractionDelivered/InteractionReceived
// facts, terminal flip + OrderDelivered emit. Multi-consumer group
// orders are all-or-none — if any transferItem call fails (which
// should never happen post-gate-5; this is defensive), the partial
// transfers ALREADY committed for earlier consumers remain (no
// rollback). That's a substrate bug, not a domain failure.
//
// On gate failure: Order is left at OrderStateReady (gates 1-3, 5-7)
// or transitions to OrderStateExpired (gate 4 only). Returns a
// descriptive error for the tool dispatcher to surface to the LLM.
func DeliverOrder(sellerID ActorID, orderID OrderID, at time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Gate 1: existence.
			o, ok := w.Orders[orderID]
			if !ok || o == nil {
				return nil, fmt.Errorf("deliver_order: order %d not found", orderID)
			}

			// Gate 2: auth.
			if o.SellerID != sellerID {
				return nil, fmt.Errorf("deliver_order: order %d belongs to %q, not %q", orderID, o.SellerID, sellerID)
			}

			// Gate 3: state.
			switch o.State {
			case OrderStateDelivered:
				return nil, fmt.Errorf("deliver_order: order %d already delivered", orderID)
			case OrderStateExpired:
				return nil, fmt.Errorf("deliver_order: order %d already expired", orderID)
			case OrderStateReady:
				// fall through
			default:
				return nil, fmt.Errorf("deliver_order: order %d in state %q, expected %q", orderID, o.State, OrderStateReady)
			}

			// Gate 4: live TTL. Sweep cadence is 60s but this gate
			// catches the gap between TTL elapsing and the next sweep.
			// Flip in-band like PayLedger's accept_pay does.
			if !o.ExpiresAt.IsZero() && !at.Before(o.ExpiresAt) {
				finalizeOrderTerminal(w, o, OrderStateExpired, at)
				return nil, fmt.Errorf("deliver_order: order %d expired at %v", orderID, o.ExpiresAt)
			}

			// Gate 4b: advance booking not yet due (ZBBS-HOME-403). A lodging
			// order booked for a future date can't be checked in early. Enforced
			// here, not only in perception — a direct/tool deliver_order with the
			// id would otherwise grant the room before the guest's booked date.
			// Same-day bookings (ReadyBy == today) pass; the immediate take-home
			// path never reaches DeliverOrder.
			today := orderDateUTC(at, w.Settings.Location)
			if !o.ReadyBy.IsZero() && o.ReadyBy.After(today) {
				return nil, fmt.Errorf("deliver_order: order %d is booked for %s — not ready to deliver yet",
					orderID, o.ReadyBy.Format("2006-01-02"))
			}

			// Defensive Order-shape gates (PR S6 R1 code_review fix):
			// invalid Qty / empty ConsumerIDs / overflow can come from
			// future repo loads or test hooks; reject before the stock
			// multiplication so a zero-consumer order doesn't deliver
			// nothing-but-stamp-delivered.
			if o.Qty <= 0 {
				return nil, fmt.Errorf("deliver_order: order %d has invalid quantity %d", orderID, o.Qty)
			}
			if len(o.ConsumerIDs) == 0 {
				return nil, fmt.Errorf("deliver_order: order %d has no consumers", orderID)
			}
			if o.Qty > math.MaxInt/len(o.ConsumerIDs) {
				return nil, fmt.Errorf("deliver_order: order %d total quantity overflows int (qty=%d, consumers=%d)",
					orderID, o.Qty, len(o.ConsumerIDs))
			}

			// Gate 5: seller exists + stock. The stock check is skipped for
			// "service"-capability items (e.g. nights_stay) — they carry no
			// inventory (ZBBS-HOME-296; mirrors the pay_with_item gate-10
			// skip). The seller-existence check always runs.
			seller, ok := w.Actors[sellerID]
			if !ok || seller == nil {
				return nil, fmt.Errorf("deliver_order: seller %q not found", sellerID)
			}
			if !itemHasCapability(w, o.Item, "service") {
				requiredQty := o.Qty * len(o.ConsumerIDs)
				if seller.Inventory[o.Item] < requiredQty {
					return nil, fmt.Errorf("deliver_order: seller %q has %d %s, need %d for order %d",
						sellerID, seller.Inventory[o.Item], o.Item, requiredQty, orderID)
				}
			}

			// Gate 6: co-presence per consumer. Also resolve each
			// consumer pointer once so we don't re-look-up in the
			// transfer loop.
			consumers := make([]*Actor, 0, len(o.ConsumerIDs))
			for _, cid := range o.ConsumerIDs {
				consumer, ok := w.Actors[cid]
				if !ok || consumer == nil {
					return nil, fmt.Errorf("deliver_order: consumer %q not found", cid)
				}
				if seller.CurrentHuddleID == "" {
					return nil, fmt.Errorf("deliver_order: seller %q not in a huddle (cannot deliver)", sellerID)
				}
				if consumer.CurrentHuddleID == "" || consumer.CurrentHuddleID != seller.CurrentHuddleID {
					return nil, fmt.Errorf("deliver_order: consumer %q not co-present with seller", cid)
				}
				consumers = append(consumers, consumer)
			}

			// Gate 7: ItemKind catalog.
			if w.ItemKinds[o.Item] == nil {
				return nil, fmt.Errorf("deliver_order: item %q no longer in catalog", o.Item)
			}

			// Gate 8: the outstanding balance is affordable (LLM-357). A
			// partial-payment commission collected only a deposit at accept; the
			// balance (orderBalanceDue) is settled below as an atomic coin↔goods
			// swap once the goods move. The BUYER funds it, so they must be
			// co-present with the seller and hold the coins — a short or absent
			// buyer bounces the delivery HERE, before any goods move, and the
			// order rides to expiry (forged-but-uncollected → deposit forfeit).
			// Zero for every full-prepay order, so this is a no-op there.
			var balanceBuyer *Actor
			balanceDue := orderBalanceDue(o)
			if balanceDue > 0 {
				buyer, ok := w.Actors[o.BuyerID]
				if !ok || buyer == nil {
					return nil, fmt.Errorf("deliver_order: buyer %q not found to settle the %d-coin balance on order %d", o.BuyerID, balanceDue, orderID)
				}
				if buyer.CurrentHuddleID == "" || buyer.CurrentHuddleID != seller.CurrentHuddleID {
					return nil, fmt.Errorf("deliver_order: buyer %q is not here to pay the %d-coin balance on order %d", o.BuyerID, balanceDue, orderID)
				}
				if buyer.SpendableCoins() < balanceDue {
					return nil, fmt.Errorf("deliver_order: buyer %q has %d coins to spend, needs %d to settle the balance on order %d", o.BuyerID, buyer.SpendableCoins(), balanceDue, orderID)
				}
				if seller.Coins > math.MaxInt-balanceDue {
					return nil, fmt.Errorf("deliver_order: settling %d would overflow seller %q coins on order %d", balanceDue, sellerID, orderID)
				}
				balanceBuyer = buyer
			}

			// Fulfillment — shared with the ZBBS-HOME-398 immediate-handover
			// path (commitPayTransfer). transferOrderGoods grants the lodging
			// room or moves the goods to each consumer. Co-presence (gate 6),
			// catalog (gate 7), the SalientFacts below, and finalizeOrderTerminal
			// stay on this deferred-delivery caller.
			if err := transferOrderGoods(w, o, seller, consumers, at); err != nil {
				return nil, err
			}

			// LLM-357: the goods are handed over — settle the outstanding
			// balance now (validated affordable in gate 8; the world is
			// single-goroutine, so the buyer's coins can't have moved since).
			// The deposit taken at accept plus this balance equal the full price.
			if balanceDue > 0 && balanceBuyer != nil {
				balanceBuyer.Coins -= balanceDue
				seller.Coins += balanceDue
				drawVisitorSpend(balanceBuyer, balanceDue)
				// LLM-411: the balance leg wears on its proportional share of the
				// sale's margin over the seller's cost basis — the deposit leg took
				// its share back at accept, so the two together tax the margin once.
				accrueStallWear(w, seller, saleWear{
					Lines:     []QuoteLine{{ItemKind: o.Item, Qty: o.Qty}},
					Consumers: len(o.ConsumerIDs),
					Amount:    o.Amount,
					Charge:    balanceDue,
				}, at)
			}

			// Bidirectional buyer↔seller SalientFacts. Multi-consumer
			// per-consumer writes intentionally omitted (matches the
			// posture from S4's commitPayTransfer).
			buyer := w.Actors[o.BuyerID]
			buyerName, sellerName := string(o.BuyerID), string(o.SellerID)
			if buyer != nil {
				buyerName = buyer.DisplayName
			}
			if seller != nil {
				sellerName = seller.DisplayName
			}
			sellerFact := orderDeliveredFactText(buyerName, o.Item, o.Qty, len(o.ConsumerIDs), false)
			buyerFact := orderDeliveredFactText(sellerName, o.Item, o.Qty, len(o.ConsumerIDs), true)
			// LLM-357: on a partial-payment commission, record that the balance was
			// settled on collection so both memories carry the whole deal, not just
			// the handover.
			if balanceDue > 0 {
				sellerFact += fmt.Sprintf(" They settled the %d-coin balance on collection.", balanceDue)
				buyerFact += fmt.Sprintf(" I settled the %d-coin balance on collection.", balanceDue)
			}
			if _, err := RecordInteraction(o.SellerID, o.BuyerID, InteractionDelivered, sellerFact, at).Fn(w); err != nil {
				log.Printf("sim.DeliverOrder: RecordInteraction seller→buyer: %v", err)
			}
			if _, err := RecordInteraction(o.BuyerID, o.SellerID, InteractionReceived, buyerFact, at).Fn(w); err != nil {
				log.Printf("sim.DeliverOrder: RecordInteraction buyer→seller: %v", err)
			}

			// Terminal flip + OrderDelivered emit.
			finalizeOrderTerminal(w, o, OrderStateDelivered, at)
			return nil, nil
		},
	}
}

// transferOrderGoods executes an Order's physical handover: it grants the
// lodging room for a "lodging"-capability item, or moves the goods to each
// consumer for an ordinary item. It does NOT flip the Order state, write
// SalientFacts, or run the deliver_order gate matrix — the caller owns those.
//
// Shared by two callers:
//   - DeliverOrder (the deferred deliver_order tool — lodging check-in and
//     future craft), after its 7-gate matrix and before its Delivered
//     SalientFacts + finalize. This is the only caller that exercises the
//     lodging branch below.
//   - fulfillTakeHomeOrderAtAccept (ZBBS-HOME-398 immediate handover), right
//     after a PHYSICAL order is minted at accept; the accept gate matrix
//     already validated co-presence and stock, so the error paths below are
//     defensive there (and the lodging branch is never reached via that path).
//
// consumers MUST be the resolved, non-nil *Actor pointers for o.ConsumerIDs,
// already validated co-present with the seller by the caller. MUST run on the
// world goroutine.
//
// Capability contract: lodging IMPLIES service (no inventory). A
// lodging-but-not-service item is a misconfigured catalog and is rejected
// loudly rather than treated as an unconsumed physical good.
func transferOrderGoods(w *World, o *Order, seller *Actor, consumers []*Actor, at time.Time) error {
	isLodging := itemHasCapability(w, o.Item, "lodging")
	if isLodging && !itemHasCapability(w, o.Item, "service") {
		return fmt.Errorf("order %d: item %q has the lodging capability without service — misconfigured catalog", o.ID, o.Item)
	}
	if isLodging {
		// Lodging grants the room to the BUYER; the lodger is bedded into it
		// (InsideRoomID) at the actual bed-down, NOT here at check-in (LLM-14).
		// The caller validated co-presence of the
		// CONSUMERS, so enforce the single-self-consumer scope: a
		// buyer-not-in-consumers (or multi-consumer) order would grant +
		// teleport an actor whose co-presence was never checked and strand the
		// listed consumers. Booking a room on another's behalf is out of scope.
		if len(o.ConsumerIDs) != 1 || o.ConsumerIDs[0] != o.BuyerID {
			return fmt.Errorf("order %d: lodging order must have the buyer as its sole consumer (buyer=%q consumers=%v)", o.ID, o.BuyerID, o.ConsumerIDs)
		}
		if seller.WorkStructureID == "" {
			return fmt.Errorf("order %d: keeper %q has no work structure to lodge in", o.ID, seller.ID)
		}
		// LLM-47 backstop: never deliver a nights_stay onto a night the buyer
		// already holds from this seller — a second delivered (buyer, seller,
		// ready_by) row violates pay_ledger_lodging_active_once and wedges every
		// checkpoint. The accept-time advance (advancePastHeldLodging in
		// PayWithItem) normally prevents this, so in the common path the night is
		// unchanged; this is defense-in-depth against any other path that mints a
		// same-night booking. No self-exclusion is needed: this order's own grant
		// is created just below by AssignBedroomForLodger, so at backstop time the
		// buyer's RoomAccess reflects only PRIOR grants. And it can't sneak in on a
		// retry — once transferOrderGoods succeeds (grant created) nothing fallible
		// runs before finalizeOrderTerminal (the RecordInteraction errors are
		// logged, not returned), so a granted order always terminalizes and is
		// never re-delivered. The advance also targets a FIXED checkout date, so it
		// is idempotent (no relative +1 night to double-apply).
		if adjusted := advancePastHeldLodging(w, o.BuyerID, o.SellerID, o.ReadyBy, at); !adjusted.Equal(o.ReadyBy) {
			log.Printf("sim/lodging: order %d ready_by advanced %s → %s to avoid double-booking a night %s already holds at %s",
				o.ID, o.ReadyBy.Format("2006-01-02"), adjusted.Format("2006-01-02"), o.BuyerID, seller.DisplayName)
			o.ReadyBy = adjusted
			// The checkpoint persists pay_ledger.ready_by from the Order
			// (repo/pg/orders.go), so o.ReadyBy is the value that reaches the DB;
			// keep the PayLedger read-model (perception, ledger readers) in step.
			if le := w.PayLedger[o.LedgerID]; le != nil {
				le.ReadyBy = adjusted
			}
		}
		// The grant runs for o.Qty nights from the booked date (ReadyBy), not
		// from the check-in instant — an advance booking checked in on its
		// ready_by date still gets the nights it paid for. ReadyBy == the
		// creation date for a same-day booking, so this is unchanged for the
		// common path. ZBBS-HOME-403.
		expiresAt := ComputeLodgerUntil(o.ReadyBy, o.Qty, w.Settings.LodgingCheckOutHour, w.Settings.Location)
		res, err := AssignBedroomForLodger(StructureID(seller.WorkStructureID), o.BuyerID, int64(o.LedgerID), expiresAt).Fn(w)
		if err != nil {
			if err == ErrNoPrivateRooms {
				return fmt.Errorf("order %d: %s has no bedrooms — not set up for lodging", o.ID, seller.DisplayName)
			}
			return fmt.Errorf("order %d: assign bedroom: %w", o.ID, err)
		}
		abr, _ := res.(AssignBedroomResult)
		if abr.RoomID == 0 {
			return fmt.Errorf("order %d: all bedrooms at %s are occupied — try again shortly", o.ID, seller.DisplayName)
		}
		return nil
	}
	// Mending (LLM-625): a service whose delivery restores the buyer's worn
	// garment units instead of transferring goods — the lodging shape. The
	// accept gate (gate 10c) pre-validated the mender's workplace, thread
	// stock, and that the buyer has something worn, so the error paths here
	// are defensive, mirroring the lodging branch's posture.
	if itemHasCapability(w, o.Item, CapabilityMending) {
		// Single self-consumer, like lodging: the buyer brings their own
		// clothes. Mending a third party's wardrobe is out of scope — their
		// wear counters were never validated co-present. The resolved actor is
		// checked against the id list too (code_review): this is the boundary
		// that protects settlement, and a malformed direct call could otherwise
		// validate the buyer's wear yet mend someone else.
		if len(o.ConsumerIDs) != 1 || o.ConsumerIDs[0] != o.BuyerID {
			return fmt.Errorf("order %d: mending order must have the buyer as its sole consumer (buyer=%q consumers=%v)", o.ID, o.BuyerID, o.ConsumerIDs)
		}
		if len(consumers) != 1 || consumers[0] == nil || consumers[0].ID != o.BuyerID {
			return fmt.Errorf("order %d: mending order resolved a consumer other than buyer %q", o.ID, o.BuyerID)
		}
		buyerActor := consumers[0]
		// The shared preconditions (service capability, mender workplace,
		// thread, worn garments) — the same predicate the accept gate and the
		// commit preflight ran, so an error here is defensive only.
		if err := ValidateMendingDelivery(w, seller, buyerActor, o.Item); err != nil {
			return fmt.Errorf("order %d: %w", o.ID, err)
		}
		mended := MendGarments(w.ItemKinds, buyerActor)
		if len(mended) == 0 {
			// Unreachable past ValidateMendingDelivery; kept so a mend that
			// touched nothing can never charge thread.
			return fmt.Errorf("order %d: %s has nothing worn to mend", o.ID, buyerActor.DisplayName)
		}
		seller.Inventory[MendThreadKind] -= MendThreadPerMend
		if seller.Inventory[MendThreadKind] <= 0 {
			delete(seller.Inventory, MendThreadKind)
		}
		return nil
	}
	// Equipment service (LLM-648): the mending arm's shape for the wright's
	// trade — validate, reset the buyer's due business, draw the whetstone.
	if itemHasCapability(w, o.Item, CapabilityEquipmentService) {
		if o.Qty != 1 {
			// The intake gates and commit preflight enforce qty 1; this is the
			// defensive delivery boundary — one visit resets one business and
			// draws one stone, so a multi-unit order must never settle here.
			return fmt.Errorf("order %d: equipment-service qty must be 1 (got %d)", o.ID, o.Qty)
		}
		if len(o.ConsumerIDs) != 1 || o.ConsumerIDs[0] != o.BuyerID {
			return fmt.Errorf("order %d: equipment-service order must have the buyer as its sole consumer (buyer=%q consumers=%v)", o.ID, o.BuyerID, o.ConsumerIDs)
		}
		if len(consumers) != 1 || consumers[0] == nil || consumers[0].ID != o.BuyerID {
			return fmt.Errorf("order %d: equipment-service order resolved a consumer other than buyer %q", o.ID, o.BuyerID)
		}
		buyerActor := consumers[0]
		if err := ValidateEquipmentServiceDelivery(w, seller, buyerActor, o.Item); err != nil {
			return fmt.Errorf("order %d: %w", o.ID, err)
		}
		serviced := ServiceEquipment(w, buyerActor.ID)
		if serviced == nil {
			// Unreachable past ValidateEquipmentServiceDelivery; kept so a
			// service that touched nothing can never charge a whetstone.
			return fmt.Errorf("order %d: %s has no business due for service", o.ID, buyerActor.DisplayName)
		}
		seller.Inventory[WhetstoneKind] -= WhetstonesPerService
		if seller.Inventory[WhetstoneKind] <= 0 {
			delete(seller.Inventory, WhetstoneKind)
		}
		return nil
	}
	// Ordinary goods. The atomic-commit contract requires every per-consumer
	// transfer to succeed or none to mutate state. Preflight the AGGREGATE
	// required stock (and nil consumers) BEFORE any mutation so a multi-consumer
	// order can't half-commit — transfer to the first consumer, then fail on a
	// later one. Both callers' gates already validate this (DeliverOrder gate 5;
	// pay-accept gate 10), so this makes the helper self-enforce the atomicity
	// contract rather than trust the caller. "service" items carry no inventory
	// (infinite stock) — skip their stock check, mirroring gate 5 (lodging,
	// which implies service, already returned above).
	n := len(consumers)
	if n == 0 {
		return fmt.Errorf("order %d: no consumers", o.ID)
	}
	for _, consumer := range consumers {
		if consumer == nil {
			return fmt.Errorf("order %d: nil consumer in preflight", o.ID)
		}
	}
	if !itemHasCapability(w, o.Item, "service") {
		if o.Qty <= 0 {
			return fmt.Errorf("order %d: invalid quantity %d", o.ID, o.Qty)
		}
		if o.Qty > math.MaxInt/n {
			return fmt.Errorf("order %d: aggregate quantity overflows int (qty=%d consumers=%d)", o.ID, o.Qty, n)
		}
		if required := o.Qty * n; seller.Inventory[o.Item] < required {
			return fmt.Errorf("order %d: insufficient stock for %d consumers (have %d, need %d)",
				o.ID, n, seller.Inventory[o.Item], required)
		}
	}
	for _, consumer := range consumers {
		if err := transferItem(w, seller, consumer, o.Item, o.Qty); err != nil {
			// A substrate invariant violation, not a domain failure — the
			// gates + preflight should have caught it.
			return fmt.Errorf("order %d: transferItem to %q: %w", o.ID, consumer.ID, err)
		}
	}
	return nil
}

// mintAndFulfillOrderNow mints the Order for an accepted !ConsumeNow offer and
// fulfills it in the SAME tick (ZBBS-HOME-398), returning the still-Ready Order
// for the caller (commitPayTransfer) to flip to Delivered AFTER writing the
// Paid/PaidBy facts — so OrderDelivered fires after the payment facts exist (a
// subscriber on OrderDelivered can assume the Paid facts are present).
// transferOrderGoods does the fulfillment and branches by capability: physical
// goods are handed to the buyer; a same-day walk-in room is granted via
// AssignBedroomForLodger (LLM-84). A FUTURE lodging reservation does NOT use this
// path — it stays a deferred book→check-in flow (createOrderForPayWithItem) — so
// the only lodging this sees is a same-day walk-in.
//
// Fulfilling at accept (no deferred deliver_order beat, no buyer-seller
// rendezvous) means the buyer can never be charged for goods or a room that then
// fail to materialize. The Order is still MINTED (not skipped) so that, once the
// caller flips it to Delivered, its durable pay_ledger row persists at the next
// checkpoint — that row is what the price-book restart seed reads
// (OrdersRepo.LoadRecentPrices, state='accepted' regardless of
// fulfillment_status). Skipping the Order entirely would silently drop
// cross-restart price memory. (A crash between accept and the next checkpoint
// loses this tick whole — the coin debit, the goods/room move, and the price
// observation together — matching the engine's transient-state-lossy /
// persistent-state-consistent crash model.)
//
// The accept gate matrix already validated co-presence, aggregate stock, and
// (for same-day lodging) room availability, so transferOrderGoods cannot fail in
// practice; a non-nil return is a substrate invariant violation surfaced to the
// caller, consistent with its other defensive-error returns. MUST run on the
// world goroutine.
func mintAndFulfillOrderNow(w *World, entry *PayLedgerEntry, seller *Actor, at time.Time) (*Order, error) {
	id := createOrderForPayWithItem(w, entry, at)
	o := w.Orders[id]
	if o == nil {
		return nil, fmt.Errorf("order %d: vanished immediately after mint", id)
	}
	consumers := make([]*Actor, 0, len(o.ConsumerIDs))
	for _, cid := range o.ConsumerIDs {
		c, ok := w.Actors[cid]
		if !ok || c == nil {
			return nil, fmt.Errorf("order %d: consumer %q not found at immediate handover", id, cid)
		}
		consumers = append(consumers, c)
	}
	if err := transferOrderGoods(w, o, seller, consumers, at); err != nil {
		return nil, err
	}
	return o, nil
}

// orderDeliveredFactText composes the InteractionDelivered /
// InteractionReceived fact text. Mirrors the v1 deliver narration
// pattern but keeps it minimal — item kind + qty + counterparty.
// For group orders (>1 consumer), the buyer-side fact omits the
// per-consumer breakdown; the buyer didn't necessarily watch each
// handover.
//
// buyerSide=true renders the buyer's view ("Hannah delivered me 2
// stew"); buyerSide=false renders the seller's view ("I delivered
// 2 stew to Jefferey"). For multi-consumer group orders, the
// seller's view says "...to Jefferey and 2 others" since the
// SalientFact lives on the buyer↔seller pair specifically.
func orderDeliveredFactText(counterpartyName string, item ItemKind, qty, consumerCount int, buyerSide bool) string {
	itemDesc := string(item)
	if qty > 1 {
		itemDesc = fmt.Sprintf("%d %s", qty, item)
	}
	if buyerSide {
		return fmt.Sprintf("%s delivered me %s.", counterpartyName, itemDesc)
	}
	// Seller side.
	var b strings.Builder
	fmt.Fprintf(&b, "I delivered %s to %s", itemDesc, counterpartyName)
	if consumerCount > 1 {
		fmt.Fprintf(&b, " and %d others", consumerCount-1)
	}
	b.WriteString(".")
	return b.String()
}

// recordExpiredDepositFacts writes the bidirectional relationship memory a
// partial-payment commission leaves when it expires (LLM-357): a forfeit
// (buyer-fault — the buyer never collected, so the seller kept the deposit and
// re-shelved the goods) or a refund (seller-fault — the seller never made it, so
// the deposit went back). The reputation seed both keepers carry forward. Only
// called for a genuine partial (0 < Deposit < Amount); full-prepay expiry stays
// as silent as it was before LLM-357. MUST run on the world goroutine.
func recordExpiredDepositFacts(w *World, o *Order, forfeited bool, at time.Time) {
	if o == nil {
		return
	}
	buyerName, sellerName := string(o.BuyerID), string(o.SellerID)
	if b := w.Actors[o.BuyerID]; b != nil {
		buyerName = b.DisplayName
	}
	if s := w.Actors[o.SellerID]; s != nil {
		sellerName = s.DisplayName
	}
	itemDesc := string(o.Item)
	if o.Qty > 1 {
		itemDesc = fmt.Sprintf("%d %s", o.Qty, o.Item)
	}
	// The fact is specifically about the deposit; read it straight off the order
	// rather than through orderAmountPaidAtAccept so the narration can't drift
	// from the actual sum with any future accept-time pricing change (they are
	// equal here — the caller guards 0 < Deposit < Amount). LLM-357.
	deposit := o.Deposit
	var sellerKind, buyerKind InteractionKind
	var sellerFact, buyerFact string
	if forfeited {
		sellerKind, buyerKind = InteractionKeptDeposit, InteractionForfeitedDeposit
		sellerFact = fmt.Sprintf("%s never came for the %s I made to order, so I kept their %d-coin deposit and put the goods back up for sale.", buyerName, itemDesc, deposit)
		buyerFact = fmt.Sprintf("I never collected the %s I commissioned from %s, and forfeited my %d-coin deposit.", itemDesc, sellerName, deposit)
	} else {
		sellerKind, buyerKind = InteractionRefundedDeposit, InteractionDepositRefunded
		sellerFact = fmt.Sprintf("I never made the %s %s commissioned, so their %d-coin deposit went back to them.", itemDesc, buyerName, deposit)
		buyerFact = fmt.Sprintf("%s never made the %s I commissioned, and my %d-coin deposit came back.", sellerName, itemDesc, deposit)
	}
	if _, err := RecordInteraction(o.SellerID, o.BuyerID, sellerKind, sellerFact, at).Fn(w); err != nil {
		log.Printf("sim.recordExpiredDepositFacts: seller->buyer order %d: %v", o.ID, err)
	}
	if _, err := RecordInteraction(o.BuyerID, o.SellerID, buyerKind, buyerFact, at).Fn(w); err != nil {
		log.Printf("sim.recordExpiredDepositFacts: buyer->seller order %d: %v", o.ID, err)
	}
}
