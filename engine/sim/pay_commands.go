package sim

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"
)

// MaxPayAmount is the upper bound on amount accepted by the Pay Command,
// mirroring the handler-side cap. Re-enforced inside the Command Fn because
// Pay is exported — non-handler callers (tests, admin paths, future
// in-engine cascades) could otherwise mint or overdraw coins.
const MaxPayAmount = math.MaxInt32

// Pay returns a Command that commits a coin transfer from buyerID to the
// huddle peer whose DisplayName matches recipientName (case-insensitive).
// Phase 3 PR B — the port of v1's `case "pay":` commit arm from
// agent_tick.go to the v2 in-memory substrate, scoped to **pure coin
// transfer**: no items, no qty, no consume_now, no consumers, no
// in_response_to, no deliberation tick. The mismatched-pay haggling chain
// + ledger + inventory port to later PRs alongside their substrate.
//
// Pre-conditions the caller (the pay handlers.CommitFn) normalizes but
// the Command Fn ALSO re-validates because Pay is exported — non-handler
// callers (tests, admin paths, future in-engine cascades) must not be
// able to mint coins via a negative amount or smuggle a no-op event via
// amount=0:
//
//   - recipientName trimmed, non-empty
//   - amount >= 1 and <= MaxPayAmount (re-checked here)
//   - forText trimmed; control-char-rejected; length <= MaxPayForChars
//
// World-state pre-conditions checked here:
//
//   - buyerID resolves to a real actor in w.Actors
//   - buyer.MoveIntent == nil (not walk-in-flight)
//   - buyer.CurrentHuddleID != "" (must be in a conversation)
//   - recipientName resolves to a single huddle peer (case-insensitive
//     DisplayName; ambiguity → reject)
//   - resolved seller != buyer (no self-pay)
//   - buyer.Coins >= amount (sufficient balance)
//   - seller.Coins + amount does not overflow int (balance overflow guard)
//
// On success:
//
//   - buyer.Coins -= amount, seller.Coins += amount
//   - emits Paid{BuyerID, SellerID, Amount, ForText, At}
//   - RecordInteraction(buyer, seller, InteractionPaid, "<text>", at)
//   - RecordInteraction(seller, buyer, InteractionPaidBy, "<text>", at)
//     (the KindNPCShared gate inside RecordInteraction filters which writes
//     actually persist — stateful-VA NPCs get pay continuity from their VA's
//     own memory; the engine-side gate silently no-ops for them)
//   - no warrants stamped here — the Paid event subscriber
//     (handlers/pay_reactor.go) mints a PaidWarrantReason warrant on the
//     seller
//
// Same-huddle gate (locked at PR B design walkthrough): pay is a
// transactional act between conversation participants. v1 lacked this gate
// (an artifact of an early "tip jar" framing that never materialized into a
// working flow), letting PCs tip a beggar from across the village. PR B
// fixes that — proximity matters for payments the same way it matters for
// speech.
func Pay(buyerID ActorID, recipientName string, amount int, forText string, at time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Re-validate amount inside the Command Fn — Pay is exported,
			// so non-handler callers (tests, admin paths) could otherwise
			// pass amount<=0 (mint coins via negative-amount underflow on
			// the buyer side) or amount>MaxInt32 (silent int32 wrap in
			// any future int32 ledger column). Both rejected at decode
			// for the handler path; defense in depth here.
			if amount < 1 {
				return nil, fmt.Errorf("Pay: amount must be at least 1 (got %d)", amount)
			}
			if amount > MaxPayAmount {
				return nil, fmt.Errorf("Pay: amount exceeds maximum (got %d, max %d)", amount, MaxPayAmount)
			}

			buyer, ok := w.Actors[buyerID]
			if !ok {
				return nil, fmt.Errorf("Pay: buyer %q not in world", buyerID)
			}
			if buyer.MoveIntent != nil {
				return nil, errors.New(
					"you are walking — finish your move before paying. " +
						"Either pay BEFORE the move_to, or wait until you arrive.",
				)
			}
			if buyer.CurrentHuddleID == "" {
				return nil, errors.New(
					"you're not in a conversation — start one with the person you want to pay first.",
				)
			}

			// Resolve seller against huddle peers only. Tighter than v1's
			// village-wide name lookup AND eliminates cross-village collisions
			// (two NPCs named "John" in different rooms can't be confused).
			// Ambiguity (two co-huddled peers with case-insensitive equal
			// DisplayName) → reject: money transfers must not pick a
			// recipient non-deterministically.
			sellerID, ok, ambiguous := findHuddlePeerByDisplayName(w, buyerID, buyer.CurrentHuddleID, recipientName)
			if ambiguous {
				return nil, fmt.Errorf(
					"more than one person named %q is in this conversation — use a unique full name before paying.",
					recipientName,
				)
			}
			if !ok {
				// Reroute a workplace name to the worker (ZBBS-HOME-460). The
				// buy cues name the structure ("buy from Ellis Farm"), so the
				// model passes the building where the tool wants the co-present
				// person. If exactly one peer here works at a structure by that
				// name, pay them rather than rejecting and forcing a retry.
				peerID, structureName, peerOK, peerAmbiguous := findHuddlePeerByWorkplaceName(w, buyerID, buyer.CurrentHuddleID, recipientName)
				if peerAmbiguous {
					return nil, fmt.Errorf(
						"more than one person here works at %q — name the person you want to pay.",
						recipientName,
					)
				}
				if !peerOK {
					return nil, fmt.Errorf(
						"no one named %q in this conversation — re-check who is here before paying.",
						recipientName,
					)
				}
				sellerID = peerID
				log.Printf("sim.Pay: rerouted payment from building %q to its worker %q (buyer %q)",
					structureName, peerID, buyerID)
			}
			if sellerID == buyerID {
				// Defensive — findHuddlePeerByDisplayName excludes the buyer
				// from the peer scan, so this can only fire if the peer set
				// invariant ever drifts. Cheap to keep.
				return nil, errors.New("you cannot pay yourself")
			}
			seller, ok := w.Actors[sellerID]
			if !ok {
				return nil, fmt.Errorf("Pay: seller %q vanished mid-resolve", sellerID)
			}

			// LLM-172: bare pay is a pure coin transfer — it does NOT settle a
			// scene quote or deliver the quoted good. When forText names a good the
			// seller has posted an active quote for, the buyer is almost certainly
			// trying to BUY it and reached for the wrong tool; left to proceed the
			// coins move but no item is delivered and the quote stays open, so the
			// seller re-offers and the buyer re-accepts (the live Ezekiel/John stew
			// loop). Redirect to the settlement tool. forText that names no active
			// quoted good (a tip, a debt, an unquoted item) falls through to a
			// plain transfer.
			if q := findCoinQuoteForPay(w, buyer, sellerID, forText, at); q != nil {
				if buyer.SpendableCoins() < q.Amount {
					// Coin-short for the quote: redirecting to pay_with_item would just
					// bounce on funds and loop with the right tool (code_review). Steer
					// to the real ways forward for a broke buyer — bargain the price
					// down, or barter goods via offer_trade — not a settlement they
					// can't cover.
					return nil, fmt.Errorf(
						"%s has an open quote for %s (quote_id %d, %d coins) but you only have %d to spend — a plain pay won't deliver the %s. Agree a lower coin price, or call offer_trade with goods you'll give and want_item %q.",
						seller.DisplayName, q.Lines[0].ItemKind, q.ID, q.Amount, buyer.SpendableCoins(),
						q.Lines[0].ItemKind, q.Lines[0].ItemKind,
					)
				}
				return nil, fmt.Errorf(
					"%s has an open quote for %s (quote_id %d, %d coins) — a plain pay only hands over coins and won't deliver the %s. Call pay_with_item with quote_id %d, item %q, qty %d, and amount %d to actually receive it.",
					seller.DisplayName, q.Lines[0].ItemKind, q.ID, q.Amount,
					q.Lines[0].ItemKind, q.ID, q.Lines[0].ItemKind, q.Lines[0].Qty, q.Amount,
				)
			}

			// LLM-649: a memo that names a BUNDLE of goods is a purchase the model
			// reached the wrong tool for, not a tip. The LLM-172 guard above
			// resolves the whole memo as one item, so it sees nothing in
			// "5x wheat, 3x flour, 2x firewood" and the pay fell through as a plain
			// transfer — coins moved, no goods did (live 2026-08-31: a factor paid
			// Josiah 26 and then 30 coins on two such memos, the second for goods
			// Josiah did not even hold). Two or more distinct goods, or one good
			// with an explicit count, is the purchase shape and is refused; a single
			// good with no count ("the ale you gave me") stays a debt memo.
			if goods, counted := goodsNamedInPayMemo(w, forText); len(goods) >= 2 || (len(goods) == 1 && counted) {
				return nil, fmt.Errorf(
					"a plain pay only hands over coins — it delivers none of the %s. Buy each good on its own: call pay_with_item with item, qty and amount; %s hands it over on accepting.",
					joinItemLabels(w, goods), seller.DisplayName,
				)
			}

			// LLM-177: lodging is never settled by a bare coin transfer — pay
			// moves coins but mints no Order and grants no RoomAccess, so "pay 4
			// for a room" leaves the guest with nowhere to sleep while the keeper
			// often reverses coins back trying to "complete" the deal (the live
			// Ezekiel/Hannah loop). The LLM-172 goods guard above misses this:
			// lodging phrasing ("a room for the night", "lodging") doesn't
			// canonicalize to the nights_stay item kind, so findCoinQuoteForPay
			// returns nil. Catch it here instead — on an open room offer between
			// the two (either direction, so it also catches a keeper reversing
			// coins to the guest) or on lodging vocabulary when no quote is posted
			// yet. A bare pay touching lodging is wrong whoever pays whom: the
			// guest rents with pay_with_item, the keeper grants the room by
			// accepting it. A PUBLIC room quote only counts as "this is a botched
			// room payment" when the pay text itself signals lodging (lodgingIntent)
			// — otherwise an unrelated tip that happens to coincide with an open
			// public room quote would be misread; a quote TARGETED at the
			// counterparty blocks any bare pay between them (a room deal is
			// unambiguously underway).
			lodgingIntent := isLodgingToken(forText) || isLodgingNightPhrase(forText)
			if lq := activeLodgingQuoteBetween(w, sellerID, buyerID, buyer.CurrentHuddleID, at, lodgingIntent); lq != nil {
				keeperName := string(lq.SellerID)
				if keeper := w.Actors[lq.SellerID]; keeper != nil && keeper.DisplayName != "" {
					keeperName = keeper.DisplayName
				}
				return nil, fmt.Errorf(
					"renting a room isn't a plain coin payment — it moves coins but grants no room. %s has a night's-stay offer (quote_id %d, %d coins): the guest rents it with pay_with_item (quote_id %d, item %q, qty %d, amount %d), and the keeper grants the room by accepting that — never by paying.",
					keeperName, lq.ID, lq.Amount, lq.ID, lq.Lines[0].ItemKind, lq.Lines[0].Qty, lq.Amount,
				)
			}
			if isLodgingToken(forText) {
				return nil, errors.New(
					"renting a room isn't a plain coin payment — it moves coins but grants no room. Ask the keeper for a room; they'll offer you a night's stay, then take it with pay_with_item.",
				)
			}

			// LLM-202: keep labor compensation out of the bare-pay channel. A labor
			// offer (solicit_work / accept_work) settles its reward at completion
			// through the labor sweep, so a separate pay between the same pair
			// double-compensates the one job — the live John Ellis / Silence Walker
			// case (8 coins paid by hand up front AND a 2-coin labor contract booked
			// on top). The pair-state is the signal, deterministically: a live
			// (pending or working) labor offer between the two in either direction
			// blocks any bare pay between them. forText is deliberately NOT consulted
			// — "helping with serving ale" is not a labor token and parsing free text
			// for work-intent is brittle; the standing arrangement is the fact.
			if lo := activeLaborBetween(w, buyerID, sellerID, at); lo != nil {
				unit := "coins"
				if lo.Reward == 1 {
					unit = "coin"
				}
				switch {
				case lo.EmployerID == buyerID && lo.WorkerID == sellerID && lo.State == LaborStateWorking:
					// The dominant, live case: the employer is paying their own worker
					// who is mid-contract. Name the worker (the seller) accurately.
					return nil, fmt.Errorf(
						"%s is working a job for you right now (%d %s, paid when the work is done) — don't pay separately; the reward settles on its own as they finish. Say a word and let them work.",
						seller.DisplayName, lo.Reward, unit,
					)
				case lo.EmployerID == buyerID && lo.WorkerID == sellerID:
					// Pending offer and the buyer is the employer who should book it,
					// not pay by hand.
					return nil, fmt.Errorf(
						"%s has offered to work for you for %d %s — that reward pays when the job's done, so don't pay by hand (it would compensate the same work twice). Accept the offer with accept_work to set them working, or talk terms first.",
						seller.DisplayName, lo.Reward, unit,
					)
				default:
					// Any other direction (notably the worker paying their own
					// employer — the LLM-164 "paid while waiting" shape). Role-neutral
					// copy that doesn't assert who works for whom.
					return nil, fmt.Errorf(
						"you and %s already have a work arrangement in play (%d %s) — don't settle it with a bare pay; labor pays out on its own when the job's done.",
						seller.DisplayName, lo.Reward, unit,
					)
				}
			}

			// SpendableCoins, not the raw wallet (LLM-644): a visitor's bare
			// pay draws the same trip budget as every other buy door.
			if buyer.SpendableCoins() < amount {
				return nil, fmt.Errorf(
					"insufficient coins (have %d to spend, need %d) — agree on a lower amount before paying.",
					buyer.SpendableCoins(), amount,
				)
			}
			// Seller balance overflow guard. amount is bounded by
			// MaxPayAmount (MaxInt32), but seller.Coins is `int` so on a
			// platform where int >= int64 the sum can still wrap into
			// negative territory if seller already holds a near-MaxInt
			// balance. Theoretical at current village scale, but mint-
			// path-adjacent — a wrapped negative balance is the same
			// failure mode as the amount<1 path the validation above
			// covers.
			if seller.Coins > math.MaxInt-amount {
				return nil, fmt.Errorf(
					"Pay: would overflow seller balance (have %d, adding %d)",
					seller.Coins, amount,
				)
			}

			// Transfer. Single-threaded on the world goroutine, so the two
			// updates are atomic by construction — no FOR UPDATE locks
			// needed like v1's executePayTransfer.
			buyer.Coins -= amount
			seller.Coins += amount
			drawVisitorSpend(buyer, amount)

			// LLM-557: coin a keeper hands a constable settles his town rate.
			// Inline here, the same placement accrueStallWear takes on the sale
			// path, so the debt and the coin move together. A no-op for every
			// other pay in the village. LLM-572: what it settled shapes the
			// relationship facts written below.
			rateSettled, rateBusiness := settleTownRate(w, buyer, seller, amount)

			// Emit the Paid event. World.emit stamps EventID + RootEventID
			// and dispatches subscribers synchronously inside the world
			// goroutine.
			w.emit(&Paid{
				BuyerID:  buyerID,
				SellerID: sellerID,
				Amount:   amount,
				ForText:  forText,
				At:       at,
				// LLM-607: carry what the settlement actually was, not just
				// how much coin moved. This is the only place in the engine
				// that knows, and the subscribers that write the durable row
				// and the coin tally both need it.
				RateSettled: rateSettled,
			})

			// LLM-159: a coin payment is non-conversational progress (and
			// activity) in the buyer's huddle — stamp both clocks so the loop
			// sweep spares a huddle that just transacted and the silence sweep
			// keeps it alive. The pay command requires the buyer be in a huddle
			// (validated above), so CurrentHuddleID is set here.
			touchHuddleProgress(w, buyer.CurrentHuddleID, at)

			// Bidirectional relationship writes. Texts mirror v1's
			// recordPayInteractions: first person from each actor's POV,
			// optional ForText folded in.
			buyerName := buyer.DisplayName
			sellerName := seller.DisplayName
			buyerFact := payFactText("I", "paid", sellerName, amount, forText)
			sellerFact := payFactText(buyerName, "paid", "me", amount, forText)
			// LLM-572: when the coin settled town-rate arrears, say so instead.
			// A rate is an obligation discharged; the generic text above voices it
			// as a purchase, and consolidation cannot tell a purchase that was
			// never delivered from a tax that was never meant to deliver anything.
			// Engine-authored from what settleTownRate actually did, not inferred
			// from the model's forText — the payer's stated purpose is exactly the
			// thing that misled the reader, so it is not what decides the wording.
			if rateSettled > 0 {
				buyerFact = townRatePaidFactText("I", "paid", sellerName, amount, rateSettled, rateBusiness)
				sellerFact = townRatePaidFactText(buyerName, "paid", "me", amount, rateSettled, rateBusiness)
			}
			if _, err := RecordInteraction(buyerID, sellerID, InteractionPaid, buyerFact, at).Fn(w); err != nil {
				log.Printf("sim.Pay: RecordInteraction buyer→seller %q→%q: %v", buyerID, sellerID, err)
			}
			if _, err := RecordInteraction(sellerID, buyerID, InteractionPaidBy, sellerFact, at).Fn(w); err != nil {
				log.Printf("sim.Pay: RecordInteraction seller→buyer %q→%q: %v", sellerID, buyerID, err)
			}
			return nil, nil
		},
	}
}

