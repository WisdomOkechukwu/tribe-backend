#!/usr/bin/env bash
# A guided tour of the API with curl. Run it while server, worker and
# scheduler are running (see README "Run it"):
#
#   ./scripts/try-the-api.sh                 # uses http://localhost:8080
#   API=http://localhost:8081 ./scripts/try-the-api.sh
#
# It signs a new user up, adds test money, swaps USD -> GBP on the NGN rail,
# replays a request with the same Idempotency-Key, sends money, signs in on a
# SECOND phone (which signs the first phone out), and checks the books.
set -euo pipefail

API="${API:-http://localhost:8080}"
MOBILE="$API/api/mobile/v1"
ADMIN="$API/admin-api"
PHONE_A="phone-A-$RANDOM"
PHONE_B="phone-B-$RANDOM"
EMAIL="demo$RANDOM@example.com"
TAG="demo$RANDOM"

title() { printf "\n\033[1;34m▶ %s\033[0m\n" "$1"; }
new_key() { uuidgen | tr 'A-Z' 'a-z'; }

title "1. Sign up: ask for an email code (the worker 'sends' it; locally the API also returns it)"
CODE=$(curl -s -X POST "$MOBILE/auth/email/send-code" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\"}" | jq -r '.data.debug_code')
echo "code: $CODE"

title "2. Verify the code -> verification_token"
TOKEN=$(curl -s -X POST "$MOBILE/auth/email/verify" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"code\":\"$CODE\"}" | jq -r '.data.verification_token')

title "3. Register on PHONE A (a 'new entry': this phone becomes the active one)"
REGISTERED=$(curl -s -X POST "$MOBILE/auth/register" -H 'Content-Type: application/json' -d "{
  \"email\":\"$EMAIL\",\"verification_token\":\"$TOKEN\",\"password\":\"Tribe@2026\",
  \"first_name\":\"Demo\",\"last_name\":\"User\",\"tag\":\"$TAG\",\"country\":\"NG\",
  \"device\":{\"imei\":\"$PHONE_A\",\"name\":\"Phone A\",\"platform\":\"ios\"}}")
ACCESS_A=$(echo "$REGISTERED" | jq -r '.data.tokens.access_token')
USER_ID=$(echo "$REGISTERED" | jq -r '.data.user.id')
AUTH_A=(-H "Authorization: Bearer $ACCESS_A" -H "X-Device-Imei: $PHONE_A" -H 'Content-Type: application/json')

title "4. Create a PIN"
curl -s -X POST "$MOBILE/pin" "${AUTH_A[@]}" -H "Idempotency-Key: $(new_key)" -d '{"pin":"2580"}' | jq -c '.message'

title "5. Admins credit 100 USD from the vault's FREE funds (finance requests, only a SUPER ADMIN approves)"
SUPER_TOKEN=$(curl -s -X POST "$ADMIN/sessions" -H 'Content-Type: application/json' \
  -d '{"email":"super@tribe.africa","password":"Tribe@2026"}' | jq -r '.data.access_token')
FINANCE_TOKEN=$(curl -s -X POST "$ADMIN/sessions" -H 'Content-Type: application/json' \
  -d '{"email":"finance@tribe.africa","password":"Tribe@2026"}' | jq -r '.data.access_token')
echo "free USD before (vault minus what customers are owed):"
curl -s "$ADMIN/free-funds" -H "Authorization: Bearer $FINANCE_TOKEN" | jq -c '.data[] | select(.asset_code == "USD")'
SEED_FUNDING=$(curl -s -X POST "$ADMIN/treasury-fundings" -H "Authorization: Bearer $FINANCE_TOKEN" -H "Idempotency-Key: $(new_key)" \
  -H 'Content-Type: application/json' -d "{\"asset\":\"USD\",\"amount\":\"1000\",\"external_reference\":\"BANK-$RANDOM$RANDOM\",\"note\":\"Free funds for credits\"}" | jq -r '.data.id')
