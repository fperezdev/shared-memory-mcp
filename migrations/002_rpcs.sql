-- 002_rpcs.sql
-- JSONB RPCs that collapse multi-table fetches into a single round-trip
-- and a transactional upsert helper for the hot create_entities path.

-- Search observations within a project, ranked by ts_rank.
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
    and o.fts @@ websearch_to_tsquery('simple', p_query)
  order by ts_rank(o.fts, websearch_to_tsquery('simple', p_query)) desc
  limit p_limit;
$$;

-- Read the entire graph for a project as a single jsonb document.
-- Shape: { entities: [{name, type, observations: [...], relations: [...]}], stats: {...} }
create or replace function read_graph(
  p_project_id   uuid,
  p_entity_limit int default 5000
) returns jsonb
language sql stable as $$
  with picked as (
    select e.*
    from entities e
    where e.project_id = p_project_id
    order by e.created_at desc
    limit p_entity_limit
  ),
  with_observations as (
    select
      p.id, p.name, p.entity_type, p.created_at,
      coalesce(
        (select jsonb_agg(jsonb_build_object('content', o.content, 'created_at', o.created_at)
                          order by o.created_at desc)
         from observations o where o.entity_id = p.id),
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
         where r.from_entity_id = w.id),
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
      'entityCount', (select count(*) from entities where project_id = p_project_id),
      'observationCount', (select count(*) from observations o join entities e on e.id = o.entity_id where e.project_id = p_project_id),
      'relationCount', (select count(*) from relations where project_id = p_project_id)
    )
  )
  from with_relations;
$$;

-- Open specific nodes by name with full context.
create or replace function open_nodes(
  p_project_id uuid,
  p_names      text[]
) returns jsonb
language sql stable as $$
  with picked as (
    select e.*
    from entities e
    where e.project_id = p_project_id
      and e.name = any (p_names)
  ),
  with_observations as (
    select
      p.id, p.name, p.entity_type, p.created_at,
      coalesce(
        (select jsonb_agg(jsonb_build_object('content', o.content, 'created_at', o.created_at)
                          order by o.created_at desc)
         from observations o where o.entity_id = p.id),
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
         where r.from_entity_id = w.id),
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

-- Atomically upsert an entity and append observations. Used by create_entities
-- and add_observations to avoid select-then-insert race windows.
create or replace function upsert_entity_with_observations(
  p_project_id   uuid,
  p_name         text,
  p_type         text,
  p_observations text[]
) returns uuid
language plpgsql as $$
declare
  v_entity_id uuid;
  v_content   text;
begin
  insert into entities (project_id, name, entity_type)
  values (p_project_id, p_name, p_type)
  on conflict (project_id, name) do update
    set entity_type = excluded.entity_type
  returning id into v_entity_id;

  if p_observations is not null and array_length(p_observations, 1) > 0 then
    foreach v_content in array p_observations loop
      if length(v_content) between 1 and 32767 then
        insert into observations (entity_id, content) values (v_entity_id, v_content);
      end if;
    end loop;
  end if;

  return v_entity_id;
end;
$$;
