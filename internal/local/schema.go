package local

// SchemaSQL is the DDL applied to a fresh local.db. It mirrors the Postgres
// content tables, swapping tsvector for FTS5 and replacing server-side
// generated timestamps with TEXT columns that the Go code populates.
//
// The cache is single-tenant (one project per server instance), but the
// project_id column is kept so a deleted-then-recreated cache can be
// rebuilt from a paginated pull without filtering shenanigans.
const SchemaSQL = `
create table if not exists projects (
  id          text primary key,
  slug        text not null unique,
  name        text not null,
  created_at  text not null
);

create table if not exists entities (
  id                 text primary key,
  project_id         text not null,
  name               text not null,
  entity_type        text not null,
  created_at         text not null,
  updated_at         text not null,
  deleted_at         text,
  last_writer_device text,
  sync_state         text not null default 'synced',  -- 'pending_push' | 'synced'
  unique(project_id, name)
);
create index if not exists entities_project_name_idx    on entities(project_id, name);
create index if not exists entities_project_updated_idx on entities(project_id, updated_at);

create table if not exists observations (
  id                 text primary key,
  entity_id          text not null,
  content            text not null,
  created_at         text not null,
  updated_at         text not null,
  deleted_at         text,
  last_writer_device text,
  sync_state         text not null default 'synced'
);
create index if not exists observations_entity_recency_idx on observations(entity_id, created_at desc);
create index if not exists observations_entity_updated_idx on observations(entity_id, updated_at);

-- FTS5 virtual table indexing observation content. content= refers back to
-- the observations table by rowid; triggers below keep the index in sync.
create virtual table if not exists observations_fts using fts5(
  content,
  content='observations',
  content_rowid='rowid'
);

create trigger if not exists observations_ai after insert on observations begin
  insert into observations_fts(rowid, content) values (new.rowid, new.content);
end;
create trigger if not exists observations_ad after delete on observations begin
  insert into observations_fts(observations_fts, rowid, content) values('delete', old.rowid, old.content);
end;
create trigger if not exists observations_au after update on observations begin
  insert into observations_fts(observations_fts, rowid, content) values('delete', old.rowid, old.content);
  insert into observations_fts(rowid, content) values (new.rowid, new.content);
end;

create table if not exists relations (
  id                 text primary key,
  project_id         text not null,
  from_entity_id     text not null,
  to_entity_id       text not null,
  relation_type      text not null,
  created_at         text not null,
  updated_at         text not null,
  deleted_at         text,
  last_writer_device text,
  sync_state         text not null default 'synced',
  unique(from_entity_id, to_entity_id, relation_type)
);
create index if not exists relations_project_idx         on relations(project_id);
create index if not exists relations_from_idx            on relations(from_entity_id);
create index if not exists relations_to_idx              on relations(to_entity_id);
create index if not exists relations_project_updated_idx on relations(project_id, updated_at);

create table if not exists audit_log (
  id              text primary key,
  project_id      text not null,
  device_id       text not null,
  tool_name       text not null,
  args_hash       text not null,
  occurred_at     text not null,
  sync_state      text not null default 'pending_push'
);
create index if not exists audit_log_pending_idx on audit_log(sync_state, occurred_at);

-- pending_writes is the write-behind queue. payload is JSON keyed by op.
create table if not exists pending_writes (
  id           integer primary key autoincrement,
  op           text not null,
  payload      text not null,
  enqueued_at  text not null,
  attempts     integer not null default 0,
  next_attempt_at text not null default (datetime('now')),
  last_error   text
);
create index if not exists pending_writes_due_idx on pending_writes(next_attempt_at);

-- sync_cursor stores per-project pull watermarks (one for each content table).
create table if not exists sync_cursor (
  project_id           text not null,
  resource             text not null,  -- 'entities' | 'observations' | 'relations'
  last_pulled_at       text not null default '1970-01-01T00:00:00Z',
  primary key (project_id, resource)
);
`
