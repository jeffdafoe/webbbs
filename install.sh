#!/bin/bash
set -e

echo -e "\033[1;36m==================================="
echo "  ZBBS Installer"
echo -e "===================================\033[0m"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo "Please run as root (sudo)"
    exit 1
fi

# Install dependencies
echo -e "\033[1m[1/4] Installing system dependencies...\033[0m"
apt update
apt install -y git ansible curl

# Clone repository
echo -e "\033[1m[2/4] Cloning ZBBS repository...\033[0m"
if [ -d "/var/www/zbbs" ]; then
    echo "Directory /var/www/zbbs already exists. Pulling latest..."
    cd /var/www/zbbs
    git pull
else
    git clone https://github.com/jeffdafoe/zbbs.git /var/www/zbbs
fi

# Run setup playbook (installs PHP, PostgreSQL, Node, Mercure, Apache)
echo -e "\033[1m[3/4] Running setup...\033[0m"
cd /var/www/zbbs/infrastructure
export ANSIBLE_CONFIG=/var/www/zbbs/infrastructure/ansible.cfg
ansible-playbook -i inventory/local.yml playbooks/setup.yml

# Run deploy playbook (composer install, npm install, migrations, JWT keys)
echo -e "\033[1m[4/4] Running deploy...\033[0m"
ansible-playbook -i inventory/local.yml playbooks/deploy.yml --extra-vars "run_migrations=true"

echo ""
echo -e "\033[1;32m==================================="
echo "  Installation complete!"
echo -e "===================================\033[0m"
echo ""
