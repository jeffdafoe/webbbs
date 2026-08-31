package sim

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// pay_with_item_commands.go — Phase 3 PR S4 step 5.
//
// Five Command Fns that drive the buyer-initiated pay-with-item commerce
// flow on top of the PR S4 substrate (pay_ledger.go +
// events_pay_with_item.go) and the PR S3 scene_quote substrate. Mirrors
// the established pattern from PR B (Pay) and PR S3 (SceneQuoteCreate):
// every Command Fn re-validates everything its handler did, mutates on
// the world goroutine, emits events, and kicks off relationship writes
// via RecordInteraction.
//
// Design spec:
//   - shared/tasks/engine-in-memory-rewrite/pay-with-item-architecture-design
//   - shared/tasks/engine-in-memory-rewrite/pay-ledger-substrate-design
//   - shared/tasks/engine-in-memory-rewrite/scene-quote-design § 8 (fast-path)
//
// PR S4 step-5 design locks (settled 2026-05-16 EOS-26):
//
//   - One PR — substrate + Command Fns + handlers + subscribers + sweep
//     ship together. No S4a/S4b split.
//
//   - sim.AcceptPay revalidates 10 gates first-failure-wins, in this
//     order: auth → state → TTL → co-presence (huddle, both sides) →
//     seller break → ItemKind catalog → consumer departure → stock →
//     funds. Auth + state are idempotent rejects (tool error, NO
//     transition). The other eight all drive a terminal flip (expired,
//     failed_unavailable, failed_insufficient_stock,
//     failed_insufficient_funds).
//
//   - No structure-level closed-shop check inside AcceptPay. Movement
//     layer's EntryPolicy + huddle co-presence already enforces that —
//     a buyer not allowed inside the seller's shop can't share the
//     seller's huddle, and the co-presence gate catches it.
//
//   - PayCountered owns the countered transition (separate event family
//     from PayWithItemResolved). Per architecture § 10 + ledger-substrate
//     § 6 ambiguity resolved in favor of separate events at EOS-26:
//     PayCountered fires on counter; PayWithItemResolved fires on every
//     other terminal. PayTerminalStateCountered exists for type
//     completeness but PayWithItemResolved is never emitted with that
//     value.
//
//   - withdraw_pay ships in S4 — buyer-callable, pending-only, no
//     co-presence required, optional message reuses entry.Message.
//
//   - in_response_to chain validation lives in PayWithItem (not the
//     handler): parent exists, parent State == countered, same buyer +
//     seller, parent.ResolvedAt within PayLedgerInResponseToWindow,
//     parent has no pre-existing child (O(N) scan over World.PayLedger;
//     defer reverse index per ledger-substrate § 6).
//
// Fast-path quote matching (scene-quote-design § 8) runs inside
// PayWithItem when quote_id != 0: six predicates, any failure is a
// strict-reject tool error, NO silent fall-through to the slow path.
// On all-pass: ledger entry minted ALREADY in Accepted state (skips
// Pending), atomic transfer fires, PayWithItemResolved emits with
// TerminalState=Accepted.

// MaxPayWithItemAmount caps the offered total in coins. Same ceiling as
// MaxPayAmount and MaxSceneQuoteAmount so handler-side bounds, fast-path
// matching, and downstream pg-impl int4 columns all share one mental
// model. Re-enforced inside each Command Fn — PayWithItem / AcceptPay /
// CounterPay are exported and non-handler callers (tests, admin paths,
// future cascades) could otherwise pass an unbounded amount.
const MaxPayWithItemAmount = math.MaxInt32

// MaxPayWithItemQty caps qty-per-consumer. Mirrors MaxConsumeQty and
// MaxSceneQuoteQty. The Command Fn additionally enforces that
// Qty * effectiveConsumerCount doesn't overflow int before the stock
// check uses the product.
const MaxPayWithItemQty = math.MaxInt32

// MaxPayWithItemConsumers caps len(ConsumerIDs) on a group order.
// Matches SceneQuoteMaxConsumers so the consumer-set equality predicate
// in the fast path doesn't have to special-case quote-side vs offer-side
// caps. Architecture § 9 caps at 8 — small enough to keep per-tick
// prompt cost bounded, generous enough for "round of ale at the tavern."
const MaxPayWithItemConsumers = SceneQuoteMaxConsumers

// MaxPayWithItemPayItems caps len(PayItems) on a barter offer — the
// distinct goods lines a buyer may pay WITH (or a seller may demand in a
// counter). ZBBS-HOME-393. 8 matches MaxPayWithItemConsumers: small
// enough to keep the seller's offer-decision prompt line bounded,
// generous enough for a realistic mixed-goods payment.
const MaxPayWithItemPayItems = 8

// PayItemInput is one goods line on a barter offer as it arrives from the
// tool layer: a free-text item NAME (resolved to a canonical ItemKind
// inside the Command Fn via resolveItemKind) and a positive quantity.
// The Command turns a []PayItemInput into the resolved []ItemKindQty it
// stores on the entry. ZBBS-HOME-393.
type PayItemInput struct {
	Item string
	Qty  int
}

// MaxPayMessageRunes caps the rune length of model-controlled free text
// stored on PayLedgerEntry.Message — counter message, decline reason,
// withdraw note. 220 runes matches MaxSalientFactTextLen so a counter /
// decline / withdraw message can be included whole in a downstream
// SalientFact write without secondary truncation.
const MaxPayMessageRunes = 220

// PayLedgerInResponseToWindow bounds how far back a parent ledger entry
// may have been countered for a buyer's in_response_to follow-up to be
// accepted. Architecture § 7's "~1 hour" — long enough to let the buyer
// step away, think, come back; short enough that an abandoned counter
// chain doesn't resurrect across game-sessions or natural conversation
// turnover.
const PayLedgerInResponseToWindow = 1 * time.Hour

// MaxPayCounterChainDepth bounds how deep a buyer↔seller counter chain
// may go. The initial offer is depth 0; each buyer in_response_to
// response increments depth (parent.Depth + 1). A parent already at this
// depth can't be responded to, so the chain tops out at 3 rounds of
// countering — matching v1's cap (bound escalation so an LLM buyer and
// seller can't haggle unboundedly).
//
// v1 also dropped counter_pay from the seller's toolset once the chain
// reached the cap. That seller-side prompt gating belongs to the v2
// deliberation-prompt surface, which isn't built yet — so this
// buyer-side gate in validateInResponseTo is what actually bounds the
// chain today. (Without toolset gating a seller can still emit one final
// counter on a depth-cap offer, but the buyer can't respond to it, so the
// chain still terminates.)
const MaxPayCounterChainDepth = 3

// PayWithItemResult is the value returned by the PayWithItem Command Fn.
// The handler narrates LedgerID + State back to the LLM so the model has
// a stable identifier to reference in a follow-up tool call (acceptance
// awaiting, counter response via in_response_to, withdraw, etc.) and
// knows whether the offer is mid-flight (pending) or already resolved
// (fast-path accepted).
type PayWithItemResult struct {
	LedgerID LedgerID
	State    PayLedgerState
	// FastPath is true when a non-zero quote_id matched all six fast-path
	// predicates and the entry was minted in Accepted state. False for
	// slow-path (pending) entries, including those that referenced a
	// quote_id which failed any predicate (those return an error before
	// any entry is minted — no silent fall-through, scene-quote-design
	// § 8).
	FastPath bool
	// EatHereClamped is true when the buyer asked for take-home but the
	// engine forced eat-here (non-portable consumable, ZBBS-WORK-403/405).
	// Carried on the result so an LLM buyer's tool feedback can state the
	// adjusted disposition instead of leaving the model believing it
	// carried the goods off.
	EatHereClamped bool
	// Lines is the bundle's item lines on a bundle quote-take (LLM-101), so the
	// harness settle feedback names the whole bundle rather than just the
	// representative first line the buyer echoed. Empty for a single-item take.
	Lines []QuoteLine
	// Fast-path settle summary (ZBBS-HOME-436). A quote-take settles, pays,
	// and (consume_now) feeds the buyer inside this one tool call, but the
	// buyer's feedback was a bare [ok] — and the within-tick perception body
	// re-renders from the tick-start snapshot, so the buyer's felt needs
	// never legibly move. The model read "nothing happened" and re-bought to
	// the iteration budget (the Ezekiel six-meat morning, live 2026-06-12).
	// These fields let the harness voice what the settle actually did,
	// computed on the world goroutine from LIVE post-commit state. All zero
	// for slow-path (pending) entries.
	BuyerAte        int     // units the buyer themself ate now
	KeptToInventory int     // surplus units pocketed into the buyer's pack (needs-clamp)
	TookHome        bool    // physical goods handed over at accept
	Booked          bool    // future-night lodging Order minted, awaiting keeper check-in
	LodgedNow       bool    // same-day walk-in room granted on the spot (LLM-84)
	MendedNow       bool    // worn garments mended on the spot (LLM-625)
	ServicedNow     bool    // business equipment serviced on the spot (LLM-648)
	SatisfiesNeed   NeedKey // primary need the consumed item satisfies ("" when n/a)
	FeltAfter       string  // buyer's post-meal felt label(s) for the item's needs; "" = sated
	// MealMinutes is the buyer's eat-here dwell duration in minutes when this
	// settle started a sit-down meal/drink (0 otherwise — take-home, immediate-
	// only items, or eating-while-walking). The slow-burn payoff is collected
	// only by staying put, so the settle feedback uses this to tell the buyer to
	// stay and finish instead of walking off and wasting it (ZBBS-WORK-409).
	MealMinutes int

	// Announced / SayRefused carry the fate of the optional spoken line
	// pay_with_item folds in, mirroring SceneQuoteCreateResult (LLM-350). The
	// buyer's handoff word used to be a separate speak the restock cue asked for
	// after the offer — unreachable, since pay_with_item ends the tick.
	Announced  bool
	SayRefused string
	// ReroutedSellerName is the worker's display name when the model named a
	// building (its workplace) instead of the person and the engine rerouted
	// the offer to them (ZBBS-HOME-460). Empty on the common path. The harness
	// echo prefers it over the original seller arg so "bide for their answer"
	// names a person who can actually answer.
	ReroutedSellerName string
}

