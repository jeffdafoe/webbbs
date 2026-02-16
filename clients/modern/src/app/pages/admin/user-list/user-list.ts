import { Component, signal, OnInit } from '@angular/core';
import { RouterLink } from '@angular/router';
import { FormsModule } from '@angular/forms';
import { DatePipe } from '@angular/common';
import { UserAdminService, UserSummary } from '../../../services/user-admin.service';
import { AppLayout } from '../../../components/app-layout/app-layout';

@Component({
    selector: 'app-user-list',
    imports: [RouterLink, FormsModule, DatePipe, AppLayout],
    templateUrl: './user-list.html',
    styleUrl: './user-list.scss'
})
export class UserList implements OnInit {
    users = signal<UserSummary[]>([]);
    total = signal(0);
    page = signal(1);
    limit = signal(25);
    search = signal('');
    loading = signal(false);

    constructor(private userAdminService: UserAdminService) {}

    ngOnInit(): void {
        this.loadUsers();
    }

    loadUsers(): void {
        this.loading.set(true);
        const searchValue = this.search();
        this.userAdminService.listUsers(this.page(), this.limit(), searchValue || undefined)
            .subscribe({
                next: (response) => {
                    this.users.set(response.users);
                    this.total.set(response.total);
                    this.loading.set(false);
                },
                error: () => {
                    this.loading.set(false);
                }
            });
    }

    onSearch(): void {
        this.page.set(1);
        this.loadUsers();
    }

    nextPage(): void {
        if (this.page() * this.limit() < this.total()) {
            this.page.update(p => p + 1);
            this.loadUsers();
        }
    }

    previousPage(): void {
        if (this.page() > 1) {
            this.page.update(p => p - 1);
            this.loadUsers();
        }
    }

    totalPages(): number {
        return Math.ceil(this.total() / this.limit());
    }

    roleLabel(roles: string[]): string {
        if (roles.includes('ROLE_SYSOP')) {
            return 'Sysop';
        }
        if (roles.includes('ROLE_MODERATOR')) {
            return 'Moderator';
        }
        if (roles.includes('ROLE_MEMBER')) {
            return 'Member';
        }
        return 'User';
    }
}
