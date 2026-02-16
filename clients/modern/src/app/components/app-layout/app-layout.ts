import { Component } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';
import { SettingsService } from '../../services/settings.service';

@Component({
    selector: 'app-layout',
    imports: [RouterLink],
    templateUrl: './app-layout.html',
    styleUrl: './app-layout.scss'
})
export class AppLayout {
    constructor(
        protected authService: AuthService,
        protected settingsService: SettingsService
    ) {}

    logout(): void {
        this.authService.logout();
    }
}
