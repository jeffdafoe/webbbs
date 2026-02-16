const ESC = '\x1b[';

// Every color resets first (ESC[0;...) to prevent attribute bleed.
// This matches ZChat's EmulationColorString pattern from GENDOOR.PAS.

// Standard DOS 16-color foreground palette (0-F)
// Using ANSI 90-97 for bright colors instead of bold+30-37
export const ansi = {
    // Reset
    reset: ESC + '0m',

    // Foreground colors (0-7: normal)
    black: ESC + '0;30m',
    blue: ESC + '0;34m',
    green: ESC + '0;32m',
    cyan: ESC + '0;36m',
    red: ESC + '0;31m',
    magenta: ESC + '0;35m',
    brown: ESC + '0;33m',
    lightGray: ESC + '0;37m',

    // Foreground colors (8-F: bright/high-intensity)
    darkGray: ESC + '0;90m',
    lightBlue: ESC + '0;94m',
    lightGreen: ESC + '0;92m',
    lightCyan: ESC + '0;96m',
    lightRed: ESC + '0;91m',
    lightMagenta: ESC + '0;95m',
    yellow: ESC + '0;93m',
    white: ESC + '0;97m',

    // Background colors (0-7)
    bgBlack: ESC + '40m',
    bgBlue: ESC + '44m',
    bgGreen: ESC + '42m',
    bgCyan: ESC + '46m',
    bgRed: ESC + '41m',
    bgMagenta: ESC + '45m',
    bgBrown: ESC + '43m',
    bgLightGray: ESC + '47m',

    // Background colors (8-F: bright)
    bgDarkGray: ESC + '100m',
    bgLightBlue: ESC + '104m',
    bgLightGreen: ESC + '102m',
    bgLightCyan: ESC + '106m',
    bgLightRed: ESC + '101m',
    bgLightMagenta: ESC + '105m',
    bgYellow: ESC + '103m',
    bgWhite: ESC + '107m',

    // Cursor and screen control
    clearScreen: ESC + '2J' + ESC + 'H',
    clearToEnd: ESC + '0J',
    cursorUp: (n: number) => ESC + n + 'A',
    cursorDown: (n: number) => ESC + n + 'B',
    cursorForward: (n: number) => ESC + n + 'C',
    cursorBack: (n: number) => ESC + n + 'D',
    cursorTo: (row: number, col: number) => ESC + row + ';' + col + 'H',
    hideCursor: ESC + '?25l',
    showCursor: ESC + '?25h',
};

// Combined foreground + background (resets first, then sets both)
export function ansiFgBg(fg: string, bg: string): string {
    return ansi.reset + fg + bg;
}