// PayWithItem returns the Command for the buyer-initiated pay-with-item
// commerce surface. Two paths:
//
//   - Slow path (quoteID == 0): mints a Pending PayLedgerEntry, emits
//     PayOfferReceived. The seller's reactor tick perceives the offer
//     via PayOfferWarrantReason and decides accept_pay / decline_pay /
//     counter_pay on a subsequent tick. Returns
//     {LedgerID, Pending, FastPath:false}.
//
//   - Fast path (quoteID != 0): runs the six-predicate match
//     (scene-quote-design § 8) against world state. On all-pass: mints
//     an entry directly in Accepted state, performs the atomic coin +
//     item transfer + ConsumeNow per-consumer application, emits
//     PayWithItemResolved{TerminalState: Accepted}, and any per-consumer
//     ItemConsumed events. Returns {LedgerID, Accepted, FastPath:true}.
//
//     Any predicate failure on the fast path is a STRICT-REJECT tool
//     error — the call never falls through to slow path. Buyer who
//     wanted slow-path semantics must call again with quoteID=0.
//
// in_response_to (parentID) is the counter-chain link: a non-zero
// parentID asserts this offer is the buyer's response to a previously
// countered parent ledger. Validation: parent exists, parent
// State == countered, parent.BuyerID == buyerID, parent.SellerID
// resolves to the same seller, parent.ResolvedAt within
// PayLedgerInResponseToWindow, parent has no pre-existing child (O(N)
// scan; ledger-substrate § 6 defers the reverse index).
//
// PayWithItem itself does NOT enforce parent matching on item terms —
// the buyer may legitimately change the item/qty/consume_now/consumers
// on their counter-response (architecture § 7). The seller can decline
// if the terms drift unfavorably.
func PayWithItem(
	buyerID ActorID,
	sellerName string,
	itemName string,
	qty int,
	amount int,
	consumeNow bool,
	consumerNames []string,
	payItems []PayItemInput,
	quoteID QuoteID,
	parentID LedgerID,
	forText string,
	at time.Time,
	// opts carries the optional buyer-facing extras — the advance-booking offset
	// (ReadyInDays; ZBBS-HOME-403) and the partial-payment deposit (Deposit;
	// LLM-357). Variadic so the many call sites that pass NO options compile
	// unchanged; only the buyer-facing tool + PC routes pass one, and at most one
	// value may be passed. This replaced the prior `readyInDays ...int` tail — a
	// source-breaking change, but PayWithItem is engine-internal with no external
	// consumers, so the few callers that passed a bare int were migrated to
	// PayWithItemOpts{ReadyInDays: N} within this repo. See PayWithItemOpts.
	opts ...PayWithItemOpts,
) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Numeric defense. PayWithItem is exported — non-handler
			// callers could pass shapes the decode side rejects.
			//
			// Coins are now OPTIONAL (>= 0): an offer may pay with coins,
			// goods (payItems), or both, but must carry at least one of the
			// two. The "must offer something" rule is enforced after
			// payItems are validated (it also closes the free-goods hole —
			// an all-zero offer was the ZBBS-HOME-391 economy bug).
			if amount < 0 {
				return nil, fmt.Errorf("PayWithItem: amount cannot be negative (got %d)", amount)
			}
			if amount > MaxPayWithItemAmount {
				return nil, fmt.Errorf("PayWithItem: amount exceeds maximum (got %d, max %d)", amount, MaxPayWithItemAmount)
			}
			if len(payItems) > MaxPayWithItemPayItems {
				return nil, fmt.Errorf(
					"PayWithItem: too many goods lines (got %d, max %d) — combine into fewer lines.",
					len(payItems), MaxPayWithItemPayItems,
				)
			}
			if qty < 1 {
				return nil, fmt.Errorf("PayWithItem: qty must be at least 1 (got %d)", qty)
			}
			if qty > MaxPayWithItemQty {
				return nil, fmt.Errorf("PayWithItem: qty exceeds maximum (got %d, max %d)", qty, MaxPayWithItemQty)
			}
			if len(consumerNames) > MaxPayWithItemConsumers {
				return nil, fmt.Errorf(
					"PayWithItem: too many consumers (got %d, max %d) — split the order into smaller offers.",
					len(consumerNames), MaxPayWithItemConsumers,
				)
			}
			// Conflicting offer-mode guard. quote_id selects the fast-path
			// quote accept; in_response_to links this offer into a counter
			// chain. They express different lifecycle intents and the fast
			// path returns before any counter-chain handling, so it would
			// silently win — reject the ambiguous combination outright. The
			// pc/pay handler rejects this earlier (400); enforced here too
			// because NPC / tool callers reach the substrate directly.
			if quoteID != 0 && parentID != 0 {
				return nil, errors.New(
					"an offer can either accept a quote (quote_id) or respond to a counter (in_response_to), not both — drop one.",
				)
			}
			// Barter is slow-path only (ZBBS-HOME-393). A posted quote is a
			// coin-priced sale; taking it with goods has no defined match
			// semantics (the fast-path's exact-term predicate is coin-only).
			// Reject the combination outright rather than silently dropping
			// the goods.
			if quoteID != 0 && len(payItems) > 0 {
				return nil, errors.New(
					"you can't pay a posted quote with goods — a quote is a coin price. Drop quote_id to make a barter offer, or drop the goods to take the quote.",
				)
			}

			buyer, ok := w.Actors[buyerID]
			if !ok {
				return nil, fmt.Errorf("PayWithItem: buyer %q not in world", buyerID)
			}
			if buyer.MoveIntent != nil {
				return nil, errors.New(
					"you are walking — finish your move before making an offer. " +
						"Either offer BEFORE the move_to, or wait until you arrive.",
				)
			}
			if buyer.CurrentHuddleID == "" {
				return nil, errors.New(
					"you're not in a conversation — start one with the person you want to offer to first.",
				)
			}

			// Resolve scene anchor first; quote lookup needs SceneID.
			sceneID, ok := resolveSellerScene(w, buyer.CurrentHuddleID)
			if !ok {
				return nil, errors.New(
					"your current conversation isn't anchored to a scene — wait for the scene to be established before making an offer.",
				)
			}

			// Resolve seller against huddle peers — tight scope (same
			// huddle, case-insensitive, ambiguity reject). Mirrors PR B's
			// Pay.
			sellerID, ok, ambiguous := findHuddlePeerByDisplayName(w, buyerID, buyer.CurrentHuddleID, sellerName)
			if ambiguous {
				return nil, fmt.Errorf(
					"more than one person named %q is in this conversation — use a unique full name before offering.",
					sellerName,
				)
			}
			// reroutedSellerName carries the worker's display name when the
			// model named a building instead of the person (set below), so the
			// tool-result echo names the real recipient. Empty on the common
			// path. ZBBS-HOME-460.
			var reroutedSellerName string
			if !ok {
				// Reroute a workplace name to the worker — see sim.Pay's note.
				// The restock/satiation buy cues name the structure ("buy from
				// Ellis Farm"), so the model offers to the place where the
				// co-present worker is wanted.
				peerID, structureName, peerOK, peerAmbiguous := findHuddlePeerByWorkplaceName(w, buyerID, buyer.CurrentHuddleID, sellerName)
				if peerAmbiguous {
					return nil, fmt.Errorf(
						"more than one person here works at %q — name the person you want to offer to.",
						sellerName,
					)
				}
				if !peerOK {
					return nil, fmt.Errorf(
						"no one named %q in this conversation — re-check who is here before offering.",
						sellerName,
					)
				}
				sellerID = peerID
				if peer, peerExists := w.Actors[peerID]; peerExists {
					reroutedSellerName = peer.DisplayName
				}
				log.Printf("sim.PayWithItem: rerouted offer from building %q to its worker %q (buyer %q)",
					structureName, peerID, buyerID)
			}
			if sellerID == buyerID {
				return nil, errors.New("you cannot make an offer to yourself")
			}
			seller, ok := w.Actors[sellerID]
			if !ok {
				return nil, fmt.Errorf("PayWithItem: seller %q vanished mid-resolve", sellerID)
			}

			// LLM-290: coins named as the good to buy are currency, not an item.
			// The pay_with_item handler translates that shape to a plain payment
			// before it ever builds this command, so reaching here means another
			// entrance leaked a coin token through — steer rather than staking a
			// nonsense goods-offer. Checked BEFORE resolveItemKind so a lingering
			// phantom 'coin' catalog row can never resolve the call into a real
			// offer.
			if IsCoinToken(itemName) {
				return nil, errors.New(
					"coins aren't a good to buy — to hand someone coins, use pay (recipient + amount). To sell your goods for coins, post them with sell.",
				)
			}
			// ZBBS-WORK-412 deliberately does NOT mint here. This is the BUY
			// path (the buyer names the good to receive); unlike the sell /
			// pay-with sites, a mint wouldn't reject the same tick — PayWithItem
			// would register a pending offer the seller can't fill, recreating
			// the poisoned-ledger retry loop (salem-multi-item-pay-protocol).
			// Discovery mint is scoped to same-tick-rejecting sites: consume,
			// scene_quote, and pay_items goods (resolvePayItems).
			kind, ok := resolveItemKind(w, itemName)
			if !ok {
				// LLM-167: a buyer naming "work"/"labor" as the good it wants is
				// reaching for the labor market through the barter tool — steer
				// to the labor verbs instead of the dead-end unknown-kind error.
				if isLaborToken(itemName) {
					return nil, errors.New(laborTradeSteerMsg)
				}
				// LLM-561: teach the shape, not just the fault. The live failure
				// was item "bundle" — a buyer answering a multi-line quote by
				// putting the whole purchase into the one-kind item slot. When
				// the named seller actually has a bundle standing here, name both
				// real routes (that quote_id whole, or one good at a time) with
				// the concrete quote_id; an ordinary misnamed good keeps the
				// short error — bundle teaching is noise for a typo, and this
				// site also serves offer_trade's want_item (code_review).
				if bq := activeBundleQuoteFrom(w, buyer, sellerID, sceneID, at); bq != nil {
					return nil, fmt.Errorf(
						"unknown item kind %q — an offer carries one kind of good. %s's bundle is taken whole: call pay_with_item with quote_id %d and amount %d; or offer for one good at a time, with no quote_id.",
						itemName, seller.DisplayName, bq.ID, bq.Amount,
					)
				}
				return nil, fmt.Errorf(
					"unknown item kind %q — an offer carries one kind of good, named as it appears in this world; check the items available before offering.",
					itemName,
				)
			}

			// LLM-189: reverse-pay role-gate. A seller must not fire the
			// buyer verb (pay_with_item, or offer_trade which lowers onto
			// this command) at the very counterparty she is selling THIS
			// item to — that mints a phantom reverse-direction offer. The
			// live Prudence→Anne blueberry inversion: Prudence held a
			// standing blueberry quote to Anne AND had just sold her 5, yet
			// named Anne as "seller" and staked a mirror offer Anne could
			// never fill, deadlocking the huddle. A seller consummates a
			// sale via accept_pay, never by buying her own goods back. Reject
			// at dispatch — the substrate stays authoritative; closing the
			// taken quote (runPayWithItemFastPath) thins the perception cue
			// that lured the model here, and this is the hard backstop.
			//
			// LLM-551: the gate stands aside when the named seller holds an
			// ACTIVE quote for this kind targeted at the caller. That quote is
			// the substrate settling direction in the caller's favour — she is
			// taking an offer the seller genuinely posted, which is the exact
			// opposite of a phantom reverse offer, and the only tool that takes
			// it is this one. Without the escape a completed sale poisoned the
			// reverse trade for the LIFE of the huddle: arm 2 matches a terminal
			// `accepted` entry with no recency bound, and a huddle stays open
			// hours past its last word (LLM-537). Live: Patience Walker sold
			// Josiah Thorne 3 flour at 18:54, he posted her a 4-flour quote at
			// 19:11, and her ~20 exactly-matching pay_with_item calls were all
			// refused until she gave up and went home.
			if callerSellsItemTo(w, buyerID, sellerID, kind, sceneID, buyer.CurrentHuddleID, at) &&
				!activeTargetedQuoteOffers(w, sellerID, buyerID, kind, sceneID, at) {
				return nil, fmt.Errorf(
					"you're the one selling %s to %s here — you don't buy it back. %s",
					kind, seller.DisplayName,
					reversePaySettleSteer(w, buyerID, sellerID, kind, buyer.CurrentHuddleID, sceneID, at),
				)
			}

			// LLM-555: the same fact as the gate above, remembered past the end of
			// the conversation that produced it. That gate's arm 2 scopes to the
			// CURRENT huddle and walks ledger entries reaped an hour after they
			// resolve, so it cannot see a sale made in an earlier conversation —
			// and every reversal in the live ledger crossed a huddle boundary (55
			// of 55 since 2026-05-06), which is to say the gate above never once
			// fired on the churn it is shaped for. Live 2026-07-28: iron went
			// factor -> Josiah at 19:53, back Josiah -> factor at 20:50, and Josiah
			// was buying it off him again at 21:32.
			//
			// A SEPARATE gate rather than a third arm because the steer differs.
			// reversePaySettleSteer opens "Wait for them to pay you", which is
			// sound mid-negotiation and actively misleading about a sale made an
			// hour ago in another room — there is no payment coming to wait for.
			// Both messages end on the same escape route, which is the one the
			// quote check below actually honours.
			//
			// It carries LLM-551's escape hatch for the reason LLM-551 exists: a
			// reverse-pay gate with no way out poisons a legitimate sale in the
			// other direction, and this memory outlives a huddle by design, so
			// without the hatch it would poison one for three hours rather than
			// for one conversation. A seller who posts the caller a targeted quote
			// for this kind is settling direction in the caller's favour — that is
			// the counterparty saying "I do mean to sell you this", which is
			// exactly what the steer tells the caller to go and ask for.
			if ActorRecentlySoldTo(buyer, sellerID, kind, at) &&
				!activeTargetedQuoteOffers(w, sellerID, buyerID, kind, sceneID, at) {
				return nil, fmt.Errorf(
					"you sold %s to %s a short while ago — you don't buy your own goods straight back. If they mean to sell it on to you, they must post the offer first.",
					kind, seller.DisplayName,
				)
			}

			// LLM-291: a wholesale producer must not fire the buyer verb for
			// its OWN produce. Live hud-9b23…: Moses (James Farm, wholesaler),
			// pressed to answer a customer who wanted his carrots, named the
			// CUSTOMER as "seller" and staked a pay_with_item to "buy" his own
			// carrots back — a phantom reverse offer the customer could never
			// fill. Distinct from the two neighbouring gates: the reverse-pay
			// role-gate above keys on a sale TO this counterparty (Moses had
			// only spoken, no quote/ledger), and the wholesale gate below keys
			// on the SELLER arg's workplace (here the wholesaler is the CALLER,
			// not the arg). Same sim.IsOwnProduce the Consume guard and eat-cue
			// filter on, so cue and block agree; item-scoped, so a farmhand
			// buying unrelated goods is untouched. Steer to the wholesale
			// channel — not "wait for them to pay you" (a retail buyer can't
			// pay a wholesaler; the wholesale gate would reject them), so the
			// redirect names what the customer must actually do.
			//
			// This is an ABSOLUTE prohibition, not merely a reverse-direction
			// guard: a wholesale producer never buys its own produce back from
			// ANYONE — the good it grows is stock to sell (it can't even eat it,
			// LLM-267), and the only legitimate flow for this kind is the
			// distributor buying FROM the producer. So the check ignores who is
			// named as seller (customer, peer, or even the distributor);
			// buying one's own crop back is not a case we support.
			if IsOwnProduce(w.VillageObjects, buyer.WorkStructureID, buyer.RestockPolicy, kind) {
				distributor := DistributorSteerLabel(w.VillageObjects, w.Actors)
				return nil, fmt.Errorf(
					"you produce %s to sell — you don't buy it back. Your %s goes wholesale to %s, whose shop stocks it for the village; send buyers to %s.",
					kind, kind, distributor, distributor,
				)
			}

			// LLM-293: general reverse-pay guard. The seller-side mistake that the
			// LLM-189 gate (needs a targeted quote / accepted sale) and the LLM-291
			// arm (wholesale only) both miss: a seller, pressed to consummate a sale
			// a customer just asked for, fires the BUYER verb and names the CUSTOMER
			// as `seller` — a phantom reverse offer the customer can't fill. Live:
			// Hannah (innkeeper, porridge producer) answered "I would like to buy
			// another bowl" with pay_with_item{seller:"Lewis", item:"porridge"},
			// offering to buy her own porridge back from her customer. Reject when
			// the caller deals in `kind` as one of its OWN wares (RestockPolicy.
			// Manages — any produce/forage/buy entry) AND the named seller can't
			// actually supply it. The counterparty test is what keeps a legitimate
			// RESTOCK safe: a reseller buying a good it vends FROM a real supplier
			// (producer/forager, the distributor, or anyone holding stock) passes,
			// because that seller can fill the offer; only a phantom buy from a
			// non-supplier is blocked. Steers to the sell path rather than "wait for
			// them to pay you" — the customer hasn't offered yet. Covers offer_trade
			// (lowers onto this command).
			if buyer.RestockPolicy.Manages(kind) && !counterpartyCanSupply(w, seller, kind) {
				return nil, fmt.Errorf(
					"you sell %s — to sell it to %s, offer it with sell or wait for their bid and use accept_pay; pay_with_item is for buying, not selling.",
					kind, seller.DisplayName,
				)
			}

			// Wholesale tier (LLM-223, generalized to the wholesaler tag in
			// LLM-252): wholesaler-tagged sellers (farms, mill) sell only to the
			// village distributor — plus, since LLM-477, to a TRANSFORMER buying an
			// input its own recipes require (the mill's wheat), so a maker isn't
			// forced to buy retail from the same shop it sells to wholesale. Any
			// other buyer is rejected at dispatch and steered to the distributor —
			// the hard backstop beneath the perception filter (eachVendorOffer drops
			// wholesale offers a buyer has no allowance for, so no cue lures a buyer
			// into this rejection). Keys on the SELLER's work anchor, so a hired hand
			// or a seller trading away from the wholesale structure is gated too, and
			// on the BUYER's anchor + policy for the allowance.
			if SellerAtWholesaler(w.VillageObjects, seller.WorkStructureID) &&
				!BuyerMayBuyWholesale(w.VillageObjects, w.Recipes, buyer.WorkStructureID, buyer.RestockPolicy, kind) {
				distributor := DistributorSteerLabel(w.VillageObjects, w.Actors)
				return nil, fmt.Errorf(
					"wholesale goods are sold only to %s, whose shop supplies the village — buy your %s from %s, not straight from the source.",
					distributor, kind, distributor,
				)
			}

			// Merchant visitor errand confinement (LLM-455, generalizing the LLM-410 factor
			// gate): a merchant visitor's TRADE goods move only between him and his errand
			// Counterparty — the keeper who sells the good he buys, or the distributor who
			// absorbs the imports the factor sells. Keyed on the errand's counterparty
			// STRUCTURE, so it covers a villager buying a visitor's wares (visitor = seller),
			// a visitor buying a trade good from anyone but his keeper (visitor = buyer), and
			// both legs of the factor's two-way distributor deal. His SELF-provisioning is
			// exempt: a service (a room, nights_stay) or a consumable (a meal, journeycake) is
			// a bed / supper / road-food for himself, not errand trade, so he still rides the
			// ordinary lodging/eating lifecycle. The perception rounds cue keeps him pointed at
			// his counterparty and the talk-only tool gate strips commerce elsewhere; this is
			// the substrate backstop.
			if steer := TradeErrandSteer(w.VillageObjects, w.Actors, buyer, seller, w.ItemKinds[kind]); steer != "" {
				return nil, errors.New(steer)
			}

			// ZBBS-WORK-403: a purchase of a non-portable consumable always
			// settles eat-here, clamped HERE on the world goroutine so
			// it holds regardless of what the client sent — a failed catalog
			// fetch or a direct API call must not carry stew out of the
			// tavern (the `portable` capability was seeded in the item data
			// precisely to prevent that; code_review). ZBBS-WORK-405 widened
			// the clamp from PC-only to every buyer: v1 gated take-home of
			// non-portables for all actors (v1 pay.go/serve.go rejected it
			// outright), and no valid NPC flow buys non-portables take-home
			// — such a purchase is a config bug, not a disposition to
			// preserve. Sits before the duplicate-offer gate and both settle
			// paths, so the clamped value is the offer's identity
			// everywhere. Service kinds are excluded — they clamp the OTHER
			// way (fast path, WORK-402). eatHereClamped rides the result so
			// an LLM buyer's tool feedback can say what actually happened
			// (the consume-clamp precedent: a silently adjusted action
			// leaves the model believing it carried off goods it never
			// held).
			eatHereClamped := false
			if !consumeNow && w.ItemKinds[kind].EatHereOnly() {
				consumeNow = true
				eatHereClamped = true
			}

			// Consumer resolution. Empty list = buyer is implicit single
			// consumer. Non-empty enforces huddle membership +
			// case-insensitive name resolution + duplicate rejection +
			// seller-as-consumer rejection. Buyer-as-consumer is
			// allowed.
			consumerIDs, err := resolvePayConsumers(w, buyer, sellerID, consumerNames)
			if err != nil {
				return nil, err
			}

			// Resolve the barter goods the buyer offers to pay WITH
			// (ZBBS-HOME-393). Each free-text name → canonical ItemKind;
			// duplicate kinds and non-positive qty reject. Empty for a
			// pure-coin offer.
			resolvedPayItems, err := resolvePayItems(w, payItems)
			if err != nil {
				return nil, err
			}

			// Must offer something. An offer with no coins AND no goods is
			// the free-goods hole (ZBBS-HOME-391) — reject it. Coins-only,
			// goods-only, and mixed all pass.
			if amount == 0 && len(resolvedPayItems) == 0 {
				return nil, errors.New(
					"an offer must include coins or goods — set an amount, add pay_items, or both.",
				)
			}

			// Lodging-shape intake gates (ZBBS-WORK-343 + WORK-344). Both
			// reject upfront rather than commit coin into an Order that
			// deliver_order will refuse forever (failure mode: Order stays
			// Ready, keeper LLM burns ticks retrying).
			//
			// Keyed on the "lodging" capability rather than hardcoded
			// "nights_stay" so a future operator-defined lodging kind
			// inherits both guards. Matches deliver_order's own capability
			// check at order_commands.go.
			if itemHasCapability(w, kind, "lodging") {
				// LLM-391 — a room is delivered as an Order + RoomAccess grant,
				// never eaten on the spot, so consume_now is meaningless for
				// lodging. Normalize it to the booking shape HERE, before the
				// WORK-344 consumer gate, the consume_now/advance-booking check,
				// and the fast/slow split all read it. Left unnormalized, a
				// consume_now nights_stay settled the coins in commitPayTransfer's
				// eat-on-the-spot branch and granted NO room: the guest paid for a
				// bed it never held, appeared unhoused once its prior grant lapsed,
				// re-booked the same night, and the second delivered
				// (buyer, seller, ready_by) row collided with the first on
				// pay_ledger_lodging_active_once — wedging every checkpoint (the
				// 2026-07-12 Ezekiel/Hannah incident). The fast path already forces
				// this for service kinds (runPayWithItemFastPath); the slow-path
				// offer intake did not, which was the gap.
				consumeNow = false

				// LLM-182 — buyer-side need-a-room gate. The lodging seek/offer
				// cues are already home-gated in perception
				// (actorSnapIsLodgingSeeker), but nothing stopped a homed villager
				// who FREELANCES a room request (no engine cue prompts it) from
				// minting a real nights_stay purchase — paying for a bed it never
				// uses, then walking home to sleep (Prudence Ward → Ward Residence,
				// live 2026-06-29). Reject before any ledger/coin side effect, the
				// buyer-side mirror of the seller gates below. An active room grant
				// is NOT a disqualifier — only a home is — so a homeless lodger
				// renewing the next night (LLM-46/96) still books.
				if buyer.HomeStructureID != "" {
					if home, ok := w.Structures[buyer.HomeStructureID]; ok && home.DisplayName != "" {
						return nil, fmt.Errorf(
							"you already have a home (%s) — head there to sleep; you don't need to rent a room.",
							home.DisplayName,
						)
					}
					return nil, errors.New(
						"you already have a home — head there to sleep; you don't need to rent a room.",
					)
				}

				// WORK-343 — operator-data guard. A keeper whose
				// work_structure has zero private bedrooms (or no work
				// structure at all) is structurally unable to fulfill any
				// lodging Order. Distinct from "all rooms occupied" — that
				// case is transient and stays at delivery time
				// (AssignBedroomForLodger returns RoomID=0 → "try again
				// shortly"). Zero-rooms is the deliberate v1 scope.
				if seller.WorkStructureID == "" {
					return nil, fmt.Errorf(
						"%s has no work structure set up for lodging — ask an operator to fix.",
						seller.DisplayName,
					)
				}
				sellerStructure, ok := w.Structures[seller.WorkStructureID]
				if !ok {
					return nil, fmt.Errorf(
						"%s's work structure %q not found — ask an operator to fix.",
						seller.DisplayName, seller.WorkStructureID,
					)
				}
				privateRoomCount := 0
				for _, r := range sellerStructure.Rooms {
					if r != nil && r.Kind == RoomKindPrivate {
						privateRoomCount++
					}
				}
				if privateRoomCount == 0 {
					return nil, fmt.Errorf(
						"%s isn't set up for boarding — no bedrooms in their establishment for %s. Ask an operator to add rooms before booking here.",
						seller.DisplayName, kind,
					)
				}

				// WORK-344 — a room booked for a non-buyer consumer is a
				// guaranteed-impossible Order: deliver_order's lodging branch
				// (order_commands.go) enforces a single self-consumer. The
				// redundant consumerNames=[buyer] case is permitted; only
				// non-buyer consumers are rejected. consume_now was normalized
				// to false just above (LLM-391), so this now also closes the
				// former "book a room for someone else with consume_now" hole
				// that the earlier !consumeNow guard let slip through.
				for _, cid := range consumerIDs {
					if cid != buyerID {
						return nil, fmt.Errorf(
							"%s can't be booked for someone else — only the buyer can take the room (drop the consumers list).",
							kind,
						)
					}
				}
			}

			// Mending-shape intake gates (LLM-625) — the lodging block's posture
			// applied to the garment-repair service: reject structurally
			// impossible offers upfront, before any coin/ledger side effect.
			// Transient conditions (the mender's thread stock) stay at the
			// accept gate (10c), the all-rooms-occupied split.
			if itemHasCapability(w, kind, CapabilityMending) {
				// A mend is delivered as an effect on the buyer's wear counters,
				// never eaten — consume_now is meaningless here for the same
				// LLM-391 reason as lodging: left set, the coins settle in the
				// eat-on-the-spot branch and no garment is mended.
				consumeNow = false

				// The mend restores the BUYER's own worn units; delivery
				// enforces a single self-consumer (order_commands.go), so a
				// non-buyer consumer is a guaranteed-impossible order.
				for _, cid := range consumerIDs {
					if cid != buyerID {
						return nil, fmt.Errorf(
							"%s can't be bought for someone else — the buyer brings their own clothes (drop the consumers list).",
							kind,
						)
					}
				}

				// Seller-side structural gate: only a keeper of a mending-tagged
				// structure can mend (the WORK-343 zero-bedrooms shape).
				if !ActorIsMender(w.VillageObjects, StructureID(seller.WorkStructureID)) {
					return nil, fmt.Errorf(
						"%s doesn't take in mending — their shop isn't set up for it.",
						seller.DisplayName,
					)
				}

				// Buyer-side gate, the LLM-182 you-already-have-a-home mirror:
				// paying to mend a wardrobe with nothing worn buys nothing, and
				// wear only accrues, so this can't self-resolve within the
				// offer's TTL.
				if len(WornGarmentKinds(w.ItemKinds, buyer.Inventory, buyer.GarmentWear)) == 0 {
					return nil, errors.New(
						"your clothes have no wear worth paying to mend yet.",
					)
				}
			}

			// Equipment-service intake gates (LLM-648) — the mending block's
			// posture applied to the wright's trade: reject structurally
			// impossible offers upfront, before any coin/ledger side effect.
			// Transient conditions (the wright's whetstone stock) stay at the
			// accept gate, the thread-shortfall split.
			if itemHasCapability(w, kind, CapabilityEquipmentService) {
				// One visit, one service, one price: delivery resets ONE
				// business and draws ONE whetstone, so a qty > 1 offer would
				// charge for units that can never be delivered (code_review).
				if qty != 1 {
					return nil, errors.New(
						"the wright's service is bought one visit at a time — qty must be 1.",
					)
				}
				// The service is delivered as an effect on the buyer's owned
				// business, never eaten — consume_now is meaningless here for
				// the same LLM-391 reason as lodging and mending.
				consumeNow = false

				// The service restores the BUYER's own business; a non-buyer
				// consumer is a guaranteed-impossible order.
				for _, cid := range consumerIDs {
					if cid != buyerID {
						return nil, fmt.Errorf(
							"%s can't be bought for someone else — the wright services the buyer's own business (drop the consumers list).",
							kind,
						)
					}
				}

				// Seller-side structural gate: only a keeper of a wright-tagged
				// structure works the trade.
				if !ActorIsWright(w.VillageObjects, StructureID(seller.WorkStructureID)) {
					return nil, fmt.Errorf(
						"%s doesn't work a wright's trade — no one to service your equipment.",
						seller.DisplayName,
					)
				}

				// Buyer-side gate: paying for a service with nothing due buys
				// nothing, and use only accrues, so this can't self-resolve
				// within the offer's TTL.
				if DueOwnedBusiness(w.VillageObjects, buyer.ID, w.Settings.EquipmentServiceDueThreshold) == nil {
					return nil, errors.New(
						"your equipment isn't due for the wright's service yet.",
					)
				}
			}

			// Resolve the booked date (ZBBS-HOME-403). Advance booking
			// (ready_in_days > 0) is lodging-only — a physical good is handed
			// over when paid for, so a future date would just strand the
			// order — and a counter-response carries the parent's booked date
			// forward (a price haggle never moves the date the buyer asked
			// for). Validated here, before any entry is staked.
			if len(opts) > 1 {
				return nil, errors.New("PayWithItem: at most one options value may be passed")
			}
			var days, depositArg int
			if len(opts) == 1 {
				days = opts[0].ReadyInDays
				depositArg = opts[0].Deposit
			}
			// Deposit (partial-payment commission, LLM-357): a coin-only,
			// take-home offer may put money down now and settle the balance at
			// deliver_order. Guard the shape here; it's only HONORED when the
			// offer resolves to a commission at accept (depositChargeForEntry
			// re-checks), so an unmet deposit degrades harmlessly to full prepay.
			if depositArg < 0 {
				return nil, fmt.Errorf("PayWithItem: deposit cannot be negative (got %d)", depositArg)
			}
			if depositArg > 0 && depositArg >= amount {
				return nil, fmt.Errorf("PayWithItem: deposit %d must be less than the total price %d (a deposit is a partial payment)", depositArg, amount)
			}
			if depositArg > 0 && consumeNow {
				return nil, errors.New("a deposit needs consume_now=false — you can only put money down on a made-to-order good you'll collect later.")
			}
			if depositArg > 0 && len(payItems) > 0 {
				return nil, errors.New("a deposit must be coin-only — pay the down payment in coins, not goods.")
			}
			// Only a genuine partial (0 < deposit < total) rides onto the entry;
			// 0 and "deposit == full price" both mean full prepay (sentinel 0).
			depositForEntry := 0
			if depositArg > 0 && depositArg < amount {
				depositForEntry = depositArg
			}
			// Advance booking requires a deferred order to hold the future date;
			// a consume_now (eat/check-in-on-the-spot) offer mints no Order, so a
			// future ready_in_days would be silently lost. Reject it (days==0 is
			// fine — that's same-day). ZBBS-HOME-403.
			if consumeNow && days > 0 {
				return nil, errors.New(
					"ready_in_days needs consume_now=false — an advance booking is a deferred order, not taken on the spot.",
				)
			}
			// A consume_now offer mints no Order to hold a booked date, so it is
			// always same-day — never carry a parent's future booking onto it
			// (that would write a future ReadyBy onto an accepted entry with no
			// order waiting for it). Only the deferred path resolves carry/advance.
			readyBy := orderDateUTC(at, w.Settings.Location)
			if !consumeNow {
				readyBy, err = resolveOrderReadyBy(w, kind, parentID, days, at)
				if err != nil {
					return nil, err
				}
				// LLM-47: a lodging booking for a night the buyer already holds
				// from this seller (a "renewal") advances past the buyer's held
				// coverage, so it extends the stay rather than double-booking a
				// night — a duplicate (buyer, seller, ready_by) would later collide
				// on pay_ledger_lodging_active_once at delivery and wedge
				// checkpointing. Coverage is read from the buyer's durable RoomAccess
				// grants (advancePastHeldLodging), so it survives the order pruning +
				// restart-load filter that made a w.Orders scan a no-op in prod.
				if itemHasCapability(w, kind, "lodging") {
					readyBy = advancePastHeldLodging(w, buyerID, sellerID, readyBy, at)
					// LLM-84: a SAME-DAY walk-in room may be paid with goods
					// (barter) — it is granted at accept (commitPayTransfer) with
					// no un-occupied booking window that could expire, so there is
					// nothing to refund. A FUTURE booking stays coin-only: it sits
					// as a deferred Order until check-in, and an expired advance
					// booking refunds only the coin Amount (the Order carries no
					// goods leg). ZBBS-HOME-403, narrowed by LLM-84.
					if len(resolvedPayItems) > 0 && readyBy.After(orderDateUTC(at, w.Settings.Location)) {
						return nil, errors.New(
							"a room booked for a future night must be paid in coins — set an amount, or book it for tonight to pay with goods.",
						)
					}
				}
			}

			// Overflow guard on qty * effectiveConsumers — Inventory[kind]
			// is plain int, so a wrapped product could silently pass the
			// stock check.
			effectiveConsumers := effectivePayConsumerCount(consumerIDs)
			if qty > math.MaxInt/effectiveConsumers {
				return nil, fmt.Errorf(
					"PayWithItem: qty %d × %d consumers overflows int — split the order.",
					qty, effectiveConsumers,
				)
			}
			needed := qty * effectiveConsumers

			// in_response_to validation (architecture § 7). Doesn't
			// require the buyer to keep matching terms — they can shift
			// qty/item/consume_now if they want their counter-response
			// to propose different terms.
			if parentID != 0 {
				if err := validateInResponseTo(w, parentID, buyerID, sellerID, at); err != nil {
					return nil, err
				}
			}

			// Fast-path quote matching (scene-quote-design § 8). All six
			// predicates must pass; any failure is a strict-reject tool
			// error.
			if quoteID != 0 {
				res, fastErr := runPayWithItemFastPath(
					w, buyer, seller, sellerID, sceneID, kind, qty,
					amount, consumeNow, consumerIDs, needed, quoteID,
					parentID, forText, at, readyBy,
				)
				if fastErr != nil {
					return nil, fastErr
				}
				return stampEatHereClamped(res, eatHereClamped), nil
			}

			// Opportunistic quote auto-match (ZBBS-HOME-424). The explicit
			// fast path requires the model to echo a quote_id, but a weak
			// buyer model routinely answers a seller's standing quote with a
			// bare pay_with_item — minting a CROSSING offer for goods the
			// seller is already offering. The two pending intents then
			// deadlock: the seller's quote waits on the buyer, the buyer's
			// offer waits on the seller, and the duplicate gate below
			// rejects every retry (observed live: nine minutes to settle a
			// 4-coin water, conversation hud-6c849d…). When a bare coin
			// offer matches an open quote on every term predicate, take the
			// quote instead of minting its mirror image. Barter offers are
			// exempt (a quote is a coin price — ZBBS-HOME-393), as are
			// counter-responses (their lifecycle is the counter chain). On
			// any fast-path failure the offer falls through to the slow
			// path unchanged — runPayWithItemFastPath mutates nothing
			// before all predicates pass, and the strict-reject contract
			// only binds an EXPLICIT quote_id.
			if parentID == 0 && len(resolvedPayItems) == 0 {
				if matchID := findAutoMatchQuote(w, buyer, sellerID, sceneID, kind, qty, amount, consumeNow, consumerIDs, at); matchID != 0 {
					res, fastErr := runPayWithItemFastPath(
						w, buyer, seller, sellerID, sceneID, kind, qty,
						amount, consumeNow, consumerIDs, needed, matchID,
						parentID, forText, at, readyBy,
					)
					if fastErr == nil {
						withdrawCrossingOffers(w, buyerID, sellerID, sceneID, kind, qty, consumeNow, consumerIDs, at)
						return stampEatHereClamped(res, eatHereClamped), nil
					}
				}
			}

			// Cross-tick duplicate-offer gate (ZBBS-WORK-391). The same-tick
			// repeat-offer guard (ZBBS-HOME-395, harness) resets every tick by
			// design, and the buyer-side pending-offers cue (ZBBS-HOME-413) is
			// perception-only — a weak model reads "make no second offer for
			// the same goods while this one stands" and offers again anyway (observed
			// live: Prudence staked three identical meat offers across three
			// ticks while the first sat unanswered, then ate the accepted
			// duplicates back-to-back). This is the ledger-backed rung: a NEW
			// offer matching a still-Pending entry on (buyer, seller, item,
			// disposition) is rejected model-facing. The key deliberately
			// mirrors payOfferKey — price and qty excluded, so a re-offer at
			// drifted terms still matches; counter-responses (parentID != 0)
			// are a distinct lifecycle and exempt, and quote-takes never reach
			// the slow path. Entries past ExpiresAt are skipped rather than
			// blocking the buyer on the sweep's cadence.
			if parentID == 0 {
				for _, e := range w.PayLedger {
					if e == nil || e.State != PayLedgerStatePending {
						continue
					}
					if e.BuyerID != buyerID || e.SellerID != sellerID || e.ItemKind != kind || e.ConsumeNow != consumeNow {
						continue
					}
					// A zero ExpiresAt (legacy/seeded entry) is treated as
					// never-expiring rather than always-expired — the gate
					// must not wave a duplicate through just because an entry
					// predates TTL stamping.
					if !e.ExpiresAt.IsZero() && !at.Before(e.ExpiresAt) {
						continue
					}
					return nil, fmt.Errorf(
						"you already have an offer for %s before %s, awaiting their answer (offer id %d) — wait for their response, or withdraw_pay it before offering again.",
						kind, seller.DisplayName, e.ID,
					)
				}
			}

			// Slow path: mint pending entry + emit PayOfferReceived.
			//
			// Offer-time funds fast-fail (ZBBS-WORK-231) — an
			// OPTIMIZATION, not a correctness gate. Pending entries
			// reserve nothing and AcceptPay's gate 11 stays the
			// authoritative funds check at acceptance time. But minting a
			// pending offer the buyer demonstrably can't cover wastes a
			// seller deliberation tick: the offer stamps the seller's
			// warrant, the seller's reactor burns an LLM round-trip, and
			// accept_pay then resolves to failed_insufficient_funds.
			// Rejecting here spares that tick. This is a point-in-time
			// snapshot — the buyer's balance can still change before
			// accept — so it neither replaces nor weakens gate 11.
			//
			// Stock and seller-break are deliberately NOT fast-failed
			// here: stock is contended (reservation accounting lives at
			// accept) and break is transient, so both stay deferred to
			// acceptance. A pending offer staked against an on-break or
			// out-of-stock seller is harmless — it resolves at accept
			// time, or is withdrawn / expires first.
			if err := payOfferShortfall(w, buyer, amount, qty, resolvedPayItems); err != nil {
				return nil, err
			}

			id := w.nextLedgerSeq()
			ttl := effectivePayLedgerTTL(w.Settings)
			expiresAt := at.Add(ttl)
			depth, parentRefForLineage := 0, LedgerID(0)
			if parentID != 0 {
				if parent := w.PayLedger[parentID]; parent != nil {
					depth = parent.Depth + 1
					parentRefForLineage = parent.ID
				}
			}
			entry := &PayLedgerEntry{
				ID:          id,
				BuyerID:     buyerID,
				SellerID:    sellerID,
				ItemKind:    kind,
				Qty:         qty,
				ConsumeNow:  consumeNow,
				ConsumerIDs: append([]ActorID(nil), consumerIDs...),
				ReadyBy:     readyBy,
				Amount:      amount,
				Deposit:     depositForEntry,
				PayItems:    cloneItemKindQtys(resolvedPayItems),
				QuoteID:     0, // slow path didn't reference a quote
				ParentID:    parentRefForLineage,
				Depth:       depth,
				State:       PayLedgerStatePending,
				CreatedAt:   at,
				ExpiresAt:   expiresAt,
				SceneID:     sceneID,
				HuddleID:    buyer.CurrentHuddleID,
			}
			w.PayLedger[id] = entry

			evt := &PayOfferReceived{
				LedgerID:       id,
				BuyerID:        buyerID,
				SellerID:       sellerID,
				ItemKind:       kind,
				QtyPerConsumer: qty,
				ConsumeNow:     consumeNow,
				ConsumerIDs:    cloneActorIDs(consumerIDs),
				Amount:         amount,
				PayItems:       cloneItemKindQtys(resolvedPayItems),
				QuoteID:        0,
				ParentID:       parentRefForLineage,
				Depth:          depth,
				SceneID:        sceneID,
				HuddleID:       buyer.CurrentHuddleID,
				ExpiresAt:      expiresAt,
				At:             at,
			}
			w.emit(evt)
			entry.RootEventID = evt.RootEventID()
			entry.SourceEventID = evt.EventID()

			return PayWithItemResult{
				LedgerID:           id,
				State:              PayLedgerStatePending,
				FastPath:           false,
				EatHereClamped:     eatHereClamped,
				ReroutedSellerName: reroutedSellerName,
			}, nil
		},
	}
}

