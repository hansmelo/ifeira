// iFeira: shared fridge and pantry tracker for a family.
//
// One Go program serves the web app, keeps the data in a Turso (libSQL)
// database and reads receipts and product photos with Claude.
// It uses only the Go standard library.
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

//go:embed web
var webFiles embed.FS

// Config comes from environment variables (set them in Vercel → Settings → Environment Variables).
type Config struct {
	DBURL          string // TURSO_DATABASE_URL, e.g. libsql://ifeira-yourname.turso.io
	DBToken        string // TURSO_AUTH_TOKEN
	FamilyPassword string // FAMILY_PASSWORD: the password everyone in the family types once
	SessionSecret  string // SESSION_SECRET (optional): signs the login cookie
	AnthropicKey   string // ANTHROPIC_API_KEY (optional): turns on reading with Claude
	AnthropicModel string // ANTHROPIC_MODEL (optional), default claude-sonnet-5-5
	AnthropicURL   string // ANTHROPIC_BASE_URL (optional), default https://api.anthropic.com
}

func loadConfig() Config {
	c := Config{
		DBURL:          strings.TrimSpace(os.Getenv("TURSO_DATABASE_URL")),
		DBToken:        strings.TrimSpace(os.Getenv("TURSO_AUTH_TOKEN")),
		FamilyPassword: os.Getenv("FAMILY_PASSWORD"),
		SessionSecret:  os.Getenv("SESSION_SECRET"),
		AnthropicKey:   strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicModel: strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL")),
		AnthropicURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")), "/"),
	}
	if c.AnthropicModel == "" {
		c.AnthropicModel = "claude-sonnet-5-5"
	}
	if c.AnthropicURL == "" {
		c.AnthropicURL = "https://api.anthropic.com"
	}
	return c
}

type App struct {
	cfg  Config
	db   *DB
	auth *Auth
}

func main() {
	cfg := loadConfig()
	app := &App{cfg: cfg, auth: NewAuth(cfg.FamilyPassword, cfg.SessionSecret)}
	if cfg.DBURL != "" {
		app.db = NewDB(cfg.DBURL, cfg.DBToken)
	}
	for _, p := range app.setupProblems() {
		log.Printf("setup: %s", p)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withHeaders(app.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("iFeira listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", a.handleConfig)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)

	mux.Handle("GET /api/state", a.requireLogin(a.handleGetState))
	mux.Handle("POST /api/sync", a.requireLogin(a.handleSync))
	mux.Handle("GET /api/photos/{id}", a.requireLogin(a.handleGetPhoto))
	mux.Handle("PUT /api/photos/{id}", a.requireLogin(a.handlePutPhoto))
	mux.Handle("DELETE /api/photos/{id}", a.requireLogin(a.handleDeletePhoto))
	mux.Handle("POST /api/ai/receipt", a.requireLogin(a.handleReceipt))
	mux.Handle("POST /api/ai/identify", a.requireLogin(a.handleIdentify))

	static := staticHandler()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		static.ServeHTTP(w, r)
	})
	return mux
}

// setupProblems lists missing configuration, shown on the login screen.
func (a *App) setupProblems() []string {
	var p []string
	if a.cfg.DBURL == "" {
		p = append(p, "TURSO_DATABASE_URL não está definido")
	}
	if a.cfg.FamilyPassword == "" {
		p = append(p, "FAMILY_PASSWORD não está definido")
	}
	return p
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authed": a.auth.Valid(r),
		"ai":     a.cfg.AnthropicKey != "",
		"setup":  a.setupProblems(),
	})
}

/* ---------- static files ---------- */

func staticHandler() http.Handler {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" || p == "." {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			if path.Ext(p) != "" { // a missing file, e.g. /sortable.min.js
				http.NotFound(w, r)
				return
			}
			p = "index.html" // any app path falls back to the app
		}
		switch {
		case p == "index.html" || p == "sw.js" || strings.HasSuffix(p, ".webmanifest"):
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + p
		if p == "index.html" {
			r2.URL.Path = "/" // FileServer redirects /index.html to /
		}
		files.ServeHTTP(w, r2)
	})
}

/* ---------- helpers ---------- */

func withHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
