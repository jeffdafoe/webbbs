// Package httpapi is the v2 engine's client-facing read surface. Handlers
// close over a *sim.World and read lock-free, with no command channel on the
// read path. Per-tick state (world / agents / objects) is read from the
// published snapshot (world.Published()); reference state loaded once at
// startup (assets / terrain / sprite catalogs) is read directly off *sim.World, which is
// immutable post-load. The wire DTOs here are v2-native — shaped by
// sim.Snapshot + the reference catalogs, not v1's DB-era JSON — and are the
// single source of truth documented in the shared contract note
// shared/notes/codebase/salem-engine-v2/client-contract (consumed by the
// Godot client).
package httpapi

import (
	"time"

	"github.com/jeffdafoe/llm-memory-plugin-salem-1692/engine/sim"
)

// ContractVersion is the monotonic version of the whole read API. The Godot
// client embeds the version it was built against and fails loudly on a
// mismatch. Bump ONLY on a breaking change (rename/remove/retype a field,
// change the WS envelope); additive new optional fields do not bump it.
//
// ZBBS-WORK-363 kept this at 1: WorldStateDTO.Now was re-sourced from the dead
// WorldEnvironment.Now (never assigned → always zero) to the real
// Snapshot.PublishedAt — a value change, not a shape change — and the camera
// zoom floors are additive. Old clients keep decoding, so no bump.
//
// LLM-626 bumped 1 → 2: the umbilical settings DTO REMOVED two fields
// (visitor_spawn_chance_permille, visitor_passer_through_chance_permille;
// replaced by the three spawn-roll chances) — a shape change under the rule
// above. The Godot client rebuilds in the same deploy, so the loud version
// mismatch only ever bites a stale cached client, which is the point.
const ContractVersion = 2

// WorldStateDTO is the GET /api/village/world response — coarse world state
// for the client's top bar / lighting + the per-player camera zoom floor.
// The carrier of ContractVersion.
type WorldStateDTO struct {
	ContractVersion int    `json:"contract_version"`
	Phase           string `json:"phase"` // "day" | "night"
	Tick            uint64 `json:"tick"`
	// Now is the engine's wall clock (UTC) at snapshot-publish time — the
	// authoritative world clock every client reads off the public world poll.
	// Sourced from Snapshot.PublishedAt (real time.Now()); the sim runs its
	// day/night + time-of-day off the same clock in WorldSettings.Timezone
	// (default America/New_York). Serialized UTC; the client localizes for
	// display. (Was previously wired to the never-assigned WorldEnvironment.Now,
	// which always serialized as the 0001-01-01 zero time.)
	Now        time.Time `json:"now"`
	Weather    string    `json:"weather"`
	Atmosphere string    `json:"atmosphere"`
	// LastTransitionAt is the wall-clock instant of the most recent REAL
	// day↔night flip (UTC) — Environment.LastPhaseFlipAt, not the ticker's
	// From==To-inclusive dedupe stamp. The client positions its sunset/sunrise color curve
	// from elapsed = Now − LastTransitionAt on load/resync, so a reconnecting
	// client lands mid-transition instead of snapping to the pole (LLM-578).
	// Pointer + omitempty: omitted only when NO transition has ever happened
	// (fresh world) — same omit-on-unset-only posture as the umbilical's
	// hearth_lit_until (LLM-559); a value time.Time would stamp the 0001-01-01
	// zero instant instead of omitting.
	LastTransitionAt *time.Time `json:"last_transition_at,omitempty"`
	// DawnTime / DuskTime are the configured "HH:MM" phase boundaries (in the
	// village timezone). The client has read these keys off this DTO since the
	// editor-panel schedule prepopulation shipped, but the fields never existed
	// — it silently fell back to defaults (LLM-578 adjacent gap). Omitted when
	// the boundaries didn't parse at snapshot publish (DawnDuskMinuteOK false).
	DawnTime string `json:"dawn_time,omitempty"`
	DuskTime string `json:"dusk_time,omitempty"`
	// Camera zoom floors (min zoom-out) — different for admins vs regular
	// users. Every client reads its floor from here; the admin config panel
	// reads+writes them via GET/POST /api/village/config + /admin/zoom-settings.
	ZoomMinAdmin   float64 `json:"zoom_min_admin"`
	ZoomMinRegular float64 `json:"zoom_min_regular"`
}

