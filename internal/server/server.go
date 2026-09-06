// Package server exposes a HomeBox instance as MCP tools.
//
// Reads and writes. Every tool carries MCP annotations so a client can tell
// them apart before calling: readOnlyHint on reads, destructiveHint on
// anything that modifies or removes, idempotentHint where repeating a call
// changes nothing further. A client that surfaces those can ask before it
// deletes; one that ignores them at least had the chance.
//
// What is deliberately NOT exposed, and why:
//
//   - /actions/*            including wipe-inventory, which empties the
//     database. No phrasing of a user's request should
//     be one tool call away from that.
//   - /users/* auth         login, register, password reset, and the API-key
//     endpoints. A server authenticated by one key has
//     no business minting or listing others.
//   - /groups membership    invitations and member removal are account
//     administration, not inventory.
//   - exports and imports   bulk data movement, in and out. The blast radius
//     does not fit a tool call.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/excavador/homebox-mcp/internal/homebox"
)

func ptr[T any](v T) *T { return &v }

// Annotations, by what a tool does to the inventory.
func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

// creates add; they are not destructive, and calling twice makes two things.
func creates() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: false}
}

// updates overwrite what was there, which is destructive in the MCP sense,
// but repeating one changes nothing further.
func updates() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}
}

func deletes() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true}
}

type idArgs struct {
	ID string `json:"id" jsonschema:"the UUID"`
}

type bodyArgs struct {
	ID   string         `json:"id" jsonschema:"the UUID"`
	Body map[string]any `json:"body" jsonschema:"the object fields to write, in HomeBox's own shape"`
}

type createArgs struct {
	Body map[string]any `json:"body" jsonschema:"the object to create, in HomeBox's own shape"`
}

// New builds the MCP server and registers every tool.
func New(c *homebox.Client, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "homebox", Version: version}, nil)

	addEntities(s, c)
	addTags(s, c)
	addEntityTypes(s, c)
	addTemplates(s, c)
	addMaintenance(s, c)
	addAttachments(s, c)
	addLookups(s, c)
	addConfiguration(s, c)

	return s
}

