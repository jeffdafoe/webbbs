#!/bin/bash
# Smoke test: verify web server is serving all endpoints correctly
# Usage: sudo bash tests/smoke/web-server.sh

set -e
. /etc/profile.d/zbbs.sh

PASS=0
FAIL=0
BASE="http://zbbs.local"

check() {
    local label="$1"
    local expected="$2"
    local actual="$3"

    if [ "$actual" = "$expected" ]; then
        echo "  PASS  $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL  $label (expected $expected, got $actual)"
        FAIL=$((FAIL + 1))
    fi
}

echo ""
echo "=== Web Server Smoke Test ==="
echo ""

# Landing page
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/")
check "Landing page (GET /)" "200" "$STATUS"

# Terminal client
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/terminal/")
check "Terminal client (GET /terminal/)" "200" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/terminal/main.js")
check "Terminal JS bundle (GET /terminal/main.js)" "200" "$STATUS"

# API login
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"jdafoe","password":"spotteddog"}')
check "API login (POST /api/login)" "200" "$STATUS"

# API auth - login then hit /api/me
TOKEN=$(curl -s -X POST "$BASE/api/login" \
    -H "Content-Type: application/json" \
    -d '{"username":"jdafoe","password":"spotteddog"}' \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])" 2>/dev/null)

if [ -n "$TOKEN" ]; then
    STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/me" \
        -H "Authorization: Bearer $TOKEN")
    check "API auth (GET /api/me with JWT)" "200" "$STATUS"

    # Verify response contains expected fields
    BODY=$(curl -s "$BASE/api/me" -H "Authorization: Bearer $TOKEN")
    if echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'username' in d and 'roles' in d" 2>/dev/null; then
        check "API auth response has username and roles" "200" "200"
    else
        check "API auth response has username and roles" "valid json" "missing fields"
    fi
else
    check "API auth (GET /api/me with JWT)" "200" "no token"
    check "API auth response has username and roles" "valid json" "skipped"
fi

# API without auth should 401
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/me")
check "API without auth (GET /api/me)" "401" "$STATUS"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
echo ""

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
