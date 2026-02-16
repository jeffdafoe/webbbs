import { Component, computed } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';
import { AppLayout } from '../../components/app-layout/app-layout';
import { Spinner } from '../../components/spinner/spinner';

@Component({
    selector: 'app-dashboard',
    imports: [RouterLink, AppLayout, Spinner],
    templateUrl: './dashboard.html',
    styleUrl: './dashboard.scss'
})
export class Dashboard {
    loading = computed(() => this.authService.userInfo() === null);

    constructor(protected authService: AuthService) {}
}
