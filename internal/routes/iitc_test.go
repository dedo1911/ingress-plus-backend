package routes

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/iitc"
	"github.com/dedo1911/ingress-plus-backend/internal/notify"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

type iitcHarness struct {
	t   *testing.T
	app *tests.TestApp
	mux http.Handler
	wm  *iitc.Watermarker
}

func newIITCHarness(t *testing.T) *iitcHarness {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []core.Field{
		&core.TextField{Name: "username"},
		&core.TextField{Name: "verification"},
		&core.TextField{Name: "faction"},
		&core.BoolField{Name: "supporter"},
	} {
		if users.Fields.GetByName(f.GetName()) == nil {
			users.Fields.Add(f)
		}
	}
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}

	if err := iitc.CreateCollections(app); err != nil {
		t.Fatal(err)
	}
	iitc.RegisterHooks(app, &notify.Telegram{})

	wm, _ := iitc.NewWatermarker("test-key")
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	RegisterIITC(&core.ServeEvent{App: app, Router: router}, wm)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	return &iitcHarness{t: t, app: app, mux: mux, wm: wm}
}

func (h *iitcHarness) user(username string, set map[string]any) (*core.Record, string) {
	h.t.Helper()
	users, _ := h.app.FindCollectionByNameOrId("users")
	r := core.NewRecord(users)
	r.SetEmail(username + "@iitc.test")
	r.SetPassword("1234567890")
	r.Set("username", username)
	for k, v := range set {
		r.Set(k, v)
	}
	if err := h.app.Save(r); err != nil {
		h.t.Fatal(err)
	}
	token, err := r.NewAuthToken()
	if err != nil {
		h.t.Fatal(err)
	}
	return r, token
}

func (h *iitcHarness) superuserToken() string {
	h.t.Helper()
	col, _ := h.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	r := core.NewRecord(col)
	r.SetEmail("root@iitc.test")
	r.SetPassword("1234567890")
	if err := h.app.Save(r); err != nil {
		h.t.Fatal(err)
	}
	token, _ := r.NewAuthToken()
	return token
}

func (h *iitcHarness) do(method, url string, body io.Reader, contentType, token string) (int, map[string]any) {
	h.t.Helper()
	req := httptest.NewRequest(method, url, body)
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (h *iitcHarness) json(method, url, body, token string) (int, map[string]any) {
	return h.do(method, url, strings.NewReader(body), "application/json", token)
}

func (h *iitcHarness) uploadVersion(pluginID, version, code, token string) (int, map[string]any) {
	var buf bytes.Buffer
	mp := multipart.NewWriter(&buf)
	_ = mp.WriteField("plugin", pluginID)
	_ = mp.WriteField("version", version)
	_ = mp.WriteField("status", iitc.VersionApproved) // must be ignored for authors
	fw, _ := mp.CreateFormFile("file", "plugin.user.js")
	_, _ = fw.Write([]byte(code))
	mp.Close()
	return h.do(http.MethodPost, "/api/collections/iitc_plugin_versions/records", &buf, mp.FormDataContentType(), token)
}

func (h *iitcHarness) load(slug, token string) (int, map[string]any, string) {
	h.t.Helper()
	return h.loadWith(slug, token, nil)
}

// loadWith is load with the encodings the client says it can inflate.
func (h *iitcHarness) loadWith(slug, token string, encodings []string) (int, map[string]any, string) {
	h.t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	body, _ := json.Marshal(map[string]any{
		"clientPub": base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()),
		"encodings": encodings,
	})
	status, out := h.json(http.MethodPost, "/api/iitc/plugins/"+slug+"/load", string(body), "Bearer "+token)
	if status != http.StatusOK {
		return status, out, ""
	}
	decode := func(k string) []byte {
		b, err := base64.StdEncoding.DecodeString(out[k].(string))
		if err != nil {
			h.t.Fatalf("decoding %s: %v", k, err)
		}
		return b
	}
	plain, err := iitc.Open(priv, &iitc.Sealed{ServerPub: decode("serverPub"), IV: decode("iv"), Ciphertext: decode("ciphertext")})
	if err != nil {
		h.t.Fatalf("decrypting payload: %v", err)
	}
	if out["encoding"] == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(plain))
		if err != nil {
			h.t.Fatalf("opening gzip payload: %v", err)
		}
		if plain, err = io.ReadAll(zr); err != nil {
			h.t.Fatalf("inflating payload: %v", err)
		}
	}
	return status, out, string(plain)
}

