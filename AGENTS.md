# Agent Instructions

## General

- This is a Go HTTP API backed by PostgreSQL. Source lives under `src/`.
- Build: `cd src && go build ./...`
- Test: `cd src && go test ./...`
- Always run tests after making code changes to verify nothing is broken.

## README

**Keep `README.md` up to date whenever you:**

- Add, remove, or rename a route.
- Change a route's query parameters, request body, or response shape.
- Add or remove environment variables.
- Change build or run instructions.

The API routes table and each route's section must reflect the live code in
`src/main.go` and `src/handlers/`.

## Database

- Schema source of truth: `https://github.com/markmnl/fmsgd/blob/main/dd.sql`
- Ensure all SQL in Go source files aligns with that schema.
- When adding recipients via the `add-to` route, update `msg.add_to_from`
  in the same transaction as the `msg_add_to` inserts.
- The WebSocket hub (`/fmsg/ws`) LISTENs on the `new_msg` LISTEN/NOTIFY channel
  for push delivery. `new_msg` fires once per recipient (payload `<msg id>,<addr>`)
  whenever a message becomes sent/arrived. Do not rename or remove that trigger
  without updating the hub. The `new_msg_to` channel is the sender daemon's
  outbound delivery queue and is not used by this service.
- For each `new_msg` notification the hub also dispatches a Web Push (when
  VAPID is configured) to the recipient's `push_subscription` rows, so delivery
  does not depend on a live WebSocket.
