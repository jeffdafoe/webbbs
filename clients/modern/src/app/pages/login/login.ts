import { Component, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';

@Component({
    selector: 'app-login',
    imports: [FormsModule, RouterLink],
    templateUrl: './login.html',
    styleUrl: './login.scss'
})
export class Login {
    username = signal('');
    password = signal('');

    constructor(private authService: AuthService) {}

    onSubmit(): void {
        const user = this.username();
        const pass = this.password();
        if (user && pass) {
            this.authService.login(user, pass);
        }
    }
}