func (h *iitcHarness) mintToken(authToken string) string {
	h.t.Helper()
	status, out := h.json(http.MethodPost, "/api/iitc/tokens", `{"label":"Firefox"}`, authToken)
	if status != http.StatusOK {
		h.t.Fatalf("minting token: %d %v", status, out)
	}
	return out["token"].(string)
}

func slugs(out map[string]any) []string {
	var s []string
	list, _ := out["plugins"].([]any)
	for _, p := range list {
		s = append(s, p.(map[string]any)["slug"].(string))
	}
	return s
}

func pluginCode(version string) string {
	return "// ==UserScript==\n// @name IITC plugin: Test\n// @version " + version + "\n// ==/UserScript==\nfunction wrapper() { window.TEST_PLUGIN = '" + version + "' }\n"
}

func TestIITCStoreFlow(t *testing.T) {
	h := newIITCHarness(t)
	author, authorAuth := h.user("author", nil)
	_, otherAuth := h.user("other", nil)
	supporter, supporterAuth := h.user("supporter", map[string]any{"supporter": true})
	root := h.superuserToken()

	// --- Plugin creation: author/status/current_version are forced.
	status, plugin := h.json(http.MethodPost, "/api/collections/iitc_plugins/records",
		`{"slug":"Test-Plugin","name":"Test","author":"`+supporter.Id+`","status":"approved","access":{"rules":[{"type":"supporter"}]}}`, authorAuth)
	if status != http.StatusOK {
		t.Fatalf("create plugin: %d %v", status, plugin)
	}
	if plugin["author"] != author.Id || plugin["status"] != iitc.StatusDraft || plugin["slug"] != "test-plugin" {
		t.Fatalf("create guard didn't apply: %v", plugin)
	}
	pluginID := plugin["id"].(string)

	if status, _ := h.json(http.MethodPost, "/api/collections/iitc_plugins/records",
		`{"slug":"bad-rules","name":"x","access":{"rules":[{"type":"everyone"}]}}`, authorAuth); status != http.StatusBadRequest {
		t.Fatalf("invalid access rules accepted: %d", status)
	}

	// --- Store catalog: only the author sees a draft.
	if _, out := h.json(http.MethodGet, "/api/plugins", "", authorAuth); len(slugs(out)) != 1 {
		t.Fatalf("author catalog: %v", out)
	}
	if _, out := h.json(http.MethodGet, "/api/plugins", "", supporterAuth); len(slugs(out)) != 0 {
		t.Fatalf("draft visible to others: %v", out)
	}
	if status, _ := h.json(http.MethodGet, "/api/plugins", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous catalog: %d", status)
	}

	// --- Versions.
	if status, out := h.uploadVersion(pluginID, "1.0.1", pluginCode("1.0.0"), authorAuth); status != http.StatusBadRequest {
		t.Fatalf("mismatched version accepted: %d %v", status, out)
	}
	if status, out := h.uploadVersion(pluginID, "1.0.0", "console.log('no header')", authorAuth); status != http.StatusBadRequest {
		t.Fatalf("headerless file accepted: %d %v", status, out)
	}
	if status, _ := h.uploadVersion(pluginID, "1.0.0", pluginCode("1.0.0"), otherAuth); status == http.StatusOK {
		t.Fatal("non-author uploaded a version")
	}
	status, version := h.uploadVersion(pluginID, "1.0.0", pluginCode("1.0.0"), authorAuth)
	if status != http.StatusOK {
		t.Fatalf("upload version: %d %v", status, version)
	}
	if version["status"] != iitc.VersionPending || len(version["sha256"].(string)) != 64 {
		t.Fatalf("version guard didn't apply: %v", version)
	}
	versionID := version["id"].(string)
	if status, _ := h.uploadVersion(pluginID, "1.0.1", pluginCode("1.0.1"), authorAuth); status != http.StatusBadRequest {
		t.Fatalf("second pending version accepted: %d", status)
	}

	p, _ := h.app.FindRecordById(iitc.PluginsCollection, pluginID)
	if p.GetString("status") != iitc.StatusPendingReview {
		t.Fatalf("plugin not queued for review: %s", p.GetString("status"))
	}

	// --- Authors can't approve themselves.
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugins/records/"+pluginID, `{"status":"approved"}`, authorAuth); status != http.StatusForbidden {
		t.Fatalf("author self-approved: %d", status)
	}
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugins/records/"+pluginID, `{"current_version":"`+versionID+`"}`, authorAuth); status != http.StatusForbidden {
		t.Fatalf("author set current_version: %d", status)
	}
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugin_versions/records/"+versionID, `{"status":"approved"}`, authorAuth); status == http.StatusOK {
		t.Fatal("author approved their own version")
	}
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugins/records/"+pluginID, `{"slug":"renamed"}`, authorAuth); status != http.StatusBadRequest {
		t.Fatalf("slug changed: %d", status)
	}

	// --- Sideloader tokens.
	authorToken := h.mintToken(authorAuth)
	supporterToken := h.mintToken(supporterAuth)
	otherToken := h.mintToken(otherAuth)

	if status, _ := h.json(http.MethodGet, "/api/iitc/plugins", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, _ := h.json(http.MethodGet, "/api/iitc/plugins", "", authorAuth); status != http.StatusUnauthorized {
		t.Fatalf("PocketBase JWT accepted as an IITC token: %d", status)
	}
	if status, _ := h.json(http.MethodGet, "/api/iitc/plugins", "", "Bearer ipx_nope"); status != http.StatusUnauthorized {
		t.Fatalf("bogus token: %d", status)
	}
	if status, me := h.json(http.MethodGet, "/api/iitc/me", "", "Bearer "+supporterToken); status != http.StatusOK || me["username"] != "supporter" {
		t.Fatalf("me: %d %v", status, me)
	}

	// The author can try their pending version before review.
	_, out := h.json(http.MethodGet, "/api/iitc/plugins", "", "Bearer "+authorToken)
	list := out["plugins"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["devVersion"] != "1.0.0" {
		t.Fatalf("author IITC catalog: %v", out)
	}
	status, loaded, code := h.load("test-plugin", authorToken)
	if status != http.StatusOK || loaded["dev"] != true || !strings.Contains(code, "window.TEST_PLUGIN = \"1.0.0\"") {
		t.Fatalf("author dev load: %d %v", status, loaded)
	}
	if !strings.Contains(code, h.wm.Fingerprint(author.Id)) {
		t.Fatal("served code isn't watermarked")
	}
	if loaded["encoding"] != "identity" {
		t.Fatalf("plain load without encodings: %v", loaded["encoding"])
	}
	status, loaded, code = h.loadWith("test-plugin", authorToken, []string{"gzip"})
	if status != http.StatusOK || loaded["encoding"] != "gzip" || !strings.Contains(code, "window.TEST_PLUGIN = \"1.0.0\"") {
		t.Fatalf("gzip load: %d %v %q", status, loaded, code)
	}
	if !strings.Contains(code, "// @version 1.0.0") || !strings.Contains(code, h.wm.Fingerprint(author.Id)) {
		t.Fatalf("gzip load lost the header or the watermark: %q", code)
	}
	if status, _, _ := h.load("test-plugin", supporterToken); status != http.StatusNotFound {
		t.Fatalf("unreviewed plugin served to supporter: %d", status)
	}

	// --- Review.
	if status, out := h.json(http.MethodPatch, "/api/collections/iitc_plugin_versions/records/"+versionID, `{"status":"approved"}`, root); status != http.StatusOK {
		t.Fatalf("approve: %d %v", status, out)
	}
	p, _ = h.app.FindRecordById(iitc.PluginsCollection, pluginID)
	if p.GetString("status") != iitc.StatusApproved || p.GetString("current_version") != versionID {
		t.Fatalf("plugin not published: %s %s", p.GetString("status"), p.GetString("current_version"))
	}
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugin_versions/records/"+versionID, `{"version":"9.9.9"}`, root); status != http.StatusBadRequest {
		t.Fatalf("version record mutated: %d", status)
	}

	// --- Access rules: supporter yes, other no (and other never sees it).
	if _, out := h.json(http.MethodGet, "/api/iitc/plugins", "", "Bearer "+supporterToken); len(slugs(out)) != 1 {
		t.Fatalf("supporter IITC catalog: %v", out)
	}
	if _, out := h.json(http.MethodGet, "/api/iitc/plugins", "", "Bearer "+otherToken); len(slugs(out)) != 0 {
		t.Fatalf("plugin listed for a user without access: %v", out)
	}
	if status, _ := h.json(http.MethodGet, "/api/plugins/test-plugin", "", otherAuth); status != http.StatusNotFound {
		t.Fatalf("detail for a user without access: %d", status)
	}
	if status, _, _ := h.load("test-plugin", otherToken); status != http.StatusNotFound {
		t.Fatalf("load for a user without access: %d", status)
	}

	status, detail := h.json(http.MethodGet, "/api/plugins/test-plugin", "", supporterAuth)
	if status != http.StatusOK || detail["version"] != "1.0.0" {
		t.Fatalf("supporter detail: %d %v", status, detail)
	}
	if _, leaked := detail["access"]; leaked {
		t.Fatal("access rules shown to a non-author")
	}

	status, loaded, code = h.load("test-plugin", supporterToken)
	if status != http.StatusOK || loaded["dev"] != false || !strings.Contains(code, h.wm.Fingerprint(supporter.Id)) {
		t.Fatalf("supporter load: %d %v", status, loaded)
	}
	if status, _ := h.json(http.MethodPost, "/api/iitc/plugins/test-plugin/load", `{"clientPub":"AAAA"}`, "Bearer "+supporterToken); status != http.StatusBadRequest {
		t.Fatalf("bad client key: %d", status)
	}

	// --- Revoking a device is deleting its token record.
	tokenRec, _ := h.app.FindFirstRecordByData(iitc.TokensCollection, "token_hash", iitc.HashToken(supporterToken))
	if status, _ := h.json(http.MethodDelete, "/api/collections/iitc_tokens/records/"+tokenRec.Id, "", otherAuth); status == http.StatusNoContent {
		t.Fatal("another user revoked the token")
	}
	if status, _ := h.json(http.MethodDelete, "/api/collections/iitc_tokens/records/"+tokenRec.Id, "", supporterAuth); status != http.StatusNoContent {
		t.Fatalf("revoke: %d", status)
	}
	if status, _ := h.json(http.MethodGet, "/api/iitc/plugins", "", "Bearer "+supporterToken); status != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", status)
	}

	// The sideloader's own Disconnect revokes its token too.
	if status, _ := h.json(http.MethodDelete, "/api/iitc/tokens/current", "", "Bearer "+otherToken); status != http.StatusNoContent {
		t.Fatalf("self-revoke: %d", status)
	}
	if status, _ := h.json(http.MethodGet, "/api/iitc/me", "", "Bearer "+otherToken); status != http.StatusUnauthorized {
		t.Fatalf("self-revoked token still works: %d", status)
	}

	// --- Pulling the only approved version unpublishes the plugin.
	if status, _ := h.json(http.MethodPatch, "/api/collections/iitc_plugin_versions/records/"+versionID, `{"status":"rejected"}`, root); status != http.StatusOK {
		t.Fatalf("reject: %d", status)
	}
	p, _ = h.app.FindRecordById(iitc.PluginsCollection, pluginID)
	if p.GetString("status") != iitc.StatusRejected || p.GetString("current_version") != "" {
		t.Fatalf("plugin still published: %s %s", p.GetString("status"), p.GetString("current_version"))
	}
}

