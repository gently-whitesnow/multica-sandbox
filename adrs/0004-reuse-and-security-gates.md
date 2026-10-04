# ADR 0004: Reuse infrastructure and prove execution boundaries

Status: Accepted
Date: 2026-10-04

## Context

The project should integrate Multica with enforceable execution boundaries.
Reimplementing orchestration, identity or secret storage increases maintenance
and security risk. Successful task execution alone proves no isolation property.

## Decision

Own the Multica integration, environment contract and backend/adapter conformance.
Keep Multica as task authority. Keep session recovery separate from disposable
execution state; do not build another agent reasoning loop or task scheduler.

Evaluate maintained components before adding infrastructure:

| Concern | Candidate or existing integration |
| --- | --- |
| Kubernetes lifecycle and warm pools | Kubernetes SIGs Agent Sandbox |
| Kernel isolation | Deployment-selected gVisor/Kata; Docker/Sysbox first |
| Workload identity | SPIFFE/SPIRE or deployment OIDC |
| Tool authorization transport | Official MCP SDKs and existing trusted services |
| Downstream credential custody | Deployment secret manager, e.g. OpenBao |

These are evaluation candidates, not selected dependencies or security claims.
Agent Sandbox does not replace our Multica adapter or access policy. Verify its
network, router authorization and service-account defaults before adoption.
Keep credential resolution behind MCP services; the runner needs no vault client.
Do not implement cryptography, OAuth issuance or a generic credential proxy here.

Before selecting a dependency, record its upstream version, license, maintenance,
security defaults, operational cost and a small compatibility experiment. Before
building a replacement, record the concrete unmet requirement in an ADR. Pin
validated versions/digests and test upgrades; tracking upstream means continuous
compatibility work, not unreviewed production updates.

Require executable negative tests before advertising a backend or adapter:

- A hostile task cannot read service secrets, host sockets, metadata, another
  task's files/identity or bypass network policy, including through DNS/IPv6.
- Wrong-audience, expired, revoked and cross-task identities fail. Forged context
  and out-of-scope tool arguments fail even with otherwise valid authentication.
- Credentialed MCP requests cannot redirect to attacker destinations; errors,
  results and audit records do not reveal credentials.
- Cancellation/restart stops access, reaps execution and prevents duplicate active
  attempts. Bound retries and resource use; audit survives sandbox destruction.
- Preparation caches contain no credentials or identity. Keys include source,
  lockfiles, image/tool digests, runner, platform, recipe and relevant policy.
  Partition private caches by tenant/access boundary; reauthorize every restore.
  Changed inputs or policy invalidate reuse. Keep session snapshots separate.
- Warm pools supply only unused environments; never return a used sandbox to the
  clean pool. Every run gets new writable state and fresh authorization.

Start with a fake hostile runner and a credentialed test MCP service, then one
real agent adapter. Verify the same boundary for preparation and execution.
Wire these checks into verify.sh and enable Harness runtime checks when code lands.

## Consequences

A narrow first working slice precedes broad adapter support. Some infrastructure
remains deployment-owned. Conformance tests are necessary evidence, not a proof
against every kernel exploit or misuse of an authorized operation. There is no
runtime implementation or security test suite yet.

## References

- [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- [Agent Sandbox threat model](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/security/threat_model.md)
- [OpenBao credential leases](https://openbao.org/docs/2.5.x/concepts/lease/)
- [VK Tech: sandbox, RBAC and egress](https://habr.com/ru/companies/vktech/articles/1068846/)