// WorldConfigDTO is the GET /api/village/config response — the admin-only
// world-config read surface the config panel renders. Distinct from
// WorldStateDTO (the public, hot-path world poll) so admin/operator config
// stays off every client's per-frame fetch. The read is admin-gated
// (Actor.IsAdmin, resolved on the world goroutine) and runs through the command
// channel so it reads live w.Settings / w.Environment with no snapshot lag.
//
// The "World clock" readout comes from WorldStateDTO.Now on the public /world
// poll (not duplicated here). Last/NextTransitionAt bracket the day↔night cycle;
// Last/NextRotationAt bracket the daily asset rotation. DawnTime/DuskTime/
// RotationTime are the configured HH:MM boundaries (in Timezone). ZoomMin* are
// echoed so the panel's edit fields prepopulate without a second fetch.
type WorldConfigDTO struct {
	Timezone            string    `json:"timezone"`
	LastTransitionAt    time.Time `json:"last_transition_at"`
	NextTransitionAt    time.Time `json:"next_transition_at"`
	NextTransitionPhase string    `json:"next_transition_phase"`
	DawnTime            string    `json:"dawn_time"`
	DuskTime            string    `json:"dusk_time"`
	RotationTime        string    `json:"rotation_time"`
	LastRotationAt      time.Time `json:"last_rotation_at"`
	NextRotationAt      time.Time `json:"next_rotation_at"`
	AgentTicksPaused    bool      `json:"agent_ticks_paused"`
	ZoomMinAdmin        float64   `json:"zoom_min_admin"`
	ZoomMinRegular      float64   `json:"zoom_min_regular"`
	// TownChestCoins is the estate rate's chest (LLM-652): coin the daily
	// assessment has taken out of resident purses and not yet spent back. Read
	// side only — the coin sweep needs it to check Σ purses + chest.
	TownChestCoins int `json:"town_chest_coins"`
}

// AgentDTO is one actor in the GET /api/village/agents response.
//
// Sprite is the resolved character sprite inlined from the npc_sprite catalog
// (World.Sprites) keyed by the actor's SpriteID — sheet + frame dims +
// animation rows, everything the client needs to build the AnimatedSprite2D
// from this one fetch. Absent (omitempty) for actors with no sprite_id or a
// dangling ref. Facing is the spawn/initial render direction; the client
// derives live facing from movement delta, so this only seeds the first
// render. Facing is ALWAYS present and normalized to a valid enum member
// ("south" when unset) so the wire shape is identical for pg-loaded and
// in-memory-spawned actors (a pg actor's facing column is NOT NULL default
// 'south').
type AgentDTO struct {
	ID                string          `json:"id"`
	DisplayName       string          `json:"display_name"`
	Kind              string          `json:"kind"`  // npc_stateful | npc_shared | pc | decorative
	State             string          `json:"state"` // idle | walking | conversing | ...
	Role              string          `json:"role,omitempty"`
	LLMAgent          string          `json:"llm_memory_agent,omitempty"` // backing VA; editor agent-picker keys on it (absent for actors with no VA)
	X                 int             `json:"x"`                          // tile coordinate (actors move on the integer grid)
	Y                 int             `json:"y"`
	Facing            string          `json:"facing"` // north | south | east | west (spawn facing; "south" when unset)
	InsideStructureID string          `json:"inside_structure_id,omitempty"`
	CurrentHuddleID   string          `json:"current_huddle_id,omitempty"`
	Sprite            *AgentSpriteDTO `json:"sprite,omitempty"`

	// Live needs (ZBBS-HOME-462) — current hunger/thirst/tiredness in [0, NeedMax].
	// The editor's per-NPC needs readout renders these; v1's /npcs row carried them
	// but the v2 port dropped them, so the panel defaulted to 0/0/0. NOT omitempty —
	// a need of 0 is a real value, and a fresh npc_created actor correctly reports
	// 0/0/0. Live updates ride the npc_needs_changed frame (World.emitNeedsDeltas).
	Hunger    int `json:"hunger"`
	Thirst    int `json:"thirst"`
	Tiredness int `json:"tiredness"`

	// Coins (LLM-70) — the actor's current purse balance, rendered beside the
	// needs on the editor's villager-list row. NOT omitempty (0 is a real
	// balance). This is the load-time / refresh value; live updates ride the
	// npc_coins_changed frame (World.emitCoinsDeltas, LLM-71), same posture as
	// needs.
	Coins int `json:"coins"`

	// Editor metadata (ZBBS-HOME-290) — the NPC config the Godot editor/HUD
	// shows + edits, ported from v1. Additive, no contract_version bump.
	// Attributes is the sorted slug set the editor renders as behavior chips
	// (display names resolved client-side from /api/village/npc-behaviors);
	// omitempty so an attribute-less NPC just omits the key. Home/WorkStructureID
	// are the "Lives at / Works at" anchors (omitempty when unset). Schedule
	// *Minute is the work-shift window in minute-of-day; emitted as explicit null
	// (NOT omitempty) when unset so the editor can tell "inherit dawn/dusk" (null)
	// from a real value — matching the loiter-offset pointer convention on
	// ObjectDTO.
	Attributes       []string `json:"attributes,omitempty"`
	HomeStructureID  string   `json:"home_structure_id,omitempty"`
	WorkStructureID  string   `json:"work_structure_id,omitempty"`
	ScheduleStartMin *int     `json:"schedule_start_minute"`
	ScheduleEndMin   *int     `json:"schedule_end_minute"`

	// In-flight source activity (LLM-441) — the load-time / refresh value of the
	// actor's timed repair/stoke/harvest window, so a client connecting mid-window
	// draws the tooltip "busy" line immediately (live open/close rides the
	// npc_source_activity_changed frame, same posture as needs/coins). Both
	// omitempty: an idle actor omits both. Kind is the wire form (repair/stoke/
	// harvest), gated to the rendered kinds; Label is the resolved place name,
	// non-empty only for a repair (stoke/harvest render place-less client-side).
	SourceActivityKind  string `json:"source_activity_kind,omitempty"`
	SourceActivityLabel string `json:"source_activity_label,omitempty"`
}

