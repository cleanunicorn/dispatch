// Package sqlite implements store.Store on a single SQLite file
// (modernc.org/sqlite, pure Go, no cgo).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/executor"
	"github.com/cleanunicorn/dispatch/internal/store"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"github.com/cleanunicorn/dispatch/internal/workflow"
)

const schema = `
CREATE TABLE IF NOT EXISTS log (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	at      TEXT NOT NULL,
	task    TEXT NOT NULL,
	thread  TEXT NOT NULL,
	kind    TEXT NOT NULL,
	payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS log_task ON log(task, seq);
CREATE INDEX IF NOT EXISTS log_thread ON log(thread, seq);
CREATE INDEX IF NOT EXISTS log_task_kind ON log(task, kind, seq);

CREATE TABLE IF NOT EXISTS tasks (
	id         TEXT PRIMARY KEY,
	thread     TEXT NOT NULL,
	definition BLOB NOT NULL,
	session    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL,
	last_seq   INTEGER NOT NULL DEFAULT 0,
	prompt     TEXT NOT NULL DEFAULT '',
	resumes    INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_thread ON tasks(thread, updated_at);

CREATE TABLE IF NOT EXISTS definitions (
	name TEXT PRIMARY KEY,
	body BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS flows (
	thread     TEXT PRIMARY KEY,
	body       BLOB NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workflows (
	thread     TEXT PRIMARY KEY,
	body       BLOB NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS closed_threads (
	thread    TEXT PRIMARY KEY,
	closed_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS web_users (
	name       TEXT PRIMARY KEY,
	password   TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS web_sessions (
	token      TEXT PRIMARY KEY,
	user       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS web_sessions_user ON web_sessions(user);
`

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate adds columns introduced after the first release.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	for _, add := range []struct{ col, ddl string }{
		{"transport", `ALTER TABLE tasks ADD COLUMN transport TEXT NOT NULL DEFAULT ''`},
		{"prompt", `ALTER TABLE tasks ADD COLUMN prompt TEXT NOT NULL DEFAULT ''`},
		{"resumes", `ALTER TABLE tasks ADD COLUMN resumes INTEGER NOT NULL DEFAULT 0`},
		{"requester", `ALTER TABLE tasks ADD COLUMN requester TEXT NOT NULL DEFAULT ''`},
		{"model", `ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT ''`},
		{"model_pin", `ALTER TABLE tasks ADD COLUMN model_pin TEXT NOT NULL DEFAULT ''`},
		{"asker", `ALTER TABLE tasks ADD COLUMN asker TEXT NOT NULL DEFAULT ''`},
	} {
		if cols[add.col] {
			continue
		}
		if _, err := db.Exec(add.ddl); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Append(ctx context.Context, r store.Record) (int64, error) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO log(at, task, thread, kind, payload) VALUES(?,?,?,?,?)`,
		r.At.UTC().Format(time.RFC3339Nano), string(r.Task), string(r.Thread), r.Kind, r.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Replay(ctx context.Context, after int64, fn func(store.Record) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, at, task, thread, kind, payload FROM log WHERE seq > ? ORDER BY seq`, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanRecord reads one row of the log's SELECT column list.
func scanRecord(rows *sql.Rows) (store.Record, error) {
	var r store.Record
	var at, task, thread string
	if err := rows.Scan(&r.Seq, &at, &task, &thread, &r.Kind, &r.Payload); err != nil {
		return r, err
	}
	r.At, _ = time.Parse(time.RFC3339Nano, at)
	r.Task, r.Thread = executor.TaskID(task), transport.ThreadID(thread)
	return r, nil
}

// tail runs a newest-first query and returns its rows oldest-first.
func (s *Store) tail(ctx context.Context, query string, args ...any) ([]store.Record, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}

func (s *Store) ThreadRecords(ctx context.Context, thread transport.ThreadID, limit int) ([]store.Record, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.tail(ctx,
		`SELECT seq, at, task, thread, kind, payload FROM log WHERE thread = ? ORDER BY seq DESC LIMIT ?`,
		string(thread), limit)
}

func (s *Store) ThreadRecordsOfKind(ctx context.Context, thread transport.ThreadID, kind string, limit int) ([]store.Record, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.tail(ctx, // walks log_thread(thread, seq) newest-first until limit of the kind are found
		`SELECT seq, at, task, thread, kind, payload FROM log WHERE thread = ? AND kind = ? ORDER BY seq DESC LIMIT ?`,
		string(thread), kind, limit)
}

func (s *Store) ThreadHeadOfKind(ctx context.Context, thread transport.ThreadID, kind string, limit int) ([]store.Record, error) {
	if limit <= 0 {
		limit = 50
	}
	recs, err := s.tail(ctx,
		`SELECT seq, at, task, thread, kind, payload FROM log WHERE thread = ? AND kind = ? ORDER BY seq ASC LIMIT ?`,
		string(thread), kind, limit)
	slices.Reverse(recs) // tail reversed a newest-first scan; this one was oldest-first already
	return recs, err
}

func (s *Store) TaskRecords(ctx context.Context, task executor.TaskID, kind string, limit int) ([]store.Record, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.tail(ctx, // uses the log_task_kind(task, kind, seq) index
		`SELECT seq, at, task, thread, kind, payload FROM log WHERE task = ? AND kind = ? ORDER BY seq DESC LIMIT ?`,
		string(task), kind, limit)
}

// taskCols is the tasks column list, in the order scanTask reads it and
// PutTask binds it. A column added here needs a `?` in PutTask's VALUES,
// an `excluded.` line in its ON CONFLICT SET, a dest in scanTask, and an
// ALTER entry in migrate — the CREATE TABLE above only runs for a fresh
// database, which is why transport and requester live in migrate alone.
const taskCols = "id, transport, thread, definition, requester, asker, session, model, model_pin, status, last_seq, prompt, resumes, updated_at"

func (s *Store) PutTask(ctx context.Context, t store.TaskState) error {
	def, err := json.Marshal(t.Definition)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks(`+taskCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET transport=excluded.transport, thread=excluded.thread, definition=excluded.definition,
			requester=excluded.requester, asker=excluded.asker, session=excluded.session, model=excluded.model, model_pin=excluded.model_pin, status=excluded.status,
			last_seq=excluded.last_seq, prompt=excluded.prompt, resumes=excluded.resumes, updated_at=excluded.updated_at`,
		string(t.ID), t.Transport, string(t.Thread), def, t.Requester, t.Asker, t.Session, t.Model, t.ModelPin, t.Status, t.LastSeq, t.Prompt, t.Resumes,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetTask(ctx context.Context, id executor.TaskID) (store.TaskState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, string(id))
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, store.ErrNotFound
	}
	return t, err
}

func (s *Store) ListTasks(ctx context.Context, status string) ([]store.TaskState, error) {
	q := `SELECT ` + taskCols + ` FROM tasks`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TaskState
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LatestTaskForThread returns the most recently updated task on a thread.
func (s *Store) LatestTaskForThread(ctx context.Context, thread transport.ThreadID) (store.TaskState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE thread = ? ORDER BY updated_at DESC LIMIT 1`, string(thread))
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, store.ErrNotFound
	}
	return t, err
}

type scanner interface{ Scan(dest ...any) error }

// scanTask reads one row of taskCols.
func scanTask(sc scanner) (store.TaskState, error) {
	var t store.TaskState
	var id, thread, updated string
	var def []byte
	if err := sc.Scan(&id, &t.Transport, &thread, &def, &t.Requester, &t.Asker, &t.Session, &t.Model, &t.ModelPin, &t.Status, &t.LastSeq, &t.Prompt, &t.Resumes, &updated); err != nil {
		return t, err
	}
	t.ID, t.Thread = executor.TaskID(id), transport.ThreadID(thread)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if err := json.Unmarshal(def, &t.Definition); err != nil {
		return t, fmt.Errorf("sqlite: task %s definition: %w", id, err)
	}
	return t, nil
}

func (s *Store) PutDefinition(ctx context.Context, d agent.Definition) error {
	if d.Name == "" {
		return fmt.Errorf("sqlite: definition name is required")
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO definitions(name, body) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET body=excluded.body`, d.Name, body)
	return err
}

func (s *Store) GetDefinition(ctx context.Context, name string) (agent.Definition, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM definitions WHERE name = ?`, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.Definition{}, store.ErrNotFound
	}
	if err != nil {
		return agent.Definition{}, err
	}
	var d agent.Definition
	return d, json.Unmarshal(body, &d)
}

func (s *Store) ListDefinitions(ctx context.Context) ([]agent.Definition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM definitions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Definition
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d agent.Definition
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDefinition removes a definition by name.
func (s *Store) DeleteDefinition(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM definitions WHERE name = ?`, name)
	return err
}

