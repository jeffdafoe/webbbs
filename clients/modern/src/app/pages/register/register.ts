import { Component, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AuthService } from '../../services/auth.service';

@Component({
    selector: 'app-register',
    imports: [FormsModule, RouterLink],
    templateUrl: './register.html',
    styleUrl: './register.scss'
})
export class Register {
    username = signal('');
    password = signal('');
    email = signal('');

    constructor(private authService: AuthService) {}

    onSubmit(): void {
        const user = this.username();
        const pass = this.password();
        if (user && pass) {
            this.authService.register(user, pass, this.email() || undefined);
        }
    }
}
