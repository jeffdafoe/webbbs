package sim

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// scene_quote_commands.go — Phase 3 PR S3.
//
// sim.SceneQuoteCreate is the substrate-level Command for the
// scene_quote tool. Implements the 10 validation gates locked at
// scene-quote-design § 5 (seller scene, item catalog, break, stock,
// target buyer resolution, consumer-huddle requirement, consumer
// resolution, numeric guards, duplicate-key replacement, per-(seller,
// scene) cap), then mints the quote and emits SceneQuoteCreated.
//
// The handler (handlers/scene_quote.go) is a pure builder per PR shape
// rule #3 — static-only validation, no world reads. World-state
// validation runs here per rule #4 — Command Fn re-validates
// everything the handler did, since SceneQuoteCreate is exported and
// non-handler callers (tests, future in-engine cascades) could
// otherwise bypass the bounds.

// MaxSceneQuoteAmount caps the bundle total in coins. Matches
// MaxPayAmount so a buyer's pay_with_item fast-path doesn't need to
// reconcile a different ceiling at acceptance time.
const MaxSceneQuoteAmount = math.MaxInt32

// MaxSceneQuoteQty caps qty-per-consumer. Same ceiling as
// MaxConsumeQty. The Command Fn additionally enforces that
// Qty * effectiveConsumerCount doesn't overflow before the stock
// check uses the product.
const MaxSceneQuoteQty = math.MaxInt32

// QuoteLineInput is one bundle line as it arrives from the tool layer: a
// free-text item NAME (resolved to a canonical ItemKind inside the Command Fn
// via resolveOrMintItemKind) and a positive per-consumer quantity. The Command
// turns a []QuoteLineInput into the resolved []QuoteLine it stores on the
// quote, merging duplicate canonical kinds (LLM-101). Mirrors PayItemInput's
// free-text-name shape on the pay side.
type QuoteLineInput struct {
	ItemName string
	Qty      int
}

// SceneQuoteCreateResult is the value returned by the SceneQuoteCreate
// Command — the handler narrates QuoteID and ExpiresAt back to the
// LLM so it has a stable identifier to reference in a follow-up
// speak ("I've got 2 stew for 6 coins, quote #5") and a timer
// horizon. Returned by Command.Fn; the handler wraps it for the
// tool result.
type SceneQuoteCreateResult struct {
	QuoteID   QuoteID
	ExpiresAt time.Time
	// EatHereClamped is true when the seller proposed take-home but the
	// engine forced eat-here (non-portable consumable, ZBBS-WORK-405 —
	// the seller-side mirror of the pay_with_item buyer clamp). Carried
	// on the result so the seller model's tool feedback can state the
	// adjusted disposition instead of leaving it believing it posted a
	// take-home quote.
	EatHereClamped bool
	// Announced is true when the seller's `say` line was spoken alongside the
	// quote (LLM-343). The speech is best-effort: a quote that posts but whose
	// words are refused still stands, so the seller's tool feedback must not
	// claim it was heard. Set by HandleSceneQuote after SpeakTo, not by
	// SceneQuoteCreate itself.
	Announced bool
	// SayRefused carries SpeakTo's model-facing reason when the quote posted but
	// the words did not go out — the line named someone who has left the
	// conversation, or the seller has already spoken and is owed a reply. Empty
	// when Announced, or when no `say` was offered. Echoed verbatim in the tool
	// result: guessing at the reason would tell the seller the wrong thing to
	// fix.
	SayRefused string
}

