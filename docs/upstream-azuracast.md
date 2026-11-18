# Azuracast as an upstream: spike findings

Measured 2026-11-18 against a test station on an Azuracast instance (Icecast
frontend, Liquidsoap backend), using `cmd/icesource`. The DJ port is a
Liquidsoap harbor, so these findings should hold for any harbor input.

## Connecting

- Both `PUT / HTTP/1.1` and `SOURCE / HTTP/1.0` with `Authorization: Basic`
  are accepted. The reply is a bare `200 OK` about 0.6s after the request;
  no `100 Continue` is sent even when asked for.
- Bad credentials get `401 Unauthorized` and the connection is closed,
  within about 0.7s.
- A second source while one is live gets `403 Mountpoint already taken`.
  A new source can connect immediately after the previous one disconnects.
- The hostname is behind a CDN that does not pass the DJ port; the source
  connection has to go to the origin IP.

## Metadata

- `GET /admin/metadata?mode=updinfo&mount=/&song=...` with the streamer's
  credentials returns `200` and the title appears in Azuracast's now-playing.
  Forwarding DJ titles works as planned.
- The streamer shown as live is the account that opened the source
  connection.

## Checking credentials without going live

The same metadata request doubles as a credential check:

| State | Good credentials | Bad credentials |
| --- | --- | --- |
| Nobody live | `500` | `401` |
| Somebody live | `200` (title is overwritten) | `401` |

So "401 means bad, anything else means good". While nobody is live the check
has no side effects. While somebody is live it rewrites the title, so the
only safe check is to re-send the title that is already current.

A request for a mount other than `/` returns `400` whatever the credentials,
so it cannot be used as a side-effect-free probe.

## Timing

- Azuracast's now-playing API reported the streamer live about 27s after
  the source connected, and not live about 14s after it disconnected. The
  API lags the audio; do not use it to time handoffs.
- A listener on the public mount saw no stall longer than a second across
  a disconnect followed by an immediate reconnect.

## Not measured

The test station has no AutoDJ content, so what a listener *hears* between
two DJs (how much AutoDJ audio leaks in, and for how long) is still unknown.
That needs a station with a playlist, and a listener that decodes the audio.
