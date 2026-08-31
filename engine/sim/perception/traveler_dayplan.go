package perception

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// traveler_dayplan.go — the salem-visitor traveler's day-plan cues (LLM-373).
// The engine walks the traveler its circuit and, of an evening, to the tavern
// (engine/sim/visitor.go dispatchVisitorCircuit); these render surfaces give the
// shared salem-visitor VA the SITUATION at each stage so it trades and books with
// purpose rather than idling. Two turn-fresh sections here:
//
//   - "## On your rounds"     — the daytime business circuit: frame the visit as a
//                               peddler's round so the VA greets + trades + passes
//                               news instead of standing mute.
//   - "## A bed for the night" — the evening booking: a homeless traveler at the inn
//                               is told to buy a night's lodging with pay_with_item,
//                               the same buyer-initiated flow a PC uses.
//
// The evening-leisure "the tavern's open of an evening" cue is ALSO extended to the
// traveler (buildVisitorEveningLeisure below, dispatched from buildEveningLeisure) —
// the social-hours pull that draws it to the tavern with the rest of the village.

// TravelerRoundsView is the errand-anchored "## Your rounds" surface (LLM-455, generalizing
// LLM-379). For a MERCHANT it frames the one real trade he came to do at his errand
// counterparty (the must-hit stop) and casts every other open shop as a talk-only social
// call — show his face, pass his news — so commerce stays confined to his errand (enforced
// structurally by the talk-only tool gate + TradeErrandSteer; this is the legible framing).
// For a PASSER-THROUGH it frames the same social circuit with no trade at all. The engine
// renders the situation; the model navigates with move_to. Content-gated: a nil view (off
// his daytime rounds) writes nothing.
type TravelerRoundsView struct {
	// Errand is the merchant's one bound trade (LLM-455); nil for a passer-through, who
	// carries no commerce and makes a pure social circuit.
	Errand *RoundsErrand
	// Visited is the display names of the keeper-businesses he has already called at this
	// stay — rendered back so a stateless shared VA "remembers" and does not repeat a shop.
	Visited []string
	// OpenShops is the keeper-businesses still tending (snapshotKeeperPresent, the twin of
	// the arrival-recording gate so the list can't outrun what a visit records), unvisited,
	// not the inn, not his errand counterparty (rendered as the must-hit stop), not the one
	// he stands in — each a talk-only social call with a bearing. Nearest first. NEVER a
	// single "go here" imperative: the list, and he picks with move_to.
	OpenShops []RoundsShop
	// MinutesToDusk drives the escalating nightfall pressure; only meaningful when HasClock.
	// HasClock is false on an unusable dawn/dusk clock (suppress the line).
	MinutesToDusk int
	HasClock      bool
}

// RoundsErrand is the merchant's bound trade as the rounds cue needs it (LLM-455) — his one
// piece of real business, the anchor of his day.
type RoundsErrand struct {
	// Buy — true when he buys GoodKind from the counterparty (injecting coin); false for a
	// seller (the factor), who lays out his imported bale and the keeper buys it.
	Buy bool
	// GoodKind is the EXACT catalog kind for the pay_with_item item argument ("cheese");
	// GoodLabel is its display noun for the prose ("fresh cheese" -> here just the singular).
	GoodKind  string
	GoodLabel string
	// PackGoodNoun is the errand good as he now CARRIES it — the count-aware noun over
	// the quantity in his pack ("nails" for nine, "nail" for one), with PackGoodPlural
	// carrying the verb agreement for the settled lead (LLM-574). Set only on a settled
	// buy errand; every other branch speaks of the good in the abstract and uses
	// GoodLabel. Nine nails announced as "the nail is bought" understates the pack
	// against the inventory line two sections above it.
	PackGoodNoun   string
	PackGoodPlural bool
	// KeeperName is the counterparty keeper's display name — the pay_with_item seller arg
	// when co-present; "" when the keeper is not in the huddle.
	KeeperName string
	// ShopLabel is the counterparty structure's display name for the prose.
	ShopLabel string
	// AtShop is true when he stands co-present with the counterparty keeper — the trade-now
	// moment (the only place his commerce tools are cued).
	AtShop bool
	// Direction / Steps / HasBearing point him at the counterparty when he is not there yet.
	Direction  string
	Steps      int
	HasBearing bool
	// Settled — the errand trade is done (or proven impossible for the day); the cue turns to
	// winding him down to the tavern instead of pressing his rounds.
	Settled bool
}

// RoundsShop is one still-open shop on the traveler's rounds: its name and a bearing
// from where he stands.
type RoundsShop struct {
	Name      string
	Direction string // "north" … "" when he is on top of it
	Steps     int    // Chebyshev tiles, for a rough near/far sense
}

