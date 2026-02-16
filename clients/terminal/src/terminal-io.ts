import { Terminal } from '@xterm/xterm';
import { ansi } from './ansi';

export interface MenuItem {
    label: string;
    value: string;
    hotkey?: string;
}

export interface ColumnDefinition {
    header: string;
    width: number;
}

export interface LightbarPosition {
    row: number;
    col: number;
    width?: number;
}

const COLS = 80;

export class TerminalIO {
    constructor(private terminal: Terminal) {}

    prompt(text: string): Promise<string> {
        return new Promise((resolve) => {
            this.terminal.write(text);
            let buffer = '';

            const disposable = this.terminal.onData((data) => {
                const code = data.charCodeAt(0);

                if (code === 13) {
                    disposable.dispose();
                    this.terminal.writeln('');
                    resolve(buffer);
                } else if (code === 127 || code === 8) {
                    if (buffer.length > 0) {
                        buffer = buffer.slice(0, -1);
                        this.terminal.write('\b \b');
                    }
                } else if (code >= 32) {
                    buffer += data;
                    this.terminal.write(data);
                }
            });
        });
    }

    promptPassword(text: string): Promise<string> {
        return new Promise((resolve) => {
            this.terminal.write(text);
            let buffer = '';

            const disposable = this.terminal.onData((data) => {
                const code = data.charCodeAt(0);

                if (code === 13) {
                    disposable.dispose();
                    this.terminal.writeln('');
                    resolve(buffer);
                } else if (code === 127 || code === 8) {
                    if (buffer.length > 0) {
                        buffer = buffer.slice(0, -1);
                        this.terminal.write('\b \b');
                    }
                } else if (code >= 32) {
                    buffer += data;
                    this.terminal.write('*');
                }
            });
        });
    }

    lightbar(items: MenuItem[], options?: { title?: string; position?: LightbarPosition }): Promise<string | null> {
        return new Promise((resolve) => {
            let selected = 0;
            const title = options?.title;
            const position = options?.position;

            let maxWidth = 0;
            for (let i = 0; i < items.length; i++) {
                if (items[i].label.length > maxWidth) {
                    maxWidth = items[i].label.length;
                }
            }
            const barWidth = maxWidth + 4;

            if (position) {
                // Positioned mode: absolute cursor placement, padded writes, no clearing
                // Center the bar within the widget slot width
                const slotWidth = position.width || 80;
                const centeredCol = position.col + Math.floor((slotWidth - barWidth) / 2);

                const draw = (): void => {
                    let row = position.row;

                    if (title) {
                        this.terminal.write(ansi.cursorTo(row, centeredCol));
                        this.terminal.write(ansi.cyan + title + ansi.reset);
                        row++;
                        this.terminal.write(ansi.cursorTo(row, centeredCol));
                        this.terminal.write(ansi.darkGray + '\u2500'.repeat(barWidth) + ansi.reset);
                        row++;
                    }

                    for (let i = 0; i < items.length; i++) {
                        const text = '  ' + items[i].label;
                        this.terminal.write(ansi.cursorTo(row + i, centeredCol));
                        if (i === selected) {
                            const padded = text.padEnd(barWidth);
                            this.terminal.write(ansi.white + ansi.bgBlue + padded + ansi.reset);
                        } else {
                            const padded = text.padEnd(barWidth);
                            this.terminal.write(padded);
                        }
                    }
                };

                draw();
                this.terminal.write(ansi.hideCursor);

                const disposable = this.terminal.onData((data) => {
                    if (data === '\x1b[A') {
                        if (selected > 0) {
                            selected--;
                            draw();
                            this.terminal.write(ansi.hideCursor);
                        }
                    } else if (data === '\x1b[B') {
                        if (selected < items.length - 1) {
                            selected++;
                            draw();
                            this.terminal.write(ansi.hideCursor);
                        }
                    } else if (data.charCodeAt(0) === 13) {
                        disposable.dispose();
                        resolve(items[selected].value);
                    } else if (data.length === 1 && data.charCodeAt(0) === 27) {
                        disposable.dispose();
                        resolve(null);
                    } else {
                        const pressed = data.toLowerCase();
                        for (let i = 0; i < items.length; i++) {
                            if (items[i].hotkey && items[i].hotkey!.toLowerCase() === pressed) {
                                disposable.dispose();
                                resolve(items[i].value);
                                return;
                            }
                        }
                    }
                });
            } else {
                // Freeflow mode: relative cursor movement, clearToEnd
                const leftMargin = Math.floor((COLS - barWidth) / 2);
                const marginStr = ' '.repeat(leftMargin);

                const draw = (): void => {
                    const totalLines = items.length + (title ? 3 : 0);
                    this.terminal.write(ansi.cursorUp(totalLines));
                    this.terminal.write(ansi.clearToEnd);

                    if (title) {
                        this.terminal.writeln(ansi.cyan + marginStr + title + ansi.reset);
                        this.terminal.writeln(ansi.darkGray + marginStr + '\u2500'.repeat(barWidth) + ansi.reset);
                    }

                    for (let i = 0; i < items.length; i++) {
                        if (i === selected) {
                            const padded = items[i].label.padEnd(barWidth - 4);
                            this.terminal.writeln(marginStr + ansi.white + ansi.bgBlue + '  ' + padded + '  ' + ansi.reset);
                        } else {
                            this.terminal.writeln(`${marginStr}  ${items[i].label}`);
                        }
                    }

                    if (title) {
                        this.terminal.writeln('');
                    }
                };

                const totalLines = items.length + (title ? 3 : 0);
                for (let i = 0; i < totalLines; i++) {
                    this.terminal.writeln('');
                }
                draw();

                const disposable = this.terminal.onData((data) => {
                    if (data === '\x1b[A') {
                        if (selected > 0) {
                            selected--;
                            draw();
                        }
                    } else if (data === '\x1b[B') {
                        if (selected < items.length - 1) {
                            selected++;
                            draw();
                        }
                    } else if (data.charCodeAt(0) === 13) {
                        disposable.dispose();
                        this.terminal.writeln('');
                        resolve(items[selected].value);
                    } else if (data.length === 1 && data.charCodeAt(0) === 27) {
                        disposable.dispose();
                        this.terminal.writeln('');
                        resolve(null);
                    } else {
                        const pressed = data.toLowerCase();
                        for (let i = 0; i < items.length; i++) {
                            if (items[i].hotkey && items[i].hotkey!.toLowerCase() === pressed) {
                                disposable.dispose();
                                this.terminal.writeln('');
                                resolve(items[i].value);
                                return;
                            }
                        }
                    }
                });
            }
        });
    }

