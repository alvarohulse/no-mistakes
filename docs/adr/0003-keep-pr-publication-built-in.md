---
status: accepted (not yet implemented)
---

# Keep PR publication built in

Provider adapters retain ownership of PR discovery, fork routing, immutable-head push binding, exact PR targeting, create/update, owned-body verification, and recovery. Customization is layered around that boundary: a trusted typed policy command supplies supported draft/ready, label, assignee, and reviewer preferences, while durable lifecycle events drive warning-only post-publication commands.

Normal PRs default Ready for Review and diagnostic PRs default Draft; policy may override either. Policy is reevaluated for publication and CI readiness. Lifecycle delivery uses a SQLite outbox with stable event IDs derived from Run ID, ordered delivery with up to three automatic attempts per replay generation, downstream deduplication, and durable bounded/redacted receipts. Exhausted events remain visible for manual replay with the same event ID; external side effects are never claimed to be exactly once.

## Considered options

- **Arbitrary command replaces PR discovery/create/update:** rejected because it would duplicate provider identity, fork routing, body ownership, push safety, recovery, and idempotency rules while making host behavior harder to verify.
- **One generic post command with no typed attributes:** rejected because draft state and provider-owned collections need capability-aware read/apply semantics rather than shell conventions.
- **Exactly-once lifecycle commands:** rejected because a comment or hosted mutation can succeed before a daemon crash prevents the success receipt. Stable event IDs and receiver deduplication are the honest boundary.
- **Blocking the pipeline on every customization failure:** rejected because labels, assignees, reviewers, and third-party comments are ancillary to code and body publication. Failures remain durable warnings.
- **Authoritative replacement of hosted labels/reviewers:** rejected as the default because it can erase human, CODEOWNERS, host-default, or third-party state. Reconciliation should remove only values no-mistakes previously added.
- **Expose raw metadata or command details in lifecycle events:** rejected because metadata is untrusted local input and commands may contain private policy, paths, or credentials.

## Consequences

Provider capabilities remain asymmetric and must report applied, unchanged, unsupported, failed, or unverified per field. Event receipts live with rich local run state unless a later retention decision says otherwise. Provider-specific questions such as Azure Draft readiness and GitLab label creation remain explicit compatibility decisions rather than being hidden behind a falsely uniform interface.
