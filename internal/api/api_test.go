package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"bankcmp/internal/app"
	"bankcmp/internal/journal"
	"bankcmp/internal/model"
	"bankcmp/internal/seed"
)

func testServer(t *testing.T) (*httptest.Server, *journal.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := journal.Open(filepath.Join(dir, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := app.New(store, 1)
	srv := &Server{App: a, Store: store}
	ts := httptest.NewServer(srv.Handler(nil))
	t.Cleanup(ts.Close)
	return ts, store
}

func doJSON(t *testing.T, method, url string, body any) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(res.Body)
	return res, out.Bytes()
}

func TestConnectionAndCrossOriginGuards(t *testing.T) {
	ts, _ := testServer(t)

	// Creating a connection works and the DSN is redacted in responses.
	res, body := doJSON(t, "POST", ts.URL+"/api/connections", map[string]string{
		"name": "pg", "driver": "postgres", "dsn": "postgres://user:secret@host/db",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", res.StatusCode, body)
	}
	var c model.Connection
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatal(err)
	}
	if c.DSN == "postgres://user:secret@host/db" {
		t.Fatal("password leaked in response")
	}

	// A cross origin mutation is rejected.
	req, _ := http.NewRequest("POST", ts.URL+"/api/connections", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross origin request was not rejected: %d", res.StatusCode)
	}
	res.Body.Close()

	// A non-JSON content type is rejected.
	req, _ = http.NewRequest("POST", ts.URL+"/api/connections", bytes.NewReader([]byte(`name=x`)))
	req.Header.Set("Content-Type", "text/plain")
	res, _ = http.DefaultClient.Do(req)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type not rejected: %d", res.StatusCode)
	}
	res.Body.Close()
}

func TestTaskReportCSV(t *testing.T) {
	ts, store := testServer(t)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	if err := seed.SeedAll(dir, 500, 3, manifest); err != nil {
		t.Fatal(err)
	}
	// Use the seeded config's table specs by creating the task with IDs and a
	// config body.
	cfgText := `tables:
  - name: accounts
    columns:
      - name: balance
        type: decimal
      - name: open_date
        type: date
`
	_, body := doJSON(t, "POST", ts.URL+"/api/connections", map[string]string{
		"name": "src", "driver": "sqlite", "dsn": filepath.Join(dir, "src.db"),
	})
	var srcConn model.Connection
	_ = json.Unmarshal(body, &srcConn)
	_, body = doJSON(t, "POST", ts.URL+"/api/connections", map[string]string{
		"name": "tgt", "driver": "sqlite", "dsn": filepath.Join(dir, "tgt.db"),
	})
	var tgtConn model.Connection
	_ = json.Unmarshal(body, &tgtConn)

	res, body := doJSON(t, "POST", ts.URL+"/api/tasks", map[string]any{
		"name": "csv", "source_id": srcConn.ID, "target_id": tgtConn.ID,
		"config": cfgText, "start": true,
		"options": map[string]any{"workers": 1, "chunk_workers": 2, "chunk_rows": 100, "recheck": false},
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %s", res.StatusCode, body)
	}
	var task journal.Task
	_ = json.Unmarshal(body, &task)
	// Wait for completion.
	deadline := 100
	for i := 0; i < deadline; i++ {
		_, body = doJSON(t, "GET", ts.URL+"/api/tasks/"+itoa(task.ID), nil)
		var d struct {
			Task struct {
				Status model.TaskStatus `json:"status"`
			} `json:"task"`
		}
		_ = json.Unmarshal(body, &d)
		if d.Task.Status == model.TaskSucceeded || d.Task.Status == model.TaskFailed {
			if d.Task.Status != model.TaskSucceeded {
				t.Fatalf("task failed: %s", body)
			}
			break
		}
		waitSmall()
	}
	res, body = doJSON(t, "GET", ts.URL+"/api/tasks/"+itoa(task.ID)+"/report.csv", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("csv: %d %s", res.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("table,status")) {
		t.Fatalf("csv header missing: %s", body)
	}
	_ = store
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func waitSmall() { time.Sleep(50 * time.Millisecond) }

// TestRepairUsesRealValuesForDigestColumns proves repair generation never
// writes a digest back into a column.  The table's memo/receipt columns are
// compared by digest, but the generated INSERT must carry the source values.
func TestRepairUsesRealValuesForDigestColumns(t *testing.T) {
	ts, _ := testServer(t)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	if err := seed.SeedAll(dir, 400, 5, manifest); err != nil {
		t.Fatal(err)
	}
	// Force a missing row in transactions so a repair INSERT is generated.
	mustRunDB(t, filepath.Join(dir, "tgt.db"), `DELETE FROM transactions WHERE id=1999`)
	// Use the seeded config but only the transactions table.
	fullCfg, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfgText := string(fullCfg)
	cfgText = cfgText[strings.Index(cfgText, "tables:"):]

	_, body := doJSON(t, "POST", ts.URL+"/api/connections", map[string]string{
		"name": "s", "driver": "sqlite", "dsn": filepath.Join(dir, "src.db")})
	var sc model.Connection
	_ = json.Unmarshal(body, &sc)
	_, body = doJSON(t, "POST", ts.URL+"/api/connections", map[string]string{
		"name": "t", "driver": "sqlite", "dsn": filepath.Join(dir, "tgt.db")})
	var tc model.Connection
	_ = json.Unmarshal(body, &tc)
	res, body := doJSON(t, "POST", ts.URL+"/api/tasks", map[string]any{
		"name": "digest", "source_id": sc.ID, "target_id": tc.ID,
		"config": cfgText, "start": false,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %s", res.StatusCode, body)
	}
	var task journal.Task
	_ = json.Unmarshal(body, &task)
	// Run the comparison directly so the table run rows exist.
	if err := runTaskNow(t, ts.URL, task.ID); err != nil {
		t.Fatal(err)
	}
	res, body = doJSON(t, "POST", ts.URL+fmt.Sprintf("/api/tasks/%d/repairs", task.ID), map[string]any{
		"table": "transactions", "kinds": []string{"missing"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("repair: %d %s", res.StatusCode, body)
	}
	var out struct {
		Repair model.RepairBatch `json:"repair"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Repair.Statements == 0 {
		t.Fatalf("expected an INSERT for the deleted row: %s", body)
	}
	if strings.Contains(out.Repair.SQL, "e3b0c44298fc1c14") || regexp.MustCompile(`'[0-9a-f]{64}'`).MatchString(out.Repair.SQL) {
		t.Fatalf("repair SQL contains a SHA-256 digest instead of a value: %s", out.Repair.SQL)
	}
	if !strings.Contains(out.Repair.SQL, "流水备注") {
		t.Fatalf("repair SQL does not contain the real memo value: %s", out.Repair.SQL)
	}
}

func mustRunDB(t *testing.T, path, q string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func runTaskNow(t *testing.T, base string, id int64) error {
	t.Helper()
	res, body := doJSON(t, "POST", base+fmt.Sprintf("/api/tasks/%d/start", id), nil)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("start: %d %s", res.StatusCode, body)
	}
	for i := 0; i < 200; i++ {
		_, body = doJSON(t, "GET", base+fmt.Sprintf("/api/tasks/%d", id), nil)
		var d struct {
			Task struct {
				Status model.TaskStatus `json:"status"`
			} `json:"task"`
		}
		_ = json.Unmarshal(body, &d)
		if d.Task.Status == model.TaskSucceeded {
			return nil
		}
		if d.Task.Status == model.TaskFailed {
			return fmt.Errorf("task failed: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("task did not finish: %s", body)
}
