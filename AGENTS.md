# Agent Instructions

## General

- This is a Go HTTP API backed by PostgreSQL. The executable entrypoint lives under `cmd/fmsg-webapi/`, with application packages under `internal/`.
- Build: `go build ./...`
- Test: `go test ./...`
- Always run tests after making code changes to verify nothing is broken.
- This repository is an open-source implementation and not tied to one specific deployment - config should come from the environment
- Check sensitive files are not accidentally committed to git, e.g .env should NEVER be

## README

**Keep `README.md` up to date and concise whenever you:**

- Add, remove, or rename a route.
- Change a route's query parameters, request body, or response shape.
- Add or remove environment variables.
- Change build or run instructions.

The API routes table and each route's section must reflect the live code in
`cmd/fmsg-webapi/main.go` and `internal/handlers/`.

## Database

- Schema source of truth: `https://github.com/markmnl/fmsgd/blob/main/dd.sql`
- Ensure all SQL in Go source files aligns with that schema.
- When adding recipients via the `add-to` route, insert one `msg_add_to_batch`
  row (`add_to_from` = authenticated identity, plus `time_added`) and insert the
  `msg_add_to` rows with that batch's `batch_id`, all in the same transaction.
- The WebSocket hub (`/fmsg/ws`) LISTENs on the `new_msg` LISTEN/NOTIFY channel
  for push delivery. `new_msg` fires once per recipient (payload `<msg id>,<addr>`)
  whenever a message becomes sent/arrived. Do not rename or remove that trigger
  without updating the hub. The `new_msg_to` channel is the sender daemon's
  outbound delivery queue and is not used by this service.
- For each `new_msg` notification the hub also dispatches a Web Push (when
  VAPID is configured) to the recipient's `push_subscription` rows, so delivery
  does not depend on a live WebSocket.
