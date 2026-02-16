import { Terminal } from '@xterm/xterm';
import { ansi } from './ansi';

export interface WidgetPosition {
    type: 'menu' | 'prompt' | 'lightbar';
    name: string;
    row: number;
    col: number;
    width: number;
}

export interface RenderResult {
    widgets: WidgetPosition[];
    statusBar: 'top' | 'bottom' | null;
}

interface CursorState {
    row: number;
    col: number;
    savedRow: number;
    savedCol: number;
}

// Pipe code |XX -> ANSI escape sequence
// Foreground codes reset first to prevent attribute bleed (ZChat EmulationColorString pattern)
// Background codes don't reset so they can combine with existing fg
const PIPE_FOREGROUND: Record<string, string> = {
    '00': '\x1b[0;30m',
    '01': '\x1b[0;34m',
    '02': '\x1b[0;32m',
    '03': '\x1b[0;36m',
    '04': '\x1b[0;31m',
    '05': '\x1b[0;35m',
    '06': '\x1b[0;33m',
    '07': '\x1b[0;37m',
    '08': '\x1b[0;90m',
    '09': '\x1b[0;94m',
    '0A': '\x1b[0;92m',
    '0B': '\x1b[0;96m',
    '0C': '\x1b[0;91m',
    '0D': '\x1b[0;95m',
    '0E': '\x1b[0;93m',
    '0F': '\x1b[0;97m',
};

const PIPE_BACKGROUND: Record<string, string> = {
    '10': '\x1b[40m',
    '11': '\x1b[44m',
    '12': '\x1b[42m',
    '13': '\x1b[46m',
    '14': '\x1b[41m',
    '15': '\x1b[45m',
    '16': '\x1b[43m',
    '17': '\x1b[47m',
};

function isHexDigit(char: string): boolean {
    return /^[0-9A-Fa-f]$/.test(char);
}

function parseHexColor(hex: string): { r: number; g: number; b: number } | null {
    if (hex.length === 3) {
        const r = parseInt(hex[0] + hex[0], 16);
        const g = parseInt(hex[1] + hex[1], 16);
        const b = parseInt(hex[2] + hex[2], 16);
        return { r, g, b };
    }
    if (hex.length === 6) {
        const r = parseInt(hex.substring(0, 2), 16);
        const g = parseInt(hex.substring(2, 4), 16);
        const b = parseInt(hex.substring(4, 6), 16);
        return { r, g, b };
    }
    return null;
}

export class TemplateRenderer {
    private terminal: Terminal;

    constructor(terminal: Terminal) {
        this.terminal = terminal;
    }

