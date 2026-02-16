import { Terminal } from '@xterm/xterm';
import { ApiClient } from './api-client';
import { TerminalIO, LightbarPosition } from './terminal-io';
import { WidgetPosition } from './template-renderer';
import { DialogBox } from './dialog-box';
import { ansi } from './ansi';

export class LoginHandler {
    private io: TerminalIO;
    private menuPosition?: LightbarPosition;

    constructor(
        private terminal: Terminal,
        private apiClient: ApiClient,
        private renderWelcome: () => void,
        widgets?: WidgetPosition[]
    ) {
        this.io = new TerminalIO(terminal);
        if (widgets) {
            const loginWidget = widgets.find(w => w.name === 'login' && w.type === 'menu');
            if (loginWidget) {
                this.menuPosition = { row: loginWidget.row, col: loginWidget.col, width: loginWidget.width };
            }
        }
    }

    async run(): Promise<string | null> {
        while (true) {
            const choice = await this.io.lightbar([
                { label: 'Login', value: 'login', hotkey: 'L' },
                { label: 'Register', value: 'register', hotkey: 'R' },
                { label: 'Quit', value: 'quit', hotkey: 'Q' }
            ], { position: this.menuPosition });

            if (choice === 'login') {
                const token = await this.handleLogin();
                if (token) {
                    return token;
                }
                this.renderWelcome();
            } else if (choice === 'register') {
                await this.handleRegister();
                this.renderWelcome();
            } else {
                this.io.info('  Goodbye.');
                return null;
            }
        }
    }

    private async handleLogin(): Promise<string | null> {
        const dialog = new DialogBox(this.terminal, 'Login', [
            { label: 'Username', name: 'username' },
            { label: 'Password', name: 'password', password: true }
        ]);

        while (true) {
            const result = await dialog.run();

            if (result.cancelled) {
                return null;
            }

            const username = result.values.username;
            const password = result.values.password;

            if (!username || !password) {
                dialog.setMessage('All fields are required.', ansi.yellow);
                dialog.clearFields();
                continue;
            }

            try {
                const loginResult = await this.apiClient.login(username, password);
                dialog.setMessage('Login successful!', ansi.lightGreen);
                await this.sleep(800);
                return loginResult.token;
            } catch {
                dialog.setMessage('Invalid username or password.', ansi.lightRed);
                dialog.clearFields();
            }
        }
    }

    private async handleRegister(): Promise<void> {
        const dialog = new DialogBox(this.terminal, 'New User Registration', [
            { label: 'Username', name: 'username' },
            { label: 'Password', name: 'password', password: true },
            { label: 'Confirm', name: 'confirm', password: true },
            { label: 'Email', name: 'email' }
        ], 40);

        while (true) {
            const result = await dialog.run();

            if (result.cancelled) {
                return;
            }

            const username = result.values.username;
            const password = result.values.password;
            const confirm = result.values.confirm;
            const email = result.values.email;

            if (!username || !password) {
                dialog.setMessage('Username and password are required.', ansi.yellow);
                dialog.clearFields();
                continue;
            }

            if (password !== confirm) {
                dialog.setMessage('Passwords do not match.', ansi.yellow);
                dialog.clearFields();
                continue;
            }

            try {
                await this.apiClient.register(username, password, email || undefined);
                dialog.setMessage('Registration successful! You may now log in.', ansi.lightGreen);
                await this.sleep(1500);
                return;
            } catch (error) {
                if (error instanceof Error) {
                    dialog.setMessage(error.message, ansi.lightRed);
                } else {
                    dialog.setMessage('Registration failed.', ansi.lightRed);
                }
                dialog.clearFields();
            }
        }
    }

    private sleep(ms: number): Promise<void> {
        return new Promise(resolve => setTimeout(resolve, ms));
    }
}
