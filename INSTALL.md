# WebBBS Installation Guide

## Prerequisites

- Debian 12 (Bookworm) or compatible Linux distribution
- sudo access

## Quick Start

Run this single command to install everything:

```bash
curl -fsSL https://raw.githubusercontent.com/jeffdafoe/webbbs/main/install.sh | sudo bash
```

Or manually:

```bash
# Install git and ansible
sudo apt update && sudo apt install -y git ansible

# Clone and run setup
git clone https://github.com/jeffdafoe/webbbs.git /var/www/webbbs
cd /var/www/webbbs/infrastructure
sudo ansible-playbook -i inventory/local.yml playbooks/setup.yml
```

## What Gets Installed

| Component | Version | Purpose |
|-----------|---------|---------|
| PHP | 8.3 | Backend runtime |
| Composer | Latest | PHP package manager |
| Symfony CLI | Latest | Development server |
| PostgreSQL | 15 | Database |
| Node.js | 20 LTS | Frontend build tools |
| Angular CLI | Latest | Frontend framework |
| Mercure | 0.16 | Real-time messaging |
| Apache | Latest | Web server |

## Default Configuration

### Database
- Host: `localhost`
- Database: `webbbs`
- User: `webbbs`
- Password: `webbbs_dev` (change in production!)

### Ports
| Service | Port |
|---------|------|
| Apache (API proxy) | 8000 |
| Apache (Frontend proxy) | 4200 |
| Mercure hub | 3000 |
| Symfony dev server | 8001 |
| Angular dev server | 4201 |

### Mercure
- JWT secret is set via environment variable `MERCURE_JWT_SECRET`
- Default development secret is in the systemd service file

## Development

After installation, start the development servers:

```bash
# Terminal 1 - Start Symfony API
cd /var/www/webbbs/api
symfony server:start --port=8001

# Terminal 2 - Start Angular frontend
cd /var/www/webbbs/frontend
ng serve --port=4201
```

Access the application:
- Frontend: http://localhost:4200 (via Apache) or http://localhost:4201 (direct)
- API: http://localhost:8000 (via Apache) or http://localhost:8001 (direct)

## Production Deployment

1. Edit `infrastructure/inventory/production.yml` with your server details:
   ```yaml
   all:
     hosts:
       production:
         ansible_host: YOUR_SERVER_IP
         ansible_user: YOUR_USER
   ```

2. Update passwords in `infrastructure/group_vars/production.yml`

3. Deploy:
   ```bash
   cd /var/www/webbbs/infrastructure
   ansible-playbook -i inventory/production.yml playbooks/deploy.yml
   ```

## Troubleshooting

### Apache won't start
Check if another service is using the ports:
```bash
sudo ss -tlnp | grep -E ':(80|8000|4200)'
```

### Database connection failed
Verify PostgreSQL is running and credentials are correct:
```bash
sudo -u postgres psql -c "\l"
psql -h localhost -U webbbs -d webbbs
```

### Mercure not connecting
Check the service status:
```bash
sudo systemctl status mercure
sudo journalctl -u mercure -f
```

## License

AGPL-3.0 - See LICENSE file for details.
