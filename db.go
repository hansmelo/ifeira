package main

// A small client for Turso / libSQL over HTTP (the "Hrana over HTTP" v2
// pipeline API). Using HTTP keeps the program free of database drivers and
// works well on serverless hosts: every call is one short HTTPS request.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DB struct {
	url   string
	token string
	hc    *http.Client

	migrateOnce sync.Once
	migrateErr  error
}

func NewDB(rawURL, token string) *DB {
	u := rawURL
	switch {
	case strings.HasPrefix(u, "libsql://localhost"), strings.HasPrefix(u, "libsql://127.0.0.1"):
		u = "http://" + strings.TrimPrefix(u, "libsql://") // local `turso dev` server
	case strings.HasPrefix(u, "libsql://"):
		u = "https://" + strings.TrimPrefix(u, "libsql://")
	case strings.HasPrefix(u, "wss://"):
		u = "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		u = "http://" + strings.TrimPrefix(u, "ws://")
	}
	return &DB{url: strings.TrimRight(u, "/"), token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

// Stmt is one SQL statement with positional (?) arguments.
type Stmt struct {
	SQL  string
	Args []any
}

func Q(sql string, args ...any) Stmt { return Stmt{SQL: sql, Args: args} }

// Result holds the rows of one statement. Values are string, int64, float64, []byte or nil.
type Result struct {
	Cols []string
	Rows [][]any
}

/* ---- wire format ---- */

type hValue struct {
	Type   string `json:"type"`
	Value  any    `json:"value,omitempty"`
	Base64 string `json:"base64,omitempty"`
}

type hStmt struct {
	SQL      string   `json:"sql"`
	Args     []hValue `json:"args,omitempty"`
	WantRows bool     `json:"want_rows"`
}

type hCond struct {
	Type string `json:"type"`
	Step *int   `json:"step,omitempty"`
	Cond *hCond `json:"cond,omitempty"`
}

type hStep struct {
	Stmt      hStmt  `json:"stmt"`
	Condition *hCond `json:"condition,omitempty"`
}

type hRequest struct {
	Type  string `json:"type"`
	Stmt  *hStmt `json:"stmt,omitempty"`
	Batch *struct {
		Steps []hStep `json:"steps"`
	} `json:"batch,omitempty"`
}

type hError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

type hExecResult struct {
	Cols []struct {
		Name string `json:"name"`
	} `json:"cols"`
	Rows [][]hValue `json:"rows"`
}

type hResponse struct {
	Results []struct {
		Type     string  `json:"type"`
		Error    *hError `json:"error"`
		Response *struct {
			Type   string          `json:"type"`
			Result json.RawMessage `json:"result"`
		} `json:"response"`
	} `json:"results"`
}

func encodeArg(v any) (hValue, error) {
	switch x := v.(type) {
	case nil:
		return hValue{Type: "null"}, nil
	case string:
		return hValue{Type: "text", Value: x}, nil
	case int:
		return hValue{Type: "integer", Value: strconv.Itoa(x)}, nil
	case int64:
		return hValue{Type: "integer", Value: strconv.FormatInt(x, 10)}, nil
	case bool:
		if x {
			return hValue{Type: "integer", Value: "1"}, nil
		}
		return hValue{Type: "integer", Value: "0"}, nil
	case float64:
		return hValue{Type: "float", Value: x}, nil
	case []byte:
		return hValue{Type: "blob", Base64: base64.StdEncoding.EncodeToString(x)}, nil
	}
	return hValue{}, fmt.Errorf("db: unsupported argument type %T", v)
}

func decodeValue(v hValue) any {
	switch v.Type {
	case "text":
		s, _ := v.Value.(string)
		return s
	case "integer":
		switch n := v.Value.(type) {
		case string:
			i, _ := strconv.ParseInt(n, 10, 64)
			return i
		case float64:
			return int64(n)
		}
		return int64(0)
	case "float":
		f, _ := v.Value.(float64)
		return f
	case "blob":
		s := strings.TrimRight(v.Base64, "=")
		b, err := base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return []byte(nil)
		}
		return b
	}
	return nil
}

func toHStmt(s Stmt) (hStmt, error) {
	hs := hStmt{SQL: s.SQL, WantRows: true}
	for _, a := range s.Args {
		v, err := encodeArg(a)
		if err != nil {
			return hs, err
		}
		hs.Args = append(hs.Args, v)
	}
	return hs, nil
}

func (d *DB) post(ctx context.Context, reqs []hRequest) (*hResponse, error) {
	body, err := json.Marshal(map[string]any{"baton": nil, "requests": append(reqs, hRequest{Type: "close"})})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url+"/v2/pipeline", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
	res, err := d.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("db: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out hResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("db: bad response: %w", err)
	}
	return &out, nil
}

// Query runs statements one after another in a single round trip and returns their results.
func (d *DB) Query(ctx context.Context, stmts ...Stmt) ([]Result, error) {
	if err := d.migrate(ctx); err != nil {
		return nil, err
	}
	return d.query(ctx, stmts...)
}

func (d *DB) query(ctx context.Context, stmts ...Stmt) ([]Result, error) {
	reqs := make([]hRequest, 0, len(stmts))
	for _, s := range stmts {
		hs, err := toHStmt(s)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, hRequest{Type: "execute", Stmt: &hs})
	}
	out, err := d.post(ctx, reqs)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(stmts))
	for i := range stmts {
		if i >= len(out.Results) {
			return nil, errors.New("db: missing result")
		}
		r := out.Results[i]
		if r.Type != "ok" || r.Response == nil {
			msg := "unknown error"
			if r.Error != nil {
				msg = r.Error.Message
			}
			return nil, fmt.Errorf("db: %s", msg)
		}
		var er hExecResult
		if err := json.Unmarshal(r.Response.Result, &er); err != nil {
			return nil, err
		}
		res := Result{}
		for _, c := range er.Cols {
			res.Cols = append(res.Cols, c.Name)
		}
		for _, row := range er.Rows {
			vals := make([]any, len(row))
			for j, v := range row {
				vals[j] = decodeValue(v)
			}
			res.Rows = append(res.Rows, vals)
		}
		results = append(results, res)
	}
	return results, nil
}

