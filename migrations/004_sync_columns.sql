-- 004_sync_columns.sql
-- Adds the columns and indexes the local-first sync engine needs:
--   updated_at        : server-authoritative watermark used by pull
--   deleted_at        : soft-delete tombstone; reads filter IS NULL
--   last_writer_device: which device originated this row's most recent write
-- Plus updates the read RPCs to honor deleted_at, and an audit-log column
-- recording when a queued mutation actually reached the server.

-- Idempotent column additions ------------------------------------------------

alter table entities      add column if not exists updated_at         timestamptz not null default now();
alter table entities      add column if not exists deleted_at         timestamptz null;
alter table entities      add column if not exists last_writer_device text        null;

alter table observations  add column if not exists updated_at         timestamptz not null default now();
alter table observations  add column if not exists deleted_at         timestamptz null;
alter table observations  add column if not exists last_writer_device text        null;

alter table relations     add column if not exists updated_at         timestamptz not null default now();
alter table relations     add column if not exists deleted_at         timestamptz null;
alter table relations     add column if not exists last_writer_device text        null;

alter table audit_log     add column if not exists received_at        timestamptz not null default now();

-- Trigger to keep updated_at fresh on UPDATE -------------------------------

create or replace function set_updated_at() returns trigger
language plpgsql as $$
begin
  new.updated_at = now();
  return new;
end;
$$;

do $$ begin
  if not exists (select 1 from pg_trigger where tgname = 'entities_set_updated_at') then
    create trigger entities_set_updated_at      before update on entities      for each row execute function set_updated_at();
    create trigger observations_set_updated_at  before update on observations  for each row execute function set_updated_at();
    create trigger relations_set_updated_at     before update on relations     for each row execute function set_updated_at();
  end if;
end $$;

-- Indexes for pull keyset pagination ---------------------------------------

create index if not exists entities_project_updated_idx     on entities     (project_id, updated_at);
create index if not exists observations_entity_updated_idx  on observations (entity_id, updated_at);
create index if not exists relations_project_updated_idx    on relations    (project_id, updated_at);

-- Update read RPCs to filter out tombstones --------------------------------

create or replace function search_observations(
  p_project_id uuid,
  p_query      text,
  p_limit      int default 20
) returns table (
  entity_id      uuid,
  entity_name    text,
  entity_type    text,
  observation_id uuid,
  content        text,
  rank           real
)
language sql stable as $$
  select
    e.id, e.name, e.entity_type,
    o.id, o.content,
    ts_rank(o.fts, websearch_to_tsquery('simple', p_query))
  from observations o
  join entities e on e.id = o.entity_id
  where e.project_id = p_project_id
    and e.deleted_at is null
    and o.deleted_at is null
    and o.fts @@ websearch_to_tsquery('simple', p_query)
  order by ts_rank(o.fts, websearch_to_tsquery('simple', p_query)) desc
  limit p_limit;
$$;

create or replace function read_graph(
  p_project_id   uuid,
  p_entity_limit int default 5000
) returns jsonb
language sql stable as $$
  with picked as (
    select e.*
    from entities e
    where e.project_id = p_project_id and e.deleted_at is null
    order by e.created_at desc
    limit p_entity_limit
  ),
  with_observations as (
    select
      p.id, p.name, p.entity_type, p.created_at,
      coalesce(
        (select jsonb_agg(jsonb_build_object('content', o.content, 'created_at', o.created_at)
                          order by o.created_at desc)
         from observations o where o.entity_id = p.id and o.deleted_at is null),
        '[]'::jsonb
      ) as observations
    from picked p
  ),
  with_relations as (
    select
      w.*,
      coalesce(
        (select jsonb_agg(jsonb_build_object(
                   'to', te.name,
                   'type', r.relation_type,
                   'created_at', r.created_at))
         from relations r
         join entities te on te.id = r.to_entity_id
         where r.from_entity_id = w.id
           and r.deleted_at is null
           and te.deleted_at is null),
        '[]'::jsonb
      ) as relations
    from with_observations w
  )
  select jsonb_build_object(
    'entities', coalesce(jsonb_agg(jsonb_build_object(
      'name', name,
      'entityType', entity_type,
      'createdAt', created_at,
      'observations', observations,
      'relations', relations
    ) order by created_at desc), '[]'::jsonb),
    'stats', jsonb_build_object(
      'entityCount',     (select count(*) from entities      where project_id = p_project_id and deleted_at is null),
      'observationCount',(select count(*) from observations o join entities e on e.id = o.entity_id where e.project_id = p_project_id and e.deleted_at is null and o.deleted_at is null),
      'relationCount',   (select count(*) from relations     where project_id = p_project_id and deleted_at is null)
    )
  )
  from with_relations;
$$;

create or replace function open_nodes(
  p_project_id uuid,
  p_names      text[]
) returns jsonb
language sql stable as $$
  with picked as (
    select e.*
    from entities e
    where e.project_id = p_project_id
      and e.deleted_at is null
      and e.name = any (p_names)
  ),
  with_observations as (
    select
      p.id, p.name, p.entity_type, p.created_at,
      coalesce(
        (select jsonb_agg(jsonb_build_object('content', o.content, 'created_at', o.created_at)
                          order by o.created_at desc)
         from observations o where o.entity_id = p.id and o.deleted_at is null),
        '[]'::jsonb
      ) as observations
    from picked p
  ),
  with_relations as (
    select
      w.*,
      coalesce(
        (select jsonb_agg(jsonb_build_object(
                   'to', te.name,
                   'type', r.relation_type,
                   'created_at', r.created_at))
         from relations r
         join entities te on te.id = r.to_entity_id
         where r.from_entity_id = w.id
           and r.deleted_at is null
           and te.deleted_at is null),
        '[]'::jsonb
      ) as relations
    from with_observations w
  )
  select coalesce(jsonb_agg(jsonb_build_object(
    'name', name,
    'entityType', entity_type,
    'createdAt', created_at,
    'observations', observations,
    'relations', relations
  )), '[]'::jsonb)
  from with_relations;
$$;