// stampEatHereClamped marks a settle-path result with the upstream
// disposition clamp (ZBBS-WORK-405) so tool feedback can state what
// actually happened. The fast path doesn't know about the clamp — it
// received the already-clamped consumeNow — so the stamp happens at the
// call sites that do. No-op when nothing was clamped or the value isn't
// a PayWithItemResult.
func stampEatHereClamped(res any, clamped bool) any {
	if !clamped {
		return res
	}
	if r, ok := res.(PayWithItemResult); ok {
		r.EatHereClamped = true
		return r
	}
	return res
}

// runPayWithItemFastPath validates the six fast-path predicates and, on
// all-pass, mints the entry directly in Accepted state + commits the
// atomic transfer + emits PayWithItemResolved{Accepted}. Any predicate
// failure is a strict-reject tool error (scene-quote-design § 8 — "no
// silent fall-through").
//
// The six predicates:
//
//  1. Quote takeable. Quote exists, State == Active, not past
//     ExpiresAt.
//  2. Buyer eligible. Quote is public OR explicitly targets this
//     buyer.
//  3. Co-presence. Buyer's scene matches quote's scene, AND buyer +
//     seller share a non-empty huddle.
//  4. Exact term match. ItemKind, Qty, and consumer set
//     (order-independent) all agree. ConsumeNow is deliberately NOT
//     matched (ZBBS-WORK-402): disposition is the buyer's term — the
//     buyer's value rides the entry, and service kinds clamp to the
//     service shape.
//  5. Amount at-or-above floor. Quote's Amount is the minimum;
//     overpayment is tipping.
//  6. Independent preconditions — seller stock, buyer coins, seller
//     not on break. These are also revalidated by the slow-path
//     acceptance flow; centralizing here keeps the fast path
//     symmetric.
func runPayWithItemFastPath(
	w *World,
	buyer, seller *Actor,
	sellerID ActorID,
	sceneID SceneID,
	kind ItemKind,
	qty int,
	amount int,
	consumeNow bool,
	consumerIDs []ActorID,
	needed int,
	quoteID QuoteID,
	parentID LedgerID,
	forText string,
	at time.Time,
	readyBy time.Time,
) (any, error) {
	quote, ok := w.Quotes[quoteID]
	if !ok || quote == nil {
		return nil, fmt.Errorf("quote %d not available", quoteID)
	}
	if quote.State != SceneQuoteStateActive {
		return nil, fmt.Errorf("quote %d is no longer active", quoteID)
	}
	if !quote.ExpiresAt.IsZero() && !at.Before(quote.ExpiresAt) {
		return nil, fmt.Errorf("quote %d expired", quoteID)
	}

	if quote.TargetBuyer != "" && quote.TargetBuyer != buyer.ID {
		return nil, fmt.Errorf("quote %d is not for you", quoteID)
	}

	if quote.SellerID != sellerID {
		return nil, fmt.Errorf(
			"quote %d is from a different seller — drop the quote_id or call the right seller.",
			quoteID,
		)
	}

	if buyer.CurrentHuddleID == "" || seller.CurrentHuddleID == "" ||
		buyer.CurrentHuddleID != seller.CurrentHuddleID {
		return nil, fmt.Errorf(
			"need to be in %s's huddle to take quote %d",
			seller.DisplayName, quoteID,
		)
	}
	if sceneID != quote.SceneID {
		return nil, fmt.Errorf(
			"you're not in the same scene as quote %d — get back to the conversation that quote was posted in.",
			quoteID,
		)
	}

	// LLM-101: a multi-line quote is a bundle, taken WHOLE. The buyer's
	// item/qty/consume_now/consumer args don't describe a bundle (the PC modal
	// / perception render echoes a single representative line), so for a bundle
	// they're ignored — the take adopts the quote's lines, disposition, and
	// consumer set. A single-line quote keeps the original exact-term contract
	// (the buyer named the one item they're buying).
	bundle := len(quote.Lines) > 1
	entryConsumeNow := consumeNow
	entryConsumerIDs := consumerIDs
	var entryItemKind ItemKind
	var entryQty int
	var entryLines []QuoteLine
	var deliverLines []QuoteLine

	if bundle {
		entryConsumeNow = quote.ConsumeNow
		entryConsumerIDs = quote.ConsumerIDs
		entryLines = cloneQuoteLines(quote.Lines)
		deliverLines = quote.Lines
	} else {
		// Exact term match against the buyer's named terms. Consumer set
		// comparison is order-independent (matches the supersede key in
		// scene_quote_commands.go).
		line := quote.Lines[0]
		if line.ItemKind != kind {
			// LLM-172: name the quote's actual item and the corrected retry so a
			// misread self-corrects in-place. The bare rejection dead-ended the
			// live trace — the buyer put a carried good ("nail") where the quoted
			// good ("stew") belonged, got this error with no fix, and fell back to
			// a bare pay that leaked coins for nothing.
			return nil, fmt.Errorf(
				"quote %d is for %q, not %q — retry pay_with_item with quote_id %d and item %q; the item is the good the quote sells, not one you're carrying.",
				quoteID, line.ItemKind, kind, quoteID, line.ItemKind,
			)
		}
		if line.Qty != qty {
			return nil, fmt.Errorf(
				"quote %d is for qty %d, not %d — retry pay_with_item with quote_id %d and qty %d.",
				quoteID, line.Qty, qty, quoteID, line.Qty,
			)
		}
		// Disposition is the BUYER's term, not the quote's (ZBBS-WORK-402 —
		// the quote's ConsumeNow survives as the UI row default, but a take no
		// longer has to match it). Service kinds have no choice — no physical
		// good exists, so the engine forces the service shape rather than
		// rejecting a confused caller (mirrors the client's forced handling
		// for bookings, HOME-423).
		if itemHasCapability(w, kind, "service") {
			entryConsumeNow = false
		}
		if !actorIDSetsEqual(quote.ConsumerIDs, consumerIDs) {
			return nil, fmt.Errorf(
				"quote %d has different consumer set — re-check who the quote is for.",
				quoteID,
			)
		}
		entryItemKind = kind
		entryQty = qty
		deliverLines = []QuoteLine{line}
	}

	if amount < quote.Amount {
		return nil, fmt.Errorf(
			"quote %d requires at least %d coins (you offered %d)",
			quoteID, quote.Amount, amount,
		)
	}

	// Independent preconditions — symmetric with AcceptPay.
	if seller.BreakUntil != nil && seller.BreakUntil.After(at) {
		return nil, fmt.Errorf(
			"%s is on break — try again after their break ends.",
			seller.DisplayName,
		)
	}
	// Stock reservation accounting (PR S6 R1): accepted-but-not-yet-delivered
	// Orders keep goods in the seller's inventory, so subtract
	// outstandingReadyOrderQty before comparing against need — otherwise two
	// concurrent fast-path accepts against the same physical stew could both
	// pass. A bundle stock-checks every line independently (LLM-101); a
	// single-line take keeps the original scalar check. "service"-capability
	// items carry no inventory and skip the check (ZBBS-HOME-296) — a bundle
	// can't hold one (rejected at quote creation), so the skip only matters
	// single-item, and the slow-path acceptPendingOffer gate-10 skip mirrors it.
	if bundle {
		effConsumers := effectivePayConsumerCount(entryConsumerIDs)
		for _, ln := range deliverLines {
			if itemHasCapability(w, ln.ItemKind, "service") {
				continue
			}
			if ln.Qty > math.MaxInt/effConsumers {
				return nil, fmt.Errorf(
					"PayWithItem: qty %d × %d consumers overflows int — split the order.",
					ln.Qty, effConsumers,
				)
			}
			need := ln.Qty * effConsumers
			// sellerCoverableStock (LLM-409): on-hand minus goods earmarked for a
			// Ready order — the same coverage predicate quote create and the
			// pre-publish reconcile use.
			if coverable, reserved := sellerCoverableStock(w, seller, ln.ItemKind); coverable < need {
				noteOutOfStock(w, buyer.ID, seller.ID, ln.ItemKind, at)
				return nil, fmt.Errorf(
					"%s doesn't have enough %s (have %d, reserved %d, need %d)",
					seller.DisplayName, ln.ItemKind, seller.Inventory[ln.ItemKind], reserved, need,
				)
			}
		}
	} else if !itemHasCapability(w, kind, "service") {
		// sellerCoverableStock (LLM-409): on-hand minus goods earmarked for a
		// Ready order — the shared coverage predicate.
		available, reserved := sellerCoverableStock(w, seller, kind)
		if available < needed {
			// ZBBS-HOME-363: the buyer walked here and found the seller dry on
			// this item. This fast-path rejects with no ledger entry (the
			// out-of-stock subscriber would miss it), so record the experiential
			// memory inline through the shared recorder.
			noteOutOfStock(w, buyer.ID, seller.ID, kind, at)
			return nil, fmt.Errorf(
				"%s doesn't have enough %s (have %d, reserved %d, need %d)",
				seller.DisplayName, kind, seller.Inventory[kind], reserved, needed,
			)
		}
	}
	// LLM-84: a same-day lodging quote-take grants the room at this accept (the
	// service stock-skip leaves no inventory gate to catch a full inn), so
	// reject up front when no room is grantable — mirrors the slow-path gate
	// 10b. Only a single-item service quote reaches lodging (a bundle can't
	// hold a service kind), so guard on !bundle.
	if !bundle && itemHasCapability(w, kind, "lodging") && !readyBy.After(orderDateUTC(at, w.Settings.Location)) {
		if !lodgingRoomGrantable(w, seller, buyer.ID) {
			return nil, fmt.Errorf(
				"%s has no room free right now — try again shortly.",
				seller.DisplayName,
			)
		}
	}
	// LLM-625: a mending quote-take mends at this accept (the service
	// stock-skip leaves no gate to catch a threadless mender or an unworn
	// wardrobe), so reject up front — mirrors the slow-path gate 10c. Only a
	// single-item service quote reaches mending, the lodging shape above.
	if !bundle && itemHasCapability(w, kind, CapabilityMending) {
		if !ActorIsMender(w.VillageObjects, StructureID(seller.WorkStructureID)) {
			return nil, fmt.Errorf(
				"%s doesn't take in mending — their shop isn't set up for it.",
				seller.DisplayName,
			)
		}
		if seller.Inventory[MendThreadKind] < MendThreadPerMend {
			return nil, fmt.Errorf(
				"%s has no thread to mend with right now — try again once they've restocked.",
				seller.DisplayName,
			)
		}
		if len(WornGarmentKinds(w.ItemKinds, buyer.Inventory, buyer.GarmentWear)) == 0 {
			return nil, errors.New(
				"your clothes have no wear worth paying to mend yet.",
			)
		}
		for _, cid := range entryConsumerIDs {
			if cid != buyer.ID {
				return nil, fmt.Errorf(
					"%s can't be bought for someone else — the buyer brings their own clothes.",
					kind,
				)
			}
		}
	}
	// LLM-648: an equipment-service quote-take delivers at this accept, so
	// reject up front — mirrors the slow-path intake gates, the mending shape
	// above.
	if !bundle && itemHasCapability(w, kind, CapabilityEquipmentService) {
		if qty != 1 {
			return nil, errors.New(
				"the wright's service is bought one visit at a time — qty must be 1.",
			)
		}
		if !ActorIsWright(w.VillageObjects, StructureID(seller.WorkStructureID)) {
			return nil, fmt.Errorf(
				"%s doesn't work a wright's trade — no one to service your equipment.",
				seller.DisplayName,
			)
		}
		if seller.Inventory[WhetstoneKind] < WhetstonesPerService {
			return nil, fmt.Errorf(
				"%s has no whetstone to work with right now — try again once they've restocked.",
				seller.DisplayName,
			)
		}
		if DueOwnedBusiness(w.VillageObjects, buyer.ID, w.Settings.EquipmentServiceDueThreshold) == nil {
			return nil, errors.New(
				"your equipment isn't due for the wright's service yet.",
			)
		}
		for _, cid := range entryConsumerIDs {
			if cid != buyer.ID {
				return nil, fmt.Errorf(
					"%s can't be bought for someone else — the wright services the buyer's own business.",
					kind,
				)
			}
		}
	}
	if !buyerCanAfford(buyer, amount) {
		return nil, fmt.Errorf(
			"insufficient coins (have %d to spend, need %d) — quote a smaller offer.",
			buyer.SpendableCoins(), amount,
		)
	}
	if seller.Coins > math.MaxInt-amount {
		return nil, fmt.Errorf(
			"PayWithItem: would overflow seller balance (have %d, adding %d)",
			seller.Coins, amount,
		)
	}

	// All six predicates passed. Mint entry directly in Accepted state
	// (skips Pending). The entry's CreatedAt and ResolvedAt are both
	// `at` to capture the fast-path's single-instant resolution.
	id := w.nextLedgerSeq()
	depth, parentRefForLineage := 0, LedgerID(0)
	if parentID != 0 {
		if parent := w.PayLedger[parentID]; parent != nil {
			depth = parent.Depth + 1
			parentRefForLineage = parent.ID
		}
	}
	entry := &PayLedgerEntry{
		ID:          id,
		BuyerID:     buyer.ID,
		SellerID:    sellerID,
		ItemKind:    entryItemKind,
		Qty:         entryQty,
		ConsumeNow:  entryConsumeNow,
		ConsumerIDs: append([]ActorID(nil), entryConsumerIDs...),
		Lines:       entryLines,
		ReadyBy:     readyBy,
		Amount:      amount,
		QuoteID:     quoteID,
		ParentID:    parentRefForLineage,
		Depth:       depth,
		State:       PayLedgerStateAccepted,
		CreatedAt:   at,
		ResolvedAt:  at,
		// ExpiresAt left zero — entry is already terminal, sweep skips
		// non-pending entries. The TTL concept doesn't apply to a fast-path
		// accept.
		SceneID:  sceneID,
		HuddleID: buyer.CurrentHuddleID,
	}
	w.PayLedger[id] = entry

	// Atomic transfer. Coin debit first (smaller blast radius if a
	// subsequent step somehow drifts), then item movement + ConsumeNow
	// application + relationship writes. All on the world goroutine,
	// serialized by construction — no rollback needed.
	out, err := commitPayTransfer(w, buyer, seller, entry, at, forText)
	if err != nil {
		// Theoretically unreachable — predicates 6 covered every
		// mutation failure mode. If it ever fires, that's a bug, not
		// a domain error.
		return nil, fmt.Errorf("PayWithItem fast-path transfer: %w", err)
	}
	// LLM-188: record any needs-clamp surplus pocketed to the buyer so the
	// settled-offer perception line can reconcile the eaten-vs-kept split.
	entry.KeptUnits = out.keptToInventory

	// Emit PayWithItemResolved{Accepted}. Fast path skips
	// PayOfferReceived because the offer never sat pending (architecture
	// § 4 + events_pay_with_item.go).
	evt := &PayWithItemResolved{
		LedgerID:       id,
		BuyerID:        buyer.ID,
		SellerID:       sellerID,
		ItemKind:       entryItemKind,
		QtyPerConsumer: entryQty,
		ConsumeNow:     entryConsumeNow,
		ConsumerIDs:    cloneActorIDs(entryConsumerIDs),
		Lines:          cloneQuoteLines(entryLines), // LLM-101: bundle lines (audit / action log)
		Amount:         amount,
		PayItems:       cloneItemKindQtys(entry.PayItems), // LLM-105: settled barter goods (audit)
		TerminalState:  PayTerminalStateAccepted,
		BuyerTookQuote: true, // ZBBS-WORK-420: this IS the instant quote-take path
		IsGift:         entry.IsGift,
		SceneID:        sceneID,
		HuddleID:       buyer.CurrentHuddleID,
		At:             at,
	}
	w.emit(evt)
	entry.RootEventID = evt.RootEventID()
	entry.SourceEventID = evt.EventID()

	// LLM-189: close the quote. A take is whole-lot (the exact-qty
	// predicates above passed), so this quote's lot is now sold — flip it
	// to the Taken terminal so it stops rendering as a phantom "standing"
	// offer in the seller's perception (buildStandingQuotesFromMe filters
	// on Active) and can't be auto-matched / explicitly taken again. Both
	// fast-path entries — explicit quote_id and the slow-path auto-match —
	// route through here, so this single site covers both. Mirrors the
	// sweep/supersede terminal transition (flipQuoteTerminal handles the
	// scene-index removal + audit event; SceneQuoteExpired has no
	// subscribers, so the emit is trace-only).
	flipQuoteTerminal(w, w.Scenes[quote.SceneID], quote, SceneQuoteStateTaken, SceneQuoteExpiredReasonTaken, at)

	return PayWithItemResult{
		LedgerID:        id,
		State:           PayLedgerStateAccepted,
		FastPath:        true,
		Lines:           cloneQuoteLines(entryLines),
		BuyerAte:        out.buyerAte,
		KeptToInventory: out.keptToInventory,
		TookHome:        out.tookHome,
		Booked:          out.booked,
		LodgedNow:       out.lodgedNow,
		MendedNow:       out.mendedNow,
		ServicedNow:     out.servicedNow,
		SatisfiesNeed:   out.satisfiesNeed,
		FeltAfter:       out.feltAfter,
		MealMinutes:     out.mealMinutes,
	}, nil
}