    render(template: string, macros: Record<string, string>): RenderResult {
        // Step 1: Detect and strip @STATUSBAR:position@
        let statusBar: 'top' | 'bottom' | null = null;
        let stripped = template;
        const statusBarMatch = template.match(/@STATUSBAR:(top|bottom)@\r?\n?/);
        if (statusBarMatch) {
            statusBar = statusBarMatch[1] as 'top' | 'bottom';
            stripped = template.replace(statusBarMatch[0], '');
        }

        // Step 2: Substitute macros (text replacement before rendering)
        const substituted = this.substituteMacros(stripped, macros);

        // Step 3: If statusbar is top, start rendering on row 2
        const widgets: WidgetPosition[] = [];
        const startRow = statusBar === 'top' ? 2 : 1;
        const cursor: CursorState = { row: startRow, col: 1, savedRow: startRow, savedCol: 1 };
        if (startRow > 1) {
            this.terminal.write(ansi.cursorTo(startRow, 1));
        }
        let i = 0;

        while (i < substituted.length) {
            // Check for pipe code: | followed by two hex digits
            if (substituted[i] === '|' && i + 2 < substituted.length) {
                const code = substituted[i + 1].toUpperCase() + substituted[i + 2].toUpperCase();
                if (isHexDigit(substituted[i + 1]) && isHexDigit(substituted[i + 2])) {
                    const fg = PIPE_FOREGROUND[code];
                    const bg = PIPE_BACKGROUND[code];
                    if (fg) {
                        this.terminal.write(fg);
                        i += 3;
                        continue;
                    }
                    if (bg) {
                        this.terminal.write(bg);
                        i += 3;
                        continue;
                    }
                }
            }

            // Check for curly brace hex color: {#RGB} or {#RRGGBB} or {bg:#RGB} or {reset}
            if (substituted[i] === '{') {
                const closeIndex = substituted.indexOf('}', i + 1);
                if (closeIndex !== -1) {
                    const content = substituted.substring(i + 1, closeIndex);
                    const ansiSequence = this.parseCurlyColor(content);
                    if (ansiSequence !== null) {
                        this.terminal.write(ansiSequence);
                        i = closeIndex + 1;
                        continue;
                    }
                }
            }

            // Check for widget marker: @WIDGET:type:name@ or @WIDGET:type:name:width@
            if (substituted[i] === '@' && substituted.substring(i + 1, i + 8) === 'WIDGET:') {
                const closeIndex = substituted.indexOf('@', i + 1);
                if (closeIndex !== -1) {
                    const inner = substituted.substring(i + 8, closeIndex);
                    const parts = inner.split(':');
                    if (parts.length >= 2) {
                        const widgetType = parts[0].toLowerCase();
                        const widgetName = parts[1];
                        const widgetWidth = parts.length >= 3 ? parseInt(parts[2], 10) : 80;
                        if (widgetType === 'menu' || widgetType === 'prompt' || widgetType === 'lightbar') {
                            widgets.push({
                                type: widgetType,
                                name: widgetName,
                                row: cursor.row,
                                col: cursor.col,
                                width: widgetWidth,
                            });
                            // Emit spaces to fill the widget slot
                            const spaces = ' '.repeat(widgetWidth);
                            this.terminal.write(spaces);
                            cursor.col += widgetWidth;
                        }
                    }
                    i = closeIndex + 1;
                    continue;
                }
            }

            // Check for ANSI escape sequence (passthrough, but track cursor)
            if (substituted[i] === '\x1b' && i + 1 < substituted.length && substituted[i + 1] === '[') {
                const result = this.parseAnsiSequence(substituted, i, cursor);
                this.terminal.write(substituted.substring(i, i + result.length));
                i += result.length;
                continue;
            }

            // Regular character — write and advance cursor
            if (substituted[i] === '\n') {
                this.terminal.write('\n');
                cursor.row++;
                cursor.col = 1;
                i++;
                continue;
            }

            if (substituted[i] === '\r') {
                this.terminal.write('\r');
                cursor.col = 1;
                i++;
                continue;
            }

            this.terminal.write(substituted[i]);
            cursor.col++;
            i++;
        }

        return { widgets, statusBar };
    }

    renderStatusBar(statusBarTemplate: string, macros: Record<string, string>, position: 'top' | 'bottom'): void {
        const row = position === 'top' ? 1 : 25;

        // First pass: substitute and render to measure visible length (without padding)
        const substituted = this.substituteMacros(statusBarTemplate, macros);

        let visibleCount = 0;
        let ansiOutput = '';
        let currentBg = '';
        let i = 0;
        while (i < substituted.length) {
            if (substituted[i] === '|' && i + 2 < substituted.length) {
                const code = substituted[i + 1].toUpperCase() + substituted[i + 2].toUpperCase();
                if (isHexDigit(substituted[i + 1]) && isHexDigit(substituted[i + 2])) {
                    const bg = PIPE_BACKGROUND[code];
                    if (bg) {
                        currentBg = bg;
                        ansiOutput += bg;
                        i += 3;
                        continue;
                    }
                    const fg = PIPE_FOREGROUND[code];
                    if (fg) {
                        ansiOutput += fg;
                        if (currentBg) {
                            ansiOutput += currentBg;
                        }
                        i += 3;
                        continue;
                    }
                }
            }
            if (substituted[i] === '\r' || substituted[i] === '\n') {
                i++;
                continue;
            }
            ansiOutput += substituted[i];
            visibleCount++;
            i++;
        }

        // Right-align: find where RIGHT value appears and insert padding before it
        const rightValue = macros['RIGHT'] || '';
        if (rightValue.length > 0 && visibleCount < 80) {
            const pad = 80 - visibleCount;
            const rightIndex = ansiOutput.lastIndexOf(rightValue);
            if (rightIndex > 0) {
                ansiOutput = ansiOutput.substring(0, rightIndex) + ' '.repeat(pad) + ansiOutput.substring(rightIndex);
            }
        } else if (visibleCount < 80) {
            ansiOutput += ' '.repeat(80 - visibleCount);
        }

        this.terminal.write(ansi.cursorTo(row, 1) + ansiOutput + ansi.reset);
    }

