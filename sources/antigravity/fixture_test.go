package antigravity

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// conversationSchema is the DDL agy 1.5.14 writes to
// conversations/<uuid>.db, copied verbatim from `.schema`.
const conversationSchema = "CREATE TABLE `trajectory_meta` (`trajectory_id` text,`cascade_id` text,`trajectory_type` integer,`source` integer,PRIMARY KEY (`trajectory_id`));" +
	"CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`has_subtrajectory` numeric NOT NULL DEFAULT false,`metadata` blob,`error_details` blob,`permissions` blob,`task_details` blob,`render_info` blob,`step_payload` blob,`step_format` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));" +
	"CREATE INDEX `idx_steps_status` ON `steps`(`status`);" +
	"CREATE INDEX `idx_steps_step_type` ON `steps`(`step_type`);" +
	"CREATE TABLE `gen_metadata` (`idx` integer,`data` blob,`size` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `parent_references` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `trajectory_metadata_blob` (`id` text DEFAULT \"main\",`data` blob,PRIMARY KEY (`id`));" +
	"CREATE TABLE `battle_mode_infos` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));"

const stepEphemeral = 101 // observed in real DBs; carries no conversation turn

var fixtureStart = time.Unix(1795377617, 0)

// convDB is a writable synthetic agy conversation database. The writer stays
// open with autocheckpoint off, so writes sit in the WAL as they do while agy
// is running.
type convDB struct {
	t    *testing.T
	db   *sql.DB
	path string
	next int
}

func newConvDB(t *testing.T, dir, id string) *convDB {
	t.Helper()
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	c := &convDB{t: t, db: db, path: path}
	c.exec("PRAGMA journal_mode=WAL")
	c.exec("PRAGMA wal_autocheckpoint=0")
	c.exec(conversationSchema)
	c.exec("INSERT INTO trajectory_meta VALUES (?, ?, 4, 17)", "557844e8-8ecb-45a4-969e-96dcaf923739", id)
	return c
}

func (c *convDB) exec(q string, args ...any) {
	c.t.Helper()
	if _, err := c.db.Exec(q, args...); err != nil {
		c.t.Fatalf("fixture exec %q: %v", q, err)
	}
}

// setMeta writes trajectory_metadata_blob. An empty uri omits the workspace
// message, as some real conversations do.
func (c *convDB) setMeta(uri string, created time.Time) {
	fields := []pbField{}
	if uri != "" {
		fields = append(fields, pbM(1, pbS(1, uri), pbS(2, uri)))
	}
	fields = append(fields,
		pbM(2, pbV(1, uint64(created.Unix())), pbV(2, uint64(created.Nanosecond()))),
		pbS(3, "3a6d54f0-0bee-4c95-91d1-374c3c2074c5"),
		pbS(18, "default-cli-project"),
	)
	c.exec("INSERT OR REPLACE INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)", pbMsg(fields...))
}

func (c *convDB) addExecutor(idx int, model string, window uint64) {
	data := pbMsg(pbV(1, 4), pbM(10, pbM(1, pbV(1, 1535), pbV(6, window), pbS(28, model))))
	c.exec("INSERT INTO executor_metadata (idx, data) VALUES (?, ?)", idx, data)
}

// addStep appends a step and returns its idx.
func (c *convDB) addStep(stepType, status int, metadata []byte) int {
	idx := c.next
	c.next++
	c.exec("INSERT INTO steps (idx, step_type, status, metadata, step_payload) VALUES (?, ?, ?, ?, ?)",
		idx, stepType, status, metadata, []byte("synthetic payload"))
	return idx
}

func (c *convDB) setStatus(idx, status int) {
	c.exec("UPDATE steps SET status = ? WHERE idx = ?", status, idx)
}

// stepMeta builds step metadata with the creation timestamp in field 1.
func stepMeta(at time.Time, extra ...pbField) []byte {
	ts := pbM(1, pbV(1, uint64(at.Unix())), pbV(2, uint64(at.Nanosecond())))
	return pbMsg(append([]pbField{ts, pbV(3, 2)}, extra...)...)
}

func toolMeta(at time.Time, name string) []byte {
	return stepMeta(at, pbM(4, pbS(1, "call_1"), pbS(2, name)), pbV(11, 1535))
}

func plannerMeta(at time.Time, input, output, cached uint64) []byte {
	usage := []pbField{pbV(1, 1535), pbV(3, output), pbV(6, 24)}
	if input > 0 {
		usage = append(usage, pbV(2, input))
	}
	if cached > 0 {
		usage = append(usage, pbV(5, cached))
	}
	return stepMeta(at, pbM(9, usage...), pbV(11, 1535))
}

func at(sec int) time.Time { return fixtureStart.Add(time.Duration(sec) * time.Second) }
