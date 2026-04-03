# fmsg-webapi

HTTP API providing user/client message handling for an fmsg host. Exposes CRUD
operations for a messaging datastore backed by PostgreSQL. Authentication is
delegated to an external system — this service validates JWT tokens and enforces
fine-grained authorisation rules based on the user identity they contain.

## Environment Variables

| Variable            | Default                  | Description                                             |
| ------------------- | ------------------------ | ------------------------------------------------------- |
| `FMSG_DATA_DIR`     | *(required)*             | Path where message data files are stored, e.g. `/opt/fmsg/data` |
| `FMSG_API_JWT_SECRET` | *(required)*           | HMAC secret used to validate JWT tokens                 |
| `FMSG_API_PORT`     | `8000`                   | TCP port the HTTP server listens on                     |
| `FMSG_ID_URL`       | `http://127.0.0.1:8080`  | Base URL of the fmsgid identity service                 |

Standard PostgreSQL environment variables (`PGHOST`, `PGPORT`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE`) are used for database connectivity.

A `.env` file placed in the working directory is loaded automatically at startup
(values in the environment take precedence).

## Building

Requires **Go 1.25** or newer.

```bash
cd src
go build ./...
```

## Running

```bash
export FMSG_DATA_DIR=/opt/fmsg/data
export FMSG_API_JWT_SECRET=changeme
export PGHOST=localhost
export PGUSER=fmsg
export PGPASSWORD=secret
export PGDATABASE=fmsg

cd src
go run .
```

The server starts on port `8000` by default. Override with `FMSG_API_PORT`.

## API Routes

All routes are prefixed with `/api/v1` and require a valid `Authorization: Bearer <token>` header.

| Method   | Path                                        | Description              |
| -------- | ------------------------------------------- | ------------------------ |
| `GET`    | `/api/v1/messages`                          | List messages for user   |
| `POST`   | `/api/v1/messages`                          | Create a draft message   |
| `GET`    | `/api/v1/messages/:id`                      | Retrieve a message       |
| `PUT`    | `/api/v1/messages/:id`                      | Update a draft message   |
| `DELETE` | `/api/v1/messages/:id`                      | Delete a draft message   |
| `POST`   | `/api/v1/messages/:id/send`                 | Send a message           |
| `POST`   | `/api/v1/messages/:id/add-to`               | Add recipients           |
| `GET`    | `/api/v1/messages/:id/data`                 | Download message data    |
| `POST`   | `/api/v1/messages/:id/attachments`          | Upload an attachment     |
| `GET`    | `/api/v1/messages/:id/attachments/:filename`| Download an attachment   |
| `DELETE` | `/api/v1/messages/:id/attachments/:filename`| Delete an attachment     |

### GET `/api/v1/messages`

Returns messages where the authenticated user is a recipient (listed in `msg_to` or `msg_add_to`), ordered by message ID descending.

**Query parameters:**

| Parameter | Default | Description |
| --------- | ------- | ----------- |
| `limit`   | `20`    | Max messages to return (1–100) |
| `offset`  | `0`     | Number of messages to skip for pagination |

**Response:** JSON array of message objects. Each object has the same shape as the single-message response from `GET /api/v1/messages/:id` (with an additional `id` field). Message body data and attachment contents are not included — use the dedicated download endpoints instead.

### POST `/api/v1/messages`

Creates a draft message. The `from` address must match the authenticated user. The message is stored with `time_sent = NULL` (draft status) until explicitly sent.

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
| `400`  | Missing/invalid fields or empty `to` |
| `403`  | `from` does not match authenticated user |

### GET `/api/v1/messages/:id`

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
  "add_to": [],
  "time": null,
  "topic": "Hello",
  "type": "text/plain",
  "size": 12,
  "attachments": []
}
```

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Message not found |
| `403`  | Authenticated user is not a participant |

### PUT `/api/v1/messages/:id`

Updates a draft message. Only the owner (`from`) may update, and the message must not have been sent yet. Accepts the same body as `POST /api/v1/messages`. Recipients in `to` are fully replaced.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid fields |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### DELETE `/api/v1/messages/:id`

Deletes a draft message and all its attachments from the database and disk. Only the owner may delete, and the message must not have been sent.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### POST `/api/v1/messages/:id/send`

Marks a draft message as sent by setting `time_sent` to the current timestamp. Only the owner may send.

**Response:** `200 OK` with `{"id": <int>, "time": <float64>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `403`  | Not the owner |
| `404`  | Message not found |
| `409`  | Message already sent |

### POST `/api/v1/messages/:id/add-to`

Adds additional recipients to an existing message. The authenticated user must be an existing participant — the sender (`from`) or a primary recipient (listed in `to`).

**Request body (JSON):**

| Field    | Type       | Required | Description |
| -------- | ---------- | -------- | ----------- |
| `add_to` | `string[]` | yes      | Addresses to add (at least one) |

New addresses must be distinct among themselves (case-insensitive).

**Response:** `200 OK` with `{"id": <int>, "added": <int>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Empty `add_to` or duplicate addresses in request |
| `403`  | Authenticated user is not an existing participant (sender or `to` recipient) |
| `404`  | Message not found |

### GET `/api/v1/messages/:id/data`

Downloads the binary body of a message. The authenticated user must be a participant — the sender (`from`) or a recipient (listed in `to` or `add_to`).

**Response:** The raw message body file with `Content-Disposition: attachment` header. The `Content-Type` is inferred from the stored file extension.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Message not found or data file not available |
| `403`  | Authenticated user is not a participant |

### POST `/api/v1/messages/:id/attachments`

Uploads a file attachment for a draft message. Only the owner may upload, and the message must not have been sent.

**Request:** `multipart/form-data` with a `file` field.

**Response:** `201 Created` with `{"filename": "<string>", "size": <int>}`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Missing or invalid `file` field |
| `403`  | Not the owner, or message already sent |
| `404`  | Message not found |

### GET `/api/v1/messages/:id/attachments/:filename`

Downloads an attachment by filename. The authenticated user must be a participant — the sender (`from`) or a recipient (listed in `to` or `add_to`).

**Response:** The attachment file with `Content-Disposition: attachment` header.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid filename |
| `403`  | Authenticated user is not a participant |
| `404`  | Message or attachment not found |

### DELETE `/api/v1/messages/:id/attachments/:filename`

Deletes an attachment from a draft message. Only the owner may delete, and the message must not have been sent.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `400`  | Invalid filename |
| `403`  | Not the owner, or message already sent |
| `404`  | Message or attachment not found |
