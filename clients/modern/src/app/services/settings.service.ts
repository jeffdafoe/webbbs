import { Injectable, signal } from '@angular/core';
import { HttpClient } from '@angular/common/http';

export interface PublicSettings {
    bbs_name: string;
    bbs_tagline: string;
    bbs_phone: string;
    registration_enabled: string;
}

@Injectable({ providedIn: 'root' })
export class SettingsService {
    private settings = signal<PublicSettings>({
        bbs_name: 'ZBBS',
        bbs_tagline: 'Bulletin Board System',
        bbs_phone: '555-ZBBS',
        registration_enabled: 'true'
    });

    readonly publicSettings = this.settings.asReadonly();

    constructor(private http: HttpClient) {
        this.load();
    }

    private load(): void {
        this.http.get<PublicSettings>('/api/settings/public').subscribe({
            next: (data) => this.settings.set(data),
            error: () => {}
        });
    }

    get bbsName(): string {
        return this.settings().bbs_name;
    }
}
