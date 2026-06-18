// Package tools wires MCP tool handlers to the local cache, the
// write-behind queue, and the audit log. Handlers never block on the
// network: writes go to SQLite, enqueue a pending op, and return.
package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/fperez/shared-memory-mcp/internal/audit"
	"github.com/fperez/shared-memory-mcp/internal/local"
)

// Trigger is what tool handlers call after enqueueing a write — it nudges
// the sync engine to drain ASAP rather than waiting for the next tick.
type Trigger interface {
	Trigger()
}

type Ctx struct {
	DB        *sql.DB
	ProjectID string
	DeviceID  string
	Sync      Trigger
}

// Register adds every tool to the MCP server.
func Register(s *server.MCPServer, c *Ctx) {
	s.AddTool(toolCreateEntities(), wrap(c, handleCreateEntities))
	s.AddTool(toolAddObservations(), wrap(c, handleAddObservations))
	s.AddTool(toolAddRelations(), wrap(c, handleAddRelations))
	s.AddTool(toolDeleteEntities(), wrap(c, handleDeleteEntities))
	s.AddTool(toolDeleteObservations(), wrap(c, handleDeleteObservations))
	s.AddTool(toolDeleteRelations(), wrap(c, handleDeleteRelations))
	s.AddTool(toolReadGraph(), wrap(c, handleReadGraph))
	s.AddTool(toolSearchNodes(), wrap(c, handleSearchNodes))
	s.AddTool(toolOpenNodes(), wrap(c, handleOpenNodes))
}

// wrap adapts a handler that knows about our Ctx into the mcp-go
// signature. It also returns errors as tool errors (visible to the
// model) rather than as Go errors (which mcp-go would treat as fatal).
func wrap(c *Ctx, h func(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error)) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := h(ctx, c, req)
		if err != nil {
			return mcp.NewToolResultErrorFromErr(req.Params.Name+" failed", err), nil
		}
		return res, nil
	}
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return mcp.NewToolResultText(string(b)), nil
}

// ---- create_entities ----

func toolCreateEntities() mcp.Tool {
	return mcp.NewTool("create_entities",
		mcp.WithDescription("Create one or more entities in the project's knowledge graph. Each entity has a name (unique per project), a type tag, and optional initial observations."),
		mcp.WithArray("entities",
			mcp.Required(),
			mcp.Description("List of entities to create."),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":         map[string]any{"type": "string"},
					"entityType":   map[string]any{"type": "string"},
					"observations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"name", "entityType"},
			}),
		),
	)
}

type entityInput struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

func handleCreateEntities(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Entities []entityInput `json:"entities"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Entities) == 0 {
		return nil, fmt.Errorf("entities must be non-empty")
	}

	created := []string{}
	for _, e := range args.Entities {
		if e.Name == "" || e.EntityType == "" {
			return nil, fmt.Errorf("entity name and entityType are required")
		}
		id, err := local.UpsertEntity(ctx, c.DB, c.ProjectID, e.Name, e.EntityType, c.DeviceID)
		if err != nil {
			return nil, err
		}
		if _, err := local.Enqueue(ctx, c.DB, local.OpUpsertEntity, map[string]string{
			"ID": id, "ProjectID": c.ProjectID, "Name": e.Name, "EntityType": e.EntityType,
		}); err != nil {
			return nil, err
		}
		for _, content := range e.Observations {
			if content == "" {
				continue
			}
			obsID, err := local.AddObservation(ctx, c.DB, id, content, c.DeviceID)
			if err != nil {
				return nil, err
			}
			if _, err := local.Enqueue(ctx, c.DB, local.OpAddObservation, map[string]string{
				"ID": obsID, "EntityID": id, "Content": content,
			}); err != nil {
				return nil, err
			}
		}
		created = append(created, e.Name)
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "create_entities", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"created": created})
}

// ---- add_observations ----

func toolAddObservations() mcp.Tool {
	return mcp.NewTool("add_observations",
		mcp.WithDescription("Append free-text observations to existing entities by name."),
		mcp.WithArray("items", mcp.Required(),
			mcp.Description("List of {entityName, contents[]}."),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{"type": "string"},
					"contents":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"entityName", "contents"},
			}),
		),
	)
}

type obsAddItem struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

func handleAddObservations(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Items []obsAddItem `json:"items"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Items) == 0 {
		return nil, fmt.Errorf("items is required and must be non-empty (got %d items). Raw arguments may help: check the request payload.", len(args.Items))
	}
	type added struct {
		EntityName string `json:"entityName"`
		Count      int    `json:"count"`
	}
	results := []added{}
	for _, item := range args.Items {
		entityID, ok, err := local.EntityIDByName(ctx, c.DB, c.ProjectID, item.EntityName)
		if err != nil {
			return nil, err
		}
		if !ok {
			// Auto-create with a generic type so observations can land.
			entityID, err = local.UpsertEntity(ctx, c.DB, c.ProjectID, item.EntityName, "entity", c.DeviceID)
			if err != nil {
				return nil, err
			}
			_, _ = local.Enqueue(ctx, c.DB, local.OpUpsertEntity, map[string]string{
				"ID": entityID, "ProjectID": c.ProjectID, "Name": item.EntityName, "EntityType": "entity",
			})
		}
		count := 0
		for _, content := range item.Contents {
			if content == "" {
				continue
			}
			obsID, err := local.AddObservation(ctx, c.DB, entityID, content, c.DeviceID)
			if err != nil {
				return nil, err
			}
			_, _ = local.Enqueue(ctx, c.DB, local.OpAddObservation, map[string]string{
				"ID": obsID, "EntityID": entityID, "Content": content,
			})
			count++
		}
		results = append(results, added{EntityName: item.EntityName, Count: count})
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "add_observations", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"added": results})
}

