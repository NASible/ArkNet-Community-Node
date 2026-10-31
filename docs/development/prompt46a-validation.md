# Prompt 46A validation — Core assertion issuance

Date: 2026-10-30. Branch `phase20/46a-core-assertion-issuance`.

## Acceptance criteria

| Criterion | Evidence |
| --- | --- |
| Binding (Node, member, purpose, nonce, challenge) | `core/internal/assertions` `TestIssuedAssertionBindsNodeMemberPurposeNonceChallengeAndExpiry`, `TestSignedButInconsistentPayloadsAreRejected`; `core/node_login_test.go` `TestNodeLoginIssuesAssertionBoundToNodeMemberNonceAndChallenge` |
| Expiry (exact boundaries, 60 s skew) | `TestExpiryIsExactAtTheSkewedBoundary` (including `issued_at == now + skew`); HTTP `TestNodeLoginAssertionExpires` |
| Single use | HTTP `TestNodeLoginNonceIsSingleUse`; PostgreSQL `TestLoginNonceIsClaimedOncePerNodeEvenConcurrently` (16 concurrent claims, exactly one succeeds) |
| Wrong Node | `TestVerifyRejectsAnotherNodesNonceOrChallenge`, HTTP `TestNodeLoginWrongNodeIsRefused`, blocked Node in `TestNodeLoginRefusesMalformedSubmissionsAndUntrustedNodes` |
| Wrong member | HTTP `TestNodeLoginWrongMemberGetsOwnPairwiseIDOrIsDenied`; PostgreSQL `TestLoginMembershipIsPairwiseAndRequiresActiveUserAndTrustedNode` |
| Key rotation | `TestKeyRotationPublishesBothAndSignsWithActiveKey`, HTTP `TestAssertionKeySetPublishesActiveAndRotatedKeys`; config tests for the published-keys file |
| No password or assertion in Core logs | `TestNodeLoginLogsNeitherPasswordsNorAssertions` captures the slog output of failed, successful and replayed logins and checks it for password, assertion, nonce, challenge, member ID, user ID, username and callback URL |

Every guard was mutation-checked: disabling it made the named test fail.
The PostgreSQL store tests ran against `postgres:18-alpine`.

## Live check

A compiled Core binary with a disposable PostgreSQL database passed 17 steps.
It enrolled and approved a Node, created a user, accepted an invitation and
registered a callback. It refused a plain-HTTP callback, an unregistered
callback, a wrong password and a replayed nonce. It issued an assertion that
verified against `GET /api/federation/v1/assertion-keys`. A grep of the Core
log for every secret involved found none.

## Independent review

A read-only security review found no blockers and no major issues. Fixes made
from its findings:

- strict ASCII callback hosts and ports, because the origin reaches the CSP;
- page `Referrer-Policy: same-origin`, so browsers without `Sec-Fetch-Site`
  can log in;
- the tracker public key is refused in the published-keys file;
- nonce expiry is computed on the database clock;
- concurrent callback replacement is serialized;
- assertion timestamps are bounded to 2020–2200;
- the extra negative tests the review asked for.

Accepted residual items are listed in
[global login assertions](../global-login-assertions.md#core-implementation-46a).

## Gate

`scripts/check-all.sh --e2e` passed 13 of 15 steps: mvp-e2e, node-upgrade and
product-swarm all passed. Two steps timed out while the host's load average
was about 22:

- node-ui: two vitest tests hit the 5 s limit;
- postgres-tests: two node-agent restart and outage tests failed.

Neither touches 46A code. Rerun in isolation, node-ui passed 18 files / 260
tests, and Core plus node-agent PostgreSQL tests passed three runs out of
three.
