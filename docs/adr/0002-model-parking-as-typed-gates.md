---
status: accepted (not yet implemented)
---

# Model parking as typed gates

`parked` is an orthogonal durable wait condition on an active run, not a terminal run status and not an action. A typed gate records why execution stopped, the evidence available at that point, and the actions legal for that gate. Review judgment gates may retain Approve/Fix/Skip/Abort, exhausted verifiable gates cannot use Approve or Skip to turn failed evidence into publication, and environment parks remain abort-and-restart controller failures.

An explicit trusted opt-in may add **Publish diagnostic PR** to durable Review, Build, Test, Lint, and final-verification gates. The action safely publishes the frozen committed head through the normal drafting and PR ownership path, requires a no-mistakes-owned Risk Assessment marked `Uncertified`, and leaves the original run parked. Diagnostic PRs default to Draft, but Draft is configurable and is not the source of certification truth.

## Considered options

- **Top-level `parked` run status:** rejected because the run still owns its worktree, branch custody, active gate, and resumable execution state.
- **Represent parking only through step status:** rejected because environment and future controller gates can park before a step exists.
- **Automatic diagnostic PR on every park:** rejected because publication is an external side effect and many parks are transient or environmental. Publication requires both trusted enablement and an explicit action.
- **Allow failed Build/Test/Lint gates to Approve or Skip into publication:** rejected because that would convert missing or failed verification into a successful pipeline claim.
- **Use Draft as the diagnostic signal:** rejected because Draft is provider-dependent and policy-overridable. Verified `Uncertified` Risk Assessment content is the invariant.
- **Permit diagnostic publication from environment parks:** rejected because deterministic worktree preparation failed before trustworthy pipeline evidence existed.
- **Treat diagnostic publication as pipeline success:** rejected because it is a review artifact for a parked run, not certification.

## Consequences

Gate identity, allowed actions, and evidence must be durable and recoverable. Repeated diagnostic publication updates the same safely discovered PR; normal Push/PR later replaces diagnostic truth only after the run satisfies its ordinary publication contract. Aborting a diagnostic run does not delete or close the PR and must preserve push/custody truth.