func addEntities(s *mcp.Server, c *homebox.Client) {
	type listArgs struct {
		Query     string   `json:"query,omitempty" jsonschema:"free-text search over names and descriptions"`
		Page      int      `json:"page,omitempty" jsonschema:"1-based page number"`
		PageSize  int      `json:"pageSize,omitempty" jsonschema:"results per page; HomeBox caps this itself"`
		Tags      []string `json:"tags,omitempty" jsonschema:"only entities carrying ALL of these tag ids"`
		ParentIDs []string `json:"parentIds,omitempty" jsonschema:"only entities directly inside these parent ids"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_list_entities",
		// ITEMS, not "entities": locations do not come back here. Saying
		// otherwise makes a model report an empty house.
		Description: "List or search ITEMS in the inventory. Locations are NOT returned -- use homebox_entity_tree for those.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a listArgs) (*mcp.CallToolResult, any, error) {
		q := url.Values{}
		if a.Query != "" {
			q.Set("q", a.Query)
		}
		if a.Page > 0 {
			q.Set("page", strconv.Itoa(a.Page))
		}
		if a.PageSize > 0 {
			q.Set("pageSize", strconv.Itoa(a.PageSize))
		}
		for _, t := range a.Tags {
			q.Add("tags", t)
		}
		for _, p := range a.ParentIDs {
			q.Add("parentIds", p)
		}

		return call(ctx, c, http.MethodGet, "/entities", q, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_get_entity", Description: "One entity in full, by id.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodGet, "/entities/", a.ID, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_create_entity",
		Description: "Create an item or location. The body is HomeBox's own shape: name, entityTypeId, parentId, description, manufacturer, modelNumber, serialNumber, notes, quantity, purchasePrice, fields, tagIds and so on. The whole body is applied.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a createArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		created, err := c.Call(ctx, http.MethodPost, "/entities", nil, a.Body)
		if err != nil {
			return nil, nil, err
		}

		obj, ok := created.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("create returned %T, want an object", created)
		}

		// HomeBox's create accepts only name, entityTypeId and parentId; every
		// other field in the body is dropped, and it still answers 201. Nothing
		// about the response says so. Apply the rest with the PUT that HomeBox
		// does honour, so the tool does what its description promises.
		if !needsSecondWrite(a.Body) {
			return nil, obj, nil
		}

		id, _ := obj["id"].(string)
		if id == "" {
			return nil, obj, nil
		}

		full := mergeInto(obj, a.Body)

		// The POST already set the parent; a PUT that omits it would clear it.
		if pid, ok := a.Body["parentId"]; ok {
			full["parentId"] = pid
		}

		return call(ctx, c, http.MethodPut, "/entities/"+url.PathEscape(id), nil, full)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_entity",
		Description: "REPLACE an entity: fields omitted from the body are cleared, so send the whole object -- " +
			"read it first with homebox_get_entity. The parent is carried over unless the body sets parentId, " +
			"so a replace does not move the entity by accident. To change only a few fields, use homebox_patch_entity.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		body, err := withRelations(ctx, c, a.ID, a.Body)
		if err != nil {
			return nil, nil, err
		}

		return call(ctx, c, http.MethodPut, "/entities/"+url.PathEscape(a.ID), nil, body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_patch_entity",
		Description: "Change SOME fields of an entity, leaving the rest -- including its parent -- alone. " +
			"Prefer this over homebox_update_entity unless a full replace is intended.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		// HomeBox answers PATCH /entities/{id} with 200 and the unchanged
		// entity: the request is accepted and nothing is written. Read, merge
		// and PUT instead, which is what a caller means by a partial update.
		current, err := c.Call(ctx, http.MethodGet, "/entities/"+url.PathEscape(a.ID), nil, nil)
		if err != nil {
			return nil, nil, err
		}

		obj, ok := current.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("entity %s returned %T, want an object", a.ID, current)
		}

		body, err := withRelations(ctx, c, a.ID, mergeInto(obj, a.Body))
		if err != nil {
			return nil, nil, err
		}

		return call(ctx, c, http.MethodPut, "/entities/"+url.PathEscape(a.ID), nil, body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_delete_entity",
		Description: "DELETE an entity permanently. Deleting a location removes what it contains.",
		Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodDelete, "/entities/", a.ID, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_duplicate_entity", Description: "Copy an entity, returning the new one.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		// HomeBox decodes the request body unconditionally here and 500s when it is
		// absent, so send an empty object rather than no body at all.
		return call(ctx, c, http.MethodPost, "/entities/"+url.PathEscape(a.ID)+"/duplicate", nil, map[string]any{})
	})
}

func addTags(s *mcp.Server, c *homebox.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_list_tags", Description: "All tags. Tags replaced what older HomeBox called labels.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return call(ctx, c, http.MethodGet, "/tags", nil, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_get_tag", Description: "One tag by id.", Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodGet, "/tags/", a.ID, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_create_tag", Description: "Create a tag: name, description, colour, icon.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a createArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		return call(ctx, c, http.MethodPost, "/tags", nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_tag", Description: "Replace a tag.", Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodPut, "/tags/", a.ID, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_delete_tag",
		Description: "DELETE a tag. It is removed from every entity carrying it.",
		Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodDelete, "/tags/", a.ID, nil)
	})
}

func addEntityTypes(s *mcp.Server, c *homebox.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_entity_types",
		Description: "The entity types, which are what distinguish an item from a location.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return call(ctx, c, http.MethodGet, "/entity-types", nil, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_create_entity_type", Description: "Create an entity type.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a createArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		return call(ctx, c, http.MethodPost, "/entity-types", nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_entity_type", Description: "Replace an entity type.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodPut, "/entity-types/", a.ID, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_delete_entity_type",
		Description: "DELETE an entity type. Entities using it are affected.",
		Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodDelete, "/entity-types/", a.ID, nil)
	})
}

func addTemplates(s *mcp.Server, c *homebox.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_list_templates", Description: "Entity templates.", Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return call(ctx, c, http.MethodGet, "/templates", nil, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_get_template", Description: "One template by id.", Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodGet, "/templates/", a.ID, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_create_template", Description: "Create a template.", Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a createArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Body) == 0 {
			return nil, nil, fmt.Errorf("body is required")
		}

		return call(ctx, c, http.MethodPost, "/templates", nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_template", Description: "Replace a template.", Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodPut, "/templates/", a.ID, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_delete_template", Description: "DELETE a template.", Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodDelete, "/templates/", a.ID, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_create_item_from_template", Description: "Create an item from a template.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		return call(ctx, c, http.MethodPost, "/templates/"+url.PathEscape(a.ID)+"/create-item", nil, a.Body)
	})
}

func addMaintenance(s *mcp.Server, c *homebox.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_maintenance",
		Description: "Maintenance records across the inventory, or for one entity when an id is given.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		if a.ID != "" {
			return call(ctx, c, http.MethodGet, "/entities/"+url.PathEscape(a.ID)+"/maintenance", nil, nil)
		}

		return call(ctx, c, http.MethodGet, "/maintenance", nil, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_create_maintenance", Description: "Record maintenance against an entity.",
		Annotations: creates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required (the entity)")
		}

		return call(ctx, c, http.MethodPost, "/entities/"+url.PathEscape(a.ID)+"/maintenance", nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_maintenance", Description: "Replace a maintenance record.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a bodyArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodPut, "/maintenance/", a.ID, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_delete_maintenance", Description: "DELETE a maintenance record.",
		Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		return byID(ctx, c, http.MethodDelete, "/maintenance/", a.ID, nil)
	})
}

func addAttachments(s *mcp.Server, c *homebox.Client) {
	type attArgs struct {
		ID           string         `json:"id" jsonschema:"the entity UUID"`
		AttachmentID string         `json:"attachmentId,omitempty" jsonschema:"the attachment UUID"`
		Body         map[string]any `json:"body,omitempty" jsonschema:"fields to write"`
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_get_attachment",
		Description: "One attachment's metadata. Binary uploads are not supported by this server -- use the HomeBox UI.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a attArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" || a.AttachmentID == "" {
			return nil, nil, fmt.Errorf("id and attachmentId are required")
		}

		return call(ctx, c, http.MethodGet,
			"/entities/"+url.PathEscape(a.ID)+"/attachments/"+url.PathEscape(a.AttachmentID), nil, nil)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_update_attachment", Description: "Rename or re-type an attachment.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a attArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" || a.AttachmentID == "" {
			return nil, nil, fmt.Errorf("id and attachmentId are required")
		}

		return call(ctx, c, http.MethodPut,
			"/entities/"+url.PathEscape(a.ID)+"/attachments/"+url.PathEscape(a.AttachmentID), nil, a.Body)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_delete_attachment", Description: "DELETE an attachment.", Annotations: deletes(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a attArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" || a.AttachmentID == "" {
			return nil, nil, fmt.Errorf("id and attachmentId are required")
		}

		return call(ctx, c, http.MethodDelete,
			"/entities/"+url.PathEscape(a.ID)+"/attachments/"+url.PathEscape(a.AttachmentID), nil, nil)
	})
}

func addLookups(s *mcp.Server, c *homebox.Client) {
	simple := []struct{ name, path, desc string }{
		{"homebox_entity_tree", "/entities/tree", "How entities nest inside one another. This is where LOCATIONS are."},
		{"homebox_custom_fields", "/entities/fields", "Custom field names defined on entities."},
		{"homebox_currencies", "/currencies", "Currencies HomeBox knows, with code, symbol and decimal places."},
	}
	for _, t := range simple {
		path, desc := t.path, t.desc
		mcp.AddTool(s, &mcp.Tool{Name: t.name, Description: desc, Annotations: readOnly()},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
				return call(ctx, c, http.MethodGet, path, nil, nil)
			})
	}

	// This one answers text/csv, not JSON, so it cannot go through call().
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_bill_of_materials",
		Description: "The bill-of-materials report, as CSV: one row per entity with quantity and price.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		csv, err := c.Text(ctx, "/reporting/bill-of-materials", nil)
		if err != nil {
			return nil, nil, err
		}

		return nil, map[string]any{"csv": csv}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_entity_path", Description: "An entity's ancestors, root first.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a idArgs) (*mcp.CallToolResult, any, error) {
		if a.ID == "" {
			return nil, nil, fmt.Errorf("id is required")
		}

		return call(ctx, c, http.MethodGet, "/entities/"+url.PathEscape(a.ID)+"/path", nil, nil)
	})

	type fieldArgs struct {
		Field string `json:"field" jsonschema:"the custom field name, from homebox_custom_fields"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_custom_field_values",
		Description: "The values in use for one custom field, so a query filters on a real value rather than a guess.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a fieldArgs) (*mcp.CallToolResult, any, error) {
		if a.Field == "" {
			return nil, nil, fmt.Errorf("field is required")
		}

		return call(ctx, c, http.MethodGet, "/entities/fields/values", url.Values{"field": {a.Field}}, nil)
	})

	type assetArgs struct {
		AssetID string `json:"assetId" jsonschema:"the printed asset id, e.g. 000-001"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_get_asset", Description: "Look an entity up by printed asset id rather than UUID.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a assetArgs) (*mcp.CallToolResult, any, error) {
		if a.AssetID == "" {
			return nil, nil, fmt.Errorf("assetId is required")
		}

		return call(ctx, c, http.MethodGet, "/assets/"+url.PathEscape(a.AssetID), nil, nil)
	})

	type barcodeArgs struct {
		Barcode string `json:"barcode" jsonschema:"the scanned barcode"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_search_barcode",
		Description: "Look a product up from a barcode. Queries an EXTERNAL product database, not your inventory.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a barcodeArgs) (*mcp.CallToolResult, any, error) {
		if a.Barcode == "" {
			return nil, nil, fmt.Errorf("barcode is required")
		}

		return call(ctx, c, http.MethodGet, "/products/search-from-barcode",
			url.Values{"data": {a.Barcode}}, nil)
	})

	type statsArgs struct {
		Group string `json:"group,omitempty" jsonschema:"one of: locations, tags, purchase-price; omit for overall totals"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_statistics",
		Description: "Inventory statistics: totals overall, or by location, tag or purchase price.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a statsArgs) (*mcp.CallToolResult, any, error) {
		path := "/groups/statistics"

		switch a.Group {
		case "":
		case "locations", "tags", "purchase-price":
			path += "/" + a.Group
		default:
			// Named explicitly: an arbitrary value would become a path segment.
			return nil, nil, fmt.Errorf("group must be one of locations, tags, purchase-price")
		}

		return call(ctx, c, http.MethodGet, path, nil, nil)
	})
}

// addConfiguration exposes settings as ONE read and ONE write, rather than a
// tool per knob.
//
// Configuration is read far more often than it is changed, and a model
// choosing between eight near-identical setting tools is a worse outcome
// than one that returns the lot. The write takes the same shape back, so a
// round trip is read, edit, write.
func addConfiguration(s *mcp.Server, c *homebox.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "homebox_get_configuration",
		Description: "All configuration in one object: group settings, this user's preferences, and the notifiers.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		out := map[string]any{}

		// Each part is fetched independently and reported even if another
		// fails: a missing notifier permission should not hide the group
		// settings that did come back.
		for key, path := range map[string]string{
			"group":     "/groups",
			"user":      "/users/self/settings",
			"notifiers": "/notifiers",
		} {
			v, err := c.Call(ctx, http.MethodGet, path, nil, nil)
			if err != nil {
				out[key] = map[string]any{"error": err.Error()}
				continue
			}

			out[key] = v
		}

		return nil, out, nil
	})

	type cfgArgs struct {
		Group map[string]any `json:"group,omitempty" jsonschema:"group settings to write, from homebox_get_configuration"`
		User  map[string]any `json:"user,omitempty" jsonschema:"this user's preferences to write"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "homebox_set_configuration",
		// Notifiers are absent on purpose: each is a webhook URL, and
		// creating or editing one is arranging for HomeBox to send data
		// somewhere. That belongs to a person at a screen.
		Description: "Write configuration. Send only the sections to change, in the shape homebox_get_configuration returned. Notifiers cannot be changed here.",
		Annotations: updates(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a cfgArgs) (*mcp.CallToolResult, any, error) {
		if len(a.Group) == 0 && len(a.User) == 0 {
			return nil, nil, fmt.Errorf("nothing to write: provide group, user, or both")
		}

		out := map[string]any{}

		if len(a.Group) > 0 {
			v, err := c.Call(ctx, http.MethodPut, "/groups", nil, a.Group)
			if err != nil {
				return nil, nil, fmt.Errorf("group settings: %w", err)
			}

			out["group"] = v
		}

		if len(a.User) > 0 {
			v, err := c.Call(ctx, http.MethodPut, "/users/self/settings", nil, a.User)
			if err != nil {
				return nil, nil, fmt.Errorf("user settings: %w", err)
			}

			out["user"] = v
		}

		return nil, out, nil
	})
}