// findHuddlePeerByDisplayName resolves a case-insensitive DisplayName to a
// peer ActorID within the buyer's huddle. The buyer is excluded from the
// scan so "pay yourself" with the buyer's own name reads as "no one named X"
// rather than silently matching — keeps the error message accurate to the
// model's intent (a self-pay attempt looks the same as a typo of another
// peer's name).
//
// Trailing whitespace on the lookup string is tolerated — the handler
// already trims, but defense in depth keeps the lookup robust to a future
// caller that forgets. Case-insensitive match uses `strings.EqualFold`,
// the Unicode-aware standard comparison (handles Turkic I, German ß, etc.
// correctly and avoids allocating lowercased copies of each peer name).
//
// Returns (sellerID, ok, ambiguous):
//
//   - (id, true, false)  — single match found
//   - ("", false, false) — no match (recipient not in huddle / typo)
//   - ("", false, true)  — TWO OR MORE peers share this name; the caller
//     rejects with an "ambiguous" error rather than picking a recipient
//     non-deterministically. Money-transfer paths must not be ambiguous;
//     village-scale data has unique display names so this is currently
//     theoretical, but the cost of guarding is zero.
func findHuddlePeerByDisplayName(w *World, buyerID ActorID, huddleID HuddleID, name string) (ActorID, bool, bool) {
	target := strings.TrimSpace(name)
	if target == "" {
		return "", false, false
	}
	members, ok := w.actorsByHuddle[huddleID]
	if !ok {
		return "", false, false
	}
	var found ActorID
	for peerID := range members {
		if peerID == buyerID {
			continue
		}
		peer, ok := w.Actors[peerID]
		if !ok {
			continue
		}
		if strings.EqualFold(peer.DisplayName, target) {
			if found != "" {
				return "", false, true
			}
			found = peerID
		}
	}
	if found == "" {
		return "", false, false
	}
	return found, true, false
}

