# ZBBS Installation Guide

## Prerequisites

- Debian 12 (Bookworm) or compatible Linux distribution
- sudo access
- For WSL development: Windows 10/11 with WSL2

## Quick Start

Run this single command to install everything:

```bash
curl -fsSL https://raw.githubusercontent.com/jeffdafoe/zbbs/main/install.sh | sudo bash
```

Or manually:

```bash
# Install git and ansible
sudo apt update && sudo apt install -y git ansible

# Clone the repo
git clone https://github.com/jeffdafoe/zbbs.git /var/www/zbbs
cd /var/www/zbbs

# Run install (setup + deploy)
sudo bash install.sh
```

On first run, you will be prompted for secrets (database password, JWT key passphrase, Mercure secret). Press Enter to accept the defaults or type a custom value.

After installation, configure the BBS and create the sysop account:

```bash
source /etc/profile.d/zbbs.sh && cd /var/www/zbbs/api && php bin/console zbbs:setup
```

## Reinstalling

To reinstall (re-prompts for secrets, reinstalls all packages, redeploys):

```bash
sudo bash reinstall.sh
```

## What Gets Installed

| Component | Version | Purpose |
|-----------|---------|---------|
| PHP | 8.3 | Backend runtime (via PHP-FPM) |
| Composer | Latest | PHP package manager |
| Symfony CLI | Latest | Symfony tooling |
| PostgreSQL | 15 | Database |
| Node.js | 20 LTS | Frontend build tools |
| Angular CLI | Latest | Frontend framework |
| Mercure | 0.16 | Real-time messaging |
| Apache | Latest | Web server |

## Default Configuration

### URL Structure

Everything is served through Apache on port 80 with path-based routing:

| Path | What |
|------|------|
| `/` | Landing page |
| `/api/` | Symfony API (PHP-FPM) |
| `/modern/` | Angular SPA |
| `/terminal/` | Terminal client |

### Database
- Database: `zbbs`
- User: `zbbs`
- Password prompted on first run (saved to `infrastructure/local-secrets.yml`, gitignored)

### Secrets

All secrets are prompted on first Ansible run and saved to `infrastructure/local-secrets.yml` (gitignored, mode 0600). To re-prompt, run `reinstall.sh` or delete the file and re-run the playbook.

Environment variables are deployed to `/etc/profile.d/zbbs.sh` and the PHP-FPM pool config.

### Mercure
- Runs on localhost:3000, accessed only by the API server-side
- JWT secret configured during install

## Production Deployment

1. Edit `infrastructure/inventory/production.yml` with your server details
2. Set environment variables: `ZBBS_DB_PASSWORD`, `ZBBS_JWT_PASSPHRASE`, `MERCURE_JWT_SECRET`
3. Deploy:
   ```bash
   cd /var/www/zbbs/infrastructure
   ansible-playbook -i inventory/production.yml playbooks/setup.yml
   ansible-playbook -i inventory/production.yml playbooks/deploy.yml
   ```

## Troubleshooting

### Apache won't start
Check if another service is using port 80:
```bash
sudo ss -tlnp | grep :80
```

### Database connection failed
Verify PostgreSQL is running and credentials are correct:
```bash
sudo -u postgres psql -c "\l"
psql -h localhost -U zbbs -d zbbs
```

### Mercure not connecting
Check the service status:
```bash
sudo systemctl status mercure
sudo journalctl -u mercure -f
```

## Development Setup

For local development with hot-reload workflows, see the platform-specific guides:

- [Windows (WSL2) Setup](docs/development/windows/) — step-by-step for Windows with WSL2
- [Linux Setup](docs/development/linux/) — step-by-step for native Linux

## Further Reading

- [Development Guide](docs/development.md) — project layout, making changes, troubleshooting
- [Template System](docs/templates.md) — ANSI template format for the terminal client

## License

AGPL-3.0 - See LICENSE file for details.