// buildTravelerRounds returns the rounds surface when the subject is a traveler on his
// daytime rounds (arriving / making_rounds / a legacy 'present' row); nil in the evening
// (the seek-a-bed cue owns it) or off-visitor. For a merchant it anchors on his bound errand
// (LLM-455); for a passer-through it frames a pure social circuit. Pure over the snapshot.
func buildTravelerRounds(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, members []HuddleMember) *TravelerRoundsView {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState == nil {
		return nil
	}
	switch actorSnap.VisitorState.Phase {
	case sim.VisitorPhaseArriving, sim.VisitorPhaseMakingRounds, sim.VisitorPhasePresent:
		// on his daytime rounds
	default:
		return nil
	}
	vs := actorSnap.VisitorState
	view := &TravelerRoundsView{}

	// Merchant errand (LLM-455): the one trade + its counterparty. Excludes the counterparty
	// from the talk-only OpenShops list (it is rendered as the must-hit stop instead).
	var counterparty sim.StructureID
	if vs.Trade != nil {
		counterparty = vs.Trade.Counterparty
		e := &RoundsErrand{
			Buy:      vs.Trade.Direction == sim.TradeDirectionBuy,
			GoodKind: string(vs.Trade.Good),
			Settled:  vs.Trade.Settled,
		}
		e.GoodLabel = e.GoodKind
		if def := snap.ItemKinds[vs.Trade.Good]; def != nil {
			e.GoodLabel = def.Singular()
		}
		if st := snap.Structures[counterparty]; st != nil && st.DisplayName != "" {
			e.ShopLabel = st.DisplayName
		} else {
			e.ShopLabel = string(counterparty)
		}
		for _, m := range members {
			ks := snap.Actors[m.ID]
			if ks != nil && ks.BusinessownerState != nil && ks.WorkStructureID == counterparty {
				e.AtShop = true
				e.KeeperName = m.DisplayName
				break
			}
		}
		if !e.AtShop {
			if vobj := snap.VillageObjects[sim.VillageObjectID(counterparty)]; vobj != nil {
				tile := vobj.Pos.Tile()
				e.Direction = cardinalDirection(float64(actorSnap.Pos.X), float64(actorSnap.Pos.Y), float64(tile.X), float64(tile.Y))
				e.Steps = actorSnap.Pos.Chebyshev(tile)
				e.HasBearing = e.Direction != ""
			}
		}
		if e.Buy && e.Settled {
			if qty := actorSnap.Inventory[vs.Trade.Good]; qty > 0 {
				if def := snap.ItemKinds[vs.Trade.Good]; def != nil {
					e.PackGoodNoun = def.CountNoun(qty)
				}
				e.PackGoodPlural = qty > 1
			}
		}
		view.Errand = e
	}

	// The shop he stands in (a talk-only call excluded from OpenShops below).
	var atShopID sim.StructureID
	if sid := actorSnap.InsideStructureID; sid != "" {
		for _, m := range members {
			ks := snap.Actors[m.ID]
			if ks != nil && ks.BusinessownerState != nil && ks.WorkStructureID == sid {
				atShopID = sid
				break
			}
		}
	}

	// Rounds so far.
	visitedSet := make(map[sim.StructureID]bool, len(vs.VisitedBusinesses))
	for _, sid := range vs.VisitedBusinesses {
		visitedSet[sid] = true
		if st := snap.Structures[sid]; st != nil {
			view.Visited = append(view.Visited, st.DisplayName)
		}
	}

	// Open shops as talk-only social calls: a business with its keeper tending now (the
	// twin of the recording gate), unvisited, not the inn, not his errand counterparty,
	// not where he stands.
	for id, vobj := range snap.VillageObjects {
		stID := sim.StructureID(id)
		if vobj == nil || stID == atShopID || stID == counterparty || visitedSet[stID] {
			continue
		}
		st, ok := snap.Structures[stID]
		if !ok || st == nil {
			continue
		}
		// TagBusiness first: snapshotKeeperPresent only asks whether someone whose
		// workplace this is stands here awake, which the constable at his Meeting House
		// post satisfies — and a traveler sent to pass the news at a meeting house
		// apologized for the intrusion himself (LLM-554).
		if !structureSnapIsBusiness(snap, stID) {
			continue
		}
		if structureSnapIsLodging(snap, stID) || !snapshotKeeperPresent(snap, stID) {
			continue
		}
		tile := vobj.Pos.Tile()
		view.OpenShops = append(view.OpenShops, RoundsShop{
			Name:      st.DisplayName,
			Direction: cardinalDirection(float64(actorSnap.Pos.X), float64(actorSnap.Pos.Y), float64(tile.X), float64(tile.Y)),
			Steps:     actorSnap.Pos.Chebyshev(tile),
		})
	}
	sort.Slice(view.OpenShops, func(i, j int) bool {
		if view.OpenShops[i].Steps != view.OpenShops[j].Steps {
			return view.OpenShops[i].Steps < view.OpenShops[j].Steps
		}
		return view.OpenShops[i].Name < view.OpenShops[j].Name
	})

	if snap.LocalMinuteOfDay != nil && snap.DawnDuskMinuteOK {
		view.HasClock = true
		view.MinutesToDusk = snap.DuskMinute - *snap.LocalMinuteOfDay
	}
	return view
}