// findHuddlePeerByWorkplaceName resolves a building/structure name to the
// huddle peer who WORKS there — the reroute for when the model names a
// vendor's workplace instead of the vendor (ZBBS-HOME-460). The buy cues
// model "where to buy" as a place ("buy from Ellis Farm (structure_id: …)" —
// a shop doesn't move, the vendor does), so the weak shared-NPC model
// faithfully passes the structure's name where the tool wants the co-present
// person (Elizabeth Ellis, who works the farm). Rather than reject and force
// a retry, we map the place back to the worker standing in the conversation.
//
// Scope is the SAME safe set as findHuddlePeerByDisplayName: peers in the
// buyer's own huddle only. A peer matches when its WorkStructureID resolves
// to a Structure whose DisplayName equals `name` (case-insensitive). This
// POSITIVELY identifies `name` as a present peer's workplace — so an absent
// third party's name (or a typo) never silently routes a payment to whoever
// happens to be standing here; only a real building kept by a real, present
// worker matches.
//
// Returns (peerID, structureName, ok, ambiguous):
//
//   - (id, "<DisplayName>", true, false)  — exactly one present peer works at
//     a structure named `name`, OR several work at the SAME such structure and
//     exactly one of them owns it
//   - ("", "", false, false)              — no present peer works at a place
//     by that name (caller falls through to its person-not-found reject)
//   - ("", "", false, true)               — ambiguous: present peers work at
//     two DIFFERENT structures sharing this display name (a name is not a
//     positive id of one building), or several share one structure and
//     ownership can't break the tie; reject rather than route money
//     non-deterministically
//
// Worker, not owner, is the base key: Ellis Farm carries no owner, yet
// Elizabeth works it and is the real seller. Ownership only breaks a tie among
// several hands at ONE structure (owner vs hired hand). Different buildings
// that merely share a name are never resolved by ownership — that stays
// ambiguous.
func findHuddlePeerByWorkplaceName(w *World, buyerID ActorID, huddleID HuddleID, name string) (ActorID, string, bool, bool) {
	target := strings.TrimSpace(name)
	if target == "" {
		return "", "", false, false
	}
	members, ok := w.actorsByHuddle[huddleID]
	if !ok {
		return "", "", false, false
	}
	var (
		found            ActorID
		foundStructName  string
		foundIsOwner     bool
		matchedStructure StructureID
		sameStructure    = true
		matchCount       int
		ownerCount       int
	)
	for peerID := range members {
		if peerID == buyerID {
			continue
		}
		peer, ok := w.Actors[peerID]
		if !ok || peer.WorkStructureID == "" {
			continue
		}
		structure, ok := w.Structures[peer.WorkStructureID]
		if !ok || !strings.EqualFold(structure.DisplayName, target) {
			continue
		}
		// Track whether every match is the SAME structure. Two different
		// buildings that merely share a display name are NOT a positive id of
		// one place — that stays ambiguous even if one has an owner.
		if matchCount == 0 {
			matchedStructure = peer.WorkStructureID
		} else if peer.WorkStructureID != matchedStructure {
			sameStructure = false
		}
		matchCount++
		// Within one matched structure, BusinessownerState marks its proprietor:
		// the engine flags a structure's keeper as BusinessownerState != nil with
		// WorkStructureID == that structure (arrival_business_huddle.go uses the
		// same pairing). A hired hand has WorkStructureID set but no
		// BusinessownerState. The owner-vs-hand tiebreak is only consulted in the
		// same-structure switch case below, so this can't cross-attribute
		// ownership from some other business.
		isOwner := peer.BusinessownerState != nil
		if isOwner {
			ownerCount++
		}
		// Keep the best candidate, preferring an owner over a hired hand.
		if found == "" || (isOwner && !foundIsOwner) {
			found, foundStructName, foundIsOwner = peerID, structure.DisplayName, isOwner
		}
	}
	switch {
	case found == "":
		return "", "", false, false
	case matchCount == 1:
		return found, foundStructName, true, false
	case !sameStructure:
		// Present peers work at two DIFFERENT structures sharing this name —
		// reject rather than guess which building was meant.
		return "", "", false, true
	case ownerCount == 1 && foundIsOwner:
		// Several hands at the SAME shop, exactly one its proprietor — the
		// owner is the seller (the loop left `found` on that owner).
		return found, foundStructName, true, false
	default:
		return "", "", false, true
	}
}

