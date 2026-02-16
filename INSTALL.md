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

# Clone and run setup
git clone https://github.com/jeffdafoe/zbbs.git /var/www/zbbs
cd /var/www/zbbs/infrastructure
sudo ansible-playbook -i inventory/local.yml playbooks/setup.yml

# Install dependencies
cd /var/www/zbbs/api && composer install
cd /var/www/zbbs/clients/terminal && npm install
cd /var/www/zbbs/clients/modern && npm install

# Run migrations and deploy
cd /var/www/zbbs/infrastructure
ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars 'run_migrations=true'

# Build clients
cd /var/www/zbbs/clients/terminal && node build.mjs
cd /var/www/zbbs/clients/modern && npx ng build

# Configure the BBS (creates sysop account and initial settings)
source /etc/profile.d/zbbs.sh && cd /var/www/zbbs/api && php bin/console zbbs:setup
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
- Password configured in `infrastructure/inventory/local.yml` (defaults to `zbbs_dev` for development)

### Mercure
- Runs on localhost:3000, accessed only by the API server-side
- JWT secret set via environment variable `MERCURE_JWT_SECRET`

## Production Deployment

1. Edit `infrastructure/inventory/production.yml` with your server details
2. Set environment variables for passwords (or update the inventory)
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