func (s *Store) PutUser(ctx context.Context, u store.User) error {
	if u.Name == "" || u.Password == "" {
		return fmt.Errorf("sqlite: user name and password are required")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_users(name, password, created_at) VALUES(?,?,?)
		ON CONFLICT(name) DO UPDATE SET password=excluded.password`, u.Name, u.Password, u.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetUser(ctx context.Context, name string) (store.User, error) {
	var u store.User
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT name, password, created_at FROM web_users WHERE name = ?`, name).Scan(&u.Name, &u.Password, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, store.ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, password, created_at FROM web_users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		var u store.User
		var created string
		if err := rows.Scan(&u.Name, &u.Password, &created); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, name string) error {
	if err := s.DeleteUserSessions(ctx, name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_users WHERE name = ?`, name)
	return err
}

func (s *Store) PutSession(ctx context.Context, ss store.Session) error {
	if ss.Token == "" || ss.User == "" {
		return fmt.Errorf("sqlite: session token and user are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions(token, user, created_at, expires_at) VALUES(?,?,?,?)`,
		ss.Token, ss.User, ss.CreatedAt.UTC().Format(time.RFC3339Nano), ss.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) GetSession(ctx context.Context, token string) (store.Session, error) {
	var ss store.Session
	var created, expires string
	err := s.db.QueryRowContext(ctx, `SELECT token, user, created_at, expires_at FROM web_sessions WHERE token = ?`, token).Scan(&ss.Token, &ss.User, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ss, store.ErrNotFound
	}
	if err != nil {
		return ss, err
	}
	ss.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	ss.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	return ss, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token = ?`, token)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, user string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE user = ?`, user)
	return err
}

func (s *Store) PutFlow(ctx context.Context, f store.FlowState) error {
	if f.Thread == "" {
		return fmt.Errorf("sqlite: flow thread is required")
	}
	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO flows(thread, body, updated_at) VALUES(?,?,?) ON CONFLICT(thread) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at`,
		string(f.Thread), body, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListFlows(ctx context.Context) ([]store.FlowState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM flows ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.FlowState
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var f store.FlowState
		if err := json.Unmarshal(body, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// A workflow run, one per thread. It is the same shape as a flow — a
// multi-step thing in progress on a thread, replaced in place and read
// back on the next start — and a table of its own because the two are
// resumed by different code and a `kind` column that decided which would
// be one more thing to get wrong.
func (s *Store) PutWorkflow(ctx context.Context, w workflow.State) error {
	if w.Thread == "" {
		return fmt.Errorf("sqlite: workflow thread is required")
	}
	w.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO workflows(thread, body, updated_at) VALUES(?,?,?) ON CONFLICT(thread) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at`,
		string(w.Thread), body, w.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) ListWorkflows(ctx context.Context) ([]workflow.State, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM workflows ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workflow.State
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var w workflow.State
		if err := json.Unmarshal(body, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) DeleteWorkflow(ctx context.Context, thread transport.ThreadID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workflows WHERE thread = ?`, string(thread))
	return err
}

func (s *Store) DeleteFlow(ctx context.Context, thread transport.ThreadID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM flows WHERE thread = ?`, string(thread))
	return err
}

// SetThreadClosed records (or clears) that a human ended the conversation
// on thread. It is deliberately not part of the task row: several tasks
// can share a thread, and a task's own status keeps changing after it.
func (s *Store) SetThreadClosed(ctx context.Context, thread transport.ThreadID, closed bool) error {
	if thread == "" {
		return fmt.Errorf("sqlite: thread is required")
	}
	if !closed {
		_, err := s.db.ExecContext(ctx, `DELETE FROM closed_threads WHERE thread = ?`, string(thread))
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO closed_threads(thread, closed_at) VALUES(?,?) ON CONFLICT(thread) DO UPDATE SET closed_at=excluded.closed_at`,
		string(thread), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ClosedThreads lists the closed threads, oldest first.
func (s *Store) ClosedThreads(ctx context.Context) ([]transport.ThreadID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread FROM closed_threads ORDER BY closed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []transport.ThreadID
	for rows.Next() {
		var th string
		if err := rows.Scan(&th); err != nil {
			return nil, err
		}
		out = append(out, transport.ThreadID(th))
	}
	return out, rows.Err()
}
