#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if [ -z "$1" ]; then
    echo "Usage: $0 <username>"
    echo "Example: $0 jdafoe"
    exit 1
fi

. /etc/profile.d/zbbs.sh
php "$SCRIPT_DIR/api/bin/console" zbbs:reset-password "$1"