// payFactText renders the SalientFact text for a pay write. Both sides use
// the same shape — the caller supplies the subject ("I" / buyer name) and
// the object ("seller name" / "me"). ForText is folded in as " for {trim}"
// when non-empty (handler-trimmed already, but defensive).
//
//	payFactText("I",       "paid", "Ezekiel", 5, "")    → "I paid Ezekiel 5 coins."
//	payFactText("Hannah",  "paid", "me",      5, "ale") → "Hannah paid me 5 coins for ale."
func payFactText(subject, verb, object string, amount int, forText string) string {
	for_ := strings.TrimSpace(forText)
	coins := "coins"
	if amount == 1 {
		coins = "coin"
	}
	if for_ == "" {
		return fmt.Sprintf("%s %s %s %d %s.", subject, verb, object, amount, coins)
	}
	return fmt.Sprintf("%s %s %s %d %s for %s.", subject, verb, object, amount, coins, for_)
}

// townRatePaidFactText renders the SalientFact text for a pay that settled town-rate
// arrears (LLM-572). Same subject/object shape as payFactText, which it replaces on
// that path only.
//
// The closing clause is the whole point: "No goods were bought and none are owed in
// return." A town-rate line is otherwise a payment with a stated purpose and no
// delivery recorded against it, which is precisely the shape of an order placed and
// never filled — and the consolidation prompt tells the model to trust the ledger
// over what was said, so a reader that draws the wrong conclusion here draws it with
// full confidence and writes it into a durable summary. Moses James paid two coins of
// rate and came to believe the constable "takes my coin ... and I get nothing", then
// collected a five-coin refund on that belief. The clause closes the inference by
// stating the thing the ledger could not: nothing was ever owed back.
//
// No pronoun for the counterparty, deliberately — the village does not model gender
// on actors, and "owed him" would be a coin-flip on every line. The passive keeps it
// true from either side, so both directions render from one function.
//
// The payer's own forText is dropped rather than folded in. It is model-authored and
// it is the misleading half ("Day's rate on the James Farm" reads as an order); the
// engine knows what actually settled, so the engine's account is the one that goes
// into memory.
// The place clause is dropped rather than allowed to push the sentence past
// MaxSalientFactTextLen. NewSalientFact truncates at that cap, and a truncated fact
// is a memory that ENDS MID-SENTENCE — which here would cut the closing clause off
// precisely, since it is last. Better to lose which shop the rate was levied on than
// the statement that nothing is owed back: the shop is colour, the closing is the
// fix. Unreachable at present village name lengths; it exists so a future rename
// cannot quietly reintroduce the defect.
func townRatePaidFactText(subject, verb, object string, amount, settled int, business *VillageObject) string {
	rate := "the town rate"
	if settled == 1 {
		rate = "the day's rate"
	}
	where := ""
	if business != nil {
		if name := strings.TrimSpace(business.DisplayName); name != "" {
			where = fmt.Sprintf(" on the %s", name)
		}
	}
	text := townRatePaidSentence(subject, verb, object, amount, settled, rate, where)
	if where != "" && len([]rune(text)) > MaxSalientFactTextLen {
		text = townRatePaidSentence(subject, verb, object, amount, settled, rate, "")
	}
	return text
}

