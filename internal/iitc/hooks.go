package iitc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/dedo1911/ingress-plus-backend/internal/notify"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

func badRequest(msg string) error { return router.NewBadRequestError(msg, nil) }

func forbidden(msg string) error { return router.NewForbiddenError(msg, nil) }

// RegisterHooks wires the store's REST guards and the approval flow.
//
// Authors manage their plugins and upload versions through the normal
// PocketBase record API; the collection rules limit them to their own
// records, and these hooks limit what they can change on them. Review is a
// superuser editing a version's status, from /admin or the dashboard.
func RegisterHooks(app core.App, t *notify.Telegram) {
	app.OnRecordCreateRequest(PluginsCollection).BindFunc(guardPluginCreate)
	app.OnRecordUpdateRequest(PluginsCollection).BindFunc(guardPluginUpdate)
	app.OnRecordCreateRequest(VersionsCollection).BindFunc(guardVersionCreate)
	app.OnRecordUpdateRequest(VersionsCollection).BindFunc(guardVersionUpdate)

	app.OnRecordAfterCreateSuccess(VersionsCollection).BindFunc(func(e *core.RecordEvent) error {
		if err := syncPluginWithVersion(e.App, e.Record); err != nil {
			e.App.Logger().Error("iitc: syncing plugin after version create", "error", err, "version", e.Record.Id)
		}
		if e.Record.GetString("status") == VersionPending {
			t.SendAsync(e.App.Logger(), t.Topics.Plugins, Message(e.App, "IITC plugin version to review", e.Record))
		}
		return e.Next()
	})

	app.OnRecordAfterUpdateSuccess(VersionsCollection).BindFunc(func(e *core.RecordEvent) error {
		oldStatus := e.Record.Original().GetString("status")
		newStatus := e.Record.GetString("status")
		if oldStatus == newStatus {
			return e.Next()
		}
		if err := syncPluginWithVersion(e.App, e.Record); err != nil {
			e.App.Logger().Error("iitc: syncing plugin after version review", "error", err, "version", e.Record.Id)
		}
		t.SendAsync(e.App.Logger(), t.Topics.Plugins, Message(e.App, "IITC plugin version "+newStatus, e.Record))
		return e.Next()
	})
}

// Message formats a Telegram notification about a version.
func Message(app core.App, label string, version *core.Record) string {
	name := version.GetString("plugin")
	if plugin, err := app.FindRecordById(PluginsCollection, version.GetString("plugin")); err == nil {
		name = plugin.GetString("name") + " (" + plugin.GetString("slug") + ")"
	}
	return notify.Message(label, name+" "+version.GetString("version"))
}

func isUserAuth(e *core.RecordRequestEvent) bool {
	return e.Auth != nil && e.Auth.Collection().Name == "users"
}

func guardPluginCreate(e *core.RecordRequestEvent) error {
	if !e.HasSuperuserAuth() {
		if !isUserAuth(e) {
			return forbidden("Sign in to publish a plugin.")
		}
		e.Record.Set("author", e.Auth.Id)
		e.Record.Set("status", StatusDraft)
		e.Record.Set("current_version", "")
		e.Record.Set("review_notes", "")
	} else if e.Record.GetString("status") == "" {
		e.Record.Set("status", StatusDraft)
	}

	slug := strings.ToLower(strings.TrimSpace(e.Record.GetString("slug")))
	if !ValidSlug(slug) {
		return badRequest("The plugin id must be 3-40 lowercase letters, digits or dashes, and can't start or end with a dash.")
	}
	e.Record.Set("slug", slug)

	if err := ValidateAccess(e.Record.GetString("access")); err != nil {
		return badRequest(err.Error())
	}
	return e.Next()
}

// CanAuthorChangeStatus reports which plugin status changes an author can
// make themselves. Everything else happens through review:
//   - approved -> draft unpublishes the plugin without losing its versions
//   - draft -> approved republishes it, but only if a version was approved
//     before (current_version is set); a never-reviewed plugin can't get
//     there on its own
//
// Nothing leaves "disabled": that is an admin takedown.
func CanAuthorChangeStatus(oldStatus, newStatus string, hasApprovedVersion bool) bool {
	if oldStatus == newStatus {
		return true
	}
	switch {
	case oldStatus == StatusApproved && newStatus == StatusDraft:
		return true
	case oldStatus == StatusDraft && newStatus == StatusApproved:
		return hasApprovedVersion
	}
	return false
}

func guardPluginUpdate(e *core.RecordRequestEvent) error {
	original := e.Record.Original()

	// The slug is what the sideloader remembers enabled plugins by, so
	// changing it would silently switch the plugin off on every device.
	if e.Record.GetString("slug") != original.GetString("slug") {
		return badRequest("The plugin id can't be changed.")
	}

	if !e.HasSuperuserAuth() {
		for _, field := range []string{"author", "current_version", "review_notes"} {
			if e.Record.GetString(field) != original.GetString(field) {
				return forbidden(fmt.Sprintf("%q can't be changed.", field))
			}
		}
		if !CanAuthorChangeStatus(original.GetString("status"), e.Record.GetString("status"), original.GetString("current_version") != "") {
			return forbidden(fmt.Sprintf("You can't move this plugin from %q to %q.", original.GetString("status"), e.Record.GetString("status")))
		}
	}

	if err := ValidateAccess(e.Record.GetString("access")); err != nil {
		return badRequest(err.Error())
	}
	return e.Next()
}

