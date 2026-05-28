-- 003_audit.sql
-- Per-device audit log of every mutation. Writes are best-effort: an audit
-- failure must not block a successful tool call (the MCP server logs and
-- continues if the audit insert fails).

create table if not exists audit_log (
  id           uuid primary key default gen_random_uuid(),
  project_id   uuid not null references projects(id) on delete cascade,
  device_id    text not null,
  tool_name    text not null,
  args_hash    text not null,
  occurred_at  timestamptz not null default now()
);

create index if not exists audit_log_project_time_idx on audit_log (project_id, occurred_at desc);
create index if not exists audit_log_device_idx       on audit_log (project_id, device_id, occurred_at desc);
