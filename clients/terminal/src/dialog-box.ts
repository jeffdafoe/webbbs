import { Terminal } from '@xterm/xterm';
import { ansi } from './ansi';

export interface DialogField {
    label: string;
    name: string;
    password?: boolean;
}

export interface DialogResult {
    cancelled: boolean;
    values: Record<string, string>;
}

// Raw ANSI sequences for dialog rendering — these set fg WITHOUT resetting bg.
// The ansi.ts module uses reset-first (ESC[0;XXm) which clears background.
// For overlaid dialogs we need to layer fg on top of bg.
const ESC = '\x1b[';
const color = {
    bgBlue: ESC + '44m',
    bgDarkGray: ESC + '100m',
    fgWhite: ESC + '97m',
    fgLightCyan: ESC + '96m',
    fgLightGray: ESC + '37m',
    fgDarkGray: ESC + '90m',
    fgYellow: ESC + '93m',
    fgLightGreen: ESC + '92m',
    fgLightRed: ESC + '91m',
};

export class DialogBox {
    private startRow: number;
    private startCol: number;
    private innerWidth: number;
    private totalHeight: number;
    private fieldInputCol: number;
    private fieldInputWidth: number;
    private messageRow: number;
    private maxLabelLength: number;
    private drawn: boolean = false;

    constructor(
        private terminal: Terminal,
        private title: string,
        private fields: DialogField[],
        private width: number = 34
    ) {
        this.innerWidth = this.width - 2;
        // rows: top border, title, separator, blank, ...fields, blank, message, blank, bottom border
        this.totalHeight = 4 + this.fields.length + 3;
        this.startRow = Math.floor((25 - this.totalHeight) / 2) + 1;
        this.startCol = Math.floor((80 - this.width) / 2) + 1;

        this.maxLabelLength = 0;
        for (let i = 0; i < this.fields.length; i++) {
            if (this.fields[i].label.length > this.maxLabelLength) {
                this.maxLabelLength = this.fields[i].label.length;
            }
        }
        // "║" + "  " + label + ": " = border(1) + padding(2) + label + colon-space(2)
        this.fieldInputCol = this.startCol + 1 + 2 + this.maxLabelLength + 2;
        // From input start to right border "║"
        this.fieldInputWidth = this.startCol + this.width - 1 - this.fieldInputCol - 1;
        this.messageRow = this.startRow + 4 + this.fields.length;
    }

    draw(): void {
        const t = this.terminal;
        const col = this.startCol;
        let row = this.startRow;

        // Clear the area behind the dialog so template borders don't bleed through
        for (let r = this.startRow; r < this.startRow + this.totalHeight + 1; r++) {
            t.write(ansi.cursorTo(r, 1));
            t.write(ansi.reset + ' '.repeat(80));
        }

        // Top border
        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan);
        t.write('╔' + '═'.repeat(this.innerWidth) + '╗');
        this.drawShadowRight(row);

