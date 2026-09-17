package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/cleanunicorn/dispatch/internal/transport"
	"path/filepath"
	"testing"
	"time"

	"github.com/cleanunicorn/dispatch/internal/agent"
	"github.com/cleanunicorn/dispatch/internal/store"
)

func TestStore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var _ store.Store = s

	seq1, err := s.Append(ctx, store.Record{Task: "t1", Thread: "th1", Kind: "inbound", Payload: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	seq2, _ := s.Append(ctx, store.Record{Task: "t1", Thread: "th1", Kind: "agent", Payload: []byte(`{"b":2}`)})
	if seq2 != seq1+1 {
		t.Fatalf("seq %d %d", seq1, seq2)
	}
	var got []store.Record
	if err := s.Replay(ctx, seq1, func(r store.Record) error { got = append(got, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "agent" || string(got[0].Payload) != `{"b":2}` {
		t.Fatalf("replay = %+v", got)
	}

	def := agent.Definition{Name: "coder", Kind: agent.KindClaude, Model: "haiku", AllowedTools: []string{"Read"}}
	if err := s.PutDefinition(ctx, def); err != nil {
		t.Fatal(err)
	}
	d2, err := s.GetDefinition(ctx, "coder")
	if err != nil || d2.Model != "haiku" || len(d2.AllowedTools) != 1 {
		t.Fatalf("get def = %+v err=%v", d2, err)
	}
	if _, err := s.GetDefinition(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	ts := store.TaskState{ID: "t1", Transport: "slack", Thread: "th1", Definition: def, Requester: "U42", Asker: "U7", Status: store.StatusRunning, LastSeq: seq2}
	if err := s.PutTask(ctx, ts); err != nil {
		t.Fatal(err)
	}
	ts.Session = "sess"
	ts.Model = "claude-haiku-4-5"
	ts.ModelPin = "opus"
	ts.Status = store.StatusIdle
	if err := s.PutTask(ctx, ts); err != nil {
		t.Fatal(err)
	}
	back, err := s.GetTask(ctx, "t1")
	if err != nil || back.Session != "sess" || back.Model != "claude-haiku-4-5" || back.ModelPin != "opus" || back.Status != store.StatusIdle || back.Definition.Name != "coder" || back.Transport != "slack" || back.Requester != "U42" || back.Asker != "U7" {
		t.Fatalf("get task = %+v err=%v", back, err)
	}
	latest, err := s.LatestTaskForThread(ctx, "th1")
	if err != nil || latest.ID != "t1" {
		t.Fatalf("latest = %+v err=%v", latest, err)
	}
	idle, _ := s.ListTasks(ctx, store.StatusIdle)
	if len(idle) != 1 {
		t.Fatalf("idle = %d", len(idle))
	}
	if err := s.DeleteDefinition(ctx, "coder"); err != nil {
		t.Fatal(err)
	}
	if defs, _ := s.ListDefinitions(ctx); len(defs) != 0 {
		t.Fatalf("defs = %d", len(defs))
	}

	flow := store.FlowState{Thread: "th9", Transport: "slack", Surface: "chat", Kind: "agent_add", Answers: []string{"reviewer"}}
	if err := s.PutFlow(ctx, flow); err != nil {
		t.Fatal(err)
	}
	flow.Answers = append(flow.Answers, "opus")
	if err := s.PutFlow(ctx, flow); err != nil {
		t.Fatal(err)
	}
	flows, err := s.ListFlows(ctx)
	if err != nil || len(flows) != 1 || flows[0].Surface != "chat" || len(flows[0].Answers) != 2 || flows[0].Answers[1] != "opus" {
		t.Fatalf("flows = %+v err=%v", flows, err)
	}
	if err := s.DeleteFlow(ctx, "th9"); err != nil {
		t.Fatal(err)
	}
	if flows, _ := s.ListFlows(ctx); len(flows) != 0 {
		t.Fatalf("flows after delete = %+v", flows)
	}
}

func TestTaskRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i, r := range []store.Record{
		{Task: "t1", Thread: "th", Kind: "verdict", Payload: []byte(`{"n":0}`)},
		{Task: "t1", Thread: "th", Kind: "decision", Payload: []byte(`{"n":1}`)},
		{Task: "t2", Thread: "th", Kind: "verdict", Payload: []byte(`{"n":2}`)},
		{Task: "t1", Thread: "th", Kind: "verdict", Payload: []byte(`{"n":3}`)},
		{Task: "t1", Thread: "th", Kind: "verdict", Payload: []byte(`{"n":4}`)},
	} {
		if _, err := s.Append(ctx, r); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := s.TaskRecords(ctx, "t1", "verdict", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0].Payload) != `{"n":0}` || string(got[2].Payload) != `{"n":4}` {
		t.Fatalf("TaskRecords = %+v, want t1's three verdicts oldest first", got)
	}
	last, err := s.TaskRecords(ctx, "t1", "verdict", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || string(last[0].Payload) != `{"n":4}` {
		t.Fatalf("TaskRecords(limit 1) = %+v, want only the newest", last)
	}
}

func TestThreadRecordsOfKind(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	th := transport.ThreadID("C/1.0")
	for i, kind := range []string{"inbound", "agent", "outbound", "agent", "outbound", "inbound", "agent", "outbound"} {
		if _, err := s.Append(ctx, store.Record{At: time.Now(), Task: "t", Thread: th, Kind: kind, Payload: []byte(fmt.Sprintf(`{"i":%d}`, i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ThreadRecordsOfKind(ctx, th, "inbound", 1)
	if err != nil || len(got) != 1 || string(got[0].Payload) != `{"i":5}` {
		t.Fatalf("last inbound = %+v, %v", got, err)
	}
	got, err = s.ThreadRecordsOfKind(ctx, th, "inbound", 5)
	if err != nil || len(got) != 2 || got[0].Seq > got[1].Seq {
		t.Fatalf("inbound records = %+v, %v (want both, oldest first)", got, err)
	}
}

// A database from before a column existed gains it on open, and the
// tasks already in it keep working — model_pin is the newest, and the
// oldest schema here is the one that shipped first.
func TestMigrateAddsTaskColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, thread TEXT NOT NULL, definition BLOB NOT NULL,
		session TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
		last_seq INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO tasks(id, thread, definition, status, updated_at) VALUES('t1','th1','{}','idle','')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	back, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if back.ModelPin != "" {
		t.Errorf("model_pin = %q, want empty", back.ModelPin)
	}
	back.ModelPin = "opus"
	if err := s.PutTask(ctx, back); err != nil {
		t.Fatal(err)
	}
	if again, err := s.GetTask(ctx, "t1"); err != nil || again.ModelPin != "opus" {
		t.Fatalf("get task = %+v err=%v", again, err)
	}
}