// ---- add_relations ----

func toolAddRelations() mcp.Tool {
	return mcp.NewTool("add_relations",
		mcp.WithDescription("Create directed relations between entities (e.g. 'feature:auth' depends_on 'lib:jwt')."),
		mcp.WithArray("relations", mcp.Required(),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from":         map[string]any{"type": "string"},
					"to":           map[string]any{"type": "string"},
					"relationType": map[string]any{"type": "string"},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)
}

type relationInput struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

func handleAddRelations(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Relations []relationInput `json:"relations"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Relations) == 0 {
		return nil, fmt.Errorf("relations is required and must be non-empty")
	}
	added := 0
	for _, r := range args.Relations {
		if r.From == "" || r.To == "" || r.RelationType == "" {
			continue
		}
		id, err := local.UpsertRelation(ctx, c.DB, c.ProjectID, r.From, r.To, r.RelationType, c.DeviceID)
		if err != nil {
			return nil, err
		}
		fromID := local.EntityID(c.ProjectID, r.From)
		toID := local.EntityID(c.ProjectID, r.To)
		_, _ = local.Enqueue(ctx, c.DB, local.OpUpsertRelation, map[string]string{
			"ID": id, "ProjectID": c.ProjectID, "FromID": fromID, "ToID": toID, "RelationType": r.RelationType,
		})
		added++
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "add_relations", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"added": added})
}

// ---- delete_entities ----

func toolDeleteEntities() mcp.Tool {
	return mcp.NewTool("delete_entities",
		mcp.WithDescription("Soft-delete entities by name, hiding them and their observations/outgoing relations from reads."),
		mcp.WithArray("names", mcp.Required(),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
}

func handleDeleteEntities(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Names []string `json:"names"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Names) == 0 {
		return nil, fmt.Errorf("names is required and must be non-empty")
	}
	deleted := 0
	for _, name := range args.Names {
		id := local.EntityID(c.ProjectID, name)
		ok, err := local.DeleteEntity(ctx, c.DB, c.ProjectID, name, c.DeviceID)
		if err != nil {
			return nil, err
		}
		if ok {
			deleted++
			_, _ = local.Enqueue(ctx, c.DB, local.OpDeleteEntity, map[string]string{"ID": id})
		}
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "delete_entities", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"deleted": deleted})
}

// ---- delete_observations ----

func toolDeleteObservations() mcp.Tool {
	return mcp.NewTool("delete_observations",
		mcp.WithDescription("Remove specific observation contents from entities. Matches by exact content string."),
		mcp.WithArray("items", mcp.Required(),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entityName": map[string]any{"type": "string"},
					"contents":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"entityName", "contents"},
			}),
		),
	)
}

