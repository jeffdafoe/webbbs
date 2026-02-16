import { Injectable, ApplicationRef, createComponent, EnvironmentInjector } from '@angular/core';
import { DialogComponent, DialogButton } from '../components/dialog/dialog';

export interface DialogOptions {
    message: string;
    buttons?: DialogButton[];
    confirmLabel?: string;
    cancelLabel?: string;
    confirmStyle?: 'primary' | 'danger' | 'neutral';
}

@Injectable({ providedIn: 'root' })
export class DialogService {
    constructor(
        private applicationRef: ApplicationRef,
        private injector: EnvironmentInjector,
    ) {}

    open(options: DialogOptions): Promise<string | null> {
        return new Promise((resolve) => {
            const componentRef = createComponent(DialogComponent, {
                environmentInjector: this.injector,
            });

            componentRef.instance.message = options.message;

            if (options.buttons) {
                componentRef.instance.buttons = options.buttons;
            } else {
                componentRef.instance.buttons = [
                    {
                        label: options.cancelLabel || 'Cancel',
                        value: 'cancel',
                        style: 'neutral',
                    },
                    {
                        label: options.confirmLabel || 'OK',
                        value: 'confirm',
                        style: options.confirmStyle || 'primary',
                    },
                ];
            }

            componentRef.instance.resolve = (value: string | null) => {
                this.applicationRef.detachView(componentRef.hostView);
                componentRef.destroy();
                resolve(value);
            };

            this.applicationRef.attachView(componentRef.hostView);
            document.body.appendChild(componentRef.location.nativeElement);
        });
    }

    confirm(message: string): Promise<boolean> {
        return this.open({ message }).then((value) => value === 'confirm');
    }
}