    lightbarList(items: string[], title?: string): Promise<number> {
        return new Promise((resolve) => {
            let selected = 0;

            let maxWidth = 0;
            for (let i = 0; i < items.length; i++) {
                const len = this.stripAnsi(items[i]).length;
                if (len > maxWidth) {
                    maxWidth = len;
                }
            }
            const barWidth = maxWidth + 4;
            const leftMargin = Math.floor((COLS - barWidth) / 2);
            const marginStr = ' '.repeat(leftMargin);

            const draw = (): void => {
                const totalLines = items.length + (title ? 3 : 0);
                this.terminal.write(ansi.cursorUp(totalLines));
                this.terminal.write(ansi.clearToEnd);

                if (title) {
                    this.terminal.writeln(ansi.cyan + marginStr + title + ansi.reset);
                    this.terminal.writeln(ansi.darkGray + marginStr + '\u2500'.repeat(barWidth) + ansi.reset);
                }

                for (let i = 0; i < items.length; i++) {
                    if (i === selected) {
                        const plain = this.stripAnsi(items[i]);
                        const padded = plain.padEnd(barWidth - 4);
                        this.terminal.writeln(marginStr + ansi.white + ansi.bgBlue + '  ' + padded + '  ' + ansi.reset);
                    } else {
                        this.terminal.writeln(`${marginStr}  ${items[i]}`);
                    }
                }

                if (title) {
                    this.terminal.writeln('');
                }
            };

            // Placeholder lines
            const totalLines = items.length + (title ? 3 : 0);
            for (let i = 0; i < totalLines; i++) {
                this.terminal.writeln('');
            }
            draw();

            const disposable = this.terminal.onData((data) => {
                if (data === '\x1b[A') {
                    if (selected > 0) {
                        selected--;
                        draw();
                    }
                } else if (data === '\x1b[B') {
                    if (selected < items.length - 1) {
                        selected++;
                        draw();
                    }
                } else if (data.charCodeAt(0) === 13) {
                    disposable.dispose();
                    this.terminal.writeln('');
                    resolve(selected);
                } else if (data.length === 1 && data.charCodeAt(0) === 27) {
                    disposable.dispose();
                    this.terminal.writeln('');
                    resolve(-1);
                }
            });
        });
    }

    waitForKey(): Promise<string> {
        return new Promise((resolve) => {
            const disposable = this.terminal.onData((data) => {
                disposable.dispose();
                resolve(data);
            });
        });
    }

    error(message: string): void {
        this.terminal.writeln(ansi.red + message + ansi.reset);
    }

    success(message: string): void {
        this.terminal.writeln(ansi.green + message + ansi.reset);
    }

    info(message: string): void {
        this.terminal.writeln(ansi.cyan + message + ansi.reset);
    }

    highlight(message: string): void {
        this.terminal.writeln(ansi.white + message + ansi.reset);
    }

    separator(): void {
        this.terminal.writeln(ansi.darkGray + '  ' + '─'.repeat(COLS - 4) + ansi.reset);
    }

    statusBar(left: string, right: string): void {
        const padding = COLS - left.length - right.length;
        const pad = padding > 0 ? ' '.repeat(padding) : ' ';
        this.terminal.write(ansi.cursorTo(1, 1) + ansi.bgDarkGray + ansi.white + left + pad + right + ansi.reset);
    }

    clear(): void {
        this.terminal.write(ansi.clearScreen);
    }

    pause(): Promise<void> {
        this.terminal.write(ansi.darkGray + '  Press any key to continue...' + ansi.reset);
        return new Promise((resolve) => {
            const disposable = this.terminal.onData(() => {
                disposable.dispose();
                this.terminal.writeln('');
                resolve();
            });
        });
    }

    formatColumns(columns: ColumnDefinition[], values: string[]): string {
        let result = '';
        for (let i = 0; i < columns.length; i++) {
            const plain = this.stripAnsi(values[i] || '');
            const ansiLen = (values[i] || '').length - plain.length;
            result += (values[i] || '').padEnd(columns[i].width + ansiLen);
        }
        return result;
    }

    formatColumnHeader(columns: ColumnDefinition[]): string {
        let result = '';
        for (let i = 0; i < columns.length; i++) {
            result += columns[i].header.padEnd(columns[i].width);
        }
        return result;
    }

    private stripAnsi(text: string): string {
        return text.replace(/\x1b\[[0-9;]*m/g, '');
    }
}