// travelerPackBoundHome reports whether the subject's carried goods are a settled
// BUY-errand traveler's own provisions — the gate on the inventory-line coda that
// says so (LLM-574, moved here from the LLM-544 rounds line).
//
// A buyer spawns with an EMPTY pack (seedBuyerPack, unlike the factor's
// seedFactorPack), so everything in it was come by HERE — bought on his errand, or
// given him over a threshold. That is what makes "your own, not stock to sell" a
// statement of fact rather than a hopeful one, and it is why the claim is scoped to a
// buy errand: a factor's pack IS trade stock, and telling him otherwise would undercut
// the two-way deal his own cue is pressing.
//
// Requires at least one non-service good: a lodger whose whole pack is the nights_stay
// he booked carries nothing bound anywhere, and a coda over a bare room grant would be
// a claim about nothing.
func travelerPackBoundHome(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot) bool {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState == nil {
		return false
	}
	trade := actorSnap.VisitorState.Trade
	if trade == nil || trade.Direction != sim.TradeDirectionBuy || !trade.Settled {
		return false
	}
	for kind, qty := range actorSnap.Inventory {
		if qty <= 0 {
			continue
		}
		if def := snap.ItemKinds[kind]; def != nil && def.HasCapability("service") {
			continue
		}
		return true
	}
	return false
}

// renderTravelerRounds writes the "## Your rounds" surface — a scene (his one piece of
// business, the shops he may look in on, the failing light), not a stat pile. Content-gated.
// Commerce is confined to the errand counterparty (the talk-only tool gate enforces it); the
// prose makes that legible without ever naming a single "go here next" move (LLM-379/455).
func renderTravelerRounds(b *strings.Builder, v *TravelerRoundsView) {
	if v == nil {
		return
	}
	b.WriteString("## Your rounds\n")
	if v.Errand != nil {
		// Bed pressure starts when the light is going (LLM-508) — same boundary as
		// roundsNightfallLine's see-about-a-bed tier, so the settled lead and the
		// nightfall line can never argue about whether it's bedtime. On an unusable
		// clock the lead stays social: a bedtime claim needs a clock to stand on.
		renderRoundsErrand(b, v.Errand, v.HasClock && v.MinutesToDusk <= roundsBedPressureMins)
	} else {
		b.WriteString("You're only passing through this town. Look in on whom you please to show your face and share what news you carry from the road — you've no trade to press here.\n")
	}
	if len(v.Visited) > 0 {
		fmt.Fprintf(b, "So far you've called at %s.\n", joinNames(v.Visited))
	}
	// The other open shops are talk-only social calls (news, a friendly word) — never a
	// place to trade. The errand-settled / passer-through cases still list them; the
	// at-counterparty case does too, so he knows where to go once his business is done.
	if len(v.OpenShops) > 0 {
		b.WriteString("Others keeping shop this hour, to look in on and pass the news (no trading there): ")
		for i, s := range v.OpenShops {
			if i > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(b, "%s, %s", sanitizeInline(s.Name), roundsDistPhrase(s.Steps, s.Direction))
		}
		b.WriteString(".\n")
	}
	if v.HasClock {
		// Trading only while an errand is open: a settled merchant has been told his
		// business is done, and a passer-through has none — for both, the daylight line
		// speaks to the social circuit instead (LLM-507).
		trading := v.Errand != nil && !v.Errand.Settled
		b.WriteString(roundsNightfallLine(v.MinutesToDusk, trading))
	}
	b.WriteString("\n")
}

