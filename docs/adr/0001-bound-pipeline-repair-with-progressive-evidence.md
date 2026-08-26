---
status: accepted
---

# Bound pipeline repair with progressive evidence

Pipeline repair is bounded at two levels: at most three `Review → Build → Test → Document → Lint` passes per run, and finite per-pass repair limits for Build, Test, and Lint. Repairs revalidate their owning step and continue forward; a source mutation may trigger another full pass while the run budget remains. When the final allowed mutation leaves Build, Test, or Lint evidence behind the source, one no-fix `Build → Test → Lint` verification runs before publication.

Evidence is progressive rather than one universal final-head attestation. Every gate keeps its actual pass/fail disposition and tested commit. Later changes do not rewrite an earlier Review pass as failed, but PR and local status surfaces must expose the tested-head provenance instead of implying that Review inspected the final head. Three full passes are the signal to split or rethink the change rather than continue increasingly niche automated review.

## Considered options

- **Unbounded Review/repair cycling:** rejected because per-step counters can reset each other, model feedback becomes increasingly pedantic, and a broad change that cannot converge should be redesigned or split.
- **Only per-step repair limits:** rejected because independently bounded steps can still create an unbounded whole-pipeline loop.
- **Mandatory same-final-head Review after every downstream fix:** rejected because it spends Review repeatedly and exhausts the useful review frontier quickly. Static Build/Test/Lint verification is accepted for some later pipeline-authored changes.
- **Cross-step Review dispatch:** rejected because Review does not own Build/Test/Lint evidence and backward execution edges recreate cycles. Repairs remain same-step and forward-only within a pass.
- **Treat an earlier Review as failed or “stale”:** rejected because the Review did pass against its recorded subject. The tested head is provenance, not a new verdict.
- **Build before Review:** rejected for this design in favor of retaining the current fork and upstream order.

## Consequences

The product must persist pass counters, per-gate tested heads, repair ownership, and final-verification state. Compact PR output may remove the standalone Review Evidence section, but it must retain underlying Review records and enough tested-head provenance to describe what each pass actually established.
