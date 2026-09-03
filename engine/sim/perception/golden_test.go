package perception

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// golden_test.go — LLM-106 perception golden-payload scenario harness (proof of
// concept). Each scenario builds a deterministic Snapshot fixture for one
// situation the perception layer branches on, renders the WHOLE assembled prompt
// (durable + ephemeral — exactly what the model receives, via combinedPrompt),
// and pins it to a checked-in golden file under testdata/goldens/.
//
// The value is the DIFF: a cue change shows, per scenario, exactly how the prompt
// an NPC sees changed — surfacing a cue that leaks into (or vanishes from) a
// situation it shouldn't, which per-builder unit tests structurally can't see
// (they assert one builder's output, never the assembled prompt across the
// branching axes). After an INTENDED change, regenerate and review:
//
//	go test ./sim/perception -run TestPerceptionGoldens -update-goldens
//	git diff -- engine/sim/perception/testdata/goldens   # read every changed scenario
//
// Scope (POC): scenarios MUST be clock-free — no pending deliveries / owed orders.
// renderPendingDeliveries{From,To}Me read time.Now() for the per-order expiry
// clause (render.go), so an order-bearing scenario is not byte-stable yet.
// Injecting that render clock from the Payload is the prerequisite for bringing
// order scenarios into the matrix — tracked on LLM-106. The per-scenario
// determinism guard below trips loudly if a wall-clock read ever sneaks in.

var updateGoldens = flag.Bool("update-goldens", false, "rewrite perception scenario golden files instead of comparing")

// perceptionScenario is one situation under test: a deterministic fixture builder
// plus a stable, filesystem-safe name that maps to testdata/goldens/<name>.golden.
// summary documents intent for a human reading the scenario list — it is NOT
// written into the golden, which stays the exact prompt text the model sees.
type perceptionScenario struct {
	name    string
	summary string
	build   func() (snap *sim.Snapshot, actorID sim.ActorID, warrants []sim.WarrantMeta)
}

func renderScenario(sc perceptionScenario) string {
	snap, actorID, warrants := sc.build()
	return combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
}

func TestPerceptionGoldens(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			got := renderScenario(sc)

			// Determinism guard: re-render from a freshly built fixture and require
			// byte equality. Map-iteration order or a wall-clock read sneaking into
			// the render path would trip this here rather than silently churning the
			// golden on the next -update.
			if second := renderScenario(sc); second != got {
				t.Fatalf("non-deterministic render for %q: two renders of the same fixture differ", sc.name)
			}

			goldenPath := filepath.Join("testdata", "goldens", sc.name+".golden")
			if *updateGoldens {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("mkdir goldens dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s (run with -update-goldens to create it): %v", goldenPath, err)
			}
			if got != string(want) {
				t.Errorf("perception prompt for %q changed.\nIf this change is intended, re-run with -update-goldens and review the golden diff before committing.\n--- got ---\n%s\n--- want (golden) ---\n%s", sc.name, got, string(want))
			}
		})
	}
}

// TestGoldensNeverAdvertiseHomeAsMoveTargetWhenInside is the LLM-214 cross-scenario
// invariant: whenever the subject actor is standing INSIDE its own home, the
// rendered prompt must never advertise that home's structure_id as a move target.
// "(destination: <id>)" is the load-bearing token the model echoes into move_to
// (HOME-349), and you can't move to where you already are — the no-op the model
// looped on (Lewis Walker calling move_to{residence} every tick). Runs over the
// whole matrix so a future cue can't reintroduce the current-home move target for
// any situation, not just the one weary_resident_in_own_home scenario pins.
func TestGoldensNeverAdvertiseHomeAsMoveTargetWhenInside(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.HomeStructureID == "" || a.InsideStructureID != a.HomeStructureID {
				return // subject isn't inside its own home — invariant N/A here
			}
			token := "(destination: " + string(a.HomeStructureID) + ")"
			if out := renderScenario(sc); strings.Contains(out, token) {
				t.Errorf("scenario %q: subject stands inside its own home but the prompt advertises that home as a move target %q — you can't move_to where you stand (LLM-214)", sc.name, token)
			}
		})
	}
}

// TestGoldensTargetedQuoteIsVisibleToItsTarget is the LLM-551 cross-scenario
// invariant: whenever an ACTIVE scene-quote names the subject as its target
// buyer, the rendered prompt must account for it — either actionably (its
// quote_id, the token pay_with_item needs to TAKE it) or with an explicit
// decline steer (LLM-171, when every quoted good is one she makes herself or
// already holds at cap). What it may never be is silently absent.
//
// That silence was the defect. A targeted quote warrants its buyer exactly once,
// and a buyer who spends that tick answering aloud — speak is terminal, so this
// is the ordinary case — saw nothing of the offer ever again. Patience Walker
// spent 22 minutes and ~20 rejected pay_with_item calls trying to buy flour that
// was already set out for her, while the seller's prompt showed it standing
// throughout. Stated as a matrix invariant rather than a single golden because
// the failure is an ABSENCE: any future scenario that puts a quote in front of a
// buyer must keep it reachable, and an absence is exactly what a per-scenario
// golden diff is worst at catching.
//
// Carve-out: a lodging quote to a subject who already has a home is DELIBERATELY
// suppressed (LLM-182/208) — she structurally cannot take the room, so the offer
// is dropped rather than dangled. That is the one situation where invisible is
// correct.
func TestGoldensTargetedQuoteIsVisibleToItsTarget(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subject := snap.Actors[actorID]
			if subject == nil {
				return
			}
			var out string
			// Keyed on q.ID, not the map key — the builder and renderer both use
			// the quote's own id, so the assertion must check the identifier that
			// actually reaches the prompt.
			for _, q := range snap.Quotes {
				if q == nil || q.State != sim.SceneQuoteStateActive || q.TargetBuyer != actorID {
					continue
				}
				id := q.ID
				if subject.HomeStructureID != "" && quoteGrantsLodging(snap, q.Lines) {
					continue // homed subject, lodging quote — suppressed on purpose
				}
				if out == "" {
					out = renderScenario(sc)
				}
				takeToken := "quote_id " + strconv.FormatUint(uint64(id), 10)
				declined := strings.Contains(out, "there's no reason to buy")
				if !strings.Contains(out, takeToken) && !declined {
					t.Errorf("scenario %q: quote %d is active and addressed to the subject, but the prompt carries neither %q (the token pay_with_item needs to take it) nor a decline steer — the buyer cannot act on an offer she cannot see (LLM-551)", sc.name, id, takeToken)
				}
			}
		})
	}
}

// TestGoldensTravelerPrefaceIffSubjectIsTraveler is the LLM-370 cross-scenario
// invariant: the self-identity preface (its unique "making your way through Salem"
// phrase) renders in a scenario's prompt IFF the SUBJECT is a transient traveler
// (its snapshot carries a VisitorState with an archetype). Guards both directions
// across the whole matrix — a traveler subject must get its persona preface, and
// every persistent NPC / PC must NOT, so the engine-injected identity can neither
// leak into a non-traveler's prompt nor silently stop opening a traveler's.
func TestGoldensTravelerPrefaceIffSubjectIsTraveler(t *testing.T) {
	const prefaceMarker = "making your way through Salem"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			wantPreface := a != nil && a.VisitorState != nil && a.VisitorState.Archetype != ""
			hasPreface := strings.Contains(renderScenario(sc), prefaceMarker)
			if wantPreface != hasPreface {
				t.Errorf("scenario %q: subject is traveler=%v but preface present=%v — the self-identity preface must open the message iff the subject is a transient traveler (LLM-370)", sc.name, wantPreface, hasPreface)
			}
		})
	}
}

// TestGoldensTravelerVocationMatchesArchetype is the LLM-566 invariant, two
// halves. Matrix half: every scenario whose subject is a traveler carries the
// archetype's vocation sentence IFF sim.VisitorVocation knows the archetype —
// a pool traveler must state its calling, and an archetype outside the pool
// (merchant-derived labels like "peddler") must not grow one. Pool half: every
// passer-through archetype (keyed off the exported sprite map, which init()
// proves congruent with the pool) has a non-empty vocation that renders through
// the production preface builder — so adding a pool archetype without a
// vocation line fails here even before the sim init() panic is ever exercised
// by a test binary that imports it.
func TestGoldensTravelerVocationMatchesArchetype(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.VisitorState == nil || a.VisitorState.Archetype == "" {
				return
			}
			rendered := renderScenario(sc)
			if vocation := sim.VisitorVocation(a.VisitorState.Archetype); vocation != "" {
				if !strings.Contains(rendered, vocation) {
					t.Errorf("scenario %q: traveler archetype %q has a vocation line but the prompt does not carry it (LLM-566)", sc.name, a.VisitorState.Archetype)
				}
				return
			}
			for archetype := range sim.VisitorArchetypeSprite {
				if voc := sim.VisitorVocation(archetype); voc != "" && strings.Contains(rendered, voc) {
					t.Errorf("scenario %q: traveler archetype %q is outside the passer-through pool but the prompt carries the %q vocation line (LLM-566)", sc.name, a.VisitorState.Archetype, archetype)
				}
			}
		})
	}

	for archetype := range sim.VisitorArchetypeSprite {
		vocation := sim.VisitorVocation(archetype)
		if vocation == "" {
			t.Errorf("passer-through archetype %q has no vocation line (LLM-566)", archetype)
			continue
		}
		var b strings.Builder
		renderTravelerPreface(&b, &TravelerSelfView{Name: "Test Traveler", Archetype: archetype, Vocation: vocation})
		if !strings.Contains(b.String(), vocation) {
			t.Errorf("passer-through archetype %q: vocation line does not survive renderTravelerPreface (LLM-566)", archetype)
		}
	}

	// The explicit non-pool contract, independent of scenario inventory: a
	// merchant-derived label and an unknown archetype both project an empty
	// Vocation through the production builder.
	for _, archetype := range []string{"peddler", "tinker nobody registered"} {
		a := &sim.ActorSnapshot{
			DisplayName:  "Test Traveler the " + archetype,
			VisitorState: &sim.VisitorState{Archetype: archetype},
		}
		v := buildTravelerSelf(&sim.Snapshot{}, "vstr-test", a)
		if v == nil {
			t.Fatalf("archetype %q: buildTravelerSelf returned nil for a traveler subject", archetype)
		}
		if v.Vocation != "" {
			t.Errorf("archetype %q is outside the passer-through pool but buildTravelerSelf set Vocation %q (LLM-566)", archetype, v.Vocation)
		}
	}
}

// TestGoldensRoadWordSharedClauseMatchesPresentCompany is the LLM-545
// cross-scenario invariant, computed through the production builder rather than
// restated wording: for every scenario whose subject carries a road-word payload,
// the preface's shared-word clause ("you have already passed" in either tier)
// renders IFF roadWordSharedWith finds a stamped peer in present company,
// and every name it renders belongs to a co-present actor. Guards the
// co-presence rule matrix-wide — a stamp for someone elsewhere must never
// surface a name, and a stamped co-present peer must never be silently dropped.
func TestGoldensRoadWordSharedClauseMatchesPresentCompany(t *testing.T) {
	const clauseMarker = "you have already passed"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			rendered := renderScenario(sc)
			hasClause := strings.Contains(strings.ToLower(rendered), clauseMarker)
			if a == nil || a.VisitorState == nil || a.VisitorState.Payload == "" {
				if hasClause {
					t.Errorf("scenario %q: shared-word clause rendered for a subject with no rumor payload", sc.name)
				}
				return
			}
			names, _ := roadWordSharedWith(snap, actorID, a)
			if want := len(names) > 0; want != hasClause {
				t.Errorf("scenario %q: production builder finds %d co-present shared peers but clause present=%v — the shared-word clause must render iff a stamped peer is in the scene (LLM-545)", sc.name, len(names), hasClause)
			}
			for _, n := range names {
				if !strings.Contains(rendered, n) {
					t.Errorf("scenario %q: co-present shared peer %q missing from the rendered clause", sc.name, n)
				}
			}
		})
	}
}

// TestGoldensSelfNameIffSharedVillager is the LLM-432 cross-scenario
// invariant: the "You are <name>." self-name line renders IFF the subject is
// a shared-VA villager (KindNPCShared, not a traveler) with a display name.
// The shared VA's system prompt is a generic sim context, so this line is the
// ONLY place a shared villager learns who it is — without it, a bystander
// hearing "ezekiel, you sleeping over there?" cannot rule itself out as the
// addressee (the live Patience Walker misfire). Guards both directions:
// stateful NPCs and PCs must NOT get the line (their identity comes from
// their own VA's <Self> block / the player), and travelers keep their own
// preface (LLM-370) instead.
func TestGoldensSelfNameIffSharedVillager(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			// Sanitize like the render path does, so a display name the
			// renderer flattens can't make the invariant look for a marker
			// the renderer never emits.
			name := sanitizeInline(a.DisplayName)
			if name == "" {
				return
			}
			marker := "You are " + name + ".\n"
			wantSelfName := a.Kind == sim.KindNPCShared && a.VisitorState == nil
			hasSelfName := strings.Contains(renderScenario(sc), marker)
			if wantSelfName != hasSelfName {
				t.Errorf("scenario %q: shared villager=%v but self-name line present=%v — '## Who you are' must open with the subject's own name iff it is a shared-VA villager (LLM-432)", sc.name, wantSelfName, hasSelfName)
			}
		})
	}
}

// TestGoldensTradeSceneAffordanceAlwaysStatesReadiness is the LLM-509 cross-scenario
// invariant: every "## Your trade" batch-affordance line must close with a readiness
// beat — "and you have the makings." for an inputs recipe, "and you have everything you
// need." for a no-input (origin) recipe — and never render bare with only the time.
// The bare no-input branch was the silence a weak model filled with its real-world
// prior, confabulating an iron requirement (and a supplier and a debt) for a pure-labor
// craft (Ezekiel Crane, 2026-07-23). Runs over the whole matrix so no future cue change
// can reintroduce a batch offer that states how long it takes but not whether the
// producer has what it needs.
func TestGoldensTradeSceneAffordanceAlwaysStatesReadiness(t *testing.T) {
	// Accumulated across the (sequential, non-parallel) subtests: how many trade
	// affordance lines the invariant actually inspected matrix-wide. Guarded > 0
	// below so a cue-wording change that stopped matching "A batch — " can't turn
	// the whole invariant vacuous while still passing green (code_review).
	matrixChecked := 0
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, line := range strings.Split(renderScenario(sc), "\n") {
				// Only the tradeGoodScene affordance clause carries this exact shape.
				if !strings.Contains(line, "A batch — ") || !strings.Contains(line, "takes about ") {
					continue
				}
				matrixChecked++
				// The affordance is the last thing tradeGoodScene writes, so the line must
				// END with a readiness clause — HasSuffix, not Contains, so a clause buried
				// mid-line can't satisfy it and the bare "…takes about X." can't slip through.
				if !strings.HasSuffix(line, ", and you have the makings.") &&
					!strings.HasSuffix(line, ", and you have everything you need.") {
					t.Errorf("scenario %q: trade-scene batch affordance renders with no readiness beat (bare no-input branch reintroduced, LLM-509):\n  %s", sc.name, line)
				}
			}
		})
	}
	if matrixChecked == 0 {
		t.Error("no scenario rendered a '## Your trade' batch affordance line — the readiness-beat invariant is vacuous (LLM-509)")
	}
}

// TestGoldensReturnerContinuityIffRepeatVisit is the LLM-372 cross-scenario
// invariant: the returner continuity block (the visit clause that opens it)
// renders IFF the subject is a returning traveler on a repeat visit
// (ActorSnapshot.Returner != nil && VisitCount >= 2). Computes the expected
// marker through returnerVisitClause itself (like the rain invariant) so it tracks
// production exactly. Guards both directions — a repeat-visit returner must carry
// the continuity, and a one-shot / first-visit traveler (nil Returner) must NOT,
// so the "you've been here before" beat can neither leak to a stranger nor
// silently stop opening for a returner.
func TestGoldensReturnerContinuityIffRepeatVisit(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			wantReturner := a != nil && a.Returner != nil && a.Returner.VisitCount >= 2
			marker := ""
			if a != nil && a.Returner != nil {
				marker = returnerVisitClause(a.Returner.VisitCount)
			}
			hasReturner := marker != "" && strings.Contains(renderScenario(sc), marker)
			if wantReturner != hasReturner {
				t.Errorf("scenario %q: repeat-visit returner=%v but continuity present=%v — the returner continuity block must render iff the subject is a returning traveler on a repeat visit (LLM-372)", sc.name, wantReturner, hasReturner)
			}
		})
	}
}

// TestGoldensRainLineIffStorm is the LLM-364 cross-scenario invariant: the felt
// rain line renders in a scenario's prompt IFF that scenario's snapshot has an
// active storm (Environment.Weather == storm). Guards both directions across the
// whole matrix — a storm scenario must carry the line, and every clear/weatherless
// scenario must NOT (so the deterministic weather cue can't leak into a calm-day
// prompt, and a storm can't silently stop surfacing).
func TestGoldensRainLineIffStorm(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, _, _ := sc.build()
			// Compute "should this scenario show rain?" through weatherProse itself
			// rather than restating the token check — the invariant then matches
			// production exactly and won't drift when future tokens (fog/snow) gain
			// their own prose (code_review, LLM-364).
			wantRain := weatherProse(snap.Environment.Weather) == weatherStormProse
			hasRain := strings.Contains(renderScenario(sc), weatherStormProse)
			if wantRain != hasRain {
				t.Errorf("scenario %q: storm=%v but rain line present=%v — the felt rain line must render iff a storm is overhead (LLM-364)", sc.name, wantRain, hasRain)
			}
		})
	}
}

// TestGoldensContactLineIffCoPresentPeer is the LLM-547 cross-scenario invariant
// on the co-presence gate: a contact line may name ONLY someone actually in the
// scene.
//
// The property matters more than the six goldens that pin the tiers. The ledger
// is a per-pair record with no notion of place, so the one thing standing
// between it and a roll-call of everyone the subject has ever spoken to — re-sent
// every tick, prompt-bloating and legibility-destroying — is that the tier is
// attached in resolveCoPresentMember, which runs only for a present peer. That
// is a structural guarantee rather than a check, and structural guarantees are
// exactly the ones a later refactor breaks silently.
//
// Computed through contactRecencyLines, production's own function, rather than
// by restating its wording — a reword leaves the invariant matching, and a
// change to which peers it draws from is caught. Same reasoning as
// TestGoldensRainLineIffStorm computing through weatherProse.
func TestGoldensContactLineIffCoPresentPeer(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			lines := contactRecencyLines(p.Surroundings)
			if len(lines) == 0 {
				return
			}
			// Every name the block can legitimately mention: the people in the
			// scene, by the same DisplayName render uses.
			present := make(map[string]struct{})
			for _, group := range [][]HuddleMember{p.Surroundings.HuddleMembers, p.Surroundings.CoPresent} {
				for _, m := range group {
					present[sanitizeInline(m.DisplayName)] = struct{}{}
				}
			}
			for _, line := range lines {
				named := false
				for name := range present {
					if name != "" && strings.Contains(line, name) {
						named = true
						break
					}
				}
				if !named {
					t.Errorf("scenario %q: contact line %q names nobody who is in the scene — "+
						"the fact may only be told about a co-present peer (LLM-547)", sc.name, line)
				}
			}
		})
	}
}

// TestGoldensContactLineNamesPeopleNotPlaces is the LLM-547 register invariant,
// and it guards the constraint most likely to be "improved" back into a defect.
//
// LLM-530 names the round's NEXT stop verbatim precisely BECAUSE it has to work
// as a move_to token. The reasoning inverts for history: naming shops already
// walked hands the model fresh, attractive destinations — which is the behaviour
// this ticket exists to stop. People for what is done, places for what remains.
//
// Structure DISPLAY NAMES are the failure mode, not structure ids: a well-meant
// "you had your word with Prudence Ward at the PW Apothecary" reads natural and
// is exactly wrong. So the check sweeps every structure in the scenario.
func TestGoldensContactLineNamesPeopleNotPlaces(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			lines := contactRecencyLines(p.Surroundings)
			if len(lines) == 0 {
				return
			}
			for _, line := range lines {
				for id, st := range snap.Structures {
					if st == nil || st.DisplayName == "" {
						continue
					}
					if strings.Contains(line, st.DisplayName) {
						t.Errorf("scenario %q: contact line %q names the place %q — a history line "+
							"must name people only; naming a walked stop hands the model a fresh "+
							"destination, which is the behaviour LLM-547 fixes", sc.name, line, st.DisplayName)
					}
					if strings.Contains(line, string(id)) {
						t.Errorf("scenario %q: contact line %q carries the structure id %q — a "+
							"history line is never a move_to affordance (LLM-547)", sc.name, line, id)
					}
				}
			}
		})
	}
}

// TestGoldensContactLineUsesVerbatimDisplayName is the LLM-547 register
// invariant on the NAME, and it guards a silent failure rather than an ugly one.
//
// resolveAddressee (sim/speak_commands.go) matches a peer by full display name
// or first name only, case-insensitively. An honorific — "Mistress Ward", where
// "Ward" is the surname — matches NOTHING, so the model's reply silently
// degrades from addressing her to addressing the whole huddle, and no error is
// ever raised. Period flavour therefore lives in the construction around the
// name ("had your word with"), never in the name itself.
//
// Asserting the verbatim name is PRESENT is the check that survives rewording:
// a future line that dresses the name up would drop the exact token and fail
// here, while any rephrasing that keeps the name intact passes.
func TestGoldensContactLineUsesVerbatimDisplayName(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			// Rebuild the expectation from the payload: every member carrying a
			// tier is a member whose verbatim name must appear in some line.
			type tiered struct {
				tier sim.ContactTier
				name string
			}
			var want []tiered
			for _, group := range [][]HuddleMember{p.Surroundings.HuddleMembers, p.Surroundings.CoPresent} {
				for _, m := range group {
					// Acquainted is part of the expectation, not an exception to
					// it: an unacquainted peer is rendered "a stranger" elsewhere
					// in this section, and this line cannot follow that gating
					// because it MUST carry the verbatim name. So the fact is
					// withheld along with the name, and such a member is owed no
					// line at all.
					if m.ContactTier != sim.ContactTierNone && m.DisplayName != "" && m.Acquainted {
						want = append(want, tiered{m.ContactTier, sanitizeInline(m.DisplayName)})
					}
				}
			}
			if len(want) == 0 {
				return
			}
			// The render caps the block, so only names that SURVIVE the cap are
			// owed a line. Select them with production's own ordering — loudest
			// tier first, then name — rather than by collection order: the two
			// coincide only while no scenario carries more facts than the cap, so
			// taking a prefix of the payload order would quietly assert the wrong
			// names the moment one does (code_review, LLM-547).
			sort.SliceStable(want, func(i, j int) bool {
				if want[i].tier != want[j].tier {
					return want[i].tier > want[j].tier
				}
				return want[i].name < want[j].name
			})
			if len(want) > maxRenderedContactLines {
				want = want[:maxRenderedContactLines]
			}
			joined := strings.Join(contactRecencyLines(p.Surroundings), "\n")
			for _, w := range want {
				if !strings.Contains(joined, w.name) {
					t.Errorf("scenario %q: no contact line carries the verbatim DisplayName %q — an "+
						"honorific or shortened form resolves to nobody in resolveAddressee and "+
						"silently degrades the reply to the whole huddle (LLM-547)", sc.name, w.name)
				}
			}
		})
	}
}

// TestGoldensNoDutySteerWhileOnRounds is the LLM-540 cross-scenario invariant:
// whenever the subject carries a constable route — ACTIVE or SUSPENDED — the
// rendered prompt must never argue him back to his post. The round is the single
// movement voice while it stands; a "make your way to the Meeting House" line beside
// "you broke off at the Ellis Farm" is two orders in one prompt, and a weak model
// takes the imperative.
//
// This is the property LLM-540 leans on. It restores the shift-duty WARRANT while a
// round is suspended — the wake without which he has no recurring tick at all — on
// the strength of the steer staying silent on its own (buildDutySteer yields for any
// constable RouteLabel). If a later change makes the steer render during a round,
// that restored warrant becomes the march LLM-531 removed, and this catches it
// matrix-wide rather than in the one fixture that happens to pin it.
//
// The marker is renderDutySteer's own production constant, not a copy of its
// wording — a reword that keeps the clause leaves the test matching, and one that
// changes it changes the constant too, so the invariant can't go quietly vacuous
// against a stale literal (the same reasoning as TestGoldensRainLineIffStorm
// computing through weatherProse).
func TestGoldensNoDutySteerWhileOnRounds(t *testing.T) {
	// Applicability is decided OUT here, not inside the subtest, so the vacuity
	// counter is not written from subtest bodies — that stays correct if these are
	// ever given t.Parallel().
	var onRounds []perceptionScenario
	for _, sc := range perceptionScenarios {
		snap, actorID, _ := sc.build()
		if a := snap.Actors[actorID]; a != nil && a.RouteLabel == sim.AttrConstable {
			onRounds = append(onRounds, sc)
		}
	}
	// Guard against the invariant going vacuous if the constable fixtures are ever
	// renamed or dropped: with no rounds scenario in the matrix it would pass green
	// while testing nothing.
	if len(onRounds) == 0 {
		t.Fatal("no scenario put the subject on a constable round — the no-duty-steer invariant is vacuous (LLM-540)")
	}
	for _, sc := range onRounds {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if out := renderScenario(sc); strings.Contains(out, dutySteerToPostMarker) {
				t.Errorf("scenario %q: subject is on his rounds but the prompt still steers him back to post (%q) — the round is the single movement voice while it stands (LLM-540)", sc.name, dutySteerToPostMarker)
			}
		})
	}
}

// TestDutySteerToPostMarkerMatchesRender is the anchor for the invariant above: it
// pins that renderDutySteer's to-work arm actually emits dutySteerToPostMarker. The
// invariant asserts an ABSENCE, so on its own it would pass for the wrong reason if
// the marker ever stopped appearing in the real line — this is the positive control
// that fails instead.
func TestDutySteerToPostMarkerMatchesRender(t *testing.T) {
	var b strings.Builder
	renderDutySteer(&b, &DutySteerView{ToWork: true, TargetID: "shop", TargetLabel: "General Store"})
	if got := b.String(); !strings.Contains(got, dutySteerToPostMarker) {
		t.Errorf("the to-work duty steer no longer contains %q, so the LLM-540 absence invariant matches nothing:\n%s", dutySteerToPostMarker, got)
	}
}

// TestGoldensWarrantTextCompleteOrMarked is the cross-scenario invariant behind
// LLM-396 and LLM-400: an untrusted free-text payload carried on a warrant — the
// words someone said, the note attached to a payment — reaches the reader's prompt
// either COMPLETE, or, if the renderer had to elide it, visibly marked with the
// elision marker. It is never a bare mid-word prefix.
//
// A bare prefix is not a cosmetic defect, it is a behavioral one. A listener shown
// "…they're about finding home in small" perceives an unfinished sentence and does
// the socially obvious thing: it asks the speaker to finish. The reply is clipped
// the same way, so a fresh dangling question is minted every single turn and the
// huddle cannot terminate — the live 13-minute Inn loop (Lewis / Silence / Hannah,
// 2026-07-14). It also defeats the "you already spoke, wait for their reply" guard,
// which can never be the most pressing matter while a question hangs unanswered. A
// clipped payment note is the quieter version of the same lie: the seller reads a
// reason that stops mid-clause as though the buyer had finished saying it.
//
// Stated matrix-wide rather than pinned in the goldens on purpose: both cuts lived
// a package away (handlers/speech_reactor.go, handlers/pay_reactor.go), not in the
// render path, so a scenario-local assertion would not have caught either. It is
// stated over warrant text in general, rather than per kind, so the next warrant
// that carries free text inherits the guarantee by joining the switch below —
// which is exactly what the pay path failed to do the first time around.
func TestGoldensWarrantTextCompleteOrMarked(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			out := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
			for _, w := range warrants {
				var text, kind string
				switch r := w.Reason.(type) {
				case sim.NPCSpeechWarrantReason:
					text, kind = r.Excerpt, "speech excerpt"
				case sim.PCSpeechWarrantReason:
					text, kind = r.Excerpt, "speech excerpt"
				case sim.PaidWarrantReason:
					text, kind = r.ForText, "payment for-text"
				default:
					continue // carries no untrusted free text — invariant N/A
				}
				// Empty text renders the payload-less form of the line ("X spoke to
				// you.", "X paid you N coins.") — nothing to elide.
				if strings.TrimSpace(text) == "" {
					continue
				}
				// Ask the render path itself what this text becomes, rather than
				// restating the cap — the invariant then tracks production and won't
				// drift if MaxBytesPerWarrant moves (same reasoning as the LLM-364
				// rain invariant computing through weatherProse).
				rendered, elided := sanitizeText(text, DefaultRenderConfig().MaxBytesPerWarrant)
				if !strings.Contains(out, rendered) {
					t.Fatalf("scenario %q: %s does not appear in the prompt at all:\n  want substring: %q", sc.name, kind, rendered)
				}
				if !elided {
					continue // the full text reached the reader — the good case
				}
				if !strings.HasSuffix(rendered, elisionMarker) {
					t.Errorf(
						"scenario %q: %s was elided but rendered as a bare prefix, with no %q marker — "+
							"the reader takes a cut sentence for a whole one. For speech that is the LLM-396 "+
							"conversation loop; for a payment note it is a reason the seller answers wrong. "+
							"Rendered tail: %q",
						sc.name, kind, elisionMarker, tailOf(rendered, 48),
					)
				}
			}
		})
	}
}

// tailOf returns the last n runes of s, for readable failure messages on long
// utterances (the head of a clipped line is never the interesting part — the cut
// is at the tail).
func tailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// TestGoldensTendNeedYieldsToEating is the LLM-276 cross-scenario invariant: whenever
// a tend-need warrant is present (the seek-work backstop redirected a workless idle
// worker with a resolvable hunger/thirst to eat), the rendered prompt must carry the
// tend-need felt pull and must NOT carry the go-earn seek-work directive — eating
// outranks job-hunting exactly as it does for a red need. Runs over the whole matrix so
// no future cue can reintroduce the labor directive for a redirected-to-eat worker.
func TestGoldensTendNeedYieldsToEating(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			_, _, warrants := sc.build()
			tendNeed := false
			for _, wm := range warrants {
				if _, ok := wm.Reason.(sim.TendNeedWarrantReason); ok {
					tendNeed = true
					break
				}
			}
			if !tendNeed {
				return // invariant N/A — no tend-need redirect in this scenario
			}
			out := renderScenario(sc)
			if strings.Contains(out, "offer your labor") {
				t.Errorf("scenario %q: tend-need active but the prompt still shows the seek-work go-earn line — eating must outrank job-hunting (LLM-276)", sc.name)
			}
			if !strings.Contains(out, "the means to see to it") {
				t.Errorf("scenario %q: tend-need active but the prompt lacks the tend-need felt pull (LLM-276)", sc.name)
			}
		})
	}
}

// TestGoldensSettledCloseNamesTheOffer is the LLM-296 cross-scenario invariant:
// every "## Recently settled offers" CLOSE line ("didn't go through") must name
// what the buyer OFFERED ("Your offer of ..."), never the thin want-item-only
// line that let two declines render byte-identically — so the standing "never
// repeat what you said" instruction had nothing to bind to and the model
// re-posted the same bundle. Runs over the whole matrix so a future edit to the
// close copy can't drop the offered bundle for any situation.
func TestGoldensSettledCloseNamesTheOffer(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, line := range strings.Split(renderScenario(sc), "\n") {
				if strings.Contains(line, "didn't go through") && !strings.Contains(line, "Your offer of ") {
					t.Errorf("scenario %q: a settled-offer close names no offered bundle (LLM-296):\n%s", sc.name, line)
				}
			}
		})
	}
}

// TestGoldensStandingOfferWithinOnHand is the LLM-409 cross-scenario render
// invariant: no rendered "## Offers you've put out" standing sell offer may quote
// more of an item than the seller holds ON HAND. This is the weaker, snapshot-
// checkable half of the production guarantee — the reconcile actually enforces
// the stronger reserved-aware bound (on-hand minus goods owed to a Ready order),
// which is unit-tested in TestReconcileQuoteCoverage_ReservedOrderMakesUncoverable
// (goldens don't carry Order state). What this pins across every fixture is that
// no situation, and no future render/build change, advertises a lot exceeding raw
// on-hand. Service kinds (lodging) carry no inventory and are exempt. Non-vacuous:
// seller_with_shortfall_lot_at_post and seller_with_taken_quote_at_post both
// render a standing offer, so the check exercises real lots.
func TestGoldensStandingOfferWithinOnHand(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			if len(p.StandingQuotesFromMe) == 0 {
				return // no standing offer here — invariant N/A
			}
			subject := snap.Actors[actorID]
			for _, q := range p.StandingQuotesFromMe {
				for _, ln := range q.Lines {
					if def := snap.ItemKinds[ln.ItemKind]; def != nil && def.HasCapability("service") {
						continue // a service lot has no inventory to fall short of
					}
					exercised = true
					onHand := 0
					if subject != nil {
						onHand = subject.Inventory[ln.ItemKind]
					}
					if ln.Qty > onHand {
						t.Errorf("scenario %q renders a standing offer of %d %s but the seller holds only %d (LLM-409: no lot may exceed on-hand)",
							sc.name, ln.Qty, ln.ItemKind, onHand)
					}
				}
			}
		})
	}
	if !exercised {
		t.Fatal("no scenario rendered a standing sell offer — the LLM-409 within-on-hand invariant is vacuous")
	}
}

// TestGoldensNoCoPresentBuyGoadAfterTwoDeclines is the LLM-308 cross-scenario invariant, spanning
// ALL co-present-buy cue families (restock, stall-repair nails, farm-upkeep shovels): whenever the
// subject has declined an item at least copresentStandoffDeclineThreshold times to a STILL-CO-PRESENT
// seller in its CURRENT huddle — at any point in the huddle's lifetime (LLM-510: the decline counter
// latches; only a fresh huddle resets it) — no rendered line may goad "Buy it now" for
// THAT (seller, item) pair. The scoping is seller-AND-item because coPresentBuyStandoff itself is
// seller-scoped: the same item can be legitimately goaded from a DIFFERENT co-present seller the
// subject has NOT stonewalled. So the assertion fires only when a "Buy it now" line carries both the
// stonewalled seller's rendered display name AND the exact `item "<kind>"` token — the two tokens
// every co-present goad emits (renderCoPresentBuy and renderRestocking's inline arm). Item-only would
// false-fail a valid two-seller prompt (code_review). This is the growth-loop backstop: the live sage
// loop was a restock cue re-firing "Buy it now … a qty up to 3" through 11 declines, and this asserts
// no un-generalized cue can recreate it, in any situation. The decline set mirrors coPresentBuyStandoff
// (Declined / insufficient-stock / insufficient-goods; a hard insufficient-funds is a coin block, not
// a terms standoff, so it is excluded here). Non-vacuous: the three *_standoff scenarios each drive the
// >=2-decline state, so the check exercises a real stonewalled seller. Caveat: matching on rendered
// display name is imperfect if two co-present sellers share a name; a strict version would inspect the
// built view/cue data, at the cost of the cross-family uniformity a single text scan gives.
func TestGoldensNoCoPresentBuyGoadAfterTwoDeclines(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, _ := sc.build()
		a := snap.Actors[actorID]
		if a == nil || a.CurrentHuddleID == "" || snap.PublishedAt.IsZero() {
			continue // no huddle to scope a standoff, or no clock to age the ledger against
		}
		// Count the subject's recent declines per item to each co-present seller in this huddle.
		type key struct {
			seller sim.ActorID
			item   sim.ItemKind
		}
		declines := map[key]int{}
		for _, e := range snap.PayLedger {
			if e == nil || e.BuyerID != actorID || e.HuddleID != a.CurrentHuddleID {
				continue
			}
			if e.ResolvedAt.IsZero() {
				continue // mid-construction — the cue skips these too (LLM-510: no recency filter on declines)
			}
			switch e.State {
			case sim.PayLedgerStateDeclined,
				sim.PayLedgerStateFailedInsufficientStock,
				sim.PayLedgerStateFailedInsufficientGoods:
				declines[key{e.SellerID, e.ItemKind}]++
			}
		}
		stonewalled := map[key]bool{}
		for k, n := range declines {
			if n < copresentStandoffDeclineThreshold {
				continue
			}
			// Only a seller STILL co-present in this huddle can be goaded, so scope the assertion to
			// those: a seller who declined then left can't be re-goaded, and keeping its departed name
			// in the set would let it shadow a legitimate goad of the same item by a different seller.
			if s := snap.Actors[k.seller]; s != nil && s.CurrentHuddleID == a.CurrentHuddleID {
				stonewalled[k] = true
			}
		}
		if len(stonewalled) == 0 {
			continue // invariant N/A — no co-present seller has stonewalled an item here
		}
		exercised = true
		for _, line := range strings.Split(renderScenario(sc), "\n") {
			if !strings.Contains(line, "Buy it now") {
				continue
			}
			for k := range stonewalled {
				s := snap.Actors[k.seller]
				if s == nil {
					continue
				}
				sellerName := sanitizeInline(s.DisplayName)
				if sellerName != "" && strings.Contains(line, sellerName) && strings.Contains(line, `item "`+string(k.item)+`"`) {
					t.Errorf("scenario %q: a co-present 'Buy it now' still goads %q from %q after >=%d declines in the current huddle (LLM-308):\n%s",
						sc.name, k.item, sellerName, copresentStandoffDeclineThreshold, line)
				}
			}
		}
	}
	if !exercised {
		t.Error("matrix must exercise a scenario with >=2 in-huddle declines to a co-present seller (LLM-308)")
	}
}

// TestGoldensRestockCueNeverHoldsOffOnLowStock is the LLM-424 cross-scenario invariant:
// every item in the "## Restocking" section is by definition below its reorder point, so
// the section must NEVER tell such a keeper to hold off buying on flow-velocity grounds —
// the old LLM-385 "restocking faster than it sells, so buy sparingly, if at all" overstock
// steer read exactly backwards on a sold-out, self-consumed line (the live Josiah cheese
// confabulation, where the model invented phantom unsold stock to reconcile an empty shelf
// against a "you're overstocked" cue). No rendered scenario may carry that hold-off.
// Non-vacuous: at least one scenario must render the self-use accounting that replaced it
// ("used or traded the rest"), so the check proves the reframed path is exercised, not
// merely that a phrase nobody emits is absent. True coin-poor overstock is a separate,
// Conserve-mode steer and is unaffected.
func TestGoldensRestockCueNeverHoldsOffOnLowStock(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		section := restockingSection(renderScenario(sc))
		if section == "" {
			continue // no "## Restocking" cue in this scenario — invariant N/A
		}
		if strings.Contains(section, "buy sparingly") || strings.Contains(section, "restocking faster than it sells") {
			t.Errorf("scenario %q: the '## Restocking' cue holds off buying on a low-stock item (LLM-424):\n%s", sc.name, section)
		}
		if strings.Contains(section, "used or traded the rest") {
			exercised = true
		}
	}
	if !exercised {
		t.Error("matrix must exercise a scenario whose '## Restocking' cue renders the self-use accounting (LLM-424 'used or traded the rest')")
	}
}

// TestGoldensRestockCueNeverPairsAggregateCostWithSales is the LLM-427 cross-scenario
// invariant: the "## Restocking" cue must never render the aggregate weekly P&L pair —
// coins paid for EVERYTHING bought this window against revenue from ONLY the units that
// sold ("at a cost of X coins and sales of Y coins"). The two figures were never
// apples-to-apples, and the model subtracted them into a phantom loss on any good bought
// faster than it sells — the live Josiah Thorne cheese refusal ("a fool's arithmetic I
// don't mean to repeat") on a line that earned a coin per unit. Demand renders as a
// graded judgment and per-unit economics as a margin verdict instead. Non-vacuous both
// ways: at least one scenario must render a demand clause and at least one a margin
// verdict, so the check proves the replacement path is exercised, not merely that a
// phrase nobody emits is absent.
func TestGoldensRestockCueNeverPairsAggregateCostWithSales(t *testing.T) {
	var demandExercised, marginExercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		section := restockingSection(renderScenario(sc))
		if section == "" {
			continue // no "## Restocking" cue in this scenario — invariant N/A
		}
		if strings.Contains(section, "at a cost of") || strings.Contains(section, "and sales of") {
			t.Errorf("scenario %q: the '## Restocking' cue pairs aggregate bought-cost with sold-revenue (LLM-427):\n%s", sc.name, section)
		}
		// Deliberately the tier phrasings, not "this past week" — the LLM-424
		// self-use line carries that fragment too and must not satisfy this.
		if strings.Contains(section, "It sells slowly") ||
			strings.Contains(section, "It sells steadily") ||
			strings.Contains(section, "It sells briskly") {
			demandExercised = true
		}
		if hasRestockMarginVerdict(section) {
			marginExercised = true
		}
	}
	if !demandExercised {
		t.Error("matrix must exercise a scenario whose '## Restocking' cue renders a demand judgment (LLM-427 'It sells … this past week')")
	}
	if !marginExercised {
		t.Error("matrix must exercise a scenario whose '## Restocking' cue renders a margin verdict (LLM-427)")
	}
}

// restockMarginVerdictLabels are the trailing verdict labels of the five LLM-492 margin
// clauses. Every clause shares one shape — "You are buying at about X and selling at
// about Y — <label>." — so the label alone identifies a verdict, which lets
// TestGoldensRestockMarginVerdictAlwaysStatesItsRates assert the rate half separately
// rather than assume it. A detector keyed on the WHOLE clause could not: it would go
// silent the moment someone emitted a bare label, which is the regression that test
// exists to catch.
//
// One list, package-level, because there were two hand-maintained copies of it — an
// inline one in TestGoldensRestockCueNeverPairsAggregateCostWithSales and a closure in
// TestRestockBuyAnchorRendersWhenRateKnown — and a rewording satisfied one while
// breaking the other. Every invariant that needs to know whether a line carries a
// margin verdict reads this.
var restockMarginVerdictLabels = []string{
	"— highly profitable on coin.",
	"— nicely profitable on coin.",
	"— slightly profitable on coin.",
	"— breakeven on coin; you need to buy lower or sell for more.",
	"— losing on coin; you need to buy lower or sell for more.",
}

// hasRestockMarginVerdict reports whether s carries any of the margin-tier clauses.
// Takes a whole section or a single line.
func hasRestockMarginVerdict(s string) bool {
	for _, label := range restockMarginVerdictLabels {
		if strings.Contains(s, label) {
			return true
		}
	}
	return false
}

// TestGoldensRestockMarginVerdictAlwaysStatesItsRates pins LLM-492: a margin verdict
// never appears without the two rates it judges, and the retired wording that
// generalised past those rates never comes back.
//
// The sibling of TestGoldensMakingsVerdictAlwaysHasACostFigure on the wares cue, and it
// exists for the same reason. Both rates come from the coin price book, which excludes
// pure-barter settlements by design (ZBBS-HOME-393 — booking a zero Amount would enter a
// free reading and poison every derived rate). Village-wide barter runs about a third of
// settlements, concentrated in the raw and intermediate goods this section governs, so a
// verdict floated free of its numbers would assert the whole economics of a line the
// book only partly observed. Welding the rates to the label bounds the claim to what was
// actually seen.
//
// Live case: a distributor was told wheat "earns you nothing on its own" and declined to
// restock it on eight consecutive ticks, quoting the cue's own reasoning back at it —
// while that wheat was the input leg of a working trade, swapped roughly 1:1 to the mill
// for flour he resold at double. 61% of his wheat volume was barter and therefore
// invisible to the verdict. The fix is NOT a barter-derived number: nothing available
// here can price the goods leg, and inventing one would repeat the LLM-475 defect in a
// new place (uncertainty stays silent).
func TestGoldensRestockMarginVerdictAlwaysStatesItsRates(t *testing.T) {
	// The exact retired clauses. Either reappearing means the verdict has gone back to
	// claiming more than the coin legs establish.
	retired := []string{
		"it earns you nothing on its own",
		"Each one earns you coin",
	}
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		section := restockingSection(renderScenario(sc))
		if section == "" {
			continue // no "## Restocking" cue in this scenario — invariant N/A
		}
		for _, phrase := range retired {
			if strings.Contains(section, phrase) {
				t.Errorf("scenario %q: '## Restocking' carries the retired margin wording %q, which asserts more than the coin legs establish (LLM-492):\n%s",
					sc.name, phrase, section)
			}
		}
		for _, line := range strings.Split(section, "\n") {
			// Detect on the RATE half, not the label, so both of the assertions below
			// are real: keying detection on the label would make the scope check
			// tautological (every label contains "on coin") and would go silent on a
			// line that stated rates and then drew an unscoped conclusion.
			if !strings.Contains(line, "You are buying at about ") {
				continue
			}
			exercised = true
			if !strings.Contains(line, " and selling at about ") {
				t.Errorf("scenario %q: item line %q states a buy rate without the sell rate it is judged against (LLM-492)", sc.name, line)
			}
			// The scope has to reach the MODEL (code_review): a bare "breakeven" reads
			// as a claim about the good, which is the inference this ticket exists to
			// stop. Both rates come from the coin price book and nothing else.
			if !strings.Contains(line, "on coin") {
				t.Errorf("scenario %q: item line %q renders a margin verdict without scoping it to the coin trade (LLM-492)", sc.name, line)
			}
			if !hasRestockMarginVerdict(line) {
				t.Errorf("scenario %q: item line %q states both rates but carries no recognised verdict label (LLM-492)", sc.name, line)
			}
		}
	}
	if !exercised {
		t.Error("matrix must exercise a scenario whose '## Restocking' cue renders a margin verdict, or this invariant is vacuous (LLM-492)")
	}
}

// restockingSection returns the body of the "## Restocking" section of a rendered
// prompt — from its header to the next "## " header (or end of prompt) — or "" when the
// prompt has no such section. Scoping the LLM-424 invariant to this slice keeps a phrase
// in an unrelated section from either false-failing the hold-off check or vacuously
// satisfying the self-use non-vacuity guard (code_review).
func restockingSection(prompt string) string {
	const header = "## Restocking\n"
	i := strings.Index(prompt, header)
	if i < 0 {
		return ""
	}
	body := prompt[i+len(header):]
	if j := strings.Index(body, "\n## "); j >= 0 {
		return body[:j]
	}
	return body
}

// TestGoldensEnRouteWorkerNotOfferedNewWork is the LLM-229 cross-scenario
// invariant: whenever the subject is a WORKER relocating to an accepted job (an
// EnRoute LaborOffer with the subject as worker), the rendered prompt must offer
// neither the solicit affordance nor the businesses directory — the worker is
// already committed, and a second job would strand the first. Runs over the
// whole matrix so a future cue can't reintroduce work-seeking for a committed
// relocating worker in any situation, not just the one worker_en_route_to_workplace
// scenario pins.
func TestGoldensEnRouteWorkerNotOfferedNewWork(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			enRoute := false
			for _, o := range snap.LaborLedger {
				if o != nil && o.State == sim.LaborStateEnRoute && o.WorkerID == actorID {
					enRoute = true
					break
				}
			}
			if !enRoute {
				return // subject isn't relocating to a job — invariant N/A here
			}
			out := renderScenario(sc)
			if strings.Contains(out, "make the offer with solicit_work") {
				t.Errorf("scenario %q: subject is relocating to an accepted job but the prompt still offers the solicit affordance (LLM-229)", sc.name)
			}
			if strings.Contains(out, "head to one of the town's businesses") {
				t.Errorf("scenario %q: subject is relocating to an accepted job but the prompt still shows the seek-work businesses directory (LLM-229)", sc.name)
			}
		})
	}
}

// TestGoldensUnansweredRequestIsNeverToolless is the LLM-346 cross-scenario
// invariant: an actor facing an unanswered direct request always has a way to
// answer it. Concretely — in ANY scenario where a Pending labor offer awaits the
// subject's answer, the rendered prompt must name at least one answer tool and
// must carry that offer's id, the token the model echoes back into it.
//
// "At least one" is the honest bar, not "both". An employer who cannot cover the
// wage is deliberately steered to decline_work alone (LLM-158): accepting would
// only flip the offer to failed_unavailable, so naming the accept in prose would
// coach a doomed call. What must never happen is a rendered obligation with NO
// mechanical answer at all. Where the subject CAN say yes — a worker weighing an
// offered job, whose purse is nobody's concern — accept_work must be named too, so
// that a refusal is a choice rather than the only door out.
//
// This is the discussion-109 lockstep stated as a property rather than a wiring
// detail: the tools are gated on the same standing view this section renders from
// (gateTools reads perception.PendingLaborOffers), so a prompt describing an offer
// it cannot answer means the two have drifted, and a missing id means the model
// holds tools it cannot address.
//
// The bug it exists to catch is the one that filed the ticket. Lewis Walker's
// deliberation prompt quoted Prudence Ward's request back to him verbatim and
// advertised no work tool at all. Any future gate that silences a responder (a
// comfort ceiling, an evening-leisure suppression, a coin threshold) reproduces it
// exactly, and trips here first.
func TestGoldensUnansweredRequestIsNeverToolless(t *testing.T) {
	var exercisedSolicited, exercisedOffered bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			offers := PendingLaborOffers(p)
			if len(offers) == 0 {
				return // nobody is waiting on this actor — invariant N/A here
			}
			out := renderScenario(sc)
			if !strings.Contains(out, "accept_work") && !strings.Contains(out, "decline_work") {
				t.Errorf("scenario %q: a labor offer awaits the subject's answer but the prompt names neither accept_work nor decline_work — the request is rendered with no way to answer it (LLM-346)", sc.name)
			}
			for _, o := range offers {
				if o.SubjectIsWorker() {
					exercisedOffered = true
					if !strings.Contains(out, "accept_work") {
						t.Errorf("scenario %q: an employer offered the subject a job but the prompt never names accept_work — a worker who can say yes must be able to (LLM-346)", sc.name)
					}
				} else {
					exercisedSolicited = true
				}
				if id := "offer id " + strconv.FormatUint(uint64(o.LaborID), 10); !strings.Contains(out, id) {
					t.Errorf("scenario %q: offer %d awaits the subject's answer but the prompt never carries %q — the model cannot address the tools it was given (LLM-346)", sc.name, o.LaborID, id)
				}
			}
		})
	}
	// Both directions must actually be walked, or the invariant silently covers
	// only the half that existed before LLM-346.
	if !exercisedSolicited {
		t.Error("matrix must exercise an employer answering a solicited offer (LLM-346)")
	}
	if !exercisedOffered {
		t.Error("matrix must exercise a worker answering an offered job (LLM-346)")
	}
}

// TestGoldensLaboringPeerNotAnOfferTarget is the LLM-231 cross-scenario invariant: a
// co-present huddle peer fulfilling a hired job (a Working LaborOffer with the peer as
// worker) is never surfaced as an offerable customer — a worker mid-job is not a valid
// sale target. The laboring set is recomputed from the raw LaborLedger (laboringOfferFor),
// NOT from HuddleMember.Laboring, so it independently asserts buildOfferableCustomers drops
// them rather than pinning the cue against its own flag. Requires the matrix to exercise
// the exclusion at least once (a seller scenario with a laboring co-present peer).
func TestGoldensLaboringPeerNotAnOfferTarget(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, warrants := sc.build()
		p := Build(snap, actorID, warrants)
		if p.OfferableCustomers == nil {
			continue // subject isn't a seller with co-present customers — invariant N/A here
		}
		for _, m := range p.Surroundings.HuddleMembers {
			if laboringOfferFor(snap, m.ID) == nil {
				continue
			}
			exercised = true
			label := descriptorLabel(m.DisplayName, m.Role, m.Acquainted)
			for _, name := range p.OfferableCustomers.CustomerNames {
				if name == label {
					t.Errorf("scenario %q: laboring peer %q surfaced as an offerable customer — a worker mid-job is not a pitch target (LLM-231)", sc.name, label)
				}
			}
		}
	}
	if !exercised {
		t.Error("matrix must exercise a seller scenario with a laboring co-present peer (LLM-231)")
	}
}

// TestGoldensConversationLinesCarryIntervalStamps is the LLM-217 cross-scenario
// invariant: in any scenario whose snapshot carries a clock (PublishedAt set —
// every clocked fixture stamps its utterances relative to it), every line of
// "## Recent conversation here" must carry an interval stamp ("(just now)" /
// "(40s ago)"). The stamp is what lets the model tell rapid-fire churn from a
// normally paced exchange (the Patience Walker go-home ↔ seek-work loop read as
// one continuous moment without it); a future cue path that builds UtteranceView
// without At — or a render change that drops the stamp — fails here for every
// affected scenario, not just the one the LLM-217 golden pins.
func TestGoldensConversationLinesCarryIntervalStamps(t *testing.T) {
	stamped := regexp.MustCompile(`\((just now|\d+[smh] ago)\): `)
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, _, _ := sc.build()
			if snap.PublishedAt.IsZero() {
				return // clockless fixture — stamps are correctly omitted
			}
			out := renderScenario(sc)
			_, section, found := strings.Cut(out, "## Recent conversation here\n")
			if !found {
				return // no conversation section in this situation
			}
			section, _, _ = strings.Cut(section, "\n\n")
			for _, line := range strings.Split(section, "\n") {
				if !stamped.MatchString(line) {
					t.Errorf("scenario %q: conversation line %q carries no interval stamp — the model can't gauge tempo without it (LLM-217)", sc.name, line)
				}
			}
		})
	}
}

// TestGoldensRestockNeverTargetsRememberedShutSupplier is the LLM-216 cross-scenario
// invariant: within the "## Restocking" section of any scenario, a structure the
// subject remembers finding shut (a live ObservedClosed memory) must never appear as
// a "(destination: <id>)" walk-to target. A shut supplier is a dead end the weak
// model toured on (Josiah's every-tick move_to loop among shut farms), so the restock
// builder DROPS it rather than annotating it. Runs over the whole matrix so a future
// restock cue change can't reintroduce a shut supplier as a target for any situation,
// not just the one keeper_restock_drops_shut_keeps_open_supplier scenario pins.
// Non-vacuous: that scenario renders a restock section while remembering James Farm
// shut, so the check actually exercises a shut structure.
func TestGoldensRestockNeverTargetsRememberedShutSupplier(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			_, section, found := strings.Cut(renderScenario(sc), "## Restocking\n")
			if !found {
				return // no restock section in this situation — invariant N/A here
			}
			// Bound the scan to the restock section by cutting at the next markdown
			// header, NOT the first blank line — a future intra-section blank line would
			// otherwise hide a bad remembered-shut target lower in the same section
			// (code_review). The section runs to the next "## " or end of prompt.
			if idx := strings.Index(section, "\n## "); idx >= 0 {
				section = section[:idx]
			}
			for structureID := range snap.Structures {
				if !businessRememberedShut(snap, a, structureID) {
					continue
				}
				token := "(destination: " + string(structureID) + ")"
				if strings.Contains(section, token) {
					t.Errorf("scenario %q: the restock section advertises remembered-shut supplier %q as a move target — a shut supplier is a dead end and must be dropped (LLM-216)", sc.name, token)
				}
			}
		})
	}
}

// TestGoldensRoleNeedNeverRendersSilent is the LLM-406 cross-scenario invariant — the
// role-liveness rule: an actor with an unmet ROLE obligation that the engine can say
// something true and useful about is never handed a turn that says nothing about it.
//
// Concretely, for the restock obligation: if the subject holds a `buy` entry below the
// reorder point AND the engine can name at least one supplier of that item — a payable
// one, a co-present one, or merely a BLOCKED one — then the prompt must carry a
// "## Restocking" section. What the section says is up to the situation: go buy, hold
// off and sell down first, or "you are stuck, here is who has it and here is why you
// can't have it". What it may not do is stay silent.
//
// Silence is the bug this ticket exists for. The live Josiah Thorne stood at his post
// with an unmet obligation, a supplier for every item of it, and NOT ONE WORD in his
// prompt — no cue, no warrant, no way to reason about his own deadlock. He was not
// erroring and not looping; he simply could not recover, and nothing surfaced it. And
// because the wholesale tier made him the farms' only lawful buyer, his silence jammed
// the entire village's retail food chain behind him.
//
// The invariant is scoped to what the engine can actually SAY: an item literally nobody
// sells anywhere yields no supplier, and a line for it would be a dead-end want the
// weak model improvises a destination for (the Hannah Boggs phantom fetch-water hires,
// LLM-260). That case is exempt by construction — there is nothing to name.
//
// Exempt too are the situations where the engine drops the section by DESIGN, each of
// which is a considered decision rather than an oversight:
//   - restock disabled outright (RestockReorderPct == 0, the operator off-switch);
//   - a degraded business (LLM-304) — it cannot turn a buy into shelf stock until it is
//     mended, and the "## Your business" cue carries that obligation instead;
//   - a degeneracy-flagged actor (LLM-94) — the place-naming movement steers are thinned
//     precisely because it is in a futile move loop;
//   - a keeper settled at the tavern for the evening — off the clock.
func TestGoldensRoleNeedNeverRendersSilent(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			// Build ONCE and render THAT snapshot. Deciding whether the invariant applies
			// from one build and then rendering a second one (via renderScenario, which
			// rebuilds) would let the test diagnose a different world than the one it
			// inspected — the fixtures are deterministic today, so it would agree, but it
			// is the wrong thing to be relying on (code_review).
			snap, actorID, warrants := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || subj.RestockPolicy == nil || snap.RestockReorderPct <= 0 {
				return // no role policy, or the producer is switched off — invariant N/A
			}
			if ownerBusinessDegraded(snap, actorID) || degeneracyFlagged(subj) || settledAtLeisureVenue(snap, subj) {
				return // the section is dropped by design in these situations — see the doc above
			}
			floors := sim.ReorderFloors(snap.Recipes, subj.RestockPolicy)
			for _, e := range sim.EffectiveBuyEntries(snap.Recipes, subj.RestockPolicy) {
				if !sim.RestockReorderThresholdMet(subj.Inventory[e.Item], e.Cap(), snap.RestockReorderPct, floors[e.Item]) {
					continue // not low — no unmet obligation for this item
				}
				// Can the engine name ANYONE for this item — a destination, a seller in
				// the room, or a supplier it must explain away? If not, there is nothing
				// truthful to say and silence is correct.
				vendors, blocked := findItemVendors(snap, actorID, subj, e.Item)
				coName, _ := coPresentSellerForItem(snap, actorID, subj, e.Item)
				if len(vendors) == 0 && len(blocked) == 0 && coName == "" {
					continue
				}
				out := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
				if !strings.Contains(out, "## Restocking") {
					t.Errorf("scenario %q: subject is low on %q and the engine can name a supplier for it "+
						"(payable=%d blocked=%d coPresent=%q), yet the prompt carries no '## Restocking' section at all. "+
						"An unmet role obligation must never render silent (LLM-406):\n%s",
						sc.name, e.Item, len(vendors), len(blocked), coName, out)
					return
				}
			}
		})
	}
}

// TestGoldensNeverCoachSpeakingAtCompany is the LLM-220 cross-scenario
// invariant: no rendered situation coaches the actor to speak at whoever is
// present. The old co-presence clause ("— speak to start conversing with them")
// fired on every arrival and pushed NPCs into unprompted monologues at any
// co-present actor, PCs included (the live Josiah-at-the-Tavern cold-open).
// Naming the company is legibility; telling the actor to speak is compulsion.
func TestGoldensNeverCoachSpeakingAtCompany(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if strings.Contains(out, "speak to start conversing") {
				t.Errorf("scenario %q: co-presence line coaches speaking — presence must be stated neutrally (LLM-220):\n%s", sc.name, out)
			}
		})
	}
}

// TestGoldensNonDistributorRestockNeverTargetsFarm is the LLM-223 cross-scenario
// invariant: within the "## Restocking" section of any scenario whose subject is
// NOT the village distributor, a farm-tagged structure must never appear as a
// "(destination: <id>)" walk-to target. Farm-origin goods flow farms → distributor
// → everyone else, so perception routes a non-distributor's restock through the
// distributor, never straight to a farm the PayWithItem backstop would refuse. Runs
// over the whole matrix so a future restock/vendor cue change can't reintroduce a
// farm as a target for any non-distributor situation. Non-vacuous: the
// reseller_restock_routed_to_distributor_not_farm scenario renders a restock section
// with a farm-tagged milk supplier present in the fixture.
func TestGoldensNonDistributorRestockNeverTargetsFarm(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || sim.ActorIsDistributor(snap.VillageObjects, a.WorkStructureID) {
				return // subject is the distributor (or missing) — invariant N/A here
			}
			// LLM-477: a TRANSFORMER legitimately targets a wholesale source for its own
			// production inputs, so a farm destination is no longer categorically wrong
			// for it. This text-level scan can't tell WHICH item a destination serves, so
			// the transformer case is held instead by the item-precise structural
			// invariant TestGoldensWholesaleSourceOnlyForAllowedItems below.
			if sim.BuyerWholesaleAllowance(snap.VillageObjects, snap.Recipes, a.WorkStructureID, a.RestockPolicy).Any() {
				return
			}
			_, section, found := strings.Cut(renderScenario(sc), "## Restocking\n")
			if !found {
				return // no restock section in this situation — invariant N/A here
			}
			// Bound the scan to the restock section — cut at the next markdown header
			// so a farm id lower in the prompt (a home/work anchor) can't false-positive.
			if idx := strings.Index(section, "\n## "); idx >= 0 {
				section = section[:idx]
			}
			for id, obj := range snap.VillageObjects {
				if !sim.IsFarmStructure(obj) {
					continue
				}
				token := "(destination: " + string(id) + ")"
				if strings.Contains(section, token) {
					t.Errorf("scenario %q: the restock section advertises farm %q as a move target for a non-distributor — farm goods must route through the distributor (LLM-223)", sc.name, token)
				}
			}
		})
	}
}

// TestGoldensWholesaleSourceOnlyForAllowedItems is the LLM-477 cross-scenario
// invariant, and the item-precise half of the wholesale tier's perception guarantee:
// across the whole matrix, whenever the RESTOCK DIRECTORY advertises a
// wholesaler-tagged structure as a supplier of an item, the subject must actually be
// entitled to buy THAT item there — it is the distributor, or the item is one of its
// own production inputs. Re-derived from the snapshot rather than trusting the filter
// that produced the vendor list, so a future change to eachVendorOffer can't quietly
// widen the grant.
//
// This is what keeps the tier honest now that TestGoldensNonDistributorRestockNeverTargetsFarm
// exempts transformers: that text-level scan can only ask "is a farm named at all",
// while this one asks the question that matters — "named for WHAT".
//
// SCOPE (code_review): this walks the restock-directory surface only — it iterates the
// subject's buy demand and checks findItemVendors. It does NOT cover the satiation /
// consumption path or the co-present peer path, which ride the same allowance but are
// reached through different builders. Those two are held by the focused
// TestWholesaleGate_Transformer subtests instead; if this invariant is ever widened to
// claim all four enforcement points, it has to grow assertions over those builders too.
func TestGoldensWholesaleSourceOnlyForAllowedItems(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || subj.RestockPolicy == nil {
				return // no restock manifest — invariant N/A here
			}
			allowance := sim.BuyerWholesaleAllowance(snap.VillageObjects, snap.Recipes, subj.WorkStructureID, subj.RestockPolicy)
			for _, e := range sim.EffectiveBuyEntries(snap.Recipes, subj.RestockPolicy) {
				vendors, _ := findItemVendors(snap, actorID, subj, e.Item)
				for _, vd := range vendors {
					if !sim.SellerAtWholesaler(snap.VillageObjects, vd.StructureID) {
						continue // an ordinary vendor — the tier says nothing about it
					}
					if !allowance.Allows(e.Item) {
						t.Errorf("scenario %q: the restock directory advertises wholesale source %q for %q, but the subject holds no wholesale allowance for that item — wholesale-origin goods must route through the distributor unless the item feeds the subject's own production (LLM-477)", sc.name, vd.StructureID, e.Item)
					}
				}
			}
		})
	}
}

// TestGoldensRestockSupplierProducesOrForagesOrIsDistributor is the LLM-252
// cross-scenario invariant: every supplier the restock directory advertises for a
// low `buy` item must supply that item at first hand — some vendor stationed there
// produces or forages it — or be the distributor. A structure whose only vendors of
// the item hold it via a past `buy` (a fellow reseller) must never surface: that is
// the Josiah↔John carrot buy-back. Re-derived from snap.Actors so it independently
// confirms findItemVendors' output rather than trusting the gate that produced it.
func TestGoldensRestockSupplierProducesOrForagesOrIsDistributor(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || subj.RestockPolicy == nil {
				return // no reseller restock manifest — invariant N/A here
			}
			// Effective demand (LLM-260) so derived buy inputs are held to the
			// same supplier invariant as hand-authored entries.
			for _, e := range sim.EffectiveBuyEntries(snap.Recipes, subj.RestockPolicy) {
				vendors, _ := findItemVendors(snap, actorID, subj, e.Item)
				for _, vd := range vendors {
					if sim.ActorIsDistributor(snap.VillageObjects, vd.StructureID) {
						continue // the distributor is a standing supplier of everything
					}
					firstHand := false
					for _, a := range snap.Actors {
						if a != nil && a.WorkStructureID == vd.StructureID && a.RestockPolicy.ProducesOrForages(e.Item) {
							firstHand = true
							break
						}
					}
					if !firstHand {
						t.Errorf("scenario %q: restock directory advertises %q as a supplier of %q, but no vendor there produces/forages it and it is not the distributor — a reseller holding bought stock must not be a supplier (LLM-252)", sc.name, vd.StructureID, e.Item)
					}
				}
			}
		})
	}
}

// TestGoldensConserveKeeperNeverGetsBuyImperative is the LLM-294 cross-scenario
// invariant: whenever the subject is in conserve mode (coin-poor + overstocked, per
// merchantConserve), its "## Restocking" section must lead with the hold-off-buying
// steer and must NEVER carry a "Buy it now" imperative — the cue cannot tell a keeper
// to conserve and to buy in the same breath, even with a seller co-present. Runs over
// the whole matrix so no future cue can reintroduce the buy imperative for a conserving
// keeper.
func TestGoldensConserveKeeperNeverGetsBuyImperative(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || !merchantConserve(snap, actorID, subj).Active {
				return // invariant N/A — subject isn't conserving in this scenario
			}
			out := renderScenario(sc)
			// Isolate the "## Restocking" section: from its header to the next section
			// header (or end). The conserve steer is scoped to this section.
			const header = "## Restocking\n"
			idx := strings.Index(out, header)
			if idx < 0 {
				return // no restock section rendered (nothing low to buy) — nothing to assert
			}
			section := out[idx+len(header):]
			if next := strings.Index(section, "\n## "); next >= 0 {
				section = section[:next]
			}
			if !strings.Contains(section, "Hold off buying") {
				t.Errorf("scenario %q: subject is conserving but the Restocking section lacks the hold-off-buying steer:\n%s", sc.name, section)
			}
			if strings.Contains(section, "Buy it now") {
				t.Errorf("scenario %q: subject is conserving but the Restocking section still carries a 'Buy it now' imperative (LLM-294):\n%s", sc.name, section)
			}
		})
	}
}

// TestGoldensConserveLowItemAlwaysSelfResolves is the LLM-298 cross-scenario
// invariant: whenever the subject is conserving, every "- You are low on …" bullet in
// its "## Restocking" section must state what to do INSTEAD (the no-errand-now steer),
// never a bare lack. Conserve strips the co-present imperative and the walk-to list, so
// a bare "You are low on X" names a want with no outlet — the vacuum llama-3.3-70b
// filled by inventing a nonexistent "Market" to move_to (live scene 019f38de). The
// per-item steer closes the want even on a restock-wakeup turn that points at this
// section. The complementary non-conserve guarantee (a low item always carries a named
// destination/seller) is structural: buildRestocking omits any item with neither a
// co-present seller nor a walk-to vendor, so no bare non-conserve line can exist. Runs
// over the whole matrix so no future cue can reintroduce the dangling want.
func TestGoldensConserveLowItemAlwaysSelfResolves(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || !merchantConserve(snap, actorID, subj).Active {
				return // invariant N/A — subject isn't conserving in this scenario
			}
			out := renderScenario(sc)
			const header = "## Restocking\n"
			idx := strings.Index(out, header)
			if idx < 0 {
				return // no restock section rendered (nothing low to buy) — nothing to assert
			}
			section := out[idx+len(header):]
			if next := strings.Index(section, "\n## "); next >= 0 {
				section = section[:next]
			}
			for _, line := range strings.Split(section, "\n") {
				if !strings.HasPrefix(line, "- You are low on ") {
					continue // co-present standing-offer line etc. carry their own steer
				}
				if !strings.Contains(line, "no errand for it now") {
					t.Errorf("scenario %q: conserve Restocking names a low item with no no-errand-now steer (LLM-298 dangling want):\n%s", sc.name, line)
				}
			}
		})
	}
}

// TestGoldensConserveNoProductionInputsNag is the LLM-298 Phase 3 cross-scenario
// invariant: a conserving subject (coin-poor + overstocked) never renders the
// "## Keeping up production" section. That section is pure buy-motivation for a low
// input, but conserve tells the keeper to hold off buying and sell down — so a
// "running low on X" line there dangles a second want with no legal outlet (the live
// sage→stew produce-retry nag). Runs over the whole matrix so no future path
// reintroduces it for a conserving keeper.
func TestGoldensConserveNoProductionInputsNag(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || !merchantConserve(snap, actorID, subj).Active {
				return // invariant N/A — subject isn't conserving in this scenario
			}
			if out := renderScenario(sc); strings.Contains(out, "## Keeping up production") {
				t.Errorf("scenario %q: conserving subject still gets the '## Keeping up production' nag (LLM-298):\n%s", sc.name, out)
			}
		})
	}
}

// TestGoldensUnobtainableInputSurfacesNoDemand is the LLM-260 cross-scenario
// invariant: an effective buy item (explicit or derived from a produce recipe)
// that NO other actor in the world holds at a workplace must surface in NEITHER
// demand section — no "## Restocking" line, no "## Keeping up production"
// runway/booster line. "Nobody holds it anywhere" is the loosest possible
// vendor superset (every gate variant — wholesale, LLM-252 first-hand, LLM-216
// drops — can only shrink it), so the check is independent of the gates that
// produced the render: if the item genuinely has no source, ANY demand line for
// it is a dead-end cue the model would improvise on (the live Hannah Boggs
// phantom fetch-water hires).
func TestGoldensUnobtainableInputSurfacesNoDemand(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			subj := snap.Actors[actorID]
			if subj == nil || subj.RestockPolicy == nil {
				return // no restock manifest — invariant N/A here
			}
			prompt := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
			for _, e := range sim.EffectiveBuyEntries(snap.Recipes, subj.RestockPolicy) {
				held := false
				for id, a := range snap.Actors {
					if id == actorID || a == nil || a.WorkStructureID == "" {
						continue
					}
					if a.Inventory[e.Item] > 0 {
						held = true
						break
					}
				}
				if held {
					continue // someone holds it — obtainability is the gated cues' call
				}
				label := itemDisplayLabel(snap, e.Item)
				for _, header := range []string{"## Restocking", "## Keeping up production"} {
					if strings.Contains(promptSection(prompt, header), label) {
						t.Errorf("scenario %q: %s names %q, but no actor in the world holds it at a workplace — an unobtainable item must surface no demand (LLM-260)", sc.name, header, label)
					}
				}
			}
		})
	}
}

// promptSection returns the body of the markdown section starting at header,
// cut at the next "## " heading; "" when the section is absent.
func promptSection(prompt, header string) string {
	idx := strings.Index(prompt, header)
	if idx < 0 {
		return ""
	}
	section := prompt[idx+len(header):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section
}

// perceptionScenarios is the (growing) matrix. Seeded from LLM-106 with two
// situations: a keeper alone at its post, and a tired keeper on shift at its post.
// Each new live (a)-class failure should add a scenario here (and, where it states
// a property over the whole matrix, a cross-scenario invariant test).
var perceptionScenarios = []perceptionScenario{
	{
		name: "listener_hears_long_utterance",
		summary: "LLM-396: a listener perceives a 372-rune utterance — the real line Silence Walker spoke " +
			"in the Inn on 2026-07-14, which Lewis received clipped mid-word at \"…finding home in small\" and " +
			"answered by asking her to finish the sentence, a loop that ran unbroken for 13 minutes. The golden " +
			"pins that the FULL line now reaches the listener (no bare mid-word prefix, no silent elision) and " +
			"that it renders exactly ONCE — the warrant carries the whole text while the ring/SalientFact copies " +
			"stay in the 220-rune form, the exact case that would break the ZBBS-WORK-374 same-tick dedup and " +
			"show the model the same line twice. Matrix guard: TestGoldensSpeechExcerptCompleteOrMarked.",
		build: listenerHearsLongUtterance,
	},
	{
		name: "lingering_inn_conversation",
		summary: "LLM-397: the live 2026-07-14 inn scene (hud-e26338a9…) past its wind-down window — Lewis, " +
			"Hannah and Silence, an hour into a conversation that bought and ate a bowl of porridge and moved " +
			"on to a widow's memories of her husband. It is NOT a loop: no repetition, and the sale reset the " +
			"endurance counter, so only the lingering clock can see it. The golden pins the register the engine " +
			"uses on a healthy conversation it merely wants to end — a graceful farewell, the owed-reply nag " +
			"suppressed, and pointedly NOT the endurance line's 'nothing new is coming of it', which here would " +
			"be false. The eco arc this replaced simply severed this scene mid-sentence, ten times in a hundred " +
			"minutes. Matrix guard: TestGoldensLingeringNeverClaimsFruitlessness.",
		build: lingeringInnConversation,
	},
	{
		name: "keeper_alone_at_post_onshift",
		summary: "Stateful keeper arrives at its own store during working hours with no one else present " +
			"(the live Josiah Thorne case, LLM-106). The golden pins exactly what the engine shows him: " +
			"co-presence reads 'no one else here', yet the turn is speak-eligible and framed for trade — " +
			"the structural pull that made the model greet an empty room. The speak-audience gate (LLM-106 slice 2) " +
			"fixed it at the tool-advertising layer, so this PAYLOAD is unchanged — the golden is a regression pin; " +
			"the fix's guard is the handlers gating test.",
		build: keeperAloneAtPostOnShift,
	},
	{
		name: "constable_walking_rounds_at_store",
		summary: "LLM-514: the constable (Gideon Marsh) mid-rounds, having stepped inside the General Store on " +
			"his circuit of the village's businesses. The golden pins the diegetic rounds self-state cue — " +
			"'You are walking your rounds through the village. You stand before the General Store.' — that keeps " +
			"any reactor tick during the tour in character (a suspicious constable), and that the go-to-post duty " +
			"steer yields to it while the route runs.",
		build: constableWalkingRoundsAtStore,
	},
	{
		name: "constable_at_last_rounds_stop",
		summary: "LLM-524 boundary: the constable at the FINAL stop of his rounds circuit " +
			"(RouteStopsAhead == 0). Pins that the 'more places on your round still lie ahead' " +
			"continuation line is OMITTED — the line names what remains, and at the last stop " +
			"nothing does. The mid-circuit twin (constable_walking_rounds_at_store) pins its presence.",
		build: constableAtLastRoundsStop,
	},
	{
		name: "constable_round_owed_off_post",
		summary: "LLM-540/548: the constable idle at the Blacksmith with a round still owed — he stepped " +
			"off the circuit himself and has finished whatever pulled him away. Pins the pairing that " +
			"makes the wake safe: the cue names what is left and the next place to walk to (Ellis Farm), " +
			"AND the go-to-post duty steer is absent, so the prompt never carries two orders at once. " +
			"The tick exists at all because a part-walked round no longer suppresses his shift-duty " +
			"warrant; before that he had no recurring wake source and stood still until the 30-minute " +
			"idle backstop. Waking him is not marching him. Under LLM-548 the count and the next name " +
			"are told to him with no arrival requirement — a man away from every stop is exactly who " +
			"needs them — and nothing claims he stands before anywhere, because he does not.",
		build: constableRoundOwedOffPost,
	},
	{
		name: "constable_woken_at_his_post_with_a_round_owed",
		summary: "LLM-549: the constable on-shift INSIDE his post with a fresh eight-stop round owed and " +
			"nothing walked. This is the one situation with no other wake source — shift duty has no " +
			"at-work arm and the idle backstop only covers actors outdoors — so before the beat wake " +
			"existed this prompt was never built and he stood in the Meeting House for twelve minutes. " +
			"Pins that the wake arrives carrying its own argument and nothing else: the rounds cue names " +
			"the count and the first place he owes, the wake's own warrant is filtered out of \"Since your " +
			"last turn\" rather than printing the raw kind under the scene it duplicates, and no go-to-post " +
			"steer appears even though he is standing in his post.",
		build: constableWokenAtHisPostWithARoundOwed,
	},
	{
		name: "constable_at_a_villagers_door",
		summary: "LLM-547 base: the constable mid-round at the Apothecary with Prudence Ward within " +
			"earshot and NO history between them inside the recall horizon. Pins the silent tier — " +
			"a pair who have not spoken get no line at all, so the fact costs nothing in the common " +
			"case. Every other LLM-547 golden is this fixture plus a contact trail, so their diffs " +
			"against this one are exactly the sentence each tier renders.",
		build: constableAtAVillagersDoor,
	},
	{
		name: "constable_already_called_on_her_this_round",
		summary: "LLM-547 quiet brake: one call on Ward 40 minutes ago, inside the 2h brake window. " +
			"The scene says he has already had his word with her THIS ROUND — the phrasing is round-" +
			"aware because he is carrying a circuit. This is the line whose absence let him re-open " +
			"with 'Goodwife Ward, good afternoon to you' at 20:44, 21:09 and 21:19 as though each " +
			"were the first. No imperative: it states the fact and leaves the conclusion to him.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			return withContacts(snap, actorID, "prudence", 40*time.Minute), actorID, warrants
		},
	},
	{
		name: "constable_called_on_her_twice_this_round",
		summary: "LLM-547 weighted brake: two calls inside the brake window (75 and 20 minutes back). " +
			"The line adds the count AND the peer's state — there was little left unsaid by the end " +
			"of it — which is the half that makes it an argument rather than a tally. Ward's actual " +
			"rebuke ('Three times is thrice too many') landed on the THIRD approach; this tier exists " +
			"so he feels it before she has to deliver it. Pronoun-free by construction: actors carry " +
			"no gender field, so the ticket's 'she has said her piece' would have been a guess " +
			"rendered as fact.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			return withContacts(snap, actorID, "prudence", 75*time.Minute, 20*time.Minute), actorID, warrants
		},
	},
	{
		name: "constable_had_his_word_with_her_earlier_today",
		summary: "LLM-547 continuity: one call 5 hours back — past the 2h brake, inside the 8h recall " +
			"horizon. The SAME record now does the opposite rhetorical job: not 'hold off' but 'you " +
			"have history, build on it'. The tier ladder extends in both directions rather than " +
			"decaying to silence, which is why one mechanism covers both the constable's repeat and " +
			"LLM-546's innkeeper greeting a player she served that afternoon as a stranger.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			return withContacts(snap, actorID, "prudence", 5*time.Hour), actorID, warrants
		},
	},
	{
		name: "constable_last_spoke_with_her_beyond_recall",
		summary: "LLM-547 horizon: one call 10 hours back, past the 8h recall horizon. Renders nothing " +
			"— they read as strangers again, which is the truth at that distance. Byte-identical to " +
			"constable_at_a_villagers_door: pins that the horizon actually drops the fact rather than " +
			"quietly degrading it to the continuity line.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			return withContacts(snap, actorID, "prudence", 10*time.Hour), actorID, warrants
		},
	},
	{
		name: "constable_carries_history_for_someone_not_here",
		summary: "LLM-547 co-presence gate: a fresh, brake-window-recent trail with Josiah Thorne, who " +
			"is NOT in the scene. Renders nothing about him. The guard is structural rather than a " +
			"check — the tier is attached in resolveCoPresentMember, which only ever runs for someone " +
			"actually present — but it is pinned here because the failure it prevents (a roll-call of " +
			"everyone the constable has ever spoken to, re-sent every tick) would be both a prompt-" +
			"bloat and a legibility regression.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			return withContacts(snap, actorID, "josiah", 15*time.Minute), actorID, warrants
		},
	},
	{
		name: "constable_spoke_with_a_stranger_he_cannot_name",
		summary: "LLM-547 acquaintance gate: a brake-window-recent trail with a peer the constable does " +
			"NOT know by name. Renders nothing, and is byte-identical to constable_at_a_villagers_door " +
			"apart from the company line calling her a stranger. The line cannot follow the usual " +
			"descriptorLabel gating — it must carry the verbatim DisplayName or the model's reply " +
			"silently degrades to the whole huddle — so when the name is withheld the fact goes with " +
			"it. The alternative is a scene contradicting itself one line apart: 'You are outdoors, " +
			"with a stranger. You had your word with Prudence Ward a short while ago.' Found by the " +
			"end-to-end Speak→perception test, not by inspection.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := constableAtAVillagersDoor()
			snap.Actors[actorID].Acquaintances = nil
			return withContacts(snap, actorID, "prudence", 40*time.Minute), actorID, warrants
		},
	},
	{
		name: "storm_weather_over_keeper_at_post",
		summary: "LLM-364: the keeper_alone_at_post_onshift fixture with a storm overhead " +
			"(Environment.Weather = storm). The golden pins the deterministic felt rain line — " +
			"\"Rain falls steady over the village, and the lanes are turning to mud.\" — rendered right after " +
			"the time-of-day line, so an NPC deciding its turn actually perceives the weather (the client's " +
			"rain / lightning FX alone never reached deliberation, and the atmosphere line was pulled by " +
			"WORK-374). The diff against keeper_alone_at_post_onshift is exactly that one added line; clear / " +
			"empty weather renders nothing (matrix guard: TestGoldensRainLineIffStorm).",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := keeperAloneAtPostOnShift()
			snap.Environment.Weather = sim.WeatherStorm
			return snap, actorID, warrants
		},
	},
	{
		name: "villager_summoned_sees_the_call",
		summary: "LLM-323/LLM-414: the keeper_alone_at_post_onshift fixture with a summons delivered to the subject " +
			"(PendingSummon set — a messenger reached them). The golden pins the target-side '## You have been " +
			"summoned' section: who sent for them, where to go WITH the meet place's structure_id inline (the move_to " +
			"token a cross-village target can't resolve from a bare label), the reason, and the move_to steer. LLM-414 " +
			"also pins the SUPPRESSIONS: the at-post duty stabilizer yields to the summons (no 'stay and look after your " +
			"work' line), so the summons is the single actionable movement voice. The diff against " +
			"keeper_alone_at_post_onshift is that added block + the absent steer.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := keeperAloneAtPostOnShift()
			snap.Actors[actorID].PendingSummon = &sim.PendingSummon{
				SummonerName:     "Goodwife Bishop",
				Place:            "the town square",
				PlaceStructureID: "town_square",
				Reason:           "There is news of the trial.",
				At:               snap.PublishedAt,
			}
			return snap, actorID, warrants
		},
	},
	{
		name: "villager_summoned_in_evening_goes_not_home",
		summary: "LLM-414 (the live 2026-07-14 incident shape): a homed day-shift agent (Ezekiel), off-shift at 20:30 " +
			"inside the evening window — the state where the go-home wind-down and the tavern invitation both compete — " +
			"with a summons just delivered (PendingSummon). The golden pins that '## You have been summoned' renders AND " +
			"that BOTH competing movement voices are silent: no evening 'tavern is open' invitation and no go-home steer. " +
			"Live, the target was summoned just past dusk and walked home instead of to the meet place because the engine " +
			"argued for home against its own errand. The model still chooses freely — the scene simply stops contradicting " +
			"the summons.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			snap, actorID, warrants := homedWorkerEveningTavernOpen()
			snap.Actors[actorID].PendingSummon = &sim.PendingSummon{
				SummonerName:     "John Ellis",
				Place:            "the Tavern",
				PlaceStructureID: "tavern",
				Reason:           "A guest asks after you.",
				At:               snap.PublishedAt,
			}
			return snap, actorID, warrants
		},
	},
	{
		name: "visitor_arrives_at_keepers_workplace",
		summary: "LLM-284: a tavern keeper (John Ellis) arrives at another keeper's workplace — the Blacksmith, " +
			"kept by the co-present Ezekiel Crane — on an errand. The golden pins the keeper possessive in the " +
			"'## Since your last turn' arrival line ('You arrived at Ezekiel Crane's Blacksmith.') so the model reads " +
			"whose shop it entered instead of greeting the smith as if hosting its own forge (the live host-role " +
			"inversion). A regression back to the ownerless 'the Blacksmith' shows in the diff.",
		build: visitorArrivesAtKeepersWorkplace,
	},
	{
		name: "traveler_self_identity_preface",
		summary: "LLM-370: a transient traveler (salem-visitor Elias Drum the peddler, in from Boston, weary) perceives " +
			"its OWN turn, standing in the Tavern. The golden pins the self-identity preface that OPENS the message — " +
			"'You are Elias Drum, a peddler making your way through Salem. You hail from Boston, and your manner today is " +
			"weary.' — ahead of the '# Your turn' header, so the stateless salem-visitor VA (no per-visitor identity in " +
			"its system prompt) speaks in-character as this specific traveler. Before LLM-370 perception never read " +
			"VisitorState and a traveler was rendered as a generic stranger with no persona at all.",
		build: travelerSelfIdentityPreface,
	},
	{
		name: "traveler_self_identity_preface_vocation",
		summary: "LLM-566: a POOL-archetype traveler (Master Whitcombe the circuit preacher, in from Beverly, wry) " +
			"perceives its own turn in the Tavern. The golden pins the vocation sentence the preface now carries after " +
			"the origin/manner clause — what a circuit preacher DOES (blessings, souls, scripture at a hearth or the " +
			"meeting house) — so the model plays the calling instead of a generic news-carrier wearing the label. The " +
			"live case: Whitcombe toured six businesses as a genial gossip whose whole clergy was 'the Lord willing.' " +
			"The peddler goldens above pin the other arm — an archetype OUTSIDE the passer-through pool (merchant-derived " +
			"labels) gets no vocation sentence.",
		build: travelerSelfIdentityPrefaceVocation,
	},
	{
		name: "traveler_self_identity_preface_with_rumor",
		summary: "LLM-371: the same traveler (Elias Drum the peddler) now carries a grounded rumor — VisitorState.Payload, " +
			"selected at spawn from the action log. The golden pins the extra preface clause that closes the persona line — " +
			"'Word reached you on the road that Ezekiel Crane turned out a plow for the Hale farm.' — so the stateless " +
			"salem-visitor VA has one true, checkable thing to trade in conversation instead of empty small-talk. The " +
			"payload rides the same self-preface stream as the persona (renderTravelerPreface); an empty payload drops the " +
			"clause (the no-rumor case is the LLM-370 traveler_self_identity_preface golden).",
		build: travelerSelfIdentityPrefaceWithRoadWord,
	},
	{
		name: "traveler_rumor_spent_with_present_company",
		summary: "LLM-545: the rumor-carrying traveler is back in a huddle with the one person he has already passed " +
			"the word to (VisitorState.PayloadSharedWith, stamped on the Speak commit path when she answered him). The " +
			"golden pins the preface's SPENT reframing — 'The word you picked up on the road — that … — you have already " +
			"passed to Hannah Boggs, and heard what they had to say; that matter is spent between you.' — replacing the " +
			"fresh 'Word reached you …' framing that made Brother Ashford reopen the Josiah Thorne rumour on Hannah Boggs " +
			"half an hour after she set him straight (the live specimen this ticket was filed on).",
		build: travelerRumorSpentWithPresentCompany,
	},
	{
		name: "traveler_rumor_already_told_one_of_company",
		summary: "LLM-545 mixed company: one peer has had the word (Hannah Boggs), one has not (Goodman Stark). The " +
			"golden pins the fresh 'Word reached you …' line SURVIVING — there is a new listener — plus the annotation " +
			"'You have already passed that word to Hannah Boggs.', so the model carries the news to the newcomer without " +
			"re-opening it on the one who already answered it.",
		build: travelerRumorAlreadyToldOneOfCompany,
	},
	{
		name: "traveler_rumor_told_someone_not_here",
		summary: "LLM-545 co-presence guard: the traveler has passed his word to Hannah Boggs, but present company is " +
			"Goodman Stark alone — Hannah is elsewhere in the snapshot. The golden pins the PLAIN fresh rumor line with " +
			"no mention of Hannah: the shared-word memory matters face to face only (the LLM-547 posture), and naming an " +
			"absent listener would hand the model a person to go find instead of news to trade here.",
		build: travelerRumorToldSomeoneNotHere,
	},
	{
		name: "traveler_observed_by_villager",
		summary: "LLM-370: a villager (Goodwife Bishop) stands in the Tavern with a co-present transient traveler she " +
			"does not yet know by name. The golden pins the observer cue in '## Around you' — 'a peddler lately come from " +
			"Boston is here with you.' — where the bare 'a stranger' descriptorLabel used to render (unacquainted, no " +
			"role). Archetype + origin reach the observer's prompt; disposition stays out (it colors the traveler's own " +
			"voice only). The traveler stays addressable in the company line, so no separate redundant 'a stranger' beat.",
		build: travelerObservedByVillager,
	},
	{
		name: "traveler_returner_self_preface",
		summary: "LLM-372: a returning traveler (Elias Drum the peddler, back for his 3rd visit) perceives its OWN turn. " +
			"The golden pins the returner continuity that closes the self-preface — 'You have come through Salem a few " +
			"times before. You know Jeff here — you last saw them a few weeks back.' — so the stateless salem-visitor VA " +
			"greets a player it remembers instead of a stranger. Present ONLY for a repeat visit (Returner.VisitCount >= 2); " +
			"a first-visit traveler (traveler_self_identity_preface, no Returner) shows no continuity block.",
		build: travelerReturnerSelfPreface,
	},
	{
		name: "traveler_returner_episodic_memory",
		summary: "LLM-383: a returning traveler (Elias Drum, back for his 3rd visit) whose acquaintance with Jeff carries a " +
			"FOLDED episodic summary — the returner remembers not just that it knows Jeff, but what happened last visit. The " +
			"golden pins the remembered specifics woven into the self-preface after the recognition clause — the distilled " +
			"impression prose ('Jeff frets over the fence line ... bought a bundle of your nails to set it right'), turning " +
			"recognition into being remembered. Only the folded summary is surfaced (never a raw fact list — re-surfacing a " +
			"stored heard-utterance as a live one drove the ZBBS-HOME-412 re-pitch bug); the no-summary case is " +
			"traveler_returner_self_preface.",
		build: travelerReturnerEpisodicMemory,
	},
	{
		name: "hungry_worker_with_means_redirected_to_eat",
		summary: "LLM-276: a workless, on-shift, idle worker whose hunger sits in the upper felt band (15, " +
			"below the red-line 18) and who can resolve it now (holds coin, a free bush + a porridge vendor in reach). " +
			"The seek-work backstop stamped a tend-need warrant, so the prompt steers the worker to EAT — the tend-need " +
			"felt line, the eat/drink options, and the one-target need-redirect — and suppresses the businesses directory " +
			"and the solicit-work hustle, exactly as a red need would. The perception half of the Silence Walker beg-loop fix.",
		build: hungryWorkerWithMeansRedirectedToEat,
	},
	{
		name: "tired_keeper_at_post_onshift",
		summary: "Tired keeper standing at its own post, on shift (LLM-100 positive case). The '## How you can rest' " +
			"cue offers take_break (rest in place) only because the actor is on shift. The golden pins the bullet's " +
			"presence; a regression to the on-shift gate would flip it in the diff.",
		build: tiredKeeperAtPostOnShift,
	},
	{
		name: "weary_resident_in_own_home",
		summary: "LLM-214: a weary salem-vendor (Anne Walker) stands INSIDE its own home, off-shift for the evening, with " +
			"a separate workplace. Before the fix the '## How you can rest' list handed it the home structure_id as a move_to " +
			"target ('sleep in your own bed (structure_id …)') for the structure it was standing in — the no-op move Lewis / " +
			"Anne looped on — and the anchor pointer told it to 'head back there'. The golden pins the in-place cues: the rest " +
			"section leads with the RestAtHome take_break bullet and carries NO home structure_id, and the anchor states " +
			"'You're home' while keeping only the workplace as a reachable move target. The matrix-wide guard is " +
			"TestGoldensNeverAdvertiseHomeAsMoveTargetWhenInside.",
		build: wearyResidentInOwnHome,
	},
	{
		name: "walker_home_after_shut_store_trip",
		summary: "LLM-366: a workless, scheduleless salem-vendor Walker (the live Silence Walker) sits idle inside " +
			"its own home in the morning — the 'find work / see who's about' decision turn that had no legible " +
			"shut-status signal at decision time. Its self-action trail shows it just walked to the General Store and " +
			"found it shut (a live ObservedClosed memory within TTL), so '## What you've recently done' renders 'You went " +
			"to the General Store but found it shut, no one tending it' instead of a neutral 'You arrived at the General " +
			"Store'. The FIRST golden to exercise the self-action trail (sets PublishedAt + an ActionLog); it pins the " +
			"LLM-217 churn-mirror carrying the dead-end outcome that broke the home↔closed-store loop. A regression that " +
			"dropped the FoundShut annotation reverts the line to the neutral arrival in the diff.",
		build: walkerHomeAfterShutStoreTrip,
	},
	{
		name: "turned_away_outdoors_at_shut_tavern",
		summary: "LLM-463: the turned-away case — the villager is standing OUTDOORS at the shut Tavern's loiter " +
			"pin, having just walked there and found it keeperless (the live Moses James turn, 2026-07-18 17:04). " +
			"walker_home_after_shut_store_trip covers remembering a dead end from indoors somewhere else; this covers " +
			"reading it while still at the shut door, which is the situation the loop actually repeats from. It uses the " +
			"live Tavern geometry (anchor 106,131 / loiter_offset -3,+1) so the actor rests one row below the footprint " +
			"— genuinely outdoors — and the walked action-log row therefore carries a BLANK conversational-scope " +
			"StructureID, since loiterScopeConversable refuses the outdoor scope of a keeperless structure. The golden " +
			"pins that '## What you've recently done' still names the dead end from DestStructureID alone. Keying " +
			"FoundShut back off the scope stamp reverts the line to 'You arrived at the Tavern' in the diff — the live " +
			"regression that had NPCs re-walking to the shut Tavern for a day.",
		build: turnedAwayOutdoorsAtShutTavern,
	},
	{
		name: "shared_npc_soul_who_you_are",
		summary: "A shared-VA keeper (Hannah, salem-vendor) at her own post during working hours, carrying a " +
			"synthesized about_me soul (LLM-199). The golden pins that '## Who you are' renders the soul prose — the " +
			"fix for the empty-block bug (shared VAs had no rendered identity because render emitted only the never-" +
			"populated seed_text). A regression that muted about_me, reverted the render field, or dropped the build " +
			"gate would show the block going empty in the diff.",
		build: sharedNpcWithSoul,
	},
	{
		name: "shared_npc_bystander_hears_addressed_speech",
		summary: "LLM-432: the live Patience Walker misfire (2026-07-15). A shared-VA villager in the evening tavern " +
			"hears a PC address a co-present third party by name ('ezekiel, you sleeping over there?'), and her soul " +
			"prose — like the live one — never states her own name. Nothing else in a shared-VA prompt does (the " +
			"shared VA's system prompt is a generic sim context), so she couldn't rule herself out as the addressee " +
			"and answered as if teased. The golden pins the 'You are Patience Walker.' self-name line opening " +
			"'## Who you are' ahead of the soul prose. A regression that drops the Name field from the build or the " +
			"render shows the line vanishing in the diff.",
		build: sharedNpcBystanderHearsAddressedSpeech,
	},
	{
		name: "homed_worker_evening_tavern_open",
		summary: "A homed day-shift agent (Ezekiel, 07:00–19:00), off-shift and awake at 20:30 — inside the " +
			"evening window [shift-end, 22:00) — standing at his forge after closing up (LLM-149, Lever 2). The golden " +
			"pins the evening 'tavern's open' invitation in ## Around you (carrying the tavern + home structure_ids, no " +
			"forced walk) AND that the off-shift go-home wind-down steer ('Your working hours are over …') is ABSENT: the " +
			"cue REPLACES that turn-in pressure for the window (bedtime is Lever 1's 22:00 gate). A regression that let the " +
			"go-home steer leak back in, or dropped the invitation, shows in the diff.",
		build: homedWorkerEveningTavernOpen,
	},
	{
		name: "unscheduled_worker_evening_tavern_open",
		summary: "LLM-352: a homed labor vendor (Lewis Walker) with NO schedule row and no fixed workplace, day-active " +
			"on the world dawn/dusk window (07:00–19:00, LLM-137), standing outdoors at 20:30 — inside its [dusk, 22:00) " +
			"evening. The golden pins that the evening 'tavern's open' invitation now fires for an UNSCHEDULED worker exactly " +
			"as it does for a scheduled one — before the fix the Walkers were shut out of the evening (no cue) and bedded at " +
			"dusk. A regression that re-keys the evening on a present schedule row drops the invitation here.",
		build: unscheduledWorkerEveningTavernOpen,
	},
	{
		name: "walker_women_evening_at_home_may_turn_in",
		summary: "LLM-447: the Walker \"Long Goodnight\", reconstructed from the live 2026-07-16 trace. Three unscheduled " +
			"day-worker women (Silence, Patience, Anne — shared-VA, tiredness 12, BELOW the awareness floor of 13, so no " +
			"tiredness line appears anywhere on the page) are huddled together inside their own home at 21:20, an hour and " +
			"a half past dusk and forty minutes before the auto-bed hour. Live, this state offered exactly one affordance — " +
			"talk — because sleep was fully deterministic and no sleep verb existed: they cycled six huddles of wind-down " +
			"topics over ~80 minutes, said \"let's be off then\" three times without moving, traded 26 goodnights in two " +
			"minutes, and not one of them went to bed. Every topic the models generated was ending-shaped and none mapped " +
			"to a tool. The golden pins the fix: '## The evening draws in' is PRESENT and names turn_in, with the goodnight " +
			"routed through its say (never a separate speak — both verbs are terminal, so a cue asking for both could not " +
			"be obeyed). Note the cue fires on the CLOCK, not the meter: a regression that gates turn_in on tiredness drops " +
			"it here, where the women are at 12, and restores the loop exactly.",
		build: walkerWomenEveningAtHomeMayTurnIn,
	},
	{
		name: "farm_owner_settled_in_tavern_evening",
		summary: "LLM-345: Elizabeth Ellis (farm day 06:00–18:00, owes 3 upkeep shovels) has TAKEN the evening invitation " +
			"and stands inside the Tavern at 19:40, in the [shift-end, 22:00) window. The golden pins both levers of the fix. " +
			"Lever A — the evening framing survives the threshold: ## Around you carries the destination-free settled-in scene " +
			"('… here you are inside the Tavern of an evening … can wait for the morning') instead of falling silent, which is " +
			"what left the farm ledger as the loudest thing on the page. Lever B — '## Farm upkeep' is ABSENT: the walk-away " +
			"errand yields to the room. Live, its presence here walked her home inside ninety seconds.",
		build: farmOwnerSettledInTavernEvening,
	},
	{
		name: "homed_worker_evening_batch_holds_leisure",
		summary: "LLM-335: the SAME homed day-shift agent and evening as homed_worker_evening_tavern_open (Ezekiel at his " +
			"forge at 20:30, inside the [shift-end, 22:00) window, flush enough for a drink), but with a Cheese batch in the " +
			"works AT his post. The batch pins him there (LLM-319 pause model), so the evening 'tavern's open' invitation YIELDS " +
			"to a quiet diegetic hold ('… the batch of Cheese still wants a few more minutes of your eye …') rather than pulling " +
			"him two ways at once — the live pester that surfaced (nagged to the tavern AND to stay for the cheese). The golden " +
			"pins the hold line PRESENT, the tavern invitation ('the tavern is open of an evening') and the off-shift go-home " +
			"WIND-DOWN steer ('Your working hours are over …') BOTH ABSENT, and the standing 'You are making a batch of Cheese …' " +
			"self-state line still rendering. The at-post anchors line ('you can head home whenever you wish') is a separate, " +
			"permissive home-location reference — NOT a wind-down steer — and correctly stays: it names no pressure and doesn't " +
			"pull against the hold. Mirror of homed_worker_evening_tavern_open (same evening, no batch → the invitation).",
		build: homedWorkerEveningBatchHoldsLeisure,
	},
	{
		name: "homed_worker_evening_broke_still_invited",
		summary: "LLM-353: a homed day-shift agent (Ezekiel, 07:00–19:00) off-shift at 20:30 — inside the evening window — " +
			"holding only 2 coins, with the tavern's cheapest drink at 3 (ale, sold by the co-located keeper). Coin no longer " +
			"gates the evening: Salem pays in goods as readily as coin, so the empty purse is no barrier. The golden pins that " +
			"the 'tavern's open of an evening' invitation is PRESENT and the off-shift go-home wind-down steer ('Your working " +
			"hours are over …') is ABSENT — the broke get the same evening as anyone else; the model decides whether to go. " +
			"This is the DoD case: a broke agent in the evening window still receives the tavern invitation. Before LLM-353 the " +
			"coin floor turned this agent away (invitation absent, wind-down present).",
		build: homedWorkerEveningBrokeStillInvited,
	},
	{
		name: "homed_workers_evening_commons_still_solicits",
		summary: "LLM-353: two homed day-shift workers (Ezekiel + Lewis, different homes and trades) off-shift at 20:30, together " +
			"at the Village Commons — neither at home nor the tavern — with a tavern placed and the subject flush (10 coins). The " +
			"re-key keys the solicit-work suppression on tookEveningLeisure (gone to the pub), not on affluence: this worker is " +
			"still in the road, so he is STILL offered the solicit-work affordance ('make the offer with solicit_work') even " +
			"though he could afford a drink. The golden pins the evening invitation PRESENT (he is homed in the window) AND the " +
			"solicit affordance PRESENT — a man still in the road might be job-hunting. This is the DoD invariant: an off-shift " +
			"worker who has not taken the evening still sees the seek-work cues, so the re-key can't silently regress into the " +
			"affluence proxy. Before LLM-353 affluence suppressed the affordance here. Makes " +
			"TestGoldensSeekWorkSurvivesEveningUntilTavern non-vacuous.",
		build: homedWorkersEveningCommonsStillSolicits,
	},
	{
		name: "lodger_evening_tavern_open",
		summary: "LLM-311: the SAME evening as homed_worker_evening_tavern_open, but the agent (Ezekiel) is homeless-by-design " +
			"(home NULL) and lodges via an active nightly room grant at the Inn — the canonical rent-a-room NPC. Before LLM-311 the " +
			"living-evening scope was homed-only, so this agent got no tavern invitation and the off-shift wind-down steered it to " +
			"its rented room all evening (the live Inn↔Blacksmith oscillation). The golden pins that the evening 'tavern's open' " +
			"invitation now fires — its co-equal 'stay in' destination is the rented Inn (destination: inn), not an empty home token " +
			"— AND that the go-home/wind-down steer ('Your working hours are over …') is ABSENT: a lodger with a paid room has an " +
			"evening exactly as a homed peer does. A regression that re-narrowed the scope to homed-only shows in the diff.",
		build: lodgerEveningTavernOpen,
	},
	{
		name: "workless_tired_rejoiner_self_action_trail",
		summary: "LLM-217: a workless, tired shared-worker NPC (Patience Walker, the live case) stands back in the Tavern " +
			"huddle with John Ellis the tavernkeeper after twice announcing 'I'll head home now', walking out, and bouncing " +
			"back — the go-home ↔ seek-work oscillation. The golden pins the two perception surfaces that make the churn " +
			"visible: '## Recent conversation here' lines carry interval stamps (John's byte-identical re-greetings read " +
			"'2m ago' vs 'just now', not as one moment), and '## What you've recently done' lists her own departed/arrived " +
			"trail most-recent-first with stamps. Her in-current-huddle 'I'll head home now.' spoke entry appears ONLY in the " +
			"conversation ring — the trail's current-huddle spoke de-dup keeps it out — and John's own walked entry is absent " +
			"(subject filter). The matrix-wide guard is TestGoldensConversationLinesCarryIntervalStamps.",
		build: worklessTiredRejoinerSelfActionTrail,
	},
	{
		name: "keeper_with_ready_order",
		summary: "An innkeeper holds a Ready order (a nights_stay check-in) for a co-present guest. Exercises the " +
			"order book with a deterministic expiry clause — the LLM-106 render-clock fix anchors 'expires in N " +
			"minutes' to the snapshot instant (RenderedAt), so this golden is byte-stable. Without it the expiry text " +
			"drifts with wall-clock and the determinism guard trips. Demonstrates an order-bearing scenario joining " +
			"the matrix.",
		build: keeperWithReadyOrder,
	},
	{
		name: "smith_commission_awaiting_forge",
		summary: "LLM-338. A blacksmith (Ezekiel) has taken a co-present customer's (Elizabeth's) prepayment for a " +
			"shovel he doesn't yet hold — a commission Order sits Ready but he holds 0 shovels, so DeliverOrder gate 5 " +
			"would bounce a deliver_order. The '## Orders to deliver' line must render passively ('you've yet to make " +
			"it') with NO deliver_order instruction, steering him to forge it first rather than into a bounce loop. The " +
			"live Elizabeth Ellis <-> Ezekiel Crane case. A regression that re-emits the deliver_order cue on an unforged " +
			"commission shows in the diff.",
		build: smithCommissionAwaitingForge,
	},
	{
		name: "grower_at_stripped_bush",
		summary: "A forager stands at her own raspberry bush after harvesting it clean (the live Prudence case, " +
			"LLM-98). Her bushes sit wider apart than LoiterAttributionTiles, so the only in-reach gather candidate " +
			"is the now-empty bush — ResolveGatherSource hands it back. The golden pins that the prompt carries NO " +
			"'you can gather' cue (and so no gather tool): the LLM-98 stock gate suppresses the depleted source. A " +
			"regression would make the 'You're at Raspberry Bush — you can gather raspberries here.' line reappear in the diff.",
		build: growerAtStrippedBush,
	},
	{
		name: "herbalist_ranged_wild_forage",
		summary: "The LLM-253 ranged forage cue. Prudence (herbalist, tagged sim.AttrForageRange) is low on sage " +
			"(0 of cap 5) and owns no sage bush, while an UNOWNED Sage Bush with 10 ripe sits ~80 tiles to the " +
			"northeast — the gap the owner-only owned-bush cue and the proximity-only at-bush cue both leave open. The " +
			"golden pins the distinct '## Free sources you can gather from' section (never 'your bushes'), the qualitative " +
			"distance+direction ('a long walk to the northeast'), the ripe count, and a move_to-by-structure_id handle — " +
			"move_to ONLY, no gather mention (LLM-59/79). Paired with untagged_forager_no_ranged_wild_forage.",
		build: herbalistRangedWildForage,
	},
	{
		name: "untagged_forager_no_ranged_wild_forage",
		summary: "The tag gate for the LLM-253 ranged forage cue: the SAME fixture as herbalist_ranged_wild_forage " +
			"(low on sage, unowned distant Sage Bush) but the forager does NOT carry sim.AttrForageRange. The golden " +
			"pins that NO '## Free sources you can gather from' section renders — ranged awareness of an unowned distant source " +
			"is the tagged 'herbalist gift' only. Enforced across the matrix by TestGoldensRangedWildForageRequiresTag.",
		build: untaggedForagerNoRangedWildForage,
	},
	{
		name: "general_store_water_forage_at_well",
		summary: "LLM-254 two-row Well. The town Well is an UNOWNED commons carrying BOTH a public thirst " +
			"drink row (Amount -8, slake-in-place) AND a yield-only water gather row (Amount 0, unset attribute — " +
			"the LLM-264 clean yield row). Josiah Thorne (merchant, tagged sim.AttrForageRange, low on water with " +
			"a `forage water` restock entry) is thirsty ~10 tiles away, so ONE unowned object surfaces in TWO " +
			"independent cues at once with no owner-gate conflict: the free-drink satiation cue ('## What you can " +
			"eat or drink' — the -8 thirst row) AND the ranged forage cue ('## Free sources you can gather from' — " +
			"the water yield row, 20 ready to gather). The forage count reads the yield row alone; the -8 drink row " +
			"never pollutes it (forageStockForItem gates on Amount==0). Byte-stable: on shift, no orders, no clock read.",
		build: generalStoreWaterForageAtWell,
	},
	{
		name: "miller_with_hired_hauler_keeps_water_cue",
		summary: "LLM-621. A keeper at his OWN post, out of water (0 of a 20 cap, `forage water`, tagged " +
			"sim.AttrForageRange) with a full commons Well nearby, and his own hired hauler co-present mid-job " +
			"(StateLaboring + a Working LaborOffer naming him employer). The '## Free sources you can gather from' " +
			"cue must SURVIVE: a worker fulfilling a contract is not custom, so customerEngaged's co-presence arm " +
			"does not fire — the same rule LLM-231 already applies in buildOfferableCustomers, which drops a " +
			"laboring peer as a sale target even for their own employer. Pre-fix the bare huddle-member count read " +
			"the worker as a live sale and silenced the cue for the whole contract, hours rather than an encounter, " +
			"while the same prompt rendered 'Lewis Walker is working a job for you'. Pins the at-post stabilizer " +
			"reframed as a step-out errand alongside it. Fixed PublishedAt keeps the workers-for-me duration " +
			"byte-stable; on shift, no orders, no price book. LLM-622 additionally pins that step-out line " +
			"claiming NOTHING: the only source here is the commons Well, so wording that called it his own " +
			"contradicted the '## Free sources you can gather from' section printed directly beneath it, which " +
			"says no one owns them. Paired with miller_stepping_out_to_his_own_bush.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return millerWithHiredHauler(false) },
	},
	{
		name: "miller_stepping_out_to_his_own_bush",
		summary: "LLM-622 control for miller_with_hired_hauler_keeps_water_cue: the SAME miller, same hour, same " +
			"hire, same empty water — but he owns a spring of his own for it. The owned branch of the forage cue " +
			"takes over ('## Your bushes to harvest' in place of the free-sources section, since the wild scan " +
			"runs only where the grower owns no bush), and the at-post step-out line may then say 'your own " +
			"bushes' truthfully. The pair is what makes the fix discriminating: one bit of fixture — who owns " +
			"the water — has to move BOTH the forage section and the stabilizer's ownership claim together, so " +
			"a reframe that ignores the distinction cannot pass both goldens.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return millerWithHiredHauler(true) },
	},
	{
		name: "miller_at_degraded_mill_keeps_water_forage_cue",
		summary: "LLM-634. The 2026-08-15 deadlock trigger: the village's only `forage water` keeper stands inside " +
			"his OWN Mill, worn past the degrade threshold (650 >= 600) with 0 of the 5 nails a mend takes, out of " +
			"water (0 of 20, tagged sim.AttrForageRange) with a full commons Well a short walk to the northeast. " +
			"The golden pins BOTH the degraded '## Your business' framing (limping wording at the live pct, the " +
			"nail shortfall with no supplier on record) AND '## Free sources you can gather from' naming the Well " +
			"in the same prompt: a degraded stall must never silence the forage cue, because foraging spends no coin " +
			"and its yield is the upstream of the nail that mends the stall (water → nail → mend). The warrant half " +
			"is TestEvaluateRestock_DegradedForageStillWarrants (sim). Same geometry as " +
			"miller_with_hired_hauler_keeps_water_cue, alone and unhired, so the diff is the worn business alone. " +
			"On shift, no orders, no price book, no clock read — byte-stable.",
		build: millerAtDegradedMillOutOfWater,
	},
	{
		name: "quenched_at_well_no_drink_cue",
		summary: "LLM-376. A shared-VA NPC stands ON the town Well's loiter pin, thirst fully quenched (0), " +
			"still holding a stale open-ended thirst dwell credit for the Well (the immortal credit the pre-fix " +
			"arrival path stamped when the -8 gulp landed thirst on the floor). The satisfied-need gate in " +
			"buildActiveDwellCredits must drop that credit so the prompt does NOT assert 'You are drinking at " +
			"Well … until your thirst is quenched' — the cue that pinned Lewis Walker at the well for 3+ hours. " +
			"Byte-stable: idle, on shift, no orders, no clock read.",
		build: npcQuenchedAtWellStaleCredit,
	},
	{
		name: "smith_with_empty_shelf_takes_commissions",
		summary: "LLM-619 (the live 2026-08-07 Ezekiel/Moses case). The smith produces shovels, holds NONE, and has the " +
			"water a batch needs, with the buyer co-present at his forge. The sale cue used to build 'Your goods to " +
			"sell' from physical inventory alone, so the shovel vanished and the model read that as 'cannot sell' — " +
			"answering with a spoken promise, which commits nothing, leaves no order for LLM-518's farm-upkeep " +
			"suppression to find, and sent the buyer forge<->home 13 times in 75 minutes over six re-struck bargains. " +
			"The golden pins the shovel listed as a commission AND the steer naming sell for it — the listing alone " +
			"was never the whole problem, the model also had to be told the sale was still its to make. Nail is the " +
			"control: 3 on hand, so it stays an ordinary take-home sale listed once with its count.",
		build: smithWithEmptyShelfTakesCommissions,
	},
	{
		name: "grower_home_mid_shift_no_ripe_bush_still_owes_his_post",
		summary: "LLM-620 arm 1 (the live 2026-08-07 Moses James lap). A grower stands in his own house mid-shift, " +
			"low on wheat, owning bushes with NOTHING ripe. The harvest cue still renders — it fires on low stock, " +
			"not ripeness — and says 'walk out to your bushes to restock' and 'none ripe yet' in the same breath. " +
			"Level-triggered on that cue's mere presence, the duty-steer suppression treated it as an errand in " +
			"progress and yielded forever, while the SAME signal flipped the at-post stabilizer to a step-out " +
			"permission; both ends of the shift came unpinned at once and he ran James Farm<->James Residence in " +
			"6-to-17-second laps. The golden pins the to-work steer FIRING here: a cue he cannot take one step " +
			"toward is a want, not an errand. Paired with grower_home_mid_shift_ripe_bush_keeps_its_errand.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerHomeMidShift(false) },
	},
	{
		name: "grower_home_mid_shift_ripe_bush_keeps_its_errand",
		summary: "LLM-620 control for grower_home_mid_shift_no_ripe_bush_still_owes_his_post: the SAME grower, same " +
			"hour, same low wheat — but one bush is ripe. The errand is now actionable, so the HOME-400/LLM-90 " +
			"suppression still applies and he is not yanked off the harvest trip he is about to make. The golden " +
			"pins the status-only line in the yank's place ('you are away from your post at the James Farm'), " +
			"carrying the shift fact with NO destination id and no imperative — the LLM-620 second half.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerHomeMidShift(true) },
	},
	{
		name: "grower_at_post_nothing_ripe_keeps_his_post",
		summary: "LLM-643 (the live 2026-08-21 Moses James lap — the at-post door LLM-620 left open). The same grower " +
			"stands AT his farm mid-shift: low wheat, nothing ripe, no customers. The at-post anchors line used to " +
			"hand him home's destination id under 'head home once your work is done' — a condition an idle keeper " +
			"judges satisfied right now — so he walked home, the to-work yank (correct, and pinned by the LLM-620 " +
			"goldens) marched him straight back, and he lapped farm↔house in 17-to-24-second stays, 9 times in a day. " +
			"The golden pins the anchors line naming home with NO destination id and the departure pinned to the " +
			"close hour ('after you close') the at-post stabilizer states beside it; and the harvest cue header " +
			"reading 'nothing is ripe to pick just now' instead of ordering the walk its own item line contradicts " +
			"('none ripe yet'). Idle, on shift, no orders, no clock read — byte-stable.",
		build: growerAtPostMidShiftNothingRipe,
	},
	{
		name: "keeper_home_mid_shift_evening_reads_his_own_hours",
		summary: "LLM-620 arm 2 (the live 2026-08-07 Josiah Thorne lap). The shopkeeper stands in his house at 18:10 " +
			"with an ACTIONABLE restock errand — wheat low, James Farm open and affordable — so the to-work yank is " +
			"correctly deferred. Before LLM-620 that deferral returned no duty cue at all, leaving an ambient hour " +
			"('Evening settles over the village.') with nothing tying it to his own shift; he answered 'Morning's " +
			"come. I'm home, the fire's out, and there's business to attend to', walked to the shop, read the SAME " +
			"evening at his post — where the shift fact IS rendered — as 'the day's winding down', and walked home. " +
			"He honours the hour whenever a shift fact sits beside it. The golden pins that fact rendering away from " +
			"post too, with its close time and without the '(destination: …)' token that would make it a yank. It ALSO " +
			"pins the supplier cost hint as per-unit prose ('about 1 coin each'): the fixture's price observation is 6 " +
			"coins for 6 sheaves, which used to render as the bare total '~6 coins' beneath a sell price of 1 — the " +
			"misreading that kept this errand unresolved and the lap running.",
		build: keeperHomeMidShiftActionableRestock,
	},
	{
		name: "grower_stands_at_the_bush_the_cue_would_name",
		summary: "LLM-617 steer arm — the live Moses James wedge. A wheat grower with an empty pack stands exactly on " +
			"the loiter pin of his own RIPEST bush (9 units), with a second ripe bush (5) across the map. The " +
			"pre-fix ripest-wins rule named the bush underfoot, which move_to bounces as a no-op " +
			"(TerminalNoOpError); the tick ends on that terminal verb and the unchanged cue re-issues the same " +
			"handle next tick — Moses stood at his field for two hours doing exactly this. The golden pins the " +
			"steer naming the FARTHER bush, the one he can actually walk to, while the ripe count still reports " +
			"the whole plot (14) rather than only the steerable part. It also pins the handoff working: the " +
			"at-bush proximity cue owns the gather verb, so 'You're at Wheat' appears beside the walk steer.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerAtOwnRipeBush(true) },
	},
	{
		name: "grower_stands_at_his_only_ripe_bush",
		summary: "LLM-617 silence arm and the foil of grower_stands_at_the_bush_the_cue_would_name — same grower, " +
			"same posture, but the bush underfoot is his ONLY ripe one. There is no walk left to steer, so the " +
			"golden pins the cue emitting NO move_to handle and saying where he stands instead. It must not fall " +
			"through to 'none ripe yet', which would contradict the 9 ripe it reports in the same breath, and it " +
			"must not name gather — the LLM-59/79 posture holds, and the at-bush cue carries that verb.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerAtOwnRipeBush(false) },
	},
	{
		name: "grower_underfoot_with_no_gather_cue",
		summary: "LLM-617 worst case (raised by code_review). The grower stands at his own ripe bush AND the at-bush " +
			"gather cue is suppressed, because a NON-GATHERABLE object (a fence post) sits nearer and " +
			"ResolveGatherSource's nearest-wins scan blocks on it rather than yielding to the farther bush — the " +
			"same inverse-question trap LLM-550 named. The golden pins the honest outcome: the ripe count and the " +
			"standing-among-them line, NO move_to, and NO 'you can gather' line. A section with no callable action " +
			"is deliberately preferred to emitting a move_to guaranteed to terminal-no-op (which burns the tick's " +
			"one terminal verb — the wedge itself) or to inventing a gather instruction (the LLM-59/79 " +
			"rejected-tool fixation). If a later change gives this case an action, update this golden deliberately.",
		build: growerUnderfootWithNoGatherCue,
	},
	{
		name: "grower_on_ring_slot_that_is_a_neighbours_pin",
		summary: "LLM-618 fix arm — the live Moses James wedge, one layer under LLM-617. He walked to his own ripe " +
			"wheat and an ObjectVisit parked him on one of the EIGHT ring slots around its pin, which is the normal " +
			"arrival (only 1 of the 9 accepted tiles is the pin itself). That slot happened to be Chebyshev 0 from a " +
			"NEIGHBOUR's wheat plant — in the village, one plant a mis-click had assigned to a PC. The nearest-wins " +
			"scan then answered a question nobody asked, reported the foreign owner of the tile, and withheld the " +
			"gather cue at a bush he legitimately stood at; because the gather TOOL is advertised only when that " +
			"resolution yields a source, he had no picking verb at all and re-walked the same bush for two hours. " +
			"The golden pins the cue rendering off his walked-to target: 'You're at Wheat'.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerAtForeignPinRingSlot(true) },
	},
	{
		name: "grower_on_a_neighbours_pin_with_no_walked_to_target",
		summary: "LLM-618 foil of grower_on_ring_slot_that_is_a_neighbours_pin — identical geometry, but the grower " +
			"carries NO walked-to target, so the unchanged nearest-scan still owns the answer and the gather cue " +
			"stays suppressed. This arm is what keeps the fix from reading as 'the poacher gate was deleted': " +
			"standing at a neighbour's bush and reaching past it for your own must still fail. Only an actor who " +
			"deliberately walked to a source he may take from gets to skip the gate.",
		build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) { return growerAtForeignPinRingSlot(false) },
	},
	{
		name: "hungry_forager_at_stocked_bush",
		summary: "A hungry forager stands at an unowned raspberry bush that still has stock, with a cheese seller at " +
			"the General Store nearby — the LLM-113 situation (Ezekiel at the Raspberry Bush with buy options). The " +
			"golden pins the count-aware catalog phrasing the singular/plural labels drive: the gather cue 'you can " +
			"gather raspberries here', and the buy cue 'buy a wedge of cheese' (the period measure phrase with an " +
			"indefinite article) rather than the bare 'buy Cheese'. A regression in the label model flips those lines. " +
			"It also pins the free-eat line beside the paid one — a wild bush is PICK-AND-EAT, so a hungry man sees " +
			"food he can take as well as food he can buy (LLM-610 corrected the fixture's row from forage-to-sell, " +
			"which no live wild bush is, and the free-eat line appeared with it).",
		build: hungryForagerAtStockedBush,
	},
	{
		name: "smith_at_commons_well_may_not_draw",
		summary: "LLM-610 paying arm: Ezekiel Crane the smith stands ON the village well's tile, on shift, with " +
			"493 coins. The well is UNOWNED and carries the shipped two-row shape — an infinite drink row and a " +
			"separate yield-only carry row holding 20 pails. He forages firewood and BUYS water, which is not a " +
			"licence to draw it. The golden pins SILENCE on the gather: no 'you can gather water here'. Because " +
			"GatherableItem is also what advertises the `gather` tool (gateTools: atGatherableSource), cue and tool " +
			"vanish together. The live case, 2026-08-06: he walked to the well needing one pail to finish a nail, " +
			"and the engine had nothing to stop fourteen such actors from stripping the mill's supply — 58 pails " +
			"between them. Drinking is untouched: that is the OTHER row, which carries no gather item at all.",
		build: smithAtCommonsWellMayNotDraw,
	},
	{
		name: "water_drawer_at_commons_well_may_draw",
		summary: "LLM-610 foil, and the arm that proves the gate protects rather than starves: the SAME well on the " +
			"SAME tile, but the subject holds `forage water`. The golden pins the gather cue present — 'you can " +
			"gather water here'. A fix that refused everyone would pass the paying arm above and still break the " +
			"village, so the pair has to move in opposite directions.",
		build: waterDrawerAtCommonsWellMayDraw,
	},
	{
		name: "hungry_holding_nibble_sees_meal_vendor",
		summary: "LLM-307: a mildly-hungry NPC (Ezekiel Crane, hunger 14 — felt, below the red threshold 18) carries " +
			"ONLY raspberries (a nibble, magnitude 2), with a cheese seller (a good meal, magnitude 8) at the General " +
			"Store. This is the live starvation-by-snacking loop (2026-07-06): the consume-first suppression used to " +
			"collapse the section to 'You have Raspberries … consume to eat' the moment he carried any food, hiding the " +
			"meal vendor for as long as a single berry was held. The golden pins the fix: the own-stock consume line, " +
			"the bridging line ('A nibble won't quiet this hunger, though — for a real meal, see the options below.'), " +
			"AND the re-opened cheese buy entry with its structure_id. Pairs with hungry_holding_meal_keeps_suppression.",
		build: hungryHoldingNibbleSeesMealVendor,
	},
	{
		name: "hungry_holding_meal_keeps_suppression",
		summary: "LLM-307 foil: the same mildly-hungry NPC and cheese seller, but Ezekiel carries a MEAL-class satisfier " +
			"(cheese, magnitude 8) instead of a nibble. A meal on hand is the answer, so the LLM-139 consume-first " +
			"suppression must STAND: the golden pins that the section shows only 'You have Cheese (a good meal) on hand — " +
			"consume to eat.' with NO bridging line and NO re-opened vendor directory. Guards the fix against over-firing " +
			"(re-opening the directory whenever any food is carried). Pairs with hungry_holding_nibble_sees_meal_vendor.",
		build: hungryHoldingMealKeepsSuppression,
	},
	{
		name: "snack_looper_redirected_to_meal",
		summary: "LLM-307 loop-coda case: the snack-loop actor (Ezekiel, mild hunger, only raspberries) is ALSO stuck in " +
			"a looping huddle conversation (ConversationLooping) with a food-less peer — the condition under which the " +
			"LLM-176 need-redirect coda renders. Before the coda fix it steered 'consume your raspberries now' while the " +
			"section above said 'for a real meal, see the options below' — a self-contradiction that re-armed the snacking " +
			"loop. The golden pins that the coda now points at the SAME meal the section does ('go to General Store … buy " +
			"a wedge of cheese to eat'), NOT the nibble. Grace Bishop makes the huddle real (carries nothing, so no " +
			"co-present buy offer competes). Fixed PublishedAt keeps the turn-state read byte-stable.",
		build: snackLooperRedirectedToMeal,
	},
	{
		name: "smith_choosing_at_forge",
		summary: "A multi-output producer (Ezekiel the blacksmith: skillet + nail) stands IDLE at his own forge on " +
			"shift — nothing in the works, the state the production-choice warrant fires on (LLM-116, redesigned LLM-319). " +
			"The golden pins the '## Your trade' scene: one paragraph per good (stock + sell-through in felt language, then " +
			"the batch affordance), the neutral 'Start a batch with produce, or see to other things.' close, and the " +
			"'thoughts turn to your trade' wake warrant. Skillet at cap reads 'stores are full' with NO affordance (a batch " +
			"can't start); empty nail gets the plain batch offer. Pairs with smith_batch_in_flight (mid-batch -> cue gone).",
		build: smithChoosingAtForge,
	},
	{
		name: "smith_no_input_readiness_beat",
		summary: "LLM-509: a producer (Ezekiel the blacksmith) idle at his forge on shift, offered BOTH a no-input " +
			"(origin) good and an inputs good in one '## Your trade' scene, so the golden contrasts the two affordance " +
			"branches side by side. The no-input shovel — the live case (2026-07-23: shown only 'takes about 4 hours', " +
			"his model invented an iron requirement, a supplier, and a debt to source it) — closes on the readiness beat " +
			"'and you have everything you need.', while the inputs skillet (forged from iron he holds) keeps the unchanged " +
			"'and you have the makings.'. Both craftable, so both list; shovel sorts first (equal zero sell-through, then " +
			"noun ascending), leading with the origin beat. Matrix guard: TestGoldensTradeSceneAffordanceAlwaysStatesReadiness.",
		build: smithNoInputReadinessBeat,
	},
	{
		name: "smith_batch_in_flight",
		summary: "The same producer (Ezekiel) at his forge with a BATCH IN FLIGHT (nail, ~30 minutes of work left) — the " +
			"steady state after a produce call (LLM-319: one-shot timed cycles replaced the continuous focus). The golden " +
			"pins that the standing 'You are making a batch of Nail — about 30 minutes of work left; it only moves along " +
			"while you're at your post.' self-state line renders, and the '## Your trade' cue (and with it the produce tool) " +
			"is GONE mid-batch — the model isn't re-invited to start what is already running. Pairs with " +
			"smith_choosing_at_forge (idle -> scene) to pin both halves of the cycle.",
		build: smithBatchInFlight,
	},
	{
		name: "tavernkeeper_missing_input_at_post",
		summary: "A multi-output producer short of an input — John Ellis idle at his tavern on shift, stew needing sage he " +
			"doesn't hold (the live LLM-257 starvation shape, re-expressed for LLM-319 one-shot batches). The golden pins " +
			"the missing-inputs clause of the '## Your trade' scene: stew renders 'A batch would take about an hour, but " +
			"you'd need more Sage first.' (meat, held in full, is NOT named), while no-input water gets the plain batch " +
			"offer — so the model steers to the makeable good or to procuring sage, never to a doomed produce call. The " +
			"production-choice wake warrant fires (water IS craftable, so there is a real decision).",
		build: tavernkeeperMissingInputAtPost,
	},
	{
		name: "smith_batch_in_flight_off_post",
		summary: "The same producer (Ezekiel) with a batch in flight but NOT at his forge — he is at the Tavern. LLM-319: " +
			"the batch exists wherever he is (its inputs are already spent), so the standing 'You are making a batch of " +
			"nail…' line RENDERS here too — its 'it only moves along while you're at your post' tail is what tells him " +
			"progress is paused until he goes back (produce_tick's gate). The '## Your trade' cue stays absent away from " +
			"the post. Inverts the retired LLM-121 focus-hiding behavior, which suppressed the line off-post " +
			"(see TestInFlightProductionLineTracksBatch).",
		build: smithBatchInFlightOffPost,
	},
	{
		name: "smith_bartering_at_tavern",
		summary: "A smith (Ezekiel) carrying his own wares stands in the Tavern in company with John Ellis the " +
			"tavernkeeper — the live LLM-125 barter scene. Off shift, away from the forge, and nothing in the works, so " +
			"neither the '## Your trade' cue nor the in-flight batch line render; the '## What your wares fetch' cue DOES, " +
			"valuing his own-trade goods (nail 1-2, skillet 5-10 from the recipe wholesale-retail spread) so a barter has a " +
			"coin yardstick instead of an invented number. No coin sales history yet (empty PriceBook), so no recent-price clause.",
		build: smithBarteringAtTavern,
	},
	{
		name: "wholesaler_producer_bartering_with_customer",
		summary: "A WHOLESALE producer (Moses James, James Farm tagged wholesaler; grows carrots + wheat) stands in " +
			"company with a would-be customer (Silence Walker) — the LLM-291 seller leg of live hud-9b23…, where Moses, " +
			"pressed to answer a buyer, hawked a retail carrot sale the wholesale gate then refused (and mis-fired the buy " +
			"verb to 'buy' his own carrots back). His produce sells only to the village distributor (Josiah Thorne), so the " +
			"'## What your wares fetch' cue draws the WHOLESALE-CHANNEL line for each own crop — who buys it, what the " +
			"distributor pays (carrots ~2 coins, wheat ~1 coin), and to send other buyers to Josiah — NOT a retail spread " +
			"that would invite the street sale. Pairs with smith_bartering_at_tavern (the ordinary, retail-priced producer).",
		build: wholesalerProducerBarteringWithCustomer,
	},
	{
		name: "keeper_reselling_in_company",
		summary: "A pure RESELLER — Josiah Thorne, general-store keeper, all-`buy` restock (cheese, milk), produces " +
			"nothing — stands in his store in company with a customer holding bought-in stock. LLM-191: his empty " +
			"ProduceEntries() left the '## What your wares fetch' cue blank before, so he named prices with no anchor and " +
			"never reliably moved stock (live: 0 coins, empty sell-through). The golden pins the cue now valuing his resold " +
			"goods from the recipe wholesale-retail spread AND surfacing his own recent purchase cost ('you have lately " +
			"paid about N each for it') from the buyer-side PriceBook — the cost basis to mark up from. No sale history " +
			"yet, so no 'sold for' clause and (LLM-385) no below-cost caution — that fires only for a realized sale at or below cost, so the bare cost basis renders alone. Pairs with smith_bartering_at_tavern.",
		build: keeperResellingInCompany,
	},
	{
		name: "keeper_paid_with_long_note",
		summary: "LLM-400: a seller (Josiah Thorne) perceives a payment whose buyer attached a long note — 200 runes, " +
			"exactly the cap the pay tools enforce on `for`. Pins that the note reaches him WHOLE. The warrant used to " +
			"pre-cut it at 220 runes with no marker, handing the seller a reason that stops mid-clause as though the buyer " +
			"had finished saying it; the cut could not fire through a real tool path (200 < 220) but nothing linked the two " +
			"constants, so raising the tool cap would have switched it on silently. Also the matrix's only pay-warrant " +
			"prompt: the seller-just-paid line renders in the live village daily and had no golden. Matrix guard: " +
			"TestGoldensWarrantTextCompleteOrMarked; marked-elision half: TestRenderWarrants_OverCapPayForTextIsMarkedElided.",
		build: keeperPaidWithLongNote,
	},
	{
		name: "pc_customer_present_in_huddle",
		summary: "Control for pc_customer_stepped_away_in_huddle: a keeper (Josiah Thorne) in company with a LIVE " +
			"PC customer (Wendy, fresh presence stamp). She is an addressable huddle member, named in the '## Around " +
			"you' company line. Pairs with the stepped-away golden so the diff isolates the LLM-342 away-vs-present split.",
		build: keeperWithPresentPCCustomer,
	},
	{
		name: "pc_customer_stepped_away_in_huddle",
		summary: "LLM-342: the same keeper+PC-customer huddle, but the player's client has gone quiet (nil presence " +
			"stamp — a hidden tab whose WebSocket also dropped, or a departed player). Wendy stays co-present in '## " +
			"Around you' as having 'stepped away', but is NOT in the addressable company set, so no cue drives Josiah to " +
			"keep addressing an absent player. Pins the co-present-PC-has-gone-quiet, away-vs-present distinction.",
		build: keeperWithSteppedAwayPCCustomer,
	},
	{
		name: "distributor_underwater_resale",
		summary: "LLM-332 underwater escalation: the same reseller shape as keeper_reselling_in_company, now with realized " +
			"SALE history that makes one line demonstrably underwater. Josiah bought milk at ~2 and has been selling it at " +
			"~1 (the live −51-coin milk leak that drove him to rationally cut the line, starving the stew chain), while " +
			"cheese carries a healthy markup (bought ~2, sold ~4). The golden pins BOTH branches: the underwater milk line " +
			"escalates past the bare caution to name the two levers a merchant holds ('you may need to negotiate lower " +
			"costs or raise your price'); the healthy cheese line carries NO caution at all (LLM-385: caution only for a good sold " +
			"at or below cost, on the sub-coin sale-vs-buy rates). It gives a rational loss-cutter a path back to margin, not " +
			"a scolding on a profitable line.",
		build: distributorUnderwaterResale,
	},
	{
		name: "distributor_overbuying_below_resale",
		summary: "LLM-385/LLM-424/LLM-427 buy-side: a reseller (Josiah) low on milk with a walk-to supplier (Ellis Farm), " +
			"whose OWN books show what the pre-LLM-385 '## Restocking' cue could not flag. He resells milk at ~1 coin " +
			"(seller ring: 9 units for 12 coins) while the going rate is ALSO ~1, so the margin judgment grades the two " +
			"displayed rates break-even ('You pay about 1 coin for it and resell at about 1 coin, so it earns you nothing " +
			"on its own') — the LLM-427 tiered verdict that replaced the warning-tailed anchor+ceiling sentence pair and " +
			"the aggregate cost-vs-sales P&L (which here read 40 cost / 12 sales, a phantom −28 on a sub-coin-profitable " +
			"line). Demand grades steady (9/week). And he bought 35 milk this week but sold only 9, so the self-use " +
			"accounting fires ('bought about 35 of these this past week, sold only 9, and used or traded the rest') — the " +
			"LLM-424 honest reframe that replaced the backwards 'buy sparingly' hold-off, since an item below its reorder " +
			"point has not piled up on the shelf. Solo (no huddle), so the wares-fetch cue stays out and the section under " +
			"test renders cleanly.",
		build: distributorOverbuyingBelowResale,
	},
	{
		name: "keeper_self_consumes_zero_stock",
		summary: "LLM-424 live case (Josiah Thorne, 2026-07-15): a storekeeper SOLD OUT of cheese (0 on hand, cap 6) who " +
			"has been buying it far faster than he sells — bought ~25 this week, sold only ~5 — because he grazes his own " +
			"stock (no engine spoilage, so the buy>>sell gap was consumed or bartered away, not shelved). The pre-LLM-424 " +
			"cue rendered a self-contradiction: '0 on hand, room for 6' AND the overstock steer 'restocking faster than it " +
			"sells, so buy sparingly, if at all', and the weak model confabulated '22 still unsold' to reconcile the two, " +
			"talking itself out of restocking a sold-out line. The golden pins the fix: the honest self-use accounting " +
			"('bought about 25 of these this past week, sold only 5, and used or traded the rest') AND the normal " +
			"restock affordance (walk to Ellis Farm), with NO 'buy sparingly' / overstock hold-off. It also pins the " +
			"LLM-427 sequel to the same live case: slow demand and an EARNING margin verdict ('Each one earns you coin: " +
			"you buy at about 1 coin and resell at about 2 coins') in place of the aggregate cost-vs-sales P&L, which " +
			"here read 25 cost / 10 sales — a phantom −15 the model subtracted into 'a fool's arithmetic I don't mean " +
			"to repeat' and refused to restock a per-unit-profitable line over. Solo (no huddle), so the wares-fetch cue " +
			"stays out and the '## Restocking' section renders cleanly.",
		build: keeperSelfConsumesZeroStock,
	},
	{
		name: "innkeeper_pricing_with_makings_cost",
		summary: "A PRODUCER whose recipe has real inputs — Hannah Boggs keeping her inn in company with a guest, " +
			"porridge made 10 bowls at a time from 3 milk + 5 water (the live catalog shape). LLM-226: the wares-worth " +
			"cue previously gave a producer no cost anchor, so she could price below cost unknowing (live: 1-coin " +
			"porridge against an 0.8-coin makings cost). The golden pins the makings clause: inputs priced from catalog " +
			"wholesale with no purchase history (8 coins a batch), spoken per-unit as 'nearly 1 coin each' — the engine " +
			"does the division and rounds the prose UP, never down to a break-even-erasing 'about 1'. Stated as a fact " +
			"with no pricing directive (LLM-227) — the NPC decides what to do with its cost. She has a porridge batch in " +
			"flight (LLM-319), so the standing in-progress line renders and the '## Your trade' cue stays out of the " +
			"company scene. Pairs with keeper_reselling_in_company (the resale cost basis) and smith_bartering_at_tavern " +
			"(the no-inputs producer, no makings clause).",
		build: innkeeperPricingWithMakingsCost,
	},
	{
		name: "producer_input_below_batch_floor_reorders",
		summary: "LLM-279 produce-input batch floor: Hannah Boggs makes porridge (3 milk + 5 water per 10-bowl batch) " +
			"and is low on WATER at 4 — stranded in the deadlock band, above the cap fraction (derived cap 15 → fires " +
			"only below 3.75) yet below a single 5-unit batch, so she can't cover the next batch but the old cap-fraction " +
			"rule would never reorder her. A well-keeper sells water. The golden pins that BOTH the '## Restocking' cue " +
			"(walk-to the well for water) AND the '## Keeping up production' runway line now render for water — the batch " +
			"floor (2×5=10) catching what the fraction skipped — while MILK, stocked at 9 (above its 2×3=6 floor), stays " +
			"silent. She has a porridge batch in flight (LLM-319: the state in which input runway matters most), so the " +
			"standing in-progress line renders and the '## Your trade' cue is gated off. Guards the " +
			"reorder-on-batch-coverage fix end to end at the perception layer.",
		build: producerInputBelowBatchFloorReorders,
	},
	{
		name: "reseller_arrives_at_supplier_buy_here_no_huddle",
		summary: "LLM-286 arrival tick: John Ellis, a tavernkeeper reselling ale, walked to the Brewery to restock and " +
			"stands inside it with the brewer Anders (an ale PRODUCER), but NO huddle exists yet — one forms only when " +
			"someone speaks. pay_with_item bootstraps the co-located huddle on the call itself (withHuddleBootstrap, " +
			"ZBBS-HOME-400), so the keeper IS reachable this tick. The golden pins that the '## Restocking' section renders " +
			"the concrete 'Anders Brewer is here with you and sells ale. Buy it now …' imperative, NOT the 'No seller is " +
			"here now — use move_to …' walk-to list that would wrongly point him to the very Brewery he stands in. Before " +
			"LLM-286 the huddle-only co-presence gate could not fire on an arrival tick, so the buyer was told to walk to " +
			"where he already was (live: zbbs-john-ellis, virtual_agent_calls id 63123).",
		build: resellerArrivesAtSupplierBuyHereNoHuddle,
	},
	{
		name: "reseller_copresent_sage_seller_present",
		summary: "LLM-308 goad foil: Elizabeth Ellis, a shopkeeper reselling sage, shares a huddle with Josiah Thorne (a " +
			"sage forager holding 12) and is out of sage. No prior offers and he is well-stocked, so the '## Restocking' " +
			"co-present imperative fires clean — 'Josiah Thorne is here with you and sells sage. Buy it now …, a qty up to " +
			"4 …'. The foil for reseller_copresent_sage_standoff (the same setup after two declines softens the goad away).",
		build: resellerCoPresentSageSellerPresent,
	},
	{
		name: "reseller_copresent_sage_seller_low_stock",
		summary: "LLM-308 stock-cap arm: the reseller_copresent_sage_seller_present setup, but Josiah holds only 1 sage " +
			"against the 4 Elizabeth's shelf has room for. Affordable and no prior offers, so the buy still stands — the " +
			"render caps 'a qty up to N' at his 1 ('can spare only 1 just now') instead of goading the full 4 for stock he " +
			"can't deliver (the live 'a qty up to 3' against a seller holding 1).",
		build: resellerCoPresentSageSellerLowStock,
	},
	{
		name: "reseller_copresent_sage_standoff",
		summary: "LLM-308 standoff arm (the live sage loop): the reseller_copresent_sage_seller_present setup with two prior " +
			"sage offers to Josiah already declined IN THIS HUDDLE on the pay ledger — the standoff threshold. The golden " +
			"pins the '## Restocking' co-present line softening to 'your offers for sage aren't finding a deal right now — " +
			"hold off and come back later …' and the ABSENCE of 'Buy it now', so the cue stops driving the unbounded " +
			"offer→decline loop (11 rounds live). Josiah stays well-stocked (12), so the soften is standoff-driven, not the " +
			"stock cap. Pairs with TestGoldensNoCoPresentBuyGoadAfterTwoDeclines (the matrix-wide exclusion).",
		build: resellerCoPresentSageStandoff,
	},
	{
		name: "keeper_not_pitching_makers_own_ware",
		summary: "LLM-171 seller side: John Ellis keeps his tavern in company with Ezekiel Crane the smith, and John's " +
			"stock holds skillet + nail he bought FROM Ezekiel. The '## Custom at hand' cue lists those wares to pitch, so " +
			"the golden pins the producer-note line that steers the keeper off selling a smith his own ware back (the live " +
			"buy-back: John read Ezekiel's sell-offer as a buy and quoted skillets at him). A customer who makes none of " +
			"the goods draws no note (see TestProducerPitchNoteOnlyForCoPresentMaker).",
		build: keeperNotPitchingMakersOwnWare,
	},
	{
		name: "seller_huddled_with_laboring_peer",
		summary: "LLM-231: John Ellis keeps his tavern in company with two peers — Patience Walker, mid-job for Josiah " +
			"Thorne (a Working LaborOffer, StateLaboring), and Grace Bishop, free. The live shape: John burned ~20 ticks " +
			"re-pitching a laboring Patience because nothing told him she was busy. The golden pins that the '## Around you' " +
			"line annotates Patience busy ('working a job for Josiah Thorne just now — not free to trade') while Grace reads " +
			"plainly, AND that the seller offer cue lists Grace but NOT Patience (a worker mid-job is not a pitch target). The " +
			"busy annotation deliberately does not say 'won't respond' — a laborer can still answer speech (LLM-230). Pairs " +
			"with TestGoldensLaboringPeerNotAnOfferTarget (the matrix-wide exclusion).",
		build: sellerHuddledWithLaboringPeer,
	},
	{
		name: "seller_employing_own_laboring_worker",
		summary: "LLM-231 employer-seller case: John Ellis keeps his tavern (stew to sell) while a worker he himself " +
			"hired, Silence Walker, labors for him (a Working LaborOffer with John as employer), alongside Grace Bishop, a " +
			"free customer. The golden pins that Silence is STILL dropped from the '## Custom at hand' offer cue (a worker " +
			"mid-job isn't a sale target even for their own employer) while Grace is listed — AND that Silence carries NO " +
			"busy annotation in '## Around you' (the employer gets the richer '## Workers currently working for you' cue " +
			"instead; the annotation is bystander-only). Complements seller_huddled_with_laboring_peer (the bystander case).",
		build: sellerEmployingOwnLaboringWorker,
	},
	{
		name: "maker_offered_own_ware_buy_quote",
		summary: "LLM-171 buyer side: Ezekiel Crane (skillet at his cap of 5, which he makes) has a targeted skillet " +
			"quote posted at him by John Ellis for 2 coins — the mis-pitched buy-back quote from the live trace. The " +
			"golden pins that the quote warrant line WITHHOLDS the 'pay_with_item with quote_id' take and steers 'these " +
			"are wares you make yourself … decline' instead, so a mis-pitched quote can't close the buy-back loop. A " +
			"quote for a good the buyer doesn't make keeps its take (see TestBuyBackQuoteSteerOnlyForOwnProducedOrAtCap).",
		build: makerOfferedOwnWareBuyQuote,
	},
	{
		name: "buyer_offered_quote_take_names_terms",
		summary: "LLM-172 buyer side: John Ellis posts a targeted STEW quote (qty 1, 4 coins) at Ezekiel Crane — a good he " +
			"buys, not makes — so the actionable take RENDERS (unlike the maker buy-back above). Ezekiel carries 20 nails, " +
			"the live trap: the prior take said 'pay_with_item with quote_id 1 and the same item, qty, and amount', and a " +
			"buyer holding other goods bound 'the same item' to a nail, dead-ended on the term-mismatch reject, and fell " +
			"back to a bare pay that leaked coins for an undelivered stew (the quote still open). The golden pins that the " +
			"take now names the concrete 'item \"stew\", qty 1, and amount 4' so there is nothing to misbind. Only golden " +
			"exercising the single-line coin-quote actionable take (see TestCoinQuoteTakeNamesConcreteTerms).",
		build: buyerOfferedQuoteTakeNamesTerms,
	},
	{
		name: "buyer_offered_bundle_quote_counter_path",
		summary: "LLM-561: John Ellis posts a targeted multi-line BUNDLE quote (iron 1, salt 1, 2 breeches — 50 coins) at " +
			"Ezekiel Crane, who already holds 10 iron — the live shape: a buyer who wants part of a bundle, not the whole. " +
			"The golden pins that the bundle take-instruction now names a way out — take the whole bundle by quote_id, OR " +
			"make a free-form pay_with_item offer (no quote_id, one kind of good per offer) the seller can accept or " +
			"counter. Before LLM-561 the arm ended at 'it settles at once': a dead-ended buyer improvised (item \"bundle\", " +
			"then a prose haggle settled as 35 coins against a 3-coin salt, ~3% of the village money supply destroyed). " +
			"The escape deliberately steers to coin offers, not offer_trade (a bundle has no single want_item — LLM-136).",
		build: buyerOfferedBundleQuoteCounterPath,
	},
	{
		name: "buyer_sees_standing_quote_after_warrant_spent",
		summary: "LLM-551: Patience Walker stands at Josiah Thorne's counter with 12 coins and no flour. His targeted " +
			"4-flour/12-coin quote is ACTIVE, but its warrant was consumed turns ago — warrants is nil here, which is the " +
			"whole point. Before this, a targeted quote reached its buyer exactly once, on the tick it was posted; a buyer " +
			"who spent that tick answering aloud (speak is terminal, so this is the common case) never saw the offer again " +
			"and lost the quote_id with it. The live cost: 22 minutes and ~20 rejected pay_with_item calls while the " +
			"seller's own prompt showed the offer standing the whole time. The golden pins that '## Offers made to you' " +
			"names the seller, the terms, and the quote_id take-instruction on a tick with no warrant at all. Buyer-side " +
			"twin of seller_with_taken_quote_at_post.",
		build: buyerSeesStandingQuoteAfterWarrantSpent,
	},
	{
		name: "dairy_choosing_at_farm",
		summary: "LLM-144: a NON-smith multi-output producer (Elizabeth Ellis at Ellis Farm: milk + meat + cheese) stands " +
			"IDLE at her own workplace on shift — the same production-choice state smith_choosing_at_forge pins for the " +
			"blacksmith, but for a dairy/farm trade. The golden proves the '## Your trade' scene and wake warrant render " +
			"trade-neutrally (LLM-319) — one stock/sales/affordance paragraph per good across three stock tiers (empty " +
			"meat, low cheese, fair milk) and the 'thoughts turn to your trade' warrant — NOT blacksmith-only 'forge' " +
			"wording a dairywoman was wrongly shown (the live Elizabeth cheese scene 019f0969). Mirrors " +
			"smithChoosingAtForge; byte-stable.",
		build: dairyChoosingAtFarm,
	},
	{
		name: "keeper_offers_room_to_coinless_guest",
		summary: "John Ellis the tavernkeeper shares his tavern (one free private room at a live nightly rate) with " +
			"Ezekiel Crane, a homeless smith with no home, no lodging grant, and 0 coins, carrying his own wares. The " +
			"'## A room to let' cue fires and now names the goods-for-room path (LLM-136): a coinless guest is offered " +
			"the room for goods (offer_trade → accept_pay) rather than dead-ended on coins. Keeper side of the live livelock.",
		build: keeperOffersRoomToCoinlessGuest,
	},
	{
		name: "homed_guest_lodging_quote_suppressed",
		summary: "LLM-208 buyer side: John Ellis posts a targeted nights_stay (room) quote at Prudence Ward, but Prudence " +
			"HAS a home (Ward Residence) — she structurally can't take a room (the buyer-side pay_with_item guard rejects " +
			"it, LLM-182). The golden pins that the room-offer take is SUPPRESSED for her: filterHomedLodgingQuoteWarrants " +
			"drops the lodging quote warrant at build, so the prompt carries no 'offers you … nights_stay' take line and she " +
			"isn't pulled into a doomed nightly negotiation (the live John↔Prudence tavern loop). Contrast " +
			"keeper_offers_room_to_coinless_guest (a HOMELESS seeker, who correctly DOES get offered the room).",
		build: homedGuestLodgingQuoteSuppressed,
	},
	{
		name: "peers_holding_same_food_no_degenerate_buy",
		summary: "Two hungry NPCs stand together, each already carrying the same food (stew) — the LLM-138 " +
			"degenerate-buy shape from live hud-6a887a…, where each was told ONLY to BUY the other's blueberries " +
			"(the cue that drove the hollow 'I can offer thee blueberries' beats backed by no transaction). The golden " +
			"pins that the '## What you can eat or drink' section shows the subject its OWN stew to consume but carries " +
			"NO 'offer to buy it from them' peer line — buying a copy of food already in hand is pointless " +
			"(gatherCoPresentPeerOffers gate). A regression would make the buy line reappear in the diff.",
		build: peersHoldingSameFood,
	},
	{
		name: "hungry_buyer_with_wholesaler_peer_no_buy_cue",
		summary: "A hungry buyer (Silence Walker, coins in hand) huddles with a wholesaler-farmer (Moses James, work " +
			"anchor tagged wholesaler) carrying stew — the LLM-289 shape from live hud-843da92a…, where the co-present " +
			"peer cue said 'Moses James is here with you, carrying Carrots — you could offer to buy it from them now' and " +
			"every cued pay_with_item died on the LLM-223/252 wholesale gate (40 of the huddle's 57 turns). The golden " +
			"pins that the '## What you can eat or drink' section carries NO peer-buy line for the wholesaler's goods: " +
			"the peer cue now applies the same SellerAtWholesaler/ActorIsDistributor pair as the dispatch gate. A " +
			"regression would make the doomed buy line reappear in the diff.",
		build: hungryBuyerWithWholesalerPeer,
	},
	{
		name: "coinless_worker_among_peers",
		summary: "Two laborers stand together in the commons and the one we render (Goodwife Bishop, a newcomer) has " +
			"an empty purse — the LLM-153 situation, where 0-coin workers tried to BUY services from each other. The pay " +
			"path rejects a coinless buy, but the model kept attempting it because '## You' showed 'Coins in your purse: 0' " +
			"with no consequence. The golden pins the empty-purse line now spelling out that the actor cannot pay until it " +
			"earns coin — coin-specific wording so barter (offer_trade) is left untouched.",
		build: coinlessWorkerAmongPeers,
	},
	{
		name: "broke_employer_cannot_pay_labor_offer",
		summary: "A worker (Lewis Walker) has solicited the subject (Ezekiel Crane) for a 5-coin job, but the subject " +
			"has an empty purse — the LLM-158 situation. accept_work's funds gate (buyerCanAfford) would only flip the " +
			"offer to failed_unavailable, so the model 'accepts' verbally and the deal dies in silence (the live ~10-min " +
			"Lewis<->Ezekiel blacksmith dead-air). The golden pins the affordability steer: the unaffordable offer is " +
			"directed to decline_work WITH an explicit speak, and the generic accept_work/decline_work footer is suppressed " +
			"because no offer is affordable.",
		build: brokeEmployerCannotPayLaborOffer,
	},
	{
		name: "worker_en_route_to_workplace",
		summary: "LLM-229: a worker (Patience Walker) has accepted a job for Josiah Thorne struck away from his store " +
			"and is now relocating to his workplace — she is NOT yet laboring, so no coins/boost accrue and she must not " +
			"statue where the deal was struck. The golden pins the relocation self-state ('You've taken on a job for Josiah " +
			"Thorne — make your way to their workplace and get to work…'), and — because she already holds a committed job — " +
			"the absence of both the solicit affordance and the businesses directory. The matrix-wide guard is " +
			"TestGoldensEnRouteWorkerNotOfferedNewWork.",
		build: workerEnRouteToWorkplace,
	},
	{
		name: "worker_offered_work_by_keeper",
		summary: "LLM-346: Prudence Ward has asked Lewis Walker to lend a hand at her apothecary — an employer-initiated " +
			"Pending offer. The subject is the WORKER. The golden pins that he is given a work tool at all: the decision " +
			"section names her, the terms, and the offer id he echoes into accept_work/decline_work. It also pins the " +
			"absence of the affordability steer (he cannot see her purse) and of the solicit affordance (he holds 26 coins, " +
			"above the seek-work comfort ceiling — his hustle is silenced, his answer is not). Live, this prompt advertised " +
			"no work-taking tool at all and he stood at her door for 45 minutes.",
		build: workerOfferedWorkByKeeper,
	},
	{
		name: "keeper_can_offer_work_to_co_present_worker",
		summary: "LLM-346: the mint side of the same room, before anyone has offered anything. The subject is Prudence " +
			"Ward, keeper of the apothecary, with Lewis Walker co-present. The golden pins the offer_work affordance " +
			"NAMING him — nothing else in the prompt reveals which villagers take work for pay, and offer_work resolves " +
			"its target by exact display name — and pins the warning off the terminal speak (asking aloud with speak " +
			"would end her turn before the offer was ever made, the LLM-343 collision).",
		build: keeperCanOfferWorkToCoPresentWorker,
	},
	{
		name: "labor_offer_in_kind_reward",
		summary: "A worker (Anne Walker) has solicited the subject (Hannah Boggs) for a job paid in kind — 1 porridge " +
			"plus 2 coins — and the subject holds both legs. The LLM-225 situation: spoken in-kind hire terms ('a bowl " +
			"of porridge for some help') are now real contract terms, not talk that evaporates at commit (the live " +
			"Hannah Boggs Inn hires, where the workers ended up buying the promised porridge with their own coins). The " +
			"golden pins the decision line naming BOTH reward legs via the payment phrase, and the normal " +
			"accept_work/decline_work footer (the offer is affordable — the employer holds the porridge and the coins).",
		build: laborOfferInKindReward,
	},
	{
		name: "employer_missing_reward_items_steer",
		summary: "The same in-kind solicitation, but the subject does NOT hold the asked porridge (coins are ample) — " +
			"the goods-leg half of the LLM-158 affordability steer (LLM-225). accept_work's gate 8 now spans both legs " +
			"(employerCanCoverLaborReward), so an accept would only flip the offer to failed_unavailable and the deal " +
			"would die in silence. The golden pins the missing-goods decline steer ('You do not hold the 1 porridge " +
			"they ask to be paid in') and the suppressed footer (no affordable offer remains).",
		build: employerMissingRewardItemsSteer,
	},
	{
		name: "employer_recalls_returning_helper",
		summary: "A producing keeper (Hannah Boggs, who makes porridge) is solicited again by Anne Walker, who completed " +
			"a paid job for her a few hours ago (an Active ObservedHelpedByWorker memory). The LLM-228 situation: rather than " +
			"an engine hire-value pitch at the decision point (that pitch shipped in #690 and was pulled in #691), the " +
			"decision section recalls the past help experientially. The golden pins the returning-helper recall WITH the " +
			"added-output clause ('You remember Anne Walker lending you a hand recently, and you got more done for it.') — a " +
			"producing employer really does get more done from help — above the normal accept/decline footer. Pairs with " +
			"employer_recalls_returning_helper_nonproducer.",
		build: employerRecallsReturningHelper,
	},
	{
		name: "employer_recalls_returning_helper_nonproducer",
		summary: "The same returning-helper solicitation, but the employer makes no goods itself (no makeable produce " +
			"entry). The golden pins the bare social recall ('You remember Anne Walker lending you a hand recently.') with NO " +
			"'got more done' clause — a non-producer never claims output it did not make (LLM-228). Pairs with " +
			"employer_recalls_returning_helper.",
		build: employerRecallsReturningHelperNonProducer,
	},
	{
		name: "worker_among_household_no_solicit",
		summary: "Two worker-tagged Walker siblings (Lewis + Anne) stand together in their own home, both jobless — the " +
			"LLM-157 situation, where housemates solicited each other for work ('I'm looking for work, does anyone need a " +
			"hand?'). LLM-145 already hides the solicit_work tool among kin, but the seek-work backstop warrant still made " +
			"the model ask the housemate as freeform speech. The golden pins the '## Around you' annotation that now names " +
			"Anne as the subject's housemate, so the worker reads her as kin rather than a work prospect and steers to a " +
			"real employer instead. A non-kin co-present worker would carry no such annotation.",
		build: workerAmongHousehold,
	},
	{
		name: "owner_at_worn_stall",
		summary: "A business owner (Ezekiel, a smith) stands at his own worn premises (wear past the repair threshold, " +
			"below degrade) carrying too few nails to mend it. The golden pins the '## Your business' cue: the wear " +
			"problem AND the buy-nails-from-the-smith steer in one line (symmetrical awareness, LLM-118). The repair tool " +
			"rides the same StallRepair signal (handlers gating test).",
		build: ownerAtWornStall,
	},
	{
		name: "owner_at_worn_stall_with_nail_supplier",
		summary: "LLM-274: a business owner (Elizabeth Ellis) stands at her own worn Ellis Farm with 0 nails while a " +
			"SEPARATE open nail supplier — Ezekiel, the blacksmith, holding 21 nails at the Blacksmith — exists in the " +
			"world. The golden pins the destination-bearing '## Your business' steer: findItemVendors resolves the smith, " +
			"so the cue names 'buy from Blacksmith (destination: blacksmith)' with move_to + pay_with_item, replacing the " +
			"dead-end 'the smith' that llama-3.3-70b narrated but never walked (the live 2026-07-04 case). Its foil is " +
			"owner_at_worn_stall, where no other supplier exists and the generic sentence is correctly kept.",
		build: ownerAtWornStallWithNailSupplier,
	},
	{
		name: "owner_at_worn_stall_nail_keeper_copresent",
		summary: "LLM-504 (the live 2026-07-21 John Ellis case): a tavernkeeper stands at his own worn tavern, one nail " +
			"short of a mend, while the nail supplier's keeper — Ezekiel, whose workplace is the off-scene Blacksmith — is " +
			"standing IN the tavern with him. The golden pins the co-present-keeper name on the walk-to bullet: 'buy from " +
			"Blacksmith (Ezekiel Crane) (destination: blacksmith)'. The name links shop to person so the model asks the man " +
			"in front of it instead of walking to his shut smithy; deliberately NO new imperative and no change to the duty " +
			"steer, which still renders its step-out form. Its foil is owner_at_worn_stall_with_nail_supplier (Ezekiel far " +
			"away → the same bullet, name-free).",
		build: ownerAtWornStallNailKeeperCoPresent,
	},
	{
		name: "owner_off_post_at_smith_short_nails",
		summary: "LLM-277 (the live 2026-07-04 failure): Elizabeth Ellis, owner of the worn Ellis Farm with 0 nails, has " +
			"walked OFF her farm and shares the smith's huddle with Ezekiel (21 nails, the nail producer). The golden pins " +
			"the off-post '## Nails to mend your business' cue with the co-present pay_with_item buy-here imperative naming " +
			"Ezekiel — the second half of LLM-274 — AND the absence of any return-to-post steer (she is on-shift off her " +
			"post, so the to-work nag WOULD fire, but the active nail-buy errand suppresses it). Before LLM-277 she got no " +
			"buy cue and a go-home nag here, so she left without buying.",
		build: ownerOffPostAtSmithShortNails,
	},
	{
		name: "owner_off_post_short_nails_walking",
		summary: "LLM-277 walk-to arm: Elizabeth, 0 nails, is off her worn farm and NOT co-present with the smith (no shared " +
			"huddle). The golden pins the same '## Nails to mend your business' cue naming the walk-to destination ('buy from " +
			"Blacksmith (destination: blacksmith)') and no return-to-post steer — the 'while away' half of the errand that " +
			"persists across the whole walk, not just at the farm. Foil of owner_off_post_at_smith_short_nails.",
		build: ownerOffPostShortNailsWalking,
	},
	{
		name: "owner_off_post_enough_nails",
		summary: "LLM-277 negative arm: Elizabeth is off her worn farm but already carries enough nails (5 == NAILS_PER_REPAIR) " +
			"to mend it, so there is no buy errand — the '## Nails to mend your business' cue is ABSENT. With no errand to " +
			"defer, the to-work nag correctly fires (head back to the post to mend), pinning that the suppression is " +
			"conditional on an actual nail shortfall.",
		build: ownerOffPostEnoughNails,
	},
	{
		name: "keeper_conserving_owes_nail_repair",
		summary: "LLM-297 invariant anchor (the live 2026-07-06 Josiah case): a shopkeeper (Josiah Thorne) whose " +
			"working capital has collapsed — 1 coin, below the 10-coin floor, a full shelf of unsold cloth — owns his " +
			"worn General Store and has stepped away from it to the Blacksmith, sharing Ezekiel Crane's huddle. Two " +
			"standing sections fire: '## Restocking' flips to the coin-poor 'Hold off buying more for now' steer, and the " +
			"off-post '## Nails to mend your business' errand sits him in front of the nail seller. Before LLM-297 the " +
			"errand goaded 'Buy it now' into that thin purse, contradicting the restock advice; merchantConserve now softens " +
			"it to a hold-off, so the two cues agree. The non-vacuous anchor for TestGoldensBuyNowGoadNeverBesideHoldOff.",
		build: keeperConservingOwesNailRepair,
	},
	{
		name: "innkeeper_input_pile_not_overstock",
		summary: "LLM-462 anchor (the live 2026-07-18 Hannah Boggs deadlock): an innkeeper with 2 coins — below the " +
			"10-coin floor — a BARE porridge shelf, 19 water (5 to a batch of porridge) and the 1 flour her line has run " +
			"out of. The water pile used to clear the dead-stock floor on its own, reading her raw material as unsold " +
			"merchandise: '## Restocking' flipped to the hold-off steer and the sim mirror suppressed every buy warrant, " +
			"so she was never woken to fetch flour and her busiest line went dead. Scoped to actual wares she is plainly " +
			"empty-shelved, so the golden pins the ORDINARY buy cue with the miller's walk-to destination and the ABSENCE " +
			"of the 'Hold off buying more for now' lead. Foil of keeper_conserving_owes_nail_repair (overstocked on a " +
			"pure resale good → still conserving).",
		build: innkeeperInputPileNotOverstock,
	},
	{
		name: "owner_standoff_declined_nails",
		summary: "LLM-297 standoff arm: Elizabeth Ellis, off her worn farm and sharing the smith's huddle with Ezekiel, " +
			"has already had two nail offers to him declined in this huddle. She is not a keeper (no restock policy, so " +
			"merchantConserve never fires), so the softening is driven purely by the dead-ended negotiation — the cue drops " +
			"'Buy it now' for a hold-off rather than goading a third offer into the same no. Foil of " +
			"owner_off_post_at_smith_short_nails (same co-present setup, no prior offers → the buy imperative still stands).",
		build: ownerStandoffDeclinedNails,
	},
	{
		name: "owner_standoff_aged_declines_nails",
		summary: "LLM-510 latch arm (the live Elizabeth↔Ezekiel nail loop): the owner_standoff_declined_nails setup " +
			"with both declines aged 20 minutes — well past recentlyResolvedOfferWindow — while the huddle runs on. " +
			"The standoff latches for the negotiation's lifetime: the hold-off still renders instead of the 'Buy it " +
			"now' goad. Before the fix the 3-minute recency filter read these as stale, the counter fell back below " +
			"threshold, and the goad returned between wake-backoff-paced re-offers (13 declines in 53 minutes live).",
		build: ownerStandoffAgedDeclinesNails,
	},
	{
		name: "owner_short_nails_seller_low_stock",
		summary: "LLM-297 stock-cap arm: Elizabeth shares the smith's huddle with Ezekiel, but he holds only 2 nails " +
			"against the 5 a repair needs. Affordable and no prior offers, so the buy still stands — but the qty is capped " +
			"at his stock ('can spare only 2 … a qty up to 2') instead of goading 'up to 5' for stock he can't deliver (the " +
			"live smith-held-only-1-nail case). Foil of owner_off_post_at_smith_short_nails (smith well-stocked → the full " +
			"shortfall is asked).",
		build: ownerShortNailsSellerLowStock,
	},
	{
		name: "owner_at_degraded_stall",
		summary: "A business owner stands at his own DEGRADED premises (wear past the degrade threshold) carrying enough " +
			"nails, at the live default StallDegradedProducePct (LLM-446: work continues slowed; restock buying stays " +
			"shut; still sells on-hand stock). The golden pins the slowed-not-shut '## Your business' framing ('work goes " +
			"on, though slowly') plus the 'use the repair tool now to fix it' steer.",
		build: ownerAtDegradedStall,
	},
	{
		name: "owner_at_degraded_stall_produce_blocked",
		summary: "LLM-446 legacy arm: the same degraded stall with StallDegradedProducePct dialed to 0 (the LLM-304 " +
			"full block, still an operator mode). The golden pins the blocked wording ('too worn to keep stock … can't " +
			"can't take in shelf stock from suppliers or make more until you mend it') so the two tiers can't blur.",
		build: ownerAtDegradedStallProduceBlocked,
	},
	{
		name: "degraded_smith_short_of_own_nails",
		summary: "LLM-446 live-bug fixture (the 2026-07-16 Ezekiel↔Josiah 'three more minutes' loop): the sole nail " +
			"producer at his own DEGRADED Blacksmith, 0 of the 5 repair nails, a nail batch mid-flight. Pins the three " +
			"ways out rendering together — the slowed in-flight batch line (an honest clock, not the frozen 'about 3 " +
			"minutes' re-promise), the 'nails are your own work: forge what you're short' mend steer (not a buy errand " +
			"to a supplier who doesn't exist), and the slowed-not-shut '## Your business' framing.",
		build: degradedSmithShortOfOwnNails,
	},
	{
		name: "degraded_smith_short_of_own_nails_produce_blocked",
		summary: "LLM-446 code_review regression arm: the sole nail producer's fixture under the legacy pct-0 full " +
			"block. StartProductionCycle rejects here, so the golden pins the ABSENCE of the forge-your-own steer (which " +
			"would goad a refused production) — the plain shortfall fallback renders instead — and the halted in-flight " +
			"batch line ('the work stands still') in place of the frozen-clock re-promise.",
		build: degradedSmithShortOfOwnNailsProduceBlocked,
	},
	{
		name: "owner_at_degraded_store_conserving",
		summary: "LLM-304/301 (slowed framing since LLM-446): Josiah Thorne stands at his own DEGRADED General Store (restock buying shut, work slowed, still sells on-hand stock) with 0 of the 5 " +
			"nails a mend takes, 1 coin (below the 10-coin MerchantCoinFloor) and 17 unsold flour — conserve active — and " +
			"NO nail supplier survives the LLM-216 drops. The golden pins the vendor-less fallback's sell-first soften " +
			"('your purse can't take on nails just now') and the ABSENCE of the old destination-less 'buy more from the " +
			"smith' goad, which llama-3.3-70b answered by inventing 'the Smithy' and burning its turn retrying the move " +
			"(the live 2026-07-06 scene).",
		build: ownerAtDegradedStoreConserving,
	},
	{
		name: "owner_conserving_with_nail_supplier",
		summary: "LLM-301 (code_review arm): a conserving owner at her worn farm while a nail supplier DOES survive " +
			"findItemVendors (unknown price, so the affordability drop keeps him). Conserve must WIN over the vendor " +
			"list — the golden pins the sell-first soften and the ABSENCE of the 'Use move_to to reach a supplier' walk-to " +
			"goad, so this cue can never push a buy while '## Restocking' says hold off (the LLM-297 posture; " +
			"findItemVendors' affordability drop and the working-capital floor are different filters and can disagree).",
		build: ownerConservingWithNailSupplier,
	},
	{
		name: "passerby_at_worn_stall",
		summary: "A non-owner (John) stands at someone else's worn business. The golden pins the co-present " +
			"atmosphere line ('The Blacksmith here looks worn…') and the ABSENCE of the owner '## Your business' cue — a " +
			"passerby can remark on the wear but isn't handed the repair (LLM-118).",
		build: passerbyAtWornStall,
	},
	{
		name: "passerby_at_degraded_stall",
		summary: "LLM-310/446: a non-owner (John) stands at someone else's DEGRADED business at the live default " +
			"StallDegradedProducePct. The golden pins the slowed-not-shut third-person condition line ('its keeper can " +
			"sell what's on hand and still works, though the disrepair makes everything slow') — the faithful mirror of " +
			"the owner cue, NOT the old worn-only texture and NOT a false 'can sell nothing'. No '## Your business' owner " +
			"cue, no buy imperative. Foil of passerby_at_worn_stall (worn but not degraded → the plain 'looks worn and " +
			"run-down' texture).",
		build: passerbyAtDegradedStall,
	},
	{
		name: "hired_worker_at_employer_worn_business",
		summary: "LLM-271: Lewis Walker, hired to labor for Prudence Ward (a Working LaborOffer, WorkerID == subject), " +
			"stands INSIDE her worn PW Apothecary with enough nails to mend it. The golden pins the hired-framed " +
			"'## The business you're working at … needs mending' cue (NOT the owner '## Your business') plus the hired repair " +
			"warrant line — the wake that pierces the laboring shelve-gate so a hired hand actually mends it. The live " +
			"2026-07-04 case that motivated the feature; the repair tool rides the same StallRepair signal.",
		build: hiredWorkerAtEmployerWornBusiness,
	},
	{
		name: "owner_at_worn_tavern",
		summary: "John Ellis stands at his own worn Tavern — an object tagged {business, lodging, tavern} with NO " +
			"market_stall tag, pinning that LLM-247 widened the wear gate to any owned business, not just stalls. The " +
			"object has no DisplayName, so the '## Your business' cue names it from the co-located structure ('Your Tavern " +
			"is showing hard use…') and steers to buy nails from the smith (2 held < 5). The repair tool rides the same " +
			"StallRepair signal.",
		build: ownerAtWornTavern,
	},
	{
		name: "owner_inside_worn_business",
		summary: "LLM-266 regression fixture: John Ellis stands INSIDE his own worn Tavern (InsideStructureID == the " +
			"business id) and AWAY from the outdoor loiter pin — the live keeper-at-post posture the old pin-only " +
			"co-location gate silently excluded, so the '## Your business' cue had never once rendered for a real NPC. With " +
			"sim.AtBusiness treating 'inside your business structure' as co-located, the cue (and the repair tool that rides " +
			"the same StallRepair signal) renders. The non-vacuous anchor for TestGoldensRepairCueWheneverColocatedOwnerRepairable.",
		build: ownerInsideWornBusiness,
	},
	{
		name: "passerby_inside_worn_business",
		summary: "LLM-266 non-owner arm: a non-owner (Ezekiel) stands INSIDE someone else's worn business (John's Tavern) " +
			"and away from the outdoor loiter pin. The golden pins the co-present atmosphere line ('The Tavern here looks " +
			"worn…') now firing via the inside-structure branch of sim.AtBusiness, and the ABSENCE of the owner-only " +
			"'## Your business' cue (Ezekiel isn't the owner).",
		build: passerbyInsideWornBusiness,
	},
	{
		name: "farm_owner_owes_upkeep",
		summary: "A farm owner (Elizabeth Ellis) with 95 coins (floor 30, band 20 → owes 3 upkeep shovels) and none in " +
			"hand. The golden pins the '## Farm upkeep' cue: the worn-tools problem AND the buy-N-shovels-from-the-blacksmith " +
			"steer in one line (the farm wealth tax, LLM-215). Stock-based — derived from coins, not a per-object meter — and " +
			"not co-location-gated, so it rides any tick.",
		build: farmOwnerOwesUpkeep,
	},
	{
		name: "farm_owner_owes_upkeep_with_shovel_supplier",
		summary: "LLM-274: a farm owner (Elizabeth Ellis) owes 3 upkeep shovels and holds none, while a SEPARATE " +
			"shovel-producing smith (Ezekiel at the Blacksmith) exists. The golden pins the destination-bearing " +
			"'## Farm upkeep' steer: findItemVendors resolves the smith, so the cue names 'buy from Blacksmith " +
			"(destination: blacksmith)' with move_to + pay_with_item, replacing the dead-end 'from the blacksmith'. " +
			"Its foil is farm_owner_owes_upkeep, where no supplier exists and the generic sentence is correctly kept.",
		build: farmOwnerOwesUpkeepWithShovelSupplier,
	},
	{
		name: "farm_owner_owes_upkeep_seller_present",
		summary: "LLM-277 farm-upkeep co-present arm: the same owing farm owner (Elizabeth, 3 shovels short) now shares the " +
			"smith's huddle with Ezekiel (12 shovels). The golden pins the '## Farm upkeep' cue flipping from the walk-to " +
			"list to the concrete co-present pay_with_item buy-here imperative naming Ezekiel — the same fast-path the nail " +
			"repair-buy uses, closing the same at-the-seller dead-spot where the weak model narrated and walked off. Foil of " +
			"farm_owner_owes_upkeep_with_shovel_supplier (smith far off → the walk-to destination is named instead).",
		build: farmOwnerOwesUpkeepSellerPresent,
	},
	{
		name: "farm_owner_owes_upkeep_seller_low_stock",
		summary: "LLM-299 stock-cap arm (shovel twin of owner_short_nails_seller_low_stock): Elizabeth owes 3 upkeep shovels " +
			"and shares the smith's huddle with Ezekiel, but he holds only 1 shovel against the 3 she needs. Affordable and " +
			"no prior offers, so the buy still stands — but the '## Farm upkeep' cue caps the ask at his stock ('can spare " +
			"only 1 … a qty up to 1') instead of goading 'up to 3' for stock he can't deliver. Foil of " +
			"farm_owner_owes_upkeep_seller_present (smith well-stocked with 12 → the full shortfall is asked).",
		build: farmOwnerOwesUpkeepSellerLowStock,
	},
	{
		name: "farm_owner_standoff_declined_shovels",
		summary: "LLM-299 standoff arm (shovel twin of owner_standoff_declined_nails): Elizabeth owes 3 upkeep shovels and " +
			"shares the smith's huddle with Ezekiel (well-stocked, 12 shovels), but has already had two shovel offers to him " +
			"declined in this huddle. She is a plain farmer (no restock policy → merchantConserve never fires), so the " +
			"softening is driven purely by the dead-ended negotiation — the '## Farm upkeep' cue drops 'Buy it now' for a " +
			"hold-off rather than goading a third offer into the same no. Foil of farm_owner_owes_upkeep_seller_present " +
			"(same co-present setup, no prior offers → the buy imperative still stands).",
		build: farmOwnerStandoffDeclinedShovels,
	},
	{
		name: "farm_owner_conserving_owes_upkeep",
		summary: "LLM-299 conserve-coupling arm + the non-vacuous anchor for TestGoldensFarmUpkeepGoadNeverBesideHoldOff " +
			"(the shovel twin of keeper_conserving_owes_nail_repair): Marta Vale is a shopkeeper whose working capital has " +
			"collapsed (51 coins, below the 60-coin floor, a full shelf of unsold cloth) who ALSO owns a farm and owes 1 " +
			"upkeep shovel. She has stepped off her farm to the Blacksmith, sharing Ezekiel Crane's huddle — he produces both " +
			"shovels (12 held, the upkeep supply) and coal (15 held, her low restock input). Two standing sections fire: " +
			"'## Restocking' flips to the coin-poor 'Hold off buying more for now' steer, and '## Farm upkeep' sits her in " +
			"front of the shovel seller. merchantConserve now softens the shovel goad to a hold-off, so the two cues agree " +
			"instead of the '## Farm upkeep' 'Buy it now' contradicting the restock hold-off.",
		build: farmOwnerConservingOwesUpkeep,
	},
	{
		name: "farm_owner_off_post_owes_upkeep_no_supplier",
		summary: "LLM-277 review-caught edge (code_review c11007e7): a farm owner off her post owes 3 upkeep shovels but NO " +
			"shovel supplier is reachable (findItemVendors empty, none co-present). The '## Farm upkeep' cue keeps its generic " +
			"'buy … from the blacksmith' fallback (LLM-216), but the to-work steer STILL fires ('away from your post — make " +
			"your way to the Ellis Farm now') — a dead-end upkeep cue must not suppress the return-to-post nag and strand her. " +
			"Pins that hasFarmUpkeepErrand is gated on an actionable buy path, not the cue's mere presence.",
		build: farmOwnerOffPostOwesUpkeepNoSupplier,
	},
	{
		name: "keeper_at_post_onshift",
		summary: "A keeper (shopkeeper) stands at his own store during business hours. The golden pins the " +
			"'How you trade:' trade-conduct block — the positive case for the operating-hours gate (LLM-123). On shift " +
			"and at-post, the keeper is open for trade, so the cue renders.",
		build: keeperAtPostOnShift,
	},
	{
		name: "keeper_at_closed_post_offshift_night",
		summary: "The same keeper stands at his own CLOSED store late at night, off shift (the LLM-123 bug shape: " +
			"Ezekiel told to 'tend to your trade' at midnight). The golden pins that the 'How you trade:' block is ABSENT " +
			"after hours — the off-shift work-pressure that fought his needs-pull and drove the post<->Tavern oscillation " +
			"is gone — while the off-shift wind-down steer (head home) renders instead. A regression to the operating-hours " +
			"gate would make the trade block reappear in the diff.",
		build: keeperAtClosedPostOffshiftNight,
	},
	{
		name: "keeper_staying_open_offshift",
		summary: "The same keeper, off shift at night, but holding a live stay_open commitment (committed to keep the " +
			"store open past close). The golden pins that the 'How you trade:' block renders despite being off-shift — the " +
			"operating-hours gate (LLM-123) opens on a stay_open commitment too, so a keeper working late by choice still " +
			"gets the trade-conduct framing, and the routine wind-down is suppressed.",
		build: keeperStayingOpenOffshift,
	},
	{
		name: "reseller_at_post_brisk_trade",
		summary: "LLM-413 concession-gate negative arm: a reseller (Josiah Thorne, buy-sourced cheese) at his post with two " +
			"sales inside the weekly window — trade is moving, so keeperTradeSlow is false. The golden pins the rebalanced " +
			"'How you trade:' block: the margin floor ('never sell a thing for less than it cost you') stands, and the " +
			"concession line is ABSENT — the licence to discount is withheld while trade moves, instead of standing on " +
			"every turn (the one-way ratchet that helped bleed the distributor dry).",
		build: resellerAtPostBriskTrade,
	},
	{
		name: "reseller_at_post_slow_trade",
		summary: "LLM-413 concession-gate positive arm: the same reseller, but every sale on his book is older than the " +
			"weekly window — a dead week, keeperTradeSlow true. The golden pins the concession line rendering as a felt " +
			"fact ('Trade has been thin this week') with the margin floor still standing beside it and restated inside it " +
			"('but never below what a thing cost you') — the two-sided balance the old unconditional concede-on-price line " +
			"lacked. Also pins the window cutoff: stale observations don't count as moving trade.",
		build: resellerAtPostSlowTrade,
	},
	{
		name: "producer_at_post_sub_batch_sales",
		summary: "LLM-413 batch-quantum arm: a producer (Ezekiel Crane, recipe-backed nails, batch 10) sold 3 nails this " +
			"week — a count that would read as moving trade for a resold ware reads SLOW against the producer's own batch, " +
			"the same quantum the '## Your trade' scene narrates from. The golden pins the two cues agreeing ('only a few " +
			"sold this past week' beside 'Trade has been thin this week') and the concession line rendering with the floor " +
			"intact.",
		build: producerAtPostSubBatchSales,
	},
	{
		name: "lodger_renewal_due_in_conversation",
		summary: "Renewal-due lodger (Ezekiel Crane, 0 coins, room at the Tavern nearly up) mid-conversation with an " +
			"awake huddle peer — the live incident where the renewal walk-pull dragged him out of a PC exchange. Gate 1 " +
			"(LLM-127): the golden pins that NO '## Your lodging' section renders, so rent math never interrupts a live " +
			"social beat.",
		build: lodgerRenewalDueInConversation,
	},
	{
		name: "lodger_renewal_due_onshift_away",
		summary: "The same renewal-due lodger, on-shift and away from his inn, not in conversation. Gate 3 (LLM-127): the " +
			"golden pins the deferred headline ('see the keeper to renew when you are next back at the inn') — no walk-pull " +
			"off his post — plus the rate hint and the earn cue (he's broke). The abed-keeper note is absent (deferral makes " +
			"it redundant).",
		build: lodgerRenewalDueOnShiftAway,
	},
	{
		name: "lodger_renewal_due_offshift",
		summary: "The same renewal-due lodger, off-shift and away from the inn, not in conversation — the case where the " +
			"renewal IS actionable now. The golden pins the active walk-pull ('if you wish to stay on, see the keeper to " +
			"renew') plus the rate hint and earn cue: the positive baseline the two suppression gates are measured against.",
		build: lodgerRenewalDueOffShift,
	},
	{
		name: "lodger_renewal_due_desk_remembered_shut",
		summary: "The same renewal-due lodger, off-shift and away from the inn (so the walk-pull is actionable), but he went " +
			"to the Tavern within the decay window and found the keeper's desk shut (LLM-126). The golden pins the experiential " +
			"wait-steer ('you stopped by not long ago and found the keeper's desk shut — best wait until they are tending it') " +
			"in place of the retired omniscient 'the keeper is abed just now' read: the lodger only knows the desk was shut " +
			"because it was actually there, and the memory decays on the 4h closed-business TTL.",
		build: lodgerRenewalDueDeskRememberedShut,
	},
	{
		name: "buyer_remembers_vendor_shut",
		summary: "A hungry forager (Ezekiel, holding coins he could spend) stands near a cheese seller at the General Store, " +
			"but he went there within the decay window and found it shut — no keeper tending it (now including an abed keeper, " +
			"LLM-126). The golden pins the LLM-222 seller-availability drop: the '## What you can eat or drink' buy cue DROPS the " +
			"remembered-shut vendor entirely rather than annotating it 'found it shut up', so with no other satisfier nearby the " +
			"whole section is omitted. Mirrors LLM-216's restock drop — the retired annotate-only posture left the weak model " +
			"touring the dead end (his live asleep-Inn walk). He can afford the cheese, so the drop is driven by the shut memory, " +
			"not affordability.",
		build: buyerRemembersVendorShut,
	},
	{
		name: "buyer_drops_shut_keeps_open_vendor",
		summary: "A hungry forager (Ezekiel, 6 coins) can buy cheese at two shops: the General Store, which he remembers finding " +
			"shut within the decay window, and the open Tavern he has no shut memory of. The golden pins that the LLM-222 " +
			"seller-availability drop is surgical — the shut General Store is dropped from the '## What you can eat or drink' buy " +
			"cue while the open Tavern is kept — the eat/drink analogue of keeper_restock_drops_shut_keeps_open_supplier. Also the " +
			"non-vacuous fixture for TestGoldensSatiationBuyCueNeverTargetsRememberedShutVendor.",
		build: buyerDropsShutKeepsOpenVendor,
	},
	{
		name: "broke_buyer_with_goods_barters_for_food",
		summary: "A hungry forager (Ezekiel) with 0 coins but a pelt in his pack stands near an open cheese seller (Mabel, awake, " +
			"not shut). The golden pins the LLM-222 means-to-pay 'barter' state: because barter works (pay_with_item / offer_trade " +
			"accept goods), a 0-coin buyer holding tradeable goods is NOT a dead end — the buy cue is kept but steered to a goods " +
			"offer ('which your coins won't cover — offer goods you carry in trade instead (pay_items)') rather than a coin price " +
			"he can't meet. The pelt is non-food, so it drives the barter path without adding an own-stock eat cue.",
		build: brokeBuyerWithGoodsBartersForFood,
	},
	{
		name: "broke_buyer_no_goods_no_buy_cue",
		summary: "The same hungry forager (Ezekiel), 0 coins, but now with nothing at all in his pack — no coins and nothing to " +
			"trade, the one genuine payment dead-end. The golden pins the LLM-222 means-to-pay suppression: the buy cue is dropped " +
			"entirely (the co-present open cheese seller is unpayable for him), and with no free source or own stock nearby the " +
			"whole '## What you can eat or drink' section is omitted — the free-food cues, not a futile buy imperative, are what " +
			"cover this actor. Non-vacuous fixture for TestGoldensNoBuyCueWithoutMeansToPay.",
		build: brokeBuyerNoGoodsNoBuyCue,
	},
	{
		name: "broke_buyer_no_goods_no_peer_buy",
		summary: "The LLM-242 co-present peer arm of the same means-to-pay dead-end: the broke forager (Ezekiel, 0 coins, empty " +
			"pack) stands in a huddle with a co-present peer (Lewis) carrying stew he'd otherwise be cued to buy with pay_with_item. " +
			"With no coins and nothing to trade there is no means of payment, so the peer buy offer is suppressed (the sibling of the " +
			"LLM-222 vendor-cue drop); with no free source or own stock nearby the whole '## What you can eat or drink' section is " +
			"omitted. Contrast peers_holding_same_food, where the subject DOES hold goods and so keeps a means to pay. Non-vacuous " +
			"fixture for the peer half of TestGoldensNoBuyCueWithoutMeansToPay.",
		build: brokeBuyerNoGoodsNoPeerBuy,
	},
	{
		name: "producer_hungry_mild_at_post",
		summary: "A farmer (Moses James) stands at his own farm on shift, only MILDLY hungry (felt, below the red " +
			"threshold), carrying nothing edible but the carrots he grows to sell (the live grazing case, LLM-134). The " +
			"golden pins that the '## What you can eat or drink' own-stock 'consume to eat' cue is ABSENT — his own trade " +
			"stock is demoted out of the personal eat menu below desperation, so he isn't nudged to graze the merchandise. " +
			"Pairs with producer_starving_at_post (same farmer, red hunger -> the cue returns).",
		build: producerHungryMildAtPost,
	},
	{
		name: "producer_starving_at_post",
		summary: "The same farmer (Moses) at the same farm, now at the red/distress hunger tier with the same carrots and " +
			"no other food (LLM-134). The golden pins that the own-stock 'consume to eat' cue DOES surface his carrots — at " +
			"desperation the trade stock returns as the last resort the own-stock line was built to be (the ZBBS-123 don't-" +
			"starve-next-to-your-food safety net). Pairs with producer_hungry_mild_at_post.",
		build: producerStarvingAtPost,
	},
	{
		name: "wholesaler_starving_own_produce_at_post",
		summary: "A WHOLESALER farmer (Moses James, James Farm tagged wholesaler) at the red/distress hunger tier, carrying " +
			"nothing but the carrots he grows to sell and no other food (LLM-267). Unlike producer_starving_at_post, the own-" +
			"stock 'consume to eat' cue is ABSENT even at desperation — a wholesaler's produce is stock to sell, never its " +
			"larder, with NO red-tier escape hatch (the Consume guard rejects it too). Food must come from buying, foraging, " +
			"or barter. Pairs with wholesaler_bought_food_at_post (the same wholesaler IS offered a bought loaf).",
		build: wholesalerStarvingOwnProduceAtPost,
	},
	{
		name: "wholesaler_bought_food_at_post",
		summary: "The same wholesaler (Moses), mildly hungry, carrying a bought loaf of bread (NOT one of his produce rows) " +
			"alongside his own carrots (LLM-267 positive control). The own-stock 'consume to eat' cue surfaces the BREAD — real " +
			"provisions he may eat — while his carrots stay out. Proves the block is own-produce-scoped, not a blanket ban on a " +
			"wholesaler eating.",
		build: wholesalerCarryingBoughtFoodAtPost,
	},
	{
		name: "broke_worker_no_employer_seeks_work",
		summary: "A broke worker (Lewis Walker, a salem-vendor) idle at home with no employer present — the live LLM-160 " +
			"case. The golden pins the make-it-move fix: the businesses directory renders as a STANDING cue (the town's " +
			"businesses by their resolvable structure names) even with no seek-work warrant, so move_to has a real target " +
			"instead of an invented place ('the market', 'the Well') that bounces; and the triage coda is the decisive " +
			"'call move_to now' go-line, not the default act-now/await-reply coda the agree-loop fed on. A regression to the " +
			"warrant gate would drop the directory line, and a regression to the coda swap would bring back 'Choose one action'.",
		build: brokeWorkerNoEmployerSeeksWork,
	},
	{
		name: "broke_worker_seeks_work_skips_shut_business",
		summary: "The LLM-155 companion to broke_worker_no_employer_seeks_work: the same broke idle worker (Lewis Walker), but he " +
			"remembers finding the Inn shut an hour ago (an earned ObservedClosed memory within the 4h TTL). The golden pins that the " +
			"seek-work directory DROPS the remembered-shut Inn entirely — not annotates it — and lists only the open General Store, " +
			"carrying its qualitative distance + direction ('a short walk east'). A regression that stopped consulting the shut " +
			"memory would re-list the Inn; one that dropped distance would lose the walk descriptor.",
		build: brokeWorkerSeeksWorkSkipsShutBusiness,
	},
	{
		name: "worker_with_coin_no_employer_seeks_work",
		summary: "The LLM-168 live case: a WORKLESS worker (Silence Walker — worker attribute, no work_structure_id) idle at " +
			"home holding a few coins, no employer present. Under the old broke (Coins==0) gate she got no directory and no " +
			"seek-work warrant, so the brand-new Walker family idled all shift inventing move_to destinations. LLM-168 re-" +
			"anchored eligibility on workless, so the same standing businesses directory + decisive 'call move_to now' coda " +
			"fire whether or not she holds coin. The golden pins that a coin-holding workless worker gets the identical leave-" +
			"for-a-business directive as the broke one; a regression to the Coins==0 gate would drop the directory + go-coda here.",
		build: workerWithCoinNoEmployerSeeksWork,
	},
	{
		name: "seeking_worker_at_home_gets_no_bake_cue",
		summary: "LLM-459: the workless, below-ceiling Silence Walker settled at home during the day with flour, with the " +
			"household's bake already going — every bake precondition met AND the seek-work directory engaged. Pre-fix both " +
			"rendered, and the imperative coda ('call move_to now') beat the mid-scene bake invitation: live 2026-07-18 she " +
			"did laps (home → Tavern → Blueberry Bush → home) while her three over-the-ceiling housemates baked. The golden " +
			"pins the seek-work half rendering ALONE — LLM-454 built bake for villagers who stay home BECAUSE they have no " +
			"reason to seek work, so an engaged job-seeker is out of that population. A regression that dropped the " +
			"suppression re-adds the bake line here and flips TestNoPromptOffersBakeAndSeekWorkTogether.",
		build: seekingWorkerAtHomeGetsNoBakeCue,
	},
	{
		name: "comfortable_homebody_bakes",
		summary: "LLM-459 (positive half), and the matrix's first bake scenario — LLM-454 shipped the daytime bake with no " +
			"golden at all. The same at-home-with-flour fixture as seeking_worker_at_home_gets_no_bake_cue, differing ONLY in " +
			"the purse: 40 coins is at/above the seek-work ceiling, so this worker is comfortable, draws no businesses " +
			"directory, and IS the homebody population bake serves — the golden pins the join-in invitation rendering with no " +
			"seek-work coda beside it. Flipping only coins across the ceiling toggles which cue appears, proving the split is " +
			"the ceiling itself and not some other fixture difference. Also the vacuity floor for " +
			"TestNoPromptOffersBakeAndSeekWorkTogether: without a scenario that renders the bake line, that invariant would " +
			"pass having checked nothing.",
		build: comfortableHomebodyBakes,
	},
	{
		name: "seeking_worker_home_scene_hides_housemate_bake",
		summary: "LLM-562: the seek-work-engaged Walker of seeking_worker_at_home_gets_no_bake_cue with a housemate " +
			"co-present MID the bake — the shape the LLM-459 fixture never carried (its bake was active only as a flag, " +
			"nobody in the room doing it). LLM-459's suppression was affordance-deep, not scene-deep: the cue and tool " +
			"dropped, but the housemate still rendered '(at the hearth, baking just now)' beside the subject's own flour, " +
			"and the model read the scene — live 2026-07-29 Patience Walker narrated joining the bread on nine straight " +
			"home-arrival ticks under a coda offering only move_to, 179 LLM calls in a day. The golden pins the housemate " +
			"named and greetable with NO bake annotation while the seek-work directory + go-coda render as before. A " +
			"regression that re-scopes the strip to the affordance alone flips TestNoSeekWorkPromptDepictsHousemateBake.",
		build: seekingWorkerHomeSceneHidesHousemateBake,
	},
	{
		name: "comfortable_homebody_sees_housemate_bake",
		summary: "LLM-562 (positive half): identical to seeking_worker_home_scene_hides_housemate_bake except the purse — " +
			"40 coins is at/above the seek-work ceiling, so no directory engages and the housemate's bake annotation (and " +
			"the join-in cue) render normally. Flipping only coins across the ceiling toggles the annotation, proving the " +
			"suppression keys on the seek-work directive itself. Also the vacuity floor for " +
			"TestNoSeekWorkPromptDepictsHousemateBake: without a scenario rendering the annotation, that invariant would " +
			"pass having checked nothing.",
		build: comfortableHomebodySeesHousemateBake,
	},
	{
		name: "homebody_mid_bake",
		summary: "LLM-464: the comfortable_homebody_bakes homebody one step later — at the hearth with her bake window " +
			"live. Pins the mid-activity triage coda, whose completion horizon reads 'by dusk' for a bake instead of the " +
			"'shortly' the LLM-69 coda hardcoded for every source-activity kind. Shortly is honest for refresh (3s), " +
			"harvest (5s), stoke (30s) and repair (15m); a bake runs to dusk, so the engine was telling a midday baker her " +
			"five-hour batch was nearly done, and she passed it to the household verbatim ('just a few more minutes by the " +
			"hearth' — Anne Walker, live 2026-07-18), which kept the Walker house asking after bread that was hours out. " +
			"Also pins that the bake CUE drops once she's baking (she's doing it already). A regression that re-shares the " +
			"constant flips TestBakeCodaNamesDuskNotShortly.",
		build: homebodyMidBake,
	},
	{
		name: "hungry_homebody_joins_bake",
		summary: "LLM-465: the comfortable_homebody_bakes homebody, red on hunger, at a home where a batch is already " +
			"going. Pins that a pressing need no longer swallows the JOIN — the red-need guard is start-only now, because " +
			"lending a hand costs no flour, mints no batch, and leaves the need actionable (bakingMayMove keeps move_to " +
			"for a red hunger/thirst; the reactor ticks him through the shelve for it). Pre-fix the guard sat above the " +
			"join resolution and refused him the bake outright, leaving him unshelved in his own kitchen: live 2026-07-18 " +
			"Lewis Walker burned 24 turns in 70 minutes asking after the loaves, arming bakeReplyDue for both bakers with " +
			"every question. The golden carries the join invitation and the hunger imperative together — both true, and he " +
			"is free to act on either. A regression that hoists the guard back drops the bake line here.",
		build: hungryHomebodyJoinsBake,
	},
	{
		name: "scheduled_keeper_preshift_gets_no_bake_cue",
		summary: "LLM-650: the comfortable_homebody_bakes fixture with a trade — Lewis Walker, the wright (LLM-648), " +
			"home at 09:00 with the household bake going and his 10:00–18:00 shift an hour off. Pre-fix the join " +
			"invitation rendered (inDaytimeHomeWindow only asks whether the shift is on NOW), he took it, and the hearth " +
			"pin held him through the whole shift — live 2026-09-01 he joined the 08:27 bake and never reached the " +
			"workshop; the to-work steer rendered against the pin ten times and lost. The golden pins NO bake line " +
			"for a scheduled subject due at its post before dusk. A regression that drops shiftStartsBeforeDusk re-adds " +
			"the invitation here and flips TestNoPreShiftKeeperIsOfferedBake.",
		build: scheduledKeeperPreshiftGetsNoBakeCue,
	},
	{
		name: "night_keeper_offshift_joins_bake",
		summary: "LLM-650 (positive half): the same wright with his shift moved to 20:00–23:00, after the 19:00 dusk. " +
			"The day is his, so the join invitation renders as it does for the unscheduled Walkers — proving the gate " +
			"keys on a shift that would BEGIN while the loaves are in, not on having a schedule at all. Without a " +
			"scheduled subject that still bakes, a gate that refused every scheduled actor would pass unnoticed.",
		build: nightKeeperOffshiftJoinsBake,
	},
	{
		name: "comfortable_worker_no_seek_work",
		summary: "The LLM-194 case: the same workless Silence Walker as worker_with_coin_no_employer_seeks_work, but holding " +
			"coin AT/ABOVE the seek-work ceiling (40 >= the default 25). A coin-rich worker is 'comfortable' — it doesn't need " +
			"odd jobs — so the golden pins that it gets NEITHER the businesses directory NOR the 'call move_to now' go-coda: " +
			"it's left to idle and drain its purse via ordinary consumption instead of pestering keepers for work. The negative " +
			"counterpart of the 15-coin scenario (which still seeks); a regression that dropped the coin ceiling would re-add " +
			"the seek-work cue here and flip TestSeekWorkDirectiveOnlyForWorklessWorker.",
		build: comfortableWorkerNoSeekWork,
	},
	{
		name: "comfortable_worker_at_ease",
		summary: "LLM-352: the comfortable (coin-rich, workless) Walker from comfortable_worker_no_seek_work, standing out " +
			"in the village at 09:00 and carrying the at-ease warrant the seek-work backstop now stamps for it in place of the " +
			"freeze the coin ceiling (LLM-194) otherwise left it in. The golden pins BOTH halves: the 'the day is your own' " +
			"leisure line renders (pass time with neighbors, look in at the tavern, or see to a want), AND there is still NO " +
			"seek-work directory / solicit affordance (194's suppression holds). A regression that dropped the at-ease arm loses " +
			"the line and re-strands the worker.",
		build: comfortableWorkerAtEase,
	},
	{
		name: "worker_seeks_work_after_employer_declines",
		summary: "The LLM-181 live case (Lewis Walker at the General Store, hud-8db08741…), reduced: a workless worker shares a " +
			"huddle with a co-present stranger employer (Josiah Thorne) who has ALREADY declined his labor offer. Pre-fix, the " +
			"co-present employer kept hasSolicitableAudience true, which suppressed SeekWorkPlaces and the seek-work off-ramp — so " +
			"the worker re-soliciting the same refusal was never told to leave. LLM-181 drops a declined employer from the " +
			"solicitable audience, so the standing businesses directory + decisive 'call move_to now' go-coda arm DESPITE the " +
			"employer being present, and no solicit_work affordance is offered for the refuser. A regression that forgot the " +
			"decline would re-suppress the directory and bring back the solicit cue against Josiah.",
		build: workerSeeksWorkAfterEmployerDeclines,
	},
	{
		name: "worker_solicits_goods_rich_coin_poor_employer",
		summary: "The LLM-243 live case (Silence Walker / Prudence Ward at the PW Apothecary, hud-36317f65…), reduced: a workless " +
			"worker shares a huddle with a co-present stranger employer who holds 0 coins but goods on hand (berries, tea). Such " +
			"an employer can still hire IN KIND (LLM-225), so SolicitWork no longer auto-declines a bad coin ask against it — the " +
			"barter branch mints no offer and records no decline. With no Declined ledger entry the employer stays a solicitable " +
			"prospect: the golden pins the solicit_work affordance PRESENT for Prudence and the ABSENCE of the SeekWorkPlaces " +
			"businesses directory + the 'No one here can hire you' seek-work dead-end. A regression that foreclosed a coin-poor " +
			"employer (dropping it from the solicitable audience for its empty purse) would re-suppress the affordance and bring " +
			"back the dead-end. Matrix-wide guard: TestGoldensCoinPoorEmployerStaysSolicitable.",
		build: workerSolicitsGoodsRichCoinPoorEmployer,
	},
	{
		name: "worker_solicits_unacquainted_employer_unnamed_cue",
		summary: "LLM-564's fallback branch: the same workless worker / solicitable stranger pair as " +
			"worker_solicits_goods_rich_coin_poor_employer, but the SUBJECT does not know the employer by name (empty " +
			"Acquaintances). The solicit gate carries no acquaintance clause, so the affordance + tool stay offered — but the cue " +
			"must NOT name her: solicit_work resolves its employer by exact display name, and an unacquainted peer renders as a " +
			"descriptor, so a named cue would hand the model a target the tool must refuse (buildSolicitableEmployers). The " +
			"golden pins the UNNAMED wording ('You take work for pay. If someone here…') with the say/speak-first warning intact, " +
			"and — with TestGoldensSolicitCueNeverNamesSomeone — that the named branch's 'someone might have work' descriptor " +
			"leak can never render.",
		build: workerSolicitsUnacquaintedEmployerUnnamedCue,
	},
	{
		name: "worker_seeks_work_skips_no_hiring_business",
		summary: "The LLM-210 companion to broke_worker_seeks_work_skips_shut_business: the same workless idle worker (Lewis " +
			"Walker), but he last found the General Store's keeper on a break — an earned ObservedNoHiring memory within its 2h " +
			"TTL — where the keeper was PRESENT (so the store is NOT remembered shut) yet could not take him on. The golden pins " +
			"that the seek-work directory DROPS the no-hiring General Store entirely and lists only the open Blacksmith, carrying " +
			"its distance + direction, so he is steered to a business with an available keeper. A regression that stopped " +
			"consulting the no-hiring memory would re-list the General Store and re-strand him on the resting-keeper loop that " +
			"ObservedClosed (sleeping only) and ObservedDeclinedWork (a refusal) both miss.",
		build: workerSeeksWorkSkipsNoHiringBusiness,
	},
	{
		name: "worker_seeks_work_ranks_visited_business_last",
		summary: "The LLM-563 companion to the three seek-work DROP scenarios: the same workless idle worker (Patience " +
			"Walker), but she CALLED at the nearest business (the General Store) half an hour ago and nothing came of it — an " +
			"earned ObservedSeekWorkVisited memory within its 2h TTL, stamped on ANY qualifying arrival rather than a rejection " +
			"event, which is what the shut/declined/no-hiring memories all require and the live loop never produced (the " +
			"General Store four times in twelve minutes, 21 trips in three hours with nothing learned). The golden pins the " +
			"deprioritise treatment: the store stays a REAL destination (never dropped — dropping would empty the directory " +
			"after one tour while the seek-work impulse kept nagging with no target) but lists LAST, behind the farther untried " +
			"Blacksmith, carrying the terse 'you called there not long ago' aside so the ordering reads as a reason. A " +
			"regression that stopped consulting the visited memory would restore nearest-first and put the just-left store " +
			"back on top — the exact live loop.",
		build: workerSeeksWorkRanksVisitedBusinessLast,
	},
	{
		name: "red_tired_worker_no_seek_work",
		summary: "The LLM-210 case: a WORKLESS worker (Lewis Walker) idle at home holding a few coins (15, below the seek-work " +
			"ceiling → not comfortable) but at RED tiredness (20 >= the default red-line 16). A pressing need outranks job-" +
			"hunting, so the golden pins that he gets NEITHER the businesses directory NOR the 'call move_to now' go-coda — both " +
			"seek-work gates suppress and the already-present weariness cue is left to win, so he rests on his own rather than " +
			"pacing to a shop while exhausted (the live home<->store loop). The rested counterpart is " +
			"worker_with_coin_no_employer_seeks_work (same workless coin-holder, not red → still seeks). A regression that dropped " +
			"the hasRedNeed gate would re-add the directory + go-coda here and flip TestSeekWorkSuppressedByRedNeed.",
		build: redTiredWorkerNoSeekWork,
	},
	{
		name: "customer_at_shut_business_loitering",
		summary: "A laborer (Goodman Silence) stands OUTDOORS at the Tavern's loiter slot, but its only keeper (John Ellis) " +
			"is asleep inside — the live LLM-154 case (Silence stuck at the closed Tavern while seeking work). The golden pins " +
			"the at-location dead-end clause 'The Tavern is shut — no one is tending it.' next to the 'outdoors by the Tavern' " +
			"location line: a live, situated read (the keeper is abed, so the place reads shut) distinct from the ObservedClosed " +
			"memory, so a weak model isn't left to infer 'closed' from 'the keeper is asleep'.",
		build: customerAtShutBusinessLoitering,
	},
	{
		name: "customer_at_shut_business_inside",
		summary: "The same laborer, now standing INSIDE the Tavern's interior with the keeper asleep there (LLM-154). The " +
			"golden pins that the shut clause fires on the interior placement too — keyed on the current location whether the " +
			"actor entered or is loitering at the slot — and that the abed keeper is named as a co-present sleeper (visible but " +
			"not addressable).",
		build: customerAtShutBusinessInside,
	},
	{
		name: "customer_at_open_business",
		summary: "The positive control for LLM-154: the same laborer outdoors at the Tavern's loiter slot, but the keeper is " +
			"awake and present inside. The golden pins that NO shut clause renders — an awake, present keeper means the business " +
			"is tended, so the live read stays silent (render the situation, not omniscient).",
		build: customerAtOpenBusiness,
	},
	{
		name: "huddle_conversation_looping",
		summary: "Two idle workers (the Walker sisters) stand together going in circles — the live LLM-169 case: " +
			"Patience and Anne re-echo 'Let's go to the well!' / 'Let's go!' without it converting to a move. The huddle " +
			"is in an armed conversational loop (ActorSnapshot.ConversationLooping — the same huddleLoopArmed signal the " +
			"loop sweep arms on, surfaced per-tick), and Anne holds a live await edge to Patience. The golden pins the " +
			"LLM-169 swap: the 'Anne Walker is waiting for your reply.' nag is SUPPRESSED (that nag is what manufactures " +
			"the echo) and the coda is the 'you've agreed — act now or done()' loop steer, NOT the default/awaiting coda " +
			"that fed the agree-loop. A regression that dropped the flag would bring back the nag and the 'Choose one action' coda.",
		build: huddleConversationLoopingScenario,
	},
	{
		name: "hungry_looper_at_foodless_home",
		summary: "The live LLM-176 case: hungry Walker sisters loop in a huddle at their foodless residence, " +
			"confabulating 'food in the kitchen' instead of walking to a real source. Patience (the subject) is in an " +
			"armed conversational loop, feels hunger, holds nothing edible, has 1 coin, and a free Raspberry Bush sits a " +
			"walk away. The golden pins BOTH LLM-176 cues: the at-location dead-end ('There's no food to be had here — " +
			"you'll need to forage or buy a meal elsewhere.') that kills the confabulation, and the need-redirect coda " +
			"(swapping the generic 'do what you've agreed' line for 'go to Raspberry Bush (destination: …) now and eat') " +
			"that names the engine's known affordance. A regression that dropped either would bring back the silent dead " +
			"end or the plan-endorsing generic coda.",
		build: hungryLooperAtFoodlessHome,
	},
	{
		name: "undirected_reask_sole_peer",
		summary: "The live LLM-232 case: John Ellis floated a plain, unaddressed trade proposal to the only other " +
			"person in his huddle (Anne Walker) and she has said nothing back. Because the ask named no addressee it opened " +
			"no WORK-370 edge, and John's own last line is ~75s old — past the 60s directed-edge window (so even a directed " +
			"edge would have lapsed) but well inside ReaskSuppressWindow. The golden pins the LLM-232 anchor: the " +
			"sole-awake-peer condition folds the peer into " +
			"AwaitingReplyFrom, so the 'You already spoke to the villager and are waiting for their reply. Do not repeat " +
			"yourself…' line renders (name acquaintance-gated to 'the villager' here) and the coda swaps to the " +
			"awaiting-reply wait-framing — the cross-tick memory an " +
			"undirected re-ask storm otherwise lacks. A regression that dropped the anchor would leave no wait line and " +
			"re-open the re-pitch loop.",
		build: undirectedReaskSolePeerScenario,
	},
	{
		name: "paced_worker_owes_answer_to_questioner",
		summary: "The live LLM-536 case: Constable Marsh asked Silence Walker — a hired worker inside a live labor " +
			"window — a direct question, and she is only now drawing the tick her 3m reply cadence held back. His edge is " +
			"107s old: past the 60s WORK-370 window, so before LLM-536 the obligation had already stopped rendering by the " +
			"time she could speak, and the turn she eventually got went to an unrelated topic while he waited. Her " +
			"ReplyPacingWindow widens her own await window to 240s, so the golden pins 'Constable Marsh is waiting for " +
			"your reply.' surviving to the moment she can answer. A regression that dropped the widening would hand a " +
			"laboring worker a turn with nothing marking the question as owed — the tick restored, the question still lost.",
		build: pacedWorkerOwesAnswerToQuestioner,
	},
	{
		name: "hungry_actor_holding_raw_meat",
		summary: "A hungry shopkeeper (Josiah Thorne) at his post carries raw Meat — a stew INGREDIENT (food-category but " +
			"eases no need raw) — alongside edible Cheese (the live LLM-166 case: he fired consume{Meat} 22 times). The golden " +
			"pins the use annotation folded into the carry readout, 'Meat (x7, used to produce stew)', while Cheese stays bare " +
			"(the satiation cue owns edibles). A regression that dropped the annotation would let the most food-like name in a " +
			"flat inventory read as a meal again.",
		build: hungryActorHoldingRawMeat,
	},
	{
		name: "seller_with_taken_quote_at_post",
		summary: "A vendor (Prudence Ward) at her post has just SOLD one lot — its quote is now " +
			"SceneQuoteStateTaken — while a second lot stays on offer (the live LLM-189 case). The golden pins that " +
			"'## Offers you've put out' lists ONLY the still-active lot (raspberries); the taken lot (blueberries) is " +
			"gone, not shown as 'they have yet to answer'. Reverting the close-on-take fix would make the sold lot " +
			"reappear in the diff — the phantom standing offer that lured the live seller into firing pay_with_item at " +
			"her own buyer. The reverse-pay dispatch gate itself is pinned by the sim-package handler tests.",
		build: sellerWithTakenQuoteAtPost,
	},
	{
		name: "seller_with_shortfall_lot_at_post",
		summary: "LLM-409: a farmer (Josiah Thorne) at his post offered John Ellis two sheaves of wheat and then spent the " +
			"wheat out from under his own lot, so the coverage reconcile flipped that lot to SceneQuoteStateShortfall while a " +
			"turnip lot he can still cover stays active. The golden pins that '## Offers you've put out' lists ONLY the coverable " +
			"turnip lot — the wheat lot is GONE, no longer advertised or pinning him with 'bide, do not re-post' — and that the " +
			"flat '## An offer you couldn't keep' beat names the wheat he can no longer give. Reverting the reconcile would " +
			"resurface the uncoverable wheat lot under the standing-offers header.",
		build: sellerWithShortfallLotAtPost,
	},
	{
		name: "buyer_kept_consume_remainder_reconciled",
		summary: "A buyer (Anne Walker) just took a consume_now quote for 5 blueberries, but her low hunger meant the " +
			"needs-clamp ate only 1 and pocketed 4 (the live LLM-188 case). The golden pins that '## Recently settled " +
			"offers' reconciles the split — 'you ate 1 on the spot and kept the other 4' — so it agrees with the carried " +
			"Blueberries (x4) readout instead of claiming all 5 were had right away. The bare 'had it right away' line " +
			"contradicted the inventory and drove both NPCs to confabulate a missing-blueberry short-count; a regression " +
			"that dropped the reconciliation would resurface that contradiction in the diff.",
		build: buyerKeptConsumeRemainderReconciled,
	},
	{
		name: "buyer_offer_declined_seller_short_stock",
		summary: "LLM-296: a buyer (Josiah Thorne) whose pay offer of 6 carrots + 1 coin for 5 nails to Ezekiel Crane " +
			"was just declined — Ezekiel holds only 1 nail (the live hud-e7fec94 case). The golden pins that '## Recently " +
			"settled offers' names the OFFERED bundle (not just the want-item, so two declines aren't byte-identical — the " +
			"repeat the thin line drove) and appends the engine-known stock shortfall ('they hold only 1 nail') as the " +
			"informed reason it closed.",
		build: buyerOfferDeclinedSellerShortStock,
	},
	{
		name: "offeree_short_of_asked_good_pending",
		summary: "LLM-303, the live 2026-07-06 General Store case: Elizabeth Reade — NOT a nail vendor, holding ZERO " +
			"nails — has a standing pay offer from Josiah Thorne of 1 sage for 5 nails to keep. Before the fix the " +
			"seller-side stock warning fired only for a vendor already carrying some of the kind, so Elizabeth saw the " +
			"bare offer and fabricated stock she never had (then accept_pay'd three offers she couldn't fill). The golden " +
			"pins that '## Offers awaiting your decision' now carries '— you hold no nails', the fact that grounds a " +
			"decline or counter instead of a confabulated accept. A regression that re-gated the warning on vendor status " +
			"would drop the clause in the diff.",
		build: offereeShortOfAskedGoodPending,
	},
	{
		name: "employer_with_worker_on_job",
		summary: "An employer (John Ellis the tavernkeeper) stands with a worker (Silence Walker) who is mid-contract " +
			"for him — a Working labor offer, ~90 minutes left (the live LLM-202 case). The golden pins the new " +
			"'## Workers currently working for you' employer-side cue ('Silence Walker is working a job for you — about " +
			"1 hour 30 minutes left; 2 coins owed when it's done') plus the shared 'already covered … don't hire someone " +
			"else for it or pay again by hand' steer. Without it the employer saw only the pending-offer decision view and " +
			"re-hired a second worker for the same job. The worker's own '## Work offers awaiting your decision' is ABSENT " +
			"here (the offer is Working, not Pending). A regression that dropped the cue resurfaces the blind re-hire in the diff.",
		build: employerWithWorkerOnJob,
	},
	{
		name: "laboring_worker_addressed_while_working",
		summary: "LLM-230, the worker side of the same job: Silence Walker is mid-contract for John Ellis and he speaks " +
			"to her. The golden pins her standing self-state anchor — 'You are working a job for John Ellis … Stay with it " +
			"until it's done' — the cue that grounds a 'can't stop just now, I'm minding the work' reply. The reactor reply-" +
			"cadence and the speak-only tool surface that make that reply happen are covered by unit tests (the render is " +
			"unchanged); a regression that muted the laboring self-state for an addressed worker would surface here.",
		build: laboringWorkerAddressedByEmployer,
	},
	{
		name: "laboring_worker_off_post",
		summary: "LLM-268 symptom 1 (the marooning): Silence Walker walked off John Ellis's Tavern mid-contract (a " +
			"need-break that left her, needs now green) while John still holds the post. LLM-230 stripped her move_to when " +
			"the need cleared and took her tick eligibility with it, so she stood marooned until the completion sweep. The " +
			"golden pins the two things that recover her: the return-to-post felt-impulse warrant line that wakes her (the " +
			"backstop's engine-authored nudge) and her self-state cue 'you have wandered off … Head back there with " +
			"move_to'. The move_to re-grant itself is asserted in handlers/labor_gating_test.go.",
		build: laboringWorkerOffPost,
	},
	{
		name: "laboring_worker_at_post_employer_present",
		summary: "LLM-268 regression guard for LLM-230: Silence is inside the Tavern with John present, green needs — the " +
			"normal committed case. Neither off-post flag holds, so the golden pins the unchanged 'Stay with it until it's " +
			"done' self-state line with NO directional cue. Widening the move_to gate for the off-post cases must not leak " +
			"the return/accompany cue (or move_to) into the at-post case; the tool-side half is the move_to-stripped " +
			"assertion in handlers/labor_gating_test.go.",
		build: laboringWorkerAtPostEmployerPresent,
	},
	{
		name: "laboring_worker_employer_away",
		summary: "LLM-268 symptom 2 (accompany): Silence is at the Tavern but John has walked off to the General Store " +
			"mid-contract (the live Hannah/Lewis case — an employer trying to take her hire along). The golden pins the " +
			"accompany cue 'they have left the Tavern and gone to the General Store … follow after them with move_to', so a " +
			"'come with me' errand can be acted on instead of being silently impossible. The tick that lets her act rides " +
			"the employer's speech reply-cadence (unchanged); the move_to re-grant is asserted in handlers/labor_gating_test.go.",
		build: laboringWorkerEmployerAway,
	},
	{
		name: "constable_at_farm_worked_by_hired_hand",
		summary: "LLM-527: the constable stops at the Ellis Farm on his rounds while Abraham Warren — a free laborer with no " +
			"workplace of his own — works it and Elizabeth is across the village. The shut cue and the co-presence line both " +
			"keyed on KEEPER presence, so his prompt read 'with no one else here to hear you speak' and 'the Ellis Farm is shut " +
			"— no one is tending it' with a man visibly at work in front of him: the most interesting thing a round can turn up, " +
			"hidden. The golden pins the repaired scene — no shut clause, and Abraham named in '## Around you' as busy on " +
			"Elizabeth's work. The scope rule that puts him in the audience is asserted world-side in " +
			"sim/business_tended_test.go.",
		build: constableAtFarmWorkedByHiredHand,
	},
	{
		name: "worker_labor_settled_beat",
		summary: "LLM-498 worker side of the mid-shift settle beat: Abraham Warren's four-hour job for Elizabeth Ellis just " +
			"settled at the completion sweep — 12 coins and 1 milk transferred in full, mid-shift, so the LLM-190 shop-closed " +
			"close-out did NOT fire — and the LaborResolved(Completed) subscriber woke him with the pre-rendered narration. The " +
			"golden pins the 'Your work for Elizabeth Ellis is done — you've been paid 1 milk and 12 coins, as agreed.' warrant " +
			"line (renderNarrationWarrantLine's WarrantKindLaborSettled path). Before LLM-498 this settle was invisible: the " +
			"live trace has Abraham, already paid, asking to 'settle up with you for the day'.",
		build: workerLaborSettledBeat,
	},
	{
		name: "employer_labor_settled_beat",
		summary: "LLM-498 employer side of the same settle, the double-pay guard: Elizabeth Ellis's perception previously " +
			"carried ONLY the worker's settle-up line, so she reasonably paid a second wage for the settled job (live " +
			"2026-07-19: pay-5 on top of the transferred 12-coins-and-milk reward — a 40% overpay, unbounded on a larger job). " +
			"The golden pins her own settle warrant line 'Abraham Warren's work for you is done — you've paid 1 milk and 12 " +
			"coins for it, as agreed.' so the tick that reads the worker's settle-up request also reads the wage as already " +
			"squared.",
		build: employerLaborSettledBeat,
	},
	{
		name: "worker_labor_settled_closeout_both_signals",
		summary: "LLM-498 x LLM-190 combination: a job that ran up to the employer's closing time settles with BOTH worker-" +
			"side signals in one tick — the keeper's spoken close-out (an observed social event, the LLM-190 Spoke riding the " +
			"speech reactor) and the worker's own labor_settled warrant (the authoritative payment state). Deliberately " +
			"redundant (code_review round 1): the golden pins both lines rendering together in a single '## Since your last " +
			"turn' block — one wake, not two — so neither signal silently swallows the other and the pairing's prompt cost " +
			"stays visible in the diff.",
		build: workerLaborSettledCloseoutBothSignals,
	},
	{
		name: "destitute_keeper_blocked_suppliers_named_with_reasons",
		summary: "LLM-216 as amended by LLM-406: a DESTITUTE general-store keeper — 0 coins AND an empty pack — whose " +
			"bought-in carrots and milk are both empty stands alone at his store on shift. His carrot supplier (James " +
			"Farm) he remembers finding SHUT; his milk supplier (Ellis Farm) is open, but its remembered price (4 coins) " +
			"is beyond a purse and a pack that hold nothing at all. Neither is a place he can act on, so LLM-216 dropped " +
			"both and rendered NO section — and that silence is what this golden used to pin. It is also what turned an " +
			"illiquid keeper into an absorbing state: an unmet obligation and not one word about it. The golden now pins " +
			"the blocked scene — both farms NAMED, each with its reason, no '(destination: …)' token anywhere (that is " +
			"the token the model echoes into move_to, and these are precisely the places it must not walk to), and a " +
			"self-resolving coda instead of a dangling want. Suppression may take the imperative; it may not take the " +
			"facts. Compare illiquid_distributor_barters_for_stock, where the same keeper HAS goods and so gets his " +
			"suppliers back as real destinations. Matrix guards: TestGoldensRestockNeverTargetsRememberedShutSupplier " +
			"and TestGoldensRoleNeedNeverRendersSilent.",
		build: destituteKeeperBlockedSuppliersNamedWithReasons,
	},
	{
		name: "illiquid_distributor_barters_for_stock",
		summary: "LLM-406, the live Josiah Thorne deadlock (2026-07-14): the village's ONLY distributor stands at his " +
			"post with 3 coins, bare shelves, and a pack holding a horseshoe, a skillet and 18 flasks of water — he had " +
			"liquidated his book to buy the nails to mend his roof. Both farms' remembered prices (4 milk, 6 carrots) sit " +
			"above his purse, so the COINS-ONLY affordability drop erased both suppliers, omitted both items, and rendered " +
			"no '## Restocking' section at all. With empty shelves he had nothing to sell, so he could never earn back " +
			"over any price, so the cue could never return — an absorbing state, and one that jammed the whole village's " +
			"retail food chain, since the wholesale tier makes him the farms' only lawful buyer. The golden pins the " +
			"means-to-pay fix: pay_with_item settles in GOODS, his pack is not empty, so both farms return as real " +
			"destinations carrying the barter steer ('your coins won't cover it — offer goods you carry in trade') " +
			"instead of a coin price he cannot meet. His own nail purchase — 2 skillets, 2 wheat and 2 coins — was " +
			"exactly such a bundle.",
		build: illiquidDistributorBartersForStock,
	},
	{
		name: "broke_maker_makings_are_not_barter",
		summary: "LLM-636, the live Hannah Boggs treadmill (2026-08-18): the innkeeper, coin-broke after the deadlock, at " +
			"her post holding only what she works with — 2 spools of thread for mending, 6 flasks of water for the porridge " +
			"pot, the one suit of homespun on her back, and a pot of eat-here porridge — and low on salt, flour and thread, " +
			"all sold at the General Store. Live, every one of those lines rendered LLM-406's 'offer goods you carry in " +
			"trade' and she did: thread for salt, salt for homespun, homespun for firewood, 103 laps with Josiah in a week " +
			"and the mending line never held a spool. The golden pins the spoken-for reservation: the carry line marks the " +
			"thread and water 'kept to work with — not for trade' and the homespun 'your own clothes — not for trade', the " +
			"purse line says she cannot pay for anything, and every restock line lands on the LLM-406 blocked scene ('nor a " +
			"good you can spare to put up in trade') instead of the barter steer. Contrast broke_maker_own_produce_is_barter " +
			"(same pack plus her own journeycake) and illiquid_distributor_barters_for_stock (the distributor's identical-" +
			"looking pack IS his wares). Non-vacuous fixture for TestGoldensNoBarterSteerWithoutSpareGoods.",
		build: brokeMakerMakingsAreNotBarter,
	},
	{
		name: "broke_maker_own_produce_is_barter",
		summary: "The same coin-broke innkeeper (LLM-636), now also carrying 3 journeycakes of her own baking. Her produce " +
			"is exactly what a broke maker should put up, so the golden pins the other half of the reservation: the " +
			"journeycake rides the carry line unannotated, the purse line says she may offer goods in trade, and each low " +
			"restock line gets the LLM-406 barter steer back — while the thread and water stay marked not for trade. The " +
			"reservation takes the makings out of the bundle, not the buyer out of the market.",
		build: brokeMakerOwnProduceIsBarter,
	},
	{
		name: "broke_maker_at_store_no_spare_goods_hold_off",
		summary: "The co-present half of LLM-636 — where the live laps actually happened. The coin-broke innkeeper stands IN " +
			"the General Store with Josiah, holding only her makings and the suit on her back, low on salt and thread he " +
			"stocks. Live, the buy-here imperative goaded 'Buy it now … goods you carry (pay_items)' every tick, and a " +
			"refused pay_with_item mints no ledger entry, so the standoff soften could never latch. The golden pins " +
			"classifyCoPresentBuy's no-means arm: with an empty purse and no spare good, each co-present line renders the " +
			"coin hold-off ('your purse can't take on salt just now — hold off buying and let your coins recover') instead " +
			"of the goad.",
		build: brokeMakerAtStoreNoSpareGoodsHoldOff,
	},
	{
		name: "keeper_restock_drops_shut_keeps_open_supplier",
		summary: "LLM-216 shut-drop, section-present half: a general-store keeper with coin (30) is low on carrots and has " +
			"TWO carrot suppliers — Bell Farm (open, ~3 coins, affordable) and James Farm (remembered SHUT). The golden pins " +
			"that the '## Restocking' cue renders and lists ONLY Bell Farm as the move_to target: the shut James Farm is " +
			"dropped (not annotated 'found it shut up' as before), so the keeper is never routed to the dead end while a live " +
			"supplier is available. Makes TestGoldensRestockNeverTargetsRememberedShutSupplier non-vacuous (a rendered restock " +
			"section with a remembered-shut structure in the fixture). Pairs with " +
			"broke_keeper_shut_and_unaffordable_suppliers_no_restock (the whole-section suppression half).",
		build: keeperRestockDropsShutKeepsOpenSupplier,
	},
	{
		name: "reseller_restock_routed_to_distributor_not_farm",
		summary: "LLM-223 farm wholesale tier: a non-distributor reseller (Hannah Boggs, the innkeeper) is low on milk and " +
			"has two milk suppliers — Ellis Farm (farm-tagged) and Josiah's General Store (the distributor). The golden pins " +
			"that the '## Restocking' cue lists ONLY the General Store as the walk-to target: the farm is dropped for every " +
			"non-distributor buyer (farm-origin goods route through the distributor), so Hannah is never sent straight to the " +
			"farm the PayWithItem backstop would refuse. Keeps TestGoldensNonDistributorRestockNeverTargetsFarm non-vacuous " +
			"(a rendered restock section with a farm-tagged supplier in the fixture).",
		build: resellerRestockRoutedToDistributorNotFarm,
	},
	{
		name: "distributor_restock_skips_coPresent_reseller",
		summary: "LLM-252 reseller buy-back guard: the distributor (Josiah Thorne) has dipped below his carrot reorder " +
			"threshold while a fellow reseller (John Ellis, the tavernkeeper) is co-present holding carrots he bought — the " +
			"exact buy-back trigger. The golden pins that the '## Restocking' carrot cue routes Josiah to the PRODUCING James " +
			"Farm (a walk-to) and does NOT surface John: John holds carrots only via a `buy` entry, so he is neither a walk-to " +
			"supplier nor the co-present buy-here seller (no 'John Ellis is here — buy it now' imperative). A restock supplier " +
			"must produce/forage the item or be the distributor. Cross-scenario guard: " +
			"TestGoldensRestockSupplierProducesOrForagesOrIsDistributor.",
		build: distributorRestockSkipsCoPresentReseller,
	},
	{
		name: "distributor_restocking_milk_bulk_rate_anchor",
		summary: "LLM-292 buy-leg anchor (the live Josiah milk leak): the distributor (Josiah Thorne) is low on milk, his " +
			"supplier is the wholesaler-tagged Ellis Farm, and the catalog prices milk wholesale 1 / retail 2. The golden pins " +
			"the catalog anchor clause on the '## Restocking' milk line — 'The fair bulk rate buying it in is about 1 coin " +
			"each — pay above that and you are overpaying.' Before LLM-292 the cue's only rate was the buyer's own last-paid, " +
			"a self-poisoning anchor (one overpay re-anchors every later offer; live he paid ~2.2/unit against the 1-coin " +
			"book). No price book in the fixture, so the last-paid CostText and weekly P&L stay silent — isolating the " +
			"catalog anchor as the line's one rate.",
		build: distributorRestockingMilkBulkRateAnchor,
	},
	{
		name: "distributor_restock_observed_supplier_rate",
		summary: "LLM-295 observed-first buy anchor: the same distributor-buys-milk-from-the-farm shape as " +
			"distributor_restocking_milk_bulk_rate_anchor, but the PriceBook now carries a real Ellis Farm milk sale at " +
			"~2 coins/unit — above the catalog wholesale SEED of 1. The golden pins that the '## Restocking' anchor reports the " +
			"OBSERVED supplier rate in lived phrasing ('Of late it has been going for about 2 coins each'), not the seed 1, once " +
			"transaction data exists. The sale's buyer is off-snapshot, so the distributor has no last-paid at Ellis and the " +
			"walk-to line carries no price — isolating the observed anchor as the line's one rate.",
		build: distributorRestockObservedSupplierRate,
	},
	{
		name: "wholesaler_producer_observed_rates",
		summary: "LLM-295 observed-first sell figures: the wholesale producer Moses stands with a customer, and the PriceBook " +
			"carries real transactions for both figures on his wholesale-channel wares line — his own carrot sales to the shop " +
			"at ~2 coins (bulk) and the shop's carrot resales to folk at ~5 coins (shelf), both above the catalog seed " +
			"(wholesale 1 / retail 3). The golden pins the OBSERVED rates in lived phrasing ('Folk have lately paid about 5 " +
			"coins each in the shops, but the shop has lately paid you about 2 coins each'), not the seed. Josiah does not " +
			"produce carrots, so his sales count as shop (shelf) rate, not the wholesale side.",
		build: wholesalerProducerObservedRates,
	},
	{
		name: "innkeeper_fried_meat_below_makings_cost",
		summary: "LLM-475 retail leg (the live Hannah Boggs turn, 2026-07-19): her fried_meat recipe takes 1 meat AND a " +
			"skillet — a DURABLE TOOL that wears rather than being consumed — and she has been selling servings at about 3 " +
			"coins against a 5-coin cut of meat. Both defects fired on one line: the cost anchor charged the whole 5-coin " +
			"skillet against every single serving ('the makings run you about 10 coins each' against a true cost of 5), and " +
			"nothing compared the cost to what she actually realized. She sat at 0 coins trading briskly, every honest sale " +
			"reading as a loss against her never-sell-below-cost coda while the real loss went unnamed. The golden pins the " +
			"tool contributing NOTHING to the sum (meat alone: 'the makings run you about 5 coins each') and the realized " +
			"3-against-5 earning the loss verdict. Cross-scenario guard: TestGoldensMakingsVerdictAlwaysHasACostFigure.",
		build: innkeeperFriedMeatBelowMakingsCost,
	},
	{
		name: "miller_flour_wholesale_earns_nothing",
		summary: "LLM-475 wholesale leg (the live Joseph Scott turn, re-observed verbatim 2026-07-20): the LLM-292 " +
			"wholesale-channel branch replaced the whole clause set, cost anchor included, leaving wholesale producers the " +
			"one class of maker never shown their own margin. His flour line named what the shop paid him (1 coin) and what " +
			"folk paid on the shelf (2) and never what the grinding cost — so he offered five sacks of flour for five sheaves " +
			"of wheat, his recipe's exact input and a full day's grinding for nothing, and called it fair. It also made the " +
			"2026-07-19 flour reprice (wholesale 3) unreachable: a week of sales at 1.00 flat after it landed. The golden " +
			"pins the restored cost sentence and the break-even verdict in Jeff's target shape ('…, it costs you about 1 " +
			"coin each to produce, but the shop has lately paid you about 1 coin each (it earns you nothing). Send other " +
			"buyers to Josiah Thorne.'). Contrast: wholesaler_producer_observed_rates, where a RAW producer (no recipe " +
			"inputs) keeps the unchanged no-cost shape.",
		build: millerFlourWholesaleEarnsNothing,
	},
	{
		name: "miller_wheat_restock_flat_band_anchor",
		summary: "LLM-292 flat-band anchor (the live Joseph Scott case): the miller produces flour from bought-in wheat — a " +
			"DERIVED buy entry (LLM-260) below its two-batch floor (LLM-279) — and wheat's catalog band is FLAT 1/1. Live he " +
			"paid 2/unit for 20 wheat against the flat 1-coin book; even a degenerate single-point band never reached his " +
			"buy-side perception. The golden pins the anchor clause on the derived wheat '## Restocking' line (a " +
			"single-priced band collapses to its one price), the General Store (the distributor holding wheat) as the " +
			"walk-to supplier, and the '## Keeping up production' runway line the same low state motivates.",
		build: millerWheatRestockFlatBandAnchor,
	},
	{
		name: "miller_buys_wheat_direct_from_wholesale_farm",
		summary: "LLM-477 transformer wholesale tier: the miller (Joseph Scott, at the wholesaler-tagged mill) is below his " +
			"wheat floor, and the village holds BOTH the wheat-growing James Farm (wholesaler-tagged) and the General Store " +
			"(the distributor, also holding wheat). The golden pins that the '## Restocking' wheat line now offers the FARM " +
			"as a walk-to supplier alongside the store: wheat is an input to his own flour recipe, so the tier grants him the " +
			"direct channel a plain reseller never gets. Before this he could only buy wheat retail from the same shop that " +
			"takes his flour wholesale — one counterparty on both sides of the mill's business, and no wheat at all whenever " +
			"that shop was shut. Cross-scenario guard: TestGoldensWholesaleSourceOnlyForAllowedItems (which pins that the farm " +
			"is offered for wheat and nothing else). Contrast: reseller_restock_routed_to_distributor_not_farm, where a " +
			"non-transformer is still routed through the distributor.",
		build: millerBuysWheatDirectFromWholesaleFarm,
	},
	{
		name: "owner_holding_repair_nails_in_company",
		summary: "LLM-292 repair-reserve earmark (the live Josiah nail resale): Josiah stands at his worn General Store " +
			"(wear past the repair threshold) holding 3 of the 5 nails a mend takes, in a huddle with John Ellis — the " +
			"situation where a buyer's offer for the nails lands. Live, nothing marked the nails earmarked and the " +
			"role-agnostic coin band made any in-band offer look fair, so he resold 5 repair nails before mending (10 coins " +
			"lost, shop still broken). The golden pins the wares-cue earmark line ('you need 5 of these to mend your General " +
			"Store — the 3 you carry are for that mend, not for sale') rendering alongside his ordinary ware line (milk) and " +
			"the '## Your business' mend nag it shares predicates with.",
		build: ownerHoldingRepairNailsInCompany,
	},
	{
		name: "smith_holding_forge_water_in_company",
		summary: "LLM-609 production-input reservation (the live Ezekiel Crane water sale): a smith in company with " +
			"Elizabeth Ellis holds 2 flasks of water — a `buy` entry on his policy, and the required input of all three " +
			"goods he makes (nail, shovel, skillet) — alongside 6 nails of his own make. Live, on 2026-08-06 Elizabeth " +
			"paid him 1 coin for water; within the hour he held none, all three recipes were input-short and the forge " +
			"had stopped. The wares cue priced the water as stock to move and the only judgment on the line was about " +
			"margin ('selling below your costs loses you coin'), which frames a break-even trade as the problem to solve " +
			"rather than the sale itself. The golden pins the reservation line REPLACING the price ('the 2 you carry are " +
			"for making your own nails, shovels and skillets — makings, not wares') while the nails he actually sells " +
			"keep their ordinary priced line. Cross-scenario guard: TestGoldensNeverPriceAGoodTheBenchHasReserved. " +
			"Contrast: miller_offered_parity_swap_for_flour, where the miller's 37 wheat clear the same floor and so " +
			"keep their price plus a keep-back clause.",
		build: smithHoldingForgeWaterInCompany,
	},
	{
		name: "coin_poor_overstocked_keeper_conserves",
		summary: "LLM-294 working-capital tone gate: a coin-poor keeper (Hannah Boggs, 1 coin — below the 10 floor) sitting " +
			"on unsold stock (20 porridge, dead-stock over the 8 floor) is low on the milk she buys, with the milk seller " +
			"(Josiah) co-present in her huddle. The golden pins BOTH tiers: '## Restocking' flips to the hold-off-buying steer " +
			"(milk named, NO 'Buy it now' imperative despite the co-present seller), and '## What your wares fetch' carries the " +
			"sell-first nudge pointing at the sell tool for her most-overstocked ware (porridge). Cross-scenario guard: " +
			"TestGoldensConserveKeeperNeverGetsBuyImperative.",
		build: coinPoorOverstockedKeeperConserves,
	},
	{
		name: "coin_poor_keeper_alone_conserve_low_stock",
		summary: "LLM-298 dangling-want repro: John Ellis (tavernkeeper) ALONE at his post — the live config (scene 019f38de, " +
			"2026-07-06) that made llama-3.3-70b invent a nonexistent \"Market\" to move_to. 8 coins (below the 10 floor), " +
			"shelves overstocked (20 ale / 14 bread / 13 stew, all over the 8 dead-stock floor), low on the carrots he buys " +
			"in (1 of 6) with his supplier (Josiah, the general-store distributor) NOT co-present. The golden pins the " +
			"'## Restocking' conserve line self-resolving ('- You are low on carrots — no errand for it now; sell first, then " +
			"restock once your purse recovers.'): no co-present seller, no walk-to target, no bare lack for the model to " +
			"improvise a destination for. Cross-scenario guards: TestGoldensConserveKeeperNeverGetsBuyImperative + " +
			"TestGoldensConserveLowItemAlwaysSelfResolves.",
		build: coinPoorKeeperAloneConserveLowStock,
	},
	{
		name: "dairy_keeper_out_of_booster_at_post",
		summary: "LLM-248 optional booster inputs (the LLM-83 dairy sage edge): an Elizabeth-shaped dairy keeper at her farm " +
			"on shift, milk recipe carrying a sage booster (1 sage per execution → +2 milk), sage a buy entry at 0 on hand. " +
			"The golden pins the '## Keeping up production' booster line — the forgone-bonus motivation ('a measure of sage " +
			"in each batch of milk adds 2 extra to the yield') with NO supplier/structure_id/tool mechanics on the line " +
			"(the LLM-64 split; the adjacent '## Restocking' section carries the where). A booster is elective, so the line " +
			"must not read as a stall: no runway / 'enough for about N more' phrasing for it.",
		build: dairyKeeperOutOfBoosterAtPost,
	},
	{
		name: "keeper_worn_skillet_wear_runway",
		summary: "LLM-330 per-use tool durability: a John-shaped tavernkeeper at his tavern on shift, stew recipe " +
			"carrying a skillet TOOL input (durability 20), down to his last skillet with 5 uses left on it (ToolWear). " +
			"The golden pins the wear-phrased '## Keeping up production' line — 'The skillet you make stew with is " +
			"wearing down — 1 on hand, good for about 50 more before you need another.' (5 uses × 10-stew batches), NOT " +
			"the consumed-input 'You use X to make Y' phrasing — alongside the '## Restocking' walk-to line for the " +
			"skillet (the smith sells them), the LLM-64 motivate/act split under the wear model. Matrix-wide guard: " +
			"TestGoldensToolWearLineOnlyForDurableTools.",
		build: keeperWornSkilletWearRunway,
	},
	{
		name: "producer_derived_input_demand",
		summary: "LLM-260 derived procurement, the live Hannah Boggs case: an innkeeper with `porridge: produce` and NO " +
			"hand-authored buy entries stands at her inn with zero milk and zero water (porridge needs both). A dairy farm " +
			"sells milk; nobody anywhere sells water. The golden pins that demand is DERIVED from the recipe — milk gets " +
			"the '## Keeping up production' runway line AND the '## Restocking' walk-to line with no explicit buy entry — " +
			"and that the unobtainable water surfaces NOWHERE (no runway line, no restock line): the buy-path gate keeps " +
			"the engine from motivating a purchase that cannot happen (the phantom fetch-water hires). Matrix-wide guard: " +
			"TestGoldensUnobtainableInputSurfacesNoDemand.",
		build: producerDerivedInputDemand,
	},
	{
		name: "producer_self_sourced_input_no_demand",
		summary: "LLM-260 self-source override, the John Ellis water case: a tavernkeeper produces stew (needs water + meat) " +
			"and ALSO produces his own water — both at zero on hand, with live vendors selling both. The golden pins that " +
			"meat derives buy demand (runway + restock lines) while water derives NONE despite the vendor and the empty " +
			"stock: a produce/forage entry for an input means 'I self-source this', so the derived-demand walk skips it. " +
			"The '## Your trade' cue rendering alongside is deliberate: it shows the self-sourced water routed " +
			"to the PRODUCE path (water offered as a plain batch, stew short of it — the 'you'd need more' clause) while " +
			"the bought input routes to the BUY path — the two procurement lanes of the same recipe, side by side.",
		build: producerSelfSourcedInputNoDemand,
	},
	{
		name: "innkeeper_idle_at_post_trade_scene",
		summary: "LLM-319 headline change: a SINGLE-output producer — Hannah Boggs, porridge only, idle at her inn with " +
			"the makings on hand and nothing in the works — now gets the '## Your trade' cue too (the old forge-choice cue " +
			"was multi-output-gated, so a one-good keeper never saw a production decision). The golden pins the full scene " +
			"for the quietest tier: 'You have no porridge on hand, and none sold this past week. A batch — 10 more — takes " +
			"about an hour and a quarter, and you have the makings.', the neutral 'Start a batch with produce, or see to " +
			"other things.' close (never an imperative — declining is a legitimate outcome), and the 'Your thoughts turn " +
			"to your trade — nothing is in the works right now.' wake warrant. Pairs with innkeeper_batch_in_flight.",
		build: innkeeperIdleAtPostTradeScene,
	},
	{
		name: "innkeeper_batch_in_flight",
		summary: "The same single-output producer (Hannah) mid-batch: she called produce, the inputs are in the pot, and " +
			"~40 minutes of work remain (LLM-319 one-shot cycles). The golden pins the standing 'You are making a batch of " +
			"porridge — about 40 minutes of work left; it only moves along while you're at your post.' self-state line and " +
			"the ABSENCE of the '## Your trade' cue (and so of the produce tool) while the batch runs — the mid-batch half " +
			"of the innkeeper_idle_at_post_trade_scene pair.",
		build: innkeeperBatchInFlight,
	},
	{
		name: "innkeeper_batch_done_beat",
		summary: "LLM-319 completion beat: Hannah's porridge batch just LANDED — 10 bowls minted into her stores, the " +
			"in-flight window cleared — and the ProductionCycleCompleted reactor woke her with the pre-rendered narration " +
			"warrant. The golden pins the 'You finish the batch — 10 porridge ready in your stores.' warrant line (the " +
			"renderNarrationWarrantLine path, new for WarrantKindProductionDone) and that the '## Your trade' cue is back " +
			"in the same prompt — stores now read 'running low' rather than empty, makings on hand — so the very tick that " +
			"celebrates the batch is the tick that grants the next go/no-go decision.",
		build: innkeeperBatchDoneBeat,
	},
	{
		name: "producer_all_goods_at_cap",
		summary: "LLM-324 regression pin: Moses James, a two-good farm producer (carrots + wheat), idle inside his farm " +
			"on shift with BOTH goods at cap — nothing craftable. The golden proves the '## Your trade' cue is SUPPRESSED " +
			"(buildForgeChoice returns nil) so the produce tool is not advertised, killing the at-cap produce-reject loop " +
			"that burned 6-iteration budget_forced ticks live. Contrast smith_choosing_at_forge (one at cap, one craftable) " +
			"where only the craftable good is listed.",
		build: producerAllGoodsAtCap,
	},
	{
		name: "cold_outdoors_in_storm",
		summary: "LLM-412: Lewis Walker caught outdoors in a storm, cold past its red threshold. The golden pins the " +
			"situated cold line — red tier phrase, the rain soaking him, and the FREE relief steer: any roof, plus his own " +
			"home as a concrete (destination: walker_house) move target (the LLM-274 lesson — a steer with no destination " +
			"gets narrated, not walked). The storm ambient prose renders in the same prompt, so the sky and the chill agree.",
		build: coldOutdoorsInStorm,
	},
	{
		name: "cold_outdoors_in_storm_coat_for_sale",
		summary: "LLM-410: the cold_outdoors_in_storm scene with a coat SELLER present — Josiah Thorne holds coats and a " +
			"cloak at the General Store. The golden pins the vendor-gated warm-garment nudge — a warm coat or cloak would " +
			"keep the worst off, buy from General Store (destination: general_store) — rendered AFTER the unconditional " +
			"free-relief steer (a coat is a PAID upgrade to keep working outside, never a substitute for shelter). Contrast " +
			"cold_outdoors_in_storm, whose world has no seller, so no nudge renders — the vendor-gating proof.",
		build: coldOutdoorsInStormCoatForSale,
	},
	{
		name: "cold_outdoors_in_storm_coated",
		summary: "LLM-410: the same red-cold storm-outdoors scene, but Lewis is CARRYING a coat (a warms garment). The " +
			"golden pins the confirming line — the warm clothes are holding the worst of the cold off, enough to keep " +
			"working out here — after the free-relief steer, and the ABSENCE of any buy nudge (he already has one). In the " +
			"sweep the coat caps his outdoor accrual at the under-a-roof rate (coldRatePerMinuteX100).",
		build: coldOutdoorsInStormCoated,
	},
	{
		name: "cold_outdoors_in_storm_threadbare_coat",
		summary: "LLM-422: the storm-outdoors red-cold scene, but Lewis's coat has worn THREADBARE (60 of a 600 " +
			"worked-minute budget — into its last fifth), and Josiah sells coats at the General Store. The golden pins " +
			"the wear-stakes escalation: the garment line reads 'worn thin ... won't hold much longer' and steers to a " +
			"fresh coat (buy from General Store), rendered AFTER the unconditional free-relief line — replacing a failing " +
			"warms garment has real cold stakes (the sweep caps his outdoor accrual at the worse threadbare rate). " +
			"Contrast cold_outdoors_in_storm_coated (a SOUND coat → the confirming note, no replacement nudge).",
		build: coldOutdoorsInStormThreadbareCoat,
	},
	{
		name: "cold_in_unheated_room_storm",
		summary: "LLM-412: the same storm felt through a roof — Lewis chilled (mild) INSIDE a plain structure with no " +
			"hearth. The golden pins the indoors branch: the roof is keeping the rain off and staying in is easing it " +
			"(shelter is real, free relief — the absorbing-state rule), with the lit-fire upsell named but never an " +
			"imperative.",
		build: coldInUnheatedRoomStorm,
	},
	{
		name: "warm_by_lit_fire",
		summary: "LLM-412: Lewis carries a chill but stands inside the tavern whose hearth is LIT. The golden pins the " +
			"warm branch — the fire is working the chill out of him, stay by it — and the ABSENCE of any hearth cue (he " +
			"is not responsible for this fire; a lit fire needs no stoking anyway).",
		build: warmByLitFire,
	},
	{
		name: "keeper_at_dead_hearth_storm",
		summary: "LLM-412: Hannah Boggs, keeper of the tavern, inside at her post while a storm runs and her hearth sits " +
			"OUT, carrying enough firewood to feed it. The golden pins the '## Your hearth' cue (the storm pressing its " +
			"chill into the room, folk in the room feeling the cold — a cold co-present guest escalates the scene) with " +
			"the use-the-stoke-tool-now steer, plus the HearthLow warrant line (the storm wake). The stoke tool rides the " +
			"same Hearth signal (handlers gating test).",
		build: keeperAtDeadHearthStorm,
	},
	{
		name: "keeper_low_hearth_short_wood_with_supplier",
		summary: "LLM-412: Hannah at her post under a calm sky, fire down to embers, holding NO firewood, while Ezekiel — " +
			"who forages firewood (supplier of record) — works the Blacksmith. The golden pins the quiet embers scene and " +
			"the destination-bearing buy steer ('buy from Blacksmith (destination: blacksmith)'), the firewood twin of the " +
			"LLM-274 nail steer: name the supplier or the model narrates the errand and never walks.",
		build: keeperLowHearthShortWoodWithSupplier,
	},
	{
		name: "keeper_mid_stoke_no_restoke",
		summary: "LLM-435: Hannah mid-stoke of her own tavern hearth (a stoke SourceActivity in flight) under a storm, the " +
			"fire still reading OUT because the extension lands only at completion, wood in hand. The golden pins the " +
			"ABSENCE of the '## Your hearth' cue (and so the stoke tool, which rides the same Hearth signal) plus the " +
			"mid-activity coda that holds her to done() — re-advertising stoke to an actor already mid-stoke would bait the " +
			"'already busy ... before tending the fire' reject.",
		build: keeperMidStokeNoRestoke,
	},
	{
		name: "hired_worker_at_employer_low_hearth",
		summary: "LLM-412: Anne Walker, mid-hire (Working) at Hannah's tavern during a storm, fire down to embers, carrying " +
			"firewood. The golden pins the hired framing — '## The hearth where you're working', the truthful relationship, " +
			"the stoke-now steer — plus the hired hearth warrant line (the wake that pierces the laboring shelve; tending " +
			"the fire is the job, not leaving it — no task field anywhere).",
		build: hiredWorkerAtEmployerLowHearth,
	},
	{
		name: "cook_at_burning_hearth",
		summary: "LLM-474: Hannah at her post with porridge on the books (a hearth_lit-boosted recipe) and the Inn fire " +
			"well in — four hours of burn against a 60-min low line. The golden pins the '## The fire and your cooking' " +
			"line at its warm tier AND the ABSENCE of '## Your hearth': a fire burning well needs no stoke, so HearthView " +
			"is nil and the stoke tool stays unadvertised. This pair is why the cooking stake is a separate view — folding " +
			"it into HearthView would have baited the LLM-435 'already busy / not worth stoking' reject.",
		build: cookAtBurningHearth,
	},
	{
		name: "cook_at_ebbing_hearth",
		summary: "LLM-474: the same cook with the fire down to embers and firewood in hand. The golden pins BOTH sections " +
			"and the division of labour between them — '## Your hearth' carries the remedy (the wood, the stoke tool) and " +
			"'## The fire and your cooking' carries only the stake (the next porridge is the poorer for it). Neither " +
			"repeats the other, and the cooking line never issues an imperative.",
		build: cookAtEbbingHearth,
	},
	{
		name: "cook_at_dead_hearth_no_wood",
		summary: "LLM-474: fire out, no firewood carried. The golden pins the coldest tier of the cooking line (a cold " +
			"hearth makes a mean batch) while the where-and-how of getting wood stays in the hearth section — the LLM-64 " +
			"split. Note what does NOT appear: nothing tells her she cannot cook, because she can.",
		build: cookAtDeadHearthNoWood,
	},
	{
		name: "cook_at_hearthless_kitchen",
		summary: "LLM-474: the same cook and the same boosted porridge recipe in a kitchen carrying NO hearth object. The " +
			"golden pins total silence on the fire. This is the fixture standing guard over the promise that every " +
			"non-hearth kitchen in the village — every residence, the farms, the Mill, the Blacksmith — is untouched by " +
			"this ticket and keeps exactly today's yield.",
		build: cookAtHearthlessKitchen,
	},
	{
		name: "keeper_owes_town_rate_constable_present",
		summary: "LLM-557 paying side: Josiah Thorne keeps the General Store, owes one day's town rate, and the constable " +
			"is standing in the shop with him. The golden pins the '## Town rate' cue naming Gideon Marsh verbatim (he is " +
			"the pay recipient argument) and the exact coin, in the quiet single-day tier — a passing courtesy, not a " +
			"demand. Foil of keeper_owes_town_rate_arrears (several days owing → the arrears tier).",
		build: keeperOwesTownRateConstablePresent,
	},
	{
		name: "keeper_owes_town_rate_arrears",
		summary: "LLM-557 arrears tier: the same keeper and constable, but three days of rate have run on (the cap). The " +
			"golden pins the line escalating from the day's courtesy to something the keeper should feel behind on, while " +
			"still naming the constable and the exact amount the pay call needs. The tiering is the felt-needs register " +
			"applied to a debt.",
		build: keeperOwesTownRateArrears,
	},
	{
		name: "constable_collects_town_rate_at_stop",
		summary: "LLM-557 collecting side: the constable himself, called in at the General Store on his rounds, with a " +
			"keeper in arrears in front of him. The golden pins the collector's line naming who is behind and by how " +
			"much — and pins what is deliberately ABSENT: no imperative and no tool. The coin is the keeper's to hand " +
			"over, so the constable has nothing to call; an imperative here would also risk instructing a second " +
			"terminal verb alongside speak.",
		build: constableCollectsTownRateAtStop,
	},
	{
		name: "keeper_owes_town_rate_no_constable",
		summary: "LLM-557 co-location negative arm: the same keeper, the same debt, but no constable anywhere near him. " +
			"The golden pins TOTAL SILENCE on the rate. Unlike the farm-upkeep cue — whose obligation sends the owner on " +
			"an errand to the smith and so rides every tick — a keeper owes nothing but a coin handed over when the man " +
			"is in front of him. Off-scene the cue would be pure nagging with nothing to act on.",
		build: keeperOwesTownRateNoConstable,
	},
	{
		name: "smith_working_clothes_worn_thin",
		summary: "LLM-589 threadbare arm: Ezekiel works his forge in a single shift with 1,200 worked minutes left of " +
			"10,800 (inside the 20% band) and no spare. The golden pins the '## Your clothes' cue — the scene plus the " +
			"walk-to steer to the general store. Garment wear is charged by worked MINUTE (LLM-422), so the smith wears " +
			"clothes out faster than anyone alive, yet until this cue the only clothing demand surface sat behind " +
			"'Storm && !Indoors' and he was never once told. The steer names breeches, not the shift he is wearing out: " +
			"the cue takes the first working kind with a live supplier, since what it owes the model is something he can " +
			"actually buy today. Not co-location-gated (the buy happens elsewhere), so it rides any tick he draws.",
		build: smithWorkingClothesWornThin,
	},
	{
		name: "smith_has_nothing_fit_to_work_in",
		summary: "LLM-589 none arm and the outerwear split in one fixture: Ezekiel owns no working garment at all — only " +
			"a coat, which carries `warms` and belongs to the cold self-line. The golden pins the harder none-tier scene " +
			"firing anyway, and the coat neither satisfying the want nor being offered as the thing to buy. Without the " +
			"split the two clothing cues would cover for each other and a man in a good coat and rags would hear nothing. " +
			"The wording is about having nothing FIT TO WORK IN, never about being unclothed — every villager is " +
			"obviously dressed and the other reading is a retcon.",
		build: smithHasNothingFitToWorkIn,
	},
	{
		name: "smith_worn_clothes_seller_present",
		summary: "LLM-589 co-present arm: the same threadbare smith now shares Josiah's huddle at the store door. The " +
			"golden pins the cue flipping from the walk-to list to the concrete pay_with_item buy-here imperative naming " +
			"Josiah — the same fast path the nail and shovel cues take, closing the at-the-seller dead-spot where the " +
			"weak model narrates the want and then walks off. Foil of smith_working_clothes_worn_thin (Josiah far off → " +
			"the destination is named instead).",
		build: smithWornClothesSellerPresent,
	},
	{
		name: "village_has_no_cloth_to_sell",
		summary: "LLM-589 silence arm: the threadbare smith with the store's shelves bare. Clothing is import-only " +
			"(LLM-410), so no seller anywhere is an ORDINARY state, not a resolution failure — unlike farm upkeep, whose " +
			"fallback can safely name the blacksmith because the smith definitively makes shovels. The golden pins that " +
			"the section renders NOTHING: a standing line naming a problem with no action attached would ride every tick " +
			"of every working actor in the village. Foil of smith_working_clothes_worn_thin — same wear, same posture, " +
			"no stock.",
		build: villageHasNoClothToSell,
	},
	{
		name: "smith_worn_clothes_off_shift",
		summary: "LLM-595 audience boundary: the same threadbare smith stood at his own forge, but outside his shift " +
			"window (an evening shift, while the clock reads mid-morning). The business-person case is the shift " +
			"window — off shift his garments take no wear, and a cue here would be telling him his clothes are going " +
			"when they are not. The golden pins silence. Foil of smith_working_clothes_worn_thin — same wear, same " +
			"spot, wrong hours.",
		build: smithWornClothesOffShift,
	},
	{
		name: "smith_worn_clothes_mender_at_inn",
		summary: "LLM-625 mend arm: the threadbare smith of smith_working_clothes_worn_thin, with Hannah taking in " +
			"mending at the inn (thread on hand) while Josiah's shelves still hold garments. The golden pins the cue " +
			"steering to the MEND — walk-to the inn — instead of the replacement: wear lives on the wearer's in-use " +
			"unit, so a mend brings his own clothes back for less coin than new cloth, and the conserving path wins " +
			"whenever a live mender exists.",
		build: smithWornClothesMenderAtInn,
	},
	{
		name: "smith_worn_clothes_mender_present",
		summary: "LLM-625 co-present arm: the threadbare smith shares Hannah's huddle at the inn. The golden pins the " +
			"concrete pay_with_item imperative naming Hannah and the mending item — the same fast path the " +
			"seller-present garment arm takes, so the mend cannot dead-spot right at the mender.",
		build: smithWornClothesMenderPresent,
	},
	{
		name: "smith_worn_clothes_mender_out_of_thread",
		summary: "LLM-625 thread gate: Hannah takes in mending but holds no thread, so the mend arm is dead and the cue " +
			"falls back to the LLM-589 replacement steer at the general store. A mender with nothing to mend WITH must " +
			"not swallow the working replace path. Foil of smith_worn_clothes_mender_at_inn (thread 6 → 0).",
		build: smithWornClothesMenderOutOfThread,
	},
	{
		name: "smith_nothing_to_work_in_mender_live",
		summary: "LLM-625 none-tier boundary: Ezekiel owns no working garment at all while a thread-stocked mender is " +
			"live. The golden pins the NONE tier keeping the replacement steer — mending conserves garments, it cannot " +
			"create one, so a man with nothing on the peg is never sent to the mender. Foil of " +
			"smith_has_nothing_fit_to_work_in (same wardrobe, a mender added, same steer).",
		build: smithNothingToWorkInMenderLive,
	},
	{
		name: "keeper_off_post_buying_in_company",
		summary: "LLM-611 (the live 2026-08-06 Josiah case): Josiah Thorne, shopkeeper, is OFF his own post — inside the " +
			"Ellis Farm, huddled with Elizabeth Ellis — with 1 coin and a pack of merchandise he means to pay with. The " +
			"golden pins the 'How you buy:' rule, the only trade guidance a keeper carries away from its counter, and " +
			"the ABSENCE of the at-post 'How you trade:' block (WORK-385: a keeper standing in someone else's place is " +
			"not told to sell). Before this he had no rule about value at all here, and settled purchases in goods priced " +
			"at whatever the seller granted — 289 units handed over as pay_items in a week.",
		build: keeperOffPostBuyingInCompany,
	},
	{
		name: "keeper_off_post_buying_alone",
		summary: "LLM-611 audience arm: the same shopkeeper off his own post with the same pack and purse, but with nobody " +
			"within earshot — the seller is indoors in her own farmhouse, so her whereabouts is the single variable " +
			"between the two arms (the extra 'the Ellis Farm is shut' line is a consequence of that move, not a second " +
			"knob — a farm with its keeper indoors elsewhere IS untended). The golden pins silence. The rule is about " +
			"what you hand to someone; with no one to pay there is nothing to weigh, and a standing line on every solo " +
			"walking turn is noise the whole prompt pays for. Foil of keeper_off_post_buying_in_company.",
		build: keeperOffPostBuyingAlone,
	},
}

// townRateSnapshot builds the shared LLM-557 fixture: Josiah Thorne keeping the
// General Store with `owed` coins of town rate outstanding, and Gideon Marsh the
// constable placed according to `constablePresent`. Returned as (snapshot, keeperID,
// constableID) so each scenario can point the render at whichever side it pins.
func townRateSnapshot(owed int, constablePresent bool) (*sim.Snapshot, sim.ActorID, sim.ActorID) {
	const (
		keeperID    = sim.ActorID("josiah")
		constableID = sim.ActorID("gideon")
	)
	zero := 0
	start, end := 480, 1080
	now := 600
	huddle := sim.HuddleID("store_huddle")
	storePos := sim.WorldPos{X: 1500, Y: 1500}.Tile()
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               storePos,
		InsideStructureID: "general_store",
		WorkStructureID:   "general_store",
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             21,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Constable Gideon Marsh": {}},
	}
	gideon := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Constable Gideon Marsh",
		Role:             "constable",
		State:            sim.StateIdle,
		Pos:              storePos,
		WorkStructureID:  "meeting_house",
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            0,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{},
		AttributeSlugs:   []string{sim.AttrConstable},
		Acquaintances:    map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	if constablePresent {
		gideon.InsideStructureID = "general_store"
		gideon.CurrentHuddleID = huddle
	} else {
		// Off at his post across the village — nothing to collect and nothing to pay.
		gideon.InsideStructureID = "meeting_house"
		gideon.Pos = sim.WorldPos{X: 100, Y: 100}.Tile()
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{keeperID: josiah, constableID: gideon},
		Structures: map[sim.StructureID]*sim.Structure{
			"general_store": plainStructure("general_store", "General Store"),
			"meeting_house": plainStructure("meeting_house", "Meeting House"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"general_store": {
				ID:            "general_store",
				DisplayName:   "General Store",
				Pos:           sim.WorldPos{X: 1500, Y: 1500},
				OwnerActorID:  keeperID,
				Tags:          []string{sim.TagBusiness},
				RateOwed:      owed,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, keeperID, constableID
}

// keeperOwesTownRateConstablePresent — the single-day courtesy tier, keeper's side.
func keeperOwesTownRateConstablePresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, keeperID, _ := townRateSnapshot(1, true)
	return snap, keeperID, nil
}

// keeperOwesTownRateArrears — three days run on (the default cap), keeper's side.
func keeperOwesTownRateArrears() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, keeperID, _ := townRateSnapshot(sim.DefaultTownRateMaxOwed, true)
	return snap, keeperID, nil
}

// constableCollectsTownRateAtStop — the collector's side, keeper in arrears present.
func constableCollectsTownRateAtStop() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, _, constableID := townRateSnapshot(sim.DefaultTownRateMaxOwed, true)
	return snap, constableID, nil
}

// keeperOwesTownRateNoConstable — the co-location negative arm: debt, no collector.
func keeperOwesTownRateNoConstable() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, keeperID, _ := townRateSnapshot(sim.DefaultTownRateMaxOwed, false)
	return snap, keeperID, nil
}

// producerDerivedInputDemand is the LLM-260 derived-demand fixture: Hannah-shaped
// innkeeper producing porridge (milk 3 + water 5 per 4-unit batch), no explicit
// buy entries, zero of both inputs. Milk has a first-hand supplier (Ellis Farm);
// water has no vendor anywhere — the derived milk demand surfaces in both restock
// sections, the derived water demand in neither.
func producerDerivedInputDemand() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		ellisID  = sim.ActorID("ellis")
		inn      = sim.StructureID("boggs_inn")
		farm     = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"porridge": 11},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 12},
		}},
	}
	ellis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Elizabeth Ellis",
		State:           sim.StateIdle,
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"milk": 30},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, ellisID: ellis},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:  plainStructure(inn, "Boggs Inn"),
			farm: plainStructure(farm, "Ellis Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {Name: "porridge", DisplayLabel: "porridge", Category: sim.ItemCategoryFood},
			"milk":     {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"water":    {Name: "water", DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {
				OutputItem: "porridge", OutputQty: 4, RateQty: 4, RatePerHours: 1,
				Inputs:         []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}},
				WholesalePrice: 2, RetailPrice: 3,
			},
		},
		RestockReorderPct: 25,
	}
	return snap, hannahID, nil
}

// producerSelfSourcedInputNoDemand is the LLM-260 self-source-override fixture:
// John-shaped tavernkeeper producing stew (water 10 + meat 10 per 5-unit batch)
// AND his own water, zero of both inputs on hand, with first-hand vendors
// selling both. Meat derives buy demand; water derives none — the produce entry
// is the override, not vendor absence.
func producerSelfSourcedInputNoDemand() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID   = sim.ActorID("john")
		amosID   = sim.ActorID("amos")
		wellID   = sim.ActorID("welkeeper")
		tavern   = sim.StructureID("ellis_tavern")
		butchery = sim.StructureID("amos_butchery")
		wellHut  = sim.StructureID("well_hut")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             25,
		Inventory:         map[sim.ItemKind]int{"stew": 4},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "stew", Source: sim.RestockSourceProduce, Max: 10},
			{Item: "water", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	amos := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Amos Reed",
		State:           sim.StateIdle,
		WorkStructureID: butchery,
		Inventory:       map[sim.ItemKind]int{"meat": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "meat", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// A live water vendor proves the water silence below is the self-source
	// override, not a missing buy path.
	welkeeper := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Josiah Thorne",
		State:           sim.StateIdle,
		WorkStructureID: wellHut,
		Inventory:       map[sim.ItemKind]int{"water": 30},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "water", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			johnID: john, amosID: amos, wellID: welkeeper,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern:   plainStructure(tavern, "Ellis Tavern"),
			butchery: plainStructure(butchery, "Reed Butchery"),
			wellHut:  plainStructure(wellHut, "Well Hut"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"stew":  {Name: "stew", DisplayLabel: "stew", Category: sim.ItemCategoryFood},
			"meat":  {Name: "meat", DisplayLabel: "meat"},
			"water": {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"stew": {
				OutputItem: "stew", OutputQty: 5, RateQty: 5, RatePerHours: 2,
				Inputs:         []sim.RecipeInput{{Item: "water", Qty: 10}, {Item: "meat", Qty: 10}},
				WholesalePrice: 2, RetailPrice: 4,
			},
			"water": {OutputItem: "water", OutputQty: 10, RateQty: 10, RatePerHours: 1},
		},
		RestockReorderPct: 25,
	}
	return snap, johnID, nil
}

// dairyKeeperOutOfBoosterAtPost is the LLM-248 booster-cue fixture: a dairy
// keeper on shift inside her farm, producing milk whose recipe carries an
// optional sage booster (+2 per boosted execution), with sage as a buy-restock
// entry and none on hand. A sage-growing herbalist exists as a walk-to supplier
// — the LLM-260 buy-path gate silences the booster motivation when the item is
// unobtainable, so a vendor keeps the line rendering, and the golden now also
// pins the LLM-64 pairing: the booster motivate-line and the "## Restocking"
// sage buy line appear together.
func dairyKeeperOutOfBoosterAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		elizabethID = sim.ActorID("elizabeth")
		prudenceID  = sim.ActorID("prudence")
		farm        = sim.StructureID("ellis_farm")
		herbGarden  = sim.StructureID("ward_garden")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elizabeth Ellis",
		Role:              "dairywoman",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             26,
		Inventory:         map[sim.ItemKind]int{"milk": 10},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "sage", Source: sim.RestockSourceBuy, Max: 3},
		}},
	}
	// The sage supplier: a forage-sourced herbalist at her own garden, stocked —
	// the actionable buy path the booster line (and the sage Restocking line) is
	// gated on.
	prudence := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Prudence Ward",
		State:           sim.StateIdle,
		WorkStructureID: herbGarden,
		Inventory:       map[sim.ItemKind]int{"sage": 5},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "sage", Source: sim.RestockSourceForage, Max: 5},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{elizabethID: elizabeth, prudenceID: prudence},
		Structures: map[sim.StructureID]*sim.Structure{
			farm:       plainStructure(farm, "Ellis Farm"),
			herbGarden: plainStructure(herbGarden, "Ward Garden"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"sage": {Name: "sage", DisplayLabel: "sage"},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {
				OutputItem: "milk", OutputQty: 4, RateQty: 4, RatePerHours: 1,
				BoostInputs:    []sim.BoostInput{{Item: "sage", Qty: 1, BonusQty: 2}},
				WholesalePrice: 1, RetailPrice: 2,
			},
		},
		RestockReorderPct: 25,
	}
	return snap, elizabethID, nil
}

// keeperWornSkilletWearRunway is the LLM-330 durable-tool fixture: John Ellis
// at his tavern with the stew recipe's skillet input under the wear model
// (durability 20), holding his LAST skillet with 5 uses left on it. The reorder
// floor (2 × per-batch draw = 2) trips at 1 on hand, so the wear runway line
// and the skillet Restocking line render together — motivate + act, with the
// runway wear-based (5 uses × 10-stew batch = 50), not the consumed-per-batch
// figure the pre-330 stopgap would have shown.
func keeperWornSkilletWearRunway() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID    = sim.ActorID("john")
		ezekielID = sim.ActorID("ezekiel")
		tavern    = sim.StructureID("ellis_tavern")
		forge     = sim.StructureID("crane_forge")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Inventory:         map[sim.ItemKind]int{"stew": 6, "skillet": 1},
		ToolWear:          map[sim.ItemKind]int{"skillet": 5},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "stew", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "skillet", Source: sim.RestockSourceBuy, Target: 2},
		}},
	}
	// The skillet supplier: a stocked smith at his forge — the actionable buy
	// path the runway line (and the skillet Restocking line) is gated on.
	ezekiel := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Ezekiel Crane",
		Role:            "blacksmith",
		State:           sim.StateIdle,
		WorkStructureID: forge,
		Inventory:       map[sim.ItemKind]int{"skillet": 4},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{johnID: john, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Ellis Tavern"),
			forge:  plainStructure(forge, "Crane Forge"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"stew":    {Name: "stew", DisplayLabel: "stew", DisplayLabelSingular: "bowl of stew", DisplayLabelPlural: "stew", Category: sim.ItemCategoryFood},
			"skillet": {Name: "skillet", DisplayLabel: "skillet", DisplayLabelSingular: "skillet", DisplayLabelPlural: "skillets", Category: "tool", DurabilityUses: 20},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"stew": {
				OutputItem: "stew", OutputQty: 10, RateQty: 30, RatePerHours: 6,
				Inputs:         []sim.RecipeInput{{Item: "skillet", Qty: 1}},
				WholesalePrice: 3, RetailPrice: 5,
			},
		},
		RestockReorderPct: 25,
	}
	return snap, johnID, nil
}

// destituteKeeperBlockedSuppliersNamedWithReasons is the LLM-216 / LLM-406 fixture: a
// DESTITUTE general-store keeper — 0 coins and, unlike the live Josiah, an entirely
// empty pack — with empty carrot and milk stock, alone at his store on shift. His only
// carrot supplier (James Farm) he remembers finding shut; his only milk supplier (Ellis
// Farm) is open, but its remembered price (4 coins) is beyond a purse AND a pack that
// hold nothing at all. He is the one buyer LLM-406 still refuses to send anywhere: no
// coin, no goods, nothing that could settle a pay_with_item.
//
// Both suppliers are resolvable vendor structures, and both GROW what they sell — the
// produce entries are load-bearing. Without them the farms fail the LLM-252 first-hand-
// supplier gate and drop out before the shut/means gates are ever reached, and the
// fixture asserts the right output for the wrong reason (which is what it did, silently,
// from LLM-252 until LLM-406 caught it).
//
// So: WITHOUT the LLM-216 drops the cue would hand him both farms as move_to targets
// (the every-tick tour). With them, neither is a destination — but LLM-406 stops the
// item from vanishing along with the destination: both farms are NAMED as blocked, each
// with its reason, and the section renders a scene with no "(destination: …)" token
// anywhere in it. Clock-free: the shut memory and the price history are stamped relative
// to PublishedAt, and the render path reads no wall clock.
func destituteKeeperBlockedSuppliersNamedWithReasons() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID  = sim.ActorID("josiah")
		jamesID   = sim.ActorID("james")
		ellisID   = sim.ActorID("ellis")
		store     = sim.StructureID("general_store")
		jamesFarm = sim.StructureID("james_farm")
		ellisFarm = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 0, "milk": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 12},
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
		// He went to James Farm and found it shut; Ellis Farm he has no shut memory of.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: jamesFarm, Condition: sim.ObservedClosed}: published.Add(-time.Hour),
		}),
	}
	// Both farms GROW what they sell — without a produce entry they'd fail the LLM-252
	// first-hand-supplier gate and drop out before the shut/means gates were ever
	// consulted, leaving this fixture asserting the right output for the wrong reason.
	james := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "James Fuller",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: jamesFarm,
		Inventory:       map[sim.ItemKind]int{"carrots": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	ellis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Ellis Ward",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 420, Y: 420},
		WorkStructureID: ellisFarm,
		Inventory:       map[sim.ItemKind]int{"milk": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// Josiah's buyer-side price history: 6 coins/carrot from James, 4 coins/milk from
	// Ellis — both beyond his empty purse (the affordability drop), and James is shut
	// on top of that.
	carrotBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	carrotBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 6, Qty: 1, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	milkBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	milkBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 4, Qty: 1, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, jamesID: james, ellisID: ellis,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store:     plainStructure(store, "General Store"),
			jamesFarm: plainStructure(jamesFarm, "James Farm"),
			ellisFarm: plainStructure(ellisFarm, "Ellis Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {Name: "carrots", Capabilities: []string{"portable"}, DisplayLabel: "carrots", Category: sim.ItemCategoryFood},
			"milk":    {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: jamesID, Item: "carrots"}: carrotBuys,
			{SellerID: ellisID, Item: "milk"}:    milkBuys,
		},
	}
	return snap, josiahID, nil
}

// illiquidDistributorBartersForStock is the LLM-406 fixture — the live Josiah Thorne
// deadlock of 2026-07-14, reproduced from the umbilical dump.
//
// He is the village's ONLY distributor, and the wholesale tier makes him the sole
// lawful channel between the farms and everyone else. His store roof degraded; mending
// it cost nails, and to buy them he liquidated essentially his whole book — 2 skillets,
// 2 wheat and 2 coins to the smith. He came back to his post with 3 coins, bare
// shelves, and a pack holding a horseshoe, a skillet and 18 flasks of water.
//
// Every farm's remembered price (4 coins milk, 6 coins carrots) is above his 3-coin
// purse, so the old coins-only gate dropped both suppliers, omitted both items, and
// rendered NO "## Restocking" section at all. He could not earn — his shelves were
// empty, so he had nothing to sell — and so could never climb back over any supplier's
// price. The guard built to stop him touring dead ends guaranteed he stayed at zero
// forever, and it jammed the whole village's retail food chain behind him.
//
// The golden pins the means-to-pay fix: pay_with_item settles in GOODS, and his pack is
// not empty, so both farms are real destinations again — rendered with the barter steer
// ("your coins won't cover it — offer goods you carry in trade") rather than a coin
// price he cannot meet. His own nail purchase minutes earlier was exactly such a
// bundle, which is what makes the coins-only question the wrong one.
func illiquidDistributorBartersForStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID  = sim.ActorID("josiah")
		jamesID   = sim.ActorID("james")
		ellisID   = sim.ActorID("ellis")
		store     = sim.StructureID("general_store")
		jamesFarm = sim.StructureID("james_farm")
		ellisFarm = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at his post
	published := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             3,
		Needs:             map[sim.NeedKey]int{},
		// Bare shelves, but NOT an empty pack: the horseshoe, the skillet and the water
		// are means to pay, and the whole bug was never asking.
		Inventory: map[sim.ItemKind]int{
			"carrots": 0, "milk": 0,
			"horseshoe": 1, "skillet": 1, "water": 18,
		},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 12},
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	james := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "James Fuller",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: jamesFarm,
		Inventory:       map[sim.ItemKind]int{"carrots": 30},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	ellis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Ellis Ward",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 420, Y: 420},
		WorkStructureID: ellisFarm,
		Inventory:       map[sim.ItemKind]int{"milk": 29},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// Both remembered prices sit above his 3-coin purse — the coins-only drop that
	// erased him from his own supply chain.
	carrotBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	carrotBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 6, Qty: 1, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	milkBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	milkBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 4, Qty: 1, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	return &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, jamesID: james, ellisID: ellis,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store:     plainStructure(store, "General Store"),
			jamesFarm: plainStructure(jamesFarm, "James Farm"),
			ellisFarm: plainStructure(ellisFarm, "Ellis Farm"),
		},
		// The store carries the distributor tag: he is the one buyer the farms may
		// lawfully sell to, so the wholesale gate keeps both farms in scope for HIM.
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store):     {ID: sim.VillageObjectID(store), Tags: []string{sim.TagDistributor}},
			sim.VillageObjectID(jamesFarm): {ID: sim.VillageObjectID(jamesFarm), Tags: []string{sim.TagFarm}},
			sim.VillageObjectID(ellisFarm): {ID: sim.VillageObjectID(ellisFarm), Tags: []string{sim.TagFarm}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots":   {Name: "carrots", DisplayLabel: "carrots", Category: sim.ItemCategoryFood},
			"milk":      {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"horseshoe": {Name: "horseshoe", DisplayLabel: "horseshoe"},
			"skillet":   {Name: "skillet", DisplayLabel: "skillet"},
			"water":     {Name: "water", DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: jamesID, Item: "carrots"}: carrotBuys,
			{SellerID: ellisID, Item: "milk"}:    milkBuys,
		},
	}, josiahID, nil
}

// brokeMakerFixture is the LLM-636 innkeeper: Hannah Boggs, coin-broke, holding
// only her makings and the suit on her back, with Josiah's General Store the one
// supplier of the salt, flour and thread she is low on. Built from the live
// 2026-08-18 pack (thread 2, water 6, homespun 1, porridge in the pot; salt and
// flour empty). extraPack is merged into her inventory; atStore puts her in the
// store in a huddle with Josiah (the co-present arm) instead of alone at the Inn.
// No price book: at 0 coins the walk-to means-to-pay gate falls straight to the
// goods question, which is what the fixture is about.
func brokeMakerFixture(extraPack map[sim.ItemKind]int, atStore bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		josiahID = sim.ActorID("josiah")
		inn      = sim.StructureID("the_inn")
		store    = sim.StructureID("general_store")
		huddleID = sim.HuddleID("store_huddle")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift
	published := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	pack := map[sim.ItemKind]int{
		"thread": 2, "water": 6, "homespun": 1, "porridge": 12,
		"salt": 0, "flour": 0,
	}
	for k, v := range extraPack {
		pack[k] = v
	}
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         pack,
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "journeycake", Source: sim.RestockSourceProduce, Max: 12},
			{Item: "water", Source: sim.RestockSourceBuy, Max: 10},
			{Item: "flour", Source: sim.RestockSourceBuy, Max: 6},
			{Item: "salt", Source: sim.RestockSourceBuy, Max: 6},
			{Item: "thread", Source: sim.RestockSourceBuy, Max: 6},
		}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 200, Y: 200},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             194,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"salt": 10, "flour": 20, "thread": 8, "water": 20},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "salt", Source: sim.RestockSourceBuy, Max: 12},
			{Item: "flour", Source: sim.RestockSourceBuy, Max: 20},
			{Item: "thread", Source: sim.RestockSourceBuy, Max: 12},
			{Item: "water", Source: sim.RestockSourceBuy, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:   plainStructure(inn, "The Inn"),
			store: plainStructure(store, "General Store"),
		},
		// The store carries the distributor tag: that is what makes Josiah a lawful
		// supplier of goods he does not himself produce (LLM-252), and what makes his
		// own buy lines wares rather than makings (sim.BarterHolder.Stockholder).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{sim.TagDistributor}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 6, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "flour", Qty: 2}, {Item: "water", Qty: 5}}},
			"journeycake": {OutputItem: "journeycake", OutputQty: 6, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "flour", Qty: 2}, {Item: "water", Qty: 2}}},
			"flour":  {OutputItem: "flour", WholesalePrice: 3, RetailPrice: 4},
			"salt":   {OutputItem: "salt", WholesalePrice: 1, RetailPrice: 2},
			"thread": {OutputItem: "thread", WholesalePrice: 1, RetailPrice: 2},
			"water":  {OutputItem: "water", WholesalePrice: 1, RetailPrice: 1},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {Name: "porridge", DisplayLabel: "porridge", DisplayLabelSingular: "bowl of porridge", DisplayLabelPlural: "bowls of porridge",
				Category: sim.ItemCategoryFood, Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}}},
			"journeycake": {Name: "journeycake", DisplayLabel: "journeycake", DisplayLabelSingular: "journeycake", DisplayLabelPlural: "journeycakes",
				Category: sim.ItemCategoryFood, Capabilities: []string{"portable"}, Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 4}}},
			"water": {Name: "water", DisplayLabel: "water", DisplayLabelSingular: "flask of water", DisplayLabelPlural: "flasks of water",
				Category: sim.ItemCategoryDrink, Capabilities: []string{"portable"}, Satisfies: []sim.ItemSatisfaction{{Attribute: "thirst", Immediate: 8}}},
			"flour":    {Name: "flour", DisplayLabel: "flour", DisplayLabelSingular: "sack of flour", DisplayLabelPlural: "sacks of flour", Category: sim.ItemCategoryMaterial},
			"salt":     {Name: "salt", DisplayLabel: "salt", DisplayLabelSingular: "sack of salt", DisplayLabelPlural: "sacks of salt", Category: sim.ItemCategoryMaterial},
			"thread":   {Name: "thread", DisplayLabel: "thread", DisplayLabelSingular: "spool of thread", DisplayLabelPlural: "spools of thread", Category: sim.ItemCategoryMaterial},
			"homespun": {Name: "homespun", DisplayLabel: "homespun", DisplayLabelSingular: "suit of homespun", DisplayLabelPlural: "suits of homespun", Category: "clothing", WearMinutes: 3000},
		},
		RestockReorderPct: 25,
	}
	if atStore {
		hannah.InsideStructureID = store
		hannah.Pos = sim.TilePos{X: 201, Y: 200}
		hannah.CurrentHuddleID = huddleID
		josiah.CurrentHuddleID = huddleID
		snap.Huddles = map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{hannahID: {}, josiahID: {}}},
		}
	}
	return snap, hannahID, nil
}

// brokeMakerMakingsAreNotBarter — LLM-636, walk-to arm: only makings and the suit
// on her back, so no barter steer and the blocked no-means scene on every line.
func brokeMakerMakingsAreNotBarter() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return brokeMakerFixture(nil, false)
}

// brokeMakerOwnProduceIsBarter — LLM-636: the same pack plus her own journeycake,
// which IS spare, so the barter steer returns.
func brokeMakerOwnProduceIsBarter() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return brokeMakerFixture(map[sim.ItemKind]int{"journeycake": 3}, false)
}

// brokeMakerAtStoreNoSpareGoodsHoldOff — LLM-636, co-present arm: in the store with
// Josiah, no coin and no spare good, so the buy-here goad softens to the coin
// hold-off instead of goading an offer the intake gate would refuse.
func brokeMakerAtStoreNoSpareGoodsHoldOff() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return brokeMakerFixture(nil, true)
}

// keeperRestockDropsShutKeepsOpenSupplier is the LLM-216 section-present fixture: a
// coin-holding keeper (Thomas Bishop, 30 coins) is low on carrots and has two carrot
// suppliers — Bell Farm (open, remembered price ~3 coins, affordable) and James Farm
// (remembered shut). With the shut James Farm dropped and the affordable Bell Farm
// kept, the "## Restocking" cue renders and lists ONLY Bell Farm as the walk-to
// target — the visible half of the shut-drop, and the fixture that keeps
// TestGoldensRestockNeverTargetsRememberedShutSupplier non-vacuous (a rendered restock
// section carrying a remembered-shut structure). Clock-free: the shut memory and price
// history are stamped relative to PublishedAt.
func keeperRestockDropsShutKeepsOpenSupplier() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		thomasID  = sim.ActorID("thomas")
		bellID    = sim.ActorID("bell")
		jamesID   = sim.ActorID("james")
		store     = sim.StructureID("general_store")
		bellFarm  = sim.StructureID("bell_farm")
		jamesFarm = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	thomas := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Thomas Bishop",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 12},
		}},
		// He remembers James Farm shut; Bell Farm he does not.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: jamesFarm, Condition: sim.ObservedClosed}: published.Add(-time.Hour),
		}),
	}
	bell := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Bell Farmer",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: bellFarm,
		Inventory:       map[sim.ItemKind]int{"carrots": 40},
		// Produces carrots, so it's a first-hand supplier (LLM-252). Untagged (not
		// wholesaler), so this fixture isolates the shut-drop, not the wholesale gate.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	james := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "James Fuller",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 420, Y: 420},
		WorkStructureID: jamesFarm,
		Inventory:       map[sim.ItemKind]int{"carrots": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// Buyer-side price history: ~3 coins/carrot at Bell (affordable on 30 coins), ~6 at
	// James (which is shut anyway).
	bellBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	bellBuys.Push(sim.PriceObservation{BuyerID: thomasID, Amount: 3, Qty: 1, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	jamesBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	jamesBuys.Push(sim.PriceObservation{BuyerID: thomasID, Amount: 6, Qty: 1, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			thomasID: thomas, bellID: bell, jamesID: james,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store:     plainStructure(store, "General Store"),
			bellFarm:  plainStructure(bellFarm, "Bell Farm"),
			jamesFarm: plainStructure(jamesFarm, "James Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {Name: "carrots", Capabilities: []string{"portable"}, DisplayLabel: "carrots", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: bellID, Item: "carrots"}:  bellBuys,
			{SellerID: jamesID, Item: "carrots"}: jamesBuys,
		},
	}
	return snap, thomasID, nil
}

// resellerRestockRoutedToDistributorNotFarm is the LLM-223 wholesale-tier fixture
// (generalized to the wholesaler tag in LLM-252): a non-distributor reseller
// (Hannah Boggs, the innkeeper) is low on milk and has two milk suppliers — Ellis
// Farm (tagged farm+wholesaler, and a milk PRODUCER so only the wholesale gate can
// drop it) and Josiah's General Store (the distributor-tagged wholesaler). The
// wholesale source is dropped from every non-distributor's buy cues, so the
// "## Restocking" section lists ONLY the General Store as the walk-to target:
// Hannah restocks wholesale-origin milk through the distributor, never straight
// from the farm the PayWithItem backstop would refuse. Keeps
// TestGoldensNonDistributorRestockNeverTargetsFarm non-vacuous (a rendered restock
// section with a farm-tagged supplier present in the fixture). Clock-free: no
// price/shut memory and no wall-clock read in the render path.
func resellerRestockRoutedToDistributorNotFarm() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		josiahID = sim.ActorID("josiah")
		ellisID  = sim.ActorID("ellis")
		inn      = sim.StructureID("the_inn")
		store    = sim.StructureID("general_store")
		farm     = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the inn
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Josiah Thorne",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 200, Y: 200},
		WorkStructureID: store,
		Inventory:       map[sim.ItemKind]int{"milk": 40},
	}
	ellis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Ellis Ward",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"milk": 40},
		// Produces milk, so the LLM-252 supplier gate would KEEP him — isolating the
		// wholesale gate as the sole reason he's dropped from Hannah's cues below.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			hannahID: hannah, josiahID: josiah, ellisID: ellis,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:   plainStructure(inn, "The Inn"),
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: ellisID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
	}
	return snap, hannahID, nil
}

// coinPoorOverstockedKeeperConserves is the LLM-294 working-capital tone-gate fixture.
// Hannah Boggs keeps her inn on shift with just 1 coin (below the 10-coin floor) while
// holding 20 porridge she made and can't move (dead stock — over the absolute 8 floor,
// no recent sales) and is low on the milk she buys in (2 of 12). Josiah, who produces
// milk, shares her huddle — the co-present buy path that would normally fire "buy it
// now". The golden pins both tiers of the gate: "## Restocking" flips to the
// conserve steer (milk named, no buy imperative) and "## What your wares fetch" gains
// the sell-first nudge naming porridge (her most-overstocked ware) and the sell tool.
// Fixed PublishedAt, no price book, no orders → byte-stable, no wall-clock in render.
func coinPoorOverstockedKeeperConserves() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		josiahID = sim.ActorID("josiah")
		inn      = sim.StructureID("the_inn")
		store    = sim.StructureID("general_store")
		huddleID = sim.HuddleID("inn_huddle")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the inn
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddleID,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"porridge": 20, "milk": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 11, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: inn, // co-present in the inn huddle so the milk buy path resolves this tick
		CurrentHuddleID:   huddleID,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:       published,
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		MerchantCoinFloor: 10,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:   plainStructure(inn, "The Inn"),
			store: plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{hannahID: {}, josiahID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", WholesalePrice: 1, RetailPrice: 1},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {Name: "porridge", DisplayLabel: "porridge", Category: sim.ItemCategoryFood},
			"milk":     {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"water":    {Name: "water", DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
	}
	return snap, hannahID, nil
}

// coinPoorKeeperAloneConserveLowStock is the LLM-298 dangling-want repro. It mirrors
// the live scene (019f38de, 2026-07-06): John Ellis, tavernkeeper, ALONE at his post
// with a thin purse (8 coins, below the 10 floor) and overstocked shelves (20 ale,
// 14 bread, 13 stew — all clearing the 8 dead-stock floor, no recent sales) is low on
// the carrots he buys in (1 of cap 6). His carrot supplier is Josiah, the village
// DISTRIBUTOR at the general store (a reseller buys from the distributor, not wholesale
// from a farm), NOT co-present — so the conserve branch strips the walk-to list and the
// co-present buy imperative alike. The golden pins the self-resolving conserve line:
// named lack + what to do INSTEAD (hold, sell first, restock later), never a bare want
// with no outlet — the vacuum that made the live NPC invent a "Market" to move_to.
// Clock-free render (no price book, no orders), byte-stable.
func coinPoorKeeperAloneConserveLowStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID   = sim.ActorID("john")
		josiahID = sim.ActorID("josiah")
		tavern   = sim.StructureID("the_tavern")
		store    = sim.StructureID("general_store")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the tavern, alone
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"ale": 20, "bread": 14, "stew": 13, "carrots": 1},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "ale", Source: sim.RestockSourceProduce, Max: 24},
			{Item: "bread", Source: sim.RestockSourceProduce, Max: 20},
			{Item: "stew", Source: sim.RestockSourceProduce, Max: 20},
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 6},
		}},
	}
	// Josiah runs the general store — the village DISTRIBUTOR, John's carrot supplier
	// (a reseller buys from the distributor, not wholesale from a farm). NOT co-present,
	// so the conserve branch strips his walk-to entry too (no destination dangled). John
	// has never bought from him (no price on record), so the unknown price keeps the
	// supplier past the affordability drop even on 8 coins. A distributor is a restock
	// supplier of anything he stocks (isRestockSupplierOf via the TagDistributor store),
	// so no RestockPolicy is needed on him.
	josiah := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Josiah Thorne",
		Role:            "shopkeeper",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: store,
		Inventory:       map[sim.ItemKind]int{"carrots": 40},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		MerchantCoinFloor: 10,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{johnID: john, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "The Tavern"),
			store:  plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// The general store is the village distributor — the one carrot source a
			// plain reseller (John) can see. A wholesaler-tagged farm would be visible
			// only to the distributor himself (eachVendorOffer), so the reseller's
			// supply chain runs through the distributor. This surfaces the walk-to path
			// (which conserve then strips) — the live config where the low-carrots line renders.
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"ale":     {OutputItem: "ale", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2},
			"bread":   {OutputItem: "bread", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2},
			"stew":    {OutputItem: "stew", OutputQty: 10, WholesalePrice: 1, RetailPrice: 2},
			"carrots": {OutputItem: "carrots", WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"ale":     {Name: "ale", DisplayLabel: "ale", Category: sim.ItemCategoryDrink},
			"bread":   {Name: "bread", DisplayLabel: "bread", Category: sim.ItemCategoryFood},
			"stew":    {Name: "stew", DisplayLabel: "stew", Category: sim.ItemCategoryFood},
			"carrots": {Name: "carrots", Capabilities: []string{"portable"}, DisplayLabel: "carrots", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
	}
	// No restock warrant: LLM-298 Phase 2 suppresses the buy-restock wakeup for a
	// conserving keeper (sim.actorConserving), so the real steady-state render for John
	// is a routine at-post turn — the "## Restocking" section still renders (it is
	// content-gated on low stock, not on the warrant) and carries the self-resolving
	// line. The producer-side suppression is pinned separately in restock_tick_test.go.
	return snap, johnID, nil
}

// distributorRestockSkipsCoPresentReseller is the LLM-252 buy-back-guard fixture:
// Josiah Thorne (the distributor) has dipped below his carrot reorder threshold
// while John Ellis (the tavernkeeper, a carrot RESELLER via a `buy` entry) is
// co-present holding 12 carrots — the exact buy-back trigger. His only genuine
// carrot supplier, Moses at James Farm, PRODUCES carrots and is not co-present. The
// golden pins that the "## Restocking" carrot cue routes Josiah to the producing
// James Farm as a walk-to and NEVER surfaces John: a reseller who merely holds the
// item is not a first-hand supplier, so he is neither listed as a walk-to target
// nor named in the co-present buy-here imperative. Josiah is the distributor, so the
// wholesale gate keeps the farm visible to him; the reseller drop is the LLM-252
// producer/forager/distributor gate. Clock-free: no price/shut memory, no wall-clock
// read in the render path.
func distributorRestockSkipsCoPresentReseller() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID  = sim.ActorID("josiah")
		johnID    = sim.ActorID("john")
		mosesID   = sim.ActorID("moses")
		store     = sim.StructureID("general_store")
		tavern    = sim.StructureID("the_tavern")
		jamesFarm = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		CurrentHuddleID:   "h1",
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 1}, // 1 of cap 6 → below the 25% reorder line
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 6},
		}},
	}
	// John is a carrot RESELLER (buy entry) co-present with Josiah, holding stock —
	// the buy-back specimen. He must NOT surface as a supplier.
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 11, Y: 10},
		WorkStructureID:   tavern,
		InsideStructureID: store, // visiting the distributor's store, carrots in hand — the buy-back specimen
		CurrentHuddleID:   "h1",
		Inventory:         map[sim.ItemKind]int{"carrots": 12},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	// Moses PRODUCES carrots at the farm — the legitimate supplier, not co-present.
	moses := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Moses James",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: jamesFarm,
		Inventory:       map[sim.ItemKind]int{"carrots": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, johnID: john, mosesID: moses,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store:     plainStructure(store, "General Store"),
			tavern:    plainStructure(tavern, "The Tavern"),
			jamesFarm: plainStructure(jamesFarm, "James Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store):     {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
			sim.VillageObjectID(jamesFarm): {ID: sim.VillageObjectID(jamesFarm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {Name: "carrots", Capabilities: []string{"portable"}, DisplayLabel: "carrots", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
	}
	return snap, josiahID, nil
}

// distributorRestockingMilkBulkRateAnchor is the LLM-292 buy-leg fixture: the
// distributor, low on milk (2 of cap 12), with the wholesaler-tagged Ellis Farm
// as his walk-to supplier and milk priced wholesale 1 / retail 2 in the catalog.
// Pins the catalog buying-in anchor on the Restocking line. No price book — the
// per-vendor last-paid CostText, the affordability fact, and the weekly P&L all
// stay silent, so the anchor is the line's only rate. Clock-free render path.
func distributorRestockingMilkBulkRateAnchor() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID    = sim.ActorID("josiah")
		elizabethID = sim.ActorID("elizabeth")
		store       = sim.StructureID("general_store")
		farm        = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Elizabeth Ellis",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"milk": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, elizabethID: elizabeth,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: elizabethID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
	}
	return snap, josiahID, nil
}

// distributorRestockObservedSupplierRate is the LLM-295 observed-buy-anchor fixture:
// the distributor-buys-milk-from-the-farm shape, but the PriceBook carries a real
// Ellis Farm milk sale at ~2 coins/unit (to an off-snapshot villager), above the
// catalog wholesale seed of 1. Pins that the "## Restocking" buy anchor reports the
// OBSERVED reachable-supplier rate once transaction data exists. Clock-anchored via
// PublishedAt so the observation lands inside the sales window.
func distributorRestockObservedSupplierRate() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID    = sim.ActorID("josiah")
		elizabethID = sim.ActorID("elizabeth")
		store       = sim.StructureID("general_store")
		farm        = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Elizabeth Ellis",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"milk": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// Ellis has actually been selling milk at ~2 coins/unit — above the catalog
	// wholesale seed of 1. The observed rate is what the anchor must report. Buyer
	// is off-snapshot, so the distributor keeps no last-paid at Ellis (empty CostText).
	ellisMilkSales := sim.NewRingBuffer[sim.PriceObservation](8)
	ellisMilkSales.Push(sim.PriceObservation{BuyerID: "mary", Amount: 2, Qty: 1, Consumers: 1, At: published.Add(-6 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, elizabethID: elizabeth,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: elizabethID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: elizabethID, Item: "milk"}: ellisMilkSales,
		},
	}
	return snap, josiahID, nil
}

// wholesalerProducerObservedRates is the LLM-295 observed-sell-figures fixture: the
// wholesale producer Moses stands with a customer, and the PriceBook carries real
// transactions for both figures on his wholesale-channel wares line — his own carrot
// sales to the shop at ~2 coins (the bulk rate) and the shop's carrot resales to folk
// at ~5 coins (the shelf rate), both above the catalog seed (wholesale 1 / retail 3).
// Josiah does not produce carrots, so his sales count as the shop/shelf side, not the
// wholesale side. Pins the line reporting the OBSERVED rates in lived phrasing.
func wholesalerProducerObservedRates() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID   = sim.ActorID("moses")
		silenceID = sim.ActorID("silence")
		josiahID  = sim.ActorID("josiah")
		commons   = sim.StructureID("commons")
		farm      = sim.StructureID("james_farm")
		store     = sim.StructureID("general_store")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		WorkStructureID:   farm,
		Coins:             38,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 30},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 30},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             22,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Moses James": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: store,
		WorkStructureID:   store,
	}
	// Bulk rate observed: Moses's own carrot sales to the shop at ~2/unit.
	mosesCarrotSales := sim.NewRingBuffer[sim.PriceObservation](8)
	mosesCarrotSales.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 2, Qty: 1, Consumers: 1, At: published.Add(-12 * time.Hour)})
	// Shelf rate observed: the shop's carrot resales to folk at ~5/unit. Josiah does
	// not produce carrots, so observedShopSales counts him as a shop, not wholesale.
	shopCarrotSales := sim.NewRingBuffer[sim.PriceObservation](8)
	shopCarrotSales.Push(sim.PriceObservation{BuyerID: silenceID, Amount: 5, Qty: 1, Consumers: 1, At: published.Add(-6 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses, silenceID: silence, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
			farm:    plainStructure(farm, "James Farm"),
			store:   plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{mosesID: {}, silenceID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"carrots": {OutputItem: "carrots", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 3},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {
				Name:         "carrots",
				Capabilities: []string{"portable"}, DisplayLabel: "Carrots",
				DisplayLabelSingular: "carrot", DisplayLabelPlural: "carrots",
				Category: sim.ItemCategoryFood,
			},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: mosesID, Item: "carrots"}:  mosesCarrotSales,
			{SellerID: josiahID, Item: "carrots"}: shopCarrotSales,
		},
	}
	return snap, mosesID, nil
}

// millerWheatRestockFlatBandAnchor is the LLM-292 flat-band fixture: the miller
// produces flour from bought-in wheat (derived buy entry — no hand-authored wheat
// row), his wheat shelf (2) is below the two-batch floor (2×4=8), and wheat's
// catalog band is flat 1/1. His supplier is the distributor's store. Pins the
// anchor clause collapsing a single-priced band to its one price, riding a
// DERIVED entry, alongside the production runway line. Also carries a second low
// buy entry with NO catalog price (sacks, 1 of cap 8, same store supplier) whose
// line must render anchor-FREE — the mixed priced/unpriced section that keeps the
// per-line attachment check in TestRestockCatalogAnchorRendersWithCatalogPrice
// non-vacuous (code_review). Clock-free render path.
func millerWheatRestockFlatBandAnchor() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josephID = sim.ActorID("joseph")
		josiahID = sim.ActorID("josiah")
		mill     = sim.StructureID("the_mill")
		store    = sim.StructureID("general_store")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the mill
	joseph := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Joseph Scott",
		Role:              "miller",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   mill,
		InsideStructureID: mill,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"wheat": 2, "sack": 1},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "flour", Source: sim.RestockSourceProduce, Max: 10},
			{Item: "sack", Source: sim.RestockSourceBuy, Max: 8},
		}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Josiah Thorne",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 200, Y: 200},
		WorkStructureID: store,
		Inventory:       map[sim.ItemKind]int{"wheat": 40, "sack": 20},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josephID: joseph, josiahID: josiah,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			mill:  plainStructure(mill, "The Mill"),
			store: plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(mill):  {ID: sim.VillageObjectID(mill), OwnerActorID: josephID, Tags: []string{sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"flour": {OutputItem: "flour", OutputQty: 2, RateQty: 2, RatePerHours: 1, WholesalePrice: 2, RetailPrice: 3,
				Inputs: []sim.RecipeInput{{Item: "wheat", Qty: 4}}},
			"wheat": {OutputItem: "wheat", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {Name: "wheat", DisplayLabel: "wheat", Category: sim.ItemCategoryMaterial},
			"flour": {Name: "flour", DisplayLabel: "flour", Category: sim.ItemCategoryMaterial},
			"sack":  {Name: "sack", DisplayLabel: "sacks", Category: sim.ItemCategoryMaterial},
		},
		RestockReorderPct: 25,
	}
	return snap, josephID, nil
}

// millerBuysWheatDirectFromWholesaleFarm is the LLM-477 fixture: the same miller as
// millerWheatRestockFlatBandAnchor (wholesaler-tagged mill, flour ← wheat, wheat shelf
// below its floor), but the village now also holds the wheat-GROWING James Farm, itself
// wholesaler-tagged. Before LLM-477 the farm was invisible to him and his only wheat
// channel was the distributor's shop — the same shop that buys his flour wholesale, so
// he crossed the spread twice with one counterparty on both sides of his business (and
// stalled outright whenever that shop was shut). Pins that the restock cue now offers
// BOTH walk-to sources, which is the supply independence the ticket is for. Clock-free
// render path.
func millerBuysWheatDirectFromWholesaleFarm() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, josephID, warrants := millerWheatRestockFlatBandAnchor()
	const (
		mosesID = sim.ActorID("moses")
		farm    = sim.StructureID("james_farm")
	)
	snap.Actors[mosesID] = &sim.ActorSnapshot{
		Kind:            sim.KindNPCShared,
		DisplayName:     "Moses James",
		Role:            "farmer",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 150, Y: 150},
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"wheat": 40},
		// He GROWS the wheat, so he passes the LLM-252 first-hand supplier gate and
		// the LLM-477 tier is the only thing that decides whether Joseph sees him.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap.Structures[farm] = plainStructure(farm, "James Farm")
	// A live farm carries both tags; only wholesaler gates selling.
	snap.VillageObjects[sim.VillageObjectID(farm)] = &sim.VillageObject{
		ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler},
	}
	return snap, josephID, warrants
}

// ownerHoldingRepairNailsInCompany is the LLM-292 earmark fixture: Josiah on the
// loiter pin of his worn General Store (wear 450 ≥ repair threshold 400, below
// degrade 600), holding 3 of the 5 nails a mend takes plus his ordinary milk
// stock (6 of cap 12 — above the reorder line, so no Restocking section), in a
// huddle with John Ellis. Pins the wares-cue repair-reserve line alongside the
// ordinary ware line and the "## Your business" mend nag. Clock-free render path.
func ownerHoldingRepairNailsInCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		johnID   = sim.ActorID("john")
		store    = sim.VillageObjectID("general_store")
		huddle   = sim.HuddleID("h1")
	)
	zero := 0
	now := 600 // 10:00
	storePin := sim.WorldPos{X: 100, Y: 100}.Tile()
	josiah := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Josiah Thorne",
		Role:            "shopkeeper",
		State:           sim.StateIdle,
		Pos:             storePin,
		CurrentHuddleID: huddle,
		Coins:           20,
		Needs:           map[sim.NeedKey]int{},
		Inventory:       map[sim.ItemKind]int{"nail": 3, "milk": 6},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
		Acquaintances: map[string]sim.Acquaintance{"John Ellis": {}},
	}
	john := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "John Ellis",
		Role:            "tavernkeeper",
		State:           sim.StateIdle,
		Pos:             storePin,
		CurrentHuddleID: huddle,
		Coins:           15,
		Needs:           map[sim.NeedKey]int{},
		Acquaintances:   map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, johnID: john,
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			store: {
				ID:            store,
				DisplayName:   "General Store",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  josiahID,
				Tags:          []string{sim.TagBusiness},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, johnID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"nail": {Name: "nail", DisplayLabel: "nails", Category: sim.ItemCategoryMaterial},
		},
		RestockReorderPct: 25,
	}
	return snap, josiahID, nil
}

// smithHoldingForgeWaterInCompany is the LLM-609 fixture: Ezekiel Crane in a
// huddle with Elizabeth Ellis, carrying 2 water (the required input of every good
// he makes) and 6 of his own nails. The water is a `buy` entry, so before this it
// walked the resale branch of the wares cue and got a price and a margin caution
// — the exact line the live sale was weighed against.
//
// Clock-free and history-free on purpose: no realized sales or purchases, so the
// priced nail line carries no observed-rate clauses and the golden pins the
// reservation and nothing else that could drift.
func smithHoldingForgeWaterInCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID   = sim.ActorID("ezekiel")
		elizabethID = sim.ActorID("elizabeth")
		forge       = sim.VillageObjectID("blacksmith")
		huddle      = sim.HuddleID("h1")
	)
	zero := 0
	now := 600 // 10:00
	forgePin := sim.WorldPos{X: 100, Y: 100}.Tile()
	ezekiel := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Ezekiel Crane",
		Role:            "blacksmith",
		State:           sim.StateIdle,
		Pos:             forgePin,
		CurrentHuddleID: huddle,
		Coins:           514,
		Needs:           map[sim.NeedKey]int{},
		Inventory:       map[sim.ItemKind]int{"water": 2, "nail": 6},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
			{Item: "shovel", Source: sim.RestockSourceProduce, Max: 3},
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "water", Source: sim.RestockSourceBuy, Max: 12},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Elizabeth Ellis": {}},
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Elizabeth Ellis",
		Role:            "farmer",
		State:           sim.StateIdle,
		Pos:             forgePin,
		CurrentHuddleID: huddle,
		Coins:           40,
		Needs:           map[sim.NeedKey]int{},
		Acquaintances:   map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			ezekielID: ezekiel, elizabethID: elizabeth,
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			forge: {
				ID:            forge,
				DisplayName:   "Blacksmith",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  ezekielID,
				Tags:          []string{sim.TagBusiness},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, elizabethID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"water":   {OutputItem: "water", OutputQty: 1, RateQty: 12, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
			"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2, Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}}},
			"shovel":  {OutputItem: "shovel", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 5, RetailPrice: 10, Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}}},
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 6, RetailPrice: 12, Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water":   {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "water", Category: sim.ItemCategoryDrink},
			"nail":    {Name: "nail", DisplayLabel: "nails", Category: sim.ItemCategoryMaterial},
			"shovel":  {Name: "shovel", DisplayLabel: "shovels", Category: sim.ItemCategoryMaterial},
			"skillet": {Name: "skillet", DisplayLabel: "skillets", Category: sim.ItemCategoryMaterial},
		},
		RestockReorderPct: 25,
	}
	return snap, ezekielID, nil
}

// buyerKeptConsumeRemainderReconciled is the LLM-188 buyer-POV fixture: Anne
// Walker took a consume_now quote for 5 blueberries from Prudence Ward, but her
// hunger was low so the needs-clamp (consumableUnits, ZBBS-WORK-391) ate 1 and
// pocketed 4 to her pack. The settled ledger entry carries KeptUnits=4, and the
// golden pins that the "## Recently settled offers" line reads "you ate 1 on the
// spot and kept the other 4" — internally consistent with the Blueberries (x4)
// she carries — rather than the bare "you had it right away" that contradicted
// her inventory and triggered the confabulated short-count. Clock-free: the
// settled-offers recency window is measured against the fixture's PublishedAt /
// ResolvedAt, and the render path reads no wall clock.
func buyerKeptConsumeRemainderReconciled() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		anneID     = sim.ActorID("anne")
		prudenceID = sim.ActorID("prudence")
		apothecary = sim.StructureID("apothecary")
	)
	now := 915 // 15:15, the repro window
	published := time.Date(2026, 6, 30, 15, 15, 0, 0, time.UTC)
	anne := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Walker",
		Role:              "traveler",
		State:             sim.StateIdle,
		InsideStructureID: apothecary,
		Coins:             20,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"blueberries": 4},
		Acquaintances:     map[string]sim.Acquaintance{"Prudence Ward": {}},
	}
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "apothecary",
		State:             sim.StateIdle,
		InsideStructureID: apothecary,
		WorkStructureID:   apothecary,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
	}
	settled := &sim.PayLedgerEntry{
		ID: 449, BuyerID: anneID, SellerID: prudenceID,
		ItemKind: "blueberries", Qty: 5, Amount: 10, ConsumeNow: true,
		KeptUnits:  4,
		State:      sim.PayLedgerStateAccepted,
		ResolvedAt: published.Add(-30 * time.Second),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{anneID: anne, prudenceID: prudence},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{449: settled},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles:          map[sim.HuddleID]*sim.Huddle{},
		Structures:       map[sim.StructureID]*sim.Structure{apothecary: plainStructure(apothecary, "PW Apothecary")},
	}
	return snap, anneID, nil
}

// buyerOfferDeclinedSellerShortStock is the LLM-296 buyer-POV fixture: Josiah
// Thorne offered 6 carrots + 1 coin for 5 nails to Ezekiel Crane, who declined —
// Ezekiel holds only 1 nail. The settled ledger entry is Declined, and the golden
// pins that "## Recently settled offers" names the OFFERED bundle (so a re-post
// reads as visibly the same, not a byte-identical thin line the model can't tell
// apart) and appends the engine-known shortfall "they hold only 1 nail" as the
// informed reason it closed. Clock-free: the recency window is measured against
// the fixture's PublishedAt / ResolvedAt and the render path reads no wall clock.
func buyerOfferDeclinedSellerShortStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID  = sim.ActorID("josiah")
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	now := 1113 // 18:33, the live window
	published := time.Date(2026, 7, 6, 18, 33, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Josiah Thorne",
		Role:              "traveler",
		State:             sim.StateIdle,
		InsideStructureID: forge,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 6},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: forge,
		WorkStructureID:   forge,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 1},
	}
	declined := &sim.PayLedgerEntry{
		ID: 866, BuyerID: josiahID, SellerID: ezekielID,
		ItemKind: "nail", Qty: 5, Amount: 1,
		PayItems:   []sim.ItemKindQty{{Kind: "carrots", Qty: 6}},
		State:      sim.PayLedgerStateDeclined,
		ResolvedAt: published.Add(-30 * time.Second),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, ezekielID: ezekiel},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{866: declined},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles:          map[sim.HuddleID]*sim.Huddle{},
		Structures:       map[sim.StructureID]*sim.Structure{forge: plainStructure(forge, "Blacksmith")},
	}
	return snap, josiahID, nil
}

// offereeShortOfAskedGoodPending is the LLM-303 seller-POV fixture: Elizabeth
// Reade, who is NOT a nail vendor and holds ZERO nails, has a standing pay offer
// from Josiah Thorne — 1 sage for 5 nails to keep. Before the fix the seller-side
// stock warning fired only for a vendor already carrying some of the kind, so
// Elizabeth saw the bare offer and fabricated stock she never had. The golden pins
// that the offer line now carries "— you hold no nails" (plural noun from the
// catalog), the fact that grounds a decline/counter instead of a confabulated
// accept. Clock-free: the offer section reads no wall clock.
func offereeShortOfAskedGoodPending() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		elizabethID = sim.ActorID("elizabeth")
		josiahID    = sim.ActorID("josiah")
		store       = sim.StructureID("general_store")
	)
	now := 1220 // 20:20, the live window
	published := time.Date(2026, 7, 6, 20, 20, 0, 0, time.UTC)
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Reade",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: store,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"sage": 2}, // holds no nails
		Acquaintances:     map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Josiah Thorne",
		Role:              "traveler",
		State:             sim.StateIdle,
		InsideStructureID: store,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"sage": 3},
	}
	pending := &sim.PayLedgerEntry{
		ID: 871, BuyerID: josiahID, SellerID: elizabethID,
		ItemKind: "nail", Qty: 5, Amount: 0,
		PayItems:  []sim.ItemKindQty{{Kind: "sage", Qty: 1}},
		State:     sim.PayLedgerStatePending,
		ExpiresAt: published.Add(3 * time.Minute),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{elizabethID: elizabeth, josiahID: josiah},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{871: pending},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles:          map[sim.HuddleID]*sim.Huddle{},
		Structures:       map[sim.StructureID]*sim.Structure{store: plainStructure(store, "General Store")},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails", Category: "material"},
			"sage": {Name: "sage", DisplayLabelSingular: "sage", DisplayLabelPlural: "sage", Category: "material"},
		},
	}
	return snap, elizabethID, nil
}

// sellerWithTakenQuoteAtPost is the LLM-189 perception regression fixture: a
// stateful vendor who just sold a lot (quote flipped to SceneQuoteStateTaken)
// while another lot stays active. The golden proves the taken lot drops out of
// "## Offers you've put out" and only the active lot renders.
func sellerWithTakenQuoteAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		prudenceID = sim.ActorID("prudence")
		anneID     = sim.ActorID("anne")
		apothecary = sim.StructureID("apothecary")
	)
	now := 600 // 10:00
	active := &sim.SceneQuote{
		ID: 1, SellerID: prudenceID, TargetBuyer: anneID,
		Lines: []sim.QuoteLine{{ItemKind: "raspberries", Qty: 5}}, Amount: 10,
		State: sim.SceneQuoteStateActive,
	}
	taken := &sim.SceneQuote{
		ID: 2, SellerID: prudenceID, TargetBuyer: anneID,
		Lines: []sim.QuoteLine{{ItemKind: "blueberries", Qty: 5}}, Amount: 10,
		State: sim.SceneQuoteStateTaken,
	}
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "apothecary",
		State:             sim.StateIdle,
		InsideStructureID: apothecary,
		WorkStructureID:   apothecary,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		// Holds the raspberries the active lot advertises (0 blueberries — those
		// were sold on the taken lot), so the fixture satisfies the LLM-409
		// within-on-hand invariant.
		Inventory:     map[sim.ItemKind]int{"raspberries": 5},
		Acquaintances: map[string]sim.Acquaintance{"Anne": {}},
	}
	anne := &sim.ActorSnapshot{
		Kind: sim.KindNPCShared, DisplayName: "Anne", Role: "traveler", Needs: map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{prudenceID: prudence, anneID: anne},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{1: active, 2: taken},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles:          map[sim.HuddleID]*sim.Huddle{},
		Structures:       map[sim.StructureID]*sim.Structure{apothecary: plainStructure(apothecary, "PW Apothecary")},
	}
	return snap, prudenceID, nil
}

// buyerSeesStandingQuoteAfterWarrantSpent is the LLM-551 buyer-side fixture, cut
// from the live General Store deadlock of 2026-07-28. Patience Walker is the
// SUBJECT — the buyer — and Josiah Thorne's 4-flour quote in her name is active.
//
// The load-bearing detail is what is ABSENT: warrants is nil. The quote's
// one-shot SceneQuoteTargetedWarrant fired turns ago and was consumed, which
// live is the ordinary case (a buyer who answers the offer aloud spends her tick
// on a terminal speak). Every later tick she perceived nothing about an offer
// that was hers to take, so her pay_with_item calls went out bare — no quote_id —
// and crossed the quote instead of taking it. The golden proves the section
// stands the offer up on exactly such a tick.
//
// She holds 0 flour and 28 coins, as she did live.
func buyerSeesStandingQuoteAfterWarrantSpent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID   = sim.ActorID("patience")
		josiahID     = sim.ActorID("josiah")
		generalStore = sim.StructureID("general_store")
		huddle       = sim.HuddleID("h1")
	)
	now := 916 // 15:16, the hour the live scene ran
	standing := &sim.SceneQuote{
		ID: 5, SellerID: josiahID, TargetBuyer: patienceID,
		Lines: []sim.QuoteLine{{ItemKind: "flour", Qty: 4}}, Amount: 12,
		State: sim.SceneQuoteStateActive,
	}
	patience := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Patience Walker",
		Role:              "goodwife",
		State:             sim.StateIdle,
		InsideStructureID: generalStore,
		CurrentHuddleID:   huddle,
		Coins:             28,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: generalStore,
		WorkStructureID:   generalStore,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"flour": 9},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience, josiahID: josiah},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{5: standing},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{patienceID: {}, josiahID: {}}},
		},
		Structures: map[sim.StructureID]*sim.Structure{generalStore: plainStructure(generalStore, "General Store")},
	}
	return snap, patienceID, nil
}

// sellerWithShortfallLotAtPost builds the LLM-409 fixture: Josiah Thorne, a
// farmer at his post, offered John Ellis two sheaves of wheat and then spent the
// wheat out from under his own lot (the pre-publish coverage reconcile flipped
// that lot to SceneQuoteStateShortfall), while a separate turnip lot he can still
// cover stays active. The golden proves the shortfall lot is GONE from "## Offers
// you've put out" (it no longer advertises or pins him to the uncoverable
// promise) and instead surfaces as the flat "## An offer you couldn't keep" beat,
// while the coverable turnip lot still stands. Reverting the reconcile would make
// the wheat lot reappear under the standing-offers header — the exact stale
// advertisement + "bide, do not re-post" absorbing state this ticket closes.
func sellerWithShortfallLotAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		johnID   = sim.ActorID("john")
		farm     = sim.StructureID("farm")
	)
	now := 600 // 10:00
	published := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC).Add(time.Duration(now) * time.Minute)
	// Coverable: he holds 3 turnips and offers John 3 turnips — stays active.
	active := &sim.SceneQuote{
		ID: 1, SellerID: josiahID, TargetBuyer: johnID,
		Lines: []sim.QuoteLine{{ItemKind: "turnip", Qty: 3}}, Amount: 3,
		State: sim.SceneQuoteStateActive,
	}
	// Shortfall: he offered John 2 wheat, then paid his last wheat away — 0 held.
	// ResolvedAt sits inside the beat's recentlyResolvedOfferWindow.
	shortfall := &sim.SceneQuote{
		ID: 2, SellerID: josiahID, TargetBuyer: johnID,
		Lines: []sim.QuoteLine{{ItemKind: "wheat", Qty: 2}}, Amount: 2,
		State:      sim.SceneQuoteStateShortfall,
		ResolvedAt: published.Add(-1 * time.Minute),
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: farm,
		WorkStructureID:   farm,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"turnip": 3},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
	}
	john := &sim.ActorSnapshot{
		Kind: sim.KindNPCShared, DisplayName: "John Ellis", Role: "traveler", Needs: map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, johnID: john},
		Quotes:           map[sim.QuoteID]*sim.SceneQuote{1: active, 2: shortfall},
		PayLedger:        map[sim.LedgerID]*sim.PayLedgerEntry{},
		Scenes:           map[sim.SceneID]*sim.Scene{},
		Huddles:          map[sim.HuddleID]*sim.Huddle{},
		Structures:       map[sim.StructureID]*sim.Structure{farm: plainStructure(farm, "Thorne Farm")},
	}
	return snap, josiahID, nil
}

// lodgerGoldenBase builds the shared LLM-127 lodging-gate fixture: Ezekiel Crane,
// a renewal-due lodger of the Tavern (room 2, expiring 8h out — inside the 13h
// renewal window), 0 coins, scheduled 06:00–18:00. The caller positions him
// (inside) and sets the local clock (nowMin) to drive the on-shift gate, and may
// add an awake huddle companion. Renewal-due is computed off PublishedAt, so the
// rendered cue is deterministic; nowMin only moves the shift gate.
func lodgerGoldenBase(inside sim.StructureID, nowMin int, withCompanion bool) (*sim.Snapshot, sim.ActorID) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		patronID  = sim.ActorID("patron")
		tavern    = sim.StructureID("tavern")
		market    = sim.StructureID("market")
		huddleID  = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00–18:00
	published := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC).Add(time.Duration(nowMin) * time.Minute)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: inside,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		RoomAccess: map[sim.RoomAccessKey]*sim.RoomAccess{
			{RoomID: 2, Source: sim.AccessSourceLedger}: {
				RoomID:    2,
				Source:    sim.AccessSourceLedger,
				LedgerID:  1,
				ExpiresAt: ptrTime(published.Add(8 * time.Hour)),
				Active:    true,
			},
		},
	}
	actors := map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel}
	huddles := map[sim.HuddleID]*sim.Huddle{}
	if withCompanion {
		ezekiel.CurrentHuddleID = huddleID
		actors[patronID] = &sim.ActorSnapshot{
			Kind:              sim.KindNPCStateful,
			DisplayName:       "Goodwife Hale",
			Role:              "patron",
			State:             sim.StateIdle,
			InsideStructureID: inside,
			CurrentHuddleID:   huddleID,
			Needs:             map[sim.NeedKey]int{},
		}
		huddles[huddleID] = &sim.Huddle{ID: huddleID, Members: map[sim.ActorID]struct{}{ezekielID: {}, patronID: {}}}
	}
	nm := nowMin
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &nm,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           actors,
		Huddles:          huddles,
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: innStructure(tavern, "Tavern"),
			market: plainStructure(market, "Market"),
		},
		LodgingDefaultWeeklyRate: 14, // nightly 2
		LodgingBedtimeMinute:     22 * 60,
		LodgingCheckOutMinute:    11 * 60,
	}
	return snap, ezekielID
}

func lodgerRenewalDueInConversation() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id := lodgerGoldenBase("market", 12*60, true) // on-shift, awake huddle companion
	return snap, id, nil
}

func lodgerRenewalDueOnShiftAway() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id := lodgerGoldenBase("market", 12*60, false) // on-shift, away from inn, alone
	return snap, id, nil
}

func lodgerRenewalDueOffShift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id := lodgerGoldenBase("market", 20*60, false) // off-shift, away from inn, alone
	return snap, id, nil
}

// lodgerRenewalDueDeskRememberedShut is the LLM-126 Step-B surface: the same off-shift,
// away-from-inn, alone lodger as the positive baseline (so the walk-pull is actionable),
// but he went to the Tavern within the decay window and found the keeper's desk shut.
// The experiential wait-steer replaces the retired omniscient "keeper is abed" read; the
// memory is stamped relative to PublishedAt so it decays on the 4h closed-business TTL.
func lodgerRenewalDueDeskRememberedShut() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id := lodgerGoldenBase("market", 20*60, false)
	snap.Actors[id].Observed = sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
		{StructureID: "tavern", Condition: sim.ObservedClosed}: snap.PublishedAt.Add(-time.Hour),
	})
	return snap, id, nil
}

// buyerRemembersVendorShut is the LLM-126 Step-A surface: a hungry forager (Ezekiel)
// stands near a cheese seller at the General Store, but he went there within the decay
// window and found it shut — no keeper tending it (now including an abed keeper, since
// the capture gates on availability). The golden pins the "## What you can eat or drink"
// buy cue carrying the experiential "found it shut up" annotation — the only path to a
// closed cue now that the omniscient "(currently closed)" marker is retired. The seller
// is present and awake; the cue is driven by his memory, not her state. No orders, fixed
// PublishedAt (the observation is stamped relative to it) → byte-stable.
func buyerRemembersVendorShut() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
	)
	now := 600 // 10:00 — daytime
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Ezekiel Crane",
		Role:        "forager",
		State:       sim.StateIdle,
		Pos:         sim.WorldPos{X: 0, Y: 0}.Tile(),
		Coins:       6,
		Needs:       map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		// He went to the store within the decay window and found it shut.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: store, Condition: sim.ObservedClosed}: published.Add(-time.Hour),
		}),
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"cheese": {
				Name:         "cheese",
				Capabilities: []string{"portable"}, DisplayLabel: "Cheese",
				DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}},
			},
		},
	}
	return snap, ezekielID, nil
}

// cheeseKind is the shared eat-here cheese ItemKindDef the LLM-222 buy-cue
// scenarios sell — a good meal for hunger, matching buyerRemembersVendorShut.
func cheeseKind() *sim.ItemKindDef {
	return &sim.ItemKindDef{
		Name: "cheese", DisplayLabel: "Cheese",
		DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese",
		Category:  sim.ItemCategoryFood,
		Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}},
	}
}

// buyerDropsShutKeepsOpenVendor — LLM-222. A hungry forager (6 coins) can buy
// cheese at two shops: the General Store he remembers finding shut, and the open
// Tavern he has no shut memory of. The buy cue drops the shut store and keeps the
// open Tavern — the surgical eat/drink analogue of the restock
// keeper_restock_drops_shut_keeps_open_supplier golden.
func buyerDropsShutKeepsOpenVendor() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		johnID    = sim.ActorID("john")
		store     = sim.StructureID("general_store")
		tavern    = sim.StructureID("tavern")
	)
	now := 600 // 10:00 — daytime
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Ezekiel Crane",
		Role:        "forager",
		State:       sim.StateIdle,
		Pos:         sim.WorldPos{X: 0, Y: 0}.Tile(),
		Coins:       6,
		Needs:       map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		// He found the General Store shut within the decay window; the Tavern he has
		// no shut memory of.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: store, Condition: sim.ObservedClosed}: published.Add(-time.Hour),
		}),
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			store:  plainStructure(store, "General Store"),
			tavern: plainStructure(tavern, "The Tavern"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{"cheese": cheeseKind()},
	}
	return snap, ezekielID, nil
}

// brokeBuyerWithGoodsBartersForFood — LLM-222 means-to-pay "barter" state. A
// hungry forager with 0 coins but a pelt to trade stands near an open cheese
// seller: the buy cue is kept but steered to a goods offer, because barter is a
// viable path a coins==0 suppression would have hidden.
func brokeBuyerWithGoodsBartersForFood() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
	)
	now := 600 // 10:00 — daytime
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Ezekiel Crane",
		Role:        "forager",
		State:       sim.StateIdle,
		Pos:         sim.WorldPos{X: 0, Y: 0}.Tile(),
		Coins:       0,
		// A non-food trade good he can put up in barter, but no coins. Non-food so it
		// doesn't add an own-stock "consume to eat" cue.
		Inventory: map[sim.ItemKind]int{"pelt": 1},
		Needs:     map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"cheese": cheeseKind(),
			"pelt":   {Name: "pelt", DisplayLabel: "Pelt", DisplayLabelSingular: "pelt"},
		},
	}
	return snap, ezekielID, nil
}

// brokeBuyerNoGoodsNoBuyCue — LLM-222 means-to-pay suppression. The same broke
// forager, now with nothing to trade: no coins and no goods is the one genuine
// payment dead-end, so the buy cue is dropped entirely and (no free source or own
// stock nearby) the whole eat/drink section is omitted.
func brokeBuyerNoGoodsNoBuyCue() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
	)
	now := 600 // 10:00 — daytime
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Ezekiel Crane",
		Role:        "forager",
		State:       sim.StateIdle,
		Pos:         sim.WorldPos{X: 0, Y: 0}.Tile(),
		Coins:       0,
		Needs:       map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{"cheese": cheeseKind()},
	}
	return snap, ezekielID, nil
}

// brokeBuyerNoGoodsNoPeerBuy — LLM-242 means-to-pay suppression, the co-present
// peer arm (sibling of brokeBuyerNoGoodsNoBuyCue's vendor arm). The same broke
// forager (0 coins, nothing to trade) stands in a huddle with a co-present peer
// (Lewis) carrying bread he'd otherwise be told to buy with pay_with_item. No
// coins and no goods is no means to pay, so the peer offer is dropped; with no
// free source or own stock nearby the whole "## What you can eat or drink"
// section is omitted. (Lewis carries bread, not stew, so the vacuity tripwire's
// holdsBarterableGoods read of him survives LLM-445 — eat-here food no longer
// counts as barterable goods.) Contrast peers_holding_same_food, where the
// subject holds food of its own and so keeps a means to pay.
func brokeBuyerNoGoodsNoPeerBuy() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		lewisID   = sim.ActorID("lewis")
		commons   = sim.StructureID("commons")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "forager",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"bread": 1},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, lewisID: {}}},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, ezekielID, nil
}

// TestTradeCueOnlyForIdleProducerAtPost is the LLM-116/LLM-319 cross-scenario
// invariant, refined by LLM-324: the "## Your trade" cue appears in EXACTLY the
// scenarios whose subject is (a) physically inside its own work structure, (b) a
// producer, (c) IDLE with nothing in the works (ProductionItem empty; mid-batch the
// cue and its produce tool yield to the standing in-progress line), AND (d) has at
// least one good it could start a batch of right now — recipe-backed AND a whole
// batch fits under the carry cap AND the inputs are on hand (the craftableNow gate).
// A producer with every good at cap or input-starved gets NO cue, so the produce
// tool the cue gates is not advertised (LLM-324). The predicate is re-derived from
// the fixture here rather than hardcoded per name, so a new scenario can't silently
// join or leave the cue set.
func TestTradeCueOnlyForIdleProducerAtPost(t *testing.T) {
	const marker = "## Your trade"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			want := false
			if a != nil && a.RestockPolicy != nil &&
				a.WorkStructureID != "" && a.InsideStructureID == a.WorkStructureID &&
				a.ProductionItem == "" {
				for _, e := range a.RestockPolicy.ProduceEntries() {
					r := snap.Recipes[e.Item]
					if r == nil || r.RateQty <= 0 || r.RatePerHours <= 0 {
						continue // not makeable
					}
					batchQty := r.OutputQty
					if batchQty < 1 {
						batchQty = 1
					}
					fitsCap := e.Cap() <= 0 || a.Inventory[e.Item]+batchQty <= e.Cap()
					if fitsCap && sim.HasProduceInputs(r, a.Inventory) {
						want = true // ≥1 craftable good — a producer with a real decision to grant (LLM-324)
						break
					}
				}
			}
			if has := strings.Contains(renderScenario(sc), marker); has != want {
				t.Errorf("scenario %q: trade cue present=%v, want %v (idle-craftable-producer-at-post predicate)", sc.name, has, want)
			}
		})
	}
}

// TestGoldensNoInputGoodAlwaysCraftable is the LLM-257 cross-scenario invariant:
// in any scenario's trade-scene view, a good whose recipe has NO inputs must never
// be flagged not-craftable. An origin producer (nail, water) makes its good from
// nothing, so HasProduceInputs is always satisfied for it — the "always makeable"
// property the inputs-aware gate must preserve. Guards against a future change that
// mistakes a no-input recipe for a starved one, which would hang the "you'd need
// more … first" clause on a good the keeper can in fact batch at will (LLM-319).
// White-box over buildForgeChoice so it reads the structured InputsReady flag
// rather than parsing rendered text.
func TestGoldensNoInputGoodAlwaysCraftable(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			as := snap.Actors[actorID]
			if as == nil {
				return
			}
			view := buildForgeChoice(snap, actorID, as)
			if view == nil {
				return // not an idle producer at its post — no trade scene here (LLM-319)
			}
			for _, it := range view.Items {
				recipe := snap.Recipes[it.itemKind]
				if recipe == nil {
					continue
				}
				hasInput := false
				for _, in := range recipe.Inputs {
					if in.Qty > 0 {
						hasInput = true
						break
					}
				}
				if !hasInput && !it.InputsReady {
					t.Errorf("scenario %q: no-input good %q flagged not-craftable; an origin recipe is always makeable (LLM-257)", sc.name, it.itemKind)
				}
			}
		})
	}
}

// TestGoldensTradeCueOnlyCraftableGoods is the LLM-324 cross-scenario invariant:
// every good the "## Your trade" cue lists must be craftable right now — a whole
// batch fits under its carry cap AND its inputs are on hand. The cue's presence is
// what advertises the produce tool (gateTools offerCraft reads the same Items), so a
// listed-but-uncraftable good is exactly the self-contradiction that steered a
// keeper into an at-cap / input-starved produce-reject loop (live: Moses James,
// 6-iteration budget_forced ticks). Runs over the whole matrix so no future scenario
// can reintroduce a good the cue offers but StartProductionCycle would reject.
//
// The cap check RECOMPUTES batchFitsCap from the fixture (entry cap + recipe batch +
// on-hand) rather than trusting it.Stock != StockFull: stockTier reports StockEmpty
// (not Full) at zero on hand, so a batch larger than the whole cap (batchQty > cap)
// would slip past a tier-based assertion — the exact blind spot code_review caught
// in the first cut of the production gate.
func TestGoldensTradeCueOnlyCraftableGoods(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			as := snap.Actors[actorID]
			if as == nil {
				return
			}
			view := buildForgeChoice(snap, actorID, as)
			if view == nil {
				return // no trade cue here — nothing to constrain
			}
			for _, it := range view.Items {
				r := snap.Recipes[it.itemKind]
				if r == nil {
					t.Errorf("scenario %q: trade cue lists %q with no recipe", sc.name, it.itemKind)
					continue
				}
				batchQty := r.OutputQty
				if batchQty < 1 {
					batchQty = 1
				}
				cap, found := 0, false
				for _, e := range as.RestockPolicy.ProduceEntries() {
					if e.Item == it.itemKind {
						cap, found = e.Cap(), true
						break
					}
				}
				if !found {
					t.Errorf("scenario %q: trade cue lists %q that isn't in the actor's produce policy", sc.name, it.itemKind)
					continue
				}
				if fitsCap := cap <= 0 || as.Inventory[it.itemKind]+batchQty <= cap; !fitsCap {
					t.Errorf("scenario %q: trade cue lists %q but a whole batch overshoots the carry cap — the produce tool would reject it (LLM-324)", sc.name, it.itemKind)
				}
				if !sim.HasProduceInputs(r, as.Inventory) {
					t.Errorf("scenario %q: trade cue lists %q with inputs short — the produce tool would reject it (LLM-324)", sc.name, it.itemKind)
				}
			}
		})
	}
}

// TestForgeChoiceDropsBatchLargerThanCap is the LLM-324 edge case code_review
// flagged: a good with ZERO on hand but a batch size that exceeds the whole carry
// cap is NOT craftable (batchFitsCap: 0+batch > cap → the landed batch would
// overshoot), yet stockTier reads it StockEmpty via its onHand<=0 short-circuit. A
// StockFull-proxy gate would therefore wrongly list it and advertise produce;
// buildForgeChoice must gate on the exact cap predicate and drop it. The cap-5
// positive control proves the drop is the cap predicate's doing, not some other gate.
func TestForgeChoiceDropsBatchLargerThanCap(t *testing.T) {
	const (
		bakerID = sim.ActorID("baker")
		bakery  = sim.StructureID("bakery")
	)
	build := func(cap int) (*sim.Snapshot, sim.ActorID, *sim.ActorSnapshot) {
		as := &sim.ActorSnapshot{
			WorkStructureID:   bakery,
			InsideStructureID: bakery,
			Inventory:         map[sim.ItemKind]int{}, // zero on hand
			RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
				{Item: "loaf", Source: sim.RestockSourceProduce, Max: cap},
			}},
		}
		snap := &sim.Snapshot{
			Actors:     map[sim.ActorID]*sim.ActorSnapshot{bakerID: as},
			Structures: map[sim.StructureID]*sim.Structure{bakery: plainStructure(bakery, "Bakery")},
			Recipes: map[sim.ItemKind]*sim.ItemRecipe{
				"loaf": {OutputItem: "loaf", OutputQty: 5, RateQty: 5, RatePerHours: 1}, // batch of 5
			},
		}
		return snap, bakerID, as
	}
	// Batch of 5 from empty overshoots cap 3 → not craftable → cue suppressed.
	snap, id, as := build(3)
	if view := buildForgeChoice(snap, id, as); view != nil {
		t.Errorf("cap 3: buildForgeChoice = %+v, want nil (a 5-loaf batch overshoots cap 3 from empty)", view)
	}
	// Positive control: cap 5 fits the whole batch → cue lists loaf.
	snap, id, as = build(5)
	if view := buildForgeChoice(snap, id, as); view == nil {
		t.Fatal("cap 5: buildForgeChoice = nil, want a cue (a 5-loaf batch fits cap 5 from empty)")
	}
}

// TestGoldensDegradedWordingMatchesProducePct is the LLM-446 cross-scenario
// invariant: wherever the SUBJECT owns a degraded business, the "## Your
// business" cue's framing must match the production mode the engine actually
// runs — the full-block wording ("or make more until you mend it") renders IFF
// StallDegradedProducePct is 0, and the slowed wording ("work goes on, though
// slowly") IFF it is positive. Runs over the whole matrix so a future cue edit
// can't tell a producing keeper his shop is shut (the frozen-clock lie behind
// the live "three more minutes, Josiah" loop) or a blocked one that work
// continues.
func TestGoldensDegradedWordingMatchesProducePct(t *testing.T) {
	const blockedMarker = "or make more until you mend it"
	const slowedMarker = "work goes on, though slowly"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			stall := sim.OwnedWearableStall(snap.VillageObjects, actorID)
			if !sim.StallDegraded(stall, snap.StallWearDegradeThreshold) {
				return // subject doesn't own a degraded business — invariant N/A here
			}
			out := renderScenario(sc)
			wantBlocked := snap.StallDegradedProducePct <= 0
			if got := strings.Contains(out, blockedMarker); got != wantBlocked {
				t.Errorf("scenario %q: full-block wording present=%v, want %v (StallDegradedProducePct=%d)", sc.name, got, wantBlocked, snap.StallDegradedProducePct)
			}
			if got := strings.Contains(out, slowedMarker); got == wantBlocked {
				t.Errorf("scenario %q: slowed wording present=%v, want %v (StallDegradedProducePct=%d)", sc.name, got, !wantBlocked, snap.StallDegradedProducePct)
			}
		})
	}
}

// TestInFlightProductionLineTracksBatch is the LLM-319 cross-scenario invariant:
// the standing "You are making a batch of X" self-state line renders EXACTLY when
// the subject has a production cycle in flight (ProductionItem non-empty) —
// WHEREVER the actor is. Unlike the retired focus line (LLM-121 gated it to the
// work structure), the batch is a fact everywhere: its inputs are already spent,
// and the line's own "it only moves along while you're at your post" tail carries
// the pause semantics, so hiding it off-post would misstate the situation (the
// actor could no longer answer "what are you making?" — or remember to go back).
// Re-derived from the fixture rather than hardcoded per name.
func TestInFlightProductionLineTracksBatch(t *testing.T) {
	const marker = "You are making a batch of"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			item := sim.ItemKind("")
			if a != nil {
				item = a.ProductionItem
			}
			want := item != ""
			if has := strings.Contains(renderScenario(sc), marker); has != want {
				t.Errorf("scenario %q: in-flight batch line present=%v, want %v (ProductionItem=%q)", sc.name, has, want, item)
			}
		})
	}
}

// TestGoldensBoosterLineOnlyForBoostedRecipes is the LLM-248 cross-scenario
// invariant: the elective-booster line ("adds N extra to the yield") renders in
// EXACTLY the scenarios whose subject produces a good whose recipe defines a
// booster that is a low bought entry (dairy_keeper_out_of_booster_at_post) and
// nowhere else in the matrix. A booster line leaking into a scenario with no
// boosted recipe would mean the gate regressed to reading required inputs;
// required-input scenarios keep their runway phrasing and never the bonus one.
// TestGoldensHearthCookingLineOnlyForHearthCooks is the LLM-474 cross-scenario
// invariant: the fire-and-cooking section renders in EXACTLY the scenarios whose
// subject stands in a hearth-bearing kitchen making a hearth-boosted dish, and
// nowhere else in the matrix. The leak this guards against is the cue escaping
// to the many keepers who cook nothing (or cook somewhere with no fireplace) —
// which would put a fire they have no stake in, and cannot act on, into their
// scene. The hearthless fixture is deliberately in the matrix as the near-miss.
func TestGoldensHearthCookingLineOnlyForHearthCooks(t *testing.T) {
	const marker = "## The fire and your cooking"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "cook_at_burning_hearth" ||
			sc.name == "cook_at_ebbing_hearth" ||
			sc.name == "cook_at_dead_hearth_no_wood"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: hearth-cooking line present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensHearthCookingNeverForbidsCooking is the rendered half of the
// never-gates rule (its sim-side twin is TestBoostStateNeverGatesProduction).
// The cooking cue states a stake and must never tip into telling a keeper she
// CANNOT cook — food is the survival good, and a scene that says otherwise
// teaches the model the exact false rule this ticket exists to unteach (the
// Hannah confabulation, live 2026-07-18).
func TestGoldensHearthCookingNeverForbidsCooking(t *testing.T) {
	forbidding := []string{
		"cannot cook", "can't cook", "no cooking", "unable to cook",
		"cannot make", "can't make", "until the fire", "before you can cook",
	}
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		if !strings.Contains(got, "## The fire and your cooking") {
			continue
		}
		for _, phrase := range forbidding {
			if strings.Contains(strings.ToLower(got), phrase) {
				t.Errorf("scenario %q: cooking cue reads as a prohibition (%q) — a cold hearth never blocks cooking", sc.name, phrase)
			}
		}
	}
}

func TestGoldensBoosterLineOnlyForBoostedRecipes(t *testing.T) {
	const marker = "extra to the yield"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		// The exact boosted-scenario set: the LLM-248 dairy sage fixture, the
		// LLM-442 smith with the iron booster low and buyable, and the LLM-444
		// cook with the salt booster low and buyable.
		want := sc.name == "dairy_keeper_out_of_booster_at_post" ||
			sc.name == "smith_out_of_iron_vendor_stocked" ||
			sc.name == "cook_out_of_salt_vendor_stocked"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: booster line present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensToolWearLineOnlyForDurableTools is the LLM-330 cross-scenario
// invariant: the wear-phrased production-input line ("is wearing down") renders
// in EXACTLY the scenarios whose subject produces with a low durable-tool input
// (keeper_worn_skillet_wear_runway) and nowhere else in the matrix. A wear line
// leaking elsewhere would mean the Tool flag regressed to firing on plain
// consumed inputs; consumed-input scenarios keep their "You use X to make Y"
// runway phrasing and never the wear one.
func TestGoldensToolWearLineOnlyForDurableTools(t *testing.T) {
	const marker = "is wearing down"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "keeper_worn_skillet_wear_runway"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: tool-wear line present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestActiveWorkerCueOnlyForEmployerWithWorkingOffer is the LLM-202 cross-scenario
// invariant: the employer-side "X is working a job for you" cue (renderWorkersForMe)
// renders in EXACTLY the scenario where the subject is an employer with a worker
// mid-contract (a Working offer where EmployerID == subject). It must NOT appear for
// an employer whose only labor offer is Pending (broke_employer_cannot_pay_labor_offer
// — that renders in "## Work offers awaiting your decision", not as an active worker),
// nor anywhere else in the matrix. The marker is distinct from the worker's own
// "You are working a job for X" self-state line (renderLaborSelfState), which is
// second-person and never carries "is working a job for you".
func TestActiveWorkerCueOnlyForEmployerWithWorkingOffer(t *testing.T) {
	const marker = "is working a job for you"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		// LLM-231: seller_employing_own_laboring_worker also puts the subject in the
		// employer seat of a Working offer (John employs Silence), so the cue is correct there.
		// LLM-621: the miller is likewise in the employer seat of a Working offer —
		// that cue and the surviving forage cue in one prompt IS the scenario's point.
		want := sc.name == "employer_with_worker_on_job" ||
			sc.name == "seller_employing_own_laboring_worker" ||
			sc.name == "miller_with_hired_hauler_keeps_water_cue" ||
			// LLM-622: the owned-source arm of the same miller fixture — same hire.
			sc.name == "miller_stepping_out_to_his_own_bush" ||
			// LLM-651: the miller employing the wright on an odd job while the mill
			// is due — the contract line is what the service pay arm yields to.
			sc.name == "owner_with_wright_on_the_job"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: active-worker cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestWaresWorthCueOnlyInCompanyWithOwnTrade is the LLM-125 / LLM-191 cross-scenario
// invariant: the "## What your wares fetch" cue appears in EXACTLY the scenarios where
// the actor is in company (a huddle) AND has priced own wares — produced
// (smith_bartering_at_tavern) or resold (keeper_reselling_in_company, LLM-191). An
// actor alone — even at its forge with recipes — or one in company but without its
// own priced wares must never see it: the own-wares base price stays out of solo and
// no-own-trade turns, and is gated on company rather than location (unlike the forge
// cue).
func TestWaresWorthCueOnlyInCompanyWithOwnTrade(t *testing.T) {
	const marker = "## What your wares fetch"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "smith_bartering_at_tavern" || sc.name == "keeper_reselling_in_company" ||
			sc.name == "innkeeper_pricing_with_makings_cost" || // LLM-226: producer in company, priced own ware
			sc.name == "employer_recalls_returning_helper" || // LLM-228: producing keeper in company (incidental to the recall it tests)
			sc.name == "wholesaler_producer_bartering_with_customer" || // LLM-291: wholesale producer in company — cue draws the wholesale-channel line
			sc.name == "wholesaler_producer_observed_rates" || // LLM-295: same, with observed rates on both wholesale-line figures
			sc.name == "owner_holding_repair_nails_in_company" || // LLM-292: keeper in company with priced ware + the repair-reserve earmark
			sc.name == "smith_holding_forge_water_in_company" || // LLM-609: producer in company with a priced ware + a bench-reserved input
			sc.name == "coin_poor_overstocked_keeper_conserves" || // LLM-294: producer in company (priced own wares) + the conserve sell-first nudge
			sc.name == "distributor_underwater_resale" || // LLM-332: reseller in company with priced resale wares, one underwater
			sc.name == "innkeeper_fried_meat_below_makings_cost" || // LLM-475: producer in company, priced own ware sold under its makings cost
			sc.name == "miller_flour_wholesale_earns_nothing" || // LLM-475: wholesale producer in company — cost sentence restored to the channel line
			sc.name == "miller_offered_parity_swap_for_flour" || // LLM-598: wholesale producer in company weighing a barter offer — the cue's figures are what the offer verdict must agree with
			sc.name == "keeper_paid_with_long_note" || // LLM-400: built ON keeper_reselling_in_company's fixture, so it inherits the cue correctly
			sc.name == "broke_maker_at_store_no_spare_goods_hold_off" || // LLM-636: producer in company (with Josiah at the store), priced own wares + spoken-for makings held back
			sc.name == "keeper_prices_bale_leftover_iron" // LLM-646: keeper in company holding off-policy priced stock — held goods ARE own trade now
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: wares-worth cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestWholesaleChannelLineOnlyForWholesalerProduce is the LLM-291 cross-scenario
// invariant: the wholesale-channel wares line ("your own produce — it sells in bulk
// to …") appears in EXACTLY the scenarios where a wholesale producer prices its own
// produce in company (the LLM-291 seed-copy scenario and the LLM-295 observed-rate
// scenario). It guards both directions — a wholesaler's own produce must never
// regress to a retail spread (the framing that invited Moses's refused street sale,
// live hud-9b23…), and no ordinary retail producer (the smith, the innkeeper) may
// ever pick up the wholesale line.
func TestWholesaleChannelLineOnlyForWholesalerProduce(t *testing.T) {
	const marker = "your own produce — it sells in bulk to"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "wholesaler_producer_bartering_with_customer" ||
			sc.name == "wholesaler_producer_observed_rates" ||
			sc.name == "miller_flour_wholesale_earns_nothing" || // LLM-475: the same line, now carrying the maker's own cost
			sc.name == "miller_offered_parity_swap_for_flour" // LLM-598: the same miller, weighing a barter offer against that line
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: wholesale-channel wares line present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensMakingsVerdictAlwaysHasACostFigure is the LLM-475 cross-scenario
// invariant: the margin verdict is a judgment ON a stated cost, so it may never
// appear on a wares line that does not also state what the makings cost. A verdict
// without its figure would be an unexplained accusation — the model would be told
// it is losing coin with no number to negotiate off, which is precisely the
// data-free position the miller was in when he traded five sacks for five sheaves.
// Guards both phrasings of the cost sentence (the retail "the makings run you …"
// and the wholesale-channel "it costs you … to produce").
func TestGoldensMakingsVerdictAlwaysHasACostFigure(t *testing.T) {
	verdicts := []string{"(a loss)", "(it earns you nothing)"}
	for _, sc := range perceptionScenarios {
		sc := sc
		for _, line := range strings.Split(renderScenario(sc), "\n") {
			carries := false
			for _, v := range verdicts {
				if strings.Contains(line, v) {
					carries = true
				}
			}
			if !carries {
				continue
			}
			if !strings.Contains(line, "the makings run you") && !strings.Contains(line, "it costs you") {
				t.Errorf("scenario %q: margin verdict with no cost figure on the line:\n%s", sc.name, line)
			}
		}
	}
}

// TestRenderedPromptsNeverSayDistributor is the LLM-292 copy-constraint invariant
// (Jeff, 2026-07-06): rendered prose must never hand the NPC's LLM a mechanic-role
// term — "distributor" is an engine/tag concept, and the NPC is told who stocks its
// goods in in-world relational terms ("whose shop stocks it for the village", "the
// village storekeeper"), never what engine role that actor holds. Runs the whole
// matrix so no future cue — or a fixture Role string leaking through a peer label —
// can reintroduce it.
func TestRenderedPromptsNeverSayDistributor(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if got := renderScenario(sc); strings.Contains(strings.ToLower(got), "distributor") {
				t.Errorf("scenario %q: rendered prompt contains the mechanic-role term %q (LLM-292 copy constraint)", sc.name, "distributor")
			}
		})
	}
}

// TestRepairReserveLineOnlyForOwnerWithMendAndNails is the LLM-292 cross-scenario
// invariant: the earmarked repair-nails line ("… of these to mend your …") appears
// in EXACTLY the scenario where a business OWNER with a live mend obligation holds
// nails in company (owner_holding_repair_nails_in_company). A hired mender, an
// owner carrying no nails, an owner out of company, or an unworn business must
// never pick it up — and the mend-nag scenarios that share its predicates
// (owner_at_worn_stall & co) stay earmark-free because they render out of company.
func TestRepairReserveLineOnlyForOwnerWithMendAndNails(t *testing.T) {
	const marker = "of these to mend your"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "owner_holding_repair_nails_in_company"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: repair-reserve earmark line present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensNeverPriceAGoodTheBenchHasReserved is the LLM-609 cross-scenario
// invariant: no scene ever prices a good the actor's own live recipes have
// reserved. Re-derived from each fixture rather than trusting the build that
// produced the section — sim.ReorderFloors is read here exactly as buildTradeValue
// reads it, so a change that drops the discriminator from the item walk fails here
// even if it keeps the struct fields populated.
//
// Checks the structured view AND the rendered line, because each catches a
// different regression: a good silently dropped from Items renders no line at all,
// which a text-only assertion reads as a pass.
//
// The rendered half is scoped to the wares section deliberately — "## Your bushes
// to harvest" and "## Restocking" also open lines with "- <label>:", and those cues
// are about getting the good IN, which a reservation has nothing to say about.
func TestGoldensNeverPriceAGoodTheBenchHasReserved(t *testing.T) {
	checked := 0
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.RestockPolicy == nil {
				return
			}
			payload := Build(snap, actorID, warrants)
			if payload.TradeValue == nil {
				return
			}
			section := promptSection(combinedPrompt(Render(payload, DefaultRenderConfig())), "## What your wares fetch")
			for item, floor := range sim.ReorderFloors(snap.Recipes, a.RestockPolicy) {
				held := a.Inventory[item]
				// Above the floor the good keeps its price by design (the surplus IS
				// merchandise), and at zero on hand there is nothing to reserve.
				if floor <= 0 || held <= 0 || held > floor {
					continue
				}
				// A good the mend has already claimed (LLM-292) is deliberately left to
				// the repair line, which states a stronger version of the same thing.
				if item == sim.NailItemKind && payload.TradeValue.RepairReserve != nil {
					continue
				}
				var reserved *TradeValueItem
				for i := range payload.TradeValue.Items {
					if payload.TradeValue.Items[i].itemKind == item {
						reserved = &payload.TradeValue.Items[i]
					}
				}
				if reserved == nil {
					continue // not one of this actor's own wares — never valued at all
				}
				checked++
				if reserved.ReserveHeld != held || reserved.ReserveFloor != floor {
					t.Errorf("scenario %q: %q is reserved for the bench (%d held, floor %d) but the view carries held=%d floor=%d",
						sc.name, item, held, floor, reserved.ReserveHeld, reserved.ReserveFloor)
				}
				// Render turns an empty ReserveMakes back into a priced line, so a
				// disagreement between ReorderFloors and reservedMakings would silently
				// undo the reservation. The two walk the same produce entries under the
				// same Qty > 0 test, so this holds for any valid catalog — pinned here
				// rather than left to the reader.
				if len(reserved.ReserveMakes) == 0 {
					t.Errorf("scenario %q: %q has a reorder floor but reservedMakings named nothing it feeds", sc.name, item)
				}
				prefix := "- " + itemDisplayLabel(snap, item) + ":"
				rendered := 0
				for _, line := range strings.Split(section, "\n") {
					if !strings.HasPrefix(line, prefix) {
						continue
					}
					rendered++
					if !strings.Contains(line, "makings, not wares") {
						t.Errorf("scenario %q: %q is reserved for the bench but its wares line is priced:\n%s", sc.name, item, line)
					}
				}
				if rendered != 1 {
					t.Errorf("scenario %q: want exactly 1 wares line for the reserved %q, got %d", sc.name, item, rendered)
				}
			}
		})
	}
	// Non-vacuity: with no scenario holding a floored good at or under its floor the
	// whole test is a no-op and would pass a regression through in silence.
	// smith_holding_forge_water_in_company exists to keep this positive.
	if checked == 0 {
		t.Error("no scenario exercised a bench reservation — the invariant proved nothing")
	}
}

// TestGoldensKeepBackClauseOnlyAboveTheFloor is the other half of the LLM-609
// invariant: the keep-back clause is a statement about SURPLUS, so it may never
// ride a line for a good the actor does not hold more of than its bench needs. A
// keep-back naming more than the actor carries would tell a keeper to hold back
// stock that does not exist, which is the same species of defect as pricing stock
// that is already spoken for.
//
// Checked in both directions off the structured view, never by associating a
// rendered line back to an item by its display label alone: two kinds sharing a
// label would let a keep-back meant for one satisfy the check for the other. The
// forward pass requires each surplus item to own exactly one line carrying the
// clause (which is itself the ambiguity check — a shared label matches twice), and
// the reverse pass is a count, so no clause can appear without an item behind it.
func TestGoldensKeepBackClauseOnlyAboveTheFloor(t *testing.T) {
	const marker = "beyond that are yours to sell"
	seen := 0
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			if a := snap.Actors[actorID]; a == nil || a.RestockPolicy == nil {
				return
			}
			payload := Build(snap, actorID, warrants)
			if payload.TradeValue == nil {
				return
			}
			lines := strings.Split(promptSection(combinedPrompt(Render(payload, DefaultRenderConfig())), "## What your wares fetch"), "\n")

			expected := 0
			for i := range payload.TradeValue.Items {
				it := payload.TradeValue.Items[i]
				if it.ReserveFloor <= 0 || it.ReserveHeld <= it.ReserveFloor {
					continue
				}
				expected++
				prefix := "- " + itemDisplayLabel(snap, it.itemKind) + ":"
				matched := 0
				for _, line := range lines {
					if !strings.HasPrefix(line, prefix) {
						continue
					}
					matched++
					if !strings.Contains(line, marker) {
						t.Errorf("scenario %q: %q holds %d against a floor of %d but its line states no keep-back:\n%s",
							sc.name, it.itemKind, it.ReserveHeld, it.ReserveFloor, line)
					}
				}
				if matched != 1 {
					t.Errorf("scenario %q: want exactly 1 wares line for the surplus %q, got %d (a duplicate display label would read as another item's line)",
						sc.name, it.itemKind, matched)
				}
			}
			seen += expected

			// The repair earmark uses the same surplus phrasing (LLM-292) and is
			// governed by the mend, not by a recipe floor.
			rendered := 0
			for _, line := range lines {
				if strings.Contains(line, marker) && !strings.Contains(line, "to mend your") {
					rendered++
				}
			}
			if rendered != expected {
				t.Errorf("scenario %q: %d keep-back clauses rendered, but %d items carry a bench surplus", sc.name, rendered, expected)
			}
		})
	}
	// Non-vacuity, as above — the intended positive is
	// miller_offered_parity_swap_for_flour, where 37 wheat clear a floor of 10.
	if seen == 0 {
		t.Error("no scenario rendered a keep-back clause — the invariant proved nothing")
	}
}

// TestRestockBuyAnchorRendersWhenRateKnown is the buy-leg anchor invariant
// (LLM-292, reworked observed-first in LLM-295, and again for the LLM-427 margin
// verdicts), re-derived from each scenario's fixture rather than trusting the
// build that produced the section: every rendered "## Restocking" item line whose
// ITEM has a resolvable buying-in rate — an observed supplier rate if one exists,
// else the catalog seed — must carry that corrective rate in SOME clause (the
// self-poisoning last-paid anchor must never again be the cue's only number: the
// live Josiah 2.2/unit milk leg). Which clause depends on whether a realized
// resale rate also exists: with one, the LLM-427 margin verdict names the buy
// rate ("you buy at about …" / "You pay about …" / "you've been paying about …");
// without one, the lone anchor sentence keeps its overpay guard ("… above that
// and you're overpaying"). Checked per line, not section-wide, so a mixed
// rate/no-rate section can't hide an anchor attached to the wrong item
// (code_review), and both directions are guarded — an item with neither an
// observed rate nor a catalog price must not conjure either clause. A fixture
// that owes an anchor but renders no Restocking section at all fails too (the
// anchor can't render if the section disappears), as does an owed item whose
// LINE never renders; and a fixture whose duplicate display labels would make
// per-line attribution ambiguous is itself an error to fix, not a case to skip
// (code_review — the old silent skip let a missing clause hide behind a twin).
func TestRestockBuyAnchorRendersWhenRateKnown(t *testing.T) {
	const marker = "above that and you're overpaying"
	// The margin verdict's three buy-rate phrasings (one per tier). Any of them
	// carries the corrective rate on a line whose margin is judgeable.
	// Shared with the other margin invariants — see restockMarginVerdictLabels.
	hasMarginVerdict := hasRestockMarginVerdict
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subj := snap.Actors[actorID]
			// LLM-294: a keeper in conserve mode (coin-poor + overstocked) has every
			// restock item line replaced by the hold-off-buying steer — no full
			// "- You have N <label>" line, so no catalog anchor is owed regardless of
			// catalog pricing. The per-line loop below is naturally a no-op then (the
			// conserve lines don't match the "- You have " prefix).
			conserve := subj != nil && merchantConserve(snap, actorID, subj).Active
			// Expected-anchor derivation, computed BEFORE looking at the render: an
			// anchor is owed iff some below-threshold effective buy entry (the
			// section's own gate) has a catalog rate AND actually renders a full item
			// line (an unactionable item is omitted per LLM-216; a pending-offer bide
			// steer replaces the whole line per LLM-64; conserve mode replaces every
			// line per LLM-294). Also maps each entry's display label to its rate for
			// the per-line check below.
			want := false
			type buyRateExpectation struct {
				rate int
				tier restockMarginTier
			}
			expectByLabel := map[string]buyRateExpectation{}
			owedByLabel := map[string]bool{}
			if subj != nil && subj.RestockPolicy != nil {
				floors := sim.ReorderFloors(snap.Recipes, subj.RestockPolicy)
				// LLM-608: a worn business's section narrows to the inputs the owner's
				// own production consumes (and goes silent entirely when production is
				// hard-blocked at pct 0). Mirror that drop the same way the conserve and
				// LLM-216 drops below are mirrored, or the invariant demands a line the
				// build deliberately omits.
				degraded := ownerBusinessDegraded(snap, actorID)
				var productionInputs map[sim.ItemKind]bool
				if degraded && !ownerBusinessProduceBlocked(snap, actorID) {
					productionInputs = sim.ProductionInputKinds(snap.Recipes, subj.RestockPolicy)
				}
				for _, e := range sim.EffectiveBuyEntries(snap.Recipes, subj.RestockPolicy) {
					// The RESOLVED anchor rate the same way buildRestocking derives it:
					// observed reachable-supplier rate first (LLM-295), catalog seed as
					// fallback. Same reachable-supplier inputs the build uses.
					_, coID := coPresentSellerForItem(snap, actorID, subj, e.Item)
					vendors, _ := findItemVendors(snap, actorID, subj, e.Item)
					rate, observed := observedSupplierBuyRate(vendors, coID, snap, e.Item, restockSalesWindow)
					if !observed {
						rate = catalogBulkRate(snap, e.Item)
					}
					// The realized resale rate, derived the same way buildRestocking does,
					// and from it the margin tier that selects WHICH clause owes the buy
					// rate on this item's line (LLM-427).
					salesUnits, salesCoins := sellerRecentSales(snap, actorID, e.Item, restockSalesWindow)
					resale := 0
					if salesUnits > 0 {
						resale = (salesCoins + salesUnits/2) / salesUnits
					}
					exp := buyRateExpectation{rate: rate, tier: restockMarginTierOf(rate, resale)}
					label := itemDisplayLabel(snap, e.Item)
					// Rendered item lines carry only the display label, so two kinds
					// sharing a label with differing expectations would make the
					// per-line check unattributable. Silently skipping such labels let
					// a missing clause hide behind its twin (code_review), so an
					// ambiguous fixture is an error to fix, not a case to tolerate.
					if prev, ok := expectByLabel[label]; ok && ((prev.rate > 0) != (rate > 0) || prev.tier != exp.tier) {
						t.Fatalf("scenario %q: two buy entries share display label %q with differing buy-rate expectations — disambiguate the fixture so the per-line check can attribute clauses (LLM-292/LLM-427)", sc.name, label)
					}
					expectByLabel[label] = exp
					if !sim.RestockReorderThresholdMet(subj.Inventory[e.Item], e.Cap(), snap.RestockReorderPct, floors[e.Item]) {
						continue
					}
					if rate <= 0 {
						continue
					}
					if !itemHasActionableBuyPath(snap, actorID, subj, e.Item) {
						continue // omitted line (LLM-216) carries no anchor
					}
					if _, coID := coPresentSellerForItem(snap, actorID, subj, e.Item); coID != "" && hasPendingOfferTo(snap, actorID, coID, e.Item) {
						continue // bide steer replaces the item line (LLM-64)
					}
					if conserve {
						continue // conserve mode replaces the item line — no anchor owed (LLM-294)
					}
					if degraded && !productionInputs[e.Item] {
						continue // LLM-608: resale stock waits on the mending — no line, no anchor owed
					}
					want = true
					owedByLabel[label] = true // a full item line must render AND carry this label's clause
				}
			}
			_, section, found := strings.Cut(renderScenario(sc), "## Restocking\n")
			if !found {
				if want {
					t.Errorf("scenario %q: an anchor-owing buy entry exists but no '## Restocking' section rendered (LLM-292)", sc.name)
				}
				return
			}
			if idx := strings.Index(section, "\n## "); idx >= 0 {
				section = section[:idx]
			}
			// Per-line attachment: every full item line ("- You have N <label> on
			// hand…") carries the clause its own rates owe. Bide steers and the
			// walk-to sub-bullets don't match the prefix and are skipped by design.
			// seenByLabel closes the other gap (code_review): an owed item whose line
			// never renders at all would otherwise slip past a per-line-only check.
			seenByLabel := map[string]bool{}
			for _, line := range strings.Split(section, "\n") {
				rest, ok := strings.CutPrefix(line, "- You have ")
				if !ok {
					continue
				}
				head, _, ok := strings.Cut(rest, " on hand")
				if !ok {
					continue
				}
				label := strings.TrimLeft(head, "0123456789 ")
				exp, known := expectByLabel[label]
				if !known {
					continue // not one of the effective entries — nothing to assert
				}
				seenByLabel[label] = true
				if strings.Contains(line, ", and no way to restock just now.") {
					continue // the LLM-406 blocked scene: names suppliers and reasons, never a price
				}
				switch {
				case exp.tier != marginUnknown:
					// Both rates known — the margin verdict carries the buy rate, and
					// the lone overpay-guard sentence must not also render beside it.
					if !hasMarginVerdict(line) {
						t.Errorf("scenario %q: item line %q lacks the margin verdict owed a line with both rates (LLM-427)", sc.name, line)
					}
					if strings.Contains(line, marker) {
						t.Errorf("scenario %q: item line %q carries the lone overpay guard alongside a judgeable margin (LLM-427)", sc.name, line)
					}
				case exp.rate > 0:
					// Buy rate only — the pre-LLM-427 anchor sentence with its guard.
					if !strings.Contains(line, marker) {
						t.Errorf("scenario %q: item line %q anchor missing for a priced item (per-line attachment, LLM-292)", sc.name, line)
					}
				default:
					// No resolvable buy rate — neither clause may conjure one.
					if strings.Contains(line, marker) || hasMarginVerdict(line) {
						t.Errorf("scenario %q: item line %q conjures a buy-rate clause for an unpriced item (LLM-292)", sc.name, line)
					}
				}
			}
			// Every entry that owes a clause must have rendered a full item line the
			// loop above could check — a section that silently drops an owed item
			// would otherwise pass on absence (code_review).
			for label := range owedByLabel {
				if !seenByLabel[label] {
					t.Errorf("scenario %q: item %q owes a buy-rate clause but no '- You have … %s on hand' line rendered (LLM-292)", sc.name, label, label)
				}
			}
		})
	}
}

// TestProducerPitchNoteOnlyForCoPresentMaker is the LLM-171 seller-side
// cross-scenario invariant: the producer-awareness note that steers a keeper off
// pitching a maker their own ware back appears in EXACTLY the scenario where a
// co-present customer makes one of the seller's listed goods
// (keeper_not_pitching_makers_own_ware). No other "## Custom at hand" scenario —
// nor any unrelated turn — carries it.
func TestProducerPitchNoteOnlyForCoPresentMaker(t *testing.T) {
	const marker = "don't pitch those back to their own maker"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "keeper_not_pitching_makers_own_ware"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: producer-pitch note present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestBuyBackQuoteSteerOnlyForOwnProducedOrAtCap is the LLM-171 buyer-side
// cross-scenario invariant: the steer that withholds a buy-quote's take for a
// good the buyer makes itself or already holds at cap appears in EXACTLY the
// scenario where that holds (maker_offered_own_ware_buy_quote). In that scenario
// the actionable "pay_with_item with quote_id" take is absent — the steer
// REPLACES it, so the buy-back loop can't close — while no other turn shows it.
func TestBuyBackQuoteSteerOnlyForOwnProducedOrAtCap(t *testing.T) {
	const (
		steer = "there's no reason to buy"
		take  = "pay_with_item with quote_id"
	)
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "maker_offered_own_ware_buy_quote"
		if has := strings.Contains(got, steer); has != want {
			t.Errorf("scenario %q: buy-back steer present=%v, want %v", sc.name, has, want)
		}
		if want && strings.Contains(got, take) {
			t.Errorf("scenario %q: redundant buy-quote still shows the actionable take %q — it must be withheld", sc.name, take)
		}
	}
}

// TestCoinQuoteTakeNamesConcreteTerms is the LLM-172 cross-scenario invariant:
// the single-line coin-quote take never falls back to the unanchored "the same
// item, qty, and amount" phrasing that a buyer carrying other goods misbound to
// one of those (paying for nothing via a bare pay). Wherever the actionable take
// renders it must name the concrete item/qty/amount; buyer_offered_quote_take_names_terms
// pins the exact string for the live stew case.
func TestCoinQuoteTakeNamesConcreteTerms(t *testing.T) {
	const vague = "the same item, qty, and amount"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		if strings.Contains(got, vague) {
			t.Errorf("scenario %q: coin-quote take still uses the unanchored %q phrasing — name the concrete item/qty/amount", sc.name, vague)
		}
		if sc.name == "buyer_offered_quote_take_names_terms" {
			if want := `item "stew", qty 1, and amount 4`; !strings.Contains(got, want) {
				t.Errorf("scenario %q: take missing the concrete terms %q\n%s", sc.name, want, got)
			}
		}
	}
}

// TestStallRepairCueOnlyAtOwnWornStall is the LLM-118 cross-scenario invariant
// (generalized to all businesses in LLM-247): the "## Your business" owner repair
// cue appears in EXACTLY the scenarios where the actor stands at their OWN worn
// business — never for a passerby (who gets the co-present line instead) or any
// unrelated scenario. Covers a market stall (owner_at_worn/degraded_stall) AND a
// tavern (owner_at_worn_tavern) to pin that the gate is the business tag, not
// market_stall. The same StallRepair signal gates the repair tool, so this also
// pins where the tool is offered.
func TestStallRepairCueOnlyAtOwnWornStall(t *testing.T) {
	const marker = "## Your business"
	ownWornBusiness := map[string]bool{
		"owner_at_worn_stall":                               true,
		"owner_at_worn_stall_with_nail_supplier":            true, // LLM-274: owner short of nails WITH a resolvable supplier
		"owner_at_worn_stall_nail_keeper_copresent":         true, // LLM-504: supplier's keeper standing with the buyer — the named-bullet arm
		"owner_at_degraded_stall":                           true,
		"owner_at_degraded_stall_produce_blocked":           true, // LLM-446: the legacy pct-0 full-block arm
		"degraded_smith_short_of_own_nails":                 true, // LLM-446: the sole-nail-producer live-bug fixture
		"degraded_smith_short_of_own_nails_produce_blocked": true, // LLM-446: same fixture at pct 0 — the forge-your-own steer must NOT render
		"owner_at_degraded_store_conserving":                true, // LLM-301: vendor-less fallback, conserve arm
		"owner_conserving_with_nail_supplier":               true, // LLM-301: conserve wins over a surviving vendor
		"owner_at_worn_tavern":                              true,
		"owner_inside_worn_business":                        true, // LLM-266: owner INSIDE their worn business (not at the outdoor pin)
		"owner_holding_repair_nails_in_company":             true, // LLM-292: owner at own worn store's pin (the earmark fixture)
		"smith_worn_forge_out_of_water":                     true, // LLM-608: degraded smith inside his own forge, short of a bought input
		"miller_at_degraded_mill_keeps_water_forage_cue":    true, // LLM-634: degraded miller inside his own Mill, forage cue alongside
		"smith_out_of_water_owes_nails_no_seller":           true, // LLM-635: degraded smith inside his own forge, nail input short, no seller
		"smith_out_of_water_can_still_make_shovels":         true, // LLM-635: same, with another makeable good keeping the produce tool offered
		"smith_out_of_water_forage_entry_no_source":         true, // LLM-635: same, bare forage entry, no remembered source
		"smith_out_of_water_own_spring":                     true, // LLM-635: same, owned stocked spring — the forage arm
	}
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := ownWornBusiness[sc.name]
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: '## Your business' cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensRepairCueWheneverColocatedOwnerRepairable is the LLM-266 cross-scenario
// invariant: whenever the subject OWNS a repairable business and is co-located with
// it — standing INSIDE the business structure OR at its outdoor loiter pin — the
// rendered prompt must carry the "## Your business" repair cue. The repair tool is
// gated on the very same StallRepair payload signal (handlers/tool_gating.go), so
// "cue renders" ⇔ "tool advertised". The old pin-only gate silently failed this for
// every indoor keeper — the cue had never once rendered live. Runs over the whole
// matrix so a future change can't re-narrow co-location for any owned-business
// situation, not just the owner_inside_worn_business scenario that anchors it.
// Keyed off the same sim.AtBusiness predicate the production gate uses, so the two
// agree by construction; owner_inside_worn_business is the non-vacuous golden that
// would break if the cue stopped rendering end-to-end while the predicate held.
func TestGoldensRepairCueWheneverColocatedOwnerRepairable(t *testing.T) {
	const header = "## Your business"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			stall := sim.OwnedWearableStall(snap.VillageObjects, actorID)
			if stall == nil {
				return // subject owns no business — invariant N/A here
			}
			if !sim.StallRepairable(stall, snap.StallWearRepairThreshold, snap.StallWearDegradeThreshold) {
				return // business isn't worn enough to mend — no cue expected
			}
			if !sim.AtBusiness(a.Pos, a.InsideStructureID, stall.ID, objectLoiterPin(stall), true) {
				return // subject isn't co-located with their business — cue correctly absent
			}
			if out := renderScenario(sc); !strings.Contains(out, header) {
				t.Errorf("scenario %q: subject owns a repairable business and is co-located with it (inside or at pin) but the prompt omits the %q repair cue (LLM-266)", sc.name, header)
			}
		})
	}
}

// TestGoldensNoSourceActivityStartCueWhileMidSourceActivity is the LLM-435
// cross-scenario invariant: whenever the subject already has a timed source
// activity in flight (gather/repair/stoke/refresh), the built payload must carry
// NONE of the source-activity-START cues — the hearth cue (gates stoke), the
// stall-repair cue (gates repair), or the gatherable cue (gates gather). The
// substrate rejects a fresh start mid-window ("you are already busy ..."), and
// the minted result lands only at completion, so the source object still reads as
// actionable; an un-gated cue would re-advertise its start tool to a busy actor
// and bait that reject on every reactor tick inside the window (the live Hannah
// "already busy ... before tending the fire" reject). The mid-activity coda holds
// the actor to done() instead. Runs over the whole matrix so a future cue can't
// reintroduce a start affordance for a busy actor; keeper_mid_stoke_no_restoke is
// the non-vacuous fixture.
func TestGoldensNoSourceActivityStartCueWhileMidSourceActivity(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			// Key off the SAME predicate the builders gate on (the source snapshot's
			// SourceActivityKind), not the derived InFlightSourceActivity view, so the
			// test can't miss exactly the state the suppression targets.
			if !actorMidSourceActivity(snap.Actors[actorID]) {
				return // subject isn't mid a source activity — invariant N/A here
			}
			p := Build(snap, actorID, warrants)
			if p.Hearth != nil {
				t.Errorf("scenario %q: subject is mid a source activity, but the hearth (stoke) cue is advertised — a fresh stoke bounces \"already busy\"", sc.name)
			}
			if p.StallRepair != nil {
				t.Errorf("scenario %q: subject is mid a source activity, but the stall-repair cue is advertised — a fresh repair bounces \"already busy\"", sc.name)
			}
			if p.Surroundings.GatherableItem != "" {
				t.Errorf("scenario %q: subject is mid a source activity, but the gatherable (gather) cue is advertised (%q) — a fresh gather bounces \"already busy\"", sc.name, p.Surroundings.GatherableItem)
			}
		})
	}
}

// TestGoldensHiredWorkerSeesRepairCueWhenColocated is the LLM-271 cross-scenario
// invariant, the hired-worker twin of TestGoldensRepairCueWheneverColocatedOwnerRepairable:
// whenever the subject is NOT the owner but is Working a hired job whose employer owns
// a repairable business, AND the subject is co-located with that business, the rendered
// prompt must carry the hired "## The business you're working at" cue — never the owner's
// "## Your business" (that would tell a hired hand it owns the shop). The repair tool is
// gated on the same StallRepair payload signal, so "cue renders" ⇔ "tool advertised".
// Keyed off the same sim.WearableStallToMend resolver + sim.AtBusiness predicate the
// production gate uses, so the two agree by construction; hired_worker_at_employer_worn_business
// is the non-vacuous golden that would break if the cue stopped rendering end-to-end.
func TestGoldensHiredWorkerSeesRepairCueWhenColocated(t *testing.T) {
	const (
		hiredHeader = "## The business you're working at"
		ownerHeader = "## Your business"
	)
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			stall, hired := sim.WearableStallToMend(snap.VillageObjects, snap.LaborLedger, actorID)
			if !hired {
				return // subject reaches no business through a hire — invariant N/A here
			}
			if !sim.StallRepairable(stall, snap.StallWearRepairThreshold, snap.StallWearDegradeThreshold) {
				return // business isn't worn enough to mend — no cue expected
			}
			if !sim.AtBusiness(a.Pos, a.InsideStructureID, stall.ID, objectLoiterPin(stall), true) {
				return // subject isn't co-located with the employer's business — cue correctly absent
			}
			exercised = true
			out := renderScenario(sc)
			if !strings.Contains(out, hiredHeader) {
				t.Errorf("scenario %q: subject is a hired worker co-located with the employer's repairable business but the prompt omits the %q repair cue (LLM-271)", sc.name, hiredHeader)
			}
			if strings.Contains(out, ownerHeader) {
				t.Errorf("scenario %q: subject is a hired worker (not the owner) but the prompt shows the owner %q cue — a hired hand doesn't own the shop (LLM-271)", sc.name, ownerHeader)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise a hired worker co-located with the employer's repairable business (LLM-271)")
	}
}

// TestOwnerShortNailsWithSupplierNamesDestination is the LLM-274 cross-scenario
// invariant: whenever the OWNER "## Your business" repair cue renders, the owner is
// short of nails, findItemVendors resolves at least one open nail supplier, AND the
// working-capital gate isn't holding buys off (Conserve wins over the vendor list —
// LLM-301), the cue must name that supplier's move_to destination
// ("(destination: <id>)") rather than the destination-less dead end that llama-3.3-70b
// narrated but never walked (the live Elizabeth Ellis case). Keyed off the same
// buildStallRepair the production render uses, so the property holds by construction
// across the matrix; owner_at_worn_stall_with_nail_supplier is the non-vacuous golden
// that would break if the destination stopped rendering. Its foil — an owner short of
// nails with NO resolvable supplier — is correctly excluded (NailVendors empty), where
// the generic sentence with no dangling target is the intended output (LLM-216 posture).
func TestOwnerShortNailsWithSupplierNamesDestination(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			v := buildStallRepair(snap, actorID, a)
			if v == nil || v.Hired || v.HasEnoughNails {
				return // not the owner repair cue, or the owner already carries enough nails
			}
			if len(v.NailVendors) == 0 {
				return // no resolvable supplier — the generic no-destination sentence is correct here
			}
			if v.Conserve {
				return // LLM-301: conserve wins over the vendor list — the sell-first soften renders instead
			}
			exercised = true
			token := "(destination: " + string(v.NailVendors[0].StructureID) + ")"
			// Scope the assertion to the "## Your business" section so a matching token
			// in some other section (e.g. a co-rendered Restocking cue) can't mask a
			// regression of the repair cue itself (code_review).
			section := promptSection(renderScenario(sc), "## Your business")
			if !strings.Contains(section, token) {
				t.Errorf("scenario %q: owner is short of nails with a resolvable nail supplier but the '## Your business' cue omits its move_to destination %q — the model narrates the errand instead of walking it (LLM-274)", sc.name, token)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise an owner short on nails with a resolvable nail supplier (LLM-274)")
	}
}

// TestOwnerShortNailsRepairCueNeverGoadsUnactionableBuy is the LLM-301
// cross-scenario invariant, two arms:
//
//  1. CONSERVE WINS: whenever the owner "## Your business" cue renders short of
//     nails and the working-capital gate says hold off (Conserve), the sell-first
//     soften must render and the "Use move_to to reach a supplier" walk-to goad must
//     NOT — even when a supplier survives findItemVendors (the affordability drop
//     and the coin floor are different filters), so this cue can never push a buy
//     while "## Restocking" says hold off (the LLM-297 posture).
//     owner_conserving_with_nail_supplier / owner_at_degraded_store_conserving are
//     the non-vacuous anchors.
//  2. VENDOR-LESS FALLBACK: with no resolvable supplier and no conserve, the
//     section must state the plain shortfall and must NOT name "the smith" — a
//     person-shaped target with no move_to destination (the live model invented
//     "the Smithy" for it and burned its turn retrying the refused move).
//     owner_at_worn_stall & co are the anchors.
func TestOwnerShortNailsRepairCueNeverGoadsUnactionableBuy(t *testing.T) {
	var fallbackExercised, conserveExercised, conserveVendorExercised, makesNailsExercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			v := buildStallRepair(snap, actorID, a)
			if v == nil || v.Hired || v.HasEnoughNails {
				return // not the owner short-of-nails cue — invariant N/A
			}
			section := promptSection(renderScenario(sc), "## Your business")
			if v.MakesNails {
				// LLM-446: a nail-producing owner (the sole smith) must be steered to
				// forge their own — never a buy errand of any shape: no vendor walk-to,
				// no person-shaped "the smith", no coin soften. Wins over every buy
				// branch including conserve (making costs no coin).
				makesNailsExercised = true
				if !strings.Contains(section, "nails are your own work") {
					t.Errorf("scenario %q: nail-producing owner is missing the forge-your-own mend steer (LLM-446)", sc.name)
				}
				if strings.Contains(section, "Use move_to to reach a supplier") || strings.Contains(section, "(destination:") ||
					strings.Contains(section, "from the smith") || strings.Contains(section, "buy") {
					t.Errorf("scenario %q: nail-producing owner is handed a buy-shaped errand for the nails only they produce (LLM-446)\n%s", sc.name, section)
				}
				return
			}
			if v.Conserve {
				conserveExercised = true
				if len(v.NailVendors) > 0 {
					conserveVendorExercised = true
				}
				if !strings.Contains(section, "your purse can't take on nails just now") {
					t.Errorf("scenario %q: conserve is active but the sell-first soften is missing from the '## Your business' cue (LLM-301)", sc.name)
				}
				if strings.Contains(section, "Use move_to to reach a supplier") || strings.Contains(section, "(destination:") {
					t.Errorf("scenario %q: conserve is active but the cue still goads the walk-to nail buy — it must not contradict the '## Restocking' hold-off (LLM-301)", sc.name)
				}
				return
			}
			if len(v.NailVendors) > 0 {
				return // vendor-list branch — TestOwnerShortNailsWithSupplierNamesDestination covers it
			}
			fallbackExercised = true
			if strings.Contains(section, "from the smith") {
				t.Errorf("scenario %q: vendor-less repair fallback names a person-shaped smith target with no move_to destination (LLM-301)", sc.name)
			}
			if !strings.Contains(section, "you'll need to buy more before you can mend it") {
				t.Errorf("scenario %q: vendor-less repair fallback is missing the plain shortfall statement (LLM-301)", sc.name)
			}
		})
	}
	if !fallbackExercised {
		t.Error("matrix must exercise an owner short on nails with no resolvable supplier and no conserve (LLM-301)")
	}
	if !conserveExercised {
		t.Error("matrix must exercise a conserving owner short on nails (LLM-301)")
	}
	if !conserveVendorExercised {
		t.Error("matrix must exercise conserve winning over a surviving nail supplier (LLM-301)")
	}
	if !makesNailsExercised {
		t.Error("matrix must exercise a nail-producing owner short of their own nails (LLM-446)")
	}
}

// TestGoldensOwnerOffPostShortNailsSeesBuyErrand is the LLM-277 cross-scenario
// invariant: whenever the subject OWNS a repairable business, is AWAY from it (not
// co-located), is short of the nails a repair takes, and has at least one actionable
// buy path — exactly the state buildStallRepairBuy renders — the prompt must carry the
// off-post "## Nails to mend your business" errand cue AND must NOT fire the
// return-to-post to-work steer (the errand suppresses it while she fetches nails). The
// two properties are the whole point of the ticket: the cue that vanished off-post now
// persists, and the nag that yanked her home now defers. Keyed off the same
// buildStallRepairBuy the production render uses, so it holds by construction across the
// matrix; owner_off_post_at_smith_short_nails / owner_off_post_short_nails_walking are
// the non-vacuous goldens. The at-business owner is excluded by buildStallRepairBuy's
// AtBusiness gate — "## Your business" (LLM-274) covers the buy there.
func TestGoldensOwnerOffPostShortNailsSeesBuyErrand(t *testing.T) {
	const header = "## Nails to mend your business"
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			if buildStallRepairBuy(snap, actorID, a) == nil {
				return // no off-post nail-buy errand for this actor — invariant N/A
			}
			exercised = true
			p := Build(snap, actorID, warrants)
			out := combinedPrompt(Render(p, DefaultRenderConfig()))
			if !strings.Contains(out, header) {
				t.Errorf("scenario %q: owner is off her worn business, short of nails, with a buy path, but the prompt omits the %q errand cue (LLM-277)", sc.name, header)
			}
			if p.DutySteer != nil && p.DutySteer.ToWork {
				t.Errorf("scenario %q: owner is on a nail-buy errand but the return-to-post to-work steer still fires — it must defer while she fetches nails (LLM-277)", sc.name)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise an owner off-post, short of nails, with an actionable buy path (LLM-277)")
	}
}

// TestGoldensBuyNowGoadNeverBesideHoldOff is the LLM-297 cross-scenario invariant: the
// "## Nails to mend your business" co-present "Buy it now —" goad (renderCoPresentBuy) and
// the working-capital "Hold off buying more" restock steer must never render in the same
// prompt — they are contradictory buy/hold instructions. A keeper who owns a worn business,
// is off it beside a nail seller, AND is coin-poor+overstocked would emit both before the
// fix; merchantConserve now softens the nail goad for exactly that keeper.
// keeper_conserving_owes_nail_repair is the non-vacuous anchor (it carries the hold-off
// line). The goad check is scoped to the nail section because renderCoPresentBuy is shared
// with other cues (the farm-upkeep and restock co-present buys) that may legitimately say
// "Buy it now —" elsewhere in the prompt — this invariant is specifically the nail-repair /
// restock contradiction.
func TestGoldensBuyNowGoadNeverBesideHoldOff(t *testing.T) {
	const goad = "Buy it now —"
	const holdOff = "Hold off buying more"
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, holdOff) {
				return // no restock hold-off advice in this prompt — invariant N/A here
			}
			exercised = true
			if strings.Contains(promptSection(out, "## Nails to mend your business"), goad) {
				t.Errorf("scenario %q: the nail-repair goad (%q) renders in the same prompt as the restock hold-off advice (%q) — two sections issue contradictory buy/hold instructions (LLM-297)", sc.name, goad, holdOff)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise a keeper in working-capital conserve mode (the 'Hold off buying more' advice) so the buy-now/hold-off invariant isn't vacuous (LLM-297)")
	}
}

// TestGoldensFarmUpkeepGoadNeverBesideHoldOff is the LLM-299 cross-scenario invariant — the
// shovel twin of TestGoldensBuyNowGoadNeverBesideHoldOff: the "## Farm upkeep" co-present
// "Buy it now —" goad (renderCoPresentBuy) and the working-capital "Hold off buying more"
// restock steer must never render in the same prompt. A farm-owning keeper who is off her
// farm beside a shovel seller AND coin-poor+overstocked would emit both before the fix;
// merchantConserve now softens the shovel goad for exactly that keeper. The goad check is
// scoped to the "## Farm upkeep" section because renderCoPresentBuy is shared with other
// cues (the nail repair-buy and restock co-present buys) that may legitimately say
// "Buy it now —" elsewhere in the prompt. farm_owner_conserving_owes_upkeep is the
// non-vacuous anchor (it carries the hold-off line beside the farm-upkeep cue).
func TestGoldensFarmUpkeepGoadNeverBesideHoldOff(t *testing.T) {
	const goad = "Buy it now —"
	const holdOff = "Hold off buying more"
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if !strings.Contains(out, holdOff) {
				return // no restock hold-off advice in this prompt — invariant N/A here
			}
			if !strings.Contains(out, "## Farm upkeep") {
				return // no farm-upkeep cue here — the shovel goad can't collide with the hold-off
			}
			exercised = true
			if strings.Contains(promptSection(out, "## Farm upkeep"), goad) {
				t.Errorf("scenario %q: the farm-upkeep goad (%q) renders in the same prompt as the restock hold-off advice (%q) — two sections issue contradictory buy/hold instructions (LLM-299)", sc.name, goad, holdOff)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise a farm-owning keeper in working-capital conserve mode who owes upkeep (the 'Hold off buying more' advice beside a '## Farm upkeep' cue) so the buy-now/hold-off invariant isn't vacuous (LLM-299)")
	}
}

// TestFarmOwnerOwesUpkeepWithSupplierNamesDestination is the LLM-274 cross-scenario
// invariant for the farm-upkeep cue (the shovel twin of the nail invariant above):
// whenever the "## Farm upkeep" cue renders AND findItemVendors resolves at least one
// shovel supplier, the cue must name that supplier's move_to destination
// ("(destination: <id>)") rather than the destination-less "from the blacksmith" dead
// end. Keyed off the same buildFarmUpkeep the production render uses;
// farm_owner_owes_upkeep_with_shovel_supplier is the non-vacuous golden. Its foil — an
// owing farm owner with NO resolvable supplier — is correctly excluded (ShovelVendors
// empty), where the generic sentence with no dangling target is the intended output.
func TestFarmOwnerOwesUpkeepWithSupplierNamesDestination(t *testing.T) {
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			v := buildFarmUpkeep(snap, actorID, a)
			if v == nil {
				return // no upkeep cue for this actor
			}
			if len(v.ShovelVendors) == 0 {
				return // no resolvable supplier — the generic no-destination sentence is correct here
			}
			if v.CoPresentSeller != "" {
				return // LLM-277: a co-present seller renders the buy-here imperative, which supersedes the walk-to destination
			}
			exercised = true
			token := "(destination: " + string(v.ShovelVendors[0].StructureID) + ")"
			section := promptSection(renderScenario(sc), "## Farm upkeep")
			if !strings.Contains(section, token) {
				t.Errorf("scenario %q: farm owner owes upkeep with a resolvable shovel supplier but the '## Farm upkeep' cue omits its move_to destination %q — the model narrates the errand instead of walking it (LLM-274)", sc.name, token)
			}
		})
	}
	if !exercised {
		t.Error("matrix must exercise a farm owner owing upkeep with a resolvable shovel supplier (LLM-274)")
	}
}

// TestFarmUpkeepCueOnlyForOwingFarmOwner is the LLM-215 cross-scenario invariant: the
// "## Farm upkeep" cue appears in EXACTLY the scenarios where the actor owns a farm
// and owes upkeep shovels — never for a non-farm-owner or any unrelated scenario. It
// backstops the leak an owner-scoped, stock-derived cue is most prone to: showing up
// for someone who doesn't own a farm.
func TestFarmUpkeepCueOnlyForOwingFarmOwner(t *testing.T) {
	const marker = "## Farm upkeep"
	owesUpkeep := map[string]bool{
		"farm_owner_owes_upkeep":                      true,
		"farm_owner_owes_upkeep_with_shovel_supplier": true, // LLM-274: same owing owner, now with a resolvable supplier
		"farm_owner_owes_upkeep_seller_present":       true, // LLM-277: same owing owner, now co-present with the shovel seller
		"farm_owner_owes_upkeep_seller_low_stock":     true, // LLM-299: co-present, seller low on shovels — capped buy still renders the cue
		"farm_owner_standoff_declined_shovels":        true, // LLM-299: co-present, negotiation dead-ended — softened cue still renders
		"farm_owner_conserving_owes_upkeep":           true, // LLM-299: farm-owning keeper in conserve mode — softened cue still renders
		"farm_owner_off_post_owes_upkeep_no_supplier": true, // LLM-277: owes shovels, no reachable supplier — generic cue still renders
		"farm_owner_owes_upkeep_order_in_flight":      true, // LLM-518: owes shovels but they're on order — facts-only cue still renders
	}
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := owesUpkeep[sc.name]
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: '## Farm upkeep' cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestVendorOperatingCueOnlyDuringOperatingHours is the LLM-123 cross-scenario
// invariant: the "How you trade:" trade-conduct block appears in EXACTLY the
// scenarios where a keeper is at its own post AND operating — on shift
// (keeper_at_post_onshift) or staying open past close (keeper_staying_open_offshift)
// — and never at a closed post off-shift (keeper_at_closed_post_offshift_night) nor
// in any non-keeper / off-post scenario. The structural property the
// AtOwnBusinessOperating gate is meant to hold across the whole matrix: off-shift at
// a closed post, the keeper is no longer told to "tend to your trade" at midnight.
func TestVendorOperatingCueOnlyDuringOperatingHours(t *testing.T) {
	const marker = "How you trade:"
	operating := map[string]bool{
		"keeper_at_post_onshift":       true,
		"keeper_staying_open_offshift": true,
		// LLM-171: John keeps his tavern on shift, at post — legitimately operating.
		"keeper_not_pitching_makers_own_ware": true,
		// LLM-231: John keeps his tavern on shift, at post, with a laboring peer present.
		"seller_huddled_with_laboring_peer": true,
		// LLM-231: John keeps his tavern on shift, at post, employing a laboring worker.
		"seller_employing_own_laboring_worker": true,
		// LLM-413: the three concession-gate keepers, all on shift at their posts.
		"reseller_at_post_brisk_trade":     true,
		"reseller_at_post_slow_trade":      true,
		"producer_at_post_sub_batch_sales": true,
		// LLM-613: Josiah keeps his store on shift, at post, with the man he hires.
		"employer_reads_the_wages_he_paid": true,
		// LLM-619: Ezekiel keeps his forge on shift, at post, with a buyer before him.
		"smith_with_empty_shelf_takes_commissions": true,
		// LLM-621: the miller keeps his mill on shift, at post, with his hauler at work.
		"miller_with_hired_hauler_keeps_water_cue": true,
		// LLM-622: the owned-source arm of the same miller — same post, same hours.
		"miller_stepping_out_to_his_own_bush": true,
		// LLM-634: the same miller alone at his degraded Mill — same post, same hours.
		"miller_at_degraded_mill_keeps_water_forage_cue": true,
	}
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := operating[sc.name]
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: trade-conduct cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestKeeperBuyRuleOnlyOffPostWithCompany is the LLM-611 cross-scenario invariant,
// two properties over the whole matrix:
//
//  1. The buyer-side rule ("How you buy:") renders in EXACTLY the scenarios whose
//     subject is a businessowner away from its own post with someone within earshot.
//     Asserted against the gate's own inputs rather than a hand-kept name list, so a
//     future fixture that happens to put a keeper off-post can't drift out of
//     coverage silently.
//
//  2. For any Payload produced by Build, the two trade blocks are MUTUALLY EXCLUSIVE
//     — no prompt carries both the at-post conduct block and the off-post buy rule.
//     This is the WORK-385 guard in structural form: the off-post rule exists
//     precisely because the at-post block must not follow the keeper into someone
//     else's place, so the day one of them leaks into the other's situation, this
//     fails. Scoped to Build deliberately: Payload is an internal struct and a
//     hand-built one setting both flags would render both blocks. Nothing enforces
//     that at the type level, and this test does not claim otherwise.
//
// A businessowner with NO WorkStructureID is expected ABSENT — it is not away from a
// post it does not have, and the rule would name a shelf that does not exist. That is
// a malformed keeper rather than a situation the village produces, so the branch is
// asserted by the predicate below rather than by a fixture; a golden for it would pin
// an invalid world state as though the engine were meant to reach it.
func TestKeeperBuyRuleOnlyOffPostWithCompany(t *testing.T) {
	const (
		buyMarker   = "How you buy:"
		tradeMarker = "How you trade:"
	)
	var exercised bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			p := Build(snap, actorID, warrants)
			hasPost := a.BusinessownerState != nil && a.WorkStructureID != ""
			atOwnPost := hasPost && a.InsideStructureID == a.WorkStructureID
			// Both collections exclude the subject at their source (build.go skips
			// memberID == actorID; sim.colocatedAudienceIDs skips the speaker), so a
			// non-empty either way means somebody ELSE is within earshot.
			hasCompany := len(p.Surroundings.HuddleMembers) > 0 || len(p.Surroundings.CoPresent) > 0
			want := hasPost && !atOwnPost && hasCompany
			if want {
				exercised = true
			}
			got := combinedPrompt(Render(p, DefaultRenderConfig()))
			if has := strings.Contains(got, buyMarker); has != want {
				t.Errorf("scenario %q: buyer-side rule present=%v, want %v (businessowner=%v hasPost=%v atOwnPost=%v company=%v)",
					sc.name, has, want, a.BusinessownerState != nil, hasPost, atOwnPost, hasCompany)
			}
			if strings.Contains(got, buyMarker) && strings.Contains(got, tradeMarker) {
				t.Errorf("scenario %q: prompt carries BOTH the at-post trade block and the off-post buy rule — they are mutually exclusive (LLM-611/WORK-385)", sc.name)
			}
		})
	}
	if !exercised {
		t.Error("no scenario puts a businessowner off its own post with company — the LLM-611 positive arm is uncovered")
	}
}

// TestKeeperOffPostAloneHasNoAudience pins the foil half of the LLM-611 pair at the
// level the gate actually reads. keeper_off_post_buying_alone proves silence, but a
// golden showing no "How you buy:" line cannot distinguish "no audience" from "the
// gate broke" — both render the same bytes. This asserts the fixture's own premise:
// the seller is in the world, and neither audience collection reaches her.
func TestKeeperOffPostAloneHasNoAudience(t *testing.T) {
	snap, actorID, warrants := keeperOffPostBuyingAlone()
	if len(snap.Actors) < 2 {
		t.Fatal("the alone fixture must keep the seller in the world — moving her, not deleting her, is what makes it a one-variable foil")
	}
	p := Build(snap, actorID, warrants)
	if n := len(p.Surroundings.HuddleMembers); n != 0 {
		t.Errorf("HuddleMembers = %d, want 0 — the alone arm must have nobody within earshot", n)
	}
	if n := len(p.Surroundings.CoPresent); n != 0 {
		t.Errorf("CoPresent = %d, want 0 — the alone arm must have nobody within earshot", n)
	}
}

// TestVendorConcessionLineOnlyWhenTradeSlow is the LLM-413 cross-scenario
// invariant, two properties over the whole matrix:
//
//  1. The concession line ("Trade has been thin this week") renders in EXACTLY
//     the scenarios whose keeper had a slow week by the engine's own judgment
//     (keeperTradeSlow) — never as a standing instruction. A keeper with no
//     sales on record (the plain LLM-123 fixtures) is genuinely slow; the one
//     scenario with in-window movement (reseller_at_post_brisk_trade) must not
//     carry the licence to discount.
//  2. Wherever the trade-conduct block renders AT ALL, the margin floor line
//     renders with it — the counterweight is unconditional, so no future gate
//     can reintroduce a concession without a floor.
func TestVendorConcessionLineOnlyWhenTradeSlow(t *testing.T) {
	const (
		blockMarker      = "How you trade:"
		concessionMarker = "Trade has been thin this week"
		floorMarker      = "Never sell a thing for less than it cost you"
	)
	slow := map[string]bool{
		// No seller-side sales on record in these fixtures → a slow week.
		"keeper_at_post_onshift":               true,
		"keeper_staying_open_offshift":         true,
		"keeper_not_pitching_makers_own_ware":  true,
		"seller_huddled_with_laboring_peer":    true,
		"seller_employing_own_laboring_worker": true,
		// The dedicated LLM-413 arms: stale-window reseller, sub-batch producer.
		"reseller_at_post_slow_trade":      true,
		"producer_at_post_sub_batch_sales": true,
		// reseller_at_post_brisk_trade: block renders, concession does NOT.
		// LLM-613: the coin record holds the pair's week, but the fixture carries no
		// seller-side SALES, so his trade reads thin on the same rule as the others.
		"employer_reads_the_wages_he_paid": true,
		// LLM-619: the smith fixture carries no sales either, so his week reads slow too.
		"smith_with_empty_shelf_takes_commissions": true,
		// LLM-621: the miller fixture carries no sales, so his week reads thin as well.
		"miller_with_hired_hauler_keeps_water_cue": true,
		// LLM-622: same fixture, same absent sales.
		"miller_stepping_out_to_his_own_bush": true,
		// LLM-634: the degraded-Mill miller carries no sales either.
		"miller_at_degraded_mill_keeps_water_forage_cue": true,
	}
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		hasBlock := strings.Contains(got, blockMarker)
		if has := strings.Contains(got, concessionMarker); has != slow[sc.name] {
			t.Errorf("scenario %q: concession line present=%v, want %v", sc.name, has, slow[sc.name])
		}
		if hasBlock != strings.Contains(got, floorMarker) {
			t.Errorf("scenario %q: trade-conduct block present=%v but margin floor present=%v — the floor is unconditional within the block", sc.name, hasBlock, strings.Contains(got, floorMarker))
		}
	}
}

// TestLodgingDeskShutCueOnlyWhenRemembered is the LLM-126 cross-scenario invariant:
// the experiential "found the keeper's desk shut" wait-steer appears in EXACTLY the
// scenario where a renewal-due lodger remembers its inn shut and the pull is not
// deferred (lodger_renewal_due_desk_remembered_shut). It must never leak into a lodger
// turn without that memory — the omniscient keeper-asleep read it replaced is gone, so
// the cue is gated purely on the decaying experiential memory.
func TestLodgingDeskShutCueOnlyWhenRemembered(t *testing.T) {
	const marker = "found the keeper's desk shut"
	for _, sc := range perceptionScenarios {
		sc := sc
		got := renderScenario(sc)
		want := sc.name == "lodger_renewal_due_desk_remembered_shut"
		if has := strings.Contains(got, marker); has != want {
			t.Errorf("scenario %q: desk-shut cue present=%v, want %v", sc.name, has, want)
		}
	}
}

// TestGoldensSatiationBuyCueNeverTargetsRememberedShutVendor is the LLM-222 cross-
// scenario invariant: within the "## What you can eat or drink" section of any
// scenario, a vendor the buyer remembers finding shut must never appear as a
// "(destination: <id>)" buy target. A remembered-shut vendor is a seller-
// availability dead end the weak model toured on (Ezekiel's asleep-Inn walk), so
// the buy cue DROPS it rather than annotating it "found it shut up" — mirroring
// LLM-216's restock drop (TestGoldensRestockNeverTargetsRememberedShutSupplier).
// Runs over the whole matrix so a future satiation-cue change can't reintroduce a
// shut vendor as a target. Non-vacuous: buyer_drops_shut_keeps_open_vendor renders
// an eat/drink section while remembering the General Store shut, so the scan
// actually exercises a shut structure.
func TestGoldensSatiationBuyCueNeverTargetsRememberedShutVendor(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil {
				return
			}
			_, section, found := strings.Cut(renderScenario(sc), "## What you can eat or drink\n")
			if !found {
				return // no eat/drink section in this situation — invariant N/A here
			}
			// Bound the scan to the eat/drink section by cutting at the next markdown
			// header, so a shut structure's id appearing lower in the prompt (a
			// restock/seek-work target) can't false-positive.
			if idx := strings.Index(section, "\n## "); idx >= 0 {
				section = section[:idx]
			}
			for structureID := range snap.Structures {
				if !businessRememberedShut(snap, a, structureID) {
					continue
				}
				token := "(destination: " + string(structureID) + ")"
				if strings.Contains(section, token) {
					t.Errorf("scenario %q: the eat/drink buy cue advertises remembered-shut vendor %q as a move target — a shut vendor is a dead end and must be dropped (LLM-222)", sc.name, token)
				}
			}
		})
	}
}

// TestGoldensNoBuyCueWithoutMeansToPay is the cross-scenario invariant over BOTH
// buy-food affordances in "## What you can eat or drink": the walk-to vendor cue
// ("Nearby to buy", LLM-222) and the co-present peer offer ("offer to buy it from
// them now with pay_with_item", LLM-242). A buyer with 0 coins AND no barterable
// goods holds no means of payment at all, so NEITHER affordance may be shown — a
// buy imperative they can neither pay nor barter is the genuine dead end (the
// 55-hit pay_with_item no-offer rejection). Reads the SAME means-to-pay signal the
// build gates do (holdsBarterableGoods + coins), per the discussion-109 no-drift
// rule. Non-vacuous on both arms: broke_buyer_no_goods_no_buy_cue builds this actor
// beside an open cheese seller (vendor arm), broke_buyer_no_goods_no_peer_buy stands
// it in a huddle with a peer carrying stew (peer arm) — each an affordance the cue
// would otherwise advertise.
func TestGoldensNoBuyCueWithoutMeansToPay(t *testing.T) {
	const vendorMarker = "Nearby to buy ("
	const peerMarker = "offer to buy it from them now with pay_with_item"
	var sawBroke, sawPeerArm bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.Coins != 0 || holdsBarterableGoods(snap, a) {
				return // has some means to pay — invariant N/A here
			}
			sawBroke = true
			out := renderScenario(sc)
			if strings.Contains(out, vendorMarker) {
				t.Errorf("scenario %q: buyer holds 0 coins and no barterable goods but the eat/drink cue advertises a walk-to buy — no means to pay is a hard dead-end that must be suppressed (LLM-222)", sc.name)
			}
			if strings.Contains(out, peerMarker) {
				t.Errorf("scenario %q: buyer holds 0 coins and no barterable goods but the eat/drink cue offers to buy from a co-present peer — pay_with_item needs coins or goods, so the peer offer must be suppressed too (LLM-242)", sc.name)
			}
			// Track that the peer arm is genuinely exercised: this no-means subject
			// shares a huddle with a co-present non-PC peer carrying goods — the case
			// where a peer buy line WOULD render absent the LLM-242 gate. Guards the peer
			// half from silently going vacuous if its fixture is ever dropped.
			if h := snap.Huddles[a.CurrentHuddleID]; h != nil {
				for peerID := range h.Members {
					if p := snap.Actors[peerID]; peerID != actorID && p != nil && p.Kind != sim.KindPC && holdsBarterableGoods(snap, p) {
						sawPeerArm = true
					}
				}
			}
		})
	}
	if !sawBroke {
		t.Error("no scenario exercised the no-means-to-pay branch — add one (broke_buyer_no_goods_no_buy_cue)")
	}
	if !sawPeerArm {
		t.Error("no no-means-to-pay scenario stands the buyer in a huddle with a goods-carrying peer — the LLM-242 peer-offer suppression is untested; add one (broke_buyer_no_goods_no_peer_buy)")
	}
}

// TestGoldensNoBarterSteerWithoutSpareGoods is the LLM-636 cross-scenario
// invariant: a coin-broke subject whose pack holds nothing SPARE — only the
// makings it works with, the garment on its back, eat-here food — must never be
// steered to barter anywhere in the prompt. That covers the restock walk-to
// steer, the eat/drink buy steer, and the co-present buy-here goad's
// "goods you carry (pay_items)" clause, since every one of those offers a
// pay_with_item the intake gate would refuse. The spare-goods question is asked
// through the SAME predicate the render and the gate use (holdsBarterableGoods →
// sim.HoldsBarterableGoodsExcept), so the guard cannot drift from them; a
// scenario that reads the reservation differently in one cue would trip it. The
// mirror direction — a subject with a spare good keeps its steer — is pinned by
// broke_maker_own_produce_is_barter and illiquid_distributor_barters_for_stock.
// Non-vacuous: broke_maker_makings_are_not_barter (walk-to) and
// broke_maker_at_store_no_spare_goods_hold_off (co-present) build exactly this pack.
func TestGoldensNoBarterSteerWithoutSpareGoods(t *testing.T) {
	steers := []string{
		"offer goods you carry in trade",
		"goods you carry (pay_items)",
	}
	sawWalkTo, sawCoPresent := false, false
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || a.Coins != 0 || holdsBarterableGoods(snap, a) {
				return // has some means to pay — invariant N/A here
			}
			// Only a subject that HOLDS something is interesting: an empty pack is
			// LLM-222's dead-end, already guarded. This is the pack that looks full.
			holdsAnything := false
			for _, qty := range a.Inventory {
				if qty > 0 {
					holdsAnything = true
				}
			}
			if !holdsAnything {
				return
			}
			out := renderScenario(sc)
			for _, s := range steers {
				if strings.Contains(out, s) {
					t.Errorf("scenario %q: subject holds 0 coins and no SPARE good, yet the prompt steers it to barter (%q) — that offer would be refused at pay_with_item intake, and goading it is the LLM-636 treadmill", sc.name, s)
				}
			}
			if a.CurrentHuddleID != "" {
				sawCoPresent = true
			} else {
				sawWalkTo = true
			}
		})
	}
	if !sawWalkTo {
		t.Error("no scenario exercised the walk-to arm — add one (broke_maker_makings_are_not_barter)")
	}
	if !sawCoPresent {
		t.Error("no scenario exercised the co-present arm — add one (broke_maker_at_store_no_spare_goods_hold_off)")
	}
}

// TestGoldensCoinPoorEmployerStaysSolicitable is the LLM-243 cross-scenario
// invariant — the hiring-side mirror of TestGoldensNoBuyCueWithoutMeansToPay. A
// co-present stranger employer that holds 0 coins BUT tradeable goods must NOT be
// foreclosed: it can still hire in kind (LLM-225), so a workless worker huddled
// with one must never be shown the "No one here can hire you" seek-work dead-end.
// A genuinely destitute employer (0 coins AND no goods) is NOT covered — it can
// hire no one and is allowed to dead-end — so the guard reuses the SAME
// holdsBarterableGoods predicate the sim gate (employerCanHireInKind) keys on, no
// drift. The other "could hire you" conditions are re-derived here independently
// of the employer's purse (present, not same household, no prior decline), so a
// regression that added a coin gate to the solicitable-audience check — re-breaking
// the barter path — would trip this rather than silently excusing itself. The
// subject is workless by the guard, so it shares a workplace with no one.
// Non-vacuous: worker_solicits_goods_rich_coin_poor_employer builds exactly this pair.
func TestGoldensCoinPoorEmployerStaysSolicitable(t *testing.T) {
	const deadEnd = "No one here can hire you"
	var sawCoinPoorGoodsHoldingEmployer bool
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			subject := snap.Actors[actorID]
			if !subjectIsWorker(subject) || subject.WorkStructureID != "" {
				return // subject isn't a workless worker — invariant N/A here
			}
			hud := snap.Huddles[subject.CurrentHuddleID]
			if hud == nil {
				return // no huddle audience — invariant N/A here
			}
			for peerID := range hud.Members {
				if peerID == actorID {
					continue
				}
				peer := snap.Actors[peerID]
				// The LLM-243 rule covers only a coin-poor employer that can still hire
				// IN KIND — 0 coins AND holds goods. A destitute peer (no goods) is not
				// covered; it may legitimately dead-end. holdsBarterableGoods is the same
				// predicate the sim gate (employerCanHireInKind) uses.
				if peer == nil || peer.Coins != 0 || !holdsBarterableGoods(snap, peer) {
					continue
				}
				// Coin-independent "could hire you" test, mirroring isSolicitableEmployer
				// but computed inline so a coin gate added to that predicate can't silence
				// the invariant: a co-present stranger (different household) with no prior
				// decline against this worker.
				if subject.HomeStructureID != "" && subject.HomeStructureID == peer.HomeStructureID {
					continue
				}
				if employerDeclinedSubject(snap, actorID, peerID) {
					continue
				}
				sawCoinPoorGoodsHoldingEmployer = true
				if strings.Contains(renderScenario(sc), deadEnd) {
					t.Errorf("scenario %q: co-present coin-poor goods-holding employer %s could hire the worker in kind, but the prompt renders the %q dead-end — an empty purse must not foreclose a goods-holding employer (LLM-243)", sc.name, peer.DisplayName, deadEnd)
				}
			}
		})
	}
	if !sawCoinPoorGoodsHoldingEmployer {
		t.Error("no scenario exercised a co-present coin-poor goods-holding solicitable employer — add one (worker_solicits_goods_rich_coin_poor_employer)")
	}
}

// TestEmptyPurseCannotPayCueTracksActorCoins is the LLM-153 cross-scenario invariant:
// the "you cannot pay" consequence appears in EXACTLY the scenarios whose rendered
// actor holds zero coins, and never with a positive balance. The expected branch is
// derived from the BUILT actor state (snap.Actors[actorID].Coins), NOT from the
// rendered purse text — so this independently asserts the cue tracks the actor's coins
// rather than merely pinning that the rendered line is internally self-consistent (it
// would catch a positive actor wrongly rendering the empty-purse form). The matrix must
// exercise both branches for the check to mean anything, so we also require one of each.
func TestEmptyPurseCannotPayCueTracksActorCoins(t *testing.T) {
	const cannotPayMarker = "you cannot pay for anything until you earn some"
	const barterMarker = "you may be able to offer goods you carry in trade"
	var sawEmptyNoGoods, sawEmptyWithGoods, sawPositive bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, _ := sc.build()
		actor := snap.Actors[actorID]
		if actor == nil {
			t.Fatalf("scenario %q: rendered actor %q missing from snapshot", sc.name, actorID)
		}
		out := renderScenario(sc)
		// The coin-only "cannot pay for anything" form appears iff the actor has 0
		// coins AND nothing to barter — a genuine payment dead-end. A 0-coin actor
		// holding goods gets the barter-aware form instead (LLM-222), so neither purse
		// line contradicts the satiation barter cue. Both flags are recomputed from raw
		// snapshot state (Coins + holdsBarterableGoods, the SAME predicate the buy-cue
		// gate reads), not the rendered text — so this asserts the cue tracks the actor.
		empty := actor.Coins == 0
		hasGoods := holdsBarterableGoods(snap, actor)
		wantCannotPay := empty && !hasGoods
		wantBarter := empty && hasGoods
		if has := strings.Contains(out, cannotPayMarker); has != wantCannotPay {
			t.Errorf("scenario %q: coins=%d hasGoods=%v cannot-pay cue=%v, want %v", sc.name, actor.Coins, hasGoods, has, wantCannotPay)
		}
		if has := strings.Contains(out, barterMarker); has != wantBarter {
			t.Errorf("scenario %q: coins=%d hasGoods=%v barter purse cue=%v, want %v", sc.name, actor.Coins, hasGoods, has, wantBarter)
		}
		switch {
		case wantCannotPay:
			sawEmptyNoGoods = true
		case wantBarter:
			sawEmptyWithGoods = true
		default:
			sawPositive = true
		}
	}
	if !sawEmptyNoGoods || !sawEmptyWithGoods || !sawPositive {
		t.Errorf("matrix must exercise all three purse branches: emptyNoGoods=%v emptyWithGoods=%v positive=%v", sawEmptyNoGoods, sawEmptyWithGoods, sawPositive)
	}
}

// TestLaborTieAnnotationTracksWorkerKin is the LLM-157 cross-scenario invariant: the
// "(your housemate)" / "(your workmate)" relationship annotation appears in EXACTLY the
// scenarios where the subject is a worker AND at least one of its addressable co-present members (huddle
// peers ∪ co-present, the same lists Render names) shares its household or workplace.
// The expectation is recomputed from raw ActorSnapshot fields (subjectIsWorker +
// sharesHousehold/sharesWorkplace) — NOT from the member's SolicitTie — so it independently
// asserts the annotation tracks co-residence/co-employment rather than pinning the render
// against its own marker. The matrix must exercise both branches to mean anything.
func TestLaborTieAnnotationTracksWorkerKin(t *testing.T) {
	const (
		housemateMarker = "(your housemate)"
		workmateMarker  = "(your workmate)"
	)
	var sawTied, sawUntied bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, warrants := sc.build()
		subj := snap.Actors[actorID]
		p := Build(snap, actorID, warrants)
		want := false
		if subjectIsWorker(subj) {
			audience := append(append([]HuddleMember{}, p.Surroundings.HuddleMembers...), p.Surroundings.CoPresent...)
			for _, m := range audience {
				if peer := snap.Actors[m.ID]; peer != nil && (sharesHousehold(subj, peer) || sharesWorkplace(subj, peer)) {
					want = true
					break
				}
			}
		}
		if want {
			sawTied = true
		} else {
			sawUntied = true
		}
		// Scope the search to the "## Around you" block where the annotation renders,
		// not the whole prompt — so the invariant can't pass/fail on the marker phrase
		// appearing in some unrelated cue or section later (code_review note).
		around := aroundYouSection(renderScenario(sc))
		has := strings.Contains(around, housemateMarker) || strings.Contains(around, workmateMarker)
		if has != want {
			t.Errorf("scenario %q: labor-tie annotation=%v, want %v", sc.name, has, want)
		}
	}
	if !sawTied || !sawUntied {
		t.Errorf("matrix must exercise both branches: sawTied=%v sawUntied=%v", sawTied, sawUntied)
	}
}

// aroundYouSection returns the rendered "## Around you" block (its header line
// excluded), up to the next "## " section header or the end of the prompt — so a
// surroundings-specific assertion can scope to where a cue actually renders instead
// of scanning the whole prompt and risking a false match elsewhere.
func aroundYouSection(rendered string) string {
	const head = "## Around you\n"
	i := strings.Index(rendered, head)
	if i < 0 {
		return ""
	}
	rest := rendered[i+len(head):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// growerAtStrippedBush reproduces the LLM-98 live shape: Prudence, a forager,
// stands on her own raspberry bush during her shift, having just stripped it to
// zero stock. It is the only gatherable within loiter reach, so
// ResolveGatherSource resolves it — the LLM-98 stock gate is what keeps the cue
// (and the gather tool) off an empty bush. No orders, no clock read → byte-stable.
func growerAtStrippedBush() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const prudenceID = sim.ActorID("prudence")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift, mid-harvest
	bushPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	prudence := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Prudence Hart",
		Role:             "forager",
		State:            sim.StateIdle,
		Pos:              bushPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            12,
		Needs:            map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{prudenceID: prudence},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"prudence_bush": {
				ID:            "prudence_bush",
				DisplayName:   "Raspberry Bush",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  prudenceID,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Attribute: "hunger", Amount: 0, GatherItem: "raspberries", AvailableQuantity: intp(0)},
				},
			},
		},
	}
	return snap, prudenceID, nil
}

// herbalistRangedWildForage / untaggedForagerNoRangedWildForage are the LLM-253
// pair: a forager low on sage (0 of cap 5) who owns no sage bush, with an UNOWNED
// Sage Bush (10 ripe) ~80 tiles to the northeast — far outside loiter reach, so it
// falls in the gap between the owner-only owned-bush cue and the proximity-only
// at-bush cue. The tagged herbalist gets the ranged "## Free sources you can gather from"
// cue; the untagged forager gets nothing (the tag gate). Farm ~tile (23,73), bush
// ~tile (70,9): dx=+47, dy=-64 → ~79 tiles, rendered "a long walk to the
// northeast". On shift, no orders, no clock-driven text → byte-stable.
func herbalistRangedWildForage() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return wildSageScenario(true)
}

func untaggedForagerNoRangedWildForage() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return wildSageScenario(false)
}

func wildSageScenario(tagged bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const prudenceID = sim.ActorID("prudence")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	var slugs []string
	if tagged {
		slugs = []string{sim.AttrForageRange}
	}
	prudence := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Prudence Ward",
		Role:             "herbalist",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 736, Y: 2336}.Tile(), // ~tile (23,73) — her farm
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            12,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"sage": 0},
		AttributeSlugs:   slugs,
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "sage", Source: sim.RestockSourceForage, Max: 5},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{prudenceID: prudence},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"sage_bush": {
				ID:            "sage_bush",
				DisplayName:   "Sage Bush",
				Pos:           sim.WorldPos{X: 2240, Y: 288}, // ~tile (70,9) — far NW commons
				OwnerActorID:  "",                            // UNOWNED — a wild commons source
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "sage", AvailableQuantity: intp(10)},
				},
			},
		},
	}
	return snap, prudenceID, nil
}

// generalStoreWaterForageAtWell is the LLM-254 two-row Well. The town Well is an
// UNOWNED commons carrying BOTH a public thirst drink row (Amount -8, slake-in-
// place) AND a yield-only water gather row (Amount 0, unset attribute — the clean
// LLM-264 yield row). Josiah Thorne (merchant, tagged sim.AttrForageRange, low on
// water with a `forage water` restock entry) stands ~10 tiles away and is thirsty,
// so the ONE unowned object surfaces in TWO independent cues at once with no owner-
// gate conflict: the free-drink satiation cue ("## What you can eat or drink", from
// the -8 thirst row) and the ranged forage cue ("## Free sources you can gather
// from", from the water yield row — 20 ready to gather). The forage stock count
// reads the yield row alone (forageStockForItem gates on Amount==0), so the -8
// drink row never pollutes it. Well ~tile (100,135), Josiah ~tile (108,141):
// dx=-8, dy=-6 → ~10 tiles, "a short walk to the northwest". On shift, no orders,
// no clock read → byte-stable.
func generalStoreWaterForageAtWell() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const josiahID = sim.ActorID("josiah")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	josiah := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Josiah Thorne",
		Role:             "merchant",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 108 * 32, Y: 141 * 32}.Tile(), // ~10 tiles SE of the Well
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            20,
		Needs:            map[sim.NeedKey]int{"thirst": sim.DefaultThirstRedThreshold},
		Inventory:        map[sim.ItemKind]int{"water": 0},
		AttributeSlugs:   []string{sim.AttrForageRange},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "water", Source: sim.RestockSourceForage, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water": {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"town_well": {
				ID:            "town_well",
				DisplayName:   "Well",
				Pos:           sim.WorldPos{X: 100 * 32, Y: 135 * 32}, // tile (100,135) — the real Well
				OwnerActorID:  "",                                     // UNOWNED commons — both cues require this
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					// Public drink row: slakes thirst in place, yields no inventory —
					// infinite, like the real commons Well. LLM-254 drops its gather_item
					// so it is drink-only, leaving the yield row below the sole water source.
					{Attribute: "thirst", Amount: -8},
					// Yield-only water row: forage-to-sell, unset attribute (LLM-264). A
					// forage_range holder draws a pail; drinking-in-place never touches this
					// counter (separate row, gated by Amount==0 in forageStockForItem).
					{Amount: 0, GatherItem: "water", AvailableQuantity: intp(20), MaxQuantity: intp(20)},
				},
			},
		},
	}
	return snap, josiahID, nil
}

// millerWithHiredHaulerKeepsWaterCue is the live LLM-621 shape. A miller stands
// at his own post, out of water (0 of a 20 cap, a `forage water` entry) with a
// full commons Well a short walk off — and his own hired hauler is in the room
// mid-job. The employer must still read "## Free sources you can gather from":
// a worker fulfilling a contract is not custom, so nothing here is an encounter
// to finish before stepping out.
//
// The pre-fix payload contradicted itself — "Lewis Walker is working a job for
// you" and a silenced harvest cue in the same prompt — and because a contract
// runs for hours the deferral covered the whole shift rather than a sale.
//
// What this fixture pins is the laboring-peer condition alone. It does NOT model
// why the live case mattered so much: Joseph Scott was the village's only actor
// with a `forage water` restock entry, so the whole water chain stalled behind
// one hire. That is a village-config fact (who holds the entry), not something a
// perception fixture can carry — don't read the golden as documenting it.
// Geometry does match the live line: the Well sits dx=+6, dy=-4 from the miller,
// ~7 tiles, rendering "a short walk to the northeast". Both positions go through
// WorldPos.Tile() so actor and object tiles share one conversion.
//
// ownsBush switches WHOSE water he is low on and can reach (LLM-622), which is the
// only difference between the two scenarios built from this fixture. false: the
// commons Well is the only source, so the wild cue renders and the at-post step-out
// line must claim nothing. true: he owns a spring of his own for the same item, so
// buildForage takes the owned branch (the wild scan runs only at bushCount == 0) and
// the owned wording is correct. Everything else — hour, hire, geometry, coins — is
// held identical so the golden diff is the ownership claim and nothing else.
//
// Fixed PublishedAt with WorkingUntil four hours out keeps the workers-for-me
// duration byte-stable; on shift, no orders, no price book.
func millerWithHiredHauler(ownsBush bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josephID = sim.ActorID("joseph")
		lewisID  = sim.ActorID("lewis")
		mill     = sim.StructureID("mill")
	)
	zero := 0
	start, end := 360, 1140 // 06:00–19:00
	now := 690              // 11:30 — on shift
	published := time.Date(2026, 8, 8, 11, 30, 0, 0, time.UTC)
	workingUntil := published.Add(4 * time.Hour)
	joseph := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Joseph Scott",
		Role:        "miller",
		State:       sim.StateIdle,
		// Both positions go through WorldPos.Tile() so the actor tile and the object
		// tile share one conversion — a raw TilePos here would be compared against a
		// converted object tile and the distance phrase would be nonsense.
		Pos:                sim.WorldPos{X: 152 * 32, Y: 55 * 32}.Tile(),
		BusinessownerState: &sim.BusinessownerState{},
		WorkStructureID:    mill,
		InsideStructureID:  mill, // AtOwnBusiness — the arm under test
		CurrentHuddleID:    "h1",
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		Coins:              55,
		// A hauler he has hired before — the live pairing. Acquaintance only
		// changes how the worker is NAMED; the arm under test reads m.Laboring.
		Acquaintances:  map[string]sim.Acquaintance{"Lewis Walker": {FirstInteractedAt: published.Add(-30 * 24 * time.Hour)}},
		Inventory:      map[sim.ItemKind]int{"water": 0},
		AttributeSlugs: []string{sim.AttrForageRange},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "water", Source: sim.RestockSourceForage, Max: 20},
		}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Lewis Walker",
		State:           sim.StateLaboring, // the cheap mirror the ledger scan gates on
		Pos:             sim.WorldPos{X: 152 * 32, Y: 55 * 32}.Tile(),
		CurrentHuddleID: "h1",
	}
	snap := &sim.Snapshot{
		PublishedAt:       published,
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{josephID: joseph, lewisID: lewis},
		Structures:        map[sim.StructureID]*sim.Structure{mill: plainStructure(mill, "Mill")},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			"h1": {ID: "h1", Members: map[sim.ActorID]struct{}{josephID: {}, lewisID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID: 1, WorkerID: lewisID, EmployerID: josephID,
				State: sim.LaborStateWorking, Reward: 4, DurationMin: 240,
				WorkingUntil: &workingUntil,
			},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water": {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"mill_well": {
				ID:            "mill_well",
				DisplayName:   "Well",
				Pos:           sim.WorldPos{X: 158 * 32, Y: 51 * 32},
				OwnerActorID:  "", // unowned commons
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "water", AvailableQuantity: intp(20), MaxQuantity: intp(20)},
				},
			},
		},
	}
	if ownsBush {
		// His own spring, same item and same yield as the Well. It needs BOTH the
		// ownership row and the remembered gather affordance: buildForage reads the
		// known-places set (LLM-79), not a world scan, so an owned object the grower
		// does not remember gathering at is invisible to the cue.
		snap.VillageObjects["mill_spring"] = &sim.VillageObject{
			ID:            "mill_spring",
			DisplayName:   "Spring",
			Pos:           sim.WorldPos{X: 158 * 32, Y: 51 * 32},
			OwnerActorID:  josephID,
			LoiterOffsetX: &zero,
			LoiterOffsetY: &zero,
			Refreshes: []*sim.ObjectRefresh{
				{Amount: 0, GatherItem: "water", AvailableQuantity: intp(20), MaxQuantity: intp(20)},
			},
		}
		joseph.KnownPlaces = map[sim.PlaceRef]*sim.KnownPlace{
			"mill_spring": {Ref: "mill_spring", Kind: sim.PlaceKindObject, Affordances: []string{"gather:water"}},
		}
	}
	return snap, josephID, nil
}

// millerAtDegradedMillOutOfWater is the LLM-634 live shape — the 2026-08-15 trigger
// of the four-day village-wide deadlock. Joseph Scott, the village's ONLY holder of
// a `forage water` entry, stands inside his own Mill, which has worn past the
// degrade threshold (650 >= 600) while he holds none of the 5 nails a mend takes.
// He is out of water (0 of a 20 cap) and the commons Well sits a short walk to the
// northeast, full. Two cues must render together: the degraded "## Your business"
// framing (the honest state of his shop) AND "## Free sources you can gather from"
// naming the Well — foraging spends no coin, and its yield is the upstream of the
// very nail that would mend the Mill (water → nail → mend), so a degraded stall
// must never silence it. Perception never gated the forage cue on degrade; the
// warrant side did (restock_tick.go firstActionableLowEntry, the LLM-304 `!degraded`
// guard), so the cue rendered on a wake that never came. This golden pins the
// perception half so a future "tidy-up" that mirrors the old warrant gate into
// buildForage cannot land silently; TestEvaluateRestock_DegradedForageStillWarrants
// (sim) pins the wake.
//
// Same geometry as millerWithHiredHauler (Well dx=+6, dy=-4 → "a short walk to the
// northeast"), but alone and no hire, so the diff against that golden is the worn
// business alone. Live default StallDegradedProducePct (LLM-446), so the "## Your
// business" line is the limping wording, not the legacy full block. On shift, no
// orders, no price book, no clock read — byte-stable.
func millerAtDegradedMillOutOfWater() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josephID = sim.ActorID("joseph")
		mill     = sim.StructureID("mill")
	)
	zero := 0
	start, end := 360, 1140 // 06:00–19:00
	now := 690              // 11:30 — on shift
	joseph := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Joseph Scott",
		Role:               "miller",
		State:              sim.StateIdle,
		Pos:                sim.WorldPos{X: 152 * 32, Y: 55 * 32}.Tile(),
		BusinessownerState: &sim.BusinessownerState{},
		WorkStructureID:    mill,
		InsideStructureID:  mill, // AtOwnBusiness — the "## Your business" cue's location arm
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		Coins:              55,
		Inventory:          map[sim.ItemKind]int{"water": 0, "nail": 0},
		AttributeSlugs:     []string{sim.AttrForageRange},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "water", Source: sim.RestockSourceForage, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		RestockReorderPct:         25,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		StallDegradedProducePct:   sim.DefaultStallDegradedProducePct,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{josephID: joseph},
		Structures:                map[sim.StructureID]*sim.Structure{mill: plainStructure(mill, "Mill")},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water": {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "water", Category: sim.ItemCategoryDrink},
			"nail":  {Name: "nail", Capabilities: []string{"portable"}, DisplayLabel: "nails"},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// The Mill as a wearable business: shares its id with the structure he
			// stands inside (structure-backed businesses do — LLM-266), owned by him,
			// worn past degrade.
			"mill": {
				ID:            "mill",
				DisplayName:   "Mill",
				Pos:           sim.WorldPos{X: 152 * 32, Y: 55 * 32},
				OwnerActorID:  josephID,
				Tags:          []string{sim.TagBusiness},
				Wear:          650,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
			"mill_well": {
				ID:            "mill_well",
				DisplayName:   "Well",
				Pos:           sim.WorldPos{X: 158 * 32, Y: 51 * 32},
				OwnerActorID:  "", // unowned commons
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "water", AvailableQuantity: intp(20), MaxQuantity: intp(20)},
				},
			},
		},
	}
	return snap, josephID, nil
}

// npcAtWellWithThirst builds a shared-VA NPC standing ON the town Well's loiter
// pin, holding an open-ended thirst dwell credit for the Well, with thirst set to
// the given value. LLM-376: the pre-fix arrival path could stamp such a credit
// with thirst already at the floor (the -8 gulp landing on 0), and the floor-hit
// terminator — which fires only on a preNeed>0 -> postNeed==0 transition — then
// never cleared it, so perception asserted "You are drinking at Well … until your
// thirst is quenched" forever and pinned the actor (Lewis at the well for 3+
// hours). thirst==0 exercises the satisfied-need gate in buildActiveDwellCredits;
// thirst>0 is the still-recovering control. Idle, on shift, no orders, no clock
// read → byte-stable.
func npcAtWellWithThirst(thirst int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const lewisID = sim.ActorID("lewis")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	wellTile := sim.WorldPos{X: 100 * 32, Y: 135 * 32}.Tile()
	lewis := &sim.ActorSnapshot{
		Kind:                  sim.KindNPCShared,
		DisplayName:           "Lewis Walker",
		State:                 sim.StateIdle,
		Pos:                   wellTile, // standing ON the Well loiter pin
		ScheduleStartMin:      &start,
		ScheduleEndMin:        &end,
		Coins:                 27,
		Needs:                 map[sim.NeedKey]int{"thirst": thirst},
		CurrentLoiterObjectID: "town_well", // co-location gate passes: on the Well pin
		DwellCredits: map[sim.DwellCreditKey]*sim.DwellCredit{
			{ObjectID: "town_well", Attribute: "thirst", Source: sim.DwellSourceObject}: {
				ObjectID:           "town_well",
				Attribute:          "thirst",
				Source:             sim.DwellSourceObject,
				LastCreditedAt:     time.Unix(1_700_000_000, 0).UTC(),
				DwellDelta:         -2,
				DwellPeriodMinutes: 30,
			},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"town_well": {
				ID:            "town_well",
				DisplayName:   "Well",
				Pos:           sim.WorldPos{X: 100 * 32, Y: 135 * 32}, // tile (100,135)
				OwnerActorID:  "",                                     // unowned commons
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Attribute: "thirst", Amount: -8}, // public slake-in-place drink row
				},
			},
		},
	}
	return snap, lewisID, nil
}

// npcQuenchedAtWellStaleCredit is the registered LLM-376 golden situation: the
// quenched (thirst 0) case, where the satisfied-need gate must suppress the drink
// dwell cue.
func npcQuenchedAtWellStaleCredit() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return npcAtWellWithThirst(0)
}

// TestGoldenQuenchedDwellSuppressesDrinkCue is the LLM-376 property guard: a
// recovery dwell whose need is already at the floor must NOT render as an active
// "You are drinking … until your thirst is quenched" cue (the immortal-credit pin
// that trapped Lewis at the well). Pairs the quenched fixture (cue absent) with an
// otherwise-identical still-thirsty fixture at the same Well (cue present), so the
// assertion pins the guard rather than a broken fixture.
func TestGoldenQuenchedDwellSuppressesDrinkCue(t *testing.T) {
	const drinkCue = "You are drinking at Well"
	const recoverClause = "the longer you stay the more you recover"

	quenched := renderScenario(perceptionScenario{name: "quenched_at_well", build: npcQuenchedAtWellStaleCredit})
	if strings.Contains(quenched, drinkCue) {
		t.Errorf("quenched NPC (thirst 0) still shows the drink dwell cue %q — the stale credit must be gated out:\n%s", drinkCue, quenched)
	}
	if strings.Contains(quenched, recoverClause) {
		t.Errorf("quenched NPC still shows the open-ended recovery clause %q:\n%s", recoverClause, quenched)
	}

	thirsty := renderScenario(perceptionScenario{name: "thirsty_at_well", build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
		return npcAtWellWithThirst(5)
	}})
	if !strings.Contains(thirsty, drinkCue) {
		t.Errorf("still-thirsty NPC (thirst 5) lost the drink dwell cue %q — the guard must fire only at the floor:\n%s", drinkCue, thirsty)
	}
}

// TestRangedWildForageRequiresTag is the LLM-253 tag-gate unit: the ranged
// "## Free sources you can gather from" cue (with its move_to handle and qualitative
// distance+direction) renders for a forager carrying sim.AttrForageRange and does
// NOT render for the same fixture without the tag. Asserts the render directly, so
// the gate is pinned independently of whether a golden was regenerated.
func TestRangedWildForageRequiresTag(t *testing.T) {
	const header = "## Free sources you can gather from"
	tagged := renderScenario(perceptionScenario{name: "tagged", build: herbalistRangedWildForage})
	if !strings.Contains(tagged, header) {
		t.Fatalf("tagged herbalist: expected the ranged wild-forage section %q, got:\n%s", header, tagged)
	}
	if !strings.Contains(tagged, `move_to with destination "sage_bush"`) {
		t.Errorf("tagged herbalist: expected a move_to handle to the Sage Bush, got:\n%s", tagged)
	}
	if !strings.Contains(tagged, "a long walk to the northeast") {
		t.Errorf("tagged herbalist: expected the qualitative distance+direction phrase, got:\n%s", tagged)
	}
	untagged := renderScenario(perceptionScenario{name: "untagged", build: untaggedForagerNoRangedWildForage})
	if strings.Contains(untagged, header) {
		t.Errorf("untagged forager: ranged wild-forage section must NOT render without sim.AttrForageRange, got:\n%s", untagged)
	}
}

// TestGoldensRangedWildForageRequiresTag is the LLM-253 cross-scenario invariant:
// the ranged "## Free sources you can gather from" section may render only for a subject
// carrying sim.AttrForageRange. Runs over the whole matrix so a future cue can't
// leak omniscient wild-source awareness to an untagged actor in any situation, not
// just the one pair pinned above.
func TestGoldensRangedWildForageRequiresTag(t *testing.T) {
	const header = "## Free sources you can gather from"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			tagged := false
			if a != nil {
				for _, slug := range a.AttributeSlugs {
					if slug == sim.AttrForageRange {
						tagged = true
						break
					}
				}
			}
			if tagged {
				return // subject carries the tag — the cue is permitted
			}
			if out := renderScenario(sc); strings.Contains(out, header) {
				t.Errorf("scenario %q: subject lacks sim.AttrForageRange but the prompt renders the ranged wild-forage section %q (LLM-253)", sc.name, header)
			}
		})
	}
}

// TestGoldensStepOutClaimsOnlyOwnedBushes is the LLM-622 cross-scenario invariant:
// the at-post step-out reframe may call the errand's target "your own bushes" ONLY
// in a prompt that also carries the owned-bush section. Live, a miller read that
// wording directly above "## Free sources you can gather from", which says no one
// owns them — one line called the commons Well his property and the next did not.
//
// Keyed on the RENDERED pair rather than on DutySteerView.ForageErrand, on purpose:
// what has to hold is that the two sections of one prompt agree about ownership, and
// a future reframe that reaches the owned wording by some other route would satisfy
// a flag check while still contradicting the section beneath it. Runs over the whole
// matrix, so a new fixture whose only forage source is unowned is covered without
// anyone remembering to add it.
func TestGoldensStepOutClaimsOnlyOwnedBushes(t *testing.T) {
	const (
		ownedClaim   = "step out to your own bushes"
		ownedSection = "## Your bushes to harvest"
	)
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			out := renderScenario(sc)
			if strings.Contains(out, ownedClaim) && !strings.Contains(out, ownedSection) {
				t.Errorf("scenario %q: the step-out line claims %q but the prompt carries no %q — it is sending the subject to a source it does not own (LLM-622)", sc.name, ownedClaim, ownedSection)
			}
		})
	}
}

// stallWearSnapshot builds a one-business, one-actor snapshot for the LLM-118
// cues. The actor stands on the business's loiter pin; the object is a tagged,
// owned business (TagBusiness — the LLM-247 gate) worn to `wear`. ownerID is the
// owner (the perceiving actor for the owner cues; a different actor for the
// passerby cue). nails seeds the actor's pack. No orders, no clock read →
// byte-stable.
func stallWearSnapshot(actorID, ownerID sim.ActorID, displayName, role string, wear, nails int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	stallPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	actor := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      displayName,
		Role:             role,
		State:            sim.StateIdle,
		Pos:              stallPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            8,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"nail": nails},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		// The live default (LLM-446): degraded businesses produce slowed, not
		// blocked — the degraded scenarios pin the wording production actually
		// runs. The legacy pct-0 block has its own explicit scenario.
		StallDegradedProducePct: sim.DefaultStallDegradedProducePct,
		Actors:                  map[sim.ActorID]*sim.ActorSnapshot{actorID: actor},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"market_stall": {
				ID:            "market_stall",
				DisplayName:   "Blacksmith",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  ownerID,
				Tags:          []string{sim.TagBusiness},
				Wear:          wear,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, actorID, nil
}

// ownerAtWornStall: the owner at his own worn stall, short on nails — the buy-then-
// mend steer. wear 450 (>= repair 400, < degrade 600), 2 nails (< 5 needed).
func ownerAtWornStall() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return stallWearSnapshot("ezekiel", "ezekiel", "Ezekiel Crane", "blacksmith", 450, 2)
}

// ownerAtDegradedStall: the owner at his own degraded stall with nails in hand —
// the slowed-not-shut framing plus "use the repair tool now to fix it" steer at
// the live default pct (LLM-446). wear 650 (>= degrade 600).
func ownerAtDegradedStall() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return stallWearSnapshot("ezekiel", "ezekiel", "Ezekiel Crane", "blacksmith", 650, 5)
}

// ownerAtDegradedStallProduceBlocked: the same degraded stall under the legacy
// pct-0 operator setting (LLM-446 keeps it dialable) — pins the full-block
// "too worn to keep stock … can't take in shelf stock from suppliers or make more" wording so
// the legacy mode stays reachable and worded truthfully.
func ownerAtDegradedStallProduceBlocked() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := stallWearSnapshot("ezekiel", "ezekiel", "Ezekiel Crane", "blacksmith", 650, 5)
	snap.StallDegradedProducePct = 0
	return snap, actorID, warrants
}

// degradedSmithShortOfOwnNails is the LLM-446 live-bug fixture — the 2026-07-16
// Ezekiel↔Josiah "three more minutes" loop. The village's SOLE nail producer
// stands at his own DEGRADED Blacksmith holding 0 of the 5 nails his own repair
// consumes, a nail batch mid-flight. Before the fix the engine froze the batch
// (perception re-promised the frozen "about 3 minutes of work left" every tick
// for two hours) while the repair cue steered him to BUY nails no one else
// sells — a structural deadlock. The golden pins the three ways out rendering
// together: the slowed-batch line (the clock is honest), the "nails are your
// own work: forge what you're short" mend steer (not a buy errand), and the
// slowed-not-shut "## Your business" framing.
func degradedSmithShortOfOwnNails() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := stallWearSnapshot("ezekiel", "ezekiel", "Ezekiel Crane", "blacksmith", 650, 0)
	smith := snap.Actors[actorID]
	smith.RestockPolicy = &sim.RestockPolicy{Restock: []sim.RestockEntry{
		{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
	}}
	smith.ProductionItem = "nail"
	smith.ProductionRemainingSeconds = 204 // the live frozen clock, now honest: it advances slowed
	return snap, actorID, warrants
}

// degradedSmithShortOfOwnNailsProduceBlocked is the code_review regression arm:
// the same sole-nail-producer fixture under the legacy pct-0 full block, where
// StartProductionCycle REJECTS — so the forge-your-own steer must NOT render
// (it would goad a production the engine refuses; the plain shortfall fallback
// renders instead) and the in-flight batch line must state the halt rather than
// re-promising the frozen clock.
func degradedSmithShortOfOwnNailsProduceBlocked() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := degradedSmithShortOfOwnNails()
	snap.StallDegradedProducePct = 0
	return snap, actorID, warrants
}

// ownerAtDegradedStoreConserving is the LLM-301 fixture — the live 2026-07-06 Josiah
// Thorne "the Smithy" case. The owner stands at his own DEGRADED General Store (wear
// 650 >= degrade 600, shut for restock) carrying 0 of the 5 nails a mend takes, with
// 1 coin (below the 10-coin MerchantCoinFloor) and 17 unsold flour on the shelf (past
// the 8-unit dead-stock overstock floor) — so merchantConserve is Active — while NO
// nail supplier resolves (none exists here; the live case dropped the smith on
// affordability — either way NailVendors is empty). The golden pins the sell-first
// soften in the vendor-less fallback instead of the old destination-less "buy more
// from the smith" goad, which the live model answered by hallucinating a "the Smithy"
// move_to target and burning its whole turn retrying it.
func ownerAtDegradedStoreConserving() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 720              // 12:00 — on shift, at his post
	storePin := sim.WorldPos{X: 100, Y: 100}.Tile()
	josiah := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Josiah Thorne",
		Role:             "shopkeeper",
		State:            sim.StateIdle,
		Pos:              storePin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            1,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"nail": 0, "flour": 17},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "flour", Source: sim.RestockSourceBuy, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		MerchantCoinFloor:         10,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		StallDegradedProducePct:   sim.DefaultStallDegradedProducePct, // live default (LLM-446)
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"josiah": josiah},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"general_store": {
				ID:            "general_store",
				DisplayName:   "General Store",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "josiah",
				Tags:          []string{sim.TagBusiness},
				Wear:          650,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "josiah", nil
}

// ownerConservingWithNailSupplier is the LLM-301 code_review arm: conserve active
// WHILE a nail supplier survives findItemVendors. Elizabeth is coin-poor (6 < the
// 10-coin MerchantCoinFloor) and overstocked (17 flour, past the 8-unit dead-stock
// floor) — conserve Active — but she has never bought nails from Ezekiel, so his
// price is unknown and the LLM-216 affordability drop KEEPS him (patronage earns the
// number). The working-capital floor and the affordability drop are different
// filters, so this state is reachable live; the golden pins that conserve WINS —
// the sell-first soften renders, not the "Use move_to to reach a supplier" walk-to
// goad — keeping this cue from pushing a buy while "## Restocking" says hold off.
func ownerConservingWithNailSupplier() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := ownerAtWornStallWithNailSupplier()
	owner := snap.Actors[actorID]
	owner.Coins = 6
	owner.Inventory["flour"] = 17
	owner.RestockPolicy = &sim.RestockPolicy{Restock: []sim.RestockEntry{
		{Item: "flour", Source: sim.RestockSourceBuy, Max: 20},
	}}
	snap.MerchantCoinFloor = 10
	return snap, actorID, warrants
}

// ownerAtWornStallWithNailSupplier is the LLM-274 fixture, modeled on the live
// 2026-07-04 Elizabeth Ellis case: the owner stands at her own worn Ellis Farm
// (wear 450 >= repair 400, < degrade 600) carrying 0 of the 5 nails a mend needs,
// while a SEPARATE open nail supplier exists — Ezekiel, the blacksmith, stationed
// at the Blacksmith structure holding 21 nails. findItemVendors resolves the smith
// as a walk-to destination (he isn't the buyer, works at a resolvable non-farm
// structure, holds nails, and Elizabeth has no remembered-shut/unaffordable strike
// against him), so the "## Your business" no-nails steer names it with a
// structure_id instead of the dead-end "buy more from the smith". Ezekiel is placed
// far from Elizabeth so he is a supplier-of-record, not a co-present seller — the
// cue names his WORKPLACE, which is exactly the move_to affordance the live model
// lacked. The foil is ownerAtWornStall (no other smith → generic sentence kept).
func ownerAtWornStallWithNailSupplier() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	farmPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	elizabeth := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Elizabeth Ellis",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              farmPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            39,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"nail": 0},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Ezekiel Crane",
		Role:             "blacksmith",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 2000, Y: 2000}.Tile(), // far from Elizabeth — a supplier of record, not co-present
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		WorkStructureID:  "blacksmith",
		Coins:            0,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"nail": 21},
		// The smith PRODUCES nails — the LLM-252 supplier-of-record gate
		// (isRestockSupplierOf) only names producers/foragers (or the distributor), so a
		// vendor merely holding nails from a past buy would NOT resolve here.
		RestockPolicy: producePolicy("nail", 40),
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"elizabeth": elizabeth, "ezekiel": ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			"blacksmith": plainStructure("blacksmith", "Blacksmith"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_farm": {
				ID:            "ellis_farm",
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "elizabeth",
				Tags:          []string{sim.TagBusiness},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "elizabeth", nil
}

// ownerAtWornStallNailKeeperCoPresent is the LLM-504 fixture, modeled on the live
// 2026-07-21 John Ellis case (virtual_agent_calls 110823): the tavernkeeper stands
// inside his own worn tavern (at post), one nail short of the five a mend takes,
// while Ezekiel — the nail-producing smith whose WORKPLACE is the off-scene
// Blacksmith — is standing inside the tavern with him. findItemVendors still
// resolves the Blacksmith as the walk-to destination (structural vendorship rides
// WorkStructureID, not the seller's position), but the representative seller is in
// the buyer's conversational scope, so the bullet carries his name: "buy from
// Blacksmith (Ezekiel Crane) (destination: blacksmith)". Before LLM-504 the bullet
// was name-free and the cue marched John toward the empty smithy while its keeper
// drank at his bar.
func ownerAtWornStallNailKeeperCoPresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	tavernPos := sim.WorldPos{X: 640, Y: 640}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		Pos:               tavernPos.Tile(),
		InsideStructureID: "ellis_tavern",
		WorkStructureID:   "ellis_tavern", // at his post — the duty steer renders its LLM-491 step-out form
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 4}, // one short of the 5 a repair takes (the live shortfall)
		// Ezekiel is within earshot (the world-side audience set) and acquainted, so
		// "## Around you" names him — the premise the walk-to name links against.
		ColocatedAudienceIDs: []sim.ActorID{"ezekiel"},
		Acquaintances:        map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               tavernPos.Tile(),
		InsideStructureID: "ellis_tavern", // in the buyer's structure scope — the co-presence that names him
		WorkStructureID:   "blacksmith",
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 21},
		// The smith PRODUCES nails — the LLM-252 supplier-of-record gate names only
		// producers/foragers (or the distributor), not a holder of past-bought stock.
		RestockPolicy: producePolicy("nail", 40),
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"john": john, "ezekiel": ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			"ellis_tavern": plainStructure("ellis_tavern", "Ellis Tavern"),
			"blacksmith":   plainStructure("blacksmith", "Blacksmith"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_tavern": {
				ID:            "ellis_tavern",
				DisplayName:   "Ellis Tavern",
				Pos:           tavernPos,
				OwnerActorID:  "john",
				Tags:          []string{sim.TagBusiness},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "john", nil
}

// TestStallRepairWalkToNamesCoPresentKeeper is the LLM-504 semantic assertion the
// byte-pinned goldens carry implicitly: the walk-to nail bullet names the supplier's
// keeper IFF that keeper is co-present with the buyer. Runs the two fixture arms
// end-to-end (Build → Render) so it exercises the real findItemVendors co-presence
// check, not a hand-built view, and stays green through unrelated golden churn.
func TestStallRepairWalkToNamesCoPresentKeeper(t *testing.T) {
	snap, actorID, warrants := ownerAtWornStallNailKeeperCoPresent()
	out := combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
	if !strings.Contains(out, "buy from Blacksmith (Ezekiel Crane) (destination: blacksmith)") {
		t.Errorf("co-present arm: the walk-to bullet must name the co-present keeper (LLM-504), got:\n%s", out)
	}

	snap, actorID, warrants = ownerAtWornStallWithNailSupplier()
	out = combinedPrompt(Render(Build(snap, actorID, warrants), DefaultRenderConfig()))
	if !strings.Contains(out, "buy from Blacksmith (destination: blacksmith)") {
		t.Errorf("absent arm: the walk-to bullet must still render, name-free, when the keeper is off-scene, got:\n%s", out)
	}
	if strings.Contains(out, "(Ezekiel Crane)") {
		t.Errorf("absent arm: an off-scene keeper must NOT be named on the walk-to bullet (LLM-504), got:\n%s", out)
	}
}

// ellisFarmWorn is the shared LLM-277 fixture spine: Elizabeth Ellis owns the worn
// (wear 450, past the 400 repair threshold) Ellis Farm and works it as her post
// (WorkStructureID → the ellis_farm structure, which shares the business object id),
// on-shift at 10:00. Ezekiel Crane is the nail-producing smith. The caller positions
// Elizabeth (at the smith in a huddle, walking, etc.), sets her nail count, and whether
// Ezekiel shares her huddle, to exercise the three off-post arms. Elizabeth having a
// resolvable work anchor while standing off it is what makes the to-work nag fire in
// the baseline, so a golden that shows NO nag proves the errand suppressed it.
func ellisFarmWorn(elizabethPos sim.WorldPos, insideStructure sim.StructureID, huddle sim.HuddleID, nails int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	smithPos := sim.WorldPos{X: 2000, Y: 2000}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               elizabethPos.Tile(),
		InsideStructureID: insideStructure,
		WorkStructureID:   "ellis_farm", // her post — she is standing off it below
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             39,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": nails},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               smithPos.Tile(),
		InsideStructureID: "blacksmith",
		WorkStructureID:   "blacksmith",
		CurrentHuddleID:   huddle, // "" when the caller wants him a supplier-of-record, not co-present
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 21},
		// The smith PRODUCES nails — the LLM-252 supplier-of-record gate names only
		// producers/foragers (or the distributor), not a holder of past-bought stock.
		RestockPolicy: producePolicy("nail", 40),
		Acquaintances: map[string]sim.Acquaintance{"Elizabeth Ellis": {}},
	}
	// Ezekiel shares Elizabeth's huddle only when the caller passes a non-empty huddle
	// AND he is at it; for the walking/enough-nails arms the caller passes "" so he is a
	// distant supplier of record. A blank huddle on Ezekiel keeps him out of her huddle.
	if huddle == "" {
		ezekiel.CurrentHuddleID = ""
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"elizabeth": elizabeth, "ezekiel": ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			"blacksmith": plainStructure("blacksmith", "Blacksmith"),
			"ellis_farm": plainStructure("ellis_farm", "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_farm": {
				ID:            "ellis_farm",
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "elizabeth",
				Tags:          []string{sim.TagBusiness},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "elizabeth", nil
}

// ownerOffPostAtSmithShortNails is the LLM-277 co-present arm — the live 2026-07-04
// failure the ticket fixes. Elizabeth, 0 nails, has walked off her worn farm and shares
// the smith's huddle with Ezekiel (21 nails). On-shift and off her post, so the to-work
// nag WOULD fire — but the active nail-buy errand suppresses it. The golden pins the
// off-post "## Nails to mend your business" cue with the co-present pay_with_item
// buy-here imperative naming Ezekiel, and the ABSENCE of any return-to-post steer.
func ownerOffPostAtSmithShortNails() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return ellisFarmWorn(sim.WorldPos{X: 2000, Y: 2000}, "blacksmith", "smith_huddle", 0)
}

// ownerOffPostShortNailsWalking is the LLM-277 walk-to arm: Elizabeth, 0 nails, is off
// her worn farm and NOT co-present with the smith (no shared huddle, Ezekiel far off).
// The golden pins the same "## Nails to mend your business" cue naming the walk-to
// destination ("buy from Blacksmith (destination: blacksmith)") and no return-to-post
// steer — the "while away" half of the errand that persists across the walk (LLM-274
// named this destination for the at-farm cue; here it rides the whole trip).
func ownerOffPostShortNailsWalking() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return ellisFarmWorn(sim.WorldPos{X: 1000, Y: 1000}, "", "", 0)
}

// ownerOffPostEnoughNails is the LLM-277 negative arm: Elizabeth is off her worn farm
// but already carries enough nails (5 == NAILS_PER_REPAIR) to mend it, so there is no
// buy errand — the "## Nails to mend your business" cue is absent. With no errand to
// defer, the to-work nag correctly fires (she should head back to her post to mend),
// the foil that shows the suppression is conditional on an actual shortfall.
func ownerOffPostEnoughNails() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return ellisFarmWorn(sim.WorldPos{X: 1000, Y: 1000}, "", "", 5)
}

// keeperConservingOwesNailRepair is the LLM-297 invariant anchor — the live 2026-07-06
// Josiah Thorne standoff. Josiah is a shopkeeper whose working capital has collapsed
// (1 coin, below the 10-coin MerchantCoinFloor, with 20 unsold cloth on the shelf), so
// merchantConserve is Active. He owns the worn General Store (his post) but has walked
// off it to the Blacksmith, where he shares Ezekiel Crane's huddle — Ezekiel produces
// both nails (10 held, the repair supply) and coal (15 held, Josiah's low restock
// input). So two standing sections would fire at once: "## Restocking" flips to the
// coin-poor "Hold off buying more for now" steer (coal is low and Ezekiel is a
// co-present coal supplier, so the item survives the LLM-216 drop), while the off-post
// "## Nails to mend your business" errand puts a nail seller in front of him. The fix
// softens the nail goad under conserve, so the prompt no longer carries "Buy it now"
// beside "Hold off buying more".
func keeperConservingOwesNailRepair() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID  = sim.ActorID("josiah")
		ezekielID = sim.ActorID("ezekiel")
		store     = sim.StructureID("general_store")
		smithy    = sim.StructureID("blacksmith")
		huddleID  = sim.HuddleID("forge_huddle")
	)
	zero := 0
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, off his post at the smith
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 200, Y: 200},
		InsideStructureID: smithy,
		WorkStructureID:   store, // his post — he is standing off it, at the smith
		CurrentHuddleID:   huddleID,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		// Overstocked cloth is the conserve trigger; low coal is the surviving Restocking
		// item (Ezekiel supplies it co-present); 0 nails is the repair shortfall.
		Inventory: map[sim.ItemKind]int{"cloth": 20, "coal": 1, "nail": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cloth", Source: sim.RestockSourceBuy, Max: 24},
			{Item: "coal", Source: sim.RestockSourceBuy, Max: 12},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 201, Y: 200},
		InsideStructureID: smithy,
		WorkStructureID:   smithy,
		CurrentHuddleID:   huddleID,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 10, "coal": 15},
		// Produces nails AND coal, so the LLM-252 supplier-of-record gate names him for
		// each — the nail repair-buy and Josiah's coal restock both resolve to him.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 40},
			{Item: "coal", Source: sim.RestockSourceProduce, Max: 40},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:               published,
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		MerchantCoinFloor:         10,
		RestockReorderPct:         25,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			store:  plainStructure(store, "General Store"),
			smithy: plainStructure(smithy, "Blacksmith"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{josiahID: {}, ezekielID: {}}},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {
				ID:            sim.VillageObjectID(store),
				DisplayName:   "General Store",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  josiahID,
				Tags:          []string{sim.TagBusiness},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"cloth": {Name: "cloth", DisplayLabel: "cloth"},
			"coal":  {Name: "coal", DisplayLabel: "coal"},
			"nail":  {Name: "nail", DisplayLabel: "nails"},
		},
	}
	return snap, josiahID, nil
}

// innkeeperInputPileNotOverstock is the LLM-462 fixture, drawn from the live 2026-07-18
// Hannah Boggs deadlock: an innkeeper with 2 coins (below the 10-coin MerchantCoinFloor)
// whose porridge shelf is BARE, holding 19 water — a porridge ingredient, 5 to a batch —
// and 1 flour, the input her line has actually run out of.
//
// Before LLM-462 the water pile alone cleared the dead-stock floor, so she read as
// coin-poor-AND-stock-rich, "## Restocking" flipped to the hold-off steer, and the sim
// mirror suppressed every buy warrant she had. She was never woken to fetch flour again
// and her busiest line (20 porridge sales to 12 villagers that week) simply stopped.
//
// Raw material is not merchandise, so she is NOT conserving here: the golden pins the
// ORDINARY buy cue with the miller's walk-to destination, and the ABSENCE of the
// "Hold off buying more for now" lead. Foil of keeper_conserving_owes_nail_repair, whose
// overstocked cloth is a pure resale good (that keeper has no produce entry at all, so
// nothing of his is an input) and who therefore still conserves.
func innkeeperInputPileNotOverstock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		josephID = sim.ActorID("joseph")
		inn      = sim.StructureID("inn")
		mill     = sim.StructureID("mill")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at her own post
	published := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 300, Y: 300},
		InsideStructureID: inn,
		WorkStructureID:   inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             2,
		Needs:             map[sim.NeedKey]int{},
		// The live inventory: wares bare, raw material piled, the binding input at 1.
		Inventory: map[sim.ItemKind]int{"water": 19, "flour": 1, "porridge": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "water", Source: sim.RestockSourceBuy, Max: 10},
			{Item: "flour", Source: sim.RestockSourceBuy, Max: 6},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Joseph Scott": {}},
	}
	joseph := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Joseph Scott",
		Role:              "miller",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 900, Y: 900},
		InsideStructureID: mill,
		WorkStructureID:   mill,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"flour": 20},
		// Produces flour, so he passes the LLM-252 supplier-of-record gate as her walk-to
		// destination. The Mill is deliberately NOT wholesaler-tagged here — a wholesale
		// tag would drop him from her vendor scan and make the fixture vacuous.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "flour", Source: sim.RestockSourceProduce, Max: 24},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Hannah Boggs": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:               published,
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		MerchantCoinFloor:         10,
		RestockReorderPct:         25,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, josephID: joseph},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:  plainStructure(inn, "The Inn"),
			mill: plainStructure(mill, "The Mill"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{},
		// Porridge is what makes water and flour raw material rather than wares — the
		// whole point of the fixture, so the recipe has to be real here.
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 4, RateQty: 4, RatePerHours: 1,
				Inputs:         []sim.RecipeInput{{Item: "flour", Qty: 2}, {Item: "water", Qty: 5}},
				WholesalePrice: 1, RetailPrice: 2},
			"flour": {OutputItem: "flour", OutputQty: 1, RateQty: 4, RatePerHours: 1,
				WholesalePrice: 2, RetailPrice: 3},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water":    {Name: "water", DisplayLabel: "water"},
			"flour":    {Name: "flour", DisplayLabel: "flour"},
			"porridge": {Name: "porridge", DisplayLabel: "porridge"},
		},
	}
	return snap, hannahID, nil
}

// ownerStandoffDeclinedNails is the LLM-297 standoff arm: the ownerOffPostAtSmithShortNails
// co-present setup (Elizabeth off her worn farm, sharing the smith's huddle with Ezekiel,
// 0 nails), but with two prior nail offers to Ezekiel already declined IN THIS HUDDLE on
// the pay ledger. Elizabeth is a plain farmer (no restock policy → merchantConserve never
// fires), so the softening here is driven purely by the dead-ended negotiation
// (nailStandoffToSeller), proving the standoff path independent of conserve mode.
func ownerStandoffDeclinedNails() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := ellisFarmWorn(sim.WorldPos{X: 2000, Y: 2000}, "blacksmith", "smith_huddle", 0)
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	snap.PublishedAt = published
	// Thin purse — the live standoff is purse-driven (the smith declines a short purse),
	// which is why both offers were turned down.
	snap.Actors[actorID].Coins = 3
	// Two declined nail offers to Ezekiel in the current huddle, resolved a minute ago —
	// the threshold the cue reads as a standoff, and inside recentlyResolvedOfferWindow so
	// the recency guard counts them. Declined is terminal, so no ExpiresAt is needed.
	resolved := published.Add(-1 * time.Minute)
	// Amount 3 on each: the offers carried her whole thin purse (a live offer always
	// carries coins and/or goods), so the LLM-296 settled-offer line reads "Your offer
	// of 3 coins …" rather than the fixture artifact "of nothing".
	snap.PayLedger = map[sim.LedgerID]*sim.PayLedgerEntry{
		1: {ID: 1, BuyerID: actorID, SellerID: "ezekiel", ItemKind: sim.NailItemKind, Qty: 5, Amount: 3, State: sim.PayLedgerStateDeclined, HuddleID: "smith_huddle", ResolvedAt: resolved},
		2: {ID: 2, BuyerID: actorID, SellerID: "ezekiel", ItemKind: sim.NailItemKind, Qty: 5, Amount: 3, State: sim.PayLedgerStateDeclined, HuddleID: "smith_huddle", ResolvedAt: resolved},
	}
	return snap, actorID, warrants
}

// ownerStandoffAgedDeclinesNails is the LLM-510 latch arm: the ownerStandoffDeclinedNails
// setup with both declines aged well past recentlyResolvedOfferWindow while the huddle runs
// on. The decline counter takes the huddle's whole lifetime, so the standoff still reads
// blocked-terms; before the fix the recency filter dropped these as stale and the "Buy it
// now" goad returned between wake-backoff-paced re-offers (the live 13-decline nail loop).
// The "## Recently settled offers" view keeps its own 3-minute window, so that section
// drops out here — matching live, where only the freshest decline shows there while the
// standoff still holds.
func ownerStandoffAgedDeclinesNails() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := ownerStandoffDeclinedNails()
	aged := snap.PublishedAt.Add(-20 * time.Minute)
	for _, e := range snap.PayLedger {
		e.ResolvedAt = aged
	}
	return snap, actorID, warrants
}

// ownerShortNailsSellerLowStock is the LLM-297 stock-cap arm: the same co-present setup,
// but Ezekiel holds only 2 nails against the 5 a repair needs. Affordable and no prior
// offers, so the buy still stands — the render caps the ask at his stock instead of
// goading the full shortfall (the live case: the smith held only 1 nail).
func ownerShortNailsSellerLowStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := ellisFarmWorn(sim.WorldPos{X: 2000, Y: 2000}, "blacksmith", "smith_huddle", 0)
	snap.Actors["ezekiel"].Inventory[sim.NailItemKind] = 2
	return snap, actorID, warrants
}

// farmOwnerOwesUpkeepSellerPresent is the LLM-277 farm-upkeep co-present arm: Elizabeth
// owes 3 upkeep shovels (95 coins, floor 30, band 20) and holds none, and shares a
// huddle with Ezekiel — the shovel-producing smith holding 12 — at the Blacksmith. The
// golden pins the "## Farm upkeep" cue flipping from the walk-to list to the concrete
// co-present pay_with_item buy-here imperative naming Ezekiel, the same fast-path the
// nail repair-buy uses. Its foil is farmOwnerOwesUpkeepWithShovelSupplier (smith far
// off → the walk-to destination is named instead).
func farmOwnerOwesUpkeepSellerPresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const actorID = sim.ActorID("elizabeth")
	zero := 0
	start, end := 360, 1080
	now := 600
	huddle := sim.HuddleID("smith_huddle")
	smithPos := sim.WorldPos{X: 2000, Y: 2000}.Tile()
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               smithPos,
		InsideStructureID: "blacksmith",
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             95,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               smithPos,
		InsideStructureID: "blacksmith",
		WorkStructureID:   "blacksmith",
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"shovel": 12},
		RestockPolicy:     producePolicy("shovel", 40),
		Acquaintances:     map[string]sim.Acquaintance{"Elizabeth Ellis": {}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		Assets:                   emptyAssetSet,
		FarmUpkeepFloor:          30,
		FarmUpkeepCoinsPerShovel: 20,
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{actorID: elizabeth, "ezekiel": ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			"blacksmith": plainStructure("blacksmith", "Blacksmith"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_farm": {
				ID:            "ellis_farm",
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  actorID,
				Tags:          []string{sim.TagFarm},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, actorID, nil
}

// farmOwnerOwesUpkeepSellerLowStock is the LLM-299 stock-cap arm: the
// farmOwnerOwesUpkeepSellerPresent co-present setup (Elizabeth owes 3 upkeep shovels,
// sharing the smith's huddle with Ezekiel), but Ezekiel holds only 1 shovel against the 3
// she needs. Affordable and no prior offers, so the buy still stands — the render caps the
// ask at his stock instead of goading the full shortfall (the shovel twin of the live
// smith-held-only-1-nail case).
func farmOwnerOwesUpkeepSellerLowStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := farmOwnerOwesUpkeepSellerPresent()
	snap.Actors["ezekiel"].Inventory[sim.ShovelItemKind] = 1
	return snap, actorID, warrants
}

// farmOwnerStandoffDeclinedShovels is the LLM-299 standoff arm: the
// farmOwnerOwesUpkeepSellerPresent co-present setup (Elizabeth owes 3 upkeep shovels,
// sharing the smith's huddle with Ezekiel, who is well-stocked at 12), but with two prior
// shovel offers to Ezekiel already declined IN THIS HUDDLE on the pay ledger. Elizabeth is a
// plain farmer (no restock policy → merchantConserve never fires), so the softening here is
// driven purely by the dead-ended negotiation (coPresentBuyStandoff), proving the standoff
// path independent of conserve mode — the shovel twin of ownerStandoffDeclinedNails.
func farmOwnerStandoffDeclinedShovels() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := farmOwnerOwesUpkeepSellerPresent()
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	snap.PublishedAt = published
	// Two declined shovel offers to Ezekiel in the current huddle, resolved a minute ago —
	// the threshold the cue reads as a standoff, and inside recentlyResolvedOfferWindow so the
	// recency guard counts them. Declined is terminal, so no ExpiresAt is needed.
	resolved := published.Add(-1 * time.Minute)
	snap.PayLedger = map[sim.LedgerID]*sim.PayLedgerEntry{
		1: {ID: 1, BuyerID: actorID, SellerID: "ezekiel", ItemKind: sim.ShovelItemKind, Qty: 3, State: sim.PayLedgerStateDeclined, HuddleID: "smith_huddle", ResolvedAt: resolved},
		2: {ID: 2, BuyerID: actorID, SellerID: "ezekiel", ItemKind: sim.ShovelItemKind, Qty: 3, State: sim.PayLedgerStateDeclined, HuddleID: "smith_huddle", ResolvedAt: resolved},
	}
	return snap, actorID, warrants
}

// farmOwnerConservingOwesUpkeep is the LLM-299 conserve-coupling arm and the non-vacuous
// anchor for TestGoldensFarmUpkeepGoadNeverBesideHoldOff — the shovel twin of
// keeperConservingOwesNailRepair. Marta Vale is a shopkeeper whose working capital has
// collapsed (51 coins, below the 60-coin MerchantCoinFloor, with 20 unsold cloth on the
// shelf), so merchantConserve is Active. She ALSO owns Vale Farm and owes 1 upkeep shovel
// (51 coins over the 30 floor, banded by 20). She has stepped off her farm to the Blacksmith,
// where she shares Ezekiel Crane's huddle — Ezekiel produces both shovels (12 held, the
// upkeep supply) and coal (15 held, Marta's low restock input). Two standing sections fire
// at once: "## Restocking" flips to the coin-poor "Hold off buying more for now" steer (coal
// is low and Ezekiel is a co-present coal supplier, so the item survives the LLM-216 drop),
// while "## Farm upkeep" puts a shovel seller in front of her. The fix softens the shovel
// goad under conserve, so the prompt no longer carries "Buy it now" beside "Hold off buying
// more". She owns no worn business, so no nail repair cue competes here.
func farmOwnerConservingOwesUpkeep() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		martaID   = sim.ActorID("marta")
		ezekielID = sim.ActorID("ezekiel")
		farm      = sim.StructureID("marta_farm")
		smithy    = sim.StructureID("blacksmith")
		huddleID  = sim.HuddleID("forge_huddle")
	)
	zero := 0
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, off her farm at the smith
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	marta := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Marta Vale",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 200, Y: 200},
		InsideStructureID: smithy,
		WorkStructureID:   farm, // her post — she is standing off it, at the smith
		CurrentHuddleID:   huddleID,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             51, // below the 60 MerchantCoinFloor (conserve) yet above the 30 FarmUpkeepFloor (owes 1 shovel)
		Needs:             map[sim.NeedKey]int{},
		// Overstocked cloth is the conserve trigger; low coal is the surviving Restocking
		// item (Ezekiel supplies it co-present); 0 shovels is the upkeep shortfall.
		Inventory: map[sim.ItemKind]int{"cloth": 20, "coal": 1, "shovel": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cloth", Source: sim.RestockSourceBuy, Max: 24},
			{Item: "coal", Source: sim.RestockSourceBuy, Max: 12},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 201, Y: 200},
		InsideStructureID: smithy,
		WorkStructureID:   smithy,
		CurrentHuddleID:   huddleID,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"shovel": 12, "coal": 15},
		// Produces shovels AND coal, so the LLM-252 supplier-of-record gate names him for
		// each — the farm-upkeep shovel buy and Marta's coal restock both resolve to him.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "shovel", Source: sim.RestockSourceProduce, Max: 40},
			{Item: "coal", Source: sim.RestockSourceProduce, Max: 40},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Marta Vale": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:              published,
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		Assets:                   emptyAssetSet,
		MerchantCoinFloor:        60,
		RestockReorderPct:        25,
		FarmUpkeepFloor:          30,
		FarmUpkeepCoinsPerShovel: 20,
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{martaID: marta, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			farm:   plainStructure(farm, "Vale Farm"),
			smithy: plainStructure(smithy, "Blacksmith"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{martaID: {}, ezekielID: {}}},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(farm): {
				ID:            sim.VillageObjectID(farm),
				DisplayName:   "Vale Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  martaID,
				Tags:          []string{sim.TagFarm},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"cloth":  {Name: "cloth", DisplayLabel: "cloth"},
			"coal":   {Name: "coal", DisplayLabel: "coal"},
			"shovel": {Name: "shovel", DisplayLabel: "shovels"},
		},
	}
	return snap, martaID, nil
}

// farmOwnerOffPostOwesUpkeepNoSupplier is the LLM-277 review-caught edge (code_review
// c11007e7): a farm owner off her post, owing shovels, but with NO reachable shovel
// supplier anywhere (findItemVendors empty, no co-present seller). buildFarmUpkeep still
// renders the generic "from the blacksmith" fallback (the LLM-216 keep-the-sentence
// posture), but that dead-end must NOT suppress the return-to-post nag — suppressing it
// would strand her off-post with no way to complete the errand. The golden pins BOTH the
// generic "## Farm upkeep" line AND the to-work steer firing, because hasFarmUpkeepErrand
// is gated on an actionable buy path, not on the cue's mere presence.
func farmOwnerOffPostOwesUpkeepNoSupplier() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const actorID = sim.ActorID("elizabeth")
	zero := 0
	start, end := 360, 1080
	now := 600
	elizabeth := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Elizabeth Ellis",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 1000, Y: 1000}.Tile(), // off her farm
		WorkStructureID:  "ellis_farm",
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            95,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		Assets:                   emptyAssetSet,
		FarmUpkeepFloor:          30,
		FarmUpkeepCoinsPerShovel: 20,
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{actorID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			"ellis_farm": plainStructure("ellis_farm", "Ellis Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_farm": {
				ID:            "ellis_farm",
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  actorID,
				Tags:          []string{sim.TagFarm},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, actorID, nil
}

// passerbyAtWornStall: a non-owner standing at someone else's worn business — the
// co-present atmosphere line, no owner cue. The actor (John) differs from the
// business's owner (Ezekiel).
func passerbyAtWornStall() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return stallWearSnapshot("john", "ezekiel", "John Ellis", "tavernkeeper", 450, 0)
}

// passerbyAtDegradedStall is the LLM-310 non-owner arm: a passerby (John) stands at
// someone else's DEGRADED business (wear 650 >= degrade 600). The golden pins the
// faithful closed-for-restock condition line (the third-person mirror of the owner
// cue, LLM-304) instead of the worn-only texture. Foil of passerbyAtWornStall (wear
// 450 → worn but not degraded).
func passerbyAtDegradedStall() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return stallWearSnapshot("john", "ezekiel", "John Ellis", "tavernkeeper", 650, 0)
}

// hiredWorkerAtEmployerWornBusiness is the LLM-271 fixture, modeled on the live
// 2026-07-04 case: Lewis Walker, a hired hand mid-job for Prudence Ward (a Working
// LaborOffer, WorkerID == subject), stands INSIDE her worn PW Apothecary carrying
// enough nails to mend it. Prudence hired Lewis, who had nails and offered to do
// the repairs, but the owner-only repair cue never surfaced for him. The golden
// pins the hired-framed "## The business you're working at … needs mending" cue
// (NOT the owner's "## Your business") plus the hired repair warrant line — the
// wake that pierces the laboring shelve-gate. The repair tool rides the same
// StallRepair payload signal (tool_gating_stall_test.go covers the advertise side).
// Pos is far from the outdoor pin so the cue can only fire via the inside-structure
// branch of sim.AtBusiness, proving the hired path inherits LLM-266.
func hiredWorkerAtEmployerWornBusiness() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		prudenceID = sim.ActorID("prudence")
		lewisID    = sim.ActorID("lewis")
		shop       = sim.StructureID("apothecary")
		huddle     = sim.HuddleID("h1")
	)
	zero := 0
	published := time.Date(2026, 6, 30, 20, 30, 0, 0, time.UTC)
	workingUntil := published.Add(90 * time.Minute)
	acceptedAt := published.Add(-30 * time.Minute)
	insidePos := sim.WorldPos{X: 500, Y: 500}.Tile() // far from the pin at WorldPos{100,100}
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "apothecary",
		State:             sim.StateIdle,
		Pos:               insidePos,
		InsideStructureID: shop,
		WorkStructureID:   shop,
		CurrentHuddleID:   huddle,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "laborer",
		State:             sim.StateLaboring,
		Pos:               insidePos,
		InsideStructureID: shop,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 5},
		Acquaintances:     map[string]sim.Acquaintance{"Prudence Ward": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:               published,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{prudenceID: prudence, lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			shop: plainStructure(shop, "PW Apothecary"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{prudenceID: {}, lewisID: {}}},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"apothecary": {
				ID:            "apothecary",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  prudenceID,
				Tags:          []string{sim.TagBusiness, "shop", "market_stall"},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:           1,
				WorkerID:     lewisID,
				EmployerID:   prudenceID,
				Reward:       10,
				DurationMin:  120,
				State:        sim.LaborStateWorking,
				HuddleID:     huddle,
				AcceptedAt:   &acceptedAt,
				WorkingUntil: &workingUntil,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	warrants := []sim.WarrantMeta{{TriggerActorID: lewisID, Reason: sim.StallRepairHiredWarrantReason{StallID: "apothecary"}}}
	return snap, lewisID, warrants
}

// ownerAtWornTavern: John Ellis stands at his own worn Tavern — a business tagged
// {business, lodging, tavern} with NO market_stall tag, exercising the LLM-247
// widened gate (accrual keys off TagBusiness, not market_stall). The object
// carries no DisplayName, so the "## Your business" cue resolves the name from the
// co-located structure ("Your Tavern is showing hard use…"). Worn (450 >= repair
// 400, < degrade 600), short on nails (2 < 5) — the buy-then-mend steer.
func ownerAtWornTavern() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	pin := sim.WorldPos{X: 100, Y: 100}.Tile()
	john := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "John Ellis",
		Role:             "tavernkeeper",
		State:            sim.StateIdle,
		Pos:              pin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            8,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"nail": 2},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"john": john},
		Structures: map[sim.StructureID]*sim.Structure{
			"tavern": plainStructure("tavern", "Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"tavern": {
				ID:            "tavern",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "john",
				Tags:          []string{sim.TagBusiness, "lodging", "tavern"},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "john", nil
}

// ownerInsideWornBusiness: the LLM-266 regression fixture — the owner (John Ellis)
// stands INSIDE his own worn Tavern (InsideStructureID == the business id, since
// structures share the village_object's id) and AWAY from the outdoor loiter pin.
// This is the live keeper-at-post posture the old pin-only co-location gate
// silently excluded, so the "## Your business" cue had never rendered for any real
// NPC. Pos is deliberately many Chebyshev tiles from the pin (WorldPos{100,100}),
// so the cue can fire ONLY via the inside-structure branch of sim.AtBusiness — the
// pin proximity check never passes here. Worn (450 >= repair 400, < degrade 600),
// short on nails (2 < 5) — the buy-then-mend steer, named from the co-located
// structure ("Your Tavern is showing hard use…").
func ownerInsideWornBusiness() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080                          // 06:00–18:00
	now := 600                                       // 10:00 — on shift
	insidePos := sim.WorldPos{X: 500, Y: 500}.Tile() // far from the pin at WorldPos{100,100}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		Pos:               insidePos,
		InsideStructureID: "tavern",
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 2},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"john": john},
		Structures: map[sim.StructureID]*sim.Structure{
			"tavern": plainStructure("tavern", "Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"tavern": {
				ID:            "tavern",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "john",
				Tags:          []string{sim.TagBusiness, "lodging", "tavern"},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "john", nil
}

// passerbyInsideWornBusiness: the LLM-266 non-owner arm — a non-owner (Ezekiel)
// stands INSIDE someone else's worn business (John's Tavern) and away from the
// outdoor loiter pin. The co-present condition line ("The Tavern here looks
// worn…") now renders via the inside-structure branch of sim.AtBusiness, while the
// owner-only "## Your business" cue stays absent (Ezekiel owns nothing). The owner
// (John) need not be present as an actor — the condition line reads off the object.
func passerbyInsideWornBusiness() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	zero := 0
	start, end := 360, 1080                          // 06:00–18:00
	now := 600                                       // 10:00 — on shift
	insidePos := sim.WorldPos{X: 500, Y: 500}.Tile() // far from the pin at WorldPos{100,100}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               insidePos,
		InsideStructureID: "tavern",
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:          &now,
		NeedThresholds:            sim.NeedThresholds{},
		Assets:                    emptyAssetSet,
		StallWearRepairThreshold:  400,
		StallWearDegradeThreshold: 600,
		StallNailsPerRepair:       5,
		Actors:                    map[sim.ActorID]*sim.ActorSnapshot{"ezekiel": ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			"tavern": plainStructure("tavern", "Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"tavern": {
				ID:            "tavern",
				DisplayName:   "Tavern",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  "john",
				Tags:          []string{sim.TagBusiness, "lodging", "tavern"},
				Wear:          450,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, "ezekiel", nil
}

// farmUpkeepSnapshot: the actor owns a farm-tagged object and, with `coins` held
// against `floor`/`coinsPerShovel`, owes more upkeep shovels than the `shovels` they
// carry — so the "## Farm upkeep" cue renders. Not co-location-gated (the buy happens
// at the blacksmith), so the actor's position is irrelevant to the cue. No orders, no
// clock read → byte-stable.
func farmUpkeepSnapshot(coins, shovels, floor, coinsPerShovel int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const actorID = sim.ActorID("elizabeth")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	farmPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	inv := map[sim.ItemKind]int{}
	if shovels > 0 {
		inv[sim.ShovelItemKind] = shovels
	}
	actor := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Elizabeth Ellis",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              farmPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            coins,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        inv,
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		Assets:                   emptyAssetSet,
		FarmUpkeepFloor:          floor,
		FarmUpkeepCoinsPerShovel: coinsPerShovel,
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{actorID: actor},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"ellis_farm": {
				ID:            "ellis_farm",
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  actorID,
				Tags:          []string{sim.TagFarm},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, actorID, nil
}

// farmOwnerOwesUpkeep: Elizabeth owns Ellis Farm with 95 coins (floor 30, band 20 →
// owes 3 shovels) and none in hand, and NO shovel supplier exists in the world. The
// golden pins the "## Farm upkeep" cue with the GENERIC no-destination steer ("buy 3
// fresh shovels from the blacksmith") — the LLM-274 no-actionable-supplier arm.
func farmOwnerOwesUpkeep() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return farmUpkeepSnapshot(95, 0, 30, 20)
}

// farmOwnerOwesUpkeepWithShovelSupplier is the LLM-274 farm-upkeep arm: Elizabeth
// owes 3 upkeep shovels (95 coins, floor 30, band 20) and holds none, while a
// SEPARATE shovel-producing smith — Ezekiel at the Blacksmith holding 12 shovels —
// exists. findItemVendors resolves him (he PRODUCES shovels, LLM-200/LLM-252), so the
// "## Farm upkeep" cue names "buy from Blacksmith (destination: blacksmith)" with
// move_to + pay_with_item instead of the dead-end "from the blacksmith". Ezekiel is
// placed far from Elizabeth so he is a supplier of record, not co-present. The foil is
// farmOwnerOwesUpkeep (no smith → the generic sentence is correctly kept).
func farmOwnerOwesUpkeepWithShovelSupplier() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := farmUpkeepSnapshot(95, 0, 30, 20)
	start, end := 360, 1080
	snap.Actors["ezekiel"] = &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Ezekiel Crane",
		Role:             "blacksmith",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 2000, Y: 2000}.Tile(), // far from Elizabeth — a supplier of record, not co-present
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		WorkStructureID:  "blacksmith",
		Coins:            0,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"shovel": 12},
		RestockPolicy:    producePolicy("shovel", 40), // the smith PRODUCES shovels — the LLM-252 supplier-of-record gate
	}
	// Add the smith's workplace without clobbering any structures farmUpkeepSnapshot
	// may seed (it doesn't today — farm ownership keys off village_object.OwnerActorID
	// + TagFarm, not the structure map — but the nil-guard keeps this robust if that
	// changes; code_review).
	if snap.Structures == nil {
		snap.Structures = map[sim.StructureID]*sim.Structure{}
	}
	snap.Structures["blacksmith"] = plainStructure("blacksmith", "Blacksmith")
	return snap, actorID, warrants
}

// hungryForagerAtStockedBush is the LLM-113 situation: a hungry forager stands at
// an unowned (commons) raspberry bush that still has stock, with a cheese seller
// at the General Store. It exercises the singular/plural catalog phrasing in two
// cues at once — the gather affordance ("you can gather raspberries here") and the
// buy menu ("buy a wedge of cheese", the measure phrase + indefinite article). No
// orders, no clock read → byte-stable.
func hungryForagerAtStockedBush() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
	)
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — daytime
	bushPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	ezekiel := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Ezekiel Crane",
		Role:             "forager",
		State:            sim.StateIdle,
		Pos:              bushPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            6,
		Needs:            map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"raspberries": {
				Name:         "raspberries",
				Capabilities: []string{"portable"}, DisplayLabel: "Raspberries",
				DisplayLabelSingular: "raspberry", DisplayLabelPlural: "raspberries",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 2}},
			},
			"cheese": {
				Name:         "cheese",
				Capabilities: []string{"portable"}, DisplayLabel: "Cheese",
				DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}},
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"wild_bush": {
				ID:            "wild_bush",
				DisplayName:   "Raspberry Bush",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					// PICK-AND-EAT (Amount < 0), as the live unowned berry bushes are:
					// wild food anyone may take, no forage trade required. It was
					// authored Amount 0 (forage-to-sell), which no live wild bush is,
					// and LLM-610 made that inaccuracy load-bearing — a yield-only row
					// would now need a trade this forager has no restock policy to
					// carry. Amount is -(hunger satisfied by one raspberry), per LLM-87.
					{Attribute: "hunger", Amount: -2, GatherItem: "raspberries", AvailableQuantity: intp(3)},
				},
			},
		},
	}
	return snap, ezekielID, nil
}

// snackLoopScenario is the LLM-307 fixture: a mildly-hungry stateful NPC (Ezekiel
// Crane, hunger 14 — felt at/above the silent floor 10, below the red threshold
// 18) carrying ONLY the food named by `carry`, with a cheese seller (a full meal,
// magnitude 8) at the General Store nearby. Parameterized by the carried food so
// the paired goldens differ in exactly one variable — own-stock class:
//
//   - a NIBBLE (raspberries, magnitude 2): the consume-first suppression must
//     re-open the meal directory and print the bridging line — a snack can't quiet
//     a persisting hunger, so the walk-to meal must stay visible (the live Ezekiel
//     raspberry loop, 2026-07-06).
//   - a MEAL (cheese, magnitude 8): the suppression must stand — a meal on hand is
//     the answer, the directory stays noise (the LLM-139 foil).
//
// He holds 59 coins (the live purse he never spent) so the vendor clears the
// means-to-pay gate. No PriceBook/orders, so the render takes no wall-clock read
// and stays byte-stable.
func snackLoopScenario(carry sim.ItemKind) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — daytime
	ezekiel := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Ezekiel Crane",
		Role:             "blacksmith",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 100, Y: 100}.Tile(),
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            59,
		Inventory:        map[sim.ItemKind]int{carry: 3},
		Needs:            map[sim.NeedKey]int{"hunger": 14},
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mabelID: mabel},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"raspberries": {
				Name:         "raspberries",
				Capabilities: []string{"portable"}, DisplayLabel: "Raspberries",
				DisplayLabelSingular: "raspberry", DisplayLabelPlural: "raspberries",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 2}},
			},
			"cheese": {
				Name:         "cheese",
				Capabilities: []string{"portable"}, DisplayLabel: "Cheese",
				DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}},
			},
		},
	}
	return snap, ezekielID, nil
}

func hungryHoldingNibbleSeesMealVendor() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return snackLoopScenario("raspberries")
}

func hungryHoldingMealKeepsSuppression() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return snackLoopScenario("cheese")
}

// snackLooperRedirectedToMeal is the LLM-307 loop-coda fixture: the snack-loop
// actor (mild hunger, only raspberries, a reachable cheese seller) is ALSO stuck
// in a looping huddle conversation, the condition under which the LLM-176
// need-redirect coda renders. Before the coda fix, needRedirectFor returned
// "consume your raspberries" — contradicting the eat/drink section's "for a real
// meal, see the options below" and re-arming the snacking loop. Grace Bishop is a
// present, food-less huddle peer so the huddle (hence ConversationLooping) is real
// and no co-present-peer buy offer competes. Fixed PublishedAt so the turn-state
// read stays byte-stable (no wall-clock read).
func snackLooperRedirectedToMeal() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		graceID   = sim.ActorID("grace")
		mabelID   = sim.ActorID("mabel")
		store     = sim.StructureID("general_store")
		huddleID  = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:                sim.KindNPCStateful,
		DisplayName:         "Ezekiel Crane",
		Role:                "blacksmith",
		State:               sim.StateIdle,
		Pos:                 sim.TilePos{X: 10, Y: 10},
		CurrentHuddleID:     huddleID,
		Coins:               59,
		Inventory:           map[sim.ItemKind]int{"raspberries": 3},
		Needs:               map[sim.NeedKey]int{"hunger": 14}, // mild: felt, below red 18
		ConversationLooping: true,
	}
	grace := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Grace Bishop",
		Role:            "farmer",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 11, Y: 10},
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
	}
	mabel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Mabel Stone",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 40, Y: 40},
		WorkStructureID:   store,
		InsideStructureID: store,
		Coins:             20,
		Inventory:         map[sim.ItemKind]int{"cheese": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Assets:         emptyAssetSet,
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, graceID: grace, mabelID: mabel},
		Structures:     map[sim.StructureID]*sim.Structure{store: plainStructure(store, "General Store")},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{ezekielID: {}, graceID: {}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"raspberries": {Name: "raspberries", Capabilities: []string{"portable"}, DisplayLabel: "Raspberries", DisplayLabelSingular: "raspberry", DisplayLabelPlural: "raspberries", Category: sim.ItemCategoryFood, Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 2}}},
			"cheese":      {Name: "cheese", DisplayLabel: "Cheese", DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese", Category: sim.ItemCategoryFood, Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}}},
		},
	}
	return snap, ezekielID, nil
}

// grazingProducerScenario builds the LLM-134 fixture: Moses James, a carrot
// farmer, standing at his own farm on shift carrying only the carrots he grows
// to sell, at the given hunger level. No other food, vendor, or free source is
// present, so the carrots are the only possible own-stock cue — the scenario
// isolates the trade-stock demotion. No PriceBook/orders, so the render takes no
// wall-clock read and stays byte-stable.
func grazingProducerScenario(hunger int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID = sim.ActorID("moses")
		farm    = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 100, Y: 100}.Tile(),
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             4,
		Inventory:         map[sim.ItemKind]int{"carrots": 20},
		Needs:             map[sim.NeedKey]int{"hunger": hunger},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "James Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {
				Name:         "carrots",
				Capabilities: []string{"portable"}, DisplayLabel: "Carrots",
				DisplayLabelSingular: "carrot", DisplayLabelPlural: "carrots",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 3}},
			},
		},
	}
	return snap, mosesID, nil
}

func producerHungryMildAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return grazingProducerScenario(14) // mild: felt (>= silent floor 10), below red (18)
}

// hungryActorHoldingRawMeat is the LLM-166 fixture: a hungry stateful NPC stands
// at its post carrying raw Meat (a stew INGREDIENT — food-category but eases no
// need raw) alongside edible Cheese. The golden pins the use annotation folded
// into the carry readout — "Meat (x7, used to produce stew)" — while Cheese stays
// bare. This is the Josiah-eats-raw-meat case: the most food-like name in a flat
// inventory was the rejected eat that burned the turn.
func hungryActorHoldingRawMeat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		store    = sim.StructureID("general_store")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 100, Y: 100}.Tile(),
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             6,
		Inventory:         map[sim.ItemKind]int{"meat": 7, "cheese": 15},
		Needs:             map[sim.NeedKey]int{"hunger": 14}, // felt, below red
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"meat": {
				Name:         "meat",
				Capabilities: []string{"portable"}, DisplayLabel: "Meat",
				DisplayLabelSingular: "cut of meat", DisplayLabelPlural: "cuts of meat",
				Category: sim.ItemCategoryFood, // food, but no Satisfies -> inedible raw
			},
			"cheese": {
				Name:         "cheese",
				Capabilities: []string{"portable"}, DisplayLabel: "Cheese",
				DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 4}},
			},
			"stew": {Name: "stew", DisplayLabel: "Stew", DisplayLabelSingular: "bowl of stew"},
		},
		// stew consumes meat — the engine derives the reverse use-index from this
		// (World.recipeUses), aliased onto the snapshot as RecipeUses. Set both so
		// the fixture reads coherently.
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"stew": {OutputItem: "stew", Inputs: []sim.RecipeInput{{Item: "meat", Qty: 10}}},
		},
		RecipeUses: map[sim.ItemKind][]sim.ItemKind{"meat": {"stew"}},
	}
	return snap, josiahID, nil
}

func producerStarvingAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return grazingProducerScenario(sim.DefaultHungerRedThreshold) // 18 — red/desperation tier
}

// wholesalerGrazingScenario builds the LLM-267 fixture: Moses James at his own
// farm on shift, carrying only the carrots he grows — but the farm is tagged
// wholesaler (the farms + mill are the wholesale tier), so those carrots are stock
// to sell, never his larder. Unlike grazingProducerScenario (LLM-134, which lets
// the trade stock return at the red tier), his own produce must NOT surface in the
// eat cue at ANY tier. Same isolation: no other food, vendor, or free source, so the
// carrots are the only own-stock candidate. Byte-stable (no PriceBook/orders).
func wholesalerGrazingScenario(hunger int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID = sim.ActorID("moses")
		farm    = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 100, Y: 100}.Tile(),
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             4,
		Inventory:         map[sim.ItemKind]int{"carrots": 20},
		Needs:             map[sim.NeedKey]int{"hunger": hunger},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "James Farm"),
		},
		// The farm carries both tags as the live data does; only wholesaler gates
		// the own-produce block (IsOwnProduce keys on it, like SellerAtWholesaler).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(farm): {ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {
				Name:         "carrots",
				Capabilities: []string{"portable"}, DisplayLabel: "Carrots",
				DisplayLabelSingular: "carrot", DisplayLabelPlural: "carrots",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 3}},
			},
		},
	}
	return snap, mosesID, nil
}

func wholesalerStarvingOwnProduceAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return wholesalerGrazingScenario(sim.DefaultHungerRedThreshold) // 18 — red: STILL not offered its own produce
}

// wholesalerCarryingBoughtFoodAtPost is the LLM-267 positive control: the same
// wholesaler, mildly hungry, carrying a bought loaf of bread (NOT one of its produce
// rows) alongside its own carrots. The bread — real provisions — IS offered in the
// eat cue; the carrots are not. Proves the block is item-scoped (own produce only),
// not a blanket "wholesalers never eat".
func wholesalerCarryingBoughtFoodAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id, warr := wholesalerGrazingScenario(14) // mild: felt (>= silent floor 10), below red (18)
	snap.Actors[id].Inventory["bread"] = 2
	snap.ItemKinds["bread"] = &sim.ItemKindDef{
		Name: "bread", DisplayLabel: "Bread",
		DisplayLabelSingular: "loaf of bread", DisplayLabelPlural: "loaves of bread",
		Category:     sim.ItemCategoryFood,
		Capabilities: []string{"portable"}, // mirrors live: bread is the canonical carry-home food
		Satisfies:    []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}},
	}
	return snap, id, warr
}

// wholesalerProducerBarteringWithCustomer is the LLM-291 fixture: Moses James, a
// WHOLESALE producer (James Farm tagged wholesaler; grows carrots + wheat), stands
// in company with a would-be customer (Silence Walker). His produce sells only to
// the village distributor (Josiah Thorne, keeper of the distributor-tagged General
// Store), so the '## What your wares fetch' cue must draw the wholesale-channel line
// — who buys it, what the distributor pays, where to send other buyers — NOT a
// retail spread that nudges him to hawk carrots to whoever he is standing with (the
// street sale the PayWithItem wholesale gate then refuses; live hud-9b23…). Off the
// farm (InsideStructureID = commons) so '## Your trade' doesn't render; no
// pressing needs so no eat/drink cues clutter the wares section. Josiah is present
// only so distributorLabel can resolve a person to route buyers to — not co-present.
func wholesalerProducerBarteringWithCustomer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID   = sim.ActorID("moses")
		silenceID = sim.ActorID("silence")
		josiahID  = sim.ActorID("josiah")
		commons   = sim.StructureID("commons")
		farm      = sim.StructureID("james_farm")
		store     = sim.StructureID("general_store")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		WorkStructureID:   farm,
		Coins:             38,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrots": 30, "wheat": 30},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrots", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "wheat", Source: sim.RestockSourceProduce, Max: 30},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             22,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Moses James": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: store,
		WorkStructureID:   store,
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses, silenceID: silence, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
			farm:    plainStructure(farm, "James Farm"),
			store:   plainStructure(store, "General Store"),
		},
		// The farm carries both tags as the live data does; only wholesaler gates the
		// wholesale channel (IsOwnProduce / SellerAtWholesaler key on it).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(farm):  {ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{mosesID: {}, silenceID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"carrots": {OutputItem: "carrots", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 2, RetailPrice: 4},
			"wheat":   {OutputItem: "wheat", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"carrots": {
				Name:         "carrots",
				Capabilities: []string{"portable"}, DisplayLabel: "Carrots",
				DisplayLabelSingular: "carrot", DisplayLabelPlural: "carrots",
				Category: sim.ItemCategoryFood,
			},
			"wheat": {
				Name: "wheat", DisplayLabel: "Wheat",
				DisplayLabelSingular: "wheat", DisplayLabelPlural: "wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
	}
	return snap, mosesID, nil
}

// huddleConversationLoopingScenario is the LLM-169 fixture: two idle workers (the
// Walker sisters) stand together in a huddle going in circles. Patience (the
// subject) is in an armed conversational loop — ConversationLooping is set, the
// publish-time huddleLoopArmed signal the loop sweep arms on — and Anne holds a
// live await edge to her (Anne addressed Patience and waits on a reply). The golden
// pins the LLM-169 swap: the "Anne Walker is waiting for your reply." nag is
// suppressed (that nag is what manufactures the echo) and the coda is the "you've
// agreed — act now or done()" loop steer rather than the default/awaiting coda the
// agree-loop fed on. Byte-stable: fixed PublishedAt, the await edge + utterances
// stamped relative to it, no orders.
func huddleConversationLoopingScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		anneID     = sim.ActorID("anne")
		huddleID   = sim.HuddleID("walker_huddle")
	)
	now := 13 * 60 // 13:00 — afternoon
	published := time.Date(2026, 6, 28, 13, 0, 0, 0, time.UTC)
	patience := &sim.ActorSnapshot{
		Kind:                sim.KindNPCStateful,
		DisplayName:         "Patience Walker",
		Role:                "villager",
		State:               sim.StateIdle,
		CurrentHuddleID:     huddleID,
		Coins:               5,
		Needs:               map[sim.NeedKey]int{},
		ConversationLooping: true,
	}
	anne := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Anne Walker",
		Role:            "villager",
		State:           sim.StateIdle,
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
		// Anne addressed Patience and awaits her reply — the edge that would render
		// "Anne Walker is waiting for your reply." but for the LLM-169 suppression.
		AwaitingReplyFrom: map[sim.ActorID]time.Time{patienceID: published.Add(-10 * time.Second)},
	}
	utter := func(spk sim.ActorID, name, text string, agoSec int) sim.Utterance {
		return sim.Utterance{SpeakerID: spk, SpeakerName: name, Text: text, At: published.Add(-time.Duration(agoSec) * time.Second)}
	}
	snap := &sim.Snapshot{
		PublishedAt:         published,
		LocalMinuteOfDay:    &now,
		NeedThresholds:      sim.NeedThresholds{},
		Assets:              emptyAssetSet,
		NPCAwaitReplyWindow: 60 * time.Second,
		Actors:              map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience, anneID: anne},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{patienceID: {}, anneID: {}},
				RecentUtterances: []sim.Utterance{
					utter(patienceID, "Patience Walker", "Let's go to the well!", 40),
					utter(anneID, "Anne Walker", "Let's go to the well!", 32),
					utter(patienceID, "Patience Walker", "Let's go!", 24),
					utter(anneID, "Anne Walker", "Let's go to the well!", 16),
					utter(patienceID, "Patience Walker", "Lead the way, Anne.", 8),
				},
			},
		},
	}
	return snap, patienceID, nil
}

// undirectedReaskSolePeerScenario is the LLM-232 fixture: John Ellis stands in a
// two-body huddle with Anne Walker and has floated a plain, unaddressed trade
// proposal that opened no WORK-370 edge; Anne has said nothing back. John spoke
// most recently (~75s ago — past the 60s directed-edge window, so even a directed
// edge would have lapsed, but well inside ReaskSuppressWindow), and the huddle is
// NOT looping, so the sole-awake-peer
// anchor folds Anne into AwaitingReplyFrom: the golden pins the "you already
// spoke, wait, don't repeat" line + the awaiting-reply coda on an otherwise
// undirected re-ask. Fixed PublishedAt, utterances stamped relative to it, no
// orders → byte-stable.
func undirectedReaskSolePeerScenario() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID   = sim.ActorID("john")
		anneID   = sim.ActorID("anne")
		huddleID = sim.HuddleID("store_huddle")
	)
	now := 13 * 60 // 13:00 — afternoon, no sleep/return-to-post cue competes
	published := time.Date(2026, 7, 3, 13, 0, 0, 0, time.UTC)
	john := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "John Ellis",
		Role:            "villager",
		State:           sim.StateIdle,
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
	}
	anne := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Anne Walker",
		Role:            "villager",
		State:           sim.StateIdle,
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
	}
	utter := func(spk sim.ActorID, name, text string, agoSec int) sim.Utterance {
		return sim.Utterance{SpeakerID: spk, SpeakerName: name, Text: text, At: published.Add(-time.Duration(agoSec) * time.Second)}
	}
	snap := &sim.Snapshot{
		PublishedAt:         published,
		LocalMinuteOfDay:    &now,
		NeedThresholds:      sim.NeedThresholds{},
		Assets:              emptyAssetSet,
		NPCAwaitReplyWindow: 60 * time.Second,
		Actors:              map[sim.ActorID]*sim.ActorSnapshot{johnID: john, anneID: anne},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{johnID: {}, anneID: {}},
				RecentUtterances: []sim.Utterance{
					utter(anneID, "Anne Walker", "Morning, John.", 110),
					utter(johnID, "John Ellis", "Morning. Say — I've cheese to spare; could you fetch me carrots?", 85),
					utter(johnID, "John Ellis", "A fair trade, cheese for carrots?", 75),
				},
			},
		},
	}
	return snap, johnID, nil
}

// pacedWorkerOwesAnswerToQuestioner is the LLM-536 fixture, built from the live
// trace: Constable Marsh asked Silence Walker — a hired worker inside a live
// labor window — a direct question about the shut Tavern, and she is only now
// drawing the tick her 3m reply cadence held back. The edge is stamped 107s ago:
// PAST the 60s WORK-370 window, so before LLM-536 the owed-reply line was gone
// by the time she could speak, and she spent the turn on an unrelated topic
// (window mending) while the constable waited. Her ReplyPacingWindow widens her
// own await window to 240s, so the golden pins "Constable Marsh is waiting for
// your reply." surviving to the moment she can answer.
//
// A regression that dropped the widening would leave a laboring worker holding a
// turn with no obligation attached — the tick restored, the question still lost.
// Fixed PublishedAt, utterances stamped relative to it, no orders → byte-stable.
func pacedWorkerOwesAnswerToQuestioner() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		marshID   = sim.ActorID("marsh")
		huddleID  = sim.HuddleID("inn_huddle")
	)
	now := 12*60 + 30 // 12:30 — midday, no sleep cue competes
	published := time.Date(2026, 7, 27, 12, 29, 54, 0, time.UTC)
	silence := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Silence Walker",
		Role:            "villager",
		State:           sim.StateLaboring,
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
		// The pacing that shelved her ticks — and now keeps the question owed
		// across the wait it imposed.
		ReplyPacingWindow: 3 * time.Minute,
	}
	marsh := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Constable Marsh",
		Role:            "villager",
		State:           sim.StateIdle,
		CurrentHuddleID: huddleID,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{},
		// Stamped when he asked, 107s before this tick.
		AwaitingReplyFrom: map[sim.ActorID]time.Time{silenceID: published.Add(-107 * time.Second)},
	}
	utter := func(spk sim.ActorID, name, text string, agoSec int) sim.Utterance {
		return sim.Utterance{SpeakerID: spk, SpeakerName: name, Text: text, At: published.Add(-time.Duration(agoSec) * time.Second)}
	}
	snap := &sim.Snapshot{
		PublishedAt:         published,
		LocalMinuteOfDay:    &now,
		NeedThresholds:      sim.NeedThresholds{},
		Assets:              emptyAssetSet,
		NPCAwaitReplyWindow: 60 * time.Second,
		Actors:              map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence, marshID: marsh},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{silenceID: {}, marshID: {}},
				RecentUtterances: []sim.Utterance{
					utter(marshID, "Constable Marsh", "Silence — walk with me a moment, if you've a minute.", 130),
					utter(marshID, "Constable Marsh", "You said you were at the Tavern yesterday mending floors and windows. Did John Ellis seem himself to you?", 107),
				},
			},
		},
	}
	return snap, silenceID, nil
}

// hungryLooperAtFoodlessHome is the LLM-176 fixture: the Walker sisters loop in a
// huddle inside their foodless residence while hungry. Patience (the subject) is
// in an armed conversational loop, feels red-tier hunger, carries nothing edible,
// holds 1 coin, and a free Raspberry Bush sits a walk away (in VillageObjects but
// far from the home, so it lists in the eat cue yet is NOT co-located). It drives
// both LLM-176 cues at once: the no-food-here dead end (inside a structure, felt
// hunger, nothing held, no source on the tile) and the need-redirect coda (the
// looping coda names the nearest free source + move_to instead of the generic
// "do what you've agreed" line). Fixed PublishedAt, no orders/PriceBook →
// byte-stable.
func hungryLooperAtFoodlessHome() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		anneID     = sim.ActorID("anne")
		homeID     = sim.StructureID("walker_residence")
		huddleID   = sim.HuddleID("walker_huddle")
	)
	zero := 0
	now := 13 * 60 // 13:00 — afternoon, so no sleep/return-to-post cue competes
	published := time.Date(2026, 6, 29, 13, 0, 0, 0, time.UTC)
	homeTile := sim.WorldPos{X: 10, Y: 10}.Tile() // at home, far from the bush
	mkSister := func(name string, looping bool) *sim.ActorSnapshot {
		return &sim.ActorSnapshot{
			Kind:                sim.KindNPCStateful,
			DisplayName:         name,
			Role:                "villager",
			State:               sim.StateIdle,
			Pos:                 homeTile,
			HomeStructureID:     homeID,
			InsideStructureID:   homeID,
			CurrentHuddleID:     huddleID,
			Coins:               1,
			Needs:               map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
			ConversationLooping: looping,
		}
	}
	patience := mkSister("Patience Walker", true)
	anne := mkSister("Anne Walker", false)
	utter := func(spk sim.ActorID, name, text string, agoSec int) sim.Utterance {
		return sim.Utterance{SpeakerID: spk, SpeakerName: name, Text: text, At: published.Add(-time.Duration(agoSec) * time.Second)}
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience, anneID: anne},
		Structures: map[sim.StructureID]*sim.Structure{
			homeID: plainStructure(homeID, "Walker Residence"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"raspberries": {
				Name:         "raspberries",
				Capabilities: []string{"portable"}, DisplayLabel: "Raspberries",
				DisplayLabelSingular: "raspberry", DisplayLabelPlural: "raspberries",
				Category:  sim.ItemCategoryFood,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 2}},
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"wild_bush": {
				ID:            "wild_bush",
				DisplayName:   "Raspberry Bush",
				Pos:           sim.WorldPos{X: 400, Y: 400}, // a walk away — listed in the eat cue, not co-located
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Attribute: "hunger", Amount: -2}, // eases hunger on arrival — a free public source
				},
			},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{patienceID: {}, anneID: {}},
				RecentUtterances: []sim.Utterance{
					utter(patienceID, "Patience Walker", "I'm sure there's bread in the kitchen.", 40),
					utter(anneID, "Anne Walker", "Let's check the kitchen for food.", 32),
					utter(patienceID, "Patience Walker", "There must be something to eat at home.", 24),
					utter(anneID, "Anne Walker", "Let's look in the kitchen.", 16),
					utter(patienceID, "Patience Walker", "I'll find us a bite here.", 8),
				},
			},
		},
	}
	return snap, patienceID, nil
}

// TestConversationLoopingCodaOnlyWhenLooping is the LLM-169 cross-scenario
// invariant: the "you've agreed, act now or done()" loop coda appears in EXACTLY
// the scenario whose rendered actor is in an armed conversational loop
// (ActorSnapshot.ConversationLooping), and never elsewhere. The expectation is
// recomputed from the BUILT actor state, not the rendered text, so it independently
// asserts the coda tracks the flag rather than pinning the render against its own
// marker.
func TestConversationLoopingCodaOnlyWhenLooping(t *testing.T) {
	const marker = "keep saying the same thing"
	var sawLooping bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, _ := sc.build()
		actor := snap.Actors[actorID]
		if actor == nil {
			t.Fatalf("scenario %q: rendered actor %q missing from snapshot", sc.name, actorID)
		}
		want := actor.ConversationLooping
		if want {
			sawLooping = true
		}
		if has := strings.Contains(renderScenario(sc), marker); has != want {
			t.Errorf("scenario %q: looping coda present=%v, want %v (ConversationLooping=%v)", sc.name, has, want, actor.ConversationLooping)
		}
	}
	if !sawLooping {
		t.Error("matrix must exercise the looping branch (ConversationLooping=true) at least once")
	}
}

// TestIngredientUseAnnotationOnlyForInedibleRecipeInputs is the LLM-166
// cross-scenario invariant: the "used to produce X" annotation appears in EXACTLY
// the scenarios whose rendered actor carries an INEDIBLE item that some recipe
// consumes as an input (snap.RecipeUses), and never otherwise. The gate is
// recomputed from BUILT state — non-consumable AND a recipe input — so it mirrors
// inventoryItemUse exactly (an edible item, a non-ingredient, or an item with no
// catalog def draws no annotation, even if it appears in RecipeUses).
func TestIngredientUseAnnotationOnlyForInedibleRecipeInputs(t *testing.T) {
	const marker = "used to produce"
	var sawAnnotated bool
	for _, sc := range perceptionScenarios {
		sc := sc
		snap, actorID, _ := sc.build()
		actor := snap.Actors[actorID]
		want := false
		if actor != nil {
			for kind, qty := range actor.Inventory {
				if qty <= 0 {
					continue
				}
				def := snap.ItemKinds[kind]
				if def == nil || def.Consumable() {
					continue // edible or uncatalogued -> not annotated
				}
				if len(snap.RecipeUses[kind]) > 0 {
					want = true
					break
				}
			}
		}
		if want {
			sawAnnotated = true
		}
		if has := strings.Contains(renderScenario(sc), marker); has != want {
			t.Errorf("scenario %q: ingredient-use annotation present=%v, want %v", sc.name, has, want)
		}
	}
	if !sawAnnotated {
		t.Error("matrix must exercise the annotated branch (an inedible carried recipe-input) at least once")
	}
}

// TestOwnTradeStockEatCueOnlyAtDesperation is the LLM-134 cross-scenario
// invariant: a producer's own trade stock surfaces in the own-stock "consume to
// eat" cue ONLY at the red/desperation tier. The same farmer holding the same
// carrots is offered them when starving and NOT when only mildly hungry — the
// demotion that stops merchandise grazing while preserving the don't-starve-next-
// to-your-food safety net.
func TestOwnTradeStockEatCueOnlyAtDesperation(t *testing.T) {
	const cue = "consume to eat"
	mild := renderScenario(perceptionScenario{name: "producer_hungry_mild_at_post", build: producerHungryMildAtPost})
	if strings.Contains(mild, cue) {
		t.Errorf("mild-hunger producer was offered its own trade stock to eat (cue %q should be absent):\n%s", cue, mild)
	}
	red := renderScenario(perceptionScenario{name: "producer_starving_at_post", build: producerStarvingAtPost})
	if !strings.Contains(red, cue) {
		t.Errorf("starving producer was NOT offered its own trade stock as last resort (cue %q should be present):\n%s", cue, red)
	}
}

// TestWholesalerNeverCuedToEatOwnProduce is the LLM-267 invariant: a wholesaler
// owner is NEVER offered its own produce in the eat cue — not even at the red/
// desperation tier where LLM-134 lets an ORDINARY producer's trade stock return as a
// last resort. The wholesale gate has no red-tier escape hatch (it pairs with the
// hard Consume guard). A bought food the wholesaler also carries IS still offered, so
// the block is own-produce-scoped, not a blanket ban on the wholesaler eating.
func TestWholesalerNeverCuedToEatOwnProduce(t *testing.T) {
	const cue = "consume to eat"
	for _, hunger := range []int{14, sim.DefaultHungerRedThreshold} { // mild AND red
		hunger := hunger
		got := renderScenario(perceptionScenario{name: "wholesaler_own_produce", build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
			return wholesalerGrazingScenario(hunger)
		}})
		if strings.Contains(got, cue) {
			t.Errorf("wholesaler at hunger %d was offered its own produce to eat (cue %q must be absent — no red-tier escape):\n%s", hunger, cue, got)
		}
	}
	// Positive control: a bought food the wholesaler carries IS offered — and the eat
	// cue names the BREAD, not the carrots (which stay only in the inventory readout,
	// so a bare Contains on the whole prompt wouldn't prove the produce is excluded).
	bought := renderScenario(perceptionScenario{name: "wholesaler_bought_food_at_post", build: wholesalerCarryingBoughtFoodAtPost})
	eatCue := promptSection(bought, "## What you can eat or drink")
	if !strings.Contains(eatCue, cue) {
		t.Fatalf("wholesaler carrying bought bread was NOT offered it to eat (cue %q should be present):\n%s", cue, bought)
	}
	if !strings.Contains(eatCue, "Bread") {
		t.Errorf("eat cue should offer the bought Bread, got:\n%s", eatCue)
	}
	if strings.Contains(eatCue, "Carrots") {
		t.Errorf("eat cue must NOT name own-produce Carrots, got:\n%s", eatCue)
	}
}

// smithChoosingAtForge is the LLM-116 situation, redesigned for LLM-319 one-shot
// batches: Ezekiel, a multi-output producer, stands inside his own forge on shift
// with two produce goods (skillet at cap, nail empty) and NOTHING in the works —
// the idle-at-post state the production-choice warrant fires on. The "## Your
// trade" cue renders one felt-language scene per good (stock tier + sell-through
// + batch affordance): full skillet stores get NO affordance sentence (a batch
// can't start at cap), empty nail gets the plain batch offer; the section closes
// with the neutral produce line and the "thoughts turn to your trade" warrant
// renders. The standing in-flight batch line does not appear — that moves to
// smithBatchInFlight. No orders, no clock read (PriceBook/RecentProduce empty so
// the windowed sales are 0 regardless of PublishedAt) → byte-stable.
func smithChoosingAtForge() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		// ProductionItem empty — nothing in the works, the idle state the
		// production-choice warrant grants a decision in (LLM-319).
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Blacksmith"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
			"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	warrants := []sim.WarrantMeta{
		{TriggerActorID: ezekielID, Reason: sim.ProductionChoiceWarrantReason{}, SourceEventID: 1},
	}
	return snap, ezekielID, warrants
}

// smithNoInputReadinessBeat is the LLM-509 pin: Ezekiel idle at his forge with nothing
// in the works, offered a no-input (origin) good AND an inputs good in the same "## Your
// trade" scene so one golden contrasts the two affordance branches. shovel is pure labor
// (1 per 4h — no inputs, the live case), so it closes on "and you have everything you
// need"; skillet is forged from the iron he holds, so it keeps the unchanged "and you
// have the makings". Both are craftable (a batch fits under cap and the skillet's iron is
// on hand), so both list — the readiness beat can never render falsely because
// buildForgeChoice drops any input-short good first (LLM-324). shovel sorts before skillet
// (equal zero sell-through, then noun ascending), so the origin beat leads. Byte-stable:
// on shift, idle, empty PriceBook/RecentProduce.
func smithNoInputReadinessBeat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		// One batch of iron on hand covers the skillet input, so both goods are
		// craftable and both list (LLM-324 drops an input-short good before render).
		Inventory: map[sim.ItemKind]int{"skillet": 2, "iron": 1},
		// ProductionItem empty — nothing in the works, the idle production-choice state (LLM-319).
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "shovel", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Blacksmith"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			// shovel: pure labor, no inputs — the origin recipe the readiness beat covers.
			"shovel": {OutputItem: "shovel", OutputQty: 1, RateQty: 1, RatePerHours: 4, WholesalePrice: 3, RetailPrice: 6},
			// skillet: forged from iron — an inputs recipe, the "and you have the makings" branch.
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, Inputs: []sim.RecipeInput{{Item: "iron", Qty: 1}}, WholesalePrice: 5, RetailPrice: 10},
		},
	}
	warrants := []sim.WarrantMeta{
		{TriggerActorID: ezekielID, Reason: sim.ProductionChoiceWarrantReason{}, SourceEventID: 1},
	}
	return snap, ezekielID, warrants
}

// dairyChoosingAtFarm is the LLM-144 trade-neutral-wording pin, re-expressed for
// LLM-319 one-shot batches: a NON-smith multi-output producer (Elizabeth Ellis at
// Ellis Farm: milk + meat + cheese) stands IDLE at her own workplace on shift —
// the same production-choice state smithChoosingAtForge pins for the blacksmith,
// but for a dairy/farm trade. The golden proves the "## Your trade" scene and the
// "thoughts turn to your trade" wake warrant render trade-neutrally — and pins
// all three non-full stock tiers at once (meat empty, cheese low, milk fair) —
// with none of the blacksmith-only "forge" wording a dairywoman was wrongly shown
// (the live Elizabeth cheese scene 019f0969). Mirrors smithChoosingAtForge;
// byte-stable.
func dairyChoosingAtFarm() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		elizabethID = sim.ActorID("elizabeth")
		farm        = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 4, "cheese": 2},
		// ProductionItem empty — idle at the post, the production-choice state (LLM-319).
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "milk", Source: sim.RestockSourceProduce, Max: 10},
			{Item: "cheese", Source: sim.RestockSourceProduce, Max: 8},
			{Item: "meat", Source: sim.RestockSourceProduce, Max: 6},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{elizabethID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "Ellis Farm"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk":   {OutputItem: "milk", OutputQty: 1, RateQty: 1, RatePerHours: 2, WholesalePrice: 1, RetailPrice: 2},
			"cheese": {OutputItem: "cheese", OutputQty: 1, RateQty: 1, RatePerHours: 4, WholesalePrice: 2, RetailPrice: 4},
			"meat":   {OutputItem: "meat", OutputQty: 1, RateQty: 1, RatePerHours: 6, WholesalePrice: 3, RetailPrice: 6},
		},
	}
	warrants := []sim.WarrantMeta{
		{TriggerActorID: elizabethID, Reason: sim.ProductionChoiceWarrantReason{}, SourceEventID: 1},
	}
	return snap, elizabethID, warrants
}

// smithBatchInFlight is the LLM-319 mid-batch steady state: Ezekiel at his own
// forge on shift with a production cycle IN FLIGHT (a nail batch, ~30 minutes of
// base-rate work left) and NO production-choice warrant — shouldChooseProduction
// gates the wake off while a batch runs, and buildForgeChoice returns nil, so
// neither the "## Your trade" scene nor the produce tool re-appear mid-batch.
// What renders instead is the standing self-state line: "You are making a batch
// of Nail — about 30 minutes of work left; it only moves along while you're at
// your post." ItemKinds carry the display labels (LLM-113) so the line names the
// good as the live catalog does. Byte-stable (RemainingSeconds is a snapshot
// field, not a clock read).
func smithBatchInFlight() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		// A produce call opened this cycle: inputs (none — nail is an origin
		// good) consumed, one batch of work owed (LLM-319).
		ProductionItem:             "nail",
		ProductionBatchQty:         1,
		ProductionRemainingSeconds: 1800, // 30 minutes of base-rate work left
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Blacksmith"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
			"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nail":    {Name: "nail", DisplayLabel: "Nail", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails", Category: sim.ItemCategoryCraft},
			"skillet": {Name: "skillet", DisplayLabel: "Skillet", DisplayLabelSingular: "skillet", DisplayLabelPlural: "skillets", Category: sim.ItemCategoryCraft},
		},
	}
	return snap, ezekielID, nil
}

// tavernkeeperMissingInputAtPost is the LLM-257 input-starvation shape,
// re-expressed for LLM-319 one-shot batches: John Ellis, a multi-output
// tavernkeeper (stew + water), stands idle at his tavern on shift holding NO
// sage, so a stew batch cannot start (its inputs are consumed up front). The
// golden pins the missing-inputs leg of the "## Your trade" scene: stew renders
// "A batch would take about an hour, but you'd need more Sage first." — only the
// SHORT input is named (meat, held in full, is omitted) — while the no-input
// water gets the plain batch offer, so the model is steered to a good it CAN
// make or to procuring sage, never into a doomed produce call. The
// production-choice wake warrant fires (water is craftable, so there is a real
// decision to grant). Byte-stable: on shift, idle, empty
// PriceBook/RecentProduce, single-item inventory.
func tavernkeeperMissingInputAtPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID = sim.ActorID("john")
		tavern = sim.StructureID("tavern")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"meat": 10}, // has meat, NO sage → a stew batch can't start
		// ProductionItem empty — idle; a batch missing its inputs can never be in flight (LLM-319).
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "stew", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "water", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"stew":  {OutputItem: "stew", OutputQty: 1, RateQty: 1, RatePerHours: 1, Inputs: []sim.RecipeInput{{Item: "sage", Qty: 2}, {Item: "meat", Qty: 10}}, WholesalePrice: 3, RetailPrice: 5},
			"water": {OutputItem: "water", OutputQty: 1, RateQty: 12, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"stew":  {Name: "stew", DisplayLabel: "Stew", DisplayLabelSingular: "stew", DisplayLabelPlural: "stews", Category: sim.ItemCategoryFood},
			"water": {Name: "water", Capabilities: []string{"portable"}, DisplayLabel: "Water", DisplayLabelSingular: "water", DisplayLabelPlural: "water", Category: sim.ItemCategoryDrink},
			"sage":  {Name: "sage", DisplayLabel: "Sage", DisplayLabelSingular: "sage", DisplayLabelPlural: "sage", Category: sim.ItemCategoryMaterial},
			"meat":  {Name: "meat", DisplayLabel: "Meat", DisplayLabelSingular: "meat", DisplayLabelPlural: "meat", Category: sim.ItemCategoryFood},
		},
	}
	warrants := []sim.WarrantMeta{
		{TriggerActorID: johnID, Reason: sim.ProductionChoiceWarrantReason{}, SourceEventID: 1},
	}
	return snap, johnID, warrants
}

// smithBatchInFlightOffPost pins the LLM-319 away-from-post truthfulness: the same
// producer (Ezekiel) has a nail batch in flight but is NOT at his forge — he is at
// the Tavern after his shift. The batch is a fact wherever he stands (its inputs
// are already spent), so the standing "You are making a batch of nail — about 45
// minutes of work left; it only moves along while you're at your post." line
// RENDERS here — the tail is what tells him progress is paused until he returns
// (produce_tick's gate). The "## Your trade" cue stays gated off away from the
// post. Inverts the retired LLM-121 focus-hiding rule, which suppressed the old
// focus line off-post because continuous production there would have been a lie;
// a paused batch is not. No orders, no clock read → byte-stable.
func smithBatchInFlightOffPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 1140             // 19:00 — off shift, resting at the tavern
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		// The batch travels with him: opened at the forge, paused while he is
		// here at the Tavern (LLM-319). No ItemKinds map, so the line names the
		// raw kind — the catalog-less fallback.
		ProductionItem:             "nail",
		ProductionBatchQty:         1,
		ProductionRemainingSeconds: 2700, // 45 minutes owed — none of it accruing off-post
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
			"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	return snap, ezekielID, nil
}

// smithBarteringAtTavern is the LLM-125 situation: Ezekiel, a smith carrying his
// own wares, stands in the Tavern in company with John Ellis the tavernkeeper —
// the live barter scene. Off his shift, away from the forge, and with nothing in
// the works (LLM-319), so neither the "## Your trade" cue nor the in-flight batch
// line render; what DOES render is the "## What your wares fetch" cue, valuing
// his own-trade goods (nail 1-2, skillet 5-10 from the recipe wholesale-retail
// spread) so a barter has a coin yardstick. Empty PriceBook → no recent-price
// clause; no orders, no clock read → byte-stable.
func smithBarteringAtTavern() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 1140             // 19:00 — off shift, at the tavern
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, johnID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
			"nail":    {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	return snap, ezekielID, nil
}

// keeperResellingInCompany is the LLM-191 reseller leg: Josiah Thorne keeps his
// general store on shift in company with a customer (Martha). His restock policy is
// all `buy` (cheese, milk) and he produces nothing, so the pre-LLM-191 wares-worth
// cue — gated to ProduceEntries() — rendered him NOTHING, leaving a reseller to name
// prices with no anchor (the live 0-coin, empty-sell-through Josiah). He holds
// bought-in stock and his buyer-side PriceBook carries this week's restock purchases
// (cheese 8 coins / 4 units = 2 each, milk 6 coins / 6 units = 1 each), so the
// extended cue values both goods off the recipe spread AND adds the cost-basis
// clause. No seller ring for him → no realized-sale clause.
func keeperResellingInCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		marthaID = sim.ActorID("martha")
		store    = sim.StructureID("general_store")
		supplier = sim.ActorID("ellis_farm")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"cheese": 4, "milk": 6},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cheese", Source: sim.RestockSourceBuy, Max: 10},
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	martha := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Martha Bishop",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             20,
		Needs:             map[sim.NeedKey]int{},
	}
	// Josiah's buyer-side history: he restocked cheese and milk from the farm this
	// week. Keyed by the SELLER (supplier) ring; buyerRecentPurchases reads it by
	// obs.BuyerID == josiah, so the per-unit cost is 2 (cheese) and 1 (milk).
	cheeseBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	cheeseBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	milkBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	milkBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 6, Qty: 6, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, marthaID: martha},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, marthaID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 3, RetailPrice: 6},
			"milk":   {OutputItem: "milk", OutputQty: 1, RateQty: 1, RatePerHours: 2, WholesalePrice: 1, RetailPrice: 3},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: supplier, Item: "cheese"}: cheeseBuys,
			{SellerID: supplier, Item: "milk"}:   milkBuys,
		},
	}
	return snap, josiahID, nil
}

// longPaymentNote is a buyer's note sitting exactly ON the 200-rune cap every pay
// tool enforces on `for` (MaxPayForChars / MaxPayWithItemForChars) — the longest
// note production can actually emit, and 202 bytes, comfortably under the
// renderer's MaxBytesPerWarrant (600), so a correct engine shows it WHOLE.
//
// Deliberately NOT past the old 220-rune warrant cut, because nothing the tools
// can produce ever was: 200 < 220, so the bare cut never fired. That is what made
// it dangerous rather than harmless — the two constants lived in different
// packages with nothing linking them, so the cut sat one raised cap away from
// silently clipping this note mid-word.
const longPaymentNote = "the cheese and the milk both, and for keeping the last of it back for me on " +
	"Tuesday when I hadn't the coin to pay for it right then — my sister is stopping the week and I " +
	"would not have her go without"

// payForTextCapRunes restates handlers.MaxPayForChars (== MaxPayWithItemForChars,
// == the PC route's mirror of it). Stated as a literal rather than imported: the
// perception package does not depend on handlers, and the point here is to pin the
// FIXTURE's claim about itself, not to re-derive the production constant.
const payForTextCapRunes = 200

// TestLongPaymentNoteSitsAtPayCap pins what keeper_paid_with_long_note is actually
// FOR. The scenario's whole value is that its note sits exactly on the cap the pay
// tools enforce — the longest note production can emit — so it exercises the
// boundary the old 220-rune cut sat just past. Nothing else enforces that: an
// innocent edit to the prose would leave the golden passing while it quietly stopped
// covering the boundary the comments still claim it covers (code_review, round 1).
func TestLongPaymentNoteSitsAtPayCap(t *testing.T) {
	if got := utf8.RuneCountInString(longPaymentNote); got != payForTextCapRunes {
		t.Fatalf(
			"longPaymentNote = %d runes, want exactly %d (handlers.MaxPayForChars) — the fixture must sit ON "+
				"the pay tools' cap to be the longest note production can emit; re-word it to length or update both "+
				"this test and the scenario summary",
			got, payForTextCapRunes,
		)
	}
}

// keeperPaidWithLongNote is the LLM-400 fixture: Josiah Thorne's reselling-store
// scene, with a Paid warrant from Martha Bishop carrying a note at the tool's
// length cap.
//
// It pins that the seller perceives the buyer's COMPLETE reason for paying. A bare
// mid-word prefix is the quiet version of the LLM-396 speech defect: the seller
// reads a clipped clause as the whole thought and answers the wrong thing.
//
// Built on keeperResellingInCompany rather than a fresh fixture so the golden diff
// against that scenario isolates exactly one thing — the pay warrant and the line
// it renders. It is also the matrix's only pay-warrant prompt, so it doubles as
// plain coverage of the seller-just-paid situation.
func keeperPaidWithLongNote() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const marthaID = sim.ActorID("martha")
	snap, josiahID, _ := keeperResellingInCompany()
	warrants := []sim.WarrantMeta{{
		TriggerActorID: marthaID,
		SourceActorID:  marthaID,
		SourceEventID:  1,
		Reason: sim.PaidWarrantReason{
			PaidID:  1,
			Buyer:   marthaID,
			Amount:  9,
			ForText: longPaymentNote,
		},
	}}
	return snap, josiahID, warrants
}

// pcCompanyKeeperScenario builds Josiah keeping his store in company with a PC
// customer (Wendy) whose presence is controlled by pcSeen (LLM-342). Shared by the
// present / stepped-away golden pair so the two fixtures differ ONLY in the PC's
// last-seen stamp — isolating the away-vs-present split in the golden diff.
func pcCompanyKeeperScenario(pcSeen *time.Time) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		wendyID  = sim.ActorID("wendy")
		store    = sim.StructureID("general_store")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	wendy := &sim.ActorSnapshot{
		Kind:              sim.KindPC,
		DisplayName:       "Wendy",
		LoginUsername:     "wendy",
		State:             sim.StateIdle,
		InsideStructureID: store,
		CurrentHuddleID:   huddle,
		Coins:             20,
		Needs:             map[sim.NeedKey]int{},
		LastPCSeenAt:      pcSeen,
	}
	snap := &sim.Snapshot{
		PublishedAt:          published,
		LocalMinuteOfDay:     &now,
		NeedThresholds:       sim.NeedThresholds{},
		PCPresenceStaleAfter: sim.DefaultPCPresenceStaleAfter,
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, wendyID: wendy},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, wendyID: {}}},
		},
	}
	return snap, josiahID, nil
}

func keeperWithPresentPCCustomer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	seen := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) // == PublishedAt → fresh, present
	return pcCompanyKeeperScenario(&seen)
}

func keeperWithSteppedAwayPCCustomer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return pcCompanyKeeperScenario(nil) // no client attached this session → stepped away
}

// distributorUnderwaterResale is the LLM-332 / LLM-385 leg: the same reseller shape as
// keeperResellingInCompany, but now with realized SALE history that makes one line
// demonstrably underwater. Josiah the distributor bought milk at ~2 and has been selling
// it at ~1 (the live −51-coin milk leak that drove him to cut the line rather than carry
// a loss), while cheese carries a healthy markup (bought ~2, sold ~4). The golden pins
// BOTH branches: the underwater milk line gains the caution + two-lever hint ("negotiate
// lower costs or raise your price"); the healthy cheese line carries NO caution at all
// (LLM-385: the caution fires only for a good sold at or below cost — pre-LLM-385 every
// resold line carried it as boilerplate). In company with Martha so the cue renders.
func distributorUnderwaterResale() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		marthaID = sim.ActorID("martha")
		store    = sim.StructureID("general_store")
		supplier = sim.ActorID("ellis_farm")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00-18:00
	now := 720              // 12:00 — on shift, at the store
	published := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"cheese": 4, "milk": 6},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cheese", Source: sim.RestockSourceBuy, Max: 10},
			{Item: "milk", Source: sim.RestockSourceBuy, Max: 12},
		}},
	}
	martha := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Martha Bishop",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             20,
		Needs:             map[sim.NeedKey]int{},
	}
	// Buyer-side cost basis (keyed by the SELLER supplier ring; buyerRecentPurchases
	// reads obs.BuyerID == josiah): cheese 8/4 = 2 each, milk 8/4 = 2 each.
	cheeseBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	cheeseBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	milkBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	milkBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 8, Qty: 4, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	// Seller-side realized sales (keyed by josiah as SELLER): cheese 16/4 = 4 each (a
	// healthy markup over its cost 2 → NOT sold below cost, so NO caution at all,
	// LLM-385), milk 4/4 = 1 each (BELOW its cost 2 → caution + two-lever hint).
	cheeseSales := sim.NewRingBuffer[sim.PriceObservation](8)
	cheeseSales.Push(sim.PriceObservation{BuyerID: marthaID, Amount: 16, Qty: 4, Consumers: 1, At: published.Add(-12 * time.Hour)})
	milkSales := sim.NewRingBuffer[sim.PriceObservation](8)
	milkSales.Push(sim.PriceObservation{BuyerID: marthaID, Amount: 4, Qty: 4, Consumers: 1, At: published.Add(-12 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, marthaID: martha},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, marthaID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 3, RetailPrice: 6},
			"milk":   {OutputItem: "milk", OutputQty: 1, RateQty: 1, RatePerHours: 2, WholesalePrice: 1, RetailPrice: 3},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: supplier, Item: "cheese"}: cheeseBuys,
			{SellerID: supplier, Item: "milk"}:   milkBuys,
			{SellerID: josiahID, Item: "cheese"}: cheeseSales,
			{SellerID: josiahID, Item: "milk"}:   milkSales,
		},
	}
	return snap, josiahID, nil
}

// innkeeperPricingWithMakingsCost is the LLM-226 producer cost-of-goods leg: Hannah
// Boggs keeps her inn on shift in company with a guest, producing porridge from a
// recipe with REAL inputs (10 bowls from 3 milk + 5 water — the live catalog shape).
// Before LLM-226 the wares-worth cue gave a producer no cost anchor at all, so she
// could price below cost without knowing it (live: porridge quoted at 1 coin against
// an 0.8-coin makings cost). The golden pins the makings clause: with no purchase
// history the inputs price from catalog wholesale (3×1 + 5×1 = 8 a batch), and 8/10
// is spoken as "nearly 1 coin each" — rounded UP in prose, never down to a
// break-even-erasing "about 1". A fact with no pricing directive (LLM-227). She has
// a porridge batch in flight (LLM-319) — the realistic keeping-the-inn-fed state —
// so the standing in-progress line renders and the "## Your trade" cue stays out of
// this company-focused golden.
func innkeeperPricingWithMakingsCost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		guestID  = sim.ActorID("ezekiel")
		inn      = sim.StructureID("inn")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1200 // 06:00-20:00 — the innkeeper day shift
	now := 480              // 08:00 — breakfast custom
	published := time.Date(2026, 6, 25, 8, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             10,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"porridge": 30},
		// A porridge batch in flight (LLM-319): inputs already in the pot, so the
		// trade cue yields to the standing in-progress line for this golden.
		ProductionItem:             "porridge",
		ProductionBatchQty:         10,
		ProductionRemainingSeconds: 2400, // about 40 minutes of work left
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	guest := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             15,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, guestID: guest},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{hannahID: {}, guestID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, RateQty: 8, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", OutputQty: 1, RateQty: 12, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
		},
	}
	return snap, hannahID, nil
}

// innkeeperFriedMeatBelowMakingsCost is the LLM-475 retail leg — the live Hannah
// Boggs turn (virtual_agent_calls 104810, 2026-07-19). Her fried_meat recipe takes
// 1 meat (bought at ~5) AND a skillet, a durable TOOL. Both defects fired on one
// line: the cost anchor charged the whole 5-coin skillet against every single
// serving, rendering "the makings run you about 10 coins each" against a true cost
// of 5 — and nothing compared that figure to the ~3 coins she had actually been
// getting. She sat at 0 coins, trading briskly, every honest sale reading as a loss
// against her never-sell-below-cost coda while the real loss went unnamed. The
// golden pins the corrected line: the tool costs nothing, the meat alone is the
// makings, and the realized 3-against-5 earns the loss verdict.
func innkeeperFriedMeatBelowMakingsCost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		guestID  = sim.ActorID("ezekiel")
		inn      = sim.StructureID("inn")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1200
	now := 480
	published := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             10,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"fried_meat": 12, "meat": 6, "skillet": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "fried_meat", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	guest := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             15,
		Needs:             map[sim.NeedKey]int{},
	}
	// Her realized fried_meat sales: 8 servings for 25 coins — about 3 each, under
	// the 5-coin meat that goes into them.
	sales := sim.NewRingBuffer[sim.PriceObservation](8)
	sales.Push(sim.PriceObservation{BuyerID: guestID, Amount: 25, Qty: 8, Consumers: 1, At: published.Add(-30 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, guestID: guest},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{hannahID: {}, guestID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"fried_meat": {OutputItem: "fried_meat", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 4, RetailPrice: 7,
				Inputs: []sim.RecipeInput{{Item: "meat", Qty: 1}, {Item: "skillet", Qty: 1}}},
			"meat":    {OutputItem: "meat", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 5, RetailPrice: 8},
			"skillet": {OutputItem: "skillet", OutputQty: 1, RateQty: 1, RatePerHours: 3, WholesalePrice: 5, RetailPrice: 10},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"fried_meat": {Name: "fried_meat", Capabilities: []string{"portable"}, DisplayLabel: "Fried Meat",
				DisplayLabelSingular: "serving of fried meat", DisplayLabelPlural: "servings of fried meat",
				Category: sim.ItemCategoryFood},
			"meat": {Name: "meat", Capabilities: []string{"portable"}, DisplayLabel: "Meat",
				DisplayLabelSingular: "cut of meat", DisplayLabelPlural: "cuts of meat",
				Category: sim.ItemCategoryFood},
			// The skillet is the point of the scenario: a durable tool, so it is a
			// presence requirement that wears, never a cost of the makings.
			"skillet": {Name: "skillet", Capabilities: []string{"portable"}, DisplayLabel: "Skillet",
				DisplayLabelSingular: "skillet", DisplayLabelPlural: "skillets",
				DurabilityUses: 20},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: hannahID, Item: "fried_meat"}: sales,
		},
	}
	return snap, hannahID, nil
}

// millerFlourWholesaleEarnsNothing is the LLM-475 wholesale leg — the live Joseph
// Scott turn (virtual_agent_calls 104618, re-observed verbatim 2026-07-20). The
// LLM-292 wholesale-channel branch replaced the whole clause set, cost anchor
// included, so a wholesale producer was the one class of maker never shown its own
// margin: his flour line named what the shop paid him (1 coin) and what folk paid
// on the shelf (2), and never what the grinding cost. He offered five sacks of
// flour for five sheaves of wheat — his recipe's exact input, a full day's grinding
// for nothing — and called it fair; and because the line carries no cost figure,
// the 2026-07-19 flour reprice to wholesale 3 could not reach him at all (a week of
// sales at 1.00 flat after it landed). The golden pins the restored cost sentence
// and the break-even verdict in Jeff's target shape.
func millerFlourWholesaleEarnsNothing() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josephID = sim.ActorID("joseph")
		walkerID = sim.ActorID("silence")
		josiahID = sim.ActorID("josiah")
		commons  = sim.StructureID("commons")
		mill     = sim.StructureID("mill")
		store    = sim.StructureID("general_store")
		huddle   = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	joseph := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Joseph Scott",
		Role:              "miller",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		WorkStructureID:   mill,
		Coins:             16,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"flour": 18, "wheat": 6},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "flour", Source: sim.RestockSourceProduce, Max: 30},
		}},
		Acquaintances: map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	walker := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             22,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Joseph Scott": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: store,
		WorkStructureID:   store,
	}
	// Bulk rate observed: his own flour sales to the shop, 24 sacks for 24 coins.
	millSales := sim.NewRingBuffer[sim.PriceObservation](8)
	millSales.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 24, Qty: 24, Consumers: 1, At: published.Add(-18 * time.Hour)})
	// Shelf rate observed: Josiah's flour resales to folk at 2 each.
	shopSales := sim.NewRingBuffer[sim.PriceObservation](8)
	shopSales.Push(sim.PriceObservation{BuyerID: walkerID, Amount: 12, Qty: 6, Consumers: 1, At: published.Add(-8 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{josephID: joseph, walkerID: walker, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
			mill:    plainStructure(mill, "Mill"),
			store:   plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(mill):  {ID: sim.VillageObjectID(mill), OwnerActorID: josephID, Tags: []string{sim.TagWholesaler}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), OwnerActorID: josiahID, Tags: []string{sim.TagDistributor}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josephID: {}, walkerID: {}}},
		},
		// The live 5-wheat-to-5-flour treadmill, at the post-reprice band (w3/r4).
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"flour": {OutputItem: "flour", OutputQty: 5, RateQty: 5, RatePerHours: 2, WholesalePrice: 3, RetailPrice: 4,
				Inputs: []sim.RecipeInput{{Item: "wheat", Qty: 5}}},
			"wheat": {OutputItem: "wheat", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"flour": {Name: "flour", Capabilities: []string{"portable"}, DisplayLabel: "Flour",
				DisplayLabelSingular: "sack of flour", DisplayLabelPlural: "sacks of flour",
				Category: sim.ItemCategoryFood},
			"wheat": {Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryFood},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: josephID, Item: "flour"}: millSales,
			{SellerID: josiahID, Item: "flour"}: shopSales,
		},
	}
	return snap, josephID, nil
}

// producerInputBelowBatchFloorReorders is the LLM-279 fixture: Hannah Boggs makes
// porridge (3 milk + 5 water per 10-bowl batch) and holds water at 4 — below one
// 5-unit batch (she can't cover the next batch) but above the cap fraction (derived
// water cap 15 → the old rule fired only below 3.75, so she was never reordered).
// A well-keeper sells water, so the buy path is actionable. Milk at 9 is above its
// 2×3=6 floor, so it stays silent. Pins that the batch floor (2×5=10) surfaces both
// the "## Restocking" walk-to and the "## Keeping up production" runway for water.
// She has a porridge batch in flight (LLM-319) — the state in which input runway
// matters most — which also keeps the "## Your trade" cue out of this
// restock-focused golden. Clock-free: no pending orders/deliveries.
func producerInputBelowBatchFloorReorders() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID   = sim.ActorID("hannah")
		wellKeeper = sim.ActorID("osborne")
		inn        = sim.StructureID("inn")
		well       = sim.StructureID("well")
	)
	start, end := 360, 1200 // 06:00-20:00 innkeeper day shift
	now := 480              // 08:00
	published := time.Date(2026, 7, 4, 8, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             20,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"porridge": 30, "milk": 9, "water": 4},
		// Mid-batch (LLM-319): the input-runway question is live exactly while
		// she is producing, and the in-flight state gates the trade cue off.
		ProductionItem:             "porridge",
		ProductionBatchQty:         10,
		ProductionRemainingSeconds: 2400,
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	osborne := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Goodwife Osborne",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 400, Y: 400},
		WorkStructureID: well,
		Inventory:       map[sim.ItemKind]int{"water": 30},
		// Produces water, so she's a first-hand supplier (LLM-252), not a reseller.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "water", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, wellKeeper: osborne},
		Structures: map[sim.StructureID]*sim.Structure{
			inn:  plainStructure(inn, "Inn"),
			well: plainStructure(well, "Village Well"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {Name: "porridge", DisplayLabel: "porridge", Category: sim.ItemCategoryFood},
			"milk":     {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"water":    {Name: "water", DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, RateQty: 8, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
			"milk":  {OutputItem: "milk", OutputQty: 1, RateQty: 4, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
			"water": {OutputItem: "water", OutputQty: 1, RateQty: 12, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 1},
		},
	}
	return snap, hannahID, nil
}

// innkeeperTradeSceneBase is the shared LLM-319 single-output-producer fixture:
// Hannah Boggs, porridge only (10 bowls a batch from 3 milk + 5 water, ~75 min of
// work — the live catalog shape), alone at her inn on shift. The three LLM-319
// goldens (idle scene / batch in flight / batch landed) each start here and set
// only the production state, so their diffs isolate exactly what the cycle state
// changes in the prompt. No vendors exist, so the empty input stocks derive no
// restock/runway sections (LLM-260 gates demand on an actionable buy path);
// PriceBook/RecentProduce empty and no clock read → byte-stable.
func innkeeperTradeSceneBase() (*sim.Snapshot, sim.ActorID) {
	const (
		hannahID = sim.ActorID("hannah")
		inn      = sim.StructureID("inn")
	)
	start, end := 360, 1200 // 06:00-20:00 — the innkeeper day shift
	now := 480              // 08:00 — breakfast custom ahead
	published := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 3, "water": 5}, // exactly one batch's makings, no porridge
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "Boggs Inn"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 10, RateQty: 8, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2,
				Inputs: []sim.RecipeInput{{Item: "milk", Qty: 3}, {Item: "water", Qty: 5}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {Name: "porridge", DisplayLabel: "porridge", DisplayLabelSingular: "bowl of porridge", DisplayLabelPlural: "porridge", Category: sim.ItemCategoryFood},
			"milk":     {Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink},
			"water":    {Name: "water", DisplayLabel: "water", Category: sim.ItemCategoryDrink},
		},
	}
	return snap, hannahID
}

// innkeeperIdleAtPostTradeScene is the LLM-319 headline golden: a SINGLE-output
// producer idle at her post now gets the "## Your trade" scene (the retired
// forge-choice cue was multi-output-gated, so a one-good keeper never saw a
// production decision at all — she was auto-produced at). Empty stores, no sales
// this window, makings on hand → the quietest full scene: "You have no porridge
// on hand, and none sold this past week. A batch — 10 more — takes about an hour
// and a quarter, and you have the makings.", the neutral close naming the produce
// tool, and the production-choice warrant's "Your thoughts turn to your trade —
// nothing is in the works right now." beat.
func innkeeperIdleAtPostTradeScene() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, hannahID := innkeeperTradeSceneBase()
	warrants := []sim.WarrantMeta{
		{TriggerActorID: hannahID, Reason: sim.ProductionChoiceWarrantReason{}, SourceEventID: 1},
	}
	return snap, hannahID, warrants
}

// innkeeperBatchInFlight is the mid-batch half of the LLM-319 pair: the same
// innkeeper called produce — the makings went into the pot (inventory emptied),
// ProductionItem opened — and ~40 minutes of base-rate work remain. The standing
// "You are making a batch of porridge — about 40 minutes of work left; it only
// moves along while you're at your post." line renders, and the "## Your trade"
// cue (with it the produce tool) is gone until the batch lands. No warrant — a
// routine mid-batch check-in.
func innkeeperBatchInFlight() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, hannahID := innkeeperTradeSceneBase()
	hannah := snap.Actors[hannahID]
	hannah.Inventory = map[sim.ItemKind]int{} // the makings were consumed at produce time
	hannah.ProductionItem = "porridge"
	hannah.ProductionBatchQty = 10
	hannah.ProductionRemainingSeconds = 2400 // about 40 minutes of the ~75-minute cycle left
	return snap, hannahID, nil
}

// innkeeperBatchDoneBeat is the LLM-319 completion beat: the batch just LANDED —
// 10 porridge minted into her stores, the in-flight window cleared — and the
// ProductionCycleCompleted reactor woke her with the pre-rendered narration
// (ProductionCompletionNarration, the SourceActivityCompleted posture). The
// golden pins the "You finish the batch — 10 porridge ready in your stores."
// warrant line (renderNarrationWarrantLine's WarrantKindProductionDone path) and
// that the SAME prompt re-opens the "## Your trade" scene — stores now "running
// low" (10 of 30) with a fresh batch's makings still on hand, so the offer reads
// "and you have the makings" — granting the next go/no-go in the very tick that
// reports the landing. She holds one batch of milk+water because LLM-324 only
// offers a good the actor could actually start: a beat that showed "running low …
// but you'd need more milk and water first. Start a batch with produce" would be
// the very at-cap/starved self-contradiction LLM-324 removes.
func innkeeperBatchDoneBeat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, hannahID := innkeeperTradeSceneBase()
	hannah := snap.Actors[hannahID]
	hannah.Inventory = map[sim.ItemKind]int{"porridge": 10, "milk": 3, "water": 5} // the landed batch, plus one more batch's makings on hand
	hannah.RecentProduce = []sim.ProduceEvent{
		{Item: "porridge", Qty: 10, At: snap.PublishedAt.Add(-time.Minute)},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: hannahID,
			Reason: sim.ProductionDoneWarrantReason{
				Item:          "porridge",
				Qty:           10,
				NarrationText: sim.ProductionCompletionNarration("porridge", 10),
			},
			SourceEventID: 1,
		},
	}
	return snap, hannahID, warrants
}

// producerAllGoodsAtCap is the LLM-324 regression pin: Moses James, a two-good
// farm producer (carrots + wheat), stands idle inside his own farm on shift with
// BOTH goods at their carry cap — no batch of either can start. Before LLM-324 the
// "## Your trade" cue still listed both ("no room for another batch") and closed
// with "Start a batch with produce", advertising the produce tool into an
// all-at-cap reject loop (live: Moses James burned 6-iteration budget_forced ticks
// on produce→reject). buildForgeChoice now drops both capped goods and returns
// nil, so the golden has NO "## Your trade" section and the produce tool is not
// offered — he is left to see to other things. No production-choice warrant:
// shouldChooseProduction gates the wake off when nothing's craftable, the same
// craftableNow the cue now mirrors. He woke on a plain arrival, not a trade
// prompt.
func producerAllGoodsAtCap() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID = sim.ActorID("moses")
		farm    = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"carrot": 30, "wheat": 30}, // both at cap — nothing craftable
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "carrot", Source: sim.RestockSourceProduce, Max: 30},
			{Item: "wheat", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "James Farm"),
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"carrot": {OutputItem: "carrot", OutputQty: 5, RateQty: 5, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
			"wheat":  {OutputItem: "wheat", OutputQty: 5, RateQty: 5, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	return snap, mosesID, nil
}

// resellerArrivesAtSupplierBuyHereNoHuddle is the LLM-286 arrival tick: John Ellis,
// a tavernkeeper reselling ale, stepped out mid-shift and walked to the Brewery to
// restock. He stands INSIDE it with the brewer Anders (a first-hand ale producer),
// but no huddle exists yet — one forms only when someone speaks. pay_with_item
// bootstraps the co-located huddle on the call itself (withHuddleBootstrap,
// ZBBS-HOME-400), so the keeper is reachable this very tick. The golden pins that the
// "## Restocking" section renders the concrete "Anders Brewer is here with you and
// sells ale. Buy it now …" imperative rather than the "No seller is here now — use
// move_to …" walk-to list, which before LLM-286 wrongly told him to walk to the
// Brewery he already stood in (live: zbbs-john-ellis, virtual_agent_calls id 63123).
func resellerArrivesAtSupplierBuyHereNoHuddle() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID   = sim.ActorID("john")
		brewerID = sim.ActorID("anders")
		tavern   = sim.StructureID("tavern")
		brewery  = sim.StructureID("brewery")
	)
	start, end := 360, 1080 // 06:00-18:00 tavern day shift
	now := 720              // 12:00 — on shift, stepped out to restock
	published := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: brewery, // arrived at the supplier's shop, away from his own post
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"ale": 1}, // below the reorder threshold (cap 20)
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "ale", Source: sim.RestockSourceBuy, Max: 20},
		}},
		// World-computed awareness: unhuddled, John's co-located audience in the
		// Brewery is Anders — the same structure scope pay_with_item's bootstrap
		// resolves. Stamped here so the "## Around you" line names Anders as it does
		// in production (hand-built snapshots don't get the world-side republish),
		// keeping the pinned prompt internally consistent with the buy-here cue.
		ColocatedAudienceIDs: []sim.ActorID{brewerID},
	}
	anders := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Anders Brewer",
		Role:              "brewer",
		State:             sim.StateIdle,
		WorkStructureID:   brewery,
		InsideStructureID: brewery, // working inside, co-present with John — no huddle yet
		Coins:             10,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"ale": 40},
		// Produces ale, so he is a first-hand supplier (LLM-252), not a reseller.
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "ale", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{johnID: john, brewerID: anders},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern:  plainStructure(tavern, "Tavern"),
			brewery: plainStructure(brewery, "The Brewery"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"ale": {Name: "ale", Capabilities: []string{"portable"}, DisplayLabel: "ale", Category: sim.ItemCategoryDrink},
		},
		RestockReorderPct: 25,
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"ale": {OutputItem: "ale", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	return snap, johnID, nil
}

// resellerCoPresentSageSellerPresent is the LLM-308 goad foil: Elizabeth Ellis, a shopkeeper
// reselling sage, has stepped into Josiah Thorne's General Store to restock and shares a huddle
// with him. Josiah forages sage (a first-hand supplier, LLM-252) and holds 12; Elizabeth is out
// of sage (cap 4, so a 4-unit headroom) and carries a healthy 61 coins with no prior offers on
// the ledger — so the "## Restocking" co-present branch issues the clean "Buy it now … a qty up
// to 4" imperative. Its twins derive the low-stock cap (Josiah holds 1) and the standoff soften
// (two declines) from this base, the reseller counterpart of farmOwnerOwesUpkeepSellerPresent.
// Josiah forages the sage he sells purely to qualify as a supplier; the scenario exercises the
// co-present standoff/cap logic, not the supplier-qualification path (covered elsewhere).
func resellerCoPresentSageSellerPresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		elizabethID = sim.ActorID("elizabeth")
		josiahID    = sim.ActorID("josiah")
		ellisStore  = sim.StructureID("ellis_store")
		store       = sim.StructureID("general_store")
		huddle      = sim.HuddleID("store_huddle")
	)
	start, end := 360, 1080 // 06:00-18:00 shopkeeping day
	now := 720              // 12:00 — on shift, stepped out to restock
	published := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   ellisStore,
		InsideStructureID: store, // visiting Josiah's store to restock, away from her own post
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             61,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"sage": 0}, // below the reorder threshold (cap 4)
		RestockPolicy:     buyPolicy("sage", 4),
		Acquaintances:     map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "storekeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		CurrentHuddleID:   huddle,
		Coins:             10,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"sage": 12},
		// Forages sage, so he is a first-hand supplier (LLM-252), not a reseller of it.
		RestockPolicy: foragePolicy("sage", 40),
		Acquaintances: map[string]sim.Acquaintance{"Elizabeth Ellis": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{elizabethID: elizabeth, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			ellisStore: plainStructure(ellisStore, "Ellis Store"),
			store:      plainStructure(store, "General Store"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"sage": {Name: "sage", DisplayLabel: "sage", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"sage": {OutputItem: "sage", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
	}
	return snap, elizabethID, nil
}

// resellerCoPresentSageSellerLowStock is the LLM-308 stock-cap arm: the
// resellerCoPresentSageSellerPresent setup, but Josiah holds only 1 sage against the 4
// Elizabeth's shelf has room for. Affordable and no prior offers, so the buy still stands — the
// render caps the ask at his stock instead of goading the full headroom (the reseller counterpart
// of the live smith-held-only-1-nail case).
func resellerCoPresentSageSellerLowStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := resellerCoPresentSageSellerPresent()
	snap.Actors["josiah"].Inventory["sage"] = 1
	return snap, actorID, warrants
}

// resellerCoPresentSageStandoff is the LLM-308 standoff arm — the live sage loop distilled: the
// resellerCoPresentSageSellerPresent setup with two prior sage offers to Josiah already declined
// IN THIS HUDDLE on the pay ledger, resolved a minute ago (the standoff threshold, inside
// recentlyResolvedOfferWindow). Josiah stays well-stocked at 12, so the softening is driven purely
// by the dead-ended negotiation (coPresentBuyStandoff), not the stock cap — the reseller twin of
// farmOwnerStandoffDeclinedShovels / ownerStandoffDeclinedNails. Elizabeth is not in conserve mode
// (MerchantCoinFloor is unset → the working-capital gate is off), isolating the standoff path.
func resellerCoPresentSageStandoff() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := resellerCoPresentSageSellerPresent()
	// Two declined sage offers to Josiah in the current huddle, resolved a minute ago. Amount 6
	// for 3 sage on each (the live 3-for offers), so the LLM-296 settled-offer close reads "Your
	// offer of 6 coins …" rather than the fixture artifact "of nothing". Declined is terminal, so
	// no ExpiresAt is needed.
	resolved := snap.PublishedAt.Add(-1 * time.Minute)
	snap.PayLedger = map[sim.LedgerID]*sim.PayLedgerEntry{
		1: {ID: 1, BuyerID: actorID, SellerID: "josiah", ItemKind: "sage", Qty: 3, Amount: 6, State: sim.PayLedgerStateDeclined, HuddleID: "store_huddle", ResolvedAt: resolved},
		2: {ID: 2, BuyerID: actorID, SellerID: "josiah", ItemKind: "sage", Qty: 3, Amount: 6, State: sim.PayLedgerStateDeclined, HuddleID: "store_huddle", ResolvedAt: resolved},
	}
	return snap, actorID, warrants
}

// distributorOverbuyingBelowResale is the LLM-385 buy-side golden (see the scenario
// summary): a reseller low on milk with a walk-to supplier, whose own price book shows
// a resale rate at the going rate (the resale-ceiling clause fires) and a week of buying
// 35 against selling 9 (the over-buying steer fires). Solo, so the wares-fetch cue stays
// out and the "## Restocking" section renders on its own.
func distributorOverbuyingBelowResale() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		elizID   = sim.ActorID("elizabeth")
		store    = sim.StructureID("general_store")
		farm     = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00 shopkeeping day
	now := 720              // 12:00 — on shift at the store
	published := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "storekeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             50, // ≥ the ~40 last-paid bundle, so Ellis Farm survives the LLM-216 affordability drop
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 4}, // below the reorder threshold (cap 20)
		RestockPolicy:     buyPolicy("milk", 20),
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm, // at her farm, NOT co-present with Josiah — a walk-to supplier
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"milk": 40},
		RestockPolicy:     producePolicy("milk", 40), // first-hand producer (LLM-252)
	}
	// Josiah as SELLER of milk: 9 units for 12 coins → resale rate 1.3 → 1.
	sellPB := sim.NewRingBuffer[sim.PriceObservation](20)
	sellPB.Push(sim.PriceObservation{BuyerID: "cust", Amount: 12, Qty: 9, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	// Josiah's in-window PURCHASE from Elizabeth: 35 units for 40 coins. Drives
	// boughtUnits = 35 → over-buying, and (as Elizabeth's own sales) the observed
	// going-rate anchor = 40/35 → 1.
	buyPB := sim.NewRingBuffer[sim.PriceObservation](20)
	buyPB.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 40, Qty: 35, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, elizID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"milk": {Name: "milk", Capabilities: []string{"portable"}, DisplayLabel: "milk", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"milk": {OutputItem: "milk", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: josiahID, Item: "milk"}: sellPB,
			{SellerID: elizID, Item: "milk"}:   buyPB,
		},
	}
	return snap, josiahID, nil
}

// keeperSelfConsumesZeroStock is the LLM-424 live case: Josiah Thorne, storekeeper,
// SOLD OUT of cheese (0 of a 6 cap) yet buying it far faster than he sells — bought ~25
// this week, sold only ~5 — because he eats his own stock (no engine spoilage, so the
// buy>>sell gap left through non-sale channels). Elizabeth at the farm is a first-hand
// cheese producer and a walk-to supplier (not co-present), and Josiah's purse covers his
// ~25-coin last-paid bundle so the farm survives the LLM-216 affordability drop. Drives
// UsingOwnStock (25 >= 4 and 25 >= 2×5+1 = 11) with a clean restock path, so the golden
// pins the self-use accounting beside a normal buy cue and the ABSENCE of the old
// "buy sparingly" hold-off that had the model confabulating phantom unsold stock.
func keeperSelfConsumesZeroStock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		elizID   = sim.ActorID("elizabeth")
		store    = sim.StructureID("general_store")
		farm     = sim.StructureID("ellis_farm")
	)
	start, end := 360, 1080 // 06:00-18:00 shopkeeping day
	now := 720              // 12:00 — on shift at the store
	published := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "storekeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             50, // ≥ the ~25 last-paid bundle, so Ellis Farm survives the affordability drop
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"cheese": 0}, // sold out — 0 of a 6 cap
		RestockPolicy:     buyPolicy("cheese", 6),
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm, // at her farm, NOT co-present with Josiah — a walk-to supplier
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"cheese": 40},
		RestockPolicy:     producePolicy("cheese", 40), // first-hand producer (LLM-252)
	}
	// Josiah as SELLER of cheese: 5 units for 10 coins → sold ~5 this window.
	sellPB := sim.NewRingBuffer[sim.PriceObservation](20)
	sellPB.Push(sim.PriceObservation{BuyerID: "cust", Amount: 10, Qty: 5, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	// Josiah's in-window PURCHASE from Elizabeth: 25 units for 25 coins → boughtUnits = 25
	// (UsingOwnStock), and (as Elizabeth's own sales) the observed going-rate anchor = 1.
	buyPB := sim.NewRingBuffer[sim.PriceObservation](20)
	buyPB.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 25, Qty: 25, Consumers: 1, At: published.Add(-1 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, elizID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"cheese": {Name: "cheese", Capabilities: []string{"portable"}, DisplayLabel: "cheese", Category: sim.ItemCategoryFood},
		},
		RestockReorderPct: 25,
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"cheese": {OutputItem: "cheese", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 2},
		},
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: josiahID, Item: "cheese"}: sellPB,
			{SellerID: elizID, Item: "cheese"}:   buyPB,
		},
	}
	return snap, josiahID, nil
}

// keeperNotPitchingMakersOwnWare is the LLM-171 seller side: John Ellis keeps
// his tavern (on shift, at post) co-present with Ezekiel Crane the smith, and
// John's stock includes skillet + nail he BOUGHT from Ezekiel. The "## Custom
// at hand" cue lists those goods to pitch, so the golden pins the producer-note
// line — "Ezekiel Crane makes nail and skillet themselves — don't pitch those
// back to their own maker" — that steers the keeper off selling a smith his own
// ware back (the live buy-back, where John read Ezekiel's sell-offer as a buy
// and quoted skillets at him). A co-present customer who makes none of the goods
// would draw no such note.
func keeperNotPitchingMakersOwnWare() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1320 // 06:00–22:00, on shift in the evening
	now := 1140             // 19:00 — keeping the tavern, a customer present
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	john := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "John Ellis",
		Role:               "tavernkeeper",
		State:              sim.StateIdle,
		WorkStructureID:    tavern,
		InsideStructureID:  tavern,
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		CurrentHuddleID:    huddle,
		Coins:              267,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{},
		// Skillet + nail here came FROM Ezekiel — the reseller stock the cue would
		// otherwise pitch straight back at its maker.
		Inventory:     map[sim.ItemKind]int{"skillet": 4, "nail": 38},
		Acquaintances: map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{johnID: john, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{johnID: {}, ezekielID: {}}},
		},
	}
	return snap, johnID, nil
}

// sellerHuddledWithLaboringPeer is the LLM-231 fixture: John Ellis (seller, at his own
// tavern with stew to sell) is huddled with Patience Walker — mid-job for Josiah Thorne
// (a Working LaborOffer, so StateLaboring) — and Grace Bishop, a free customer. It exercises
// both halves of the fix: the busy annotation on Patience in "## Around you", and her absence
// from the seller offer cue (which still lists the free Grace). Josiah is present only as the
// employer named by the annotation (not co-present). No clock-relative churn beyond the fixed
// PublishedAt → byte-stable.
func sellerHuddledWithLaboringPeer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID     = sim.ActorID("john")
		patienceID = sim.ActorID("patience")
		graceID    = sim.ActorID("grace")
		josiahID   = sim.ActorID("josiah")
		tavern     = sim.StructureID("tavern")
		huddle     = sim.HuddleID("h1")
	)
	start, end := 360, 1320 // 06:00–22:00, on shift in the evening
	now := 1140             // 19:00 — keeping the tavern, company present
	published := time.Date(2026, 7, 3, 19, 0, 0, 0, time.UTC)
	workingUntil := published.Add(90 * time.Minute)
	john := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "John Ellis",
		Role:               "tavernkeeper",
		State:              sim.StateIdle,
		WorkStructureID:    tavern,
		InsideStructureID:  tavern,
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		CurrentHuddleID:    huddle,
		Coins:              267,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{},
		Inventory:          map[sim.ItemKind]int{"stew": 4},
		Acquaintances: map[string]sim.Acquaintance{
			"Patience Walker": {}, "Grace Bishop": {}, "Josiah Thorne": {},
		},
	}
	patience := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Patience Walker",
		Role:              "laborer",
		State:             sim.StateLaboring,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	grace := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Grace Bishop",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	josiah := &sim.ActorSnapshot{
		Kind:        sim.KindNPCStateful,
		DisplayName: "Josiah Thorne",
		Role:        "shopkeeper",
		State:       sim.StateIdle,
		Needs:       map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			johnID: john, patienceID: patience, graceID: grace, josiahID: josiah,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{johnID: {}, patienceID: {}, graceID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			5: {ID: 5, WorkerID: patienceID, EmployerID: josiahID, State: sim.LaborStateWorking, WorkingUntil: &workingUntil, HuddleID: huddle},
		},
	}
	return snap, johnID, nil
}

// sellerEmployingOwnLaboringWorker is the LLM-231 employer-seller fixture: John Ellis
// (seller, at his own tavern with stew to sell) is the EMPLOYER of a co-present laboring
// worker, Silence Walker (a Working LaborOffer with John as employer), and is also huddled
// with Grace Bishop, a free customer. It pins the two-way split of the fix: Silence is still
// dropped from the offer cue (exclusion is truthful for every observer, employer included),
// while she carries NO busy annotation in "## Around you" (that is bystander-only — the
// employer sees the "## Workers currently working for you" cue instead). Byte-stable.
func sellerEmployingOwnLaboringWorker() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID    = sim.ActorID("john")
		silenceID = sim.ActorID("silence")
		graceID   = sim.ActorID("grace")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1320 // 06:00–22:00, on shift in the evening
	now := 1140             // 19:00 — keeping the tavern, company present
	published := time.Date(2026, 7, 3, 19, 0, 0, 0, time.UTC)
	workingUntil := published.Add(90 * time.Minute)
	john := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "John Ellis",
		Role:               "tavernkeeper",
		State:              sim.StateIdle,
		WorkStructureID:    tavern,
		InsideStructureID:  tavern,
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		CurrentHuddleID:    huddle,
		Coins:              267,
		Needs:              map[sim.NeedKey]int{},
		BusinessownerState: &sim.BusinessownerState{},
		Inventory:          map[sim.ItemKind]int{"stew": 4},
		Acquaintances: map[string]sim.Acquaintance{
			"Silence Walker": {}, "Grace Bishop": {},
		},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Silence Walker",
		Role:              "laborer",
		State:             sim.StateLaboring,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	grace := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Grace Bishop",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			johnID: john, silenceID: silence, graceID: grace,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{johnID: {}, silenceID: {}, graceID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			5: {ID: 5, WorkerID: silenceID, EmployerID: johnID, Reward: 2, State: sim.LaborStateWorking, WorkingUntil: &workingUntil, HuddleID: huddle},
		},
	}
	return snap, johnID, nil
}

// makerOfferedOwnWareBuyQuote is the LLM-171 buyer side: Ezekiel Crane the smith
// (skillet at his cap of 5, which he MAKES) is co-present with John Ellis, who
// has posted a targeted skillet quote at him for 2 coins — the mis-pitched
// buy-back quote from the live trace. The golden pins that the quote warrant
// line withholds the actionable "pay_with_item with quote_id" take and instead
// steers "these are wares you make yourself … decline", so a mis-pitched quote
// can't close the buy-back loop. A quote for a good the buyer does NOT make
// keeps its normal take.
func makerOfferedOwnWareBuyQuote() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 1140             // 19:00 — off shift, visiting the tavern
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 5},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, johnID: {}}},
		},
	}
	// John's targeted skillet quote at Ezekiel — the mis-pitched buy-back offer.
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: johnID,
			Reason: sim.SceneQuoteTargetedWarrantReason{
				QuoteID: 1, SellerID: johnID,
				Lines:  []sim.QuoteLine{{ItemKind: "skillet", Qty: 1}},
				Amount: 2,
			},
			SourceEventID: 1,
		},
	}
	return snap, ezekielID, warrants
}

// buyerOfferedQuoteTakeNamesTerms is the LLM-172 buyer side: John Ellis posts a
// targeted STEW quote at Ezekiel Crane — a good Ezekiel does NOT make and isn't
// at cap on — so the actionable take renders (unlike the maker buy-back above,
// which withholds it). Ezekiel is carrying 20 nails, the live trap: the prior
// take read "call pay_with_item with quote_id 1 and the same item, qty, and
// amount", and a buyer holding other goods bound "the same item" to a nail,
// dead-ended on the term-mismatch reject, and fell back to a bare pay that
// leaked coins for an undelivered stew with the quote still open. The golden
// pins that the take now names the concrete item/qty/amount ("item \"stew\",
// qty 1, and amount 4") so there is nothing to misbind. This is the ONLY golden
// exercising the single-line coin-quote actionable take.
func buyerOfferedQuoteTakeNamesTerms() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 1140             // 19:00 — off shift, visiting the tavern
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             25,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"nail": 20},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, johnID: {}}},
		},
	}
	// John's targeted stew quote at Ezekiel — a good he buys, not makes, so the
	// actionable take renders.
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: johnID,
			Reason: sim.SceneQuoteTargetedWarrantReason{
				QuoteID: 1, SellerID: johnID,
				Lines:  []sim.QuoteLine{{ItemKind: "stew", Qty: 1}},
				Amount: 4,
			},
			SourceEventID: 1,
		},
	}
	return snap, ezekielID, warrants
}

// buyerOfferedBundleQuoteCounterPath is the LLM-561 buyer side: a targeted
// MULTI-LINE bundle quote (iron, salt, 2 breeches — 50 coins for the lot)
// posted at a buyer who already holds 10 iron — the live 2026-07-28 shape
// (factor Daniel Holcomb's five-good bundle at Josiah Thorne, who wanted four
// of the five). The bundle take-instruction previously offered exactly one
// move — take all of it at the asked price — and a buyer wanting a subset was
// at a dead end; the golden pins that the take now also names the counter
// path: a free-form pay_with_item, no quote_id, one kind of good per offer,
// which the seller answers via accept/counter. Bundle twin of
// buyer_offered_quote_take_names_terms (the single-line arm's escape hatch).
func buyerOfferedBundleQuoteCounterPath() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		forge     = sim.StructureID("blacksmith")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 1140             // 19:00 — off shift, visiting the tavern
	published := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             79,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"iron": 10},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "skillet", Source: sim.RestockSourceProduce, Max: 5},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, johnID: {}}},
		},
	}
	// John's targeted bundle quote at Ezekiel. Iron is a good Ezekiel neither
	// produces nor caps, so the redundancy steer stays out of the way (one
	// genuinely wanted line is enough — buyQuoteRedundancyReason) and the
	// normal bundle take renders, escape clause included.
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: johnID,
			Reason: sim.SceneQuoteTargetedWarrantReason{
				QuoteID: 9, SellerID: johnID,
				Lines: []sim.QuoteLine{
					{ItemKind: "iron", Qty: 1},
					{ItemKind: "salt", Qty: 1},
					{ItemKind: "woolens", Qty: 2},
				},
				Amount: 50,
			},
			SourceEventID: 1,
		},
	}
	return snap, ezekielID, warrants
}

// peersHoldingSameFood is the LLM-138 degenerate-buy scene: two hungry NPCs
// stand together, each already carrying the same food (stew). The live
// hud-6a887a… case had each told ONLY to BUY the other's blueberries — the
// degenerate cue that drove the hollow "I can offer thee blueberries" beats
// backed by no transaction. The golden pins that the satiation section shows
// the subject its OWN stock to eat but carries NO "offer to buy it from them"
// peer line, because buying a copy of food already in hand is pointless (the
// gatherCoPresentPeerOffers gate suppresses it).
func peersHoldingSameFood() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		lewisID   = sim.ActorID("lewis")
		commons   = sim.StructureID("commons")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		Inventory:         map[sim.ItemKind]int{"stew": 3},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		Inventory:         map[sim.ItemKind]int{"stew": 1},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, lewisID: {}}},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, ezekielID, nil
}

// hungryBuyerWithWholesalerPeer is the LLM-289 situation from live hud-843da92a…:
// a hungry buyer with coins huddles with a wholesaler-farmer carrying food. The
// dispatch-side wholesale gate (LLM-223/252) keys on the SELLER's work anchor
// wherever the seller stands, so the peer's goods are a guaranteed pay_with_item
// rejection for a non-distributor — the co-present peer cue must not advertise
// them. The buyer holds coin (means-to-pay) and none of the peer's food
// (degenerate-buy), so the wholesale gate is the only thing suppressing the line.
// No clock-bound content → byte-stable.
func hungryBuyerWithWholesalerPeer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		mosesID   = sim.ActorID("moses")
		commons   = sim.StructureID("commons")
		farm      = sim.StructureID("james_farm")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             22,
		Needs:             map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold},
		Acquaintances:     map[string]sim.Acquaintance{"Moses James": {}},
	}
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		WorkStructureID:   farm,
		Inventory:         map[sim.ItemKind]int{"stew": 3},
		Acquaintances:     map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence, mosesID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// The live James Farm carries market_stall+business+farm+wholesaler;
			// only the wholesaler tag gates selling.
			sim.VillageObjectID(farm): {ID: sim.VillageObjectID(farm), OwnerActorID: mosesID, Tags: []string{sim.TagFarm, sim.TagWholesaler}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{silenceID: {}, mosesID: {}}},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, silenceID, nil
}

// coinlessWorkerAmongPeers is the LLM-153 situation: two laborers stand together in
// the commons and the one we render (Goodwife Bishop, a newcomer) has an empty purse.
// Live, 0-coin workers tried to BUY services from each other — the pay path rejected
// every attempt (engine/sim/pay_commands.go), but the model kept trying because the
// prompt showed "Coins in your purse: 0" without saying that meant it could not pay.
// The golden pins the consequence line the empty-purse case now renders in "## You".
// No needs, no clock-bound content → byte-stable.
func coinlessWorkerAmongPeers() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		bishopID = sim.ActorID("bishop")
		walkerID = sim.ActorID("walker")
		commons  = sim.StructureID("commons")
		huddle   = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	bishop := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Goodwife Bishop",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	walker := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             6,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Goodwife Bishop": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{bishopID: bishop, walkerID: walker},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{bishopID: {}, walkerID: {}}},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, bishopID, nil
}

// brokeEmployerCannotPayLaborOffer is the LLM-158 situation, reduced to its
// load-bearing parts: Lewis Walker (a worker) has solicited the subject (Ezekiel
// Crane) for a 5-coin job, but Ezekiel's purse is empty. accept_work's funds
// gate (buyerCanAfford, labor_commands.go) would flip the offer to
// failed_unavailable, so the cue must steer Ezekiel to decline_work WITH a spoken
// reason rather than present accept_work — otherwise he "accepts" verbally and
// the deal dies in silence (the live Lewis<->Ezekiel blacksmith dead-air). No
// needs, no clock-bound content → byte-stable.
func brokeEmployerCannotPayLaborOffer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		lewisID   = sim.ActorID("lewis")
		commons   = sim.StructureID("commons")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			commons: plainStructure(commons, "Village Commons"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, lewisID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:          1,
				WorkerID:    lewisID,
				EmployerID:  ezekielID,
				Reward:      5,
				DurationMin: 60,
				State:       sim.LaborStatePending,
				HuddleID:    huddle,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, ezekielID, nil
}

// workerEnRouteToWorkplace is the LLM-229 relocation self-state: Patience Walker
// (a worker) accepted a job for Josiah Thorne struck away from his General Store
// and is now on her way to his workplace — an EnRoute LaborOffer with her as the
// worker. She is not yet laboring (no Working offer, no laboring mirror), so the
// self-state must send her to the post and get her to work; and because she is
// already committed, the solicit affordance and the businesses directory must
// stay suppressed even though she is a worker. Solo, no clock-bound content →
// byte-stable.
func workerEnRouteToWorkplace() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		josiahID   = sim.ActorID("josiah")
		store      = sim.StructureID("store")
	)
	published := time.Date(2026, 7, 3, 11, 0, 0, 0, time.UTC)
	patience := &sim.ActorSnapshot{
		Kind:           sim.KindNPCShared,
		DisplayName:    "Patience Walker",
		Role:           "laborer",
		State:          sim.StateIdle,
		Coins:          0,
		Needs:          map[sim.NeedKey]int{},
		AttributeSlugs: []string{sim.AttrWorker},
		Acquaintances:  map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: store,
		WorkStructureID:   store,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:          1,
				WorkerID:    patienceID,
				EmployerID:  josiahID,
				Reward:      1,
				RewardItems: []sim.ItemKindQty{{Kind: "cheese", Qty: 1}},
				DurationMin: 120,
				State:       sim.LaborStateEnRoute,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, patienceID, nil
}

// inKindLaborOfferSnapshot builds the shared LLM-225 shape: Anne Walker has
// solicited Hannah Boggs for a job paid 1 porridge + 2 coins. employerHoldsGoods
// controls whether Hannah's inventory carries the porridge — true renders the
// both-legs decision line + normal footer (labor_offer_in_kind_reward), false
// the missing-goods decline steer (employer_missing_reward_items_steer).
func inKindLaborOfferSnapshot(employerHoldsGoods bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		anneID   = sim.ActorID("anne")
		inn      = sim.StructureID("inn")
		huddle   = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		CurrentHuddleID:   huddle,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Anne Walker": {}},
	}
	if employerHoldsGoods {
		hannah.Inventory = map[sim.ItemKind]int{"porridge": 3}
	}
	anne := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		CurrentHuddleID:   huddle,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Hannah Boggs": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, anneID: anne},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "The Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{hannahID: {}, anneID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:          1,
				WorkerID:    anneID,
				EmployerID:  hannahID,
				Reward:      2,
				RewardItems: []sim.ItemKindQty{{Kind: "porridge", Qty: 1}},
				DurationMin: 120,
				State:       sim.LaborStatePending,
				HuddleID:    huddle,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, hannahID, nil
}

// apothecaryHireSnapshot builds the LLM-346 shape from the live trace: Lewis
// Walker (a `worker`) stands in the PW Apothecary with Prudence Ward, its keeper.
// `offered` controls whether Prudence has already asked him to lend a hand — true
// mints an employer-initiated Pending offer (subject Lewis sees the decision
// section), false leaves the room empty of offers (subject Prudence sees the
// offer_work affordance naming him).
//
// The two together pin the whole employer-initiated path: the cue that lets a
// keeper ask, and the section that lets the worker answer. Before this ticket
// Prudence could only say the words, and Lewis's prompt advertised no work tool at
// all — he stood at her door for 45 minutes holding a promise the world had no way
// to keep.
func apothecaryHireSnapshot(offered bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID    = sim.ActorID("lewis")
		prudenceID = sim.ActorID("prudence")
		apothecary = sim.StructureID("apothecary")
		huddle     = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 10, 11, 38, 0, 0, time.UTC)
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "apothecary",
		State:             sim.StateIdle,
		InsideStructureID: apothecary,
		WorkStructureID:   apothecary,
		CurrentHuddleID:   huddle,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	// Lewis carries 26 coins, as he did live. That is above the seek-work comfort
	// ceiling, so his solicit affordance is suppressed — which is precisely why he
	// must be able to ANSWER an offer without one (the LLM-347 comfort gate this
	// ticket subsumes). The answer tools ride the standing offer, not the hustle.
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: apothecary,
		CurrentHuddleID:   huddle,
		Coins:             26,
		Needs:             map[sim.NeedKey]int{},
		AttributeSlugs:    []string{sim.AttrWorker},
		Acquaintances:     map[string]sim.Acquaintance{"Prudence Ward": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis, prudenceID: prudence},
		Structures: map[sim.StructureID]*sim.Structure{
			apothecary: plainStructure(apothecary, "PW Apothecary"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{lewisID: {}, prudenceID: {}}},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	if !offered {
		return snap, prudenceID, nil
	}
	snap.LaborLedger = map[sim.LaborID]*sim.LaborOffer{
		1: {
			ID:          1,
			WorkerID:    lewisID,
			EmployerID:  prudenceID,
			InitiatedBy: prudenceID,
			Reward:      4,
			DurationMin: 240,
			State:       sim.LaborStatePending,
			HuddleID:    huddle,
		},
	}
	return snap, lewisID, nil
}

func workerOfferedWorkByKeeper() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return apothecaryHireSnapshot(true)
}

func keeperCanOfferWorkToCoPresentWorker() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return apothecaryHireSnapshot(false)
}

func laborOfferInKindReward() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return inKindLaborOfferSnapshot(true)
}

func employerMissingRewardItemsSteer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return inKindLaborOfferSnapshot(false)
}

// employerWithWorkerOnJob is the LLM-202 employer-side cue fixture: John Ellis the
// tavernkeeper stands with Silence Walker, who is mid-contract for him (a Working
// labor offer with ~90 minutes left). The subject is the EMPLOYER, so the new
// "## Workers currently working for you" cue (renderWorkersForMe) renders — the
// mirror of the worker's Laboring self-state. WorkingUntil is anchored to the
// snapshot instant + 90m so the "about N left" line is byte-stable (RenderedAt =
// PublishedAt). The reward (2) renders in the owed clause.
func employerWithWorkerOnJob() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID    = sim.ActorID("john")
		silenceID = sim.ActorID("silence")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 30, 20, 30, 0, 0, time.UTC)
	workingUntil := published.Add(90 * time.Minute)
	acceptedAt := published.Add(-30 * time.Minute)
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "laborer",
		State:             sim.StateLaboring,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{johnID: john, silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{johnID: {}, silenceID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:           1,
				WorkerID:     silenceID,
				EmployerID:   johnID,
				Reward:       2,
				DurationMin:  120,
				State:        sim.LaborStateWorking,
				HuddleID:     huddle,
				AcceptedAt:   &acceptedAt,
				WorkingUntil: &workingUntil,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, johnID, nil
}

// laborSettledBeatSnapshot is the shared LLM-498 fixture: the instant AFTER the
// completion sweep settled Abraham Warren's four-hour job for Elizabeth Ellis
// mid-shift — 12 coins and 1 milk transferred in full, the worker freed to
// idle, the offer terminal Completed in the ledger (not yet reaped) — with the
// two still huddled at Ellis Farm, exactly the live 2026-07-19 trace moment
// where Abraham asked to "settle up" for a wage he had already been paid. Each
// subject carries its own side's LaborSettledWarrantReason with the narration
// pre-rendered through the REAL builders (LaborSettledWorkerNarration /
// LaborSettledEmployerNarration), so the goldens stay in lockstep with the
// production phrasing. The reward deliberately carries both legs (coins + an
// in-kind milk) to pin formatPayment's two-part phrase in the beat.
func laborSettledBeatSnapshot(subjectIsWorker bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		elizabethID = sim.ActorID("elizabeth")
		abrahamID   = sim.ActorID("abraham")
		farm        = sim.StructureID("farm")
		huddle      = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 19, 15, 35, 0, 0, time.UTC)
	acceptedAt := published.Add(-4 * time.Hour)
	workingUntil := published
	resolvedAt := published
	rewardItems := []sim.ItemKindQty{{Kind: "milk", Qty: 1}}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: farm,
		CurrentHuddleID:   huddle,
		Coins:             38,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Abraham Warren": {}},
	}
	abraham := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Abraham Warren",
		Role:              "laborer",
		State:             sim.StateIdle, // settleCompletedLabor freed him before the event emitted
		InsideStructureID: farm,
		CurrentHuddleID:   huddle,
		Coins:             12,
		Inventory:         map[sim.ItemKind]int{"milk": 1},
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Elizabeth Ellis": {}},
	}
	kinds := foodDrinkCatalog()
	kinds["milk"] = &sim.ItemKindDef{Name: "milk", DisplayLabel: "milk", Category: sim.ItemCategoryDrink,
		Capabilities: []string{"portable"}, Satisfies: []sim.ItemSatisfaction{{Attribute: "thirst", Immediate: 4}}}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{elizabethID: elizabeth, abrahamID: abraham},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "Ellis Farm"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{elizabethID: {}, abrahamID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:           1,
				WorkerID:     abrahamID,
				EmployerID:   elizabethID,
				Reward:       12,
				RewardItems:  rewardItems,
				DurationMin:  240,
				State:        sim.LaborStateCompleted,
				HuddleID:     huddle,
				AcceptedAt:   &acceptedAt,
				WorkingUntil: &workingUntil,
				ResolvedAt:   &resolvedAt,
			},
		},
		ItemKinds: kinds,
	}
	if subjectIsWorker {
		warrants := []sim.WarrantMeta{{
			TriggerActorID: elizabethID,
			Reason: sim.LaborSettledWarrantReason{
				LaborID:       1,
				Counterparty:  elizabethID,
				NarrationText: sim.LaborSettledWorkerNarration("Elizabeth Ellis", 12, rewardItems),
			},
			SourceEventID: 1,
		}}
		return snap, abrahamID, warrants
	}
	warrants := []sim.WarrantMeta{{
		TriggerActorID: abrahamID,
		Reason: sim.LaborSettledWarrantReason{
			LaborID:       1,
			Counterparty:  abrahamID,
			NarrationText: sim.LaborSettledEmployerNarration("Abraham Warren", 12, rewardItems),
		},
		SourceEventID: 1,
	}}
	return snap, elizabethID, warrants
}

func workerLaborSettledBeat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return laborSettledBeatSnapshot(true)
}

func employerLaborSettledBeat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return laborSettledBeatSnapshot(false)
}

// workerLaborSettledCloseoutBothSignals is the shop-closed variant: the same
// settle, but the job ran up to closing time so the LLM-190 close-out Spoke
// ALSO fired — the worker's tick carries the keeper's spoken line (an
// NPC-speech warrant, as the speech reactor would stamp it) alongside its own
// labor_settled warrant. The excerpt mirrors laborCloseoutLine's phrasing with
// this job's formatPayment output; it is fixture-authored (the sim keeps that
// composer unexported), so a wording change there shows up as a fixture edit,
// not a silent drift.
func workerLaborSettledCloseoutBothSignals() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, abrahamID, warrants := laborSettledBeatSnapshot(true)
	warrants = append(warrants, sim.WarrantMeta{
		TriggerActorID: "elizabeth",
		Reason: sim.NPCSpeechWarrantReason{
			SpeechID: 2,
			Speaker:  "elizabeth",
			Excerpt:  "That's the shop shut for the day — your work's done, and well done. Here's your 1 milk and 12 coins, with my thanks.",
		},
		SourceEventID: 2,
	})
	return snap, abrahamID, warrants
}

// laboringWorkerAddressedByEmployer is the LLM-230 worker-side fixture: Silence
// Walker is mid-job for John Ellis in his tavern (a Working LaborOffer, WorkerID
// == subject) and John speaks to her (an NPC-speech warrant). The golden pins the
// standing "You are working a job for John Ellis … Stay with it until it's done"
// self-state line that anchors her reply — the cue that lets her answer "can't
// stop just now" instead of going silent or abandoning the job. The reply-cadence
// (reactor) and the speak-only tool surface (handlers.gateTools) are covered by
// their own unit tests; the render is unchanged, so this is a regression pin on
// the anchor's presence for the addressed-while-working situation.
func laboringWorkerAddressedByEmployer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID    = sim.ActorID("john")
		silenceID = sim.ActorID("silence")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 30, 20, 30, 0, 0, time.UTC)
	workingUntil := published.Add(90 * time.Minute)
	acceptedAt := published.Add(-30 * time.Minute)
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "laborer",
		State:             sim.StateLaboring,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{johnID: john, silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {
				ID:      huddle,
				Members: map[sim.ActorID]struct{}{johnID: {}, silenceID: {}},
				RecentUtterances: []sim.Utterance{
					{SpeakerID: johnID, SpeakerName: "John Ellis", Text: "Care to tend the fire while you're at it?", At: published.Add(-20 * time.Second)},
				},
			},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:           1,
				WorkerID:     silenceID,
				EmployerID:   johnID,
				Reward:       2,
				DurationMin:  120,
				State:        sim.LaborStateWorking,
				HuddleID:     huddle,
				AcceptedAt:   &acceptedAt,
				WorkingUntil: &workingUntil,
			},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	// John speaks to her mid-job — the addressed-while-working moment (LLM-230).
	warrants := []sim.WarrantMeta{{
		TriggerActorID: johnID,
		Reason:         sim.NPCSpeechWarrantReason{SpeechID: 1, Speaker: johnID, Excerpt: "Care to tend the fire while you're at it?"},
		SourceEventID:  1,
		HuddleID:       huddle,
		OccurredAt:     published,
	}}
	return snap, silenceID, warrants
}

// longHeardUtterance is a 372-rune line — the actual utterance Silence Walker
// spoke in the Inn on 2026-07-14 that Lewis perceived clipped at "…finding home
// in small" (LLM-396). Well past the old 220-rune warrant cut, comfortably under
// both MaxSpeakTextChars (1000 runes) and the renderer's MaxBytesPerWarrant
// (600 bytes), so a correct engine shows it WHOLE.
const longHeardUtterance = "You didn't put me on the spot, Lewis — not at all. You're right that some " +
	"memories are best held close, but the ones I've been sharing... they feel like the kind worth " +
	"telling, because they're about finding home in small, steady things. And you're right — the " +
	"making and mending, the tending, they do hold a life together. I think that's what I'm learning, " +
	"day by day."

// listenerHearsLongUtterance is the LLM-396 fixture: the laboring-worker scene
// with the employer's line replaced by a real 372-rune utterance, carried BOTH on
// the speech warrant (the "## Since your last turn" beat) and in the huddle's
// RecentUtterances ring (the "## Recent conversation here" beat).
//
// It pins two things at once:
//
//   - The listener perceives the COMPLETE utterance — not a bare mid-word prefix.
//     The clipped prefix is what drove the live 13-minute Inn loop: an unfinished
//     sentence reads as an invitation to ask the speaker to finish it.
//   - The line still renders exactly ONCE. Carrying the full text on the warrant
//     while the ring/SalientFact copies stay in the 220-rune form is precisely the
//     case that would break currentHeardExcerpts' exact-match dedup and resurrect
//     the ZBBS-WORK-374 "same line twice" duplication — which reinforces the very
//     line we don't want repeated. The golden fails loudly if it double-renders.
func listenerHearsLongUtterance() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, subject, warrants := laboringWorkerAddressedByEmployer()
	for _, h := range snap.Huddles {
		for i := range h.RecentUtterances {
			h.RecentUtterances[i].Text = longHeardUtterance
		}
	}
	for i := range warrants {
		if r, ok := warrants[i].Reason.(sim.NPCSpeechWarrantReason); ok {
			r.Excerpt = longHeardUtterance
			warrants[i].Reason = r
		}
	}
	return snap, subject, warrants
}

// returningHelperLaborOfferSnapshot builds the LLM-228 shape: Anne Walker, who
// completed a paid job for Hannah Boggs a few hours ago (an Active
// ObservedHelpedByWorker memory on Hannah), has solicited Hannah again. The
// decision section recalls the past help. employerProduces controls whether
// Hannah makes goods herself (a makeable produce entry + recipe) — true renders
// the "…and you got more done for it" clause (employer_recalls_returning_helper),
// false the bare social beat (employer_recalls_returning_helper_nonproducer). The
// offer is coins-only and affordable so the normal accept/decline footer renders
// alongside the recall.
func returningHelperLaborOfferSnapshot(employerProduces bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		anneID   = sim.ActorID("anne")
		inn      = sim.StructureID("inn")
		huddle   = sim.HuddleID("h1")
	)
	published := time.Date(2026, 7, 3, 11, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		CurrentHuddleID:   huddle,
		Coins:             50,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Anne Walker": {}},
		// Hannah remembers Anne finishing a paid job for her five hours ago — well
		// inside the 36h HelpedByWorkerMemoryTTL, so the recall reads Active.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{PeerID: anneID, Condition: sim.ObservedHelpedByWorker}: published.Add(-5 * time.Hour),
		}),
	}
	var recipes map[sim.ItemKind]*sim.ItemRecipe
	if employerProduces {
		// A single makeable produce entry makes Hannah a producer for the copy
		// split (subjectProducesGoods) without tripping the multi-output forge cue.
		hannah.RestockPolicy = &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "porridge", Source: sim.RestockSourceProduce, Max: 5},
		}}
		recipes = map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 1, RateQty: 1, RatePerHours: 2, WholesalePrice: 1, RetailPrice: 2},
		}
	}
	anne := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		CurrentHuddleID:   huddle,
		Coins:             1,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Hannah Boggs": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, anneID: anne},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "The Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{hannahID: {}, anneID: {}}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:          1,
				WorkerID:    anneID,
				EmployerID:  hannahID,
				Reward:      3,
				DurationMin: 60,
				State:       sim.LaborStatePending,
				HuddleID:    huddle,
			},
		},
		Recipes:   recipes,
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, hannahID, nil
}

func employerRecallsReturningHelper() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return returningHelperLaborOfferSnapshot(true)
}

func employerRecallsReturningHelperNonProducer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return returningHelperLaborOfferSnapshot(false)
}

// workerAmongHousehold is the LLM-157 situation: two worker-tagged Walker siblings
// (Lewis, the rendered subject, + Anne) share a home and stand together in it, both
// jobless. LLM-145 already hides the solicit_work tool when only kin are present,
// but the seek-work backstop warrant still nudged the model to ask the housemate for
// work as freeform speech. The golden pins the "## Around you" annotation that now
// names Anne as the subject's housemate. Small non-zero purses keep the empty-purse
// line out so the golden centers on the household annotation. No clock-bound content
// → byte-stable.
func workerAmongHousehold() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID = sim.ActorID("lewis")
		anneID  = sim.ActorID("anne")
		home    = sim.StructureID("walker-residence")
		huddle  = sim.HuddleID("h1")
	)
	published := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: home,
		HomeStructureID:   home,
		CurrentHuddleID:   huddle,
		AttributeSlugs:    []string{sim.AttrWorker},
		Coins:             2,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Anne Walker": {}},
	}
	anne := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: home,
		HomeStructureID:   home,
		CurrentHuddleID:   huddle,
		AttributeSlugs:    []string{sim.AttrWorker},
		Coins:             2,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:    published,
		NeedThresholds: sim.NeedThresholds{},
		Actors:         map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis, anneID: anne},
		Structures: map[sim.StructureID]*sim.Structure{
			home: plainStructure(home, "Walker Residence"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{lewisID: {}, anneID: {}}},
		},
	}
	return snap, lewisID, nil
}

// keeperOffersRoomToCoinlessGuest is the LLM-136 host-side scene. John Ellis, the
// tavernkeeper, shares his tavern (one free private room at a live nightly rate)
// with Ezekiel Crane — a homeless smith with no home, no lodging grant, and 0
// coins, carrying only his own wares. The "## A room to let" cue fires; the golden
// pins the new goods-for-room clause, so a coinless guest is offered the room for
// goods (offer_trade → accept_pay) instead of being dead-ended on coins he doesn't
// have. This is the keeper side of the live livelock from LLM-136.
func keeperOffersRoomToCoinlessGuest() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		johnID    = sim.ActorID("john")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 360, 1320 // 06:00–22:00, on shift in the evening
	now := 1140             // 19:00 — a guest seeking a bed for the night
	published := time.Date(2026, 6, 25, 19, 0, 0, 0, time.UTC)
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	// No HomeStructureID and no RoomAccess → a structural lodging-seeker. 0 coins
	// with wares on hand is the whole point: the goods path is his only way in.
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"skillet": 4, "nail": 27},
	}
	snap := &sim.Snapshot{
		PublishedAt:              published,
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		LodgingDefaultWeeklyRate: 28, // → 4 coins/night
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{johnID: john, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: {ID: tavern, DisplayName: "Tavern", Rooms: []*sim.Room{
				{ID: 1, StructureID: tavern, Kind: sim.RoomKindPrivate, Name: "bedroom_1"},
			}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{johnID: {}, ezekielID: {}}},
		},
	}
	return snap, johnID, nil
}

// homedGuestLodgingQuoteSuppressed is the LLM-208 buyer side: John Ellis posts a
// targeted nights_stay (room) quote at Prudence Ward, but Prudence HAS a home
// (Ward Residence). A homed guest can't take a room — the buyer-side
// pay_with_item guard rejects it (LLM-182) — so surfacing the offer only pulls
// her into a doomed nightly negotiation (the live John↔Prudence tavern loop).
// The golden pins that the room-offer take is SUPPRESSED for her:
// filterHomedLodgingQuoteWarrants drops the lodging quote warrant at build, so
// the assembled prompt carries no "offers you … nights_stay" / pay_with_item
// take line. Contrast keeper_offers_room_to_coinless_guest (a HOMELESS seeker,
// who correctly DOES get the offer). TestHomedGuestLodgingQuoteSuppressed pins
// that clearing her home restores the take — proving the home is the sole cause.
func homedGuestLodgingQuoteSuppressed() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		prudenceID = sim.ActorID("prudence")
		johnID     = sim.ActorID("john")
		tavern     = sim.StructureID("tavern")
		wardHome   = sim.StructureID("ward_residence")
		huddle     = sim.HuddleID("h1")
	)
	now := 1140 // 19:00 — visiting the tavern in the evening
	published := time.Date(2026, 6, 30, 19, 0, 0, 0, time.UTC)
	// Prudence has a home (Ward Residence) and no lodging grant — the homed guest.
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		HomeStructureID:   wardHome,
		CurrentHuddleID:   huddle,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"John Ellis": {}},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddle,
		Coins:             267,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:              published,
		LocalMinuteOfDay:         &now,
		NeedThresholds:           sim.NeedThresholds{},
		LodgingDefaultWeeklyRate: 28, // → 4 coins/night
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"nights_stay": {Name: "nights_stay", Capabilities: []string{"service", "lodging"}},
		},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{prudenceID: prudence, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern:   {ID: tavern, DisplayName: "Tavern", Rooms: []*sim.Room{{ID: 1, StructureID: tavern, Kind: sim.RoomKindPrivate, Name: "bedroom_1"}}},
			wardHome: plainStructure(wardHome, "Ward Residence"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{prudenceID: {}, johnID: {}}},
		},
	}
	// John's targeted nights_stay quote at Prudence — the room offer she can't take.
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: johnID,
			Reason: sim.SceneQuoteTargetedWarrantReason{
				QuoteID: 1, SellerID: johnID,
				Lines:  []sim.QuoteLine{{ItemKind: "nights_stay", Qty: 1}},
				Amount: 4,
			},
			SourceEventID: 1,
		},
	}
	return snap, prudenceID, warrants
}

// keeperAloneAtPostOnShift reproduces the LLM-106 live shape: Josiah Thorne, a
// stateful keeper, has just arrived at his own General Store during working hours
// with no one else present. He is not tired or hungry — the only stimulus is the
// arrival itself.
func keeperAloneAtPostOnShift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		store    = sim.StructureID("general_store")
		home     = sim.StructureID("thorne_residence")
	)
	start, end := 360, 1260 // working hours 06:00–21:00 (closes at 9 in the evening)
	now := 540              // 09:00 — morning, on shift
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             44,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			home:  plainStructure(home, "Thorne Residence"),
		},
	}
	// Self-arrival at the store → "## Since your last turn: You arrived at General Store."
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: josiahID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: store},
			SourceEventID:  1,
		},
	}
	return snap, josiahID, warrants
}

// constableWalkingRoundsAtStore is the LLM-514 rounds-cue scenario: the constable
// Gideon Marsh mid-rounds, having stepped inside the General Store on his circuit
// of the businesses. The route projection (RouteLabel == constable,
// RouteStopObjectID == the store) is what the republish stamps from w.ActiveRoutes
// once he has arrived at a stop; the golden pins the diegetic "you are walking your
// rounds … you stand before the General Store" self-state cue and that the routine
// go-to-post duty steer yields to it.
func constableWalkingRoundsAtStore() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		gideonID = sim.ActorID("gideon")
		store    = sim.StructureID("general_store")
		smithy   = sim.StructureID("blacksmith")
		post     = sim.StructureID("meeting_house")
		home     = sim.StructureID("marsh_residence")
	)
	start, end := 0, 1440 // on the watch all day
	now := 780            // 13:00 — mid-afternoon, on shift
	gideon := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Gideon Marsh",
		Role:              "constable",
		State:             sim.StateIdle,
		WorkStructureID:   post,
		InsideStructureID: store, // stepped inside the store on his rounds
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Needs:             map[sim.NeedKey]int{},
		// LLM-514: mid-rounds, arrived at the store — the read-path projection the
		// republish stamps from w.ActiveRoutes (RouteStopObjectID set only once he
		// has arrived at the stop).
		RouteLabel:        sim.AttrConstable,
		RouteStopObjectID: sim.VillageObjectID(store),
		// LLM-524: five businesses still ahead on the circuit, so the cue carries the
		// round forward instead of leaving a quiet stop reading as a dead end.
		RouteStopsAhead: 5,
		// LLM-530: and the next one is NAMED — move_to is how he says "I am finished
		// with this place", so the round must give him somewhere to say it about.
		RouteNextStopObjectID: sim.VillageObjectID(smithy),
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{gideonID: gideon},
		Structures: map[sim.StructureID]*sim.Structure{
			store:  plainStructure(store, "General Store"),
			smithy: plainStructure(smithy, "Blacksmith"),
			post:   plainStructure(post, "Meeting House"),
			home:   plainStructure(home, "Marsh Residence"),
		},
	}
	return snap, gideonID, nil
}

// constableAtLastRoundsStop is the LLM-524 boundary twin of
// constableWalkingRoundsAtStore: the constable standing at the FINAL stop of his
// circuit (RouteStopsAhead == 0). The golden pins that the "more places on your
// round still lie ahead" continuation line is OMITTED here — there is nothing left
// to name, and inventing a phantom remainder would push him past a finished round.
func constableAtLastRoundsStop() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		gideonID = sim.ActorID("gideon")
		store    = sim.StructureID("general_store")
		post     = sim.StructureID("meeting_house")
		home     = sim.StructureID("marsh_residence")
	)
	start, end := 0, 1440
	now := 780
	gideon := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Gideon Marsh",
		Role:              "constable",
		State:             sim.StateIdle,
		WorkStructureID:   post,
		InsideStructureID: store,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Needs:             map[sim.NeedKey]int{},
		RouteLabel:        sim.AttrConstable,
		RouteStopObjectID: sim.VillageObjectID(store),
		RouteStopsAhead:   0, // last stop — the continuation line must not appear
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{gideonID: gideon},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			post:  plainStructure(post, "Meeting House"),
			home:  plainStructure(home, "Marsh Residence"),
		},
	}
	return snap, gideonID, nil
}

// constableRoundOwedOffPost is the LLM-540 scenario: the constable stepped off his
// circuit of his own accord, finished whatever pulled him away, and is now standing
// idle at the Blacksmith with a round still owed. The wake that gets him this tick
// is his ordinary go-to-post duty warrant, which a part-walked round no longer
// suppresses — before LLM-540 it did, and with the duty steer also silent he had no
// recurring wake source at all: the cue naming what he still owed was never
// rendered and he stood still until the 30-minute idle backstop (live 2026-07-27,
// Gideon at the Blacksmith, 9m24s without a tick).
//
// The golden pins the pairing that makes the wake safe. The cue names what remains
// and where to go next, AND the go-to-post steer is absent — waking him is not the
// same as marching him back, which is the LLM-531 property that must not regress.
// The steer's absence is not incidental to this fixture: buildDutySteer yields for
// any constable RouteLabel, which is why the warrant can be restored without the
// cue coming back with it.
//
// Under LLM-548 this is the ORDINARY shape of a round rather than a paused one.
// There is no suspended state any more: the engine never dispatched him anywhere,
// so stepping off the circuit is not a departure from anything, and the cue reads
// the same here as it does on a doorstep — bar the "you stand before" clause, which
// needs a stop under his feet.
func constableRoundOwedOffPost() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		gideonID = sim.ActorID("gideon")
		smithy   = sim.StructureID("blacksmith")
		farm     = sim.StructureID("ellis_farm")
		post     = sim.StructureID("meeting_house")
		home     = sim.StructureID("marsh_residence")
	)
	start, end := 0, 1440 // on the watch all day
	now := 780            // 13:00 — on shift, so the to-work duty stands
	gideon := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Gideon Marsh",
		Role:              "constable",
		State:             sim.StateIdle,
		WorkStructureID:   post,
		InsideStructureID: smithy, // off post, where his own errand took him
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Needs:             map[sim.NeedKey]int{},
		// The republish projects the count and the next place with NO arrival
		// requirement (LLM-548) — a round is a standing obligation, and a man away
		// from every stop needs telling what he owes every bit as much as one on a
		// doorstep. RouteStopObjectID is deliberately EMPTY: he is standing in the
		// smithy, which is not a stop on this circuit, so nothing can say "you stand
		// before" anywhere.
		RouteLabel:            sim.AttrConstable,
		RouteNextStopObjectID: sim.VillageObjectID(farm),
		RouteStopsAhead:       6,
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{gideonID: gideon},
		Structures: map[sim.StructureID]*sim.Structure{
			smithy: plainStructure(smithy, "Blacksmith"),
			farm:   plainStructure(farm, "Ellis Farm"),
			post:   plainStructure(post, "Meeting House"),
			home:   plainStructure(home, "Marsh Residence"),
		},
	}
	// The restored wake (LLM-540). It carries no line of its own — Render filters
	// the shift-duty warrant text, leaving the DutySteer as its only voice, and that
	// steer yields to the round.
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: gideonID,
			Reason:         sim.ShiftDutyWarrantReason{ToWork: true, TargetStructureID: post},
		},
	}
	return snap, gideonID, warrants
}

// constableAtAVillagersDoor is the LLM-547 base fixture: the constable mid-round,
// standing outdoors at the Apothecary with Prudence Ward within earshot and
// acquainted. Deliberately the SAME shape as the live case the ticket was filed
// on — he called on Ward three times in one circuit and she was the one who
// noticed ("Three times is thrice too many").
//
// The contact ledger is left EMPTY here. Every LLM-547 scenario is this fixture
// plus a ledger, so each golden's diff against constable_at_a_villagers_door is
// exactly the line the tier under test renders (or, for the two silent tiers,
// nothing at all — which is the assertion).
//
// PublishedAt is set explicitly, unlike most fixtures: the contact tiers are
// windowed against it, so a zero clock would make every entry unimaginably old
// and silently collapse the whole feature to ContactTierNone.
func constableAtAVillagersDoor() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		gideonID   = sim.ActorID("gideon")
		prudenceID = sim.ActorID("prudence")
		apothecary = sim.StructureID("pw_apothecary")
		inn        = sim.StructureID("inn")
		post       = sim.StructureID("meeting_house")
		home       = sim.StructureID("marsh_residence")
	)
	start, end := 0, 1440 // on the watch all day
	now := 1140           // 19:00 — late in the circuit, as the live case was
	published := time.Date(2026, 7, 27, 19, 0, 0, 0, time.UTC)

	gideon := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Gideon Marsh",
		Role:             "constable",
		State:            sim.StateIdle,
		WorkStructureID:  post,
		HomeStructureID:  home,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Needs:            map[sim.NeedKey]int{},
		// Outdoors and arrived at the Apothecary stop, one short of finishing —
		// the shape of the live 21:19 read. The rounds cue names where he stands
		// from RouteStopObjectID; the location line stays a plain "outdoors"
		// because the loiter-pin name is derived from village-object geometry this
		// fixture has no reason to carry.
		RouteLabel:            sim.AttrConstable,
		RouteStopObjectID:     sim.VillageObjectID(apothecary),
		RouteNextStopObjectID: sim.VillageObjectID(inn),
		RouteStopsAhead:       1,
		// Ward is within earshot and known by name. Acquaintance matters for the
		// register invariant: the line must render her VERBATIM DisplayName, which
		// is also the token resolveAddressee matches on.
		ColocatedAudienceIDs: []sim.ActorID{prudenceID},
		Acquaintances:        map[string]sim.Acquaintance{"Prudence Ward": {}},
	}
	prudence := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Prudence Ward",
		Role:             "apothecary",
		State:            sim.StateIdle,
		WorkStructureID:  apothecary,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Needs:            map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			gideonID:   gideon,
			prudenceID: prudence,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			apothecary: plainStructure(apothecary, "PW Apothecary"),
			inn:        plainStructure(inn, "Inn"),
			post:       plainStructure(post, "Meeting House"),
			home:       plainStructure(home, "Marsh Residence"),
		},
		ContactBrakeWindow:   sim.DefaultContactBrakeWindow,
		ContactRecallHorizon: sim.DefaultContactRecallHorizon,
	}
	return snap, gideonID, nil
}

// withContacts stamps a contact trail for one ordered pair onto a fixture,
// expressed as ages BEFORE the snapshot clock so a scenario reads in the same
// units the tiers are defined in ("90 minutes ago", not an absolute timestamp).
//
// Writes only the subject→peer direction. Production writes both, but perception
// reads exactly one, and pinning the direction the read uses keeps a scenario
// from passing on the strength of the wrong row.
func withContacts(snap *sim.Snapshot, subjectID, peerID sim.ActorID, ages ...time.Duration) *sim.Snapshot {
	if snap.ContactLedger == nil {
		snap.ContactLedger = make(map[sim.ActorID]map[sim.ActorID]*sim.ContactRecord)
	}
	byPeer, ok := snap.ContactLedger[subjectID]
	if !ok {
		byPeer = make(map[sim.ActorID]*sim.ContactRecord)
		snap.ContactLedger[subjectID] = byPeer
	}
	at := make([]time.Time, 0, len(ages))
	for _, age := range ages {
		at = append(at, snap.PublishedAt.Add(-age))
	}
	byPeer[peerID] = &sim.ContactRecord{At: at}
	return snap
}

// constableWokenAtHisPostWithARoundOwed is the LLM-549 scenario, and it is the exact
// live shape the ticket was filed on: the beat is installed, he is on-shift standing
// INSIDE his post, and the only thing that will ever get him going is his own wake.
//
// Shift duty stamps nothing for an actor already at work and the idle backstop only
// covers actors outdoors, so before LLM-549 this prompt was never built at all — he
// stood in the Meeting House for twelve minutes with eight doors unwalked and
// warrant_count 0.
//
// What the golden pins is that the wake arrives CARRYING ITS OWN ARGUMENT and nothing
// else. The rounds cue names what he owes and where to go next; the wake's warrant is
// filtered out of "Since your last turn" (nonStandingCueWarrants) so the prompt does
// not also print the raw warrant kind under the scene it duplicates — a stat beside
// its own prose. And no go-to-post steer appears, though he could hardly be closer to
// his post: buildDutySteer yields for any constable RouteLabel, which is what lets the
// wake exist without becoming a march.
func constableWokenAtHisPostWithARoundOwed() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		gideonID = sim.ActorID("gideon")
		store    = sim.StructureID("general_store")
		post     = sim.StructureID("meeting_house")
		home     = sim.StructureID("marsh_residence")
	)
	start, end := 0, 1440
	now := 540 // 09:00 — on shift, just after a fresh round came due
	gideon := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Gideon Marsh",
		Role:              "constable",
		State:             sim.StateIdle,
		WorkStructureID:   post,
		InsideStructureID: post, // AT his post — the hole no other producer covers
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Needs:             map[sim.NeedKey]int{},
		// A fresh circuit: nothing walked, so the full count and the first place he
		// owes are projected with no arrival requirement. RouteStopObjectID is empty
		// — the Meeting House is his post, not a stop on the round.
		RouteLabel:            sim.AttrConstable,
		RouteNextStopObjectID: sim.VillageObjectID(store),
		RouteStopsAhead:       8,
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{gideonID: gideon},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			post:  plainStructure(post, "Meeting House"),
			home:  plainStructure(home, "Marsh Residence"),
		},
	}
	warrants := []sim.WarrantMeta{
		{TriggerActorID: gideonID, Reason: sim.ConstableRoundsWarrantReason{}},
	}
	return snap, gideonID, warrants
}

// visitorArrivesAtKeepersWorkplace reproduces the LLM-284 host-role inversion:
// John Ellis (a tavern keeper) walked to the Blacksmith on an errand and arrives
// with the smith, Ezekiel Crane, already there. Ezekiel keeps the Blacksmith
// (his WorkStructureID is it); John does not. The golden pins that the "## What
// just happened" arrival line names the keeper in the possessive — "You arrived
// at Ezekiel Crane's Blacksmith." — so the model sees whose shop it walked into
// and hosts as a guest instead of greeting the keeper as if hosting his own
// forge. A regression that dropped the possessive (back to "the Blacksmith")
// shows in the diff.
func visitorArrivesAtKeepersWorkplace() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		johnID     = sim.ActorID("john")
		ezekielID  = sim.ActorID("ezekiel")
		blacksmith = sim.StructureID("blacksmith")
		tavern     = sim.StructureID("tavern")
		johnHome   = sim.StructureID("ellis_residence")
	)
	start, end := 360, 1260 // working hours 06:00–21:00
	now := 540              // 09:00 — morning, both on shift
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: blacksmith, // walked over; standing in the smithy
		HomeStructureID:   johnHome,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		// John knows Ezekiel (a fellow village keeper — in the incident he greeted
		// him by name), so the co-present line names him rather than "the blacksmith".
		Acquaintances: map[string]sim.Acquaintance{"Ezekiel Crane": {}},
		// Ezekiel is within earshot — the co-present keeper of the incident,
		// whom John greeted as if hosting his own forge.
		ColocatedAudienceIDs: []sim.ActorID{ezekielID},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   blacksmith,
		InsideStructureID: blacksmith,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{johnID: john, ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			blacksmith: plainStructure(blacksmith, "Blacksmith"),
			tavern:     plainStructure(tavern, "Tavern"),
			johnHome:   plainStructure(johnHome, "Ellis Residence"),
		},
	}
	// John just arrived at the Blacksmith → "## Since your last turn: You arrived at
	// Ezekiel Crane's Blacksmith." (the keeper possessive, LLM-284).
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: johnID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: blacksmith},
			SourceEventID:  1,
		},
	}
	return snap, johnID, warrants
}

// travelerSelfIdentityPreface is the LLM-370 self-preface fixture: a transient
// traveler (salem-visitor) perceiving its own turn. Elias Drum the peddler, in
// from Boston and weary, has just walked into the Tavern. The golden pins the
// engine-injected identity preface that opens the message ahead of "# Your turn",
// so the stateless shared VA speaks as this specific traveler. VisitorState set on
// the subject snapshot is the whole trigger; a persistent NPC (nil VisitorState)
// gets no preface (matrix guard: TestGoldensTravelerPrefaceIffSubjectIsTraveler).
func travelerSelfIdentityPreface() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID = sim.ActorID("vstr-elias")
		tavern  = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			// LLM-644: budget = purse, as at spawn.
			SpendBudget: 8,
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	// The traveler just walked in → "## Since your last turn: You arrived at the Tavern."
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerSelfIdentityPrefaceVocation is the LLM-566 fixture: a traveler whose
// archetype is IN the passer-through pool, so the identity preface carries the
// archetype's vocation sentence (sim.VisitorVocation) after the origin/manner
// clause. Master Whitcombe the circuit preacher, in from Beverly and wry — the
// live scene that motivated the ticket. He also carries a rumor payload, so the
// golden pins the full clause order: identity → origin/manner → vocation →
// rumor. The peddler fixtures (an archetype outside the pool) pin the
// no-vocation arm.
func travelerSelfIdentityPrefaceVocation() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		whitcombeID = sim.ActorID("vstr-whitcombe")
		tavern      = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	whitcombe := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Master Whitcombe the circuit preacher",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 8,
			Archetype:   "circuit preacher",
			Origin:      "Beverly",
			Disposition: "wry",
			Payload:     "Elizabeth Ellis turned out three times the meat for Josiah Thorne",
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{whitcombeID: whitcombe},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: whitcombeID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, whitcombeID, warrants
}

// travelerSelfIdentityPrefaceWithRoadWord is the LLM-371 fixture: the same traveler
// as travelerSelfIdentityPreface, but now carrying a grounded road word on
// VisitorState.Payload. The golden pins the extra preface clause ("Word reached
// you on the road that …") that renderTravelerPreface appends after the persona,
// so the stateless salem-visitor VA has one true thing to trade. The clause is
// dropped entirely on an empty Payload — that no-road-word case is the LLM-370
// travelerSelfIdentityPreface golden, so the two goldens together pin both arms.
func travelerSelfIdentityPrefaceWithRoadWord() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID = sim.ActorID("vstr-elias")
		tavern  = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 8,
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
			Payload:     "Ezekiel Crane turned out a plow for the Hale farm",
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerRumorSpentWithPresentCompany is the LLM-545 fixture: the same
// rumor-carrying traveler, back in a huddle with the one person he has already
// passed the word to (VisitorState.PayloadSharedWith carries her id — stamped by
// recordRoadWordShared when she answered him earlier). The golden pins the
// preface's SPENT reframing — "The word you picked up on the road — that … — you
// have already passed to Hannah Boggs, and heard what they had to say; that
// matter is spent between you." — in place of the fresh "Word reached you …"
// line, which is what stopped Brother Ashford reopening the Josiah Thorne rumour
// on Hannah half an hour after she set him straight.
func travelerRumorSpentWithPresentCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID  = sim.ActorID("vstr-elias")
		hannahID = sim.ActorID("hannah")
		tavern   = sim.StructureID("tavern")
		huddleID = sim.HuddleID("h1")
	)
	now := 540 // 09:00 — morning
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Hannah Boggs": {}},
		VisitorState: &sim.VisitorState{
			SpendBudget:       8,
			Archetype:         "peddler",
			Origin:            "Boston",
			Disposition:       "weary",
			Payload:           "Ezekiel Crane turned out a plow for the Hale farm",
			PayloadSharedWith: []sim.ActorID{hannahID},
		},
	}
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias, hannahID: hannah},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{eliasID: {}, hannahID: {}}},
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerRumorAlreadyToldOneOfCompany is the LLM-545 mixed-company fixture: the
// traveler stands with one peer who has had the word (Hannah Boggs) and one who
// has not (Goodman Stark). The golden pins the fresh "Word reached you …" line
// SURVIVING — there is a new listener — followed by the annotation naming who has
// already heard it ("You have already passed that word to Hannah Boggs."), so
// the model carries the news to the newcomer without re-opening it on Hannah.
func travelerRumorAlreadyToldOneOfCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID  = sim.ActorID("vstr-elias")
		hannahID = sim.ActorID("hannah")
		starkID  = sim.ActorID("stark")
		tavern   = sim.StructureID("tavern")
		huddleID = sim.HuddleID("h1")
	)
	now := 540
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Hannah Boggs": {}, "Goodman Stark": {}},
		VisitorState: &sim.VisitorState{
			SpendBudget:       8,
			Archetype:         "peddler",
			Origin:            "Boston",
			Disposition:       "weary",
			Payload:           "Ezekiel Crane turned out a plow for the Hale farm",
			PayloadSharedWith: []sim.ActorID{hannahID},
		},
	}
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Needs:             map[sim.NeedKey]int{},
	}
	stark := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Goodman Stark",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias, hannahID: hannah, starkID: stark},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{eliasID: {}, hannahID: {}, starkID: {}}},
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerRumorToldSomeoneNotHere is the LLM-545 co-presence-guard fixture: the
// traveler has passed his word to Hannah Boggs, but present company is Goodman
// Stark alone — Hannah exists in the snapshot yet is NOT in the scene. The
// golden pins the plain fresh "Word reached you …" line with NO mention of
// Hannah: the shared-word memory matters face to face only (the LLM-547
// posture), and naming an absent listener would hand the model a person to go
// find instead of news to trade here.
func travelerRumorToldSomeoneNotHere() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID  = sim.ActorID("vstr-elias")
		hannahID = sim.ActorID("hannah")
		starkID  = sim.ActorID("stark")
		tavern   = sim.StructureID("tavern")
		inn      = sim.StructureID("inn")
		huddleID = sim.HuddleID("h1")
	)
	now := 540
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Goodman Stark": {}},
		VisitorState: &sim.VisitorState{
			SpendBudget:       8,
			Archetype:         "peddler",
			Origin:            "Boston",
			Disposition:       "weary",
			Payload:           "Ezekiel Crane turned out a plow for the Hale farm",
			PayloadSharedWith: []sim.ActorID{hannahID},
		},
	}
	stark := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Goodman Stark",
		Role:              "farmer",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Needs:             map[sim.NeedKey]int{},
	}
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		InsideStructureID: inn, // elsewhere — must not be named in the preface
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias, hannahID: hannah, starkID: stark},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
			inn:    plainStructure(inn, "Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, Members: map[sim.ActorID]struct{}{eliasID: {}, starkID: {}}},
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerObservedByVillager is the LLM-370 observer-cue fixture: a villager
// (Goodwife Bishop) standing in the Tavern with a co-present transient traveler
// she doesn't yet know by name. The golden pins the "## Around you" observer cue —
// "a peddler lately come from Boston is here with you." — in place of the bare "a
// stranger" an unacquainted, roleless actor rendered before LLM-370. Disposition
// (weary) is deliberately absent from the observer's view; it colors only the
// traveler's own preface.
func travelerObservedByVillager() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		bishopID = sim.ActorID("bishop")
		eliasID  = sim.ActorID("vstr-elias")
		tavern   = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	bishop := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Goodwife Bishop",
		Role:              "goodwife",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		// The traveler is within earshot but a stranger to Bishop — no acquaintance
		// entry, so the observer cue names them by archetype + origin, not by name.
		ColocatedAudienceIDs: []sim.ActorID{eliasID},
	}
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{bishopID: bishop, eliasID: elias},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	return snap, bishopID, nil
}

// travelerReturnerSelfPreface is the LLM-372 fixture: the same traveler as
// travelerSelfIdentityPreface, but now a RETURNER on a repeat visit — its
// ActorSnapshot carries a Returner projection (VisitCount 3, and a remembered
// player Jeff last seen a few weeks back). The golden pins the continuity block
// that closes the self-preface, so the stateless salem-visitor VA greets someone
// it remembers rather than a stranger. Present only for VisitCount >= 2 (a nil
// Returner — the one-shot / first-visit case — is travelerSelfIdentityPreface).
func travelerReturnerSelfPreface() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID = sim.ActorID("vstr-elias")
		tavern  = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 8,
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
			RecurringID: "rvis-0000e71a",
		},
		Returner: &sim.ReturnerSnapshot{
			VisitCount: 3,
			KnownHere: []sim.ReturnerKnownPC{
				{PCActorID: "pc-jeff", DisplayName: "Jeff", Recency: sim.RecencyWeeks},
			},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// travelerReturnerEpisodicMemory is the LLM-383 fixture: the returner from
// travelerReturnerSelfPreface, but now its acquaintance with Jeff carries a FOLDED
// episodic summary (the visit-end fold's output). The golden pins the remembered
// specifics woven into the self-preface after the recognition clause — the
// distilled impression prose, scenes-not-stats, never a raw fact list. The
// no-summary case (coarse familiarity only) is travelerReturnerSelfPreface.
func travelerReturnerEpisodicMemory() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		eliasID = sim.ActorID("vstr-elias")
		tavern  = sim.StructureID("tavern")
	)
	now := 540 // 09:00 — morning
	elias := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Elias Drum the peddler",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		VisitorState: &sim.VisitorState{
			SpendBudget: 8,
			Archetype:   "peddler",
			Origin:      "Boston",
			Disposition: "weary",
			RecurringID: "rvis-0000e71a",
		},
		Returner: &sim.ReturnerSnapshot{
			VisitCount: 3,
			KnownHere: []sim.ReturnerKnownPC{
				{
					PCActorID:   "pc-jeff",
					DisplayName: "Jeff",
					Recency:     sim.RecencyWeeks,
					Summary:     "Jeff frets over the fence line on his north field; last visit he bought a bundle of your nails to set it right.",
				},
			},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{eliasID: elias},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: eliasID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: tavern},
			SourceEventID:  1,
		},
	}
	return snap, eliasID, warrants
}

// worklessTiredRejoinerSelfActionTrail is the LLM-217 fixture: the live Patience
// Walker oscillation, mid-loop. She is workless (no work structure), tired, and
// back in the Tavern huddle with John Ellis after two announce-leave-return
// cycles. The huddle ring holds John's two byte-identical re-greetings plus her
// own "I'll head home now." — with At stamps spanning the cycles — and the
// snapshot's ActionLog carries her consumed/departed/arrived trail (plus one of
// John's arrivals, which the subject filter must drop, and one of her own spoke
// entries in the CURRENT huddle, which the ring de-dup must keep out of the
// trail). Fixed PublishedAt, utterances and log entries stamped relative to it
// → byte-stable.
func worklessTiredRejoinerSelfActionTrail() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		johnID     = sim.ActorID("john")
		tavern     = sim.StructureID("tavern")
		home       = sim.StructureID("walker_residence")
		huddleID   = sim.HuddleID("tavern_huddle")
	)
	start, end := 360, 1260 // John's working hours 06:00–21:00
	now := 15*60 + 50       // 15:50 — afternoon, John on shift
	published := time.Date(2026, 7, 1, 19, 50, 0, 0, time.UTC)
	ago := func(sec int) time.Time { return published.Add(-time.Duration(sec) * time.Second) }
	patience := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Patience Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		HomeStructureID:   home,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             3,
		Needs:             map[sim.NeedKey]int{"tiredness": sim.DefaultTirednessRedThreshold},
	}
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
			home:   plainStructure(home, "Walker Residence"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{patienceID: {}, johnID: {}},
				RecentUtterances: []sim.Utterance{
					{SpeakerID: johnID, SpeakerName: "John Ellis", Text: "Welcome back to the tavern, Patience!", At: ago(170)},
					{SpeakerID: patienceID, SpeakerName: "Patience Walker", Text: "I'll head home now.", At: ago(150)},
					{SpeakerID: johnID, SpeakerName: "John Ellis", Text: "Welcome back to the tavern, Patience!", At: ago(8)},
				},
			},
		},
		ActionLog: []sim.ActionLogEntry{
			{Seq: 1, ActorID: patienceID, OccurredAt: ago(480), ActionType: sim.ActionTypeConsumed, Text: "carrot", HuddleID: huddleID},
			{Seq: 2, ActorID: patienceID, OccurredAt: ago(420), ActionType: sim.ActionTypeDeparted, Text: "Tavern"},
			// John's own arrival — the subject filter drops it from HER trail.
			{Seq: 3, ActorID: johnID, OccurredAt: ago(300), ActionType: sim.ActionTypeWalked, Text: "Tavern"},
			{Seq: 4, ActorID: patienceID, OccurredAt: ago(240), ActionType: sim.ActionTypeWalked, Text: "Tavern"},
			// Her announce line, spoken IN the current huddle — the ring above
			// renders it, so the self-action trail must NOT repeat it.
			{Seq: 5, ActorID: patienceID, OccurredAt: ago(150), ActionType: sim.ActionTypeSpoke, Text: "I'll head home now.", HuddleID: huddleID},
			{Seq: 6, ActorID: patienceID, OccurredAt: ago(130), ActionType: sim.ActionTypeDeparted, Text: "Tavern"},
			{Seq: 7, ActorID: patienceID, OccurredAt: ago(45), ActionType: sim.ActionTypeWalked, Text: "Tavern"},
		},
	}
	return snap, patienceID, nil
}

// sharedNpcWithSoul is the LLM-199 case: a shared-VA keeper standing at her own
// post during working hours, carrying a synthesized about_me soul. The golden
// pins that "## Who you are" renders that prose (the empty-block fix) rather
// than a bare header — the render now emits AboutMe, gated by a non-empty value.
func sharedNpcWithSoul() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		inn      = sim.StructureID("wayfarer_inn")
	)
	start, end := 360, 1260 // working hours 06:00–21:00
	now := 540              // 09:00 — on shift
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Hannah",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		Narrative: &sim.NarrativeState{
			AboutMe: "I am Hannah, keeper of the Wayfarer Inn. My days run to the rhythm of the hearth and the door — I take a quiet pride in a warm room and a fair reckoning, and I have come to know the regulars by their thirst.",
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "Wayfarer Inn"),
		},
	}
	warrants := []sim.WarrantMeta{
		{
			TriggerActorID: hannahID,
			Reason:         sim.ArrivalWarrantReason{AtStructureID: inn},
			SourceEventID:  1,
		},
	}
	return snap, hannahID, warrants
}

// sharedNpcBystanderHearsAddressedSpeech is the LLM-432 fixture: the live
// Patience Walker misfire of 2026-07-15. A shared-VA villager sits in the
// evening tavern when a PC asks a co-present third party by name — "ezekiel,
// you sleeping over there?" — and her soul prose, like the live one, never
// states her own name. Before the fix nothing in the prompt did either
// (the shared VA's system prompt is a generic sim context), so she could
// not rule herself out as the addressee and answered as if teased. The
// golden pins the "You are Patience Walker." self-name line opening
// "## Who you are" ahead of the soul prose.
func sharedNpcBystanderHearsAddressedSpeech() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		ezekielID  = sim.ActorID("ezekiel")
		jeffereyID = sim.ActorID("jefferey")
		tavern     = sim.StructureID("tavern")
		huddleID   = sim.HuddleID("tavern_huddle")
	)
	now := 19*60 + 35 // 19:35 — the tavern of an evening
	published := time.Date(2026, 7, 15, 23, 35, 0, 0, time.UTC)
	ago := func(sec int) time.Time { return published.Add(-time.Duration(sec) * time.Second) }
	patience := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Patience Walker",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             22,
		Needs:             map[sim.NeedKey]int{},
		Narrative: &sim.NarrativeState{
			AboutMe: "My life in the village is a tapestry of moments. My family remains a source of comfort, and I find steadiness in the day's work and the fire of an evening.",
		},
	}
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             15,
		Needs:             map[sim.NeedKey]int{},
	}
	jefferey := &sim.ActorSnapshot{
		Kind:              sim.KindPC,
		DisplayName:       "Jefferey",
		State:             sim.StateIdle,
		InsideStructureID: tavern,
		CurrentHuddleID:   huddleID,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			patienceID: patience,
			ezekielID:  ezekiel,
			jeffereyID: jefferey,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:      huddleID,
				Members: map[sim.ActorID]struct{}{patienceID: {}, ezekielID: {}, jeffereyID: {}},
				RecentUtterances: []sim.Utterance{
					{SpeakerID: jeffereyID, SpeakerName: "Jefferey", Text: "ezekiel, you sleeping over there?", At: ago(10)},
				},
			},
		},
	}
	warrants := []sim.WarrantMeta{{
		TriggerActorID: jeffereyID,
		Reason:         sim.PCSpeechWarrantReason{SpeechID: 1, Speaker: jeffereyID, Excerpt: "ezekiel, you sleeping over there?"},
		SourceEventID:  1,
		HuddleID:       huddleID,
		OccurredAt:     published,
	}}
	return snap, patienceID, warrants
}

// tiredKeeperAtPostOnShift is the LLM-100 positive case: a tired keeper standing
// inside its own post during its shift, so the rest-in-place (take_break) cue
// fires. No co-present actor, no orders — the rest section is the point.
func tiredKeeperAtPostOnShift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             3,
		Needs:             map[sim.NeedKey]int{"tiredness": sim.DefaultTirednessRedThreshold},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Blacksmith"),
		},
	}
	return snap, ezekielID, nil
}

// wearyResidentInOwnHome is the LLM-214 fixture: a weary salem-vendor standing
// INSIDE its own home (home != work), off-shift in the evening. Before the fix the
// "## How you can rest" list handed it the home structure_id as a move_to target
// ("sleep in your own bed (structure_id …)") for the structure it was already in —
// the no-op move Lewis / Anne Walker looped on — and the anchor pointer told it to
// "head back there whenever you wish". The golden pins the in-place cues: the rest
// section leads with the RestAtHome take_break bullet (no home id), and the anchor
// states "You're home" while keeping only the workplace as a reachable move target.
// Off-shift and already home, so no to-work / wind-down steer clutters the pin.
func wearyResidentInOwnHome() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		anneID = sim.ActorID("anne")
		home   = sim.StructureID("walker_residence")
		garden = sim.StructureID("walker_garden")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 1200             // 20:00 — off shift, home for the evening
	anne := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Walker",
		Role:              "gardener",
		State:             sim.StateIdle,
		WorkStructureID:   garden,
		HomeStructureID:   home,
		InsideStructureID: home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{"tiredness": 23},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{anneID: anne},
		Structures: map[sim.StructureID]*sim.Structure{
			home:   plainStructure(home, "Walker Residence"),
			garden: plainStructure(garden, "Walker Garden"),
		},
	}
	return snap, anneID, nil
}

// walkerHomeAfterShutStoreTrip is the LLM-366 scenario: a workless, scheduleless
// salem-vendor Walker (the live Silence Walker) sits idle inside its own home in
// the morning — the "find work / see who's about" decision turn that had no
// legible shut-status signal at decision time. Its recent self-action trail shows
// it just walked to the General Store and found it shut (a live ObservedClosed
// memory within the 4h TTL), so "## What you've recently done" renders "You went to
// the General Store but found it shut, no one tending it" instead of a neutral
// "You arrived at the General Store" — the LLM-217 churn-mirror finally carrying the
// dead-end outcome that broke the home↔closed-store loop. This is the FIRST golden
// to exercise the self-action trail (it sets PublishedAt + an ActionLog), so the
// recently-done block appears in a golden here for the first time.
func walkerHomeAfterShutStoreTrip() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		home      = sim.StructureID("walker_residence")
		store     = sim.StructureID("general_store")
	)
	published := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	morning := 465 // 07:45 — before the General Store's 09:00 open
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		HomeStructureID:   home,
		InsideStructureID: home,
		Coins:             25,
		Needs:             map[sim.NeedKey]int{},
		// Found the store shut 4 minutes ago (the walked trail entry below), still
		// within the 4h closed-business TTL.
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: store, Condition: sim.ObservedClosed}: published.Add(-4 * time.Minute),
		}),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &morning,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			home:  plainStructure(home, "Walker Residence"),
			store: plainStructure(store, "General Store"),
		},
		// The churn trail: home → (shut) store → home, oldest first in the log.
		// DestStructureID is where each trip was AIMED; StructureID is the
		// conversational scope it came to rest in. The store trip carries a BLANK
		// scope on purpose — that is what the engine really stamps for a walk to a
		// shut business, since loiterScopeConversable refuses the outdoor scope of a
		// keeperless structure (LLM-463). The home trips carry both: the walker ends
		// up inside its own home, which is scoped unconditionally.
		ActionLog: []sim.ActionLogEntry{
			{Seq: 1, ActorID: silenceID, OccurredAt: published.Add(-6 * time.Minute), ActionType: sim.ActionTypeWalked, Text: "Walker Residence", DestStructureID: home, StructureID: home},
			{Seq: 2, ActorID: silenceID, OccurredAt: published.Add(-4 * time.Minute), ActionType: sim.ActionTypeWalked, Text: "General Store", DestStructureID: store},
			{Seq: 3, ActorID: silenceID, OccurredAt: published.Add(-2 * time.Minute), ActionType: sim.ActionTypeWalked, Text: "Walker Residence", DestStructureID: home, StructureID: home},
		},
	}
	return snap, silenceID, nil
}

// turnedAwayOutdoorsAtShutTavern is the LLM-463 scenario: the villager is standing
// at the shut Tavern's door, not remembering it from elsewhere. Moses James walked
// over at midday, found John Ellis abed and the taproom keeperless, and his next
// turn rendered from that spot — "You are outdoors by the Tavern" with the live
// "The Tavern is shut" cue, and a trail that said only "You arrived at the Tavern."
// Nothing in front of him said the trip had been a dead end, so he walked it again.
//
// The geometry is the live Tavern's (anchor tile 106,131; loiter_offset -3,+1 →
// pin 103,132; footprint 3/4/6/0 → y ends at 131). That puts the visitor slot one
// row BELOW the building, so the actor is genuinely outdoors — and the walked
// action-log row below carries NO conversational-scope StructureID, exactly as the
// engine stamps it, because loiterScopeConversable refuses the outdoor scope of a
// keeperless structure (LLM-359). The dead-end line must survive on
// DestStructureID alone.
func turnedAwayOutdoorsAtShutTavern() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID = sim.ActorID("moses")
		johnID  = sim.ActorID("john")
		tavern  = sim.StructureID("tavern")
		farm    = sim.StructureID("james_farm")
	)
	published := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	midday := 720
	arrived := published.Add(-1 * time.Minute)
	moses := &sim.ActorSnapshot{
		Kind:            sim.KindNPCStateful,
		DisplayName:     "Moses James",
		Role:            "farmer",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 103, Y: 133}, // a visitor slot on the pin's ring
		WorkStructureID: farm,
		Coins:           28,
		Needs:           map[sim.NeedKey]int{},
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: tavern, Condition: sim.ObservedClosed}: arrived,
		}),
	}
	// The sole keeper, abed on the premises — an asleep keeper is not tending, so
	// the business reads shut (LLM-126).
	john := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "John Ellis",
		Role:              "tavernkeeper",
		State:             sim.StateSleeping,
		Pos:               sim.TilePos{X: 103, Y: 130},
		WorkStructureID:   tavern,
		HomeStructureID:   tavern,
		InsideStructureID: tavern,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &midday,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses, johnID: john},
		Structures: map[sim.StructureID]*sim.Structure{
			tavern: plainStructure(tavern, "Tavern"),
			farm:   plainStructure(farm, "James Farm"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {
				ID: sim.VillageObjectID(tavern), AssetID: "tavern-asset", DisplayName: "Tavern",
				OwnerActorID: johnID, EntryPolicy: sim.EntryPolicyOpen,
				Pos:           sim.WorldPos{X: float64(106-sim.PadX) * sim.TileSize, Y: float64(131-sim.PadY) * sim.TileSize},
				LoiterOffsetX: intp(-3), LoiterOffsetY: intp(1),
				Tags: []string{sim.TagBusiness, sim.VisitorTagTavern, "lodging"},
			},
		},
		Assets: map[sim.AssetID]*sim.Asset{
			"tavern-asset": {
				ID: "tavern-asset", Category: "structure",
				DoorOffsetX: intp(-2), DoorOffsetY: intp(-1),
				FootprintLeft: 3, FootprintRight: 4, FootprintTop: 6, FootprintBottom: 0,
			},
		},
		// The trip that just happened. StructureID is blank — the shut Tavern gives
		// the arriver no conversational scope — so DestStructureID is the ONLY thing
		// tying this row to the business it was aimed at.
		ActionLog: []sim.ActionLogEntry{
			{Seq: 1, ActorID: mosesID, OccurredAt: published.Add(-3 * time.Minute), ActionType: sim.ActionTypeDeparted, Text: "James Farm", StructureID: farm},
			{Seq: 2, ActorID: mosesID, OccurredAt: arrived, ActionType: sim.ActionTypeWalked, Text: "Tavern", DestStructureID: tavern},
		},
	}
	return snap, mosesID, nil
}

// homedWorkerEveningTavernOpen is the LLM-149 (Lever 2) positive case: a homed
// day-shift agent, off-shift and awake in the evening window [shift-end, 22:00),
// standing at its workplace after closing up. The evening "tavern's open" cue
// fires in ## Around you, and the off-shift go-home wind-down steer is suppressed
// in-window so the cue is the single voice. No co-present actor, no orders — the
// evening invitation is the point.
func homedWorkerEveningTavernOpen() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
		home      = sim.StructureID("crane_cottage")
		tavern    = sim.StructureID("tavern")
	)
	start, end := 420, 1140 // 07:00–19:00
	now := 1230             // 20:30 — off shift, inside the evening window
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — the evening window's close
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			home:   plainStructure(home, "Crane Cottage"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		// The tavern venue: a VillageObject tagged "tavern" bridged to the
		// same-id Structure (the shared-identity bridge nearestTaggedVenue reads).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
	}
	return snap, ezekielID, nil
}

// unscheduledWorkerEveningTavernOpen is the LLM-352 case: a homed LABOR VENDOR — a
// Walker — with NO schedule row and no fixed workplace, day-active on the world
// dawn/dusk window (LLM-137, gated on the AttrWorker marker). Standing outdoors in
// the [dusk, bedtime) evening, it now earns the SAME "tavern's open" invitation a
// dawn→dusk-scheduled worker gets, instead of being shut out of the evening (and
// bedded at dusk) because it carried no schedule row. DawnMinute/DuskMinute is set
// so shiftWindowBounds supplies the day-active fallback the evening keys on. A
// regression that re-restricts the evening to schedule-carriers drops the
// invitation here and shows in the diff.
func unscheduledWorkerEveningTavernOpen() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID = sim.ActorID("lewis")
		home    = sim.StructureID("walker_residence")
		tavern  = sim.StructureID("tavern")
	)
	now := 1230 // 20:30 — after dusk (19:00), inside the evening window
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared, // the Walkers run on the shared salem-vendor VA
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: "", // standing outdoors in the village at dusk
		HomeStructureID:   home,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		AttributeSlugs:    []string{sim.AttrWorker},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — the evening window's close
		DawnMinute:           420,  // 07:00
		DuskMinute:           1140, // 19:00
		DawnDuskMinuteOK:     true,
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			home:   plainStructure(home, "Walker Residence"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
	}
	return snap, lewisID, nil
}

// walkerWomenEveningAtHomeMayTurnIn reconstructs the live LLM-447 state: the three
// Walker women huddled in the Walker Residence on the evening of 2026-07-16, at
// 21:20 — past dusk (19:00), before the auto-bed hour (22:00).
//
// The fixture's load-bearing details, each of which killed an alternative fix:
//
//   - tiredness 12 on all three, below the awareness floor of 13. Their prompts
//     carried no tiredness line at all, so the red-tiredness march (≥16) was
//     nowhere near firing and a meter-gated bedtime verb would never have
//     appeared. Bedtime here is clock-and-social.
//   - unscheduled workers, so isActorOnShift reads false and they qualify —
//     the LLM-352 shape the evening was widened for in the first place.
//   - all three inside their shared home in one huddle, which is the trap:
//     home is the terminal destination of everyone's day, so this conversation
//     could not end by anyone leaving. Someone had to be able to end it by
//     going to bed.
func walkerWomenEveningAtHomeMayTurnIn() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID  = sim.ActorID("silence")
		patienceID = sim.ActorID("patience")
		anneID     = sim.ActorID("anne")
		home       = sim.StructureID("walker_residence")
		tavern     = sim.StructureID("tavern")
		huddleID   = sim.HuddleID("hud-longgoodnight")
	)
	now := 1280 // 21:20 — 2h20m past dusk, 40m before the 22:00 auto-bed hour
	// Housemates know each other by name — without this they render as "a stranger
	// (your housemate)", which is fixture noise in a scene about a family evening.
	knows := func(names ...string) map[string]sim.Acquaintance {
		m := make(map[string]sim.Acquaintance, len(names))
		for _, n := range names {
			m[n] = sim.Acquaintance{}
		}
		return m
	}
	walker := func(name string, x int, acquainted map[string]sim.Acquaintance) *sim.ActorSnapshot {
		return &sim.ActorSnapshot{
			Acquaintances:     acquainted,
			Kind:              sim.KindNPCShared, // the Walkers run on the shared salem-vendor VA
			DisplayName:       name,
			State:             sim.StateIdle,
			Pos:               sim.TilePos{X: x, Y: 10},
			InsideStructureID: home,
			HomeStructureID:   home,
			CurrentHuddleID:   huddleID,
			Coins:             8,
			// 12 — below the awareness floor (13), so nothing on the page mentions
			// being tired. The cue must fire anyway.
			Needs:          map[sim.NeedKey]int{"tiredness": 12},
			AttributeSlugs: []string{sim.AttrWorker},
		}
	}
	silence := walker("Silence Walker", 10, knows("Patience Walker", "Anne Walker"))
	patience := walker("Patience Walker", 11, knows("Silence Walker", "Anne Walker"))
	anne := walker("Anne Walker", 12, knows("Silence Walker", "Patience Walker"))
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — where the deterministic auto-bed would finally take them
		DawnMinute:           420,  // 07:00
		DuskMinute:           1140, // 19:00 — the turn_in window's open
		DawnDuskMinuteOK:     true,
		NeedThresholds:       sim.NeedThresholds{},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			silenceID: silence, patienceID: patience, anneID: anne,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			home:   plainStructure(home, "Walker Residence"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {ID: huddleID, StructureID: home, Members: map[sim.ActorID]struct{}{
				silenceID: {}, patienceID: {}, anneID: {},
			}},
		},
	}
	return snap, silenceID, nil
}

// farmOwnerSettledInTavernEvening is the LLM-345 case, reconstructed from the live
// trace: Elizabeth Ellis, a farm owner who owes 3 upkeep shovels, finishes her farm
// day at 18:00 and takes the tavern invitation. At 19:40 she is standing INSIDE the
// Tavern. Before this fix the evening cue vanished at the threshold and her prompt
// held only the farm ledger — "Upkeep calls for 3 shovels…", the anchors line offering
// her farm and her house — under a coda ranking obligations above idle matters; she
// read it and walked home inside ninety seconds.
//
// The golden pins both levers at once. Lever A: the settled-in scene renders in
// ## Around you ("… here you are inside the Tavern of an evening …") with no
// destination to walk to. Lever B: "## Farm upkeep" is ABSENT — the walk-away errand
// yields to the room for the evening. A regression that resurrects the shovel cue in
// the pub, or drops the room, shows in the diff.
func farmOwnerSettledInTavernEvening() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		actorID = sim.ActorID("elizabeth")
		home    = sim.StructureID("ellis_residence")
		farm    = sim.StructureID("ellis_farm")
		tavern  = sim.StructureID("tavern")
	)
	zero := 0
	start, end := 360, 1080 // 06:00–18:00, the farm day
	now := 1180             // 19:40 — off shift, inside the [18:00, 22:00) evening window
	tavernPos := sim.WorldPos{X: 400, Y: 400}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               tavernPos.Tile(),
		InsideStructureID: tavern,
		WorkStructureID:   farm,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             95, // floor 30, band 20 → owes 3 shovels, none in hand
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:         &now,
		LodgingBedtimeMinute:     1320, // 22:00 — the evening window's close
		NeedThresholds:           sim.NeedThresholds{},
		Assets:                   emptyAssetSet,
		FarmUpkeepFloor:          30,
		FarmUpkeepCoinsPerShovel: 20,
		Actors:                   map[sim.ActorID]*sim.ActorSnapshot{actorID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			home:   plainStructure(home, "Ellis Residence"),
			farm:   plainStructure(farm, "Ellis Farm"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// The venue: a "tavern"-tagged VillageObject bridged to the same-id Structure.
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: tavernPos},
			// Her farm — the workplace the upkeep errand would have sent her away to tend,
			// sharing its id with the same-named Structure (the shared-identity bridge).
			sim.VillageObjectID(farm): {
				ID:            sim.VillageObjectID(farm),
				DisplayName:   "Ellis Farm",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				OwnerActorID:  actorID,
				Tags:          []string{sim.TagFarm},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, actorID, nil
}

// homedWorkerEveningBatchHoldsLeisure is the LLM-335 case: the SAME homed day-shift
// agent and evening as homedWorkerEveningTavernOpen (Ezekiel at his forge at 20:30, in
// the [shift-end, 22:00) window, affordably), but with a Cheese batch in the works at
// his post. The batch pins him there (LLM-319 pause model), so buildEveningLeisure
// returns the BatchHold variant: the "tavern's open" invitation yields to a quiet hold
// and the standing "You are making a batch of Cheese …" line still renders, so the scene
// speaks with ONE voice (stay a little longer) instead of nagging him to the tavern AND
// to mind the cheese. No RestockPolicy → the "## Your trade" cue is out of scope here
// (it is already gone mid-batch anyway); the evening hold is the point.
func homedWorkerEveningBatchHoldsLeisure() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
		home      = sim.StructureID("crane_cottage")
		tavern    = sim.StructureID("tavern")
	)
	start, end := 420, 1140 // 07:00–19:00
	now := 1230             // 20:30 — off shift, inside the evening window
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		// A produce call opened this cycle (LLM-319): cheese in the works at the post.
		ProductionItem:             "cheese",
		ProductionBatchQty:         1,
		ProductionRemainingSeconds: 600, // ~10 minutes of base-rate work left
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — the evening window's close
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			home:   plainStructure(home, "Crane Cottage"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{"cheese": cheeseKind()},
		// The tavern venue: a VillageObject tagged "tavern" bridged to the same-id
		// Structure (the shared-identity bridge nearestTaggedVenue reads).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
	}
	return snap, ezekielID, nil
}

// lodgerEveningTavernOpen is the LLM-311 case: the SAME evening as
// homedWorkerEveningTavernOpen, but the agent is homeless-by-design (HomeStructureID
// "") and lodges via an active nightly ledger room grant at the Inn — the canonical
// rent-a-room NPC (Ezekiel). Before LLM-311 the living-evening scope was homed-only, so
// this agent got NO tavern invitation and the off-shift wind-down steered it to its
// rented room the whole evening (the live Inn↔Blacksmith oscillation). With the
// night-place scope the cue fires exactly as for a homed peer — its co-equal "stay in"
// destination is the rented Inn (destination: inn), not an empty home token — and the
// go-home/wind-down steer ("Your working hours are over …") is suppressed. Fixed
// PublishedAt (the grant clock) → byte-stable. Makes TestEveningCueReplacesGoHomeSteer
// non-vacuous for the lodger arm.
func lodgerEveningTavernOpen() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
		inn       = sim.StructureID("inn")
		tavern    = sim.StructureID("tavern")
		innRoom   = sim.RoomID(42)
	)
	start, end := 420, 1140 // 07:00–19:00
	now := 1230             // 20:30 — off shift, inside the evening window
	published := time.Date(2026, 7, 6, 20, 30, 0, 0, time.UTC)
	grantExpires := published.Add(12 * time.Hour) // active, unexpired
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		HomeStructureID:   "", // homeless by design — lodges at the Inn
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             12,
		Needs:             map[sim.NeedKey]int{},
		RoomAccess: map[sim.RoomAccessKey]*sim.RoomAccess{
			{RoomID: innRoom, Source: sim.AccessSourceLedger}: {
				RoomID: innRoom, Source: sim.AccessSourceLedger, Active: true, ExpiresAt: &grantExpires,
			},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — the evening window's close
		PublishedAt:          published,
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			tavern: plainStructure(tavern, "the Tavern"),
			// The Inn holds the lodger's rented room (room 42, distinct from the room id
			// plainStructure assigns) so structureForRoom resolves the night-place to it.
			inn: {
				ID: inn, DisplayName: "the Inn",
				Rooms: []*sim.Room{{ID: innRoom, StructureID: inn, Name: "private_1"}},
			},
		},
		// The tavern venue: a VillageObject tagged "tavern" bridged to the same-id
		// Structure (the shared-identity bridge nearestTaggedVenue reads).
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
	}
	return snap, ezekielID, nil
}

// homedWorkerEveningBrokeStillInvited is the LLM-353 case: the same homed day-shift
// agent as homedWorkerEveningTavernOpen, in the evening window, holding only 2 coins
// with the co-located keeper's cheapest drink at 3 (ale, retail 3). Coin no longer gates
// the evening — Salem pays in goods as readily as coin — so the empty purse is no barrier:
// the tavern invitation fires and the off-shift go-home wind-down steer stays suppressed,
// exactly as for a flush agent. Before LLM-353 the coin floor turned this agent away. This
// is the DoD case (a broke agent still receives the invitation). No needs / no PriceBook /
// no orders → byte-stable.
func homedWorkerEveningBrokeStillInvited() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		keeperID  = sim.ActorID("innkeep")
		forge     = sim.StructureID("blacksmith")
		home      = sim.StructureID("crane_cottage")
		tavern    = sim.StructureID("tavern")
	)
	start, end := 420, 1140 // 07:00–19:00
	now := 1230             // 20:30 — off shift, inside the evening window
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		HomeStructureID:   home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             2, // under the cheapest drink (ale, retail 3) — no longer a barrier (LLM-353)
		Needs:             map[sim.NeedKey]int{},
	}
	keeper := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		Inventory:         map[sim.ItemKind]int{"ale": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320, // 22:00 — the evening window's close
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, keeperID: keeper},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:  plainStructure(forge, "Blacksmith"),
			home:   plainStructure(home, "Crane Cottage"),
			tavern: plainStructure(tavern, "the Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"ale": {OutputItem: "ale", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 3},
		},
	}
	return snap, ezekielID, nil
}

// hungryWorkerWithMeansRedirectedToEat is the LLM-276 scenario: a workless, on-shift,
// idle worker whose hunger has climbed into the upper felt band (15, below the red-line
// 18) and who can resolve it now — it holds coin, a free bush is nearby, and a keeper
// sells porridge. The seek-work backstop has stamped a TendNeed warrant, so the prompt
// must steer the worker to EAT (the tend-need felt line + the "## What you can eat or
// drink" options + the one-target need-redirect) and must NOT show the businesses
// directory or the solicit-work hustle — those yield to the resolvable need exactly as
// they do for a red need. The perception half of the live Silence Walker beg-for-food
// fix.
func hungryWorkerWithMeansRedirectedToEat() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		keeperID  = sim.ActorID("keeper")
		home      = sim.StructureID("walker_house")
		store     = sim.StructureID("general_store")
		bush      = sim.VillageObjectID("blueberry_bush")
	)
	now := 720 // 12:00 — on shift (day window [420, 1140))
	silence := &sim.ActorSnapshot{
		Kind:            sim.KindNPCShared,
		DisplayName:     "Silence Walker",
		Role:            "villager",
		State:           sim.StateIdle,
		HomeStructureID: home,
		Coins:           5,
		Needs:           map[sim.NeedKey]int{"hunger": 15},
		AttributeSlugs:  []string{sim.AttrWorker},
	}
	keeper := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Anne Putnam",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   store,
		InsideStructureID: store,
		Inventory:         map[sim.ItemKind]int{"porridge": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		DawnMinute:       420,
		DuskMinute:       1140,
		DawnDuskMinuteOK: true,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence, keeperID: keeper},
		Structures: map[sim.StructureID]*sim.Structure{
			home:  plainStructure(home, "Walker House"),
			store: plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			bush:                       {DisplayName: "Blueberry Bush", Pos: sim.WorldPos{X: 40, Y: 40}, Refreshes: []*sim.ObjectRefresh{{Attribute: "hunger", Amount: -2}}},
			sim.VillageObjectID(store): {Pos: sim.WorldPos{X: 80, Y: 0}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"porridge": {DisplayLabel: "a bowl of porridge", Satisfies: []sim.ItemSatisfaction{{Attribute: "hunger", Immediate: 8}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"porridge": {OutputItem: "porridge", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 2, RetailPrice: 5},
		},
	}
	return snap, silenceID, []sim.WarrantMeta{{TriggerActorID: silenceID, Reason: sim.TendNeedWarrantReason{Need: "hunger"}}}
}

// homedWorkersEveningCommonsStillSolicits is the LLM-353 case: two homed day-shift
// workers (different homes + trades, so solicitable to each other) off shift in the
// evening window, together at the Commons — neither at home nor the tavern, so the
// evening invitation still fires. The subject carries AttrWorker and is flush (10 coins).
// The solicit-work suppression keys on tookEveningLeisure (gone to the pub), not on
// affluence: this worker is still in the road, so he is STILL offered solicit_work even
// though he could afford a drink — a man still in the road might be job-hunting. Before
// LLM-353 affluence suppressed the affordance here. Pins the DoD invariant (an off-shift
// worker who has not taken the evening still sees the seek-work cues). Fixed PublishedAt,
// no orders/PriceBook → byte-stable.
func homedWorkersEveningCommonsStillSolicits() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		lewisID   = sim.ActorID("lewis")
		keeperID  = sim.ActorID("innkeep")
		forge     = sim.StructureID("blacksmith")
		farm      = sim.StructureID("walker_farm")
		ezHome    = sim.StructureID("crane_cottage")
		lwHome    = sim.StructureID("walker_house")
		commons   = sim.StructureID("commons")
		tavern    = sim.StructureID("tavern")
		huddle    = sim.HuddleID("h1")
	)
	start, end := 420, 1140 // 07:00–19:00
	now := 1230             // 20:30 — off shift, inside the evening window
	published := time.Date(2026, 6, 30, 20, 30, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		HomeStructureID:   ezHome,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             10, // flush (would afford ale, retail 3) yet below the comfort ceiling (25) — still solicits (LLM-353)
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		HomeStructureID:   lwHome,
		InsideStructureID: commons,
		CurrentHuddleID:   huddle,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             8,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Ezekiel Crane": {}},
	}
	keeper := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   tavern,
		InsideStructureID: tavern,
		Inventory:         map[sim.ItemKind]int{"ale": 5},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:          published,
		LocalMinuteOfDay:     &now,
		LodgingBedtimeMinute: 1320,
		NeedThresholds:       sim.NeedThresholds{},
		Actors:               map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, lewisID: lewis, keeperID: keeper},
		Structures: map[sim.StructureID]*sim.Structure{
			forge:   plainStructure(forge, "Blacksmith"),
			farm:    plainStructure(farm, "Walker Farm"),
			ezHome:  plainStructure(ezHome, "Crane Cottage"),
			lwHome:  plainStructure(lwHome, "Walker House"),
			commons: plainStructure(commons, "Village Commons"),
			tavern:  plainStructure(tavern, "the Tavern"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {Tags: []string{sim.VisitorTagTavern}, Pos: sim.WorldPos{X: 0, Y: 0}},
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{ezekielID: {}, lewisID: {}}},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"ale": {OutputItem: "ale", OutputQty: 1, RateQty: 1, RatePerHours: 1, WholesalePrice: 1, RetailPrice: 3},
		},
		ItemKinds: foodDrinkCatalog(),
	}
	return snap, ezekielID, nil
}

// keeperWithReadyOrder is an order-bearing scenario unblocked by the LLM-106
// render-clock fix: Hannah, an innkeeper, holds a Ready order (a nights_stay
// check-in) for a co-present guest. The order's ExpiresAt is anchored to the
// snapshot instant (PublishedAt → RenderedAt), so the "expires in N minutes"
// clause renders deterministically — byte-stable with no wall-clock read.
func keeperWithReadyOrder() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		hannahID = sim.ActorID("hannah")
		guestID  = sim.ActorID("jeff")
		inn      = sim.StructureID("hannahs_inn")
		huddle   = sim.HuddleID("h1")
	)
	start, end := 360, 1320 // 06:00–22:00
	nowMin := 600           // 10:00, on shift
	// The render instant: ExpiresAt is set relative to this, so the expiry clause
	// is fixed regardless of when the test runs.
	published := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
	}
	guest := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Jeff",
		Role:              "traveler",
		State:             sim.StateIdle,
		InsideStructureID: inn,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalDateUTC:     time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LocalMinuteOfDay: &nowMin,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{hannahID: hannah, guestID: guest},
		Structures: map[sim.StructureID]*sim.Structure{
			inn: plainStructure(inn, "Hannah's Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{hannahID: {}, guestID: {}}},
		},
		Orders: map[sim.OrderID]*sim.Order{
			1: {
				ID:          1,
				State:       sim.OrderStateReady,
				SellerID:    hannahID,
				BuyerID:     guestID,
				Item:        "nights_stay",
				Qty:         1,
				ConsumerIDs: []sim.ActorID{guestID},
				CreatedAt:   published.Add(-2 * time.Minute),
				ExpiresAt:   published.Add(8 * time.Minute),
			},
		},
	}
	return snap, hannahID, nil
}

// smithCommissionAwaitingForge is the LLM-338 golden: a blacksmith (Ezekiel) has
// taken a co-present customer's (Elizabeth's) prepayment for a shovel he doesn't
// yet hold — a commission Order sits Ready but he holds 0 shovels, so
// DeliverOrder's gate 5 would bounce a deliver_order call. The "## Orders to
// deliver" line must render passively ("you've yet to make it") with NO
// deliver_order instruction, steering him to forge it first. The live Elizabeth
// Ellis <-> Ezekiel Crane case. snap.Recipes is left nil so the forge/trade cues
// stay out and the golden stays focused on the order book; ExpiresAt is anchored
// to PublishedAt for byte stability.
func smithCommissionAwaitingForge() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		smithID = sim.ActorID("ezekiel")
		buyerID = sim.ActorID("elizabeth")
		forge   = sim.StructureID("ezekiels_forge")
		huddle  = sim.HuddleID("h1")
	)
	start, end := 420, 960 // 07:00–16:00
	nowMin := 600          // 10:00, on shift
	published := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		CurrentHuddleID:   huddle,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		RestockPolicy:     &sim.RestockPolicy{Restock: []sim.RestockEntry{{Item: "shovel", Source: sim.RestockSourceProduce, Max: 10}}},
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "villager",
		State:             sim.StateIdle,
		InsideStructureID: forge,
		CurrentHuddleID:   huddle,
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalDateUTC:     time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		LocalMinuteOfDay: &nowMin,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{smithID: ezekiel, buyerID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Ezekiel's Forge"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{smithID: {}, buyerID: {}}},
		},
		Orders: map[sim.OrderID]*sim.Order{
			1: {
				ID:          1,
				State:       sim.OrderStateReady,
				SellerID:    smithID,
				BuyerID:     buyerID,
				Item:        "shovel",
				Qty:         1,
				ConsumerIDs: []sim.ActorID{buyerID},
				CreatedAt:   published.Add(-30 * time.Minute),
				ExpiresAt:   published.Add(5 * time.Hour),
			},
		},
	}
	return snap, smithID, nil
}

// operatingKeeperSnapshot builds a one-actor snapshot for the LLM-123 operating-
// hours cue: a homed shopkeeper standing inside his own store, with the given local
// minute (on/off shift) and an optional live stay_open commitment. No co-present
// actors, no recipes, no orders → no forge/wares/stall cue and byte-stable. The
// trade-conduct block ("How you trade:") renders iff the keeper is operating —
// on shift, or off-shift with stayOpen — which is exactly what the three scenarios
// below and the cross-scenario invariant exercise.
func operatingKeeperSnapshot(nowMin int, stayOpen bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		keeperID = sim.ActorID("moses")
		store    = sim.StructureID("general_store")
		home     = sim.StructureID("james_farm")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := nowMin
	// PublishedAt's wall-clock tracks the local minute so the stay_open OpenUntil
	// (set relative to it) is internally consistent; fixed date → byte-stable.
	published := time.Date(2026, 6, 25, nowMin/60, nowMin%60, 0, 0, time.UTC)
	moses := &sim.ActorSnapshot{
		Kind:               sim.KindNPCShared,
		DisplayName:        "Moses James",
		Role:               "shopkeeper",
		State:              sim.StateIdle,
		WorkStructureID:    store,
		InsideStructureID:  store,
		HomeStructureID:    home,
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		BusinessownerState: &sim.BusinessownerState{Flavor: "flamboyant"},
		Coins:              20,
		Needs:              map[sim.NeedKey]int{},
	}
	if stayOpen {
		ou := published.Add(2 * time.Hour) // committed to keep the store open until ~1am
		moses.OpenUntil = &ou
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{keeperID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			home:  plainStructure(home, "James Farm"),
		},
	}
	return snap, keeperID, nil
}

// keeperAtPostOnShift: keeper at his own store during business hours → the
// "How you trade:" block renders (LLM-123 positive case).
func keeperAtPostOnShift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return operatingKeeperSnapshot(600, false) // 10:00 — on shift, open for trade
}

// keeperAtClosedPostOffshiftNight: keeper at his own CLOSED store late at night,
// off shift → the trade block is gone (the LLM-123 fix); the off-shift wind-down
// "head home" steer renders instead.
func keeperAtClosedPostOffshiftNight() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return operatingKeeperSnapshot(1380, false) // 23:00 — off shift, stall closed
}

// keeperStayingOpenOffshift: keeper off shift at night but holding a live stay_open
// commitment → the trade block renders despite being off-shift (the operating-hours
// gate opens on a stay_open commitment too), and the routine wind-down is suppressed.
func keeperStayingOpenOffshift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return operatingKeeperSnapshot(1380, true) // 23:00 — off shift but committed to stay open
}

// tradeConductKeeperSnapshot builds the LLM-413 concession-gate fixtures: a
// keeper on shift inside his own post, alone, with the caller supplying the
// policy/recipe/price-book shape that feeds keeperTradeSlow. Inventory sits at
// cap so no "## Restocking" pull fires, and there is no company → no offer /
// trade-value cues; fixed clock → byte-stable. What varies across the three
// scenarios is exactly the concession judgment's input: what the keeper sold
// this past week, and in what natural unit (single units for a reseller's
// wares, the batch for a producer's).
func tradeConductKeeperSnapshot(keeper *sim.ActorSnapshot, keeperID sim.ActorID, post sim.StructureID, postLabel string, mutate func(*sim.Snapshot)) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const home = sim.StructureID("keeper_house")
	start, end := 360, 1080 // 06:00–18:00
	now := 720              // 12:00 — on shift, at the post
	published := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	keeper.State = sim.StateIdle
	keeper.WorkStructureID = post
	keeper.InsideStructureID = post
	keeper.HomeStructureID = home
	keeper.ScheduleStartMin = &start
	keeper.ScheduleEndMin = &end
	keeper.BusinessownerState = &sim.BusinessownerState{}
	keeper.Needs = map[sim.NeedKey]int{}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{keeperID: keeper},
		Structures: map[sim.StructureID]*sim.Structure{
			post: plainStructure(post, postLabel),
			home: plainStructure(home, "Keeper House"),
		},
		RestockReorderPct: 25,
	}
	mutate(snap)
	return snap, keeperID, nil
}

// resellerAtPostBriskTrade: a reseller (buy-sourced stock, no value-add — the
// Josiah economics) whose shop moved goods this past week: two cheese sales
// inside the window → movement is brisk against a resold ware's single-unit
// quantum → keeperTradeSlow false. The golden pins that the "How you trade:"
// block carries the margin floor but NO concession line — the licence to
// discount is withheld while trade moves (LLM-413).
func resellerAtPostBriskTrade() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		store    = sim.StructureID("general_store")
	)
	josiah := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Josiah Thorne",
		Role:        "shopkeeper",
		Coins:       14,
		Inventory:   map[sim.ItemKind]int{"cheese": 10},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cheese", Source: sim.RestockSourceBuy, Max: 10},
		}},
	}
	return tradeConductKeeperSnapshot(josiah, josiahID, store, "General Store", func(snap *sim.Snapshot) {
		sales := sim.NewRingBuffer[sim.PriceObservation](8)
		sales.Push(sim.PriceObservation{BuyerID: "cust1", Amount: 3, Qty: 1, Consumers: 1, At: snap.PublishedAt.Add(-3 * 24 * time.Hour)})
		sales.Push(sim.PriceObservation{BuyerID: "cust2", Amount: 3, Qty: 1, Consumers: 1, At: snap.PublishedAt.Add(-1 * 24 * time.Hour)})
		snap.ItemKinds = map[sim.ItemKind]*sim.ItemKindDef{
			"cheese": {Name: "cheese", Capabilities: []string{"portable"}, DisplayLabel: "cheese", DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese", Category: sim.ItemCategoryFood},
		}
		snap.PriceBook = map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "josiah", Item: "cheese"}: sales,
		}
	})
}

// resellerAtPostSlowTrade: the same reseller, but every sale on his book is
// OLDER than the weekly window (a dead week — the ring remembers the trade,
// the window doesn't) → keeperTradeSlow true. The golden pins the concession
// line rendering as a felt fact about the slow week, WITH the margin floor
// still standing beside it and restated inside it (LLM-413).
func resellerAtPostSlowTrade() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		store    = sim.StructureID("general_store")
	)
	josiah := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Josiah Thorne",
		Role:        "shopkeeper",
		Coins:       14,
		Inventory:   map[sim.ItemKind]int{"cheese": 10},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "cheese", Source: sim.RestockSourceBuy, Max: 10},
		}},
	}
	return tradeConductKeeperSnapshot(josiah, josiahID, store, "General Store", func(snap *sim.Snapshot) {
		sales := sim.NewRingBuffer[sim.PriceObservation](8)
		sales.Push(sim.PriceObservation{BuyerID: "cust1", Amount: 3, Qty: 1, Consumers: 1, At: snap.PublishedAt.Add(-10 * 24 * time.Hour)})
		sales.Push(sim.PriceObservation{BuyerID: "cust2", Amount: 3, Qty: 1, Consumers: 1, At: snap.PublishedAt.Add(-8 * 24 * time.Hour)})
		snap.ItemKinds = map[sim.ItemKind]*sim.ItemKindDef{
			"cheese": {Name: "cheese", Capabilities: []string{"portable"}, DisplayLabel: "cheese", DisplayLabelSingular: "wedge of cheese", DisplayLabelPlural: "wedges of cheese", Category: sim.ItemCategoryFood},
		}
		snap.PriceBook = map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "josiah", Item: "cheese"}: sales,
		}
	})
}

// producerAtPostSubBatchSales: a producer (recipe-backed smith) who sold 3
// nails this past week against a batch of 10 — movement that would read brisk
// for a resold ware reads SLOW against the producer's own batch quantum (the
// same unit the "## Your trade" scene narrates from, so the two cues agree:
// "only a few sold this past week" beside "Trade has been thin this week").
// The golden pins the batch-quantum leg of keeperTradeSlow: concession line
// present, margin floor present, forge scene consistent (LLM-413).
func producerAtPostSubBatchSales() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		forge     = sim.StructureID("blacksmith")
	)
	ezekiel := &sim.ActorSnapshot{
		Kind:        sim.KindNPCShared,
		DisplayName: "Ezekiel Crane",
		Role:        "blacksmith",
		Coins:       9,
		Inventory:   map[sim.ItemKind]int{"nail": 12},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 30},
		}},
	}
	return tradeConductKeeperSnapshot(ezekiel, ezekielID, forge, "Blacksmith", func(snap *sim.Snapshot) {
		sales := sim.NewRingBuffer[sim.PriceObservation](8)
		sales.Push(sim.PriceObservation{BuyerID: "cust1", Amount: 6, Qty: 3, Consumers: 1, At: snap.PublishedAt.Add(-2 * 24 * time.Hour)})
		snap.ItemKinds = map[sim.ItemKind]*sim.ItemKindDef{
			"nail": {Name: "nail", DisplayLabel: "nails", DisplayLabelSingular: "nail", DisplayLabelPlural: "nails", Category: sim.ItemCategoryCraft},
		}
		snap.Recipes = map[sim.ItemKind]*sim.ItemRecipe{
			"nail": {OutputItem: "nail", OutputQty: 10, RateQty: 10, RatePerHours: 2, WholesalePrice: 1, RetailPrice: 2},
		}
		snap.PriceBook = map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: "ezekiel", Item: "nail"}: sales,
		}
	})
}

// brokeWorkerNoEmployerSeeksWork builds the live LLM-160 situation: a broke
// salem-vendor worker (Lewis Walker) idle at home with no employer present. Drives
// the standing seek-work directory + the "go now" coda — see the scenario summary.
func brokeWorkerNoEmployerSeeksWork() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID   = sim.ActorID("lewis")
		residence = sim.StructureID("walker_residence")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Coins:             0,
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	return snap, lewisID, nil
}

// brokeWorkerSeeksWorkSkipsShutBusiness is the LLM-155 companion to
// brokeWorkerNoEmployerSeeksWork: the same broke idle worker, but he carries an
// earned ObservedClosed memory of the Inn (found shut an hour ago, within the 4h
// TTL). The golden pins that the directory DROPS the remembered-shut Inn entirely
// — not annotates it — and lists only the open General Store with its qualitative
// distance + direction. Positions are set so the kept entry renders "a short walk
// east"; the Inn's position is irrelevant since it is dropped.
func brokeWorkerSeeksWorkSkipsShutBusiness() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID   = sim.ActorID("lewis")
		residence = sim.StructureID("walker_residence")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime
	published := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Pos:               sim.WorldToTile(0, 0),
		Coins:             0,
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: inn, Condition: sim.ObservedClosed}: published.Add(-time.Hour),
		}),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Pos: sim.WorldPos{X: 0, Y: 160}, Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Pos: sim.WorldPos{X: 160, Y: 0}, Tags: []string{"business", "shop"}},
		},
	}
	return snap, lewisID, nil
}

// workerWithCoinNoEmployerSeeksWork is the LLM-168 live case: a WORKLESS worker
// (Silence Walker — worker attribute, no work_structure_id) idle at home holding a
// few coins, no employer present. The same fixture as brokeWorkerNoEmployerSeeksWork
// but with coins: under the old broke (Coins==0) gate she got no directory; LLM-168
// re-anchored eligibility on workless, so the standing seek-work directory + "go now"
// coda fire whether or not she holds coin.
func workerWithCoinNoEmployerSeeksWork() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		residence = sim.StructureID("walker_residence")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Coins:             15, // holds coin, but workless → still directed to seek work
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	return snap, silenceID, nil
}

// seekingWorkerAtHomeGetsNoBakeCue is the LLM-459 live case: the same workless,
// below-ceiling Silence Walker as workerWithCoinNoEmployerSeeksWork, but settled at
// home during the day carrying flour, with the household's bake already going at the
// hearth — every buildBakeChoice precondition met. Pre-fix she got BOTH the bake
// invitation ("Call bake to join in") and the seek-work directory + "call move_to now"
// coda, and the imperative coda won: live 2026-07-18 she walked home -> Tavern ->
// Blueberry Bush -> home while her three over-the-ceiling housemates baked. The golden
// pins that the seek-work half renders ALONE — bake is for the homebodies who have no
// reason to seek work, so an engaged job-seeker is not offered it.
func seekingWorkerAtHomeGetsNoBakeCue() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		residence = sim.StructureID("walker_residence")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime, well clear of the 30m-before-dusk bake cutoff
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Coins:             8, // below the default seek-work ceiling (25) → still seeking
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{sim.BakeFlourItem: 4},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		// Dawn/dusk so the unscheduled worker's day-active window resolves and the
		// bake gate's before-dusk arm is genuinely satisfied — without these the
		// scenario would prove nothing (buildBakeChoice would return nil on the
		// zero-valued DuskMinute regardless of the fix).
		DawnMinute:       420,  // 07:00
		DuskMinute:       1140, // 19:00
		DawnDuskMinuteOK: true,
		// The household bake is already going here, so this is the JOIN arm — the
		// cheapest, most-tempting form of the cue, and the one live Silence saw.
		HomeBakesActive: map[sim.StructureID]bool{residence: true},
		NeedThresholds:  sim.NeedThresholds{},
		Actors:          map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	return snap, silenceID, nil
}

// comfortableHomebodyBakes is the positive half of the LLM-459 pair and the matrix's
// first bake scenario at all — LLM-454 shipped without one, which left the bake cue
// with no render-path coverage and would have made the LLM-459 corpus invariant
// vacuous on its bake side. Identical to seekingWorkerAtHomeGetsNoBakeCue except the
// purse: 40 coins is at/above the default seek-work ceiling (25), so this worker is
// "comfortable", draws no seek-work directory, and is exactly the homebody population
// LLM-454 wrote bake for. Live counterpart: Patience/Lewis/Anne Walker (36/28/37
// coins), who baked normally on 2026-07-18 while their 8-coin housemate did laps.
func comfortableHomebodyBakes() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := seekingWorkerAtHomeGetsNoBakeCue()
	snap.Actors[actorID].DisplayName = "Anne Walker"
	snap.Actors[actorID].Coins = 40 // at/above the default ceiling → comfortable, no seek-work
	return snap, actorID, warrants
}

// seekingWorkerHomeSceneHidesHousemateBake is the LLM-562 live case: the same
// seek-work-engaged Walker as seekingWorkerAtHomeGetsNoBakeCue, but with a housemate
// co-present MID the bake — the shape LLM-459's fixture never carried, because its
// household bake was active only as a flag (HomeBakesActive), with nobody in the room
// doing it. Live 2026-07-29 that difference was the whole bug: Patience Walker's bake
// cue and tool were correctly stripped, but Lewis rendered as "(at the hearth, baking
// just now)" beside her flour inventory, and she narrated joining the bread on nine
// straight home-arrival ticks under a coda that only offered move_to — 179 LLM calls
// in a day, the village's highest. The golden pins the housemate PRESENT and named
// (greetable, addressable) with no bake annotation; the seek-work directory and
// go-coda render as before.
func seekingWorkerHomeSceneHidesHousemateBake() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := seekingWorkerAtHomeGetsNoBakeCue()
	const lewisID = sim.ActorID("lewis")
	residence := snap.Actors[actorID].HomeStructureID
	snap.Actors[actorID].ColocatedAudienceIDs = []sim.ActorID{lewisID}
	snap.Actors[actorID].Acquaintances = map[string]sim.Acquaintance{"Lewis Walker": {}}
	snap.Actors[lewisID] = &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		// Mid the household bake; the home structure id is the source-activity
		// object (shared under the WORK-342 id bridge).
		SourceActivityKind:     sim.SourceActivityBake,
		SourceActivityObjectID: sim.VillageObjectID(residence),
	}
	return snap, actorID, warrants
}

// comfortableHomebodySeesHousemateBake is the positive half of the LLM-562 pair:
// identical to seekingWorkerHomeSceneHidesHousemateBake except the purse — 40 coins
// is at/above the seek-work ceiling, so no directory engages and the bake annotation
// (and the join-in cue) render normally. Flipping only coins across the ceiling
// toggles the annotation, proving the suppression keys on the seek-work directive and
// not some other fixture difference. Also the vacuity floor for
// TestNoSeekWorkPromptDepictsHousemateBake: without a scenario that renders the
// annotation, that invariant would pass having checked nothing.
func comfortableHomebodySeesHousemateBake() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := seekingWorkerHomeSceneHidesHousemateBake()
	snap.Actors[actorID].DisplayName = "Anne Walker"
	snap.Actors[actorID].Coins = 40 // at/above the default ceiling → comfortable, no seek-work
	return snap, actorID, warrants
}

// homebodyMidBake is the LLM-464 case: the SAME comfortable homebody as
// comfortableHomebodyBakes, but already at the hearth with her bake window live, so
// the golden pins the mid-activity triage coda instead of the invitation (the bake
// cue correctly drops — actorMidSourceActivity — since she's doing it already).
// The coda's completion horizon is the whole point. A bake runs until dusk, but the
// LLM-69 coda promised every source activity would finish "shortly" — true for the
// seconds-long kinds it was written for, a five-hour lie for a bake. Live 2026-07-18
// the engine told Anne Walker at midday her batch would finish shortly and she
// relayed it to the household as "the loaves are nearly ready — just a few more
// minutes by the hearth", which kept the whole Walker house asking after bread that
// was hours out. The coda now names the same horizon the bake cue does: by dusk.
func homebodyMidBake() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := comfortableHomebodyBakes()
	subject := snap.Actors[actorID]
	subject.SourceActivityKind = sim.SourceActivityBake
	// Structures share ids with village_objects, so the home resolves the source
	// label the same way a dwell pin does → "baking bread at Walker Residence".
	subject.SourceActivityObjectID = sim.VillageObjectID("walker_residence")
	return snap, actorID, warrants
}

// hungryHomebodyJoinsBake is the LLM-465 case: the comfortable_homebody_bakes homebody,
// red on hunger, at a home where a batch is already going. The pre-fix gate ran the
// red-need check above the join resolution, so it refused him the bake outright — no
// cue, no tool, no shelve — and left him loose and fully tickable in his own kitchen.
// Live 2026-07-18 that was Lewis Walker: 24 turns in 70 minutes asking Anne and Patience
// how the loaves were coming, each question arming bakeReplyDue for BOTH bakers, making
// him the pump on the whole turboyapping loop. The guard is right for STARTING (an
// afternoon's commitment does not outrank a pressing need) and wrong for JOINING, which
// costs no flour, mints no batch, and leaves the need actionable: bakingMayMove keeps
// move_to for a red hunger/thirst and the reactor ticks him through the shelve for it.
// The golden pins the join invitation rendering ALONGSIDE the hunger imperative — both
// are true at once, and he is free to act on either.
func hungryHomebodyJoinsBake() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := comfortableHomebodyBakes()
	subject := snap.Actors[actorID]
	subject.DisplayName = "Lewis Walker"
	// Red on hunger at the default threshold (18) — the live line Lewis was over.
	subject.Needs = map[sim.NeedKey]int{"hunger": sim.DefaultHungerRedThreshold}
	// He carries no flour: the join arm is the whole point, and flour would prove
	// nothing about it (a resident with flour joins an existing batch either way).
	subject.Inventory = map[sim.ItemKind]int{}
	return snap, actorID, warrants
}

// comfortableWorkerNoSeekWork is the LLM-194 case: the SAME workless worker as
// workerWithCoinNoEmployerSeeksWork, but holding coin AT/ABOVE the seek-work ceiling
// (40 >= the default 25). The snapshot is built directly, so SeekWorkCoinCeiling is 0
// and subjectIsComfortable resolves it to the default — the worker reads as comfortable,
// so the golden pins that it gets NEITHER the businesses directory NOR the "call move_to
// now" go-coda: a coin-rich worker stops hustling and is left to idle/consume. The
// negative counterpart of worker_with_coin_no_employer_seeks_work (same actor, 15 coins,
// still seeks).
func comfortableWorkerNoSeekWork() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		residence = sim.StructureID("walker_residence")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Coins:             40, // at/above the default seek-work ceiling (25) → comfortable
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	return snap, silenceID, nil
}

// comfortableWorkerAtEase is the LLM-352 companion to comfortableWorkerNoSeekWork: the
// SAME comfortable (coin-rich, workless) Walker, standing out in the village in the
// daytime and carrying the at-ease warrant the seek-work backstop now stamps for it in
// place of leaving it to freeze. The golden pins both halves of the comfortable-worker
// picture — the "the day is your own" leisure line renders, AND there is still NO
// seek-work directory / solicit affordance (LLM-194's suppression holds). A regression
// that dropped the at-ease arm loses the line and re-strands the worker; one that
// re-added the seek-work cue for a comfortable worker would surface the directory here.
func comfortableWorkerAtEase() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID = sim.ActorID("silence")
		residence = sim.StructureID("walker_residence")
		tavern    = sim.StructureID("tavern")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime, day-active on the dawn/dusk window
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		State:             sim.StateIdle,
		InsideStructureID: "", // out in the village, NOT settled at home
		HomeStructureID:   residence,
		Coins:             40, // at/above the default seek-work ceiling (25) → comfortable
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			tavern:    plainStructure(tavern, "the Tavern"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(tavern): {ID: sim.VillageObjectID(tavern), Tags: []string{"business", "tavern"}},
			sim.VillageObjectID(store):  {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	warrants := []sim.WarrantMeta{{TriggerActorID: silenceID, Reason: sim.AtEaseWarrantReason{}}}
	return snap, silenceID, warrants
}

// workerSeeksWorkAfterEmployerDeclines is the LLM-181 live case (Lewis Walker at the
// General Store, hud-8db08741…), reduced to its load-bearing parts: a workless worker
// shares a huddle with a co-present stranger employer (Josiah Thorne) who has already
// declined his labor offer. The declined ledger entry is what flips
// hasSolicitableAudience to false, so SeekWorkPlaces populates and the seek-work
// off-ramp ("call move_to now") arms even though an employer is physically present —
// the fix that frees the worker from re-soliciting the same refusal. No needs, the
// offer is terminal (no clock-bound content) → byte-stable.
func workerSeeksWorkAfterEmployerDeclines() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID   = sim.ActorID("lewis")
		josiahID  = sim.ActorID("josiah")
		residence = sim.StructureID("walker_residence")
		thorne    = sim.StructureID("thorne_house")
		commons   = sim.StructureID("commons")
		inn       = sim.StructureID("inn")
		store     = sim.StructureID("general_store")
		huddle    = sim.HuddleID("h1")
	)
	now := 540 // 09:00 — daytime, on shift
	published := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		Role:              "laborer",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		HomeStructureID:   residence,
		CurrentHuddleID:   huddle,
		Coins:             0,
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Josiah Thorne": {}},
	}
	// Josiah is a structural stranger to Lewis (different home; Lewis is workless so
	// they never share a workplace) — solicitable by anchor, excluded only by the decline.
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		HomeStructureID:   thorne,
		WorkStructureID:   store,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Lewis Walker": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			commons:   plainStructure(commons, "Village Commons"),
			residence: plainStructure(residence, "Walker Residence"),
			thorne:    plainStructure(thorne, "Thorne House"),
			inn:       plainStructure(inn, "Inn"),
			store:     plainStructure(store, "General Store"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{lewisID: {}, josiahID: {}}},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(inn):   {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
		LaborLedger: map[sim.LaborID]*sim.LaborOffer{
			1: {
				ID:          1,
				WorkerID:    lewisID,
				EmployerID:  josiahID,
				Reward:      10,
				DurationMin: 60,
				State:       sim.LaborStateDeclined,
				HuddleID:    huddle,
			},
		},
	}
	return snap, lewisID, nil
}

// workerSolicitsGoodsRichCoinPoorEmployer is the LLM-243 reduction: a workless
// worker (Silence Walker) shares a huddle with a co-present stranger employer
// (Prudence Ward) who holds 0 coins but goods on hand. Post-fix, a bad coin ask
// against her mints no offer and records no decline (the barter branch), so the
// ledger is EMPTY — she stays a solicitable prospect. The subject is the worker;
// the golden must show the solicit_work affordance for Prudence and NOT the
// SeekWorkPlaces businesses directory or the "No one here can hire you" dead-end.
// Coins=15 for Silence is below the seek-work ceiling (25), so the directory WOULD
// arm if Prudence were foreclosed — the suppression is earned by her solicitability,
// not by the worker being comfortable (non-vacuous).
func workerSolicitsGoodsRichCoinPoorEmployer() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		silenceID  = sim.ActorID("silence")
		prudenceID = sim.ActorID("prudence")
		residence  = sim.StructureID("walker_residence")
		wardHome   = sim.StructureID("ward_house")
		apothecary = sim.StructureID("pw_apothecary")
		commons    = sim.StructureID("commons")
		inn        = sim.StructureID("inn")
		huddle     = sim.HuddleID("h1")
	)
	now := 540 // 09:00 — daytime, on shift
	published := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Silence Walker",
		Role:              "peddler",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		HomeStructureID:   residence,
		CurrentHuddleID:   huddle,
		Coins:             15,
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Prudence Ward": {}},
	}
	// Prudence is a structural stranger to Silence (different home; Silence is
	// workless so they never share a workplace) and has NO declined offer against
	// her — solicitable by anchor. Her empty purse must not change that: she holds
	// berries and tea and can hire in kind (LLM-225).
	prudence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Prudence Ward",
		Role:              "herbalist",
		State:             sim.StateIdle,
		InsideStructureID: commons,
		HomeStructureID:   wardHome,
		WorkStructureID:   apothecary,
		CurrentHuddleID:   huddle,
		Coins:             0,
		Inventory:         map[sim.ItemKind]int{"blueberry": 4, "coca_tea": 9, "raspberry": 14},
		Needs:             map[sim.NeedKey]int{},
		Acquaintances:     map[string]sim.Acquaintance{"Silence Walker": {}},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{silenceID: silence, prudenceID: prudence},
		Structures: map[sim.StructureID]*sim.Structure{
			commons:    plainStructure(commons, "Village Commons"),
			residence:  plainStructure(residence, "Walker Residence"),
			wardHome:   plainStructure(wardHome, "Ward House"),
			apothecary: plainStructure(apothecary, "PW Apothecary"),
			inn:        plainStructure(inn, "Inn"),
		},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{silenceID: {}, prudenceID: {}}},
		},
		// Businesses exist and would populate SeekWorkPlaces if the directory armed —
		// so the absence of the dead-end is because Prudence stays solicitable.
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(apothecary): {ID: sim.VillageObjectID(apothecary), Tags: []string{"business", "shop"}},
			sim.VillageObjectID(inn):        {ID: sim.VillageObjectID(inn), Tags: []string{"business", "lodging"}},
		},
	}
	return snap, silenceID, nil
}

// workerSolicitsUnacquaintedEmployerUnnamedCue is LLM-564's fallback-branch
// scenario: workerSolicitsGoodsRichCoinPoorEmployer with the subject's
// Acquaintances emptied, so Prudence stays solicitable (the gate carries no
// acquaintance clause) but must not be named by the cue.
func workerSolicitsUnacquaintedEmployerUnnamedCue() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, silenceID, warrants := workerSolicitsGoodsRichCoinPoorEmployer()
	snap.Actors[silenceID].Acquaintances = nil
	return snap, silenceID, warrants
}

// TestGoldensSolicitCueNeverNamesSomeone is the LLM-564 corpus invariant: the
// solicit cue's NAMED branch must never render with nameOf's "someone" fallback
// in the employer slot ("someone might have work that wants doing…"). That
// wording can only appear when buildSolicitableEmployers admitted an actor that
// buildWarrantActorNames could not resolve — the two disagreeing about
// acquaintance — and it hands the model a target string solicit_work must
// refuse. The unnamed branch is the correct rendering for that situation.
func TestGoldensSolicitCueNeverNamesSomeone(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if strings.Contains(renderScenario(sc), "someone might have work that wants doing") {
				t.Errorf("scenario %q: the solicit cue rendered its named branch with the %q fallback — "+
					"buildSolicitableEmployers admitted an actor buildWarrantActorNames could not name (LLM-564)",
					sc.name, "someone")
			}
		})
	}
}

// workerSeeksWorkSkipsNoHiringBusiness is the LLM-210 companion to
// brokeWorkerSeeksWorkSkipsShutBusiness: the same workless idle worker (Lewis Walker),
// but he last found the General Store's keeper on a break — an earned ObservedNoHiring
// memory within its 2h TTL — where the keeper was PRESENT (so the store is NOT
// remembered shut) yet could not take him on. The seek-work directory drops the
// no-hiring store and lists only the open Blacksmith, steering him to a business with
// an available keeper instead of looping back to the resting one.
func workerSeeksWorkSkipsNoHiringBusiness() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID    = sim.ActorID("lewis")
		residence  = sim.StructureID("walker_residence")
		store      = sim.StructureID("general_store")
		blacksmith = sim.StructureID("blacksmith")
	)
	now := 540 // 09:00 — daytime
	published := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Pos:               sim.WorldToTile(0, 0),
		Coins:             0,
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: store, Condition: sim.ObservedNoHiring}: published.Add(-30 * time.Minute),
		}),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			residence:  plainStructure(residence, "Walker Residence"),
			store:      plainStructure(store, "General Store"),
			blacksmith: plainStructure(blacksmith, "Blacksmith"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store):      {ID: sim.VillageObjectID(store), Pos: sim.WorldPos{X: 160, Y: 0}, Tags: []string{"business", "shop"}},
			sim.VillageObjectID(blacksmith): {ID: sim.VillageObjectID(blacksmith), Pos: sim.WorldPos{X: 0, Y: 160}, Tags: []string{"business", "shop"}},
		},
	}
	return snap, lewisID, nil
}

// workerSeeksWorkRanksVisitedBusinessLast is the LLM-563 case: a workless idle
// worker (Patience Walker) at home, with the NEAREST business (the General Store)
// carrying a fresh visited memory — she called there half an hour ago and nothing
// came of it. The directory must list the farther, untried Blacksmith FIRST and
// the store LAST with the "you called there not long ago" aside: never dropped,
// just outranked, so the go-coda always has a target and the rotation converges
// on doors not yet knocked.
func workerSeeksWorkRanksVisitedBusinessLast() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		patienceID = sim.ActorID("patience")
		residence  = sim.StructureID("walker_residence")
		store      = sim.StructureID("general_store")
		blacksmith = sim.StructureID("blacksmith")
	)
	now := 540 // 09:00 — daytime
	published := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	patience := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Patience Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Pos:               sim.WorldToTile(0, 0),
		Coins:             11, // the live case: under the seek-work ceiling (25)
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{},
		Observed: sim.NewObservedStates(map[sim.ObservedStateKey]time.Time{
			{StructureID: store, Condition: sim.ObservedSeekWorkVisited}: published.Add(-30 * time.Minute),
		}),
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{patienceID: patience},
		Structures: map[sim.StructureID]*sim.Structure{
			residence:  plainStructure(residence, "Walker Residence"),
			store:      plainStructure(store, "General Store"),
			blacksmith: plainStructure(blacksmith, "Blacksmith"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// The visited store is NEARER than the untried blacksmith, so plain
			// nearest-first would put it on top — the ordering under test.
			sim.VillageObjectID(store):      {ID: sim.VillageObjectID(store), Pos: sim.WorldPos{X: 160, Y: 0}, Tags: []string{"business", "shop"}},
			sim.VillageObjectID(blacksmith): {ID: sim.VillageObjectID(blacksmith), Pos: sim.WorldPos{X: 0, Y: 480}, Tags: []string{"business", "shop"}},
		},
	}
	return snap, patienceID, nil
}

// redTiredWorkerNoSeekWork is the LLM-210 case: a WORKLESS worker (Lewis Walker) idle at
// home holding a few coins (15, below the seek-work ceiling → not comfortable) but at RED
// tiredness (20 >= the default red-line 16). A red need outranks job-hunting, so both
// seek-work gates suppress — the businesses directory and the "call move_to now" go-coda
// are gone and the weariness cue is left to win. The rested counterpart is
// worker_with_coin_no_employer_seeks_work (same workless coin-holder, not red → still seeks).
func redTiredWorkerNoSeekWork() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID   = sim.ActorID("lewis")
		residence = sim.StructureID("walker_residence")
		store     = sim.StructureID("general_store")
	)
	now := 540 // 09:00 — daytime
	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		InsideStructureID: residence,
		HomeStructureID:   residence,
		Coins:             15, // below the seek-work ceiling (25) → not comfortable
		AttributeSlugs:    []string{sim.AttrWorker},
		Needs:             map[sim.NeedKey]int{"tiredness": 20}, // red: >= the default red-line (16)
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{lewisID: lewis},
		Structures: map[sim.StructureID]*sim.Structure{
			residence: plainStructure(residence, "Walker Residence"),
			store:     plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {ID: sim.VillageObjectID(store), Tags: []string{"business", "shop"}},
		},
	}
	return snap, lewisID, nil
}

// TestSeekWorkDirectiveOnlyForWorklessWorker is the LLM-160/155/168 cross-scenario
// invariant: the decisive "call move_to now" go-coda appears in EXACTLY the
// workless-worker-no-employer scenarios and nowhere else in the matrix. A regression
// that re-gated the directory on a warrant, that restored the Coins==0 gate (dropping
// the coin-holding worker_with_coin scenario), or that let another scenario trip the
// workless-worker-with-no-employer condition, would flip a cell here.
func TestSeekWorkDirectiveOnlyForWorklessWorker(t *testing.T) {
	const marker = "call move_to now"
	seekWorkScenarios := map[string]bool{
		"broke_worker_no_employer_seeks_work":         true,
		"broke_worker_seeks_work_skips_shut_business": true,
		"worker_with_coin_no_employer_seeks_work":     true,
		"worker_seeks_work_after_employer_declines":   true,
		"worker_seeks_work_skips_no_hiring_business":  true,
		// LLM-563: a visited business is deprioritised, never dropped — the
		// directory stays populated, so the go-coda renders here too.
		"worker_seeks_work_ranks_visited_business_last": true,
		// LLM-459: a workless below-ceiling worker settled at home with the household
		// bake going. It belongs here — the seek-work half is exactly what SHOULD
		// survive; the point of the scenario is that the bake invitation does not
		// render beside it (TestNoPromptOffersBakeAndSeekWorkTogether).
		"seeking_worker_at_home_gets_no_bake_cue": true,
		// LLM-562: the same seek-work-engaged worker with a housemate co-present
		// mid-bake. The go-coda SHOULD survive — the scenario's point is that the
		// housemate's bake annotation does not render beside it
		// (TestNoSeekWorkPromptDepictsHousemateBake).
		"seeking_worker_home_scene_hides_housemate_bake": true,
	}
	for _, sc := range perceptionScenarios {
		want := seekWorkScenarios[sc.name]
		got := strings.Contains(renderScenario(sc), marker)
		if got != want {
			t.Errorf("scenario %q: seek-work go-coda present = %v, want %v", sc.name, got, want)
		}
	}
}

// TestSeekWorkSuppressedByRedNeed is the LLM-210 cross-scenario invariant: a red need
// outranks job-hunting, so the SAME workless worker gets the businesses directory when
// rested but NOT when red-tired. Flipping only tiredness across the red-line toggles the
// directory, proving the gate is the need itself and not some other fixture difference. A
// regression that dropped the hasRedNeed gate would leave the directory present in both.
func TestSeekWorkSuppressedByRedNeed(t *testing.T) {
	const directoryMarker = "offer your labor"
	render := func(tiredness int) string {
		return renderScenario(perceptionScenario{
			name: "redneed_flip",
			build: func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
				snap, id, warrants := redTiredWorkerNoSeekWork()
				snap.Actors[id].Needs["tiredness"] = tiredness
				return snap, id, warrants
			},
		})
	}
	if strings.Contains(render(20), directoryMarker) {
		t.Errorf("red-tired workless worker: seek-work directory present, want absent")
	}
	if !strings.Contains(render(0), directoryMarker) {
		t.Errorf("rested workless worker: seek-work directory absent, want present")
	}
}

// terminalToolNames are the tools whose success ENDS the tick
// (TerminalPolicy=TerminalOnSuccess in the handlers registry). Duplicated here
// as a literal because perception cannot import handlers — handlers imports
// perception. handlers.TestTerminalToolsMatchPerceptionInvariantList pins this
// list against the real registry, so adding a terminal tool without updating
// this slice fails there with a pointer back to this test. LLM-350.
var terminalToolNames = []string{
	"accept_pay", "accept_work", "bake", "counter_pay", "decline_pay", "decline_work",
	"gather", "move_to", "offer_trade", "offer_work", "pay_with_item", "repair",
	"sell", "solicit_work", "speak", "stoke", "stop", "summon", "turn_in",
	"withdraw_pay",
}

// affirmativeSpeakRe matches an instruction to CALL the speak tool: "use speak",
// "call speak", "then speak", "also speak". It deliberately does NOT match speak
// used as an ordinary verb ("the words you speak aloud in say"), nor speak named
// inside a prohibition ("Do not name a price with the speak tool") — those are the
// two ways a legitimate cue mentions it.
var affirmativeSpeakRe = regexp.MustCompile(`(?i)\b(use|call)\s+(the\s+)?speak\b|\b(then|also)\s+speak\b`)

// negationRe matches a prohibition marker.
var negationRe = regexp.MustCompile(`(?i)\b(do not|don't|never)\b`)

// clauseSplitRe splits a cue line into clauses on sentence/clause punctuation.
//
// Scoping the negation check to the clause carrying the speak instruction — rather
// than to the whole line — is what makes the invariant sound. A line reading "Do
// not delay; call accept_pay, then use speak." carries a negation AND a live
// two-terminal-verb instruction; a line-wide negation check waves it through, and
// so does a proximity window, because the negation sits within a few words of the
// speak (code_review).
var clauseSplitRe = regexp.MustCompile(`[.;:]`)

// instructsSpeakAlongside reports whether line tells the model to CALL speak from a
// clause that does not forbid it.
func instructsSpeakAlongside(line string) bool {
	for _, clause := range clauseSplitRe.Split(line, -1) {
		if affirmativeSpeakRe.MatchString(clause) && !negationRe.MatchString(clause) {
			return true
		}
	}
	return false
}

// TestGoldensNoCueInstructsTwoTerminalVerbs is the LLM-350 cross-scenario
// invariant, generalized from the LLM-343 seller-only version it replaces.
//
// Every tool in terminalToolNames ends the tick on success, speak among them
// (LLM-321). So a cue that instructs the model to call speak AND some other
// terminal tool can never be obeyed: whichever call lands first ends the tick and
// the harness skips the rest of the batch as post_terminal. Both orderings lose
// something. Speak-then-act loses the act — the offer goes unanswered, the sale
// expires. Act-then-speak loses the words — the village transacts in silence.
//
// The rule: on any single rendered line that names another terminal verb, no
// clause may instruct a call to speak unless that same clause forbids it. Line
// granularity is the right unit for finding the cue, because each cue writes one
// \n-terminated line and the two-verb instructions this catches ("Respond with
// accept_pay… Then also use speak") split their clauses across sentences within
// one line; clause granularity is the right unit for judging it, so a negation
// elsewhere on the line cannot launder a live instruction.
//
// Not a lint: it renders the whole matrix, so it fires on the cue as an NPC
// actually receives it. accept_gift / decline_gift deliberately pair with a
// following speak and are NOT in the list — they are non-terminal, so that cue is
// followable, and the invariant must not drag them in.
func TestGoldensNoCueInstructsTwoTerminalVerbs(t *testing.T) {
	var checked int
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			for _, line := range strings.Split(renderScenario(sc), "\n") {
				named := namedTerminalTools(line)
				if len(named) < 2 || !slices.Contains(named, "speak") {
					continue
				}
				checked++
				if instructsSpeakAlongside(line) {
					t.Errorf("scenario %q: cue instructs a call to the terminal speak tool alongside %v "+
						"— whichever lands first ends the tick and the other is skipped as "+
						"post_terminal (LLM-350). Fold the utterance into the tool's `say` argument "+
						"instead.\n    %s",
						sc.name, without(named, "speak"), line)
				}
			}
		})
	}
	// Guard against the invariant going vacuous — a tool rename would otherwise
	// make every line stop matching and the test would pass having asserted
	// nothing. The matrix renders the sell, offer_work, pay_with_item, pay-response
	// and labor-response cues, so this floor is comfortably met today.
	if checked == 0 {
		t.Fatal("invariant matched no lines at all — terminalToolNames is probably stale (LLM-350)")
	}
}

// namedTerminalTools returns the terminal tools named as whole words on line.
func namedTerminalTools(line string) []string {
	var out []string
	for _, name := range terminalToolNames {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(line) {
			out = append(out, name)
		}
	}
	return out
}

func without(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}

// TestGoldensTransactionCuesRouteWordsThroughSay is the positive half of the
// invariant above: it is not enough that a cue stops asking for a second speak —
// the words have to go somewhere, or the fix would just have muted the village.
//
// Each entry pins one cue to the `say` argument that now carries its utterance,
// and to the banned phrasing it used to carry. Keyed off the cue's BODY, never
// its header alone, so a future section reusing a title isn't dragged in.
func TestGoldensTransactionCuesRouteWordsThroughSay(t *testing.T) {
	cues := []struct {
		name    string
		present string // renders only when the cue fired
		wantSay string // the phrase routing words into the tool's `say`
		banned  []string
	}{
		{
			name:    "seller (sell, LLM-343)",
			present: "Your goods to sell:",
			wantSay: "the words you speak aloud in say",
			banned:  []string{"say your price for it plainly in your reply", "and call sell with that named item"},
		},
		{
			name:    "pay response (accept_pay/decline_pay/counter_pay, LLM-350)",
			present: "## Offers awaiting your decision",
			wantSay: "passing the offer id as ledger_id and the words you speak aloud in say",
			banned:  []string{"Then also use speak", "because the pay response itself passes in silence"},
		},
		{
			name:    "labor response (accept_work/decline_work, LLM-350)",
			present: "passing the offer id as labor_id",
			wantSay: "the words you speak aloud in say",
			banned:  []string{"Then also use speak", "because the work response itself passes in silence"},
		},
		{
			name:    "buyer offer (pay_with_item, LLM-350)",
			present: "Buy it now — call pay_with_item",
			wantSay: "your handoff line in say",
			banned:  []string{"Then also use speak for a brief handoff line"},
		},
	}
	for _, cue := range cues {
		cue := cue
		t.Run(cue.name, func(t *testing.T) {
			var fired int
			for _, sc := range perceptionScenarios {
				out := renderScenario(sc)
				if !strings.Contains(out, cue.present) {
					continue // cue N/A in this scenario
				}
				fired++
				if !strings.Contains(out, cue.wantSay) {
					t.Errorf("scenario %q: cue does not route its words through `say` (want %q)", sc.name, cue.wantSay)
				}
				for _, banned := range cue.banned {
					if strings.Contains(out, banned) {
						t.Errorf("scenario %q: cue reintroduces the two-terminal-verb instruction %q", sc.name, banned)
					}
				}
			}
			if fired == 0 {
				t.Errorf("no scenario rendered this cue (marker %q) — the assertion is vacuous", cue.present)
			}
		})
	}
}

// TestInstructsSpeakAlongside pins the detector behind the LLM-350 invariant. It
// is the piece most likely to rot into a rubber stamp, so its two failure modes
// are pinned directly: waving through a live instruction (false negative) and
// tripping on a cue that only names speak to forbid it (false positive).
func TestInstructsSpeakAlongside(t *testing.T) {
	instructs := []string{
		// The pre-LLM-350 cues, verbatim.
		"Respond first with accept_pay, decline_pay, or counter_pay, passing the offer id as ledger_id. Then also use speak for a brief reply, because the pay response itself passes in silence.",
		"Respond with accept_work or decline_work, passing the offer id as labor_id. Then also use speak for a brief reply, because the work response itself passes in silence.",
		"call decline_work (offer id 1), then use speak to tell them you cannot pay what they ask.",
		"Buy it now — first call pay_with_item with seller \"Anders Brewer\". Then also use speak for a brief handoff line as you make the offer.",
		"Now call speak to say a brief word about it, then call done().",
		// A negation elsewhere on the line must NOT launder a live instruction. This
		// is the case a line-wide check — and a proximity window — both miss.
		"Do not delay; call accept_pay, then use speak.",
		"Never dawdle. Then also use speak.",
	}
	for _, line := range instructs {
		if !instructsSpeakAlongside(line) {
			t.Errorf("missed a live speak instruction:\n    %s", line)
		}
	}

	forbids := []string{
		// The shipped cues. Each names speak only to warn the model off it.
		"Respond with accept_pay, decline_pay, or counter_pay, passing the offer id as ledger_id and the words you speak aloud in say. Do not reply with the speak tool: speaking ends your turn, and the offer would go unanswered.",
		"call sell — the named item and quantity in lines, your price in coins in amount, and the words you speak aloud in say. Do not name a price with the speak tool: speaking ends your turn, and the offer would never be made.",
		"Put what you say to them in offer_work's `say`, in your own voice; do NOT ask with speak first, because speaking ends your turn and the offer would never reach them.",
		"call pay_with_item with seller \"Ezekiel Crane\", and your handoff line in say. Do not speak first: speaking ends your turn, and the offer would never be made.",
		"call decline_work (offer id 1), telling them in say that you cannot pay what they ask.",
		// speak as an ordinary verb, not a tool call.
		"the words you speak aloud in say",
	}
	for _, line := range forbids {
		if instructsSpeakAlongside(line) {
			t.Errorf("false positive on a cue that does not instruct a speak call:\n    %s", line)
		}
	}
}

// lingeringInnConversation is the LLM-397 scene, rebuilt from the live
// 2026-07-14 inn transcript (hud-e26338a9…): Hannah Boggs, Lewis Walker and
// Silence Walker have been talking in the Inn for well over the wind-down
// window. Nothing here is wrong. The porridge was bought, paid for, and eaten;
// the talk is varied and warm; a widow is remembering her husband. The engine's
// only remaining interest is that the scene should be allowed to END, so the
// steer asks for a farewell and nothing else.
//
// The golden's job is to pin exactly that register — a graceful close, NOT the
// endurance arm's "nothing new is coming of it", which on this scene would be a
// flat falsehood (a sale had just closed) and which a weak model answers by
// arguing with the premise instead of saying goodbye. Guarded across the matrix
// by TestGoldensLingeringNeverClaimsFruitlessness.
func lingeringInnConversation() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		lewisID   = sim.ActorID("lewis")
		hannahID  = sim.ActorID("hannah")
		silenceID = sim.ActorID("silence")
		inn       = sim.StructureID("the_inn")
		huddleID  = sim.HuddleID("inn_huddle")
	)
	noon := 750 // 12:30 — the midday chill of the transcript
	published := time.Date(2026, 7, 14, 12, 30, 0, 0, time.UTC)

	lewis := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Lewis Walker",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 10},
		InsideStructureID: inn,
		CurrentHuddleID:   huddleID,
		// The whole point of the arm: not looping, not starved of progress —
		// just long.
		ConversationLingering: true,
		Coins:                 4,
		Needs:                 map[sim.NeedKey]int{},
		Acquaintances: map[string]sim.Acquaintance{
			"Hannah Boggs":   {},
			"Silence Walker": {},
		},
	}
	hannah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 11, Y: 10},
		InsideStructureID: inn,
		CurrentHuddleID:   huddleID,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
	}
	silence := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Silence Walker",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 10, Y: 11},
		InsideStructureID: inn,
		CurrentHuddleID:   huddleID,
		Coins:             6,
		Needs:             map[sim.NeedKey]int{},
	}

	ring := []sim.Utterance{
		{SpeakerID: hannahID, SpeakerName: "Hannah Boggs", At: published.Add(-6 * time.Minute),
			Text: "His name was Thomas. Thomas Boggs. A blacksmith by trade, though he could turn his hand to anything."},
		{SpeakerID: silenceID, SpeakerName: "Silence Walker", At: published.Add(-4 * time.Minute),
			Text: "A man whose memory still warms you after seven winters — that's a life worth remembering."},
		{SpeakerID: hannahID, SpeakerName: "Hannah Boggs", At: published.Add(-2 * time.Minute),
			Text: "Take your ease, both of you. There's no rush to leave a warm bowl and a warm fire."},
	}

	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &noon,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			lewisID: lewis, hannahID: hannah, silenceID: silence,
		},
		Structures: map[sim.StructureID]*sim.Structure{inn: plainStructure(inn, "The Inn")},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddleID: {
				ID:               huddleID,
				Members:          map[sim.ActorID]struct{}{lewisID: {}, hannahID: {}, silenceID: {}},
				RecentUtterances: ring,
			},
		},
	}
	// Hannah has just spoken to him — the reply pressure the wind-down must
	// override rather than obey. Left unsuppressed, this nag is exactly what
	// keeps a finished conversation alive for another beat.
	warrants := []sim.WarrantMeta{{
		TriggerActorID: hannahID,
		SourceEventID:  1,
		OccurredAt:     published.Add(-2 * time.Minute),
		HuddleID:       huddleID,
		Reason: sim.NPCSpeechWarrantReason{
			SpeechID: 1,
			Speaker:  hannahID,
			Excerpt:  "Take your ease, both of you. There's no rush to leave a warm bowl and a warm fire.",
		},
	}}
	return snap, lewisID, warrants
}

// TestGoldensLingeringNeverClaimsFruitlessness is the LLM-397 cross-scenario
// invariant. The lingering steer and the endurance steer both end in "wind this
// down", and the temptation to collapse them into one line will recur — but they
// are not interchangeable: the endurance arm only fires when nothing has come of
// the conversation, while the lingering arm routinely fires on scenes where
// something plainly has. Any prompt that tells an NPC nothing came of a
// conversation that just sold breakfast is lying to it, and the fix for the
// falsehood is not a better threshold — it is a separate line.
func TestGoldensLingeringNeverClaimsFruitlessness(t *testing.T) {
	const fruitless = "nothing new is coming of it"
	got := renderScenario(perceptionScenario{
		name:  "lingering_inn_conversation",
		build: lingeringInnConversation,
	})
	if strings.Contains(got, fruitless) {
		t.Errorf("the lingering steer must never claim the conversation was fruitless — the live scene it fires on had just closed a sale.\nprompt:\n%s", got)
	}
	if !strings.Contains(got, "Let the conversation come to its natural end") {
		t.Errorf("a lingering conversation should be steered to a graceful close.\nprompt:\n%s", got)
	}
}

// dwellPinnedEaterInn rebuilds the LLM-416 Inn scene: the same over-long huddle as
// lingeringInnConversation, but the subject (Lewis) is mid item-dwell — eating a
// bowl of porridge at the Inn, four ticks from finishing, so his dwell self-cue
// tells him leaving now wastes it. `coda` selects which wind-down flag the
// classifier has raised on him ("runlong" or "lingering"); `pinned` toggles the
// dwell so a caller can compare the pinned scene against the identical un-pinned
// control. `subjectID` lets the same scene be rendered from the eater's POV (Lewis,
// Lever A) or an onlooker's (Hannah, Lever B). Fixed clock + timestamps → byte-stable.
func dwellPinnedEaterInn(coda string, pinned bool, subjectID sim.ActorID) func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return func() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
		const (
			lewisID   = sim.ActorID("lewis")
			hannahID  = sim.ActorID("hannah")
			silenceID = sim.ActorID("silence")
			inn       = sim.StructureID("the_inn")
			innPin    = sim.VillageObjectID("the_inn")
			huddleID  = sim.HuddleID("inn_huddle")
		)
		noon := 750 // 12:30
		published := time.Date(2026, 7, 15, 12, 40, 0, 0, time.UTC)
		remaining := 4

		lewis := &sim.ActorSnapshot{
			Kind:              sim.KindNPCStateful,
			DisplayName:       "Lewis Walker",
			State:             sim.StateIdle,
			Pos:               sim.TilePos{X: 10, Y: 10},
			InsideStructureID: inn,
			CurrentHuddleID:   huddleID,
			Coins:             4,
			Needs:             map[sim.NeedKey]int{"hunger": 6},
			Acquaintances: map[string]sim.Acquaintance{
				"Hannah Boggs":   {},
				"Silence Walker": {},
			},
		}
		switch coda {
		case "runlong":
			lewis.ConversationRunLong = true
		case "lingering":
			lewis.ConversationLingering = true
		}
		if pinned {
			// Standing on the Inn pin with an item dwell keyed to it — the
			// co-location gate in buildActiveDwellCredits passes, so the eating
			// self-cue renders and midItemDwell suppresses the leave coda.
			lewis.CurrentLoiterObjectID = innPin
			lewis.DwellCredits = map[sim.DwellCreditKey]*sim.DwellCredit{
				{ObjectID: innPin, Attribute: "hunger", Source: sim.DwellSourceItem}: {
					ObjectID:           innPin,
					Kind:               "porridge",
					Attribute:          "hunger",
					Source:             sim.DwellSourceItem,
					LastCreditedAt:     published.Add(-2 * time.Minute),
					RemainingTicks:     &remaining,
					DwellDelta:         -1,
					DwellPeriodMinutes: 2,
				},
			}
		}

		hannah := &sim.ActorSnapshot{
			Kind:              sim.KindNPCStateful,
			DisplayName:       "Hannah Boggs",
			Role:              "innkeeper",
			State:             sim.StateIdle,
			Pos:               sim.TilePos{X: 11, Y: 10},
			InsideStructureID: inn,
			CurrentHuddleID:   huddleID,
			Coins:             30,
			Needs:             map[sim.NeedKey]int{},
			Acquaintances: map[string]sim.Acquaintance{
				"Lewis Walker":   {},
				"Silence Walker": {},
			},
		}
		silence := &sim.ActorSnapshot{
			Kind:              sim.KindNPCStateful,
			DisplayName:       "Silence Walker",
			State:             sim.StateIdle,
			Pos:               sim.TilePos{X: 10, Y: 11},
			InsideStructureID: inn,
			CurrentHuddleID:   huddleID,
			Coins:             6,
			Needs:             map[sim.NeedKey]int{},
		}

		ring := []sim.Utterance{
			{SpeakerID: hannahID, SpeakerName: "Hannah Boggs", At: published.Add(-2 * time.Minute),
				Text: "Take your ease — there's no rush to leave a warm bowl."},
		}

		snap := &sim.Snapshot{
			PublishedAt:      published,
			LocalMinuteOfDay: &noon,
			NeedThresholds:   sim.NeedThresholds{},
			Assets:           emptyAssetSet,
			Actors: map[sim.ActorID]*sim.ActorSnapshot{
				lewisID: lewis, hannahID: hannah, silenceID: silence,
			},
			Structures: map[sim.StructureID]*sim.Structure{inn: plainStructure(inn, "The Inn")},
			Huddles: map[sim.HuddleID]*sim.Huddle{
				huddleID: {
					ID:               huddleID,
					Members:          map[sim.ActorID]struct{}{lewisID: {}, hannahID: {}, silenceID: {}},
					RecentUtterances: ring,
				},
			},
		}
		// Hannah has just spoken — the reply pressure the wind-down otherwise
		// overrides. Present so the scene matches the live one that motivated the fix.
		warrants := []sim.WarrantMeta{{
			TriggerActorID: hannahID,
			SourceEventID:  1,
			OccurredAt:     published.Add(-2 * time.Minute),
			HuddleID:       huddleID,
			Reason: sim.NPCSpeechWarrantReason{
				SpeechID: 1,
				Speaker:  hannahID,
				Excerpt:  "Take your ease — there's no rush to leave a warm bowl.",
			},
		}}
		return snap, subjectID, warrants
	}
}

// TestGoldenDwellPinnedEaterNoLeaveCoda is the LLM-416 property guard (Lever A) and
// its cross-scenario invariant: a subject mid item-dwell — eating a bought meal it
// would waste by leaving — must NOT be handed a wind-down "leave" coda it cannot
// obey. That contradiction (dwell self-cue "don't leave or you waste it" vs. coda
// "bring it to a close") is what drove the live Inn farewell storm (2026-07-15).
// Checked across the whole wind-down matrix (both run-long and lingering), each
// pinned (coda suppressed) and un-pinned (coda present) so the test proves the
// dwell is what suppresses it, not the scene shape.
func TestGoldenDwellPinnedEaterNoLeaveCoda(t *testing.T) {
	cases := []struct {
		coda      string
		leaveLine string
	}{
		{"runlong", "Bring it to a close"},
		{"lingering", "Let the conversation come to its natural end"},
	}
	for _, tc := range cases {
		t.Run(tc.coda, func(t *testing.T) {
			pinned := renderScenario(perceptionScenario{
				name:  "dwell_eater_" + tc.coda + "_pinned",
				build: dwellPinnedEaterInn(tc.coda, true, "lewis"),
			})
			if !strings.Contains(pinned, "porridge") {
				t.Fatalf("precondition: the pinned eater must perceive its own active meal.\nprompt:\n%s", pinned)
			}
			if strings.Contains(pinned, tc.leaveLine) {
				t.Errorf("a dwell-pinned eater must NOT get the %s leave coda %q — it cannot leave without wasting its meal.\nprompt:\n%s", tc.coda, tc.leaveLine, pinned)
			}
			// Absence of the leave line is not enough — prove the fall-through lands on
			// the benign decision coda, not a leave/close or reply-pressure line. Here
			// the subject owes Hannah a reply (AwaitingReplyFrom empty → AwaitingReply()
			// false), so it deterministically hits the default "choose one action …
			// done()" coda, which permits eating on in silence.
			if !strings.Contains(pinned, "Choose one action, then call done()") {
				t.Errorf("a dwell-pinned eater should fall through to the benign decision coda (a silent done() is permitted), not a leave or reply-pressure line.\nprompt:\n%s", pinned)
			}

			control := renderScenario(perceptionScenario{
				name:  "dwell_eater_" + tc.coda + "_control",
				build: dwellPinnedEaterInn(tc.coda, false, "lewis"),
			})
			if !strings.Contains(control, tc.leaveLine) {
				t.Errorf("control (no dwell): the %s wind-down leave coda %q should render when the subject is NOT pinned — else this guard proves nothing.\nprompt:\n%s", tc.coda, tc.leaveLine, control)
			}
		})
	}
}

// TestGoldenObserverSeesEatingPeer is the LLM-416 Lever B guard: an onlooker huddled
// with a mid-meal peer must see, in "## Around you", that the peer is eating — so a
// proprietor reads a lingering diner as still at their meal rather than as someone
// about to leave (the loop's other half). Rendered from Hannah's POV.
func TestGoldenObserverSeesEatingPeer(t *testing.T) {
	got := renderScenario(perceptionScenario{
		name:  "observer_sees_eating_peer",
		build: dwellPinnedEaterInn("lingering", true, "hannah"),
	})
	if !strings.Contains(got, "Lewis Walker") || !strings.Contains(got, "(eating porridge just now)") {
		t.Errorf("an observer should see a co-present eater annotated as eating in ## Around you.\nprompt:\n%s", got)
	}
}

// turnInCueMarker is the "## The evening draws in" section heading — the single
// rendered marker for the LLM-447 voluntary bed-down affordance. The invariants
// below scan the whole scenario matrix for it.
const turnInCueMarker = "## The evening draws in"

// TestGoldensTurnInCueIffEligible is the LLM-447 cross-scenario invariant: the
// bed-down cue renders IFF the subject could actually turn in — settled inside
// the place it sleeps (its own home, or an inn it holds a room grant on),
// off-shift, awake, and past dusk.
//
// Guarding BOTH directions is the point, because the two failure modes are
// opposite and both bad. A missing cue where the actor is eligible re-opens the
// Long Goodnight — a household with nothing to do but talk. A cue where the actor
// is NOT eligible hands the model a tool the substrate will refuse (sim.TurnIn
// re-checks the same gate), which spends a turn on a rejected call and teaches
// the model the verb doesn't work.
//
// It re-derives eligibility from the snapshot rather than calling
// buildTurnInChoice, so a bug that widened the builder can't launder itself
// through the invariant that is supposed to catch it.
func TestGoldensTurnInCueIffEligible(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, _ := sc.build()
			a := snap.Actors[actorID]
			if a == nil || snap.LocalMinuteOfDay == nil || !snap.DawnDuskMinuteOK {
				return // no clock to reason about — invariant N/A
			}
			if a.Kind != sim.KindNPCStateful && a.Kind != sim.KindNPCShared {
				return // PCs/decoratives have no turn_in surface at all
			}
			eligible := turnInEligibleFromSnapshot(snap, a)
			got := strings.Contains(renderScenario(sc), turnInCueMarker)
			if eligible && !got {
				t.Errorf("scenario %q: subject is settled where it sleeps, off-shift, past dusk — but no %q cue. "+
					"With no way to end the evening the household can only keep talking (LLM-447)", sc.name, turnInCueMarker)
			}
			if !eligible && got {
				t.Errorf("scenario %q: %q cue rendered but the subject can't turn in here — sim.TurnIn would refuse "+
					"the call, wasting the turn (LLM-447 tool-cue lockstep)", sc.name, turnInCueMarker)
			}
		})
	}
}

// turnInEligibleFromSnapshot re-derives the LLM-447 bed-down eligibility straight
// from the snapshot — deliberately NOT by calling buildTurnInChoice, so the
// invariant above is an independent check rather than a tautology.
func turnInEligibleFromSnapshot(snap *sim.Snapshot, a *sim.ActorSnapshot) bool {
	if a.State == sim.StateSleeping || a.InsideStructureID == "" {
		return false
	}
	if a.SourceActivityKind != "" {
		return false // mid-bake / mid-harvest — finish it first
	}
	if !minuteInWindow(snap.DuskMinute, snap.DawnMinute, *snap.LocalMinuteOfDay) {
		return false // not yet evening
	}
	if a.ScheduleStartMin != nil && a.ScheduleEndMin != nil {
		s, e := *a.ScheduleStartMin, *a.ScheduleEndMin
		now := *snap.LocalMinuteOfDay
		onShift := now >= s && now < e
		if s > e {
			onShift = now >= s || now < e
		}
		if onShift {
			return false // on shift — belongs at its post, not its bed
		}
	}
	if a.HomeStructureID != "" && a.InsideStructureID == a.HomeStructureID {
		_, resolves := resolveStructureLabel(snap, a.HomeStructureID)
		return resolves
	}
	// Lodger arm: inside the inn it holds an active grant on.
	for _, ra := range a.RoomAccess {
		if !sim.IsActiveLedgerGrant(ra, snap.PublishedAt) {
			continue
		}
		if s := structureForRoom(snap, ra.RoomID); s != nil && a.InsideStructureID == s.ID {
			return true
		}
	}
	return false
}

// TestGoldensSingleBedtimeCue guards the invariant that survived the LLM-36
// deletion: a prompt carries at most ONE way to be told to go to bed, and it is
// the tool-backed one.
//
// LLM-36's "## Turn in for the night" predated the sleep verb — its own comment
// read "the steer is situational, NOT a tool call" — so it asked a lodger to wind
// down and end its turn for the engine backstop to bed. turn_in does the same
// thing with a real tool over a wider window, so the old section was deleted. Two
// differently-mechanised "go to bed" instructions on one page would leave the
// model guessing which is real, and the one without a tool is the one it cannot
// obey.
//
// Scanning for the retired marker (rather than asserting on the deleted symbol,
// which no longer compiles) is deliberate: it catches a REintroduction by any
// route — a revert, a new cue that reuses the heading, a copy-paste from the
// git history — not just a resurrection of the original function.
func TestGoldensSingleBedtimeCue(t *testing.T) {
	const retiredMarker = "## Turn in for the night"
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if out := renderScenario(sc); strings.Contains(out, retiredMarker) {
				t.Errorf("scenario %q renders %q — the LLM-36 retire section was deleted in favour of the "+
					"tool-backed %q cue; something reintroduced a second bedtime instruction with no tool behind it",
					sc.name, retiredMarker, turnInCueMarker)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// LLM-589 — the working-clothes cue.
//
// The subject is Ezekiel at his forge because he is the motivating case: garment
// wear is charged by worked MINUTE (LLM-422), so the smith wears his shift out
// faster than anyone in the village, and until this cue the only clothing demand
// surface in the engine sat behind `Storm && !Indoors` — he was never once told.
//
// The seller is Josiah at the general store because that is the ONLY shape the
// production world has: clothing is import-only (LLM-410), so nobody produces a
// garment and every seller is a reseller. findItemVendors would refuse a plain
// reseller as a supplier of record (LLM-252) — the cue can name a destination at
// all only because isRestockSupplierOf admits the DISTRIBUTOR for anything he
// stocks. These fixtures therefore give Josiah no RestockPolicy and tag his store
// TagDistributor, matching the live world rather than a producer stand-in.
// ---------------------------------------------------------------------------

// garmentKinds is the clothing slice of the live catalog (LLM-410 labels and
// LLM-589 wear budgets, verbatim). The coat is here to hold the SPLIT in place:
// it carries `warms`, so it belongs to the cold self-line and must never satisfy
// (or be steered toward by) the working-clothes cue.
// clothingCategory is the item_kind.category the LLM-410 garments carry.
// sim.ItemCategory is a free-text soft type with named constants only for the
// engine-known four (food/drink/material/craft), so clothing is spelled out here
// exactly as the migration seeds it.
const clothingCategory = sim.ItemCategory("clothing")

func garmentKinds() map[sim.ItemKind]*sim.ItemKindDef {
	return map[sim.ItemKind]*sim.ItemKindDef{
		"linens": {
			Name: "linens", DisplayLabel: "Linens",
			DisplayLabelSingular: "set of linens", DisplayLabelPlural: "sets of linens",
			Category: clothingCategory, WearMinutes: 10800,
			Description: "Plain linen worn next to the skin, and washed oftener than anything else.",
		},
		"woolens": {
			Name: "woolens", DisplayLabel: "Woolens",
			DisplayLabelSingular: "set of woolens", DisplayLabelPlural: "sets of woolens",
			Category: clothingCategory, WearMinutes: 14400,
			Description: "Stout wool cut for the working day, and mended more than once.",
		},
		"coat": {
			Name: "coat", DisplayLabel: "Coat",
			DisplayLabelSingular: "coat", DisplayLabelPlural: "coats",
			Category: clothingCategory, WearMinutes: 18000,
			Capabilities: []string{string(sim.CapabilityWarms)},
			Description:  "A heavy wool coat, long against the wind and the rain.",
		},
	}
}

// workingClothesSnapshot: Ezekiel works his forge with `wear` worked minutes left
// on the garments he carries, while Josiah keeps the general store — the village
// distributor, and so the only resolvable clothing supplier — a long walk away.
// The business-person fact is load-bearing (LLM-595): inside his shift window,
// Ezekiel is working his day, which is what puts him in sim.actorWearsGarments —
// the set whose garments the wear sweep actually touches — with no ActorState
// beyond idle involved. No orders, no clock read → byte-stable.
func workingClothesSnapshot(inv, wear map[sim.ItemKind]int) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		actorID  = sim.ActorID("ezekiel")
		josiahID = sim.ActorID("josiah")
		store    = sim.StructureID("general_store")
		forge    = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	ezekiel := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Ezekiel Crane",
		Role:              "blacksmith",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 100, Y: 100}.Tile(),
		WorkStructureID:   forge,
		InsideStructureID: forge,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         inv,
		GarmentWear:       wear,
	}
	// No RestockPolicy on Josiah — he PRODUCES nothing, exactly as in the live
	// world. His standing as a supplier comes entirely from the distributor tag on
	// the store below. Placed far from the forge, so he is a supplier of record to
	// walk to rather than a seller already in the huddle.
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 4000, Y: 4000}.Tile(),
		WorkStructureID:   store,
		InsideStructureID: store,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             0,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"linens": 3, "woolens": 2, "coat": 1},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:              &now,
		NeedThresholds:                sim.NeedThresholds{},
		Assets:                        emptyAssetSet,
		GarmentThreadbareFractionX100: sim.DefaultGarmentThreadbareFractionX100,
		ItemKinds:                     garmentKinds(),
		// The LLM-410 price anchors, verbatim: inert recipes (no producer, no
		// inputs) carrying wholesale/retail only — clothing has no production path.
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"linens":  {OutputItem: "linens", OutputQty: 1, WholesalePrice: 3, RetailPrice: 6},
			"woolens": {OutputItem: "woolens", OutputQty: 1, WholesalePrice: 4, RetailPrice: 7},
			"coat":    {OutputItem: "coat", OutputQty: 1, WholesalePrice: 9, RetailPrice: 15},
		},
		Actors: map[sim.ActorID]*sim.ActorSnapshot{actorID: ezekiel, josiahID: josiah},
		Structures: map[sim.StructureID]*sim.Structure{
			forge: plainStructure(forge, "Blacksmith"),
			store: plainStructure(store, "General Store"),
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			sim.VillageObjectID(store): {
				ID: sim.VillageObjectID(store), OwnerActorID: josiahID,
				Tags: []string{sim.TagBusiness, sim.TagDistributor},
			},
		},
	}
	return snap, actorID, nil
}

// smithWorkingClothesWornThin is the LLM-589 threadbare arm and the everyday shape
// of this cue: Ezekiel labours at the forge in a single shift with 1,200 worked
// minutes left of its 10,800 (11%, inside the 20% threadbare band) and no spare
// behind it. The golden pins the "## Your clothes" scene plus the walk-to steer to
// the general store. The steer names BREECHES rather than the shift he is wearing
// out — the cue takes the first working kind with a live supplier, because what it
// owes the model is a garment he can actually buy today, not a like-for-like
// replacement of the one on his back.
func smithWorkingClothesWornThin() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return workingClothesSnapshot(
		map[sim.ItemKind]int{"linens": 1},
		map[sim.ItemKind]int{"linens": 1200},
	)
}

// smithHasNothingFitToWorkIn is the LLM-589 none arm: Ezekiel owns no working
// garment at all — only a coat, which carries `warms` and so belongs to the cold
// self-line. The golden pins both halves of that split at once: the harder
// none-tier scene fires, and the coat neither satisfies the want nor is offered as
// the thing to buy. Without the split the two clothing cues would quietly cover for
// each other and a man in a good coat and rags would hear nothing.
func smithHasNothingFitToWorkIn() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return workingClothesSnapshot(map[sim.ItemKind]int{"coat": 1}, nil)
}

// smithWornClothesSellerPresent is the LLM-589 co-present arm: the same threadbare
// smith now shares Josiah's huddle at the store door. The golden pins the cue
// flipping from the walk-to list to the concrete pay_with_item buy-here imperative
// naming Josiah — the same fast path the nail and shovel cues take, closing the
// at-the-seller dead-spot where the weak model narrates the want and then walks
// off. Foil of smith_working_clothes_worn_thin (Josiah far off → a destination is
// named instead).
func smithWornClothesSellerPresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWorkingClothesWornThin()
	const (
		huddle = sim.HuddleID("store_huddle")
		store  = sim.StructureID("general_store")
	)
	storePos := sim.WorldPos{X: 4000, Y: 4000}.Tile()
	smith := snap.Actors[actorID]
	// Inside the shop with the keeper, mirroring farmOwnerOwesUpkeepSellerPresent.
	// The first cut of this fixture put the smith door-side (Pos at the store,
	// InsideStructureID cleared) on the theory that a customer can't enter a shop —
	// the huddle still resolved the buy, but "## Around you" then read "you are
	// outdoors, with no one else here to hear you speak" directly above a line
	// naming Josiah as standing with him. A golden that pins an incoherent world
	// teaches the next reader the wrong thing about the cue.
	smith.Pos = storePos
	smith.InsideStructureID = store
	smith.CurrentHuddleID = huddle
	smith.Acquaintances = map[string]sim.Acquaintance{"Josiah Thorne": {}}
	josiah := snap.Actors["josiah"]
	josiah.CurrentHuddleID = huddle
	josiah.Acquaintances = map[string]sim.Acquaintance{"Ezekiel Crane": {}}
	// The huddle ROSTER, not just the id on each actor. buildSurroundings reads
	// snap.Huddles for the company line; with the id set and no roster behind it,
	// "## Around you" reports an empty room while the cue names Josiah as standing
	// right there — which is what this fixture printed on its first pass, and what
	// the two older co-present goldens (shovels, nails) still print today.
	snap.Huddles = map[sim.HuddleID]*sim.Huddle{
		huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{actorID: {}, "josiah": {}}},
	}
	return snap, actorID, warrants
}

// villageHasNoClothToSell is the LLM-589 silence arm, and the one most likely to be
// "fixed" into noise by a later hand: the threadbare smith with the store's shelves
// bare. Clothing is import-only (LLM-410), so no seller anywhere is an ORDINARY
// state rather than a resolution failure — unlike the farm-upkeep cue, which can
// safely fall back on naming the blacksmith because the smith definitively makes
// shovels. The golden pins that the section renders NOTHING at all: a standing line
// naming a problem with no action attached would otherwise ride every tick of every
// working actor in the village, which is the noise the scenes-not-stats register
// exists to keep out. Foil of smith_working_clothes_worn_thin — same wear, same
// posture, no stock.
func villageHasNoClothToSell() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWorkingClothesWornThin()
	snap.Actors["josiah"].Inventory = map[sim.ItemKind]int{}
	return snap, actorID, warrants
}

// smithWornClothesOffShift is the LLM-595 audience boundary: the same threadbare
// smith stood at his own forge, but OUTSIDE his shift window (an evening shift,
// while the village clock reads mid-morning). The business-person case is the
// shift window — off shift his garments take no wear, so the cue would be
// telling him his clothes are going when they are not. The golden pins silence.
// Foil of smith_working_clothes_worn_thin — same wear, same spot, wrong hours.
func smithWornClothesOffShift() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWorkingClothesWornThin()
	eveStart, eveEnd := 1140, 180 // 19:00–03:00 — excludes the fixture's 10:00
	snap.Actors[actorID].ScheduleStartMin = &eveStart
	snap.Actors[actorID].ScheduleEndMin = &eveEnd
	return snap, actorID, warrants
}

// withMenderAtInn extends a working-clothes fixture with the LLM-625 mending
// economy: Hannah keeps the Inn — tagged TagMending — holding `thread` spools of
// thread, and the catalog gains the mending service kind (capabilities
// service+mending, retail 4) and the imported thread material. Built as an
// overlay on workingClothesSnapshot so every mend golden is a strict foil of an
// existing LLM-589 arm: same smith, same wear, one new resolution in the world.
func withMenderAtInn(snap *sim.Snapshot, thread int) *sim.Snapshot {
	const (
		hannahID = sim.ActorID("hannah")
		inn      = sim.StructureID("inn")
	)
	start, end := 360, 1080
	snap.ItemKinds["mending"] = &sim.ItemKindDef{
		Name: "mending", DisplayLabel: "Mending",
		DisplayLabelSingular: "mending", DisplayLabelPlural: "mending",
		Category:     sim.ItemCategory("service"),
		Capabilities: []string{"service", string(sim.CapabilityMending)},
		Description:  "Needle and thread put to worn clothes — brought back good as new.",
	}
	snap.ItemKinds[sim.MendThreadKind] = &sim.ItemKindDef{
		Name: sim.MendThreadKind, DisplayLabel: "Thread",
		DisplayLabelSingular: "spool of thread", DisplayLabelPlural: "spools of thread",
		Category: sim.ItemCategoryMaterial,
	}
	snap.Recipes["mending"] = &sim.ItemRecipe{OutputItem: "mending", OutputQty: 1, WholesalePrice: 2, RetailPrice: 4}
	snap.Recipes[sim.MendThreadKind] = &sim.ItemRecipe{OutputItem: sim.MendThreadKind, OutputQty: 1, WholesalePrice: 1, RetailPrice: 2}
	inventory := map[sim.ItemKind]int{}
	if thread > 0 {
		inventory[sim.MendThreadKind] = thread
	}
	snap.Actors[hannahID] = &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Hannah Boggs",
		Role:              "innkeeper",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 2000, Y: 2000}.Tile(),
		WorkStructureID:   inn,
		InsideStructureID: inn,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             30,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         inventory,
	}
	snap.Structures[inn] = plainStructure(inn, "The Ordinary")
	snap.VillageObjects[sim.VillageObjectID(inn)] = &sim.VillageObject{
		ID: sim.VillageObjectID(inn), OwnerActorID: hannahID,
		Tags: []string{sim.TagBusiness, sim.TagMending},
	}
	return snap
}

// smithWornClothesMenderAtInn is the LLM-625 mend arm: the threadbare smith of
// smith_working_clothes_worn_thin, with Hannah now taking in mending at the inn
// — thread on hand — while Josiah's shelves still hold replacement garments.
// The golden pins the cue steering to the MEND (walk-to the inn) instead of the
// replacement: the wearer's own garment comes back for less coin, so the
// conserving path wins whenever it exists.
func smithWornClothesMenderAtInn() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWorkingClothesWornThin()
	return withMenderAtInn(snap, 6), actorID, warrants
}

// smithWornClothesMenderPresent is the LLM-625 co-present arm: the threadbare
// smith shares Hannah's huddle at the inn. The golden pins the concrete
// pay_with_item buy-here imperative naming Hannah and the mending item — the
// same fast path the seller-present garment arm takes, so the mend cannot
// dead-spot right at the mender.
func smithWornClothesMenderPresent() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWornClothesMenderAtInn()
	const (
		huddle = sim.HuddleID("inn_huddle")
		inn    = sim.StructureID("inn")
	)
	smith := snap.Actors[actorID]
	smith.Pos = sim.WorldPos{X: 2000, Y: 2000}.Tile()
	smith.InsideStructureID = inn
	smith.CurrentHuddleID = huddle
	smith.Acquaintances = map[string]sim.Acquaintance{"Hannah Boggs": {}}
	hannah := snap.Actors["hannah"]
	hannah.CurrentHuddleID = huddle
	hannah.Acquaintances = map[string]sim.Acquaintance{"Ezekiel Crane": {}}
	snap.Huddles = map[sim.HuddleID]*sim.Huddle{
		huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{actorID: {}, "hannah": {}}},
	}
	return snap, actorID, warrants
}

// smithWornClothesMenderOutOfThread is the LLM-625 thread gate: Hannah takes in
// mending but holds no thread, so the mend arm is dead and the cue falls back
// to the LLM-589 replacement steer at the general store. The golden pins the
// fallback — a mender with nothing to mend WITH must not swallow the working
// replace path. Foil of smith_worn_clothes_mender_at_inn (thread 6 → 0).
func smithWornClothesMenderOutOfThread() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithWorkingClothesWornThin()
	return withMenderAtInn(snap, 0), actorID, warrants
}

// smithNothingToWorkInMenderLive is the LLM-625 none-tier boundary: Ezekiel
// owns no working garment at all while a thread-stocked mender is live. The
// golden pins that the NONE tier keeps the replacement steer — mending
// conserves garments, it cannot create one, so a man with nothing on the peg
// is never sent to the mender. Foil of smith_has_nothing_fit_to_work_in (same
// wardrobe, a mender added, same steer).
func smithNothingToWorkInMenderLive() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := smithHasNothingFitToWorkIn()
	return withMenderAtInn(snap, 6), actorID, warrants
}

// keeperOffPostBuying builds the LLM-611 fixture: Josiah Thorne, shopkeeper, standing
// inside the Ellis Farm rather than his own General Store — the restocking trip, which
// is where a distributor does the bulk of its bargaining. Purse at 1 coin against a
// pack of merchandise, the live 2026-08-06 shape: he cannot meet a coin price, so he
// offers goods, and nothing was telling him what those goods are worth.
//
// withCompany toggles the audience half of the gate (a shared huddle with Elizabeth,
// the seller) so the paired goldens differ in exactly one variable.
func keeperOffPostBuying(withCompany bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID    = sim.ActorID("josiah")
		elizabethID = sim.ActorID("elizabeth")
		store       = sim.StructureID("general_store")
		farm        = sim.StructureID("ellis_farm")
		home        = sim.StructureID("thorne_residence")
		farmhouse   = sim.StructureID("ellis_farmhouse")
		huddle      = sim.HuddleID("h1")
	)
	start, end := 360, 1260 // 06:00–21:00
	now := 720              // 12:00 — midday, on shift and away restocking
	published := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:               sim.KindNPCStateful,
		DisplayName:        "Josiah Thorne",
		Role:               "merchant",
		State:              sim.StateIdle,
		WorkStructureID:    store,
		InsideStructureID:  farm,
		HomeStructureID:    home,
		ScheduleStartMin:   &start,
		ScheduleEndMin:     &end,
		BusinessownerState: &sim.BusinessownerState{},
		Coins:              1,
		Needs:              map[sim.NeedKey]int{},
		Inventory:          map[sim.ItemKind]int{"flour": 12, "carrots": 4, "firewood": 2, "milk": 2},
	}
	elizabeth := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Elizabeth Ellis",
		Role:              "farmer",
		State:             sim.StateIdle,
		WorkStructureID:   farm,
		InsideStructureID: farm,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             40,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"cheese": 1},
	}
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{josiahID: josiah, elizabethID: elizabeth},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			farm:  plainStructure(farm, "Ellis Farm"),
			home:  plainStructure(home, "Thorne Residence"),
		},
	}
	// Elizabeth stays in the world in BOTH arms — only her whereabouts move. Deleting
	// her would change actor discovery and relationship rendering as well as the
	// audience, so the foil could shift for reasons that have nothing to do with this
	// gate. Alone, she is inside her own farmhouse: a different structure, so neither
	// the huddle roster nor the co-located audience reaches her.
	josiah.Acquaintances = map[string]sim.Acquaintance{"Elizabeth Ellis": {}}
	if withCompany {
		josiah.CurrentHuddleID = huddle
		elizabeth.CurrentHuddleID = huddle
		snap.Huddles = map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, Members: map[sim.ActorID]struct{}{josiahID: {}, elizabethID: {}}},
		}
	} else {
		elizabeth.InsideStructureID = farmhouse
		snap.Structures[farmhouse] = plainStructure(farmhouse, "Ellis Farmhouse")
	}
	return snap, josiahID, nil
}

func keeperOffPostBuyingInCompany() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return keeperOffPostBuying(true)
}

func keeperOffPostBuyingAlone() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return keeperOffPostBuying(false)
}

// commonsWellScenario builds the SHIPPED well (LLM-254) with one actor standing
// at it: an infinite drink row carrying NO GatherItem, plus a separate finite
// yield-only carry row. Parameterized by the subject's restock manifest so the
// paired goldens differ in exactly one variable — whether he holds the trade.
//
// Unowned, so ownership grants nothing and the LLM-610 permission gate is the
// only thing deciding whether a pail is on offer.
func commonsWellScenario(policy *sim.RestockPolicy) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const subjectID = sim.ActorID("ezekiel")
	zero := 0
	start, end := 420, 960 // 07:00–16:00
	now := 780             // 13:00 — on shift
	wellPin := sim.WorldPos{X: 100, Y: 100}.Tile()
	subject := &sim.ActorSnapshot{
		Kind:             sim.KindNPCStateful,
		DisplayName:      "Ezekiel Crane",
		Role:             "blacksmith",
		State:            sim.StateIdle,
		Pos:              wellPin,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            493,
		Needs:            map[sim.NeedKey]int{},
		RestockPolicy:    policy,
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{subjectID: subject},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"water": {
				Name:         "water",
				Capabilities: []string{"portable"}, DisplayLabel: "Water",
				DisplayLabelSingular: "pail of water", DisplayLabelPlural: "pails of water",
				Category:  sim.ItemCategoryDrink,
				Satisfies: []sim.ItemSatisfaction{{Attribute: "thirst", Immediate: 8}},
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"village_well": {
				ID:            "village_well",
				DisplayName:   "Village Well",
				Pos:           sim.WorldPos{X: 100, Y: 100},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Attribute: "thirst", Amount: -8}, // drink: infinite, ungathered
					{Amount: 0, GatherItem: "water", AvailableQuantity: intp(20), MaxQuantity: intp(20)}, // carry: yield-only
				},
			},
		},
	}
	return snap, subjectID, nil
}

// smithAtCommonsWellMayNotDraw — the live 2026-08-06 situation. He BUYS water
// (a buy entry is not a licence to draw), and forages only firewood.
func smithAtCommonsWellMayNotDraw() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return commonsWellScenario(&sim.RestockPolicy{Restock: []sim.RestockEntry{
		{Item: "firewood", Source: sim.RestockSourceForage, Max: 10},
		{Item: "water", Source: sim.RestockSourceBuy, Max: 12},
	}})
}

// waterDrawerAtCommonsWellMayDraw — the same well, the same tile, the role holder.
func waterDrawerAtCommonsWellMayDraw() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	return commonsWellScenario(&sim.RestockPolicy{Restock: []sim.RestockEntry{
		{Item: "water", Source: sim.RestockSourceForage, Max: 20},
	}})
}

// TestGoldensNeverOfferAGatherWithoutTheTrade is the LLM-610 cross-scenario
// invariant. Across the WHOLE matrix: if a scene offers a gather, the resolved
// source must be one the subject may actually take from — a pick-and-eat commons,
// something he owns, or a yield-only source whose item is in his forage manifest.
//
// The property, not the instance: the smith-at-the-well golden pins one scene, and
// this pins that no scenario anyone adds later can quietly reintroduce the leak.
func TestGoldensNeverOfferAGatherWithoutTheTrade(t *testing.T) {
	for _, sc := range perceptionScenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			snap, actorID, warrants := sc.build()
			p := Build(snap, actorID, warrants)
			if p.Surroundings.GatherableItem == "" {
				return // no gather on offer — nothing to check
			}
			a := snap.Actors[actorID]
			if a == nil {
				t.Fatalf("scenario %q has no subject actor", sc.name)
			}
			_, obj, row := sim.ResolveGatherSource(
				snap.VillageObjects, snap.Assets, a.Pos, actorID, a.GatherTargetObjectID,
				sim.LowForageItems(a.RestockPolicy, a.Inventory, snap.RestockReorderPct),
				sim.ForageItems(a.RestockPolicy))
			if row == nil {
				t.Fatalf("cue offers %q but the shared resolution finds no source — cue and command have diverged",
					p.Surroundings.GatherableItem)
			}
			if !sim.MayGatherSource(obj, row, actorID, sim.ForageItems(a.RestockPolicy)) {
				t.Errorf("scene offers a gather of %q from %q the subject may not take: yield-only=%v, owner=%q, forage set=%v",
					p.Surroundings.GatherableItem, p.Surroundings.GatherableSource,
					row.IsYieldOnly(), obj.OwnerActorID, sim.ForageItems(a.RestockPolicy))
			}
		})
	}
}

// growerAtOwnRipeBush is the LLM-617 fixture pair: Moses James, a wheat grower with
// an empty pack, standing exactly on the loiter pin of one of his OWN ripe bushes.
//
// `spare` adds a second ripe bush far across the map. That one variable is the
// whole foil:
//   - spare=true  → a walkable ripe bush exists, so the steer must name THAT one
//     and never the bush underfoot.
//   - spare=false → every ripe bush is underfoot, so there is no walk to steer and
//     the cue must say where he stands instead of emitting a handle.
//
// The bush underfoot is the RIPEST (9 units vs 5), so the pre-LLM-617 ripest-wins
// rule picked precisely the bush move_to would bounce as a no-op — the live wedge
// that held Moses at his field for two hours re-calling move_to on a bush he had
// already reached.
//
// Both bushes carry an explicit (0,0) loiter override so the pin IS the anchor
// tile, keeping the fixture independent of the footprint fallback. Idle, on shift,
// no orders, no price book, no clock read → byte-stable.
func growerAtOwnRipeBush(spare bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const mosesID = sim.ActorID("moses")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	nearTile := sim.WorldPos{X: 10 * 32, Y: 87 * 32}.Tile()
	moses := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Moses James",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              nearTile, // standing ON the near bush's loiter pin
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            46,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"wheat": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceForage, Max: 30},
		}},
		// The owned-bush cue is sourced from EARNED MEMORY (LLM-79), so the grower
		// only sees bushes he remembers gathering at.
		KnownPlaces: map[sim.PlaceRef]*sim.KnownPlace{
			"near_bush": {Ref: "near_bush", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
			"far_bush":  {Ref: "far_bush", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
		},
	}
	objects := map[sim.VillageObjectID]*sim.VillageObject{
		"near_bush": {
			ID:            "near_bush",
			DisplayName:   "Wheat",
			Pos:           sim.WorldPos{X: 10 * 32, Y: 87 * 32},
			OwnerActorID:  mosesID,
			LoiterOffsetX: &zero,
			LoiterOffsetY: &zero,
			Refreshes: []*sim.ObjectRefresh{
				// Forage-to-sell (Amount 0): a yield-only harvest row, as the live
				// owned crop plots are. The RIPEST of the two on purpose.
				{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(9), MaxQuantity: intp(9)},
			},
		},
	}
	if spare {
		objects["far_bush"] = &sim.VillageObject{
			ID:            "far_bush",
			DisplayName:   "Wheat",
			Pos:           sim.WorldPos{X: 40 * 32, Y: 40 * 32},
			OwnerActorID:  mosesID,
			LoiterOffsetX: &zero,
			LoiterOffsetY: &zero,
			Refreshes: []*sim.ObjectRefresh{
				{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(5), MaxQuantity: intp(9)},
			},
		}
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {
				Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
		VillageObjects: objects,
	}
	return snap, mosesID, nil
}

// growerUnderfootWithNoGatherCue is the LLM-617 worst case, added at code_review's
// request: the grower stands at his own ripe bush AND the at-bush gather cue is
// suppressed, so the section carries no callable action at all.
//
// Reachable because the two cues ask different questions. A NON-GATHERABLE object
// sits nearer than the bush (a fence post on the pin the grower occupies), and
// sim.ResolveGatherSource answers "which place is he at?" — nearest wins, and a
// non-gatherable nearest BLOCKS rather than yielding to a farther bush — so gather
// is never advertised. sim.ActorAtObjectPin answers the object-keyed question about
// the bush itself and still reports him as standing there, so the walk steer is
// correctly withheld.
//
// The golden pins the honest outcome: the ripe count and the standing-among-them
// line, NO move_to, and NO "you can gather" line. That is deliberately preferred to
// the alternative — emitting a move_to guaranteed to terminal-no-op, which burns the
// tick's one terminal verb and is the wedge itself — and to inventing a gather
// instruction, which is the LLM-59/79 rejected-tool fixation this cue was shaped to
// avoid. If a future change makes the section carry an action here, that is an
// improvement and this golden should be updated deliberately, not silently.
func growerUnderfootWithNoGatherCue() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const mosesID = sim.ActorID("moses")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	standTile := sim.WorldPos{X: 10 * 32, Y: 87 * 32}.Tile()
	moses := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Moses James",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              standTile,
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            46,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"wheat": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceForage, Max: 30},
		}},
		KnownPlaces: map[sim.PlaceRef]*sim.KnownPlace{
			"near_bush": {Ref: "near_bush", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {
				Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			// Ripe and his own, one tile off — inside LoiterAttributionTiles, so the
			// already-at guard withholds the steer.
			"near_bush": {
				ID:            "near_bush",
				DisplayName:   "Wheat",
				Pos:           sim.WorldPos{X: 10 * 32, Y: 88 * 32},
				OwnerActorID:  mosesID,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(9), MaxQuantity: intp(9)},
				},
			},
			// NEARER and carries no gatherable row at all: ResolveGatherSource's
			// nearest-wins scan stops here and blocks, so gather is never advertised.
			"fence_post": {
				ID:            "fence_post",
				DisplayName:   "Fence Post",
				Pos:           sim.WorldPos{X: 10 * 32, Y: 87 * 32},
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
			},
		},
	}
	return snap, mosesID, nil
}

// growerAtForeignPinRingSlot is the LLM-618 fixture pair: Moses James standing on a
// legal visitor slot of his OWN ripe wheat, where that slot is Chebyshev 0 from a
// NEIGHBOUR's wheat plant.
//
//	Moses          (10,89) — a ring slot, the normal place an ObjectVisit parks him
//	mine_bush pin  (11,90) — his own ripe bush, Chebyshev 1 (LLM-617 withholds its steer)
//	neighbour_bush (10,89) — another actor's ripe bush, Chebyshev 0 — the strict nearest
//
// These are the live tile numbers from the 2026-08-07 wedge, and the collision is not
// exotic: pickObjectVisitorSlot parks an arrival on one of eight ring slots, so any
// ring slot may be some other object's pin.
//
// `walkedTo` is the single variable, and it is the whole foil:
//   - true  → Moses carries mine_bush as his walked-to target (stamped on arrival from
//     ActorArrived.DestObjectID), so walkedToGatherSeed answers the known-place
//     question about the bush he came for and the gather cue renders.
//   - false → no target, so the unchanged nearest-scan answers, finds the neighbour's
//     bush owning the tile, and the cue stays suppressed. The poacher gate is intact.
//
// Both bushes carry an explicit (0,0) loiter override so each pin IS its anchor tile,
// keeping the fixture independent of the footprint fallback. Idle, on shift, no orders,
// no price book, no clock read → byte-stable.
func growerAtForeignPinRingSlot(walkedTo bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const mosesID = sim.ActorID("moses")
	zero := 0
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	moses := &sim.ActorSnapshot{
		Kind:             sim.KindNPCShared,
		DisplayName:      "Moses James",
		Role:             "farmer",
		State:            sim.StateIdle,
		Pos:              sim.WorldPos{X: 10 * 32, Y: 89 * 32}.Tile(),
		ScheduleStartMin: &start,
		ScheduleEndMin:   &end,
		Coins:            46,
		Needs:            map[sim.NeedKey]int{},
		Inventory:        map[sim.ItemKind]int{"wheat": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceForage, Max: 30},
		}},
		// Only his own bush is remembered — the owned-bush cue is sourced from
		// EARNED MEMORY (LLM-79), and a neighbour's plant was never his to learn.
		KnownPlaces: map[sim.PlaceRef]*sim.KnownPlace{
			"mine_bush": {Ref: "mine_bush", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
		},
	}
	if walkedTo {
		moses.GatherTargetObjectID = "mine_bush"
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {
				Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"mine_bush": {
				ID:            "mine_bush",
				DisplayName:   "Wheat",
				Pos:           sim.WorldPos{X: 11 * 32, Y: 90 * 32},
				OwnerActorID:  mosesID,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(6), MaxQuantity: intp(9)},
				},
			},
			// The neighbour's plant, ripe and owned by someone else, sitting exactly on
			// the tile Moses was parked at. Nothing about it is malformed — it is an
			// ordinary bush that happens to pin the slot he arrived on.
			"neighbour_bush": {
				ID:            "neighbour_bush",
				DisplayName:   "Wheat",
				Pos:           sim.WorldPos{X: 10 * 32, Y: 89 * 32},
				OwnerActorID:  sim.ActorID("neighbour"),
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(2), MaxQuantity: intp(3)},
				},
			},
		},
	}
	return snap, mosesID, nil
}

// smithWithEmptyShelfTakesCommissions is the LLM-619 fixture — the live Ezekiel
// Crane / Moses James shape of 2026-08-07.
//
// The smith PRODUCES shovels, holds NONE (one is on the fire), and has the water a
// batch needs. Moses is co-present at the forge. Before this fix the sale cue built
// "Your goods to sell" from physical inventory alone, so the shovel vanished from
// the listing entirely and the model read that as "cannot sell" — answering with a
// spoken promise instead. A promise commits nothing, so no order row existed for
// LLM-518's farm-upkeep suppression to find, and Moses walked forge<->home 13 times
// in 75 minutes re-striking the same bargain six times.
//
// The golden pins the shovel present as a commission, and the steer that names
// `sell` for it — because the listing alone was never the whole problem; the model
// also had to be told the sale was still its to make. Nail is the control: he holds
// 3, so it stays an ordinary take-home sale listed with its count, never doubled.
//
// Idle, no orders, no price book, no clock read → byte-stable.
func smithWithEmptyShelfTakesCommissions() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		ezekielID = sim.ActorID("ezekiel")
		mosesID   = sim.ActorID("moses")
		forge     = sim.StructureID("blacksmith")
	)
	start, end := 360, 1080 // 06:00–18:00
	now := 600              // 10:00 — on shift
	huddle := sim.HuddleID("hud-llm619")
	ezekiel := &sim.ActorSnapshot{
		Kind: sim.KindNPCStateful, DisplayName: "Ezekiel Crane", Role: "blacksmith",
		State: sim.StateIdle, WorkStructureID: forge, InsideStructureID: forge,
		BusinessownerState: &sim.BusinessownerState{},
		CurrentHuddleID:    huddle,
		ScheduleStartMin:   &start, ScheduleEndMin: &end,
		Coins: 511, Needs: map[sim.NeedKey]int{},
		// No shovel on hand — the whole point. Water is the shovel's input.
		Inventory: map[sim.ItemKind]int{"water": 3, "nail": 3},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "shovel", Source: sim.RestockSourceProduce, Max: 3},
			{Item: "nail", Source: sim.RestockSourceProduce, Max: 20},
		}},
	}
	moses := &sim.ActorSnapshot{
		Kind: sim.KindNPCShared, DisplayName: "Moses James", Role: "farmer",
		State: sim.StateIdle, InsideStructureID: forge, CurrentHuddleID: huddle,
		Coins: 50, Needs: map[sim.NeedKey]int{},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors:           map[sim.ActorID]*sim.ActorSnapshot{ezekielID: ezekiel, mosesID: moses},
		Structures:       map[sim.StructureID]*sim.Structure{forge: plainStructure(forge, "Blacksmith")},
		Huddles: map[sim.HuddleID]*sim.Huddle{
			huddle: {ID: huddle, StructureID: forge, Members: map[sim.ActorID]struct{}{ezekielID: {}, mosesID: {}}},
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"shovel": {Name: "shovel", DisplayLabel: "Shovel", Capabilities: []string{"portable"},
				DisplayLabelSingular: "shovel", DisplayLabelPlural: "shovels", Category: sim.ItemCategoryCraft},
			"nail": {Name: "nail", DisplayLabel: "Nail", Capabilities: []string{"portable"},
				DisplayLabelSingular: "nail", DisplayLabelPlural: "nails", Category: sim.ItemCategoryCraft},
			"water": {Name: "water", DisplayLabel: "Water", Capabilities: []string{"portable"},
				DisplayLabelSingular: "flask of water", DisplayLabelPlural: "flasks of water", Category: sim.ItemCategoryDrink},
		},
		Recipes: map[sim.ItemKind]*sim.ItemRecipe{
			"shovel": {OutputItem: "shovel", OutputQty: 1, RateQty: 1, RatePerHours: 1,
				Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}}},
			"nail": {OutputItem: "nail", OutputQty: 1, RateQty: 1, RatePerHours: 1,
				Inputs: []sim.RecipeInput{{Item: "water", Qty: 1}}},
		},
	}
	return snap, ezekielID, nil
}

// growerHomeMidShift builds the LLM-620 grower pair: Moses James standing inside
// his own house during working hours, low on wheat, with owned bushes he remembers
// gathering at. ripe selects the whole foil:
//
//   - ripe=false → the plot carries NOTHING ripe. The harvest cue still renders
//     (it gates on low stock, not ripeness) and reads "walk out to your bushes to
//     restock … none ripe yet — they will regrow, so check back later". There is no
//     step to take, so this is a standing want and the to-work steer must FIRE.
//   - ripe=true  → one bush has stock. The errand is real, the HOME-400/LLM-90
//     suppression applies, and the status-only arm renders in the yank's place.
//
// Two bushes on purpose, both remembered and both his: with ripe=true only one
// carries stock, so the fixture also proves Actionable reads the plot rather than
// the first entry. He is at HOME, not at the field, which is the state the live lap
// kept returning to and the one that used to render no shift fact at all.
//
// Idle, no orders, no price book, no clock read → byte-stable.
func growerHomeMidShift(ripe bool) (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		mosesID = sim.ActorID("moses")
		farm    = sim.StructureID("james_farm")
		home    = sim.StructureID("james_residence")
	)
	zero := 0
	start, end := 360, 1080 // working hours 06:00–18:00
	now := 900              // 15:00 — mid-shift, away from post
	stock := 0
	if ripe {
		stock = 7
	}
	moses := &sim.ActorSnapshot{
		Kind:              sim.KindNPCShared,
		DisplayName:       "Moses James",
		Role:              "farmer",
		State:             sim.StateIdle,
		Pos:               sim.WorldPos{X: 20 * 32, Y: 100 * 32}.Tile(),
		WorkStructureID:   farm,
		HomeStructureID:   home,
		InsideStructureID: home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             44,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"wheat": 2},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceForage, Max: 30},
		}},
		// Earned memory (LLM-79): the owned-bush cue only sees bushes he remembers.
		KnownPlaces: map[sim.PlaceRef]*sim.KnownPlace{
			"north_field": {Ref: "north_field", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
			"south_field": {Ref: "south_field", Kind: sim.PlaceKindObject, Affordances: []string{"gather:wheat"}},
		},
	}
	snap := &sim.Snapshot{
		LocalMinuteOfDay:  &now,
		NeedThresholds:    sim.NeedThresholds{},
		Assets:            emptyAssetSet,
		RestockReorderPct: 25,
		Actors:            map[sim.ActorID]*sim.ActorSnapshot{mosesID: moses},
		Structures: map[sim.StructureID]*sim.Structure{
			farm: plainStructure(farm, "James Farm"),
			home: plainStructure(home, "James Residence"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {
				Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
		VillageObjects: map[sim.VillageObjectID]*sim.VillageObject{
			"north_field": {
				ID:            "north_field",
				DisplayName:   "Wheat",
				Pos:           sim.WorldPos{X: 60 * 32, Y: 40 * 32},
				OwnerActorID:  mosesID,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(stock), MaxQuantity: intp(9)},
				},
			},
			"south_field": {
				ID:            "south_field",
				DisplayName:   "Wheat",
				Pos:           sim.WorldPos{X: 62 * 32, Y: 44 * 32},
				OwnerActorID:  mosesID,
				LoiterOffsetX: &zero,
				LoiterOffsetY: &zero,
				Refreshes: []*sim.ObjectRefresh{
					{Amount: 0, GatherItem: "wheat", AvailableQuantity: intp(0), MaxQuantity: intp(9)},
				},
			},
		},
	}
	return snap, mosesID, nil
}

// growerAtPostMidShiftNothingRipe is the LLM-643 at-post arm: growerHomeMidShift's
// grower (nothing ripe) moved to his own post. Only the location differs — the
// diff against the home arm is exactly which duty voice renders (the at-post
// stabilizer here, the to-work yank there) and what the anchors line offers.
func growerAtPostMidShiftNothingRipe() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, id, warrants := growerHomeMidShift(false)
	a := snap.Actors[id]
	a.InsideStructureID = a.WorkStructureID
	return snap, id, warrants
}

// keeperHomeMidShiftActionableRestock is the LLM-620 Josiah Thorne scenario: the
// shopkeeper at home during working hours with a restock errand that IS actionable
// — wheat low, a supplier (James Farm) open, in business, and inside his purse.
//
// The suppression is therefore correct and stays: he is not yanked back to the shop
// while a real errand stands. What the golden pins is the second half of the fix —
// that deferring the yank no longer deletes the shift fact along with it. The hour
// is evening (18:10) against a 21:00 close, exactly the live configuration in which
// he wrote "Morning's come" at home and "the day's winding down" at his post twenty
// nine seconds later.
//
// Idle, no huddle, no orders, one price observation for the cost hint → byte-stable.
func keeperHomeMidShiftActionableRestock() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	const (
		josiahID = sim.ActorID("josiah")
		mosesID  = sim.ActorID("moses")
		store    = sim.StructureID("general_store")
		home     = sim.StructureID("thorne_residence")
		farm     = sim.StructureID("james_farm")
	)
	start, end := 360, 1260 // working hours 06:00–21:00 (closes at 9 in the evening)
	now := 1090             // 18:10 — evening, still on shift
	published := time.Date(2026, 7, 1, 18, 10, 0, 0, time.UTC)
	josiah := &sim.ActorSnapshot{
		Kind:              sim.KindNPCStateful,
		DisplayName:       "Josiah Thorne",
		Role:              "shopkeeper",
		State:             sim.StateIdle,
		Pos:               sim.TilePos{X: 87, Y: 144},
		WorkStructureID:   store,
		HomeStructureID:   home,
		InsideStructureID: home,
		ScheduleStartMin:  &start,
		ScheduleEndMin:    &end,
		Coins:             114,
		Needs:             map[sim.NeedKey]int{},
		Inventory:         map[sim.ItemKind]int{"wheat": 0},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceBuy, Max: 20},
		}},
	}
	moses := &sim.ActorSnapshot{
		Kind:            sim.KindNPCShared,
		DisplayName:     "Moses James",
		State:           sim.StateIdle,
		Pos:             sim.TilePos{X: 20, Y: 100},
		WorkStructureID: farm,
		Inventory:       map[sim.ItemKind]int{"wheat": 40},
		RestockPolicy: &sim.RestockPolicy{Restock: []sim.RestockEntry{
			{Item: "wheat", Source: sim.RestockSourceProduce, Max: 40},
		}},
	}
	// Buyer-side price history: what Josiah last paid Moses for wheat, which is the
	// cost hint the walk-to bullet renders.
	wheatBuys := sim.NewRingBuffer[sim.PriceObservation](8)
	wheatBuys.Push(sim.PriceObservation{BuyerID: josiahID, Amount: 6, Qty: 6, Consumers: 1, At: published.Add(-2 * 24 * time.Hour)})
	snap := &sim.Snapshot{
		PublishedAt:      published,
		LocalMinuteOfDay: &now,
		NeedThresholds:   sim.NeedThresholds{},
		Assets:           emptyAssetSet,
		Actors: map[sim.ActorID]*sim.ActorSnapshot{
			josiahID: josiah, mosesID: moses,
		},
		Structures: map[sim.StructureID]*sim.Structure{
			store: plainStructure(store, "General Store"),
			home:  plainStructure(home, "Thorne Residence"),
			farm:  plainStructure(farm, "James Farm"),
		},
		ItemKinds: map[sim.ItemKind]*sim.ItemKindDef{
			"wheat": {
				Name: "wheat", Capabilities: []string{"portable"}, DisplayLabel: "Wheat",
				DisplayLabelSingular: "sheaf of wheat", DisplayLabelPlural: "sheaves of wheat",
				Category: sim.ItemCategoryMaterial,
			},
		},
		RestockReorderPct: 25,
		PriceBook: map[sim.PriceBookKey]*sim.RingBuffer[sim.PriceObservation]{
			{SellerID: mosesID, Item: "wheat"}: wheatBuys,
		},
	}
	return snap, josiahID, nil
}

// scheduledKeeperPreshiftGetsNoBakeCue is the LLM-650 live case: the comfortable
// homebody fixture given Lewis Walker's wright trade (LLM-648) — a 10:00–18:00 shift at
// Lewis's Workshop, an hour off at the 09:00 fixture clock, with the household bake
// already going. The bake would run to dusk and swallow the shift, so no invitation.
func scheduledKeeperPreshiftGetsNoBakeCue() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := comfortableHomebodyBakes()
	const workshop = sim.StructureID("lewis_workshop")
	start, end := 600, 1080 // 10:00–18:00
	a := snap.Actors[actorID]
	a.DisplayName = "Lewis Walker"
	a.Role = "wright"
	a.WorkStructureID = workshop
	a.ScheduleStartMin, a.ScheduleEndMin = &start, &end
	snap.Structures[workshop] = plainStructure(workshop, "Lewis's Workshop")
	return snap, actorID, warrants
}

// nightKeeperOffshiftJoinsBake is the positive half of the LLM-650 pair: the same
// wright with a 20:00–23:00 shift, which begins after dusk — the day is free, the
// invitation renders.
func nightKeeperOffshiftJoinsBake() (*sim.Snapshot, sim.ActorID, []sim.WarrantMeta) {
	snap, actorID, warrants := scheduledKeeperPreshiftGetsNoBakeCue()
	start, end := 1200, 1380 // 20:00–23:00
	a := snap.Actors[actorID]
	a.ScheduleStartMin, a.ScheduleEndMin = &start, &end
	return snap, actorID, warrants
}
