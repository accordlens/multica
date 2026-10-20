# Independent evaluation for v1.0.0

The contract author cannot be the only evaluator. For the PR, independent
ClaudeQA reviews the exact head and repeats the commands in the README. Human
acceptance still follows the task's recorded reviewer rules. QA findings belong
to the task and revision, not a general opinion about the role.

## Role goals

Assess every goal in `dev.json` and `qa.json` against its own `evaluation` field.
Record the goal ID, evidence reference/hash, reviewer identity, source SHA,
observed behavior, score and remaining uncertainty. The examples and
anti-examples in those files define the distinctions to look for.

Use this ordinal scale; do not average away a critical defect:

- **0 — not met:** evidence contradicts the expected behavior, or a critical
  claim has no evidence.
- **1 — partial:** some behavior is demonstrated, but a material case or the
  required independent verification is missing.
- **2 — met in the stated scope:** evidence supports the behavior and the
  reviewer repeats the applicable key scenario.
- **null — not assessed:** name the missing evidence and next check. Null is
  not zero and cannot count as passed.

Review all eight Dev goals: problem, correctness, simplicity, diagnosis,
evidence, uncertainty, review_response, handoff. Review all five QA goals:
independence, regressions, reproducibility, criteria, decision. A fixture does
not demonstrate the real agent's behavior on these goals.

## Same-case comparison

The corpus covers success, blocker, unknown fact, review disagreement, changed
requirements and conflicting evidence. Each has English and Polish prose. The
baseline/profile pairs are manually authored fixtures, not sampled outputs of
baseline and newly configured models. Their critical evidence text is the same.
Missing fields in the legacy baseline are deliberate. Its higher checker error
count demonstrates the envelope rules, not improved model performance.

The deterministic report measures:

- Required-field and structured-state validity; correctness of source links and
  context relative to the supplied evidence.
- Exact critical-claim retention. Tests deliberately remove negation, dates,
  numbers, commands and error meaning, change a run ID, reuse old evidence and
  hide failed checks.
- UTF-8 bytes, whitespace prose words, advisory sentence/term warnings and local
  checker duration. These are labelled proxies, not model token counts or
  generation time.

It records provider, model, input/output tokens, model latency, cost, additional
model revisions, task/semantic quality and readability as **null**. There were
no provider evaluation calls. Do not infer token savings, free usage, model
rankings, style compliance or semantic improvement from these fixtures.

An independently reviewed case record can use this neutral format:

```json
{
  "case_id": "unknown_fact",
  "variant": "profile",
  "source_revision": "<exact reviewed SHA>",
  "provider": null,
  "model": null,
  "reviewer_id": "<existing reviewer identity>",
  "task_alignment": null,
  "fact_preservation": null,
  "handoff_correctness": null,
  "readability": null,
  "evidence": [],
  "uncertainty": ["No independent semantic score yet."]
}
```

Use the 0/1/2/null scale for those four quality dimensions, citing specific
statements. For readability, have a Polish reader identify the result, its
certainty and the required next action without the author's explanation. For
fact preservation, compare both directions, including negation scope and error
meaning; a token match alone is insufficient. Repeat for English agent prose.
Keep the original variant labels hidden during an independently arranged
language assessment when feasible; reveal them after recording the scores.

For a future approved model experiment, freeze the same cases and acceptance
criteria before generation. Run both variants with the same per-model task
inputs and environment, retain raw outputs and usage with stable operation IDs,
then have a separate reviewer score them. Record provider/model separately;
do not pool unmeasured or differently priced usage. Confirm budget before paid
calls. This sprint does not authorize new paid API evaluation.

## Pilot acceptance record

- Offline contracts/checker: automated results plus exact source SHA and report
  hash. A local failure blocks that scope and retains original critical content.
- Existing UI/API index: stored receipt for an API-returned run, metadata
  readback, report/profile attachments, and the existing Metadata dialog.
- Independent PR review: separate reviewer/run ID, exact head and explicit
  decision. A fixture or the author's own tests cannot fill this item.
- Agent behavior and language efficacy: pending until independent evidence is
  recorded. A prompt and linter do not guarantee either.
- Persistent activation: pending a separate decision on the concrete preview.
  It is not necessary to run the offline pilot and is not authorized by this PR.

Required checks, current-head review and applicable human acceptance remain
separate gates. No style score can turn an untested behavior into PASS.