// AcceptPay returns the Command for the seller's accept-pending-offer
// path. Phase 3 PR S4 step 5. Runs the 10-gate revalidation matrix
// (active-work § Settled at EOS-26) first-failure-wins, then commits
// the atomic transfer + relationship writes + warrant-stamping event.
//
// The 10 gates, in evaluation order:
//
//  1. Caller exists in the world.
//  2. Ledger entry exists.
//  3. Caller == entry.SellerID (auth — idempotent reject, NO transition).
//  4. entry.State == Pending (state — idempotent reject, NO transition).
//  5. now < entry.ExpiresAt (TTL — flip to Expired terminal).
//  6. Buyer + seller still in entry.HuddleID (co-presence — flip to
//     failed_unavailable). Both halves checked.
//  7. seller.BreakUntil <= now (break — flip to failed_unavailable).
//  8. w.ItemKinds[entry.ItemKind] still present (catalog — flip to
//     failed_unavailable).
//  9. All entry.ConsumerIDs still in entry.HuddleID (consumer
//     departure — flip to failed_unavailable). Skipped when no
//     consumers were specified (buyer-as-implicit-consumer covered by
//     the co-presence gate).
//  10. seller.Inventory[ItemKind] >= Qty * effectiveConsumerCount
//     (stock — reject the accept_pay call with a retryable
//     ModelFacingError naming the shortfall + legal alternatives,
//     LLM-302; NO transition). The failed_insufficient_stock terminal
//     is retained only for CounterPay's coercion path (see
//     acceptPendingOffer's viaAcceptTool).
//  11. buyer.Coins >= entry.Amount (funds — flip to
//     failed_insufficient_funds).
//
// (Gates 10-11 are stock-first / funds-second per active-work — seller-
// knowable state checked first on the seller's tick. The "10 gate"
// nomenclature collapses the auth + state idempotent rejects together;
// the breakdown above expands them for implementation clarity.)
//
// On all-pass: atomic coin debit + item transfer + ConsumeNow per-
// consumer apply, flip entry.State to Accepted, emit
// PayWithItemResolved{Accepted}.
//
// On gate 5-9 / 11 fail: flip entry to the specific terminal state, emit
// PayWithItemResolved with matching TerminalState. NOT a tool error —
// the gate failure IS the terminal resolution. Returns nil err with the
// PayLedgerEntry's new state so callers can inspect the outcome. Gate 10
// (stock) is the exception: through an accept tool call it returns a
// ModelFacingError and leaves the entry Pending (LLM-302).
func AcceptPay(callerID ActorID, ledgerID LedgerID, at time.Time) Command {
	return acceptPayCommand(callerID, ledgerID, at, false)
}

// acceptPayCommand is the shared accept path for AcceptPay (expectGift false)
// and AcceptGift (expectGift true). The expectGift flag enforces the
// verb/disposition match (LLM-138) right after the entry is resolved, keeping
// accept_gift and accept_pay mutually exclusive at the SUBSTRATE — not merely at
// the gateTools advertising layer — so a model can't cross gift/buy semantics at
// resolution time by passing a wrong-kind ledger_id.
func acceptPayCommand(callerID ActorID, ledgerID LedgerID, at time.Time, expectGift bool) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Gate 1: caller exists.
			caller, ok := w.Actors[callerID]
			if !ok {
				return nil, fmt.Errorf("AcceptPay: caller %q not in world", callerID)
			}

			// Gate 2: entry exists.
			entry, ok := w.PayLedger[ledgerID]
			if !ok || entry == nil {
				return nil, fmt.Errorf(
					"AcceptPay: ledger %d not found — re-check the ledger_id.",
					ledgerID,
				)
			}

			// Verb/disposition match (LLM-138): accept_pay must not resolve a
			// gift, and accept_gift must not resolve a purchase offer.
			if entry.IsGift != expectGift {
				if expectGift {
					return nil, fmt.Errorf("offer %d is not a gift — use accept_pay to answer a purchase offer.", ledgerID)
				}
				return nil, fmt.Errorf("offer %d is a gift — use accept_gift to take it.", ledgerID)
			}

			// Gate 3: auth (idempotent reject — NO transition).
			if entry.SellerID != callerID {
				return nil, fmt.Errorf(
					"AcceptPay: only the seller of ledger %d may accept it",
					ledgerID,
				)
			}

			// Gate 4: state idempotent reject (NO transition).
			if entry.State != PayLedgerStatePending {
				return nil, fmt.Errorf(
					"AcceptPay: ledger %d is no longer pending (currently %s) — nothing to accept.",
					ledgerID, entry.State,
				)
			}

			// Gate 4b: no duplicate undelivered room (LLM-89). A nights_stay
			// grant is created only at deliver_order, so between accept and
			// deliver the buyer holds no grant and nothing else stops the
			// keeper accepting a SECOND room offer from the same buyer —
			// minting a duplicate order that double-charges them for one stay
			// (gate 10's stock reservation is skipped for service items, so it
			// can't catch this). Reject while an undelivered room from this
			// keeper to this buyer is still outstanding; hand that one over
			// first. Idempotent reject (NO transition) — the offer stays
			// pending so it can be accepted once the prior is delivered (a
			// genuine next night).
			if itemHasCapability(w, entry.ItemKind, "lodging") {
				if priorID, dup := undeliveredLodgingOrderFor(w, callerID, entry.BuyerID); dup {
					who := string(entry.BuyerID)
					if b := w.Actors[entry.BuyerID]; b != nil && b.DisplayName != "" {
						who = b.DisplayName
					}
					return nil, fmt.Errorf(
						"AcceptPay: %s already holds an undelivered room from you (order #%d) — hand that over with deliver_order before selling another night.",
						who, priorID,
					)
				}
			}

			// Gates 5-11 + the atomic transfer / flip / emit live in
			// acceptPendingOffer, shared with CounterPay's
			// non-increasing-counter coercion (a seller counter at or
			// below the offered amount is "yes, deal" and resolves as an
			// accept). Gates 1-4 above are AcceptPay-specific (auth +
			// state idempotent rejects) and stay inline. viaAcceptTool
			// true (LLM-302): an accept-time stock shortfall surfaces as a
			// retryable ModelFacingError, not a silent terminal.
			state, err := acceptPendingOffer(w, caller, entry, at, true)
			return state, err
		},
	}
}

// acceptPendingOffer runs AcceptPay gates 5-11 (TTL, co-presence, seller
// break, ItemKind catalog, consumer departure, stock, funds) on an
// already-pending entry, first-failure-wins, then on all-pass commits
// the atomic transfer, flips the entry to Accepted, and emits
// PayWithItemResolved{Accepted}. On a gate 5-11 failure it flips the
// entry to the matching terminal and emits PayWithItemResolved with that
// state — NOT a tool error; the gate failure IS the resolution. Returns
// the entry's resulting state.
//
// The caller guarantees gates 1-4 (caller exists, entry exists,
// seller == entry.SellerID, entry.State == Pending). seller is the
// accepting party (== entry.SellerID).
//
// Shared by AcceptPay (the seller's explicit accept) and CounterPay's
// non-increasing-counter coercion. Both reach this point having already
// verified the same four preconditions, and both want identical
// accept-time semantics, so the gate matrix lives here once.
//
// viaAcceptTool (LLM-302) distinguishes the two callers at the ONE gate
// where they should diverge: the bought-item stock check (gate 10). When
// the seller reached here through their own accept_pay / accept_gift tool
// call (viaAcceptTool true), a stock shortfall is knowable at accept time
// and is the seller's to fix — so reject the call with a retryable
// ModelFacingError naming the shortfall and the legal alternatives
// (decline_pay / counter_pay) instead of flipping to a silent
// failed_insufficient_stock terminal. A soft "[ok] … the sale fell
// through" terminal echo (ZBBS-WORK-432) reads as agreement to the weak
// stateful model, which kept "accepting" goods it never held (the live
// Josiah×Elizabeth nails episode). CounterPay's coercion path passes
// false and keeps the terminal flip — it is the only remaining producer
// of failed_insufficient_stock, the retained backstop. Every OTHER gate
// (TTL, co-presence, break, catalog, funds, goods) is unaffected by the
// flag: those flip to their terminal regardless of caller.
func acceptPendingOffer(w *World, seller *Actor, entry *PayLedgerEntry, at time.Time, viaAcceptTool bool) (PayLedgerState, error) {
	// Gate 5: TTL. From here on, gate failures DRIVE terminal
	// transitions rather than idempotent rejects.
	if !entry.ExpiresAt.IsZero() && !at.Before(entry.ExpiresAt) {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateExpired, "", at), nil
	}

	// Gate 6: co-presence. Both buyer and seller must still be
	// in entry.HuddleID. (Architecture § 3 — accept requires
	// co-presence; offer creation captured HuddleID and we
	// re-check it here.)
	buyer, buyerOK := w.Actors[entry.BuyerID]
	if !buyerOK ||
		buyer.CurrentHuddleID != entry.HuddleID ||
		seller.CurrentHuddleID != entry.HuddleID ||
		entry.HuddleID == "" {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
	}

	// Gates 7–10b are the bought-item / commerce preconditions (the
	// recipient's break, stall, the bought ItemKind's catalog presence,
	// stock, and lodging-room availability). A gift (LLM-138) carries no
	// bought item — ItemKind is empty, the recipient provides nothing — so
	// these are skipped for it. Gate 12 below still revalidates the GIVER
	// holds the gift goods (entry.PayItems), which is the real gift
	// precondition. Co-presence (gate 6) already ran and applies to a gift too.

	// Gate 7: seller break (simple-strict, ledger-substrate § 11).
	if !entry.IsGift && seller.BreakUntil != nil && seller.BreakUntil.After(at) {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
	}

	// Gate 8: ItemKind catalog still has this kind (skipped for a gift — it has
	// no bought item, so entry.ItemKind is empty).
	if !entry.IsGift {
		if _, ok := w.ItemKinds[entry.ItemKind]; !ok {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
	}

	// Gate 9: consumer departure. Only relevant when a non-
	// empty consumer set was specified (buyer-as-implicit-
	// consumer is covered by gate 6's co-presence check).
	if len(entry.ConsumerIDs) > 0 {
		if !allConsumersInHuddle(w, entry.HuddleID, entry.ConsumerIDs) {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
	}

	// Gate 10: stock. Skipped for "service"-capability items (e.g.
	// nights_stay), which carry no inventory — infinite-stock
	// (ZBBS-HOME-296). Must mirror the fast-path skip in
	// runPayWithItemFastPath so the two paths agree. Funds (gate 11),
	// co-presence, catalog, TTL, and counter-chain gates all still run
	// for service items — only the stock/inventory check is bypassed.
	if !entry.IsGift && !itemHasCapability(w, entry.ItemKind, "service") {
		effectiveConsumers := effectivePayConsumerCount(entry.ConsumerIDs)
		// Defensive overflow guard — entry.Qty was capped at intake,
		// but a future repo could load entries with whatever shape;
		// re-check before the multiplication.
		if effectiveConsumers > 0 && entry.Qty > math.MaxInt/effectiveConsumers {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
		needed := entry.Qty * effectiveConsumers
		// Stock reservation accounting (PR S6 R1 code_review fix):
		// subtract Ready-Order obligations on this seller+item so
		// two pending offers against the same physical stock cannot
		// both accept. See outstandingReadyOrderQty in order.go.
		have := seller.Inventory[entry.ItemKind]
		reserved := outstandingReadyOrderQty(w, seller.ID, entry.ItemKind)
		// LLM-338: a stock shortfall on a good the seller MAKES is a commission,
		// not a dead-end — accept it and let commitPayTransfer mint a deferred
		// Order the keeper forges and hands over via deliver_order (gate 5 stock
		// is the readiness gate; refund-on-expiry covers "never made"). Only a
		// NON-commission shortfall (a good the seller doesn't produce, a barter
		// offer, a service/lodging item) still rejects here.
		if have-reserved < needed && !isCommissionOrder(w, seller, entry) {
			// LLM-302: a stock shortfall reached through the seller's own
			// accept tool call is a knowable-now, seller-fixable rejection —
			// hand the model a retryable error naming the shortfall and its
			// legal next moves instead of a silent terminal it misreads as a
			// yes. The counter-coercion caller (viaAcceptTool false) keeps the
			// terminal flip as the retained backstop.
			if viaAcceptTool {
				return entry.State, ModelFacingError{Msg: acceptStockShortfallMessage(entry.ItemKind, have, reserved, needed)}
			}
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedInsufficientStock, "", at), nil
		}
	}

	// Gate 10b (LLM-84): same-day walk-in lodging. A room is assigned at
	// THIS accept (commitPayTransfer grants it eagerly, like physical goods),
	// not deferred to a keeper check-in — so if no private room is grantable
	// right now (none at all, or every one occupied by another), fail BEFORE
	// the commit takes payment rather than charge for a room we can't grant.
	// A FUTURE reservation skips this: it mints a deferred Order and the room
	// is assigned at deliver_order on the booked day. This is the lodging
	// analog of the (service-skipped) stock gate above.
	if !entry.IsGift && itemHasCapability(w, entry.ItemKind, "lodging") && !isAdvanceLodgingBooking(w, entry, at) {
		if !lodgingRoomGrantable(w, seller, entry.BuyerID) {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
	}

	// Gate 10c (LLM-625): mending. The garment repair happens at THIS accept
	// (commitPayTransfer delivers it eagerly, like a same-day room) and coins
	// move before the delivery branch runs, so every commit-time precondition
	// must reject HERE, before payment: the seller works at a mending shop,
	// holds thread, and the buyer actually has something worn. The thread
	// shortfall is seller-fixable, so the accept-tool caller gets a retryable
	// error naming it (the gate-10 stock posture); the rest terminal-flip.
	if !entry.IsGift && itemHasCapability(w, entry.ItemKind, CapabilityMending) {
		if !ActorIsMender(w.VillageObjects, StructureID(seller.WorkStructureID)) {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
		if seller.Inventory[MendThreadKind] < MendThreadPerMend {
			if viaAcceptTool {
				return entry.State, ModelFacingError{Msg: fmt.Sprintf(
					"You have no %s to mend with — buy some before taking mending work.", MendThreadKind)}
			}
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedInsufficientStock, "", at), nil
		}
		if len(WornGarmentKinds(w.ItemKinds, buyer.Inventory, buyer.GarmentWear)) == 0 {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
	}

	// Gate 10d (LLM-648): equipment service — the mending gate's shape for the
	// wright's trade. The whetstone shortfall is seller-fixable, so the
	// accept-tool caller gets a retryable error naming it; the rest
	// terminal-flip.
	if !entry.IsGift && itemHasCapability(w, entry.ItemKind, CapabilityEquipmentService) {
		if entry.Qty != 1 {
			// Delivery resets ONE business and draws ONE whetstone — a multi-
			// unit entry would charge for units that can never be delivered.
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
		if !ActorIsWright(w.VillageObjects, StructureID(seller.WorkStructureID)) {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
		if seller.Inventory[WhetstoneKind] < WhetstonesPerService {
			if viaAcceptTool {
				return entry.State, ModelFacingError{Msg: fmt.Sprintf(
					"You have no %s to work with — buy one before taking service work.", WhetstoneKind)}
			}
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedInsufficientStock, "", at), nil
		}
		if DueOwnedBusiness(w.VillageObjects, entry.BuyerID, w.Settings.EquipmentServiceDueThreshold) == nil {
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
		}
	}

	// Gate 11: funds. buyerCanAfford is the shared predicate; the
	// failure ACTION here is a terminal flip (an entry already
	// exists), not the tool-error reject the offer-time sites use.
	// LLM-357: a partial-payment commission only needs the deposit up front —
	// the balance is gated again at deliver_order. depositCharge is the full
	// Amount for every non-partial offer.
	depositCharge := depositChargeForEntry(w, seller, entry)
	if !buyerCanAfford(buyer, depositCharge) {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedInsufficientFunds, "", at), nil
	}
	// Gate 12: barter goods (ZBBS-HOME-393). The buyer must still hold
	// every PayItem they offered to pay WITH — and hold it SPARE (the
	// spoken-for reservation, LLM-636), so a counter that named the buyer's
	// makings, or a pack that shrank into its reserve since the mint, cannot
	// move goods the carry line called not for trade. Like funds, this is a
	// drift backstop — the mint fast-fail already rejected an
	// uncoverable offer, but the buyer's holdings can change between
	// mint and accept. Flip to the goods-specific terminal so admin /
	// telemetry can distinguish a goods shortfall from a coin one.
	if !buyerCanSparePayItems(w, buyer, entry.PayItems) {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedInsufficientGoods, "", at), nil
	}
	// Seller balance overflow guard — symmetric with PR B's Pay
	// and the fast-path predicate 6. Guards the coin actually credited now
	// (the deposit for a partial-payment commission; LLM-357).
	if seller.Coins > math.MaxInt-depositCharge {
		return finalizePayLedgerTerminal(w, entry, PayTerminalStateFailedUnavailable, "", at), nil
	}

	// All gates pass. Atomic transfer + state flip + emit.
	out, err := commitPayTransfer(w, buyer, seller, entry, at, "")
	if err != nil {
		// Theoretically unreachable — gates covered every path.
		return entry.State, fmt.Errorf("acceptPendingOffer: transfer for ledger %d: %w", entry.ID, err)
	}
	entry.State = PayLedgerStateAccepted
	entry.ResolvedAt = at
	// LLM-188: record any needs-clamp surplus pocketed to the buyer so the
	// settled-offer perception line can reconcile the eaten-vs-kept split.
	entry.KeptUnits = out.keptToInventory
	// ZBBS-HOME-417: a completed transaction is conversational activity —
	// reset the huddle's silence-sweep dormancy clock so a busy-but-quiet
	// commerce huddle isn't concluded mid-trade. (Speech usually accompanies
	// commerce here, but a silent accept must still count.) It is also
	// non-conversational PROGRESS (LLM-159), so touchHuddleProgress stamps both
	// clocks and the loop sweep spares the trading huddle too.
	touchHuddleProgress(w, entry.HuddleID, at)

	evt := &PayWithItemResolved{
		LedgerID:       entry.ID,
		BuyerID:        entry.BuyerID,
		SellerID:       entry.SellerID,
		ItemKind:       entry.ItemKind,
		QtyPerConsumer: entry.Qty,
		ConsumeNow:     entry.ConsumeNow,
		ConsumerIDs:    cloneActorIDs(entry.ConsumerIDs),
		Amount:         entry.Amount,
		PayItems:       cloneItemKindQtys(entry.PayItems), // LLM-105: settled barter goods (audit)
		TerminalState:  PayTerminalStateAccepted,
		IsGift:         entry.IsGift,
		SceneID:        entry.SceneID,
		HuddleID:       entry.HuddleID,
		At:             at,
	}
	w.emit(evt)

	return entry.State, nil
}

// acceptStockShortfallMessage renders the accept_pay tool-error copy (LLM-302)
// for a seller who cannot fill the bought item at accept time. It names the
// shortfall and the two legal next moves (decline_pay / counter_pay) so the
// weak stateful model has a copyable next action instead of a silent terminal
// it misreads as agreement. accept_gift rides the same viaAcceptTool=true path
// but never reaches here — a gift carries no bought item, so gate 10 is skipped
// for it.
//
// have is the seller's physical holding of kind; reserved is the quantity
// already promised to Ready Orders (outstandingReadyOrderQty); needed is the
// quantity this offer requires (Qty × effective consumers). The zero-holding
// case gets the short "you hold no X" phrasing from the ticket; any partial or
// reservation-driven shortfall gets the transparent (have/reserved/need)
// breakdown that mirrors the mint-time reject in runPayWithItemFastPath. The
// item kind renders raw — the display-noun helper lives in the perception
// package, which sim cannot import, and the sibling commerce rejections render
// ItemKind the same way.
func acceptStockShortfallMessage(kind ItemKind, have, reserved, needed int) string {
	if have <= 0 {
		return fmt.Sprintf(
			"you hold no %s — you can't accept this trade; decline_pay it, or counter_pay with what you can actually give.",
			kind,
		)
	}
	return fmt.Sprintf(
		"you don't have enough %s to fill this trade (have %d, reserved %d, need %d) — decline_pay it, or counter_pay with what you can actually give.",
		kind, have, reserved, needed,
	)
}

// DeclinePay returns the Command for the seller's pending-decline path.
// Phase 3 PR S4 step 5. Three gates first-failure-wins:
//
//  1. Caller exists, ledger exists, caller == entry.SellerID (auth
//     idempotent reject — tool error, NO transition).
//  2. entry.State == Pending (state idempotent reject — tool error,
//     NO transition).
//  3. reason rune-length ≤ MaxPayMessageRunes.
//
// No co-presence / break / catalog / stock / funds gates — decline is a
// seller-side rejection with no transfer. A seller can decline a
// pending offer even after wandering off (architecture § 3 — only
// accept requires co-presence).
//
// On all-pass: flip entry to Declined terminal, populate entry.Message
// with the decline reason (trim + rune-truncate), emit
// PayWithItemResolved{Declined}. Returns the new state.
func DeclinePay(callerID ActorID, ledgerID LedgerID, reason string, at time.Time) Command {
	return declinePayCommand(callerID, ledgerID, reason, at, false)
}

// declinePayCommand is the shared decline path for DeclinePay (expectGift false)
// and DeclineGift (expectGift true) — the verb/disposition boundary (LLM-138),
// mirroring acceptPayCommand.
func declinePayCommand(callerID ActorID, ledgerID LedgerID, reason string, at time.Time, expectGift bool) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if _, ok := w.Actors[callerID]; !ok {
				return nil, fmt.Errorf("DeclinePay: caller %q not in world", callerID)
			}
			entry, ok := w.PayLedger[ledgerID]
			if !ok || entry == nil {
				return nil, fmt.Errorf(
					"DeclinePay: ledger %d not found — re-check the ledger_id.",
					ledgerID,
				)
			}
			// Verb/disposition match (LLM-138), mirroring acceptPayCommand.
			if entry.IsGift != expectGift {
				if expectGift {
					return nil, fmt.Errorf("offer %d is not a gift — use decline_pay to answer a purchase offer.", ledgerID)
				}
				return nil, fmt.Errorf("offer %d is a gift — use decline_gift to turn it down.", ledgerID)
			}
			if entry.SellerID != callerID {
				return nil, fmt.Errorf(
					"DeclinePay: only the seller of ledger %d may decline it",
					ledgerID,
				)
			}
			if entry.State != PayLedgerStatePending {
				return nil, fmt.Errorf(
					"DeclinePay: ledger %d is no longer pending (currently %s) — nothing to decline.",
					ledgerID, entry.State,
				)
			}
			normalizedReason := truncatePayMessage(reason)
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateDeclined, normalizedReason, at), nil
		},
	}
}