// SceneQuoteCreate returns a Command that mints a SceneQuote on the
// seller's current scene and emits SceneQuoteCreated. Phase 3 PR S3.
//
// itemName is raw free-text from the LLM tool call; the Command
// resolves it to a canonical ItemKind via resolveItemKind
// (case-insensitive + trim) — same pattern as Consume.
//
// targetBuyerName empty = public quote; non-empty = single addressed
// buyer (resolved against the scene's participants, NOT the huddle —
// scene is the quote's visibility scope, design § 5 gate 5).
//
// consumerNames empty = buyer is implicit single consumer; non-empty =
// group-order participant list (resolved against the seller's huddle
// peers, design § 5 gate 7). Requires seller to have an active huddle.
//
// Per design § 5 the gates run in this order: cheap structural checks
// first (scene, catalog), then state (break, stock, resolution), then
// maintenance (duplicate-key, cap). Maximizes early-exit and lets
// the costly cap-displacement and emit work only on validated inputs.
//
// On success:
//
//   - Mints a fresh QuoteID via w.nextQuoteSeq.
//   - Inserts SceneQuote into w.Quotes (state Active, ExpiresAt =
//     now + effective TTL).
//   - Appends QuoteID to scene.QuoteIDs.
//   - Emits SceneQuoteCreated (subscriber stamps targeted-buyer
//     warrant if TargetBuyer is an NPC).
//   - Returns SceneQuoteCreateResult{QuoteID, ExpiresAt}.
//
// On duplicate-key (gate 9): old quote → terminal Superseded,
// removed from scene index, SceneQuoteExpired{Reason: "superseded"}
// emitted BEFORE the new quote's SceneQuoteCreated, so admin replay
// sees the displacement in causal order.
//
// On cap-hit (gate 10): oldest active quote in (seller, scene)
// bucket → terminal CapDisplaced, same removal + emit shape as
// supersede.
func SceneQuoteCreate(
	sellerID ActorID,
	lines []QuoteLineInput,
	amount int,
	consumeNow bool,
	targetBuyerName string,
	consumerNames []string,
	at time.Time,
) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Defense-in-depth re-validation. SceneQuoteCreate is
			// exported — non-handler callers (tests, admin paths,
			// future cascades) could pass through bad shapes.
			if amount < 1 {
				return nil, fmt.Errorf("SceneQuoteCreate: amount must be at least 1 (got %d)", amount)
			}
			if amount > MaxSceneQuoteAmount {
				return nil, fmt.Errorf("SceneQuoteCreate: amount exceeds maximum (got %d, max %d)", amount, MaxSceneQuoteAmount)
			}
			if len(lines) == 0 {
				return nil, errors.New("a quote must offer at least one item line.")
			}

			seller, ok := w.Actors[sellerID]
			if !ok {
				return nil, fmt.Errorf("SceneQuoteCreate: seller %q not in world", sellerID)
			}

			// Gate 1: seller has an active scene. Scenes observe
			// huddles; the seller's scene is derived from
			// seller.CurrentHuddleID → look up scene in w.Scenes
			// whose Huddles set contains that huddle. Quotes have
			// no perceptive audience without one.
			if seller.CurrentHuddleID == "" {
				return nil, errors.New(
					"you're not in a conversation — start one with the people you want to quote items to first.",
				)
			}
			sceneID, ok := resolveSellerScene(w, seller.CurrentHuddleID)
			if !ok {
				return nil, errors.New(
					"your current conversation isn't anchored to a scene — wait for the scene to be established before posting a quote.",
				)
			}
			scene := w.Scenes[sceneID]

			// Gate 2: resolve the ItemKind. ZBBS-WORK-412: on a miss, mint it
			// at qty 0 — a seller quoting a good the world doesn't have yet is a
			// discovery signal. The seller holds 0 of a freshly-minted kind, so
			// the stock gate below still rejects the quote.
			resolvedLines, err := resolveQuoteLines(w, lines)
			if err != nil {
				return nil, err
			}

			// A bundle (>1 line) can't carry a service-capability kind (e.g.
			// nights_stay): a service has no inventory and delivers as a
			// deferred/eager Order, which the bundle take path deliberately
			// does not mint. Service items stay on the single-item quote path.
			if len(resolvedLines) > 1 {
				for _, ln := range resolvedLines {
					if itemHasCapability(w, ln.ItemKind, "service") {
						return nil, fmt.Errorf(
							"%s can't be part of a bundle — quote it on its own.", ln.ItemKind,
						)
					}
				}
			}

			// ZBBS-WORK-405 (extended to bundles, LLM-101): if ANY line is a
			// non-portable consumable the whole bundle clamps to eat-here —
			// disposition is bundle-level, and a basket holding a served dish
			// can't be carried out. Applied before the quote becomes anything
			// (dedup key, supersede match, the persisted quote, the Created
			// event), so a clamped buyer offer can opportunistically match (the
			// HOME-424 auto-match requires disposition equality) and the seller
			// model isn't left believing it posted a take-home offer it didn't.
			eatHereClamped := false
			if !consumeNow {
				for _, ln := range resolvedLines {
					if w.ItemKinds[ln.ItemKind].EatHereOnly() {
						consumeNow = true
						eatHereClamped = true
						break
					}
				}
			}

			// Gate 3: closed-shop / break gate (simple-strict).
			// Same rule applied at pay_with_item / accept_pay per
			// ledger-substrate § 11. Future structure-level
			// closed-shop check goes here too.
			if seller.BreakUntil != nil && seller.BreakUntil.After(at) {
				return nil, errors.New(
					"you're on a break right now — wait until your break ends before posting a quote.",
				)
			}
			// Gate 6 (before gate 4 — consumer requires huddle is
			// a structural prerequisite to the stock-check
			// arithmetic, since effectiveConsumerCount depends on
			// the consumer list shape).
			if len(consumerNames) > 0 && seller.CurrentHuddleID == "" {
				// Defensive — gate 1 already enforced the huddle
				// requirement, but a future change could relax
				// gate 1 (e.g. public-scene quotes without a
				// huddle). Keep this check tight to the consumer
				// semantic.
				return nil, errors.New(
					"consumers can only be specified within an active huddle.",
				)
			}
			if len(consumerNames) > SceneQuoteMaxConsumers {
				return nil, fmt.Errorf(
					"too many consumers (got %d, max %d) — split the order into smaller quotes.",
					len(consumerNames), SceneQuoteMaxConsumers,
				)
			}

			// Gate 7: consumer resolution. Per-name case-insensitive
			// trim-match against seller's huddle peers; ambiguity
			// + missing both reject; duplicate-name reject;
			// seller-as-consumer reject. Buyer-as-consumer
			// allowed (the buyer often is one of the consumers in
			// "round of ale for the table" semantics).
			consumerIDs, err := resolveQuoteConsumers(w, seller, consumerNames)
			if err != nil {
				return nil, err
			}

			// Gate 8 + 4: per-line overflow guard, then stock check.
			// effectiveConsumers is bundle-level (the consumer set applies to
			// every line); each line stock-checks independently.
			effectiveConsumers := len(consumerIDs)
			if effectiveConsumers == 0 {
				effectiveConsumers = 1
			}
			for _, ln := range resolvedLines {
				// Overflow guard: Qty * effectiveConsumers must fit in int.
				// Inputs are capped at MaxSceneQuoteQty (MaxInt32), so on a
				// 32-bit int platform the product could wrap and a negative
				// would silently pass the stock check.
				if ln.Qty > math.MaxInt/effectiveConsumers {
					return nil, fmt.Errorf(
						"SceneQuoteCreate: qty %d × %d consumers overflows int — split the order.",
						ln.Qty, effectiveConsumers,
					)
				}
				needed := ln.Qty * effectiveConsumers
				// Service-capability items carry no inventory (capacity grant,
				// not stock) so they skip the stock check. A bundle can't hold
				// one (rejected above), so this only fires for a single-item
				// service quote, preserving prior behavior.
				if itemHasCapability(w, ln.ItemKind, "service") {
					continue
				}
				// Coverage is on-hand MINUS goods earmarked for a Ready order
				// (sellerCoverableStock, LLM-409): a quote backed only by stock
				// already promised to a pending delivery is uncoverable the moment
				// it posts and would flip to shortfall on the next reconcile pass.
				// Gating create on the same reserved-aware predicate the accept path
				// and the reconcile use keeps all three coverage checks in one shape
				// (previously create gated on raw Inventory and could mint a
				// born-dead lot).
				if coverable, reserved := sellerCoverableStock(w, seller, ln.ItemKind); coverable < needed {
					return nil, fmt.Errorf(
						"insufficient stock (have %d %s, reserved %d, need %d) — quote a smaller quantity before posting.",
						seller.Inventory[ln.ItemKind], ln.ItemKind, reserved, needed,
					)
				}
			}

			// Gate 5: TargetBuyer resolution. Empty stays empty
			// (public quote). Non-empty resolves against actors
			// currently in the scene — every actor in any huddle
			// the scene observes is a scene participant. Same
			// ambiguity / missing rejection as findHuddlePeerByDisplayName.
			var targetBuyerID ActorID
			if strings.TrimSpace(targetBuyerName) != "" {
				targetBuyerID, err = resolveQuoteTargetBuyer(w, sellerID, scene, targetBuyerName)
				if err != nil {
					return nil, err
				}
			}

			// LLM-208: seller-side homed-guest lodging gate. A homed buyer can't
			// take a room — the buyer-side pay_with_item guard rejects it
			// (pay_with_item_commands.go, LLM-182) — so a nights_stay quote aimed
			// at one only dangles an offer that dead-ends in a doomed nightly
			// negotiation (John Ellis → Prudence Ward, live 2026-06-30). Reject at
			// creation, the seller-side mirror of that buyer gate. Keyed on the
			// "lodging" capability so a future operator-defined lodging kind
			// inherits it. Only a resolved target can be pre-checked here; a public
			// quote is suppressed per-viewer in perception instead
			// (build.go filterHomedLodgingQuoteWarrants).
			if targetBuyerID != "" && quoteLinesGrantLodging(w, resolvedLines) {
				if targetBuyer, ok := w.Actors[targetBuyerID]; ok && targetBuyer.HomeStructureID != "" {
					if home, ok := w.Structures[targetBuyer.HomeStructureID]; ok && home.DisplayName != "" {
						return nil, fmt.Errorf(
							"%s has a home (%s) and doesn't need a room — don't offer them lodging.",
							targetBuyer.DisplayName, home.DisplayName,
						)
					}
					return nil, fmt.Errorf(
						"%s has a home and doesn't need a room — don't offer them lodging.",
						targetBuyer.DisplayName,
					)
				}
			}

			// LLM-645: seller-side churn gate — the other half of the LLM-555
			// buy-back memory. The buyer-side gate (pay_with_item_commands.go)
			// refuses buying a good back off the person you sold it to within
			// SoldToPeerMemoryTTL, but it deliberately stands aside when the
			// counterparty holds an active targeted quote for that kind (the
			// LLM-551 escape hatch: a posted quote reads as "I do mean to sell
			// you this"). A vendor pair defeats that in practice, because
			// vendors QUOTE: the buyer of the first leg posts the reverse
			// offer himself and hands the original seller the hatch key. Live
			// 2026-08-24: the factor sold Josiah 3 woolens at 19:36, Josiah
			// posted him a targeted quote for those woolens, and the factor
			// took it (quote_id 13) at 20:19 — straight through the hatch;
			// Josiah then re-bought them at 20:36 off the factor's bundle
			// quote. Three legs of the same woolens inside an hour, and every
			// churn case in the ledger since the gate shipped is this shape.
			//
			// So the quote itself is refused at creation: a TARGETED quote for
			// a kind the target sold the seller within the TTL is the churn's
			// first move, and without it the buyer-side gate holds — a PUBLIC
			// quote never opens the hatch (activeTargetedQuoteOffers requires
			// TargetBuyer). The memory consulted is the one LLM-555 already
			// stamps on the TARGET ("I sold kind to this seller"); no new
			// state. Kind-scoped like the buyer gate: inventory is fungible,
			// so "these exact units" has no meaning — quoting the kind to
			// ANYONE ELSE stays untouched, which keeps the distributor whole.
			// A PC target carries no such memory (the capture subscriber skips
			// non-agent sellers), so quoting at a player is never gated here.
			if targetBuyerID != "" {
				if target, ok := w.Actors[targetBuyerID]; ok {
					for _, ln := range resolvedLines {
						if ActorRecentlySoldTo(target, sellerID, ln.ItemKind, at) {
							return nil, fmt.Errorf(
								"you bought %s off %s a short while ago — you don't offer it straight back to them. Quote it to another buyer.",
								ln.ItemKind, target.DisplayName,
							)
						}
					}
				}
			}

			// Gate 9: duplicate-key resolution. Non-Amount key —
			// the legitimate use case for "same key, new amount"
			// IS re-pricing. Replace any matching active quote in
			// the same (seller, scene) bucket; the replaced
			// quote → terminal Superseded BEFORE the new quote's
			// SceneQuoteCreated emit so admin replay sees the
			// displacement in causal order.
			//
			// Run BEFORE the cap check (design § 4): an exact-key
			// duplicate frees a slot, which may save us from
			// hitting the cap.
			supersedeMatchingQuotes(w, scene, sellerID, resolvedLines, consumeNow, targetBuyerID, consumerIDs, at)

			// Gate 10: per-(seller, scene) cap. Count remaining
			// active quotes in the bucket (post-supersede); if at
			// cap, displace the oldest active quote.
			displaceQuotesIfAtCap(w, scene, sellerID, at)

			// Mint + insert + emit.
			id := w.nextQuoteSeq()
			ttl := effectiveSceneQuoteTTL(w.Settings)
			expiresAt := at.Add(ttl)
			q := &SceneQuote{
				ID:          id,
				SceneID:     sceneID,
				SellerID:    sellerID,
				TargetBuyer: targetBuyerID,
				Lines:       resolvedLines,
				ConsumeNow:  consumeNow,
				ConsumerIDs: consumerIDs,
				Amount:      amount,
				State:       SceneQuoteStateActive,
				CreatedAt:   at,
				ExpiresAt:   expiresAt,
			}
			w.Quotes[id] = q
			scene.QuoteIDs = append(scene.QuoteIDs, id)

			evt := &SceneQuoteCreated{
				QuoteID:     id,
				SceneID:     sceneID,
				SellerID:    sellerID,
				TargetBuyer: targetBuyerID,
				Lines:       cloneQuoteLines(resolvedLines),
				Amount:      amount,
				ConsumeNow:  consumeNow,
				ConsumerIDs: cloneActorIDs(consumerIDs),
				ExpiresAt:   expiresAt,
				At:          at,
				HuddleID:    seller.CurrentHuddleID,
			}
			w.emit(evt)

			// Stamp the source-event lineage on the quote AFTER
			// emit so the IDs are populated. Subscriber dispatch
			// has already run by this point (synchronous), so
			// the warrant subscriber sees the quote's lineage
			// via the event itself rather than via this back-
			// stamp; the back-stamp is for admin / replay queries
			// that walk World.Quotes directly.
			q.RootEventID = evt.RootEventID()
			q.SourceEventID = evt.EventID()

			return SceneQuoteCreateResult{
				QuoteID:        id,
				ExpiresAt:      expiresAt,
				EatHereClamped: eatHereClamped,
			}, nil
		},
	}
}

