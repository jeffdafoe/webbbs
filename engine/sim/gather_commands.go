package sim

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// gather_commands.go — ZBBS-WORK-328. The v2 revival of v1's `gather` verb
// (engine/gather.go) as a general environmental-harvest substrate.
//
// A gatherable source is a placed VillageObject carrying an ObjectRefresh row
// with GatherItem set (see object_refresh.go). Gather is the "fill a pail"
// complement to ApplyObjectRefreshAtArrival's "drink in place": both resolve
// the same loitering source and, for a finite source, draw down the SAME
// AvailableQuantity — so a well/bush is one shared stock that the regen tick
// (RunObjectRefreshRegen) refills. The same command backs both the NPC
// `gather` tool and the PC pc/gather route; the actor kind doesn't matter
// here (gating happens at the tool/route layer).

// MaxGatherQty bounds qty accepted by the Gather Command — defense in depth
// against int wrap on inventory math from a non-handler caller (Gather is
// exported). Gameplay-reasonable caps belong at the handler/route layer.
const MaxGatherQty = math.MaxInt32

var (
	// ErrNoGatherSource — the actor isn't loitering at a gatherable source
	// (no named object owns their tile, or the one that does carries no
	// GatherItem refresh row). LLM-facing signal: "walk to a well/bush first."
	ErrNoGatherSource = errors.New("no gatherable source here")

	// ErrGatherableDepleted — the source is finite and currently empty. It
	// refills over time via the regen tick; this is a transient reject, not a
	// permanent one.
	ErrGatherableDepleted = errors.New("the source is depleted right now")

	// ErrNotYourSource — the gatherable source is OWNED by another actor.
	// Gather + eat at an owned village object are owner-only (LLM-50 D2,
	// VillageObject.OwnedByOther); unowned sources are commons. A permanent
	// reject for this actor — LLM-facing signal: it belongs to someone else,
	// look for a wild source instead.
	ErrNotYourSource = errors.New("that source belongs to someone else — find a wild one")
)

// GatherResult is the Command reply — what was harvested, for the handler /
// route to build the tool-result or HTTP response narration without
// re-deriving anything.
type GatherResult struct {
	ObjectID   VillageObjectID
	SourceName string // resolved display/catalog name, e.g. "Old Well"
	Item       ItemKind
	Qty        int // units actually gathered (<= requested when finite ran low)
	// SourceDepleted is true when this pick emptied a finite source (a bush picked
	// clean — gather takes all ripe units, LLM-87). Drives the "it's bare now"
	// completion beat (LLM-175); always false for an infinite source (a well).
	SourceDepleted bool
}

// findGatherableObjectNear resolves the village object the actor should harvest
// from and its first gatherable row via the shared ResolveGatherSource (the same
// resolver the perception cue uses, so cue and command agree). lowItems is the
// actor's below-threshold forage set, the item bias for the dense-plot fallback.
func findGatherableObjectNear(w *World, actor *Actor) (VillageObjectID, *VillageObject, *ObjectRefresh) {
	low := LowForageItems(actor.RestockPolicy, actor.Inventory, w.Settings.RestockReorderPct)
	return ResolveGatherSource(w.VillageObjects, w.Assets, actor.Pos, actor.ID, actor.GatherTargetObjectID, low, ForageItems(actor.RestockPolicy))
}