curl -s -X POST "$ADMIN/treasury-fundings/$SEED_FUNDING/approve" -H "Authorization: Bearer $SUPER_TOKEN" -H "Idempotency-Key: $(new_key)" | jq -c '{message}'
CREDIT_ID=$(curl -s -X POST "$ADMIN/customers/$USER_ID/adjustments" -H "Authorization: Bearer $FINANCE_TOKEN" -H "Idempotency-Key: $(new_key)" \
  -H 'Content-Type: application/json' -d '{"kind":"credit","asset":"USD","amount":"100","reason":"Demo credit"}' | jq -r '.data.id')
curl -s -X POST "$ADMIN/customer-adjustments/$CREDIT_ID/approve" -H "Authorization: Bearer $SUPER_TOKEN" -H "Idempotency-Key: $(new_key)" \
  | jq -c '.data | {kind, status, amount_decimal, approved_by_name}'

title "6. Quote 20 USD -> GBP. The global provider has uses_ngn_rail ON, so the route is USD -> NGN -> GBP"
QUOTE=$(curl -s -X POST "$MOBILE/quotes" "${AUTH_A[@]}" -d '{"transaction_type":"swap","from_asset":"USD","to_asset":"GBP","amount":"20"}')
echo "$QUOTE" | jq -c '.data | {from_amount, fee, to_amount, uses_ngn_rail, route: [.route[] | "\(.from_asset)->\(.to_asset) @ \(.rate)"]}'
QUOTE_ID=$(echo "$QUOTE" | jq -r '.data.id')

title "7. Swap with the quote, then send the SAME request again with the SAME Idempotency-Key"
SWAP_KEY=$(new_key)
curl -s -X POST "$MOBILE/swaps" "${AUTH_A[@]}" -H "Idempotency-Key: $SWAP_KEY" -d "{\"quote_id\":\"$QUOTE_ID\",\"pin\":\"2580\"}" | jq -c '{message, id: .data.id}'
curl -s -i -X POST "$MOBILE/swaps" "${AUTH_A[@]}" -H "Idempotency-Key: $SWAP_KEY" -d "{\"quote_id\":\"$QUOTE_ID\",\"pin\":\"2580\"}" \
  | grep -i -E '^idempotent-replay|"id"' | head -2 | cut -c1-120
echo "(same transaction id, and the header Idempotent-Replay: true -> nothing was charged twice)"

title "8. Send 5 USD to @ada (Tribe to Tribe, instant)"
curl -s -X POST "$MOBILE/sends" "${AUTH_A[@]}" -H "Idempotency-Key: $(new_key)" \
  -d '{"recipient_kind":"tribe","handle":"@ada","asset":"USD","amount":"5","pin":"2580"}' | jq -c '{message, status: .data.status}'

title "9. Send 3 USD to a bank account containing 'fail' -> pending now, the WORKER refunds it a moment later"
FAILED_ID=$(curl -s -X POST "$MOBILE/sends" "${AUTH_A[@]}" -H "Idempotency-Key: $(new_key)" \
  -d '{"recipient_kind":"bank","handle":"fail-12345678","recipient_name":"Test","asset":"USD","amount":"3","pin":"2580"}' | jq -r '.data.id')
sleep 3
curl -s "$MOBILE/transactions/$FAILED_ID" "${AUTH_A[@]}" | jq -c '.data.transaction | {status, failure_reason}'

title "10. Wallet balances (computed from snapshot + journal legs)"
curl -s "$MOBILE/wallets" "${AUTH_A[@]}" | jq -c '.data.balances'

title "11. Sign in on PHONE B -> 202 'verify_new_device' (a code is emailed)"
LOGIN_B=$(curl -s -X POST "$MOBILE/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"Tribe@2026\",\"device\":{\"imei\":\"$PHONE_B\",\"name\":\"Phone B\"}}")
echo "$LOGIN_B" | jq -c '.data | {next, verification_id}'

title "12. Enter the code on PHONE B -> phone B becomes active, phone A is signed out"
curl -s -X POST "$MOBILE/auth/device/verify" -H 'Content-Type: application/json' -H "X-Device-Imei: $PHONE_B" -d "{
  \"verification_id\":\"$(echo "$LOGIN_B" | jq -r '.data.verification_id')\",\"code\":\"$(echo "$LOGIN_B" | jq -r '.data.debug_code')\"}" | jq -c '.message'