// AgentSpriteDTO is the resolved character sprite inlined onto an AgentDTO.
// It is the render subset of the sprite catalog entry (no pack — the inline
// view carries only what the client needs to draw + animate the actor). The
// raw catalog (with pack) is available at GET /api/village/sprites.
type AgentSpriteDTO struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Sheet       string               `json:"sheet"`
	FrameWidth  int                  `json:"frame_width"`
	FrameHeight int                  `json:"frame_height"`
	Animations  []SpriteAnimationDTO `json:"animations"`
	// Behaviors are the sprite's engine-behavior slugs (LLM-579). The client
	// keys render decisions on them — a "waterfowl" sprite gets the ground
	// decal (ripple on water / shadow on land) and the swim animation family.
	Behaviors []string `json:"behaviors,omitempty"`
	// RenderScale is the client draw scale for this sprite (LLM-580):
	// villagers 2.0, ducks 1.0. Omitted when zero (a test-seeded sprite);
	// the client falls back to its default.
	RenderScale float64 `json:"render_scale,omitempty"`
}

// ObjectDTO is one placed village object in the GET /api/village/objects
// response. In v2 a building is both a village_object AND a sim.Structure
// sharing an ID (the shared-identity bridge); this surfaces the village_object
// half — position, asset, visual state.
type ObjectDTO struct {
	ID           string   `json:"id"`
	AssetID      string   `json:"asset_id"`
	X            float64  `json:"x"`
	Y            float64  `json:"y"`
	CurrentState string   `json:"current_state,omitempty"`
	DisplayName  string   `json:"display_name,omitempty"`
	Tags         []string `json:"tags,omitempty"`

	// Editor metadata (ZBBS-HOME-289). Owner / PlacedBy / EntryPolicy are
	// admin-only labels — the editor sets them via the admin routes and reads
	// current state here. LoiterOffsetX/Y are the raw per-instance override
	// (null = no override; emitted as null, not omitted, so the editor can tell
	// "cleared" from absent). EffectiveLoiterOffsetX/Y are the SERVER-computed
	// resolved offset (tile units relative to the anchor) the editor renders the
	// loiter pin at — single source of truth with the engine's walk resolver, so
	// the pin lands where visitors actually park (see sim.EffectiveLoiterOffset).
	Owner                  string `json:"owner,omitempty"`
	PlacedBy               string `json:"placed_by,omitempty"`
	EntryPolicy            string `json:"entry_policy,omitempty"`
	LoiterOffsetX          *int   `json:"loiter_offset_x"`
	LoiterOffsetY          *int   `json:"loiter_offset_y"`
	EffectiveLoiterOffsetX int    `json:"effective_loiter_offset_x"`
	EffectiveLoiterOffsetY int    `json:"effective_loiter_offset_y"`

	// HasInterior reports whether this placement also has a paired Structure
	// (the shared-identity bridge — engine/sim/structure_anchors.go). True for
	// buildings (inns, houses, taverns, anything with rooms / actors that
	// bind via Inside/Home/WorkStructureID) and for legacy props that today
	// carry a Structure shell to make structure_enter resolve (noticeboards).
	// False for bare placements (wells, lamps, gather piles, future props
	// with no interior). The client uses this to dispatch the right /pc/move
	// destination kind on a click: true → structure_enter (walk inside),
	// false → object_visit (walk to the object's loiter slot, ZBBS-WORK-351).
	HasInterior bool `json:"has_interior"`

	// Noticeboard content (ZBBS-HOME-291) — the cascade-authored prose for a
	// notice board, surfaced for the editor's content modal. Both omitempty so a
	// non-noticeboard (or a board with no authored content yet) carries neither.
	// Sourced from the snapshot's NoticeboardContent map by object id; the
	// engine-internal AtState (the SaveNoticeboardContent stale-guard) is NOT on
	// the wire — the client just renders the current text. Additive, no version bump.
	ContentText     string     `json:"content_text,omitempty"`
	ContentPostedAt *time.Time `json:"content_posted_at,omitempty"`

	// Refreshes — the object's per-attribute need-decrement-on-arrival policies,
	// surfaced for the editor's refresh panel (it has no standalone GET; the v2
	// read path is here). Same wire shape the admin/object/set-refresh route
	// accepts and echoes (adminObjectRefreshRow), incl. the dwell-recovery fields.
	// Omitted for the common case of an object with no refreshes. Additive read
	// field, no contract-version bump (same posture as the HOME-289/291 fields).
	Refreshes []adminObjectRefreshRow `json:"refreshes,omitempty"`
}

