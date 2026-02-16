# ZBBS Development Guide

## Environment

Apache serves everything on port 80, same as production. No dev servers — you build, Apache serves.

| URL | What |
|-----|------|
| `http://zbbs.local/` | Landing page (choose terminal or modern) |
| `http://zbbs.local/api/` | Symfony API via PHP-FPM |
| `http://zbbs.local/modern/` | Angular SPA (built static files) |
| `http://zbbs.local/terminal/` | Terminal client (static files) |

Mercure runs on localhost:3000 but is only accessed by the API server-side. It is not exposed through Apache.

**Getting started?** Follow the step-by-step guide for your platform:
- [Windows (WSL2) Setup](development/windows/)
- [Linux Setup](development/linux/)

The rest of this document is a reference for day-to-day development.

## Setup Command

After the infrastructure is installed and services are running, `zbbs:setup` configures the BBS and creates the sysop (admin) account:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console zbbs:setup
```

This prompts for the BBS name, tagline, phone number, and sysop credentials. It can only be run once. If you visit the site before running setup, it will show a message directing you to run this command.

## Project Layout

```
zbbs/
    api/                    Symfony 7 application
        public/             Web root (index.php)
        config/jwt/         JWT keypair
    clients/
        landing/            Static HTML landing page
        modern/             Angular 19 SPA
        terminal/           Terminal client (vanilla JS + xterm.js)
            dist/           Built output served by Apache
            templates/      .ans template files
    docs/                   Documentation
    infrastructure/
        group_vars/         Shared Ansible variables
        inventory/          Per-environment host/var files
        playbooks/          setup.yml, deploy.yml
        roles/              apache, php, postgresql, nodejs, mercure, common
        templates/          Environment variable templates
    migrations/             Raw SQL migration files
```

## Making Changes

### PHP / API

Edit files in `api/`. Clear the Symfony cache to pick up changes:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console cache:clear
```

For routing or config changes, you may also need to restart PHP-FPM:

```bash
systemctl restart php8.3-fpm
```

### Angular / Modern Client

Edit files in `clients/modern/`. Rebuild after changes:

```bash
cd /path/to/zbbs/clients/modern && ng build
```

The built files land in `clients/modern/dist/modern/browser/` which Apache serves at `/modern/`.

### Terminal Client

Edit files in `clients/terminal/`. The `dist/` directory is what Apache serves. If you have a build step, run it. If it's direct-edit files, just refresh.

### Landing Page

Edit `clients/landing/index.html` directly. Refresh browser.

### Infrastructure

After changing Ansible templates or roles:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml
```

## Database

PostgreSQL 15, accessed via unix socket from PHP-FPM. Database name and credentials are configured in the Ansible inventory.

Connect directly:

```bash
sudo -u postgres psql -d zbbs
```

### Migrations

Raw SQL files in `migrations/`. Applied via the deploy playbook:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'
```

## Apache Config

Single vhost on port 80 with path-based routing. The config is generated from Jinja2 templates:

- `roles/apache/templates/site-base.conf.j2` — shared structure (document root, aliases, PHP-FPM handler, rewrite rules)
- `roles/apache/templates/site-development.conf.j2` — extends base
- `roles/apache/templates/site-production.conf.j2` — extends base, adds security headers and logging

After template changes, re-run the setup playbook to deploy.

## Environment Variables

Set in `/etc/profile.d/zbbs.sh`, deployed by the deploy playbook from `infrastructure/templates/zbbs-env.sh.j2`. PHP-FPM gets database and Mercure credentials from its pool config.

## Troubleshooting

### Checking Service Status

```bash
systemctl status apache2 php8.3-fpm postgresql mercure --no-pager
```

### Checking Logs

Apache error log:

```bash
tail -20 /var/log/apache2/error.log
```

Apache access log:

```bash
tail -20 /var/log/apache2/other_vhosts_access.log
```

PHP-FPM log:

```bash
tail -20 /var/log/php8.3-fpm.log
```

Systemd journal (useful for seeing why services restarted):

```bash
journalctl -u php8.3-fpm --no-pager -n 20
```

### API Returns 503

This means Apache can't reach PHP-FPM. Common causes:
- PHP-FPM is not running (check service status)
- PHP-FPM pool socket doesn't match Apache config (check `/run/php/` for socket files)

### API Returns 500

Check the Symfony log:

```bash
tail -30 /path/to/zbbs/api/var/log/dev.log
```

## Stack

| Component | Version |
|-----------|---------|
| PHP | 8.3 |
| Symfony | 7 |
| PostgreSQL | 15 |
| Node.js | 20 |
| Angular | 19 |
| Mercure | 0.16 |
| Apache | Latest |