func townRatePaidSentence(subject, verb, object string, amount, settled int, rate, where string) string {
	const closing = "the town's due, owed and now settled. No goods were bought and none are owed in return."
	if settled == amount {
		// The whole payment was the rate — the ordinary case, since the cue names
		// the exact coin owed and the arrears cap keeps it small.
		return fmt.Sprintf("%s %s %s %s%s, %s — %s",
			subject, verb, object, rate, where, coinsPhrase(amount), closing)
	}
	// A larger payment that also cleared arrears. The settlement policy is
	// deliberately over-broad (see settleTownRate), so this is a gift or a debt
	// repayment that happened to discharge the levy — name both parts rather than
	// call the whole sum a rate.
	was := "was"
	if settled != 1 {
		was = "were"
	}
	return fmt.Sprintf("%s %s %s %s; %s of it %s %s owing%s — %s",
		subject, verb, object, coinsPhrase(amount), coinsPhrase(settled), was, rate, where, closing)
}

// coinsPhrase voices a coin count with the right singular. Shared by the town-rate
// fact texts, which need it in more than one slot per sentence.
func coinsPhrase(n int) string {
	if n == 1 {
		return "1 coin"
	}
	return fmt.Sprintf("%d coins", n)
}

// findCoinQuoteForPay returns the active single-line scene quote the seller has
// posted for the good the buyer named in a bare pay's forText — i.e. the buyer
// is trying to buy a quoted good with the coin-only pay tool, which transfers
// coins but never delivers the good or settles the quote (LLM-172). Returns nil
// when forText names no active quoted good — resolveItemKind can't canonicalize
// it, the buyer's huddle maps to no scene, or no matching quote exists — so a
// tip, a debt, or an unquoted payment proceeds as a plain coin transfer.
//
// Eligibility mirrors findAutoMatchQuote's filter (active, unexpired, this
// seller, visible to this buyer in their scene, single-line, not addressed to
// someone else), but the match keys off the forText item rather than echoed
// pay_with_item terms — a bare pay carries no item/qty fields, only free text.
// Single-line only: a bundle has no single forText good to name and is taken
// whole via an explicit quote_id.
//
// Among multiple eligible quotes the cheapest (then lowest ID) wins, matching
// findAutoMatchQuote — the chosen quote's ID lands in the corrective steer, so
// the pick must be deterministic across runs and not ride map-iteration order.
func findCoinQuoteForPay(w *World, buyer *Actor, sellerID ActorID, forText string, at time.Time) *SceneQuote {
	kind, ok := resolveItemKind(w, forText)
	if !ok {
		return nil
	}
	sceneID, ok := resolveSellerScene(w, buyer.CurrentHuddleID)
	if !ok {
		return nil
	}
	var best *SceneQuote
	for _, q := range w.Quotes {
		if q == nil || q.State != SceneQuoteStateActive {
			continue
		}
		if !q.ExpiresAt.IsZero() && !at.Before(q.ExpiresAt) {
			continue
		}
		if q.SellerID != sellerID || q.SceneID != sceneID {
			continue
		}
		if q.TargetBuyer != "" && q.TargetBuyer != buyer.ID {
			continue
		}
		if len(q.Lines) != 1 || q.Lines[0].ItemKind != kind {
			continue
		}
		if best == nil || q.Amount < best.Amount || (q.Amount == best.Amount && q.ID < best.ID) {
			best = q
		}
	}
	return best
}

