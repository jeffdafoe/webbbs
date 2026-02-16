# Linux Development Setup

Development on Linux runs services natively. No WSL layer.

> **Note:** Commands below use `/path/to/zbbs` as a placeholder. Replace it with wherever you cloned the repo (e.g. `/var/www/zbbs` or `~/zbbs`).

## Hosts File

Add to `/etc/hosts`:

```
127.0.0.1 zbbs.local
```

The setup playbook will remind you if this is missing.

## First-Time Setup

1. Run the installer (setup + deploy):

```bash
cd /path/to/zbbs
sudo bash install.sh
```

You will be prompted for secrets (database password, JWT key passphrase, Mercure secret). Press Enter to accept defaults.

2. Configure the BBS and create the sysop account:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console zbbs:setup
```

3. Open `http://zbbs.local/` in your browser.

## Reinstalling

To reinstall everything (re-prompts for secrets, reinstalls packages, redeploys):

```bash
cd /path/to/zbbs
sudo bash reinstall.sh
```

## Starting Services

Services are managed by systemd and should start on boot. If not:

```bash
systemctl start postgresql && systemctl start php8.3-fpm && systemctl start apache2 && systemctl start mercure
```

## Common Commands

Clear Symfony cache:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console cache:clear
```

Restart PHP-FPM:

```bash
systemctl restart php8.3-fpm
```

Build terminal client:

```bash
cd /path/to/zbbs/clients/terminal && node build.mjs
```

Build Angular client:

```bash
cd /path/to/zbbs/clients/modern && ng build
```

Run migrations:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'
```

Connect to database:

```bash
sudo -u postgres psql -d zbbs
```

## Troubleshooting

### Checking Logs

```bash
tail -20 /var/log/apache2/error.log
tail -20 /var/log/apache2/other_vhosts_access.log
tail -20 /var/log/php8.3-fpm.log
journalctl -u php8.3-fpm --no-pager -n 20
tail -30 /path/to/zbbs/api/var/log/dev.log
```