func TestIITCAdminUploadIsApproved(t *testing.T) {
	h := newIITCHarness(t)
	h.superuserToken() // root@iitc.test
	admin, adminAuth := h.user("root", nil)
	_, otherAuth := h.user("other", map[string]any{"verified": true})

	upload := func(slug, token string) (map[string]any, *core.Record) {
		t.Helper()
		status, plugin := h.json(http.MethodPost, "/api/collections/iitc_plugins/records", `{"slug":"`+slug+`","name":"Test"}`, token)
		if status != http.StatusOK {
			t.Fatalf("create plugin: %d %v", status, plugin)
		}
		status, version := h.uploadVersion(plugin["id"].(string), "1.0.0", pluginCode("1.0.0"), token)
		if status != http.StatusOK {
			t.Fatalf("upload version: %d %v", status, version)
		}
		p, _ := h.app.FindRecordById(iitc.PluginsCollection, plugin["id"].(string))
		return version, p
	}

	// The superuser's email alone isn't enough: an unverified account could
	// belong to anyone.
	if v, _ := upload("unverified-admin", adminAuth); v["status"] != iitc.VersionPending {
		t.Fatalf("unverified admin email auto-approved: %v", v)
	}
	if v, _ := upload("regular", otherAuth); v["status"] != iitc.VersionPending {
		t.Fatalf("regular upload not pending: %v", v)
	}

	admin.SetVerified(true)
	if err := h.app.Save(admin); err != nil {
		t.Fatal(err)
	}
	v, p := upload("admin-plugin", adminAuth)
	if v["status"] != iitc.VersionApproved {
		t.Fatalf("admin upload not approved: %v", v)
	}
	if p.GetString("status") != iitc.StatusApproved || p.GetString("current_version") != v["id"] {
		t.Fatalf("admin plugin not published: %s %s", p.GetString("status"), p.GetString("current_version"))
	}
}