// isLodgingToken reports whether a bare pay's free-text `for` names lodging — a
// closed, word-level allow-list of room nouns. Word-level (via FieldsFunc on
// non-letters) rather than substring so "mushroom"/"embed" don't trip
// "room"/"bed". Deliberately omits the bare time-word "night" ("for last
// night's ale" is a legitimate tip); a vague "a night" payment is instead
// caught by activeLodgingQuoteBetween whenever a room offer is actually on the
// table. Mirrors isLaborToken (LLM-167). LLM-177.
func isLodgingToken(forText string) bool {
	for _, tok := range strings.FieldsFunc(strings.ToLower(forText), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch tok {
		case "room", "rooms", "lodging", "lodgings", "lodge", "bedroom", "bedrooms", "bed", "beds":
			return true
		}
	}
	return false
}

// isLodgingNightPhrase reports whether the WHOLE pay text is one of the vague
// "a night"/"night's stay" phrasings that mean lodging only in context. Kept as
// an exact normalized-phrase match (article stripped) rather than word-level so
// it catches "a night" / "the night's stay" without firing on "for last
// night's ale" (a legitimate tip that merely contains "night"). Used ONLY to
// qualify an open lodging quote (activeLodgingQuoteBetween) — never on its own,
// since a bare "a night" with no room offer on the table is too weak to steer.
// LLM-177.
func isLodgingNightPhrase(forText string) bool {
	switch stripLeadingArticle(strings.ToLower(strings.TrimSpace(forText))) {
	case "night", "nights", "night's stay", "nights stay", "night stay", "stay the night":
		return true
	}
	return false
}

