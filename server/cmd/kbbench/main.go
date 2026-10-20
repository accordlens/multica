// kbbench runs only the existing membership middleware and SearchIssues handler.
// No router, event bus, agent, scheduler, model, or outbound HTTP client is started.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type source struct {
	ID       string `json:"source_id"`
	Revision int    `json:"revision"`
	Text     string `json:"text"`
	At       string `json:"at"`
	Author   string `json:"author"`
	Deleted  bool   `json:"deleted"`
}
type document struct {
	ID          string   `json:"issue_id"`
	Workspace   string   `json:"workspace"`
	Title       string   `json:"title"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Description source   `json:"description"`
	Status      string   `json:"status"`
	Assignee    string   `json:"assignee"`
	Comments    []source `json:"comments"`
}
type plan struct {
	ID         string     `json:"question_id"`
	Query      string     `json:"query"`
	Asker      string     `json:"asker_id"`
	Workspaces []string   `json:"workspaces"`
	Documents  []document `json:"documents"`
}
type input struct {
	Synthetic  bool `json:"synthetic"`
	Workspaces map[string]struct {
		Members []string `json:"members"`
	} `json:"workspaces"`
	Plans []plan `json:"plans"`
}
type workspaceRecord struct{ ID, Key, Slug string }
type userRecord struct{ ID, Key, Email string }
type fixture struct {
	Kind       string            `json:"kind"`
	Database   string            `json:"database"`
	Status     string            `json:"status"`
	Workspaces []workspaceRecord `json:"workspaces"`
	Users      []userRecord      `json:"users"`
}
type citation struct {
	ID        string `json:"source_id"`
	Kind      string `json:"kind"`
	Revision  int    `json:"revision"`
	Hash      string `json:"content_sha256"`
	Workspace string `json:"workspace"`
	Text      string `json:"text"`
}
type hit struct {
	ID      string     `json:"issue_id"`
	Sources []citation `json:"sources"`
}
type queryResult struct {
	Workspace string          `json:"workspace"`
	Status    int             `json:"status"`
	Body      json.RawMessage `json:"body"`
	LatencyMS float64         `json:"latency_ms"`
}
type response struct {
	ID                string        `json:"question_id"`
	Hits              []hit         `json:"hits"`
	AllowedSources    []citation    `json:"allowed_sources"`
	AllowedWorkspaces []string      `json:"allowed_workspaces"`
	Queries           []queryResult `json:"queries"`
	DeniedProbes      []queryResult `json:"denied_probes"`
	DeletedExposures  []string      `json:"raw_deleted_text_exposures"`
	HeapBytes         uint64        `json:"go_heap_sample_bytes"`
}

// Managed Git worktrees may not get automatic Go VCS stamping. The documented
// clean-checkout build supplies commit explicitly, as the repository Makefile does.
var commit string

func version() map[string]any {
	if regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(commit) {
		return map[string]any{"code_sha": commit, "dirty": false, "identity_source": "explicit linker stamp from the documented clean-checkout build"}
	}
	v := map[string]any{"code_sha": "unknown", "dirty": true}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				v["code_sha"] = setting.Value
			case "vcs.modified":
				v["dirty"] = setting.Value != "false"
			}
		}
	}
	return v
}

func databaseConfig(raw, expected string) (*pgx.ConnConfig, error) {
	if !regexp.MustCompile(`^multica_[a-z0-9_]*kbbench[a-z0-9_]*$`).MatchString(expected) || raw == "" {
		return nil, errors.New("set explicit KBBENCH_DATABASE_URL and KBBENCH_DATABASE_NAME for a dedicated multica_*kbbench* database")
	}
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, errors.New("invalid benchmark database connection configuration")
	}
	local := func(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }
	if !local(cfg.Host) || cfg.Database != expected {
		return nil, errors.New("benchmark database must be local and match the explicit dedicated name")
	}
	for _, fallback := range cfg.Fallbacks {
		if !local(fallback.Host) {
			return nil, errors.New("remote fallback database hosts are forbidden")
		}
	}
	cfg.ConnectTimeout = 5 * time.Second
	cfg.RuntimeParams["statement_timeout"] = "15000"
	return cfg, nil
}

func saveFixture(path string, f fixture, create bool) error {
	flags := os.O_WRONLY | os.O_TRUNC
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(f)
	closeErr := file.Close()
	return errors.Join(err, closeErr)
}

// cleanup is limited to the manifest's random IDs and verifies their slugs and
// emails before deleting. It never drops a database or truncates a table.
func cleanup(ctx context.Context, conn *pgx.Conn, f fixture) error {
	if f.Kind != "multica-kbbench-fixture-v1" || f.Database != conn.Config().Database {
		return errors.New("cleanup manifest/database mismatch")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, ws := range f.Workspaces {
		var slug string
		err := tx.QueryRow(ctx, `SELECT slug FROM workspace WHERE id=$1 FOR UPDATE`, ws.ID).Scan(&slug)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(slug, "kbbench-") || slug != ws.Slug {
			return errors.New("workspace identity changed; refusing cleanup")
		}
		for _, statement := range []string{
			`DELETE FROM comment WHERE workspace_id=$1`,
			`DELETE FROM issue WHERE workspace_id=$1`,
			`DELETE FROM search_index_change WHERE workspace_id=$1`,
			`DELETE FROM member WHERE workspace_id=$1`,
			`DELETE FROM workspace WHERE id=$1`,
		} {
			if _, err := tx.Exec(ctx, statement, ws.ID); err != nil {
				return err
			}
		}
	}
	for _, u := range f.Users {
		var email string
		err := tx.QueryRow(ctx, `SELECT email FROM "user" WHERE id=$1 FOR UPDATE`, u.ID).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if email != u.Email || !strings.HasSuffix(email, "@kbbench.invalid") {
			return errors.New("user identity changed; refusing cleanup")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM "user" WHERE id=$1`, u.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func setup(ctx context.Context, conn *pgx.Conn, in input, path string) (fixture, error) {
	f := fixture{Kind: "multica-kbbench-fixture-v1", Database: conn.Config().Database, Status: "reserved"}
	if !in.Synthetic || len(in.Plans) == 0 {
		return f, errors.New("a nonempty synthetic plan is required")
	}
	var populated bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace) OR EXISTS(SELECT 1 FROM "user") OR EXISTS(SELECT 1 FROM agent) OR EXISTS(SELECT 1 FROM agent_task_queue) OR EXISTS(SELECT 1 FROM autopilot)`).Scan(&populated); err != nil {
		return f, err
	}
	if populated {
		return f, errors.New("refusing import: benchmark database contains application data; use the checkout's empty dedicated database")
	}
	users := map[string]userRecord{}
	for key, ws := range in.Workspaces {
		id := uuid.NewString()
		f.Workspaces = append(f.Workspaces, workspaceRecord{ID: id, Key: key, Slug: "kbbench-" + id})
		for _, member := range ws.Members {
			if _, exists := users[member]; !exists {
				uid := uuid.NewString()
				u := userRecord{ID: uid, Key: member, Email: uid + "@kbbench.invalid"}
				users[member] = u
				f.Users = append(f.Users, u)
			}
		}
	}
	if err := saveFixture(path, f, true); err != nil {
		return f, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return f, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, u := range f.Users {
		if _, err := tx.Exec(ctx, `INSERT INTO "user" (id,name,email) VALUES ($1,$2,$3)`, u.ID, u.Key, u.Email); err != nil {
			return f, err
		}
	}
	for _, ws := range f.Workspaces {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace (id,name,slug,issue_prefix) VALUES ($1,$2,$3,'SYN')`, ws.ID, ws.Key, ws.Slug); err != nil {
			return f, err
		}
		for _, member := range in.Workspaces[ws.Key].Members {
			if _, err := tx.Exec(ctx, `INSERT INTO member (workspace_id,user_id,role) VALUES ($1,$2,'member')`, ws.ID, users[member].ID); err != nil {
				return f, err
			}
		}
	}
	return f, tx.Commit(ctx)
}

