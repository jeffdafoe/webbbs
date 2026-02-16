import { Component, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';
import { Spinner } from '../../components/spinner/spinner';

@Component({
    selector: 'app-login',
    imports: [FormsModule, RouterLink, Spinner],
    templateUrl: './login.html',
    styleUrl: './login.scss'
})
export class Login {
    username = signal('');
    password = signal('');
    loading = signal(false);
    error = signal('');

    constructor(private authService: AuthService) {}

    onSubmit(): void {
        const user = this.username();
        const pass = this.password();
        if (user && pass) {
            this.loading.set(true);
            this.error.set('');
            this.authService.login(user, pass).subscribe({
                error: () => {
                    this.error.set('Invalid username or password.');
                    this.loading.set(false);
                }
            });
        }
    }
}
