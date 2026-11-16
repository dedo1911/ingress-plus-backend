package iitc

import (
	"fmt"
	"io"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// PluginSummary is one entry of the store and sideloader catalogs. It never
// carries code, and access rules only appear for the plugin's own author.
type PluginSummary struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	HomepageURL string `json:"homepageUrl"`
	Icon        string `json:"icon"`
	Author      string `json:"author"`
	Version     string `json:"version"`
	Changelog   string `json:"changelog"`
	Updated     string `json:"updated"`

	// Author-only fields.
	IsAuthor   bool    `json:"isAuthor"`
	ID         string  `json:"id,omitempty"`
	Status     string  `json:"status,omitempty"`
	DevVersion string  `json:"devVersion,omitempty"`
	Access     *Access `json:"access,omitempty"`
}

// VersionInfo is a version as shown on the plugin detail page.
type VersionInfo struct {
	Version     string `json:"version"`
	Changelog   string `json:"changelog"`
	Created     string `json:"created"`
	Status      string `json:"status,omitempty"`
	ReviewNotes string `json:"reviewNotes,omitempty"`
}

// PluginDetail is the plugin detail page payload.
type PluginDetail struct {
	PluginSummary
	ReviewNotes string        `json:"reviewNotes,omitempty"`
	Versions    []VersionInfo `json:"versions"`
}

func iconURL(plugin *core.Record) string {
	icon := plugin.GetString("icon")
	if icon == "" {
		return ""
	}
	return fmt.Sprintf("/api/files/%s/%s/%s", plugin.Collection().Id, plugin.Id, icon)
}

func summarize(app core.App, plugin, user *core.Record) PluginSummary {
	s := PluginSummary{
		Slug:        plugin.GetString("slug"),
		Name:        plugin.GetString("name"),
		Description: plugin.GetString("description"),
		Category:    plugin.GetString("category"),
		HomepageURL: plugin.GetString("homepage_url"),
		Icon:        iconURL(plugin),
		Updated:     plugin.GetDateTime("updated").String(),
	}
	if author := plugin.ExpandedOne("author"); author != nil {
		s.Author = author.GetString("username")
	}
	if current := plugin.ExpandedOne("current_version"); current != nil {
		s.Version = current.GetString("version")
		s.Changelog = current.GetString("changelog")
	}

	if IsAuthor(user, plugin) {
		s.IsAuthor = true
		s.ID = plugin.Id
		s.Status = plugin.GetString("status")
		if access, err := ParseAccess(plugin.GetString("access")); err == nil {
			s.Access = &access
		}
		if pending, err := app.FindRecordsByFilter(VersionsCollection, "plugin = {:p} && status = {:s}", "-created", 1, 0,
			dbx.Params{"p": plugin.Id, "s": VersionPending}); err == nil && len(pending) > 0 {
			s.DevVersion = pending[0].GetString("version")
		}
	}
	return s
}

// ListAccessible returns every plugin user may see, sorted by name: the
// published plugins their access rules grant, plus their own.
func ListAccessible(app core.App, user *core.Record) ([]PluginSummary, error) {
	records, err := app.FindRecordsByFilter(PluginsCollection,
		"(status = {:approved} && current_version != '') || author = {:uid}", "name", 0, 0,
		dbx.Params{"approved": StatusApproved, "uid": user.Id})
	if err != nil {
		return nil, err
	}

	visible := records[:0]
	for _, r := range records {
		if CanAccess(user, r) {
			visible = append(visible, r)
		}
	}
	if errs := app.ExpandRecords(visible, []string{"author", "current_version"}, nil); len(errs) > 0 {
		app.Logger().Warn("iitc: expanding plugin catalog", "errors", errs)
	}

	out := make([]PluginSummary, 0, len(visible))
	for _, r := range visible {
		out = append(out, summarize(app, r, user))
	}
	return out, nil
}

// FindAccessible looks a plugin up by slug, returning ErrNotFound both when
// it doesn't exist and when user can't access it.
func FindAccessible(app core.App, user *core.Record, slug string) (*core.Record, error) {
	if !ValidSlug(slug) {
		return nil, ErrNotFound
	}
	plugin, err := app.FindFirstRecordByData(PluginsCollection, "slug", slug)
	if err != nil || !CanAccess(user, plugin) {
		return nil, ErrNotFound
	}
	if errs := app.ExpandRecord(plugin, []string{"author", "current_version"}, nil); len(errs) > 0 {
		app.Logger().Warn("iitc: expanding plugin", "errors", errs)
	}
	return plugin, nil
}

// Detail builds the plugin detail payload. Other agents see the approved
// version history; the author also sees pending and rejected uploads with
// their review notes.
func Detail(app core.App, plugin, user *core.Record) (PluginDetail, error) {
	d := PluginDetail{PluginSummary: summarize(app, plugin, user), Versions: []VersionInfo{}}
	author := IsAuthor(user, plugin)

	filter := "plugin = {:p} && status = {:s}"
	if author {
		filter = "plugin = {:p}"
		d.ReviewNotes = plugin.GetString("review_notes")
	}
	versions, err := app.FindRecordsByFilter(VersionsCollection, filter, "-created", 50, 0,
		dbx.Params{"p": plugin.Id, "s": VersionApproved})
	if err != nil {
		return d, err
	}
	for _, v := range versions {
		info := VersionInfo{
			Version:   v.GetString("version"),
			Changelog: v.GetString("changelog"),
			Created:   v.GetDateTime("created").String(),
		}
		if author {
			info.Status = v.GetString("status")
			info.ReviewNotes = v.GetString("review_notes")
		}
		d.Versions = append(d.Versions, info)
	}
	return d, nil
}

// ResolveVersion picks the version record to serve. Without an explicit
// version that's the published one, falling back for the author alone to
// their newest pending upload so they can try it before review. An explicit
// version other than the published one is only served to the author.
func ResolveVersion(app core.App, plugin, user *core.Record, requested string) (*core.Record, error) {
	current := plugin.GetString("current_version")
	author := IsAuthor(user, plugin)

	if requested == "" {
		if current != "" {
			return app.FindRecordById(VersionsCollection, current)
		}
		if !author {
			return nil, ErrNotFound
		}
		pending, err := app.FindRecordsByFilter(VersionsCollection, "plugin = {:p} && status = {:s}", "-created", 1, 0,
			dbx.Params{"p": plugin.Id, "s": VersionPending})
		if err != nil || len(pending) == 0 {
			return nil, ErrNotFound
		}
		return pending[0], nil
	}

	version, err := app.FindFirstRecordByFilter(VersionsCollection, "plugin = {:p} && version = {:v}",
		dbx.Params{"p": plugin.Id, "v": requested})
	if err != nil {
		return nil, ErrNotFound
	}
	if version.Id != current && !author {
		return nil, ErrNotFound
	}
	return version, nil
}

// ReadCode reads a version's file from storage.
func ReadCode(app core.App, version *core.Record) ([]byte, error) {
	fsys, err := app.NewFilesystem()
	if err != nil {
		return nil, err
	}
	defer fsys.Close()

	r, err := fsys.GetReader(version.BaseFilesPath() + "/" + version.GetString("file"))
	if err != nil {
		return nil, err
	}
	defer r.Close()

	return io.ReadAll(io.LimitReader(r, MaxPluginBytes+1))
}