// Tx runs statements as one transaction: all of them apply, or none do.
func (d *DB) Tx(ctx context.Context, stmts ...Stmt) error {
	if err := d.migrate(ctx); err != nil {
		return err
	}
	if len(stmts) == 0 {
		return nil
	}
	steps := []hStep{{Stmt: hStmt{SQL: "BEGIN"}}}
	for _, s := range stmts {
		hs, err := toHStmt(s)
		if err != nil {
			return err
		}
		hs.WantRows = false
		prev := len(steps) - 1
		steps = append(steps, hStep{Stmt: hs, Condition: &hCond{Type: "ok", Step: &prev}})
	}
	last := len(steps) - 1
	steps = append(steps, hStep{Stmt: hStmt{SQL: "COMMIT"}, Condition: &hCond{Type: "ok", Step: &last}})
	commit := len(steps) - 1
	steps = append(steps, hStep{Stmt: hStmt{SQL: "ROLLBACK"}, Condition: &hCond{Type: "not", Cond: &hCond{Type: "ok", Step: &commit}}})

	req := hRequest{Type: "batch", Batch: &struct {
		Steps []hStep `json:"steps"`
	}{Steps: steps}}
	out, err := d.post(ctx, []hRequest{req})
	if err != nil {
		return err
	}
	if len(out.Results) == 0 {
		return errors.New("db: missing batch result")
	}
	r := out.Results[0]
	if r.Type != "ok" || r.Response == nil {
		if r.Error != nil {
			return fmt.Errorf("db: %s", r.Error.Message)
		}
		return errors.New("db: batch failed")
	}
	var br struct {
		StepErrors []*hError `json:"step_errors"`
	}
	if err := json.Unmarshal(r.Response.Result, &br); err != nil {
		return err
	}
	for i, e := range br.StepErrors {
		if e != nil && i != len(steps)-1 {
			return fmt.Errorf("db: %s", e.Message)
		}
	}
	return nil
}

// migrate creates the tables the first time this instance talks to the database.
func (d *DB) migrate(ctx context.Context) error {
	d.migrateOnce.Do(func() {
		_, d.migrateErr = d.query(ctx,
			Q(`CREATE TABLE IF NOT EXISTS products (id TEXT PRIMARY KEY, data TEXT NOT NULL, updated_at INTEGER NOT NULL)`),
			Q(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`),
			Q(`CREATE TABLE IF NOT EXISTS photos (id TEXT PRIMARY KEY, mime TEXT NOT NULL, data BLOB NOT NULL, updated_at INTEGER NOT NULL)`),
		)
	})
	if d.migrateErr != nil {
		err := d.migrateErr
		d.migrateOnce = sync.Once{} // try again on the next request
		return err
	}
	return nil
}