// CounterPay returns the Command for the seller's pending-counter path.
// Phase 3 PR S4 step 5. Five gates first-failure-wins:
//
//  1. Caller exists.
//  2. Ledger exists.
//  3. Caller == entry.SellerID (auth — tool error, NO transition).
//  4. entry.State == Pending (state — tool error, NO transition).
//  5. counterAmount in [1, MaxPayWithItemAmount].
//  6. message rune-length ≤ MaxPayMessageRunes.
//
// No co-presence / break / catalog / stock / funds gates — counter is
// terms-only; the buyer's optional response via PayWithItem
// (in_response_to=parent_id) is what re-runs validation.
//
// On all-pass: flip parent entry to Countered terminal, populate
// entry.CounterAmount + entry.CounterPayItems with the seller's terms +
// entry.Message with the trimmed/truncated counter text + entry.ResolvedAt,
// emit PayCountered (NOT PayWithItemResolved — distinct event family per
// EOS-26 architecture lock).
//
// Symmetric barter (ZBBS-HOME-393): a counter may propose coins
// (counterAmount), goods (counterPayItems), or both — but must propose at
// least one. counterAmount is now optional (>= 0): a seller can counter
// with pure goods terms ("I want 6 nails, not 5"). The buyer's optional
// response is a fresh PayWithItem (in_response_to=parent_id) restating
// whatever payment they choose.
//
// PayCountered.OriginalAmount carries the buyer's original coin offer for
// the buyer's perception prompt; CounterAmount + CounterPayItems carry the
// seller's counter terms.
func CounterPay(callerID ActorID, ledgerID LedgerID, counterAmount int, counterPayItems []PayItemInput, message string, at time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			caller, ok := w.Actors[callerID]
			if !ok {
				return nil, fmt.Errorf("CounterPay: caller %q not in world", callerID)
			}
			entry, ok := w.PayLedger[ledgerID]
			if !ok || entry == nil {
				return nil, fmt.Errorf(
					"CounterPay: ledger %d not found — re-check the ledger_id.",
					ledgerID,
				)
			}
			if entry.SellerID != callerID {
				return nil, fmt.Errorf(
					"CounterPay: only the seller of ledger %d may counter it",
					ledgerID,
				)
			}
			if entry.State != PayLedgerStatePending {
				return nil, fmt.Errorf(
					"CounterPay: ledger %d is no longer pending (currently %s) — nothing to counter.",
					ledgerID, entry.State,
				)
			}
			if counterAmount < 0 {
				return nil, fmt.Errorf("CounterPay: counter amount cannot be negative (got %d)", counterAmount)
			}
			if counterAmount > MaxPayWithItemAmount {
				return nil, fmt.Errorf(
					"CounterPay: counter amount exceeds maximum (got %d, max %d)",
					counterAmount, MaxPayWithItemAmount,
				)
			}
			if len(counterPayItems) > MaxPayWithItemPayItems {
				return nil, fmt.Errorf(
					"CounterPay: too many goods lines (got %d, max %d) — combine into fewer lines.",
					len(counterPayItems), MaxPayWithItemPayItems,
				)
			}
			resolvedCounterItems, err := resolvePayItems(w, counterPayItems)
			if err != nil {
				return nil, err
			}
			// The counter names goods the BUYER would hand over, so it is the
			// buyer's spare that governs (LLM-636): a seller asking for the
			// buyer's makings or the coat on their back is refused here, with a
			// steer, rather than minting a counter the buyer's accept then fails
			// on gate 12 as a wordless "the sale fell through". Coverage of the
			// counter's quantities against the buyer's raw holdings is NOT checked
			// here — the buyer's holdings can change before they answer, and gate
			// 12 owns that at accept; only the reservation, which is a standing
			// fact about the buyer's trade, is worth refusing up front.
			if buyer := w.Actors[entry.BuyerID]; buyer != nil {
				for _, short := range spokenForShortfall(w, buyer, resolvedCounterItems) {
					spokenFor := SpokenFor(w.ItemKinds, w.Recipes, LiveBarterHolder(w, buyer))
					if held := buyer.Inventory[short.Kind]; held >= short.Qty {
						return nil, fmt.Errorf(
							"%s can spare only %d of their %d %s — %s. Counter for goods they can spare, or coins.",
							buyer.DisplayName, SpareQty(buyer.Inventory, spokenFor, short.Kind), held, short.Kind,
							theirSpokenForClause(spokenFor[short.Kind].Reason),
						)
					}
				}
			}
			// A counter must propose something — coins, goods, or both
			// (symmetric with the offer-side rule, ZBBS-HOME-393).
			if counterAmount == 0 && len(resolvedCounterItems) == 0 {
				return nil, errors.New(
					"a counter must propose coins or goods — set an amount, add pay_items, or both.",
				)
			}

			// Non-increasing-counter coercion (v1 LLM-behavior scar,
			// observed 2026-05-08). A seller "countering" at or below the
			// buyer's offered amount isn't proposing a new price — it's
			// agreeing ("I can let you have it for 1 coin" at the offered
			// price, or volunteering a discount). Treat it as an accept at
			// the buyer's offered amount rather than recording a pointless
			// counter the buyer then has to re-accept. The counter message
			// is dropped — no undermining counter-speak on what is really a
			// yes. Gates 1-4 are already satisfied above (caller exists,
			// entry exists, caller == seller, state == pending), so the
			// shared accept path takes it from gate 5.
			//
			// Coercion applies ONLY to pure-coin haggles: if either the
			// original offer OR the counter involves goods (ZBBS-HOME-393),
			// the coin comparison is meaningless (you can't say "5 nails <=
			// 4 coins"), so any goods-bearing counter is a real counter and
			// flips to Countered for the buyer to weigh.
			pureCoinHaggle := len(entry.PayItems) == 0 && len(resolvedCounterItems) == 0
			if pureCoinHaggle && counterAmount <= entry.Amount {
				// viaAcceptTool false (LLM-302): this coercion is a counter_pay
				// call, not an accept_pay one, so a stock shortfall keeps the
				// failed_insufficient_stock terminal flip — the retained backstop.
				state, err := acceptPendingOffer(w, caller, entry, at, false)
				return state, err
			}

			normalizedMessage := truncatePayMessage(message)
			entry.State = PayLedgerStateCountered
			entry.CounterAmount = counterAmount
			entry.CounterPayItems = cloneItemKindQtys(resolvedCounterItems)
			entry.Message = normalizedMessage
			entry.ResolvedAt = at

			evt := &PayCountered{
				ParentID:        entry.ID,
				BuyerID:         entry.BuyerID,
				SellerID:        entry.SellerID,
				ItemKind:        entry.ItemKind,
				QtyPerConsumer:  entry.Qty,
				ConsumeNow:      entry.ConsumeNow,
				ConsumerIDs:     cloneActorIDs(entry.ConsumerIDs),
				OriginalAmount:  entry.Amount,
				CounterAmount:   counterAmount,
				CounterPayItems: cloneItemKindQtys(resolvedCounterItems),
				Message:         normalizedMessage,
				SceneID:         entry.SceneID,
				HuddleID:        entry.HuddleID,
				At:              at,
			}
			w.emit(evt)

			// Bidirectional relationship writes (KindNPCShared gate
			// filters which writes persist). Counter is a non-trivial
			// social move — worth capturing on both sides.
			buyerName := actorDisplayName(w, entry.BuyerID)
			sellerName := actorDisplayName(w, entry.SellerID)
			buyerFact := payCounteredFactText(buyerName, sellerName, entry.Amount, entry.PayItems, counterAmount, resolvedCounterItems, entry.ItemKind, entry.Qty, normalizedMessage, true)
			sellerFact := payCounteredFactText(buyerName, sellerName, entry.Amount, entry.PayItems, counterAmount, resolvedCounterItems, entry.ItemKind, entry.Qty, normalizedMessage, false)
			if _, err := RecordInteraction(entry.BuyerID, entry.SellerID, InteractionCounteredBy, buyerFact, at).Fn(w); err != nil {
				log.Printf("sim.CounterPay: RecordInteraction buyer→seller %q→%q: %v", entry.BuyerID, entry.SellerID, err)
			}
			if _, err := RecordInteraction(entry.SellerID, entry.BuyerID, InteractionCountered, sellerFact, at).Fn(w); err != nil {
				log.Printf("sim.CounterPay: RecordInteraction seller→buyer %q→%q: %v", entry.SellerID, entry.BuyerID, err)
			}
			return entry.State, nil
		},
	}
}

// WithdrawPay returns the Command for the buyer's pending-withdraw path.
// Phase 3 PR S4 step 5 (ledger-substrate § 9 — bundled into S4 to keep
// the offer state machine complete in one PR).
//
// Three gates first-failure-wins:
//
//  1. Caller exists, ledger exists, caller == entry.BuyerID (auth —
//     tool error, NO transition; buyer-callable only).
//  2. entry.State == Pending (state — tool error, NO transition).
//  3. message rune-length ≤ MaxPayMessageRunes.
//
// No co-presence — withdraw is unilateral; the buyer can withdraw an
// offer they made even after wandering off (ledger-substrate § 9).
//
// On all-pass: flip entry to WithdrawnByBuyer terminal, populate
// entry.Message with the trimmed/truncated withdraw note, emit
// PayWithItemResolved{WithdrawnByBuyer}.
func WithdrawPay(callerID ActorID, ledgerID LedgerID, message string, at time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			if _, ok := w.Actors[callerID]; !ok {
				return nil, fmt.Errorf("WithdrawPay: caller %q not in world", callerID)
			}
			entry, ok := w.PayLedger[ledgerID]
			if !ok || entry == nil {
				return nil, fmt.Errorf(
					"WithdrawPay: ledger %d not found — re-check the ledger_id.",
					ledgerID,
				)
			}
			if entry.BuyerID != callerID {
				return nil, fmt.Errorf(
					"WithdrawPay: only the buyer of ledger %d may withdraw it",
					ledgerID,
				)
			}
			if entry.State != PayLedgerStatePending {
				return nil, fmt.Errorf(
					"WithdrawPay: ledger %d is no longer pending (currently %s) — nothing to withdraw.",
					ledgerID, entry.State,
				)
			}
			normalizedMessage := truncatePayMessage(message)
			return finalizePayLedgerTerminal(w, entry, PayTerminalStateWithdrawnByBuyer, normalizedMessage, at), nil
		},
	}
}

// ---- shared helpers --------------------------------------------------

// callerSellsItemTo reports whether `caller` is, in this huddle/scene, the
// SELLER of `kind` to `counterparty` — the signal the reverse-pay role-gate
// (LLM-189) rejects a pay_with_item on. Two arms, either sufficient, each
// requiring CONCRETE direction (caller→counterparty), not mere visibility:
//
//   - an ACTIVE sell quote the caller posted that is TARGETED AT the
//     counterparty in this scene and offers `kind` (any bundle line); or
//   - an ACCEPTED sale in the CURRENT huddle where the caller was the seller
//     and the counterparty the buyer of `kind` (the just-closed deal the
//     model misreads as still open and tries to "settle" with the buyer
//     verb — the live Prudence→Anne case).
//
// A PUBLIC quote (no TargetBuyer) is deliberately NOT evidence: it is visible
// to everyone and ties no specific counterparty to the sale, so it must not
// block a legitimate restock — a reseller advertising `kind` to the room while
// buying `kind` from a co-present supplier. Item-scoped for the same reason: a
// vendor buying OTHER goods is never gated. Huddle-scoping arm 2 bounds it to
// the active conversation without a recency constant.
func callerSellsItemTo(
	w *World,
	caller, counterparty ActorID,
	kind ItemKind,
	sceneID SceneID,
	huddleID HuddleID,
	at time.Time,
) bool {
	// Arm 1: an active sell quote from caller TARGETED at counterparty,
	// offering this item in this scene.
	if activeTargetedQuoteOffers(w, caller, counterparty, kind, sceneID, at) {
		return true
	}
	// Arm 2: an accepted sale of this item from caller to counterparty in the
	// current huddle.
	if huddleID != "" {
		for _, e := range w.PayLedger {
			if e == nil || e.State != PayLedgerStateAccepted {
				continue
			}
			if e.SellerID == caller && e.BuyerID == counterparty &&
				e.ItemKind == kind && e.HuddleID == huddleID {
				return true
			}
		}
	}
	return false
}

// activeTargetedQuoteOffers reports whether `seller` holds an ACTIVE, unexpired
// scene-quote in `sceneID`, addressed specifically to `buyer`, that offers
// `kind` on any of its bundle lines. Extracted from callerSellsItemTo's arm 1
// (LLM-551) so the reverse-pay gate and its counter-directional escape ask the
// same question of the same map rather than each rolling its own scan — the two
// readings must never disagree about whether a quote stands.
//
// A PUBLIC quote (empty TargetBuyer) never matches: `buyer` is always a
// resolved, non-empty actor, and a quote addressed to the room ties no specific
// counterparty to the sale. That asymmetry is load-bearing in BOTH callers —
// as gate evidence it must not block a reseller who advertises `kind` to the
// room while restocking it from a co-present supplier; as escape evidence it
// must not let a passer-by treat a public advertisement as direction settled
// between the two of them.
func activeTargetedQuoteOffers(
	w *World,
	seller, buyer ActorID,
	kind ItemKind,
	sceneID SceneID,
	at time.Time,
) bool {
	for _, q := range w.Quotes {
		if q == nil || q.State != SceneQuoteStateActive {
			continue
		}
		if q.SellerID != seller || q.SceneID != sceneID {
			continue
		}
		if !q.ExpiresAt.IsZero() && !at.Before(q.ExpiresAt) {
			continue
		}
		if q.TargetBuyer != buyer {
			continue
		}
		for _, line := range q.Lines {
			if line.ItemKind == kind {
				return true
			}
		}
	}
	return false
}

// reversePaySettleSteer builds the second sentence of the reverse-pay
// rejection: what the caller should do INSTEAD of the buyer verb. It names
// accept_pay only when there is genuinely an offer to accept — a pending
// pay-ledger entry from the counterparty for this good — and carries its
// ledger id, since accept_pay needs one.
//
// LLM-551: the old text named accept_pay unconditionally. That verb is
// seller-side (it answers a buyer's pay offer), so a caller who reached this
// gate while actually trying to BUY was handed a verb that could not apply to
// her: Patience Walker never once called it across 22 minutes of retries. With
// the counter-directional escape in place the common buy case no longer lands
// here at all, but the residual remains — a would-be buyer whose seller has not
// posted a quote yet — so the fallback states the situation and names the move
// that opens the buy path (get the offer posted) rather than misdirecting her
// to a tool she cannot use.
// The entry must be one accept_pay could actually settle: pending, not past its
// own ExpiresAt (the aging sweep flips a lapsed offer to expired, so between the
// deadline and the sweep the map still reads pending), and raised in THIS
// conversation — both the huddle and the scene it is anchored to. Huddle alone
// is not quite enough: a scene can conclude while its huddle lives on, so an
// entry from a prior scene could otherwise match on huddle id. Naming a stale or
// far-off offer id would hand the model an accept_pay that rejects — the same
// cue-versus-gate mismatch this gate's own steer exists to avoid.
func reversePaySettleSteer(
	w *World,
	caller, counterparty ActorID,
	kind ItemKind,
	huddleID HuddleID,
	sceneID SceneID,
	at time.Time,
) string {
	for _, e := range w.PayLedger {
		if e == nil || e.State != PayLedgerStatePending {
			continue
		}
		if e.SellerID != caller || e.BuyerID != counterparty || e.ItemKind != kind {
			continue
		}
		if huddleID == "" || e.HuddleID != huddleID || e.SceneID != sceneID {
			continue
		}
		if !e.ExpiresAt.IsZero() && !at.Before(e.ExpiresAt) {
			continue
		}
		return fmt.Sprintf("Their offer is waiting on you — settle it with accept_pay, offer id %d.", e.ID)
	}
	return "Wait for them to pay you. If you mean to buy this from them instead, they must post the offer first — ask them for it, then take it with pay_with_item."
}

// counterpartyCanSupply reports whether `seller` (the actor a buyer named) could
// actually provide `kind` — the escape hatch that keeps the LLM-293 general
// reverse-pay guard from blocking a legitimate restock. A seller can supply the
// good if it makes it at first hand (ProducesOrForages), is the village
// distributor (the standing supplier of everything), or simply holds stock of it
// right now. Only when NONE of these hold is the named seller a non-supplier —
// the phantom-offer case the guard rejects. Framed as "can this offer be filled"
// rather than "is this a sanctioned supply source" on purpose: if the seller
// holds the good the trade is fillable (odd direction, but not a deadlock), so we
// don't hard-block it; we block only the genuinely unfillable reverse offer. Nil
// seller reads as unable to supply.
func counterpartyCanSupply(w *World, seller *Actor, kind ItemKind) bool {
	if seller == nil {
		return false
	}
	if seller.RestockPolicy.ProducesOrForages(kind) {
		return true
	}
	if ActorIsDistributor(w.VillageObjects, seller.WorkStructureID) {
		return true
	}
	return seller.Inventory[kind] > 0
}

// findAutoMatchQuote returns the id of an open quote a bare (quote_id-less)
// coin offer can take — same seller, same scene, buyer-eligible, exact term
// match, amount at or above the quote's floor — or 0 when none qualifies.
// The predicates mirror the fast path's 1–5; gate 6 (stock, coins, break)
// stays in runPayWithItemFastPath, which re-validates everything, so a miss
// there falls back to the slow path rather than erroring. A below-floor
// amount is a haggle, not a take — the slow path stakes it for the seller to
// counter. Cheapest floor wins, then lowest id, so a duplicate-quote field
// resolves deterministically. ZBBS-HOME-424.
func findAutoMatchQuote(
	w *World,
	buyer *Actor,
	sellerID ActorID,
	sceneID SceneID,
	kind ItemKind,
	qty int,
	amount int,
	consumeNow bool,
	consumerIDs []ActorID,
	at time.Time,
) QuoteID {
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
		// Auto-match only single-line quotes — a bare single-item offer can't
		// expand into a multi-line bundle (LLM-101). Bundle takes always go
		// through an explicit quote_id.
		if len(q.Lines) != 1 {
			continue
		}
		if q.Lines[0].ItemKind != kind || q.Lines[0].Qty != qty || q.ConsumeNow != consumeNow {
			continue
		}
		if !actorIDSetsEqual(q.ConsumerIDs, consumerIDs) {
			continue
		}
		if amount < q.Amount {
			continue
		}
		if best == nil || q.Amount < best.Amount || (q.Amount == best.Amount && q.ID < best.ID) {
			best = q
		}
	}
	if best == nil {
		return 0
	}
	return best.ID
}