func snapshot(ctx context.Context, conn *pgx.Conn, f fixture, p plan) (map[string]document, map[string]string, error) {
	workspaces := map[string]string{}
	for _, ws := range f.Workspaces {
		workspaces[ws.Key] = ws.ID
	}
	documents, comments := map[string]document{}, map[string]string{}
	prefixes := map[string]string{}
	users := map[string]string{}
	for _, user := range f.Users {
		users[user.Key] = user.ID
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, ws := range f.Workspaces {
		for _, sql := range []string{`DELETE FROM comment WHERE workspace_id=$1`, `DELETE FROM issue WHERE workspace_id=$1`, `DELETE FROM search_index_change WHERE workspace_id=$1`} {
			if _, err := tx.Exec(ctx, sql, ws.ID); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, doc := range p.Documents {
		workspaceID := workspaces[doc.Workspace]
		prefix, suffix, ok := strings.Cut(doc.ID, "-")
		number, err := strconv.Atoi(suffix)
		if !ok || err != nil || number <= 0 || workspaceID == "" || doc.Description.Revision <= 0 {
			return nil, nil, errors.New("invalid snapshot issue identity or revision")
		}
		created, err := time.Parse(time.RFC3339, doc.CreatedAt)
		if err != nil {
			return nil, nil, err
		}
		updated, err := time.Parse(time.RFC3339, doc.UpdatedAt)
		if err != nil {
			return nil, nil, err
		}
		if existing := prefixes[doc.Workspace]; existing != "" && existing != prefix {
			return nil, nil, errors.New("mixed issue prefixes in a fixture workspace")
		}
		prefixes[doc.Workspace] = prefix
		// Stable entity UUIDs and source timestamps preserve production SQL
		// tie-breaks across imports; random fixture ownership IDs remain isolated.
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("multica-kbbench:issue:"+doc.Workspace+":"+doc.ID)).String()
		documents[id] = doc
		if _, err := tx.Exec(ctx, `UPDATE workspace SET issue_prefix=$2 WHERE id=$1`, workspaceID, prefix); err != nil {
			return nil, nil, err
		}
		if users[doc.Description.Author] == "" {
			return nil, nil, errors.New("unknown description author")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO issue (id,workspace_id,title,description,status,priority,creator_type,creator_id,number,revision,created_at,assignee_type,assignee_id,updated_at) VALUES ($1,$2,$3,$4,$5,'none','member',$6,$7,$8,$9,CASE WHEN $10::uuid IS NULL THEN NULL ELSE 'member' END,$10,$11)`,
			id, workspaceID, doc.Title, doc.Description.Text, doc.Status, users[doc.Description.Author], number, doc.Description.Revision, created, nullableID(users[doc.Assignee]), updated); err != nil {
			return nil, nil, err
		}
		for _, comment := range doc.Comments {
			if users[comment.Author] == "" {
				return nil, nil, errors.New("unknown comment author")
			}
			cid := uuid.NewSHA1(uuid.NameSpaceURL, []byte("multica-kbbench:comment:"+doc.Workspace+":"+comment.ID)).String()
			comments[cid] = comment.ID
			created, err := time.Parse(time.RFC3339, comment.At)
			if err != nil {
				return nil, nil, err
			}
			var deleted *time.Time
			if comment.Deleted {
				deleted = &created
			}
			if _, err := tx.Exec(ctx, `INSERT INTO comment (id,workspace_id,issue_id,author_type,author_id,content,type,revision,created_at,deleted_at) VALUES ($1,$2,$3,'member',$4,$5,'comment',$6,$7,$8)`,
				cid, workspaceID, id, users[comment.Author], comment.Text, comment.Revision, created, deleted); err != nil {
				return nil, nil, err
			}
		}
	}
	return documents, comments, tx.Commit(ctx)
}

func nullableID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func cite(id string, revision int, text, workspace, kind string) citation {
	digest := sha256.Sum256([]byte(text))
	return citation{ID: id, Kind: kind, Revision: revision, Text: text, Workspace: workspace, Hash: hex.EncodeToString(digest[:])}
}

func hydrate(ctx context.Context, conn *pgx.Conn, id string, doc document, comments map[string]string) ([]citation, error) {
	var text string
	var revision int
	if err := conn.QueryRow(ctx, `SELECT description,revision FROM issue WHERE id=$1`, id).Scan(&text, &revision); err != nil {
		return nil, err
	}
	result := []citation{cite(doc.Description.ID, revision, text, doc.Workspace, "description")}
	rows, err := conn.Query(ctx, `SELECT id::text,content,revision FROM comment WHERE issue_id=$1 AND deleted_at IS NULL ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, content string
		var revision int
		if err := rows.Scan(&cid, &content, &revision); err != nil {
			return nil, err
		}
		result = append(result, cite(comments[cid], revision, content, doc.Workspace, "comment"))
	}
	return result, rows.Err()
}

func query(h http.Handler, workspace, workspaceID, userID, phrase string) (queryResult, error) {
	req := httptest.NewRequest("GET", "/api/issues/search?q="+url.QueryEscape(phrase)+"&limit=5&include_closed=true", nil)
	req.Header.Set("X-User-ID", userID)
	req.Header.Set("X-Workspace-ID", workspaceID)
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, req)
	result := queryResult{Workspace: workspace, Status: w.Code, Body: append(json.RawMessage(nil), w.Body.Bytes()...), LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
	if !json.Valid(result.Body) {
		return result, errors.New("non-JSON search response")
	}
	return result, nil
}

func evaluate(ctx context.Context, conn *pgx.Conn, f fixture, in input, trace io.Writer) ([]response, error) {
	queries := db.New(conn)
	h := &handler.Handler{Queries: queries, TxStarter: conn}
	search := middleware.RequireWorkspaceMember(queries)(http.HandlerFunc(h.SearchIssues))
	responses := []response{}
	for _, p := range in.Plans {
		docs, comments, err := snapshot(ctx, conn, f, p)
		if err != nil {
			return nil, err
		}
		userID := ""
		for _, user := range f.Users {
			if user.Key == p.Asker {
				userID = user.ID
			}
		}
		if userID == "" {
			return nil, errors.New("unknown asker")
		}
		r := response{ID: p.ID, Hits: []hit{}, AllowedSources: []citation{}, AllowedWorkspaces: p.Workspaces, Queries: []queryResult{}, DeniedProbes: []queryResult{}, DeletedExposures: []string{}}
		// Preserve each endpoint's order. Multi-workspace results are kept in
		// separate queries; there is no fabricated cross-workspace relevance score.
		if len(p.Workspaces) != 1 {
			return nil, errors.New("current benchmark supports one selected workspace per query; multi-workspace ranking is not defined")
		}
		for _, ws := range f.Workspaces {
			allowed := ws.Key == p.Workspaces[0]
			qr, err := query(search, ws.Key, ws.ID, userID, p.Query)
			if err != nil {
				return nil, err
			}
			if err := json.NewEncoder(trace).Encode(map[string]any{"question_id": p.ID, "response": qr}); err != nil {
				return nil, err
			}
			if !allowed {
				// A principal may be a member of another workspace while querying
				// only one. Such a query is not an unauthorized probe.
				member := false
				for _, name := range in.Workspaces[ws.Key].Members {
					member = member || name == p.Asker
				}
				if !member {
					r.DeniedProbes = append(r.DeniedProbes, qr)
					if qr.Status != http.StatusNotFound && qr.Status != http.StatusForbidden {
						return nil, errors.New("unauthorized workspace probe was not denied")
					}
				}
				continue
			}
			r.Queries = append(r.Queries, qr)
			if qr.Status != http.StatusOK {
				return nil, fmt.Errorf("search failed with status %d", qr.Status)
			}
			// The endpoint exposes no snippet source ID/revision. Detect only
			// definite full-text tombstone disclosures, never claim that a zero
			// count excludes partial-snippet leaks. This is not semantic grading.
			var rawBody any
			if err := json.Unmarshal(qr.Body, &rawBody); err != nil {
				return nil, err
			}
			for _, doc := range p.Documents {
				for _, c := range doc.Comments {
					if c.Deleted && c.Text != "" && !hasLiveCopy(p.Documents, c.Text) && containsText(rawBody, c.Text) {
						r.DeletedExposures = append(r.DeletedExposures, c.ID)
					}
				}
			}
			var body struct {
				Issues []struct{ ID string } `json:"issues"`
			}
			if err := json.Unmarshal(qr.Body, &body); err != nil {
				return nil, err
			}
			for id, doc := range docs {
				if doc.Workspace == ws.Key {
					sources, err := hydrate(ctx, conn, id, doc, comments)
					if err != nil {
						return nil, err
					}
					r.AllowedSources = append(r.AllowedSources, sources...)
				}
			}
			for _, item := range body.Issues {
				doc, exists := docs[item.ID]
				if !exists {
					return nil, errors.New("search returned a non-fixture issue")
				}
				sources, err := hydrate(ctx, conn, item.ID, doc, comments)
				if err != nil {
					return nil, err
				}
				r.Hits = append(r.Hits, hit{ID: doc.ID, Sources: sources})
			}
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		r.HeapBytes = memory.HeapAlloc
		responses = append(responses, r)
	}
	return responses, nil
}

func hasLiveCopy(documents []document, text string) bool {
	for _, doc := range documents {
		if strings.Contains(doc.Title, text) || strings.Contains(doc.Description.Text, text) {
			return true
		}
		for _, c := range doc.Comments {
			if !c.Deleted && strings.Contains(c.Text, text) {
				return true
			}
		}
	}
	return false
}

func containsText(value any, text string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, text)
	case []any:
		for _, item := range v {
			if containsText(item, text) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if containsText(item, text) {
				return true
			}
		}
	}
	return false
}

func run() (err error) {
	showVersion := flag.Bool("version", false, "print embedded Git build identity")
	cleanupPath := flag.String("cleanup", "", "clean only fixture IDs recorded in this manifest")
	flag.Parse()
	if *showVersion {
		return json.NewEncoder(os.Stdout).Encode(version())
	}
	cfg, err := databaseConfig(os.Getenv("KBBENCH_DATABASE_URL"), os.Getenv("KBBENCH_DATABASE_NAME"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return errors.New("cannot connect to the explicit benchmark database; check local environment and migrations")
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(131, hashtext(current_database()))`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errors.New("another benchmark owns this database")
	}
	if *cleanupPath != "" {
		data, err := os.ReadFile(*cleanupPath)
		if err != nil {
			return err
		}
		var f fixture
		if err := json.Unmarshal(data, &f); err != nil {
			return err
		}
		if err := cleanup(ctx, conn, f); err != nil {
			return err
		}
		f.Status = "cleaned"
		return saveFixture(*cleanupPath, f, false)
	}
	path := os.Getenv("KBBENCH_FIXTURE_MANIFEST")
	if path == "" {
		return errors.New("KBBENCH_FIXTURE_MANIFEST is required")
	}
	var in input
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 32<<20))
	if err := decoder.Decode(&in); err != nil {
		return err
	}
	f, err := setup(ctx, conn, in, path)
	if err != nil {
		return err
	}
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanErr := cleanup(cleanCtx, conn, f)
		if cleanErr == nil {
			f.Status = "cleaned"
			cleanErr = saveFixture(path, f, false)
		}
		err = errors.Join(err, cleanErr)
	}()
	trace, err := os.OpenFile(path+".queries.jsonl", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, trace.Close()) }()
	responses, err := evaluate(ctx, conn, f, in, trace)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"responses": responses, "query_identity_kind": "synthetic member projection of asker IDs and workspace memberships; token authentication is not exercised",
		"backend":                "in-process production RequireWorkspaceMember + SearchIssues + PostgreSQL; no network server",
		"supported_source_kinds": []string{"description", "comment"},
	})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