// Gather returns a Command that harvests qty units of the gatherable source
// the actor is loitering at into their inventory.
//
// Pre-conditions (Gather is exported — non-handler callers must not bypass):
//   - qty defaults to 1 when < 1 (v1 behavior); rejected when > MaxGatherQty
//   - actorID resolves to a real actor in w.Actors
//   - actor.MoveIntent == nil (not walk-in-flight — must have arrived)
//   - the actor is loitering at a gatherable source (ErrNoGatherSource)
//   - the source's GatherItem resolves to a known kind (else misconfiguration
//     surfaces as ErrUnknownItemKind)
//   - a finite source has stock (ErrGatherableDepleted)
//
// On success:
//   - finite source: AvailableQuantity decrements by qty, clamped so qty never
//     exceeds remaining stock (a request for 5 from a bush with 3 yields 3)
//   - infinite source (well, AvailableQuantity nil): no decrement, qty as asked
//   - actor.Inventory[kind] += qty (lazy-inits the map)
//   - emits ItemGathered
func Gather(actorID ActorID, qty int, at time.Time) Command {
	return Command{
		Fn: func(w *World) (any, error) {
			// Work off locals — never mutate the captured qty, so a re-run of
			// this (exported) Command observes the original request, not a
			// clamped/defaulted value.
			requested := qty
			if requested < 1 {
				requested = 1
			}
			if requested > MaxGatherQty {
				return nil, fmt.Errorf("Gather: qty exceeds maximum (got %d, max %d)", requested, MaxGatherQty)
			}

			actor, ok := w.Actors[actorID]
			if !ok {
				return nil, fmt.Errorf("Gather: actor %q not in world", actorID)
			}
			if actor.MoveIntent != nil {
				return nil, errors.New(
					"you are walking — finish your move before gathering. " +
						"Walk to the source and arrive, then gather.",
				)
			}

			objID, obj, row := findGatherableObjectNear(w, actor)
			if row == nil {
				return nil, fmt.Errorf("Gather: %w", ErrNoGatherSource)
			}
			// Strict owner-gate (LLM-50 D2): an owned source is owner-only.
			// Same resolve-then-check posture the cue + arrival paths use; the
			// PC route (pc_gather.go) delegates to this Command, so gating here
			// covers both the NPC tool and the PC verb.
			if obj.OwnedByOther(actorID) {
				return nil, fmt.Errorf("Gather: %w", ErrNotYourSource)
			}

			// gather_item is stored canonical, but resolve case-insensitively
			// (and trim) so a hand-edited value still maps; an unresolvable
			// value is a data misconfiguration and surfaces loudly.
			kind, ok := resolveItemKind(w, string(row.GatherItem))
			if !ok {
				return nil, fmt.Errorf("Gather: %w %q (source %s gather_item)", ErrUnknownItemKind, row.GatherItem, objID)
			}

			return applyGatherMint(w, actor, objID, obj, row, kind, requested, at)
		},
	}
}

// applyGatherMint is the EFFECT half of a harvest: clamp the requested qty to a
// finite source's remaining stock, decrement that supply, credit the actor's
// inventory, recompute the bush's berry/bare visual, and emit ItemGathered.
// Shared by the instant Gather command and the timed harvest completion
// (source_activity.go). Caller has resolved the gatherable object + row, passed
// the owner gate, and resolved kind; requested is the validated amount (>= 1).
// Returns the harvested GatherResult, or ErrGatherableDepleted when a finite
// source has run dry by the time the mint lands — a transient reject for the
// instant caller, a benign nothing-harvested completion for the timed one.
func applyGatherMint(w *World, actor *Actor, objID VillageObjectID, obj *VillageObject, row *ObjectRefresh, kind ItemKind, requested int, at time.Time) (GatherResult, error) {
	// Resolve the actual amount (clamped to a finite source's stock) and
	// validate fully BEFORE any mutation — an early return after decrementing
	// supply would lose stock with no inventory credited.
	actual := requested
	if row.IsFinite() {
		avail := *row.AvailableQuantity
		if avail <= 0 {
			return GatherResult{}, fmt.Errorf("Gather: %w", ErrGatherableDepleted)
		}
		if actual > avail {
			actual = avail
		}
	}
	cur := actor.Inventory[kind]
	if cur > math.MaxInt-actual {
		// Pathological accumulated stock — refuse rather than wrap negative.
		return GatherResult{}, fmt.Errorf("Gather: inventory quantity overflow for %q (have %d, +%d)", kind, cur, actual)
	}

	// Mutations (post-validation): decrement finite supply, credit inventory.
	if row.IsFinite() {
		drawDownStock(row, actual, at)
		// Picking may have emptied a finite bush — recompute its
		// berries/bare visual so it goes bare.
		refreshObjectBerryState(w, obj, at)
	}
	if actor.Inventory == nil {
		actor.Inventory = make(map[ItemKind]int)
	}
	actor.Inventory[kind] = cur + actual

	// Equipment service (LLM-648): harvesting your OWN source (the wheat field,
	// the berry rows) is worked output like a minted batch, so it accrues
	// deep-maintenance demand on the harvester's owned business. A commons
	// source (the village well) is nobody's equipment and accrues nothing.
	if obj.OwnerActorID == actor.ID {
		AccrueEquipmentUse(w, actor.ID, actual)
	}

	catalogName := ""
	if a := w.Assets[obj.AssetID]; a != nil {
		catalogName = a.Name
	}
	name := obj.EffectiveDisplayName(catalogName)

	w.emit(&ItemGathered{
		ActorID:  actor.ID,
		ObjectID: objID,
		Item:     kind,
		Qty:      actual,
		At:       at,
	})

	// A finite source emptied by this pick reads as "bare" downstream (the
	// completion beat's "it's bare now" copy, LLM-175). Read AFTER drawDownStock
	// decremented the supply above; an infinite source (nil AvailableQuantity) is
	// never depleted.
	depleted := row.IsFinite() && *row.AvailableQuantity <= 0

	return GatherResult{
		ObjectID:       objID,
		SourceName:     name,
		Item:           kind,
		Qty:            actual,
		SourceDepleted: depleted,
	}, nil
}
