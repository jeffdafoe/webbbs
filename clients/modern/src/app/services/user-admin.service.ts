import { Injectable } from '@angular/core';
import { HttpClient, HttpParams } from '@angular/common/http';
import { Observable } from 'rxjs';

export interface UserSummary {
    id: string;
    username: string;
    email: string | null;
    roles: string[];
    isActive: boolean;
    createdAt: string;
    lastLoginAt: string | null;
    alias: string | null;
}

export interface UserProfile {
    alias: string | null;
    gender: string;
    entryMessage: string | null;
    exitMessage: string | null;
    bio: string | null;
    avatarUrl: string | null;
    preferredColor: string | null;
    timezone: string | null;
}

export interface UserDetail extends UserSummary {
    profile: UserProfile;
}

export interface UserListResponse {
    users: UserSummary[];
    total: number;
    page: number;
    limit: number;
}

@Injectable({
    providedIn: 'root'
})
export class UserAdminService {
    constructor(private http: HttpClient) {}

    listUsers(page: number = 1, limit: number = 25, search?: string): Observable<UserListResponse> {
        let params = new HttpParams()
            .set('page', page.toString())
            .set('limit', limit.toString());

        if (search) {
            params = params.set('search', search);
        }

        return this.http.get<UserListResponse>('/api/users', { params });
    }

    getUser(id: string): Observable<UserDetail> {
        return this.http.get<UserDetail>(`/api/users/${id}`);
    }

    createUser(data: Record<string, unknown>): Observable<UserDetail> {
        return this.http.post<UserDetail>('/api/users', data);
    }

    updateUser(id: string, data: Record<string, unknown>): Observable<UserDetail> {
        return this.http.patch<UserDetail>(`/api/users/${id}`, data);
    }

    deleteUser(id: string): Observable<void> {
        return this.http.delete<void>(`/api/users/${id}`);
    }
}
