import { Component } from '@angular/core';
import { Router, RouterLink } from '@angular/router';
import { Location } from '@angular/common';
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
        protected settingsService: SettingsService,
        private router: Router,
        private location: Location,
    ) {}

    get showBack(): boolean {
        return this.router.url !== '/home';
    }

    goBack(): void {
        this.location.back();
    }

    logout(): void {
        this.authService.logout();
    }
}