// activeLodgingQuoteBetween finds an active, unexpired single-line lodging
// (room-granting) quote in the payer's scene posted by EITHER party to the bare
// pay — so it fires whether the guest pays the keeper or the keeper reverses
// coins back to the guest (both are the wrong tool for a room). Eligibility
// mirrors findCoinQuoteForPay (active, unexpired, in this scene), but keys off
// the "lodging" capability rather than a forText item match, since lodging
// phrasing doesn't canonicalize to nights_stay.
//
// Targeting decides how much intent the pay text needs: a quote TARGETED at the
// counterparty marks a room deal already underway, so it matches any bare pay
// between the two. A PUBLIC quote (TargetBuyer empty) is visible to the whole
// huddle and must NOT capture an unrelated tip that merely coincides with an
// open room offer, so it matches only when lodgingIntent is set (the pay text
// itself signals lodging). Cheapest then lowest-ID wins so the quote_id named in
// the steer is deterministic across runs. LLM-177.
func activeLodgingQuoteBetween(w *World, partyA, partyB ActorID, huddleID HuddleID, at time.Time, lodgingIntent bool) *SceneQuote {
	sceneID, ok := resolveSellerScene(w, huddleID)
	if !ok {
		return nil
	}
	var best *SceneQuote
	for _, q := range w.Quotes {
		if q == nil || q.State != SceneQuoteStateActive {
			continue
		}
		if !q.ExpiresAt.IsZero() && !at.Before(q.ExpiresAt) {
			continue
		}
		if q.SceneID != sceneID {
			continue
		}
		// One of the two must have posted it.
		if q.SellerID != partyA && q.SellerID != partyB {
			continue
		}
		other := partyA
		if q.SellerID == partyA {
			other = partyB
		}
		if q.TargetBuyer != "" {
			// Targeted: must be addressed to the counterparty; then it blocks
			// any bare pay between them.
			if q.TargetBuyer != other {
				continue
			}
		} else if !lodgingIntent {
			// Public: only a lodging-intent pay text counts as a botched room
			// payment — don't capture an unrelated tip.
			continue
		}
		if len(q.Lines) != 1 || !itemHasCapability(w, q.Lines[0].ItemKind, "lodging") {
			continue
		}
		if best == nil || q.Amount < best.Amount || (q.Amount == best.Amount && q.ID < best.ID) {
			best = q
		}
	}
	return best
}

