# Eval scenarios

Each scenario is a directory: `scenario.yaml` (metadata + ground truth),
`inject.sh` (breaks the bench cluster), and any manifests it needs. The full
18-scenario list is in `specs/001-build-spec.md` §4.2; the harness and grader
land in M4.

Scoring is deterministic wherever possible: root-cause class match, culprit
fuzzy match, evidence-citation checks against captured tool results, and
red-herring penalties. S17 (compound with red herring) and S18 (nothing is
actually wrong) exist to measure resistance to plausible-but-wrong stories.
