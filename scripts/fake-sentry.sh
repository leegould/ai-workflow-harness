#!/bin/sh
# Sends a Sentry-shaped issue webhook to the harness, standing in for a real
# Sentry project: an error in the deployed poster becomes a ticket. Run it
# twice to see the repeat added to the same ticket rather than a new one.
#
#   scripts/fake-sentry.sh                 # to http://127.0.0.1:8080
#   HARNESS=http://host:port scripts/fake-sentry.sh
#
# If SENTRY_WEBHOOK_SECRET is set, the body is signed the way Sentry signs it.
set -eu
url="${HARNESS:-http://127.0.0.1:8080}/api/sources/sentry"
body='{
  "action": "created",
  "data": {
    "issue": {
      "id": "4501",
      "shortId": "POSTER-7",
      "title": "Unknown theme \"Blue\": rendered with the default styles",
      "culprit": "main.Render (poster.go)",
      "level": "warning",
      "count": "14",
      "web_url": "https://sentry.example.invalid/issues/4501/",
      "metadata": { "type": "UnknownTheme", "value": "theme \"Blue\" in poster.json matches no theme, so the poster fell back to the default look", "filename": "poster.go", "function": "Render" }
    }
  }
}'
sig=""
if [ -n "${SENTRY_WEBHOOK_SECRET:-}" ]; then
	sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$SENTRY_WEBHOOK_SECRET" | sed 's/^.* //')
fi
curl -sS -X POST "$url" -H 'Content-Type: application/json' -H 'Sentry-Hook-Resource: issue' \
	-H "Sentry-Hook-Signature: $sig" --data "$body"
echo