// goodsNamedInPayMemo resolves the goods a bare pay's free-text memo names, in
// memo order and deduplicated, and reports whether any of them carried an
// explicit count. Tokens split on commas, semicolons, ampersands, plus signs
// and the word "and"; a leading count ("5x", "5", "2 x") is stripped before
// resolution. Prose that resolves to no good contributes nothing, so "your
// kindness" and "what I owe you" both come back empty (LLM-649).
func goodsNamedInPayMemo(w *World, forText string) (goods []ItemKind, counted bool) {
	seen := map[ItemKind]bool{}
	isSep := func(r rune) bool { return r == ',' || r == ';' || r == '&' || r == '+' }
	var segment []string
	flush := func() {
		if len(segment) == 0 {
			return
		}
		name, hasCount := stripLeadingCount(strings.Join(segment, " "))
		segment = segment[:0]
		kind, ok := resolveItemKind(w, name)
		if !ok {
			return
		}
		if hasCount {
			counted = true
		}
		if !seen[kind] {
			seen[kind] = true
			goods = append(goods, kind)
		}
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(forText), isSep) {
		// Words are split on any whitespace, of any count, and a bare "and" word
		// ends a good — "wheat and  flour", "bread\tand\tale" (code_review: a
		// single-space split around "and" was bypassable by a second space).
		for _, word := range strings.Fields(tok) {
			if word == "and" {
				flush()
				continue
			}
			segment = append(segment, word)
		}
		flush()
	}
	return goods, counted
}

// stripLeadingCount drops a leading numeric count from a memo token — "5x
// wheat", "5 wheat", "2 x cloak" — and reports whether one was there. A bare
// number with nothing after it counts nothing and is returned unchanged.
func stripLeadingCount(s string) (string, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return s, false
	}
	rest := strings.TrimSpace(s[i:])
	if rest == "x" || strings.HasPrefix(rest, "x ") {
		rest = strings.TrimSpace(rest[1:])
	}
	if rest == "" {
		return s, false
	}
	return rest, true
}

// joinItemLabels renders resolved goods as prose: "bread and cheese",
// "wheat, flour and firewood".
func joinItemLabels(w *World, goods []ItemKind) string {
	labels := make([]string, 0, len(goods))
	for _, kind := range goods {
		labels = append(labels, itemKindDisplayLabel(w, kind))
	}
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}