// renderRoundsErrand writes the merchant's one-trade anchor: the wind-down when his business
// is settled, the trade-here instruction when he stands with his counterparty (the only place
// his commerce tools are cued), or the make-for-it steer with a bearing when he is not there
// yet (LLM-455). bedTime tiers the settled wind-down (LLM-508): a merchant who settles early
// in the day gets the rest of the day for social calls, and the supper-and-bed pitch only
// once the light is going — before that, an all-afternoon bed lead had him announcing
// goodnight for hours in the middle of the day.
func renderRoundsErrand(b *strings.Builder, e *RoundsErrand, bedTime bool) {
	keeper := sanitizeInline(e.KeeperName)
	shop := sanitizeInline(e.ShopLabel)
	good := sanitizeInline(e.GoodLabel)
	switch {
	case e.Settled && e.Buy && bedTime:
		fmt.Fprintf(b, "You have what you came for — %s and stowed in your pack. Your business in this village is done; the tavern's the place now, for your supper and a bed before the road.\n", boughtClause(e, good))
	case e.Settled && e.Buy:
		fmt.Fprintf(b, "You have what you came for — %s and stowed in your pack. Your business in this village is done; the rest of the day is yours to look in on the other shops and pass the news.\n", boughtClause(e, good))
	// The seller's settled lead names the SHIPMENT, not his pack, and hedges it with "most"
	// (LLM-553). The errand is the headline import he came to land; the cloth and charms beside
	// it are a secondary bale that routinely goes home with him. The original wording, "Your
	// goods are sold", rendered directly beneath the "You are carrying:" line and contradicted
	// it — a scene arguing with itself in adjacent lines, which a weak model resolves by picking
	// one at random. "Most" is also literally true at the settle boundary, where up to a quarter
	// of the shipment may still be undelivered by design (sellErrandRemainderDivisor).
	case e.Settled && bedTime:
		fmt.Fprintf(b, "Most of the %s you brought is into the village and your business here is done; the tavern's the place now, for your supper and a bed before the road.\n", good)
	case e.Settled:
		fmt.Fprintf(b, "Most of the %s you brought is into the village and your business here is done; the rest of the day is yours to look in on the other shops and pass the news.\n", good)
	case e.AtShop && e.Buy:
		fmt.Fprintf(b, "You're with %s at %s — the one keeper you came to deal with. Buy the %s you're after: call pay_with_item with seller \"%s\", item \"%s\", the quantity you want, consume_now false, coins in amount, and your words in say.\n",
			keeper, shop, good, keeper, sanitizeInline(e.GoodKind))
	case e.AtShop:
		fmt.Fprintf(b, "You're with %s at %s — the one keeper you came to deal with. Lay out the cloth, iron, and salt you carry from the city and let them buy what the village needs; a warm coat or a bar of iron is worth most just now. Buy their surplus in turn to carry off: call pay_with_item with seller \"%s\", the item, the quantity, consume_now false, coins in amount, and your words in say.\n",
			keeper, shop, keeper)
	case e.Buy:
		fmt.Fprintf(b, "You came to buy %s at %s, %s — that is your business here, so make for it. %s\n",
			good, shop, roundsDistPhrase(e.Steps, e.Direction), otherShopsAside(shop))
	default:
		fmt.Fprintf(b, "You came to deal with the keeper of %s, %s — that is your business here, so make for it. %s\n",
			shop, roundsDistPhrase(e.Steps, e.Direction), otherShopsAside(shop))
	}
}

// boughtClause renders the settled buyer's purchase as he now holds it — "the nails
// are bought" over a pack of nine, "the nail is bought" over one (LLM-574). Falls back
// to the abstract singular label when the good is no longer in his pack at all (given
// away, eaten, or an unlabeled discovery kind), which is the pre-LLM-574 wording.
func boughtClause(e *RoundsErrand, good string) string {
	if e.PackGoodNoun == "" {
		return "the " + good + " is bought"
	}
	if e.PackGoodPlural {
		return "the " + sanitizeInline(e.PackGoodNoun) + " are bought"
	}
	return "the " + sanitizeInline(e.PackGoodNoun) + " is bought"
}

// otherShopsAside is the one-line reminder that the other shops are for news, not trade —
// the legible half of the talk-only confinement (LLM-455).
func otherShopsAside(shop string) string {
	return "The other shops you may look in on to show your face and pass what news you carry, but your trading is done with " + shop + " alone."
}

// roundsDistPhrase renders a shop's bearing as a diegetic phrase — "just to the west",
// "a short way to the north", "off to the east" — or "right here" when the traveler
// stands on it.
func roundsDistPhrase(steps int, dir string) string {
	if dir == "" {
		return "right here"
	}
	switch {
	case steps <= 8:
		return "just to the " + dir
	case steps <= 20:
		return "a short way to the " + dir
	default:
		return "off to the " + dir
	}
}