// TerrainDTO is the GET /api/village/terrain response. The terrain grid is a
// fixed-size row-major byte array (one byte per tile, one of the frozen 6
// terrain-type values 1..6) base64-encoded into Data. The client decodes Data
// into a PackedByteArray and indexes it as data[y*map_w + x]. The byte->visual
// mapping is 100% client-side (a wang-corner blend renderer), so the server
// ships only the grid + the metadata needed to place it; no legend.
type TerrainDTO struct {
	ContractVersion int    `json:"contract_version"`
	MapW            int    `json:"map_w"` // grid width in tiles (row stride)
	MapH            int    `json:"map_h"` // grid height in tiles
	PadX            int    `json:"pad_x"` // world (0,0) maps to internal tile (pad_x, pad_y)
	PadY            int    `json:"pad_y"`
	TileSize        int    `json:"tile_size"` // world pixels per tile
	Data            string `json:"data"`      // base64 of the flat map_w*map_h byte grid
}

// AssetDTO is one catalog entry in the GET /api/village/assets response — the
// definition of a placeable thing (a tree, a market stall, a building). It is
// the render graph the client needs to draw + animate an object instance;
// engine-only behavior fields (rotation_algo, transition_spread_seconds,
// occupied_*, is_obstacle, is_passage) are intentionally absent. This is the
// object/terrain catalog only — character sprites live in a separate catalog
// (SpriteDTO, served at GET /api/village/sprites + inlined onto AgentDTO).
type AssetDTO struct {
	ID                string          `json:"id"` // asset.id UUID; ObjectDTO.asset_id references it
	Name              string          `json:"name"`
	Category          string          `json:"category"` // tree | nature | structure | prop
	DefaultState      string          `json:"default_state"`
	AnchorX           float64         `json:"anchor_x"`
	AnchorY           float64         `json:"anchor_y"`
	Layer             string          `json:"layer"`        // objects | above
	ZIndex            int             `json:"z_index"`      // Godot CanvasItem z; <0 renders below NPCs
	RenderScale       float64         `json:"render_scale"` // client draw scale (LLM-599): 2.0 default, tuned per asset
	VisibleWhenInside bool            `json:"visible_when_inside"`
	StandOffsetX      *int            `json:"stand_offset_x,omitempty"`
	StandOffsetY      *int            `json:"stand_offset_y,omitempty"`
	DoorOffsetX       *int            `json:"door_offset_x,omitempty"`
	DoorOffsetY       *int            `json:"door_offset_y,omitempty"`
	Footprint         FootprintDTO    `json:"footprint"`
	FitsSlot          *string         `json:"fits_slot,omitempty"` // overlay assets: which slot they snap into
	Pack              *TilesetPackDTO `json:"pack,omitempty"`
	States            []AssetStateDTO `json:"states"`
	Slots             []AssetSlotDTO  `json:"slots,omitempty"`
}

// FootprintDTO is the per-side tile footprint (counts from the anchor tile in
// each cardinal direction; the anchor tile is always included).
type FootprintDTO struct {
	Left   int `json:"left"`
	Right  int `json:"right"`
	Top    int `json:"top"`
	Bottom int `json:"bottom"`
}

// TilesetPackDTO is the source tileset an asset's sheets came from.
type TilesetPackDTO struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	URL  *string `json:"url,omitempty"`
}

