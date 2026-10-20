# Scoped review handoffs (ACCO-96)

Use an existing issue thread as the topic, ordinary comments as history, and a
finite, explicitly addressed wakeup condition as the work trigger. A decision
notice delivers information without requesting a model run. This pilot adds no
broker, scheduler, chat system, dependency engine, database migration or package.

## Research and choice

Audited on 20 October 2026: fork patches through `b92ed3655c106efd9033658ce405c8369a4e322f`,
upstream `47120a4131564175da044d63d897ed9b5d76979a`, and ACCO-128's prepared
integration `48c8249bb02604ab3550176b2cb76751ce8a2ca7`, followed by its E2E-only
fixture correction `0360c18d4333a4e6c9c34c173e419286562f8549`. The integration belongs
to ACCO-128; this change reuses it. The manifests pin Go 1.26.9, pgx 5.11.0,
Chi 5.3.2, pnpm 10.28.2 and PostgreSQL 17 for the local environment.

The existing implementation already separates these concerns:

- `handler/comment.go` persists comments, publishes realtime updates and resolves
  explicit agent addressing. Ordinary issue comments retain their existing
  conversation rules. This change does not modify private chat or invocation.
- `issueworkflow` and `handler/issue_workflow_entry.go` own project workflow
  columns, handoffs, assignment and protection against a moved source step.
- `service/issue_wakeup*`, `pkg/db/queries/wakeup.sql` and the existing scheduler
  own durable event receipts, dispatch transactions, offline recovery, authority
  checks, bounded prompt evidence and trigger-chain limits.
- Workspace membership and attachment download authorization remain the source
  ACL. A subscription is not permission to read another workspace or source.

Three alternatives were compared against Dev→QA review, decision changes,
parallel tasks sharing an agreement, offline return and human-only discussion:

1. **Task/thread plus addressed handoff — selected.** It has the existing issue,
   thread, run and artifact identities, one native queue and current source ACL.
   A bare mention cannot pin a report to a source revision or distinguish a
   notice from a request. The additional envelope and condition supply that
   distinction. One request needs one logical recipient run; information-only
   messages need none. This is the only implemented/measured candidate.
2. **Durable knowledge board with threads.** Useful for finding a cross-task
   agreement, but duplicating source content introduces synchronization and ACL
   work without improving this review handoff. The pilot instead indexes exact
   source pointers in existing issue metadata. ACCO-97 can consume these
   pointers; a summary or index entry does not satisfy a workflow dependency.
3. **Topics/pub-sub with stored subscriptions.** Appropriate when independent
   consumers need distinct retained streams. For two roles on one PC, a broker
   adds a service, consumer permissions, retention policy and recovery surface.
   Scoped wakeups already provide the required stored subscriptions and receipts.
   No broker benchmark or performance advantage is claimed.

Protocol sources informed the boundaries, rather than the storage architecture:

- [CloudEvents 1.0.2](https://github.com/cloudevents/spec/blob/v1.0.2/cloudevents/spec.md)
  defines event identity using source plus ID. It is an event format, not a
  durable execution engine. The pilot borrows stable producer identity and a
  versioned type; its envelope is not advertised as CloudEvents compliant.
- [A2A 1.0.1](https://a2a-protocol.org/v1.0.1/specification/#331-idempotency)
  distinguishes task/message/artifact concepts and makes send-message
  idempotency optional. Adopting that protocol would not establish exactly-once
  model execution or source permissions for Multica.
- [NATS JetStream 2.12 reference](https://docs.nats.io/reference/2.12/jetstream)
  and [delivery/acknowledgment documentation](https://docs.nats.io/learn/jetstream/delivery-and-acknowledgment)
  describe persisted delivery, consumer acknowledgments and redelivery. They
  are comparison material, accessed 20 October 2026; NATS is not installed.
  Broker delivery guarantees do not make an external API side effect atomic.

## Message and subscription contract

`issueworkflow.HandoffEnvelope` is the complete JSON inside one
`json title="Multica handoff v1"` code fence. There are three intents:
`review.request`, `review.result`, `decision.changed`. Only the first can satisfy
the new `review_handoff` condition. Free text carries goal, summary and explicit
uncertainty. It must not include side-effecting `mention://` links.

The envelope's `id` is a stable producer event UUID, distinct from the server's
comment UUID. `issue_id` is the existing issue UUID; `thread_id` is the existing
root comment; `run_id` is the source `agent_task_queue.id`; `workflow_step` is
that run's stored status key. `workflow_id` is the project's actual named
workflow. `workflow_version` fingerprints the normalized stored steps, including
handlers, instructions and next/back edges. Workflow API responses expose this
as `definition_version`. A full source SHA is required. Artifact references are
existing issue-owned attachment IDs plus SHA-256 content references. The digest
is an author's reference, not a server-certified test result.

Registration pins thread, sender, workflow/version, source SHA and the wakeup's
recipient. It requires two distinct agents, `once`, an expiry within 72 hours,
and no timeout invocation. Duplicate subscriptions for the same scope are
rejected, including a previously consumed subscription. An explicit update is a
new subscription revision and an intentional retry; deleting and recreating one
also creates new authorization. There is no automatic retry conversation.

Before dispatch the service checks actual comment authorship and source-run
lineage, the source run's issue/step/human originator, current workflow version,
sender invocation access and attachment ownership. Another issue, private chat,
captured source object, deleted thread or missing report cannot supply a report
to this subscription. The request goes through the existing wakeup queue.
It never joins an unrelated pending run's prompt. Claim rechecks current
authority, expiry, source availability and the exact frozen envelope.

The logical run allowance is one per subscription revision. Existing scheduler
chain/rate protection still applies; two reciprocal role subscriptions have at
most two logical runs. These are execution-count bounds, not a provider invoice
cap. The synthetic pilot invokes no provider.

## Before and after

Before: “QA, review the latest changes” can leave the intended revision, report,
expected answer and replay behavior unclear.

After: Dev publishes `review.request` for a full SHA with its actual run,
workflow version and report attachment. Only the subscribed QA can consume that
scope. QA returns `review.result` with the request's `causation_id`, the same
revision and report references. Results explicitly expect `none`, so the reply
does not wake Dev. The decision remains in the task thread. The pilot writes a
small `handoff_index_v1` metadata pointer with comment/run/revision/decision and
artifact IDs using the existing metadata API. This is an explicit publication
step, not an automatic knowledge engine. It never sets `done`, claims PASS or
replaces ACCO-93/94's workflow/dependency evidence.

A changed decision remains an ordinary durable `decision.changed` comment.
Parallel tasks can reference the same source task/thread/decision; they do not
receive new authority or become unblocked merely because they have that link.

## Run the local pilot

Use the managed worktree environment with a separate synthetic database. Do not
point the runner at production. It rejects a missing URL, non-loopback database
host or database name outside the managed `multica_*` namespace.

```sh
make up C=api ARGS='--name review-pilot --ephemeral'
make env-exec ARGS='-- bash scripts/review-handoff-pilot.sh'
make down
```

The runner uses existing Go fixtures and PostgreSQL. Its external execution
boundary is an asserted queue row and a synthetic reviewer result. A guard
rejects invocation of an installed provider CLI. It requires each named test to
pass; a skipped/unavailable database is not accepted as a successful pilot.
JSONL evidence and a JSON summary are written under `test-results/`.

For a manual subscription on a local test instance, save this JSON using real
IDs and the workflow API's returned definition version:

```json
{
  "thread_id": "<existing root comment UUID>",
  "sender_id": "<Dev agent UUID>",
  "workflow_id": "<project workflow UUID>",
  "workflow_version": "<64-character definition_version>",
  "source_revision": "<full source SHA>"
}
```

```sh
multica issue wakeup create ISSUE --agent-id QA_UUID \
  --until-handoff-file ./filter.json --expires-in 1h \
  --instruction-file ./review-instruction.txt
```

Dev posts the encoded envelope with ordinary `issue comment add --parent` and
`--content-file` from its source run. Keep the producer event ID unchanged when
reposting after a lost response. Use `wakeup get`, `runs`, `disable` or `delete`
to inspect or withdraw the subscription. `trigger` cannot bypass this predicate;
inspect the prior run before an explicit subscription update/retry. Update the
source SHA and report when code changes; an old pinned review is not approval
for a new head. Existing private chat and explicit invocation remain separate.

The browser check is `pnpm exec playwright test e2e/review-handoff.spec.ts`. It
uses `TestApiClient` to create/clean synthetic task data and captures
`test-results/review-handoff.png`. No daemon or provider is needed.

## Measurements and failure cases

The local `-race` pilot passed 35 cases with zero skips. It observed:
20 human-only comments in an unassigned source task/thread → 0 new runs;
information-only notice → 0 runs;
offline persistence plus repeated event → 1 logical handoff; terminal
result → 0 extra runs. Two scheduler passes after a simulated 120-second offline
delay took about 33 ms to recover and queue the handoff on this Mac. This is one
local observation, not a latency SLA or model turnaround measurement. The normal
condition polling fallback is 30 seconds; durable comment hints can reach the
condition sooner. `age_seconds`, `published_at`, the full SHA and `delayed`
(at least 60 seconds old) accompany the request.

Cases include wrong recipient/revision/author/run, missing or foreign report,
changed workflow, removed subscription, revoked membership, archived sender,
expired deadline, post-dispatch source edits, concurrent dispatch, finite
request floods and the existing causal loop guard. A separate HTTP test
publishes through the ordinary comment API, repeats the producer event with a
different server comment UUID, and checks live membership revocation, source history/file denial and removal
of subscription history. API schemas preserve valid provenance and reject
malformed fields; older workflow responses remain readable.

Routing/gates/dedup use zero model calls and zero routing tokens. Synthetic
provider spend is $0 because no provider is invoked. Actual Dev/QA runtime cost
is separate and must come from Multica usage; it is not measured by this test.
No model quality, provider turnaround or token-saving claim is made.

## Limits, ACL and rollback

This pilot supports explicit named project workflows, excludes squad-owned
conversations with their separate leader/worker routing, and supports up to 100 handoff
records per subscribed sender in one root thread (maximum 12KB and eight artifact references per
envelope). Exceeding the bound fails closed with a visible diagnostic; use a new
scoped thread. Delivery follows committed comment order, not sender clock order.
Reposting identical producer data is harmless; changed content cannot silently
replace a frozen prompt. Missing/out-of-order lineage waits for a later poll.

Visibility is exactly the source issue's workspace ACL. This is not a private
subconversation inside a shared issue. Unsubscribe removes subscription history
and prevents new delivery/claim; it does not revoke an independently held right
to read source comments. Membership revocation removes source read access and
claim authority. Already received data or a started process cannot be recalled.
No raw storage URL or signed download capability is copied into routing facts.

Human discussion in this pilot uses an unassigned issue and a member-owned
thread. Entering a project's agent-handled workflow column deliberately starts
the existing workflow run; normal member conversation with an assigned agent
also retains its existing behavior. Neither is reclassified as a human-only
conversation. Private chat/invocation semantics are unchanged.

The database transaction and stable receipt key prevent duplicate logical
delivery for one saved subscription revision. They cannot atomically commit an
external model/API side effect. A timeout after such an effect requires checking
the prior run and provider operation ID before explicit retry. No general
exactly-once guarantee is made. Revocation after the final claim check can also
race execution; existing cancellation/Stop controls remain necessary.

To roll back, disable/delete pilot subscriptions and revert this additive code
change. Keep the source thread, run history and indexed references for audit.
There is no schema downgrade or agent/binding/secret reconfiguration. Production
deployment and real provider evaluation are outside this synthetic pilot.