// roundsBedPressureMins is the minutes-to-dusk boundary where the rounds surface starts
// pressing toward a bed: roundsNightfallLine's see-about-a-bed tier and the settled
// wind-down's supper-and-bed lead (LLM-508) both key on it, so the two lines in the same
// section can never contradict each other about whether it's bedtime.
const roundsBedPressureMins = 60

// roundsNightfallLine is the escalating pressure toward seeking a bed, keyed on minutes
// to dusk. Its wording lets the model decide when to break off trading. trading is false
// for a settled merchant or a passer-through — the plenty-of-daylight tier then points at
// the social circuit rather than contradicting the "your business is done" / "no trade to
// press" lead in the same section (LLM-507); the lower tiers are trade-neutral already.
func roundsNightfallLine(minsToDusk int, trading bool) string {
	switch {
	case minsToDusk <= 0:
		return "The light has all but gone — best see about a bed for the night before long.\n"
	case minsToDusk <= roundsBedPressureMins:
		return "The light is going fast now; you'll want to see about a bed before it's dark.\n"
	case minsToDusk <= 180:
		return "The afternoon is wearing on and the light is starting to lengthen.\n"
	case trading:
		return "There's plenty of daylight left for your trade.\n"
	default:
		return "There's still plenty of light left for you to visit the other businesses in the village.\n"
	}
}

// TravelerSeekBedView is the content-gated "## A bed for the night" section: the
// evening booking cue for a homeless traveler co-present with an innkeeper. It names
// the innkeeper (for the pay_with_item seller arg, which resolves by display name)
// and the inn. nil skips the section.
type TravelerSeekBedView struct {
	// KeeperName is the innkeeper's display name — used verbatim as the
	// pay_with_item seller argument (findHuddlePeerByDisplayName resolves it) and in
	// the prose, mirroring how the co-present buy cue names its seller.
	KeeperName string
	// InnLabel is the inn's display name for the prose ("Hannah's Inn").
	InnLabel string
}

// buildTravelerSeekBed returns the seek-a-bed view when the subject is a homeless
// traveler (no home, no active room grant), it is the civil evening, and the keeper
// of a lodging structure is co-present in its huddle — the moment a booking is both
// wanted and actionable (pay_with_item needs a co-present seller). nil otherwise.
// Pure over the snapshot.
func buildTravelerSeekBed(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, members []HuddleMember) *TravelerSeekBedView {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState == nil {
		return nil
	}
	if !actorSnapIsLodgingSeeker(actorSnap, snap.PublishedAt) {
		return nil // has a home or already holds a room — not seeking a bed
	}
	if !visitorCivilEvening(snap, actorSnap) {
		return nil // still daytime — the rounds cue owns the day
	}
	for _, m := range members {
		ks := snap.Actors[m.ID]
		if ks == nil || ks.BusinessownerState == nil || ks.WorkStructureID == "" {
			continue
		}
		if !structureSnapIsLodging(snap, ks.WorkStructureID) {
			continue
		}
		inn := "the inn"
		if st := snap.Structures[ks.WorkStructureID]; st != nil {
			inn = innLabel(st)
		}
		return &TravelerSeekBedView{KeeperName: m.DisplayName, InnLabel: inn}
	}
	return nil
}

// renderTravelerSeekBed writes the "## A bed for the night" booking cue: it names the
// innkeeper and inn and spells out the pay_with_item call for a nights_stay, with the
// barter-first payment (a ware from the pack) named ahead of coins (LLM-353). The
// keeper's side is already cued to accept (renderPayOffers). Content-gated.
func renderTravelerSeekBed(b *strings.Builder, v *TravelerSeekBedView) {
	if v == nil {
		return
	}
	keeper := sanitizeInline(v.KeeperName)
	inn := sanitizeInline(v.InnLabel)
	b.WriteString("## A bed for the night\n")
	fmt.Fprintf(b, "The light is going and the day's trade is behind you. You have no bed of your own in this town, but %s lets rooms here at %s. Offer them something from your pack for a night's lodging — a ware, or a few coins. Call pay_with_item with seller \"%s\", item \"nights_stay\", qty 1, consume_now false, and your payment: a ware you carry in pay_items, coins in amount, or both, with your words in say. They will take your offer or name their price.\n\n",
		keeper, inn, keeper)
}

