#!/bin/bash
# agentlink v2 API integration smoke test (account + team + device sessions).
# Usage: SERVER=http://YOUR_SERVER_IP:8080 bash api_test.sh
#
# Drives the released auth surface end to end with curl: register an account,
# device-login, create+select a team, create a project, lock+apply a file, read
# it back from the snapshot, then log out and confirm the device session is
# rejected. There is no registration password and no API key.

set -uo pipefail

SERVER="${SERVER:-http://localhost:8080}"
PASS=0
FAIL=0
STAMP=$$
USER="intuser${STAMP}"                 # unique per run; must match [a-z][a-z0-9]{2,}
PASSWORD="correct horse battery ${STAMP}"
DEVICE="int-dev-${STAMP}"
SESSION="main"
CRED=""

check() {
  local name="$1" status="$2"
  if [[ "$status" -eq 0 ]]; then
    echo "  PASS  $name"; ((PASS++))
  else
    echo "  FAIL  $name"; ((FAIL++))
  fi
}

# req METHOD PATH BODY [DEVICE_CRED] [SESSION] → prints HTTP status, body in /tmp/al-resp.json
req() {
  local method="$1" path="$2" body="$3" cred="${4:-}" session="${5:-}"
  local headers=(-H "Content-Type: application/json")
  [[ -n "$cred" ]] && headers+=(-H "Authorization: Device $cred")
  [[ -n "$session" ]] && headers+=(-H "X-Agentlink-Session: $session")
  if [[ -n "$body" ]]; then
    curl -s -o /tmp/al-resp.json -w "%{http_code}" -X "$method" "$SERVER$path" "${headers[@]}" -d "$body"
  else
    curl -s -o /tmp/al-resp.json -w "%{http_code}" -X "$method" "$SERVER$path" "${headers[@]}"
  fi
}

extract() { python3 -c "import json;j=json.load(open('/tmp/al-resp.json'));print($1)" 2>/dev/null; }
is2xx() { [[ "$1" =~ ^2 ]]; }

echo "=========================================="
echo " agentlink v2 API Integration Smoke Test"
echo " Server: $SERVER   User: $USER"
echo "=========================================="

echo ""; echo "=== Health ==="
code=$(req GET /health "")
if [[ "$code" == "200" ]] && [[ "$(extract "j['ok']")" == "True" ]]; then
  check "health check returned ok" 0
else
  check "health check (got $code)" 1
fi

echo ""; echo "=== Register account ==="
code=$(req POST /api/auth/register "{\"username\":\"$USER\",\"password\":\"$PASSWORD\"}")
is2xx "$code" && check "account registered" 0 || check "register (got $code)" 1

echo ""; echo "=== Device login ==="
code=$(req POST /api/auth/device-login "{\"username\":\"$USER\",\"password\":\"$PASSWORD\",\"device_name\":\"$DEVICE\"}")
CRED=$(extract "j['device_credential']")
if is2xx "$code" && [[ -n "$CRED" ]]; then
  check "device logged in, credential received" 0
else
  check "device login (got $code)" 1
fi

echo ""; echo "=== Auth negatives ==="
code=$(req GET /api/teams "")
[[ "$code" == "401" ]] && check "unauthenticated request returns 401" 0 || check "no-auth (got $code)" 1
code=$(req GET /api/teams "" "ds_invalidcredential")
[[ "$code" == "401" ]] && check "bad device credential returns 401" 0 || check "bad-cred (got $code)" 1

echo ""; echo "=== Team create ==="
code=$(req POST /api/teams "{\"name\":\"Integration Team\"}" "$CRED")
TEAM=$(extract "j['team']['id']")
if is2xx "$code" && [[ -n "$TEAM" ]]; then
  check "team created ($TEAM)" 0
else
  check "team create (got $code)" 1
fi

echo ""; echo "=== Project create ==="
code=$(req POST "/api/teams/$TEAM/projects" "{\"name\":\"Prototype\"}" "$CRED")
PROJECT=$(extract "j['id']")
if is2xx "$code" && [[ -n "$PROJECT" ]]; then
  check "project created ($PROJECT)" 0
else
  check "project create (got $code)" 1
fi

echo ""; echo "=== Lock + apply ==="
code=$(req POST "/api/teams/$TEAM/locks/acquire" "{\"project_id\":\"$PROJECT\",\"path\":\"index.html\"}" "$CRED" "$SESSION")
is2xx "$code" && check "file lock acquired" 0 || check "lock acquire (got $code)" 1

code=$(req POST "/api/teams/$TEAM/projects/$PROJECT/apply" "{\"path\":\"index.html\",\"content\":\"<h1>integration</h1>\"}" "$CRED" "$SESSION")
is2xx "$code" && check "file applied (committed)" 0 || check "apply (got $code)" 1

echo ""; echo "=== Snapshot ==="
code=$(req GET "/api/teams/$TEAM/projects/$PROJECT/snapshot" "" "$CRED")
CONTENT=$(extract "[f['content'] for f in j['files'] if f['path']=='index.html'][0]")
if is2xx "$code" && [[ "$CONTENT" == "<h1>integration</h1>" ]]; then
  check "snapshot returns applied content" 0
else
  check "snapshot (got $code, content='$CONTENT')" 1
fi

echo ""; echo "=== Members ==="
code=$(req GET "/api/teams/$TEAM/members" "" "$CRED")
MEMBERS=$(extract "len(j['members'])")
if is2xx "$code" && [[ "${MEMBERS:-0}" -ge 1 ]]; then
  check "team lists $MEMBERS member(s)" 0
else
  check "members (got $code)" 1
fi

echo ""; echo "=== Logout revokes device session ==="
code=$(req POST /api/auth/device-logout "" "$CRED")
is2xx "$code" && check "device logged out" 0 || check "logout (got $code)" 1

code=$(req GET /api/teams "" "$CRED")
[[ "$code" == "401" ]] && check "revoked credential returns 401" 0 || check "post-logout (got $code)" 1

echo ""
echo "=========================================="
echo " Results: $PASS passed / $FAIL failed"
echo "=========================================="
[[ "$FAIL" -eq 0 ]]