// activeBundleQuoteFrom returns the seller's live MULTI-LINE quote visible to
// this buyer in this scene, if any — the context that turns the unknown-item
// reject into a bundle-teaching one (LLM-561: a buyer answering a bundle put
// the whole purchase into the one-kind item slot as item "bundle"; the bare
// reject left it to improvise a 35-coin payment against a 3-coin salt).
// Liveness/visibility predicates mirror findAutoMatchQuote; among several
// standing bundles the newest (highest ID) wins, deterministically.
func activeBundleQuoteFrom(w *World, buyer *Actor, sellerID ActorID, sceneID SceneID, at time.Time) *SceneQuote {
	// Defensive (code_review): the only call site resolves sellerID against
	// the buyer's huddle peers BEFORE item resolution, so seller co-presence
	// is already guaranteed when this runs — an absent seller rejects
	// upstream as "no one named … in this conversation" and never reaches
	// the unknown-item error. Checked here anyway, mirroring the fast path's
	// shared-huddle predicate, so the helper stays honest for any future
	// caller: a quote whose seller has left the buyer's huddle must not be
	// taught as takeable.
	seller, ok := w.Actors[sellerID]
	if !ok || buyer.CurrentHuddleID == "" || seller.CurrentHuddleID != buyer.CurrentHuddleID {
		return nil
	}
	var best *SceneQuote
	for _, q := range w.Quotes {
		if q == nil || q.State != SceneQuoteStateActive || len(q.Lines) < 2 {
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
		if best == nil || q.ID > best.ID {
			best = q
		}
	}
	return best
}

// withdrawCrossingOffers resolves the buyer's OWN still-pending offers that
// mirror the transaction a quote auto-match just settled: left pending they
// invite a later double-settle (the seller accepting a stale mirror of a
// sale already made). "Mirror" is the settled take's term identity — same
// scene, kind, qty, disposition, and consumer set — NOT every same-goods
// offer: a distinct live order (different qty/disposition, e.g. a 10-water
// take-home staked before a 1-water consume-now quote take) must survive
// (code_review). Amount is excluded because above-floor overpayment is
// allowed on the take, and the duplicate gate's own key excludes price.
// Counter-chain entries (ParentID != 0) are skipped — a distinct lifecycle,
// same exemption the duplicate gate applies. WithdrawnByBuyer is the
// buyer-drove-this terminal — the reactor skips notifying the seller of it,
// and the seller's stale offer warrant drops via
// filterStalePayOfferWarrants. ZBBS-HOME-424.
func withdrawCrossingOffers(
	w *World,
	buyerID, sellerID ActorID,
	sceneID SceneID,
	kind ItemKind,
	qty int,
	consumeNow bool,
	consumerIDs []ActorID,
	at time.Time,
) {
	for _, e := range w.PayLedger {
		if e == nil || e.State != PayLedgerStatePending || e.ParentID != 0 {
			continue
		}
		if e.BuyerID != buyerID || e.SellerID != sellerID || e.ItemKind != kind {
			continue
		}
		if e.SceneID != sceneID || e.Qty != qty || e.ConsumeNow != consumeNow {
			continue
		}
		if !actorIDSetsEqual(e.ConsumerIDs, consumerIDs) {
			continue
		}
		finalizePayLedgerTerminal(w, e, PayTerminalStateWithdrawnByBuyer, "superseded — settled against the seller's open offer", at)
	}
}

// buyerCanAfford reports whether buyer can put at least amount coins into
// a purchase. It is the single definition of the funds comparison, shared
// by all the sites that ask it: the offer-time fast-fail in PayWithItem's
// slow path, the fast-path predicate 6, AcceptPay's gate 11, and the labor
// reward checks (employerCanCoverLaborReward). Centralizing the predicate
// keeps those from drifting on what "can afford" means — and LLM-644 is
// exactly the reserved-coins concept the original comment anticipated: for
// a visitor the answer is SpendableCoins (wallet capped by the remaining
// trip budget), so a factor flush with bale proceeds still cannot afford
// past the purse he arrived with. Residents are unchanged (spendable ==
// wallet).
//
// The ACTION taken when this returns false is intentionally NOT shared,
// because it differs by lifecycle stage: the two offer-time sites reject
// with a tool error (no ledger entry is minted, or a would-be fast-path
// accept is aborted), while AcceptPay flips an already-pending entry to
// the failed_insufficient_funds terminal. Sharing the action would force
// one of those behaviors onto the other.
func buyerCanAfford(buyer *Actor, amount int) bool {
	return buyer.SpendableCoins() >= amount
}

// buyerHoldsPayItems reports whether the buyer currently holds every goods
// line they offered to pay WITH (the barter leg, ZBBS-HOME-393). The
// goods counterpart to buyerCanAfford. Like the coin check, the ACTION on
// false differs by lifecycle stage (mint → tool-error reject; accept →
// failed_insufficient_goods terminal), so only the predicate is shared.
// An empty list (pure-coin offer) trivially holds.
func buyerHoldsPayItems(buyer *Actor, payItems []ItemKindQty) bool {
	for _, pi := range payItems {
		if buyer.Inventory[pi.Kind] < pi.Qty {
			return false
		}
	}
	return true
}

// buyerCanSparePayItems is buyerHoldsPayItems with the spoken-for reservation
// (LLM-636): every goods line must be covered by the buyer's SPARE units, not
// merely held. This is the one goods-coverage question every trade path asks —
// pay_with_item mint and accept (gate 12), counter_pay naming the buyer's
// goods, and the labor wage in kind (employerCanCoverLaborReward) — so a
// making or the garment on someone's back can never enter a trade by any
// door once the carry line has called it "not for trade". A gift (give) is
// deliberately not a trade and stays on the raw holdings check (LLM-544).
func buyerCanSparePayItems(w *World, buyer *Actor, payItems []ItemKindQty) bool {
	return len(spokenForShortfall(w, buyer, payItems)) == 0
}

// spokenForShortfall returns the goods lines the holder cannot cover from
// SPARE units — held short OR held but spoken for — for the caller to name in
// its steer. nil when every line is coverable.
func spokenForShortfall(w *World, holder *Actor, payItems []ItemKindQty) []ItemKindQty {
	if len(payItems) == 0 {
		return nil
	}
	spokenFor := SpokenFor(w.ItemKinds, w.Recipes, LiveBarterHolder(w, holder))
	var short []ItemKindQty
	for _, pi := range payItems {
		if SpareQty(holder.Inventory, spokenFor, pi.Kind) < pi.Qty {
			short = append(short, pi)
		}
	}
	return short
}

// payOfferShortfall returns a descriptive tool error if the buyer can't
// cover the offer's coins + goods at this instant, or nil if they can.
// The offer-time fast-fail (mint + fast-path) — an OPTIMIZATION that
// spares a wasted seller deliberation tick, not a reservation. Coins are
// reported first, then the first goods line short. ZBBS-HOME-393.
//
// The goods leg is checked against the buyer's SPARE units, not its raw
// holdings (SpokenFor, LLM-636): a maker naming the thread it keeps for
// mending, or the one suit on its back, is steered to goods it can spare —
// the same reservation the "## Restocking" barter steer and the carry
// line already read, so the tool refuses only what the prompt said was
// not for trade. Gate 12 at accept asks the same question
// (buyerCanSparePayItems), and counter_pay asks it of the buyer up front.
func payOfferShortfall(w *World, buyer *Actor, amount, qty int, payItems []ItemKindQty) error {
	if !buyerCanAfford(buyer, amount) {
		// Name the quantity the purse actually covers at the offered unit price,
		// so the model lowers the QUANTITY rather than just the coins
		// (ZBBS-HOME-459). The old "offer fewer coins" steer pointed at the wrong
		// lever: the buyer dropped coins, kept the quantity, and re-offered
		// underpriced (the John Ellis 25-meat-on-248-coins case). amount>=1 here
		// (amount==0 can't be unaffordable) and qty>=1 (validated upstream).
		// Multiply before dividing to keep the floor honest; int64 guards the
		// product against overflow on a 32-bit int build, clamped back into int
		// range (code_review). SpendableCoins, not the raw wallet (LLM-644): the
		// figure named must be the one the gate compared, or a proceeds-flush
		// visitor is told he "has" coin the trade will still refuse.
		spendable := buyer.SpendableCoins()
		affordable64 := int64(spendable) * int64(qty) / int64(amount)
		if affordable64 > int64(math.MaxInt32) {
			affordable64 = int64(math.MaxInt32)
		}
		affordable := int(affordable64)
		if affordable < 1 {
			return fmt.Errorf(
				"insufficient coins (have %d to spend, need %d) — you can't afford even one at this price; lower the quantity or pay with goods you carry.",
				spendable, amount,
			)
		}
		return fmt.Errorf(
			"insufficient coins (have %d to spend, need %d) — at this price you can afford %d; lower the quantity or pay with goods you carry.",
			spendable, amount, affordable,
		)
	}
	spokenFor := SpokenFor(w.ItemKinds, w.Recipes, LiveBarterHolder(w, buyer))
	for _, pi := range payItems {
		held := buyer.Inventory[pi.Kind]
		if held < pi.Qty {
			return fmt.Errorf(
				"you don't have %d %s to offer (you carry %d) — offer goods you actually hold.",
				pi.Qty, pi.Kind, held,
			)
		}
		if spare := SpareQty(buyer.Inventory, spokenFor, pi.Kind); spare < pi.Qty {
			return fmt.Errorf(
				"you can spare only %d of your %d %s — %s. Offer goods you can spare.",
				spare, held, pi.Kind, spokenForClause(spokenFor[pi.Kind].Reason),
			)
		}
	}
	return nil
}

// spokenForClause is the terse reason a spoken-for good is refused as
// payment, keyed on the SpokenFor claim (LLM-636) — second person, for the
// holder's own offer.
func spokenForClause(reason SpokenForReason) string {
	if reason == SpokenForGarment {
		return "the rest is your own clothes"
	}
	return "the rest you keep to work with"
}

// theirSpokenForClause is spokenForClause in the third person, for a seller
// whose counter named the buyer's spoken-for goods.
func theirSpokenForClause(reason SpokenForReason) string {
	if reason == SpokenForGarment {
		return "the rest is their own clothes"
	}
	return "the rest they keep to work with"
}

// resolvePayItems resolves a barter offer's free-text goods lines to
// canonical ItemKindQty. Each name → resolveItemKind (case-insensitive +
// trim); a kind may appear at most once (callers should net duplicates);
// qty must be positive and within MaxPayWithItemQty. Empty input returns
// nil (a pure-coin offer). ZBBS-HOME-393.
//
// Shared by PayWithItem (the buyer's pay_items) and CounterPay (the
// seller's counter goods) so the two resolve goods identically.
func resolvePayItems(w *World, payItems []PayItemInput) ([]ItemKindQty, error) {
	if len(payItems) == 0 {
		return nil, nil
	}
	resolved := make([]ItemKindQty, 0, len(payItems))
	seen := make(map[ItemKind]struct{}, len(payItems))
	for _, pi := range payItems {
		if pi.Qty < 1 {
			return nil, fmt.Errorf("pay_items: quantity must be at least 1 (got %d)", pi.Qty)
		}
		if pi.Qty > MaxPayWithItemQty {
			return nil, fmt.Errorf("pay_items: quantity exceeds maximum (got %d, max %d)", pi.Qty, MaxPayWithItemQty)
		}
		// LLM-167: an NPC paying with "work"/"labor" is reaching for the labor
		// market through the barter tool. Steer to the labor verbs BEFORE the
		// discovery mint below — otherwise the token mints a phantom inert kind
		// into the catalog and dead-ends on the holdings shortfall, with no hint
		// the labor flow exists.
		if isLaborToken(pi.Item) {
			return nil, errors.New(laborTradeSteerMsg)
		}
		// LLM-290: coins in a goods list are currency, not a good. The
		// pay_with_item handler folds coin rows into `amount` before building
		// its command, so reaching here means another entrance (counter_pay,
		// a future caller) carried one through — steer to the amount field
		// BEFORE the mint, same posture as the labor steer above.
		if IsCoinToken(pi.Item) {
			return nil, errors.New(
				"coins aren't a pay_items good — put the coin count in 'amount' instead; pay_items is for physical goods you carry.",
			)
		}
		// ZBBS-WORK-412: mint an unknown pay-with good at qty 0 (an NPC offering
		// to pay with a good it names is a discovery signal). The offerer holds 0
		// of a freshly-minted kind, so the holdings shortfall check rejects the
		// offer with a "you have no X to give" steer.
		kind, ok := resolveOrMintItemKind(w, pi.Item)
		if !ok {
			return nil, fmt.Errorf(
				"unknown item kind %q in pay_items — check the items you carry before offering.",
				pi.Item,
			)
		}
		if _, dup := seen[kind]; dup {
			return nil, fmt.Errorf(
				"%q appears more than once in pay_items — combine it into a single line.",
				pi.Item,
			)
		}
		// A "service" item is not a transferable good — its delivery is an
		// effect bound to the SELLER's establishment (lodging grants a room
		// there), so handing one over as payment is meaningless. Without this
		// gate an innkeeper holding a vestigial nights_stay token could barter
		// a room for a drink (observed live: "1 nights_stay for 1 water",
		// conversation hud-6c849d…). Applies to counter goods too — both
		// directions resolve through here. ZBBS-HOME-424.
		if itemHasCapability(w, kind, "service") {
			return nil, fmt.Errorf(
				"%q is a service, not a carryable good — it can't be offered as payment. Pay with coins or physical goods.",
				pi.Item,
			)
		}
		// A non-portable consumable (porridge, stew, a poured drink — any
		// EatHereOnly kind) is eaten where it's served and can't leave the
		// premises, so it can't be handed over and carried off as payment. The buy
		// leg is already clamped at PayWithItem (the EatHereOnly consume-clamp);
		// this applies the SAME rule to every TENDERED-goods leg — all of which
		// resolve through here: the barter pay_items, a counter offer, a gift
		// (GiveItems), and a labor in-kind reward. Without it an eat-here food
		// leaks into the recipient's pack and circulates as de-facto currency
		// (observed live: porridge). EatHereOnly is nil-safe, and a freshly-minted
		// unknown kind is not consumable, so it passes here and rejects instead on
		// the holdings shortfall — same posture as the service gate above. LLM-445.
		if w.ItemKinds[kind].EatHereOnly() {
			return nil, fmt.Errorf(
				"%q is eaten where it's served and can't be carried off as payment — offer coins or a portable good in trade instead.",
				pi.Item,
			)
		}
		seen[kind] = struct{}{}
		resolved = append(resolved, ItemKindQty{Kind: kind, Qty: pi.Qty})
	}
	return resolved, nil
}

// formatPayment renders an offer's payment terms as prose for SalientFact
// memory and perception lines: coins, goods, or both. ZBBS-HOME-393.
//
//	formatPayment(5, nil)                       → "5 coins"
//	formatPayment(0, [{nails,5}])               → "5 nails"
//	formatPayment(3, [{nails,5}])               → "5 nails and 3 coins"
//	formatPayment(3, [{nails,5},{hammer,2}])    → "5 nails, 2 hammers and 3 coins"
//
// Returns "nothing" only for an all-empty payment, a state the intake
// gates reject — so callers always get a non-empty phrase in practice.
func formatPayment(amount int, payItems []ItemKindQty) string {
	parts := make([]string, 0, len(payItems)+1)
	for _, pi := range payItems {
		parts = append(parts, fmt.Sprintf("%d %s", pi.Qty, pi.Kind))
	}
	if amount > 0 {
		coins := "coins"
		if amount == 1 {
			coins = "coin"
		}
		parts = append(parts, fmt.Sprintf("%d %s", amount, coins))
	}
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// FormatPayment is the exported wrapper over formatPayment (LLM-374). The
// settlement renderers live in other packages — the third-person Village feed
// in httpapi (renderActionLogEntry) and the first-person self-trail in
// perception (selfActionLine) — and both need to phrase a Paid action's full
// coins-and/or-goods tender identically to the in-package offer/counter lines.
// A thin wrapper keeps those cross-package callers on the one canonical
// formatter without exporting the internal name or duplicating the logic.
func FormatPayment(amount int, payItems []ItemKindQty) string {
	return formatPayment(amount, payItems)
}

// effectivePayConsumerCount returns max(1, len(consumerIDs)). Empty
// consumer set = buyer is implicit single consumer.
func effectivePayConsumerCount(consumerIDs []ActorID) int {
	if len(consumerIDs) == 0 {
		return 1
	}
	return len(consumerIDs)
}

// truncatePayMessage trims surrounding whitespace and caps to
// MaxPayMessageRunes, MARKED when it has to cut (LLM-405). The handler intake
// also trims + validates control characters, and REJECTS an over-cap message
// outright rather than shortening it, so on the tool path this never fires —
// it is defense-in-depth for non-handler callers (seeded entries, PC routes,
// future writers).
//
// It marks anyway. The message reaches a human through the operator console,
// and — for a gift's "for" note, the fourth caller — it reaches an NPC through
// the gave/received_gift relationship facts. Neither reader can tell a clipped
// note from a whole one without the marker.
func truncatePayMessage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return capRunesMarked(s, MaxPayMessageRunes)
}

// resolvePayConsumers resolves each consumer name to a huddle-peer
// ActorID. Same rules as resolveQuoteConsumers (case-insensitive trim,
// ambiguity reject, missing reject, duplicate reject, seller-as-consumer
// reject). Buyer-as-consumer IS allowed (the buyer often is one of the
// consumers in "round at the table" semantics).
//
// Returns the resolved IDs in input order; empty input returns nil.
func resolvePayConsumers(w *World, buyer *Actor, sellerID ActorID, names []string) ([]ActorID, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if buyer.CurrentHuddleID == "" {
		return nil, errors.New(
			"consumers can only be specified within an active huddle.",
		)
	}
	members, ok := w.actorsByHuddle[buyer.CurrentHuddleID]
	if !ok {
		return nil, errors.New(
			"consumers can only be specified within an active huddle.",
		)
	}
	resolved := make([]ActorID, 0, len(names))
	seen := make(map[ActorID]struct{}, len(names))
	for _, raw := range names {
		target := strings.TrimSpace(raw)
		if target == "" {
			return nil, errors.New("consumer name is empty after trim — every consumer must have a name.")
		}
		var found ActorID
		for peerID := range members {
			peer, ok := w.Actors[peerID]
			if !ok {
				continue
			}
			if strings.EqualFold(peer.DisplayName, target) {
				if found != "" {
					return nil, fmt.Errorf(
						"more than one person named %q is in this conversation — use a unique full name.",
						target,
					)
				}
				found = peerID
			}
		}
		if found == "" {
			return nil, fmt.Errorf(
				"no one named %q in this conversation — re-check who is here before offering.",
				target,
			)
		}
		if found == sellerID {
			return nil, errors.New(
				"the seller can't be a consumer of their own sale — drop their name from the consumer list.",
			)
		}
		if _, dup := seen[found]; dup {
			return nil, fmt.Errorf(
				"%q appears more than once in the consumer list — list each person only once.",
				target,
			)
		}
		seen[found] = struct{}{}
		resolved = append(resolved, found)
	}
	return resolved, nil
}

// validateInResponseTo runs the architecture § 7 in_response_to chain
// rules. The buyer is NOT required to keep matching item terms across
// the counter chain — they can shift qty/item/consume_now between
// rounds. Validation pins identity (same buyer, same seller),
// freshness (parent.ResolvedAt within
// PayLedgerInResponseToWindow), and chain shape (parent has been
// countered, hasn't already been answered).
func validateInResponseTo(w *World, parentID LedgerID, buyerID, sellerID ActorID, at time.Time) error {
	parent, ok := w.PayLedger[parentID]
	if !ok || parent == nil {
		return fmt.Errorf(
			"in_response_to: parent ledger %d not found",
			parentID,
		)
	}
	if parent.State != PayLedgerStateCountered {
		return fmt.Errorf(
			"in_response_to: parent ledger %d is not countered (currently %s) — you can only respond to a counter.",
			parentID, parent.State,
		)
	}
	if parent.BuyerID != buyerID {
		return fmt.Errorf(
			"in_response_to: ledger %d isn't your offer — you can't respond to someone else's counter.",
			parentID,
		)
	}
	if parent.SellerID != sellerID {
		return fmt.Errorf(
			"in_response_to: ledger %d was countered by a different seller — respond to the original seller.",
			parentID,
		)
	}
	if parent.ResolvedAt.IsZero() || at.Sub(parent.ResolvedAt) > PayLedgerInResponseToWindow {
		return fmt.Errorf(
			"in_response_to: ledger %d's counter is too old (older than %s) — make a fresh offer instead.",
			parentID, PayLedgerInResponseToWindow,
		)
	}
	// Depth cap. The response this validates would be at parent.Depth+1;
	// a parent already at MaxPayCounterChainDepth can't be answered, which
	// bounds the haggle chain. See MaxPayCounterChainDepth.
	if parent.Depth >= MaxPayCounterChainDepth {
		return fmt.Errorf(
			"in_response_to: ledger %d is at the counter-chain depth limit (%d rounds) — make a fresh offer instead of haggling further.",
			parentID, MaxPayCounterChainDepth,
		)
	}
	// Parent-uniqueness scan (ledger-substrate § 6 — O(N) over
	// World.PayLedger). Cheap at expected sizes; reverse index
	// deferred.
	for _, e := range w.PayLedger {
		if e == nil || e.ID == parentID {
			continue
		}
		if e.ParentID == parentID {
			return fmt.Errorf(
				"in_response_to: ledger %d has already been answered by ledger %d — make a fresh offer instead.",
				parentID, e.ID,
			)
		}
	}
	return nil
}

// allConsumersInHuddle reports whether every consumerID is currently in
// huddleID. Used by AcceptPay's gate 9 (consumer departure between
// offer creation and accept).
func allConsumersInHuddle(w *World, huddleID HuddleID, consumerIDs []ActorID) bool {
	if huddleID == "" {
		return false
	}
	members, ok := w.actorsByHuddle[huddleID]
	if !ok {
		return false
	}
	for _, cid := range consumerIDs {
		if _, in := members[cid]; !in {
			return false
		}
	}
	return true
}

// finalizePayLedgerTerminal flips entry to the given terminal state,
// stamps ResolvedAt + Message, emits PayWithItemResolved, and returns
// the new state. Used by every terminal flip path EXCEPT counter
// (which has its own event family and lives inline in CounterPay).
//
// Caller guarantees entry.State is currently Pending — this helper
// performs no state-check itself.
func finalizePayLedgerTerminal(
	w *World,
	entry *PayLedgerEntry,
	terminal PayTerminalState,
	message string,
	at time.Time,
) PayLedgerState {
	entry.State = PayLedgerState(terminal)
	entry.ResolvedAt = at
	entry.Message = message

	evt := &PayWithItemResolved{
		LedgerID:       entry.ID,
		BuyerID:        entry.BuyerID,
		SellerID:       entry.SellerID,
		ItemKind:       entry.ItemKind,
		QtyPerConsumer: entry.Qty,
		ConsumeNow:     entry.ConsumeNow,
		ConsumerIDs:    cloneActorIDs(entry.ConsumerIDs),
		Amount:         entry.Amount,
		PayItems:       cloneItemKindQtys(entry.PayItems), // LLM-105: barter goods snapshot (mirrors entry; only Accepted drives the audit row)
		TerminalState:  terminal,
		IsGift:         entry.IsGift,
		Message:        message,
		SceneID:        entry.SceneID,
		HuddleID:       entry.HuddleID,
		At:             at,
	}
	w.emit(evt)

	// Relationship writes for the decline path — both directions, so a
	// shared-VA NPC's perception can later render "Bob declined my offer
	// for ale" + "I declined Alice's offer for ale" appropriately.
	// Other terminal kinds (expired / withdrawn / failed_*) don't get
	// relationship writes — they're low-signal lifecycle events rather
	// than social moves.
	if terminal == PayTerminalStateDeclined {
		buyerName := actorDisplayName(w, entry.BuyerID)
		sellerName := actorDisplayName(w, entry.SellerID)
		buyerFact := payDeclinedFactText(buyerName, sellerName, entry.Amount, entry.PayItems, entry.ItemKind, entry.Qty, message, true)
		sellerFact := payDeclinedFactText(buyerName, sellerName, entry.Amount, entry.PayItems, entry.ItemKind, entry.Qty, message, false)
		if _, err := RecordInteraction(entry.BuyerID, entry.SellerID, InteractionPayDeclinedBy, buyerFact, at).Fn(w); err != nil {
			log.Printf("sim.finalizePayLedgerTerminal: RecordInteraction buyer→seller %q→%q: %v", entry.BuyerID, entry.SellerID, err)
		}
		if _, err := RecordInteraction(entry.SellerID, entry.BuyerID, InteractionDeclinedPay, sellerFact, at).Fn(w); err != nil {
			log.Printf("sim.finalizePayLedgerTerminal: RecordInteraction seller→buyer %q→%q: %v", entry.SellerID, entry.BuyerID, err)
		}
	}

	// Rumor seed (LLM-387): a settlement that fell through because the buyer
	// couldn't cover the price is a witnessed coin-short moment — the seller was
	// just told "the buyer couldn't cover the price". Seed a rung-0 short_on_coin
	// rumor about the buyer into the seller + any co-present huddle witnesses.
	// Social layer only; no coin or state moves.
	if terminal == PayTerminalStateFailedInsufficientFunds {
		seedShortOnCoinRumor(w, entry, at)
	}
	return entry.State
}

// payTransferOutcome is commitPayTransfer's buyer-visible summary of the
// atomic commit, carried onto PayWithItemResult for fast-path settles so the
// buyer's tool feedback can voice what actually happened (ZBBS-HOME-436).
type payTransferOutcome struct {
	buyerAte        int     // units the buyer themself consumed now
	keptToInventory int     // surplus units pocketed into the buyer's pack
	tookHome        bool    // physical take-home handed over at accept
	booked          bool    // future-night lodging Order minted for keeper check-in
	lodgedNow       bool    // same-day walk-in room granted on the spot (LLM-84)
	mendedNow       bool    // worn garments mended on the spot (LLM-625)
	servicedNow     bool    // business equipment serviced on the spot (LLM-648)
	satisfiesNeed   NeedKey // primary need the consumed item satisfies
	feltAfter       string  // buyer's post-consume felt label(s); "" = sated
	mealMinutes     int     // buyer's eat-here dwell duration in minutes; 0 = no ongoing meal/drink (ZBBS-WORK-409)
}

// preflightMendingEntry rejects, before commitPayTransfer mutates anything, a
// ledger entry whose mend could not be delivered (LLM-625, code_review): a
// mending kind riding a bundle line (the quote path forbids service kinds in
// bundles, so only a legacy/direct entry can carry one — and deliverBundleLines
// has no mending arm), a gifted mend (the gift path settles before the delivery
// branches and would quietly skip the mend), a consumer set that is not the
// buyer alone, or a mend failing ValidateMendingDelivery. nil for every entry
// with no mending involvement — the common case, one capability probe.
func preflightMendingEntry(w *World, buyer, seller *Actor, entry *PayLedgerEntry) error {
	for _, ln := range entry.Lines {
		if itemHasCapability(w, ln.ItemKind, CapabilityMending) {
			return fmt.Errorf("commitPayTransfer: ledger %d carries mending inside a bundle — mending is single-item only", entry.ID)
		}
	}
	if !itemHasCapability(w, entry.ItemKind, CapabilityMending) {
		return nil
	}
	if entry.IsGift {
		return fmt.Errorf("commitPayTransfer: ledger %d is a gift of mending — a mend is bought, not given", entry.ID)
	}
	for _, cid := range entry.ConsumerIDs {
		if cid != entry.BuyerID {
			return fmt.Errorf("commitPayTransfer: ledger %d mends a non-buyer consumer %q — the buyer brings their own clothes", entry.ID, cid)
		}
	}
	if err := ValidateMendingDelivery(w, seller, buyer, entry.ItemKind); err != nil {
		return fmt.Errorf("commitPayTransfer: ledger %d: %w", entry.ID, err)
	}
	return nil
}

// preflightEquipmentServiceEntry is preflightMendingEntry's mirror for the
// wright's trade (LLM-648): rejects, before commitPayTransfer mutates
// anything, an equipment-service entry riding a bundle line, a gifted service,
// a consumer set that is not the buyer alone, or a service failing
// ValidateEquipmentServiceDelivery. nil for every entry with no
// equipment-service involvement — the common case, one capability probe.
func preflightEquipmentServiceEntry(w *World, buyer, seller *Actor, entry *PayLedgerEntry) error {
	for _, ln := range entry.Lines {
		if itemHasCapability(w, ln.ItemKind, CapabilityEquipmentService) {
			return fmt.Errorf("commitPayTransfer: ledger %d carries equipment service inside a bundle — the service is single-item only", entry.ID)
		}
	}
	if !itemHasCapability(w, entry.ItemKind, CapabilityEquipmentService) {
		return nil
	}
	if entry.IsGift {
		return fmt.Errorf("commitPayTransfer: ledger %d is a gift of equipment service — the service is bought, not given", entry.ID)
	}
	if entry.Qty != 1 {
		return fmt.Errorf("commitPayTransfer: ledger %d buys %d equipment services — delivery is one visit, one reset, one stone (qty must be 1)", entry.ID, entry.Qty)
	}
	for _, cid := range entry.ConsumerIDs {
		if cid != entry.BuyerID {
			return fmt.Errorf("commitPayTransfer: ledger %d services a non-buyer consumer %q — the wright services the buyer's own business", entry.ID, cid)
		}
	}
	if err := ValidateEquipmentServiceDelivery(w, seller, buyer, entry.ItemKind); err != nil {
		return fmt.Errorf("commitPayTransfer: ledger %d: %w", entry.ID, err)
	}
	return nil
}

// isAdvanceLodgingBooking reports whether a lodging entry is booked for a FUTURE
// night (ready_by past today) rather than a same-day walk-in. A same-day room is
// granted at the pay accept (LLM-84); a future reservation stays a deferred Order
// that the keeper fulfills via deliver_order on the booked day. Non-lodging
// entries are never advance bookings. Uses the world timezone for the day
// boundary, matching createOrderForPayWithItem / orderDateUTC.
func isAdvanceLodgingBooking(w *World, entry *PayLedgerEntry, at time.Time) bool {
	if entry == nil || !itemHasCapability(w, entry.ItemKind, "lodging") {
		return false
	}
	return entry.ReadyBy.After(orderDateUTC(at, w.Settings.Location))
}

// isCommissionOrder reports whether an accepted take-home offer should mint a
// DEFERRED "commission" Order rather than reject for lack of stock or deliver at
// accept: the seller MAKES the good (a produce entry + a makeable recipe) but
// doesn't currently hold enough to hand over, so it must still be forged
// (LLM-338). When true, the accept skips the stock reject (acceptPendingOffer
// gate 10) and commitPayTransfer mints a Ready Order the keeper fulfils via
// deliver_order once produced (gate 5 stock is the readiness gate);
// refund-on-expiry (ZBBS-HOME-403) returns the buyer's coins if it's never made.
//
// Constraints, each load-bearing:
//   - Non-gift, non-consume-now: a commission is a take-home purchase.
//   - Coin-only (no pay_items): a commission that expires refunds COINS; a
//     barter leg couldn't be reversed, so a goods-paid stockless offer is NOT a
//     commission and falls through to the normal stock reject (mirrors the
//     advance-lodging coin-only rule).
//   - Not service / lodging: those carry no inventory and own their fulfilment
//     paths; the stock gate already skips them.
//   - Seller Produces(kind) AND makeableRecipe: only a good the seller can
//     actually forge — otherwise the order could never be fulfilled and would
//     only ever expire-and-refund.
//   - Stock short (have − reserved < needed): with stock on hand it's a normal
//     deliver-at-accept take-home sale, not a commission.
//
// CommissionableKind reports whether `kind` is a good the holder of `policy` can
// take a FORWARD order for — sell it now and forge it after — rather than one that
// must be handed over from stock at accept time. The KIND-level half of
// isCommissionOrder's test, split out because it is the half that is knowable
// before any offer exists.
//
// Exported so the perception cue and the accept path share one definition of
// "commissionable" (the LLM-617 lesson, applied deliberately). The seller-side
// sale cue advertises exactly these goods when stock is empty, and
// acceptPendingOffer waives its stock gate for exactly these goods; a cue that
// judged it independently would either dangle a sale accept rejects, or hide one
// accept would have honoured — which is LLM-619: Ezekiel's shovel was absent from
// "Your goods to sell" purely because he held none, so six struck bargains never
// became orders and the buyer walked the same loop for over an hour.
//
// The three gates, all pure over catalog + policy:
//   - not a service / lodging kind — those are rendered, not forged, and their
//     shortfall is a real rejection.
//   - a makeable recipe (positive rate) — otherwise the order could never be
//     fulfilled and would only ever expire-and-refund.
//   - the seller actually PRODUCES it — a reseller who merely stocks a good has
//     no forge to make good on the promise.
//
// It deliberately says nothing about whether the inputs are on hand right now.
// That axis belongs to the caller: accept treats a commission as a promise to
// forge later, while the cue additionally requires sim.HasProduceInputs so it
// never invites a seller to promise what they cannot start.
func CommissionableKind(recipes map[ItemKind]*ItemRecipe, kinds map[ItemKind]*ItemKindDef, policy *RestockPolicy, kind ItemKind) bool {
	if def := kinds[kind]; def != nil && (def.HasCapability("service") || def.HasCapability("lodging")) {
		return false
	}
	r := recipes[kind]
	if r == nil || r.RateQty <= 0 || r.RatePerHours <= 0 {
		return false
	}
	return policy.Produces(kind)
}

// MUST run inside a Command.Fn (reads w.Orders / w.Recipes).
func isCommissionOrder(w *World, seller *Actor, entry *PayLedgerEntry) bool {
	if entry == nil || seller == nil || entry.IsGift || entry.ConsumeNow {
		return false
	}
	if len(entry.PayItems) > 0 {
		return false
	}
	kind := entry.ItemKind
	if !CommissionableKind(w.Recipes, w.ItemKinds, seller.RestockPolicy, kind) {
		return false
	}
	effConsumers := effectivePayConsumerCount(entry.ConsumerIDs)
	if effConsumers <= 0 || entry.Qty > math.MaxInt/effConsumers {
		return false
	}
	needed := entry.Qty * effConsumers
	have := seller.Inventory[kind] - outstandingReadyOrderQty(w, seller.ID, kind)
	return have < needed
}

// depositChargeForEntry returns the coins to move buyer→seller at accept. For a
// partial-payment commission (LLM-357) with a valid deposit (0 < Deposit <
// Amount) it is the Deposit — the balance is collected later at deliver_order.
// Every other case (a full-prepay commission, a normal sale, a barter, a gift)
// charges the full Amount. The isCommissionOrder re-check is load-bearing: a
// stray Deposit on an offer that does NOT resolve to a commission at accept
// (e.g. the seller turned out to hold stock, so it delivers now) still charges
// the full price rather than under-charging for goods handed over immediately.
// MUST run inside a Command.Fn (isCommissionOrder reads w.Orders / w.Recipes).
func depositChargeForEntry(w *World, seller *Actor, entry *PayLedgerEntry) int {
	if entry == nil {
		return 0
	}
	if entry.Deposit > 0 && entry.Deposit < entry.Amount && isCommissionOrder(w, seller, entry) {
		return entry.Deposit
	}
	return entry.Amount
}

// entrySaleLines resolves the goods an accepted offer moves, as the {item, qty} lines
// the wear accrual prices its cost basis over (LLM-411). A bundle quote-take carries
// its goods in Lines and leaves the scalar ItemKind empty (LLM-101); every other offer
// — slow accept, fast take, counter, barter, lodging — carries the single scalar line.
// Nil when the offer moves no goods at all, which the accrual reads as "no cost basis"
// and wears on the full charge.
func entrySaleLines(entry *PayLedgerEntry) []QuoteLine {
	if entry == nil {
		return nil
	}
	if len(entry.Lines) > 0 {
		return entry.Lines
	}
	if entry.ItemKind == "" || entry.Qty < 1 {
		return nil
	}
	return []QuoteLine{{ItemKind: entry.ItemKind, Qty: entry.Qty}}
}

// maxDwellMinutes returns the longest remaining dwell duration in minutes across
// the stamped item-dwell snapshots (0 when none carry a countdown). An eat-here
// meal or drink keeps easing a need for this long after the first bite, but the
// buyer collects it only by staying put — walking off deletes the credit. The
// settle feedback uses this to tell the buyer to stay and finish rather than
// bolt and forfeit the food and the coins (ZBBS-WORK-409).
func maxDwellMinutes(stamped []DwellCreditSnapshot) int {
	best := 0
	for _, s := range stamped {
		if s.RemainingTicks == nil {
			continue
		}
		m := (*s.RemainingTicks) * s.PeriodMinutes
		if m > best {
			best = m
		}
	}
	return best
}

// buyerFeltAfterConsume reports the buyer's post-consume felt state for the
// need(s) the item satisfies: the primary need key (largest per-unit restore)
// and the joined felt labels for every satisfied need still at or above the
// awareness floor. An empty felt string means the meal left the buyer below
// the floor — sated, nothing to voice. Runs on the world goroutine against
// live post-commit needs, which the once-per-tick perception render cannot
// show the model mid-tick. ZBBS-HOME-436.
func buyerFeltAfterConsume(buyer *Actor, def *ItemKindDef, thresholds NeedThresholds) (NeedKey, string) {
	if buyer == nil || def == nil {
		return "", ""
	}
	// Strict > keeps the FIRST def.Satisfies entry on an Immediate tie —
	// item def order is the authoritative priority, matching how
	// consumableUnits and the dwell-credit upsert walk the same slice.
	primary, best := NeedKey(""), 0
	var felt []string
	seen := make(map[string]bool)
	for _, s := range def.Satisfies {
		if s.Immediate <= 0 {
			continue
		}
		if s.Immediate > best {
			best, primary = s.Immediate, s.Attribute
		}
		n, ok := FindNeed(s.Attribute)
		if !ok {
			continue
		}
		// Dedup so two Satisfies lines on the same attribute (or needs
		// sharing a vocabulary word) can't render "hungry, hungry".
		if label := n.Label(n.Tier(buyer.Needs[s.Attribute], thresholds.Get(s.Attribute))); label != "" && !seen[label] {
			seen[label] = true
			felt = append(felt, label)
		}
	}
	return primary, strings.Join(felt, ", ")
}

// commitPayTransfer performs the AcceptPay / fast-path-accept atomic
// commit: coin debit + item movement + ConsumeNow per-consumer apply +
// bidirectional buyer/seller relationship writes + per-consumer
// ItemConsumed emit when ConsumeNow=true.
//
// The world goroutine serializes everything, so by the time this
// function is called the gates have already pre-validated stock,
// funds, and overflow. A nonzero error return here is a bug, not a
// domain failure.
//
// Item flow:
//
//   - ConsumeNow=false, "lodging" item: a deferred booking. The Order is
//     minted at Ready and left for the keeper to check the guest in via
//     deliver_order (the room grant happens there; the eviction exemption is
//     gated on that check-in). This is the designed two-phase lodging flow —
//     see the salem-engine-v2/lodging codebase note — and is NOT flattened.
//   - ConsumeNow=false, everything else (physical takeaway): the Order is
//     minted AND immediately delivered in the same tick via
//     fulfillTakeHomeOrderAtAccept (ZBBS-HOME-398) — goods move to the buyer
//     right here at accept, and the Order is flipped to Delivered so its
//     durable pay_ledger row still persists (the price-book restart seed reads
//     accepted rows). At accept the buyer is co-present and the seller holds
//     the stock, so there is nothing to defer and no window for the HOME-396
//     takeaway-expiry robbery. Phase 3 PR S6 originally deferred even this to
//     a separate deliver_order beat for seller narrative agency, but the
//     buyer-seller rendezvous gap made deferral a routine robbery path.
//   - ConsumeNow=true: stock leaves seller's inventory, but does NOT
//     land on the consumer's. Instead, the consumer's needs decrement
//     per the ItemKind's Satisfies list (mirrors sim.Consume), dwell
//     credits are upserted at the consumer's nearest VillageObject, and
//     one ItemConsumed event per consumer is emitted with the applied
//     deltas. No Order minted — eat-on-the-spot has no post-accept
//     fulfillment to track.
//
// forText (slow-path: empty; fast-path: the buyer's flavor text on the
// pay_with_item call) is folded into the buyer/seller SalientFact via
// payAcceptedFactText.
//
// The returned payTransferOutcome summarizes the buyer-visible effects so
// the fast path can voice them in the buyer's tool feedback (ZBBS-HOME-436).
// AcceptPay discards it — the buyer isn't the actor reading that result.
// On a non-nil error the outcome is meaningless and MUST be discarded:
// error paths return a zero outcome, but some fire after partial mutation,
// so the outcome never doubles as a rollback record (code_review).
func commitPayTransfer(
	w *World,
	buyer, seller *Actor,
	entry *PayLedgerEntry,
	at time.Time,
	forText string,
) (payTransferOutcome, error) {
	var out payTransferOutcome
	// LLM-625 mending preflight (code_review). The mending delivery arm runs
	// AFTER the coin transfer below, and an error return from this function
	// does not roll that transfer back — so every way a mend can fail must
	// reject HERE, before any mutation. The intake/accept/fast-path gates
	// normally guarantee delivery, making this the invariant boundary for
	// entries that reached commit some other way (reloaded across a deploy,
	// seeded, or directly constructed).
	if err := preflightMendingEntry(w, buyer, seller, entry); err != nil {
		return payTransferOutcome{}, err
	}
	if err := preflightEquipmentServiceEntry(w, buyer, seller, entry); err != nil {
		return payTransferOutcome{}, err
	}
	// Two-way swap (ZBBS-HOME-393): the buyer pays with coins AND/OR goods.
	// Validate the goods leg in full BEFORE mutating anything so a single
	// bad line aborts the whole swap (validate-all-then-apply, mirroring
	// AdjustActorHoldings). The coin leg's seller-overflow was already
	// guarded by the caller's gates; buyer-holds was revalidated at gate 12,
	// but re-check here defensively — a subscriber firing mid-accept could
	// have moved the buyer's holdings.
	type payItemMove struct {
		kind          ItemKind
		buyerPostQty  int // >= 0, validated
		sellerPostQty int // overflow-checked
	}
	// Aggregate by kind FIRST. resolvePayItems rejects duplicate canonical
	// kinds at intake, but commitPayTransfer is the atomicity boundary and
	// takes ledger data directly (seeded entries, future persisted reloads),
	// so it must not assume uniqueness: without aggregation two lines of the
	// same kind would each compute their post-quantity from the ORIGINAL
	// inventory and the second apply would clobber the first (lost quantity).
	//
	// Item CLASS, by contrast, is deliberately NOT re-checked here (LLM-445):
	// the eat-here/service tendered-goods gate lives at intake
	// (resolvePayItems), and an already-minted entry settles as offered. Safe
	// because the ledger has no durable projection (a restart wipes pending
	// entries) and offers expire in minutes — so a pre-gate entry can't
	// outlive a deploy. Pinned by TestGift_PreGateEatHereEntry_StillSettles;
	// adding a settle-time class check should be a deliberate decision there.
	totals := make(map[ItemKind]int, len(entry.PayItems))
	for _, pi := range entry.PayItems {
		if pi.Qty < 1 {
			return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: invalid pay_item qty %d for %s", pi.Qty, pi.Kind)
		}
		next, err := addChecked(totals[pi.Kind], pi.Qty)
		if err != nil {
			return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: pay_item total for %s would overflow", pi.Kind)
		}
		totals[pi.Kind] = next
	}
	moves := make([]payItemMove, 0, len(totals))
	for kind, qty := range totals {
		have := buyer.Inventory[kind]
		if have < qty {
			return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: buyer %q lacks %d %s mid-commit (have %d)", buyer.ID, qty, kind, have)
		}
		sellerPost, err := addChecked(seller.Inventory[kind], qty)
		if err != nil {
			return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: seller %q %s balance would overflow", seller.ID, kind)
		}
		moves = append(moves, payItemMove{kind: kind, buyerPostQty: have - qty, sellerPostQty: sellerPost})
	}

	// All legs validated — apply coins + goods together. Coin overflow
	// guarded by caller. LLM-357: a partial-payment commission moves only the
	// deposit now; the balance is collected at deliver_order.
	// depositChargeForEntry is the full Amount for every non-partial offer.
	charge := depositChargeForEntry(w, seller, entry)
	buyer.Coins -= charge
	seller.Coins += charge
	// LLM-644: visitor spending draws down the trip budget — coin top-ups on
	// barter legs included, since charge is whatever coin actually moved.
	drawVisitorSpend(buyer, charge)
	// LLM-118: a market stall wears in proportion to the coin its owner takes
	// in here; crossing the repair threshold wakes them to mend it. LLM-411: the
	// goods ride along so the wear taxes the sale's MARGIN over what the seller
	// paid for them, not its gross — a reseller's leg wears only on what it earns.
	accrueStallWear(w, seller, saleWear{
		Lines:     entrySaleLines(entry),
		Consumers: len(entry.ConsumerIDs),
		Amount:    entry.Amount,
		Charge:    charge,
	}, at)
	for _, m := range moves {
		if m.buyerPostQty == 0 {
			delete(buyer.Inventory, m.kind) // delete-on-zero invariant
		} else {
			buyer.Inventory[m.kind] = m.buyerPostQty
		}
		if seller.Inventory == nil {
			seller.Inventory = make(map[ItemKind]int)
		}
		seller.Inventory[m.kind] = m.sellerPostQty
	}

	// LLM-138: a gift ends here. The PayItems swap above already moved the
	// gift goods giver→recipient, and a gift carries no coins (Amount 0, the
	// debit above was a no-op) and no bought-item leg to deliver. Record the
	// gift relationship facts — the gift counterpart to the Paid/PaidBy pair
	// below — and return before the bought-item delivery branches.
	if entry.IsGift {
		giverName := buyer.DisplayName
		recipientName := seller.DisplayName
		// The gift's optional "for" note rides entry.Message (the accept path
		// calls commitPayTransfer with forText="" — LLM-138 stores the note on the
		// entry at mint, since GiveItems is the only writer of the Message field
		// for a gift).
		giverFact := giftFactText(w, giverName, recipientName, entry.PayItems, entry.Message, true)
		recipientFact := giftFactText(w, giverName, recipientName, entry.PayItems, entry.Message, false)
		if _, err := RecordInteraction(entry.BuyerID, entry.SellerID, InteractionGave, giverFact, at).Fn(w); err != nil {
			log.Printf("sim.commitPayTransfer: gift RecordInteraction giver→recipient %q→%q: %v", entry.BuyerID, entry.SellerID, err)
		}
		if _, err := RecordInteraction(entry.SellerID, entry.BuyerID, InteractionReceivedGift, recipientFact, at).Fn(w); err != nil {
			log.Printf("sim.commitPayTransfer: gift RecordInteraction recipient→giver %q→%q: %v", entry.SellerID, entry.BuyerID, err)
		}
		return out, nil
	}

	consumers := entry.ConsumerIDs
	implicitBuyerConsumer := len(consumers) == 0
	if implicitBuyerConsumer {
		consumers = []ActorID{entry.BuyerID}
	}

	// eagerlyDelivered holds an Order that was minted + fulfilled THIS tick
	// (physical goods handed to the buyer, or a same-day walk-in room granted —
	// LLM-84) but NOT yet flipped to Delivered: the flip is deferred until after
	// the Paid/PaidBy facts below so OrderDelivered fires after the payment facts
	// exist (ZBBS-HOME-398; code_review).
	var eagerlyDelivered *Order
	// orderMinted tracks whether any branch below minted a durable Order —
	// set beside each mint call rather than inferred from entry shape, so
	// the LLM-246 order-less write-through at the bottom keys on what
	// actually happened. An entry-shape predicate (ConsumeNow || Lines)
	// would silently double-write a pay_ledger id if a future entry shape
	// with Lines ever started minting Orders (code_review, LLM-246).
	orderMinted := false

	def := w.ItemKinds[entry.ItemKind]
	if len(entry.Lines) > 0 {
		// LLM-101 bundle take: deliver every line (eat-here consume or
		// take-home hand-over), no durable Order. Coins + any barter already
		// moved above; leaves eagerlyDelivered nil so the Order-flip below is
		// skipped.
		bundleOut, bundleErr := deliverBundleLines(w, buyer, seller, entry, consumers, at)
		if bundleErr != nil {
			return payTransferOutcome{}, bundleErr
		}
		out = bundleOut
	} else if entry.ConsumeNow && !itemHasCapability(w, entry.ItemKind, "lodging") && !itemHasCapability(w, entry.ItemKind, CapabilityMending) {
		// Eat-on-the-spot: stock leaves seller, consumer needs
		// satisfied directly. Per-consumer apply + dwell stamp +
		// ItemConsumed emit. No Order minted.
		//
		// LLM-391 — lodging is explicitly excluded so a consume_now
		// nights_stay can NEVER land here (where no room is granted). PayWithItem
		// normalizes consume_now→false for lodging at intake, but an entry that
		// bypassed intake (a pre-fix pending offer reloaded across a deploy, or a
		// future direct construction) would otherwise be silently "consumed" for
		// nothing. Mending (LLM-625) is excluded for the identical reason: its
		// delivery is an effect on the buyer's wear counters, and this branch
		// would settle the coins without mending a stitch. Falling through to the lodging branch below routes it through
		// transferOrderGoods → AssignBedroomForLodger, whose advancePastHeldLodging
		// coalesces a same-night re-book instead of minting a duplicate
		// (buyer, seller, ready_by) that wedges the checkpoint.
		//
		// ZBBS-WORK-391: each consumer eats only what their needs can absorb
		// (consumableUnits); the surplus goes to the BUYER's inventory instead
		// of burning into an already-zeroed need (the Prudence case: a
		// seller-pitched 10-meat bundle eaten in one go against a hunger one
		// unit would have cleared). The purchase itself is untouched — the
		// buyer pays the full amount and the seller's stock drops by the full
		// qty; only the eat-vs-pocket split changes. Surplus is routed to the
		// buyer (not the consumer) because the buyer paid for it; for the
		// common implicit-self-consumer offer they are the same actor anyway.
		//
		// The split is pre-computed for ALL consumers before this branch
		// mutates anything, so the one new failure mode — buyer-inventory
		// overflow pocketing the surplus — rejects up front rather than
		// mid-loop after stock/needs moved (code_review). The splits are
		// order-independent: resolvePayConsumers rejects duplicate consumers,
		// so no consumer's eat affects another's needs.
		type consumeSplit struct {
			cid      ActorID
			consumer *Actor
			eat      int
			kept     int
		}
		splits := make([]consumeSplit, 0, len(consumers))
		totalKept := 0
		for _, cid := range consumers {
			consumer, ok := w.Actors[cid]
			if !ok {
				// Shouldn't happen — gate 9 verified consumer presence
				// in the huddle. Conservative skip.
				continue
			}
			eat := consumableUnits(consumer, def, entry.Qty)
			splits = append(splits, consumeSplit{cid: cid, consumer: consumer, eat: eat, kept: entry.Qty - eat})
			totalKept += entry.Qty - eat
		}
		if totalKept > 0 {
			if _, err := addChecked(buyer.Inventory[entry.ItemKind], totalKept); err != nil {
				return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: buyer %q %s balance would overflow pocketing surplus", buyer.ID, entry.ItemKind)
			}
		}
		out.keptToInventory = totalKept
		for _, sp := range splits {
			cid, consumer, eat, kept := sp.cid, sp.consumer, sp.eat, sp.kept
			// A non-lodging "service"-capability item carries no inventory —
			// infinite-stock, so there's nothing to deplete (ZBBS-HOME-296;
			// mirrors the gate-10 stock skip). Without this guard a consume_now
			// service offer would trip the drained-inventory error below.
			// nights_stay is the sole service kind today and is excluded from
			// this branch by the lodging guard above (LLM-391), so this handles
			// a hypothetical future non-lodging service kind.
			if !itemHasCapability(w, entry.ItemKind, "service") {
				have := seller.Inventory[entry.ItemKind]
				if have < entry.Qty {
					// Defensive — gate 10 ensured `seller.Inventory[kind]
					// >= Qty * effectiveConsumers`. If a subscriber fired
					// mid-loop somehow drained inventory, abort transfer.
					return payTransferOutcome{}, fmt.Errorf("commitPayTransfer: seller %q inventory drained mid-commit", seller.ID)
				}
				seller.Inventory[entry.ItemKind] = have - entry.Qty
				if seller.Inventory[entry.ItemKind] == 0 {
					delete(seller.Inventory, entry.ItemKind)
				}
			}
			if kept > 0 {
				if buyer.Inventory == nil {
					buyer.Inventory = make(map[ItemKind]int)
				}
				// Preflighted above against totalKept; per-split adds can't
				// overflow if the total didn't.
				buyer.Inventory[entry.ItemKind] += kept
			}
			applied := applyConsumeSatisfactions(consumer, def, eat)
			structureID, _ := resolveLoiteringObject(w, consumer.Pos, LoiterAttributionTiles)
			var stamped []DwellCreditSnapshot
			if structureID != "" && def != nil {
				stamped = UpsertItemDwellCredits(consumer, entry.ItemKind, def.Satisfies, structureID, at)
			}
			// Kept is stamped only on the BUYER's own consume event — the
			// surplus lands in the buyer's inventory, so a non-buyer
			// consumer's event claiming "you kept N" would be false (and its
			// narration beat would tell the wrong actor they pocketed food).
			// Group-order surplus from non-buyer consumers reaches the buyer
			// silently (their inventory shows it next tick). code_review.
			eventKept := 0
			if cid == entry.BuyerID {
				eventKept = kept
				out.buyerAte = eat
				out.mealMinutes = maxDwellMinutes(stamped)
			}
			w.emit(&ItemConsumed{
				ActorID: cid,
				Kind:    entry.ItemKind,
				Qty:     eat,
				Kept:    eventKept,
				Applied: applied,
				At:      at,
			})
			if len(stamped) > 0 {
				narration := ""
				if def != nil {
					narration = def.ConsumeDwellNarration
				}
				w.emit(&DwellStarted{
					ActorID:       cid,
					Kind:          entry.ItemKind,
					StructureID:   structureID,
					Credits:       stamped,
					NarrationText: narration,
					At:            at,
				})
			}
		}
		if out.buyerAte > 0 {
			out.satisfiesNeed, out.feltAfter = buyerFeltAfterConsume(buyer, def, w.Settings.NeedThresholds)
		}
	} else if itemHasCapability(w, entry.ItemKind, "lodging") {
		if isAdvanceLodgingBooking(w, entry, at) {
			// FUTURE reservation: a deferred booking, NOT an immediate handover.
			// Mint the Order at Ready and leave it for the keeper to check the
			// guest in via deliver_order on the booked day — the room grant
			// (AssignBedroomForLodger) happens there, and the eviction exemption
			// is gated on it. ZBBS-HOME-403 advance booking; see the
			// salem-engine-v2/lodging codebase note.
			bookedID := createOrderForPayWithItem(w, entry, at)
			// Postcondition, not trust (code_review R2): orderMinted must
			// track world state, so a future internal no-op path in the
			// helper falls back to the order-less write-through below
			// instead of silently persisting nothing. The two eager
			// branches get this for free — mintAndFulfillOrderNow errors
			// out itself when the order isn't in w.Orders post-mint.
			orderMinted = w.Orders[bookedID] != nil
			// booked reports "an advance-booking Order now exists", not
			// "this branch ran" — keep it tied to the postcondition so the
			// buyer's tool feedback can't claim a booking that didn't mint
			// (code_review R3; today the two are always equal).
			out.booked = orderMinted
		} else {
			// SAME-DAY walk-in (LLM-84): grant the room NOW, at accept, the same
			// way physical goods are handed over eagerly. mintAndFulfillOrderNow
			// runs transferOrderGoods, whose lodging branch calls
			// AssignBedroomForLodger; the Order is flipped Delivered after the
			// Paid facts below. The pre-commit availability gate (gate 10b /
			// fast-path) guarantees a room is grantable, so this can't fail for
			// contention. The guest holds the room immediately and beds into it
			// at night via the sleep machine — no separate keeper check-in.
			o, err := mintAndFulfillOrderNow(w, entry, seller, at)
			if err != nil {
				return payTransferOutcome{}, err
			}
			eagerlyDelivered = o
			orderMinted = true
			out.lodgedNow = true
		}
	} else if itemHasCapability(w, entry.ItemKind, CapabilityMending) {
		// Mending (LLM-625): always same-day — the buyer is standing in the
		// shop in the clothes being mended, so there is no advance-booking
		// arm. mintAndFulfillOrderNow runs transferOrderGoods, whose mending
		// branch restores the buyer's worn units and draws the seller's
		// thread; gate 10c pre-validated all three preconditions, so this
		// can't fail for contention. The Order flips Delivered after the Paid
		// facts below, the lodging walk-in shape.
		o, err := mintAndFulfillOrderNow(w, entry, seller, at)
		if err != nil {
			return payTransferOutcome{}, err
		}
		eagerlyDelivered = o
		orderMinted = true
		out.mendedNow = true
	} else if itemHasCapability(w, entry.ItemKind, CapabilityEquipmentService) {
		// Equipment service (LLM-648): always same-day — the wright is
		// standing at the buyer's business, tools in hand, so there is no
		// advance-booking arm. mintAndFulfillOrderNow runs
		// transferOrderGoods, whose service branch resets the business's
		// EquipmentUse and draws the wright's whetstone; gate 10d
		// pre-validated all three preconditions, so this can't fail for
		// contention. The mending walk-in shape.
		o, err := mintAndFulfillOrderNow(w, entry, seller, at)
		if err != nil {
			return payTransferOutcome{}, err
		}
		eagerlyDelivered = o
		orderMinted = true
		out.servicedNow = true
	} else if isCommissionOrder(w, seller, entry) {
		// Commission (LLM-338): the seller MAKES this good but doesn't hold enough
		// to hand over now, so mint a DEFERRED Ready Order — the same shape as an
		// advance lodging booking — instead of transferring stock that doesn't
		// exist. The keeper forges it and hands it over via deliver_order once made
		// (gate 5 stock is the readiness gate); refund-on-expiry (ZBBS-HOME-403)
		// returns the buyer's coins if it's never delivered. eagerlyDelivered stays
		// nil, so nothing flips to Delivered below — the Order sits Ready. Restamp
		// ExpiresAt from the recipe's forge lead time: a commission needs far
		// longer than the 10-minute takeaway TTL the mint gives a non-lodging Order.
		bookedID := createOrderForPayWithItem(w, entry, at)
		if o := w.Orders[bookedID]; o != nil {
			o.ExpiresAt = commissionOrderExpiresAt(w, entry.ItemKind, at)
			// LLM-357: record the deposit ONLY for a genuine partial payment
			// (0 < deposit < total), where depositChargeForEntry charged just the
			// deposit at accept and orderBalanceDue must collect the rest at
			// deliver_order. A full-prepay commission leaves Deposit at the zero
			// sentinel. This lives on the commission branch — NOT in
			// createOrderForPayWithItem, which the lodging path also uses and where
			// the full amount is always charged — so a deposit can never attach to
			// an order that was already paid in full.
			if entry.Deposit > 0 && entry.Deposit < entry.Amount {
				o.Deposit = entry.Deposit
			}
		}
		orderMinted = w.Orders[bookedID] != nil
	} else {
		// Physical take-home (ZBBS-HOME-398): mint the Order and move the goods
		// to the buyer right now, at accept, while the parties are co-present and
		// the seller holds the stock. Nothing to defer, so no window for the
		// takeaway-expiry robbery (HOME-396). The Order is minted (so its durable
		// pay_ledger row persists for the price-book restart seed) but the flip to
		// Delivered is held until after the Paid/PaidBy facts below. A non-nil
		// return is a substrate invariant violation (gates guaranteed
		// fulfillment), handled like the ConsumeNow drift errors above.
		o, err := mintAndFulfillOrderNow(w, entry, seller, at)
		if err != nil {
			return payTransferOutcome{}, err
		}
		eagerlyDelivered = o
		orderMinted = true
		out.tookHome = true
	}

	// Bidirectional Paid / PaidBy SalientFacts for the buyer↔seller
	// pair. Per-consumer relationship writes (buyer↔consumer,
	// seller↔consumer) intentionally NOT performed — the bookkeeping
	// gets thorny on a 6-person group order and the per-consumer
	// ItemConsumed event already gives subscribers the per-consumer
	// signal they need.
	buyerName := buyer.DisplayName
	sellerName := seller.DisplayName
	buyerFact := payAcceptedFactText(buyerName, sellerName, entry.Amount, entry.PayItems, entry.ItemKind, entry.Qty, len(entry.ConsumerIDs), forText, true)
	sellerFact := payAcceptedFactText(buyerName, sellerName, entry.Amount, entry.PayItems, entry.ItemKind, entry.Qty, len(entry.ConsumerIDs), forText, false)
	if _, err := RecordInteraction(entry.BuyerID, entry.SellerID, InteractionPaid, buyerFact, at).Fn(w); err != nil {
		log.Printf("sim.commitPayTransfer: RecordInteraction buyer→seller %q→%q: %v", entry.BuyerID, entry.SellerID, err)
	}
	if _, err := RecordInteraction(entry.SellerID, entry.BuyerID, InteractionPaidBy, sellerFact, at).Fn(w); err != nil {
		log.Printf("sim.commitPayTransfer: RecordInteraction seller→buyer %q→%q: %v", entry.SellerID, entry.BuyerID, err)
	}

	// ZBBS-HOME-398: now that the Paid/PaidBy facts exist, flip the
	// eagerly-fulfilled Order (take-home goods, or a same-day room — LLM-84) to
	// Delivered — so OrderDelivered fires AFTER the payment facts (code_review).
	// The Delivered/Received facts are intentionally NOT written: paid and
	// received coincide in this same instant, so the Paid/PaidBy pair above
	// already records the exchange (unlike the deferred deliver_order path, where
	// the handover is a separate later beat with its own facts).
	if eagerlyDelivered != nil {
		flipOrderTerminal(w, eagerlyDelivered, OrderStateDelivered, at)
	}

	// LLM-246: a settlement that minted no Order — today an eat-here single
	// or a bundle take — writes its durable pay_ledger row here, at accept.
	// Order-minting settlements persist via the checkpoint upsert instead;
	// double-writing them here would collide on the same id. Keyed on the
	// per-branch orderMinted outcome, not on entry shape (code_review).
	if !orderMinted {
		w.writeOrderlessSettlement(entry, at)
	}
	return out, nil
}

// deliverBundleLines delivers every line of a bundle quote-take (LLM-101) — the
// per-line analogue of commitPayTransfer's single-item delivery. A bundle is
// fast-path only (minted Accepted) and mints NO durable Order: take-home goods
// are handed straight to the consumers and eat-here lines are consumed in
// place, both persisted via the Actors checkpoint. Bundles can't contain a
// service/lodging kind (rejected at quote creation), so there is no Order /
// room-grant branch. Coins (and any barter) were already moved by the caller.
//
// Lines hold distinct canonical kinds (merged at quote creation), so per-line
// buyer-inventory pockets never alias. The fast-path gates already validated
// per-line stock, so the seller-drain / transfer errors here are substrate
// invariant violations, not domain failures. consumers is the resolved list
// (normalized to [buyer] when the offer was sole-consumer).
func deliverBundleLines(w *World, buyer, seller *Actor, entry *PayLedgerEntry, consumers []ActorID, at time.Time) (payTransferOutcome, error) {
	var out payTransferOutcome
	if entry.ConsumeNow {
		// Eat-here bundle: each consumer eats what their needs absorb per line
		// (consumableUnits clamp, ZBBS-WORK-391); the surplus pockets to the
		// BUYER, who paid. Same shape as the single-item consume_now branch,
		// looped per line, with the buyer-overflow preflighted per line's kind.
		type bundleSplit struct {
			cid       ActorID
			consumer  *Actor
			eat, kept int
		}
		for _, ln := range entry.Lines {
			def := w.ItemKinds[ln.ItemKind]
			splits := make([]bundleSplit, 0, len(consumers))
			totalKept := 0
			for _, cid := range consumers {
				consumer, ok := w.Actors[cid]
				if !ok {
					continue
				}
				eat := consumableUnits(consumer, def, ln.Qty)
				splits = append(splits, bundleSplit{cid: cid, consumer: consumer, eat: eat, kept: ln.Qty - eat})
				totalKept += ln.Qty - eat
			}
			if totalKept > 0 {
				if _, err := addChecked(buyer.Inventory[ln.ItemKind], totalKept); err != nil {
					return payTransferOutcome{}, fmt.Errorf("deliverBundleLines: buyer %q %s balance would overflow pocketing surplus", buyer.ID, ln.ItemKind)
				}
			}
			out.keptToInventory += totalKept
			for _, sp := range splits {
				have := seller.Inventory[ln.ItemKind]
				if have < ln.Qty {
					return payTransferOutcome{}, fmt.Errorf("deliverBundleLines: seller %q %s drained mid-commit", seller.ID, ln.ItemKind)
				}
				seller.Inventory[ln.ItemKind] = have - ln.Qty
				if seller.Inventory[ln.ItemKind] == 0 {
					delete(seller.Inventory, ln.ItemKind)
				}
				if sp.kept > 0 {
					if buyer.Inventory == nil {
						buyer.Inventory = make(map[ItemKind]int)
					}
					buyer.Inventory[ln.ItemKind] += sp.kept
				}
				applied := applyConsumeSatisfactions(sp.consumer, def, sp.eat)
				structureID, _ := resolveLoiteringObject(w, sp.consumer.Pos, LoiterAttributionTiles)
				var stamped []DwellCreditSnapshot
				if structureID != "" && def != nil {
					stamped = UpsertItemDwellCredits(sp.consumer, ln.ItemKind, def.Satisfies, structureID, at)
				}
				// Kept is stamped only on the BUYER's own event — the surplus
				// lands in the buyer's inventory (matches the single-item rule).
				eventKept := 0
				if sp.cid == entry.BuyerID {
					eventKept = sp.kept
					out.buyerAte += sp.eat
					if m := maxDwellMinutes(stamped); m > out.mealMinutes {
						out.mealMinutes = m
					}
				}
				w.emit(&ItemConsumed{
					ActorID: sp.cid,
					Kind:    ln.ItemKind,
					Qty:     sp.eat,
					Kept:    eventKept,
					Applied: applied,
					At:      at,
				})
				if len(stamped) > 0 {
					narration := ""
					if def != nil {
						narration = def.ConsumeDwellNarration
					}
					w.emit(&DwellStarted{
						ActorID:       sp.cid,
						Kind:          ln.ItemKind,
						StructureID:   structureID,
						Credits:       stamped,
						NarrationText: narration,
						At:            at,
					})
				}
			}
		}
		// SatisfiesNeed / FeltAfter stay zero for a bundle — multiple kinds
		// satisfy multiple needs, so a single per-need verdict would mislead;
		// the settle feedback degrades to the generic bundle line.
		return out, nil
	}

	// Take-home bundle: hand each line's qty to each consumer (no Order). For
	// the common sole-consumer offer that is the buyer. Per-line stock was
	// validated by the fast-path gates, so transferItem cannot fail in practice.
	for _, ln := range entry.Lines {
		for _, cid := range consumers {
			consumer, ok := w.Actors[cid]
			if !ok {
				return payTransferOutcome{}, fmt.Errorf("deliverBundleLines: consumer %q vanished mid-commit", cid)
			}
			if err := transferItem(w, seller, consumer, ln.ItemKind, ln.Qty); err != nil {
				return payTransferOutcome{}, fmt.Errorf("deliverBundleLines: transfer %s to %q: %w", ln.ItemKind, cid, err)
			}
		}
	}
	out.tookHome = true
	return out, nil
}

// consumableUnits returns how many of maxQty units the actor's current
// needs can actually absorb — the largest per-attribute ceil(need /
// per-unit-restore) across the item's immediate satisfactions. A positive
// maxQty is clamped to [1, maxQty]; a non-positive maxQty returns 0 (both
// callers validate qty >= 1 first, but this helper is shared logic and must
// not promise a floor it can't keep). The floor of 1 is deliberate: a
// consume was asked for, so one unit is always eaten even when every need
// is already at zero (the in-world beat is "you eat one and are full; the
// rest you keep", not a purchase that eats nothing). Both consume paths
// share this so the accept-time clamp can't be defeated by re-consuming
// the pocketed surplus next tick (ZBBS-WORK-391).
func consumableUnits(actor *Actor, def *ItemKindDef, maxQty int) int {
	if maxQty <= 0 {
		return 0
	}
	if maxQty == 1 || actor == nil || def == nil {
		return maxQty
	}
	units := 0
	for _, s := range def.Satisfies {
		if s.Immediate <= 0 {
			continue
		}
		need := actor.Needs[s.Attribute]
		if need <= 0 {
			continue
		}
		n := (need + s.Immediate - 1) / s.Immediate
		if n > units {
			units = n
		}
	}
	if units < 1 {
		units = 1
	}
	if units > maxQty {
		units = maxQty
	}
	return units
}

// applyConsumeSatisfactions applies a Consume's per-qty satisfaction
// effect to actor.Needs, mirroring the inner loop of sim.Consume. Used
// by the ConsumeNow branch of commitPayTransfer when items are
// consumed at acceptance time rather than added to inventory.
//
// Returns the per-need post-clamp deltas (positive magnitudes; absent
// from the map when the need didn't actually move). nil def is a
// no-op (def is nil only for an ItemKind whose catalog entry vanished,
// which the AcceptPay gate-8 + fast-path predicate-6 both check before
// reaching this helper).
func applyConsumeSatisfactions(actor *Actor, def *ItemKindDef, qty int) map[NeedKey]int {
	if actor == nil || def == nil {
		return nil
	}
	if actor.Needs == nil {
		actor.Needs = make(map[NeedKey]int)
	}
	var applied map[NeedKey]int
	for _, s := range def.Satisfies {
		if s.Immediate <= 0 {
			continue
		}
		pre := actor.Needs[s.Attribute]
		post := ClampNeed(pre - s.Immediate*qty)
		if pre == post {
			continue
		}
		actor.Needs[s.Attribute] = post
		if applied == nil {
			applied = make(map[NeedKey]int)
		}
		applied[s.Attribute] = pre - post
	}
	return applied
}

// actorDisplayName returns the actor's DisplayName, or "" if the actor
// isn't in the world. Used by SalientFact text builders for declined /
// countered paths where the helper doesn't have the *Actor pointer
// already in hand.
func actorDisplayName(w *World, id ActorID) string {
	a, ok := w.Actors[id]
	if !ok || a == nil {
		return ""
	}
	return a.DisplayName
}

// payAcceptedFactText renders the SalientFact text for an accepted pay-
// with-item transfer. The two sides share the same shape with subject /
// object pronouns flipped via buyerSide.
//
//	buyerSide=true:  "I paid Aldous 5 coins for 2 stew." / with consumers:
//	                  "I paid Aldous 5 coins for 2 stew × 3."
//	buyerSide=false: "Hannah paid me 5 coins for 2 stew."
//
// payItems folds the barter goods into the payment phrase via formatPayment
// ("5 nails", "5 nails and 3 coins"), so a goods trade reads "I paid Aldous
// 5 nails for 2 stew." (ZBBS-HOME-393).
//
// forText is folded in as " for <trim>" before the final period when
// non-empty (mirrors PR B's payFactText).
func payAcceptedFactText(buyerName, sellerName string, amount int, payItems []ItemKindQty, kind ItemKind, qty, consumerCount int, forText string, buyerSide bool) string {
	subject, object, verb := buyerName, sellerName, "paid"
	if buyerSide {
		subject = "I"
	} else {
		object = "me"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s %s for %d %s", subject, verb, object, formatPayment(amount, payItems), qty, kind)
	if consumerCount > 1 {
		fmt.Fprintf(&b, " × %d", consumerCount)
	}
	forText = strings.TrimSpace(forText)
	if forText != "" {
		fmt.Fprintf(&b, " for %s", forText)
	}
	b.WriteString(".")
	return b.String()
}

// payDeclinedFactText renders a SalientFact for a declined offer.
//
//	buyerSide=true:  "Aldous declined my offer of 5 coins for 2 stew."
//	                  + " Reason: <reason>." when reason non-empty.
//	buyerSide=false: "I declined Hannah's offer of 5 coins for 2 stew."
//	                  + " Reason: <reason>." when reason non-empty.
//
// payItems folds the barter goods into the offer phrase (ZBBS-HOME-393).
func payDeclinedFactText(buyerName, sellerName string, amount int, payItems []ItemKindQty, kind ItemKind, qty int, reason string, buyerSide bool) string {
	offer := formatPayment(amount, payItems)
	var b strings.Builder
	if buyerSide {
		fmt.Fprintf(&b, "%s declined my offer of %s for %d %s", sellerName, offer, qty, kind)
	} else {
		fmt.Fprintf(&b, "I declined %s's offer of %s for %d %s", buyerName, offer, qty, kind)
	}
	if reason != "" {
		fmt.Fprintf(&b, ". Reason: %s", reason)
	}
	b.WriteString(".")
	return b.String()
}

// payCounteredFactText renders a SalientFact for a counter.
//
//	buyerSide=true:  "Aldous countered my offer of 5 coins for 2 stew with 7 coins."
//	                  + " Note: <message>." when message non-empty.
//	buyerSide=false: "I countered Hannah's offer of 5 coins for 2 stew with 7 coins."
//	                  + " Note: <message>." when message non-empty.
//
// originalPayItems / counterPayItems fold the barter goods into the offer
// and counter phrases respectively (ZBBS-HOME-393), so a goods haggle reads
// "Aldous countered my offer of 5 nails for 2 stew with 6 nails."
func payCounteredFactText(buyerName, sellerName string, originalAmount int, originalPayItems []ItemKindQty, counterAmount int, counterPayItems []ItemKindQty, kind ItemKind, qty int, message string, buyerSide bool) string {
	offer := formatPayment(originalAmount, originalPayItems)
	counter := formatPayment(counterAmount, counterPayItems)
	var b strings.Builder
	if buyerSide {
		fmt.Fprintf(&b, "%s countered my offer of %s for %d %s with %s", sellerName, offer, qty, kind, counter)
	} else {
		fmt.Fprintf(&b, "I countered %s's offer of %s for %d %s with %s", buyerName, offer, qty, kind, counter)
	}
	if message != "" {
		fmt.Fprintf(&b, ". Note: %s", message)
	}
	b.WriteString(".")
	return b.String()
}