// byID is the shape most tools share: require an id, then act on it.
func byID(ctx context.Context, c *homebox.Client, method, prefix, id string, body any) (*mcp.CallToolResult, any, error) {
	if id == "" {
		return nil, nil, fmt.Errorf("id is required")
	}

	return call(ctx, c, method, prefix+url.PathEscape(id), nil, body)
}

// call performs the request and returns the decoded JSON as structured
// content, so the client gets real data rather than a string it must parse.
//
// MCP requires structuredContent to be an OBJECT. Several HomeBox endpoints
// answer with a top-level array -- /entities/tree, /tags, /entities/{id}/path
// and /entities/fields all do -- and returning one straight through makes the
// client reject the whole response as malformed, with an error that names
// schema validation rather than the tool.
//
// So a non-object is wrapped. The key is "items" because that is what HomeBox
// itself calls the array inside its paged responses, and a caller reading one
// shape should not have to learn a second.
func call(ctx context.Context, c *homebox.Client, method, path string, q url.Values, body any) (*mcp.CallToolResult, any, error) {
	out, err := c.Call(ctx, method, path, q, body)
	if err != nil {
		return nil, nil, err
	}

	if _, ok := out.(map[string]any); !ok {
		return nil, map[string]any{"items": out}, nil
	}

	return nil, out, nil
}

