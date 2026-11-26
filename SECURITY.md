# Security Policy

## Supported versions

Security fixes are provided for the latest minor release. During the `0.x`
series, users should upgrade to the newest available patch or minor release.

| Version | Supported |
|---|---|
| `0.12.x` | Yes |
| `0.11.x` and older | No |

## Reporting a vulnerability

Please report security vulnerabilities privately through GitHub Security
Advisories:

https://github.com/Beiyu114514/linkc-bc-soga-v1/security/advisories/new

Do not open a public issue for an undisclosed vulnerability. Include:

- the affected version or commit;
- the deployment mode and operating system;
- a minimal reproduction or proof of concept;
- the expected and observed impact;
- any known mitigations;
- whether you intend to disclose the issue publicly.

The maintainer will acknowledge the report, validate the impact, and coordinate
a fix and disclosure timeline. These are good-faith response targets, not a
service-level agreement.

## Scope

Reports involving authentication bypass, command injection, credential
disclosure, privilege escalation, sandbox escape, unsafe file handling, remote
code execution, or a vulnerability in how `linkc-node` consumes panel data are
in scope.

Configuration mistakes, unsupported deployments, and vulnerabilities that exist
only in an unmodified upstream dependency should be reported upstream when
possible. If `linkc-node` exposes or amplifies the issue, report it here as well.

## Security hygiene

- Never include production API keys, UUIDs, private keys, or customer data in a
  report.
- Use a private advisory unless the maintainer agrees to public disclosure.
- Always verify release checksums and container digests before deployment.
