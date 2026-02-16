# Windows (WSL2) Development Setup

Development on Windows uses WSL2 (Debian) to run the Linux services. All commands run inside WSL via `wsl -u root bash -c "..."`.

> **Note:** Commands below assume the repo is cloned to `C:\dev\zbbs` (which maps to `/mnt/c/dev/zbbs` inside WSL). Adjust the paths if your clone is elsewhere.

## Quick Start

Run the setup script to configure `.wslconfig`, check the hosts file, and start services:

```powershell
powershell -ExecutionPolicy Bypass -File docs\development\windows\setup.ps1
```

The rest of this document explains what the script does and how to troubleshoot.

## WSL2 Configuration

Add to `C:\Users\<username>\.wslconfig`:

```ini
[general]
instanceIdleTimeout=-1

[wsl2]
networkingMode=mirrored
vmIdleTimeout=-1
```

- `instanceIdleTimeout=-1` — prevents WSL2 from shutting down the distro instance when idle (default is 15 seconds)
- `networkingMode=mirrored` — makes WSL2 ports accessible from Windows via localhost/zbbs.local
- `vmIdleTimeout=-1` — prevents WSL2 from shutting down the VM when idle (default is 60 seconds, starts after all instances exit)

Both timeout settings are required. Without them, WSL2 shuts down the instance after 15 seconds of no terminal activity, then the VM 60 seconds later. This kills all services (Apache, PHP-FPM, PostgreSQL, Mercure) and causes 503 errors on the next browser request.

Apply changes:

```
wsl --shutdown
```

## Hosts File

Add to `C:\Windows\System32\drivers\etc\hosts` (requires running Notepad as Administrator):

```
127.0.0.1 zbbs.local
```

The setup playbook will remind you if this is missing.

## Starting Services

After a reboot or `wsl --shutdown`, services need to be started:

```bash
wsl -u root bash -c "systemctl start postgresql && systemctl start php8.3-fpm && systemctl start apache2 && systemctl start mercure"
```

Or re-run the setup playbook which ensures everything is running:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml"
```

## First-Time Setup

After services are running, complete the initial setup:

1. Run the Ansible setup playbook to install and configure everything:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml"
```

2. Install dependencies:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/api && composer install"
wsl -u root bash -c "cd /mnt/c/dev/zbbs/clients/terminal && npm install"
wsl -u root bash -c "cd /mnt/c/dev/zbbs/clients/modern && npm install"
```

3. Run migrations:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'"
```

4. Configure the BBS and create the sysop account:

```bash
wsl -u root bash -c "source /etc/profile.d/zbbs.sh && cd /mnt/c/dev/zbbs/api && php bin/console zbbs:setup"
```

5. Build the clients:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/clients/terminal && node build.mjs"
wsl -u root bash -c "cd /mnt/c/dev/zbbs/clients/modern && npx ng build"
```

6. Open `http://zbbs.local/` in your browser.

## Common Commands

All commands are prefixed with `wsl -u root bash -c` since services run inside WSL.

Clear Symfony cache:

```bash
wsl -u root bash -c "source /etc/profile.d/zbbs.sh && cd /mnt/c/dev/zbbs/api && php bin/console cache:clear"
```

Restart PHP-FPM:

```bash
wsl -u root bash -c "systemctl restart php8.3-fpm"
```

Build Angular client:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/clients/modern && ng build"
```

Run setup playbook:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml"
```

Run migrations:

```bash
wsl -u root bash -c "cd /mnt/c/dev/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'"
```

Connect to database:

```bash
wsl -u root bash -c "sudo -u postgres psql -d zbbs"
```

## Troubleshooting

### Services Keep Restarting (503 Errors)

If API requests fail with 503 errors, the WSL2 VM is probably shutting down between requests. Check with:

```bash
wsl -u root bash -c "uptime"
```

If uptime shows only seconds, the VM is rebooting each time. Verify `.wslconfig` has both `instanceIdleTimeout=-1` and `vmIdleTimeout=-1`, then `wsl --shutdown` and restart services.

### Site Not Responding After Reboot

1. Check WSL is running: `wsl -u root bash -c "uptime"`
2. Start services: `wsl -u root bash -c "systemctl start postgresql && systemctl start php8.3-fpm && systemctl start apache2 && systemctl start mercure"`
3. Verify hosts file has `127.0.0.1 zbbs.local`

### Checking Logs

```bash
wsl -u root bash -c "tail -20 /var/log/apache2/error.log"
wsl -u root bash -c "tail -20 /var/log/apache2/other_vhosts_access.log"
wsl -u root bash -c "tail -20 /var/log/php8.3-fpm.log"
wsl -u root bash -c "journalctl -u php8.3-fpm --no-pager -n 20"
wsl -u root bash -c "tail -30 /mnt/c/dev/zbbs/api/var/log/dev.log"
```
