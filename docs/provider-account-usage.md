Provider account usage is a sourced snapshot, separate from the existing run
token/cost reports. This first slice supports manually recorded Anthropic Claude
subscription usage only. It does not call a provider, scrape credentials, estimate
a plan allowance from tokens, enforce quotas, refill limits, or resume runs.

## Official sources and limits

Public documentation checked on 23 October 2026:

- [Claude Code costs and /usage](https://code.claude.com/docs/en/costs): the session
  token/dollar figure differs from subscription plan usage. The interactive
  `/usage` screen can show plan usage. A failed usage fetch can show cached bars;
  preserve the original observation time, rather than the time they were copied.
- [Claude Pro/Max and Claude Code](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan):
  account activity shares subscription windows across Claude and Claude Code.
- [Claude API rate limits](https://platform.claude.com/docs/en/api/rate-limits):
  API organization/workspace limits and response headers describe a different
  scope from a Claude subscription. API reset headers must not be used as the
  reset of a subscription pool.

These sources do not establish a supported public machine API for this runtime's
subscription quota/reset. This implementation therefore accepts a member's manual
snapshot of `/usage` or Claude Settings → Usage. It has no provider API endpoint
or collector. `null` means UNKNOWN, including an unavailable reset, percentage,
window boundary, or remaining allowance. A sourced numeric zero stays zero.
Percent used does not calculate remaining, and token/cost reports never populate
this snapshot. Plan allowances, account identity, or reset times are not guessed.

## Record and read

The authenticated runtime owner prepares a JSON file using
`server/pkg/accountusage/testdata/claude-manual.json` as the **synthetic format
example**, replacing its values with actual sourced evidence. The example is not
a live account reading. Do not upload credentials, email addresses, raw transcripts
or billing data. `account_ref`, `observation_id`, and `evidence_ref` are opaque
references; the account alias must be consistent across the owner's runtimes.
Record a new observation ID and its actual observation timestamp on each refresh.

```sh
multica runtime usage RUNTIME_UUID --record-account-snapshot ./snapshot.json --output json
multica runtime usage RUNTIME_UUID --account --output json
```

The first command writes through `PUT /api/runtimes/{runtimeId}/usage/account`,
then reads back through `GET` at that path. It requires member credentials and
runtime ownership; task tokens, cloud machine credentials and non-owner admins
cannot attest a snapshot. The API stamps the recording member and recording time.
Reading uses the existing runtime access gate. Private runtime snapshots are not
exposed through the governance runtime list.

Within the current workspace, the projection groups readable runtimes by owner,
provider and account reference. It selects the newest observation once and lists
its runtime bindings. Two agents on that runtime share the same pool; their run
costs are not combined with its allowance. Different accounts or owners remain
separate. Equally recent conflicting observations display UNKNOWN until a newer
manual reading resolves them. A newer snapshot replaces the previous current
observation; this is not a new cost ledger or an observation-history service.

The runtime association is an explicit owner assertion about the default account.
Per-agent credential overrides and cross-workspace account matching are not
discovered. Do not assert a shared identity when those bindings are uncertain.

## Freshness and workflow

`observed_at` and `stale_after_seconds` describe freshness; record times do not
refresh the source observation. A passed `reset_at` or known window end also makes the snapshot stale.
The reset is an absolute RFC 3339 instant with its explicit source IANA zone,
including DST. Never convert a vague provider time into an invented date/zone.
The UI recomputes freshness on rendering and existing query refresh/focus. There
is no polling timer. Stale figures stay labelled as observed evidence.

An unchanged observation ID/content is idempotent. Reusing that ID with different
evidence or replacing a current snapshot with an older/equally dated observation
returns 409. The write holds the existing runtime row lock, rechecks ownership,
stores the snapshot in the existing JSONB metadata and publishes the existing
runtime update event. Daemon re-registration preserves this server-owned key.
No schema migration or migration-number reservation is needed.

Fake rate limits retain `agent_error.provider_capacity_or_rate_limit` in the
existing workflow, whose retry posture remains manual. Recording evidence, replaying
a record or passing a reset never creates a run or clears a workflow pause. After
verifying fresh provider evidence, use the existing explicit retry/resume controls;
their access checks and deduplication remain authoritative.

## Verification and next slice

The shared synthetic JSON fixture drives Go projection, API-schema and UI tests.
Tests cover shared/separate accounts, owner scope, UNKNOWN versus zero, stale and
reset boundaries, DST, replay/conflict handling and fake rate-limit classification.
DB-backed handler tests cover member ownership, privacy, idempotent recording and
unchanged run counts. They require the reserved full local CI slot; narrow unit
checks do not replace that gate or independent review of the exact commit.

Persistent canonical account bindings across workspaces, per-agent overrides,
observation history and any future enforcement require a separate reviewed
extension coordinated with ACCO-93/94/132 and fresh DDL reservations if necessary.
This slice reserves no migration numbers and changes no provider settings.