// resolveSellerScene picks the scene the seller's quote should anchor
// to from the set of scenes observing seller.CurrentHuddleID. A
// structure huddle may be observed by multiple structure scenes
// minted at the same structure over time; pick deterministically by
// lexicographically smallest SceneID — matches the perception layer's
// resolvePrimaryScene fallback so the quote ends up on the same
// scene the LLM is reasoning within.
//
// Returns ("", false) when no scene observes the huddle (the
// "huddle exists but no scene anchors it" race window).
func resolveSellerScene(w *World, huddleID HuddleID) (SceneID, bool) {
	var candidates []SceneID
	for id, scene := range w.Scenes {
		if scene == nil {
			continue
		}
		if _, observed := scene.Huddles[huddleID]; observed {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })
	return candidates[0], true
}

// resolveQuoteTargetBuyer walks every actor in every huddle the scene
// observes, looking for a single case-insensitive DisplayName match
// against name. Excludes the seller (you can't target-quote yourself).
// Ambiguity → reject, missing → reject — same rules as
// findHuddlePeerByDisplayName.
func resolveQuoteTargetBuyer(w *World, sellerID ActorID, scene *Scene, name string) (ActorID, error) {
	target := strings.TrimSpace(name)
	if target == "" {
		return "", errors.New("target_buyer is empty after trim")
	}
	var found ActorID
	seen := make(map[ActorID]struct{})
	for huddleID := range scene.Huddles {
		members, ok := w.actorsByHuddle[huddleID]
		if !ok {
			continue
		}
		for actorID := range members {
			if actorID == sellerID {
				continue
			}
			if _, dup := seen[actorID]; dup {
				continue
			}
			seen[actorID] = struct{}{}
			actor, ok := w.Actors[actorID]
			if !ok {
				continue
			}
			if strings.EqualFold(actor.DisplayName, target) {
				if found != "" {
					return "", fmt.Errorf(
						"more than one person named %q is in this scene — use a unique full name before quoting.",
						target,
					)
				}
				found = actorID
			}
		}
	}
	if found == "" {
		return "", fmt.Errorf(
			"no one named %q in this scene — re-check who is here before quoting.",
			target,
		)
	}
	return found, nil
}