// buildVisitorEveningLeisure is the transient-traveler arm of the evening-leisure
// cue (LLM-373), dispatched from buildEveningLeisure. A homeless traveler has no
// night-place of its own, so the resident subjectNightPlace / inEveningLeisure gates
// exclude it — but of an evening it should be drawn to the tavern like anyone, for
// company and to seek its bed. This returns the same EveningLeisureView the resident
// path does, with the tavern as the venue and NO home destination (HomeID ""), so
// renderEveningLeisure takes its no-home branch. Scoped to a traveler still seeking a
// bed — once booked it is a lodger and the standard lodger cues take over. Pure over
// the snapshot.
func buildVisitorEveningLeisure(snap *sim.Snapshot, a *sim.ActorSnapshot) *EveningLeisureView {
	if !visitorCivilEvening(snap, a) {
		return nil
	}
	if !actorSnapIsLodgingSeeker(a, snap.PublishedAt) {
		return nil // booked a room already — the lodger evening path covers it
	}
	venueID, venueLabel, ok := nearestTaggedVenue(snap, a, sim.VisitorTagTavern)
	if !ok {
		return nil // no tavern placed — nothing to steer to
	}
	// Settled tier is gated on being inside the SELECTED venue, not just any leisure
	// venue — with two taverns, insideLeisureVenue would mislabel the traveler as
	// settled at the nearest one while standing in the other (the LLM-345 precision).
	if a.InsideStructureID == venueID {
		if leavingLeisureVenue(a) {
			return nil // already walking out — don't argue at its back
		}
		return &EveningLeisureView{SettledIn: true, VenueLabel: venueLabel}
	}
	if a.MoveDestKind == sim.MoveDestinationStructureEnter && a.MoveDestStructureID == venueID {
		return nil // already walking in — the invitation was acted on
	}
	return &EveningLeisureView{VenueID: venueID, VenueLabel: venueLabel}
}

// visitorCivilEvening reports whether now is in the village's civil evening
// [dusk, lodger bedtime) — the window in which a traveler is drawn to the tavern and
// seeks its bed. Requires a usable dawn/dusk clock; false otherwise (the cues degrade
// to silence rather than firing on a bad clock). Mirrors the lodger night-window
// posture in lodging.go.
func visitorCivilEvening(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if snap == nil || a == nil || snap.LocalMinuteOfDay == nil || !snap.DawnDuskMinuteOK {
		return false
	}
	return minuteInWindow(snap.DuskMinute, snap.LodgingBedtimeMinute, *snap.LocalMinuteOfDay)
}

// structureSnapIsLodging reports whether a structure is the village inn (its backing
// VillageObject carries the "lodging" tag) over the published snapshot — the
// snapshot-side twin of sim.structureIsLodging.
func structureSnapIsLodging(snap *sim.Snapshot, sid sim.StructureID) bool {
	if snap == nil {
		return false
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(sid)]
	return vobj != nil && vobj.HasTag("lodging")
}

// structureSnapIsBusiness reports whether a structure is a place of business (its
// backing VillageObject carries sim.TagBusiness) over the published snapshot — the
// snapshot-side twin of sim.structureIsBusiness, and the same predicate move_to's
// destination hint and the seek-work directory resolve a business by.
func structureSnapIsBusiness(snap *sim.Snapshot, sid sim.StructureID) bool {
	if snap == nil {
		return false
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(sid)]
	return vobj != nil && vobj.HasTag(sim.TagBusiness)
}

// ErrandVisitView is the keeper-facing "## A trader's come to deal" cue (LLM-455, generalizing
// the LLM-410 factor-visit cue): when a merchant visitor whose errand counterparty is THIS
// keeper's shop is co-present, tell the keeper who he is and what the deal is. For a SELLER (a
// factor) the keeper is the one who must act — buy the imported bale with pay_with_item; for a
// BUYER the keeper simply sells as usual (his ordinary seller cues carry the tools), so this is
// a light "a buyer's come for your <good>" heads-up. nil unless the subject is the merchant's
// counterparty keeper with him co-present.
type ErrandVisitView struct {
	// TraderName is the visitor's display name — the pay_with_item seller arg when the keeper
	// buys a seller's bale, and named in the prose.
	TraderName string
	// Origin colors the prose ("out of Boston"); "" drops the clause.
	Origin string
	// Sell is true when the visitor is a seller (a factor bringing imports to sell the keeper);
	// false when he is a buyer coming to buy GoodLabel.
	Sell bool
	// GoodLabel is the display noun of the good a BUYER wants (unused for a seller).
	GoodLabel string
	// Pack lists a SELLER's actual pack goods (his live inventory), each with the
	// keeper's own worth reference where one resolves (LLM-647). Sorted by noun for
	// deterministic render. Empty for a buyer errand or an empty pack.
	Pack []PackGood
}

