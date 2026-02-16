import { Routes } from '@angular/router';
import { authGuard } from './guards/auth.guard';

export const routes: Routes = [
    {
        path: 'login',
        loadComponent: () => import('./pages/login/login').then(m => m.Login)
    },
    {
        path: 'register',
        loadComponent: () => import('./pages/register/register').then(m => m.Register)
    },
    {
        path: 'home',
        loadComponent: () => import('./pages/dashboard/dashboard').then(m => m.Dashboard),
        canActivate: [authGuard]
    },
    {
        path: 'admin/users',
        loadComponent: () => import('./pages/admin/user-list/user-list').then(m => m.UserList),
        canActivate: [authGuard]
    },
    {
        path: 'admin/users/:id',
        loadComponent: () => import('./pages/admin/user-edit/user-edit').then(m => m.UserEdit),
        canActivate: [authGuard]
    },
    {
        path: '',
        redirectTo: 'login',
        pathMatch: 'full'
    }
];