// resolveQuoteConsumers resolves each consumer name to a huddle peer
// ActorID. Same rules per consumer entry as findHuddlePeerByDisplayName
// PLUS: duplicate-name reject (model intent unclear), seller-as-
// consumer reject. Buyer-as-consumer is allowed (the buyer often is
// in the consumer list for "round at the table" semantics — though
// at this layer we don't know who the buyer is, so the actual
// buyer-as-consumer enforcement happens at pay_with_item time).
//
// Returns the resolved IDs in input order; an empty input returns nil
// (matches the convention used elsewhere — empty == buyer is sole
// consumer).
func resolveQuoteConsumers(w *World, seller *Actor, names []string) ([]ActorID, error) {
	if len(names) == 0 {
		return nil, nil
	}
	members, ok := w.actorsByHuddle[seller.CurrentHuddleID]
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
				"no one named %q in this conversation — re-check who is here before quoting.",
				target,
			)
		}
		if found == seller.ID {
			return nil, errors.New(
				"the seller can't be a consumer of their own quote — drop your own name from the consumer list.",
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

// supersedeMatchingQuotes finds any active quote in (sellerID, scene)
// whose non-Amount key matches and flips it to Superseded. Amount is
// NOT part of the key — re-pricing the same terms IS the legitimate
// use case. Per scene-quote-design § 4 the consumer-set match is
// order-independent.
//
// Emits SceneQuoteExpired{Reason: "superseded"} for each replaced
// quote BEFORE the caller's subsequent mint emits
// SceneQuoteCreated, so admin replay sees the displacement in
// causal order.
//
// Typically replaces 0 or 1 quote (the key is tight enough that
// natural use produces unique entries); the design doesn't promise
// "at most one match," so the loop handles any count.
func supersedeMatchingQuotes(
	w *World,
	scene *Scene,
	sellerID ActorID,
	lines []QuoteLine,
	consumeNow bool,
	targetBuyerID ActorID,
	consumerIDs []ActorID,
	at time.Time,
) {
	if scene == nil || len(scene.QuoteIDs) == 0 {
		return
	}
	// Snapshot the index — we'll mutate scene.QuoteIDs via
	// removeQuoteFromSceneIndex inside the loop.
	ids := make([]QuoteID, len(scene.QuoteIDs))
	copy(ids, scene.QuoteIDs)
	for _, qid := range ids {
		q, ok := w.Quotes[qid]
		if !ok || q == nil || q.State != SceneQuoteStateActive {
			continue
		}
		if q.SellerID != sellerID {
			continue
		}
		if q.ConsumeNow != consumeNow || q.TargetBuyer != targetBuyerID {
			continue
		}
		// Bundle line set must match (order-independent, LLM-101) — the
		// multi-line analogue of the old ItemKind+Qty scalar key.
		if !quoteLinesEqual(q.Lines, lines) {
			continue
		}
		if !actorIDSetsEqual(q.ConsumerIDs, consumerIDs) {
			continue
		}
		flipQuoteTerminal(w, scene, q, SceneQuoteStateSuperseded, SceneQuoteExpiredReasonSuperseded, at)
	}
}

// displaceQuotesIfAtCap counts active quotes in (sellerID, scene) and,
// if at SceneQuoteMaxPerSellerScene, flips the oldest active one to
// CapDisplaced. Loops in case the bucket is over-cap (defensive —
// natural use can't exceed cap by more than one, but a future
// pathway could).
func displaceQuotesIfAtCap(w *World, scene *Scene, sellerID ActorID, at time.Time) {
	if scene == nil {
		return
	}
	for {
		var active []*SceneQuote
		for _, qid := range scene.QuoteIDs {
			q, ok := w.Quotes[qid]
			if !ok || q == nil || q.State != SceneQuoteStateActive {
				continue
			}
			if q.SellerID != sellerID {
				continue
			}
			active = append(active, q)
		}
		if len(active) < SceneQuoteMaxPerSellerScene {
			return
		}
		// Find oldest by CreatedAt (tie-break by QuoteID for
		// determinism — sub-microsecond mints in tests can share
		// a timestamp).
		sort.Slice(active, func(i, j int) bool {
			if !active[i].CreatedAt.Equal(active[j].CreatedAt) {
				return active[i].CreatedAt.Before(active[j].CreatedAt)
			}
			return active[i].ID < active[j].ID
		})
		flipQuoteTerminal(w, scene, active[0], SceneQuoteStateCapDisplaced, SceneQuoteExpiredReasonCapDisplaced, at)
	}
}

// flipQuoteTerminal performs the common terminal-state transition:
// flip State + stamp ResolvedAt, drop from scene.QuoteIDs index,
// emit SceneQuoteExpired with the given reason. Caller guarantees q
// is currently active.
func flipQuoteTerminal(w *World, scene *Scene, q *SceneQuote, state SceneQuoteState, reason string, at time.Time) {
	q.State = state
	q.ResolvedAt = at
	removeQuoteFromSceneIndex(scene, q.ID)
	w.emit(&SceneQuoteExpired{
		QuoteID:  q.ID,
		SceneID:  q.SceneID,
		SellerID: q.SellerID,
		Reason:   reason,
		At:       at,
	})
}

// actorIDSetsEqual reports whether two ActorID slices contain the same
// elements (order-independent, multiplicity-aware). Used by the
// duplicate-key check (consumer sets must match exactly regardless of
// the order the LLM listed names). Cheap O(n²) given the
// SceneQuoteMaxConsumers=8 cap; a map-based path isn't worth the
// allocation for tiny slices.
func actorIDSetsEqual(a, b []ActorID) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	counts := make(map[ActorID]int, len(a))
	for _, id := range a {
		counts[id]++
	}
	for _, id := range b {
		counts[id]--
		if counts[id] < 0 {
			return false
		}
	}
	return true
}

// cloneActorIDs returns an independent copy of ids. Used to give
// emitted events their own backing array so a mutation of the
// SceneQuote's slice post-emit doesn't reach back into a subscriber's
// retained event reference.
func cloneActorIDs(ids []ActorID) []ActorID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]ActorID, len(ids))
	copy(out, ids)
	return out
}

