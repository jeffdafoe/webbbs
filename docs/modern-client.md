# Modern Client Developer Guide

The modern client is an Angular 19 SPA at `clients/modern/`. It provides the web-based admin and user interface for ZBBS.

## Shared Components

Shared components live in `clients/modern/src/app/components/`. These are available for any page or feature to use.

### AppLayout

Wraps page content with the standard header (logo, username, logout button) and centered main area.

```html
<app-layout>
    <h2>My Page</h2>
    <p>Content goes here.</p>
</app-layout>
```

Import `AppLayout` in your component's `imports` array.

### DialogService

Injectable service for themed modal dialogs. Replaces `window.confirm()` with a styled overlay that matches the app theme.

**Import:**

```typescript
import { DialogService } from '../services/dialog.service';
```

**Simple confirm (returns boolean):**

```typescript
constructor(private dialogService: DialogService) {}

async onDelete(): Promise<void> {
    const confirmed = await this.dialogService.confirm('Delete this item?');
    if (!confirmed) {
        return;
    }
    // proceed with delete
}
```

**Custom buttons and styles:**

```typescript
const result = await this.dialogService.open({
    message: 'Delete this user? This cannot be undone.',
    confirmLabel: 'Delete',
    confirmStyle: 'danger',
});

if (result === 'confirm') {
    // user clicked Delete
}
// result is 'cancel' if they clicked Cancel, null if they clicked the overlay
```

**Fully custom buttons:**

```typescript
const result = await this.dialogService.open({
    message: 'Save changes before leaving?',
    buttons: [
        { label: 'Discard', value: 'discard', style: 'danger' },
        { label: 'Cancel', value: 'cancel', style: 'neutral' },
        { label: 'Save', value: 'save', style: 'primary' },
    ],
});

if (result === 'save') { ... }
if (result === 'discard') { ... }
```

**Button styles:**

| Style | Appearance |
|-------|-----------|
| `primary` | Red filled button (app accent color) |
| `danger` | Bright red filled button |
| `neutral` | Gray outline button |

## Shared Services

Services live in `clients/modern/src/app/services/`.

| Service | Purpose |
|---------|---------|
| `AuthService` | JWT auth, login/logout, user info signal |
| `DialogService` | Modal dialog overlays |
| `SettingsService` | Public BBS settings (name, tagline) |
| `UserAdminService` | Sysop user management API calls |

## Project Structure

```
clients/modern/src/app/
    components/             Shared UI components
        app-layout/         Page wrapper with header
        dialog/             Modal dialog component
    pages/                  Route-level pages
        admin/              Admin pages (user-list, user-edit)
        auth/               Login, register
        home/               Dashboard
    services/               Injectable services
```

## Theme Colors

The app uses a consistent dark theme. Key colors:

| Color | Hex | Usage |
|-------|-----|-------|
| Background | `#1a1a2e` | Page background |
| Panel | `#16213e` | Header, cards, dialog boxes |
| Border | `#0f3460` | Panel borders, fieldset borders |
| Accent | `#e94560` | Primary buttons, logo, active elements |
| Text | `#e0e0e0` | Primary text |
| Muted text | `#a0a0b0` | Labels, secondary text |
| Danger | `#ff5555` | Delete actions, error states |
| Success | `#50fa7b` | Success messages |
