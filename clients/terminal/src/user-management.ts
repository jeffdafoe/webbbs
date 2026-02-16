import { Terminal } from '@xterm/xterm';
import { ApiClient } from './api-client';
import { TerminalIO, ColumnDefinition } from './terminal-io';
import { ansi } from './ansi';

interface UserSummary {
    id: string;
    username: string;
    email: string | null;
    roles: string[];
    isActive: boolean;
    createdAt: string;
    lastLoginAt: string | null;
    alias: string | null;
}

interface UserProfile {
    alias: string | null;
    gender: string;
    entryMessage: string | null;
    exitMessage: string | null;
    bio: string | null;
    avatarUrl: string | null;
    preferredColor: string | null;
    timezone: string | null;
}

interface UserDetail extends UserSummary {
    profile: UserProfile;
}

interface UserListResponse {
    users: UserSummary[];
    total: number;
    page: number;
    limit: number;
}

export class UserManagement {
    private io: TerminalIO;

    constructor(
        private terminal: Terminal,
        private apiClient: ApiClient
    ) {
        this.io = new TerminalIO(terminal);
    }

    async run(): Promise<void> {
        while (true) {
            this.io.clear();
            this.io.statusBar(' User Management', 'ESC=Back ');
            this.terminal.writeln('');

            const choice = await this.io.lightbar([
                { label: 'List Users', value: 'list', hotkey: 'L' },
                { label: 'Search Users', value: 'search', hotkey: 'S' },
                { label: 'Back', value: 'back', hotkey: 'Q' }
            ]);

            if (choice === 'list') {
                await this.listUsers();
            } else if (choice === 'search') {
                await this.searchUsers();
            } else {
                return;
            }
        }
    }

    private async listUsers(search?: string): Promise<void> {
        let page = 1;
        const limit = 10;

        while (true) {
            this.io.clear();

            let path = `/api/users?page=${page}&limit=${limit}`;
            if (search) {
                path += `&search=${encodeURIComponent(search)}`;
            }

            let data: UserListResponse;
            try {
                data = await this.apiClient.get(path) as UserListResponse;
            } catch {
                this.io.error('  Failed to load users.');
                await this.io.pause();
                return;
            }

            if (data.users.length === 0) {
                this.io.info('  No users found.');
                await this.io.pause();
                return;
            }

            const totalPages = Math.ceil(data.total / limit);

            let statusRight = `Page ${page}/${totalPages}  ${data.total} users `;
            if (search) {
                statusRight = `"${search}"  ${statusRight}`;
            }
            this.io.statusBar(' User List', statusRight);
            this.terminal.writeln('');

            // Build display lines for lightbar selection
            const columns: ColumnDefinition[] = [
                { header: 'Username', width: 20 },
                { header: 'Role', width: 14 },
                { header: 'Active', width: 8 }
            ];

            const displayItems: string[] = [];
            for (let i = 0; i < data.users.length; i++) {
                const user = data.users[i];
                const active = user.isActive ? ansi.green + 'Yes' + ansi.reset : ansi.red + 'No' + ansi.reset;
                displayItems.push(this.io.formatColumns(columns, [
                    user.username,
                    this.getDisplayRole(user.roles),
                    active
                ]));
            }

            // Calculate same margin as lightbar will use
            const headerStr = this.io.formatColumnHeader(columns);
            const rowWidth = headerStr.length;
            const barWidth = rowWidth + 4;
            const leftMargin = Math.floor((80 - barWidth) / 2);
            const marginStr = ' '.repeat(leftMargin);
            this.terminal.writeln(marginStr + ansi.white + '  ' + headerStr + ansi.reset);
            this.terminal.writeln(ansi.darkGray + marginStr + '\u2500'.repeat(barWidth) + ansi.reset);

            // Add navigation items
            if (page < totalPages) {
                displayItems.push(ansi.cyan + '── Next Page ──' + ansi.reset);
            }
            if (page > 1) {
                displayItems.push(ansi.cyan + '── Prev Page ──' + ansi.reset);
            }
            displayItems.push(ansi.darkGray + '── Back ──' + ansi.reset);

            const selected = await this.io.lightbarList(displayItems);

            if (selected === -1) {
                return;
            }

            // Figure out what was selected
            const userCount = data.users.length;
            let navOffset = userCount;

            if (selected < userCount) {
                await this.editUser(data.users[selected].id);
            } else if (page < totalPages && selected === navOffset) {
                page++;
            } else if (page > 1 && selected === navOffset + (page < totalPages ? 1 : 0)) {
                page--;
            } else {
                return;
            }
        }
    }