// cloneQuoteLines returns an independent copy of lines. QuoteLine is a flat
// value type, so copying the backing array fully isolates the clone — used to
// give the emitted SceneQuoteCreated event its own slice so a post-emit
// mutation of the quote's Lines can't reach a subscriber's retained reference.
func cloneQuoteLines(lines []QuoteLine) []QuoteLine {
	if len(lines) == 0 {
		return nil
	}
	out := make([]QuoteLine, len(lines))
	copy(out, lines)
	return out
}

// resolveQuoteLines resolves each free-text bundle line to a canonical
// ItemKind and merges duplicate kinds by summing their quantities (LLM-101).
// Per-line qty must be in [1, MaxSceneQuoteQty]; a merged total is
// overflow-checked and re-capped. Unknown names mint a kind at qty 0
// (ZBBS-WORK-412 discovery signal the caller's stock gate then rejects);
// resolveOrMintItemKind only returns ok=false for an unusable name. Returns
// the merged lines in first-seen order (deterministic for the dedup key + the
// rendered line list); rejects an empty input and a bundle exceeding
// MaxSceneQuoteLines distinct kinds.
func resolveQuoteLines(w *World, lines []QuoteLineInput) ([]QuoteLine, error) {
	if len(lines) == 0 {
		return nil, errors.New("a quote must offer at least one item line.")
	}
	merged := make([]QuoteLine, 0, len(lines))
	index := make(map[ItemKind]int, len(lines))
	for _, in := range lines {
		if in.Qty < 1 {
			return nil, fmt.Errorf("each line's quantity must be at least 1 (got %d for %q).", in.Qty, in.ItemName)
		}
		if in.Qty > MaxSceneQuoteQty {
			return nil, fmt.Errorf("quantity exceeds maximum (got %d, max %d).", in.Qty, MaxSceneQuoteQty)
		}
		// LLM-167: a seller quoting "work"/"labor" as a good is reaching for the
		// labor market through the sell/quote tool. Steer to the labor verbs
		// BEFORE the discovery mint below — otherwise the token mints a phantom
		// inert kind into the catalog and dead-ends on the stock shortfall, with
		// no hint the labor flow exists.
		if isLaborToken(in.ItemName) {
			return nil, errors.New(laborTradeSteerMsg)
		}
		// LLM-290: a quote FOR coins is never meaningful — the quote's price is
		// already in coins (amount). Steer BEFORE the mint, so a coin token
		// can't (re-)mint the phantom 'coin' kind the earlier live occurrence
		// created.
		if IsCoinToken(in.ItemName) {
			return nil, errors.New(
				"coins aren't a good to quote — a quote's price is already in coins (amount). Name the good you're selling.",
			)
		}
		kind, ok := resolveOrMintItemKind(w, in.ItemName)
		if !ok {
			return nil, fmt.Errorf(
				"unknown item kind %q — check the items available in this world before quoting.",
				in.ItemName,
			)
		}
		if pos, seen := index[kind]; seen {
			sum, err := addChecked(merged[pos].Qty, in.Qty)
			if err != nil || sum > MaxSceneQuoteQty {
				return nil, fmt.Errorf("merged quantity for %s exceeds maximum — quote a smaller amount.", kind)
			}
			merged[pos].Qty = sum
			continue
		}
		if len(merged) >= MaxSceneQuoteLines {
			return nil, fmt.Errorf(
				"too many item kinds in one quote (max %d) — split into separate quotes.",
				MaxSceneQuoteLines,
			)
		}
		index[kind] = len(merged)
		merged = append(merged, QuoteLine{ItemKind: kind, Qty: in.Qty})
	}
	return merged, nil
}

