# UI usability validation — 2026-09-25

## Implemented

- Connected the existing HTML workspace to modular JavaScript; fixed the missing firewall selector that previously stopped initialization.
- Added real metric cards, 60-sample CPU/memory chart, sampling state, host information and pause/resume.
- Added hash navigation, page descriptions, last-update time, refresh busy states and dismissible errors.
- Added process search/reset and descriptive confirmation dialogs; service search/status filtering, 25-row pagination, details and service-to-log navigation.
- Added file breadcrumbs, transactional directory navigation (failed navigation keeps the last valid directory), upload filename/size feedback and permission dialogs.
- Added log keyword filtering, line counts and wrapping; firewall engine-specific options and disabled writes until successful engine detection.
- Improved typography/contrast, touch targets, mobile six-module navigation and contained table scrolling.
- Kept plain JS/CSS, Go embedding, existing APIs and server-side authorization.

## Verified

- Reproduced the original JavaScript initialization exception in the browser before implementation.
- Real Linux server in disposable loopback configuration: login, metrics, pause, process search, confirmation/cancel, 122-service list, state/name filter, service detail, service-to-log link, log filter/wrap, file listing, breadcrumbs, empty directory, permission dialog, nonexistent path error, refresh recovery.
- Desktop 1440 × 1000 and mobile 390 × 844; mobile overview and service table have no page-wide horizontal overflow.
- Read-only instance: upload, chmod and service restart disabled; detail reads remain enabled.
- Independent code review identified and fixed dropped log requests during rapid navigation and the skip link changing modules. Refreshes now track request identities; obsolete responses cannot complete a newer refresh. Log loading clears old service output.
- Four Go package test binaries built for Linux using Windows Go 1.26.2 and executed successfully under WSL: root, config, auth, sysinfo.
- Linux-targeted `go vet ./...` succeeded. Linux amd64 and arm64 builds succeeded.
- Final amd64 smoke test passed again after review fixes: file upload/download round trip, audit verification and graceful shutdown. Startup probe 35.55 ms; measured RSS 11,492 KiB; binary 9,244,834 bytes.

## Limits

- Native WSL race-test invocation could not complete because its dependency downloads timed out; the cross-compiled Linux tests passed without race instrumentation.
- WSL has no usable firewall engine. Checked the visible unsupported/error state and disabled submission; no real firewall or service mutations performed.
- Temporary preview uses explicit test credentials and disposable files. It is a development tool, not deployment configuration. Run `python3 scripts/preview.py /path/to/linux-binary --readonly` in Linux for a read-only preview at port 8893.
