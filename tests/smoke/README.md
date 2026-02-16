# Smoke Tests

Integration tests that verify the deployed environment is working correctly.

## Running

All scripts run from WSL as root:

```bash
sudo bash tests/smoke/web-server.sh
```

## Tests

### web-server.sh

Verifies nginx is serving all endpoints through PHP-FPM correctly:

- Landing page returns 200
- Terminal client serves static files (index.html, main.js)
- API login endpoint accepts credentials and returns a JWT
- API authenticated endpoint (/api/me) accepts the JWT and returns user data
- API authenticated endpoint rejects requests without a JWT (401)

Requires the `jdafoe` dev account with password `spotteddog`.
