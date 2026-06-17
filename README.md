[![Build & Test](https://github.com/markmnl/fmsg-webapi/actions/workflows/build-test.yml/badge.svg)](https://github.com/markmnl/fmsg-webapi/actions/workflows/build-test.yml)

# fmsg-webapi

HTTP API providing user/client message handling for an fmsg host. Exposes CRUD operations for a messaging datastore backed by PostgreSQL. Authentication is delegated to an external system — this service validates JWT tokens and enforces fine-grained authorisation rules based on the user identity they contain.

## Environment Variables

| Variable            | Default                  | Description                                             |
| ------------------- | ------------------------ | ------------------------------------------------------- |
| `FMSG_DATA_DIR`     | *(required)*             | Path where message data files are stored, e.g. `/var/lib/fmsgd/` |
| `FMSG_JWT_JWKS_URL` | *(prod)*                 | JWKS endpoint for the configured identity provider (e.g. `https://idp.example.com/.well-known/jwks.json`). When set, the API verifies RS256 JWTs. RSA public keys are fetched and cached, refreshed and looked up by the token's `kid` header. |
| `FMSG_JWT_ISSUER`   | *(prod, required with JWKS)* | Expected `iss` claim value (e.g. `https://idp.example.com/`). Tokens with a different issuer are rejected. This must exactly match the token issuer. |
| `FMSG_JWT_AUDIENCE` | *(prod, required with JWKS)* | Expected `aud` claim value for this application or API. |
| `FMSG_JWT_ADDRESS_CLAIM` | *(prod, required with JWKS)* | JWT claim name containing the fmsg address in `@user@domain` form, e.g. `fmsg_address` or a namespaced custom claim. |
| `FMSG_API_TOKEN_ED25519_PRIVATE_KEY` | *(optional)* | Base64-encoded Ed25519 private key or seed used to mint first-party JWTs from API keys. Required to enable `/fmsg/token` and sub-account routes. |
| `FMSG_API_TOKEN_ISSUER` | `fmsg-webapi` | Issuer for first-party API-key JWTs. |
| `FMSG_API_TOKEN_AUDIENCE` | `fmsg-webapi` | Audience for first-party API-key JWTs. |
| `FMSG_API_TOKEN_TTL` | `12h` | Lifetime of JWTs minted by `POST /fmsg/token`. |
| `FMSG_TRUSTED_PROXIES` | *(optional)* | Comma-separated trusted proxy CIDRs/IPs for Gin client IP resolution. Leave unset to use direct client addresses for API-key CIDR checks. |
| `FMSG_TLS_CERT`     | *(optional)*             | Path to the TLS certificate file (e.g. `/etc/letsencrypt/live/example.com/fullchain.pem`). When set with `FMSG_TLS_KEY`, enables HTTPS. |
| `FMSG_TLS_KEY`      | *(optional)*             | Path to the TLS private key file (e.g. `/etc/letsencrypt/live/example.com/privkey.pem`). Must be set together with `FMSG_TLS_CERT`. |
| `FMSG_API_PORT`     | `443` (TLS) / `8000` (plain) | TCP port to listen on. |
| `FMSG_ID_URL`       | `http://127.0.0.1:8080`  | Base URL of the fmsgid identity service                 |
| `FMSG_API_MAX_DATA_SIZE`| `10`                 | Maximum message data size in megabytes                  |
| `FMSG_API_MAX_ATTACH_SIZE`| `10`               | Maximum attachment file size in megabytes               |
| `FMSG_API_MAX_MSG_SIZE`| `20`                  | Maximum total message size (data + attachments) in megabytes |
| `FMSG_API_SHORT_TEXT_SIZE`| `768`               | Maximum bytes of message body returned inline as `short_text` for `text/*` UTF-8 messages |
| `FMSG_CORS_ORIGINS` | *(optional)*             | Comma-separated list of browser origins allowed via CORS, e.g. `https://example.com,https://www.example.com`. Use `*` to allow any origin. When unset, no CORS headers are emitted (server-to-server callers are unaffected). |
| `FMSG_VAPID_PUBLIC_KEY` | *(optional)*         | VAPID public key (URL-safe base64) for Web Push. Browsers also pass this as `applicationServerKey`. Generate the pair once with `npx web-push generate-vapid-keys`. |
| `FMSG_VAPID_PRIVATE_KEY` | *(optional)*        | VAPID private key (URL-safe base64) for Web Push. |
| `FMSG_VAPID_SUBJECT` | *(optional)*            | VAPID `sub` contact, a `mailto:` or `https:` URL, e.g. `mailto:admin@example.com`. |
| `FMSG_PUSH_ICON_URL` | `/icon-192.png`         | Icon URL placed in the Web Push payload. |

Standard PostgreSQL environment variables (`PGHOST`, `PGPORT`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE`) are used for database connectivity.

fmsg-webapi assumes [fmsgd](https://github.com/markmnl/fmsgd) is the fmsg host
implementation and uses the same PostgreSQL database. The fmsgd schema remains
the base schema; this repository's [dd.sql](dd.sql) adds the API-key and
delegation tables used by fmsg-webapi.

A `.env` file placed in the working directory is loaded automatically at startup
(values in the environment take precedence).

## Authentication

Most `/fmsg/*` routes require an `Authorization: Bearer <token>` header. The
API can enable either or both authentication methods at startup:

- RS256/JWKS tokens from an external identity provider.
- First-party Ed25519 JWTs minted by `POST /fmsg/token` from opaque API keys.

Startup fails unless at least one method is configured.

### RS256 (production, JWKS-backed JWTs)

Active when `FMSG_JWT_JWKS_URL` is set. Tokens must be issued by the configured
identity provider and signed with RS256. The JWKS endpoint is polled on a
schedule; the provider can rotate keys by adding a new JWK with a fresh `kid`.

Required token header: `alg: RS256`, `kid: <known to JWKS>`, `typ: JWT`.

Relevant claims:

| Claim | Description |
| ----- | ----------- |
| `iss` | Must equal `FMSG_JWT_ISSUER`. |
| `aud` | Must contain `FMSG_JWT_AUDIENCE`. |
| configured address claim | The claim named by `FMSG_JWT_ADDRESS_CLAIM`; must contain the user address in `@user@domain` form. Tokens without this claim are rejected with `403 no fmsg account for this identity`. |
| `sub` | Provider-specific identity. It is validated as part of the signed token but is not used as the fmsg address in RS256 mode. |
| `exp` | Expiry timestamp (must be in the future, ±10 s leeway). |
| `iat` | Optional issued-at timestamp; validated when present. |
| `nbf` | Optional not-before timestamp; validated when present. |

A 10-second clock-skew leeway is applied to `iat`/`nbf`/`exp` validation. After
the address claim is validated, the API checks fmsgid to confirm the address is
known and accepting new messages.

Clients must send a JWT that matches the configured issuer and audience and
includes the configured address claim. Whether that token is an ID token or
access token is determined by the identity provider configuration for the
deployment.

### API Keys And First-Party JWTs

Active when `FMSG_API_TOKEN_ED25519_PRIVATE_KEY` is set. Programmatic clients
authenticate with opaque API keys bound to API-access grants. A grant may be a
derived sub-account such as `@alice_bot@example.com`, or an explicit delegated
identity such as `@sales@example.com`. The server stores only API-key hashes and
exchanges valid keys for short-lived Ed25519 JWTs.

API keys are sent only to `POST /fmsg/token`:

```http
Authorization: Bearer fmsgk_<key_id>_<secret>
```

The returned JWT contains `sub` (the granted address), `owner`, `api_key_id`,
`iss`, `aud`, `iat`, and `exp`. Protected routes re-check the backing key row on
each request, so deleting a grant or expiring its key invalidates existing
tokens before their normal expiry.

An RS256-authenticated owner can perform normal message routes as one of their
granted identities without changing request bodies:

```http
X-FMSG-Act-As: @user_bot@example.com
```

The requested address must be granted to the authenticated user and must exist
in fmsgid.

Apply [dd.sql](dd.sql) before enabling API-key auth. The file is idempotent and
also upgrades deployments that previously applied the earlier API-key table.

To set a custom per-owner grant limit, insert an owner config row:

```sql
INSERT INTO fmsg_api_sub_account (owner_addr, agent, max_sub_accounts)
VALUES ('@alice@example.com', '', 10)
ON CONFLICT (owner_addr, agent)
DO UPDATE SET max_sub_accounts = EXCLUDED.max_sub_accounts;
```

Operators can bootstrap or rotate keys without RS256 by using the built-in CLI
command. It uses the standard `PG*` connection environment variables and prints
the plaintext API key once.

Derived sub-account:

```bash
go run ./cmd/fmsg-webapi api-key create \
  -owner @alice@example.com \
  -agent bot \
  -cidr 203.0.113.0/24 \
  -expires 2026-12-31T00:00:00Z

go run ./cmd/fmsg-webapi api-key rotate \
  -owner @alice@example.com \
  -agent bot \
  -expires 2027-03-31T00:00:00Z
```

Delegated identity:

```bash
go run ./cmd/fmsg-webapi api-key create-delegation \
  -owner @mark@fmsg.io \
  -agent sales \
  -addr @sales@fmsg.io \
  -cidr 203.0.113.0/24 \
  -expires 2026-12-31T00:00:00Z

go run ./cmd/fmsg-webapi api-key rotate-delegation \
  -owner @mark@fmsg.io \
  -agent sales \
  -expires 2027-03-31T00:00:00Z
```

## Building

Requires **Go 1.25** or newer.

```bash
go build ./...
```

## Testing

```bash
go test ./...
```

## Running

### TLS mode (production)

Set `FMSG_TLS_CERT` and `FMSG_TLS_KEY` to enable HTTPS. Listens on port `443`
by default; override with `FMSG_API_PORT`.

```bash
export FMSG_DATA_DIR=/opt/fmsg/data
export FMSG_JWT_JWKS_URL=https://idp.example.com/.well-known/jwks.json
export FMSG_JWT_ISSUER=https://idp.example.com/
export FMSG_JWT_AUDIENCE=fmsg-web-client
export FMSG_JWT_ADDRESS_CLAIM=fmsg_address
# Optional: also enable programmatic API keys.
# export FMSG_API_TOKEN_ED25519_PRIVATE_KEY=$(openssl rand -base64 32)
export FMSG_TLS_CERT=/etc/letsencrypt/live/example.com/fullchain.pem
export FMSG_TLS_KEY=/etc/letsencrypt/live/example.com/privkey.pem
export PGHOST=localhost
export PGUSER=fmsg
export PGPASSWORD=secret
export PGDATABASE=fmsg

go run ./cmd/fmsg-webapi
```

### Plain HTTP mode (development / reverse proxy)

Omit the TLS variables to run a plain HTTP server. Override the port with
`FMSG_API_PORT` (default `8000`).

This is the recommended mode when fronting fmsg-webapi with Apache, nginx, or
any other reverse proxy that already terminates TLS (e.g. Apache on `:443`
proxying `https://fmsgapi.example.com/` to `http://127.0.0.1:8000/`).

```bash
export FMSG_DATA_DIR=/var/lib/fmsgd/
export FMSG_API_TOKEN_ED25519_PRIVATE_KEY=$(openssl rand -base64 32)
export PGHOST=localhost
export PGUSER=fmsg
export PGPASSWORD=secret
export PGDATABASE=fmsg

go run ./cmd/fmsg-webapi
```

The server starts on port `8000` by default. Override with `FMSG_API_PORT`.

The HTTP server is configured with `ReadHeaderTimeout: 10s`, `WriteTimeout: 65s`,
and `IdleTimeout: 120s`. The write timeout is generous so large attachment
transfers are not dropped prematurely. These timeouts do not apply to
`/fmsg/ws` connections: once upgraded, a WebSocket connection is hijacked from
the HTTP server and kept alive by its own ping/pong heartbeat.

## API Routes

All routes are prefixed with `/fmsg`. `POST /fmsg/token` accepts an API key and
returns a JWT. Other routes require a valid `Authorization: Bearer <token>`
header. The WebSocket route `/fmsg/ws` additionally accepts the token via an
`access_token` query parameter (browsers cannot set headers on a WebSocket).

Rate limiting is enforced at the host level (e.g. `nftables`) rather than in
the application.

| Method   | Path                                        | Description              |
| -------- | ------------------------------------------- | ------------------------ |
| `GET`    | `/fmsg`                          | List messages for user   |
| `GET`    | `/fmsg/sent`                     | List authored messages (sent + drafts) |
| `GET`    | `/fmsg/ws`                       | WebSocket for pushed event notifications |
| `POST`   | `/fmsg/token`                    | Exchange an API key for a JWT |
| `GET`    | `/fmsg/sub-accounts`             | List owned API-access grants |
| `POST`   | `/fmsg/sub-accounts`             | Create a derived sub-account API key |
| `POST`   | `/fmsg/sub-accounts/:agent/rotate-key` | Rotate a grant API key |
| `DELETE` | `/fmsg/sub-accounts/:agent`      | Delete a grant |
| `POST`   | `/fmsg`                          | Create a draft message   |
| `GET`    | `/fmsg/:id`                      | Retrieve a message       |
| `PUT`    | `/fmsg/:id`                      | Update a draft message   |
| `DELETE` | `/fmsg/:id`                      | Delete a draft message   |
| `POST`   | `/fmsg/:id/send`                 | Send a message           |
| `POST`   | `/fmsg/:id/read`                 | Mark a message as read   |
| `POST`   | `/fmsg/:id/add-to`               | Add recipients           |
| `GET`    | `/fmsg/:id/data`                 | Download message data    |
| `POST`   | `/fmsg/:id/attach`          | Upload an attachment     |
| `GET`    | `/fmsg/:id/attach/:filename`| Download an attachment   |
| `DELETE` | `/fmsg/:id/attach/:filename`| Delete an attachment     |
| `POST`   | `/fmsg/push/subscribe`           | Register a Web Push subscription |
| `DELETE` | `/fmsg/push/subscribe`           | Remove a Web Push subscription   |

The `/fmsg/push/subscribe` routes are registered only when Web Push is
configured (see [Web Push](#web-push)).

The `/fmsg/token` and `/fmsg/sub-accounts*` routes are registered only when
API-key auth is configured with `FMSG_API_TOKEN_ED25519_PRIVATE_KEY`.

### POST `/fmsg/token`

Exchanges an opaque API key for a short-lived JWT.

**Authentication:** `Authorization: Bearer fmsgk_<key_id>_<secret>`.

The key must be unexpired, match the stored hash, be used from an allowed CIDR,
and belong to a granted address that exists in fmsgid.

**Response:**

```json
{
  "access_token": "eyJ...",
  "token_type": "Bearer",
  "expires_in": 43200,
  "expires_at": "2026-12-31T12:00:00Z"
}
```

### GET `/fmsg/sub-accounts`

Lists API-access grants owned by the RS256-authenticated user. Grants with
`grant_type: "derived_sub_account"` use the `@user_agent@domain` convention.
Grants with `grant_type: "delegated_identity"` are explicit operator-created
delegations to arbitrary fmsg addresses.

**Response:**

```json
{
  "max_sub_accounts": 5,
  "sub_accounts": [
    {
      "agent": "bot",
      "addr": "@alice_bot@example.com",
      "grant_type": "derived_sub_account",
      "key_id": "abc",
      "allowed_cidrs": ["203.0.113.0/24"],
      "key_expires_at": "2026-12-31T00:00:00Z"
    },
    {
      "agent": "sales",
      "addr": "@sales@example.com",
      "grant_type": "delegated_identity",
      "display_name": "Sales mailbox",
      "key_id": "def",
      "allowed_cidrs": ["203.0.113.0/24"],
      "key_expires_at": "2026-12-31T00:00:00Z"
    }
  ]
}
```

### POST `/fmsg/sub-accounts`

Creates a derived sub-account and returns its plaintext API key once. Requires
RS256 owner authentication.

```json
{
  "agent": "bot",
  "allowed_cidrs": ["203.0.113.0/24"],
  "key_expires_at": "2026-12-31T00:00:00Z"
}
```

The derived address is `@user_bot@domain`. `agent` may contain letters, digits,
dots, and hyphens, but not underscores.

Delegated identities such as `@sales@example.com` are not created by this
self-service route. They are operator-created with `api-key create-delegation`
after the operator has confirmed the owner is allowed to manage the delegated
address.

### POST `/fmsg/sub-accounts/:agent/rotate-key`

Rotates any grant API key owned by the RS256-authenticated user and returns the
new plaintext key once. Requires `key_expires_at`; `allowed_cidrs` may be
supplied to replace the existing ranges.

### DELETE `/fmsg/sub-accounts/:agent`

Deletes a grant row and revokes future token exchange. Existing JWTs for that
key are rejected on their next protected-route request.

### GET `/fmsg/ws`

Upgrades the connection to a WebSocket over which the server pushes events that
pertain to the authenticated user. Intended for always-connected clients
(browsers, desktop apps): a single shared PostgreSQL listener fans events out to
all connected clients, so the number of connections does not consume database
connection-pool capacity.

**Authentication:** the JWT is verified exactly as for the REST API. Supply it
either as an `Authorization: Bearer <token>` header (non-browser clients) or as
an `access_token` query parameter (browsers, which cannot set headers on a
WebSocket). The handshake fails with `401`/`400`/`403`/`503` — the same statuses
as the REST middleware — before the connection is upgraded.

**Events:** every frame is a JSON envelope with a `type` discriminator so new
event types can be added without breaking clients:

```json
{ "type": "new_msg", "data": { ...message... } }
```

| `type`     | `data` | Sent when |
| ---------- | ------ | --------- |
| `new_msg`  | A message object, same shape as an item in the `GET /fmsg` list response (includes `id`). | A new message arrives for the authenticated user. |

A client only ever receives events for messages it is a participant on. The
server sends periodic WebSocket pings; clients should respond with pongs (most
WebSocket libraries do this automatically) to keep the connection alive.

**Browser example:**
```js
const ws = new WebSocket(`wss://api.example.com/fmsg/ws?access_token=${jwt}`);
ws.onmessage = (e) => {
  const event = JSON.parse(e.data);
  if (event.type === "new_msg") displayMessage(event.data);
};
```

### GET `/fmsg`

Returns messages where the authenticated user is a recipient (listed in `msg_to` or `msg_add_to`), ordered by message ID descending.

**Query parameters:**

| Parameter | Default | Description |
| --------- | ------- | ----------- |
| `limit`   | `20`    | Max messages to return (1–100) |
| `offset`  | `0`     | Number of messages to skip for pagination |

**Response:** JSON array of message objects. Each object has the same shape as the single-message response from `GET /fmsg/:id` (with an additional `id` field), including the `read`/`time_read` fields reflecting the caller's per-recipient read state. Message body data and attachment contents are not included — use the dedicated download endpoints instead.

### GET `/fmsg/sent`

Returns messages authored by the authenticated user (`msg.from_addr = <identity>`), ordered by message ID descending.

This includes both sent messages and drafts (`time_sent` may be `NULL`).

**Query parameters:**

| Parameter | Default | Description |
| --------- | ------- | ----------- |
| `limit`   | `20`    | Max messages to return (1–100) |
| `offset`  | `0`     | Number of messages to skip for pagination |

**Response:** JSON array of message objects with the same shape as `GET /fmsg`.

### POST `/fmsg`

Creates a draft message. The `from` address must match the authenticated user. The message is stored with `time_sent = NULL` (draft status) until explicitly sent.

Add-to recipients are not part of this body — they are added later via `POST /fmsg/:id/add-to`. Any `add_to` field sent here is ignored.

**Request body (JSON):**

| Field       | Type       | Required | Description |
| ----------- | ---------- | -------- | ----------- |
| `version`   | `int`      | yes      | Protocol version (currently `1`) |
| `from`      | `string`   | yes      | Sender address (`@user@domain`), must match JWT identity |
| `to`        | `string[]` | yes      | Recipient addresses (at least one) |
| `pid`       | `int`      | no       | Parent message ID (required for replies) |
| `topic`     | `string`   | no       | Thread topic (only on root messages without `pid`) |
| `type`      | `string`   | yes      | MIME type of the message body |
| `size`      | `int`      | yes      | Data size in bytes |
| `important` | `bool`     | no       | Mark message as important |
| `no_reply`  | `bool`     | no       | Indicate replies will be discarded |
| `data`      | `string`   | no       | Message body content |

**Response:** `201 Created` with `{"id": <int>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Missing/invalid fields, empty `to`, or `topic` set together with `pid` |
| `403`  | `from` does not match authenticated user |

### GET `/fmsg/:id`

Retrieves a single message by ID. The authenticated user must be a participant — the sender (`from`) or a recipient (listed in `to` or `add_to`).

**Response:** JSON message object:

```json
{
  "version": 1,
  "has_pid": false,
  "has_add_to": false,
  "common_type": false,
  "important": false,
  "no_reply": false,
  "deflate": false,
  "pid": null,
  "from": "@alice@example.com",
  "to": ["@bob@example.com"],
  "add_to": [
    {
      "add_to_from": "@bob@example.com",
      "to": ["@carol@example.com", "@dave@example.com"],
      "time": 1717459200.123
    }
  ],
  "time": null,
  "topic": "Hello",
  "type": "text/plain",
  "size": 12,
  "short_text": "hello world",
  "read": false,
  "time_read": null,
  "attachments": []
}
```

`add_to` is an array of add-to batches, one per `POST /fmsg/:id/add-to` call.
Each batch records who added the recipients (`add_to_from`), the recipients
added (`to`), and when the add-to happened (`time`, seconds since the Unix
epoch). `has_add_to` is `true` when the array is non-empty.

The `read` and `time_read` fields reflect the calling user's per-recipient
read state (set by `POST /fmsg/:id/read`). For the sender's own messages
they are always `false`/`null` (read state is recipient-scoped).

The `short_text` field is included only when the message `type` is `text/*` and
the stored body is valid UTF-8. When `FMSG_API_SHORT_TEXT_SIZE` is greater than
`0`, it contains up to that many bytes (default 768) of the body, truncated on
a UTF-8 rune boundary, so UIs can render a preview without a separate
`GET /fmsg/:id/data` round-trip. Set `FMSG_API_SHORT_TEXT_SIZE=0` to disable
`short_text` generation.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Message not found |
| `403`  | Authenticated user is not a participant |

### PUT `/fmsg/:id`

Updates a draft message. Only the owner (`from`) may update, and the message must not have been sent yet. Accepts the same body as `POST /fmsg`. Recipients in `to` are fully replaced.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid fields, or `topic` set together with `pid` |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### DELETE `/fmsg/:id`

Deletes a draft message and all its attachments from the database and disk. Only the owner may delete, and the message must not have been sent.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### POST `/fmsg/:id/send`

Marks a draft message as sent by setting `time_sent` to the current timestamp. Only the owner may send.

**Response:** `200 OK` with `{"id": <int>, "time": <float64>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `403`  | Not the owner |
| `404`  | Message not found |
| `409`  | Message already sent |

### POST `/fmsg/:id/read`

Marks a message as read by the authenticated recipient by setting `time_read`
on the caller's `msg_to` or `msg_add_to` row. Idempotent: re-reading an
already-read message returns the original `time_read` without updating it.

**Response:** `200 OK` with `{"id": <int>, "time_read": <float64>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Authenticated user is not a recipient of this message (or message does not exist) |

### POST `/fmsg/:id/add-to`

Adds additional recipients to an existing message. The authenticated user must be an existing participant — the sender (`from`) or a primary recipient (listed in `to`).

This endpoint records the add-to as a new `msg_add_to_batch` row (capturing the authenticated identity as `add_to_from` and a timestamp) and inserts the recipients into `msg_add_to` referencing that batch — all in a single transaction.

**Request body (JSON):**

| Field    | Type       | Required | Description |
| -------- | ---------- | -------- | ----------- |
| `add_to` | `string[]` | yes      | Addresses to add (at least one) |

New addresses must be distinct among themselves (case-insensitive).

**Response:** `200 OK` with `{"id": <int>, "added": <int>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Empty `add_to` or duplicate addresses |
| `403`  | Authenticated user is not an existing participant (sender or `to` recipient) |
| `404`  | Message not found |

### GET `/fmsg/:id/data`

Downloads the binary body of a message. The authenticated user must be a participant — the sender (`from`) or a recipient (listed in `to` or `add_to`).

**Response:** The raw message body file with `Content-Disposition: attachment` header. The `Content-Type` is inferred from the stored file extension.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Message not found or data file not available |
| `403`  | Authenticated user is not a participant |

### POST `/fmsg/:id/attach`

Uploads a file attachment for a draft message. Only the owner may upload, and the message must not have been sent.

**Request:** `multipart/form-data` with a `file` field.

**Response:** `201 Created` with `{"filename": "<string>", "size": <int>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Missing or invalid `file` field |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### GET `/fmsg/:id/attach/:filename`

Downloads an attachment by filename. The authenticated user must be a participant — the sender (`from`) or a recipient (listed in `to` or `add_to`).

**Response:** The attachment file with `Content-Disposition: attachment` header.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid filename |
| `403`  | Authenticated user is not a participant |
| `404`  | Message or attachment not found |

### DELETE `/fmsg/:id/attach/:filename`

Deletes an attachment from a draft message. Only the owner may delete, and the message must not have been sent.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid filename |
| `403`  | Not the owner, or message already sent |
| `404`  | Message or attachment not found |

### POST `/fmsg/push/subscribe`

Registers (or refreshes) a [Web Push](https://developer.mozilla.org/docs/Web/API/Push_API)
subscription for the authenticated user. The subscription is keyed by
`(user address, endpoint)` — POSTing the same endpoint again updates its keys.

**Request body:** the browser's `PushSubscription.toJSON()` output. The
`expirationTime` field is accepted but ignored.

```json
{
  "endpoint": "https://fcm.googleapis.com/fcm/send/abc...",
  "expirationTime": null,
  "keys": { "p256dh": "BPx...", "auth": "k9..." }
}
```

**Response:** `201 Created`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Malformed body, or missing `endpoint`/`keys.p256dh`/`keys.auth` |

### DELETE `/fmsg/push/subscribe`

Removes a Web Push subscription for the authenticated user. Idempotent —
removing an unknown endpoint still returns success.

**Request body:**

```json
{ "endpoint": "https://fcm.googleapis.com/fcm/send/abc..." }
```

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Malformed body, or missing `endpoint` |

## Web Push

When a `new_msg` event fires for a recipient (the same trigger that drives the
`/fmsg/ws` WebSocket), the server also sends an encrypted [Web Push](https://developer.mozilla.org/docs/Web/API/Push_API)
to every subscription that recipient has registered — so a user is notified
even with no live WebSocket. The client service worker decides whether to
display a notification; the server always sends.

The push carries a JSON payload:

```json
{
  "title": "@sender@example.com",
  "body": "message preview text",
  "threadId": 42,
  "url": "/app3.html?thread=42",
  "tag": "thread-42",
  "icon": "/icon-192.png"
}
```

`threadId`, `url` and `tag` reference the thread's **root** message id, so
replies group with their thread. If a push service responds `404` or `410` the
subscription is dead and is deleted automatically.

**Enabling.** Web Push is active only when `FMSG_VAPID_PUBLIC_KEY`,
`FMSG_VAPID_PRIVATE_KEY` and `FMSG_VAPID_SUBJECT` are all set; otherwise the
`/fmsg/push/subscribe` routes are not registered and no pushes are sent.
Generate the key pair once and keep it stable (rotating it invalidates every
existing browser subscription):

```
npx web-push generate-vapid-keys
```

**Database.** Subscriptions are stored in a `push_subscription` table:

```sql
CREATE TABLE push_subscription (
    addr       text        NOT NULL,
    endpoint   text        NOT NULL,
    p256dh     text        NOT NULL,
    auth       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (addr, endpoint)
);
```
