# Delegated OAuth tokens: claims contract

This is the contract between an identity provider (or authorization server)
that issues delegated tokens, a resource server such as an MCP server that
obtains them, and this API. It applies when `FMSG_JWT_OAUTH_AUDIENCE` is set.

A delegated token lets a third-party client message as a user who consented to
it. It is not an owner session: it cannot create, rotate or delete API keys,
administer sub-account grants, or manage push subscriptions.

## Obtaining a token

A resource server must not forward the bearer token it received from its own
client. It obtains a separate token for this API from the identity provider,
for example by OAuth 2.0 Token Exchange (RFC 8693), and sends that token as
`Authorization: Bearer <token>`.

## Token format

A JWT signed with EdDSA (Ed25519). The `kid` header must name a key in the JWKS
at `FMSG_JWT_JWKS_URL`. The `typ` header is not checked; `at+jwt` (RFC 9068) is
recommended.

| Claim | Required | Rule |
| ----- | -------- | ---- |
| `iss` | yes | Equals `FMSG_JWT_ISSUER`. |
| `aud` | yes | Contains `FMSG_JWT_OAUTH_AUDIENCE` and does not contain `FMSG_JWT_AUDIENCE`. A token carrying both is rejected with 401. |
| address claim | yes | The claim named by `FMSG_JWT_ADDRESS_CLAIM`, holding the consented identity as `@user@domain`. |
| `exp` | yes | Keep it short (minutes). Revoking a grant at the issuer takes effect here when outstanding tokens expire. |
| `iat`, `nbf` | no | Validated when present. |
| `scope` | yes | Space-delimited string. An array of strings under `scope` or `scp` is also accepted. |
| `act` | recommended | Actor claim (RFC 8693) naming the resource server. A token that carries `act` with the owner audience is rejected, which catches an issuer that mislabels the audience. |
| `client_id`, `jti` | no | Not interpreted; useful in issuer audit trails. |
| `fmsg_identities` | no | Array of `@user@domain` addresses the token may select with `X-FMSG-Act-As`. |

Issuers should omit claims that other services treat as proof of an owner
session.

## Scopes

| Scope | Routes |
| ----- | ------ |
| `fmsg:read` | `GET /fmsg`, `/fmsg/sent`, `/fmsg/:id`, `/fmsg/:id/data`, `/fmsg/:id/thread`, `/fmsg/:id/thread/messages`, `/fmsg/:id/attach/:filename`, `/fmsg/ws` |
| `fmsg:write` | `POST /fmsg`, `PUT`/`DELETE /fmsg/:id`, `POST /fmsg/:id/send`, `/read`, `/add-to`, `/react`, `/attach`, `DELETE /fmsg/:id/attach/:filename` |

Every other route is closed to delegated tokens whatever scopes they carry, and
new routes stay closed until added to the table in
`internal/middleware/oauth.go`. Unknown scope values are ignored.

Message permissions, quotas and fmsgid acceptance checks
apply to delegated tokens exactly as they do to owner sessions. Scopes only
narrow access; they never add to it.

## Failure behaviour

| Condition | Response |
| --------- | -------- |
| `scope` missing, empty, or not a string / array of strings | 403 |
| Route needs a scope the token lacks, or is closed to delegated tokens | 403 with `WWW-Authenticate: Bearer error="insufficient_scope"` |
| `aud` contains neither configured audience, or both | 401 |
| `act` present on an owner-audience token | 401 |
| `fmsg_identities` malformed | 403 |

A delegated token is never downgraded into, or mistaken for, an owner session:
the audience alone decides which kind of token it is, and a delegated token
that fails these checks is refused.

## Acting as another identity

`X-FMSG-Act-As` (and `act_as` on the WebSocket) is refused for delegated tokens
unless the requested address is listed in `fmsg_identities`. A listed address
must also pass the normal owner/sub-account grant check, so the claim can only
narrow what the owner could already do. Administration stays closed after
switching identity.

## WebSocket

A connection opened with a delegated token is closed when the token expires.
Reconnect with a fresh token.
