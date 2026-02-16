export class ApiClient {
    private token: string | null = null;

    setToken(token: string): void {
        this.token = token;
    }

    getToken(): string | null {
        return this.token;
    }

    clearToken(): void {
        this.token = null;
    }

    async login(username: string, password: string): Promise<{ token: string }> {
        const response = await fetch('/api/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ username, password })
        });

        if (!response.ok) {
            const error = await response.json().catch(() => ({ message: 'Login failed' }));
            throw new Error(error.message || 'Invalid credentials');
        }

        const data = await response.json();
        this.token = data.token;
        return data;
    }

    async register(username: string, password: string, email?: string): Promise<{ message: string; username: string }> {
        const body: Record<string, string> = { username, password };
        if (email) {
            body.email = email;
        }

        const response = await fetch('/api/register', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            const error = await response.json().catch(() => ({ error: 'Registration failed' }));
            throw new Error(error.error || 'Registration failed');
        }

        return response.json();
    }

    async get(path: string): Promise<unknown> {
        const response = await fetch(path, {
            headers: this.authHeaders()
        });

        if (!response.ok) {
            throw new Error(`API error: ${response.status}`);
        }

        return response.json();
    }

    async patch(path: string, body: Record<string, unknown>): Promise<unknown> {
        const response = await fetch(path, {
            method: 'PATCH',
            headers: {
                'Content-Type': 'application/json',
                ...this.authHeaders()
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            throw new Error(`API error: ${response.status}`);
        }

        return response.json();
    }

    async post(path: string, body: Record<string, unknown>): Promise<unknown> {
        const response = await fetch(path, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                ...this.authHeaders()
            },
            body: JSON.stringify(body)
        });

        if (!response.ok) {
            throw new Error(`API error: ${response.status}`);
        }

        return response.json();
    }

    async delete(path: string): Promise<void> {
        const response = await fetch(path, {
            method: 'DELETE',
            headers: this.authHeaders()
        });

        if (!response.ok) {
            throw new Error(`API error: ${response.status}`);
        }
    }

    private authHeaders(): Record<string, string> {
        if (!this.token) {
            return {};
        }
        return { 'Authorization': `Bearer ${this.token}` };
    }
}
