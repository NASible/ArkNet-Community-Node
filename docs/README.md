# Documentation

Architecture, development, and operations documentation belongs here.

- [Development prompt pack](development/hermes-mvp-prompts.md): ordered
  implementation tasks, including the repository foundation.
- [Phase 20 prompt pack](development/hermes-phase20-prompts.md): ordered tasks
  that close the open ARKSEC release blockers, with the user decisions they need.
- [Repository overview](../README.md): component map and architecture boundaries.
- [MVP validation report](mvp-validation-report.md): all 20 Prompt 43 results
  from the 2026-10-27 final-tree run (20 PASS), Phase 19A blockers,
  ARKSEC-linked limitations, and separate functional and security verdicts.
- [MVP security review](mvp-security-review.md): Prompt 42 requirement matrix,
  fixes, residual risk, and verification gate.
- [Security follow-ups](security-follow-ups.md): tracked release-blocking and
  post-review security work with acceptance criteria.
- [Multi-node swarm proof](multi-node-swarm-proof.md): Prompt 27 measured results.
- [Global users](global-users.md): Core-owned accounts, password storage, and sessions.
- [Core abuse controls](core-abuse-controls.md): shared login limits, signed enrollment, pending quota and fixed expiry.
- [Node invitations](node-invitations.md): privacy-preserving membership invitations.
- [Pairwise membership IDs](pairwise-membership-ids.md): per-Node user pseudonyms.
- [Node-local users](local-users.md): Node-scoped accounts, offline authentication, and privacy boundaries.
- [Node operator authentication](node-operator-authentication.md): design for operator accounts, bootstrap, sessions, the route permission matrix and shell recovery (ARKSEC-002; not yet implemented).
- [Global member login on a Node](cached-global-authorization.md): Core-bound login, offline login during Core outages, and reconnect refresh.
- [Node revocation](node-revocation.md): what a revoked Node stops, how fast, and the residual window for a Node that ignores revocation.
- [Private relay design](private-relay-design.md): Prompt 48 spike comparing relay options, with measurements and open questions (ARKSEC-004, D5).
- [Private relay service](../relay/README.md): the relay's protocol, revocation message and configuration (Prompt 48A).
- [Global login assertions](global-login-assertions.md): design for Core-bound login without password transit, and the offline model (ARKSEC-001; implemented in 46A and 46B).
- [Primary home node](home-node.md): Core-only home-node relationship, explicit change, and routing access.
- [Media routing](media-routing.md): home-node-or-Core decision from Core-observed health and verified replicas.
- [Media-miss events](media-miss-events.md): non-blocking Core miss recording, private aggregates, and MANUAL/ASK/AUTOMATIC Node policy.
- [Node diagnostics](node-diagnostics.md): the Diagnostics page — classified Core/tracker failures, local service health, storage, transfers, plain-language actions.
- [Node installation](node-installation.md): `install.sh` bootstrap for a fresh Ubuntu LTS machine, through enrollment.
- [Node upgrade](node-upgrade.md): `arknet-node upgrade`/`rollback` — verified download, image pull, backup, Compose apply, validation, automatic rollback.
- [Update discovery](update-discovery.md): read-only channel-index discovery, verified newer releases, automatic-updates setting (default ON).
- [Release manifest](release-manifest.md): versioned whole-stack Node release, stable/development channels, pinned components.
- [Removed media](removed-media.md): Core tombstones, retained Node copies, peer eligibility, manual deletion, and Jellyfin behavior.

The prompt pack describes future tasks; it does not imply those features are
implemented.
