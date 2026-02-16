import { Terminal } from '@xterm/xterm';
import { WebLinksAddon } from '@xterm/addon-web-links';
import '@xterm/xterm/css/xterm.css';
import { ApiClient } from './api-client';
import { LoginHandler } from './login-handler';
import { TemplateRenderer } from './template-renderer';
import { ScreenRunner, RouteHandler } from './screen-runner';
import { TerminalIO } from './terminal-io';
import { UserManagement } from './user-management';
import { ansi } from './ansi';
import welcomeTemplate from '../templates/welcome.ans';
import homeTemplate from '../templates/home.ans';
import statusBarTemplate from '../templates/statusbar.ans';

interface PublicSettings {
    bbs_name: string;
    bbs_tagline: string;
    bbs_phone: string;
    registration_enabled: string;
    terminal_border_color: string;
}

interface UserInfo {
    id: string;
    username: string;
    email: string | null;
    roles: string[];
    isActive: boolean;
    createdAt: string;
    lastLoginAt: string | null;
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
        cursor: '#f8f8f2',
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

async function initTerminal(): Promise<void> {
    await document.fonts.ready;

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
const io = new TerminalIO(terminal);
const renderer = new TemplateRenderer(terminal);
const loginHandler = new LoginHandler(terminal, apiClient);

let settings: PublicSettings | null = null;
let userInfo: UserInfo | null = null;
let currentScreen = 'welcome';

// --- Route table ---

const routes = new Map<string, RouteHandler>();

routes.set('login', async () => {
    const token = await loginHandler.login();
    if (token) {
        await fetchUserInfo();
        return 'screen:home';
    }
    return null;
});

routes.set('register', async () => {
    await loginHandler.register();
    return null;
});

routes.set('disconnect', async () => {
    await modemDisconnect();
    apiClient.clearToken();
    userInfo = null;
    return 'screen:welcome';
});

routes.set('users', async () => {
    const userManagement = new UserManagement(terminal, apiClient);
    await userManagement.run();
    return null;
});

const screenRunner = new ScreenRunner(terminal, routes);

// --- Screen rendering ---

function renderWelcome(): ReturnType<typeof renderer.render> {
    terminal.write(ansi.clearScreen);
    const result = renderer.render(welcomeTemplate, {
        BBSNAME: settings!.bbs_name,
        TAGLINE: settings!.bbs_tagline,
        PHONE: settings!.bbs_phone,
        SEPARATOR: '── ─ ──── ─ ──',
    });
    terminal.write(ansi.hideCursor);
    return result;
}

function renderHome(): ReturnType<typeof renderer.render> {
    terminal.write(ansi.clearScreen);

    let greeting = 'Main Menu';
    if (userInfo) {
        greeting = `Welcome, ${userInfo.username}`;
    }

    const result = renderer.render(homeTemplate, {
        BBSNAME: settings!.bbs_name,
        SEPARATOR: '── ─ ──── ─ ──',
        GREETING: greeting,
    });

    if (result.statusBar) {
        let left = ` ${settings!.bbs_name} Home`;
        let right = '';
        if (userInfo) {
            const role = getDisplayRole();
            right = `${userInfo.username} [${role}] `;
        }
        renderer.renderStatusBar(statusBarTemplate, {
            LEFT: left,
            RIGHT: right,
        }, result.statusBar);
    }

    terminal.write(ansi.hideCursor);

    // Inject sysop items
    const menuWidget = result.widgets.find(w => w.name === 'home' && w.type === 'menu');
    if (menuWidget && userInfo && userInfo.roles.includes('ROLE_SYSOP')) {
        menuWidget.items.unshift({ label: 'User Management', route: 'users' });
    }

    return result;
}

// --- Main loop ---

async function start(): Promise<void> {
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

    let needsRedraw = true;
    let lastResult: ReturnType<typeof renderHome> | null = null;

    while (true) {
        if (needsRedraw) {
            if (currentScreen === 'home') {
                lastResult = renderHome();
            } else {
                lastResult = renderWelcome();
            }
        }

        const action = await screenRunner.run(lastResult!.widgets);

        if (action === 'noop') {
            needsRedraw = false;
        } else if (action && action.startsWith('screen:')) {
            currentScreen = action.substring(7);
            needsRedraw = true;
        } else {
            needsRedraw = true;
        }
    }
}

// --- Helpers ---

async function fetchUserInfo(): Promise<void> {
    try {
        userInfo = await apiClient.get('/api/me') as UserInfo;
    } catch {
        io.error('  Failed to load user info.');
    }
}

function getDisplayRole(): string {
    if (!userInfo) {
        return 'User';
    }
    if (userInfo.roles.includes('ROLE_SYSOP')) {
        return 'Sysop';
    }
    if (userInfo.roles.includes('ROLE_MODERATOR')) {
        return 'Moderator';
    }
    if (userInfo.roles.includes('ROLE_MEMBER')) {
        return 'Member';
    }
    return 'User';
}

async function modemDisconnect(): Promise<void> {
    io.clear();
    terminal.writeln('');

    const hangup = '+++ATH';
    terminal.write('  ');
    for (let i = 0; i < hangup.length; i++) {
        await io.sleep(100);
        terminal.write(ansi.white + hangup[i] + ansi.reset);
    }

    await io.sleep(800);
    terminal.writeln('');
    terminal.writeln('');
    terminal.writeln('  ' + ansi.red + 'NO CARRIER' + ansi.reset);

    await io.sleep(600);
    terminal.writeln('');
    terminal.write('  ' + ansi.darkGray + '*click*' + ansi.reset);

    await io.sleep(1500);
    terminal.writeln('');
    terminal.writeln('');
    await io.waitForKey();

    // ATDT redial sequence
    io.clear();
    terminal.writeln('');
    const phone = settings!.bbs_phone;
    const dialString = `ATDT ${phone}`;
    terminal.write('  ');
    for (let i = 0; i < dialString.length; i++) {
        await io.sleep(100);
        terminal.write(ansi.white + dialString[i] + ansi.reset);
    }

    await io.sleep(800);
    terminal.writeln('');
    terminal.writeln('');
    terminal.writeln('  ' + ansi.lightGray + 'CONNECT 9600' + ansi.reset);
    await io.sleep(1000);
}