// PackGood is one good in a selling visitor's pack as the counterparty keeper
// prices it (LLM-647): the count-aware noun, the quantity carried, what one unit
// is worth to THIS keeper, and which rung of the realized-first ladder said so
// (his own recent sales, else his purchases, else the catalog seed). Render
// words each figure by its Source — a purchase-derived figure is what he has
// been paying, never presented as what the good fetches (a past overpayment must
// not read back as a retail realization). Source worthNone means the good
// doesn't price for him: the listing still names it, but no figure is attached —
// silence, never a guessed number.
type PackGood struct {
	Noun   string
	Qty    int
	Worth  int
	Source worthSource
}

// buildErrandVisit returns the keeper-facing cue when the subject is a resident keeper and a
// merchant visitor whose errand Counterparty is the subject's OWN work structure is co-present
// (LLM-455). nil for anyone else, or when no such visitor is present. Pure over the snapshot.
//
// For a SELLER the view also carries the pack listing with the keeper's own worth
// per good (LLM-647): the keeper is about to compose a pay_with_item payment
// against the visitor's asks, and with no reference in the scene an above-market
// ask has nothing arguing against it — live, Josiah overpaid visiting merchants
// by ~617 coins in two weeks while every resident leg priced fine, because his
// asks are anchored (LLM-646) and his payments were not.
func buildErrandVisit(snap *sim.Snapshot, actorID sim.ActorID, actorSnap *sim.ActorSnapshot, members []HuddleMember) *ErrandVisitView {
	if snap == nil || actorSnap == nil {
		return nil
	}
	if actorSnap.VisitorState != nil || actorSnap.BusinessownerState == nil || actorSnap.WorkStructureID == "" {
		return nil // the counterparty is a resident keeper at his own post, never a visitor
	}
	for _, m := range members {
		vs := snap.Actors[m.ID]
		if vs == nil || vs.VisitorState == nil || vs.VisitorState.Trade == nil {
			continue
		}
		t := vs.VisitorState.Trade
		if t.Counterparty != actorSnap.WorkStructureID {
			continue // his errand is with someone else
		}
		view := &ErrandVisitView{
			TraderName: m.DisplayName,
			Origin:     vs.VisitorState.Origin,
			Sell:       t.Direction == sim.TradeDirectionSell,
		}
		if !view.Sell {
			view.GoodLabel = string(t.Good)
			if def := snap.ItemKinds[t.Good]; def != nil {
				view.GoodLabel = def.Singular()
			}
			return view
		}
		view.Pack = buildPackGoods(snap, actorID, vs)
		return view
	}
	return nil
}

// buildPackGoods lists a selling visitor's live inventory as the keeper prices it
// (LLM-647). Worth per unit is offerItemUnitWorth — the same realized-first
// resolution the LLM-598 offer verdict uses, priced for the KEEPER (his own sales
// first, so an import he retails is anchored at what it fetches from his counter,
// which is the honest ceiling on what buying more of it is worth to him). A good
// that resolves no price keeps Worth 0 and renders without a figure. Sorted by
// noun (then kind) so the render is deterministic.
func buildPackGoods(snap *sim.Snapshot, keeperID sim.ActorID, seller *sim.ActorSnapshot) []PackGood {
	if seller == nil || len(seller.Inventory) == 0 {
		return nil
	}
	type entry struct {
		kind sim.ItemKind
		g    PackGood
	}
	entries := make([]entry, 0, len(seller.Inventory))
	for kind, qty := range seller.Inventory {
		if qty <= 0 {
			continue
		}
		noun := string(kind)
		if def := snap.ItemKinds[kind]; def != nil {
			noun = def.CountNoun(qty)
		}
		worth, source := offerItemUnitWorthSource(snap, keeperID, kind)
		entries = append(entries, entry{kind: kind, g: PackGood{
			Noun:   noun,
			Qty:    qty,
			Worth:  worth,
			Source: source,
		}})
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].g.Noun != entries[j].g.Noun {
			return entries[i].g.Noun < entries[j].g.Noun
		}
		return entries[i].kind < entries[j].kind
	})
	out := make([]PackGood, len(entries))
	for i, e := range entries {
		out[i] = e.g
	}
	return out
}

// renderErrandVisit writes the "## A trader's come to deal" cue for the counterparty keeper
// (LLM-455). For a seller it frames the imported bale and names pay_with_item for the leg the
// keeper drives (buying it); for a buyer it is a light heads-up that his ordinary sale is what's
// wanted. Content-gated.
func renderErrandVisit(b *strings.Builder, v *ErrandVisitView) {
	if v == nil {
		return
	}
	name := sanitizeInline(v.TraderName)
	origin := ""
	if v.Origin != "" {
		origin = " out of " + sanitizeInline(v.Origin)
	}
	b.WriteString("## A trader's come to deal\n")
	if v.Sell {
		fmt.Fprintf(b, "%s, a factor%s, is here to deal with you. He's brought city goods to sell, and he'll buy the surplus stacking up in your store to carry off.", name, origin)
		renderPackGoods(b, v.Pack)
		fmt.Fprintf(b, " Buy what the village needs from his pack with pay_with_item (seller \"%s\", the item, the quantity, consume_now false, coins in amount, your words in say), and let him buy your surplus in turn.\n\n", name)
		return
	}
	good := sanitizeInline(v.GoodLabel)
	fmt.Fprintf(b, "%s, a trader%s, has come to buy your %s to carry off and sell elsewhere. Sell him what you can spare as you would any customer, and name your price if he asks.\n\n", name, origin, good)
}

