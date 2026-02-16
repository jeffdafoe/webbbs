import { Terminal } from '@xterm/xterm';
import { WebLinksAddon } from '@xterm/addon-web-links';
import '@xterm/xterm/css/xterm.css';
import { ApiClient } from './api-client';
import { LoginHandler } from './login-handler';
import { MainMenu } from './main-menu';
import { TemplateRenderer } from './template-renderer';
import { ansi } from './ansi';
import welcomeTemplate from '../templates/welcome.ans';

interface PublicSettings {
    bbs_name: string;
    bbs_tagline: string;
    bbs_phone: string;
    registration_enabled: string;
    terminal_border_color: string;
}

// Calculate fontSize to fill the viewport without CSS transform.
// The font has a 2:1 height:width ratio (cell is fontSize tall, fontSize/2 wide).
// Terminal needs 80 cols * (fontSize/2) wide and 25 rows * fontSize tall.
const borderSize = 3;
const margin = 16;
const availableWidth = window.innerWidth - (borderSize + margin) * 2;
const availableHeight = window.innerHeight - (borderSize + margin) * 2;
const fontSizeFromWidth = Math.floor(availableWidth / (80 / 2));
const fontSizeFromHeight = Math.floor(availableHeight / 25);
const fontSize = Math.min(fontSizeFromWidth, fontSizeFromHeight);

const terminal = new Terminal({
    fontFamily: "'ZBBS VGA', monospace",
    fontSize: fontSize,
    cols: 80,
    rows: 25,
    letterSpacing: 0,
    lineHeight: 1,
    theme: {
        background: '#0a0a1a',
        foreground: '#c0c0c0',
        cursor: '#e94560',
        cursorAccent: '#0a0a1a',
        red: '#e94560',
        green: '#50fa7b',
        yellow: '#f1fa8c',
        blue: '#6272a4',
        magenta: '#ff79c6',
        cyan: '#8be9fd',
        white: '#f8f8f2',
        brightRed: '#ff5555',
        brightGreen: '#69ff94',
        brightYellow: '#ffffa5',
        brightBlue: '#d6acff',
        brightMagenta: '#ff92df',
        brightCyan: '#a4ffff',
        brightWhite: '#ffffff'
    },
    cursorBlink: false,
    cursorStyle: 'bar',
    cursorInactiveStyle: 'none',
    scrollback: 0,
    allowProposedApi: true
});

terminal.loadAddon(new WebLinksAddon());

// Load font explicitly so OffscreenCanvas can use it, then open terminal
async function initTerminal(): Promise<void> {
    await document.fonts.ready;

    // Load font into FontFaceSet so canvas/OffscreenCanvas can access it
    const font = new FontFace('ZBBS VGA', "url('fonts/ZBBS_VGA.woff')");
    await font.load();
    document.fonts.add(font);

    const container = document.getElementById('terminal');
    if (container) {
        terminal.open(container);

        terminal.write(ansi.hideCursor);
        terminal.focus();
        document.addEventListener('click', () => terminal.focus());
        start();
    }
}
initTerminal();

const apiClient = new ApiClient();

async function start(): Promise<void> {
    let settings: PublicSettings | null = null;

    try {
        const fetched = await fetch('/api/settings/public');
        if (fetched.ok) {
            const data = await fetched.json();
            if (data.status === 'ready') {
                settings = data as PublicSettings;
            }
        }
    } catch {
        // API unreachable
    }

    if (!settings) {
        terminal.write(ansi.clearScreen);
        terminal.write('\r\n');
        terminal.write('  ' + ansi.white + 'ZBBS has not been configured yet.' + ansi.reset + '\r\n');
        terminal.write('\r\n');
        terminal.write('  Run ' + ansi.lightCyan + 'php bin/console zbbs:setup' + ansi.reset + ' on the server.\r\n');
        return;
    }

    const outer = document.getElementById('terminal-outer');
    if (outer && settings.terminal_border_color) {
        outer.style.background = settings.terminal_border_color;
    }

    const renderer = new TemplateRenderer(terminal);
    let lastResult: ReturnType<typeof renderer.render> = { widgets: [], statusBar: null };

    const renderWelcome = (): void => {
        terminal.write(ansi.clearScreen);
        lastResult = renderer.render(welcomeTemplate, {
            BBSNAME: settings.bbs_name,
            TAGLINE: settings.bbs_tagline,
            PHONE: settings.bbs_phone,
            SEPARATOR: '── ─ ──── ─ ──',
        });
        terminal.write(ansi.hideCursor);
    };

    while (true) {
        renderWelcome();

        const loginHandler = new LoginHandler(terminal, apiClient, renderWelcome, lastResult.widgets);
        const token = await loginHandler.run();

        if (token) {
            const mainMenu = new MainMenu(terminal, apiClient, settings);
            await mainMenu.run();
            apiClient.clearToken();
        }
    }
}

