import { Terminal } from '@xterm/xterm';
import { ApiClient } from './api-client';
import { TerminalIO, MenuItem } from './terminal-io';
import { TemplateRenderer } from './template-renderer';
import { UserManagement } from './user-management';
import { ansi } from './ansi';
import homeTemplate from '../templates/home.ans';
import statusBarTemplate from '../templates/statusbar.ans';

interface UserInfo {
    id: string;
    username: string;
    email: string | null;
    roles: string[];
    isActive: boolean;
    createdAt: string;
    lastLoginAt: string | null;
}

interface PublicSettings {
    bbs_name: string;
    bbs_tagline: string;
    bbs_phone: string;
    registration_enabled: string;
}

export class MainMenu {
    private io: TerminalIO;
    private userInfo: UserInfo | null = null;

    constructor(
        private terminal: Terminal,
        private apiClient: ApiClient,
        private settings: PublicSettings
    ) {
        this.io = new TerminalIO(terminal);
    }

    async run(): Promise<void> {
        await this.fetchUserInfo();
        const renderer = new TemplateRenderer(this.terminal);

        while (true) {
            this.terminal.write(ansi.clearScreen);

            let greeting = 'Main Menu';
            if (this.userInfo) {
                greeting = `Welcome, ${this.userInfo.username}`;
            }

            const result = renderer.render(homeTemplate, {
                BBSNAME: this.settings.bbs_name,
                SEPARATOR: '── ─ ──── ─ ──',
                GREETING: greeting,
            });

            if (result.statusBar) {
                let left = ` ${this.settings.bbs_name} Home`;
                let right = '';
                if (this.userInfo) {
                    const role = this.getDisplayRole();
                    right = `${this.userInfo.username} [${role}] `;
                }
                renderer.renderStatusBar(statusBarTemplate, {
                    LEFT: left,
                    RIGHT: right,
                }, result.statusBar);
            }

            this.terminal.write(ansi.hideCursor);

            const items: MenuItem[] = [];

            if (this.isSysop()) {
                items.push({ label: 'User Management', value: 'users', hotkey: 'U' });
            }
            items.push({ label: 'Edit Profile', value: 'profile', hotkey: 'P' });
            items.push({ label: 'Who Is Online', value: 'who', hotkey: 'W' });
            items.push({ label: 'Logout', value: 'logout', hotkey: 'Q' });

            const homeWidget = result.widgets.find(w => w.name === 'home' && w.type === 'menu');
            const menuPosition = homeWidget ? { row: homeWidget.row, col: homeWidget.col, width: homeWidget.width } : undefined;

            const choice = await this.io.lightbar(items, { position: menuPosition });

            if (choice === 'users') {
                const userManagement = new UserManagement(this.terminal, this.apiClient);
                await userManagement.run();
            } else if (choice === 'profile') {
                this.io.info('  Edit Profile - coming soon');
                await this.io.pause();
            } else if (choice === 'who') {
                this.io.info('  Who Is Online - coming soon');
                await this.io.pause();
            } else {
                await this.modemDisconnect();
                return;
            }
        }
    }

    private async fetchUserInfo(): Promise<void> {
        try {
            this.userInfo = await this.apiClient.get('/api/me') as UserInfo;
        } catch {
            this.io.error('  Failed to load user info.');
        }
    }

    private isSysop(): boolean {
        if (!this.userInfo) {
            return false;
        }
        return this.userInfo.roles.includes('ROLE_SYSOP');
    }

    private async modemDisconnect(): Promise<void> {
        this.io.clear();
        this.terminal.writeln('');

        // +++ATH hangup sequence
        const hangup = '+++ATH';
        this.terminal.write('  ');
        for (let i = 0; i < hangup.length; i++) {
            await this.sleep(100);
            this.terminal.write(ansi.white + hangup[i] + ansi.reset);
        }

        await this.sleep(800);
        this.terminal.writeln('');
        this.terminal.writeln('');
        this.terminal.writeln('  ' + ansi.red + 'NO CARRIER' + ansi.reset);

        await this.sleep(600);
        this.terminal.writeln('');
        this.terminal.write('  ' + ansi.darkGray + '*click*' + ansi.reset);

        await this.sleep(1500);
        this.terminal.writeln('');
        this.terminal.writeln('');
        await this.io.waitForKey();

        // ATDT redial sequence
        this.io.clear();
        this.terminal.writeln('');
        const phone = this.settings.bbs_phone;
        const dialString = `ATDT ${phone}`;
        this.terminal.write('  ');
        for (let i = 0; i < dialString.length; i++) {
            await this.sleep(100);
            this.terminal.write(ansi.white + dialString[i] + ansi.reset);
        }

        await this.sleep(800);
        this.terminal.writeln('');
        this.terminal.writeln('');
        this.terminal.writeln('  ' + ansi.lightGray + 'CONNECT 9600' + ansi.reset);
        await this.sleep(1000);
    }

    private sleep(milliseconds: number): Promise<void> {
        return new Promise((resolve) => setTimeout(resolve, milliseconds));
    }

    private getDisplayRole(): string {
        if (!this.userInfo) {
            return 'User';
        }
        if (this.userInfo.roles.includes('ROLE_SYSOP')) {
            return 'Sysop';
        }
        if (this.userInfo.roles.includes('ROLE_MODERATOR')) {
            return 'Moderator';
        }
        if (this.userInfo.roles.includes('ROLE_MEMBER')) {
            return 'Member';
        }
        return 'User';
    }
}
