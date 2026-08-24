package perception

import (
	"fmt"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// lodging.go — ZBBS-HOME-296 PR2. The lodger-side "## Your lodging"
// perception section: tells an NPC who is renting a room which inn it's at
// and how close the grant is to expiring, so the LLM can renew with the
// keeper before it lapses. Pure over the published Snapshot — it reads
// ActorSnapshot.RoomAccess (carried on the snapshot for exactly this) and
// resolves the inn name via Snapshot.Structures.
//
// The grant the section describes IS the lodger relationship: an active
// ledger RoomAccess with a future ExpiresAt (see sim.IsActiveLedgerGrant,
// the canonical per-grant lodging predicate). This file also carries the
// keeper-side occupancy section and, near renewal, the affordability cue —
// the lever (HOME-296 §6) that makes a broke lodger perceive a rent shortfall
// in time to act, before the engine-auto rebook backstop fires at 6h.

// LodgingView is the content-gated "## Your lodging" section. nil means the
// actor holds no active lodging grant and render omits the section.
type LodgingView struct {
	// InnName is the display name of the structure the rented room is in
	// ("Hannah's Inn"), or a generic fallback when the structure has no name.
	InnName string

	// ExpiresAt is the soonest-expiring active ledger grant's expiry instant.
	// When an actor somehow holds more than one active lodging grant, the
	// nearest expiry is surfaced — that's the one the lodger must act on first.
	ExpiresAt time.Time

	// NightlyRate is the per-night rent the keeper advertises
	// (sim.LodgingNightlyRate of the world's weekly rate); 0 when the lodging
	// rate is unset/disabled, which suppresses both the rate hint and the
	// affordability cue.
	NightlyRate int

	// Coins is the lodger's coin balance at snapshot time — the affordability
	// cue compares it against NightlyRate to decide whether to warn of a
	// shortfall.
	Coins int

	// DeskRememberedShut is true when the lodger has a decaying experiential memory
	// (LLM-126, businessRememberedShut on the 4h closed-business TTL) of arriving at
	// its inn and finding the keeper's desk shut — no one tending it. Render flags it
	// (within the renewal window) so the lodger waits rather than walking to a desk
	// it just found unattended. Replaces the old omniscient KeeperAsleep snapshot
	// read: the lodger only "knows" the desk was shut if it was actually there.
	DeskRememberedShut bool

	// RenewalInFlight is true when the lodger already has a renewal moving to the
	// keeper of this inn — a still-pending pay_with_item offer, or an accepted
	// order awaiting hand-over (OrderStateReady). The grant only extends on
	// delivery, so in the accept→deliver window the expiry-driven renewal cue is
	// stale-by-construction; render replaces it with a wait-steer so the lodger
	// doesn't pay a second time. Clears once the order delivers. LLM-81.
	RenewalInFlight bool

	// RenewalDue is true when the stay is into its final night before checkout
	// (snapshot clock >= ExpiresAt - lodgingRenewalWindow). While false the lodger
	// is settled and the cue must not invite a re-buy — confirm the room and stop;
	// while true the lodger is steered to renew. Computed at build against
	// snap.PublishedAt (like TenureLabel) so the rendered cue is deterministic and
	// render needs no village TZ. LLM-96.
	RenewalDue bool

	// InConversation is true when the lodger shares its huddle with an awake peer
	// — a live conversation. Gate 1 (LLM-127): while renewal-due, render drops the
	// whole "## Your lodging" block so a rent cue never pulls the lodger out of an
	// exchange mid-conversation (the Ezekiel-walks-off-mid-chat incident). Derived
	// from anyHuddlePeerAwake over the huddle members at build.
	InConversation bool

	// RenewalPullDeferred is true when the lodger is renewal-due but on-shift away
	// from its inn. Gate 3 (LLM-127): the renewal steer is deferred rather than
	// pulling the lodger off its post — it lodges at the inn and renews co-located
	// when next there off-shift. False when off-shift OR already at the inn, where
	// the walk-pull is actionable now.
	RenewalPullDeferred bool
}

// buildLodgingView returns the lodging view for actorSnap, or nil when the
// actor holds no active ledger RoomAccess (i.e. isn't a lodger). Pure over
// the snapshot. The gate is sim.IsActiveLedgerGrant (active, ledger source,
// future ExpiresAt); it also selects the soonest-expiring grant so the
// rendered cue points at the most urgent renewal. ZBBS-HOME-296 PR2. members is
// the actor's huddle roster (build.go passes p.Surroundings.HuddleMembers) —
// the LLM-127 conversation gate reads it via anyHuddlePeerAwake.
func buildLodgingView(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, members []HuddleMember) *LodgingView {
	if snap == nil || actorSnap == nil {
		return nil
	}
	now := snap.PublishedAt

	var best *sim.RoomAccess
	for _, ra := range actorSnap.RoomAccess {
		if !sim.IsActiveLedgerGrant(ra, now) {
			continue
		}
		if best == nil || ra.ExpiresAt.Before(*best.ExpiresAt) {
			best = ra
		}
	}
	if best == nil {
		return nil
	}

	innName := "the inn"
	rememberedShut := false
	renewalInFlight := false
	atInn := false
	if s := structureForRoom(snap, best.RoomID); s != nil {
		innName = innLabel(s) // shared with the recovery-options inn finder
		keeper := keeperOf(snap, s.ID)
		// LLM-126: the experiential "found the desk shut" memory, keyed on the inn
		// structure — the same ObservedClosed entry the buy cues read. Replaces the
		// old omniscient keeper-asleep read so the lodger only knows the desk was
		// shut if it actually went there.
		rememberedShut = businessRememberedShut(snap, actorSnap, s.ID)
		// LLM-81: defer the renewal cue when a renewal is already moving to this keeper.
		renewalInFlight = lodgingRenewalInFlight(snap, actorID, keeper)
		atInn = actorSnap.InsideStructureID == s.ID
	}
	renewalDue := !now.Before(best.ExpiresAt.Add(-lodgingRenewalWindow(snap)))
	return &LodgingView{
		InnName:             innName,
		ExpiresAt:           *best.ExpiresAt,
		NightlyRate:         sim.LodgingNightlyRate(snap.LodgingDefaultWeeklyRate),
		Coins:               actorSnap.SpendableCoins(),
		DeskRememberedShut:  rememberedShut,
		RenewalInFlight:     renewalInFlight,
		RenewalDue:          renewalDue,
		InConversation:      anyHuddlePeerAwake(snap, members),
		RenewalPullDeferred: renewalDue && lodgerOnShiftAwayFromInn(actorSnap, snap, atInn),
	}
}

// lodgerOnShiftAwayFromInn reports whether a renewal-due lodger's renewal
// walk-pull should be deferred (gate 3, LLM-127): true only when the lodger is
// on its work shift AND not currently inside its inn. At the inn (atInn) the
// renewal is actionable on the spot, and off-shift the lodger is free to walk
// over — both keep the pull. An unscheduled lodger, or a snapshot with no local
// clock, counts as off-shift (no defer), matching sim.isActorOnShift's
// nil-schedule semantics. The shift test reuses minuteInWindow, the snapshot-pure
// shift-window check shared with the duty steer.
func lodgerOnShiftAwayFromInn(actorSnap *sim.ActorSnapshot, snap *sim.Snapshot, atInn bool) bool {
	if atInn {
		return false
	}
	if actorSnap.ScheduleStartMin == nil || actorSnap.ScheduleEndMin == nil || snap.LocalMinuteOfDay == nil {
		return false
	}
	return minuteInWindow(*actorSnap.ScheduleStartMin, *actorSnap.ScheduleEndMin, *snap.LocalMinuteOfDay)
}

// KeeperLodgingView is the content-gated "## Your inn" section shown to an
// actor who keeps a lodging structure. nil means the subject doesn't keep an
// inn and render omits the section.
type KeeperLodgingView struct {
	InnName        string
	RoomsAvailable int
	RoomsTotal     int

	// NightlyRate is the per-night rent the keeper quotes; surfaced only when
	// a room is free to sell. 0 when the lodging rate is unset/disabled.
	NightlyRate int
}

// buildKeeperLodgingView returns the keeper occupancy view when actorSnap
// works at a structure that has private bedrooms (an inn) AND has an awake
// huddle peer present to hear about it, or nil otherwise. RoomsAvailable =
// private rooms in the structure minus the distinct rooms currently held by an
// active ledger grant (any actor's). Pure over the snapshot. ZBBS-HOME-296 PR2.
//
// The awake-peer gate is LLM-22: the "## Your inn" line is a standing,
// every-tick sell cue, and with no awake listener it drove keepers to re-pitch
// rooms into an empty or all-asleep huddle (John Ellis pitching a sleeping
// Ezekiel a room six times). Gating the whole view here also keeps the
// downstream "## A room to let" offer cue correct: that cue only fires on an
// awake co-present seeker, which is a strict subset of "an awake peer is here,"
// so suppressing the shared view when no one is awake never starves it.
func buildKeeperLodgingView(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, members []HuddleMember) *KeeperLodgingView {
	if snap == nil || actorSnap == nil || actorSnap.WorkStructureID == "" {
		return nil
	}
	s := snap.Structures[actorSnap.WorkStructureID]
	if s == nil {
		return nil
	}

	privateRooms := make(map[sim.RoomID]bool)
	for _, r := range s.Rooms {
		if r != nil && r.Kind == sim.RoomKindPrivate {
			privateRooms[r.ID] = true
		}
	}
	total := len(privateRooms)
	if total == 0 {
		return nil // not a lodging structure — no keeper section
	}
	if !anyHuddlePeerAwake(snap, members) {
		return nil // no awake listener — don't cue a pitch into a dead room (LLM-22)
	}

	now := snap.PublishedAt
	occupied := make(map[sim.RoomID]bool)
	for _, other := range snap.Actors {
		if other == nil {
			continue
		}
		for _, ra := range other.RoomAccess {
			if sim.IsActiveLedgerGrant(ra, now) && privateRooms[ra.RoomID] {
				occupied[ra.RoomID] = true
			}
		}
	}

	available := total - len(occupied)
	if available < 0 {
		available = 0
	}
	return &KeeperLodgingView{
		InnName:        innLabel(s),
		RoomsAvailable: available,
		RoomsTotal:     total,
		NightlyRate:    sim.LodgingNightlyRate(snap.LodgingDefaultWeeklyRate),
	}
}

// LodgingOfferView is the seller-side "offer a room" cue (ZBBS-WORK-382). When
// a lodging keeper (works at a structure with free private bedrooms at a
// configured nightly rate) shares a huddle with a structural lodging-seeker —
// an actor with no home AND no active lodging grant, i.e. someone with nowhere
// to sleep — this surfaces that seeker by name alongside the scene_quote call
// for a nights_stay, so the keeper LLM proactively offers the room instead of
// narrating a stock greeting. It is the Finding-1 legibility idiom applied to
// lodging: scene_quote is the only seller path for a room, but the weak model
// never connects "asked about a room" to it (and scene_quote's own schema
// frames qty as per-consumer goods, which misleads for a nights booking). The
// decision stays with the model and the buyer keeps full accept/decline agency
// via pay_with_item. nil/empty skips the section.
type LodgingOfferView struct {
	// SeekerNames are the acquaintance-gated labels (descriptorLabel) of the
	// co-present lodging-seekers the keeper may offer a room to.
	SeekerNames []string

	// InnName is the keeper's inn display name (from KeeperLodgingView).
	InnName string

	// RoomsAvailable is the keeper's current vacancy (from KeeperLodgingView);
	// the cue only builds when this is > 0.
	RoomsAvailable int

	// NightlyRate is the per-night rent the keeper quotes; the cue only builds
	// when this is > 0 (a 0 rate means lodging pricing is disabled).
	NightlyRate int
}

// buildLodgingOfferCue builds the seller-side "offer a room" cue. It reuses the
// already-computed KeeperLodgingView (so the subject is confirmed a keeper with
// vacancy + a live nightly rate) and scans the keeper's co-present huddle for
// structural lodging-seekers — actors with no home and no active lodging grant.
// Mirrors buildOfferableCustomers' two storm guards so a stuck cue can't drive
// a re-offer loop: a seeker already mid-pay-offer with this keeper, or one the
// keeper already has a live quote out to, is dropped. Returns nil (Render
// content-gates) when the subject isn't a keeper-with-vacancy or no seeker is
// co-present. Pure over the snapshot. ZBBS-WORK-382.
func buildLodgingOfferCue(snap *sim.Snapshot, subject sim.ActorID, keeper *KeeperLodgingView, members []HuddleMember) *LodgingOfferView {
	if snap == nil || keeper == nil || keeper.RoomsAvailable <= 0 || keeper.NightlyRate <= 0 {
		return nil
	}
	now := snap.PublishedAt
	// members is already sorted by ID (buildSurroundings), so names is deterministic.
	var seekers []string
	for _, m := range members {
		if m.ID == subject {
			continue // never offer a room to yourself
		}
		if !actorSnapIsLodgingSeeker(snap.Actors[m.ID], now) {
			continue
		}
		if customerHasPendingOfferWithSeller(snap, m.ID, subject) {
			continue // already booking — renderPayOffers cues accept/decline/counter
		}
		if sellerHasActiveQuoteToBuyer(snap, subject, m.ID) {
			continue // already offered, awaiting the buyer — don't re-post every tick
		}
		seekers = append(seekers, descriptorLabel(m.DisplayName, m.Role, m.Acquainted))
	}
	if len(seekers) == 0 {
		return nil
	}
	return &LodgingOfferView{
		SeekerNames:    seekers,
		InnName:        keeper.InnName,
		RoomsAvailable: keeper.RoomsAvailable,
		NightlyRate:    keeper.NightlyRate,
	}
}

// actorSnapIsLodgingSeeker reports whether a snapshot actor has nowhere of its
// own to sleep — no HomeStructureID AND no active ledger RoomAccess. Such an
// actor beds nowhere via the auto-sleep machine (the home arm needs a home, the
// lodger arm needs a grant), so it is a candidate to be offered a room. Nil-safe
// (a missing snapshot is not a seeker). ZBBS-WORK-382.
func actorSnapIsLodgingSeeker(as *sim.ActorSnapshot, now time.Time) bool {
	if as == nil || as.HomeStructureID != "" {
		return false
	}
	for _, ra := range as.RoomAccess {
		if sim.IsActiveLedgerGrant(ra, now) {
			return false
		}
	}
	return true
}

// anyHuddlePeerAwake reports whether at least one of the subject's huddle peers
// is awake at snapshot time. members already excludes the subject
// (buildSurroundings), so any awake member is a co-present listener. Sleepers
// stay in the huddle roster (membership doesn't drop on bed-down), so the
// per-member State check is what separates an empty/all-asleep room from a live
// audience. StateSleeping is the snapshot-side asleep proxy; a resting/on-break
// peer still counts as a listener. LLM-22.
func anyHuddlePeerAwake(snap *sim.Snapshot, members []HuddleMember) bool {
	if snap == nil {
		return false
	}
	for _, m := range members {
		if a := snap.Actors[m.ID]; a != nil && a.State != sim.StateSleeping {
			return true
		}
	}
	return false
}

// structureForRoom returns the structure that contains roomID, or nil when
// no structure declares it. Linear over the snapshot's structures/rooms —
// fine at Salem's scale (mirrors sim.findRoom, which works on the live world).
// RoomID is a globally unique per-instance id (legacy BIGSERIAL — see
// sim.RoomID), so at most one structure declares a given room and the
// first match is unambiguous despite the map-iteration order.
func structureForRoom(snap *sim.Snapshot, roomID sim.RoomID) *sim.Structure {
	for _, s := range snap.Structures {
		if s == nil {
			continue
		}
		for _, r := range s.Rooms {
			if r != nil && r.ID == roomID {
				return s
			}
		}
	}
	return nil
}

// lodgingStatusLine renders the "## Your lodging" headline in one of three states.
// renewalDue is false while the lodger is settled — it holds a paid room and is
// not yet into its final night, so the line confirms the room and explicitly
// does NOT invite paying for another (the LLM-96 double-buy was a settled lodger
// re-told to "renew" the room it had just bought). renewalDue flips true once the
// stay is into its final night before checkout (now >= RenewalDueAt), where the
// line steers the lodger to renew. pullDeferred (gate 3, LLM-127) softens that
// renewal-due steer to "renew when next at the inn" when the lodger is on-shift
// away from its post, so the cue doesn't walk it off duty. The caller decides
// renewalDue/pullDeferred; this stays a pure formatter. Pre-LLM-96 this tiered by
// raw time-to-expiry (24h/48h), which fired the renewal cue on a one-night stay
// from purchase since such a stay expires inside 24h.
func lodgingStatusLine(innName string, renewalDue, pullDeferred bool) string {
	inn := sanitizeInline(innName)
	if !renewalDue {
		return fmt.Sprintf("Your room at %s is paid — you are settled here for now, no need to arrange another.", inn)
	}
	if pullDeferred {
		return fmt.Sprintf("Your room at %s is nearly up — see the keeper to renew when you are next back at the inn.", inn)
	}
	return fmt.Sprintf("Your room at %s is nearly up — if you wish to stay on, see the keeper to renew.", inn)
}

// lodgingAffordabilityCue returns the rent-shortfall warning, or "" when it
// shouldn't fire. The lever of HOME-296 §6, retargeted by LLM-96: it fires only
// once the stay is renewal-due (into its final night before checkout) and the
// lodger can't cover a night (Coins < NightlyRate). Gating on RenewalDue rather
// than a flat 48h window stops it nagging a settled lodger that it is "short for
// another night" the moment it pays — the same premature framing that drove the
// double-buy. Suppressed entirely when the rate is disabled. Pure.
func lodgingAffordabilityCue(v *LodgingView) string {
	if v.NightlyRate <= 0 || !v.RenewalDue {
		return ""
	}
	if v.Coins >= v.NightlyRate {
		return ""
	}
	// LLM-136: don't steer a coin-short lodger only toward earning coins — the
	// room can be paid in goods. Name the direct barter (offer_trade) alongside
	// the earn-coins fallback so a producer can renew with its wares. Phrased
	// conditionally ("if you have wares") — LodgingView carries no inventory, so
	// an empty-handed lodger mustn't be told to offer goods it doesn't have.
	return fmt.Sprintf("You have only %d coins — short of the %d for another night. If you have wares to spare, offer them for the room directly with offer_trade, putting your words in that call's say; otherwise earn coins before your room lapses.",
		v.Coins, v.NightlyRate)
}

// renderLodging writes the "## Your lodging" section. Content-gated: a nil
// view writes nothing. The renewal tier and affordability cue are computed
// against time.Now() — the same wall-clock posture renderPendingDeliveriesToMe
// uses for order expiry (Render has no snapshot, so it can't read
// Snapshot.PublishedAt here).
func renderLodging(b *strings.Builder, v *LodgingView) {
	if v == nil {
		return
	}
	// Gate 1 (LLM-127): renewal-due but mid-conversation (an awake huddle peer).
	// Drop the whole "## Your lodging" block — a live social beat must not carry
	// rent math or a "see the keeper" pull that walks the lodger out of the exchange.
	// Scoped to renewal-due: the settled confirmation line below is harmless
	// background, so a settled lodger still gets it mid-conversation.
	if v.RenewalDue && v.InConversation {
		return
	}
	b.WriteString("## Your lodging\n")
	// LLM-81: a renewal is already in flight to the keeper (a pending offer, or an
	// accepted order awaiting hand-over). The grant only extends on delivery, so the
	// renewal line below is stale-by-construction in this window and fights the
	// in-flight order — drop it, the rate hint, and the shortfall cue for a positive
	// wait-steer, so the lodger bides instead of paying twice. Clears once the order
	// delivers and the grant extends. Mirrors LLM-64.
	if v.RenewalInFlight {
		fmt.Fprintf(b, "Your renewal at %s is paid and with the keeper — they will bring you the room shortly. Do not pay for it again; wait here for them.\n\n", sanitizeInline(v.InnName))
		return
	}
	b.WriteString(lodgingStatusLine(v.InnName, v.RenewalDue, v.RenewalPullDeferred))
	// Settled (not yet the final night): confirm the room and stop — the rate
	// hint, asleep-keeper note, and shortfall cue all push toward renewing, which
	// a settled lodger must not be nudged to do (LLM-96).
	if !v.RenewalDue {
		b.WriteString("\n\n")
		return
	}
	if v.NightlyRate > 0 {
		fmt.Fprintf(b, " Renewing is %d coins a night.", v.NightlyRate)
	}
	// The lodger went to the inn not long ago and found the keeper's desk shut —
	// flag it so it waits rather than walking back to an unattended desk (LLM-126,
	// decaying on the 4h closed-business TTL). Replaces the old omniscient
	// KeeperAsleep read: the lodger only knows the desk was shut if it was actually
	// there. Skipped when the pull is already deferred (gate 3, LLM-127): the line
	// then steers "renew when next at the inn", so the desk-shut note is redundant.
	if v.DeskRememberedShut && !v.RenewalPullDeferred {
		b.WriteString(" You stopped by not long ago and found the keeper's desk shut — best wait until they are tending it before going to renew.")
	}
	b.WriteString("\n")
	if cue := lodgingAffordabilityCue(v); cue != "" {
		b.WriteString(cue)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// lodgingRenewalInFlight reports whether `lodger` already has a room renewal at
// this inn moving — either a still-pending pay_with_item offer to `keeper` for a
// lodging-capability item, or an accepted-but-undelivered order (OrderStateReady)
// from `keeper` for one. The lodging grant only extends on delivery, so between
// accept and deliver the expiry-driven "## Your lodging" cue is stale-by-
// construction; this is the signal renderLodging uses to defer that cue rather
// than let a weak model pay twice. Mirrors the LLM-64 buy-cue deferral, but spans
// the whole accept→deliver window (not just the Pending state, since the duplicate
// fires after accept). Pure over the snapshot. LLM-81.
func lodgingRenewalInFlight(snap *sim.Snapshot, lodger, keeper sim.ActorID) bool {
	if snap == nil || lodger == "" || keeper == "" {
		return false
	}
	now := snap.PublishedAt
	for _, e := range snap.PayLedger {
		if e == nil || e.State != sim.PayLedgerStatePending {
			continue
		}
		if e.BuyerID != lodger || e.SellerID != keeper {
			continue
		}
		// Skip an expired-but-unswept offer (mirrors hasPendingOfferTo).
		if !e.ExpiresAt.IsZero() && !now.Before(e.ExpiresAt) {
			continue
		}
		if itemGrantsLodging(snap, e.ItemKind) {
			return true
		}
	}
	for _, o := range snap.Orders {
		if o == nil || o.State != sim.OrderStateReady || o.SellerID != keeper {
			continue
		}
		consumes := o.BuyerID == lodger
		for _, cid := range o.ConsumerIDs {
			if cid == lodger {
				consumes = true
				break
			}
		}
		if consumes && itemGrantsLodging(snap, o.Item) {
			return true
		}
	}
	return false
}

// itemGrantsLodging reports whether an item kind carries the "lodging" capability
// in the world catalog — the marker for a room-granting service (item_kind.go).
func itemGrantsLodging(snap *sim.Snapshot, kind sim.ItemKind) bool {
	def := snap.ItemKinds[kind]
	return def != nil && def.HasCapability("lodging")
}

// renderKeeperLodging writes the "## Your inn" section for an inn-keeper.
// Content-gated: a nil view writes nothing. The nightly rate is appended only
// when a room is free to sell and the rate is set.
//
// When there's a room to sell and a rate to price it, the passive vacancy line
// is followed by the how-to-let-a-room mechanic. The two act-now offer cues —
// renderLodgingOffer ("## A room to let") and renderKeeperHeldLodgers ("##
// Already lodging here") — only name the nights_stay sell call for a homeless
// seeker or an existing resident respectively. A guest who asks for a room but
// is neither (e.g. a traveler already lodging at ANOTHER inn — so not a
// "seeker", and not held HERE) otherwise leaves the keeper with no cue for the
// nights_stay item kind, so the weak model invents "room" and the sale fails
// (item discovery mints a 0-stock "room", insufficient-stock rejects). This
// states the mechanic generally so any room request can be filled. Conditional
// ("if a guest asks") so it's a mechanic, not an unsolicited pitch; the args
// mirror renderLodgingOffer (qty is NIGHTS, consume_now false, single guest so
// no consumers).
func renderKeeperLodging(b *strings.Builder, v *KeeperLodgingView) {
	if v == nil {
		return
	}
	b.WriteString("## Your inn\n")
	fmt.Fprintf(b, "%d of %d rooms available tonight at %s",
		v.RoomsAvailable, v.RoomsTotal, sanitizeInline(v.InnName))
	if v.RoomsAvailable > 0 && v.NightlyRate > 0 {
		fmt.Fprintf(b, ", %d coins a night", v.NightlyRate)
	}
	b.WriteString(".\n")
	if v.RoomsAvailable > 0 && v.NightlyRate > 0 {
		fmt.Fprintf(b, "If a guest asks to lodge, call sell with item \"nights_stay\", consume_now false, qty set to the number of nights, and amount set to nights × %d coins (your nightly rate). A room is for the one guest, so leave consumers empty; use target_buyer only if you know the guest's name.\n", v.NightlyRate)
	}
	b.WriteString("\n")
}

// renderLodgingOffer writes the seller-side "offer a room" cue (ZBBS-WORK-382):
// a co-present lodging-seeker by name plus the scene_quote call for a
// nights_stay with its lodging-specific args spelled out — qty is the number of
// NIGHTS (not rooms or guests), consume_now is false (a booking, not eat-here),
// and the room is for the single guest (omit consumers). scene_quote's own
// schema frames qty as "per consumer" goods, so without this spelled out the
// weak keeper model misreads a nights booking. Phrased conditionally ("if they
// want a room") so it respects the vendor block's "don't pitch unless they show
// interest" rule. Content-gated: a nil/empty view skips the section.
func renderLodgingOffer(b *strings.Builder, v *LodgingOfferView) {
	if v == nil || len(v.SeekerNames) == 0 {
		return
	}
	b.WriteString("## A room to let\n")
	who := joinNames(v.SeekerNames) // sanitizes each name inline
	beVerb, haveVerb := "is", "has"
	if len(v.SeekerNames) > 1 {
		beVerb, haveVerb = "are", "have"
	}
	// LLM-136: the coin offer (sell → quote) is the default, but a guest may be
	// coinless (a homeless producer pays in its wares). Naming the goods path
	// here keeps the keeper from dead-ending such a guest on coins it doesn't
	// have — the offer travels the existing barter flow (offer_trade → the
	// keeper's standing accept_pay/counter_pay decision on the pending offer).
	fmt.Fprintf(b, "%s %s here with you and %s nowhere to stay. If they want a room, offer it — call sell with item \"nights_stay\", consume_now false, qty set to the number of nights, and amount set to nights × %d coins (your nightly rate). A room is for the one guest, so leave consumers empty; use target_buyer only if you know the guest's name, otherwise leave it empty and anyone here may take the offer. They are then free to take it or leave it. If a guest has no coins, you needn't turn them away — tell them they may offer goods for the room, then accept_pay their offer_trade (or counter_pay to adjust the terms).\n", who, beVerb, haveVerb, v.NightlyRate)
	roomWord := "rooms"
	if v.RoomsAvailable == 1 {
		roomWord = "room"
	}
	fmt.Fprintf(b, "You have %d %s free at %s.\n\n", v.RoomsAvailable, roomWord, sanitizeInline(v.InnName))
}

// lodgingRenewalWindow is the lead time before a grant's checkout instant at
// which the stay becomes "renewal-due" — the span from the lodger bedtime on the
// final night to checkout the next morning, derived from the village bedtime and
// checkout hour (both minute-of-day on the snapshot). The lodger's "## Your
// lodging" cue and the keeper's "## Already lodging here" cue both subtract it
// from ExpiresAt, so the two sides agree on when it is renewal time (LLM-46).
//
// LLM-96: this replaces a flat 48h constant. A nights_stay expires at checkout
// the morning after the last paid night (ComputeLodgerUntil = readyBy + qty days
// at checkout hour), so a one-night stay's whole life is under 24h — the old 48h
// window was wider than the stay and flagged it renewal-due from the instant it
// was bought, driving an immediate second booking on both sides. Anchoring the
// window to bedtime-of-final-night keeps a freshly-bought room settled until its
// last night. Falls back to 48h when the snapshot has no usable clock (a
// hand-built snapshot leaves the minutes at 0).
func lodgingRenewalWindow(snap *sim.Snapshot) time.Duration {
	if snap == nil {
		return 48 * time.Hour
	}
	// bedtime on day D-1 → checkout on day D, in minutes:
	// (midnight - bedtime) carries to the next day, then + checkout hour.
	mins := (1440 - snap.LodgingBedtimeMinute) + snap.LodgingCheckOutMinute
	if mins <= 0 || mins >= 1440 {
		return 48 * time.Hour
	}
	return time.Duration(mins) * time.Minute
}

// KeeperHeldLodgersView is the keeper-side "this guest already lodges here"
// signal (LLM-38). The offer cue (buildLodgingOfferCue) is correctly suppressed
// for an actor who already holds a grant — actorSnapIsLodgingSeeker returns
// false — but suppression alone leaves the keeper free-forming an offer off the
// passive "## Your inn" vacancy line ("1 of 4 rooms available…"), so the keeper
// re-offers a room the guest is already in. This is the positive counter-signal:
// it names each co-present actor holding an active ledger grant at THIS inn so
// the keeper LLM affirms ("you're already settled") instead of re-pitching.
//
// Once a held grant is into its final night before checkout (within
// lodgingRenewalWindow of expiry) the cue flips for
// that guest (LLM-46): the stay is ending, so the keeper is steered to OFFER a
// renewal (posting a nights_stay quote) rather than wave it off as settled —
// closing the gap where a renewing NPC lodger could not get the keeper to act.
// nil/empty skips the section. Pure over the snapshot.
type KeeperHeldLodgersView struct {
	// Lodgers are the co-present actors already holding a room at this inn.
	Lodgers []HeldLodger

	// NightlyRate is the keeper's per-night rent, surfaced so a renewal-due
	// guest's cue can spell out the sell amount. 0 when the lodging rate
	// is unset/disabled — no renewal is offered in that case.
	NightlyRate int
}

// HeldLodger is one co-present actor already lodging at the keeper's inn.
type HeldLodger struct {
	// Name is the acquaintance-gated label (descriptorLabel).
	Name string

	// TenureLabel voices the time left on the stay ("paid for about 2 more
	// nights"). Computed at build time against snap.PublishedAt so the rendered
	// cue is deterministic against the snapshot rather than a render-time clock.
	TenureLabel string

	// RenewalDue is true when this guest's grant is into its final night before
	// checkout (within lodgingRenewalWindow of expiry): the stay is ending, so the
	// keeper should renew it rather than wave it off as settled (LLM-46). Requires
	// a live nightly rate — a renewal can't be priced without one.
	RenewalDue bool

	// OfferRenewal is true when the keeper should POST the renewal quote this
	// turn: RenewalDue and no renewal already in flight (no standing quote to
	// this guest, no pending pay from them). Once a quote stands it goes false
	// and the cue switches to "await their answer" so the keeper doesn't re-post.
	OfferRenewal bool
}

// buildKeeperHeldLodgers builds the keeper-side held-lodger signal (LLM-38). It
// reuses the already-computed KeeperLodgingView (so the subject is confirmed a
// keeper of a lodging structure) and scans the keeper's co-present huddle for
// actors holding an active ledger grant at a private room IN the keeper's own
// structure — the guests the keeper must NOT re-offer a room to. Unlike the
// offer cue this is informational, not an act-now instruction, so it is NOT
// location-gated (it mirrors the ungated "## Your inn" status): a keeper who
// runs into their lodger should affirm rather than re-pitch wherever they meet.
// Per-lodger it also computes the renewal-due flip (LLM-46): a grant into its
// final night before checkout turns the "already settled" affirm into a renewal
// offer, gated by the same two storm guards the offer cue uses so a standing quote or
// in-flight pay suppresses a re-post. Returns nil (Render content-gates) when
// the subject isn't a keeper or no held lodger is co-present. Pure over the
// snapshot.
func buildKeeperHeldLodgers(snap *sim.Snapshot, subject sim.ActorID, keeper *KeeperLodgingView, members []HuddleMember) *KeeperHeldLodgersView {
	if snap == nil || keeper == nil {
		return nil
	}
	subj := snap.Actors[subject]
	if subj == nil || subj.WorkStructureID == "" {
		return nil
	}
	s := snap.Structures[subj.WorkStructureID]
	if s == nil {
		return nil
	}
	// The keeper's own private rooms — the held-grant rooms that count as
	// "lodging here". Same room set buildKeeperLodgingView scans for occupancy.
	privateRooms := make(map[sim.RoomID]bool)
	for _, r := range s.Rooms {
		if r != nil && r.Kind == sim.RoomKindPrivate {
			privateRooms[r.ID] = true
		}
	}
	if len(privateRooms) == 0 {
		return nil
	}

	now := snap.PublishedAt
	var held []HeldLodger
	for _, m := range members {
		if m.ID == subject {
			continue
		}
		as := snap.Actors[m.ID]
		if as == nil {
			continue
		}
		// The soonest-expiring active grant this peer holds at THIS inn (a guest
		// could in principle hold more than one; the nearest expiry is the one
		// that speaks to "how long are they settled").
		var best *sim.RoomAccess
		for _, ra := range as.RoomAccess {
			if !sim.IsActiveLedgerGrant(ra, now) || !privateRooms[ra.RoomID] {
				continue
			}
			if best == nil || ra.ExpiresAt.Before(*best.ExpiresAt) {
				best = ra
			}
		}
		if best == nil {
			continue
		}
		// Renewal-due flip (LLM-46): the stay is into its final night before
		// checkout (now >= ExpiresAt - lodgingRenewalWindow). Before that the guest
		// is settled and the keeper must NOT offer another room — the LLM-96 fix, so
		// the keeper doesn't re-sell a guest the room it just bought. OfferRenewal
		// also requires no renewal already in flight — a pending pay from the guest
		// (the accept/decline path owns it) or a standing quote already out to them
		// (await the answer; reuses the offer cue's two storm guards so the keeper
		// can't re-post every tick).
		renewalDue := keeper.NightlyRate > 0 && !now.Before(best.ExpiresAt.Add(-lodgingRenewalWindow(snap)))
		offerRenewal := renewalDue &&
			!customerHasPendingOfferWithSeller(snap, m.ID, subject) &&
			!sellerHasActiveQuoteToBuyer(snap, subject, m.ID)
		held = append(held, HeldLodger{
			Name:         descriptorLabel(m.DisplayName, m.Role, m.Acquainted),
			TenureLabel:  heldLodgerTenure(*best.ExpiresAt, now),
			RenewalDue:   renewalDue,
			OfferRenewal: offerRenewal,
		})
	}
	if len(held) == 0 {
		return nil
	}
	return &KeeperHeldLodgersView{Lodgers: held, NightlyRate: keeper.NightlyRate}
}

// renderKeeperHeldLodgers writes the "## Already lodging here" section: each
// co-present guest who already holds a room at the keeper's inn and the time
// left on their stay. A settled guest gets a "do not offer another" steer so the
// keeper affirms instead of re-pitching off the "## Your inn" vacancy line; a
// guest whose stay is ending (RenewalDue) gets a renewal steer instead — the
// spelled-out sell call when the keeper should post it (OfferRenewal),
// otherwise an "await their answer" line while a renewal is in flight (LLM-46).
// Content-gated: a nil/empty view writes nothing. The tenure phrase is
// precomputed at build time (TenureLabel) so this stays deterministic against
// the snapshot.
func renderKeeperHeldLodgers(b *strings.Builder, v *KeeperHeldLodgersView) {
	if v == nil || len(v.Lodgers) == 0 {
		return
	}
	b.WriteString("## Already lodging here\n")
	for _, l := range v.Lodgers {
		name := sanitizeInline(l.Name)
		switch {
		case l.OfferRenewal:
			// The renewal rides the same sell → take path as a fresh room
			// (renderLodgingOffer), with the args spelled out — qty is the number
			// of NIGHTS, not goods. AssignBedroomForLodger extends the existing
			// grant on take, so no vacancy is needed.
			fmt.Fprintf(b, "%s already holds a room here, %s — their stay is ending. If they wish to stay on, offer a renewal: call sell with item \"nights_stay\", consume_now false, qty set to the number of nights, amount set to nights × %d coins (your nightly rate), and target_buyer %s. They are then free to take it or leave it.\n",
				name, l.TenureLabel, v.NightlyRate, name)
		case l.RenewalDue:
			fmt.Fprintf(b, "%s already holds a room here, %s — their stay is ending and you have offered them a renewal. Await their answer rather than offering again.\n",
				name, l.TenureLabel)
		default:
			fmt.Fprintf(b, "%s already holds a room here, %s. Do not offer another — if they ask about lodging, tell them they are already settled.\n",
				name, l.TenureLabel)
		}
	}
	b.WriteString("\n")
}

// heldLodgerTenure phrases the time left on a held grant for the keeper cue.
// Three tiers mirroring lodgingStatusLine, voiced from the keeper's side. A
// just-expired grant (clock drift past the snapshot gate) falls into the first
// tier — "paid through the day" — which is harmless.
func heldLodgerTenure(expiresAt, now time.Time) string {
	d := expiresAt.Sub(now)
	switch {
	case d <= 24*time.Hour:
		return "paid through the day"
	case d <= 48*time.Hour:
		return "paid through tomorrow"
	default:
		nights := int(d / (24 * time.Hour))
		return fmt.Sprintf("paid for about %d more nights", nights)
	}
}