func handleDeleteObservations(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Items []obsAddItem `json:"items"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Items) == 0 {
		return nil, fmt.Errorf("items is required and must be non-empty")
	}
	type res struct {
		EntityName string `json:"entityName"`
		Count      int    `json:"count"`
	}
	out := []res{}
	for _, item := range args.Items {
		n, err := local.DeleteObservations(ctx, c.DB, c.ProjectID, item.EntityName, item.Contents, c.DeviceID)
		if err != nil {
			return nil, err
		}
		// Look up the affected observation IDs to enqueue ops. We do this
		// after the soft-delete by querying for the matching deleted rows.
		ids, err := lookupRecentlyDeletedObservations(ctx, c.DB, c.ProjectID, item.EntityName, item.Contents)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			_, _ = local.Enqueue(ctx, c.DB, local.OpDeleteObservation, map[string]string{"ID": id})
		}
		out = append(out, res{EntityName: item.EntityName, Count: n})
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "delete_observations", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"deleted": out})
}

func lookupRecentlyDeletedObservations(ctx context.Context, db *sql.DB, projectID, entityName string, contents []string) ([]string, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	placeholders := ""
	args := []any{projectID, entityName}
	for i, c := range contents {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, c)
	}
	q := fmt.Sprintf(`
		select o.id
		from observations o
		join entities e on e.id = o.entity_id
		where e.project_id = ? and e.name = ?
		  and o.deleted_at is not null
		  and o.sync_state = 'pending_push'
		  and o.content in (%s)
	`, placeholders)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- delete_relations ----

func toolDeleteRelations() mcp.Tool {
	return mcp.NewTool("delete_relations",
		mcp.WithDescription("Remove directed relations between entities."),
		mcp.WithArray("relations", mcp.Required(),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from":         map[string]any{"type": "string"},
					"to":           map[string]any{"type": "string"},
					"relationType": map[string]any{"type": "string"},
				},
				"required": []string{"from", "to", "relationType"},
			}),
		),
	)
}

func handleDeleteRelations(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Relations []relationInput `json:"relations"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	if len(args.Relations) == 0 {
		return nil, fmt.Errorf("relations is required and must be non-empty")
	}
	deleted := 0
	for _, r := range args.Relations {
		id := local.RelationID(c.ProjectID, r.From, r.To, r.RelationType)
		ok, err := local.DeleteRelation(ctx, c.DB, c.ProjectID, r.From, r.To, r.RelationType, c.DeviceID)
		if err != nil {
			return nil, err
		}
		if ok {
			deleted++
			_, _ = local.Enqueue(ctx, c.DB, local.OpDeleteRelation, map[string]string{"ID": id})
		}
	}
	_, _ = audit.Record(ctx, c.DB, c.ProjectID, c.DeviceID, "delete_relations", args)
	c.Sync.Trigger()
	return jsonResult(map[string]any{"deleted": deleted})
}

// ---- read_graph ----

func toolReadGraph() mcp.Tool {
	return mcp.NewTool("read_graph",
		mcp.WithDescription("Return the full project graph (entities, observations, relations) as JSON. Capped by `limit`."),
		mcp.WithNumber("limit", mcp.Description("Max entities; default 5000.")),
	)
}

func handleReadGraph(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := req.GetInt("limit", 5000)
	g, err := local.ReadGraph(ctx, c.DB, c.ProjectID, limit)
	if err != nil {
		return nil, err
	}
	return jsonResult(g)
}

// ---- search_nodes ----

func toolSearchNodes() mcp.Tool {
	return mcp.NewTool("search_nodes",
		mcp.WithDescription("Full-text search over observation content within this project. Returns ranked matches with their owning entity."),
		mcp.WithString("query", mcp.Required(), mcp.Description("FTS5 query (supports phrases, AND, OR, NEAR, prefix*).")),
		mcp.WithNumber("limit", mcp.Description("Max results; default 20.")),
	)
}

func handleSearchNodes(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q, err := req.RequireString("query")
	if err != nil {
		return nil, err
	}
	limit := req.GetInt("limit", 20)
	rs, err := local.SearchNodes(ctx, c.DB, c.ProjectID, q, limit)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{"results": rs})
}

// ---- open_nodes ----

func toolOpenNodes() mcp.Tool {
	return mcp.NewTool("open_nodes",
		mcp.WithDescription("Fetch specific entities by name with all their observations and outgoing relations."),
		mcp.WithArray("names", mcp.Required(),
			mcp.Items(map[string]any{"type": "string"}),
		),
	)
}

func handleOpenNodes(ctx context.Context, c *Ctx, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Names []string `json:"names"`
	}
	if err := req.BindArguments(&args); err != nil {
		return nil, err
	}
	ents, err := local.OpenNodes(ctx, c.DB, c.ProjectID, args.Names)
	if err != nil {
		return nil, err
	}
	return jsonResult(ents)
}
