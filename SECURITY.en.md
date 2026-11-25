# Security vulnerability disclosure

This file describes the **vulnerability disclosure process for this plugin repository (`kore-plugin-db-postgres`)**; it is not a security design document.

## Scope

Covered: the implementation of `PostgreSQL 管理插件` (id `db-postgres`), the capability declarations in `info.json`, the `web/` front end, and `make dist` artifacts.

Not covered: kernel and protocol issues — report those per [kore SECURITY.md](https://github.com/kore-pl/kore/blob/main/SECURITY.md); known vulnerabilities in upstream third-party dependencies should also be reported upstream.

## Supported versions

| Version | Support status | Notes |
| --- | --- | --- |
| `0.4.0` (latest) | ✅ Accepted and fixed | Security fixes are provided only for the latest patch version |
| Earlier `0.x` versions | ❌ Not supported | Please upgrade to the latest version first |

During the 0.x phase we use a minimal support surface: only the latest patch of a given minor receives fixes; older patch versions are not backported individually.

## Attack surface of this repository

- **RPC methods**: all `db-postgres.*` methods (see the method table in `README.md`); missing parameter validation can lead to privilege escalation or injection.
- **capabilities**: `info.json.capabilities` is the plugin's entire authorization surface; every extra declaration is another attack path.
- **Isolated process and UDS**: the plugin runs as an isolated process; the socket is created by the host shell, `chown kore-<id>`, mode `0600`; bypassing socket permissions or connecting to another party's socket counts as a vulnerability.
- **Host write surface**: writes actually granted via `capabilities` (files / processes / network / databases, etc.); writing beyond the declared scope is a vulnerability.
- **Front end**: `web/` runs in an isolated-origin iframe and communicates with the host shell via postMessage; XSS, message forgery, or cross-plugin data reads are all security-boundary issues.

## Reporting

1. **Prefer GitHub Security Advisory**: this repository's Security page → "Report a vulnerability" (private channel, no public issue).
2. **Backup email**: `pengtang1024@users.noreply.github.com`, subject `[kore security] db-postgres`.
3. **Do not open public issues, and do not post PoCs in discussions or PRs.**
4. Reports should include: affected version, reproduction steps or PoC, impact assessment, whether it has been publicly disclosed, and attribution preference.

## Timeline and disclosure

- Confirm receipt and give a preliminary assessment **within 72 hours**; complex issues get a synchronized investigation plan.
- Coordinate disclosure after a fix, with a default **90-day** window; if the issue is already being exploited in the wild and earlier disclosure is needed, please discuss first.
- After the fix is released, record the security fix in `CHANGELOG.md` (without exploitation details) and publish a security advisory as needed.

## Acknowledgments

With your consent we will credit you in the release notes; those who prefer to remain anonymous will be treated as such.