    private substituteMacros(template: string, macros: Record<string, string>): string {
        // @PAD:width@ — emit N spaces, no macro lookup
        let result = template.replace(/@PAD:(\d+)@/g, (_match, widthStr) => {
            return ' '.repeat(parseInt(widthStr, 10));
        });

        // @CENTER:NAME:width@, @CENTER:NAME@, @NAME:width@, or @NAME@
        // But NOT @WIDGET:...@
        result = result.replace(/@(CENTER:)?([A-Z_]+)(?::(\d+))?@/g, (match, centerPrefix, name, widthStr) => {
            if (name === 'WIDGET') {
                return match;
            }

            const value = macros[name];
            if (value === undefined) {
                return match;
            }

            // @CENTER:NAME:width@ or @CENTER:NAME@ — center the value
            if (centerPrefix) {
                const width = widthStr ? parseInt(widthStr, 10) : 80;
                const truncated = value.length > width ? value.substring(0, width) : value;
                const leftPad = Math.floor((width - truncated.length) / 2);
                const rightPad = width - truncated.length - leftPad;
                return ' '.repeat(leftPad) + truncated + ' '.repeat(rightPad);
            }

            // @NAME:width@ — left-align within width
            if (widthStr) {
                const width = parseInt(widthStr, 10);
                if (value.length > width) {
                    return value.substring(0, width);
                }
                return value.padEnd(width);
            }

            return value;
        });

        return result;
    }

    private parseCurlyColor(content: string): string | null {
        // {reset}
        if (content === 'reset') {
            return '\x1b[0m';
        }

        // {bg:#RRGGBB} or {bg:#RGB}
        if (content.startsWith('bg:#')) {
            const hex = content.substring(4);
            const color = parseHexColor(hex);
            if (color) {
                return `\x1b[48;2;${color.r};${color.g};${color.b}m`;
            }
            return null;
        }

        // {#RRGGBB} or {#RGB}
        if (content.startsWith('#')) {
            const hex = content.substring(1);
            const color = parseHexColor(hex);
            if (color) {
                return `\x1b[38;2;${color.r};${color.g};${color.b}m`;
            }
            return null;
        }

        return null;
    }

    private parseAnsiSequence(
        str: string,
        start: number,
        cursor: CursorState
    ): { length: number } {
        // start points to ESC, start+1 points to [
        let i = start + 2;
        let params = '';

        // Collect parameter bytes (digits and semicolons)
        while (i < str.length && ((str[i] >= '0' && str[i] <= '9') || str[i] === ';')) {
            params += str[i];
            i++;
        }

        if (i >= str.length) {
            return { length: i - start };
        }

        const finalByte = str[i];
        const length = i - start + 1;

        // Parse parameter values
        const paramValues = params.split(';').map(p => {
            if (p === '') {
                return 0;
            }
            return parseInt(p, 10);
        });

        switch (finalByte) {
            case 'H':
            case 'f':
                // Cursor position: ESC[row;colH
                cursor.row = paramValues[0] || 1;
                cursor.col = (paramValues.length > 1) ? (paramValues[1] || 1) : 1;
                break;
            case 'A':
                // Cursor up
                cursor.row = Math.max(1, cursor.row - (paramValues[0] || 1));
                break;
            case 'B':
                // Cursor down
                cursor.row += (paramValues[0] || 1);
                break;
            case 'C':
                // Cursor forward
                cursor.col += (paramValues[0] || 1);
                break;
            case 'D':
                // Cursor back
                cursor.col = Math.max(1, cursor.col - (paramValues[0] || 1));
                break;
            case 'E':
                // Cursor next line
                cursor.row += (paramValues[0] || 1);
                cursor.col = 1;
                break;
            case 'F':
                // Cursor previous line
                cursor.row = Math.max(1, cursor.row - (paramValues[0] || 1));
                cursor.col = 1;
                break;
            case 'G':
                // Cursor horizontal absolute
                cursor.col = paramValues[0] || 1;
                break;
            case 's':
                // Save cursor position
                cursor.savedRow = cursor.row;
                cursor.savedCol = cursor.col;
                break;
            case 'u':
                // Restore cursor position
                cursor.row = cursor.savedRow;
                cursor.col = cursor.savedCol;
                break;
            case 'J':
                // Erase display — doesn't move cursor, just pass through
                break;
            case 'K':
                // Erase line — doesn't move cursor
                break;
            case 'm':
                // SGR (color) — doesn't move cursor
                break;
        }

        return { length };
    }
}