// createHonours lists the only fields HomeBox applies on POST /entities.
// Anything else in the body is discarded, silently and with a 201.
var createHonours = map[string]bool{"name": true, "entityTypeId": true, "parentId": true}

// needsSecondWrite reports whether the body carries anything create would drop.
func needsSecondWrite(body map[string]any) bool {
	for k := range body {
		if !createHonours[k] {
			return true
		}
	}

	return false
}

// mergeInto overlays patch onto a copy of base. base is never modified.
func mergeInto(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}

	for k, v := range patch {
		out[k] = v
	}

	return out
}

// relations are the fields HomeBox returns as objects when you read an entity
// but accepts as ids when you write one. A PUT that omits the id form clears
// the relation, so a read-modify-write silently drops it.
//
// This is not theoretical: it unparented rooms and stripped the tags off
// twenty-two items during a bulk load, and every call returned success.
var relations = []struct{ read, write string }{
	{"parent", "parentId"},
	{"entityType", "entityTypeId"},
}

// withRelations fills in the id form of every relation the caller did not set,
// taking it from the body if it carries the read shape and otherwise from the
// entity itself. An explicit id always wins, so deliberate moves still work.
func withRelations(ctx context.Context, c *homebox.Client, id string, body map[string]any) (map[string]any, error) {
	out := mergeInto(body, nil)

	var current map[string]any

	// look returns the read-shaped value, preferring what the caller sent.
	look := func(key string) (any, error) {
		if v, ok := out[key]; ok {
			return v, nil
		}

		if current == nil {
			got, err := c.Call(ctx, http.MethodGet, "/entities/"+url.PathEscape(id), nil, nil)
			if err != nil {
				return nil, err
			}

			m, ok := got.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("entity %s returned %T, want an object", id, got)
			}

			current = m
		}

		return current[key], nil
	}

	for _, r := range relations {
		if _, ok := out[r.write]; ok {
			continue
		}

		v, err := look(r.read)
		if err != nil {
			return nil, err
		}

		if m, ok := v.(map[string]any); ok {
			if rid, ok := m["id"].(string); ok && rid != "" {
				out[r.write] = rid
			}
		}
	}

	// Tags are the same problem one level out: a list of objects on read, a
	// list of ids on write.
	if _, ok := out["tagIds"]; !ok {
		v, err := look("tags")
		if err != nil {
			return nil, err
		}

		if arr, ok := v.([]any); ok && len(arr) > 0 {
			ids := make([]string, 0, len(arr))

			for _, t := range arr {
				m, ok := t.(map[string]any)
				if !ok {
					continue
				}

				if tid, ok := m["id"].(string); ok && tid != "" {
					ids = append(ids, tid)
				}
			}

			if len(ids) > 0 {
				out["tagIds"] = ids
			}
		}
	}

	return out, nil
}
