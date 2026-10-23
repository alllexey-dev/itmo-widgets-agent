# Changelog

## 0.2.0

Needs Backend with connected clients (`docs/contracts/connections.md`, itmo-widgets-backend PR #58) and an app
with «Устройства». Not compatible with 0.1.x pairings: unpair the old agent and run `pair` again.

- Own ITMO.ID session: the phone signs the agent in to ITMO.ID in a separate window and hands the session over
  sealed during pairing (`CONNECT`). The agent no longer shares the phone's session, and it shows as its own row on
  id.itmo.ru.
- `renew`: a new ITMO session for the same connection (`RENEW`), at least every 90 days or after the session was
  closed; replaces `pair --session`. The replaced session is logged out.
- `unpair` and a revoked connection (Backend 401) end the agent's own ITMO session by OIDC logout, then wipe the
  files; a revoked agent exits with code 3 as before. A 0.1.x `session.json` (no `sid`, the phone's own session)
  is only deleted, never logged out.
- Backend calls go to `/api/client/**` with the `iwc_` connection token: events with `itmo=ok|closed|none`,
  `PUT /api/client/status` with the version at start, `GET /api/client/me`, `DELETE /api/client/self`. The claim
  is proven by the `claimSecret` from the challenge.
- `pair` asks for the scopes of `IW_FEATURES` (`sport`, `friends:read`) and sends the platform, so the phone can
  label the ITMO.ID sign-in.
- `status` shows when the ITMO session ends, and what Backend says about the connection (name, granted access).
- Removed: the alpha pairing (`AGENT`, `AGENT_SESSION`, poll secret, `iwa_` tokens, `/api/agent/**`),
  `pair --session` and `login` (pasting a refresh token).

## 0.1.1

- One-command installers for Linux, macOS, FreeBSD and Windows; `attach` to a running agent.

## 0.1.0

- First release: pairing by QR, sport bookings from Backend events, ntfy notifications.