echo "Phone A tries again:"
curl -s "$MOBILE/wallets" "${AUTH_A[@]}" | jq -c '{code, message}'

title "13. Admin: sign in, check the books, run the daily snapshot by hand"
ADMIN_TOKEN=$(curl -s -X POST "$ADMIN/sessions" -H 'Content-Type: application/json' \
  -d '{"email":"super@tribe.africa","password":"Tribe@2026"}' | jq -r '.data.access_token')
curl -s "$ADMIN/ledger/verify" -H "Authorization: Bearer $ADMIN_TOKEN" | jq -c '.data'
curl -s -X POST "$ADMIN/jobs/daily-snapshot/run" -H "Authorization: Bearer $ADMIN_TOKEN" -H "Idempotency-Key: $(new_key)" | jq -c '.message'

title "14. Treasury funding needs TWO admins: finance requests, the same person cannot approve, super approves"
FINANCE_TOKEN=$(curl -s -X POST "$ADMIN/sessions" -H 'Content-Type: application/json' \
  -d '{"email":"finance@tribe.africa","password":"Tribe@2026"}' | jq -r '.data.access_token')
FUNDING_ID=$(curl -s -X POST "$ADMIN/treasury-fundings" -H "Authorization: Bearer $FINANCE_TOKEN" -H "Idempotency-Key: $(new_key)" \
  -H 'Content-Type: application/json' -d "{\"asset\":\"USD\",\"amount\":\"5000\",\"external_reference\":\"BANK-$RANDOM$RANDOM\",\"note\":\"Demo top-up\"}" | jq -r '.data.id')
echo "requested $FUNDING_ID (pending, nothing posted yet)"
echo "finance tries to approve its own request:"
curl -s -X POST "$ADMIN/treasury-fundings/$FUNDING_ID/approve" -H "Authorization: Bearer $FINANCE_TOKEN" -H "Idempotency-Key: $(new_key)" | jq -c '{code, message}'
echo "super approves it:"
curl -s -X POST "$ADMIN/treasury-fundings/$FUNDING_ID/approve" -H "Authorization: Bearer $ADMIN_TOKEN" -H "Idempotency-Key: $(new_key)" | jq -c '.data | {status, approved_by_name, amount_decimal, journal_entry_id}'

title "15. Provider webhook: a good signature is accepted once, a replay is ignored, a bad signature is refused"
WEBHOOK_BODY="{\"event_id\":\"evt-$RANDOM$RANDOM\",\"type\":\"payout.succeeded\",\"data\":{\"transaction_id\":\"$FAILED_ID\"}}"
SIGNATURE="sha256=$(printf '%s' "$WEBHOOK_BODY" | openssl dgst -sha256 -hmac 'simulator-webhook-secret' | sed 's/^.* //')"
curl -s -X POST "$API/webhooks/global-simulator" -H "X-Tribe-Signature: $SIGNATURE" -d "$WEBHOOK_BODY" | jq -c '.data'
curl -s -X POST "$API/webhooks/global-simulator" -H "X-Tribe-Signature: $SIGNATURE" -d "$WEBHOOK_BODY" | jq -c '.data'
curl -s -X POST "$API/webhooks/global-simulator" -H "X-Tribe-Signature: sha256=forged" -d "$WEBHOOK_BODY" | jq -c '{code, message}'

title "16. Live rates: listen to the SSE stream for 12 seconds (Ctrl+C to stop early)"
LOGIN_B2=$(curl -s -X POST "$MOBILE/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"Tribe@2026\",\"device\":{\"imei\":\"$PHONE_B\"}}" | jq -r '.data.tokens.access_token')
curl -s -N --max-time 12 "$MOBILE/rates/stream" -H "Authorization: Bearer $LOGIN_B2" -H "X-Device-Imei: $PHONE_B" \
  | grep '^event:' || true

title "Done ✔"
