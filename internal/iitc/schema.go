package iitc

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// CreateCollections creates the store's collections if they don't exist yet.
// Production schemas are made by hand from docs/iitc-plugins.md like every
// other collection here; this exists so tests and a fresh local pb_data get
// exactly the same shape (go run . iitc-setup). It never alters a collection
// that is already there.
func CreateCollections(app core.App) error {
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}

	plugins, err := app.FindCollectionByNameOrId(PluginsCollection)
	if err != nil {
		plugins = core.NewBaseCollection(PluginsCollection)
		plugins.ListRule = types.Pointer("author = @request.auth.id")
		plugins.ViewRule = types.Pointer("author = @request.auth.id")
		plugins.CreateRule = types.Pointer(`@request.auth.id != "" && @request.auth.collectionName = "users"`)
		plugins.UpdateRule = types.Pointer("author = @request.auth.id")
		plugins.DeleteRule = types.Pointer("author = @request.auth.id")
		plugins.Fields.Add(
			&core.TextField{Name: "slug", Required: true, Min: 3, Max: 40, Pattern: slugPattern.String()},
			&core.TextField{Name: "name", Required: true, Max: 80},
			&core.TextField{Name: "description", Max: 20000},
			&core.TextField{Name: "category", Max: 40},
			&core.URLField{Name: "homepage_url"},
			&core.FileField{Name: "icon", MaxSelect: 1, MaxSize: 256 << 10, MimeTypes: []string{"image/png", "image/jpeg", "image/svg+xml", "image/webp"}},
			&core.RelationField{Name: "author", Required: true, CollectionId: users.Id, MaxSelect: 1, CascadeDelete: true},
			&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{StatusDraft, StatusPendingReview, StatusApproved, StatusRejected, StatusDisabled}},
			&core.JSONField{Name: "access", MaxSize: 64 << 10},
			&core.TextField{Name: "review_notes", Max: 5000},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		plugins.AddIndex("idx_iitc_plugins_slug", true, "slug", "")
		if err := app.Save(plugins); err != nil {
			return err
		}
	}

	versions, err := app.FindCollectionByNameOrId(VersionsCollection)
	if err != nil {
		versions = core.NewBaseCollection(VersionsCollection)
		versions.ListRule = types.Pointer("plugin.author = @request.auth.id")
		versions.ViewRule = types.Pointer("plugin.author = @request.auth.id")
		versions.CreateRule = types.Pointer("plugin.author = @request.auth.id")
		versions.Fields.Add(
			&core.RelationField{Name: "plugin", Required: true, CollectionId: plugins.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "version", Required: true, Max: 40, Pattern: versionPattern.String()},
			&core.TextField{Name: "changelog", Max: 5000},
			&core.FileField{Name: "file", Required: true, MaxSelect: 1, MaxSize: MaxPluginBytes, Protected: true},
			&core.TextField{Name: "sha256", Max: 64},
			&core.SelectField{Name: "status", Required: true, MaxSelect: 1, Values: []string{VersionPending, VersionApproved, VersionRejected}},
			&core.TextField{Name: "review_notes", Max: 5000},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		versions.AddIndex("idx_iitc_versions_plugin_version", true, "plugin, version", "")
		if err := app.Save(versions); err != nil {
			return err
		}
	}

	// current_version points back at the versions collection, so it can only
	// be added once that collection exists.
	if plugins.Fields.GetByName("current_version") == nil {
		plugins.Fields.Add(&core.RelationField{Name: "current_version", CollectionId: versions.Id, MaxSelect: 1})
		if err := app.Save(plugins); err != nil {
			return err
		}
	}

	if _, err := app.FindCollectionByNameOrId(TokensCollection); err != nil {
		tokens := core.NewBaseCollection(TokensCollection)
		tokens.ListRule = types.Pointer("user = @request.auth.id")
		tokens.ViewRule = types.Pointer("user = @request.auth.id")
		tokens.DeleteRule = types.Pointer("user = @request.auth.id")
		tokens.Fields.Add(
			&core.RelationField{Name: "user", Required: true, CollectionId: users.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "label", Max: maxLabelLength},
			&core.TextField{Name: "token_hash", Required: true, Max: 64, Hidden: true},
			&core.DateField{Name: "last_used_at"},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		tokens.AddIndex("idx_iitc_tokens_hash", true, "token_hash", "")
		if err := app.Save(tokens); err != nil {
			return err
		}
	}

	return nil
}
