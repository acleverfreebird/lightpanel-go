# Linux panel usability and reliability

Goal: make the existing single-host Linux panel dependable for everyday administration, retaining Go/net/http and embedded vanilla JS. No remote push or production host changes.

Use inline execution, regression tests before fixes, and one local commit per completed area. Existing installer/service changes are retained and verified as part of deployment work.

- [ ] Deployment/helper: preserve socket access fixes; bound command output and staged binary size; fail safely on socket ownership errors; order panel after helper; verify installer shell syntax and Linux helper tests.
- [ ] Files: no-clobber rename, preserve mode/ownership on edit, reject special/non-UTF-8 editing targets, nonblocking chmod, intuitive relative paths; verify temporary-directory lifecycle tests and JS path tests.
- [ ] Services: list installed as well as loaded units, expose startup configuration, add enable/disable/reload through the same validated ACL on both sides; verify command construction and denial tests.
- [ ] Diagnostics: authenticated health/capability view with runtime user, tools and helper reachability; expose useful errors and privilege constraints in overview; verify authentication and response tests.
- [ ] Delivery: Linux CI with tests/race/vet, architecture builds, JS tests and smoke checks; reconcile outdated documentation with current filesystem implementation, run full validation and record limits.

Acceptance: Linux tests and vet pass, amd64/arm64 build, real local HTTP smoke passes, changes are committed individually. Testing must not restart host services or change firewall rules. Distribution-specific integration and public TLS deployment remain explicitly identified if not exercised.
