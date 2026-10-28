# Local Application Adapters

The Node Agent interacts with local applications through the small
`internal/application.Adapter` interface. Jellyfin is the first implementation,
under `internal/application/jellyfin`; application-neutral Agent code depends on
the interface rather than the Jellyfin HTTP client.

The contract covers:

- stable application ID;
- enabled state;
- configuration status;
- health;
- version discovery;
- start, stop, and restart lifecycle requests.

This is a compile-time Go interface, not a dynamic plugin system. Adding Seerr or
Calibre-Web later means adding another package that implements the same contract
and wiring it explicitly in the Agent composition root. It does not mean loading
untrusted binaries, discovering marketplace plugins, or exposing a generic
remote-control endpoint.

## Jellyfin adapter

The Jellyfin adapter uses only its configured local base URL:

- `GET /health` supplies health;
- `GET /System/Info/Public` supplies the version;
- a non-empty `JELLYFIN_API_URL` means configured and enabled.

Health remains projected into the existing `jellyfin_healthy` status field so the
current dashboard and API contract remain compatible.

Lifecycle operations are separated behind the generic
`application.Lifecycle` interface. The current Agent is not given the Docker
socket and does not contain an orchestrator implementation. Consequently,
Jellyfin `Start`, `Stop`, and `Restart` return
`application.ErrLifecycleUnavailable` unless a trusted lifecycle controller is
explicitly injected. This keeps the operation contract ready for a later,
bounded controller without granting the Agent broad Docker control in this
phase. Docker Compose continues to provide actual process lifecycle and
`restart: unless-stopped` behavior.

No adapter receives PostgreSQL credentials or access to the PostgreSQL network.
Application adapters must not return secrets through status or logs.
