import { Component, Input } from '@angular/core';

export interface DialogButton {
    label: string;
    value: string;
    style: 'primary' | 'danger' | 'neutral';
}

@Component({
    selector: 'app-dialog',
    templateUrl: './dialog.html',
    styleUrl: './dialog.scss'
})
export class DialogComponent {
    @Input() message = '';
    @Input() buttons: DialogButton[] = [];

    resolve!: (value: string | null) => void;

    onButtonClick(value: string): void {
        this.resolve(value);
    }

    onOverlayClick(): void {
        this.resolve(null);
    }
}
