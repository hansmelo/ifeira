package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	maxProductBytes = 8 << 10
	maxLayoutBytes  = 64 << 10
	maxSyncItems    = 3000
	maxPhotoBytes   = 2 << 20
)

// GET /api/state?since=REV
// Returns {changed:false, rev} when nothing changed since REV, otherwise the
// whole fridge: {changed:true, rev, layout, products}.
func (a *App) handleGetState(w http.ResponseWriter, r *http.Request) {
	res, err := a.db.Query(r.Context(),
		Q(`SELECT value FROM meta WHERE key = 'rev'`),
		Q(`SELECT value FROM meta WHERE key = 'layout'`),
		Q(`SELECT data FROM products ORDER BY id`),
	)
	if err != nil {
		dbFail(w, err)
		return
	}
	rev := ""
	if len(res[0].Rows) > 0 {
		rev, _ = res[0].Rows[0][0].(string)
	}
	if since := r.URL.Query().Get("since"); since != "" && since == rev {
		writeJSON(w, http.StatusOK, map[string]any{"changed": false, "rev": rev})
		return
	}
	var layout json.RawMessage
	if len(res[1].Rows) > 0 {
		if s, ok := res[1].Rows[0][0].(string); ok && json.Valid([]byte(s)) {
			layout = json.RawMessage(s)
		}
	}
	products := make([]json.RawMessage, 0, len(res[2].Rows))
	for _, row := range res[2].Rows {
		if s, ok := row[0].(string); ok && json.Valid([]byte(s)) {
			products = append(products, json.RawMessage(s))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": true, "rev": rev, "layout": layout, "products": products})
}

// POST /api/sync {upsert:[product...], delete:[id...], layout:{zones,temps}|null}
// Applies one person's changes in a single transaction. Each product is its own
// row, so two people editing different products never overwrite each other.
func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Upsert []json.RawMessage `json:"upsert"`
		Delete []string          `json:"delete"`
		Layout json.RawMessage   `json:"layout"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if len(body.Upsert)+len(body.Delete) > maxSyncItems {
		writeError(w, http.StatusRequestEntityTooLarge, "too_many_items")
		return
	}
	now := time.Now().Unix()
	var stmts []Stmt
	for _, raw := range body.Upsert {
		if len(raw) > maxProductBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "product_too_large")
			return
		}
		var head struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || !idPattern.MatchString(head.ID) || head.Name == "" {
			writeError(w, http.StatusBadRequest, "bad_product")
			return
		}
		stmts = append(stmts, Q(`INSERT INTO products (id, data, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`, head.ID, string(raw), now))
	}
	for _, id := range body.Delete {
		if !idPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, "bad_id")
			return
		}
		stmts = append(stmts, Q(`DELETE FROM products WHERE id = ?`, id))
	}
	if len(body.Layout) > 0 && string(body.Layout) != "null" {
		if len(body.Layout) > maxLayoutBytes || !json.Valid(body.Layout) {
			writeError(w, http.StatusBadRequest, "bad_layout")
			return
		}
		stmts = append(stmts, Q(`INSERT INTO meta (key, value) VALUES ('layout', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(body.Layout)))
	}
	rev := strconv.FormatInt(time.Now().UnixNano(), 36)
	stmts = append(stmts,
		Q(`INSERT INTO meta (key, value) VALUES ('rev', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, rev),
		// Photos of products deleted more than a day ago (the day leaves room for "Desfazer").
		Q(`DELETE FROM photos WHERE id NOT IN (SELECT id FROM products) AND updated_at < ?`, now-86400),
	)
	if err := a.db.Tx(r.Context(), stmts...); err != nil {
		dbFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rev": rev})
}

// GET /api/photos/{id}
func (a *App) handleGetPhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	res, err := a.db.Query(r.Context(), Q(`SELECT mime, data FROM photos WHERE id = ?`, id))
	if err != nil {
		dbFail(w, err)
		return
	}
	if len(res[0].Rows) == 0 {
		http.NotFound(w, r)
		return
	}
	mime, _ := res[0].Rows[0][0].(string)
	data, _ := res[0].Rows[0][1].([]byte)
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	// The app asks for /api/photos/ID?v=VERSION, so a cached copy never goes stale.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// PUT /api/photos/{id} with the image as the body (the app sends a ~100 KB JPEG).
func (a *App) handlePutPhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad_id")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPhotoBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "photo_too_large")
		return
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/webp":
	default:
		writeError(w, http.StatusUnsupportedMediaType, "not_an_image")
		return
	}
	now := time.Now().Unix()
	if err := a.db.Tx(r.Context(), Q(`INSERT INTO photos (id, mime, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET mime = excluded.mime, data = excluded.data, updated_at = excluded.updated_at`, id, mime, data, now)); err != nil {
		dbFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "v": strconv.FormatInt(time.Now().UnixMilli(), 36)})
}

// DELETE /api/photos/{id}
func (a *App) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad_id")
		return
	}
	if err := a.db.Tx(r.Context(), Q(`DELETE FROM photos WHERE id = ?`, id)); err != nil {
		dbFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func dbFail(w http.ResponseWriter, err error) {
	log.Printf("database error: %v", err)
	writeError(w, http.StatusBadGateway, "database")
}