// AssetStateDTO is one visual variant of an asset (e.g. "open"/"closed",
// "lit"/"unlit"). Animated states have frame_count > 1 (frames are consecutive
// horizontally in the sheet starting at src_x/src_y).
type AssetStateDTO struct {
	State      string         `json:"state"`
	Sheet      string         `json:"sheet"`
	SrcX       int            `json:"src_x"`
	SrcY       int            `json:"src_y"`
	SrcW       int            `json:"src_w"`
	SrcH       int            `json:"src_h"`
	FrameCount int            `json:"frame_count"`
	FrameRate  float64        `json:"frame_rate"`
	Tags       []string       `json:"tags,omitempty"`
	Light      *AssetLightDTO `json:"light,omitempty"` // present only on light-emitting states
}

// AssetLightDTO are the PointLight2D parameters for a light-emitting state.
type AssetLightDTO struct {
	Color            string  `json:"color"`  // hex #RRGGBB
	Radius           int     `json:"radius"` // world pixels
	Energy           float64 `json:"energy"`
	OffsetX          int     `json:"offset_x"`
	OffsetY          int     `json:"offset_y"`
	FlickerAmplitude float64 `json:"flicker_amplitude"` // 0 = steady
	FlickerPeriodMs  int     `json:"flicker_period_ms"`
}

// AssetSlotDTO is a named attachment point on an asset (e.g. a campfire's "top"
// slot where a pot can be placed). Overlay assets declare which slot they fit
// via AssetDTO.FitsSlot.
type AssetSlotDTO struct {
	SlotName string `json:"slot_name"`
	OffsetX  int    `json:"offset_x"`
	OffsetY  int    `json:"offset_y"`
}

// SpriteDTO is one entry in the GET /api/village/sprites response — the raw
// character-sprite catalog the editor's sprite picker renders. It is a
// SEPARATE catalog from AssetDTO: character sprites use a row-indexed
// directional animation model (direction × animation → row_index), unlike an
// asset state's single src-rect. AgentDTO references a sprite by id and (in
// V3) inlines the resolved sheet + animations so the client can build the
// AnimatedSprite2D from one agents fetch.
type SpriteDTO struct {
	ID          string               `json:"id"` // npc_sprite.id UUID; Actor.sprite_id references it
	Name        string               `json:"name"`
	Sheet       string               `json:"sheet"`
	FrameWidth  int                  `json:"frame_width"`
	FrameHeight int                  `json:"frame_height"`
	Pack        *TilesetPackDTO      `json:"pack,omitempty"`
	Animations  []SpriteAnimationDTO `json:"animations"`
	Behaviors   []string             `json:"behaviors,omitempty"`    // engine-behavior slugs (LLM-579), e.g. "waterfowl"
	RenderScale float64              `json:"render_scale,omitempty"` // client draw scale (LLM-580): villagers 2.0, ducks 1.0
}

// SpriteAnimationDTO is one (direction, animation) row mapping into a sprite
// sheet. RowIndex is the 0-indexed sheet row; frames run left-to-right from
// column 0 through frame_count-1. Direction is north/south/east/west and
// Animation is idle/walk.
type SpriteAnimationDTO struct {
	Direction  string  `json:"direction"`
	Animation  string  `json:"animation"`
	RowIndex   int     `json:"row_index"`
	FrameCount int     `json:"frame_count"`
	FrameRate  float64 `json:"frame_rate"`
}

// NPCBehaviorDTO is one entry in the GET /api/village/npc-behaviors response —
// the actor-assignable attribute catalog the editor's "add attribute" dropdown
// renders. The endpoint and DTO keep the historical "behavior" label for
// URL/wire-format stability (the legacy npc_behavior allowlist table was
// retired in ZBBS-113); the data is sourced from attribute_definition (scope
// actor/both) via World.AttributeDefinitions.
type NPCBehaviorDTO struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// RefreshAttributeDTO is one entry in the GET /api/village/refresh-attributes
// response — the need catalog the refresh editor's attribute dropdown renders.
// Sourced from the frozen sim.Needs registry; Name is the NeedKey the
// set-refresh route validates against (sim.FindNeed). The v2 replacement for
// v1's /api/refresh-attributes.
type RefreshAttributeDTO struct {
	Name         string `json:"name"`
	DisplayLabel string `json:"display_label"`
}

// actorKindString maps the internal ActorKind enum to its stable wire form.
// A new enum value renders as "unknown" rather than leaking the int.
func actorKindString(k sim.ActorKind) string {
	switch k {
	case sim.KindNPCStateful:
		return "npc_stateful"
	case sim.KindNPCShared:
		return "npc_shared"
	case sim.KindPC:
		return "pc"
	case sim.KindDecorative:
		return "decorative"
	default:
		return "unknown"
	}
}