// renderPackGoods writes the pack listing with the keeper's worth references
// (LLM-647): what the visitor actually carries, and — for each good that prices —
// a figure worded by its provenance, so the keeper weighs the visitor's asks
// against his own numbers instead of taking them on faith. Provenance matters
// (code_review): a sale-derived figure IS what the good fetches from his counter;
// a purchase-derived figure is only what he has been paying — for an import that
// may be a past overpayment, so it renders as exactly that claim and no more; a
// catalog figure is the going rate. Goods that don't price are named without a
// figure. When at least one good priced, closes with the weigh-it counsel; an
// all-unpriced pack gets the bare listing and no counsel — no figures to weigh
// against. Writes nothing for an empty pack (the prose above already covers the
// deal in general terms). Nouns come from the catalog via the view; sanitized here.
func renderPackGoods(b *strings.Builder, pack []PackGood) {
	if len(pack) == 0 {
		return
	}
	b.WriteString(" In his pack: ")
	priced := false
	for i, g := range pack {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "%d %s", g.Qty, sanitizeInline(g.Noun))
		if g.Worth <= 0 {
			continue
		}
		switch g.Source {
		case worthFromSales:
			priced = true
			fmt.Fprintf(b, " (fetches about %d each from your counter)", g.Worth)
		case worthFromPurchases:
			priced = true
			fmt.Fprintf(b, " (you've been paying about %d each)", g.Worth)
		case worthFromCatalog:
			priced = true
			fmt.Fprintf(b, " (the customary price is about %d)", g.Worth)
		}
	}
	b.WriteString(".")
	if priced {
		// "what you know of their prices", not "what they are worth" — a
		// purchase-derived figure may be a past overpayment and a catalog seed is
		// custom, not observation; neither supports an intrinsic-worth claim
		// (code_review, round 2).
		b.WriteString(" Weigh his asking prices against what you know of their prices — counter a dear ask or let it go rather than overpay; a bale bought above what it sells on for is coin lost.")
	}
}

// structureSnapIsTavernOrInn reports whether a structure is a tavern or an inn (a "tavern" or
// "lodging" tagged VillageObject) over the snapshot — the visitor's self-provisioning venues (a
// meal, a bed, journeycake), where his commerce tools stay reachable (LLM-455).
func structureSnapIsTavernOrInn(snap *sim.Snapshot, sid sim.StructureID) bool {
	if snap == nil {
		return false
	}
	vobj := snap.VillageObjects[sim.VillageObjectID(sid)]
	return vobj != nil && (vobj.HasTag(sim.VisitorTagTavern) || vobj.HasTag("lodging"))
}

// visitorCommerceStripped reports whether a visitor's commerce tools should be withheld this
// tick (LLM-455) — the talk-only-rounds gate. A visitor's trade is confined to sanctioned
// places: his errand counterparty (his one real trade) and any tavern/inn (self-provisioning —
// a meal, a bed, journeycake). Anywhere else is talk-only, so the pay / offer / quote tools are
// stripped unless he is co-present with a sanctioned keeper. False for a non-visitor (the gate is
// visitor-only). Pure over the snapshot.
func visitorCommerceStripped(snap *sim.Snapshot, actorSnap *sim.ActorSnapshot, members []HuddleMember) bool {
	if snap == nil || actorSnap == nil || actorSnap.VisitorState == nil {
		return false
	}
	var counterparty sim.StructureID
	if actorSnap.VisitorState.Trade != nil {
		counterparty = actorSnap.VisitorState.Trade.Counterparty
	}
	for _, m := range members {
		ks := snap.Actors[m.ID]
		if ks == nil || ks.BusinessownerState == nil || ks.WorkStructureID == "" {
			continue
		}
		if counterparty != "" && ks.WorkStructureID == counterparty {
			return false // at his errand keeper — his one real trade is allowed
		}
		if structureSnapIsTavernOrInn(snap, ks.WorkStructureID) {
			return false // at a tavern/inn keeper — self-provisioning allowed
		}
	}
	return true
}