// readUpload returns the content of the single file being uploaded to field.
func readUpload(record *core.Record, field string) ([]byte, error) {
	files := record.GetUploadedFiles(field)
	if len(files) != 1 {
		return nil, badRequest("Upload exactly one .js file.")
	}
	file := files[0]
	if !strings.EqualFold(filepath.Ext(file.OriginalName), ".js") {
		return nil, badRequest("The plugin must be a .js file.")
	}
	rc, err := file.Reader.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	code, err := io.ReadAll(io.LimitReader(rc, MaxPluginBytes+1))
	if err != nil {
		return nil, err
	}
	return code, nil
}

func guardVersionCreate(e *core.RecordRequestEvent) error {
	plugin, err := e.App.FindRecordById(PluginsCollection, e.Record.GetString("plugin"))
	if err != nil {
		return badRequest("Unknown plugin.")
	}

	if !e.HasSuperuserAuth() {
		if !isUserAuth(e) || !IsAuthor(e.Auth, plugin) {
			return forbidden("You can only upload versions of your own plugins.")
		}
		if plugin.GetString("status") == StatusDisabled {
			return forbidden("This plugin was disabled by the Ingress Plus team.")
		}
		pending, err := e.App.CountRecords(VersionsCollection, dbx.HashExp{"plugin": plugin.Id, "status": VersionPending})
		if err != nil {
			return err
		}
		if pending > 0 {
			return badRequest("This plugin already has a version waiting for review.")
		}
		e.Record.Set("status", VersionPending)
		e.Record.Set("review_notes", "")
	} else if e.Record.GetString("status") == "" {
		e.Record.Set("status", VersionPending)
	}

	code, err := readUpload(e.Record, "file")
	if err != nil {
		return err
	}
	meta, err := ParseUserscript(code)
	if err != nil {
		return badRequest(err.Error())
	}

	version := strings.TrimSpace(e.Record.GetString("version"))
	if version == "" {
		version = meta.Version
	}
	if version != meta.Version {
		return badRequest(fmt.Sprintf("The version field (%s) doesn't match the @version in the userscript header (%s).", version, meta.Version))
	}
	if !ValidVersion(version) {
		return badRequest("The version must look like 1.2.3.")
	}
	e.Record.Set("version", version)

	sum := sha256.Sum256(code)
	e.Record.Set("sha256", hex.EncodeToString(sum[:]))

	return e.Next()
}

// CanReviewVersion reports which version status changes review can make.
// approved -> rejected pulls a version that turned out to be bad; the
// plugin then falls back to its previous approved version.
func CanReviewVersion(oldStatus, newStatus string) bool {
	if oldStatus == newStatus {
		return true
	}
	switch oldStatus {
	case VersionPending:
		return newStatus == VersionApproved || newStatus == VersionRejected
	case VersionApproved:
		return newStatus == VersionRejected
	}
	return false
}

func guardVersionUpdate(e *core.RecordRequestEvent) error {
	// The update rule is empty, so only superusers get here. Versions are
	// immutable: what was reviewed is exactly what gets served.
	original := e.Record.Original()
	for _, field := range []string{"plugin", "version", "file", "sha256"} {
		if e.Record.GetString(field) != original.GetString(field) {
			return badRequest(fmt.Sprintf("%q can't be changed on an uploaded version.", field))
		}
	}
	if len(e.Record.GetUploadedFiles("file")) > 0 {
		return badRequest("The file of an uploaded version can't be replaced.")
	}
	if !CanReviewVersion(original.GetString("status"), e.Record.GetString("status")) {
		return badRequest(fmt.Sprintf("A version can't go from %q to %q.", original.GetString("status"), e.Record.GetString("status")))
	}
	return e.Next()
}

// latestApprovedVersion returns the newest approved version of plugin other
// than exceptID, or nil.
func latestApprovedVersion(app core.App, pluginID, exceptID string) *core.Record {
	records, err := app.FindRecordsByFilter(VersionsCollection,
		"plugin = {:plugin} && status = {:status} && id != {:except}", "-created", 1, 0,
		dbx.Params{"plugin": pluginID, "status": VersionApproved, "except": exceptID})
	if err != nil || len(records) == 0 {
		return nil
	}
	return records[0]
}

// syncPluginWithVersion updates the plugin after one of its versions was
// created or reviewed, so the plugin's status and current_version always
// describe what the store serves.
func syncPluginWithVersion(app core.App, version *core.Record) error {
	plugin, err := app.FindRecordById(PluginsCollection, version.GetString("plugin"))
	if err != nil {
		return err
	}
	status := plugin.GetString("status")
	disabled := status == StatusDisabled
	changed := false

	switch version.GetString("status") {
	case VersionPending:
		// A first submission puts the plugin in the review queue. An
		// already-published plugin keeps serving its current version
		// while the update is reviewed.
		if plugin.GetString("current_version") == "" && (status == StatusDraft || status == StatusRejected) {
			plugin.Set("status", StatusPendingReview)
			changed = true
		}

	case VersionApproved:
		plugin.Set("current_version", version.Id)
		if !disabled {
			plugin.Set("status", StatusApproved)
		}
		changed = true

	case VersionRejected:
		if plugin.GetString("current_version") == version.Id {
			if previous := latestApprovedVersion(app, plugin.Id, version.Id); previous != nil {
				plugin.Set("current_version", previous.Id)
			} else {
				plugin.Set("current_version", "")
				if !disabled {
					plugin.Set("status", StatusRejected)
				}
			}
			changed = true
		} else if plugin.GetString("current_version") == "" && status == StatusPendingReview {
			plugin.Set("status", StatusRejected)
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return app.Save(plugin)
}
