# ADR 0018: Agent skills

Status: Accepted
Date: 2026-10-11

## Context

Multica binds skills to agents and adds built-in platform skills, such as
`multica-platform` with the issue, mention and project contracts. The upstream
daemon (v0.5.2) handles them as follows:
- Without `skill-bundles-v1`, a claim carries `agent.skills` inline: `id`, `name`,
  `description`, `content` and supporting `files` (`path`, `content`). Without
  `platform-skill-v1`, the server also sends a redirect stub under the
  pre-merge built-in name.
- `writeSkillFiles` lays each skill down in the provider's native root
  (`.opencode/skills/<slug>/` for OpenCode). The slug is the sanitized name,
  deduplicated within the batch (`-multica`, `-multica-N`). `SKILL.md` gets
  frontmatter whose `name` is the slug; every other key stays as written.
  A supporting `SKILL.md` duplicate is skipped.
- The brief lists the model-visible skills (`disable-model-invocation` hides
  one) and points the sub-issue section at `multica-platform`, or at the
  pre-merge `multica-working-on-issues` when only that is installed. Web chat
  renders the skills a message selects with `slash://skill/<id>` links.

Before this decision the adapter dropped skills. The brief and chat prompt
omitted them, and the agent could not reach the platform contracts.

## Decision

Mirror upstream for tasks on the Multica relay path.

**Claims.** The client advertises `platform-skill-v1` and parses inline skills.
Claim responses may reach 16 MiB, since they carry skill content; other
responses keep 2 MiB. A claim is
rejected when it holds more than 64 skills, a skill has more than 256 files,
a file exceeds 1 MiB, or all files exceed 8 MiB. Paths that are absolute, contain
`..`, backslashes or NUL, appear twice or are also a directory reject it too.
Upstream trusts the server here; the sandbox bounds what it writes.

**Layout.** Slugs, frontmatter rewriting and visibility are ported unchanged;
a differential fuzz against upstream found no divergence. Skills go to
`$XDG_CONFIG_HOME/opencode/skills/<slug>/` (`/workspace/config/opencode/skills`),
OpenCode's global root, not into the repository workdir. The root starts empty
in every attempt, so upstream's collision probe always keeps the batch slug,
and retained workdirs carry no skill files. The controller builds a tar of
regular files with clean relative paths and extracts it with the image's
`tar` as the attempt user before the agent starts. The config directory is
sealed afterwards, as before.

**Agent.** The `skill` tool is allowed with the other local tools. The brief
gains upstream's Sub-issue Creation and Skills sections, and web chat its
explicit skill selection.

Rejected alternatives:
- `skill-bundles-v1` with a hash-keyed cache. It needs a bundle resolver and a
  cache outside attempts, which is separate work.
- Writing into `.opencode/skills` in the workdir. Retained workdirs would keep
  stale skills, and agent commits could include them.
- One projection write per file. It costs one Docker exec per file.

## Consequences

OpenCode 1.18.34 and 1.18.35 discover skills in the global root. Agent images
need `tar` on the relay path; the startup image check enforces it. Project
resources (`.multica/project/resources.json`) are not projected. The upstream
listing gap for user-installed skill collisions cannot occur in an empty root.
