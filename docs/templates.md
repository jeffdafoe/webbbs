# ZBBS Template System

Templates are `.ans` files that control how screens look in the terminal client. They support ANSI art, pipe color codes, hex colors, variable macros, and widget markers for interactive elements.

## File Location

Templates live in `clients/terminal/templates/`. Each file is named after the screen it represents:

```
templates/
    welcome.ans     - Welcome/login screen
    main-menu.ans   - Main menu after login
    goodbye.ans     - Logoff screen
```

Filenames are lowercase, hyphenated for multi-word names. The `.ans` extension is standard ANSI art format — files created in PabloDraw, TheDraw, or ACiDDraw work as-is.

## Pipe Color Codes

Format: `|XX` where XX is a two-digit hex code.

### Foreground Colors (|00 - |0F)

```
|00  Black           |08  Dark Gray
|01  Blue            |09  Light Blue
|02  Green           |0A  Light Green
|03  Cyan            |0B  Light Cyan
|04  Red             |0C  Light Red
|05  Magenta         |0D  Light Magenta
|06  Brown           |0E  Yellow
|07  Light Gray      |0F  White
```

### Background Colors (|10 - |17)

```
|10  Black           |14  Red
|11  Blue            |15  Magenta
|12  Green           |16  Brown
|13  Cyan            |17  Light Gray
```

### How They Work

Foreground codes reset all attributes first, then set the color. This prevents bold/color bleed from previous text. Background codes only set the background without resetting, so you can combine them with foreground codes:

```
|0F|11Hello World
```

This produces white text on a blue background. The `|0F` sets bright white foreground (resetting everything first), then `|11` adds blue background on top.

### Note on Hex vs Decimal

These are hex digits (0-9, A-F), not decimal. `|0F` is code 15 (white), `|10` is code 16 (black background). This follows the Renegade BBS pipe code tradition but uses hex for the DOS 16-color palette.

## Hex Colors

Format: `{#RRGGBB}` or `{#RGB}` for 24-bit color beyond the 16-color palette.

```
{#FF5500}        Orange foreground
{#333}           Dark gray foreground (shorthand)
{bg:#1a1a2e}     Dark blue background
{reset}          Reset all colors to default
```

These generate xterm.js 24-bit color sequences. Use them when the 16 pipe colors aren't enough. Works in any terminal that supports 24-bit color.

## Macros

Format: `@NAME@` — variable names between at-signs, uppercase.

### Available Macros

```
@BBSNAME@     BBS name from system settings
@TAGLINE@     BBS tagline
@PHONE@       BBS phone number
@USERNAME@    Current user's username
@ALIAS@       Current user's display alias
@ROLE@        Current user's role
@DATE@        Current date
@TIME@        Current time
@ONLINE@      Number of users online
```

### Width Specifier

Format: `@NAME:width@` — pad or truncate the value to an exact character width.

```
@BBSNAME:30@     BBS name, padded to 30 characters (or truncated if longer)
@USERNAME:15@    Username, exactly 15 characters wide
```

This is important for ANSI art layouts where you need values to occupy a fixed amount of space regardless of content. If the value is shorter than the width, it's right-padded with spaces. If longer, it's truncated.

### Centering

Format: `@CENTER:NAME:width@` — center the value within a given width.

```
@CENTER:BBSNAME:34@    BBS name, centered in 34 characters
@CENTER:TAGLINE@       Tagline, centered in 80 characters (default)
```

The width is optional — if omitted, it defaults to 80 (full terminal width). The output is padded with spaces on both sides to fill the specified width. If the value is longer than the width, it gets truncated.

### Padding

Format: `@PAD:width@` — emit an exact number of spaces.

```
@PAD:74@     74 spaces (useful for empty lines inside boxes)
@PAD:10@     10 spaces
```

No macro lookup — this is purely a spacing primitive. Useful when you need a precise number of spaces to fill a fixed-width area.

### Unrecognized Macros

Any `@NAME@` that doesn't match a known macro passes through as-is. This means you won't get broken output from typos — the literal text just shows up.

## Widget Markers

Format: `@WIDGET:TYPE:name:width:items@` — marks where an interactive element should render.

### Full Syntax

```
@WIDGET:TYPE:name@                              No width, no items
@WIDGET:TYPE:name:width@                        Width only
@WIDGET:TYPE:name:width:Item1,Item2,Item3@      Width and items
```

### Types

```
MENU        Lightbar selection menu
PROMPT      Text input field
LIGHTBAR    Lightbar list
```

### Width

The optional width parameter controls how many characters the widget slot occupies. If omitted, it defaults to 80 (full terminal width).

```
@WIDGET:MENU:login:40@      40-character wide menu slot
@WIDGET:MENU:login@         80-character wide (default)
```

### Menu Items

For `MENU` widgets, you can define the menu items directly in the template as a comma-separated list after the width. Each item can optionally specify a route in square brackets:

```
@WIDGET:MENU:login:74:Login,Register,Quit[disconnect]@
@WIDGET:MENU:home:74:Edit Profile[profile],Who Is Online[who],Logout[disconnect]@
```

Each item has:
- **Display label** — the text shown in the lightbar (e.g. "Edit Profile")
- **Route** — the action to invoke, specified in `[brackets]`. If omitted, defaults to the label lowercased (e.g. "Login" → `login`)
- **Hotkey** — the first character of the label (e.g. "Login" → "L")

Routes are registered in a central route table. When a user selects a menu item, the `ScreenRunner` looks up the route and calls its handler. Multiple menu items across different templates can point to the same route (e.g. both "Quit" and "Logout" can route to `disconnect`).

Items defined in the template can be supplemented by code at runtime — for example, role-specific items (like "User Management" for sysops) can be prepended with their route.

### How They Work

The template renderer records the cursor position (row, column) where the marker appears, then emits spaces to fill the widget slot width. The `ScreenRunner` class reads the widget positions and items, builds the lightbar menu, and dispatches to route handlers.

This means you design your ANSI art with space for the interactive element, put the marker where you want it to appear, and the code handles the rest. The spaces ensure the layout stays intact even before the widget renders.

### Example

```
|01  ╔═══════════════════════════════════════╗
|01  ║  |0FWelcome to @BBSNAME:30@          |01║
|01  ╠═══════════════════════════════════════╣
|01  ║@WIDGET:MENU:login:37:Login,Register,Quit[disconnect]@║
|01  ╚═══════════════════════════════════════╝
```

The login menu renders as a centered lightbar inside the box. Selecting "Login" invokes the `login` route, "Register" invokes `register`, and "Quit" invokes `disconnect`.

## Raw ANSI

Any ANSI escape sequences in the template pass through untouched to xterm.js. This means:

- Cursor positioning (`ESC[row;colH`)
- Cursor save/restore (`ESC[s` / `ESC[u`)
- Cursor movement (`ESC[nA`, `ESC[nB`, `ESC[nC`, `ESC[nD`)
- Screen clearing (`ESC[2J`)
- Any other standard ANSI sequences

ANSI art created in PabloDraw, TheDraw, or ACiDDraw works as-is. The template renderer only intercepts pipe codes (`|XX`), hex colors (`{...}`), and macros (`@...@`). Everything else goes straight to the terminal.

## Creating Templates

1. Create your ANSI art in PabloDraw, ACiDDraw, or any ANSI editor
2. Save as `.ans` format
3. Open in a text editor to add `@MACRO@` and `@WIDGET@` markers
4. Place the file in `clients/terminal/templates/`

When adding macros inside ANSI art, account for the width the macro value will consume. Use width specifiers (`@BBSNAME:20@`) to keep layouts predictable.
