#!/bin/bash
set -e

# Usage:
#   First time:  curl -sSL https://raw.githubusercontent.com/jeffdafoe/zbbs/main/install.sh -o /tmp/install.sh && sudo bash /tmp/install.sh
#   Re-install:  sudo bash /opt/zbbs/install.sh
#   Deploy only: sudo bash /opt/zbbs/deploy.sh

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
if [ -d "/opt/zbbs" ]; then
    echo "Directory /opt/zbbs already exists. Pulling latest..."
    cd /opt/zbbs
    git pull
else
    git clone https://github.com/jeffdafoe/zbbs.git /opt/zbbs
fi

# Run setup playbook (will prompt for secrets on first run)
echo -e "\033[1m[3/4] Running setup...\033[0m"
cd /opt/zbbs/infrastructure
export ANSIBLE_CONFIG=/opt/zbbs/infrastructure/ansible.cfg
ansible-playbook -i inventory/production.yml playbooks/setup.yml

# Run deploy playbook
echo -e "\033[1m[4/4] Running deploy...\033[0m"
ansible-playbook -i inventory/production.yml playbooks/deploy.yml

echo ""
echo -e "\033[1;32m==================================="
echo "  Installation complete!"
echo -e "===================================\033[0m"
echo ""
echo "To deploy updates later, run:"
echo "  sudo bash /opt/zbbs/deploy.sh"
echo ""