        row++;
        // Title row
        const titlePadded = this.centerText(this.title, this.innerWidth);
        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan + '║');
        t.write(color.fgWhite + titlePadded);
        t.write(color.fgLightCyan + '║');
        this.drawShadowRight(row);

        row++;
        // Separator
        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan);
        t.write('╠' + '═'.repeat(this.innerWidth) + '╣');
        this.drawShadowRight(row);

        row++;
        // Blank row
        this.drawEmptyRow(row);
        row++;

        // Field rows
        for (let i = 0; i < this.fields.length; i++) {
            this.drawFieldRow(row, this.fields[i], '');
            row++;
        }

        // Blank row
        this.drawEmptyRow(row);
        row++;

        // Message row (empty)
        this.drawEmptyRow(row);
        row++;

        // Blank row
        this.drawEmptyRow(row);
        row++;

        // Bottom border
        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan);
        t.write('╚' + '═'.repeat(this.innerWidth) + '╝');
        this.drawShadowRight(row);

        row++;
        // Shadow on bottom
        t.write(ansi.cursorTo(row, col + 1));
        t.write(ansi.reset + color.fgDarkGray + '░'.repeat(this.width));
        t.write(ansi.reset);
    }

    run(): Promise<DialogResult> {
        return new Promise((resolve) => {
            if (!this.drawn) {
                this.draw();
                this.drawn = true;
            }

            const values: string[] = [];
            for (let i = 0; i < this.fields.length; i++) {
                values.push('');
            }

            let currentField = 0;
            this.positionCursorInField(currentField);
            this.terminal.write(ansi.showCursor);

            // Defer listener attachment to drain any buffered keystrokes
            // (e.g. the Enter that triggered this dialog from the lightbar)
            setTimeout(() => {

            const disposable = this.terminal.onData((data) => {
                const code = data.charCodeAt(0);

                // ESC - cancel
                if (data.length === 1 && code === 27) {
                    disposable.dispose();
                    this.terminal.write(ansi.hideCursor);
                    const result: Record<string, string> = {};
                    for (let i = 0; i < this.fields.length; i++) {
                        result[this.fields[i].name] = values[i];
                    }
                    resolve({ cancelled: true, values: result });
                    return;
                }

                // Enter - submit if on last field, else move to next
                if (code === 13) {
                    if (currentField < this.fields.length - 1) {
                        currentField++;
                        this.positionCursorInField(currentField, values[currentField]);
                    } else {
                        disposable.dispose();
                        this.terminal.write(ansi.hideCursor);
                        const result: Record<string, string> = {};
                        for (let i = 0; i < this.fields.length; i++) {
                            result[this.fields[i].name] = values[i];
                        }
                        resolve({ cancelled: false, values: result });
                    }
                    return;
                }

                // Tab - move to next field (or wrap)
                if (code === 9) {
                    currentField = (currentField + 1) % this.fields.length;
                    this.positionCursorInField(currentField, values[currentField]);
                    return;
                }

                // Shift+Tab (ESC [ Z) - move to previous field
                if (data === '\x1b[Z') {
                    currentField = (currentField - 1 + this.fields.length) % this.fields.length;
                    this.positionCursorInField(currentField, values[currentField]);
                    return;
                }

                // Backspace
                if (code === 127 || code === 8) {
                    if (values[currentField].length > 0) {
                        values[currentField] = values[currentField].slice(0, -1);
                        this.redrawFieldValue(currentField, values[currentField]);
                    }
                    return;
                }

                // Printable character
                if (code >= 32 && values[currentField].length < this.fieldInputWidth) {
                    values[currentField] += data;
                    this.redrawFieldValue(currentField, values[currentField]);
                }
            });

            }, 0);
        });
    }

    setMessage(text: string, rawColor: string): void {
        // Map ansi.ts colors (which reset) to our layered equivalents
        let fg = color.fgWhite;
        if (rawColor === ansi.yellow) {
            fg = color.fgYellow;
        } else if (rawColor === ansi.lightGreen) {
            fg = color.fgLightGreen;
        } else if (rawColor === ansi.lightRed) {
            fg = color.fgLightRed;
        }

        const padded = this.centerText(text, this.innerWidth);
        this.terminal.write(ansi.cursorTo(this.messageRow, this.startCol));
        this.terminal.write(ansi.reset + color.bgBlue + color.fgLightCyan + '║');
        this.terminal.write(fg + padded);
        this.terminal.write(color.fgLightCyan + '║');
        this.terminal.write(ansi.reset);
    }

    clearMessage(): void {
        this.drawEmptyRow(this.messageRow);
    }

    clearFields(): void {
        for (let i = 0; i < this.fields.length; i++) {
            const fieldRow = this.startRow + 4 + i;
            this.drawFieldRow(fieldRow, this.fields[i], '');
        }
        this.positionCursorInField(0);
    }

    private drawFieldRow(row: number, field: DialogField, value: string): void {
        const t = this.terminal;
        const col = this.startCol;

        const labelPadded = field.label.padStart(this.maxLabelLength);
        const displayValue = field.password ? '*'.repeat(value.length) : value;
        const inputContent = displayValue.padEnd(this.fieldInputWidth);

        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan + '║');
        t.write(color.fgWhite + '  ');
        t.write(color.fgLightGray + labelPadded);
        t.write(color.fgDarkGray + ': ');
        t.write(ansi.reset + color.bgDarkGray + color.fgWhite + inputContent);
        // Fill remaining space to right border
        const usedInner = 2 + this.maxLabelLength + 2 + this.fieldInputWidth;
        const remaining = this.innerWidth - usedInner;
        if (remaining > 0) {
            t.write(ansi.reset + color.bgBlue + ' '.repeat(remaining));
        }
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan + '║');
        this.drawShadowRight(row);
    }

    private drawEmptyRow(row: number): void {
        const t = this.terminal;
        const col = this.startCol;

        t.write(ansi.cursorTo(row, col));
        t.write(ansi.reset + color.bgBlue + color.fgLightCyan);
        t.write('║' + ' '.repeat(this.innerWidth) + '║');
        this.drawShadowRight(row);
    }

    private drawShadowRight(row: number): void {
        this.terminal.write(ansi.reset + color.fgDarkGray + '░');
        this.terminal.write(ansi.reset);
    }

    private positionCursorInField(fieldIndex: number, value?: string): void {
        const row = this.startRow + 4 + fieldIndex;
        const valueLen = value ? value.length : 0;
        this.terminal.write(ansi.cursorTo(row, this.fieldInputCol + valueLen));
    }

    private redrawFieldValue(fieldIndex: number, value: string): void {
        const row = this.startRow + 4 + fieldIndex;
        const field = this.fields[fieldIndex];
        const displayValue = field.password ? '*'.repeat(value.length) : value;
        const inputContent = displayValue.padEnd(this.fieldInputWidth);

        this.terminal.write(ansi.cursorTo(row, this.fieldInputCol));
        this.terminal.write(ansi.reset + color.bgDarkGray + color.fgWhite + inputContent);
        this.terminal.write(ansi.cursorTo(row, this.fieldInputCol + value.length));
    }

    private centerText(text: string, width: number): string {
        if (text.length >= width) {
            return text.substring(0, width);
        }
        const leftPad = Math.floor((width - text.length) / 2);
        const rightPad = width - text.length - leftPad;
        return ' '.repeat(leftPad) + text + ' '.repeat(rightPad);
    }
}
