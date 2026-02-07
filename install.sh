#!/bin/bash
set -e

echo "==================================="
echo "  WebBBS Installer"
echo "==================================="
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

# Detect the actual user (not root)
ACTUAL_USER="${SUDO_USER:-$USER}"
if [ "$ACTUAL_USER" = "root" ]; then
    echo "Warning: Running as root user. Project will be owned by root."
    ACTUAL_USER="root"
fi

echo "Installing for user: $ACTUAL_USER"
echo

# Install dependencies
echo "[1/5] Installing system dependencies..."
apt update
apt install -y git ansible curl

# Clone repository
echo "[2/5] Cloning WebBBS repository..."
if [ -d "/var/www/webbbs" ]; then
    echo "Directory /var/www/webbbs already exists. Pulling latest..."
    cd /var/www/webbbs
    git pull
else
    git clone https://github.com/jeffdafoe/webbbs.git /var/www/webbbs
fi

# Set ownership
chown -R "$ACTUAL_USER:$ACTUAL_USER" /var/www/webbbs

# Run Ansible playbook
echo "[3/5] Running Ansible setup (this may take a few minutes)..."
cd /var/www/webbbs/infrastructure
ansible-playbook -i inventory/local.yml playbooks/setup.yml

# Initialize Symfony (if not already done)
echo "[4/5] Initializing Symfony project..."
cd /var/www/webbbs
if [ ! -d "api/vendor" ]; then
    sudo -u "$ACTUAL_USER" symfony new api --webapp
    cd api
    sudo -u "$ACTUAL_USER" composer require symfony/mercure-bundle
    cd ..
else
    echo "Symfony project already exists, skipping..."
fi

# Initialize Angular (if not already done)
echo "[5/5] Initializing Angular project..."
if [ ! -d "frontend/node_modules" ]; then
    sudo -u "$ACTUAL_USER" ng new frontend --routing --style=scss --standalone --skip-git
else
    echo "Angular project already exists, skipping..."
fi

# Set final ownership
chown -R "$ACTUAL_USER:$ACTUAL_USER" /var/www/webbbs

echo
echo "==================================="
echo "  Installation Complete!"
echo "==================================="
echo
echo "To start development:"
echo
echo "  Terminal 1 (API):"
echo "    cd /var/www/webbbs/api"
echo "    symfony server:start --port=8001"
echo
echo "  Terminal 2 (Frontend):"
echo "    cd /var/www/webbbs/frontend"
echo "    ng serve --port=4201"
echo
echo "Access:"
echo "  Frontend: http://localhost:4201"
echo "  API:      http://localhost:8001"
echo
