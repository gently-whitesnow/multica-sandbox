# ADR 0033: Release versions

Status: Proposed.
Date: 2026-11-04

## Context

A deployment shows which controller runs only as a commit and an image digest
(`sandbox_build_info{revision}`, the deploy summary). Operators cannot tell from
those which release runs or whether it carries a given fix.

## Decision

- `VERSION` holds the controller release as `MAJOR.MINOR.PATCH`. A change to the
  controller, helper, CLI image or their configuration bumps it in the same pull
  request: PATCH for fixes, MINOR for features or new configuration, MAJOR for a
  breaking configuration change. A test keeps the format.
- The image build reads `VERSION` into the controller. `sandbox_build_info` carries
  `version` next to `revision`, and the `controller ready` log names both.
- `images.yml` tags every image `v<VERSION>` as well as the commit, labels it
  `org.opencontainers.image.version` and records the version in the release
  artifact. It refuses a version whose Git tag `v<VERSION>` marks another commit,
  then creates that tag.
- `deploy.yml` names the version in its summary.

## Consequences

Dashboards, logs and deploy summaries name releases. Digests stay the pins, so a
moved tag cannot change a deployment. A forgotten bump fails publication instead of
shipping two builds under one version.
