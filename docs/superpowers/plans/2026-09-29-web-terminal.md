# Web Terminal implementation plan

Goal: restore an independently bounded, audited Linux PTY module, enabled by default.

Architecture: authenticated POST issues a 30-second single-use ticket bound to the login session and exact configured Origin. WebSocket upgrade requires the same session and Origin; reserve quotas before starting a fixed /bin/sh PTY. No helper shell operation is introduced.

Constraints: explicit public_origin required for terminal (wildcard fails closed); read-only rejects both endpoints; 4 PTYs globally, 2 per admin, 128 pending tickets with one per login session; 4 KiB messages; 1 MiB input and 16 MiB output; 5-minute input idle and 30-minute absolute timeout; bounded writes; revoke on logout and shutdown. Audit metadata only. Shell inherits panel UID and existing service limits, and is not a sandbox.

- [x] Add failing route/config/security tests; run Linux Go tests in WSL.
- [x] Add terminal configuration and authenticated revocation identity.
- [x] Implement ticket store, exact Origin gate, admission quotas and lifecycle audit in pkg/terminal.
- [x] Implement PTY transport, size/byte/time limits and cleanup; test replay, concurrency, expiry, logout, shutdown and real PTY.
- [x] Wire server routes and shutdown; add a bounded plain-text terminal UI and deployment documentation.
- [x] Run go test -race ./..., go vet ./..., Linux amd64/arm64 builds and JS checks; review the final diff.
