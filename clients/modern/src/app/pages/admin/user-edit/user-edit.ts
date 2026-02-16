import { Component, signal, OnInit } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, Router } from '@angular/router';
import { UserAdminService, UserDetail } from '../../../services/user-admin.service';
import { AppLayout } from '../../../components/app-layout/app-layout';

@Component({
    selector: 'app-user-edit',
    imports: [FormsModule, AppLayout],
    templateUrl: './user-edit.html',
    styleUrl: './user-edit.scss'
})
export class UserEdit implements OnInit {
    loading = signal(false);
    saving = signal(false);
    error = signal('');
    successMessage = signal('');
    userId = signal('');

    username = signal('');
    email = signal('');
    password = signal('');
    role = signal('ROLE_USER');
    isActive = signal(true);

    alias = signal('');
    bio = signal('');
    entryMessage = signal('');
    exitMessage = signal('');
    preferredColor = signal('');
    timezone = signal('');

    constructor(
        private route: ActivatedRoute,
        private router: Router,
        private userAdminService: UserAdminService
    ) {}

    ngOnInit(): void {
        const id = this.route.snapshot.paramMap.get('id');
        if (id) {
            this.userId.set(id);
            this.loadUser(id);
        }
    }

    loadUser(id: string): void {
        this.loading.set(true);
        this.userAdminService.getUser(id).subscribe({
            next: (user) => {
                this.populateForm(user);
                this.loading.set(false);
            },
            error: () => {
                this.error.set('User not found.');
                this.loading.set(false);
            }
        });
    }

    populateForm(user: UserDetail): void {
        this.username.set(user.username);
        this.email.set(user.email || '');
        this.isActive.set(user.isActive);

        if (user.roles.includes('ROLE_SYSOP')) {
            this.role.set('ROLE_SYSOP');
        } else if (user.roles.includes('ROLE_MODERATOR')) {
            this.role.set('ROLE_MODERATOR');
        } else if (user.roles.includes('ROLE_MEMBER')) {
            this.role.set('ROLE_MEMBER');
        } else {
            this.role.set('ROLE_USER');
        }

        if (user.profile) {
            this.alias.set(user.profile.alias || '');
            this.bio.set(user.profile.bio || '');
            this.entryMessage.set(user.profile.entryMessage || '');
            this.exitMessage.set(user.profile.exitMessage || '');
            this.preferredColor.set(user.profile.preferredColor || '');
            this.timezone.set(user.profile.timezone || '');
        }
    }

    onSave(): void {
        this.error.set('');
        this.successMessage.set('');
        this.saving.set(true);

        const data: Record<string, unknown> = {
            email: this.email(),
            roles: this.buildRoles(),
            isActive: this.isActive(),
            alias: this.alias(),
            bio: this.bio(),
            entryMessage: this.entryMessage(),
            exitMessage: this.exitMessage(),
            preferredColor: this.preferredColor(),
            timezone: this.timezone(),
        };

        const passwordValue = this.password();
        if (passwordValue) {
            data['password'] = passwordValue;
        }

        this.userAdminService.updateUser(this.userId(), data).subscribe({
            next: () => {
                this.successMessage.set('User updated.');
                this.password.set('');
                this.saving.set(false);
            },
            error: (error) => {
                this.error.set(error.error?.error || 'Failed to update user.');
                this.saving.set(false);
            }
        });
    }

    onDelete(): void {
        if (!confirm('Delete this user? This cannot be undone.')) {
            return;
        }

        this.userAdminService.deleteUser(this.userId()).subscribe({
            next: () => {
                this.router.navigate(['/admin/users']);
            },
            error: () => {
                this.error.set('Failed to delete user.');
            }
        });
    }

    private buildRoles(): string[] {
        const selected = this.role();
        if (selected === 'ROLE_SYSOP') {
            return ['ROLE_SYSOP'];
        }
        if (selected === 'ROLE_MODERATOR') {
            return ['ROLE_MODERATOR'];
        }
        if (selected === 'ROLE_MEMBER') {
            return ['ROLE_MEMBER'];
        }
        return ['ROLE_USER'];
    }
}
