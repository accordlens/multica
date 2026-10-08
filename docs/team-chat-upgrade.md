# Team chat C2 upgrade and recovery

This is an expansion of existing chat_session/chat_message records, protocol 2,
through schema 582. Team creation/send stays disabled in capabilities until C3/C4
and the C2 acceptance gates are accepted. Legacy endpoints return agent_dm only.

## Backup and upgrade

1. Stop chat writers and queue claims in a maintenance window. Record the running
   commit, image digests, configuration and SELECT version FROM schema_migrations.
2. Take a consistent PostgreSQL backup (`pg_dump --format=custom`) and a storage
   backup at the same boundary. Validate both in an isolated restoration; record
   session/message/task/input/attachment/read counts and their IDs before upgrade.
3. Upgrade the API/migrator together. Run `go run ./cmd/migrate up` from server.
   The runner serializes migration runners. Expansion/backfill 565 is transactional;
   566–578, 580 and 582 build one index CONCURRENTLY per file. The runner cleans an
   INVALID interrupted index before retry. 579 preserves protected-object tombstones.
4. Verify 582 is recorded and all new indexes have indisvalid/indisready=true. Check
   authors (human user.id; historical assistant task.agent_id), deterministic
   message_seq and legacy last_read_at/read counts against the backup. IDs,
   task input ownership, existing attachment bindings and stored content stay intact.
5. Run the four legacy regressions and C2 HTTP/WS/file negatives, including an open
   socket, copied user-bound tickets, `/uploads` aliases and removed membership.
   Team capabilities must remain false until the integrated downstream paths pass.

Backfill is retryable: assigned sequences/authors and existing read/participant
records are preserved. It updates existing rows and can hold locks on a large
history; measure the maintenance window on a restored representative database.
Every canonical append now serializes sequence allocation on the conversation.
Legacy channel enqueue and ingress briefly serialize too. Preparation/network I/O
stays outside enqueue transactions; an input arriving after enqueue belongs to
its next immutable batch. The C2 concurrency tests cover this ordering.

## Recovery

An older image is not a database rollback. Expand migrations deliberately retain
new data on down; concurrent index down files only remove indexes. Prefer a
forward repair and rerun of the migrator for an interrupted index build. Never
mark an invalid index as applied by hand. For a failed data upgrade restore the
validated database AND storage backup into an isolated target, verify counts and
old-reader behavior, then switch the target only with deployment authorization.
Do not remove protected-object tombstones while any old object URL may survive
failed storage GC. Deletion prunes participants/reads/threads/events/drafts in the
same transaction; tombstones are intentionally retained as security records.
Task privacy records also survive the historical FK clearing chat_session_id.
Migration 581 conservatively quarantines pre-existing tasks with neither issue
nor conversation ownership: their source may be a previously deleted private
chat. Their rows/input/result remain intact, but human-facing history stays
hidden until an operator reviews/classifies the origin from backup evidence.
Never grant a workspace admin automatic access to these unknown records. New
unscoped public tasks are explicitly classified so retry does not quarantine
them. Conversation scope cannot be reassigned on an existing private task.

## Storage origin gate

Local disk is served only through API `/uploads`; do not expose the upload
volume as a separate nginx/static-server directory. The route checks protected
objects, current conversation ACL and normalized keys, including legacy paths
and deleted attachment rows. Unrelated issue objects keep their existing path.

For S3/MinIO/CDN, the application cannot revoke a URL served outside its process.
Before enabling team chat, operators must prove the bucket/origin is private and
that private-chat and historical chat object keys are excluded from CDN wildcard
cookies, public policies and old presigned capabilities. Migrate historical
objects/URLs and invalidate old CDN paths when required. A workspace-wide CDN
cookie is not conversation authorization. Restrict private reads to the API
service identity; only the API proxies bytes. Obtain direct-origin denial evidence
for anonymous users, an active nonparticipant and a revoked user. No cloud policy
or production storage changes are performed by these migrations. A deployment
without this evidence fails the C2 origin gate and must not enable team chat.

Private API tickets expire in 60 seconds and bind user, conversation, acl_version
and attachment. Redemption also authenticates the user. Web uses same-origin
cookies; desktop/native clients fetch authorized bytes and use local blobs.
Already downloaded copies cannot be retracted. Current uploads remain 100 MiB;
C6 owns the separate streaming/quarantine integration.

## Review replay

Run `make test` against an isolated database, then the C2 tests in
`server/internal/handler/team_chat_test.go` and
`server/cmd/server/team_chat_ws_test.go`. Repeat the open-socket revoke and copied
file-ticket cases as an independent reviewer and record the result on ACCO-65.
C3 owns create/operation-ID management integration; C4 owns send/outbox/replay;
C8 owns chat search. C2 keeps private chat out of the workspace issue search index
and enforces current ACL on legacy fanout/files without deferring those gaps.