// quoteLinesGrantLodging reports whether any line in a resolved bundle carries
// the "lodging" capability — the marker for a room-granting service kind
// (nights_stay). A lodging kind is a service and so can never be part of a
// multi-line bundle (rejected upstream), but the loop keeps the check total.
// Used by the LLM-208 seller-side homed-guest gate.
func quoteLinesGrantLodging(w *World, lines []QuoteLine) bool {
	for _, ln := range lines {
		if itemHasCapability(w, ln.ItemKind, "lodging") {
			return true
		}
	}
	return false
}

// quoteLinesEqual reports whether two bundle line sets are equal,
// order-independent — the model may list "blueberries, raspberries" or the
// reverse for the same bundle (LLM-101). Both inputs hold merged unique kinds,
// so a per-kind quantity comparison suffices. Used by the duplicate-key
// supersede check (the multi-line analogue of the old ItemKind+Qty scalar key).
func quoteLinesEqual(a, b []QuoteLine) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	counts := make(map[ItemKind]int, len(a))
	for _, ln := range a {
		counts[ln.ItemKind] += ln.Qty
	}
	for _, ln := range b {
		counts[ln.ItemKind] -= ln.Qty
		if counts[ln.ItemKind] == 0 {
			delete(counts, ln.ItemKind)
		}
	}
	return len(counts) == 0
}
