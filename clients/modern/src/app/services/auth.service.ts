import { Injectable, signal, computed } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { Router } from '@angular/router';
import { Observable, tap, map } from 'rxjs';

interface LoginResponse {
    token: string;
}

interface RegisterResponse {
    message: string;
    username: string;
}

interface UserInfo {
    id: string;
    username: string;
    email: string | null;
    roles: string[];
    isActive: boolean;
    createdAt: string;
    lastLoginAt: string | null;
}

@Injectable({
    providedIn: 'root'
})
export class AuthService {
    private tokenSignal = signal<string | null>(this.getStoredToken());
    private userInfoSignal = signal<UserInfo | null>(null);

    isAuthenticated = computed(() => this.tokenSignal() !== null);
    token = computed(() => this.tokenSignal());
    userInfo = computed(() => this.userInfoSignal());

    hasRole(role: string): boolean {
        const info = this.userInfoSignal();
        if (info === null) {
            return false;
        }
        return info.roles.includes(role);
    }

    constructor(
        private http: HttpClient,
        private router: Router
    ) {
        if (this.isAuthenticated()) {
            this.fetchUserInfo();
        }
    }

    login(username: string, password: string): Observable<void> {
        return this.http.post<LoginResponse>('/api/login', { username, password }).pipe(
            tap((response) => {
                localStorage.setItem('jwt_token', response.token);
                this.tokenSignal.set(response.token);
                this.fetchUserInfo();
                this.router.navigate(['/home']);
            }),
            map(() => undefined),
        );
    }

    register(username: string, password: string, email?: string): void {
        const body: Record<string, string> = { username, password };
        if (email) {
            body['email'] = email;
        }

        this.http.post<RegisterResponse>('/api/register', body)
            .subscribe({
                next: () => {
                    this.router.navigate(['/login']);
                },
                error: (error) => {
                    console.error('Registration failed:', error);
                }
            });
    }

    logout(): void {
        localStorage.removeItem('jwt_token');
        this.tokenSignal.set(null);
        this.userInfoSignal.set(null);
        this.router.navigate(['/login']);
    }

    getToken(): string | null {
        return this.tokenSignal();
    }

    private fetchUserInfo(): void {
        this.http.get<UserInfo>('/api/me').subscribe({
            next: (info) => {
                this.userInfoSignal.set(info);
            },
            error: () => {
                this.logout();
            }
        });
    }

    private getStoredToken(): string | null {
        if (typeof localStorage === 'undefined') {
            return null;
        }
        return localStorage.getItem('jwt_token');
    }
}
