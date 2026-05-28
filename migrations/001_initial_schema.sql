-- 001_initial_schema.sql
-- Core knowledge-graph tables: projects, entities, observations, relations.
-- Plus the schema_migrations bookkeeping table used by the migrate runner.

create extension if not exists pgcrypto;

create table if not exists schema_migrations (
  version     text primary key,
  applied_at  timestamptz not null default now()
);

create table if not exists projects (
  id          uuid primary key default gen_random_uuid(),
  slug        text unique not null,
  name        text not null,
  created_at  timestamptz not null default now()
);

create table if not exists entities (
  id           uuid primary key default gen_random_uuid(),
  project_id   uuid not null references projects(id) on delete cascade,
  name         text not null,
  entity_type  text not null,
  created_at   timestamptz not null default now(),
  unique (project_id, name)
);

create index if not exists entities_project_idx      on entities (project_id);
create index if not exists entities_project_type_idx on entities (project_id, entity_type);

create table if not exists observations (
  id          uuid primary key default gen_random_uuid(),
  entity_id   uuid not null references entities(id) on delete cascade,
  content     text not null check (length(content) > 0 and length(content) < 32768),
  fts         tsvector generated always as (to_tsvector('simple', content)) stored,
  created_at  timestamptz not null default now()
);

create index if not exists observations_entity_recency_idx on observations (entity_id, created_at desc);
create index if not exists observations_fts_idx           on observations using gin (fts);

create table if not exists relations (
  id              uuid primary key default gen_random_uuid(),
  project_id      uuid not null references projects(id) on delete cascade,
  from_entity_id  uuid not null references entities(id) on delete cascade,
  to_entity_id    uuid not null references entities(id) on delete cascade,
  relation_type   text not null,
  created_at      timestamptz not null default now(),
  unique (from_entity_id, to_entity_id, relation_type)
);

create index if not exists relations_project_idx on relations (project_id);
create index if not exists relations_from_idx    on relations (from_entity_id);
create index if not exists relations_to_idx      on relations (to_entity_id);
