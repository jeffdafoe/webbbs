import { Component } from '@angular/core';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';
import { AppLayout } from '../../components/app-layout/app-layout';

@Component({
    selector: 'app-dashboard',
    imports: [RouterLink, AppLayout],
    templateUrl: './dashboard.html',
    styleUrl: './dashboard.scss'
})
export class Dashboard {
    constructor(protected authService: AuthService) {}
}