    private async searchUsers(): Promise<void> {
        this.terminal.writeln('');
        const search = await this.io.prompt('  Search: ');
        if (!search) {
            return;
        }
        await this.listUsers(search);
    }

    private async editUser(userId: string): Promise<void> {
        let user: UserDetail;
        try {
            user = await this.apiClient.get(`/api/users/${userId}`) as UserDetail;
        } catch {
            this.io.error('  Failed to load user.');
            await this.io.pause();
            return;
        }

        while (true) {
            this.io.clear();
            this.io.statusBar(` Edit User: ${user.username}`, 'ESC=Back ');
            this.terminal.writeln('');

            this.terminal.writeln('  Username:   ' + ansi.white + user.username + ansi.reset);
            this.terminal.writeln('  Email:      ' + ansi.lightGray + (user.email || '(none)') + ansi.reset);
            this.terminal.writeln('  Role:       ' + ansi.brown + this.getDisplayRole(user.roles) + ansi.reset);
            const activeText = user.isActive ? ansi.green + 'Yes' + ansi.reset : ansi.red + 'No' + ansi.reset;
            this.terminal.writeln('  Active:     ' + activeText);
            this.terminal.writeln('  Created:    ' + ansi.lightGray + this.formatDate(user.createdAt) + ansi.reset);
            this.terminal.writeln('  Last Login: ' + ansi.lightGray + (user.lastLoginAt ? this.formatDate(user.lastLoginAt) : 'Never') + ansi.reset);
            this.terminal.writeln('');

            const choice = await this.io.lightbar([
                { label: 'Change Role', value: 'role', hotkey: 'R' },
                { label: 'Toggle Active', value: 'active', hotkey: 'A' },
                { label: 'Back', value: 'back', hotkey: 'Q' }
            ]);

            if (choice === 'role') {
                user = await this.changeRole(user);
            } else if (choice === 'active') {
                user = await this.toggleActive(user);
            } else {
                return;
            }
        }
    }

    private async changeRole(user: UserDetail): Promise<UserDetail> {
        this.terminal.writeln('');

        const choice = await this.io.lightbar([
            { label: 'User', value: 'ROLE_USER' },
            { label: 'Member', value: 'ROLE_MEMBER' },
            { label: 'Moderator', value: 'ROLE_MODERATOR' },
            { label: 'Sysop', value: 'ROLE_SYSOP' }
        ], 'Select Role');

        if (!choice) {
            return user;
        }

        try {
            const updated = await this.apiClient.patch(`/api/users/${user.id}`, { roles: [choice] }) as UserDetail;
            this.io.success('  Role updated.');
            await this.io.pause();
            return updated;
        } catch {
            this.io.error('  Failed to update role.');
            await this.io.pause();
            return user;
        }
    }

    private async toggleActive(user: UserDetail): Promise<UserDetail> {
        const newStatus = !user.isActive;

        try {
            const updated = await this.apiClient.patch(`/api/users/${user.id}`, { isActive: newStatus }) as UserDetail;
            if (newStatus) {
                this.io.success('  User activated.');
            } else {
                this.io.success('  User deactivated.');
            }
            await this.io.pause();
            return updated;
        } catch {
            this.io.error('  Failed to update status.');
            await this.io.pause();
            return user;
        }
    }

    private getDisplayRole(roles: string[]): string {
        if (roles.includes('ROLE_SYSOP')) {
            return 'Sysop';
        }
        if (roles.includes('ROLE_MODERATOR')) {
            return 'Moderator';
        }
        if (roles.includes('ROLE_MEMBER')) {
            return 'Member';
        }
        return 'User';
    }

    private formatDate(dateString: string): string {
        const date = new Date(dateString);
        return date.toLocaleDateString('en-US', {
            year: 'numeric',
            month: 'short',
            day: 'numeric'
        });
    }
}
