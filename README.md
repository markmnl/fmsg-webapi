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
| `GET`    | `/api/v1/messages/:id/data`                 | Download message data    |
| `POST`   | `/api/v1/messages/:id/attachments`          | Upload an attachment     |
| `GET`    | `/api/v1/messages/:id/attachments/:filename`| Download an attachment   |
| `DELETE` | `/api/v1/messages/:id/attachments/:filename`| Delete an attachment     |

### GET `/api/v1/messages`

Returns messages where the authenticated user is a recipient (i.e. listed in `msg_to`), ordered by message ID descending.

**Query parameters:**

| Parameter | Default | Description |
| --------- | ------- | ----------- |
| `limit`   | `20`    | Max messages to return (1–100) |
| `offset`  | `0`     | Number of messages to skip for pagination |

**Response:** JSON array of message objects. Each object has the same shape as the single-message response from `GET /api/v1/messages/:id` (with an additional `id` field), except that the `data` field is always empty.

### GET `/api/v1/messages/:id/data`

Downloads the binary body of a message. The authenticated user must be the sender (`from_addr`) or a recipient (listed in `msg_to`).

**Response:** The raw message body file with `Content-Disposition: attachment` header. The `Content-Type` is inferred from the stored file extension.

**Errors:**

| Status | Condition |
| ------ | --------- |
| `404`  | Message not found or data file not available |
| `403`  | Authenticated user is neither sender nor recipient |
