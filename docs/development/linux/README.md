# Linux Development Setup

Development on Linux runs services natively. No WSL layer.

> **Note:** Commands below use `/path/to/zbbs` as a placeholder. Replace it with wherever you cloned the repo (e.g. `/var/www/zbbs` or `~/zbbs`).

## Hosts File

Add to `/etc/hosts`:

```
127.0.0.1 zbbs.local
```

The setup playbook will remind you if this is missing.

## Initial Setup

Clone the repo and run the setup playbook:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml
```

This installs and configures Apache, PHP-FPM, PostgreSQL, Node.js, and Mercure.

## Starting Services

Services are managed by systemd and should start on boot. If not:

```bash
systemctl start postgresql && systemctl start php8.3-fpm && systemctl start apache2 && systemctl start mercure
```

## First-Time Setup

After the setup playbook has run and services are started:

1. Install dependencies:

```bash
cd /path/to/zbbs/api && composer install
cd /path/to/zbbs/clients/terminal && npm install
cd /path/to/zbbs/clients/modern && npm install
```

2. Run migrations:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'
```

3. Configure the BBS and create the sysop account:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console zbbs:setup
```

4. Build the clients:

```bash
cd /path/to/zbbs/clients/terminal && node build.mjs
cd /path/to/zbbs/clients/modern && npx ng build
```

5. Open `http://zbbs.local/` in your browser.

## Common Commands

Clear Symfony cache:

```bash
source /etc/profile.d/zbbs.sh && cd /path/to/zbbs/api && php bin/console cache:clear
```

Restart PHP-FPM:

```bash
systemctl restart php8.3-fpm
```

Build Angular client:

```bash
cd /path/to/zbbs/clients/modern && ng build
```

Run setup playbook:

```bash
cd /path/to/zbbs/infrastructure && ansible-playbook -i inventory/local.yml playbooks/setup.yml
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
